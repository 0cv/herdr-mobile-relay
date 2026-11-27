import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { chromium, expect } from '@playwright/test';

const captureTime = new Date('2026-08-12T09:05:00Z');
const origin = 'http://127.0.0.1:4174';
const [output] = process.argv.slice(2);
if (!output) throw new Error('Usage: capture-demo-home.mjs <output directory>');

const demoLabel = 'Demo — sample data';
const rolloutQuestion = 'How should we roll out the new search page?';
const rolloutChoices = [
  ['Preview first', 'Start with a small group using the preview link. Keep classic search as the default while we check feedback.'],
  ['All at once', 'Make the new search page the default for everyone. Keep the classic page ready in case we need to roll back.'],
];
const qrLabel = 'Sample QR — Herdr demo (not a setup link)';
const viewport = { width: 390, height: 844 };
const deviceScaleFactor = 2;
const contextOptions = {
  viewport,
  deviceScaleFactor,
  isMobile: true,
  hasTouch: true,
  locale: 'en-US',
  timezoneId: 'UTC',
  colorScheme: 'dark',
  reducedMotion: 'reduce',
};

async function visible(locator) {
  await expect(locator).toBeVisible();
  await expect(locator).toBeInViewport({ ratio: 1 });
}
const button = (scope, name) => scope.getByRole('button', { name, exact: true });
const text = (scope, value) => scope.getByText(value, { exact: true });
const region = (scope, name) => scope.getByRole('region', { name, exact: true });

async function openAgent(page, name) {
  const row = page.getByRole('button', { name: `Open ${name} on Atlas`, exact: true, includeHidden: true });
  await expect(row).toBeAttached();
  if (!await row.isVisible()) await row.locator('xpath=ancestor::details[1]/summary').click();
  await row.click();
}

async function settingsSection(page, name) {
  await button(page, 'Settings').click();
  const sections = region(page.getByRole('main', { name: 'Settings' }), name);
  return name === 'Devices' ? sections.filter({ has: text(page, 'Atlas') }) : sections;
}

async function frameSettings(page, section, edge = 'top') {
  const header = page.getByRole('banner');
  const { y, height } = await header.boundingBox();
  const belowHeader = y + height;
  await section.evaluate((element, { edge, belowHeader }) => {
    const rect = element.getBoundingClientRect();
    const offset = edge === 'bottom' ? rect.bottom - (window.innerHeight - 12) : rect.top - (belowHeader + 12);
    window.scrollBy({ top: offset, behavior: 'instant' });
  }, { edge, belowHeader });
  await visible(section);
  await expect.poll(async () => (await section.boundingBox())?.y).toBeGreaterThanOrEqual(belowHeader);
}

async function openInvite(page) {
  const devices = await settingsSection(page, 'Devices');
  await frameSettings(page, devices);
  await button(devices, 'Invite Device').click();
  const invite = page.getByRole('dialog', { name: 'Invite Device', exact: true });
  const role = invite.getByLabel('Role', { exact: true });
  await role.selectOption('reader');
  await expect(role).toHaveValue('reader');
  await visible(role);
  await visible(text(invite, demoLabel));
  await visible(text(invite, 'Preview an invitation. No invitation is created in the demo.'));
  return invite;
}

const scenes = {
  async 'home.jpeg'(page) {
    const agents = page.getByRole('main', { name: 'Agents' });
    for (const name of ['Atlas', 'Birch']) {
      const computer = button(agents, name);
      await expect(computer).toHaveAttribute('aria-pressed', 'true');
      await visible(computer);
    }
    const waiting = region(agents, 'Needs input');
    for (const [name, prompt, age] of [['Patch review', 'make test', '2m'], ['Release plan', rolloutQuestion, '4m']]) {
      const card = button(waiting, `Open ${name} on Atlas`);
      await expect(card).toBeEnabled();
      await visible(text(card, `Needs input · ${age}`));
      await visible(text(card, prompt));
    }
    const workspaces = region(agents, 'Workspaces');
    const summary = (name) => workspaces.locator('.workspace-card > summary').filter({ has: text(page, name) });
    const worktree = region(workspaces, 'fix/one worktree');
    for (const [workspace, label] of [['Search', 'Open Search polish on Atlas'], ['Docs', 'Open Docs preview on Birch']]) {
      if (await button(agents, label).isVisible()) await summary(workspace).click();
    }
    if (!await worktree.isVisible()) await summary('Project').click();
    await summary('Examples').evaluate(element => {
      window.scrollBy({ top: Math.max(0, Math.ceil(element.getBoundingClientRect().bottom - innerHeight)), behavior: 'instant' });
    });
    for (const name of ['Project', 'Search', 'Docs', 'Examples']) await visible(summary(name));
    await visible(text(worktree, 'fix/one'));
    const result = button(worktree, 'Open Fixture checks on Atlas');
    const idle = button(workspaces, 'Open Release notes on Atlas');
    await visible(result);
    await visible(idle);
    await visible(result.locator('.status-check'));
    await visible(idle.locator('.status-ring'));
    await visible(text(summary('Project'), '1 new result'));
    await visible(text(result, 'New result · 32m ago'));
    for (const choice of ['Allow once', 'Decline', 'Preview first', 'All at once', 'More…']) {
      await visible(button(waiting, choice));
    }
  },
  async 'terminal.jpeg'(page) {
    await openAgent(page, 'Search polish');
    const terminal = page.getByRole('log', { name: 'Agent terminal output', exact: true });
    const first = text(terminal, 'OpenCode · Search polish');
    const last = text(terminal, 'Checking keyboard focus and the empty search state…');
    await visible(first);
    await visible(text(terminal, '→ Read src/search/Results.svelte'));
    await visible(text(terminal, '✓ classic search remains available'));
    await visible(last);
    const { y, height } = await terminal.boundingBox();
    expect((await first.boundingBox()).y).toBeLessThan(y + height * .1);
    expect((await last.boundingBox()).y).toBeGreaterThan(y + height * .75);
    const prompt = page.getByRole('combobox', { name: 'Prompt', exact: true });
    await expect(prompt).toBeEnabled();
    await visible(prompt);
    await visible(button(page, 'Send prompt'));
    const enter = button(page, 'Enter');
    await expect(enter).toBeEnabled();
    await visible(enter);
  },
  async 'agent_plan.jpeg'(page) {
    await button(region(page, 'Needs input'), 'More…').click();
    const question = page.getByRole('form', { name: rolloutQuestion, exact: true });
    await visible(text(question, rolloutQuestion));
    for (const [name, description] of rolloutChoices) {
      const choice = question.getByRole('radio', { name: `${name} ${description}`, exact: true });
      await expect(choice).not.toBeChecked();
      await visible(choice);
      await visible(text(question, name));
      await visible(text(question, description));
    }
    const submit = button(question, 'Choose rollout');
    await expect(submit).toBeDisabled();
    const previewFirst = question.getByRole('radio', { name: /^Preview first / });
    await previewFirst.check();
    await expect(previewFirst).toBeChecked();
    await expect(submit).toBeEnabled();
    await visible(submit);
    await visible(button(page, 'Keyboard'));
    await visible(button(page, 'Enter'));
    await visible(page.getByRole('combobox', { name: 'Prompt', exact: true }));
  },
  async 'conversations.jpeg'(page) {
    await openAgent(page, 'Release notes');
    await visible(page.getByRole('banner').getByText(/^Full history · /));
    const history = region(page, 'Conversation with Release notes');
    for (const prompt of ['Review the search rollout plan', 'Draft a note for the search preview', 'Verify the rollout before review']) {
      await visible(text(history, prompt));
    }
    await visible(history.getByText(/Let’s start with a preview group/));
    await visible(history.getByText(/I added the preview-first rollout note/));
    await visible(history.getByText(/preview flag and fallback checks passed/));
    await visible(history.locator('pre code .syntax-keyword'));
    for (const tool of ['Read', 'Edit', 'Bash']) {
      await visible(history.locator('summary').filter({ hasText: new RegExp(`${tool}.*completed`, 'i') }));
    }
    await visible(page.getByRole('combobox', { name: 'Prompt', exact: true }));
  },
  async 'git-history.jpeg'(page) {
    await openAgent(page, 'Release notes');
    await button(page, 'Inspect workspace').click();
    const workspace = page.getByRole('dialog', { name: 'Workspace', exact: true });
    const changes = workspace.getByRole('tab', { name: /^Changes\s*\(3\)$/ });
    await changes.click();
    await expect(changes).toHaveAttribute('aria-selected', 'true');
    await visible(changes);
    const notes = workspace.getByRole('button', { name: /^docs\/release-notes\.md\s*Modified$/ });
    await notes.click();
    const zoom = button(workspace, 'Zoom in diff');
    for (let step = 0; step < 4; step++) await zoom.click();
    await expect(button(workspace, 'Reset diff zoom')).toHaveText('160%');
    const diff = workspace.getByLabel('Diff for docs/release-notes.md', { exact: true });
    await visible(text(workspace, demoLabel));
    await visible(text(workspace, 'Unified diff'));
    await visible(notes);
    await visible(workspace.getByRole('button', { name: /^src\/search\/Results\.svelte\s*Modified$/ }));
    await visible(workspace.getByRole('button', { name: /^tests\/search\.test\.ts\s*Modified$/ }));
    await visible(diff.locator('.diff-meta').first());
    const hunks = diff.locator('.diff-hunk');
    await expect(hunks).toHaveCount(3);
    for (const hunk of await hunks.all()) await visible(hunk);
    for (const [kind, line] of [
      ['deletion', '-Roll out to everyone.'], ['addition', '+Start with the search preview group.'],
      ['deletion', '-- Smoke-test search after launch.'], ['addition', '+- Verify the classic search fallback.'],
      ['deletion', '-Pause the release if needed.'], ['addition', '+Turn off the preview flag to return to classic search.'],
    ]) {
      const row = diff.locator(`.diff-${kind}`).filter({ hasText: line });
      await expect(row).toHaveText(line);
      await visible(row);
    }
  },
  async 'devices.jpeg'(page) {
    const devices = await settingsSection(page, 'Devices');
    await expect(devices.getByRole('article')).toHaveCount(2);
    await frameSettings(page, devices);
    await visible(devices.getByRole('heading', { name: 'Paired devices', exact: true }));
    for (const [name, role] of [['Slate Tablet', 'Controller'], ['Field Phone', 'Reader']]) {
      const device = devices.getByRole('article', { name: `${name}, ${role.toLowerCase()}`, exact: true });
      await visible(text(device, name));
      await visible(text(device, role));
    }
    await visible(text(devices, 'Current device'));
    const birch = region(page.getByRole('main', { name: 'Settings' }), 'Devices').filter({ has: text(page, 'Birch') });
    await visible(birch);
    await visible(text(birch, 'Current device'));
    await visible(birch.getByRole('article', { name: 'Pocket Reader, reader', exact: true }));
  },
  async 'devices-invite.jpeg'(page) {
    const invite = await openInvite(page);
    await visible(invite.locator('.dialog-content'));
    await visible(button(invite, 'Show sample QR'));
    await visible(button(invite, 'Cancel'));
    await expect(invite.getByRole('img', { name: qrLabel, exact: true })).toHaveCount(0);
  },
  async 'devices-qr.jpeg'(page) {
    const invite = await openInvite(page);
    await button(invite, 'Show sample QR').click();
    const qr = invite.getByRole('img', { name: qrLabel, exact: true });
    await visible(qr);
    const modules = qr.locator('path');
    await expect(modules).toHaveAttribute('d', /\S/);
    await visible(modules);
    await expect(modules).toHaveCSS('fill', 'rgb(0, 0, 0)');
    await visible(text(invite, `${qrLabel}.`));
    await visible(text(invite, demoLabel));
    await visible(invite.getByLabel('Role', { exact: true }));
    await visible(button(invite, 'Close'));
    await visible(invite.locator('.dialog-content'));
    return qr;
  },
  async 'speech.jpeg'(page) {
    const speech = await settingsSection(page, 'Read Aloud');
    const voices = speech.getByRole('list', { name: 'Sample voices', exact: true });
    await expect(voices.getByRole('listitem')).toHaveText(['English — Meadow', 'French — Willow']);
    await frameSettings(page, speech, 'bottom');
    await visible(page.getByRole('heading', { name: 'Appearance', exact: true }));
    await visible(voices);
    await visible(text(speech, 'Voice downloads, removal and playback are unavailable in the demo.'));
  },
  async 'notifications.jpg'(page) {
    const notifications = await settingsSection(page, 'Notifications');
    await frameSettings(page, notifications);
    const settings = page.getByRole('main', { name: 'Settings' });
    await visible(region(settings, 'Read Aloud'));
    await visible(region(settings, 'Updates'));
    const status = notifications.getByRole('status');
    await expect(status).toHaveText('Notification delivery: Off (unavailable in demo)');
    await visible(status);
    await visible(text(notifications, 'The demo does not use your notification permission or send notifications.'));
  },
};

await expect.poll(async () => {
  const response = await fetch(origin, { signal: AbortSignal.timeout(1_000) }).catch(() => null);
  return response?.status;
}, { message: 'Demo server should be ready on loopback', timeout: 10_000 }).toBe(200);

const browser = await chromium.launch();
try {
  for (const [file, scene] of Object.entries(scenes)) {
    const context = await browser.newContext(contextOptions);
    try {
      const page = await context.newPage();
      await page.clock.setFixedTime(captureTime);
      await page.goto(origin);
      await expect(page.locator('html')).toHaveAttribute('data-theme', 'nord');
      await visible(page.getByRole('banner').getByRole('heading', { name: demoLabel, exact: true }));
      await page.evaluate(async () => {
        const faces = await document.fonts.load('16px "Herdr Nerd Symbols"', '⚡');
        if (!faces.length) throw new Error('The Herdr Nerd Symbols font is missing.');
        await document.fonts.ready;
      });
      const qr = await scene(page);
      await page.evaluate(async () => {
        await document.fonts.ready;
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      });
      const qrPixels = await qr?.evaluate((svg, scale) => {
        const matrix = svg.getScreenCTM();
        const pixel = (x, y) => {
          const point = new DOMPoint(x, y).matrixTransform(matrix);
          return [Math.floor(point.x * scale), Math.floor(point.y * scale)];
        };
        return { topLeftFinder: pixel(3.5, 3.5), quietZone: pixel(-2, 3.5) };
      }, deviceScaleFactor);
      const jpeg = await page.screenshot({
        type: 'jpeg', quality: 90, scale: 'device', animations: 'disabled', caret: 'hide',
      });
      expect(jpeg.readUInt16BE(0)).toBe(0xffd8);
      const dimensions = await page.evaluate(async ({ base64, qrPixels }) => {
        const bytes = Uint8Array.from(atob(base64), (byte) => byte.charCodeAt(0));
        const image = await createImageBitmap(new Blob([bytes], { type: 'image/jpeg' }));
        const { width, height } = image;
        if (qrPixels) {
          const canvas = document.createElement('canvas');
          canvas.width = width;
          canvas.height = height;
          const ctx = canvas.getContext('2d');
          ctx.drawImage(image, 0, 0);
          const rgb = ([x, y]) => ctx.getImageData(x, y, 1, 1).data.subarray(0, 3);
          if (Math.max(...rgb(qrPixels.topLeftFinder)) > 64) {
            throw new Error('The QR finder mark is not dark in the JPEG.');
          }
          if (Math.min(...rgb(qrPixels.quietZone)) < 192) {
            throw new Error('The QR quiet zone is not light in the JPEG.');
          }
        }
        image.close();
        return { width, height };
      }, { base64: jpeg.toString('base64'), qrPixels });
      expect(dimensions).toEqual({ width: viewport.width * deviceScaleFactor, height: viewport.height * deviceScaleFactor });
      await writeFile(join(output, file), jpeg);
    } catch (error) {
      throw new Error(`Could not capture ${file}`, { cause: error });
    } finally {
      await context.close();
    }
  }
} finally {
  await browser.close();
}

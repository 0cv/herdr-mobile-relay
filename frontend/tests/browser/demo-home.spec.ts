import { expect, test, type Browser, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

test.use({ serviceWorkers: 'allow' });

declare global {
  interface Window {
    __inspectDemoOrigin(): Promise<unknown>;
  }
}

const appOrigin = 'http://127.0.0.1:4173';
const demoOrigin = 'http://127.0.0.1:4174';
const release = JSON.parse(readFileSync(resolve(process.env.HERDR_WEB_ROOT || 'dist', 'release.json'), 'utf8')) as {
  build: string;
  files: { javascript: { path: string } };
};
const demoLoader = '/assets/demo-loader.js';
const demoScript = '/assets/demo.js';
const demoAssets = new Map([
  ['/', 'document'],
  [demoLoader, 'script'],
  [demoScript, 'script'],
  ['/assets/demo.css', 'stylesheet'],
  ['/fonts/nerd-symbols-mono-v3.4.0.woff2', 'font'],
]);

async function watchDemo(page: Page, options: { block?: string; initialSearch?: string; simulatedPermission?: NotificationPermission } = {}) {
  const unexpected: string[] = [];
  const storage: string[] = [];
  const permissions: string[] = [];
  const blocked: string[] = [];
  page.on('pageerror', (error) => unexpected.push(error.message));
  page.on('websocket', (socket) => unexpected.push(socket.url()));
  page.on('filechooser', () => unexpected.push('file chooser'));
  page.on('console', (message) => {
    const [kind, value] = message.text().split(': ', 2);
    if (kind === 'Unexpected demo access') unexpected.push(value);
    if (kind === 'Demo origin storage') storage.push(value);
    if (kind === 'Demo origin permission') permissions.push(value);
  });
  await page.route('**/*', (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const type = request.resourceType();
    const allowedSearch = !url.search || (type === 'document' && url.pathname === '/' && url.search === options.initialSearch);
    if (url.origin !== demoOrigin || !allowedSearch || request.method() !== 'GET' || demoAssets.get(url.pathname) !== type) {
      unexpected.push(`${request.method()} ${request.url()}`);
      return route.abort();
    }
    if (url.pathname !== options.block) return route.continue();
    blocked.push(url.pathname);
    return route.abort();
  });
  await page.addInitScript((simulatedPermission) => {
    const debug = console.debug.bind(console);
    const NotificationApi = window.Notification;
    const nativeLocal = window.localStorage;
    const nativeSession = window.sessionStorage;
    const nativeWorkers = navigator.serviceWorker;
    const getRegistrations = nativeWorkers?.getRegistrations.bind(nativeWorkers);
    Object.defineProperty(window, '__inspectDemoOrigin', { value: async () => ({
      localEntries: nativeLocal.length,
      sessionEntries: nativeSession.length,
      controlled: Boolean(nativeWorkers?.controller),
      registrations: (await getRegistrations?.())?.length ?? 0,
    }) });
    debug(`Demo origin storage: ${nativeLocal.length},${nativeSession.length}`);
    debug(`Demo origin permission: ${NotificationApi?.permission ?? 'unavailable'}`);
    function unexpected(name: string): never {
      debug(`Unexpected demo access: ${name}`);
      throw new Error(`Unexpected demo access: ${name}`);
    }
    function guard(object: object, ...names: string[]) {
      for (const name of names) Object.defineProperty(object, name, { configurable: true, get: () => unexpected(name) });
    }
    guard(window, 'localStorage', 'sessionStorage', 'indexedDB', 'caches', 'fetch', 'XMLHttpRequest', 'WebSocket', 'RTCPeerConnection', 'Worker', 'SharedWorker');
    guard(navigator, 'serviceWorker', 'permissions', 'sendBeacon', 'setAppBadge', 'clearAppBadge', 'vibrate', 'wakeLock', 'credentials');
    if (nativeWorkers) guard(nativeWorkers, 'register');
    const clipboard = navigator.clipboard ?? {};
    guard(clipboard, 'writeText', 'write');
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: clipboard });
    const input = HTMLInputElement.prototype;
    const click = input.click;
    const showPicker = input.showPicker;
    input.click = function () {
      if (this.type === 'file') unexpected('file input click');
      else click.call(this);
    };
    input.showPicker = function () {
      if (this.type === 'file') unexpected('file input showPicker');
      else showPicker?.call(this);
    };
    guard(DataTransferItem.prototype, 'getAsFile');
    guard(window, 'showOpenFilePicker', 'speechSynthesis', 'Audio', 'AudioContext');
    guard(HTMLMediaElement.prototype, 'play');
    Object.defineProperty(window, 'Notification', { configurable: true, value: new Proxy(NotificationApi ?? function () {}, {
      get(target, property) {
        if (property === 'permission') {
          debug('Unexpected demo access: Notification.permission');
          return simulatedPermission ?? Reflect.get(target, property);
        }
        if (property === 'requestPermission') return unexpected('Notification.requestPermission');
        return Reflect.get(target, property);
      },
      construct: () => unexpected('new Notification'),
    }) });
  }, options.simulatedPermission);
  return { unexpected, storage, permissions, blocked };
}

async function expectCleanDemoOrigin(page: Page) {
  expect(await page.evaluate(() => window.__inspectDemoOrigin()))
    .toEqual({ localEntries: 0, sessionEntries: 0, controlled: false, registrations: 0 });
}

const demoLabel = 'Demo — sample data';
const rolloutQuestion = 'How should we roll out the new search page?';

async function expectDemoHeader(page: Page) {
  await expect(page.getByRole('banner').getByRole('heading', { name: demoLabel, exact: true })).toBeVisible();
}

async function expectSampleHome(page: Page) {
  await expectDemoHeader(page);
  const agents = page.getByRole('main', { name: 'Agents' });
  for (const label of ['Atlas', 'Birch']) {
    await expect(agents.getByRole('button', { name: label, exact: true })).toHaveAttribute('aria-pressed', 'true');
  }
  await expect(agents.getByRole('button', { name: 'Open Patch review on Atlas' }).getByText('make test', { exact: true })).toBeInViewport();
  await expect(agents.getByRole('button', { name: 'Allow once', exact: true })).toBeVisible();
  await expect(agents.getByRole('button', { name: 'Decline', exact: true })).toBeVisible();
  await expect(agents.getByRole('button', { name: 'Open Release plan on Atlas' }).getByText(rolloutQuestion, { exact: true })).toBeInViewport();
  for (const name of ['Preview first', 'All at once', 'More…']) {
    await expect(agents.getByRole('button', { name, exact: true })).toBeVisible();
  }
  for (const name of ['Patch review on Atlas', 'Release plan on Atlas', 'Release notes on Atlas', 'Search polish on Atlas', 'Fixture checks on Atlas', 'Docs preview on Birch', 'Example index on Birch']) {
    await expect(agents.getByRole('button', { name: `Open ${name}`, exact: true, includeHidden: true })).toBeEnabled();
  }
  await expect(page.getByRole('button', { name: 'Settings', exact: true })).toBeVisible();
  await expect(page.locator('link[rel="manifest"]')).toHaveCount(0);
}

async function openSettings(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page).toHaveURL(`${demoOrigin}/#settings`);
  await expectDemoHeader(page);
  const settings = page.getByRole('main', { name: 'Settings' });
  await expect(settings.getByText('Atlas — Controller', { exact: true })).toBeVisible();
  await expect(settings.getByText('Birch — Reader', { exact: true })).toBeVisible();
  await expect(settings.getByText('Notification delivery: Off (unavailable in demo)')).toBeVisible();
  await expect(settings.getByRole('button', { name: /Add|Remove|Reconnect|Pair|Rename|Revoke|Reset|Forget|Check for Updates/ })).toHaveCount(0);
  await expect(settings.getByRole('switch')).toHaveCount(0);
  return settings;
}

function demoAgent(page: Page, name: string, host = 'Atlas') {
  return page.getByRole('button', { name: `Open ${name} on ${host}`, exact: true, includeHidden: true });
}

async function openDemoAgent(page: Page, name: string) {
  const row = demoAgent(page, name);
  await expect(row).toBeAttached();
  if (!await row.isVisible()) await row.locator('xpath=ancestor::details[1]/summary').click();
  await row.click();
  await expectDemoHeader(page);
}

const back = (page: Page) => page.getByRole('button', { name: 'Back', exact: true }).click();
const composer = (page: Page) => page.getByRole('combobox', { name: 'Prompt', exact: true });
const output = (page: Page) => page.getByLabel('Agent terminal output');
const scripted = 'Scripted reply · no command or model ran';
const searchTestResult = [
  '✓ preview flag opens new search',
  '✓ clearing returns focus to search',
  '✓ no matches suggest a shorter query',
  '✓ classic search remains available',
  '4 passed in the sample log',
];
const notesPrompt = 'Fix the empty state for **sample** [preview](https://search-demo.invalid) <b>as text</b>';
const launchedName = 'preview-check';

async function expectNoCopyOrAttachments(page: Page) {
  await expect(page.getByRole('button', { name: /^(Copy|Attach|Read .*aloud|Speak)/i })).toHaveCount(0);
  await expect(page.locator('input[type=file]')).toHaveCount(0);
  await expect(page.getByText('Copy controls and Read Aloud are unavailable in the demo.')).toBeVisible();
}

async function expectTerminalExchange(page: Page, input: string, reply: string) {
  const echo = output(page).locator('[data-terminal-row]').filter({ hasText: input });
  await expect(echo).toHaveText(input);
  const nextRow = echo.locator('xpath=following-sibling::*[1][@data-terminal-row]');
  await expect(nextRow).toHaveText(reply);
}

async function expectTerminalResult(page: Page, heading: string, rows: string[]) {
  const command = output(page).getByText(heading, { exact: true });
  await expect(command).toBeVisible();
  await expect(command.locator(`xpath=following-sibling::*[@data-terminal-row][position() <= ${rows.length}]`)).toHaveText(rows);
}

async function expectKeyNote(page: Page, text: string) {
  const note = page.getByRole('status').filter({ hasText: text });
  await expect(note).toHaveText(text);
  await expect(note).toBeInViewport();
}

async function exerciseSampleChecks(page: Page) {
  const key = (name: string) => page.getByRole('button', { name, exact: true });
  const log = output(page);
  const earlier = log.getByText('# Task: verify the keyboard path', { exact: true });
  const screen = log.locator('[data-terminal-row-count]');
  const rowsBefore = Number(await screen.getAttribute('data-terminal-row-count'));
  const scrollTop = await log.evaluate((element) => { element.scrollTop = 40; return element.scrollTop; });
  expect(scrollTop).toBeGreaterThan(0);

  await expect(earlier).toBeInViewport();
  await key('Arrow keys').click();
  await key('Down').click();
  await expectKeyNote(page, '› Sample check 1/3: make test');
  await key('Enter').click();
  await expectKeyNote(page, 'Scripted check · make test — 4 passed in the sample log');
  await expect(screen).toHaveAttribute('data-terminal-row-count', String(rowsBefore + 7 + searchTestResult.length));
  await expect(log.getByText('Scripted check · $ make test', { exact: true })).not.toBeInViewport();
  await expect(earlier).toBeInViewport();
  expect(await log.evaluate((element) => element.scrollTop)).toBe(scrollTop);

  await log.evaluate((element) => { element.scrollTop = element.scrollHeight; });
  await expectTerminalResult(page, 'Scripted check · $ make test', [...searchTestResult, 'No command ran.']);
  await log.evaluate((element, top) => { element.scrollTop = top; }, scrollTop);
  await expect(earlier).toBeInViewport();
  await key('Down').click();
  await expectKeyNote(page, '› Sample check 1/3: make test');
  await key('Up').click();
  await expectKeyNote(page, '› Sample check 3/3: make docs');
  await key('Enter').click();
  const docsResult = '3 pages built; no broken internal links.';
  await expectKeyNote(page, `Scripted check · make docs — ${docsResult}`);
  await expect(screen).toHaveAttribute('data-terminal-row-count', String(rowsBefore + 18 + searchTestResult.length));
  await expect(log.getByText('Scripted check · $ make docs', { exact: true })).not.toBeInViewport();
  await expect(earlier).toBeInViewport();
  expect(await log.evaluate((element) => element.scrollTop)).toBe(scrollTop);
  await log.evaluate((element) => { element.scrollTop = element.scrollHeight; });
  await expectTerminalExchange(page, 'Keys: Up', '› Sample check 3/3: make docs');
  await expectTerminalResult(page, 'Scripted check · $ make docs', [docsResult, 'No command ran.']);
}

test('sample checks stay readable above a scrolled terminal at 320 px', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 844 });
  const observation = await watchDemo(page);
  await page.goto(demoOrigin);
  await openDemoAgent(page, 'Search polish');
  await composer(page).fill('Run the search tests');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expectTerminalResult(page, '→ $ make test', searchTestResult);
  await exerciseSampleChecks(page);
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

async function exerciseTerminal(page: Page) {
  await openDemoAgent(page, 'Search polish');
  await expect(output(page)).toContainText('OpenCode · Search polish');
  await expectNoCopyOrAttachments(page);
  await expect(page.getByText('Replies are scripted. Attachments are unavailable in the demo.')).toBeVisible();
  const input = composer(page);
  await expect(input).toHaveAttribute('spellcheck', 'false');
  await expect(input).toHaveAttribute('autocorrect', 'off');
  const paste = await input.evaluate((input) => {
    const plain = new DataTransfer();
    plain.setData('text/plain', 'Plain text');
    const file = new DataTransfer();
    file.items.add(new File(['sample'], 'sample.txt', { type: 'text/plain' }));
    const dispatch = (clipboardData: DataTransfer) => input.dispatchEvent(new ClipboardEvent('paste', { clipboardData, bubbles: true, cancelable: true }));
    return { plain: dispatch(plain), file: dispatch(file) };
  });
  expect(paste).toEqual({ plain: true, file: false });
  await expect(input).toHaveValue('');
  await input.fill('/');
  await expect(input).toHaveAttribute('aria-expanded', 'false');
  await expect(page.getByRole('region', { name: 'Command suggestions' })).toHaveCount(0);
  await expectTerminalResult(page, '$ make test', ['loading search preview fixtures…', ...searchTestResult]);
  await input.fill('Run the search tests');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expectTerminalExchange(page, 'You: Run the search tests', scripted);
  await expectTerminalResult(page, '→ $ make test', searchTestResult);
  for (const [text, row] of [
    ['How does the preview work? https://search-demo.invalid', '# Review: The preview group can try /search?preview=1.'],
    ['Fix the empty state', '# Review: Clear search now announces the reset.'],
    ['git status', '# Review: Modified: docs/release-notes.md'],
  ]) {
    await input.fill(text);
    await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
    await expectTerminalExchange(page, `You: ${text}`, scripted);
    await expect(output(page).getByText(row, { exact: true })).toBeVisible();
  }
  await expect(output(page).getByText('Clear restores focus but doesn’t announce the reset.', { exact: true })).toBeVisible();
  await expectTerminalResult(page, '→ Edit src/search/Results.svelte', [
    'Sample edit: announce “Search cleared”.',
    'Keep the live region mounted across reset.',
  ]);
  await expectTerminalResult(page, '→ $ make test-search-reset', ['2 passed: reset announcement, focus after clearing.']);
  await expect(output(page).getByRole('link')).toHaveCount(0);
  await expectNoCopyOrAttachments(page);
  await page.getByRole('button', { name: 'Tab', exact: true }).click();
  await expectTerminalExchange(page, 'Keys: Tab', 'Demo response: Key input received; no terminal action ran.');
  await exerciseSampleChecks(page);
  const key = (name: string) => page.getByRole('button', { name, exact: true });
  const arrowMenu = page.getByRole('group', { name: 'Navigation keys' });
  await arrowMenu.getByRole('button', { name: 'Page down', exact: true }).click();
  await expectTerminalExchange(page, 'Keys: PageDown', 'Demo response: Key input received; no terminal action ran.');
  await key('Right').click();
  await expectTerminalExchange(page, 'Keys: Right', '› Sample check 1/3: make test');
  await key('Esc').click();
  await expectTerminalExchange(page, 'Keys: Escape', 'Sample input and check selection cleared.');
  await key('Enter').click();
  await expect(output(page).getByText('No sample input. Use the arrows to select a check.', { exact: true })).toBeVisible();
  await key('Ctrl').click();
  await expect(page.getByLabel('Modifier shortcut character')).toBeFocused();
  await page.keyboard.type('c');
  await expectTerminalExchange(page, 'Keys: ctrl+c', '^C · Sample step interrupted.');
  await back(page);

  await openDemoAgent(page, 'Patch review');
  await expect(output(page)).toContainText('Reviewed the search pagination patch.');
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  const panel = page.getByRole('region', { name: 'Keyboard', exact: true });
  await panel.getByLabel('Terminal text', { exact: true }).fill('sample manual text');
  await panel.getByRole('button', { name: 'Send text', exact: true }).click();
  await expect(panel.getByText('Text delivered; Enter not sent.', { exact: true })).toBeVisible();
  await panel.getByRole('button', { name: 'Enter', exact: true }).click();
  await expect(panel.getByRole('status')).toHaveText('Enter delivered. Sample text committed. The waiting choice is unchanged.');
  await expect(panel.getByRole('status')).toBeInViewport();
  await expect(page.getByText(/delivery is uncertain/i)).toHaveCount(0);
  await panel.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(output(page).getByText('Typed: sample manual text', { exact: true })).toBeInViewport();
  await expectTerminalExchange(page, 'Typed: sample manual text', 'Demo: Text received, waiting for Enter.');
  await expectTerminalExchange(page, 'Keys: Enter', 'Sample text committed. The waiting choice is unchanged.');
  await expect(page.getByRole('button', { name: 'Allow once', exact: true })).toBeVisible();
  await back(page);
}

test('git status agrees with the workspace inspector on a linked worktree', async ({ page }) => {
  const observation = await watchDemo(page);
  await page.goto(demoOrigin);
  await openDemoAgent(page, 'Fixture checks');
  await composer(page).fill('git status');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expectTerminalExchange(page, 'You: git status', scripted);
  await expect(output(page).getByText('On branch fix/one', { exact: true })).toBeInViewport();
  await expect(output(page).getByText('On branch main', { exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const workspace = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await workspace.getByRole('tab', { name: /^Changes\s*\(2\)$/ }).click();
  await expect(workspace.getByTitle('Git branch')).toHaveText('fix/one');
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

async function openSampleConversation(page: Page, expectedEntries = 6) {
  await openDemoAgent(page, 'Release notes');
  await expect(page.getByRole('main', { name: 'Conversation' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Full history' })).toHaveAttribute('aria-pressed', 'true');
  const history = page.getByRole('region', { name: 'Conversation with Release notes' });
  const entries = history.getByRole('article');
  await expect(entries).toHaveCount(expectedEntries);
  for (const prompt of ['Review the search rollout plan', 'Draft a note for the search preview', 'Verify the rollout before review']) {
    await expect(history.getByText(prompt, { exact: true })).toBeVisible();
  }
  await expect(history.getByText(/Let’s start with a preview group/)).toBeVisible();
  await expect(history.getByText(/I added the preview-first rollout note/)).toBeVisible();
  await expect(history.getByText(/preview flag and fallback checks passed/)).toBeVisible();
  await expect(history.locator('pre code')).toHaveText('/search?preview=1');
  for (const [index, tool] of ['Read', 'Edit', 'Bash'].entries()) {
    await expect(entries.nth(index * 2 + 1).locator('summary')).toContainText(new RegExp(`${tool}.*completed`, 'i'));
  }
  return history;
}

async function exerciseConversation(page: Page) {
  const history = await openSampleConversation(page);
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await expect(search).toHaveJSProperty('spellcheck', false);
  await search.fill('sample unmatched conversation phrase');
  await expect(page.getByText('No loaded turns match “sample unmatched conversation phrase”.', { exact: true })).toBeVisible();
  await search.clear();
  const edit = history.locator('summary').filter({ hasText: 'Edit' });
  await expect(edit).toContainText('completed');
  await edit.click();
  await expect(edit.locator('xpath=..').getByText(/path: docs\/release-notes\.md/)).toBeVisible();
  await expect(history.getByText('Updated the sample release notes.', { exact: true })).toBeVisible();
  const rollout = history.getByRole('article').last();
  await rollout.locator('summary').filter({ hasText: 'Bash' }).click();
  await expect(rollout.getByText('command: make test-rollout', { exact: true })).toBeVisible();
  await expect(rollout.getByText('2 passed: preview group, fallback to classic search.', { exact: true })).toBeVisible();
  await expectNoCopyOrAttachments(page);
  const input = composer(page);
  await expect(input).toHaveAttribute('spellcheck', 'false');
  await expect(input).toHaveAttribute('autocorrect', 'off');
  await input.fill('Run the search tests');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expect(history.getByText('Run the search tests', { exact: true })).toBeVisible();
  const suite = history.getByRole('article').last();
  await suite.locator('summary').filter({ hasText: 'Bash' }).click();
  await expect(suite.getByText('command: make test', { exact: true })).toBeVisible();
  await expect(suite.getByText(searchTestResult.join('\n'))).toBeVisible();
  await input.fill(notesPrompt);
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  const echoed = history.getByText(notesPrompt, { exact: true });
  await expect(echoed).toBeVisible();
  await expect(echoed.locator('a, strong, b')).toHaveCount(0);
  const reply = history.getByRole('article').last();
  await expect(reply.getByText('Clear search now announces the reset. Focus still returns to the search box. (Scripted.)', { exact: true })).toBeVisible();
  const resetEdit = reply.locator('summary').filter({ hasText: 'Edit' });
  await expect(resetEdit).toContainText('completed');
  await resetEdit.click();
  await expect(reply.getByText(/Sample edit: announce “Search cleared”\./)).toBeVisible();
  const resetCheck = reply.locator('summary').filter({ hasText: 'Bash' });
  await expect(resetCheck).toContainText('completed');
  await resetCheck.click();
  await expect(reply.getByText('command: make test-search-reset', { exact: true })).toBeVisible();
  await expect(reply.getByText('2 passed: reset announcement, focus after clearing.', { exact: true })).toBeVisible();
  await expectNoCopyOrAttachments(page);
  await input.fill('Unsent demo draft');
  await back(page);
}

async function exerciseLaunch(page: Page) {
  const header = page.getByRole('banner');
  await header.getByRole('button', { name: 'Manage workspaces' }).click();
  const workspaces = page.getByRole('main', { name: 'Workspaces' });
  const rows = workspaces.getByRole('region', { name: 'Herdr workspaces' });
  const computer = workspaces.getByLabel('Computer', { exact: true });
  const rowLaunch = rows.getByRole('button', { name: 'Start Agent', exact: true });
  const writeActions = workspaces.getByRole('button', { name: /Create Workspace|Rename|Reorder|Worktrees|Close|Remove/ });
  await expect(rows.getByText('Project', { exact: true })).toBeVisible();
  await expect(rows.getByText('fix/one', { exact: true })).toBeVisible();
  await expect(rows.getByText('Search', { exact: true })).toBeVisible();
  await expect(rowLaunch).toHaveCount(3);
  await expect(writeActions).toHaveCount(0);
  await computer.selectOption({ label: 'Birch' });
  await expect(rows.getByText('Docs', { exact: true })).toBeVisible();
  await expect(rows.getByText('Examples', { exact: true })).toBeVisible();
  await expect(rowLaunch).toHaveCount(0);
  await expect(writeActions).toHaveCount(0);
  await computer.selectOption({ label: 'Atlas' });
  await rows.getByRole('article').filter({ has: page.getByText('fix/one', { exact: true }) })
    .getByRole('button', { name: 'Start Agent', exact: true }).click();
  const launch = page.getByRole('main', { name: 'Start Agent' });
  await expectDemoHeader(page);
  await expect(launch.getByText(/New tab in workspace fix\/one/)).toBeVisible();
  await expect(launch.getByRole('button', { name: 'project-fix-one', exact: true })).toBeVisible();
  const route = decodeURIComponent(new URL(page.url()).hash);
  expect(route).toContain('"workspaceId":"w4"');
  expect(route).toContain('"cwd":"/home/demo/project-fix-one"');
  await back(page);
  await expect(workspaces).toBeVisible();
  await back(page);
  await expect(page.getByRole('main', { name: 'Agents' })).toBeVisible();

  await header.getByRole('button', { name: 'Start agent', exact: true }).click();
  await expect(page).toHaveURL(`${demoOrigin}/#launch`);
  await expect(launch.getByLabel('Computer', { exact: true }).getByRole('option')).toHaveText(['Atlas']);
  const profile = launch.getByLabel('Agent', { exact: true });
  await expect(profile.getByRole('option')).toHaveText(['Claude Code', 'Codex', 'OpenCode', 'Pi']);
  const start = launch.getByRole('button', { name: 'Start Agent', exact: true });
  await expect(start).toBeDisabled();
  const homeFolder = launch.getByRole('button', { name: 'Home', exact: true });
  await homeFolder.click();
  await launch.getByRole('button', { name: /📁 project$/ }).click();
  await expect(launch.getByRole('button', { name: 'project', exact: true })).toBeVisible();
  await launch.getByRole('button', { name: 'Open parent directory' }).click();
  await expect(launch.getByText(/Agents cannot start in the home folder itself/)).toBeVisible();
  await expect(start).toBeDisabled();
  await homeFolder.click();
  await launch.getByRole('button', { name: /📁 search$/ }).click();
  await profile.selectOption({ label: 'OpenCode' });
  await launch.getByLabel('Name', { exact: true }).fill(launchedName);
  await launch.getByLabel(/Initial task/).fill('Check the sample preview');
  await start.click();
  await expect(page.getByRole('main', { name: 'Agents' })).toBeVisible();
  await openDemoAgent(page, launchedName);
  await expect(output(page)).toContainText(`OpenCode · ${launchedName}`);
  await expect(output(page)).toContainText('Sample agent ready in /home/demo/search. No process was started.');
  await expectTerminalExchange(page, 'You: Check the sample preview', scripted);
  await expect(output(page).getByText('# Review: The preview group can try /search?preview=1.', { exact: true })).toBeVisible();
  await expectNoCopyOrAttachments(page);
  await back(page);
}

test('a launch without an initial task opens with a mostly filled terminal', async ({ page }) => {
  const observation = await watchDemo(page);
  await page.goto(demoOrigin);
  await page.getByRole('banner').getByRole('button', { name: 'Start agent', exact: true }).click();
  const launch = page.getByRole('main', { name: 'Start Agent' });
  await launch.getByRole('button', { name: 'Home', exact: true }).click();
  await launch.getByRole('button', { name: /📁 search$/ }).click();
  await launch.getByLabel('Agent', { exact: true }).selectOption({ label: 'OpenCode' });
  await launch.getByLabel('Name', { exact: true }).fill(launchedName);
  await expect(launch.getByLabel(/Initial task/)).toHaveValue('');
  await launch.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await openDemoAgent(page, launchedName);
  const log = output(page);
  await expect(log.getByText('# Bash: check the search fixtures', { exact: true })).toBeInViewport();
  await expect(log).toContainText('Sample agent ready in /home/demo/search. No process was started.');
  await expect.poll(() => log.evaluate((element) => {
    const bounds = element.getBoundingClientRect();
    const top = Math.max(bounds.top, 0);
    const bottom = Math.min(bounds.bottom, window.innerHeight);
    const covered = Array.from(element.querySelectorAll('[data-terminal-row]')).reduce((height, row) => {
      if (!row.textContent?.trim()) return height;
      const { top: start, bottom: end } = row.getBoundingClientRect();
      return height + Math.max(0, Math.min(end, bottom) - Math.max(start, top));
    }, 0);
    return covered / (bottom - top);
  })).toBeGreaterThan(0.6);
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

async function exerciseInspector(page: Page) {
  await openSampleConversation(page, 10);
  await page.getByRole('button', { name: 'Manage agent', exact: true }).click();
  const manage = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(manage.getByText(demoLabel, { exact: true })).toBeInViewport();
  await expect(manage.getByLabel('Default View')).toBeEnabled();
  await expect(manage.getByRole('button', { name: /Rename|Stop|Clear/ })).toHaveCount(0);
  await manage.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const workspace = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(workspace.getByText(demoLabel, { exact: true })).toBeInViewport();
  await expect(workspace.getByRole('tab', { name: 'Files', exact: true })).toHaveAttribute('aria-selected', 'true');
  const files = workspace.getByRole('complementary', { name: 'Workspace files' });
  await files.getByRole('button', { name: 'README.md' }).click();
  await expect(workspace.getByLabel('Contents of README.md')).toHaveValue(/A sample project for trying the new search page/);
  await files.getByRole('button', { name: 'release-notes.md', exact: true }).click();
  await expect(workspace.getByLabel('Contents of docs/release-notes.md')).toHaveValue(/Start with the search preview group/);
  await files.getByRole('button', { name: 'Results.svelte', exact: true }).click();
  await expect(workspace.getByLabel('Contents of src/search/Results.svelte')).toHaveValue(/searchBox\?\.focus\(\)/);
  const changes = workspace.getByRole('tab', { name: /^Changes\s*\(2\)$/ });
  await changes.click();
  await expect(changes).toHaveAttribute('aria-selected', 'true');
  for (const [path, hunks, removed, added] of [
    ['docs/release-notes.md', ['@@ -1,2 +1,3 @@', '@@ -9,2 +10,3 @@', '@@ -17,2 +19,2 @@'], '-Roll out to everyone.', '+Start with the search preview group.'],
    ['src/search/Results.svelte', ['@@ -5,2 +5,3 @@', '@@ -13,2 +14,3 @@'], '-  <p>No results.</p>', '+    searchBox?.focus();'],
  ] as const) {
    await workspace.getByRole('button', { name: `${path} Modified`, exact: true }).click();
    const diff = workspace.getByLabel(`Diff for ${path}`, { exact: true });
    await expect(diff.locator('.diff-hunk')).toHaveText([...hunks]);
    for (const [tone, text] of [['deletion', removed], ['addition', added]]) {
      const row = diff.locator(`.diff-${tone}`).filter({ hasText: text });
      await expect(row).toBeInViewport({ ratio: 1 });
    }
  }
  await expect(workspace.getByRole('button', { name: /Copy|Download|Edit/ })).toHaveCount(0);
  await workspace.getByRole('button', { name: 'Close workspace inspector' }).click();
  await expect(workspace).toHaveCount(0);
  await back(page);
}

async function expectSampleAppearance(page: Page) {
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'nord');
  await expect(page.locator('html')).toHaveAttribute('data-interface-size', 'compact');
}

async function exerciseSettings(page: Page) {
  const settings = await openSettings(page);
  const atlas = settings.getByRole('region', { name: 'Devices', exact: true })
    .filter({ has: page.getByText('Atlas', { exact: true }) });
  const birch = settings.getByRole('region', { name: 'Devices', exact: true })
    .filter({ has: page.getByText('Birch', { exact: true }) });
  await expect(atlas.getByRole('heading', { name: 'Paired devices' })).toBeVisible();
  await expect(atlas.getByRole('article', { name: 'Slate Tablet, controller', exact: true })).toBeVisible();
  await expect(atlas.getByRole('article', { name: 'Field Phone, reader', exact: true })).toBeVisible();
  await expect(birch.getByRole('article', { name: 'Pocket Reader, reader', exact: true })).toBeVisible();
  await expect(birch.getByRole('button', { name: 'Invite Device' })).toHaveCount(0);
  await atlas.getByRole('button', { name: 'Invite Device' }).click();
  const invite = page.getByRole('dialog', { name: 'Invite Device' });
  await expect(invite.getByText(demoLabel, { exact: true })).toBeInViewport();
  await expect(invite.getByText(/No invitation is created in the demo/)).toBeVisible();
  const role = invite.getByLabel('Role', { exact: true });
  await expect(role).toHaveValue('reader');
  await role.selectOption('controller');
  await role.selectOption('reader');
  const label = 'Sample QR — Herdr demo (not a setup link)';
  const qr = invite.getByRole('img', { name: label, exact: true });
  await expect(qr).toHaveCount(0);
  await invite.getByRole('button', { name: 'Show sample QR' }).click();
  await expect(qr).toBeInViewport();
  const size = 21;
  await expect(qr).toHaveAttribute('viewBox', `-4 -4 ${size + 8} ${size + 8}`);
  const path = await qr.locator('path').getAttribute('d');
  const squares = [...(path ?? '').matchAll(/M(\d+) (\d+)h1v1h-1z/g)];
  expect(squares.map(([square]) => square).join('')).toBe(path);
  const modules = Buffer.alloc(Math.ceil(size * size / 8));
  for (const [, x, y] of squares) {
    const column = Number(x);
    const row = Number(y);
    expect(column).toBeLessThan(size);
    expect(row).toBeLessThan(size);
    const index = row * size + column;
    modules[index >> 3] |= 1 << (7 - (index & 7));
  }
  const packedHerdrDemo = '/lv8FZBukrt05duprsEpB/qv4AAAqkiSDIzL/f/osiNvHABB6/k+cEwjuuiF0pyupSsEOy/rLYA=';
  expect(modules.toString('base64')).toBe(packedHerdrDemo);
  await expect(invite.getByText(`${label}.`, { exact: true })).toBeVisible();
  await expect(page.getByRole('link')).toHaveCount(0);
  await expect(invite.getByRole('button', { name: /Create Invitation|Copy link/ })).toHaveCount(0);
  await invite.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(settings.getByText(/One-use invitation link|Invitation created/)).toHaveCount(0);
  await expect(atlas.getByRole('link')).toHaveCount(0);
  await expect(atlas.getByRole('article')).toHaveCount(2);

  const notifications = settings.getByRole('region', { name: 'Notifications', exact: true });
  await expect(notifications.getByRole('status')).toHaveText('Notification delivery: Off (unavailable in demo)');
  await expect(notifications.getByRole('button')).toHaveCount(0);
  await expect(notifications.getByText(/does not use your notification permission/)).toBeVisible();
  const speech = settings.getByRole('region', { name: 'Read Aloud', exact: true });
  await expect(speech.getByRole('list', { name: 'Sample voices' }).getByRole('listitem')).toHaveText(['English — Meadow', 'French — Willow']);
  await expect(speech.getByText(/downloads, removal and playback are unavailable/)).toBeVisible();
  await expect(speech.getByRole('button')).toHaveCount(0);
  await expect(settings.getByRole('region', { name: 'Updates' }).getByText(/updates are unavailable in the demo/)).toBeVisible();
  await settings.getByRole('group', { name: 'Theme', exact: true }).getByRole('button', { name: 'light', exact: true }).click();
  await settings.getByRole('group', { name: 'Interface Size', exact: true }).getByRole('button', { name: 'Large', exact: true }).click();
  await back(page);
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('html')).toHaveAttribute('data-interface-size', 'large');
}

const target = { relay_id: 'saved', server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'saved-term', generation: 1, agent_session_id: 'saved-session' };
const draftIdentity = JSON.stringify([target, 'codex', '/sample/project']);
const draftKey = `herdr_prompt_draft_v1:${encodeURIComponent(draftIdentity)}`;

// Headless Shell can report denied after a grant. Use full pinned Chromium for these tests.
const permissionTest = test.extend<object, { browser: Browser }>({
  browser: [async ({ browser, browserName, playwright }, use) => {
    if (browserName !== 'chromium') return use(browser);
    const chromium = await playwright.chromium.launch({ channel: 'chromium' });
    try {
      await use(chromium);
    } finally {
      await chromium.close();
    }
  }, { scope: 'worker' }],
});

for (const permission of ['granted', 'denied'] as const) {
  permissionTest(`demo home is isolated with notifications ${permission}`, async ({ context, page: realApp, browserName }) => {
    test.setTimeout(60_000);
    const sockets: string[] = [];
    await context.routeWebSocket('**/*', async (socket) => {
      sockets.push(socket.url());
      await socket.close();
    });
    await context.route('**/*', (route) => new URL(route.request().url()).origin === appOrigin
      ? route.continue() : route.abort());
    await realApp.goto(appOrigin);
    await expect(realApp.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
    await realApp.evaluate(async () => { await navigator.serviceWorker.register('/sw.js'); });
    const realController = () => realApp.evaluate(() => {
      const worker = navigator.serviceWorker.controller;
      return worker && { url: worker.scriptURL, state: worker.state };
    });
    const expectedController = { url: `${appOrigin}/sw.js`, state: 'activated' };
    await expect.poll(realController).toEqual(expectedController);

    const local = {
      herdr_relays: JSON.stringify([{ id: 'saved', label: 'Saved relay', url: 'wss://saved-relay.invalid', token: '', paired: true }]),
      herdr_device_auth_v1: JSON.stringify({ version: 1, relays: { saved: {
        kind: 'credential', id: 'saved-credential', deviceId: 'saved-device', version: 1,
        secret: 'A'.repeat(43), locale: 'en', issuedAt: Date.now(), role: 'controller',
      } } }),
      [draftKey]: JSON.stringify({ version: 1, identity: draftIdentity, text: 'Disposable draft', updatedAt: Date.now() }),
      herdr_hidden_servers: '["demo-atlas"]',
      herdr_theme: 'light',
      herdr_push_enabled: 'true',
    };
    const session = { herdr_pending_relay_updates: '{"sentinel":true}' };
    await realApp.evaluate(({ local, session }) => {
      for (const [key, value] of Object.entries(local)) localStorage.setItem(key, value);
      for (const [key, value] of Object.entries(session)) sessionStorage.setItem(key, value);
    }, { local, session });

    // Playwright's mobile WebKit has no Notification API; simulate its getter there only.
    const simulatedPermission = browserName === 'webkit' ? permission : undefined;
    if (simulatedPermission) {
      test.info().annotations.push({ type: 'permission simulation', description: 'Notification API unavailable in mobile WebKit' });
    } else {
      await context.grantPermissions(permission === 'granted' ? ['notifications'] : [], { origin: demoOrigin });
    }
    const popup = realApp.waitForEvent('popup');
    await realApp.evaluate(() => { window.open('about:blank'); });
    const demo = await popup;
    expect(await demo.evaluate(() => sessionStorage.getItem('herdr_pending_relay_updates'))).toBe(session.herdr_pending_relay_updates);
    const observation = await watchDemo(demo, { simulatedPermission });
    await demo.goto(demoOrigin);
    await expectSampleHome(demo);
    await expect(demo).toHaveURL(`${demoOrigin}/`);
    const root = demo.locator('html');
    await expect(root).toHaveAttribute('data-theme', 'nord');
    await expect(root).toHaveAttribute('spellcheck', 'false');
    await expect(root).toHaveAttribute('autocorrect', 'off');
    await expect(demo.getByText('Saved relay')).toHaveCount(0);
    await expectCleanDemoOrigin(demo);

    await demo.getByRole('button', { name: 'Open Patch review on Atlas' }).click();
    await expect(demo).toHaveURL(/#pane=r3\./);
    await expectDemoHeader(demo);
    await expect(demo.getByLabel('Agent terminal output')).toContainText('Reviewed the search pagination patch.');
    await demo.getByRole('button', { name: 'Back', exact: true }).click();
    await expectSampleHome(demo);
    await openSettings(demo);
    await demo.getByRole('button', { name: 'Back', exact: true }).click();
    await expectSampleHome(demo);

    await demo.getByRole('button', { name: 'Search all agents' }).click();
    const dialog = demo.getByRole('dialog', { name: 'Jump to agent' });
    await expect(dialog.getByText(demoLabel, { exact: true })).toBeVisible();
    const search = dialog.getByRole('searchbox', { name: 'Search agents and workspaces' });
    await expect(search).toHaveJSProperty('spellcheck', false);
    await search.fill('sample unmatched agent phrase');
    await expect(dialog.getByText('No matching agents.', { exact: true })).toBeVisible();
    await search.clear();
    await demo.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);

    await exerciseTerminal(demo);
    await exerciseConversation(demo);
    await exerciseInspector(demo);
    await exerciseLaunch(demo);
    await exerciseSettings(demo);
    await demo.getByRole('button', { name: 'Preview first', exact: true }).click();
    await expect(demo.getByRole('region', { name: 'Needs input' }).getByRole('button', { name: 'Open Release plan on Atlas' })).toHaveCount(0);
    await expectCleanDemoOrigin(demo);

    const atlas = demo.getByRole('button', { name: 'Atlas', exact: true });
    await atlas.click();
    await expect(demo.getByText('make test', { exact: true })).toHaveCount(0);
    await expectDemoHeader(demo);
    const secondTab = await context.newPage();
    const secondObservation = await watchDemo(secondTab, { simulatedPermission });
    await secondTab.goto(demoOrigin);
    await expectSampleHome(secondTab);
    await expectSampleAppearance(secondTab);
    await expect(demoAgent(secondTab, launchedName)).toHaveCount(0);
    const freshHistory = await openSampleConversation(secondTab);
    await expect(freshHistory.getByText(notesPrompt, { exact: true })).toHaveCount(0);
    await expect(composer(secondTab)).toHaveValue('');
    await expectCleanDemoOrigin(secondTab);
    expect(secondObservation.unexpected).toEqual([]);
    await secondTab.close();

    await demo.getByRole('button', { name: 'Open Docs preview on Birch' }).click();
    await expect(demo).toHaveURL(/#pane=r3\./);
    await expect(demo.getByLabel('Agent terminal output')).toContainText('Building the sample docs preview…');
    await expect(demo.getByText(/Reader access is read only/)).toBeVisible();
    await expect(composer(demo)).toBeDisabled();
    await expect(demo.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
    for (const name of ['Tab', 'Enter']) {
      await expect(demo.getByRole('button', { name, exact: true })).toBeDisabled();
    }
    await expect(demo.getByRole('button', { name: 'Keyboard', exact: true })).toHaveCount(0);
    await expect(demo.getByRole('button', { name: 'Inspect workspace' })).toBeDisabled();
    await expectNoCopyOrAttachments(demo);
    await expectCleanDemoOrigin(demo);
    await demo.reload();
    await expectSampleHome(demo);
    await expect(demo).toHaveURL(`${demoOrigin}/`);
    await expectSampleAppearance(demo);
    await expect(demoAgent(demo, launchedName)).toHaveCount(0);
    const resetHistory = await openSampleConversation(demo);
    await expect(resetHistory.getByText(notesPrompt, { exact: true })).toHaveCount(0);
    await expect(composer(demo)).toHaveValue('');
    await back(demo);
    await openDemoAgent(demo, 'Search polish');
    await expect(output(demo)).toContainText('OpenCode · Search polish');
    await expect(output(demo)).not.toContainText('git status');
    await expectCleanDemoOrigin(demo);
    expect(observation.storage).toEqual(['0,0', '0,0']);
    const expectedPermission = simulatedPermission ? 'unavailable' : permission;
    expect(observation.permissions).toEqual([expectedPermission, expectedPermission]);
    expect(observation.unexpected).toEqual([]);
    expect(sockets).toEqual([]);

    const stored = await realApp.evaluate(({ localKeys, sessionKeys }) => ({
      local: Object.fromEntries(localKeys.map((key) => [key, localStorage.getItem(key)])),
      session: Object.fromEntries(sessionKeys.map((key) => [key, sessionStorage.getItem(key)])),
    }), { localKeys: Object.keys(local), sessionKeys: Object.keys(session) });
    expect(stored).toEqual({ local, session });
    expect(await realController()).toEqual(expectedController);
  });
}

async function expectWaitingKeyFeedback(page: Page) {
  const arrows = page.getByRole('button', { name: 'Arrow keys', exact: true });
  if (await arrows.getAttribute('aria-expanded') === 'false') await arrows.click();
  const down = page.getByRole('button', { name: 'Down', exact: true });
  await down.click();
  await expectKeyNote(page, 'Sample checks are unavailable while this agent is waiting for a choice.');
  await page.getByRole('button', { name: 'Enter', exact: true }).click();
  await expectKeyNote(page, 'Use the waiting controls to make a choice.');
  await expect(down).toBeInViewport({ ratio: 1 });
}

for (const { agent, other, choice, from, outcome } of [
  { agent: 'Patch review', other: 'Release plan', choice: 'Allow once', from: 'the agent list', outcome: 'Approval: Allow once.' },
  { agent: 'Patch review', other: 'Release plan', choice: 'Decline', from: 'the agent view', outcome: 'Approval: Decline.' },
  { agent: 'Release plan', other: 'Patch review', choice: 'Preview first', from: 'the agent list', outcome: 'Rollout choice: Preview first.' },
  { agent: 'Release plan', other: 'Patch review', choice: 'All at once', from: 'the agent view', outcome: 'Rollout choice: All at once.' },
]) {
  test(`answers ${agent} from ${from}`, async ({ page }) => {
    const observation = await watchDemo(page);
    await page.goto(demoOrigin);
    await expectSampleHome(page);
    const waiting = page.getByRole('region', { name: 'Needs input' });
    const sibling = waiting.getByRole('button', { name: `Open ${other} on Atlas`, exact: true });
    const siblingText = await sibling.innerText();
    if (from === 'the agent view') {
      if (agent === 'Release plan') {
        await waiting.getByRole('button', { name: 'More…', exact: true }).click();
        await expectDemoHeader(page);
        await expect(page.getByText(rolloutQuestion, { exact: true })).toBeVisible();
        const selected = page.getByRole('radio', { name: choice });
        await selected.check();
        await expect(output(page)).toBeHidden();
        for (const width of [390, 320]) {
          await test.step(`keeps waiting key feedback readable at ${width} px`, async () => {
            await page.setViewportSize({ width, height: 844 });
            await expectWaitingKeyFeedback(page);
            await expect(selected).toBeChecked();
          });
        }
        await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
        const panel = page.getByRole('region', { name: 'Keyboard', exact: true });
        await expect(output(page)).toBeHidden();
        await expect(selected).toBeHidden();
        await expect(panel.getByLabel('Terminal text', { exact: true })).toHaveValue('');
        await panel.getByRole('button', { name: 'Enter', exact: true }).click();
        await expect(panel.getByRole('status')).toHaveText('Enter delivered. Use the waiting controls to make a choice.');
        await expect(panel.getByRole('status')).toBeInViewport();
        await panel.getByRole('button', { name: 'Close', exact: true }).click();
        await expect(selected).toBeChecked();
        await page.getByRole('button', { name: 'Choose rollout', exact: true }).click();
      } else {
        await openDemoAgent(page, agent);
        await expectWaitingKeyFeedback(page);
        await expectTerminalExchange(page, 'Keys: Enter', 'Use the waiting controls to make a choice.');
        await expect(output(page)).not.toContainText('› Sample check');
        await expect(page.getByRole('button', { name: 'Allow once', exact: true })).toBeEnabled();
        await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
        const panel = page.getByRole('region', { name: 'Keyboard', exact: true });
        await panel.getByLabel('Terminal text', { exact: true }).fill('How does the preview work?');
        await panel.getByRole('button', { name: 'Send text', exact: true }).click();
        await expect(panel.getByText('Text delivered; Enter not sent.', { exact: true })).toBeVisible();
        await panel.getByRole('button', { name: 'Close', exact: true }).click();
        await page.getByRole('button', { name: choice, exact: true }).click();
        await expect(output(page)).toContainText(outcome);
        const arrows = page.getByRole('button', { name: 'Arrow keys', exact: true });
        if (await arrows.getAttribute('aria-expanded') === 'false') await arrows.click();
        await page.getByRole('button', { name: 'Down', exact: true }).click();
        await expect(output(page).getByText('› Sample check 1/3: make test', { exact: true })).toBeInViewport();
        await page.getByRole('button', { name: 'Enter', exact: true }).click();
        await expectTerminalResult(page, 'Scripted check · $ make test', [...searchTestResult, 'No command ran.']);
        await expect(output(page)).not.toContainText(scripted);
      }
      await expect(output(page)).toContainText(outcome);
      await back(page);
    } else {
      await waiting.getByRole('button', { name: choice, exact: true }).click();
    }
    await expect(waiting.getByRole('button', { name: `Open ${agent} on Atlas` })).toHaveCount(0);
    await expect(sibling).toHaveText(siblingText);
    await openDemoAgent(page, agent);
    await expect(output(page)).toContainText(outcome);
    await expect(page.getByRole('button', { name: choice, exact: true })).toHaveCount(0);
    await expectCleanDemoOrigin(page);
    await page.reload();
    await expectSampleHome(page);
    expect(observation.unexpected).toEqual([]);
  });
}

test('shows five workspaces and activity relative to the visit', async ({ page }) => {
  const observation = await watchDemo(page);
  await page.clock.setFixedTime(new Date('2031-04-06T14:08:00Z'));
  await page.goto(demoOrigin);
  await expectSampleHome(page);
  const workspaces = page.getByRole('region', { name: 'Workspaces', exact: true });
  const summary = (name: string) => workspaces.locator('.workspace-card > summary')
    .filter({ has: page.getByText(name, { exact: true }) });
  for (const [name, host, age] of [['Project', 'Atlas', '16m'], ['Search', 'Atlas', '9m'], ['Docs', 'Birch', '1h'], ['Examples', 'Birch', '1h']]) {
    await expect(summary(name).getByText(`@${host}`, { exact: true })).toBeVisible();
    await expect(summary(name).locator('time')).toHaveText(age);
  }
  const project = summary('Project');
  await expect(project.getByText('1 worktree', { exact: true })).toBeVisible();
  if (!await workspaces.getByRole('region', { name: 'fix/one worktree' }).isVisible()) await project.click();
  const worktree = workspaces.getByRole('region', { name: 'fix/one worktree' });
  await expect(worktree.getByText('fix/one', { exact: true })).toBeVisible();
  await expect(worktree.locator('time')).toHaveText('32m');
  const history = await openSampleConversation(page);
  await expect(history.locator('time').last()).toHaveAttribute('datetime', '2031-04-06T13:52:00.000Z');
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

test.describe('demo in landscape', () => {
  test.use({ viewport: { width: 844, height: 390 } });

  test('the demo label stays in view when the search dialog scrolls', async ({ page }) => {
    const observation = await watchDemo(page);
    await page.goto(demoOrigin);
    await expect(page.getByRole('button', { name: 'Open Patch review on Atlas' })).toBeVisible();
    await page.getByRole('button', { name: 'Search all agents' }).click();
    const dialog = page.getByRole('dialog', { name: 'Jump to agent' });
    const label = dialog.getByText(demoLabel, { exact: true });
    await expect(label).toBeInViewport({ ratio: 1 });

    const content = dialog.locator('.dialog-content');
    await content.evaluate((element) => { element.scrollTop = element.scrollHeight; });
    await expect.poll(() => content.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    await expect(label).toBeInViewport({ ratio: 1 });
    expect(observation.unexpected).toEqual([]);
  });

  test('reloading a scrolled Agents view returns to the top', async ({ page }) => {
    const observation = await watchDemo(page);
    await page.goto(demoOrigin);
    await expectSampleHome(page);
    await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);

    await page.reload();
    await expectSampleHome(page);
    expect(await page.evaluate(() => ({ scrollY: window.scrollY, restoration: history.scrollRestoration })))
      .toEqual({ scrollY: 0, restoration: 'manual' });
    expect(observation.unexpected).toEqual([]);
  });
});

test('browser history cannot reopen a route from before the demo reload', async ({ page }) => {
  const observation = await watchDemo(page);
  await page.goto(demoOrigin);
  await expectSampleHome(page);
  await page.getByRole('button', { name: 'Open Patch review on Atlas' }).click();
  const output = page.getByLabel('Agent terminal output');
  await expect(output).toContainText('Reviewed the search pagination patch.');
  await openSettings(page);

  await page.goBack();
  await expect(output).toContainText('Reviewed the search pagination patch.');
  await page.goForward();
  await expect(page.getByRole('main', { name: 'Settings' })).toBeVisible();

  await page.reload();
  await expectSampleHome(page);
  await expect(page).toHaveURL(`${demoOrigin}/`);
  await page.goBack();
  await expect(page).toHaveURL(`${demoOrigin}/`);
  await expectSampleHome(page);
  await page.goForward();
  await expect(page).toHaveURL(`${demoOrigin}/`);
  await expectSampleHome(page);
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

test('the demo ignores setup links on load and during navigation', async ({ page }) => {
  const initialSearch = '?demo=1';
  const observation = await watchDemo(page, { initialSearch });
  const setup = '#setup=synthetic-demo-link&label=Not+a+sample+computer&relay=wss%3A%2F%2Fsetup.invalid';
  await page.goto(`${demoOrigin}/${initialSearch}${setup}`);
  await expectSampleHome(page);
  await expect(page).toHaveURL(`${demoOrigin}/`);

  await page.evaluate((hash) => { location.hash = hash; }, setup);
  await expect(page).toHaveURL(`${demoOrigin}/${setup}`);
  await expectSampleHome(page);
  await expect(page.getByText('Not a sample computer')).toHaveCount(0);
  await openSettings(page);
  await page.reload();
  await expectSampleHome(page);
  await page.goBack();
  await expect(page).toHaveURL(`${demoOrigin}/`);
  await expectSampleHome(page);
  await expectCleanDemoOrigin(page);
  expect(observation.unexpected).toEqual([]);
});

test('the real app ignores a demo query and fetches no demo code', async ({ page }) => {
  const requests: string[] = [];
  page.on('request', (request) => requests.push(new URL(request.url()).pathname));
  await page.goto(`${appOrigin}/?demo=1`);
  await expect(page.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
  await expect(page.getByRole('button', { name: 'Settings', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Demo — sample data' })).toHaveCount(0);
  expect(requests).toContain(`/${release.files.javascript.path}`);
  expect(requests.filter((path) => /demo/i.test(path))).toEqual([]);
});

for (const block of [demoLoader, demoScript]) {
  test(`the demo reports a load failure and recovers on reload when ${block} is blocked`, async ({ page, request }) => {
    const options = { block };
    const observation = await watchDemo(page, options);
    await page.goto(demoOrigin);
    await expect(page).toHaveURL(`${demoOrigin}/`);
    await expect(page.getByRole('alert')).toHaveText('Demo could not load. Reload this page to try again.');
    await expect(page.getByRole('heading', { name: 'Demo — sample data' })).toBeVisible();
    await expect(page.getByRole('button', { name: /Settings|Open app home/ })).toHaveCount(0);
    expect(observation.blocked).toEqual([block]);
    expect((await request.get(`${demoOrigin}/sw.js`)).status()).toBe(404);
    options.block = '';
    await page.reload();
    await expectSampleHome(page);
    await expect(page).toHaveURL(`${demoOrigin}/`);
    await expect(page.locator('#demo-startup')).toHaveCount(0);
    expect(observation.unexpected).toEqual([]);
  });
}

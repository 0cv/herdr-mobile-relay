import { expect, test, type Page } from '@playwright/test';
import { readFile, readdir, rename, writeFile } from 'node:fs/promises';
import { createConnection } from 'node:net';

interface Operation { method?: string; params?: { pane_id?: string; text?: string; keys?: string[] }; argv?: string[]; started_at?: string }
async function operations(): Promise<Operation[]> {
  const values: Operation[] = [];
  for (const path of [process.env.HERDR_ATTENTION_OPERATIONS!, process.env.HERDR_ATTENTION_INPUT_OPERATIONS!]) {
    try { values.push(...(await readFile(path, 'utf8')).trim().split('\n').filter(Boolean).map(line => JSON.parse(line))); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
  }
  return values.sort((a, b) => (a.started_at || '').localeCompare(b.started_at || '')).map(op => op.method ? { method: op.method, params: op.params } : { argv: op.argv });
}
const mutations = async () => (await operations()).filter(op => op.method === 'pane.send_input' || ['send-keys', 'send-text', 'resize'].includes(op.argv?.[1] || ''));
const button = (page: Page, name: string) => page.getByRole('button', { name, exact: true });
// The app appends an update note to the Settings button's accessible name when an update is available.
const settingsButton = (page: Page) => page.getByRole('button', { name: /^Settings(,|$)/ });
const openDialog = (page: Page, name: string) => button(page, `Open ${name} on Captured relay`).click();

async function metadata(value: Record<string, { cwd: string }>) {
  const path = process.env.HERDR_ATTENTION_METADATA!;
  await writeFile(path + '.next', JSON.stringify(value), { mode: 0o600 });
  await rename(path + '.next', path);
}

async function control(command: Record<string, unknown>): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const socket = createConnection(process.env.HERDR_ATTENTION_CONTROL!);
    socket.setEncoding('utf8');
    socket.setTimeout(3_000, () => socket.destroy(new Error('Private fixture control timed out')));
    socket.once('connect', () => socket.write(JSON.stringify(command) + '\n'));
    socket.once('error', reject);
    let response = '';
    socket.on('data', chunk => {
      response += chunk;
      if (!response.includes('\n')) return;
      socket.end();
      try {
        const result = JSON.parse(response.slice(0, response.indexOf('\n')));
        if (!result.ok) throw new Error(result.error || 'Private fixture control failed');
        resolve(result.state);
      } catch (error) { reject(error); }
    });
  });
}

async function openManual(page: Page, keyboard = false) {
  if (keyboard) { await button(page, 'Keyboard').focus(); await page.keyboard.press('Enter'); }
  else await button(page, 'Keyboard').click();
  const panel = page.locator('section[aria-label="Keyboard"]');
  await expect(panel).toBeVisible();
  await expect(page.getByLabel('Terminal text', { exact: true })).toBeFocused();
  await expect(panel.getByRole('button', { name: 'Enter', exact: true })).toBeEnabled();
}

async function bootController(page: Page) {
  const relayURL = process.env.HERDR_ATTENTION_WS_URL;
  if (!relayURL) throw new Error('Private relay fixture is required');
  let credential = '';
  try { credential = await readFile(process.env.HERDR_ATTENTION_CREDENTIALS!, 'utf8'); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
  await page.addInitScript(({ relayURL, credential }) => {
    if (credential) localStorage.setItem('herdr_device_auth_v1', credential);
    localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'captured-attention', label: 'Captured relay', url: relayURL, token: 'attention-browser-test-key-32byt' }]));
    localStorage.setItem('herdr_default_agent_view', 'conversation');
    const sockets: WebSocket[] = [];
    const Native = WebSocket;
    window.WebSocket = class extends Native { constructor(url: string | URL, protocols?: string | string[]) { super(url, protocols); sockets.push(this); } };
    Object.assign(window, { closePrivateRelay: () => sockets.forEach(socket => socket.close()), privateRelayConnections: () => sockets.length, privateRelayReadyState: () => sockets.at(-1)?.readyState });
  }, { relayURL, credential });
  await page.goto('/');
  await expect(page.getByRole('img', { name: '1/1 relays connected', exact: true })).toBeVisible();
  await writeFile(process.env.HERDR_ATTENTION_CREDENTIALS!, await page.evaluate(() => localStorage.getItem('herdr_device_auth_v1') || ''), { mode: 0o600 });
}

test('literal input stays explicit in Terminal through reconnect and target switch', async ({ page }) => {
  await bootController(page);
  const initial = (await mutations()).length;
  await openDialog(page, 'arbitrary-dialog');
  const prompt = page.getByLabel('Prompt', { exact: true });
  await expect(prompt).toBeEnabled();
  await prompt.fill('editable Conversation draft');
  expect((await mutations()).length).toBe(initial);
  await expect(button(page, 'Keyboard')).toHaveCount(0);
  await expect(button(page, 'Enter')).toHaveCount(0);
  await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
  await button(page, 'Terminal view').click();
  await expect(page.getByRole('log')).toContainText('/violet-comet');
  // Terminal can restore the saved shared draft between fill selecting the old text and typing the new.
  await expect(async () => {
    await prompt.fill('unrelated normal draft');
    await expect(prompt).toHaveValue('unrelated normal draft', { timeout: 1_000 });
  }).toPass();
  await openManual(page);
  expect((await mutations()).length).toBe(initial);
  await page.getByText('Current terminal context', { exact: true }).click();
  await expect(page.locator('section[aria-label="Keyboard"] pre')).toContainText('/violet-comet');
  const literal = '  /another-command 界😀  ';
  await page.getByLabel('Terminal text', { exact: true }).fill(literal);
  await button(page, 'Send text').click();
  await expect(page.getByText('Text delivered; Enter not sent.', { exact: true })).toBeVisible();
  await expect.poll(async () => (await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: 'arbitrary-dialog', text: literal } }]);
  await button(page, 'Enter').click();
  await expect(page.getByText('Enter delivered.', { exact: true })).toBeVisible();
  expect((await mutations()).slice(initial + 1)).toEqual([{ argv: ['pane', 'send-keys', 'arbitrary-dialog', 'Enter'] }]);
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain(literal);
  await button(page, 'Close').click();
  await expect(button(page, 'Keyboard')).toBeFocused();
  await expect(prompt).toHaveValue('unrelated normal draft');
  await button(page, 'Conversation history').click();
  await expect(button(page, 'Keyboard')).toHaveCount(0);
  await button(page, 'Terminal view').click();
  await openManual(page);
  await page.getByLabel('Terminal text', { exact: true }).fill('terminal-only');
  await button(page, 'Send text').click();
  await expect(page.getByText('Text delivered; Enter not sent.', { exact: true })).toBeVisible();
  const beforeReconnect = (await mutations()).length;
  await page.evaluate(() => (window as unknown as { closePrivateRelay: () => void }).closePrivateRelay());
  await expect(button(page, 'Keyboard')).toBeVisible();
  await expect.poll(() => page.evaluate(() => (window as unknown as { privateRelayReadyState: () => number }).privateRelayReadyState())).toBe(1);
  await expect(page.locator('.prompt-composer .agent-progress, .prompt-composer [aria-atomic="true"]')).toHaveCount(0);
  await openManual(page);
  expect((await mutations()).length).toBe(beforeReconnect);
  await button(page, 'Back').click();
  await openDialog(page, 'orchestration-dialog');
  const terminalView = button(page, 'Terminal view');
  if (await terminalView.isVisible()) await terminalView.click();
  await openManual(page);
  await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
  await page.getByLabel('Terminal text', { exact: true }).fill('generic guidance');
  await button(page, 'Send text').click();
  await expect(page.getByText('Text delivered; Enter not sent.', { exact: true })).toBeVisible();
  expect((await mutations()).at(-1)).toEqual({ method: 'pane.send_input', params: { pane_id: 'orchestration-dialog', text: 'generic guidance' } });
  await button(page, 'Close').click();
  await button(page, 'Back').click();
  await button(page, 'Open qoder-approval on Captured relay').click();
  await expect(button(page, 'Allow once')).toBeVisible();
  const beforeMode = (await mutations()).length;
  const approvalTerminal = button(page, 'Terminal view');
  if (await approvalTerminal.isVisible()) await approvalTerminal.click();
  await openManual(page);
  await button(page, 'Close').click();
  await expect(button(page, 'Keyboard')).toBeFocused();
  await expect(button(page, 'Allow once')).toBeVisible();
  expect((await mutations()).length).toBe(beforeMode);
});

test('Terminal question forms retain an editable draft and keyboard-accessible manual escape', async ({ page }) => {
  await bootController(page);
  for (const target of ['codex-notes', 'claude-multi-select']) {
    await openDialog(page, target);
    const terminal = button(page, 'Terminal view');
    if (await terminal.isVisible()) await terminal.click();
    await expect(page.locator('main.question-only')).toBeVisible();
    const normal = page.getByLabel('Prompt', { exact: true });
    await expect(normal).toBeEnabled();
    await expect(normal).toBeInViewport();
    await expect(button(page, 'Keyboard')).toBeInViewport();
    const before = (await mutations()).length;
    await normal.fill('question-local draft');
    await openManual(page, true);
    await expect(page.getByLabel('Terminal text', { exact: true })).toBeInViewport();
    await button(page, 'Close').focus();
    await page.keyboard.press('Enter');
    await expect(button(page, 'Keyboard')).toBeFocused();
    await expect(normal).toHaveValue('question-local draft');
    await expect(page.getByRole('group').first()).toBeVisible();
    expect((await mutations()).length).toBe(before);
    await button(page, 'Back').click();
  }
});

const manualViews: string[] = ['Terminal'];
for (const view of manualViews) {
  test(`${view} can leave uncertain delivery without replay or losing the send guard`, async ({ page }) => {
    await bootController(page);
    const target = view === 'Conversation' ? 'arbitrary-dialog' : 'codex-notes';
    await openDialog(page, target);
    const switchView = button(page, `${view} view`);
    if (await switchView.isVisible()) await switchView.click();
    await expect(page.getByRole('main', { name: view === 'Conversation' ? 'Conversation' : /^Questions for / })).toBeVisible();
    await openManual(page);
    const initial = (await mutations()).length;
    await page.getByLabel('Terminal text', { exact: true }).fill('uncertain-fixture-text');
    await button(page, 'Send text').click();
    const panel = page.locator('section[aria-label="Keyboard"]');
    await expect(panel.getByRole('button', { name: 'Enter', exact: true })).toBeDisabled();
    await expect(button(page, 'Close')).toBeEnabled();
    await button(page, 'Close').click();
    await expect(button(page, 'Keyboard')).toBeFocused();
    await expect(page.getByText('Delivery is uncertain. Tap Keyboard to check the terminal before sending.', { exact: true })).toBeVisible();
    await page.getByLabel('Prompt', { exact: true }).fill('unrelated draft after uncertainty');
    await expect(button(page, 'Send prompt')).toBeDisabled();
    await expect(button(page, 'Enter')).toBeDisabled();
    if (view === 'Terminal') {
      await expect(page.locator('.question-form button[type="submit"]')).toBeDisabled();
      await expect(page.getByRole('radio').first()).toBeEnabled();
    }
    const connections = await page.evaluate(() => (window as unknown as { privateRelayConnections: () => number }).privateRelayConnections());
    await page.evaluate(() => (window as unknown as { closePrivateRelay: () => void }).closePrivateRelay());
    await expect.poll(() => page.evaluate(() => (window as unknown as { privateRelayConnections: () => number }).privateRelayConnections())).toBeGreaterThan(connections);
    await expect.poll(() => page.evaluate(() => (window as unknown as { privateRelayReadyState: () => number }).privateRelayReadyState())).toBe(1);
    await expect(page.locator('.prompt-composer .agent-progress, .prompt-composer [aria-atomic="true"]')).toHaveCount(0);
    await expect(button(page, 'Send prompt')).toBeDisabled();
    await expect(button(page, 'Enter')).toBeDisabled();
    if (view === 'Terminal') await expect(page.locator('.question-form button[type="submit"]')).toBeDisabled();
    await expect(page.getByText('Delivery is uncertain. Tap Keyboard to check the terminal before sending.', { exact: true })).toBeVisible();
    await button(page, 'Keyboard').click();
    await expect(panel).toBeVisible();
    await expect(page.getByLabel('Terminal text', { exact: true })).toBeFocused();
    await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
    await expect(panel.getByRole('button', { name: 'Send text', exact: true })).toBeDisabled();
    await expect(panel.getByRole('button', { name: 'Enter', exact: true })).toBeDisabled();
    expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: target, text: 'uncertain-fixture-text' } }]);
    await expect(button(page, 'I checked the terminal')).toBeEnabled();
    await button(page, 'I checked the terminal').click();
    await expect(panel.getByRole('button', { name: 'Enter', exact: true })).toBeEnabled();
    await button(page, 'Close').click();
    await expect(button(page, 'Enter')).toBeEnabled();
    if (view === 'Terminal') await expect(page.locator('.question-form button[type="submit"]')).toBeEnabled();
    expect((await mutations()).length).toBe(initial + 1);
  });
}

for (const view of manualViews) {
  test(`${view} preserves the uncertainty fence across same-target cwd changes`, async ({ page }) => {
    await bootController(page);
    const target = view === 'Conversation' ? 'arbitrary-dialog' : 'codex-notes';
    await openDialog(page, target);
    const switchView = button(page, `${view} view`);
    if (await switchView.isVisible()) await switchView.click();
    await openManual(page);
    const route = page.url();
    const initial = (await mutations()).length;
    await page.getByLabel('Terminal text', { exact: true }).fill('uncertain-fixture-text');
    await button(page, 'Send text').click();
    await expect(button(page, 'I checked the terminal')).toBeVisible();
    try {
      await metadata({ [target]: { cwd: '/tmp/same-target-new-directory' } });
      await expect(button(page, 'Keyboard')).toBeVisible();
      expect(page.url()).toBe(route);
      await expect(page.getByText('Delivery is uncertain. Tap Keyboard to check the terminal before sending.', { exact: true })).toBeVisible();
      await page.getByLabel('Prompt', { exact: true }).fill('draft after metadata changed');
      await expect(button(page, 'Send prompt')).toBeDisabled();
      await expect(button(page, 'Enter')).toBeDisabled();
      if (view === 'Terminal') await expect(page.locator('.question-form button[type="submit"]')).toBeDisabled();
      await button(page, 'Keyboard').click();
      await expect(button(page, 'I checked the terminal')).toBeEnabled();
      await expect(button(page, 'Enter')).toBeDisabled();
      await button(page, 'I checked the terminal').click();
      await expect(button(page, 'Enter')).toBeEnabled();
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: target, text: 'uncertain-fixture-text' } }]);
    } finally { await metadata({}); }
  });

  test(`${view} rejects a post-uncertainty pre-lock response after unlock until explicit post-recovery inspection`, async ({ page }) => {
    await page.clock.install();
    await page.addInitScript(() => {
      Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class {} });
      Object.defineProperty(navigator, 'credentials', { configurable: true, value: {
        create: async () => ({ rawId: new Uint8Array([1, 2, 3]).buffer }),
        get: async () => ({ type: 'public-key' }),
      } });
    });
    await bootController(page);
    await settingsButton(page).click();
    await page.getByRole('switch', { name: 'Require Fingerprint / Device Unlock', exact: true }).click();
    await expect(page.getByText('Device verification is enabled.', { exact: true })).toBeVisible();
    await button(page, 'Back').click();
    await openDialog(page, 'orchestration-dialog');
    const switchView = button(page, `${view} view`);
    if (await switchView.isVisible()) await switchView.click();
    await openManual(page);
    await page.clock.pauseAt(new Date(await page.evaluate(() => Date.now()) + 100));
    const initial = (await mutations()).length;
    const beforeLock = `pre-lock-${view.toLowerCase()}-${Date.now()}`;
    const afterUnlock = `post-unlock-${view.toLowerCase()}-${Date.now()}`;
    const snapshot = await control({ cmd: 'snapshot' });
    const originalContent = (snapshot.content as Record<string, string>)['orchestration-dialog'];
    const arrived = async (barrier: string) => (await readdir(process.env.HERDR_ATTENTION_BARRIERS!)).some(name => name.startsWith(barrier + '.') && name.endsWith('.arrived'));
    try {
      await page.getByLabel('Terminal text', { exact: true }).fill('uncertain-fixture-text');
      await button(page, 'Send text').click();
      await expect(button(page, 'I checked the terminal')).toBeVisible();
      await page.clock.runFor(10);
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: 'Delayed pre-lock snapshot\n> ' });
      await control({ cmd: 'inject', command: 'pane read', injection: { before_response_barrier: beforeLock } });
      await page.clock.runFor(2_000);
      await expect.poll(() => arrived(beforeLock)).toBe(true);
      await page.evaluate(() => document.dispatchEvent(new Event('freeze')));
      const unlock = page.getByRole('dialog', { name: 'Unlock Herdr', exact: true });
      await expect(unlock).toBeVisible();
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: 'orchestration-dialog', text: 'uncertain-fixture-text' } }]);
      await page.clock.runFor(10);
      await button(page, 'Unlock').click();
      await expect(unlock).toBeHidden();
      await expect(button(page, 'Keyboard')).toBeVisible();
      await control({ cmd: 'clear_injection', command: 'pane read' });
      await control({ cmd: 'release_barrier', barrier: beforeLock });
      const context = page.locator('section[aria-label="Keyboard"] pre');
      await expect(context).toContainText('Delayed pre-lock snapshot');
      await page.getByLabel('Prompt', { exact: true }).fill('draft before recovery observation');
      await expect(button(page, 'Send prompt')).toBeDisabled();
      await expect(button(page, 'Enter')).toBeDisabled();
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: 'Post-unlock snapshot\n> ' });
      await control({ cmd: 'inject', command: 'pane read', injection: { before_response_barrier: afterUnlock } });
      await button(page, 'Keyboard').click();
      await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
      await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
      await page.getByLabel('Terminal text', { exact: true }).fill('transient after unlock');
      await expect.poll(() => arrived(afterUnlock)).toBe(true);
      for (const label of ['I checked the terminal', 'Send text', 'Enter']) await expect(button(page, label)).toBeDisabled();
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: 'orchestration-dialog', text: 'uncertain-fixture-text' } }]);
      await control({ cmd: 'clear_injection', command: 'pane read' });
      await control({ cmd: 'release_barrier', barrier: afterUnlock });
      await expect(context).toContainText('Post-unlock snapshot');
      await expect(button(page, 'I checked the terminal')).toBeEnabled();
      await expect(button(page, 'Send text')).toBeDisabled();
      await expect(button(page, 'Enter')).toBeDisabled();
      await button(page, 'I checked the terminal').click();
      await expect(button(page, 'Send text')).toBeEnabled();
      await expect(button(page, 'Enter')).toBeEnabled();
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: 'orchestration-dialog', text: 'uncertain-fixture-text' } }]);
      const storage = await page.evaluate(() => JSON.stringify(localStorage));
      for (const text of ['uncertain-fixture-text', 'transient after unlock', 'draft before recovery observation']) expect(storage).not.toContain(text);
    } finally {
      await control({ cmd: 'clear_injection', command: 'pane read' });
      await control({ cmd: 'release_barrier', barrier: beforeLock });
      await control({ cmd: 'release_barrier', barrier: afterUnlock });
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: originalContent });
    }
  });

  test(`${view} rejects a delayed pre-send read until a correlated post-outcome observation`, async ({ page }) => {
    await page.clock.install();
    await bootController(page);
    await openDialog(page, 'orchestration-dialog');
    const switchView = button(page, `${view} view`);
    if (await switchView.isVisible()) await switchView.click();
    await openManual(page);
    await page.clock.pauseAt(new Date(await page.evaluate(() => Date.now()) + 100));
    const initial = (await mutations()).length;
    const barrier = `pre-send-${view.toLowerCase()}-${Date.now()}`;
    const snapshot = await control({ cmd: 'snapshot' });
    const originalContent = (snapshot.content as Record<string, string>)['orchestration-dialog'];
    try {
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: 'Delayed pre-send snapshot\n> ' });
      await control({ cmd: 'inject', command: 'pane read', injection: { before_response_barrier: barrier } });
      await page.clock.runFor(2_000);
      await expect.poll(async () => (await readdir(process.env.HERDR_ATTENTION_BARRIERS!)).some(name => name.startsWith(barrier + '.') && name.endsWith('.arrived'))).toBe(true);
      await page.getByLabel('Terminal text', { exact: true }).fill('uncertain-fixture-text');
      await button(page, 'Send text').click();
      await expect(button(page, 'I checked the terminal')).toBeVisible();
      await expect(button(page, 'I checked the terminal')).toBeDisabled();
      await page.clock.runFor(10);
      await control({ cmd: 'clear_injection', command: 'pane read' });
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: 'Post-outcome snapshot\n> ' });
      await control({ cmd: 'release_barrier', barrier });
      await page.getByText('Current terminal context', { exact: true }).click();
      const context = page.locator('section[aria-label="Keyboard"] pre');
      // A newer read can replace the held one, and the page then drops its late response instead of showing it.
      await context.filter({ hasText: 'Delayed pre-send snapshot' }).waitFor({ timeout: 2_000 }).catch(() => {});
      await expect(button(page, 'I checked the terminal')).toBeDisabled();
      await expect(button(page, 'Enter')).toBeDisabled();
      // The relay rejects a read that overlaps its own check of the changed screen; the next 2 s refresh reads again.
      // Earlier runs leave their screens in the relay's scrollback, so wait for what a post-outcome read enables.
      await expect(async () => {
        await page.clock.runFor(2_000);
        await expect(button(page, 'I checked the terminal')).toBeEnabled({ timeout: 1_000 });
      }).toPass();
      await expect(context).toContainText('Post-outcome snapshot');
      await expect(button(page, 'Enter')).toBeDisabled();
      await button(page, 'I checked the terminal').click();
      await expect(button(page, 'Enter')).toBeEnabled();
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: 'orchestration-dialog', text: 'uncertain-fixture-text' } }]);
    } finally {
      await control({ cmd: 'clear_injection', command: 'pane read' });
      await control({ cmd: 'release_barrier', barrier });
      await control({ cmd: 'set_content', pane: 'orchestration-dialog', content: originalContent });
    }
  });
}

for (const { target, transition } of [
  { target: 'arbitrary-dialog', transition: 'reconnect' },
  { target: 'orchestration-dialog', transition: 'reconnect' },
  { target: 'arbitrary-dialog', transition: 'metadata' },
]) {
  test(`Conversation keeps ${target} uncluttered and uncertainty fenced across ${transition} and view changes`, async ({ page }) => {
    await bootController(page);
    await openDialog(page, target);
    await expect(page.getByRole('main', { name: 'Conversation', exact: true })).toBeVisible();
    const absentTerminalControls = async () => {
      for (const name of ['Keyboard', 'Enter', 'Tab', 'Ctrl', 'Send text', 'I checked the terminal']) await expect(button(page, name)).toHaveCount(0);
      await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
      await expect(page.getByLabel('Terminal text', { exact: true })).toHaveCount(0);
    };
    await absentTerminalControls();
    await expect(page.getByText('history answer', { exact: true })).toBeVisible();
    await page.getByLabel('Prompt', { exact: true }).fill('editable Conversation draft');
    const initial = (await mutations()).length;
    await button(page, 'Terminal view').click();
    await openManual(page, true);
    await page.getByLabel('Terminal text', { exact: true }).fill('uncertain-fixture-text');
    await button(page, 'Send text').click();
    await expect(button(page, 'I checked the terminal')).toBeVisible();
    await button(page, 'Conversation history').click();
    const route = page.url();
    try {
      await expect(page.getByText('Delivery is uncertain. In Terminal, tap Keyboard to check before sending.', { exact: true })).toBeVisible();
      await absentTerminalControls();
      await page.getByLabel('Prompt', { exact: true }).fill('draft while delivery is uncertain');
      await expect(button(page, 'Send prompt')).toBeDisabled();
      await expect(button(page, 'Attach files')).toBeDisabled();
      await page.getByLabel('Prompt', { exact: true }).press('Control+Enter');
      if (transition === 'metadata') {
        await button(page, 'Terminal view').click();
        await button(page, 'Keyboard').click();
        await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
        await metadata({ [target]: { cwd: '/tmp/same-target-new-directory' } });
        await expect(button(page, 'Keyboard')).toBeVisible();
        await button(page, 'Conversation history').click();
        expect(page.url()).toBe(route);
      } else {
        const connections = await page.evaluate(() => (window as unknown as { privateRelayConnections: () => number }).privateRelayConnections());
        await page.evaluate(() => (window as unknown as { closePrivateRelay: () => void }).closePrivateRelay());
        await expect.poll(() => page.evaluate(() => (window as unknown as { privateRelayConnections: () => number }).privateRelayConnections())).toBeGreaterThan(connections);
        await expect.poll(() => page.evaluate(() => (window as unknown as { privateRelayReadyState: () => number }).privateRelayReadyState())).toBe(1);
      }
      await expect(button(page, 'Send prompt')).toBeDisabled();
      await absentTerminalControls();
      await button(page, 'Terminal view').click();
      await button(page, 'Keyboard').click();
      await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
      await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
      await expect(button(page, 'Enter')).toBeDisabled();
      await expect(button(page, 'I checked the terminal')).toBeEnabled();
      await button(page, 'I checked the terminal').click();
      await expect(button(page, 'Enter')).toBeEnabled();
      await button(page, 'Conversation history').click();
      await absentTerminalControls();
      await expect(page.getByText(/^Delivery is uncertain\./)).toHaveCount(0);
      expect((await mutations()).slice(initial)).toEqual([{ method: 'pane.send_input', params: { pane_id: target, text: 'uncertain-fixture-text' } }]);
      expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('uncertain-fixture-text');
    } finally { if (transition === 'metadata') await metadata({}); }
  });
}

test('manual refresh preserves configured Terminal history across repeated reads', async ({ page }) => {
  await bootController(page);
  await settingsButton(page).click();
  const historySetting = page.getByRole('group', { name: 'Terminal History', exact: true }).getByRole('button', { name: '500', exact: true });
  await historySetting.click();
  await expect(historySetting).toHaveAttribute('aria-pressed', 'true');
  await button(page, 'Back').click();
  await openDialog(page, 'arbitrary-dialog');
  await button(page, 'Terminal view').click();
  const transcript = page.getByLabel('Full terminal transcript', { exact: true });
  await expect(transcript).toHaveValue(/scrollback-anchor-000/);
  const original = await transcript.inputValue();
  const recentReads = async () => (await operations()).filter(operation => operation.argv?.[0] === 'pane' && operation.argv[1] === 'read' && operation.argv[2] === 'arbitrary-dialog' && ['recent', 'recent-unwrapped'].includes(operation.argv[6]) && ['30', '100', '500', '1000', '10000'].includes(operation.argv[4]));
  const initialReads = (await recentReads()).length;
  const initialMutations = (await mutations()).length;
  await openManual(page);
  await expect.poll(async () => (await recentReads()).slice(initialReads).filter(operation => operation.argv?.[4] === '500').length).toBeGreaterThanOrEqual(3);
  const smallerReads = (await recentReads()).slice(initialReads).filter(operation => Number(operation.argv?.[4]) < 500);
  expect(smallerReads).toEqual([]);
  await expect(transcript).toHaveValue(original);
  await button(page, 'Close').click();
  await expect(transcript).toHaveValue(original);
  expect((await mutations()).length).toBe(initialMutations);
});

test('an authenticated reader cannot send text, keys, scroll or resize mutations', async ({ page, browser }) => {
  await bootController(page);
  await settingsButton(page).click();
  await button(page, 'Invite Device').click();
  const invite = page.getByRole('dialog', { name: 'Invite Device', exact: true });
  await invite.getByLabel('Device name', { exact: true }).fill('Private manual-input reader');
  await invite.getByLabel('Role', { exact: true }).selectOption('reader');
  await invite.getByRole('button', { name: 'Create Invitation', exact: true }).click();
  const link = await page.getByLabel('One-use invitation link', { exact: true }).inputValue();
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' });
  try {
    await context.addInitScript(() => { Object.defineProperty(navigator, 'standalone', { value: true }); localStorage.setItem('herdr_default_agent_view', 'conversation'); });
    const reader = await context.newPage();
    await reader.goto(link);
    await expect(reader.getByRole('img', { name: '1/1 relays connected', exact: true })).toBeVisible();
    await openDialog(reader, 'arbitrary-dialog');
    await expect(reader.getByLabel('Prompt', { exact: true })).toBeDisabled();
    await expect(button(reader, 'Keyboard')).toHaveCount(0);
    const before = (await mutations()).length;
    await button(reader, 'Terminal view').click();
    await expect(button(reader, 'Enter')).toBeDisabled();
    await expect(button(reader, 'Keyboard')).toHaveCount(0);
    await reader.getByRole('log').dispatchEvent('wheel', { deltaY: -120 });
    await reader.setViewportSize({ width: 390, height: 740 });
    expect((await mutations()).length).toBe(before);
  } finally { await context.close(); }
});

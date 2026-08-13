import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const webRoot = process.env.HERDR_WEB_ROOT || 'dist';
const APP_METADATA = JSON.parse(
  readFileSync(resolve(webRoot, 'version.json'), 'utf8'),
) as { version: string; assets: number };
const APP_RELEASE = APP_METADATA.version;

interface RelayFixture {
  id: string;
  label: string;
  url: string;
  token: string;
}

interface BootOptions {
  standalone?: boolean;
}

async function boot(page: Page, relays: RelayFixture[] = [], path = '/', options: BootOptions = {}) {
  await page.addInitScript(({ savedRelays, standalone }) => {
    if (savedRelays.length) localStorage.setItem('herdr_relays', JSON.stringify(savedRelays));
    if (standalone) {
      const nativeMatchMedia = window.matchMedia.bind(window);
      Object.defineProperty(window, 'matchMedia', {
        configurable: true,
        value(query: string) {
          const result = nativeMatchMedia(query);
          if (query === '(display-mode: standalone)') {
            Object.defineProperty(result, 'matches', { configurable: true, value: true });
          }
          return result;
        },
      });
    }
    const nativeSetTimeout = window.setTimeout.bind(window);
    window.setTimeout = ((handler: TimerHandler, timeout?: number, ...args: unknown[]) =>
      nativeSetTimeout(handler, timeout === 3000 ? 30 : timeout, ...args)) as typeof window.setTimeout;

    const sockets: MockSocket[] = [];
    const commands: Record<string, unknown>[] = [];
    const socketCommands: Record<string, unknown>[][] = [];
    let nextInteraction: Record<string, unknown> | null = null;
    let autoCommands = true;

    class MockSocket {
      static OPEN = 1;
      static CONNECTING = 0;
      static CLOSING = 2;
      static CLOSED = 3;
      readyState = MockSocket.CONNECTING;
      onopen: (() => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      readonly index: number;
      constructor(readonly url: string) {
        this.index = sockets.length;
        sockets.push(this);
        socketCommands.push([]);
        queueMicrotask(() => {
          this.readyState = MockSocket.OPEN;
          this.onopen?.();
        });
      }
      send(serialized: string) {
        const message = JSON.parse(serialized) as Record<string, unknown>;
        commands.push(message);
        socketCommands[this.index].push(message);
        if (['e2ee_client_hello', 'read_pane', 'watch_pane', 'unwatch_pane', 'pane_applied', 'get_activity', 'list_directories', 'refresh_agents'].includes(String(message.type))) return;
        if (!autoCommands) return;
        if (message.type === 'upload_image') {
          queueMicrotask(() => this.server({
            type: 'upload_result', ok: true, request_id: message.request_id, pane_id: message.pane_id,
            path: '/home/test/.cache/herdr-mobile-relay/uploads/shot.png',
          }));
          return;
        }
        if (message.type === 'list_slash_commands') {
          queueMicrotask(() => this.server({
            type: 'command_result', request_id: message.request_id, ok: true, phase: 'completed',
            data: {
              commands: [
                { command: '/help', description: 'Show the full command reference and explain every available action', source: 'builtin' },
                { command: '/copy', description: 'Copy the latest agent response', source: 'builtin' },
                { command: '/model', description: 'Choose the active model', source: 'builtin' },
                { command: '/plan', description: 'Enter plan mode', argument_hint: '[prompt]', source: 'builtin' },
                ...Array.from({ length: 18 }, (_, index) => ({
                  command: `/sample-${index + 1}`,
                  description: `Example command ${index + 1}`,
                  source: 'builtin',
                })),
              ],
              truncated: false,
            },
          }));
          return;
        }
        if (String(message.type).startsWith('workspace_')) {
          let data: Record<string, unknown>;
          switch (message.type) {
            case 'workspace_tree':
              data = {
                root: '/work/mobile',
                entries: [
                  { path: 'README.md', name: 'README.md', kind: 'file', size: 24 },
                  { path: 'src', name: 'src', kind: 'directory' },
                  { path: 'src/main.ts', name: 'main.ts', kind: 'file', size: 42 },
                ],
              };
              break;
            case 'workspace_file':
              data = {
                path: message.path,
                media_type: 'text/markdown',
                kind: 'text',
                text: '# Workspace preview\n\nRead-only file contents.',
                size: 46,
              };
              break;
            case 'workspace_git_status':
              data = {
                available: true,
                branch: 'feature/mobile',
                files: [{ path: 'README.md', status: ' M' }],
              };
              break;
            default:
              data = {
                path: message.path,
                diff: [
                  'diff --git a/README.md b/README.md',
                  '--- a/README.md',
                  '+++ b/README.md',
                  '@@ -1 +1 @@',
                  '-Old text',
                  '+Read-only change',
                ].join('\n'),
              };
          }
          queueMicrotask(() => this.server({
            type: 'command_result',
            action: message.type,
            request_id: message.request_id,
            ok: true,
            phase: 'completed',
            data,
          }));
          return;
        }
        if (message.type === 'get_conversation_history') {
          const older = Boolean(message.before);
          queueMicrotask(() => this.server({
            type: 'command_result',
            action: message.type,
            request_id: message.request_id,
            ok: true,
            phase: 'completed',
            data: {
              available: true,
              entries: older
                ? [{ id: 'turn-1', timestamp: '2026-08-12T09:00:00Z', role: 'user', text: 'first retained question' }]
                : [
                  {
                    id: 'turn-2',
                    timestamp: '2026-08-12T09:00:01Z',
                    role: 'assistant',
                    text: 'intermediate progress update',
                    tools: [{ id: 'tool-1', name: 'Read', input: 'README.md', output: 'file contents' }],
                  },
                  {
                    id: 'turn-2-final',
                    timestamp: '2026-08-12T09:00:02Z',
                    role: 'assistant',
                    text: '# middle retained answer',
                  },
                  { id: 'turn-3', timestamp: '2026-08-12T09:00:03Z', role: 'user', text: 'latest retained question' },
                ],
              has_more: !older,
              total: 4,
              file_truncated: true,
            },
          }));
          return;
        }
        if (message.type === 'copy_agent_response') {
          queueMicrotask(() => this.server({
            type: 'command_result',
            action: message.type,
            request_id: message.request_id,
            ok: true,
            phase: 'completed',
            data: {
              text: '# Remote markdown response\n\n- Exact copied output',
              source: 'clipboard',
              chars: 49,
              lines: 3,
            },
          }));
          return;
        }
        if (message.type === 'push_subscribe' || message.type === 'push_unsubscribe') return;
        const phase = message.type === 'answer_question' && nextInteraction
          ? 'advanced'
          : message.type === 'navigate_question' && nextInteraction ? 'navigated' : 'confirmed';
        let data: Record<string, unknown> = {};
        if ((message.type === 'answer_question' || message.type === 'navigate_question') && nextInteraction) data = { interaction: nextInteraction };
        else if (message.type === 'agent_start') data = { pane_id: 'w1:pre-placement' };
        else if (message.type === 'lease_pane_size') data = { columns: message.columns };
        else if (message.type === 'agent_clear') data = {
          pane_id: 'w1:pre-clear', name: 'clear-codex-123', cwd: '/home/test/Development/relay',
        };
        if (message.type === 'answer_question' || message.type === 'navigate_question') nextInteraction = null;
        queueMicrotask(() => this.server({
          type: 'command_result', action: message.type, request_id: message.request_id, ok: true, phase, data,
        }));
      }
      close() { this.readyState = MockSocket.CLOSED; }
      server(message: unknown) { this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent); }
      serverClose() { this.readyState = MockSocket.CLOSED; this.onclose?.(); }
    }

    Object.defineProperty(window, 'WebSocket', { configurable: true, value: MockSocket });
    Object.assign(window, {
      __relayCommands: commands,
      __relaySockets: sockets,
      __relaySocketCommands(index: number) { return socketCommands[index] || []; },
      __relayServer(index: number, message: unknown) { sockets[index]?.server(message); },
      __relayClose(index: number) { sockets[index]?.serverClose(); },
      __relayNextInteraction(interaction: Record<string, unknown>) { nextInteraction = interaction; },
      __relayAutoCommands(enabled: boolean) { autoCommands = enabled; },
    });
  }, {
    savedRelays: relays,
    standalone: options.standalone ?? false,
  });
  await page.goto(path);
}

async function socketCount(page: Page) {
  return page.evaluate(() => (window as any).__relaySockets.length as number);
}

async function server(page: Page, index: number, message: unknown) {
  await page.evaluate(({ socketIndex, payload }) => (window as any).__relayServer(socketIndex, payload), { socketIndex: index, payload: message });
}

async function commands(page: Page) {
  return page.evaluate(() => (window as any).__relayCommands as Record<string, unknown>[]);
}

async function commandsForSocket(page: Page, index: number) {
  return page.evaluate((socketIndex) => {
    const harnessWindow = window as unknown as {
      __relaySocketCommands(next: number): Record<string, unknown>[];
    };
    return harnessWindow.__relaySocketCommands(socketIndex);
  }, index);
}

async function setAutoCommands(page: Page, enabled: boolean) {
  await page.evaluate((value) => {
    const harnessWindow = window as unknown as { __relayAutoCommands(next: boolean): void };
    harnessWindow.__relayAutoCommands(value);
  }, enabled);
}

async function handshake(page: Page, index: number, overrides: Record<string, unknown> = {}) {
  await server(page, index, {
    type: 'push_config', protocol: 2, version: 'abc1234', host: index ? 'mac' : 'fedora',
    capabilities: ['attention_classification', 'clear_activities', 'directory_browser', 'self_update', 'structured_questions', 'slash_commands'],
    agent_profiles: [{ id: 'codex', label: 'Codex' }, { id: 'claude', label: 'Claude Code' }],
    ...overrides,
  });
}

const fedora = { id: 'fedora', label: 'Fedora', url: 'wss://fedora.example', token: '' };

test('keeps activity cards inside the page and confirms permanent deletion', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'activity_history',
    activities: [{
      id: 'activity-1', timestamp: Date.now(), kind: 'finished', summary: 'codex completed',
      pane_id: 'w1:p1', project: 'herdr-mobile-relay', agent: 'codex', status: 'completed',
    }],
  });

  await page.getByRole('button', { name: 'Activity history' }).click();
  const activity = page.getByRole('button', { name: /codex completed/ });
  await expect(page.getByRole('heading', { name: 'Last 24 hours' })).toBeVisible();
  await expect(page.locator('.activity-summary-metrics > div').filter({ hasText: 'Completed' })).toContainText('1');
  await expect(activity).toBeVisible();
  const headingBox = await page.getByRole('heading', { name: 'Activity', level: 2 }).boundingBox();
  const deleteBox = await page.getByRole('button', { name: 'Delete all' }).boundingBox();
  const box = await activity.boundingBox();
  const chevronBox = await activity.locator('.activity-chevron').boundingBox();
  const viewport = page.viewportSize();
  expect(headingBox).not.toBeNull();
  expect(deleteBox).not.toBeNull();
  expect(box).not.toBeNull();
  expect(chevronBox).not.toBeNull();
  expect(viewport).not.toBeNull();
  const headingCenter = headingBox!.y + headingBox!.height / 2;
  const deleteCenter = deleteBox!.y + deleteBox!.height / 2;
  expect(Math.abs(headingCenter - deleteCenter)).toBeLessThanOrEqual(2);
  expect(viewport!.width - (box!.x + box!.width)).toBeGreaterThanOrEqual(10);
  expect(box!.x + box!.width - (chevronBox!.x + chevronBox!.width)).toBeGreaterThanOrEqual(10);

  await page.getByRole('button', { name: 'Delete all' }).click();
  const dialog = page.getByRole('dialog', { name: 'Delete all activity?' });
  await expect(dialog).toContainText('Running agents and their conversations are not affected.');
  await dialog.getByRole('button', { name: 'Delete all' }).click();
  await expect(page.getByText('No activity yet.')).toBeVisible();
  expect((await commands(page)).find((command) => command.type === 'clear_activities'))
    .toMatchObject({ type: 'clear_activities', protocol: 2 });
});

test('sizes captured activity text with terminal typography', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'activity_history',
    activities: [{
      id: 'activity-typography',
      timestamp: Date.now(),
      summary: 'omp completed',
      agent: 'omp',
      status: 'completed',
      extract: '\ue0b0 status\nCaptured activity output',
    }],
  });

  await page.getByRole('button', { name: 'Activity history' }).click();
  await page.getByRole('button', { name: /omp completed/ }).click();
  const extract = page.getByRole('region', { name: 'Captured excerpt' }).locator('pre');
  await expect(extract).toContainText('Captured activity output');

  const typography = await extract.evaluate((element) => {
    const root = document.documentElement;
    const measure = (size: 'compact' | 'regular' | 'large') => {
      root.dataset.interfaceSize = size;
      return {
        root: Number.parseFloat(getComputedStyle(root).fontSize),
        extract: Number.parseFloat(getComputedStyle(element).fontSize),
      };
    };
    return {
      compact: measure('compact'),
      regular: measure('regular'),
      large: measure('large'),
      family: getComputedStyle(element).fontFamily,
    };
  });
  expect(typography.compact.extract).toBeCloseTo(typography.compact.root * .72, 2);
  expect(typography.regular.extract).toBeCloseTo(typography.regular.root * .82, 2);
  expect(typography.large.extract).toBeCloseTo(typography.large.root * .94, 2);
  expect(typography.family).toContain('Herdr Nerd Symbols');
});

test('keeps device verification modal until native authentication succeeds', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_require_device_unlock', 'true');
    localStorage.setItem('herdr_device_unlock_credential', 'AQ');
    Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class {} });
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: {
        get: () => new Promise((resolve) => {
          Object.assign(window, { __resolveDeviceVerification: () => resolve({}) });
        }),
      },
    });
  });
  await boot(page, [fedora]);

  const unlockDialog = page.getByRole('dialog', { name: 'Unlock Herdr' });
  await expect(unlockDialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(unlockDialog).toBeVisible();
  expect(await socketCount(page)).toBe(0);

  await page.evaluate(() => (window as any).__resolveDeviceVerification());
  await expect(unlockDialog).toBeHidden();
  await expect.poll(() => socketCount(page)).toBe(1);
});

test('imports quick setup and merges agents from multiple relays', async ({ page }) => {
  await boot(
    page,
    [],
    '/#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation&relay=wss%3A%2F%2Frelay-fedora.example.com',
    { standalone: true },
  );
  await expect(page.getByRole('button', { name: 'Activity history' }).locator('svg')).toBeVisible();
  await expect.poll(() => socketCount(page)).toBe(1);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[0]))
    .toMatchObject({
      label: 'Fedora Workstation',
      url: 'wss://relay-fedora.example.com',
      token: '0123456789abcdef0123456789abcdef',
    });
  expect(await page.evaluate(() => location.hash)).toBe('');

  await page.evaluate(() => {
    location.hash = '#setup=abcdef0123456789abcdef0123456789&label=Mac&relay=wss%3A%2F%2Fmac.example';
  });
  await expect.poll(() => socketCount(page)).toBe(3);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[1]))
    .toMatchObject({
      label: 'Mac',
      url: 'wss://mac.example',
      token: 'abcdef0123456789abcdef0123456789',
    });
  expect(await page.evaluate(() => location.hash)).toBe('');
  await page.evaluate(() => {
    const relays = JSON.parse(localStorage.getItem('herdr_relays') || '[]');
    localStorage.setItem('herdr_relays', JSON.stringify(relays.map((relay: RelayFixture) => ({
      ...relay,
      token: '',
    }))));
  });
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(2);
  await expect.poll(async () =>
    (await commandsForSocket(page, 0)).some((command) => command.type === 'register_app_origin')).toBe(true);
  const base = 0;
  await handshake(page, base);
  await handshake(page, base + 1);
  await server(page, base, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app', agent: 'codex' }] });
  await server(page, base + 1, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Mac app', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  const headerBox = await page.getByRole('banner').boundingBox();
  const connectionBox = await page.getByRole('img', { name: /relays connected/ }).boundingBox();
  const settingsBox = await page.getByRole('button', { name: 'Settings' }).boundingBox();
  expect(headerBox && connectionBox && settingsBox).toBeTruthy();
  const leadingInset = connectionBox!.x + connectionBox!.width / 2 - headerBox!.x;
  const trailingInset = headerBox!.x + headerBox!.width - settingsBox!.x - settingsBox!.width / 2;
  expect(Math.abs(leadingInset - trailingInset)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Open Fedora app on Fedora Workstation' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open Mac app on Mac' })).toBeVisible();
});

test('sorts a cold idle snapshot by the latest Herdr activity', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1',
        status: 'idle',
        project: 'herdr-mobile-relay',
        tab_label: 'codex_dummy',
        agent: 'codex',
        updated_at: 0,
        activity_seq: 735,
      },
      {
        pane_id: 'w1:p2',
        status: 'idle',
        project: 'herdr-mobile-relay',
        tab_label: 'codex_review_bugs',
        agent: 'codex',
        updated_at: 0,
        activity_seq: 794,
      },
    ],
  });

  await expect(page.locator('.workspace-card .workspace-tab h3').first()).toHaveText('codex_review_bugs');
});

test('keeps an opened workspace expanded across tab navigation and inventory refreshes', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const agents = [
    {
      pane_id: 'w1:p1',
      workspace_id: 'workspace-mobile',
      status: 'idle',
      project: 'Mobile app',
      agent: 'codex',
      cwd: '/work/mobile',
    },
    {
      pane_id: 'w2:p1',
      workspace_id: 'workspace-docs',
      status: 'idle',
      project: 'Docs',
      agent: 'claude',
      cwd: '/work/docs',
    },
  ];
  await server(page, 0, { type: 'agents', agents });

  const workspace = page.locator('details.workspace-card').filter({ hasText: 'Mobile app' });
  await expect(workspace).toHaveCount(1);
  await workspace.locator('summary').click();
  await expect(workspace).toHaveAttribute('open', '');
  await workspace.getByRole('button', { name: 'Open Mobile app on Fedora' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(workspace).toHaveAttribute('open', '');


  await server(page, 0, {
    type: 'agents',
    agents: [{ ...agents[0], activity_seq: 2 }, { ...agents[1] }],
  });

  await expect(workspace).toHaveAttribute('open', '');
  await expect(workspace.getByRole('button', { name: 'Open Mobile app on Fedora' })).toBeVisible();
});

test('reconnects and blocks mutations for an incompatible relay protocol', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    protocol: 1,
    version: 'old',
    capabilities: ['attention_classification', 'clear_activities', 'directory_browser', 'structured_questions', 'slash_commands'],
  });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Old relay', agent: 'codex', options: ['Approve once', 'Deny'] }] });
  await page.getByRole('button', { name: 'Settings, relay update needs attention' }).click();
  await expect(page.getByText(/Relay outdated/)).toBeVisible();
  await page.getByRole('button', { name: 'How to update Fedora' }).click();
  const updateHelp = page.getByRole('dialog', { name: 'Update Fedora' });
  await expect(updateHelp).toContainText('herdr plugin install 0cv/herdr-mobile-relay');
  await updateHelp.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Remove Fedora' }).click();
  const removeDialog = page.getByRole('dialog', { name: 'Remove Fedora?' });
  await expect(removeDialog).toContainText('You will need its setup link or connection details to add it again.');
  await removeDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(page.getByText('wss://fedora.example')).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Approve once' }).click();
  await expect(page.getByRole('status').filter({ hasText: /protocol v1/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  await page.evaluate(() => (window as any).__relayClose(0));
  await expect.poll(() => socketCount(page)).toBe(2);
});

test('centers plan keys and enables text only for the terminal editor', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['structured_questions', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', project: 'Old controls', agent: 'opencode',
      options: ['Approve once', 'Deny'],
    }],
  });
  await expect(page.getByRole('heading', { name: 'Needs inspection' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Approve once' })).toBeHidden();
  await page.getByRole('button', { name: 'Open Old controls on Fedora' }).click();
  const terminal = page.getByRole('log');
  const history = Array.from({ length: 120 }, (_, index) => `question history ${index + 1}`);
  const choiceContent = [
    ...history,
    'Other (type your own)',
    'Enter select · n note · ↑/↓ move · Tab/←/→ · Esc cancel',
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: choiceContent,
  });
  const planInput = page.getByPlaceholder('Needs inspection — use terminal controls');
  await expect(planInput).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Attach image' })).toBeDisabled();
  const directions = await page.locator('.generic-menu-actions > div > button').evaluateAll((elements) =>
    elements.slice(0, 4).map((element) => ({
      key: element.querySelector('kbd')?.textContent,
      label: element.textContent?.replace(element.querySelector('kbd')?.textContent || '', '').trim(),
    })));
  expect(directions).toEqual([
    { key: 'Left', label: 'Previous' },
    { key: 'Up', label: 'Up' },
    { key: 'Down', label: 'Down' },
    { key: 'Right', label: 'Next' },
  ]);
  const promptContent = [
    ...history,
    'Custom answer: Which weekend?',
    '>',
    'enter or ctrl+q submit  esc cancel  ctrl+g external editor',
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: promptContent,
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  const terminalInput = page.getByPlaceholder('Type terminal input…');
  await expect(terminalInput).toBeEnabled();
  await terminalInput.fill('custom weekend');
  await page.getByRole('button', { name: 'Submit terminal text' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_text' || command.type === 'send_keys')
    .map((command) => ({ type: command.type, text: command.text, keys: command.keys }))).toEqual([
      { type: 'send_text', text: 'custom weekend', keys: undefined },
      { type: 'send_keys', text: undefined, keys: ['Enter'] },
    ]);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: `${promptContent}\nAccepted custom weekend`,
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Old controls', agent: 'opencode' }],
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Enter' })).toBeEnabled();

  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'chat',
      project: 'Chat ready', agent: 'codex',
    }],
  });
  await expect(page.getByPlaceholder('Type a reply…')).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Approve once' })).toBeHidden();
});

test('shows inventory failure instead of zero agents and recovers without reconnecting', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    inventory: {
      state: 'error',
      error_code: 'protocol_mismatch',
      message: 'Run `herdr server live-handoff` on this computer, then refresh.',
      last_attempt_at: 123,
      last_success_at: 0,
      stale: false,
    },
  });
  await server(page, 0, { type: 'agents', agents: [] });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toContainText('live-handoff');
  await expect(page.getByText('No chat agents are running.')).toBeHidden();
  await expect(page.getByRole('img', { name: /agent inventory unavailable/ })).toBeVisible();

  await server(page, 0, {
    type: 'inventory_status',
    state: 'ready',
    error_code: '',
    message: '',
    last_attempt_at: 200,
    last_success_at: 200,
    stale: false,
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Recovered relay', agent: 'codex' }],
  });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Open Recovered relay on Fedora' })).toBeVisible();
  expect(await socketCount(page)).toBe(1);
});

test('loads a deployed phone app and preserves pending relay updates', async ({ page }) => {
  const deployedAssets = APP_METADATA.assets + 1;
  const availableUpdate = {
    state: 'available',
    current_version: '0.14.0',
    current_revision: 'a'.repeat(40),
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(12),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    checked_at: 124,
    can_install: true,
    mode: 'local',
  };
  await page.route('**/version.json?*', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ version: APP_RELEASE, assets: deployedAssets }),
    });
  });
  await boot(page, [fedora]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.14.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  const loadUpdate = page.getByRole('button', { name: 'Load Update', exact: true }).first();
  await expect(loadUpdate).toBeEnabled();
  await loadUpdate.click();
  const dialog = page.getByRole('dialog', { name: 'Load Update' });
  const reloadRequest = page.waitForRequest((request) =>
    new URL(request.url()).searchParams.has('herdr_reload'));
  await dialog.getByRole('button', { name: 'Load Update', exact: true }).click();

  const reloadUrl = new URL((await reloadRequest).url());
  expect(reloadUrl.pathname).toBe('/index.html');
  expect(reloadUrl.searchParams.get('herdr_reload'))
    .toMatch(new RegExp(`^${APP_RELEASE.replaceAll('.', '\\.')}-\\d+$`));
  await expect.poll(() => {
    const current = new URL(page.url());
    return `${current.pathname}${current.searchParams.has('herdr_reload') ? '?reloading' : ''}`;
  }).toBe('/');
  const plan = await page.evaluate(() => JSON.parse(sessionStorage.getItem('herdr_update_progress') || 'null'));
  expect(plan).toMatchObject({
    targetVersion: APP_RELEASE,
    relayIds: ['fedora'],
    startedRelayIds: [],
  });

  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.14.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });
  await expect.poll(async () => (await commandsForSocket(page, 0)).some(
    (command) => command.type === 'install_update',
  )).toBe(true);
});

test('checks every self-updating relay automatically after connection', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'wss://mac.example', token: '' };
  await boot(page, [fedora, mac]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(2);
  const staleUpdate = {
    state: 'current',
    current_version: '0.0.1',
    current_revision: 'abc1234',
    upstream_version: '0.0.1',
    checked_at: 123,
    can_install: false,
    mode: 'local',
  };
  await handshake(page, 0, {
    release_version: '0.0.1',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: staleUpdate,
  });
  await handshake(page, 1, {
    release_version: '0.0.1',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: staleUpdate,
  });

  await expect.poll(async () => (await commands(page)).filter(
    (command) => command.type === 'check_update',
  ).length).toBe(2);
  for (const index of [0, 1]) {
    const check = (await commandsForSocket(page, index)).find(
      (command) => command.type === 'check_update',
    )!;
    await server(page, index, {
      type: 'command_result',
      request_id: check.request_id,
      ok: true,
      phase: 'confirmed',
      data: {
        update: {
          state: 'available',
          current_version: '0.0.1',
          current_revision: 'abc1234',
          available_version: APP_RELEASE,
          available_revision: 'f'.repeat(12),
          target_revision: 'f'.repeat(40),
          upstream_version: APP_RELEASE,
          checked_at: 124,
          can_install: true,
          mode: 'local',
        },
      },
    });
  }

  await page.getByRole('button', { name: 'Settings, relay update available' }).click();
  await expect(page.getByText(`Phone app is current at v${APP_RELEASE}.`)).toBeVisible();
  await expect(page.getByText('2 relay updates are available.')).toBeVisible();
  await expect(page.getByText(`Update v${APP_RELEASE} available`)).toHaveCount(2);
  await expect(page.getByRole('button', { name: 'Update Relays' })).toBeEnabled();
  await expect(page.getByRole('button', { name: /^Update (Fedora|Mac)/ })).toHaveCount(0);
});

test('keeps update controls steady while app and relay checks are in flight', async ({ page }) => {
  const availableUpdate = {
    state: 'available',
    current_version: '0.12.0',
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(12),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    checked_at: 124,
    can_install: true,
    mode: 'local',
  };
  await boot(page, [fedora]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { update: availableUpdate });

  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(`Phone app is current at v${APP_RELEASE}.`)).toBeVisible();
  await expect(page.getByText(`Update v${APP_RELEASE} available`)).toBeVisible();

  const checkButton = page.locator('button.update-check-button');
  const aboutCard = page.locator('section.card').filter({ has: checkButton });
  await checkButton.scrollIntoViewIfNeeded();
  const beforeButton = await checkButton.boundingBox();
  const beforeCard = await aboutCard.boundingBox();
  expect(beforeButton).not.toBeNull();
  expect(beforeCard).not.toBeNull();

  await page.route('**/version.json?*', async (route) => {
    await page.waitForTimeout(250);
    await route.continue();
  });
  await checkButton.click();
  await expect(checkButton).toBeDisabled();
  await expect(checkButton).toHaveAttribute('aria-busy', 'true');
  await expect(checkButton).toHaveText('Check for Updates');
  expect(await checkButton.evaluate((button) => getComputedStyle(button).opacity)).toBe('1');

  const duringButton = await checkButton.boundingBox();
  const duringCard = await aboutCard.boundingBox();
  expect(duringButton).not.toBeNull();
  expect(duringCard).not.toBeNull();
  // Browser engines may adjust page scroll when the focused control changes.
  expect(duringButton!.x).toBeCloseTo(beforeButton!.x, 1);
  expect(duringButton!.y - duringCard!.y).toBeCloseTo(beforeButton!.y - beforeCard!.y, 1);
  expect(duringButton!.width).toBeCloseTo(beforeButton!.width, 1);
  expect(duringButton!.height).toBeCloseTo(beforeButton!.height, 1);
  expect(duringCard!.x).toBeCloseTo(beforeCard!.x, 1);
  expect(duringCard!.width).toBeCloseTo(beforeCard!.width, 1);
  expect(duringCard!.height).toBeCloseTo(beforeCard!.height, 1);

  const check = (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'check_update')
    .at(-1)!;
  await server(page, 0, {
    type: 'command_result',
    request_id: check.request_id,
    ok: true,
    phase: 'confirmed',
    data: { update: availableUpdate },
  });
  await expect(checkButton).toBeEnabled();
  await expect(checkButton).toHaveText('Check for Updates');
});

test('confirms and tracks one relay update through its verified reconnect', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.7.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'available',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      available_revision: 'f'.repeat(12),
      target_revision: 'f'.repeat(40),
      checked_at: 123,
      can_install: true,
      mode: 'local',
    },
  });

  await page.getByRole('button', { name: 'Settings, relay update available' }).click();
  await expect(page.getByText('Update v0.8.0 available')).toBeVisible();
  await expect(page.getByText(`Phone app version ${APP_RELEASE}`)).toBeVisible();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Relays' });
  await expect(dialog).toContainText('Update Fedora first');
  await setAutoCommands(page, false);
  await dialog.getByRole('button', { name: 'Start Update' }).click();
  const progress = page.getByRole('dialog', { name: 'Updating Herdr' });

  await expect.poll(async () =>
    (await commands(page)).filter((command) => command.type === 'install_update').length).toBe(1);
  const install = (await commands(page)).find((command) => command.type === 'install_update')!;
  expect(install).toMatchObject({
    expected_version: '0.8.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
    protocol: 2,
  });
  await server(page, 0, {
    type: 'update_status',
    update: {
      state: 'scheduled',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      target_revision: 'f'.repeat(40),
      mode: 'local',
    },
  });
  await server(page, 0, {
    type: 'command_result',
    request_id: install.request_id,
    ok: true,
    phase: 'scheduled',
    data: {
      update: {
        state: 'scheduled',
        current_version: '0.7.0',
        current_revision: 'abc1234',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
        mode: 'local',
      },
    },
  });
  await expect(progress).toContainText('Update scheduled…');

  await server(page, 0, {
    type: 'update_status',
    update: {
      state: 'restarting',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      target_revision: 'f'.repeat(40),
      mode: 'local',
    },
  });
  await expect(progress).toContainText('Restarting relay…');
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1, {
    host: 'fedora',
    release_version: '0.8.0',
    revision: 'f'.repeat(12),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: '0.8.0',
      current_revision: 'f'.repeat(12),
      checked_at: 124,
      can_install: false,
      mode: 'local',
    },
  });

  const complete = page.getByRole('dialog', { name: 'Update complete' });
  await expect(complete).toContainText('Updated to v0.8.0');
  await expect(complete.getByRole('button', { name: 'Close' })).toBeVisible();
});

test('resumes fleet progress and updates the second relay automatically', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'wss://mac.example', token: '' };
  const previousVersion = '0.12.0';
  const availableUpdate = {
    state: 'available',
    current_version: previousVersion,
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(40),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    can_install: true,
    mode: 'plugin',
  };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });
  await handshake(page, 1, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  const confirmation = page.getByRole('dialog', { name: 'Update Relays' });
  await expect(confirmation).toContainText('Update Fedora first');
  await confirmation.getByRole('button', { name: 'Start Update' }).click();

  let progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText('Starting update…');
  await expect(progress).toContainText('Verify release');
  await expect(progress).toContainText('Install relay');
  await expect(progress).toContainText('Reconnect');
  await expect(progress.getByRole('button', { name: 'Finish Later' })).toHaveCount(0);
  expect((await commandsForSocket(page, 0)).find((command) => command.type === 'install_update')).toMatchObject({
    expected_version: APP_RELEASE,
    expected_revision: 'f'.repeat(40),
  });

  await server(page, 0, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'preparing' },
  });
  await expect(progress).toContainText('Verifying release…');
  await server(page, 0, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'restarting' },
  });
  await expect(progress).toContainText('Restarting relay…');
  await page.evaluate(() => {
    const relayWindow = window as unknown as { __relayClose(index: number): void };
    relayWindow.__relayClose(0);
  });
  await expect.poll(() => socketCount(page)).toBe(3);
  await handshake(page, 2, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });

  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'install_update')).toBe(true);
  progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText(`Updated to v${APP_RELEASE}`);
  await expect(progress).toContainText('Mac');
  await expect(progress.getByRole('button', { name: 'Update', exact: true })).toHaveCount(0);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect.poll(() => socketCount(page)).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });
  await handshake(page, 1, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText('updates relays one at a time');
  await server(page, 1, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'installing' },
  });
  await expect(progress).toContainText('Installing relay…');
  await handshake(page, 1, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });

  progress = page.getByRole('dialog', { name: 'Update complete' });
  await expect(progress).toContainText('2 of 2 update items complete');
  await progress.getByRole('button', { name: 'Close' }).click();
  await expect(progress).toHaveCount(0);
});

test('keeps a failed relay online and offers an explicit close action', async ({ page }) => {
  const availableUpdate = {
    state: 'available',
    current_version: '0.12.0',
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(40),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    can_install: true,
    mode: 'plugin',
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: '0.12.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  await page.getByRole('dialog', { name: 'Update Relays' }).getByRole('button', { name: 'Start Update' }).click();
  await expect(page.getByRole('dialog', { name: 'Updating Herdr' })).toBeVisible();
  await server(page, 0, {
    type: 'update_status',
    update: {
      ...availableUpdate,
      state: 'failed',
      error: 'Release signature did not match; the current relay is still running.',
    },
  });

  const progress = page.getByRole('dialog', { name: 'Update needs attention' });
  await expect(progress.getByRole('alert')).toContainText('Release signature did not match; the current relay is still running.');
  await expect(progress.getByRole('button', { name: 'Close' })).toBeVisible();
  await progress.getByRole('button', { name: 'Close' }).click();
  await expect(progress).toHaveCount(0);
  await expect(page.getByRole('img', { name: 'Fedora relay connected' })).toBeVisible();
});
test('offers the one-time Terminal bootstrap instead of retrying a legacy deploy-first failure', async ({ page }) => {
  const availableUpdate = {
    state: 'available',
    current_version: '0.13.1',
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(40),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    can_install: true,
    mode: 'plugin',
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: '0.13.1',
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    update: availableUpdate,
    app_deploy: {
      configured: false,
      state: 'idle',
    },
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  await page.getByRole('dialog', { name: 'Update Relays' }).getByRole('button', { name: 'Start Update' }).click();
  await expect(page.getByRole('dialog', { name: 'Updating Herdr' })).toBeVisible();
  await server(page, 0, {
    type: 'update_status',
    update: {
      ...availableUpdate,
      state: 'failed',
      error: 'deploy target app before relay: No HTTPS app deployment origin is configured',
    },
  });

  const progress = page.getByRole('dialog', { name: 'Update needs attention' });
  await expect(progress).toContainText('Manual update required');
  await expect(progress).toContainText('HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install');
  await expect(progress.getByRole('button', { name: 'Copy Update Command' })).toBeVisible();
  await expect(progress.getByRole('button', { name: 'Try Again' })).toHaveCount(0);
});


test('does not poll the app origin after its deployment target is loaded', async ({ page }) => {
  const versionRequests: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/version.json') versionRequests.push(request.url());
  });
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: APP_RELEASE,
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'f'.repeat(40),
      state: 'succeeded',
      target_version: APP_RELEASE,
    },
  });
  await page.waitForTimeout(300);
  const settledRequestCount = versionRequests.length;

  await page.waitForTimeout(2_200);

  expect(versionRequests).toHaveLength(settledRequestCount);
});

test('confirms deployment when an authorized relay has the upstream app bundle', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '9.0.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    update: {
      state: 'current',
      current_version: '9.0.0',
      upstream_version: '9.0.0',
    },
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'f'.repeat(40),
      state: 'idle',
    },
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(`Version 9.0.0 is released, but this app origin still serves ${APP_RELEASE}.`)).toBeVisible();
  await page.getByRole('button', { name: 'Update Herdr' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Herdr' });
  await expect(dialog).toContainText('Publish the phone app from Fedora');
  await dialog.getByRole('button', { name: 'Start Update' }).click();

  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'deploy_app_update')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'deploy_app_update')).toMatchObject({
    expected_version: '9.0.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
  });
  const publishing = 'Publishing v9.0.0 from Fedora and waiting for this app origin to update. This can take up to two minutes.';
  for (const state of ['scheduled', 'deploying']) {
    await server(page, 0, {
      type: 'app_deploy_status',
      app_deploy: {
        configured: true,
        origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'f'.repeat(40),
        state,
        target_version: '9.0.0',
      },
    });
    await expect(page.getByText(publishing)).toBeVisible();
  }
});

test('deploys a Pages app before updating its owner relay', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '8.0.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    update: {
      state: 'available',
      current_version: '8.0.0',
      current_revision: 'abc1234',
      available_version: '9.0.0',
      available_revision: 'f'.repeat(12),
      target_revision: 'f'.repeat(40),
      upstream_version: '9.0.0',
      can_install: true,
      mode: 'plugin',
    },
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'abc1234',
      state: 'idle',
    },
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Herdr' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Herdr' });
  await expect(dialog).toContainText('Publish the phone app first, then update Fedora');
  await dialog.getByRole('button', { name: 'Start Update' }).click();

  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'install_update')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'install_update')).toMatchObject({
    expected_version: '9.0.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
  });
  expect((await commands(page)).some((command) => command.type === 'deploy_app_update')).toBe(false);
});

test('reports when Herdr clips older terminal history', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Clipped app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Clipped app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'visible recent output',
    truncated: true,
  });
  const clippedNotice = page.getByRole('status').filter({ hasText: 'Older terminal history is not shown' });
  await expect(clippedNotice).toBeVisible();

  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'complete output',
    truncated: false,
  });
  await expect(clippedNotice).toBeHidden();
});

test('applies relay-watched terminal deltas and pauses the watcher when hidden', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_realtime_delta', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Realtime app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Realtime app on Fedora' }).click();
  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'read_pane')).toBe(true);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'initial terminal output\nstable row\nthird row\n',
    content_fingerprint: 'content-1',
  });
  await expect(page.getByRole('log')).toContainText('initial terminal output');

  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'watch_pane')).toMatchObject({
    type: 'watch_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-1',
  });
  await server(page, 0, {
    type: 'pane_delta',
    pane_id: 'w1:p1',
    format: 'ansi',
    base_fingerprint: 'content-1',
    content_fingerprint: 'content-2',
    segments: [
      { copy_lines: 3 },
      { text: 'fresh terminal output\n' },
    ],
  });
  await expect(page.getByRole('log')).toContainText('fresh terminal output');
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'pane_applied')).toMatchObject({
    type: 'pane_applied',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });

  const activeCommandCount = (await commands(page)).length;
  await page.waitForTimeout(750);
  expect(await commands(page)).toHaveLength(activeCommandCount);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'unwatch_pane')).toMatchObject({
    type: 'unwatch_pane',
    pane_id: 'w1:p1',
  });
  const hiddenCommandCount = (await commands(page)).length;
  await page.waitForTimeout(750);
  expect(await commands(page)).toHaveLength(hiddenCommandCount);
});

test('resets drafts and terminal output when moving to another agent', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'working', project: 'Working A', agent: 'codex' },
      { pane_id: 'w1:p2', status: 'blocked', attention_kind: 'approval', project: 'Blocked B', agent: 'claude', options: ['Approve once', 'Deny'] },
    ],
  });
  await page.getByRole('button', { name: 'Open Working A on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'private output from agent A' });
  await expect(page.getByRole('log')).toContainText('private output from agent A');
  await page.getByRole('combobox', { name: 'Prompt' }).fill('draft intended only for A');

  await page.getByRole('button', { name: 'Next blocked' }).click();

  await expect(page.getByRole('main', { name: 'Terminal for Blocked B' })).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('');
  await expect(page.getByRole('log')).not.toContainText('private output from agent A');
});

test('replaces a half-open socket immediately when a sleeping phone resumes', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Resume app on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'cached terminal output' });
  await expect(page.getByRole('log')).toContainText('cached terminal output');

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.waitForTimeout(5_100);
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Agent unavailable' })).toBeHidden();
  await expect(page.getByRole('log')).toContainText('cached terminal output');
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1);
  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'read_pane')).toBe(true);
  await server(page, 1, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'fresh output after resume',
  });
  await expect(page.getByRole('log')).toContainText('fresh output after resume');
  await server(page, 1, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
});

test('keeps Claude desktop prompt and status in the shared terminal output', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Wrapped status', agent: 'claude' }],
  });
  await page.getByRole('button', { name: 'Open Wrapped status on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      ...Array.from({ length: 8 }, (_, index) => `Conversation output ${index + 1}`),
      '❯ Try "edit Info.plist to..."',
      '─'.repeat(100),
      'Opus 4.8',
      'ctx: -',
      'main ~16',
      '/rc ⏸ manual mode on · ← for agents',
    ].join('\n'),
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('Conversation output 8');
  await expect(terminal).toContainText('edit Info.plist');
  await expect(terminal).toContainText('Opus 4.8');
  await expect(terminal).toContainText('ctx: -');
  await expect(terminal).toContainText('manual mode');
});

test('uses Resize Session as the only terminal layout', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resize-only app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resize-only app on Fedora' }).click();

  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_layout'))).toBeNull();
});

test('reports unavailable Resize Session without exposing legacy width modes', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Older relay app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Older relay app on Fedora' }).click();
  const border = `╭${'─'.repeat(98)}╮`;
  const rightBorder = (content: string) => `${content.padEnd(99)}│`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      border,
      `\u001b[48;2;15;18;22m${rightBorder(' Welcome back!')}\u001b[0m`,
      rightBorder('  prewalk    Switch model'),
      rightBorder('  dump       Copy session'),
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const lines = terminal.locator('.ansi-line');
  await expect(lines).toHaveCount(4);
  expect(await lines.first().textContent()).toBe(border);
  await expect(lines.nth(1)).toHaveClass(/ansi-line-background/);
  const lineWidths = await lines.evaluateAll((elements) => (
    elements.map((element) => Math.round(element.getBoundingClientRect().width))
  ));
  expect(new Set(lineWidths).size).toBe(1);
  const rightEdges = await lines.evaluateAll((elements) => elements.map((element) => {
    const cells = element.querySelectorAll<HTMLElement>('.terminal-cell');
    return cells[cells.length - 1]?.getBoundingClientRect().right || 0;
  }));
  expect(Math.max(...rightEdges) - Math.min(...rightEdges)).toBeLessThan(0.1);
  expect(await terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await expect(page.getByRole('alert')).toHaveText('Resize Session is unavailable on this relay.');
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  expect((await commands(page)).filter((command) =>
    command.type === 'lease_pane_size' || command.type === 'release_pane_size')).toHaveLength(0);
});

test('virtualizes terminal history and copies the latest response', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Long history', agent: 'opencode' }],
  });
  await page.getByRole('button', { name: 'Open Long history on Fedora' }).click();
  const content = Array.from({ length: 1_000 }, (_, index) => {
    const row = `row ${String(index + 1).padStart(4, '0')}`;
    return index % 50 === 0 ? `${row} ${'wrapped content '.repeat(32).trimEnd()}` : row;
  }).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content,
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  const mountedRows = screen.locator('[data-terminal-row]');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1000');
  await expect(terminal).toContainText('row 1000');
  expect(await mountedRows.count()).toBeLessThan(250);

  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row'))).toBe(0);
  const topHeights = await mountedRows.evaluateAll((elements) => elements.slice(0, 2)
    .map((element) => element.getBoundingClientRect().height));
  expect(Math.abs(topHeights[0] - topHeights[1])).toBeLessThan(0.1);

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row')))
    .toBeGreaterThan(100);
  expect(await mountedRows.count()).toBeLessThan(250);
  const middleAnchor = await terminal.evaluate((element) => {
    const viewportTop = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > viewportTop);
    if (!row) return null;
    return {
      index: Number(row.dataset.terminalRow),
      offset: row.getBoundingClientRect().top - viewportTop,
    };
  });
  expect(middleAnchor).not.toBeNull();
  const updatedContent = `${content}\nrow 1001 updated while scrolled`;
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: updatedContent,
  });
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1001');
  expect(await mountedRows.count()).toBeLessThan(250);
  await expect.poll(async () => terminal.evaluate((element, expected) => {
    const viewportTop = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > viewportTop);
    return row ? Math.abs(Number(row.dataset.terminalRow) - expected) : Number.POSITIVE_INFINITY;
  }, middleAnchor?.index || 0)).toBeLessThanOrEqual(1);
  await expect.poll(async () => terminal.evaluate((element, expected) => {
    const viewport = element.getBoundingClientRect();
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => Number(candidate.dataset.terminalRow) === expected);
    if (!row) return false;
    const bounds = row.getBoundingClientRect();
    return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
  }, middleAnchor?.index || 0)).toBe(true);

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(1000);
  const bottomUpdatedContent = `${updatedContent}\nrow 1002 appended at bottom`;
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: bottomUpdatedContent,
  });
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1002');
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(1001);
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  const viewport = page.viewportSize();
  expect(viewport).not.toBeNull();
  await page.setViewportSize({ width: viewport!.width, height: viewport!.height - 180 });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await terminal.evaluate((element) => {
    const view = element.closest<HTMLElement>('.terminal-view');
    if (!view) throw new Error('Terminal view is missing');
    view.style.height = `${view.clientHeight - 120}px`;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1002');
  expect(await mountedRows.count()).toBeLessThan(250);
  const finalResponseContent = [
    bottomUpdatedContent,
    '',
    '     + Thought: 1.0s',
    '     Final response line one.',
    '     Second line.',
    '     ▣ Build · model · 2m 19s',
    '     Next question',
  ].join('\n');
  const finalResponse = 'Final response line one.\nSecond line.';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: finalResponseContent,
  });
  await expect(screen).toContainText('Final response line one.');
  const fullTranscript = page.getByRole('textbox', { name: 'Full terminal transcript' });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  const copyButton = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(fullTranscript).toHaveValue(/Final response line one\./);
  await expect(responseTranscript).toHaveValue(finalResponse);
  await copyButton.click();
  await expect.poll(() => page.evaluate(() =>
    Reflect.get(window, '__copiedTerminal'))).toBe(finalResponse);
  await expect(page.getByRole('status').filter({ hasText: 'Final response copied.' })).toBeVisible();
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  });
  await copyButton.click();
  await expect(responseTranscript).toBeFocused();
  expect(await responseTranscript.evaluate((element) => {
    if (!(element instanceof HTMLTextAreaElement)) return null;
    return { start: element.selectionStart, end: element.selectionEnd };
  })).toEqual({ start: 0, end: finalResponse.length });
  await expect(page.getByRole('status').filter({ hasText: 'Final response selected.' })).toBeVisible();
});

test('copies visible terminal output when no completed response is available', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Shell output', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Shell output on Fedora' }).click();
  const visibleOutput = 'shell output\nsecond line';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: visibleOutput,
  });
  const fullTranscript = page.getByRole('textbox', { name: 'Full terminal transcript' });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  await expect(fullTranscript).toHaveValue(visibleOutput);
  await expect(responseTranscript).toHaveValue('');
  await page.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect.poll(() => page.evaluate(() =>
    Reflect.get(window, '__copiedTerminal'))).toBe(visibleOutput);
  await expect(page.getByRole('status').filter({ hasText: 'Copied the visible terminal output.' })).toBeVisible();
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  });
  await page.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(fullTranscript).toBeFocused();
  expect(await fullTranscript.evaluate((element) => {
    if (!(element instanceof HTMLTextAreaElement)) return null;
    return { start: element.selectionStart, end: element.selectionEnd };
  })).toEqual({ start: 0, end: visibleOutput.length });
  await expect(page.getByRole('status').filter({ hasText: 'Output selected.' })).toBeVisible();
});

test('uses relay response copy before parser and surfaces failures', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Remote copy', agent: 'claude' }],
  });
  await page.getByRole('button', { name: 'Open Remote copy on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: '     + Thought: 0.1s\n     Parsed terminal response.\n     ▣ Build · model · 1s',
  });
  const copyButton = page.getByRole('button', { name: 'Copy', exact: true });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  await expect(responseTranscript).toHaveValue('Parsed terminal response.');
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'list_slash_commands')).toHaveLength(1);
  await setAutoCommands(page, false);

  await copyButton.click();
  await expect(page.getByRole('button', { name: 'Copying…', exact: true })).toBeDisabled();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const failedCopyCommand = (await commandsForSocket(page, 0))
    .find((command) => command.type === 'copy_agent_response') as Record<string, unknown>;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: failedCopyCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Agent did not confirm a copied response',
  });
  await expect(copyButton).toHaveText('Copy');
  await expect(page.getByRole('status').filter({ hasText: 'Agent did not confirm a copied response' })).toBeVisible();
  expect(await page.evaluate(() => Reflect.get(window, '__copiedTerminal'))).toBeUndefined();

  await setAutoCommands(page, true);
  await copyButton.click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedTerminal')))
    .toBe('# Remote markdown response\n\n- Exact copied output');
  await expect(responseTranscript).toHaveValue('Parsed terminal response.');
  await expect(page.getByRole('region', { name: 'Copied agent response' })).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Copied agent response markdown' }))
    .toHaveValue('# Remote markdown response\n\n- Exact copied output');
  await expect(page.getByRole('status').filter({ hasText: 'Agent response copied.' })).toBeVisible();
});

test('virtualizes large ANSI grids when Resize Session is unavailable', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'ANSI grid history', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open ANSI grid history on Fedora' }).click();
  const content = Array.from({ length: 600 }, (_, index) => {
    if (index % 20 === 0) return `╭${'─'.repeat(98)}╮`;
    if (index % 20 === 1) {
      return `\u001b[48;2;61;64;64m${`│ grid row ${index + 1}`.padEnd(99)}│\u001b[0m`;
    }
    return `\u001b[38;2;95;175;255mANSI agent row ${index + 1} 🐑\u001b[0m`;
  }).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content,
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  const mountedRows = screen.locator('[data-terminal-row]');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '600');
  await expect.poll(async () => Number(await mountedRows.last().getAttribute('data-terminal-row'))).toBe(599);
  expect(await mountedRows.count()).toBeLessThan(250);
  expect(await terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);

  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row'))).toBe(0);
  const gridGeometry = await terminal.locator('.terminal-grid-line').first().evaluate((element) => ({
    height: element.getBoundingClientRect().height,
    lineHeight: Number.parseFloat(getComputedStyle(element).lineHeight),
  }));
  expect(gridGeometry.height).toBeLessThan(gridGeometry.lineHeight * 2);
  await expect(terminal.locator('.ansi-line-background').first()).toBeVisible();

  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => Number(await mountedRows.first().getAttribute('data-terminal-row')))
    .toBeGreaterThan(100);
  expect(await mountedRows.count()).toBeLessThan(250);
});



test('restores a non-bottom anchor after a Resize Session width change', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resize anchor', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resize anchor on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  await page.waitForTimeout(300);

  const content = Array.from(
    { length: 600 },
    (_, index) => `resize anchor row ${String(index + 1).padStart(4, '0')}`,
  ).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content,
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '600');
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => terminal.evaluate((element) => {
    const viewport = element.getBoundingClientRect();
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => {
        const bounds = candidate.getBoundingClientRect();
        return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
      });
    return row ? Number(row.dataset.terminalRow) : -1;
  })).toBeGreaterThan(100);
  const anchor = await terminal.evaluate((element) => {
    const viewport = element.getBoundingClientRect();
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => {
        const bounds = candidate.getBoundingClientRect();
        return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
      });
    return {
      index: Number(row?.dataset.terminalRow),
      text: row?.textContent || '',
    };
  });
  expect(anchor.text).toMatch(/^resize anchor row \d{4}$/);

  await setAutoCommands(page, false);
  const viewport = page.viewportSize()!;
  await page.setViewportSize({ width: viewport.width + 160, height: viewport.height });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(2);
  await expect(terminal).toContainText('Resizing terminal…');
  const lease = (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').at(-1)!;
  const readsBeforeResult = (await commands(page))
    .filter((command) => command.type === 'read_pane').length;
  await server(page, 0, {
    type: 'command_result',
    action: 'lease_pane_size',
    request_id: lease.request_id,
    ok: true,
    data: { columns: lease.columns },
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(readsBeforeResult);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content,
  });

  await expect(screen).toHaveAttribute('data-terminal-row-count', '600');
  await expect.poll(async () => terminal.evaluate((element, expected) => {
    const viewport = element.getBoundingClientRect();
    const row = [...element.querySelectorAll<HTMLElement>('[data-terminal-row]')]
      .find((candidate) => {
        const bounds = candidate.getBoundingClientRect();
        return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
      });
    if (!row) return Number.POSITIVE_INFINITY;
    return Math.abs(Number(row.dataset.terminalRow) - expected);
  }, anchor.index)).toBeLessThanOrEqual(1);
  await expect(terminal).toContainText(anchor.text);
});

test('leases measured terminal columns and releases on teardown', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resizable app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resizable app on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  const acquire = (await commands(page))
    .find((command) => command.type === 'lease_pane_size')!;
  expect(acquire).toMatchObject({
    type: 'lease_pane_size',
    pane_id: 'w1:p1',
    protocol: 2,
  });
  expect(acquire.request_id).toEqual(expect.any(String));
  expect(acquire).not.toHaveProperty('client_id');
  expect(acquire.columns).toEqual(expect.any(Number));
  expect(Number(acquire.columns)).toBeGreaterThanOrEqual(40);
  expect(Number(acquire.columns)).toBeLessThanOrEqual(240);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(0);
  await page.waitForTimeout(300);

  const terminal = page.getByRole('log');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: [
      `\u001b[38;2;80;200;160mhttps://example.test/${'unbroken-token'.repeat(12)}\u001b[0m`,
      `\u001b[48;2;61;64;64m${'› Summarize recent commits'.padEnd(151)}\u001b[0m`,
      '  gpt-5.6-sol medium'.padEnd(151),
    ].join('\n'),
  });
  await expect(terminal).toHaveClass(/preserve-layout/);
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect(terminal.locator('.ansi-line')).toHaveCount(3);
  await expect(terminal.locator('.ansi-line-background')).toHaveText('› Summarize recent commits');
  const resizeGeometry = await terminal.evaluate((element) => {
    const screen = element.querySelector<HTMLElement>('.term-screen');
    const renderedLines = [...element.querySelectorAll<HTMLElement>('.ansi-line')];
    const background = element.querySelector<HTMLElement>('.ansi-line-background');
    const firstLine = renderedLines[0];
    const firstLineStyle = getComputedStyle(firstLine);
    return {
      backgroundWidth: background?.getBoundingClientRect().width || 0,
      clientWidth: element.clientWidth,
      firstLineHeight: firstLine.getBoundingClientRect().height,
      lineHeight: Number.parseFloat(firstLineStyle.lineHeight),
      scrollWidth: element.scrollWidth,
      screenWidth: screen?.getBoundingClientRect().width || 0,
      lineLengths: renderedLines.map((line) => line.textContent?.length || 0),
    };
  });
  expect(resizeGeometry.scrollWidth).toBeLessThanOrEqual(resizeGeometry.clientWidth);
  expect(resizeGeometry.screenWidth).toBeLessThanOrEqual(resizeGeometry.clientWidth);
  expect(resizeGeometry.backgroundWidth).toBeLessThan(resizeGeometry.clientWidth);
  expect(resizeGeometry.firstLineHeight).toBeGreaterThan(resizeGeometry.lineHeight * 2);
  expect(Math.max(...resizeGeometry.lineLengths)).toBeGreaterThan(Number(acquire.columns));

  const storedHistory = Array.from(
    { length: 120 },
    (_, index) => `stored resize history row ${index + 1}`,
  ).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: storedHistory,
  });
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');
  await expect(terminal).toContainText('stored resize history row 120');

  const viewport = page.viewportSize()!;
  await page.setViewportSize({ width: viewport.width + 200, height: viewport.height });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(2);
  const leases = (await commands(page)).filter((command) => command.type === 'lease_pane_size');
  expect(leases[1].columns).not.toBe(leases[0].columns);
  await page.waitForTimeout(300);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: storedHistory,
  });
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');

  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'release_pane_size').length).toBe(1);
  const release = (await commands(page)).find((command) => command.type === 'release_pane_size')!;
  expect(release).toMatchObject({
    type: 'release_pane_size',
    pane_id: 'w1:p1',
    protocol: 2,
  });
  expect(release.request_id).toEqual(expect.any(String));
  expect(release).not.toHaveProperty('client_id');

  const leaseCountBeforeReentry = (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length;
  const readCountBeforeReentry = (await commands(page))
    .filter((command) => command.type === 'read_pane').length;
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Resizable app on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(leaseCountBeforeReentry + 1);
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');
  await expect(terminal).toContainText('stored resize history row 120');
  await expect(terminal).not.toContainText('Resizing terminal…');

  const reentryLease = (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').at(-1)!;
  await server(page, 0, {
    type: 'command_result',
    action: 'lease_pane_size',
    request_id: reentryLease.request_id,
    ok: true,
    data: { columns: reentryLease.columns },
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(readCountBeforeReentry);
  await page.waitForTimeout(300);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: Array.from(
      { length: 46 },
      (_, index) => `transient viewport row ${index + 1}`,
    ).join('\n'),
  });
  await expect(terminal).toContainText('stored resize history row 120');
  await expect(terminal).not.toContainText('transient viewport row');
  await page.waitForTimeout(300);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: storedHistory,
  });
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');

  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'release_pane_size').length).toBe(2);
});

test('renders the selected full history while Resize Session is active', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resize history', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resize history on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(0);

  const historyRows = Array.from(
    { length: 1_000 },
    (_, index) => `complete resize history row ${String(index + 1).padStart(4, '0')}`,
  );
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: historyRows.join('\n'),
  });

  const terminal = page.getByRole('log');
  const screen = terminal.locator('.term-screen');
  await expect(screen).toHaveAttribute('data-terminal-row-count', '1000');
  await expect(terminal).toContainText('complete resize history row 1000');
  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(terminal).toContainText('complete resize history row 0001');
  expect((await commands(page)).find((command) => command.type === 'read_pane'))
    .toMatchObject({ lines: 1_000 });
});

test('wraps stale wide grids but preserves current grids in Resize Session', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Qoder grid', agent: 'qodercli' }],
  });
  await page.getByRole('button', { name: 'Open Qoder grid on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(0);
  const lease = (await commands(page))
    .find((command) => command.type === 'lease_pane_size')!;
  const columns = Number(lease.columns);

  const staleTable = [
    `┌${'─'.repeat(columns + 78)}┐`,
    `│ ${'Lookback | Sharpe | Max DD | 2x-cost Sharpe'.padEnd(columns + 76)}│`,
    `└${'─'.repeat(columns + 78)}┘`,
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: staleTable,
  });

  const terminal = page.getByRole('log');
  await expect(terminal.locator('.terminal-grid-line')).toHaveCount(0);
  await expect(terminal).toContainText('Lookback | Sharpe | Max DD | 2x-cost Sharpe');
  const wrappedGeometry = await terminal.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
  }));
  expect(wrappedGeometry.scrollWidth).toBeLessThanOrEqual(wrappedGeometry.clientWidth);

  const currentTable = [
    `┌${'─'.repeat(columns - 2)}┐`,
    `│ ${'Current grid'.padEnd(columns - 4)}│`,
    `└${'─'.repeat(columns - 2)}┘`,
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: currentTable,
  });
  await expect(terminal.locator('.terminal-grid-line')).toHaveCount(3);
  const lineHeights = await terminal.locator('.terminal-grid-line').evaluateAll((lines) => {
    const lineHeight = Number.parseFloat(getComputedStyle(lines[0]).lineHeight);
    return {
      lineHeight,
      heights: lines.map((line) => line.getBoundingClientRect().height),
    };
  });
  expect(Math.max(...lineHeights.heights)).toBeLessThan(lineHeights.lineHeight * 1.2);
});

test('surfaces one explicit error when a pane-size lease fails', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Lease error app', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Lease error app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'native terminal output',
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  const acquire = (await commands(page))
    .find((command) => command.type === 'lease_pane_size')!;
  await server(page, 0, {
    type: 'command_result',
    action: 'lease_pane_size',
    request_id: acquire.request_id,
    ok: false,
    error: 'pane resize denied',
  });
  await expect(page.getByRole('alert')).toHaveText('Resize Session failed: pane resize denied');

  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'release_pane_size').length).toBe(1);
});

test('does not repopulate prompts after an unsafe dispatch failure', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Unsafe retry app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Unsafe retry app on Fedora' }).click();
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await prompt.fill('run this only once');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((entry) => entry.type === 'submit_prompt')).toBe(true);
  const command = (await commands(page)).find((entry) => entry.type === 'submit_prompt')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: (command as Record<string, unknown>).request_id,
    action: 'submit_prompt',
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed; review the agent before retrying',
    data: { dispatched_unknown: true },
  });
  await expect(prompt).toHaveValue('');
});

test('preserves unsafe prompt state for older relays without error data', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Legacy retry app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Legacy retry app on Fedora' }).click();
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await prompt.fill('run this only once');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((entry) => entry.type === 'submit_prompt')).toBe(true);
  const command = (await commands(page)).find((entry) => entry.type === 'submit_prompt')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: (command as Record<string, unknown>).request_id,
    action: 'submit_prompt',
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed; review the agent before retrying',
  });
  await expect(prompt).toHaveValue('');
});

test('opens native conversation history and pages older turns', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedConversation', text);
        },
      },
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'conversation_history',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'History app',
      agent: 'omp',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });

  await page.getByRole('button', { name: 'Open History app on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('middle retained answer')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeVisible();
  await expect(page.getByText('4 recorded messages')).toBeVisible();
  await expect(page.getByText(/session log is larger than 16 MB/)).toBeVisible();
  await page.getByRole('button', { name: 'Copy History app message as Markdown' }).click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedConversation')))
    .toBe('# middle retained answer');
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeHidden();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('intermediate progress update')).toBeVisible();
  await expect(page.getByText('Read', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('intermediate progress update')).toBeHidden();

  await page.getByRole('button', { name: 'Load older turns' }).click();
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'get_conversation_history' && command.before === 'turn-2'
  ))).toMatchObject({ pane_id: 'w1:p1', before: 'turn-2' });

  const search = page.getByRole('searchbox', { name: 'Search displayed conversation' });
  await search.fill('first retained');
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeHidden();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
});

test('inspects workspace files and Git changes without write controls', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'workspace_inspection',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      workspace_id: 'workspace-mobile',
      status: 'working',
      project: 'Workspace app',
      agent: 'codex',
      cwd: '/work/mobile',
    }],
  });

  await page.getByRole('button', { name: 'Open Workspace app on Fedora' }).click();
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const dialog = page.getByRole('dialog', { name: 'Workspace' });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText('/work/mobile', { exact: true })).toBeVisible();
  await expect(dialog.getByText('feature/mobile', { exact: true })).toBeVisible();
  const sectionTabsBox = await dialog.getByRole('tablist', { name: 'Workspace sections' }).boundingBox();
  const branchBox = await dialog.getByText('feature/mobile', { exact: true }).boundingBox();
  expect(sectionTabsBox).not.toBeNull();
  expect(branchBox).not.toBeNull();
  expect(Math.abs(
    sectionTabsBox!.y + sectionTabsBox!.height / 2
    - (branchBox!.y + branchBox!.height / 2),
  )).toBeLessThan(2);


  await dialog.locator('aside').getByRole('button', { name: 'README.md', exact: true }).click();
  await expect(dialog.getByRole('textbox', { name: 'Contents of README.md' }))
    .toHaveValue('# Workspace preview\n\nRead-only file contents.');

  await dialog.getByRole('tab', { name: /Changes/ }).click();
  await dialog.locator('aside').getByRole('button', { name: /README\.md/ }).click();
  const diff = dialog.getByLabel('Diff for README.md');
  await expect(diff).toContainText('diff --git a/README.md b/README.md');
  await expect(diff.locator('.diff-hunk')).toHaveText('@@ -1 +1 @@');
  await expect(diff.locator('.diff-deletion')).toHaveText('-Old text');
  await expect(diff.locator('.diff-addition')).toHaveText('+Read-only change');
  const initialDiffFontSize = await diff.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  await diff.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 11, isPrimary: true, clientX: 80, clientY: 180,
  });
  await diff.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 180, clientY: 180,
  });
  await diff.dispatchEvent('pointermove', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 240, clientY: 180,
  });
  await diff.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 240, clientY: 180,
  });
  await diff.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 11, isPrimary: true, clientX: 80, clientY: 180,
  });
  await expect.poll(() => diff.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)))
    .toBeGreaterThan(initialDiffFontSize * 1.4);
  const changedFiles = dialog.locator('aside');
  await changedFiles.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 7, isPrimary: true, clientX: 150, clientY: 220,
  });
  await changedFiles.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 7, isPrimary: true, clientX: 40, clientY: 224,
  });
  await expect(changedFiles).toHaveCount(0);
  const showChanges = dialog.getByRole('button', { name: 'Show changed-file list' });
  await expect(showChanges).toBeVisible();
  await showChanges.click();
  await expect(dialog.locator('aside')).toBeVisible();

  await expect(dialog.getByRole('button', { name: /save|write|apply|commit/i })).toHaveCount(0);

  await expect.poll(async () => (await commands(page))
    .map((command) => String(command.type))
    .filter((type) => type.startsWith('workspace_'))
    .sort()).toEqual([
    'workspace_file',
    'workspace_git_diff',
    'workspace_git_status',
    'workspace_tree',
  ]);
});

test('uses a workspace agent rail in a tablet terminal', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1',
        workspace_id: 'workspace-mobile',
        status: 'working',
        project: 'Mobile workspace',
        agent: 'codex',
        cwd: '/work/mobile',
      },
      {
        pane_id: 'w2:p1',
        workspace_id: 'workspace-api',
        status: 'idle',
        project: 'API workspace',
        agent: 'claude',
        cwd: '/work/api',
      },
    ],
  });

  await page.getByRole('button', { name: 'Search all agents' }).click();
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Mobile workspace');
  await jump.getByRole('button', { name: /Mobile workspace/ }).click();

  const rail = page.getByRole('complementary', { name: 'Agent navigation' });
  const terminal = page.getByRole('main', { name: 'Terminal for Mobile workspace' });
  await expect(rail).toBeVisible();
  await expect(terminal).toBeVisible();
  const [railBox, terminalBox] = await Promise.all([rail.boundingBox(), terminal.boundingBox()]);
  expect(railBox).not.toBeNull();
  expect(terminalBox).not.toBeNull();
  expect(railBox!.x + railBox!.width).toBeLessThanOrEqual(terminalBox!.x + 1);
  expect(terminalBox!.width).toBeGreaterThan(600);

  await rail.getByRole('button', { name: /API workspace/ }).click();
  await expect(page.getByRole('main', { name: 'Terminal for API workspace' })).toBeVisible();
});

test('keeps the Pi desktop UI separate from the generic mobile composer', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Pi app', agent: 'pi' }],
  });
  await page.getByRole('button', { name: 'Open Pi app on Fedora' }).click();
  const rule = '─'.repeat(120);
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      '\u001b[38;2;166;227;161mConversation output\u001b[0m', '', rule, '@\u001b[7m \u001b[0m', rule,
      '  .gitattributes                   .gitattributes',
      '→ AGENTS.md                        AGENTS.md',
      '  frontend/                        frontend', '  (4/20)',
      '\u001b[2m~/Development/herdr-mobile-relay (main)\u001b[0m',
      `\u001b[2m$0.000 (sub) 0.0%/272k (auto)${' '.repeat(80)}gpt-5.6-sol • xhigh\u001b[0m`,
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await expect(terminal).not.toHaveClass(/bottom-ui-terminal/);
  await expect(prompt).toHaveValue('');
  await expect(terminal).toContainText('Conversation output');
  await expect(terminal).toContainText('@');
  await expect(terminal).toContainText('.gitattributes');
  await expect(terminal).toContainText('AGENTS.md');
  await expect(terminal).toContainText('~/Development/herdr-mobile-relay');
  await expect(terminal).toContainText('gpt-5.6-sol');
  await expect(terminal.locator('.agent-current-ui-start')).toHaveCount(0);

  await prompt.fill('@AGENTS.md');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({
    pane_id: 'w1:p1', text: '@AGENTS.md',
  });
  expect((await commands(page)).filter((command) => (
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(['ctrl+c'])
  ))).toHaveLength(0);
});

test('keeps the Codex picker in the shared terminal with generic controls', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Codex placeholder', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Codex placeholder on Fedora' }).click();
  const background = '\u001b[48;2;61;64;64m                    \u001b[0m';
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      'Completed output', background,
      '\u001b[1;48;2;61;64;64m›\u001b[0m\u001b[48;2;61;64;64m @\u001b[0m',
      background,
      '> Default templates          Default templates for documents and presentations       Plugin',
      '  Analytics Dashboard        Create spreadsheets with the dashboard template         Skill',
      '  enter insert · esc close · ←/→ switch search modes                      [All Results]   Filesystem Only    Plugins',
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await expect(terminal).not.toHaveClass(/bottom-ui-terminal/);
  await expect(terminal).toContainText('Completed output');
  await expect(terminal).toContainText('Default templates');
  await expect(terminal).toContainText('Analytics Dashboard');
  await expect(terminal).toContainText('All Results');
  await expect(terminal.locator('.codex-picker-item')).toHaveCount(0);
  await expect(prompt).toHaveValue('');
  await expect(page.getByRole('button', { name: 'Previous result' })).toHaveCount(0);

  await page.getByRole('button', { name: 'Arrow keys' }).click();
  await page.getByRole('button', { name: 'Right', exact: true }).click();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(['Right'])
  ))).toMatchObject({ pane_id: 'w1:p1', keys: ['Right'] });
});

test('finds and highlights matches across virtualized terminal output', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Search app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Search app on Fedora' }).click();
  const lines = Array.from({ length: 320 }, (_, index) => {
    if (index === 24) return 'needle early result';
    if (index === 286) return 'needle late result';
    return `terminal row ${index}`;
  });
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: lines.join('\n'),
  });

  await page.getByRole('button', { name: 'Find in terminal', exact: true }).click();
  const search = page.getByRole('searchbox', { name: 'Find in terminal output' });
  await search.fill('needle');
  await expect(page.getByText('1 of 2', { exact: true })).toBeVisible();
  await expect(page.locator('mark.terminal-find-match.active')).toHaveText('needle');
  await expect(page.getByRole('log')).toContainText('needle early result');

  await page.getByRole('button', { name: 'Next match' }).click();
  await expect(page.getByText('2 of 2', { exact: true })).toBeVisible();
  await expect(page.locator('mark.terminal-find-match.active')).toHaveText('needle');
  await expect(page.getByRole('log')).toContainText('needle late result');

  await search.press('Escape');
  await expect(search).toBeHidden();
  await expect(page.locator('mark.terminal-find-match')).toHaveCount(0);
});

test('discovers slash commands per terminal and fills them before sending', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'working', project: 'Codex app', agent: 'codex', cwd: '/home/test/codex' },
      { pane_id: 'w1:p2', status: 'idle', project: 'Claude app', agent: 'claude', cwd: '/home/test/claude' },
    ],
  });

  await page.getByRole('button', { name: 'Search all agents' }).click();
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Codex app');
  await jump.getByRole('button', { name: /Codex app/ }).click();
  const codexComposer = page.getByRole('combobox', { name: 'Prompt' });
  const restingComposerBox = await codexComposer.boundingBox();
  await codexComposer.fill('/');
  const popover = page.getByRole('region', { name: 'Command suggestions' });
  await expect(popover).toBeVisible();
  const [popoverBox, composerBox, viewport] = await Promise.all([
    popover.boundingBox(),
    codexComposer.boundingBox(),
    page.evaluate(() => ({ width: innerWidth, height: innerHeight })),
  ]);
  expect(restingComposerBox).not.toBeNull();
  expect(popoverBox).not.toBeNull();
  expect(composerBox).not.toBeNull();
  expect(composerBox!.y).toBeCloseTo(restingComposerBox!.y, 0);
  expect(composerBox!.height).toBeCloseTo(restingComposerBox!.height, 0);
  expect(popoverBox!.y + popoverBox!.height).toBeLessThan(composerBox!.y);
  expect(popoverBox!.height).toBeLessThanOrEqual(viewport.height * 0.5);
  await expect(popover.getByRole('option')).toHaveCount(22);
  const description = popover.getByText('Show the full command reference and explain every available action');
  await expect(description).toBeVisible();
  expect(await description.evaluate((element) => ({
    overflow: getComputedStyle(element).overflow,
    textOverflow: getComputedStyle(element).textOverflow,
    whiteSpace: getComputedStyle(element).whiteSpace,
  }))).toEqual({ overflow: 'visible', textOverflow: 'clip', whiteSpace: 'normal' });
  expect(await page.getByRole('listbox', { name: 'Slash commands' }).evaluate((element) => (
    element.scrollHeight > element.clientHeight && getComputedStyle(element).overflowY === 'auto'
  ))).toBe(true);

  await codexComposer.fill('/pl');
  const menu = page.getByRole('listbox', { name: 'Slash commands' });
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('option', { name: /\/plan/ })).toBeVisible();
  await expect(menu.getByRole('option', { name: /\/model/ })).toBeHidden();
  await menu.getByRole('option', { name: /\/plan/ }).click();
  await expect(codexComposer).toHaveValue('/plan ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);
  await codexComposer.pressSequentially('Review the release');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  expect((await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({
    pane_id: 'w1:p1', text: '/plan Review the release',
  });

  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Search all agents' }).click();
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Claude app');
  await jump.getByRole('button', { name: /Claude app/ }).click();
  const claudeComposer = page.getByRole('combobox', { name: 'Prompt' });
  await claudeComposer.fill('/he');
  await expect(page.getByRole('option', { name: /\/help/ })).toBeVisible();
  await claudeComposer.press('Enter');
  await expect(claudeComposer).toHaveValue('/help');
  expect((await commands(page)).filter((command) => command.type === 'list_slash_commands')).toHaveLength(2);
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(1);
});

test('scales the whole interface from accessible settings', async ({ page }) => {
  await boot(page, [fedora]);
  await page.getByRole('button', { name: 'Settings' }).click();
  const sizes = page.getByRole('group', { name: 'Interface Size' });
  const history = page.getByRole('group', { name: 'Terminal History' });
  const refresh = page.getByRole('group', { name: 'Terminal Refresh' });
  const heading = page.getByRole('heading', { name: 'Settings', level: 2 });
  expect(await refresh.getByRole('button', { name: '250 ms' })
    .evaluate((element) => getComputedStyle(element).textTransform)).toBe('none');

  await sizes.getByRole('button', { name: 'Compact' }).click();
  const compactHeadingSize = await heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  const compactInputSize = await page.getByLabel('Relay Name').evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  await page.getByLabel('Relay Name').focus();
  expect(compactInputSize).toBeGreaterThanOrEqual(16);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await sizes.getByRole('button', { name: 'Large' }).click();
  const largeHeadingSize = await heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));

  expect(largeHeadingSize).toBeGreaterThan(compactHeadingSize);
  expect(await page.evaluate(() => document.documentElement.dataset.interfaceSize)).toBe('large');
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_font_size'))).toBe('large');
  await expect(page.getByRole('group', { name: 'Terminal Width' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Fit to Phone' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Original Columns' })).toHaveCount(0);
  await expect(page.getByText(/Resize Session automatically leases/)).toBeVisible();

  await history.getByRole('button', { name: '10000' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_history_lines'))).toBe('10000');
  await refresh.getByRole('button', { name: '100 ms' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_refresh_ms'))).toBe('100');
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_realtime_delta', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'History app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open History app on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .some((command) => command.type === 'read_pane' && command.lines === 10000)).toBe(true);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    content: 'History output',
    format: 'ansi',
    content_fingerprint: 'history-content',
  });
  await expect.poll(async () => (await commands(page)).findLast((command) => command.type === 'watch_pane'))
    .toMatchObject({ pane_id: 'w1:p1', interval_ms: 100 });
});

test('scales and expands the prompt composer for multiline text', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Composer app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Composer app on Fedora' }).click();

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('One line');
  const compactHeight = (await composer.boundingBox())!.height;
  await composer.fill('Line one\nLine two\nLine three\nLine four');
  await expect.poll(async () => (await composer.boundingBox())!.height).toBeGreaterThan(compactHeight + 20);

  await composer.fill(Array.from({ length: 30 }, (_, index) => `Line ${index + 1}`).join('\n'));
  expect(await composer.evaluate((element) => {
    const textarea = element as HTMLTextAreaElement;
    return textarea.scrollHeight > textarea.clientHeight && getComputedStyle(textarea).overflowY === 'auto';
  })).toBe(true);

  await page.getByRole('button', { name: 'Settings' }).click();
  const sizes = page.getByRole('group', { name: 'Interface Size' });
  await sizes.getByRole('button', { name: 'Regular' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await composer.fill('One line');
  const regularHeight = (await composer.boundingBox())!.height;

  await page.getByRole('button', { name: 'Settings' }).click();
  await sizes.getByRole('button', { name: 'Large' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await composer.fill('One line');
  const largeHeight = (await composer.boundingBox())!.height;

  expect(regularHeight).toBeGreaterThan(compactHeight);
  expect(largeHeight).toBeGreaterThan(regularHeight);
});

test('handles approvals, chained questions, and notification routing', async ({ page }) => {
  const target = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', action: 'approve', index: 0, total: 2, notification_id: 'notice-1',
  }));
  await boot(page, [fedora], `/#notify=${target}`);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Approvals', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', event_id: 'notice-1', project: 'Approvals', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  await expect(page.getByRole('main', { name: /Terminal for Approvals/ })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(1);
  expect((await commands(page)).find((command) => command.type === 'respond')).toMatchObject({ event_id: 'notice-1' });

  const first = {
    id: 'q1', kind: 'single_select', question: 'Choose deployment scope',
    options: [{ index: 0, label: 'Repository', description: 'All files' }, { index: 1, label: 'Module' }],
    other: { label: 'None of the above', placeholder: 'Optional notes', allow_empty: true },
    submit_label: 'Next', can_go_back: false, can_chat: true, question_index: 1, question_total: 2,
  };
  const second = {
    ...first, id: 'q2', question: 'Choose device coverage', submit_label: 'Submit', can_go_back: true, question_index: 2,
  };
  await setAutoCommands(page, false);
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', attention_kind: 'question', project: 'Approvals', agent: 'claude', interaction: first, question_layout: true });
  await expect(page.getByText('Question 1 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Chat about this' })).toBeHidden();
  await page.evaluate(() => {
    const trace = window as unknown as {
      __questionUnmounts: number;
      __questionObserver: MutationObserver;
    };
    trace.__questionUnmounts = 0;
    trace.__questionObserver = new MutationObserver(() => {
      if (!document.querySelector('form.question-form')) trace.__questionUnmounts += 1;
    });
    trace.__questionObserver.observe(document.body, { childList: true, subtree: true });
  });
  await page.getByRole('radio', { name: /Repository/ }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;

  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Approvals', agent: 'claude' }],
  });
  await expect(page.getByRole('group', { name: 'Choose deployment scope' })).toBeVisible();
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'chat',
      project: 'Approvals', agent: 'claude',
    }],
  });
  await expect(page.getByRole('group', { name: 'Choose deployment scope' })).toBeVisible();
  await expect(page.getByRole('main', { name: /Questions for Approvals/ })).toBeVisible();

  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id,
    ok: true, phase: 'advanced', data: { interaction: second },
  });
  await setAutoCommands(page, true);
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeVisible();
  await expect(page.getByText('Question 2 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Previous' })).toBeVisible();
  expect(answer).toMatchObject({ selected_indices: [0], other_selected: false, protocol: 2 });
  for (let update = 0; update < 3; update += 1) {
    await server(page, 0, {
      type: 'agent_update', pane_id: 'w1:p1', status: 'blocked', pane_revision: 10 + update,
    });
  }
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi',
    attention_kind: 'question', interaction: second, question_layout: true,
  });
  expect(await page.evaluate(() => {
    const trace = window as unknown as {
      __questionUnmounts: number;
      __questionObserver: MutationObserver;
    };
    trace.__questionObserver.disconnect();
    return trace.__questionUnmounts;
  })).toBe(0);

  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: 'Module' }).click();
  await page.getByRole('button', { name: 'Submit' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'answer_question').length).toBe(2);
  const finalAnswer = (await commands(page))
    .filter((command) => command.type === 'answer_question').at(-1)!;
  await server(page, 0, {
    type: 'blocked', pane_id: 'w1:p1', attention_kind: 'unknown',
    project: 'Approvals', agent: 'claude', interaction: null, question_layout: false,
  });
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: finalAnswer.request_id,
    ok: true, phase: 'confirmed',
  });
  await expect(page.getByText('Answers submitted.')).toBeVisible();
  await setAutoCommands(page, true);

  await server(page, 0, {
    type: 'blocked', pane_id: 'w1:p1', attention_kind: 'approval', project: 'Approvals', agent: 'claude',
    interaction: null, question_layout: false, options: ['Proceed with plan', 'Cancel'],
  });
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Proceed with plan' })).toBeVisible();
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await expect(composer).toBeDisabled();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Proceed with plan' }).click();
  await expect(page.getByRole('button', { name: 'Copy', exact: true })).toBeDisabled();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(2);
  const approvalResponse = (await commands(page)).filter((command) => command.type === 'respond').at(-1)!;
  await server(page, 0, {
    type: 'command_result', request_id: approvalResponse.request_id, action: 'respond', ok: true, phase: 'accepted',
  });
  await setAutoCommands(page, true);

  const working = { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Approvals', agent: 'claude' }] };
  await server(page, 0, working);
  await server(page, 0, working);
  await expect(composer).toBeEnabled();
});

test('hands an active chat to structured questions without losing terminal position', async ({ page }) => {
  const interaction = {
    id: 'live-question', kind: 'single_select', question: 'Choose the delivery style',
    options: [{ index: 0, label: 'Direct' }, { index: 1, label: 'Staged' }],
    submit_label: 'Submit', can_go_back: false, question_index: 1, question_total: 1,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Live chat', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Live chat on Fedora' }).click();
  const terminal = page.getByRole('log');
  const content = Array.from({ length: 140 }, (_, index) => `conversation line ${index + 1}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content });
  await expect(terminal).toContainText('conversation line 140');
  const before = await terminal.evaluate((element) => {
    const target = Math.max(0, element.scrollHeight - element.clientHeight - 80);
    element.scrollTop = target;
    element.dispatchEvent(new Event('scroll'));
    return target;
  });
  await expect.poll(async () => terminal.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);

  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content,
    attention_kind: 'question',
    interaction,
    question_layout: true,
  });
  await expect(page.getByRole('group', { name: interaction.question })).toBeVisible();
  await expect(page.getByRole('log')).toBeHidden();

  for (let update = 0; update < 2; update += 1) {
    await server(page, 0, {
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Live chat', agent: 'codex' }],
    });
  }
  await expect(page.getByRole('group', { name: interaction.question })).toBeHidden();
  const after = await terminal.evaluate((element) => element.scrollTop);
  expect(Math.abs(after - before), `before=${before} after=${after}`).toBeLessThan(2);
});

test('shows a live plan question immediately without reopening the agent', async ({ page }) => {
  const interaction = {
    id: 'plan-question', kind: 'single_select',
    question: 'Where should the new app live relative to the existing Herdr relay app?',
    options: [
      { index: 0, label: 'Separate package', description: 'Keep the relay release path untouched.' },
      { index: 1, label: 'New route', description: 'Add the questionnaire inside the existing app.' },
    ],
    other: { label: 'None of the above', placeholder: 'Add details' },
    submit_label: 'Next', can_go_back: false, question_index: 1, question_total: 3,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Plan app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Plan app on Fedora' }).click();
  const questionContent = 'Question 1/3 (3 unanswered)\nWhere should the new app live?';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: questionContent,
    content_fingerprint: 'plan-question-content',
  });
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toContainText('Question 1/3');
  await server(page, 0, {
    type: 'pane_delta',
    pane_id: 'w1:p1',
    format: 'plain',
    base_fingerprint: 'plan-question-content',
    content_fingerprint: 'plan-question-content',
    segments: [],
    attention_kind: 'question',
    interaction,
    question_layout: true,
  });

  await expect(page.getByRole('group', { name: interaction.question })).toBeVisible();
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toBeHidden();
  await expect(page.getByRole('radio', { name: 'Separate package' })).toBeVisible();
  await expect(page.getByPlaceholder('Type a reply…')).toBeHidden();
});

test('keeps a nonfinal question mounted when a transient frame is confirmed', async ({ page }) => {
  const first = {
    id: 'q1', kind: 'single_select', question: 'Choose deployment scope',
    options: [{ index: 0, label: 'Repository' }, { index: 1, label: 'Module' }],
    other: { label: 'Other', placeholder: 'Other answer' },
    submit_label: 'Next', can_go_back: false, question_index: 1, question_total: 2,
  };
  const second = {
    ...first, id: 'q2', question: 'Choose device coverage',
    submit_label: 'Submit', can_go_back: true, question_index: 2,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question',
      project: 'Questions', agent: 'claude', interaction: first, question_layout: true,
    }],
  });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: 'Repository' }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;

  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id,
    ok: true, phase: 'confirmed',
  });
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await expect(page.getByText('Unexpected question result.')).toBeVisible();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi',
    attention_kind: 'question', interaction: second, question_layout: true,
  });
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
});

test('rejects stale notification approvals and retries transient failures', async ({ page }) => {
  const staleTarget = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', action: 'approve', index: 0, total: 2, notification_id: 'old-event',
  }));
  await boot(page, [fedora], `/#notify=${staleTarget}`);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', options: ['Approve once', 'Deny'], event_id: 'new-event', project: 'Stale approval', agent: 'codex' }],
  });
  await expect(page.getByRole('status').filter({ hasText: /older approval request/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  const retryTarget = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', action: 'approve', index: 0, total: 2, notification_id: 'new-event',
  }));
  await page.evaluate(() => (window as any).__relayAutoCommands(false));
  await page.evaluate((target) => { location.hash = `#notify=${target}`; }, retryTarget);
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(1);
  const first = (await commands(page)).filter((command) => command.type === 'respond')[0];
  await server(page, 0, {
    type: 'command_result', request_id: first.request_id, action: 'respond', ok: false, phase: 'failed', error: 'Relay reconnecting',
  });
  await expect(page.getByRole('status').filter({ hasText: 'Relay reconnecting' })).toBeVisible();

  await page.evaluate(() => (window as any).__relayAutoCommands(true));
  await page.evaluate((target) => { location.hash = `#notify=${target}`; }, retryTarget);
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(2);
});

test('restores structured questions from the cached agent snapshot after reload', async ({ page }) => {
  const interaction = {
    id: 'reload-question', kind: 'single_select', question: 'Choose reconnect behavior',
    options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Fixed retry' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const snapshot = {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude',
      prompt: interaction.question, command: interaction.question, options: [],
      interaction, question_layout: true,
    }],
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, snapshot);
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();

  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, snapshot);

  await expect(page.getByText('Choose reconnect behavior')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'yes, single permission' })).toBeHidden();
});

test('restores a confirmed choice after navigating away from an incomplete draft', async ({ page }) => {
  const first = {
    id: 'confirmed-reconnect', kind: 'single_select', question: 'Choose reconnect strategy',
    options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Signals' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'confirmed-offline', kind: 'multi_select', question: 'Choose offline scope',
    options: [{ index: 0, label: 'App shell' }], submit_label: 'Next', can_go_back: true,
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: first, question_layout: true });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await page.getByRole('textbox', { name: 'Other answer' }).focus();
  await expect(page.getByRole('radio', { name: 'Other' })).toBeChecked();

  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: second, question_layout: true });
  await expect(page.getByRole('group', { name: 'Choose offline scope' })).toBeVisible();
  const confirmed = {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 1 })),
  };
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: confirmed, question_layout: true });

  await expect(page.getByRole('radio', { name: 'Signals' })).toBeChecked();
  await expect(page.getByRole('radio', { name: 'Other' })).not.toBeChecked();
});

test('keeps the third single choice checked across live pane transitions', async ({ page }) => {
  const first = {
    id: 'live-reconnect', kind: 'single_select', question: 'When the relay connection drops, how should the client attempt to reconnect?',
    options: [
      { index: 0, label: 'Exponential backoff', description: 'Retry on a growing delay.' },
      { index: 1, label: 'Fixed short interval', description: 'Retry every few seconds.' },
      { index: 2, label: 'Backoff plus signals', description: 'Reset when connectivity returns.', selected: true },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'live-offline', kind: 'multi_select', question: 'Which capabilities should remain available offline?',
    options: [
      { index: 0, label: 'App shell', selected: true },
      { index: 1, label: 'Queued prompts' },
      { index: 2, label: 'Activity cache', selected: true },
      { index: 3, label: 'Notification handoff' },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: true,
  };
  const agent = {
    pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude',
    interaction: first, question_layout: true,
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [agent] });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Questions for Questions' })).toBeVisible();
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Refresh terminal' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Attach image' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Arrow keys' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Tab', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toBeVisible();
  await expect(page.getByRole('radio', { name: /Backoff plus signals/ })).toBeChecked();

  await page.evaluate(() => (window as any).__relayAutoCommands(false));
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: second, question_layout: true });
  await server(page, 0, { type: 'command_result', request_id: answer.request_id, ok: true, phase: 'advanced', data: { interaction: second } });
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();

  await page.getByRole('button', { name: /Previous/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'navigate_question').length).toBe(1);
  const navigation = (await commands(page)).find((command) => command.type === 'navigate_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: first, question_layout: true });
  await server(page, 0, { type: 'command_result', request_id: navigation.request_id, ok: true, phase: 'navigated', data: { interaction: first } });
  await server(page, 0, { type: 'agents', agents: [agent] });
  for (let refresh = 0; refresh < 20; refresh += 1) {
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: first, question_layout: true });
    await page.waitForTimeout(5);
  }

  await expect(page.getByRole('radio', { name: /Backoff plus signals/ })).toBeChecked();
  await expect(page.getByRole('button', { name: 'Next' })).toBeEnabled();
  expect((await commands(page)).filter((command) => command.type === 'read_pane').length).toBeLessThan(6);
});

test('keeps normal single-select answers across repeated question navigation', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const first = {
    id: 'stable-reconnect', kind: 'single_select', question: 'Choose reconnect behavior',
    options: [
      { index: 0, label: 'Backoff' },
      { index: 1, label: 'Fixed retry' },
      { index: 2, label: 'Backoff plus signals' },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'offline-scope', kind: 'multi_select', question: 'Choose offline scope',
    options: [{ index: 0, label: 'App shell' }, { index: 1, label: 'Activity cache' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: true,
  };
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: first, question_layout: true });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  const questionForm = page.getByRole('form', { name: 'Choose reconnect behavior' });
  const formHeight = await questionForm.evaluate((element) => element.getBoundingClientRect().height);
  expect(formHeight / await page.evaluate(() => innerHeight)).toBeGreaterThan(0.65);

  await page.getByRole('textbox', { name: 'Other answer' }).fill('Hello');
  await expect(page.getByRole('radio', { name: 'Other' })).toBeChecked();
  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), second);
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: 'Choose offline scope' })).toBeVisible();
  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), {
    ...first, can_go_back: false, other: { ...first.other, selected: true, text: 'Hello' },
  });
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await page.getByRole('radio', { name: 'Backoff plus signals' }).click();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');

  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), second);
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 2 })),
    other: { ...first.other, selected: false, text: '' },
  });
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Backoff plus signals' })).toBeChecked();
  await expect(page.getByRole('radio', { name: 'Other' })).not.toBeChecked();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');
  expect((await commands(page)).filter((command) => command.type === 'navigate_question')).toHaveLength(2);
});

test('does not report failed question navigation as opened', async ({ page }) => {
  const second = {
    id: 'failed-navigation', kind: 'single_select', question: 'Choose release scope',
    options: [{ index: 0, label: 'Backend' }, { index: 1, label: 'Everything' }],
    submit_label: 'Submit', can_go_back: true, question_index: 2, question_total: 2,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'codex',
      interaction: second, question_layout: true,
    }],
  });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await page.evaluate(() => (window as any).__relayAutoCommands(false));
  await page.getByRole('button', { name: 'Previous' }).click();
  const navigation = (await commands(page)).find((command) => command.type === 'navigate_question')!;
  await server(page, 0, {
    type: 'command_result', request_id: navigation.request_id, action: 'navigate_question',
    ok: true, phase: 'accepted',
  });
  await server(page, 0, {
    type: 'command_result', request_id: navigation.request_id, action: 'navigate_question',
    ok: false, phase: 'unconfirmed', error: 'The agent still shows the same question; try again',
  });
  await expect(page.getByRole('status').filter({ hasText: 'still shows the same question' })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'Opened previous question' })).toBeHidden();
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
});

test('refreshes agents on return home and preserves shared terminal behavior', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode' }] });
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  const attachImage = page.getByRole('button', { name: 'Attach image' });
  const arrowKeys = page.getByRole('button', { name: 'Arrow keys' });
  const enterKey = page.getByRole('button', { name: 'Enter' });
  const tabKey = page.getByRole('button', { name: 'Tab', exact: true });
  const ctrlKey = page.getByRole('button', { name: 'Ctrl', exact: true });
  const shiftKey = page.getByRole('button', { name: 'Shift', exact: true });
  const altKey = page.getByRole('button', { name: 'Alt', exact: true });
  const modifierLetter = page.getByRole('textbox', { name: 'Modifier shortcut character' });
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(attachImage.locator('svg')).toBeVisible();
  await expect(arrowKeys.locator('svg')).toBeVisible();
  await expect(enterKey).toBeVisible();
  await expect(ctrlKey).toBeVisible();
  await expect(shiftKey).toBeVisible();
  await expect(altKey).toBeVisible();
  await expect(copyOutput).toBeVisible();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(altKey).toHaveAttribute('aria-pressed', 'false');
  await expect(attachImage).not.toContainText('▧');
  await expect(arrowKeys).not.toContainText('⌨');
  await arrowKeys.click();
  await expect(page.getByRole('button', { name: 'Up', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: /^(Home|End|Page up|Page down)$/ })).toHaveCount(0);
  await expect(page.locator('.arrow-popup').getByRole('button', { name: 'Enter' })).toHaveCount(0);
  await enterKey.click();
  expect((await commands(page)).find((command) => command.type === 'send_keys')).toMatchObject({
    pane_id: 'w1:p1', keys: ['Enter'],
  });
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('c');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+c'])).length).toBe(1);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('d');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+d'])).length).toBe(1);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await modifierLetter.press('c');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+c'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('o');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+o'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');
  await expect(modifierLetter).not.toBeFocused();
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['Tab'])).length).toBe(1);
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await tabKey.click();
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+tab'])).length).toBe(2);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+tab'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await arrowKeys.click();
  await page.getByRole('button', { name: 'Up', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+up'])).length).toBe(1);
  await altKey.click();
  await expect(altKey).toHaveAttribute('aria-pressed', 'true');
  await modifierLetter.press('x');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+alt+shift+x'])).length).toBe(1);
  await altKey.click();
  await enterKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+enter'])).length).toBe(1);
  await expect(page.getByRole('status').filter({ hasText: 'Ctrl+Shift+Enter sent' })).toBeVisible();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await shiftKey.click();
  await expect(modifierLetter).not.toBeFocused();
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await page.getByRole('combobox', { name: 'Prompt' }).focus();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(modifierLetter).not.toBeFocused();
  const refreshesBeforeBack = (await commands(page)).filter((command) => command.type === 'refresh_agents').length;
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'refresh_agents').length)
    .toBe(refreshesBeforeBack + 1);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode' }] });
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeVisible();
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: '     + Thought: 1.0s\n\u001b[38;5;6m     Safe <img src=x onerror=alert(1)>\u001b[0m\n     Docs https://example.com/report?q=1\n     Details\n     ▣ Build · model · 1s' });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('Safe <img src=x onerror=alert(1)>');
  expect(await terminal.locator('img').count()).toBe(0);
  const terminalLink = terminal.getByRole('link', { name: 'https://example.com/report?q=1' });
  await expect(terminalLink).toHaveAttribute('target', '_blank');
  await expect(terminalLink).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(terminalLink).toHaveAttribute('referrerpolicy', 'no-referrer');
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal = text;
        },
      },
    });
  });
  await copyOutput.click();
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe('Safe <img src=x onerror=alert(1)>\nDocs https://example.com/report?q=1\nDetails');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: [
      '     + Thought: 0.08s',
      '     Updated the canonical plan to:',
      '',
      '     - Final plan consistency assertions pass.',
      '     ▣ Build · model · 0.08s',
      '     ※ recap: Goal: implement the strategy.',
    ].join('\n'),
  });
  await copyOutput.click();
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe('Updated the canonical plan to:\n\n- Final plan consistency assertions pass.');

  const permissionHeader = terminal.locator('.ansi-line', { hasText: 'Permissions ·' });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: 'Permissions ·  \u001b[48;2;36;74;50mAllow\u001b[0m Ask Deny',
  });
  await expect(permissionHeader.locator('span', { hasText: 'Allow' }))
    .toHaveCSS('background-color', 'rgb(36, 74, 50)');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: 'Permissions ·  Allow Ask \u001b[48;2;36;74;50mDeny\u001b[0m',
  });
  await expect(permissionHeader.locator('span', { hasText: 'Deny' }))
    .toHaveCSS('background-color', 'rgb(36, 74, 50)');

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: ['Before', '', '', '', '', '----------------', '', '————————', '', '________________', '', '', '', 'After'].join('\n'),
  });
  expect(await terminal.evaluate((element) => {
    const rows = element.querySelector('.term-screen')?.children || element.children;
    let blankRun = 0;
    let maximumBlankRun = 0;
    for (const row of rows) {
      if (row.classList.contains('ansi-line') && !row.textContent?.trim()) {
        blankRun += 1;
        maximumBlankRun = Math.max(maximumBlankRun, blankRun);
      } else blankRun = 0;
    }
    return {
      maximumBlankRun,
      separators: element.querySelectorAll('.term-separator').length,
    };
  })).toEqual({ maximumBlankRun: 4, separators: 0 });

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: `─ Worked for 1m 46s ${'─'.repeat(120)}`,
  });
  await expect(terminal.locator('.ansi-line')).toHaveText(`─ Worked for 1m 46s ${'─'.repeat(120)}`);

  const claudeRule = '─'.repeat(120);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'claude' }],
  });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      `\u001b[36m│\u001b[0m  Claude result${' '.repeat(80)}\u001b[36m│\u001b[0m`,
      '\u001b[36m│\u001b[0m',
      `\u001b[36m╰${claudeRule}╯\u001b[0m`,
      `\u001b[36m${claudeRule}\u001b[0m`,
      `\u001b[36m╭${claudeRule}╮\u001b[0m`,
      `\u001b[38;5;147m${'▔'.repeat(150)}\u001b[0m`,
      `\u001b[2m${claudeRule} Opus 4.8 | ctx: 20%\u001b[0m`,
    ].join('\n'),
  });
  await expect(terminal.locator('.ansi-line').filter({ hasText: 'Claude result' })).toContainText('Claude result');
  await expect(terminal.locator('.ansi-line').filter({ hasText: 'Opus 4.8' })).toContainText('Opus 4.8 | ctx: 20%');
  await expect(terminal.locator('.term-separator')).toHaveCount(0);

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: `${'.'.repeat(120)} [29%]`,
  });
  await expect(terminal.locator('.ansi-line')).toHaveText(`${'.'.repeat(120)} [29%]`);

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: '\u001b[48;2;250;250;250;38;2;20;20;20mMac light terminal\u001b[0m',
  });
  const normalizedMacRow = terminal.locator('.ansi-line', { hasText: 'Mac light terminal' });
  await expect(normalizedMacRow).toHaveCSS('background-color', 'rgb(61, 64, 64)');
  await expect(normalizedMacRow.locator('span')).toHaveCSS('color', 'rgb(236, 239, 244)');

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.focus();
  await composer.fill('draft prompt');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'newest live frame' });
  await expect(page.getByRole('log')).toContainText('newest live frame');
  await expect(composer).toBeFocused();
  await expect(composer).toHaveValue('draft prompt');
  await composer.evaluate((element) => (element as HTMLTextAreaElement).blur());

  const longFrame = Array.from({ length: 120 }, (_, index) => `terminal line ${index}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: longFrame });
  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: `${longFrame}\nlatest output` });
  const jumpToLatest = page.getByRole('button', { name: 'Jump to latest' });
  await expect(jumpToLatest).toBeVisible();
  await jumpToLatest.click();
  await expect.poll(() => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);

  await page.locator('input[type=file]').setInputFiles({ name: 'shot.png', mimeType: 'image/png', buffer: Buffer.from('png') });
  await expect(composer).toHaveValue(/Image: \/home\/test\/.cache\/herdr-mobile-relay\/uploads\/shot.png/);
  expect((await commands(page)).find((command) => command.type === 'upload_image')).toMatchObject({ mime: 'image/png', protocol: 2, pane_id: 'w1:p1' });

  await composer.fill('send this');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  expect((await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({ text: 'send this', pane_id: 'w1:p1' });
});

test('uses relay response copy and allows dismissing the markdown preview', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification', 'clear_activities', 'directory_browser', 'self_update',
      'structured_questions', 'slash_commands', 'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Terminal app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'remote copy fixture',
  });
  const copied = '# Remote markdown response\n\n- Exact copied output';
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal = text;
        },
      },
    });
  });
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(copyOutput).toBeVisible();
  await copyOutput.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe(copied);
  const preview = page.getByRole('region', { name: 'Copied agent response' });
  await expect(preview).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Copied agent response markdown' })).toHaveValue(copied);
  await page.getByRole('button', { name: 'Dismiss' }).click();
  await expect(preview).toBeHidden();
});

test('surfaces relay response-copy failures instead of parsing terminal output', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification', 'clear_activities', 'directory_browser', 'self_update',
      'structured_questions', 'slash_commands', 'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Terminal app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'parser fallback must not run',
  });
  await setAutoCommands(page, false);
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(copyOutput).toBeVisible();
  await copyOutput.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const command = (await commands(page)).find((entry) => entry.type === 'copy_agent_response')!;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: command.request_id,
    ok: false,
    phase: 'failed',
    error: 'The agent did not confirm a copied response; try again',
  });
  await expect(page.getByRole('status').filter({
    hasText: 'The agent did not confirm a copied response; try again',
  })).toBeVisible();
});

test('resets the home page scroll offset before opening a terminal', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: Array.from({ length: 20 }, (_, index) => ({
      pane_id: `w1:p${index + 1}`,
      status: 'working',
      project: `Scrollable agent ${index + 1}`,
      agent: 'codex',
    })),
  });

  const lastWorkspace = page.locator('.workspace-card').filter({ hasText: 'Scrollable agent 20' });
  await lastWorkspace.locator('summary').click();
  const lastAgent = page.getByRole('button', { name: 'Open Scrollable agent 20 on Fedora' });
  await page.evaluate(() => {
    document.documentElement.style.minHeight = '300vh';
    window.scrollTo(0, 100);
  });
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  await page.evaluate(() => { window.scrollTo = () => {}; });
  await lastAgent.click();

  const terminal = page.getByRole('main', { name: 'Terminal for Scrollable agent 20' });
  await expect(terminal).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  await expect.poll(async () => terminal.evaluate((element) => {
    const bounds = element.getBoundingClientRect();
    return Math.round(window.innerHeight - bounds.bottom);
  })).toBe(0);
});

test('ignores a directory result after switching computers', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'wss://mac.example', token: '' };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 0);
  await handshake(page, 1);
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0)).filter((command) => command.type === 'list_directories').length).toBe(1);
  const fedoraDirectory = (await commandsForSocket(page, 0)).find((command) => command.type === 'list_directories')!;

  await page.getByLabel('Computer').selectOption('mac');
  await expect.poll(async () => (await commandsForSocket(page, 1)).filter((command) => command.type === 'list_directories').length).toBe(1);
  await page.getByLabel('Agent', { exact: true }).selectOption('codex');
  const macDirectory = (await commandsForSocket(page, 1)).find((command) => command.type === 'list_directories')!;
  await server(page, 1, {
    type: 'command_result', request_id: macDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/Users/test/mac-project', label: '~/mac-project' },
      parent: '/Users/test', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/mac-project' })).toBeVisible();

  await server(page, 0, {
    type: 'command_result', request_id: fedoraDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/fedora-project', label: '~/fedora-project' },
      parent: '/home/test', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/mac-project' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('mac-project-codex');

  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commandsForSocket(page, 1)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commandsForSocket(page, 1)).find((command) => command.type === 'agent_start')).toMatchObject({
    cwd: '/Users/test/mac-project', name: 'mac-project-codex', profile_id: 'codex',
  });
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'agent_start')).toHaveLength(0);
});

test('launches and manages agent lifecycle commands', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    agent_profiles: [
      { id: 'qodercli', label: 'Qoder' },
      { id: 'opencode', label: 'OpenCode' },
      { id: 'codex', label: 'Codex' },
      { id: 'pi', label: 'Pi' },
      { id: 'kimi', label: 'Kimi' },
      { id: 'claude', label: 'Claude Code' },
      { id: 'omp', label: 'Oh My Pi' },
    ],
  });
  await page.getByRole('button', { name: 'Start agent' }).click();
  const agentType = page.getByLabel('Agent', { exact: true });
  await expect(agentType.locator('option')).toHaveText([
    'Claude Code', 'Codex', 'Kimi', 'Oh My Pi', 'OpenCode', 'Pi', 'Qoder',
  ]);
  await expect(agentType).toHaveValue('claude');
  await agentType.selectOption('codex');
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const directoryCommand = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: directoryCommand.request_id, ok: true, phase: 'confirmed',
    data: {
      current: { path: '/home/test/Development/relay', label: '~/Development/relay' },
      parent: '/home/test/Development',
      directories: [{ name: 'frontend', path: '/home/test/Development/relay/frontend' }],
    },
  });
  await expect(page.getByLabel('Name')).toHaveValue('relay-codex');
  const currentDirectory = page.getByRole('button', { name: '~/Development/relay' });
  const directoryList = page.getByLabel('Subdirectories');
  await currentDirectory.click();
  await expect(directoryList).toBeVisible();
  await page.getByLabel('Name').focus();
  await expect(directoryList).toBeHidden();
  await currentDirectory.click();
  await page.getByRole('button', { name: /frontend/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(2);
  const childDirectoryCommand = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(childDirectoryCommand).toMatchObject({ path: '/home/test/Development/relay/frontend' });
  await server(page, 0, {
    type: 'command_result', request_id: childDirectoryCommand.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/Development/relay/frontend', label: '~/Development/relay/frontend' },
      parent: '/home/test/Development/relay', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/relay/frontend' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('frontend-codex');
  await page.getByRole('button', { name: 'Open parent directory' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(3);
  const parentDirectoryCommand = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(parentDirectoryCommand).toMatchObject({ path: '/home/test/Development/relay' });
  await server(page, 0, {
    type: 'command_result', request_id: parentDirectoryCommand.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/Development/relay', label: '~/Development/relay' },
      parent: '/home/test/Development', directories: [{ name: 'frontend', path: '/home/test/Development/relay/frontend' }],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/relay' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('relay-codex');
  await page.getByLabel(/Initial task/).fill('Run the migration');
  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    profile_id: 'codex', cwd: '/home/test/Development/relay', name: 'relay-codex', prompt: 'Run the migration',
  });
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeHidden();

  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p2', status: 'working', project: 'relay',
      cwd: '/home/test/Development/relay', name: 'relay-codex', agent: 'codex',
      session: 'Current Session Review',
    }],
  });
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();
  await expect.poll(() => page.evaluate(() => location.hash)).toBe('#pane=fedora%3A%3Aw1%3Ap2');
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const manageDialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(manageDialog).toBeVisible();
  await expect(manageDialog.getByLabel('New tab name')).toHaveCount(0);
  await expect(manageDialog.getByLabel('New session name')).toHaveCount(0);
  const renameTabAction = manageDialog.getByRole('button', { name: 'Rename Tab' });
  await expect(renameTabAction).toBeVisible();
  await expect(manageDialog.getByRole('button', { name: 'Rename Session' })).toBeVisible();
  await renameTabAction.click();
  await expect(manageDialog.getByLabel('New tab name')).toBeFocused();
  await expect(manageDialog.getByLabel('New tab name')).toHaveValue('relay-codex');
  await setAutoCommands(page, false);
  await manageDialog.getByLabel('New tab name').fill('Sol-fhsh');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'agent_rename' && command.name === 'Sol-fhsh')).toBe(true);
  const failedRename = (await commands(page)).find((command) =>
    command.type === 'agent_rename' && command.name === 'Sol-fhsh')!;
  await server(page, 0, {
    type: 'command_result', action: 'agent_rename', request_id: failedRename.request_id,
    ok: false, phase: 'failed', error: 'Could not rename tab',
  });
  const renameError = page.getByRole('status').filter({ hasText: 'Could not rename tab' });
  await expect(renameError).toBeVisible();
  expect(await renameError.evaluate((element) => element.matches(':popover-open'))).toBe(true);
  await expect(manageDialog).toBeVisible();
  await setAutoCommands(page, true);
  await manageDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(renameTabAction).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(manageDialog).toBeHidden();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await renameTabAction.click();
  await manageDialog.getByLabel('New tab name').fill('123');
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p2', status: 'working', updated_at: 2 });
  await expect(manageDialog.getByLabel('New tab name')).toHaveValue('123');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'agent_rename' && command.name === '123')).toBe(true);

  await page.getByRole('button', { name: 'Manage agent' }).click();
  const renameSessionAction = manageDialog.getByRole('button', { name: 'Rename Session' });
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toBeFocused();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('Current Session Review');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename Current Session Review')).toBe(true);
  await expect(manageDialog).toBeHidden();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('Current Session Review');
  await manageDialog.getByLabel('New session name').fill('cancelled-session');
  await manageDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(renameSessionAction).toBeFocused();
  expect((await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename cancelled-session')).toBe(false);
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', session_name: '', updated_at: 3,
  });
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('');
  await manageDialog.getByLabel('New session name').fill('renamed-session');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename renamed-session')).toBe(true);

  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', agent: 'opencode', updated_at: 4,
  });
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await expect(manageDialog.getByRole('button', { name: 'Rename Tab' })).toBeVisible();
  await expect(manageDialog.getByRole('button', { name: 'Rename Session' })).toHaveCount(0);
  await page.keyboard.press('Escape');
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', agent: 'codex', updated_at: 5,
  });

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('button', { name: 'Clear Agent' }).click();
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p2', status: 'working', updated_at: 3 });
  await expect(page.getByRole('button', { name: 'Confirm Clear' })).toBeVisible();
  await page.getByRole('button', { name: 'Confirm Clear' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_clear')).toBe(true);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p3', status: 'working', project: 'relay', cwd: '/home/test/Development/relay', name: 'clear-codex-123', agent: 'codex' }] });
  await expect.poll(() => page.evaluate(() => location.hash)).toBe('#pane=fedora%3A%3Aw1%3Ap3');
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('button', { name: 'Stop Agent' }).click();
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p3', status: 'working', updated_at: 4 });
  await expect(page.getByRole('button', { name: 'Confirm Stop' })).toBeVisible();
  await page.getByRole('button', { name: 'Confirm Stop' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_stop')).toBe(true);
});

import { expect, test, type Page } from '@playwright/test';

interface RelayFixture {
  id: string;
  label: string;
  url: string;
  token: string;
}

async function boot(page: Page, relays: RelayFixture[] = [], path = '/') {
  await page.addInitScript(({ savedRelays }) => {
    if (savedRelays.length) localStorage.setItem('herdr_relays', JSON.stringify(savedRelays));
    const nativeSetTimeout = window.setTimeout.bind(window);
    window.setTimeout = ((handler: TimerHandler, timeout?: number, ...args: unknown[]) =>
      nativeSetTimeout(handler, timeout === 3000 ? 30 : timeout, ...args)) as typeof window.setTimeout;

    const sockets: MockSocket[] = [];
    const commands: Record<string, unknown>[] = [];
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
      constructor(readonly url: string) {
        sockets.push(this);
        queueMicrotask(() => {
          this.readyState = MockSocket.OPEN;
          this.onopen?.();
        });
      }
      send(serialized: string) {
        const message = JSON.parse(serialized) as Record<string, unknown>;
        commands.push(message);
        if (message.type === 'read_pane' || message.type === 'get_activity' || message.type === 'list_directories' || message.type === 'refresh_agents') return;
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
        if (message.type === 'push_subscribe' || message.type === 'push_unsubscribe') return;
        const phase = message.type === 'answer_question' && nextInteraction
          ? 'advanced'
          : message.type === 'navigate_question' && nextInteraction ? 'navigated' : 'confirmed';
        let data: Record<string, unknown> = {};
        if ((message.type === 'answer_question' || message.type === 'navigate_question') && nextInteraction) data = { interaction: nextInteraction };
        else if (message.type === 'agent_start') data = { pane_id: 'w1:pre-placement' };
        else if (message.type === 'agent_clear') data = {
          pane_id: 'w1:pre-clear', name: 'clear-codex-123', cwd: '/home/test/Development/relay',
        };
        if (message.type === 'answer_question' || message.type === 'navigate_question') nextInteraction = null;
        queueMicrotask(() => this.server({ type: 'command_result', request_id: message.request_id, ok: true, phase, data }));
      }
      close() { this.readyState = MockSocket.CLOSED; }
      server(message: unknown) { this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent); }
      serverClose() { this.readyState = MockSocket.CLOSED; this.onclose?.(); }
    }

    Object.defineProperty(window, 'WebSocket', { configurable: true, value: MockSocket });
    Object.assign(window, {
      __relayCommands: commands,
      __relaySockets: sockets,
      __relayServer(index: number, message: unknown) { sockets[index]?.server(message); },
      __relayClose(index: number) { sockets[index]?.serverClose(); },
      __relayNextInteraction(interaction: Record<string, unknown>) { nextInteraction = interaction; },
      __relayAutoCommands(enabled: boolean) { autoCommands = enabled; },
    });
  }, { savedRelays: relays });
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

async function handshake(page: Page, index: number, overrides: Record<string, unknown> = {}) {
  await server(page, index, {
    type: 'push_config', protocol: 2, version: 'abc1234', host: index ? 'mac' : 'fedora',
    capabilities: ['directory_browser', 'structured_questions', 'slash_commands'],
    agent_profiles: [{ id: 'codex', label: 'Codex' }, { id: 'claude', label: 'Claude Code' }],
    ...overrides,
  });
}

const fedora = { id: 'fedora', label: 'Fedora', url: 'wss://fedora.example', token: 'secret' };
const mac = { id: 'mac', label: 'Mac', url: 'wss://mac.example', token: 'secret' };

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
  await boot(page, [], '/#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation');
  await expect(page.getByRole('button', { name: 'Activity history' }).locator('svg')).toBeVisible();
  await expect.poll(() => socketCount(page)).toBe(1);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[0]))
    .toMatchObject({ label: 'Fedora Workstation', token: '0123456789abcdef0123456789abcdef' });
  expect(await page.evaluate(() => location.hash)).toBe('');

  await page.evaluate((relay) => {
    const saved = JSON.parse(localStorage.getItem('herdr_relays') || '[]');
    localStorage.setItem('herdr_relays', JSON.stringify([...saved, relay]));
  }, mac);
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(2);
  const base = 0;
  await handshake(page, base);
  await handshake(page, base + 1);
  await server(page, base, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app', agent: 'codex' }] });
  await server(page, base + 1, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Mac app', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  const headerBox = await page.getByRole('banner').boundingBox();
  const connectionBox = await page.getByRole('img', { name: /relays connected/ }).boundingBox();
  const settingsBox = await page.getByRole('button', { name: 'Settings' }).boundingBox();
  expect(headerBox && connectionBox && settingsBox).toBeTruthy();
  const leadingInset = connectionBox!.x + connectionBox!.width / 2 - headerBox!.x;
  const trailingInset = headerBox!.x + headerBox!.width - settingsBox!.x - settingsBox!.width / 2;
  expect(Math.abs(leadingInset - trailingInset)).toBeLessThan(2);
  await expect(page.getByText('Fedora app')).toBeVisible();
  await expect(page.getByText('Mac app')).toBeVisible();
  await expect(page.getByText('@Fedora Workstation')).toBeVisible();
  await expect(page.getByText('@Mac')).toBeVisible();
});

test('reconnects and blocks mutations for an incompatible relay protocol', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { protocol: 1, version: 'old' });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Old relay', agent: 'codex', options: ['Approve once', 'Deny'] }] });
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByText(/Relay outdated/)).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Approve once' }).click();
  await expect(page.getByRole('status').filter({ hasText: /protocol v1/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  await page.evaluate(() => (window as any).__relayClose(0));
  await expect.poll(() => socketCount(page)).toBe(2);
});

test('resets drafts and terminal output when moving to another agent', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'working', project: 'Working A', agent: 'codex' },
      { pane_id: 'w1:p2', status: 'blocked', project: 'Blocked B', agent: 'claude', options: ['Approve once', 'Deny'] },
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

  await page.getByRole('button', { name: 'Open Codex app on Fedora' }).click();
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
  await expect(popover.getByRole('option')).toHaveCount(21);
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
  await page.getByRole('button', { name: 'Open Claude app on Fedora' }).click();
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
  const heading = page.getByRole('heading', { name: 'Settings', level: 2 });

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
});

test('handles approvals, chained questions, and notification routing', async ({ page }) => {
  const target = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', action: 'approve', index: 0, total: 2, notification_id: 'notice-1',
  }));
  await boot(page, [fedora], `/#notify=${target}`);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Approvals', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  await expect(page.getByRole('main', { name: /Terminal for Approvals/ })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(1);

  const first = {
    id: 'q1', kind: 'single_select', question: 'Choose deployment scope',
    options: [{ index: 0, label: 'Repository', description: 'All files' }, { index: 1, label: 'Module' }],
    other: { label: 'None of the above', placeholder: 'Optional notes', allow_empty: true },
    submit_label: 'Next', can_go_back: false, can_chat: true, question_index: 1, question_total: 2,
  };
  const second = {
    ...first, id: 'q2', question: 'Choose device coverage', submit_label: 'Submit', can_go_back: true, question_index: 2,
  };
  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), second);
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', project: 'Approvals', agent: 'claude', interaction: first, question_layout: true });
  await expect(page.getByText('Question 1 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Chat about this' })).toBeHidden();
  await page.getByRole('radio', { name: /Repository/ }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeVisible();
  await expect(page.getByText('Question 2 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Previous' })).toBeVisible();
  const answer = (await commands(page)).find((command) => command.type === 'answer_question');
  expect(answer).toMatchObject({ selected_indices: [0], other_selected: false, protocol: 2 });

  await server(page, 0, {
    type: 'blocked', pane_id: 'w1:p1', project: 'Approvals', agent: 'claude',
    interaction: null, question_layout: false, options: ['Proceed with plan', 'Cancel'],
  });
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Proceed with plan' })).toBeVisible();
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await expect(composer).toBeDisabled();

  const working = { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Approvals', agent: 'claude' }] };
  await server(page, 0, working);
  await server(page, 0, working);
  await expect(composer).toBeEnabled();
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
      pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude',
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
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude', interaction: first, question_layout: true });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await page.getByRole('textbox', { name: 'Other answer' }).focus();
  await expect(page.getByRole('radio', { name: 'Other' })).toBeChecked();

  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude', interaction: second, question_layout: true });
  await expect(page.getByRole('group', { name: 'Choose offline scope' })).toBeVisible();
  const confirmed = {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 1 })),
  };
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude', interaction: confirmed, question_layout: true });

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
    pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude',
    interaction: first, question_layout: true,
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [agent] });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Questions for Questions' })).toBeVisible();
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Refresh terminal' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Attach image' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Arrow keys' })).toBeHidden();
  await expect(page.getByRole('radio', { name: /Backoff plus signals/ })).toBeChecked();

  await page.evaluate(() => (window as any).__relayAutoCommands(false));
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', interaction: second, question_layout: true });
  await server(page, 0, { type: 'command_result', request_id: answer.request_id, ok: true, phase: 'advanced', data: { interaction: second } });
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();

  await page.getByRole('button', { name: /Previous/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'navigate_question').length).toBe(1);
  const navigation = (await commands(page)).find((command) => command.type === 'navigate_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', interaction: first, question_layout: true });
  await server(page, 0, { type: 'command_result', request_id: navigation.request_id, ok: true, phase: 'navigated', data: { interaction: first } });
  await server(page, 0, { type: 'agents', agents: [agent] });
  for (let refresh = 0; refresh < 20; refresh += 1) {
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', interaction: first, question_layout: true });
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
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', project: 'Questions', agent: 'claude', interaction: first, question_layout: true });
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
  await page.getByRole('radio', { name: 'Backoff plus signals' }).click();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');

  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), second);
  await page.getByRole('button', { name: 'Next' }).click();
  await page.evaluate((interaction) => (window as any).__relayNextInteraction(interaction), {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 2 })),
    other: { ...first.other, selected: false, text: '' },
  });
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByRole('radio', { name: 'Backoff plus signals' })).toBeChecked();
  await expect(page.getByRole('radio', { name: 'Other' })).not.toBeChecked();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');
});

test('refreshes agents on return home and preserves terminal behavior', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'codex' }] });
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  const attachImage = page.getByRole('button', { name: 'Attach image' });
  const arrowKeys = page.getByRole('button', { name: 'Arrow keys' });
  const enterKey = page.getByRole('button', { name: 'Enter' });
  await expect(attachImage.locator('svg')).toBeVisible();
  await expect(arrowKeys.locator('svg')).toBeVisible();
  await expect(enterKey).toBeVisible();
  await expect(attachImage).not.toContainText('▧');
  await expect(arrowKeys).not.toContainText('⌨');
  await arrowKeys.click();
  await expect(page.getByRole('button', { name: 'Up' })).toBeVisible();
  await expect(page.locator('.arrow-popup').getByRole('button', { name: 'Enter' })).toHaveCount(0);
  await enterKey.click();
  expect((await commands(page)).find((command) => command.type === 'send_keys')).toMatchObject({
    pane_id: 'w1:p1', keys: ['Enter'],
  });
  const refreshesBeforeBack = (await commands(page)).filter((command) => command.type === 'refresh_agents').length;
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'refresh_agents').length)
    .toBe(refreshesBeforeBack + 1);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'codex' }] });
  await expect(page.getByRole('heading', { name: 'Working' })).toBeVisible();
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: '\u001b[38;5;6mSafe\u001b[0m <img src=x onerror=alert(1)>' });
  await expect(page.getByRole('log')).toContainText('Safe <img src=x onerror=alert(1)>');
  expect(await page.getByRole('log').locator('img').count()).toBe(0);

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.focus();
  await composer.fill('draft prompt');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'newest live frame' });
  await expect(page.getByRole('log')).not.toContainText('newest live frame');
  await composer.evaluate((element) => (element as HTMLTextAreaElement).blur());
  await expect(page.getByRole('log')).toContainText('newest live frame');

  const longFrame = Array.from({ length: 120 }, (_, index) => `terminal line ${index}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: longFrame });
  const terminal = page.getByRole('log');
  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: `${longFrame}\nlatest output` });
  const jumpToLatest = page.getByRole('button', { name: 'Jump to latest output' });
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

  const lastAgent = page.getByRole('button', { name: 'Open Scrollable agent 20 on Fedora' });
  await page.evaluate(() => { document.documentElement.style.minHeight = '300vh'; });
  await lastAgent.scrollIntoViewIfNeeded();
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

test('launches and manages agent lifecycle commands', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await page.getByRole('button', { name: 'Start agent' }).click();
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

  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p2', status: 'working', project: 'relay', cwd: '/home/test/Development/relay', name: 'relay-codex', agent: 'codex' }] });
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();
  await expect.poll(() => page.evaluate(() => location.hash)).toBe('#pane=fedora%3A%3Aw1%3Ap2');
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const manageDialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(manageDialog).toBeVisible();
  await expect(page.getByLabel('New name')).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(manageDialog).toBeHidden();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByLabel('New name').fill('renamed-agent');
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p2', status: 'working', updated_at: 2 });
  await expect(page.getByLabel('New name')).toHaveValue('renamed-agent');
  await page.getByRole('button', { name: 'Rename' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_rename')).toBe(true);

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

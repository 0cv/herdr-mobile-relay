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
    let conversationFixture: { entries: unknown[]; total: number } | null = null;
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
        if (message.type === 'worktree_list') {
          queueMicrotask(() => this.server({
            type: 'command_result',
            action: message.type,
            request_id: message.request_id,
            ok: true,
            phase: 'completed',
            data: {
              source: {
                repo_key: 'repo',
                repo_name: 'project',
                repo_root: '/work/project',
                source_checkout_path: '/work/project',
                source_workspace_id: 'w1',
              },
              worktrees: [
                {
                  path: '/work/worktrees/project/fix-one',
                  branch: 'fix/one',
                  is_bare: false,
                  is_detached: false,
                  is_prunable: false,
                  is_linked_worktree: true,
                  label: 'fix/one',
                  open_workspace_id: null,
                },
              ],
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
          if (conversationFixture) {
            const fixture = conversationFixture;
            queueMicrotask(() => this.server({
              type: 'command_result',
              action: message.type,
              request_id: message.request_id,
              ok: true,
              phase: 'completed',
              data: {
                available: true,
                entries: older ? [] : fixture.entries,
                has_more: false,
                total: fixture.total,
                file_truncated: false,
              },
            }));
            return;
          }
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
                    text: '',
                    tools: [{ id: 'tool-1', name: 'Read', input: 'README.md', output: 'file contents' }],
                  },
                  {
                    id: 'turn-2-progress',
                    timestamp: '2026-08-12T09:00:02Z',
                    role: 'assistant',
                    text: 'intermediate progress update',
                  },
                  {
                    id: 'turn-2-final',
                    timestamp: '2026-08-12T09:00:03Z',
                    role: 'assistant',
                    text: '# middle retained answer',
                  },
                  { id: 'turn-3', timestamp: '2026-08-12T09:00:04Z', role: 'user', text: 'latest retained question' },
                ],
              has_more: !older,
              total: 5,
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
        else if (message.type === 'lease_pane_size') {
          data = typeof message.rows === 'number'
            ? { columns: message.columns, rows: message.rows }
            : { columns: message.columns };
        }
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
      __relayConversationFixture(fixture: { entries: unknown[]; total: number } | null) {
        conversationFixture = fixture;
      },
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

interface ConversationFixture {
  entries: Record<string, unknown>[];
  total: number;
}

async function setConversationFixture(page: Page, fixture: ConversationFixture | null) {
  await page.evaluate((value) => {
    const harnessWindow = window as unknown as {
      __relayConversationFixture(next: ConversationFixture | null): void;
    };
    harnessWindow.__relayConversationFixture(value);
  }, fixture);
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

test('manages workspace modals, grouped worktrees, and drag ordering', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'directory_browser', 'workspace_management', 'workspace_reorder_block', 'worktree_management'],
  });
  const parent = {
    workspace_id: 'w1', number: 1, label: 'Project', pane_count: 1, tab_count: 1,
    cwd: '/work/project',
    worktree: {
      repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
      checkout_path: '/work/project', is_linked_worktree: false,
    },
  };
  const shellOnly = {
    workspace_id: 'w2', number: 2, label: 'Shell Only', pane_count: 1, tab_count: 1,
    cwd: '/work/shell-only',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1,
      tab_label: 'Agent', status: 'working', project: 'fallback-project', agent: 'codex',
    }],
  });

  await expect(page.getByText('Project', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Shell Only', { exact: true }).first()).toBeVisible();
  // Idle-only cards start collapsed in the mixed default; a tap expands them.
  await expect(page.getByText('No agents are running in this workspace.')).toBeHidden();
  await page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary').click();
  await expect(page.getByText('No agents are running in this workspace.')).toBeVisible();

  await page.getByRole('button', { name: 'Manage workspaces' }).click();
  await expect(page.locator('#workspace-manager-title')).toBeVisible();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const workspaceDirectory = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: workspaceDirectory.request_id,
    action: 'list_directories',
    ok: true,
    phase: 'completed',
    data: {
      current: { path: '/work/mobile', label: 'mobile' },
      parent: '/work',
      directories: [],
    },
  });

  await page.getByRole('button', { name: 'Create Workspace' }).click();
  const createDialog = page.locator('#workspace-create-dialog');
  await expect(createDialog.getByRole('heading', { name: 'Create Workspace' })).toBeVisible();
  await createDialog.getByLabel('Label').fill('Phone Workspace');
  await createDialog.getByRole('button', { name: 'Confirm' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_create')).toMatchObject({
    cwd: '/work/mobile',
    label: 'Phone Workspace',
  });
  await expect(createDialog).toBeHidden();

  const phoneWorkspace = {
    workspace_id: 'w3', number: 3, label: 'Phone Workspace', pane_count: 1, tab_count: 1,
    cwd: '/work/mobile',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly, phoneWorkspace] });
  const phoneCard = page.locator('.workspace-management-card').filter({ hasText: 'Phone Workspace' });
  await expect(phoneCard).toContainText('1 tab');
  await expect(phoneCard).toContainText('0 agents');

  const projectCard = page.locator('.workspace-management-card').first();
  await projectCard.getByRole('button', { name: 'Rename' }).click();
  await projectCard.getByLabel('Workspace label').fill('Renamed Project');
  await projectCard.getByRole('button', { name: 'Save' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_rename')).toMatchObject({
    workspace_id: 'w1',
    label: 'Renamed Project',
  });

  await projectCard.getByRole('button', { name: 'Worktrees' }).click();
  const worktreeDialog = page.locator('#worktree-manager-dialog');
  await expect(worktreeDialog.getByRole('heading', { name: 'Project Worktrees' })).toBeVisible();
  await expect(worktreeDialog.getByText('fix/one', { exact: true })).toBeVisible();
  await worktreeDialog.locator('.worktree-list').getByRole('button', { name: 'Open' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'worktree_open')).toMatchObject({
    workspace_id: 'w1',
    path: '/work/worktrees/project/fix-one',
  });

  const linked = {
    workspace_id: 'w4', number: 4, label: 'fix/one', pane_count: 1, tab_count: 1,
    cwd: '/work/worktrees/project/fix-one',
    worktree: {
      repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
      checkout_path: '/work/worktrees/project/fix-one', is_linked_worktree: true,
    },
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, linked, shellOnly, phoneWorkspace] });
  await worktreeDialog.getByLabel('Branch').fill('fix/issue-14');
  await worktreeDialog.getByLabel('Base ref').fill('main');
  await worktreeDialog.getByRole('button', { name: 'Confirm' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'worktree_create')).toMatchObject({
    workspace_id: 'w1',
    branch: 'fix/issue-14',
    base: 'main',
  });
  await worktreeDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(worktreeDialog).toBeHidden();

  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.locator('.workspace-worktree-card')).toContainText('fix/one');
  await page.getByRole('button', { name: 'Manage workspaces' }).click();
  await expect(page.locator('#workspace-manager-title')).toBeVisible();

  const projectSlot = page.locator('.workspace-management-slot').first();
  await expect(projectSlot.locator('.workspace-management-card')).toHaveCount(2);
  await expect(projectSlot.locator('.nested-workspace')).toContainText('fix/one');
  const targetSlot = page.locator('.workspace-management-slot').nth(1);
  const sourceHeader = projectSlot.locator('.workspace-management-card').first().locator('header');
  const sourceBox = await sourceHeader.boundingBox();
  const targetBox = await targetSlot.boundingBox();
  expect(sourceBox).not.toBeNull();
  expect(targetBox).not.toBeNull();
  await page.mouse.move(sourceBox!.x + sourceBox!.width / 2, sourceBox!.y + sourceBox!.height / 2);
  await page.mouse.down();
  await page.waitForTimeout(750);
  await expect(projectSlot).toHaveClass(/workspace-dragging/);
  await page.mouse.move(targetBox!.x + targetBox!.width / 2, targetBox!.y + targetBox!.height * .8, { steps: 4 });
  await page.mouse.up();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_reorder')).toMatchObject({
    workspace_ids: ['w1', 'w4'],
    before_workspace_id: 'w3',
  });

  await phoneCard.getByRole('button', { name: 'Start Agent' }).click();
  await expect(page.getByText('New tab in workspace Phone Workspace.')).toBeVisible();
  await page.getByLabel('Name').fill('phone-workspace-codex');
  await page.getByRole('button', { name: 'Start Agent' }).last().click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    workspace_id: 'w3',
    cwd: '/work/mobile',
  });
  await server(page, 0, {
    type: 'workspaces',
    workspaces: [parent, linked, shellOnly, { ...phoneWorkspace, pane_count: 2, tab_count: 2 }],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w3:p2', workspace_id: 'w3', tab_id: 'w3:t2', tab_number: 2,
      tab_label: 'phone-workspace-codex', status: 'working', project: 'mobile', agent: 'codex',
      cwd: '/work/mobile',
    }],
  });
});

test('groups working agents and synchronizes tab order in both directions', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'tab_reorder'],
  });
  const agents = [
    {
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1, tab_order: 1,
      tab_label: 'First', status: 'working', project: 'mobile', agent: 'codex',
    },
    {
      pane_id: 'w1:p2', workspace_id: 'w1', tab_id: 'w1:t2', tab_number: 2, tab_order: 2,
      tab_label: 'Second', status: 'working', project: 'mobile', agent: 'claude',
    },
  ];
  await server(page, 0, { type: 'agents', agents });

  const working = page.locator('section.workspace-section');
  await expect(working.getByText('mobile', { exact: true }).first()).toBeVisible();
  await expect(working.locator('summary strong')).toHaveCount(1);
  await expect(working.locator('.workspace-tab-header h3')).toHaveText(['First', 'Second']);

  // Long-press the first tab's agent card, then drag below the second tab.
  const source = working.locator('.workspace-tab').filter({ hasText: 'First' }).getByRole('button', { name: /^Open mobile/ });
  const target = working.locator('.workspace-tab').filter({ hasText: 'Second' });
  const sourceBox = await source.boundingBox();
  const targetBox = await target.boundingBox();
  expect(sourceBox).not.toBeNull();
  expect(targetBox).not.toBeNull();
  await page.mouse.move(sourceBox!.x + sourceBox!.width / 2, sourceBox!.y + sourceBox!.height / 2);
  await page.mouse.down();
  await page.waitForTimeout(750);
  await expect(working.locator('.workspace-tab.tab-dragging')).toHaveCount(1);
  await page.mouse.move(targetBox!.x + targetBox!.width / 2, targetBox!.y + targetBox!.height * .8, { steps: 4 });
  await page.mouse.up();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'tab_reorder')).toMatchObject({
    pane_id: 'w1:p1',
    insert_index: 2,
  });

  // Desktop-side move arrives as refreshed visual positions; numbers stay.
  await server(page, 0, {
    type: 'agents',
    agents: [
      { ...agents[0], tab_order: 2 },
      { ...agents[1], tab_order: 1 },
    ],
  });
  await expect(working.locator('.workspace-tab-header h3')).toHaveText(['Second', 'First']);

  // The long press must not have opened the agent terminal.
  await expect(page.locator('section.workspace-section')).toBeVisible();
});

test('defaults to the mixed workspace layout and separates state sections on demand', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification'] });
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', workspace_id: 'w1', status: 'done', project: 'alpha', agent: 'codex' },
      { pane_id: 'w1:p2', workspace_id: 'w1', status: 'idle', project: 'alpha', agent: 'claude' },
      { pane_id: 'w2:p1', workspace_id: 'w2', status: 'working', project: 'beta', agent: 'codex' },
      { pane_id: 'w2:p2', workspace_id: 'w2', status: 'idle', project: 'beta', agent: 'claude' },
      { pane_id: 'w4:p1', workspace_id: 'w4', status: 'idle', project: 'delta', agent: 'codex' },
      {
        pane_id: 'w3:p1', workspace_id: 'w3', status: 'blocked', attention_kind: 'approval',
        project: 'gamma', agent: 'codex', prompt: 'Approve the plan?', options: ['Yes', 'No'],
      },
    ],
  });

  // Default: one headingless mixed list, one card per workspace with a state
  // dot, blocked stays on top.
  await expect(page.locator('section.agent-section').first()).toContainText('Needs input');
  await expect(page.getByRole('region', { name: 'Workspaces' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Done' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Working' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeHidden();
  const card = (project: string) => page.locator('.workspace-card').filter({ hasText: project });
  await expect(card('alpha').getByRole('img', { name: 'Has a done session' })).toBeVisible();
  await expect(card('beta').getByRole('img', { name: 'Has a working session' })).toBeVisible();
  await expect(card('delta').getByRole('img', { name: 'All sessions idle' })).toBeVisible();
  await expect(page.locator('.workspace-card summary strong')).toHaveText(['alpha', 'beta', 'delta']);
  // Active workspaces start expanded; idle-only cards stay collapsed.
  await expect(card('alpha')).toHaveAttribute('open', '');
  await expect(card('beta')).toHaveAttribute('open', '');
  await expect(card('delta')).not.toHaveAttribute('open', '');

  // By State: sections separated by state, done first after the input queue.
  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'By State' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  const done = page.locator('.done-section');
  await expect(done.getByRole('heading', { name: 'Done' })).toBeVisible();
  await expect(done.locator('summary strong')).toHaveText(['alpha']);
  await expect(done.locator('.workspace-done-count')).toHaveText('1 done');
  const stateWorking = page.locator('.working-section');
  await expect(stateWorking.locator('summary strong')).toHaveText(['beta']);
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeHidden();
  await expect(page.locator('section.agent-section').first()).toContainText('Needs input');

  // Back to the mixed default.
  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Mixed' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('heading', { name: 'Done' })).toBeHidden();
  await expect(page.getByRole('region', { name: 'Workspaces' })).toBeVisible();
});

test('keeps activity cards inside the page and confirms permanent deletion', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'activity_history',
    activities: [
      {
        id: 'activity-working', timestamp: Date.now(), kind: 'working', summary: 'omp working',
        pane_id: 'w1:p1', project: 'herdr-mobile-relay', agent: 'omp', status: 'working',
      },
      {
        id: 'activity-1', timestamp: Date.now(), kind: 'finished', summary: 'codex completed',
        pane_id: 'w1:p1', project: 'herdr-mobile-relay', agent: 'codex', status: 'completed',
      },
    ],
  });

  await page.getByRole('button', { name: 'Activity history' }).click();
  const activity = page.getByRole('button', { name: /codex completed/ });
  await expect(page.getByRole('heading', { name: 'Last 24 hours' })).toBeVisible();
  await expect(page.locator('.activity-summary-metrics > div').filter({ hasText: 'Completed' })).toContainText('1');
  await expect(activity).toBeVisible();
  await expect(page.getByRole('button', { name: /omp working/ })).toHaveCount(0);
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
  const fullResponse = Array.from({ length: 16 }, (_, index) => `Response line ${index + 1}`).join('\n');
  await server(page, 0, {
    type: 'activity_history',
    activities: [{
      id: 'activity-typography',
      kind: 'finished',
      timestamp: Date.now(),
      summary: 'omp completed',
      agent: 'omp',
      status: 'completed',
      extract: `\ue0b0 status\n${fullResponse}`,
    }],
  });

  await page.getByRole('button', { name: 'Activity history' }).click();
  await page.getByRole('button', { name: /omp completed/ }).click();
  const extract = page.getByRole('region', { name: 'Captured response' }).locator('pre');
  await expect(extract).toContainText('Response line 16');

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

  // The session behind this dialog stays connected while locked, so the gate
  // has to hide the page rather than dim it: a translucent scrim would leave
  // live agent names and status changes legible before verification.
  const backdrop = await unlockDialog.evaluate(
    (element) => getComputedStyle(element, '::backdrop').backgroundColor,
  );
  const alpha = /^rgba?\((?:[^,]+,){3}\s*([\d.]+)\s*\)$/.exec(backdrop)?.[1];
  expect({ backdrop, alpha: alpha === undefined ? 1 : Number(alpha) }).toEqual({ backdrop, alpha: 1 });

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
  // The pre-reload URL is already "/", so polling the URL alone can be
  // satisfied before the reload commits, leaving later evaluations racing the
  // context swap. Mark this document: only the reloaded one lacks the mark.
  await page.evaluate(() => {
    (window as unknown as { __herdrPreReload?: boolean }).__herdrPreReload = true;
  });
  const reloadRequest = page.waitForRequest((request) =>
    new URL(request.url()).searchParams.has('herdr_reload'));
  await dialog.getByRole('button', { name: 'Load Update', exact: true }).click();

  const reloadUrl = new URL((await reloadRequest).url());
  expect(reloadUrl.pathname).toBe('/index.html');
  expect(reloadUrl.searchParams.get('herdr_reload'))
    .toMatch(new RegExp(`^${APP_RELEASE.replaceAll('.', '\\.')}-\\d+$`));
  await page.waitForFunction(() =>
    !(window as unknown as { __herdrPreReload?: boolean }).__herdrPreReload);
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
  const terminal = page.getByRole('log');
  const cachedScreen = await terminal.locator('.term-screen').innerHTML();
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'read_pane')).toMatchObject({
    type: 'read_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  await server(page, 0, {
    type: 'pane_unchanged',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'watch_pane')).toMatchObject({
    type: 'watch_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
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
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Resume app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: 'cached terminal output', content_fingerprint: 'resume-cache-1',
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('cached terminal output');
  const cachedScreen = await terminal.locator('.term-screen').innerHTML();

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.waitForTimeout(5_100);
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Agent unavailable' })).toBeHidden();
  await expect(page.getByRole('log')).toContainText('cached terminal output');
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();

  // A half-open socket answers nothing — not even the refocus width lease.
  // Without this the harness acks the lease, which reads as live traffic and
  // defeats the resume probe this test exists to exercise.
  await setAutoCommands(page, false);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(() => socketCount(page)).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 1, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'read_pane')).toBe(true);
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  await expect(terminal).not.toContainText('Resizing terminal…');
  await server(page, 1, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: 'fresh output after resume', content_fingerprint: 'resume-cache-2',
  });
  await expect(page.getByRole('log')).toContainText('fresh output after resume');
  await server(page, 1, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
});

test('replaces a half-open socket when the browser reports a network handoff', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'connection', {
      configurable: true,
      value: new EventTarget(),
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const refreshesBefore = (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'refresh_agents').length;

  await page.evaluate(() => {
    (navigator as Navigator & { connection: EventTarget }).connection.dispatchEvent(new Event('change'));
  });

  await expect.poll(async () =>
    (await commandsForSocket(page, 0))
      .filter((command) => command.type === 'refresh_agents').length).toBe(refreshesBefore + 1);
  // The old socket deliberately never answers the probe. The foreground
  // health deadline replaces it without waiting for WebSocket/TCP timeouts.
  await expect.poll(() => socketCount(page)).toBe(2);
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
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'terminal history before resize',
  });

  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_layout'))).toBeNull();
});

test('keeps a wide pane readable while its size lease is still pending', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'claude' }],
  });
  // The relay advertises the lease capability unconditionally, so the pane is
  // shown before any width is granted. Leaving the lease unanswered pins that
  // state: wrapping then had no cap and broke every row mid-word.
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  const wideRow = `${'the pane is far wider than the phone viewport '.repeat(3)}and keeps going`;
  // A box border padded out to the pane's width. Preserved spaces may hang
  // past the edge instead of wrapping, which WebKit reports as scrollable
  // width, handing the reader a pane that pans sideways.
  const paddedRow = `╰─${' '.repeat(150)}─╯`;
  // A bordered row of cells: box-drawing renders as a fixed-width cell grid
  // with no wrap opportunity between cells, so past the phone's width it has
  // to fall back to text that can wrap.
  const gridRow = `│ ${'aligned cell '.repeat(12)}│`;
  // Long unbroken tokens are routine in agent output.
  const tokenRow = `https://example.test/${'x'.repeat(300)}`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: ['first row', wideRow, paddedRow, gridRow, tokenRow, 'last row'].join('\n'),
  });

  const terminal = page.getByRole('log');
  const lines = terminal.locator('.ansi-line');
  await expect(lines).toHaveCount(6);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBeGreaterThan(0);

  // The pane is wider than the phone and no lease has landed, so alignment is
  // impossible either way. Wrapping at the container keeps the text readable:
  // sideways scrolling stranded the reader on line tails after every refresh.
  const geometry = await terminal.evaluate((element) => {
    const rows = Array.from(element.querySelectorAll<HTMLElement>('.ansi-line'));
    const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight);
    const wide = rows[1];
    // A word split across lines reports more than one client rect. A word too
    // long to fit a line has to break; one that would have fit must not.
    let brokenWords = 0;
    const walker = document.createTreeWalker(wide, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent || '';
      const pattern = /\S+/g;
      for (let match = pattern.exec(text); match; match = pattern.exec(text)) {
        const range = document.createRange();
        range.setStart(node, match.index);
        range.setEnd(node, match.index + match[0].length);
        const rects = Array.from(range.getClientRects());
        const advance = rects.reduce((total, rect) => total + rect.width, 0);
        if (rects.length > 1 && advance <= element.clientWidth) brokenWords += 1;
      }
    }
    return {
      lineHeight,
      wideHeight: wide.getBoundingClientRect().height,
      narrowHeight: rows[0].getBoundingClientRect().height,
      brokenWords,
      scrollWidth: element.scrollWidth,
      clientWidth: element.clientWidth,
    };
  });
  expect(geometry.lineHeight).toBeGreaterThan(0);
  expect(geometry.narrowHeight).toBeLessThan(geometry.lineHeight * 1.5);
  expect(geometry.wideHeight).toBeGreaterThan(geometry.lineHeight * 1.5);
  expect(geometry.brokenWords).toBe(0);
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth + 1);
});

test('estimates wrapped row heights while a lease is pending', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  // Rows that wrap to several lines each. The virtualizer sizes unmounted rows
  // from an estimate, so an estimate of one line per row understates the whole
  // log and the scroll geometry lands the reader in the wrong place.
  const content = Array.from({ length: 400 }, (_, index) =>
    `row ${String(index + 1).padStart(4, '0')} ${'wide content '.repeat(14)}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content });

  const terminal = page.getByRole('log');
  await expect(terminal.locator('.ansi-line').first()).toBeVisible();
  const estimated = await terminal.evaluate((element) => element.scrollHeight);
  // Walking the log mounts and measures every row, so the height afterwards is
  // the truth the estimate was predicting.
  const step = await terminal.evaluate((element) => Math.max(1, element.clientHeight - 20));
  for (let top = 0; top <= estimated; top += step) {
    await terminal.evaluate((element, offset) => {
      element.scrollTop = offset;
      element.dispatchEvent(new Event('scroll'));
    }, top);
    await page.waitForTimeout(50);
  }
  await expect.poll(async () => {
    const measured = await terminal.evaluate((element) => element.scrollHeight);
    return Math.abs(measured - estimated) / measured < 0.1;
  }).toBe(true);
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

test('keeps rapid streaming frames pinned to the latest terminal row', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Streaming', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Streaming on Fedora' }).click();
  const initial = Array.from({ length: 200 }, (_, index) => `stream history ${index + 1}`).join('\n');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: initial,
  });

  const terminal = page.getByRole('log');
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  let maxBottomDistance = 0;
  for (let frame = 1; frame <= 24; frame += 1) {
    const content = Array.from(
      { length: 200 },
      (_, index) => `stream history ${frame + index + 1}`,
    );
    content.push(`streaming response chunk ${frame}`);
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'plain',
      content: content.join('\n'),
    });
    await page.waitForTimeout(8);
    maxBottomDistance = Math.max(
      maxBottomDistance,
      await terminal.evaluate((element) =>
        element.scrollHeight - element.scrollTop - element.clientHeight),
    );
  }

  expect(maxBottomDistance).toBeLessThan(48);
  await expect(terminal).toContainText('streaming response chunk 24');
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();
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
  // The control is an icon, so its idle state shows in the accessible name.
  await expect(copyButton).toBeEnabled();
  await expect(copyButton).toHaveAccessibleName('Copy');
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
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'resize anchor baseline',
  });
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
  await expect(terminal).toContainText(anchor.text);
  await expect(terminal).not.toContainText('Resizing terminal…');
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
  await expect(terminal).toContainText(anchor.text);
  await expect(terminal).not.toContainText('Resizing terminal…');
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
  // Height leasing is opt-in: resizing the shared pane's height strands
  // stale status-bar copies of inline agents in the scrollback.
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_height_lease', 'true'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resizable app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resizable app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'resizable terminal baseline',
  });
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
  expect(acquire.rows).toEqual(expect.any(Number));
  expect(Number(acquire.rows)).toBeGreaterThanOrEqual(10);
  expect(Number(acquire.rows)).toBeLessThanOrEqual(120);
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
  // Issue #11: the wrap cap must be the probed cell advance times the leased
  // column count, emitted in px — never derived from ch. Measured: with the
  // symbols face loaded, Playwright WebKit 26.5 and Chromium both resolve
  // 1ch to the glyph advance (ratio 0.9997) while shipped Safari 26.1
  // resolves it to the 0.5em spec fallback (ratio 0.8087), so the defect is
  // NOT reproducible under any CI engine and the arithmetic check alone
  // would pass a ch-derived cap here. The serialization checks are the part
  // that fails on a revert; do not replace them with a font-load probe.
  const capGeometry = await terminal.evaluate((element) => {
    const probe = element.querySelector<HTMLElement>(':scope > span[aria-hidden]');
    const line = element.querySelector<HTMLElement>('.ansi-line');
    return {
      cellWidth: probe ? probe.getBoundingClientRect().width / 10 : 0,
      capWidth: line ? Number.parseFloat(getComputedStyle(line).maxWidth) : 0,
      style: element.getAttribute('style') || '',
    };
  });
  expect(capGeometry.cellWidth).toBeGreaterThan(0);
  expect(Math.abs(capGeometry.capWidth - capGeometry.cellWidth * Number(acquire.columns)))
    .toBeLessThan(0.5);
  const cellVar = capGeometry.style.match(/--terminal-cell-width: (\d+(?:\.\d+)?)px/);
  expect(cellVar).not.toBeNull();
  expect(Number(cellVar![1])).toBeCloseTo(capGeometry.cellWidth, 1);
  expect(capGeometry.style).toMatch(/--terminal-width: \d+(?:\.\d+)?px/);
  expect(capGeometry.style).not.toMatch(/\dch\b/);

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
  await expect.poll(async () => terminal.evaluate((element) => {
    const lastRow = element.querySelector<HTMLElement>('[data-terminal-row="119"]');
    if (!lastRow) return Number.POSITIVE_INFINITY;
    return element.getBoundingClientRect().bottom - lastRow.getBoundingClientRect().bottom;
  })).toBeLessThan(1);
  const stableBottomScreen = await terminal.locator('.term-screen').innerHTML();
  await terminal.evaluate((element) => {
    const bottom = Math.max(0, element.scrollHeight - element.clientHeight);
    element.scrollTop = Math.max(0, bottom - 12);
    element.dispatchEvent(new Event('scroll'));
    element.scrollTop = Math.max(0, bottom - 6);
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(1);
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', stableBottomScreen);

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
  // Reopening keeps the phone's cached pane painted while the relay restores
  // the shared width and checks for a newer stable frame.
  await expect(terminal).toContainText('stored resize history row 120');
  await expect(terminal).not.toContainText('Resizing terminal…');

  const reentryLease = (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').at(-1)!;
  await server(page, 0, {
    type: 'command_result',
    action: 'lease_pane_size',
    request_id: reentryLease.request_id,
    ok: true,
    data: { columns: reentryLease.columns, rows: reentryLease.rows },
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(readCountBeforeReentry);
  await page.waitForTimeout(300);
  // A frame the relay read while the agent was still repainting is transient:
  // it must not replace the stable screen.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: Array.from(
      { length: 46 },
      (_, index) => `transient viewport row ${index + 1}`,
    ).join('\n'),
    viewport_only: true,
    viewport_rows: 46,
    resize_settling: true,
    content_fingerprint: 'resize-settling',
  });
  await expect(terminal).not.toContainText('transient viewport row');
  await expect(terminal).toContainText('stored resize history row 120');
  const readsBeforeSettledUpdate = (await commands(page))
    .filter((command) => command.type === 'read_pane').length;
  // The content can stop changing before the relay's resize settle window
  // closes. Its metadata-only delta must trigger a stable frame resync.
  await server(page, 0, {
    type: 'pane_delta',
    pane_id: 'w1:p1',
    format: 'ansi',
    base_fingerprint: 'resize-settling',
    content_fingerprint: 'resize-settling',
    segments: null,
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length)
    .toBeGreaterThan(readsBeforeSettledUpdate);
  await page.waitForTimeout(300);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: storedHistory,
    content_fingerprint: 'resize-settled',
  });
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');
  await expect(terminal).toContainText('stored resize history row 120');

  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'release_pane_size').length).toBe(2);
});

test('paints again when the relay keeps a pane settling after a resize', async ({ page }) => {
  // Frames read while the agent repaints are suppressed so a transient screen
  // never replaces the stable one. That wait must be bounded: a shared session
  // whose desktop client keeps fighting the leased size re-arms the relay's
  // settling window on every read, and an unbounded wait freezes the phone on
  // its last painted frame for as long as that lasts.
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_height_lease', 'true'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Resizable app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Resizable app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'stable row before resize',
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('stable row before resize');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);

  // A width change captures the painted frame as the settling baseline.
  const viewport = page.viewportSize()!;
  await page.setViewportSize({ width: viewport.width + 120, height: viewport.height });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(2);

  // The relay now reports every frame as settling for longer than the pane
  // takes to repaint. The newest screen must still reach the phone.
  for (let index = 1; index <= 12; index += 1) {
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'ansi',
      content: `live row ${index}`,
      resize_settling: true,
    });
    await page.waitForTimeout(500);
  }
  await expect(terminal).toContainText('live row 12');
});

test('paints again when a re-entry lease never lands', async ({ page }) => {
  // The same wait also covers a pane with no lease yet. Reopening a pane asks
  // for a fresh lease while the cached frame is painted, so a relay that never
  // answers must not strand the phone on that cached frame.
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
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'cached row before re-entry',
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('cached row before re-entry');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);

  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'release_pane_size').length).toBe(1);
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Resizable app on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(2);

  for (let index = 1; index <= 12; index += 1) {
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'ansi',
      content: `unleased row ${index}`,
    });
    await page.waitForTimeout(500);
  }
  await expect(terminal).toContainText('unleased row 12');
});

test('does not lease rows unless the height setting is on', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands'],
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
  // The relay advertises row support, but the default-off setting keeps the
  // lease width-only so the shared pane's height is never touched.
  expect(acquire.columns).toEqual(expect.any(Number));
  expect(acquire).not.toHaveProperty('rows');
});

test('keeps renewing the pane lease briefly while hidden and re-leases on return', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_size_lease', 'pane_realtime_delta'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Sleeping app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Sleeping app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'before sleep',
    content_fingerprint: 'sleep-1',
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  // Desktop Safari reports an occluded window as hidden, so a hidden page
  // keeps renewing within the grace window instead of lapsing the lease —
  // and resizing the shared pane — on every app switch. The 10s renewal
  // must therefore still fire. Grace expiry is unit-tested on the pure
  // policy helper; five minutes cannot elapse here.
  await page.waitForTimeout(11_000);
  expect(((await commands(page))
    .filter((command) => command.type === 'lease_pane_size')).length).toBeGreaterThanOrEqual(2);
  expect((await commands(page))
    .filter((command) => command.type === 'release_pane_size')).toHaveLength(0);

  // Refocus re-leases at once — the lease may have lapsed and the pane may be
  // back at the desktop width — and re-reads the pane without waiting for the
  // resync interval.
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBeGreaterThanOrEqual(3);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThanOrEqual(2);
});

const resizeCapabilities = [
  'attention_classification', 'pane_size_lease', 'slash_commands',
];

async function openResizePane(page: Page, project: string) {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: resizeCapabilities });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project, agent: 'omp' }],
  });
  await page.getByRole('button', { name: `Open ${project} on Fedora` }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(0);
}

test('renders the raw pane window as terminal history', async ({ page }) => {
  await openResizePane(page, 'Raw window');
  const windowRow = (index: number) => `raw window row ${String(index).padStart(3, '0')}`;
  const windowRows = Array.from({ length: 120 }, (_, index) => windowRow(index + 1));
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: windowRows.join('\n'),
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: windowRows.join('\n'),
    viewport_only: true,
    viewport_rows: 20,
  });

  const terminal = page.getByRole('log');
  // The raw window renders exactly as read: scrolled-back rows included.
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '120');
  await expect(terminal).toContainText(windowRow(120));

  // A window shorter than the requested history carries the truncation flag.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: windowRows.concat('raw window row 121').join('\n'),
    viewport_only: true,
    viewport_rows: 20,
    truncated: true,
  });
  await expect(page.getByRole('status').filter({ hasText: 'Older terminal history is not shown' }))
    .toBeVisible();
});

test('keeps the reader anchored while streamed output grows and unsticks on scroll-up', async ({ page }) => {
  // Rows 401-700 are three-character rows: too short for anchor text
  // matching, so only correct index math can keep the reader anchored there.
  const historyRow = (index: number) => (index > 400 && index <= 700
    ? String(index)
    : `anchored history row ${String(index).padStart(4, '0')}`);
  const historyRows = Array.from({ length: 1_000 }, (_, index) => historyRow(index + 1));
  await openResizePane(page, 'Anchored stream');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: historyRows[0],
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  await page.waitForTimeout(300);

  const streamedRows = Array.from({ length: 100 }, (_, index) => `anchored burst row ${index + 1}`);
  const stream = historyRows.concat(streamedRows);
  // Herdr always serves the newest 1,000-row window: streamed output appends
  // at the bottom and crops the oldest rows off the front.
  const frame = (streamed: number) => server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: stream
      .slice(Math.max(0, historyRows.length + streamed - 1_000), historyRows.length + streamed)
      .join('\n'),
    viewport_only: true,
    viewport_rows: 46,
  });
  await frame(0);
  const terminal = page.getByRole('log');
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '1000');

  // Pinned at the bottom: appended output is shown immediately.
  await frame(54);
  await expect(terminal).toContainText('anchored burst row 54');
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();

  // Scrolling up leaves sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeVisible();
  const firstVisibleRow = () => terminal.evaluate((element) => {
    const top = element.getBoundingClientRect().top;
    const row = [...element.querySelectorAll('[data-terminal-row]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > top + 1);
    return {
      text: row?.textContent || '',
      offset: row ? Math.round(row.getBoundingClientRect().top - top) : 0,
    };
  });
  // The virtual window re-renders on the next animation frame.
  await expect.poll(async () => (await firstVisibleRow()).offset).toBeLessThanOrEqual(1);
  const before = await firstVisibleRow();
  expect(before.text.trim()).toMatch(/^\d{3}$/);

  // More output arrives and the served window crops rows from the front:
  // the reader's position must not move and sticky mode must stay off.
  await frame(100);
  await expect.poll(async () => (await firstVisibleRow()).text).toBe(before.text);
  const after = await firstVisibleRow();
  // Same row, and within a row of the same position: cropped rows shift every
  // index, so only exact index math keeps this stable.
  const rowHeight = await terminal.evaluate((element) => {
    const row = element.querySelector<HTMLElement>('[data-terminal-row]');
    return row ? row.getBoundingClientRect().height : 20;
  });
  expect(Math.abs(after.offset - before.offset)).toBeLessThan(rowHeight);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeVisible();

  // Returning to the bottom re-engages sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();
});

test('wraps stale wide grids but preserves current grids in Resize Session', async ({ page }) => {
  const staleTable = [
    `┌${'─'.repeat(180)}┐`,
    `│ ${'Lookback | Sharpe | Max DD | 2x-cost Sharpe'.padEnd(178)}│`,
    `└${'─'.repeat(180)}┘`,
  ];
  const desktopHistory = Array.from({ length: 46 }, (_, index) => `desktop history row ${index + 1}`);
  await openResizePane(page, 'Qoder grid');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: desktopHistory.join('\n'),
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'lease_pane_size').length).toBe(1);
  const lease = (await commands(page))
    .find((command) => command.type === 'lease_pane_size')!;
  const columns = Number(lease.columns);
  const terminal = page.getByRole('log');
  await page.waitForTimeout(300);

  const currentTable = [
    `┌${'─'.repeat(columns - 2)}┐`,
    `│ ${'Current grid'.padEnd(columns - 4)}│`,
    `└${'─'.repeat(columns - 2)}┘`,
  ];
  const currentViewport = Array.from({ length: 43 }, (_, index) => `current viewport row ${index + 1}`)
    .concat(currentTable);
  // The stale table was captured at 188 columns before the lease and is still
  // in the window Herdr serves, above the current-width screen.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: staleTable.concat(desktopHistory, currentViewport).join('\n'),
    viewport_only: true,
    viewport_rows: currentViewport.length,
  });
  await expect(terminal).toContainText('Current grid');
  await expect(terminal.locator('.terminal-grid-line')).toHaveCount(3);
  const lineHeights = await terminal.locator('.terminal-grid-line').evaluateAll((lines) => {
    const lineHeight = Number.parseFloat(getComputedStyle(lines[0]).lineHeight);
    return {
      lineHeight,
      heights: lines.map((line) => line.getBoundingClientRect().height),
    };
  });
  expect(Math.max(...lineHeights.heights)).toBeLessThan(lineHeights.lineHeight * 1.2);
  // The desktop-width rows sit above the screen; the stale grid enters the
  // DOM once it is scrolled into view and wraps instead of overflowing.
  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(terminal).toContainText('Lookback | Sharpe | Max DD | 2x-cost Sharpe');
  const wrappedGeometry = await terminal.evaluate((element) => ({
    clientWidth: element.clientWidth,
    scrollWidth: element.scrollWidth,
  }));
  expect(wrappedGeometry.scrollWidth).toBeLessThanOrEqual(wrappedGeometry.clientWidth);
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

test('reads and replies from native conversation history', async ({ page }) => {
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
  await expect(page.getByText('5 recorded messages')).toBeVisible();
  await expect(page.getByText(/session log is larger than 16 MB/)).toBeVisible();
  await page.getByRole('button', { name: 'Copy History app message as Markdown' }).click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedConversation')))
    .toBe('# middle retained answer');
  // Superseded prose collapses out of the compact view; a tool call is a
  // distinct event and stays, so the Read call is visible in both modes.
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('intermediate progress update')).toBeVisible();
  await expect(page.getByText('Read', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('intermediate progress update')).toBeHidden();

  const composer = page.getByRole('textbox', { name: 'Prompt' });
  await composer.fill('Review the attached screen');
  const imageInput = page.locator('.conversation-composer input[type=file]');
  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  const attachedPrompt = [
    'Review the attached screen',
    'Image: /home/test/.cache/herdr-mobile-relay/uploads/shot.png',
    '',
  ].join('\n');
  await expect(composer).toHaveValue(attachedPrompt);
  await expect(page.getByText('Image attached: shot.png')).toBeVisible();
  const historyReadsBeforeSend = (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length;
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === attachedPrompt.replace(/\n$/, '')
  ))).toMatchObject({ pane_id: 'w1:p1' });
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Image attached: shot.png')).toBeHidden();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length).toBeGreaterThan(historyReadsBeforeSend);

  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  await expect(page.getByText('Image attached: shot.png')).toBeVisible();
  await page.getByRole('button', { name: 'Clear prompt text' }).click();
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Image attached: shot.png')).toBeHidden();

  await setAutoCommands(page, false);
  await composer.fill('restore this known failure');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => (
    command.type === 'submit_prompt' && command.text === 'restore this known failure'
  ))).toBe(true);
  const failedCommand = (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === 'restore this known failure'
  ))!;
  await server(page, 0, {
    type: 'command_result',
    action: 'submit_prompt',
    request_id: failedCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Relay rejected prompt',
  });
  await expect(composer).toHaveValue('restore this known failure');

  await composer.fill('do not send this twice');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => (
    command.type === 'submit_prompt' && command.text === 'do not send this twice'
  ))).toBe(true);
  const ambiguousCommand = (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === 'do not send this twice'
  ))!;
  await server(page, 0, {
    type: 'command_result',
    action: 'submit_prompt',
    request_id: ambiguousCommand.request_id,
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed',
  });
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Command may have executed Check the terminal before sending again.')).toBeVisible();
  await setAutoCommands(page, true);

  await page.getByRole('button', { name: 'Load older turns' }).click();
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'get_conversation_history' && command.before === 'turn-2'
  ))).toMatchObject({ pane_id: 'w1:p1', before: 'turn-2' });

  const search = page.getByRole('searchbox', { name: 'Search displayed conversation' });
  await search.fill('first retained');
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeHidden();
  await server(page, 0, {
    type: 'agent_update',
    pane_id: 'w1:p1',
    status: 'blocked',
    attention_kind: 'approval',
    options: ['Approve once', 'Reject'],
    updated_at: 2,
  });
  await expect(composer).toBeDisabled();
  await expect(page.getByText('Switch to Terminal to handle the pending agent interaction.')).toBeVisible();

  await page.getByRole('button', { name: 'Terminal view' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('textbox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('button', { name: 'Open History app on Fedora' })).toBeVisible();
});

test('keeps tool-only agent turns and decodes their arguments', async ({ page }) => {
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
      project: 'Tool history',
      agent: 'claude',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });
  // Claude Code records one text-less assistant turn per tool call and
  // serialises the arguments as JSON, which is the shape the compact view used
  // to discard entirely.
  const output = Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n');
  await setConversationFixture(page, {
    entries: [
      { id: 'turn-1', timestamp: '2026-08-12T09:00:00Z', role: 'user', text: 'run the probe' },
      {
        id: 'turn-2',
        timestamp: '2026-08-12T09:00:01Z',
        role: 'assistant',
        text: '',
        tools: [{
          id: 'tool-1',
          name: 'Bash',
          input: JSON.stringify({
            command: 'python3 /tmp/band-sample.py',
            description: 'List the band',
            timeout: 300000,
          }),
          output,
        }],
      },
      { id: 'turn-3', timestamp: '2026-08-12T09:00:02Z', role: 'assistant', text: 'Probe finished.' },
    ],
    total: 3,
  });

  await page.getByRole('button', { name: 'Open Tool history on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Probe finished.')).toBeVisible();

  // The compact view must keep the tool turn: it is the only record of the work.
  const card = page.getByLabel(/^Conversation with/).locator('details').filter({ hasText: 'Bash' });
  await expect(card).toBeVisible();
  await card.locator('summary').click();

  const panels = card.locator('pre');
  await expect(panels.first()).toContainText('command: python3 /tmp/band-sample.py');
  await expect(panels.first()).toContainText('timeout: 300000');
  await expect(panels.first()).not.toContainText('{"command"');

  await expect(panels.nth(1)).not.toContainText('line-199');
  await card.getByRole('button', { name: 'Show all 200 lines' }).click();
  await expect(panels.nth(1)).toContainText('line-199');
  await card.getByRole('button', { name: 'Show less' }).click();
  await expect(panels.nth(1)).not.toContainText('line-199');
});

test('opens a long conversation at its newest turn and holds the pin', async ({ page }) => {
  // The view polls for new turns every five seconds. Shortening that interval
  // keeps the streaming assertions below deterministic instead of sleeping
  // through real intervals, exactly as boot() does for reconnect timers.
  await page.addInitScript(() => {
    const nativeSetInterval = window.setInterval.bind(window);
    window.setInterval = ((handler: TimerHandler, timeout?: number, ...args: unknown[]) =>
      nativeSetInterval(handler, timeout === 5000 ? 60 : timeout, ...args)) as typeof window.setInterval;
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
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
      project: 'Long conversation',
      agent: 'claude',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });

  // Tables and fenced code are the reported trigger: they lay out taller than
  // the frame that first measures the list, so a scroll written once against
  // an early scrollHeight lands short of the end (issue #12).
  const heavyTurn = (index: number) => [
    `### Turn ${index} heading`,
    '',
    `Prose that wraps several times on a phone: ${'detail '.repeat(24).trim()}.`,
    '',
    '| Field | Value |',
    '| --- | --- |',
    `| index | ${index} |`,
    `| note | ${'wide cell content '.repeat(6).trim()} |`,
    '',
    '```ts',
    ...Array.from({ length: 6 }, (_, line) => `const value${line} = compute(${index}, ${line});`),
    '```',
  ].join('\n');
  const entries = Array.from({ length: 60 }, (_, index) => ({
    id: `turn-${index + 1}`,
    timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: index % 2 ? heavyTurn(index + 1) : `question ${index + 1}`,
  }));
  await setConversationFixture(page, { entries, total: entries.length });

  await page.getByRole('button', { name: 'Open Long conversation on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  const bottomGap = () => list.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight);

  await expect(page.getByText('Turn 60 heading')).toBeVisible();
  // A pin assertion is only meaningful while the transcript really overflows.
  expect(await list.evaluate((element) => element.scrollHeight > element.clientHeight * 3)).toBe(true);
  await expect.poll(bottomGap).toBeLessThan(2);

  // Turns arriving while the reader sits at the end keep the end in view.
  const streamed = [...entries, {
    id: 'turn-61', timestamp: '2026-08-12T10:00:00Z', role: 'user', text: 'newest streamed question',
  }];
  await setConversationFixture(page, { entries: streamed, total: streamed.length });
  await expect(page.getByText('newest streamed question')).toBeVisible();
  await expect.poll(bottomGap).toBeLessThan(2);

  // Reading history wins over the pin: later turns must not yank the view.
  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  const later = [...streamed, {
    id: 'turn-62', timestamp: '2026-08-12T10:01:00Z', role: 'user', text: 'later streamed question',
  }];
  await setConversationFixture(page, { entries: later, total: later.length });
  await expect(page.getByText('later streamed question')).toBeVisible();
  expect(await list.evaluate((element) => element.scrollTop)).toBeLessThan(48);
  expect(await bottomGap()).toBeGreaterThan(200);

  // Scrolling back to the end restores the pin.
  await list.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  const final = [...later, {
    id: 'turn-63', timestamp: '2026-08-12T10:02:00Z', role: 'user', text: 'final streamed question',
  }];
  await setConversationFixture(page, { entries: final, total: final.length });
  await expect(page.getByText('final streamed question')).toBeVisible();
  await expect.poll(bottomGap).toBeLessThan(2);
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
  await expect(page.getByText('Type to find', { exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Previous match' }).locator('svg.find-action-symbol')).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Next match' }).locator('svg.find-action-symbol')).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Close find' }).locator('svg.find-action-symbol')).toHaveCount(1);
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

  await history.getByRole('button', { name: '500' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_history_lines'))).toBe('500');
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
    .some((command) => command.type === 'read_pane' && command.lines === 500)).toBe(true);
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
  await expect(page.getByRole('button', { name: 'Open Terminal app on Fedora' })).toBeVisible();
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

  // Working workspace cards render expanded, so the last agent is reachable
  // without toggling its card.
  const lastAgent = page.getByRole('button', { name: 'Open Scrollable agent 20 on Fedora' });
  await expect(lastAgent).toBeVisible();
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

test('answers a hidden terminal prompt without storing the value anywhere', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'secret_input'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Secret app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Secret app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: 'make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });

  const prompt = page.getByRole('region', { name: 'Hidden terminal prompt' });
  await expect(prompt).toContainText('[sudo] password for cv:');
  const secret = prompt.getByLabel('Value for the hidden terminal prompt');
  await expect(secret).toHaveAttribute('type', 'password');
  await secret.fill('hunter2');
  await prompt.getByRole('button', { name: 'Send hidden value' }).click();

  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'send_secret'))
    .toMatchObject({ pane_id: 'w1:p1', text: 'hunter2' });
  // The secret must never travel as ordinary text, nor be persisted on the phone.
  const sent = await commands(page);
  expect(sent.some((command) => command.type === 'send_text' || command.type === 'submit_prompt')).toBe(false);
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('hunter2');
  await expect(secret).toHaveValue('');

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'installed', no_echo: false,
  });
  await expect(prompt).toBeHidden();
});

test('keeps a saved draft across a hidden prompt but never persists what it hides', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'secret_input'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Draft app', agent: 'codex' }],
  });
  const openTerminal = async () => {
    await page.getByRole('button', { name: 'Open Draft app on Fedora' }).click();
    return page.getByRole('combobox', { name: 'Prompt' });
  };
  const paneContent = async (extra: Record<string, unknown>) => server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', ...extra,
  });

  let prompt = await openTerminal();
  await paneContent({ content: 'a shell at rest' });
  await prompt.fill('half-written instructions');

  // Recognition pauses persistence; it must not delete a draft it did not author.
  await paneContent({
    content: 'sudo make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });
  await expect(page.getByRole('region', { name: 'Hidden terminal prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  prompt = await openTerminal();
  await expect(prompt).toHaveValue('half-written instructions');

  // Text authored while the prompt is up stays out of storage, even after the
  // prompt clears: it may be the secret typed into the wrong field.
  await paneContent({
    content: 'sudo make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });
  await prompt.fill('hunter2');
  await paneContent({ content: 'installed', no_echo: false });
  await expect(page.getByText('Not saved on this phone')).toBeVisible();
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('hunter2');

  await page.getByRole('button', { name: 'Back' }).click();
  prompt = await openTerminal();
  await expect(prompt).toHaveValue('half-written instructions');
});

test('names a hidden prompt a relay cannot answer', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Old relay', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Old relay on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Enter passphrase:',
    no_echo: true, no_echo_prompt: 'Enter passphrase:',
  });

  const prompt = page.getByRole('region', { name: 'Hidden terminal prompt' });
  await expect(prompt).toContainText('Enter passphrase:');
  await expect(prompt).toContainText('answer it at the computer');
  await expect(prompt.getByLabel('Value for the hidden terminal prompt')).toHaveCount(0);
});

test('interrupts and sends function keys from the terminal pad', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Pad app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Pad app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'a shell at rest',
  });

  const keysSent = async (keys: string[]) => (await commands(page)).filter((command) =>
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(keys)).length;

  // The pad carries no dedicated Ctrl+C button: interrupting is the armed Ctrl
  // chord, which keeps the whole pad on one row.
  await expect(page.getByRole('button', { name: 'Ctrl+C' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Ctrl', exact: true }).click();
  await page.getByRole('textbox', { name: 'Modifier shortcut character' }).press('c');
  await expect.poll(() => keysSent(['ctrl+c'])).toBe(1);

  // Ctrl stays armed for repeated chords, so the pad must be disarmed before an
  // unmodified key: otherwise F5 would leave as ctrl+f5.
  const ctrlKey = page.getByRole('button', { name: 'Ctrl', exact: true });
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');

  await page.getByRole('button', { name: 'Function keys' }).first().click();
  await page.getByRole('button', { name: 'F5', exact: true }).click();
  await expect.poll(() => keysSent(['f5'])).toBe(1);
});

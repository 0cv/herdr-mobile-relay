import { expect, test, type Locator, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const webRoot = process.env.HERDR_WEB_ROOT || 'dist';
const APP_METADATA = JSON.parse(
  readFileSync(resolve(webRoot, 'version.json'), 'utf8'),
) as { version: string; assets: number; build: string };
const APP_RELEASE = APP_METADATA.version;

interface RelayFixture {
  id: string;
  label: string;
  url: string;
  token: string;
}

interface BootOptions {
  standalone?: boolean;
  navigatorStandalone?: boolean;
  userAgent?: string;
  largeSlashCatalog?: boolean;
  answerHealthPings?: boolean;
}

async function scrollConversationToBottom(page: Page) {
  const list = page.locator('.conversation-list');
  await expect.poll(async () => {
    await list.evaluate(async element => {
      element.scrollTop = element.scrollHeight;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    const range = (await list.locator('.conversation-source-status').allTextContents()).join('').match(/Showing \d+–(\d+) of (\d+)/);
    return (!range || range[1] === range[2])
      && await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight < 2);
  }, { timeout: 15_000, intervals: [16] }).toBe(true);
}

async function showOlderConversationEntry(page: Page, id: string) {
  const list = page.locator('.conversation-list');
  const entry = list.locator(`[data-conversation-entry-id="${id}"]`);
  await expect.poll(async () => {
    if (await entry.count()) return true;
    const earlier = page.getByRole('button', { name: 'Show earlier loaded messages', exact: true });
    if (await earlier.count()) await earlier.evaluate(element => (element as HTMLElement).click());
    else {
      await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
      await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    }
    return Boolean(await entry.count());
  }, { timeout: 15_000, intervals: [16] }).toBe(true);
}

async function replyToPaneRead(page: Page, content = 'Ready', noEcho = false) {
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content, no_echo: noEcho });
  return read.request_id;
}

async function expectNoActivityFooter(composer: Locator) {
  await expect(composer.locator('.agent-progress, [role="status"][aria-atomic="true"]')).toHaveCount(0);
  await expect(composer.getByText(/^(?:●)?(?:Working…|Waiting for reply…|Finished|Agent status unknown)$/)).toHaveCount(0);
}

async function boot(page: Page, relays: RelayFixture[] = [], path = '/', options: BootOptions = {}) {
  await page.addInitScript(({ savedRelays, standalone, navigatorStandalone, userAgent, largeSlashCatalog, answerHealthPings }) => {
    if (savedRelays.length) localStorage.setItem('herdr_relays', JSON.stringify(savedRelays));
    if (navigatorStandalone !== null) {
      Object.defineProperty(navigator, 'standalone', { configurable: true, value: navigatorStandalone });
    }
    if (userAgent) {
      Object.defineProperty(navigator, 'userAgent', { configurable: true, value: userAgent });
    }
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
    let conversationFixture: ConversationFixture | null = null;
    let autoCommands = true;
    const uploadFiles = new Map<string, Array<{ name: string; media_type: string; bytes: number }>>();
    const uploadReceived = new Map<string, number>();
    const installedVoices = new Set(['en']);
    const voiceNames: Record<string, string> = {
      en: 'en_US-lessac-medium', fr: 'fr_FR-siwis-medium', de: 'de_DE-thorsten-medium',
      es: 'es_ES-davefx-medium', zh: 'zh_CN-huayan-medium',
    };
    const speechVoicePayload = () => ({
      cache_dir: '/home/test/.cache/herdr-mobile-relay/speech',
      engine_installed: true,
      languages: ['en', 'fr', 'de', 'es', 'zh'].filter((code) => installedVoices.has(code)),
      voices: ['en', 'fr', 'de', 'es', 'zh'].map((code) => ({
        language: code,
        name: voiceNames[code],
        installed: installedVoices.has(code),
        bytes: 63206179,
        engine: installedVoices.has(code) ? 'piper' : 'espeak-ng',
      })),
    });

    class MockSocket {
      static OPEN = 1;
      static CONNECTING = 0;
      static CLOSING = 2;
      static CLOSED = 3;
      readyState = MockSocket.CONNECTING;
      onopen: (() => void) | null = null;
      onclose: ((event?: { code: number }) => void) | null = null;
      onerror: (() => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      readonly index: number;
      constructor(readonly url: string, readonly protocol = '') {
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
        if (answerHealthPings && message.type === 'ping') {
          queueMicrotask(() => this.server({ type: 'pong' }));
          return;
        }
        if (['e2ee_client_hello', 'read_pane', 'watch_pane', 'unwatch_pane', 'pane_applied', 'get_activity', 'list_directories', 'refresh_agents'].includes(String(message.type))) return;
        if (!autoCommands) return;
        if (message.type === 'upload_begin') {
          const uploadId = `upload-${String(message.request_id)}`;
          uploadFiles.set(uploadId, message.files as Array<{ name: string; media_type: string; bytes: number }>);
          queueMicrotask(() => this.server({
            type: 'upload_begin_result',
            request_id: message.request_id,
            result: {
              upload_id: uploadId,
              chunk_bytes: 262144,
              expires_at: new Date(Date.now() + 60_000).toISOString(),
              limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 },
            },
          }));
          return;
        }
        if (message.type === 'upload_chunk') {
          const key = `${String(message.upload_id)}:${String(message.file_index)}`;
          const received = (uploadReceived.get(key) || 0) + atob(String(message.data || '')).length;
          uploadReceived.set(key, received);
          queueMicrotask(() => this.server({
            type: 'upload_chunk_result',
            request_id: message.request_id,
            result: {
              file_index: message.file_index,
              next_sequence: Number(message.sequence) + 1,
              received_bytes: received,
            },
          }));
          return;
        }
        if (message.type === 'upload_finish') {
          const files = uploadFiles.get(String(message.upload_id)) || [];
          const digests = message.files as Array<{ file_index: number; sha256: string }>;
          queueMicrotask(() => this.server({
            type: 'upload_finish_result',
            request_id: message.request_id,
            result: {
              attachments: files.map((file, index) => ({
                ref: `attachment:test-${index + 1}`,
                name: file.name,
                media_type: file.media_type,
                bytes: file.bytes,
                sha256: digests[index]?.sha256 || '',
                expires_at: new Date(Date.now() + 60_000).toISOString(),
              })),
            },
          }));
          return;
        }
        if (message.type === 'upload_cancel') {
          queueMicrotask(() => this.server({
            type: 'upload_cancel_result',
            request_id: message.request_id,
            result: {},
          }));
          return;
        }
        if (message.type === 'list_slash_commands') {
          const commands = largeSlashCatalog
            ? Array.from({ length: 4096 }, (_, index) => ({
              command: `/catalog-${String(index).padStart(4, '0')}`,
              description: 'D'.repeat(240),
              argument_hint: 'H'.repeat(120),
              source: 'personal',
            }))
            : [
              { command: '/help', description: 'Show the full command reference and explain every available action', source: 'builtin' },
              { command: '/copy', description: 'Copy the latest agent response', source: 'builtin' },
              { command: '/model', description: 'Choose the active model', source: 'builtin' },
              { command: '/plan', description: 'Enter plan mode', argument_hint: '[prompt]', source: 'builtin' },
              ...Array.from({ length: 18 }, (_, index) => ({
                command: `/sample-${index + 1}`,
                description: `Example command ${index + 1}`,
                source: 'builtin',
              })),
            ];
          if (largeSlashCatalog) {
            commands[350] = {
              command: '/late-command',
              description: 'Late command',
              argument_hint: 'H'.repeat(120),
              source: 'personal',
            };
          }
          queueMicrotask(() => this.server({
            type: 'command_result', request_id: message.request_id, ok: true, phase: 'completed',
            data: { commands, truncated: false },
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
        if (message.type === 'get_conversation_updates') return;
        if (message.type === 'get_conversation_history') {
          const cursor = typeof message.cursor === 'string' ? message.cursor : '';
          const older = Boolean(cursor);
          if (conversationFixture) {
            const fixture = conversationFixture;
            const configuredPage = cursor ? fixture.pages?.[cursor] : undefined;
            const pageEntries = configuredPage?.entries ?? (cursor ? [] : fixture.entries);
            const pageCursor = configuredPage?.nextCursor || (!cursor ? fixture.nextCursor : '') || '';
            const pageHasMore = configuredPage?.hasMore ?? (!cursor ? fixture.hasMore : undefined) ?? Boolean(pageCursor);
            queueMicrotask(() => this.server({
              type: 'command_result',
              action: message.type,
              request_id: message.request_id,
              ok: true,
              phase: 'completed',
              data: {
                available: true,
                state: configuredPage?.state || 'ready',
                mode: older ? 'snapshot' : 'recent',
                source_revision: configuredPage?.sourceRevision || fixture.sourceRevision || 'fixture',
                snapshot_id: configuredPage?.snapshotId || (older ? 'snapshot-1' : ''),
                next_cursor: pageCursor,
                entries: pageEntries,
                capabilities: fixture.capabilities,
                subscription_id: fixture.capabilities ? 'fixture-subscription' : undefined,
                sequence: fixture.sequence || 0,
                has_more: pageHasMore,
                total: fixture.total,
                diagnostics: configuredPage?.diagnostics || fixture.diagnostics || { source_truncated: false, corrupt_records: 0, oversized_records: 0 },
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
              state: 'ready',
              mode: older ? 'snapshot' : 'recent',
              source_revision: 'fixture',
              snapshot_id: older ? 'snapshot-1' : '',
              next_cursor: older ? '' : 'cursor-1',
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
              diagnostics: { source_truncated: true, corrupt_records: 0, oversized_records: 0 },
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
        if (message.type === 'speak_text') {
          const samples = 800;
          const wav = new Uint8Array(44 + samples * 2);
          const view = new DataView(wav.buffer);
          const ascii = (offset: number, value: string) => {
            for (let i = 0; i < value.length; i++) wav[offset + i] = value.charCodeAt(i);
          };
          ascii(0, 'RIFF'); view.setUint32(4, 36 + samples * 2, true); ascii(8, 'WAVEfmt ');
          view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
          view.setUint32(24, 8000, true); view.setUint32(28, 16000, true);
          view.setUint16(32, 2, true); view.setUint16(34, 16, true);
          ascii(36, 'data'); view.setUint32(40, samples * 2, true);
          queueMicrotask(() => this.server({
            type: 'command_result',
            action: message.type,
            request_id: message.request_id,
            ok: true,
            phase: 'completed',
            data: { format: 'wav', audio: btoa(String.fromCharCode(...wav)) },
          }));
          return;
        }
        if (['speech_voices_list', 'speech_voice_install', 'speech_voice_remove'].includes(String(message.type))) {
          if (message.type === 'speech_voice_install') installedVoices.add(String(message.language));
          if (message.type === 'speech_voice_remove') installedVoices.delete(String(message.language));
          queueMicrotask(() => this.server({
            type: 'command_result', action: message.type, request_id: message.request_id,
            ok: true, phase: 'completed', data: speechVoicePayload(),
          }));
          return;
        }
        if (message.type === 'push_subscribe' || message.type === 'push_unsubscribe') return;
        const phase = message.type === 'send_secret' ? 'completed' : message.type === 'answer_question' && nextInteraction
          ? 'advanced'
          : message.type === 'navigate_question' && nextInteraction ? 'navigated' : 'confirmed';
        let data: Record<string, unknown> = {};
        if ((message.type === 'answer_question' || message.type === 'navigate_question') && nextInteraction) data = { interaction: nextInteraction };
        else if (message.type === 'agent_start') data = { pane_id: 'w1:pre-placement' };
        else if (message.type === 'lease_pane_size') {
          data = typeof message.rows === 'number'
            ? { columns: message.columns, rows: message.rows }
            : { columns: message.columns, rows: 46 };
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
      server(message: unknown) {
        const withExactIdentity = (agent: Record<string, unknown>) => ({
          server_session_id: 'primary',
          terminal_id: `terminal-${String(agent.pane_id)}`,
          generation: 1,
          agent_session_id: '',
          ...(agent.attention_kind === 'approval' && Array.isArray(agent.options) && agent.options.length >= 2
            ? { approval_fingerprint: `fingerprint-${String(agent.event_id || agent.pane_id)}` }
            : {}),
          ...agent,
        });
        let payload = message as Record<string, unknown>;
        if (payload?.type === 'agents' && Array.isArray(payload.agents)) {
          payload = { ...payload, agents: (payload.agents as Record<string, unknown>[]).map(withExactIdentity) };
        } else if ((payload?.type === 'blocked' || payload?.type === 'agent_update') && payload.pane_id) {
          payload = withExactIdentity(payload);
        }
        this.onmessage?.({ data: JSON.stringify(payload) } as MessageEvent);
      }
      serverClose(code?: number) { this.readyState = MockSocket.CLOSED; this.onclose?.(code === undefined ? undefined : { code }); }
    }

    Object.defineProperty(window, 'WebSocket', { configurable: true, value: MockSocket });
    Object.assign(window, {
      __relayCommands: commands,
      __relaySockets: sockets,
      __relaySocketCommands(index: number) { return socketCommands[index] || []; },
      __relayServer(index: number, message: unknown) { sockets[index]?.server(message); },
      __relayClose(index: number, code?: number) { sockets[index]?.serverClose(code); },
      __relayNextInteraction(interaction: Record<string, unknown>) { nextInteraction = interaction; },
      __relayConversationFixture(fixture: ConversationFixture | null) {
        conversationFixture = fixture;
      },
      __relayAutoCommands(enabled: boolean) { autoCommands = enabled; },
    });
  }, {
    savedRelays: relays,
    standalone: options.standalone ?? false,
    navigatorStandalone: options.navigatorStandalone ?? null,
    userAgent: options.userAgent ?? '',
    largeSlashCatalog: options.largeSlashCatalog ?? false,
    answerHealthPings: options.answerHealthPings ?? true,
  });
  await page.goto(path);
}

async function socketCount(page: Page) {
  try {
    return await page.evaluate(() => (window as any).__relaySockets.length as number);
  } catch (error) {
    // WebKit can tear down the old document between page.reload() and the
    // first poll against the new one. Let expect.poll observe the replacement
    // context instead of turning that expected navigation into a test failure.
    if (error instanceof Error && error.message.includes('Execution context was destroyed')) return 0;
    throw error;
  }
}

async function server(page: Page, index: number, message: unknown) {
  await page.evaluate(({ socketIndex, payload }) => (window as any).__relayServer(socketIndex, payload), { socketIndex: index, payload: message });
}

async function commands(page: Page) {
  return page.evaluate(() => (window as any).__relayCommands as Record<string, unknown>[]);
}

async function updateProgressPlan(page: Page): Promise<Record<string, unknown> | null> {
  try {
    return await page.evaluate(() => JSON.parse(sessionStorage.getItem('herdr_update_progress') || 'null'));
  } catch (error) {
    if (error instanceof Error && error.message.includes('Execution context was destroyed')) return null;
    throw error;
  }
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
  capabilities?: Record<string, unknown>;
  sequence?: number;
  entries: Record<string, unknown>[];
  total: number;
  nextCursor?: string;
  hasMore?: boolean;
  diagnostics?: Record<string, unknown>;
  sourceRevision?: string;
  pages?: Record<string, {
    entries: Record<string, unknown>[];
    nextCursor?: string;
    hasMore?: boolean;
    state?: 'ready' | 'preparing' | 'failed';
    snapshotId?: string;
    sourceRevision?: string;
    diagnostics?: Record<string, unknown>;
  }>;
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
    type: 'push_config', protocol: 3, version: 'abc1234', host: index ? 'mac' : 'fedora',
    home: '/home/test',
    capabilities: ['attention_classification', 'clear_activities', 'directory_browser', 'self_update', 'structured_questions', 'slash_commands'],
    agent_profiles: [{ id: 'codex', label: 'Codex' }, { id: 'claude', label: 'Claude Code' }],
    ...overrides,
  });
}

const fedora = { id: 'fedora', label: 'Fedora', url: 'ws://127.0.0.1:8871', token: '' };

test('guides an unpairable relay without ever dialing it', async ({ page }) => {
  // A relay key that is not a usable relay key, with no enrolled credential:
  // nothing can be presented, so the app must not spend a dial finding out.
  await boot(page, [{ ...fedora, token: 'truncated-key' }]);

  await expect(page.getByText('Fedora needs pairing. Import a device invitation link.')).toBeVisible();
  expect(await socketCount(page)).toBe(0);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.waitForTimeout(80);
  expect(await socketCount(page)).toBe(0);

  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByText('This computer needs pairing. Import a device invitation link, or remove and add the relay.')).toBeVisible();
});

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
    cwd: '/home/test/shell-only',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1, cwd: '/work/project',
      tab_label: 'Agent', status: 'working', project: 'fallback-project', agent: 'codex',
    }],
  });

  await expect(page.getByText('Project', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Shell Only', { exact: true }).first()).toBeVisible();
  // Idle-only cards start collapsed in the mixed default; a tap expands them.
  await expect(page.getByText('No agents are running in this workspace.')).toBeHidden();
  await page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary').click();
  await expect(page.getByText('No agents are running in this workspace.')).toBeVisible();
  // The computer is named on the workspace card, once, and every row under it
  // spends its width on the directory instead of repeating that name.
  const projectWorkspace = page.locator('.workspace-card').filter({ hasText: 'Project' }).first();
  await expect(projectWorkspace.locator('summary .workspace-card-title .host-badge')).toHaveText('@Fedora');
  await expect(projectWorkspace.locator('summary .path-row')).toHaveText('/work/project');
  await expect(projectWorkspace.locator('.compact-agent-card .agent-path')).toHaveText('/work/project');
  await expect(projectWorkspace.locator('.compact-agent-card .host-badge')).toHaveCount(0);
  await expect(page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary .path-row'))
    .toHaveText('~/shell-only');

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
  await expect(card('delta').getByRole('img', { name: 'All sessions idle and ready for input' })).toBeVisible();
  // Idle-and-ready reads as a green ring, distinct from the filled unread-done
  // dot and from the grey used only for unknown states.
  await expect(card('delta').locator('.workspace-state-dot')).toHaveClass(/status-ready/);
  await expect(card('alpha').locator('.workspace-state-dot')).toHaveClass(/status-success/);
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
    .toMatchObject({ type: 'clear_activities', protocol: 3 });
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

const IPHONE_SAFARI_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1';

test('keeps an iOS setup link unredeemed for Home Screen installation', async ({ page }) => {
  const setupHash = '#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation&relay=wss%3A%2F%2Frelay-fedora.example.com';
  await boot(page, [], `/${setupHash}`, { navigatorStandalone: false, userAgent: IPHONE_SAFARI_UA });

  // The bootstrap key is one-use and the installed copy has its own storage:
  // a Safari tab that dialled would spend it before the Home Screen app opens.
  await expect(page.getByRole('status').filter({ hasText: 'Add Herdr to the iPhone or iPad Home Screen' })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'This browser tab keeps the setup link unused' })).toBeVisible();
  expect(await page.locator('link[rel="manifest"]').getAttribute('href')).toBe('/setup.webmanifest');
  expect(await page.evaluate(() => location.hash)).toBe(setupHash);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[0]))
    .toMatchObject({
      label: 'Fedora Workstation',
      url: 'wss://relay-fedora.example.com',
      token: '0123456789abcdef0123456789abcdef',
    });
  await page.waitForTimeout(500);
  expect(await socketCount(page)).toBe(0);

  // Focus and reconnect requests must not spend the key either.
  await page.evaluate(() => {
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('focus'));
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Waiting for the Home Screen app' })).toBeVisible();
  await page.getByRole('button', { name: 'Reconnect All' }).click();
  await page.waitForTimeout(500);
  expect(await socketCount(page)).toBe(0);
});

test('an iOS browser tab without pairing material still connects', async ({ page }) => {
  await boot(page, [fedora], '/', { navigatorStandalone: false, userAgent: IPHONE_SAFARI_UA });
  await expect.poll(() => socketCount(page)).toBe(1);
});

test('imports quick setup and merges agents from multiple relays', async ({ page }) => {
  await boot(
    page,
    [],
    '/#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation&relay=wss%3A%2F%2Frelay-fedora.example.com',
    { standalone: true, navigatorStandalone: true },
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
  expect(await page.locator('link[rel="manifest"]').getAttribute('href')).toBe('/manifest.webmanifest');

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
    localStorage.setItem('herdr_relays', JSON.stringify(relays.map((relay: RelayFixture, index: number) => ({
      ...relay,
      url: `ws://127.0.0.1:${8871 + index}`,
      token: '',
    }))));
    localStorage.removeItem('herdr_device_auth_v1');
  });
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(2);
  const base = 0;
  await handshake(page, base);
  await handshake(page, base + 1);
  expect((await commandsForSocket(page, base)).some((command) =>
    command.type === 'register_app_origin')).toBe(false);
  expect((await commandsForSocket(page, base + 1)).some((command) =>
    command.type === 'register_app_origin')).toBe(false);
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

test('rechecks Herdr terminal compatibility and wraps actual failures on mobile', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    herdr_status: {
      server_version: '0.9.0',
      generation: 1,
      features: {
        direct_terminal: { state: 'unknown', reason: 'not_checked', generation: 1 },
        'pane.read': { state: 'unknown', reason: 'reconnect_required', generation: 1 },
        'client_shell.endpoint': { state: 'unknown', reason: 'not_advertised', generation: 1 },
      },
    },
  });
  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(/Herdr server: 0\.9\.0/)).toBeVisible();
  await expect(page.getByText('Terminal reads: Rechecking after Herdr reconnect')).toBeVisible();
  await expect(page.getByText(/Could not check|Server upgrade needed|Server feature unavailable/)).toHaveCount(0);

  await server(page, 0, {
    type: 'herdr_status',
    status: {
      server_version: '0.9.0',
      generation: 2,
      features: {
        direct_terminal: { state: 'unknown', reason: 'not_checked', generation: 2 },
        'pane.read': { state: 'unknown', reason: 'timeout', generation: 2 },
        'workspace.move_block': { state: 'unsupported', reason: 'method_not_supported', generation: 2 },
      },
    },
  });
  const warning = page.getByRole('status').filter({ hasText: 'Terminal reads: Could not check' });
  await expect(warning).toHaveText('Terminal reads: Could not check · Workspace group reorder: Server upgrade needed');
  await expect(warning).toHaveCSS('white-space', 'normal');
  expect(await warning.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);

  await server(page, 0, {
    type: 'herdr_status',
    status: {
      server_version: '0.9.0',
      generation: 3,
      features: {
        'pane.read': { state: 'supported', reason: 'recognized_validation_refusal', generation: 3 },
        'workspace.move_block': { state: 'supported', reason: 'recognized_validation_refusal', generation: 3 },
      },
    },
  });
  await expect(page.locator('.herdr-feature-warning')).toHaveCount(0);
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
  await expect(page.getByText(fedora.url, { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Approve once' }).click();
  await expect(page.getByRole('status').filter({ hasText: /protocol v1/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  await page.evaluate(() => (window as any).__relayClose(0));
  await expect.poll(() => socketCount(page)).toBe(2);
});

test('keeps Cursor filtering text-only across status updates and restores prompts after selecting', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' }],
  });
  await page.getByRole('button', { name: 'Open Cursor picker on Fedora' }).click();
  const footer = 'Type to filter • Enter to select • Tab to edit';
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: `Available models\n${footer}`,
  });
  const input = page.getByPlaceholder('Type filter text…');
  const mutations = async () => (await commands(page)).filter((command) =>
    ['send_filter_text', 'send_input', 'send_text', 'send_keys', 'submit_prompt'].includes(String(command.type)));
  await input.fill('grok');
  expect(await mutations()).toEqual([]);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'done', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' }],
  });
  await expect(input).toBeEnabled();
  await expect(input).toHaveValue('grok');
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  await page.getByRole('button', { name: 'Send filter text' }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
  ]);
  expect((await mutations())[0]).not.toHaveProperty('keys');
  await expect(input).toBeEnabled();
  await expect(input).toHaveValue('');
  await page.getByRole('button', { name: 'Arrow keys' }).click();
  await page.getByRole('button', { name: 'Down', exact: true }).click();
  await page.getByRole('button', { name: 'Enter', exact: true }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
    expect.objectContaining({ type: 'send_keys', keys: ['Down'] }),
    expect.objectContaining({ type: 'send_keys', keys: ['Enter'] }),
  ]);
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: `${footer}\nSelected model: Grok Fast\n>`,
  });
  await expect(page.getByRole('combobox', { name: 'Prompt', exact: true })).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'done', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' }],
  });
  const prompt = page.getByPlaceholder('Type a reply…');
  await expect(prompt).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeEnabled();
  await prompt.fill('Continue');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
    expect.objectContaining({ type: 'send_keys', keys: ['Down'] }),
    expect.objectContaining({ type: 'send_keys', keys: ['Enter'] }),
    expect.objectContaining({ type: 'submit_prompt', text: 'Continue' }),
  ]);
});

test('centers plan keys and enables text only for the terminal editor', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['structured_questions', 'slash_commands', 'manual_terminal_input'],
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
  const planInput = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await expect(planInput).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
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
  const beforeManual = (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id;
  await page.getByRole('button', { name: 'Type in terminal', exact: true }).click();
  const terminalInput = page.getByRole('textbox', { name: 'Terminal text', exact: true });
  await expect(terminalInput).toBeEnabled();
  await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(beforeManual);
  await replyToPaneRead(page, promptContent);
  await terminalInput.fill('custom weekend');
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Send text', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => ['send_terminal_text', 'send_input', 'send_text', 'send_keys'].includes(String(command.type)))
    .map((command) => ({ type: command.type, text: command.text, keys: command.keys }))).toEqual([
      { type: 'send_terminal_text', text: 'custom weekend', keys: undefined },
    ]);
  await page.getByRole('button', { name: 'Back to prompt and structured controls', exact: true }).click();
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
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter' })).toBeDisabled();

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
      error_code: 'topology_churn',
      message: 'Agent inventory is changing too quickly to produce a stable snapshot.',
      last_attempt_at: 123,
      last_success_at: 0,
      stale: true,
    },
  });
  const staleAgents = [{ pane_id: 'w1:p1', status: 'working', project: 'Existing relay', agent: 'codex' }];
  await server(page, 0, { type: 'agents', agents: staleAgents });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toContainText('changing too quickly');
  await expect(page.getByRole('button', { name: 'Open Existing relay on Fedora' })).toBeVisible();
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
  await server(page, 0, { type: 'agents', agents: staleAgents });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Open Existing relay on Fedora' })).toBeVisible();
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
  const reloadNavigation = page.waitForNavigation({ waitUntil: 'domcontentloaded' });
  await dialog.getByRole('button', { name: 'Load Update', exact: true }).click();

  const reloadUrl = new URL((await reloadRequest).url());
  expect(reloadUrl.pathname).toBe('/index.html');
  expect(reloadUrl.searchParams.get('herdr_reload'))
    .toMatch(new RegExp(`^${APP_RELEASE.replaceAll('.', '\\.')}-\\d+$`));
  await reloadNavigation;
  await page.waitForFunction(() =>
    !(window as unknown as { __herdrPreReload?: boolean }).__herdrPreReload);
  await expect.poll(() => updateProgressPlan(page)).toMatchObject({
    targetVersion: APP_RELEASE,
    relayIds: ['fedora'],
    startedRelayIds: ['fedora'],
    phoneAppRequired: true,
    phoneAcknowledged: false,
    phoneReloadAttempts: 2,
    phoneState: 'failed',
  });

  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.14.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });
  await expect.poll(async () => (await commandsForSocket(page, 0)).some(
    (command) => command.type === 'check_update' || command.type === 'install_update',
  )).toBe(true);
  const checkUpdate = (await commandsForSocket(page, 0)).find((command) => command.type === 'check_update');
  if (checkUpdate) {
    await server(page, 0, {
      type: 'command_result',
      request_id: checkUpdate.request_id,
      ok: true,
      phase: 'completed',
      data: { update: availableUpdate },
    });
  }
  await expect.poll(async () => (await commandsForSocket(page, 0)).some(
    (command) => command.type === 'install_update',
  )).toBe(true);
});

test('finalizes the loaded phone with a cached legacy manifest bootstrap', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
  // The 0.20.8 bootstrap installs the manifest but emits no asset-ready flags.
  await page.route('**/manifest-loader.js', (route) => route.fulfill({
    contentType: 'application/javascript',
    body: `const setupToken = new URLSearchParams(location.hash.slice(1)).get('setup') || '';
const manifestLink = document.createElement('link');
manifestLink.rel = 'manifest';
manifestLink.href = navigator.standalone === false && setupToken.length >= 16 && setupToken.length <= 512
  ? 'setup.webmanifest'
  : 'manifest.webmanifest';
document.head.append(manifestLink);`,
  }));
  await page.addInitScript(({ metadata, relayIds }) => {
    localStorage.setItem('herdr_theme', 'light');
    sessionStorage.setItem('herdr_update_progress', JSON.stringify({
      targetVersion: metadata.version,
      relayIds,
      startedRelayIds: relayIds,
      appRelayId: '',
      phoneAppRequired: true,
      phoneTarget: metadata,
      phoneState: 'loading',
      phoneAcknowledged: false,
      phoneReloadAttempts: 1,
      startedAt: Date.now(),
    }));
  }, { metadata: APP_METADATA, relayIds: [fedora.id, mac.id] });
  await boot(page, [fedora, mac], '/?herdr_reload=resume#settings', { standalone: true });
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 0, { release_version: APP_RELEASE });
  await handshake(page, 1, { release_version: APP_RELEASE });

  const dialog = page.getByRole('dialog', { name: 'Update complete', exact: true });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('progressbar')).toHaveAttribute('value', '100');
  await dialog.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(page.getByText(`Phone app version ${APP_RELEASE}`, { exact: true })).toBeVisible();
  expect(await page.evaluate(() => ({
    relays: JSON.parse(localStorage.getItem('herdr_relays') || '[]'),
    theme: localStorage.getItem('herdr_theme'),
  }))).toEqual({ relays: [fedora, mac], theme: 'light' });
});

test('checks every self-updating relay automatically after connection', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
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
    protocol: 3,
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
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
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
  const initialRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: initialRead.request_id, target: initialRead.target,
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
  const resumedRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: resumedRead.request_id, target: resumedRead.target,
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
  await boot(page, [fedora], '/', { answerHealthPings: false });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands'],
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

  // A half-open socket answers nothing, including refocus reads.
  await setAutoCommands(page, false);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(() => socketCount(page), { timeout: 12_000 }).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 1, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'read_pane')).toBe(true);
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
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
  await boot(page, [fedora], '/', { answerHealthPings: false });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const refreshesBefore = (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'refresh_agents').length;

  await setAutoCommands(page, false);
  await page.evaluate(() => {
    (navigator as Navigator & { connection: EventTarget }).connection.dispatchEvent(new Event('change'));
  });

  await expect.poll(async () =>
    (await commandsForSocket(page, 0))
      .filter((command) => command.type === 'refresh_agents').length).toBe(refreshesBefore + 1);
  // The old socket deliberately never answers the probe. The foreground
  // health deadline replaces it without waiting for WebSocket/TCP timeouts.
  await expect.poll(() => socketCount(page), { timeout: 12_000 }).toBe(2);
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

test('keeps wide controller panes aligned and horizontally scrollable', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'claude' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  const wideRow = `${'the pane is far wider than the phone viewport '.repeat(3)}and keeps going`;
  // A box border padded out to the pane's width. Preserved spaces may hang
  // past the edge instead of wrapping, which WebKit reports as scrollable
  // width, handing the reader a pane that pans sideways.
  const paddedRow = `╰─${' '.repeat(150)}─╯`;
  // Box-drawing rows must retain their desktop-width column alignment.
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

  // Preserve desktop alignment and permit horizontal scrolling, rather than
  // claiming that browser wrapping creates an independently sized TUI.
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
  expect(geometry.wideHeight).toBeLessThan(geometry.lineHeight * 1.5);
  expect(geometry.brokenWords).toBe(0);
  expect(geometry.scrollWidth).toBeGreaterThan(geometry.clientWidth);
});

test('wraps wide terminal output for a paired reader credential', async ({ page }) => {
  // A reader never leases, even opted in on a relay that supports it.
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'pane_size_opt_in'] });
  await page.evaluate(() => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fedora: {
          kind: 'credential',
          id: 'credential-reader',
          version: 1,
          secret: 'R'.repeat(43),
          deviceId: 'device-reader',
          role: 'reader',
          locale: 'en',
          issuedAt: Date.now(),
        },
      },
    }));
  });
  // Push a fresh connection snapshot so App's derived reader-role state observes
  // the persisted credential before the agent view is rendered.
  await handshake(page, 0, { capabilities: ['attention_classification', 'pane_size_opt_in'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Reader pane', agent: 'claude' }],
  });
  await page.getByRole('button', { name: 'Open Reader pane on Fedora' }).click();
  const wideRow = `https://example.test/${'reader-output'.repeat(40)}`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: wideRow,
  });

  const reader = page.getByRole('main', { name: 'Terminal for Reader pane', exact: true });
  const readOnlyFeedback = reader.getByRole('status').filter({ hasText: 'Reader access is read only' });
  await expect(readOnlyFeedback).toContainText('Reader access is read only');
  await expect(readOnlyFeedback).toBeVisible();
  await expectNoActivityFooter(reader.locator('.prompt-composer'));
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeDisabled();
  const terminal = page.getByRole('log');
  const geometry = await terminal.evaluate((element) => {
    const row = element.querySelector<HTMLElement>('.ansi-line')!;
    return {
      rowHeight: row.getBoundingClientRect().height,
      lineHeight: Number.parseFloat(getComputedStyle(row).lineHeight),
      scrollWidth: element.scrollWidth,
      clientWidth: element.clientWidth,
    };
  });
  expect(geometry.rowHeight).toBeGreaterThan(geometry.lineHeight * 1.5);
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth + 1);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /^Settings(?:,|$)/ }).click();
  expect((await commands(page)).filter((command) => command.type === 'lease_pane_size')).toHaveLength(0);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true }))
    .toHaveAccessibleDescription(/controller terminals keep the desktop width and scroll horizontally; read-only reader panes wrap to the phone width\./);
});

const optInCapabilities = ['attention_classification', 'pane_size_opt_in', 'pane_size_opt_in_rows', 'slash_commands'];

async function leaseCommands(page: Page) {
  return (await commands(page)).filter((command) => command.type === 'lease_pane_size');
}

async function releaseCommands(page: Page) {
  return (await commands(page)).filter((command) => command.type === 'release_pane_size');
}

async function openSizedPane(page: Page, project: string, capabilities = optInCapabilities) {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project, agent: 'omp' }],
  });
  await page.getByRole('button', { name: `Open ${project} on Fedora` }).click();
  await showWidePane(page, 0);
}

async function showWidePane(page: Page, socket: number) {
  const read = (await commands(page)).findLast(command => command.type === 'read_pane');
  await server(page, socket, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    request_id: read?.request_id, target: read?.target,
    content: ['resize session baseline', `${'desktop width output '.repeat(12)}end`].join('\n'),
  });
  await expect(page.getByRole('log')).toContainText('resize session baseline');
}

async function settingsFromTerminal(page: Page) {
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /^Settings(?:,|$)/ }).click();
}

async function expectDesktopWidthLayout(page: Page) {
  const terminal = page.getByRole('log');
  await expect(terminal).not.toHaveClass(/resize-(layout|pending)/);
  await expect.poll(() => terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
}

test('followup defaults: Resize Session starts on with height off', async ({ page }) => {
  await openSizedPane(page, 'Default on');
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  expect((await leaseCommands(page))[0]).not.toHaveProperty('rows');
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBeNull();
  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).not.toBeChecked();
});

test('followup defaults: explicitly saved Resize Session off stays off', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Default off');
  await page.waitForTimeout(300);
  expect(await leaseCommands(page)).toHaveLength(0);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);

  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).not.toBeChecked();
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeDisabled();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBe('false');
  expect(await releaseCommands(page)).toHaveLength(0);
});

test('never leases on the legacy capability alone, even with consent saved', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_terminal_resize_session', 'true');
    localStorage.setItem('herdr_terminal_height_lease', 'true');
  });
  await openSizedPane(page, 'Legacy relay', ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands']);
  await page.waitForTimeout(500);
  expect(await leaseCommands(page)).toHaveLength(0);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeEnabled();

  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  const height = page.getByRole('switch', { name: 'Lease Terminal Height', exact: true });
  await expect(height).toBeChecked();
  await expect(height).toBeDisabled();
  expect(await releaseCommands(page)).toHaveLength(0);
});

test('opts in, leases with opt_in, and releases immediately when turned off in Settings', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Opt-in pane');
  expect(await leaseCommands(page)).toHaveLength(0);
  await settingsFromTerminal(page);
  const resize = page.getByRole('switch', { name: 'Resize Session', exact: true });
  await resize.click();
  await expect(resize).toBeChecked();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBe('true');
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeEnabled();
  expect(await leaseCommands(page)).toHaveLength(0);

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Opt-in pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  const [lease] = await leaseCommands(page);
  expect(lease).toMatchObject({ type: 'lease_pane_size', pane_id: 'w1:p1', protocol: 3, opt_in: true });
  expect(lease).not.toHaveProperty('rows');
  expect(lease).not.toHaveProperty('client_id');
  expect(Number(lease.columns)).toBeGreaterThanOrEqual(40);
  expect(Number(lease.columns)).toBeLessThanOrEqual(240);
  await showWidePane(page, 0);
  const terminal = page.getByRole('log');
  await expect(terminal).toHaveClass(/resize-layout/);
  await expect.poll(() => terminal.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);

  // Leaving the pane keeps the relay's short navigation grace...
  await settingsFromTerminal(page);
  await expect.poll(async () => (await releaseCommands(page)).length).toBe(1);
  expect((await releaseCommands(page))[0]).not.toHaveProperty('immediate');
  // ...which turning the switch off ends at once, with no terminal open.
  await resize.click();
  await expect(resize).not.toBeChecked();
  await expect.poll(async () => (await releaseCommands(page)).length).toBe(2);
  expect((await releaseCommands(page))[1]).toMatchObject({ type: 'release_pane_size', pane_id: 'w1:p1', immediate: true });
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeDisabled();

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Opt-in pane on Fedora' }).click();
  await showWidePane(page, 0);
  await page.waitForTimeout(300);
  expect(await leaseCommands(page)).toHaveLength(1);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
});

test('leases the height only when the relay supports it and the height switch is on', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_terminal_resize_session', 'true');
    localStorage.setItem('herdr_terminal_height_lease', 'true');
  });
  await openSizedPane(page, 'Height pane', ['attention_classification', 'pane_size_opt_in', 'slash_commands']);
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(0);
  expect((await leaseCommands(page))[0]).not.toHaveProperty('rows');

  await settingsFromTerminal(page);
  const height = page.getByRole('switch', { name: 'Lease Terminal Height', exact: true });
  await expect(height).toBeChecked();
  await expect(height).toBeDisabled();
  await handshake(page, 0, { capabilities: optInCapabilities });
  await expect(height).toBeEnabled();

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  const beforeRows = (await leaseCommands(page)).length;
  await page.getByRole('button', { name: 'Open Height pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(beforeRows);
  const withRows = (await leaseCommands(page)).at(-1)!;
  expect(withRows).toMatchObject({ opt_in: true });
  expect(Number(withRows.rows)).toBeGreaterThanOrEqual(10);
  expect(Number(withRows.rows)).toBeLessThanOrEqual(120);

  await settingsFromTerminal(page);
  await height.click();
  await expect(height).not.toBeChecked();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  const beforeWidthOnly = (await leaseCommands(page)).length;
  await page.getByRole('button', { name: 'Open Height pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(beforeWidthOnly);
  expect((await leaseCommands(page)).at(-1)).not.toHaveProperty('rows');
});

test('ignores a lease reply that lands after Resize Session was turned off', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: optInCapabilities });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Late reply', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Late reply on Fedora' }).click();
  await showWidePane(page, 0);
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  const [lease] = await leaseCommands(page);

  await settingsFromTerminal(page);
  const resize = page.getByRole('switch', { name: 'Resize Session', exact: true });
  await resize.click();
  await expect(resize).not.toBeChecked();
  await expect.poll(async () => (await releaseCommands(page))
    .some((command) => command.immediate === true && command.pane_id === 'w1:p1')).toBe(true);
  await server(page, 0, {
    type: 'command_result', action: 'lease_pane_size', request_id: lease.request_id,
    ok: true, phase: 'completed', data: { columns: lease.columns, rows: 46 },
  });

  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Late reply on Fedora' }).click();
  await showWidePane(page, 0);
  await page.waitForTimeout(300);
  expect(await leaseCommands(page)).toHaveLength(1);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
});

test('stays unleased across a reconnect after Resize Session is turned off', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await openSizedPane(page, 'Reconnect pane');
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  await settingsFromTerminal(page);
  await page.getByRole('switch', { name: 'Resize Session', exact: true }).click();
  await expect.poll(async () => (await releaseCommands(page))
    .some((command) => command.immediate === true)).toBe(true);
  await page.getByRole('button', { name: 'Back', exact: true }).click();

  await page.evaluate(() => (window as any).__relayClose(0));
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1, { capabilities: optInCapabilities });
  await server(page, 1, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Reconnect pane', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Reconnect pane on Fedora' }).click();
  await showWidePane(page, 1);
  await page.waitForTimeout(500);
  expect((await commandsForSocket(page, 1)).filter((command) => command.type === 'lease_pane_size')).toHaveLength(0);
  await expectDesktopWidthLayout(page);
});

test('keeps renewing an opted-in lease briefly while hidden and re-leases on return', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await openSizedPane(page, 'Hidden pane', ['attention_classification', 'pane_size_opt_in', 'pane_realtime_delta']);
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  // Desktop Safari reports an occluded window as hidden, so a hidden page
  // keeps renewing within the grace window instead of lapsing the lease on
  // every app switch. Grace expiry is unit-tested on the pure policy helper.
  await page.waitForTimeout(11_000);
  expect((await leaseCommands(page)).length).toBeGreaterThanOrEqual(2);
  expect(await releaseCommands(page)).toHaveLength(0);

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThanOrEqual(3);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThanOrEqual(2);
});

test('keeps pane resizing off through hidden timers and refocus', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Sleeping pane', ['attention_classification', 'pane_size_opt_in', 'pane_realtime_delta']);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  // Cross the ten-second renewal cadence. Off means no timer acquisition.
  await page.waitForTimeout(11_000);
  expect(await leaseCommands(page)).toHaveLength(0);
  expect(await releaseCommands(page)).toHaveLength(0);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThanOrEqual(2);
  expect(await leaseCommands(page)).toHaveLength(0);
});

test('estimates fixed row heights for a controller pane', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  // Desktop-width rows remain single lines on a controller pane. The
  // virtualizer must estimate that fixed-grid geometry correctly.
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

test('keeps box-drawn controller rows aligned at the desktop width', async ({ page }) => {
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
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
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
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
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
  expect((await commandsForSocket(page, 0))
    .filter((command) => command.type === 'list_slash_commands')).toHaveLength(0);
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

test('reads a response aloud in the language the relay has a voice for', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Spoken output', agent: 'codex' }],
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  // A relay that can already read aloud turns the setting on by itself the
  // first time; the phone never has to find it.
  await expect(page.getByRole('switch', { name: 'Read Responses Aloud' })).toBeChecked();
  await page.getByLabel('Language').selectOption('fr');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Spoken output on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: '     Parsed terminal response.',
  });
  const speakButton = page.getByRole('button', { name: 'Read latest response aloud' });
  await speakButton.click();
  await expect(page.getByRole('status').filter({ hasText: 'This relay has no French voice' })).toBeVisible();
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'speak_text')).toHaveLength(0);

  // The relay gains a French voice; the same button now streams audio for it.
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en', 'fr'],
  });
  await speakButton.click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'speak_text').length).toBe(1);
  expect((await commandsForSocket(page, 0)).find((command) => command.type === 'speak_text'))
    .toMatchObject({ language: 'fr', text: 'Remote markdown response\nExact copied output' });
});

test('speak reports the relay copy failure when no parser can read the pane', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Spoken omp', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Spoken omp on Fedora' }).click();
  // omp draws no response marker the phone can parse, so the relay's /copy
  // transaction is the only source of text.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: ' Completed omp response.\n\n────────────────────\n\n────────────────────',
  });
  await expect(page.getByRole('button', { name: 'Read latest response aloud' })).toBeVisible();
  expect((await commandsForSocket(page, 0))
    .filter((command) => command.type === 'list_slash_commands')).toHaveLength(0);
  await setAutoCommands(page, false);

  await page.getByRole('button', { name: 'Read latest response aloud' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const copyCommand = (await commandsForSocket(page, 0))
    .find((command) => command.type === 'copy_agent_response') as Record<string, unknown>;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: copyCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Agent did not confirm a copied response',
  });
  await expect(page.getByRole('status').filter({ hasText: 'Agent did not confirm a copied response' })).toBeVisible();
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'speak_text')).toHaveLength(0);
});

test('a reader speaks the latest transcript turn without the relay copy command', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await page.evaluate(() => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fedora: {
          kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43),
          deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now(),
        },
      },
    }));
  });
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Read aloud', agent: 'omp', conversation_history_available: true }],
  });
  await setConversationFixture(page, {
    entries: [
      { id: 'turn-1', timestamp: '2026-09-02T10:00:00Z', role: 'user', text: 'Summarise the build' },
      { id: 'turn-2', timestamp: '2026-09-02T10:00:05Z', role: 'assistant', text: 'The build passed with no warnings.' },
    ],
    total: 2,
  });
  await page.getByRole('button', { name: 'Open Read aloud on Fedora' }).click();
  const reader = page.getByRole('main', { name: 'Terminal for Read aloud', exact: true });
  const readOnlyFeedback = reader.getByRole('status').filter({ hasText: 'Reader access is read only' });
  await expect(readOnlyFeedback).toContainText('Reader access is read only');
  await expect(readOnlyFeedback).toBeVisible();
  await expectNoActivityFooter(reader.locator('.prompt-composer'));
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: ' Completed omp response.' });

  await page.getByRole('button', { name: 'Read latest response aloud' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'speak_text').length).toBe(1);
  const commands = await commandsForSocket(page, 0);
  // The relay copy command types into the agent's terminal, which a reader may
  // not do; the agent's transcript is the read-only source instead.
  expect(commands.filter((command) => command.type === 'copy_agent_response')).toHaveLength(0);
  expect(commands.find((command) => command.type === 'speak_text'))
    .toMatchObject({ text: 'The build passed with no warnings.' });
});

test('downloads a missing relay speech voice from Settings', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'speech_voice_management'],
    speech_languages: ['en'],
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('switch', { name: 'Read Responses Aloud' })).toBeChecked();
  await page.getByLabel('Language').selectOption('fr');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();

  const englishRow = page.getByLabel('English voice on Fedora', { exact: true });
  const frenchRow = page.getByLabel('French voice on Fedora', { exact: true });
  await expect(englishRow).toContainText('Neural voice cached, 63 MB');
  await expect(frenchRow).toContainText('Not downloaded - 63 MB download');

  // A download in flight parks only its own row.
  await setAutoCommands(page, false);
  const germanDownload = page.getByLabel('German voice on Fedora', { exact: true })
    .getByRole('button', { name: 'Download the German voice on Fedora' });
  await germanDownload.click();
  await expect(germanDownload).toBeDisabled();
  await expect(germanDownload).toHaveText('Downloading…');
  await expect(germanDownload).toHaveAttribute('aria-busy', 'true');
  await setAutoCommands(page, true);

  await frenchRow.getByRole('button', { name: 'Download the French voice on Fedora' }).click();
  await expect(frenchRow).toContainText('Neural voice cached, 63 MB');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toHaveCount(0);

  // The language being read aloud is never blocked from removal; the relay
  // falls back to its own system engine.
  await frenchRow.getByRole('button', { name: 'Remove the French voice on Fedora' }).click();
  await expect(frenchRow).toContainText('Not downloaded - 63 MB download');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();
  const sent = await commandsForSocket(page, 0);
  expect(sent.filter((command) => command.type === 'speech_voices_list')).toHaveLength(1);
  expect(sent.filter((command) => command.type === 'speech_voice_install'))
    .toMatchObject([{ language: 'de' }, { language: 'fr' }]);
  expect(sent.filter((command) => command.type === 'speech_voice_remove'))
    .toMatchObject([{ language: 'fr' }]);
});

test('virtualizes large ANSI grids on a controller pane', async ({ page }) => {
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

async function openOmpPane(page: Page, project: string) {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'slash_commands'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project, agent: 'omp' }],
  });
  await page.getByRole('button', { name: `Open ${project} on Fedora` }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThan(0);
}

test('renders the raw pane window as terminal history', async ({ page }) => {
  await openOmpPane(page, 'Raw window');
  const windowRow = (index: number) => `raw window row ${String(index).padStart(3, '0')}`;
  const windowRows = Array.from({ length: 120 }, (_, index) => windowRow(index + 1));
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: windowRows.join('\n'),
  });
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
  await openOmpPane(page, 'Anchored stream');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: historyRows[0],
  });
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
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Scrolling up leaves sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
    element.dispatchEvent(new Event('scroll'));
  });
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
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
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);

  // Returning to the bottom re-engages sticky mode.
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
});

test('keeps a small scroll-up from the bottom while output streams', async ({ page }) => {
  const rows = Array.from({ length: 220 }, (_, index) => `near-bottom row ${index + 1}`);
  await openOmpPane(page, 'Near bottom');
  const frame = (count: number) => server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: rows.slice(0, count).join('\n'),
  });
  await frame(150);
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('near-bottom row 150');
  const bottomDistance = () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect.poll(bottomDistance).toBeLessThan(2);

  // A finger drags the view up by less than the 48px "near the bottom" band.
  await terminal.evaluate((element) => {
    element.dispatchEvent(new Event('touchmove', { bubbles: true }));
    element.scrollTop -= 20;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  await frame(151);
  await expect(terminal).toContainText('near-bottom row 151');
  expect(await bottomDistance()).toBeGreaterThan(10);

  // Long after the scroll, an agent keeps rewriting its last line.
  await page.waitForTimeout(1_600);
  const readingTop = await terminal.evaluate((element) => element.scrollTop);
  for (let tick = 1; tick <= 5; tick += 1) {
    await server(page, 0, {
      type: 'pane_content',
      pane_id: 'w1:p1',
      format: 'ansi',
      content: rows.slice(0, 150).concat(`working ${tick}`).join('\n'),
    });
    await expect(terminal).toContainText(`working ${tick}`);
    await page.waitForTimeout(200);
  }
  expect(Math.abs(await terminal.evaluate((element) => element.scrollTop) - readingTop))
    .toBeLessThanOrEqual(1);
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Scrolling partway back down is still reading, not following.
  await terminal.evaluate((element) => {
    element.scrollTop += 5;
    element.dispatchEvent(new Event('scroll'));
  });
  await frame(152);
  await expect(terminal).toContainText('near-bottom row 152');
  expect(await bottomDistance()).toBeGreaterThan(10);

  await terminal.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await frame(200);
  await expect(terminal).toContainText('near-bottom row 200');
  await expect.poll(bottomDistance).toBeLessThan(2);

  await terminal.evaluate((element) => {
    element.scrollTop -= 20;
    element.dispatchEvent(new Event('scroll'));
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);

  // Reaching the bottom follows again, even when output lands before the scroll event.
  await terminal.evaluate((element, message) => {
    element.scrollTop = element.scrollHeight;
    (window as any).__relayServer(0, message);
  }, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: rows.slice(0, 203).join('\n') });
  await expect(terminal).toContainText('near-bottom row 203');
  await frame(220);
  await expect(terminal).toContainText('near-bottom row 220');
  await expect.poll(bottomDistance).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeHidden();
});

test('renders rows after a long wheel scroll to just above the bottom', async ({ page }) => {
  const rows = Array.from({ length: 1_000 }, (_, index) => `wheel row ${index + 1}`);
  await openOmpPane(page, 'Wheel jump');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: rows.join('\n'),
  });
  const terminal = page.getByRole('log');
  await expect(terminal.locator('.term-screen')).toHaveAttribute('data-terminal-row-count', '1000');
  await terminal.evaluate((element) => {
    element.scrollTop = element.scrollHeight / 2;
  });
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
  await page.waitForTimeout(300);

  // One big wheel scroll lands 20px above the bottom.
  await terminal.evaluate((element) => {
    element.dispatchEvent(new WheelEvent('wheel', { bubbles: true, deltaY: 10_000 }));
    element.scrollTop = element.scrollHeight - element.clientHeight - 20;
  });
  const visibleRow = () => terminal.evaluate((element) => {
    const viewport = element.getBoundingClientRect();
    return [...element.querySelectorAll('[data-terminal-row]')].some((row) => {
      const bounds = row.getBoundingClientRect();
      return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
    });
  });
  await expect.poll(visibleRow).toBe(true);
  // Being near the bottom is not a request to follow output.
  expect(await terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(10);
  await expect.poll(() => terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
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
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
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
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
      session: 'Current session',
      conversation_history_available: true,
    }],
  });

  await page.getByRole('button', { name: 'Open History app on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect(page.getByText('middle retained answer')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeVisible();
  await page.getByRole('button', { name: 'Copy History app message as Markdown' }).click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedConversation')))
    .toBe('# middle retained answer');
  // The compact view keeps only user prompts and final answers. Full history
  // owns both superseded prose and the tool activity attached to it.
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeHidden();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('intermediate progress update')).toBeVisible();
  await expect(page.getByText('Read', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeHidden();

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('Review the attached screen');
  const imageInput = page.locator('.conversation-input-area input[type=file][accept="image/*"]');
  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  const attachedPrompt = [
    'Review the attached screen',
    'Attachment: attachment:test-1',
    '',
  ].join('\n');
  await expect(composer).toHaveValue(attachedPrompt);
  await expect(page.getByText('Attached history-shot.png')).toBeVisible();
  const historyReadsBeforeSend = (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length;
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === attachedPrompt.replace(/\n$/, '')
  ))).toMatchObject({ pane_id: 'w1:p1' });
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Attached history-shot.png')).toBeHidden();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length).toBeGreaterThan(historyReadsBeforeSend);

  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  await expect(page.getByText('Attached history-shot.png')).toBeVisible();
  await page.getByRole('button', { name: 'Clear prompt text' }).click();
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Attached history-shot.png')).toBeHidden();

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

  await expect(page.getByText('first retained question')).toBeVisible();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'get_conversation_history' && command.cursor === 'cursor-1'
  ))).toMatchObject({ pane_id: 'w1:p1', cursor: 'cursor-1' });

  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('first retained');
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeHidden();
  await server(page, 0, {
    type: 'agent_update',
    pane_id: 'w1:p1',
    status: 'blocked',
    attention_kind: 'approval',
    options: ['Approve once', 'Reject'],
    server_session_id: 'session-1',
    terminal_id: 'terminal-1',
    generation: 1,
    updated_at: 2,
  });
  await expect(composer).toBeEnabled();
  await composer.fill('approval-local draft');
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Attach files', exact: true })).toBeDisabled();
  const promptsBeforeApprovalEdit = (await commands(page)).filter(command => command.type === 'submit_prompt').length;
  await composer.press('Control+Enter');
  expect((await commands(page)).filter(command => command.type === 'submit_prompt').length).toBe(promptsBeforeApprovalEdit);
  await expect(page.getByRole('button', { name: 'Type in terminal', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Approve once', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'Terminal view' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('button', { name: 'Open History app on Fedora' })).toBeVisible();
});

for (const provider of ['claude', 'codex', 'opencode', 'qoder', 'pi', 'omp', 'omo', 'hermes', 'cursor', 'kimi']) {
test(`default agent view: fresh ${provider} openings use clean Conversation`, async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: provider,
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  const before = await commands(page);
  await setConversationFixture(page, { entries: [
    { id: 'default-user', timestamp: '', role: 'user', text: 'Show the final answer' },
    { id: 'default-tool', timestamp: '', role: 'assistant', text: '', tools: [{ id: 'tool', name: 'Default tool', output: 'done' }] },
    { id: 'default-answer', timestamp: '', role: 'assistant', text: '```ts\nconst visibleAnswer = true;\n```' },
  ], total: 3 });
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Conversation', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('button', { name: 'Full history', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toHaveCount(0);
  await expect(page.getByText('Show the final answer', { exact: true })).toBeVisible();
  await expect(page.locator('.conversation-markdown code')).toHaveText('const visibleAnswer = true;\n');
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toHaveCount(0);
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await expect(page.getByLabel('Prompt', { exact: true })).toBeEnabled();
  for (const name of ['Type in terminal', 'Enter', 'Tab', 'Ctrl', 'Send text']) await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0);
  await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(true);
  expect(openingCommands.find(command => command.type === 'read_pane')).toMatchObject({ target: { server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'session-1' } });
  expect(openingCommands.some(command => command.type === 'watch_pane')).toBe(false);
  expect(openingCommands.some(command => String(command.type).startsWith('agent_') || ['submit_prompt', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size'].includes(String(command.type)))).toBe(false);
  await page.getByRole('button', { name: 'Terminal view' }).tap();
  await expect(page.getByRole('main', { name: 'Terminal for Default conversation' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Type in terminal', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBeNull();
});
}

for (const attention of ['idle', 'question', 'approval', 'pending']) {
  test(`phone Conversation controls: ${attention} delivery stays fenced across view changes`, async ({ page }) => {
    const button = (name: string) => page.getByRole('button', { name, exact: true });
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'manual_terminal_input', 'structured_questions', 'attention_classification'] });
    await server(page, 0, { type: 'agents', agents: [{
      pane_id: 'w1:p1', status: ['idle', 'pending'].includes(attention) ? 'idle' : 'blocked', project: 'Phone controls', agent: 'pi',
      conversation_history_available: true, agent_session_id: 'phone-controls-session', attention_kind: attention === 'pending' ? 'unknown' : attention,
      options: ['Allow once', 'Deny'], event_id: 'phone-approval',
      interaction: attention === 'question' ? { id: 'phone-question', kind: 'single_select', question: 'Choose destination', options: [{ index: 0, label: 'North', selected: true }], submit_label: 'Submit' } : undefined,
    }] });
    await setConversationFixture(page, { entries: [{ id: 'code', timestamp: '', role: 'assistant', text: '```ts\nconst kept = true;\n```' }], total: 1 });
    await button('Open Phone controls on Fedora').click();
    await expect(page.locator('.conversation-markdown code')).toHaveText('const kept = true;\n');
    await expect(button('Type in terminal')).toHaveCount(0);
    await button('Terminal view').click();
    await button('Type in terminal').click();
    await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
    const observe = async () => {
      const read = (await commands(page)).findLast(command => command.type === 'read_pane');
      expect(read).toBeTruthy();
      await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read!.request_id, target: read!.target, content: 'RAW TERMINAL PREVIEW\n> ', format: 'plain', no_echo: false });
    };
    await observe();
    await expect(button('Enter')).toBeEnabled();
    await setAutoCommands(page, false);
    await page.getByLabel('Terminal text', { exact: true }).fill('private phone terminal attempt');
    await button('Send text').click();
    const attempt = (await commands(page)).findLast(command => command.type === 'send_terminal_text')!;
    expect(attempt).toMatchObject({ pane_id: 'w1:p1', text: 'private phone terminal attempt', target: { server_session_id: 'primary', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'phone-controls-session' } });
    const unknown = () => server(page, 0, { type: 'command_result', request_id: attempt.request_id, ok: true, phase: 'dispatched_unknown' });
    if (attention !== 'pending') await unknown();
    await setAutoCommands(page, true);
    await button('Conversation history').click();
    if (attention === 'pending') {
      await expect(page.getByText('Waiting for terminal input delivery.', { exact: true })).toBeVisible();
      await unknown();
    }
    await expect(page.getByText('Delivery is uncertain. You can edit drafts; open Terminal and use Type in terminal to inspect before sending.', { exact: true })).toBeVisible();
    await page.getByLabel('Prompt', { exact: true }).fill('editable draft after uncertainty');
    await expect(button('Send prompt')).toBeDisabled();
    await expect(button('Attach files')).toBeDisabled();
    await page.getByLabel('Prompt', { exact: true }).press('Control+Enter');
    for (const name of ['Type in terminal', 'Enter', 'Tab', 'Ctrl', 'Send text', 'I checked the terminal']) await expect(button(name)).toHaveCount(0);
    await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
    await expect(page.getByText('RAW TERMINAL PREVIEW', { exact: true })).toHaveCount(0);
    if (attention === 'question') {
      await expect(page.getByRole('radio', { name: 'North' })).toBeEnabled();
      await expect(button('Submit')).toBeDisabled();
    } else if (attention === 'approval') await expect(button('Allow once')).toBeDisabled();
    await button('Terminal view').click();
    await button('Type in terminal').click();
    await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
    await expect(button('I checked the terminal')).toBeDisabled();
    await observe();
    await expect(button('I checked the terminal')).toBeEnabled();
    await expect(button('Enter')).toBeDisabled();
    await button('I checked the terminal').click();
    await expect(button('Enter')).toBeEnabled();
    await button('Conversation history').click();
    await expect(page.getByText(/^Delivery is uncertain\./)).toHaveCount(0);
    if (attention === 'question') await expect(button('Submit')).toBeEnabled();
    else if (attention === 'approval') await expect(button('Allow once')).toBeEnabled();
    else {
      await page.getByLabel('Prompt', { exact: true }).fill('new normal draft');
      await expect(button('Send prompt')).toBeEnabled();
    }
    expect((await commands(page)).filter(command => ['send_terminal_text', 'send_keys', 'submit_prompt', 'answer_question', 'respond', 'navigate_question', 'clarify_question'].includes(String(command.type)))).toEqual([attempt]);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('private phone terminal attempt');
  });
}

test('default agent view: stored Terminal wins across reload', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Terminal', exact: true }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('terminal');
  await page.getByRole('button', { name: 'Back' }).click();
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Stored terminal', agent: 'pi', conversation_history_available: true, agent_session_id: 'stored-session' }] });
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Stored terminal on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Stored terminal' })).toBeVisible();
  expect((await commands(page)).slice(before.length).some(command => command.type === 'get_conversation_history')).toBe(false);
});

test('default agent view: Conversation opens directly and persists across reload', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: 'codex',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  const settingsViews = page.getByRole('group', { name: 'Default View' });
  await settingsViews.getByRole('button', { name: 'Conversation' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, {
    entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'Direct conversation answer' }],
    total: 1,
  });
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Direct conversation answer')).toBeVisible();
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.find(command => command.type === 'read_pane')).toMatchObject({ target: { server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'session-1' } });
  expect(openingCommands.some(command => command.type === 'watch_pane')).toBe(false);
  expect(openingCommands.some(command => String(command.type).startsWith('agent_') || ['submit_prompt', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size'].includes(String(command.type)))).toBe(false);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(true);

  await page.getByRole('button', { name: 'Back' }).click();
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: 'codex',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
});

test('conversation loads while the browser reports offline over a live relay', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(Navigator.prototype, 'onLine', { configurable: true, get: () => false });
    localStorage.setItem('herdr_default_agent_view', 'conversation');
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Offline flag conversation',
      agent: 'pi',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await setConversationFixture(page, {
    entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'Answer despite offline flag' }],
    total: 1,
  });
  await page.getByRole('button', { name: 'Open Offline flag conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Answer despite offline flag')).toBeVisible();
});

test('pane view override: both directions, inheritance, and explicit equal values', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1', status: 'working', project: 'Override A', agent: 'codex',
        conversation_history_available: true, agent_session_id: 'session-a', terminal_id: 'terminal-a',
      },
      {
        pane_id: 'w1:p2', status: 'working', project: 'Override B', agent: 'codex',
        conversation_history_available: true, agent_session_id: 'session-b', terminal_id: 'terminal-b',
      },
    ],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'override answer' }], total: 1 });

  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const manage = page.getByRole('dialog', { name: 'Manage Agent' });
  const viewSelect = manage.getByRole('combobox', { name: 'Default View' });
  await viewSelect.selectOption('terminal');
  await manage.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Override B on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Terminal' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override A' })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' }).selectOption('conversation');
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Override B on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override B' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' }).selectOption('terminal');
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override A' })).toBeVisible();
});

test('default agent view: metadata fallback stays silent and does not switch later', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: [] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Unavailable metadata', agent: 'unknown', agent_session_id: '' }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Unavailable metadata on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Unavailable metadata' })).toBeVisible();
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(false);
  expect(await page.getByRole('alert').allTextContents()).not.toContain('unavailable transcript');
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p1', status: 'working',
    conversation_history_available: true, updated_at: 2,
  });
  await page.waitForTimeout(50);
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toHaveCount(0);
});

test('default agent view: unavailable initial page replaces history without an extra entry', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Unavailable transcript', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Unavailable transcript on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  expect(page.locator('.terminal-layout')).toHaveCount(0);
  const historyRequest = (await commands(page)).find((command) => command.type === 'get_conversation_history');
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Native transcript unavailable' },
  });
  await expect(page.getByRole('main', { name: 'Terminal for Unavailable transcript' })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('button', { name: 'Open Unavailable transcript on Fedora' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: stale automatic responses cannot redirect Settings', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Stale opening', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Stale opening on Fedora' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'get_conversation_history')).toBeTruthy();
  const historyRequest = (await commands(page)).find((command) => command.type === 'get_conversation_history');
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Settings', exact: true }).last()).toBeVisible();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Native transcript unavailable' },
  });
  await page.waitForTimeout(50);
  await expect(page.getByRole('heading', { name: 'Settings', exact: true }).last()).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: manual switching ignores preferences and keeps explicit unavailable history', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Manual switching', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'manual answer' }], total: 1 });
  await page.getByRole('button', { name: 'Open Manual switching on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Terminal view' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Manual switching' })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const historyRequest = (await commands(page)).filter((command) => command.type === 'get_conversation_history').at(-1);
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Manual transcript unavailable' },
  });
  await expect(page.getByText('Manual transcript unavailable')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Terminal view' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: empty and failed pages do not trigger fallback', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Empty transcript', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [], total: 0 });
  await page.getByRole('button', { name: 'Open Empty transcript on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('No conversation messages have been recorded yet.')).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Empty transcript on Fedora' }).click();
  const historyRequest = (await commands(page)).filter((command) => command.type === 'get_conversation_history').at(-1);
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: false,
    phase: 'failed',
    error: 'History read failed',
  });
  await expect(page.getByRole('alert')).toContainText('History read failed');
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Terminal view' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('pane view override: readers can change it while mutation actions stay disabled', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await page.evaluate(() => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fedora: {
          kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43),
          deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now(),
        },
      },
    }));
  });
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Reader preference', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Open Reader preference on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Reader preference' })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const dialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(dialog.getByRole('combobox', { name: 'Default View' })).toBeEnabled();
  await expect(dialog.getByRole('button', { name: 'Rename Tab' })).toBeDisabled();
  await expect(dialog.getByRole('button', { name: 'Clear Agent' })).toBeDisabled();
  const before = await commands(page);
  await dialog.getByRole('combobox', { name: 'Default View' }).selectOption('conversation');
  expect(await page.evaluate(() => localStorage.getItem('herdr_pane_agent_view_overrides'))).toContain('conversation');
  expect((await commands(page)).slice(before.length).some((command) => String(command.type).startsWith('agent_'))).toBe(false);
  await dialog.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await expect(page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' })).toHaveValue('conversation');
});

test('shows tool-only agent turns only in full history and decodes their arguments', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
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
      project: 'Tool history',
      agent: 'claude',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });
  // Claude Code records one text-less assistant turn per tool call and
  // serialises the arguments as JSON. Conversation omits that activity; Full
  // history keeps it available for inspection.
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

  const card = page.getByLabel(/^Conversation with/).locator('details').filter({ hasText: 'Bash' });
  await expect(card).toBeHidden();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(card).toBeVisible();
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('Bash');
  await expect(card).toBeVisible();
  await search.fill('');
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

async function openFollowingConversation(page: Page, mode: 'conversation' | 'activity' = 'activity') {
  await page.addInitScript(value => localStorage.setItem('herdr-conversation-view', value), mode);
  await boot(page, [fedora], '/', { answerHealthPings: true });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
  const capabilities = { source: 'pi_events', active_branch: true };
  const entry = (id: string, text: string, role = 'assistant') => ({ id, text, role, timestamp: '' });
  const entries = Array.from({ length: 24 }, (_, index) => entry(`follow:${index}`, `Follow seed ${index}\n\n${'wrapped history '.repeat(index === 0 ? 500 : 12)}`, index % 2 ? 'assistant' : 'user'));
  await setConversationFixture(page, { entries, total: 24, sourceRevision: 'stream:1', sequence: 0, capabilities });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', agent: 'pi', project: 'Streaming follow', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Streaming follow on Fedora' }).click();
  const list = page.locator('.conversation-list');
  const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect(list.locator('article').last()).toContainText('Follow seed 23');
  await expect.poll(gap).toBeLessThan(2);
  let sequence = 0;
  const deliver = async (incoming: Record<string, unknown>[], remove: string[] = []) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === sequence).length).toBeGreaterThan(0);
    const request = (await commands(page)).findLast(command => command.type === 'get_conversation_updates' && command.after === sequence)!;
    await server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: { kind: 'batch', subscription_id: 'fixture-subscription', source_revision: 'stream:1', sequence: ++sequence, capabilities, state: 'generating', entries: incoming, remove } });
  };
  const latestInView = async () => {
    await expect.poll(gap).toBeLessThan(2);
    expect(await list.locator('article').last().evaluate(element => element.getBoundingClientRect().bottom <= element.closest('.conversation-list')!.getBoundingClientRect().bottom + 2)).toBe(true);
  };
  return { list, gap, entry, deliver, latestInView };
}

for (const interaction of ['untouched', 'touch', 'return-to-end'] as const) {
  test(`conversation follow-the-end streams growing partials after ${interaction}`, async ({ page }) => {
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page);
    await page.waitForTimeout(350);
    if (interaction !== 'untouched') {
      await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
      if (interaction === 'return-to-end') {
        await list.evaluate(element => { element.scrollTop -= 140; element.dispatchEvent(new Event('scroll')); });
        await list.evaluate(element => { element.scrollTop = element.scrollHeight - element.clientHeight - 18; element.dispatchEvent(new Event('scroll')); });
        expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
        await scrollConversationToBottom(page);
      }
      await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    }
    for (let index = 1; index <= 12; index++) {
      await deliver([entry('live:following', `${'Streaming paragraph with wrapped words. '.repeat(index * 6)}\n\nSTREAM END ${index}`)]);
      await expect(list.locator('[data-conversation-entry-id="live:following"]')).toContainText(`STREAM END ${index}`);
      await latestInView();
    }
    await deliver([{ ...entry('live:following', ''), tools: [{ id: 'following-tool', name: 'Streaming tool', output: 'First tool line', state: 'running' }] }]);
    const tool = list.locator('[data-conversation-entry-id="live:following"] details');
    await tool.locator('summary').click();
    for (let index = 1; index <= 6; index++) {
      await deliver([{ ...entry('live:following', ''), tools: [{ id: 'following-tool', name: 'Streaming tool', output: 'Tool output wraps on the phone. '.repeat(index * 4), state: 'running' }] }]);
      await expect(tool).toHaveAttribute('open', '');
      await latestInView();
    }
  });
}

for (const heightChange of [-80, 80]) {
  test(`conversation follow-the-end releases on scroll-up during a viewport resize (${heightChange})`, async ({ page }) => {
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    const reading = await list.evaluate((element, change) => {
      const previousTop = element.scrollTop;
      const previousHeight = element.clientHeight;
      element.style.flex = `0 0 ${previousHeight + change}px`;
      const height = element.clientHeight;
      element.scrollTop = Math.min(previousTop, element.scrollHeight - height) - 120;
      const top = element.scrollTop;
      element.dispatchEvent(new Event('scroll'));
      return { top, height, previousHeight };
    }, heightChange);
    expect(reading.height - reading.previousHeight).toBe(heightChange);
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(reading.top, 0);
    await deliver([entry('follow:23', 'Streaming after a simultaneous resize and upward scroll\n\n' + 'more streamed content '.repeat(100))]);
    await expect(list.locator('[data-conversation-entry-id="follow:23"]')).toContainText('Streaming after a simultaneous resize');
    expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(reading.top, 0);
    await scrollConversationToBottom(page);
    await latestInView();
  });
}

test('conversation follow-the-end leaves selection and restored reading positions alone', async ({ page }) => {
  const { list, gap, entry, deliver, latestInView } = await openFollowingConversation(page);
  await deliver([entry('live:selection', 'Initial selection output')]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toBeAttached();
  await latestInView();
  const selectedEntry = list.locator('[data-conversation-entry-id="follow:23"]');
  await selectedEntry.locator('.conversation-markdown').evaluate(element => {
    const range = document.createRange();
    range.selectNodeContents(element);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
  });
  const selected = await page.evaluate(() => window.getSelection()!.toString());
  expect(selected.length).toBeGreaterThan(0);
  const top = await list.evaluate(element => element.scrollTop);
  await deliver([entry('live:selection', 'Do not move a selection\n\n' + 'more output '.repeat(80))]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toBeAttached();
  expect(await page.evaluate(() => window.getSelection()!.toString())).toBe(selected);
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(top, 0);
  expect(await gap()).toBeGreaterThan(100);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await latestInView();
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop -= 160; element.dispatchEvent(new Event('scroll')); });
  await list.dispatchEvent('pointerup', { pointerType: 'touch' });
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await list.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const restored = await list.evaluate(element => element.scrollTop);
  await deliver([entry('live:selection', 'Output after view switch\n\n' + 'more output '.repeat(120))]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toContainText('Output after view switch');
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(restored, 0);
});

for (const mode of ['conversation', 'activity'] as const) {
  test(`conversation follow-the-end advances the bounded ${mode} window and follows layout`, async ({ page }) => {
    test.setTimeout(60_000);
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page, mode);
    for (let index = 1; index <= 6; index++) {
      await deliver([entry('follow:23', `${'Growing recorded answer. '.repeat(index * 12)}\n\nRECORDED END ${index}`)]);
      await expect(list.locator('article').last()).toContainText(`RECORDED END ${index}`);
      await latestInView();
    }
    for (let index = 24; index < 54; index += 2) {
      await deliver([entry(`follow:${index}`, `New prompt ${index}`, 'user'), entry(`follow:${index + 1}`, `New answer ${index + 1}\n\n\`\`\`ts\n${'const line = true;\n'.repeat(index % 8 + 1)}\`\`\``)]);
      await expect(list.locator('article').last()).toContainText(`New answer ${index + 1}`);
      await latestInView();
      expect(await list.locator('article').count()).toBe(24);
    }
    await deliver([{ ...entry('follow:notice', 'Notice at the latest end'), kind: 'notice' }]);
    await expect(list.locator('article').last()).toContainText('Notice at the latest end');
    await latestInView();
    const composer = page.getByRole('combobox', { name: 'Prompt', exact: true });
    await composer.fill('Composer grows\n'.repeat(8));
    await latestInView();
    const size = page.viewportSize()!;
    await page.setViewportSize({ width: size.width, height: size.height - 180 });
    await latestInView();
    await list.locator('article').last().evaluate(async element => {
      const image = document.createElement('img');
      image.alt = 'Late layout fixture';
      image.src = 'data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" width="120" height="320"%3E%3C/svg%3E';
      element.append(image);
      await image.decode();
    });
    await latestInView();
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollTop -= 180; element.dispatchEvent(new Event('scroll')); });
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    const anchor = await list.locator('article').evaluateAll(elements => {
      const top = elements[0].closest('.conversation-list')!.getBoundingClientRect().top;
      const visible = elements.find(element => element.getBoundingClientRect().bottom > top)!;
      return { id: visible.getAttribute('data-conversation-entry-id')!, y: visible.getBoundingClientRect().top };
    });
    await deliver([entry('follow:later', 'Do not pull the reader down')]);
    const retained = list.locator(`[data-conversation-entry-id="${anchor.id}"]`);
    await expect.poll(async () => Math.abs((await retained.boundingBox())!.y - anchor.y)).toBeLessThanOrEqual(2);
    await scrollConversationToBottom(page);
    await deliver([entry('follow:resumed', 'Following again')]);
    await expect(list.locator('article').last()).toContainText('Following again');
    await latestInView();
  });
}

test('opens a long conversation at its newest turn and holds the pin', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
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
  await list.evaluate(async (element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
    await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  });
  const readingTop = await list.evaluate(element => element.scrollTop);
  const readingAnchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
  const later = [...streamed, {
    id: 'turn-62', timestamp: '2026-08-12T10:01:00Z', role: 'user', text: 'later streamed question',
  }];
  await setConversationFixture(page, { entries: later, total: later.length });
  await expect(page.getByText('later streamed question')).toHaveCount(0);
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 62 loaded messages.' })).toBeAttached();
  await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', readingAnchor!);
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(readingTop, 0);
  expect(await bottomGap()).toBeGreaterThan(200);

  // Scrolling back to the end restores the pin.
  await scrollConversationToBottom(page);
  const final = [...later, {
    id: 'turn-63', timestamp: '2026-08-12T10:02:00Z', role: 'user', text: 'final streamed question',
  }];
  await setConversationFixture(page, { entries: final, total: final.length });
  await expect(page.getByText('final streamed question')).toBeVisible();
  await expect.poll(bottomGap).toBeLessThan(2);
});

test('loads older conversation automatically when scrolled near the top', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'structured_questions', 'slash_commands', 'conversation_history'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Automatic history',
      agent: 'codex',
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
      conversation_history_available: true,
      agent_session_id: 'automatic-session',
    }],
  });
  const latestEntries = Array.from({ length: 100 }, (_, index) => [
    { id: `latest-user-${index}`, timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:00Z`, role: 'user', text: `latest question ${index}` },
    { id: `latest-answer-${index}`, timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:01Z`, role: 'assistant', text: `latest answer ${index}` },
  ]).flat();
  const olderEntries = Array.from({ length: 12 }, (_, index) => [
    { id: `older-user-${index}`, timestamp: `2026-08-12T08:${String(index).padStart(2, '0')}:00Z`, role: 'user', text: `older automatic question ${index}` },
    { id: `older-answer-${index}`, timestamp: `2026-08-12T08:${String(index).padStart(2, '0')}:01Z`, role: 'assistant', text: `older automatic answer ${index}` },
  ]).flat();
  await setConversationFixture(page, {
    entries: latestEntries,
    total: latestEntries.length + olderEntries.length,
    nextCursor: 'older-1',
    hasMore: true,
    pages: { 'older-1': { entries: olderEntries } },
  });

  await page.getByRole('button', { name: 'Open Automatic history on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  await expect(page.getByText('latest answer 99')).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'get_conversation_history').length).toBe(1);
  expect(await list.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);

  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await showOlderConversationEntry(page, 'older-user-0');
  await expect(page.getByText('older automatic answer 11')).toBeAttached();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'get_conversation_history' && command.cursor === 'older-1').length).toBe(1);
  await expect(page.getByText('Beginning of conversation reached.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Return to latest' })).toHaveCount(0);

  const readingTop = await list.evaluate((element) => element.scrollTop);
  await setConversationFixture(page, {
    entries: [...latestEntries.slice(-199), {
      id: 'live-reply', timestamp: '2026-08-12T12:00:00Z', role: 'user', text: 'live message while reading history',
    }],
    total: latestEntries.length + olderEntries.length + 1,
  });
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 225 loaded messages.' })).toBeAttached({ timeout: 10_000 });
  await expect(page.getByText('live message while reading history')).toHaveCount(0);
  await expect(page.getByText('older automatic question 0', { exact: true })).toBeAttached();
  expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
  expect(await list.evaluate((element) => element.scrollTop)).toBeCloseTo(readingTop, 0);

  await scrollConversationToBottom(page);
  await expect(page.getByText('live message while reading history')).toBeVisible();
  expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
});

test('retains a Claude continuation cursor through repeated preparation', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'conversation_history'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Claude continuation',
      agent: 'claude',
      conversation_history_available: true,
      agent_session_id: 'claude-chain-session',
    }],
  });
  const segmentBEntries = Array.from({ length: 24 }, (_, index) => ({
    id: `segment-b-${index}`,
    timestamp: `2026-09-02T11:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `complete child history ${index}`,
  }));
  const segmentAEntries = Array.from({ length: 24 }, (_, index) => ({
    id: `segment-a-${index}`,
    timestamp: `2026-09-02T10:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `complete parent history ${index}`,
  }));
  await setConversationFixture(page, {
    entries: [{ id: 'latest', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'latest continuation answer' }],
    total: 49,
    nextCursor: 'chain-prepare-b',
    hasMore: true,
    diagnostics: {
      source_truncated: false, corrupt_records: 0, oversized_records: 0,
      continuation_incomplete: true, continuation_reason: 'missing_source',
    },
    pages: {
      'chain-prepare-b': {
        entries: [], state: 'preparing', snapshotId: 'chain-snapshot', nextCursor: 'chain-ready-b', hasMore: true,
      },
      'chain-ready-b': {
        entries: segmentBEntries,
        state: 'ready', snapshotId: 'chain-snapshot', nextCursor: 'chain-prepare-a', hasMore: true,
      },
      'chain-prepare-a': {
        entries: [], state: 'preparing', snapshotId: 'chain-snapshot', nextCursor: 'chain-ready-a', hasMore: true,
      },
      'chain-ready-a': {
        entries: segmentAEntries,
        state: 'ready', snapshotId: 'chain-snapshot', hasMore: false,
      },
    },
  });
  await page.getByRole('button', { name: 'Open Claude continuation on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  const list = page.locator('.conversation-list');
  await expect(page.getByText('latest continuation answer')).toBeVisible();
  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await showOlderConversationEntry(page, 'segment-b-0');
  await expect(page.getByText('complete child history 0')).toHaveCount(1);
  await expect(page.getByText('complete child history 23')).toHaveCount(1);
  await showOlderConversationEntry(page, 'segment-a-0');
  await expect(page.getByText('complete parent history 0')).toHaveCount(1);
  await expect(page.getByText('complete parent history 23')).toHaveCount(1);
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeVisible();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history')
    .map((command) => command.cursor || '')).toEqual(['', 'chain-prepare-b', 'chain-ready-b', 'chain-prepare-a', 'chain-ready-a']);
  await setConversationFixture(page, {
    entries: [
      { id: 'latest', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'latest continuation answer' },
      { id: 'recovered', timestamp: '2026-09-02T12:01:00Z', role: 'assistant', text: 'appended C answer' },
    ],
    total: 2,
    diagnostics: { source_truncated: false, corrupt_records: 0, oversized_records: 0 },
  });
  await page.getByRole('button', { name: 'Reload history' }).click();
  await expect(page.getByText('appended C answer')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeHidden();
});

test('accepts the first Claude continuation and rejects a replacement cursor', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Claude acceptance', agent: 'claude',
      conversation_history_available: true, agent_session_id: 'claude-acceptance-session',
    }],
  });
  const segmentA = Array.from({ length: 24 }, (_, index) => ({
    id: `accept-a-${index}`,
    timestamp: `2026-09-02T10:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `A continuation history ${index}`,
  }));
  const segmentB = Array.from({ length: 24 }, (_, index) => ({
    id: `accept-b-${index}`,
    timestamp: `2026-09-02T11:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `B continuation history ${index}`,
  }));
  await setConversationFixture(page, {
    entries: segmentA,
    total: segmentA.length,
    sourceRevision: 'revision-a',
    diagnostics: {
      source_truncated: false, corrupt_records: 0, oversized_records: 0,
      continuation_incomplete: true, continuation_reason: 'missing_source',
    },
  });
  await page.getByRole('button', { name: 'Open Claude acceptance on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('A continuation history 23')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeVisible();

  await setConversationFixture(page, {
    entries: [...segmentA, ...segmentB],
    total: segmentA.length + segmentB.length,
    nextCursor: 'replacement-cursor',
    hasMore: true,
    sourceRevision: 'revision-a',
    diagnostics: { source_truncated: false, corrupt_records: 0, oversized_records: 0 },
    pages: {
      'replacement-cursor': {
        entries: [{ id: 'replacement', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'replacement history must not enter the old lane' }],
        sourceRevision: 'revision-replacement',
        hasMore: false,
      },
    },
  });
  await page.getByRole('button', { name: 'Reload history' }).click();
  await expect(page.getByText('B continuation history 23')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeHidden();

  const list = page.locator('.conversation-list');
  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await showOlderConversationEntry(page, 'accept-a-0');
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history' && command.cursor === 'replacement-cursor').length).toBe(1);
  await expect(page.getByRole('alert').filter({ hasText: 'The conversation source changed while history was being browsed.' })).toBeVisible();
  await scrollConversationToBottom(page);
  await expect(page.getByText('B continuation history 23')).toBeVisible();
  await expect(page.getByText('replacement history must not enter the old lane')).toBeHidden();
});

for (const availability of ['available', 'no-capability', 'no-directory'] as const) {
  test(`Conversation workspace button is compact and ${availability}`, async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 740 });
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', ...(availability !== 'no-capability' ? ['workspace_inspection'] : [])] });
    await setConversationFixture(page, { entries: [{ id: 'workspace:answer', timestamp: '', role: 'assistant', text: 'Workspace conversation' }], total: 1 });
    await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'Conversation workspace with a long title', agent: 'pi', cwd: availability === 'no-directory' ? '' : '/work/mobile', conversation_history_available: true }] });
    await page.getByRole('button', { name: 'Open Conversation workspace with a long title on Fedora' }).click();
    await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
    const header = page.locator('.app-header');
    const inspect = header.getByRole('button', { name: 'Inspect workspace', exact: true });
    await expect(inspect).toBeVisible();
    for (const name of ['Terminal view', 'Inspect workspace', 'Manage agent']) {
      const box = await header.getByRole('button', { name, exact: true }).boundingBox();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(360);
    }
    expect((await header.boundingBox())!.height).toBeLessThanOrEqual(50);
    if (availability !== 'available') {
      await expect(inspect).toBeDisabled();
      await expect(inspect).toHaveAttribute('title', 'Workspace inspection is unavailable');
      return;
    }
    await expect(inspect).toBeEnabled();
    await expect(inspect).toHaveAttribute('title', 'Inspect workspace files and Git changes');
    await inspect.click();
    const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
    await expect(dialog).toBeVisible();
    await dialog.locator('aside').getByRole('button', { name: 'README.md', exact: true }).click();
    await expect(dialog.getByRole('textbox', { name: 'Contents of README.md' })).toHaveValue('# Workspace preview\n\nRead-only file contents.');
    await dialog.getByRole('tab', { name: /Changes/ }).click();
    await dialog.locator('aside').getByRole('button', { name: /README\.md/ }).click();
    await expect(dialog.getByLabel('Diff for README.md')).toContainText('+Read-only change');
  });
}

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
      { pane_id: 'w1:p1', status: 'working', project: 'Codex app', agent: 'codex', cwd: '/home/test/codex', agent_session_id: 'slash-codex-session' },
      { pane_id: 'w1:p2', status: 'idle', project: 'Claude app', agent: 'claude', cwd: '/home/test/claude', agent_session_id: 'slash-claude-session' },
    ],
  });

  await page.getByRole('button', { name: 'Search all agents' }).click();
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Codex app');
  await jump.getByRole('button', { name: /Codex app/ }).click();
  const codexComposer = page.getByRole('combobox', { name: 'Prompt' });
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const slashRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: slashRead.request_id, target: slashRead.target, format: 'plain', content: 'Ready for normal input', no_echo: false });
  await expect(page.getByRole('log')).toContainText('Ready for normal input');
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

test('keeps a large slash catalog responsive in the installed PWA', async ({ page }) => {
  await boot(page, [fedora], '/', {
    largeSlashCatalog: true,
    standalone: true,
    navigatorStandalone: true,
  });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Large catalog app', agent: 'codex', cwd: '/home/test/large' }],
  });
  await page.getByRole('button', { name: 'Open Large catalog app on Fedora' }).click();
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('/');
  const popover = page.getByRole('region', { name: 'Command suggestions' });
  await expect(popover).toBeVisible();
  await expect(popover.getByRole('option')).toHaveCount(200);
  await expect(popover.getByText('200+ matching', { exact: true })).toBeVisible();
  await expect(popover.getByText('More matching commands are hidden; keep typing to narrow the list.', { exact: true })).toBeVisible();
  await page.keyboard.press('ArrowUp');
  await expect(composer).toHaveAttribute('aria-activedescendant', 'slash-command-option-199');
  await page.keyboard.press('Enter');
  await expect(composer).toHaveValue('/catalog-0199 ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);

  const filterStarted = await page.evaluate(() => performance.now());
  await composer.fill('/late');
  await expect(popover.getByRole('option', { name: /\/late-command/ })).toBeVisible();
  await expect(popover.getByRole('option')).toHaveCount(1);
  const filterMs = await page.evaluate((started) => performance.now() - started, filterStarted);
  console.log(`large slash catalog filter: ${filterMs.toFixed(2)}ms`);
  expect(filterMs).toBeLessThan(1_000);
  await popover.getByRole('option', { name: /\/late-command/ }).click();
  await expect(composer).toHaveValue('/late-command ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);

  const loadedVersion = await page.evaluate(async () => (await fetch('/version.json')).json());
  expect(loadedVersion.assets).toBe(APP_METADATA.assets);
  expect(loadedVersion.build).toBe(APP_METADATA.build);
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
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  await expect(page.getByText(/laptop and SSH views of that pane look garbled/)).toBeVisible();
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true }))
    .toHaveAccessibleDescription(/Resizing may truncate scrollback and lose older output\./);

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
  const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: read.request_id, target: read.target,
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
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', event_id: 'notice-1', project: 'Approvals', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  // A legacy notification action has no backend-verifiable identity: the app
  // opens the live thread read-only and never executes the approval itself.
  await expect(page.getByRole('main', { name: /Terminal for Approvals/ })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'No notification action was taken' })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

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
  expect(answer).toMatchObject({ selected_indices: [0], other_selected: false, protocol: 3 });
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
  await expect(composer).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Proceed with plan' }).click();
  await expect(page.getByRole('button', { name: 'Copy', exact: true })).toBeDisabled();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(1);
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

for (const resizeWhileHidden of [false, true]) {
test(resizeWhileHidden
  ? 'preserves terminal position through a delayed hidden-layout scroll'
  : 'hands an active chat to structured questions without losing terminal position', async ({ page }) => {
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
  await terminal.evaluate(async (element) => {
    await document.fonts.ready;
    let previous = '';
    let settled = 0;
    while (settled < 3) {
      await new Promise(requestAnimationFrame);
      const geometry = JSON.stringify([
        element.scrollTop, element.scrollHeight, element.clientHeight, element.clientWidth,
        ...[...element.querySelectorAll('[data-terminal-row], .terminal-virtual-spacer')]
          .map((row) => row.getBoundingClientRect().height),
      ]);
      settled = geometry === previous ? settled + 1 : 0;
      previous = geometry;
    }
  });
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
  if (resizeWhileHidden) {
    await page.getByRole('log', { includeHidden: true }).evaluate(async (element) => {
      element.style.flex = 'none';
      element.style.height = `${element.scrollHeight - element.scrollTop - 24}px`;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      element.style.removeProperty('height');
      element.style.removeProperty('flex');
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
  }

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
}

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
  await expect(page.getByPlaceholder('Type a reply…')).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
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

test('never executes notification approvals, stale or current', async ({ page }) => {
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
  await expect(page.getByRole('main', { name: /Terminal for Stale approval/ })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'No notification action was taken' })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  // Even a target naming the current event opens read-only: a legacy
  // notification action carries no backend-verifiable approval identity.
  const currentTarget = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', action: 'approve', index: 0, total: 2, notification_id: 'new-event',
  }));
  await page.evaluate((target) => { location.hash = `#notify=${target}`; }, currentTarget);
  await expect(page.getByRole('main', { name: /Terminal for Stale approval/ })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(0);
});

test('shows long approval choices in full on touch-sized buttons', async ({ page }) => {
  const long = 'Yes, allow all edits during this session (shift+tab to cycle)';
  const path = 'Yes, and always allow ./frontend/src/components/AgentListWithAVeryLongUnbrokenFileName.svelte';
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Long choice', agent: 'claude', options: [long, path, 'No'] }],
  });
  const choice = page.getByRole('button', { name: long, exact: true });
  await expect(choice).toBeVisible();
  // A long unbroken token wraps inside the card instead of widening the page.
  const card = (await page.locator('.agent-actions').boundingBox())!;
  const pathBox = (await page.getByRole('button', { name: path, exact: true }).boundingBox())!;
  expect(pathBox.x + pathBox.width).toBeLessThanOrEqual(card.x + card.width + 1);
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
  expect((await choice.boundingBox())!.height).toBeGreaterThanOrEqual(44);
  expect((await page.getByRole('button', { name: 'No', exact: true }).boundingBox())!.height).toBeGreaterThanOrEqual(44);
});

test('answers a short single-choice question from its card in one tap', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const short = {
    id: 'short-question', kind: 'single_select',
    question: `Which database should the migration target? ${'It rewrites every table. '.repeat(12)}Production data is dropped.`,
    options: [{ index: 0, label: 'Postgres' }, { index: 1, label: 'SQLite' }, { index: 2, label: 'MySQL' }],
    submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  const long = {
    ...short, id: 'long-question',
    options: ['A', 'B', 'C', 'D', 'E'].map((label, index) => ({ index, label })),
  };
  const described = {
    ...short, id: 'described-question',
    options: [{ index: 0, label: 'Keep' }, { index: 1, label: 'Drop', description: 'Deletes the table' }],
  };
  const essay = {
    ...short, id: 'essay-question', question: `Pick one. ${'Background detail. '.repeat(30)}`,
    options: [{ index: 0, label: 'One' }, { index: 1, label: 'Two' }, { index: 2, label: 'Three' }],
  };
  const followUp = { ...short, id: 'follow-up', question: 'Which region?', options: [{ index: 0, label: 'Asia' }, { index: 1, label: 'Europe' }] };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Short', agent: 'claude', options: [], interaction: short, question_layout: true },
      { pane_id: 'w1:p2', status: 'blocked', attention_kind: 'question', project: 'Long', agent: 'claude', options: [], interaction: long, question_layout: true },
      { pane_id: 'w1:p3', status: 'blocked', attention_kind: 'question', project: 'Described', agent: 'claude', options: [], interaction: described, question_layout: true },
      { pane_id: 'w1:p4', status: 'blocked', attention_kind: 'question', project: 'Essay', agent: 'claude', options: [], interaction: essay, question_layout: true },
    ],
  });
  // Five options, options with descriptions, or a very long question keep the full form;
  // nothing is preselected.
  await expect(page.getByRole('button', { name: 'Choose answer (5)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (3)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'More…' })).toBeVisible();
  // The whole question is visible next to its one-tap answers.
  const shortCard = page.locator('.agent-card', { has: page.getByRole('button', { name: 'SQLite', exact: true }) });
  const shortPrompt = shortCard.locator('.prompt-preview');
  expect(await shortPrompt.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeLessThanOrEqual(1);

  // Replies come from the test, shaped like the relay's finishQuestionWatch.
  await setAutoCommands(page, false);
  const answers = async () => (await commands(page)).filter((command) => command.type === 'answer_question');
  const reply = async (result: Record<string, unknown>) => {
    const last = (await answers()).at(-1)!;
    await server(page, 0, { type: 'command_result', action: 'answer_question', request_id: last.request_id, ...result });
  };
  const unexpected = 'The agent opened an unexpected question; the screen was refreshed';

  // A reply carrying a malformed question neither crashes the card nor replaces it.
  await page.getByRole('button', { name: 'SQLite', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(1);
  expect((await answers())[0]).toMatchObject({ interaction_id: 'short-question', selected_indices: [1], other_selected: false });
  await reply({ ok: false, phase: 'failed', error: unexpected, data: { interaction: { id: 'broken', question: 'No options' } } });
  await expect(page.getByRole('button', { name: 'SQLite', exact: true })).toBeVisible();
  await expect(page.locator('.toast')).toHaveText('The agent asked another question; it will appear with the next update.');
  await expect(page.locator('.toast.error')).toHaveCount(0);

  // A real follow-up arrives as a failed reply with the new question: the card shows it,
  // without an error and without leaving the list.
  await page.getByRole('button', { name: 'SQLite', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(2);
  await reply({ ok: false, phase: 'failed', error: unexpected, data: { interaction: followUp } });
  await expect(page.getByRole('button', { name: 'Asia', exact: true })).toBeVisible();
  await expect(page.locator('.toast.error')).toHaveCount(0);
  await expect(page.getByRole('main', { name: /Terminal for/ })).toHaveCount(0);

  // A plain failure is reported where the user is.
  await page.getByRole('button', { name: 'Asia', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(3);
  await reply({ ok: false, phase: 'failed', error: 'Pane changed before the answer landed' });
  await expect(page.locator('.toast.error')).toContainText('Pane changed before the answer landed');
  await expect(page.getByRole('main', { name: /Terminal for/ })).toHaveCount(0);
  expect(pageErrors).toEqual([]);
});

test('treats a follow-up question from the answer form like the card does', async ({ page }) => {
  const lone = {
    id: 'lone-question', kind: 'single_select', question: 'Which database?',
    options: [{ index: 0, label: 'Postgres' }, { index: 1, label: 'SQLite' }],
    submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  const followUp = { ...lone, id: 'follow-up', question: 'Which region?', options: [{ index: 0, label: 'Asia' }, { index: 1, label: 'Europe' }] };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [], interaction: lone, question_layout: true }],
  });
  await page.getByRole('button', { name: 'More…' }).click();
  await expect(page.getByRole('group', { name: 'Which database?' })).toBeVisible();
  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: /SQLite/ }).click();
  await page.getByRole('button', { name: 'Submit' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id, ok: false, phase: 'failed',
    error: 'The agent opened an unexpected question; the screen was refreshed', data: { interaction: followUp },
  });
  await expect(page.getByRole('group', { name: 'Which region?' })).toBeVisible();
  await expect(page.locator('.toast')).toHaveText('The agent asked another question.');
  await expect(page.locator('.toast.error')).toHaveCount(0);

  // In a questionnaire, landing on an unexpected question is still reported as an error.
  const third = { ...followUp, id: 'third', question: 'Which zone?', question_index: 3, question_total: 3, submit_label: 'Next' };
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [],
      interaction: { ...followUp, id: 'first-of-three', question: 'Which tier?', question_index: 1, question_total: 3, submit_label: 'Next' }, question_layout: true }],
  });
  await expect(page.getByRole('group', { name: 'Which tier?' })).toBeVisible();
  await page.getByRole('radio', { name: /Asia/ }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(2);
  const second = (await commands(page)).filter((command) => command.type === 'answer_question').at(-1)!;
  // An agent update lands before the rejection; the toast still judges the submitted question.
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [], interaction: lone, question_layout: true }],
  });
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: second.request_id, ok: false, phase: 'failed',
    error: 'The agent opened an unexpected question; the screen was refreshed', data: { interaction: third },
  });
  await expect(page.getByRole('group', { name: 'Which zone?' })).toBeVisible();
  await expect(page.locator('.toast.error')).toHaveText('The agent opened an unexpected question; the screen was refreshed');
});

test('keeps one-tap answers working with missing option indexes and explains disabled ones', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const unindexed = {
    id: 'unindexed', kind: 'single_select', question: 'Which side?',
    options: [{ label: 'Left' }, { label: 'Right' }], submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    inventory: { state: 'error', error_code: 'topology_churn', message: 'Agent inventory is changing too quickly.', last_attempt_at: 1, last_success_at: 0, stale: true },
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Sides', agent: 'claude', options: [], interaction: unindexed, question_layout: true }],
  });
  const left = page.getByRole('button', { name: 'Left', exact: true });
  await expect(left).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Right', exact: true })).toBeVisible();
  await expect(left).toHaveAttribute('title', 'Agent controls are unavailable until Herdr inventory recovers.');

  // Once inventory recovers, an option without an index answers by its position.
  await server(page, 0, { type: 'inventory_status', state: 'ready', error_code: '', message: '', last_attempt_at: 2, last_success_at: 2, stale: false });
  await page.getByRole('button', { name: 'Right', exact: true }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  expect((await commands(page)).find((command) => command.type === 'answer_question')).toMatchObject({ interaction_id: 'unindexed', selected_indices: [1] });
  expect(pageErrors).toEqual([]);
});

test('keeps the terminal key row compact on touch screens', async ({ page }) => {
  await openOmpPane(page, 'Key row');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'ready' });
  const esc = page.getByRole('button', { name: 'Esc', exact: true }).last();
  await expect(esc).toBeVisible();
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
  expect((await esc.boundingBox())!.height).toBeLessThan(44);
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
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
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
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 }] });
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  const attachFiles = page.getByRole('button', { name: 'Attach files' });
  const arrowKeys = page.getByRole('button', { name: 'Arrow keys' });
  const enterKey = page.getByRole('button', { name: 'Enter' });
  const tabKey = page.getByRole('button', { name: 'Tab', exact: true });
  const ctrlKey = page.getByRole('button', { name: 'Ctrl', exact: true });
  const shiftKey = page.getByRole('button', { name: 'Shift', exact: true });
  const altKey = page.getByRole('button', { name: 'Alt', exact: true });
  const modifierLetter = page.getByRole('textbox', { name: 'Modifier shortcut character' });
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(attachFiles.locator('svg')).toBeVisible();
  await expect(arrowKeys.locator('svg')).toBeVisible();
  await expect(enterKey).toBeVisible();
  await expect(ctrlKey).toBeVisible();
  await expect(shiftKey).toBeVisible();
  await expect(altKey).toBeVisible();
  await expect(copyOutput).toBeVisible();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(altKey).toHaveAttribute('aria-pressed', 'false');
  await expect(attachFiles).not.toContainText('▧');
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
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
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
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await page.getByRole('combobox', { name: 'Prompt' }).focus();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
  const refreshesBeforeBack = (await commands(page)).filter((command) => command.type === 'refresh_agents').length;
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'refresh_agents').length)
    .toBe(refreshesBeforeBack + 1);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 }] });
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
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'claude', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 }],
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
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await terminal.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await expect.poll(() => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);

  await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'shot.png', mimeType: 'image/png', buffer: Buffer.from('png') });
  await expect(composer).toHaveValue(/Attachment: attachment:test-1/);
  expect((await commands(page)).find((command) => command.type === 'upload_begin')).toMatchObject({
    files: [{ name: 'shot.png', media_type: 'image/png', bytes: 3 }],
    protocol: 3,
    target: {
      server_session_id: 'session-1',
      pane_id: 'w1:p1',
      terminal_id: 'terminal-1',
      generation: 1,
    },
  });
  expect((await commands(page)).find((command) => command.type === 'upload_chunk')).toMatchObject({
    data: 'cG5n',
    sha256: '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c',
    protocol: 3,
  });

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
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
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

test('waits for a dotted directory selection before launching', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    agent_profiles: [{ id: 'claude', label: 'Claude Code' }],
  });
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(1);
  const initialDirectory = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: initialDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/cv/Development', label: '~/Development' },
      parent: '/home/cv',
      directories: [
        { name: 'test.com', path: '/home/cv/Development/test.com' },
        { name: 'testcom', path: '/home/cv/Development/testcom' },
      ],
    },
  });

  await page.getByRole('button', { name: '~/Development' }).click();
  await page.getByRole('button', { name: /test\.com/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(2);
  const dottedDirectory = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(dottedDirectory).toMatchObject({ path: '/home/cv/Development/test.com' });
  const startAgent = page.getByRole('button', { name: 'Start Agent', exact: true });
  await expect(startAgent).toBeDisabled();

  await server(page, 0, {
    type: 'command_result', request_id: dottedDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/cv/Development/test.com', label: '~/Development/test.com' },
      parent: '/home/cv/Development',
      directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/test.com' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('test-com-claude');
  await expect(startAgent).toBeEnabled();
  expect(await page.getByLabel('Name').evaluate((input: HTMLInputElement) => input.checkValidity())).toBe(true);

  await startAgent.click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    profile_id: 'claude', cwd: '/home/cv/Development/test.com', name: 'test-com-claude',
  });
});

test('keeps a newly launched agent open when its native session is discovered', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { agent_profiles: [{ id: 'pi', label: 'Pi' }] });
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const listing = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: listing.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/project', label: '~/project' }, parent: '/home/test', directories: [] },
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  const launch = (await commands(page)).find((command) => command.type === 'agent_start')!;
  const launched = {
    pane_id: 'w1:p2', terminal_id: 'terminal-new', generation: 1, agent_session_id: '',
    status: 'idle', project: 'project', cwd: '/home/test/project', name: 'project-pi', agent: 'pi',
  };
  await server(page, 0, { type: 'agents', agents: [launched] });
  await server(page, 0, {
    type: 'command_result', request_id: launch.request_id, ok: true, phase: 'completed',
    data: { pane_id: launched.pane_id, name: launched.name, cwd: launched.cwd },
  });
  await page.getByRole('button', { name: 'Open project on Fedora', exact: true }).click();
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await server(page, 0, { type: 'agents', agents: [{ ...launched, generation: 2, agent_session_id: 'native-session' }] });
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await expect.poll(async () => (await commands(page)).findLast((command) => command.type === 'read_pane')?.target)
    .toMatchObject({ terminal_id: 'terminal-new', generation: 2, agent_session_id: 'native-session' });
  await expect(page.getByText('This agent is not available yet.')).toBeHidden();
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
  await page.getByRole('button', { name: 'Open relay on Fedora', exact: true }).click();
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();
  await expect.poll(() => page.evaluate(() => location.hash))
    .toBe('#pane=r3.WzMsImZlZG9yYSIsInByaW1hcnkiLCJ3MTpwMiIsInRlcm1pbmFsLXcxOnAyIiwxLCIiXQ');
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
  await expect.poll(() => page.evaluate(() => {
    const token = location.hash.match(/^#pane=r3\.(.+)$/)?.[1] || '';
    if (!token) return null;
    const padded = token.replaceAll('-', '+').replaceAll('_', '/') + '='.repeat((4 - token.length % 4) % 4);
    return JSON.parse(atob(padded));
  })).toEqual([3, 'fedora', 'primary', 'w1:p3', 'terminal-w1:p3', 1, '']);
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
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const secretRead = (await commands(page)).findLast(command => command.type === 'read_pane');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    request_id: secretRead!.request_id, target: secretRead!.target,
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
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Draft app', agent: 'codex', agent_session_id: 'draft-session' }],
  });
  const openTerminal = async () => {
    await page.getByRole('button', { name: 'Open Draft app on Fedora' }).click();
    return page.getByRole('combobox', { name: 'Prompt' });
  };
  const paneContent = async (extra: Record<string, unknown>) => {
    const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', ...(extra.no_echo === true ? {} : { request_id: read.request_id, target: read.target }), format: 'ansi', ...extra });
  };

  let prompt = await openTerminal();
  await paneContent({ content: 'a shell at rest', no_echo: false });
  await prompt.fill('half-written instructions');

  // Recognition pauses persistence; it must not delete a draft it did not author.
  await paneContent({
    content: 'sudo make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });
  await expect(page.getByRole('region', { name: 'Hidden terminal prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  prompt = await openTerminal();
  await expect(prompt).toHaveValue('');
  await paneContent({ content: 'Ready', no_echo: false });
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
  await expect(prompt).toHaveValue('');
  await paneContent({ content: 'Ready', no_echo: false });
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
  await expect(prompt).toContainText('answer at the computer');
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

test('live conversation deltas preserve local reading, selection, tool expansion and table offsets', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status: 'working', project: 'Live reading', agent: 'pi', cwd: '/fixture', conversation_history_available: true }] });
  const capabilities = { source: 'pi_events', active_branch: true };
  const entries = Array.from({ length: 100 }, (_, index) => ({ id: `pi:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Paragraph ${index}: ${'stable historical reading '.repeat(12)}` }));
  entries[99].text = '| Field | Wide value |\n| --- | --- |\n| Fixture | ' + 'wide '.repeat(90) + ' |';
  const toolEntry = { ...entries[99], tools: [{ id: 'tool', name: 'Fixture tool', input: 'synthetic input', output: 'synthetic output' }] };
  await setConversationFixture(page, { entries: [...entries.slice(0, 99), toolEntry], total: 100, sourceRevision: 'live-source:1', capabilities, sequence: 0 });
  await page.getByRole('button', { name: 'Open Live reading on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
  await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toHaveCount(0);
  await expect(page.locator('[data-conversation-entry-id="pi:99"] > small')).toHaveCount(0);
  const list = page.locator('.conversation-list');
  const tool = list.locator('details').filter({ hasText: 'Fixture tool' });
  await tool.locator('summary').click();
  await expect(tool).toHaveAttribute('open', '');
  const table = list.locator('.conversation-table');
  await table.evaluate(element => { element.scrollLeft = 180; });
  const horizontal = await table.evaluate(element => element.scrollLeft);
  await list.evaluate(element => { element.scrollTop = element.scrollHeight / 2; element.dispatchEvent(new Event('scroll')); });
  const paragraph = list.locator('.conversation-entry').filter({ hasText: 'Paragraph 90:' });
  const anchorBefore = await paragraph.evaluate(element => element.getBoundingClientRect().top);
  await paragraph.evaluate(element => { const selection = window.getSelection(); const range = document.createRange(); range.selectNodeContents(element.querySelector('.conversation-message') || element); selection?.removeAllRanges(); selection?.addRange(range); });
  const selected = await page.evaluate(() => window.getSelection()?.toString());
  const baseline = (await commands(page)).length;
  const composer = page.locator('.conversation-input-area .prompt-composer');
  for (const status of ['idle', 'working', 'done']) {
    await server(page, 0, { type: 'agent_update', pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status });
    await expectNoActivityFooter(composer);
    expect(Math.abs(await paragraph.evaluate(element => element.getBoundingClientRect().top) - anchorBefore)).toBeLessThanOrEqual(2);
    expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
    await expect(tool).toHaveAttribute('open', '');
    expect(await table.evaluate(element => element.scrollLeft)).toBe(horizontal);
  }
  const deliver = async (after: number, batch: Record<string, unknown>) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
    const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
    await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'live-source:1', sequence: after + 1, entries: [], remove: [], state: 'generating', capabilities, ...batch } });
  };
  await deliver(0, { entries: [{ id: 'live:message', role: 'assistant', text: 'First partial', timestamp: '' }] });
  await expect(list.getByText('First partial', { exact: true })).toHaveCount(0);
  await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
  await deliver(1, { entries: [{ id: 'live:message', role: 'assistant', text: 'Second partial', timestamp: '' }] });
  await expect(list.getByText('Second partial', { exact: true })).toHaveCount(0);
  expect(Math.abs(await paragraph.evaluate(element => element.getBoundingClientRect().top) - anchorBefore)).toBeLessThanOrEqual(2);
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
  await expect(tool).toHaveAttribute('open', '');
  expect(await table.evaluate(element => element.scrollLeft)).toBe(horizontal);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await expect(list.getByText('Second partial', { exact: true })).toBeVisible();
  await expect(list.locator('[data-conversation-entry-id="live:message"] > small')).toHaveText('Live · not saved yet');
  await deliver(2, { entries: [{ id: 'live:tools', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'call', name: 'Progress tool', input: '{"path":"live-argument"}', output: 'progress one', state: 'running' }] }] });
  const progress = list.locator('details').filter({ hasText: 'Progress tool' });
  await expect(progress.locator('summary')).toContainText('running');
  await progress.locator('summary').click();
  await expect(progress.getByText('progress one', { exact: true })).toBeVisible();
  const liveSearch = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  for (const query of ['Progress tool', 'live-argument', 'progress one']) {
    await liveSearch.fill(query);
    await expect(progress).toBeVisible();
  }
  await deliver(3, { entries: [{ id: 'live:tools', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'call', name: 'Progress tool', input: '{"path":"live-argument"}', output: 'progress two', state: 'failed', error: true }] }] });
  await expect(progress).toBeHidden();
  await liveSearch.fill('progress two');
  await expect(progress).toBeVisible();
  await liveSearch.fill('no matching live tool');
  await expect(progress).toBeHidden();
  await liveSearch.fill('');
  await expect(progress.locator('summary')).toContainText('failed');
  await expect(progress).toHaveAttribute('open', '');
  await list.evaluate(element => { element.scrollTop = element.scrollHeight / 2; element.dispatchEvent(new Event('scroll')); });
  await deliver(4, { entries: [{ id: 'pi:final', role: 'assistant', text: 'Final response', timestamp: '' }], remove: ['live:message', 'live:tools'], state: 'ready' });
  await expect(list.getByText('Second partial', { exact: true })).toBeHidden();
  await expect(list.getByText('Final response', { exact: true })).toHaveCount(1);
  const emitted = (await commands(page)).slice(baseline);
  expect(emitted.filter(command => ['send_keys', 'send_input', 'lease_pane_size'].includes(String(command.type)))).toEqual([]);
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await page.evaluate(() => (window as any).__relayClose(0));
  await expect(list.getByText('Final response', { exact: true })).toHaveCount(1);
  const firstBeforeScroll = await list.locator('article').first().getAttribute('data-conversation-entry-id');
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect(list.locator('article').first()).not.toHaveAttribute('data-conversation-entry-id', firstBeforeScroll!);
});

test('loaded windows retain tool expansion and table offsets without leaking state to another target', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  const target = { pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'terminal-1', generation: 1, agent_session_id: 'state-session', status: 'working', project: 'Window A', agent: 'pi', cwd: '/fixture', conversation_history_available: true };
  await server(page, 0, { type: 'agents', agents: [target, { ...target, pane_id: 'w1:p2', terminal_id: 'terminal-2', agent_session_id: 'other-session', project: 'Window B' }] });
  const entries = Array.from({ length: 240 }, (_, index) => ({ id: `pi:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Window message ${index}` }));
  const tableText = '| Field | Wide value |\n| --- | --- |\n| Fixture | ' + 'wide '.repeat(90) + ' |';
  const content = entries.map((entry, index) => index === 220 ? { ...entry, text: tableText, tools: [{ id: 'tool', name: 'Window tool', input: '{}', output: 'done' }] } : entry);
  await setConversationFixture(page, { entries: content.slice(40), total: 240, sourceRevision: 'state:1', hasMore: true, nextCursor: 'older-state', pages: { 'older-state': { entries: content.slice(0, 40) } } });
  await page.getByRole('button', { name: 'Open Window A on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  const list = page.locator('.conversation-list');
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await showOlderConversationEntry(page, 'pi:0');
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 240 loaded messages.' })).toBeAttached();
  await scrollConversationToBottom(page);
  const article = page.locator('[data-conversation-entry-id="pi:220"]');
  const tool = article.locator('details');
  await tool.locator('summary').click();
  await expect(tool).toHaveAttribute('open', '');
  const table = article.locator('.conversation-table');
  await table.evaluate(element => { element.scrollLeft = 180; element.dispatchEvent(new Event('scroll')); });
  const offset = await table.evaluate(element => element.scrollLeft);
  expect(offset).toBeGreaterThan(0);
  await page.getByRole('button', { name: 'Show earlier loaded messages', exact: true }).evaluate(element => (element as HTMLElement).click());
  await showOlderConversationEntry(page, 'pi:180');
  await expect(article).toHaveCount(0);
  expect(await page.locator('.conversation-entry').count()).toBeLessThanOrEqual(24);
  await scrollConversationToBottom(page);
  await expect(tool).toHaveAttribute('open', '');
  await expect.poll(() => table.evaluate(element => element.scrollLeft)).toBe(offset);
  await expect(page.getByText('Tool detail was truncated by the relay.')).toHaveCount(0);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Window B on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(tool).not.toHaveAttribute('open', '');
  expect(await table.evaluate(element => element.scrollLeft)).toBe(0);
});
for (const source of ['recorded_history', 'pi_events']) {
  test(`scroll to bottom follows staggered ${source} appends after older paging`, async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status: 'working', project: 'Follow window', agent: source === 'pi_events' ? 'pi' : 'codex', cwd: '/fixture', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 240 }, (_, index) => ({ id: `entry:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Seed message ${index}` }));
    await setConversationFixture(page, { entries: entries.slice(40), total: 240, sourceRevision: 'follow:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'older-follow', pages: { 'older-follow': { entries: entries.slice(0, 40) } } });
    await page.getByRole('button', { name: 'Open Follow window on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');
    await expect(list.getByText('Seed message 239', { exact: true })).toBeAttached();
    await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
    const branchTrust = page.locator('.conversation-toolbar .conversation-branch-trust');
    if (source === 'recorded_history') {
      await expect(branchTrust).toBeVisible();
      await expect(branchTrust).toContainText('Branch unverified');
      await expect(branchTrust.locator('.sr-only')).toHaveText(': saved history may not reflect the current branch.');
    } else {
      await expect(branchTrust).toHaveCount(0);
    }
    await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    await showOlderConversationEntry(page, 'entry:0');
    await expect(list.getByText('Seed message 0', { exact: true })).toBeAttached();
    await scrollConversationToBottom(page);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    const deliver = async (after: number, incoming: Record<string, unknown>[]) => {
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'follow:1', sequence: after + 1, entries: incoming, remove: [], state: 'recorded', capabilities } });
    };
    await deliver(0, [{ id: 'entry:240', role: 'user', text: 'Appended prompt', timestamp: '' }, { id: 'entry:241', role: 'assistant', text: 'Appended intermediate', timestamp: '' }]);
    await expect(list.getByText('Appended intermediate', { exact: true })).toBeVisible();
    await deliver(1, [{ id: 'entry:242', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'tail-tool', name: 'Appended tool', input: '{}', output: 'done' }] }]);
    await expect(list.locator('summary').filter({ hasText: 'Appended tool' })).toBeVisible();
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
    await list.evaluate(element => { element.dispatchEvent(new Event('pointerdown', { bubbles: true })); element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    await deliver(2, [{ id: 'entry:243', role: 'assistant', text: 'While reading older', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    await expect(list.getByText('While reading older', { exact: true })).toHaveCount(0);
  });

  test(`bottom scrolling traverses multiple bounded loaded windows (${source})`, async ({ page }) => {
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 640 }, (_, index) => ({ id: `window:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Bounded message ${index} ` + 'reading anchor '.repeat(8) }));
    await setConversationFixture(page, { entries: entries.slice(440), total: 640, sourceRevision: 'windows:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'window-240', pages: {
      'window-240': { entries: entries.slice(240, 440), hasMore: true, nextCursor: 'window-40' },
      'window-40': { entries: entries.slice(40, 240), hasMore: true, nextCursor: 'window-0' },
      'window-0': { entries: entries.slice(0, 40), hasMore: false },
    } });
    await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', project: 'Bounded windows', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    await page.getByRole('button', { name: 'Open Bounded windows on Fedora' }).click();
    const list = page.locator('.conversation-list');
    await expect(list.locator('[data-conversation-entry-id="window:639"]')).toBeAttached();
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await expect.poll(async () => {
      const earlier = page.getByRole('button', { name: 'Show earlier loaded messages', exact: true });
      if (await earlier.count()) await earlier.evaluate(element => (element as HTMLElement).click());
      await list.evaluate(async element => {
        element.scrollTop = 0;
        element.dispatchEvent(new Event('scroll'));
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      });
      return list.locator('article').first().getAttribute('data-conversation-entry-id');
    }, { timeout: 15_000, intervals: [16] }).toBe('window:0');
    await expect(list.locator('article')).toHaveCount(24);
    await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Show newer loaded messages' })).toHaveCount(0);
    const anchor = await list.locator('article').last().getAttribute('data-conversation-entry-id');
    await list.evaluate(async element => {
      element.scrollTop = element.scrollHeight;
      (window as any).__bottomAnchorTop = [...element.querySelectorAll('article')].at(-1)!.getBoundingClientRect().top;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', 'window:12');
    const retained = list.locator(`[data-conversation-entry-id="${anchor}"]`);
    expect(Math.abs(await retained.evaluate(element => element.getBoundingClientRect().top) - await page.evaluate(() => (window as any).__bottomAnchorTop))).toBeLessThanOrEqual(2);
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect(list.locator('[data-conversation-entry-id="window:639"]')).toBeVisible();
    await expect(list.locator('article')).toHaveCount(24);
  });

  test(`reader Conversation and Full history keep ${source} updates read-only`, async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
    await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await page.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43), deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now() } } })));
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity', 'pane_size_opt_in', 'pane_size_opt_in_rows'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'reader-session', status: 'working', project: 'Reader conversation', agent: 'pi', cwd: '/fixture', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    await setConversationFixture(page, { entries: [{ id: 'pi:seed', timestamp: '', role: 'assistant', text: 'Readable seed' }], total: 1, sourceRevision: 'reader:1', capabilities, sequence: 0 });
    await page.getByRole('button', { name: 'Open Reader conversation on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    const composer = page.getByRole('combobox', { name: 'Prompt', exact: true });
    const assertConversationLocked = async () => {
      await expect(composer).toBeDisabled();
      await expect(composer).toHaveAttribute('placeholder', 'Reader access is read only');
      for (const name of ['Attach photos', 'Attach files', 'Send prompt']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    };
    let after = 0;
    for (const mode of ['Conversation', 'Full history']) {
      await page.getByRole('button', { name: mode, exact: true }).click();
      await assertConversationLocked();
      await expect(page.getByText('Readable seed', { exact: true })).toBeVisible();
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      after += 1;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'reader:1', sequence: after, entries: [{ id: `pi:prompt:${after}`, timestamp: '', role: 'user', text: `Reader prompt ${mode}` }, { id: `pi:answer:${after}`, timestamp: '', role: 'assistant', text: `Reader update ${mode}` }, ...(source === 'pi_events' ? [{ id: 'live:reader', timestamp: '', role: 'assistant', text: 'Reader provisional update' }] : [])], remove: [], state: source === 'pi_events' ? 'generating' : 'recorded', capabilities } });
      await expect(page.getByText(`Reader update ${mode}`, { exact: true })).toBeVisible();
      if (source === 'pi_events' && mode === 'Full history') {
        await expect(page.getByText('Reader provisional update', { exact: true })).toBeVisible();
        await expect(page.locator('[data-conversation-entry-id="live:reader"] > small')).toHaveText('Live · not saved yet');
      }
      await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
      if (source === 'recorded_history') await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toContainText('Branch unverified');
      else await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toHaveCount(0);
      await assertConversationLocked();
    }
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await expect(page.getByRole('status').filter({ hasText: 'Reader access is read only' })).toBeVisible();
    await expect(page.getByRole('combobox', { name: 'Prompt', exact: true })).toBeDisabled();
    for (const name of ['Attach photos', 'Attach files', 'Enter', 'Tab', 'Ctrl', 'Shift', 'Arrow keys', 'Function keys']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    expect(await page.locator('.terminal-find button:enabled, .terminal-find select:enabled').count()).toBe(0);
    await page.getByRole('log').dispatchEvent('wheel', { deltaY: -200 });
    await page.setViewportSize({ width: 390, height: 740 });
    await page.getByRole('button', { name: 'Back', exact: true }).click();
    const mutations = ['acknowledge_pane', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size', 'resize_pane', 'submit_prompt', 'upload_begin', 'upload_chunk', 'upload_finish', 'copy_agent_response'];
    expect((await commands(page)).filter(command => String(command.type).startsWith('agent_') || mutations.includes(String(command.type)))).toEqual([]);
  });
}


for (const source of ['recorded_history', 'pi_events']) {
  test(`scroll intent: small touch movements stay unpinned until the actual bottom (${source})`, async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("herdr_default_agent_view", "terminal"));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll intent', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 120 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries, total: 120, sourceRevision: 'scroll:1', capabilities, sequence: 0 });
    await page.getByRole('button', { name: 'Open Scroll intent on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');

    await expect(list.locator('article')).toHaveCount(24);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    for (const delta of [-8, -8, -8, 5, -4, 7]) {
      const before = await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
      await list.evaluate((element, value) => { element.scrollBy(0, value); element.dispatchEvent(new Event('scroll')); }, delta);
      await expect(page.locator('.conversation-jump')).toHaveCount(0);
      expect(Math.abs(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight) - (before - delta))).toBeLessThanOrEqual(2);
    }
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollBy(0, -160); element.dispatchEvent(new Event('scroll')); });
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  });

  test(`scroll intent: wheel window and bottom arrival preserve reading and resume (${source})`, async ({ page, browserName }) => {
    await page.addInitScript(() => localStorage.setItem("herdr_default_agent_view", "terminal"));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll window', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 280 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries: entries.slice(80), total: 280, sourceRevision: 'scroll:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'scroll-older', pages: { 'scroll-older': { entries: entries.slice(0, 80) } } });
    await page.getByRole('button', { name: 'Open Scroll window on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');
    const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);

    await expect.poll(gap).toBeLessThan(2);
    await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    await showOlderConversationEntry(page, 'scroll:0');
    await expect(list.locator('[data-conversation-entry-id="scroll:0"]')).toBeAttached();
    await scrollConversationToBottom(page);
    await expect.poll(gap).toBeLessThan(2);
    await page.waitForTimeout(350);
    const box = (await list.boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    for (const delta of [-150, -110, -75, -50, -30]) {
      if (browserName === 'webkit') {
        await list.evaluate((element, value) => {
          element.dispatchEvent(new WheelEvent('wheel', { deltaY: value, bubbles: true }));
          element.scrollBy(0, value);
          element.dispatchEvent(new Event('scroll'));
        }, delta);
      } else await page.mouse.wheel(0, delta);
    }
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await page.waitForTimeout(100);
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    const top = await list.evaluate(element => element.scrollTop);
    const deliver = async (after: number, incoming: Record<string, unknown>[]) => {
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'scroll:1', sequence: after + 1, entries: incoming, remove: [], state: 'generating', capabilities } });
    };
    await deliver(0, [{ id: 'live:scroll', role: 'assistant', text: 'Provisional while reading', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    expect(Math.abs(await list.evaluate(element => element.scrollTop) - top)).toBeLessThanOrEqual(2);
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await deliver(1, [{ id: 'scroll:280', role: 'assistant', text: 'Committed while reading', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    await scrollConversationToBottom(page);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollBy(0, -400); element.dispatchEvent(new Event('scroll')); });
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(element => {
      (window as any).__scrollTail = new Promise<void>(resolve => {
        let count = 0;
        const timer = setInterval(() => {
          element.scrollBy(0, -35);
          element.dispatchEvent(new Event('scroll'));
          if (++count === 20) { clearInterval(timer); resolve(); }
        }, 16);
      });
    });
    await list.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
    await page.evaluate(() => (window as any).__scrollTail);
    await expect.poll(gap).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(gap).toBeLessThan(2);
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    await deliver(2, [{ id: 'scroll:281', role: 'assistant', text: 'Following after momentum', timestamp: '' }]);
    await expect(list.getByText('Following after momentum', { exact: true })).toBeVisible();
    await expect.poll(gap).toBeLessThan(2);
    expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
    for (let after = 3; after <= 6; after++) {
      await page.waitForTimeout(100);
      await deliver(after, [{ id: `scroll:${279 + after}`, role: 'assistant', text: `Stream continues ${after}`, timestamp: '' }]);
      await expect(list.getByText(`Stream continues ${after}`, { exact: true })).toBeVisible();
    }
    await page.waitForTimeout(350);
    await list.evaluate(element => {
      element.dispatchEvent(new WheelEvent('wheel', { deltaY: -240, bubbles: true }));
      element.scrollBy(0, -240);
      element.dispatchEvent(new Event('scroll'));
    });
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  });
}

test('scroll intent: upward movement releases following and explicit bottom arrival restores it', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Momentum', agent: 'pi', conversation_history_available: true, agent_session_id: 'momentum-session' }] });
  await setConversationFixture(page, { entries: Array.from({ length: 80 }, (_, index) => ({ id: `momentum:${index}`, role: index % 2 ? 'assistant' : 'user', text: `Momentum ${index} ` + 'wrapped text '.repeat(15), timestamp: '' })), total: 80 });
  await page.getByRole('button', { name: 'Open Momentum on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect.poll(gap).toBeLessThan(2);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -400); element.dispatchEvent(new Event('scroll')); });
  await list.dispatchEvent('pointerup', { pointerType: 'touch' });

  expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await list.evaluate(element => {
    (window as any).__momentumDone = new Promise<void>(resolve => {
      let count = 0;
      const timer = setInterval(() => {
        element.scrollBy(0, -35);
        element.dispatchEvent(new Event('scroll'));
        if (++count === 20) { clearInterval(timer); resolve(); }
      }, 16);
    });
  });
  await list.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await page.evaluate(() => (window as any).__momentumDone);
  await expect.poll(gap).toBeGreaterThan(2);
  await scrollConversationToBottom(page);
  await expect.poll(gap).toBeLessThan(2);
  await expect(page.locator('.conversation-jump')).toHaveCount(0);
  await page.waitForTimeout(350);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -200); element.dispatchEvent(new Event('scroll')); });
  expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
});

const composerTarget = { server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'composer-terminal', generation: 7, agent_session_id: 'composer-session' };
const composerQuestion = {
  id: 'composer-question', kind: 'single_select', question: 'Choose composer coverage',
  options: [{ index: 0, label: 'Both views' }, { index: 1, label: 'Neither' }],
  other: { hidden: true }, submit_label: 'Submit', can_go_back: false,
};

async function openSharedComposer(page: Page, mode: string, reader = false) {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  if (reader) await page.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43), deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now() } } })));
  await handshake(page, 0, { capabilities: ['conversation_history', 'attention_classification', 'structured_questions', 'slash_commands', 'secret_input'] });
  await setConversationFixture(page, { entries: [{ id: 'composer-answer', role: 'assistant', text: 'Composer history stays readable', timestamp: '' }], total: 1 });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'Shared composer', agent: 'codex', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Shared composer on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: mode, exact: true }).click();
  await expect(page.locator('.prompt-composer > .draft-status')).toHaveCount(0);
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Ready', no_echo: false });
  return page.getByRole('combobox', { name: 'Prompt', exact: true });
}

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: activity stays in the header without footer layout or focus changes`, async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 740 });
    await page.emulateMedia({ reducedMotion: 'reduce' });
    const prompt = await openSharedComposer(page, mode);
    await page.evaluate(() => { document.documentElement.style.fontSize = '20px'; });
    const composer = page.locator('.conversation-input-area .prompt-composer');
    await expectNoActivityFooter(composer);
    await prompt.focus();
    await page.evaluate(async () => {
      await document.fonts.ready;
      await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
    });
    const initialPrompt = (await prompt.boundingBox())!;
    const requests = (await commands(page)).filter(command => command.type !== 'read_pane');
    for (const status of ['working', 'unknown', 'done', 'idle']) {
      await server(page, 0, { ...composerTarget, type: 'agent_update', status });
      await expectNoActivityFooter(composer);
      await expect(page.locator('.app-header > .status-dot')).toBeVisible();
      expect(Math.abs((await prompt.boundingBox())!.y - initialPrompt.y)).toBeLessThanOrEqual(2);
      await expect(prompt).toBeFocused();
    }
    await page.waitForTimeout(350);
    expect((await commands(page)).filter(command => command.type !== 'read_pane')).toEqual(requests);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working' });
    await expectNoActivityFooter(page.locator('.terminal-bottom .prompt-composer'));
    await expect(page.locator('.app-header > .status-dot')).toBeVisible();
  });

  test(`shared composer ${mode}: slash selection fills without submitting and keyboard navigation works`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('/');
    await expect(page.getByRole('option', { name: /\/copy/ })).toHaveAttribute('aria-selected', 'true');
    await prompt.press('ArrowDown');
    await prompt.press('Enter');
    await expect(prompt).toHaveValue('/help');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    await prompt.fill('/pl');
    await prompt.press('Tab');
    await expect(prompt).toHaveValue('/plan ');
    await prompt.fill('/plan shared coverage');
    await prompt.press('Control+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({ target: composerTarget, text: '/plan shared coverage' });
  });

  test(`shared composer ${mode}: slash discovery settles during frequent live inventory updates`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await setAutoCommands(page, false);
    await prompt.fill('/');
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_slash_commands').length).toBe(1);
    const request = (await commands(page)).find(command => command.type === 'list_slash_commands')!;
    for (let update = 0; update < 12; update++) {
      await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working', updated_at: update });
      await server(page, 0, { type: 'pong' });
    }
    await server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: { commands: [{ command: '/help', description: 'Help', source: 'builtin' }], truncated: false } });
    await expect(page.getByRole('listbox')).toHaveAttribute('aria-busy', 'false');
    expect((await commands(page)).filter(command => command.type === 'list_slash_commands')).toHaveLength(1);
    await page.getByRole('button', { name: 'Refresh commands' }).click();
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_slash_commands').length).toBe(2);
  });

  test(`shared composer ${mode}: stale pane state suspends drafts without a terminal subscription`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('safe fresh draft');
    await expect.poll(async () => JSON.stringify(await page.evaluate(() => ({ ...localStorage })))).toContain('safe fresh draft');
    const reads = (await commands(page)).filter(command => command.type === 'read_pane');
    expect(reads.at(-1)).toMatchObject({ lines: 30, content_fingerprint: '', target: composerTarget });
    expect((await commands(page)).filter(command => command.type === 'watch_pane')).toHaveLength(0);
    const draftStatus = page.locator('.prompt-composer > .draft-status');
    await expect(draftStatus).toHaveText('Not saved on this phone: waiting for fresh terminal input state.');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    await prompt.fill('unchecked-secret');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', target: composerTarget, format: 'plain', content: 'Ready', no_echo: false });
    await expect(draftStatus).toHaveText('Not saved on this phone: terminal input was hidden or could not be checked when this text was typed.');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('unchecked-secret');
    await page.getByRole('button', { name: 'Terminal view' }).click();
    await replyToPaneRead(page);
    await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('safe fresh draft');
  });

  test(`shared composer ${mode}: photo upload inserts a ref before exact-target prompt dispatch`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('Review picture');
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'conversation.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect(prompt).toHaveValue('Review picture\nAttachment: attachment:test-1\n');
    await expect(page.getByText('Attached conversation.png')).toBeVisible();
    const upload = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(upload.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_finish']);
    for (const command of upload) expect(command.target).toEqual(composerTarget);
    await prompt.press('Meta+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({ target: composerTarget, text: 'Review picture\nAttachment: attachment:test-1' });
    await expect(prompt).toHaveValue('');
  });

  test(`shared composer ${mode}: upload failure remains visible while locked and restart is guarded`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('Wait for upload');
    await setAutoCommands(page, false);
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'retry.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'upload_begin').length).toBe(1);
    await prompt.press('Control+Enter');
    await prompt.press('Meta+Enter');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    const begin = (await commands(page)).find(command => command.type === 'upload_begin')!;
    await server(page, 0, { type: 'upload_begin_result', request_id: begin.request_id, error: { code: 'attachment_upload_expired' } });
    const restart = page.getByRole('button', { name: 'Restart interrupted files' });
    await expect(restart).toBeEnabled();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'blocked', attention_kind: 'unknown' });
    await expect(restart).toBeDisabled();
    await expect(page.getByRole('alert').filter({ hasText: 'The upload expired.' })).toBeVisible();
    await restart.dispatchEvent('click');
    expect((await commands(page)).filter(command => command.type === 'upload_begin')).toHaveLength(1);
    for (let snapshot = 0; snapshot < 2; snapshot += 1) await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'idle' });
    await expect(restart).toBeEnabled();
    await setAutoCommands(page, true);
    await restart.click();
    await expect(prompt).toHaveValue('Wait for upload\nAttachment: attachment:test-1\n');
    expect((await commands(page)).filter(command => command.type === 'upload_begin')).toHaveLength(2);
  });

  test(`shared composer ${mode}: hidden values and tainted drafts stay out of storage across views`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('safe shared draft');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Password:', no_echo: true, no_echo_prompt: 'Password:' });
    const hidden = page.getByLabel('Value for the hidden terminal prompt');
    await expect(hidden).toHaveAttribute('type', 'password');
    const hiddenRead = await replyToPaneRead(page, 'Password:', true);
    await prompt.fill('secret-in-wrong-field');
    await hidden.fill('masked-secret');
    await hidden.press('Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'send_secret')).toMatchObject({ target: composerTarget, text: 'masked-secret' });
    await expect(hidden).toHaveValue('');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(hiddenRead);
    await replyToPaneRead(page);
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Ready', no_echo: false });
    const draftStatus = page.locator('.prompt-composer > .draft-status');
    await expect(draftStatus).toContainText('Not saved on this phone');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('secret-in-wrong-field');
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('masked-secret');
    await page.getByRole('button', { name: 'Terminal view' }).click();
    await replyToPaneRead(page);
    await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('safe shared draft');
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await expect(page.locator('.conversation-page')).toBeVisible();
    await replyToPaneRead(page);
    await expect(prompt).toHaveValue('safe shared draft');
  });

  test(`shared composer ${mode}: terminal-only keys send arrows function keys and armed chords`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await expect(page.getByRole('button', { name: 'Keys', exact: true })).toHaveCount(0);
    await expect(page.getByRole('textbox', { name: 'Modifier shortcut character' })).toHaveCount(0);
    const assertRow = async () => {
      const row = page.locator('.term-keys:visible');
      const boxes = await row.locator('button').evaluateAll(elements => elements.map(element => {
        const rect = element.getBoundingClientRect();
        return { top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right };
      }));
      const rowBox = (await row.boundingBox())!;
      const promptBox = (await prompt.boundingBox())!;
      expect(boxes).toHaveLength(8);
      expect(Math.max(...boxes.map(box => box.top)) - Math.min(...boxes.map(box => box.top))).toBeLessThan(2);
      expect(Math.max(...boxes.map(box => box.bottom))).toBeLessThanOrEqual(promptBox.y);
      expect(Math.min(...boxes.map(box => box.left))).toBeGreaterThanOrEqual(rowBox.x);
      expect(Math.max(...boxes.map(box => box.right))).toBeLessThanOrEqual(rowBox.x + rowBox.width);
    };
    await expect(page.locator('.term-keys')).toHaveCount(0);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await assertRow();
    for (const name of ['Esc', 'Tab', 'Enter']) {
      const key = page.getByRole('button', { name, exact: true });
      await key.focus();
      await key.press('Enter');
    }
    await page.getByRole('button', { name: 'Arrow keys' }).click();
    await page.getByRole('button', { name: 'Down', exact: true }).click();
    await page.getByRole('button', { name: 'Function keys' }).click();
    await page.getByRole('button', { name: 'F5', exact: true }).click();
    await page.getByRole('button', { name: 'Ctrl', exact: true }).click();
    await page.getByRole('textbox', { name: 'Modifier shortcut character' }).press('c');
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'send_keys').map(command => command.keys)).toEqual([['Escape'], ['Tab'], ['Enter'], ['Down'], ['f5'], ['ctrl+c']]);
    for (const command of (await commands(page)).filter(command => command.type === 'send_keys')) expect(command.target).toEqual({ relay_id: 'fedora', ...composerTarget });
  });

  test(`shared composer ${mode}: structured questions and approvals respond without leaving history`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'question', interaction: composerQuestion, question_layout: true });
    await expect(prompt).toBeEnabled();
    await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
    await page.getByRole('radio', { name: 'Both views' }).check();
    expect((await commands(page)).filter(command => command.type === 'answer_question')).toEqual([]);
    await page.getByRole('button', { name: 'Submit', exact: true }).click();
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'answer_question')).toMatchObject({ target: composerTarget, interaction_id: 'composer-question' });
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'approval', options: ['Approve once', 'Deny'], command: 'Run composer checks?' });
    await page.getByRole('button', { name: 'Approve once', exact: true }).click();
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'respond')).toMatchObject({ target: composerTarget, index: 0 });
    await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
    await expect(page.getByText('Composer history stays readable')).toBeVisible();
  });

  test(`shared composer ${mode}: reader denial matrix includes slash uploads hidden values keys and responses`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode, true);
    await expect(prompt).toBeDisabled();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working' });
    await expectNoActivityFooter(page.locator('.prompt-composer'));
    for (const name of ['Attach photos', 'Attach files', 'Send prompt']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await prompt.evaluate(element => { (element as HTMLTextAreaElement).value = '/plan'; element.dispatchEvent(new Event('input', { bubbles: true })); });
    await prompt.dispatchEvent('keydown', { key: 'Enter', ctrlKey: true });
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'reader.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect(page.locator('.term-keys')).toHaveCount(0);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    for (const name of ['Esc', 'Tab', 'Enter', 'Ctrl', 'Shift', 'Alt', 'Function keys', 'Arrow keys']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await page.locator('.term-keys button').first().dispatchEvent('click');
    await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'question', interaction: composerQuestion });
    await expect(page.getByRole('radio')).toHaveCount(0);
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'approval', options: ['Approve once', 'Deny'] });
    await expect(page.getByRole('button', { name: 'Approve once' })).toHaveCount(0);
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Password:', no_echo: true });
    const hidden = page.getByLabel('Value for the hidden terminal prompt');
    await expect(hidden).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Send hidden value' })).toBeDisabled();
    await hidden.dispatchEvent('keydown', { key: 'Enter' });
    await expect(page.getByRole('listbox')).toHaveCount(0);
    const denied = ['submit_prompt', 'send_input', 'send_text', 'send_keys', 'send_secret', 'respond', 'answer_question', 'navigate_question', 'list_slash_commands', 'upload_begin', 'upload_chunk', 'upload_finish', 'upload_cancel'];
    expect((await commands(page)).filter(command => String(command.type).startsWith('agent_') || denied.includes(String(command.type)))).toEqual([]);
  });
}

test('phone feedback integrated history loads on upward demand and explicit action with a stable anchor across view changes', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  const entries = Array.from({ length: 72 }, (_, index) => ({ id: `integrated-${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Integrated message ${index} ` + 'anchor content '.repeat(30) }));
  await setConversationFixture(page, { entries: entries.slice(48), total: 72, sourceRevision: 'integrated:1', hasMore: true, nextCursor: 'older-24', pages: {
    'older-24': { entries: entries.slice(24, 48), hasMore: true, nextCursor: 'older-0' },
    'older-0': { entries: entries.slice(0, 24), hasMore: false },
  } });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'Integrated history', agent: 'codex', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Integrated history on Fedora' }).click();
  const list = page.locator('.conversation-list');
  await expect(list.locator('[data-conversation-entry-id="integrated-71"]')).toBeVisible();
  const reads = async () => (await commands(page)).filter(command => command.type === 'get_conversation_history');
  expect(await reads()).toHaveLength(1);
  expect((await reads())[0].limit).toBe(24);
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  expect(await reads()).toHaveLength(1);
  await setAutoCommands(page, false);
  await page.waitForTimeout(300);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect.poll(async () => (await reads()).length).toBe(2);
  const older = (await reads())[1];
  expect(older.cursor).toBe('older-24');
  const anchor = list.locator('[data-conversation-entry-id="integrated-48"]');
  const offset = async () => (await anchor.boundingBox())!.y - (await list.boundingBox())!.y;
  const before = await offset();
  await server(page, 0, { type: 'command_result', request_id: older.request_id, ok: true, phase: 'completed', data: { available: true, entries: entries.slice(24, 48), has_more: true, next_cursor: 'older-0', state: 'ready', mode: 'snapshot', snapshot_id: 'snapshot-1', source_revision: 'integrated:1' } });
  await expect(list.locator('[data-conversation-entry-id="integrated-36"]')).toBeAttached();
  await expect.poll(async () => Math.abs(await offset() - before)).toBeLessThanOrEqual(2);
  await expect(list.locator('.conversation-entry')).toHaveCount(24);
  expect(await reads()).toHaveLength(2);
  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  expect(await reads()).toHaveLength(2);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await reads()).length).toBe(3);
  expect((await reads())[2].cursor).toBe('older-0');
  await expect(page.getByRole('button', { name: 'Load older turns', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await expect(list.locator('[data-conversation-entry-id="integrated-71"]')).toBeVisible();
  await expect.poll(async () => (await reads()).length).toBe(4);
  const ids = await list.locator('.conversation-entry').evaluateAll(elements => elements.map(element => element.getAttribute('data-conversation-entry-id')));
  expect(new Set(ids).size).toBe(ids.length);
  expect(ids).toHaveLength(24);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});


test('followup combined notices and Pi bridge stay fenced across paging, modes and reconnect', async ({ page }) => {
  await boot(page, [fedora], '/', { answerHealthPings: true });
  await expect.poll(() => socketCount(page)).toBe(1);
  await page.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } })));
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  await handshake(page, 0, { capabilities });
  const target = { ...composerTarget, agent_session_id: 'followup-pi' };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Followup Pi', agent: 'pi', conversation_history_available: true };
  await server(page, 0, { type: 'agents', agents: [agent] });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Followup Pi on Fedora' }).click();
  const last = async (type: string) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(page)).findLast(command => command.type === type)!;
  };
  const respond = (request: Record<string, unknown>, data: unknown, phase = 'completed') => server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase, data });
  const notice = { id: 'pi:notice', role: 'assistant', kind: 'notice', timestamp: '', text: 'Generic extension request <img src=x onerror=alert(1)>' };
  const base = { available: true, total: null, state: 'ready', mode: 'native', snapshot_id: 'snapshot', source_revision: 'followup:1', capabilities: { source: 'pi_events', active_branch: true } };
  await respond(await last('get_conversation_history'), { ...base, entries: [notice], has_more: true, next_cursor: 'older', subscription_id: 'subscription', sequence: 0 });
  await expect(page.getByText(notice.text)).toBeVisible();
  await expect(page.locator('.conversation-notice img')).toHaveCount(0);
  await expectNoActivityFooter(page.locator('.prompt-composer'));
  await expect(page.locator('.app-header > .status-dot')).toBeVisible();
  await respond(await last('get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: 'followup-pi', pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
  const read = await last('read_pane');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
  const prompt = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await prompt.fill('/orchestrate-answer synthetic guidance');
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  const sentAt = Date.now();
  await respond(await last('send_pi_prompt_unconfirmed'), { sent_at: sentAt }, 'unconfirmed');
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_history' && !command.cursor).length).toBe(2);
  const unrelated = { id: 'pi:unrelated-user', role: 'user', timestamp: new Date(sentAt + 1).toISOString(), text: '/orchestrate-answer synthetic guidance' };
  await respond(await last('get_conversation_history'), { ...base, entries: [notice, unrelated], has_more: true, next_cursor: 'older', subscription_id: 'subscription', sequence: 0 });
  await expect(page.getByText(unrelated.text, { exact: true })).toBeVisible();
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_history' && command.cursor === 'older').length).toBe(1);
  const older = (await commands(page)).findLast(command => command.type === 'get_conversation_history' && command.cursor === 'older')!;
  expect(older.cursor).toBe('older');
  await respond(older, { ...base, entries: [{ ...notice, id: 'pi:older', text: 'Older generic notice' }], has_more: false, next_cursor: '' });
  await expect(page.getByText('Older generic notice', { exact: true })).toBeVisible();
  const updates = await last('get_conversation_updates');
  await respond(updates, { kind: 'batch', subscription_id: 'subscription', source_revision: 'followup:1', sequence: 1, state: 'live', capabilities: base.capabilities, remove: [], entries: [{ ...notice, id: 'pi:live', text: 'Live generic notice' }, { ...unrelated, id: 'pi:unrelated-live-user' }] });
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await expect(page.getByText('Live generic notice', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('Live generic notice', { exact: true })).toBeVisible();
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  const mockCredential = await page.evaluate(() => {
    const saved = localStorage.getItem('herdr_device_auth_v1');
    localStorage.removeItem('herdr_device_auth_v1');
    return saved;
  });
  await page.evaluate(() => (window as any).__relayClose(0));
  await expect.poll(() => socketCount(page)).toBe(2);
  await page.evaluate(saved => localStorage.setItem('herdr_device_auth_v1', saved!), mockCredential);
  await handshake(page, 1, { capabilities });
  await server(page, 1, { type: 'agents', agents: [agent] });
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Type in terminal', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await page.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  expect((await commands(page)).filter(command => command.type === 'send_pi_prompt_unconfirmed')).toHaveLength(1);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

test('followup remote tokenless relay refuses plaintext', async ({ page }) => {
  await boot(page, [{ ...fedora, url: 'wss://remote.example' }]);
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Settings', level: 2, exact: true })).toBeVisible();
  expect(await socketCount(page)).toBe(0);
  await page.getByRole('button', { name: 'Reconnect All' }).click();
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.waitForTimeout(80);
  expect(await socketCount(page)).toBe(0);
  expect(await commands(page)).toHaveLength(0);
});

test('followup Pi sends are exclusive across pages', async ({ page, context }) => {
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  const target = { ...composerTarget, agent_session_id: 'atomic-pi' };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Atomic Pi', agent: 'pi', conversation_history_available: true };
  const second = await context.newPage();
  const pages = [page, second];
  const last = async (view: Page, type: string) => {
    await expect.poll(async () => (await commands(view)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(view)).findLast(command => command.type === type)!;
  };
  const respond = (view: Page, request: Record<string, unknown>, data: unknown, phase = 'completed') => server(view, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase, data });
  for (const view of pages) {
    if (view === second) await page.evaluate(() => localStorage.removeItem('herdr_device_auth_v1'));
    await view.addInitScript(() => Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' }));
    await boot(view, [fedora], '/', { answerHealthPings: true });
    await expect.poll(() => socketCount(view)).toBe(1);
    await view.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } })));
    await setAutoCommands(view, false);
    await handshake(view, 0, { capabilities });
    await server(view, 0, { type: 'agents', agents: [agent] });
    await view.getByRole('button', { name: 'Open Atomic Pi on Fedora' }).click();
    await respond(view, await last(view, 'get_conversation_history'), { available: true, total: null, entries: [], has_more: false, state: 'ready', mode: 'native', source_revision: 'atomic:1' });
    await respond(view, await last(view, 'get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: 'atomic-pi', pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
    const read = await last(view, 'read_pane');
    await server(view, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
    await view.getByRole('combobox', { name: 'Prompt', exact: true }).fill('one simultaneous reply');
    await expect(view.getByRole('button', { name: 'Send prompt', exact: true })).toBeEnabled();
  }
  await Promise.all(pages.map(view => view.getByRole('button', { name: 'Send prompt', exact: true }).click()));
  const counts = async () => Promise.all(pages.map(async view => (await commands(view)).filter(command => command.type === 'send_pi_prompt_unconfirmed').length));
  await expect.poll(async () => (await counts()).reduce((sum, count) => sum + count, 0)).toBe(1);
  const winner = pages[(await counts()).indexOf(1)];
  const loser = pages.find(view => view !== winner)!;
  const markers = () => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('herdr_pi_send:')).map(key => localStorage.getItem(key)));
  const held = () => page.evaluate(async () => (await navigator.locks.query()).held?.filter(lock => lock.name?.startsWith('herdr_pi_send:')).length);
  await expect(loser.getByText('Check the previous Pi send first.', { exact: true })).toBeVisible();
  await expect.poll(held).toBe(1);
  await loser.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual(['unknown']);
  await expect.poll(held).toBe(1);
  await respond(winner, await last(winner, 'send_pi_prompt_unconfirmed'), { sent_at: Date.now() }, 'unconfirmed');
  await expect.poll(held).toBe(0);
  await loser.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual([]);
  expect((await counts()).reduce((sum, count) => sum + count, 0)).toBe(1);
  for (const view of pages) expect((await commands(view)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
  await second.close();
});

for (const phase of ['unconfirmed', 'dispatched_unknown', 'pending']) test(`followup Pi send survives reload after ${phase}`, async ({ page }) => {
  await boot(page, [fedora], '/', { answerHealthPings: true });
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  const target = { ...composerTarget, agent_session_id: `reload-pi-${phase}` };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Reload Pi', agent: 'pi', conversation_history_available: true };
  const credential = JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } });
  const last = async (type: string) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(page)).findLast(command => command.type === type)!;
  };
  const respond = (request: Record<string, unknown>, data: unknown, resultPhase = 'completed', ok = true) => server(page, 0, { type: 'command_result', request_id: request.request_id, ok, phase: resultPhase, error: ok ? '' : 'Synthetic uncertain delivery', data });
  const admit = async () => {
    await expect.poll(() => socketCount(page)).toBe(1);
    await page.evaluate(saved => localStorage.setItem('herdr_device_auth_v1', saved), credential);
    await setAutoCommands(page, false);
    await handshake(page, 0, { capabilities });
    await server(page, 0, { type: 'agents', agents: [agent] });
  };
  const ready = async () => {
    await respond(await last('get_conversation_history'), { available: true, total: null, entries: [{ id: 'pi:notice', role: 'assistant', kind: 'notice', timestamp: '', text: 'Reload-safe extension request' }], has_more: false, state: 'ready', mode: 'native', source_revision: 'reload:1' });
    await respond(await last('get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: target.agent_session_id, pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
    const read = await last('read_pane');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
  };
  const reload = async () => {
    await page.evaluate(() => localStorage.removeItem('herdr_device_auth_v1'));
    await page.reload();
    await admit();
    await ready();
  };
  const markers = () => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('herdr_pi_send:')).map(key => localStorage.getItem(key)));
  const send = page.getByRole('button', { name: 'Send prompt', exact: true });
  const prompt = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await admit();
  await page.getByRole('button', { name: 'Open Reload Pi on Fedora' }).click();
  await ready();
  await prompt.fill('one private reply');
  await expect(send).toBeEnabled();
  await send.click();
  const request = await last('send_pi_prompt_unconfirmed');
  expect(await markers()).toEqual(['unknown']);
  if (phase !== 'pending') {
    await respond(request, { sent_at: Date.now() }, phase, phase === 'unconfirmed');
    await expect(page.getByRole('button', { name: 'I checked — allow another send', exact: true })).toBeVisible();
  }
  expect((await commands(page)).filter(command => command.type === 'send_pi_prompt_unconfirmed')).toHaveLength(1);
  expect(await markers()).toEqual([phase === 'unconfirmed' ? 'sent' : 'unknown']);
  await reload();
  await expect(page.getByText(phase === 'unconfirmed' ? 'Sent to Pi — not confirmed' : 'Pi send not confirmed', { exact: true })).toBeVisible();
  await prompt.fill('another manual reply');
  await expect(send).toBeDisabled();
  await prompt.press('Control+Enter');
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Type in terminal', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await page.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual([]);
  expect((await commands(page)).filter(command => ['send_pi_prompt_unconfirmed', 'submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
  await reload();
  await expect(page.getByRole('button', { name: 'I checked — allow another send', exact: true })).toHaveCount(0);
  await prompt.fill('new draft after acknowledgement');
  await expect(send).toBeEnabled();
  expect((await commands(page)).filter(command => ['send_pi_prompt_unconfirmed', 'submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

test('followup direct TLS device rejection stops retrying', async ({ page }) => {
  const relay = { id: 'rejected-direct', label: 'Rejected direct', url: 'wss://relay.example.invalid', token: '', paired: true };
  await page.addInitScript(id => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: {
    [id]: { kind: 'credential', id: 'synthetic-credential', deviceId: 'synthetic-device', version: 1, secret: 'A'.repeat(43), role: 'controller', locale: 'en', issuedAt: Date.now() },
  } })), relay.id);
  await boot(page, [relay]);
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'e2ee_client_hello').length).toBe(1);
  const before = await page.evaluate(() => localStorage.getItem('herdr_device_auth_v1'));
  await page.evaluate(() => (window as any).__relayClose(0, 4401));
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByText('This computer refused this device. Import a new invitation link, or remove and add the relay.', { exact: true })).toBeVisible();
  await page.evaluate(() => {
    window.dispatchEvent(new Event('focus'));
    window.dispatchEvent(new Event('online'));
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.waitForTimeout(1500);
  expect(await socketCount(page)).toBe(1);
  expect((await commands(page)).map(command => command.type)).toEqual(['e2ee_client_hello']);
  expect(await page.evaluate(saved => localStorage.getItem('herdr_device_auth_v1') === saved, before)).toBe(true);
});

test('followup raw development close stays recoverable', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await page.evaluate(() => (window as any).__relayClose(0, 4401));
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByText('This computer refused this device. Import a new invitation link, or remove and add the relay.', { exact: true })).toHaveCount(0);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_secret', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

for (const role of ['controller', 'reader']) test(`followup Pi Integration setting is ${role === 'reader' ? 'read-only for readers' : 'controller-controlled with per-profile results'}`, async ({ page }) => {
  await boot(page, [fedora], '/', { answerHealthPings: true });
  await expect.poll(() => socketCount(page)).toBe(1);
  await page.evaluate(role => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: {
    kind: 'credential', id: 'synthetic-setting', version: 1, secret: 'S'.repeat(43), deviceId: 'synthetic-setting', role, locale: 'en', issuedAt: Date.now(),
  } } })), role);
  await setAutoCommands(page, false);
  await handshake(page, 0, { capabilities: ['pi_integration'] });
  await page.getByRole('button', { name: /^Settings(?:,|$)/ }).click();
  const last = async (type: string) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(page)).findLast(command => command.type === type)!;
  };
  const respond = (request: Record<string, unknown>, enabled: boolean) => server(page, 0, {
    type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: {
      instance: 'synthetic-setting-manager', enabled,
      profiles: [{ path: '/synthetic/pi/agent', status: enabled ? 'installed' : 'not installed' },
        { path: '/synthetic/refused/agent', status: 'refused', reason: 'Unowned directory' }],
    },
  });
  await respond(await last('get_pi_integration'), role === 'reader');
  const toggle = page.getByRole('switch', { name: 'Pi Integration', exact: true });
  await expect(page.getByText(/Reload Pi to activate changes/)).toBeVisible();
  await expect(page.getByText('/synthetic/refused/agent: refused — Unowned directory', { exact: true })).toBeVisible();
  if (role === 'reader') {
    await expect(toggle).toBeChecked();
    await expect(toggle).toBeDisabled();
    await toggle.evaluate(element => (element as HTMLButtonElement).click());
    await expect(page.getByText('Controller access required to change this setting.', { exact: true })).toBeVisible();
    expect((await commands(page)).filter(command => command.type === 'set_pi_integration')).toHaveLength(0);
  } else {
    await expect(toggle).not.toBeChecked();
    await expect(toggle).toBeEnabled();
    await toggle.click();
    const install = await last('set_pi_integration');
    expect(install.enabled).toBe(true);
    expect(install.instance).toBe('synthetic-setting-manager');
    expect(Object.keys(install).sort()).toEqual(['client_id', 'enabled', 'instance', 'protocol', 'request_id', 'type']);
    await respond(install, true);
    await expect(toggle).toBeChecked();
    await expect(page.getByText('/synthetic/pi/agent: installed', { exact: true })).toBeVisible();
    await toggle.click();
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'set_pi_integration').length).toBe(2);
    const remove = await last('set_pi_integration');
    expect(remove.enabled).toBe(false);
    await respond(remove, false);
    await expect(toggle).not.toBeChecked();
  }
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_pi_prompt_unconfirmed', 'send_keys', 'send_terminal_text'].includes(String(command.type)))).toHaveLength(0);
});

test('followup blocked Pi without a bridge points to Settings without fallback input', async ({ page }) => {
  await boot(page, [fedora], '/', { answerHealthPings: true });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'pi_integration', 'manual_terminal_input'] });
  await setConversationFixture(page, { entries: [{ id: 'setting-notice', role: 'assistant', kind: 'notice', text: 'Synthetic extension question', timestamp: '' }], total: 1 });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, agent_session_id: 'setting-pi', status: 'blocked', attention_kind: 'unknown', project: 'Setting Pi', agent: 'pi', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Setting Pi on Fedora', exact: true }).click();
  await expect(page.getByText('For live Pi replies, enable Settings → Pi Integration, then /reload Pi.', { exact: true })).toBeVisible();
  await expectNoActivityFooter(page.locator('.prompt-composer'));
  await expect(page.getByRole('button', { name: 'Type in terminal', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await expect(page.getByText('For live Pi replies, enable Settings → Pi Integration, then /reload Pi.', { exact: true })).toBeVisible();
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_pi_prompt_unconfirmed', 'send_keys', 'send_terminal_text'].includes(String(command.type)))).toHaveLength(0);
});

test('hidden secret requires correlated observation and never retries uncertain delivery across views', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['secret_input', 'manual_terminal_input', 'conversation_history'] });
  await setConversationFixture(page, { entries: [{ id: 'secret-history', role: 'assistant', timestamp: '', text: 'Synthetic secret input history' }], total: 1 });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Secret safety', agent: 'codex', agent_session_id: 'secret-safety-session', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Secret safety on Fedora' }).click();
  const input = page.getByLabel('Value for the hidden terminal prompt');
  const send = page.getByRole('button', { name: 'Send hidden value' });
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: 'Password:', no_echo: true });
  await expect(input).toBeDisabled();
  expect((await commands(page)).filter(command => command.type === 'send_secret')).toHaveLength(0);
  const observe = async () => {
    const previous = (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id;
    await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(previous);
    const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, content: 'Password:', no_echo: true });
  };
  await observe();
  await expect(input).toBeEnabled();
  await input.fill('synthetic-browser-value');
  await page.evaluate(() => (window as unknown as { __relayAutoCommands: (enabled: boolean) => void }).__relayAutoCommands(false));
  await send.click();
  await expect(input).toHaveValue('');
  const secret = (await commands(page)).find(command => command.type === 'send_secret')!;
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await expect(send).toBeDisabled();
  await expect(page.getByText(/Delivery is uncertain/)).toBeVisible({ timeout: 20_000 });
  await server(page, 0, { type: 'command_result', request_id: secret.request_id, phase: 'completed', ok: true });
  await page.getByRole('button', { name: 'Type in terminal', exact: true }).click();
  await observe();
  await expect(page.getByRole('button', { name: 'I checked the terminal', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Back to prompt and structured controls', exact: true }).click();
  await expect(send).toBeDisabled();
  await expect(input).toHaveValue('');
  await input.evaluate(element => { element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); });
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await expect(page.getByText(/Delivery is uncertain/)).toBeVisible();
  await expect(send).toBeDisabled();
  expect((await commands(page)).filter(command => command.type === 'send_secret')).toHaveLength(1);
  expect(await page.evaluate(() => JSON.stringify(localStorage) + JSON.stringify(sessionStorage))).not.toContain('synthetic-browser-value');
});

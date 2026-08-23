import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setTerminalHistoryLines, setTerminalRefreshInterval } from '$lib/preferences';
import { relayStore, type CommandError } from '$lib/store';
import type { RelayTransport, TransportHandlers, TransportStatus, TransportStatusDetail } from '$lib/transports';
import type { RelayConfig, RelayWorkspace } from '$lib/types';
import { pendingRelayUpdate } from '$lib/updates';

type TransportFactory = (relay: RelayConfig, handlers: TransportHandlers) => RelayTransport;

/**
 * Lets one test swap in a transport the store cannot otherwise reach, such as
 * a path that reports a fatal failure. Every other test keeps the real
 * WebSocket transport and the MockWebSocket below.
 */
const transportHijack = vi.hoisted(() => ({ current: null as TransportFactory | null }));

vi.mock('$lib/transports', async (importOriginal) => {
  const actual = await importOriginal() as { createRelayTransport: TransportFactory };
  return {
    ...actual,
    createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers) =>
      (transportHijack.current ?? actual.createRelayTransport)(relay, handlers),
  };
});

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];
  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  constructor(readonly url: string, readonly protocols?: string | string[]) { MockWebSocket.instances.push(this); }
  send(payload: string) { this.sent.push(payload); }
  close() { this.readyState = MockWebSocket.CLOSED; }
  open() { this.readyState = MockWebSocket.OPEN; this.onopen?.(); }
  message(payload: unknown) { this.onmessage?.({ data: JSON.stringify(payload) }); }
  serverClose() { this.readyState = MockWebSocket.CLOSED; this.onclose?.(); }
}

describe('relay command store', () => {
  beforeEach(() => {
    transportHijack.current = null;
    MockWebSocket.instances = [];
    sessionStorage.clear();
    vi.stubGlobal('WebSocket', MockWebSocket);
    relayStore.destroy();
    setTerminalRefreshInterval(250);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });
  });

  afterEach(() => {
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('preserves relay URLs, protocol v2 command shapes, and confirmations', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    expect(socket.url).toBe('wss://fedora.example');
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'agent_rename', pane_id: 'w1:p1', name: '123' });
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'agent_rename', pane_id: 'w1:p1', name: '123', protocol: 2 });
    expect(command.client_id).toBeTruthy();
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'confirmed' });
  });

  it('stores empty workspaces and sends workspace and worktree commands', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['workspace_management', 'workspace_reorder_block', 'worktree_management'],
      agent_profiles: [],
      inventory: { state: 'ready' },
    });
    socket.message({
      type: 'workspaces',
      workspaces: [{
        workspace_id: 'w1',
        number: 1,
        label: 'Project',
        pane_count: 1,
        tab_count: 1,
        cwd: '/home/user/project',
        worktree: {
          repo_key: 'repo',
          repo_name: 'project',
          repo_root: '/home/user/project',
          checkout_path: '/home/user/project',
          is_linked_worktree: false,
        },
      }, {
        workspace_id: 'w2',
        number: 2,
        label: 'Empty',
        pane_count: 1,
        tab_count: 1,
        cwd: '/home/user/empty',
      }],
    });
    expect(get(relayStore.workspaces)).toEqual([
      expect.objectContaining({ workspace_id: 'w1', label: 'Project', cwd: '/home/user/project' }),
      expect.objectContaining({ workspace_id: 'w2', label: 'Empty', cwd: '/home/user/empty' }),
    ]);

    const workspace = get(relayStore.workspaces)[0];
    const rename = relayStore.renameWorkspace(workspace, 'Renamed');
    const renameCommand = JSON.parse(socket.sent.at(-1)!);
    expect(renameCommand).toMatchObject({
      type: 'workspace_rename',
      workspace_id: 'w1',
      label: 'Renamed',
      protocol: 2,
    });
    socket.message({
      type: 'command_result',
      request_id: renameCommand.request_id,
      action: 'workspace_rename',
      ok: true,
      phase: 'completed',
    });
    await expect(rename).resolves.toMatchObject({ ok: true });

    const reorder = relayStore.reorderWorkspaceBlock(workspace.relay_id, ['w1', 'w2'], 'w5', 2);
    const reorderCommand = JSON.parse(socket.sent.at(-1)!);
    expect(reorderCommand).toMatchObject({
      type: 'workspace_reorder',
      workspace_ids: ['w1', 'w2'],
      before_workspace_id: 'w5',
      protocol: 2,
    });
    socket.message({
      type: 'command_result',
      request_id: reorderCommand.request_id,
      action: 'workspace_reorder',
      ok: true,
      phase: 'completed',
    });
    await expect(reorder).resolves.toMatchObject({ ok: true });

    socket.message({
      type: 'push_config',
      protocol: 2,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['workspace_management', 'worktree_management'],
      agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const legacyReorder = relayStore.reorderWorkspaceBlock(workspace.relay_id, ['w1'], '', 2);
    const legacyCommand = JSON.parse(socket.sent.at(-1)!);
    expect(legacyCommand).toMatchObject({
      type: 'workspace_reorder',
      workspace_id: 'w1',
      insert_index: 2,
      protocol: 2,
    });
    expect(legacyCommand).not.toHaveProperty('workspace_ids');
    socket.message({
      type: 'command_result',
      request_id: legacyCommand.request_id,
      action: 'workspace_reorder',
      ok: true,
      phase: 'completed',
    });
    await expect(legacyReorder).resolves.toMatchObject({ ok: true });

    const listing = relayStore.listWorktrees(workspace);
    const listCommand = JSON.parse(socket.sent.at(-1)!);
    expect(listCommand).toMatchObject({ type: 'worktree_list', workspace_id: 'w1', protocol: 2 });
    socket.message({
      type: 'command_result',
      request_id: listCommand.request_id,
      action: 'worktree_list',
      ok: true,
      phase: 'completed',
      data: {
        source: {
          repo_key: 'repo',
          repo_name: 'project',
          repo_root: '/home/user/project',
          source_checkout_path: '/home/user/project',
          source_workspace_id: 'w1',
        },
        worktrees: [],
      },
    });
    await expect(listing).resolves.toMatchObject({ source: { repo_name: 'project' }, worktrees: [] });
  });

  it('keeps relay keys out of the WebSocket URL and waits for encrypted authentication', async () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    relayStore.addRelay({
      label: 'Fedora',
      url: 'wss://fedora.example/ws?region=test',
      token: '0123456789abcdef0123456789abcdef',
    });
    const socket = MockWebSocket.instances.at(-1)!;
    expect(socket.url).toBe('wss://fedora.example/ws?region=test');
    expect(socket.protocols).toBe('herdr-e2ee-v1');
    socket.open();
    await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
    const hello = JSON.parse(socket.sent[0]);
    expect(hello).toMatchObject({ type: 'e2ee_client_hello', version: 1 });
    expect(socket.sent[0]).not.toContain('0123456789abcdef0123456789abcdef');
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(get(relayStore.connections).get(relayId)?.status).toBe('connecting');
    expect(relayStore.sendRaw(relayId, { type: 'refresh_agents' })).toBe(false);
  });

  it('acquires and releases validated pane-size leases for capable relays', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['pane_size_lease'],
      agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', agent: 'omp' }],
    });
    const agent = get(relayStore.agents)[0];

    const lease = relayStore.leasePaneSize(agent, 83, 32);
    const acquire = socket.sent.map((payload) => JSON.parse(payload))
      .findLast((message) => message.type === 'lease_pane_size');
    expect(acquire).toMatchObject({
      type: 'lease_pane_size',
      pane_id: 'w1:p1',
      columns: 83,
      protocol: 2,
    });
    expect(acquire.request_id).toBeTruthy();
    expect(acquire).not.toHaveProperty('client_id');
    // The relay does not advertise row support: rows must not be sent, and
    // no applied height may be believed.
    expect(acquire).not.toHaveProperty('rows');
    socket.message({
      type: 'command_result',
      action: 'lease_pane_size',
      request_id: acquire.request_id,
      ok: true,
      data: { columns: 83 },
    });
    await expect(lease).resolves.toEqual({ columns: 83, rows: 0 });

    const release = relayStore.releasePaneSize(agent);
    const releaseCommand = socket.sent.map((payload) => JSON.parse(payload))
      .findLast((message) => message.type === 'release_pane_size');
    expect(releaseCommand).toMatchObject({
      type: 'release_pane_size',
      pane_id: 'w1:p1',
      protocol: 2,
    });
    expect(releaseCommand.request_id).toBeTruthy();
    expect(releaseCommand).not.toHaveProperty('client_id');
    socket.message({
      type: 'command_result',
      action: 'release_pane_size',
      request_id: releaseCommand.request_id,
      ok: true,
    });
    await expect(release).resolves.toBeUndefined();
    await expect(relayStore.leasePaneSize(agent, 39)).rejects.toThrow(/between 40 and 240/);
  });

  it('leases rows only when the relay advertises row support', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['pane_size_lease', 'pane_size_lease_rows'],
      agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', agent: 'omp' }],
    });
    const agent = get(relayStore.agents)[0];

    const lease = relayStore.leasePaneSize(agent, 83, 32);
    const acquire = socket.sent.map((payload) => JSON.parse(payload))
      .findLast((message) => message.type === 'lease_pane_size');
    expect(acquire).toMatchObject({
      type: 'lease_pane_size',
      pane_id: 'w1:p1',
      columns: 83,
      rows: 32,
      protocol: 2,
    });
    socket.message({
      type: 'command_result',
      action: 'lease_pane_size',
      request_id: acquire.request_id,
      ok: true,
      // Another client holds a shorter lease: the applied height wins.
      data: { columns: 83, rows: 30 },
    });
    await expect(lease).resolves.toEqual({ columns: 83, rows: 30 });
    await expect(relayStore.leasePaneSize(agent, 80, 9)).rejects.toThrow(/between 10 and 120/);
  });

  it('replaces a stale connecting attempt on revalidation but keeps a young one', () => {
    // The socket never opens: the connection sits in 'connecting', like a
    // dial started before the phone's radio came back after sleep.
    expect(MockWebSocket.instances).toHaveLength(1);
    vi.useFakeTimers();

    // Focus events fire on every app switch; a young dial must not be churned.
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(1);

    // Once the revalidation event postdates the dial by the stale threshold,
    // the blackholed attempt is replaced instead of waiting out its own
    // handshake timeout plus reconnect backoff.
    vi.advanceTimersByTime(5_000);
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('binds approvals to their blocked event and coalesces pane polling', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora',
      capabilities: ['attention_classification'], agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1', status: 'blocked', event_id: 'event-1', agent: 'codex',
        attention_kind: 'approval', options: ['Approve once', 'Deny'],
      }],
    });
    const agent = get(relayStore.agents)[0];

    relayStore.readPane(agent);
    relayStore.readPane(agent);
    expect(socket.sent.map((payload) => JSON.parse(payload)).filter((message) => message.type === 'read_pane')).toHaveLength(1);
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', content: 'blocked', format: 'ansi' });
    relayStore.readPane(agent);
    expect(socket.sent.map((payload) => JSON.parse(payload)).filter((message) => message.type === 'read_pane')).toHaveLength(2);

    const approval = relayStore.respond(agent, 0, 2, 'Approve once');
    const command = socket.sent.map((payload) => JSON.parse(payload)).findLast((message) => message.type === 'respond');
    expect(command).toMatchObject({ pane_id: 'w1:p1', event_id: 'event-1', index: 0, total: 2 });
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(approval).resolves.toBe(true);
  });

  it('does not trust blocked controls from an old relay', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'old', host: 'fedora',
      capabilities: ['structured_questions'], agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1', status: 'blocked', event_id: 'event-1', agent: 'codex',
        options: ['Approve once', 'Deny'],
        interaction: {
          id: 'old-question', kind: 'single_select', question: 'Trust this?',
          options: [{ index: 0, label: 'Yes' }],
        },
        question_layout: true,
      }],
    });
    const agent = get(relayStore.agents)[0];
    expect(agent).toMatchObject({
      attention_kind: 'unknown',
      attention_capable: false,
      interaction: null,
      question_layout: false,
    });
    expect(agent.options).toBeUndefined();
  });

  it('deletes persisted activity through its relay and clears the merged view', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, host: 'fedora',
      capabilities: ['clear_activities'], agent_profiles: [],
    });
    socket.message({
      type: 'activity_history',
      activities: [{ id: 'activity-1', timestamp: 123, summary: 'Prompt sent' }],
    });
    expect(get(relayStore.activities)).toHaveLength(1);

    const pending = relayStore.clearActivities();
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'clear_activities', protocol: 2 });
    socket.message({
      type: 'command_result', request_id: command.request_id,
      action: 'clear_activities', ok: true, phase: 'completed',
    });

    await expect(pending).resolves.toBeUndefined();
    expect(get(relayStore.activities)).toEqual([]);
  });

  it('rejects mutations on protocol mismatch', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 1, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    await expect(relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' })).rejects.toThrow(/protocol v1/);
  });

  it('retains agents, blocks mutations, and recovers when inventory becomes ready', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      host: 'fedora',
      capabilities: [],
      agent_profiles: [],
      inventory: { state: 'ready', last_attempt_at: 100, last_success_at: 100 },
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Relay', agent: 'codex' }],
    });
    socket.message({
      type: 'inventory_status',
      state: 'error',
      error_code: 'protocol_mismatch',
      message: 'Run `herdr server live-handoff` on this computer, then refresh.',
      last_attempt_at: 123,
      last_success_at: 100,
      stale: true,
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const before = socket.sent.length;

    await expect(
      relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' }),
    ).rejects.toThrow(/live-handoff/);
    expect(socket.sent).toHaveLength(before);
    expect(get(relayStore.agents)).toHaveLength(1);
    expect(get(relayStore.connections).get(relayId)?.inventory).toMatchObject({
      state: 'error',
      errorCode: 'protocol_mismatch',
      stale: true,
    });

    socket.message({
      type: 'inventory_status',
      state: 'error',
      error_code: 'protocol_mismatch',
      message: 'Run `herdr server live-handoff` on this computer, then refresh.',
      last_attempt_at: 124,
      last_success_at: 0,
      stale: false,
    });
    socket.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toHaveLength(1);

    socket.message({
      type: 'inventory_status',
      state: 'ready',
      error_code: '',
      message: '',
      last_attempt_at: 200,
      last_success_at: 200,
      stale: false,
    });
    const pending = relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' });
    const command = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true });
    await expect(pending).resolves.toMatchObject({ ok: true });
    expect(get(relayStore.connections).get(relayId)?.inventory.state).toBe('ready');
  });

  it('checks and schedules an exact relay update across a protocol mismatch', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 99,
      version: 'abc123',
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'available',
        current_version: '0.7.0',
        current_revision: 'abc123',
        available_version: '0.8.0',
        available_revision: 'f'.repeat(12),
        target_revision: 'f'.repeat(40),
        can_install: true,
        mode: 'local',
      },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const checking = relayStore.checkRelayUpdate(relayId);
    const checkCommand = JSON.parse(socket.sent.at(-1)!);
    expect(checkCommand).toMatchObject({ type: 'check_update', protocol: 2 });
    socket.message({
      type: 'command_result',
      request_id: checkCommand.request_id,
      ok: true,
      phase: 'confirmed',
      data: {
        update: {
          state: 'available',
          current_version: '0.7.0',
          available_version: '0.8.0',
          target_revision: 'f'.repeat(40),
          can_install: true,
          mode: 'local',
        },
      },
    });
    await checking;

    const installing = relayStore.installRelayUpdate(relayId);
    const installCommand = JSON.parse(socket.sent.at(-1)!);
    expect(installCommand).toMatchObject({
      type: 'install_update',
      expected_version: '0.8.0',
      expected_revision: 'f'.repeat(40),
      expected_origin: location.origin,
      protocol: 2,
    });
    socket.message({
      type: 'command_result',
      request_id: installCommand.request_id,
      ok: true,
      phase: 'scheduled',
      data: { update: { state: 'scheduled', target_revision: 'f'.repeat(40) } },
    });
    await installing;

    expect(pendingRelayUpdate(relayId)).toEqual({
      version: '0.8.0',
      revision: 'f'.repeat(40),
    });
  });

  it('clears a pending relay update after an explicit scheduling failure', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'available',
        available_version: '0.8.0',
        target_revision: 'e'.repeat(40),
        can_install: true,
      },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const installing = relayStore.installRelayUpdate(relayId);
    const command = JSON.parse(socket.sent.at(-1)!);

    socket.message({
      type: 'command_result',
      request_id: command.request_id,
      ok: false,
      phase: 'failed',
      error: 'Update changed',
      data: { update: { state: 'failed', error: 'Update changed' } },
    });

    await expect(installing).rejects.toThrow('Update changed');
    expect(pendingRelayUpdate(relayId)).toBeNull();
  });

  it('requests a fresh agent snapshot on connect and on demand', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({ type: 'refresh_agents' });

    relayStore.requestAgents();
    expect(socket.sent.map((payload) => JSON.parse(payload).type)).toEqual([
      'refresh_agents',
      'refresh_agents',
    ]);
  });

  it('privately registers the hosting origin when running as an installed app', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    vi.stubGlobal('matchMedia', vi.fn(() => ({
      matches: true,
      media: '(display-mode: standalone)',
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })));
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });

    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const sent = socket.sent.map((payload) => JSON.parse(payload));

    expect(sent).toEqual([
      {
        type: 'register_app_origin',
        origin: location.origin,
        protocol: 2,
      },
      { type: 'refresh_agents' },
    ]);
  });

  it('requests the default and configured numbers of terminal history lines', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    };

    relayStore.readPane(agent);
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'read_pane',
      pane_id: 'w1:p1',
      lines: 1_000,
      format: 'ansi',
      content_fingerprint: '',
    });

    setTerminalHistoryLines(500);
    relayStore.readPane(agent, true);
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'read_pane',
      pane_id: 'w1:p1',
      lines: 500,
      format: 'ansi',
      content_fingerprint: '',
    });
    setTerminalHistoryLines(1_000);
  });

  it('applies acknowledged terminal deltas and resynchronizes mismatched bases', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      capabilities: ['pane_realtime_delta'],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent);
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'one\ntwo\nthree\nfour\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });

    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      lines: 1_000,
      interval_ms: 250,
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    setTerminalRefreshInterval(100);
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      interval_ms: 100,
      content_fingerprint: 'content-1',
    });
    setTerminalRefreshInterval(250);
    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      format: 'ansi',
      base_fingerprint: 'content-1',
      content_fingerprint: 'content-1',
      segments: null,
      question_layout: true,
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)?.content)
      .toBe('one\ntwo\nthree\nfour\n');
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'pane_applied',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-1',
    });

    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      format: 'ansi',
      base_fingerprint: 'content-1',
      content_fingerprint: 'content-2',
      segments: [
        { copy_lines: 4 },
        { text: 'five\n' },
      ],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)?.content)
      .toBe('one\ntwo\nthree\nfour\nfive\n');
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'pane_applied',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-2',
    });

    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      base_fingerprint: 'missing-base',
      content_fingerprint: 'content-3',
      segments: [{ text: 'invalid' }],
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'read_pane',
      pane_id: 'w1:p1',
      content_fingerprint: '',
    });
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'resynchronized\n',
      format: 'ansi',
      content_fingerprint: 'content-3',
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-3',
    });
  });

  it('merges agents from independent relays without pane id collisions', () => {
    relayStore.addRelay({ label: 'Mac', url: 'wss://mac.example', token: '' });
    const [fedora, mac] = MockWebSocket.instances.slice(-2);
    fedora.open();
    mac.open();
    fedora.message({ type: 'push_config', protocol: 2, inventory: { state: 'ready' } });
    mac.message({ type: 'push_config', protocol: 2, inventory: { state: 'ready' } });
    fedora.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app' }] });
    mac.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Mac app' }] });
    const agents = get(relayStore.agents);
    expect(agents).toHaveLength(2);
    expect(new Set(agents.map((agent) => agent.pane_id)).size).toBe(2);
    expect(agents.map((agent) => agent.project).sort()).toEqual(['Fedora app', 'Mac app']);
  });

  it('ignores blocked and agent update frames older than the pane revision', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, inventory: { state: 'ready' } });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', pane_revision: 12, project: 'Current' }],
    });

    socket.message({
      type: 'blocked', pane_id: 'w1:p1', status: 'blocked',
      pane_revision: 11, event_id: 'stale-blocked',
    });
    socket.message({
      type: 'agent_update', pane_id: 'w1:p1', status: 'idle', pane_revision: 10,
    });

    expect(get(relayStore.agents)[0]).toMatchObject({
      status: 'working',
      pane_revision: 12,
      project: 'Current',
    });
  });

  it('accepts a fresh low pane revision after a relay reconnect', () => {
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.message({ type: 'push_config', protocol: 2, inventory: { state: 'ready' } });
    first.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', pane_revision: 100 }],
    });

    relayStore.connectAll();
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 2, inventory: { state: 'ready' } });
    replacement.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'blocked', pane_revision: 1, event_id: 'fresh' }],
    });

    expect(get(relayStore.agents)[0]).toMatchObject({
      status: 'blocked',
      pane_revision: 1,
      event_id: 'fresh',
    });
  });

  it('stores terminal frames without agent-specific relay metadata', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'output', format: 'ansi',
      desktop_footer_lines: 6, desktop_prompt_lines: 2,
    });

    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toEqual({
      paneId: `${relayId}::w1:p1`,
      content: 'output',
      format: 'ansi',
    });
  });

  it('preserves Herdr scrollback truncation on terminal frames', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'clipped output', format: 'ansi',
      truncated: true,
    });

    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      content: 'clipped output',
      truncated: true,
    });
  });

  it('updates Herdr scrollback truncation on terminal deltas', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'one\n', format: 'ansi',
      content_fingerprint: 'content-1', truncated: false, viewport_only: true,
      viewport_rows: 46,
    });
    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'content-1', content_fingerprint: 'content-2',
      truncated: true, segments: [{ copy_lines: 1 }, { text: 'two\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      truncated: true,
      viewportOnly: true,
      viewportRows: 46,
    });

    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'content-2', content_fingerprint: 'content-3',
      truncated: false, segments: [{ copy_lines: 1 }, { text: 'three\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      truncated: false,
      viewportOnly: true,
      viewportRows: 46,
    });
  });

  it('carries a recognized no-echo prompt on full frames and deltas', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: '[sudo] password for cv: ', format: 'ansi',
      content_fingerprint: 'secret-1', no_echo: true, no_echo_prompt: '[sudo] password for cv:',
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      noEcho: true,
      noEchoPrompt: '[sudo] password for cv:',
    });

    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'secret-1', content_fingerprint: 'secret-2',
      no_echo: false, segments: [{ text: 'done\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      noEcho: false,
      noEchoPrompt: undefined,
    });
  });

  it('sends a secret only to a relay that advertises secret input', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
    };
    await expect(relayStore.sendSecret(agent, 'hunter2')).rejects.toThrow(/does not support password prompts/);

    socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: ['secret_input'], agent_profiles: [] });
    await expect(relayStore.sendSecret(agent, '')).rejects.toThrow(/Enter the password/);

    const pending = relayStore.sendSecret(agent, 'hunter2');
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'send_secret', pane_id: 'w1:p1', text: 'hunter2' });
    expect(typeof command.request_id).toBe('string');
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true });
    await expect(pending).resolves.toMatchObject({ ok: true });
  });

  it('ignores late events from a socket that has already been replaced', async () => {
    const oldSocket = MockWebSocket.instances.at(-1)!;
    oldSocket.open();
    oldSocket.message({ type: 'push_config', protocol: 2, version: 'old', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    relayStore.connectAll();
    const currentSocket = MockWebSocket.instances.at(-1)!;
    currentSocket.open();
    currentSocket.message({ type: 'push_config', protocol: 2, version: 'new', host: 'fedora', capabilities: [], agent_profiles: [] });
    currentSocket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Current agent' }] });
    const pending = relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' });
    const command = JSON.parse(currentSocket.sent.at(-1)!);

    oldSocket.message({ type: 'agents', agents: [] });
    oldSocket.serverClose();

    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Current agent']);
    currentSocket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true });
  });

  it('keeps the last agent snapshot until a reconnected relay sends a fresh one', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'old', host: 'fedora', capabilities: [], agent_profiles: [] });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume safely' }] });

    socket.serverClose();
    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Resume safely']);

    await vi.advanceTimersByTimeAsync(3_000);
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 2, version: 'new', host: 'fedora', capabilities: [], agent_profiles: [] });
    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Resume safely']);

    replacement.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toEqual([]);
  });

  it('replaces a half-open socket when its foreground health probe receives no traffic', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });

    relayStore.revalidateConnections(25);
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('refresh_agents');
    await vi.advanceTimersByTimeAsync(24);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('gives a foregrounded page two seconds to answer its app-level ping', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });

    relayStore.revalidateConnections();
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('refresh_agents');
    await vi.advanceTimersByTimeAsync(1_999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('waits longer for the ping while the page is hidden', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });

      relayStore.revalidateConnections();
      await vi.advanceTimersByTimeAsync(9_999);
      expect(MockWebSocket.instances).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(MockWebSocket.instances).toHaveLength(2);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('keeps every connected relay warm with a proof-of-life ping', async () => {
    vi.useFakeTimers();
    relayStore.addRelay({ label: 'Debian', url: 'wss://debian.example', token: '' });
    // Adding a relay redials both; keep only the two live sockets so a later
    // count means "nothing was redialed".
    const [connected, dialing] = MockWebSocket.instances.slice(-2);
    MockWebSocket.instances = [connected, dialing];
    expect(dialing.url).toBe('wss://debian.example');
    connected.open();
    connected.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
    const sentOnConnect = connected.sent.length;

    await vi.advanceTimersByTimeAsync(119_999);
    expect(connected.sent).toHaveLength(sentOnConnect);
    await vi.advanceTimersByTimeAsync(1);
    expect(JSON.parse(connected.sent.at(-1)!).type).toBe('refresh_agents');
    expect(connected.sent).toHaveLength(sentOnConnect + 1);
    // The keepalive proves a live path; it never dials one, so the relay still
    // completing its handshake hears nothing.
    expect(dialing.sent).toEqual([]);

    // The reply doubles as the health signal: an answered ping keeps the socket
    // past the gateway's five-minute idle reaper.
    connected.message({ type: 'inventory_status', state: 'ready' });
    await vi.advanceTimersByTimeAsync(120_000);
    expect(connected.sent).toHaveLength(sentOnConnect + 2);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('drops a connection that misses its keepalive while hidden and leaves the dial to resume', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
      const relayId = get(relayStore.relayConfigs)[0].id;

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
      relayStore.setHidden(true);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(JSON.parse(socket.sent.at(-1)!).type).toBe('refresh_agents');

      // Ten seconds of silence after the ping: the path is gone.
      await vi.advanceTimersByTimeAsync(10_000);
      expect(socket.readyState).toBe(MockWebSocket.CLOSED);
      expect(get(relayStore.connections).get(relayId)?.status).toBe('disconnected');
      // No socket churn and no radio churn for a page nobody is looking at.
      expect(MockWebSocket.instances).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(600_000);
      expect(MockWebSocket.instances).toHaveLength(1);

      // Resume dials at once: the relay is not connected, so there is nothing
      // to probe.
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
      relayStore.setHidden(false);
      relayStore.revalidateConnections(2_000);
      expect(MockWebSocket.instances).toHaveLength(2);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('stops pinging after an hour hidden and starts again when the app comes back', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
      const pings = () => socket.sent.filter((payload) => JSON.parse(payload).type === 'refresh_agents').length;
      const onConnect = pings();

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
      relayStore.setHidden(true);
      // Answer every ping: a healthy hidden page holds its socket for the hour.
      for (let tick = 0; tick < 29; tick += 1) {
        await vi.advanceTimersByTimeAsync(120_000);
        socket.message({ type: 'inventory_status', state: 'ready' });
      }
      expect(pings()).toBe(onConnect + 29);

      // An hour hidden: the radio wakeups stop being worth it and the socket
      // is left to lapse on its own.
      await vi.advanceTimersByTimeAsync(120_000);
      await vi.advanceTimersByTimeAsync(600_000);
      expect(pings()).toBe(onConnect + 29);

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
      relayStore.setHidden(false);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(pings()).toBe(onConnect + 30);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('dials a stale resumed connection at once and only probes a fresh one', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    // Fresh proof of life: keep the session and spend one probe on it, so an
    // app switch never churns a healthy direct path.
    relayStore.revalidateConnections(2_000);
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('refresh_agents');
    expect(MockWebSocket.instances).toHaveLength(1);
    socket.message({ type: 'inventory_status', state: 'ready' });
    expect(relayStore.connection(relayId)?.healthTimer).toBeNull();

    // A silence a throttled hidden tab could still produce stays inside the
    // bound: a healthy connection must never be redialed for being slow.
    vi.setSystemTime(Date.now() + 240_000);
    relayStore.revalidateConnections(2_000);
    expect(MockWebSocket.instances).toHaveLength(1);
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('refresh_agents');
    socket.message({ type: 'inventory_status', state: 'ready' });
    const beforeResume = socket.sent.length;

    // A frozen page runs no timers while the clock keeps moving, which is the
    // gap `setSystemTime` reproduces here. The keepalive cannot have run, so
    // the silence is proof the path died with the freeze.
    vi.setSystemTime(Date.now() + 240_001);
    relayStore.revalidateConnections(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);
    // Dialed on the spot, with no probe spent on the corpse.
    expect(socket.sent).toHaveLength(beforeResume);
  });

  it('stamps the last-heard time on inbound traffic and clears the pending probe', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.open();
    // A completed handshake is itself proof of life.
    expect(relayStore.connection(relayId)?.lastMessageAt).toBe(Date.now());

    await vi.advanceTimersByTimeAsync(1_000);
    relayStore.revalidateConnections(2_000);
    expect(relayStore.connection(relayId)?.healthTimer).not.toBeNull();
    socket.message({ type: 'push_config', protocol: 2, host: 'fedora', capabilities: [], agent_profiles: [] });
    expect(relayStore.connection(relayId)?.lastMessageAt).toBe(Date.now());
    expect(relayStore.connection(relayId)?.healthTimer).toBeNull();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('replaces a half-open socket after the relay announces its update restart', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'installing',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
      },
    });
    socket.message({
      type: 'update_status',
      update: {
        state: 'restarting',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
      },
    });

    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
    expect(socket.readyState).toBe(MockWebSocket.CLOSED);
  });

  it('does not restart a WebSocket handshake that is still connecting', () => {
    relayStore.revalidateConnections(25);

    expect(MockWebSocket.instances).toHaveLength(1);
    expect(MockWebSocket.instances[0].readyState).toBe(MockWebSocket.CONNECTING);
    expect(MockWebSocket.instances[0].sent).toEqual([]);
  });

  it('backs repeated reconnect attempts off after the first retry', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.serverClose();

    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);

    const second = MockWebSocket.instances.at(-1)!;
    second.serverClose();
    await vi.advanceTimersByTimeAsync(1_999);
    expect(MockWebSocket.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(3);
  });

  it('drops the reconnect backoff so a resumed page retries at once', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.serverClose();
    await vi.advanceTimersByTimeAsync(1_000);
    MockWebSocket.instances.at(-1)!.serverClose();
    expect(MockWebSocket.instances).toHaveLength(2);

    relayStore.resetReconnectBackoff();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);

    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(3);

    // The cleared attempt counter also puts the next failure back on the base delay.
    MockWebSocket.instances.at(-1)!.serverClose();
    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(4);
  });

  it('retries a fatal failure at the slow cadence instead of stranding', async () => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let attempts = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        attempts += 1;
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => false,
      close: () => {},
    });
    relayStore.addRelay({ label: 'Gateway', url: 'wss://gateway.example', token: '' });
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(attempts).toBe(1);

    // A fatal close waits out the full minute: retrying sooner only burns
    // battery, but never retrying strands the phone until a manual reload.
    report('closed', { reason: 'Relay key rejected', fatal: true });
    expect(get(relayStore.connections).get(relayId)?.status).toBe('disconnected');
    await vi.advanceTimersByTimeAsync(59_999);
    expect(attempts).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(attempts).toBe(2);
  });

  it('keeps the normal cadence when the gateway does not know the relay yet', async () => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let attempts = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        attempts += 1;
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => false,
      close: () => {},
    });
    relayStore.addRelay({ label: 'Gateway', url: '', token: 'k', transport: 'hybrid', gatewayUrl: 'wss://gw.example' });
    expect(attempts).toBe(1);

    // `unknown_relay` is what a gateway answers while a relay restarts and its
    // registration lapses; the phone must be back the moment it re-registers.
    report('closed', {
      reason: 'That computer is not connected to the gateway.',
      fatal: true,
      code: 'unknown_relay',
    });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(attempts).toBe(2);
  });

  it('rereads watched panes as soon as the relay reconnects', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, capabilities: ['pane_realtime_delta'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent as never);
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'before the drop\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({ type: 'watch_pane', pane_id: 'w1:p1' });

    // The connection dies while the terminal stays open; reads and watches
    // sent in the gap are lost. The reconnect must revive the stream itself,
    // not leave it to the fifteen-second resync interval.
    socket.serverClose();
    await vi.advanceTimersByTimeAsync(1_000);
    const replacement = MockWebSocket.instances.at(-1)!;
    expect(replacement).not.toBe(socket);
    replacement.open();
    const sent = replacement.sent.map((payload) => JSON.parse(payload) as Record<string, unknown>);
    expect(sent.some((message) => message.type === 'read_pane' && message.pane_id === 'w1:p1')).toBe(true);
  });

  it('honors terminal refresh while traffic is relayed and caps its history', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    setTerminalRefreshInterval(100);
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    let deliver: (message: Record<string, any>) => void = () => {};
    const sent: Record<string, unknown>[] = [];
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        report = handlers.onStatus;
        deliver = handlers.onMessage;
        handlers.onStatus('connecting');
      },
      send: (payload) => {
        sent.push(payload);
        return true;
      },
      close: () => {},
    });
    relayStore.addRelay({ label: 'Gateway', url: '', token: '', transport: 'hybrid', gatewayUrl: 'wss://gw.example' });
    const relayId = get(relayStore.relayConfigs)[0].id;

    report('connected', { path: 'gateway' });
    deliver({ type: 'push_config', protocol: 2, capabilities: ['pane_realtime_delta'], agent_profiles: [] });
    expect(get(relayStore.connections).get(relayId)?.path).toBe('gateway');

    const agent = {
      relay_id: relayId,
      relay_label: 'Gateway',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent as never);
    deliver({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'one\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    // Acknowledged deltas make the selected cadence affordable on the metered
    // path; only scrollback remains capped.
    expect(sent.at(-1)).toMatchObject({ type: 'watch_pane', interval_ms: 100, lines: 1_000 });

    // Promotion to the direct path keeps the same user-selected cadence.
    report('connected', { path: 'webrtc' });
    expect(get(relayStore.connections).get(relayId)?.path).toBe('webrtc');
    expect(sent.at(-1)).toMatchObject({ type: 'watch_pane', interval_ms: 100 });
  });

  it('records the gateway that answered and drops it on the relay URL path', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => true,
      close: () => {},
    });
    relayStore.addRelay({
      label: 'Gateway',
      url: 'wss://fedora.example',
      token: '',
      transport: 'hybrid',
      gatewayUrl: 'wss://a.example',
      gatewayUrls: ['wss://a.example', 'wss://b.example'],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    // The head of the list was skipped, so the answer is the dialed entry.
    report('connected', { path: 'gateway', gatewayUrl: 'wss://b.example' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('wss://b.example');

    report('connected', { path: 'webrtc', gatewayUrl: 'wss://b.example' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('wss://b.example');

    // The legacy relay URL carries no gateway, so nothing may still name one.
    report('connected', { path: 'websocket' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('');
  });

  it('adopts an advertised hybrid descriptor without a QR re-scan', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(get(relayStore.relayConfigs)[0].transport).toBeUndefined();

    socket.message({
      type: 'push_config',
      protocol: 2,
      capabilities: [],
      agent_profiles: [],
      release_version: '0.17.0',
      update: { state: 'current', upstream_version: '0.17.1' },
      hybrid: {
        transport: 'herdr-hybrid-v1',
        gateway_url: 'wss://gw.example',
        gateway_urls: [
          'wss://gw.example',
          'wss://backup.example',
          'https://not-a-websocket.example',
          'wss://backup.example',
        ],
        gateway_version: '0.17.0',
        gateway_revision: 'gateway-revision',
        gateway_available_version: '0.17.1',
        relay_id: 'Ccy3nT9AULlAceTEnhTvoQ',
        direct: true,
      },
    });

    const stored = get(relayStore.relayConfigs).find((entry) => entry.id === relayId)!;
    expect(stored.transport).toBe('hybrid');
    expect(stored.gatewayUrl).toBe('wss://gw.example');
    expect(stored.gatewayUrls).toEqual(['wss://gw.example', 'wss://backup.example']);
    expect(get(relayStore.connections).get(relayId)).toMatchObject({
      gatewayVersion: '0.17.0',
      gatewayAvailableVersion: '0.17.1',
    });
    // The legacy URL survives so the hybrid path can fall back to it.
    expect(stored.url).toBe('wss://fedora.example');
    expect(JSON.parse(localStorage.getItem('herdr_relays')!)).toContainEqual(
      expect.objectContaining({
        transport: 'hybrid',
        gatewayUrl: 'wss://gw.example',
        gatewayUrls: ['wss://gw.example', 'wss://backup.example'],
      }),
    );

    socket.message({
      type: 'push_config',
      protocol: 2,
      capabilities: [],
      agent_profiles: [],
      hybrid: {
        transport: 'herdr-hybrid-v1',
        gateway_url: 'wss://backup.example',
        gateway_urls: ['wss://backup.example', 'wss://gw.example'],
        relay_id: 'Ccy3nT9AULlAceTEnhTvoQ',
        direct: true,
      },
    });
    const reordered = get(relayStore.relayConfigs).find((entry) => entry.id === relayId)!;
    expect(reordered.gatewayUrl).toBe('wss://backup.example');
    expect(reordered.gatewayUrls).toEqual(['wss://backup.example', 'wss://gw.example']);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('ignores a hybrid descriptor that does not name a WebSocket gateway', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 2,
      capabilities: [],
      agent_profiles: [],
      hybrid: { transport: 'herdr-hybrid-v1', gateway_url: 'https://gw.example' },
    });
    expect(get(relayStore.relayConfigs)[0].transport).toBeUndefined();
  });

  it('rejects an image upload when its relay disconnects', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const upload = relayStore.uploadImage({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    }, new File(['png'], 'shot.png', { type: 'image/png' }));
    await vi.waitFor(() => expect(socket.sent.some((payload) => JSON.parse(payload).type === 'upload_image')).toBe(true));

    socket.serverClose();
    await expect(upload).rejects.toThrow('Relay disconnected');
  });

  it('times out image uploads that receive no result', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const upload = relayStore.uploadImage({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    }, new File(['png'], 'shot.png', { type: 'image/png' }), 5);

    await expect(upload).rejects.toThrow('Image upload did not finish in time');
  });

  it('accepts an upload result only from the relay that received the image', async () => {
    relayStore.addRelay({ label: 'Mac', url: 'wss://mac.example', token: 'secret' });
    const [fedoraSocket, macSocket] = MockWebSocket.instances.slice(-2);
    fedoraSocket.open();
    macSocket.open();
    fedoraSocket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    macSocket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'mac', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs).find((relay) => relay.label === 'Fedora')!.id;
    const upload = relayStore.uploadImage({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
    }, new File(['png'], 'shot.png', { type: 'image/png' }));
    await vi.waitFor(() => expect(fedoraSocket.sent.some((payload) => JSON.parse(payload).type === 'upload_image')).toBe(true));
    const request = fedoraSocket.sent.map((payload) => JSON.parse(payload)).find((message) => message.type === 'upload_image');
    let settled = false;
    void upload.then(() => { settled = true; }, () => { settled = true; });

    macSocket.message({ type: 'upload_result', request_id: request.request_id, ok: true, path: '/wrong/shot.png' });
    await Promise.resolve();
    expect(settled).toBe(false);

    fedoraSocket.message({ type: 'upload_result', request_id: request.request_id, ok: true, path: '/right/shot.png' });
    await expect(upload).resolves.toBe('/right/shot.png');
  });

  it('does not apply a directory result to a replacement connection', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: ['directory_browser'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const listing = relayStore.listDirectories(relayId, '/home/test');
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test', label: '~' }, parent: '/home', directories: [] },
    });
    relayStore.connectAll();

    await expect(listing).rejects.toThrow('Relay reconnected while loading directories');
    expect(get(relayStore.connections).get(relayId)?.directoryBrowser).toBeNull();
  });

  it('keeps the newest directory listing when responses arrive out of order', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: ['directory_browser'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const older = relayStore.listDirectories(relayId, '/home/test/older');
    const olderRequest = JSON.parse(socket.sent.at(-1)!);
    const newer = relayStore.listDirectories(relayId, '/home/test/newer');
    const newerRequest = JSON.parse(socket.sent.at(-1)!);

    socket.message({
      type: 'command_result', request_id: newerRequest.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test/newer', label: 'newer' }, parent: '/home/test', directories: [] },
    });
    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test/older', label: 'older' }, parent: '/home/test', directories: [] },
    });

    await expect(newer).resolves.toMatchObject({ current: { path: '/home/test/newer' } });
    await expect(older).resolves.toMatchObject({ current: { path: '/home/test/older' } });
    expect(get(relayStore.connections).get(relayId)?.directoryBrowser?.current.path).toBe('/home/test/newer');
    expect(get(relayStore.connections).get(relayId)?.directoryLoading).toBe(false);
  });

  it('keeps waiting for the final result once the relay accepts a command', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready', last_attempt_at: 100, last_success_at: 100 },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const pending = relayStore.sendCommand(relayId, { type: 'respond', pane_id: 'w1:p1', index: 0, total: 2 }, 12_000);
    const command = JSON.parse(socket.sent.at(-1)!);
    await vi.advanceTimersByTimeAsync(9_000);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'accepted' });

    // Past the original 12 second send timeout the request must still be open.
    await vi.advanceTimersByTimeAsync(5_000);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'confirmed' });
  });

  it('loads and caches slash commands for one agent identity', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/project',
    };

    const first = relayStore.loadSlashCommands(agent);
    const duplicate = relayStore.loadSlashCommands(agent);
    const requests = socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands');
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({ pane_id: 'w1:p1', protocol: 2 });
    socket.message({
      type: 'command_result', request_id: requests[0].request_id, ok: true, phase: 'completed',
      data: {
        commands: [
          { command: '/zeta', description: 'Last command', source: 'builtin' },
          { command: '/Alpha', description: 'First command', source: 'builtin' },
          { command: '/model', description: 'Choose model', source: 'builtin' },
        ],
        truncated: false,
      },
    });

    const alphabeticalCommands = [{ command: '/Alpha' }, { command: '/model' }, { command: '/zeta' }];
    await expect(first).resolves.toMatchObject({ commands: alphabeticalCommands });
    await expect(duplicate).resolves.toMatchObject({ commands: alphabeticalCommands });
    await expect(relayStore.loadSlashCommands(agent)).resolves.toMatchObject({ commands: alphabeticalCommands });
    expect(socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands')).toHaveLength(1);

    const changed = relayStore.loadSlashCommands({ ...agent, cwd: '/home/test/other' });
    const changedRequest = socket.sent.map((payload) => JSON.parse(payload)).at(-1)!;
    expect(changedRequest.type).toBe('list_slash_commands');
    socket.message({
      type: 'command_result', request_id: changedRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [], truncated: false },
    });
    await expect(changed).resolves.toEqual({ commands: [], truncated: false });
  });

  it('invalidates slash-command caches on reconnect and rejects unsupported relays', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'claude', cwd: '/home/test/project',
    };
    await expect(relayStore.loadSlashCommands(agent)).rejects.toThrow(/does not provide/);

    socket.message({ type: 'push_config', protocol: 2, capabilities: ['slash_commands'], agent_profiles: [] });
    const pending = relayStore.loadSlashCommands(agent);
    const request = socket.sent.map((payload) => JSON.parse(payload)).at(-1)!;
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true,
      data: { commands: [{ command: '/help', description: 'Help', source: 'builtin' }], truncated: false },
    });
    await pending;

    relayStore.connectAll();
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 2, capabilities: ['slash_commands'], agent_profiles: [] });
    const refreshed = relayStore.loadSlashCommands(agent);
    const refreshedRequest = JSON.parse(replacement.sent.at(-1)!);
    expect(refreshedRequest.type).toBe('list_slash_commands');
    replacement.message({
      type: 'command_result', request_id: refreshedRequest.request_id, ok: true,
      data: { commands: [], truncated: false },
    });
    await refreshed;
  });

  it('does not let an older slash-command response overwrite a newer identity cache', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agentOld = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/old',
    };
    const agentNew = { ...agentOld, cwd: '/home/test/new' };

    const olderPromise = relayStore.loadSlashCommands(agentOld);
    const olderRequest = socket.sent.map((p) => JSON.parse(p)).at(-1)!;
    const newerPromise = relayStore.loadSlashCommands(agentNew);
    const newerRequest = socket.sent.map((p) => JSON.parse(p)).at(-1)!;
    expect(olderRequest.request_id).not.toBe(newerRequest.request_id);

    socket.message({
      type: 'command_result', request_id: newerRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [{ command: '/new', description: 'New', source: 'builtin' }], truncated: false },
    });
    await newerPromise;

    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [{ command: '/old', description: 'Old', source: 'builtin' }], truncated: false },
    });
    await olderPromise;

    const cached = relayStore.loadSlashCommands(agentNew);
    await expect(cached).resolves.toMatchObject({ commands: [{ command: '/new' }] });
    const allRequests = socket.sent.map((p) => JSON.parse(p))
      .filter((m) => m.type === 'list_slash_commands');
    expect(allRequests).toHaveLength(2);
  });

  it('keeps a newer responding window when an older timer is cleared', async () => {
    vi.useFakeTimers();
    relayStore.markResponding('fedora::w1:p1');
    await vi.advanceTimersByTimeAsync(1_000);
    relayStore.clearResponding('fedora::w1:p1');
    relayStore.markResponding('fedora::w1:p1');

    await vi.advanceTimersByTimeAsync(9_000);
    expect(get(relayStore.responding).has('fedora::w1:p1')).toBe(true);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(get(relayStore.responding).has('fedora::w1:p1')).toBe(false);
  });

  it('tags a command lost to a disconnect after its frame was written as dispatched_unknown', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'submit_prompt', pane_id: 'w1:p1', text: 'ship it' });
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('submit_prompt');

    socket.serverClose();
    const error = await pending.then(() => null, (caught) => caught as CommandError);
    expect(error?.message).toBe('Relay disconnected');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('tags an accepted command whose confirmation never arrives as dispatched_unknown', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'submit_prompt', pane_id: 'w1:p1', text: 'ship it' });
    const command = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'accepted' });
    const outcome = pending.then(() => null, (caught) => caught as CommandError);

    await vi.advanceTimersByTimeAsync(10_000);
    const error = await outcome;
    expect(error?.message).toBe('Relay confirmation timed out');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('tags a written command that never hears back at all as dispatched_unknown', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const outcome = relayStore.sendCommand(relayId, { type: 'agent_rename', pane_id: 'w1:p1', name: 'renamed' })
      .then(() => null, (caught) => caught as CommandError);

    await vi.advanceTimersByTimeAsync(15_000);
    const error = await outcome;
    expect(error?.message).toBe('Relay confirmation timed out');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('keeps definitive pre-send failures plain and names the missing capability', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const workspace = {
      relay_id: relayId, relay_label: 'Fedora', workspace_id: 'w1', number: 1, label: 'Project',
      focused: false, pane_count: 1, tab_count: 1, active_tab_id: '', agent_status: '',
      cwd: '/home/user/project',
    } as RelayWorkspace;

    const workspaceError = await relayStore.createWorkspace(relayId, '/home/user/project', 'Project')
      .then(() => null, (caught) => caught as CommandError);
    expect(workspaceError?.message).toBe('This relay does not support workspace management');
    expect(workspaceError?.data?.dispatched_unknown).toBeUndefined();

    const worktreeError = await relayStore.listWorktrees(workspace)
      .then(() => null, (caught) => caught as CommandError);
    expect(worktreeError?.message).toBe('This relay does not support worktree management');
    expect(worktreeError?.data?.dispatched_unknown).toBeUndefined();

    relayStore.addRelay({ label: 'Mac', url: 'wss://mac.example', token: '' });
    const macId = get(relayStore.relayConfigs).find((relay) => relay.label === 'Mac')!.id;
    const offlineError = await relayStore.sendCommand(macId, { type: 'refresh_agents' })
      .then(() => null, (caught) => caught as CommandError);
    expect(offlineError?.message).toBe('Relay is not connected');
    expect(offlineError?.data?.dispatched_unknown).toBeUndefined();
  });
});

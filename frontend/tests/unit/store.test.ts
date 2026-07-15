import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { relayStore } from '$lib/store';

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
  constructor(readonly url: string) { MockWebSocket.instances.push(this); }
  send(payload: string) { this.sent.push(payload); }
  close() { this.readyState = MockWebSocket.CLOSED; }
  open() { this.readyState = MockWebSocket.OPEN; this.onopen?.(); }
  message(payload: unknown) { this.onmessage?.({ data: JSON.stringify(payload) }); }
  serverClose() { this.readyState = MockWebSocket.CLOSED; this.onclose?.(); }
}

describe('relay command store', () => {
  beforeEach(() => {
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: 'secret' });
  });

  afterEach(() => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('preserves auth URLs, protocol v2 command shapes, and confirmations', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    expect(socket.url).toBe('wss://fedora.example?token=secret');
    socket.open();
    socket.message({ type: 'push_config', protocol: 2, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'agent_rename', pane_id: 'w1:p1', name: 'renamed' });
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'agent_rename', pane_id: 'w1:p1', name: 'renamed', protocol: 2 });
    expect(command.client_id).toBeTruthy();
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'confirmed' });
  });

  it('rejects mutations on protocol mismatch', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 1, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    await expect(relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' })).rejects.toThrow(/protocol v1/);
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

  it('merges agents from independent relays without pane id collisions', () => {
    relayStore.addRelay({ label: 'Mac', url: 'wss://mac.example', token: 'secret' });
    const [fedora, mac] = MockWebSocket.instances.slice(-2);
    fedora.open();
    mac.open();
    fedora.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app' }] });
    mac.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Mac app' }] });
    const agents = get(relayStore.agents);
    expect(agents).toHaveLength(2);
    expect(new Set(agents.map((agent) => agent.pane_id)).size).toBe(2);
    expect(agents.map((agent) => agent.project).sort()).toEqual(['Fedora app', 'Mac app']);
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

    await vi.advanceTimersByTimeAsync(2_999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);

    const second = MockWebSocket.instances.at(-1)!;
    second.serverClose();
    await vi.advanceTimersByTimeAsync(5_999);
    expect(MockWebSocket.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(3);
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
});

import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';
import { MockWebSocket, connectRelay, makeAgent } from '../support/relay';

let socket: MockWebSocket;
let plainSocket: MockWebSocket;
let agent: Agent;
let plainAgent: Agent;

const frames = (target: MockWebSocket, type: string) => target.messages().filter(frame => frame.type === type);
const lastRead = (target: MockWebSocket) => frames(target, 'read_pane').at(-1)!;

function reconnect(relayId: string) {
  const relay = get(relayStore.relayConfigs).find(row => row.id === relayId)!;
  relayStore.disconnectRelay(relay.id);
  relayStore.connectRelay(relay);
  return connectRelay({ capabilities: ['pane_realtime_delta'] }, MockWebSocket.instances.at(-1)!);
}

beforeEach(() => {
  MockWebSocket.instances = [];
  vi.stubGlobal('WebSocket', MockWebSocket);
  localStorage.clear();
  sessionStorage.clear();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  relayStore.addRelay({ label: 'Streaming', url: 'ws://127.0.0.1:8871', token: '' });
  relayStore.addRelay({ label: 'Plain', url: 'ws://127.0.0.1:8872', token: '' });
  const [streaming, plain] = get(relayStore.relayConfigs);
  socket = connectRelay({ capabilities: ['pane_realtime_delta'] }, MockWebSocket.instances.at(-2)!);
  plainSocket = connectRelay({ capabilities: [] }, MockWebSocket.instances.at(-1)!);
  agent = makeAgent({ relay_id: streaming.id, raw_pane_id: 'w1:p1' });
  plainAgent = makeAgent({ relay_id: plain.id, raw_pane_id: 'w1:p1' });
});

afterEach(() => {
  relayStore.destroy();
  relayStore.relayConfigs.set([]);
  vi.unstubAllGlobals();
});

describe('pane_unchanged', () => {
  it('completes the outstanding read and starts a watch with the received fingerprint', () => {
    relayStore.watchPane(agent);
    relayStore.readPane(agent);
    relayStore.readPane(agent);
    expect(frames(socket, 'read_pane')).toHaveLength(1);

    socket.message({ type: 'pane_unchanged', request_id: lastRead(socket).request_id, pane_id: 'w1:p1', content_fingerprint: 'fp-1' });

    expect(frames(socket, 'watch_pane')).toEqual([expect.objectContaining({ pane_id: 'w1:p1', content_fingerprint: 'fp-1' })]);
    relayStore.readPane(agent);
    expect(frames(socket, 'read_pane')).toHaveLength(2);
  });

  it('completes the read of a listed pane nobody watches without starting a watch', () => {
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'terminal-1', generation: 1, agent_session_id: 'session-1', agent: 'codex' }] });
    const listed = get(relayStore.agents)[0];
    relayStore.readPane(listed);
    socket.message({ type: 'pane_unchanged', request_id: lastRead(socket).request_id, pane_id: 'w1:p1', content_fingerprint: 'fp-1' });
    expect(frames(socket, 'watch_pane')).toHaveLength(0);
    relayStore.readPane(listed);
    expect(frames(socket, 'read_pane')).toHaveLength(2);
  });
});

describe('pane_resync', () => {
  it('sends a forced read for a watched pane even while a read is outstanding', () => {
    relayStore.watchPane(agent);
    relayStore.readPane(agent);
    socket.message({ type: 'pane_content', request_id: lastRead(socket).request_id, pane_id: 'w1:p1', content: 'screen', content_fingerprint: 'fp-1' });
    relayStore.readPane(agent);
    expect(frames(socket, 'read_pane')).toHaveLength(2);

    socket.message({ type: 'pane_resync', pane_id: 'w1:p1' });

    expect(frames(socket, 'read_pane')).toHaveLength(3);
    expect(lastRead(socket)).toMatchObject({ pane_id: 'w1:p1', content_fingerprint: '' });
  });

  it('sends nothing for a pane nobody watches', () => {
    const before = socket.sent.length;
    socket.message({ type: 'pane_resync', pane_id: 'w1:p1' });
    expect(socket.sent).toHaveLength(before);
  });
});

describe('unwatchPane', () => {
  it('sends unwatch_pane on a relay with realtime deltas and stops the watch until watchPane is called again', () => {
    relayStore.watchPane(agent);
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', content: 'one', content_fingerprint: 'fp-1' });
    expect(frames(socket, 'watch_pane')).toHaveLength(1);

    relayStore.unwatchPane(agent);
    expect(frames(socket, 'unwatch_pane')).toEqual([expect.objectContaining({ pane_id: 'w1:p1' })]);

    socket.message({ type: 'pane_content', pane_id: 'w1:p1', content: 'two', content_fingerprint: 'fp-2' });
    expect(frames(socket, 'watch_pane')).toHaveLength(1);

    relayStore.watchPane(agent);
    expect(frames(socket, 'watch_pane')).toHaveLength(2);
    expect(frames(socket, 'watch_pane').at(-1)).toMatchObject({ content_fingerprint: 'fp-2' });
  });

  it('sends nothing on a relay without realtime deltas', () => {
    relayStore.watchPane(plainAgent);
    const before = plainSocket.sent.length;
    relayStore.unwatchPane(plainAgent);
    expect(plainSocket.sent).toHaveLength(before);
  });
});

describe('a read answered by another request', () => {
  it('is ignored until the answer carries the outstanding request id', () => {
    const pane = makeAgent({ relay_id: agent.relay_id, raw_pane_id: 'w9:p9' });
    relayStore.watchPane(pane);
    relayStore.readPane(pane);
    const requestId = lastRead(socket).request_id;

    socket.message({ type: 'pane_content', request_id: 'stale-request', pane_id: 'w9:p9', content: 'old', content_fingerprint: 'fp-old' });
    socket.message({ type: 'pane_unchanged', request_id: 'stale-request', pane_id: 'w9:p9', content_fingerprint: 'fp-old' });

    expect(get(relayStore.terminalFrames).has(pane.pane_id)).toBe(false);
    expect(frames(socket, 'watch_pane')).toHaveLength(0);
    relayStore.readPane(pane);
    expect(frames(socket, 'read_pane')).toHaveLength(1);

    socket.message({ type: 'pane_content', request_id: requestId, pane_id: 'w9:p9', content: 'fresh', content_fingerprint: 'fp-new' });

    expect(get(relayStore.terminalFrames).get(pane.pane_id)?.content).toBe('fresh');
    expect(frames(socket, 'watch_pane')).toEqual([expect.objectContaining({ content_fingerprint: 'fp-new' })]);
  });
});

describe('disconnecting the relay', () => {
  it('lets the next read send without waiting out the retry window', () => {
    relayStore.readPane(agent);
    relayStore.readPane(agent);
    expect(frames(socket, 'read_pane')).toHaveLength(1);

    const replacement = reconnect(agent.relay_id);
    relayStore.readPane(agent);

    expect(frames(replacement, 'read_pane')).toHaveLength(1);
  });

  it('starts a new watch after the relay reconnects', () => {
    relayStore.watchPane(agent);
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', content: 'one', content_fingerprint: 'fp-1' });
    expect(frames(socket, 'watch_pane')).toHaveLength(1);

    const replacement = reconnect(agent.relay_id);
    relayStore.watchPane(agent);
    replacement.message({ type: 'pane_unchanged', request_id: lastRead(replacement).request_id, pane_id: 'w1:p1', content_fingerprint: 'fp-2' });

    expect(frames(replacement, 'watch_pane')).toEqual([expect.objectContaining({ pane_id: 'w1:p1', content_fingerprint: 'fp-2' })]);
  });
});

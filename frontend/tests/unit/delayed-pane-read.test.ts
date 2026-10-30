import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setTerminalHistoryLines } from '$lib/preferences';
import { CommandError, relayStore } from '$lib/store';
import { createHybridTransport } from '$lib/transports/path-manager';
import type { RelayTransport, TransportAuthentication, TransportHandlers, TransportStatus, TransportStatusDetail } from '$lib/transports';
import type { Agent, CommandResult, QuestionDraft, QuestionInteraction, RelayConfig } from '$lib/types';

type Wire = Record<string, any>;
type TransportFactory = (relay: RelayConfig, handlers: TransportHandlers, authentication?: TransportAuthentication) => RelayTransport;
const transportHijack = vi.hoisted(() => ({ current: null as TransportFactory | null }));

vi.mock('$lib/transports', async (importOriginal) => {
  const actual = await importOriginal() as { createRelayTransport: TransportFactory };
  return {
    ...actual,
    createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers, authentication?: TransportAuthentication) =>
      (transportHijack.current ?? actual.createRelayTransport)(relay, handlers, authentication),
  };
});

interface Peer {
  messages(): Wire[];
  message(payload: Wire): void;
}

// The legacy tests still traverse the real WebSocket/encrypted-transport
// implementation's explicitly supported tokenless loopback plaintext path.
class MockWebSocket implements Peer {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];
  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onclose: ((event?: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  constructor(readonly url: string, readonly protocols?: string | string[]) { MockWebSocket.instances.push(this); }
  get protocol() { return typeof this.protocols === 'string' ? this.protocols : ''; }
  send(payload: string) {
    if (this.readyState !== MockWebSocket.OPEN) throw new Error('Synthetic socket is not open');
    this.sent.push(payload);
  }
  close() { this.readyState = MockWebSocket.CLOSED; }
  open() { this.readyState = MockWebSocket.OPEN; this.onopen?.(); }
  messages(): Wire[] { return this.sent.map(payload => JSON.parse(payload)); }
  message(payload: Wire) { this.onmessage?.({ data: JSON.stringify(payload) }); }
}

const interaction: QuestionInteraction = {
  id: 'question-1', kind: 'multi_select', question: 'Which checks?',
  options: [{ index: 0, label: 'Unit' }, { index: 1, label: 'Type' }, { index: 2, label: 'Lint' }],
  other: { label: 'Other' }, can_go_back: true, can_chat: true,
};
const draft: QuestionDraft = { selected: new Set([2, 0]), otherSelected: true, otherText: 'Keep exact spacing  ' };
const producers = [
  { name: 'respond', delay: 500, payload: {
    type: 'respond', index: 0, total: 2, choice: 'Approve once', source: 'App',
    event_id: 'event-1', approval_fingerprint: 'approval-1',
  }, invoke: (agent: Agent) => relayStore.respond(agent, 0, 2) },
  { name: 'answerQuestion', delay: 400, payload: {
    type: 'answer_question', interaction_id: interaction.id, selected_indices: [0, 2],
    other_selected: true, other_text: draft.otherText, source: 'App',
  }, invoke: (agent: Agent) => relayStore.answerQuestion(agent, interaction, draft) },
  { name: 'navigateQuestionPrevious', delay: 400, payload: {
    type: 'navigate_question', interaction_id: interaction.id, direction: 'previous', source: 'App',
  }, invoke: (agent: Agent) => relayStore.navigateQuestionPrevious(agent, interaction) },
  { name: 'clarifyQuestion', delay: 400, payload: {
    type: 'clarify_question', interaction_id: interaction.id, source: 'App',
  }, invoke: (agent: Agent) => relayStore.clarifyQuestion(agent, interaction) },
];
type Producer = typeof producers[number];

function target(agent: Agent) {
  return {
    relay_id: agent.relay_id, pane_id: agent.raw_pane_id, server_session_id: 'primary',
    terminal_id: `terminal-${agent.raw_pane_id}`, generation: 1, agent_session_id: 'session-1',
  };
}

function inventory(peer: Peer, producer: Producer): void {
  peer.message({
    type: 'push_config', protocol: 3, capabilities: ['attention_classification', 'structured_questions'],
    inventory: { state: 'ready' }, agent_profiles: [],
  });
  peer.message({
    type: 'agents', agents: ['w1:p1', 'w1:p2'].map(paneId => ({
      pane_id: paneId, server_session_id: 'primary', terminal_id: `terminal-${paneId}`,
      generation: 1, agent_session_id: 'session-1', agent: 'codex', status: 'blocked', event_id: 'event-1',
      ...(producer.name === 'respond'
        ? { attention_kind: 'approval', options: ['Approve once', 'Deny'], approval_fingerprint: 'approval-1' }
        : { attention_kind: 'question', interaction, question_layout: true }),
    })),
  });
}

function agentFor(relayId: string, paneId = 'w1:p1'): Agent {
  const agent = get(relayStore.agents).find(agent => agent.relay_id === relayId && agent.raw_pane_id === paneId)!;
  expect(agent).toBeDefined();
  return agent;
}

function legacyPeers(producer: Producer) {
  for (const port of [8871, 8872]) {
    relayStore.addRelay({ label: `Relay ${port}`, url: `ws://127.0.0.1:${port}`, token: '' });
  }
  // addRelay reconnects every configured relay: finish configuring both first.
  return get(relayStore.relayConfigs).map(relay => {
    const peer = MockWebSocket.instances.filter(socket => socket.url === relay.url).at(-1)!;
    peer.open();
    inventory(peer, producer);
    return { peer, relay, agent: agentFor(relay.id) };
  });
}

function reads(peer: Peer): Wire[] { return peer.messages().filter(message => message.type === 'read_pane'); }

function expectRead(peer: Peer, agent: Agent, count = 1, fingerprint = '', lines = 10_000): Wire {
  expect(reads(peer)).toHaveLength(count);
  const read = reads(peer).at(-1)!;
  expect(read).toEqual({
    type: 'read_pane', request_id: expect.any(String), pane_id: agent.raw_pane_id,
    lines, format: 'ansi', content_fingerprint: fingerprint, server_session_id: 'primary', target: target(agent),
  });
  return read;
}

function acknowledge(peer: Peer, agent: Agent, read: Wire, fingerprint = 'pane-after-first'): void {
  peer.message({
    type: 'pane_content', request_id: read.request_id, pane_id: agent.raw_pane_id, target: target(agent),
    content: 'Actual correlated pane reply', content_fingerprint: fingerprint, format: 'ansi', no_echo: false,
  });
  expect(get(relayStore.terminalFrames).get(agent.pane_id)).toMatchObject({
    target: target(agent), content: 'Actual correlated pane reply', readStartedAt: expect.any(Number),
  });
}

function start(producer: Producer, peer: Peer, agent: Agent) {
  const promise = producer.invoke(agent);
  const command = peer.messages().at(-1)!;
  expect(command).toEqual({
    ...producer.payload, pane_id: agent.raw_pane_id, server_session_id: 'primary', target: target(agent),
    request_id: expect.any(String), protocol: 3, client_id: expect.any(String),
  });
  expect(get(relayStore.responding).has(agent.pane_id)).toBe(true);
  return { promise, command };
}

function result(command: Wire, phase = 'confirmed'): CommandResult {
  return { type: 'command_result', request_id: command.request_id, ok: true, phase, data: { exact: 'result' } };
}

async function succeed(producer: Producer, peer: Peer, job: ReturnType<typeof start>, phase = 'confirmed') {
  const final = result(job.command, phase);
  peer.message(final);
  const value = await job.promise;
  if (producer.name === 'respond') expect(value).toBe(true);
  else expect(value).toEqual(final);
}

async function boundary(producer: Producer, peer: Peer, agent: Agent, lines = 10_000): Promise<Wire> {
  await vi.advanceTimersByTimeAsync(producer.delay - 1);
  expect(reads(peer)).toHaveLength(0);
  await vi.advanceTimersByTimeAsync(1);
  return expectRead(peer, agent, 1, '', lines);
}

async function staggeredPathRefreshes(producer: Producer, peer: Peer, first: Agent, second: Agent, lines: number): Promise<void> {
  // The first job settled 100ms before the path transition; the second command
  // was still pending across it. Both must retain their own delay and target.
  await vi.advanceTimersByTimeAsync(producer.delay - 101);
  expect(reads(peer)).toHaveLength(0);
  await vi.advanceTimersByTimeAsync(1);
  expectRead(peer, first, 1, '', lines);
  await vi.advanceTimersByTimeAsync(99);
  expect(reads(peer)).toHaveLength(1);
  await vi.advanceTimersByTimeAsync(1);
  expectRead(peer, second, 2, '', lines);
}

// Authenticated application-path peers at the production HybridTransport's
// documented factory seam. No real encryption, ICE, socket or STUN is claimed.
// Unlike a store-only status fixture, all wrapper status/filter/path decisions
// below are made by the actual production path manager.
class ApplicationPath implements RelayTransport, Peer {
  closed = false;
  ready = false;
  sent: Wire[] = [];
  constructor(readonly kind: 'gateway' | 'webrtc', readonly handlers: TransportHandlers, readonly gatewayUrl: string) {}
  connect(): void { this.status('connecting'); }
  close(): void { this.closed = true; this.ready = false; }
  send(payload: Wire): boolean {
    if (!this.ready || this.closed) return false;
    this.sent.push(structuredClone(payload));
    return true;
  }
  messages(): Wire[] { return this.sent; }
  message(payload: Wire): void { this.handlers.onMessage(payload); }
  status(status: TransportStatus, detail?: TransportStatusDetail): void {
    this.ready = status === 'connected';
    if (status === 'closed') this.closed = true;
    this.handlers.onStatus(status, detail);
  }
}

function hybridPeer(producer: Producer) {
  const gateways: ApplicationPath[] = [];
  const directs: ApplicationPath[] = [];
  const statuses: TransportStatus[] = [];
  let wrapper!: RelayTransport;
  transportHijack.current = (relay, handlers, authentication) => {
    wrapper = createHybridTransport(relay, {
      onMessage: message => handlers.onMessage(message),
      onStatus: (status, detail) => { statuses.push(status); handlers.onStatus(status, detail); },
    }, {
      createGateway: (relay, handlers) => {
        const path = new ApplicationPath('gateway', handlers, relay.gatewayUrl!);
        gateways.push(path);
        return path;
      },
      createDirect: (relay, _signal, handlers) => {
        const path = new ApplicationPath('webrtc', handlers, relay.gatewayUrl!);
        directs.push(path);
        return path;
      },
    }, authentication);
    return wrapper;
  };
  relayStore.addRelay({
    label: 'Hybrid', url: '', token: '', transport: 'hybrid',
    gatewayUrl: 'wss://gateway-a.invalid', gatewayUrls: ['wss://gateway-a.invalid', 'wss://gateway-b.invalid'],
  });
  const relay = get(relayStore.relayConfigs)[0];
  gateways[0].status('connected', { path: 'gateway' });
  inventory(gateways[0], producer);
  const owner = relayStore.connection(relay.id)!;
  expect(owner.status).toBe('connected');
  expect(owner.path).toBe('gateway');
  expect(owner.transport).toBe(wrapper);
  expect(directs).toHaveLength(1);
  return { gateways, directs, statuses, wrapper, relay, owner, agent: agentFor(relay.id) };
}

function sameOwner(fixture: ReturnType<typeof hybridPeer>, path: 'gateway' | 'webrtc'): void {
  expect(relayStore.connection(fixture.relay.id)).toBe(fixture.owner);
  expect(fixture.owner).toMatchObject({ closed: false, status: 'connected', path });
  expect(fixture.owner.transport).toBe(fixture.wrapper);
}

function promote(fixture: ReturnType<typeof hybridPeer>): void {
  fixture.directs[0].status('connected');
  // A handshake alone must not promote: a real relay snapshot is required.
  sameOwner(fixture, 'gateway');
  fixture.directs[0].message({
    type: 'push_config', protocol: 3, capabilities: ['attention_classification', 'structured_questions'],
    inventory: { state: 'ready' },
  });
  sameOwner(fixture, 'webrtc');
}

beforeEach(() => {
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  relayStore.toast.set(null);
  localStorage.clear();
  sessionStorage.clear();
  MockWebSocket.instances = [];
  transportHijack.current = null;
  vi.stubGlobal('WebSocket', MockWebSocket);
  vi.useFakeTimers();
  setTerminalHistoryLines(10_000);
});

afterEach(() => {
  relayStore.destroy();
  relayStore.relayConfigs.set([]);
  transportHijack.current = null;
  setTerminalHistoryLines(1_000);
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('delayed pane read current contract', () => {
  describe.each(producers)('$name', producer => {
    it('success refreshes live owner exactly at deadline', async () => {
      const [{ peer, agent }] = legacyPeers(producer);
      const job = start(producer, peer, agent);
      await succeed(producer, peer, job);
      if (producer.name === 'respond') expect(get(relayStore.toast)).toMatchObject({ message: 'Confirmed: Approve once', error: false });
      expect([...draft.selected]).toEqual([2, 0]);
      await boundary(producer, peer, agent);
    });

    it('failure retains result semantics and refreshes live owner', async () => {
      const [{ peer, agent }] = legacyPeers(producer);
      const job = start(producer, peer, agent);
      // Attach rejection handling before transport ingress, never fabricate a
      // pending record or replace the store's command/pane methods.
      const outcome = job.promise.then(value => value, error => error);
      peer.message({
        type: 'command_result', request_id: job.command.request_id, ok: false,
        phase: 'not_started', error: 'Synthetic refusal', data: { detail: 'retained' },
      });
      const value = await outcome;
      if (producer.name === 'respond') {
        expect(value).toBe(false);
        expect(get(relayStore.responding).has(agent.pane_id)).toBe(false);
        expect(get(relayStore.toast)).toMatchObject({ message: 'Synthetic refusal', error: true });
      } else {
        expect(value).toBeInstanceOf(CommandError);
        expect(value).toMatchObject({ message: 'Synthetic refusal', data: { detail: 'retained', not_started: true } });
      }
      await boundary(producer, peer, agent);
    });

    it('accepted is nonterminal and final settlement starts delay', async () => {
      const [{ peer, agent }] = legacyPeers(producer);
      const job = start(producer, peer, agent);
      const settled = vi.fn();
      void job.promise.then(settled);
      peer.message(result(job.command, 'accepted'));
      await vi.advanceTimersByTimeAsync(producer.delay + 73);
      expect(settled).not.toHaveBeenCalled();
      expect(reads(peer)).toHaveLength(0);
      expect(get(relayStore.toast)).toMatchObject({ message: 'Command accepted; waiting for agent…', error: false });
      await succeed(producer, peer, job);
      expect(settled).toHaveBeenCalledTimes(1);
      await boundary(producer, peer, agent);
    });

    it('staggered same-pane completions remain independent with correlated acknowledgments', async () => {
      const [{ peer, agent }] = legacyPeers(producer);
      const first = start(producer, peer, agent);
      const second = start(producer, peer, agent);
      expect(first.command.request_id).not.toBe(second.command.request_id);
      await succeed(producer, peer, first);
      await vi.advanceTimersByTimeAsync(100);
      await succeed(producer, peer, second);
      await vi.advanceTimersByTimeAsync(producer.delay - 101);
      expect(reads(peer)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1);
      const firstRead = expectRead(peer, agent);
      // Keep real coalescing: no second read while the first is outstanding.
      relayStore.readPane(agent);
      expect(reads(peer)).toHaveLength(1);
      acknowledge(peer, agent, firstRead);
      await vi.advanceTimersByTimeAsync(99);
      expect(reads(peer)).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      const secondRead = expectRead(peer, agent, 2, 'pane-after-first');
      expect(secondRead.request_id).not.toBe(firstRead.request_id);
      acknowledge(peer, agent, secondRead, 'pane-after-second');
    });

    it('separate panes and relays keep independent refreshes', async () => {
      const [a, b] = legacyPeers(producer);
      const otherPane = agentFor(a.relay.id, 'w1:p2');
      await succeed(producer, a.peer, start(producer, a.peer, a.agent));
      await vi.advanceTimersByTimeAsync(100);
      await succeed(producer, a.peer, start(producer, a.peer, otherPane));
      await vi.advanceTimersByTimeAsync(100);
      await succeed(producer, b.peer, start(producer, b.peer, b.agent));
      await vi.advanceTimersByTimeAsync(producer.delay - 201);
      expect(reads(a.peer)).toHaveLength(0);
      expect(reads(b.peer)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1);
      const first = expectRead(a.peer, a.agent);
      expect(reads(b.peer)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(99);
      expect(reads(a.peer)).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      const second = expectRead(a.peer, otherPane, 2);
      expect(reads(b.peer)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(99);
      expect(reads(b.peer)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1);
      const third = expectRead(b.peer, b.agent);
      expect(new Set([first.request_id, second.request_id, third.request_id]).size).toBe(3);
      expect(reads(a.peer)).toHaveLength(2);
    });

    it('real hybrid promotion preserves pending settlement and delayed refresh', async () => {
      const fixture = hybridPeer(producer);
      const gateway = fixture.gateways[0];
      await succeed(producer, gateway, start(producer, gateway, fixture.agent));
      await vi.advanceTimersByTimeAsync(100);
      const otherPane = agentFor(fixture.relay.id, 'w1:p2');
      const job = start(producer, gateway, otherPane);
      gateway.message(result(job.command, 'accepted'));
      promote(fixture);
      // The production wrapper admits correlated final results from its
      // draining gateway, but routes both delayed reads over the direct path.
      await succeed(producer, gateway, job);
      await staggeredPathRefreshes(producer, fixture.directs[0], fixture.agent, otherPane, 10_000);
      expect(reads(gateway)).toHaveLength(0);
      sameOwner(fixture, 'webrtc');
    });

    it('real hybrid ready fallback preserves pending settlement and delayed refresh', async () => {
      const fixture = hybridPeer(producer);
      promote(fixture);
      const direct = fixture.directs[0];
      await succeed(producer, direct, start(producer, direct, fixture.agent));
      await vi.advanceTimersByTimeAsync(100);
      const otherPane = agentFor(fixture.relay.id, 'w1:p2');
      const job = start(producer, direct, otherPane);
      direct.message(result(job.command, 'accepted'));
      direct.status('closed', { reason: 'Synthetic direct loss' });
      expect(fixture.statuses.slice(-2)).toEqual(['connecting', 'connected']);
      sameOwner(fixture, 'gateway');
      expect(direct.closed).toBe(true);
      await succeed(producer, fixture.gateways[0], job);
      await staggeredPathRefreshes(producer, fixture.gateways[0], fixture.agent, otherPane, 1_000);
      expect(reads(direct)).toHaveLength(0);
      sameOwner(fixture, 'gateway');
    });

    it('real hybrid gateway replacement preserves a settled job on the same wrapper', async () => {
      const fixture = hybridPeer(producer);
      const gateway = fixture.gateways[0];
      await succeed(producer, gateway, start(producer, gateway, fixture.agent));
      await vi.advanceTimersByTimeAsync(100);
      gateway.status('closed', { reason: 'Synthetic gateway loss' });
      expect(fixture.gateways).toHaveLength(2);
      expect(relayStore.connection(fixture.relay.id)).toBe(fixture.owner);
      expect(fixture.owner).toMatchObject({ closed: false, status: 'connecting' });
      const replacement = fixture.gateways[1];
      expect(replacement.gatewayUrl).toBe('wss://gateway-b.invalid');
      replacement.status('connected', { path: 'gateway' });
      sameOwner(fixture, 'gateway');
      expect(fixture.owner.activeGatewayUrl).toBe('wss://gateway-b.invalid');
      await vi.advanceTimersByTimeAsync(producer.delay - 101);
      expect(reads(replacement)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1);
      expectRead(replacement, fixture.agent, 1, '', 1_000);
      expect(reads(gateway)).toHaveLength(0);
    });

    it('real hybrid connecting at deadline suppresses delivery without inventing a retry', async () => {
      const fixture = hybridPeer(producer);
      const gateway = fixture.gateways[0];
      await succeed(producer, gateway, start(producer, gateway, fixture.agent));
      gateway.status('closed', { reason: 'Synthetic gateway loss' });
      expect(fixture.owner.status).toBe('connecting');
      await vi.advanceTimersByTimeAsync(producer.delay);
      expect(reads(gateway)).toHaveLength(0);
      const replacement = fixture.gateways[1];
      expect(reads(replacement)).toHaveLength(0);
      replacement.status('connected', { path: 'gateway' });
      sameOwner(fixture, 'gateway');
      await vi.advanceTimersByTimeAsync(producer.delay);
      expect(reads(replacement)).toHaveLength(0);
      // Suppressed delivery did not fabricate a pending read or retry policy.
      relayStore.readPane(fixture.agent);
      expectRead(replacement, fixture.agent, 1, '', 1_000);
    });
  });

  it('respond unconfirmed resolves true and refreshes at 500ms', async () => {
    const producer = producers[0];
    const [{ peer, agent }] = legacyPeers(producer);
    await succeed(producer, peer, start(producer, peer, agent), 'unconfirmed');
    expect(get(relayStore.toast)).toMatchObject({ message: 'Accepted; agent still appears blocked.', error: false });
    await boundary(producer, peer, agent);
  });

  it('respond invalid index or unverified approval returns false without refresh', async () => {
    const [{ peer, agent }] = legacyPeers(producers[0]);
    const before = peer.messages();
    await expect(relayStore.respond(agent, -1, 2)).resolves.toBe(false);
    expect(get(relayStore.toast)).toBeNull();
    await expect(relayStore.respond({ ...agent, approval_fingerprint: '' }, 0, 2, 'Approve once')).resolves.toBe(false);
    await expect(relayStore.respond({ ...agent, options: [] }, 0, 2)).resolves.toBe(false);
    expect(get(relayStore.toast)).toMatchObject({ message: 'This approval no longer has an exact verified identity.', error: true });
    expect(get(relayStore.responding).has(agent.pane_id)).toBe(false);
    await vi.advanceTimersByTimeAsync(500);
    expect(peer.messages()).toEqual(before);
  });
});

describe('delayed pane read lifetime', () => {
  type LifetimeAllocation = {
    handle: ReturnType<typeof setTimeout>;
    callback: (...args: any[]) => void;
    args: any[];
    allocatedAt: number;
  };

  // Observe real fake-clock handles, forwarding every argument. Successful
  // production clears, throwing attempts and manual test cleanup are separate.
  function lifetimeTimers(delay: number, duringAllocation?: () => void) {
    const lifetimeNativeSet = globalThis.setTimeout;
    const lifetimeNativeClear = globalThis.clearTimeout;
    const lifetimeAllocations: LifetimeAllocation[] = [];
    const lifetimeAttempts: LifetimeAllocation['handle'][] = [];
    const lifetimeCanceled: LifetimeAllocation['handle'][] = [];
    const lifetimeTestCleanup: LifetimeAllocation['handle'][] = [];
    const lifetimeFault: { current: { handle: LifetimeAllocation['handle']; error: Error } | null } = { current: null };
    const lifetimeSetHook = vi.spyOn(globalThis, 'setTimeout').mockImplementation((callback, ms, ...args) => {
      const lifetimeHandle = lifetimeNativeSet(callback, ms, ...args);
      if (ms === delay && typeof callback === 'function') {
        lifetimeAllocations.push({ handle: lifetimeHandle, callback, args, allocatedAt: Date.now() });
        // Allocate first, then reenter public teardown, then return the actual
        // handle. Never invoke the callback in a candidate const-timer TDZ.
        duringAllocation?.();
      }
      return lifetimeHandle;
    });
    const lifetimeClearHook = vi.spyOn(globalThis, 'clearTimeout').mockImplementation((...args) => {
      const lifetimeMatch = lifetimeAllocations.find(allocation => allocation.handle === args[0]);
      if (lifetimeMatch) lifetimeAttempts.push(lifetimeMatch.handle);
      if (lifetimeFault.current && args[0] === lifetimeFault.current.handle) throw lifetimeFault.current.error;
      const lifetimeReturn = lifetimeNativeClear(...args);
      if (lifetimeMatch) lifetimeCanceled.push(lifetimeMatch.handle);
      return lifetimeReturn;
    });
    return {
      allocations: lifetimeAllocations, attempts: lifetimeAttempts, canceled: lifetimeCanceled,
      testCleanup: lifetimeTestCleanup, fault: lifetimeFault,
      restore() {
        lifetimeSetHook.mockRestore();
        lifetimeClearHook.mockRestore();
        for (const allocation of lifetimeAllocations) {
          lifetimeNativeClear(allocation.handle);
          lifetimeTestCleanup.push(allocation.handle);
        }
      },
    };
  }

  function lifetimeOne(clock: ReturnType<typeof lifetimeTimers>): LifetimeAllocation {
    expect(clock.allocations, 'SETUP_REAL_DELAYED_ALLOCATION').toHaveLength(1);
    return clock.allocations[0];
  }

  function lifetimeReady(relay: RelayConfig, producer: Producer) {
    const lifetimePeer = MockWebSocket.instances.filter(socket => socket.url === relay.url).at(-1)!;
    expect(lifetimePeer, 'SETUP_SOCKET_CREATED').toBeDefined();
    lifetimePeer.open();
    inventory(lifetimePeer, producer);
    const lifetimeAgent = agentFor(relay.id);
    const lifetimeOwner = relayStore.connection(relay.id)!;
    expect(lifetimeOwner, 'SETUP_READY_OWNER').toMatchObject({ closed: false, status: 'connected' });
    expect(lifetimePeer.readyState, 'SETUP_READY_SOCKET').toBe(MockWebSocket.OPEN);
    expect(reads(lifetimePeer), 'SETUP_NO_PRIOR_PANE_READ').toHaveLength(0);
    return { peer: lifetimePeer, relay, agent: lifetimeAgent, owner: lifetimeOwner };
  }

  function lifetimeSingle(producer: Producer) {
    relayStore.addRelay({ label: 'Lifetime relay', url: 'ws://127.0.0.1:8873', token: '' });
    expect(get(relayStore.relayConfigs), 'SETUP_SINGLE_RELAY').toHaveLength(1);
    return lifetimeReady(get(relayStore.relayConfigs)[0], producer);
  }

  function lifetimeReplace(relay: RelayConfig, producer: Producer) {
    relayStore.disconnectRelay(relay.id);
    relayStore.connectRelay(relay);
    return lifetimeReady(relay, producer);
  }

  function lifetimeTerminalClose(peer: MockWebSocket): void {
    expect(peer.onclose, 'SETUP_ACTUAL_TERMINAL_HANDLER').toBeTypeOf('function');
    peer.readyState = MockWebSocket.CLOSED;
    peer.onclose?.({ code: 1006 });
  }

  function lifetimeObserve(job: ReturnType<typeof start>): Promise<{ value?: boolean | CommandResult; error?: unknown }> {
    // Attach both continuations before any ingress/retirement that can reject.
    return job.promise.then(value => ({ value }), error => ({ error }));
  }

  function lifetimeRejected(producer: Producer, outcome: Awaited<ReturnType<typeof lifetimeObserve>>): void {
    if (producer.name === 'respond') {
      expect(outcome, 'SETUP_RESPOND_RETIRED_RESULT').toEqual({ value: false });
      return;
    }
    expect(outcome.error, 'SETUP_QUESTION_RETIRED_ERROR').toBeInstanceOf(CommandError);
    expect(outcome.error, 'SETUP_QUESTION_RETIRED_REASON').toMatchObject({ message: 'Relay disconnected' });
  }

  async function lifetimeFreshRead(producer: Producer, fixture: ReturnType<typeof lifetimeReady>): Promise<void> {
    await vi.advanceTimersByTimeAsync(producer.delay);
    expect(reads(fixture.peer), 'LIFETIME_NO_INHERITED_READ').toHaveLength(0);
    relayStore.readPane(fixture.agent);
    expectRead(fixture.peer, fixture.agent);
  }

  for (const producer of producers) {
    describe(producer.name, () => {
      it('settled job cannot refresh same-ID replacement', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          const lifetimeReplacement = lifetimeReplace(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_SAME_ID_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          expect(target(lifetimeReplacement.agent), 'SETUP_EXACT_SAME_TARGET').toEqual(target(lifetimeFixture.agent));
          await vi.advanceTimersByTimeAsync(producer.delay - 1);
          expect(reads(lifetimeReplacement.peer), 'SETUP_BEFORE_OLD_DEADLINE').toHaveLength(0);
          await vi.advanceTimersByTimeAsync(1);
          // Primary oracle: actual misdelivery, not disconnected suppression or
          // a prepopulated pending read. Cancellation is deliberately second.
          expect(reads(lifetimeReplacement.peer), 'L01_NO_SAME_ID_MISDELIVERY').toHaveLength(0);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L01_PHYSICAL_CANCEL').toHaveLength(1);
          relayStore.readPane(lifetimeReplacement.agent);
          expectRead(lifetimeReplacement.peer, lifetimeReplacement.agent);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('remove and re-add cannot inherit old job', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          relayStore.removeRelay(lifetimeFixture.relay.id);
          expect(get(relayStore.relayConfigs), 'SETUP_REMOVED_RELAY').toHaveLength(0);
          relayStore.addRelay(lifetimeFixture.relay);
          const lifetimeReplacement = lifetimeReady(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_READD_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          expect(target(lifetimeReplacement.agent), 'SETUP_READD_SAME_TARGET').toEqual(target(lifetimeFixture.agent));
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L02_PHYSICAL_CANCEL').toHaveLength(1);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('destroy and recreate cannot inherit old job', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          relayStore.destroy();
          relayStore.addRelay(lifetimeFixture.relay);
          const lifetimeReplacement = lifetimeReady(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_RECREATE_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          expect(target(lifetimeReplacement.agent), 'SETUP_RECREATE_SAME_TARGET').toEqual(target(lifetimeFixture.agent));
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L03_PHYSICAL_CANCEL').toHaveLength(1);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('terminal close and recovery cannot inherit old job', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          lifetimeTerminalClose(lifetimeFixture.peer);
          expect(lifetimeFixture.owner.status, 'SETUP_TERMINAL_STATUS_INGRESS').toBe('disconnected');
          expect(relayStore.connection(lifetimeFixture.relay.id), 'SETUP_TERMINAL_RECORD_RETAINED').toBe(lifetimeFixture.owner);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L04_PHYSICAL_CANCEL').toHaveLength(1);
          relayStore.connectRelay(lifetimeFixture.relay);
          const lifetimeReplacement = lifetimeReady(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_TERMINAL_RECOVERY_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('disconnect before settlement cannot register late finally', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          const lifetimeJob = start(producer, lifetimeFixture.peer, lifetimeFixture.agent);
          const lifetimeOutcome = lifetimeObserve(lifetimeJob);
          expect(lifetimeClock.allocations, 'SETUP_UNRESOLVED_NO_DELAY').toHaveLength(0);
          relayStore.disconnectRelay(lifetimeFixture.relay.id);
          lifetimeRejected(producer, await lifetimeOutcome);
          expect(lifetimeClock.allocations, 'L05_NO_LATE_ALLOCATION').toHaveLength(0);
          relayStore.connectRelay(lifetimeFixture.relay);
          await lifetimeFreshRead(producer, lifetimeReady(lifetimeFixture.relay, producer));
        } finally {
          lifetimeClock.restore();
        }
      });

      it('successful settlement followed by replacement before continuation keeps original owner', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          const lifetimeJob = start(producer, lifetimeFixture.peer, lifetimeFixture.agent);
          const lifetimeOutcome = lifetimeObserve(lifetimeJob);
          const lifetimeFinal = result(lifetimeJob.command);
          lifetimeFixture.peer.message(lifetimeFinal);
          // NO await/succeed/timer flush between real final ingress and owner
          // replacement: the producer continuation must still be queued.
          const lifetimeReplacement = lifetimeReplace(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_SYNC_REPLACED_OWNER').not.toBe(lifetimeFixture.owner);
          const lifetimeSettled = await lifetimeOutcome;
          expect(lifetimeSettled, 'SETUP_SYNC_FINAL_RESULT_PRESERVED').toEqual({ value: producer.name === 'respond' ? true : lifetimeFinal });
          expect(lifetimeClock.allocations, 'L06_NO_REPLACED_CONTINUATION_ALLOCATION').toHaveLength(0);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('hidden health retirement cannot register late finally', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          const lifetimeConnectedAt = Date.now();
          relayStore.setHidden(true);
          await vi.advanceTimersByTimeAsync(119_900);
          const lifetimeJob = start(producer, lifetimeFixture.peer, lifetimeFixture.agent);
          const lifetimeOutcome = lifetimeObserve(lifetimeJob);
          expect(lifetimeClock.allocations, 'SETUP_HEALTH_UNRESOLVED_NO_DELAY').toHaveLength(0);
          // Withhold ALL ingress, including accepted/final/pong. Any ingress
          // would clear healthTimer; the responding indicator is unrelated.
          await vi.advanceTimersByTimeAsync(100);
          expect(Date.now() - lifetimeConnectedAt, 'SETUP_ACTUAL_KEEPALIVE_TICK').toBe(120_000);
          expect(lifetimeFixture.peer.messages().filter(message => message.type === 'ping'), 'SETUP_KEEPALIVE_PING_SENT').toHaveLength(1);
          expect(lifetimeFixture.owner.healthTimer, 'SETUP_HEALTH_TIMER_ARMED').not.toBeNull();
          await vi.advanceTimersByTimeAsync(9_999);
          expect(lifetimeFixture.owner.status, 'SETUP_HEALTH_NOT_RETIRED_EARLY').toBe('connected');
          await vi.advanceTimersByTimeAsync(1);
          expect(Date.now() - lifetimeConnectedAt, 'SETUP_HEALTH_DEADLINE').toBe(130_000);
          expect(relayStore.connection(lifetimeFixture.relay.id), 'SETUP_HEALTH_RETAINED_OWNER').toBe(lifetimeFixture.owner);
          expect(lifetimeFixture.owner, 'SETUP_ACTUAL_HEALTH_RETIREMENT').toMatchObject({ closed: true, status: 'disconnected', healthTimer: null });
          expect(lifetimeFixture.peer.readyState, 'SETUP_HEALTH_TRANSPORT_CLOSED').toBe(MockWebSocket.CLOSED);
          lifetimeRejected(producer, await lifetimeOutcome);
          expect(lifetimeClock.allocations, 'L07_NO_HEALTH_LATE_ALLOCATION').toHaveLength(0);
          relayStore.setHidden(false);
          relayStore.revalidateConnections();
          const lifetimeReplacement = lifetimeReady(lifetimeFixture.relay, producer);
          expect(lifetimeReplacement.owner, 'SETUP_HEALTH_RECOVERY_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          expect(Date.now() - lifetimeConnectedAt, 'SETUP_RECOVERY_BEFORE_FINALLY_DELAY').toBe(130_000);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('canceled callback replay cannot deliver', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          const lifetimeReplacement = lifetimeReplace(lifetimeFixture.relay, producer);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L08_GENUINE_CANCEL_BEFORE_REPLAY').toHaveLength(1);
          lifetimeTimer.callback(...lifetimeTimer.args);
          expect(reads(lifetimeReplacement.peer), 'L08_REPLAY_GUARDED').toHaveLength(0);
          await lifetimeFreshRead(producer, lifetimeReplacement);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('teardown during timer allocation prevents registration', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        let lifetimeReplacement: ReturnType<typeof lifetimeReady> | undefined;
        const lifetimeClock = lifetimeTimers(producer.delay, () => {
          lifetimeReplacement = lifetimeReplace(lifetimeFixture.relay, producer);
        });
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          expect(lifetimeReplacement, 'SETUP_ALLOCATION_REENTRY_COMPLETED').toBeDefined();
          expect(lifetimeReplacement!.owner, 'SETUP_ALLOCATION_REENTRY_NEW_OWNER').not.toBe(lifetimeFixture.owner);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L09_RETURNED_ORPHAN_CANCELED').toHaveLength(1);
          await lifetimeFreshRead(producer, lifetimeReplacement!);
        } finally {
          lifetimeClock.restore();
        }
      });

      it('closed retained record cannot deliver', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        const lifetimeOriginalClosed = lifetimeFixture.owner.closed;
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          lifetimeOne(lifetimeClock);
          lifetimeFixture.owner.closed = true;
          expect(relayStore.connection(lifetimeFixture.relay.id), 'SETUP_CLOSED_SAME_CURRENT_RECORD').toBe(lifetimeFixture.owner);
          expect(lifetimeFixture.owner.status, 'SETUP_CLOSED_STILL_CONNECTED').toBe('connected');
          expect(lifetimeFixture.peer.readyState, 'SETUP_CLOSED_TRANSPORT_STILL_LIVE').toBe(MockWebSocket.OPEN);
          await vi.advanceTimersByTimeAsync(producer.delay);
          expect(reads(lifetimeFixture.peer), 'L10_CLOSED_RECORD_NO_DELIVERY').toHaveLength(0);
          lifetimeFixture.owner.closed = lifetimeOriginalClosed;
          relayStore.readPane(lifetimeFixture.agent);
          expectRead(lifetimeFixture.peer, lifetimeFixture.agent);
        } finally {
          lifetimeFixture.owner.closed = lifetimeOriginalClosed;
          lifetimeClock.restore();
        }
      });

      it('delivery reentrancy consumes job before read', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        const lifetimeNativeSend = lifetimeFixture.peer.send.bind(lifetimeFixture.peer);
        let lifetimeReplay: (() => void) | undefined;
        let lifetimeReentered = false;
        const lifetimeSendHook = vi.spyOn(lifetimeFixture.peer, 'send').mockImplementation(payload => {
          lifetimeNativeSend(payload);
          if (!lifetimeReentered && (JSON.parse(payload) as Wire).type === 'read_pane') {
            lifetimeReentered = true;
            // Tokenless send is synchronous. No early ack/forced read: replay
            // before the FIRST send returns and pendingPaneReads is registered.
            lifetimeReplay!();
          }
        });
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          lifetimeReplay = () => lifetimeTimer.callback(...lifetimeTimer.args);
          await vi.advanceTimersByTimeAsync(producer.delay);
          expect(lifetimeReentered, 'SETUP_ACTUAL_SEND_REENTRY').toBe(true);
          expect(reads(lifetimeFixture.peer), 'L11_DELIVERY_CONSUMED_BEFORE_READ').toHaveLength(1);
          const lifetimeRead = expectRead(lifetimeFixture.peer, lifetimeFixture.agent);
          acknowledge(lifetimeFixture.peer, lifetimeFixture.agent, lifetimeRead);
        } finally {
          lifetimeSendHook.mockRestore();
          lifetimeClock.restore();
        }
      });

      it('clearTimeout exception propagates and surviving delivery remains guarded', async () => {
        const lifetimeFixture = lifetimeSingle(producer);
        const lifetimeClock = lifetimeTimers(producer.delay);
        const lifetimeOriginalClosed = lifetimeFixture.owner.closed;
        const lifetimeTransport = lifetimeFixture.owner.transport;
        try {
          await succeed(producer, lifetimeFixture.peer, start(producer, lifetimeFixture.peer, lifetimeFixture.agent));
          const lifetimeTimer = lifetimeOne(lifetimeClock);
          const lifetimeInjected = new Error('T046 targeted genuine delayed clear');
          lifetimeClock.fault.current = { handle: lifetimeTimer.handle, error: lifetimeInjected };
          let lifetimeCaught: unknown;
          try {
            relayStore.disconnectRelay(lifetimeFixture.relay.id);
          } catch (lifetimeError) {
            lifetimeCaught = lifetimeError;
          }
          expect(lifetimeCaught, 'L12_EXACT_CLEAR_ERROR_PROPAGATES').toBe(lifetimeInjected);
          lifetimeClock.fault.current = null;
          expect(lifetimeClock.attempts.filter(handle => handle === lifetimeTimer.handle), 'L12_TARGETED_CLEAR_ATTEMPT').toHaveLength(1);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeTimer.handle), 'L12_THROW_IS_NOT_CANCEL').toHaveLength(0);
          // Narrow fault contract, NOT universal exception-safe cleanup. A
          // thrown cancel can leave a closed owner with live connected transport.
          expect(relayStore.connection(lifetimeFixture.relay.id), 'SETUP_THROW_RETAINS_CURRENT_OWNER').toBe(lifetimeFixture.owner);
          expect(lifetimeFixture.owner, 'SETUP_THROW_CLOSED_BUT_CONNECTED').toMatchObject({ closed: true, status: 'connected' });
          expect(lifetimeFixture.owner.transport, 'SETUP_THROW_RETAINS_TRANSPORT').toBe(lifetimeTransport);
          expect(lifetimeFixture.peer.readyState, 'SETUP_THROW_TRANSPORT_LIVE').toBe(MockWebSocket.OPEN);
          lifetimeTimer.callback(...lifetimeTimer.args);
          expect(reads(lifetimeFixture.peer), 'L12_SURVIVING_CALLBACK_GUARDED_WHILE_LIVE').toHaveLength(0);
        } finally {
          lifetimeClock.fault.current = null;
          lifetimeFixture.owner.closed = lifetimeOriginalClosed;
          lifetimeClock.restore();
        }
      });

      it('retiring relay A preserves relay B job', async () => {
        // The existing two-relay setup finishes ALL configuration before work.
        const [lifetimeA, lifetimeB] = legacyPeers(producer);
        const lifetimeBOwner = relayStore.connection(lifetimeB.relay.id);
        const lifetimeClock = lifetimeTimers(producer.delay);
        try {
          await succeed(producer, lifetimeA.peer, start(producer, lifetimeA.peer, lifetimeA.agent));
          expect(lifetimeClock.allocations, 'SETUP_ISOLATION_A_ALLOCATION').toHaveLength(1);
          await vi.advanceTimersByTimeAsync(100);
          await succeed(producer, lifetimeB.peer, start(producer, lifetimeB.peer, lifetimeB.agent));
          expect(lifetimeClock.allocations, 'SETUP_ISOLATION_B_ALLOCATION').toHaveLength(2);
          const lifetimeBTimer = lifetimeClock.allocations[1];
          relayStore.disconnectRelay(lifetimeA.relay.id);
          expect(relayStore.connection(lifetimeB.relay.id), 'G01_B_OWNER_UNCHANGED').toBe(lifetimeBOwner);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeBTimer.handle), 'G01_B_NOT_CANCELED').toHaveLength(0);
          await vi.advanceTimersByTimeAsync(producer.delay - 101);
          expect(reads(lifetimeB.peer), 'G01_B_NOT_AT_A_DEADLINE').toHaveLength(0);
          await vi.advanceTimersByTimeAsync(1);
          expect(reads(lifetimeB.peer), 'G01_B_NOT_AT_A_DEADLINE_EXACT').toHaveLength(0);
          await vi.advanceTimersByTimeAsync(99);
          expect(reads(lifetimeB.peer), 'G01_B_NOT_BEFORE_OWN_DEADLINE').toHaveLength(0);
          await vi.advanceTimersByTimeAsync(1);
          expect(Date.now(), 'G01_B_EXACT_DEADLINE').toBe(lifetimeBTimer.allocatedAt + producer.delay);
          expectRead(lifetimeB.peer, lifetimeB.agent);
          expect(lifetimeClock.canceled.filter(handle => handle === lifetimeBTimer.handle), 'G01_B_NEVER_CANCELED').toHaveLength(0);
          // Deliberately no assertion that A was canceled: that missing cancel
          // is a RED contract above, never an extra requirement of this green.
        } finally {
          lifetimeClock.restore();
        }
      });
    });
  }
});

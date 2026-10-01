import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  RESUME_DEADLINE_MS,
  RESUME_MAX_EPOCHS,
  RESUME_METRICS_OPT_OUT_KEY,
  RESUME_RETENTION_MS,
  ResumeMetrics,
  resumeMetrics,
  type ResumeClock,
  type ResumeSample,
} from '$lib/resume-metrics';
import { censoredQuantile, summarizeResume } from '$lib/resume-summary';
import { relayStore } from '$lib/store';
import type { RelayTransport, TransportAuthentication, TransportHandlers } from '$lib/transports';
import type { RelayConfig } from '$lib/types';

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

/** A deterministic clock: monotonic and wall time move only when told to. */
function fakeClock(options: { discarded?: boolean; onLine?: boolean | null } = {}) {
  let now = 1_000;
  let wall = 1_800_000_000_000;
  const frames: Array<() => void> = [];
  const clock: ResumeClock = {
    now: () => now,
    wall: () => wall,
    frame: (callback) => { frames.push(callback); },
    onLine: () => options.onLine ?? true,
    discarded: () => options.discarded ?? false,
  };
  return {
    clock,
    /** Time passes for a running page: both clocks advance. */
    advance(ms: number) { now += ms; wall += ms; },
    /** A frozen page: only the wall clock moves. */
    freeze(ms: number) { wall += ms; },
    setWall(value: number) { wall = value; },
    renderFrames() { for (const callback of frames.splice(0)) callback(); },
    pendingFrames: () => frames.length,
  };
}

function only(metrics: ResumeMetrics, epochIndex = -1): ResumeSample {
  const epoch = metrics.snapshot().at(epochIndex)!;
  expect(epoch.samples).toHaveLength(1);
  return epoch.samples[0];
}

/** Drives an authenticated WSS dial through every observable phase. */
function dialWss(metrics: ResumeMetrics, time: ReturnType<typeof fakeClock>, relayId: string, generation: number) {
  time.advance(5); metrics.phase(relayId, generation, 'dial', 'websocket');
  time.advance(40); metrics.phase(relayId, generation, 'open', 'websocket');
  time.advance(3); metrics.phase(relayId, generation, 'e2ee-hello', 'websocket');
  time.advance(20); metrics.phase(relayId, generation, 'e2ee-server-hello', 'websocket');
  time.advance(4); metrics.phase(relayId, generation, 'e2ee-confirm', 'websocket');
  time.advance(18); metrics.phase(relayId, generation, 'authenticated', 'websocket');
  metrics.connected(relayId, generation, 'websocket');
}

describe('local resume metrics', () => {
  it('records the exact phase order of a cold WSS dial labelled by an authenticated descriptor', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    time.advance(2);
    metrics.frame('relay-a', generation);
    metrics.ingress('relay-a', generation, 'cloudflare');
    time.advance(1);
    metrics.inventory('relay-a', generation, true);
    // Arrival alone is not success: the frame that paints it must run.
    expect(only(metrics).outcome).toBeNull();
    time.advance(16);
    time.renderFrames();

    const [epoch] = metrics.snapshot();
    expect(epoch).toMatchObject({
      trigger: 'cold-start',
      lifecycle: 'cold-launch',
      hiddenMs: null,
      navigationToAppMs: 1_000,
      onLine: true,
    });
    const sample = epoch.samples[0];
    expect(sample).toMatchObject({
      path: 'wss/cloudflare',
      lifecycle: 'cold-launch',
      attempts: 1,
      pathAttempts: 1,
      superseded: 0,
      timeouts: 0,
      failures: 0,
      outcome: 'fresh',
      doneAt: 109,
      direct: null,
    });
    expect(sample.phases).toEqual({
      dial: 5,
      open: 45,
      'e2ee-hello': 48,
      'e2ee-server-hello': 68,
      'e2ee-confirm': 72,
      authenticated: 90,
      'first-frame': 92,
      inventory: 93,
      rendered: 109,
    });
    const order = Object.entries(sample.phases).sort((left, right) => left[1] - right[1]).map(([name]) => name);
    expect(order).toEqual([
      'dial', 'open', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated',
      'first-frame', 'inventory', 'rendered',
    ]);
  });

  it('labels WSS ingress only from an authenticated descriptor, never from the relay address', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const cases: Array<[string, unknown, boolean]> = [
      ['relay-on-cloudflare.example.com', 'cloudflare', false],
      ['relay.tail1234.ts.net', undefined, true],
      ['legacy-url', 'gateway', true],
      ['byo', 'tailscale-byo', true],
      ['cli', 'tailscale-cli', true],
      ['managed', 'tailscale-managed', true],
      ['garbage', { host: 'relay.example' }, true],
    ];
    for (const [relayId, descriptor, authenticated] of cases) {
      const generation = metrics.attempt(relayId, false);
      metrics.phase(relayId, generation, 'dial', 'websocket');
      metrics.phase(relayId, generation, 'open', 'websocket');
      if (authenticated) metrics.phase(relayId, generation, 'authenticated', 'websocket');
      metrics.connected(relayId, generation, 'websocket');
      metrics.ingress(relayId, generation, descriptor);
      metrics.inventory(relayId, generation, true);
    }
    time.renderFrames();
    const paths = metrics.snapshot()[0].samples.map((sample) => sample.path);
    expect(paths).toEqual([
      // Plaintext loopback development never authenticates, so its descriptor is ignored.
      'wss/ingress-unknown',
      // An older relay without a descriptor stays unknown despite a tailnet-looking name.
      'wss/ingress-unknown',
      'wss/other',
      'wss/tailscale-byo',
      'wss/tailscale-cli',
      'wss/tailscale-managed',
      'wss/ingress-unknown',
    ]);
    const serialized = JSON.stringify(metrics.snapshot());
    for (const marker of ['cloudflare.example', 'ts.net', 'relay.example', 'legacy-url', 'garbage']) {
      expect(serialized).not.toContain(marker);
    }
  });

  it('labels relayed gateway inventory separately from the later direct promotion', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('gateway-relay', true);
    for (const phase of ['dial', 'open', 'gateway-hello', 'gateway-proof', 'gateway-ready', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated'] as const) {
      time.advance(10);
      metrics.phase('gateway-relay', generation, phase, 'gateway');
    }
    metrics.connected('gateway-relay', generation, 'gateway');
    metrics.ingress('gateway-relay', generation, 'gateway');
    time.advance(5);
    metrics.frame('gateway-relay', generation);
    metrics.inventory('gateway-relay', generation, true);
    time.advance(10);
    time.renderFrames();
    for (const phase of ['dial', 'offer', 'answer', 'ice-connected', 'open', 'e2ee-hello', 'authenticated'] as const) {
      time.advance(20);
      metrics.phase('gateway-relay', generation, phase, 'webrtc');
    }
    time.advance(20);
    metrics.phase('gateway-relay', generation, 'promoted', 'webrtc');
    metrics.connected('gateway-relay', generation, 'webrtc');

    const sample = only(metrics);
    expect(sample).toMatchObject({ path: 'gateway/relayed', outcome: 'fresh', doneAt: 105 });
    expect(sample.phases).toMatchObject({ 'gateway-hello': 30, 'gateway-proof': 40, 'gateway-ready': 50, authenticated: 90, inventory: 95 });
    expect(sample.phases).not.toHaveProperty('offer');
    expect(sample.direct).toEqual({
      attempts: 1,
      failures: 0,
      promotedAt: 265,
      phases: { dial: 125, offer: 145, answer: 165, 'ice-connected': 185, open: 205, 'e2ee-hello': 225, authenticated: 245 },
    });
    const [group] = summarizeResume(metrics.snapshot()).groups;
    expect(group).toMatchObject({ path: 'gateway/relayed', direct: { attempted: 1, promoted: 1, failures: 0 } });
  });

  it('coalesces a wake burst into one epoch without suppressing a later network change', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    metrics.hidden();
    time.freeze(30_000);
    metrics.wake('visible');
    time.advance(3);
    metrics.wake('focus');
    time.advance(37);
    metrics.network('online');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();

    time.advance(5_000);
    // Focus after the wake has completed is an app switch, not a wake.
    metrics.wake('focus');
    // Going offline never opens an epoch by itself.
    metrics.network('offline');
    expect(metrics.snapshot()).toHaveLength(2);
    // A real network change while visible is its own recovery to measure.
    metrics.network('change');

    const epochs = metrics.snapshot();
    expect(epochs.map((epoch) => epoch.trigger)).toEqual(['cold-start', 'visible', 'network']);
    expect(epochs[1]).toMatchObject({ hiddenMs: 30_000, lifecycle: null, coalesced: 2 });
    expect(epochs[1].signals).toEqual({ visible: 0, focus: 3, online: 40 });
    expect(epochs[1].samples[0]).toMatchObject({ lifecycle: 'reconnect', outcome: 'fresh' });
    expect(epochs[2].signals).toEqual({ change: 0 });
  });

  it('keeps failed and replaced dials inside one epoch and ignores abandoned callbacks', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const warm = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', warm, 'authenticated', 'websocket');
    metrics.connected('relay-a', warm, 'websocket');
    metrics.inventory('relay-a', warm, true);
    time.renderFrames();
    metrics.hidden();
    time.freeze(10 * 60_000);
    time.advance(10);

    metrics.wake('visible');
    metrics.probe('relay-a', warm);
    time.advance(2_000);
    // The probe went unanswered: the half-open socket is replaced.
    metrics.end('relay-a', warm, 'timeout');
    const blackholed = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', blackholed, 'dial', 'websocket');
    time.advance(5_000);
    // A dial that never opened is replaced on the next revalidation.
    const replacement = metrics.attempt('relay-a', false);
    // Late callbacks from the abandoned dial must not leak into the sample.
    metrics.phase('relay-a', blackholed, 'open', 'websocket');
    metrics.phase('relay-a', blackholed, 'authenticated', 'websocket');
    metrics.connected('relay-a', blackholed, 'websocket');
    metrics.inventory('relay-a', blackholed, true);
    metrics.phase('relay-a', warm, 'open', 'websocket');
    time.advance(30);
    metrics.phase('relay-a', replacement, 'dial', 'websocket');
    time.advance(50);
    metrics.phase('relay-a', replacement, 'open', 'websocket');
    time.advance(50);
    metrics.phase('relay-a', replacement, 'authenticated', 'websocket');
    metrics.connected('relay-a', replacement, 'websocket');
    time.advance(10);
    metrics.frame('relay-a', replacement);
    metrics.inventory('relay-a', replacement, true);
    time.advance(16);
    time.renderFrames();

    const sample = only(metrics);
    expect(sample).toMatchObject({
      lifecycle: 'reconnect',
      attempts: 2,
      pathAttempts: 2,
      superseded: 1,
      timeouts: 1,
      failures: 0,
      outcome: 'fresh',
      doneAt: 7_156,
    });
    expect(sample.phases).toEqual({
      probe: 0,
      dial: 7_030,
      open: 7_080,
      authenticated: 7_130,
      'first-frame': 7_140,
      inventory: 7_140,
      rendered: 7_156,
    });
  });

  it('requires a post-wake request on the current generation before inventory counts as fresh', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const live = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', live, 'authenticated', 'websocket');
    metrics.connected('relay-a', live, 'websocket');
    metrics.inventory('relay-a', live, true);
    time.renderFrames();
    metrics.hidden();
    time.advance(1_000);
    metrics.wake('visible');

    // A snapshot already in flight before the wake proves nothing.
    metrics.inventory('relay-a', live, true);
    expect(metrics.snapshot()[1].samples).toHaveLength(0);
    time.advance(20);
    metrics.probe('relay-a', live);
    time.advance(40);
    metrics.frame('relay-a', live);
    // A starting, error or stale inventory is not authoritative.
    metrics.inventory('relay-a', live, false);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    time.advance(5);
    metrics.inventory('relay-a', live, true);
    time.advance(16);
    time.renderFrames();

    expect(only(metrics)).toMatchObject({
      lifecycle: 'warm',
      attempts: 0,
      outcome: 'fresh',
      doneAt: 81,
      phases: { probe: 20, 'probe-answer': 60, inventory: 65, rendered: 81 },
    });
    // A replaced generation finishing its frame later changes nothing.
    const stale = metrics.attempt('relay-b', false);
    metrics.attempt('relay-b', false);
    metrics.inventory('relay-b', stale, true);
    time.renderFrames();
    expect(metrics.snapshot()[1].samples).toHaveLength(2);
    expect(metrics.snapshot()[1].samples[1].outcome).toBeNull();
  });

  it('censors deadline misses, reports late fresh frames, and records auth refusal', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const slow = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', slow, 'dial', 'websocket');
    time.advance(RESUME_DEADLINE_MS);
    metrics.phase('relay-a', slow, 'open', 'websocket');
    metrics.phase('relay-a', slow, 'authenticated', 'websocket');
    metrics.connected('relay-a', slow, 'websocket');
    time.advance(1_000);
    metrics.inventory('relay-a', slow, true);
    time.renderFrames();
    expect(only(metrics, 0)).toMatchObject({ outcome: 'deadline', doneAt: null, lateFreshAt: 61_000 });
    expect(only(metrics, 0).phases).toEqual({ dial: 0 });

    metrics.hidden();
    time.advance(500);
    metrics.wake('visible');
    const refused = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', refused, 'dial', 'websocket');
    metrics.phase('relay-a', refused, 'open', 'websocket');
    metrics.end('relay-a', refused, 'auth-rejected');
    expect(only(metrics)).toMatchObject({ outcome: 'auth-rejected', doneAt: null });

    const groups = summarizeResume(metrics.snapshot()).groups;
    expect(groups.map((group) => [
      group.lifecycle, group.valid_attempts, group.fresh_on_time, group.late_fresh, group.non_completions,
      group.time_to_fresh_ms.n,
    ])).toEqual([
      ['cold-launch', 1, 0, 1, { deadline: 1 }, 1],
      ['reconnect', 1, 0, 0, { 'auth-rejected': 1 }, 1],
    ]);
  });

  it('splits device unlock from network work and keeps a locked wake open', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    time.advance(150);
    metrics.unlock('request');
    time.advance(2_450);
    // Still waiting for the authenticator: the network hint joins this wake.
    metrics.network('online');
    time.advance(400);
    metrics.unlock('success');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.advance(10);
    time.renderFrames();

    const epochs = metrics.snapshot();
    expect(epochs).toHaveLength(1);
    expect(epochs[0].unlock).toEqual({ requestedAt: 150, unlockedAt: 3_000 });
    expect(epochs[0].signals).toMatchObject({ online: 2_600 });
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 3_100 });
    const summary = summarizeResume(epochs);
    expect(summary.unlock).toMatchObject({ requested: 1, unlocked: 1, cancelled: 0 });
    expect(summary.groups[0].time_to_fresh_ms.n).toBe(1);
    expect(summary.groups[0].transport_eligible_ms.n).toBe(1);

    // A cancelled unlock is a refusal, never a fast resume.
    metrics.hidden();
    time.advance(100);
    metrics.wake('visible');
    metrics.unlock('request');
    const pending = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', pending, 'dial', 'websocket');
    metrics.unlock('failure');
    time.advance(RESUME_DEADLINE_MS);
    expect(only(metrics)).toMatchObject({ outcome: 'unlock-cancelled', doneAt: null });
    expect(summarizeResume(metrics.snapshot()).unlock).toMatchObject({ requested: 2, unlocked: 1, cancelled: 1 });
  });

  it('closes an unfinished epoch when the page hides and records hidden wall time', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', generation, 'dial', 'websocket');
    metrics.hidden();
    expect(only(metrics)).toMatchObject({ outcome: 'hidden', doneAt: null });
    time.freeze(90_000);
    metrics.hidden();
    time.freeze(10_000);
    metrics.wake('pageshow');
    expect(metrics.snapshot()[1]).toMatchObject({ trigger: 'pageshow', lifecycle: 'bfcache', hiddenMs: 100_000 });
    const summary = summarizeResume(metrics.snapshot());
    // Hidden before an outcome is abandonment, not a failed or successful attempt.
    expect(summary.groups[0]).toMatchObject({ valid_attempts: 0, abandoned: 1 });
  });

  it('cancels a removed relay sample and drops every callback after clearing', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', generation, 'dial', 'websocket');
    metrics.removed('relay-a');
    expect(only(metrics)).toMatchObject({ outcome: 'removed', doneAt: null });
    // A removed relay's late callbacks are ignored.
    metrics.phase('relay-a', generation, 'open', 'websocket');
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    expect(only(metrics).phases).toEqual({ dial: 0 });
    expect(summarizeResume(metrics.snapshot()).groups[0]).toMatchObject({ valid_attempts: 0, abandoned: 1 });

    const next = metrics.attempt('relay-b', false);
    metrics.clear();
    metrics.phase('relay-b', next, 'dial', 'websocket');
    metrics.inventory('relay-b', next, true);
    time.renderFrames();
    expect(metrics.snapshot()).toHaveLength(0);
    expect(summarizeResume([])).toMatchObject({ epochs: 0, samples: 0, groups: [] });
  });

  it('marks a discarded page and never reports unobservable phases as measured', () => {
    const time = fakeClock({ discarded: true });
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    const [group] = summarizeResume(metrics.snapshot()).groups;
    expect(group.lifecycle).toBe('discarded');
    expect(group.unavailable).toEqual(['os-wake-to-js', 'dns', 'tcp', 'tls']);
    expect(group.not_applicable).toEqual(expect.arrayContaining(['gateway-hello', 'gateway-proof', 'gateway-ready', 'probe']));
    for (const phase of ['dns', 'tcp', 'tls', 'os-wake-to-js']) expect(group.phase_p50_ms).not.toHaveProperty(phase);
    // One sample is too few for a median, and never a guessed zero.
    expect(group.time_to_fresh_ms).toEqual({ n: 1, p50: 'insufficient', p95: 'insufficient' });

    const warmTime = fakeClock();
    const warmMetrics = new ResumeMetrics(warmTime.clock, true);
    warmMetrics.wake('cold-start');
    const live = warmMetrics.attempt('relay-a', false);
    warmMetrics.connected('relay-a', live, 'websocket');
    warmMetrics.hidden();
    warmMetrics.wake('visible');
    warmMetrics.probe('relay-a', live);
    warmMetrics.inventory('relay-a', live, true);
    warmTime.renderFrames();
    const warmGroup = summarizeResume(warmMetrics.snapshot()).groups.find((entry) => entry.lifecycle === 'warm')!;
    // A reused connection performed no dial, so its handshake phases do not apply.
    expect(warmGroup.not_applicable).toEqual(expect.arrayContaining(['dial', 'open', 'e2ee-hello', 'authenticated']));
  });

  it('bounds the ring to 100 epochs and 24 hours, and clears on request or opt-out', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    for (let index = 0; index < RESUME_MAX_EPOCHS + 20; index += 1) {
      metrics.wake('visible');
      metrics.hidden();
      time.advance(10);
    }
    expect(metrics.snapshot()).toHaveLength(RESUME_MAX_EPOCHS);
    time.advance(RESUME_RETENTION_MS + 1);
    expect(metrics.snapshot()).toHaveLength(0);

    metrics.wake('visible');
    expect(metrics.snapshot()).toHaveLength(1);
    // A wall clock that jumps backwards cannot extend retention.
    time.setWall(1_800_000_000_000 - 60 * 60_000);
    expect(metrics.snapshot()).toHaveLength(0);

    metrics.wake('visible');
    const revisions = get(metrics.revision);
    metrics.clear();
    expect(metrics.snapshot()).toHaveLength(0);
    expect(get(metrics.revision)).toBeGreaterThan(revisions);

    metrics.wake('visible');
    metrics.setEnabled(false);
    expect(localStorage.getItem(RESUME_METRICS_OPT_OUT_KEY)).toBe('off');
    expect(metrics.snapshot()).toHaveLength(0);
    metrics.hidden();
    metrics.wake('visible');
    const generation = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', generation, 'dial', 'websocket');
    expect(metrics.snapshot()).toHaveLength(0);
    expect(new ResumeMetrics(time.clock).enabled).toBe(false);
    metrics.setEnabled(true);
    expect(localStorage.getItem(RESUME_METRICS_OPT_OUT_KEY)).toBeNull();
  });

  it('computes censored nearest-rank quantiles without inventing values', () => {
    const infinity = Number.POSITIVE_INFINITY;
    expect(censoredQuantile([100, 200, 300, 400, 500], 0.5, 5)).toBe(300);
    expect(censoredQuantile([100, 200, 300, 400], 0.5, 5)).toBe('insufficient');
    expect(censoredQuantile([100, 200, infinity, infinity, infinity], 0.5, 5)).toBe('beyond-deadline');
    const twenty = Array.from({ length: 20 }, (_, index) => (index + 1) * 10);
    expect(censoredQuantile(twenty, 0.95, 20)).toBe(190);
    twenty[19] = infinity;
    expect(censoredQuantile(twenty, 0.95, 20)).toBe(190);
    twenty[18] = infinity;
    expect(censoredQuantile(twenty, 0.95, 20)).toBe('beyond-deadline');
  });
});

describe('store resume-metric wiring', () => {
  afterEach(() => {
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    resumeMetrics.setEnabled(true);
    vi.restoreAllMocks();
  });

  it('scopes samples to the live connection generation and the authenticated descriptor', async () => {
    const transports: Array<{ handlers: TransportHandlers; authentication?: TransportAuthentication; closed: boolean }> = [];
    transportHijack.current = (_relay, handlers, authentication) => {
      const entry = { handlers, authentication, closed: false };
      transports.push(entry);
      return {
        kind: 'websocket',
        connect: () => { handlers.onStatus('connecting'); },
        send: () => !entry.closed,
        close: () => { entry.closed = true; },
      };
    };
    relayStore.destroy();
    resumeMetrics.setEnabled(true);
    resumeMetrics.wake('cold-start');
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora-secret-host.example', token: '' });
    expect(transports).toHaveLength(1);
    transports[0].authentication?.observe?.('dial', 'websocket');
    // Replacing the dial supersedes the first attempt inside the same epoch.
    relayStore.connectAll(true);
    expect(transports).toHaveLength(2);

    const [abandoned, current] = transports;
    abandoned.authentication?.observe?.('authenticated', 'websocket');
    abandoned.handlers.onStatus('connected', { path: 'websocket' });
    abandoned.handlers.onMessage({ type: 'push_config', protocol: 3, ingress: 'cloudflare', inventory: { state: 'ready' } });

    const observe = current.authentication?.observe;
    expect(observe).toBeTypeOf('function');
    for (const phase of ['dial', 'open', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated'] as const) {
      observe?.(phase, 'websocket');
    }
    current.handlers.onStatus('connected', { path: 'websocket' });
    current.handlers.onMessage({
      type: 'push_config', protocol: 3, host: 'fedora-secret-host', ingress: 'tailscale-byo',
      capabilities: [], agent_profiles: [], inventory: { state: 'ready', stale: false },
    });
    current.handlers.onMessage({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1', agent: 'codex', status: 'idle', cwd: '/home/private/project',
        server_session_id: 'primary', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: '',
      }],
    });

    await vi.waitFor(() => {
      expect(resumeMetrics.snapshot().at(-1)?.samples[0]?.outcome).toBe('fresh');
    });
    const sample = resumeMetrics.snapshot().at(-1)!.samples[0];
    expect(sample).toMatchObject({ path: 'wss/tailscale-byo', attempts: 2, superseded: 1 });
    expect(Object.keys(sample.phases)).toEqual(expect.arrayContaining([
      'dial', 'open', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated', 'first-frame', 'inventory', 'rendered',
    ]));
    const exported = JSON.stringify(summarizeResume(resumeMetrics.snapshot()));
    for (const marker of ['secret-host', 'fedora', 'Fedora', '/home/private', 'w1:p1', 'terminal-']) {
      expect(exported).not.toContain(marker);
      expect(JSON.stringify(resumeMetrics.snapshot())).not.toContain(marker);
    }

    relayStore.destroy();
    expect(resumeMetrics.snapshot()).toHaveLength(0);
  });
});

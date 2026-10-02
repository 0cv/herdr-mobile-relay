import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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
import type { RelayTransport, TransportAuthentication, TransportHandlers, TransportPhase } from '$lib/transports';
import { createHybridTransport } from '$lib/transports/path-manager';
import type { RelayConfig } from '$lib/types';
import { CorrelatedInventoryFixture } from './correlated-inventory-fixture';

function upgradedPeer(handlers: TransportHandlers): CorrelatedInventoryFixture {
  const receive = handlers.onMessage;
  const peer = new CorrelatedInventoryFixture(receive);
  handlers.onMessage = (message) => {
    receive(peer.server(message) as Record<string, any>);
    peer.flush();
  };
  return peer;
}

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
    /** Runs queued frame callbacks newest first. */
    renderFramesReverse() { for (const callback of frames.splice(0).reverse()) callback(); },
    /** Runs one queued frame callback (oldest is 0), leaving the rest queued. */
    runFrame(index: number) { frames.splice(index, 1)[0]?.(); },
    pendingFrames: () => frames.length,
  };
}

type FrameOrder = 'in order' | 'obsolete first' | 'reverse';
const FRAME_ORDERS: FrameOrder[] = ['in order', 'obsolete first', 'reverse'];

/**
 * Flushes the two frame callbacks queued by an obsolete and a current
 * snapshot. In 'obsolete first' the obsolete callback runs alone first and
 * must leave the current snapshot pending and the sample unfinished.
 */
function flushTwoFrames(time: ReturnType<typeof fakeClock>, metrics: ResumeMetrics, order: FrameOrder, inventoryAt: number) {
  expect(time.pendingFrames()).toBe(2);
  if (order === 'in order') time.renderFrames();
  else if (order === 'reverse') time.renderFramesReverse();
  else {
    time.runFrame(0);
    expect(only(metrics)).toMatchObject({ outcome: null, phases: { inventory: inventoryAt } });
    expect(only(metrics).phases).not.toHaveProperty('rendered');
    time.runFrame(0);
  }
  expect(time.pendingFrames()).toBe(0);
}

/** Metrics with an agent list on screen, as when the app shows its home view. */
function shown(metrics: ResumeMetrics): ResumeMetrics {
  metrics.presentInventory();
  return metrics;
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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

  it('classifies a back/forward cache restore even when pageshow joins an earlier wake signal', () => {
    const orders: Array<{ first: 'resume' | 'visible'; redial: boolean }> = [
      { first: 'resume', redial: false },
      { first: 'visible', redial: false },
      { first: 'visible', redial: true },
    ];
    for (const { first, redial } of orders) {
      const label = `${first}${redial ? ' with redial' : ''}`;
      const time = fakeClock();
      const metrics = shown(new ResumeMetrics(time.clock, true));
      metrics.setParticipants(() => [{ id: 'relay-a', hybrid: false }]);
      metrics.wake('cold-start');
      let generation = metrics.attempt('relay-a', false);
      dialWss(metrics, time, 'relay-a', generation);
      metrics.inventory('relay-a', generation, true);
      time.renderFrames();
      metrics.hidden();
      time.freeze(60_000);
      time.advance(10);

      metrics.wake(first);
      if (redial) generation = metrics.attempt('relay-a', false);
      expect(only(metrics).lifecycle, label).toBe(redial ? 'reconnect' : 'warm');
      time.advance(3);
      // security.ts reports pageshow only when it is persisted.
      metrics.wake('pageshow');
      if (first === 'resume') {
        time.advance(2);
        metrics.wake('visible');
      }
      if (redial) dialWss(metrics, time, 'relay-a', generation);
      else metrics.probe('relay-a', generation);
      time.advance(20);
      metrics.frame('relay-a', generation);
      metrics.inventory('relay-a', generation, true);
      time.advance(16);
      time.renderFrames();

      const epoch = metrics.snapshot().at(-1)!;
      // The original start and deadline are kept; only the category changes.
      expect(epoch, label).toMatchObject({ trigger: first, lifecycle: 'bfcache', hiddenMs: 60_010 });
      expect(epoch.signals, label).toMatchObject({ [first]: 0, pageshow: 3 });
      expect(epoch.samples[0], label).toMatchObject({ lifecycle: 'bfcache', outcome: 'fresh' });
    }
  });

  it('coalesces a wake burst into one epoch without suppressing a later network change', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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
    const warmMetrics = shown(new ResumeMetrics(warmTime.clock, true));
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
    const metrics = shown(new ResumeMetrics(time.clock, true));
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

/** Drives an authenticated gateway dial through every observable phase. */
function dialGateway(metrics: ResumeMetrics, time: ReturnType<typeof fakeClock>, relayId: string, generation: number) {
  for (const phase of ['dial', 'open', 'gateway-hello', 'gateway-proof', 'gateway-ready', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated'] as const) {
    time.advance(10);
    metrics.phase(relayId, generation, phase, 'gateway');
  }
  metrics.connected(relayId, generation, 'gateway');
}

interface InternalTrack {
  epoch: unknown;
  sample: unknown;
  record: unknown;
  pending: { sample: { outcome: string | null; phases: Record<string, number> } } | null;
  generation: number;
}

function internalTracks(metrics: ResumeMetrics): Map<string, InternalTrack> {
  return (metrics as unknown as { tracks: Map<string, InternalTrack> }).tracks;
}

describe('resume metric completion and retirement', () => {
  it('does not complete from inventory whose connection closed before the frame painted', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const first = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', first);
    metrics.inventory('relay-a', first, true);
    // The socket closes before the next animation frame.
    metrics.end('relay-a', first, 'failed');
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: null, failures: 1 });
    expect(only(metrics).phases).not.toHaveProperty('inventory');

    const second = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', second);
    metrics.inventory('relay-a', second, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 212, attempts: 2, failures: 1, pathAttempts: 2 });
    expect(only(metrics).paths.map((record) => [record.path, record.end ?? 'open', record.servedAt !== undefined]))
      .toEqual([['websocket', 'failed', true], ['websocket', 'open', true]]);
  });

  it('does not complete from inventory whose gateway path was replaced before the frame painted', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-g', true);
    dialGateway(metrics, time, 'relay-g', generation);
    metrics.inventory('relay-g', generation, true);
    // The serving gateway dies and the list moves on, inside one connection.
    metrics.phase('relay-g', generation, 'failed', 'gateway');
    metrics.connecting('relay-g', generation);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();

    dialGateway(metrics, time, 'relay-g', generation);
    metrics.inventory('relay-g', generation, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', path: 'gateway/relayed', pathAttempts: 2, pathFailures: 0 });
    expect(only(metrics).paths.map((record) => [record.path, record.end ?? 'open'])).toEqual([['gateway', 'failed'], ['gateway', 'open']]);
  });

  it('does not complete from relayed inventory when the direct path is promoted before the paint', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-g', true);
    dialGateway(metrics, time, 'relay-g', generation);
    metrics.inventory('relay-g', generation, true);
    // The direct upgrade takes over before the frame that would paint it.
    metrics.phase('relay-g', generation, 'promoted', 'webrtc');
    metrics.connected('relay-g', generation, 'webrtc');
    time.advance(16);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    expect(only(metrics).phases).not.toHaveProperty('inventory');
    // The direct path's own snapshot completes it.
    time.advance(20);
    metrics.inventory('relay-g', generation, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', path: 'gateway/direct', doneAt: 142 });
  });

  it('does not complete from inventory the relay reports stale or not ready before the paint', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    // inventory_status turns stale (or the relay reports an error) first.
    metrics.inventory('relay-a', generation, false);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh' });
  });

  it('counts fresh inventory only in a frame painted while an inventory view is mounted', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    // Settings is open: the snapshot is published but nothing renders it.
    metrics.inventory('relay-a', generation, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: null });
    expect(only(metrics).phases).not.toHaveProperty('rendered');
    time.advance(1_000);
    const hide = metrics.presentInventory();
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 1_122, phases: { inventory: 90, rendered: 1_122 } });

    // With every view gone again, the next wake waits for one to return.
    hide();
    hide();
    metrics.hidden();
    time.advance(100);
    metrics.wake('visible');
    metrics.probe('relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    metrics.presentInventory();
    time.renderFrames();
    expect(only(metrics).outcome).toBe('fresh');
  });

  it('ignores observations that arrive after their connection ended', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', generation, 'dial', 'websocket');
    time.advance(30);
    metrics.phase('relay-a', generation, 'open', 'websocket');
    metrics.end('relay-a', generation, 'failed');
    // A handshake promise that settles after the socket closed.
    time.advance(40);
    metrics.phase('relay-a', generation, 'e2ee-hello', 'websocket');
    metrics.phase('relay-a', generation, 'authenticated', 'websocket');
    metrics.connected('relay-a', generation, 'websocket');
    metrics.frame('relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    const sample = only(metrics);
    expect(sample).toMatchObject({ outcome: null, failures: 1 });
    expect(sample.phases).toEqual({ dial: 0, open: 30 });
    expect(sample.paths).toEqual([{ path: 'websocket', dialAt: 0, reached: 'open', end: 'failed', endedAt: 30 }]);
  });

  it('lets only the current snapshot complete when a direct promotion replaces it before any frame', () => {
    for (const order of FRAME_ORDERS) {
      const time = fakeClock();
      const metrics = shown(new ResumeMetrics(time.clock, true));
      metrics.wake('cold-start');
      const generation = metrics.attempt('relay-g', true);
      dialGateway(metrics, time, 'relay-g', generation);
      metrics.inventory('relay-g', generation, true);
      time.advance(20);
      metrics.phase('relay-g', generation, 'promoted', 'webrtc');
      metrics.connected('relay-g', generation, 'webrtc');
      // Withdrawn at once, not by its stale frame later.
      expect(only(metrics).phases, order).not.toHaveProperty('inventory');
      time.advance(10);
      metrics.inventory('relay-g', generation, true);
      time.advance(16);
      flushTwoFrames(time, metrics, order, 120);
      expect(only(metrics), order).toMatchObject({
        outcome: 'fresh',
        path: 'gateway/direct',
        doneAt: 136,
        phases: { authenticated: 90, inventory: 120, rendered: 136 },
        direct: { promotedAt: 110 },
      });
    }
  });

  it('lets only the current snapshot complete when inventory turns stale and fresh again before any frame', () => {
    for (const order of FRAME_ORDERS) {
      const time = fakeClock();
      const metrics = shown(new ResumeMetrics(time.clock, true));
      metrics.wake('cold-start');
      const generation = metrics.attempt('relay-a', false);
      dialWss(metrics, time, 'relay-a', generation);
      metrics.inventory('relay-a', generation, true);
      time.advance(5);
      metrics.inventory('relay-a', generation, false);
      expect(only(metrics).phases, order).not.toHaveProperty('inventory');
      time.advance(5);
      metrics.inventory('relay-a', generation, true);
      time.advance(16);
      flushTwoFrames(time, metrics, order, 100);
      expect(only(metrics), order).toMatchObject({ outcome: 'fresh', doneAt: 116, phases: { dial: 5, inventory: 100, rendered: 116 } });
    }
  });

  it('lets only the replacement connection complete when it delivers before the old frame', () => {
    for (const order of FRAME_ORDERS) {
      const time = fakeClock();
      const metrics = shown(new ResumeMetrics(time.clock, true));
      metrics.wake('cold-start');
      const first = metrics.attempt('relay-a', false);
      dialWss(metrics, time, 'relay-a', first);
      metrics.inventory('relay-a', first, true);
      time.advance(5);
      const second = metrics.attempt('relay-a', false);
      expect(only(metrics).phases, order).not.toHaveProperty('inventory');
      dialWss(metrics, time, 'relay-a', second);
      metrics.inventory('relay-a', second, true);
      time.advance(16);
      flushTwoFrames(time, metrics, order, 185);
      expect(only(metrics), order).toMatchObject({
        outcome: 'fresh',
        doneAt: 201,
        attempts: 2,
        superseded: 1,
        phases: { dial: 100, authenticated: 185, inventory: 185, rendered: 201 },
      });
    }
  });

  it('lets only the new gateway path complete when the old one fell back before its frame', () => {
    for (const order of FRAME_ORDERS) {
      const time = fakeClock();
      const metrics = shown(new ResumeMetrics(time.clock, true));
      metrics.wake('cold-start');
      const generation = metrics.attempt('relay-g', true);
      dialGateway(metrics, time, 'relay-g', generation);
      metrics.inventory('relay-g', generation, true);
      time.advance(5);
      metrics.phase('relay-g', generation, 'failed', 'gateway');
      metrics.connecting('relay-g', generation);
      expect(only(metrics).phases, order).not.toHaveProperty('inventory');
      dialGateway(metrics, time, 'relay-g', generation);
      metrics.inventory('relay-g', generation, true);
      time.advance(16);
      flushTwoFrames(time, metrics, order, 185);
      expect(only(metrics), order).toMatchObject({
        outcome: 'fresh',
        path: 'gateway/relayed',
        doneAt: 201,
        pathAttempts: 2,
        phases: { dial: 105, inventory: 185, rendered: 201 },
      });
      expect(only(metrics).paths.map((record) => record.end ?? 'open'), order).toEqual(['failed', 'open']);
    }
  });

  it('never lets an obsolete frame erase or alter a completed sample', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    // A second valid snapshot replaces the first before the paint: the frame
    // shows the newer one, so it owns the completion.
    time.advance(4);
    metrics.inventory('relay-a', generation, true);
    time.advance(12);
    time.runFrame(1);
    const completed = only(metrics);
    expect(completed).toMatchObject({ outcome: 'fresh', doneAt: 106, phases: { inventory: 94, rendered: 106 } });
    time.advance(30);
    time.runFrame(0);
    // Later invalidations do not reach a finished sample either.
    metrics.inventory('relay-a', generation, false);
    metrics.connecting('relay-a', generation);
    metrics.end('relay-a', generation, 'failed');
    expect(only(metrics)).toEqual(completed);
  });

  it('requires a visible inventory view at the frame, not only a mounted one', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    let railShown = false;
    // Registered while mounted, but hidden (a phone-width terminal rail).
    const hideRail = metrics.presentInventory(() => railShown);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: null, phases: { inventory: 90 } });
    expect(only(metrics).phases).not.toHaveProperty('rendered');

    // A view that is visible when registered but hidden by a resize before
    // the frame does not count either.
    railShown = true;
    const hideList = metrics.presentInventory(() => railShown);
    railShown = false;
    time.advance(16);
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    hideList();
    hideRail();

    // A failing visibility check counts as not visible.
    const hideBroken = metrics.presentInventory(() => {
      throw new Error('detached');
    });
    time.renderFrames();
    expect(only(metrics).outcome).toBeNull();
    hideBroken();

    // The waiting snapshot is retried, unchanged, once a visible view appears.
    time.advance(500);
    metrics.presentInventory(() => true);
    expect(time.pendingFrames()).toBe(1);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 638, phases: { inventory: 90, rendered: 638 } });
  });

  it('retries the same waiting snapshot and never a withdrawn one', () => {
    const time = fakeClock();
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    // A view appears and queues a retry, but the inventory turns stale first.
    metrics.presentInventory();
    metrics.inventory('relay-a', generation, false);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: null });
    expect(only(metrics).phases).not.toHaveProperty('inventory');
    // Repeated releases while one retry is queued schedule it once.
    metrics.setLocked(true);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    metrics.setLocked(false);
    metrics.presentInventory();
    expect(time.pendingFrames()).toBe(1);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 122, phases: { inventory: 106, rendered: 106 } });
  });

  it('counts inventory painted behind the device lock only once it can be seen', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.setLocked(true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: null, phases: { inventory: 90, rendered: 106 } });
    time.advance(400);
    metrics.setLocked(false);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ outcome: 'fresh', doneAt: 522, phases: { rendered: 106 } });
  });

  it('enrols every eligible relay at the wake so a locked, undialled wake still ends', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.setParticipants(() => [{ id: 'relay-a', hybrid: false }, { id: 'relay-g', hybrid: true }]);
    metrics.setLocked(true);
    metrics.wake('cold-start');
    expect(metrics.snapshot()[0].samples.map((sample) => [sample.path, sample.lifecycle, sample.outcome]))
      .toEqual([['wss/ingress-unknown', 'cold-launch', null], ['gateway/relayed', 'cold-launch', null]]);
    time.advance(150);
    metrics.unlock('request');
    time.advance(2_000);
    metrics.unlock('failure');
    time.advance(RESUME_DEADLINE_MS);
    expect(metrics.snapshot()[0].samples.map((sample) => sample.outcome)).toEqual(['unlock-cancelled', 'unlock-cancelled']);
    for (const group of summarizeResume(metrics.snapshot()).groups) {
      expect(group).toMatchObject({ valid_attempts: 1, fresh_on_time: 0, non_completions: { 'unlock-cancelled': 1 } });
    }

    // A verification slower than the deadline: the wake fails, and its unlock
    // delay is still recorded.
    metrics.hidden();
    time.advance(100);
    metrics.wake('visible');
    metrics.unlock('request');
    time.advance(70_000);
    metrics.unlock('success');
    metrics.setLocked(false);
    const late = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', late);
    const second = metrics.snapshot()[1];
    expect(second.samples.map((sample) => [sample.lifecycle, sample.outcome, sample.attempts]))
      .toEqual([['reconnect', 'deadline', 0], ['reconnect', 'deadline', 0]]);
    expect(second.unlock).toEqual({ requestedAt: 0, unlockedAt: 70_000 });
  });

  it('ends a carried dial that never reports a phase as a deadline failure', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.setParticipants(() => [{ id: 'relay-a', hybrid: false }]);
    metrics.wake('cold-start');
    metrics.attempt('relay-a', false);
    metrics.hidden();
    time.advance(100);
    metrics.wake('visible');
    time.advance(RESUME_DEADLINE_MS);
    expect(only(metrics)).toMatchObject({ lifecycle: 'reconnect', attempts: 0, outcome: 'deadline', doneAt: null });
    expect(summarizeResume(metrics.snapshot()).groups.find((group) => group.lifecycle === 'reconnect'))
      .toMatchObject({ valid_attempts: 1, non_completions: { deadline: 1 } });
  });

  it('keeps sampling a healthy connection after Clear and after turning measurement off and on', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const live = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', live);
    metrics.ingress('relay-a', live, 'cloudflare');
    metrics.inventory('relay-a', live, true);
    time.renderFrames();

    const warmResume = () => {
      metrics.hidden();
      time.advance(1_000);
      metrics.wake('visible');
      time.advance(10);
      metrics.probe('relay-a', live);
      time.advance(30);
      metrics.frame('relay-a', live);
      metrics.inventory('relay-a', live, true);
      time.advance(16);
      time.renderFrames();
      return only(metrics);
    };
    metrics.clear();
    expect(metrics.snapshot()).toHaveLength(0);
    expect(warmResume()).toMatchObject({ lifecycle: 'warm', path: 'wss/cloudflare', outcome: 'fresh', doneAt: 56 });
    metrics.setEnabled(false);
    metrics.setEnabled(true);
    expect(metrics.snapshot()).toHaveLength(0);
    expect(warmResume()).toMatchObject({ lifecycle: 'warm', path: 'wss/cloudflare', outcome: 'fresh' });

    // A connection established while measurement was off is still attributed.
    metrics.setEnabled(false);
    const quiet = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', quiet, 'authenticated', 'websocket');
    metrics.connected('relay-a', quiet, 'websocket');
    metrics.ingress('relay-a', quiet, 'tailscale-byo');
    metrics.setEnabled(true);
    metrics.hidden();
    metrics.wake('visible');
    metrics.probe('relay-a', quiet);
    metrics.inventory('relay-a', quiet, true);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ lifecycle: 'warm', path: 'wss/tailscale-byo', outcome: 'fresh' });
  });

  it('measures a network change that follows a completed wake as its own recovery', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const live = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', live);
    metrics.inventory('relay-a', live, true);
    time.advance(10);
    time.renderFrames();
    expect(only(metrics).outcome).toBe('fresh');
    // A lifecycle duplicate inside the window still joins the finished wake.
    metrics.wake('focus');
    expect(metrics.snapshot()).toHaveLength(1);
    // A real network change half a second later is a distinct recovery.
    time.advance(400);
    metrics.network('change');
    expect(metrics.snapshot().map((epoch) => epoch.trigger)).toEqual(['cold-start', 'network']);
    metrics.probe('relay-a', live);
    time.advance(2_000);
    metrics.end('relay-a', live, 'timeout');
    const replacement = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', replacement);
    metrics.inventory('relay-a', replacement, true);
    time.advance(16);
    time.renderFrames();
    expect(only(metrics)).toMatchObject({ lifecycle: 'reconnect', outcome: 'fresh', timeouts: 1, attempts: 1, doneAt: 2_106 });
  });

  it('drops tracking references to measurements that left the ring', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    metrics.phase('relay-a', generation, 'dial', 'websocket');
    metrics.hidden();
    expect(internalTracks(metrics).get('relay-a')).toMatchObject({ epoch: expect.any(Object), sample: expect.any(Object) });
    time.advance(RESUME_RETENTION_MS + 1);
    expect(metrics.snapshot()).toHaveLength(0);
    expect(internalTracks(metrics).get('relay-a')).toMatchObject({ epoch: null, sample: null, record: null, generation });

    // The count bound severs references too, not only the age bound.
    metrics.wake('visible');
    metrics.attempt('relay-b', false);
    metrics.hidden();
    for (let index = 0; index < RESUME_MAX_EPOCHS; index += 1) {
      time.advance(10);
      metrics.wake('visible');
      metrics.hidden();
    }
    expect(metrics.snapshot()).toHaveLength(RESUME_MAX_EPOCHS);
    expect(internalTracks(metrics).get('relay-b')).toMatchObject({ epoch: null, sample: null, record: null });
    metrics.clear();
    for (const track of internalTracks(metrics).values()) expect(track).toMatchObject({ epoch: null, sample: null, pending: null });
    metrics.reset();
    expect(internalTracks(metrics).size).toBe(0);
  });

  it('detaches a pending snapshot on epoch replacement, pruning, Clear and opt-out without touching old samples', () => {
    const time = fakeClock();
    // No visible view: each snapshot stays pending.
    const metrics = new ResumeMetrics(time.clock, true);
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-a', false);
    dialWss(metrics, time, 'relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    time.renderFrames();
    expect(internalTracks(metrics).get('relay-a')?.pending).toEqual(expect.any(Object));
    // A new wake replaces the epoch: the old snapshot is detached, and the old
    // sample keeps the arrival it recorded.
    time.advance(RESUME_DEADLINE_MS);
    metrics.wake('visible');
    expect(internalTracks(metrics).get('relay-a')?.pending).toBeNull();
    expect(metrics.snapshot()[0].samples[0]).toMatchObject({ outcome: 'deadline', phases: { inventory: 90 } });
    metrics.presentInventory();
    time.renderFrames();
    expect(metrics.snapshot()[0].samples[0]).not.toHaveProperty('lateFreshAt');

    for (const detach of [() => metrics.clear(), () => metrics.setEnabled(false), () => {
      time.advance(RESUME_RETENTION_MS + 1);
      metrics.snapshot();
    }]) {
      metrics.setEnabled(true);
      metrics.hidden();
      metrics.wake('visible');
      metrics.probe('relay-a', generation);
      metrics.setLocked(true);
      metrics.inventory('relay-a', generation, true);
      time.renderFrames();
      expect(internalTracks(metrics).get('relay-a')?.pending).toEqual(expect.any(Object));
      detach();
      expect(internalTracks(metrics).get('relay-a')?.pending).toBeNull();
      metrics.setLocked(false);
      time.renderFrames();
      expect(time.pendingFrames()).toBe(0);
    }

    // Teardown detaches a snapshot whose frame is still queued: the obsolete
    // frame finishes nothing in the dropped epoch.
    metrics.setEnabled(true);
    metrics.hidden();
    metrics.wake('visible');
    metrics.probe('relay-a', generation);
    metrics.inventory('relay-a', generation, true);
    const track = internalTracks(metrics).get('relay-a');
    const dropped = track?.pending?.sample;
    expect(dropped).toEqual(expect.any(Object));
    metrics.reset();
    expect(track?.pending).toBeNull();
    time.renderFrames();
    expect(dropped?.outcome).toBeNull();
    expect(dropped?.phases).not.toHaveProperty('rendered');
    expect(metrics.snapshot()).toEqual([]);
  });

  it('keeps failed gateway and legacy dials in the sample when a later path succeeds', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-g', true);
    time.advance(5);
    metrics.phase('relay-g', generation, 'dial', 'gateway');
    time.advance(20);
    metrics.phase('relay-g', generation, 'open', 'gateway');
    time.advance(25);
    metrics.phase('relay-g', generation, 'failed', 'gateway');
    time.advance(5);
    metrics.phase('relay-g', generation, 'dial', 'gateway');
    time.advance(10);
    metrics.phase('relay-g', generation, 'timeout', 'gateway');
    metrics.phase('relay-g', generation, 'failed', 'gateway');
    // Every gateway failed: the legacy relay URL is the last resort.
    time.advance(5);
    metrics.phase('relay-g', generation, 'dial', 'websocket');
    time.advance(30);
    metrics.phase('relay-g', generation, 'authenticated', 'websocket');
    metrics.connected('relay-g', generation, 'websocket');
    metrics.inventory('relay-g', generation, true);
    time.advance(16);
    time.renderFrames();
    const sample = only(metrics);
    expect(sample).toMatchObject({ outcome: 'fresh', pathAttempts: 3, pathFailures: 2, timeouts: 1, failures: 0 });
    expect(sample.paths).toEqual([
      { path: 'gateway', dialAt: 5, reached: 'open', end: 'failed', endedAt: 50 },
      { path: 'gateway', dialAt: 55, reached: 'dial', end: 'timeout', endedAt: 65 },
      { path: 'websocket', dialAt: 70, reached: 'authenticated', servedAt: 100 },
    ]);
    expect(summarizeResume(metrics.snapshot()).groups[0]).toMatchObject({
      retries: { path_attempts: 3, path_failures: 2, timeouts: 1 },
      path_outcomes: { 'gateway:failed': 1, 'gateway:timeout': 1, 'websocket:served': 1 },
    });
  });

  it('reports gateway and legacy path failures from the path manager', () => {
    const observed: Array<[TransportPhase, string]> = [];
    const scoped: Array<TransportAuthentication | undefined> = [];
    const gateways: TransportHandlers[] = [];
    const legacies: TransportHandlers[] = [];
    const fake = (kind: RelayTransport['kind']): RelayTransport => ({ kind, connect: () => {}, send: () => true, close: () => {} });
    const relay: RelayConfig = {
      id: 'relay-g', label: 'Gateway', url: 'wss://legacy.invalid', token: '', transport: 'hybrid',
      gatewayUrl: 'wss://a.invalid', gatewayUrls: ['wss://a.invalid', 'wss://b.invalid'],
    };
    const transport = createHybridTransport(relay, { onMessage: () => {}, onStatus: () => {} }, {
      createGateway: (_relay, handlers, authentication) => {
        gateways.push(handlers);
        scoped.push(authentication);
        return fake('gateway');
      },
      createDirect: () => fake('webrtc'),
      createLegacy: (_relay, handlers, authentication) => {
        legacies.push(handlers);
        scoped.push(authentication);
        return fake('websocket');
      },
    }, { observe: (phase, path) => observed.push([phase, path]) });
    transport.connect();
    scoped[0]?.observe?.('dial', 'gateway');
    gateways[0].onStatus('closed', { reason: 'unknown', fatal: true, code: 'unknown_relay' });
    scoped[1]?.observe?.('dial', 'gateway');
    gateways[1].onStatus('closed', { reason: 'unknown', fatal: true, code: 'unknown_relay' });
    expect(legacies).toHaveLength(1);
    // A replaced gateway's late callback is not part of the current attempt.
    scoped[0]?.observe?.('authenticated', 'gateway');
    legacies[0].onStatus('closed', { reason: 'Relay disconnected' });
    expect(observed).toEqual([
      ['dial', 'gateway'], ['failed', 'gateway'], ['dial', 'gateway'], ['failed', 'gateway'], ['failed', 'websocket'],
    ]);

    // A device refusal is an authorization outcome, not a path failure.
    observed.length = 0;
    gateways.length = 0;
    const refused = createHybridTransport({ ...relay, url: '', gatewayUrls: undefined }, { onMessage: () => {}, onStatus: () => {} }, {
      createGateway: (_relay, handlers) => {
        gateways.push(handlers);
        return fake('gateway');
      },
      createDirect: () => fake('webrtc'),
    }, { observe: (phase, path) => observed.push([phase, path]) });
    refused.connect();
    gateways[0].onStatus('closed', { reason: 'refused', fatal: true, code: 'device_unauthorized' });
    expect(observed).toEqual([]);
  });

  it('marks socket phases not applicable on a reused or direct path and unavailable after a dial', () => {
    const time = fakeClock();
    const metrics = shown(new ResumeMetrics(time.clock, true));
    metrics.wake('cold-start');
    const generation = metrics.attempt('relay-g', true);
    dialGateway(metrics, time, 'relay-g', generation);
    metrics.inventory('relay-g', generation, true);
    time.renderFrames();
    for (const phase of ['dial', 'offer', 'answer', 'ice-connected', 'open', 'authenticated', 'promoted'] as const) {
      time.advance(10);
      metrics.phase('relay-g', generation, phase, 'webrtc');
    }
    metrics.connected('relay-g', generation, 'webrtc');
    metrics.hidden();
    time.advance(1_000);
    metrics.wake('visible');
    metrics.probe('relay-g', generation);
    metrics.inventory('relay-g', generation, true);
    time.renderFrames();

    const groups = summarizeResume(metrics.snapshot()).groups;
    const cold = groups.find((group) => group.path === 'gateway/relayed')!;
    expect(cold.unavailable).toEqual(['os-wake-to-js', 'dns', 'tcp', 'tls']);
    expect(cold.direct).toMatchObject({ attempted: 1, promoted: 1 });
    expect(cold.direct.not_applicable).toEqual(expect.arrayContaining(['dns', 'tcp', 'tls', 'websocket-open']));
    const direct = groups.find((group) => group.path === 'gateway/direct')!;
    expect(direct).toMatchObject({ lifecycle: 'warm', valid_attempts: 1, fresh_on_time: 1 });
    // ICE over the live session replaced DNS, TCP and TLS: they did not happen.
    expect(direct.unavailable).toEqual(['os-wake-to-js']);
    expect(direct.not_applicable).toEqual(expect.arrayContaining(['dns', 'tcp', 'tls', 'dial', 'open', 'authenticated']));
  });
});

describe('store resume-metric wiring', () => {
  let hide: () => void = () => {};
  beforeEach(() => {
    // These tests stand in for an app showing its agent list.
    hide = resumeMetrics.presentInventory();
  });

  afterEach(() => {
    hide();
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    resumeMetrics.setEnabled(true);
    vi.restoreAllMocks();
  });

  it('withdraws pending freshness when the relay reports its inventory stale', async () => {
    let live: TransportHandlers | null = null;
    transportHijack.current = (_relay, handlers) => {
      live = handlers;
      const peer = upgradedPeer(handlers);
      return { kind: 'websocket', connect: () => { handlers.onStatus('connecting'); },
        send: (payload) => { peer.client(JSON.stringify(payload)); return true; }, close: () => {} };
    };
    relayStore.destroy();
    resumeMetrics.setEnabled(true);
    resumeMetrics.wake('cold-start');
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });
    const handlers = live as TransportHandlers | null;
    handlers!.onStatus('connected', { path: 'websocket' });
    handlers!.onMessage({ type: 'push_config', protocol: 3, capabilities: [], agent_profiles: [], inventory: { state: 'ready', stale: false } });
    const agents = [{
      pane_id: 'w1:p1', agent: 'codex', status: 'idle',
      server_session_id: 'primary', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: '',
    }];
    handlers!.onMessage({ type: 'agents', agents });
    // Before the next frame, the relay reports its inventory stale.
    handlers!.onMessage({ type: 'inventory_status', state: 'error', stale: true, error_code: 'herdr_down' });
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(resumeMetrics.snapshot().at(-1)?.samples[0]?.outcome).toBeNull();
    handlers!.onMessage({ type: 'inventory_status', state: 'ready', stale: false });
    handlers!.onMessage({ type: 'agents', agents });
    await vi.waitFor(() => {
      expect(resumeMetrics.snapshot().at(-1)?.samples[0]?.outcome).toBe('fresh');
    });
  });

  it('enrols only relays that can resume', () => {
    transportHijack.current = (_relay, handlers) => ({
      kind: 'websocket',
      connect: () => { handlers.onStatus('connecting'); },
      send: () => true,
      close: () => {},
    });
    relayStore.destroy();
    expect(relayStore.resumeParticipants()).toEqual([]);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });
    // A relay key that cannot authenticate never dials, so it never resumes.
    relayStore.addRelay({ label: 'Truncated', url: 'wss://truncated.example', token: 'truncated-key' });
    const [fedora, truncated] = get(relayStore.relayConfigs);
    expect(get(relayStore.connections).get(truncated.id)?.pairingRequired).toBe(true);
    expect(relayStore.resumeParticipants()).toEqual([{ id: fedora.id, hybrid: false }]);
  });

  it('samples a warm resume on the same healthy socket after Clear and after off/on', async () => {
    const sent: Record<string, unknown>[] = [];
    let handlers: TransportHandlers | null = null;
    transportHijack.current = (_relay, transportHandlers) => {
      handlers = transportHandlers;
      const peer = upgradedPeer(transportHandlers);
      return {
        kind: 'websocket',
        connect: () => { transportHandlers.onStatus('connecting'); },
        send: (payload) => {
          sent.push(payload);
          peer.client(JSON.stringify(payload));
          return true;
        },
        close: () => {},
      };
    };
    relayStore.destroy();
    resumeMetrics.setEnabled(true);
    // Consume the page's one cold start, so later wakes are ordinary ones.
    resumeMetrics.wake('cold-start');
    resumeMetrics.hidden();
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'wss://fedora.example', token: '' });
    const live = handlers as TransportHandlers | null;
    expect(live).not.toBeNull();
    live!.onStatus('connected', { path: 'websocket' });
    const agents = [{
      pane_id: 'w1:p1', agent: 'codex', status: 'idle',
      server_session_id: 'primary', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: '',
    }];
    live!.onMessage({ type: 'push_config', protocol: 3, capabilities: [], agent_profiles: [], inventory: { state: 'ready', stale: false } });
    live!.onMessage({ type: 'agents', agents });

    const resets = [
      () => resumeMetrics.clear(),
      () => {
        resumeMetrics.setEnabled(false);
        resumeMetrics.setEnabled(true);
      },
    ];
    for (const reset of resets) {
      reset();
      expect(resumeMetrics.snapshot()).toHaveLength(0);
      resumeMetrics.hidden();
      resumeMetrics.wake('visible');
      sent.length = 0;
      relayStore.revalidateConnections(2_000);
      expect(sent).toContainEqual(expect.objectContaining({ type: 'refresh_agents', snapshot_request_id: expect.any(String) }));
      live!.onMessage({ type: 'inventory_status', state: 'ready', stale: false });
      live!.onMessage({ type: 'agents', agents });
      await vi.waitFor(() => {
        expect(resumeMetrics.snapshot().at(-1)?.samples[0]).toMatchObject({ lifecycle: 'warm', outcome: 'fresh', attempts: 0 });
      });
      expect(resumeMetrics.snapshot().at(-1)?.samples).toHaveLength(1);
    }
  });

  it('scopes samples to the live connection generation and the authenticated descriptor', async () => {
    const transports: Array<{ handlers: TransportHandlers; authentication?: TransportAuthentication; closed: boolean }> = [];
    transportHijack.current = (_relay, handlers, authentication) => {
      const entry = { handlers, authentication, closed: false };
      transports.push(entry);
      const peer = upgradedPeer(handlers);
      return {
        kind: 'websocket',
        connect: () => { handlers.onStatus('connecting'); },
        send: (payload) => { if (entry.closed) return false; peer.client(JSON.stringify(payload)); return true; },
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

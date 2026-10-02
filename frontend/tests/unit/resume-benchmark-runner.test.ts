import { describe, expect, it } from 'vitest';
import {
  buildPreregistration,
  digest,
  epochSchedule,
  epochSeed,
  DEFAULT_PILOT_SCENARIOS,
  DIRECT_UPGRADE_WINDOW_MS,
  DEFAULT_PILOT_TRANSPORTS,
  directUpgradeRecord,
  harnessErrorClass,
  HARNESS_INVALID_CRITERIA,
  IDLE_BEFORE_HIDE_MS,
  NON_COMPLETION_OUTCOMES,
  observeDirectUpgrade,
  pairOrder,
  relayForTransport,
  SCENARIOS,
  TRANSPORTS,
  parseArguments,
  PILOT_MIN_EPOCHS,
} from '../../scripts/run-resume-benchmarks.mjs';

const CANDIDATE = 'a'.repeat(40);
const BASELINE = 'b'.repeat(40);
const pilotOptions = {
  design: 'pilot' as const,
  browsers: ['chromium', 'webkit'] as Array<'chromium' | 'webkit'>,
  transports: ['wss-cloudflare', 'gateway-relayed'] as Array<'wss-cloudflare' | 'gateway-relayed'>,
  scenarios: ['warm-short', 'hidden-5m', 'blackhole-restore', 'discard'] as Array<'warm-short' | 'hidden-5m' | 'blackhole-restore' | 'discard'>,
  samples: PILOT_MIN_EPOCHS,
  baseSeed: 20_261_001,
  candidateSha: CANDIDATE,
  baselineSha: '',
};

describe('resume benchmark runner preregistration', () => {
  it('records strata, seeds, schedules, endpoints and bounds before any epoch runs', () => {
    const plan = buildPreregistration(pilotOptions);
    expect(plan).toMatchObject({
      design: 'pilot',
      candidate_sha: CANDIDATE,
      baseline_sha: CANDIDATE,
      deadline_ms: 60_000,
      sample_size_per_stratum: 30,
      sample_unit: 'attempted wake epoch',
      replacement_limit: 2,
      idle_before_hide_ms: IDLE_BEFORE_HIDE_MS,
      direct_upgrade_window_ms: DIRECT_UPGRADE_WINDOW_MS,
    });
    expect(plan.hide_sequence).toMatch(/kill the socket .* then advance the frozen wall clock/);
    expect(plan.strata).toHaveLength(16);
    expect(plan.strata.every((stratum) => stratum.role === 'pilot')).toBe(true);
    expect(plan.strata.map((stratum) => stratum.id)).toContain('webkit/gateway-relayed/blackhole-restore');
    expect(plan.network_restoration_schedule).toMatchObject({
      restore_after_wake_ms: [1_000, 4_000],
      retransmit_schedule_ms: [1_000, 3_000, 7_000, 15_000, 31_000],
      online_event_at_restore: true,
    });
    expect(plan.unlock_schedule.positive_strata).toMatch(/disabled/);
    expect(plan.negative_controls.map((control) => control.id)).toEqual([
      'revoked-credential', 'cancelled-unlock', 'permanent-outage', 'ios-deferred-pairing',
    ]);
    expect(plan.negative_controls.every((control) => control.counts_as_latency_success === false)).toBe(true);
    // Only failures established outside the page under test are exclusions;
    // crashes, failed loads and failed warm-ups are censored non-completions.
    expect(plan.harness_invalid_criteria.map((criterion) => criterion.id)).toEqual(['server-unavailable', 'browser-unavailable']);
    expect(HARNESS_INVALID_CRITERIA.map((criterion) => criterion.id)).not.toEqual(expect.arrayContaining(['browser-crash']));
    expect(plan.non_completion_outcomes).toEqual(expect.arrayContaining(['page-crash', 'browser-disconnected', 'warmup-failed', 'load-failed']));
    expect(NON_COMPLETION_OUTCOMES).not.toEqual(expect.arrayContaining(plan.harness_invalid_criteria.map((criterion) => criterion.id)));
    expect(plan).toMatchObject({ browsers: ['chromium', 'webkit'], variants: ['baseline'] });
    expect(plan.endpoints.primary).toMatch(/agent card .*not a workspace label.*ready, non-stale.*live authenticated path/);
    expect(plan.bounds).toMatchObject({ reliability_margin: 0.01, target_p95_ratio: 0.8, regression_ratio: 1.1, min_pairs: 400 });
    expect(plan.endpoints.not_measured).toEqual(expect.arrayContaining(['OS wake-to-JS, DNS, TCP, TLS']));
    expect(plan.endpoints.not_measured.join(' ')).not.toMatch(/direct WebRTC upgrade/);
    expect(plan.endpoints.supplementary.join(' ')).toMatch(/direct WebRTC upgrade outcome .* until the first promotion or 30 s, whichever is first, independent of when the primary outcome is recorded .*separate from the primary endpoint/);
    expect(digest(plan)).toBe(digest(JSON.parse(JSON.stringify(plan))));
  });

  it('counts every preregistered paired comparison in the family', () => {
    const plan = buildPreregistration({
      ...pilotOptions,
      design: 'paired',
      browsers: ['chromium'],
      transports: ['wss-cloudflare'],
      scenarios: ['warm-short', 'blackhole-restore'],
      samples: 400,
      baselineSha: BASELINE,
      targets: ['chromium/wss-cloudflare/blackhole-restore'],
    });
    expect(plan.strata.map((stratum) => [stratum.id, stratum.role])).toEqual([
      ['chromium/wss-cloudflare/warm-short', 'regression'],
      ['chromium/wss-cloudflare/blackhole-restore', 'acceptance-target'],
    ]);
    expect(plan.analysis.planned_family_size).toBe(5);
    expect(plan).toMatchObject({ baseline_sha: BASELINE, candidate_sha: CANDIDATE, sample_unit: 'matched baseline/candidate pair' });
    // Both variants run every negative control.
    expect(plan.variants).toEqual(['baseline', 'candidate']);
  });

  it('refuses underpowered or ambiguous designs', () => {
    expect(() => buildPreregistration({ ...pilotOptions, samples: 29 })).toThrow(/at least 30/);
    expect(() => buildPreregistration({ ...pilotOptions, design: 'paired', samples: 399, baselineSha: BASELINE }))
      .toThrow(/at least 400/);
    expect(() => buildPreregistration({ ...pilotOptions, design: 'paired', samples: 400, baselineSha: CANDIDATE }))
      .toThrow(/distinct/);
  });
});

describe('resume benchmark matched conditions', () => {
  it('derives identical seeded conditions for both variants of a pair', () => {
    const stratum = 'chromium/wss-cloudflare/blackhole-restore';
    const seed = epochSeed(20_261_001, stratum, 7);
    expect(epochSeed(20_261_001, stratum, 7)).toBe(seed);
    expect(epochSeed(20_261_001, stratum, 8)).not.toBe(seed);
    expect(epochSeed(20_261_002, stratum, 7)).not.toBe(seed);
    const schedule = epochSchedule(seed, 'blackhole-restore');
    expect(epochSchedule(seed, 'blackhole-restore')).toEqual(schedule);
    expect(schedule.frozenMs).toBe(30_000);
    expect(schedule.hiddenMs).toBeGreaterThanOrEqual(200);
    expect(schedule.hiddenMs).toBeLessThanOrEqual(400);
    expect(schedule.restoreAfterMs).toBeGreaterThanOrEqual(1_000);
    expect(schedule.restoreAfterMs).toBeLessThanOrEqual(4_000);
    expect(epochSchedule(seed, 'warm-short').restoreAfterMs).toBeNull();
    expect(epochSchedule(seed, 'hidden-5m').frozenMs).toBe(300_000);
  });

  it('randomizes the baseline/candidate order independently per pair', () => {
    const firsts = Array.from({ length: 400 }, (_, index) => pairOrder(epochSeed(1, 'stratum', index))[0]);
    const baselineFirst = firsts.filter((variant) => variant === 'baseline').length;
    expect(baselineFirst).toBeGreaterThan(160);
    expect(baselineFirst).toBeLessThan(240);
    for (const index of [0, 1, 2]) {
      const order = pairOrder(epochSeed(1, 'stratum', index));
      expect([...order].sort()).toEqual(['baseline', 'candidate']);
      expect(pairOrder(epochSeed(1, 'stratum', index))).toEqual(order);
    }
  });

  it('records harness exceptions only as fixed classes', () => {
    expect(harnessErrorClass(new Error('page.evaluate: Execution context was destroyed, most likely because of a navigation')))
      .toBe('context-destroyed');
    expect(harnessErrorClass(new Error('page.waitForFunction: Timeout 60000ms exceeded.'))).toBe('timeout');
    expect(harnessErrorClass(new Error('Target page, context or browser has been closed'))).toBe('target-closed');
    expect(harnessErrorClass(new Error('wss://relay.example/ws failed'))).toBe('other');
    expect(harnessErrorClass('not an error')).toBe('other');
  });

  it('offers every approved workload and path while the default pilot stays bounded', () => {
    expect(Object.keys(SCENARIOS)).toEqual([
      'warm-short', 'hidden-30s', 'hidden-5m', 'hidden-long', 'blackhole-restore', 'network-change', 'discard',
    ]);
    expect(Object.fromEntries(Object.entries(SCENARIOS).map(([id, spec]) => [id, [spec.wake, spec.connection]]))).toEqual({
      'warm-short': ['visible', 'kept'],
      'hidden-30s': ['visible', 'kept'],
      'hidden-5m': ['visible', 'half-open'],
      'hidden-long': ['visible', 'half-open'],
      'blackhole-restore': ['visible', 'half-open'],
      'network-change': ['network-change', 'half-open'],
      discard: ['reload', 'none (reloaded)'],
    });
    expect(SCENARIOS['hidden-30s'].frozen_wall_ms).toBe(30_000);
    expect(SCENARIOS['hidden-long'].frozen_wall_ms).toBeGreaterThan(60 * 60_000);
    expect(Object.keys(TRANSPORTS)).toEqual(['wss-cloudflare', 'wss-tailscale', 'gateway-relayed', 'gateway-direct']);
    expect(relayForTransport('wss-cloudflare')).toMatchObject({ transport: 'wss', ingress: 'cloudflare' });
    expect(relayForTransport('wss-tailscale')).toMatchObject({ transport: 'wss', ingress: 'tailscale-managed' });
    expect(relayForTransport('gateway-direct')).toMatchObject({ transport: 'hybrid' });
    expect(() => relayForTransport('wss-elsewhere')).toThrow(/unknown transport/);
    expect([...DEFAULT_PILOT_SCENARIOS]).toEqual(['warm-short', 'hidden-5m', 'blackhole-restore', 'discard']);
    expect([...DEFAULT_PILOT_TRANSPORTS]).toEqual(['wss-cloudflare', 'gateway-relayed']);

    // A preregistered design can select all of them.
    const everything = buildPreregistration({
      ...pilotOptions,
      transports: Object.keys(TRANSPORTS) as typeof pilotOptions.transports,
      scenarios: Object.keys(SCENARIOS) as typeof pilotOptions.scenarios,
    });
    expect(everything.strata).toHaveLength(2 * 4 * 7);
    expect(everything.strata.map((stratum) => stratum.id)).toEqual(expect.arrayContaining([
      'webkit/wss-tailscale/network-change', 'chromium/gateway-direct/hidden-long', 'chromium/wss-cloudflare/hidden-30s',
    ]));
    const schedule = epochSchedule(epochSeed(1, 'chromium/wss-cloudflare/network-change', 0), 'network-change');
    expect(schedule).toMatchObject({ frozenMs: 0, restoreAfterMs: null });
    expect(schedule.hiddenMs).toBeGreaterThanOrEqual(200);
    expect(schedule.hiddenMs).toBeLessThanOrEqual(400);
  });

  it('bounds the direct upgrade record to its own window, however late the primary outcome', async () => {
    expect(DIRECT_UPGRADE_WINDOW_MS).toBe(30_000);
    const event = (kind: string, at: number) => ({ kind, at_ms: at });
    const closed = (events: unknown[], kept: boolean | null = false) => ({ events, window_closed: true, kept_direct: kept });
    const success = [
      event('peer', 20.4), event('offer', 40), event('answer', 60), event('open', 120.6), event('authenticated', 180), event('promoted', 200),
    ];
    const nothing = { attempts: 0, refused: 0, offer_ms: null, answer_ms: null, open_ms: null, authenticated_ms: null, promoted_ms: null };

    // A promotion inside the window ends the observation: attempts and
    // refusals after it, or after the window, never change the record.
    const promoted = directUpgradeRecord({ events: [...success, event('peer', 5_000), event('refused', 5_100)], window_closed: false, kept_direct: null });
    expect(promoted).toEqual({
      outcome: 'promoted', window_ms: 30_000, attempts: 1, refused: 0,
      offer_ms: 40, answer_ms: 60, open_ms: 121, authenticated_ms: 180, promoted_ms: 200,
    });
    expect(directUpgradeRecord(closed([...success, event('peer', 31_000), event('refused', 31_100)]))).toEqual(promoted);

    // A slow primary outcome read 40 s after the wake, whose only direct
    // attempt, offer and promotion came 31-32 s after it: nothing was tried
    // inside the window, and nothing after it is reported.
    const late = closed([
      event('peer', 31_000), event('offer', 31_050), event('answer', 31_100), event('open', 31_500), event('authenticated', 31_700), event('promoted', 32_000),
    ]);
    expect(directUpgradeRecord(late)).toEqual({ outcome: 'not-attempted', window_ms: 30_000, ...nothing });

    // Attempts inside the window that never promote, with more after it.
    const refused = closed([
      event('peer', 2_100), event('offer', 2_150), event('refused', 2_200),
      event('peer', 6_200), event('offer', 6_250), event('refused', 6_300),
      event('peer', 29_990), event('offer', 30_040), event('refused', 30_090),
      event('peer', 45_000), event('promoted', 46_000),
    ]);
    expect(directUpgradeRecord(refused)).toEqual({
      outcome: 'not-promoted', window_ms: 30_000, attempts: 3, refused: 2,
      offer_ms: 2_150, answer_ms: null, open_ms: null, authenticated_ms: null, promoted_ms: null,
    });

    // Whether the wake's direct session survived is decided by the fixture
    // when the window closes: a later loss or new attempt cannot undo it.
    expect(directUpgradeRecord(closed([], true))).toEqual({ outcome: 'stayed-direct', window_ms: 30_000, ...nothing });
    expect(directUpgradeRecord(closed([event('peer', 40_000), event('promoted', 40_300)], true))).toMatchObject({ outcome: 'stayed-direct', attempts: 0 });
    expect(directUpgradeRecord(closed([event('peer', 1_000)], true))).toMatchObject({ outcome: 'not-promoted', attempts: 1 });
    expect(directUpgradeRecord(closed([], false))).toMatchObject({ outcome: 'not-attempted' });

    // Until the window closes, or something is promoted inside it, nothing is settled.
    expect(directUpgradeRecord({ events: [event('peer', 1_000)], window_closed: false, kept_direct: null })).toEqual({ outcome: 'unobserved', window_ms: 30_000 });
    expect(directUpgradeRecord(null)).toEqual({ outcome: 'unobserved', window_ms: 30_000 });

    // A shorter window bounds every count and milestone the same way.
    expect(directUpgradeRecord(closed(success), 150)).toEqual({
      outcome: 'not-promoted', window_ms: 150, attempts: 1, refused: 0,
      offer_ms: 40, answer_ms: 60, open_ms: 121, authenticated_ms: null, promoted_ms: null,
    });

    // Only fixed outcome names and finite, non-negative offsets are retained.
    const noisy = directUpgradeRecord({
      events: [
        event('peer', 10), { kind: 'sdp', at_ms: 11, sdp: 'v=0 secret' }, { kind: 'offer', at_ms: 'wss://relay.example' },
        event('answer', -1), event('open', Number.NaN), { kind: 'promoted', at_ms: 300, relay: 'wss://relay.example' },
      ],
      window_closed: true,
      kept_direct: false,
      relay: 'wss://relay.example',
    });
    expect(noisy).toEqual({ outcome: 'promoted', window_ms: 30_000, ...nothing, attempts: 1, promoted_ms: 300 });
    expect(JSON.stringify(noisy)).not.toMatch(/wss:|v=0/);

    // The runner waits for the window (or an in-window promotion) and reads once.
    const waits: unknown[] = [];
    const page = {
      waitForFunction: async (_: unknown, limit: unknown, options: unknown) => { waits.push({ limit, options }); },
      evaluate: async (_: unknown, argument: unknown) => {
        expect(argument).toEqual({ name: 'directTimeline', value: 1 });
        return late;
      },
    };
    expect(await observeDirectUpgrade(page as never)).toEqual({ outcome: 'not-attempted', window_ms: 30_000, ...nothing });
    expect(waits).toEqual([{ limit: 30_000, options: { polling: 100, timeout: 35_000 } }]);
  });

  it('parses runner arguments with the pilot defaults', () => {
    const options = parseArguments(['--out-dir', '/tmp/resume-pilot', '--candidate-sha', CANDIDATE]);
    expect(options).toMatchObject({
      design: 'pilot',
      samples: 30,
      candidateSha: CANDIDATE,
      browsers: ['chromium', 'webkit'],
      transports: ['wss-cloudflare', 'gateway-relayed'],
      scenarios: ['warm-short', 'hidden-5m', 'blackhole-restore', 'discard'],
      outDir: '/tmp/resume-pilot',
    });
    expect(parseArguments(['--design', 'paired']).samples).toBe(400);
  });
});

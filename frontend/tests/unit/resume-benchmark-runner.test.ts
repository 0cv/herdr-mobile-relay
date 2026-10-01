import { describe, expect, it } from 'vitest';
import {
  buildPreregistration,
  digest,
  epochSchedule,
  epochSeed,
  harnessErrorClass,
  IDLE_BEFORE_HIDE_MS,
  pairOrder,
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
    expect(plan.harness_invalid_criteria.map((criterion) => criterion.id)).toEqual(['browser-crash', 'bundle-load-failed', 'warmup-timeout']);
    expect(plan.bounds).toMatchObject({ reliability_margin: 0.01, target_p95_ratio: 0.8, regression_ratio: 1.1, min_pairs: 400 });
    expect(plan.endpoints.not_measured).toEqual(expect.arrayContaining(['OS wake-to-JS, DNS, TCP, TLS']));
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

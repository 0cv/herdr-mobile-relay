import { describe, expect, it } from 'vitest';
import {
  analyzeEvidence,
  analyzePaired,
  analyzePilot,
  bootstrapQuantileInterval,
  classifyAttempt,
  EVIDENCE_SCHEMA,
  nearestRankQuantile,
  newcombePairedDifference,
  normalQuantile,
  pairedQuantileRatioBound,
  reliabilityPlanning,
  renderMarkdown,
  wilsonInterval,
} from '../../scripts/analyze-resume-benchmarks.mjs';

type Attempt = NonNullable<Parameters<typeof classifyAttempt>[0]>;
const INFINITY = Number.POSITIVE_INFINITY;
const EXCLUSIONS = ['browser-crash', 'bundle-load-failed', 'warmup-timeout'];

function preregistration(strata: Array<{ id: string; role?: string }>, pairs = 400) {
  return {
    deadline_ms: 60_000,
    sample_size_per_stratum: pairs,
    strata,
    bounds: { reliability_margin: 0.01, target_p95_ratio: 0.8, regression_ratio: 1.1, min_pairs: 400, alpha: 0.05, power: 0.8 },
    analysis: { bootstrap_resamples: 400, bootstrap_seed: 7 },
    harness_invalid_criteria: EXCLUSIONS.map((id) => ({ id })),
  };
}

function attempt(stratum: string, variant: 'baseline' | 'candidate', pair: number, time: number | null, outcome = 'deadline'): Attempt {
  return {
    stratum,
    variant,
    pair,
    seed: 1_000 + pair,
    outcome: time === null ? outcome : 'fresh',
    time_to_fresh_ms: time,
    harness_invalid: null,
  };
}

function pairs(stratum: string, count: number, timing: (index: number) => [number | null, number | null]): Attempt[] {
  const records: Attempt[] = [];
  for (let index = 0; index < count; index += 1) {
    const [baseline, candidate] = timing(index);
    records.push(attempt(stratum, 'baseline', index, baseline), attempt(stratum, 'candidate', index, candidate));
  }
  return records;
}

function paired(strata: Array<{ id: string; role?: string }>, attempts: Attempt[], options: { pairs?: number; controls?: Record<string, unknown>[] } = {}) {
  return analyzePaired({
    preregistration: preregistration(strata, options.pairs ?? 400),
    attempts,
    negative_controls: options.controls ?? [],
  });
}

describe('resume benchmark statistics', () => {
  it('matches known normal quantiles', () => {
    expect(normalQuantile(0.5)).toBeCloseTo(0, 12);
    expect(normalQuantile(0.8)).toBeCloseTo(0.841621, 5);
    expect(normalQuantile(0.95)).toBeCloseTo(1.644854, 5);
    expect(normalQuantile(0.975)).toBeCloseTo(1.959964, 6);
    expect(normalQuantile(0.9875)).toBeCloseTo(2.241403, 5);
    expect(normalQuantile(0.025)).toBeCloseTo(-1.959964, 6);
    expect(() => normalQuantile(1)).toThrow(RangeError);
  });

  it('matches hand-computed Wilson score intervals', () => {
    const z = 1.959964;
    expect(wilsonInterval(0, 10, z).estimate).toBe(0);
    expect(wilsonInterval(0, 10, z).lower).toBeCloseTo(0, 12);
    expect(wilsonInterval(0, 10, z).upper).toBeCloseTo(0.277533, 4);
    const high = wilsonInterval(48, 50, z);
    expect(high.lower).toBeCloseTo(0.865399, 4);
    expect(high.upper).toBeCloseTo(0.988961, 4);
    const pilot = wilsonInterval(27, 30, z);
    expect(pilot.lower).toBeCloseTo(0.74379, 3);
    expect(pilot.upper).toBeCloseTo(0.9654, 3);
  });

  it("matches Newcombe's paired-difference example and the zero-discordance bound", () => {
    // Newcombe (1998) example: 36 concordant positive, 12 + 2 discordant, 0 concordant negative.
    const example = newcombePairedDifference({ a: 36, b: 12, c: 2, d: 0 }, 1.959964);
    expect(example.estimate).toBeCloseTo(0.2, 10);
    expect(example.lower).toBeCloseTo(0.0569, 3);
    expect(example.upper).toBeCloseTo(0.3404, 3);

    // No discordant pairs at all still leaves a bound: (z^2/n) / (1 + z^2/n).
    const none = newcombePairedDifference({ a: 0, b: 0, c: 0, d: 400 }, normalQuantile(0.95));
    expect(none.estimate).toBe(0);
    expect(none.phi).toBe(0);
    expect(none.upper).toBeCloseTo(0.006718, 5);
    expect(none.lower).toBeCloseTo(-0.006718, 5);
    // Four hundred pairs with zero failures cannot show a 1-point margin once
    // the family is large enough: 400 is a floor, not a guarantee.
    expect(newcombePairedDifference({ a: 0, b: 0, c: 0, d: 400 }, normalQuantile(0.975)).upper).toBeCloseTo(0.009512, 5);
    expect(newcombePairedDifference({ a: 0, b: 0, c: 0, d: 400 }, normalQuantile(0.99)).upper).toBeCloseTo(0.013349, 5);
  });

  it('places censored attempts last in nearest-rank quantiles', () => {
    expect(nearestRankQuantile([5, 1, 3, INFINITY], 0.5)).toBe(3);
    expect(nearestRankQuantile([5, 1, 3, INFINITY], 0.75)).toBe(5);
    expect(nearestRankQuantile([5, 1, 3, INFINITY], 1)).toBe(INFINITY);
    expect(nearestRankQuantile([], 0.5)).toBeNaN();
    const twenty = Array.from({ length: 20 }, (_, index) => index + 1);
    expect(nearestRankQuantile(twenty, 0.95)).toBe(19);
  });

  it('classifies completions, censoring reasons and exclusions objectively', () => {
    expect(classifyAttempt(attempt('s', 'baseline', 0, 1_200), 60_000, EXCLUSIONS)).toEqual({ excluded: null, completed: true, time: 1_200, reason: null });
    expect(classifyAttempt(attempt('s', 'baseline', 0, 60_001), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, time: INFINITY, reason: 'late' });
    expect(classifyAttempt(attempt('s', 'baseline', 0, null, 'auth-rejected'), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'auth-rejected' });
    expect(classifyAttempt(undefined, 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'missing-outcome' });
    expect(classifyAttempt({ stratum: 's' }, 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'missing-outcome' });
    expect(classifyAttempt({ ...attempt('s', 'baseline', 0, 10), harness_invalid: 'browser-crash' }, 60_000, EXCLUSIONS)).toMatchObject({ excluded: 'browser-crash' });
    // A reason nobody preregistered is a failure, not an exclusion.
    expect(classifyAttempt({ ...attempt('s', 'baseline', 0, 10), harness_invalid: 'slow-network' }, 60_000, EXCLUSIONS))
      .toMatchObject({ excluded: null, completed: false, reason: 'disallowed-exclusion:slow-network' });
  });

  it('is reproducible for a fixed bootstrap seed', () => {
    const values = [100, 200, 300, 400, 500, 600, 700, 800, 900, INFINITY];
    expect(bootstrapQuantileInterval(values, 0.5, 0.95, 500, 3)).toEqual(bootstrapQuantileInterval(values, 0.5, 0.95, 500, 3));
    const identical = Array.from({ length: 50 }, () => ({ baseline: 1_000, candidate: 700 }));
    expect(pairedQuantileRatioBound(identical, 0.95, 0.025, 300, 9)).toEqual({ estimate: 0.7, upper: 0.7, identifiable: true });
  });

  it('plans confirmatory sample sizes from pilot failure rates', () => {
    expect(reliabilityPlanning(0.1, 0.01, 0.025, 0.8)).toMatchObject({
      non_inferiority_pairs: 14_128,
      zero_failure_precision_pairs: 381,
      recommended_minimum_pairs: 14_128,
    });
    expect(reliabilityPlanning(0, 0.01, 0.025, 0.8)).toMatchObject({
      non_inferiority_pairs: 0,
      zero_failure_precision_pairs: 381,
      recommended_minimum_pairs: 400,
    });
  });
});

describe('resume benchmark pilot analysis', () => {
  it('reports every attempted epoch with censoring and no performance claim', () => {
    const attempts: Attempt[] = [];
    for (let index = 0; index < 30; index += 1) {
      attempts.push(attempt('chromium/wss/warm', 'baseline', index, index < 27 ? (index + 1) * 100 : null));
    }
    attempts.push({ ...attempt('chromium/wss/warm', 'baseline', 30, 50), harness_invalid: 'warmup-timeout' });
    const analysis = analyzePilot({
      preregistration: { ...preregistration([{ id: 'chromium/wss/warm' }], 30), analysis: { bootstrap_resamples: 500, bootstrap_seed: 3, planned_family_size: 2 } },
      attempts,
      negative_controls: [{ control: 'revoked-credential', browser: 'chromium', expected: 'refusal', observed: 'refusal', safety: 'pass' }],
    });
    expect(analysis).toMatchObject({ design: 'pilot', claim: 'none', sample_requirement_met: true });
    const [stratum] = analysis.strata;
    expect(stratum).toMatchObject({
      attempted: 31,
      valid_attempts: 30,
      completed_on_time: 27,
      completion_rate: 0.9,
      excluded: { count: 1, reasons: { 'warmup-timeout': 1 } },
      non_completions: { deadline: 3 },
      time_to_fresh_ms: { p50: 1_500, p90: 2_700, p95: 'beyond-deadline' },
      successful_only_ms: { supplementary: true, n: 27, p50: 1_400, p95: 2_600 },
      planning: { non_inferiority_pairs: 14_128, recommended_minimum_pairs: 14_128 },
    });
    expect(stratum.completion_rate_ci95[0]).toBeCloseTo(0.74379, 3);
    expect(stratum.completion_rate_ci95[1]).toBeCloseTo(0.9654, 3);
    expect(stratum.time_to_fresh_ms.p95_ci95[1]).toBe('beyond-deadline');
    expect(analysis.negative_controls.results[0]).toMatchObject({ safety: 'pass', counted_as_latency_success: false });
    expect(renderMarkdown(analysis)).toContain('no performance claim');
  });

  it('flags a preregistered stratum that did not reach its attempted sample', () => {
    const analysis = analyzePilot({
      preregistration: preregistration([{ id: 'webkit/gateway/discard' }], 30),
      attempts: [attempt('webkit/gateway/discard', 'baseline', 0, 900)],
      negative_controls: [],
    });
    expect(analysis.sample_requirement_met).toBe(false);
    expect(analysis.strata[0]).toMatchObject({ attempted: 1, sample_requirement_met: false });
  });

  it('treats a failed negative control as a safety failure', () => {
    const analysis = analyzePilot({
      preregistration: preregistration([], 30),
      attempts: [],
      negative_controls: [{ control: 'cancelled-unlock', browser: 'webkit', expected: 'locked', observed: 'fresh-render', safety: 'fail' }],
    });
    expect(analysis.negative_controls.all_safe).toBe(false);
  });

  it('rejects evidence with an unknown schema', () => {
    expect(() => analyzeEvidence({ schema: 'other' })).toThrow(/schema/);
    expect(analyzeEvidence({ schema: EVIDENCE_SCHEMA, design: 'pilot' })).toMatchObject({ design: 'pilot' });
  });
});

describe('resume benchmark paired acceptance', () => {
  const target = [{ id: 'chromium/wss/blackhole', role: 'acceptance-target' }];

  it('accepts a genuine joint improvement with its adjusted level retained', () => {
    const analysis = paired(target, pairs(target[0].id, 400, () => [1_000, 700]));
    expect(analysis.multiplicity).toMatchObject({ method: 'bonferroni', one_sided: true, family_size: 2, adjusted_alpha: 0.025 });
    const [stratum] = analysis.strata;
    expect(stratum.reliability.upper_bound).toBeCloseTo(0.009512, 5);
    expect(stratum.latency.p95_ratio).toMatchObject({ estimate: 0.7, upper_bound: 0.7, identifiable: true, verdict: 'pass' });
    expect(stratum).toMatchObject({ valid_pairs: 400, verdict: 'pass', paired_outcomes: { both_completed: 400 } });
    expect(analysis).toMatchObject({ verdict: 'pass', accepted: true });
  });

  it('does not accept a candidate that is faster when it succeeds but fails more', () => {
    const analysis = paired(target, pairs(target[0].id, 400, (index) => [1_000, index < 380 ? 500 : null]));
    const [stratum] = analysis.strata;
    // Success-only percentiles look twice as fast; they are supplementary.
    expect(stratum.successful_only).toMatchObject({ supplementary: true, p95_ratio: 0.5 });
    expect(stratum.paired_outcomes).toMatchObject({ candidate_only_failed: 20, baseline_only_failed: 0 });
    expect(stratum.reliability.difference).toBeCloseTo(0.05, 10);
    expect(stratum.reliability.verdict).toBe('not-accepted');
    expect(stratum.latency.p95_ratio.identifiable).toBe(false);
    expect(stratum.verdict).toBe('not-accepted');
    expect(analysis).toMatchObject({ verdict: 'not-accepted', accepted: false });
  });

  it('keeps a censored p95 inconclusive even when reliability is unchanged', () => {
    const analysis = paired(target, pairs(target[0].id, 400, (index) => (index < 24 ? [null, null] : [1_000, 700])));
    const [stratum] = analysis.strata;
    expect(stratum.paired_outcomes).toMatchObject({ both_failed: 24, candidate_only_failed: 0, baseline_only_failed: 0 });
    expect(stratum.reliability.upper_bound).toBeCloseTo(0.00837, 3);
    expect(stratum.reliability.verdict).toBe('pass');
    expect(stratum.latency.p95_ratio).toMatchObject({ identifiable: false, verdict: 'inconclusive' });
    expect(stratum.reasons).toContain('p95-not-identifiable-below-deadline');
    expect(analysis).toMatchObject({ verdict: 'inconclusive', accepted: false });
  });

  it('reports insufficient samples as inconclusive', () => {
    const analysis = paired(target, pairs(target[0].id, 30, () => [1_000, 700]));
    expect(analysis.strata[0]).toMatchObject({ valid_pairs: 30, verdict: 'inconclusive', reliability: { verdict: 'inconclusive' } });
    expect(analysis.strata[0].reasons).toContain('insufficient-pairs');
    expect(analysis.accepted).toBe(false);
  });

  it('counts missing outcomes as non-completions instead of dropping them', () => {
    const attempts = pairs(target[0].id, 400, () => [1_000, 700])
      .filter((record) => !(record.variant === 'candidate' && Number(record.pair) < 3));
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.valid_pairs).toBe(400);
    expect(stratum.non_completions.candidate).toEqual({ 'missing-outcome': 3 });
    expect(stratum.paired_outcomes.candidate_only_failed).toBe(3);
    expect(stratum.reliability.verdict).toBe('not-accepted');
  });

  it('rejects imbalanced failures even when both arms sometimes fail', () => {
    const attempts = pairs(target[0].id, 400, (index) => [index < 10 ? null : 1_000, index >= 10 && index < 40 ? null : 700]);
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.paired_outcomes).toMatchObject({ baseline_only_failed: 10, candidate_only_failed: 30, both_failed: 0 });
    expect(stratum.reliability.difference).toBeCloseTo(0.05, 10);
    expect(stratum.verdict).toBe('not-accepted');
  });

  it('excludes both arms only for preregistered infrastructure reasons', () => {
    const attempts = pairs(target[0].id, 400, () => [1_000, 700]);
    // Pair 0 lost its baseline browser; a same-seed replacement round is used instead.
    attempts[0] = { ...attempts[0], harness_invalid: 'browser-crash' };
    attempts.push({ ...attempt(target[0].id, 'baseline', 0, 1_000), replacement: 1 }, { ...attempt(target[0].id, 'candidate', 0, 700), replacement: 1 });
    // An unregistered excuse is a candidate failure.
    attempts[3] = { ...attempts[3], harness_invalid: 'slow-network' };
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.excluded_pairs).toEqual({ count: 1, reasons: { 'browser-crash': 1 } });
    expect(stratum.valid_pairs).toBe(400);
    expect(stratum.non_completions.candidate).toEqual({ 'disallowed-exclusion:slow-network': 1 });
  });

  it('adjusts for every preregistered comparison, including regression controls', () => {
    const strata = [
      { id: 'chromium/gateway/blackhole', role: 'acceptance-target' },
      { id: 'webkit/wss/warm', role: 'regression' },
    ];
    const attempts = [
      ...pairs(strata[0].id, 600, () => [1_000, 700]),
      ...pairs(strata[1].id, 600, () => [1_000, 1_050]),
    ];
    const analysis = paired(strata, attempts, { pairs: 600 });
    expect(analysis.multiplicity).toMatchObject({ family_size: 5, adjusted_alpha: 0.01 });
    expect(analysis.multiplicity.critical_z).toBeCloseTo(2.326348, 5);
    const regression = analysis.strata[1];
    expect(regression.reliability.upper_bound).toBeCloseTo(0.008939, 5);
    expect(regression.latency).toMatchObject({
      p95_ratio: { estimate: 1.05, upper_bound: 1.05, threshold: 1.1, verdict: 'pass' },
      p50_ratio: { estimate: 1.05, upper_bound: 1.05, threshold: 1.1, verdict: 'pass' },
    });
    expect(analysis).toMatchObject({ verdict: 'pass', accepted: true });
  });

  it('never accepts when a negative control was unsafe', () => {
    const analysis = paired(target, pairs(target[0].id, 400, () => [1_000, 700]), {
      controls: [{ control: 'revoked-credential', browser: 'chromium', expected: 'refusal', observed: 'fresh-render', safety: 'fail' }],
    });
    expect(analysis.strata[0].verdict).toBe('pass');
    expect(analysis).toMatchObject({ verdict: 'not-accepted', accepted: false });
    expect(renderMarkdown(analysis)).toContain('safety assertions, not latency successes');
  });
});

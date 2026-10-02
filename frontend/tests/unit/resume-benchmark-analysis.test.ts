import { describe, expect, it } from 'vitest';
import {
  analyzeEvidence,
  analyzePaired,
  analyzePilot,
  bootstrapQuantileInterval,
  boundVerdict,
  classifyAttempt,
  epochSeed,
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
type Variant = 'baseline' | 'candidate';
type Control = Record<string, unknown>;
const INFINITY = Number.POSITIVE_INFINITY;
const BASE_SEED = 7;
const EXCLUSIONS = ['server-unavailable', 'browser-unavailable'];
const CONTROLS = ['revoked-credential', 'cancelled-unlock', 'permanent-outage', 'ios-deferred-pairing'];

function preregistration(strata: Array<{ id: string; role?: string }>, pairs = 400) {
  return {
    deadline_ms: 60_000,
    sample_size_per_stratum: pairs,
    strata,
    browsers: ['chromium'],
    seeds: { base_seed: BASE_SEED },
    bounds: { reliability_margin: 0.01, target_p95_ratio: 0.8, regression_ratio: 1.1, min_pairs: 400, alpha: 0.05, power: 0.8 },
    analysis: { bootstrap_resamples: 400, bootstrap_seed: 7, planned_family_size: 2 },
    harness_invalid_criteria: EXCLUSIONS.map((id) => ({ id })),
    replacement_limit: 2,
    negative_controls: CONTROLS.map((id) => ({ id, trials_per_browser: 1 })),
  };
}

/** One passing result for every preregistered control, browser, trial and variant. */
function safeControls(variants: Variant[]): Control[] {
  return variants.flatMap((variant) => CONTROLS.map((control) => ({
    control, browser: 'chromium', variant, trial: 0, expected: 'refusal', observed: 'refusal', safety: 'pass',
  })));
}

function attempt(stratum: string, variant: Variant, pair: number, time: number | null, outcome = 'deadline'): Attempt {
  return {
    stratum,
    variant,
    pair,
    seed: epochSeed(BASE_SEED, stratum, pair),
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

function paired(
  strata: Array<{ id: string; role?: string }>,
  attempts: Attempt[],
  options: { pairs?: number; controls?: Control[] } = {},
) {
  return analyzePaired({
    preregistration: preregistration(strata, options.pairs ?? 400),
    attempts,
    negative_controls: options.controls ?? safeControls(['baseline', 'candidate']),
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
    expect(classifyAttempt(attempt('s', 'baseline', 0, 0), 60_000, EXCLUSIONS)).toMatchObject({ completed: true, time: 0 });
    expect(classifyAttempt(attempt('s', 'baseline', 0, 60_001), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, time: INFINITY, reason: 'late' });
    expect(classifyAttempt(attempt('s', 'baseline', 0, null, 'auth-rejected'), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'auth-rejected' });
    expect(classifyAttempt(attempt('s', 'baseline', 0, null, 'page-crash'), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, excluded: null, reason: 'page-crash' });
    expect(classifyAttempt(attempt('s', 'baseline', 0, null, 'warmup-failed'), 60_000, EXCLUSIONS)).toMatchObject({ completed: false, excluded: null, reason: 'warmup-failed' });
    expect(classifyAttempt(undefined, 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'missing-outcome' });
    expect(classifyAttempt({ stratum: 's' }, 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'missing-outcome' });
    expect(classifyAttempt({ ...attempt('s', 'baseline', 0, 10), harness_invalid: 'server-unavailable' }, 60_000, EXCLUSIONS)).toMatchObject({ excluded: 'server-unavailable' });
    // A reason nobody preregistered is a failure, not an exclusion; app
    // crashes and failed warm-ups are not infrastructure.
    for (const reason of ['slow-network', 'browser-crash', 'warmup-timeout']) {
      expect(classifyAttempt({ ...attempt('s', 'baseline', 0, 10), harness_invalid: reason }, 60_000, EXCLUSIONS))
        .toMatchObject({ excluded: null, completed: false, reason: `disallowed-exclusion:${reason}` });
    }
  });

  it('never turns an absent or malformed fresh time into a completion', () => {
    for (const time of [null, undefined, '', '1200', Number.NaN, -5, INFINITY, { ms: 10 }]) {
      const record = { stratum: 's', variant: 'candidate' as const, pair: 0, outcome: 'fresh', time_to_fresh_ms: time };
      expect(classifyAttempt(record, 60_000, EXCLUSIONS), `time ${String(time)}`)
        .toEqual({ excluded: null, completed: false, time: INFINITY, reason: 'missing-measurement' });
    }
    const absent: Attempt = { stratum: 's', variant: 'candidate', pair: 0, outcome: 'fresh' };
    expect(classifyAttempt(absent, 60_000, EXCLUSIONS)).toMatchObject({ completed: false, reason: 'missing-measurement' });
  });

  it('is reproducible for a fixed bootstrap seed', () => {
    const values = [100, 200, 300, 400, 500, 600, 700, 800, 900, INFINITY];
    expect(bootstrapQuantileInterval(values, 0.5, 0.95, 500, 3)).toEqual(bootstrapQuantileInterval(values, 0.5, 0.95, 500, 3));
    const identical = Array.from({ length: 50 }, () => ({ baseline: 1_000, candidate: 700 }));
    expect(pairedQuantileRatioBound(identical, 0.95, 0.025, 300, 9)).toEqual({ estimate: 0.7, lower: 0.7, upper: 0.7, identifiable: true });
    // A censored baseline quantile is unknown: it can neither pass nor reject.
    const unknown = Array.from({ length: 20 }, () => ({ baseline: INFINITY, candidate: 700 }));
    expect(pairedQuantileRatioBound(unknown, 0.95, 0.025, 200, 9)).toMatchObject({ lower: 0, upper: INFINITY, identifiable: false });
    // A censored candidate quantile is only known to be at least the
    // deadline: against a 1 s baseline the ratio is at least 60.
    const slower = Array.from({ length: 20 }, () => ({ baseline: 1_000, candidate: INFINITY }));
    expect(pairedQuantileRatioBound(slower, 0.95, 0.025, 200, 9)).toMatchObject({ estimate: INFINITY, lower: 60, upper: INFINITY, identifiable: false });
  });

  it('never turns censoring into a demonstrated latency rejection the evidence does not support', () => {
    // Censored at 60 s against a 59 s baseline, the candidate's true p95 could
    // be 61 s (ratio 1.03): neither a pass nor a demonstrated violation.
    const close = Array.from({ length: 40 }, () => ({ baseline: 59_000, candidate: INFINITY }));
    const bound = pairedQuantileRatioBound(close, 0.95, 0.01, 200, 9);
    expect(bound.lower).toBeCloseTo(60_000 / 59_000, 10);
    expect(bound).toMatchObject({ upper: INFINITY, identifiable: false });
    expect(boundVerdict({ enough: true, ...bound, threshold: 1.1 })).toBe('inconclusive');
    // Against a 1 s baseline the same censoring does demonstrate a violation.
    const far = pairedQuantileRatioBound(Array.from({ length: 40 }, () => ({ baseline: 1_000, candidate: INFINITY })), 0.95, 0.01, 200, 9);
    expect(boundVerdict({ enough: true, ...far, threshold: 1.1 })).toBe('not-accepted');
    // A different deadline moves the censoring-aware lower bound with it.
    expect(pairedQuantileRatioBound(close, 0.95, 0.01, 200, 9, 120_000).lower).toBeCloseTo(120_000 / 59_000, 10);
  });

  it('separates a pass, a demonstrated violation and an imprecise bound', () => {
    expect(boundVerdict({ enough: true, identifiable: true, lower: -0.01, upper: 0.009, threshold: 0.01 })).toBe('pass');
    expect(boundVerdict({ enough: true, identifiable: true, lower: 0.02, upper: 0.08, threshold: 0.01 })).toBe('not-accepted');
    expect(boundVerdict({ enough: true, identifiable: true, lower: -0.013, upper: 0.013, threshold: 0.01 })).toBe('inconclusive');
    expect(boundVerdict({ enough: true, identifiable: false, lower: 0.5, upper: INFINITY, threshold: 0.8 })).toBe('inconclusive');
    expect(boundVerdict({ enough: true, identifiable: false, lower: INFINITY, upper: INFINITY, threshold: 0.8 })).toBe('not-accepted');
    expect(boundVerdict({ enough: false, identifiable: true, lower: 0.5, upper: 0.7, threshold: 0.8 })).toBe('inconclusive');
  });

  it('derives the same preregistered seed for both variants of a pair', () => {
    expect(epochSeed(7, 'chromium/wss/warm', 3)).toBe(epochSeed(7, 'chromium/wss/warm', 3));
    expect(epochSeed(7, 'chromium/wss/warm', 3)).not.toBe(epochSeed(7, 'chromium/wss/warm', 4));
    expect(epochSeed(7, 'chromium/wss/warm', 3)).not.toBe(epochSeed(8, 'chromium/wss/warm', 3));
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
  const stratum = 'chromium/wss/warm';

  it('reports every attempted epoch with censoring and no performance claim', () => {
    const attempts: Attempt[] = [];
    for (let index = 0; index < 29; index += 1) {
      attempts.push(attempt(stratum, 'baseline', index, index < 27 ? (index + 1) * 100 : null));
    }
    // Pair 29 lost its static server, then its same-seed replacement ran.
    attempts.push({ ...attempt(stratum, 'baseline', 29, 50), harness_invalid: 'server-unavailable' });
    attempts.push({ ...attempt(stratum, 'baseline', 29, null), replacement: 1 });
    const analysis = analyzePilot({
      preregistration: preregistration([{ id: stratum }], 30),
      attempts,
      negative_controls: safeControls(['baseline']),
    });
    expect(analysis).toMatchObject({ design: 'pilot', claim: 'none', sample_requirement_met: true, violations: {} });
    const [result] = analysis.strata;
    expect(result).toMatchObject({
      attempted: 31,
      valid_attempts: 30,
      completed_on_time: 27,
      completion_rate: 0.9,
      excluded: { count: 1, reasons: { 'server-unavailable': 1 } },
      missing: 0,
      non_completions: { deadline: 3 },
      time_to_fresh_ms: { p50: 1_500, p90: 2_700, p95: 'beyond-deadline' },
      successful_only_ms: { supplementary: true, n: 27, p50: 1_400, p95: 2_600 },
      planning: { non_inferiority_pairs: 14_128, recommended_minimum_pairs: 14_128 },
    });
    expect(result.completion_rate_ci95[0]).toBeCloseTo(0.74379, 3);
    expect(result.completion_rate_ci95[1]).toBeCloseTo(0.9654, 3);
    expect(result.time_to_fresh_ms.p95_ci95[1]).toBe('beyond-deadline');
    expect(analysis.negative_controls).toMatchObject({ complete: true, all_safe: true, expected: 4, missing: 0 });
    expect(analysis.negative_controls.results[0]).toMatchObject({ safety: 'pass', counted_as_latency_success: false });
    expect(renderMarkdown(analysis)).toContain('no performance claim');
  });

  it('counts only measured epochs toward the preregistered sample', () => {
    const excludedOnly: Attempt[] = [];
    for (let index = 0; index < 30; index += 1) {
      for (let replacement = 0; replacement <= 2; replacement += 1) {
        excludedOnly.push({ ...attempt(stratum, 'baseline', index, 50), replacement, harness_invalid: 'browser-unavailable' });
      }
    }
    const excluded = analyzePilot({ preregistration: preregistration([{ id: stratum }], 30), attempts: excludedOnly, negative_controls: safeControls(['baseline']) });
    expect(excluded.strata[0]).toMatchObject({ attempted: 90, valid_attempts: 0, exhausted: 30, sample_requirement_met: false });
    expect(excluded.sample_requirement_met).toBe(false);
    expect(renderMarkdown(excluded)).toContain('pilot is incomplete');

    // A warm-up the app never finished is an attempted epoch that failed.
    const failedWarmups = Array.from({ length: 30 }, (_, index) => attempt(stratum, 'baseline', index, null, 'warmup-failed'));
    const failed = analyzePilot({ preregistration: preregistration([{ id: stratum }], 30), attempts: failedWarmups, negative_controls: safeControls(['baseline']) });
    expect(failed.strata[0]).toMatchObject({ valid_attempts: 30, completed_on_time: 0, non_completions: { 'warmup-failed': 30 }, sample_requirement_met: true });

    const short = analyzePilot({
      preregistration: preregistration([{ id: 'webkit/gateway/discard' }], 30),
      attempts: [attempt('webkit/gateway/discard', 'baseline', 0, 900)],
      negative_controls: safeControls(['baseline']),
    });
    expect(short.strata[0]).toMatchObject({ attempted: 1, valid_attempts: 1, missing: 29, sample_requirement_met: false });
  });

  it('treats a failed or missing negative control as unsafe', () => {
    const failing = safeControls(['baseline']);
    failing[1] = { ...failing[1], observed: 'fresh-render', safety: 'fail' };
    const unsafe = analyzePilot({ preregistration: preregistration([], 30), attempts: [], negative_controls: failing });
    expect(unsafe.negative_controls).toMatchObject({ failed: 1, all_safe: false });
    const truncated = analyzePilot({ preregistration: preregistration([], 30), attempts: [], negative_controls: [] });
    expect(truncated.negative_controls).toMatchObject({ expected: 4, missing: 4, complete: false, all_safe: false });
    const undeclared = analyzePilot({
      preregistration: { ...preregistration([], 30), negative_controls: [] },
      attempts: [],
      negative_controls: [],
    });
    expect(undeclared.negative_controls).toMatchObject({ declared: 0, complete: false, all_safe: false });
  });

  it('rejects evidence with an unknown schema', () => {
    expect(() => analyzeEvidence({ schema: 'other' })).toThrow(/schema/);
    expect(analyzeEvidence({ schema: EVIDENCE_SCHEMA, design: 'pilot' })).toMatchObject({ design: 'pilot', sample_requirement_met: false });
  });
});

describe('resume benchmark paired acceptance', () => {
  const target = [{ id: 'chromium/wss/blackhole', role: 'acceptance-target' }];

  it('accepts a genuine joint improvement with its adjusted level retained', () => {
    const analysis = paired(target, pairs(target[0].id, 400, () => [1_000, 700]));
    expect(analysis.multiplicity).toMatchObject({ method: 'bonferroni', one_sided: true, family_size: 2, adjusted_alpha: 0.025 });
    const [stratum] = analysis.strata;
    expect(stratum.reliability.upper_bound).toBeCloseTo(0.009512, 5);
    expect(stratum.latency.p95_ratio).toMatchObject({ estimate: 0.7, lower_bound: 0.7, upper_bound: 0.7, identifiable: true, verdict: 'pass' });
    expect(stratum).toMatchObject({ valid_pairs: 400, verdict: 'pass', paired_outcomes: { both_completed: 400 }, violations: {} });
    expect(analysis).toMatchObject({ verdict: 'pass', accepted: true, negative_controls: { complete: true, all_safe: true, expected: 8 } });
  });

  it('does not accept a candidate that is faster when it succeeds but fails more', () => {
    const analysis = paired(target, pairs(target[0].id, 400, (index) => [1_000, index < 380 ? 500 : null]));
    const [stratum] = analysis.strata;
    // Success-only percentiles look twice as fast; they are supplementary.
    expect(stratum.successful_only).toMatchObject({ supplementary: true, p95_ratio: 0.5 });
    expect(stratum.paired_outcomes).toMatchObject({ candidate_only_failed: 20, baseline_only_failed: 0 });
    expect(stratum.reliability.difference).toBeCloseTo(0.05, 10);
    // The loss is demonstrated: even the lower bound exceeds the margin.
    expect(stratum.reliability.lower_bound).toBeGreaterThan(0.02);
    expect(stratum.reliability.verdict).toBe('not-accepted');
    expect(stratum.latency.p95_ratio.identifiable).toBe(false);
    expect(stratum.verdict).toBe('not-accepted');
    expect(stratum.reasons).toContain('reliability-loss-exceeds-margin');
    expect(analysis).toMatchObject({ verdict: 'not-accepted', accepted: false });
  });

  it('never accepts a candidate whose fresh outcomes carry no measurement', () => {
    const analysis = paired(target, pairs(target[0].id, 400, () => [1_000, null])
      .map((record) => (record.variant === 'candidate' ? { ...record, outcome: 'fresh', time_to_fresh_ms: null } : record)));
    const [stratum] = analysis.strata;
    expect(stratum.non_completions.candidate).toEqual({ 'missing-measurement': 400 });
    expect(stratum.paired_outcomes.candidate_only_failed).toBe(400);
    expect(stratum.reliability.verdict).toBe('not-accepted');
    expect(analysis.accepted).toBe(false);
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
    expect(analysis.strata[0]).toMatchObject({ valid_pairs: 30, missing_pairs: 370, verdict: 'inconclusive', reliability: { verdict: 'inconclusive' } });
    expect(analysis.strata[0].reasons).toContain('insufficient-pairs');
    expect(analysis.accepted).toBe(false);
    const belowFloor = paired(target, pairs(target[0].id, 30, () => [1_000, 700]), { pairs: 30 });
    expect(belowFloor.strata[0].reasons).toContain('preregistered-sample-below-floor');
    expect(belowFloor).toMatchObject({ verdict: 'inconclusive', accepted: false });
  });

  it('counts missing outcomes as non-completions instead of dropping them', () => {
    const attempts = pairs(target[0].id, 400, () => [1_000, 700])
      .filter((record) => !(record.variant === 'candidate' && Number(record.pair) < 3));
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.valid_pairs).toBe(400);
    expect(stratum.non_completions.candidate).toEqual({ 'missing-outcome': 3 });
    expect(stratum.paired_outcomes.candidate_only_failed).toBe(3);
    // Three extra failures neither show a loss beyond the margin nor exclude
    // one: the result is precision-limited, and the baseline is kept.
    expect(stratum.reliability.upper_bound).toBeGreaterThan(0.01);
    expect(stratum.reliability.lower_bound).toBeLessThan(0.01);
    expect(stratum.reliability.verdict).toBe('inconclusive');
    expect(stratum.reasons).toContain('reliability-bound-too-wide');
    expect(stratum.verdict).toBe('inconclusive');
  });

  it('rejects imbalanced failures even when both arms sometimes fail', () => {
    const attempts = pairs(target[0].id, 400, (index) => [index < 10 ? null : 1_000, index >= 10 && index < 40 ? null : 700]);
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.paired_outcomes).toMatchObject({ baseline_only_failed: 10, candidate_only_failed: 30, both_failed: 0 });
    expect(stratum.reliability.difference).toBeCloseTo(0.05, 10);
    expect(stratum.reliability.lower_bound).toBeGreaterThan(0.015);
    expect(stratum.verdict).toBe('not-accepted');
  });

  it('excludes both arms only for preregistered infrastructure reasons', () => {
    const attempts = pairs(target[0].id, 400, () => [1_000, 700]);
    // Pair 0 lost its static server; a same-seed replacement round is used instead.
    attempts[0] = { ...attempts[0], harness_invalid: 'server-unavailable' };
    attempts.push({ ...attempt(target[0].id, 'baseline', 0, 1_000), replacement: 1 }, { ...attempt(target[0].id, 'candidate', 0, 700), replacement: 1 });
    // An unregistered excuse, or an app crash, is a candidate failure.
    attempts[3] = { ...attempts[3], harness_invalid: 'slow-network' };
    attempts[5] = { ...attempts[5], outcome: 'page-crash', time_to_fresh_ms: null };
    const [stratum] = paired(target, attempts).strata;
    expect(stratum.excluded_pairs).toEqual({ rounds: 1, exhausted: 0, reasons: { 'server-unavailable': 1 } });
    expect(stratum.valid_pairs).toBe(400);
    expect(stratum.violations).toEqual({});
    expect(stratum.non_completions.candidate).toEqual({ 'disallowed-exclusion:slow-network': 1, 'page-crash': 1 });
  });

  it('refuses evidence collected beyond the preregistered sample or replacements', () => {
    // Doubling the sample under an unchanged 400-pair registration would
    // narrow the bound enough to pass; it is outside the registration instead.
    const overcollected = paired(target, pairs(target[0].id, 800, () => [1_000, 700]));
    expect(overcollected.strata[0]).toMatchObject({ valid_pairs: 400, verdict: 'invalid', violations: { 'pair-outside-preregistered-sample': 800 } });
    expect(overcollected).toMatchObject({ verdict: 'invalid', accepted: false });

    const base = pairs(target[0].id, 400, () => [1_000, 700]);
    const beyondLimit = [...base, { ...attempt(target[0].id, 'baseline', 1, 1_000), replacement: 3 }];
    expect(paired(target, beyondLimit).strata[0].violations).toEqual({ 'replacement-limit-exceeded': 1 });

    const unjustified = [...base, { ...attempt(target[0].id, 'baseline', 2, 900), replacement: 1 }, { ...attempt(target[0].id, 'candidate', 2, 600), replacement: 1 }];
    const unjustifiedResult = paired(target, unjustified);
    expect(unjustifiedResult.strata[0].violations).toEqual({ 'unjustified-replacement': 1 });
    expect(unjustifiedResult.accepted).toBe(false);

    const duplicated = [...base, attempt(target[0].id, 'candidate', 4, 300)];
    expect(paired(target, duplicated).strata[0].violations).toEqual({ 'duplicate-record': 1 });

    const reseeded = base.map((record, index) => (index === 6 ? { ...record, seed: 12_345 } : record));
    expect(paired(target, reseeded).strata[0].violations).toEqual({ 'seed-mismatch': 1 });

    const stray = paired(target, [...base, attempt('chromium/wss/unregistered', 'candidate', 0, 100)]);
    expect(stray).toMatchObject({ violations: { 'unregistered-stratum': 1 }, verdict: 'invalid', accepted: false });
  });

  it('labels a precision-limited family inconclusive rather than rejected', () => {
    const strata = [
      { id: 'chromium/gateway/blackhole', role: 'acceptance-target' },
      { id: 'webkit/wss/warm', role: 'regression' },
    ];
    const analysis = paired(strata, [
      ...pairs(strata[0].id, 400, () => [1_000, 700]),
      ...pairs(strata[1].id, 400, () => [1_000, 1_050]),
    ]);
    expect(analysis.multiplicity).toMatchObject({ family_size: 5, adjusted_alpha: 0.01 });
    for (const stratum of analysis.strata) {
      expect(stratum.reliability.upper_bound).toBeCloseTo(0.013349, 5);
      expect(stratum.reliability.lower_bound).toBeCloseTo(-0.013349, 5);
      expect(stratum.reliability.verdict).toBe('inconclusive');
      expect(stratum.reasons).toContain('reliability-bound-too-wide');
      expect(stratum.latency.p95_ratio.verdict).toBe('pass');
      expect(stratum.verdict).toBe('inconclusive');
    }
    expect(analysis).toMatchObject({ verdict: 'inconclusive', accepted: false });
  });

  it('rejects a target whose latency gain is demonstrably below 20 percent', () => {
    const analysis = paired(target, pairs(target[0].id, 400, () => [1_000, 1_000]));
    expect(analysis.strata[0].latency.p95_ratio).toMatchObject({ estimate: 1, lower_bound: 1, upper_bound: 1, verdict: 'not-accepted' });
    expect(analysis.strata[0].reasons).toContain('p95-ratio-exceeds-threshold');
    expect(analysis).toMatchObject({ verdict: 'not-accepted', accepted: false });
  });

  it('keeps a censored regression latency inconclusive at the stratum level', () => {
    const regression = [{ id: 'webkit/wss/warm', role: 'regression' }];
    const analysis = paired(regression, pairs(regression[0].id, 400, () => [59_000, null]));
    const [stratum] = analysis.strata;
    expect(stratum.latency.p95_ratio).toMatchObject({ identifiable: false, verdict: 'inconclusive' });
    expect(stratum.latency.p95_ratio.lower_bound).toBeCloseTo(60_000 / 59_000, 5);
    expect(stratum.latency.p50_ratio).toMatchObject({ identifiable: false, verdict: 'inconclusive' });
    expect(stratum.reasons).toContain('p95-not-identifiable-below-deadline');
    expect(stratum.reasons).not.toContain('p95-ratio-exceeds-threshold');
    // The candidate still fails every attempt, which the reliability bound rejects.
    expect(stratum.reliability.verdict).toBe('not-accepted');
  });

  it('refuses registrations that relax the approved contract', () => {
    const controls = safeControls(['baseline', 'candidate']);
    const base = preregistration(target);
    // Every outcome misses the 60 s deadline; a 120 s registration would accept them.
    const slow = pairs(target[0].id, 400, () => [100_000, 70_000]);
    const relaxedDeadline = analyzePaired({ preregistration: { ...base, deadline_ms: 120_000 }, attempts: slow, negative_controls: controls });
    expect(relaxedDeadline).toMatchObject({ verdict: 'invalid', accepted: false, deadline_ms: 60_000, contract_violations: ['deadline-not-60s'] });
    expect(relaxedDeadline.reasons).toContain('registration-violates-contract');
    expect(relaxedDeadline.strata[0].non_completions.candidate).toEqual({ late: 400 });

    const good = pairs(target[0].id, 400, () => [1_000, 700]);
    const relaxed: Array<[string, number]> = [
      ['reliability_margin', 0.05], ['target_p95_ratio', 0.95], ['regression_ratio', 1.5],
      ['min_pairs', 30], ['alpha', 0.2], ['power', 0.5],
    ];
    for (const [key, value] of relaxed) {
      const result = analyzePaired({
        preregistration: { ...base, bounds: { ...base.bounds, [key]: value } },
        attempts: good,
        negative_controls: controls,
      });
      expect(result.contract_violations, key).toEqual([`relaxed-${key}`]);
      expect(result, key).toMatchObject({ verdict: 'invalid', accepted: false });
    }

    const excuse = analyzePaired({
      preregistration: { ...base, harness_invalid_criteria: [...base.harness_invalid_criteria, { id: 'slow-network' }] },
      attempts: good.map((record, index) => (index === 1 ? { ...record, harness_invalid: 'slow-network' } : record)),
      negative_controls: controls,
    });
    expect(excuse).toMatchObject({ verdict: 'invalid', accepted: false, contract_violations: ['unapproved-exclusion-criterion'] });
    // The unapproved reason is still a candidate failure, never an exclusion.
    expect(excuse.strata[0].non_completions.candidate).toEqual({ 'disallowed-exclusion:slow-network': 1 });

    const missingControl = analyzePaired({
      preregistration: { ...base, negative_controls: base.negative_controls.slice(1) },
      attempts: good,
      negative_controls: controls.filter((control) => control.control !== 'revoked-credential'),
    });
    expect(missingControl).toMatchObject({ verdict: 'invalid', accepted: false, contract_violations: ['negative-control-not-registered'] });

    // Stricter bounds are allowed, and used: a 0.5-point margin is too tight
    // for 400 pairs, so the result is inconclusive rather than invalid.
    const stricter = analyzePaired({
      preregistration: { ...base, bounds: { ...base.bounds, reliability_margin: 0.005 } },
      attempts: good,
      negative_controls: controls,
    });
    expect(stricter).toMatchObject({ verdict: 'inconclusive', accepted: false, contract_violations: [] });
    expect(stricter.strata[0].reliability).toMatchObject({ margin: 0.005, verdict: 'inconclusive' });

    const pilot = analyzePilot({
      preregistration: { ...preregistration([{ id: 'chromium/wss/warm' }], 30), deadline_ms: 120_000 },
      attempts: Array.from({ length: 30 }, (_, index) => attempt('chromium/wss/warm', 'baseline', index, 90_000)),
      negative_controls: safeControls(['baseline']),
    });
    expect(pilot).toMatchObject({ sample_requirement_met: false, contract_violations: ['deadline-not-60s'] });
    expect(pilot.strata[0]).toMatchObject({ completed_on_time: 0, non_completions: { late: 30 } });
    const small = analyzePilot({
      preregistration: preregistration([{ id: 'chromium/wss/warm' }], 10),
      attempts: Array.from({ length: 10 }, (_, index) => attempt('chromium/wss/warm', 'baseline', index, 900)),
      negative_controls: safeControls(['baseline']),
    });
    expect(small).toMatchObject({ sample_requirement_met: false, contract_violations: ['pilot-sample-below-30'] });
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

  it('requires every negative control on both variants and never accepts an unsafe one', () => {
    const attempts = pairs(target[0].id, 400, () => [1_000, 700]);
    const unsafeCandidate = safeControls(['baseline', 'candidate']).map((control) => (
      control.variant === 'candidate' && control.control === 'revoked-credential'
        ? { ...control, observed: 'fresh-render', safety: 'fail' }
        : control
    ));
    const unsafe = paired(target, attempts, { controls: unsafeCandidate });
    expect(unsafe.strata[0].verdict).toBe('pass');
    expect(unsafe).toMatchObject({ verdict: 'not-accepted', accepted: false });
    expect(unsafe.reasons).toContain('negative-control-unsafe');
    expect(renderMarkdown(unsafe)).toContain('safety assertions, not latency successes');

    // Safe baseline controls cannot vouch for an unchecked candidate.
    const baselineOnly = paired(target, attempts, { controls: safeControls(['baseline']) });
    expect(baselineOnly.negative_controls).toMatchObject({ expected: 8, observed: 4, missing: 4, complete: false, all_safe: false });
    expect(baselineOnly).toMatchObject({ verdict: 'invalid', accepted: false });

    const none = paired(target, attempts, { controls: [] });
    expect(none).toMatchObject({ verdict: 'invalid', accepted: false });
    expect(none.reasons).toContain('negative-controls-incomplete');

    const duplicated = paired(target, attempts, { controls: [...safeControls(['baseline', 'candidate']), safeControls(['candidate'])[0]] });
    expect(duplicated.negative_controls).toMatchObject({ duplicates: 1, complete: false });
    expect(duplicated.accepted).toBe(false);
  });
});

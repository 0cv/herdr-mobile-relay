#!/usr/bin/env node
/**
 * Analyzer for the hosted resume benchmark.
 *
 * Every valid attempted wake epoch is in the denominator. A non-completion is
 * right-censored at the fixed deadline (an infinite time here), keeps its
 * reason, and is never given a completed latency. Success-only percentiles are
 * reported as supplementary diagnostics only. Quantiles use the nearest-rank
 * definition over all attempts; a quantile that lands among censored values is
 * not identifiable, and neither is a bound that does.
 *
 * Pilot evidence (one variant) yields variance and workload estimates for
 * planning, never acceptance. Paired evidence (baseline/candidate) yields
 * one-sided, multiplicity-adjusted bounds:
 *   - reliability: Newcombe's hybrid-score interval for the paired difference
 *     in non-completion rates (method 10, Statistics in Medicine 1998);
 *   - latency: a paired (block) percentile bootstrap of the all-attempt
 *     quantile ratio candidate/baseline.
 * Family-wise error is controlled with Bonferroni over every preregistered
 * acceptance comparison; the adjusted level, method and counts are retained.
 */
import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

export const ANALYSIS_SCHEMA = 'herdr-resume-benchmark-analysis/1';
export const EVIDENCE_SCHEMA = 'herdr-resume-benchmark/1';
export const DEFAULT_DEADLINE_MS = 60_000;
export const DEFAULT_BOUNDS = Object.freeze({
  reliability_margin: 0.01,
  target_p95_ratio: 0.8,
  regression_ratio: 1.1,
  min_pairs: 400,
  alpha: 0.05,
  power: 0.8,
});
const RANK_EPSILON = 1e-9;

/**
 * Inverse standard normal CDF (Acklam's rational approximation, relative
 * error below 1.2e-9 across the open unit interval).
 *
 * @param {number} p
 * @returns {number}
 */
export function normalQuantile(p) {
  if (!(p > 0 && p < 1)) throw new RangeError('normalQuantile needs 0 < p < 1');
  const a = [-3.969683028665376e1, 2.209460984245205e2, -2.759285104469687e2, 1.38357751867269e2, -3.066479806614716e1, 2.506628277459239];
  const b = [-5.447609879822406e1, 1.615858368580409e2, -1.556989798598866e2, 6.680131188771972e1, -1.328068155288572e1];
  const c = [-7.784894002430293e-3, -3.223964580411365e-1, -2.400758277161838, -2.549732539343734, 4.374664141464968, 2.938163982698783];
  const d = [7.784695709041462e-3, 3.224671290700398e-1, 2.445134137142996, 3.754408661907416];
  const low = 0.02425;
  if (p < low) {
    const q = Math.sqrt(-2 * Math.log(p));
    return (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5])
      / ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1);
  }
  if (p > 1 - low) return -normalQuantile(1 - p);
  const q = p - 0.5;
  const r = q * q;
  return (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) * r + a[5]) * q
    / (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) * r + 1);
}

/**
 * Wilson score interval for a binomial proportion at critical value z.
 *
 * @param {number} successes
 * @param {number} n
 * @param {number} z
 * @returns {{ estimate: number; lower: number; upper: number }}
 */
export function wilsonInterval(successes, n, z) {
  if (n <= 0) return { estimate: Number.NaN, lower: 0, upper: 1 };
  const p = successes / n;
  const z2 = z * z;
  const denominator = 1 + z2 / n;
  const center = p + z2 / (2 * n);
  const spread = z * Math.sqrt((p * (1 - p)) / n + z2 / (4 * n * n));
  return {
    estimate: p,
    lower: Math.max(0, (center - spread) / denominator),
    upper: Math.min(1, (center + spread) / denominator),
  };
}

/**
 * Newcombe's hybrid score interval (method 10) for p1 - p2 with paired data.
 * Cells: a = both positive, b = first only, c = second only, d = neither.
 *
 * @param {{ a: number; b: number; c: number; d: number }} table
 * @param {number} z
 * @returns {{ estimate: number; lower: number; upper: number; phi: number }}
 */
export function newcombePairedDifference({ a, b, c, d }, z) {
  const n = a + b + c + d;
  if (n <= 0) return { estimate: Number.NaN, lower: -1, upper: 1, phi: 0 };
  const p1 = (a + b) / n;
  const p2 = (a + c) / n;
  const first = wilsonInterval(a + b, n, z);
  const second = wilsonInterval(a + c, n, z);
  const marginProduct = (a + b) * (c + d) * (a + c) * (b + d);
  const phi = marginProduct > 0 ? (a * d - b * c) / Math.sqrt(marginProduct) : 0;
  const estimate = p1 - p2;
  const delta = Math.sqrt(Math.max(0,
    (p1 - first.lower) ** 2 - 2 * phi * (p1 - first.lower) * (second.upper - p2) + (second.upper - p2) ** 2));
  const epsilon = Math.sqrt(Math.max(0,
    (first.upper - p1) ** 2 - 2 * phi * (first.upper - p1) * (p2 - second.lower) + (p2 - second.lower) ** 2));
  return { estimate, lower: Math.max(-1, estimate - delta), upper: Math.min(1, estimate + epsilon), phi };
}

/**
 * Nearest-rank quantile. Censored (infinite) values sort last.
 *
 * @param {number[]} values
 * @param {number} q
 * @returns {number}
 */
export function nearestRankQuantile(values, q) {
  if (!values.length) return Number.NaN;
  const sorted = Float64Array.from(values).sort();
  return sorted[Math.max(0, Math.ceil(q * sorted.length - RANK_EPSILON) - 1)];
}

/**
 * Deterministic PRNG (mulberry32) so bootstrap results are reproducible.
 *
 * @param {number} seed
 * @returns {() => number}
 */
export function seededRandom(seed) {
  let value = seed >>> 0;
  return () => {
    value = (value + 0x6d2b79f5) >>> 0;
    let t = value;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * Percentile bootstrap interval for one arm's all-attempt quantile.
 *
 * @param {number[]} values
 * @param {number} q
 * @param {number} level two-sided coverage, e.g. 0.95
 * @param {number} resamples
 * @param {number} seed
 * @returns {{ lower: number; upper: number }}
 */
export function bootstrapQuantileInterval(values, q, level, resamples, seed) {
  if (!values.length) return { lower: Number.NaN, upper: Number.NaN };
  const random = seededRandom(seed);
  const replicates = new Float64Array(resamples);
  const sample = new Array(values.length);
  for (let replicate = 0; replicate < resamples; replicate += 1) {
    for (let index = 0; index < values.length; index += 1) sample[index] = values[Math.floor(random() * values.length)];
    replicates[replicate] = nearestRankQuantile(sample, q);
  }
  replicates.sort();
  const tail = (1 - level) / 2;
  return {
    lower: replicates[Math.max(0, Math.ceil(tail * resamples - RANK_EPSILON) - 1)],
    upper: replicates[Math.max(0, Math.ceil((1 - tail) * resamples - RANK_EPSILON) - 1)],
  };
}

/**
 * One-sided upper bound for the paired quantile ratio candidate/baseline.
 * Blocks (independent pairs, or pairs sharing a scenario block) are resampled
 * with replacement. A replicate whose candidate quantile is censored is +∞;
 * one whose baseline quantile is censored is unknown and counted as +∞, so
 * censoring can only widen the bound.
 *
 * @param {{ baseline: number; candidate: number; block?: string }[]} pairs
 * @param {number} q
 * @param {number} alpha one-sided level after multiplicity adjustment
 * @param {number} resamples
 * @param {number} seed
 * @returns {{ estimate: number; upper: number; identifiable: boolean }}
 */
export function pairedQuantileRatioBound(pairs, q, alpha, resamples, seed) {
  const ratio = (/** @type {number[]} */ candidate, /** @type {number[]} */ baseline) => {
    const top = nearestRankQuantile(candidate, q);
    const bottom = nearestRankQuantile(baseline, q);
    if (!Number.isFinite(top) || !Number.isFinite(bottom) || bottom <= 0) return Number.POSITIVE_INFINITY;
    return top / bottom;
  };
  if (!pairs.length) return { estimate: Number.NaN, upper: Number.POSITIVE_INFINITY, identifiable: false };
  const estimate = ratio(pairs.map((pair) => pair.candidate), pairs.map((pair) => pair.baseline));
  /** @type {Map<string, { baseline: number; candidate: number }[]>} */
  const grouped = new Map();
  pairs.forEach((pair, index) => {
    const key = pair.block ?? `pair-${index}`;
    const members = grouped.get(key) ?? [];
    members.push(pair);
    grouped.set(key, members);
  });
  const blocks = [...grouped.values()];
  const random = seededRandom(seed);
  const replicates = new Float64Array(resamples);
  for (let replicate = 0; replicate < resamples; replicate += 1) {
    /** @type {number[]} */
    const candidate = [];
    /** @type {number[]} */
    const baseline = [];
    for (let draw = 0; draw < blocks.length; draw += 1) {
      for (const pair of blocks[Math.floor(random() * blocks.length)]) {
        candidate.push(pair.candidate);
        baseline.push(pair.baseline);
      }
    }
    replicates[replicate] = ratio(candidate, baseline);
  }
  replicates.sort();
  const upper = replicates[Math.max(0, Math.ceil((1 - alpha) * resamples - RANK_EPSILON) - 1)];
  return { estimate, upper, identifiable: Number.isFinite(estimate) && Number.isFinite(upper) };
}

/** @param {number} value @param {number} deadline */
function reportable(value, deadline) {
  if (Number.isNaN(value)) return null;
  return Number.isFinite(value) && value <= deadline ? Math.round(value) : 'beyond-deadline';
}

/** @param {number} value */
function round6(value) {
  return Number.isFinite(value) ? Math.round(value * 1e6) / 1e6 : null;
}

/**
 * @typedef {{
 *   stratum: string;
 *   variant?: 'baseline' | 'candidate';
 *   pair?: number;
 *   block?: string;
 *   seed?: number;
 *   order?: string;
 *   replacement?: number;
 *   outcome?: string;
 *   time_to_fresh_ms?: number | null;
 *   harness_invalid?: string | null;
 *   dials?: number;
 *   handshakes?: number;
 *   bytes?: number;
 *   hidden_dials?: number;
 *   hidden_bytes?: number;
 * }} AttemptRecord
 */

/**
 * Classifies one attempted epoch. Only a fresh outcome with a time inside the
 * deadline completes; everything else is censored with its reason.
 *
 * @param {AttemptRecord | undefined} record
 * @param {number} deadline
 * @param {string[]} allowedExclusions
 * @returns {{ excluded: string | null; completed: boolean; time: number; reason: string | null }}
 */
export function classifyAttempt(record, deadline, allowedExclusions) {
  if (!record) return { excluded: null, completed: false, time: Number.POSITIVE_INFINITY, reason: 'missing-outcome' };
  if (record.harness_invalid) {
    if (allowedExclusions.includes(record.harness_invalid)) {
      return { excluded: record.harness_invalid, completed: false, time: Number.POSITIVE_INFINITY, reason: null };
    }
    return { excluded: null, completed: false, time: Number.POSITIVE_INFINITY, reason: `disallowed-exclusion:${record.harness_invalid}` };
  }
  const time = Number(record.time_to_fresh_ms);
  if (record.outcome === 'fresh' && Number.isFinite(time) && time >= 0 && time <= deadline) {
    return { excluded: null, completed: true, time, reason: null };
  }
  if (record.outcome === 'fresh') return { excluded: null, completed: false, time: Number.POSITIVE_INFINITY, reason: 'late' };
  return {
    excluded: null,
    completed: false,
    time: Number.POSITIVE_INFINITY,
    reason: typeof record.outcome === 'string' && record.outcome ? record.outcome : 'missing-outcome',
  };
}

/** @param {Record<string, number>} counts @param {string | null} key */
function tally(counts, key) {
  if (key) counts[key] = (counts[key] || 0) + 1;
}

/** @param {number[]} values */
function mean(values) {
  return values.length ? values.reduce((total, value) => total + value, 0) / values.length : null;
}

/** @param {number[]} values */
function sampleSd(values) {
  if (values.length < 2) return null;
  const average = /** @type {number} */ (mean(values));
  return Math.sqrt(values.reduce((total, value) => total + (value - average) ** 2, 0) / (values.length - 1));
}

/**
 * Pairs needed for a paired non-inferiority test of non-completion rates,
 * assuming no true difference and a discordance rate psi (normal
 * approximation), plus the count that a zero-failure result needs for its
 * Wilson bound to fall inside the margin. Both are planning aids only.
 *
 * @param {number} failureRate
 * @param {number} margin
 * @param {number} alpha one-sided, after multiplicity adjustment
 * @param {number} power
 */
export function reliabilityPlanning(failureRate, margin, alpha, power) {
  const zAlpha = normalQuantile(1 - alpha);
  const zBeta = normalQuantile(power);
  const discordance = 2 * failureRate * (1 - failureRate);
  const nonInferiority = Math.ceil(((zAlpha + zBeta) ** 2 * discordance) / (margin * margin));
  const zeroFailure = Math.ceil((zAlpha * zAlpha * (1 - margin)) / margin);
  return {
    assumed_failure_rate: round6(failureRate),
    assumed_discordance: round6(discordance),
    alpha: round6(alpha),
    power,
    non_inferiority_pairs: nonInferiority,
    zero_failure_precision_pairs: zeroFailure,
    recommended_minimum_pairs: Math.max(DEFAULT_BOUNDS.min_pairs, nonInferiority, zeroFailure),
  };
}

/**
 * @param {{ preregistration?: Record<string, any>; attempts?: AttemptRecord[]; negative_controls?: Record<string, any>[] }} evidence
 */
function settings(evidence) {
  const registration = evidence.preregistration || {};
  const analysis = registration.analysis || {};
  return {
    deadline: Number(registration.deadline_ms) || DEFAULT_DEADLINE_MS,
    bounds: { ...DEFAULT_BOUNDS, ...(registration.bounds || {}) },
    allowedExclusions: Array.isArray(registration.harness_invalid_criteria)
      ? registration.harness_invalid_criteria.map((entry) => String(entry.id || entry))
      : [],
    resamples: Number(analysis.bootstrap_resamples) || 2_000,
    seed: Number(analysis.bootstrap_seed) || 1,
    planned: Number(registration.sample_size_per_stratum) || 0,
    plannedFamily: Number(analysis.planned_family_size) || 2,
    strata: Array.isArray(registration.strata) ? registration.strata : [],
  };
}

/**
 * @param {Record<string, any>[]} controls
 */
function negativeControlSummary(controls) {
  const results = controls.map((control) => ({
    control: String(control.control || ''),
    browser: String(control.browser || ''),
    expected: String(control.expected || ''),
    observed: String(control.observed || ''),
    safety: control.safety === 'pass' ? 'pass' : 'fail',
    // A refusal is the expected safe behaviour; it is never a latency success.
    counted_as_latency_success: false,
  }));
  return { results, all_safe: results.every((result) => result.safety === 'pass') };
}

/**
 * Pilot: one variant, every attempted epoch. Produces descriptive estimates
 * with uncertainty and planning inputs for a later confirmatory experiment.
 *
 * @param {{ preregistration?: Record<string, any>; attempts?: AttemptRecord[]; negative_controls?: Record<string, any>[] }} evidence
 */
export function analyzePilot(evidence) {
  const config = settings(evidence);
  const attempts = evidence.attempts || [];
  /** @type {Map<string, AttemptRecord[]>} */
  const byStratum = new Map();
  for (const name of config.strata.map((/** @type {any} */ stratum) => String(stratum.id || stratum))) byStratum.set(name, []);
  for (const record of attempts) {
    const list = byStratum.get(record.stratum) ?? [];
    list.push(record);
    byStratum.set(record.stratum, list);
  }
  const familyAlpha = config.bounds.alpha / config.plannedFamily;
  const strata = [...byStratum.entries()].map(([stratum, records], index) => {
    /** @type {Record<string, number>} */
    const reasons = {};
    /** @type {Record<string, number>} */
    const exclusions = {};
    /** @type {number[]} */
    const all = [];
    /** @type {number[]} */
    const successes = [];
    for (const record of records) {
      const result = classifyAttempt(record, config.deadline, config.allowedExclusions);
      if (result.excluded) {
        tally(exclusions, result.excluded);
        continue;
      }
      all.push(result.time);
      if (result.completed) successes.push(result.time);
      else tally(reasons, result.reason);
    }
    const valid = all.length;
    const completion = wilsonInterval(successes.length, valid, normalQuantile(0.975));
    const seed = config.seed + index * 101;
    const p50 = bootstrapQuantileInterval(all, 0.5, 0.95, config.resamples, seed);
    const p95 = bootstrapQuantileInterval(all, 0.95, 0.95, config.resamples, seed + 1);
    const logs = successes.filter((value) => value > 0).map((value) => Math.log(value));
    const numeric = (/** @type {keyof AttemptRecord} */ key) => records
      .map((record) => Number(record[key]))
      .filter((value) => Number.isFinite(value));
    return {
      stratum,
      attempted: records.length,
      planned: config.planned,
      sample_requirement_met: records.length >= config.planned,
      excluded: { count: records.length - valid, reasons: exclusions },
      valid_attempts: valid,
      completed_on_time: successes.length,
      completion_rate: round6(completion.estimate),
      completion_rate_ci95: [round6(completion.lower), round6(completion.upper)],
      non_completions: reasons,
      time_to_fresh_ms: {
        p50: reportable(nearestRankQuantile(all, 0.5), config.deadline),
        p90: reportable(nearestRankQuantile(all, 0.9), config.deadline),
        p95: reportable(nearestRankQuantile(all, 0.95), config.deadline),
        p50_ci95: [reportable(p50.lower, config.deadline), reportable(p50.upper, config.deadline)],
        p95_ci95: [reportable(p95.lower, config.deadline), reportable(p95.upper, config.deadline)],
      },
      successful_only_ms: {
        supplementary: true,
        n: successes.length,
        p50: reportable(nearestRankQuantile(successes, 0.5), config.deadline),
        p95: reportable(nearestRankQuantile(successes, 0.95), config.deadline),
      },
      log_time_sd: round6(sampleSd(logs) ?? Number.NaN),
      workload: {
        dials_mean: round6(mean(numeric('dials')) ?? Number.NaN),
        handshakes_mean: round6(mean(numeric('handshakes')) ?? Number.NaN),
        bytes_mean: round6(mean(numeric('bytes')) ?? Number.NaN),
        hidden_dials_total: numeric('hidden_dials').reduce((total, value) => total + value, 0),
        hidden_bytes_total: numeric('hidden_bytes').reduce((total, value) => total + value, 0),
      },
      planning: reliabilityPlanning(
        valid ? (valid - successes.length) / valid : 0,
        config.bounds.reliability_margin,
        familyAlpha,
        config.bounds.power,
      ),
    };
  });
  const controls = negativeControlSummary(evidence.negative_controls || []);
  return {
    schema: ANALYSIS_SCHEMA,
    design: 'pilot',
    claim: 'none',
    note: 'Pilot estimates of variance and workload only. It is not p95 acceptance and supports no performance claim.',
    deadline_ms: config.deadline,
    quantile_method: 'nearest-rank over all valid attempts; non-completions censored at the deadline',
    interval_methods: {
      completion_rate: 'Wilson score, two-sided 95%',
      quantiles: `percentile bootstrap, two-sided 95%, ${config.resamples} resamples`,
    },
    planning_assumptions: {
      family_size: config.plannedFamily,
      adjusted_alpha: round6(familyAlpha),
      adjustment: 'bonferroni',
      margin: config.bounds.reliability_margin,
      power: config.bounds.power,
      discordance: 'independent failures at the pilot rate in both arms (2p(1-p)); a correlated pair design needs fewer pairs',
    },
    strata,
    negative_controls: controls,
    sample_requirement_met: strata.every((stratum) => stratum.sample_requirement_met),
  };
}

/**
 * Paired confirmatory analysis.
 *
 * @param {{ preregistration?: Record<string, any>; attempts?: AttemptRecord[]; negative_controls?: Record<string, any>[] }} evidence
 */
export function analyzePaired(evidence) {
  const config = settings(evidence);
  const attempts = evidence.attempts || [];
  const strataPlan = config.strata.map((/** @type {any} */ stratum) => ({
    id: String(stratum.id || stratum),
    role: stratum.role === 'regression' ? 'regression' : 'acceptance-target',
  }));
  const family = strataPlan.reduce((total, stratum) => total + (stratum.role === 'regression' ? 3 : 2), 0) || 1;
  const adjustedAlpha = config.bounds.alpha / family;
  const z = normalQuantile(1 - adjustedAlpha);
  const strata = strataPlan.map((plan, index) => {
    /** @type {Map<string, { baseline?: AttemptRecord; candidate?: AttemptRecord }>} */
    const rounds = new Map();
    for (const record of attempts) {
      if (record.stratum !== plan.id) continue;
      const key = `${record.pair ?? 0}:${record.replacement ?? 0}`;
      const round = rounds.get(key) ?? {};
      if (record.variant === 'candidate') round.candidate = record;
      else round.baseline = record;
      rounds.set(key, round);
    }
    /** @type {Map<number, { baseline: ReturnType<typeof classifyAttempt>; candidate: ReturnType<typeof classifyAttempt>; block?: string }>} */
    const pairs = new Map();
    /** @type {Record<string, number>} */
    const exclusions = {};
    const replacementCounts = new Map();
    for (const [key, round] of [...rounds.entries()].sort(([left], [right]) => left.localeCompare(right, 'en', { numeric: true }))) {
      const pair = Number(key.split(':')[0]);
      replacementCounts.set(pair, (replacementCounts.get(pair) || 0) + 1);
      const baseline = classifyAttempt(round.baseline, config.deadline, config.allowedExclusions);
      const candidate = classifyAttempt(round.candidate, config.deadline, config.allowedExclusions);
      // An objective infrastructure exclusion removes both variants of the pair.
      if (baseline.excluded || candidate.excluded) {
        tally(exclusions, baseline.excluded || candidate.excluded);
        continue;
      }
      if (!pairs.has(pair)) pairs.set(pair, { baseline, candidate, block: round.baseline?.block ?? round.candidate?.block });
    }
    const valid = [...pairs.values()];
    const table = { a: 0, b: 0, c: 0, d: 0 };
    /** @type {Record<string, number>} */
    const baselineReasons = {};
    /** @type {Record<string, number>} */
    const candidateReasons = {};
    for (const pair of valid) {
      const candidateFailed = !pair.candidate.completed;
      const baselineFailed = !pair.baseline.completed;
      if (candidateFailed && baselineFailed) table.a += 1;
      else if (candidateFailed) table.b += 1;
      else if (baselineFailed) table.c += 1;
      else table.d += 1;
      tally(baselineReasons, pair.baseline.reason);
      tally(candidateReasons, pair.candidate.reason);
    }
    const reliability = newcombePairedDifference(table, z);
    const reasons = [];
    const enough = valid.length >= Math.max(config.planned, config.bounds.min_pairs);
    if (!enough) reasons.push('insufficient-pairs');
    const reliabilityVerdict = !enough
      ? 'inconclusive'
      : reliability.upper <= config.bounds.reliability_margin ? 'pass' : 'not-accepted';
    const latencyPairs = valid.map((pair) => ({ baseline: pair.baseline.time, candidate: pair.candidate.time, block: pair.block }));
    const seed = config.seed + index * 101;
    const p95 = pairedQuantileRatioBound(latencyPairs, 0.95, adjustedAlpha, config.resamples, seed);
    const threshold95 = plan.role === 'regression' ? config.bounds.regression_ratio : config.bounds.target_p95_ratio;
    /** @param {{ upper: number; identifiable: boolean }} bound @param {number} threshold */
    const verdictFor = (bound, threshold) => {
      if (!enough || !bound.identifiable) return 'inconclusive';
      return bound.upper <= threshold ? 'pass' : 'not-accepted';
    };
    const latency = {
      p95_ratio: {
        estimate: round6(p95.estimate),
        upper_bound: round6(p95.upper),
        threshold: threshold95,
        identifiable: p95.identifiable,
        verdict: verdictFor(p95, threshold95),
      },
    };
    if (plan.role === 'regression') {
      const p50 = pairedQuantileRatioBound(latencyPairs, 0.5, adjustedAlpha, config.resamples, seed + 1);
      Object.assign(latency, {
        p50_ratio: {
          estimate: round6(p50.estimate),
          upper_bound: round6(p50.upper),
          threshold: config.bounds.regression_ratio,
          identifiable: p50.identifiable,
          verdict: verdictFor(p50, config.bounds.regression_ratio),
        },
      });
    }
    if (!p95.identifiable) reasons.push('p95-not-identifiable-below-deadline');
    const verdicts = [reliabilityVerdict, ...Object.values(latency).map((entry) => entry.verdict)];
    const verdict = verdicts.includes('not-accepted')
      ? 'not-accepted'
      : verdicts.every((entry) => entry === 'pass') ? 'pass' : 'inconclusive';
    const successful = valid.filter((pair) => pair.baseline.completed && pair.candidate.completed);
    const baselineSuccess = valid.filter((pair) => pair.baseline.completed).map((pair) => pair.baseline.time);
    const candidateSuccess = valid.filter((pair) => pair.candidate.completed).map((pair) => pair.candidate.time);
    return {
      stratum: plan.id,
      role: plan.role,
      planned_pairs: Math.max(config.planned, config.bounds.min_pairs),
      valid_pairs: valid.length,
      excluded_pairs: { count: Object.values(exclusions).reduce((total, value) => total + value, 0), reasons: exclusions },
      paired_outcomes: {
        both_completed: table.d,
        baseline_only_failed: table.c,
        candidate_only_failed: table.b,
        both_failed: table.a,
      },
      completion_rate: {
        baseline: round6(valid.length ? (table.d + table.b) / valid.length : Number.NaN),
        candidate: round6(valid.length ? (table.d + table.c) / valid.length : Number.NaN),
      },
      non_completions: { baseline: baselineReasons, candidate: candidateReasons },
      reliability: {
        difference: round6(reliability.estimate),
        upper_bound: round6(reliability.upper),
        margin: config.bounds.reliability_margin,
        method: 'newcombe-hybrid-score-paired (method 10)',
        verdict: reliabilityVerdict,
      },
      latency,
      successful_only: {
        supplementary: true,
        pairs: successful.length,
        p95_ratio: round6(nearestRankQuantile(candidateSuccess, 0.95) / nearestRankQuantile(baselineSuccess, 0.95)),
      },
      verdict,
      reasons,
    };
  });
  const verdicts = strata.map((stratum) => stratum.verdict);
  const overall = verdicts.includes('not-accepted')
    ? 'not-accepted'
    : verdicts.length && verdicts.every((verdict) => verdict === 'pass') ? 'pass' : 'inconclusive';
  const controls = negativeControlSummary(evidence.negative_controls || []);
  return {
    schema: ANALYSIS_SCHEMA,
    design: 'paired',
    deadline_ms: config.deadline,
    multiplicity: {
      method: 'bonferroni',
      one_sided: true,
      family_size: family,
      alpha: config.bounds.alpha,
      adjusted_alpha: round6(adjustedAlpha),
      critical_z: round6(z),
    },
    bootstrap: { resamples: config.resamples, seed: config.seed, unit: 'pair or declared block' },
    strata,
    negative_controls: controls,
    verdict: controls.all_safe ? overall : 'not-accepted',
    accepted: controls.all_safe && overall === 'pass',
  };
}

/**
 * @param {{
 *   schema?: string;
 *   design?: string;
 *   preregistration?: Record<string, any>;
 *   attempts?: AttemptRecord[];
 *   negative_controls?: Record<string, any>[];
 * }} evidence
 */
export function analyzeEvidence(evidence) {
  if (evidence.schema !== EVIDENCE_SCHEMA) throw new Error(`unsupported evidence schema ${String(evidence.schema)}`);
  return evidence.design === 'paired' ? analyzePaired(evidence) : analyzePilot(evidence);
}

/**
 * @param {unknown} value
 * @returns {string}
 */
function cell(value) {
  if (value === null || value === undefined) return 'n/a';
  if (Array.isArray(value)) return value.map(cell).join(' – ');
  return String(value);
}

/** @param {ReturnType<typeof analyzePilot> | ReturnType<typeof analyzePaired>} analysis */
export function renderMarkdown(analysis) {
  const lines = [];
  if (analysis.design === 'pilot') {
    const pilot = /** @type {ReturnType<typeof analyzePilot>} */ (analysis);
    lines.push('## Resume benchmark pilot (no performance claim)', '', pilot.note, '');
    lines.push('| Stratum | Attempted | Valid | On time | Completion (95% CI) | p50 ms (95% CI) | p95 ms (95% CI) | Non-completions |');
    lines.push('| --- | --- | --- | --- | --- | --- | --- | --- |');
    for (const stratum of pilot.strata) {
      lines.push(`| ${stratum.stratum} | ${stratum.attempted} | ${stratum.valid_attempts} | ${stratum.completed_on_time} | ${cell(stratum.completion_rate)} (${cell(stratum.completion_rate_ci95)}) | ${cell(stratum.time_to_fresh_ms.p50)} (${cell(stratum.time_to_fresh_ms.p50_ci95)}) | ${cell(stratum.time_to_fresh_ms.p95)} (${cell(stratum.time_to_fresh_ms.p95_ci95)}) | ${cell(JSON.stringify(stratum.non_completions))} |`);
    }
  } else {
    const paired = /** @type {ReturnType<typeof analyzePaired>} */ (analysis);
    lines.push(`## Resume benchmark paired analysis: ${paired.verdict}`, '');
    lines.push(`Bonferroni family ${paired.multiplicity.family_size}, adjusted one-sided alpha ${paired.multiplicity.adjusted_alpha}.`, '');
    lines.push('| Stratum | Role | Pairs | Reliability upper bound | p95 ratio upper bound | Verdict |');
    lines.push('| --- | --- | --- | --- | --- | --- |');
    for (const stratum of paired.strata) {
      lines.push(`| ${stratum.stratum} | ${stratum.role} | ${stratum.valid_pairs}/${stratum.planned_pairs} | ${cell(stratum.reliability.upper_bound)} | ${cell(stratum.latency.p95_ratio.upper_bound)} | ${stratum.verdict} |`);
    }
  }
  lines.push('', '### Negative controls (safety assertions, not latency successes)', '');
  for (const control of analysis.negative_controls.results) {
    lines.push(`- ${control.browser} ${control.control}: expected ${control.expected}, observed ${control.observed} — ${control.safety}`);
  }
  return `${lines.join('\n')}\n`;
}

async function main() {
  const args = process.argv.slice(2);
  const input = args.find((arg) => !arg.startsWith('--'));
  const option = (/** @type {string} */ name) => {
    const index = args.indexOf(name);
    return index >= 0 ? args[index + 1] : undefined;
  };
  if (!input) {
    console.error('usage: analyze-resume-benchmarks.mjs <evidence.json> [--out analysis.json] [--markdown summary.md]');
    process.exit(2);
  }
  const evidence = JSON.parse(await readFile(input, 'utf8'));
  const analysis = analyzeEvidence(evidence);
  const out = option('--out');
  const markdown = option('--markdown');
  if (out) await writeFile(out, `${JSON.stringify(analysis, null, 2)}\n`);
  if (markdown) await writeFile(markdown, renderMarkdown(analysis));
  process.stdout.write(renderMarkdown(analysis));
  const complete = analysis.design === 'pilot'
    ? /** @type {ReturnType<typeof analyzePilot>} */ (analysis).sample_requirement_met
    : true;
  if (!analysis.negative_controls.all_safe || !complete) process.exit(1);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}

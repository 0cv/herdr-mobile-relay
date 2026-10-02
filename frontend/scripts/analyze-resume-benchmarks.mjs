#!/usr/bin/env node
/**
 * Analyzer for the hosted resume benchmark.
 *
 * Every valid attempted wake epoch is in the denominator. A non-completion is
 * right-censored at the fixed deadline (an infinite time here), keeps its
 * reason, and is never given a completed latency; a "fresh" record without a
 * finite numeric time is a missing measurement, not a zero. Success-only
 * percentiles are reported as supplementary diagnostics only. Quantiles use
 * the nearest-rank definition over all attempts; a quantile that lands among
 * censored values is not identifiable, and neither is a bound that does.
 *
 * The registration itself must stay inside the approved contract: the fixed
 * 60-second deadline, bounds no laxer than the defaults (stricter is
 * allowed), only the objective infrastructure exclusions, every required
 * negative control, and at least 30 epochs per pilot stratum. Evidence is
 * then checked against its preregistration before any statistic: only
 * preregistered strata, pair indices, seeds and bounded replacement rounds
 * count, so collecting more than the fixed sample cannot buy precision. The
 * preregistered negative controls must be complete for every browser, trial
 * and variant, and all safe.
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
 * A bound inside its threshold passes; a lower bound beyond it demonstrates a
 * violation (not-accepted); anything else is inconclusive. Either way the
 * baseline is kept unless every comparison passes.
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

/** @param {string} text */
function hash32(text) {
  let value = 0x811c9dc5;
  for (let index = 0; index < text.length; index += 1) {
    value ^= text.charCodeAt(index);
    value = Math.imul(value, 0x01000193) >>> 0;
  }
  return value >>> 0;
}

/**
 * The preregistered seed for one epoch (or one matched pair): both variants
 * of a pair use exactly the same seed, so their scripted conditions match.
 *
 * @param {number} baseSeed
 * @param {string} stratum
 * @param {number} index
 */
export function epochSeed(baseSeed, stratum, index) {
  return (hash32(`${baseSeed}:${stratum}:${index}`) % 2_000_000_000) + 1;
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
 * One-sided bounds for the paired quantile ratio candidate/baseline. Blocks
 * (independent pairs, or pairs sharing a declared block) are resampled with
 * replacement. Censoring is handled so it can only widen the interval:
 *   - a censored candidate quantile is only known to be at least the
 *     deadline, so the upper bound is +∞ and the lower bound uses the
 *     deadline as the candidate quantile;
 *   - a censored baseline quantile is unknown: +∞ for the upper bound and 0
 *     for the lower one.
 * A comparison whose estimate or upper bound is censored is not
 * identifiable and can never pass; it can still be rejected when even the
 * censoring-aware lower bound exceeds the threshold.
 *
 * @param {{ baseline: number; candidate: number; block?: string }[]} pairs
 * @param {number} q
 * @param {number} alpha one-sided level after multiplicity adjustment
 * @param {number} resamples
 * @param {number} seed
 * @param {number} [deadline] the censoring time of non-completions
 * @returns {{ estimate: number; lower: number; upper: number; identifiable: boolean }}
 */
export function pairedQuantileRatioBound(pairs, q, alpha, resamples, seed, deadline = DEFAULT_DEADLINE_MS) {
  /** @param {number[]} candidate @param {number[]} baseline */
  const quantiles = (candidate, baseline) => ({
    top: nearestRankQuantile(candidate, q),
    bottom: nearestRankQuantile(baseline, q),
  });
  /** @param {{ top: number; bottom: number }} value @param {number} unknown */
  const ratio = ({ top, bottom }, unknown) => {
    if (!Number.isFinite(bottom) || bottom <= 0) return unknown;
    if (!Number.isFinite(top)) return Number.POSITIVE_INFINITY;
    return top / bottom;
  };
  /** The smallest ratio compatible with censoring at the deadline. */
  const lowest = (/** @type {{ top: number; bottom: number }} */ { top, bottom }) => {
    if (!Number.isFinite(bottom) || bottom <= 0) return 0;
    return (Number.isFinite(top) ? top : Math.max(deadline, 0)) / bottom;
  };
  if (!pairs.length) {
    return { estimate: Number.NaN, lower: 0, upper: Number.POSITIVE_INFINITY, identifiable: false };
  }
  const estimate = ratio(quantiles(pairs.map((pair) => pair.candidate), pairs.map((pair) => pair.baseline)), Number.NaN);
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
  const uppers = new Float64Array(resamples);
  const lowers = new Float64Array(resamples);
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
    const value = quantiles(candidate, baseline);
    uppers[replicate] = ratio(value, Number.POSITIVE_INFINITY);
    lowers[replicate] = lowest(value);
  }
  uppers.sort();
  lowers.sort();
  const upper = uppers[Math.max(0, Math.ceil((1 - alpha) * resamples - RANK_EPSILON) - 1)];
  const lower = lowers[Math.max(0, Math.ceil(alpha * resamples - RANK_EPSILON) - 1)];
  return { estimate, lower, upper, identifiable: Number.isFinite(estimate) && Number.isFinite(upper) };
}

/**
 * Decision for one preregistered comparison: a bound inside the threshold
 * passes; a lower bound beyond it demonstrates a violation; anything else,
 * including a precision-limited interval, is inconclusive.
 *
 * @param {{ enough: boolean; identifiable: boolean; lower: number; upper: number; threshold: number }} input
 * @returns {'pass' | 'not-accepted' | 'inconclusive'}
 */
export function boundVerdict({ enough, identifiable, lower, upper, threshold }) {
  if (!enough) return 'inconclusive';
  if (identifiable && upper <= threshold) return 'pass';
  if (lower > threshold) return 'not-accepted';
  return 'inconclusive';
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
 *   time_to_fresh_ms?: unknown;
 *   harness_invalid?: string | null;
 *   dials?: number;
 *   handshakes?: number;
 *   bytes?: number;
 *   hidden_dials?: number;
 *   hidden_bytes?: number;
 * }} AttemptRecord
 * @typedef {{ excluded: string | null; completed: boolean; time: number; reason: string | null }} Classification
 * @typedef {{
 *   preregistration?: Record<string, any>;
 *   attempts?: AttemptRecord[];
 *   negative_controls?: Record<string, any>[];
 * }} Evidence
 */

/**
 * Classifies one attempted epoch. Only a fresh outcome with a finite numeric
 * time inside the deadline completes; everything else is censored with its
 * reason. Absent, null, string or negative times are missing measurements.
 *
 * @param {AttemptRecord | undefined} record
 * @param {number} deadline
 * @param {string[]} allowedExclusions
 * @returns {Classification}
 */
export function classifyAttempt(record, deadline, allowedExclusions) {
  /** @param {string} reason */
  const censored = (reason) => ({ excluded: null, completed: false, time: Number.POSITIVE_INFINITY, reason });
  if (!record) return censored('missing-outcome');
  if (record.harness_invalid) {
    if (allowedExclusions.includes(record.harness_invalid)) {
      return { excluded: record.harness_invalid, completed: false, time: Number.POSITIVE_INFINITY, reason: null };
    }
    return censored(`disallowed-exclusion:${record.harness_invalid}`);
  }
  if (record.outcome === 'fresh') {
    const time = record.time_to_fresh_ms;
    if (typeof time !== 'number' || !Number.isFinite(time) || time < 0) return censored('missing-measurement');
    if (time > deadline) return censored('late');
    return { excluded: null, completed: true, time, reason: null };
  }
  return censored(typeof record.outcome === 'string' && record.outcome ? record.outcome : 'missing-outcome');
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

/** The only harness exclusions the approved contract allows. */
export const OBJECTIVE_EXCLUSIONS = Object.freeze(['server-unavailable', 'browser-unavailable']);
/** Negative controls every registration must declare. */
export const REQUIRED_NEGATIVE_CONTROLS = Object.freeze([
  'revoked-credential', 'cancelled-unlock', 'permanent-outage', 'ios-deferred-pairing',
]);
export const PILOT_MIN_EPOCHS = 30;
/**
 * Each preregistered bound may be stricter than the approved contract, never
 * laxer: smaller margins, ratios and alpha; more pairs and power.
 *
 * @type {Record<string, (value: number) => boolean>}
 */
const BOUND_LIMITS = {
  reliability_margin: (value) => value > 0 && value <= DEFAULT_BOUNDS.reliability_margin,
  target_p95_ratio: (value) => value > 0 && value <= DEFAULT_BOUNDS.target_p95_ratio,
  regression_ratio: (value) => value > 0 && value <= DEFAULT_BOUNDS.regression_ratio,
  min_pairs: (value) => Number.isSafeInteger(value) && value >= DEFAULT_BOUNDS.min_pairs,
  alpha: (value) => value > 0 && value <= DEFAULT_BOUNDS.alpha,
  power: (value) => value >= DEFAULT_BOUNDS.power && value < 1,
};

/**
 * Ways a registration departs from the approved B1/B3 contract. The analyzer
 * refuses such evidence rather than analyse it under relaxed rules: a fixed
 * 60-second deadline, bounds no laxer than the defaults, only the objective
 * infrastructure exclusions, every negative control, and at least 30
 * attempted epochs per pilot stratum.
 *
 * @param {Record<string, any>} registration
 * @param {string} design
 * @returns {string[]}
 */
export function contractViolations(registration, design) {
  /** @type {string[]} */
  const violations = [];
  if (registration.deadline_ms !== DEFAULT_DEADLINE_MS) violations.push('deadline-not-60s');
  const bounds = registration.bounds ?? {};
  if (!bounds || typeof bounds !== 'object' || Array.isArray(bounds)) violations.push('invalid-bounds');
  else {
    for (const [key, valid] of Object.entries(BOUND_LIMITS)) {
      if (bounds[key] !== undefined && !(typeof bounds[key] === 'number' && valid(bounds[key]))) {
        violations.push(`relaxed-${key}`);
      }
    }
  }
  const criteria = registration.harness_invalid_criteria ?? [];
  if (!Array.isArray(criteria)
    || criteria.some((/** @type {any} */ entry) => !OBJECTIVE_EXCLUSIONS.includes(String(entry?.id ?? entry)))) {
    violations.push('unapproved-exclusion-criterion');
  }
  const controls = Array.isArray(registration.negative_controls) ? registration.negative_controls : [];
  for (const id of REQUIRED_NEGATIVE_CONTROLS) {
    const declared = controls.find((/** @type {any} */ control) => control?.id === id);
    const trials = Number(declared?.trials_per_browser);
    if (!declared || !Number.isSafeInteger(trials) || trials < 1) {
      violations.push('negative-control-not-registered');
      break;
    }
  }
  if (design !== 'paired' && !(Number(registration.sample_size_per_stratum) >= PILOT_MIN_EPOCHS)) {
    violations.push('pilot-sample-below-30');
  }
  return violations;
}

/**
 * @param {Evidence} evidence
 * @param {string} design
 */
function settings(evidence, design) {
  const registration = evidence.preregistration || {};
  const analysis = registration.analysis || {};
  const strata = Array.isArray(registration.strata) ? registration.strata : [];
  const baseSeed = Number(registration.seeds?.base_seed);
  const browsers = Array.isArray(registration.browsers)
    ? registration.browsers.map(String)
    : [...new Set(strata.map((/** @type {any} */ stratum) => String(stratum.browser || '')).filter(Boolean))];
  const replacementLimit = Number(registration.replacement_limit);
  const supplied = registration.bounds && typeof registration.bounds === 'object' ? registration.bounds : {};
  /** @type {Record<string, number>} */
  const stricter = {};
  for (const [key, valid] of Object.entries(BOUND_LIMITS)) {
    if (typeof supplied[key] === 'number' && valid(supplied[key])) stricter[key] = supplied[key];
  }
  return {
    registration,
    contract: contractViolations(registration, design),
    // The deadline is fixed: a registration cannot extend it.
    deadline: DEFAULT_DEADLINE_MS,
    bounds: { ...DEFAULT_BOUNDS, ...stricter },
    allowedExclusions: Array.isArray(registration.harness_invalid_criteria)
      ? registration.harness_invalid_criteria
        .map((/** @type {any} */ entry) => String(entry?.id ?? entry))
        .filter((/** @type {string} */ id) => OBJECTIVE_EXCLUSIONS.includes(id))
      : [],
    resamples: Number(analysis.bootstrap_resamples) || 2_000,
    seed: Number(analysis.bootstrap_seed) || 1,
    baseSeed: Number.isFinite(baseSeed) ? baseSeed : null,
    planned: Number.isSafeInteger(Number(registration.sample_size_per_stratum))
      ? Number(registration.sample_size_per_stratum)
      : 0,
    replacementLimit: Number.isSafeInteger(replacementLimit) && replacementLimit >= 0 ? replacementLimit : 0,
    plannedFamily: Number(analysis.planned_family_size) || 2,
    strata,
    browsers,
  };
}

/**
 * Checks every attempt record against the preregistered universe and groups
 * the admissible ones into rounds: stratum → pair index → replacement round →
 * variant. A replacement round is admissible only after an excluded round,
 * and at most `replacement_limit` of them. Anything else is a violation.
 *
 * @param {AttemptRecord[]} attempts
 * @param {ReturnType<typeof settings>} config
 * @param {Array<'baseline' | 'candidate'>} variants
 */
export function indexAttempts(attempts, config, variants) {
  const known = new Set(config.strata.map((/** @type {any} */ stratum) => String(stratum.id || stratum)));
  /** @type {Record<string, number>} */
  const violations = {};
  /** @type {Map<string, Record<string, number>>} */
  const stratumViolations = new Map();
  /** @type {Map<string, Map<number, Map<number, Partial<Record<'baseline' | 'candidate', AttemptRecord>>>>>} */
  const index = new Map();
  /** @param {string} stratum @param {string} code */
  const violate = (stratum, code) => {
    tally(violations, code);
    const forStratum = stratumViolations.get(stratum) ?? {};
    tally(forStratum, code);
    stratumViolations.set(stratum, forStratum);
  };
  for (const record of attempts) {
    const stratum = String(record?.stratum ?? '');
    if (!known.has(stratum)) {
      tally(violations, 'unregistered-stratum');
      continue;
    }
    const variant = record.variant ?? (variants.length === 1 ? variants[0] : undefined);
    if (!variant || !variants.includes(variant)) {
      violate(stratum, 'unknown-variant');
      continue;
    }
    const pair = record.pair;
    if (typeof pair !== 'number' || !Number.isSafeInteger(pair) || pair < 0 || pair >= config.planned) {
      violate(stratum, 'pair-outside-preregistered-sample');
      continue;
    }
    const replacement = record.replacement ?? 0;
    if (typeof replacement !== 'number' || !Number.isSafeInteger(replacement) || replacement < 0
      || replacement > config.replacementLimit) {
      violate(stratum, 'replacement-limit-exceeded');
      continue;
    }
    if (config.baseSeed !== null && record.seed !== epochSeed(config.baseSeed, stratum, pair)) {
      violate(stratum, 'seed-mismatch');
      continue;
    }
    const pairs = index.get(stratum) ?? new Map();
    const rounds = pairs.get(pair) ?? new Map();
    const round = rounds.get(replacement) ?? {};
    if (round[variant]) {
      violate(stratum, 'duplicate-record');
      continue;
    }
    round[variant] = record;
    rounds.set(replacement, round);
    pairs.set(pair, rounds);
    index.set(stratum, pairs);
  }
  return { index, violations, stratumViolations };
}

/**
 * Resolves one pair index to its admissible round: the first round in which
 * no variant was excluded for a preregistered infrastructure reason. A later
 * round is a violation unless every earlier round was excluded.
 *
 * @param {Map<number, Partial<Record<'baseline' | 'candidate', AttemptRecord>>> | undefined} rounds
 * @param {ReturnType<typeof settings>} config
 * @param {Array<'baseline' | 'candidate'>} variants
 * @returns {{ status: 'missing' | 'exhausted' | 'valid'; round?: Record<string, Classification>; block?: string; exclusions: string[]; violation: boolean }}
 */
function resolvePair(rounds, config, variants) {
  /** @type {string[]} */
  const exclusions = [];
  if (!rounds || !rounds.size) return { status: 'missing', exclusions, violation: false };
  const order = [...rounds.keys()].sort((left, right) => left - right);
  for (let position = 0; position < order.length; position += 1) {
    if (order[position] !== position) return { status: 'missing', exclusions, violation: true };
    const records = /** @type {Partial<Record<'baseline' | 'candidate', AttemptRecord>>} */ (rounds.get(position));
    /** @type {Record<string, Classification>} */
    const classified = {};
    for (const variant of variants) {
      classified[variant] = classifyAttempt(records[variant], config.deadline, config.allowedExclusions);
    }
    const excluded = variants.map((variant) => classified[variant].excluded).find(Boolean);
    if (excluded) {
      exclusions.push(excluded);
      continue;
    }
    const block = variants.map((variant) => records[variant]?.block).find((value) => value !== undefined);
    // Rounds after an admissible one are unjustified replacements.
    return { status: 'valid', round: classified, block, exclusions, violation: position !== order.length - 1 };
  }
  return { status: 'exhausted', exclusions, violation: false };
}

/**
 * Coverage of the preregistered negative controls. Every declared control
 * must have exactly one passing result for each browser, trial and variant.
 * A refusal is the expected safe behaviour; it is never a latency success.
 *
 * @param {Record<string, any>[]} controls
 * @param {ReturnType<typeof settings>} config
 * @param {Array<'baseline' | 'candidate'>} variants
 */
export function negativeControlSummary(controls, config, variants) {
  const declared = Array.isArray(config.registration.negative_controls) ? config.registration.negative_controls : [];
  /** @type {Set<string>} */
  const expected = new Set();
  for (const control of declared) {
    const trials = Math.max(0, Number(control.trials_per_browser) || 0);
    for (const browser of config.browsers) {
      for (let trial = 0; trial < trials; trial += 1) {
        for (const variant of variants) expected.add(`${String(control.id)}|${browser}|${trial}|${variant}`);
      }
    }
  }
  /** @type {Set<string>} */
  const seen = new Set();
  let duplicates = 0;
  let unexpected = 0;
  const results = controls.map((control) => {
    const result = {
      control: String(control.control || ''),
      browser: String(control.browser || ''),
      variant: String(control.variant || (variants.length === 1 ? variants[0] : '')),
      trial: Number(control.trial),
      expected: String(control.expected || ''),
      observed: String(control.observed || ''),
      safety: control.safety === 'pass' ? 'pass' : 'fail',
      counted_as_latency_success: false,
    };
    const key = `${result.control}|${result.browser}|${result.trial}|${result.variant}`;
    if (seen.has(key)) duplicates += 1;
    else if (!expected.has(key)) unexpected += 1;
    seen.add(key);
    return result;
  });
  const missing = [...expected].filter((key) => !seen.has(key)).length;
  const failed = results.filter((result) => result.safety !== 'pass').length;
  const complete = declared.length > 0 && missing === 0 && duplicates === 0 && unexpected === 0;
  return {
    declared: declared.length,
    expected: expected.size,
    observed: results.length,
    missing,
    duplicates,
    unexpected,
    failed,
    complete,
    all_safe: complete && failed === 0,
    results,
  };
}

/**
 * Pilot: one variant, every attempted epoch. Produces descriptive estimates
 * with uncertainty and planning inputs for a later confirmatory experiment.
 *
 * @param {Evidence} evidence
 */
export function analyzePilot(evidence) {
  const config = settings(evidence, 'pilot');
  /** @type {Array<'baseline' | 'candidate'>} */
  const variants = ['baseline'];
  const { index, violations, stratumViolations } = indexAttempts(evidence.attempts || [], config, variants);
  const familyAlpha = config.bounds.alpha / config.plannedFamily;
  const strata = config.strata.map((/** @type {any} */ plan, /** @type {number} */ position) => {
    const stratum = String(plan.id || plan);
    const pairs = index.get(stratum) ?? new Map();
    /** @type {Record<string, number>} */
    const reasons = {};
    /** @type {Record<string, number>} */
    const exclusions = {};
    /** @type {number[]} */
    const all = [];
    /** @type {number[]} */
    const successes = [];
    /** @type {AttemptRecord[]} */
    const used = [];
    let attempted = 0;
    let missing = 0;
    let exhausted = 0;
    let unjustified = 0;
    for (const rounds of pairs.values()) for (const round of rounds.values()) attempted += Object.keys(round).length;
    for (let pair = 0; pair < config.planned; pair += 1) {
      const resolved = resolvePair(pairs.get(pair), config, variants);
      for (const reason of resolved.exclusions) tally(exclusions, reason);
      if (resolved.violation) unjustified += 1;
      if (resolved.status === 'missing') missing += 1;
      if (resolved.status === 'exhausted') exhausted += 1;
      if (resolved.status !== 'valid' || !resolved.round) continue;
      const result = resolved.round.baseline;
      all.push(result.time);
      if (result.completed) successes.push(result.time);
      else tally(reasons, result.reason);
      const record = pairs.get(pair)?.get(resolved.exclusions.length)?.baseline;
      if (record) used.push(record);
    }
    const ownViolations = { ...(stratumViolations.get(stratum) ?? {}) };
    if (unjustified) ownViolations['unjustified-replacement'] = unjustified;
    const valid = all.length;
    const completion = wilsonInterval(successes.length, valid, normalQuantile(0.975));
    const seed = config.seed + position * 101;
    const p50 = bootstrapQuantileInterval(all, 0.5, 0.95, config.resamples, seed);
    const p95 = bootstrapQuantileInterval(all, 0.95, 0.95, config.resamples, seed + 1);
    const logs = successes.filter((value) => value > 0).map((value) => Math.log(value));
    const numeric = (/** @type {keyof AttemptRecord} */ key) => used
      .map((record) => Number(record[key]))
      .filter((value) => Number.isFinite(value));
    return {
      stratum,
      attempted,
      planned: config.planned,
      // Only measured, non-excluded epochs count toward the planned sample.
      sample_requirement_met: config.planned > 0 && valid === config.planned
        && Object.keys(ownViolations).length === 0,
      excluded: { count: Object.values(exclusions).reduce((total, value) => total + value, 0), reasons: exclusions },
      missing,
      exhausted,
      violations: ownViolations,
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
  const controls = negativeControlSummary(evidence.negative_controls || [], config, variants);
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
    contract_violations: config.contract,
    violations,
    strata,
    negative_controls: controls,
    sample_requirement_met: strata.length > 0 && strata.every((stratum) => stratum.sample_requirement_met)
      && Object.keys(violations).length === 0 && config.contract.length === 0,
  };
}

/**
 * Paired confirmatory analysis.
 *
 * @param {Evidence} evidence
 */
export function analyzePaired(evidence) {
  const config = settings(evidence, 'paired');
  /** @type {Array<'baseline' | 'candidate'>} */
  const variants = ['baseline', 'candidate'];
  const { index, violations, stratumViolations } = indexAttempts(evidence.attempts || [], config, variants);
  const strataPlan = config.strata.map((/** @type {any} */ stratum) => ({
    id: String(stratum.id || stratum),
    role: stratum.role === 'regression' ? 'regression' : 'acceptance-target',
  }));
  const family = strataPlan.reduce((total, stratum) => total + (stratum.role === 'regression' ? 3 : 2), 0) || 1;
  const adjustedAlpha = config.bounds.alpha / family;
  const z = normalQuantile(1 - adjustedAlpha);
  const floorMet = config.planned >= config.bounds.min_pairs;
  const strata = strataPlan.map((plan, position) => {
    const pairs = index.get(plan.id) ?? new Map();
    /** @type {Record<string, number>} */
    const exclusions = {};
    /** @type {Array<{ baseline: Classification; candidate: Classification; block?: string }>} */
    const valid = [];
    let missing = 0;
    let exhausted = 0;
    let unjustified = 0;
    for (let pair = 0; pair < config.planned; pair += 1) {
      const resolved = resolvePair(pairs.get(pair), config, variants);
      for (const reason of resolved.exclusions) tally(exclusions, reason);
      if (resolved.violation) unjustified += 1;
      if (resolved.status === 'missing') missing += 1;
      if (resolved.status === 'exhausted') exhausted += 1;
      if (resolved.status === 'valid' && resolved.round) {
        valid.push({ baseline: resolved.round.baseline, candidate: resolved.round.candidate, block: resolved.block });
      }
    }
    const ownViolations = { ...(stratumViolations.get(plan.id) ?? {}) };
    if (unjustified) ownViolations['unjustified-replacement'] = unjustified;
    const invalid = Object.keys(ownViolations).length > 0;
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
    /** @type {string[]} */
    const reasons = [];
    if (!floorMet) reasons.push('preregistered-sample-below-floor');
    if (valid.length !== config.planned) reasons.push('insufficient-pairs');
    if (invalid) reasons.push('evidence-outside-preregistration');
    const enough = floorMet && !invalid && config.planned > 0 && valid.length === config.planned;
    const reliability = newcombePairedDifference(table, z);
    const reliabilityVerdict = boundVerdict({
      enough,
      identifiable: Number.isFinite(reliability.upper),
      lower: reliability.lower,
      upper: reliability.upper,
      threshold: config.bounds.reliability_margin,
    });
    if (enough && reliabilityVerdict === 'not-accepted') reasons.push('reliability-loss-exceeds-margin');
    if (enough && reliabilityVerdict === 'inconclusive') reasons.push('reliability-bound-too-wide');
    const latencyPairs = valid.map((pair) => ({ baseline: pair.baseline.time, candidate: pair.candidate.time, block: pair.block }));
    const seed = config.seed + position * 101;
    /** @param {number} q @param {number} threshold @param {string} name @param {number} offset */
    const latencyComparison = (q, threshold, name, offset) => {
      const bound = pairedQuantileRatioBound(latencyPairs, q, adjustedAlpha, config.resamples, seed + offset, config.deadline);
      const verdict = boundVerdict({ enough, ...bound, threshold });
      if (!bound.identifiable) reasons.push(`${name}-not-identifiable-below-deadline`);
      else if (enough && verdict === 'not-accepted') reasons.push(`${name}-ratio-exceeds-threshold`);
      else if (enough && verdict === 'inconclusive') reasons.push(`${name}-ratio-bound-too-wide`);
      return {
        estimate: round6(bound.estimate),
        lower_bound: round6(bound.lower),
        upper_bound: round6(bound.upper),
        threshold,
        identifiable: bound.identifiable,
        verdict,
      };
    };
    /** @type {Record<string, ReturnType<typeof latencyComparison>>} */
    const latency = {
      p95_ratio: latencyComparison(
        0.95,
        plan.role === 'regression' ? config.bounds.regression_ratio : config.bounds.target_p95_ratio,
        'p95',
        0,
      ),
    };
    if (plan.role === 'regression') latency.p50_ratio = latencyComparison(0.5, config.bounds.regression_ratio, 'p50', 1);
    const verdicts = [reliabilityVerdict, ...Object.values(latency).map((entry) => entry.verdict)];
    const verdict = invalid
      ? 'invalid'
      : verdicts.includes('not-accepted')
        ? 'not-accepted'
        : verdicts.every((entry) => entry === 'pass') ? 'pass' : 'inconclusive';
    const successful = valid.filter((pair) => pair.baseline.completed && pair.candidate.completed);
    const baselineSuccess = valid.filter((pair) => pair.baseline.completed).map((pair) => pair.baseline.time);
    const candidateSuccess = valid.filter((pair) => pair.candidate.completed).map((pair) => pair.candidate.time);
    return {
      stratum: plan.id,
      role: plan.role,
      planned_pairs: config.planned,
      valid_pairs: valid.length,
      missing_pairs: missing,
      excluded_pairs: {
        rounds: Object.values(exclusions).reduce((total, value) => total + value, 0),
        exhausted,
        reasons: exclusions,
      },
      violations: ownViolations,
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
        lower_bound: round6(reliability.lower),
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
  const controls = negativeControlSummary(evidence.negative_controls || [], config, variants);
  const verdicts = strata.map((stratum) => stratum.verdict);
  const globallyInvalid = Object.keys(violations).length > 0 || !strata.length;
  /** @type {string[]} */
  const reasons = [];
  if (controls.failed) reasons.push('negative-control-unsafe');
  if (!controls.complete) reasons.push('negative-controls-incomplete');
  if (globallyInvalid) reasons.push('evidence-outside-preregistration');
  if (config.contract.length) reasons.push('registration-violates-contract');
  let verdict;
  // An unsafe control rejects anything. A registration outside the approved
  // contract is not analysed further: its bounds cannot decide either way.
  if (controls.failed) verdict = 'not-accepted';
  else if (config.contract.length) verdict = 'invalid';
  else if (verdicts.includes('not-accepted')) verdict = 'not-accepted';
  else if (globallyInvalid || !controls.complete || verdicts.includes('invalid')) verdict = 'invalid';
  else if (verdicts.every((entry) => entry === 'pass')) verdict = 'pass';
  else verdict = 'inconclusive';
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
    replacement_limit: config.replacementLimit,
    contract_violations: config.contract,
    violations,
    strata,
    negative_controls: controls,
    reasons,
    verdict,
    accepted: verdict === 'pass',
  };
}

/** @param {Evidence & { schema?: string; design?: string }} evidence */
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
      lines.push(`| ${stratum.stratum} | ${stratum.attempted} | ${stratum.valid_attempts}/${stratum.planned} | ${stratum.completed_on_time} | ${cell(stratum.completion_rate)} (${cell(stratum.completion_rate_ci95)}) | ${cell(stratum.time_to_fresh_ms.p50)} (${cell(stratum.time_to_fresh_ms.p50_ci95)}) | ${cell(stratum.time_to_fresh_ms.p95)} (${cell(stratum.time_to_fresh_ms.p95_ci95)}) | ${cell(JSON.stringify(stratum.non_completions))} |`);
    }
    if (pilot.contract_violations.length) {
      lines.push('', `**The registration departs from the approved contract (${pilot.contract_violations.join(', ')}).**`);
    }
    if (!pilot.sample_requirement_met) lines.push('', '**The preregistered sample was not met; the pilot is incomplete.**');
  } else {
    const paired = /** @type {ReturnType<typeof analyzePaired>} */ (analysis);
    lines.push(`## Resume benchmark paired analysis: ${paired.verdict}`, '');
    if (paired.contract_violations.length) {
      lines.push(`The registration departs from the approved contract (${paired.contract_violations.join(', ')}).`, '');
    }
    lines.push(`Bonferroni family ${paired.multiplicity.family_size}, adjusted one-sided alpha ${paired.multiplicity.adjusted_alpha}.`, '');
    lines.push('| Stratum | Role | Pairs | Reliability bounds | p95 ratio bounds | Verdict | Reasons |');
    lines.push('| --- | --- | --- | --- | --- | --- | --- |');
    for (const stratum of paired.strata) {
      lines.push(`| ${stratum.stratum} | ${stratum.role} | ${stratum.valid_pairs}/${stratum.planned_pairs} | ${cell(stratum.reliability.lower_bound)} – ${cell(stratum.reliability.upper_bound)} | ${cell(stratum.latency.p95_ratio.lower_bound)} – ${cell(stratum.latency.p95_ratio.upper_bound)} | ${stratum.verdict} | ${stratum.reasons.join(', ')} |`);
    }
  }
  const controls = analysis.negative_controls;
  lines.push('', '### Negative controls (safety assertions, not latency successes)', '');
  lines.push(`${controls.observed}/${controls.expected} preregistered trials recorded; ${controls.missing} missing, ${controls.duplicates} duplicate, ${controls.unexpected} unexpected, ${controls.failed} unsafe.`, '');
  for (const control of controls.results) {
    lines.push(`- ${control.browser} ${control.variant} ${control.control} #${control.trial}: expected ${control.expected}, observed ${control.observed} — ${control.safety}`);
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
    : /** @type {ReturnType<typeof analyzePaired>} */ (analysis).verdict !== 'invalid';
  if (!analysis.negative_controls.all_safe || !complete) process.exit(1);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}

import {
  RESUME_DEADLINE_MS,
  RESUME_MAX_EPOCHS,
  RESUME_RETENTION_MS,
  type ResumeEpoch,
  type ResumeLifecycle,
  type ResumeOutcome,
  type ResumePath,
  type ResumePhase,
  type ResumeSample,
} from './resume-metrics';

/**
 * Redacted aggregates of the local resume ring, for the Settings view and the
 * explicit user export. The output holds counts, path classes and rounded
 * durations only: no timestamps, identifiers, hostnames, addresses or errors.
 */
export const RESUME_SUMMARY_SCHEMA = 'herdr-resume-summary/1';
/** Quantiles below these sample counts are reported as insufficient. */
export const RESUME_MIN_MEDIAN_SAMPLES = 5;
export const RESUME_MIN_P95_SAMPLES = 20;

export type QuantileValue = number | 'insufficient' | 'beyond-deadline';

export interface ResumeDistribution {
  n: number;
  p50: QuantileValue;
  p95: QuantileValue;
}

export interface ResumeGroupSummary {
  path: ResumePath;
  lifecycle: ResumeLifecycle;
  samples: number;
  in_progress: number;
  /** Fresh, deadline, auth-rejected and unlock-cancelled samples. */
  valid_attempts: number;
  fresh_on_time: number;
  non_completions: Partial<Record<ResumeOutcome, number>>;
  /** Hidden or removed before an outcome; not counted as attempts. */
  abandoned: number;
  late_fresh: number;
  /** Every valid attempt; non-completions are censored at the deadline. */
  time_to_fresh_ms: ResumeDistribution;
  /** Supplementary only: completed samples, biased toward easy cases. */
  successful_only_ms: ResumeDistribution;
  /** Fresh render measured from device unlock (or the wake when unlocked). */
  transport_eligible_ms: ResumeDistribution;
  retries: { attempts: number; path_attempts: number; superseded: number; timeouts: number; failures: number };
  phase_p50_ms: Partial<Record<ResumePhase, number>>;
  unavailable: string[];
  not_applicable: string[];
  direct: { attempted: number; promoted: number; failures: number; promoted_p50_ms: QuantileValue };
}

export interface ResumeSummary {
  schema: typeof RESUME_SUMMARY_SCHEMA;
  scope: string;
  deadline_ms: number;
  retention: { max_epochs: number; max_age_hours: number };
  epochs: number;
  samples: number;
  triggers: Record<string, number>;
  coalesced_signals: number;
  network_hints: Record<string, number>;
  hidden_duration: Record<string, number>;
  unlock: { requested: number; unlocked: number; cancelled: number; unlock_ms: ResumeDistribution };
  groups: ResumeGroupSummary[];
}

const WSS_HANDSHAKE: ResumePhase[] = ['dial', 'open'];
const GATEWAY_PHASES: ResumePhase[] = ['gateway-hello', 'gateway-proof', 'gateway-ready'];
const E2EE_PHASES: ResumePhase[] = ['e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated'];
const PHASE_ORDER: ResumePhase[] = [
  'probe', 'probe-answer', ...WSS_HANDSHAKE, ...GATEWAY_PHASES, ...E2EE_PHASES,
  'first-frame', 'inventory', 'rendered',
];
const VALID: Partial<Record<ResumeOutcome, true>> = {
  fresh: true,
  deadline: true,
  'auth-rejected': true,
  'unlock-cancelled': true,
};

/**
 * Nearest-rank quantile over all valid attempts, with non-completions placed
 * beyond the deadline. A quantile that lands among them is not identifiable.
 */
export function censoredQuantile(values: number[], q: number, minimum: number): QuantileValue {
  if (values.length < minimum || values.length === 0) return 'insufficient';
  const sorted = [...values].sort((left, right) => left - right);
  // The epsilon keeps binary rounding of q * n from skipping a rank.
  const value = sorted[Math.max(0, Math.ceil(q * sorted.length - 1e-9) - 1)];
  return Number.isFinite(value) && value < RESUME_DEADLINE_MS ? Math.round(value) : 'beyond-deadline';
}

function distribution(values: number[]): ResumeDistribution {
  return {
    n: values.length,
    p50: censoredQuantile(values, 0.5, RESUME_MIN_MEDIAN_SAMPLES),
    p95: censoredQuantile(values, 0.95, RESUME_MIN_P95_SAMPLES),
  };
}

function median(values: number[]): number | undefined {
  if (!values.length) return undefined;
  const sorted = [...values].sort((left, right) => left - right);
  return Math.round(sorted[Math.ceil(sorted.length / 2) - 1]);
}

function hiddenBucket(epoch: ResumeEpoch): string {
  if (epoch.hiddenMs === null) return 'unavailable';
  if (epoch.hiddenMs < 30_000) return 'under_30s';
  if (epoch.hiddenMs < 5 * 60_000) return 'under_5m';
  if (epoch.hiddenMs < 60 * 60_000) return 'under_1h';
  return 'over_1h';
}

function increment(record: Record<string, number>, key: string, by = 1): void {
  record[key] = (record[key] || 0) + by;
}

/** Phases a path class cannot observe, or that do not apply to it. */
export function phaseAvailability(path: ResumePath, lifecycle: ResumeLifecycle, dialed: boolean): {
  unavailable: string[];
  not_applicable: string[];
} {
  // Browsers expose no DNS/TCP/TLS split inside a WebSocket dial, and nothing
  // before the first script runs, so these are never measured or guessed.
  const unavailable = ['os-wake-to-js', 'dns', 'tcp', 'tls'];
  const notApplicable: string[] = [];
  if (!path.startsWith('gateway/')) notApplicable.push(...GATEWAY_PHASES);
  if (!dialed) notApplicable.push(...WSS_HANDSHAKE, ...GATEWAY_PHASES, ...E2EE_PHASES);
  if (lifecycle === 'cold-launch' || lifecycle === 'discarded') notApplicable.push('probe', 'probe-answer');
  return { unavailable, not_applicable: [...new Set(notApplicable)] };
}

export function summarizeResume(epochs: ResumeEpoch[]): ResumeSummary {
  const groups = new Map<string, { path: ResumePath; lifecycle: ResumeLifecycle; samples: Array<[ResumeEpoch, ResumeSample]> }>();
  const triggers: Record<string, number> = {};
  const hints: Record<string, number> = {};
  const hidden: Record<string, number> = {};
  const unlockDurations: number[] = [];
  let coalesced = 0;
  let samples = 0;
  let requested = 0;
  let unlocked = 0;
  let cancelled = 0;
  for (const epoch of epochs) {
    increment(triggers, epoch.trigger);
    increment(hidden, hiddenBucket(epoch));
    coalesced += epoch.coalesced;
    for (const hint of ['online', 'offline', 'change']) {
      if (epoch.signals[hint] !== undefined) increment(hints, hint);
    }
    const unlock = epoch.unlock;
    if (unlock?.requestedAt !== undefined) requested += 1;
    if (unlock?.unlockedAt !== undefined) {
      unlocked += 1;
      unlockDurations.push(unlock.unlockedAt - (unlock.requestedAt ?? 0));
    } else if (unlock?.failedAt !== undefined) cancelled += 1;
    for (const sample of epoch.samples) {
      samples += 1;
      const key = `${sample.path}|${sample.lifecycle}`;
      const group = groups.get(key) ?? { path: sample.path, lifecycle: sample.lifecycle, samples: [] };
      group.samples.push([epoch, sample]);
      groups.set(key, group);
    }
  }
  return {
    schema: RESUME_SUMMARY_SCHEMA,
    scope: 'Local in-memory resume timings from this device. Aggregates and sample counts only; nothing identifies a computer, network or person.',
    deadline_ms: RESUME_DEADLINE_MS,
    retention: { max_epochs: RESUME_MAX_EPOCHS, max_age_hours: RESUME_RETENTION_MS / 3_600_000 },
    epochs: epochs.length,
    samples,
    triggers,
    coalesced_signals: coalesced,
    network_hints: hints,
    hidden_duration: hidden,
    unlock: { requested, unlocked, cancelled, unlock_ms: distribution(unlockDurations) },
    groups: [...groups.values()]
      .sort((left, right) => left.path.localeCompare(right.path) || left.lifecycle.localeCompare(right.lifecycle))
      .map(({ path, lifecycle, samples: members }) => summarizeGroup(path, lifecycle, members)),
  };
}

function summarizeGroup(
  path: ResumePath,
  lifecycle: ResumeLifecycle,
  members: Array<[ResumeEpoch, ResumeSample]>,
): ResumeGroupSummary {
  const nonCompletions: Partial<Record<ResumeOutcome, number>> = {};
  const allAttempts: number[] = [];
  const successful: number[] = [];
  const eligible: number[] = [];
  const phases = new Map<ResumePhase, number[]>();
  const retries = { attempts: 0, path_attempts: 0, superseded: 0, timeouts: 0, failures: 0 };
  const promoted: number[] = [];
  let inProgress = 0;
  let abandoned = 0;
  let fresh = 0;
  let late = 0;
  let dialed = false;
  let directAttempted = 0;
  let directFailures = 0;
  for (const [epoch, sample] of members) {
    retries.attempts += sample.attempts;
    retries.path_attempts += sample.pathAttempts;
    retries.superseded += sample.superseded;
    retries.timeouts += sample.timeouts;
    retries.failures += sample.failures;
    dialed ||= sample.attempts > 0 || sample.phases.dial !== undefined;
    if (sample.lateFreshAt !== undefined) late += 1;
    if (sample.direct) {
      directAttempted += sample.direct.attempts;
      directFailures += sample.direct.failures;
      if (sample.direct.promotedAt !== undefined) promoted.push(sample.direct.promotedAt);
    }
    if (!sample.outcome) {
      inProgress += 1;
      continue;
    }
    if (!VALID[sample.outcome]) {
      abandoned += 1;
      continue;
    }
    if (sample.outcome === 'fresh' && sample.doneAt !== null) {
      fresh += 1;
      allAttempts.push(sample.doneAt);
      successful.push(sample.doneAt);
      const unlockedAt = epoch.unlock?.unlockedAt ?? 0;
      eligible.push(Math.max(0, sample.doneAt - unlockedAt));
      for (const phase of PHASE_ORDER) {
        const at = sample.phases[phase];
        if (at === undefined) continue;
        const list = phases.get(phase) ?? [];
        list.push(at);
        phases.set(phase, list);
      }
    } else {
      increment(nonCompletions as Record<string, number>, sample.outcome);
      allAttempts.push(Number.POSITIVE_INFINITY);
      eligible.push(Number.POSITIVE_INFINITY);
    }
  }
  const phaseMedians: Partial<Record<ResumePhase, number>> = {};
  for (const phase of PHASE_ORDER) {
    const value = median(phases.get(phase) ?? []);
    if (value !== undefined) phaseMedians[phase] = value;
  }
  return {
    path,
    lifecycle,
    samples: members.length,
    in_progress: inProgress,
    valid_attempts: allAttempts.length,
    fresh_on_time: fresh,
    non_completions: nonCompletions,
    abandoned,
    late_fresh: late,
    time_to_fresh_ms: distribution(allAttempts),
    successful_only_ms: distribution(successful),
    transport_eligible_ms: distribution(eligible),
    retries,
    phase_p50_ms: phaseMedians,
    ...phaseAvailability(path, lifecycle, dialed),
    direct: {
      attempted: directAttempted,
      promoted: promoted.length,
      failures: directFailures,
      promoted_p50_ms: censoredQuantile(promoted, 0.5, 1),
    },
  };
}

const PATH_LABELS: Record<ResumePath, string> = {
  'wss/ingress-unknown': 'WSS, ingress not reported',
  'wss/cloudflare': 'Cloudflare WSS',
  'wss/tailscale-managed': 'Tailscale WSS (managed)',
  'wss/tailscale-byo': 'Tailscale WSS (operator-owned)',
  'wss/tailscale-cli': 'Tailscale WSS (CLI-backed)',
  'wss/other': 'WSS, other ingress',
  'gateway/relayed': 'Gateway relayed',
  'gateway/direct': 'Gateway direct (WebRTC)',
};

const LIFECYCLE_LABELS: Record<ResumeLifecycle, string> = {
  warm: 'warm',
  reconnect: 'reconnect',
  bfcache: 'back/forward cache',
  discarded: 'discarded page',
  'cold-launch': 'cold launch',
};

export function resumePathLabel(path: ResumePath): string {
  return PATH_LABELS[path];
}

export function resumeLifecycleLabel(lifecycle: ResumeLifecycle): string {
  return LIFECYCLE_LABELS[lifecycle];
}

export function formatQuantile(value: QuantileValue): string {
  if (value === 'insufficient') return 'too few samples';
  if (value === 'beyond-deadline') return 'over 60 s';
  return value < 1_000 ? `${value} ms` : `${(value / 1_000).toFixed(1)} s`;
}

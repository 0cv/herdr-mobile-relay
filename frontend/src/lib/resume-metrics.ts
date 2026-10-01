import { writable } from 'svelte/store';
import type { TransportKind, TransportPhase } from './transports/types';

/**
 * Local, in-memory resume timing. Nothing here is uploaded, persisted or sent
 * to a relay: a bounded ring of wake epochs lives in this page's memory only,
 * holds transport class, phase offsets and outcomes, and never a hostname,
 * URL, address, relay/device/pane identifier, content or raw error.
 *
 * A wake epoch starts at the first observable wake signal (cold start,
 * visible, bfcache pageshow, resume, or a network change while visible) and
 * owns every dial, retry and replacement that follows, so failed attempts stay
 * inside its elapsed time instead of becoming separate samples. Each relay
 * contributes one sample per epoch. A sample succeeds only when authoritative,
 * ready, non-stale inventory requested after the wake arrives on the current
 * connection generation and its frame is rendered before the fixed deadline.
 */

/** Fixed completion deadline per epoch, from its first observable event. */
export const RESUME_DEADLINE_MS = 60_000;
export const RESUME_MAX_EPOCHS = 100;
export const RESUME_RETENTION_MS = 24 * 60 * 60_000;
/** Wake, focus and network signals this close to an epoch start join it. */
export const RESUME_COALESCE_MS = 2_000;
const MAX_SAMPLES = 16;
const FUTURE_SKEW_MS = 5 * 60_000;
export const RESUME_METRICS_OPT_OUT_KEY = 'herdr_resume_metrics';

export type WakeTrigger = 'cold-start' | 'visible' | 'pageshow' | 'resume' | 'network';
export type WakeSignal = 'cold-start' | 'visible' | 'pageshow' | 'resume' | 'focus';
export type NetworkHint = 'online' | 'offline' | 'change';
export type ResumeLifecycle = 'warm' | 'reconnect' | 'bfcache' | 'discarded' | 'cold-launch';
export type ResumePath =
  | 'wss/ingress-unknown'
  | 'wss/cloudflare'
  | 'wss/tailscale-managed'
  | 'wss/tailscale-byo'
  | 'wss/tailscale-cli'
  | 'wss/other'
  | 'gateway/relayed'
  | 'gateway/direct';
export type ResumeOutcome = 'fresh' | 'deadline' | 'auth-rejected' | 'unlock-cancelled' | 'hidden' | 'removed';
export type AttemptEnd = 'superseded' | 'timeout' | 'auth-rejected' | 'failed';
export type ResumePhase = TransportPhase | 'probe' | 'probe-answer' | 'first-frame' | 'inventory' | 'rendered';

/** The direct WebRTC upgrade, kept apart from the path that served inventory. */
export interface DirectTimeline {
  attempts: number;
  failures: number;
  promotedAt?: number;
  /** Milestones of the latest direct attempt, in ms from the epoch start. */
  phases: Partial<Record<TransportPhase, number>>;
}

export interface ResumeSample {
  /** Path class that delivered the fresh inventory (or was in use). */
  path: ResumePath;
  lifecycle: ResumeLifecycle;
  /** Connection attempts the store started inside this epoch. */
  attempts: number;
  /** Raw path dials inside those attempts, including gateway fallbacks. */
  pathAttempts: number;
  superseded: number;
  timeouts: number;
  failures: number;
  /** Milestones of the attempt (or probe) that served the sample. */
  phases: Partial<Record<ResumePhase, number>>;
  outcome: ResumeOutcome | null;
  /** Milliseconds from the epoch start to the rendered fresh frame. */
  doneAt: number | null;
  /** A fresh snapshot that arrived only after the deadline. */
  lateFreshAt?: number;
  direct: DirectTimeline | null;
}

export interface ResumeEpoch {
  trigger: WakeTrigger;
  /** Set when the whole epoch shares one category; otherwise per sample. */
  lifecycle: ResumeLifecycle | null;
  /** Monotonic start, used only for in-page durations. */
  startedAt: number;
  /** Wall-clock start, used only for retention. */
  wallStartedAt: number;
  /** Wall-clock hidden duration; null when it cannot be observed. */
  hiddenMs: number | null;
  /** Cold start only: navigation start to app start. OS wake-to-JS is unobservable. */
  navigationToAppMs: number | null;
  /** navigator.onLine at the wake, a hint only. */
  onLine: boolean | null;
  /** First offset of each wake or network signal folded into this epoch. */
  signals: Record<string, number>;
  coalesced: number;
  unlock: { requestedAt?: number; unlockedAt?: number; failedAt?: number } | null;
  closed: boolean;
  samples: ResumeSample[];
}

export interface ResumeClock {
  /** Monotonic milliseconds since navigation start. */
  now(): number;
  /** Wall-clock milliseconds. */
  wall(): number;
  /** Runs the callback in the next animation frame. */
  frame(callback: () => void): void;
  onLine(): boolean | null;
  discarded(): boolean;
}

interface Track {
  generation: number;
  hybrid: boolean;
  ended: boolean;
  timedOut: boolean;
  authenticated: boolean;
  ingress: string;
  path: TransportKind | '';
  epoch: ResumeEpoch | null;
  sample: ResumeSample | null;
  carried: boolean;
}

const INGRESS_PATHS = new Map<string, ResumePath>([
  ['cloudflare', 'wss/cloudflare'],
  ['tailscale-managed', 'wss/tailscale-managed'],
  ['tailscale-byo', 'wss/tailscale-byo'],
  ['tailscale-cli', 'wss/tailscale-cli'],
]);

function browserClock(): ResumeClock {
  return {
    now: () => performance.now(),
    wall: () => Date.now(),
    frame: (callback) => {
      if (typeof requestAnimationFrame === 'function') requestAnimationFrame(() => callback());
      else setTimeout(callback, 0);
    },
    onLine: () => (typeof navigator.onLine === 'boolean' ? navigator.onLine : null),
    discarded: () => (document as Document & { wasDiscarded?: boolean }).wasDiscarded === true,
  };
}

function storedEnabled(): boolean {
  try {
    return localStorage.getItem(RESUME_METRICS_OPT_OUT_KEY) !== 'off';
  } catch {
    return true;
  }
}

function keepProbe(phases: ResumeSample['phases']): ResumeSample['phases'] {
  const kept: ResumeSample['phases'] = {};
  if (phases.probe !== undefined) kept.probe = phases.probe;
  if (phases['probe-answer'] !== undefined) kept['probe-answer'] = phases['probe-answer'];
  return kept;
}

export class ResumeMetrics {
  /** Bumped whenever recorded data changes, so Settings can re-read it. */
  readonly revision = writable(0);
  enabled: boolean;
  private readonly clock: ResumeClock;
  private epochs: ResumeEpoch[] = [];
  private tracks = new Map<string, Track>();
  private hiddenAt: number | null = null;
  private coldPending = true;
  private revisionValue = 0;

  constructor(clock: ResumeClock = browserClock(), enabled = storedEnabled()) {
    this.clock = clock;
    this.enabled = enabled;
  }

  /** A wake signal opens an epoch unless one is already collecting. */
  wake(signal: WakeSignal): void {
    if (!this.enabled) return;
    const epoch = this.active();
    if (epoch) this.signal(epoch, signal);
    // Focus fires on every window switch; it only ever joins a wake.
    else if (signal !== 'focus') this.open(signal === 'cold-start' ? 'visible' : signal, signal);
  }

  /**
   * Network events are hints: they never gate a dial. One arriving during a
   * wake is folded into it; a real change while the app is in use starts its
   * own epoch so the recovery it causes is measured rather than suppressed.
   */
  network(hint: NetworkHint): void {
    if (!this.enabled) return;
    const epoch = this.active();
    if (epoch) this.signal(epoch, hint);
    else if (hint !== 'offline') this.open('network', hint);
  }

  /** The page went hidden, was frozen, or is being put in the bfcache. */
  hidden(): void {
    if (!this.enabled) return;
    this.hiddenAt ??= this.clock.wall();
    const epoch = this.last();
    if (epoch && !epoch.closed) this.close(epoch, 'hidden');
  }

  unlock(event: 'request' | 'success' | 'failure'): void {
    if (!this.enabled) return;
    const epoch = this.current();
    if (!epoch) return;
    const at = this.at(epoch);
    const unlock = epoch.unlock ??= {};
    if (event === 'request') unlock.requestedAt ??= at;
    else if (event === 'success') unlock.unlockedAt ??= at;
    else unlock.failedAt = at;
  }

  /**
   * A new connection attempt for a relay. The returned generation scopes every
   * later callback, so one from an abandoned dial is never counted.
   */
  attempt(relayId: string, hybrid: boolean): number {
    const track = this.track(relayId);
    const epoch = this.enabled ? this.current() : null;
    const sample = epoch ? this.sample(track) : null;
    if (epoch && sample && !sample.outcome) {
      if (!track.ended && track.generation > 0) sample.superseded += 1;
      sample.attempts += 1;
      sample.lifecycle = epoch.lifecycle ?? 'reconnect';
      sample.phases = keepProbe(sample.phases);
      sample.path = hybrid ? 'gateway/relayed' : 'wss/ingress-unknown';
    }
    track.generation += 1;
    track.hybrid = hybrid;
    track.ended = false;
    track.timedOut = false;
    track.authenticated = false;
    track.ingress = '';
    track.path = '';
    track.carried = false;
    return track.generation;
  }

  phase(relayId: string, generation: number, phase: TransportPhase, path: TransportKind): void {
    const track = this.tracked(relayId, generation);
    if (!track) return;
    if (path === 'webrtc') {
      const sample = this.existing(track);
      const epoch = this.last();
      if (!sample || !epoch) return;
      const at = this.at(epoch);
      const direct = sample.direct ??= { attempts: 0, failures: 0, phases: {} };
      if (phase === 'dial') {
        direct.attempts += 1;
        direct.phases = {};
      }
      if (phase === 'failed') direct.failures += 1;
      else if (phase === 'promoted') direct.promotedAt ??= at;
      else direct.phases[phase] ??= at;
      return;
    }
    if (phase === 'dial') track.timedOut = false;
    if (phase === 'timeout') track.timedOut = true;
    const sample = this.sample(track);
    const epoch = this.last();
    if (!sample || !epoch || sample.outcome) return;
    if (phase === 'dial') {
      sample.pathAttempts += 1;
      sample.phases = keepProbe(sample.phases);
      track.authenticated = false;
    }
    if (phase === 'authenticated') track.authenticated = true;
    if (phase === 'timeout') sample.timeouts += 1;
    else if (phase !== 'failed') sample.phases[phase] ??= this.at(epoch);
  }

  /** The transport reported an authenticated, usable path. */
  connected(relayId: string, generation: number, path: TransportKind): void {
    const track = this.tracked(relayId, generation);
    if (track) track.path = path;
  }

  /**
   * The relay's ingress descriptor from push_config. It labels a WSS path only
   * when it arrived inside an authenticated session; hostnames are never used.
   */
  ingress(relayId: string, generation: number, value: unknown): void {
    const track = this.tracked(relayId, generation);
    if (!track || !track.authenticated) return;
    track.ingress = typeof value === 'string' && value.length <= 32 ? value : '';
  }

  /** A post-wake refresh request on an existing connection. */
  probe(relayId: string, generation: number): void {
    const track = this.tracked(relayId, generation);
    const sample = track && this.sample(track);
    const epoch = this.last();
    if (sample && epoch && !sample.outcome) sample.phases.probe ??= this.at(epoch);
  }

  /** Any authenticated application message on the current connection. */
  frame(relayId: string, generation: number): void {
    const track = this.tracked(relayId, generation);
    const sample = track && this.existing(track);
    const epoch = this.last();
    if (!track || !sample || !epoch || sample.outcome) return;
    if (sample.attempts > 0 || track.carried) sample.phases['first-frame'] ??= this.at(epoch);
    else if (sample.phases.probe !== undefined) sample.phases['probe-answer'] ??= this.at(epoch);
  }

  /**
   * An agents snapshot was published. Only a ready, non-stale snapshot on the
   * current generation, requested after the wake, completes the sample, and
   * only once its frame renders.
   */
  inventory(relayId: string, generation: number, fresh: boolean): void {
    const track = this.tracked(relayId, generation);
    if (!track || !fresh) return;
    this.current();
    const epoch = this.last();
    const sample = epoch && track.epoch === epoch ? track.sample : null;
    if (!epoch || !sample) return;
    if (sample.outcome) {
      if (sample.outcome === 'deadline' && sample.lateFreshAt === undefined) sample.lateFreshAt = this.at(epoch);
      return;
    }
    if (sample.phases.inventory !== undefined) return;
    if (!sample.attempts && !track.carried && sample.phases.probe === undefined) return;
    sample.phases.inventory = this.at(epoch);
    sample.path = this.label(track);
    this.clock.frame(() => {
      if (track.generation !== generation || track.sample !== sample) return;
      const at = this.at(epoch);
      if (sample.outcome) {
        if (sample.outcome === 'deadline' && sample.lateFreshAt === undefined) sample.lateFreshAt = at;
        return;
      }
      sample.phases.rendered = at;
      if (at < RESUME_DEADLINE_MS) this.finish(epoch, sample, 'fresh', at);
      else {
        sample.lateFreshAt = at;
        this.finish(epoch, sample, 'deadline', null);
      }
    });
  }

  end(relayId: string, generation: number, why: AttemptEnd): void {
    const track = this.tracked(relayId, generation);
    if (!track || track.ended) return;
    track.ended = true;
    const sample = this.existing(track);
    const epoch = this.last();
    if (!sample || !epoch || sample.outcome) return;
    if (why === 'auth-rejected') this.finish(epoch, sample, 'auth-rejected', null);
    else if (why === 'timeout') sample.timeouts += 1;
    // A handshake timeout was already counted when its timer fired.
    else if (why === 'failed') sample.failures += track.timedOut ? 0 : 1;
    else sample.superseded += 1;
  }

  removed(relayId: string): void {
    const track = this.tracks.get(relayId);
    const sample = track && this.existing(track);
    const epoch = this.last();
    this.tracks.delete(relayId);
    if (sample && epoch && !sample.outcome) this.finish(epoch, sample, 'removed', null);
  }

  /** Copies of the retained epochs, oldest first. */
  snapshot(): ResumeEpoch[] {
    this.current();
    this.prune();
    return JSON.parse(JSON.stringify(this.epochs)) as ResumeEpoch[];
  }

  /** Forgets every measurement and every per-relay tracking entry. */
  clear(): void {
    this.epochs = [];
    this.tracks.clear();
    this.hiddenAt = null;
    this.bump();
  }

  setEnabled(enabled: boolean): void {
    try {
      if (enabled) localStorage.removeItem(RESUME_METRICS_OPT_OUT_KEY);
      else localStorage.setItem(RESUME_METRICS_OPT_OUT_KEY, 'off');
    } catch {
      // The choice still applies to this page when storage is unavailable.
    }
    this.enabled = enabled;
    this.clear();
  }

  private tracked(relayId: string, generation: number): Track | null {
    const track = this.tracks.get(relayId);
    return this.enabled && generation > 0 && track && track.generation === generation ? track : null;
  }

  private track(relayId: string): Track {
    let track = this.tracks.get(relayId);
    if (!track) {
      track = {
        generation: 0,
        hybrid: false,
        ended: true,
        timedOut: false,
        authenticated: false,
        ingress: '',
        path: '',
        epoch: null,
        sample: null,
        carried: false,
      };
      this.tracks.set(relayId, track);
    }
    return track;
  }

  private last(): ResumeEpoch | undefined {
    return this.epochs[this.epochs.length - 1];
  }

  /** The epoch still inside its deadline; an expired one is closed on the way. */
  private current(): ResumeEpoch | null {
    const epoch = this.last();
    if (!epoch || epoch.closed) return null;
    if (this.clock.now() - epoch.startedAt < RESUME_DEADLINE_MS) return epoch;
    this.close(epoch, 'deadline');
    return null;
  }

  /** The epoch still collecting, so new signals coalesce into it. */
  private active(): ResumeEpoch | null {
    const epoch = this.current();
    if (!epoch) return null;
    const locked = epoch.unlock?.requestedAt !== undefined
      && epoch.unlock.unlockedAt === undefined
      && epoch.unlock.failedAt === undefined;
    return this.clock.now() - epoch.startedAt < RESUME_COALESCE_MS
      || locked
      || epoch.samples.some((sample) => !sample.outcome)
      ? epoch
      : null;
  }

  private open(trigger: WakeTrigger, first: string): void {
    const previous = this.last();
    if (previous && !previous.closed) this.close(previous, 'deadline');
    const now = this.clock.now();
    const wall = this.clock.wall();
    const cold = this.coldPending;
    this.coldPending = false;
    this.epochs.push({
      trigger: cold ? 'cold-start' : trigger,
      lifecycle: cold
        ? this.clock.discarded() ? 'discarded' : 'cold-launch'
        : trigger === 'pageshow' ? 'bfcache' : null,
      startedAt: now,
      wallStartedAt: wall,
      hiddenMs: !cold && this.hiddenAt !== null && wall >= this.hiddenAt ? wall - this.hiddenAt : null,
      navigationToAppMs: cold ? Math.max(0, Math.round(now)) : null,
      onLine: this.clock.onLine(),
      signals: { [first]: 0 },
      coalesced: 0,
      unlock: null,
      closed: false,
      samples: [],
    });
    this.hiddenAt = null;
    this.prune();
    this.bump();
  }

  private signal(epoch: ResumeEpoch, name: string): void {
    epoch.signals[name] ??= this.at(epoch);
    epoch.coalesced += 1;
  }

  private close(epoch: ResumeEpoch, reason: 'hidden' | 'deadline'): void {
    epoch.closed = true;
    const cancelled = epoch.unlock?.failedAt !== undefined && epoch.unlock.unlockedAt === undefined;
    for (const sample of epoch.samples) {
      if (sample.outcome) continue;
      sample.outcome = reason === 'hidden' ? 'hidden' : cancelled ? 'unlock-cancelled' : 'deadline';
    }
    this.bump();
  }

  private finish(epoch: ResumeEpoch, sample: ResumeSample, outcome: ResumeOutcome, at: number | null): void {
    sample.outcome = outcome;
    sample.doneAt = at;
    if (epoch.lifecycle) sample.lifecycle = epoch.lifecycle;
    this.bump();
  }

  private sample(track: Track): ResumeSample | null {
    const epoch = this.current();
    if (!epoch) return null;
    if (track.epoch === epoch) return track.sample;
    if (epoch.samples.length >= MAX_SAMPLES) return null;
    // A dial that began before the wake and is still running is carried into
    // this epoch: its pre-wake milestones are simply not part of the sample.
    const carried = !track.ended && !track.path && track.generation > 0;
    const sample: ResumeSample = {
      path: this.label(track),
      lifecycle: epoch.lifecycle ?? (carried ? 'reconnect' : 'warm'),
      attempts: 0,
      pathAttempts: 0,
      superseded: 0,
      timeouts: 0,
      failures: 0,
      phases: {},
      outcome: null,
      doneAt: null,
      direct: null,
    };
    track.epoch = epoch;
    track.sample = sample;
    track.carried = carried;
    epoch.samples.push(sample);
    return sample;
  }

  private existing(track: Track): ResumeSample | null {
    const epoch = this.current();
    return epoch && track.epoch === epoch ? track.sample : null;
  }

  private label(track: Track): ResumePath {
    if (track.path === 'webrtc') return 'gateway/direct';
    if (track.path === 'gateway' || (!track.path && track.hybrid)) return 'gateway/relayed';
    if (!track.path || !track.authenticated) return 'wss/ingress-unknown';
    return INGRESS_PATHS.get(track.ingress) ?? (track.ingress ? 'wss/other' : 'wss/ingress-unknown');
  }

  private at(epoch: ResumeEpoch): number {
    return Math.round(this.clock.now() - epoch.startedAt);
  }

  private prune(): void {
    const now = this.clock.now();
    const wall = this.clock.wall();
    this.epochs = this.epochs.filter((epoch) => now - epoch.startedAt <= RESUME_RETENTION_MS
      && wall - epoch.wallStartedAt <= RESUME_RETENTION_MS
      && epoch.wallStartedAt <= wall + FUTURE_SKEW_MS).slice(-RESUME_MAX_EPOCHS);
  }

  private bump(): void {
    this.revisionValue += 1;
    try {
      this.revision.set(this.revisionValue);
    } catch {
      // A failing Settings subscriber must never reach the reconnect paths.
    }
  }
}

export const resumeMetrics = new ResumeMetrics();

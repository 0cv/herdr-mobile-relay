import { writable } from 'svelte/store';
import type { TransportKind, TransportPhase } from './transports/types';

/**
 * Local, in-memory resume timing. Nothing here is uploaded, persisted or sent
 * to a relay: a bounded ring of wake epochs lives in this page's memory only,
 * holds transport class, phase offsets and outcomes, and never a hostname,
 * URL, address, relay/device/pane identifier, content or raw error.
 *
 * A wake epoch starts at the first observable wake signal (cold start,
 * visible, bfcache pageshow, resume, or a network change while visible). Every
 * relay eligible to connect at that moment is enrolled with one sample, and
 * the epoch owns every dial, retry and replacement that follows, so failed
 * attempts stay inside its elapsed time instead of becoming separate samples.
 * A sample succeeds only when authoritative, ready, non-stale inventory
 * requested after the wake arrives on the current connection generation and
 * path, is still the current authoritative snapshot on that path when its
 * frame paints, and that frame shows a visible inventory view while the app
 * is unlocked, before the fixed deadline. A wake that never gets that far
 * ends as a censored non-completion.
 */

/** Fixed completion deadline per epoch, from its first observable event. */
export const RESUME_DEADLINE_MS = 60_000;
export const RESUME_MAX_EPOCHS = 100;
export const RESUME_RETENTION_MS = 24 * 60 * 60_000;
/** Lifecycle signals this close to an epoch start join it. */
export const RESUME_COALESCE_MS = 2_000;
export const RESUME_MAX_SAMPLES = 16;
/** Path-attempt records kept per sample; counters keep counting past it. */
export const RESUME_MAX_PATH_RECORDS = 8;
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
export type PathEnd = 'failed' | 'timeout' | 'superseded' | 'auth-rejected';
export type ResumePhase = TransportPhase | 'probe' | 'probe-answer' | 'first-frame' | 'inventory' | 'rendered';

/** The direct WebRTC upgrade, kept apart from the path that served inventory. */
export interface DirectTimeline {
  attempts: number;
  failures: number;
  promotedAt?: number;
  /** Milestones of the latest direct attempt, in ms from the epoch start. */
  phases: Partial<Record<TransportPhase, number>>;
}

/** One raw relay-URL or gateway dial inside a sample, and how it ended. */
export interface PathAttemptRecord {
  path: 'websocket' | 'gateway';
  dialAt: number;
  /** Last observable milestone this dial reached. */
  reached: TransportPhase;
  /** When the dial became the usable path, if it did. */
  servedAt?: number;
  endedAt?: number;
  end?: PathEnd;
}

export interface ResumeSample {
  /** Path class that delivered the fresh inventory (or was in use). */
  path: ResumePath;
  lifecycle: ResumeLifecycle;
  /** Connection attempts the store started inside this epoch. */
  attempts: number;
  /** Raw path dials inside those attempts, including gateway fallbacks. */
  pathAttempts: number;
  /** Path dials that ended (failed or timed out) before becoming usable. */
  pathFailures: number;
  superseded: number;
  timeouts: number;
  failures: number;
  /** The first RESUME_MAX_PATH_RECORDS path dials of this sample. */
  paths: PathAttemptRecord[];
  /** Milestones of the attempt (or probe) that served the sample. */
  phases: Partial<Record<ResumePhase, number>>;
  outcome: ResumeOutcome | null;
  /** Milliseconds from the epoch start to the visible fresh frame. */
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
  closedBy: 'hidden' | 'deadline' | 'superseded' | null;
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

/** A relay eligible to connect when a wake begins. */
export interface ResumeParticipant {
  id: string;
  hybrid: boolean;
}

/**
 * One fresh snapshot waiting for the frame that shows it. Each snapshot is
 * its own object: a frame (or a retry after unlocking or a view appearing)
 * acts only if its object is still the track's pending snapshot, so an
 * obsolete callback can never complete, alter or erase a newer one. This is
 * internal bookkeeping and never part of a sample or the export.
 */
interface PendingSnapshot {
  relayId: string;
  epoch: ResumeEpoch;
  sample: ResumeSample;
  generation: number;
  validity: number;
  path: TransportKind | '';
  /** Its frame ran with nothing visible showing it, or behind the lock. */
  waiting: boolean;
}

/**
 * Live connection identity for one relay. It is not a measurement: it lets a
 * later sample attribute callbacks to the right connection after the ring is
 * cleared or measurement is switched back on. The measurement references
 * (epoch, sample, record, pending) are dropped whenever their epoch leaves
 * the ring.
 */
interface Track {
  generation: number;
  hybrid: boolean;
  ended: boolean;
  /** Connected on an authenticated path and not since connecting or closed. */
  live: boolean;
  timedOut: boolean;
  authenticated: boolean;
  ingress: string;
  path: TransportKind | '';
  /**
   * Changes whenever a pending fresh snapshot stops being valid: the path
   * that delivered it changed (a dial, a direct promotion or fallback, a
   * reconnect) or the relay reported its inventory not ready or stale.
   */
  validity: number;
  carried: boolean;
  epoch: ResumeEpoch | null;
  sample: ResumeSample | null;
  record: PathAttemptRecord | null;
  /** The current fresh snapshot not yet seen, if any. */
  pending: PendingSnapshot | null;
}

/**
 * Whether an inventory view's root element is laid out and shown on a visible
 * page. Inventory views register it with `presentInventory`, and it is
 * evaluated at the frame that would complete a sample.
 */
export function shownOnScreen(element?: Element | null): boolean {
  if (!element?.isConnected || document.visibilityState === 'hidden') return false;
  const { width, height } = element.getBoundingClientRect();
  return width > 0 && height > 0 && getComputedStyle(element).visibility === 'visible';
}

/** Cached summary rows never carry this live-only presentation selector. */
export function shownRelayRows(root: Element | null | undefined, relayId: string): boolean {
  if (!shownOnScreen(root)) return false;
  return [...root!.querySelectorAll('[data-live-relay]')]
    .some((row) => row.getAttribute('data-live-relay') === relayId && shownOnScreen(row));
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
  private participants: (() => ResumeParticipant[]) | null = null;
  private hiddenAt: number | null = null;
  private coldPending = true;
  private locked = false;
  /** Visibility checks of the mounted views that render the agent inventory. */
  private views = new Set<(relayId: string) => boolean>();
  private revisionValue = 0;

  constructor(clock: ResumeClock = browserClock(), enabled = storedEnabled()) {
    this.clock = clock;
    this.enabled = enabled;
  }

  /** Names the relays that take part in a wake; the store provides it. */
  setParticipants(provider: (() => ResumeParticipant[]) | null): void {
    this.participants = provider;
  }

  /**
   * Enrolls every eligible relay in the current epoch, so a wake whose dial
   * never starts (locked, cancelled, offline) still ends with an outcome.
   */
  enroll(): void {
    if (!this.enabled || !this.participants) return;
    const epoch = this.current();
    if (!epoch) return;
    let participants: ResumeParticipant[];
    try {
      participants = this.participants();
    } catch {
      return;
    }
    for (const participant of participants) {
      const track = this.track(participant.id);
      if (track.generation === 0) track.hybrid = participant.hybrid;
      this.sample(track);
    }
  }

  /** A wake signal opens an epoch unless one is already collecting. */
  wake(signal: WakeSignal): void {
    if (!this.enabled) return;
    const epoch = this.collecting('wake');
    if (!epoch) {
      // Focus fires on every window switch; it only ever joins a wake.
      if (signal !== 'focus') this.open(signal === 'cold-start' ? 'visible' : signal, signal);
      return;
    }
    this.signal(epoch, signal);
    // A persisted pageshow can follow the resume or visible event of the same
    // restore: the wake keeps its start and deadline but is a bfcache restore.
    if (signal === 'pageshow' && !epoch.lifecycle) {
      epoch.lifecycle = 'bfcache';
      for (const sample of epoch.samples) sample.lifecycle = 'bfcache';
    }
  }

  /**
   * Network events are hints: they never gate a dial. One arriving while a
   * wake is still collecting is folded into it; one arriving after every
   * sample has finished starts its own epoch, so the recovery it causes is
   * measured rather than swallowed. Offline never opens an epoch.
   */
  network(hint: NetworkHint): void {
    if (!this.enabled) return;
    const epoch = this.collecting('network');
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
    let epoch = this.current();
    // A slow verification still records its unlock delay on the wake whose
    // deadline it exhausted; it cannot change that wake's outcome.
    const last = this.last();
    if (!epoch && event !== 'request' && last?.closedBy === 'deadline'
      && last.unlock?.requestedAt !== undefined && last.unlock.unlockedAt === undefined) {
      epoch = last;
    }
    if (!epoch) return;
    const at = this.at(epoch);
    const unlock = epoch.unlock ??= {};
    if (event === 'request') unlock.requestedAt ??= at;
    else if (event === 'success') unlock.unlockedAt ??= at;
    else unlock.failedAt = at;
  }

  /**
   * Mirrors the device lock. Inventory painted behind the lock is not yet
   * visible, so its sample completes at the first frame after unlocking.
   */
  setLocked(locked: boolean): void {
    if (this.locked === locked) return;
    this.locked = locked;
    if (!locked) this.release();
  }

  /**
   * Registers a mounted view that renders the agent inventory (the agent
   * list, or the rail beside a terminal) with a check of whether it is shown.
   * Fresh inventory completes a sample only in a frame in which a registered
   * view is visible; one published while, say, Settings is open or a phone
   * terminal hides the rail completes at the first frame after a visible view
   * is registered. Returns the unregister function.
   */
  presentInventory(visible: (relayId: string) => boolean = () => true): () => void {
    const view = (relayId: string) => visible(relayId);
    this.views.add(view);
    this.release();
    return () => {
      this.views.delete(view);
    };
  }

  /** Retries, once each, the exact snapshots that were waiting to be seen. */
  private release(): void {
    for (const track of this.tracks.values()) {
      const pending = track.pending;
      if (!pending?.waiting) continue;
      pending.waiting = false;
      this.clock.frame(() => this.settle(track, pending));
    }
  }

  private visibleView(relayId: string): boolean {
    for (const view of this.views) {
      try {
        if (view(relayId)) return true;
      } catch {
        // A view whose check fails is not shown.
      }
    }
    return false;
  }

  /**
   * Withdraws the pending snapshot. Its inventory and rendered offsets go
   * with it at once unless the sample already finished; nothing else of the
   * sample is touched.
   */
  private withdrawPending(track: Track): void {
    const pending = track.pending;
    if (!pending) return;
    track.pending = null;
    if (pending.sample.outcome) return;
    delete pending.sample.phases.inventory;
    delete pending.sample.phases.rendered;
  }

  /** Whatever was pending can no longer complete: a new path, generation or readiness. */
  private invalidate(track: Track): void {
    track.validity += 1;
    this.withdrawPending(track);
  }

  /**
   * A new connection attempt for a relay. The returned generation scopes every
   * later callback, so one from an abandoned dial is never counted.
   */
  attempt(relayId: string, hybrid: boolean): number {
    const track = this.track(relayId);
    this.invalidate(track);
    const epoch = this.enabled ? this.current() : null;
    const sample = epoch ? this.sample(track) : null;
    if (epoch && sample && !sample.outcome) {
      if (!track.ended && track.generation > 0) {
        sample.superseded += 1;
        this.endRecord(track, sample, 'superseded', this.at(epoch));
      }
      sample.attempts += 1;
      sample.lifecycle = epoch.lifecycle ?? 'reconnect';
      sample.phases = keepProbe(sample.phases);
      sample.path = hybrid ? 'gateway/relayed' : 'wss/ingress-unknown';
    }
    track.generation += 1;
    track.hybrid = hybrid;
    track.ended = false;
    track.live = false;
    track.timedOut = false;
    track.authenticated = false;
    track.ingress = '';
    track.path = '';
    track.carried = false;
    track.record = null;
    return track.generation;
  }

  phase(relayId: string, generation: number, phase: TransportPhase, path: TransportKind): void {
    const track = this.tracked(relayId, generation);
    if (!track) return;
    if (path === 'webrtc') {
      if (!this.enabled) return;
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
    // Connection identity is tracked even while measurement is off.
    if (phase === 'dial') {
      track.timedOut = false;
      track.authenticated = false;
      this.invalidate(track);
    }
    if (phase === 'timeout') track.timedOut = true;
    if (phase === 'authenticated') track.authenticated = true;
    if (!this.enabled) return;
    const sample = this.sample(track);
    const epoch = this.last();
    if (!sample || !epoch || sample.outcome) return;
    const at = this.at(epoch);
    if (phase === 'dial') {
      this.endRecord(track, sample, 'superseded', at);
      sample.pathAttempts += 1;
      sample.phases = keepProbe(sample.phases);
      const record: PathAttemptRecord = { path: path === 'gateway' ? 'gateway' : 'websocket', dialAt: at, reached: 'dial' };
      if (sample.paths.length < RESUME_MAX_PATH_RECORDS) sample.paths.push(record);
      track.record = record;
      sample.phases.dial = at;
      return;
    }
    if (phase === 'timeout') {
      sample.timeouts += 1;
      this.endRecord(track, sample, 'timeout', at);
      return;
    }
    if (phase === 'failed') {
      this.endRecord(track, sample, 'failed', at);
      return;
    }
    sample.phases[phase] ??= at;
    if (track.record && !track.record.end) track.record.reached = phase;
  }

  /** The transport reported an authenticated, usable path. */
  connected(relayId: string, generation: number, path: TransportKind): void {
    const track = this.tracked(relayId, generation);
    if (!track) return;
    // A direct promotion or a fallback changes the path frames arrive on: a
    // snapshot from the previous path no longer completes the sample.
    if (track.path !== path) this.invalidate(track);
    track.path = path;
    track.live = true;
    if (!this.enabled || path === 'webrtc' || !track.record || track.record.end) return;
    const sample = this.existing(track);
    const epoch = this.last();
    if (sample && epoch && !sample.outcome) track.record.servedAt ??= this.at(epoch);
  }

  /** The transport fell back to connecting: the path that served frames is gone. */
  connecting(relayId: string, generation: number): void {
    const track = this.tracked(relayId, generation);
    if (!track || !track.live) return;
    track.live = false;
    this.invalidate(track);
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
    if (!track || !this.enabled) return;
    const sample = this.sample(track);
    const epoch = this.last();
    if (sample && epoch && !sample.outcome) sample.phases.probe ??= this.at(epoch);
  }

  /** Any authenticated application message on the current connection. */
  frame(relayId: string, generation: number): void {
    const track = this.tracked(relayId, generation);
    if (!track || !this.enabled) return;
    const sample = this.existing(track);
    const epoch = this.last();
    if (!sample || !epoch || sample.outcome) return;
    if (sample.attempts > 0 || track.carried) sample.phases['first-frame'] ??= this.at(epoch);
    else if (sample.phases.probe !== undefined) sample.phases['probe-answer'] ??= this.at(epoch);
  }

  /**
   * Inventory was published: an agents snapshot, or the relay's readiness.
   * Only a ready, non-stale snapshot on the current generation and live
   * path, requested after the wake, completes the sample, and only once its
   * frame shows it in a visible inventory view while the app is unlocked. A
   * newer fresh snapshot replaces one still waiting for its frame, because
   * that frame shows the newer one. A non-authoritative report (not ready,
   * stale) withdraws any pending one.
   */
  inventory(relayId: string, generation: number, fresh: boolean): void {
    const track = this.tracked(relayId, generation);
    if (!track) return;
    if (!fresh) {
      this.invalidate(track);
      return;
    }
    if (!track.live || !this.enabled) return;
    this.current();
    const epoch = this.last();
    const sample = epoch && track.epoch === epoch ? track.sample : null;
    if (!epoch || !sample) return;
    if (sample.outcome) {
      if (sample.outcome === 'deadline' && sample.lateFreshAt === undefined) sample.lateFreshAt = this.at(epoch);
      return;
    }
    if (!sample.attempts && !track.carried && sample.phases.probe === undefined) return;
    this.withdrawPending(track);
    const pending: PendingSnapshot = {
      relayId, epoch, sample, generation, validity: track.validity, path: track.path, waiting: false,
    };
    track.pending = pending;
    sample.phases.inventory = this.at(epoch);
    sample.path = this.label(track);
    this.clock.frame(() => this.settle(track, pending));
  }

  end(relayId: string, generation: number, why: AttemptEnd): void {
    const track = this.tracks.get(relayId);
    if (!track || generation <= 0 || track.generation !== generation || track.ended) return;
    track.ended = true;
    track.live = false;
    this.invalidate(track);
    if (!this.enabled) return;
    const sample = this.existing(track);
    const epoch = this.last();
    if (!sample || !epoch || sample.outcome) return;
    this.endRecord(track, sample, why, this.at(epoch));
    if (why === 'auth-rejected') this.finish(epoch, sample, 'auth-rejected', null);
    else if (why === 'timeout') sample.timeouts += 1;
    // A handshake timeout was already counted when its timer fired.
    else if (why === 'failed') sample.failures += track.timedOut ? 0 : 1;
    else sample.superseded += 1;
  }

  /** The relay was removed from this device. */
  removed(relayId: string): void {
    this.withdraw(relayId);
    this.tracks.delete(relayId);
  }

  /** The relay stopped being able to connect (pairing deferred or required). */
  withdraw(relayId: string): void {
    const track = this.tracks.get(relayId);
    if (!track) return;
    track.ended = true;
    track.live = false;
    this.invalidate(track);
    const sample = this.existing(track);
    const epoch = this.last();
    if (this.enabled && sample && epoch && !sample.outcome) this.finish(epoch, sample, 'removed', null);
  }

  /** Copies of the retained epochs, oldest first. */
  snapshot(): ResumeEpoch[] {
    this.current();
    this.prune();
    return JSON.parse(JSON.stringify(this.epochs)) as ResumeEpoch[];
  }

  /**
   * Forgets every measurement. Live connection identity stays, without any
   * timing, so the next wake on a healthy connection is still sampled.
   */
  clear(): void {
    this.epochs = [];
    this.hiddenAt = null;
    for (const track of this.tracks.values()) this.detach(track);
    this.bump();
  }

  /** App teardown: forgets measurements and connection identity alike. */
  reset(): void {
    this.epochs = [];
    for (const track of this.tracks.values()) this.detach(track);
    this.tracks.clear();
    this.hiddenAt = null;
    this.locked = false;
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

  /**
   * The live connection a callback belongs to. A callback from a replaced
   * generation, or one arriving after its connection ended (a handshake
   * promise that settles after the socket closed), is not part of any sample.
   */
  private tracked(relayId: string, generation: number): Track | null {
    const track = this.tracks.get(relayId);
    return generation > 0 && track && track.generation === generation && !track.ended ? track : null;
  }

  private track(relayId: string): Track {
    let track = this.tracks.get(relayId);
    if (!track) {
      track = {
        generation: 0,
        hybrid: false,
        ended: true,
        live: false,
        timedOut: false,
        authenticated: false,
        ingress: '',
        path: '',
        validity: 0,
        carried: false,
        epoch: null,
        sample: null,
        record: null,
        pending: null,
      };
      this.tracks.set(relayId, track);
    }
    return track;
  }

  private detach(track: Track): void {
    track.epoch = null;
    track.sample = null;
    track.record = null;
    track.pending = null;
    track.carried = false;
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

  /**
   * The epoch a new signal joins: one still waiting for a relay or for device
   * verification. A completed epoch only absorbs lifecycle duplicates within
   * the coalescing window (visible, pageshow and focus fire together), never a
   * network event, which gets its own epoch.
   */
  private collecting(kind: 'wake' | 'network'): ResumeEpoch | null {
    const epoch = this.current();
    if (!epoch) return null;
    const unlockPending = epoch.unlock?.requestedAt !== undefined
      && epoch.unlock.unlockedAt === undefined
      && epoch.unlock.failedAt === undefined;
    if (unlockPending || epoch.samples.some((sample) => !sample.outcome)) return epoch;
    const young = this.clock.now() - epoch.startedAt < RESUME_COALESCE_MS;
    if (young && (kind === 'wake' || epoch.samples.length === 0)) return epoch;
    return null;
  }

  private open(trigger: WakeTrigger, first: string): void {
    const previous = this.last();
    if (previous && !previous.closed) this.close(previous, 'superseded');
    // Snapshots of the replaced wake are detached, leaving its samples as recorded.
    for (const track of this.tracks.values()) track.pending = null;
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
      closedBy: null,
      samples: [],
    });
    this.hiddenAt = null;
    this.prune();
    this.enroll();
    this.bump();
  }

  private signal(epoch: ResumeEpoch, name: string): void {
    epoch.signals[name] ??= this.at(epoch);
    epoch.coalesced += 1;
  }

  private close(epoch: ResumeEpoch, reason: 'hidden' | 'deadline' | 'superseded'): void {
    epoch.closed = true;
    epoch.closedBy = reason;
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

  /**
   * The frame after a fresh snapshot was published, or a retry once a
   * visible view appeared or the app unlocked. It acts only for the track's
   * current pending snapshot: anything that retired its connection, path or
   * readiness already withdrew it, and a newer snapshot replaced it, so an
   * obsolete callback returns without touching anything. A frame with no
   * visible inventory view did not show it, and one behind the device lock
   * was not seen: both leave it waiting for the first frame in which it is.
   */
  private settle(track: Track, pending: PendingSnapshot): void {
    if (track.pending !== pending) return;
    const { epoch, sample } = pending;
    if (track.sample !== sample || track.generation !== pending.generation || track.ended || !track.live
      || track.validity !== pending.validity || track.path !== pending.path
      || (sample.outcome && sample.outcome !== 'deadline')) {
      this.withdrawPending(track);
      return;
    }
    const at = this.at(epoch);
    const visible = this.visibleView(pending.relayId);
    if (visible && !sample.outcome) sample.phases.rendered ??= at;
    if (!visible || this.locked) {
      pending.waiting = true;
      return;
    }
    track.pending = null;
    if (sample.outcome) sample.lateFreshAt ??= at;
    else if (at < RESUME_DEADLINE_MS) this.finish(epoch, sample, 'fresh', at);
    else {
      sample.lateFreshAt = at;
      this.finish(epoch, sample, 'deadline', null);
    }
  }

  private endRecord(track: Track, sample: ResumeSample, end: PathEnd, at: number): void {
    const record = track.record;
    if (!record || record.end) return;
    record.end = end;
    record.endedAt = at;
    if (end !== 'superseded' && record.servedAt === undefined) sample.pathFailures += 1;
  }

  private sample(track: Track): ResumeSample | null {
    const epoch = this.current();
    if (!epoch) return null;
    if (track.epoch === epoch) return track.sample;
    if (epoch.samples.length >= RESUME_MAX_SAMPLES) return null;
    // A dial that began before the wake and is still running is carried into
    // this epoch: its pre-wake milestones are simply not part of the sample.
    const carried = !track.ended && !track.live && track.generation > 0;
    const sample: ResumeSample = {
      path: this.label(track),
      lifecycle: epoch.lifecycle ?? (track.live ? 'warm' : 'reconnect'),
      attempts: 0,
      pathAttempts: 0,
      pathFailures: 0,
      superseded: 0,
      timeouts: 0,
      failures: 0,
      paths: [],
      phases: {},
      outcome: null,
      doneAt: null,
      direct: null,
    };
    track.epoch = epoch;
    track.sample = sample;
    track.record = null;
    track.pending = null;
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

  /**
   * Applies the count and age bounds, and severs every tracking reference to
   * an evicted epoch so nothing outside the ring keeps its measurements alive.
   */
  private prune(): void {
    const now = this.clock.now();
    const wall = this.clock.wall();
    this.epochs = this.epochs.filter((epoch) => now - epoch.startedAt <= RESUME_RETENTION_MS
      && wall - epoch.wallStartedAt <= RESUME_RETENTION_MS
      && epoch.wallStartedAt <= wall + FUTURE_SKEW_MS).slice(-RESUME_MAX_EPOCHS);
    const retained = new Set(this.epochs);
    for (const track of this.tracks.values()) {
      if (track.epoch && !retained.has(track.epoch)) this.detach(track);
    }
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

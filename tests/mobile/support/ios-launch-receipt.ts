import { spawn, type ChildProcess } from 'node:child_process';
import type { EventEmitter } from 'node:events';
import type { WebDriverCommandEvidence, WebDriverSnapshot } from './webdriver';

export const IOS_LAUNCH_PREDICATE = '((process == "SpringBoard" OR process == "Web" OR process == "runningboardd" OR process == "frontboardd" OR process == "launchd" OR process == "lsd") AND (eventMessage CONTAINS[c] "com.apple.webapp" OR eventMessage CONTAINS[c] "WebClip" OR eventMessage CONTAINS[c] "com.apple.WebKit.PushBundle." OR eventMessage CONTAINS[c] "com.apple.SafariViewService")) OR (process == "Web" AND (subsystem == "com.apple.UIKit" OR subsystem == "com.apple.runningboard" OR subsystem == "com.apple.FrontBoard")) OR (process == "SafariViewService" AND ((subsystem == "com.apple.UIKit" AND (category == "ViewServiceSessionManager" OR category == "ViewServices" OR category == "AppLifecycle")) OR (subsystem == "com.apple.mobilesafari" AND (category == "WebApp" OR category == "WebPush")) OR (subsystem == "com.apple.runningboard" AND category == "monitor")))';
export const IOS_LAUNCH_LIMITS = Object.freeze({ collectionMs: 20_000, readMs: 17_000, termMs: 1_000, killCloseMs: 1_000, serializationMs: 1_000, windowMs: 126_000, armBytes: 16_384, stdoutBytes: 2_097_152, stderrBytes: 16_384, lineBytes: 8_192, records: 4_096, eventsBytes: 524_288, receiptBytes: 32_768, projectedStderrBytes: 8_192 });

export interface IOSReceiptClock {
  wall(): number;
  mono(): number;
  timer(callback: () => void, ms: number): ReturnType<typeof setTimeout>;
  clear(timer: ReturnType<typeof setTimeout>): void;
}
export const iosReceiptClock: IOSReceiptClock = {
  wall: () => Date.now(), mono: () => performance.now(),
  timer: (callback, ms) => setTimeout(callback, ms), clear: timer => clearTimeout(timer),
};

type Stamp = Readonly<{ wallMs: number; monoMs: number }>;
type Boundary = Readonly<{ attemptId: number; dispatchedOrdinal: number | null; sessionGeneration: number; entryMs: number; dispatchMs: number | null; settlementMs: number; acknowledged: boolean }>;
type LaunchStage = 'preparation' | 'click' | 'provider' | 'foreground' | 'other' | 'attachment' | 'complete';
export interface IOSLaunchFailure {
  readonly eligible: boolean;
  readonly reason: 'single-acknowledged-tap' | 'launch-not-eligible';
  readonly stage: LaunchStage;
  readonly invocation: number;
  readonly taps: number;
  readonly bundle: 'com.apple.webapp';
  readonly readiness: 'ready' | 'unavailable';
  readonly page: number | null;
  readonly remainingMs: number | null;
  readonly tap: Stamp | null;
  readonly settled: Stamp | null;
  readonly before: Boundary | null;
  readonly click: Boundary | null;
  readonly fatal: Stamp;
}

function stamp(clock: IOSReceiptClock): Stamp { return Object.freeze({ wallMs: clock.wall(), monoMs: clock.mono() }); }
function positive(value: unknown): value is number { return Number.isSafeInteger(value) && Number(value) > 0; }
function boundary(command?: WebDriverCommandEvidence): Boundary | null {
  const timing = command?.timing;
  if (!timing || !positive(timing.attemptId) || !positive(timing.sessionGeneration)
    || !Number.isFinite(timing.entryMs) || !Number.isFinite(timing.settlementMs)) return null;
  return Object.freeze({ attemptId: timing.attemptId, sessionGeneration: timing.sessionGeneration,
    dispatchedOrdinal: positive(timing.dispatchedOrdinal) ? timing.dispatchedOrdinal : null,
    entryMs: timing.entryMs, dispatchMs: Number.isFinite(timing.dispatchMs) ? timing.dispatchMs! : null,
    settlementMs: timing.settlementMs!, acknowledged: timing.admitted && timing.sent && timing.completed && timing.outcome === 'success' });
}

export class IOSLaunchObservation {
  private invocation = 0;
  private taps = 0;
  private stage: LaunchStage = 'preparation';
  private tap: Stamp | null = null;
  private settled: Stamp | null = null;
  private before: Boundary | null = null;
  private click: Boundary | null = null;
  private page: number | null = null;
  private remainingMs: number | null = null;
  private originalFatal?: unknown;
  private queryRevoked = false;
  failure?: Readonly<IOSLaunchFailure>;

  constructor(readonly clock: IOSReceiptClock = iosReceiptClock) {}
  get queryEligible(): boolean { return this.failure?.eligible === true && !this.queryRevoked; }
  begin(): void {
    this.invocation += 1;
    this.queryRevoked ||= this.invocation > 1;
    this.stage = 'preparation';
  }
  enter(stage: LaunchStage): void {
    this.queryRevoked ||= stage === 'attachment' || stage === 'complete';
    this.stage = stage;
  }
  arm(snapshot: WebDriverSnapshot, page: number, remainingMs: number): void {
    this.taps += 1;
    this.stage = 'click';
    if (this.invocation !== 1 || this.taps !== 1) return;
    this.tap = stamp(this.clock);
    this.before = boundary(snapshot.lastCommand);
    this.page = positive(page) ? page : null;
    this.remainingMs = Number.isFinite(remainingMs) && remainingMs > 0 ? remainingMs : null;
  }
  acknowledge(snapshot: WebDriverSnapshot, icon: string): void {
    if (this.invocation !== 1 || this.taps !== 1) return;
    this.settled = stamp(this.clock);
    const command = snapshot.lastCommand;
    const after = boundary(command);
    const before = this.before;
    if (snapshot.unusable || !snapshot.sessionId || !command || command.method !== 'POST'
      || !/^\/session\/[^/]+\/element\//u.test(command.path)
      || !command.path.endsWith(`/element/${encodeURIComponent(icon)}/click`)
      || command.timing?.operation !== 'other' || !after?.acknowledged || !before?.acknowledged
      || after.attemptId !== before.attemptId + 1 || before.dispatchedOrdinal === null
      || after.dispatchedOrdinal !== before.dispatchedOrdinal + 1 || after.sessionGeneration !== before.sessionGeneration) return;
    this.click = after;
  }
  freeze(error: unknown, snapshot: WebDriverSnapshot): void {
    if (this.invocation !== 1 || this.failure || this.stage === 'attachment' || this.stage === 'complete') return;
    const last = snapshot.lastCommand?.timing;
    const unresolved = !last || last.settlementMs === undefined || (last.sent && !last.completed)
      || last.sessionGeneration !== this.click?.sessionGeneration;
    const eligible = this.taps === 1 && this.click !== null && !unresolved
      && (this.stage === 'provider' || this.stage === 'foreground');
    this.originalFatal = error;
    this.failure = Object.freeze({ eligible, reason: eligible ? 'single-acknowledged-tap' : 'launch-not-eligible',
      invocation: this.invocation, taps: this.taps, stage: this.stage, bundle: 'com.apple.webapp',
      readiness: this.tap ? 'ready' : 'unavailable', page: this.page, remainingMs: this.remainingMs,
      tap: this.tap, settled: this.settled, before: this.before, click: this.click, fatal: stamp(this.clock) });
  }
}

export function iosReceiptError(error: unknown): 'ENOENT' | 'EACCES' | 'EPERM' | 'EEXIST' | 'ENOSPC' | 'EIO' | 'ETIMEDOUT' | 'unknown-withheld' {
  try {
    if (!error || typeof error !== 'object') return 'unknown-withheld';
    const code: unknown = Object.getOwnPropertyDescriptor(error, 'code')?.value;
    switch (code) {
      case 'ENOENT': case 'EACCES': case 'EPERM': case 'EEXIST': case 'ENOSPC': case 'EIO': case 'ETIMEDOUT': return code;
      default: return 'unknown-withheld';
    }
  } catch { return 'unknown-withheld'; }
}

export function iosLaunchQuery(udid: string, failure: IOSLaunchFailure): { argv: string[]; startMs: number; endMs: number } | null {
  if (!failure.eligible || !/^[0-9A-Fa-f]{8}(?:-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}$/u.test(udid)) return null;
  const tap = failure.tap;
  const fatal = failure.fatal;
  if (!tap || ![tap.wallMs, tap.monoMs, fatal.wallMs, fatal.monoMs].every(Number.isFinite)
    || tap.wallMs < 0 || tap.monoMs < 0 || fatal.wallMs < tap.wallMs || fatal.monoMs < tap.monoMs
    || Math.abs((fatal.wallMs - tap.wallMs) - (fatal.monoMs - tap.monoMs)) > 1_000) return null;
  const startMs = Math.floor((tap.wallMs - 5_000) / 1_000) * 1_000;
  const endMs = Math.ceil(fatal.wallMs / 1_000) * 1_000;
  if (startMs < 0 || endMs - startMs > IOS_LAUNCH_LIMITS.windowMs || endMs > 253402300799000) return null;
  const format = (ms: number) => new Date(ms).toISOString().slice(0, 19).replace('T', ' ') + '+0000';
  return { startMs, endMs, argv: ['simctl', 'spawn', udid, 'log', 'show', '--style', 'compact', '--start', format(startMs), '--end', format(endMs), '--predicate', IOS_LAUNCH_PREDICATE] };
}

const consumeLateError = () => undefined;
type PublicFields = Record<string, string | number>;
const nativeUUID = '[0-9A-Fa-f]{8}(?:-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}';
const sessionMessage = new RegExp(`^Configuring connection on service com\\.apple\\.uikit\\.viewservice\\.com\\.apple\\.SafariViewService to host pid ([1-9][0-9]{0,9}) for session (${nativeUUID})$`, 'u');
const sceneMessage = new RegExp(`^sceneOfRecord: sceneID: sceneID:com\\.apple\\.SafariViewService-default  persistentID: (${nativeUUID})$`, 'u');

function projectLine(line: string, ordinal: number, fixtureOrigin: string): PublicFields | null {
  const header = /^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3,6}) (?:Df|Er|Ft|In|Db) (SpringBoard|Web|runningboardd|frontboardd|launchd|lsd|SafariViewService)\[([1-9][0-9]{0,9}):[0-9a-fA-F]{1,16}\] \[(com\.apple\.(?:UIKit|runningboard|FrontBoard|mobilesafari)):([A-Za-z]{1,40})\] (.{1,7800})$/u.exec(line);
  if (!header) return null;
  const [, nativeTime, producer, pid, subsystem, category, message] = header;
  const calendar = nativeTime.slice(0, 23).replace(' ', 'T') + 'Z';
  const parsed = Date.parse(calendar);
  if (!Number.isFinite(parsed) || new Date(parsed).toISOString() !== calendar || Number(pid) > 2_147_483_647) return null;
  const fields: PublicFields = { sourceOrdinal: ordinal, nativeTime, clockMapping: 'unknown', temporalRelationToFatal: 'unavailable', producer, producerPid: Number(pid), subsystem, category };
  let match: RegExpExecArray | null;
  if (producer === 'SafariViewService' && subsystem === 'com.apple.UIKit' && category === 'ViewServiceSessionManager' && (match = sessionMessage.exec(message))) {
    if (Number(match[1]) > 2_147_483_647) return null;
    return { ...fields, relation: 'service-session-host', hostPid: Number(match[1]), serviceSession: match[2] };
  }
  if (producer === 'SafariViewService' && subsystem === 'com.apple.UIKit' && category === 'AppLifecycle' && (match = sceneMessage.exec(message))) {
    return { ...fields, relation: 'service-scene-persistent-id', scene: 'com.apple.SafariViewService-default', persistentId: match[1] };
  }
  if (producer === 'SafariViewService' && subsystem === 'com.apple.mobilesafari' && category === 'WebApp'
    && (match = /^Loading UIWebClip with identifier '([0-9A-F]{32})'; version: ([0-9]{1,5})$/u.exec(message))) {
    return { ...fields, relation: 'service-loads-webclip', webClip: match[1], version: Number(match[2]) };
  }
  if (producer === 'SafariViewService' && subsystem === 'com.apple.mobilesafari' && category === 'WebPush'
    && (match = /^Web Clip with identifier '([0-9A-F]{32})', script from origin <WKSecurityOrigin: 0x[0-9a-f]{1,16}; protocol = (https?); host = ([a-zA-Z0-9.-]{1,253}); port = ([0-9]{1,5})> updated app badge count to ([0-9]{1,10})$/u.exec(message))) {
    const port = Number(match[4]);
    if (port > 65535) return null;
    let observed: string;
    try { observed = new URL(`${match[2]}://${match[3]}${port ? `:${port}` : ''}`).origin; }
    catch { return null; }
    return { ...fields, relation: 'webclip-origin-badge', webClip: match[1], origin: observed === fixtureOrigin ? 'fixture-origin' : 'other-origin', badge: Number(match[5]) };
  }
  if (subsystem === 'com.apple.runningboard' && category === 'monitor'
    && (match = /^Received state update for ([1-9][0-9]{0,9}) \(app<(com\.apple\.(?:webapp|SafariViewService))\(\(null\)\)>, (unknown|running)-(NotVisible|Foreground|Background)$/u.exec(message))) {
    if (Number(match[1]) > 2_147_483_647) return null;
    return { ...fields, relation: 'reported-process-state', subjectPid: Number(match[1]), bundle: match[2], runningState: match[3], visibility: match[4] };
  }
  return null;
}

export interface IOSReceiptHooks {
  clock?: IOSReceiptClock;
  spawn?: (binary: string, argv: string[]) => ChildProcess;
  signal?: AbortSignal;
  queryEligible?: boolean;
}

function freezeTree<T>(value: T): Readonly<T> {
  if (value && typeof value === 'object') {
    for (const child of Object.values(value)) freezeTree(child);
    Object.freeze(value);
  }
  return value;
}

export async function collectIOSLaunchReceipt(udid: string, failure: IOSLaunchFailure, fixtureOrigin: string, hooks: IOSReceiptHooks = {}) {
  const clock = hooks.clock || iosReceiptClock;
  const started = stamp(clock);
  const deadline = started.monoMs + IOS_LAUNCH_LIMITS.collectionMs;
  const readDeadline = started.monoMs + IOS_LAUNCH_LIMITS.readMs;
  const query = hooks.queryEligible === false ? null : iosLaunchQuery(udid, failure);
  const flags = { deadline: false, cancelled: false, receiveCap: false, recordCap: false, outputCap: false, encodingLoss: false, handlerFault: false, overrun: false };
  const process = { spawnAttempted: false, spawned: false, pid: null as number | null, exitCode: null as number | null, signal: null as string | null,
    exitObserved: false, closeObserved: false, closeCode: null as number | null, closeSignal: null as string | null, statusConflict: false,
    signalState: null as 'absent' | 'recognized' | 'unknown-withheld' | null,
    closeSignalState: null as 'absent' | 'recognized' | 'unknown-withheld' | null, unknownSignalObserved: false,
    stdoutEnded: false, stderrEnded: false, stdoutClosed: false, stderrClosed: false,
    spawnError: null as ReturnType<typeof iosReceiptError> | null,
    error: null as ReturnType<typeof iosReceiptError> | null, streamError: null as ReturnType<typeof iosReceiptError> | null,
    authorityRetired: false, termSent: false, killSent: false, signalRejected: false,
    localStop: 'not-started' as 'not-started' | 'confirmed' | 'unconfirmed', hostDescendants: 'unknown', simulatorWorker: 'unknown' };
  const counters = () => ({ received: 0, retained: 0, discarded: 0, overshoot: 0, counterSaturated: false, lines: 0, withheld: 0, invalidUTF8: 0, overlong: 0, incompleteTail: 0 });
  const stdout = counters();
  const stderr = counters();
  const events: string[] = [];
  const stderrClasses: string[] = [];
  let eventBytes = 0;
  let stderrPublicBytes = 0;
  let closed = false;
  let closedAt = started;
  let reading = true;
  let authority = false;
  let child: ChildProcess | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let stageDeadline = readDeadline;
  let termDeadline = readDeadline;
  let closeDeadline = readDeadline;
  let finish!: () => void;
  let stop!: () => void;
  const decoders: Array<{ end(): void }> = [];
  const unsubscribe: Array<() => void> = [];
  const listen = (emitter: EventEmitter, event: string, callback: (...args: any[]) => void) => {
    if (closed) return;
    unsubscribe.push(() => emitter.off(event, callback));
    emitter.on(event, callback);
  };
  const retire = () => { authority = false; if (!closed) process.authorityRetired = true; };
  const safeSignal = (signal: 'SIGTERM' | 'SIGKILL') => {
    if (!child || !authority || closed) return;
    try {
      if (!positive(child.pid) || child.pid !== process.pid || child.exitCode !== null || child.signalCode !== null) { retire(); return; }
      if (!child.kill(signal)) { process.signalRejected = true; retire(); return; }
      if (signal === 'SIGTERM') process.termSent = true;
      else process.killSent = true;
    } catch { process.signalRejected = true; retire(); }
  };
  const guard = (callback: () => void) => {
    if (closed) return;
    try {
      const enteredAt = clock.mono();
      flags.deadline ||= enteredAt >= stageDeadline;
      flags.overrun ||= enteredAt > deadline;
      callback();
      if (closed) return;
      if (clock.mono() >= deadline || (!reading && clock.mono() >= closeDeadline)) {
        flags.deadline = true;
        flags.overrun ||= clock.mono() > deadline;
        finish();
        return;
      }
      if (reading && clock.mono() >= readDeadline) { flags.deadline = true; stop(); }
    } catch { flags.handlerFault = true; retire(); stop(); }
  };
  const schedule = (at: number, callback: () => void) => {
    if (timer !== undefined) clock.clear(timer);
    stageDeadline = Math.min(at, deadline);
    timer = clock.timer(() => guard(callback), Math.max(0, stageDeadline - clock.mono()));
  };
  const expired = () => {
    if (clock.mono() < readDeadline) return false;
    flags.deadline = true;
    stop();
    return true;
  };
  const lineReader = (kind: 'stdout' | 'stderr') => {
    const counts = kind === 'stdout' ? stdout : stderr;
    const cap = kind === 'stdout' ? IOS_LAUNCH_LIMITS.stdoutBytes : IOS_LAUNCH_LIMITS.stderrBytes;
    const buffer = Buffer.alloc(IOS_LAUNCH_LIMITS.lineBytes);
    let length = 0;
    let dropping = false;
    const end = () => {
      if (length || dropping) { counts.incompleteTail += 1; counts.withheld += 1; flags.encodingLoss = true; }
      length = 0;
      dropping = false;
      buffer.fill(0);
    };
    decoders.push({ end });
    const complete = () => {
      counts.lines += 1;
      if (dropping) { counts.withheld += 1; dropping = false; length = 0; return; }
      let line: string;
      try { line = new TextDecoder('utf-8', { fatal: true }).decode(buffer.subarray(0, length)); }
      catch { counts.invalidUTF8 += 1; counts.withheld += 1; flags.encodingLoss = true; length = 0; return; }
      length = 0;
      if (kind === 'stderr') {
        const classification = new Map([
          ['log: Permission denied', 'permission-denied'], ['log: Operation not permitted', 'permission-denied'],
          ['log: Invalid predicate', 'unsupported-predicate'], ['log: Invalid start date', 'unsupported-date'],
          ['log: Invalid end date', 'unsupported-date'], ['simctl: No such file or directory', 'missing-executable'],
        ]).get(line);
        if (!classification) { counts.withheld += 1; return; }
        if (stderrPublicBytes + Buffer.byteLength(classification) + 1 > IOS_LAUNCH_LIMITS.projectedStderrBytes) { flags.outputCap = true; stop(); return; }
        if (expired()) return;
        stderrClasses.push(classification);
        stderrPublicBytes += Buffer.byteLength(classification) + 1;
        return;
      }
      const record = projectLine(line, counts.lines, fixtureOrigin);
      if (!record) { counts.withheld += 1; return; }
      if (events.length >= IOS_LAUNCH_LIMITS.records) { flags.recordCap = true; stop(); return; }
      const serialized = JSON.stringify(record) + '\n';
      const bytes = Buffer.byteLength(serialized);
      if (eventBytes + bytes > IOS_LAUNCH_LIMITS.eventsBytes) { flags.outputCap = true; stop(); return; }
      if (expired()) return;
      events.push(serialized);
      eventBytes += bytes;
    };
    return (chunk: unknown) => guard(() => {
      if (!Buffer.isBuffer(chunk)) { flags.handlerFault = true; stop(); return; }
      counts.counterSaturated ||= !Number.isSafeInteger(counts.received + chunk.byteLength);
      counts.received = Math.min(Number.MAX_SAFE_INTEGER, counts.received + chunk.byteLength);
      counts.overshoot = Math.max(0, counts.received - cap);
      if (!reading || counts.received >= cap || clock.mono() >= readDeadline) {
        counts.discarded = Math.min(Number.MAX_SAFE_INTEGER, counts.discarded + chunk.byteLength);
        if (counts.received >= cap) flags.receiveCap = true;
        if (clock.mono() >= readDeadline) flags.deadline = true;
        stop();
        return;
      }
      let consumed = 0;
      try {
        for (let i = 0; i < chunk.byteLength && reading; i += 1) {
          if (i % IOS_LAUNCH_LIMITS.lineBytes === 0 && expired()) break;
          consumed += 1;
          const byte = chunk[i];
          if (byte === 10) {
            try { complete(); }
            catch { counts.withheld += 1; throw new Error('IOS_LAUNCH_PROJECTION_WITHHELD'); }
            continue;
          }
          if (dropping) continue;
          if (length === buffer.byteLength) { dropping = true; length = 0; counts.overlong += 1; flags.encodingLoss = true; continue; }
          buffer[length++] = byte;
        }
      } finally {
        counts.retained += consumed;
        counts.discarded = Math.min(Number.MAX_SAFE_INTEGER, counts.discarded + chunk.byteLength - consumed);
      }
    });
  };
  const abort = () => guard(() => { flags.cancelled = true; stop(); });
  await new Promise<void>(resolve => {
    finish = () => {
      if (closed) return;
      reading = false;
      for (const decoder of decoders) decoder.end();
      retire();
      if (timer !== undefined) clock.clear(timer);
      hooks.signal?.removeEventListener('abort', abort);
      process.localStop = !process.spawnAttempted ? 'not-started' : process.closeObserved ? 'confirmed' : 'unconfirmed';
      closedAt = stamp(clock);
      closed = true;
      try {
        for (const remove of unsubscribe) remove();
        child?.on('error', consumeLateError);
        child?.stdout?.on('error', consumeLateError);
        child?.stderr?.on('error', consumeLateError);
        child?.stdout?.destroy();
        child?.stderr?.destroy();
      } catch { flags.handlerFault = true; process.localStop = 'unconfirmed'; }
      resolve();
    };
    stop = () => {
      if (closed || !reading) return;
      reading = false;
      const stopAt = clock.mono();
      termDeadline = Math.min(stopAt + IOS_LAUNCH_LIMITS.termMs, started.monoMs + 18_000);
      closeDeadline = Math.min(stopAt + IOS_LAUNCH_LIMITS.termMs + IOS_LAUNCH_LIMITS.killCloseMs, started.monoMs + 19_000);
      if (stopAt >= closeDeadline) { flags.deadline = true; flags.overrun ||= stopAt > deadline; finish(); return; }
      if (stopAt < termDeadline) safeSignal('SIGTERM');
      if (closed) return;
      schedule(termDeadline, () => {
        if (clock.mono() >= closeDeadline) { flags.deadline = true; flags.overrun ||= clock.mono() > deadline; finish(); return; }
        safeSignal('SIGKILL');
        if (!closed) schedule(closeDeadline, finish);
      });
    };
    if (!query || hooks.signal?.aborted) {
      flags.cancelled = hooks.signal?.aborted === true;
      finish();
      return;
    }
    hooks.signal?.addEventListener('abort', abort, { once: true });
    if (expired()) return;
    process.spawnAttempted = true;
    try {
      child = (hooks.spawn || ((binary, argv) => spawn(binary, argv, { detached: false, stdio: ['ignore', 'pipe', 'pipe'] })))('xcrun', query.argv);
      if (closed) {
        child.on('error', consumeLateError);
        child.stdout?.on('error', consumeLateError);
        child.stderr?.on('error', consumeLateError);
        child.stdout?.destroy();
        child.stderr?.destroy();
        return;
      }
      process.pid = positive(child.pid) ? child.pid : null;
      authority = !process.authorityRetired && process.pid !== null;
      const output = lineReader('stdout');
      const errors = lineReader('stderr');
      listen(child, 'error', error => { retire(); guard(() => {
        process.error = iosReceiptError(error);
        if (!process.spawned) process.spawnError = process.error;
        stop();
      }); });
      listen(child, 'spawn', () => guard(() => {
        process.spawned = true;
        if (!positive(child?.pid) || child?.pid !== process.pid) { flags.handlerFault = true; retire(); stop(); }
      }));
      const status = (code: number | null, signal: unknown) => {
        const recognized = typeof signal === 'string' && /^SIG(?:TERM|KILL|INT|ABRT|SEGV|PIPE|HUP|QUIT|BUS|ILL|TRAP|USR1|USR2|ALRM|CHLD|CONT|FPE|INFO|IO|IOT|LOST|POLL|PROF|PWR|STKFLT|STOP|SYS|TSTP|TTIN|TTOU|UNUSED|URG|VTALRM|WINCH|XCPU|XFSZ|BREAK)$/u.test(signal);
        const signalState = signal == null ? 'absent' : recognized ? 'recognized' : 'unknown-withheld';
        process.unknownSignalObserved ||= signalState === 'unknown-withheld';
        return { code: Number.isSafeInteger(code) ? code : null, signal: recognized ? signal as string : null, signalState } as const;
      };
      listen(child, 'exit', (code, signal) => { retire(); guard(() => {
        const observed = status(code, signal);
        if (process.exitObserved) {
          process.statusConflict ||= process.exitCode !== observed.code || process.signal !== observed.signal
            || process.signalState !== observed.signalState || observed.signalState === 'unknown-withheld';
          return;
        }
        process.exitObserved = true;
        process.exitCode = observed.code;
        process.signal = observed.signal;
        process.signalState = observed.signalState;
        stop();
      }); });
      listen(child, 'close', (code, signal) => { retire(); guard(() => {
        const observed = status(code, signal);
        process.closeObserved = true;
        process.closeCode = observed.code;
        process.closeSignal = observed.signal;
        process.closeSignalState = observed.signalState;
        process.statusConflict ||= process.exitObserved && (process.exitCode !== observed.code || process.signal !== observed.signal
          || process.signalState !== observed.signalState || observed.signalState === 'unknown-withheld');
        finish();
      }); });
      for (const kind of ['stdout', 'stderr'] as const) {
        const stream = child[kind];
        if (!stream) { flags.handlerFault = true; continue; }
        listen(stream, 'data', kind === 'stdout' ? output : errors);
        listen(stream, 'end', () => guard(() => { process[`${kind}Ended`] = true; }));
        listen(stream, 'close', () => guard(() => { process[`${kind}Closed`] = true; }));
        listen(stream, 'error', error => guard(() => { process.streamError = iosReceiptError(error); retire(); stop(); }));
      }
      if (closed) return;
      if (process.pid === null) { flags.handlerFault = true; retire(); stop(); }
      if (flags.handlerFault) stop();
      if (reading) schedule(readDeadline, () => { flags.deadline = true; stop(); });
      else if (clock.mono() >= closeDeadline) { flags.deadline = true; finish(); }
      else if (clock.mono() < termDeadline && !process.termSent) safeSignal('SIGTERM');
    } catch (error) {
      process.error = iosReceiptError(error);
      if (!process.spawned) process.spawnError = process.error;
      retire();
      if (child) stop();
      else finish();
    }
  });
  const serializationStartedAt = stamp(clock);
  flags.overrun ||= serializationStartedAt.monoMs > deadline;
  const incomplete = Object.values(flags).some(Boolean) || stdout.discarded > 0 || stderr.discarded > 0 || process.localStop === 'unconfirmed'
    || (process.spawnAttempted && (process.exitCode !== 0 || process.signal !== null || !process.exitObserved || process.statusConflict || process.unknownSignalObserved || process.error !== null || process.streamError !== null));
  const receipt = {
    schema: 1, outcome: !query ? 'not-eligible' : incomplete ? 'incomplete' : events.length ? 'public-projection-unreviewed' : 'identity-not-observable',
    failure, query: query ? { binary: 'xcrun', ...query } : null, limits: IOS_LAUNCH_LIMITS,
    collection: { started, closedAt, serializationStartedAt, serializationObservedAt: serializationStartedAt, elapsedMs: serializationStartedAt.monoMs - started.monoMs, configuredDeadlineMonoMs: deadline, overrunMs: Math.max(0, serializationStartedAt.monoMs - deadline), hardRealtimeGuarantee: false },
    flags, process, stdout, stderr, stderrClasses, eventCount: events.length, eventBytes,
    coverage: { clockMapping: 'unknown', tapToRequest: 'unproved', requestToHost: 'unproved', clipToHost: 'unproved', identityUnavailableReason: 'absent-or-unsupported-in-selected-public-grammar', manualReviewRequired: true, remoteProductionAfterCutoff: 'unknown', retainedByteMeaning: 'accepted-for-bounded-projection-not-public-output', withheldByteMeaning: 'consumed-malformed-or-faulting-lines-count-as-retained', discardedByteMeaning: 'delivered-but-not-consumed-including-fault-tail' },
    persistenceCompletion: 'unavailable-pre-write',
  };
  let eventsText = events.join('');
  let receiptText = JSON.stringify(receipt) + '\n';
  const admittedAt = stamp(clock);
  receipt.collection.serializationObservedAt = admittedAt;
  receipt.collection.elapsedMs = admittedAt.monoMs - started.monoMs;
  receipt.collection.overrunMs = Math.max(0, admittedAt.monoMs - deadline);
  if (admittedAt.monoMs > deadline || Buffer.byteLength(receiptText) > IOS_LAUNCH_LIMITS.receiptBytes) {
    receipt.outcome = 'incomplete';
    flags.overrun ||= admittedAt.monoMs > deadline;
    flags.outputCap ||= Buffer.byteLength(receiptText) > IOS_LAUNCH_LIMITS.receiptBytes;
    eventsText = '';
    receipt.eventCount = 0;
    receipt.eventBytes = 0;
  }
  receiptText = JSON.stringify(receipt) + '\n';
  const finalObservation = stamp(clock);
  if (finalObservation.monoMs > deadline) {
    flags.overrun = true;
    receipt.outcome = 'incomplete';
    receipt.collection.serializationObservedAt = finalObservation;
    receipt.collection.elapsedMs = finalObservation.monoMs - started.monoMs;
    receipt.collection.overrunMs = finalObservation.monoMs - deadline;
    receiptText = JSON.stringify(receipt) + '\n';
  }
  if (Buffer.byteLength(receiptText) > IOS_LAUNCH_LIMITS.receiptBytes || Buffer.byteLength(eventsText) > IOS_LAUNCH_LIMITS.eventsBytes) {
    throw new Error('IOS_LAUNCH_RECEIPT_SIZE');
  }
  return freezeTree({ receipt, eventsText, receiptText, blockNativeDiagnostics: process.localStop === 'unconfirmed' });
}

export type IOSLaunchReceipt = Awaited<ReturnType<typeof collectIOSLaunchReceipt>>;
export interface IOSReceiptWriteStatus {
  artifact: 'directory' | 'events' | 'receipt';
  state: 'not-started' | 'pending' | 'fulfilled' | 'rejected';
  bytes: number;
  started: Stamp | null;
  settled: Stamp | null;
  elapsedMs: number | null;
  error: ReturnType<typeof iosReceiptError> | null;
}
export function iosReceiptWriteStatus(artifact: IOSReceiptWriteStatus['artifact']): IOSReceiptWriteStatus {
  return { artifact, state: 'not-started', bytes: 0, started: null, settled: null, elapsedMs: null, error: null };
}
export async function observeIOSReceiptWrite(status: IOSReceiptWriteStatus, clock: IOSReceiptClock, bytes: number, write: () => Promise<unknown>): Promise<boolean> {
  status.bytes = bytes;
  status.started = stamp(clock);
  status.state = 'pending';
  try { await write(); status.state = 'fulfilled'; }
  catch (error) { status.state = 'rejected'; status.error = iosReceiptError(error); }
  status.settled = stamp(clock);
  status.elapsedMs = status.settled.monoMs - status.started.monoMs;
  return status.state === 'fulfilled';
}

import { createConnection } from 'node:net';
import type { Writable } from 'node:stream';
import { isAbsolute, relative, resolve, sep } from 'node:path';
import { BoundedCommandError, boundedCommand, type BoundedCommandResult } from './bounded-process';
import { monotonicNowNs } from './owned-process';
import { redactText, writeSanitizedJson } from './diagnostics';

export interface AndroidTransportCommandResult {
  startedAt: string;
  endedAt: string;
  stdout: string;
  stderr: string;
  exitCode: number | null;
  signal: string | null;
  timedOut: boolean;
  truncated: boolean;
  unavailable: boolean;
}

export interface AndroidTransportTestFixture {
  adbPort: number;
  hostCommandScript: string;
  stateFile: string;
  acquisitionLogFile: string;
  commandPidFile: string;
  descendantPidFile: string;
  lateWriteFile: string;
}

function validateTransportFixture(value: unknown, outputDir: string): AndroidTransportTestFixture | undefined {
  if (value === undefined) return undefined;
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Android transport fixture is malformed');
  const fixture = value as Record<string, unknown>;
  const fields = ['adbPort', 'hostCommandScript', 'stateFile', 'acquisitionLogFile', 'commandPidFile', 'descendantPidFile', 'lateWriteFile'];
  if (Object.keys(fixture).length !== fields.length || fields.some(field => !Object.hasOwn(fixture, field))
    || !Number.isSafeInteger(fixture.adbPort) || Number(fixture.adbPort) < 1 || Number(fixture.adbPort) > 65_535) {
    throw new Error('Android transport fixture is malformed');
  }
  const paths = fields.slice(1).map(field => fixture[field]);
  if (paths.some(path => typeof path !== 'string' || !path || !isAbsolute(path) || path.includes('\0'))) {
    throw new Error('Android transport fixture paths are malformed');
  }
  const root = resolve(outputDir);
  if (paths.some(path => {
    const pathFromRoot = relative(root, resolve(path as string));
    return pathFromRoot === '' || pathFromRoot === '..' || pathFromRoot.startsWith(`..${sep}`) || isAbsolute(pathFromRoot);
  })) throw new Error('Android transport fixture paths must stay inside its output directory');
  return fixture as unknown as AndroidTransportTestFixture;
}

export interface AndroidTransportCommandDiagnostics {
  observerSupervisor?: Writable;
  onBoundedResult?: (result: BoundedCommandResult) => void;
}

export async function transportCommand(file: string, args: string[], timeoutMs = 1500, limit = 16_384,
  signal?: AbortSignal, diagnostics: AndroidTransportCommandDiagnostics = {}): Promise<AndroidTransportCommandResult> {
  const startedAt = new Date().toISOString();
  let result: BoundedCommandResult | undefined;
  try {
    result = await boundedCommand('/usr/bin/env', ['-u', 'ADB_TRACE', file, ...args], timeoutMs, {
      label: 'collect Android host transport state',
      maxBytes: limit,
      maxBytesPerStream: limit,
      cleanupReservationMs: Math.min(250, Math.floor(timeoutMs / 3)),
      signal,
      observerSupervisor: diagnostics.observerSupervisor,
    });
  } catch (error) {
    if (error instanceof BoundedCommandError) result = error.result;
  }
  if (result) diagnostics.onBoundedResult?.(result);
  const endedAt = new Date().toISOString();
  return {
    startedAt,
    endedAt,
    stdout: redactText(result?.stdout || ''),
    stderr: redactText(result?.stderr || ''),
    exitCode: result && !result.launchError ? result.code : null,
    signal: result?.signal || null,
    timedOut: result?.timedOut || false,
    truncated: result?.outputLimitExceeded || false,
    unavailable: !result || Boolean(result.launchError) || result.aborted || result.timedOut || result.outputLimitExceeded
      || result.code !== 0 || Boolean(result.signal) || !result.childExited || !result.ownedProcessesExited || !result.stdioClosed,
  };
}

export async function transportAdbObservation(serial: string, service: 'get-state' | 'uptime' | 'clock', port = 5037,
  timeoutMs = 1500, signal?: AbortSignal): Promise<AndroidTransportCommandResult> {
  const startedAt = new Date().toISOString();
  return await new Promise<AndroidTransportCommandResult>((resolveResult) => {
    let pending = Buffer.alloc(0);
    let output = Buffer.alloc(0);
    let stage = service === 'get-state' ? 'status' : 'transport';
    let expected: number | undefined;
    let truncated = false;
    let closed = false;
    let resolved = false;
    let terminal: Omit<AndroidTransportCommandResult, 'endedAt'> | undefined;
    const socket = createConnection({ host: '127.0.0.1', port });
    const completeOutput = () => service === 'get-state'
      ? output.toString('utf8').trim().length > 0
      : service === 'clock'
        ? /^\d{10}\.\d{9}\r?\n?$/u.test(output.toString('utf8'))
        : /^(?:transport-observation\r?\n)?\d+(?:\.\d+)?[ \t]+\d+(?:\.\d+)?\r?\n?$/u.test(output.toString('utf8'));
    const complete = (unavailable: boolean, timedOut = false) => {
      if (!terminal) {
        terminal = { startedAt, stdout: redactText(output.toString()),
          stderr: unavailable ? 'ADB observation unavailable' : '',
          exitCode: unavailable || service !== 'get-state' ? null : 0, signal: null, unavailable, timedOut, truncated };
        clearTimeout(timer);
        signal?.removeEventListener('abort', abort);
        if (!closed) socket.destroy();
      }
      if (closed && !resolved) {
        resolved = true;
        resolveResult({ ...terminal, endedAt: new Date().toISOString() });
      }
    };
    const abort = () => complete(true);
    const timer = setTimeout(() => complete(true, true), timeoutMs);
    const request = (value: string) => {
      const bytes = Buffer.from(value);
      if (bytes.length > 65_535) { complete(true); return; }
      try { socket.write(Buffer.concat([Buffer.from(bytes.length.toString(16).padStart(4, '0')), bytes])); }
      catch { complete(true); }
    };
    socket.on('connect', () => request(service === 'get-state'
      ? `host-serial:${serial}:get-state` : `host:transport:${serial}`));
    socket.on('data', (chunk: Buffer) => {
      pending = Buffer.concat([pending, chunk]);
      while (!terminal) {
        if (stage === 'transport' || stage === 'status') {
          if (pending.length < 4) return;
          if (pending.subarray(0, 4).toString() !== 'OKAY') { complete(true); return; }
          pending = pending.subarray(4);
          if (stage === 'transport') {
            stage = 'status';
            request(service === 'clock' ? 'shell:date +%s.%N' : 'shell:echo transport-observation; cat /proc/uptime');
          } else {
            stage = service === 'get-state' ? 'length' : 'output';
          }
          continue;
        }
        if (stage === 'length') {
          if (pending.length < 4) return;
          const length = pending.subarray(0, 4).toString();
          if (!/^[0-9a-f]{4}$/iu.test(length)) { complete(true); return; }
          expected = parseInt(length, 16);
          pending = pending.subarray(4);
          stage = 'output';
        }
        const count = Math.min(pending.length, expected ?? pending.length);
        const retained = Math.min(count, 16_384 - output.length);
        if (retained < count) truncated = true;
        output = Buffer.concat([output, pending.subarray(0, retained)]);
        pending = Buffer.alloc(0);
        if (expected !== undefined) {
          expected -= count;
          if (expected === 0) complete(truncated || !completeOutput());
        }
        return;
      }
    });
    socket.on('error', () => complete(true));
    socket.once('close', () => {
      closed = true;
      if (!terminal) complete(stage !== 'output' || (expected !== undefined && expected !== 0) || truncated || !completeOutput());
      else complete(terminal.unavailable, terminal.timedOut);
    });
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) abort();
  });
}

export interface AndroidTransportTrigger {
  guestEpochSeconds: string;
  triggeredAt: string;
  anchorGuestEpochSeconds?: string;
  guestClockSample?: { guestEpochSeconds: string; startedAt: string; endedAt: string };
}

export interface AndroidTransportCandidate {
  guestEpochSeconds: string;
  triggeredAt: string;
  candidateMonotonicMs: number;
  anchorGuestEpochSeconds: string;
  anchorMonotonicMs: number;
}

export const ANDROID_TRANSPORT_TRIGGER_DEADLINE_MS = 5_000;
export const ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS = 2_000;

export function androidGuestEpochNanoseconds(value: string, requireNanoseconds = false): bigint | undefined {
  const match = value.match(requireNanoseconds ? /^(\d{10})\.(\d{9})$/u : /^(\d{1,10})\.(\d{3,9})$/u);
  if (!match) return undefined;
  return BigInt(match[1]) * 1_000_000_000n + BigInt(match[2].padEnd(9, '0'));
}

const ANDROID_TRANSPORT_RECORD_BYTES = 2_048;
const ANDROID_TRANSPORT_FLUSH = 'timeout expired while flushing socket, closing';
type AndroidTransportEnd = 'complete' | 'irrelevant-tail' | 'failed';

export class AndroidTransportTriggerDetector {
  private readonly record = Buffer.alloc(ANDROID_TRANSPORT_RECORD_BYTES);
  private readonly decoder = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });
  private readonly markerNeedle: string;
  private recordBytes = 0;
  private lastByte = 0;
  private relevanceTail = '';
  private markerSeen = false;
  private flushSeen = false;
  private adbdSeen = false;
  private adbdPrefix = 0;
  private previousWord = false;
  private ended?: AndroidTransportEnd;
  private anchor?: { guestEpochSeconds: string; monotonicMs: number };
  private readonly observedBuffers = new Set<string>();
  private failureValue?: string;
  private accepted = false;

  constructor(private readonly measurementId?: string) {
    if (measurementId !== undefined && !/^[A-Za-z0-9-]{1,80}$/u.test(measurementId)) {
      throw new Error('Android transport measurement identity is malformed');
    }
    this.markerNeedle = measurementId ? `HerdrMeasure: ${measurementId} ` : '';
  }

  get failure(): string | undefined { return this.failureValue; }
  get isAccepted(): boolean { return this.accepted; }

  accept(): boolean {
    if (this.failureValue || this.accepted) return false;
    this.accepted = true;
    return true;
  }

  private scan(text: string): void {
    const searchable = this.relevanceTail + text;
    this.markerSeen ||= Boolean(this.markerNeedle && searchable.includes(this.markerNeedle));
    this.flushSeen ||= searchable.includes(ANDROID_TRANSPORT_FLUSH);
    this.relevanceTail = searchable.slice(1 - Math.max(this.markerNeedle.length, ANDROID_TRANSPORT_FLUSH.length));
    for (const character of text) {
      if (this.adbdSeen) break;
      if (this.adbdPrefix === 4 && character === ':') this.adbdSeen = true;
      else if (this.adbdPrefix === 4 && /\s/u.test(character)) {
        this.previousWord = false;
        continue;
      } else if (this.adbdPrefix > 0 && character === 'adbd'[this.adbdPrefix]) this.adbdPrefix++;
      else this.adbdPrefix = character === 'a' && !this.previousWord ? 1 : 0;
      this.previousWord = /\w/u.test(character);
    }
  }

  private decode(bytes?: Buffer): void {
    try { this.scan(this.decoder.decode(bytes, { stream: bytes !== undefined })); }
    catch { this.failureValue = 'collector trigger record contained invalid UTF-8'; }
  }

  finish(): AndroidTransportEnd {
    if (this.ended) return this.ended;
    this.decode();
    if (this.failureValue) return this.ended = 'failed';
    if (this.recordBytes === 0) return this.ended = 'complete';
    const partialNeedle = [this.markerNeedle, ANDROID_TRANSPORT_FLUSH].some(needle => {
      for (let length = 1; length < needle.length; length++) {
        if (this.relevanceTail.endsWith(needle.slice(0, length))) return true;
      }
      return false;
    });
    const tail = this.record.subarray(0, Math.min(this.recordBytes, this.record.length)).toString('utf8');
    const hasBody = /^\s*\d{1,10}\.\d{3,9}\s+\d+\s+\d+\s+[VDIWEF]\s+[^:\s][^:]*:\s*\S/u.test(tail);
    if (!hasBody || this.markerSeen || this.flushSeen || this.adbdSeen || this.adbdPrefix || partialNeedle) {
      this.failureValue = 'collector trigger record was incomplete at EOF';
      return this.ended = 'failed';
    }
    return this.ended = 'irrelevant-tail';
  }

  observe(chunk: Buffer, monotonicMs = performance.now()): AndroidTransportCandidate[] {
    if (this.failureValue || this.ended) return [];
    const candidates: AndroidTransportCandidate[] = [];
    let offset = 0;
    while (offset < chunk.length) {
      const window = chunk.subarray(offset, Math.min(chunk.length, offset + ANDROID_TRANSPORT_RECORD_BYTES));
      const newline = window.indexOf(10);
      const bytes = newline < 0 ? window : window.subarray(0, newline);
      bytes.copy(this.record, Math.min(this.recordBytes, this.record.length), 0,
        Math.min(bytes.length, Math.max(0, this.record.length - this.recordBytes)));
      this.recordBytes = Math.min(ANDROID_TRANSPORT_RECORD_BYTES + 2, this.recordBytes + bytes.length);
      if (bytes.length) this.lastByte = bytes[bytes.length - 1];
      this.decode(bytes);
      if (this.failureValue) return [];
      offset += bytes.length;
      if (newline < 0) continue;
      offset++;
      this.decode();
      if (this.failureValue) return [];
      const length = this.recordBytes - (this.lastByte === 13 ? 1 : 0);
      let candidate: AndroidTransportCandidate | undefined;
      if (length > this.record.length) {
        if (this.markerSeen || (this.adbdSeen && this.flushSeen)) {
          this.failureValue = 'collector trigger record exceeded its framing bound';
        }
      } else candidate = this.observeLine(this.record.subarray(0, length).toString('utf8'), monotonicMs);
      if (this.failureValue) return [];
      this.recordBytes = 0;
      this.lastByte = 0;
      this.relevanceTail = '';
      this.markerSeen = this.flushSeen = this.adbdSeen = this.previousWord = false;
      this.adbdPrefix = 0;
      if (!candidate || (!this.measurementId && candidates.length)) continue;
      if (candidates.length === 32) {
        this.failureValue = 'collector trigger queue exceeded its bound';
        return [];
      }
      candidates.push(candidate);
    }
    return candidates;
  }

  private observeLine(line: string, monotonicMs: number): AndroidTransportCandidate | undefined {
    if (!this.measurementId) {
      const legacy = line.match(/^\s*(\d+\.\d+)\s+\d+\s+\d+\s+[VDIWEF]\s+adbd\s*:\s*timeout expired while flushing socket, closing\s*$/u);
      if (legacy && !this.accepted) return { guestEpochSeconds: legacy[1], triggeredAt: new Date().toISOString(),
        candidateMonotonicMs: monotonicMs, anchorGuestEpochSeconds: legacy[1], anchorMonotonicMs: monotonicMs };
      return;
    }
    const bufferBanner = line.match(/^--------- beginning of (main|system)$/u);
    if (bufferBanner) {
      if (this.observedBuffers.has(bufferBanner[1]) || (bufferBanner[1] === 'main' && this.anchor)) {
        this.failureValue = 'collector log buffer cursor restarted after admission';
        return;
      }
      this.observedBuffers.add(bufferBanner[1]);
      return;
    }
    if (line.includes(`HerdrMeasure: ${this.measurementId} `)) {
      const marker = line.match(/^\s*(\d{10}\.\d{3,9})\s+\d+\s+\d+\s+I\s+HerdrMeasure:\s+([A-Za-z0-9-]{1,80}) (.*)$/u);
      if (!marker || marker[2] !== this.measurementId) {
        this.failureValue = 'collector measurement start marker was malformed or duplicated';
        return;
      }
      if (marker[3] === 'END' || marker[3].startsWith('OP_BEGIN ') || marker[3].startsWith('OP_END ')) return;
      if (marker[3] !== 'START' || this.anchor) {
        this.failureValue = 'collector measurement start marker was malformed or duplicated';
        return;
      }
      this.anchor = { guestEpochSeconds: marker[1], monotonicMs };
      return;
    }
    if (!this.anchor || this.accepted || !/\badbd\s*:/u.test(line) || !line.includes('timeout expired while flushing socket, closing')) return;
    const signal = line.match(/^\s*(\d{1,10}\.\d{3,9})\s+\d+\s+\d+\s+[VDIWEF]\s+adbd\s*:\s+timeout expired while flushing socket, closing$/u);
    if (!signal) {
      this.failureValue = 'collector transport trigger record was malformed';
      return;
    }
    return { guestEpochSeconds: signal[1], triggeredAt: new Date().toISOString(), candidateMonotonicMs: monotonicMs,
      anchorGuestEpochSeconds: this.anchor.guestEpochSeconds, anchorMonotonicMs: this.anchor.monotonicMs };
  }
}

export class AndroidTransportObservation {
  private readonly triggerDetector = new AndroidTransportTriggerDetector();
  private readonly fixture?: AndroidTransportTestFixture;
  private task?: Promise<void>;
  failure?: string;

  constructor(private readonly serial: string, private readonly outputDir: string,
    private readonly run: typeof transportCommand = transportCommand,
    private readonly runAdb: typeof transportAdbObservation = transportAdbObservation, fixture?: AndroidTransportTestFixture) {
    this.fixture = validateTransportFixture(fixture, outputDir);
  }

  observe(chunk: Buffer): void {
    const candidate = this.triggerDetector.observe(chunk)[0];
    this.failure ||= this.triggerDetector.failure;
    if (candidate && !this.task && this.triggerDetector.accept()) {
      this.start({ guestEpochSeconds: candidate.guestEpochSeconds, triggeredAt: candidate.triggeredAt });
    }
  }

  async observeTrigger(trigger: AndroidTransportTrigger): Promise<void> {
    if (!this.task) this.start(trigger);
    await this.task;
  }

  async finish(): Promise<void> {
    this.triggerDetector.finish();
    await this.task;
    this.failure ||= this.triggerDetector.failure;
  }

  private start(trigger: AndroidTransportTrigger): void {
    const receivedAtNs = monotonicNowNs();
    const triggeredAtMs = Date.parse(trigger.triggeredAt);
    if (!Number.isFinite(triggeredAtMs) || triggeredAtMs > Date.now()) {
      this.failure = 'transport trigger timestamp is invalid or in the future';
      this.task = Promise.resolve();
      return;
    }
    this.task = this.collect(trigger, receivedAtNs).catch(() => { this.failure = 'transport observation could not be saved'; });
  }

  private async collect({ guestEpochSeconds, triggeredAt, anchorGuestEpochSeconds, guestClockSample }: AndroidTransportTrigger, receivedAtNs: bigint): Promise<void> {
    const commands: Array<[string, string, string[]]> = [
      ['host-processes', 'ps', ['-e', '-o', 'pid=,comm=,stat=,wchan=,rss=,pcpu=']],
      ['host-memory', 'cat', ['/proc/meminfo', '/proc/pressure/memory', '/proc/pressure/cpu', '/proc/pressure/io']],
      ['host-tcp', 'ss', ['-tnp']],
      ['adb-state', 'adb-socket', ['get-state']],
      ['guest-uptime', 'adb-socket', ['uptime']],
    ];
    const observations = await Promise.all(commands.map(async ([name, file, args]) => {
      const startedAt = new Date().toISOString();
      try {
        if (file === 'adb-socket') {
          return { name, ...await this.runAdb(this.serial, name === 'adb-state' ? 'get-state' : 'uptime', this.fixture?.adbPort ?? 5037, 1500) };
        }
        if (this.fixture) {
          return { name, ...await this.run('/bin/sh', [this.fixture.hostCommandScript, this.fixture.stateFile,
            this.fixture.acquisitionLogFile, this.fixture.commandPidFile, this.fixture.descendantPidFile,
            this.fixture.lateWriteFile, name], 1500, 16_384) };
        }
        return { name, ...await this.run(file, args, 1500, 16_384) };
      } catch {
        return { name, startedAt, endedAt: new Date().toISOString(), unavailable: true,
          stdout: '', stderr: '', exitCode: null, signal: null, timedOut: false, truncated: false };
      }
    }));
    const failed = observations.some(observation => observation.unavailable || observation.timedOut
      || observation.truncated || (observation.exitCode !== 0 && !(observation.name === 'guest-uptime' && observation.exitCode === null)));
    const completedAtNs = monotonicNowNs();
    const endedAt = new Date().toISOString();
    if (completedAtNs <= receivedAtNs) throw new Error('transport observation did not finish after its trigger');
    if (failed) this.failure = 'transport acquisition was unavailable or failed';
    await writeSanitizedJson(resolve(this.outputDir, 'android-transport-observation.json'), {
      trigger: 'guest-adbd-flush-timeout', guestEpochSeconds, triggeredAt, endedAt,
      hostMonotonicReceipt: { receivedAtNs: receivedAtNs.toString(), completedAtNs: completedAtNs.toString() },
      ...(guestClockSample ? { triggerValidation: {
        method: 'start-anchored-logcat-epoch-with-live-adb-clock-sample',
        anchorGuestEpochSeconds,
        guestClockSample,
        freshnessLimitMs: ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS,
        absoluteEmissionProven: false,
      } } : {}),
      acquisitionStatus: failed ? 'failed' : 'collected', observations,
    });
  }
}

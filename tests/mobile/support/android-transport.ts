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

export class AndroidTransportTriggerDetector {
  private pending = '';
  private anchor?: { guestEpochSeconds: string; monotonicMs: number };
  private readonly observedBuffers = new Set<string>();
  private failureValue?: string;
  private accepted = false;

  constructor(private readonly measurementId?: string) {
    if (measurementId !== undefined && !/^[A-Za-z0-9-]{1,80}$/u.test(measurementId)) {
      throw new Error('Android transport measurement identity is malformed');
    }
  }

  get failure(): string | undefined { return this.failureValue; }
  get isAccepted(): boolean { return this.accepted; }

  accept(): boolean {
    if (this.failureValue || this.accepted) return false;
    this.accepted = true;
    return true;
  }

  observe(chunk: Buffer, monotonicMs = performance.now()): AndroidTransportCandidate[] {
    if (this.failureValue) return [];
    this.pending += chunk.toString('utf8');
    if (this.pending.length > 2_048 && !this.pending.includes('\n')) {
      this.failureValue = 'collector trigger record exceeded its framing bound';
      return [];
    }
    const lines = this.pending.split('\n');
    this.pending = lines.pop() || '';
    if (this.pending.length > 2_048) {
      this.failureValue = 'collector trigger record exceeded its framing bound';
      this.pending = '';
      return [];
    }
    const candidates: AndroidTransportCandidate[] = [];
    for (const rawLine of lines) {
      const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine;
      if (!this.measurementId) {
        const legacy = line.match(/^\s*(\d+\.\d+)\s+\d+\s+\d+\s+[VDIWEF]\s+adbd\s*:\s*timeout expired while flushing socket, closing\s*$/u);
        if (legacy) candidates.push({ guestEpochSeconds: legacy[1], triggeredAt: new Date().toISOString(),
          candidateMonotonicMs: monotonicMs, anchorGuestEpochSeconds: legacy[1], anchorMonotonicMs: monotonicMs });
        continue;
      }
      const bufferBanner = line.match(/^--------- beginning of (main|system)$/u);
      if (bufferBanner) {
        if (this.observedBuffers.has(bufferBanner[1]) || (bufferBanner[1] === 'main' && this.anchor)) {
          this.failureValue = 'collector log buffer cursor restarted after admission';
          return [];
        }
        this.observedBuffers.add(bufferBanner[1]);
        continue;
      }
      if (line.includes(`HerdrMeasure: ${this.measurementId} `)) {
        const marker = line.match(/^(\d{10}\.\d{3,9})\s+\d+\s+\d+\s+I\s+HerdrMeasure:\s+([A-Za-z0-9-]{1,80}) (.*)$/u);
        if (!marker || marker[2] !== this.measurementId) {
          this.failureValue = 'collector measurement start marker was malformed or duplicated';
          return [];
        }
        if (marker[3] === 'END' || marker[3].startsWith('OP_BEGIN ') || marker[3].startsWith('OP_END ')) continue;
        if (marker[3] !== 'START' || this.anchor) {
          this.failureValue = 'collector measurement start marker was malformed or duplicated';
          return [];
        }
        this.anchor = { guestEpochSeconds: marker[1], monotonicMs };
        continue;
      }
      if (!this.anchor || this.accepted || !line.includes('adbd:') || !line.includes('timeout expired while flushing socket, closing')) continue;
      const signal = line.match(/^(\d{1,10}\.\d{3,9})\s+\d+\s+\d+\s+[VDIWEF]\s+adbd:\s+timeout expired while flushing socket, closing$/u);
      if (!signal) {
        this.failureValue = 'collector transport trigger record was malformed';
        return [];
      }
      candidates.push({ guestEpochSeconds: signal[1], triggeredAt: new Date().toISOString(), candidateMonotonicMs: monotonicMs,
        anchorGuestEpochSeconds: this.anchor.guestEpochSeconds, anchorMonotonicMs: this.anchor.monotonicMs });
    }
    return candidates;
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
    if (this.task) return;
    const candidate = this.triggerDetector.observe(chunk)[0];
    if (candidate) this.start({ guestEpochSeconds: candidate.guestEpochSeconds, triggeredAt: candidate.triggeredAt });
  }

  async observeTrigger(trigger: AndroidTransportTrigger): Promise<void> {
    if (!this.task) this.start(trigger);
    await this.task;
  }

  async finish(): Promise<void> {
    await this.task;
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

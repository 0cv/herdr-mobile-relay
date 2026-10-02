import { spawn, type ChildProcess, type SpawnOptions } from 'node:child_process';
import { closeSync, openSync } from 'node:fs';
import { createRequire } from 'node:module';
import { EventEmitter } from 'node:events';
import { fileURLToPath } from 'node:url';
import { isAbsolute } from 'node:path';
import type { Duplex, Readable, Writable } from 'node:stream';

export interface SpawnBinding {
  targetPID: number;
  spawnParentPID: number;
  groupID: number;
  anchorPID: number;
}

export interface TargetExit {
  code: number | null;
  signal: NodeJS.Signals | null;
  launchError?: string;
}

export interface TerminalTargetCallbackReceipt extends SpawnBinding {
  callback: 'spawn';
}

export interface TerminalTransport {
  authorityControl: Writable;
  authorityOutput: Readable;
  targetCallback: Promise<TerminalTargetCallbackReceipt | undefined>;
}

export interface TerminalTarget {
  cwd: string;
  env: Record<string, string>;
}

export interface RetirementEvidence {
  targetExitObserved: boolean;
  targetCloseObserved: boolean;
  anchorExitObserved: boolean;
  anchorCloseObserved: boolean;
  groupAbsent: boolean;
  inputClosedObserved: boolean;
  stdoutNaturalEnd: boolean;
  stdoutCloseObserved: boolean;
  stderrNaturalEnd: boolean;
  stderrCloseObserved: boolean;
  targetStdoutNaturalEnd?: boolean;
  targetStdoutCloseObserved?: boolean;
  targetStderrNaturalEnd?: boolean;
  targetStderrCloseObserved?: boolean;
  managerStdoutNaturalEnd?: boolean;
  managerStdoutCloseObserved?: boolean;
  managerStderrNaturalEnd?: boolean;
  managerStderrCloseObserved?: boolean;
  supervisorStdoutFinished?: boolean;
  supervisorStdoutCloseObserved?: boolean;
  supervisorStderrFinished?: boolean;
  supervisorStderrCloseObserved?: boolean;
  callerStdoutNaturalEnd?: boolean;
  callerStdoutCloseObserved?: boolean;
  callerStderrNaturalEnd?: boolean;
  callerStderrCloseObserved?: boolean;
  managerProcessCreated?: boolean;
  targetDispatchRequested?: boolean;
  targetProcessCreated?: boolean;
  targetNoChildObserved?: boolean;
  targetPIDObserved?: boolean;
  targetExecConfirmed?: boolean;
  targetUnconfirmedCloseObserved?: boolean;
  targetOutputRelayHealthy?: boolean;
  supervisorExitObserved?: boolean;
  supervisorCloseObserved?: boolean;
  supervisorExitCode?: number | null;
  supervisorSignal?: NodeJS.Signals | null;
}

export type OwnedReadable = EventEmitter & { destroy?: () => void };

export interface OwnedProcess {
  supervisor: ChildProcess | undefined;
  stdout: OwnedReadable;
  stderr: OwnedReadable;
  stdin: Writable;
  inputClosed: Promise<void>;
  ready: Promise<void>;
  started: Promise<SpawnBinding | undefined>;
  targetExit: Promise<TargetExit | undefined>;
  targetClose: Promise<TargetExit | undefined>;
  failure: Promise<Error>;
  stopCause: Promise<string>;
  retirement: Promise<RetirementEvidence>;
  terminalTransport?: TerminalTransport;
  binding?: SpawnBinding;
  readonly targetPID?: number;
  readonly spawnParentPID?: number;
  readonly groupID?: number;
  start(): void;
  stop(reason: string): void;
  release(): void;
  retire(reason: string): Promise<RetirementEvidence>;
}

export interface OwnedProcessOptions {
  timeoutMs?: number;
  hardDeadlineNs?: bigint;
  cleanupReservationMs?: number;
  input?: Buffer | string;
  signal?: AbortSignal;
  observerStdout?: Writable;
  observerStderr?: Writable;
  observerSupervisor?: Writable;
  terminalTransport?: boolean;
  terminalTarget?: TerminalTarget;
}

function terminalTargetConfig(value: unknown): TerminalTarget | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined;
  const targetPrototype = Object.getPrototypeOf(value);
  if (targetPrototype !== Object.prototype && targetPrototype !== null) return undefined;
  const targetKeys = Reflect.ownKeys(value);
  if (targetKeys.length !== 2 || !targetKeys.includes('cwd') || !targetKeys.includes('env')) return undefined;
  const cwdDescriptor = Object.getOwnPropertyDescriptor(value, 'cwd');
  const envDescriptor = Object.getOwnPropertyDescriptor(value, 'env');
  if (!cwdDescriptor || !('value' in cwdDescriptor) || !envDescriptor || !('value' in envDescriptor)) return undefined;
  const cwd: unknown = cwdDescriptor.value;
  const environment: unknown = envDescriptor.value;
  if (typeof cwd !== 'string' || !cwd || !isAbsolute(cwd) || cwd.includes('\0')
    || !environment || typeof environment !== 'object' || Array.isArray(environment)) return undefined;
  const prototype = Object.getPrototypeOf(environment);
  if (prototype !== Object.prototype && prototype !== null) return undefined;
  const env: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const key of Reflect.ownKeys(environment)) {
    if (typeof key !== 'string' || !key || key.includes('=') || key.includes('\0')) return undefined;
    const descriptor = Object.getOwnPropertyDescriptor(environment, key);
    if (!descriptor || !('value' in descriptor) || typeof descriptor.value !== 'string' || descriptor.value.includes('\0')) return undefined;
    env[key] = descriptor.value;
  }
  return { cwd, env };
}

interface ControlRecord {
  type?: string;
  [key: string]: unknown;
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(error: Error): void;
}

interface DescriptorFFI {
  FFIType: Record<string, unknown>;
  ptr(value: ArrayBuffer | ArrayBufferView): number;
  dlopen(name: string, definitions: Record<string, unknown>): { symbols: Record<string, (...args: number[]) => number> };
}

interface DescriptorLibrary {
  symbols: Record<string, (...args: number[]) => number>;
  ptr(value: ArrayBuffer | ArrayBufferView): number;
}

let descriptorLibrary: DescriptorLibrary | undefined;

function bunDescriptorLibrary(): DescriptorLibrary {
  if (descriptorLibrary) return descriptorLibrary;
  const ffi = createRequire(import.meta.url)('bun:ffi') as DescriptorFFI;
  const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
  const loaded = ffi.dlopen(library, {
    dup: { args: [ffi.FFIType.i32], returns: ffi.FFIType.i32 },
    clock_gettime: { args: [ffi.FFIType.i32, ffi.FFIType.ptr], returns: ffi.FFIType.i32 },
  });
  descriptorLibrary = { symbols: loaded.symbols, ptr: ffi.ptr };
  return descriptorLibrary;
}

export function monotonicNowNs(): bigint {
  if (!process.versions.bun) return process.hrtime.bigint();
  const library = bunDescriptorLibrary();
  const timespec = Buffer.alloc(16);
  const clockID = process.platform === 'darwin' ? 4 : 1;
  if (library.symbols.clock_gettime(clockID, library.ptr(timespec)) !== 0) throw new Error('OWNED_PROCESS: monotonic clock could not be read');
  return timespec.readBigInt64LE(0) * 1_000_000_000n + timespec.readBigInt64LE(8);
}

export function callerDeadlineToRuntime(deadlineNs: bigint, runtimeNowNs: bigint, callerNowNs: bigint): bigint {
  return runtimeNowNs + deadlineNs - callerNowNs;
}

export function callerDurationToRuntime(timeoutMs: number, runtimeNowNs: bigint, callerStartedAtNs: bigint, callerNowNs: bigint): bigint {
  const requestedNs = BigInt(Math.max(1, timeoutMs)) * 1_000_000n;
  return runtimeNowNs + requestedNs - (callerNowNs - callerStartedAtNs);
}

function duplicateObserverDescriptor(descriptor: number): number {
  if (!process.versions.bun) return descriptor;
  return bunDescriptorLibrary().symbols.dup(descriptor);
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });
  void promise.catch(() => undefined);
  return { promise, resolve, reject };
}

function controlStream(child: ChildProcess): Duplex {
  const value = child.stdio[3];
  if (!value || !('write' in value)) throw new Error('OWNED_PROCESS: control channel was not created');
  return value as Duplex;
}

function readableStream(child: ChildProcess, index: number): Readable {
  const value = child.stdio[index];
  if (!value || !('on' in value)) throw new Error(`OWNED_PROCESS: stream ${index} was not created`);
  return value as Readable;
}

function writableStream(child: ChildProcess, index: number): Writable {
  const value = child.stdio[index];
  if (!value || !('write' in value)) throw new Error(`OWNED_PROCESS: stream ${index} was not created`);
  return value as Writable;
}

function writeRecord(stream: Duplex, value: Record<string, unknown>): void {
  stream.write(`${JSON.stringify(value)}\n`);
}

function processGroupExists(groupID: number): boolean {
  try { process.kill(-groupID, 0); return true; } catch (error) {
    return (error as NodeJS.ErrnoException).code !== 'ESRCH';
  }
}

function untilDeadline<T>(promise: Promise<T>, deadlineNs: bigint): Promise<{ completed: boolean; value?: T }> {
  const remaining = Math.max(0, Number((deadlineNs - monotonicNowNs()) / 1_000_000n));
  if (remaining === 0) return Promise.resolve({ completed: false });
  return new Promise(resolve => {
    const timer = setTimeout(() => resolve({ completed: false }), Math.min(remaining, 2_147_000_000));
    promise.then(value => {
      clearTimeout(timer);
      resolve({ completed: true, value });
    }, () => {
      clearTimeout(timer);
      resolve({ completed: false });
    });
  });
}

export class OwnedProcessHandle implements OwnedProcess {
  readonly supervisor: ChildProcess;
  readonly stdout: Readable;
  readonly stderr: Readable;
  readonly stdin: Writable;
  readonly inputClosed: Promise<void>;
  readonly ready: Promise<void>;
  readonly started: Promise<SpawnBinding | undefined>;
  readonly targetExit: Promise<TargetExit | undefined>;
  readonly targetClose: Promise<TargetExit | undefined>;
  readonly failure: Promise<Error>;
  readonly stopCause: Promise<string>;
  readonly retirement: Promise<RetirementEvidence>;
  readonly terminalTransport?: TerminalTransport;
  binding?: SpawnBinding;
  private readonly control: Duplex;
  private readonly input: Writable;
  private readonly readyDeferred = deferred<void>();
  private readonly startedDeferred = deferred<SpawnBinding | undefined>();
  private readonly targetExitDeferred = deferred<TargetExit | undefined>();
  private readonly targetCloseDeferred = deferred<TargetExit | undefined>();
  private readonly failureDeferred = deferred<Error>();
  private readonly stopCauseDeferred = deferred<string>();
  private readonly retirementDeferred = deferred<RetirementEvidence>();
  private readonly terminalCallbackDeferred = deferred<TerminalTargetCallbackReceipt | undefined>();
  private readonly terminalTarget?: TerminalTarget;
  private readonly supervisorClose = deferred<void>();
  private readonly inputClosedDeferred = deferred<void>();
  private readonly stdoutNaturalEnd = deferred<void>();
  private readonly stderrNaturalEnd = deferred<void>();
  private readonly stdoutClose = deferred<void>();
  private readonly stderrClose = deferred<void>();
  private readonly deadlineNs: bigint;
  private supervisorExited = false;
  private supervisorClosed = false;
  private supervisorExitCode: number | null = null;
  private supervisorSignal: NodeJS.Signals | null = null;
  private supervisorDiagnostic = '';
  private readySettled = false;
  private targetStarted = false;
  private targetDispatchRequested = false;
  private terminalCallbackObserved = false;
  private stdoutEnded = false;
  private stderrEnded = false;
  private stdoutClosed = false;
  private stderrClosed = false;
  private inputWasClosed = false;
  private exitValue?: TargetExit;
  private closeValue?: TargetExit;
  private targetExitMessageObserved = false;
  private targetCloseMessageObserved = false;
  private retirementSourceValue?: RetirementEvidence;
  private retirementValue?: RetirementEvidence;
  private retirementUnproved = false;
  private firstFailure?: Error;
  private released = false;
  private stopped = false;
  private protocol = '';
  private executionTimer?: ReturnType<typeof setTimeout>;
  private hardTimer?: ReturnType<typeof setTimeout>;
  private readonly onAbort = () => this.stop('aborted');
  private binary?: string;
  private args?: string[];
  private executionDeadlineNs?: bigint;
  private hardDeadlineNs?: bigint;
  private killDelayMs = 0;

  constructor(binary: string, args: string[], options: OwnedProcessOptions = {}) {
    if (process.platform === 'win32') throw new Error('OWNED_PROCESS: process-group ownership requires a POSIX host');
    const launchedAtCallerNs = process.hrtime.bigint();
    const launchedAt = monotonicNowNs();
    const callerNowNs = process.hrtime.bigint();
    if (options.hardDeadlineNs !== undefined && options.timeoutMs !== undefined) {
      throw new Error('OWNED_PROCESS: provide a duration or an absolute hard deadline, not both');
    }
    if (options.hardDeadlineNs === undefined && options.timeoutMs === undefined) throw new Error('OWNED_PROCESS: finite lifetime is required');
    const terminalTargetSupplied = Object.hasOwn(options, 'terminalTarget') || options.terminalTarget !== undefined;
    if (terminalTargetSupplied) {
      const terminalTarget = terminalTargetConfig(options.terminalTarget);
      if (!options.terminalTransport || !terminalTarget) throw new Error('OWNED_PROCESS: invalid terminal target configuration');
      this.terminalTarget = terminalTarget;
    }
    const hardDeadlineNs = options.hardDeadlineNs === undefined
      ? callerDurationToRuntime(options.timeoutMs!, launchedAt, launchedAtCallerNs, callerNowNs)
      : callerDeadlineToRuntime(options.hardDeadlineNs, launchedAt, callerNowNs);
    const hardMs = Number((hardDeadlineNs - launchedAt) / 1_000_000n);
    if (hardMs < 1) throw new Error('OWNED_PROCESS: lifetime deadline has expired');
    const requestedCleanup = options.cleanupReservationMs ?? 200;
    if (!Number.isSafeInteger(requestedCleanup) || requestedCleanup < 0) throw new Error('OWNED_PROCESS: invalid cleanup reservation');
    const cleanupMs = Math.min(requestedCleanup, Math.floor(hardMs / 3));
    const executionDeadlineNs = hardDeadlineNs - BigInt(cleanupMs) * 1_000_000n;
    this.deadlineNs = hardDeadlineNs;
    this.ready = this.readyDeferred.promise;
    this.inputClosed = this.inputClosedDeferred.promise;
    this.started = this.startedDeferred.promise;
    this.targetExit = this.targetExitDeferred.promise;
    this.targetClose = this.targetCloseDeferred.promise;
    this.failure = this.failureDeferred.promise;
    this.stopCause = this.stopCauseDeferred.promise;
    this.retirement = this.retirementDeferred.promise;
    const runtime = fileURLToPath(new URL('./owned-process-runtime.ts', import.meta.url));
    const temporaryObserverDescriptors: number[] = [];
    const observerDescriptor = (stream: Writable | undefined): number => {
      let descriptor: number;
      if (stream) {
        const streamDescriptor = (stream as Writable & { fd?: number | null }).fd;
        if (!Number.isSafeInteger(streamDescriptor) || streamDescriptor! < 0) throw new Error('OWNED_PROCESS: observer stream has no open descriptor');
        descriptor = streamDescriptor!;
      } else {
        descriptor = openSync('/dev/null', 'w');
        temporaryObserverDescriptors.push(descriptor);
      }
      if (process.versions.bun) {
        while (temporaryObserverDescriptors.at(-1) === undefined || temporaryObserverDescriptors.at(-1)! < 16) {
          temporaryObserverDescriptors.push(openSync('/dev/null', 'w'));
        }
        const duplicate = duplicateObserverDescriptor(descriptor);
        temporaryObserverDescriptors.push(duplicate);
        if (duplicate < 17) throw new Error('OWNED_PROCESS: observer descriptor could not be isolated from runtime stdio');
        return duplicate;
      }
      return descriptor;
    };
    let supervisor: ChildProcess | undefined;
    this.binary = binary;
    this.args = args;
    this.executionDeadlineNs = executionDeadlineNs;
    this.hardDeadlineNs = hardDeadlineNs;
    this.killDelayMs = Math.min(100, Math.floor(cleanupMs / 3));

    try {
      const stdio = ['pipe', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe',
        observerDescriptor(options.observerStdout), observerDescriptor(options.observerStderr), observerDescriptor(options.observerSupervisor)] satisfies NonNullable<SpawnOptions['stdio']>;
      if (options.terminalTransport) stdio.push('pipe', 'pipe');
      const supervisorCommand = process.versions.bun ? 'bun' : 'node';
      const supervisorRuntimeArgs = process.versions.bun ? ['--no-env-file', '--experimental-strip-types'] : ['--experimental-strip-types'];
      supervisor = spawn(supervisorCommand, [...supervisorRuntimeArgs, runtime, '--supervisor',
        String(executionDeadlineNs), String(hardDeadlineNs)], {
        detached: true,
        stdio,
      });
    } finally {
      for (const descriptor of temporaryObserverDescriptors) closeSync(descriptor);
    }
    if (!supervisor) throw new Error('OWNED_PROCESS: supervisor could not be spawned');
    this.supervisor = supervisor;
    this.stdout = readableStream(this.supervisor, 4);
    this.stderr = readableStream(this.supervisor, 5);
    this.control = controlStream(this.supervisor);
    this.input = writableStream(this.supervisor, 6);
    this.stdin = this.input;
    const markInputEnded = () => {
      if (this.inputWasClosed) return;
      this.inputWasClosed = true;
      this.inputClosedDeferred.resolve();
    };
    this.input.once('finish', markInputEnded);
    this.input.once('close', markInputEnded);
    this.input.on('error', error => {
      this.fail(error);
      this.stop('caller-input-failed');
    });
    if (this.input.writableFinished || this.input.closed) markInputEnded();
    const lease = this.supervisor.stdin;
    if (!lease) throw new Error('OWNED_PROCESS: caller lease writer was not created');
    this.control.setEncoding('utf8');
    this.control.on('data', (chunk: string) => this.readControl(chunk));
    this.control.on('error', error => this.fail(error instanceof Error ? error : new Error('OWNED_PROCESS: control channel failed')));
    if (options.terminalTransport) {
      const authorityControl = writableStream(this.supervisor, 10);
      const authorityOutput = readableStream(this.supervisor, 11);
      this.terminalTransport = { authorityControl, authorityOutput, targetCallback: this.terminalCallbackDeferred.promise };
      const failTerminalTransport = (error: Error) => {
        this.fail(error);
        this.stop('terminal-transport-failed');
      };
      authorityControl.on('error', error => failTerminalTransport(error instanceof Error ? error : new Error('OWNED_PROCESS: terminal authority channel failed')));
      authorityControl.once('close', () => {
        if (!authorityControl.writableFinished && !this.terminalCallbackObserved) {
          failTerminalTransport(new Error('OWNED_PROCESS: terminal authority descriptor closed before target start'));
        }
      });
      authorityOutput.on('error', error => failTerminalTransport(error instanceof Error ? error : new Error('OWNED_PROCESS: terminal output channel failed')));
      authorityOutput.once('end', () => {
        if (!this.terminalCallbackObserved) failTerminalTransport(new Error('OWNED_PROCESS: terminal output descriptor ended before target start'));
      });
    }
    this.control.on('end', () => {
      if (!this.retirementValue?.groupAbsent) this.fail(new Error('OWNED_PROCESS: supervisor control channel closed'));
    });
    this.stdout.on('end', () => { this.stdoutEnded = true; this.stdoutNaturalEnd.resolve(); this.settleRetirementProjection(); });
    this.stdout.on('close', () => { this.stdoutClosed = true; if (this.stdoutEnded) this.stdoutNaturalEnd.resolve(); this.stdoutClose.resolve(); this.settleRetirementProjection(); });
    this.stderr.on('end', () => { this.stderrEnded = true; this.stderrNaturalEnd.resolve(); this.settleRetirementProjection(); });
    this.stderr.on('close', () => { this.stderrClosed = true; if (this.stderrEnded) this.stderrNaturalEnd.resolve(); this.stderrClose.resolve(); this.settleRetirementProjection(); });
    this.supervisor.stderr?.on('data', chunk => {
      this.supervisorDiagnostic = (this.supervisorDiagnostic + String(chunk)).slice(-4_000);
    });
    this.supervisor.once('error', error => this.fail(error));
    this.supervisor.once('exit', (code, signal) => {
      this.supervisorExited = true;
      this.supervisorExitCode = code;
      this.supervisorSignal = signal;
    });
    this.supervisor.once('close', () => {
      this.supervisorClosed = true;
      if (this.supervisorExitCode !== 0 || this.supervisorSignal !== null) {
        const status = this.supervisorSignal ? `signal ${this.supervisorSignal}` : `exit ${this.supervisorExitCode}`;
        const diagnostic = this.supervisorDiagnostic.trim();
        this.fail(new Error(`OWNED_PROCESS: supervisor ended with ${status}${diagnostic ? `: ${diagnostic}` : ''}`));
      }
      this.stopCauseDeferred.resolve('supervisor-closed-unproved');
      this.supervisorClose.resolve();
      if (!this.exitValue) this.targetExitDeferred.resolve(undefined);
      if (!this.closeValue) this.targetCloseDeferred.resolve(undefined);
      this.settleRetirementProjection(true);
      this.settleImpossibleLaunch(new Error('OWNED_PROCESS: supervisor closed before establishing its lease'), true);
    });
    this.executionTimer = setTimeout(() => this.stop('timeout'), Math.max(0, Number((executionDeadlineNs - monotonicNowNs()) / 1_000_000n)));
    this.hardTimer = setTimeout(() => {
      this.stop('hard-deadline');
      this.settleRetirementProjection(true);
    }, Math.max(0, Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n)));
    options.signal?.addEventListener('abort', this.onAbort, { once: true });
    if (options.signal?.aborted) this.onAbort();
    if (options.input === undefined) this.input.end();
    else this.input.end(options.input);
    lease.on('error', error => this.fail(error));
  }

  get targetPID(): number | undefined { return this.binding?.targetPID; }
  get spawnParentPID(): number | undefined { return this.binding?.spawnParentPID; }
  get groupID(): number | undefined { return this.binding?.groupID; }
  get isSupervisorClosed(): boolean { return this.supervisorClosed; }

  start(): void {
    if (this.released || this.stopped) return;
    this.targetDispatchRequested = true;
    writeRecord(this.control, { type: 'start' });
  }

  stop(reason: string): void {
    if (this.stopped || this.released) return;
    this.stopped = true;
    if (this.executionTimer) clearTimeout(this.executionTimer);
    this.settleImpossibleLaunch(new Error(`OWNED_PROCESS: dispatch stopped (${reason})`));
    try { writeRecord(this.control, { type: 'stop', reason }); } catch (error) {
      this.fail(error instanceof Error ? error : new Error('OWNED_PROCESS: stop request failed'));
    }
  }

  release(): void {
    if (this.released) return;
    this.released = true;
    if (this.executionTimer) clearTimeout(this.executionTimer);
    if (this.hardTimer && this.retirementValue) clearTimeout(this.hardTimer);
    writeRecord(this.control, { type: 'release' });
    if (this.supervisor.stdin && !this.supervisor.stdin.writableEnded) this.supervisor.stdin.end();
  }

  async retire(reason: string): Promise<RetirementEvidence> {
    this.stop(reason);
    const retirement = await untilDeadline(this.retirement, this.deadlineNs);
    if (!retirement.completed) this.settleRetirementProjection(true);
    const evidence = await this.retirement;
    await untilDeadline(Promise.all([this.stdoutClose.promise, this.stderrClose.promise]), this.deadlineNs);
    const groupAbsent = evidence.groupAbsent && (!this.binding || processGroupAbsent(this.binding.groupID));
    this.release();
    await Promise.all([
      untilDeadline(this.inputClosed, this.deadlineNs),
      untilDeadline(this.supervisorClose.promise, this.deadlineNs),
    ]);
    const supervisorRetired = this.supervisorExited && this.supervisorClosed;
    return {
      ...evidence,
      groupAbsent: groupAbsent && (!this.binding || processGroupAbsent(this.binding.groupID)),
      inputClosedObserved: this.inputWasClosed,
      callerStdoutNaturalEnd: this.stdoutEnded,
      callerStdoutCloseObserved: this.stdoutClosed,
      callerStderrNaturalEnd: this.stderrEnded,
      callerStderrCloseObserved: this.stderrClosed,
      supervisorExitObserved: this.supervisorExited,
      supervisorCloseObserved: supervisorRetired,
      supervisorExitCode: this.supervisorExitCode,
      supervisorSignal: this.supervisorSignal,
    };
  }

  private emptyRetirementEvidence(): RetirementEvidence {
    return {
      targetExitObserved: this.targetExitMessageObserved, targetCloseObserved: this.targetCloseMessageObserved,
      anchorExitObserved: false, anchorCloseObserved: false, groupAbsent: false,
      inputClosedObserved: this.inputWasClosed, stdoutNaturalEnd: false, stdoutCloseObserved: false,
      stderrNaturalEnd: false, stderrCloseObserved: false,
      targetStdoutNaturalEnd: false, targetStdoutCloseObserved: false,
      targetStderrNaturalEnd: false, targetStderrCloseObserved: false,
      managerStdoutNaturalEnd: false, managerStdoutCloseObserved: false,
      managerStderrNaturalEnd: false, managerStderrCloseObserved: false,
      supervisorStdoutFinished: false, supervisorStdoutCloseObserved: false,
      supervisorStderrFinished: false, supervisorStderrCloseObserved: false,
      callerStdoutNaturalEnd: this.stdoutEnded, callerStdoutCloseObserved: this.stdoutClosed,
      callerStderrNaturalEnd: this.stderrEnded, callerStderrCloseObserved: this.stderrClosed,
      managerProcessCreated: false, targetDispatchRequested: false, targetProcessCreated: false, targetNoChildObserved: false,
      targetPIDObserved: false, targetExecConfirmed: false, targetUnconfirmedCloseObserved: false, targetOutputRelayHealthy: false,
    };
  }

  private settleRetirementProjection(force = false): void {
    if (this.retirementValue) return;
    if (!force && !this.retirementUnproved && !this.supervisorClosed && !this.retirementSourceValue) return;
    const evidence = this.retirementSourceValue || this.emptyRetirementEvidence();
    this.retirementValue = {
      ...evidence,
      targetExitObserved: this.targetExitMessageObserved,
      targetCloseObserved: this.targetCloseMessageObserved,
      callerStdoutNaturalEnd: this.stdoutEnded,
      callerStdoutCloseObserved: this.stdoutClosed,
      callerStderrNaturalEnd: this.stderrEnded,
      callerStderrCloseObserved: this.stderrClosed,
    };
    this.retirementDeferred.resolve(this.retirementValue);
    if (this.released && this.hardTimer) clearTimeout(this.hardTimer);
  }

  private settleImpossibleLaunch(error: Error, definitive = false): void {
    if (!this.readySettled) {
      this.readySettled = true;
      this.readyDeferred.reject(error);
    }
    if (this.targetStarted || (this.targetDispatchRequested && !definitive)) return;
    if (!this.terminalCallbackObserved) this.terminalCallbackDeferred.resolve(undefined);
    this.startedDeferred.resolve(undefined);
  }

  private fail(error: Error): void {
    if (this.firstFailure) return;
    this.firstFailure = error;
    this.failureDeferred.resolve(error);
  }

  private readControl(chunk: string): void {
    this.protocol += chunk;
    if (this.protocol.length > 1_100_000) {
      this.fail(new Error('OWNED_PROCESS: control record exceeded its bound'));
      this.stop('protocol-error');
      return;
    }
    for (;;) {
      const end = this.protocol.indexOf('\n');
      if (end < 0) return;
      const line = this.protocol.slice(0, end);
      this.protocol = this.protocol.slice(end + 1);
      let record: ControlRecord;
      try { record = JSON.parse(line) as ControlRecord; } catch {
        this.fail(new Error('OWNED_PROCESS: invalid control record'));
        this.stop('protocol-error');
        continue;
      }
      this.receive(record);
    }
  }

  private receive(record: ControlRecord): void {
    if (record.type === 'runtime-clock') {
      const clockNs = typeof record.clockNs === 'string' && /^\d+$/u.test(record.clockNs) ? BigInt(record.clockNs) : undefined;
      if (clockNs === undefined || !this.binary || !this.executionDeadlineNs || !this.hardDeadlineNs) {
        this.fail(new Error('OWNED_PROCESS: invalid runtime clock handshake'));
        this.stop('clock-handshake-failed');
        return;
      }
      const callerNowNs = monotonicNowNs();
      const executionRemaining = this.executionDeadlineNs - callerNowNs;
      const hardRemaining = this.hardDeadlineNs - callerNowNs;
      if (hardRemaining <= 0n) {
        this.stop('timeout');
        return;
      }
      writeRecord(this.control, {
        type: 'configure', binary: this.binary, args: this.args,
        executionDeadlineNs: (clockNs + (executionRemaining > 0n ? executionRemaining : 0n)).toString(),
        hardDeadlineNs: (clockNs + hardRemaining).toString(),
        killDelayMs: this.killDelayMs,
        ...(this.terminalTransport ? { terminalTransport: true } : {}),
        ...(this.terminalTarget ? { terminalTarget: this.terminalTarget } : {}),
      });
      return;
    }
    if (record.type === 'ready') {
      this.readySettled = true;
      this.readyDeferred.resolve();
      return;
    }
    if (record.type === 'stopping' || record.type === 'retirement-unproved') {
      if (typeof record.reason === 'string') this.stopCauseDeferred.resolve(record.reason);
      if (!this.targetStarted) this.settleImpossibleLaunch(new Error('OWNED_PROCESS: launch was stopped before dispatch'));
    }
    if (record.type === 'target-started') {
      const binding = { targetPID: record.targetPID, spawnParentPID: record.spawnParentPID, groupID: record.groupID, anchorPID: record.anchorPID };
      const validBinding = [binding.targetPID, binding.spawnParentPID, binding.groupID, binding.anchorPID]
        .every(value => Number.isSafeInteger(value) && Number(value) > 0);
      if (this.targetStarted || !validBinding || (this.terminalTransport ? record.callback !== 'spawn' : record.callback !== undefined)) {
        this.fail(new Error('OWNED_PROCESS: invalid target spawn binding or callback receipt'));
        this.stop('target-binding-invalid');
        return;
      }
      this.binding = binding as SpawnBinding;
      this.targetStarted = true;
      this.startedDeferred.resolve(this.binding);
      if (this.terminalTransport) {
        this.terminalCallbackObserved = true;
        this.terminalCallbackDeferred.resolve({ ...this.binding, callback: 'spawn' });
      }
      return;
    }
    if (record.type === 'target-exit') {
      const value: TargetExit = { code: typeof record.code === 'number' ? record.code : null, signal: typeof record.signal === 'string' ? record.signal as NodeJS.Signals : null };
      this.exitValue = value;
      if (this.binding) this.targetExitMessageObserved = true;
      this.targetExitDeferred.resolve(value);
      return;
    }
    if (record.type === 'target-launch-error') {
      const message = typeof record.message === 'string' ? record.message : 'target launch failed';
      this.settleImpossibleLaunch(new Error(message), true);
      const value: TargetExit = {
        code: typeof record.code === 'number' ? record.code : null,
        signal: typeof record.signal === 'string' ? record.signal as NodeJS.Signals : null,
        launchError: message,
      };
      this.exitValue = value;
      this.targetExitDeferred.resolve(value);
      this.fail(new Error(message));
      return;
    }
    if (record.type === 'target-close') {
      const value: TargetExit = {
        code: typeof record.code === 'number' ? record.code : null,
        signal: typeof record.signal === 'string' ? record.signal as NodeJS.Signals : null,
        ...(typeof record.launchError === 'string' ? { launchError: record.launchError } : {}),
      };
      this.closeValue = value;
      if (this.binding) this.targetCloseMessageObserved = true;
      this.targetCloseDeferred.resolve(value);
      return;
    }
    if (record.type === 'failure' || (record.type === 'stopping' && typeof record.failure === 'string')) {
      this.fail(new Error(typeof record.message === 'string' ? record.message : String(record.failure)));
      return;
    }
    if (record.type === 'retirement-ready' || record.type === 'retirement-unproved') {
      if (!this.targetStarted) this.settleImpossibleLaunch(new Error('OWNED_PROCESS: launch ended without a target start'), true);
      const value = record.evidence && typeof record.evidence === 'object' ? record.evidence as RetirementEvidence : this.emptyRetirementEvidence();
      if (!this.retirementSourceValue) this.retirementSourceValue = value;
      if (!this.exitValue) this.targetExitDeferred.resolve(undefined);
      if (!this.closeValue) this.targetCloseDeferred.resolve(undefined);
      if (record.type === 'retirement-unproved') {
        this.retirementUnproved = true;
        this.fail(new Error('OWNED_PROCESS: target retirement could not be proved before its hard deadline'));
      }
      this.settleRetirementProjection();
    }
  }
}

export function spawnOwnedProcess(binary: string, args: string[], options: OwnedProcessOptions = {}): OwnedProcessHandle {
  if (!binary || !Array.isArray(args) || args.some(value => typeof value !== 'string')) throw new Error('OWNED_PROCESS: invalid executable request');
  return new OwnedProcessHandle(binary, args, options);
}

export function processGroupAbsent(groupID: number): boolean {
  return !processGroupExists(groupID);
}

export async function verifyOwnedRetirement(owned: OwnedProcessHandle): Promise<RetirementEvidence> {
  return owned.retire('normal-retirement');
}

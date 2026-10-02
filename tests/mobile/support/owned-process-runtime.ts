import { closeSync, createReadStream, createWriteStream, fstatSync, writeSync } from 'node:fs';
import { spawn, type ChildProcess } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { Socket } from 'node:net';
import { constants as osConstants } from 'node:os';
import { fileURLToPath } from 'node:url';
import { isAbsolute } from 'node:path';
import { createRequire } from 'node:module';
import { Buffer } from 'node:buffer';
import { StringDecoder } from 'node:string_decoder';
import type { Duplex, Writable } from 'node:stream';

interface LaunchRequest {
  binary: string;
  args: string[];
  executionDeadlineNs: string;
  hardDeadlineNs: string;
  killDelayMs: number;
  terminalTransport?: boolean;
  terminalTarget?: TerminalTarget;
}

interface TerminalTarget {
  cwd: string;
  env: Record<string, string>;
}

type RecordValue = Record<string, unknown>;

export type GroupPollPhase = 'before-manager-close' | 'after-manager-close';

interface OwnedGroupPollOwner {
  inspect(phase: GroupPollPhase): boolean;
  finalize(): void;
}

export function createOwnedGroupPollOwner(options: {
  schedule(callback: () => void, delayMs: number): ReturnType<typeof setTimeout>;
  cancel(handle: ReturnType<typeof setTimeout>): void;
  probe(): boolean;
  observe(present: boolean, phase: GroupPollPhase, isCurrent: () => boolean): void;
}): OwnedGroupPollOwner {
  interface PendingPoll {
    phase: GroupPollPhase;
    token: number;
    handle?: ReturnType<typeof setTimeout>;
  }

  let generation = 0;
  let currentPhase: GroupPollPhase | undefined;
  let pendingPoll: PendingPoll | undefined;
  let finalized = false;
  const observedPhases = new Set<GroupPollPhase>();
  const owns = (token: number, phase: GroupPollPhase) => !finalized && generation === token && currentPhase === phase;

  const sample = (token: number, phase: GroupPollPhase): boolean => {
    if (!owns(token, phase) || observedPhases.has(phase)) return false;
    const present = options.probe();
    if (!owns(token, phase) || observedPhases.has(phase)) return false;
    if (!present) observedPhases.add(phase);
    options.observe(present, phase, () => owns(token, phase));
    if (!owns(token, phase)) return false;
    if (!present) return true;

    const pending: PendingPoll = { phase, token };
    pendingPoll = pending;
    let handle: ReturnType<typeof setTimeout>;
    try {
      handle = options.schedule(() => {
        if (pendingPoll !== pending || !owns(pending.token, pending.phase)) return;
        pendingPoll = undefined;
        const nextToken = ++generation;
        sample(nextToken, pending.phase);
      }, 10);
    } catch (error) {
      if (pendingPoll === pending) pendingPoll = undefined;
      throw error;
    }
    pending.handle = handle;
    if (pendingPoll !== pending || !owns(token, phase)) options.cancel(handle);
    return owns(token, phase);
  };

  return {
    inspect(phase) {
      if (finalized || observedPhases.has(phase)
        || (phase === 'before-manager-close' && currentPhase === 'after-manager-close')) return false;
      const previous = pendingPoll;
      pendingPoll = undefined;
      const token = ++generation;
      currentPhase = phase;
      if (previous?.handle !== undefined) options.cancel(previous.handle);
      if (!owns(token, phase)) return false;
      return sample(token, phase);
    },
    finalize() {
      if (finalized) return;
      finalized = true;
      generation++;
      currentPhase = undefined;
      const previous = pendingPoll;
      pendingPoll = undefined;
      if (previous?.handle !== undefined) options.cancel(previous.handle);
    },
  };
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
  const environmentPrototype = Object.getPrototypeOf(environment);
  if (environmentPrototype !== Object.prototype && environmentPrototype !== null) return undefined;
  const env: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const key of Reflect.ownKeys(environment)) {
    if (typeof key !== 'string' || !key || key.includes('=') || key.includes('\0')) return undefined;
    const descriptor = Object.getOwnPropertyDescriptor(environment, key);
    if (!descriptor || !('value' in descriptor) || typeof descriptor.value !== 'string' || descriptor.value.includes('\0')) return undefined;
    env[key] = descriptor.value;
  }
  return { cwd, env };
}

const closedDescriptors = new Set<number>();

function closeDescriptor(fd: number): void {
  if (closedDescriptors.has(fd)) return;
  closedDescriptors.add(fd);
  try { closeSync(fd); } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'EBADF') throw error;
  }
}

interface NativeSymbols {
  getpid(): number;
  getppid(): number;
  getpgrp(): number;
  getpgid(pid: number): number;
  getsid(pid: number): number;
  setpgid(pid: number, groupID: number): number;
}

interface NativeRuntime {
  symbols: NativeSymbols;
}

class ControlChannel extends EventEmitter {
  private readonly fd: number;
  private readonly incoming;
  private decoder?: StringDecoder;
  private readonly undelivered: Buffer[] = [];
  private endUndelivered = false;
  destroyed = false;
  readableEnded = false;

  constructor(fd: number) {
    super();
    this.fd = fd;
    this.incoming = createReadStream('', { fd, autoClose: false });
    this.incoming.on('data', (value: string | Buffer) => {
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
      if (this.listenerCount('data') && !this.undelivered.length) this.deliver(chunk);
      else this.undelivered.push(chunk);
    });
    this.incoming.on('end', () => {
      if (this.undelivered.length) this.endUndelivered = true;
      else this.end();
    });
    this.incoming.on('error', error => this.emit('error', error));
    this.incoming.once('close', () => closeDescriptor(fd));
    this.on('newListener', event => {
      if (event === 'data' && this.undelivered.length) queueMicrotask(() => this.deliverUndelivered());
    });
  }

  setEncoding(encoding: BufferEncoding): void {
    this.decoder = new StringDecoder(encoding);
  }

  private deliver(chunk: Buffer): void {
    if (!this.decoder) {
      this.emit('data', chunk);
      return;
    }
    const text = this.decoder.write(chunk);
    if (text) this.emit('data', text);
  }

  private deliverUndelivered(): void {
    while (this.undelivered.length && this.listenerCount('data')) this.deliver(this.undelivered.shift()!);
    if (this.endUndelivered && !this.undelivered.length) {
      this.endUndelivered = false;
      this.end();
    }
  }

  private end(): void {
    const remainder = this.decoder?.end();
    if (remainder) this.emit('data', remainder);
    this.readableEnded = true;
    this.emit('end');
  }

  write(value: string, callback?: (error?: Error | null) => void): boolean {
    if (this.destroyed) throw new Error('owned process control channel is closed');
    const bytes = Buffer.from(value);
    let offset = 0;
    try {
      while (offset < bytes.length) {
        try { offset += writeSync(this.fd, bytes, offset, bytes.length - offset); } catch (error) {
          if ((error as NodeJS.ErrnoException).code === 'EINTR') continue;
          throw error;
        }
      }
      callback?.();
      return true;
    } catch (error) {
      const failure = error instanceof Error ? error : new Error('owned process control write failed');
      if (callback) { callback(failure); return false; }
      throw failure;
    }
  }

  destroy(error?: Error): void {
    if (this.destroyed) return;
    this.destroyed = true;
    // Linux close() leaves a read already blocked on this socket waiting for the peer; shutdown wakes it.
    if (!closedDescriptors.has(this.fd)) runtimeLibrary?.symbols.shutdown(this.fd, 0);
    this.incoming.destroy(error);
    closeDescriptor(this.fd);
  }
}

export function controlChannel(fd: number): ControlChannel {
  return new ControlChannel(fd);
}

function supervisorControlChannel(fd: number): ControlChannel {
  return new ControlChannel(fd);
}

function writer(fd: number, autoClose = true) {
  return createWriteStream('', { fd, autoClose });
}

function reader(fd: number) {
  const stream = createReadStream('', { fd, autoClose: false });
  stream.once('close', () => closeDescriptor(fd));
  stream.once('end', () => stream.destroy());
  return stream;
}

function observeInputPipe(source: ReturnType<typeof reader>, observe: (event: string, summary: RecordValue) => void) {
  let destination: Writable | undefined;
  let sourceBytes = 0;
  let countSaturated = false;
  const times: Record<string, string> = {};
  const errors: Record<string, string> = {};
  const snapshot = () => ({
    clockNs: nowNs().toString(), sourceBytes, countSaturated, connected: destination !== undefined,
    sourceReadableLength: source.readableLength, sourceDestroyed: source.destroyed,
    destinationWritableLength: destination?.writableLength ?? null,
    destinationNeedDrain: destination?.writableNeedDrain ?? null,
    destinationEnded: destination?.writableEnded ?? null,
    destinationFinished: destination?.writableFinished ?? null,
    destinationDestroyed: destination?.destroyed ?? null,
    ...times, ...errors,
  });
  const mark = (event: string, error?: unknown) => {
    times[`${event}Ns`] = nowNs().toString();
    if (error !== undefined) {
      const code = (error as NodeJS.ErrnoException)?.code;
      errors[`${event}Code`] = typeof code === 'string' && /^E[A-Z0-9]{1,15}$/u.test(code) ? code : 'other';
    }
    observe(event, snapshot());
  };
  source.once('end', () => mark('sourceEnd'));
  source.once('close', () => mark('sourceClose'));
  source.once('error', error => mark('sourceError', error));
  return {
    snapshot,
    connected(writer: NodeJS.WritableStream) {
      destination = writer as Writable;
      source.on('data', (chunk: Buffer | string) => {
        const next = sourceBytes + Buffer.byteLength(chunk);
        countSaturated ||= !Number.isSafeInteger(next);
        sourceBytes = countSaturated ? Number.MAX_SAFE_INTEGER : next;
        times.firstByteNs ??= nowNs().toString();
        times.lastByteNs = nowNs().toString();
      });
      destination.once('finish', () => mark('destinationFinish'));
      destination.once('close', () => mark('destinationClose'));
      destination.once('error', error => mark('destinationError', error));
      mark('connected');
    },
  };
}

function sanitizedInputPipeSummary(value: unknown): RecordValue {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
  const input = value as RecordValue;
  const summary: RecordValue = {};
  for (const key of ['sourceBytes', 'sourceReadableLength', 'destinationWritableLength']) {
    if (Number.isSafeInteger(input[key]) && Number(input[key]) >= 0) summary[key] = input[key];
  }
  for (const key of ['countSaturated', 'connected', 'sourceDestroyed', 'destinationNeedDrain', 'destinationEnded',
    'destinationFinished', 'destinationDestroyed']) {
    if (typeof input[key] === 'boolean') summary[key] = input[key];
  }
  for (const key of ['clockNs', 'connectedNs', 'firstByteNs', 'lastByteNs', 'sourceEndNs', 'sourceCloseNs', 'sourceErrorNs',
    'destinationFinishNs', 'destinationCloseNs', 'destinationErrorNs']) {
    if (typeof input[key] === 'string' && /^\d{1,30}$/u.test(input[key])) summary[key] = input[key];
  }
  for (const key of ['sourceErrorCode', 'destinationErrorCode']) {
    if (typeof input[key] === 'string' && /^(?:E[A-Z0-9]{1,15}|other)$/u.test(input[key])) summary[key] = input[key];
  }
  return summary;
}

function controlStream(child: ChildProcess): Duplex {
  const value = child.stdio[3];
  if (!value || !('write' in value)) throw new Error('owned process control channel was not created');
  return value as Duplex;
}

function readableStream(child: ChildProcess, index: number): NodeJS.ReadableStream {
  const value = child.stdio[index];
  if (!value || !('on' in value)) throw new Error(`owned process stream ${index} was not created`);
  return value as NodeJS.ReadableStream;
}

function writableStream(child: ChildProcess, index: number): NodeJS.WritableStream {
  const value = child.stdio[index];
  if (!value || !('write' in value)) throw new Error(`owned process stream ${index} was not created`);
  return value as NodeJS.WritableStream;
}

function encode(value: RecordValue): string {
  return `${JSON.stringify(value)}\n`;
}

function parseLines(stream: ControlChannel | Duplex, receive: (value: RecordValue) => void,
  onReceiveError: (error: Error) => void = error => stream.destroy(error)): void {
  let pending = '';
  stream.setEncoding('utf8');
  stream.on('data', (chunk: string) => {
    pending += chunk;
    if (pending.length > 1_100_000) {
      onReceiveError(new Error('owned process control frame exceeded its bound'));
      return;
    }
    for (;;) {
      const end = pending.indexOf('\n');
      if (end < 0) return;
      const line = pending.slice(0, end);
      pending = pending.slice(end + 1);
      let value: RecordValue;
      try {
        value = JSON.parse(line) as RecordValue;
        if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('invalid owned process control frame');
      } catch {
        onReceiveError(new Error('invalid owned process control frame'));
        return;
      }
      try { receive(value); } catch (error) {
        onReceiveError(error instanceof Error ? error : new Error('owned process control handler failed'));
        return;
      }
    }
  });
  stream.on('end', () => {
    if (pending) onReceiveError(new Error('owned process control stream ended with an incomplete frame'));
  });
}

function send(stream: ControlChannel | Duplex, value: RecordValue): void {
  try { stream.write(encode(value)); } catch (error) {
    stream.destroy(error instanceof Error ? error : new Error('owned process control write failed'));
  }
}

interface RuntimeLibrarySymbols {
  clock_gettime(clockID: number, timespec: number): number;
  shutdown(fd: number, how: number): number;
}

interface RuntimeLibrary {
  symbols: RuntimeLibrarySymbols;
  pointer(value: ArrayBuffer | ArrayBufferView): number;
}

const runtimeLibrary: RuntimeLibrary | undefined = process.versions.bun
  ? (() => {
    const ffi = createRequire(import.meta.url)('bun:ffi') as {
      FFIType: Record<string, unknown>;
      ptr(value: ArrayBuffer | ArrayBufferView): number;
      dlopen(name: string, definitions: Record<string, unknown>): { symbols: RuntimeLibrarySymbols };
    };
    const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
    const loaded = ffi.dlopen(library, {
      clock_gettime: { args: [ffi.FFIType.i32, ffi.FFIType.ptr], returns: ffi.FFIType.i32 },
      shutdown: { args: [ffi.FFIType.i32, ffi.FFIType.i32], returns: ffi.FFIType.i32 },
    });
    return { symbols: loaded.symbols, pointer: ffi.ptr };
  })()
  : undefined;
const runtimeTimespec = Buffer.alloc(16);

function nowNs(): bigint {
  if (!runtimeLibrary) return process.hrtime.bigint();
  const clockID = process.platform === 'darwin' ? 4 : 1;
  if (runtimeLibrary.symbols.clock_gettime(clockID, runtimeLibrary.pointer(runtimeTimespec)) !== 0) {
    throw new Error('owned process monotonic clock could not be read');
  }
  return runtimeTimespec.readBigInt64LE(0) * 1_000_000_000n + runtimeTimespec.readBigInt64LE(8);
}

interface AbsoluteDeadlineTimer {
  cancel(): void;
}

function scheduleAt(deadlineNs: string, callback: () => void): AbsoluteDeadlineTimer {
  const deadline = BigInt(deadlineNs);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let cancelled = false;
  const schedule = () => {
    if (cancelled) return;
    const remainingNs = deadline - nowNs();
    const roundedDelayMs = (remainingNs + 999_999n) / 1_000_000n;
    const delay = Number(remainingNs <= 0n ? 0n
      : roundedDelayMs > 2_147_000_000n ? 2_147_000_000n : roundedDelayMs);
    timer = setTimeout(() => {
      if (cancelled) return;
      if (nowNs() < deadline) {
        schedule();
        return;
      }
      cancelled = true;
      callback();
    }, delay);
  };
  schedule();
  return {
    cancel() {
      cancelled = true;
      if (timer) clearTimeout(timer);
    },
  };
}

async function loadNativeRuntime(): Promise<NativeRuntime> {
  const load = new Function('return import("bun:ffi")') as () => Promise<Record<string, unknown>>;
  const ffi = await load();
  const types = ffi.FFIType as Record<string, unknown>;
  const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
  const definitions = {
    getpid: { args: [], returns: types.i32 },
    getppid: { args: [], returns: types.i32 },
    getpgrp: { args: [], returns: types.i32 },
    getpgid: { args: [types.i32], returns: types.i32 },
    getsid: { args: [types.i32], returns: types.i32 },
    setpgid: { args: [types.i32, types.i32], returns: types.i32 },
  };
  const open = ffi.dlopen as (name: string, symbols: Record<string, unknown>) => { symbols: Record<string, (...args: number[]) => unknown> };
  const symbols = open(library, definitions).symbols as unknown as NativeSymbols;
  return { symbols };
}

function signalOwnedProcessGroup(native: NativeRuntime | undefined, groupID: number | undefined,
  sessionID: number, signal: NodeJS.Signals): string | undefined {
  if (!native || !Number.isSafeInteger(groupID) || groupID! <= 0 || !Number.isSafeInteger(sessionID) || sessionID <= 0) {
    return 'process-group identity was unavailable before signal';
  }
  try {
    if (native.symbols.getpgid(0) !== groupID || native.symbols.getsid(0) !== sessionID) {
      return 'live process-group or session identity changed before signal';
    }
    process.kill(-groupID, signal);
    return undefined;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    return `process-group ${signal} failed (${code || 'unknown'})`;
  }
}

function observationErrorCode(error: unknown): string {
  const code = (error as NodeJS.ErrnoException)?.code;
  return typeof code === 'string' && /^E[A-Z0-9]{1,15}$/u.test(code) ? code : 'other';
}

function scheduleObservationRetry(retry: () => void): () => void {
  const timer = setTimeout(retry, 10);
  timer.unref();
  return () => clearTimeout(timer);
}

export function createSupervisorObservationWriter(sink: {
  write(bytes: Buffer, offset: number, length: number): number;
  close(): void;
}, scheduleRetry: (retry: () => void) => () => void = scheduleObservationRetry) {
  const maximumBytes = 128 * 1024;
  const maximumRecordBytes = 16 * 1024;
  const endRecordReserve = 512;
  const queue: Buffer[] = [];
  let headOffset = 0;
  let acceptedBytes = 0;
  let writtenBytes = 0;
  let droppedRecords = 0;
  let droppedRecordBytes = 0;
  let sinkError: string | undefined;
  let accepting = true;
  let sinkClosed = false;
  let cancelRetry: (() => void) | undefined;
  let retryGeneration = 0;
  const dropped = (bytes: number) => {
    droppedRecords = Math.min(Number.MAX_SAFE_INTEGER, droppedRecords + 1);
    droppedRecordBytes = Math.min(Number.MAX_SAFE_INTEGER, droppedRecordBytes + bytes);
  };
  const queuedBytes = () => queue.reduce((total, bytes) => total + bytes.length, 0) - headOffset;
  const cancelScheduledRetry = () => {
    retryGeneration++;
    cancelRetry?.();
    cancelRetry = undefined;
  };
  const discardQueue = () => {
    for (const bytes of queue.splice(0)) dropped(bytes.length);
    headOffset = 0;
  };
  const closeSink = () => {
    if (sinkClosed) return;
    sinkClosed = true;
    cancelScheduledRetry();
    discardQueue();
    try { sink.close(); } catch (error) { sinkError ??= observationErrorCode(error); }
  };
  const flush = () => {
    while (queue.length && !sinkError && !sinkClosed) {
      const head = queue[0]!;
      const remaining = head.length - headOffset;
      let written: number;
      try {
        written = sink.write(head, headOffset, remaining);
      } catch (error) {
        const code = observationErrorCode(error);
        if (code === 'EAGAIN' || code === 'EWOULDBLOCK' || code === 'EINTR') break;
        sinkError = code;
        break;
      }
      if (!Number.isSafeInteger(written) || written < 0 || written > remaining) {
        sinkError = 'other';
        break;
      }
      if (written === 0) break;
      writtenBytes += written;
      headOffset += written;
      if (headOffset === head.length) {
        queue.shift();
        headOffset = 0;
      }
    }
    if (sinkError) {
      closeSink();
      return;
    }
    if (!queue.length) {
      cancelScheduledRetry();
      return;
    }
    if (cancelRetry || !accepting) return;
    const generation = ++retryGeneration;
    cancelRetry = scheduleRetry(() => {
      if (generation !== retryGeneration || sinkClosed) return;
      cancelRetry = undefined;
      flush();
    });
  };
  const record = (value: RecordValue) => {
    if (!accepting) return false;
    const bytes = Buffer.from(encode({ ...value, observerDelivery: 'best-effort', observerDroppedRecords: droppedRecords,
      observerDroppedRecordBytes: droppedRecordBytes }));
    if (sinkError || bytes.length > maximumRecordBytes || acceptedBytes + bytes.length > maximumBytes - endRecordReserve) {
      dropped(bytes.length);
      return false;
    }
    acceptedBytes += bytes.length;
    queue.push(bytes);
    flush();
    return !sinkError;
  };
  return {
    record,
    snapshot: () => ({ acceptedBytes, writtenBytes, queuedBytes: queuedBytes(), droppedRecords, droppedRecordBytes, sinkError,
      accepting, sinkClosed }),
    close() {
      if (!accepting) return;
      accepting = false;
      cancelScheduledRetry();
      if (!sinkError) {
        const end = Buffer.from(encode({ type: 'supervisor-observation-ended', delivery: 'best-effort',
          complete: droppedRecords === 0, droppedRecords, droppedRecordBytes }));
        acceptedBytes += end.length;
        queue.push(end);
        flush();
      }
      closeSink();
    },
  };
}

export function admitNonblockingDescriptor(fd: number, fcntl: (fd: number, command: number, argument: number) => number) {
  const getStatusFlags = 3;
  const setStatusFlags = 4;
  const nonblocking = 4;
  const before = fcntl(fd, getStatusFlags, 0);
  if (!Number.isSafeInteger(before) || before < 0) return { admitted: false as const, reason: 'status-read-failed' };
  const expected = before | nonblocking;
  const changed = expected !== before;
  if (changed && fcntl(fd, setStatusFlags, expected) !== 0) return { admitted: false as const, reason: 'status-write-failed', before };
  const after = fcntl(fd, getStatusFlags, 0);
  if (after === expected) return { admitted: true as const, before, after };
  if (changed) fcntl(fd, setStatusFlags, before);
  return { admitted: false as const, reason: 'status-readback-mismatch', before, after };
}

function supervisorObserver(fd: number) {
  let socket: Socket | undefined;
  let write: (bytes: Buffer, offset: number, length: number) => number;
  let sink: string;
  try {
    const stat = fstatSync(fd);
    if (process.versions.bun && stat.isSocket()) {
      const ffi = createRequire(import.meta.url)('bun:ffi') as {
        FFIType: Record<string, unknown>;
        ptr(bytes: Buffer): number;
        dlopen(name: string, definitions: Record<string, unknown>): { symbols: Record<string, (...args: number[]) => number> };
        toArrayBuffer(address: number, offset: number, length: number): ArrayBuffer;
      };
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const errorName = process.platform === 'darwin' ? '__error' : '__errno_location';
      const symbols = ffi.dlopen(library, {
        send: { args: [ffi.FFIType.i32, ffi.FFIType.ptr, ffi.FFIType.u64, ffi.FFIType.i32], returns: ffi.FFIType.i64 },
        [errorName]: { args: [], returns: ffi.FFIType.ptr },
        ...(process.platform === 'darwin'
          ? { __fcntl: { args: [ffi.FFIType.i32, ffi.FFIType.i32, ffi.FFIType.i64], returns: ffi.FFIType.i32 } } : {}),
      }).symbols;
      if (process.platform === 'darwin') {
        const admission = admitNonblockingDescriptor(fd, (descriptor, command, argument) => symbols.__fcntl!(descriptor, command, argument));
        if (!admission.admitted) throw new Error(`optional observer nonblocking admission failed (${admission.reason})`);
        sink = 'darwin-nonblocking-socket';
      } else sink = 'send-dontwait-socket';
      const messageDontWait = process.platform === 'darwin' ? 0x80 : 0x40;
      const messageNoSignal = process.platform === 'darwin' ? 0x80000 : 0x4000;
      const flags = messageDontWait | messageNoSignal;
      write = (bytes, offset, length) => {
        const written = symbols.send!(fd, ffi.ptr(bytes.subarray(offset)), length, flags);
        if (written >= 0) return Number(written);
        const errno = new DataView(ffi.toArrayBuffer(symbols[errorName]!(), 0, 4)).getInt32(0, true);
        const code = ['EAGAIN', 'EINTR', 'EPIPE', 'EBADF'].find(name => osConstants.errno[name as keyof typeof osConstants.errno] === errno) ?? 'EIO';
        throw Object.assign(new Error('optional observer write failed'), { code });
      };
    } else {
      if (process.versions.bun && stat.isFIFO()) throw new Error('unsupported observer descriptor');
      if (stat.isSocket() || stat.isFIFO()) {
        socket = new Socket({ fd, readable: false, writable: true });
        socket.on('error', () => undefined);
        socket.unref();
      }
      if (!socket && !stat.isFile()) throw new Error('unsupported observer descriptor');
      write = (bytes, offset, length) => writeSync(fd, bytes, offset, length);
      sink = socket ? 'node-nonblocking-stream' : 'synchronous-regular-file';
    }
  } catch {
    sink = 'omitted';
    write = () => { throw Object.assign(new Error('optional observer unavailable'), { code: 'EBADF' }); };
  }
  const writer = createSupervisorObservationWriter({
    write,
    close: () => {
      if (socket) {
        closedDescriptors.add(fd);
        socket.destroy();
      } else closeDescriptor(fd);
    },
  });
  return { ...writer, sink };
}

function armGroupDeadline(deadline: string | undefined, callback: () => void): AbsoluteDeadlineTimer | undefined {
  if (!deadline || !/^\d+$/u.test(deadline)) return undefined;
  return scheduleAt(deadline, callback);
}

async function runAnchorGuardian(): Promise<void> {
  const control = controlChannel(3);
  const startupDeadlineValue = process.argv[3] || '';
  const startupHardDeadlineNs = /^\d+$/u.test(startupDeadlineValue) ? BigInt(startupDeadlineValue) : 0n;
  const expectedAnchorPID = Number(process.argv[4]);
  const expectedSession = Number(process.argv[5]);
  let native: NativeRuntime | undefined;
  let groupID: number | undefined;
  let groupEstablished = false;
  let deadlinesArmed = false;
  let stopping = false;
  let startupExpired = false;
  let executionDeadlineNs = '';
  let hardDeadlineNs = '';
  let killDelayMs = 250;
  let groupKillTimer: ReturnType<typeof setTimeout> | undefined;
  let executionTimer: AbsoluteDeadlineTimer | undefined;
  let hardTimer: AbsoluteDeadlineTimer | undefined;
  const startup = { timer: undefined as AbsoluteDeadlineTimer | undefined };

  const sendControl = (value: RecordValue) => send(control, value);
  const clearDeadlines = () => {
    startup.timer?.cancel();
    executionTimer?.cancel();
    hardTimer?.cancel();
  };
  const killGroup = (signal: NodeJS.Signals): boolean => {
    if (!groupEstablished) return false;
    const failure = signalOwnedProcessGroup(native, groupID, expectedSession, signal);
    if (failure) sendControl({ type: 'guardian-group-signal-refused', signal, groupID, guardianPID: process.pid, reason: failure });
    return !failure;
  };
  const scheduleGroupKill = () => {
    const hardRemaining = Math.max(0, Number(((hardDeadlineNs ? BigInt(hardDeadlineNs) : startupHardDeadlineNs) - nowNs()) / 1_000_000n));
    const delay = Math.min(killDelayMs, hardRemaining);
    if (groupKillTimer) clearTimeout(groupKillTimer);
    groupKillTimer = setTimeout(() => killGroup('SIGKILL'), delay);
  };
  const stopGroup = (reason: string, killNow = false) => {
    if (!groupEstablished) {
      startupExpired = true;
      clearDeadlines();
      sendControl({ type: 'guardian-stopping', reason });
      control.destroy();
      process.exitCode = 1;
      return;
    }
    if (stopping && !killNow) return;
    stopping = true;
    startup.timer?.cancel();
    executionTimer?.cancel();
    sendControl({ type: 'guardian-stopping', reason });
    if (killNow) {
      hardTimer?.cancel();
      if (groupKillTimer) clearTimeout(groupKillTimer);
      killGroup('SIGKILL');
      return;
    }
    if (!killGroup('SIGTERM')) return;
    scheduleGroupKill();
  };
  const onTermination = () => {
    if (!groupEstablished) return stopGroup('anchor-termination');
    if (stopping) return;
    stopping = true;
    startup.timer?.cancel();
    executionTimer?.cancel();
    sendControl({ type: 'guardian-stopping', reason: 'group-termination' });
    scheduleGroupKill();
  };
  control.on('error', () => stopGroup('anchor-control-failed'));
  control.on('end', () => stopGroup('anchor-control-lost'));
  startup.timer = scheduleAt(startupHardDeadlineNs.toString(), () => {
    if (groupEstablished) stopGroup('guardian-startup-hard-deadline', true);
    else {
      startupExpired = true;
      clearDeadlines();
      sendControl({ type: 'guardian-stopping', reason: 'guardian-startup-hard-deadline' });
      control.destroy();
      process.exitCode = 1;
    }
  });

  try {
    native = await loadNativeRuntime();
    const { symbols } = native;
    const guardianPID = symbols.getpid();
    const guardianParentPID = symbols.getppid();
    groupID = symbols.getpgid(0);
    const sessionID = symbols.getsid(0);
    if (guardianPID <= 0 || expectedAnchorPID <= 0 || guardianParentPID !== expectedAnchorPID
      || groupID !== expectedAnchorPID || sessionID !== expectedSession
      || symbols.getpgid(expectedAnchorPID) !== expectedAnchorPID || symbols.getsid(expectedAnchorPID) !== expectedSession) {
      startup.timer?.cancel();
      sendControl({ type: 'guardian-bootstrap-error', message: 'guardian parent, process-group, or session identity did not match its anchor' });
      control.destroy();
      process.exitCode = 1;
      return;
    }
    groupEstablished = true;
    process.on('SIGTERM', onTermination);
    process.on('SIGHUP', onTermination);
    process.on('SIGINT', onTermination);
    if (startupExpired || nowNs() >= startupHardDeadlineNs) {
      stopGroup('guardian-startup-hard-deadline', true);
      return;
    }
    sendControl({ type: 'guardian-native-ready', guardianPID, guardianParentPID, groupID, sessionID, clockNs: nowNs().toString() });
    parseLines(control, message => {
      if (message.type === 'stop' || message.type === 'kill-group') {
        stopGroup(typeof message.reason === 'string' ? message.reason : 'anchor-request', message.type === 'kill-group');
        return;
      }
      if (stopping || startupExpired) {
        sendControl({ type: 'guardian-configuration-refused', reason: 'guardian was already stopping before configuration' });
        return;
      }
      if (message.type !== 'configure-guardian' || deadlinesArmed || typeof message.executionDeadlineNs !== 'string'
        || !/^\d+$/u.test(message.executionDeadlineNs) || typeof message.hardDeadlineNs !== 'string'
        || !/^\d+$/u.test(message.hardDeadlineNs) || typeof message.killDelayMs !== 'number'
        || !Number.isSafeInteger(message.killDelayMs) || message.killDelayMs < 0) {
        stopGroup('invalid-guardian-deadlines');
        return;
      }
      executionDeadlineNs = message.executionDeadlineNs;
      hardDeadlineNs = message.hardDeadlineNs;
      killDelayMs = message.killDelayMs;
      if (BigInt(executionDeadlineNs) >= BigInt(hardDeadlineNs) || BigInt(hardDeadlineNs) > startupHardDeadlineNs
        || nowNs() >= BigInt(hardDeadlineNs)) {
        stopGroup('invalid-guardian-deadlines');
        return;
      }
      deadlinesArmed = true;
      startup.timer?.cancel();
      executionTimer = armGroupDeadline(executionDeadlineNs, () => stopGroup('timeout'));
      hardTimer = armGroupDeadline(hardDeadlineNs, () => stopGroup('hard-deadline', true));
      sendControl({ type: 'guardian-ready', guardianPID, guardianParentPID, groupID, sessionID,
        executionDeadlineNs, hardDeadlineNs, killDelayMs });
    });
  } catch (error) {
    sendControl({ type: 'guardian-bootstrap-error', message: error instanceof Error ? error.message.slice(0, 1_000) : 'guardian initialization failed' });
    clearDeadlines();
    if (groupEstablished) stopGroup('guardian-bootstrap-error');
    else {
      control.destroy();
      process.exitCode = 1;
    }
  }
}

async function runAnchor(): Promise<void> {
  const control = controlChannel(3);
  const startupDeadlineValue = process.argv[3] || '';
  const startupHardDeadlineNs = /^\d+$/u.test(startupDeadlineValue) ? BigInt(startupDeadlineValue) : 0n;
  const expectedParent = Number(process.argv[4]);
  const expectedSession = Number(process.argv[5]);
  let native: NativeRuntime | undefined;
  let anchorPID: number | undefined;
  let groupID: number | undefined;
  let sessionID: number | undefined;
  let guardian: ChildProcess | undefined;
  let guardianControl: Duplex | undefined;
  let guardianPID: number | undefined;
  let guardianNativeReady = false;
  let guardianConfigured = false;
  let guardianClosed = false;
  let pendingAnchorReady: RecordValue | undefined;
  let groupEstablished = false;
  let deadlinesArmed = false;
  let stopping = false;
  let startupExpired = false;
  let executionDeadlineNs = '';
  let hardDeadlineNs = '';
  let killDelayMs = 250;
  let groupKillTimer: ReturnType<typeof setTimeout> | undefined;
  let executionTimer: AbsoluteDeadlineTimer | undefined;
  let hardTimer: AbsoluteDeadlineTimer | undefined;
  const startup = { timer: undefined as AbsoluteDeadlineTimer | undefined };

  const sendControl = (value: RecordValue) => send(control, value);
  const clearDeadlines = () => {
    startup.timer?.cancel();
    executionTimer?.cancel();
    hardTimer?.cancel();
  };
  const killGroup = (signal: NodeJS.Signals): boolean => {
    if (!groupEstablished) return false;
    const failure = signalOwnedProcessGroup(native, groupID, expectedSession, signal);
    if (failure) sendControl({ type: 'anchor-group-signal-refused', signal, groupID, anchorPID, reason: failure });
    return !failure;
  };
  const scheduleGroupKill = () => {
    const hardRemaining = Math.max(0, Number(((hardDeadlineNs ? BigInt(hardDeadlineNs) : startupHardDeadlineNs) - nowNs()) / 1_000_000n));
    const delay = Math.min(killDelayMs, hardRemaining);
    if (groupKillTimer) clearTimeout(groupKillTimer);
    groupKillTimer = setTimeout(() => killGroup('SIGKILL'), delay);
  };
  const stopGroup = (reason: string, killNow = false) => {
    if (!groupEstablished) {
      startupExpired = true;
      clearDeadlines();
      sendControl({ type: 'anchor-stopping', reason });
      control.destroy();
      process.exitCode = 1;
      return;
    }
    if (stopping && !killNow) return;
    stopping = true;
    startup.timer?.cancel();
    executionTimer?.cancel();
    sendControl({ type: 'anchor-stopping', reason });
    if (killNow) {
      hardTimer?.cancel();
      if (groupKillTimer) clearTimeout(groupKillTimer);
      killGroup('SIGKILL');
      return;
    }
    if (!killGroup('SIGTERM')) return;
    scheduleGroupKill();
  };
  const onTermination = () => {
    if (!groupEstablished) return stopGroup('supervisor-request');
    if (stopping) return;
    stopping = true;
    startup.timer?.cancel();
    executionTimer?.cancel();
    sendControl({ type: 'anchor-stopping', reason: 'group-termination' });
    scheduleGroupKill();
  };
  control.on('error', () => stopGroup('supervisor-control-failed'));
  control.on('end', () => stopGroup('supervisor-control-lost'));
  startup.timer = scheduleAt(startupHardDeadlineNs.toString(), () => {
    if (groupEstablished) stopGroup('startup-hard-deadline', true);
    else {
      startupExpired = true;
      clearDeadlines();
      sendControl({ type: 'anchor-stopping', reason: 'startup-hard-deadline' });
      control.destroy();
      process.exitCode = 1;
    }
  });

  try {
    native = await loadNativeRuntime();
    const { symbols } = native;
    anchorPID = symbols.getpid();
    const anchorParent = symbols.getppid();
    if (symbols.setpgid(0, 0) !== 0) {
      sendControl({ type: 'anchor-bootstrap-error', message: 'setpgid failed' });
      process.exitCode = 1;
      return;
    }
    groupID = symbols.getpgid(0);
    sessionID = symbols.getsid(0);
    if (anchorPID <= 0 || anchorParent !== expectedParent || groupID !== anchorPID || sessionID !== expectedSession) {
      sendControl({ type: 'anchor-bootstrap-error', message: 'anchor process-group or session identity did not match the supervisor' });
      process.exitCode = 1;
      return;
    }
    groupEstablished = true;
    process.on('SIGTERM', onTermination);
    process.on('SIGHUP', onTermination);
    process.on('SIGINT', onTermination);
    if (startupExpired || nowNs() >= startupHardDeadlineNs) {
      stopGroup('startup-hard-deadline', true);
      return;
    }

    let resolveGuardianStartup!: () => void;
    let rejectGuardianStartup!: (error: Error) => void;
    const guardianStartup = new Promise<void>((resolve, reject) => {
      resolveGuardianStartup = resolve;
      rejectGuardianStartup = reject;
    });
    guardian = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', fileURLToPath(import.meta.url),
      '--anchor-guardian', startupHardDeadlineNs.toString(), String(anchorPID), String(sessionID)], {
      detached: false,
      stdio: ['ignore', 'ignore', 'ignore', 'pipe'], cwd: process.cwd(), env: process.env,
    });
    if (guardian.pid === undefined) throw new Error('anchor guardian did not receive a process ID');
    guardianControl = controlStream(guardian);
    guardianControl.on('error', error => {
      if (!guardianNativeReady) rejectGuardianStartup(error instanceof Error ? error : new Error('anchor guardian control failed'));
      else stopGroup('anchor-guardian-control-failed');
    });
    parseLines(guardianControl, message => {
      if (message.type === 'guardian-native-ready') {
        const candidatePID = Number(message.guardianPID);
        const clock = typeof message.clockNs === 'string' && /^\d+$/u.test(message.clockNs) ? BigInt(message.clockNs) : undefined;
        if (guardianNativeReady || candidatePID !== guardian!.pid || Number(message.guardianParentPID) !== anchorPID
          || Number(message.groupID) !== groupID || Number(message.sessionID) !== sessionID || clock === undefined
          || native!.symbols.getpgid(candidatePID) !== groupID || native!.symbols.getsid(candidatePID) !== sessionID) {
          const error = new Error('anchor guardian identity or clock did not match its live group member');
          rejectGuardianStartup(error);
          stopGroup('anchor-guardian-admission-failed', true);
          return;
        }
        guardianPID = candidatePID;
        guardianNativeReady = true;
        resolveGuardianStartup();
        return;
      }
      if (message.type === 'guardian-ready') {
        if (!guardianNativeReady || guardianConfigured || stopping || startupExpired || !pendingAnchorReady
          || Number(message.guardianPID) !== guardianPID || Number(message.guardianParentPID) !== anchorPID
          || Number(message.groupID) !== groupID || Number(message.sessionID) !== sessionID
          || message.executionDeadlineNs !== executionDeadlineNs || message.hardDeadlineNs !== hardDeadlineNs
          || message.killDelayMs !== killDelayMs || native!.symbols.getpgid(guardianPID!) !== groupID
          || native!.symbols.getsid(guardianPID!) !== sessionID) {
          stopGroup('anchor-guardian-readiness-invalid', true);
          return;
        }
        guardianConfigured = true;
        sendControl(pendingAnchorReady);
        pendingAnchorReady = undefined;
        return;
      }
      if (message.type === 'guardian-configuration-refused') {
        const failure = new Error(String(message.reason || 'anchor guardian was already stopping'));
        sendControl({ type: 'anchor-guardian-failure', reason: message.type, message: failure.message });
        stopGroup('anchor-guardian-configuration-refused', true);
        return;
      }
      if (message.type === 'guardian-bootstrap-error' || message.type === 'guardian-group-signal-refused') {
        const failure = new Error(String(message.message || message.reason || 'anchor guardian failed'));
        if (!guardianNativeReady) rejectGuardianStartup(failure);
        sendControl({ type: 'anchor-guardian-failure', reason: message.type, message: failure.message });
        stopGroup('anchor-guardian-failed');
        return;
      }
      if (message.type === 'guardian-stopping') {
        stopping = true;
        startup.timer?.cancel();
        executionTimer?.cancel();
        const reason = typeof message.reason === 'string' ? message.reason : 'guardian-stopping';
        sendControl({ type: 'anchor-stopping', reason });
        scheduleGroupKill();
        return;
      }
      stopGroup('invalid-anchor-guardian-message', true);
    }, error => {
      if (!guardianNativeReady) rejectGuardianStartup(error);
      else stopGroup('anchor-guardian-protocol-failed');
    });
    guardian.once('error', error => {
      if (!guardianNativeReady) rejectGuardianStartup(error);
      else stopGroup('anchor-guardian-launch-failed');
    });
    guardian.once('close', () => {
      guardianClosed = true;
      if (!guardianNativeReady) rejectGuardianStartup(new Error('anchor guardian closed before identity admission'));
      else if (!stopping && !startupExpired) stopGroup('anchor-guardian-closed');
    });
    await guardianStartup;
    if (startupExpired || stopping || nowNs() >= startupHardDeadlineNs || guardianPID === undefined) {
      stopGroup('anchor-guardian-startup-expired', true);
      return;
    }
    const guardianIdentity = { guardianPID, guardianParentPID: anchorPID, guardianGroupID: groupID, guardianSessionID: sessionID };
    sendControl({ type: 'anchor-native-ready', anchorPID, spawnParentPID: anchorParent,
      groupID, sessionID, ...guardianIdentity, clockNs: nowNs().toString() });
    parseLines(control, message => {
      if (message.type === 'stop' || message.type === 'kill-group') {
        stopGroup(typeof message.reason === 'string' ? message.reason : 'supervisor-request', message.type === 'kill-group');
        return;
      }
      if (message.type !== 'configure-anchor') {
        stopGroup('invalid-anchor-configuration');
        return;
      }
      if (stopping || startupExpired) {
        sendControl({ type: 'anchor-configuration-refused', reason: 'anchor was already stopping before configuration' });
        stopGroup('anchor-configuration-after-stopping', true);
        return;
      }
      if (deadlinesArmed || typeof message.executionDeadlineNs !== 'string'
        || typeof message.hardDeadlineNs !== 'string' || typeof message.killDelayMs !== 'number') {
        stopGroup('invalid-anchor-deadlines');
        return;
      }
      executionDeadlineNs = message.executionDeadlineNs;
      hardDeadlineNs = message.hardDeadlineNs;
      killDelayMs = Number.isSafeInteger(message.killDelayMs) ? Math.max(0, message.killDelayMs) : 50;
      if (nowNs() >= startupHardDeadlineNs) {
        stopGroup('startup-hard-deadline', true);
        return;
      }
      if (nowNs() >= BigInt(hardDeadlineNs)) {
        stopGroup('hard-deadline', true);
        return;
      }
      if (!guardianControl || guardianClosed) {
        stopGroup('anchor-guardian-control-unavailable', true);
        return;
      }
      deadlinesArmed = true;
      startup.timer?.cancel();
      executionTimer = armGroupDeadline(executionDeadlineNs, () => stopGroup('timeout'));
      hardTimer = armGroupDeadline(hardDeadlineNs, () => stopGroup('hard-deadline', true));
      pendingAnchorReady = { type: 'anchor-ready', anchorPID, spawnParentPID: anchorParent, groupID, sessionID, ...guardianIdentity };
      send(guardianControl, { type: 'configure-guardian', executionDeadlineNs, hardDeadlineNs, killDelayMs });
    });
  } catch (error) {
    sendControl({ type: 'anchor-bootstrap-error', message: error instanceof Error ? error.message.slice(0, 1_000) : 'anchor initialization failed' });
    clearDeadlines();
    if (groupEstablished) stopGroup('anchor-bootstrap-error');
    else {
      control.destroy();
      process.exitCode = 1;
    }
  }
}

function runAnchorManager(): void {
  const control = controlChannel(3);
  const stdout = writer(4, false);
  const stderr = writer(5, false);
  const input = reader(6);
  const startupHardDeadlineNs = process.argv[3] || '';
  const supervisorPID = Number(process.argv[4]);
  const terminalTransportEnabled = process.argv[5] === '--terminal-transport';
  const runtime = fileURLToPath(import.meta.url);
  let anchor: ChildProcess | undefined;
  let anchorControl: Duplex | undefined;
  let target: ChildProcess | undefined;
  let anchorPID: number | undefined;
  let groupID: number | undefined;
  let anchorReady = false;
  let anchorClosed = false;
  let targetSpawned = false;
  let targetExecConfirmed = false;
  let targetClosed = false;
  let targetLaunchErrorSent = false;
  let pendingExit: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  let pendingClose: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  let targetExecutionDeadlineNs = '';
  let firstFailure: string | undefined;
  let stopping = false;
  let closing = false;
  let targetOutputRelayed = false;
  let targetOutputRelayHealthy = true;
  let targetStdoutEnded = false;
  let targetStdoutClosed = false;
  let targetStderrEnded = false;
  let targetStderrClosed = false;
  let targetNoChildSent = false;
  let writerEndRequested = false;
  let stdoutClosed = false;
  let stderrClosed = false;
  let outputRelayFailed = false;
  const outputStates = {
    stdout: { bytes: 0, finished: false, acknowledged: false, descriptorClosed: false, streamClosed: false, finalAcknowledged: false },
    stderr: { bytes: 0, finished: false, acknowledged: false, descriptorClosed: false, streamClosed: false, finalAcknowledged: false },
  };
  const pendingAnchorMessages: RecordValue[] = [];

  const sendSupervisor = (value: RecordValue) => send(control, value);
  const inputObservation = observeInputPipe(input, (event, summary) => sendSupervisor({ type: 'manager-input-observation', event, summary }));
  let inputStopObserved = false;
  const observeInputStop = () => {
    if (inputStopObserved) return;
    inputStopObserved = true;
    sendSupervisor({ type: 'manager-input-observation', event: 'stop', summary: inputObservation.snapshot() });
  };
  let terminalDescriptorsClosed = false;
  const closeTerminalDescriptors = () => {
    if (!terminalTransportEnabled || terminalDescriptorsClosed) return;
    terminalDescriptorsClosed = true;
    closeDescriptor(7);
    closeDescriptor(8);
  };
  if (terminalTransportEnabled) {
    try {
      if (!fstatSync(7).isSocket() || !fstatSync(8).isSocket()) throw new Error('terminal transport descriptors are unavailable');
    } catch {
      sendSupervisor({ type: 'terminal-transport-unavailable', reason: 'descriptor-unavailable' });
      closeTerminalDescriptors();
      input.destroy();
      stdout.destroy();
      stderr.destroy();
      closeDescriptor(4);
      closeDescriptor(5);
      closeDescriptor(6);
      control.destroy();
      process.exitCode = 1;
      return;
    }
  }
  const sendAnchor = (value: RecordValue) => {
    if (!anchorControl) pendingAnchorMessages.push(value);
    else send(anchorControl, value);
  };
  const closeOutputDescriptor = (stream: 'stdout' | 'stderr') => {
    if (outputStates[stream].descriptorClosed) return;
    const destination = stream === 'stdout' ? stdout : stderr;
    if (!destination.destroyed) destination.destroy();
  };
  const abortOutputRelay = (reason: string) => {
    if (outputRelayFailed) return;
    outputRelayFailed = true;
    targetOutputRelayHealthy = false;
    firstFailure ||= reason;
    target?.stdout?.unpipe(stdout);
    target?.stderr?.unpipe(stderr);
    target?.stdout?.resume();
    target?.stderr?.resume();
    closeOutputDescriptor('stdout');
    closeOutputDescriptor('stderr');
  };
  const stop = (reason: string, failure?: string, stream?: 'stdout' | 'stderr') => {
    if (failure) {
      firstFailure ||= failure;
      sendSupervisor({ type: 'failure', reason, stream, message: firstFailure });
    }
    stopping = true;
    observeInputStop();
    if (!targetSpawned) input.resume();
    sendAnchor({ type: 'stop', reason });
  };
  const handleManagerSignal = () => stop('anchor-manager-signal');
  process.on('SIGTERM', handleManagerSignal);
  process.on('SIGHUP', handleManagerSignal);
  process.on('SIGINT', handleManagerSignal);
  const outputRetired = (stream: 'stdout' | 'stderr') => {
    const state = outputStates[stream];
    return state.streamClosed && state.descriptorClosed && (outputRelayFailed || (state.finished && state.finalAcknowledged));
  };
  const closeIfRetired = () => {
    if (closing || !anchorClosed || (targetSpawned && !targetClosed)) return;
    if (!targetOutputRelayed && !writerEndRequested && !outputRelayFailed) {
      writerEndRequested = true;
      stdout.end();
      stderr.end();
    }
    if ((!outputStates.stdout.finished && !stdoutClosed) || (!outputStates.stderr.finished && !stderrClosed)) return;
    if (targetOutputRelayed && (!targetStdoutEnded || !targetStdoutClosed || !targetStderrEnded || !targetStderrClosed)) return;
    if (!outputRetired('stdout') || !outputRetired('stderr')) return;
    closing = true;
    sendSupervisor({ type: 'manager-input-observation', event: 'teardown', summary: inputObservation.snapshot() });
    sendSupervisor({ type: 'manager-teardown-started', anchorClosed, targetSpawned, targetClosed });
    closeTerminalDescriptors();
    input.destroy();
    anchorControl?.destroy();
    control.destroy();
  };
  const outputFailure = (stream: 'stdout' | 'stderr', error: unknown) => {
    if (outputRelayFailed) return;
    const message = error instanceof Error ? error.message : `owned manager ${stream} output failed`;
    abortOutputRelay(message);
    stop('manager-output-failed', message, stream);
    closeIfRetired();
  };
  const outputFinished = (stream: 'stdout' | 'stderr') => {
    const state = outputStates[stream];
    state.finished = true;
    if (!outputRelayFailed) sendSupervisor({ type: 'manager-output-finished', stream, bytes: state.bytes });
    closeIfRetired();
  };
  stdout.once('finish', () => outputFinished('stdout'));
  stderr.once('finish', () => outputFinished('stderr'));
  stdout.once('error', error => outputFailure('stdout', error));
  stderr.once('error', error => outputFailure('stderr', error));
  stdout.once('close', () => {
    stdoutClosed = true;
    outputStates.stdout.streamClosed = true;
    outputStates.stdout.descriptorClosed = true;
    if ((!outputStates.stdout.finished || !outputStates.stdout.acknowledged) && !outputRelayFailed) {
      outputFailure('stdout', new Error('owned manager stdout closed before its byte acknowledgement'));
    }
    closeIfRetired();
  });
  stderr.once('close', () => {
    stderrClosed = true;
    outputStates.stderr.streamClosed = true;
    outputStates.stderr.descriptorClosed = true;
    if ((!outputStates.stderr.finished || !outputStates.stderr.acknowledged) && !outputRelayFailed) {
      outputFailure('stderr', new Error('owned manager stderr closed before its byte acknowledgement'));
    }
    closeIfRetired();
  });
  const sendTargetLaunchError = (message: string, value?: { code: number | null; signal: NodeJS.Signals | null }) => {
    firstFailure ||= message;
    if (targetLaunchErrorSent) return;
    targetLaunchErrorSent = true;
    const observedExit = value ?? pendingExit ?? (target && (target.exitCode !== null || target.signalCode !== null)
      ? { code: target.exitCode, signal: target.signalCode } : undefined);
    sendSupervisor({ type: 'target-launch-error', message,
      code: observedExit?.code ?? null, signal: observedExit?.signal ?? null });
  };
  const deliverTargetExit = (value: { code: number | null; signal: NodeJS.Signals | null }) => {
    if (!targetExecConfirmed) return;
    sendSupervisor({ type: 'target-exit', code: value.code, signal: value.signal });
  };
  const deliverTargetClose = (value: { code: number | null; signal: NodeJS.Signals | null }) => {
    targetClosed = true;
    if (targetExecConfirmed) sendSupervisor({ type: 'target-close', code: value.code, signal: value.signal });
    else sendSupervisor({ type: 'target-unconfirmed-close', code: value.code, signal: value.signal,
      ...(targetLaunchErrorSent && firstFailure ? { launchError: firstFailure } : {}) });
    closeIfRetired();
  };
  const confirmTargetSpawn = (child: ChildProcess) => {
    if (targetExecConfirmed || child.pid === undefined) return;
    targetExecConfirmed = true;
    sendSupervisor({ type: 'target-started', targetPID: child.pid, spawnParentPID: process.pid,
      groupID, anchorPID, ...(terminalTransportEnabled ? { callback: 'spawn' } : {}) });
    if (pendingExit) deliverTargetExit(pendingExit);
    if (pendingClose) deliverTargetClose(pendingClose);
  };
  const sendNoChild = () => {
    if (target || targetNoChildSent) return;
    targetNoChildSent = true;
    sendSupervisor({ type: 'target-no-child', launchRequestReceived: true, spawnParentPID: process.pid, groupID, anchorPID });
  };
  const relayTargetOutput = (source: NodeJS.ReadableStream, destination: ReturnType<typeof writer>, stream: 'stdout' | 'stderr') => {
    let sourceEnded = false;
    const failRelay = (reason: string, message: string) => {
      if (!targetOutputRelayHealthy) return;
      targetOutputRelayHealthy = false;
      source.unpipe(destination);
      source.resume();
      abortOutputRelay(message);
      stop(reason, message, stream);
    };
    source.on('data', chunk => {
      const bytes = Buffer.isBuffer(chunk) ? chunk.byteLength : Buffer.byteLength(String(chunk));
      const state = outputStates[stream];
      if (!Number.isSafeInteger(state.bytes + bytes)) {
        failRelay('target-output-failed', `${stream} relay byte count exceeded its safe bound`);
        return;
      }
      state.bytes += bytes;
    });
    source.once('end', () => {
      sourceEnded = true;
      if (stream === 'stdout') targetStdoutEnded = true;
      else targetStderrEnded = true;
      sendSupervisor({ type: 'target-source-end', stream });
      closeIfRetired();
    });
    source.once('close', () => {
      if (stream === 'stdout') targetStdoutClosed = true;
      else targetStderrClosed = true;
      sendSupervisor({ type: 'target-source-close', stream, naturalEnd: sourceEnded });
      closeIfRetired();
    });
    destination.once('finish', () => {
      if (!sourceEnded) failRelay('target-output-failed', `${stream} relay finished before target source EOF`);
    });
    destination.once('error', error => {
      failRelay('target-output-failed', error instanceof Error ? error.message : `${stream} relay failed`);
    });
    destination.once('close', () => {
      if (!destination.writableFinished || !sourceEnded) {
        failRelay('target-output-failed', `${stream} relay closed before target source EOF and writer finish`);
      }
    });
    source.once('error', error => failRelay('target-stream-error', error instanceof Error ? error.message : `${stream} stream failed`));
    source.pipe(destination);
  };
  const launchTarget = async (message: RecordValue) => {
    const terminalTargetSupplied = Object.hasOwn(message, 'terminalTarget');
    const terminalTarget = terminalTargetSupplied ? terminalTargetConfig(message.terminalTarget) : undefined;
    if (targetSpawned || !anchorReady || groupID === undefined || typeof message.binary !== 'string'
      || !Array.isArray(message.args) || message.args.some(value => typeof value !== 'string')
      || typeof message.executionDeadlineNs !== 'string' || !/^\d+$/u.test(message.executionDeadlineNs)
      || (terminalTargetSupplied && (!terminalTransportEnabled || !terminalTarget))) {
      sendTargetLaunchError('invalid target launch request or terminal target configuration');
      sendNoChild();
      stop('invalid-target-launch', 'invalid target launch request from supervisor');
      return;
    }
    targetExecutionDeadlineNs = message.executionDeadlineNs;
    if (stopping || nowNs() >= BigInt(targetExecutionDeadlineNs)) {
      sendNoChild();
      stop('timeout');
      return;
    }
    const binary = message.binary;
    const args = message.args as string[];
    let managerGroupRestoreFailed: boolean;
    try {
      const native = await loadNativeRuntime();
      if (stopping || nowNs() >= BigInt(targetExecutionDeadlineNs)) {
        sendNoChild();
        stop('timeout');
        return;
      }
      if (native.symbols.getpgid(0) !== supervisorPID || native.symbols.getsid(0) !== supervisorPID) {
        throw new Error('anchor manager process group or session changed before target launch');
      }
      const spawnTargetInGroup = () => {
        if (native.symbols.setpgid(0, groupID!) !== 0) throw new Error('anchor manager could not enter the target process group');
        let spawnFailure: unknown;
        try {
          if (terminalTargetSupplied && !terminalTarget) throw new Error('invalid terminal target configuration');
          target = spawn(binary, args, {
            detached: false,
            stdio: terminalTransportEnabled ? ['pipe', 'pipe', 'pipe', 7, 8] : ['pipe', 'pipe', 'pipe'],
            cwd: terminalTarget ? terminalTarget.cwd : process.cwd(),
            env: terminalTarget ? terminalTarget.env : process.env,
          });
        } catch (error) {
          spawnFailure = error;
        } finally {
          closeTerminalDescriptors();
        }
        const restoreFailed = native.symbols.setpgid(0, supervisorPID) !== 0;
        if (spawnFailure) throw spawnFailure;
        return restoreFailed;
      };
      managerGroupRestoreFailed = spawnTargetInGroup();
    } catch (error) {
      sendTargetLaunchError(error instanceof Error ? error.message : 'target process could not be spawned');
      sendNoChild();
      stop('target-launch-error');
      closeIfRetired();
      return;
    }
    const targetProcess = target;
    if (!targetProcess) {
      sendTargetLaunchError('target process was not created');
      sendNoChild();
      stop('target-launch-error');
      closeIfRetired();
      return;
    }
    targetSpawned = true;
    const targetPID = targetProcess.pid;
    if (targetPID !== undefined) {
      sendSupervisor({ type: 'target-process-started', processID: targetPID, spawnParentPID: process.pid, groupID, anchorPID });
      targetOutputRelayed = true;
    } else {
      sendSupervisor({ type: 'target-process-unconfirmed', processID: null, spawnParentPID: process.pid, groupID, anchorPID });
    }
    if (targetPID !== undefined) {
      relayTargetOutput(targetProcess.stdout!, stdout, 'stdout');
      relayTargetOutput(targetProcess.stderr!, stderr, 'stderr');
      const targetInput = targetProcess.stdin!;
      targetInput.on('error', error => {
        const failure = error instanceof Error ? error.message : 'owned target input pipe failed';
        input.unpipe(targetInput);
        input.resume();
        stop((error as NodeJS.ErrnoException).code === 'EPIPE' ? 'target-input-closed' : 'target-input-failed', failure);
      });
      input.on('error', error => stop('caller-input-failed', error.message));
      input.pipe(targetInput);
      inputObservation.connected(targetInput);
    } else {
      targetProcess.stdout?.resume();
      targetProcess.stderr?.resume();
    }
    targetProcess.once('spawn', () => confirmTargetSpawn(targetProcess));
    targetProcess.once('error', error => {
      if (!targetExecConfirmed) {
        sendTargetLaunchError(error.message);
        stop('target-launch-error', error.message);
      } else stop('target-process-error', error.message);
    });
    targetProcess.once('exit', (code, signal) => {
      const value = { code, signal };
      if (targetExecConfirmed) deliverTargetExit(value);
      else pendingExit = value;
    });
    targetProcess.once('close', (code, signal) => {
      pendingClose = { code, signal };
      if (targetPID !== undefined) input.unpipe(targetProcess.stdin!);
      if (!targetExecConfirmed && !targetLaunchErrorSent) {
        const failure = 'target process closed before successful spawn was confirmed';
        sendTargetLaunchError(failure, pendingClose);
        stop('target-spawn-unproved', failure);
      }
      if (targetClosed) return;
      deliverTargetClose(pendingClose);
    });
    if (managerGroupRestoreFailed) {
      stop('anchor-manager-group-restore-failed', 'anchor manager could not leave the target process group');
    }
    if (targetPID === undefined) stop('target-launch-error');
  };

  control.on('error', () => {
    if (!closing) abortOutputRelay('supervisor control channel failed');
    stop('supervisor-control-failed');
    closeIfRetired();
  });
  control.on('end', () => {
    if (closing) return;
    abortOutputRelay('supervisor control channel was lost');
    if (anchorClosed && (!targetSpawned || targetClosed)) closeIfRetired();
    else stop('supervisor-control-lost');
  });
  parseLines(control, message => {
    if (message.type === 'manager-output-abort') {
      if (typeof message.reason !== 'string') throw new Error('invalid supervisor output-relay abort request');
      abortOutputRelay(message.reason);
      stop('supervisor-output-relay-aborted', message.reason);
      closeIfRetired();
      return;
    }
    if (message.type === 'manager-output-received' || message.type === 'manager-output-retired') {
      const stream = message.stream;
      if (stream !== 'stdout' && stream !== 'stderr') throw new Error('output-relay acknowledgement named an unknown stream');
      const state = outputStates[stream];
      if (!Number.isSafeInteger(message.bytes) || Number(message.bytes) < 0 || Number(message.bytes) !== state.bytes) {
        throw new Error(`output-relay acknowledgement byte count did not match ${stream}`);
      }
      if (message.type === 'manager-output-received') {
        if (!state.finished || state.acknowledged || state.descriptorClosed || outputRelayFailed) {
          throw new Error(`output-relay byte acknowledgement was duplicate or premature for ${stream}`);
        }
        state.acknowledged = true;
        closeOutputDescriptor(stream);
        return;
      }
      if (!state.acknowledged || !(stream === 'stdout' ? stdout : stderr).destroyed || state.finalAcknowledged
        || message.naturalEnd !== true || message.closeObserved !== true) {
        throw new Error(`output-relay final acknowledgement was duplicate or unproved for ${stream}`);
      }
      state.finalAcknowledged = true;
      closeIfRetired();
      return;
    }
    if (message.type === 'configure-anchor' || message.type === 'stop' || message.type === 'kill-group') {
      sendAnchor(message);
      return;
    }
    if (message.type === 'launch-target') {
      launchTarget(message);
      return;
    }
    if (message.type === 'release') {
      if (anchorClosed && (!targetSpawned || targetClosed)) closeIfRetired();
      return;
    }
    stop('invalid-manager-command', 'invalid anchor manager request');
  }, error => stop('manager-control-handler-failed', error.message));

  try {
    anchor = spawn('bun', ['--no-env-file', '--experimental-strip-types', runtime, '--anchor', startupHardDeadlineNs,
      String(process.pid), String(supervisorPID)], {
      detached: false,
      stdio: ['ignore', 'ignore', 'ignore', 'pipe'], cwd: process.cwd(), env: process.env,
    });
  } catch (error) {
    sendSupervisor({ type: 'anchor-bootstrap-error', message: error instanceof Error ? error.message : 'anchor could not be launched' });
    anchorClosed = true;
    closeIfRetired();
    return;
  }
  if (anchor.pid === undefined) {
    sendSupervisor({ type: 'anchor-bootstrap-error', message: 'anchor did not receive a process ID' });
    anchorClosed = true;
    closeIfRetired();
    return;
  }
  const childControl = controlStream(anchor);
  anchorControl = childControl;
  sendSupervisor({ type: 'anchor-process-started', processID: anchor.pid, spawnParentPID: process.pid,
    sessionID: supervisorPID, startupDeadlineNs: startupHardDeadlineNs });
  for (const message of pendingAnchorMessages.splice(0)) send(childControl, message);
  childControl.on('error', error => stop('anchor-control-failed', error.message));
  parseLines(childControl, message => {
    if (message.type === 'anchor-stopping') {
      stopping = true;
      observeInputStop();
      if (!targetSpawned) input.resume();
      sendSupervisor(message);
      return;
    }
    if (message.type === 'anchor-native-ready') {
      anchorPID = Number(message.anchorPID);
      groupID = Number(message.groupID);
    }
    if (message.type === 'anchor-ready') anchorReady = true;
    sendSupervisor(message);
  }, error => stop('anchor-control-handler-failed', error.message));
  anchor.once('error', error => {
    sendSupervisor({ type: 'anchor-bootstrap-error', message: error.message });
    stop('anchor-launch-error', error.message);
  });
  anchor.once('exit', (code, signal) => sendSupervisor({ type: 'anchor-exit', code, signal }));
  anchor.once('close', (code, signal) => {
    anchorClosed = true;
    anchorControl?.destroy();
    anchorControl = undefined;
    sendSupervisor({ type: 'anchor-close', code, signal });
    closeIfRetired();
  });
  input.on('error', error => stop('caller-input-failed', error.message));
}

function runSupervisor(): void {
  const startupExecutionDeadlineNs = process.argv[3] || '';
  const startupHardDeadlineNs = process.argv[4] || '';
  const observer = supervisorObserver(9);
  observer.record({ type: 'supervisor-runtime-started', pid: process.pid, clockNs: nowNs().toString(), observerSink: observer.sink });
  const control = supervisorControlChannel(3);
  const lease = reader(0);
  const stdout = writer(4);
  const stderr = writer(5);
  const input = reader(6);
  const observeStdout = writer(7);
  const observeStderr = writer(8);
  let executionDeadlineNs = '';
  let hardDeadlineNs = '';
  let config: LaunchRequest | undefined;
  let anchor: ChildProcess | undefined;
  let anchorControl: Duplex | undefined;
  let anchorManagerPID: number | undefined;
  let anchorExecutionDeadlineNs = '';
  let anchorPID: number | undefined;
  let guardianPID: number | undefined;
  let groupID: number | undefined;
  let anchorLaunchFailed = false;
  let anchorClosed = false;
  let anchorExited = false;
  let anchorManagerClosed = false;
  let targetExecConfirmed = false;
  let terminalTargetCallbackObserved = false;
  let targetLaunchRequested = false;
  let targetProcessCreated = false;
  let targetNoChildObserved = false;
  let targetProcessPID: number | undefined;
  let targetExit = false;
  let targetClose = false;
  let targetUnconfirmedClose = false;
  let targetStdoutEnded = false;
  let targetStdoutClosed = false;
  let targetStderrEnded = false;
  let targetStderrClosed = false;
  let managerStdoutEnded = false;
  let managerStdoutClosed = false;
  let managerStderrEnded = false;
  let managerStderrClosed = false;
  const managerOutputStates = {
    stdout: { received: 0, expected: undefined as number | undefined, receivedAckSent: false, finalAckSent: false, relayFailed: false, sourceEnded: false, sourceClosed: false },
    stderr: { received: 0, expected: undefined as number | undefined, receivedAckSent: false, finalAckSent: false, relayFailed: false, sourceEnded: false, sourceClosed: false },
  };
  let supervisorStdoutFinished = false;
  let supervisorStdoutClosed = false;
  let supervisorStderrFinished = false;
  let supervisorStderrClosed = false;
  let targetOutputRelayHealthy = true;
  let retirementOutputEndsRequested = false;
  let inputClosed = false;
  let groupAbsent = false;
  let stopReason: string | undefined;
  let firstFailure: string | undefined;
  let releaseRequested = false;
  let leaseLost = false;
  let retirementSent = false;
  let unprovedSent = false;
  let supervisorEnded = false;
  let targetSpawned = false;
  let executionTimer: AbsoluteDeadlineTimer | undefined;
  let hardTimer: AbsoluteDeadlineTimer | undefined;
  let controlBroken = false;

  const sendControl = (value: RecordValue) => {
    if (!controlBroken) {
      try { control.write(encode(value)); } catch { controlBroken = true; }
    }
  };
  const recordObserver = (value: RecordValue) => observer.record(value);
  const inputObservation = observeInputPipe(input, (event, summary) => recordObserver({ type: 'supervisor-input-observation', event, summary }));
  let managerInputSummary: RecordValue | undefined;
  const evidence = () => {
    const targetOutputSource = targetProcessCreated && targetProcessPID !== undefined;
    const managerOutputSource = anchorManagerPID !== undefined;
    const stdoutNaturalEnd = targetOutputSource ? targetStdoutEnded
      : managerOutputSource ? managerStdoutEnded : supervisorStdoutFinished;
    const stdoutCloseObserved = targetOutputSource ? targetStdoutClosed
      : managerOutputSource ? managerStdoutClosed : supervisorStdoutClosed;
    const stderrNaturalEnd = targetOutputSource ? targetStderrEnded
      : managerOutputSource ? managerStderrEnded : supervisorStderrFinished;
    const stderrCloseObserved = targetOutputSource ? targetStderrClosed
      : managerOutputSource ? managerStderrClosed : supervisorStderrClosed;
    return {
      targetExitObserved: targetExit, targetCloseObserved: targetClose,
      targetPIDObserved: targetProcessPID !== undefined,
      anchorExitObserved: anchorExited, anchorCloseObserved: anchorClosed, groupAbsent,
      inputClosedObserved: inputClosed,
      stdoutNaturalEnd, stdoutCloseObserved, stderrNaturalEnd, stderrCloseObserved,
      targetStdoutNaturalEnd: targetStdoutEnded, targetStdoutCloseObserved: targetStdoutClosed,
      targetStderrNaturalEnd: targetStderrEnded, targetStderrCloseObserved: targetStderrClosed,
      managerStdoutNaturalEnd: managerStdoutEnded, managerStdoutCloseObserved: managerStdoutClosed,
      managerStderrNaturalEnd: managerStderrEnded, managerStderrCloseObserved: managerStderrClosed,
      supervisorStdoutFinished, supervisorStdoutCloseObserved: supervisorStdoutClosed,
      supervisorStderrFinished, supervisorStderrCloseObserved: supervisorStderrClosed,
      callerStdoutNaturalEnd: false, callerStdoutCloseObserved: false,
      callerStderrNaturalEnd: false, callerStderrCloseObserved: false,
      managerProcessCreated: anchorManagerPID !== undefined,
      targetDispatchRequested: targetLaunchRequested, targetProcessCreated, targetNoChildObserved,
      targetExecConfirmed, targetUnconfirmedCloseObserved: targetUnconfirmedClose,
      targetOutputRelayHealthy,
      firstFailure, stopReason,
    };
  };
  const groupExists = (id: number): boolean => {
    try { process.kill(-id, 0); return true; } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === 'ESRCH') return false;
      return true;
    }
  };
  const finalizeIfReleased = (force = false) => {
    if (supervisorEnded || (!force && !retirementSent) || (!force && !releaseRequested && !leaseLost)) return;
    if (force && !retirementSent) {
      process.exitCode = 1;
      firstFailure ||= 'owned process retirement was not proved before the hard deadline';
    }
    supervisorEnded = true;
    groupPollOwner?.finalize();
    recordObserver({ type: 'supervisor-teardown-started', clockNs: nowNs().toString(),
      input: inputObservation.snapshot(), managerInput: managerInputSummary,
      unmetRetirementPrerequisites: unmetRetirementPrerequisites() });
    executionTimer?.cancel();
    hardTimer?.cancel();
    if (force) {
      input.destroy();
      lease.destroy();
      if (anchorControl && !anchorControl.destroyed) anchorControl.destroy();
      control.destroy();
      closeDescriptor(0);
      closeDescriptor(3);
      closeDescriptor(6);
      stdout.destroy();
      stderr.destroy();
      observeStdout.destroy();
      observeStderr.destroy();
      closeDescriptor(4);
      closeDescriptor(5);
      closeDescriptor(7);
      closeDescriptor(8);
    } else {
      control.destroy();
      stdout.end();
      stderr.end();
      observeStdout.end();
      observeStderr.end();
    }
    if (config?.terminalTransport) {
      closeDescriptor(10);
      closeDescriptor(11);
    }
    observer.close();
  };
  const retirementPrerequisites = () => {
    const sourceStreamsRetired = targetStdoutEnded && targetStdoutClosed && targetStderrEnded && targetStderrClosed;
    const targetSettled = targetLaunchRequested
      ? targetNoChildObserved || (targetProcessCreated && (targetExecConfirmed
        ? targetExit && targetClose && sourceStreamsRetired
        : targetUnconfirmedClose && (!targetSpawned || sourceStreamsRetired)))
      : !targetProcessCreated && !targetNoChildObserved;
    const terminalCallbackSettled = !config?.terminalTransport || !targetExecConfirmed || terminalTargetCallbackObserved;
    const managerRelayRetired = anchorManagerPID === undefined
      || (managerStdoutEnded && managerStdoutClosed && managerStderrEnded && managerStderrClosed
        && managerOutputStates.stdout.finalAckSent && managerOutputStates.stderr.finalAckSent
        && managerOutputStates.stdout.received === managerOutputStates.stdout.expected
        && managerOutputStates.stderr.received === managerOutputStates.stderr.expected
        && !managerOutputStates.stdout.relayFailed && !managerOutputStates.stderr.relayFailed);
    return { anchorClosed, anchorManagerClosed, groupAbsent, inputClosed, targetSettled, terminalCallbackSettled, managerRelayRetired,
      stdoutRetired: supervisorStdoutClosed && (supervisorStdoutFinished || !targetOutputRelayHealthy),
      stderrRetired: supervisorStderrClosed && (supervisorStderrFinished || !targetOutputRelayHealthy) };
  };
  const unmetRetirementPrerequisites = () => Object.entries(retirementPrerequisites()).filter(([, met]) => !met).map(([name]) => name);
  const notifyRetirement = () => {
    const prerequisites = retirementPrerequisites();
    if (!prerequisites.anchorClosed || !prerequisites.anchorManagerClosed || !prerequisites.groupAbsent || !prerequisites.inputClosed
      || !prerequisites.targetSettled || !prerequisites.terminalCallbackSettled || !prerequisites.managerRelayRetired || retirementSent) return;
    if (!retirementOutputEndsRequested) {
      retirementOutputEndsRequested = true;
      stdout.end();
      stderr.end();
      observeStdout.end();
      observeStderr.end();
    }
    const stdoutRetired = supervisorStdoutClosed && (supervisorStdoutFinished || !targetOutputRelayHealthy);
    const stderrRetired = supervisorStderrClosed && (supervisorStderrFinished || !targetOutputRelayHealthy);
    if (!stdoutRetired || !stderrRetired) return;
    recordObserver({ type: 'supervisor-retirement-ready', evidence: evidence() });
    retirementSent = true;
    sendControl({ type: 'retirement-ready', evidence: evidence() });
    finalizeIfReleased();
  };
  const groupPollOwner = createOwnedGroupPollOwner({
    schedule: (callback, delayMs) => setTimeout(callback, delayMs),
    cancel: handle => clearTimeout(handle),
    probe: () => groupID !== undefined && groupExists(groupID),
    observe: (present, _phase, isCurrent) => {
      if (!isCurrent() || supervisorEnded) return;
      groupAbsent = !present;
      if (present) return;
      recordObserver({ type: 'supervisor-group-absent', groupID });
      if (!isCurrent() || supervisorEnded) return;
      notifyRetirement();
    },
  });
  const inspectGroup = (phase: GroupPollPhase) => {
    if (groupID === undefined || !anchorClosed) return true;
    return groupPollOwner?.inspect(phase) ?? false;
  };
  const outputFailure = (stream: 'stdout' | 'stderr', message: string) => {
    targetOutputRelayHealthy = false;
    firstFailure ||= message;
    stop('caller-output-failed', `${stream}: ${firstFailure}`);
  };
  stdout.once('finish', () => { supervisorStdoutFinished = true; notifyRetirement(); });
  stdout.once('close', () => {
    supervisorStdoutClosed = true;
    if (!supervisorStdoutFinished) outputFailure('stdout', 'caller-facing stdout closed before writer finish');
    notifyRetirement();
  });
  stdout.once('error', error => outputFailure('stdout', error instanceof Error ? error.message : 'caller-facing stdout failed'));
  stderr.once('finish', () => { supervisorStderrFinished = true; notifyRetirement(); });
  stderr.once('close', () => {
    supervisorStderrClosed = true;
    if (!supervisorStderrFinished) outputFailure('stderr', 'caller-facing stderr closed before writer finish');
    notifyRetirement();
  });
  stderr.once('error', error => outputFailure('stderr', error instanceof Error ? error.message : 'caller-facing stderr failed'));
  const managerControlWritable = () => Boolean(anchor && anchorControl && !anchorManagerClosed
    && anchor.exitCode === null && anchor.signalCode === null
    && !anchorControl.destroyed && anchorControl.writable && !anchorControl.writableEnded);
  const stop = (reason: string, failure?: string) => {
    if (supervisorEnded) return;
    recordObserver({ type: 'supervisor-stopping', reason, failure, anchorClosed, anchorManagerClosed, groupAbsent,
      inputClosed, targetSpawned, targetProcessPID, targetExecConfirmed, targetExit, targetClose, targetStdoutEnded, targetStdoutClosed,
      targetStderrEnded, targetStderrClosed, clockNs: nowNs().toString(), input: inputObservation.snapshot(), managerInput: managerInputSummary,
      unmetRetirementPrerequisites: unmetRetirementPrerequisites() });
    const previousStopReason = stopReason;
    if (!stopReason) stopReason = reason;
    if (!firstFailure && failure) firstFailure = failure;
    sendControl({ type: 'stopping', reason: stopReason, failure: firstFailure });
    if (!targetSpawned && !inputClosed) input.resume();
    const executionExpired = /^\d+$/u.test(executionDeadlineNs) && nowNs() >= BigInt(executionDeadlineNs);
    const anchorOwnsTimeout = reason === 'timeout' || previousStopReason === 'timeout' || executionExpired;
    if (!previousStopReason && !anchorOwnsTimeout && managerControlWritable()) send(anchorControl!, { type: 'stop', reason: stopReason });
    else if (!previousStopReason && !anchorOwnsTimeout && anchor && !anchorManagerClosed
      && anchor.exitCode === null && anchor.signalCode === null) {
      try {
        if (!anchor.kill('SIGTERM')) firstFailure ||= 'owned anchor manager rejected SIGTERM';
      } catch (error) {
        firstFailure ||= error instanceof Error ? error.message : 'owned anchor manager could not be signaled';
      }
    } else if (!previousStopReason && !anchorOwnsTimeout && groupID !== undefined) {
      firstFailure ||= 'owned process group stop refused because its live anchor control is unavailable';
      recordObserver({ type: 'supervisor-group-signal-refused', signal: 'SIGTERM', groupID, anchorPID, anchorClosed, anchorManagerClosed });
    }
    if (!anchor && !retirementSent) {
      groupAbsent = true;
      anchorClosed = true;
      anchorExited = true;
      anchorManagerClosed = true;
      if (inputClosed && !targetSpawned) notifyRetirement();
    }
  };
  const failManagerOutputRelay = (reason: string) => {
    targetOutputRelayHealthy = false;
    firstFailure ||= reason;
    managerOutputStates.stdout.relayFailed = true;
    managerOutputStates.stderr.relayFailed = true;
    recordObserver({ type: 'supervisor-manager-output-protocol-error', reason });
    if (managerControlWritable()) send(anchorControl!, { type: 'manager-output-abort', reason });
    stop('manager-output-protocol-invalid', reason);
  };
  const acknowledgeReceivedManagerOutput = (stream: 'stdout' | 'stderr') => {
    const state = managerOutputStates[stream];
    if (state.relayFailed || state.expected === undefined || state.receivedAckSent) return;
    if (state.received > state.expected) {
      failManagerOutputRelay(`${stream} manager relay exceeded its advertised byte count`);
      return;
    }
    if (state.received !== state.expected) return;
    if (state.sourceEnded || state.sourceClosed) {
      failManagerOutputRelay(`${stream} manager relay reached EOF before its byte acknowledgement`);
      return;
    }
    state.receivedAckSent = true;
    send(anchorControl!, { type: 'manager-output-received', stream, bytes: state.expected });
  };
  const receiveManagerOutputFinished = (message: RecordValue) => {
    const stream = message.stream;
    if ((stream !== 'stdout' && stream !== 'stderr') || anchorManagerPID === undefined
      || !Number.isSafeInteger(message.bytes) || Number(message.bytes) < 0) {
      failManagerOutputRelay('manager output completion named an unknown stream or invalid byte count');
      return;
    }
    const state = managerOutputStates[stream];
    if (state.expected !== undefined || state.sourceEnded || state.sourceClosed || state.relayFailed) {
      failManagerOutputRelay(`duplicate or late ${stream} manager output completion`);
      return;
    }
    state.expected = Number(message.bytes);
    acknowledgeReceivedManagerOutput(stream);
  };
  const markLeaseLost = () => {
    if (leaseLost) return;
    leaseLost = true;
    releaseRequested = true;
    stop('lease-lost');
    finalizeIfReleased();
  };
  const relay = (source: NodeJS.ReadableStream, destination: Writable, observer: Writable | undefined,
    streamName: 'stdout' | 'stderr', onEnd: () => void, onClose: () => void) => {
    let destinationBroken = false;
    let observerBroken = false;
    let destinationBlocked = false;
    let observerBlocked = false;
    let sourceEnded = false;
    const resume = () => {
      if (!destinationBlocked && !observerBlocked) source.resume();
    };
    const breakDestination = (error?: NodeJS.ErrnoException) => {
      if (destinationBroken) return;
      destinationBroken = true;
      destinationBlocked = false;
      if (error?.code === 'EPIPE') recordObserver({ type: 'downstream-epipe', stream: streamName });
      targetOutputRelayHealthy = false;
      const message = error instanceof Error ? error.message : `${streamName} caller-facing relay closed early`;
      firstFailure ||= message;
      stop('caller-output-failed', `${streamName}: ${firstFailure}`);
      resume();
    };
    const breakObserver = () => { observerBroken = true; observerBlocked = false; resume(); };
    destination.on('error', breakDestination);
    destination.once('close', () => {
      if (!destination.writableFinished) breakDestination();
    });
    observer?.on('error', breakObserver);
    observer?.on('close', breakObserver);
    source.on('data', (chunk: Buffer) => {
      const outputState = managerOutputStates[streamName];
      const nextReceived = outputState.received + chunk.byteLength;
      if (!Number.isSafeInteger(nextReceived)) {
        failManagerOutputRelay(`${streamName} manager relay byte count exceeded its safe bound`);
        return;
      }
      outputState.received = nextReceived;
      acknowledgeReceivedManagerOutput(streamName);
      if (!destinationBroken && !destination.write(chunk)) {
        destinationBlocked = true;
        source.pause();
        destination.once('drain', () => { destinationBlocked = false; resume(); });
      }
      if (observer && !observerBroken && !observer.write(chunk)) {
        observerBlocked = true;
        source.pause();
        observer.once('drain', () => { observerBlocked = false; resume(); });
      }
    });
    source.once('end', () => {
      sourceEnded = true;
      const outputState = managerOutputStates[streamName];
      outputState.sourceEnded = true;
      recordObserver({ type: 'supervisor-manager-relay-end', stream: streamName });
      if (outputState.expected === undefined || outputState.received !== outputState.expected || !outputState.receivedAckSent) {
        failManagerOutputRelay(`${streamName} manager relay ended without an exact byte handoff`);
      }
      onEnd();
      destination.end();
      observer?.end();
    });
    source.once('close', () => {
      const outputState = managerOutputStates[streamName];
      outputState.sourceClosed = true;
      recordObserver({ type: 'supervisor-manager-relay-close', stream: streamName, naturalEnd: sourceEnded });
      if (!sourceEnded || outputState.expected === undefined || outputState.received !== outputState.expected
        || !outputState.receivedAckSent || outputState.relayFailed) {
        failManagerOutputRelay(`${streamName} manager relay closed without a proven byte handoff and natural EOF`);
      } else if (!outputState.finalAckSent) {
        outputState.finalAckSent = true;
        send(anchorControl!, { type: 'manager-output-retired', stream: streamName, bytes: outputState.expected,
          naturalEnd: true, closeObserved: true });
      }
      onClose();
      notifyRetirement();
    });
    source.on('error', error => {
      const outputState = managerOutputStates[streamName];
      outputState.relayFailed = true;
      failManagerOutputRelay(error instanceof Error ? error.message : 'manager relay stream failed');
      stop('manager-relay-error', firstFailure);
    });
  };
  const receiveTargetSourceEvent = (message: RecordValue) => {
    const stream = message.stream;
    if (!targetLaunchRequested || !targetProcessCreated || targetNoChildObserved
      || (stream !== 'stdout' && stream !== 'stderr')) {
      targetOutputRelayHealthy = false;
      stop('target-source-protocol-invalid', 'target source event did not match a created target and known stream');
      return;
    }
    const ended = stream === 'stdout' ? targetStdoutEnded : targetStderrEnded;
    const closed = stream === 'stdout' ? targetStdoutClosed : targetStderrClosed;
    if (message.type === 'target-source-end') {
      if (ended || closed) {
        targetOutputRelayHealthy = false;
        stop('target-source-protocol-invalid', `duplicate or late ${stream} source end`);
        return;
      }
      if (stream === 'stdout') targetStdoutEnded = true;
      else targetStderrEnded = true;
      recordObserver({ type: 'supervisor-target-source-end', stream });
      notifyRetirement();
      return;
    }
    if (closed || message.naturalEnd !== ended) {
      targetOutputRelayHealthy = false;
      stop('target-source-protocol-invalid', `invalid or duplicate ${stream} source close`);
      return;
    }
    if (stream === 'stdout') targetStdoutClosed = true;
    else targetStderrClosed = true;
    if (!ended) {
      targetOutputRelayHealthy = false;
      stop('target-source-closed-before-end', `${stream} target source closed without natural EOF`);
    }
    recordObserver({ type: 'supervisor-target-source-close', stream, naturalEnd: ended });
    notifyRetirement();
  };
  const startAnchor = () => {
    if (!config || anchor || leaseLost || stopReason) return;
    sendControl({ type: 'supervisor-progress', stage: 'anchor-launch-requested' });
    if (nowNs() >= BigInt(config.executionDeadlineNs)) { stop('timeout'); return; }
    const cleanupAllowanceNs = BigInt(config.hardDeadlineNs) - BigInt(config.executionDeadlineNs);
    const startupReserveNs = cleanupAllowanceNs > 0n
      ? cleanupAllowanceNs / 2n < 500_000_000n ? cleanupAllowanceNs / 2n : 500_000_000n
      : 0n;
    const anchorStartupDeadlineNs = (BigInt(config.hardDeadlineNs) - startupReserveNs).toString();
    try {
      anchor = spawn('bun', ['--no-env-file', '--experimental-strip-types', fileURLToPath(import.meta.url), '--anchor-manager',
        anchorStartupDeadlineNs, String(process.pid), ...(config.terminalTransport ? ['--terminal-transport'] : [])], {
        detached: false,
        stdio: config.terminalTransport
          ? ['ignore', 'ignore', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe', 10, 11]
          : ['ignore', 'ignore', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe'],
        cwd: process.cwd(), env: process.env,
      });
    } catch (error) {
      stop('anchor-launch-error', error instanceof Error ? error.message : 'anchor manager could not be launched');
      return;
    } finally {
      if (config.terminalTransport) {
        closeDescriptor(10);
        closeDescriptor(11);
      }
    }
    if (anchor.pid === undefined) {
      anchorLaunchFailed = true;
      stop('anchor-launch-error', 'anchor manager did not receive a process ID');
      return;
    }
    anchorManagerPID = anchor.pid;
    const managerControl = controlStream(anchor);
    anchorControl = managerControl;
    const anchorInput = writableStream(anchor, 6);
    anchorInput.on('error', error => stop('caller-input-failed', error.message));
    input.on('error', error => stop('caller-input-failed', error.message));
    input.pipe(anchorInput);
    inputObservation.connected(anchorInput);
    relay(readableStream(anchor, 4), stdout, observeStdout, 'stdout',
      () => { managerStdoutEnded = true; notifyRetirement(); },
      () => { managerStdoutClosed = true; notifyRetirement(); });
    relay(readableStream(anchor, 5), stderr, observeStderr, 'stderr',
      () => { managerStderrEnded = true; notifyRetirement(); },
      () => { managerStderrClosed = true; notifyRetirement(); });
    managerControl.on('error', () => {
      if (anchorManagerClosed || anchor?.exitCode !== null || anchor?.signalCode !== null) return;
      failManagerOutputRelay('owned anchor manager control channel failed');
    });
    parseLines(managerControl, message => {
      if (message.type === 'manager-input-observation') {
        if (typeof message.event !== 'string' || !['connected', 'sourceEnd', 'sourceClose', 'sourceError',
          'destinationFinish', 'destinationClose', 'destinationError', 'stop', 'teardown'].includes(message.event)) return;
        managerInputSummary = sanitizedInputPipeSummary(message.summary);
        recordObserver({ type: 'supervisor-manager-input-observation', event: message.event, summary: managerInputSummary });
        return;
      }
      if (message.type === 'terminal-transport-unavailable') {
        if (!config?.terminalTransport || targetLaunchRequested || targetProcessCreated) {
          stop('terminal-transport-protocol-invalid', 'manager reported an unavailable terminal channel outside startup');
          return;
        }
        anchorLaunchFailed = true;
        const failure = 'terminal authority descriptors were absent, closed, or not connected sockets';
        firstFailure ||= failure;
        recordObserver({ type: 'supervisor-terminal-transport-unavailable' });
        sendControl({ type: 'failure', reason: 'terminal-transport-unavailable', message: firstFailure });
        stop('terminal-transport-unavailable', firstFailure);
        return;
      }
      if (message.type === 'manager-output-finished') {
        receiveManagerOutputFinished(message);
        return;
      }
      if (message.type === 'manager-teardown-started') recordObserver({ type: 'supervisor-manager-teardown-started', anchorClosed,
        targetSpawned, targetClose, message });
      if (message.type === 'anchor-process-started') {
        const values = [message.processID, message.spawnParentPID, message.sessionID];
        const processID = Number(message.processID);
        if (!Number.isSafeInteger(processID) || processID <= 0 || Number(message.spawnParentPID) !== anchorManagerPID
          || Number(message.sessionID) !== process.pid || values.some(value => !Number.isSafeInteger(value))) {
          stop('anchor-admission-failed', 'anchor process identity did not match its actual manager and supervisor');
          return;
        }
        anchorPID = processID;
        recordObserver({ type: 'anchor-process-started', processID, spawnParentPID: anchorManagerPID,
          startupDeadlineNs: anchorStartupDeadlineNs });
        sendControl(message);
        return;
      }
      if (message.type === 'anchor-native-ready') {
        const clock = typeof message.clockNs === 'string' && /^\d+$/u.test(message.clockNs) ? BigInt(message.clockNs) : undefined;
        const candidateGuardianPID = Number(message.guardianPID);
        const identities = [message.anchorPID, message.spawnParentPID, message.groupID, message.sessionID,
          message.guardianPID, message.guardianParentPID, message.guardianGroupID, message.guardianSessionID];
        if (clock === undefined || Number(message.anchorPID) !== anchorPID || Number(message.spawnParentPID) !== anchorManagerPID
          || Number(message.groupID) !== anchorPID || Number(message.sessionID) !== process.pid
          || !Number.isSafeInteger(candidateGuardianPID) || candidateGuardianPID <= 0 || candidateGuardianPID === anchorPID
          || Number(message.guardianParentPID) !== anchorPID || Number(message.guardianGroupID) !== anchorPID
          || Number(message.guardianSessionID) !== process.pid || identities.some(value => !Number.isSafeInteger(value))) {
          stop('anchor-admission-failed', 'anchor or guardian identity and clock did not match their owned process group');
          return;
        }
        guardianPID = candidateGuardianPID;
        groupID = Number(message.groupID);
        sendControl(message);
        const supervisorNow = nowNs();
        recordObserver({ type: 'anchor-clock-handshake', anchorClockNs: clock.toString(), supervisorClockNs: supervisorNow.toString() });
        const executionRemaining = BigInt(config!.executionDeadlineNs) - supervisorNow - 5_000_000n;
        const cleanupAllowanceNs = BigInt(config!.hardDeadlineNs) - BigInt(config!.executionDeadlineNs);
        const reserveNs = cleanupAllowanceNs > 0n
          ? cleanupAllowanceNs / 2n < 500_000_000n ? cleanupAllowanceNs / 2n : 500_000_000n
          : 0n;
        const anchorHardDeadline = BigInt(config!.hardDeadlineNs) - reserveNs;
        const hardRemaining = anchorHardDeadline - supervisorNow - 5_000_000n;
        if (hardRemaining <= 0n || executionRemaining <= 0n) {
          stop('timeout');
          return;
        }
        anchorExecutionDeadlineNs = (clock + executionRemaining).toString();
        send(anchorControl!, { type: 'configure-anchor', executionDeadlineNs: anchorExecutionDeadlineNs,
          hardDeadlineNs: (clock + hardRemaining).toString(), killDelayMs: config!.killDelayMs });
        return;
      }
      if (message.type === 'anchor-ready') {
        const values = [message.anchorPID, message.spawnParentPID, message.groupID, message.sessionID,
          message.guardianPID, message.guardianParentPID, message.guardianGroupID, message.guardianSessionID];
        if (Number(message.anchorPID) !== anchorPID || Number(message.spawnParentPID) !== anchorManagerPID
          || Number(message.groupID) !== anchorPID || Number(message.sessionID) !== process.pid
          || Number(message.guardianPID) !== guardianPID || !Number.isSafeInteger(guardianPID)
          || guardianPID === anchorPID || Number(message.guardianParentPID) !== anchorPID
          || Number(message.guardianGroupID) !== anchorPID || Number(message.guardianSessionID) !== process.pid
          || values.some(value => !Number.isSafeInteger(value))) {
          stop('anchor-admission-failed', 'anchor or guardian identity changed before readiness');
          return;
        }
        sendControl(message);
        if (leaseLost || stopReason || nowNs() >= BigInt(config!.executionDeadlineNs)) {
          stop(stopReason || 'timeout');
          return;
        }
        targetLaunchRequested = true;
        send(anchorControl!, { type: 'launch-target', binary: config!.binary, args: config!.args,
          executionDeadlineNs: anchorExecutionDeadlineNs,
          ...(config!.terminalTarget ? { terminalTarget: config!.terminalTarget } : {}) });
        return;
      }
      if (message.type === 'anchor-exit') {
        anchorExited = true;
        recordObserver({ type: 'supervisor-anchor-exit', code: message.code, signal: message.signal });
      }
      if (message.type === 'anchor-close') {
        anchorClosed = true;
        recordObserver({ type: 'supervisor-anchor-close', code: message.code, signal: message.signal, groupID });
        if (groupID === undefined) groupAbsent = true;
        const currentGroupInspection = inspectGroup(anchorManagerClosed ? 'after-manager-close' : 'before-manager-close');
        if (currentGroupInspection && !supervisorEnded) notifyRetirement();
      }
      if (message.type === 'target-process-started' || message.type === 'target-process-unconfirmed') {
        const values = [message.spawnParentPID, message.groupID, message.anchorPID];
        const knownPID = message.type === 'target-process-started'
          ? Number.isSafeInteger(message.processID) && Number(message.processID) > 0
          : message.processID === null;
        if (!targetLaunchRequested || targetProcessCreated || targetNoChildObserved || !knownPID
          || Number(message.spawnParentPID) !== anchorManagerPID || Number(message.groupID) !== groupID
          || Number(message.anchorPID) !== anchorPID || values.some(value => !Number.isSafeInteger(value))) {
          stop('target-admission-failed', 'target process creation did not match one requested child and its actual manager and group');
          return;
        }
        targetProcessCreated = true;
        if (message.type === 'target-process-started') {
          targetSpawned = true;
          targetProcessPID = Number(message.processID);
          recordObserver({ type: 'supervisor-target-process-started', targetPID: targetProcessPID,
            spawnParentPID: anchorManagerPID, groupID, anchorPID });
        } else recordObserver({ type: 'supervisor-target-process-unconfirmed', groupID, anchorPID });
      }
      if (message.type === 'target-no-child') {
        if (!targetLaunchRequested || targetProcessCreated || targetNoChildObserved || message.launchRequestReceived !== true
          || Number(message.spawnParentPID) !== anchorManagerPID || Number(message.groupID) !== groupID
          || Number(message.anchorPID) !== anchorPID) {
          stop('target-admission-failed', 'manager no-child outcome did not match an outstanding launch request');
          return;
        }
        targetNoChildObserved = true;
        recordObserver({ type: 'supervisor-target-no-child', groupID, anchorPID });
      }
      if (message.type === 'target-source-end' || message.type === 'target-source-close') {
        receiveTargetSourceEvent(message);
        return;
      }
      if (message.type === 'target-started') {
        const values = [message.targetPID, message.spawnParentPID, message.groupID, message.anchorPID];
        if (!targetProcessCreated || targetExecConfirmed || !targetSpawned || Number(message.targetPID) !== targetProcessPID
          || !Number.isSafeInteger(message.targetPID) || Number(message.targetPID) <= 0
          || Number(message.spawnParentPID) !== anchorManagerPID || Number(message.groupID) !== groupID
          || Number(message.anchorPID) !== anchorPID || values.some(value => !Number.isSafeInteger(value))
          || (config?.terminalTransport ? message.callback !== 'spawn' || terminalTargetCallbackObserved : message.callback !== undefined)) {
          stop('target-admission-failed', 'target exec identity did not match its actual manager, PID, group, and callback');
          return;
        }
        targetExecConfirmed = true;
        recordObserver({ type: 'supervisor-target-started', targetPID: targetProcessPID,
          spawnParentPID: anchorManagerPID, groupID, anchorPID });
        if (config?.terminalTransport) {
          terminalTargetCallbackObserved = true;
          recordObserver({ type: 'supervisor-terminal-target-callback', targetPID: targetProcessPID,
            spawnParentPID: anchorManagerPID, groupID, anchorPID });
        }
      }
      if (message.type === 'target-exit') {
        if (!targetExecConfirmed || targetExit) {
          stop('target-lifecycle-invalid', 'target exit was missing exec confirmation or duplicated');
          return;
        }
        targetExit = true;
        recordObserver({ type: 'supervisor-target-exit', code: message.code, signal: message.signal });
      }
      if (message.type === 'target-close') {
        if (!targetExecConfirmed || targetClose) {
          stop('target-lifecycle-invalid', 'target close was missing exec confirmation or uniqueness');
          return;
        }
        targetClose = true;
        recordObserver({ type: 'supervisor-target-close', code: message.code, signal: message.signal });
      }
      if (message.type === 'target-unconfirmed-close') {
        if (!targetProcessCreated || targetExecConfirmed || targetUnconfirmedClose) {
          stop('target-lifecycle-invalid', 'unconfirmed target close did not match one unconfirmed child or uniqueness');
          return;
        }
        targetUnconfirmedClose = true;
      }
      if (message.type === 'failure') {
        firstFailure ||= typeof message.message === 'string' ? message.message : 'owned target process failed';
        if (message.reason === 'target-output-failed' || message.reason === 'target-stream-error'
          || message.reason === 'manager-output-closed' || message.reason === 'manager-output-failed'
          || message.reason === 'caller-output-failed' || message.reason === 'manager-relay-closed-early' || message.reason === 'manager-relay-error') {
          targetOutputRelayHealthy = false;
        }
      }
      if (message.type === 'anchor-stopping' || message.type === 'stopping') {
        const reason = typeof message.reason === 'string' ? message.reason : 'anchor-stopping';
        if (!stopReason) stopReason = reason;
        if (message.type === 'anchor-stopping') sendControl({ type: 'stopping', reason });
      }
      if (message.type === 'anchor-bootstrap-error') {
        anchorLaunchFailed = true;
        const failure = typeof message.message === 'string' ? message.message : 'anchor initialization failed';
        stop('anchor-launch-error', failure);
      } else if (message.type === 'anchor-kill-error') {
        const failure = typeof message.message === 'string' ? message.message : 'anchor could not retire its process group';
        stop('anchor-retirement-failed', failure);
      } else if (message.type === 'target-launch-error') {
        firstFailure ||= typeof message.message === 'string' ? message.message : 'target launch failed';
        sendControl(message);
      } else if (message.type === 'target-started' || message.type === 'target-exit' || message.type === 'target-close'
        || message.type === 'target-unconfirmed-close' || message.type === 'failure' || message.type === 'stopping'
        || message.type === 'anchor-exit' || message.type === 'anchor-close') {
        if (message.type === 'target-unconfirmed-close') {
          sendControl({ type: 'target-close', code: message.code, signal: message.signal,
            ...(typeof message.launchError === 'string' ? { launchError: message.launchError } : {}) });
        } else sendControl(message);
      }
      if (message.type === 'stopping' || message.type === 'failure' || message.type === 'target-input-error') {
        stop(typeof message.reason === 'string' ? message.reason : 'owned-process-failure',
          typeof message.message === 'string' ? message.message : undefined);
      }
      notifyRetirement();
    }, error => failManagerOutputRelay(`anchor manager control protocol failed: ${error.message}`));
    anchor.once('error', error => stop('anchor-manager-launch-error', error.message));
    anchor.once('close', (code, signal) => {
      anchorManagerClosed = true;
      recordObserver({ type: 'supervisor-manager-close', code, signal, anchorClosed, groupAbsent, targetSpawned, targetExit,
        targetClose, targetStdoutEnded, targetStdoutClosed, targetStderrEnded, targetStderrClosed, inputClosed });
      anchorControl?.destroy();
      if (anchorPID === undefined && anchorLaunchFailed) {
        anchorExited = true;
        anchorClosed = true;
        groupAbsent = true;
      }
      const currentGroupInspection = inspectGroup('after-manager-close');
      if (currentGroupInspection && !supervisorEnded) notifyRetirement();
    });
  };

  const scheduleDeadlines = (executionDeadline: string, hardDeadline: string) => {
    executionDeadlineNs = executionDeadline;
    hardDeadlineNs = hardDeadline;
    executionTimer?.cancel();
    hardTimer?.cancel();
    executionTimer = scheduleAt(executionDeadlineNs, () => {
      recordObserver({ type: 'supervisor-execution-deadline', clockNs: nowNs().toString() });
      stop('timeout');
    });
    hardTimer = scheduleAt(hardDeadlineNs, () => {
      recordObserver({ type: 'supervisor-hard-deadline', clockNs: nowNs().toString(), retirementSent, inputClosed,
        targetSpawned, anchorClosed, groupAbsent });
      releaseRequested = true;
      if (managerControlWritable()) send(anchorControl!, { type: 'manager-output-abort', reason: 'hard-deadline' });
      stop('hard-deadline');
      if (managerControlWritable() && !anchorClosed) send(anchorControl!, { type: 'kill-group', reason: 'hard-deadline' });
      else if (groupID !== undefined) {
        firstFailure ||= 'owned process group kill refused because its live anchor control is unavailable';
        recordObserver({ type: 'supervisor-group-signal-refused', signal: 'SIGKILL', groupID, anchorPID, anchorClosed, anchorManagerClosed });
      }
      if (!retirementSent && !unprovedSent) {
        unprovedSent = true;
        recordObserver({ type: 'supervisor-retirement-unproved', evidence: evidence(), input: inputObservation.snapshot(),
          managerInput: managerInputSummary, unmetRetirementPrerequisites: unmetRetirementPrerequisites() });
        sendControl({ type: 'retirement-unproved', evidence: evidence() });
      }
      if (anchor && !anchorManagerClosed && anchor.exitCode === null && anchor.signalCode === null) {
        recordObserver({ type: 'supervisor-manager-forced-kill', managerPID: anchor.pid });
        try {
          if (!anchor.kill('SIGKILL')) firstFailure ||= 'owned anchor manager could not be killed at its hard deadline';
        } catch (error) {
          firstFailure ||= error instanceof Error ? error.message : 'owned anchor manager could not be killed at its hard deadline';
        }
      }
      finalizeIfReleased(true);
    });
  };
  const armDeadlines = () => {
    if (!/^\d+$/u.test(startupExecutionDeadlineNs) || !/^\d+$/u.test(startupHardDeadlineNs)) {
      stop('invalid-supervisor-deadlines', 'supervisor did not receive original monotonic deadlines');
      return;
    }
    const executionDeadline = BigInt(startupExecutionDeadlineNs);
    const hardDeadline = BigInt(startupHardDeadlineNs);
    if (executionDeadline >= hardDeadline) {
      stop('invalid-supervisor-deadlines', 'supervisor startup deadlines were inconsistent');
      return;
    }
    scheduleDeadlines(startupExecutionDeadlineNs, startupHardDeadlineNs);
  };

  lease.on('end', markLeaseLost);
  lease.on('error', markLeaseLost);
  control.on('error', () => { controlBroken = true; stop('supervisor-control-failed'); });
  control.on('end', () => { controlBroken = true; stop('supervisor-control-lost'); });
  input.once('close', () => { inputClosed = true; recordObserver({ type: 'supervisor-input-close' }); notifyRetirement(); });
  input.once('end', () => { inputClosed = true; recordObserver({ type: 'supervisor-input-end' }); notifyRetirement(); });
  lease.resume();
  armDeadlines();
  recordObserver({ type: 'supervisor-deadlines-armed', executionDeadlineNs: startupExecutionDeadlineNs,
    hardDeadlineNs: startupHardDeadlineNs, clockNs: nowNs().toString() });

  parseLines(control, (message) => {
    if (message.type === 'configure') {
      if (config || typeof message.binary !== 'string' || !Array.isArray(message.args)
        || message.args.some(value => typeof value !== 'string')
        || message.args.some(value => value.includes('\0')) || message.binary.includes('\0')
        || typeof message.executionDeadlineNs !== 'string' || typeof message.hardDeadlineNs !== 'string'
        || typeof message.killDelayMs !== 'number'
        || (message.terminalTransport !== undefined && message.terminalTransport !== true)
        || (Object.hasOwn(message, 'terminalTarget')
          && (message.terminalTransport !== true || !terminalTargetConfig(message.terminalTarget)))) {
        stop('invalid-launch-request', 'invalid owned process launch configuration');
        return;
      }
      const terminalTarget = Object.hasOwn(message, 'terminalTarget') ? terminalTargetConfig(message.terminalTarget) : undefined;
      config = { ...message, ...(terminalTarget ? { terminalTarget } : {}) } as unknown as LaunchRequest;
      const configuredExecution = BigInt(config.executionDeadlineNs);
      const configuredHard = BigInt(config.hardDeadlineNs);
      const startupExecutionDeadline = BigInt(startupExecutionDeadlineNs);
      const startupHardDeadline = BigInt(startupHardDeadlineNs);
      if (configuredExecution > startupExecutionDeadline || configuredHard > startupHardDeadline
        || configuredExecution >= configuredHard || configuredHard <= nowNs()) {
        stop('invalid-launch-request', 'translated launch deadlines extended or outlived the finite startup allowance');
        return;
      }
      scheduleDeadlines(config.executionDeadlineNs, config.hardDeadlineNs);
      sendControl({ type: 'ready' });
      if (leaseLost) stop('lease-lost');
    } else if (message.type === 'start') startAnchor();
    else if (message.type === 'stop') stop(typeof message.reason === 'string' ? message.reason : 'stop-request');
    else if (message.type === 'release') {
      releaseRequested = true;
      if (!anchor && !retirementSent) stop('released-before-dispatch');
      finalizeIfReleased();
    }
  }, error => stop('supervisor-control-handler-failed', error.message));
  sendControl({ type: 'runtime-clock', clockNs: nowNs().toString() });
}

const mode = process.argv[2];
if (mode === '--anchor') void runAnchor();
else if (mode === '--anchor-guardian') void runAnchorGuardian();
else if (mode === '--anchor-manager') runAnchorManager();
else if (mode === '--supervisor') runSupervisor();

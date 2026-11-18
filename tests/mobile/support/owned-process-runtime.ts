import { closeSync, createReadStream, createWriteStream, fstatSync, writeSync } from 'node:fs';
import { spawn, type ChildProcess } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { Socket } from 'node:net';
import { constants as osConstants } from 'node:os';
import { fileURLToPath } from 'node:url';
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

export function releaseOwnedInputOnFinish(stream: Writable): void {
  const cleanup = () => {
    stream.off('finish', finish);
    stream.off('error', cleanup);
    stream.off('close', cleanup);
  };
  const finish = () => {
    cleanup();
    if (stream.writableFinished && !stream.errored && !stream.destroyed && !stream.closed) stream.destroy();
  };
  if (stream.destroyed || stream.closed || stream.errored) return;
  if (stream.writableFinished) { finish(); return; }
  stream.once('finish', finish);
  stream.once('error', cleanup);
  stream.once('close', cleanup);
}

function writableStream(child: ChildProcess, index: number): Writable {
  const value = child.stdio[index];
  if (!value || !('write' in value)) throw new Error(`owned process stream ${index} was not created`);
  return value as Writable;
}

function encode(value: RecordValue): string {
  return `${JSON.stringify(value)}\n`;
}

interface LineSource extends EventEmitter {
  setEncoding(encoding: BufferEncoding): unknown;
  destroy(error?: Error): unknown;
}

function parseLines(stream: LineSource, receive: (value: RecordValue) => void,
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

interface GuardianState {
  control: ControlChannel;
  startupHardDeadlineNs: bigint;
  expectedAnchorPID: number;
  expectedSession: number;
  native: NativeRuntime | undefined;
  groupID: number | undefined;
  groupEstablished: boolean;
  deadlinesArmed: boolean;
  stopping: boolean;
  startupExpired: boolean;
  executionDeadlineNs: string;
  hardDeadlineNs: string;
  killDelayMs: number;
  groupKillTimer: ReturnType<typeof setTimeout> | undefined;
  startupTimer: AbsoluteDeadlineTimer | undefined;
  executionTimer: AbsoluteDeadlineTimer | undefined;
  hardTimer: AbsoluteDeadlineTimer | undefined;
}

interface GuardianIdentity {
  guardianPID: number;
  guardianParentPID: number;
  sessionID: number;
}

function createGuardianState(): GuardianState {
  const startupDeadlineValue = process.argv[3] || '';
  return {
    control: controlChannel(3),
    startupHardDeadlineNs: /^\d+$/u.test(startupDeadlineValue) ? BigInt(startupDeadlineValue) : 0n,
    expectedAnchorPID: Number(process.argv[4]),
    expectedSession: Number(process.argv[5]),
    native: undefined,
    groupID: undefined,
    groupEstablished: false,
    deadlinesArmed: false,
    stopping: false,
    startupExpired: false,
    executionDeadlineNs: '',
    hardDeadlineNs: '',
    killDelayMs: 250,
    groupKillTimer: undefined,
    startupTimer: undefined,
    executionTimer: undefined,
    hardTimer: undefined,
  };
}

function guardianSendControl(state: GuardianState, value: RecordValue) {
  send(state.control, value);
}

function guardianClearDeadlines(state: GuardianState) {
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  state.hardTimer?.cancel();
}

function guardianKillGroup(state: GuardianState, signal: NodeJS.Signals): boolean {
  if (!state.groupEstablished) return false;
  const failure = signalOwnedProcessGroup(state.native, state.groupID, state.expectedSession, signal);
  if (failure) guardianSendControl(state, { type: 'guardian-group-signal-refused', signal, groupID: state.groupID, guardianPID: process.pid, reason: failure });
  return !failure;
}

function guardianScheduleGroupKill(state: GuardianState) {
  const hardRemaining = Math.max(0, Number(((state.hardDeadlineNs ? BigInt(state.hardDeadlineNs) : state.startupHardDeadlineNs) - nowNs()) / 1_000_000n));
  const delay = Math.min(state.killDelayMs, hardRemaining);
  if (state.groupKillTimer) clearTimeout(state.groupKillTimer);
  state.groupKillTimer = setTimeout(() => guardianKillGroup(state, 'SIGKILL'), delay);
}

function guardianStopGroup(state: GuardianState, reason: string, killNow = false) {
  if (!state.groupEstablished) {
    state.startupExpired = true;
    guardianClearDeadlines(state);
    guardianSendControl(state, { type: 'guardian-stopping', reason });
    state.control.destroy();
    process.exitCode = 1;
    return;
  }
  if (state.stopping && !killNow) return;
  state.stopping = true;
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  guardianSendControl(state, { type: 'guardian-stopping', reason });
  if (killNow) {
    state.hardTimer?.cancel();
    if (state.groupKillTimer) clearTimeout(state.groupKillTimer);
    guardianKillGroup(state, 'SIGKILL');
    return;
  }
  if (!guardianKillGroup(state, 'SIGTERM')) return;
  guardianScheduleGroupKill(state);
}

function guardianOnTermination(state: GuardianState) {
  if (!state.groupEstablished) return guardianStopGroup(state, 'anchor-termination');
  if (state.stopping) return;
  state.stopping = true;
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  guardianSendControl(state, { type: 'guardian-stopping', reason: 'group-termination' });
  guardianScheduleGroupKill(state);
}

function guardianWatchStartup(state: GuardianState) {
  state.control.on('error', () => guardianStopGroup(state, 'anchor-control-failed'));
  state.control.on('end', () => guardianStopGroup(state, 'anchor-control-lost'));
  state.startupTimer = scheduleAt(state.startupHardDeadlineNs.toString(), () => {
    if (state.groupEstablished) guardianStopGroup(state, 'guardian-startup-hard-deadline', true);
    else {
      state.startupExpired = true;
      guardianClearDeadlines(state);
      guardianSendControl(state, { type: 'guardian-stopping', reason: 'guardian-startup-hard-deadline' });
      state.control.destroy();
      process.exitCode = 1;
    }
  });
}

function guardianVerifyIdentity(state: GuardianState, native: NativeRuntime): GuardianIdentity | undefined {
  const { symbols } = native;
  const guardianPID = symbols.getpid();
  const guardianParentPID = symbols.getppid();
  state.groupID = symbols.getpgid(0);
  const sessionID = symbols.getsid(0);
  if (guardianPID <= 0 || state.expectedAnchorPID <= 0 || guardianParentPID !== state.expectedAnchorPID
    || state.groupID !== state.expectedAnchorPID || sessionID !== state.expectedSession
    || symbols.getpgid(state.expectedAnchorPID) !== state.expectedAnchorPID || symbols.getsid(state.expectedAnchorPID) !== state.expectedSession) {
    state.startupTimer?.cancel();
    guardianSendControl(state, { type: 'guardian-bootstrap-error', message: 'guardian parent, process-group, or session identity did not match its anchor' });
    state.control.destroy();
    process.exitCode = 1;
    return undefined;
  }
  return { guardianPID, guardianParentPID, sessionID };
}

function guardianConfigure(state: GuardianState, identity: GuardianIdentity, message: RecordValue) {
  if (state.stopping || state.startupExpired) {
    guardianSendControl(state, { type: 'guardian-configuration-refused', reason: 'guardian was already stopping before configuration' });
    return;
  }
  if (message.type !== 'configure-guardian' || state.deadlinesArmed || typeof message.executionDeadlineNs !== 'string'
    || !/^\d+$/u.test(message.executionDeadlineNs) || typeof message.hardDeadlineNs !== 'string'
    || !/^\d+$/u.test(message.hardDeadlineNs) || typeof message.killDelayMs !== 'number'
    || !Number.isSafeInteger(message.killDelayMs) || message.killDelayMs < 0) {
    guardianStopGroup(state, 'invalid-guardian-deadlines');
    return;
  }
  state.executionDeadlineNs = message.executionDeadlineNs;
  state.hardDeadlineNs = message.hardDeadlineNs;
  state.killDelayMs = message.killDelayMs;
  if (BigInt(state.executionDeadlineNs) >= BigInt(state.hardDeadlineNs) || BigInt(state.hardDeadlineNs) > state.startupHardDeadlineNs
    || nowNs() >= BigInt(state.hardDeadlineNs)) {
    guardianStopGroup(state, 'invalid-guardian-deadlines');
    return;
  }
  state.deadlinesArmed = true;
  state.startupTimer?.cancel();
  state.executionTimer = armGroupDeadline(state.executionDeadlineNs, () => guardianStopGroup(state, 'timeout'));
  state.hardTimer = armGroupDeadline(state.hardDeadlineNs, () => guardianStopGroup(state, 'hard-deadline', true));
  guardianSendControl(state, { type: 'guardian-ready', ...identity, groupID: state.groupID,
    executionDeadlineNs: state.executionDeadlineNs, hardDeadlineNs: state.hardDeadlineNs, killDelayMs: state.killDelayMs });
}

function guardianAdmit(state: GuardianState, identity: GuardianIdentity) {
  state.groupEstablished = true;
  const onTermination = () => guardianOnTermination(state);
  process.on('SIGTERM', onTermination);
  process.on('SIGHUP', onTermination);
  process.on('SIGINT', onTermination);
  if (state.startupExpired || nowNs() >= state.startupHardDeadlineNs) {
    guardianStopGroup(state, 'guardian-startup-hard-deadline', true);
    return;
  }
  guardianSendControl(state, { type: 'guardian-native-ready', ...identity, groupID: state.groupID, clockNs: nowNs().toString() });
  parseLines(state.control, message => {
    if (message.type === 'stop' || message.type === 'kill-group') {
      guardianStopGroup(state, typeof message.reason === 'string' ? message.reason : 'anchor-request', message.type === 'kill-group');
      return;
    }
    guardianConfigure(state, identity, message);
  });
}

function guardianFailBootstrap(state: GuardianState, error: unknown) {
  guardianSendControl(state, { type: 'guardian-bootstrap-error', message: error instanceof Error ? error.message.slice(0, 1_000) : 'guardian initialization failed' });
  guardianClearDeadlines(state);
  if (state.groupEstablished) guardianStopGroup(state, 'guardian-bootstrap-error');
  else {
    state.control.destroy();
    process.exitCode = 1;
  }
}

async function runAnchorGuardian(): Promise<void> {
  const state = createGuardianState();
  guardianWatchStartup(state);
  try {
    state.native = await loadNativeRuntime();
    const identity = guardianVerifyIdentity(state, state.native);
    if (!identity) return;
    guardianAdmit(state, identity);
  } catch (error) {
    guardianFailBootstrap(state, error);
  }
}

interface AnchorState {
  control: ControlChannel;
  startupHardDeadlineNs: bigint;
  expectedParent: number;
  expectedSession: number;
  native: NativeRuntime | undefined;
  anchorPID: number | undefined;
  spawnParentPID: number | undefined;
  groupID: number | undefined;
  sessionID: number | undefined;
  guardian: ChildProcess | undefined;
  guardianControl: Duplex | undefined;
  guardianPID: number | undefined;
  guardianNativeReady: boolean;
  guardianConfigured: boolean;
  guardianClosed: boolean;
  resolveGuardianStartup: () => void;
  rejectGuardianStartup: (error: Error) => void;
  pendingAnchorReady: RecordValue | undefined;
  groupEstablished: boolean;
  deadlinesArmed: boolean;
  stopping: boolean;
  startupExpired: boolean;
  executionDeadlineNs: string;
  hardDeadlineNs: string;
  killDelayMs: number;
  groupKillTimer: ReturnType<typeof setTimeout> | undefined;
  startupTimer: AbsoluteDeadlineTimer | undefined;
  executionTimer: AbsoluteDeadlineTimer | undefined;
  hardTimer: AbsoluteDeadlineTimer | undefined;
}

function createAnchorState(): AnchorState {
  const startupDeadlineValue = process.argv[3] || '';
  return {
    control: controlChannel(3),
    startupHardDeadlineNs: /^\d+$/u.test(startupDeadlineValue) ? BigInt(startupDeadlineValue) : 0n,
    expectedParent: Number(process.argv[4]),
    expectedSession: Number(process.argv[5]),
    native: undefined,
    anchorPID: undefined,
    spawnParentPID: undefined,
    groupID: undefined,
    sessionID: undefined,
    guardian: undefined,
    guardianControl: undefined,
    guardianPID: undefined,
    guardianNativeReady: false,
    guardianConfigured: false,
    guardianClosed: false,
    resolveGuardianStartup: () => undefined,
    rejectGuardianStartup: () => undefined,
    pendingAnchorReady: undefined,
    groupEstablished: false,
    deadlinesArmed: false,
    stopping: false,
    startupExpired: false,
    executionDeadlineNs: '',
    hardDeadlineNs: '',
    killDelayMs: 250,
    groupKillTimer: undefined,
    startupTimer: undefined,
    executionTimer: undefined,
    hardTimer: undefined,
  };
}

function anchorSendControl(state: AnchorState, value: RecordValue) {
  send(state.control, value);
}

function anchorClearDeadlines(state: AnchorState) {
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  state.hardTimer?.cancel();
}

function anchorKillGroup(state: AnchorState, signal: NodeJS.Signals): boolean {
  if (!state.groupEstablished) return false;
  const failure = signalOwnedProcessGroup(state.native, state.groupID, state.expectedSession, signal);
  if (failure) anchorSendControl(state, { type: 'anchor-group-signal-refused', signal, groupID: state.groupID, anchorPID: state.anchorPID, reason: failure });
  return !failure;
}

function anchorScheduleGroupKill(state: AnchorState) {
  const hardRemaining = Math.max(0, Number(((state.hardDeadlineNs ? BigInt(state.hardDeadlineNs) : state.startupHardDeadlineNs) - nowNs()) / 1_000_000n));
  const delay = Math.min(state.killDelayMs, hardRemaining);
  if (state.groupKillTimer) clearTimeout(state.groupKillTimer);
  state.groupKillTimer = setTimeout(() => anchorKillGroup(state, 'SIGKILL'), delay);
}

function anchorStopGroup(state: AnchorState, reason: string, killNow = false) {
  if (!state.groupEstablished) {
    state.startupExpired = true;
    anchorClearDeadlines(state);
    anchorSendControl(state, { type: 'anchor-stopping', reason });
    state.control.destroy();
    process.exitCode = 1;
    return;
  }
  if (state.stopping && !killNow) return;
  state.stopping = true;
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  anchorSendControl(state, { type: 'anchor-stopping', reason });
  if (killNow) {
    state.hardTimer?.cancel();
    if (state.groupKillTimer) clearTimeout(state.groupKillTimer);
    anchorKillGroup(state, 'SIGKILL');
    return;
  }
  if (!anchorKillGroup(state, 'SIGTERM')) return;
  anchorScheduleGroupKill(state);
}

function anchorOnTermination(state: AnchorState) {
  if (!state.groupEstablished) return anchorStopGroup(state, 'supervisor-request');
  if (state.stopping) return;
  state.stopping = true;
  state.startupTimer?.cancel();
  state.executionTimer?.cancel();
  anchorSendControl(state, { type: 'anchor-stopping', reason: 'group-termination' });
  anchorScheduleGroupKill(state);
}

function anchorWatchStartup(state: AnchorState) {
  state.control.on('error', () => anchorStopGroup(state, 'supervisor-control-failed'));
  state.control.on('end', () => anchorStopGroup(state, 'supervisor-control-lost'));
  state.startupTimer = scheduleAt(state.startupHardDeadlineNs.toString(), () => {
    if (state.groupEstablished) anchorStopGroup(state, 'startup-hard-deadline', true);
    else {
      state.startupExpired = true;
      anchorClearDeadlines(state);
      anchorSendControl(state, { type: 'anchor-stopping', reason: 'startup-hard-deadline' });
      state.control.destroy();
      process.exitCode = 1;
    }
  });
}

function anchorEstablishGroup(state: AnchorState, native: NativeRuntime): boolean {
  const { symbols } = native;
  state.anchorPID = symbols.getpid();
  state.spawnParentPID = symbols.getppid();
  if (symbols.setpgid(0, 0) !== 0) {
    anchorSendControl(state, { type: 'anchor-bootstrap-error', message: 'setpgid failed' });
    process.exitCode = 1;
    return false;
  }
  state.groupID = symbols.getpgid(0);
  state.sessionID = symbols.getsid(0);
  if (state.anchorPID <= 0 || state.spawnParentPID !== state.expectedParent || state.groupID !== state.anchorPID || state.sessionID !== state.expectedSession) {
    anchorSendControl(state, { type: 'anchor-bootstrap-error', message: 'anchor process-group or session identity did not match the supervisor' });
    process.exitCode = 1;
    return false;
  }
  state.groupEstablished = true;
  const onTermination = () => anchorOnTermination(state);
  process.on('SIGTERM', onTermination);
  process.on('SIGHUP', onTermination);
  process.on('SIGINT', onTermination);
  if (state.startupExpired || nowNs() >= state.startupHardDeadlineNs) {
    anchorStopGroup(state, 'startup-hard-deadline', true);
    return false;
  }
  return true;
}

function anchorAdmitGuardianNative(state: AnchorState, message: RecordValue) {
  const candidatePID = Number(message.guardianPID);
  const clock = typeof message.clockNs === 'string' && /^\d+$/u.test(message.clockNs) ? BigInt(message.clockNs) : undefined;
  if (state.guardianNativeReady || candidatePID !== state.guardian!.pid || Number(message.guardianParentPID) !== state.anchorPID
    || Number(message.groupID) !== state.groupID || Number(message.sessionID) !== state.sessionID || clock === undefined
    || state.native!.symbols.getpgid(candidatePID) !== state.groupID || state.native!.symbols.getsid(candidatePID) !== state.sessionID) {
    const error = new Error('anchor guardian identity or clock did not match its live group member');
    state.rejectGuardianStartup(error);
    anchorStopGroup(state, 'anchor-guardian-admission-failed', true);
    return;
  }
  state.guardianPID = candidatePID;
  state.guardianNativeReady = true;
  state.resolveGuardianStartup();
}

function anchorAdmitGuardianReady(state: AnchorState, message: RecordValue) {
  if (!state.guardianNativeReady || state.guardianConfigured || state.stopping || state.startupExpired || !state.pendingAnchorReady
    || Number(message.guardianPID) !== state.guardianPID || Number(message.guardianParentPID) !== state.anchorPID
    || Number(message.groupID) !== state.groupID || Number(message.sessionID) !== state.sessionID
    || message.executionDeadlineNs !== state.executionDeadlineNs || message.hardDeadlineNs !== state.hardDeadlineNs
    || message.killDelayMs !== state.killDelayMs || state.native!.symbols.getpgid(state.guardianPID!) !== state.groupID
    || state.native!.symbols.getsid(state.guardianPID!) !== state.sessionID) {
    anchorStopGroup(state, 'anchor-guardian-readiness-invalid', true);
    return;
  }
  state.guardianConfigured = true;
  anchorSendControl(state, state.pendingAnchorReady);
  state.pendingAnchorReady = undefined;
}

function anchorHandleGuardianMessage(state: AnchorState, message: RecordValue) {
  if (message.type === 'guardian-native-ready') {
    anchorAdmitGuardianNative(state, message);
    return;
  }
  if (message.type === 'guardian-ready') {
    anchorAdmitGuardianReady(state, message);
    return;
  }
  if (message.type === 'guardian-configuration-refused') {
    const failure = new Error(String(message.reason || 'anchor guardian was already stopping'));
    anchorSendControl(state, { type: 'anchor-guardian-failure', reason: message.type, message: failure.message });
    anchorStopGroup(state, 'anchor-guardian-configuration-refused', true);
    return;
  }
  if (message.type === 'guardian-bootstrap-error' || message.type === 'guardian-group-signal-refused') {
    const failure = new Error(String(message.message || message.reason || 'anchor guardian failed'));
    if (!state.guardianNativeReady) state.rejectGuardianStartup(failure);
    anchorSendControl(state, { type: 'anchor-guardian-failure', reason: message.type, message: failure.message });
    anchorStopGroup(state, 'anchor-guardian-failed');
    return;
  }
  if (message.type === 'guardian-stopping') {
    state.stopping = true;
    state.startupTimer?.cancel();
    state.executionTimer?.cancel();
    const reason = typeof message.reason === 'string' ? message.reason : 'guardian-stopping';
    anchorSendControl(state, { type: 'anchor-stopping', reason });
    anchorScheduleGroupKill(state);
    return;
  }
  anchorStopGroup(state, 'invalid-anchor-guardian-message', true);
}

function anchorStartGuardian(state: AnchorState): Promise<void> {
  const guardianStartup = new Promise<void>((resolve, reject) => {
    state.resolveGuardianStartup = resolve;
    state.rejectGuardianStartup = reject;
  });
  const guardian = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', fileURLToPath(import.meta.url),
    '--anchor-guardian', state.startupHardDeadlineNs.toString(), String(state.anchorPID), String(state.sessionID)], {
    detached: false,
    stdio: ['ignore', 'ignore', 'ignore', 'pipe'], cwd: process.cwd(), env: process.env,
  });
  state.guardian = guardian;
  if (guardian.pid === undefined) throw new Error('anchor guardian did not receive a process ID');
  const guardianControl = controlStream(guardian);
  state.guardianControl = guardianControl;
  guardianControl.on('error', error => {
    if (!state.guardianNativeReady) state.rejectGuardianStartup(error instanceof Error ? error : new Error('anchor guardian control failed'));
    else anchorStopGroup(state, 'anchor-guardian-control-failed');
  });
  parseLines(guardianControl, message => anchorHandleGuardianMessage(state, message), error => {
    if (!state.guardianNativeReady) state.rejectGuardianStartup(error);
    else anchorStopGroup(state, 'anchor-guardian-protocol-failed');
  });
  guardian.once('error', error => {
    if (!state.guardianNativeReady) state.rejectGuardianStartup(error);
    else anchorStopGroup(state, 'anchor-guardian-launch-failed');
  });
  guardian.once('close', () => {
    state.guardianClosed = true;
    if (!state.guardianNativeReady) state.rejectGuardianStartup(new Error('anchor guardian closed before identity admission'));
    else if (!state.stopping && !state.startupExpired) anchorStopGroup(state, 'anchor-guardian-closed');
  });
  return guardianStartup;
}

function anchorHandleControlMessage(state: AnchorState, guardianIdentity: RecordValue, message: RecordValue) {
  if (message.type === 'stop' || message.type === 'kill-group') {
    anchorStopGroup(state, typeof message.reason === 'string' ? message.reason : 'supervisor-request', message.type === 'kill-group');
    return;
  }
  if (message.type !== 'configure-anchor') {
    anchorStopGroup(state, 'invalid-anchor-configuration');
    return;
  }
  if (state.stopping || state.startupExpired) {
    anchorSendControl(state, { type: 'anchor-configuration-refused', reason: 'anchor was already stopping before configuration' });
    anchorStopGroup(state, 'anchor-configuration-after-stopping', true);
    return;
  }
  if (state.deadlinesArmed || typeof message.executionDeadlineNs !== 'string'
    || typeof message.hardDeadlineNs !== 'string' || typeof message.killDelayMs !== 'number') {
    anchorStopGroup(state, 'invalid-anchor-deadlines');
    return;
  }
  state.executionDeadlineNs = message.executionDeadlineNs;
  state.hardDeadlineNs = message.hardDeadlineNs;
  state.killDelayMs = Number.isSafeInteger(message.killDelayMs) ? Math.max(0, message.killDelayMs) : 50;
  if (nowNs() >= state.startupHardDeadlineNs) {
    anchorStopGroup(state, 'startup-hard-deadline', true);
    return;
  }
  if (nowNs() >= BigInt(state.hardDeadlineNs)) {
    anchorStopGroup(state, 'hard-deadline', true);
    return;
  }
  if (!state.guardianControl || state.guardianClosed) {
    anchorStopGroup(state, 'anchor-guardian-control-unavailable', true);
    return;
  }
  state.deadlinesArmed = true;
  state.startupTimer?.cancel();
  state.executionTimer = armGroupDeadline(state.executionDeadlineNs, () => anchorStopGroup(state, 'timeout'));
  state.hardTimer = armGroupDeadline(state.hardDeadlineNs, () => anchorStopGroup(state, 'hard-deadline', true));
  state.pendingAnchorReady = { type: 'anchor-ready', anchorPID: state.anchorPID, spawnParentPID: state.spawnParentPID,
    groupID: state.groupID, sessionID: state.sessionID, ...guardianIdentity };
  send(state.guardianControl, { type: 'configure-guardian', executionDeadlineNs: state.executionDeadlineNs,
    hardDeadlineNs: state.hardDeadlineNs, killDelayMs: state.killDelayMs });
}

function anchorServeControl(state: AnchorState, guardianPID: number) {
  const guardianIdentity = { guardianPID, guardianParentPID: state.anchorPID, guardianGroupID: state.groupID, guardianSessionID: state.sessionID };
  anchorSendControl(state, { type: 'anchor-native-ready', anchorPID: state.anchorPID, spawnParentPID: state.spawnParentPID,
    groupID: state.groupID, sessionID: state.sessionID, ...guardianIdentity, clockNs: nowNs().toString() });
  parseLines(state.control, message => anchorHandleControlMessage(state, guardianIdentity, message));
}

function anchorFailBootstrap(state: AnchorState, error: unknown) {
  anchorSendControl(state, { type: 'anchor-bootstrap-error', message: error instanceof Error ? error.message.slice(0, 1_000) : 'anchor initialization failed' });
  anchorClearDeadlines(state);
  if (state.groupEstablished) anchorStopGroup(state, 'anchor-bootstrap-error');
  else {
    state.control.destroy();
    process.exitCode = 1;
  }
}

async function runAnchor(): Promise<void> {
  const state = createAnchorState();
  anchorWatchStartup(state);
  try {
    state.native = await loadNativeRuntime();
    if (!anchorEstablishGroup(state, state.native)) return;
    await anchorStartGuardian(state);
    if (state.startupExpired || state.stopping || nowNs() >= state.startupHardDeadlineNs || state.guardianPID === undefined) {
      anchorStopGroup(state, 'anchor-guardian-startup-expired', true);
      return;
    }
    anchorServeControl(state, state.guardianPID);
  } catch (error) {
    anchorFailBootstrap(state, error);
  }
}

type ExitValue = { code: number | null; signal: NodeJS.Signals | null };

interface ManagerOutputState {
  bytes: number;
  finished: boolean;
  acknowledged: boolean;
  descriptorClosed: boolean;
  streamClosed: boolean;
  finalAcknowledged: boolean;
}

interface ManagerState {
  control: ControlChannel;
  stdout: ReturnType<typeof writer>;
  stderr: ReturnType<typeof writer>;
  input: ReturnType<typeof reader>;
  inputObservation: ReturnType<typeof observeInputPipe>;
  startupHardDeadlineNs: string;
  supervisorPID: number;
  anchor: ChildProcess | undefined;
  anchorControl: Duplex | undefined;
  target: ChildProcess | undefined;
  anchorPID: number | undefined;
  groupID: number | undefined;
  anchorReady: boolean;
  anchorClosed: boolean;
  targetSpawned: boolean;
  targetExecConfirmed: boolean;
  targetClosed: boolean;
  targetLaunchErrorSent: boolean;
  pendingExit: ExitValue | undefined;
  pendingClose: ExitValue | undefined;
  targetExecutionDeadlineNs: string;
  firstFailure: string | undefined;
  stopping: boolean;
  closing: boolean;
  targetOutputRelayed: boolean;
  targetOutputRelayHealthy: boolean;
  targetStdoutEnded: boolean;
  targetStdoutClosed: boolean;
  targetStderrEnded: boolean;
  targetStderrClosed: boolean;
  targetNoChildSent: boolean;
  writerEndRequested: boolean;
  stdoutClosed: boolean;
  stderrClosed: boolean;
  outputRelayFailed: boolean;
  outputStates: { stdout: ManagerOutputState; stderr: ManagerOutputState };
  pendingAnchorMessages: RecordValue[];
  inputStopObserved: boolean;
}

function createManagerState(): ManagerState {
  const control = controlChannel(3);
  const stdout = writer(4, false);
  const stderr = writer(5, false);
  const input = reader(6);
  const newOutputState = (): ManagerOutputState => ({ bytes: 0, finished: false, acknowledged: false,
    descriptorClosed: false, streamClosed: false, finalAcknowledged: false });
  return {
    control,
    stdout,
    stderr,
    input,
    inputObservation: observeInputPipe(input, (event, summary) => send(control, { type: 'manager-input-observation', event, summary })),
    startupHardDeadlineNs: process.argv[3] || '',
    supervisorPID: Number(process.argv[4]),
    anchor: undefined,
    anchorControl: undefined,
    target: undefined,
    anchorPID: undefined,
    groupID: undefined,
    anchorReady: false,
    anchorClosed: false,
    targetSpawned: false,
    targetExecConfirmed: false,
    targetClosed: false,
    targetLaunchErrorSent: false,
    pendingExit: undefined,
    pendingClose: undefined,
    targetExecutionDeadlineNs: '',
    firstFailure: undefined,
    stopping: false,
    closing: false,
    targetOutputRelayed: false,
    targetOutputRelayHealthy: true,
    targetStdoutEnded: false,
    targetStdoutClosed: false,
    targetStderrEnded: false,
    targetStderrClosed: false,
    targetNoChildSent: false,
    writerEndRequested: false,
    stdoutClosed: false,
    stderrClosed: false,
    outputRelayFailed: false,
    outputStates: { stdout: newOutputState(), stderr: newOutputState() },
    pendingAnchorMessages: [],
    inputStopObserved: false,
  };
}

function managerSendSupervisor(state: ManagerState, value: RecordValue) {
  send(state.control, value);
}

function managerObserveInputStop(state: ManagerState) {
  if (state.inputStopObserved) return;
  state.inputStopObserved = true;
  managerSendSupervisor(state, { type: 'manager-input-observation', event: 'stop', summary: state.inputObservation.snapshot() });
}

function managerSendAnchor(state: ManagerState, value: RecordValue) {
  if (!state.anchorControl) state.pendingAnchorMessages.push(value);
  else send(state.anchorControl, value);
}

function managerCloseOutputDescriptor(state: ManagerState, stream: 'stdout' | 'stderr') {
  if (state.outputStates[stream].descriptorClosed) return;
  const destination = stream === 'stdout' ? state.stdout : state.stderr;
  if (!destination.destroyed) destination.destroy();
}

function managerAbortOutputRelay(state: ManagerState, reason: string) {
  if (state.outputRelayFailed) return;
  state.outputRelayFailed = true;
  state.targetOutputRelayHealthy = false;
  state.firstFailure ||= reason;
  state.target?.stdout?.unpipe(state.stdout);
  state.target?.stderr?.unpipe(state.stderr);
  state.target?.stdout?.resume();
  state.target?.stderr?.resume();
  managerCloseOutputDescriptor(state, 'stdout');
  managerCloseOutputDescriptor(state, 'stderr');
}

function managerStop(state: ManagerState, reason: string, failure?: string, stream?: 'stdout' | 'stderr') {
  if (failure) {
    state.firstFailure ||= failure;
    managerSendSupervisor(state, { type: 'failure', reason, stream, message: state.firstFailure });
  }
  state.stopping = true;
  managerObserveInputStop(state);
  if (!state.targetSpawned) state.input.resume();
  managerSendAnchor(state, { type: 'stop', reason });
}

function managerOutputRetired(state: ManagerState, stream: 'stdout' | 'stderr') {
  const output = state.outputStates[stream];
  return output.streamClosed && output.descriptorClosed && (state.outputRelayFailed || (output.finished && output.finalAcknowledged));
}

function managerCloseIfRetired(state: ManagerState) {
  if (state.closing || !state.anchorClosed || (state.targetSpawned && !state.targetClosed)) return;
  if (!state.targetOutputRelayed && !state.writerEndRequested && !state.outputRelayFailed) {
    state.writerEndRequested = true;
    state.stdout.end();
    state.stderr.end();
  }
  if ((!state.outputStates.stdout.finished && !state.stdoutClosed) || (!state.outputStates.stderr.finished && !state.stderrClosed)) return;
  if (state.targetOutputRelayed && (!state.targetStdoutEnded || !state.targetStdoutClosed || !state.targetStderrEnded || !state.targetStderrClosed)) return;
  if (!managerOutputRetired(state, 'stdout') || !managerOutputRetired(state, 'stderr')) return;
  state.closing = true;
  managerSendSupervisor(state, { type: 'manager-input-observation', event: 'teardown', summary: state.inputObservation.snapshot() });
  managerSendSupervisor(state, { type: 'manager-teardown-started', anchorClosed: state.anchorClosed, targetSpawned: state.targetSpawned, targetClosed: state.targetClosed });
  state.input.destroy();
  state.anchorControl?.destroy();
  state.control.destroy();
}

function managerOutputFailure(state: ManagerState, stream: 'stdout' | 'stderr', error: unknown) {
  if (state.outputRelayFailed) return;
  const message = error instanceof Error ? error.message : `owned manager ${stream} output failed`;
  managerAbortOutputRelay(state, message);
  managerStop(state, 'manager-output-failed', message, stream);
  managerCloseIfRetired(state);
}

function managerOutputFinished(state: ManagerState, stream: 'stdout' | 'stderr') {
  const output = state.outputStates[stream];
  output.finished = true;
  if (!state.outputRelayFailed) managerSendSupervisor(state, { type: 'manager-output-finished', stream, bytes: output.bytes });
  managerCloseIfRetired(state);
}

function managerSendTargetLaunchError(state: ManagerState, message: string, value?: ExitValue) {
  state.firstFailure ||= message;
  if (state.targetLaunchErrorSent) return;
  state.targetLaunchErrorSent = true;
  const observedExit = value ?? state.pendingExit ?? (state.target && (state.target.exitCode !== null || state.target.signalCode !== null)
    ? { code: state.target.exitCode, signal: state.target.signalCode } : undefined);
  managerSendSupervisor(state, { type: 'target-launch-error', message,
    code: observedExit?.code ?? null, signal: observedExit?.signal ?? null });
}

function managerDeliverTargetExit(state: ManagerState, value: ExitValue) {
  if (!state.targetExecConfirmed) return;
  managerSendSupervisor(state, { type: 'target-exit', code: value.code, signal: value.signal });
}

function managerDeliverTargetClose(state: ManagerState, value: ExitValue) {
  state.targetClosed = true;
  if (state.targetExecConfirmed) managerSendSupervisor(state, { type: 'target-close', code: value.code, signal: value.signal });
  else managerSendSupervisor(state, { type: 'target-unconfirmed-close', code: value.code, signal: value.signal,
    ...(state.targetLaunchErrorSent && state.firstFailure ? { launchError: state.firstFailure } : {}) });
  managerCloseIfRetired(state);
}

function managerConfirmTargetSpawn(state: ManagerState, child: ChildProcess) {
  if (state.targetExecConfirmed || child.pid === undefined) return;
  state.targetExecConfirmed = true;
  managerSendSupervisor(state, { type: 'target-started', targetPID: child.pid, spawnParentPID: process.pid,
    groupID: state.groupID, anchorPID: state.anchorPID });
  if (state.pendingExit) managerDeliverTargetExit(state, state.pendingExit);
  if (state.pendingClose) managerDeliverTargetClose(state, state.pendingClose);
}

function managerSendNoChild(state: ManagerState) {
  if (state.target || state.targetNoChildSent) return;
  state.targetNoChildSent = true;
  managerSendSupervisor(state, { type: 'target-no-child', launchRequestReceived: true, spawnParentPID: process.pid, groupID: state.groupID, anchorPID: state.anchorPID });
}

function managerRelayTargetOutput(state: ManagerState, source: NodeJS.ReadableStream, destination: ReturnType<typeof writer>, stream: 'stdout' | 'stderr') {
  let sourceEnded = false;
  const failRelay = (reason: string, message: string) => {
    if (!state.targetOutputRelayHealthy) return;
    state.targetOutputRelayHealthy = false;
    source.unpipe(destination);
    source.resume();
    managerAbortOutputRelay(state, message);
    managerStop(state, reason, message, stream);
  };
  source.on('data', chunk => {
    const bytes = Buffer.isBuffer(chunk) ? chunk.byteLength : Buffer.byteLength(String(chunk));
    const output = state.outputStates[stream];
    if (!Number.isSafeInteger(output.bytes + bytes)) {
      failRelay('target-output-failed', `${stream} relay byte count exceeded its safe bound`);
      return;
    }
    output.bytes += bytes;
  });
  source.once('end', () => {
    sourceEnded = true;
    if (stream === 'stdout') state.targetStdoutEnded = true;
    else state.targetStderrEnded = true;
    managerSendSupervisor(state, { type: 'target-source-end', stream });
    managerCloseIfRetired(state);
  });
  source.once('close', () => {
    if (stream === 'stdout') state.targetStdoutClosed = true;
    else state.targetStderrClosed = true;
    managerSendSupervisor(state, { type: 'target-source-close', stream, naturalEnd: sourceEnded });
    managerCloseIfRetired(state);
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
}

function managerWatchOutputs(state: ManagerState) {
  state.stdout.once('finish', () => managerOutputFinished(state, 'stdout'));
  state.stderr.once('finish', () => managerOutputFinished(state, 'stderr'));
  state.stdout.once('error', error => managerOutputFailure(state, 'stdout', error));
  state.stderr.once('error', error => managerOutputFailure(state, 'stderr', error));
  state.stdout.once('close', () => {
    state.stdoutClosed = true;
    state.outputStates.stdout.streamClosed = true;
    state.outputStates.stdout.descriptorClosed = true;
    if ((!state.outputStates.stdout.finished || !state.outputStates.stdout.acknowledged) && !state.outputRelayFailed) {
      managerOutputFailure(state, 'stdout', new Error('owned manager stdout closed before its byte acknowledgement'));
    }
    managerCloseIfRetired(state);
  });
  state.stderr.once('close', () => {
    state.stderrClosed = true;
    state.outputStates.stderr.streamClosed = true;
    state.outputStates.stderr.descriptorClosed = true;
    if ((!state.outputStates.stderr.finished || !state.outputStates.stderr.acknowledged) && !state.outputRelayFailed) {
      managerOutputFailure(state, 'stderr', new Error('owned manager stderr closed before its byte acknowledgement'));
    }
    managerCloseIfRetired(state);
  });
}

function managerSpawnInGroup(state: ManagerState, native: NativeRuntime, binary: string, args: string[]): boolean {
  if (native.symbols.setpgid(0, state.groupID!) !== 0) throw new Error('anchor manager could not enter the target process group');
  let spawnFailure: unknown;
  try {
    state.target = spawn(binary, args, {
      detached: false,
      stdio: ['pipe', 'pipe', 'pipe'],
      cwd: process.cwd(),
      env: process.env,
    });
  } catch (error) {
    spawnFailure = error;
  }
  const restoreFailed = native.symbols.setpgid(0, state.supervisorPID) !== 0;
  if (spawnFailure) throw spawnFailure;
  return restoreFailed;
}

// Undefined means the launch failed and was already reported; otherwise whether leaving the target process group failed.
async function managerSpawnTarget(state: ManagerState, binary: string, args: string[]): Promise<boolean | undefined> {
  try {
    const native = await loadNativeRuntime();
    if (state.stopping || nowNs() >= BigInt(state.targetExecutionDeadlineNs)) {
      managerSendNoChild(state);
      managerStop(state, 'timeout');
      return undefined;
    }
    if (native.symbols.getpgid(0) !== state.supervisorPID || native.symbols.getsid(0) !== state.supervisorPID) {
      throw new Error('anchor manager process group or session changed before target launch');
    }
    return managerSpawnInGroup(state, native, binary, args);
  } catch (error) {
    managerSendTargetLaunchError(state, error instanceof Error ? error.message : 'target process could not be spawned');
    managerSendNoChild(state);
    managerStop(state, 'target-launch-error');
    managerCloseIfRetired(state);
    return undefined;
  }
}

function managerConnectTargetStreams(state: ManagerState, targetProcess: ChildProcess) {
  managerRelayTargetOutput(state, targetProcess.stdout!, state.stdout, 'stdout');
  managerRelayTargetOutput(state, targetProcess.stderr!, state.stderr, 'stderr');
  const targetInput = targetProcess.stdin!;
  targetInput.on('error', error => {
    const failure = error instanceof Error ? error.message : 'owned target input pipe failed';
    state.input.unpipe(targetInput);
    state.input.resume();
    managerStop(state, (error as NodeJS.ErrnoException).code === 'EPIPE' ? 'target-input-closed' : 'target-input-failed', failure);
  });
  state.input.on('error', error => managerStop(state, 'caller-input-failed', error.message));
  state.input.pipe(targetInput);
  state.inputObservation.connected(targetInput);
}

function managerWatchTarget(state: ManagerState, targetProcess: ChildProcess, targetPID: number | undefined) {
  targetProcess.once('spawn', () => managerConfirmTargetSpawn(state, targetProcess));
  targetProcess.once('error', error => {
    if (!state.targetExecConfirmed) {
      managerSendTargetLaunchError(state, error.message);
      managerStop(state, 'target-launch-error', error.message);
    } else managerStop(state, 'target-process-error', error.message);
  });
  targetProcess.once('exit', (code, signal) => {
    const value = { code, signal };
    if (state.targetExecConfirmed) managerDeliverTargetExit(state, value);
    else state.pendingExit = value;
  });
  targetProcess.once('close', (code, signal) => {
    state.pendingClose = { code, signal };
    if (targetPID !== undefined) state.input.unpipe(targetProcess.stdin!);
    if (!state.targetExecConfirmed && !state.targetLaunchErrorSent) {
      const failure = 'target process closed before successful spawn was confirmed';
      managerSendTargetLaunchError(state, failure, state.pendingClose);
      managerStop(state, 'target-spawn-unproved', failure);
    }
    if (state.targetClosed) return;
    managerDeliverTargetClose(state, state.pendingClose);
  });
}

async function managerLaunchTarget(state: ManagerState, message: RecordValue) {
  if (state.targetSpawned || !state.anchorReady || state.groupID === undefined || typeof message.binary !== 'string'
    || !Array.isArray(message.args) || message.args.some(value => typeof value !== 'string')
    || typeof message.executionDeadlineNs !== 'string' || !/^\d+$/u.test(message.executionDeadlineNs)) {
    managerSendTargetLaunchError(state, 'invalid target launch request');
    managerSendNoChild(state);
    managerStop(state, 'invalid-target-launch', 'invalid target launch request from supervisor');
    return;
  }
  state.targetExecutionDeadlineNs = message.executionDeadlineNs;
  if (state.stopping || nowNs() >= BigInt(state.targetExecutionDeadlineNs)) {
    managerSendNoChild(state);
    managerStop(state, 'timeout');
    return;
  }
  const managerGroupRestoreFailed = await managerSpawnTarget(state, message.binary, message.args as string[]);
  if (managerGroupRestoreFailed === undefined) return;
  const targetProcess = state.target;
  if (!targetProcess) {
    managerSendTargetLaunchError(state, 'target process was not created');
    managerSendNoChild(state);
    managerStop(state, 'target-launch-error');
    managerCloseIfRetired(state);
    return;
  }
  state.targetSpawned = true;
  const targetPID = targetProcess.pid;
  if (targetPID !== undefined) {
    managerSendSupervisor(state, { type: 'target-process-started', processID: targetPID, spawnParentPID: process.pid, groupID: state.groupID, anchorPID: state.anchorPID });
    state.targetOutputRelayed = true;
    managerConnectTargetStreams(state, targetProcess);
  } else {
    managerSendSupervisor(state, { type: 'target-process-unconfirmed', processID: null, spawnParentPID: process.pid, groupID: state.groupID, anchorPID: state.anchorPID });
    targetProcess.stdout?.resume();
    targetProcess.stderr?.resume();
  }
  managerWatchTarget(state, targetProcess, targetPID);
  if (managerGroupRestoreFailed) {
    managerStop(state, 'anchor-manager-group-restore-failed', 'anchor manager could not leave the target process group');
  }
  if (targetPID === undefined) managerStop(state, 'target-launch-error');
}

function managerReceiveOutputAcknowledgement(state: ManagerState, message: RecordValue) {
  const stream = message.stream;
  if (stream !== 'stdout' && stream !== 'stderr') throw new Error('output-relay acknowledgement named an unknown stream');
  const output = state.outputStates[stream];
  if (!Number.isSafeInteger(message.bytes) || Number(message.bytes) < 0 || Number(message.bytes) !== output.bytes) {
    throw new Error(`output-relay acknowledgement byte count did not match ${stream}`);
  }
  if (message.type === 'manager-output-received') {
    if (!output.finished || output.acknowledged || output.descriptorClosed || state.outputRelayFailed) {
      throw new Error(`output-relay byte acknowledgement was duplicate or premature for ${stream}`);
    }
    output.acknowledged = true;
    managerCloseOutputDescriptor(state, stream);
    return;
  }
  if (!output.acknowledged || !(stream === 'stdout' ? state.stdout : state.stderr).destroyed || output.finalAcknowledged
    || message.naturalEnd !== true || message.closeObserved !== true) {
    throw new Error(`output-relay final acknowledgement was duplicate or unproved for ${stream}`);
  }
  output.finalAcknowledged = true;
  managerCloseIfRetired(state);
}

function managerHandleSupervisorMessage(state: ManagerState, message: RecordValue) {
  if (message.type === 'manager-output-abort') {
    if (typeof message.reason !== 'string') throw new Error('invalid supervisor output-relay abort request');
    managerAbortOutputRelay(state, message.reason);
    managerStop(state, 'supervisor-output-relay-aborted', message.reason);
    managerCloseIfRetired(state);
    return;
  }
  if (message.type === 'manager-output-received' || message.type === 'manager-output-retired') {
    managerReceiveOutputAcknowledgement(state, message);
    return;
  }
  if (message.type === 'configure-anchor' || message.type === 'stop' || message.type === 'kill-group') {
    managerSendAnchor(state, message);
    return;
  }
  if (message.type === 'launch-target') {
    void managerLaunchTarget(state, message);
    return;
  }
  if (message.type === 'release') {
    if (state.anchorClosed && (!state.targetSpawned || state.targetClosed)) managerCloseIfRetired(state);
    return;
  }
  managerStop(state, 'invalid-manager-command', 'invalid anchor manager request');
}

function managerWatchSupervisor(state: ManagerState) {
  const onSignal = () => managerStop(state, 'anchor-manager-signal');
  process.on('SIGTERM', onSignal);
  process.on('SIGHUP', onSignal);
  process.on('SIGINT', onSignal);
  managerWatchOutputs(state);
  state.control.on('error', () => {
    if (!state.closing) managerAbortOutputRelay(state, 'supervisor control channel failed');
    managerStop(state, 'supervisor-control-failed');
    managerCloseIfRetired(state);
  });
  state.control.on('end', () => {
    if (state.closing) return;
    managerAbortOutputRelay(state, 'supervisor control channel was lost');
    if (state.anchorClosed && (!state.targetSpawned || state.targetClosed)) managerCloseIfRetired(state);
    else managerStop(state, 'supervisor-control-lost');
  });
  parseLines(state.control, message => managerHandleSupervisorMessage(state, message),
    error => managerStop(state, 'manager-control-handler-failed', error.message));
}

function managerSpawnAnchor(state: ManagerState): ChildProcess | undefined {
  try {
    state.anchor = spawn('bun', ['--no-env-file', '--experimental-strip-types', fileURLToPath(import.meta.url), '--anchor',
      state.startupHardDeadlineNs, String(process.pid), String(state.supervisorPID)], {
      detached: false,
      stdio: ['ignore', 'ignore', 'ignore', 'pipe'], cwd: process.cwd(), env: process.env,
    });
  } catch (error) {
    managerSendSupervisor(state, { type: 'anchor-bootstrap-error', message: error instanceof Error ? error.message : 'anchor could not be launched' });
    state.anchorClosed = true;
    managerCloseIfRetired(state);
    return undefined;
  }
  if (state.anchor.pid === undefined) {
    managerSendSupervisor(state, { type: 'anchor-bootstrap-error', message: 'anchor did not receive a process ID' });
    state.anchorClosed = true;
    managerCloseIfRetired(state);
    return undefined;
  }
  return state.anchor;
}

function managerHandleAnchorMessage(state: ManagerState, message: RecordValue) {
  if (message.type === 'anchor-stopping') {
    state.stopping = true;
    managerObserveInputStop(state);
    if (!state.targetSpawned) state.input.resume();
    managerSendSupervisor(state, message);
    return;
  }
  if (message.type === 'anchor-native-ready') {
    state.anchorPID = Number(message.anchorPID);
    state.groupID = Number(message.groupID);
  }
  if (message.type === 'anchor-ready') state.anchorReady = true;
  managerSendSupervisor(state, message);
}

function managerWatchAnchor(state: ManagerState, anchor: ChildProcess) {
  const childControl = controlStream(anchor);
  state.anchorControl = childControl;
  managerSendSupervisor(state, { type: 'anchor-process-started', processID: anchor.pid, spawnParentPID: process.pid,
    sessionID: state.supervisorPID, startupDeadlineNs: state.startupHardDeadlineNs });
  for (const message of state.pendingAnchorMessages.splice(0)) send(childControl, message);
  childControl.on('error', error => managerStop(state, 'anchor-control-failed', error.message));
  parseLines(childControl, message => managerHandleAnchorMessage(state, message),
    error => managerStop(state, 'anchor-control-handler-failed', error.message));
  anchor.once('error', error => {
    managerSendSupervisor(state, { type: 'anchor-bootstrap-error', message: error.message });
    managerStop(state, 'anchor-launch-error', error.message);
  });
  anchor.once('exit', (code, signal) => managerSendSupervisor(state, { type: 'anchor-exit', code, signal }));
  anchor.once('close', (code, signal) => {
    state.anchorClosed = true;
    state.anchorControl?.destroy();
    state.anchorControl = undefined;
    managerSendSupervisor(state, { type: 'anchor-close', code, signal });
    managerCloseIfRetired(state);
  });
  state.input.on('error', error => managerStop(state, 'caller-input-failed', error.message));
}

function runAnchorManager(): void {
  const state = createManagerState();
  managerWatchSupervisor(state);
  const anchor = managerSpawnAnchor(state);
  if (!anchor) return;
  managerWatchAnchor(state, anchor);
}

interface ManagerRelayState {
  received: number;
  expected: number | undefined;
  receivedAckSent: boolean;
  finalAckSent: boolean;
  relayFailed: boolean;
  sourceEnded: boolean;
  sourceClosed: boolean;
}

interface SupervisorState {
  startupExecutionDeadlineNs: string;
  startupHardDeadlineNs: string;
  observer: ReturnType<typeof supervisorObserver>;
  control: ControlChannel;
  lease: ReturnType<typeof reader>;
  stdout: ReturnType<typeof writer>;
  stderr: ReturnType<typeof writer>;
  input: ReturnType<typeof reader>;
  observeStdout: ReturnType<typeof writer>;
  observeStderr: ReturnType<typeof writer>;
  inputObservation: ReturnType<typeof observeInputPipe>;
  groupPollOwner: OwnedGroupPollOwner;
  executionDeadlineNs: string;
  hardDeadlineNs: string;
  config: LaunchRequest | undefined;
  anchor: ChildProcess | undefined;
  anchorControl: Duplex | undefined;
  anchorManagerPID: number | undefined;
  anchorStartupDeadlineNs: string;
  anchorExecutionDeadlineNs: string;
  anchorPID: number | undefined;
  guardianPID: number | undefined;
  groupID: number | undefined;
  anchorLaunchFailed: boolean;
  anchorClosed: boolean;
  anchorExited: boolean;
  anchorManagerClosed: boolean;
  targetExecConfirmed: boolean;
  targetLaunchRequested: boolean;
  targetProcessCreated: boolean;
  targetNoChildObserved: boolean;
  targetProcessPID: number | undefined;
  targetExit: boolean;
  targetClose: boolean;
  targetUnconfirmedClose: boolean;
  targetStdoutEnded: boolean;
  targetStdoutClosed: boolean;
  targetStderrEnded: boolean;
  targetStderrClosed: boolean;
  managerStdoutEnded: boolean;
  managerStdoutClosed: boolean;
  managerStderrEnded: boolean;
  managerStderrClosed: boolean;
  managerOutputStates: { stdout: ManagerRelayState; stderr: ManagerRelayState };
  managerInputSummary: RecordValue | undefined;
  supervisorStdoutFinished: boolean;
  supervisorStdoutClosed: boolean;
  supervisorStderrFinished: boolean;
  supervisorStderrClosed: boolean;
  targetOutputRelayHealthy: boolean;
  retirementOutputEndsRequested: boolean;
  inputClosed: boolean;
  groupAbsent: boolean;
  stopReason: string | undefined;
  firstFailure: string | undefined;
  releaseRequested: boolean;
  leaseLost: boolean;
  retirementSent: boolean;
  unprovedSent: boolean;
  supervisorEnded: boolean;
  targetSpawned: boolean;
  executionTimer: AbsoluteDeadlineTimer | undefined;
  hardTimer: AbsoluteDeadlineTimer | undefined;
  controlBroken: boolean;
}

function createSupervisorState(): SupervisorState {
  const observer = supervisorObserver(9);
  observer.record({ type: 'supervisor-runtime-started', pid: process.pid, clockNs: nowNs().toString(), observerSink: observer.sink });
  const control = supervisorControlChannel(3);
  const lease = reader(0);
  const stdout = writer(4);
  const stderr = writer(5);
  const input = reader(6);
  const observeStdout = writer(7);
  const observeStderr = writer(8);
  const newManagerRelayState = (): ManagerRelayState => ({ received: 0, expected: undefined, receivedAckSent: false,
    finalAckSent: false, relayFailed: false, sourceEnded: false, sourceClosed: false });
  const state: SupervisorState = {
    startupExecutionDeadlineNs: process.argv[3] || '',
    startupHardDeadlineNs: process.argv[4] || '',
    observer,
    control,
    lease,
    stdout,
    stderr,
    input,
    observeStdout,
    observeStderr,
    inputObservation: observeInputPipe(input, (event, summary) => supervisorRecordObserver(state, { type: 'supervisor-input-observation', event, summary })),
    groupPollOwner: createOwnedGroupPollOwner({
      schedule: (callback, delayMs) => setTimeout(callback, delayMs),
      cancel: handle => clearTimeout(handle),
      probe: () => state.groupID !== undefined && supervisorGroupExists(state.groupID),
      observe: (present, _phase, isCurrent) => {
        if (!isCurrent() || state.supervisorEnded) return;
        state.groupAbsent = !present;
        if (present) return;
        supervisorRecordObserver(state, { type: 'supervisor-group-absent', groupID: state.groupID });
        if (!isCurrent() || state.supervisorEnded) return;
        supervisorNotifyRetirement(state);
      },
    }),
    executionDeadlineNs: '',
    hardDeadlineNs: '',
    config: undefined,
    anchor: undefined,
    anchorControl: undefined,
    anchorManagerPID: undefined,
    anchorStartupDeadlineNs: '',
    anchorExecutionDeadlineNs: '',
    anchorPID: undefined,
    guardianPID: undefined,
    groupID: undefined,
    anchorLaunchFailed: false,
    anchorClosed: false,
    anchorExited: false,
    anchorManagerClosed: false,
    targetExecConfirmed: false,
    targetLaunchRequested: false,
    targetProcessCreated: false,
    targetNoChildObserved: false,
    targetProcessPID: undefined,
    targetExit: false,
    targetClose: false,
    targetUnconfirmedClose: false,
    targetStdoutEnded: false,
    targetStdoutClosed: false,
    targetStderrEnded: false,
    targetStderrClosed: false,
    managerStdoutEnded: false,
    managerStdoutClosed: false,
    managerStderrEnded: false,
    managerStderrClosed: false,
    managerOutputStates: { stdout: newManagerRelayState(), stderr: newManagerRelayState() },
    managerInputSummary: undefined,
    supervisorStdoutFinished: false,
    supervisorStdoutClosed: false,
    supervisorStderrFinished: false,
    supervisorStderrClosed: false,
    targetOutputRelayHealthy: true,
    retirementOutputEndsRequested: false,
    inputClosed: false,
    groupAbsent: false,
    stopReason: undefined,
    firstFailure: undefined,
    releaseRequested: false,
    leaseLost: false,
    retirementSent: false,
    unprovedSent: false,
    supervisorEnded: false,
    targetSpawned: false,
    executionTimer: undefined,
    hardTimer: undefined,
    controlBroken: false,
  };
  return state;
}

function supervisorSendControl(state: SupervisorState, value: RecordValue) {
  if (!state.controlBroken) {
    try { state.control.write(encode(value)); } catch { state.controlBroken = true; }
  }
}

function supervisorRecordObserver(state: SupervisorState, value: RecordValue) { return state.observer.record(value); }

function supervisorEvidence(state: SupervisorState) {
  const targetOutputSource = state.targetProcessCreated && state.targetProcessPID !== undefined;
  const managerOutputSource = state.anchorManagerPID !== undefined;
  const stdoutNaturalEnd = targetOutputSource ? state.targetStdoutEnded
    : managerOutputSource ? state.managerStdoutEnded : state.supervisorStdoutFinished;
  const stdoutCloseObserved = targetOutputSource ? state.targetStdoutClosed
    : managerOutputSource ? state.managerStdoutClosed : state.supervisorStdoutClosed;
  const stderrNaturalEnd = targetOutputSource ? state.targetStderrEnded
    : managerOutputSource ? state.managerStderrEnded : state.supervisorStderrFinished;
  const stderrCloseObserved = targetOutputSource ? state.targetStderrClosed
    : managerOutputSource ? state.managerStderrClosed : state.supervisorStderrClosed;
  return {
    targetExitObserved: state.targetExit, targetCloseObserved: state.targetClose,
    targetPIDObserved: state.targetProcessPID !== undefined,
    anchorExitObserved: state.anchorExited, anchorCloseObserved: state.anchorClosed, groupAbsent: state.groupAbsent,
    inputClosedObserved: state.inputClosed,
    stdoutNaturalEnd, stdoutCloseObserved, stderrNaturalEnd, stderrCloseObserved,
    targetStdoutNaturalEnd: state.targetStdoutEnded, targetStdoutCloseObserved: state.targetStdoutClosed,
    targetStderrNaturalEnd: state.targetStderrEnded, targetStderrCloseObserved: state.targetStderrClosed,
    managerStdoutNaturalEnd: state.managerStdoutEnded, managerStdoutCloseObserved: state.managerStdoutClosed,
    managerStderrNaturalEnd: state.managerStderrEnded, managerStderrCloseObserved: state.managerStderrClosed,
    supervisorStdoutFinished: state.supervisorStdoutFinished, supervisorStdoutCloseObserved: state.supervisorStdoutClosed,
    supervisorStderrFinished: state.supervisorStderrFinished, supervisorStderrCloseObserved: state.supervisorStderrClosed,
    callerStdoutNaturalEnd: false, callerStdoutCloseObserved: false,
    callerStderrNaturalEnd: false, callerStderrCloseObserved: false,
    managerProcessCreated: state.anchorManagerPID !== undefined,
    targetDispatchRequested: state.targetLaunchRequested, targetProcessCreated: state.targetProcessCreated, targetNoChildObserved: state.targetNoChildObserved,
    targetExecConfirmed: state.targetExecConfirmed, targetUnconfirmedCloseObserved: state.targetUnconfirmedClose,
    targetOutputRelayHealthy: state.targetOutputRelayHealthy,
    firstFailure: state.firstFailure, stopReason: state.stopReason,
  };
}

function supervisorGroupExists(id: number): boolean {
  try { process.kill(-id, 0); return true; } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === 'ESRCH') return false;
    return true;
  }
}

function supervisorFinalizeIfReleased(state: SupervisorState, force = false) {
  if (state.supervisorEnded || (!force && !state.retirementSent) || (!force && !state.releaseRequested && !state.leaseLost)) return;
  if (force && !state.retirementSent) {
    process.exitCode = 1;
    state.firstFailure ||= 'owned process retirement was not proved before the hard deadline';
  }
  state.supervisorEnded = true;
  state.groupPollOwner.finalize();
  supervisorRecordObserver(state, { type: 'supervisor-teardown-started', clockNs: nowNs().toString(),
    input: state.inputObservation.snapshot(), managerInput: state.managerInputSummary,
    unmetRetirementPrerequisites: supervisorUnmetRetirementPrerequisites(state) });
  state.executionTimer?.cancel();
  state.hardTimer?.cancel();
  if (force) {
    state.input.destroy();
    state.lease.destroy();
    if (state.anchorControl && !state.anchorControl.destroyed) state.anchorControl.destroy();
    state.control.destroy();
    closeDescriptor(0);
    closeDescriptor(3);
    closeDescriptor(6);
    state.stdout.destroy();
    state.stderr.destroy();
    state.observeStdout.destroy();
    state.observeStderr.destroy();
    closeDescriptor(4);
    closeDescriptor(5);
    closeDescriptor(7);
    closeDescriptor(8);
  } else {
    state.control.destroy();
    state.stdout.end();
    state.stderr.end();
    state.observeStdout.end();
    state.observeStderr.end();
  }
  state.observer.close();
}

function supervisorRetirementPrerequisites(state: SupervisorState) {
  const sourceStreamsRetired = state.targetStdoutEnded && state.targetStdoutClosed && state.targetStderrEnded && state.targetStderrClosed;
  const targetSettled = state.targetLaunchRequested
    ? state.targetNoChildObserved || (state.targetProcessCreated && (state.targetExecConfirmed
      ? state.targetExit && state.targetClose && sourceStreamsRetired
      : state.targetUnconfirmedClose && (!state.targetSpawned || sourceStreamsRetired)))
    : !state.targetProcessCreated && !state.targetNoChildObserved;
  const managerRelayRetired = state.anchorManagerPID === undefined
    || (state.managerStdoutEnded && state.managerStdoutClosed && state.managerStderrEnded && state.managerStderrClosed
      && state.managerOutputStates.stdout.finalAckSent && state.managerOutputStates.stderr.finalAckSent
      && state.managerOutputStates.stdout.received === state.managerOutputStates.stdout.expected
      && state.managerOutputStates.stderr.received === state.managerOutputStates.stderr.expected
      && !state.managerOutputStates.stdout.relayFailed && !state.managerOutputStates.stderr.relayFailed);
  return { anchorClosed: state.anchorClosed, anchorManagerClosed: state.anchorManagerClosed, groupAbsent: state.groupAbsent, inputClosed: state.inputClosed, targetSettled, managerRelayRetired,
    stdoutRetired: state.supervisorStdoutClosed && (state.supervisorStdoutFinished || !state.targetOutputRelayHealthy),
    stderrRetired: state.supervisorStderrClosed && (state.supervisorStderrFinished || !state.targetOutputRelayHealthy) };
}

function supervisorUnmetRetirementPrerequisites(state: SupervisorState) { return Object.entries(supervisorRetirementPrerequisites(state)).filter(([, met]) => !met).map(([name]) => name); }

function supervisorNotifyRetirement(state: SupervisorState) {
  const prerequisites = supervisorRetirementPrerequisites(state);
  if (!prerequisites.anchorClosed || !prerequisites.anchorManagerClosed || !prerequisites.groupAbsent || !prerequisites.inputClosed
    || !prerequisites.targetSettled || !prerequisites.managerRelayRetired || state.retirementSent) return;
  if (!state.retirementOutputEndsRequested) {
    state.retirementOutputEndsRequested = true;
    state.stdout.end();
    state.stderr.end();
    state.observeStdout.end();
    state.observeStderr.end();
  }
  const stdoutRetired = state.supervisorStdoutClosed && (state.supervisorStdoutFinished || !state.targetOutputRelayHealthy);
  const stderrRetired = state.supervisorStderrClosed && (state.supervisorStderrFinished || !state.targetOutputRelayHealthy);
  if (!stdoutRetired || !stderrRetired) return;
  supervisorRecordObserver(state, { type: 'supervisor-retirement-ready', evidence: supervisorEvidence(state) });
  state.retirementSent = true;
  supervisorSendControl(state, { type: 'retirement-ready', evidence: supervisorEvidence(state) });
  supervisorFinalizeIfReleased(state);
}

function supervisorInspectGroup(state: SupervisorState, phase: GroupPollPhase) {
  if (state.groupID === undefined || !state.anchorClosed) return true;
  return state.groupPollOwner.inspect(phase);
}

function supervisorOutputFailure(state: SupervisorState, stream: 'stdout' | 'stderr', message: string) {
  state.targetOutputRelayHealthy = false;
  state.firstFailure ||= message;
  supervisorStop(state, 'caller-output-failed', `${stream}: ${state.firstFailure}`);
}

function supervisorManagerControlWritable(state: SupervisorState) { return Boolean(state.anchor && state.anchorControl && !state.anchorManagerClosed
  && state.anchor.exitCode === null && state.anchor.signalCode === null
  && !state.anchorControl.destroyed && state.anchorControl.writable && !state.anchorControl.writableEnded); }

function supervisorStop(state: SupervisorState, reason: string, failure?: string) {
  if (state.supervisorEnded) return;
  supervisorRecordObserver(state, { type: 'supervisor-stopping', reason, failure, anchorClosed: state.anchorClosed, anchorManagerClosed: state.anchorManagerClosed, groupAbsent: state.groupAbsent,
    inputClosed: state.inputClosed, targetSpawned: state.targetSpawned, targetProcessPID: state.targetProcessPID, targetExecConfirmed: state.targetExecConfirmed, targetExit: state.targetExit, targetClose: state.targetClose, targetStdoutEnded: state.targetStdoutEnded, targetStdoutClosed: state.targetStdoutClosed,
    targetStderrEnded: state.targetStderrEnded, targetStderrClosed: state.targetStderrClosed, clockNs: nowNs().toString(), input: state.inputObservation.snapshot(), managerInput: state.managerInputSummary,
    unmetRetirementPrerequisites: supervisorUnmetRetirementPrerequisites(state) });
  const previousStopReason = state.stopReason;
  if (!state.stopReason) state.stopReason = reason;
  if (!state.firstFailure && failure) state.firstFailure = failure;
  supervisorSendControl(state, { type: 'stopping', reason: state.stopReason, failure: state.firstFailure });
  if (!state.targetSpawned && !state.inputClosed) state.input.resume();
  const executionExpired = /^\d+$/u.test(state.executionDeadlineNs) && nowNs() >= BigInt(state.executionDeadlineNs);
  const anchorOwnsTimeout = reason === 'timeout' || previousStopReason === 'timeout' || executionExpired;
  if (!previousStopReason && !anchorOwnsTimeout && supervisorManagerControlWritable(state)) send(state.anchorControl!, { type: 'stop', reason: state.stopReason });
  else if (!previousStopReason && !anchorOwnsTimeout && state.anchor && !state.anchorManagerClosed
    && state.anchor.exitCode === null && state.anchor.signalCode === null) {
    try {
      if (!state.anchor.kill('SIGTERM')) state.firstFailure ||= 'owned anchor manager rejected SIGTERM';
    } catch (error) {
      state.firstFailure ||= error instanceof Error ? error.message : 'owned anchor manager could not be signaled';
    }
  } else if (!previousStopReason && !anchorOwnsTimeout && state.groupID !== undefined) {
    state.firstFailure ||= 'owned process group stop refused because its live anchor control is unavailable';
    supervisorRecordObserver(state, { type: 'supervisor-group-signal-refused', signal: 'SIGTERM', groupID: state.groupID, anchorPID: state.anchorPID, anchorClosed: state.anchorClosed, anchorManagerClosed: state.anchorManagerClosed });
  }
  if (!state.anchor && !state.retirementSent) {
    state.groupAbsent = true;
    state.anchorClosed = true;
    state.anchorExited = true;
    state.anchorManagerClosed = true;
    if (state.inputClosed && !state.targetSpawned) supervisorNotifyRetirement(state);
  }
}

function supervisorFailManagerOutputRelay(state: SupervisorState, reason: string) {
  state.targetOutputRelayHealthy = false;
  state.firstFailure ||= reason;
  state.managerOutputStates.stdout.relayFailed = true;
  state.managerOutputStates.stderr.relayFailed = true;
  supervisorRecordObserver(state, { type: 'supervisor-manager-output-protocol-error', reason });
  if (supervisorManagerControlWritable(state)) send(state.anchorControl!, { type: 'manager-output-abort', reason });
  supervisorStop(state, 'manager-output-protocol-invalid', reason);
}

function supervisorAcknowledgeReceivedManagerOutput(state: SupervisorState, stream: 'stdout' | 'stderr') {
  const output = state.managerOutputStates[stream];
  if (output.relayFailed || output.expected === undefined || output.receivedAckSent) return;
  if (output.received > output.expected) {
    supervisorFailManagerOutputRelay(state, `${stream} manager relay exceeded its advertised byte count`);
    return;
  }
  if (output.received !== output.expected) return;
  if (output.sourceEnded || output.sourceClosed) {
    supervisorFailManagerOutputRelay(state, `${stream} manager relay reached EOF before its byte acknowledgement`);
    return;
  }
  output.receivedAckSent = true;
  send(state.anchorControl!, { type: 'manager-output-received', stream, bytes: output.expected });
}

function supervisorReceiveManagerOutputFinished(state: SupervisorState, message: RecordValue) {
  const stream = message.stream;
  if ((stream !== 'stdout' && stream !== 'stderr') || state.anchorManagerPID === undefined
    || !Number.isSafeInteger(message.bytes) || Number(message.bytes) < 0) {
    supervisorFailManagerOutputRelay(state, 'manager output completion named an unknown stream or invalid byte count');
    return;
  }
  const output = state.managerOutputStates[stream];
  if (output.expected !== undefined || output.sourceEnded || output.sourceClosed || output.relayFailed) {
    supervisorFailManagerOutputRelay(state, `duplicate or late ${stream} manager output completion`);
    return;
  }
  output.expected = Number(message.bytes);
  supervisorAcknowledgeReceivedManagerOutput(state, stream);
}

function supervisorMarkLeaseLost(state: SupervisorState) {
  if (state.leaseLost) return;
  state.leaseLost = true;
  state.releaseRequested = true;
  supervisorStop(state, 'lease-lost');
  supervisorFinalizeIfReleased(state);
}

function supervisorRelay(state: SupervisorState, source: NodeJS.ReadableStream, destination: Writable, observer: Writable | undefined, streamName: 'stdout' | 'stderr', onEnd: () => void, onClose: () => void) {
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
    if (error?.code === 'EPIPE') supervisorRecordObserver(state, { type: 'downstream-epipe', stream: streamName });
    state.targetOutputRelayHealthy = false;
    const message = error instanceof Error ? error.message : `${streamName} caller-facing relay closed early`;
    state.firstFailure ||= message;
    supervisorStop(state, 'caller-output-failed', `${streamName}: ${state.firstFailure}`);
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
    const outputState = state.managerOutputStates[streamName];
    const nextReceived = outputState.received + chunk.byteLength;
    if (!Number.isSafeInteger(nextReceived)) {
      supervisorFailManagerOutputRelay(state, `${streamName} manager relay byte count exceeded its safe bound`);
      return;
    }
    outputState.received = nextReceived;
    supervisorAcknowledgeReceivedManagerOutput(state, streamName);
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
    const outputState = state.managerOutputStates[streamName];
    outputState.sourceEnded = true;
    supervisorRecordObserver(state, { type: 'supervisor-manager-relay-end', stream: streamName });
    if (outputState.expected === undefined || outputState.received !== outputState.expected || !outputState.receivedAckSent) {
      supervisorFailManagerOutputRelay(state, `${streamName} manager relay ended without an exact byte handoff`);
    }
    onEnd();
    destination.end();
    observer?.end();
  });
  source.once('close', () => {
    const outputState = state.managerOutputStates[streamName];
    outputState.sourceClosed = true;
    supervisorRecordObserver(state, { type: 'supervisor-manager-relay-close', stream: streamName, naturalEnd: sourceEnded });
    if (!sourceEnded || outputState.expected === undefined || outputState.received !== outputState.expected
      || !outputState.receivedAckSent || outputState.relayFailed) {
      supervisorFailManagerOutputRelay(state, `${streamName} manager relay closed without a proven byte handoff and natural EOF`);
    } else if (!outputState.finalAckSent) {
      outputState.finalAckSent = true;
      send(state.anchorControl!, { type: 'manager-output-retired', stream: streamName, bytes: outputState.expected,
        naturalEnd: true, closeObserved: true });
    }
    onClose();
    supervisorNotifyRetirement(state);
  });
  source.on('error', error => {
    const outputState = state.managerOutputStates[streamName];
    outputState.relayFailed = true;
    supervisorFailManagerOutputRelay(state, error instanceof Error ? error.message : 'manager relay stream failed');
    supervisorStop(state, 'manager-relay-error', state.firstFailure);
  });
}

function supervisorReceiveTargetSourceEvent(state: SupervisorState, message: RecordValue) {
  const stream = message.stream;
  if (!state.targetLaunchRequested || !state.targetProcessCreated || state.targetNoChildObserved
    || (stream !== 'stdout' && stream !== 'stderr')) {
    state.targetOutputRelayHealthy = false;
    supervisorStop(state, 'target-source-protocol-invalid', 'target source event did not match a created target and known stream');
    return;
  }
  const ended = stream === 'stdout' ? state.targetStdoutEnded : state.targetStderrEnded;
  const closed = stream === 'stdout' ? state.targetStdoutClosed : state.targetStderrClosed;
  if (message.type === 'target-source-end') {
    if (ended || closed) {
      state.targetOutputRelayHealthy = false;
      supervisorStop(state, 'target-source-protocol-invalid', `duplicate or late ${stream} source end`);
      return;
    }
    if (stream === 'stdout') state.targetStdoutEnded = true;
    else state.targetStderrEnded = true;
    supervisorRecordObserver(state, { type: 'supervisor-target-source-end', stream });
    supervisorNotifyRetirement(state);
    return;
  }
  if (closed || message.naturalEnd !== ended) {
    state.targetOutputRelayHealthy = false;
    supervisorStop(state, 'target-source-protocol-invalid', `invalid or duplicate ${stream} source close`);
    return;
  }
  if (stream === 'stdout') state.targetStdoutClosed = true;
  else state.targetStderrClosed = true;
  if (!ended) {
    state.targetOutputRelayHealthy = false;
    supervisorStop(state, 'target-source-closed-before-end', `${stream} target source closed without natural EOF`);
  }
  supervisorRecordObserver(state, { type: 'supervisor-target-source-close', stream, naturalEnd: ended });
  supervisorNotifyRetirement(state);
}

function supervisorStartAnchor(state: SupervisorState) {
  if (!state.config || state.anchor || state.leaseLost || state.stopReason) return;
  supervisorSendControl(state, { type: 'supervisor-progress', stage: 'anchor-launch-requested' });
  if (nowNs() >= BigInt(state.config.executionDeadlineNs)) { supervisorStop(state, 'timeout'); return; }
  const cleanupAllowanceNs = BigInt(state.config.hardDeadlineNs) - BigInt(state.config.executionDeadlineNs);
  const startupReserveNs = cleanupAllowanceNs > 0n
    ? cleanupAllowanceNs / 2n < 500_000_000n ? cleanupAllowanceNs / 2n : 500_000_000n
    : 0n;
  state.anchorStartupDeadlineNs = (BigInt(state.config.hardDeadlineNs) - startupReserveNs).toString();
  try {
    state.anchor = spawn('bun', ['--no-env-file', '--experimental-strip-types', fileURLToPath(import.meta.url), '--anchor-manager',
      state.anchorStartupDeadlineNs, String(process.pid)], {
      detached: false,
      stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe'],
      cwd: process.cwd(), env: process.env,
    });
  } catch (error) {
    supervisorStop(state, 'anchor-launch-error', error instanceof Error ? error.message : 'anchor manager could not be launched');
    return;
  }
  if (state.anchor.pid === undefined) {
    state.anchorLaunchFailed = true;
    supervisorStop(state, 'anchor-launch-error', 'anchor manager did not receive a process ID');
    return;
  }
  supervisorConnectAnchor(state, state.anchor);
}

function supervisorConnectAnchor(state: SupervisorState, anchor: ChildProcess) {
  state.anchorManagerPID = anchor.pid;
  const managerControl = controlStream(anchor);
  state.anchorControl = managerControl;
  const anchorInput = writableStream(anchor, 6);
  anchorInput.on('error', error => supervisorStop(state, 'caller-input-failed', error.message));
  state.input.on('error', error => supervisorStop(state, 'caller-input-failed', error.message));
  // This one-way endpoint must release only after all writes and finalization succeed.
  releaseOwnedInputOnFinish(anchorInput);
  state.input.pipe(anchorInput);
  state.inputObservation.connected(anchorInput);
  supervisorRelay(state, readableStream(anchor, 4), state.stdout, state.observeStdout, 'stdout',
    () => { state.managerStdoutEnded = true; supervisorNotifyRetirement(state); },
    () => { state.managerStdoutClosed = true; supervisorNotifyRetirement(state); });
  supervisorRelay(state, readableStream(anchor, 5), state.stderr, state.observeStderr, 'stderr',
    () => { state.managerStderrEnded = true; supervisorNotifyRetirement(state); },
    () => { state.managerStderrClosed = true; supervisorNotifyRetirement(state); });
  managerControl.on('error', () => {
    if (state.anchorManagerClosed || anchor.exitCode !== null || anchor.signalCode !== null) return;
    supervisorFailManagerOutputRelay(state, 'owned anchor manager control channel failed');
  });
  parseLines(managerControl, message => supervisorHandleManagerMessage(state, message),
    error => supervisorFailManagerOutputRelay(state, `anchor manager control protocol failed: ${error.message}`));
  anchor.once('error', error => supervisorStop(state, 'anchor-manager-launch-error', error.message));
  anchor.once('close', (code, signal) => supervisorObserveManagerClose(state, code, signal));
}

function supervisorObserveManagerClose(state: SupervisorState, code: number | null, signal: NodeJS.Signals | null) {
  state.anchorManagerClosed = true;
  supervisorRecordObserver(state, { type: 'supervisor-manager-close', code, signal, anchorClosed: state.anchorClosed, groupAbsent: state.groupAbsent,
    targetSpawned: state.targetSpawned, targetExit: state.targetExit, targetClose: state.targetClose,
    targetStdoutEnded: state.targetStdoutEnded, targetStdoutClosed: state.targetStdoutClosed,
    targetStderrEnded: state.targetStderrEnded, targetStderrClosed: state.targetStderrClosed, inputClosed: state.inputClosed });
  state.anchorControl?.destroy();
  if (state.anchorPID === undefined && state.anchorLaunchFailed) {
    state.anchorExited = true;
    state.anchorClosed = true;
    state.groupAbsent = true;
  }
  const currentGroupInspection = supervisorInspectGroup(state, 'after-manager-close');
  if (currentGroupInspection && !state.supervisorEnded) supervisorNotifyRetirement(state);
}

function supervisorAdmitAnchorProcess(state: SupervisorState, message: RecordValue) {
  const values = [message.processID, message.spawnParentPID, message.sessionID];
  const processID = Number(message.processID);
  if (!Number.isSafeInteger(processID) || processID <= 0 || Number(message.spawnParentPID) !== state.anchorManagerPID
    || Number(message.sessionID) !== process.pid || values.some(value => !Number.isSafeInteger(value))) {
    supervisorStop(state, 'anchor-admission-failed', 'anchor process identity did not match its actual manager and supervisor');
    return;
  }
  state.anchorPID = processID;
  supervisorRecordObserver(state, { type: 'anchor-process-started', processID, spawnParentPID: state.anchorManagerPID,
    startupDeadlineNs: state.anchorStartupDeadlineNs });
  supervisorSendControl(state, message);
}

function supervisorAdmitAnchorNative(state: SupervisorState, message: RecordValue) {
  const clock = typeof message.clockNs === 'string' && /^\d+$/u.test(message.clockNs) ? BigInt(message.clockNs) : undefined;
  const candidateGuardianPID = Number(message.guardianPID);
  const identities = [message.anchorPID, message.spawnParentPID, message.groupID, message.sessionID,
    message.guardianPID, message.guardianParentPID, message.guardianGroupID, message.guardianSessionID];
  if (clock === undefined || Number(message.anchorPID) !== state.anchorPID || Number(message.spawnParentPID) !== state.anchorManagerPID
    || Number(message.groupID) !== state.anchorPID || Number(message.sessionID) !== process.pid
    || !Number.isSafeInteger(candidateGuardianPID) || candidateGuardianPID <= 0 || candidateGuardianPID === state.anchorPID
    || Number(message.guardianParentPID) !== state.anchorPID || Number(message.guardianGroupID) !== state.anchorPID
    || Number(message.guardianSessionID) !== process.pid || identities.some(value => !Number.isSafeInteger(value))) {
    supervisorStop(state, 'anchor-admission-failed', 'anchor or guardian identity and clock did not match their owned process group');
    return;
  }
  state.guardianPID = candidateGuardianPID;
  state.groupID = Number(message.groupID);
  supervisorSendControl(state, message);
  const supervisorNow = nowNs();
  supervisorRecordObserver(state, { type: 'anchor-clock-handshake', anchorClockNs: clock.toString(), supervisorClockNs: supervisorNow.toString() });
  const executionRemaining = BigInt(state.config!.executionDeadlineNs) - supervisorNow - 5_000_000n;
  const cleanupAllowanceNs = BigInt(state.config!.hardDeadlineNs) - BigInt(state.config!.executionDeadlineNs);
  const reserveNs = cleanupAllowanceNs > 0n
    ? cleanupAllowanceNs / 2n < 500_000_000n ? cleanupAllowanceNs / 2n : 500_000_000n
    : 0n;
  const anchorHardDeadline = BigInt(state.config!.hardDeadlineNs) - reserveNs;
  const hardRemaining = anchorHardDeadline - supervisorNow - 5_000_000n;
  if (hardRemaining <= 0n || executionRemaining <= 0n) {
    supervisorStop(state, 'timeout');
    return;
  }
  state.anchorExecutionDeadlineNs = (clock + executionRemaining).toString();
  send(state.anchorControl!, { type: 'configure-anchor', executionDeadlineNs: state.anchorExecutionDeadlineNs,
    hardDeadlineNs: (clock + hardRemaining).toString(), killDelayMs: state.config!.killDelayMs });
}

function supervisorAdmitAnchorReady(state: SupervisorState, message: RecordValue) {
  const values = [message.anchorPID, message.spawnParentPID, message.groupID, message.sessionID,
    message.guardianPID, message.guardianParentPID, message.guardianGroupID, message.guardianSessionID];
  if (Number(message.anchorPID) !== state.anchorPID || Number(message.spawnParentPID) !== state.anchorManagerPID
    || Number(message.groupID) !== state.anchorPID || Number(message.sessionID) !== process.pid
    || Number(message.guardianPID) !== state.guardianPID || !Number.isSafeInteger(state.guardianPID)
    || state.guardianPID === state.anchorPID || Number(message.guardianParentPID) !== state.anchorPID
    || Number(message.guardianGroupID) !== state.anchorPID || Number(message.guardianSessionID) !== process.pid
    || values.some(value => !Number.isSafeInteger(value))) {
    supervisorStop(state, 'anchor-admission-failed', 'anchor or guardian identity changed before readiness');
    return;
  }
  supervisorSendControl(state, message);
  if (state.leaseLost || state.stopReason || nowNs() >= BigInt(state.config!.executionDeadlineNs)) {
    supervisorStop(state, state.stopReason || 'timeout');
    return;
  }
  state.targetLaunchRequested = true;
  send(state.anchorControl!, { type: 'launch-target', binary: state.config!.binary, args: state.config!.args,
    executionDeadlineNs: state.anchorExecutionDeadlineNs });
}

function supervisorObserveAnchorExit(state: SupervisorState, message: RecordValue) {
  if (message.type === 'anchor-exit') {
    state.anchorExited = true;
    supervisorRecordObserver(state, { type: 'supervisor-anchor-exit', code: message.code, signal: message.signal });
  }
  if (message.type === 'anchor-close') {
    state.anchorClosed = true;
    supervisorRecordObserver(state, { type: 'supervisor-anchor-close', code: message.code, signal: message.signal, groupID: state.groupID });
    if (state.groupID === undefined) state.groupAbsent = true;
    const currentGroupInspection = supervisorInspectGroup(state, state.anchorManagerClosed ? 'after-manager-close' : 'before-manager-close');
    if (currentGroupInspection && !state.supervisorEnded) supervisorNotifyRetirement(state);
  }
}

function supervisorAdmitTargetProcess(state: SupervisorState, message: RecordValue): boolean {
  if (message.type === 'target-process-started' || message.type === 'target-process-unconfirmed') {
    const values = [message.spawnParentPID, message.groupID, message.anchorPID];
    const knownPID = message.type === 'target-process-started'
      ? Number.isSafeInteger(message.processID) && Number(message.processID) > 0
      : message.processID === null;
    if (!state.targetLaunchRequested || state.targetProcessCreated || state.targetNoChildObserved || !knownPID
      || Number(message.spawnParentPID) !== state.anchorManagerPID || Number(message.groupID) !== state.groupID
      || Number(message.anchorPID) !== state.anchorPID || values.some(value => !Number.isSafeInteger(value))) {
      supervisorStop(state, 'target-admission-failed', 'target process creation did not match one requested child and its actual manager and group');
      return false;
    }
    state.targetProcessCreated = true;
    if (message.type === 'target-process-started') {
      state.targetSpawned = true;
      state.targetProcessPID = Number(message.processID);
      supervisorRecordObserver(state, { type: 'supervisor-target-process-started', targetPID: state.targetProcessPID,
        spawnParentPID: state.anchorManagerPID, groupID: state.groupID, anchorPID: state.anchorPID });
    } else supervisorRecordObserver(state, { type: 'supervisor-target-process-unconfirmed', groupID: state.groupID, anchorPID: state.anchorPID });
  }
  if (message.type === 'target-no-child') {
    if (!state.targetLaunchRequested || state.targetProcessCreated || state.targetNoChildObserved || message.launchRequestReceived !== true
      || Number(message.spawnParentPID) !== state.anchorManagerPID || Number(message.groupID) !== state.groupID
      || Number(message.anchorPID) !== state.anchorPID) {
      supervisorStop(state, 'target-admission-failed', 'manager no-child outcome did not match an outstanding launch request');
      return false;
    }
    state.targetNoChildObserved = true;
    supervisorRecordObserver(state, { type: 'supervisor-target-no-child', groupID: state.groupID, anchorPID: state.anchorPID });
  }
  return true;
}

function supervisorTrackTargetLifecycle(state: SupervisorState, message: RecordValue): boolean {
  if (message.type === 'target-started') {
    const values = [message.targetPID, message.spawnParentPID, message.groupID, message.anchorPID];
    if (!state.targetProcessCreated || state.targetExecConfirmed || !state.targetSpawned || Number(message.targetPID) !== state.targetProcessPID
      || !Number.isSafeInteger(message.targetPID) || Number(message.targetPID) <= 0
      || Number(message.spawnParentPID) !== state.anchorManagerPID || Number(message.groupID) !== state.groupID
      || Number(message.anchorPID) !== state.anchorPID || values.some(value => !Number.isSafeInteger(value))
      || message.callback !== undefined) {
      supervisorStop(state, 'target-admission-failed', 'target exec identity did not match its actual manager, PID, and group');
      return false;
    }
    state.targetExecConfirmed = true;
    supervisorRecordObserver(state, { type: 'supervisor-target-started', targetPID: state.targetProcessPID,
      spawnParentPID: state.anchorManagerPID, groupID: state.groupID, anchorPID: state.anchorPID });
  }
  if (message.type === 'target-exit') {
    if (!state.targetExecConfirmed || state.targetExit) {
      supervisorStop(state, 'target-lifecycle-invalid', 'target exit was missing exec confirmation or duplicated');
      return false;
    }
    state.targetExit = true;
    supervisorRecordObserver(state, { type: 'supervisor-target-exit', code: message.code, signal: message.signal });
  }
  if (message.type === 'target-close') {
    if (!state.targetExecConfirmed || state.targetClose) {
      supervisorStop(state, 'target-lifecycle-invalid', 'target close was missing exec confirmation or uniqueness');
      return false;
    }
    state.targetClose = true;
    supervisorRecordObserver(state, { type: 'supervisor-target-close', code: message.code, signal: message.signal });
  }
  if (message.type === 'target-unconfirmed-close') {
    if (!state.targetProcessCreated || state.targetExecConfirmed || state.targetUnconfirmedClose) {
      supervisorStop(state, 'target-lifecycle-invalid', 'unconfirmed target close did not match one unconfirmed child or uniqueness');
      return false;
    }
    state.targetUnconfirmedClose = true;
  }
  return true;
}

function supervisorRecordManagerFailure(state: SupervisorState, message: RecordValue) {
  if (message.type === 'failure') {
    state.firstFailure ||= typeof message.message === 'string' ? message.message : 'owned target process failed';
    if (message.reason === 'target-output-failed' || message.reason === 'target-stream-error'
      || message.reason === 'manager-output-closed' || message.reason === 'manager-output-failed'
      || message.reason === 'caller-output-failed' || message.reason === 'manager-relay-closed-early' || message.reason === 'manager-relay-error') {
      state.targetOutputRelayHealthy = false;
    }
  }
  if (message.type === 'anchor-stopping' || message.type === 'stopping') {
    const reason = typeof message.reason === 'string' ? message.reason : 'anchor-stopping';
    if (!state.stopReason) state.stopReason = reason;
    if (message.type === 'anchor-stopping') supervisorSendControl(state, { type: 'stopping', reason });
  }
}

function supervisorForwardManagerMessage(state: SupervisorState, message: RecordValue) {
  if (message.type === 'anchor-bootstrap-error') {
    state.anchorLaunchFailed = true;
    const failure = typeof message.message === 'string' ? message.message : 'anchor initialization failed';
    supervisorStop(state, 'anchor-launch-error', failure);
  } else if (message.type === 'anchor-kill-error') {
    const failure = typeof message.message === 'string' ? message.message : 'anchor could not retire its process group';
    supervisorStop(state, 'anchor-retirement-failed', failure);
  } else if (message.type === 'target-launch-error') {
    state.firstFailure ||= typeof message.message === 'string' ? message.message : 'target launch failed';
    supervisorSendControl(state, message);
  } else if (message.type === 'target-started' || message.type === 'target-exit' || message.type === 'target-close'
    || message.type === 'target-unconfirmed-close' || message.type === 'failure' || message.type === 'stopping'
    || message.type === 'anchor-exit' || message.type === 'anchor-close') {
    if (message.type === 'target-unconfirmed-close') {
      supervisorSendControl(state, { type: 'target-close', code: message.code, signal: message.signal,
        ...(typeof message.launchError === 'string' ? { launchError: message.launchError } : {}) });
    } else supervisorSendControl(state, message);
  }
  if (message.type === 'stopping' || message.type === 'failure' || message.type === 'target-input-error') {
    supervisorStop(state, typeof message.reason === 'string' ? message.reason : 'owned-process-failure',
      typeof message.message === 'string' ? message.message : undefined);
  }
}

function supervisorHandleManagerMessage(state: SupervisorState, message: RecordValue) {
  if (message.type === 'manager-input-observation') {
    if (typeof message.event !== 'string' || !['connected', 'sourceEnd', 'sourceClose', 'sourceError',
      'destinationFinish', 'destinationClose', 'destinationError', 'stop', 'teardown'].includes(message.event)) return;
    state.managerInputSummary = sanitizedInputPipeSummary(message.summary);
    supervisorRecordObserver(state, { type: 'supervisor-manager-input-observation', event: message.event, summary: state.managerInputSummary });
    return;
  }
  if (message.type === 'manager-output-finished') {
    supervisorReceiveManagerOutputFinished(state, message);
    return;
  }
  if (message.type === 'manager-teardown-started') supervisorRecordObserver(state, { type: 'supervisor-manager-teardown-started', anchorClosed: state.anchorClosed,
    targetSpawned: state.targetSpawned, targetClose: state.targetClose, message });
  if (message.type === 'anchor-process-started') {
    supervisorAdmitAnchorProcess(state, message);
    return;
  }
  if (message.type === 'anchor-native-ready') {
    supervisorAdmitAnchorNative(state, message);
    return;
  }
  if (message.type === 'anchor-ready') {
    supervisorAdmitAnchorReady(state, message);
    return;
  }
  supervisorObserveAnchorExit(state, message);
  if (!supervisorAdmitTargetProcess(state, message)) return;
  if (message.type === 'target-source-end' || message.type === 'target-source-close') {
    supervisorReceiveTargetSourceEvent(state, message);
    return;
  }
  if (!supervisorTrackTargetLifecycle(state, message)) return;
  supervisorRecordManagerFailure(state, message);
  supervisorForwardManagerMessage(state, message);
  supervisorNotifyRetirement(state);
}

function supervisorOnHardDeadline(state: SupervisorState) {
  supervisorRecordObserver(state, { type: 'supervisor-hard-deadline', clockNs: nowNs().toString(), retirementSent: state.retirementSent, inputClosed: state.inputClosed,
    targetSpawned: state.targetSpawned, anchorClosed: state.anchorClosed, groupAbsent: state.groupAbsent });
  state.releaseRequested = true;
  if (supervisorManagerControlWritable(state)) send(state.anchorControl!, { type: 'manager-output-abort', reason: 'hard-deadline' });
  supervisorStop(state, 'hard-deadline');
  if (supervisorManagerControlWritable(state) && !state.anchorClosed) send(state.anchorControl!, { type: 'kill-group', reason: 'hard-deadline' });
  else if (state.groupID !== undefined) {
    state.firstFailure ||= 'owned process group kill refused because its live anchor control is unavailable';
    supervisorRecordObserver(state, { type: 'supervisor-group-signal-refused', signal: 'SIGKILL', groupID: state.groupID, anchorPID: state.anchorPID, anchorClosed: state.anchorClosed, anchorManagerClosed: state.anchorManagerClosed });
  }
  if (!state.retirementSent && !state.unprovedSent) {
    state.unprovedSent = true;
    supervisorRecordObserver(state, { type: 'supervisor-retirement-unproved', evidence: supervisorEvidence(state), input: state.inputObservation.snapshot(),
      managerInput: state.managerInputSummary, unmetRetirementPrerequisites: supervisorUnmetRetirementPrerequisites(state) });
    supervisorSendControl(state, { type: 'retirement-unproved', evidence: supervisorEvidence(state) });
  }
  if (state.anchor && !state.anchorManagerClosed && state.anchor.exitCode === null && state.anchor.signalCode === null) {
    supervisorRecordObserver(state, { type: 'supervisor-manager-forced-kill', managerPID: state.anchor.pid });
    try {
      if (!state.anchor.kill('SIGKILL')) state.firstFailure ||= 'owned anchor manager could not be killed at its hard deadline';
    } catch (error) {
      state.firstFailure ||= error instanceof Error ? error.message : 'owned anchor manager could not be killed at its hard deadline';
    }
  }
  supervisorFinalizeIfReleased(state, true);
}

function supervisorScheduleDeadlines(state: SupervisorState, executionDeadline: string, hardDeadline: string) {
  state.executionDeadlineNs = executionDeadline;
  state.hardDeadlineNs = hardDeadline;
  state.executionTimer?.cancel();
  state.hardTimer?.cancel();
  state.executionTimer = scheduleAt(state.executionDeadlineNs, () => {
    supervisorRecordObserver(state, { type: 'supervisor-execution-deadline', clockNs: nowNs().toString() });
    supervisorStop(state, 'timeout');
  });
  state.hardTimer = scheduleAt(state.hardDeadlineNs, () => supervisorOnHardDeadline(state));
}

function supervisorArmDeadlines(state: SupervisorState) {
  if (!/^\d+$/u.test(state.startupExecutionDeadlineNs) || !/^\d+$/u.test(state.startupHardDeadlineNs)) {
    supervisorStop(state, 'invalid-supervisor-deadlines', 'supervisor did not receive original monotonic deadlines');
    return;
  }
  const executionDeadline = BigInt(state.startupExecutionDeadlineNs);
  const hardDeadline = BigInt(state.startupHardDeadlineNs);
  if (executionDeadline >= hardDeadline) {
    supervisorStop(state, 'invalid-supervisor-deadlines', 'supervisor startup deadlines were inconsistent');
    return;
  }
  supervisorScheduleDeadlines(state, state.startupExecutionDeadlineNs, state.startupHardDeadlineNs);
}

function supervisorWatchCallerOutputs(state: SupervisorState) {
  state.stdout.once('finish', () => { state.supervisorStdoutFinished = true; supervisorNotifyRetirement(state); });
  state.stdout.once('close', () => {
    state.supervisorStdoutClosed = true;
    if (!state.supervisorStdoutFinished) supervisorOutputFailure(state, 'stdout', 'caller-facing stdout closed before writer finish');
    supervisorNotifyRetirement(state);
  });
  state.stdout.once('error', error => supervisorOutputFailure(state, 'stdout', error instanceof Error ? error.message : 'caller-facing stdout failed'));
  state.stderr.once('finish', () => { state.supervisorStderrFinished = true; supervisorNotifyRetirement(state); });
  state.stderr.once('close', () => {
    state.supervisorStderrClosed = true;
    if (!state.supervisorStderrFinished) supervisorOutputFailure(state, 'stderr', 'caller-facing stderr closed before writer finish');
    supervisorNotifyRetirement(state);
  });
  state.stderr.once('error', error => supervisorOutputFailure(state, 'stderr', error instanceof Error ? error.message : 'caller-facing stderr failed'));
}

function supervisorWatchEndpoints(state: SupervisorState) {
  state.lease.on('end', () => supervisorMarkLeaseLost(state));
  state.lease.on('error', () => supervisorMarkLeaseLost(state));
  state.control.on('error', () => { state.controlBroken = true; supervisorStop(state, 'supervisor-control-failed'); });
  state.control.on('end', () => { state.controlBroken = true; supervisorStop(state, 'supervisor-control-lost'); });
  state.input.once('close', () => { state.inputClosed = true; supervisorRecordObserver(state, { type: 'supervisor-input-close' }); supervisorNotifyRetirement(state); });
  state.input.once('end', () => { state.inputClosed = true; supervisorRecordObserver(state, { type: 'supervisor-input-end' }); supervisorNotifyRetirement(state); });
  state.lease.resume();
}

function supervisorConfigure(state: SupervisorState, message: RecordValue) {
  if (state.config || typeof message.binary !== 'string' || !Array.isArray(message.args)
    || message.args.some(value => typeof value !== 'string')
    || message.args.some(value => value.includes('\0')) || message.binary.includes('\0')
    || typeof message.executionDeadlineNs !== 'string' || typeof message.hardDeadlineNs !== 'string'
    || typeof message.killDelayMs !== 'number') {
    supervisorStop(state, 'invalid-launch-request', 'invalid owned process launch configuration');
    return;
  }
  const config = message as unknown as LaunchRequest;
  state.config = config;
  const configuredExecution = BigInt(config.executionDeadlineNs);
  const configuredHard = BigInt(config.hardDeadlineNs);
  const startupExecutionDeadline = BigInt(state.startupExecutionDeadlineNs);
  const startupHardDeadline = BigInt(state.startupHardDeadlineNs);
  if (configuredExecution > startupExecutionDeadline || configuredHard > startupHardDeadline
    || configuredExecution >= configuredHard || configuredHard <= nowNs()) {
    supervisorStop(state, 'invalid-launch-request', 'translated launch deadlines extended or outlived the finite startup allowance');
    return;
  }
  supervisorScheduleDeadlines(state, config.executionDeadlineNs, config.hardDeadlineNs);
  supervisorSendControl(state, { type: 'ready' });
  if (state.leaseLost) supervisorStop(state, 'lease-lost');
}

function supervisorHandleControlMessage(state: SupervisorState, message: RecordValue) {
  if (message.type === 'configure') supervisorConfigure(state, message);
  else if (message.type === 'start') supervisorStartAnchor(state);
  else if (message.type === 'stop') supervisorStop(state, typeof message.reason === 'string' ? message.reason : 'stop-request');
  else if (message.type === 'release') {
    state.releaseRequested = true;
    if (!state.anchor && !state.retirementSent) supervisorStop(state, 'released-before-dispatch');
    supervisorFinalizeIfReleased(state);
  }
}

function runSupervisor(): void {
  const state = createSupervisorState();
  supervisorWatchCallerOutputs(state);
  supervisorWatchEndpoints(state);
  supervisorArmDeadlines(state);
  supervisorRecordObserver(state, { type: 'supervisor-deadlines-armed', executionDeadlineNs: state.startupExecutionDeadlineNs,
    hardDeadlineNs: state.startupHardDeadlineNs, clockNs: nowNs().toString() });
  parseLines(state.control, message => supervisorHandleControlMessage(state, message),
    error => supervisorStop(state, 'supervisor-control-handler-failed', error.message));
  supervisorSendControl(state, { type: 'runtime-clock', clockNs: nowNs().toString() });
}

const mode = process.argv[2];
if (mode === '--anchor') void runAnchor();
else if (mode === '--anchor-guardian') void runAnchorGuardian();
else if (mode === '--anchor-manager') runAnchorManager();
else if (mode === '--supervisor') runSupervisor();

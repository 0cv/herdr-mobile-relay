import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { constants, openSync, closeSync } from 'node:fs';
import { open, readlink, rename, writeFile } from 'node:fs/promises';
import { basename, resolve } from 'node:path';
import { constants as osConstants } from 'node:os';
import { fileURLToPath } from 'node:url';

const RECEIPT_LIMIT = 2048;
const STAT_LIMIT = 4096;
const BOOT_FILE = '/proc/sys/kernel/random/boot_id';
type Unavailable = 'unsupported-os' | 'missing' | 'io-error' | 'malformed' | 'byte-limit' | 'probe-timeout' | 'spawn-error' | 'exit-before-binding';
type ProcessKind = 'qemu' | 'launcher' | 'other' | 'unknown';
interface Birth { startTicks: string; bootIdSha256: string }
interface Binding {
  childPid: number | null;
  birth: Birth | null;
  processKind: ProcessKind;
  status: 'bound' | Unavailable;
}
interface NativeExit { observedAtMs: number; code: number | null; signal: NodeJS.Signals | null }
export interface EmulatorLifetimeReceipt {
  schema: 1;
  binding: Binding;
  bindingSha256: string;
  exit?: NativeExit;
}
export interface HostEmulatorAtSettlement {
  hostSettledAtMs: number;
  sampleStartedAtMs: number;
  sampleEndedAtMs: number;
  state: 'alive' | 'gone' | 'unknown';
  reason: 'same-birth' | 'absent' | 'zombie' | 'exited' | 'pid-reused' | 'boot-mismatch' | 'invalid-receipt' | Unavailable;
  bindingSha256?: string;
  processKind?: ProcessKind;
  nativeExitCoverage: 'observed' | 'unknown';
  nativeExit?: NativeExit;
}
interface ProbeIO {
  platform: string;
  read: (path: string, limit: number, signal: AbortSignal) => Promise<string>;
  readlink: (path: string) => Promise<string>;
}
class ProbeError extends Error {
  reason: Unavailable;
  constructor(reason: Unavailable) { super(reason); this.reason = reason; }
}
function unavailable(error: unknown): Unavailable {
  if (error instanceof ProbeError) return error.reason;
  return (error as NodeJS.ErrnoException)?.code === 'ENOENT' ? 'missing' : 'io-error';
}
function checkTime(signal: AbortSignal): void {
  if (signal.aborted) throw new ProbeError('probe-timeout');
}
export async function readEmulatorFile(path: string, limit: number, signal: AbortSignal): Promise<string> {
  checkTime(signal);
  const file = await open(path, constants.O_RDONLY | constants.O_NONBLOCK | constants.O_NOFOLLOW);
  try {
    checkTime(signal);
    if (!(await file.stat()).isFile()) throw new ProbeError('io-error');
    const buffer = Buffer.alloc(limit + 1);
    let length = 0;
    for (;;) {
      checkTime(signal);
      const { bytesRead } = await file.read(buffer, length, buffer.length - length, null);
      checkTime(signal);
      length += bytesRead;
      if (length > limit) throw new ProbeError('byte-limit');
      if (!bytesRead) break;
    }
    const text = buffer.subarray(0, length).toString('utf8');
    if (!Buffer.from(text).equals(buffer.subarray(0, length))) throw new ProbeError('malformed');
    return text;
  } finally { await file.close(); }
}
const defaultIO: ProbeIO = { platform: process.platform, read: readEmulatorFile, readlink };
function hash(text: string): string { return createHash('sha256').update(text).digest('hex'); }
export function emulatorBindingSha256(binding: Binding): string {
  return hash(JSON.stringify({ childPid: binding.childPid,
    birth: binding.birth && { startTicks: binding.birth.startTicks, bootIdSha256: binding.birth.bootIdSha256 },
    processKind: binding.processKind, status: binding.status }));
}
function parseStat(text: string, pid: number): { startTicks: string; state: string } {
  const match = /^(\d+) \([\s\S]*\) ([A-Za-z]) (.*)\n?$/u.exec(text);
  const fields = match?.[3].trim().split(/\s+/u);
  if (!match || match[1] !== String(pid) || !/^[RSDZTtXxKWPI]$/u.test(match[2])
    || !fields || fields.length < 19 || !/^\d{1,40}$/u.test(fields[18])) {
    throw new ProbeError('malformed');
  }
  return { state: match[2], startTicks: fields[18] };
}
function bootHash(text: string): string {
  const value = text.trim();
  if (!/^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/u.test(value)) throw new ProbeError('malformed');
  return hash(value);
}
function imageKind(path: string): ProcessKind {
  if (Buffer.byteLength(path) > 4096) return 'unknown';
  const name = basename(path);
  if (['qemu-system-x86_64', 'qemu-system-aarch64', 'qemu-system-i386', 'qemu-system-arm'].includes(name)) return 'qemu';
  return name === 'emulator' ? 'launcher' : 'other';
}
export async function bindEmulatorChild(childPid: number | null, io: ProbeIO = defaultIO,
  signal = new AbortController().signal): Promise<Binding> {
  const binding: Binding = { childPid, birth: null, processKind: 'unknown', status: 'spawn-error' };
  if (childPid === null) return binding;
  if (io.platform !== 'linux') return { ...binding, status: 'unsupported-os' };
  try {
    const stat = parseStat(await io.read(`/proc/${childPid}/stat`, STAT_LIMIT, signal), childPid);
    const bootIdSha256 = bootHash(await io.read(BOOT_FILE, 64, signal));
    try { binding.processKind = imageKind(await io.readlink(`/proc/${childPid}/exe`)); } catch { binding.processKind = 'unknown'; }
    checkTime(signal);
    return { ...binding, birth: { startTicks: stat.startTicks, bootIdSha256 }, status: 'bound' };
  } catch (error) { return { ...binding, status: unavailable(error) }; }
}
function keys(value: unknown, expected: string[]): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    && Object.keys(value).sort().join(',') === expected.sort().join(',');
}
function validTime(value: unknown): value is number { return Number.isSafeInteger(value) && (value as number) >= 0; }
function validPid(value: unknown): value is number { return Number.isSafeInteger(value) && (value as number) > 0 && (value as number) <= 2147483647; }
export function parseEmulatorLifetime(text: string): EmulatorLifetimeReceipt | undefined {
  if (Buffer.byteLength(text) > RECEIPT_LIMIT) return;
  try {
    const value = JSON.parse(text);
    if (!keys(value, value.exit === undefined ? ['schema', 'binding', 'bindingSha256'] : ['schema', 'binding', 'bindingSha256', 'exit'])
      || value.schema !== 1 || !keys(value.binding, ['childPid', 'birth', 'processKind', 'status'])) return;
    const b = value.binding;
    if (!(b.childPid === null || validPid(b.childPid)) || !['qemu', 'launcher', 'other', 'unknown'].includes(b.processKind as string)
      || !['bound', 'unsupported-os', 'missing', 'io-error', 'malformed', 'byte-limit', 'probe-timeout', 'spawn-error', 'exit-before-binding'].includes(b.status as string)) return;
    if (b.status === 'bound') {
      if (!validPid(b.childPid) || !keys(b.birth, ['startTicks', 'bootIdSha256'])
        || typeof b.birth.startTicks !== 'string' || !/^\d{1,40}$/u.test(b.birth.startTicks)
        || typeof b.birth.bootIdSha256 !== 'string' || !/^[a-f0-9]{64}$/u.test(b.birth.bootIdSha256)) return;
    } else if (b.birth !== null) return;
    if (typeof value.bindingSha256 !== 'string' || value.bindingSha256 !== emulatorBindingSha256(b as unknown as Binding)) return;
    if (value.exit !== undefined) {
      if (!keys(value.exit, ['observedAtMs', 'code', 'signal']) || !validTime(value.exit.observedAtMs)
        || !(value.exit.code === null || (Number.isInteger(value.exit.code) && (value.exit.code as number) >= 0 && (value.exit.code as number) <= 255))
        || !(value.exit.signal === null || (typeof value.exit.signal === 'string' && Object.hasOwn(osConstants.signals, value.exit.signal)))) return;
      if ((value.exit.code === null) === (value.exit.signal === null)) return;
    }
    return value as unknown as EmulatorLifetimeReceipt;
  } catch { return; }
}
export async function sampleHostEmulator(file: string, expectedPid: string | undefined, hostSettledAtMs: number,
  budgetMs: number, io: ProbeIO = defaultIO): Promise<HostEmulatorAtSettlement> {
  const sampleStartedAtMs = Date.now();
  const base = { hostSettledAtMs, sampleStartedAtMs };
  const controller = new AbortController();
  const deadline = performance.now() + Math.min(250, Math.max(0, budgetMs));
  const finish = (state: HostEmulatorAtSettlement['state'], reason: HostEmulatorAtSettlement['reason'],
    extra: Partial<HostEmulatorAtSettlement> = {}): HostEmulatorAtSettlement => {
    const expired = performance.now() > deadline;
    return { ...base, nativeExitCoverage: 'unknown', ...extra, sampleEndedAtMs: Date.now(),
      state: expired ? 'unknown' : state, reason: expired ? 'probe-timeout' : reason };
  };
  if (!(budgetMs > 0)) return finish('unknown', 'probe-timeout');
  let timer: ReturnType<typeof setTimeout> | undefined;
  const probe = async (): Promise<HostEmulatorAtSettlement> => {
    let receipt: EmulatorLifetimeReceipt | undefined;
    try {
      receipt = parseEmulatorLifetime(await io.read(file, RECEIPT_LIMIT, controller.signal));
      checkTime(controller.signal);
      if (!receipt || !expectedPid || String(receipt.binding.childPid) !== expectedPid) return finish('unknown', 'invalid-receipt');
      const extra = { bindingSha256: receipt.bindingSha256, processKind: receipt.binding.processKind,
        ...(receipt.exit ? { nativeExit: receipt.exit, nativeExitCoverage: 'observed' as const } : {}) };
      if (io.platform !== 'linux') return finish('unknown', 'unsupported-os', extra);
      if (!receipt.binding.birth) return finish('unknown', receipt.binding.status === 'bound' ? 'malformed' : receipt.binding.status, extra);
      const boot = bootHash(await io.read(BOOT_FILE, 64, controller.signal));
      if (boot !== receipt.binding.birth.bootIdSha256) return finish('unknown', 'boot-mismatch', extra);
      let stat;
      try { stat = parseStat(await io.read(`/proc/${receipt.binding.childPid}/stat`, STAT_LIMIT, controller.signal), receipt.binding.childPid!); }
      catch (error) { return finish(unavailable(error) === 'missing' ? 'gone' : 'unknown', unavailable(error) === 'missing' ? 'absent' : unavailable(error), extra); }
      checkTime(controller.signal);
      if (stat.startTicks !== receipt.binding.birth.startTicks) return finish('unknown', 'pid-reused', extra);
      if (['Z', 'X', 'x'].includes(stat.state)) return finish('gone', stat.state === 'Z' ? 'zombie' : 'exited', extra);
      return finish('alive', 'same-birth', extra);
    } catch (error) { return finish('unknown', unavailable(error)); }
  };
  try {
    return await Promise.race([probe(), new Promise<HostEmulatorAtSettlement>(resolve => {
      timer = setTimeout(() => { controller.abort(); resolve(finish('unknown', 'probe-timeout')); }, Math.min(250, budgetMs));
    })]);
  } finally { if (timer) clearTimeout(timer); controller.abort(); }
}
export async function observeEmulator(file: string, log: string, binary: string, args: string[], io: ProbeIO = defaultIO): Promise<void> {
  const logFd = openSync(log, 'w', 0o600);
  const child = spawn(binary, args, { stdio: ['ignore', logFd, logFd] });
  let exit: NativeExit | undefined;
  let spawnFailed = false;
  const publication: { publish?: () => void } = {};
  const ended = new Promise<void>(resolve => {
    child.once('exit', (code, signal) => { exit = { observedAtMs: Date.now(), code, signal }; publication.publish?.(); resolve(); });
    child.once('error', () => { spawnFailed = true; resolve(); });
  });
  closeSync(logFd);
  const binding = await bindEmulatorChild(child.pid ?? null, io);
  if (spawnFailed && child.pid === undefined) binding.status = 'spawn-error';
  if (exit) { binding.birth = null; binding.processKind = 'unknown'; binding.status = 'exit-before-binding'; }
  const bindingSha256 = emulatorBindingSha256(binding);
  let queue = Promise.resolve();
  publication.publish = () => {
    const contents = JSON.stringify({ schema: 1, binding, bindingSha256, ...(exit ? { exit } : {}) });
    queue = queue.then(async () => {
      await writeFile(`${file}.tmp`, contents, { mode: 0o600 });
      await rename(`${file}.tmp`, file);
    }).catch(() => { process.stderr.write('EMULATOR_RECEIPT_WRITE_FAILED\n'); });
  };
  publication.publish();
  await ended;
  await queue;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [file, log, binary, ...args] = process.argv.slice(2);
  if (!file || !log || !binary) process.stderr.write('EMULATOR_OBSERVER_ARGUMENTS_INVALID\n');
  else await observeEmulator(file, log, binary, args).catch(() => { process.stderr.write('EMULATOR_OBSERVER_UNAVAILABLE\n'); });
}

import { access, constants, open, readdir, readFile, readlink, realpath } from 'node:fs/promises';
import { basename, join, resolve } from 'node:path';
import { userInfo } from 'node:os';
import { fileURLToPath } from 'node:url';
import { command } from './process';
import { redactText, writeBoundedText, writeSanitizedJson } from './diagnostics';

const COMMAND_TIMEOUT_MS = 10_000;
const LINE_BYTES = 512;
const SERVER_LOG_BYTES = 256 * 1024;
const KERNEL_LINES = 64;
const kernelPattern = /segfault|general protection|traps:|out of memory|oom-kill|killed process|blocked for more than|kvm|qemu|emulator|adb/iu;

export interface AndroidHostSnapshotOptions {
  label: string;
  outputDir: string;
  procRoot?: string;
  serverLog?: string;
  kernelCommand?: [string, ...string[]];
}

function boundedLines(text: string, limit: number): string[] {
  return text.split(/\r?\n/u).filter(line => line.trim()).slice(-limit).map(line => redactText(line).slice(0, LINE_BYTES));
}

async function readSmall(path: string): Promise<string | undefined> {
  try { return (await readFile(path, 'utf8')).slice(0, 64 * 1024); } catch { return undefined; }
}

async function readTail(path: string, limit: number): Promise<{ text: string; bytes: number } | undefined> {
  let handle;
  try { handle = await open(path, 'r'); } catch { return undefined; }
  try {
    const { size } = await handle.stat();
    const length = Math.min(size, limit);
    const buffer = Buffer.alloc(length);
    await handle.read(buffer, 0, length, size - length);
    return { text: buffer.toString('utf8'), bytes: size };
  } finally { await handle.close(); }
}

async function executable(path: string): Promise<string | undefined> {
  try { await access(path, constants.X_OK); return await realpath(path); } catch { return undefined; }
}

async function pathAdb(): Promise<string | undefined> {
  for (const directory of (process.env.PATH || '').split(':').filter(Boolean)) {
    const found = await executable(join(directory, 'adb'));
    if (found) return found;
  }
}

export async function adbServers(procRoot = '/proc'): Promise<Array<{ pid: number; startTicks: string | null; exe: string | null; args: string }>> {
  const servers = [];
  for (const entry of await readdir(procRoot).catch(() => [] as string[])) {
    if (!/^\d+$/u.test(entry)) continue;
    const argv = (await readSmall(join(procRoot, entry, 'cmdline')))?.split('\0').filter(Boolean) ?? [];
    if (basename(argv[0] ?? '') !== 'adb' || !argv.includes('fork-server')) continue;
    const fields = /\) \S+ (.*)$/su.exec((await readSmall(join(procRoot, entry, 'stat'))) ?? '')?.[1].trim().split(/\s+/u);
    const exe = await readlink(join(procRoot, entry, 'exe')).catch(() => null);
    servers.push({ pid: Number(entry), startTicks: fields?.[18] ?? null, exe, args: redactText(argv.slice(1).join(' ')).slice(0, LINE_BYTES) });
    if (servers.length === 8) break;
  }
  return servers;
}

export async function captureAndroidHostSnapshot(options: AndroidHostSnapshotOptions): Promise<void> {
  if (!/^[a-z][a-z-]{0,31}$/u.test(options.label) || !options.outputDir) throw new Error('ANDROID_HOST_SNAPSHOT: invalid arguments');
  const procRoot = options.procRoot ?? '/proc';
  const failures: string[] = [];
  const run = async (name: string, binary: string, args: string[]) => {
    try {
      return await command(binary, args, COMMAND_TIMEOUT_MS, { maxBuffer: 4 * 1024 * 1024 });
    } catch (error) {
      failures.push(`${name}: ${error instanceof Error ? error.message : 'failed'}`.slice(0, LINE_BYTES));
    }
  };
  const adb = await pathAdb();
  const sdkAdb = process.env.ANDROID_HOME ? await executable(join(process.env.ANDROID_HOME, 'platform-tools', 'adb')) : undefined;
  const servers = await adbServers(procRoot);
  const version = adb ? await run('adb version', adb, ['version']) : undefined;
  // `adb devices` would start a server when none is running, so skip it rather than change the state being recorded.
  const devices = adb && servers.length ? await run('adb devices', adb, ['devices', '-l']) : undefined;
  const kernelCommand = options.kernelCommand ?? ['sudo', '-n', 'dmesg', '--time-format', 'iso'];
  const kernel = await run('kernel log', kernelCommand[0], kernelCommand.slice(1));
  const meminfo = (await readSmall(join(procRoot, 'meminfo'))) ?? '';
  const memory = Object.fromEntries(['MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree'].map(key =>
    [key, Number(new RegExp(`^${key}:\\s+(\\d+) kB$`, 'mu').exec(meminfo)?.[1] ?? NaN) || null]));
  const pressure = Object.fromEntries(await Promise.all(['cpu', 'memory', 'io'].map(async kind =>
    [kind, boundedLines((await readSmall(join(procRoot, 'pressure', kind))) ?? '', 2)])));
  const serverLogPath = options.serverLog ?? join(process.env.TMPDIR || '/tmp', `adb.${userInfo().uid}.log`);
  const serverLog = await readTail(serverLogPath, SERVER_LOG_BYTES);
  if (serverLog) await writeBoundedText(join(options.outputDir, `android-adb-server-${options.label}.log`), serverLog.text);
  await writeSanitizedJson(join(options.outputDir, `android-host-${options.label}.json`), {
    schema: 1,
    label: options.label,
    capturedAt: new Date().toISOString(),
    adb: { pathBinary: adb ?? null, sdkBinary: sdkAdb ?? null, sameBinary: Boolean(adb && adb === sdkAdb), version: version ? boundedLines(version.stdout, 8) : null },
    servers,
    devices: devices ? { stdout: boundedLines(devices.stdout, 16), stderr: boundedLines(devices.stderr, 16) } : null,
    serverLog: serverLog ? { bytes: serverLog.bytes, retainedBytes: Math.min(serverLog.bytes, SERVER_LOG_BYTES) } : 'missing',
    host: { loadavg: (await readSmall(join(procRoot, 'loadavg')))?.trim() ?? null, memoryKiB: memory, pressure },
    kernel: kernel ? boundedLines(kernel.stdout.split('\n').filter(line => kernelPattern.test(line)).join('\n'), KERNEL_LINES) : null,
    failures,
  });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [label, outputDir] = process.argv.slice(2);
  await captureAndroidHostSnapshot({ label: label ?? '', outputDir: outputDir ?? '' })
    .catch(() => { process.stderr.write('ANDROID_HOST_SNAPSHOT_UNAVAILABLE\n'); });
}

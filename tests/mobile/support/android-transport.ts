import { spawn } from 'node:child_process';
import { join } from 'node:path';
import { createConnection } from 'node:net';
import { redactText, writeSanitizedJson } from './diagnostics';

export async function transportCommand(file: string, args: string[], timeoutMs = 1500, limit = 16_384) {
  const startedAt = new Date().toISOString();
  return await new Promise<{
    startedAt: string; endedAt: string; stdout: string; stderr: string;
    exitCode: number | null; signal: string | null; timedOut: boolean; truncated: boolean; unavailable: boolean;
  }>((resolve) => {
    let stdout = Buffer.alloc(0);
    let stderr = Buffer.alloc(0);
    let truncated = false;
    let timedOut = false;
    let unavailable = false;
    let settled = false;
    let fallback: ReturnType<typeof setTimeout> | undefined;
    const child = spawn(file, args, { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, ADB_TRACE: '' } });
    const append = (current: Buffer, chunk: Buffer) => {
      if (current.length + chunk.length > limit) truncated = true;
      return Buffer.concat([current, chunk.subarray(0, Math.max(0, limit - current.length))]);
    };
    child.stdout.on('data', (chunk: Buffer) => { stdout = append(stdout, chunk); });
    child.stderr.on('data', (chunk: Buffer) => { stderr = append(stderr, chunk); });
    const finish = (exitCode: number | null, signal: string | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(fallback);
      child.stdout.destroy();
      child.stderr.destroy();
      resolve({ startedAt, endedAt: new Date().toISOString(), stdout: redactText(stdout.toString()),
        stderr: redactText(stderr.toString()), exitCode, signal, timedOut, truncated, unavailable });
    };
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill('SIGKILL');
      fallback = setTimeout(() => finish(null, 'SIGKILL'), 250);
    }, timeoutMs);
    child.on('error', () => { unavailable = true; finish(null, null); });
    child.on('close', finish);
  });
}

export async function transportAdbObservation(serial: string, service: 'get-state' | 'uptime', port = 5037, timeoutMs = 1500) {
  const startedAt = new Date().toISOString();
  return await new Promise<Awaited<ReturnType<typeof transportCommand>>>((resolve) => {
    let pending = Buffer.alloc(0);
    let output = Buffer.alloc(0);
    let stage = service === 'get-state' ? 'status' : 'transport';
    let expected: number | undefined;
    let settled = false;
    let truncated = false;
    const socket = createConnection({ host: '127.0.0.1', port });
    const finish = (unavailable: boolean, timedOut = false) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.destroy();
      resolve({ startedAt, endedAt: new Date().toISOString(), stdout: redactText(output.toString()),
        stderr: unavailable ? 'ADB observation unavailable' : '', exitCode: unavailable || service === 'uptime' ? null : 0,
        signal: null, unavailable, timedOut, truncated });
    };
    const timer = setTimeout(() => finish(true, true), timeoutMs);
    const request = (value: string) => {
      const bytes = Buffer.from(value);
      if (bytes.length > 65535) { finish(true); return; }
      socket.write(Buffer.concat([Buffer.from(bytes.length.toString(16).padStart(4, '0')), bytes]));
    };
    socket.on('connect', () => request(service === 'get-state'
      ? `host-serial:${serial}:get-state` : `host:transport:${serial}`));
    socket.on('data', (chunk: Buffer) => {
      pending = Buffer.concat([pending, chunk]);
      while (!settled) {
        if (stage === 'transport' || stage === 'status') {
          if (pending.length < 4) return;
          if (pending.subarray(0, 4).toString() !== 'OKAY') { finish(true); return; }
          pending = pending.subarray(4);
          if (stage === 'transport') {
            stage = 'status';
            request('shell:echo transport-observation; cat /proc/uptime');
          } else {
            stage = service === 'get-state' ? 'length' : 'output';
          }
          continue;
        }
        if (stage === 'length') {
          if (pending.length < 4) return;
          const length = pending.subarray(0, 4).toString();
          if (!/^[0-9a-f]{4}$/iu.test(length)) { finish(true); return; }
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
          if (expected === 0) finish(false);
        }
        return;
      }
    });
    socket.on('error', () => finish(true));
    socket.on('close', () => finish(stage !== 'output' || (expected !== undefined && expected !== 0)));
  });
}

export class AndroidTransportObservation {
  private pending = '';
  private task?: Promise<void>;
  failure?: string;

  constructor(private readonly serial: string, private readonly outputDir: string,
    private readonly run = transportCommand, private readonly runAdb = transportAdbObservation) {}

  observe(chunk: Buffer): void {
    if (this.task) return;
    this.pending += chunk.toString('utf8');
    const lines = this.pending.split('\n');
    this.pending = (lines.pop() || '').slice(-512);
    const signal = lines.find(line => /^\s*\d+\.\d+\s+\d+\s+\d+\s+[VDIWEF]\s+adbd\s*:\s*timeout expired while flushing socket, closing\s*$/u.test(line));
    if (!signal) return;
    const guestEpochSeconds = signal.trim().split(/\s+/u)[0];
    this.task = this.collect(guestEpochSeconds).catch(() => { this.failure = 'transport observation could not be saved'; });
  }

  async finish(): Promise<void> {
    await this.task;
  }

  private async collect(guestEpochSeconds: string): Promise<void> {
    const triggeredAt = new Date().toISOString();
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
        return { name, ...await (file === 'adb-socket'
          ? this.runAdb(this.serial, name === 'adb-state' ? 'get-state' : 'uptime') : this.run(file, args)) };
      } catch {
        return { name, startedAt, endedAt: new Date().toISOString(), unavailable: true,
          stdout: '', stderr: '', exitCode: null, signal: null };
      }
    }));
    await writeSanitizedJson(join(this.outputDir, 'android-transport-observation.json'), {
      trigger: 'guest-adbd-flush-timeout', guestEpochSeconds, triggeredAt, endedAt: new Date().toISOString(), observations,
    });
  }
}

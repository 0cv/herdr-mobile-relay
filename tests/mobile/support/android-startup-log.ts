import { createConnection } from 'node:net';
import { join } from 'node:path';
import { redactText, writeBoundedText, writeSanitizedJson } from './diagnostics';

async function writeBoundedBufferText(filename: string, value: Buffer, maximum: number): Promise<void> {
  const redacted = Buffer.from(redactText(value.toString('utf8'))).subarray(0, maximum).toString('utf8');
  await writeBoundedText(filename, redacted, maximum);
}

export class AndroidStartupLog {
  private startedAt?: string;
  private serial?: string;
  private endedAt?: string;
  private stdout: Buffer[] = [];
  private stderr: Buffer[] = [];
  private bytes = 0;
  private frames = 0;
  private pending = Buffer.alloc(0);
  private stage: 'transport' | 'status' | 'frames' | 'done' = 'transport';
  private exitCode: number | null = null;
  private error: string | null = null;
  private truncated = false;
  private socket?: ReturnType<typeof createConnection>;
  private handshake?: ReturnType<typeof setTimeout>;
  private lifetime?: ReturnType<typeof setTimeout>;
  private saved?: Promise<void>;
  private handshakePromise?: Promise<void>;
  private settleHandshakeCallback?: () => void;
  private handshakeSettled = false;

  constructor(private readonly outputDir: string,
    private readonly connect: typeof createConnection = createConnection,
    private readonly lifetimeMs = 30 * 60_000,
    private readonly handshakeMs = 5_000) {
    if (!Number.isInteger(lifetimeMs) || lifetimeMs <= 0 || lifetimeMs > 30 * 60_000
      || !Number.isInteger(handshakeMs) || handshakeMs <= 0 || handshakeMs > 5_000) throw new Error('Invalid diagnostic lifetime');
  }

  start(serial: string): void {
    if (this.socket || this.endedAt) throw new Error('Android startup diagnostic already started');
    this.handshakePromise = new Promise(resolve => { this.settleHandshakeCallback = resolve; });
    this.handshakeSettled = false;
    this.startedAt = new Date().toISOString();
    this.serial = serial;
    if (!/^emulator-\d{1,5}$/u.test(serial) || process.env.ADB_SERVER_SOCKET ||
        process.env.ANDROID_ADB_SERVER_ADDRESS || process.env.ANDROID_ADB_SERVER_PORT) {
      this.end('unsupported ADB endpoint');
      return;
    }
    this.handshake = setTimeout(() => this.end('handshake deadline'), this.handshakeMs);
    this.lifetime = setTimeout(() => this.end('lifetime limit'), this.lifetimeMs);
    try {
      this.socket = this.connect({ host: '127.0.0.1', port: 5037 });
    } catch {
      this.end('ADB socket unavailable');
      return;
    }
    const request = (value: string) => {
      const body = Buffer.from(value);
      this.socket?.write(Buffer.concat([Buffer.from(body.length.toString(16).padStart(4, '0')), body]));
    };
    this.socket.on('connect', () => request(`host:transport:${serial}`));
    this.socket.on('data', (chunk: Buffer) => {
      if (this.endedAt) return;
      if (this.pending.length + chunk.length > 1_048_581) { this.truncated = true; this.end('pending buffer limit'); return; }
      this.pending = Buffer.concat([this.pending, chunk]);
      while (this.pending.length && !this.endedAt) {
        if (this.stage === 'done') { this.end('trailing shell response'); return; }
        if (this.stage === 'transport' || this.stage === 'status') {
          if (this.pending.length < 4) return;
          if (this.pending.subarray(0, 4).toString('ascii') !== 'OKAY') { this.end('ADB service refused or malformed status'); return; }
          this.pending = this.pending.subarray(4);
          if (this.stage === 'transport') {
            if (this.pending.length) { this.end('premature shell response'); return; }
            this.stage = 'status';
            request('shell,v2,raw:logcat -b all -v threadtime');
            return;
          }
          this.stage = 'frames';
          clearTimeout(this.handshake);
          this.settleHandshake();
          continue;
        }
        if (this.pending.length < 5) return;
        const id = this.pending[0];
        const length = this.pending.readUInt32LE(1);
        if (![1, 2, 3].includes(id) || length > 1_048_576 || (id === 3 && length !== 1)) {
          this.end('malformed shell frame'); return;
        }
        if (this.pending.length < length + 5) return;
        if (++this.frames > 100_000 || this.bytes + length > 16_777_216) {
          this.truncated = true; this.end('output or frame limit'); return;
        }
        const body = Buffer.from(this.pending.subarray(5, length + 5));
        this.pending = this.pending.subarray(length + 5);
        if (id === 3) {
          this.exitCode = body[0];
          this.stage = 'done';
        } else {
          this.bytes += length;
          (id === 1 ? this.stdout : this.stderr).push(body);
        }
      }
    });
    this.socket.on('error', error => this.end(`ADB socket error: ${error.message}`));
    this.socket.on('close', () => this.end(this.stage === 'done' && !this.pending.length ? null : 'transport closed without complete exit frame'));
  }

  private settleHandshake(): void {
    if (this.handshakeSettled) return;
    this.handshakeSettled = true;
    this.settleHandshakeCallback?.();
  }

  waitForHandshake(): Promise<void> {
    return this.handshakePromise || Promise.resolve();
  }

  private end(error: string | null): void {
    if (this.endedAt) return;
    this.endedAt = new Date().toISOString();
    this.settleHandshake();
    this.error = error;
    clearTimeout(this.handshake);
    clearTimeout(this.lifetime);
    this.socket?.destroy();
    this.pending = Buffer.alloc(0);
  }

  finish(): Promise<void> {
    this.startedAt ||= new Date().toISOString();
    this.end('owner finalized diagnostic stream');
    this.saved ||= this.save();
    return this.saved;
  }

  private async save(): Promise<void> {
    await writeBoundedBufferText(join(this.outputDir, 'android-startup-logcat.log'), Buffer.concat(this.stdout), 16_777_216);
    await writeBoundedBufferText(join(this.outputDir, 'android-startup-logcat-stderr.log'), Buffer.concat(this.stderr), 16_777_216);
    await writeSanitizedJson(join(this.outputDir, 'android-startup-logcat.json'), {
      diagnosticOnly: true, exhaustiveCoverage: false, serial: this.serial, adbEndpoint: '127.0.0.1:5037',
      command: `host:transport:${this.serial}; shell,v2,raw:logcat -b all -v threadtime`,
      startedAt: this.startedAt, endedAt: this.endedAt, exitCode: this.exitCode, error: this.error,
      truncated: this.truncated, bytes: this.bytes, frames: this.frames,
    });
    this.stdout = [];
    this.stderr = [];
  }
}

import { spawn, type ChildProcess } from 'node:child_process';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { validateMeasurementIdentity, type MeasurementIdentity } from './support/mobile-result';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { ANDROID_LOG_LIMIT, androidMeasurementMarker, parseAndroidProcesses, assessAndroidEnvironment, readEnvironmentInputs, type AndroidEnvironmentCheck, type AndroidPlannedTermination } from './android-environment';
import { command, stopProcess } from './support/process';
import { redactText, writeSanitizedJson } from './support/diagnostics';
import { requireOwnedDevice } from './support/device';
import { isAndroidPackageProcess, isAndroidTerminationPackage } from './android-events';
import type { AppiumClient } from './support/webdriver';
import { AndroidTransportObservation } from './support/android-transport';

export interface AndroidMeasurementOutcome {
  assessment?: AndroidEnvironmentCheck;
  assessmentSha256?: string;
  errors: unknown[];
}

export interface AndroidMeasurementIO {
  command: typeof command;
  requireOwnedDevice: typeof requireOwnedDevice;
  mkdir: typeof mkdir;
  writeFile: typeof writeFile;
  readFile: typeof readFile;
  writeJson: typeof writeSanitizedJson;
  stopProcess: typeof stopProcess;
  spawnCollector: () => ChildProcess;
  transport: Pick<AndroidTransportObservation, 'observe' | 'finish' | 'failure'>;
}

export class AndroidEnvironmentMeasurement {
  readonly id = randomUUID();
  private collector?: ChildProcess;
  private chunks: Buffer[] = [];
  private bytes = 0;
  private collectorFailure?: Error;
  private active = false;
  private started = false;
  private readonly operations: AndroidPlannedTermination[] = [];
  private identity?: MeasurementIdentity;
  private finished = false;
  private readonly errors: unknown[] = [];

  bind(identity: MeasurementIdentity): void {
    if (this.identity || this.started || identity.measurementId !== this.id) throw new Error('ANDROID_ENVIRONMENT: identity binding must occur once before begin');
    validateMeasurementIdentity(identity);
    this.identity = structuredClone(identity);
  }

  plannedOperations(): AndroidPlannedTermination[] { return structuredClone(this.operations); }

  private async attempt(operation: () => Promise<unknown>): Promise<void> {
    try { await operation(); } catch (error) { if (!this.errors.includes(error)) this.errors.push(error); }
  }

  private readonly transport: AndroidMeasurementIO['transport'];
  private readonly io: AndroidMeasurementIO;

  constructor(private readonly serial: string, private readonly outputDir: string, private readonly toolchains: string, io: Partial<AndroidMeasurementIO> = {}) {
    this.io = { command, requireOwnedDevice, mkdir, writeFile, readFile, writeJson: writeSanitizedJson, stopProcess,
      spawnCollector: () => spawn('adb', ['-s', serial, 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1'], { stdio: ['ignore', 'pipe', 'pipe'] }),
      transport: new AndroidTransportObservation(serial, outputDir), ...io };
    this.transport = this.io.transport;
  }

  failureOutcome(): AndroidMeasurementOutcome { return { errors: [...this.errors] }; }

  private path(name: string): string {
    return join(this.outputDir, `android-environment-${name}.json`);
  }

  private async snapshot(boundary: 'start' | 'end'): Promise<void> {
    const name = boundary === 'start' ? 'before' : 'after';
    await this.io.command(process.execPath, [fileURLToPath(new URL('./android-environment.ts', import.meta.url)), 'snapshot',
      '--serial', this.serial, '--toolchains', this.toolchains, '--output', this.path(name),
      '--boundary', boundary, '--measurement', this.id], 180_000);
  }

  async begin(): Promise<void> {
    if (this.started) throw new Error('ANDROID_ENVIRONMENT: rebaseline is forbidden');
    this.started = true;
    try {
      await this.io.requireOwnedDevice('android', this.serial);
      await this.io.mkdir(this.outputDir, { recursive: true });
      await this.io.writeFile(this.path('session'), `${JSON.stringify({ id: this.id })}\n`, { flag: 'wx', mode: 0o600 });
      await this.io.writeJson(this.path('operations'), this.operations);
      this.collector = this.io.spawnCollector();
      this.collector.on('error', (error) => { this.collectorFailure ||= error; });
      this.collector.on('exit', () => {
        if (this.active) this.collectorFailure ||= new Error('ANDROID_ENVIRONMENT: log collector exited inside measurement');
      });
      this.collector.stderr!.on('data', () => { this.collectorFailure ||= new Error('ANDROID_ENVIRONMENT: log collector reported stderr'); });
      this.collector.stdout!.on('data', (chunk: Buffer) => {
        try { this.transport.observe(chunk); } catch (error) { this.collectorFailure ||= error instanceof Error ? error : new Error('ANDROID_ENVIRONMENT: transport observation failed'); }
        this.bytes += chunk.length;
        if (this.bytes > ANDROID_LOG_LIMIT) {
          this.collectorFailure ||= new Error('ANDROID_ENVIRONMENT: measured log exceeds bound');
          this.collector?.kill('SIGTERM');
          return;
        }
        this.chunks.push(chunk);
      });
      this.active = true;
      await this.snapshot('start');
      await this.waitForMarker('START');
    } catch (error) {
      this.errors.push(error);
      await this.closeCollector();
      await this.attempt(() => this.io.writeJson(this.path('completion'), { passed: false, errors: this.errors.map(error => error instanceof Error ? error.message : String(error)) }));
      throw error;
    }
  }

  private async waitForMarker(marker: string): Promise<void> {
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      if (this.collectorFailure) throw this.collectorFailure;
      if (Buffer.concat(this.chunks).toString('utf8').includes(`: ${this.id} ${marker}\n`)) return;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    throw new Error(`ANDROID_ENVIRONMENT: collector did not read ${marker} marker`);
  }

  async terminate(packageName: string, pid: string): Promise<void> {
    if (!this.active || this.collectorFailure) throw this.collectorFailure || new Error('ANDROID_ENVIRONMENT: termination is outside measurement');
    if (!isAndroidTerminationPackage(packageName) || !/^[1-9]\d*$/u.test(pid)) throw new Error('ANDROID_ENVIRONMENT: invalid planned termination target');
    await requireOwnedDevice('android', this.serial);
    const current = await command('adb', ['-s', this.serial, 'shell', 'pidof', packageName]);
    if (current.stderr.trim() || current.stdout.trim() !== pid) throw new Error('ANDROID_ENVIRONMENT: planned termination PID changed');
    const observed = await command('adb', ['-s', this.serial, 'shell', 'ps', '-A', '-o', 'PID,NAME'], 5000);
    const inventory = parseAndroidProcesses(observed.stdout);
    if (observed.stderr.trim() || inventory[pid] !== packageName) throw new Error('ANDROID_ENVIRONMENT: planned termination process inventory changed');
    const affected = Object.fromEntries(Object.entries(inventory).filter(([, name]) => isAndroidPackageProcess(name, packageName)));
    const operation: AndroidPlannedTermination = {
      id: randomUUID(), measurementId: this.id, packageName, pid, processes: affected,
      command: ['shell', 'am', 'force-stop', '--user', '0', packageName], succeeded: false,
    };
    this.operations.push(operation);
    await writeSanitizedJson(this.path('operations'), this.operations);
    await androidMeasurementMarker(this.serial, `${this.id} OP_BEGIN ${operation.id} ${packageName} ${pid}`);
    const stopped = await command('adb', ['-s', this.serial, ...operation.command], 20_000);
    if (stopped.stdout.trim() || stopped.stderr.trim()) throw new Error('ANDROID_ENVIRONMENT: force-stop returned unexpected output');
    const remaining = await command('adb', ['-s', this.serial, 'shell', 'ps', '-A', '-o', 'PID,NAME'], 5000);
    const processes = parseAndroidProcesses(remaining.stdout);
    if (remaining.stderr.trim() || Object.keys(affected).some((targetPid) => processes[targetPid])
      || Object.values(processes).some((name) => isAndroidPackageProcess(name, packageName))) {
      throw new Error('ANDROID_ENVIRONMENT: planned termination did not remove the observed package process set');
    }
    await androidMeasurementMarker(this.serial, `${this.id} OP_END ${operation.id} ${packageName} ${pid}`);
    operation.succeeded = true;
    await writeSanitizedJson(this.path('operations'), this.operations);
  }

  async observeBootstrapClose(driver: AppiumClient): Promise<void> {
    const id = randomUUID();
    const observations: Record<string, unknown> = {
      id, measurementId: this.id, action: 'Appium session DELETE',
      ownership: 'Driver session close requested; browser termination ownership and initiator command identity are not established',
      qualifiesPlannedTermination: false,
    };
    const observe = async (boundary: 'BEGIN' | 'END') => {
      try {
        if (!this.active) throw new Error('measurement inactive');
        const inventory = await command('adb', ['-s', this.serial, 'shell', 'ps', '-A', '-o', 'PID,NAME'], 2_000);
        if (inventory.stderr.trim()) throw new Error('process inventory reported stderr');
        observations[`${boundary}Processes`] = parseAndroidProcesses(inventory.stdout);
        const marker = await command('adb', ['-s', this.serial, 'shell', 'log', '-p', 'i', '-t', 'HerdrMeasure',
          `'${this.id} CLOSE_${boundary} ${id}'`], 2_000);
        if (marker.stdout.trim() || marker.stderr.trim()) throw new Error('clock marker returned unexpected output');
        observations[`${boundary}MarkerSettled`] = true;
      } catch {
        observations[`${boundary}ObservationFailed`] = true;
      }
    };
    const before = driver.snapshot();
    observations.sessionPresentBefore = Boolean(before.sessionId);
    observations.unusableBefore = before.unusable;
    await observe('BEGIN');
    try {
      await driver.close();
      observations.closeResolved = true;
    } finally {
      const after = driver.snapshot();
      observations.sessionPresentAfter = Boolean(after.sessionId);
      observations.unusableAfter = after.unusable;
      observations.fatalCode = after.firstFatal?.code;
      observations.deleteCommands = after.commands.filter(entry => !before.commands.includes(entry) && entry.method === 'DELETE').map(entry => ({
        method: entry.method, failed: Boolean(entry.error), timedOut: entry.timedOut, durationMs: entry.durationMs,
      }));
      await observe('END');
      await writeSanitizedJson(this.path('bootstrap-close'), observations).catch(() => undefined);
    }
  }

  private async closeCollector(): Promise<void> {
    this.active = false;
    if (this.collector) await this.attempt(() => this.io.stopProcess(this.collector!));
    await this.attempt(() => this.transport.finish());
    let persistedBytes = 0;
    await this.attempt(async () => {
      const raw = Buffer.concat(this.chunks);
      const text = raw.toString('utf8');
      if (!Buffer.from(text, 'utf8').equals(raw)) throw new Error('ANDROID_ENVIRONMENT: invalid collector UTF-8');
      const persisted = Buffer.from(redactText(text), 'utf8');
      await this.io.writeFile(join(this.outputDir, 'android-qualification-logcat.log'), persisted, { mode: 0o600 });
      persistedBytes = persisted.length;
    });
    await this.attempt(() => this.io.writeJson(this.path('collector'), {
      bytes: persistedBytes, acquiredBytes: this.bytes, failure: this.collectorFailure?.message || (this.errors.length ? 'COLLECTION_FAILED' : undefined), transportObservationFailure: this.transport.failure,
    }));
    this.chunks = [];
  }

  async finish(): Promise<AndroidMeasurementOutcome> {
    if (this.finished) throw new Error('ANDROID_ENVIRONMENT: measurement already finalized');
    this.finished = true;
    if (this.active) {
      await this.attempt(() => this.snapshot('end'));
      await this.attempt(() => this.waitForMarker('END'));
    } else this.errors.push(new Error('ANDROID_ENVIRONMENT: no active measurement to finish'));
    await this.closeCollector();
    if (this.collectorFailure && !this.errors.includes(this.collectorFailure)) this.errors.push(this.collectorFailure);
    let assessment: AndroidEnvironmentCheck | undefined;
    let assessmentSha256: string | undefined;
    await this.attempt(async () => {
      if (!this.identity) throw new Error('ANDROID_ENVIRONMENT: measurement identity missing');
      await this.io.writeJson(this.path('identity'), this.identity);
    });
    await this.attempt(async () => {
      if (!this.identity) throw new Error('ANDROID_ENVIRONMENT: measurement identity missing');
      assessment = assessAndroidEnvironment(await readEnvironmentInputs(this.outputDir), this.identity);
      await this.io.writeJson(this.path('check'), assessment);
      const bytes = await this.io.readFile(this.path('check'));
      assessmentSha256 = createHash('sha256').update(bytes).digest('hex');
      assessment = JSON.parse(bytes.toString('utf8')) as AndroidEnvironmentCheck;
      if (assessment.collection.status !== 'PASS') throw new Error('ANDROID_ENVIRONMENT: collection guarantee failed');
    });
    await this.attempt(() => this.io.writeJson(this.path('completion'), {
      passed: this.errors.length === 0, errors: this.errors.map(error => error instanceof Error ? error.message : String(error)), assessmentSha256,
    }));
    return { assessment: assessment ? structuredClone(assessment) : undefined, assessmentSha256, errors: [...this.errors] };
  }
}

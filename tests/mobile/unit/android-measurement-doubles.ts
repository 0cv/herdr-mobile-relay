import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import type { AndroidMeasurementIO } from '../android-measurement';
import { sanitizeValue } from '../support/diagnostics';
import { AndroidTransportObservation, transportAdbObservation, transportCommand, type AndroidTransportTrigger } from '../support/android-transport';
import { boundedCommand, type BoundedCommandResult } from '../support/bounded-process';

export interface AndroidTransportFixture {
  adbPort: number;
  hostCommandScript: string;
  stateFile: string;
  acquisitionLogFile: string;
  commandPidFile: string;
  descendantPidFile: string;
  lateWriteFile: string;
}

export function transportFixtureIO(fixture: AndroidTransportFixture, io: Partial<AndroidMeasurementIO>): Partial<AndroidMeasurementIO> {
  const run = io.boundedCommand || boundedCommand;
  return {
    ...io,
    transportAdbObservation: (serial, service, _port, timeout, signal) => transportAdbObservation(serial, service, fixture.adbPort, timeout, signal),
    boundedCommand: (binary, args, timeout, options) => {
      if (!args.includes('--android-transport-observation-trigger')) return run(binary, args, timeout, options);
      return run(binary, [fileURLToPath(import.meta.url), '--android-transport-observation-trigger'], timeout, {
        ...options, input: JSON.stringify({ ...JSON.parse(String(options.input)), fixture }),
      });
    },
  };
}

const sha256 = (value: string | Buffer) => createHash('sha256').update(value).digest('hex');

function completedCommand(stdout: string): BoundedCommandResult {
  const bytes = Buffer.byteLength(stdout);
  return { stdout, stderr: '', code: 0, durationMs: 1, timedOut: false, outputLimitExceeded: false, aborted: false,
    childExited: true, targetPID: 1, targetCloseObserved: true, ownedProcessesExited: true, stdioClosed: true,
    stdoutBytes: bytes, stderrBytes: 0, stdoutRetainedBytes: bytes, stderrRetainedBytes: 0,
    stdoutSha256: sha256(stdout), stderrSha256: sha256('') };
}

export function inProcessOwnedPersistenceIO(fixtureDirectory: string, io: Partial<AndroidMeasurementIO>): Partial<AndroidMeasurementIO> {
  const run = io.boundedCommand || boundedCommand;
  return {
    ...io,
    boundedCommand: async (binary, args, timeout, options) => {
      if (args.includes('--gms-diagnostic-ownership')) return completedCommand('OWNED\n');
      if (binary === 'adb' && args.includes('dumpsys')) return completedCommand(await readFile(join(fixtureDirectory, 'valid-gms.dump'), 'utf8'));
      if (args.includes('--eval')) {
        const input = String(options.input);
        const startedAt = process.hrtime.bigint();
        const { parseAndroidGmsComponentState } = await import('../android-environment');
        const state = parseAndroidGmsComponentState(input);
        return completedCommand([`PARSER_STARTED ${startedAt}`, `PARSER_READY ${startedAt + 1n}`,
          `PARSER_INPUT ${Buffer.byteLength(input)} ${sha256(input)}`, JSON.stringify(state), `PARSER_OUTPUT ${startedAt + 2n}`, ''].join('\n'));
      }
      if (args.includes('--gms-diagnostic-persist')) {
        const request = JSON.parse(String(options.input));
        const contents = `${JSON.stringify(sanitizeValue(request.snapshot), null, 2)}\n`;
        await mkdir(dirname(request.target), { recursive: true });
        await writeFile(request.target, contents, { mode: 0o600 });
        return completedCommand(`${JSON.stringify({ byteLength: Buffer.byteLength(contents), sha256: sha256(contents) })}\n`);
      }
      if (args.includes('--android-measurement-persist-json') || args.includes('--android-measurement-persist-text')) {
        const request = JSON.parse(String(options.input));
        await mkdir(dirname(request.target), { recursive: true });
        await writeFile(request.target, request.contents, { mode: 0o600 });
        return completedCommand(`${JSON.stringify({ byteLength: request.expectedBytes, sha256: request.expectedSha256 })}\n`);
      }
      return run(binary, args, timeout, options);
    },
  };
}

export function fixtureTransportObservation(serial: string, output: string, fixture: AndroidTransportFixture,
  run: typeof transportCommand = transportCommand): AndroidTransportObservation {
  const names: Record<string, string> = { ps: 'host-processes', cat: 'host-memory', ss: 'host-tcp' };
  return new AndroidTransportObservation(serial, output,
    (file, _args, timeout, limit, signal, diagnostics) => run('/bin/sh', [fixture.hostCommandScript, fixture.stateFile,
      fixture.acquisitionLogFile, fixture.commandPidFile, fixture.descendantPidFile, fixture.lateWriteFile, names[file]], timeout, limit, signal, diagnostics),
    (device, service, _port, timeout, signal) => transportAdbObservation(device, service, fixture.adbPort, timeout, signal));
}

async function run(): Promise<void> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) chunks.push(Buffer.from(chunk));
  const input = Buffer.concat(chunks);
  if (process.argv[2] === '--android-transport-observation-trigger') {
    const request = JSON.parse(input.toString()) as AndroidTransportTrigger & { serial: string; outputDir: string; fixture: AndroidTransportFixture };
    const observation = fixtureTransportObservation(request.serial, request.outputDir, request.fixture);
    await observation.observeTrigger(request);
    process.stdout.write(`${JSON.stringify({ failure: observation.failure })}\n`);
    return;
  }
  const worker = fileURLToPath(new URL('../android-measurement.ts', import.meta.url));
  const result = spawnSync(process.execPath, [worker, process.argv[2]], { input, encoding: 'utf8', timeout: 10_000 });
  if (result.error || result.status !== 0) throw result.error || new Error(result.stderr);
  process.stderr.write(result.stderr);
  process.on('SIGTERM', () => undefined);
  setInterval(() => undefined, 1_000);
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  void run().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });
}

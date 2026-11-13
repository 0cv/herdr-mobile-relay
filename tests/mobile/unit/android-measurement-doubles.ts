import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import type { AndroidMeasurementIO } from '../android-measurement';
import { AndroidTransportObservation, transportAdbObservation, transportCommand, type AndroidTransportTrigger } from '../support/android-transport';
import { boundedCommand } from '../support/bounded-process';

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

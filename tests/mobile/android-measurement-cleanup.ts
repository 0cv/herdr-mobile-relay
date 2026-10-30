import { existsSync, rmSync } from 'node:fs';
import { basename, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { redactText } from './support/diagnostics';

export const ANDROID_GMS_OBSERVATION_FILE = 'android-environment-gms-observations.json';
const ANDROID_COLLECTOR_FILE = 'android-environment-collector.json';
const CLEANUP_REQUEST_LIMIT = 4_096;

export function checkedTemporaryFile(directory: string, target: string, temporary: string): string {
  const resolvedDirectory = resolve(directory);
  const resolvedTarget = resolve(target);
  const resolvedTemporary = resolve(temporary);
  if (dirname(resolvedTarget) !== resolvedDirectory || dirname(resolvedTemporary) !== resolvedDirectory
    || !basename(resolvedTemporary).startsWith(`.${basename(resolvedTarget)}.`)
    || !/^[a-f0-9-]{36}\.pending$/u.test(basename(resolvedTemporary).slice(basename(resolvedTarget).length + 2))) {
    throw new Error('Android measurement persistence paths are malformed');
  }
  return resolvedTemporary;
}

async function readCleanupRequest(): Promise<{ directory?: string; target?: string; temporary?: string }> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const chunk of process.stdin) {
    const value = Buffer.from(chunk);
    bytes += value.byteLength;
    if (bytes > CLEANUP_REQUEST_LIMIT) throw new Error('GMS diagnostic input exceeds its byte bound');
    chunks.push(value);
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

async function invalidateCollector(): Promise<void> {
  const request = await readCleanupRequest();
  if (!request.directory || !request.target || dirname(resolve(request.target)) !== resolve(request.directory)
    || basename(resolve(request.target)) !== ANDROID_COLLECTOR_FILE) {
    throw new Error('Android collector invalidation request is malformed');
  }
  rmSync(resolve(request.target), { force: true });
  process.stdout.write(`${JSON.stringify({ removed: !existsSync(resolve(request.target)) })}\n`);
}

async function cleanupMeasurementFile(): Promise<void> {
  const request = await readCleanupRequest();
  if (!request.directory || !request.target || !request.temporary) throw new Error('Android measurement cleanup request is malformed');
  const temporary = checkedTemporaryFile(request.directory, request.target, request.temporary);
  rmSync(temporary, { force: true });
  process.stdout.write('{"removed":true}\n');
}

async function cleanupGmsDiagnostic(): Promise<void> {
  const request = await readCleanupRequest();
  if (!request.directory || !request.target || !request.temporary || basename(resolve(request.target)) !== ANDROID_GMS_OBSERVATION_FILE
    || dirname(resolve(request.target)) !== resolve(request.directory)) {
    throw new Error('GMS cleanup request is malformed');
  }
  const temporary = checkedTemporaryFile(request.directory, request.target, request.temporary);
  rmSync(temporary, { force: true });
  process.stdout.write('{"removed":true}\n');
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  const modes: Record<string, () => Promise<void>> = {
    '--android-measurement-invalidate-collector': invalidateCollector,
    '--android-measurement-cleanup-file': cleanupMeasurementFile,
    '--gms-diagnostic-cleanup': cleanupGmsDiagnostic,
  };
  const operation = modes[process.argv[2]];
  if (operation) {
    void operation().catch((error: unknown) => {
      const message = error instanceof Error ? error.message : 'GMS diagnostic operation failed';
      process.stderr.write(`${redactText(message).slice(0, 500)}\n`);
      process.exitCode = 1;
    });
  }
}

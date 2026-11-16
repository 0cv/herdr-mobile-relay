import { spawnSync, type SpawnSyncReturns } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { dirname, join, resolve, basename } from 'node:path';
import { createHash } from 'node:crypto';
import {
  runnerId,
  diagnosticSource,
  type DiagnosticContext,
  type ProcessEvidence,
  type PartialProcessEvidence,
  type RunnerPathShape,
  type RunnerMatchEvidence,
} from './ios-xctest-diagnostics';

export const maxProcessCommandMs = 2_000;
export const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu;

export function deadlineTimeout(deadline: number): number {
  const timeout = deadline - Date.now();
  if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('XCTEST: startup deadline');
  return timeout;
}

export function executeSync(file: string, args: string[], timeout: number, maxBuffer: number, context?: DiagnosticContext): SpawnSyncReturns<string> {
  if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('XCTEST: startup deadline');
  const startedAt = context?.recorder ? new Date().toISOString() : '';
  const monotonicStartMs = context?.recorder?.now() || 0;
  try {
    const result = spawnSync(file, args, {encoding: 'utf8', timeout, maxBuffer}) as SpawnSyncReturns<string>;
    if (context?.recorder) context.recorder.command(file, args, timeout, startedAt, monotonicStartMs, result, undefined, context);
    return result;
  } catch (error) {
    if (context?.recorder) context.recorder.command(file, args, timeout, startedAt, monotonicStartMs, undefined, error, context);
    throw error;
  }
}

export function run(file: string, args: string[], timeout = 10_000, context?: DiagnosticContext): string {
  const result = executeSync(file, args, timeout, 4_194_304, context);
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
  if (result.error || result.status !== 0) throw new Error(`XCTEST: ${file} failed (${result.status ?? 'unknown'})`);
  return result.stdout.trim();
}

export function listeners(port: number, timeout = maxProcessCommandMs, observed?: (status: number | null, stderrBytes: number) => void, context?: DiagnosticContext): string[] {
  if (!Number.isInteger(port) || port < 1 || port > 65535 || timeout <= 0) throw new Error('XCTEST: startup deadline');
  const result = executeSync('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN', '-t'], timeout, 65_536, context ? {...context, port} : undefined);
  observed?.(result.status, Buffer.byteLength(result.stderr || ''));
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
  if (result.error || (result.status !== 0 && result.status !== 1)) throw new Error('XCTEST: cannot inspect endpoint ownership');
  const values = [...new Set(result.stdout.trim().split(/\s+/u).filter(Boolean))];
  if (values.some(value => !/^[1-9]\d{0,9}$/u.test(value) || Number(value) > 2_147_483_647)) throw new Error('XCTEST: endpoint returned an invalid process identifier');
  return values;
}

function processExecutable(pid: string, deadline: number, context?: DiagnosticContext): string {
  const output = run('lsof', ['-nP', '-a', '-p', pid, '-d', 'txt', '-Fn'], deadlineTimeout(deadline), context ? {
    ...context,
    source: diagnosticSource,
    operation: 'inspect-process-executable',
    pid,
  } : undefined);
  const executable = output.split(/\r?\n/u).find(line => line.startsWith('n'))?.slice(1);
  if (!executable || !executable.startsWith('/')) throw new Error('XCTEST: process executable is unavailable');
  return realpathSync(executable);
}

export function processEvidence(pid: string, deadline: number, context?: DiagnosticContext): ProcessEvidence {
  const processContext = context ? {...context, source: diagnosticSource, pid} : undefined;
  const executable = processExecutable(pid, deadline, processContext);
  const command = run('ps', ['-p', pid, '-o', 'command='], deadlineTimeout(deadline), processContext ? {
    ...processContext,
    operation: 'inspect-process-command',
  } : undefined);
  return {
    pid,
    executable,
    command: command.includes('test-without-building') && command.includes('-xctestrun') && command.includes('-destination')
      ? '[expected XCTest process]'
      : '[unrecognized process command]',
    birth: run('ps', ['-p', pid, '-o', 'lstart='], deadlineTimeout(deadline), processContext ? {
      ...processContext,
      operation: 'inspect-process-birth',
    } : undefined),
  };
}

export function diagnosticProcessEvidence(pid: string, deadline: number, context?: DiagnosticContext): {evidence: PartialProcessEvidence; error?: unknown} {
  const processContext = context ? {...context, source: diagnosticSource, pid} : undefined;
  const evidence: PartialProcessEvidence = {pid};
  try {
    evidence.executable = processExecutable(pid, deadline, processContext);
    run('ps', ['-p', pid, '-o', 'command='], deadlineTimeout(deadline), processContext ? {
      ...processContext,
      operation: 'inspect-process-command',
    } : undefined);
    evidence.birth = run('ps', ['-p', pid, '-o', 'lstart='], deadlineTimeout(deadline), processContext ? {
      ...processContext,
      operation: 'inspect-process-birth',
    } : undefined);
    return {evidence};
  } catch (error) {
    return {evidence, error};
  }
}

export function commandContainsPath(command: string, path: string): boolean {
  const resolved = resolve(path);
  const variants = [resolved];
  if (resolved.startsWith('/private/')) variants.push(resolved.slice('/private'.length));
  if (resolved.startsWith('/var/')) variants.push(`/private${resolved}`);
  return variants.some(variant => command.includes(variant));
}

export function runnerExecutableMatches(executable: string, product: string, udid: string, deadline: number, productBinaryHash?: string, evaluation?: RunnerMatchEvidence, context?: DiagnosticContext, boundedHash = false): boolean {
  try {
    const hash = (path: string): string => boundedHash ? boundedBinaryHash(path, deadline, context) : binaryHash(path);
    const resolved = realpathSync(executable);
    const app = dirname(resolved);
    const container = dirname(app);
    const applicationDirectory = dirname(container);
    const expectedApplicationDirectory = `/Devices/${udid}/data/Containers/Bundle/Application`;
    const productPath = realpathSync(product);
    const pathShape: RunnerPathShape = {};
    const pathChecks: Array<[keyof RunnerPathShape, () => boolean, string]> = [
      ['applicationNameMatches', () => basename(app) === basename(productPath), 'runner-application-name'],
      ['containerUuid', () => uuidPattern.test(basename(container)), 'runner-container-uuid'],
      ['simulatorPath', () => applicationDirectory.endsWith(expectedApplicationDirectory), 'runner-simulator-path'],
      ['executableName', () => basename(resolved) === 'WebDriverAgentRunner-Runner', 'runner-executable-name'],
    ];
    for (const [key, check, stage] of pathChecks) {
      pathShape[key] = check();
      if (evaluation) evaluation.pathShape = {...pathShape};
      if (!pathShape[key]) {
        if (evaluation) { evaluation.failureStage = stage; evaluation.errorCategory = 'runner-path-mismatch'; }
        return false;
      }
    }
    let bundleIdentifier: string;
    try {
      bundleIdentifier = run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(app, 'Info.plist')], deadlineTimeout(deadline), context ? {
        ...context,
        source: diagnosticSource,
        operation: 'inspect-runner-bundle-id',
      } : undefined);
    } catch (error) {
      if (evaluation) { evaluation.bundleId = {status: 'error'}; evaluation.failureStage = 'runner-bundle-id-command'; evaluation.errorCategory = 'runner-command-error'; }
      throw error;
    }
    const bundleMatches = bundleIdentifier === runnerId;
    if (evaluation) evaluation.bundleId = {status: 'evaluated', matches: bundleMatches};
    if (!bundleMatches) {
      if (evaluation) { evaluation.failureStage = 'runner-bundle-id'; evaluation.errorCategory = 'runner-bundle-mismatch'; }
      return false;
    }
    if (evaluation) evaluation.executableHash = {status: 'not-evaluated', ...(productBinaryHash ? {expected: productBinaryHash} : {})};
    let actualHash: string;
    try {
      actualHash = hash(resolved);
    } catch (error) {
      if (evaluation) { evaluation.executableHash = {status: 'error', ...(productBinaryHash ? {expected: productBinaryHash} : {})}; evaluation.failureStage = 'runner-executable-hash-command'; evaluation.errorCategory = 'runner-command-error'; }
      throw error;
    }
    let expectedHash: string;
    try {
      expectedHash = productBinaryHash || hash(join(productPath, 'WebDriverAgentRunner-Runner'));
    } catch (error) {
      if (evaluation) {
        evaluation.executableHash = {status: 'error', actual: actualHash, ...(productBinaryHash ? {expected: productBinaryHash} : {})};
        evaluation.failureStage = 'runner-product-hash-command';
        evaluation.errorCategory = 'runner-command-error';
      }
      throw error;
    }
    const hashMatches = actualHash === expectedHash;
    if (evaluation) evaluation.executableHash = {status: 'evaluated', expected: expectedHash, actual: actualHash, matches: hashMatches};
    if (!hashMatches && evaluation) { evaluation.failureStage = 'runner-executable-hash'; evaluation.errorCategory = 'runner-hash-mismatch'; }
    return hashMatches;
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') throw error;
    if (evaluation && !evaluation.errorCategory) {
      evaluation.failureStage ||= 'runner-executable-observation';
      evaluation.errorCategory = 'runner-observation-error';
    }
    return false;
  }
}

export function binaryHash(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

export function boundedBinaryHash(path: string, deadline: number, context?: DiagnosticContext): string {
  const script = "const{createHash}=require('node:crypto');const{readFileSync}=require('node:fs');process.stdout.write(createHash('sha256').update(readFileSync(process.argv[1])).digest('hex'));";
  const hash = run(process.execPath, ['-e', script, path], deadlineTimeout(deadline), context);
  if (!/^[0-9a-f]{64}$/u.test(hash)) throw new Error('XCTEST: binary hash is unavailable');
  return hash;
}

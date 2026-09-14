import { spawn, spawnSync, type ChildProcess, type SpawnSyncReturns } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve, basename } from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { StringDecoder } from 'node:string_decoder';
import { redactText, sanitizeValue } from './diagnostics';

const runnerId = 'com.facebook.WebDriverAgentRunner.xctrunner';
const productId = 'com.facebook.WebDriverAgentRunner';
const version = '16.12.1';
const maxStartupMs = 300_000;
const maxStartupLogBytes = 100 * 1024 * 1024;
const startupOutputMarker = '[startup output suppressed]\n';
const maxStatusBytes = 65_536;
const maxProcessCommandMs = 2_000;
const maxRunnerCleanupMs = 10_000;
const maxOwnerBytes = 1_048_576;
const maxDiagnosticCommands = 128;
const maxDiagnosticInspections = 8;
const maxDiagnosticLifecycle = 128;
const maxDiagnosticOutputBytes = 4_096;
const maxDiagnosticSerializedBytes = 384 * 1024;
const maxOwnerStatusBytes = 16 * 1024;
const maxOwnerCausalArgumentBytes = 128;
const maxOwnerCausalOutputBytes = 512;
const maxOwnerListenerCommandBytes = 4 * 1024;
const diagnosticSource = 'tests/mobile/support/ios-xctest.ts';
const privateOwnerKeyPattern = /(?:password|passwd|secret|token|credential|private|api[_-]?key)/iu;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu;
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));

function monotonicMilliseconds(): number {
  return Number(process.hrtime.bigint()) / 1_000_000;
}

function redactDiagnosticText(value: string): string {
  return redactText(value);
}

function redactedBounded(value: string | Buffer, maximum: number): string {
  const text = typeof value === 'string' ? value : value.toString('utf8');
  return Buffer.from(redactDiagnosticText(text)).subarray(0, maximum).toString('utf8');
}

function diagnosticText(value: unknown, maximum = maxDiagnosticOutputBytes, operation = ''): { content: string; bytes: number; truncated: boolean; suppressed?: boolean } {
  const text = value === undefined || value === null ? '' : typeof value === 'string' ? value : value instanceof Buffer ? value.toString('utf8') : String(value);
  const bytes = Buffer.byteLength(text);
  const classification = operation === 'inspect-listener' ? 'listener command' : operation === 'collect-system-log' ? 'system log' : operation ? `${operation} command` : 'command';
  const content = bytes ? `[${classification} output suppressed]\n` : '';
  return {
    content: Buffer.from(content).subarray(0, maximum).toString('utf8'),
    bytes,
    truncated: bytes > maximum,
    ...(bytes ? {suppressed: true} : {}),
  };
}

function safeCommand(file: string, args: string[]): string[] {
  return [file, ...args].slice(0, 32).map(argument => redactedBounded(String(argument), 512));
}

interface DiagnosticContext {
  recorder?: WdaDiagnosticsRecorder;
  phase: string;
  source: string;
  operation: string;
  endpoint?: string;
  port?: number;
  pid?: string;
  frozenOwner?: boolean;
}

interface CommandDiagnostic {
  id: number;
  phase: string;
  source: string;
  operation: string;
  command: string[];
  timeoutMs: number;
  startedAt: string;
  endedAt: string;
  monotonicStartMs: number;
  monotonicEndMs: number;
  durationMs: number;
  status: number | null;
  signal: NodeJS.Signals | null;
  endpoint?: string;
  port?: number;
  pid?: string;
  frozenOwner?: boolean;
  stdout: { content: string; bytes: number; truncated: boolean; suppressed?: boolean };
  stderr: { classification: string; content: string; bytes: number; truncated: boolean; suppressed?: boolean };
  error?: { category: string; code?: string; message?: string };
}

interface LifecycleDiagnostic {
  id: number;
  event: string;
  at: string;
  monotonicMs: number;
  detail?: unknown;
}

interface InspectionDiagnostic {
  id: number;
  phase: string;
  startedAt: string;
  endedAt: string;
  monotonicStartMs: number;
  monotonicEndMs: number;
  durationMs: number;
  frozenOwner: boolean;
  commandIds: number[];
  endpoints: { wda: unknown; mjpeg: unknown };
  processIds: string[];
  error?: string;
}

interface WdaDiagnostics {
  schema: 1;
  clock: { kind: 'process-relative-monotonic'; startedAt: string };
  commands: CommandDiagnostic[];
  inspections: InspectionDiagnostic[];
  lifecycle: LifecycleDiagnostic[];
  droppedCommands?: number;
  droppedInspections?: number;
  droppedLifecycle?: number;
}

interface InspectionToken {
  id: number;
  phase: string;
  frozenOwner: boolean;
  startedAt: string;
  monotonicStartMs: number;
  commandStart: number;
}

class WdaDiagnosticsRecorder {
  readonly data: WdaDiagnostics;
  private readonly origin = monotonicMilliseconds();
  private commandSequence = 0;
  private inspectionSequence = 0;
  private lifecycleSequence = 0;

  constructor() {
    this.data = {
      schema: 1,
      clock: {kind: 'process-relative-monotonic', startedAt: new Date().toISOString()},
      commands: [],
      inspections: [],
      lifecycle: [],
    };
  }

  now(): number {
    return Math.max(0, monotonicMilliseconds() - this.origin);
  }

  command(
    file: string,
    args: string[],
    timeoutMs: number,
    startedAt: string,
    monotonicStartMs: number,
    result?: SpawnSyncReturns<string>,
    thrown?: unknown,
    context?: DiagnosticContext,
  ): number {
    const endedAt = new Date().toISOString();
    const monotonicEndMs = this.now();
    const error = result?.error || (thrown instanceof Error ? thrown : undefined);
    const code = (error as NodeJS.ErrnoException | undefined)?.code;
    const category = code === 'ETIMEDOUT' ? 'timeout'
      : code === 'ENOBUFS' ? 'output-limit'
      : error ? 'spawn-error'
      : result && result.status !== 0 ? 'nonzero-exit' : 'none';
    const stderr = diagnosticText(result?.stderr, maxDiagnosticOutputBytes, context?.operation);
    const stderrClassification = code === 'ETIMEDOUT' ? 'timeout'
      : stderr.bytes > 0 ? 'present'
      : result?.status === 1 && context?.operation === 'inspect-listener' ? 'empty-or-no-listener'
      : 'empty';
    const record: CommandDiagnostic = {
      id: ++this.commandSequence,
      phase: context?.phase || 'unknown',
      source: context?.source || diagnosticSource,
      operation: context?.operation || 'command',
      command: safeCommand(file, args),
      timeoutMs,
      startedAt,
      endedAt,
      monotonicStartMs,
      monotonicEndMs,
      durationMs: Math.max(0, Number((monotonicEndMs - monotonicStartMs).toFixed(3))),
      status: result?.status ?? null,
      signal: result?.signal ?? null,
      ...(context?.endpoint ? {endpoint: context.endpoint} : {}),
      ...(context?.port !== undefined ? {port: context.port} : {}),
      ...(context?.pid ? {pid: context.pid} : {}),
      ...(context?.frozenOwner !== undefined ? {frozenOwner: context.frozenOwner} : {}),
      stdout: diagnosticText(result?.stdout, maxDiagnosticOutputBytes, context?.operation),
      stderr: {classification: stderrClassification, ...stderr},
      ...(error ? {error: {category, ...(code ? {code} : {}), message: diagnosticText(error.message, 1_024).content}} : {}),
    };
    this.data.commands.push(record);
    if (this.data.commands.length > maxDiagnosticCommands) {
      this.data.commands.splice(0, this.data.commands.length - maxDiagnosticCommands);
      this.data.droppedCommands = (this.data.droppedCommands || 0) + 1;
    }
    this.trimToBudget();
    return record.id;
  }

  recentCommandIds(limit = 8): number[] {
    return this.data.commands.slice(-limit).map(command => command.id);
  }

  commandIdsFrom(start: number): number[] {
    return this.data.commands.filter(command => command.id >= start).map(command => command.id);
  }

  lifecycle(event: string, detail: Record<string, unknown> = {}): number {
    const record: LifecycleDiagnostic = {
      id: ++this.lifecycleSequence,
      event,
      at: new Date().toISOString(),
      monotonicMs: Number(this.now().toFixed(3)),
      detail: sanitizeValue(detail),
    };
    this.data.lifecycle.push(record);
    if (this.data.lifecycle.length > maxDiagnosticLifecycle) {
      this.data.lifecycle.splice(0, this.data.lifecycle.length - maxDiagnosticLifecycle);
      this.data.droppedLifecycle = (this.data.droppedLifecycle || 0) + 1;
    }
    this.trimToBudget();
    return record.id;
  }

  beginInspection(phase: string, frozenOwner: boolean): InspectionToken {
    return {
      id: ++this.inspectionSequence,
      phase,
      frozenOwner,
      startedAt: new Date().toISOString(),
      monotonicStartMs: this.now(),
      commandStart: this.commandSequence + 1,
    };
  }

  finishInspection(token: InspectionToken, endpoints: { wda: unknown; mjpeg: unknown }, processIds: string[], error?: unknown): void {
    const monotonicEndMs = this.now();
    const record: InspectionDiagnostic = {
      id: token.id,
      phase: token.phase,
      startedAt: token.startedAt,
      endedAt: new Date().toISOString(),
      monotonicStartMs: token.monotonicStartMs,
      monotonicEndMs,
      durationMs: Math.max(0, Number((monotonicEndMs - token.monotonicStartMs).toFixed(3))),
      frozenOwner: token.frozenOwner,
      commandIds: this.commandIdsFrom(token.commandStart),
      endpoints,
      processIds: processIds.slice(0, 16),
      ...(error ? {error: diagnosticText(error instanceof Error ? error.message : error, 1_024).content} : {}),
    };
    this.data.inspections.push(record);
    if (this.data.inspections.length > maxDiagnosticInspections) {
      this.data.inspections.splice(0, this.data.inspections.length - maxDiagnosticInspections);
      this.data.droppedInspections = (this.data.droppedInspections || 0) + 1;
    }
    this.trimToBudget();
  }

  checkpoint(): number {
    return this.commandSequence + 1;
  }

  commandsForIds(ids: number[]): CommandDiagnostic[] {
    const wanted = new Set(ids);
    return this.data.commands.filter(command => wanted.has(command.id));
  }

  private trimToBudget(): void {
    const size = (): number => Buffer.byteLength(JSON.stringify(this.data));
    while (size() > maxDiagnosticSerializedBytes) {
      if (this.data.commands.length) {
        this.data.commands.shift();
        this.data.droppedCommands = (this.data.droppedCommands || 0) + 1;
      } else if (this.data.inspections.length) {
        this.data.inspections.shift();
        this.data.droppedInspections = (this.data.droppedInspections || 0) + 1;
      } else if (this.data.lifecycle.length) {
        this.data.lifecycle.shift();
        this.data.droppedLifecycle = (this.data.droppedLifecycle || 0) + 1;
      } else {
        break;
      }
    }
  }
}

const required = (key: string): string => {
  const value = process.env[key];
  if (!value) throw new Error(`XCTEST: missing ${key}`);
  return value;
};

function deadlineTimeout(deadline: number): number {
  const timeout = deadline - Date.now();
  if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('XCTEST: startup deadline');
  return timeout;
}

function executeSync(file: string, args: string[], timeout: number, maxBuffer: number, context?: DiagnosticContext): SpawnSyncReturns<string> {
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

function run(file: string, args: string[], timeout = 10_000, context?: DiagnosticContext): string {
  const result = executeSync(file, args, timeout, 4_194_304, context);
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
  if (result.error || result.status !== 0) throw new Error(`XCTEST: ${file} failed (${result.status ?? 'unknown'})`);
  return result.stdout.trim();
}

function ownership(udid: string): void {
  if (!uuidPattern.test(udid) || readFileSync(required('MOBILE_DEVICE_OWNERSHIP_FILE'), 'utf8').trim() !== `ios:${udid}`) {
    throw new Error('XCTEST: simulator ownership mismatch');
  }
}

function portValue(key: string, fallback: number): number {
  const value = Number(process.env[key] || fallback);
  if (!Number.isInteger(value) || value < 1024 || value > 65535) throw new Error('XCTEST: invalid port');
  return value;
}

function listeners(port: number, timeout = maxProcessCommandMs, observed?: (status: number | null) => void, context?: DiagnosticContext): string[] {
  if (!Number.isInteger(port) || port < 1 || port > 65535 || timeout <= 0) throw new Error('XCTEST: startup deadline');
  const result = executeSync('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN', '-t'], timeout, 65_536, context ? {...context, port} : undefined);
  observed?.(result.status);
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

interface ProcessEvidence {
  pid: string;
  executable: string;
  command: string;
  birth: string;
}

function processEvidence(pid: string, deadline: number, context?: DiagnosticContext): ProcessEvidence {
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

function safeProcessBirth(value: string, sanitized: boolean): string {
  if (!sanitized) return value;
  const trimmed = value.trim();
  return /^(?:[A-Za-z]{3} [A-Za-z]{3} {1,2}\d{1,2} \d{2}:\d{2}:\d{2} \d{4}|birth-[A-Za-z0-9_-]+|\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)$/u.test(trimmed)
    ? trimmed
    : '[unrecognized process birth]';
}

function commandContainsPath(command: string, path: string): boolean {
  const resolved = resolve(path);
  const variants = [resolved];
  if (resolved.startsWith('/private/')) variants.push(resolved.slice('/private'.length));
  if (resolved.startsWith('/var/')) variants.push(`/private${resolved}`);
  return variants.some(variant => command.includes(variant));
}

function runnerExecutableMatches(executable: string, product: string, udid: string, deadline: number, productBinaryHash?: string, evaluation?: RunnerMatchEvidence, context?: DiagnosticContext): boolean {
  try {
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
      actualHash = binaryHash(resolved);
    } catch (error) {
      if (evaluation) { evaluation.executableHash = {status: 'error', ...(productBinaryHash ? {expected: productBinaryHash} : {})}; evaluation.failureStage = 'runner-executable-hash-command'; evaluation.errorCategory = 'runner-command-error'; }
      throw error;
    }
    let expectedHash: string;
    try {
      expectedHash = productBinaryHash || binaryHash(join(productPath, 'WebDriverAgentRunner-Runner'));
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

class ReceiptValidationError extends Error {}

interface StatusResponse {
  statusCode: number;
  body: unknown;
  bytes?: number;
  truncated?: boolean;
}

async function readStatus(url: string, timeout: number): Promise<StatusResponse> {
  if (timeout <= 0) throw new Error('XCTEST: startup deadline');
  const response = await fetch(`${url}/status`, { signal: AbortSignal.timeout(timeout), redirect: 'error' });
  const reader = response.body?.getReader();
  if (!reader) return { statusCode: response.status, body: null };
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  let truncated = false;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      const available = maxStatusBytes - bytes;
      if (available <= 0) {
        truncated = true;
        break;
      }
      chunks.push(value.subarray(0, available));
      bytes += Math.min(value.length, available);
      if (value.length > available) {
        truncated = true;
        break;
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  const text = Buffer.concat(chunks).toString('utf8');
  let body: unknown;
  try { body = JSON.parse(text); }
  catch { body = { raw: text }; }
  return { statusCode: response.status, body, bytes, ...(truncated ? { truncated: true } : {}) };
}

export function validWdaStatus(status: unknown, runtime: string): boolean {
  const value = (status as { value?: { ready?: boolean; state?: string; build?: { version?: string; productBundleIdentifier?: string }; os?: { version?: string } } })?.value;
  return value?.ready === true && value.state === 'success' && value.build?.version === version
    && value.build.productBundleIdentifier === productId && value.os?.version === runtime;
}

export function selectXctestrun(root: string, product: string, deadline = Date.now() + 10_000, context?: DiagnosticContext): string {
  const files = readdirSync(root).filter(name => /^WebDriverAgentRunner_.*\.xctestrun$/u.test(name));
  if (files.length !== 1) throw new Error('XCTEST: ambiguous or missing xctestrun');
  const path = join(root, files[0]);
  const data = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', path], deadline - Date.now(), context ? {
    ...context,
    source: diagnosticSource,
    operation: 'select-xctestrun',
  } : undefined));
  const keys = Object.keys(data).filter(key => key !== '__xctestrun_metadata__');
  const target = data.WebDriverAgentRunner;
  const productPath = realpathSync(product);
  if (keys.length !== 1 || keys[0] !== 'WebDriverAgentRunner' || target?.TestHostBundleIdentifier !== runnerId
    || target.TestBundlePath !== '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest'
    || target.TestHostPath !== '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app'
    || realpathSync(join(root, 'Debug-iphonesimulator/WebDriverAgentRunner-Runner.app')) !== productPath
    || !existsSync(join(productPath, 'PlugIns/WebDriverAgentRunner.xctest'))) {
    throw new Error('XCTEST: unexpected test product');
  }
  if (run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(productPath, 'Info.plist')], deadline - Date.now(), context ? {
    ...context,
    source: diagnosticSource,
    operation: 'validate-product-bundle-id',
  } : undefined) !== runnerId) {
    throw new Error('XCTEST: runner identifier mismatch');
  }
  return path;
}

type RunnerIdentity = Pick<ProcessEvidence, 'pid' | 'executable' | 'birth'>;
type ListenerEvidence = Partial<ProcessEvidence> & Pick<ProcessEvidence, 'pid' | 'executable'>;

interface ListenerCommandStatus {
  port: number;
  status: number | null;
}

type ListenerEndpointStatus = 'not-evaluated' | 'evaluated' | 'error';
type AssociationStatus = 'not-evaluated' | 'match' | 'mismatch';

interface ListenerEndpointSnapshot {
  status: ListenerEndpointStatus;
  pids?: string[];
  errorCategory?: string;
}

interface ListenerSnapshot {
  wda: ListenerEndpointSnapshot;
  mjpeg: ListenerEndpointSnapshot;
  evidence: ListenerEvidence[];
  commands: ListenerCommandStatus[];
  inspectionId?: number;
  commandStart?: number;
  commandIds?: number[];
}

type RunnerPathShape = Partial<Record<'applicationNameMatches' | 'containerUuid' | 'simulatorPath' | 'executableName', boolean>>;

interface RunnerMatchEvidence {
  pathShape?: RunnerPathShape;
  bundleId?: { status: 'not-evaluated' | 'evaluated' | 'error'; matches?: boolean };
  executableHash?: { status: 'not-evaluated' | 'evaluated' | 'error'; expected?: string; actual?: string; matches?: boolean };
  failureStage?: string;
  errorCategory?: string;
}

type ListenerEndpointDiagnostic =
  | { status: 'not-evaluated' }
  | { status: 'error'; errorCategory: string }
  | { status: 'evaluated'; count: number; pids: string[] };

interface ListenerValidation {
  checkedAt: string;
  endpoints: {
    wda: ListenerEndpointDiagnostic;
    mjpeg: ListenerEndpointDiagnostic;
  };
  commandStatus: ListenerCommandStatus[];
  runnerEvidencePresent: 'not-evaluated' | 'present' | 'absent';
  runnerAssociation: AssociationStatus;
  mjpegAssociation: AssociationStatus;
  bundleId: { status: 'not-evaluated' | 'evaluated' | 'error'; matches?: boolean };
  executableHash: { status: 'not-evaluated' | 'evaluated' | 'error'; expected?: string; actual?: string; matches?: boolean };
  cachedProductExecutableHash?: string;
  cachedProductExecutableHashAt?: string;
  pathShape?: RunnerMatchEvidence['pathShape'];
  failureStage: string;
  errorCategory: string;
}

interface FirstFailure {
  at: string;
  monotonicMs: number;
  phase: string;
  source: string;
  stage: string;
  category: string;
  message: string;
  frozenOwner: boolean;
  predicate: {
    status: 'evaluated' | 'not-evaluated';
    listenerValidation?: ListenerValidation;
  };
  commandIds: number[];
  causalCommands: CommandDiagnostic[];
  inspectionId?: number;
}

interface Owner {
  udid: string;
  pid?: number;
  command?: string[];
  runnerPid?: string;
  runnerExecutable?: string;
  receipt?: string;
  installReceipt?: string;
  listenerEvidence?: ListenerEvidence[];
  listenerValidation?: ListenerValidation;
  firstFailure?: FirstFailure;
  diagnostics?: WdaDiagnostics;
  cachedProductExecutableHash?: string;
  cachedProductExecutableHashAt?: string;
  pidBirth?: string;
  runnerBirth?: string;
  startedAt: string;
  ready: boolean;
  endedAt?: string;
  exitCode?: number | null;
  signal?: string | null;
  error?: string;
  receiptError?: string;
  url: string;
  product: string;
  xctestrun?: string;
  xctestrunHash?: string;
  status?: StatusResponse;
  logBytes?: number;
  logTruncated?: boolean;
  receivedLogBytes?: number;
  receivedStreamBytes?: { stdout: number; stderr: number };
  safeLogBytes?: number;
  pendingLogBytes?: number;
  logPersistenceTruncated?: boolean;
  startupOutputLimit?: {
    limitBytes: number;
    receivedBytes: number;
    stream: 'stdout' | 'stderr';
    streamOffset: number;
    chunkBytes: number;
    acceptedBytes: number;
    overLimitBytes: number;
  };
}

function sanitizeOwnerValue(value: unknown, key = ''): unknown {
  if (privateOwnerKeyPattern.test(key)) return '[REDACTED]';
  if (typeof value === 'string') return redactDiagnosticText(value);
  if (Array.isArray(value)) return value.map(entry => sanitizeOwnerValue(entry));
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([entryKey, entry]) => [entryKey, sanitizeOwnerValue(entry, entryKey)]));
  return value;
}

function ownerText(value: unknown, maximum: number, sanitized: boolean): string {
  const text = value === undefined || value === null ? '' : String(value);
  const safe = sanitized ? redactDiagnosticText(text) : text;
  return Buffer.from(safe).subarray(0, maximum).toString('utf8');
}

function boundedRecord<T extends {content: string; bytes: number; truncated: boolean; suppressed?: boolean}>(record: T, maximum: number, sanitized: boolean): T {
  const content = ownerText(record.content, maximum, sanitized);
  return {...record, content, truncated: record.truncated || Buffer.byteLength(record.content) > maximum || content !== record.content};
}

function boundedCausalCommand(command: CommandDiagnostic, sanitized: boolean): CommandDiagnostic {
  return {
    ...command,
    command: command.command.slice(0, 32).map(argument => ownerText(argument, maxOwnerCausalArgumentBytes, sanitized)),
    stdout: boundedRecord(command.stdout, maxOwnerCausalOutputBytes, sanitized),
    stderr: boundedRecord(command.stderr, maxOwnerCausalOutputBytes, sanitized),
    ...(command.error ? {error: {...command.error, ...(command.error.message ? {message: ownerText(command.error.message, 2_048, sanitized)} : {})}} : {}),
  };
}

function boundedStatus(status: StatusResponse): StatusResponse {
  const body = status.body as { value?: unknown } | null;
  const value = body?.value as {
    ready?: unknown;
    state?: unknown;
    build?: { version?: unknown; productBundleIdentifier?: unknown };
    os?: { version?: unknown };
  } | undefined;
  const structured = Boolean(value && typeof value === 'object');
  const ready = value?.ready === true || value?.ready === false ? value.ready : 'not-evaluated';
  const state = value?.state === 'success' || value?.state === 'failure' ? value.state : 'unrecognized';
  const buildVersion = value?.build?.version === version ? version : 'unrecognized';
  const productBundleIdentifier = value?.build?.productBundleIdentifier === productId ? productId : 'unrecognized';
  const osVersion = typeof value?.os?.version === 'string' && /^\d+(?:\.\d+){1,3}$/u.test(value.os.version)
    ? value.os.version
    : 'unrecognized';
  return {
    statusCode: status.statusCode,
    ...(status.bytes !== undefined ? {bytes: status.bytes} : {}),
    ...(status.truncated ? {truncated: true} : {}),
    body: {
      classification: structured ? 'wda-status' : 'unrecognized',
      ready,
      state,
      buildVersion,
      productBundleIdentifier,
      osVersion,
      payload: 'suppressed',
      diagnostic: 'suppressed',
      diagnosticBytes: status.bytes || 0,
      diagnosticTruncated: (status.bytes || 0) > maxOwnerStatusBytes || Boolean(status.truncated),
      ...(status.bytes !== undefined ? {bodyBytes: status.bytes} : {}),
      ...(status.truncated ? {bodyTruncated: true} : {}),
    },
  };
}

function safeXctestrunEvidence(data: unknown): string {
  const record = data && typeof data === 'object' ? data as Record<string, unknown> : {};
  const target = record.WebDriverAgentRunner && typeof record.WebDriverAgentRunner === 'object'
    ? record.WebDriverAgentRunner as Record<string, unknown>
    : {};
  const known = (value: unknown, expected: string): string => value === expected ? expected : 'unrecognized';
  const environmentVariables = target.EnvironmentVariables && typeof target.EnvironmentVariables === 'object'
    ? Object.keys(target.EnvironmentVariables as Record<string, unknown>)
    : [];
  const evidence = {
    schema: 1,
    format: 'xctestrun',
    targets: Object.keys(record).filter(key => key !== '__xctestrun_metadata__').slice(0, 16),
    WebDriverAgentRunner: {
      TestHostBundleIdentifier: known(target.TestHostBundleIdentifier, runnerId),
      TestBundlePath: known(target.TestBundlePath, '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest'),
      TestHostPath: known(target.TestHostPath, '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app'),
      EnvironmentVariables: {
        count: environmentVariables.length,
        values: 'suppressed',
      },
    },
    payload: 'suppressed',
  };
  return `${JSON.stringify(evidence, null, 2)}\n`;
}

function boundedOwnerValue(value: Owner, sanitized: boolean): Owner {
  return {
    ...value,
    ...(value.command ? {command: value.command.slice(0, 32).map(argument => ownerText(argument, 512, sanitized))} : {}),
    ...(value.pidBirth ? {pidBirth: safeProcessBirth(value.pidBirth, sanitized)} : {}),
    ...(value.runnerBirth ? {runnerBirth: safeProcessBirth(value.runnerBirth, sanitized)} : {}),
    ...(value.listenerEvidence ? {
      listenerEvidence: value.listenerEvidence.slice(0, 16).map(entry => ({
        ...entry,
        executable: ownerText(entry.executable, maxOwnerListenerCommandBytes, sanitized),
        ...(entry.command ? {command: ownerText(entry.command, maxOwnerListenerCommandBytes, sanitized)} : {}),
        ...(entry.birth ? {birth: safeProcessBirth(entry.birth, sanitized)} : {}),
      })),
    } : {}),
    ...(value.firstFailure ? {
      firstFailure: {
        ...value.firstFailure,
        message: ownerText(value.firstFailure.message, 2_048, sanitized),
        causalCommands: value.firstFailure.causalCommands.map(command => boundedCausalCommand(command, sanitized)),
      },
    } : {}),
    ...(value.diagnostics ? {
      diagnostics: {
        ...value.diagnostics,
        commands: value.diagnostics.commands.map(command => boundedCausalCommand(command, sanitized)),
      },
    } : {}),
    ...(value.status ? {status: boundedStatus(value.status)} : {}),
  };
}

function ownerContent(owner: Owner, sanitized: boolean): string {
  const value = boundedOwnerValue((sanitized ? sanitizeOwnerValue(sanitizeValue(owner)) : owner) as Owner, sanitized);
  const serialize = (entry: Owner): string => `${JSON.stringify(entry, null, 2)}\n`;
  const content = serialize(value);
  if (Buffer.byteLength(content) <= maxOwnerBytes) return content;
  const reduced: Owner = {
    ...value,
    ...(value.listenerEvidence ? {listenerEvidence: value.listenerEvidence.slice(0, 1)} : {}),
    ...(value.diagnostics ? {diagnostics: {...value.diagnostics, commands: [], inspections: [], lifecycle: []}} : {}),
  };
  const reducedContent = serialize(reduced);
  if (Buffer.byteLength(reducedContent) > maxOwnerBytes) throw new Error('XCTEST: owner evidence limit');
  return reducedContent;
}

function writeOwner(root: string, owner: Owner): void {
  const privateNext = join(root, 'owner-private.next.json');
  writeFileSync(privateNext, ownerContent(owner, false), { mode: 0o600 });
  renameSync(privateNext, join(root, 'owner-private.json'));
  const next = join(root, 'owner.next.json');
  writeFileSync(next, ownerContent(owner, true), { mode: 0o600 });
  renameSync(next, join(root, 'owner.json'));
}

function binaryHash(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

const diagnosticPids = (values: string[]): string[] => values.slice(0, 16);

function endpointDiagnostic(endpoint: ListenerEndpointSnapshot): ListenerEndpointDiagnostic {
  if (endpoint.status === 'evaluated') {
    const pids = endpoint.pids || [];
    return {status: 'evaluated', count: pids.length, pids: diagnosticPids(pids)};
  }
  return endpoint.status === 'error'
    ? {status: 'error', errorCategory: endpoint.errorCategory || 'command-error'}
    : {status: 'not-evaluated'};
}

function listenerValidation(
  snapshot: ListenerSnapshot,
  commands: ListenerCommandStatus[],
  runnerAssociation: ListenerValidation['runnerAssociation'],
  mjpegAssociation: ListenerValidation['mjpegAssociation'],
  evaluation: RunnerMatchEvidence,
  failureStage: string,
  errorCategory: string,
  cachedProductExecutableHash?: string,
  cachedProductExecutableHashAt?: string,
): ListenerValidation {
  const runnerPid = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids?.[0] : undefined;
  return {
    checkedAt: new Date().toISOString(),
    endpoints: {wda: endpointDiagnostic(snapshot.wda), mjpeg: endpointDiagnostic(snapshot.mjpeg)},
    commandStatus: commands.slice(0, 4),
    runnerEvidencePresent: runnerPid
      ? snapshot.evidence.length === 0 ? 'not-evaluated' : snapshot.evidence.some(entry => entry.pid === runnerPid) ? 'present' : 'absent'
      : 'not-evaluated',
    runnerAssociation,
    mjpegAssociation,
    bundleId: evaluation.bundleId || {status: 'not-evaluated'},
    executableHash: evaluation.executableHash || {status: 'not-evaluated'},
    ...(cachedProductExecutableHash ? {cachedProductExecutableHash} : {}),
    ...(cachedProductExecutableHashAt ? {cachedProductExecutableHashAt} : {}),
    ...(evaluation.pathShape ? {pathShape: evaluation.pathShape} : {}),
    failureStage,
    errorCategory,
  };
}

type StartupOutputStream = 'stdout' | 'stderr';

interface StartupOutputState {
  decoder: StringDecoder;
  recordCount: number;
  incompleteUtf8: boolean;
  finalized: boolean;
}

async function supervise(): Promise<void> {
  const root = required('IOS_XCTEST_STATE_DIR');
  const udid = required('IOS_SIMULATOR_UDID');
  const productInput = process.env.IOS_WDA_PREBUILT_PATH || '';
  const requestedPort = process.env.IOS_WDA_PORT || '8100';
  const owner: Owner = {
    udid,
    product: productInput,
    url: `http://127.0.0.1:${requestedPort}`,
    startedAt: new Date().toISOString(),
    ready: false,
  };
  const diagnostics = new WdaDiagnosticsRecorder();
  owner.diagnostics = diagnostics.data;
  let stop = false;
  let deadline = 0;
  let port = 0;
  let mjpeg = 0;
  let product = '';
  let productBinaryHash = '';
  let child: ChildProcess | undefined;
  let exited: Promise<void> | undefined;
  let childEnded = false;
  let frozenRunner: RunnerIdentity | undefined;
  const commandContext = (phase: string, operation: string, extra: Partial<Pick<DiagnosticContext, 'endpoint' | 'port' | 'pid' | 'frozenOwner'>> = {}): DiagnosticContext => ({
    recorder: diagnostics,
    phase,
    source: diagnosticSource,
    operation,
    ...extra,
  });
  diagnostics.lifecycle('supervisor-start', {phase: 'startup', pid: process.pid, simulator: udid});
  const errorMessage = (error: unknown): string => error instanceof Error ? error.message : String(error);
  const receiptErrorCategory = (error: unknown): string => {
    const message = errorMessage(error);
    if (message.includes('ENOENT') || message.includes('no such file')) return 'receipt-unavailable';
    if (message.startsWith('XCTEST: xcrun failed')) return 'receipt-command-failed';
    if (message === 'XCTEST: registration receipt mismatch') return 'receipt-mismatch';
    return 'receipt-validation-error';
  };
  const safeFailureMessage = (stage: string, category: string): string => {
    if (stage === 'startup-deadline') return 'XCTEST: startup deadline';
    if (stage === 'startup-log-limit') return 'XCTEST: log output limit';
    if (stage === 'registration-receipt') return 'XCTEST: registration receipt validation failed';
    if (stage === 'status-evidence-write') return 'XCTEST: WDA status evidence write failed';
    if (stage.startsWith('managed-')) return 'XCTEST: managed WDA listener changed';
    if (['wda-pid-count', 'wda-runner-evidence', 'wda-executable-evidence'].includes(stage)) return 'XCTEST: unknown WDA listener';
    return `XCTEST: ${category}`;
  };
  const rememberFailure = (failure: {
    error?: unknown;
    message?: string;
    phase: string;
    stage: string;
    category: string;
    predicate?: ListenerValidation;
    frozenOwner?: boolean;
    commandIds?: number[];
    inspectionId?: number;
  }): void => {
    const message = safeFailureMessage(failure.stage, failure.category);
    const commandIds = (failure.commandIds ?? diagnostics.recentCommandIds()).slice(-16);
    const causalCommands = diagnostics.commandsForIds(commandIds);
    const first = !owner.firstFailure;
    owner.error ||= message;
    diagnostics.lifecycle('failure-observed', {
      phase: failure.phase,
      stage: failure.stage,
      category: failure.category,
      message,
      first,
      frozenOwner: failure.frozenOwner ?? Boolean(frozenRunner),
      ...(commandIds.length ? {commandIds} : {}),
      ...(failure.inspectionId !== undefined ? {inspectionId: failure.inspectionId} : {}),
    });
    if (first) {
      owner.firstFailure = {
        at: new Date().toISOString(),
        monotonicMs: Number(diagnostics.now().toFixed(3)),
        phase: failure.phase,
        source: diagnosticSource,
        stage: failure.stage,
        category: failure.category,
        message,
        frozenOwner: failure.frozenOwner ?? Boolean(frozenRunner),
        predicate: failure.predicate
          ? {status: 'evaluated', listenerValidation: JSON.parse(JSON.stringify(failure.predicate)) as ListenerValidation}
          : {status: 'not-evaluated'},
        commandIds,
        causalCommands: JSON.parse(JSON.stringify(causalCommands)) as CommandDiagnostic[],
        ...(failure.inspectionId !== undefined ? {inspectionId: failure.inspectionId} : {}),
      };
    }
  };
  const snapshotCommandIds = (snapshot: ListenerSnapshot): number[] => snapshot.commandStart === undefined
    ? snapshot.commandIds || []
    : diagnostics.commandIdsFrom(snapshot.commandStart);
  const save = (): void => {
    try {
      writeOwner(root, owner);
    } catch {
      rememberFailure({message: 'XCTEST: owner evidence write failed', phase: 'supervisor', stage: 'owner-evidence-write', category: 'owner-evidence-write'});
      stop = true;
    }
  };
  const remaining = (): number => {
    const value = deadline - Date.now();
    if (value <= 0) throw new Error('XCTEST: startup deadline');
    return value;
  };
  const onSignal = (): void => {
    diagnostics.lifecycle('stop-requested', {phase: 'supervise'});
    stop = true;
  };
  process.on('SIGTERM', onSignal);
  process.on('SIGINT', onSignal);
  const log = join(root, 'wda-preflight-private.log');
  const publicLog = process.env.STARTUP_TEST_LOG_FINALIZATION_TARGET || join(root, 'wda-preflight.log');
  let safeLogBytes = 0;
  let receivedLogBytes = 0;
  let rawOutputSuppressed = false;
  const streamBytes: Record<StartupOutputStream, number> = {stdout: 0, stderr: 0};
  const outputStates: Record<StartupOutputStream, StartupOutputState> = {
    stdout: {decoder: new StringDecoder('utf8'), recordCount: 0, incompleteUtf8: false, finalized: false},
    stderr: {decoder: new StringDecoder('utf8'), recordCount: 0, incompleteUtf8: false, finalized: false},
  };
  const updateOutputAccounting = (): void => {
    owner.logBytes = safeLogBytes;
    owner.safeLogBytes = safeLogBytes;
    owner.receivedLogBytes = receivedLogBytes;
    owner.receivedStreamBytes = {...streamBytes};
    owner.pendingLogBytes = 0;
  };
  const writeSafeLog = (text: string): void => {
    const encoded = Buffer.from(text);
    const available = Math.max(0, maxStartupLogBytes - safeLogBytes);
    if (!encoded.length || !available) {
      updateOutputAccounting();
      return;
    }
    const retained = encoded.subarray(0, available);
    try {
      appendFileSync(log, retained, { mode: 0o600 });
      safeLogBytes += retained.length;
    } catch {
      rememberFailure({message: 'XCTEST: startup log write failed', phase: 'startup', stage: 'startup-log-write', category: 'startup-log-write'});
      stop = true;
    }
    updateOutputAccounting();
  };
  const recordStartupChunk = (state: StartupOutputState, chunk: Buffer): void => {
    const decoded = state.decoder.write(chunk);
    state.recordCount += [...decoded].filter(character => character === '\n').length;
  };
  const markLogLimit = (stream: StartupOutputStream, streamOffset: number, chunkBytes: number, acceptedBytes: number): void => {
    if (rawOutputSuppressed) return;
    rawOutputSuppressed = true;
    owner.logTruncated = true;
    owner.startupOutputLimit = {
      limitBytes: maxStartupLogBytes,
      receivedBytes: receivedLogBytes,
      stream,
      streamOffset,
      chunkBytes,
      acceptedBytes,
      overLimitBytes: receivedLogBytes - maxStartupLogBytes,
    };
    diagnostics.lifecycle('startup-output-limit', {
      phase: 'startup',
      stream,
      streamOffset,
      chunkBytes,
      acceptedBytes,
      receivedBytes: receivedLogBytes,
      limitBytes: maxStartupLogBytes,
      overLimitBytes: receivedLogBytes - maxStartupLogBytes,
    });
    rememberFailure({message: 'XCTEST: log output limit', phase: 'startup', stage: 'startup-log-limit', category: 'output-limit'});
    writeSafeLog(startupOutputMarker);
    updateOutputAccounting();
    stop = true;
  };
  const retain = (chunk: Buffer, stream: StartupOutputStream): void => {
    const offset = streamBytes[stream];
    streamBytes[stream] += chunk.length;
    const receivedBefore = receivedLogBytes;
    receivedLogBytes += chunk.length;
    diagnostics.lifecycle('child-output', {
      phase: 'startup',
      stream,
      offset,
      bytes: chunk.length,
      monotonicReceiveMs: Number(diagnostics.now().toFixed(3)),
    });
    const acceptedBytes = Math.max(0, Math.min(chunk.length, maxStartupLogBytes - receivedBefore));
    if (!outputStates[stream].finalized && acceptedBytes) {
      if (!rawOutputSuppressed && safeLogBytes === 0) writeSafeLog(startupOutputMarker);
      recordStartupChunk(outputStates[stream], chunk.subarray(0, acceptedBytes));
    }
    if (acceptedBytes < chunk.length) markLogLimit(stream, offset + acceptedBytes, chunk.length, acceptedBytes);
    updateOutputAccounting();
  };
  const finishOutput = (): void => {
    for (const [stream, state] of Object.entries(outputStates) as Array<[StartupOutputStream, StartupOutputState]>) {
      if (state.finalized) continue;
      const tail = state.decoder.end();
      state.incompleteUtf8 = tail.length > 0;
      if (!rawOutputSuppressed && streamBytes[stream] > 0) {
        writeSafeLog(`[startup output summary; stream=${stream}; bytes=${streamBytes[stream]}; records=${state.recordCount}; utf8Complete=${!state.incompleteUtf8}]\n`);
      }
      state.finalized = true;
    }
    updateOutputAccounting();
  };
  const plistEvidence = (format: 'xml' | 'json' | 'binary', status: 'parsed' | 'suppressed' | 'conversion-failed', bundleIdentifier: 'known' | 'unrecognized' | 'not-evaluated'): string => `${JSON.stringify({
    schema: 1,
    format,
    status,
    bundleIdentifier,
    values: 'suppressed',
  }, null, 2)}\n`;
  const plistBundleIdentifier = (value: unknown): 'known' | 'unrecognized' | 'not-evaluated' => {
    const bundleIdentifier = value && typeof value === 'object' ? (value as Record<string, unknown>).CFBundleIdentifier : undefined;
    if (bundleIdentifier === runnerId) return 'known';
    return typeof bundleIdentifier === 'string' ? 'unrecognized' : 'not-evaluated';
  };
  const installedInfoEvidence = (path: string, phase: string): string => {
    const raw = readFileSync(path);
    const text = raw.toString('utf8');
    const trimmed = text.trimStart();
    if (trimmed.startsWith('<?xml') || trimmed.startsWith('<plist')) return plistEvidence('xml', 'suppressed', 'not-evaluated');
    try {
      return plistEvidence('json', 'parsed', plistBundleIdentifier(JSON.parse(text)));
    } catch (error) {
      if (!(error instanceof SyntaxError)) throw error;
    }
    try {
      const converted = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', path], Math.min(10_000, remaining()), commandContext(phase, 'read-installed-info', {endpoint: 'wda', port})));
      return plistEvidence('binary', 'parsed', plistBundleIdentifier(converted));
    } catch {
      return plistEvidence('binary', 'conversion-failed', 'not-evaluated');
    }
  };
  const receipt = (phase: string): string => {
    const installed = realpathSync(run('xcrun', ['simctl', 'get_app_container', udid, runnerId, 'app'], Math.min(10_000, remaining()), commandContext(phase, 'read-install-receipt', {endpoint: 'wda', port})));
    owner.receipt = installed;
    writeFileSync(join(root, 'installed-Info.plist'), installedInfoEvidence(join(installed, 'Info.plist'), phase), { mode: 0o600 });
    const applicationDirectory = dirname(dirname(installed));
    const expectedApplicationDirectory = `/Devices/${udid}/data/Containers/Bundle/Application`;
    if (!uuidPattern.test(basename(dirname(installed))) || basename(installed) !== basename(product)
      || !applicationDirectory.endsWith(expectedApplicationDirectory)) {
      throw new Error('XCTEST: registration receipt mismatch');
    }
    if (run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(installed, 'Info.plist')], Math.min(10_000, remaining()), commandContext(phase, 'validate-install-receipt-bundle', {endpoint: 'wda', port})) !== runnerId
      || binaryHash(join(installed, 'WebDriverAgentRunner-Runner')) !== binaryHash(join(product, 'WebDriverAgentRunner-Runner'))) {
      throw new Error('XCTEST: registration receipt mismatch');
    }
    return installed;
  };
  const listenerCommandErrorCategory = (error: unknown): string => {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') return 'timeout';
    if (error instanceof Error && error.message.includes('invalid process identifier')) return 'invalid-process-id';
    return 'command-error';
  };
  const persistListenerCommandFailure = (snapshot: ListenerSnapshot, failureStage: string, error: unknown, phase: string): void => {
    const category = listenerCommandErrorCategory(error);
    const diagnosticCategory = category === 'timeout' ? 'listener-command-timeout'
      : category === 'invalid-process-id' ? 'listener-invalid-process-id' : 'listener-command-error';
    const validation = listenerValidation(snapshot, snapshot.commands, 'not-evaluated', 'not-evaluated', {}, failureStage,
      diagnosticCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
    owner.listenerValidation = validation;
    rememberFailure({error, phase, stage: failureStage, category: diagnosticCategory, predicate: validation,
      frozenOwner: Boolean(frozenRunner), commandIds: snapshotCommandIds(snapshot), inspectionId: snapshot.inspectionId});
    save();
  };
  const inspectListeners = (inspectionDeadline: number, phase = 'initial'): ListenerSnapshot => {
    const token = diagnostics.beginInspection(phase, Boolean(frozenRunner));
    const commands: ListenerCommandStatus[] = [];
    let wdaEndpoint: ListenerEndpointSnapshot = {status: 'not-evaluated'};
    let mjpegEndpoint: ListenerEndpointSnapshot = {status: 'not-evaluated'};
    const evidence: ListenerEvidence[] = [];
    let failure: unknown;
    const snapshot = (): ListenerSnapshot => ({
      wda: wdaEndpoint,
      mjpeg: mjpegEndpoint,
      evidence,
      commands,
      inspectionId: token.id,
      commandStart: token.commandStart,
      commandIds: diagnostics.commandIdsFrom(token.commandStart),
    });
    try {
      try {
        wdaEndpoint = {status: 'evaluated', pids: listeners(port, deadlineTimeout(inspectionDeadline), status => commands.push({port, status}), commandContext(phase, 'inspect-listener', {
          endpoint: 'wda', port, frozenOwner: Boolean(frozenRunner),
        }))};
      } catch (error) {
        failure = error;
        wdaEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
        persistListenerCommandFailure(snapshot(), 'wda-listener-command', error, phase);
        throw error;
      }
      try {
        mjpegEndpoint = {status: 'evaluated', pids: listeners(mjpeg, deadlineTimeout(inspectionDeadline), status => commands.push({port: mjpeg, status}), commandContext(phase, 'inspect-listener', {
          endpoint: 'mjpeg', port: mjpeg, frozenOwner: Boolean(frozenRunner),
        }))};
      } catch (error) {
        failure = error;
        mjpegEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
        persistListenerCommandFailure(snapshot(), 'mjpeg-listener-command', error, phase);
        throw error;
      }
      const pids = wdaEndpoint.pids || [];
      const mjpegPids = mjpegEndpoint.pids || [];
      for (const pid of [...new Set([...pids, ...mjpegPids])]) {
        try {
          const endpoint = pids.includes(pid) ? 'wda' : 'mjpeg';
          evidence.push(processEvidence(pid, inspectionDeadline, commandContext(phase, 'inspect-listener-process', {
            endpoint, port: endpoint === 'wda' ? port : mjpeg, pid, frozenOwner: Boolean(frozenRunner),
          })));
        } catch (error) {
          failure = error;
          evidence.push({pid, executable: '[unavailable]'});
          diagnostics.lifecycle('listener-process-evidence-error', {
            phase,
            pid,
            category: listenerCommandErrorCategory(error),
            frozenOwner: Boolean(frozenRunner),
          });
          owner.listenerEvidence = evidence;
          save();
          if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
            const current = snapshot();
            const validation = listenerValidation(current, commands, 'not-evaluated', 'not-evaluated', {}, 'listener-process-evidence', 'listener-process-timeout', owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
            owner.listenerValidation = validation;
            rememberFailure({error, phase, stage: 'listener-process-evidence', category: 'listener-process-timeout', predicate: validation,
              frozenOwner: Boolean(frozenRunner), commandIds: snapshotCommandIds(current), inspectionId: current.inspectionId});
            save();
            throw error;
          }
        }
      }
      owner.listenerEvidence = evidence;
      save();
      return snapshot();
    } catch (error) {
      failure ||= error;
      throw error;
    } finally {
      diagnostics.finishInspection(token, {wda: endpointDiagnostic(wdaEndpoint), mjpeg: endpointDiagnostic(mjpegEndpoint)},
        [...new Set([...(wdaEndpoint.pids || []), ...(mjpegEndpoint.pids || [])])], failure);
    }
  };
  const listenerPids = (phase = 'cleanup'): string[] => [...new Set([
    ...listeners(port, maxProcessCommandMs, undefined, commandContext(phase, 'inspect-listener', {endpoint: 'wda', port, frozenOwner: Boolean(frozenRunner)})),
    ...listeners(mjpeg, maxProcessCommandMs, undefined, commandContext(phase, 'inspect-listener', {endpoint: 'mjpeg', port: mjpeg, frozenOwner: Boolean(frozenRunner)})),
  ])];
  const ownedListeners = (pids: string[], phase = 'cleanup'): boolean => {
    if (!owner.runnerPid || !owner.runnerBirth || !owner.runnerExecutable || pids.length === 0 || pids.some(pid => pid !== owner.runnerPid)) return false;
    try {
      const inspectionDeadline = Date.now() + maxProcessCommandMs * 3;
      const evidence = processEvidence(owner.runnerPid, inspectionDeadline, commandContext(phase, 'inspect-managed-listener-process', {
        endpoint: 'managed-listener', port, pid: owner.runnerPid, frozenOwner: true,
      }));
      return evidence.birth === owner.runnerBirth && evidence.executable === owner.runnerExecutable
        && runnerExecutableMatches(evidence.executable, owner.product, udid, inspectionDeadline, productBinaryHash, undefined, commandContext(phase, 'validate-managed-listener', {
          endpoint: 'managed-listener', port, pid: owner.runnerPid, frozenOwner: true,
        }));
    } catch {
      return false;
    }
  };
  const validateListenerSnapshot = (snapshot: ListenerSnapshot, expected: RunnerIdentity | undefined, inspectionDeadline: number, phase = 'initial', frozenOwner = Boolean(frozenRunner)): RunnerIdentity => {
    const wdaPids = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids || [] : [];
    const mjpegPids = snapshot.mjpeg.status === 'evaluated' ? snapshot.mjpeg.pids || [] : [];
    const runner = snapshot.wda.status === 'evaluated' ? snapshot.evidence.find(entry => entry.pid === wdaPids[0]) : undefined;
    const admittedRunner = runner as ListenerEvidence | undefined;
    const evaluation: RunnerMatchEvidence = {};
    const causalCommandIds = (): number[] => snapshotCommandIds(snapshot);
    let runnerAssociation: ListenerValidation['runnerAssociation'] = 'not-evaluated';
    let mjpegAssociation: ListenerValidation['mjpegAssociation'] = 'not-evaluated';
    const persist = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): void => {
      owner.listenerValidation = listenerValidation(snapshot, snapshot.commands, observedRunnerAssociation, observedMjpegAssociation, evaluation, failureStage, errorCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
      save();
    };
    const record = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): never => {
      const validation = listenerValidation(snapshot, snapshot.commands, observedRunnerAssociation, observedMjpegAssociation, evaluation, failureStage, errorCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
      owner.listenerValidation = validation;
      const message = failureStage.startsWith('managed-') ? 'XCTEST: managed WDA listener changed' : 'XCTEST: unknown WDA listener';
      rememberFailure({message, phase, stage: failureStage, category: errorCategory, predicate: validation,
        frozenOwner, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
      save();
      throw new Error(message);
    };
    if (expected) {
      if (wdaPids.length !== 1) record('mismatch', mjpegAssociation, 'managed-wda-pid-count', 'managed-listener-identity-mismatch');
      if (!admittedRunner) record('mismatch', mjpegAssociation, 'managed-wda-runner-evidence', 'managed-listener-identity-mismatch');
      if (admittedRunner!.pid !== expected.pid) record('mismatch', mjpegAssociation, 'managed-wda-pid', 'managed-listener-identity-mismatch');
      if (admittedRunner!.executable !== expected.executable) record('mismatch', mjpegAssociation, 'managed-wda-executable', 'managed-listener-identity-mismatch');
      if (admittedRunner!.birth !== expected.birth) record('mismatch', mjpegAssociation, 'managed-wda-birth', 'managed-listener-identity-mismatch');
      runnerAssociation = 'match';
      if (mjpegPids.length > 1) record(runnerAssociation, 'mismatch', 'managed-mjpeg-pid-count', 'managed-listener-identity-mismatch');
      if (mjpegPids.some(pid => pid !== expected.pid)) record(runnerAssociation, 'mismatch', 'managed-mjpeg-pid', 'managed-listener-identity-mismatch');
      mjpegAssociation = 'match';
    }
    if (snapshot.wda.status !== 'evaluated' || wdaPids.length !== 1) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-pid-count', 'listener-endpoint-cardinality');
    if (!admittedRunner) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-runner-evidence', 'listener-process-evidence');
    if (admittedRunner!.executable === '[unavailable]') record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-executable-evidence', 'listener-process-evidence');
    if (!admittedRunner!.birth) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-birth-evidence', 'listener-process-evidence');
    let executableMatch = false;
    try {
      executableMatch = runnerExecutableMatches(admittedRunner!.executable, product, udid, inspectionDeadline, productBinaryHash, evaluation, commandContext(phase, 'validate-runner-identity', {
        endpoint: 'wda', port, pid: admittedRunner!.pid, frozenOwner,
      }));
    } catch (error) {
      if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
        const failureStage = evaluation.failureStage || 'runner-executable-deadline';
        const failureCategory = evaluation.errorCategory || 'listener-deadline';
        const validation = listenerValidation(snapshot, snapshot.commands, runnerAssociation, mjpegAssociation, evaluation, failureStage, failureCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
        owner.listenerValidation = validation;
        rememberFailure({error, phase, stage: failureStage, category: failureCategory, predicate: validation,
          frozenOwner, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
        save();
        throw error;
      }
      record(runnerAssociation, mjpegAssociation, evaluation.failureStage || 'runner-executable-observation', evaluation.errorCategory || 'runner-command-error');
    }
    if (!executableMatch) record(runnerAssociation, mjpegAssociation, evaluation.failureStage || 'runner-executable-observation', evaluation.errorCategory || 'runner-identity-mismatch');
    if (mjpegPids.length > 1) record(runnerAssociation, 'mismatch', 'mjpeg-pid-count', 'listener-endpoint-cardinality');
    if (mjpegPids.some(pid => pid !== admittedRunner!.pid)) record(runnerAssociation, 'mismatch', 'mjpeg-pid-association', 'listener-endpoint-association');
    if (mjpegAssociation === 'not-evaluated') mjpegAssociation = 'match';
    persist(runnerAssociation, mjpegAssociation, 'validated', 'none');
    return { pid: admittedRunner!.pid, executable: admittedRunner!.executable, birth: admittedRunner!.birth! };
  };
  const stopChild = async (): Promise<void> => {
    if (!child || childEnded) {
      diagnostics.lifecycle('child-stop-skipped', {phase: 'cleanup', childPresent: Boolean(child), childEnded});
      return;
    }
    const requestSignal = (signal: NodeJS.Signals): void => {
      let sent = false;
      try { sent = child?.kill(signal) || false; }
      catch (error) {
        rememberFailure({error, phase: 'cleanup', stage: 'child-signal', category: 'child-signal-error', frozenOwner: Boolean(frozenRunner)});
      }
      diagnostics.lifecycle('child-signal-requested', {phase: 'cleanup', pid: child?.pid, signal, accepted: sent, frozenOwner: Boolean(frozenRunner)});
      if (!sent && !childEnded) rememberFailure({message: `XCTEST: child signal ${signal} was not accepted`, phase: 'cleanup', stage: 'child-signal', category: 'child-signal-rejected', frozenOwner: Boolean(frozenRunner)});
    };
    requestSignal('SIGTERM');
    await Promise.race([exited || Promise.resolve(), sleep(10_000)]);
    if (!childEnded) requestSignal('SIGKILL');
    await Promise.race([exited || Promise.resolve(), sleep(2_000)]);
    if (!childEnded) rememberFailure({message: 'XCTEST: child cleanup did not settle', phase: 'cleanup', stage: 'child-cleanup', category: 'child-cleanup-timeout', frozenOwner: Boolean(frozenRunner)});
    diagnostics.lifecycle('child-stop-finished', {phase: 'cleanup', pid: child.pid, childEnded});
  };
  const waitForOwnedListenersToExit = async (): Promise<void> => {
    const limit = Date.now() + maxRunnerCleanupMs;
    while (Date.now() < limit) {
      const pids = listenerPids('cleanup-wait');
      if (!pids.length) return;
      if (!ownedListeners(pids, 'cleanup-wait')) throw new Error('XCTEST: unknown WDA listener remained during cleanup');
      await sleep(100);
    }
    throw new Error('XCTEST: owned WDA listener cleanup deadline');
  };

  try {
    deadline = Number(readFileSync(join(root, 'deadline'), 'utf8'));
    if (!Number.isSafeInteger(deadline) || deadline <= Date.now() || deadline > Date.now() + maxStartupMs) {
      throw new Error('XCTEST: invalid startup deadline');
    }
    port = portValue('IOS_WDA_PORT', 8100);
    mjpeg = portValue('IOS_WDA_MJPEG_PORT', 9100);
    if (port === mjpeg) throw new Error('XCTEST: WDA and MJPEG ports must differ');
    owner.url = `http://127.0.0.1:${port}`;
    product = realpathSync(required('IOS_WDA_PREBUILT_PATH'));
    owner.product = product;
    productBinaryHash = binaryHash(join(product, 'WebDriverAgentRunner-Runner'));
    owner.cachedProductExecutableHash = productBinaryHash;
    owner.cachedProductExecutableHashAt = new Date().toISOString();
    save();
    ownership(udid);
    if (listeners(port, Math.min(maxProcessCommandMs, remaining()), undefined, commandContext('preflight', 'inspect-listener', {
      endpoint: 'wda', port, frozenOwner: false,
    })).length || listeners(mjpeg, Math.min(maxProcessCommandMs, remaining()), undefined, commandContext('preflight', 'inspect-listener', {
      endpoint: 'mjpeg', port: mjpeg, frozenOwner: false,
    })).length) {
      throw new Error('XCTEST: occupied endpoint');
    }
    const xctestrun = selectXctestrun(required('IOS_WDA_BOOTSTRAP_PATH'), product, deadline, commandContext('preflight', 'select-xctestrun'));
    const packageRoot = dirname(required('IOS_WDA_AGENT_PATH'));
    const packageVersion = JSON.parse(readFileSync(join(packageRoot, 'package.json'), 'utf8')).version;
    if (packageVersion !== version) throw new Error('XCTEST: unpinned WDA version');
    const data = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', xctestrun], remaining(), commandContext('preflight', 'read-xctestrun')));
    data.WebDriverAgentRunner.EnvironmentVariables = {
      ...data.WebDriverAgentRunner.EnvironmentVariables,
      USE_PORT: String(port),
      USE_IP: '127.0.0.1',
      MJPEG_SERVER_PORT: String(mjpeg),
      WDA_PRODUCT_BUNDLE_IDENTIFIER: productId,
    };
    writeFileSync(xctestrun, JSON.stringify(data));
    run('plutil', ['-convert', 'xml1', xctestrun], remaining(), commandContext('preflight', 'write-xctestrun'));
    owner.xctestrun = realpathSync(xctestrun);
    owner.xctestrunHash = binaryHash(owner.xctestrun);
    const selectedXctestrun = safeXctestrunEvidence(data);
    if (Buffer.byteLength(selectedXctestrun) > maxOwnerBytes) throw new Error('XCTEST: selected xctestrun evidence limit');
    writeFileSync(join(root, 'selected.xctestrun'), selectedXctestrun, { mode: 0o600 });
    run('xcrun', ['simctl', 'install', udid, product], remaining(), commandContext('preflight', 'install-wda', {endpoint: 'simulator'}));
    owner.receipt = receipt('preflight-install');
    owner.installReceipt = owner.receipt;
    writeFileSync(join(root, 'installed-Info.plist'), installedInfoEvidence(join(owner.receipt, 'Info.plist'), 'preflight-install'), { mode: 0o600 });
    owner.command = ['xcodebuild', 'test-without-building', '-xctestrun', owner.xctestrun, '-destination', `id=${udid}`];
    diagnostics.lifecycle('child-spawn-requested', {phase: 'startup', command: safeCommand(owner.command[0], owner.command.slice(1)), frozenOwner: false});
    try {
      child = spawn(owner.command[0], owner.command.slice(1), {stdio: ['ignore', 'pipe', 'pipe']});
    } catch (error) {
      rememberFailure({error, phase: 'startup', stage: 'child-spawn', category: 'child-spawn-error'});
      throw error;
    }
    if (!child.pid) throw new Error('XCTEST: xcodebuild did not provide a process identifier');
    owner.pid = child.pid;
    diagnostics.lifecycle('child-spawned', {phase: 'startup', pid: child.pid, frozenOwner: false});
    child.stdout?.on('data', chunk => retain(Buffer.from(chunk), 'stdout'));
    child.stderr?.on('data', chunk => retain(Buffer.from(chunk), 'stderr'));
    exited = new Promise(resolve => {
      child!.on('error', error => {
        diagnostics.lifecycle('child-error', {phase: 'startup', pid: child?.pid, category: 'child-process-error', code: (error as NodeJS.ErrnoException).code});
        rememberFailure({message: 'XCTEST: child process error', phase: 'startup', stage: 'child-process', category: 'child-process-error'});
        save();
      });
      child!.on('exit', (code, signal) => {
        diagnostics.lifecycle('child-exit', {phase: 'startup', pid: child?.pid, exitCode: code, signal, frozenOwner: Boolean(frozenRunner)});
      });
      child!.on('close', (code, signal) => {
        childEnded = true;
        finishOutput();
        owner.exitCode = code;
        owner.signal = signal;
        diagnostics.lifecycle('child-close', {phase: 'startup', pid: child?.pid, exitCode: code, signal, frozenOwner: Boolean(frozenRunner)});
        save();
        resolve();
      });
    });
    try {
      owner.pidBirth = run('ps', ['-p', String(owner.pid), '-o', 'lstart='], Math.min(maxProcessCommandMs, remaining()), commandContext('startup', 'inspect-child-birth', {pid: String(owner.pid)}));
      diagnostics.lifecycle('child-birth-observed', {phase: 'startup', pid: owner.pid, birth: safeProcessBirth(owner.pidBirth, true)});
    } catch (error) {
      rememberFailure({error, phase: 'startup', stage: 'child-pid-birth', category: 'child-pid-birth'});
    }
    save();
    while (!stop && !childEnded && !existsSync(join(root, 'stop'))) {
      remaining();
      try {
        const initial = inspectListeners(deadline, 'initial');
        const initialWdaPids = initial.wda.status === 'evaluated' ? initial.wda.pids || [] : [];
        const initialMjpegPids = initial.mjpeg.status === 'evaluated' ? initial.mjpeg.pids || [] : [];
        if (!initialWdaPids.length && !initialMjpegPids.length) {
          if (frozenRunner) {
            const validation = listenerValidation(initial, initial.commands, 'not-evaluated', 'not-evaluated', {},
              'managed-listener-endpoints-empty', 'managed-listener-endpoint-disappearance', owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
            owner.listenerValidation = validation;
            rememberFailure({message: 'XCTEST: managed WDA listener changed', phase: 'initial', stage: 'managed-listener-endpoints-empty', category: 'managed-listener-endpoint-disappearance', predicate: validation,
              frozenOwner: true, commandIds: snapshotCommandIds(initial), inspectionId: initial.inspectionId});
            save();
            throw new Error('XCTEST: managed WDA listener changed');
          }
          await sleep(Math.min(250, remaining()));
          continue;
        }
        let candidate = validateListenerSnapshot(initial, frozenRunner, deadline, 'initial');
        if (!frozenRunner) {
          try {
            owner.receipt = receipt('after-initial');
          } catch (error) {
            owner.receiptError = receiptErrorCategory(error);
            const message = 'XCTEST: registration receipt validation failed';
            rememberFailure({message, phase: 'after-initial', stage: 'registration-receipt', category: 'registration-receipt-validation', predicate: owner.listenerValidation,
              frozenOwner: false, commandIds: snapshotCommandIds(initial), inspectionId: initial.inspectionId});
            save();
            throw new ReceiptValidationError(message);
          }
          const afterReceipt = inspectListeners(deadline, 'after-receipt');
          candidate = validateListenerSnapshot(afterReceipt, candidate, deadline, 'after-receipt', false);
          frozenRunner = candidate;
          owner.runnerPid = candidate.pid;
          owner.runnerExecutable = candidate.executable;
          owner.runnerBirth = candidate.birth;
          diagnostics.lifecycle('runner-frozen', {
            phase: 'after-receipt',
            pid: candidate.pid,
            birth: safeProcessBirth(candidate.birth, true),
            executable: redactedBounded(candidate.executable, 1_024),
            frozenOwner: true,
          });
          save();
        }
        let status: StatusResponse | undefined;
        const statusTimeout = Math.min(2_000, remaining());
        diagnostics.lifecycle('status-attempt', {
          phase: 'status',
          endpoint: 'wda',
          port,
          timeoutMs: statusTimeout,
          frozenOwner: Boolean(frozenRunner),
        });
        try {
          status = await readStatus(owner.url, statusTimeout);
          diagnostics.lifecycle('status-result', {
            phase: 'status',
            endpoint: 'wda',
            port,
            statusCode: status.statusCode,
            truncated: Boolean(status.truncated),
            frozenOwner: Boolean(frozenRunner),
          });
          owner.status = boundedStatus(status);
          try {
            writeFileSync(join(root, 'wda-status.json'), `${JSON.stringify({checkedAt: new Date().toISOString(), ...boundedStatus(status)}, null, 2)}\n`, { mode: 0o600 });
          } catch {
            const statusError = new Error('XCTEST: WDA status evidence write failed');
            rememberFailure({error: statusError, phase: 'status', stage: 'status-evidence-write', category: 'status-evidence-write', frozenOwner: Boolean(frozenRunner)});
            throw statusError;
          }
          save();
        } catch (error) {
          diagnostics.lifecycle('status-error', {
            phase: 'status',
            endpoint: 'wda',
            port,
            category: 'status-command-error',
            frozenOwner: Boolean(frozenRunner),
          });
          throw error;
        } finally {
          const afterStatus = inspectListeners(deadline, 'after-status');
          validateListenerSnapshot(afterStatus, frozenRunner, deadline, 'after-status', true);
        }
        const statusReady = Boolean(status && status.statusCode === 200 && !status.truncated && validWdaStatus(status.body, required('IOS_PLATFORM_VERSION')));
        diagnostics.lifecycle('readiness-predicate', {
          phase: 'readiness',
          endpoint: 'wda',
          evaluated: Boolean(status),
          result: statusReady,
          frozenOwner: Boolean(frozenRunner),
        });
        if (statusReady) {
          remaining();
          if (childEnded) throw new Error('XCTEST: runner exited during readiness');
          owner.ready = true;
          diagnostics.lifecycle('ready-admitted', {phase: 'readiness', endpoint: 'wda', port, frozenOwner: true});
          save();
          break;
        }
      } catch (error) {
        if (error instanceof ReceiptValidationError || (error instanceof Error && error.message.startsWith('XCTEST:'))) throw error;
      }
      await sleep(Math.min(250, remaining()));
    }
    if (!owner.ready) throw new Error(`XCTEST: runner exited before readiness (${owner.exitCode ?? 'unknown'})`);
    const lifetimeDeadline = Date.now() + 60 * 60_000;
    while (!stop && !childEnded && !existsSync(join(root, 'stop')) && Date.now() < lifetimeDeadline) await sleep(250);
    if (childEnded) rememberFailure({message: 'XCTEST: runner exited during scenario', phase: 'scenario', stage: 'scenario-child-exit', category: 'child-exit', frozenOwner: Boolean(frozenRunner)});
    if (!stop && !childEnded && !existsSync(join(root, 'stop'))) rememberFailure({message: 'XCTEST: scenario owner lifetime limit', phase: 'scenario', stage: 'scenario-lifetime', category: 'scenario-lifetime', frozenOwner: Boolean(frozenRunner)});
  } catch (error) {
    const message = errorMessage(error);
    rememberFailure({error, message, phase: 'supervisor', stage: message === 'XCTEST: startup deadline' ? 'startup-deadline' : 'supervisor-error', category: message === 'XCTEST: startup deadline' ? 'startup-deadline' : 'supervisor-error', frozenOwner: Boolean(frozenRunner)});
  } finally {
    diagnostics.lifecycle('cleanup-enter', {phase: 'cleanup', frozenOwner: Boolean(frozenRunner), firstFailure: owner.firstFailure?.stage});
    owner.ready = false;
    diagnostics.lifecycle('owner-ready-cleared', {phase: 'cleanup', frozenOwner: Boolean(frozenRunner)});
    save();
    let ownedListenerObserved = false;
    try {
      ownership(udid);
      const pids = listenerPids('cleanup');
      diagnostics.lifecycle('cleanup-listeners-observed', {
        phase: 'cleanup',
        pids: pids.slice(0, 16),
        count: pids.length,
        frozenOwner: Boolean(frozenRunner),
      });
      if (pids.length) {
        if (!ownedListeners(pids, 'cleanup-owner-check')) {
          rememberFailure({message: 'XCTEST: refusing unknown WDA listener cleanup', phase: 'cleanup', stage: 'cleanup-listener-ownership', category: 'cleanup-unknown-listener', frozenOwner: Boolean(frozenRunner)});
        } else {
          ownedListenerObserved = true;
          diagnostics.lifecycle('owned-listener-termination-requested', {phase: 'cleanup', pid: owner.runnerPid, frozenOwner: true});
          run('xcrun', ['simctl', 'terminate', udid, runnerId], 10_000, commandContext('cleanup', 'terminate-owned-runner', {
            endpoint: 'simulator', pid: owner.runnerPid, frozenOwner: true,
          }));
          diagnostics.lifecycle('owned-listener-termination-finished', {phase: 'cleanup', pid: owner.runnerPid, frozenOwner: true});
        }
      }
    } catch (error) {
      rememberFailure({error, phase: 'cleanup', stage: 'cleanup-listener', category: 'cleanup-listener-error', frozenOwner: Boolean(frozenRunner)});
    }
    try { await stopChild(); }
    catch (error) {
      rememberFailure({error, phase: 'cleanup', stage: 'child-cleanup', category: 'child-cleanup-error', frozenOwner: Boolean(frozenRunner)});
    }
    if (ownedListenerObserved) {
      try { await waitForOwnedListenersToExit(); }
      catch (error) {
        rememberFailure({error, phase: 'cleanup', stage: 'owned-listener-cleanup', category: 'owned-listener-cleanup-error', frozenOwner: true});
      }
    }
    try {
      ownership(udid);
      const result = executeSync('xcrun', ['simctl', 'spawn', udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process CONTAINS[c] "WebDriverAgent" OR process == "xctest"'], 10_000, 16_777_216, commandContext('cleanup', 'collect-system-log', {endpoint: 'simulator', frozenOwner: Boolean(frozenRunner)}));
      const stdoutBytes = Buffer.byteLength(result.stdout || '');
      const stderrBytes = Buffer.byteLength(result.stderr || '');
      diagnostics.lifecycle('system-log-collected', {phase: 'cleanup', status: result.status, signal: result.signal, stdoutBytes, stderrBytes, output: 'suppressed'});
      writeFileSync(join(root, 'ios-wda-system.log'), `${JSON.stringify({schema: 1, status: result.status, signal: result.signal, stdoutBytes, stderrBytes, output: 'suppressed'})}\n`, { mode: 0o600 });
    } catch (error) {
      rememberFailure({error, phase: 'cleanup', stage: 'system-log', category: 'system-log-error', frozenOwner: Boolean(frozenRunner)});
    }
    try {
      finishOutput();
      if (existsSync(log)) {
        const safeLog = redactedBounded(readFileSync(log, 'utf8'), maxStartupLogBytes);
        writeFileSync(log, safeLog, { mode: 0o600 });
        writeFileSync(publicLog, safeLog, { mode: 0o600 });
      }
    } catch (error) {
      rememberFailure({error, message: 'XCTEST: startup log finalization failed', phase: 'cleanup', stage: 'startup-log-finalization', category: 'startup-log-finalization-error', frozenOwner: Boolean(frozenRunner)});
    }
    owner.endedAt = new Date().toISOString();
    diagnostics.lifecycle('owner-ended', {phase: 'cleanup', endedAt: owner.endedAt, firstFailure: owner.firstFailure?.stage});
    save();
    process.off('SIGTERM', onSignal);
    process.off('SIGINT', onSignal);
  }
}

export async function managedWdaCapabilities(udid: string): Promise<Record<string, unknown>> {
  const root = required('IOS_XCTEST_STATE_DIR');
  const owner: Owner = JSON.parse(readFileSync(join(root, 'owner-private.json'), 'utf8'));
  ownership(udid);
  const product = realpathSync(required('IOS_WDA_PREBUILT_PATH'));
  const productBinaryHash = binaryHash(join(product, 'WebDriverAgentRunner-Runner'));
  if (!owner.ready || owner.endedAt || owner.udid !== udid || !owner.pid || !owner.runnerPid || !owner.runnerExecutable || !owner.receipt || !owner.pidBirth || !owner.runnerBirth
    || !owner.xctestrun || !owner.xctestrunHash || owner.product !== product || !/^http:\/\/127\.0\.0\.1:[1-9]\d{3,4}$/u.test(owner.url)
    || binaryHash(owner.xctestrun) !== owner.xctestrunHash) {
    throw new Error('XCTEST: missing live managed owner');
  }
  const inspectionDeadline = Date.now() + maxProcessCommandMs * 3;
  const command = run('ps', ['-p', String(owner.pid), '-o', 'command='], deadlineTimeout(inspectionDeadline));
  const birth = run('ps', ['-p', String(owner.pid), '-o', 'lstart='], deadlineTimeout(inspectionDeadline));
  if (!command.includes(`-destination id=${udid}`) || !commandContainsPath(command, owner.xctestrun)
    || !command.includes('test-without-building') || birth !== owner.pidBirth) {
    throw new Error('XCTEST: managed owner changed');
  }
  const pids = listeners(Number(new URL(owner.url).port), deadlineTimeout(inspectionDeadline));
  const mjpegPids = listeners(portValue('IOS_WDA_MJPEG_PORT', 9100), deadlineTimeout(inspectionDeadline));
  if (pids.length !== 1 || pids[0] !== owner.runnerPid || mjpegPids.length > 1 || mjpegPids.some(pid => pid !== owner.runnerPid)) {
    throw new Error('XCTEST: managed WDA listener changed');
  }
  const listener = processEvidence(owner.runnerPid, inspectionDeadline);
  if (listener.birth !== owner.runnerBirth || listener.executable !== owner.runnerExecutable
    || !runnerExecutableMatches(listener.executable, product, udid, inspectionDeadline, productBinaryHash)) {
    throw new Error('XCTEST: managed WDA executable changed');
  }
  const status = await readStatus(owner.url, 2_000);
  if (status.statusCode !== 200 || status.truncated || !validWdaStatus(status.body, required('IOS_PLATFORM_VERSION'))) {
    throw new Error('XCTEST: managed WDA not ready');
  }
  const afterStatusDeadline = Date.now() + maxProcessCommandMs * 3;
  const afterStatusPids = listeners(Number(new URL(owner.url).port), deadlineTimeout(afterStatusDeadline));
  const afterStatusMjpegPids = listeners(portValue('IOS_WDA_MJPEG_PORT', 9100), deadlineTimeout(afterStatusDeadline));
  if (afterStatusPids.length !== 1 || afterStatusPids[0] !== owner.runnerPid || afterStatusMjpegPids.length > 1
    || afterStatusMjpegPids.some(pid => pid !== owner.runnerPid)) {
    throw new Error('XCTEST: managed WDA listener changed');
  }
  const afterStatusListener = processEvidence(owner.runnerPid, afterStatusDeadline);
  if (afterStatusListener.birth !== owner.runnerBirth || afterStatusListener.executable !== owner.runnerExecutable
    || !runnerExecutableMatches(afterStatusListener.executable, product, udid, afterStatusDeadline, productBinaryHash)) {
    throw new Error('XCTEST: managed WDA executable changed');
  }
  return { 'appium:webDriverAgentUrl': owner.url };
}

async function main(): Promise<void> {
  const mode = process.argv[2];
  const root = required('IOS_XCTEST_STATE_DIR');
  if (mode === 'supervise') {
    await supervise();
    return;
  }
  if (mode === 'start') {
    mkdirSync(root, { recursive: false, mode: 0o700 });
    const deadline = Date.now() + maxStartupMs;
    writeFileSync(join(root, 'deadline'), String(deadline), { mode: 0o600 });
    const owner: Owner = {
      udid: required('IOS_SIMULATOR_UDID'),
      product: process.env.IOS_WDA_PREBUILT_PATH || '',
      url: `http://127.0.0.1:${process.env.IOS_WDA_PORT || '8100'}`,
      startedAt: new Date().toISOString(),
      ready: false,
    };
    writeOwner(root, owner);
    const child = spawn(process.execPath, [fileURLToPath(import.meta.url), 'supervise'], { detached: true, stdio: 'ignore', env: process.env });
    let spawnError: Error | undefined;
    child.on('error', error => {
      spawnError = error;
      try {
        const current: Owner = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
        current.error ||= 'XCTEST: supervisor spawn failed';
        current.endedAt ||= new Date().toISOString();
        writeOwner(root, current);
      } catch (error) {
        spawnError ||= error instanceof Error ? error : new Error(String(error));
      }
    });
    child.unref();
    while (Date.now() < deadline) {
      if (spawnError) throw spawnError;
      if (existsSync(join(root, 'owner.json'))) {
        const current: Owner = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
        if (current.ready) return;
        if (current.endedAt) throw new Error(current.error || 'XCTEST: startup exited');
      }
      await sleep(100);
    }
    writeFileSync(join(root, 'stop'), 'startup deadline', { mode: 0o600 });
    throw new Error('XCTEST: startup deadline');
  }
  if (mode === 'stop') {
    if (!existsSync(root)) return;
    ownership(required('IOS_SIMULATOR_UDID'));
    const privateOwnerPath = join(root, 'owner-private.json');
    const ownerPath = existsSync(privateOwnerPath) ? privateOwnerPath : join(root, 'owner.json');
    if (!existsSync(ownerPath)) throw new Error('XCTEST: missing owner state');
    const readOwner = (): Owner => JSON.parse(readFileSync(ownerPath, 'utf8'));
    if (!readOwner().endedAt) writeFileSync(join(root, 'stop'), 'owner cleanup', { mode: 0o600 });
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      if (existsSync(ownerPath)) {
        const current = readOwner();
        if (current.endedAt) {
          if (current.error) throw new Error(current.error);
          return;
        }
      }
      await sleep(100);
    }
    throw new Error('XCTEST: supervisor cleanup deadline');
  }
  throw new Error('XCTEST: expected start, stop or supervise');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main().catch(error => {
    console.error(redactText(error instanceof Error ? error.message : String(error)));
    process.exitCode = 1;
  });
}

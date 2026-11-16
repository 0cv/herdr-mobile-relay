import { spawn, spawnSync, type ChildProcess, type SpawnSyncReturns } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve, basename } from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { StringDecoder } from 'node:string_decoder';
import { redactText, sanitizeValue } from './diagnostics';
import { XCTestOutputDiagnostics, type XCTestOutputSummary } from './xctest-output';

const runnerId = 'com.facebook.WebDriverAgentRunner.xctrunner';
const productId = 'com.facebook.WebDriverAgentRunner';
const version = '16.12.8';
const maxStartupMs = 300_000;
const maxStartupLogBytes = 100 * 1024 * 1024;
const startupOutputMarker = '[startup output suppressed]\n';
const maxStatusBytes = 65_536;
const maxProcessCommandMs = 2_000;
const maxRunnerCleanupMs = 10_000;
const maxCleanupMs = 30_000;
const maxPreFreezeDiagnosticMs = 8_000;
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
const yieldToChildExit = (): Promise<void> => new Promise(resolve => setImmediate(resolve));

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
  const classification = operation === 'inspect-listener' ? 'listener command' : operation ? `${operation} command` : 'command';
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

function listeners(port: number, timeout = maxProcessCommandMs, observed?: (status: number | null, stderrBytes: number) => void, context?: DiagnosticContext): string[] {
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

interface ProcessEvidence {
  pid: string;
  executable: string;
  command: string;
  birth: string;
}

interface PartialProcessEvidence {
  pid: string;
  executable?: string;
  birth?: string;
}

interface ReceiptSnapshot {
  path: string;
  applicationPathMatches: boolean;
  bundleIdentifierMatches?: boolean;
  installedExecutableHash?: string;
  productExecutableHash?: string;
  executableHashMatches?: boolean;
  failureStage?: 'bundle-id' | 'installed-hash' | 'product-hash';
  failureCategory?: 'command-error' | 'timeout';
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

function diagnosticProcessEvidence(pid: string, deadline: number, context?: DiagnosticContext): {evidence: PartialProcessEvidence; error?: unknown} {
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

function runnerExecutableMatches(executable: string, product: string, udid: string, deadline: number, productBinaryHash?: string, evaluation?: RunnerMatchEvidence, context?: DiagnosticContext, boundedHash = false): boolean {
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
  stderr?: 'present';
}

interface CleanupListenerSnapshot {
  wda: ListenerEndpointSnapshot;
  mjpeg: ListenerEndpointSnapshot;
}

interface CleanupListenerObservation {
  phase: 'cleanup';
  checkedAt: string;
  monotonicMs: number;
  endpoints: {
    wda: {observation: ListenerEndpointDiagnostic; commandId?: number};
    mjpeg: {observation: ListenerEndpointDiagnostic; commandId?: number};
  };
  commandIds: number[];
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

type DiagnosticCheckStatus = 'evaluated' | 'rejected' | 'not-evaluated' | 'budget-exhausted';

interface DiagnosticCheck {
  status: DiagnosticCheckStatus;
  matches?: boolean;
  category?: string;
}

interface DiagnosticHashCheck extends DiagnosticCheck {
  expected?: string;
  actual?: string;
}

type DiagnosticPathShape = {
  [key in 'applicationNameMatches' | 'containerUuid' | 'simulatorPath' | 'executableName']: DiagnosticCheck;
};

interface InitialCandidateDiagnostic {
  schema: 1;
  phase: 'initial-diagnostic';
  candidatePid: string;
  initial: {
    status: DiagnosticCheckStatus;
    birth?: string;
    executable: DiagnosticCheck;
  };
  current: {
    status: DiagnosticCheckStatus;
    category?: string;
    birth?: string;
    executable: DiagnosticCheck;
  };
  comparisons: {
    pid: DiagnosticCheck;
    birth: DiagnosticCheck;
    executable: DiagnosticCheck;
    pathShape: DiagnosticPathShape;
    runnerBundleId: DiagnosticCheck;
    runnerExecutableHash: DiagnosticHashCheck;
    productExecutableHash: DiagnosticHashCheck;
    refreshedReceipt: {
      status: DiagnosticCheckStatus;
      category?: string;
      sameAsInstallReceipt: DiagnosticCheck;
      applicationPath: DiagnosticCheck;
      bundleId: DiagnosticCheck;
      executableHash: DiagnosticHashCheck;
    };
  };
  cleanup: {
    status: DiagnosticCheckStatus;
    wda: DiagnosticCheck;
    mjpeg: DiagnosticCheck;
  };
  startedAtMonotonicMs: number;
  endedAtMonotonicMs: number;
  budgetMs: number;
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
  cleanupListenerObservation?: CleanupListenerObservation;
  initialCandidateDiagnostic?: InitialCandidateDiagnostic;
  pid?: number;
  command?: string[];
  runnerPid?: string;
  runnerExecutable?: string;
  receipt?: string;
  installReceipt?: string;
  listenerEvidence?: ListenerEvidence[];
  listenerValidation?: ListenerValidation;
  firstFailure?: FirstFailure;
  exitRevocation?: { primaryWriteFailed?: true; callbackFailed?: true; persistencePrerequisite?: 'failed' };
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
  xctestOutput?: XCTestOutputSummary;
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

const boundedStatusMarker = Symbol('boundedStatus');
type BoundedStatus = StatusResponse & { readonly [boundedStatusMarker]: true };

function boundedStatus(status: StatusResponse): BoundedStatus {
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
  const bounded: StatusResponse = {
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
  Object.defineProperty(bounded, boundedStatusMarker, {value: true});
  return bounded as BoundedStatus;
}

function isBoundedStatus(status: StatusResponse): status is BoundedStatus {
  return typeof status === 'object' && status !== null
    && (status as Partial<BoundedStatus>)[boundedStatusMarker] === true;
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
    ...(value.status ? {status: isBoundedStatus(value.status) ? value.status : boundedStatus(value.status)} : {}),
  };
}

function ownerContent(owner: Owner, sanitized: boolean): string {
  const bounded = boundedOwnerValue(owner, sanitized);
  const value = (sanitized ? sanitizeOwnerValue(sanitizeValue(bounded)) : bounded) as Owner;
  const serialize = (entry: Owner): string => `${JSON.stringify(entry, null, 2)}\n`;
  const content = serialize(value);
  if (Buffer.byteLength(content) <= maxOwnerBytes) return content;
  const reduced: Owner = {
    ...value,
    ...(value.listenerEvidence ? {listenerEvidence: value.listenerEvidence.slice(0, 1)} : {}),
    ...(value.diagnostics ? {diagnostics: {...value.diagnostics, commands: [], inspections: [], lifecycle: []}} : {}),
    ...(value.xctestOutput ? {xctestOutput: {...value.xctestOutput, events: [], droppedEvents: value.xctestOutput.droppedEvents + value.xctestOutput.events.length}} : {}),
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

function writeStatusEvidence(root: string, status: BoundedStatus): void {
  const next = join(root, 'wda-status.next.json');
  writeFileSync(next, `${JSON.stringify({checkedAt: new Date().toISOString(), ...status}, null, 2)}\n`, { mode: 0o600 });
  renameSync(next, join(root, 'wda-status.json'));
}

function binaryHash(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

function boundedBinaryHash(path: string, deadline: number, context?: DiagnosticContext): string {
  const script = "const{createHash}=require('node:crypto');const{readFileSync}=require('node:fs');process.stdout.write(createHash('sha256').update(readFileSync(process.argv[1])).digest('hex'));";
  const hash = run(process.execPath, ['-e', script, path], deadlineTimeout(deadline), context);
  if (!/^[0-9a-f]{64}$/u.test(hash)) throw new Error('XCTEST: binary hash is unavailable');
  return hash;
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
  observedRunnerPid?: string,
): ListenerValidation {
  const runnerPid = observedRunnerPid ?? (snapshot.wda.status === 'evaluated' ? snapshot.wda.pids?.[0] : undefined);
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

function diagnosticCheck(value: boolean | undefined, budgetExhausted = false, category?: string): DiagnosticCheck {
  if (value === undefined) return {status: budgetExhausted ? 'budget-exhausted' : 'not-evaluated'};
  return value
    ? {status: 'evaluated', matches: true}
    : {status: 'rejected', matches: false, ...(category ? {category} : {})};
}

function diagnosticHashCheck(
  value: RunnerMatchEvidence['executableHash'] | undefined,
  budgetExhausted = false,
): DiagnosticHashCheck {
  if (!value) return {status: budgetExhausted ? 'budget-exhausted' : 'not-evaluated'};
  if (value.status === 'error') return {
    status: 'rejected',
    category: 'runner-command-error',
    ...(value.expected ? {expected: value.expected} : {}),
    ...(value.actual ? {actual: value.actual} : {}),
  };
  if (value.status === 'not-evaluated') return {
    status: budgetExhausted ? 'budget-exhausted' : 'not-evaluated',
    ...(value.expected ? {expected: value.expected} : {}),
  };
  return {
    status: value.matches ? 'evaluated' : 'rejected',
    ...(value.matches !== undefined ? {matches: value.matches} : {}),
    ...(value.matches === false ? {category: 'runner-hash-mismatch'} : {}),
    ...(value.expected ? {expected: value.expected} : {}),
    ...(value.actual ? {actual: value.actual} : {}),
  };
}

function diagnosticPathShape(evaluation: RunnerMatchEvidence, budgetExhausted = false): DiagnosticPathShape {
  const path = evaluation.pathShape;
  return {
    applicationNameMatches: diagnosticCheck(path?.applicationNameMatches, budgetExhausted, 'runner-path-mismatch'),
    containerUuid: diagnosticCheck(path?.containerUuid, budgetExhausted, 'runner-path-mismatch'),
    simulatorPath: diagnosticCheck(path?.simulatorPath, budgetExhausted, 'runner-path-mismatch'),
    executableName: diagnosticCheck(path?.executableName, budgetExhausted, 'runner-path-mismatch'),
  };
}

type StartupOutputStream = 'stdout' | 'stderr';

interface StartupOutputState {
  decoder: StringDecoder;
  recordCount: number;
  incompleteUtf8: boolean;
  finalized: boolean;
}

interface SupervisorState {
  root: string;
  udid: string;
  productInput: string;
  owner: Owner;
  diagnostics: WdaDiagnosticsRecorder;
  xctestOutput: XCTestOutputDiagnostics;
  log: string;
  publicLog: string;
  streamBytes: Record<StartupOutputStream, number>;
  outputStates: Record<StartupOutputStream, StartupOutputState>;
  stop: boolean;
  deadline: number;
  port: number;
  mjpeg: number;
  product: string;
  productBinaryHash: string;
  child: ChildProcess | undefined;
  exited: Promise<void> | undefined;
  childExitObserved: Promise<void> | undefined;
  childEnded: boolean;
  childExited: boolean;
  childErrorObserved: boolean;
  frozenRunner: RunnerIdentity | undefined;
  pendingRunner: RunnerIdentity | undefined;
  pendingInitialSnapshot: ListenerSnapshot | undefined;
  initialCandidateSnapshot: ListenerSnapshot | undefined;
  cleanupListenerSnapshot: CleanupListenerSnapshot | undefined;
  safeLogBytes: number;
  receivedLogBytes: number;
  rawOutputSuppressed: boolean;
}

function createSupervisorState(): SupervisorState {
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
  const xctestOutput = new XCTestOutputDiagnostics(() => ({
    observedAt: new Date().toISOString(), monotonicReceiveMs: Number(diagnostics.now().toFixed(3)),
  }));
  owner.xctestOutput = xctestOutput.data;
  return {
    root,
    udid,
    productInput,
    owner,
    diagnostics,
    xctestOutput,
    log: join(root, 'wda-preflight-private.log'),
    publicLog: process.env.STARTUP_TEST_LOG_FINALIZATION_TARGET || join(root, 'wda-preflight.log'),
    streamBytes: {stdout: 0, stderr: 0},
    outputStates: {
      stdout: {decoder: new StringDecoder('utf8'), recordCount: 0, incompleteUtf8: false, finalized: false},
      stderr: {decoder: new StringDecoder('utf8'), recordCount: 0, incompleteUtf8: false, finalized: false},
    },
    stop: false,
    deadline: 0,
    port: 0,
    mjpeg: 0,
    product: '',
    productBinaryHash: '',
    child: undefined,
    exited: undefined,
    childExitObserved: undefined,
    childEnded: false,
    childExited: false,
    childErrorObserved: false,
    frozenRunner: undefined,
    pendingRunner: undefined,
    pendingInitialSnapshot: undefined,
    initialCandidateSnapshot: undefined,
    cleanupListenerSnapshot: undefined,
    safeLogBytes: 0,
    receivedLogBytes: 0,
    rawOutputSuppressed: false,
  };
}

function commandContext(state: SupervisorState, phase: string, operation: string, extra: Partial<Pick<DiagnosticContext, 'endpoint' | 'port' | 'pid' | 'frozenOwner'>> = {}): DiagnosticContext {
  return {
    recorder: state.diagnostics,
    phase,
    source: diagnosticSource,
    operation,
    ...extra,
  };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function receiptErrorCategory(error: unknown): string {
  const message = errorMessage(error);
  if (message.includes('ENOENT') || message.includes('no such file')) return 'receipt-unavailable';
  if (message.startsWith('XCTEST: xcrun failed')) return 'receipt-command-failed';
  if (message === 'XCTEST: registration receipt mismatch') return 'receipt-mismatch';
  return 'receipt-validation-error';
}

function safeFailureMessage(stage: string, category: string): string {
  if (stage === 'startup-deadline') return 'XCTEST: startup deadline';
  if (stage === 'startup-log-limit') return 'XCTEST: log output limit';
  if (stage === 'registration-receipt') return 'XCTEST: registration receipt validation failed';
  if (stage === 'status-evidence-write') return 'XCTEST: WDA status evidence write failed';
  if (stage.startsWith('managed-')) return 'XCTEST: managed WDA listener changed';
  if (stage.startsWith('pending-') || ['wda-pid-count', 'wda-runner-evidence', 'wda-executable-evidence'].includes(stage)) return 'XCTEST: unknown WDA listener';
  return `XCTEST: ${category}`;
}

interface FailureRecord {
  error?: unknown;
  message?: string;
  phase: string;
  stage: string;
  category: string;
  predicate?: ListenerValidation;
  frozenOwner?: boolean;
  commandIds?: number[];
  inspectionId?: number;
}

function rememberFailure(state: SupervisorState, failure: FailureRecord): void {
  const message = safeFailureMessage(failure.stage, failure.category);
  const commandIds = (failure.commandIds ?? state.diagnostics.recentCommandIds()).slice(-16);
  const causalCommands = state.diagnostics.commandsForIds(commandIds);
  const first = !state.owner.firstFailure;
  state.owner.error ||= message;
  state.diagnostics.lifecycle('failure-observed', {
    phase: failure.phase,
    stage: failure.stage,
    category: failure.category,
    message,
    first,
    frozenOwner: failure.frozenOwner ?? Boolean(state.frozenRunner),
    ...(commandIds.length ? {commandIds} : {}),
    ...(failure.inspectionId !== undefined ? {inspectionId: failure.inspectionId} : {}),
  });
  if (first) {
    state.owner.firstFailure = {
      at: new Date().toISOString(),
      monotonicMs: Number(state.diagnostics.now().toFixed(3)),
      phase: failure.phase,
      source: diagnosticSource,
      stage: failure.stage,
      category: failure.category,
      message,
      frozenOwner: failure.frozenOwner ?? Boolean(state.frozenRunner),
      predicate: failure.predicate
        ? {status: 'evaluated', listenerValidation: JSON.parse(JSON.stringify(failure.predicate)) as ListenerValidation}
        : {status: 'not-evaluated'},
      commandIds,
      causalCommands: JSON.parse(JSON.stringify(causalCommands)) as CommandDiagnostic[],
      ...(failure.inspectionId !== undefined ? {inspectionId: failure.inspectionId} : {}),
    };
  }
}

function snapshotCommandIds(state: SupervisorState, snapshot: ListenerSnapshot): number[] {
  return snapshot.commandStart === undefined
    ? snapshot.commandIds || []
    : state.diagnostics.commandIdsFrom(snapshot.commandStart);
}

function saveFailure(state: SupervisorState): void {
  state.owner.ready = false;
  try {
    writeFileSync(join(state.root, 'owner-private.json'), ownerContent(state.owner, false), { mode: 0o600 });
    writeFileSync(join(state.root, 'owner.json'), ownerContent(state.owner, true), { mode: 0o600 });
  } catch {
    return;
  }
}

function save(state: SupervisorState): boolean {
  try {
    writeOwner(state.root, state.owner);
    return true;
  } catch {
    rememberFailure(state, {message: 'XCTEST: owner evidence write failed', phase: 'supervisor', stage: 'owner-evidence-write', category: 'owner-evidence-write'});
    state.stop = true;
    saveFailure(state);
    return false;
  }
}

// Exit callbacks cannot rely on save(): its diagnostic catch-body may throw.
function rememberExitFailure(state: SupervisorState, stage: 'child-exit' | 'child-exit-callback' | 'owner-evidence-write'): void {
  try {
    rememberFailure(state, {phase: 'startup', stage, category: stage, frozenOwner: Boolean(state.frozenRunner)});
  } catch {
    // Keep an already selected causal record intact, even if diagnostics fail.
    try {
      state.owner.error ||= `XCTEST: ${stage}`;
      state.owner.firstFailure ||= {
        at: new Date().toISOString(), monotonicMs: Number(state.diagnostics.now().toFixed(3)),
        phase: 'startup', source: diagnosticSource, stage, category: stage,
        message: `XCTEST: ${stage}`, frozenOwner: Boolean(state.frozenRunner),
        predicate: {status: 'not-evaluated'}, commandIds: [], causalCommands: [],
      };
    } catch { /* The callback still must revoke and resolve. */ }
  }
}

function saveExitRevocation(state: SupervisorState): void {
  state.owner.ready = false;
  try {
    writeOwner(state.root, state.owner);
    return;
  } catch {
    state.owner.exitRevocation = {...state.owner.exitRevocation, primaryWriteFailed: true};
    rememberExitFailure(state, 'owner-evidence-write');
  }
  // Two atomic replacements, not a transaction. A private fault must not
  // bypass public revocation. Alternate owned paths avoid obstructed .nexts.
  for (const sanitized of [false, true]) {
    const target = join(state.root, sanitized ? 'owner.json' : 'owner-private.json');
    const next = join(state.root, sanitized ? 'owner-exit.next.json' : 'owner-private-exit.next.json');
    try {
      writeFileSync(next, ownerContent(state.owner, sanitized), {mode: 0o600});
      renameSync(next, target);
    } catch {
      try {
        unlinkSync(target);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
        // Unwritable AND non-invalidatable disk is a failed prerequisite,
        // never a promise that stale disk readiness was revoked.
        state.owner.exitRevocation.persistencePrerequisite = 'failed';
        process.exitCode ||= 1;
      }
    }
  }
}

function remaining(state: SupervisorState): number {
  const value = state.deadline - Date.now();
  if (value <= 0) throw new Error('XCTEST: startup deadline');
  return value;
}

async function ensureChildRunning(state: SupervisorState, phase: string): Promise<void> {
  if (!state.child || state.childExited || state.childEnded) throw new Error(`XCTEST: runner exited during ${phase}`);
  let alive = state.child.exitCode === null && state.child.signalCode === null;
  if (alive) {
    try {
      process.kill(state.child.pid!, 0);
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === 'ESRCH') alive = false;
      else if (code !== 'EPERM') throw error;
    }
  }
  if (alive) {
    const childState = executeSync('ps', ['-p', String(state.child.pid), '-o', 'stat='], Math.min(maxProcessCommandMs, remaining(state)), 4_096,
      commandContext(state, 'startup', 'inspect-child-state', {pid: String(state.child.pid), frozenOwner: Boolean(state.frozenRunner)}));
    if ((childState.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
    if (childState.error) throw new Error('XCTEST: cannot inspect child state');
    alive = childState.status === 0 && !/^Z/u.test(childState.stdout.trim());
    await yieldToChildExit();
    if (state.childExited || state.childEnded || state.child.exitCode !== null || state.child.signalCode !== null) alive = false;
  }
  if (!alive) {
    if (state.childExitObserved) await Promise.race([state.childExitObserved, sleep(Math.min(1_000, remaining(state)))]);
    if (!state.childExited && !state.childEnded) throw new Error(`XCTEST: child exit was not observed during ${phase}`);
    throw new Error(`XCTEST: runner exited during ${phase}`);
  }
}

function updateOutputAccounting(state: SupervisorState): void {
  state.owner.logBytes = state.safeLogBytes;
  state.owner.safeLogBytes = state.safeLogBytes;
  state.owner.receivedLogBytes = state.receivedLogBytes;
  state.owner.receivedStreamBytes = {...state.streamBytes};
  state.owner.pendingLogBytes = 0;
}

function writeSafeLog(state: SupervisorState, text: string): void {
  const encoded = Buffer.from(text);
  const available = Math.max(0, maxStartupLogBytes - state.safeLogBytes);
  if (!encoded.length || !available) {
    updateOutputAccounting(state);
    return;
  }
  const retained = encoded.subarray(0, available);
  try {
    appendFileSync(state.log, retained, { mode: 0o600 });
    state.safeLogBytes += retained.length;
  } catch {
    rememberFailure(state, {message: 'XCTEST: startup log write failed', phase: 'startup', stage: 'startup-log-write', category: 'startup-log-write'});
    state.stop = true;
  }
  updateOutputAccounting(state);
}

function recordStartupChunk(state: SupervisorState, outputState: StartupOutputState, chunk: Buffer, stream: StartupOutputStream): void {
  const decoded = outputState.decoder.write(chunk);
  outputState.recordCount += decoded.split('\n').length - 1;
  state.xctestOutput.observe(decoded, stream);
}

function markLogLimit(state: SupervisorState, stream: StartupOutputStream, streamOffset: number, chunkBytes: number, acceptedBytes: number): void {
  if (state.rawOutputSuppressed) return;
  state.rawOutputSuppressed = true;
  state.owner.logTruncated = true;
  state.xctestOutput.data.inputTruncated = true;
  state.owner.startupOutputLimit = {
    limitBytes: maxStartupLogBytes,
    receivedBytes: state.receivedLogBytes,
    stream,
    streamOffset,
    chunkBytes,
    acceptedBytes,
    overLimitBytes: state.receivedLogBytes - maxStartupLogBytes,
  };
  state.diagnostics.lifecycle('startup-output-limit', {
    phase: 'startup',
    stream,
    streamOffset,
    chunkBytes,
    acceptedBytes,
    receivedBytes: state.receivedLogBytes,
    limitBytes: maxStartupLogBytes,
    overLimitBytes: state.receivedLogBytes - maxStartupLogBytes,
  });
  rememberFailure(state, {message: 'XCTEST: log output limit', phase: 'startup', stage: 'startup-log-limit', category: 'output-limit'});
  writeSafeLog(state, startupOutputMarker);
  updateOutputAccounting(state);
  state.stop = true;
}

function retain(state: SupervisorState, chunk: Buffer, stream: StartupOutputStream): void {
  const offset = state.streamBytes[stream];
  state.streamBytes[stream] += chunk.length;
  const receivedBefore = state.receivedLogBytes;
  state.receivedLogBytes += chunk.length;
  state.diagnostics.lifecycle('child-output', {
    phase: 'startup',
    stream,
    offset,
    bytes: chunk.length,
    monotonicReceiveMs: Number(state.diagnostics.now().toFixed(3)),
  });
  const acceptedBytes = Math.max(0, Math.min(chunk.length, maxStartupLogBytes - receivedBefore));
  if (!state.outputStates[stream].finalized && acceptedBytes) {
    if (!state.rawOutputSuppressed && state.safeLogBytes === 0) writeSafeLog(state, startupOutputMarker);
    recordStartupChunk(state, state.outputStates[stream], chunk.subarray(0, acceptedBytes), stream);
  }
  if (acceptedBytes < chunk.length) markLogLimit(state, stream, offset + acceptedBytes, chunk.length, acceptedBytes);
  updateOutputAccounting(state);
}

function finishOutput(state: SupervisorState): void {
  for (const [stream, outputState] of Object.entries(state.outputStates) as Array<[StartupOutputStream, StartupOutputState]>) {
    if (outputState.finalized) continue;
    const tail = outputState.decoder.end();
    outputState.incompleteUtf8 = tail.length > 0;
    state.xctestOutput.observe(tail, stream);
    state.xctestOutput.finish(stream);
    if (!state.rawOutputSuppressed && state.streamBytes[stream] > 0) {
      writeSafeLog(state, `[startup output summary; stream=${stream}; bytes=${state.streamBytes[stream]}; records=${outputState.recordCount}; utf8Complete=${!outputState.incompleteUtf8}]\n`);
    }
    outputState.finalized = true;
  }
  updateOutputAccounting(state);
}

function plistEvidence(format: 'xml' | 'json' | 'binary', status: 'parsed' | 'suppressed' | 'conversion-failed', bundleIdentifier: 'known' | 'unrecognized' | 'not-evaluated'): string {
  return `${JSON.stringify({
    schema: 1,
    format,
    status,
    bundleIdentifier,
    values: 'suppressed',
  }, null, 2)}\n`;
}

function plistBundleIdentifier(value: unknown): 'known' | 'unrecognized' | 'not-evaluated' {
  const bundleIdentifier = value && typeof value === 'object' ? (value as Record<string, unknown>).CFBundleIdentifier : undefined;
  if (bundleIdentifier === runnerId) return 'known';
  return typeof bundleIdentifier === 'string' ? 'unrecognized' : 'not-evaluated';
}

function installedInfoEvidence(state: SupervisorState, path: string, phase: string): string {
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
    const converted = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', path], Math.min(10_000, remaining(state)), commandContext(state, phase, 'read-installed-info', {endpoint: 'wda', port: state.port})));
    return plistEvidence('binary', 'parsed', plistBundleIdentifier(converted));
  } catch {
    return plistEvidence('binary', 'conversion-failed', 'not-evaluated');
  }
}

function observeReceipt(state: SupervisorState, phase: string, persist: boolean, receiptDeadline: number, diagnostic = false, boundedHash = false): ReceiptSnapshot {
  const installed = realpathSync(run('xcrun', ['simctl', 'get_app_container', state.udid, runnerId, 'app'], deadlineTimeout(receiptDeadline), commandContext(state, phase, 'read-install-receipt', {endpoint: 'wda', port: state.port})));
  if (persist) {
    state.owner.receipt = installed;
    writeFileSync(join(state.root, 'installed-Info.plist'), installedInfoEvidence(state, join(installed, 'Info.plist'), phase), { mode: 0o600 });
  }
  const applicationDirectory = dirname(dirname(installed));
  const expectedApplicationDirectory = `/Devices/${state.udid}/data/Containers/Bundle/Application`;
  const applicationPathMatches = uuidPattern.test(basename(dirname(installed))) && basename(installed) === basename(state.product)
    && applicationDirectory.endsWith(expectedApplicationDirectory);
  if (!applicationPathMatches) return {path: installed, applicationPathMatches};
  let bundleIdentifierMatches: boolean;
  try {
    bundleIdentifierMatches = run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(installed, 'Info.plist')], Math.min(10_000, deadlineTimeout(receiptDeadline)), commandContext(state, phase, 'validate-install-receipt-bundle', {endpoint: 'wda', port: state.port})) === runnerId;
  } catch (error) {
    if (!diagnostic) throw error;
    return {
      path: installed,
      applicationPathMatches,
      failureStage: 'bundle-id',
      failureCategory: error instanceof Error && error.message === 'XCTEST: startup deadline' ? 'timeout' : 'command-error',
    };
  }
  if (!bundleIdentifierMatches) return {path: installed, applicationPathMatches, bundleIdentifierMatches};
  const hash = (path: string, operation: string): string => boundedHash
    ? boundedBinaryHash(path, receiptDeadline, commandContext(state, phase, operation, {endpoint: 'wda', port: state.port}))
    : binaryHash(path);
  let installedExecutableHash: string;
  try {
    installedExecutableHash = hash(join(installed, 'WebDriverAgentRunner-Runner'), 'hash-install-receipt');
  } catch (error) {
    if (!diagnostic) throw error;
    return {
      path: installed,
      applicationPathMatches,
      bundleIdentifierMatches,
      failureStage: 'installed-hash',
      failureCategory: error instanceof Error && error.message === 'XCTEST: startup deadline' ? 'timeout' : 'command-error',
    };
  }
  let productExecutableHash: string;
  try {
    productExecutableHash = hash(join(state.product, 'WebDriverAgentRunner-Runner'), 'hash-product');
  } catch (error) {
    if (!diagnostic) throw error;
    return {
      path: installed,
      applicationPathMatches,
      bundleIdentifierMatches,
      installedExecutableHash,
      failureStage: 'product-hash',
      failureCategory: error instanceof Error && error.message === 'XCTEST: startup deadline' ? 'timeout' : 'command-error',
    };
  }
  return {
    path: installed,
    applicationPathMatches,
    bundleIdentifierMatches,
    installedExecutableHash,
    productExecutableHash,
    executableHashMatches: installedExecutableHash === productExecutableHash,
  };
}

function receipt(state: SupervisorState, phase: string): string {
  const observed = observeReceipt(state, phase, true, state.deadline);
  if (!observed.applicationPathMatches || observed.bundleIdentifierMatches !== true || observed.executableHashMatches !== true) {
    throw new Error('XCTEST: registration receipt mismatch');
  }
  return observed.path;
}

function listenerCommandErrorCategory(error: unknown): string {
  if (error instanceof Error && error.message === 'XCTEST: startup deadline') return 'timeout';
  if (error instanceof Error && error.message.includes('invalid process identifier')) return 'invalid-process-id';
  return 'command-error';
}

function persistListenerCommandFailure(state: SupervisorState, snapshot: ListenerSnapshot, failureStage: string, error: unknown, phase: string): void {
  const category = listenerCommandErrorCategory(error);
  const deadline = category === 'timeout';
  const diagnosticStage = deadline ? 'startup-deadline' : failureStage;
  const diagnosticCategory = deadline ? 'startup-deadline'
    : category === 'invalid-process-id' ? 'listener-invalid-process-id' : 'listener-command-error';
  const validation = listenerValidation(snapshot, snapshot.commands, 'not-evaluated', 'not-evaluated', {}, diagnosticStage,
    diagnosticCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
  state.owner.listenerValidation = validation;
  rememberFailure(state, {error, phase, stage: diagnosticStage, category: diagnosticCategory, predicate: validation,
    frozenOwner: Boolean(state.frozenRunner), commandIds: snapshotCommandIds(state, snapshot), inspectionId: snapshot.inspectionId});
  save(state);
}

function inspectListeners(state: SupervisorState, inspectionDeadline: number, phase = 'initial'): ListenerSnapshot {
  const token = state.diagnostics.beginInspection(phase, Boolean(state.frozenRunner));
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
    commandIds: state.diagnostics.commandIdsFrom(token.commandStart),
  });
  try {
    try {
      wdaEndpoint = {status: 'evaluated', pids: listeners(state.port, deadlineTimeout(inspectionDeadline), (status, stderrBytes) => commands.push({port: state.port, status, ...(stderrBytes ? {stderr: 'present'} : {})}), commandContext(state, phase, 'inspect-listener', {
        endpoint: 'wda', port: state.port, frozenOwner: Boolean(state.frozenRunner),
      }))};
    } catch (error) {
      failure = error;
      wdaEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
      persistListenerCommandFailure(state, snapshot(), 'wda-listener-command', error, phase);
      throw error;
    }
    try {
      mjpegEndpoint = {status: 'evaluated', pids: listeners(state.mjpeg, deadlineTimeout(inspectionDeadline), (status, stderrBytes) => commands.push({port: state.mjpeg, status, ...(stderrBytes ? {stderr: 'present'} : {})}), commandContext(state, phase, 'inspect-listener', {
        endpoint: 'mjpeg', port: state.mjpeg, frozenOwner: Boolean(state.frozenRunner),
      }))};
    } catch (error) {
      failure = error;
      mjpegEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
      persistListenerCommandFailure(state, snapshot(), 'mjpeg-listener-command', error, phase);
      throw error;
    }
    const pids = wdaEndpoint.pids || [];
    const mjpegPids = mjpegEndpoint.pids || [];
    for (const pid of [...new Set([...pids, ...mjpegPids])]) {
      try {
        const endpoint = pids.includes(pid) ? 'wda' : 'mjpeg';
        evidence.push(processEvidence(pid, inspectionDeadline, commandContext(state, phase, 'inspect-listener-process', {
          endpoint, port: endpoint === 'wda' ? state.port : state.mjpeg, pid, frozenOwner: Boolean(state.frozenRunner),
        })));
      } catch (error) {
        failure = error;
        evidence.push({pid, executable: '[unavailable]'});
        state.diagnostics.lifecycle('listener-process-evidence-error', {
          phase,
          pid,
          category: listenerCommandErrorCategory(error),
          frozenOwner: Boolean(state.frozenRunner),
        });
        state.owner.listenerEvidence = evidence;
        save(state);
        if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
          const current = snapshot();
          if (phase === 'initial' && !state.initialCandidateSnapshot) state.initialCandidateSnapshot = current;
          const validation = listenerValidation(current, commands, 'not-evaluated', 'not-evaluated', {}, 'listener-process-evidence', 'listener-process-timeout', state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
          state.owner.listenerValidation = validation;
          rememberFailure(state, {error, phase, stage: 'listener-process-evidence', category: 'listener-process-timeout', predicate: validation,
            frozenOwner: Boolean(state.frozenRunner), commandIds: snapshotCommandIds(state, current), inspectionId: current.inspectionId});
          save(state);
          throw error;
        }
      }
    }
    state.owner.listenerEvidence = evidence;
    save(state);
    return snapshot();
  } catch (error) {
    failure ||= error;
    throw error;
  } finally {
    state.diagnostics.finishInspection(token, {wda: endpointDiagnostic(wdaEndpoint), mjpeg: endpointDiagnostic(mjpegEndpoint)},
      [...new Set([...(wdaEndpoint.pids || []), ...(mjpegEndpoint.pids || [])])], failure);
  }
}

function retainCleanupListenerObservation(
  state: SupervisorState,
  wda: ListenerEndpointSnapshot,
  wdaCommandId: number | undefined,
  mjpeg: ListenerEndpointSnapshot,
  mjpegCommandId: number | undefined,
  commandIds: number[],
): void {
  if (state.owner.cleanupListenerObservation) return;
  state.cleanupListenerSnapshot = {wda, mjpeg};
  state.owner.cleanupListenerObservation = {
    phase: 'cleanup',
    checkedAt: new Date().toISOString(),
    monotonicMs: Number(state.diagnostics.now().toFixed(3)),
    endpoints: {
      wda: {observation: endpointDiagnostic(wda), ...(wdaCommandId !== undefined ? {commandId: wdaCommandId} : {})},
      mjpeg: {observation: endpointDiagnostic(mjpeg), ...(mjpegCommandId !== undefined ? {commandId: mjpegCommandId} : {})},
    },
    commandIds: [...new Set(commandIds)].slice(0, 4),
  };
  save(state);
}

function listenerPids(state: SupervisorState, inspectionDeadline: number, phase = 'cleanup'): string[] {
  const query = (endpoint: 'wda' | 'mjpeg', listenerPort: number): {
    snapshot: ListenerEndpointSnapshot;
    commandId?: number;
    error?: unknown;
  } => {
    const commandStart = state.diagnostics.checkpoint();
    try {
      const pids = listeners(listenerPort, deadlineTimeout(inspectionDeadline), undefined, commandContext(state, phase, 'inspect-listener', {
        endpoint, port: listenerPort, frozenOwner: Boolean(state.frozenRunner),
      }));
      return {snapshot: {status: 'evaluated', pids}, commandId: state.diagnostics.commandIdsFrom(commandStart).at(-1)};
    } catch (error) {
      return {
        snapshot: {status: 'error', errorCategory: listenerCommandErrorCategory(error)},
        commandId: state.diagnostics.commandIdsFrom(commandStart).at(-1),
        error,
      };
    }
  };
  const wda = query('wda', state.port);
  if (wda.error) {
    const commandIds = [wda.commandId].filter((id): id is number => id !== undefined);
    retainCleanupListenerObservation(state, wda.snapshot, wda.commandId, {status: 'not-evaluated'}, undefined, commandIds);
    throw wda.error;
  }
  const mjpegResult = query('mjpeg', state.mjpeg);
  const commandIds = [wda.commandId, mjpegResult.commandId].filter((id): id is number => id !== undefined);
  if (mjpegResult.error) {
    retainCleanupListenerObservation(state, wda.snapshot, wda.commandId, mjpegResult.snapshot, mjpegResult.commandId, commandIds);
    throw mjpegResult.error;
  }
  retainCleanupListenerObservation(state, wda.snapshot, wda.commandId, mjpegResult.snapshot, mjpegResult.commandId, commandIds);
  return [...new Set([...(wda.snapshot.pids || []), ...(mjpegResult.snapshot.pids || [])])];
}

function ownedListeners(state: SupervisorState, pids: string[], phaseDeadline: number, phase = 'cleanup'): boolean {
  if (!state.owner.runnerPid || !state.owner.runnerBirth || !state.owner.runnerExecutable || pids.length === 0 || pids.some(pid => pid !== state.owner.runnerPid)) return false;
  try {
    const inspectionDeadline = Math.min(phaseDeadline, Date.now() + maxProcessCommandMs * 3);
    const evidence = processEvidence(state.owner.runnerPid, inspectionDeadline, commandContext(state, phase, 'inspect-managed-listener-process', {
      endpoint: 'managed-listener', port: state.port, pid: state.owner.runnerPid, frozenOwner: true,
    }));
    return evidence.birth === state.owner.runnerBirth && evidence.executable === state.owner.runnerExecutable
      && runnerExecutableMatches(evidence.executable, state.owner.product, state.udid, inspectionDeadline, state.productBinaryHash, undefined, commandContext(state, phase, 'validate-managed-listener', {
        endpoint: 'managed-listener', port: state.port, pid: state.owner.runnerPid, frozenOwner: true,
      }));
  } catch {
    return false;
  }
}

function validateListenerSnapshot(state: SupervisorState, snapshot: ListenerSnapshot, expected: RunnerIdentity | undefined, inspectionDeadline: number, phase = 'initial', frozenOwner = Boolean(state.frozenRunner)): RunnerIdentity {
  const wdaPids = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids || [] : [];
  const mjpegPids = snapshot.mjpeg.status === 'evaluated' ? snapshot.mjpeg.pids || [] : [];
  const runner = snapshot.wda.status === 'evaluated' ? snapshot.evidence.find(entry => entry.pid === wdaPids[0]) : undefined;
  const admittedRunner = runner as ListenerEvidence | undefined;
  const evaluation: RunnerMatchEvidence = {};
  const causalCommandIds = (): number[] => snapshotCommandIds(state, snapshot);
  let runnerAssociation: ListenerValidation['runnerAssociation'] = 'not-evaluated';
  let mjpegAssociation: ListenerValidation['mjpegAssociation'] = 'not-evaluated';
  const persist = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): void => {
    state.owner.listenerValidation = listenerValidation(snapshot, snapshot.commands, observedRunnerAssociation, observedMjpegAssociation, evaluation, failureStage, errorCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
    save(state);
  };
  const record = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): never => {
    const validation = listenerValidation(snapshot, snapshot.commands, observedRunnerAssociation, observedMjpegAssociation, evaluation, failureStage, errorCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
    state.owner.listenerValidation = validation;
    const message = failureStage.startsWith('managed-') ? 'XCTEST: managed WDA listener changed' : 'XCTEST: unknown WDA listener';
    rememberFailure(state, {message, phase, stage: failureStage, category: errorCategory, predicate: validation,
      frozenOwner, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
    save(state);
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
    executableMatch = runnerExecutableMatches(admittedRunner!.executable, state.product, state.udid, inspectionDeadline, state.productBinaryHash, evaluation, commandContext(state, phase, 'validate-runner-identity', {
      endpoint: 'wda', port: state.port, pid: admittedRunner!.pid, frozenOwner,
    }));
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
      const failureStage = evaluation.failureStage || 'runner-executable-deadline';
      const failureCategory = evaluation.errorCategory || 'listener-deadline';
      const validation = listenerValidation(snapshot, snapshot.commands, runnerAssociation, mjpegAssociation, evaluation, failureStage, failureCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
      state.owner.listenerValidation = validation;
      rememberFailure(state, {error, phase, stage: failureStage, category: failureCategory, predicate: validation,
        frozenOwner, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
      save(state);
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
}

function validatePendingSnapshot(state: SupervisorState, snapshot: ListenerSnapshot, expected: RunnerIdentity | undefined, inspectionDeadline: number, phase = 'initial', allowHttp = false): RunnerIdentity {
  const wdaPids = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids || [] : [];
  const mjpegPids = snapshot.mjpeg.status === 'evaluated' ? snapshot.mjpeg.pids || [] : [];
  const candidatePid = expected?.pid || (mjpegPids.length === 1 ? mjpegPids[0] : undefined);
  const observed = candidatePid ? snapshot.evidence.find(entry => entry.pid === candidatePid) : undefined;
  const evaluation: RunnerMatchEvidence = {};
  const causalCommandIds = (): number[] => snapshotCommandIds(state, snapshot);
  let runnerAssociation: ListenerValidation['runnerAssociation'] = 'not-evaluated';
  let mjpegAssociation: ListenerValidation['mjpegAssociation'] = 'not-evaluated';
  const persist = (failureStage: string, errorCategory: string): void => {
    state.owner.listenerValidation = listenerValidation(snapshot, snapshot.commands, runnerAssociation, mjpegAssociation, evaluation,
      failureStage, errorCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt, candidatePid);
    if (!save(state)) throw new Error('XCTEST: owner evidence write failed');
  };
  const record = (failureStage: string, errorCategory: string): never => {
    const validation = listenerValidation(snapshot, snapshot.commands, runnerAssociation, mjpegAssociation, evaluation,
      failureStage, errorCategory, state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt, candidatePid);
    state.owner.listenerValidation = validation;
    const message = 'XCTEST: unknown WDA listener';
    rememberFailure(state, {message, phase, stage: failureStage, category: errorCategory, predicate: validation,
      frozenOwner: false, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
    save(state);
    throw new Error(message);
  };
  const recordDeadline = (error: unknown): never => {
    const validation = listenerValidation(snapshot, snapshot.commands, runnerAssociation, mjpegAssociation, evaluation,
      'startup-deadline', 'startup-deadline', state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt, candidatePid);
    state.owner.listenerValidation = validation;
    rememberFailure(state, {error, phase, stage: 'startup-deadline', category: 'startup-deadline', predicate: validation,
      frozenOwner: false, commandIds: causalCommandIds(), inspectionId: snapshot.inspectionId});
    save(state);
    throw error;
  };
  if (state.childExited || state.owner.firstFailure || state.stop || existsSync(join(state.root, 'stop'))) {
    if (state.childExited) record('pending-child-exit', 'child-exit');
    throw new Error('XCTEST: startup stopped before WDA listener admission');
  }
  if (snapshot.commands.find(command => command.port === state.port)?.stderr) record('wda-pid-count', 'listener-command-error');
  if (snapshot.commands.find(command => command.port === state.mjpeg)?.stderr) record('mjpeg-pid-count', 'listener-command-error');
  if (snapshot.wda.status !== 'evaluated' || wdaPids.length !== (allowHttp ? 1 : 0)) record('pending-wda-pid-count', 'listener-endpoint-cardinality');
  if (snapshot.mjpeg.status !== 'evaluated' || mjpegPids.length > 1 || (!allowHttp && mjpegPids.length !== 1)) record('pending-mjpeg-pid-count', 'listener-endpoint-cardinality');
  if (allowHttp && expected && mjpegPids.some(pid => pid !== expected.pid)) record('pending-mjpeg-pid', 'managed-listener-identity-mismatch');
  if (allowHttp && expected && wdaPids[0] !== expected.pid) record('pending-runner-pid', 'managed-listener-identity-mismatch');
  if (!candidatePid || !observed) record('pending-mjpeg-runner-evidence', 'listener-process-evidence');
  if (!observed?.executable || observed.executable === '[unavailable]') record('pending-mjpeg-executable-evidence', 'listener-process-evidence');
  if (!observed?.birth) record('pending-mjpeg-birth-evidence', 'listener-process-evidence');
  if (expected) {
    if (observed!.pid !== expected.pid) record('pending-runner-pid', 'managed-listener-identity-mismatch');
    if (observed!.executable !== expected.executable) record('pending-runner-executable', 'managed-listener-identity-mismatch');
    if (observed!.birth !== expected.birth) record('pending-runner-birth', 'managed-listener-identity-mismatch');
  }
  runnerAssociation = allowHttp ? 'match' : 'not-evaluated';
  mjpegAssociation = 'match';
  let executableMatch = false;
  try {
    executableMatch = runnerExecutableMatches(observed!.executable, state.product, state.udid, inspectionDeadline, state.productBinaryHash, evaluation,
      commandContext(state, phase, 'validate-pending-runner-identity', {
        endpoint: allowHttp ? 'wda' : 'mjpeg', port: allowHttp ? state.port : state.mjpeg, pid: observed!.pid, frozenOwner: false,
      }), true);
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') recordDeadline(error);
    record(evaluation.failureStage || 'pending-runner-executable-observation', evaluation.errorCategory || 'runner-command-error');
  }
  if (!executableMatch) record(evaluation.failureStage ? `pending-${evaluation.failureStage}` : 'pending-runner-identity', evaluation.errorCategory || 'runner-identity-mismatch');
  try {
    ownership(state.udid);
  } catch {
    record('pending-simulator-ownership', 'simulator-ownership-mismatch');
  }
  if (!state.owner.xctestrun || !state.owner.xctestrunHash) record('pending-xctestrun-evidence', 'xctestrun-identity-mismatch');
  try {
    if (realpathSync(state.owner.xctestrun!) !== state.owner.xctestrun) record('pending-xctestrun-path', 'xctestrun-identity-mismatch');
  } catch {
    record('pending-xctestrun-path', 'xctestrun-identity-error');
  }
  let currentXctestrunHash: string;
  try {
    currentXctestrunHash = boundedBinaryHash(state.owner.xctestrun!, inspectionDeadline,
      commandContext(state, phase, 'validate-pending-xctestrun', {endpoint: 'simulator', frozenOwner: false}));
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') recordDeadline(error);
    record('pending-xctestrun-hash', 'xctestrun-identity-error');
  }
  if (currentXctestrunHash! !== state.owner.xctestrunHash) record('pending-xctestrun-hash', 'xctestrun-identity-mismatch');
  let currentProductHash: string;
  try {
    currentProductHash = boundedBinaryHash(join(state.product, 'WebDriverAgentRunner-Runner'), inspectionDeadline,
      commandContext(state, phase, 'validate-pending-product', {endpoint: 'simulator', frozenOwner: false}));
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') recordDeadline(error);
    record('pending-product-hash', 'product-identity-error');
  }
  if (currentProductHash! !== state.productBinaryHash) record('pending-product-hash', 'product-identity-mismatch');
  try {
    if (realpathSync(state.productInput) !== state.product) record('pending-product-path', 'product-identity-mismatch');
  } catch {
    record('pending-product-path', 'product-identity-error');
  }
  persist('pending', 'none');
  return {pid: observed!.pid, executable: observed!.executable, birth: observed!.birth!};
}

interface CandidateDiagnosticBudget {
  deadline: number;
  exhausted: () => boolean;
  exhaustedBy: (error: unknown) => boolean;
}

type CandidateComparisons = InitialCandidateDiagnostic['comparisons'];

function unavailableCheck(isExhausted: boolean): DiagnosticCheck {
  return {status: isExhausted ? 'budget-exhausted' : 'not-evaluated'};
}

function recordInitialCandidateDiagnostic(state: SupervisorState, snapshot: ListenerSnapshot, phaseDeadline: number, cleanupSnapshot?: CleanupListenerSnapshot): void {
  if (state.owner.initialCandidateDiagnostic || state.frozenRunner || snapshot.wda.status !== 'evaluated' || snapshot.mjpeg.status !== 'evaluated') return;
  const wdaPids = snapshot.wda.pids || [];
  const mjpegPids = snapshot.mjpeg.pids || [];
  if (wdaPids.length !== 0 || mjpegPids.length !== 1) return;
  const candidatePid = mjpegPids[0];
  if (!candidatePid) return;
  const cleanupCheck = (endpoint: ListenerEndpointSnapshot | undefined): DiagnosticCheck => {
    if (!endpoint) return {status: 'not-evaluated'};
    if (endpoint.status === 'evaluated') return diagnosticCheck((endpoint.pids || []).includes(candidatePid), false, 'cleanup-candidate-association');
    if (endpoint.status === 'error') return {status: 'rejected', category: 'cleanup-command-error'};
    return {status: 'not-evaluated'};
  };
  const cleanupWda = cleanupCheck(cleanupSnapshot?.wda);
  const cleanupMjpeg = cleanupCheck(cleanupSnapshot?.mjpeg);
  const cleanup = {
    status: !cleanupSnapshot ? 'not-evaluated' as DiagnosticCheckStatus
      : cleanupSnapshot.wda.status === 'error' || cleanupSnapshot.mjpeg.status === 'error' ? 'rejected' as DiagnosticCheckStatus
        : cleanupSnapshot.wda.status === 'evaluated' && cleanupSnapshot.mjpeg.status === 'evaluated' ? 'evaluated' as DiagnosticCheckStatus
          : 'not-evaluated' as DiagnosticCheckStatus,
    wda: cleanupWda,
    mjpeg: cleanupMjpeg,
  };
  const startedAtMonotonicMs = Number(state.diagnostics.now().toFixed(3));
  const budgetMs = Math.max(0, Math.min(maxPreFreezeDiagnosticMs, state.deadline - Date.now(), phaseDeadline - Date.now()));
  const diagnosticDeadline = Date.now() + budgetMs;
  const budgetExhausted = (): boolean => Date.now() >= diagnosticDeadline || Date.now() >= state.deadline;
  const budget: CandidateDiagnosticBudget = {
    deadline: diagnosticDeadline,
    exhausted: budgetExhausted,
    exhaustedBy: (error: unknown): boolean => budgetExhausted()
      || error instanceof Error && error.message === 'XCTEST: startup deadline',
  };
  const initialEvidence = snapshot.evidence.find(entry => entry.pid === candidatePid);
  const initialExecutableAvailable = Boolean(initialEvidence?.executable && initialEvidence.executable !== '[unavailable]');
  const initialBirthAvailable = Boolean(initialEvidence?.birth);
  const initialStatus: DiagnosticCheckStatus = initialEvidence && initialExecutableAvailable && initialBirthAvailable
    ? 'evaluated' : 'not-evaluated';
  const initial = {
    status: initialStatus,
    ...(initialEvidence?.birth ? {birth: safeProcessBirth(initialEvidence.birth, true)} : {}),
    executable: diagnosticCheck(initialExecutableAvailable ? true : undefined, false, 'process-executable-unavailable'),
  };
  const observed = diagnoseCurrentCandidate(state, candidatePid, initialEvidence, initialExecutableAvailable, budget);
  const runnerBudgetExhausted = diagnoseCandidateRunner(state, candidatePid, observed, budget);
  diagnoseCandidateProductHash(state, runnerBudgetExhausted, budget, observed.comparisons);
  diagnoseRefreshedReceipt(state, budget, observed.comparisons);
  const endedAtMonotonicMs = Number(state.diagnostics.now().toFixed(3));
  state.owner.initialCandidateDiagnostic = {
    schema: 1,
    phase: 'initial-diagnostic',
    candidatePid,
    initial,
    current: observed.current,
    comparisons: observed.comparisons,
    cleanup,
    startedAtMonotonicMs,
    endedAtMonotonicMs,
    budgetMs,
  };
  save(state);
}

function diagnoseCurrentCandidate(
  state: SupervisorState,
  candidatePid: string,
  initialEvidence: ListenerEvidence | undefined,
  initialExecutableAvailable: boolean,
  budget: CandidateDiagnosticBudget,
): {currentEvidence: PartialProcessEvidence | undefined; currentBudgetExhausted: boolean; current: InitialCandidateDiagnostic['current']; comparisons: CandidateComparisons} {
  let currentEvidence: PartialProcessEvidence | undefined;
  let currentError: unknown;
  if (!budget.exhausted()) {
    const observation = diagnosticProcessEvidence(candidatePid, budget.deadline, commandContext(state, 'initial-diagnostic', 'inspect-candidate-process', {
      endpoint: 'mjpeg', port: state.mjpeg, pid: candidatePid, frozenOwner: false,
    }));
    currentError = observation.error;
    if (observation.evidence.executable || observation.evidence.birth) currentEvidence = observation.evidence;
  }
  const currentBudgetExhausted = currentError ? budget.exhaustedBy(currentError) : budget.exhausted();
  const currentExecutableAvailable = Boolean(currentEvidence?.executable);
  const currentBirthAvailable = Boolean(currentEvidence?.birth);
  const current: InitialCandidateDiagnostic['current'] = {
    status: currentEvidence && currentExecutableAvailable && currentBirthAvailable
      ? 'evaluated'
      : currentBudgetExhausted ? 'budget-exhausted'
        : currentError ? 'rejected' : 'not-evaluated',
    ...(currentError && !currentBudgetExhausted ? {category: 'process-evidence-command-error'} : {}),
    ...(currentEvidence?.birth ? {birth: safeProcessBirth(currentEvidence.birth, true)} : {}),
    executable: currentExecutableAvailable
      ? diagnosticCheck(true)
      : unavailableCheck(currentBudgetExhausted),
  };
  const comparisons: CandidateComparisons = {
    pid: currentEvidence
      ? {status: 'evaluated', matches: currentEvidence.pid === candidatePid}
      : unavailableCheck(currentBudgetExhausted),
    birth: initialEvidence?.birth && currentEvidence?.birth
      ? diagnosticCheck(initialEvidence.birth === currentEvidence.birth, false, 'runner-birth-mismatch')
      : unavailableCheck(currentBudgetExhausted),
    executable: initialExecutableAvailable && currentExecutableAvailable && initialEvidence?.executable && currentEvidence
      ? diagnosticCheck(initialEvidence.executable === currentEvidence.executable, false, 'runner-executable-changed')
      : unavailableCheck(currentBudgetExhausted),
    pathShape: diagnosticPathShape({}, currentBudgetExhausted),
    runnerBundleId: {status: currentBudgetExhausted ? 'budget-exhausted' : 'not-evaluated'},
    runnerExecutableHash: diagnosticHashCheck(undefined, currentBudgetExhausted),
    productExecutableHash: diagnosticHashCheck(undefined, currentBudgetExhausted),
    refreshedReceipt: {
      status: currentBudgetExhausted ? 'budget-exhausted' : 'not-evaluated',
      sameAsInstallReceipt: unavailableCheck(currentBudgetExhausted),
      applicationPath: unavailableCheck(currentBudgetExhausted),
      bundleId: unavailableCheck(currentBudgetExhausted),
      executableHash: diagnosticHashCheck(undefined, currentBudgetExhausted),
    },
  };
  return {currentEvidence, currentBudgetExhausted, current, comparisons};
}

function diagnoseCandidateRunner(
  state: SupervisorState,
  candidatePid: string,
  observed: {currentEvidence: PartialProcessEvidence | undefined; currentBudgetExhausted: boolean; comparisons: CandidateComparisons},
  budget: CandidateDiagnosticBudget,
): boolean {
  const {currentEvidence, comparisons} = observed;
  let runnerEvaluation: RunnerMatchEvidence = {};
  let runnerBudgetExhausted = observed.currentBudgetExhausted;
  if (currentEvidence?.executable && !budget.exhausted()) {
    runnerEvaluation = {};
    try {
      runnerExecutableMatches(currentEvidence.executable, state.product, state.udid, budget.deadline, state.productBinaryHash, runnerEvaluation, commandContext(state, 'initial-diagnostic', 'validate-candidate-identity', {
        endpoint: 'mjpeg', port: state.mjpeg, pid: candidatePid, frozenOwner: false,
      }), true);
    } catch (error) {
      runnerBudgetExhausted = budget.exhaustedBy(error);
    }
  }
  comparisons.pathShape = diagnosticPathShape(runnerEvaluation, runnerBudgetExhausted);
  const bundleEvaluation = runnerEvaluation.bundleId;
  comparisons.runnerBundleId = bundleEvaluation?.status === 'error'
    ? {status: 'rejected', category: 'runner-command-error'}
    : diagnosticCheck(bundleEvaluation?.matches, runnerBudgetExhausted, 'runner-bundle-mismatch');
  comparisons.runnerExecutableHash = diagnosticHashCheck(runnerEvaluation.executableHash, runnerBudgetExhausted);
  return runnerBudgetExhausted;
}

function diagnoseCandidateProductHash(state: SupervisorState, runnerBudgetExhausted: boolean, budget: CandidateDiagnosticBudget, comparisons: CandidateComparisons): void {
  if (!runnerBudgetExhausted && !budget.exhausted()) {
    try {
      const currentProductHash = boundedBinaryHash(join(state.product, 'WebDriverAgentRunner-Runner'), budget.deadline,
        commandContext(state, 'initial-diagnostic', 'hash-product', {endpoint: 'wda', port: state.port}));
      comparisons.productExecutableHash = {
        ...diagnosticCheck(currentProductHash === state.productBinaryHash, false, 'product-hash-changed'),
        expected: state.productBinaryHash,
        actual: currentProductHash,
      };
    } catch (error) {
      const productBudgetExhausted = budget.exhaustedBy(error);
      comparisons.productExecutableHash = {
        status: productBudgetExhausted ? 'budget-exhausted' : 'rejected',
        ...(productBudgetExhausted ? {} : {category: 'product-hash-unavailable'}),
        expected: state.productBinaryHash,
      };
    }
  } else {
    comparisons.productExecutableHash = diagnosticHashCheck(undefined, runnerBudgetExhausted || budget.exhausted());
    comparisons.productExecutableHash.expected = state.productBinaryHash;
  }
}

function diagnoseRefreshedReceipt(state: SupervisorState, budget: CandidateDiagnosticBudget, comparisons: CandidateComparisons): void {
  if (!budget.exhausted()) {
    try {
      const refreshed = observeReceipt(state, 'initial-diagnostic', false, budget.deadline, true, true);
      const receiptBudgetExhausted = refreshed.failureCategory === 'timeout' || budget.exhausted();
      const sameAsInstallReceipt = state.owner.installReceipt
        ? diagnosticCheck(refreshed.path === state.owner.installReceipt, false, 'receipt-changed')
        : unavailableCheck(receiptBudgetExhausted);
      const bundleId = refreshed.bundleIdentifierMatches !== undefined
        ? diagnosticCheck(refreshed.bundleIdentifierMatches, false, 'receipt-bundle-mismatch')
        : refreshed.failureStage === 'bundle-id'
          ? {status: receiptBudgetExhausted ? 'budget-exhausted' : 'rejected', ...(receiptBudgetExhausted ? {} : {category: 'receipt-command-error'})} as DiagnosticCheck
          : unavailableCheck(receiptBudgetExhausted);
      const receiptHash = refreshed.installedExecutableHash
        ? {
            ...diagnosticCheck(refreshed.installedExecutableHash === state.productBinaryHash, false, 'receipt-runner-hash-mismatch'),
            expected: state.productBinaryHash,
            actual: refreshed.installedExecutableHash,
          }
        : refreshed.failureStage === 'installed-hash'
          ? {status: receiptBudgetExhausted ? 'budget-exhausted' : 'rejected', ...(receiptBudgetExhausted ? {} : {category: 'receipt-command-error'})} as DiagnosticHashCheck
          : diagnosticHashCheck(undefined, receiptBudgetExhausted);
      comparisons.refreshedReceipt = {
        status: receiptBudgetExhausted ? 'budget-exhausted'
          : refreshed.applicationPathMatches && refreshed.bundleIdentifierMatches === true && refreshed.executableHashMatches === true ? 'evaluated' : 'rejected',
        ...(refreshed.failureStage ? {category: receiptBudgetExhausted ? 'receipt-timeout' : 'receipt-command-error'} : {}),
        sameAsInstallReceipt,
        applicationPath: diagnosticCheck(refreshed.applicationPathMatches, false, 'receipt-path-mismatch'),
        bundleId,
        executableHash: receiptHash,
      };
    } catch (error) {
      const receiptBudgetExhausted = budget.exhaustedBy(error);
      comparisons.refreshedReceipt = {
        status: receiptBudgetExhausted ? 'budget-exhausted' : 'rejected',
        category: receiptBudgetExhausted ? 'receipt-timeout' : 'receipt-command-error',
        sameAsInstallReceipt: unavailableCheck(receiptBudgetExhausted),
        applicationPath: unavailableCheck(receiptBudgetExhausted),
        bundleId: unavailableCheck(receiptBudgetExhausted),
        executableHash: diagnosticHashCheck(undefined, receiptBudgetExhausted),
      };
    }
  } else {
    comparisons.refreshedReceipt = {
      status: 'budget-exhausted',
      category: 'receipt-timeout',
      sameAsInstallReceipt: unavailableCheck(true),
      applicationPath: unavailableCheck(true),
      bundleId: unavailableCheck(true),
      executableHash: diagnosticHashCheck(undefined, true),
    };
  }
}

async function stopChild(state: SupervisorState): Promise<void> {
  if (!state.child || state.childEnded || state.childExited) {
    state.diagnostics.lifecycle('child-stop-skipped', {phase: 'cleanup', childPresent: Boolean(state.child), childEnded: state.childEnded, childExited: state.childExited, childErrorObserved: state.childErrorObserved});
    if (state.childExited && !state.childEnded) {
      await Promise.race([state.exited || Promise.resolve(), sleep(10_000)]);
      if (!state.childEnded) rememberFailure(state, {message: 'XCTEST: child cleanup did not settle', phase: 'cleanup', stage: 'child-cleanup', category: 'child-cleanup-timeout', frozenOwner: Boolean(state.frozenRunner)});
      state.diagnostics.lifecycle('child-stop-finished', {phase: 'cleanup', pid: state.child?.pid, childEnded: state.childEnded});
    }
    return;
  }
  const requestSignal = (signal: NodeJS.Signals): void => {
    let sent = false;
    try { sent = state.child?.kill(signal) || false; }
    catch (error) {
      rememberFailure(state, {error, phase: 'cleanup', stage: 'child-signal', category: 'child-signal-error', frozenOwner: Boolean(state.frozenRunner)});
    }
    state.diagnostics.lifecycle('child-signal-requested', {phase: 'cleanup', pid: state.child?.pid, signal, accepted: sent, frozenOwner: Boolean(state.frozenRunner)});
    if (!sent && !state.childEnded) rememberFailure(state, {message: `XCTEST: child signal ${signal} was not accepted`, phase: 'cleanup', stage: 'child-signal', category: 'child-signal-rejected', frozenOwner: Boolean(state.frozenRunner)});
  };
  requestSignal('SIGTERM');
  await Promise.race([state.exited || Promise.resolve(), sleep(10_000)]);
  if (!state.childEnded) requestSignal('SIGKILL');
  await Promise.race([state.exited || Promise.resolve(), sleep(2_000)]);
  if (!state.childEnded) rememberFailure(state, {message: 'XCTEST: child cleanup did not settle', phase: 'cleanup', stage: 'child-cleanup', category: 'child-cleanup-timeout', frozenOwner: Boolean(state.frozenRunner)});
  state.diagnostics.lifecycle('child-stop-finished', {phase: 'cleanup', pid: state.child.pid, childEnded: state.childEnded});
}

async function waitForOwnedListenersToExit(state: SupervisorState, cleanupDeadline: number): Promise<void> {
  const limit = Math.min(cleanupDeadline, Date.now() + maxRunnerCleanupMs);
  while (Date.now() < limit) {
    const pids = listenerPids(state, limit, 'cleanup-wait');
    if (!pids.length) return;
    if (!ownedListeners(state, pids, limit, 'cleanup-wait')) throw new Error('XCTEST: unknown WDA listener remained during cleanup');
    await sleep(Math.max(0, Math.min(100, limit - Date.now())));
  }
  throw new Error('XCTEST: owned WDA listener cleanup deadline');
}

function preflight(state: SupervisorState): void {
  state.deadline = Number(readFileSync(join(state.root, 'deadline'), 'utf8'));
  if (!Number.isSafeInteger(state.deadline) || state.deadline <= Date.now() || state.deadline > Date.now() + maxStartupMs) {
    throw new Error('XCTEST: invalid startup deadline');
  }
  state.port = portValue('IOS_WDA_PORT', 8100);
  state.mjpeg = portValue('IOS_WDA_MJPEG_PORT', 9100);
  if (state.port === state.mjpeg) throw new Error('XCTEST: WDA and MJPEG ports must differ');
  state.owner.url = `http://127.0.0.1:${state.port}`;
  state.product = realpathSync(required('IOS_WDA_PREBUILT_PATH'));
  state.owner.product = state.product;
  state.productBinaryHash = binaryHash(join(state.product, 'WebDriverAgentRunner-Runner'));
  state.owner.cachedProductExecutableHash = state.productBinaryHash;
  state.owner.cachedProductExecutableHashAt = new Date().toISOString();
  save(state);
  ownership(state.udid);
  if (listeners(state.port, remaining(state), undefined, commandContext(state, 'preflight', 'inspect-listener', {
    endpoint: 'wda', port: state.port, frozenOwner: false,
  })).length || listeners(state.mjpeg, remaining(state), undefined, commandContext(state, 'preflight', 'inspect-listener', {
    endpoint: 'mjpeg', port: state.mjpeg, frozenOwner: false,
  })).length) {
    throw new Error('XCTEST: occupied endpoint');
  }
  const xctestrun = selectXctestrun(required('IOS_WDA_BOOTSTRAP_PATH'), state.product, state.deadline, commandContext(state, 'preflight', 'select-xctestrun'));
  const packageRoot = dirname(required('IOS_WDA_AGENT_PATH'));
  const packageVersion = JSON.parse(readFileSync(join(packageRoot, 'package.json'), 'utf8')).version;
  if (packageVersion !== version) throw new Error('XCTEST: unpinned WDA version');
  const data = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', xctestrun], remaining(state), commandContext(state, 'preflight', 'read-xctestrun')));
  data.WebDriverAgentRunner.EnvironmentVariables = {
    ...data.WebDriverAgentRunner.EnvironmentVariables,
    USE_PORT: String(state.port),
    USE_IP: '127.0.0.1',
    MJPEG_SERVER_PORT: String(state.mjpeg),
    WDA_PRODUCT_BUNDLE_IDENTIFIER: productId,
  };
  writeFileSync(xctestrun, JSON.stringify(data));
  run('plutil', ['-convert', 'xml1', xctestrun], remaining(state), commandContext(state, 'preflight', 'write-xctestrun'));
  state.owner.xctestrun = realpathSync(xctestrun);
  state.owner.xctestrunHash = binaryHash(state.owner.xctestrun);
  const selectedXctestrun = safeXctestrunEvidence(data);
  if (Buffer.byteLength(selectedXctestrun) > maxOwnerBytes) throw new Error('XCTEST: selected xctestrun evidence limit');
  writeFileSync(join(state.root, 'selected.xctestrun'), selectedXctestrun, { mode: 0o600 });
  run('xcrun', ['simctl', 'install', state.udid, state.product], remaining(state), commandContext(state, 'preflight', 'install-wda', {endpoint: 'simulator'}));
  state.owner.receipt = receipt(state, 'preflight-install');
  state.owner.installReceipt = state.owner.receipt;
  writeFileSync(join(state.root, 'installed-Info.plist'), installedInfoEvidence(state, join(state.owner.receipt, 'Info.plist'), 'preflight-install'), { mode: 0o600 });
}

function startRunner(state: SupervisorState): void {
  state.owner.command = ['xcodebuild', 'test-without-building', '-xctestrun', state.owner.xctestrun!, '-destination', `id=${state.udid}`];
  state.diagnostics.lifecycle('child-spawn-requested', {phase: 'startup', command: safeCommand(state.owner.command[0], state.owner.command.slice(1)), frozenOwner: false});
  try {
    state.child = spawn(state.owner.command[0], state.owner.command.slice(1), {stdio: ['ignore', 'pipe', 'pipe']});
  } catch (error) {
    rememberFailure(state, {error, phase: 'startup', stage: 'child-spawn', category: 'child-spawn-error'});
    throw error;
  }
  if (!state.child.pid) throw new Error('XCTEST: xcodebuild did not provide a process identifier');
  state.owner.pid = state.child.pid;
  state.diagnostics.lifecycle('child-spawned', {phase: 'startup', pid: state.child.pid, frozenOwner: false});
  state.child.stdout?.on('data', chunk => retain(state, Buffer.from(chunk), 'stdout'));
  state.child.stderr?.on('data', chunk => retain(state, Buffer.from(chunk), 'stderr'));
  let resolveChildExit: () => void = () => {};
  state.childExitObserved = new Promise(resolve => { resolveChildExit = resolve; });
  state.exited = new Promise(resolve => {
    state.child!.on('error', error => {
      state.childErrorObserved = true;
      const stopping = state.stop || existsSync(join(state.root, 'stop'));
      if (!stopping) state.stop = true;
      state.diagnostics.lifecycle('child-error', {phase: 'startup', pid: state.child?.pid, category: 'child-process-error', code: (error as NodeJS.ErrnoException).code, childExited: state.childExited});
      if (!stopping) rememberFailure(state, {message: 'XCTEST: child process error', phase: 'startup', stage: 'child-process', category: 'child-process-error'});
      save(state);
    });
    state.child!.on('exit', (code, signal) => {
      try {
        state.childExited = true;
        state.owner.ready = false;
        state.owner.exitCode = code;
        state.owner.signal = signal;
        const stopping = state.stop || existsSync(join(state.root, 'stop'));
        state.stop = true;
        if (!stopping) rememberExitFailure(state, 'child-exit');
        state.diagnostics.lifecycle('child-exit', {phase: 'startup', pid: state.child?.pid, exitCode: code, signal, frozenOwner: Boolean(state.frozenRunner)});
      } catch {
        state.stop = true;
        state.owner.exitRevocation = {...state.owner.exitRevocation, callbackFailed: true};
        rememberExitFailure(state, 'child-exit-callback');
      } finally {
        try {
          saveExitRevocation(state);
        } catch {
          // Guard even the publication failure path at the event boundary.
          state.owner.exitRevocation = {...state.owner.exitRevocation, persistencePrerequisite: 'failed'};
          process.exitCode ||= 1;
          rememberExitFailure(state, 'owner-evidence-write');
        } finally {
          try { resolveChildExit(); } catch { /* Never throw from this event. */ }
        }
      }
    });
    state.child!.on('close', (code, signal) => {
      state.childExited = true;
      state.childEnded = true;
      finishOutput(state);
      state.owner.exitCode = code;
      state.owner.signal = signal;
      state.diagnostics.lifecycle('child-close', {phase: 'startup', pid: state.child?.pid, exitCode: code, signal, frozenOwner: Boolean(state.frozenRunner)});
      save(state);
      resolve();
    });
  });
  try {
    state.owner.pidBirth = run('ps', ['-p', String(state.owner.pid), '-o', 'lstart='], Math.min(maxProcessCommandMs, remaining(state)), commandContext(state, 'startup', 'inspect-child-birth', {pid: String(state.owner.pid)}));
    state.diagnostics.lifecycle('child-birth-observed', {phase: 'startup', pid: state.owner.pid, birth: safeProcessBirth(state.owner.pidBirth, true)});
  } catch (error) {
    rememberFailure(state, {error, phase: 'startup', stage: 'child-pid-birth', category: 'child-pid-birth'});
  }
  save(state);
}

type StartupPoll = 'ready' | 'slept' | 'waiting';

async function pollStartup(state: SupervisorState): Promise<StartupPoll> {
  const initial = inspectListeners(state, state.deadline, 'initial');
  const initialWdaPids = initial.wda.status === 'evaluated' ? initial.wda.pids || [] : [];
  const initialMjpegPids = initial.mjpeg.status === 'evaluated' ? initial.mjpeg.pids || [] : [];
  if (!initialWdaPids.length && !initialMjpegPids.length) {
    if (state.frozenRunner) {
      const validation = listenerValidation(initial, initial.commands, 'not-evaluated', 'not-evaluated', {},
        'managed-listener-endpoints-empty', 'managed-listener-endpoint-disappearance', state.owner.cachedProductExecutableHash, state.owner.cachedProductExecutableHashAt);
      state.owner.listenerValidation = validation;
      rememberFailure(state, {message: 'XCTEST: managed WDA listener changed', phase: 'initial', stage: 'managed-listener-endpoints-empty', category: 'managed-listener-endpoint-disappearance', predicate: validation,
        frozenOwner: true, commandIds: snapshotCommandIds(state, initial), inspectionId: initial.inspectionId});
      save(state);
      throw new Error('XCTEST: managed WDA listener changed');
    }
    if (state.pendingRunner) {
      try {
        validatePendingSnapshot(state, initial, state.pendingRunner, state.deadline, 'initial');
      } catch (error) {
        state.initialCandidateSnapshot ||= state.pendingInitialSnapshot || initial;
        throw error;
      }
    }
    await sleep(Math.min(250, remaining(state)));
    return 'slept';
  }
  if (!initialWdaPids.length && !state.frozenRunner) {
    try {
      const candidate = validatePendingSnapshot(state, initial, state.pendingRunner, state.deadline, 'initial');
      if (!state.pendingRunner) {
        state.pendingRunner = candidate;
        state.pendingInitialSnapshot = initial;
        state.diagnostics.lifecycle('runner-candidate-pinned', {
          phase: 'initial',
          pid: candidate.pid,
          birth: safeProcessBirth(candidate.birth, true),
          executable: redactedBounded(candidate.executable, 1_024),
          frozenOwner: false,
        });
        save(state);
      } else {
        state.diagnostics.lifecycle('runner-candidate-revalidated', {
          phase: 'initial',
          pid: candidate.pid,
          birth: safeProcessBirth(candidate.birth, true),
          executable: redactedBounded(candidate.executable, 1_024),
          frozenOwner: false,
        });
        save(state);
      }
    } catch (error) {
      state.initialCandidateSnapshot ||= state.pendingInitialSnapshot || initial;
      throw error;
    }
    await sleep(Math.min(250, remaining(state)));
    return 'slept';
  }
  let candidate: RunnerIdentity;
  try {
    if (state.pendingRunner) validatePendingSnapshot(state, initial, state.pendingRunner, state.deadline, 'initial', true);
    candidate = validateListenerSnapshot(state, initial, state.frozenRunner || state.pendingRunner, state.deadline, 'initial');
  } catch (error) {
    state.initialCandidateSnapshot ||= state.pendingInitialSnapshot || initial;
    throw error;
  }
  await yieldToChildExit();
  if (state.owner.firstFailure || state.childExited || state.stop || existsSync(join(state.root, 'stop'))) throw new Error('XCTEST: startup stopped before WDA admission');
  if (!state.frozenRunner) await freezeRunner(state, initial, candidate);
  const status = await publishStatus(state);
  return await admitReadiness(state, status) ? 'ready' : 'waiting';
}

async function freezeRunner(state: SupervisorState, initial: ListenerSnapshot, candidate: RunnerIdentity): Promise<void> {
  try {
    state.owner.receipt = receipt(state, 'after-initial');
  } catch (error) {
    if (state.pendingInitialSnapshot) state.initialCandidateSnapshot ||= state.pendingInitialSnapshot;
    state.owner.receiptError = receiptErrorCategory(error);
    const message = 'XCTEST: registration receipt validation failed';
    rememberFailure(state, {message, phase: 'after-initial', stage: 'registration-receipt', category: 'registration-receipt-validation', predicate: state.owner.listenerValidation,
      frozenOwner: false, commandIds: snapshotCommandIds(state, initial), inspectionId: initial.inspectionId});
    save(state);
    throw new ReceiptValidationError(message);
  }
  const afterReceipt = inspectListeners(state, state.deadline, 'after-receipt');
  try {
    candidate = validateListenerSnapshot(state, afterReceipt, candidate, state.deadline, 'after-receipt', false);
  } catch (error) {
    if (state.pendingInitialSnapshot) state.initialCandidateSnapshot ||= state.pendingInitialSnapshot;
    throw error;
  }
  await yieldToChildExit();
  await ensureChildRunning(state, 'WDA freeze');
  if (state.owner.firstFailure || state.childExited || state.stop || existsSync(join(state.root, 'stop'))) throw new Error('XCTEST: startup stopped before WDA freeze');
  state.frozenRunner = candidate;
  state.pendingRunner = undefined;
  state.pendingInitialSnapshot = undefined;
  state.owner.runnerPid = candidate.pid;
  state.owner.runnerExecutable = candidate.executable;
  state.owner.runnerBirth = candidate.birth;
  state.diagnostics.lifecycle('runner-frozen', {
    phase: 'after-receipt',
    pid: candidate.pid,
    birth: safeProcessBirth(candidate.birth, true),
    executable: redactedBounded(candidate.executable, 1_024),
    frozenOwner: true,
  });
  save(state);
}

async function publishStatus(state: SupervisorState): Promise<StatusResponse | undefined> {
  let status: StatusResponse | undefined;
  const statusTimeout = Math.min(2_000, remaining(state));
  state.diagnostics.lifecycle('status-attempt', {
    phase: 'status',
    endpoint: 'wda',
    port: state.port,
    timeoutMs: statusTimeout,
    frozenOwner: Boolean(state.frozenRunner),
  });
  try {
    status = await readStatus(state.owner.url, statusTimeout);
    state.diagnostics.lifecycle('status-result', {
      phase: 'status',
      endpoint: 'wda',
      port: state.port,
      statusCode: status.statusCode,
      truncated: Boolean(status.truncated),
      frozenOwner: Boolean(state.frozenRunner),
    });
    const bounded = boundedStatus(status);
    state.owner.status = bounded;
    if (!save(state)) throw new Error('XCTEST: owner evidence write failed');
    try {
      writeStatusEvidence(state.root, bounded);
    } catch {
      const statusError = new Error('XCTEST: WDA status evidence write failed');
      rememberFailure(state, {error: statusError, phase: 'status', stage: 'status-evidence-write', category: 'status-evidence-write', frozenOwner: Boolean(state.frozenRunner)});
      throw statusError;
    }
  } catch (error) {
    state.diagnostics.lifecycle('status-error', {
      phase: 'status',
      endpoint: 'wda',
      port: state.port,
      category: 'status-command-error',
      frozenOwner: Boolean(state.frozenRunner),
    });
    throw error;
  } finally {
    const afterStatus = inspectListeners(state, state.deadline, 'after-status');
    validateListenerSnapshot(state, afterStatus, state.frozenRunner, state.deadline, 'after-status', true);
    await yieldToChildExit();
    await ensureChildRunning(state, 'readiness');
  }
  return status;
}

async function admitReadiness(state: SupervisorState, status: StatusResponse | undefined): Promise<boolean> {
  const statusReady = Boolean(status && status.statusCode === 200 && !status.truncated && validWdaStatus(status.body, required('IOS_PLATFORM_VERSION')));
  state.diagnostics.lifecycle('readiness-predicate', {
    phase: 'readiness',
    endpoint: 'wda',
    evaluated: Boolean(status),
    result: statusReady,
    frozenOwner: Boolean(state.frozenRunner),
  });
  if (statusReady) {
    remaining(state);
    if (state.childEnded || state.childExited) throw new Error('XCTEST: runner exited during readiness');
    if (state.stop || state.owner.firstFailure || existsSync(join(state.root, 'stop'))) throw new Error('XCTEST: startup stopped before readiness');
    state.owner.ready = true;
    if (!save(state)) throw new Error('XCTEST: owner evidence write failed');
    state.diagnostics.lifecycle('ready-admitted', {phase: 'readiness', endpoint: 'wda', port: state.port, frozenOwner: true});
    return true;
  }
  return false;
}

async function waitUntilReady(state: SupervisorState): Promise<void> {
  while (!state.stop && !state.childEnded && !state.childExited && !state.owner.firstFailure && !existsSync(join(state.root, 'stop'))) {
    remaining(state);
    try {
      const outcome = await pollStartup(state);
      if (outcome === 'ready') break;
      if (outcome === 'slept') continue;
    } catch (error) {
      if (state.owner.firstFailure || state.childExited || state.stop || existsSync(join(state.root, 'stop'))
        || error instanceof ReceiptValidationError || (error instanceof Error && error.message.startsWith('XCTEST:'))) throw error;
    }
    await sleep(Math.min(250, remaining(state)));
  }
  if (!state.owner.ready) throw new Error(`XCTEST: runner exited before readiness (${state.owner.exitCode ?? 'unknown'})`);
}

async function waitUntilStopped(state: SupervisorState): Promise<void> {
  const lifetimeDeadline = Date.now() + 60 * 60_000;
  while (!state.stop && !state.childEnded && !existsSync(join(state.root, 'stop')) && Date.now() < lifetimeDeadline) await sleep(250);
  if (state.childEnded) rememberFailure(state, {message: 'XCTEST: runner exited during scenario', phase: 'scenario', stage: 'scenario-child-exit', category: 'child-exit', frozenOwner: Boolean(state.frozenRunner)});
  if (!state.stop && !state.childEnded && !existsSync(join(state.root, 'stop'))) rememberFailure(state, {message: 'XCTEST: scenario owner lifetime limit', phase: 'scenario', stage: 'scenario-lifetime', category: 'scenario-lifetime', frozenOwner: Boolean(state.frozenRunner)});
}

async function cleanup(state: SupervisorState): Promise<void> {
  const cleanupDeadline = Date.now() + maxCleanupMs;
  // Reserve the existing 10s TERM + 2s KILL tail even if inspection hangs.
  const listenerDeadline = cleanupDeadline - 12_000;
  state.diagnostics.lifecycle('cleanup-enter', {phase: 'cleanup', frozenOwner: Boolean(state.frozenRunner), firstFailure: state.owner.firstFailure?.stage});
  state.owner.ready = false;
  state.diagnostics.lifecycle('owner-ready-cleared', {phase: 'cleanup', frozenOwner: Boolean(state.frozenRunner)});
  save(state);
  let ownedListenerObserved = false;
  let cleanupDiagnosticAttempted = false;
  try {
    ownership(state.udid);
    const pids = listenerPids(state, listenerDeadline, 'cleanup');
    if (state.initialCandidateSnapshot && state.cleanupListenerSnapshot) {
      cleanupDiagnosticAttempted = true;
      recordInitialCandidateDiagnostic(state, state.initialCandidateSnapshot, listenerDeadline, state.cleanupListenerSnapshot);
    }
    state.diagnostics.lifecycle('cleanup-listeners-observed', {
      phase: 'cleanup',
      pids: pids.slice(0, 16),
      count: pids.length,
      endpoints: state.owner.cleanupListenerObservation?.endpoints,
      commandIds: state.owner.cleanupListenerObservation?.commandIds,
      frozenOwner: Boolean(state.frozenRunner),
    });
    if (pids.length) {
      if (!ownedListeners(state, pids, listenerDeadline, 'cleanup-owner-check')) {
        rememberFailure(state, {message: 'XCTEST: refusing unknown WDA listener cleanup', phase: 'cleanup', stage: 'cleanup-listener-ownership', category: 'cleanup-unknown-listener', frozenOwner: Boolean(state.frozenRunner)});
      } else {
        ownedListenerObserved = true;
        state.diagnostics.lifecycle('owned-listener-termination-requested', {phase: 'cleanup', pid: state.owner.runnerPid, frozenOwner: true});
        run('xcrun', ['simctl', 'terminate', state.udid, runnerId], Math.min(10_000, deadlineTimeout(listenerDeadline)), commandContext(state, 'cleanup', 'terminate-owned-runner', {
          endpoint: 'simulator', pid: state.owner.runnerPid, frozenOwner: true,
        }));
        state.diagnostics.lifecycle('owned-listener-termination-finished', {phase: 'cleanup', pid: state.owner.runnerPid, frozenOwner: true});
      }
    }
  } catch (error) {
    if (!cleanupDiagnosticAttempted && state.initialCandidateSnapshot && state.cleanupListenerSnapshot) {
      recordInitialCandidateDiagnostic(state, state.initialCandidateSnapshot, listenerDeadline, state.cleanupListenerSnapshot);
    }
    rememberFailure(state, {error, phase: 'cleanup', stage: 'cleanup-listener', category: 'cleanup-listener-error', frozenOwner: Boolean(state.frozenRunner)});
  }
  try { await stopChild(state); }
  catch (error) {
    rememberFailure(state, {error, phase: 'cleanup', stage: 'child-cleanup', category: 'child-cleanup-error', frozenOwner: Boolean(state.frozenRunner)});
  }
  if (ownedListenerObserved) {
    try { await waitForOwnedListenersToExit(state, cleanupDeadline); }
    catch (error) {
      rememberFailure(state, {error, phase: 'cleanup', stage: 'owned-listener-cleanup', category: 'owned-listener-cleanup-error', frozenOwner: true});
    }
  }
  state.diagnostics.lifecycle('system-log-not-requested', {phase: 'cleanup', disposition: 'not-requested-product-first'});
  try {
    writeFileSync(join(state.root, 'ios-wda-system-log-disposition.json'), `${JSON.stringify({schema: 1, disposition: 'not-requested-product-first'})}\n`, { mode: 0o600 });
  } catch (error) {
    rememberFailure(state, {error, phase: 'cleanup', stage: 'system-log-disposition-evidence-write', category: 'system-log-disposition-write-error', frozenOwner: Boolean(state.frozenRunner)});
  }
  try {
    finishOutput(state);
    if (existsSync(state.log)) {
      const safeLog = redactedBounded(readFileSync(state.log, 'utf8'), maxStartupLogBytes);
      writeFileSync(state.log, safeLog, { mode: 0o600 });
      writeFileSync(state.publicLog, safeLog, { mode: 0o600 });
    }
  } catch (error) {
    rememberFailure(state, {error, message: 'XCTEST: startup log finalization failed', phase: 'cleanup', stage: 'startup-log-finalization', category: 'startup-log-finalization-error', frozenOwner: Boolean(state.frozenRunner)});
  }
  state.owner.endedAt = new Date().toISOString();
  state.diagnostics.lifecycle('owner-ended', {phase: 'cleanup', endedAt: state.owner.endedAt, firstFailure: state.owner.firstFailure?.stage});
  save(state);
}

async function supervise(): Promise<void> {
  const state = createSupervisorState();
  state.diagnostics.lifecycle('supervisor-start', {phase: 'startup', pid: process.pid, simulator: state.udid});
  const onSignal = (): void => {
    state.diagnostics.lifecycle('stop-requested', {phase: 'supervise'});
    state.stop = true;
  };
  process.on('SIGTERM', onSignal);
  process.on('SIGINT', onSignal);

  try {
    preflight(state);
    startRunner(state);
    await waitUntilReady(state);
    await waitUntilStopped(state);
  } catch (error) {
    const message = errorMessage(error);
    rememberFailure(state, {error, message, phase: 'supervisor', stage: message === 'XCTEST: startup deadline' ? 'startup-deadline' : 'supervisor-error', category: message === 'XCTEST: startup deadline' ? 'startup-deadline' : 'supervisor-error', frozenOwner: Boolean(state.frozenRunner)});
  } finally {
    await cleanup(state);
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
  // Without isHeadless, Appium reboots a booted simulator that has no UI client
  // window, which kills this managed WDA.
  return { 'appium:webDriverAgentUrl': owner.url, 'appium:isHeadless': true };
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
    const deadline = Date.now() + maxCleanupMs;
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

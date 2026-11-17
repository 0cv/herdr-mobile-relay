import { type SpawnSyncReturns } from 'node:child_process';
import { redactText, sanitizeValue } from './diagnostics';
import { type XCTestOutputSummary } from './xctest-output';

export const runnerId = 'com.facebook.WebDriverAgentRunner.xctrunner';
export const productId = 'com.facebook.WebDriverAgentRunner';
export const version = '16.12.8';
export const maxOwnerBytes = 1_048_576;
const maxDiagnosticCommands = 128;
const maxDiagnosticInspections = 8;
const maxDiagnosticLifecycle = 128;
const maxDiagnosticOutputBytes = 4_096;
const maxDiagnosticSerializedBytes = 384 * 1024;
const maxOwnerStatusBytes = 16 * 1024;
const maxOwnerCausalArgumentBytes = 128;
const maxOwnerCausalOutputBytes = 512;
const maxOwnerListenerCommandBytes = 4 * 1024;
export const diagnosticSource = 'tests/mobile/support/ios-xctest.ts';
const privateOwnerKeyPattern = /(?:password|passwd|secret|token|credential|private|api[_-]?key)/iu;

function monotonicMilliseconds(): number {
  return Number(process.hrtime.bigint()) / 1_000_000;
}

function redactDiagnosticText(value: string): string {
  return redactText(value);
}

export function redactedBounded(value: string | Buffer, maximum: number): string {
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

export function safeCommand(file: string, args: string[]): string[] {
  return [file, ...args].slice(0, 32).map(argument => redactedBounded(String(argument), 512));
}

export interface DiagnosticContext {
  recorder?: WdaDiagnosticsRecorder;
  phase: string;
  source: string;
  operation: string;
  endpoint?: string;
  port?: number;
  pid?: string;
  frozenOwner?: boolean;
}

export interface CommandDiagnostic {
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

export class WdaDiagnosticsRecorder {
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

export interface ProcessEvidence {
  pid: string;
  executable: string;
  command: string;
  birth: string;
}

export interface PartialProcessEvidence {
  pid: string;
  executable?: string;
  birth?: string;
}

export interface ReceiptSnapshot {
  path: string;
  applicationPathMatches: boolean;
  bundleIdentifierMatches?: boolean;
  installedExecutableHash?: string;
  productExecutableHash?: string;
  executableHashMatches?: boolean;
  failureStage?: 'bundle-id' | 'installed-hash' | 'product-hash';
  failureCategory?: 'command-error' | 'timeout';
}

export function safeProcessBirth(value: string, sanitized: boolean): string {
  if (!sanitized) return value;
  const trimmed = value.trim();
  return /^(?:[A-Za-z]{3} [A-Za-z]{3} {1,2}\d{1,2} \d{2}:\d{2}:\d{2} \d{4}|birth-[A-Za-z0-9_-]+|\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)$/u.test(trimmed)
    ? trimmed
    : '[unrecognized process birth]';
}

export interface StatusResponse {
  statusCode: number;
  body: unknown;
  bytes?: number;
  truncated?: boolean;
}

export type RunnerIdentity = Pick<ProcessEvidence, 'pid' | 'executable' | 'birth'>;
export type ListenerEvidence = Partial<ProcessEvidence> & Pick<ProcessEvidence, 'pid' | 'executable'>;

export interface ListenerCommandStatus {
  port: number;
  status: number | null;
  stderr?: 'present';
}

export interface CleanupListenerSnapshot {
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

export interface ListenerEndpointSnapshot {
  status: ListenerEndpointStatus;
  pids?: string[];
  errorCategory?: string;
}

export interface ListenerSnapshot {
  wda: ListenerEndpointSnapshot;
  mjpeg: ListenerEndpointSnapshot;
  evidence: ListenerEvidence[];
  commands: ListenerCommandStatus[];
  inspectionId?: number;
  commandStart?: number;
  commandIds?: number[];
}

export type RunnerPathShape = Partial<Record<'applicationNameMatches' | 'containerUuid' | 'simulatorPath' | 'executableName', boolean>>;

export interface RunnerMatchEvidence {
  pathShape?: RunnerPathShape;
  bundleId?: { status: 'not-evaluated' | 'evaluated' | 'error'; matches?: boolean };
  executableHash?: { status: 'not-evaluated' | 'evaluated' | 'error'; expected?: string; actual?: string; matches?: boolean };
  failureStage?: string;
  errorCategory?: string;
}

export type DiagnosticCheckStatus = 'evaluated' | 'rejected' | 'not-evaluated' | 'budget-exhausted';

export interface DiagnosticCheck {
  status: DiagnosticCheckStatus;
  matches?: boolean;
  category?: string;
}

export interface DiagnosticHashCheck extends DiagnosticCheck {
  expected?: string;
  actual?: string;
}

type DiagnosticPathShape = {
  [key in 'applicationNameMatches' | 'containerUuid' | 'simulatorPath' | 'executableName']: DiagnosticCheck;
};

export interface InitialCandidateDiagnostic {
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

export interface ListenerValidation {
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

export interface Owner {
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
export type BoundedStatus = StatusResponse & { readonly [boundedStatusMarker]: true };

export function boundedStatus(status: StatusResponse): BoundedStatus {
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

export function safeXctestrunEvidence(data: unknown): string {
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

export function ownerContent(owner: Owner, sanitized: boolean): string {
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

const diagnosticPids = (values: string[]): string[] => values.slice(0, 16);

export function endpointDiagnostic(endpoint: ListenerEndpointSnapshot): ListenerEndpointDiagnostic {
  if (endpoint.status === 'evaluated') {
    const pids = endpoint.pids || [];
    return {status: 'evaluated', count: pids.length, pids: diagnosticPids(pids)};
  }
  return endpoint.status === 'error'
    ? {status: 'error', errorCategory: endpoint.errorCategory || 'command-error'}
    : {status: 'not-evaluated'};
}

export function listenerValidation(
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

export function diagnosticCheck(value: boolean | undefined, budgetExhausted = false, category?: string): DiagnosticCheck {
  if (value === undefined) return {status: budgetExhausted ? 'budget-exhausted' : 'not-evaluated'};
  return value
    ? {status: 'evaluated', matches: true}
    : {status: 'rejected', matches: false, ...(category ? {category} : {})};
}

export function diagnosticHashCheck(
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

export function diagnosticPathShape(evaluation: RunnerMatchEvidence, budgetExhausted = false): DiagnosticPathShape {
  const path = evaluation.pathShape;
  return {
    applicationNameMatches: diagnosticCheck(path?.applicationNameMatches, budgetExhausted, 'runner-path-mismatch'),
    containerUuid: diagnosticCheck(path?.containerUuid, budgetExhausted, 'runner-path-mismatch'),
    simulatorPath: diagnosticCheck(path?.simulatorPath, budgetExhausted, 'runner-path-mismatch'),
    executableName: diagnosticCheck(path?.executableName, budgetExhausted, 'runner-path-mismatch'),
  };
}

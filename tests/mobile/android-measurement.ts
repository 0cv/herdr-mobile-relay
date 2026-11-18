import { mkdir, rename, rm, writeFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { validateMeasurementIdentity, type MeasurementIdentity } from './support/mobile-result';
import { basename, dirname, join, resolve } from 'node:path';
import { constants as osConstants } from 'node:os';
import { fileURLToPath } from 'node:url';
import type { Writable } from 'node:stream';
import { ANDROID_GMS_OBSERVATION_FILE, ANDROID_LOG_LIMIT, androidMeasurementMarker, parseAndroidProcesses, assessAndroidEnvironment, readEnvironmentInputs, type AndroidEnvironmentCheck, type AndroidPlannedTermination, type AndroidGmsComponentState } from './android-environment';
import { CommandError, command, type CommandResult } from './support/process';
import { sampleHostEmulator } from './support/android-emulator-lifetime';
import { BoundedCommandError, boundedCommand, type BoundedCommandResult } from './support/bounded-process';
import { spawnOwnedProcess, type OwnedProcess, type RetirementEvidence } from './support/owned-process';
import { PhaseBudget } from './support/budget';
import { redactText, sanitizeValue, writeSanitizedJson } from './support/diagnostics';
import { requireOwnedDevice } from './support/device';
import { isAndroidPackageProcess, isAndroidTerminationPackage } from './android-events';
import type { AppiumClient } from './support/webdriver';
import { ANDROID_TRANSPORT_TRIGGER_DEADLINE_MS, ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS, AndroidTransportObservation,
  AndroidTransportTriggerDetector, androidGuestEpochNanoseconds, transportAdbObservation,
  type AndroidTransportCandidate, type AndroidTransportTrigger } from './support/android-transport';

export interface AndroidMeasurementOutcome {
  assessment?: AndroidEnvironmentCheck;
  assessmentSha256?: string;
  errors: unknown[];
}

export interface AndroidMeasurementIO {
  command: typeof command;
  boundedCommand: typeof boundedCommand;
  writeJson: typeof writeSanitizedJson;
  stopProcess: (collector: OwnedProcess) => Promise<RetirementEvidence>;
  spawnCollector: () => OwnedProcess;
  collectorObserverStdout?: Writable;
  collectorObserverStderr?: Writable;
  collectorObserverSupervisor?: Writable;
  monotonicNow: () => number;
  plannedTerminationExitMs: number;
  transportAdbObservation: typeof transportAdbObservation;
}

export type AndroidGmsObservationStage = 'after-device-start' | 'before-measurement-begin' | 'after-setup-url'
  | 'after-browser-install' | 'after-initial-installed-app-launch';

const ANDROID_GMS_OBSERVATION_STAGES: AndroidGmsObservationStage[] = [
  'after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch',
];
const ANDROID_GMS_PACKAGE = 'com.google.android.gms';
const ANDROID_GMS_COMMAND_TIMEOUT_MS = 5_000;
const ANDROID_GMS_DUMP_LIMIT = 2_000_000;
const ANDROID_GMS_COMMAND_OUTPUT_LIMIT = ANDROID_GMS_DUMP_LIMIT * 2;
const ANDROID_GMS_OWNERSHIP_TIMEOUT_MS = 2_000;
const ANDROID_GMS_PARSE_RESERVE_MS = 1_000;
const ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS = 40;
const ANDROID_GMS_PERSISTENCE_TIMEOUT_MS = 5_000;
const ANDROID_MEASUREMENT_JSON_WRITE_TIMEOUT_MS = 1_000;
const ANDROID_GMS_STAGE_TIMEOUT_MS = ANDROID_GMS_OWNERSHIP_TIMEOUT_MS + ANDROID_GMS_COMMAND_TIMEOUT_MS + ANDROID_GMS_PARSE_RESERVE_MS;
const ANDROID_GMS_TOTAL_BYTES_LIMIT = ANDROID_GMS_DUMP_LIMIT * ANDROID_GMS_OBSERVATION_STAGES.length;
const ANDROID_GMS_TOTAL_TIME_LIMIT_MS = ANDROID_GMS_STAGE_TIMEOUT_MS * ANDROID_GMS_OBSERVATION_STAGES.length;
// A slow write may use spare time, but cannot expand the measurement's shared persistence pool.
const ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS = 6_000;
const ANDROID_GMS_HASH_ENCODING = 'sha256-hex-dot-separated-v1' as const;
const ANDROID_GMS_OBSERVATION_FILE_LIMIT = 8_000_000;
const ANDROID_MEASUREMENT_LOG_FILE = 'android-qualification-logcat.log';
const ANDROID_MEASUREMENT_FILE_WRITE_TIMEOUT_MS = 5_000;
const ANDROID_BEGIN_OPERATION_TIMEOUT_MS = 1_000;
const ANDROID_BEGIN_TOTAL_TIMEOUT_MS = 4_000;
const ANDROID_PLANNED_TERMINATION_EXIT_MS = 5_000;
const ANDROID_TERMINATION_COMMAND_TIMEOUT_MS = 20_000;
// The event checker rejects planned operations whose OP_BEGIN and OP_END are more than 30 seconds apart.
const ANDROID_PLANNED_TERMINATION_OPERATION_MS = 25_000;

function terminationCommandReceipt(result: CommandResult | CommandError | undefined, hostStartedAtMs: number,
  hostSettledAtMs: number): NonNullable<AndroidPlannedTermination['commandReceipt']> {
  const helperExitCode = result instanceof CommandError ? result.exitCode : result?.code;
  const signal = result?.signal;
  return {
    operation: 'am-force-stop-user-0',
    source: result instanceof CommandError ? 'command-error' : result ? 'command-result' : 'unknown-error',
    timeoutMs: ANDROID_TERMINATION_COMMAND_TIMEOUT_MS,
    hostStartedAtMs: Number.isSafeInteger(hostStartedAtMs) && hostStartedAtMs >= 0 ? hostStartedAtMs : null,
    hostSettledAtMs: Number.isSafeInteger(hostSettledAtMs) && hostSettledAtMs >= 0 ? hostSettledAtMs : null,
    durationMs: typeof result?.durationMs === 'number' && Number.isFinite(result.durationMs)
      && result.durationMs >= 0 && result.durationMs <= Number.MAX_SAFE_INTEGER ? result.durationMs : null,
    helperExitCode: typeof helperExitCode === 'number' && Number.isSafeInteger(helperExitCode) ? helperExitCode : null,
    signal: signal === undefined ? null : typeof signal === 'string' && Object.hasOwn(osConstants.signals, signal) ? signal as NodeJS.Signals : 'OTHER',
    timedOut: typeof result?.timedOut === 'boolean' ? result.timedOut : null,
  };
}

function collectorRetired(evidence: RetirementEvidence): boolean {
  return evidence.targetExitObserved && evidence.targetCloseObserved && evidence.anchorExitObserved
    && evidence.anchorCloseObserved && evidence.groupAbsent && evidence.inputClosedObserved
    && evidence.stdoutNaturalEnd && evidence.stdoutCloseObserved && evidence.stderrNaturalEnd
    && evidence.stderrCloseObserved && evidence.supervisorExitObserved === true && evidence.supervisorCloseObserved === true
    && evidence.supervisorExitCode === 0 && evidence.supervisorSignal === null;
}

async function writeEvidenceFile(directory: string, name: string, contents: string): Promise<string> {
  const bytes = Buffer.from(contents);
  const temporary = join(directory, `.${name}.${randomUUID()}.pending`);
  await mkdir(directory, { recursive: true });
  try {
    await writeFile(temporary, bytes, { flag: 'wx', mode: 0o600 });
    await rename(temporary, join(directory, name));
  } catch (error) {
    await rm(temporary, { force: true }).catch(() => undefined);
    throw error;
  }
  return createHash('sha256').update(bytes).digest('hex');
}

function retainedSha256(value: string): string {
  if (!/^[a-f0-9]{64}$/u.test(value)) throw new Error('ANDROID_ENVIRONMENT: invalid GMS diagnostic hash');
  return `sha256:${value.match(/.{1,16}/gu)!.join('.')}`;
}

function decodeRetainedSha256(value: string): string {
  const match = value.match(/^sha256:([a-f0-9]{16}(?:\.[a-f0-9]{16}){3})$/u);
  if (!match) throw new Error('ANDROID_ENVIRONMENT: invalid retained GMS hash encoding');
  return match[1].replaceAll('.', '');
}

function retainedGmsState(state: AndroidGmsComponentState): AndroidGmsStoredComponentState {
  const retainedValues = [state.packageName, state.versionName, state.versionCode, state.enabled,
    ...state.enabledComponents, ...state.disabledComponents];
  if (retainedValues.some(value => redactText(value) !== value)) {
    throw new Error('GMS component names or identity fields require secret redaction and cannot be retained canonically');
  }
  const componentStateSha256 = createHash('sha256').update(JSON.stringify({
    enabledComponents: state.enabledComponents,
    disabledComponents: state.disabledComponents,
  })).digest('hex');
  if (componentStateSha256 !== state.componentStateSha256) {
    throw new Error('GMS retained component state hash does not match its normalized arrays');
  }
  return {
    ...state,
    componentStateSha256: retainedSha256(componentStateSha256),
    packageIdentitySha256: retainedSha256(state.packageIdentitySha256),
    dumpSha256: retainedSha256(state.dumpSha256),
    sourceHashes: {
      componentStateSha256: retainedSha256(state.componentStateSha256),
      packageIdentitySha256: retainedSha256(state.packageIdentitySha256),
      dumpSha256: retainedSha256(state.dumpSha256),
    },
    hashEncoding: ANDROID_GMS_HASH_ENCODING,
  };
}

interface AndroidGmsStageObservation {
  stage: AndroidGmsObservationStage;
  outcome: 'collected' | 'failed';
  startedAt: string;
  endedAt: string;
  durationMs: number;
  timeoutMs?: number;
  ownershipDurationMs?: number;
  ownershipChildExited?: boolean;
  ownershipProcessesExited?: boolean;
  ownershipStdioClosed?: boolean;
  parseDurationMs?: number;
  parseTimeoutMs?: number;
  parseCleanupReservationMs?: number;
  parserStarted?: boolean;
  parserReady?: boolean;
  parserInputBytes?: number;
  parserInputSha256?: string;
  parserOutputProduced?: boolean;
  parserStartupMs?: number;
  parseTargetStarted?: boolean;
  parseTargetClosed?: boolean;
  normalizationTimedOut?: boolean;
  parseChildExited?: boolean;
  parseOwnedProcessesExited?: boolean;
  parseStdioClosed?: boolean;
  persistenceDurationMs?: number;
  stdoutBytes?: number;
  stderrBytes?: number;
  stdoutRetainedBytes?: number;
  stderrRetainedBytes?: number;
  stdoutSha256?: string;
  stderrSha256?: string;
  hashEncoding?: typeof ANDROID_GMS_HASH_ENCODING;
  stdoutTruncated?: boolean;
  stderrTruncated?: boolean;
  outputLimitExceeded?: boolean;
  childExited?: boolean;
  ownedProcessesExited?: boolean;
  stdioClosed?: boolean;
  exitCode?: number;
  timedOut?: boolean;
  signal?: string;
  state?: AndroidGmsStoredComponentState;
  error?: string;
}

type AndroidGmsStoredComponentState = Omit<AndroidGmsComponentState, 'componentStateSha256' | 'packageIdentitySha256' | 'dumpSha256'> & {
  componentStateSha256: string;
  packageIdentitySha256: string;
  dumpSha256: string;
  sourceHashes: { componentStateSha256: string; packageIdentitySha256: string; dumpSha256: string };
  hashEncoding: typeof ANDROID_GMS_HASH_ENCODING;
};

interface AndroidGmsObservations {
  schema: 1;
  packageName: typeof ANDROID_GMS_PACKAGE;
  limits: { commandTimeoutMs: number; commandStreamBytes: number; commandOutputBytes: number; totalCommandOutputBytes: number;
    ownershipTimeoutMs: number; parseInputBytes: number; parseReserveMs: number; persistenceOperationTimeoutMs: number;
    totalCollectionMs: number; totalPersistenceLimitMs: number };
  totalCommandOutputBytes: number;
  totalRetainedCommandOutputBytes: number;
  totalCollectionMs: number;
  priorPersistenceAttemptMs: number;
  persistenceRetirement: 'retired' | 'pending-temporary-write';
  temporaryEvidenceCleanup: 'not-needed' | 'removed-after-failure' | 'pending';
  persistenceAccounting: 'priorPersistenceAttemptMs includes prior bounded publication attempts; the current attempt is excluded and bounded by persistenceOperationTimeoutMs';
  stages: AndroidGmsStageObservation[];
  errors: string[];
  finalized?: boolean;
  terminalPublication?: 'awaiting-collector-confirmation';
}

interface CollectorState {
  process?: OwnedProcess;
  chunks: Buffer[];
  bytes: number;
  failure?: Error;
  closed: boolean;
  dataFrozen: boolean;
  stdoutHandler?: (chunk: Buffer) => void;
  stderrHandler?: () => void;
  summaryWritten: boolean;
  summaryFailed: boolean;
  persistedBytes: number;
}

interface GmsState {
  stageIndex: number;
  sequenceFailed: boolean;
  observations: AndroidGmsObservations;
  stageInFlight: boolean;
  stagePromise?: Promise<void>;
  stageAbort?: AbortController;
  finalizationStarted: boolean;
  finalizationComplete: boolean;
  finalizationPromise?: Promise<void>;
  persistenceFailed: boolean;
  publicationConfirmedSha256?: string;
}

interface TransportState {
  failure?: string;
  trigger: AndroidTransportTriggerDetector;
  abort?: AbortController;
  task?: Promise<void>;
  candidatesQueued: number;
}

interface LifecycleState {
  active: boolean;
  started: boolean;
  measurementBegun: boolean;
  beginFailed: boolean;
  beginAbort?: AbortController;
  beginPromise?: Promise<void>;
  finished: boolean;
  finishPromise?: Promise<AndroidMeasurementOutcome>;
  terminalCompletion?: Promise<AndroidMeasurementOutcome>;
  resolveTerminalCompletion?: (outcome: AndroidMeasurementOutcome) => void;
  rejectTerminalCompletion?: (reason: unknown) => void;
}

function initialGmsObservations(): AndroidGmsObservations {
  return {
    schema: 1,
    packageName: ANDROID_GMS_PACKAGE,
    limits: {
      commandTimeoutMs: ANDROID_GMS_COMMAND_TIMEOUT_MS,
      commandStreamBytes: ANDROID_GMS_DUMP_LIMIT,
      commandOutputBytes: ANDROID_GMS_COMMAND_OUTPUT_LIMIT,
      totalCommandOutputBytes: ANDROID_GMS_TOTAL_BYTES_LIMIT,
      ownershipTimeoutMs: ANDROID_GMS_OWNERSHIP_TIMEOUT_MS,
      parseInputBytes: ANDROID_GMS_DUMP_LIMIT,
      parseReserveMs: ANDROID_GMS_PARSE_RESERVE_MS,
      persistenceOperationTimeoutMs: ANDROID_GMS_PERSISTENCE_TIMEOUT_MS,
      totalCollectionMs: ANDROID_GMS_TOTAL_TIME_LIMIT_MS,
      totalPersistenceLimitMs: ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS,
    },
    totalCommandOutputBytes: 0,
    totalRetainedCommandOutputBytes: 0,
    totalCollectionMs: 0,
    priorPersistenceAttemptMs: 0,
    persistenceRetirement: 'retired',
    temporaryEvidenceCleanup: 'not-needed',
    persistenceAccounting: 'priorPersistenceAttemptMs includes prior bounded publication attempts; the current attempt is excluded and bounded by persistenceOperationTimeoutMs',
    stages: [],
    errors: [],
  };
}

interface GmsStageRun {
  io: AndroidMeasurementIO;
  serial: string;
  gms: GmsState;
  entry: AndroidGmsStageObservation;
  budget: PhaseBudget;
  signal: AbortSignal;
  allowanceMs: number;
  deadline: number;
}

function gmsStageAllowanceMs(totalCollectionMs: number, budget: PhaseBudget): number {
  const availableTotal = ANDROID_GMS_TOTAL_TIME_LIMIT_MS - totalCollectionMs;
  const availableParent = budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS;
  return Math.floor(Math.min(ANDROID_GMS_STAGE_TIMEOUT_MS, availableTotal, availableParent));
}

function throwIfGmsStageInterrupted(stage: GmsStageRun): void {
  if (stage.signal.aborted || stage.gms.finalizationStarted) throw new Error('GMS stage was interrupted by finalization');
}

async function observeGmsComponents(stage: GmsStageRun): Promise<void> {
  const { io, entry, signal, budget } = stage;
  if (stage.allowanceMs <= ANDROID_GMS_PARSE_RESERVE_MS) throw new Error('GMS diagnostic collection has no bounded stage allowance');
  const remainingBytes = ANDROID_GMS_TOTAL_BYTES_LIMIT - stage.gms.observations.totalCommandOutputBytes;
  if (remainingBytes <= 0) throw new Error('GMS diagnostic collection exhausted its aggregate byte bound');
  const ownerTimeout = Math.floor(Math.min(ANDROID_GMS_OWNERSHIP_TIMEOUT_MS,
    stage.allowanceMs - ANDROID_GMS_PARSE_RESERVE_MS - 1));
  const ownershipStarted = io.monotonicNow();
  try {
    await requireOwnedDevice('android', stage.serial, signal);
  } finally {
    entry.ownershipDurationMs = Math.ceil(Math.max(0, io.monotonicNow() - ownershipStarted));
    entry.ownershipChildExited = true;
    entry.ownershipProcessesExited = true;
    entry.ownershipStdioClosed = true;
  }
  if (entry.ownershipDurationMs > ownerTimeout) throw new Error('GMS owned-device verification exceeded its monotonic time bound');
  throwIfGmsStageInterrupted(stage);

  const dump = await runGmsDumpsys(stage, remainingBytes);
  throwIfGmsStageInterrupted(stage);
  validateGmsDump(dump);
  const retained = await normalizeGmsDump(stage, dump);
  if (io.monotonicNow() > stage.deadline || budget.exhausted || entry.parseDurationMs! > ANDROID_GMS_PARSE_RESERVE_MS) {
    throw new Error('GMS diagnostic normalization exceeded its stage or parent time bound');
  }
  throwIfGmsStageInterrupted(stage);
  entry.state = retained;
  entry.outcome = 'collected';
}

async function runGmsDumpsys(stage: GmsStageRun, remainingBytes: number): Promise<BoundedCommandResult> {
  const { io, entry, signal, budget } = stage;
  const remainingCollection = Math.min(stage.deadline - io.monotonicNow(), budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS);
  const timeoutMs = Math.floor(Math.min(ANDROID_GMS_COMMAND_TIMEOUT_MS, remainingCollection - ANDROID_GMS_PARSE_RESERVE_MS));
  if (timeoutMs < 1) throw new Error('GMS diagnostic collection exhausted its command allowance');
  entry.timeoutMs = timeoutMs;
  const commandAbort = new AbortController();
  const abortCommand = () => commandAbort.abort(signal.reason || new Error('GMS stage was interrupted'));
  signal.addEventListener('abort', abortCommand, { once: true });
  let result: BoundedCommandResult;
  try {
    result = await io.boundedCommand('adb', ['-s', stage.serial, 'shell', 'dumpsys', 'package', ANDROID_GMS_PACKAGE], timeoutMs, {
      budget,
      label: `observe GMS components at ${entry.stage}`,
      maxBytes: Math.min(ANDROID_GMS_COMMAND_OUTPUT_LIMIT, remainingBytes),
      maxBytesPerStream: ANDROID_GMS_DUMP_LIMIT,
      cleanupReservationMs: 200,
      signal: commandAbort.signal,
    });
  } catch (error) {
    if (error instanceof BoundedCommandError) {
      recordGmsCommand(stage.gms.observations, entry, error.result);
      throw gmsCommandFailure(error.result);
    }
    throw error;
  } finally {
    signal.removeEventListener('abort', abortCommand);
  }
  recordGmsCommand(stage.gms.observations, entry, result);
  return result;
}

function validateGmsDump(result: BoundedCommandResult): void {
  if (result.stderrBytes > 0 || result.stderr !== '') throw new Error('GMS diagnostic package command returned stderr');
  if (!result.stdout.endsWith('\n') || result.stdoutBytes > ANDROID_GMS_DUMP_LIMIT
    || Buffer.byteLength(result.stdout) !== result.stdoutRetainedBytes) {
    throw new Error('GMS diagnostic package dump is truncated, invalid UTF-8 or exceeds its parsing limit');
  }
}

async function normalizeGmsDump(stage: GmsStageRun, dump: BoundedCommandResult): Promise<AndroidGmsStoredComponentState> {
  const { io, entry, signal, budget } = stage;
  const normalizationStarted = io.monotonicNow();
  const normalizationTimeoutMs = Math.floor(Math.min(ANDROID_GMS_PARSE_RESERVE_MS,
    stage.deadline - io.monotonicNow(), budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS));
  if (normalizationTimeoutMs < 1) throw new Error('GMS diagnostic normalization has no bounded time remaining');
  entry.parseTimeoutMs = normalizationTimeoutMs;
  entry.parseCleanupReservationMs = ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS;
  try {
    const normalized = await io.boundedCommand(process.execPath,
      ['--no-env-file', '--eval', gmsDiagnosticParserProgram()], normalizationTimeoutMs, {
        budget,
        label: `normalize GMS components at ${entry.stage}`,
        maxBytes: ANDROID_GMS_COMMAND_OUTPUT_LIMIT,
        maxBytesPerStream: ANDROID_GMS_DUMP_LIMIT,
        cleanupReservationMs: ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS,
        input: dump.stdout,
        signal,
      });
    recordGmsParserResult(entry, normalized);
    if (normalized.stderrBytes || normalized.stdoutBytes !== normalized.stdoutRetainedBytes
      || !normalized.stdout.endsWith('\n') || normalized.targetCloseObserved !== true) {
      throw new Error('GMS diagnostic normalization output was truncated, incomplete or invalid');
    }
    const parserOutput = parseGmsParserOutput(normalized.stdout, dump.stdoutRetainedBytes, dump.stdoutSha256);
    entry.parserStartupMs = parserOutput.startupMs;
    const retained = retainedGmsState(parserOutput.state);
    validateRetainedGmsState(retained, dump.stdoutSha256);
    return retained;
  } catch (error) {
    if (error instanceof BoundedCommandError) {
      recordGmsParserResult(entry, error.result);
      const detail = redactText(error.result.stderr).trim().slice(0, 300);
      throw new Error(error.result.timedOut ? 'GMS diagnostic normalization timed out'
        : `GMS diagnostic normalization failed${detail ? `: ${detail}` : ' or exceeded its output bound'}`, { cause: error });
    }
    throw error;
  } finally {
    entry.parseDurationMs = Math.ceil(Math.max(0, io.monotonicNow() - normalizationStarted));
  }
}

function recordGmsParserResult(entry: AndroidGmsStageObservation, result: BoundedCommandResult): void {
  entry.normalizationTimedOut = result.timedOut;
  entry.parseTargetStarted = Boolean(result.targetPID);
  entry.parseTargetClosed = result.targetCloseObserved;
  entry.parseChildExited = result.childExited;
  entry.parseOwnedProcessesExited = result.ownedProcessesExited;
  entry.parseStdioClosed = result.stdioClosed;
  recordGmsParserProgress(entry, result.stdout);
}

function validateRetainedGmsState(retained: AndroidGmsStoredComponentState, dumpSha256: string): void {
  const componentHash = createHash('sha256').update(JSON.stringify({
    enabledComponents: retained.enabledComponents,
    disabledComponents: retained.disabledComponents,
  })).digest('hex');
  if (retained.packageName !== ANDROID_GMS_PACKAGE || !Array.isArray(retained.enabledComponents) || !Array.isArray(retained.disabledComponents)
    || retained.enabledComponents.some(value => typeof value !== 'string') || retained.disabledComponents.some(value => typeof value !== 'string')
    || retained.hashEncoding !== ANDROID_GMS_HASH_ENCODING
    || decodeRetainedSha256(retained.componentStateSha256) !== componentHash
    || decodeRetainedSha256(retained.componentStateSha256) !== decodeRetainedSha256(retained.sourceHashes.componentStateSha256)
    || decodeRetainedSha256(retained.dumpSha256) !== dumpSha256
    || decodeRetainedSha256(retained.dumpSha256) !== decodeRetainedSha256(retained.sourceHashes.dumpSha256)) {
    throw new Error('GMS normalized state or retained digest is inconsistent');
  }
}

function gmsCommandFailure(result: BoundedCommandResult): Error {
  if (result.timedOut) return new Error('GMS diagnostic command timed out');
  if (result.outputLimitExceeded) return new Error('GMS diagnostic command exceeded its combined or per-stream byte bound');
  if (result.aborted) return new Error('GMS diagnostic command was aborted');
  if (result.launchError) return new Error('GMS diagnostic command could not be launched');
  return new Error(`GMS diagnostic package command exited with status ${result.code}`);
}

function recordGmsCommand(observations: AndroidGmsObservations, entry: AndroidGmsStageObservation, result: BoundedCommandResult): void {
  entry.stdoutBytes = result.stdoutBytes;
  entry.stderrBytes = result.stderrBytes;
  entry.stdoutRetainedBytes = result.stdoutRetainedBytes;
  entry.stderrRetainedBytes = result.stderrRetainedBytes;
  entry.stdoutSha256 = retainedSha256(result.stdoutSha256);
  entry.stderrSha256 = retainedSha256(result.stderrSha256);
  entry.hashEncoding = ANDROID_GMS_HASH_ENCODING;
  entry.stdoutTruncated = result.stdoutBytes > result.stdoutRetainedBytes;
  entry.stderrTruncated = result.stderrBytes > result.stderrRetainedBytes;
  entry.outputLimitExceeded = result.outputLimitExceeded;
  entry.childExited = result.childExited;
  entry.ownedProcessesExited = result.ownedProcessesExited;
  entry.stdioClosed = result.stdioClosed;
  entry.exitCode = result.code;
  entry.timedOut = result.timedOut;
  entry.signal = result.signal;
  observations.totalCommandOutputBytes += result.stdoutBytes + result.stderrBytes;
  observations.totalRetainedCommandOutputBytes += result.stdoutRetainedBytes + result.stderrRetainedBytes;
}

export class AndroidEnvironmentMeasurement {
  readonly id = randomUUID();
  private readonly operations: AndroidPlannedTermination[] = [];
  private identity?: MeasurementIdentity;
  private readonly errors: unknown[] = [];
  private readonly collector: CollectorState = {
    chunks: [], bytes: 0, closed: false, dataFrozen: false, summaryWritten: false, summaryFailed: false, persistedBytes: 0,
  };
  private readonly gms: GmsState = {
    stageIndex: 0, sequenceFailed: false, observations: initialGmsObservations(), stageInFlight: false,
    finalizationStarted: false, finalizationComplete: false, persistenceFailed: false,
  };
  private readonly transport: TransportState = { trigger: new AndroidTransportTriggerDetector(this.id), candidatesQueued: 0 };
  private readonly lifecycle: LifecycleState = { active: false, started: false, measurementBegun: false, beginFailed: false, finished: false };

  bind(identity: MeasurementIdentity): void {
    if (this.identity || this.lifecycle.started || identity.measurementId !== this.id) throw new Error('ANDROID_ENVIRONMENT: identity binding must occur once before begin');
    validateMeasurementIdentity(identity);
    this.identity = structuredClone(identity);
  }

  plannedOperations(): AndroidPlannedTermination[] { return structuredClone(this.operations); }

  private async attempt(operation: () => Promise<unknown>): Promise<void> {
    try { await operation(); } catch (error) { if (!this.errors.includes(error)) this.errors.push(error); }
  }

  private readonly io: AndroidMeasurementIO;

  private readonly collectorHardDeadlineNs?: bigint;

  constructor(private readonly serial: string, private readonly outputDir: string, private readonly toolchains: string,
    io: Partial<AndroidMeasurementIO> = {}, lifetime?: PhaseBudget) {
    this.collectorHardDeadlineNs = lifetime
      ? process.hrtime.bigint() + BigInt(Math.max(0, Math.floor(lifetime.remainingMs))) * 1_000_000n
      : undefined;
    const defaultStopCollector = (collector: OwnedProcess) => collector.retire('measurement-finished');
    this.io = { command, boundedCommand, transportAdbObservation, writeJson: writeSanitizedJson, stopProcess: defaultStopCollector,
      spawnCollector: () => {
        if (this.collectorHardDeadlineNs === undefined) throw new Error('ANDROID_ENVIRONMENT: enclosing scenario lifetime is required for log collection');
        return spawnOwnedProcess('adb', ['-s', serial, 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1'], {
          hardDeadlineNs: this.collectorHardDeadlineNs,
          cleanupReservationMs: 2_000,
          observerStdout: io.collectorObserverStdout,
          observerStderr: io.collectorObserverStderr,
          observerSupervisor: io.collectorObserverSupervisor,
        });
      },
      monotonicNow: () => performance.now(), plannedTerminationExitMs: ANDROID_PLANNED_TERMINATION_EXIT_MS,
      ...io };
  }

  failureOutcome(): AndroidMeasurementOutcome {
    const errors = [...this.errors];
    const diagnosticFailure = this.gmsDiagnosticFailure();
    if (diagnosticFailure && !errors.some((error) => error instanceof Error && error.message === diagnosticFailure.message)) errors.push(diagnosticFailure);
    return { errors };
  }

  async observeGmsStage(stage: AndroidGmsObservationStage, budget: PhaseBudget): Promise<void> {
    if (this.gms.finalizationStarted || this.lifecycle.finished) throw new Error('ANDROID_ENVIRONMENT: GMS observations are finalized');
    const startedAt = new Date().toISOString();
    const started = this.io.monotonicNow();
    const expected = ANDROID_GMS_OBSERVATION_STAGES[this.gms.stageIndex];
    const entry: AndroidGmsStageObservation = { stage, outcome: 'failed', startedAt, endedAt: startedAt, durationMs: 0 };
    const rejectStage = (message: string) => {
      this.gms.sequenceFailed = true;
      entry.error = message;
      entry.endedAt = new Date().toISOString();
      entry.durationMs = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      if (this.gms.observations.stages.length < ANDROID_GMS_OBSERVATION_STAGES.length) this.gms.observations.stages.push(entry);
      this.recordGmsError(message);
    };
    if (this.gms.stageInFlight) {
      rejectStage('GMS stage observation calls cannot overlap');
      return;
    }
    if (this.gms.sequenceFailed || stage !== expected) {
      rejectStage(`GMS stage order mismatch: expected ${expected || 'no further stage'}, received ${stage}`);
      return;
    }
    const preMeasurementStage = this.gms.stageIndex < 2;
    if ((preMeasurementStage && this.lifecycle.started) || (!preMeasurementStage && (!this.lifecycle.measurementBegun || this.lifecycle.beginFailed))) {
      rejectStage(preMeasurementStage
        ? 'GMS pre-measurement stage occurred after begin'
        : 'GMS post-measurement stage requires a successfully begun measurement');
      return;
    }
    if (this.gms.observations.stages.length >= ANDROID_GMS_OBSERVATION_STAGES.length) {
      rejectStage('GMS diagnostic stage limit was reached');
      return;
    }

    this.gms.stageIndex++;
    this.gms.stageInFlight = true;
    this.gms.stageAbort = new AbortController();
    this.gms.observations.stages.push(entry);
    const task = this.collectGmsStage(entry, started, budget, this.gms.stageAbort.signal);
    this.gms.stagePromise = task;
    try {
      await task;
    } finally {
      if (this.gms.stagePromise === task) {
        this.gms.stagePromise = undefined;
        this.gms.stageAbort = undefined;
        this.gms.stageInFlight = false;
      }
    }
  }

  private async collectGmsStage(entry: AndroidGmsStageObservation, started: number, budget: PhaseBudget, signal: AbortSignal): Promise<void> {
    const allowanceMs = gmsStageAllowanceMs(this.gms.observations.totalCollectionMs, budget);
    const stage: GmsStageRun = { io: this.io, serial: this.serial, gms: this.gms, entry, budget, signal,
      allowanceMs, deadline: started + Math.max(0, allowanceMs) };
    try {
      await observeGmsComponents(stage);
    } catch (error) {
      if (signal.aborted || this.gms.finalizationStarted) {
        entry.outcome = 'failed';
        delete entry.state;
        entry.error = 'GMS stage was interrupted by finalization';
        this.recordGmsError(entry.error);
      } else {
        const message = error instanceof Error ? error.message : String(error);
        entry.error = redactText(message).slice(0, 500);
        this.recordGmsError(entry.error);
      }
    } finally {
      entry.durationMs = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      entry.endedAt = new Date().toISOString();
      this.gms.observations.totalCollectionMs += entry.durationMs;
      if (this.gms.observations.totalCollectionMs > ANDROID_GMS_TOTAL_TIME_LIMIT_MS) {
        entry.outcome = 'failed';
        delete entry.state;
        entry.error = 'GMS diagnostic collection exceeded its total time bound';
        this.recordGmsError(entry.error);
      }
      if (!this.gms.finalizationStarted) await this.persistGmsObservations(budget, entry, signal);
    }
  }

  private recordGmsError(message: string): void {
    if (!this.gms.observations.errors.includes(message) && this.gms.observations.errors.length < 50) {
      this.gms.observations.errors.push(message);
    }
  }

  private gmsDiagnosticFailure(): Error | undefined {
    if (!this.gms.observations.errors.length) return undefined;
    return new Error(`ANDROID_ENVIRONMENT: ${this.gms.observations.errors.length} GMS diagnostic observation error(s)`);
  }

  finalizeGmsObservations(): Promise<void> {
    const finalization = this.startGmsFinalization();
    if (this.lifecycle.finishPromise || (this.lifecycle.measurementBegun && !this.collector.closed)) {
      const terminal = this.lifecycle.finishPromise ?? this.waitForTerminalCompletion();
      return Promise.all([finalization, terminal]).then(([, outcome]) => {
        if (outcome.errors.length || outcome.assessment?.collection.status !== 'PASS') {
          throw new Error('ANDROID_ENVIRONMENT: terminal collector confirmation failed');
        }
      });
    }
    return finalization.then(() => {
      if (this.collector.summaryFailed) throw new Error('ANDROID_ENVIRONMENT: terminal collector summary publication failed');
    });
  }

  private waitForTerminalCompletion(): Promise<AndroidMeasurementOutcome> {
    if (!this.lifecycle.terminalCompletion) {
      this.lifecycle.terminalCompletion = new Promise<AndroidMeasurementOutcome>((resolve, reject) => {
        this.lifecycle.resolveTerminalCompletion = resolve;
        this.lifecycle.rejectTerminalCompletion = reject;
      });
    }
    return this.lifecycle.terminalCompletion;
  }

  private startGmsFinalization(): Promise<void> {
    if (this.gms.finalizationPromise) return this.gms.finalizationPromise;
    if (this.gms.finalizationComplete) return Promise.resolve();
    this.gms.finalizationStarted = true;
    this.gms.finalizationPromise = this.finalizeGmsObservationsOwned();
    return this.gms.finalizationPromise;
  }

  private async finalizeGmsObservationsOwned(): Promise<void> {
    this.lifecycle.beginAbort?.abort(new Error('GMS observation finalization interrupted measurement begin'));
    if (this.lifecycle.beginPromise) await this.lifecycle.beginPromise.catch(() => undefined);
    if (this.gms.stagePromise) {
      this.gms.stageAbort?.abort(new Error('GMS stage was interrupted by finalization'));
      this.gms.sequenceFailed = true;
      await this.gms.stagePromise.catch(() => undefined);
    }
    if (this.gms.stageIndex !== ANDROID_GMS_OBSERVATION_STAGES.length || this.gms.sequenceFailed || !this.lifecycle.measurementBegun || this.lifecycle.beginFailed) {
      this.recordGmsError('GMS diagnostic stage sequence is incomplete or unbound to a successful measurement');
    }
    this.gms.observations.finalized = true;
    this.gms.observations.terminalPublication = 'awaiting-collector-confirmation';
    await this.persistGmsObservations();
    this.gms.finalizationComplete = true;
    const failure = this.gmsDiagnosticFailure();
    if (failure && !this.errors.some((error) => error instanceof Error && error.message === failure.message)) this.errors.push(failure);
    if (this.collector.closed) await this.writeCollectorSummary();
  }

  private async persistGmsObservations(budget?: PhaseBudget, entry?: AndroidGmsStageObservation, stageSignal?: AbortSignal): Promise<void> {
    if (this.gms.persistenceFailed) return;
    const started = this.io.monotonicNow();
    const remainingPersistence = ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS - this.gms.observations.priorPersistenceAttemptMs;
    const timeoutMs = Math.floor(Math.min(ANDROID_GMS_PERSISTENCE_TIMEOUT_MS, remainingPersistence, budget?.remainingMs ?? remainingPersistence));
    let succeeded = false;
    try {
      if (timeoutMs < 1 || this.gms.observations.stages.length > ANDROID_GMS_OBSERVATION_STAGES.length) {
        throw new Error('GMS observation evidence exhausted its persistence bound');
      }
      this.gms.observations.temporaryEvidenceCleanup = 'not-needed';
      if (this.gms.observations.finalized) this.gms.publicationConfirmedSha256 = undefined;
      const snapshot = structuredClone(this.gms.observations);
      const contents = `${JSON.stringify(sanitizeValue(snapshot), null, 2)}\n`;
      if (Buffer.byteLength(contents) > ANDROID_GMS_OBSERVATION_FILE_LIMIT) throw new Error('GMS observation evidence exceeds its file bound');
      if (stageSignal?.aborted) throw stageSignal.reason;
      const sha256 = await writeEvidenceFile(this.outputDir, ANDROID_GMS_OBSERVATION_FILE, contents).catch((error: unknown) => {
        this.gms.observations.temporaryEvidenceCleanup = 'removed-after-failure';
        throw error;
      });
      if (stageSignal?.aborted || budget?.exhausted || this.io.monotonicNow() - started > timeoutMs) {
        throw new Error('GMS observation evidence failed its bounded write');
      }
      succeeded = true;
      this.gms.observations.persistenceRetirement = 'retired';
      this.gms.observations.temporaryEvidenceCleanup = 'not-needed';
      if (snapshot.finalized) this.gms.publicationConfirmedSha256 = sha256;
    } catch (error) {
      if (stageSignal?.aborted) {
        this.recordGmsError('GMS persistence was interrupted by finalization');
      } else {
        const message = error instanceof Error ? error.message : 'GMS observation evidence could not be persisted';
        this.failGmsPersistence(entry, redactText(message).slice(0, 500));
      }
    } finally {
      const duration = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      this.gms.observations.priorPersistenceAttemptMs += duration;
      if (entry) entry.persistenceDurationMs = duration;
      if (this.gms.observations.priorPersistenceAttemptMs > ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS) {
        this.failGmsPersistence(entry, 'GMS observation evidence exceeded its total persistence time bound');
      }
    }
    if (!succeeded && !stageSignal?.aborted && !this.gms.persistenceFailed) {
      this.failGmsPersistence(entry, 'GMS observation evidence could not be persisted');
    }
  }

  private failGmsPersistence(entry: AndroidGmsStageObservation | undefined, message: string): void {
    this.gms.persistenceFailed = true;
    this.recordGmsError(message);
    if (entry) {
      entry.outcome = 'failed';
      entry.error = message;
      delete entry.state;
    }
    const error = new Error(`ANDROID_ENVIRONMENT: ${message}`);
    if (!this.errors.some(existing => existing instanceof Error && existing.message === error.message)) this.errors.push(error);
  }

  private path(name: string): string {
    return join(this.outputDir, `android-environment-${name}.json`);
  }

  private async persistOwnedContents(target: string, contents: string, signal?: AbortSignal): Promise<string> {
    const isLog = basename(target) === ANDROID_MEASUREMENT_LOG_FILE;
    if (Buffer.byteLength(contents) > (isLog ? ANDROID_LOG_LIMIT : ANDROID_GMS_OBSERVATION_FILE_LIMIT)) {
      throw new Error(`ANDROID_ENVIRONMENT: ${basename(target)} exceeds its byte bound`);
    }
    if (signal?.aborted) throw signal.reason || new Error('ANDROID_ENVIRONMENT: measurement file write was aborted');
    const timeoutMs = isLog ? ANDROID_MEASUREMENT_FILE_WRITE_TIMEOUT_MS : ANDROID_MEASUREMENT_JSON_WRITE_TIMEOUT_MS;
    const started = this.io.monotonicNow();
    const sha256 = await writeEvidenceFile(dirname(target), basename(target), contents);
    if (this.io.monotonicNow() - started > timeoutMs) {
      throw new Error(`ANDROID_ENVIRONMENT: writing ${basename(target)} exceeded its time bound`);
    }
    return sha256;
  }

  private async persistOwnedJson(name: 'collector' | 'completion' | 'identity' | 'check', value: unknown, signal?: AbortSignal): Promise<string> {
    const target = this.path(name);
    const content = `${JSON.stringify(sanitizeValue(value), null, 2)}\n`;
    return this.persistOwnedContents(target, content, signal);
  }

  private async snapshot(boundary: 'start' | 'end', signal?: AbortSignal): Promise<void> {
    const name = boundary === 'start' ? 'before' : 'after';
    await this.io.boundedCommand(process.execPath, [fileURLToPath(new URL('./android-environment.ts', import.meta.url)), 'snapshot',
      '--serial', this.serial, '--toolchains', this.toolchains, '--output', this.path(name),
      '--boundary', boundary, '--measurement', this.id], 180_000, {
        label: `capture Android ${boundary} measurement snapshot`,
        maxBytes: 8 * 1024 * 1024,
        maxBytesPerStream: 4 * 1024 * 1024,
        cleanupReservationMs: 5_000,
        signal,
      });
  }

  async begin(): Promise<void> {
    if (this.lifecycle.started || this.lifecycle.finished || this.gms.finalizationStarted) throw new Error('ANDROID_ENVIRONMENT: measurement cannot begin after start or finalization; rebaseline is forbidden');
    this.lifecycle.started = true;
    const controller = new AbortController();
    this.lifecycle.beginAbort = controller;
    const operation = this.beginMeasurement(controller.signal);
    this.lifecycle.beginPromise = operation;
    try {
      await operation;
    } finally {
      if (this.lifecycle.beginPromise === operation) this.lifecycle.beginPromise = undefined;
      if (this.lifecycle.beginAbort === controller) this.lifecycle.beginAbort = undefined;
    }
  }

  private ensureBeginning(signal: AbortSignal): void {
    if (signal.aborted || this.lifecycle.finished || this.gms.finalizationStarted) {
      throw signal.reason || new Error('ANDROID_ENVIRONMENT: begin was interrupted by finalization');
    }
  }

  private async beginStep(deadline: number, operation: () => Promise<unknown>): Promise<void> {
    const timeoutMs = Math.floor(Math.min(ANDROID_BEGIN_OPERATION_TIMEOUT_MS, deadline - this.io.monotonicNow()));
    if (timeoutMs < 1) throw new Error('ANDROID_ENVIRONMENT: begin exhausted its bounded setup allowance');
    const started = this.io.monotonicNow();
    await operation();
    if (this.io.monotonicNow() - started > timeoutMs) {
      throw new Error('ANDROID_ENVIRONMENT: begin setup exceeded its time bound');
    }
  }

  private async beginMeasurement(signal: AbortSignal): Promise<void> {
    const deadline = this.io.monotonicNow() + ANDROID_BEGIN_TOTAL_TIMEOUT_MS;
    try {
      if (this.gms.stageIndex < 2 || this.gms.stageInFlight || this.gms.sequenceFailed) {
        const error = new Error('ANDROID_ENVIRONMENT: begin requires both ordered pre-measurement GMS stage attempts');
        this.recordGmsError(error.message);
        throw error;
      }
      await this.beginStep(deadline, () => requireOwnedDevice('android', this.serial, signal));
      this.ensureBeginning(signal);
      await this.beginStep(deadline, () => mkdir(this.outputDir, { recursive: true }));
      this.ensureBeginning(signal);
      await this.beginStep(deadline, () => writeFile(this.path('session'), `${JSON.stringify({ id: this.id })}\n`, { flag: 'wx', mode: 0o600 }));
      this.ensureBeginning(signal);
      await this.beginStep(deadline, () => this.io.writeJson(this.path('operations'), this.operations));
      this.ensureBeginning(signal);
      this.collector.process = this.io.spawnCollector();
      this.collector.process.failure.then(error => { if (!this.collector.dataFrozen) this.collector.failure ||= error; });
      this.collector.process.targetExit.then(exit => {
        if (this.lifecycle.active) this.collector.failure ||= new Error(exit?.launchError || 'ANDROID_ENVIRONMENT: log collector exited inside measurement');
      });
      this.collector.stderrHandler = () => {
        if (!this.collector.dataFrozen) this.collector.failure ||= new Error('ANDROID_ENVIRONMENT: log collector reported stderr');
      };
      this.collector.process.stderr.on('data', this.collector.stderrHandler);
      this.collector.stdoutHandler = (chunk: Buffer) => {
        if (this.collector.dataFrozen) return;
        this.collector.bytes += chunk.length;
        if (this.collector.bytes > ANDROID_LOG_LIMIT) {
          this.collector.failure ||= new Error('ANDROID_ENVIRONMENT: measured log exceeds bound');
          this.collector.process?.stop('collector-output-limit');
          return;
        }
        const candidates = this.transport.trigger.observe(chunk, this.io.monotonicNow());
        if (this.transport.trigger.failure) this.failTransportObservation(this.transport.trigger.failure);
        for (const candidate of candidates) this.queueTransportCandidate(candidate);
        this.collector.chunks.push(chunk);
      };
      this.collector.process.stdout.on('data', this.collector.stdoutHandler);
      await this.collector.process.ready;
      this.ensureBeginning(signal);
      this.collector.process.start();
      this.lifecycle.active = true;
      await this.snapshot('start', signal);
      this.ensureBeginning(signal);
      const markerDeadline = Date.now() + 5_000;
      await this.waitForMarker('START', signal, markerDeadline);
      this.ensureBeginning(signal);
      this.lifecycle.measurementBegun = true;
    } catch (error) {
      this.lifecycle.beginFailed = true;
      this.lifecycle.measurementBegun = false;
      this.errors.push(error);
      this.transport.abort?.abort(error);
      await this.closeCollector(signal);
      const completion = { passed: false, errors: this.errors.map(error => error instanceof Error ? error.message : String(error)) };
      await this.attempt(() => this.persistOwnedJson('completion', completion, signal));
      throw error;
    }
  }

  private async waitForMarker(marker: string, signal?: AbortSignal, deadline = Date.now() + 5_000): Promise<void> {
    while (Date.now() < deadline) {
      if (signal?.aborted) throw signal.reason || new Error('ANDROID_ENVIRONMENT: marker wait was aborted');
      if (this.collector.failure) throw this.collector.failure;
      const output = Buffer.concat(this.collector.chunks).toString('utf8');
      const complete = output.slice(0, output.lastIndexOf('\n') + 1);
      const expected = new RegExp(`^\\s*\\d{10}\\.\\d{3,9}\\s+\\d+\\s+\\d+\\s+I\\s+HerdrMeasure: ${this.id} ${marker}\\r?$`, 'mu');
      if (expected.test(complete)) return;
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          signal?.removeEventListener('abort', abort);
          resolve();
        }, 25);
        const abort = () => {
          clearTimeout(timer);
          reject(signal?.reason || new Error('ANDROID_ENVIRONMENT: marker wait was aborted'));
        };
        signal?.addEventListener('abort', abort, { once: true });
      });
    }
    throw new Error(`ANDROID_ENVIRONMENT: collector did not read ${marker} marker`);
  }

  async terminate(packageName: string, pid: string): Promise<void> {
    if (!this.lifecycle.active || this.collector.failure) throw this.collector.failure || new Error('ANDROID_ENVIRONMENT: termination is outside measurement');
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
    await this.io.writeJson(this.path('operations'), this.operations);
    const operationStarted = this.io.monotonicNow();
    await androidMeasurementMarker(this.serial, `${this.id} OP_BEGIN ${operation.id} ${packageName} ${pid}`);
    const hostStartedAtMs = Date.now();
    const observeHost = async (hostSettledAtMs: number) => {
      const file = process.env.ANDROID_EMULATOR_LIFETIME_FILE;
      if (!file) return;
      const sampleStartedAtMs = Date.now();
      try {
        operation.hostEmulatorAtSettlement = await sampleHostEmulator(file, process.env.ANDROID_EMULATOR_PID,
          hostSettledAtMs, Math.min(250, operationStarted + ANDROID_PLANNED_TERMINATION_OPERATION_MS - this.io.monotonicNow()));
      } catch {
        operation.hostEmulatorAtSettlement = { hostSettledAtMs, sampleStartedAtMs, sampleEndedAtMs: Date.now(),
          state: 'unknown', reason: 'io-error', nativeExitCoverage: 'unknown' };
      }
    };
    let stopped: CommandResult;
    try {
      stopped = await this.io.command('adb', ['-s', this.serial, ...operation.command], ANDROID_TERMINATION_COMMAND_TIMEOUT_MS);
    } catch (error) {
      const hostSettledAtMs = Date.now();
      operation.commandReceipt = terminationCommandReceipt(error instanceof CommandError ? error : undefined, hostStartedAtMs, hostSettledAtMs);
      await observeHost(hostSettledAtMs);
      await this.attempt(() => this.io.writeJson(this.path('operations'), this.operations));
      throw error;
    }
    const hostSettledAtMs = Date.now();
    const stoppedAt = this.io.monotonicNow();
    operation.commandReceipt = terminationCommandReceipt(stopped, hostStartedAtMs, hostSettledAtMs);
    await observeHost(hostSettledAtMs);
    await this.io.writeJson(this.path('operations'), this.operations);
    if (stopped.stdout.trim() || stopped.stderr.trim()) throw new Error('ANDROID_ENVIRONMENT: force-stop returned unexpected output');
    // force-stop returns once it has signalled the processes; a large process can take a moment to exit.
    const operationDeadline = operationStarted + ANDROID_PLANNED_TERMINATION_OPERATION_MS;
    const exitDeadline = Math.min(stoppedAt + this.io.plannedTerminationExitMs, operationDeadline);
    const inventoryDeadline = Math.min(stoppedAt + ANDROID_PLANNED_TERMINATION_EXIT_MS, operationDeadline);
    for (;;) {
      if (!this.lifecycle.active || this.collector.failure) throw this.collector.failure || new Error('ANDROID_ENVIRONMENT: termination is outside measurement');
      const inventoryTimeout = Math.min(5_000, Math.max(1_000, Math.ceil(inventoryDeadline - this.io.monotonicNow())));
      const remaining = await command('adb', ['-s', this.serial, 'shell', 'ps', '-A', '-o', 'PID,NAME'], inventoryTimeout);
      if (remaining.stderr.trim()) throw new Error('ANDROID_ENVIRONMENT: planned termination exit inventory reported stderr');
      const processes = parseAndroidProcesses(remaining.stdout);
      if (!Object.keys(affected).some((targetPid) => processes[targetPid])
        && !Object.values(processes).some((name) => isAndroidPackageProcess(name, packageName))) break;
      if (this.io.monotonicNow() >= exitDeadline) throw new Error('ANDROID_ENVIRONMENT: planned termination did not remove the observed package process set');
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    await androidMeasurementMarker(this.serial, `${this.id} OP_END ${operation.id} ${packageName} ${pid}`);
    operation.succeeded = true;
    await this.io.writeJson(this.path('operations'), this.operations);
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
        if (!this.lifecycle.active) throw new Error('measurement inactive');
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

  private async closeCollector(signal?: AbortSignal): Promise<void> {
    if (this.collector.closed) return;
    this.collector.closed = true;
    this.lifecycle.active = false;
    if (this.collector.process) {
      let retired = false;
      try {
        const evidence = await this.io.stopProcess(this.collector.process);
        if (!collectorRetired(evidence)) throw new Error('ANDROID_ENVIRONMENT: log collector target, streams, group or supervisors did not retire naturally');
        retired = true;
      } catch (error) {
        this.collector.failure ||= error instanceof Error ? error : new Error('ANDROID_ENVIRONMENT: log collector process tree did not retire');
        this.errors.push(error);
      }
      this.collector.dataFrozen = true;
      if (retired) {
        if (this.collector.stdoutHandler) this.collector.process.stdout.removeListener('data', this.collector.stdoutHandler);
        if (this.collector.stderrHandler) this.collector.process.stderr.removeListener('data', this.collector.stderrHandler);
        this.collector.stdoutHandler = undefined;
        this.collector.stderrHandler = undefined;
      }
    }
    this.transport.trigger.finish();
    if (this.transport.trigger.failure) this.failTransportObservation(this.transport.trigger.failure);
    let raw = Buffer.alloc(0);
    await this.attempt(async () => {
      raw = Buffer.concat(this.collector.chunks);
      const text = raw.toString('utf8');
      if (!Buffer.from(text, 'utf8').equals(raw)) throw new Error('ANDROID_ENVIRONMENT: invalid collector UTF-8');
      const persisted = redactText(text);
      await this.persistOwnedContents(join(this.outputDir, ANDROID_MEASUREMENT_LOG_FILE), persisted, signal);
      this.collector.persistedBytes = Buffer.byteLength(persisted);
    });
    if (signal?.aborted) this.transport.abort?.abort(signal.reason);
    if (this.transport.task) await this.attempt(() => this.transport.task!);
    this.collector.chunks = [];
    if (this.gms.finalizationComplete) await this.writeCollectorSummary();
  }

  private queueTransportCandidate(candidate: AndroidTransportCandidate): void {
    if (this.transport.failure || this.transport.trigger.isAccepted || this.transport.candidatesQueued >= 32) {
      if (this.transport.candidatesQueued >= 32) this.failTransportObservation('collector trigger queue exceeded its bound');
      return;
    }
    this.transport.candidatesQueued++;
    const controller = this.transport.abort || new AbortController();
    this.transport.abort = controller;
    const previous = this.transport.task || Promise.resolve();
    const task = previous.then(() => this.admitTransportCandidate(candidate, controller.signal))
      .finally(() => { this.transport.candidatesQueued--; });
    this.transport.task = task.catch(() => {
      this.failTransportObservation('transport observation could not be saved');
    });
  }

  private async admitTransportCandidate(candidate: AndroidTransportCandidate, signal: AbortSignal): Promise<void> {
    if (this.transport.failure || signal.aborted || this.transport.trigger.isAccepted) return;
    const candidateEpoch = androidGuestEpochNanoseconds(candidate.guestEpochSeconds);
    const anchorEpoch = androidGuestEpochNanoseconds(candidate.anchorGuestEpochSeconds);
    if (candidateEpoch === undefined || anchorEpoch === undefined) {
      this.failTransportObservation('collector trigger timestamp was malformed');
      return;
    }
    if (candidateEpoch < anchorEpoch) return;
    const guestElapsedMs = Number(candidateEpoch - anchorEpoch) / 1_000_000;
    const monotonicElapsedMs = candidate.candidateMonotonicMs - candidate.anchorMonotonicMs;
    if (!Number.isFinite(monotonicElapsedMs) || monotonicElapsedMs < 0
      || Math.abs(guestElapsedMs - monotonicElapsedMs) > ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS) {
      this.failTransportObservation('guest clock changed across the measurement anchor and trigger');
      return;
    }

    const deadline = candidate.candidateMonotonicMs + ANDROID_TRANSPORT_TRIGGER_DEADLINE_MS;
    const clockTimeoutMs = Math.floor(Math.min(1_500, deadline - this.io.monotonicNow() - 250));
    if (clockTimeoutMs < 1) {
      this.failTransportObservation('guest clock sampling missed the trigger deadline');
      return;
    }
    const sample = await this.io.transportAdbObservation(this.serial, 'clock', 5037,
      clockTimeoutMs, signal);
    const sampleEpochText = sample.stdout.match(/^(\d{10}\.\d{9})\r?\n?$/u)?.[1];
    const sampleEpoch = sampleEpochText ? androidGuestEpochNanoseconds(sampleEpochText, true) : undefined;
    if (signal.aborted || sample.unavailable || sample.timedOut || sample.truncated || sampleEpoch === undefined) {
      this.failTransportObservation('guest clock sample was unavailable or malformed');
      return;
    }
    if (sampleEpoch < anchorEpoch || sampleEpoch < candidateEpoch) {
      this.failTransportObservation('guest clock moved backwards before transport acquisition');
      return;
    }
    if (sampleEpoch - candidateEpoch > BigInt(ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS) * 1_000_000n) return;
    if (this.io.monotonicNow() >= deadline || !this.transport.trigger.accept()) {
      this.failTransportObservation('transport trigger could not be admitted within its deadline');
      return;
    }

    const trigger: AndroidTransportTrigger = {
      guestEpochSeconds: candidate.guestEpochSeconds,
      triggeredAt: candidate.triggeredAt,
      anchorGuestEpochSeconds: candidate.anchorGuestEpochSeconds,
      guestClockSample: { guestEpochSeconds: sampleEpochText!, startedAt: sample.startedAt, endedAt: sample.endedAt },
    };
    await this.persistTransportObservation(trigger, signal, deadline);
  }

  private failTransportObservation(message: string): void {
    if (this.transport.failure) return;
    this.transport.failure = message;
    this.transport.abort?.abort(new Error(message));
  }

  private async persistTransportObservation(trigger: AndroidTransportTrigger, signal: AbortSignal, deadline: number): Promise<void> {
    try {
      const timeoutMs = Math.floor(deadline - this.io.monotonicNow());
      if (timeoutMs < 1) throw new Error('transport trigger deadline expired before worker dispatch');
      const result = await this.io.boundedCommand(process.execPath,
        ['--no-env-file', fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--android-transport-observation-trigger'], timeoutMs, {
          label: 'persist trigger-time Android transport observation',
          maxBytes: 1_024,
          maxBytesPerStream: 512,
          cleanupReservationMs: 250,
          input: JSON.stringify({ serial: this.serial, outputDir: this.outputDir, ...trigger }),
          signal,
        });
      const receipt = JSON.parse(result.stdout) as { failure?: string };
      if (typeof receipt.failure === 'string') this.transport.failure = receipt.failure;
    } catch (error) {
      this.failTransportObservation('transport observation could not be saved');
      throw error;
    }
  }

  private async writeCollectorSummary(): Promise<void> {
    if (this.collector.summaryWritten) return;
    const summary = {
      bytes: this.collector.persistedBytes, acquiredBytes: this.collector.bytes, failure: this.collector.failure?.message || (this.errors.length || this.gms.observations.errors.length ? 'COLLECTION_FAILED' : undefined), transportObservationFailure: this.transport.failure,
      gmsObservations: {
        file: ANDROID_GMS_OBSERVATION_FILE,
        stages: this.gms.observations.stages.map(({ stage, outcome }) => ({ stage, outcome })),
        totalCommandOutputBytes: this.gms.observations.totalCommandOutputBytes,
        totalRetainedCommandOutputBytes: this.gms.observations.totalRetainedCommandOutputBytes,
        totalCollectionMs: this.gms.observations.totalCollectionMs,
        persistenceAttemptMsThroughFinalization: this.gms.observations.priorPersistenceAttemptMs,
        temporaryEvidenceCleanup: this.gms.observations.temporaryEvidenceCleanup,
        persistenceAccounting: 'persistenceAttemptMsThroughFinalization includes all bounded observation-publication attempts through finalization; retirement and temporary cleanup are reported separately',
        persistenceRetirement: this.gms.observations.persistenceRetirement,
        finalized: this.gms.finalizationComplete,
        terminalConfirmation: this.gms.publicationConfirmedSha256 ? 'confirmed' : 'unconfirmed',
        observationSha256: this.gms.publicationConfirmedSha256,
        errors: this.gms.observations.errors.length,
      },
    };
    try {
      await this.persistOwnedJson('collector', summary);
      this.collector.summaryWritten = true;
    } catch (error) {
      this.collector.summaryFailed = true;
      this.errors.push(error);
      try { await rm(this.path('collector'), { force: true }); }
      catch (cleanupError) { this.errors.push(cleanupError); }
    }
  }

  finish(): Promise<AndroidMeasurementOutcome> {
    if (this.lifecycle.finished) return Promise.reject(new Error('ANDROID_ENVIRONMENT: measurement already finalized'));
    this.lifecycle.finished = true;
    this.lifecycle.beginAbort?.abort(new Error('ANDROID_ENVIRONMENT: begin was interrupted by finalization'));
    this.lifecycle.finishPromise = this.finishOwned();
    if (this.lifecycle.terminalCompletion) {
      this.lifecycle.finishPromise.then(this.lifecycle.resolveTerminalCompletion, this.lifecycle.rejectTerminalCompletion);
    }
    return this.lifecycle.finishPromise;
  }

  private async finishOwned(): Promise<AndroidMeasurementOutcome> {
    if (this.lifecycle.beginPromise) await this.lifecycle.beginPromise.catch(() => undefined);
    await this.startGmsFinalization();
    if (this.lifecycle.active) {
      await this.attempt(() => this.snapshot('end'));
      await this.attempt(() => this.waitForMarker('END'));
    } else this.errors.push(new Error('ANDROID_ENVIRONMENT: no active measurement to finish'));
    await this.closeCollector();
    if (this.collector.failure && !this.errors.includes(this.collector.failure)) this.errors.push(this.collector.failure);
    let assessment: AndroidEnvironmentCheck | undefined;
    let assessmentSha256: string | undefined;
    await this.attempt(async () => {
      if (!this.identity) throw new Error('ANDROID_ENVIRONMENT: measurement identity missing');
      await this.persistOwnedJson('identity', this.identity);
    });
    await this.attempt(async () => {
      if (!this.identity) throw new Error('ANDROID_ENVIRONMENT: measurement identity missing');
      const inputs = await readEnvironmentInputs(this.outputDir);
      if (this.collector.summaryFailed) {
        inputs.collector = '';
        delete inputs.gmsObservations;
      }
      assessment = assessAndroidEnvironment(inputs, this.identity);
      assessmentSha256 = await this.persistOwnedJson('check', assessment);
      assessment = JSON.parse(`${JSON.stringify(sanitizeValue(assessment), null, 2)}\n`) as AndroidEnvironmentCheck;
      if (assessment.collection.status !== 'PASS') throw new Error('ANDROID_ENVIRONMENT: collection guarantee failed');
    });
    const completion = {
      passed: this.errors.length === 0,
      errors: this.errors.map(error => error instanceof Error ? error.message : String(error)),
      assessmentSha256,
    };
    await this.attempt(() => this.persistOwnedJson('completion', completion));
    return { assessment: assessment ? structuredClone(assessment) : undefined, assessmentSha256, errors: [...this.errors] };
  }
}

async function readBoundedStdin(maximum: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const chunk of process.stdin) {
    const value = Buffer.from(chunk);
    bytes += value.byteLength;
    if (bytes > maximum) throw new Error('GMS diagnostic input exceeds its byte bound');
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

function gmsDiagnosticParserProgram(): string {
  const parserModule = fileURLToPath(new URL('./android-environment.ts', import.meta.url));
  return `const startedAt = process.hrtime.bigint();
process.stdout.write(\`PARSER_STARTED \${startedAt}\\n\`);
void (async () => {
  const parser = await import(${JSON.stringify(parserModule)});
  const { createHash } = await import('node:crypto');
  const readyAt = process.hrtime.bigint();
  process.stdout.write(\`PARSER_READY \${readyAt}\\n\`);
  const chunks = [];
  let bytes = 0;
  for await (const chunk of process.stdin) {
    const value = Buffer.from(chunk);
    bytes += value.byteLength;
    if (bytes > ${ANDROID_GMS_DUMP_LIMIT}) throw new Error('GMS diagnostic input exceeds its byte bound');
    chunks.push(value);
  }
  const input = Buffer.concat(chunks);
  const inputSha256 = createHash('sha256').update(input).digest('hex');
  process.stdout.write(\`PARSER_INPUT \${input.byteLength} \${inputSha256}\\n\`);
  const state = parser.parseAndroidGmsComponentState(input.toString('utf8'));
  process.stdout.write(\`\${JSON.stringify(state)}\\nPARSER_OUTPUT \${process.hrtime.bigint()}\\n\`);
})().catch(error => {
  const message = error instanceof Error ? error.message : 'GMS diagnostic parser failed';
  process.stderr.write(\`\${message.slice(0, 500)}\\n\`);
  process.exitCode = 1;
});`;
}

function recordGmsParserProgress(entry: AndroidGmsStageObservation, output: string): void {
  const lines = output.split('\n');
  const started = lines[0]?.match(/^PARSER_STARTED (\d+)$/u)?.[1];
  const ready = lines[1]?.match(/^PARSER_READY (\d+)$/u)?.[1];
  const input = lines[2]?.match(/^PARSER_INPUT (\d+) ([a-f0-9]{64})$/u);
  entry.parserStarted = Boolean(started);
  entry.parserReady = Boolean(ready);
  entry.parserInputBytes = input ? Number(input[1]) : undefined;
  entry.parserInputSha256 = input?.[2];
  entry.parserOutputProduced = lines.some(line => /^PARSER_OUTPUT \d+$/u.test(line));
  if (started && ready && BigInt(ready) >= BigInt(started)) {
    entry.parserStartupMs = Math.ceil(Number(BigInt(ready) - BigInt(started)) / 1_000_000);
  }
}

function parseGmsParserOutput(output: string, inputBytes: number, inputSha256: string): { state: AndroidGmsComponentState; startupMs: number } {
  const lines = output.split('\n');
  if (lines.length !== 6 || lines[5] !== '') throw new Error('GMS diagnostic normalization output protocol is malformed');
  const started = lines[0].match(/^PARSER_STARTED (\d+)$/u)?.[1];
  const ready = lines[1].match(/^PARSER_READY (\d+)$/u)?.[1];
  const input = lines[2].match(/^PARSER_INPUT (\d+) ([a-f0-9]{64})$/u);
  const outputAt = lines[4].match(/^PARSER_OUTPUT (\d+)$/u)?.[1];
  if (!started || !ready || !input || !outputAt || BigInt(ready) <= BigInt(started) || BigInt(outputAt) < BigInt(ready)
    || Number(input[1]) !== inputBytes || input[2] !== inputSha256) {
    throw new Error('GMS diagnostic normalization process readiness or input receipt is invalid');
  }
  return {
    state: JSON.parse(lines[3]) as AndroidGmsComponentState,
    startupMs: Math.ceil(Number(BigInt(ready) - BigInt(started)) / 1_000_000),
  };
}

async function runAndroidTransportObservationTrigger(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(4_096)).toString('utf8')) as {
    serial?: string; outputDir?: string; guestEpochSeconds?: string; triggeredAt?: string; anchorGuestEpochSeconds?: string;
    guestClockSample?: { guestEpochSeconds?: string; startedAt?: string; endedAt?: string };
  };
  const guestEpochSeconds = request.guestEpochSeconds;
  const triggeredAt = request.triggeredAt;
  if (!request.serial || !request.outputDir || !guestEpochSeconds || !androidGuestEpochNanoseconds(guestEpochSeconds)
    || !triggeredAt || !Number.isFinite(Date.parse(triggeredAt))) {
    throw new Error('Android transport trigger request is malformed');
  }
  let guestClockSample: AndroidTransportTrigger['guestClockSample'];
  if (request.guestClockSample) {
    const sample = request.guestClockSample;
    const candidateEpoch = androidGuestEpochNanoseconds(guestEpochSeconds);
    const anchorEpoch = request.anchorGuestEpochSeconds && androidGuestEpochNanoseconds(request.anchorGuestEpochSeconds);
    const sampleEpoch = typeof sample.guestEpochSeconds === 'string'
      ? androidGuestEpochNanoseconds(sample.guestEpochSeconds, true) : undefined;
    if (Object.keys(sample).length !== 3 || !candidateEpoch || !anchorEpoch || !sampleEpoch
      || typeof sample.startedAt !== 'string' || !Number.isFinite(Date.parse(sample.startedAt))
      || typeof sample.endedAt !== 'string' || !Number.isFinite(Date.parse(sample.endedAt))
      || Date.parse(sample.endedAt) < Date.parse(sample.startedAt) || candidateEpoch < anchorEpoch
      || sampleEpoch < candidateEpoch
      || sampleEpoch - candidateEpoch > BigInt(ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS) * 1_000_000n) {
      throw new Error('Android transport guest clock receipt is malformed or stale');
    }
    guestClockSample = { guestEpochSeconds: sample.guestEpochSeconds!, startedAt: sample.startedAt, endedAt: sample.endedAt };
  } else if (request.anchorGuestEpochSeconds !== undefined) {
    throw new Error('Android transport trigger anchor is unbound to a guest clock sample');
  }
  const transport = new AndroidTransportObservation(request.serial, request.outputDir);
  await transport.observeTrigger({ guestEpochSeconds: guestEpochSeconds!, triggeredAt,
    ...(request.anchorGuestEpochSeconds ? { anchorGuestEpochSeconds: request.anchorGuestEpochSeconds } : {}),
    ...(guestClockSample ? { guestClockSample } : {}) });
  process.stdout.write(`${JSON.stringify({ failure: transport.failure })}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  const modes: Record<string, () => Promise<void>> = {
    '--android-transport-observation-trigger': runAndroidTransportObservationTrigger,
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

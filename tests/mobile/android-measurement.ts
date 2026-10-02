import { existsSync, renameSync, rmSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { validateMeasurementIdentity, type MeasurementIdentity } from './support/mobile-result';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Writable } from 'node:stream';
import { ANDROID_LOG_LIMIT, androidMeasurementMarker, parseAndroidProcesses, assessAndroidEnvironment, readEnvironmentInputs, type AndroidEnvironmentCheck, type AndroidPlannedTermination, type AndroidGmsComponentState } from './android-environment';
import { command } from './support/process';
import { BoundedCommandError, boundedCommand, type BoundedCommandResult } from './support/bounded-process';
import { spawnOwnedProcess, type OwnedProcess, type RetirementEvidence } from './support/owned-process';
import { PhaseBudget } from './support/budget';
import { redactText, sanitizeValue, writeSanitizedJson } from './support/diagnostics';
import { requireOwnedDevice } from './support/device';
import { isAndroidPackageProcess, isAndroidTerminationPackage } from './android-events';
import type { AppiumClient } from './support/webdriver';
import { ANDROID_TRANSPORT_TRIGGER_DEADLINE_MS, ANDROID_TRANSPORT_TRIGGER_FRESHNESS_MS, AndroidTransportObservation,
  AndroidTransportTriggerDetector, androidGuestEpochNanoseconds, transportAdbObservation,
  type AndroidTransportCandidate, type AndroidTransportTestFixture, type AndroidTransportTrigger } from './support/android-transport';

export interface AndroidMeasurementOutcome {
  assessment?: AndroidEnvironmentCheck;
  assessmentSha256?: string;
  errors: unknown[];
}

export interface AndroidMeasurementIO {
  command: typeof command;
  boundedCommand: typeof boundedCommand;
  writeFile: typeof writeFile;
  readFile: typeof readFile;
  writeJson: typeof writeSanitizedJson;
  stopProcess: (collector: OwnedProcess) => Promise<RetirementEvidence>;
  spawnCollector: () => OwnedProcess;
  collectorObserverStdout?: Writable;
  collectorObserverStderr?: Writable;
  collectorObserverSupervisor?: Writable;
  monotonicNow: () => number;
  transport: Pick<AndroidTransportObservation, 'observe' | 'finish' | 'failure'>;
  transportObservationFixture?: AndroidTransportTestFixture;
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
const ANDROID_GMS_OWNERSHIP_TIMEOUT_MS = 500;
const ANDROID_GMS_PARSE_RESERVE_MS = 250;
const ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS = 40;
const ANDROID_GMS_PERSISTENCE_TIMEOUT_MS = 1_000;
const ANDROID_GMS_STAGE_TIMEOUT_MS = ANDROID_GMS_OWNERSHIP_TIMEOUT_MS + ANDROID_GMS_COMMAND_TIMEOUT_MS + ANDROID_GMS_PARSE_RESERVE_MS;
const ANDROID_GMS_TOTAL_BYTES_LIMIT = ANDROID_GMS_DUMP_LIMIT * ANDROID_GMS_OBSERVATION_STAGES.length;
const ANDROID_GMS_TOTAL_TIME_LIMIT_MS = ANDROID_GMS_STAGE_TIMEOUT_MS * ANDROID_GMS_OBSERVATION_STAGES.length;
const ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS = ANDROID_GMS_PERSISTENCE_TIMEOUT_MS * (ANDROID_GMS_OBSERVATION_STAGES.length + 1);
const ANDROID_GMS_HASH_ENCODING = 'sha256-hex-dot-separated-v1' as const;
const ANDROID_GMS_OBSERVATION_FILE = 'android-environment-gms-observations.json';
const ANDROID_GMS_OBSERVATION_FILE_LIMIT = 8_000_000;
const ANDROID_MEASUREMENT_JSON_FILES = new Set(['android-environment-collector.json', 'android-environment-completion.json',
  'android-environment-identity.json', 'android-environment-check.json']);
const ANDROID_MEASUREMENT_LOG_FILE = 'android-qualification-logcat.log';
const ANDROID_MEASUREMENT_FILE_WRITE_TIMEOUT_MS = 5_000;
const ANDROID_BEGIN_OPERATION_TIMEOUT_MS = 1_000;
const ANDROID_BEGIN_TOTAL_TIMEOUT_MS = 4_000;

function collectorRetired(evidence: RetirementEvidence): boolean {
  return evidence.targetExitObserved && evidence.targetCloseObserved && evidence.anchorExitObserved
    && evidence.anchorCloseObserved && evidence.groupAbsent && evidence.inputClosedObserved
    && evidence.stdoutNaturalEnd && evidence.stdoutCloseObserved && evidence.stderrNaturalEnd
    && evidence.stderrCloseObserved && evidence.supervisorExitObserved === true && evidence.supervisorCloseObserved === true
    && evidence.supervisorExitCode === 0 && evidence.supervisorSignal === null;
}

function gmsWriterBoundary(result: BoundedCommandResult, writerTimeoutMs: number | undefined, dispatchedAfterMs: number | undefined): string {
  return `GMS writer boundary writerTimeoutMs=${writerTimeoutMs ?? 'none'} dispatchedAfterMs=${dispatchedAfterMs ?? 'none'}`
    + ` durationMs=${result.durationMs} targetStarted=${result.targetPID !== undefined} timedOut=${result.timedOut}`
    + ` aborted=${result.aborted} childExited=${result.childExited} targetClosed=${result.targetCloseObserved}`
    + ` ownedProcessesExited=${result.ownedProcessesExited} stdioClosed=${result.stdioClosed}`
    + ` exitCode=${result.code} signal=${result.signal ?? 'none'} progress=${gmsWriterProgress(result.stderr)}`;
}

export function gmsWriterProgress(stderr: string): string {
  const steps: string[] = [];
  for (const line of stderr.split('\n')) {
    const step = /^GMS_WRITER_PROGRESS (ready|input|directory|staged) (\d{1,7})(?: (\d{1,9}))?$/u.exec(line);
    if (step && steps.length < 4) steps.push(`${step[1]}@${step[2]}${step[3] ? `:${step[3]}` : ''}`);
  }
  return steps.length ? steps.join(',') : 'none';
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

export class AndroidEnvironmentMeasurement {
  readonly id = randomUUID();
  private collector?: OwnedProcess;
  private chunks: Buffer[] = [];
  private bytes = 0;
  private collectorFailure?: Error;
  private active = false;
  private started = false;
  private readonly operations: AndroidPlannedTermination[] = [];
  private identity?: MeasurementIdentity;
  private requireGmsObservationsValue = false;
  private gmsStageIndex = 0;
  private gmsObservationsSequenceFailed = false;
  private readonly gmsObservations: AndroidGmsObservations = {
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
  private gmsStageInFlight = false;
  private gmsStagePromise?: Promise<void>;
  private gmsStageAbort?: AbortController;
  private gmsFinalizationStarted = false;
  private gmsFinalizationComplete = false;
  private gmsFinalizationPromise?: Promise<void>;
  private gmsPersistenceFailed = false;
  private gmsPublicationConfirmedSha256?: string;
  private transportObservationFailure?: string;
  private readonly transportTrigger = new AndroidTransportTriggerDetector(this.id);
  private transportObservationAbort?: AbortController;
  private transportObservationTask?: Promise<void>;
  private transportCandidatesQueued = 0;
  private measurementBegun = false;
  private beginFailed = false;
  private beginAbort?: AbortController;
  private beginPromise?: Promise<void>;
  private collectorClosed = false;
  private collectorDataFrozen = false;
  private collectorStdoutHandler?: (chunk: Buffer) => void;
  private collectorStderrHandler?: () => void;
  private collectorSummaryWritten = false;
  private collectorSummaryFailed = false;
  private finishPromise?: Promise<AndroidMeasurementOutcome>;
  private terminalCompletion?: Promise<AndroidMeasurementOutcome>;
  private resolveTerminalCompletion?: (outcome: AndroidMeasurementOutcome) => void;
  private rejectTerminalCompletion?: (reason: unknown) => void;
  private collectorPersistedBytes = 0;
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

  private readonly collectorHardDeadlineNs?: bigint;

  constructor(private readonly serial: string, private readonly outputDir: string, private readonly toolchains: string,
    io: Partial<AndroidMeasurementIO> = {}, lifetime?: PhaseBudget) {
    this.collectorHardDeadlineNs = lifetime
      ? process.hrtime.bigint() + BigInt(Math.max(0, Math.floor(lifetime.remainingMs))) * 1_000_000n
      : undefined;
    const defaultStopCollector = (collector: OwnedProcess) => collector.retire('measurement-finished');
    this.io = { command, boundedCommand, writeFile, readFile, writeJson: writeSanitizedJson, stopProcess: defaultStopCollector,
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
      monotonicNow: () => performance.now(), transport: new AndroidTransportObservation(serial, outputDir), ...io };
    this.transport = this.io.transport;
  }

  failureOutcome(): AndroidMeasurementOutcome {
    const errors = [...this.errors];
    const diagnosticFailure = this.gmsDiagnosticFailure();
    if (diagnosticFailure && !errors.some((error) => error instanceof Error && error.message === diagnosticFailure.message)) errors.push(diagnosticFailure);
    return { errors };
  }

  requireGmsObservations(): void {
    if (this.started || this.finished || this.requireGmsObservationsValue || this.gmsFinalizationStarted) {
      throw new Error('ANDROID_ENVIRONMENT: GMS observation requirement must be set once before begin');
    }
    this.requireGmsObservationsValue = true;
  }

  async observeGmsStage(stage: AndroidGmsObservationStage, budget: PhaseBudget): Promise<void> {
    if (this.gmsFinalizationStarted || this.finished) throw new Error('ANDROID_ENVIRONMENT: GMS observations are finalized');
    const startedAt = new Date().toISOString();
    const started = this.io.monotonicNow();
    const expected = ANDROID_GMS_OBSERVATION_STAGES[this.gmsStageIndex];
    const entry: AndroidGmsStageObservation = { stage, outcome: 'failed', startedAt, endedAt: startedAt, durationMs: 0 };
    const rejectStage = (message: string) => {
      this.gmsObservationsSequenceFailed = true;
      entry.error = message;
      entry.endedAt = new Date().toISOString();
      entry.durationMs = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      if (this.gmsObservations.stages.length < ANDROID_GMS_OBSERVATION_STAGES.length) this.gmsObservations.stages.push(entry);
      this.recordGmsError(message);
    };
    if (!this.requireGmsObservationsValue) {
      rejectStage('GMS stage observation requirement was not activated');
      return;
    }
    if (this.gmsStageInFlight) {
      rejectStage('GMS stage observation calls cannot overlap');
      return;
    }
    if (this.gmsObservationsSequenceFailed || stage !== expected) {
      rejectStage(`GMS stage order mismatch: expected ${expected || 'no further stage'}, received ${stage}`);
      return;
    }
    const preMeasurementStage = this.gmsStageIndex < 2;
    if ((preMeasurementStage && this.started) || (!preMeasurementStage && (!this.measurementBegun || this.beginFailed))) {
      rejectStage(preMeasurementStage
        ? 'GMS pre-measurement stage occurred after begin'
        : 'GMS post-measurement stage requires a successfully begun measurement');
      return;
    }
    if (this.gmsObservations.stages.length >= ANDROID_GMS_OBSERVATION_STAGES.length) {
      rejectStage('GMS diagnostic stage limit was reached');
      return;
    }

    this.gmsStageIndex++;
    this.gmsStageInFlight = true;
    this.gmsStageAbort = new AbortController();
    this.gmsObservations.stages.push(entry);
    const task = this.collectGmsStage(entry, started, budget, this.gmsStageAbort.signal);
    this.gmsStagePromise = task;
    try {
      await task;
    } finally {
      if (this.gmsStagePromise === task) {
        this.gmsStagePromise = undefined;
        this.gmsStageAbort = undefined;
        this.gmsStageInFlight = false;
      }
    }
  }

  private async collectGmsStage(entry: AndroidGmsStageObservation, started: number, budget: PhaseBudget, signal: AbortSignal): Promise<void> {
    const availableTotal = ANDROID_GMS_TOTAL_TIME_LIMIT_MS - this.gmsObservations.totalCollectionMs;
    const availableParent = budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS;
    const collectionAllowance = Math.floor(Math.min(ANDROID_GMS_STAGE_TIMEOUT_MS, availableTotal, availableParent));
    const collectionDeadline = started + Math.max(0, collectionAllowance);
    try {
      if (collectionAllowance <= ANDROID_GMS_PARSE_RESERVE_MS) throw new Error('GMS diagnostic collection has no bounded stage allowance');
      const remainingBytes = ANDROID_GMS_TOTAL_BYTES_LIMIT - this.gmsObservations.totalCommandOutputBytes;
      if (remainingBytes <= 0) throw new Error('GMS diagnostic collection exhausted its aggregate byte bound');
      const ownerTimeout = Math.floor(Math.min(ANDROID_GMS_OWNERSHIP_TIMEOUT_MS,
        collectionAllowance - ANDROID_GMS_PARSE_RESERVE_MS - 1));
      const ownershipStarted = this.io.monotonicNow();
      let ownershipResult: BoundedCommandResult | undefined;
      try {
        ownershipResult = await this.io.boundedCommand(process.execPath,
          [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--gms-diagnostic-ownership', this.serial], ownerTimeout, {
            budget,
            label: 'verify GMS diagnostic device ownership',
            maxBytes: 512,
            maxBytesPerStream: 256,
            cleanupReservationMs: Math.min(100, Math.floor(ownerTimeout / 3)),
            signal,
          });
      } catch (error) {
        if (error instanceof BoundedCommandError) {
          ownershipResult = error.result;
          entry.ownershipChildExited = error.result.childExited;
          entry.ownershipProcessesExited = error.result.ownedProcessesExited;
          entry.ownershipStdioClosed = error.result.stdioClosed;
        }
        throw error;
      } finally {
        entry.ownershipDurationMs = Math.ceil(Math.max(0, this.io.monotonicNow() - ownershipStarted));
        if (ownershipResult) {
          entry.ownershipChildExited = ownershipResult.childExited;
          entry.ownershipProcessesExited = ownershipResult.ownedProcessesExited;
          entry.ownershipStdioClosed = ownershipResult.stdioClosed;
        }
      }
      if (entry.ownershipDurationMs > ownerTimeout) throw new Error('GMS owned-device verification exceeded its monotonic time bound');
      if (ownershipResult.stdout !== 'OWNED\n') throw new Error('GMS owned-device verification returned unexpected output');
      if (signal.aborted || this.gmsFinalizationStarted) throw new Error('GMS stage was interrupted by finalization');

      const remainingCollection = Math.min(collectionDeadline - this.io.monotonicNow(), budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS);
      const timeoutMs = Math.floor(Math.min(ANDROID_GMS_COMMAND_TIMEOUT_MS,
        remainingCollection - ANDROID_GMS_PARSE_RESERVE_MS));
      if (timeoutMs < 1) throw new Error('GMS diagnostic collection exhausted its command allowance');
      const outputLimit = Math.min(ANDROID_GMS_COMMAND_OUTPUT_LIMIT, remainingBytes);
      entry.timeoutMs = timeoutMs;
      const commandAbort = new AbortController();
      const abortCommand = () => commandAbort.abort(signal.reason || new Error('GMS stage was interrupted'));
      signal.addEventListener('abort', abortCommand, { once: true });
      const command = () => this.io.boundedCommand('adb', ['-s', this.serial, 'shell', 'dumpsys', 'package', ANDROID_GMS_PACKAGE], timeoutMs, {
        budget,
        label: `observe GMS components at ${entry.stage}`,
        maxBytes: outputLimit,
        maxBytesPerStream: ANDROID_GMS_DUMP_LIMIT,
        cleanupReservationMs: 200,
        signal: commandAbort.signal,
      });
      let result: BoundedCommandResult;
      try {
        result = await command();
      } catch (error) {
        if (error instanceof BoundedCommandError) {
          this.recordGmsCommand(entry, error.result);
          throw this.gmsCommandFailure(error.result);
        }
        throw error;
      } finally {
        signal.removeEventListener('abort', abortCommand);
      }
      this.recordGmsCommand(entry, result);
      if (signal.aborted || this.gmsFinalizationStarted) throw new Error('GMS stage was interrupted by finalization');
      if (result.stderrBytes > 0 || result.stderr !== '') throw new Error('GMS diagnostic package command returned stderr');
      if (!result.stdout.endsWith('\n') || result.stdoutBytes > ANDROID_GMS_DUMP_LIMIT
        || Buffer.byteLength(result.stdout) !== result.stdoutRetainedBytes) {
        throw new Error('GMS diagnostic package dump is truncated, invalid UTF-8 or exceeds its parsing limit');
      }
      const normalizationStarted = this.io.monotonicNow();
      let retained: AndroidGmsStoredComponentState | undefined;
      const normalizationTimeoutMs = Math.floor(Math.min(ANDROID_GMS_PARSE_RESERVE_MS,
        collectionDeadline - this.io.monotonicNow(), budget.remainingMs - ANDROID_GMS_PERSISTENCE_TIMEOUT_MS));
      if (normalizationTimeoutMs < 1) throw new Error('GMS diagnostic normalization has no bounded time remaining');
      entry.parseTimeoutMs = normalizationTimeoutMs;
      entry.parseCleanupReservationMs = ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS;
      try {
        const normalized = await this.io.boundedCommand(process.execPath,
          ['--no-env-file', '--eval', gmsDiagnosticParserProgram()], normalizationTimeoutMs, {
            budget,
            label: `normalize GMS components at ${entry.stage}`,
            maxBytes: ANDROID_GMS_COMMAND_OUTPUT_LIMIT,
            maxBytesPerStream: ANDROID_GMS_DUMP_LIMIT,
            cleanupReservationMs: ANDROID_GMS_PARSE_CLEANUP_RESERVATION_MS,
            input: result.stdout,
            signal,
          });
        entry.normalizationTimedOut = normalized.timedOut;
        entry.parseTargetStarted = Boolean(normalized.targetPID);
        entry.parseTargetClosed = normalized.targetCloseObserved;
        entry.parseChildExited = normalized.childExited;
        entry.parseOwnedProcessesExited = normalized.ownedProcessesExited;
        entry.parseStdioClosed = normalized.stdioClosed;
        recordGmsParserProgress(entry, normalized.stdout);
        if (normalized.stderrBytes || normalized.stdoutBytes !== normalized.stdoutRetainedBytes
          || !normalized.stdout.endsWith('\n') || normalized.targetCloseObserved !== true) {
          throw new Error('GMS diagnostic normalization output was truncated, incomplete or invalid');
        }
        const parserOutput = parseGmsParserOutput(normalized.stdout, result.stdoutRetainedBytes, result.stdoutSha256);
        entry.parserStartupMs = parserOutput.startupMs;
        retained = retainedGmsState(parserOutput.state);
        const componentHash = createHash('sha256').update(JSON.stringify({
          enabledComponents: retained.enabledComponents,
          disabledComponents: retained.disabledComponents,
        })).digest('hex');
        if (retained.packageName !== ANDROID_GMS_PACKAGE || !Array.isArray(retained.enabledComponents) || !Array.isArray(retained.disabledComponents)
          || retained.enabledComponents.some(value => typeof value !== 'string') || retained.disabledComponents.some(value => typeof value !== 'string')
          || retained.hashEncoding !== ANDROID_GMS_HASH_ENCODING
          || decodeRetainedSha256(retained.componentStateSha256) !== componentHash
          || decodeRetainedSha256(retained.componentStateSha256) !== decodeRetainedSha256(retained.sourceHashes.componentStateSha256)
          || decodeRetainedSha256(retained.dumpSha256) !== result.stdoutSha256
          || decodeRetainedSha256(retained.dumpSha256) !== decodeRetainedSha256(retained.sourceHashes.dumpSha256)) {
          throw new Error('GMS normalized state or retained digest is inconsistent');
        }
      } catch (error) {
        if (error instanceof BoundedCommandError) {
          entry.normalizationTimedOut = error.result.timedOut;
          entry.parseTargetStarted = Boolean(error.result.targetPID);
          entry.parseTargetClosed = error.result.targetCloseObserved;
          entry.parseChildExited = error.result.childExited;
          entry.parseOwnedProcessesExited = error.result.ownedProcessesExited;
          entry.parseStdioClosed = error.result.stdioClosed;
          recordGmsParserProgress(entry, error.result.stdout);
          const detail = redactText(error.result.stderr).trim().slice(0, 300);
          throw new Error(error.result.timedOut ? 'GMS diagnostic normalization timed out'
            : `GMS diagnostic normalization failed${detail ? `: ${detail}` : ' or exceeded its output bound'}`, { cause: error });
        }
        throw error;
      } finally {
        entry.parseDurationMs = Math.ceil(Math.max(0, this.io.monotonicNow() - normalizationStarted));
      }
      if (this.io.monotonicNow() > collectionDeadline || budget.exhausted
        || entry.parseDurationMs > ANDROID_GMS_PARSE_RESERVE_MS) {
        throw new Error('GMS diagnostic normalization exceeded its stage or parent time bound');
      }
      if (signal.aborted || this.gmsFinalizationStarted) throw new Error('GMS stage was interrupted by finalization');
      if (!retained) throw new Error('GMS diagnostic normalization produced no retained state');
      entry.state = retained;
      entry.outcome = 'collected';
    } catch (error) {
      if (signal.aborted || this.gmsFinalizationStarted) {
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
      this.gmsObservations.totalCollectionMs += entry.durationMs;
      if (this.gmsObservations.totalCollectionMs > ANDROID_GMS_TOTAL_TIME_LIMIT_MS) {
        entry.outcome = 'failed';
        delete entry.state;
        entry.error = 'GMS diagnostic collection exceeded its total time bound';
        this.recordGmsError(entry.error);
      }
      if (!this.gmsFinalizationStarted) await this.persistGmsObservations(budget, entry, signal);
    }
  }

  private gmsCommandFailure(result: BoundedCommandResult): Error {
    if (result.timedOut) return new Error('GMS diagnostic command timed out');
    if (result.outputLimitExceeded) return new Error('GMS diagnostic command exceeded its combined or per-stream byte bound');
    if (result.aborted) return new Error('GMS diagnostic command was aborted');
    if (result.launchError) return new Error('GMS diagnostic command could not be launched');
    return new Error(`GMS diagnostic package command exited with status ${result.code}`);
  }

  private recordGmsCommand(entry: AndroidGmsStageObservation, result: BoundedCommandResult): void {
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
    this.gmsObservations.totalCommandOutputBytes += result.stdoutBytes + result.stderrBytes;
    this.gmsObservations.totalRetainedCommandOutputBytes += result.stdoutRetainedBytes + result.stderrRetainedBytes;
  }

  private recordGmsError(message: string): void {
    if (!this.gmsObservations.errors.includes(message) && this.gmsObservations.errors.length < 50) {
      this.gmsObservations.errors.push(message);
    }
  }

  private gmsDiagnosticFailure(): Error | undefined {
    if (!this.gmsObservations.errors.length) return undefined;
    return new Error(`ANDROID_ENVIRONMENT: ${this.gmsObservations.errors.length} GMS diagnostic observation error(s)`);
  }

  finalizeGmsObservations(): Promise<void> {
    const finalization = this.startGmsFinalization();
    if (this.finishPromise || (this.measurementBegun && !this.collectorClosed)) {
      const terminal = this.finishPromise ?? this.waitForTerminalCompletion();
      return Promise.all([finalization, terminal]).then(([, outcome]) => {
        if (outcome.errors.length || outcome.assessment?.collection.status !== 'PASS') {
          throw new Error('ANDROID_ENVIRONMENT: terminal collector confirmation failed');
        }
      });
    }
    return finalization.then(() => {
      if (this.collectorSummaryFailed) throw new Error('ANDROID_ENVIRONMENT: terminal collector summary publication failed');
    });
  }

  private waitForTerminalCompletion(): Promise<AndroidMeasurementOutcome> {
    if (!this.terminalCompletion) {
      this.terminalCompletion = new Promise<AndroidMeasurementOutcome>((resolve, reject) => {
        this.resolveTerminalCompletion = resolve;
        this.rejectTerminalCompletion = reject;
      });
    }
    return this.terminalCompletion;
  }

  private startGmsFinalization(): Promise<void> {
    if (this.gmsFinalizationPromise) return this.gmsFinalizationPromise;
    if (!this.requireGmsObservationsValue || this.gmsFinalizationComplete) return Promise.resolve();
    this.gmsFinalizationStarted = true;
    this.gmsFinalizationPromise = this.finalizeGmsObservationsOwned();
    return this.gmsFinalizationPromise;
  }

  private async finalizeGmsObservationsOwned(): Promise<void> {
    this.beginAbort?.abort(new Error('GMS observation finalization interrupted measurement begin'));
    if (this.beginPromise) await this.beginPromise.catch(() => undefined);
    if (this.gmsStagePromise) {
      this.gmsStageAbort?.abort(new Error('GMS stage was interrupted by finalization'));
      this.gmsObservationsSequenceFailed = true;
      await this.gmsStagePromise.catch(() => undefined);
    }
    if (this.gmsStageIndex !== ANDROID_GMS_OBSERVATION_STAGES.length || this.gmsObservationsSequenceFailed || !this.measurementBegun || this.beginFailed) {
      this.recordGmsError('GMS diagnostic stage sequence is incomplete or unbound to a successful measurement');
    }
    this.gmsObservations.finalized = true;
    this.gmsObservations.terminalPublication = 'awaiting-collector-confirmation';
    await this.persistGmsObservations();
    this.gmsFinalizationComplete = true;
    const failure = this.gmsDiagnosticFailure();
    if (failure && !this.errors.some((error) => error instanceof Error && error.message === failure.message)) this.errors.push(failure);
    if (this.collectorClosed) await this.writeCollectorSummary();
  }

  private async persistGmsObservations(budget?: PhaseBudget, entry?: AndroidGmsStageObservation, stageSignal?: AbortSignal): Promise<void> {
    if (this.gmsPersistenceFailed) return;
    const started = this.io.monotonicNow();
    const remainingPersistence = ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS - this.gmsObservations.priorPersistenceAttemptMs;
    const timeoutMs = Math.floor(Math.min(ANDROID_GMS_PERSISTENCE_TIMEOUT_MS, remainingPersistence, budget?.remainingMs ?? remainingPersistence));
    const operationDeadline = started + Math.max(0, timeoutMs);
    let temporary = '';
    let commandResult: BoundedCommandResult | undefined;
    let writerAllocationMs: number | undefined;
    let writerDispatchedAfterMs: number | undefined;
    let succeeded = false;
    try {
      if (timeoutMs < 1 || this.gmsObservations.stages.length > ANDROID_GMS_OBSERVATION_STAGES.length) {
        throw new Error('GMS observation evidence exhausted its persistence bound');
      }
      this.gmsObservations.temporaryEvidenceCleanup = 'not-needed';
      if (this.gmsObservations.finalized) this.gmsPublicationConfirmedSha256 = undefined;
      const snapshot = structuredClone(this.gmsObservations);
      const expected = `${JSON.stringify(sanitizeValue(snapshot), null, 2)}\n`;
      const expectedBytes = Buffer.byteLength(expected);
      if (expectedBytes > ANDROID_GMS_OBSERVATION_FILE_LIMIT) throw new Error('GMS observation evidence exceeds its file bound');
      const expectedSha256 = createHash('sha256').update(expected).digest('hex');
      const target = resolve(join(this.outputDir, ANDROID_GMS_OBSERVATION_FILE));
      const candidateTemporary = join(this.outputDir, `.${basename(target)}.${randomUUID()}.pending`);
      const request = JSON.stringify({ directory: this.outputDir, snapshot, expectedBytes, expectedSha256, temporary: candidateTemporary, target });
      if (Buffer.byteLength(request) > ANDROID_GMS_OBSERVATION_FILE_LIMIT * 2) {
        throw new Error('GMS observation persistence request exceeds its input bound');
      }
      const commandRemaining = Math.floor(Math.min(operationDeadline - this.io.monotonicNow(), budget?.remainingMs ?? timeoutMs));
      const cleanupReserve = Math.min(200, Math.floor(commandRemaining / 4));
      const writerTimeoutMs = Math.floor(commandRemaining - cleanupReserve - 25);
      if (writerTimeoutMs < 1) throw new Error('GMS observation evidence has no persistence time remaining');
      temporary = candidateTemporary;
      writerAllocationMs = writerTimeoutMs;
      writerDispatchedAfterMs = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      const persisted = await this.io.boundedCommand(process.execPath,
        [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--gms-diagnostic-persist'], writerTimeoutMs, {
          budget,
          label: 'persist GMS diagnostic observations',
          maxBytes: 4_096,
          maxBytesPerStream: 2_048,
          cleanupReservationMs: Math.min(150, Math.floor(writerTimeoutMs / 3)),
          input: request,
          signal: stageSignal,
        });
      commandResult = persisted;
      const receipt = JSON.parse(persisted.stdout) as { byteLength?: number; sha256?: string };
      if (receipt.byteLength !== expectedBytes || receipt.sha256 !== expectedSha256
        || Buffer.byteLength(expected) > ANDROID_GMS_OBSERVATION_FILE_LIMIT
        || stageSignal?.aborted || budget?.exhausted || this.io.monotonicNow() - started > timeoutMs) {
        throw new Error('GMS observation evidence failed bounded sanitized write/read-back verification');
      }
      succeeded = true;
      this.gmsObservations.persistenceRetirement = 'retired';
      this.gmsObservations.temporaryEvidenceCleanup = 'not-needed';
      if (snapshot.finalized) this.gmsPublicationConfirmedSha256 = expectedSha256;
      temporary = '';
    } catch (error) {
      if (error instanceof BoundedCommandError) commandResult = error.result;
      const retired = !commandResult || (commandResult.childExited && commandResult.ownedProcessesExited && commandResult.stdioClosed);
      this.gmsObservations.persistenceRetirement = retired ? 'retired' : 'pending-temporary-write';
      if (!retired) this.gmsObservations.temporaryEvidenceCleanup = 'pending';
      if (temporary && retired) {
        const cleanupTimeout = Math.floor(Math.min(200, operationDeadline - this.io.monotonicNow(), budget?.remainingMs ?? timeoutMs));
        if (cleanupTimeout < 1) {
          this.gmsObservations.persistenceRetirement = 'pending-temporary-write';
          this.gmsObservations.temporaryEvidenceCleanup = 'pending';
        }
        else {
          try {
            const cleanup = await this.io.boundedCommand(process.execPath,
              [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--gms-diagnostic-cleanup'], cleanupTimeout, {
                label: 'remove GMS diagnostic temporary evidence',
                maxBytes: 512,
                maxBytesPerStream: 256,
                cleanupReservationMs: Math.min(50, Math.floor(cleanupTimeout / 3)),
                input: JSON.stringify({ directory: this.outputDir, temporary, target: resolve(join(this.outputDir, ANDROID_GMS_OBSERVATION_FILE)) }),
              });
            const receipt = JSON.parse(cleanup.stdout) as { removed?: boolean };
            if (receipt.removed) this.gmsObservations.temporaryEvidenceCleanup = 'removed-after-failure';
            else {
              this.gmsObservations.temporaryEvidenceCleanup = 'pending';
              this.failGmsPersistence(entry, 'GMS temporary observation cleanup was not confirmed');
            }
          } catch (cleanupError) {
            const cleanupResult = cleanupError instanceof BoundedCommandError ? cleanupError.result : undefined;
            if (!cleanupResult?.childExited || !cleanupResult.ownedProcessesExited || !cleanupResult.stdioClosed) {
              this.gmsObservations.persistenceRetirement = 'pending-temporary-write';
              this.gmsObservations.temporaryEvidenceCleanup = 'pending';
            }
            this.failGmsPersistence(entry, 'GMS temporary observation evidence could not be removed');
          }
        }
      }
      if (stageSignal?.aborted && retired) {
        this.recordGmsError('GMS persistence was interrupted by finalization');
      } else {
        const message = error instanceof BoundedCommandError
          ? `${gmsWriterBoundary(error.result, writerAllocationMs, writerDispatchedAfterMs)}: ${error.message}`
          : error instanceof Error ? error.message : 'GMS observation evidence could not be persisted';
        this.failGmsPersistence(entry, redactText(message).slice(0, 500));
      }
    } finally {
      const duration = Math.ceil(Math.max(0, this.io.monotonicNow() - started));
      this.gmsObservations.priorPersistenceAttemptMs += duration;
      if (entry) entry.persistenceDurationMs = duration;
      if (this.gmsObservations.priorPersistenceAttemptMs > ANDROID_GMS_TOTAL_PERSISTENCE_LIMIT_MS) {
        this.failGmsPersistence(entry, 'GMS observation evidence exceeded its total persistence time bound');
      }
    }
    if (!succeeded && !stageSignal?.aborted && !this.gmsPersistenceFailed) {
      this.failGmsPersistence(entry, 'GMS observation evidence could not be persisted');
    }
  }

  private failGmsPersistence(entry: AndroidGmsStageObservation | undefined, message: string): void {
    this.gmsPersistenceFailed = true;
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

  private async persistOwnedContents(target: string, contents: string, mode: 'json' | 'text', signal?: AbortSignal): Promise<string> {
    const bytes = Buffer.from(contents);
    const expectedSha256 = createHash('sha256').update(bytes).digest('hex');
    const temporary = join(this.outputDir, `.${basename(target)}.${randomUUID()}.pending`);
    const request = JSON.stringify({ directory: this.outputDir, target, temporary, contents, expectedBytes: bytes.length, expectedSha256 });
    try {
      const result = await this.io.boundedCommand(process.execPath,
        [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), `--android-measurement-persist-${mode}`],
        mode === 'text' ? ANDROID_MEASUREMENT_FILE_WRITE_TIMEOUT_MS : ANDROID_GMS_PERSISTENCE_TIMEOUT_MS, {
          label: `persist Android measurement ${basename(target)}`,
          maxBytes: 1_024,
          maxBytesPerStream: 512,
          cleanupReservationMs: 150,
          input: request,
          signal,
        });
      const receipt = JSON.parse(result.stdout) as { byteLength?: number; sha256?: string };
      if (receipt.byteLength !== bytes.length || receipt.sha256 !== expectedSha256) {
        throw new Error('ANDROID_ENVIRONMENT: owned measurement file write did not confirm its bytes');
      }
      return expectedSha256;
    } catch (error) {
      const retired = !(error instanceof BoundedCommandError)
        || (error.result.childExited && error.result.ownedProcessesExited && error.result.stdioClosed);
      if (retired) {
        await this.cleanupOwnedTemporary(temporary, target).catch(() => undefined);
      }
      throw error;
    }
  }

  private async cleanupOwnedTemporary(temporary: string, target: string): Promise<void> {
    const result = await this.io.boundedCommand(process.execPath,
      [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--android-measurement-cleanup-file'], 200, {
        label: 'remove retired Android measurement temporary file',
        maxBytes: 256,
        maxBytesPerStream: 128,
        cleanupReservationMs: 50,
        input: JSON.stringify({ directory: this.outputDir, temporary, target }),
      });
    if (result.stdout !== '{"removed":true}\n') throw new Error('ANDROID_ENVIRONMENT: temporary file cleanup was not confirmed');
  }

  private async invalidateCollectorSummary(): Promise<void> {
    const result = await this.io.boundedCommand(process.execPath,
      [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), '--android-measurement-invalidate-collector'], 200, {
        label: 'invalidate unconfirmed Android collector summary',
        maxBytes: 256,
        maxBytesPerStream: 128,
        cleanupReservationMs: 50,
        input: JSON.stringify({ directory: this.outputDir, target: this.path('collector') }),
      });
    if (JSON.parse(result.stdout).removed !== true) throw new Error('ANDROID_ENVIRONMENT: unconfirmed collector summary invalidation was not confirmed');
  }

  private async persistOwnedJson(name: 'collector' | 'completion' | 'identity' | 'check', value: unknown, signal?: AbortSignal): Promise<string> {
    const target = this.path(name);
    const content = `${JSON.stringify(sanitizeValue(value), null, 2)}\n`;
    return this.persistOwnedContents(target, content, 'json', signal);
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
    if (this.started || this.finished || this.gmsFinalizationStarted) throw new Error('ANDROID_ENVIRONMENT: measurement cannot begin after start or finalization; rebaseline is forbidden');
    this.started = true;
    const controller = new AbortController();
    this.beginAbort = controller;
    const operation = this.beginMeasurement(controller.signal);
    this.beginPromise = operation;
    try {
      await operation;
    } finally {
      if (this.beginPromise === operation) this.beginPromise = undefined;
      if (this.beginAbort === controller) this.beginAbort = undefined;
    }
  }

  private ensureBeginning(signal: AbortSignal): void {
    if (signal.aborted || this.finished || this.gmsFinalizationStarted) {
      throw signal.reason || new Error('ANDROID_ENVIRONMENT: begin was interrupted by finalization');
    }
  }

  private async runBeginWorker(mode: string, args: string[], input: unknown, signal: AbortSignal, deadline: number): Promise<void> {
    const timeoutMs = Math.floor(Math.min(ANDROID_BEGIN_OPERATION_TIMEOUT_MS, deadline - this.io.monotonicNow()));
    if (timeoutMs < 1) throw new Error('ANDROID_ENVIRONMENT: begin exhausted its bounded setup allowance');
    const result = await this.io.boundedCommand(process.execPath,
      [fileURLToPath(new URL('./android-measurement.ts', import.meta.url)), mode, ...args], timeoutMs, {
        label: `prepare Android measurement ${mode}`,
        maxBytes: 1_024,
        maxBytesPerStream: 512,
        cleanupReservationMs: 150,
        input: input === undefined ? undefined : JSON.stringify(input),
        signal,
      });
    if (result.stdout !== 'BEGIN_OK\n' || result.stderrBytes > 0 || this.io.monotonicNow() > deadline) {
      throw new Error('ANDROID_ENVIRONMENT: bounded measurement setup did not confirm completion');
    }
  }

  private async beginMeasurement(signal: AbortSignal): Promise<void> {
    const deadline = this.io.monotonicNow() + ANDROID_BEGIN_TOTAL_TIMEOUT_MS;
    try {
      if (this.requireGmsObservationsValue && (this.gmsStageIndex < 2 || this.gmsStageInFlight || this.gmsObservationsSequenceFailed)) {
        const error = new Error('ANDROID_ENVIRONMENT: begin requires both ordered pre-measurement GMS stage attempts');
        this.recordGmsError(error.message);
        throw error;
      }
      await this.runBeginWorker('--android-measurement-owner', [this.serial], undefined, signal, deadline);
      this.ensureBeginning(signal);
      await this.runBeginWorker('--android-measurement-mkdir', [], { path: this.outputDir }, signal, deadline);
      this.ensureBeginning(signal);
      await this.runBeginWorker('--android-measurement-session', [], {
        path: this.path('session'), contents: `${JSON.stringify({ id: this.id })}\n`,
      }, signal, deadline);
      this.ensureBeginning(signal);
      await this.runBeginWorker('--android-measurement-operations', [], { path: this.path('operations'), operations: this.operations }, signal, deadline);
      this.ensureBeginning(signal);
      this.collector = this.io.spawnCollector();
      this.collector.failure.then(error => { if (!this.collectorDataFrozen) this.collectorFailure ||= error; });
      this.collector.targetExit.then(exit => {
        if (this.active) this.collectorFailure ||= new Error(exit?.launchError || 'ANDROID_ENVIRONMENT: log collector exited inside measurement');
      });
      this.collectorStderrHandler = () => {
        if (!this.collectorDataFrozen) this.collectorFailure ||= new Error('ANDROID_ENVIRONMENT: log collector reported stderr');
      };
      this.collector.stderr.on('data', this.collectorStderrHandler);
      this.collectorStdoutHandler = (chunk: Buffer) => {
        if (this.collectorDataFrozen) return;
        if (this.requireGmsObservationsValue) {
          const candidates = this.transportTrigger.observe(chunk, this.io.monotonicNow());
          if (this.transportTrigger.failure) this.failTransportObservation(this.transportTrigger.failure);
          for (const candidate of candidates) this.queueTransportCandidate(candidate);
        } else {
          try { this.transport.observe(chunk); } catch (error) { this.collectorFailure ||= error instanceof Error ? error : new Error('ANDROID_ENVIRONMENT: transport observation failed'); }
        }
        this.bytes += chunk.length;
        if (this.bytes > ANDROID_LOG_LIMIT) {
          this.collectorFailure ||= new Error('ANDROID_ENVIRONMENT: measured log exceeds bound');
          this.collector?.stop('collector-output-limit');
          return;
        }
        this.chunks.push(chunk);
      };
      this.collector.stdout.on('data', this.collectorStdoutHandler);
      await this.collector.ready;
      this.ensureBeginning(signal);
      this.collector.start();
      this.active = true;
      await this.snapshot('start', signal);
      this.ensureBeginning(signal);
      const markerDeadline = Date.now() + 5_000;
      await this.waitForMarker('START', signal, markerDeadline);
      this.ensureBeginning(signal);
      this.measurementBegun = true;
    } catch (error) {
      this.beginFailed = true;
      this.measurementBegun = false;
      this.errors.push(error);
      this.transportObservationAbort?.abort(error);
      await this.closeCollector(signal);
      const completion = { passed: false, errors: this.errors.map(error => error instanceof Error ? error.message : String(error)) };
      if (this.requireGmsObservationsValue) await this.attempt(() => this.persistOwnedJson('completion', completion, signal));
      else await this.attempt(() => this.io.writeJson(this.path('completion'), completion));
      throw error;
    }
  }

  private async waitForMarker(marker: string, signal?: AbortSignal, deadline = Date.now() + 5_000): Promise<void> {
    while (Date.now() < deadline) {
      if (signal?.aborted) throw signal.reason || new Error('ANDROID_ENVIRONMENT: marker wait was aborted');
      if (this.collectorFailure) throw this.collectorFailure;
      const output = Buffer.concat(this.chunks).toString('utf8');
      const complete = output.slice(0, output.lastIndexOf('\n') + 1);
      const expected = new RegExp(`^\\d{10}\\.\\d{3,9}\\s+\\d+\\s+\\d+\\s+I\\s+HerdrMeasure: ${this.id} ${marker}\\r?$`, 'mu');
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

  private async closeCollector(signal?: AbortSignal): Promise<void> {
    if (this.collectorClosed) return;
    this.collectorClosed = true;
    this.active = false;
    if (this.collector) {
      let retired = false;
      try {
        const evidence = await this.io.stopProcess(this.collector);
        if (!collectorRetired(evidence)) throw new Error('ANDROID_ENVIRONMENT: log collector target, streams, group or supervisors did not retire naturally');
        retired = true;
      } catch (error) {
        this.collectorFailure ||= error instanceof Error ? error : new Error('ANDROID_ENVIRONMENT: log collector process tree did not retire');
        this.errors.push(error);
      }
      this.collectorDataFrozen = true;
      if (retired) {
        if (this.collectorStdoutHandler) this.collector.stdout.removeListener('data', this.collectorStdoutHandler);
        if (this.collectorStderrHandler) this.collector.stderr.removeListener('data', this.collectorStderrHandler);
        this.collectorStdoutHandler = undefined;
        this.collectorStderrHandler = undefined;
      }
    }
    let raw = Buffer.alloc(0);
    await this.attempt(async () => {
      raw = Buffer.concat(this.chunks);
      const text = raw.toString('utf8');
      if (!Buffer.from(text, 'utf8').equals(raw)) throw new Error('ANDROID_ENVIRONMENT: invalid collector UTF-8');
      const persisted = redactText(text);
      if (this.requireGmsObservationsValue) {
        await this.persistOwnedContents(join(this.outputDir, ANDROID_MEASUREMENT_LOG_FILE), persisted, 'text', signal);
      } else {
        await this.io.writeFile(join(this.outputDir, ANDROID_MEASUREMENT_LOG_FILE), Buffer.from(persisted), { mode: 0o600 });
      }
      this.collectorPersistedBytes = Buffer.byteLength(persisted);
    });
    if (this.requireGmsObservationsValue) {
      if (signal?.aborted) this.transportObservationAbort?.abort(signal.reason);
      if (this.transportObservationTask) await this.attempt(() => this.transportObservationTask!);
    } else await this.attempt(() => this.transport.finish());
    this.chunks = [];
    if (!this.requireGmsObservationsValue || this.gmsFinalizationComplete) await this.writeCollectorSummary();
  }

  private queueTransportCandidate(candidate: AndroidTransportCandidate): void {
    if (this.transportObservationFailure || this.transportTrigger.isAccepted || this.transportCandidatesQueued >= 32) {
      if (this.transportCandidatesQueued >= 32) this.failTransportObservation('collector trigger queue exceeded its bound');
      return;
    }
    this.transportCandidatesQueued++;
    const controller = this.transportObservationAbort || new AbortController();
    this.transportObservationAbort = controller;
    const previous = this.transportObservationTask || Promise.resolve();
    const task = previous.then(() => this.admitTransportCandidate(candidate, controller.signal))
      .finally(() => { this.transportCandidatesQueued--; });
    this.transportObservationTask = task.catch(() => {
      this.failTransportObservation('transport observation could not be saved');
    });
  }

  private async admitTransportCandidate(candidate: AndroidTransportCandidate, signal: AbortSignal): Promise<void> {
    if (this.transportObservationFailure || signal.aborted || this.transportTrigger.isAccepted) return;
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
    const sample = await transportAdbObservation(this.serial, 'clock', this.io.transportObservationFixture?.adbPort ?? 5037,
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
    if (this.io.monotonicNow() >= deadline || !this.transportTrigger.accept()) {
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
    if (this.transportObservationFailure) return;
    this.transportObservationFailure = message;
    this.transportObservationAbort?.abort(new Error(message));
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
          input: JSON.stringify({ serial: this.serial, outputDir: this.outputDir, ...trigger,
            ...(this.io.transportObservationFixture ? { fixture: this.io.transportObservationFixture } : {}) }),
          signal,
        });
      const receipt = JSON.parse(result.stdout) as { failure?: string };
      if (typeof receipt.failure === 'string') this.transportObservationFailure = receipt.failure;
    } catch (error) {
      this.failTransportObservation('transport observation could not be saved');
      throw error;
    }
  }

  private async writeCollectorSummary(): Promise<void> {
    if (this.collectorSummaryWritten) return;
    const summary = {
      bytes: this.collectorPersistedBytes, acquiredBytes: this.bytes, failure: this.collectorFailure?.message || (this.errors.length || this.gmsObservations.errors.length ? 'COLLECTION_FAILED' : undefined), transportObservationFailure: this.transportObservationFailure || this.transport.failure,
      ...(this.requireGmsObservationsValue ? { gmsObservations: {
        file: ANDROID_GMS_OBSERVATION_FILE,
        stages: this.gmsObservations.stages.map(({ stage, outcome }) => ({ stage, outcome })),
        totalCommandOutputBytes: this.gmsObservations.totalCommandOutputBytes,
        totalRetainedCommandOutputBytes: this.gmsObservations.totalRetainedCommandOutputBytes,
        totalCollectionMs: this.gmsObservations.totalCollectionMs,
        persistenceAttemptMsThroughFinalization: this.gmsObservations.priorPersistenceAttemptMs,
        temporaryEvidenceCleanup: this.gmsObservations.temporaryEvidenceCleanup,
        persistenceAccounting: 'persistenceAttemptMsThroughFinalization includes all bounded observation-publication attempts through finalization; retirement and temporary cleanup are reported separately',
        persistenceRetirement: this.gmsObservations.persistenceRetirement,
        finalized: this.gmsFinalizationComplete,
        terminalConfirmation: this.gmsPublicationConfirmedSha256 ? 'confirmed' : 'unconfirmed',
        observationSha256: this.gmsPublicationConfirmedSha256,
        errors: this.gmsObservations.errors.length,
      } } : {}),
    };
    try {
      if (this.requireGmsObservationsValue) await this.persistOwnedJson('collector', summary);
      else await this.io.writeJson(this.path('collector'), summary);
      this.collectorSummaryWritten = true;
    } catch (error) {
      this.collectorSummaryFailed = true;
      this.errors.push(error);
      const retired = !(error instanceof BoundedCommandError)
        || (error.result.childExited && error.result.ownedProcessesExited && error.result.stdioClosed);
      if (this.requireGmsObservationsValue && retired) {
        try { await this.invalidateCollectorSummary(); }
        catch (cleanupError) { this.errors.push(cleanupError); }
      }
    }
  }

  finish(): Promise<AndroidMeasurementOutcome> {
    if (this.finished) return Promise.reject(new Error('ANDROID_ENVIRONMENT: measurement already finalized'));
    this.finished = true;
    this.beginAbort?.abort(new Error('ANDROID_ENVIRONMENT: begin was interrupted by finalization'));
    this.finishPromise = this.finishOwned();
    if (this.terminalCompletion) {
      this.finishPromise.then(this.resolveTerminalCompletion, this.rejectTerminalCompletion);
    }
    return this.finishPromise;
  }

  private async finishOwned(): Promise<AndroidMeasurementOutcome> {
    if (this.beginPromise) await this.beginPromise.catch(() => undefined);
    if (this.requireGmsObservationsValue) await this.startGmsFinalization();
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
      if (this.requireGmsObservationsValue) await this.persistOwnedJson('identity', this.identity);
      else await this.io.writeJson(this.path('identity'), this.identity);
    });
    await this.attempt(async () => {
      if (!this.identity) throw new Error('ANDROID_ENVIRONMENT: measurement identity missing');
      const inputs = await readEnvironmentInputs(this.outputDir);
      if (this.collectorSummaryFailed) {
        inputs.collector = '';
        delete inputs.gmsObservations;
      }
      assessment = assessAndroidEnvironment(inputs, this.identity);
      if (this.requireGmsObservationsValue) {
        assessmentSha256 = await this.persistOwnedJson('check', assessment);
        assessment = JSON.parse(`${JSON.stringify(sanitizeValue(assessment), null, 2)}\n`) as AndroidEnvironmentCheck;
      } else {
        await this.io.writeJson(this.path('check'), assessment);
        const bytes = await this.io.readFile(this.path('check'));
        assessmentSha256 = createHash('sha256').update(bytes).digest('hex');
        assessment = JSON.parse(bytes.toString('utf8')) as AndroidEnvironmentCheck;
      }
      if (assessment.collection.status !== 'PASS') throw new Error('ANDROID_ENVIRONMENT: collection guarantee failed');
    });
    const completion = {
      passed: this.errors.length === 0,
      errors: this.errors.map(error => error instanceof Error ? error.message : String(error)),
      assessmentSha256,
    };
    await this.attempt(() => this.requireGmsObservationsValue
      ? this.persistOwnedJson('completion', completion)
      : this.io.writeJson(this.path('completion'), completion));
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

async function runGmsDiagnosticOwnership(serial: string): Promise<void> {
  await requireOwnedDevice('android', serial);
  process.stdout.write('OWNED\n');
}

async function runAndroidMeasurementOwnership(serial: string): Promise<void> {
  await requireOwnedDevice('android', serial);
  process.stdout.write('BEGIN_OK\n');
}

async function runBeginInput(): Promise<Record<string, unknown>> {
  return JSON.parse((await readBoundedStdin(ANDROID_GMS_OBSERVATION_FILE_LIMIT)).toString('utf8')) as Record<string, unknown>;
}

async function runAndroidMeasurementMkdir(): Promise<void> {
  const request = await runBeginInput();
  if (typeof request.path !== 'string') throw new Error('Android measurement directory request is malformed');
  await mkdir(request.path, { recursive: true });
  process.stdout.write('BEGIN_OK\n');
}

async function runAndroidMeasurementSession(): Promise<void> {
  const request = await runBeginInput();
  if (typeof request.path !== 'string' || typeof request.contents !== 'string'
    || basename(request.path) !== 'android-environment-session.json') throw new Error('Android measurement session request is malformed');
  await writeFile(request.path, request.contents, { flag: 'wx', mode: 0o600 });
  process.stdout.write('BEGIN_OK\n');
}

async function runAndroidMeasurementOperations(): Promise<void> {
  const request = await runBeginInput();
  if (typeof request.path !== 'string' || basename(request.path) !== 'android-environment-operations.json'
    || !Array.isArray(request.operations)) throw new Error('Android measurement operations request is malformed');
  await writeSanitizedJson(request.path, request.operations);
  process.stdout.write('BEGIN_OK\n');
}

function checkedTemporaryFile(directory: string, target: string, temporary: string): string {
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

async function runAndroidMeasurementPersistence(mode: 'json' | 'text', stallAfterPublication = false): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(ANDROID_LOG_LIMIT * 2 + 16_384)).toString('utf8')) as {
    directory?: string; target?: string; temporary?: string; contents?: string; expectedBytes?: number; expectedSha256?: string;
  };
  if (!request.directory || !request.target || !request.temporary || typeof request.contents !== 'string'
    || !Number.isSafeInteger(request.expectedBytes) || !/^[a-f0-9]{64}$/u.test(request.expectedSha256 || '')) {
    throw new Error('Android measurement persistence request is malformed');
  }
  const target = resolve(request.target);
  const allowed = new Set([...ANDROID_MEASUREMENT_JSON_FILES, ANDROID_MEASUREMENT_LOG_FILE]);
  if (dirname(target) !== resolve(request.directory) || !allowed.has(basename(target))) {
    throw new Error('Android measurement persistence target is invalid');
  }
  const temporary = checkedTemporaryFile(request.directory, target, request.temporary);
  const expected = Buffer.from(request.contents);
  const expectedSha256 = createHash('sha256').update(expected).digest('hex');
  const maximum = basename(target) === ANDROID_MEASUREMENT_LOG_FILE ? ANDROID_LOG_LIMIT : ANDROID_GMS_OBSERVATION_FILE_LIMIT;
  if (expected.byteLength !== request.expectedBytes || expectedSha256 !== request.expectedSha256 || expected.byteLength > maximum) {
    throw new Error('Android measurement persistence digest is inconsistent');
  }
  if (mode === 'json' && `${JSON.stringify(sanitizeValue(JSON.parse(request.contents)), null, 2)}\n` !== request.contents) {
    throw new Error('Android measurement JSON is not canonical sanitized evidence');
  }
  if (mode === 'text' && redactText(request.contents) !== request.contents) {
    throw new Error('Android measurement log is not sanitized');
  }
  await mkdir(resolve(request.directory), { recursive: true });
  await writeFile(temporary, expected, { flag: 'wx', mode: 0o600 });
  const staged = await readFile(temporary);
  if (!staged.equals(expected)) throw new Error('Android measurement staged read-back is inconsistent');
  renameSync(temporary, target);
  if (stallAfterPublication) {
    process.on('SIGTERM', () => undefined);
    setInterval(() => undefined, 1_000);
    return;
  }
  process.stdout.write(`${JSON.stringify({ byteLength: staged.byteLength, sha256: expectedSha256 })}\n`);
}

async function runAndroidMeasurementInvalidateCollector(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(4_096)).toString('utf8')) as { directory?: string; target?: string };
  if (!request.directory || !request.target || dirname(resolve(request.target)) !== resolve(request.directory)
    || basename(resolve(request.target)) !== 'android-environment-collector.json') {
    throw new Error('Android collector invalidation request is malformed');
  }
  rmSync(resolve(request.target), { force: true });
  process.stdout.write(`${JSON.stringify({ removed: !existsSync(resolve(request.target)) })}\n`);
}

async function runAndroidMeasurementCleanup(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(4_096)).toString('utf8')) as {
    directory?: string; target?: string; temporary?: string;
  };
  if (!request.directory || !request.target || !request.temporary) throw new Error('Android measurement cleanup request is malformed');
  const temporary = checkedTemporaryFile(request.directory, request.target, request.temporary);
  rmSync(temporary, { force: true });
  process.stdout.write('{"removed":true}\n');
}

async function runGmsDiagnosticPersistence(stallAfterPublication = false): Promise<void> {
  const progress = (step: string, detail = '') => {
    process.stderr.write(`GMS_WRITER_PROGRESS ${step} ${Math.round(process.uptime() * 1_000)}${detail}\n`);
  };
  progress('ready');
  const input = await readBoundedStdin(ANDROID_GMS_OBSERVATION_FILE_LIMIT * 2);
  progress('input', ` ${input.byteLength}`);
  const request = JSON.parse(input.toString('utf8')) as {
    directory?: string; temporary?: string; target?: string; snapshot?: AndroidGmsObservations; expectedBytes?: number; expectedSha256?: string;
  };
  if (!request.directory || !request.snapshot || !request.temporary || !request.target || !Number.isSafeInteger(request.expectedBytes)
    || !/^[a-f0-9]{64}$/u.test(request.expectedSha256 || '') || basename(resolve(request.target)) !== ANDROID_GMS_OBSERVATION_FILE
    || dirname(resolve(request.target)) !== resolve(request.directory)) {
    throw new Error('GMS persistence request is malformed');
  }
  const target = resolve(request.target);
  const temporary = checkedTemporaryFile(request.directory, target, request.temporary);
  const expected = `${JSON.stringify(sanitizeValue(request.snapshot), null, 2)}\n`;
  const expectedBytes = Buffer.byteLength(expected);
  const expectedSha256 = createHash('sha256').update(expected).digest('hex');
  if (expectedBytes !== request.expectedBytes || expectedSha256 !== request.expectedSha256
    || expectedBytes > ANDROID_GMS_OBSERVATION_FILE_LIMIT) throw new Error('GMS persistence request digest is inconsistent');
  await mkdir(dirname(target), { recursive: true });
  progress('directory');
  await writeFile(temporary, expected, { flag: 'wx', mode: 0o600 });
  progress('staged');
  const staged = await readFile(temporary);
  if (staged.byteLength !== expectedBytes || staged.toString('utf8') !== expected) {
    throw new Error('GMS persistence read-back does not match sanitized bytes');
  }
  renameSync(temporary, target);
  if (stallAfterPublication) {
    process.on('SIGTERM', () => undefined);
    setInterval(() => undefined, 1_000);
    return;
  }
  process.stdout.write(`${JSON.stringify({ byteLength: staged.byteLength, sha256: expectedSha256 })}\n`);
}

async function runGmsDiagnosticCleanup(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(4_096)).toString('utf8')) as {
    directory?: string; temporary?: string; target?: string;
  };
  if (!request.directory || !request.target || !request.temporary || basename(resolve(request.target)) !== ANDROID_GMS_OBSERVATION_FILE
    || dirname(resolve(request.target)) !== resolve(request.directory)) {
    throw new Error('GMS cleanup request is malformed');
  }
  const temporary = checkedTemporaryFile(request.directory, request.target, request.temporary);
  rmSync(temporary, { force: true });
  process.stdout.write('{"removed":true}\n');
}

async function runAndroidTransportObservation(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(ANDROID_LOG_LIMIT * 2 + 16_384)).toString('utf8')) as {
    serial?: string; outputDir?: string; log?: string;
  };
  if (!request.serial || !request.outputDir || typeof request.log !== 'string' || Buffer.byteLength(request.log) > ANDROID_LOG_LIMIT) {
    throw new Error('Android transport observation request is malformed');
  }
  const transport = new AndroidTransportObservation(request.serial, request.outputDir);
  transport.observe(Buffer.from(request.log));
  await transport.finish();
  process.stdout.write(`${JSON.stringify({ failure: transport.failure })}\n`);
}

async function runAndroidTransportObservationTrigger(): Promise<void> {
  const request = JSON.parse((await readBoundedStdin(4_096)).toString('utf8')) as {
    serial?: string; outputDir?: string; guestEpochSeconds?: string; triggeredAt?: string; anchorGuestEpochSeconds?: string;
    guestClockSample?: { guestEpochSeconds?: string; startedAt?: string; endedAt?: string }; fixture?: AndroidTransportTestFixture;
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
  const transport = new AndroidTransportObservation(request.serial, request.outputDir, undefined, undefined, request.fixture);
  await transport.observeTrigger({ guestEpochSeconds: guestEpochSeconds!, triggeredAt,
    ...(request.anchorGuestEpochSeconds ? { anchorGuestEpochSeconds: request.anchorGuestEpochSeconds } : {}),
    ...(guestClockSample ? { guestClockSample } : {}) });
  process.stdout.write(`${JSON.stringify({ failure: transport.failure })}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  const modes: Record<string, () => Promise<void>> = {
    '--android-measurement-owner': () => runAndroidMeasurementOwnership(process.argv[3] || ''),
    '--android-measurement-mkdir': runAndroidMeasurementMkdir,
    '--android-measurement-session': runAndroidMeasurementSession,
    '--android-measurement-operations': runAndroidMeasurementOperations,
    '--android-measurement-persist-json': () => runAndroidMeasurementPersistence('json'),
    '--android-measurement-persist-json-stall-after-publication': () => runAndroidMeasurementPersistence('json', true),
    '--android-measurement-persist-text': () => runAndroidMeasurementPersistence('text'),
    '--android-measurement-invalidate-collector': runAndroidMeasurementInvalidateCollector,
    '--android-measurement-cleanup-file': runAndroidMeasurementCleanup,
    '--android-transport-observation': runAndroidTransportObservation,
    '--android-transport-observation-trigger': runAndroidTransportObservationTrigger,
    '--gms-diagnostic-ownership': () => runGmsDiagnosticOwnership(process.argv[3] || ''),
    '--gms-diagnostic-persist': runGmsDiagnosticPersistence,
    '--gms-diagnostic-persist-stall-after-publication': () => runGmsDiagnosticPersistence(true),
    '--gms-diagnostic-cleanup': runGmsDiagnosticCleanup,
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

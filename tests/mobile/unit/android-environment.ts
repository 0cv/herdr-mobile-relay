import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { constants as osConstants } from 'node:os';
import { closeSync, createWriteStream, existsSync, openSync, readdirSync, readFileSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { Readable } from 'node:stream';
import { createServer as createNetServer, type Server, type Socket } from 'node:net';
import { repositoryPath, repositoryRoot } from '../support/paths';
import { AndroidEnvironmentMeasurement as Measurement, type AndroidMeasurementIO } from '../android-measurement';
import { BoundedCommandError, boundedCommand } from '../support/bounded-process';
import { monotonicNowNs, processGroupAbsent, spawnOwnedProcess, type OwnedProcessHandle } from '../support/owned-process';
import { PhaseBudget } from '../support/budget';
import { measurementIdentity } from './android-product-environment';
import { transportFixtureIO, type AndroidTransportFixture } from './android-measurement-doubles';

export class AndroidEnvironmentMeasurement extends Measurement {
  constructor(serial: string, output: string, toolchains: string, io: Partial<AndroidMeasurementIO> & { transportFixture?: AndroidTransportFixture } = {},
    lifetime = new PhaseBudget('android-measurement-unit', { timeoutMs: 30 * 60_000 })) {
    super(serial, output, toolchains, io.transportFixture ? transportFixtureIO(io.transportFixture, io) : io, lifetime);
    this.bind(measurementIdentity({ measurementId: this.id }));
  }
}

export async function beginObservedMeasurement(measurement: Measurement): Promise<void> {
  const budget = new PhaseBudget('measurement-stages', { timeoutMs: 60_000 });
  await measurement.observeGmsStage('after-device-start', budget);
  await measurement.observeGmsStage('before-measurement-begin', budget);
  await measurement.begin();
  await measurement.observeGmsStage('after-setup-url', budget);
  await measurement.observeGmsStage('after-browser-install', budget);
  await measurement.observeGmsStage('after-initial-installed-app-launch', budget);
}

interface GroupPollTestTimer {
  callback: () => void;
  cancelled: boolean;
  delivered: boolean;
  delayMs: number;
}

export function groupPollTestTimers() {
  const timers: GroupPollTestTimer[] = [];
  return {
    timers,
    schedule(callback: () => void, delayMs: number): ReturnType<typeof setTimeout> {
      const timer = { callback, cancelled: false, delivered: false, delayMs };
      timers.push(timer);
      return timer as unknown as ReturnType<typeof setTimeout>;
    },
    cancel(handle: ReturnType<typeof setTimeout>): void {
      (handle as unknown as GroupPollTestTimer).cancelled = true;
    },
    deliver(timer: GroupPollTestTimer, includeCancelled = false): void {
      assert.equal(timer.delivered, false);
      assert.ok(!timer.cancelled || includeCancelled);
      timer.delivered = true;
      timer.callback();
    },
    pending(): GroupPollTestTimer[] {
      return timers.filter(timer => !timer.cancelled && !timer.delivered);
    },
  };
}


interface Fixture {
  root: string;
  fixtureDirectory: string;
  log: string;
  diagnostic: string;
  environment: NodeJS.ProcessEnv;
}

export function observerRecords(path: string): Array<Record<string, unknown>> {
  return readFileSync(path, 'utf8').split('\n').filter(Boolean).map(line => JSON.parse(line) as Record<string, unknown>);
}

export function createBoundedSupervisorObserver(root: string, name: string) {
  const path = join(root, `${name}.jsonl`);
  const fd = openSync(path, 'wx', 0o600);
  const stream = createWriteStream('', { fd, autoClose: false });
  let closed = false;
  return {
    stream,
    async close(): Promise<void> {
      if (closed) return;
      closed = true;
      await new Promise<void>(resolve => stream.end(resolve));
      closeSync(fd);
    },
    records(): Array<Record<string, unknown>> {
      return existsSync(path) ? observerRecords(path) : [];
    },
  };
}

export function boundedCommandFailureDetails(result: BoundedCommandError['result'] | undefined,
  records: Array<Record<string, unknown>> = [], timeline = false): Record<string, unknown> {
  const retirementRecord = [...records].reverse().find(record =>
    record.type === 'supervisor-retirement-ready' || record.type === 'supervisor-retirement-unproved');
  const evidence = retirementRecord?.evidence && typeof retirementRecord.evidence === 'object'
    ? retirementRecord.evidence as Record<string, unknown> : undefined;
  const evidenceFields = ['targetExitObserved', 'targetCloseObserved', 'targetPIDObserved', 'anchorExitObserved',
    'anchorCloseObserved', 'groupAbsent', 'inputClosedObserved', 'stdoutNaturalEnd', 'stdoutCloseObserved',
    'stderrNaturalEnd', 'stderrCloseObserved', 'targetStdoutNaturalEnd', 'targetStdoutCloseObserved',
    'targetStderrNaturalEnd', 'targetStderrCloseObserved', 'managerStdoutNaturalEnd', 'managerStdoutCloseObserved',
    'managerStderrNaturalEnd', 'managerStderrCloseObserved', 'supervisorStdoutFinished', 'supervisorStdoutCloseObserved',
    'supervisorStderrFinished', 'supervisorStderrCloseObserved', 'managerProcessCreated', 'targetDispatchRequested',
    'targetProcessCreated', 'targetNoChildObserved', 'targetExecConfirmed', 'targetUnconfirmedCloseObserved',
    'targetOutputRelayHealthy', 'stopReason'];
  const safeEvidence = evidence && Object.fromEntries(evidenceFields.filter(key => key in evidence).map(key => [key, evidence[key]]));
  const missingRetirementWitnesses: string[] = [];
  const requireTrue = (key: string) => { if (evidence?.[key] !== true) missingRetirementWitnesses.push(key); };
  if (evidence) {
    for (const key of ['anchorExitObserved', 'anchorCloseObserved', 'groupAbsent', 'inputClosedObserved',
      'supervisorStdoutFinished', 'supervisorStdoutCloseObserved', 'supervisorStderrFinished', 'supervisorStderrCloseObserved']) requireTrue(key);
    if (evidence.managerProcessCreated === true || evidence.targetDispatchRequested === true
      || evidence.targetProcessCreated === true || evidence.targetNoChildObserved === true) {
      for (const key of ['managerStdoutNaturalEnd', 'managerStdoutCloseObserved', 'managerStderrNaturalEnd', 'managerStderrCloseObserved']) requireTrue(key);
    }
    if (evidence.targetDispatchRequested === true) {
      if (evidence.targetNoChildObserved === true) {
        if (evidence.targetProcessCreated === true) missingRetirementWitnesses.push('targetProcessCreated=false after targetNoChildObserved');
      } else if (evidence.targetProcessCreated === true && evidence.targetPIDObserved === true) {
        for (const key of ['targetExecConfirmed', 'targetExitObserved', 'targetCloseObserved', 'targetStdoutNaturalEnd',
          'targetStdoutCloseObserved', 'targetStderrNaturalEnd', 'targetStderrCloseObserved', 'targetOutputRelayHealthy']) requireTrue(key);
      } else if (evidence.targetProcessCreated === true) requireTrue('targetUnconfirmedCloseObserved');
      else missingRetirementWitnesses.push('targetNoChildObserved or targetProcessCreated');
    } else if (evidence.targetProcessCreated === true || evidence.targetNoChildObserved === true) {
      missingRetirementWitnesses.push('targetDispatchRequested before target process outcome');
    }
  } else if (records.length) missingRetirementWitnesses.push('supervisor-retirement-ready or supervisor-retirement-unproved evidence');
  if (result && !result.ownedProcessesExited) missingRetirementWitnesses.push('ownedProcessesExited (supervisor and process-group retirement projection)');
  if (result && !result.stdioClosed) missingRetirementWitnesses.push('stdioClosed');
  if (evidence?.targetPIDObserved === true && result) {
    if (!result.childExited) missingRetirementWitnesses.push('childExited');
    if (!result.targetCloseObserved) missingRetirementWitnesses.push('targetCloseObserved');
  }
  if (result) {
    for (const key of ['callerStdoutNaturalEnd', 'callerStdoutCloseObserved', 'callerStderrNaturalEnd', 'callerStderrCloseObserved'] as const) {
      if (result[key] !== true) missingRetirementWitnesses.push(key);
    }
  }
  const observedTypes = new Set(['supervisor-runtime-started', 'supervisor-deadlines-armed', 'supervisor-progress',
    'supervisor-stopping', 'supervisor-execution-deadline', 'supervisor-hard-deadline', 'anchor-process-started',
    'anchor-clock-handshake', 'supervisor-target-process-started', 'supervisor-target-process-unconfirmed',
    'supervisor-target-no-child', 'supervisor-target-started', 'supervisor-target-exit', 'supervisor-target-close',
    'supervisor-anchor-close', 'supervisor-group-signal-refused', 'supervisor-retirement-ready', 'supervisor-retirement-unproved',
    'supervisor-input-observation', 'supervisor-manager-input-observation', 'supervisor-teardown-started', 'supervisor-observation-ended',
    ...(timeline ? ['supervisor-target-source-end', 'supervisor-target-source-close'] : [])]);
  const observedRecords = records.filter(record => observedTypes.has(String(record.type)));
  const observerSupervisor = observedRecords.slice(-40).map(record => {
    const fields = ['type', 'clockNs', 'executionDeadlineNs', 'hardDeadlineNs', 'stage', 'reason', 'targetPID', 'targetProcessPID', 'processID',
      'groupID', 'anchorPID', 'signal', 'targetSpawned', 'targetExecConfirmed', 'targetExit', 'targetClose', 'anchorClosed',
      'groupAbsent', 'inputClosed', 'evidence', 'event', 'summary', 'input', 'managerInput', 'unmetRetirementPrerequisites',
      'observerDelivery', 'observerDroppedRecords', 'observerDroppedRecordBytes', 'delivery', 'complete', 'droppedRecords', 'droppedRecordBytes',
      ...(timeline ? ['stream', 'naturalEnd', 'anchorClockNs', 'supervisorClockNs'] : [])];
    return Object.fromEntries(fields.filter(key => key in record).map(key => [key,
      key === 'evidence' && record[key] && typeof record[key] === 'object'
        ? Object.fromEntries(evidenceFields.filter(evidenceKey => evidenceKey in (record[key] as Record<string, unknown>))
          .map(evidenceKey => [evidenceKey, (record[key] as Record<string, unknown>)[evidenceKey]]))
        : record[key]]));
  });
  return {
    result: result ? {
      timedOut: result.timedOut, aborted: result.aborted, outputLimitExceeded: result.outputLimitExceeded,
      code: result.code, signal: result.signal ?? null, launchError: Boolean(result.launchError), durationMs: result.durationMs,
      stdoutBytes: result.stdoutBytes, stderrBytes: result.stderrBytes,
      stdoutRetainedBytes: result.stdoutRetainedBytes, stderrRetainedBytes: result.stderrRetainedBytes,
      targetPID: result.targetPID ?? null, targetCloseObserved: result.targetCloseObserved, childExited: result.childExited,
      ownedProcessesExited: result.ownedProcessesExited, stdioClosed: result.stdioClosed,
      targetStdoutNaturalEnd: result.targetStdoutNaturalEnd, targetStdoutCloseObserved: result.targetStdoutCloseObserved,
      targetStderrNaturalEnd: result.targetStderrNaturalEnd, targetStderrCloseObserved: result.targetStderrCloseObserved,
      managerStdoutNaturalEnd: result.managerStdoutNaturalEnd, managerStdoutCloseObserved: result.managerStdoutCloseObserved,
      managerStderrNaturalEnd: result.managerStderrNaturalEnd, managerStderrCloseObserved: result.managerStderrCloseObserved,
      callerStdoutNaturalEnd: result.callerStdoutNaturalEnd, callerStdoutCloseObserved: result.callerStdoutCloseObserved,
      callerStderrNaturalEnd: result.callerStderrNaturalEnd, callerStderrCloseObserved: result.callerStderrCloseObserved,
      targetOutputRelayHealthy: result.targetOutputRelayHealthy,
    } : undefined,
    retirementEvidence: safeEvidence,
    observerTraceComplete: records.at(-1)?.type === 'supervisor-observation-ended' && records.at(-1)?.complete === true,
    missingRetirementWitnesses,
    observerSupervisor,
    ...(timeline ? { observerRecordsSeen: observedRecords.length, observerRecordsOmittedByCap: Math.max(0, observedRecords.length - 40) } : {}),
  };
}

export function gmsAttackTimingRule(limits: unknown, stage: unknown, elapsedMs: unknown): {
  violations: string[]; stageAllowanceMs?: number; compositeCeilingMs?: number;
} {
  const declared = limits && typeof limits === 'object' ? limits as Record<string, unknown> : {};
  const measured = stage && typeof stage === 'object' ? stage as Record<string, unknown> : {};
  const violations: string[] = [];
  for (const name of ['ownershipTimeoutMs', 'commandTimeoutMs', 'parseReserveMs', 'persistenceOperationTimeoutMs']) {
    if (!Number.isSafeInteger(declared[name]) || Number(declared[name]) <= 0) violations.push(`declared ${name} is missing or invalid`);
  }
  for (const name of ['parseDurationMs', 'durationMs']) {
    if (typeof measured[name] !== 'number' || !Number.isFinite(measured[name]) || measured[name] <= 0) {
      violations.push(`measured ${name} is missing or invalid`);
    }
  }
  if (typeof elapsedMs !== 'number' || !Number.isFinite(elapsedMs) || elapsedMs <= 0) violations.push('measured elapsedMs is missing or invalid');
  if (violations.length) return { violations };
  const parseReserveMs = Number(declared.parseReserveMs);
  const stageAllowanceMs = Number(declared.ownershipTimeoutMs) + Number(declared.commandTimeoutMs) + parseReserveMs;
  const compositeCeilingMs = stageAllowanceMs + 2 * Number(declared.persistenceOperationTimeoutMs);
  const parseDurationMs = Number(measured.parseDurationMs);
  const durationMs = Number(measured.durationMs);
  const elapsed = Number(elapsedMs);
  if (parseDurationMs > parseReserveMs) violations.push(`normalization took ${parseDurationMs} ms, beyond its ${parseReserveMs} ms parse reserve`);
  if (durationMs > stageAllowanceMs) violations.push(`collection took ${durationMs} ms, beyond its ${stageAllowanceMs} ms stage allowance`);
  if (elapsed < durationMs) violations.push(`elapsed ${elapsed} ms is shorter than the ${durationMs} ms collection it contains`);
  if (elapsed > compositeCeilingMs) violations.push(`collection and both persistence operations took ${elapsed} ms, beyond ${compositeCeilingMs} ms`);
  return { violations, stageAllowanceMs, compositeCeilingMs };
}

export function gmsTimeoutProbeBudgetMs(limits: { persistenceOperationTimeoutMs: number; ownershipTimeoutMs: number; parseReserveMs: number }): number {
  return limits.persistenceOperationTimeoutMs + limits.ownershipTimeoutMs + limits.parseReserveMs;
}

const GMS_GATED_ENTRY_CEILING_MS = 20_000;

interface GatedEntryTimers {
  schedule(callback: () => void, delayMs: number): ReturnType<typeof setTimeout>;
  cancel(handle: ReturnType<typeof setTimeout>): void;
}

const realGatedEntryTimers: GatedEntryTimers = {
  schedule: (callback, delayMs) => setTimeout(callback, delayMs),
  cancel: handle => clearTimeout(handle),
};

export function settledResult<T>(promise: Promise<T>): Promise<PromiseSettledResult<T>> {
  return Promise.allSettled([promise]).then(([result]) => result!);
}

export function internalGmsFinalization(measurement: Measurement): Promise<void> {
  const finalization = (measurement as unknown as { gms: { finalizationPromise?: Promise<void> } }).gms.finalizationPromise;
  assert.ok(finalization, 'GMS finalization was not started');
  return finalization;
}

export function awaitGatedEntry(entered: Promise<void>, settlement: Promise<unknown>, gate: string, operation: string,
  details: () => unknown, timers: GatedEntryTimers = realGatedEntryTimers, ceilingMs = GMS_GATED_ENTRY_CEILING_MS): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let open = true;
    const close = (failure?: string) => {
      if (!open) return;
      open = false;
      timers.cancel(watchdog);
      if (failure) reject(new assert.AssertionError({ message: `${failure}: ${JSON.stringify(details())}` }));
      else resolve();
    };
    const watchdog = timers.schedule(() => close(`${gate} was not entered within ${ceilingMs} ms`), ceilingMs);
    entered.then(() => close());
    settlement.then(() => close(`${operation} settled before ${gate} was entered`),
      error => close(`${operation} failed before ${gate} was entered (${error instanceof Error ? error.message : String(error)})`));
  });
}

export async function gmsStagePersistencePrecondition(root: string, stages: readonly string[],
  measurement: { failureOutcome(): { errors: unknown[] } }): Promise<{ violations: string[]; measurementErrors: string[] }> {
  const measurementErrors = measurement.failureOutcome().errors.map(error => String(error));
  const violations: string[] = [];
  let observation: { stages?: Array<{ stage?: unknown; outcome?: unknown; error?: unknown }>; errors?: unknown } | undefined;
  try {
    observation = JSON.parse(await readFile(join(root, 'android-environment-gms-observations.json'), 'utf8'));
  } catch (error) {
    violations.push(`GMS observations are missing or unreadable: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (observation) {
    const persisted = Array.isArray(observation.stages) ? observation.stages : [];
    const names = persisted.map(entry => entry?.stage);
    if (JSON.stringify(names) !== JSON.stringify(stages)) violations.push(`persisted stages ${JSON.stringify(names)} are not ${JSON.stringify(stages)}`);
    for (const entry of persisted) {
      if (entry?.outcome !== 'collected') violations.push(`stage ${String(entry?.stage)} is ${String(entry?.outcome)}: ${String(entry?.error ?? '')}`);
    }
    if (!Array.isArray(observation.errors) || observation.errors.length) violations.push(`GMS observation errors: ${JSON.stringify(observation.errors)}`);
  }
  for (const error of measurementErrors) violations.push(`measurement error: ${error}`);
  return { violations, measurementErrors };
}

export function gmsWriterDiagnostics(root: string) {
  const failures: Array<Record<string, unknown>> = [];
  const observers: Array<ReturnType<typeof createBoundedSupervisorObserver>> = [];
  return {
    failures,
    async run(binary: string, args: string[], timeout: number, options: Parameters<typeof boundedCommand>[3]) {
      const observer = createBoundedSupervisorObserver(root, `gms-writer-${observers.length + 1}-supervisor`);
      observers.push(observer);
      const writer = observers.length;
      try {
        return await boundedCommand(binary, args, timeout, { ...options, observerSupervisor: observer.stream });
      } catch (error) {
        if (error instanceof BoundedCommandError) {
          const finalized = String(options.input || '').includes('"finalized":true');
          try { failures.push({ writer, finalized, inputBytes: Buffer.byteLength(options.input || ''),
            ...boundedCommandFailureDetails(error.result, observer.records(), true) }); }
          catch (detailError) { failures.push({ writer, finalized, detailError: String(detailError) }); }
        }
        throw error;
      }
    },
    async close() {
      for (const observer of observers) await observer.close();
    },
  };
}

export function eventLoopStallWindow(input: { commandStartedMs: unknown; timeoutMs: unknown; cleanupMs: unknown; markerMs: unknown; stallEndedMs?: unknown }): {
  violation?: string; executionDeadlineMs?: number; hardDeadlineMs?: number; stallUntilMs?: number; drainAllowanceMs?: number;
} {
  const { commandStartedMs, timeoutMs, cleanupMs, markerMs, stallEndedMs } = input;
  const values = [commandStartedMs, timeoutMs, cleanupMs, markerMs, ...('stallEndedMs' in input ? [stallEndedMs] : [])];
  if (values.some(value => typeof value !== 'number' || !Number.isFinite(value))) return { violation: 'stall window inputs must be finite numbers' };
  const start = commandStartedMs as number;
  const timeout = timeoutMs as number;
  const cleanup = cleanupMs as number;
  const marker = markerMs as number;
  if (timeout <= 0 || cleanup <= 0 || cleanup >= timeout) return { violation: `cleanup reservation ${cleanup} ms must be positive and shorter than the ${timeout} ms command` };
  if (marker < start) return { violation: 'child marker precedes the command start' };
  const executionDeadlineMs = start + timeout - cleanup;
  const hardDeadlineMs = start + timeout;
  const stallUntilMs = executionDeadlineMs + cleanup / 4;
  const drainAllowanceMs = hardDeadlineMs - stallUntilMs;
  const window = { executionDeadlineMs, hardDeadlineMs, stallUntilMs, drainAllowanceMs };
  if (marker >= executionDeadlineMs) {
    return { ...window, violation: `child marker arrived ${marker - start} ms after the command started, at or after its ${timeout - cleanup} ms execution deadline; the stall cannot cross it` };
  }
  if (!('stallEndedMs' in input)) return window;
  const ended = stallEndedMs as number;
  if (ended < stallUntilMs) return { ...window, violation: `stall ended ${ended - start} ms after the command started, before crossing to ${stallUntilMs - start} ms` };
  if (ended > hardDeadlineMs - cleanup / 2) {
    return { ...window, violation: `stall ended ${ended - start} ms after the command started, leaving less than half of the ${cleanup} ms cleanup reservation to drain before ${timeout} ms` };
  }
  return window;
}

interface AndroidTransportTestEndpoint {
  server: Server;
  sockets: Set<Socket>;
  port: number;
  requests: string[];
  completed: string[];
  connected: number;
  closed: number;
  stop(): Promise<void>;
}

export async function createAndroidTransportTestEndpoint(unresponsive = false,
  replies: { stateFrame?: Buffer; uptimeFrame?: Buffer; clockFrame?: Buffer } = {}): Promise<AndroidTransportTestEndpoint> {
  const requests: string[] = [];
  const completed: string[] = [];
  const sockets = new Set<Socket>();
  let connected = 0;
  let closed = 0;
  const response = (value: string) => Buffer.concat([Buffer.from('OKAY'), Buffer.from(value.length.toString(16).padStart(4, '0')), Buffer.from(value)]);
  const server = createNetServer(socket => {
    sockets.add(socket);
    connected++;
    let pending = Buffer.alloc(0);
    let acquisition = '';
    socket.on('error', () => undefined);
    socket.on('data', chunk => {
      pending = Buffer.concat([pending, Buffer.from(chunk)]);
      while (pending.length >= 4) {
        const length = Number.parseInt(pending.subarray(0, 4).toString(), 16);
        if (!Number.isSafeInteger(length) || pending.length < length + 4) return;
        const request = pending.subarray(4, length + 4).toString();
        pending = pending.subarray(length + 4);
        requests.push(request);
        if (request === 'host-serial:emulator-5554:get-state') {
          acquisition = 'adb-state';
          if (unresponsive) continue;
          if (replies.stateFrame) socket.end(replies.stateFrame);
          else socket.write(response('device'));
        } else if (request === 'host:transport:emulator-5554') {
          acquisition = 'guest-uptime';
          socket.write('OKAY');
        } else if (request === 'shell:date +%s.%N') {
          acquisition = 'guest-clock';
          const now = Date.now();
          const guestClock = `${Math.floor(now / 1000)}.${String(now % 1000).padStart(3, '0')}000000\n`;
          socket.end(replies.clockFrame ?? Buffer.concat([Buffer.from('OKAY'), Buffer.from(guestClock)]));
        } else if (request === 'shell:echo transport-observation; cat /proc/uptime') {
          if (unresponsive) continue;
          socket.end(replies.uptimeFrame ?? Buffer.concat([Buffer.from('OKAY'), Buffer.from('1234.56 7890.12\n')]));
        } else socket.end('FAIL0000');
      }
    });
    socket.once('close', () => {
      sockets.delete(socket);
      closed++;
      if (acquisition) completed.push(acquisition);
    });
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  return {
    server, sockets, port: address.port, requests, completed,
    get connected() { return connected; },
    get closed() { return closed; },
    async stop() {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}

export async function createAndroidTransportFixture(root: string, adbPort: number, state: string): Promise<AndroidTransportFixture> {
  const fixture: AndroidTransportFixture = {
    adbPort,
    hostCommandScript: join(root, 'transport-host-command.sh'),
    stateFile: join(root, 'transport-state'),
    acquisitionLogFile: join(root, 'transport-acquisitions'),
    commandPidFile: join(root, 'transport-command.pid'),
    descendantPidFile: join(root, 'transport-descendant.pid'),
    lateWriteFile: join(root, 'transport-late-write'),
  };
  const script = `set -eu
state_file=$1
acquisition_file=$2
command_pid_file=$3
descendant_pid_file=$4
late_write_file=$5
command_name=$6
state=$(cat "$state_file")
printf '%s\\n' "$command_name" >> "$acquisition_file"
if { [ "$state" = block-processes ] && [ "$command_name" = host-processes ]; } || { [ "$state" = timeout-memory ] && [ "$command_name" = host-memory ]; }; then
  printf '%s\\n' "$$" > "$command_pid_file"
  /bin/sh -c 'printf "%s\\n" "$$" > "$1"; trap "" TERM; sleep 10; printf late > "$2"; while :; do sleep 1; done' sh "$descendant_pid_file" "$late_write_file" &
  wait "$!"
fi
if [ "$state" = nonzero-tcp ] && [ "$command_name" = host-tcp ]; then
  printf 'synthetic host acquisition failure\\n' >&2
  exit 7
fi
case "$command_name" in
  host-processes) printf '%s\\n' "$state" ;;
  host-memory) printf 'MemAvailable: %s\\n' "$state" ;;
  host-tcp) printf 'State: %s\\n' "$state" ;;
  *) exit 2 ;;
esac
`;
  await writeFile(fixture.hostCommandScript, script, { mode: 0o600 });
  await writeFile(fixture.stateFile, state);
  return fixture;
}

export async function installFakeAdbLifetimeBackstop(fixture: Fixture): Promise<NodeJS.ProcessEnv> {
  const directory = join(fixture.root, 'lifetime-backstop-bin');
  await mkdir(directory, { recursive: true });
  const fakeAdb = join(fixture.environment.PATH!.split(':')[0], 'adb');
  const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
  const watchdog = `${quote(process.execPath)} --no-env-file -e 'const group=Number(process.argv.at(-1));setTimeout(()=>{try{process.kill(-group,"SIGKILL")}catch{}},12000)' "$group" >/dev/null 2>&1 &`;
  const wrapper = `#!/bin/sh\ngroup=$(ps -o pgid= -p "$$" | tr -d '[:space:]')\ncase "$group" in ''|*[!0-9]*) exit 127;; esac\n${watchdog}\nexec ${quote(fakeAdb)} "$@"\n`;
  await writeFile(join(directory, 'adb'), wrapper, { mode: 0o700 });
  return { ...fixture.environment, PATH: `${directory}:${fixture.environment.PATH}` };
}

export async function verifyFakeAdbLifetimeBackstop(environment: NodeJS.ProcessEnv, fixture: Fixture): Promise<void> {
  const child = spawn('adb', ['-s', 'emulator-5554', 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1'], {
    detached: true, stdio: 'ignore', env: environment,
  });
  assert.ok(child.pid);
  const groupID = child.pid;
  const closed = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>(resolve => {
    child.once('close', (code, signal) => resolve({ code, signal }));
  });
  try {
    await waitForValue(() => {
      try { return readFileSync(fixture.log, 'utf8').includes('logcat -b main -b system -v epoch -T 1') ? true : undefined; }
      catch { return undefined; }
    }, 3_000, 'independently protected fake adb did not enter its logcat handler');
    let timer: ReturnType<typeof setTimeout> | undefined;
    const result = await Promise.race([closed, new Promise<undefined>(resolve => { timer = setTimeout(() => resolve(undefined), 14_000); })]);
    if (timer) clearTimeout(timer);
    assert.ok(result, 'independent fake-adb watchdog did not expire its isolated synthetic process group');
    assert.equal(result.signal, 'SIGKILL');
    assert.equal(await waitForPidAbsent(groupID, 1_000), true);
    assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
  } finally {
    if (child.exitCode === null && child.signalCode === null) {
      try { process.kill(-groupID, 'SIGKILL'); }
      catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
      await Promise.race([closed, new Promise(resolve => setTimeout(resolve, 2_000))]);
    }
    assert.equal(await waitForGroupAbsent(groupID, 2_000), true);
  }
}

export type FakeAdbDiagnosticStage = 'fixture-create' | 'fake-child-launch' | 'fake-child-exit' | 'log-producer-append';
export type FakeAdbExitCategory = 'success' | 'nonzero' | 'signal' | 'launch-error';
export type FakeAdbFixturePathCategory = 'expected-owned-path' | 'absent' | 'mismatched';
export interface FakeAdbDiagnosticEvent {
  stage: FakeAdbDiagnosticStage;
  logExists: boolean;
  fixturePathCategory: FakeAdbFixturePathCategory;
  childExitCategory?: FakeAdbExitCategory;
}
export interface FakeAdbDiagnostic {
  schema: 1;
  fixtureCreate: FakeAdbDiagnosticEvent;
  children: Array<{ events: FakeAdbDiagnosticEvent[] }>;
}

const fakeAdbDiagnosticEventKeys = new Set(['stage', 'logExists', 'fixturePathCategory', 'childExitCategory']);
const fakeAdbDiagnosticStages: FakeAdbDiagnosticStage[] = ['fake-child-launch', 'log-producer-append', 'fake-child-exit'];
const fakeAdbDiagnosticOrder = new Map(fakeAdbDiagnosticStages.map((stage, index) => [stage, index]));

function invalidFakeAdbDiagnostic(): never {
  throw new Error('FAKE_ADB_DIAGNOSTIC: required fixture, child, append, and exit witnesses missing');
}

function isFakeAdbDiagnosticStage(value: unknown): value is FakeAdbDiagnosticStage {
  return value === 'fixture-create' || value === 'fake-child-launch' || value === 'fake-child-exit' || value === 'log-producer-append';
}

function isFakeAdbExitCategory(value: unknown): value is FakeAdbExitCategory {
  return value === 'success' || value === 'nonzero' || value === 'signal' || value === 'launch-error';
}

function isFakeAdbFixturePathCategory(value: unknown): value is FakeAdbFixturePathCategory {
  return value === 'expected-owned-path' || value === 'absent' || value === 'mismatched';
}

function validateFakeAdbDiagnosticEvent(value: unknown, stage?: FakeAdbDiagnosticStage): FakeAdbDiagnosticEvent {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return invalidFakeAdbDiagnostic();
  const event = value as Record<string, unknown>;
  if ([...Object.keys(event)].some(key => !fakeAdbDiagnosticEventKeys.has(key))) return invalidFakeAdbDiagnostic();
  if (!isFakeAdbDiagnosticStage(event.stage)) return invalidFakeAdbDiagnostic();
  if (stage !== undefined && event.stage !== stage) return invalidFakeAdbDiagnostic();
  if (typeof event.logExists !== 'boolean' || !isFakeAdbFixturePathCategory(event.fixturePathCategory)) return invalidFakeAdbDiagnostic();
  const childExitCategory = event.childExitCategory;
  if (childExitCategory !== undefined && !isFakeAdbExitCategory(childExitCategory)) return invalidFakeAdbDiagnostic();
  if (event.stage !== 'fake-child-exit' && childExitCategory !== undefined) return invalidFakeAdbDiagnostic();
  if (event.stage === 'fake-child-exit' && childExitCategory === undefined) return invalidFakeAdbDiagnostic();
  if (event.stage === 'log-producer-append' && event.logExists !== true) return invalidFakeAdbDiagnostic();
  const result: FakeAdbDiagnosticEvent = {
    stage: event.stage,
    logExists: event.logExists,
    fixturePathCategory: event.fixturePathCategory,
  };
  if (Object.hasOwn(event, 'childExitCategory')) result.childExitCategory = childExitCategory;
  return result;
}

export function validateFakeAdbDiagnostic(value: unknown): asserts value is FakeAdbDiagnostic {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return invalidFakeAdbDiagnostic();
  const diagnostic = value as Record<string, unknown>;
  if (Object.keys(diagnostic).length !== 3 || diagnostic.schema !== 1 || !Array.isArray(diagnostic.children)) return invalidFakeAdbDiagnostic();
  const fixtureCreate = validateFakeAdbDiagnosticEvent(diagnostic.fixtureCreate, 'fixture-create');
  if (fixtureCreate.logExists || fixtureCreate.fixturePathCategory !== 'expected-owned-path') return invalidFakeAdbDiagnostic();
  let firstLaunched = false;
  let firstLaunchComplete = false;
  for (const child of diagnostic.children) {
    if (!child || typeof child !== 'object' || Array.isArray(child)) return invalidFakeAdbDiagnostic();
    const childRecord = child as Record<string, unknown>;
    if (Object.keys(childRecord).length !== 1 || !Array.isArray(childRecord.events)) return invalidFakeAdbDiagnostic();
    let previousOrder = -1;
    let launch: FakeAdbDiagnosticEvent | undefined;
    let append: FakeAdbDiagnosticEvent | undefined;
    let exit: FakeAdbDiagnosticEvent | undefined;
    for (const value of childRecord.events) {
      const event = validateFakeAdbDiagnosticEvent(value);
      if (event.stage === 'fake-child-exit' && exit) return invalidFakeAdbDiagnostic();
      if (event.stage === 'fake-child-launch' && launch) return invalidFakeAdbDiagnostic();
      if (event.stage === 'log-producer-append' && append) return invalidFakeAdbDiagnostic();
      const order = fakeAdbDiagnosticOrder.get(event.stage as Exclude<FakeAdbDiagnosticStage, 'fixture-create'>);
      if (order === undefined || order <= previousOrder) return invalidFakeAdbDiagnostic();
      previousOrder = order;
      if (event.stage === 'fake-child-launch') launch = event;
      if (event.stage === 'log-producer-append') append = event;
      if (event.stage === 'fake-child-exit') exit = event;
    }
    if (!launch) {
      if (childRecord.events.length !== 1 || exit?.childExitCategory !== 'launch-error') return invalidFakeAdbDiagnostic();
      continue;
    }
    if (!firstLaunched) {
      firstLaunched = true;
      firstLaunchComplete = append?.logExists === true
        && append.fixturePathCategory === 'expected-owned-path'
        && exit?.logExists === true
        && exit.fixturePathCategory === 'expected-owned-path'
        && exit.childExitCategory === 'success';
    }
  }
  if (!firstLaunched || !firstLaunchComplete) return invalidFakeAdbDiagnostic();
}
export interface Harness {
  createFixture(): Promise<Fixture>;
  writeState(directory: string, state: string): Promise<void>;
  snapshot(fixture: Fixture, state: string, output: string, diagnostics: string, timeoutMs?: number): Promise<{ passed: boolean; stderr: string }>;
  check(fixture: Fixture, before: string, after: string, log?: string): Promise<{ passed: boolean; issues: string[] }>;
}
export type Test = [string, () => Promise<void>];
export const chromeDeath = '09-10 08:45:09.613 546 1761 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP\n';
export const benign = '09-10 08:45:09.000 1208 7311 W PlatformConfigurator: \tat com.google.android.gms.platformconfigurator.PhenotypeConfigurationUpdateListener.onHandleIntent(:com.google.android.gms@242335041@24.23.35:207)\n';
export const marker = (time: string, message: string) => `09-10 08:45:${time} 2000 2000 I HerdrMeasure: android-test ${message}\n`;
export const hashText = (value: string) => createHash('sha256').update(value).digest('hex');
export const fakeCommandResult = (stdout: string, failure: Partial<BoundedCommandError['result']> = {}): BoundedCommandError['result'] => ({
  stdout, stderr: '', code: 0, durationMs: 1, timedOut: false, outputLimitExceeded: false, aborted: false,
  childExited: true, targetCloseObserved: true, ownedProcessesExited: true, stdioClosed: true,
  stdoutBytes: Buffer.byteLength(stdout), stderrBytes: 0, stdoutRetainedBytes: Buffer.byteLength(stdout), stderrRetainedBytes: 0,
  stdoutSha256: hashText(stdout), stderrSha256: hashText(''), ...failure,
});
export const decodeRetainedHash = (value: string) => {
  assert.match(value, /^sha256:[a-f0-9]{16}(?:\.[a-f0-9]{16}){3}$/u);
  return value.slice('sha256:'.length).replaceAll('.', '');
};

export interface DarwinProcessExitStatus {
  pid: number;
  filter: number;
  flags: number;
  filterFlags: number;
  waitStatus: number;
  exitCode: number;
  signal: number;
  coreDumped: boolean;
}

export interface DarwinProcessExitObserver {
  readonly pid: number;
  wait(timeoutMs: number): DarwinProcessExitStatus;
  close(): void;
}

export interface LifetimeCaller {
  child: ChildProcess;
  waitFor(event: string, timeoutMs: number): Promise<Record<string, unknown>>;
  waitForClose(timeoutMs: number): Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  output(): string;
  stderr(): string;
  observerOutput(): string;
  observerStderr(): string;
  observerSupervisor(): string;
  resumeSupervisorObserver(): void;
  stdoutEnded(): boolean;
  stderrEnded(): boolean;
  observerStreams(): { stdoutEnded: boolean; stdoutClosed: boolean; stderrEnded: boolean; stderrClosed: boolean; supervisorEnded: boolean; supervisorClosed: boolean; childCloseObserved: boolean };
  streamDiagnostics(): Record<string, unknown>;
  spawnError(): Error | undefined;
}

interface ObserverStreamTrace {
  bytes: number;
  tail: string;
  endedAtNs?: string;
  closedAtNs?: string;
  errorCode?: string;
}

export function sanitizedObserverTail(value: string): string {
  return value.slice(-160).replaceAll(repositoryRoot, '<repository>').replace(/[^\x20-\x7e]/gu, '?');
}

function describeObserverStream(stream: Readable, trace: ObserverStreamTrace): Record<string, unknown> {
  return {
    bytes: trace.bytes, endedAtNs: trace.endedAtNs ?? null, closedAtNs: trace.closedAtNs ?? null, errorCode: trace.errorCode ?? null,
    readableEnded: stream.readableEnded ?? null, closed: stream.closed ?? null, destroyed: stream.destroyed ?? null,
    readableLength: stream.readableLength ?? null, readableFlowing: stream.readableFlowing ?? null, tail: sanitizedObserverTail(trace.tail),
  };
}

export function startLifetimeCaller(source: string, env: NodeJS.ProcessEnv, binary = process.execPath,
  args = ['--no-env-file', '-e', source], supervisorObservation: 'draining' | 'paused' | 'closed' = 'draining'): LifetimeCaller {
  const child = spawn(binary, args, { cwd: repositoryRoot, env, stdio: ['ignore', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe'] });
  const observerStdout = child.stdio[3] as Readable;
  const observerStderr = child.stdio[4] as Readable;
  const observerSupervisor = child.stdio.at(5) as Readable;
  const streamTraces: Record<'stdout' | 'stderr' | 'supervisor', ObserverStreamTrace> = {
    stdout: { bytes: 0, tail: '' }, stderr: { bytes: 0, tail: '' }, supervisor: { bytes: 0, tail: '' },
  };
  const traceChunk = (trace: ObserverStreamTrace, chunk: Buffer | string) => {
    trace.bytes += Buffer.isBuffer(chunk) ? chunk.length : Buffer.byteLength(chunk);
    trace.tail = (trace.tail + String(chunk)).slice(-160);
  };
  const resumeSnapshots: Array<Record<string, unknown>> = [];
  let exitValue: { code: number | null; signal: NodeJS.Signals | null; atNs: string } | undefined;
  let closeObservedAtNs: string | undefined;
  let stdout = '';
  let stderr = '';
  let observerOutput = '';
  let observerStderrOutput = '';
  let observerSupervisorOutput = '';
  let pending = '';
  let stdoutClosed = false;
  let stderrClosed = false;
  let observerStdoutEnded = false;
  let observerStdoutClosed = false;
  let observerStderrEnded = false;
  let observerStderrClosed = false;
  let observerSupervisorEnded = false;
  let observerSupervisorClosed = false;
  let spawnFailure: Error | undefined;
  let parseFailure: Error | undefined;
  let closeValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  const events = new Map<string, Record<string, unknown>>();
  const waiters = new Map<string, Array<{ resolve(value: Record<string, unknown>): void; reject(error: Error): void; timer: ReturnType<typeof setTimeout> }>>();
  let resolveClose!: (value: { code: number | null; signal: NodeJS.Signals | null }) => void;
  const closed = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>(resolve => { resolveClose = resolve; });
  const receive = (chunk: Buffer, stream: 'stdout' | 'stderr') => {
    const text = chunk.toString('utf8');
    if (stream === 'stdout') {
      stdout = (stdout + text).slice(-128_000);
      pending += text;
      if (pending.length > 128_000) {
        parseFailure = new Error('lifetime caller event record exceeded its bound');
        child.kill('SIGKILL');
        return;
      }
      for (;;) {
        const end = pending.indexOf('\n');
        if (end < 0) break;
        const line = pending.slice(0, end);
        pending = pending.slice(end + 1);
        let value: Record<string, unknown>;
        try { value = JSON.parse(line) as Record<string, unknown>; } catch {
          parseFailure = new Error('lifetime caller emitted invalid event JSON');
          child.kill('SIGKILL');
          return;
        }
        if (typeof value.event !== 'string') continue;
        events.set(value.event, value);
        for (const waiter of waiters.get(value.event) || []) {
          clearTimeout(waiter.timer);
          waiter.resolve(value);
        }
        waiters.delete(value.event);
      }
    } else stderr = (stderr + text).slice(-128_000);
  };
  child.stdout!.on('data', chunk => receive(Buffer.from(chunk), 'stdout'));
  child.stderr!.on('data', chunk => receive(Buffer.from(chunk), 'stderr'));
  observerStdout.on('data', chunk => {
    traceChunk(streamTraces.stdout, chunk);
    observerOutput = (observerOutput + String(chunk)).slice(-128_000);
  });
  observerStderr.on('data', chunk => {
    traceChunk(streamTraces.stderr, chunk);
    observerStderrOutput = (observerStderrOutput + String(chunk)).slice(-128_000);
  });
  const receiveSupervisor = (chunk: Buffer | string) => {
    traceChunk(streamTraces.supervisor, chunk);
    observerSupervisorOutput = (observerSupervisorOutput + String(chunk)).slice(-128_000);
  };
  let supervisorReaderAttached = supervisorObservation === 'draining';
  if (supervisorReaderAttached) observerSupervisor.on('data', receiveSupervisor);
  if (supervisorObservation === 'closed') observerSupervisor.destroy();
  child.stdout!.once('end', () => { stdoutClosed = true; });
  child.stderr!.once('end', () => { stderrClosed = true; });
  observerStdout.once('end', () => { observerStdoutEnded = true; });
  observerStdout.once('close', () => { observerStdoutClosed = true; });
  observerStderr.once('end', () => { observerStderrEnded = true; });
  observerStderr.once('close', () => { observerStderrClosed = true; });
  observerSupervisor.once('end', () => { observerSupervisorEnded = true; });
  observerSupervisor.once('close', () => { observerSupervisorClosed = true; });
  for (const [name, stream] of [['stdout', observerStdout], ['stderr', observerStderr], ['supervisor', observerSupervisor]] as const) {
    stream.once('end', () => { streamTraces[name].endedAtNs = monotonicNowNs().toString(); });
    stream.once('close', () => { streamTraces[name].closedAtNs = monotonicNowNs().toString(); });
    stream.once('error', error => {
      const code = (error as NodeJS.ErrnoException).code;
      streamTraces[name].errorCode = typeof code === 'string' && /^E[A-Z0-9_]{1,31}$/u.test(code) ? code : 'other';
    });
  }
  child.once('error', error => { spawnFailure = error; });
  child.once('exit', (code, signal) => { exitValue = { code, signal, atNs: monotonicNowNs().toString() }; });
  child.once('close', (code, signal) => {
    closeObservedAtNs = monotonicNowNs().toString();
    closeValue = { code, signal };
    resolveClose(closeValue);
    for (const pendingWaiters of waiters.values()) for (const waiter of pendingWaiters) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error('lifetime owner closed before the requested event'));
    }
    waiters.clear();
  });
  return {
    child,
    waitFor(event, timeoutMs) {
      const existing = events.get(event);
      if (existing) return Promise.resolve(existing);
      if (closeValue) return Promise.reject(new Error('lifetime owner closed before the requested event'));
      return new Promise((resolve, reject) => {
        if (spawnFailure) return reject(spawnFailure);
        if (parseFailure) return reject(parseFailure);
        const startedAt = performance.now();
        const timer = setTimeout(() => {
          const current = waiters.get(event) || [];
          waiters.set(event, current.filter(waiter => waiter.timer !== timer));
          const table = child.pid ? lifetimeProcessTable() : [];
          const processes = table.filter(record => record.pid === child.pid || record.command.includes('owned-process-runtime.ts')
            && (record.ppid === child.pid || table.some(parent => parent.pid === record.ppid
              && parent.command.includes('owned-process-runtime.ts'))));
          const manager = processes.find(record => record.command.includes('--anchor-manager'));
          let descriptors = '';
          if (manager) {
            try { descriptors = execFileSync('/usr/sbin/lsof', ['-nP', '-a', '-p', String(manager.pid), '-d', '0-9'], { encoding: 'utf8', timeout: 1_000 }); }
            catch (error) { descriptors = String((error as { stdout?: Buffer }).stdout || error); }
          }
          reject(new Error(`lifetime owner did not report ${event} within ${Math.ceil(performance.now() - startedAt)}ms; state=${JSON.stringify({
            stdout, stderr, observerOutput, observerStderrOutput, observerSupervisorOutput, descriptors,
            observerStreams: { stdoutEnded: observerStdoutEnded, stdoutClosed: observerStdoutClosed,
              stderrEnded: observerStderrEnded, stderrClosed: observerStderrClosed,
              supervisorEnded: observerSupervisorEnded, supervisorClosed: observerSupervisorClosed,
              childCloseObserved: Boolean(closeValue) }, processes,
          })}`));
        }, timeoutMs);
        const current = waiters.get(event) || [];
        current.push({ resolve, reject, timer });
        waiters.set(event, current);
      });
    },
    waitForClose(timeoutMs) {
      return new Promise((resolve, reject) => {
        if (closeValue) return resolve(closeValue);
        const startedAt = performance.now();
        const timer = setTimeout(() => reject(new Error(`lifetime owner did not close within ${Math.ceil(performance.now() - startedAt)}ms`)), timeoutMs);
        closed.then(value => { clearTimeout(timer); resolve(value); });
      });
    },
    output: () => stdout,
    stderr: () => stderr,
    observerOutput: () => observerOutput,
    observerStderr: () => observerStderrOutput,
    observerSupervisor: () => observerSupervisorOutput,
    resumeSupervisorObserver: () => {
      const recordResume = resumeSnapshots.length === 0;
      if (recordResume) resumeSnapshots.push({ phase: 'before', atNs: monotonicNowNs().toString(),
        ...describeObserverStream(observerSupervisor, streamTraces.supervisor) });
      if (!supervisorReaderAttached) {
        supervisorReaderAttached = true;
        observerSupervisor.on('data', receiveSupervisor);
      }
      observerSupervisor.resume();
      if (recordResume) resumeSnapshots.push({ phase: 'after', atNs: monotonicNowNs().toString(),
        ...describeObserverStream(observerSupervisor, streamTraces.supervisor) });
    },
    stdoutEnded: () => stdoutClosed,
    stderrEnded: () => stderrClosed,
    observerStreams: () => ({ stdoutEnded: observerStdoutEnded, stdoutClosed: observerStdoutClosed,
      stderrEnded: observerStderrEnded, stderrClosed: observerStderrClosed,
      supervisorEnded: observerSupervisorEnded, supervisorClosed: observerSupervisorClosed, childCloseObserved: Boolean(closeValue) }),
    streamDiagnostics: () => ({
      observedAtNs: monotonicNowNs().toString(), supervisorObservation, supervisorReaderAttached,
      child: { pid: child.pid ?? null, exitCode: child.exitCode, signalCode: child.signalCode, exit: exitValue ?? null,
        close: closeValue ?? null, closeObservedAtNs: closeObservedAtNs ?? null,
        stdoutEnded: stdoutClosed, stderrEnded: stderrClosed, stdoutBytes: Buffer.byteLength(stdout), stderrTail: sanitizedObserverTail(stderr) },
      flags: { stdoutEnded: observerStdoutEnded, stdoutClosed: observerStdoutClosed, stderrEnded: observerStderrEnded,
        stderrClosed: observerStderrClosed, supervisorEnded: observerSupervisorEnded, supervisorClosed: observerSupervisorClosed },
      stdout: describeObserverStream(observerStdout, streamTraces.stdout),
      stderr: describeObserverStream(observerStderr, streamTraces.stderr),
      supervisor: describeObserverStream(observerSupervisor, streamTraces.supervisor),
      supervisorResume: resumeSnapshots,
    }),
    spawnError: () => spawnFailure,
  };
}

export function processGroupExists(groupID: number): boolean {
  try { process.kill(-groupID, 0); return true; } catch (error) {
    return (error as NodeJS.ErrnoException).code !== 'ESRCH';
  }
}

export async function waitForGroupAbsent(groupID: number, timeoutMs: number): Promise<boolean> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    if (!processGroupExists(groupID)) return true;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  return !processGroupExists(groupID);
}

export async function waitForPidAbsent(pid: number, timeoutMs: number): Promise<boolean> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    if (!pidExists(pid)) return true;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  return !pidExists(pid);
}

interface LifetimeProcessRecord {
  pid: number;
  ppid: number;
  pgid: number;
  command: string;
}

interface LifetimeOwnedGroup {
  supervisor: LifetimeProcessRecord;
  manager: LifetimeProcessRecord;
  anchor: LifetimeProcessRecord;
  target?: LifetimeProcessRecord;
}

export function lifetimeProcessTable(): LifetimeProcessRecord[] {
  const output = execFileSync('ps', ['-axo', 'pid=,ppid=,pgid=,command='], { encoding: 'utf8', timeout: 1_000 });
  return output.split('\n').flatMap(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/u);
    return match ? [{ pid: Number(match[1]), ppid: Number(match[2]), pgid: Number(match[3]), command: match[4] }] : [];
  });
}

export function lifetimeLiveGroupMembers(groupID: number): number[] {
  const output = execFileSync('ps', ['-axo', 'pid=,pgid=,state='], { encoding: 'utf8', timeout: 1_000 });
  return output.split('\n').flatMap(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\S+)$/u);
    return match && Number(match[2]) === groupID && !match[3].includes('Z') ? [Number(match[1])] : [];
  });
}

export function lifetimeSupervisor(ownerPID: number): LifetimeProcessRecord | undefined {
  return lifetimeProcessTable().find(record => record.ppid === ownerPID
    && record.command.includes('owned-process-runtime.ts') && record.command.includes('--supervisor'));
}

export function lifetimeOwnedGroup(ownerPID: number): LifetimeOwnedGroup | undefined {
  const records = lifetimeProcessTable();
  const supervisor = records.find(record => record.ppid === ownerPID
    && record.command.includes('owned-process-runtime.ts') && record.command.includes('--supervisor'));
  if (!supervisor) return undefined;
  const manager = records.find(record => record.ppid === supervisor.pid
    && record.command.includes('owned-process-runtime.ts') && record.command.includes('--anchor-manager'));
  if (!manager) return undefined;
  const anchor = records.find(record => record.ppid === manager.pid
    && record.command.includes('owned-process-runtime.ts') && record.command.includes('--anchor'));
  if (!anchor) return undefined;
  const target = records.find(record => record.ppid === manager.pid && record.pgid === anchor.pid && record.pid !== anchor.pid);
  return { supervisor, manager, anchor, target };
}

export async function waitForValue<T>(read: () => T | undefined, timeoutMs: number, message: string): Promise<T> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    const value = read();
    if (value !== undefined) return value;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  throw new Error(message);
}

export async function waitForFileValue<T>(path: string, parse: (text: string) => T, timeoutMs: number): Promise<T> {
  return waitForValue(() => {
    if (!existsSync(path)) return undefined;
    try { return parse(readFileSync(path, 'utf8')); } catch { return undefined; }
  }, timeoutMs, `lifetime readiness file did not appear: ${path}`);
}

export async function terminateLifetimeCaller(caller: LifetimeCaller, signal: NodeJS.Signals): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
  if (caller.child.exitCode === null && caller.child.signalCode === null) caller.child.kill(signal);
  return caller.waitForClose(8_000);
}

function supervisorHardDeadlineNs(caller: LifetimeCaller): bigint | undefined {
  for (const line of caller.observerSupervisor().split('\n')) {
    try {
      const record = JSON.parse(line) as { type?: string; hardDeadlineNs?: string };
      if (record.type === 'supervisor-deadlines-armed' && /^\d+$/u.test(record.hardDeadlineNs || '')) return BigInt(record.hardDeadlineNs!);
    } catch { continue; }
  }
  return undefined;
}

export async function waitForSupervisorHardDeadlineNs(caller: LifetimeCaller): Promise<bigint> {
  return waitForValue(() => supervisorHardDeadlineNs(caller), 1_000, 'supervisor did not publish its original hard deadline');
}

export async function assertCallerLossRetired(caller: LifetimeCaller, statusObserver: DarwinProcessExitObserver,
  supervisorPID: number, groupID: number | undefined, expectedSignal: NodeJS.Signals, hardDeadlineNs: bigint,
  caseName = 'owned-process'): Promise<void> {
  const remainingMs = () => {
    const remaining = Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n);
    assert.ok(remaining > 0, 'caller-loss retirement exceeded the original hard deadline');
    return remaining;
  };
  const closed = await caller.waitForClose(remainingMs());
  assert.equal(closed.signal, expectedSignal);
  let status: DarwinProcessExitStatus;
  try {
    status = statusObserver.wait(remainingMs());
  } catch (error) {
    const observedAtNs = monotonicNowNs();
    const records = lifetimeProcessTable().filter(record => record.pid === supervisorPID || record.pgid === groupID
      || record.command.includes('owned-process-runtime.ts'));
    let supervisorProcess: string;
    try { supervisorProcess = execFileSync('ps', ['-o', 'pid=,ppid=,pgid=,state=,command=', '-p', String(supervisorPID)], { encoding: 'utf8' }); }
    catch (statusError) { supervisorProcess = String((statusError as { stdout?: Buffer }).stdout || statusError); }
    process.stderr.write(`caller-loss-retirement ${JSON.stringify({
      error: error instanceof Error ? error.message : String(error), caseName, supervisorPID, groupID,
      supervisorAlive: pidExists(supervisorPID), groupAlive: groupID === undefined ? undefined : processGroupExists(groupID),
      observedAtNs: observedAtNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(), supervisorProcess,
      callerOutput: caller.output(), callerStderr: caller.stderr(), observer: caller.observerSupervisor(),
      streams: caller.observerStreams(), records,
    })}\n`);
    throw error;
  }
  assert.equal(status.pid, supervisorPID);
  assert.equal(status.filter, -5);
  assert.equal(status.flags & 0x4000, 0);
  assert.notEqual(status.filterFlags & 0x80000000, 0);
  assert.notEqual(status.filterFlags & 0x04000000, 0);
  assert.equal(status.waitStatus, 0, `kernel wait-style status must prove the real supervisor exited normally: ${JSON.stringify({ status, observer: caller.observerSupervisor() })}`);
  assert.equal(status.exitCode, 0);
  assert.equal(status.signal, 0);
  assert.equal(await waitForPidAbsent(supervisorPID, remainingMs()), true, 'supervisor process remains after its finite retirement bound');
  if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, remainingMs()), true, 'owned process group remains after its finite retirement bound');
  await waitForValue(() => {
    const streams = caller.observerStreams();
    return streams.stdoutEnded && streams.stdoutClosed && streams.stderrEnded && streams.stderrClosed
      && streams.supervisorEnded && streams.supervisorClosed ? true : undefined;
  }, remainingMs(), 'observer pipes did not naturally end and close after actual supervisor exit');
  assert.equal(caller.stdoutEnded(), true);
  assert.equal(caller.stderrEnded(), true);
  const streams = caller.observerStreams();
  assert.equal(streams.stdoutEnded, true);
  assert.equal(streams.stdoutClosed, true);
  assert.equal(streams.stderrEnded, true);
  assert.equal(streams.stderrClosed, true);
  assert.equal(streams.supervisorEnded, true);
  assert.equal(streams.supervisorClosed, true);
  assert.equal(streams.childCloseObserved, true);
}

export async function verifyNodeSupervisorBunAnchorStartup(fixture: Fixture, nodePath: string): Promise<void> {
  const wrapperDirectory = join(fixture.root, 'node-supervisor-bun-anchor-bin');
  const preloadPath = join(fixture.root, 'anchor-clock-skew.mjs');
  const wrapperStartedPath = join(fixture.root, 'anchor-wrapper-started');
  const clockSkewPath = join(fixture.root, 'anchor-clock-skew-ms');
  await mkdir(wrapperDirectory, { recursive: true });
  const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
  const realBun = execFileSync('which', ['bun'], { encoding: 'utf8' }).trim();
  await writeFile(preloadPath, `import { writeFileSync } from 'node:fs';const realNow=Date.now.bind(Date);let realClockSample;Date.now=()=>{realClockSample=realNow();return realClockSample-3_600_000};const skewedNow=Date.now();writeFileSync(${JSON.stringify(clockSkewPath)},String(realClockSample-skewedNow));`);
  await writeFile(join(wrapperDirectory, 'bun'), `#!/bin/sh\nfor argument do if [ "$argument" = '--anchor' ]; then printf '%s\\n' "$$" > ${quote(wrapperStartedPath)}; break; fi; done\n/bin/sleep 0.35\nexec ${quote(realBun)} --no-env-file --preload ${quote(preloadPath)} "$@"\n`, { mode: 0o700 });
  const env = { ...fixture.environment, PATH: `${wrapperDirectory}:${fixture.environment.PATH}` };
  const ownerSource = `const {createWriteStream}=await import('node:fs');const {spawnOwnedProcess}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))});const owned=spawnOwnedProcess(process.execPath,['-e','setInterval(()=>{},1000)'],{timeoutMs:5000,cleanupReservationMs:1000,observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})});await owned.ready;owned.start();process.stdout.write(JSON.stringify({event:'anchor-start-requested',pid:owned.supervisor.pid})+'\\n');Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,10000);`;
  let caller: LifetimeCaller | undefined;
  let supervisorPID: number | undefined;
  let groupID: number | undefined;
  let statusObserver: DarwinProcessExitObserver | undefined;
  let anchorObserver: DarwinProcessExitObserver | undefined;
  let supervisorStopped = false;
  try {
    caller = startLifetimeCaller(ownerSource, env, nodePath, ['--experimental-strip-types', '--input-type=module', '-e', ownerSource]);
    assert.ok(caller.child.pid);
    await caller.waitFor('anchor-start-requested', 2_000);
    const supervisor = await waitForValue(() => lifetimeSupervisor(caller!.child.pid!), 2_000, 'Node supervisor did not appear');
    supervisorPID = supervisor.pid;
    statusObserver = await observeDarwinProcessExit(supervisorPID);
    const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
    const wrapperWaitStartedAt = performance.now();
    let wrapperPID: number;
    try {
      wrapperPID = await waitForFileValue(wrapperStartedPath, text => Number(text), 2_000);
    } catch (error) {
      let observation = 'unavailable';
      try {
        observation = JSON.stringify({
          waitedMs: Math.round(performance.now() - wrapperWaitStartedAt), deadlineMs: 2_000,
          rootEntries: readdirSync(fixture.root).slice(0, 40), wrapperEntries: readdirSync(wrapperDirectory).slice(0, 10),
          supervisorPID, supervisorExists: pidExists(supervisorPID), streams: caller.streamDiagnostics(),
        });
      } catch (diagnosticError) { void diagnosticError; }
      throw new Error(`${error instanceof Error ? error.message : String(error)}; wrapper wait observation=${observation}`, { cause: error });
    }
    const anchorLaunch = await waitForValue(() => {
      const records = caller!.observerSupervisor().split('\n').flatMap(line => {
        try { return [JSON.parse(line) as Record<string, unknown>]; } catch { return []; }
      });
      return records.find(record => record.type === 'anchor-process-started');
    }, 1_000, 'supervisor did not record the actual Bun anchor child before startup delay');
    groupID = Number(anchorLaunch.processID);
    assert.equal(groupID, wrapperPID);
    anchorObserver = await observeDarwinProcessExit(groupID);
    assert.equal(process.kill(supervisorPID, 'SIGSTOP'), true);
    supervisorStopped = true;
    assert.match(execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim(), /^T/u);
    assert.equal(caller.child.kill('SIGKILL'), true);
    const callerClose = await caller.waitForClose(1_000);
    assert.equal(callerClose.signal, 'SIGKILL');
    assert.equal(Number(await waitForFileValue(clockSkewPath, text => Number(text), 2_000)), 3_600_000);
    await waitForValue(() => lifetimeProcessTable().some(record => record.pid === groupID && record.pgid === groupID) ? true : undefined,
      2_000, 'Bun anchor did not establish its independent process group while its supervisor was stopped');
    assert.equal(processGroupExists(groupID), true);
    const proofDeadlineNs = hardDeadlineNs - 200_000_000n;
    const preDeadlineNs = hardDeadlineNs - 1_500_000_000n;
    const preDeadlineRemainingMs = Number((preDeadlineNs - monotonicNowNs()) / 1_000_000n);
    await waitForValue(() => monotonicNowNs() >= preDeadlineNs ? true : undefined,
      Math.max(1, preDeadlineRemainingMs + 250),
      `original hard deadline did not approach: ${JSON.stringify({ preDeadlineNs: preDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(), nowNs: monotonicNowNs().toString(), preDeadlineRemainingMs })}`);
    assert.equal(processGroupExists(groupID), true, 'anchor group disappeared before its independent startup allowance expired');
    const anchorStatus = anchorObserver.wait(Math.max(1, Number((proofDeadlineNs - monotonicNowNs()) / 1_000_000n)));
    assert.equal(anchorStatus.signal, osConstants.signals.SIGKILL);
    await waitForValue(() => lifetimeLiveGroupMembers(groupID!).length === 0 ? true : undefined,
      Math.max(1, Number((proofDeadlineNs - monotonicNowNs()) / 1_000_000n)), 'Bun anchor did not retire its group while the Node supervisor was stopped');
    assert.ok(monotonicNowNs() < proofDeadlineNs, 'anchor retirement did not precede the supervisor hard deadline');
    assert.equal(pidExists(supervisorPID), true);
    assert.match(execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim(), /^T/u);
    assert.equal(process.kill(supervisorPID, 'SIGCONT'), true);
    supervisorStopped = false;
    await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, 'SIGKILL', hardDeadlineNs);
    const observerRecords = caller.observerSupervisor().split('\n').flatMap(line => {
      try { return [JSON.parse(line) as Record<string, unknown>]; } catch { return []; }
    });
    const supervisorClock = observerRecords.find(record => record.type === 'supervisor-runtime-started')?.clockNs;
    const anchorHandshake = observerRecords.find(record => record.type === 'anchor-clock-handshake');
    assert.match(String(supervisorClock), /^\d+$/u);
    assert.ok(anchorHandshake);
    const supervisorClockNs = BigInt(String(supervisorClock));
    const anchorClockNs = BigInt(String(anchorHandshake.anchorClockNs));
    const handshakeClockNs = BigInt(String(anchorHandshake.supervisorClockNs));
    assert.ok(supervisorClockNs <= anchorClockNs && anchorClockNs <= handshakeClockNs,
      'Node supervisor and Bun anchor must share the kernel monotonic clock domain');
  } finally {
    if (supervisorStopped && supervisorPID !== undefined && pidExists(supervisorPID)) process.kill(supervisorPID, 'SIGCONT');
    if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGKILL');
    if (groupID !== undefined && processGroupExists(groupID)) {
      try { process.kill(-groupID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
      assert.equal(await waitForGroupAbsent(groupID, 2_000), true);
    }
    if (supervisorPID !== undefined && pidExists(supervisorPID)) {
      process.kill(supervisorPID, 'SIGKILL');
      assert.equal(await waitForPidAbsent(supervisorPID, 2_000), true);
    }
    anchorObserver?.close();
    statusObserver?.close();
  }
}

export function pidExists(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch (error) {
    return (error as NodeJS.ErrnoException).code !== 'ESRCH';
  }
}

interface DarwinProcessStatusKernel {
  symbols: {
    kqueue(): number;
    kevent(queue: number, changes: number, changeCount: number, events: number, eventCount: number, timeout: number): number;
    close(descriptor: number): number;
    __error(): number;
  };
  pointer(value: ArrayBuffer | ArrayBufferView): number;
  toArrayBuffer(address: number, offset: number, length: number): ArrayBuffer;
}

let darwinProcessStatusKernel: Promise<DarwinProcessStatusKernel> | undefined;

async function loadDarwinProcessStatusKernel(): Promise<DarwinProcessStatusKernel> {
  if (process.platform !== 'darwin') throw new Error('DARWIN_PROCESS_STATUS_UNAVAILABLE: EVFILT_PROC NOTE_EXITSTATUS requires macOS');
  if (!darwinProcessStatusKernel) {
    darwinProcessStatusKernel = (async () => {
      const load = new Function('return import("bun:ffi")') as () => Promise<Record<string, unknown>>;
      const ffi = await load();
      const types = ffi.FFIType as Record<string, unknown>;
      const open = ffi.dlopen as (name: string, definitions: Record<string, unknown>) => { symbols: Record<string, (...args: number[]) => unknown> };
      const definitions = {
        kqueue: { args: [], returns: types.i32 },
        kevent: { args: [types.i32, types.ptr, types.i32, types.ptr, types.i32, types.ptr], returns: types.i32 },
        close: { args: [types.i32], returns: types.i32 },
        __error: { args: [], returns: types.ptr },
      };
      const toArrayBuffer = ffi.toArrayBuffer as (address: number, offset: number, length: number) => ArrayBuffer;
      return {
        symbols: open('/usr/lib/libSystem.B.dylib', definitions).symbols as unknown as DarwinProcessStatusKernel['symbols'],
        pointer: ffi.ptr as DarwinProcessStatusKernel['pointer'],
        toArrayBuffer,
      };
    })();
  }
  return darwinProcessStatusKernel;
}

export async function observeDarwinProcessExit(pid: number): Promise<DarwinProcessExitObserver> {
  if (!Number.isSafeInteger(pid) || pid < 1) throw new Error('DARWIN_PROCESS_STATUS: invalid PID');
  const kernel = await loadDarwinProcessStatusKernel();
  const queue = kernel.symbols.kqueue();
  if (queue < 0) throw new Error('DARWIN_PROCESS_STATUS: kqueue creation failed');
  const change = Buffer.alloc(32);
  change.writeBigUInt64LE(BigInt(pid), 0);
  change.writeInt16LE(-5, 8);
  change.writeUInt16LE(0x0011, 10);
  change.writeUInt32LE(0x84000000, 12);
  const registered = kernel.symbols.kevent(queue, kernel.pointer(change), 1, 0, 0, 0);
  if (registered !== 0) {
    const address = kernel.symbols.__error();
    const code = new DataView(kernel.toArrayBuffer(address, 0, 4)).getInt32(0, true);
    kernel.symbols.close(queue);
    throw new Error(`DARWIN_PROCESS_STATUS: EVFILT_PROC registration failed (${code})`);
  }
  let closed = false;
  let consumed = false;
  return {
    pid,
    wait(timeoutMs) {
      if (closed || consumed) throw new Error('DARWIN_PROCESS_STATUS: observer is already closed or consumed');
      if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1) throw new Error('DARWIN_PROCESS_STATUS: invalid wait bound');
      const deadline = performance.now() + timeoutMs;
      for (;;) {
        const remainingMs = Math.ceil(deadline - performance.now());
        if (remainingMs < 1) throw new Error(`DARWIN_PROCESS_STATUS: PID ${pid} did not exit within ${timeoutMs}ms`);
        const timeout = Buffer.alloc(16);
        timeout.writeBigInt64LE(BigInt(Math.floor(remainingMs / 1_000)), 0);
        timeout.writeBigInt64LE(BigInt((remainingMs % 1_000) * 1_000_000), 8);
        const event = Buffer.alloc(32);
        const count = kernel.symbols.kevent(queue, 0, 0, kernel.pointer(event), 1, kernel.pointer(timeout));
        if (count < 0) {
          const address = kernel.symbols.__error();
          const code = new DataView(kernel.toArrayBuffer(address, 0, 4)).getInt32(0, true);
          if (code === 4) continue;
          throw new Error(`DARWIN_PROCESS_STATUS: EVFILT_PROC wait failed (${code})`);
        }
        if (count === 0) throw new Error(`DARWIN_PROCESS_STATUS: PID ${pid} did not exit within ${timeoutMs}ms`);
        const eventPid = Number(event.readBigUInt64LE(0));
        const filter = event.readInt16LE(8);
        const flags = event.readUInt16LE(10);
        const filterFlags = event.readUInt32LE(12);
        const waitStatus = Number(event.readBigInt64LE(16));
        if (eventPid !== pid || filter !== -5 || (flags & 0x4000) !== 0
          || (filterFlags & 0x80000000) === 0 || (filterFlags & 0x04000000) === 0
          || !Number.isSafeInteger(waitStatus) || waitStatus < 0 || waitStatus > 0xffff) {
          throw new Error('DARWIN_PROCESS_STATUS: kernel exit event did not match the registered process and ABI');
        }
        consumed = true;
        return {
          pid: eventPid,
          filter,
          flags,
          filterFlags,
          waitStatus,
          exitCode: (waitStatus >>> 8) & 0xff,
          signal: waitStatus & 0x7f,
          coreDumped: (waitStatus & 0x80) !== 0,
        };
      }
    },
    close() {
      if (closed) return;
      closed = true;
      const result = kernel.symbols.close(queue);
      if (result !== 0) throw new Error('DARWIN_PROCESS_STATUS: kqueue descriptor could not be closed');
    },
  };
}

export async function verifyDarwinProcessExitStatusControls(nodePath: string, environment: NodeJS.ProcessEnv): Promise<void> {
  const controls = [
    { name: 'normal', target: 'setTimeout(() => {}, 1_000)', signal: undefined },
    { name: 'SIGABRT', target: 'setTimeout(() => {}, 5_000)', signal: 'SIGABRT' as const },
  ];
  for (const control of controls) {
    const callerSource = `const {spawn}=await import('node:child_process');const target=spawn(${JSON.stringify(nodePath)},['-e',${JSON.stringify(control.target)}],{stdio:'ignore'});if(!target.pid)throw new Error('status control target did not receive a PID');process.stdout.write(JSON.stringify({event:'kernel-status-target',targetPID:target.pid,parentPID:process.pid})+'\\n');const watchdog=setTimeout(()=>{process.exitCode=2},6000);target.once('close',()=>clearTimeout(watchdog));`;
    let caller: LifetimeCaller | undefined;
    let targetPID: number | undefined;
    let observer: DarwinProcessExitObserver | undefined;
    try {
      caller = startLifetimeCaller(callerSource, environment);
      const ready = await caller.waitFor('kernel-status-target', 3_000);
      targetPID = Number(ready.targetPID);
      assert.equal(Number(ready.parentPID), caller.child.pid);
      assert.equal(pidExists(targetPID), true);
      const processRecord = await waitForValue(() => lifetimeProcessTable().find(record => record.pid === targetPID), 1_000,
        'independent status-control target was not visible in the kernel process table');
      assert.equal(processRecord.ppid, caller.child.pid);
      observer = await observeDarwinProcessExit(targetPID);
      assert.equal(observer.pid, targetPID);
      assert.equal(pidExists(targetPID), true, 'status observer must bind a live process before its exit');
      if (control.signal) process.kill(targetPID, control.signal);
      const status = observer.wait(5_500);
      assert.equal(status.pid, targetPID);
      assert.equal(status.filter, -5);
      assert.notEqual(status.filterFlags & 0x80000000, 0);
      assert.notEqual(status.filterFlags & 0x04000000, 0);
      if (control.signal) {
        assert.equal(status.signal, osConstants.signals.SIGABRT);
        assert.equal(status.exitCode, 0);
      } else {
        assert.equal(status.waitStatus, 0);
        assert.equal(status.signal, 0);
        assert.equal(status.exitCode, 0);
      }
      const closed = await caller.waitForClose(3_000);
      assert.equal(closed.code, 0);
      assert.equal(closed.signal, null);
    } finally {
      observer?.close();
      if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
      if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 6_000), true);
    }
  }
}

export async function verifyObserverDescriptorOwnership(root: string, nodePath: string): Promise<void> {
  for (let mask = 0; mask < 8; mask++) {
    for (const missingExecutable of [false, true]) {
    const caseName = missingExecutable ? 'missing' : 'nonzero';
    const observerFiles = new Map<number, { path: string; stream: ReturnType<typeof createWriteStream>; closed: Promise<void> }>();
    const options: { observerStdout?: ReturnType<typeof createWriteStream>; observerStderr?: ReturnType<typeof createWriteStream>;
      observerSupervisor?: ReturnType<typeof createWriteStream> } = {};
    for (let index = 0; index < 3; index++) {
      if ((mask & (1 << index)) === 0) continue;
      const path = join(root, `observer-mask-${mask}-${caseName}-fd-${index + 7}`);
      const stream = createWriteStream(path, { flags: 'wx', mode: 0o600 });
      const opened = new Promise<void>((resolve, reject) => {
        stream.once('open', () => resolve());
        stream.once('error', reject);
      });
      const closed = new Promise<void>(resolve => stream.once('close', () => resolve()));
      await opened;
      observerFiles.set(index + 7, { path, stream, closed });
      if (index === 0) options.observerStdout = stream;
      else if (index === 1) options.observerStderr = stream;
      else options.observerSupervisor = stream;
    }
    const targetCode = `process.stdout.write(JSON.stringify({pid:process.pid,ppid:process.ppid,execPath:process.execPath,execArgv:process.execArgv})+'\\n');process.stderr.write('descriptor-stderr\\n');setTimeout(()=>process.exit(7),1500);`;
    const binary = missingExecutable ? join(root, `missing-executable-${mask}-${caseName}`) : nodePath;
    const args = missingExecutable ? [] : ['-e', targetCode];
    let owned: OwnedProcessHandle | undefined;
    let binding: Awaited<OwnedProcessHandle['started']> | undefined;
    let ownedFailure: Error | undefined;
    let supervisorDiagnostics = '';
    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];
    const stdoutEvents = { ended: false, closed: false };
    const stderrEvents = { ended: false, closed: false };
    let supervisorExit: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let supervisorClosed: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let resolveSupervisorExit!: (value: { code: number | null; signal: NodeJS.Signals | null }) => void;
    let resolveSupervisorClose!: (value: { code: number | null; signal: NodeJS.Signals | null }) => void;
    const exitObserved = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>(resolve => { resolveSupervisorExit = resolve; });
    const closeObserved = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>(resolve => { resolveSupervisorClose = resolve; });
    try {
      owned = spawnOwnedProcess(binary, args, { timeoutMs: 7_000, cleanupReservationMs: 1_000, ...options });
      owned.failure.then(error => { ownedFailure = error; });
      owned.supervisor.stderr?.on('data', chunk => { supervisorDiagnostics = (supervisorDiagnostics + String(chunk)).slice(-4_000); });
      owned.stdout.on('data', chunk => stdout.push(Buffer.from(chunk)));
      owned.stderr.on('data', chunk => stderr.push(Buffer.from(chunk)));
      owned.stdout.once('end', () => { stdoutEvents.ended = true; });
      owned.stdout.once('close', () => { stdoutEvents.closed = true; });
      owned.stderr.once('end', () => { stderrEvents.ended = true; });
      owned.stderr.once('close', () => { stderrEvents.closed = true; });
      owned.supervisor.once('exit', (code, signal) => {
        supervisorExit = { code, signal };
        resolveSupervisorExit(supervisorExit);
      });
      owned.supervisor.once('close', (code, signal) => {
        supervisorClosed = { code, signal };
        resolveSupervisorClose(supervisorClosed);
      });
      await owned.ready;
      owned.start();
      binding = await owned.started;
      const targetExit = await owned.targetExit;
      assert.ok(targetExit, JSON.stringify({ mask, caseName, binding, ownedFailure: ownedFailure?.message, supervisorDiagnostics,
        stdout: Buffer.concat(stdout).toString('utf8'), stderr: Buffer.concat(stderr).toString('utf8'),
        observer: observerFiles.has(9) ? await readFile(observerFiles.get(9)!.path, 'utf8').catch(() => '') : '' }));
      if (missingExecutable) {
        assert.equal(binding, undefined);
        assert.match(targetExit.launchError || '', /ENOENT/u);
        assert.notEqual(targetExit.code, 127);
        assert.equal(targetExit.signal, null);
        const targetClose = await owned.targetClose;
        assert.ok(targetClose);
        assert.equal(targetClose.signal, null);
        assert.match(targetClose.launchError || '', /ENOENT/u);
      } else {
        assert.ok(binding, JSON.stringify({ mask, targetExit,
          stdout: Buffer.concat(stdout).toString('utf8'), stderr: Buffer.concat(stderr).toString('utf8'),
          observer: observerFiles.has(9) ? await readFile(observerFiles.get(9)!.path, 'utf8').catch(() => '') : '' }));
        assert.equal(targetExit.code, 7);
        assert.equal(targetExit.signal, null);
        const targetClose = await owned.targetClose;
        assert.deepEqual(targetClose, targetExit);
        const targetIdentity = JSON.parse(Buffer.concat(stdout).toString('utf8')) as {
          pid: number; ppid: number; execPath: string; execArgv: string[];
        };
        assert.equal(targetIdentity.pid, binding.targetPID);
        assert.equal(targetIdentity.ppid, binding.spawnParentPID);
        assert.equal(targetIdentity.execPath, nodePath);
        assert.deepEqual(targetIdentity.execArgv, ['-e', targetCode]);
        assert.equal(Buffer.concat(stderr).toString('utf8'), 'descriptor-stderr\n');
      }
      const retirement = await owned.retire('observer-descriptor-control');
      assert.equal(retirement.targetExitObserved, !missingExecutable);
      assert.equal(retirement.targetCloseObserved, !missingExecutable);
      assert.equal(retirement.anchorExitObserved, true);
      assert.equal(retirement.anchorCloseObserved, true);
      assert.equal(retirement.groupAbsent, true);
      assert.equal(retirement.inputClosedObserved, true);
      assert.equal(retirement.stdoutNaturalEnd, true);
      assert.equal(retirement.stdoutCloseObserved, true);
      assert.equal(retirement.stderrNaturalEnd, true);
      assert.equal(retirement.stderrCloseObserved, true);
      assert.equal(stdoutEvents.ended, true);
      assert.equal(stdoutEvents.closed, true);
      assert.equal(stderrEvents.ended, true);
      assert.equal(stderrEvents.closed, true);
      if (binding) assert.equal(await processGroupAbsent(binding.groupID), true);
      const actualExit = await exitObserved;
      const actualClose = await closeObserved;
      assert.deepEqual(actualExit, { code: 0, signal: null });
      assert.deepEqual(actualClose, { code: 0, signal: null });
      assert.equal(owned.supervisor.exitCode, 0);
      assert.equal(owned.supervisor.signalCode, null);
      if (observerFiles.has(7)) assert.equal(await readFile(observerFiles.get(7)!.path, 'utf8'), missingExecutable ? '' : Buffer.concat(stdout).toString('utf8'));
      if (observerFiles.has(8)) assert.equal(await readFile(observerFiles.get(8)!.path, 'utf8'), missingExecutable ? '' : 'descriptor-stderr\n');
      if (observerFiles.has(9)) {
        const records = (await readFile(observerFiles.get(9)!.path, 'utf8')).trim().split('\n').map(line => JSON.parse(line) as Record<string, unknown>);
        const lifecycleTypes = new Set(['supervisor-runtime-started', 'supervisor-deadlines-armed', 'anchor-process-started',
          'anchor-clock-handshake', 'supervisor-teardown-started']);
        assert.deepEqual(records.filter(record => lifecycleTypes.has(String(record.type))).map(record => record.type),
          ['supervisor-runtime-started', 'supervisor-deadlines-armed', 'anchor-process-started', 'anchor-clock-handshake',
            'supervisor-teardown-started'], JSON.stringify({ mask, caseName, records }));
        assert.ok(/^\d+$/u.test(String(records[0].clockNs)));
        assert.ok(/^\d+$/u.test(String(records[1].executionDeadlineNs)));
        assert.ok(/^\d+$/u.test(String(records[1].hardDeadlineNs)));
        assert.ok(/^\d+$/u.test(String(records[1].clockNs)));
        assert.ok(BigInt(String(records[1].executionDeadlineNs)) < BigInt(String(records[1].hardDeadlineNs)));
      }
    } finally {
      if (owned) {
        if (!supervisorClosed) {
          await owned.retire('observer-descriptor-control-cleanup').catch(() => undefined);
          owned.release();
        }
        if (owned.binding) assert.equal(await waitForGroupAbsent(owned.binding.groupID, 8_000), true);
        if (owned.supervisor.pid) assert.equal(await waitForPidAbsent(owned.supervisor.pid, 8_000), true);
      }
      for (const observer of observerFiles.values()) {
        observer.stream.destroy();
        await observer.closed;
      }
    }
    }
  }
}

export async function verifyClosedTargetInput(root: string, nodePath: string): Promise<void> {
  const observer = createBoundedSupervisorObserver(root, 'closed-target-input-supervisor');
  const targetCode = 'process.on("SIGTERM",()=>{});process.stdin.destroy();setTimeout(()=>process.exit(7),50);setInterval(()=>{},1000)';
  const owned = spawnOwnedProcess(nodePath, ['-e', targetCode], {
    timeoutMs: 4_000,
    cleanupReservationMs: 1_000,
    input: Buffer.alloc(4 * 1024 * 1024, 0x61),
    observerSupervisor: observer.stream,
  });
  try {
    await owned.ready;
    owned.start();
    const binding = await owned.started;
    assert.ok(binding);
    const failure = await owned.failure;
    assert.match(failure.message, /EPIPE|broken pipe/u);
    const exit = await owned.targetExit;
    const close = await owned.targetClose;
    const retirement = await owned.retire('target-closed-input-control');
    assert.deepEqual(exit, { code: 7, signal: null });
    assert.deepEqual(close, exit);
    assert.equal(retirement.targetExitObserved, true);
    assert.equal(retirement.targetCloseObserved, true);
    assert.equal(retirement.anchorExitObserved, true);
    assert.equal(retirement.anchorCloseObserved, true);
    assert.equal(retirement.groupAbsent, true);
    assert.equal(retirement.inputClosedObserved, true);
    assert.equal(retirement.stdoutNaturalEnd, true);
    assert.equal(retirement.stdoutCloseObserved, true);
    assert.equal(retirement.stderrNaturalEnd, true);
    assert.equal(retirement.stderrCloseObserved, true);
    assert.equal(owned.supervisor.exitCode, 0);
    assert.equal(owned.supervisor.signalCode, null);
    const records = observer.records();
    const inputError = records.find(record => record.type === 'supervisor-manager-input-observation' && record.event === 'destinationError');
    const details = JSON.stringify({ inputError, records });
    assert.ok(inputError, details);
    const summary = inputError.summary as Record<string, unknown>;
    assert.equal(summary.destinationErrorCode, 'EPIPE', details);
    assert.equal(summary.destinationFinished, false, details);
    assert.match(String(summary.destinationErrorNs), /^\d+$/u, details);
    assert.ok(Number(summary.sourceBytes) < 4 * 1024 * 1024, details);
    assert.equal(records.some(record => record.type === 'supervisor-manager-input-observation' && record.event === 'destinationFinish'), false, details);
  } finally {
    if (!owned.isSupervisorClosed) {
      await owned.retire('target-closed-input-control-cleanup').catch(() => undefined);
      owned.release();
    }
    if (owned.binding) assert.equal(await waitForGroupAbsent(owned.binding.groupID, 8_000), true);
    if (owned.supervisor.pid) assert.equal(await waitForPidAbsent(owned.supervisor.pid, 8_000), true);
    await observer.close();
  }
}

export function cli(fixture: Fixture, args: string[]): { passed: boolean; stderr: string } {
  try {
    execFileSync(process.execPath, [process.env.ANDROID_ENVIRONMENT_SOURCE || repositoryPath('tests/mobile/android-environment.ts'), ...args], {
      cwd: repositoryRoot, env: fixture.environment, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { passed: true, stderr: '' };
  } catch (error) {
    return { passed: false, stderr: String((error as { stderr?: string }).stderr || error) };
  }
}

interface RuntimeControlObserver {
  records(): Record<string, unknown>[];
  waitFor(type: string, timeoutMs: number): Promise<Record<string, unknown>>;
  send(value: Record<string, unknown>): void;
}

export function observeRuntimeControl(child: ChildProcess): RuntimeControlObserver {
  const stream = child.stdio[3] as unknown as NodeJS.ReadWriteStream & { setEncoding(encoding: BufferEncoding): void };
  const records = new Map<string, Record<string, unknown>>();
  const waiters = new Map<string, Array<{ resolve(value: Record<string, unknown>): void; reject(error: Error): void; timer: ReturnType<typeof setTimeout> }>>();
  let pending = '';
  let failure: Error | undefined;
  const rejectWaiters = (error: Error) => {
    failure ||= error;
    for (const pendingWaiters of waiters.values()) for (const waiter of pendingWaiters) {
      clearTimeout(waiter.timer);
      waiter.reject(failure);
    }
    waiters.clear();
  };
  stream.setEncoding('utf8');
  stream.on('data', (chunk: string) => {
    pending += chunk;
    if (pending.length > 128_000) {
      rejectWaiters(new Error('owned runtime test control exceeded its bound'));
      return;
    }
    for (;;) {
      const end = pending.indexOf('\n');
      if (end < 0) return;
      const line = pending.slice(0, end);
      pending = pending.slice(end + 1);
      let value: Record<string, unknown>;
      try { value = JSON.parse(line) as Record<string, unknown>; } catch {
        rejectWaiters(new Error('owned runtime test control contained invalid JSON'));
        return;
      }
      if (typeof value.type !== 'string') continue;
      records.set(value.type, value);
      for (const waiter of waiters.get(value.type) || []) {
        clearTimeout(waiter.timer);
        waiter.resolve(value);
      }
      waiters.delete(value.type);
    }
  });
  stream.on('error', error => rejectWaiters(error instanceof Error ? error : new Error('owned runtime test control failed')));
  child.once('close', () => rejectWaiters(new Error('owned runtime test process closed before the requested control record')));
  return {
    records: () => [...records.values()],
    waitFor(type, timeoutMs) {
      const existing = records.get(type);
      if (existing) return Promise.resolve(existing);
      if (failure) return Promise.reject(failure);
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          const pendingWaiters = waiters.get(type) || [];
          waiters.set(type, pendingWaiters.filter(waiter => waiter.timer !== timer));
          reject(new Error(`owned runtime did not report ${type}`));
        }, timeoutMs);
        const pendingWaiters = waiters.get(type) || [];
        pendingWaiters.push({ resolve, reject, timer });
        waiters.set(type, pendingWaiters);
      });
    },
    send(value) { stream.write(`${JSON.stringify(value)}\n`); },
  };
}


export function androidCliTestRegistry() {
  const tests: Test[] = [];
  const test = (name: string, body: () => Promise<void>) => tests.push([`Android production CLI ${name}`, body]);
  return { tests, test };
}

export function androidEnvironmentCommands(harness: Harness) {
  const prepare = (fixture: Fixture, timeoutMs = 5_000) => cli(fixture, ['prepare', '--serial', 'emulator-5554', '--toolchains', process.env.ANDROID_ENVIRONMENT_TOOLCHAINS || repositoryPath('tests/mobile/toolchains.json'), '--output', join(fixture.root, 'preparation.json'), '--adb-timeout-ms', String(timeoutMs)]);
  const state = (fixture: Fixture, value: unknown) => writeFile(join(fixture.fixtureDirectory, 'valid-vending.json'), JSON.stringify(value));
  const snapshots = async (fixture: Fixture) => {
    const before = join(fixture.root, 'before.json');
    const after = join(fixture.root, 'after.json');
    for (const output of [before, after]) {
      const result = await harness.snapshot(fixture, 'valid', output, output.replace('.json', '-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
    }
    return { before, after };
  };
  return { prepare, state, snapshots };
}

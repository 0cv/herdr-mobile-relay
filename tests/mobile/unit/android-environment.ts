import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { constants as osConstants, tmpdir } from 'node:os';
import { closeSync, createWriteStream, existsSync, openSync, readFileSync } from 'node:fs';
import { appendFile, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { Readable } from 'node:stream';
import { createConnection as createNetConnection, createServer as createNetServer, type Server, type Socket } from 'node:net';
import { repositoryPath, repositoryRoot } from '../support/paths';
import { fileSha256 } from '../support/artifacts';
import { parseAndroidGmsComponentState, type AndroidEnvironmentSnapshot, type AndroidPreparation } from '../android-environment';
import { AndroidEnvironmentMeasurement as Measurement, gmsWriterProgress, type AndroidMeasurementIO } from '../android-measurement';
import { BoundedCommandError, boundedCommand } from '../support/bounded-process';
import { callerDeadlineToRuntime, callerDurationToRuntime, monotonicNowNs, processGroupAbsent, spawnOwnedProcess, type OwnedProcessHandle, type RetirementEvidence } from '../support/owned-process';
import { admitNonblockingDescriptor, createOwnedGroupPollOwner, createSupervisorObservationWriter, type GroupPollPhase } from '../support/owned-process-runtime';
import { PhaseBudget } from '../support/budget';
import { checkCLI, measurementIdentity, rehashPackage } from './android-product-environment';

class AndroidEnvironmentMeasurement extends Measurement {
  constructor(serial: string, output: string, toolchains: string, io: Partial<AndroidMeasurementIO> = {},
    lifetime = new PhaseBudget('android-measurement-unit', { timeoutMs: 30 * 60_000 })) {
    super(serial, output, toolchains, io, lifetime);
    this.bind(measurementIdentity({ measurementId: this.id }));
  }
}

interface GroupPollTestTimer {
  callback: () => void;
  cancelled: boolean;
  delivered: boolean;
  delayMs: number;
}

function groupPollTestTimers() {
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

import { androidLifecycleCases, runAndroidLifecycleCase, withAndroidRetainedLifecycleFixture } from './android-retained-launch';
import { AppiumClient } from '../support/webdriver';
import { androidEventDetails, androidLogEvents } from '../android-events';
import { AndroidTransportObservation, AndroidTransportTriggerDetector, transportAdbObservation, type AndroidTransportCommandResult, type AndroidTransportTestFixture } from '../support/android-transport';

interface Fixture {
  root: string;
  fixtureDirectory: string;
  log: string;
  diagnostic: string;
  environment: NodeJS.ProcessEnv;
}

function observerRecords(path: string): Array<Record<string, unknown>> {
  return readFileSync(path, 'utf8').split('\n').filter(Boolean).map(line => JSON.parse(line) as Record<string, unknown>);
}

function createBoundedSupervisorObserver(root: string, name: string) {
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

function boundedCommandFailureDetails(result: BoundedCommandError['result'] | undefined,
  records: Array<Record<string, unknown>> = []): Record<string, unknown> {
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
    'supervisor-input-observation', 'supervisor-manager-input-observation', 'supervisor-teardown-started', 'supervisor-observation-ended']);
  const observerSupervisor = records.filter(record => observedTypes.has(String(record.type))).slice(-40).map(record => {
    const fields = ['type', 'clockNs', 'executionDeadlineNs', 'hardDeadlineNs', 'stage', 'reason', 'targetPID', 'targetProcessPID', 'processID',
      'groupID', 'anchorPID', 'signal', 'targetSpawned', 'targetExecConfirmed', 'targetExit', 'targetClose', 'anchorClosed',
      'groupAbsent', 'inputClosed', 'evidence', 'event', 'summary', 'input', 'managerInput', 'unmetRetirementPrerequisites',
      'observerDelivery', 'observerDroppedRecords', 'observerDroppedRecordBytes', 'delivery', 'complete', 'droppedRecords', 'droppedRecordBytes'];
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
  };
}

function gmsAttackTimingRule(limits: unknown, stage: unknown, elapsedMs: unknown): {
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

const GMS_GATED_ENTRY_CEILING_MS = 20_000;

interface GatedEntryTimers {
  schedule(callback: () => void, delayMs: number): ReturnType<typeof setTimeout>;
  cancel(handle: ReturnType<typeof setTimeout>): void;
}

const realGatedEntryTimers: GatedEntryTimers = {
  schedule: (callback, delayMs) => setTimeout(callback, delayMs),
  cancel: handle => clearTimeout(handle),
};

function settledResult<T>(promise: Promise<T>): Promise<PromiseSettledResult<T>> {
  return Promise.allSettled([promise]).then(([result]) => result!);
}

function internalGmsFinalization(measurement: Measurement): Promise<void> {
  const finalization = (measurement as unknown as { gmsFinalizationPromise?: Promise<void> }).gmsFinalizationPromise;
  assert.ok(finalization, 'GMS finalization was not started');
  return finalization;
}

function awaitGatedEntry(entered: Promise<void>, settlement: Promise<unknown>, gate: string, operation: string,
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

async function gmsStagePersistencePrecondition(root: string, stages: readonly string[],
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

function gmsWriterDiagnostics(root: string) {
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
            ...boundedCommandFailureDetails(error.result, observer.records()) }); }
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

function eventLoopStallWindow(input: { commandStartedMs: unknown; timeoutMs: unknown; cleanupMs: unknown; markerMs: unknown; stallEndedMs?: unknown }): {
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

async function createAndroidTransportTestEndpoint(unresponsive = false,
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
      pending = Buffer.concat([pending, chunk]);
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

async function createAndroidTransportFixture(root: string, adbPort: number, state: string): Promise<AndroidTransportTestFixture> {
  const fixture: AndroidTransportTestFixture = {
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

async function installFakeAdbLifetimeBackstop(fixture: Fixture): Promise<NodeJS.ProcessEnv> {
  const directory = join(fixture.root, 'lifetime-backstop-bin');
  await mkdir(directory, { recursive: true });
  const fakeAdb = join(fixture.environment.PATH!.split(':')[0], 'adb');
  const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
  const watchdog = `${quote(process.execPath)} --no-env-file -e 'const group=Number(process.argv.at(-1));setTimeout(()=>{try{process.kill(-group,"SIGKILL")}catch{}},12000)' "$group" >/dev/null 2>&1 &`;
  const wrapper = `#!/bin/sh\ngroup=$(ps -o pgid= -p "$$" | tr -d '[:space:]')\ncase "$group" in ''|*[!0-9]*) exit 127;; esac\n${watchdog}\nexec ${quote(fakeAdb)} "$@"\n`;
  await writeFile(join(directory, 'adb'), wrapper, { mode: 0o700 });
  return { ...fixture.environment, PATH: `${directory}:${fixture.environment.PATH}` };
}

async function verifyFakeAdbLifetimeBackstop(environment: NodeJS.ProcessEnv, fixture: Fixture): Promise<void> {
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
interface Harness {
  createFixture(): Promise<Fixture>;
  writeState(directory: string, state: string): Promise<void>;
  snapshot(fixture: Fixture, state: string, output: string, diagnostics: string, timeoutMs?: number): Promise<{ passed: boolean; stderr: string }>;
  check(fixture: Fixture, before: string, after: string, log?: string): Promise<{ passed: boolean; issues: string[] }>;
}
type TestOutcome = void | string;
type Test = [string, () => Promise<TestOutcome>];
const chromeDeath = '09-10 08:45:09.613 546 1761 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP\n';
const benign = '09-10 08:45:09.000 1208 7311 W PlatformConfigurator: \tat com.google.android.gms.platformconfigurator.PhenotypeConfigurationUpdateListener.onHandleIntent(:com.google.android.gms@242335041@24.23.35:207)\n';
const marker = (time: string, message: string) => `09-10 08:45:${time} 2000 2000 I HerdrMeasure: android-test ${message}\n`;
const hashText = (value: string) => createHash('sha256').update(value).digest('hex');
const decodeRetainedHash = (value: string) => {
  assert.match(value, /^sha256:[a-f0-9]{16}(?:\.[a-f0-9]{16}){3}$/u);
  return value.slice('sha256:'.length).replaceAll('.', '');
};

interface DarwinProcessExitStatus {
  pid: number;
  filter: number;
  flags: number;
  filterFlags: number;
  waitStatus: number;
  exitCode: number;
  signal: number;
  coreDumped: boolean;
}

interface DarwinProcessExitObserver {
  readonly pid: number;
  wait(timeoutMs: number): DarwinProcessExitStatus;
  close(): void;
}

interface LifetimeCaller {
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

function sanitizedObserverTail(value: string): string {
  return value.slice(-160).replaceAll(repositoryRoot, '<repository>').replace(/[^\x20-\x7e]/gu, '?');
}

function describeObserverStream(stream: Readable, trace: ObserverStreamTrace): Record<string, unknown> {
  return {
    bytes: trace.bytes, endedAtNs: trace.endedAtNs ?? null, closedAtNs: trace.closedAtNs ?? null, errorCode: trace.errorCode ?? null,
    readableEnded: stream.readableEnded ?? null, closed: stream.closed ?? null, destroyed: stream.destroyed ?? null,
    readableLength: stream.readableLength ?? null, readableFlowing: stream.readableFlowing ?? null, tail: sanitizedObserverTail(trace.tail),
  };
}

function startLifetimeCaller(source: string, env: NodeJS.ProcessEnv, binary = process.execPath,
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

function processGroupExists(groupID: number): boolean {
  try { process.kill(-groupID, 0); return true; } catch (error) {
    return (error as NodeJS.ErrnoException).code !== 'ESRCH';
  }
}

async function waitForGroupAbsent(groupID: number, timeoutMs: number): Promise<boolean> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    if (!processGroupExists(groupID)) return true;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  return !processGroupExists(groupID);
}

async function waitForPidAbsent(pid: number, timeoutMs: number): Promise<boolean> {
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

function lifetimeProcessTable(): LifetimeProcessRecord[] {
  const output = execFileSync('ps', ['-axo', 'pid=,ppid=,pgid=,command='], { encoding: 'utf8', timeout: 1_000 });
  return output.split('\n').flatMap(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/u);
    return match ? [{ pid: Number(match[1]), ppid: Number(match[2]), pgid: Number(match[3]), command: match[4] }] : [];
  });
}

function lifetimeLiveGroupMembers(groupID: number): number[] {
  const output = execFileSync('ps', ['-axo', 'pid=,pgid=,state='], { encoding: 'utf8', timeout: 1_000 });
  return output.split('\n').flatMap(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\S+)$/u);
    return match && Number(match[2]) === groupID && !match[3].includes('Z') ? [Number(match[1])] : [];
  });
}

function lifetimeSupervisor(ownerPID: number): LifetimeProcessRecord | undefined {
  return lifetimeProcessTable().find(record => record.ppid === ownerPID
    && record.command.includes('owned-process-runtime.ts') && record.command.includes('--supervisor'));
}

function lifetimeOwnedGroup(ownerPID: number): LifetimeOwnedGroup | undefined {
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

async function waitForValue<T>(read: () => T | undefined, timeoutMs: number, message: string): Promise<T> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    const value = read();
    if (value !== undefined) return value;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  throw new Error(message);
}

async function waitForFileValue<T>(path: string, parse: (text: string) => T, timeoutMs: number): Promise<T> {
  return waitForValue(() => {
    if (!existsSync(path)) return undefined;
    try { return parse(readFileSync(path, 'utf8')); } catch { return undefined; }
  }, timeoutMs, `lifetime readiness file did not appear: ${path}`);
}

async function terminateLifetimeCaller(caller: LifetimeCaller, signal: NodeJS.Signals): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
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

async function waitForSupervisorHardDeadlineNs(caller: LifetimeCaller): Promise<bigint> {
  return waitForValue(() => supervisorHardDeadlineNs(caller), 1_000, 'supervisor did not publish its original hard deadline');
}

async function assertCallerLossRetired(caller: LifetimeCaller, statusObserver: DarwinProcessExitObserver,
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

async function verifyNodeSupervisorBunAnchorStartup(fixture: Fixture, nodePath: string): Promise<void> {
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
    const wrapperPID = await waitForFileValue(wrapperStartedPath, text => Number(text), 2_000);
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

function pidExists(pid: number): boolean {
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

async function observeDarwinProcessExit(pid: number): Promise<DarwinProcessExitObserver> {
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

async function verifyDarwinProcessExitStatusControls(nodePath: string, environment: NodeJS.ProcessEnv): Promise<void> {
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

async function verifyObserverDescriptorOwnership(root: string, nodePath: string): Promise<void> {
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

async function verifyClosedTargetInput(root: string, nodePath: string): Promise<void> {
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

function cli(fixture: Fixture, args: string[]): { passed: boolean; stderr: string } {
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

function observeRuntimeControl(child: ChildProcess): RuntimeControlObserver {
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

export function androidEnvironmentTests(harness: Harness): Test[] {
  const tests: Test[] = [];
  const test = (name: string, body: () => Promise<TestOutcome>) => tests.push([`Android production CLI ${name}`, body]);
  test('owned supervisor observations queue short writes and backpressure, bound loss and close failed sinks without waiting', async () => {
    const scriptedSink = (script: Array<number | string>) => {
      const sink = { output: [] as Buffer[], calls: 0, closes: 0, retries: [] as Array<() => void>, cancelled: 0 };
      const writer = createSupervisorObservationWriter({
        write(bytes, offset, length) {
          sink.calls++;
          const next = script.shift();
          if (typeof next === 'string') throw Object.assign(new Error('scripted observer write'), { code: next });
          const written = next === undefined ? length : next;
          if (Number.isSafeInteger(written) && written > 0 && written <= length) sink.output.push(Buffer.from(bytes.subarray(offset, offset + written)));
          return written;
        },
        close: () => { sink.closes++; },
      }, retry => {
        sink.retries.push(retry);
        return () => { sink.cancelled++; };
      });
      const records = () => Buffer.concat(sink.output).toString('utf8').split('\n').slice(0, -1)
        .map(line => JSON.parse(line) as Record<string, unknown>);
      return { sink, writer, records };
    };

    const pressured = scriptedSink([7, 'EAGAIN', 'EAGAIN']);
    assert.equal(pressured.writer.record({ type: 'first' }), true);
    assert.equal(pressured.sink.calls, 2);
    assert.equal(pressured.sink.retries.length, 1);
    assert.equal(pressured.writer.record({ type: 'second' }), true);
    assert.equal(pressured.sink.calls, 3);
    assert.equal(pressured.sink.retries.length, 1);
    assert.equal(Buffer.concat(pressured.sink.output).length, 7);
    assert.ok(pressured.writer.snapshot().queuedBytes > 0);
    pressured.sink.retries.shift()!();
    assert.equal(pressured.sink.calls, 5);
    assert.equal(pressured.sink.retries.length, 0);
    assert.equal(pressured.writer.snapshot().queuedBytes, 0);
    pressured.writer.close();
    pressured.writer.close();
    assert.equal(pressured.writer.record({ type: 'after-close' }), false);
    assert.equal(pressured.sink.closes, 1);
    const pressuredRecords = pressured.records();
    assert.deepEqual(pressuredRecords.map(record => record.type), ['first', 'second', 'supervisor-observation-ended']);
    assert.deepEqual(pressuredRecords.map(record => record.observerDelivery ?? record.delivery), ['best-effort', 'best-effort', 'best-effort']);
    assert.equal(pressuredRecords[2]!.complete, true);
    assert.equal(pressuredRecords[2]!.droppedRecords, 0);

    for (const wouldBlock of ['EAGAIN', 'EWOULDBLOCK', 'EINTR', 0]) {
      const blocked = scriptedSink([wouldBlock, wouldBlock]);
      assert.equal(blocked.writer.record({ type: 'blocked' }), true);
      assert.equal(blocked.sink.retries.length, 1);
      blocked.writer.close();
      assert.equal(blocked.sink.calls, 2);
      assert.equal(blocked.sink.closes, 1);
      assert.equal(blocked.sink.cancelled, 1);
      assert.equal(blocked.writer.snapshot().accepting, false);
      assert.equal(blocked.writer.snapshot().sinkClosed, true);
      assert.equal(blocked.writer.snapshot().queuedBytes, 0);
      assert.equal(blocked.writer.snapshot().droppedRecords, 2);
      assert.equal(blocked.writer.record({ type: 'after-close' }), false);
      blocked.sink.retries.shift()!();
      blocked.writer.close();
      assert.equal(blocked.sink.calls, 2);
      assert.equal(blocked.sink.closes, 1);
      assert.equal(blocked.sink.retries.length, 0);
      assert.deepEqual(blocked.records(), []);
    }

    const partial = scriptedSink([5, 'EAGAIN', 3, 'EAGAIN']);
    assert.equal(partial.writer.record({ type: 'partial-at-close' }), true);
    assert.equal(partial.sink.calls, 2);
    partial.writer.close();
    assert.equal(partial.sink.calls, 4);
    assert.equal(partial.sink.closes, 1);
    const partialBytes = Buffer.concat(partial.sink.output);
    assert.equal(partialBytes.length, 8);
    assert.equal(partialBytes.includes(0x0a), false);
    assert.deepEqual(partial.records(), []);
    assert.equal(partial.writer.snapshot().droppedRecords, 2);
    for (const retry of partial.sink.retries.splice(0)) retry();
    assert.equal(partial.sink.calls, 4);

    const staleRetry = scriptedSink(['EAGAIN']);
    staleRetry.writer.record({ type: 'queued-before-stale-retry' });
    const superseded = staleRetry.sink.retries.shift()!;
    staleRetry.writer.record({ type: 'drained-without-retry' });
    assert.equal(staleRetry.sink.cancelled, 1);
    assert.equal(staleRetry.sink.calls, 3);
    superseded();
    assert.equal(staleRetry.sink.calls, 3);
    staleRetry.writer.close();
    assert.deepEqual(staleRetry.records().map(record => record.type),
      ['queued-before-stale-retry', 'drained-without-retry', 'supervisor-observation-ended']);
    assert.equal(staleRetry.records()[2]!.complete, true);

    const recovered = scriptedSink(['EAGAIN']);
    recovered.writer.record({ type: 'queued' });
    assert.equal(recovered.sink.retries.length, 1);
    recovered.writer.record({ type: 'drains' });
    assert.equal(recovered.sink.cancelled, 1);
    assert.deepEqual(recovered.records().map(record => record.type), ['queued', 'drains']);

    for (const failure of ['EPIPE', 'EBADF', 'EIO', -1, Number.NaN, 1.5, 1_000_000]) {
      const broken = scriptedSink([failure]);
      assert.equal(broken.writer.record({ type: 'supervisor-retirement-ready' }), false);
      assert.equal(broken.sink.closes, 1);
      assert.equal(broken.writer.record({ type: 'supervisor-teardown-started' }), false);
      broken.writer.close();
      assert.equal(broken.sink.calls, 1);
      assert.equal(broken.sink.closes, 1);
      assert.equal(broken.sink.retries.length, 0);
      assert.equal(broken.writer.snapshot().sinkError, typeof failure === 'string' ? failure : 'other');
      assert.equal(broken.writer.snapshot().droppedRecords, 2);
      assert.equal(broken.writer.snapshot().queuedBytes, 0);
      assert.deepEqual(broken.records(), []);
    }

    const closeFailure = createSupervisorObservationWriter({
      write: (_bytes, _offset, length) => length,
      close: () => { throw Object.assign(new Error('scripted observer close'), { code: 'EBADF' }); },
    }, () => () => undefined);
    assert.equal(closeFailure.record({ type: 'last' }), true);
    assert.doesNotThrow(() => closeFailure.close());
    assert.equal(closeFailure.snapshot().sinkError, 'EBADF');
    assert.equal(closeFailure.snapshot().sinkClosed, true);

    const limited = scriptedSink([]);
    assert.equal(limited.writer.record({ type: 'oversized', detail: 'x'.repeat(16 * 1024) }), false);
    let accepted = 0;
    for (let index = 0; index < 20; index++) if (limited.writer.record({ type: 'bounded', detail: 'x'.repeat(8 * 1024) })) accepted++;
    limited.writer.close();
    const limitedRecords = limited.records();
    assert.ok(accepted >= 14 && accepted < 20, String(accepted));
    assert.equal(limitedRecords.length, accepted + 1);
    assert.ok(Buffer.concat(limited.sink.output).length <= 128 * 1024);
    assert.equal(limitedRecords[0]!.observerDroppedRecords, 1);
    assert.equal(limitedRecords.at(-1)!.type, 'supervisor-observation-ended');
    assert.equal(limitedRecords.at(-1)!.complete, false);
    assert.equal(limitedRecords.at(-1)!.droppedRecords, 1 + 20 - accepted);
  });
  test('owned supervisor observation sockets require verified nonblocking status flags before any optional write', async () => {
    const statusDouble = (initial: number, behavior: { readFails?: boolean; writeFails?: boolean; extraFlags?: number; ignoreWrite?: boolean } = {}) => {
      const state = { flags: initial, calls: [] as Array<[number, number, number]> };
      const fcntl = (fd: number, command: number, argument: number) => {
        state.calls.push([fd, command, argument]);
        if (command === 3) return behavior.readFails ? -1 : state.flags;
        assert.equal(command, 4);
        if (behavior.writeFails) return -1;
        if (!behavior.ignoreWrite) state.flags = argument | (state.calls.filter(call => call[1] === 4).length === 1 ? behavior.extraFlags ?? 0 : 0);
        return 0;
      };
      return { state, fcntl };
    };
    const preserved = statusDouble(0x0a);
    assert.deepEqual(admitNonblockingDescriptor(9, preserved.fcntl), { admitted: true, before: 0x0a, after: 0x0e });
    assert.deepEqual(preserved.state.calls, [[9, 3, 0], [9, 4, 0x0e], [9, 3, 0]]);
    assert.equal(preserved.state.flags, 0x0e);

    const already = statusDouble(0x06);
    assert.deepEqual(admitNonblockingDescriptor(9, already.fcntl), { admitted: true, before: 0x06, after: 0x06 });
    assert.deepEqual(already.state.calls, [[9, 3, 0], [9, 3, 0]]);

    const unreadable = statusDouble(0x02, { readFails: true });
    assert.deepEqual(admitNonblockingDescriptor(9, unreadable.fcntl), { admitted: false, reason: 'status-read-failed' });
    assert.deepEqual(unreadable.state.calls, [[9, 3, 0]]);

    const unwritable = statusDouble(0x02, { writeFails: true });
    assert.deepEqual(admitNonblockingDescriptor(9, unwritable.fcntl), { admitted: false, reason: 'status-write-failed', before: 0x02 });
    assert.equal(unwritable.state.flags, 0x02);

    const altered = statusDouble(0x02, { extraFlags: 0x40 });
    assert.deepEqual(admitNonblockingDescriptor(9, altered.fcntl), { admitted: false, reason: 'status-readback-mismatch', before: 0x02, after: 0x46 });
    assert.deepEqual(altered.state.calls, [[9, 3, 0], [9, 4, 0x06], [9, 3, 0], [9, 4, 0x02]]);
    assert.equal(altered.state.flags, 0x02);

    const ignored = statusDouble(0x02, { ignoreWrite: true });
    assert.deepEqual(admitNonblockingDescriptor(9, ignored.fcntl), { admitted: false, reason: 'status-readback-mismatch', before: 0x02, after: 0x02 });
    assert.deepEqual(ignored.state.calls.at(-1), [9, 4, 0x02]);
  });
  test('owned supervisor retires before its hard deadline with undrained, draining and closed optional observations', async () => {
    if (process.platform !== 'darwin') return 'independent supervisor exit status requires Darwin EVFILT_PROC NOTE_EXITSTATUS';
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const requests = 32;
    const targetCode = `process.on('SIGTERM',()=>{});setTimeout(async()=>{let bytes=0;for await(const chunk of process.stdin)bytes+=chunk.length;process.stdout.write('input-ready '+bytes+'\\n')},100);setInterval(()=>{},1000)`;
    for (const [runtime, mode, observation] of [['bun', 'caller-loss', 'paused'], ['bun', 'timeout', 'paused'],
      ['bun', 'timeout', 'draining'], ['bun', 'timeout', 'closed'], ['node', 'timeout', 'paused']] as const) {
      const source = `const {closeSync,createWriteStream}=await import('node:fs');const {spawnOwnedProcess,monotonicNowNs}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))});const publish=value=>process.stdout.write(JSON.stringify(value)+'\\n');let releaseBarrier;const barrierReleased=new Promise(resolve=>{releaseBarrier=resolve});const onRelease=()=>releaseBarrier('signal');process.on('SIGUSR2',onRelease);const supervisorObserver=createWriteStream('',{fd:5,autoClose:false});const owned=spawnOwnedProcess('node',['-e',${JSON.stringify(targetCode)}],{timeoutMs:5000,cleanupReservationMs:1000,input:Buffer.alloc(262144),observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:supervisorObserver});owned.failure.then(()=>{});publish({event:'dispatched',supervisorPID:owned.supervisor.pid,hardDeadlineNs:owned.supervisor.spawnargs.at(-1)});let resolveInput;const inputReady=new Promise(resolve=>{resolveInput=resolve});let output='';owned.stdout.on('data',chunk=>{output+=String(chunk);if(output.includes('input-ready 262144\\n'))resolveInput()});owned.stderr.resume();await owned.ready;let handoff;try{closeSync(5);handoff={closed:true,atNs:monotonicNowNs().toString(),wrapperBytesWritten:supervisorObserver.bytesWritten??null,wrapperWritableLength:supervisorObserver.writableLength??null}}catch(error){handoff={closed:false,code:typeof error?.code==='string'?error.code:'other'}}publish({event:'observer-handoff',...handoff});owned.start();const binding=await owned.started;await inputReady;publish({event:'armed',groupID:binding.groupID});await new Promise(resolve=>setTimeout(resolve,300));await new Promise(resolve=>owned.supervisor.stdio[3].write((JSON.stringify({type:'stop',reason:'timeout'})+'\\n').repeat(${requests}),resolve));publish({event:'pressure',requests:${requests}});if(${JSON.stringify(mode)}==='caller-loss')process.kill(process.pid,'SIGTERM');const exit=await owned.targetExit;const exitNs=monotonicNowNs().toString();const retirement=await owned.retire('observer-timeout');publish({event:'retired',exitSignal:exit?.signal,exitNs,groupAbsent:retirement.groupAbsent,anchorClosed:retirement.anchorCloseObserved,inputClosed:retirement.inputClosedObserved});let barrierTimer;const outcome=await Promise.race([barrierReleased,new Promise(resolve=>{barrierTimer=setTimeout(()=>resolve('timeout'),2000)})]);clearTimeout(barrierTimer);process.off('SIGUSR2',onRelease);publish({event:'barrier-released',outcome,atNs:monotonicNowNs().toString()});if(outcome!=='signal')process.exitCode=3;`;
      const caller = startLifetimeCaller(source, fixture.environment, runtime === 'bun' ? process.execPath : nodePath,
        runtime === 'bun' ? ['--no-env-file', '-e', source] : ['--experimental-strip-types', '--input-type=module', '-e', source], observation);
      let statusObserver: DarwinProcessExitObserver | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let originalHardDeadlineNs: bigint | undefined;
      let supervisorExitObservedAtNs: bigint | undefined;
      const failureSnapshot = (error: unknown) => {
        let processes: unknown;
        try {
          processes = lifetimeProcessTable().filter(record => record.pid === caller.child.pid || record.ppid === caller.child.pid
            || record.pid === supervisorPID || record.ppid === supervisorPID || (groupID !== undefined && record.pgid === groupID))
            .slice(0, 12).map(record => ({ pid: record.pid, ppid: record.ppid, pgid: record.pgid, command: sanitizedObserverTail(record.command) }));
        } catch (processError) {
          processes = `process table unavailable: ${sanitizedObserverTail(String(processError))}`;
        }
        const snapshot = { phase: 'failed', runtime, mode, observation,
          error: sanitizedObserverTail(error instanceof Error ? error.message : String(error)),
          hardDeadlineNs: originalHardDeadlineNs?.toString() ?? null, supervisorExitObservedAtNs: supervisorExitObservedAtNs?.toString() ?? null,
          supervisorPID: supervisorPID ?? null, groupID: groupID ?? null, streams: caller.streamDiagnostics(), processes };
        const text = JSON.stringify(snapshot);
        if (Buffer.byteLength(text) <= 8192) return text;
        const compact = JSON.stringify(snapshot, (key, value) => key === 'tail' || key === 'stderrTail' || key === 'processes' ? undefined : value);
        return compact.slice(0, 8192);
      };
      try {
        const dispatched = await caller.waitFor('dispatched', 2_000);
        supervisorPID = Number(dispatched.supervisorPID);
        assert.match(String(dispatched.hardDeadlineNs), /^\d+$/u);
        const hardDeadlineNs = BigInt(String(dispatched.hardDeadlineNs));
        originalHardDeadlineNs = hardDeadlineNs;
        const remainingMs = () => {
          const remaining = Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n);
          assert.ok(remaining > 0, 'observer pressure exceeded the original hard deadline');
          return remaining;
        };
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const handoff = await caller.waitFor('observer-handoff', remainingMs());
        assert.equal(handoff.closed, true, JSON.stringify(handoff));
        assert.equal(handoff.wrapperBytesWritten ?? 0, 0);
        assert.equal(handoff.wrapperWritableLength ?? 0, 0);
        const armed = await caller.waitFor('armed', remainingMs());
        groupID = Number(armed.groupID);
        assert.equal(processGroupExists(groupID), true);
        if (observation === 'draining') {
          assert.equal((await caller.waitFor('pressure', remainingMs())).requests, requests);
          await caller.waitFor('retired', remainingMs());
        } else assert.equal(caller.observerSupervisor(), '');
        const status = statusObserver.wait(remainingMs());
        const exitObservedAtNs = monotonicNowNs();
        caller.resumeSupervisorObserver();
        supervisorExitObservedAtNs = exitObservedAtNs;
        const observerAtResume = (caller.streamDiagnostics().supervisorResume as Array<Record<string, unknown>>)[0];
        if (observation !== 'closed') assert.equal(observerAtResume?.errorCode, null, JSON.stringify(observerAtResume));
        if (observation === 'paused') {
          assert.equal(observerAtResume?.destroyed, false, `undrained observer must still be open when draining starts: ${JSON.stringify(observerAtResume)}`);
          assert.equal(observerAtResume?.readableEnded, false);
        }
        assert.ok(exitObservedAtNs < hardDeadlineNs, 'observer pressure exceeded the original hard deadline');
        assert.equal(status.pid, supervisorPID);
        assert.equal(status.filter, -5);
        assert.equal(status.flags & 0x4000, 0);
        assert.notEqual(status.filterFlags & 0x80000000, 0);
        assert.notEqual(status.filterFlags & 0x04000000, 0);
        assert.equal(status.waitStatus, 0, JSON.stringify({ runtime, mode, observation, status }));
        assert.equal(status.exitCode, 0);
        assert.equal(status.signal, 0);
        assert.equal(await waitForPidAbsent(supervisorPID, remainingMs()), true);
        assert.equal(await waitForGroupAbsent(groupID, remainingMs()), true);
        let liveCallerObserverEOF: Record<string, unknown> | undefined;
        if (mode === 'timeout') {
          const retired = await caller.waitFor('retired', remainingMs());
          assert.equal(retired.groupAbsent, true);
          assert.equal(retired.anchorClosed, true);
          assert.equal(retired.inputClosed, true);
          assert.equal(retired.exitSignal, 'SIGKILL');
          assert.ok(BigInt(String(retired.exitNs)) >= hardDeadlineNs - 1_000_000_000n, 'observer pressure must not kill the target before its execution window');
          if (observation !== 'closed') {
            await waitForValue(() => {
              const current = caller.observerStreams();
              return current.supervisorEnded && current.supervisorClosed ? true : undefined;
            }, remainingMs(), 'supervisor observer did not naturally end and close while the caller stayed alive')
              .catch((error: unknown) => {
                let state: string;
                try { state = failureSnapshot(error); } catch (snapshotError) {
                  state = `unavailable: ${sanitizedObserverTail(String(snapshotError))}`;
                }
                throw new Error(`${error instanceof Error ? error.message : String(error)}; state=${state}`);
              });
            assert.equal(caller.child.exitCode, null, 'caller must still be alive when the supervisor observer reaches EOF');
            assert.equal(caller.child.signalCode, null);
            const supervisorTrace = caller.streamDiagnostics().supervisor as Record<string, unknown>;
            assert.equal(supervisorTrace.errorCode, null);
            assert.equal(typeof supervisorTrace.endedAtNs, 'string');
            liveCallerObserverEOF = { handoffAtNs: handoff.atNs, observerEndedAtNs: supervisorTrace.endedAtNs,
              observerClosedAtNs: supervisorTrace.closedAtNs, observerBytes: supervisorTrace.bytes, callerAliveAtNs: monotonicNowNs().toString() };
          }
          assert.equal(caller.child.kill('SIGUSR2'), true);
        }
        const closed = await caller.waitForClose(mode === 'caller-loss' ? remainingMs() : 2_000);
        assert.equal(closed.signal, mode === 'caller-loss' ? 'SIGTERM' : null);
        if (mode === 'timeout') {
          assert.equal(closed.code, 0, caller.stderr());
          assert.equal((await caller.waitFor('barrier-released', 1_000)).outcome, 'signal');
        }
        const streams = await waitForValue(() => {
          const current = caller.observerStreams();
          return current.stdoutEnded && current.stdoutClosed && current.stderrEnded && current.stderrClosed
            && (observation === 'closed' || (current.supervisorEnded && current.supervisorClosed)) ? current : undefined;
        }, mode === 'caller-loss' ? remainingMs() : 1_000, 'observer pipes did not naturally end and close after actual supervisor exit')
          .catch((error: unknown) => {
            let state: string;
            try { state = failureSnapshot(error); } catch (snapshotError) {
              state = `unavailable: ${sanitizedObserverTail(String(snapshotError))}`;
            }
            throw new Error(`${error instanceof Error ? error.message : String(error)}; state=${state}`);
          });
        assert.equal(caller.stdoutEnded(), true);
        assert.equal(caller.stderrEnded(), true);
        const text = caller.observerSupervisor();
        const lines = text.split('\n').slice(0, -1).filter(Boolean);
        const completeLines = lines.flatMap(line => {
          try { return [JSON.parse(line) as Record<string, unknown>]; } catch { return []; }
        });
        const stoppingRecords = completeLines.filter(record => record.type === 'supervisor-stopping').length;
        const ended = completeLines.find(record => record.type === 'supervisor-observation-ended');
        assert.ok(completeLines.every(record => record.observerDelivery === 'best-effort' || record.delivery === 'best-effort'));
        if (ended) assert.equal(ended.complete, ended.droppedRecords === 0);
        if (observation !== 'closed') {
          assert.equal(completeLines[0]?.type, 'supervisor-runtime-started');
          assert.equal(completeLines[0]?.observerSink, runtime === 'bun' ? 'darwin-nonblocking-socket' : 'node-nonblocking-stream');
        }
        if (observation === 'draining') {
          assert.equal(completeLines.length, lines.length);
          assert.ok(Buffer.byteLength(text) > 8192);
          assert.ok(stoppingRecords >= requests, String(stoppingRecords));
          assert.equal(completeLines.at(-1), ended);
          assert.equal(ended?.complete, true);
          assert.ok(completeLines.some(record => record.type === 'supervisor-execution-deadline'));
        } else if (observation === 'paused') {
          assert.ok(completeLines.length > 0);
          assert.ok(stoppingRecords < requests, `undrained observer must withhold offered pressure: ${stoppingRecords}`);
          assert.equal(ended, undefined);
        } else assert.equal(text, '');
        await appendFile(join(fixture.root, 'optional-observer-retirement.jsonl'), `${JSON.stringify({ runtime, mode, observation,
          supervisorPID, groupID, status, hardDeadlineNs: hardDeadlineNs.toString(), exitObservedAtNs: exitObservedAtNs.toString(),
          observerBytes: Buffer.byteLength(text), observerSink: completeLines[0]?.observerSink, stoppingRecords, streams, ended,
          handoff, observerAtResume, liveCallerObserverEOF, callerExitAtNs: (caller.streamDiagnostics().child as { exit?: { atNs?: string } | null }).exit?.atNs ?? null })}\n`);
      } catch (error) {
        await Promise.resolve()
          .then(() => appendFile(join(fixture.root, 'optional-observer-retirement.jsonl'), `${failureSnapshot(error)}\n`))
          .catch(() => undefined);
        throw error;
      } finally {
        caller.resumeSupervisorObserver();
        statusObserver?.close();
        if (caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
      }
    }
  });
  test('owned process group poll red baseline models duplicate stale callbacks under adversarial scheduling, not as a Bun occurrence', async () => {
    interface LegacyTimer {
      callback: () => void;
      cancelled: boolean;
    }
    const timers: LegacyTimer[] = [];
    let currentTimer: LegacyTimer | undefined;
    let groupPresent = true;
    let absenceRows = 0;
    const schedule = (callback: () => void): LegacyTimer => {
      const timer = { callback, cancelled: false };
      timers.push(timer);
      return timer;
    };
    const cancel = (timer: LegacyTimer) => { timer.cancelled = true; };
    const inspect = () => {
      if (!groupPresent) {
        absenceRows++;
        if (currentTimer) cancel(currentTimer);
        return;
      }
      currentTimer = schedule(inspect);
    };
    inspect();
    const anchorPoll = timers[0]!;
    inspect();
    const managerPoll = timers[1]!;
    assert.notEqual(anchorPoll, managerPoll);
    groupPresent = false;
    anchorPoll.callback();
    assert.equal(managerPoll.cancelled, true);
    managerPoll.callback();
    assert.equal(absenceRows, 2);
  });
  test('owned process group poll owner invalidates canceled callbacks in either callback order', async () => {
    for (const order of ['superseded-first', 'owned-first'] as const) {
      const timers = groupPollTestTimers();
      const probeValues = [true, true, false];
      const samples: Array<{ present: boolean; phase: GroupPollPhase }> = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          const value = probeValues.shift();
          assert.notEqual(value, undefined);
          return value!;
        },
        observe: (present, phase) => samples.push({ present, phase }),
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const supersededPoll = timers.pending()[0]!;
      assert.equal(owner.inspect('after-manager-close'), true);
      const currentPoll = timers.pending()[0]!;
      assert.equal(supersededPoll.cancelled, true);
      assert.equal(supersededPoll.delayMs, 10);
      assert.equal(currentPoll.delayMs, 10);
      if (order === 'superseded-first') {
        timers.deliver(supersededPoll, true);
        timers.deliver(currentPoll);
      } else {
        timers.deliver(currentPoll);
        timers.deliver(supersededPoll, true);
      }
      assert.equal(probes, 3);
      assert.deepEqual(samples, [
        { present: true, phase: 'before-manager-close' },
        { present: true, phase: 'after-manager-close' },
        { present: false, phase: 'after-manager-close' },
      ]);
      assert.equal(samples.filter(sample => !sample.present && sample.phase === 'after-manager-close').length, 1);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
    }
  });
  test('owned process group poll owner keeps manager snapshot ahead of fresh post-close probes', async () => {
    for (const scenario of [
      { probeValues: [false, false], snapshot: true, preCloseAbsences: 1 },
      { probeValues: [true, false], snapshot: false, preCloseAbsences: 0 },
    ]) {
      const timers = groupPollTestTimers();
      const probeValues = [...scenario.probeValues];
      const records: Array<{ type: string; phase?: GroupPollPhase; groupAbsent?: boolean }> = [];
      let groupAbsent = false;
      let retirementReady = false;
      let probes = 0;
      const retirementEvidence = {
        anchorClosed: true,
        managerClosed: false,
        inputClosed: false,
        targetSettled: false,
        relayStreamsRetired: false,
      };
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          const value = probeValues.shift();
          assert.notEqual(value, undefined);
          return value!;
        },
        observe: (present, phase, isCurrent) => {
          groupAbsent = !present;
          records.push({ type: present ? 'group-present' : 'supervisor-group-absent', phase });
          if (!present && isCurrent() && groupAbsent && Object.values(retirementEvidence).every(Boolean)) retirementReady = true;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const anchorPoll = timers.pending()[0];
      const managerCloseSnapshot = groupAbsent;
      records.push({ type: 'supervisor-manager-close', groupAbsent: managerCloseSnapshot });
      retirementEvidence.managerClosed = true;
      assert.equal(managerCloseSnapshot, scenario.snapshot);
      assert.equal(owner.inspect('after-manager-close'), true);
      assert.equal(groupAbsent, true);
      if (anchorPoll) {
        assert.equal(anchorPoll.cancelled, true);
        timers.deliver(anchorPoll, true);
      }
      assert.equal(probes, 2);
      const managerCloseIndex = records.findIndex(record => record.type === 'supervisor-manager-close');
      const postCloseAbsenceIndex = records.findIndex(record => record.type === 'supervisor-group-absent' && record.phase === 'after-manager-close');
      assert.ok(managerCloseIndex >= 0 && postCloseAbsenceIndex > managerCloseIndex);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent' && record.phase === 'before-manager-close').length,
        scenario.preCloseAbsences);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent' && record.phase === 'after-manager-close').length, 1);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent').length, scenario.preCloseAbsences + 1);
      assert.equal(retirementReady, false);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
    }
  });
  test('owned process group poll owner rejects reentrant work and remains closed after finalization', async () => {
    {
      const timers = groupPollTestTimers();
      let probes = 0;
      let observations = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return true; },
        observe: () => { observations++; },
      });
      assert.equal(probes, 0);
      assert.equal(observations, 0);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
      owner.finalize();
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 0);
      assert.equal(observations, 0);
    }
    {
      const timers = groupPollTestTimers();
      const samples: Array<{ present: boolean; phase: GroupPollPhase }> = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          if (probes === 1) {
            owner.inspect('after-manager-close');
            return false;
          }
          return true;
        },
        observe: (present, phase) => samples.push({ present, phase }),
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 2);
      assert.deepEqual(samples, [{ present: true, phase: 'after-manager-close' }]);
      assert.equal(timers.pending().length, 1);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      const probeValues = [false, false];
      const samples: GroupPollPhase[] = [];
      const notifications: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => probeValues.shift()!,
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          if (!present && phase === 'before-manager-close') owner.inspect('after-manager-close');
          if (!present && isCurrent()) notifications.push(phase);
        },
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.deepEqual(samples, ['before-manager-close', 'after-manager-close']);
      assert.deepEqual(notifications, ['after-manager-close']);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      const samples: GroupPollPhase[] = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          if (probes === 2) owner.inspect('after-manager-close');
          return true;
        },
        observe: (_present, phase) => samples.push(phase),
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const callback = timers.pending()[0]!;
      timers.deliver(callback);
      assert.equal(probes, 3);
      assert.deepEqual(samples, ['before-manager-close', 'after-manager-close']);
      assert.equal(timers.pending().length, 1);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      let probes = 0;
      let notifications = 0;
      const samples: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return false; },
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          owner.finalize();
          if (!present && isCurrent()) notifications++;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 1);
      assert.deepEqual(samples, ['before-manager-close']);
      assert.equal(notifications, 0);
      assert.deepEqual(timers.pending(), []);
      assert.equal(owner.inspect('after-manager-close'), false);
      assert.equal(probes, 1);
      assert.deepEqual(samples, ['before-manager-close']);
    }
    for (const finalization of ['normal', 'forced'] as const) {
      const timers = groupPollTestTimers();
      let probes = 0;
      let notifications = 0;
      const samples: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return true; },
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          if (!present && isCurrent()) notifications++;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const staleCallback = timers.pending()[0]!;
      owner.finalize();
      owner.finalize();
      timers.deliver(staleCallback, true);
      assert.equal(owner.inspect('after-manager-close'), false);
      assert.equal(probes, 1, finalization);
      assert.deepEqual(samples, ['before-manager-close'], finalization);
      assert.equal(notifications, 0, finalization);
      assert.deepEqual(timers.pending(), [], finalization);
    }
  });
  test('owned process-group signals require a live matching group and session member', async () => {
    const source = await readFile(repositoryPath('tests/mobile/support/owned-process-runtime.ts'), 'utf8');
    const groupSignals = [...source.matchAll(/process\.kill\(\s*-\s*([^,\n)]+)(?:,\s*([^)\n]+))?\s*\)/gu)]
      .filter(match => match[2]?.trim() !== '0');
    assert.deepEqual(groupSignals.map(match => [match[1]!.trim(), match[2]?.trim()]), [['groupID', 'signal']]);
    const killApis = [...source.matchAll(/\b([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*\(/gu)]
      .map(match => match[1]!)
      .filter(name => /(?:^|\.)(?:kill|killpg|pkill|syscall)$/u.test(name))
      .sort();
    assert.deepEqual(killApis, ['anchor.kill', 'anchor.kill', 'process.kill', 'process.kill']);
    assert.doesNotMatch(source, /\b[A-Za-z_$][\w$]*\s*\[\s*['"](?:kill|killpg|pkill)['"]\s*\]\s*\(/u);
    assert.doesNotMatch(source, /\b(?:exec|execFile|spawn|spawnSync)\s*\(\s*['"`](?:kill|pkill)['"`]/u);
    assert.match(source, /function signalOwnedProcessGroup[\s\S]*?getpgid\(0\) !== groupID[\s\S]*?getsid\(0\) !== sessionID[\s\S]*?process\.kill\(-groupID, signal\)/u);
    assert.match(source, /guardianParentPID !== expectedAnchorPID[\s\S]*?groupID !== expectedAnchorPID[\s\S]*?sessionID !== expectedSession/u);
    assert.match(source, /guardianGroupID: groupID/u);
    assert.match(source, /Number\(message\.groupID\) !== anchorPID \|\| Number\(message\.sessionID\) !== process\.pid/u);
    assert.match(source, /if \(stopping \|\| startupExpired\) \{\s*sendControl\(\{ type: 'guardian-configuration-refused'/u);
    assert.match(source, /if \(stopping \|\| startupExpired\) \{\s*sendControl\(\{ type: 'anchor-configuration-refused'/u);
    assert.match(source, /guardianConfigured \|\| stopping \|\| startupExpired \|\| !pendingAnchorReady/u);
    assert.match(source, /if \(message\.type === 'anchor-stopping'\) \{\s*stopping = true/u);
  });
  test('owned group guardian retires TERM-resistant descendants after its anchor is killed', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let cleanupHardDeadlineNs: bigint | undefined;
    const targetReadyPath = join(fixture.root, 'anchor-loss-target-ready');
    const descendantReadyPath = join(fixture.root, 'anchor-loss-descendant-ready');
    const descendantCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(descendantReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{},1000);`;
    const targetCode = `(async()=>{const fs=require('node:fs');const {spawn}=require('node:child_process');const ffi=await(new Function('return import("bun:ffi")'))();const types=ffi.FFIType;const library=process.platform==='darwin'?'/usr/lib/libSystem.B.dylib':'libc.so.6';const native=ffi.dlopen(library,{setpgid:{args:[types.i32,types.i32],returns:types.i32},getpgid:{args:[types.i32],returns:types.i32},getsid:{args:[types.i32],returns:types.i32}}).symbols;const groupID=Number(process.argv.at(-2));const sessionID=Number(process.argv.at(-1));if(native.setpgid(0,groupID)!==0||native.getpgid(0)!==groupID||native.getsid(0)!==sessionID)throw new Error('target group identity did not match the admitted anchor');process.on('SIGTERM',()=>{});const descendant=spawn(process.execPath,['--no-env-file','-e',${JSON.stringify(descendantCode)}],{stdio:'ignore',env:process.env});process.on('SIGUSR1',()=>{if(descendant.exitCode===null&&descendant.signalCode===null)descendant.kill('SIGKILL');process.exit(0)});await new Promise((resolve,reject)=>{descendant.once('spawn',resolve);descendant.once('error',reject)});fs.writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,groupID:native.getpgid(0),sessionID:native.getsid(0),descendantPID:descendant.pid}));setTimeout(()=>{if(descendant.exitCode===null&&descendant.signalCode===null)descendant.kill('SIGKILL');process.exit(0)},8000);setInterval(()=>{},1000)})().catch(error=>{require('node:fs').writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({error:String(error)}));process.exitCode=1});`;
    let anchor: ChildProcess | undefined;
    let target: ChildProcess | undefined;
    let anchorCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let targetCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let groupID: number | undefined;
    let descendantPID: number | undefined;
    let descendantExitObserver: DarwinProcessExitObserver | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      assert.ok(Number.isSafeInteger(sessionID) && sessionID > 0);
      const startedAtNs = monotonicNowNs();
      const startupDeadlineNs = startedAtNs + 12_000_000_000n;
      const executionDeadlineNs = startedAtNs + 8_000_000_000n;
      const hardDeadlineNs = startedAtNs + 10_000_000_000n;
      cleanupHardDeadlineNs = hardDeadlineNs;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      anchor.once('close', (code, signal) => { anchorCloseValue = { code, signal }; });
      let anchorStderr = '';
      anchor.stderr?.on('data', chunk => { anchorStderr = (anchorStderr + String(chunk)).slice(-4_000); });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 3_000);
      groupID = Number(nativeReady.groupID);
      const guardianPID = Number(nativeReady.guardianPID);
      assert.equal(Number(nativeReady.anchorPID), anchor.pid);
      assert.equal(Number(nativeReady.spawnParentPID), process.pid);
      assert.equal(Number(nativeReady.sessionID), sessionID);
      assert.ok(Number.isSafeInteger(groupID) && groupID === anchor.pid);
      assert.ok(Number.isSafeInteger(guardianPID) && guardianPID > 0 && guardianPID !== anchor.pid);
      assert.equal(Number(nativeReady.guardianParentPID), anchor.pid);
      assert.equal(Number(nativeReady.guardianGroupID), groupID);
      assert.equal(Number(nativeReady.guardianSessionID), sessionID);
      const guardian = await waitForValue(() => lifetimeProcessTable().find(record => record.pid === guardianPID), 2_000,
        `anchor guardian was not visible in the process table: ${anchorStderr}`);
      assert.equal(guardian.ppid, anchor.pid);
      assert.equal(guardian.pgid, groupID);
      const configuration = { type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 150 };
      control.send(configuration);
      const ready = await control.waitFor('anchor-ready', 2_000);
      assert.equal(Number(ready.guardianPID), guardianPID);
      assert.equal(Number(ready.guardianGroupID), groupID);
      target = spawn(process.execPath, ['--no-env-file', '-e', targetCode, String(groupID), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe'],
      });
      target.once('close', (code, signal) => { targetCloseValue = { code, signal }; });
      const targetIdentity = await waitForFileValue(targetReadyPath, text => JSON.parse(text) as Record<string, unknown>, 4_000);
      assert.equal(targetIdentity.error, undefined);
      assert.equal(Number(targetIdentity.pid), target.pid);
      assert.equal(Number(targetIdentity.ppid), process.pid);
      assert.equal(Number(targetIdentity.groupID), groupID);
      assert.equal(Number(targetIdentity.sessionID), sessionID);
      descendantPID = Number(targetIdentity.descendantPID);
      const descendant = await waitForFileValue(descendantReadyPath, text => JSON.parse(text) as Record<string, unknown>, 2_000);
      assert.equal(Number(descendant.pid), descendantPID);
      assert.equal(Number(descendant.ppid), target.pid);
      if (process.platform === 'darwin') descendantExitObserver = await observeDarwinProcessExit(descendantPID);
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(target.pid!));
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(descendantPID));
      assert.equal(anchor.kill('SIGKILL'), true);
      const anchorClosed = await waitForValue(() => anchorCloseValue, 2_000, 'anchor ChildProcess handle did not observe SIGKILL');
      assert.equal(anchorClosed.signal, 'SIGKILL');
      const targetClosed = await waitForValue(() => targetCloseValue, 2_000, 'live guardian did not retire the target after anchor loss');
      assert.equal(targetClosed.signal, 'SIGKILL');
      if (descendantExitObserver) assert.equal(descendantExitObserver.wait(1_000).signal, osConstants.signals.SIGKILL);
      assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs, 'guardian retirement did not precede the hard deadline');
    } finally {
      try {
        if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
        if (target && target.exitCode === null && target.signalCode === null) target.kill('SIGUSR1');
        if (groupID !== undefined && cleanupHardDeadlineNs !== undefined) {
          const remainingMs = Math.max(1, Number((cleanupHardDeadlineNs - monotonicNowNs()) / 1_000_000n) + 1_000);
          assert.equal(await waitForGroupAbsent(groupID, Math.min(13_000, remainingMs)), true,
            'anchor-loss fixture group was not retired by its identity-bound deadline');
        }
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      } finally {
        descendantExitObserver?.close();
        await rm(fixture.root, { recursive: true, force: true });
      }
    }
  });
  test('owned group termination prevents guardian readiness before configuration', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let cleanupHardDeadlineNs: bigint | undefined;
    let groupID: number | undefined;
    let anchor: ChildProcess | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      const startedAtNs = monotonicNowNs();
      const startupDeadlineNs = startedAtNs + 5_000_000_000n;
      const executionDeadlineNs = startedAtNs + 3_000_000_000n;
      const hardDeadlineNs = startedAtNs + 4_000_000_000n;
      cleanupHardDeadlineNs = hardDeadlineNs;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      let anchorStderr = '';
      anchor.stderr?.on('data', chunk => { anchorStderr = (anchorStderr + String(chunk)).slice(-4_000); });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 3_000);
      groupID = Number(nativeReady.groupID);
      const guardianPID = Number(nativeReady.guardianPID);
      assert.equal(Number(nativeReady.anchorPID), anchor.pid);
      assert.ok(Number.isSafeInteger(groupID) && groupID === anchor.pid);
      assert.equal(Number(nativeReady.guardianParentPID), anchor.pid);
      assert.equal(Number(nativeReady.guardianGroupID), groupID);
      assert.equal(Number(nativeReady.guardianSessionID), sessionID);
      const guardian = await waitForValue(() => lifetimeProcessTable().find(record => record.pid === guardianPID), 1_000,
        `anchor guardian was not visible before group termination: ${anchorStderr}`);
      assert.equal(guardian.ppid, anchor.pid);
      assert.equal(guardian.pgid, groupID);
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(anchor.pid!));
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(guardianPID));
      assert.equal(anchor.exitCode, null);
      assert.equal(anchor.signalCode, null);
      assert.equal(process.kill(-groupID, 'SIGTERM'), true);
      await control.waitFor('anchor-stopping', 1_000);
      control.send({ type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 100 });
      assert.equal(control.records().some(record => record.type === 'anchor-ready'), false);
      const anchorClose = await waitForValue(() => anchor?.signalCode ? { signal: anchor.signalCode } : undefined, 1_000,
        `anchor did not retire its group after guardian readiness refusal: ${anchorStderr}`);
      assert.equal(anchorClose.signal, 'SIGKILL');
      assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs);
    } finally {
      try {
        if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
        if (groupID !== undefined && cleanupHardDeadlineNs !== undefined) {
          const remainingMs = Math.max(1, Number((cleanupHardDeadlineNs - monotonicNowNs()) / 1_000_000n) + 1_000);
          assert.equal(await waitForGroupAbsent(groupID, Math.min(6_000, remainingMs)), true,
            'pre-configuration guardian group was not retired by its identity-bound deadline');
        }
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  });
  test('owned guardian timeout reason reaches its stopped anchor before process-group retirement', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let anchor: ChildProcess | undefined;
    let anchorStopped = false;
    let groupID: number | undefined;
    let anchorClose: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      const startupDeadlineNs = monotonicNowNs() + 5_000_000_000n;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      anchor.once('close', (code, signal) => { anchorClose = { code, signal }; });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 2_000);
      groupID = Number(nativeReady.groupID);
      assert.equal(groupID, anchor.pid);
      const executionDeadlineNs = monotonicNowNs() + 250_000_000n;
      const hardDeadlineNs = executionDeadlineNs + 2_000_000_000n;
      control.send({ type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 300 });
      const ready = await control.waitFor('anchor-ready', 1_000);
      assert.equal(Number(ready.groupID), groupID);
      assert.equal(process.kill(anchor.pid!, 'SIGSTOP'), true);
      anchorStopped = true;
      const remainingMs = Math.max(0, Math.ceil(Number(executionDeadlineNs - monotonicNowNs()) / 1_000_000) + 60);
      await new Promise(resolve => setTimeout(resolve, remainingMs));
      assert.equal(process.kill(anchor.pid!, 'SIGCONT'), true);
      anchorStopped = false;
      const stopping = await control.waitFor('anchor-stopping', 1_000);
      assert.equal(stopping.reason, 'timeout');
      const closed = await waitForValue(() => anchorClose, 1_000, 'guardian did not retire the timed-out anchor group');
      assert.equal(closed.signal, 'SIGKILL');
      assert.equal(await waitForGroupAbsent(groupID, 500), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs);
    } finally {
      if (anchorStopped && anchor?.pid) anchor.kill('SIGCONT');
      if (groupID !== undefined && processGroupExists(groupID)) {
        try { process.kill(-groupID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      }
      if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('owned guardian refuses an unrelated numeric process-group identity', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    const sentinelReadyPath = join(fixture.root, 'unrelated-group-ready');
    const sentinelSignalPath = join(fixture.root, 'unrelated-group-signaled');
    const sentinelCode = `const fs=require('node:fs');process.on('SIGTERM',()=>fs.writeFileSync(${JSON.stringify(sentinelSignalPath)},'SIGTERM'));process.on('SIGHUP',()=>fs.writeFileSync(${JSON.stringify(sentinelSignalPath)},'SIGHUP'));fs.writeFileSync(${JSON.stringify(sentinelReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{},1000);`;
    let sentinel: ChildProcess | undefined;
    let probe: ChildProcess | undefined;
    let sentinelCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let probeCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    try {
      sentinel = spawn(process.execPath, ['--no-env-file', '-e', sentinelCode], {
        cwd: repositoryRoot, env: environment, detached: true, stdio: ['ignore', 'ignore', 'pipe'],
      });
      sentinel.once('close', (code, signal) => { sentinelCloseValue = { code, signal }; });
      await waitForFileValue(sentinelReadyPath, text => JSON.parse(text) as Record<string, unknown>, 2_000);
      assert.ok(sentinel.pid);
      const runtimeDeadline = (monotonicNowNs() + 3_000_000_000n).toString();
      probe = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor-guardian',
        runtimeDeadline, String(sentinel.pid), String(sentinel.pid)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      probe.once('close', (code, signal) => { probeCloseValue = { code, signal }; });
      const control = observeRuntimeControl(probe);
      const refused = await control.waitFor('guardian-bootstrap-error', 2_000);
      assert.match(String(refused.message), /identity did not match/u);
      const probeClosed = await waitForValue(() => probeCloseValue, 1_000, 'misbound guardian process did not fail closed');
      assert.equal(probeClosed.code, 1);
      assert.equal(sentinel.exitCode, null);
      assert.equal(sentinel.signalCode, null);
      assert.equal(existsSync(sentinelSignalPath), false);
    } finally {
      try {
        if (probe && probe.exitCode === null && probe.signalCode === null) probe.kill('SIGKILL');
        let killedSentinel = false;
        if (sentinel && sentinel.exitCode === null && sentinel.signalCode === null) {
          sentinel.kill('SIGKILL');
          killedSentinel = true;
        }
        if (sentinel && sentinel.pid) {
          const closed = await waitForValue(() => sentinelCloseValue, 1_000, 'unrelated-group sentinel did not close');
          if (killedSentinel) assert.equal(closed.signal, 'SIGKILL');
          assert.equal(await waitForGroupAbsent(sentinel.pid, 1_000), true);
        }
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  });
  test('owned manager output writer failure retires the manager without fabricated stream EOF', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const bunPath = execFileSync('which', ['bun'], { encoding: 'utf8' }).trim();
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    const ownedProcessPath = repositoryPath('tests/mobile/support/owned-process.ts');
    const targetCode = "process.on('SIGTERM',()=>{});process.stdout.write('MANAGER_OUTPUT_READY');setInterval(()=>process.stdout.write('x'.repeat(8192)),5);";
    const controllerSource = `(async () => {
      const { spawn, execFileSync } = await import('node:child_process');
      const { monotonicNowNs } = await import(${JSON.stringify(ownedProcessPath)});
      const controllerPID = process.pid;
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const nativeLibrary = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(nativeLibrary, {
        setsid: { args: [], returns: ffiTypes.i32 },
        getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 },
      }).symbols;
      if (native.setsid() !== controllerPID) throw new Error('controller could not establish its owned session');
      const nodePath = ${JSON.stringify(nodePath)};
      const bunPath = ${JSON.stringify(bunPath)};
      const runtimePath = ${JSON.stringify(runtimePath)};
      const targetCode = ${JSON.stringify(targetCode)};
      const startedAtNs = monotonicNowNs();
      const hardDeadlineNs = startedAtNs + 700_000_000n;
      const executionDeadlineNs = hardDeadlineNs - 200_000_000n;
      const records = [];
      let controllerError;
      let manager;
      let control;
      let managerOutput;
      let managerPID;
      let anchorPID;
      let groupID;
      let targetPID;
      let managerProcess;
      let anchorProcess;
      let targetProcess;
      let targetStarted;
      let managerOutputPrefix = '';
      let managerOutputBytes = 0;
      let managerStderrPrefix = '';
      let managerOutputNaturalEnd = false;
      let managerOutputEndAtNs;
      let managerOutputCloseObserved = false;
      let managerOutputClosedByController = false;
      let managerOutputCloseRequestedAtNs;
      let managerOutputBytesAtClose;
      let failureObservedAtNs;
      let targetExit;
      let targetExitAtNs;
      let targetClose;
      let targetCloseAtNs;
      let anchorExit;
      let anchorExitAtNs;
      let anchorClose;
      let anchorCloseAtNs;
      let managerExit;
      let managerExitAtNs;
      let managerClose;
      let managerCloseAtNs;
      let managerStderrNaturalEnd = false;
      let managerStderrCloseObserved = false;
      let pendingControl = '';
      const emit = (event, value = {}) => process.stdout.write(JSON.stringify({ event, ...value }) + '\\n');
      const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
      const alive = pid => {
        if (!Number.isSafeInteger(pid) || pid < 1) return false;
        try { process.kill(pid, 0); return true; }
        catch (error) { return error.code !== 'ESRCH'; }
      };
      const groupAlive = id => {
        if (!Number.isSafeInteger(id) || id < 1) return false;
        try { process.kill(-id, 0); return true; }
        catch (error) { return error.code !== 'ESRCH'; }
      };
      const processInfo = pid => {
        const text = execFileSync('ps', ['-o', 'pid=,ppid=,pgid=', '-p', String(pid)], { encoding: 'utf8', timeout: 250 }).trim();
        const fields = text.split(/\\s+/u).map(Number);
        const sid = native.getsid(pid);
        if (fields.length !== 3 || fields.some(value => !Number.isSafeInteger(value)) || !Number.isSafeInteger(sid) || sid < 1) {
          throw new Error('process identity was not visible in ps or the kernel');
        }
        return { pid: fields[0], ppid: fields[1], pgid: fields[2], sid };
      };
      const send = value => {
        if (!control || control.destroyed || !control.writable) throw new Error('manager control pipe closed before the request');
        control.write(JSON.stringify(value) + '\\n');
      };
      const fail = error => { controllerError ||= error instanceof Error ? error.message : String(error); };
      const maybeCloseOutput = () => {
        if (managerOutputClosedByController || !targetStarted || !managerOutputPrefix.startsWith('MANAGER_OUTPUT_READY')) return;
        try {
          managerProcess = processInfo(managerPID);
          anchorProcess = processInfo(anchorPID);
          targetProcess = processInfo(targetPID);
          if (managerProcess.ppid !== controllerPID || managerProcess.pgid !== controllerPID || managerProcess.sid !== controllerPID
            || anchorProcess.ppid !== managerPID || anchorProcess.pgid !== anchorPID || anchorProcess.sid !== controllerPID
            || targetProcess.ppid !== managerPID || targetProcess.pgid !== anchorPID || targetProcess.sid !== controllerPID) {
            throw new Error('manager, anchor, or ready target process identity did not match the owned group');
          }
          managerOutputCloseRequestedAtNs = monotonicNowNs();
          if (managerOutputCloseRequestedAtNs >= executionDeadlineNs) throw new Error('manager output failure began after the original execution deadline');
        } catch (error) {
          fail(error);
          managerOutputCloseRequestedAtNs = monotonicNowNs();
        }
        managerOutputClosedByController = true;
        managerOutputBytesAtClose = managerOutputBytes;
        emit('manager-output-reader-closed', { managerPID, targetPID, groupID, bytes: managerOutputBytesAtClose,
          atNs: managerOutputCloseRequestedAtNs.toString() });
        managerOutput.destroy();
      };
      const handleRecord = record => {
        records.push(record);
        if (record.type === 'anchor-process-started') {
          anchorPID = Number(record.processID);
          emit('anchor-started', { anchorPID, managerPID });
        }
        if (record.type === 'anchor-native-ready') {
          groupID = Number(record.groupID);
          const anchorClockNs = BigInt(record.clockNs);
          const controllerClockNs = monotonicNowNs();
          const translate = deadline => (anchorClockNs + deadline - controllerClockNs).toString();
          send({ type: 'configure-anchor', executionDeadlineNs: translate(executionDeadlineNs),
            hardDeadlineNs: translate(hardDeadlineNs), killDelayMs: 50 });
        }
        if (record.type === 'anchor-ready') send({ type: 'launch-target', binary: nodePath,
          args: ['-e', targetCode], executionDeadlineNs: executionDeadlineNs.toString() });
        if (record.type === 'target-started') {
          targetStarted = record;
          targetPID = Number(record.targetPID);
          maybeCloseOutput();
        }
        if (record.type === 'failure' && failureObservedAtNs === undefined
          && /EPIPE|closed before all output was written/u.test(String(record.message))) failureObservedAtNs = monotonicNowNs();
        if (record.type === 'target-exit') { targetExit = record; targetExitAtNs = monotonicNowNs(); }
        if (record.type === 'target-close') { targetClose = record; targetCloseAtNs = monotonicNowNs(); }
        if (record.type === 'anchor-exit') { anchorExit = record; anchorExitAtNs = monotonicNowNs(); }
        if (record.type === 'anchor-close') { anchorClose = record; anchorCloseAtNs = monotonicNowNs(); }
      };
      const waitUntil = async (read, label, deadlineNs) => {
        while (monotonicNowNs() < deadlineNs) {
          if (controllerError) throw new Error(controllerError);
          const value = read();
          if (value !== undefined) return value;
          await delay(2);
        }
        throw new Error(label + ' exceeded its original monotonic deadline');
      };
      const cleanFailedRun = async () => {
        if (!manager || managerClose) return;
        try { send({ type: 'stop', reason: 'negative-control-test-failure-cleanup' }); } catch {}
        const cleanupDeadlineNs = monotonicNowNs() + 300_000_000n;
        while (!managerClose && monotonicNowNs() < cleanupDeadlineNs) await delay(5);
        if (groupID && groupAlive(groupID)) {
          try { process.kill(-groupID, 'SIGKILL'); } catch {}
        }
        if (!managerClose && managerPID && alive(managerPID)) manager.kill('SIGKILL');
        const finalDeadlineNs = monotonicNowNs() + 500_000_000n;
        while (!managerClose && monotonicNowNs() < finalDeadlineNs) await delay(5);
      };
      try {
        const controllerProcess = processInfo(controllerPID);
        if (controllerProcess.pgid !== controllerPID || controllerProcess.sid !== controllerPID) throw new Error('controller did not own its process group and session');
        emit('controller-started', { controllerPID, controllerProcess, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString() });
        manager = spawn(bunPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor-manager',
          hardDeadlineNs.toString(), String(controllerPID)], {
          detached: false, stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe'],
        });
        managerPID = manager.pid;
        if (!managerPID) throw new Error('production anchor manager did not receive a PID');
        control = manager.stdio[3];
        managerOutput = manager.stdio[4];
        const managerError = manager.stdio[5];
        managerOutput.on('data', chunk => {
          const bytes = Buffer.from(chunk);
          managerOutputBytes += bytes.length;
          managerOutputPrefix = (managerOutputPrefix + bytes.toString('utf8')).slice(0, 256);
          maybeCloseOutput();
        });
        managerOutput.once('end', () => { managerOutputNaturalEnd = true; managerOutputEndAtNs = monotonicNowNs(); });
        managerOutput.once('close', () => { managerOutputCloseObserved = true; });
        managerError.on('data', chunk => { managerStderrPrefix = (managerStderrPrefix + String(chunk)).slice(-2_000); });
        managerError.once('end', () => { managerStderrNaturalEnd = true; });
        managerError.once('close', () => { managerStderrCloseObserved = true; });
        control.setEncoding('utf8');
        control.on('data', chunk => {
          pendingControl += chunk;
          if (pendingControl.length > 1_100_000) { fail('manager control record exceeded its bound'); return; }
          for (;;) {
            const end = pendingControl.indexOf('\\n');
            if (end < 0) break;
            const line = pendingControl.slice(0, end);
            pendingControl = pendingControl.slice(end + 1);
            try { handleRecord(JSON.parse(line)); }
            catch (error) { fail(error); }
          }
        });
        control.on('error', error => fail(error));
        manager.once('error', error => fail(error));
        manager.once('exit', (code, signal) => {
          managerExit = { code, signal };
          managerExitAtNs = monotonicNowNs();
        });
        manager.once('close', (code, signal) => {
          managerClose = { code, signal };
          managerCloseAtNs = monotonicNowNs();
        });
        manager.stdio[6].end();
        emit('manager-started', { managerPID });
        await waitUntil(() => anchorPID && groupID && records.some(record => record.type === 'anchor-ready') ? true : undefined,
          'actual anchor readiness', executionDeadlineNs);
        await waitUntil(() => managerOutputClosedByController ? true : undefined, 'actual manager FD4 reader closure', executionDeadlineNs);
        await waitUntil(() => records.some(record => record.type === 'failure' && /EPIPE|closed before all output was written/u.test(String(record.message))),
          'actual manager FD4 writer failure', executionDeadlineNs);
        await waitUntil(() => managerClose, 'actual manager exit and close', hardDeadlineNs);
        await waitUntil(() => targetExit && targetClose && anchorExit && anchorClose
          && records.some(record => record.type === 'manager-teardown-started'), 'actual target, anchor, and manager teardown witnesses', hardDeadlineNs);
        const groupAbsent = await waitUntil(() => !groupAlive(groupID) ? monotonicNowNs() : undefined, 'actual target process-group absence', hardDeadlineNs);
        await waitUntil(() => !alive(targetPID) && !alive(anchorPID) && !alive(managerPID) ? true : undefined,
          'actual target, anchor, and manager PID absence', hardDeadlineNs);
        const supervisorRetirementReady = records.filter(record => record.type === 'supervisor-retirement-ready');
        if (supervisorRetirementReady.length) throw new Error('direct manager controller received an unproved supervisor-retirement-ready record');
        const finishedAtNs = monotonicNowNs();
        const failureRecords = records.filter(record => record.type === 'failure');
        if (!managerOutputCloseObserved || !managerOutputClosedByController
          || (managerOutputNaturalEnd && managerOutputEndAtNs < managerOutputCloseRequestedAtNs)) {
          throw new Error('manager output end/close did not follow the actual OS reader closure');
        }
        if (!failureRecords.some(record => /EPIPE|closed before all output was written/u.test(String(record.message)))) throw new Error('manager FD4 writer failure fact was missing');
        if (failureObservedAtNs === undefined || failureObservedAtNs >= executionDeadlineNs) throw new Error('manager FD4 failure missed the original execution deadline');
        if (finishedAtNs > hardDeadlineNs || BigInt(groupAbsent) > hardDeadlineNs) throw new Error('manager retirement exceeded the original 700ms hard deadline');
        if (targetExit.signal !== 'SIGKILL' || targetClose.signal !== 'SIGKILL' || anchorExit.signal !== 'SIGKILL' || anchorClose.signal !== 'SIGKILL') {
          throw new Error('actual target and anchor group retirement was not witnessed after the output failure');
        }
        if (managerExit.code !== 0 || managerExit.signal !== null || managerClose.code !== 0 || managerClose.signal !== null) {
          throw new Error('actual manager exit and close did not complete normally');
        }
        const report = {
          event: 'manager-output-result', ok: true, controllerPID, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
          cleanupReservationMs: 200, finishedAtNs: finishedAtNs.toString(), groupAbsentAtNs: String(groupAbsent),
          managerOutputCloseRequestedAtNs: managerOutputCloseRequestedAtNs.toString(),
          managerPID, managerProcess, anchorPID, anchorProcess, groupID, targetPID, targetProcess,
          targetExit, targetExitAtNs: String(targetExitAtNs), targetClose, targetCloseAtNs: String(targetCloseAtNs),
          anchorExit, anchorExitAtNs: String(anchorExitAtNs), anchorClose, anchorCloseAtNs: String(anchorCloseAtNs),
          managerExit, managerExitAtNs: String(managerExitAtNs), managerClose, managerCloseAtNs: String(managerCloseAtNs),
          managerOutputBytes, managerOutputBytesAtClose, managerOutputPrefix, managerOutputClosedByController,
          managerOutputNaturalEnd, managerOutputEndAtNs: managerOutputEndAtNs?.toString(), managerOutputCloseObserved,
          managerStderrNaturalEnd, managerStderrCloseObserved, failureObservedAtNs: String(failureObservedAtNs), failureRecords, supervisorRetirementReadyCount: supervisorRetirementReady.length, records,
        };
        emit('manager-output-result', report);
      } catch (error) {
        await cleanFailedRun();
        emit('manager-output-result', { ok: false, controllerPID, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
          failure: error instanceof Error ? error.message : String(error), managerPID, managerExit, managerClose,
          managerOutputNaturalEnd, managerOutputEndAtNs: managerOutputEndAtNs?.toString(), managerOutputCloseObserved,
          managerOutputClosedByController, managerOutputPrefix,
          managerOutputBytes, managerStderrPrefix, targetExit, targetClose, anchorExit, anchorClose, anchorPID, groupID, targetPID, records });
        process.exitCode = 1;
      }
    })().catch(error => {
      process.stderr.write(String(error) + '\\n');
      process.exitCode = 1;
    });`;
    const controller = spawn(bunPath, ['--no-env-file', '-e', controllerSource], {
      cwd: repositoryRoot, env: fixture.environment, detached: false, stdio: ['ignore', 'pipe', 'pipe'],
    });
    assert.ok(controller.pid);
    let pending = '';
    let controllerStderr = '';
    let controllerClose: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    const controllerRecords: Array<Record<string, any>> = [];
    const closed = new Promise<void>(resolve => controller.once('close', (code, signal) => {
      controllerClose = { code, signal };
      resolve();
    }));
    controller.stdout!.on('data', chunk => {
      pending += String(chunk);
      for (;;) {
        const end = pending.indexOf('\n');
        if (end < 0) break;
        const line = pending.slice(0, end);
        pending = pending.slice(end + 1);
        try { controllerRecords.push(JSON.parse(line) as Record<string, any>); }
        catch { controllerStderr += `invalid controller record: ${line.slice(0, 200)}\n`; }
      }
    });
    controller.stderr!.on('data', chunk => { controllerStderr = (controllerStderr + String(chunk)).slice(-8_000); });
    let managerPID: number | undefined;
    let anchorGroupID: number | undefined;
    const waitForControllerRecord = async (read: () => Record<string, any> | undefined, timeoutMs: number, message: string) => {
      try { return await waitForValue(read, timeoutMs, message); }
      catch (error) { throw new Error(`${message}: ${String(error)}; stderr=${controllerStderr}; records=${JSON.stringify(controllerRecords)}`, { cause: error }); }
    };
    try {
      const started = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'controller-started'),
        1_000, 'detached manager controller did not start');
      assert.equal(started.controllerPID, controller.pid);
      assert.equal(started.controllerProcess.pgid, controller.pid);
      assert.equal(started.controllerProcess.sid, controller.pid);
      const managerStarted = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'manager-started'),
        1_000, 'production manager did not start in its controller session');
      managerPID = Number(managerStarted.managerPID);
      const result = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'manager-output-result'),
        2_000, 'manager output failure controller did not return its direct lifecycle witnesses');
      const anchorStarted = await waitForValue(() => controllerRecords.find(record => record.event === 'anchor-started'),
        100, 'production manager did not start its actual anchor');
      anchorGroupID = Number(anchorStarted.anchorPID);
      await waitForValue(() => controllerClose, 1_000, 'manager output controller did not close after reporting');
      assert.equal(controllerClose?.code, 0, JSON.stringify({ controllerClose, result, controllerStderr }));
      assert.equal(controllerClose?.signal, null);
      assert.equal(result.ok, true, JSON.stringify({ result, controllerStderr, controllerRecords }));
      assert.equal(result.controllerPID, controller.pid);
      assert.equal(Number(result.managerPID), managerPID);
      assert.equal(Number(result.anchorPID), anchorGroupID);
      assert.equal(Number(result.groupID), anchorGroupID);
      assert.equal(result.cleanupReservationMs, 200);
      const startedAtNs = BigInt(result.startedAtNs);
      const executionDeadlineNs = BigInt(result.executionDeadlineNs);
      const hardDeadlineNs = BigInt(result.hardDeadlineNs);
      assert.equal(hardDeadlineNs - startedAtNs, 700_000_000n);
      assert.equal(executionDeadlineNs, hardDeadlineNs - 200_000_000n);
      assert.ok(BigInt(result.failureObservedAtNs) < executionDeadlineNs);
      assert.ok(BigInt(result.managerCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.targetCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.anchorCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.groupAbsentAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.finishedAtNs) <= hardDeadlineNs);
      assert.ok(result.managerOutputBytesAtClose > 0);
      assert.match(result.managerOutputPrefix, /^MANAGER_OUTPUT_READY/u);
      assert.equal(result.managerOutputClosedByController, true);
      if (result.managerOutputNaturalEnd) assert.ok(BigInt(result.managerOutputEndAtNs) >= BigInt(result.managerOutputCloseRequestedAtNs));
      assert.equal(result.managerOutputCloseObserved, true);
      assert.equal(result.managerStderrNaturalEnd, true);
      assert.equal(result.managerStderrCloseObserved, true);
      assert.ok(result.failureRecords.some((record: Record<string, unknown>) => /EPIPE|closed before all output was written/u.test(String(record.message))));
      assert.equal(result.targetExit.signal, 'SIGKILL');
      assert.equal(result.targetClose.signal, 'SIGKILL');
      assert.equal(result.anchorExit.signal, 'SIGKILL');
      assert.equal(result.anchorClose.signal, 'SIGKILL');
      assert.equal(result.managerExit.code, 0);
      assert.equal(result.managerExit.signal, null);
      assert.equal(result.managerClose.code, 0);
      assert.equal(result.managerClose.signal, null);
      assert.equal(result.managerProcess.ppid, controller.pid);
      assert.equal(result.managerProcess.pgid, controller.pid);
      assert.equal(result.managerProcess.sid, controller.pid);
      assert.equal(result.anchorProcess.ppid, managerPID);
      assert.equal(result.anchorProcess.pgid, anchorGroupID);
      assert.equal(result.anchorProcess.sid, controller.pid);
      assert.equal(result.targetProcess.ppid, managerPID);
      assert.equal(result.targetProcess.pgid, anchorGroupID);
      assert.equal(result.targetProcess.sid, controller.pid);
      assert.equal(result.records.some((record: Record<string, unknown>) => record.type === 'manager-teardown-started'), true);
      assert.equal(result.records.some((record: Record<string, unknown>) => record.type === 'supervisor-retirement-ready'), false);
      assert.equal(result.supervisorRetirementReadyCount, 0);
      assert.equal(pidExists(managerPID), false);
      assert.equal(pidExists(result.targetPID), false);
      assert.equal(pidExists(result.anchorPID), false);
      assert.equal(await waitForGroupAbsent(anchorGroupID, 100), true);
      assert.equal(pidExists(controller.pid), false);
      assert.equal(await waitForGroupAbsent(controller.pid, 100), true);
    } finally {
      if (anchorGroupID !== undefined && processGroupExists(anchorGroupID)) {
        try { process.kill(-anchorGroupID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForGroupAbsent(anchorGroupID, 1_000), true);
      }
      if (controller.exitCode === null && controller.signalCode === null) {
        try { process.kill(-controller.pid!, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        if (controller.exitCode === null && controller.signalCode === null) controller.kill('SIGKILL');
        await Promise.race([closed, new Promise(resolve => setTimeout(resolve, 1_000))]);
      }
      if (managerPID !== undefined && pidExists(managerPID)) {
        try { process.kill(managerPID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForPidAbsent(managerPID, 1_000), true);
      }
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('owned process retirement keeps target, relay and caller witnesses independent for full output', async () => {
    assert.notEqual(process.platform, 'win32');
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const expectedStdout = Buffer.alloc(128_000, 83);
    const expectedStderr = 'source-bound stderr payload\\n';
    const source = `process.stdout.write(Buffer.from(${JSON.stringify(expectedStdout.toString('base64'))},'base64'));process.stderr.write(${JSON.stringify(expectedStderr)});`;
    const result = await boundedCommand(nodePath, ['-e', source], 5_000, { maxBytes: 140_000, cleanupReservationMs: 1_000 });
    assert.equal(result.stdout, expectedStdout.toString('utf8'));
    assert.equal(result.stderr, expectedStderr);
    assert.equal(result.targetStdoutNaturalEnd, true);
    assert.equal(result.targetStdoutCloseObserved, true);
    assert.equal(result.targetStderrNaturalEnd, true);
    assert.equal(result.targetStderrCloseObserved, true);
    assert.equal(result.managerStdoutNaturalEnd, true);
    assert.equal(result.managerStdoutCloseObserved, true);
    assert.equal(result.managerStderrNaturalEnd, true);
    assert.equal(result.managerStderrCloseObserved, true);
    assert.equal(result.callerStdoutNaturalEnd, true);
    assert.equal(result.callerStdoutCloseObserved, true);
    assert.equal(result.callerStderrNaturalEnd, true);
    assert.equal(result.callerStderrCloseObserved, true);
    assert.equal(result.targetOutputRelayHealthy, true);
    assert.equal(result.ownedProcessesExited, true);
  });
  test('owned process stop before target start retires without dispatching the target', async () => {
    assert.notEqual(process.platform, 'win32');
    const startedPath = join(tmpdir(), `owned-stop-before-target-${process.pid}`);
    const owned = spawnOwnedProcess(process.execPath, ['-e', `require('node:fs').writeFileSync(${JSON.stringify(startedPath)},'started')`], {
      timeoutMs: 4_000, cleanupReservationMs: 1_000,
    });
    try {
      await owned.ready;
      owned.stop('test-stop-before-target-start');
      assert.equal(await owned.started, undefined);
      const retirement = await owned.retire('test-stop-before-target-start');
      assert.equal(await owned.targetExit, undefined);
      assert.equal(await owned.targetClose, undefined);
      assert.equal(retirement.targetDispatchRequested, false);
      assert.equal(retirement.targetProcessCreated, false);
      assert.equal(retirement.targetNoChildObserved, false);
      assert.equal(retirement.targetExecConfirmed, false);
      assert.equal(retirement.targetExitObserved, false);
      assert.equal(retirement.targetCloseObserved, false);
      assert.equal(retirement.anchorExitObserved, true);
      assert.equal(retirement.anchorCloseObserved, true);
      assert.equal(retirement.groupAbsent, true);
      assert.equal(retirement.supervisorExitObserved, true);
      assert.equal(retirement.supervisorCloseObserved, true);
      assert.equal(existsSync(startedPath), false);
    } finally {
      if (!owned.isSupervisorClosed) await owned.retire('test-stop-before-target-start-cleanup').catch(() => undefined);
      await rm(startedPath, { force: true });
    }
  });
  test('owned process retired immediately after start stops during anchor startup well before its deadline', async () => {
    assert.notEqual(process.platform, 'win32');
    const startedPath = join(tmpdir(), `owned-retire-after-start-${process.pid}`);
    const owned = spawnOwnedProcess(process.execPath, ['-e', `require('node:fs').writeFileSync(${JSON.stringify(startedPath)},'started');setInterval(()=>{},1000)`], {
      timeoutMs: 15_000, cleanupReservationMs: 1_000,
    });
    try {
      await owned.ready;
      owned.start();
      const retiredFrom = performance.now();
      const retirement = await owned.retire('test-retire-after-start');
      const elapsedMs = Math.round(performance.now() - retiredFrom);
      const details = JSON.stringify({ elapsedMs, retirement });
      assert.ok(elapsedMs < 5_000, details);
      assert.equal(await owned.stopCause, 'test-retire-after-start', details);
      assert.equal(await owned.started, undefined, details);
      assert.equal(retirement.targetDispatchRequested, false, details);
      assert.equal(retirement.targetProcessCreated, false, details);
      assert.equal(retirement.anchorExitObserved, true, details);
      assert.equal(retirement.anchorCloseObserved, true, details);
      assert.equal(retirement.groupAbsent, true, details);
      assert.equal(retirement.inputClosedObserved, true, details);
      assert.equal(retirement.supervisorExitObserved, true, details);
      assert.equal(retirement.supervisorCloseObserved, true, details);
      assert.equal(retirement.supervisorExitCode, 0, details);
      assert.equal(retirement.supervisorSignal, null, details);
      assert.equal(existsSync(startedPath), false, details);
    } finally {
      if (!owned.isSupervisorClosed) await owned.retire('test-retire-after-start-cleanup').catch(() => undefined);
      await rm(startedPath, { force: true });
    }
  });
  test('owned process caller output break cannot become healthy target EOF or retirement', async () => {
    assert.notEqual(process.platform, 'win32');
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const targetCode = "process.stdout.write('FIRST');setTimeout(()=>process.stdout.write(Buffer.alloc(65536,66)),100);setInterval(()=>{},1000)";
    const owned = spawnOwnedProcess(nodePath, ['-e', targetCode], { timeoutMs: 4_000, cleanupReservationMs: 1_000 });
    const callerClose = new Promise<void>(resolve => owned.stdout.once('close', resolve));
    owned.stdout.once('data', () => owned.stdout.destroy());
    try {
      await owned.ready;
      owned.start();
      const binding = await owned.started;
      assert.ok(binding);
      await owned.targetClose;
      const retirement = await owned.retire('caller-output-break-negative-control');
      await callerClose;
      assert.equal(retirement.stdoutNaturalEnd, true);
      assert.equal(retirement.stdoutCloseObserved, true);
      assert.equal(retirement.managerStdoutNaturalEnd, true);
      assert.equal(retirement.managerStdoutCloseObserved, true);
      assert.equal(retirement.callerStdoutCloseObserved, true);
      assert.equal(retirement.targetOutputRelayHealthy, false);
      assert.equal(retirement.supervisorStdoutCloseObserved, true);
      assert.equal(retirement.groupAbsent, true);
    } finally {
      if (!owned.isSupervisorClosed) {
        await owned.retire('caller-output-break-negative-control-cleanup').catch(() => undefined);
        owned.release();
      }
    }
  });
  test('HOST lease parent loss retires generic command groups', async () => {
    if (process.platform !== 'darwin') return 'independent supervisor wait-status witness requires Darwin EVFILT_PROC NOTE_EXITSTATUS';
    const fixture = await harness.createFixture();
    const fakeAdbEnvironment = await installFakeAdbLifetimeBackstop(fixture);
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const cases = [
      { name: 'node-live-term', kind: 'node', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: false },
      { name: 'node-leader-exited-kill', kind: 'node', signal: 'SIGKILL' as const, leaderExited: true, stoppedSupervisor: false },
      { name: 'node-stopped-supervisor-term', kind: 'node', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: true },
      { name: 'adb-shaped-term', kind: 'adb', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: false }
    ];
    try {
      await verifyDarwinProcessExitStatusControls(nodePath, fixture.environment);
      for (const control of cases) {
      const readyPath = join(fixture.root, `${control.name}-target-ready`);
      const descendantReadyPath = join(fixture.root, `${control.name}-descendant-ready`);
      const exitTriggerPath = join(fixture.root, `${control.name}-exit-trigger`);
      const leaderExitedPath = join(fixture.root, `${control.name}-leader-exited`);
      const postLossTriggerPath = join(fixture.root, `${control.name}-post-loss-trigger`);
      const latePath = join(fixture.root, `${control.name}-late`);
      const actorSafetyMs = control.stoppedSupervisor ? 10_000 : 500;
      const targetSafetyMs = control.stoppedSupervisor ? 10_000 : 7_000;
      const descendantCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(descendantReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{if(!fs.existsSync(${JSON.stringify(postLossTriggerPath)}))return;fs.rmSync(${JSON.stringify(postLossTriggerPath)},{force:true});process.stdout.write('POST_LOSS_EPIPE_STDOUT\\n');process.stderr.write('POST_LOSS_EPIPE_STDERR\\n')},5);setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},${actorSafetyMs});setTimeout(()=>process.exit(0),${targetSafetyMs});`;
      const targetCode = `const fs=require('node:fs');const {spawn}=require('node:child_process');process.on('SIGTERM',()=>{});const child=spawn(${JSON.stringify(nodePath)},['-e',${JSON.stringify(descendantCode)}],{stdio:'inherit'});const ready=setInterval(()=>{if(!fs.existsSync(${JSON.stringify(descendantReadyPath)}))return;clearInterval(ready);fs.writeFileSync(${JSON.stringify(readyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,execPath:process.execPath,execArgv:process.execArgv}));${control.leaderExited ? `const trigger=setInterval(()=>{if(!fs.existsSync(${JSON.stringify(exitTriggerPath)}))return;clearInterval(trigger);fs.writeFileSync(${JSON.stringify(leaderExitedPath)},'exited');process.exit(0)},5);` : 'setInterval(()=>{},1000);'}},5);setTimeout(()=>process.exit(0),${targetSafetyMs});`;
      const binary = control.kind === 'adb' ? 'adb' : nodePath;
      const args = control.kind === 'adb'
        ? ['-s', 'emulator-5554', 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1']
        : ['-e', targetCode];
      const commandTimeoutMs = control.stoppedSupervisor ? 5_000 : 8_000;
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {boundedCommand}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/bounded-process.ts'))});const pending=boundedCommand(${JSON.stringify(binary)},${JSON.stringify(args)},${commandTimeoutMs},{maxBytes:4096,cleanupReservationMs:1000,observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})});pending.then(()=>{},()=>{});process.stdout.write(JSON.stringify({event:'command-dispatched'})+'\\n');setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let descendantPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      let anchorObserver: DarwinProcessExitObserver | undefined;
      try {
        caller = startLifetimeCaller(ownerSource, control.kind === 'adb' ? fakeAdbEnvironment : fixture.environment);
        assert.ok(caller.child.pid);
        await caller.waitFor('command-dispatched', 3_000);
        const observedSupervisor = await waitForValue(() => lifetimeSupervisor(caller!.child.pid!), 3_000,
          `owned supervisor was not spawned for ${control.name}`);
        supervisorPID = observedSupervisor.pid;
        assert.equal(observedSupervisor.ppid, caller.child.pid);
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        assert.equal(pidExists(supervisorPID), true, `supervisor disappeared before its kernel status observer was armed (${control.name})`);
        if (control.kind === 'adb') {
          await waitForValue(() => {
            try { return readFileSync(fixture.log, 'utf8').includes(args.slice(2).join(' ')) ? true : undefined; } catch { return undefined; }
          }, 3_000, 'fake adb did not enter its logcat handler');
        } else {
          const targetIdentity = await waitForFileValue(readyPath, text => JSON.parse(text) as { pid: number; ppid: number; execPath: string; execArgv: string[] }, 4_000);
          const descendantIdentity = await waitForFileValue(descendantReadyPath, text => JSON.parse(text) as { pid: number; ppid: number }, 2_000);
          targetPID = targetIdentity.pid;
          descendantPID = descendantIdentity.pid;
          assert.equal(targetIdentity.execPath, nodePath);
          assert.deepEqual(targetIdentity.execArgv, ['-e', targetCode]);
          assert.equal(descendantIdentity.ppid, targetIdentity.pid);
          assert.equal(pidExists(descendantIdentity.pid), true);
        }
        const group = await waitForValue(() => lifetimeOwnedGroup(caller!.child.pid!), 3_000, 'owned supervisor and live group anchor were not observed');
        assert.equal(group.supervisor.pid, supervisorPID);
        groupID = group.anchor.pid;
        assert.equal(group.anchor.pgid, group.anchor.pid);
        assert.ok(group.target);
        assert.equal(group.target.ppid, group.manager.pid);
        assert.equal(group.target.pgid, group.anchor.pid);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        if (targetPID !== undefined) assert.equal(group.target.pid, targetPID);
        if (descendantPID !== undefined) {
          const descendant = lifetimeProcessTable().find(record => record.pid === descendantPID);
          assert.ok(descendant);
          assert.equal(descendant.ppid, targetPID);
          assert.equal(descendant.pgid, group.anchor.pid);
        }
        if (control.leaderExited) {
          await writeFile(exitTriggerPath, 'exit');
          await waitForFileValue(leaderExitedPath, text => text, 1_000);
          assert.equal(await waitForPidAbsent(targetPID!, 1_000), true, 'generic command leader must already be gone');
          assert.equal(processGroupExists(group.anchor.pid), true, 'live descendant and anchor retain the group at caller loss');
        }
        if (control.stoppedSupervisor) {
          assert.equal(process.kill(supervisorPID, 'SIGSTOP'), true);
          await waitForValue(() => {
            try { return execFileSync('ps', ['-p', String(supervisorPID), '-o', 'state='], { encoding: 'utf8' }).trim().startsWith('T') ? true : undefined; }
            catch { return undefined; }
          }, 1_000, 'supervisor did not enter the stopped state');
        }
        assert.equal(caller.child.kill(control.signal), true);
        if (control.stoppedSupervisor) {
          const closed = await caller.waitForClose(2_000);
          assert.equal(closed.signal, control.signal);
          assert.equal(processGroupExists(groupID), true, 'stopped supervisor must retain its live target group until the anchor deadline');
          anchorObserver = await observeDarwinProcessExit(groupID);
          const anchorDeadlineNs = hardDeadlineNs - 500_000_000n;
          const anchorWaitMs = Number((anchorDeadlineNs - monotonicNowNs()) / 1_000_000n);
          assert.ok(anchorWaitMs > 0, `anchor deadline already elapsed while supervisor remained stopped: ${JSON.stringify({
            anchorDeadlineNs: anchorDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
            nowNs: monotonicNowNs().toString(), caller: caller.observerSupervisor(),
          })}`);
          const anchorStatus = anchorObserver.wait(anchorWaitMs);
          assert.equal(anchorStatus.signal, osConstants.signals.SIGKILL);
          await waitForValue(() => lifetimeLiveGroupMembers(groupID!).length === 0 ? true : undefined,
            Math.max(1, Number((anchorDeadlineNs - monotonicNowNs()) / 1_000_000n)),
            'anchor deadline did not retire all live group members while its supervisor remained stopped');
          assert.equal(pidExists(supervisorPID), true);
          assert.match(execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim(), /^T/u);
          assert.equal(existsSync(latePath), false, 'synthetic actor safety expiry cannot establish anchor retirement');
          assert.ok(monotonicNowNs() < anchorDeadlineNs, 'anchor retirement must precede the original anchor deadline');
          assert.equal(process.kill(supervisorPID, 'SIGCONT'), true);
          await waitForValue(() => {
            try {
              const state = execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim();
              return state && !state.startsWith('T') ? state : undefined;
            } catch { return undefined; }
          }, Math.max(1, Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n)), 'supervisor remained stopped after SIGCONT');
          assert.equal(await waitForGroupAbsent(groupID, Math.max(1, Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n))), true,
            'resumed supervisor must reap the already-retired anchor before stream checks');
        }
        if (control.kind === 'node' && !control.stoppedSupervisor) {
          await waitForValue(() => caller!.child.signalCode === control.signal ? true : undefined, 1_000, 'caller process did not exit before the post-loss write');
          await writeFile(postLossTriggerPath, 'write');
          await waitForValue(() => caller!.observerOutput().includes('POST_LOSS_EPIPE_STDOUT')
            && caller!.observerStderr().includes('POST_LOSS_EPIPE_STDERR') ? true : undefined,
          1_000, 'supervisor did not drain both streams after downstream closure');
          const pipeEvents = caller.observerSupervisor().split('\n').map((line) => {
            try { return JSON.parse(line) as { type?: string; stream?: string }; } catch { return undefined; }
          });
          assert.ok(pipeEvents.some(event => event?.type === 'downstream-epipe' && event.stream === 'stdout'));
          assert.ok(pipeEvents.some(event => event?.type === 'downstream-epipe' && event.stream === 'stderr'));
        }
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, control.signal, hardDeadlineNs, control.name);
        if (control.kind === 'adb') {
          const diagnostic = await waitForValue(() => {
            try {
              const value = JSON.parse(readFileSync(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
              return value.children.flatMap(child => child.events).find(event => event.stage === 'fake-child-exit' && event.childExitCategory === 'signal');
            } catch { return undefined; }
          }, 1_000, 'fake adb did not record its actual signaled exit');
          assert.equal(diagnostic.stage, 'fake-child-exit');
        }
        await new Promise(resolve => setTimeout(resolve, 600));
        assert.equal(existsSync(latePath), false, `caller-loss safety actor wrote late output for ${control.name}`);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 1_000), true);
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      } finally {
        anchorObserver?.close();
        statusObserver?.close();
        if (control.stoppedSupervisor && supervisorPID !== undefined && pidExists(supervisorPID)) {
          try { process.kill(supervisorPID, 'SIGCONT'); }
          catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        }
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 8_000), true);
      }
      }
      await verifyObserverDescriptorOwnership(fixture.root, nodePath);
      await verifyClosedTargetInput(fixture.root, nodePath);
      await verifyFakeAdbLifetimeBackstop(fakeAdbEnvironment, fixture);
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('HOST lease initialization and release interruption stay owned', async () => {
    if (process.platform !== 'darwin') return 'independent supervisor wait-status witness requires Darwin EVFILT_PROC NOTE_EXITSTATUS';
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    assert.equal(callerDurationToRuntime(5_000, 10_000_000_000n, 1_000_000_000n, 2_500_000_000n), 13_500_000_000n);
    assert.equal(callerDeadlineToRuntime(6_000_000_000n, 10_000_000_000n, 2_500_000_000n), 13_500_000_000n);
    const conversionStartedAt = monotonicNowNs();
    const conversionDeadline = conversionStartedAt + 1_000_000_000n;
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 200);
    const callerAfterConversion = monotonicNowNs();
    const translatedDeadline = callerDeadlineToRuntime(conversionDeadline, conversionStartedAt, callerAfterConversion);
    assert.ok(translatedDeadline <= conversionDeadline - 150_000_000n,
      'a delayed native monotonic conversion must consume the original allowance');
    const delayedRuntimeDirectory = join(fixture.root, 'delayed-runtime-bin');
    await mkdir(delayedRuntimeDirectory, { recursive: true });
    const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
    await writeFile(join(delayedRuntimeDirectory, 'bun'), `#!/bin/sh\n/bin/sleep 0.65\nexec ${quote(process.execPath)} "$@"\n`, { mode: 0o700 });
    const interruptedRuntimeDirectory = join(fixture.root, 'interrupted-runtime-bin');
    await mkdir(interruptedRuntimeDirectory, { recursive: true });
    await writeFile(join(interruptedRuntimeDirectory, 'bun'), `#!/bin/sh\n/bin/sleep 1.25\nexec ${quote(process.execPath)} "$@"\n`, { mode: 0o700 });
    const delayedRuntimeEnvironment = { ...fixture.environment, PATH: `${delayedRuntimeDirectory}:${fixture.environment.PATH}` };
    const interruptedRuntimeEnvironment = { ...fixture.environment, PATH: `${interruptedRuntimeDirectory}:${fixture.environment.PATH}` };
    try {
      for (const mode of ['initialization', 'release', 'unconfigured-delayed-startup', 'interrupted-unconfigured-startup'] as const) {
      const targetReadyPath = join(fixture.root, `${mode}-handoff-target-ready`);
      const releaseTriggerPath = join(fixture.root, `${mode}-handoff-trigger`);
      const targetCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,execPath:process.execPath,execArgv:process.execArgv}));setInterval(()=>{if(!fs.existsSync(${JSON.stringify(releaseTriggerPath)}))return;fs.rmSync(${JSON.stringify(releaseTriggerPath)},{force:true});process.stdout.write('RELEASED\\n');process.exit(0)},5);setTimeout(()=>process.exit(0),6000);`;
      const timeoutMs = mode === 'unconfigured-delayed-startup' ? 1_600 : 8_000;
      const cleanupReservationMs = mode === 'unconfigured-delayed-startup' ? 500 : 1_000;
      const lifecycleWitness = `const mark=(event,value=null)=>process.stdout.write(JSON.stringify({event,atNs:process.hrtime.bigint().toString(),value})+'\\n');owned.targetExit.then(value=>mark('target-exit',value));owned.targetClose.then(value=>mark('target-close',value));owned.retirement.then(value=>mark('retirement',value));owned.failure.then(value=>mark('failure',String(value)));owned.supervisor.once('exit',(code,signal)=>mark('supervisor-exit',{code,signal}));owned.supervisor.once('close',(code,signal)=>mark('supervisor-close',{code,signal}));owned.stdout.once('end',()=>mark('stdout-end'));owned.stdout.once('close',()=>mark('stdout-close'));owned.stderr.once('end',()=>mark('stderr-end'));owned.stderr.once('close',()=>mark('stderr-close'));`;
      const handoff = mode === 'initialization'
        ? "await owned.ready;process.stdout.write(JSON.stringify({event:'initialized',atNs:process.hrtime.bigint().toString()})+'\\n');"
        : ['unconfigured-delayed-startup', 'interrupted-unconfigured-startup'].includes(mode)
          ? "await owned.inputClosed;process.stdout.write(JSON.stringify({event:'supervisor-spawned',atNs:process.hrtime.bigint().toString(),pid:owned.supervisor.pid})+'\\n');Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,4000);"
          : "await owned.ready;owned.start();const binding=await owned.started;process.stdout.write(JSON.stringify({event:'target-started',atNs:process.hrtime.bigint().toString(),binding})+'\\n');const exit=await owned.targetExit;const close=await owned.targetClose;owned.stop('release-handoff');const evidence=await owned.retirement;process.stdout.write(JSON.stringify({event:'release-handoff',atNs:process.hrtime.bigint().toString(),exit,close,evidence})+'\\n');";
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {spawnOwnedProcess}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))});const options={timeoutMs:${timeoutMs},cleanupReservationMs:${cleanupReservationMs},observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})};const owned=spawnOwnedProcess(${JSON.stringify(nodePath)},['-e',${JSON.stringify(targetCode)}],options);${lifecycleWitness}${handoff}setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      try {
        const delayedStartupStartedAt = ['unconfigured-delayed-startup', 'interrupted-unconfigured-startup'].includes(mode) ? monotonicNowNs() : undefined;
        const callerEnvironment = mode === 'unconfigured-delayed-startup' ? delayedRuntimeEnvironment
          : mode === 'interrupted-unconfigured-startup' ? interruptedRuntimeEnvironment : fixture.environment;
        caller = startLifetimeCaller(ownerSource, callerEnvironment);
        assert.ok(caller.child.pid);
        if (mode === 'interrupted-unconfigured-startup') {
          const spawned = await caller.waitFor('supervisor-spawned', 1_000);
          supervisorPID = Number(spawned.pid);
          assert.ok(Number.isSafeInteger(supervisorPID) && supervisorPID > 0);
          statusObserver = await observeDarwinProcessExit(supervisorPID);
          const startupProcess = lifetimeProcessTable().find(record => record.pid === supervisorPID);
          assert.ok(startupProcess);
          assert.ok(startupProcess.command.startsWith('/bin/sh '));
          assert.ok(startupProcess.command.includes('interrupted-runtime-bin/bun'));
          assert.equal(caller.observerSupervisor().includes('supervisor-runtime-started'), false);
          assert.equal(caller.child.kill('SIGTERM'), true);
          await assertCallerLossRetired(caller, statusObserver, supervisorPID, undefined, 'SIGTERM', delayedStartupStartedAt! + 3_000_000_000n);
          const elapsedMs = Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000;
          assert.ok(elapsedMs < 3_000, `interrupted unconfigured startup exceeded its finite owner deadline: ${elapsedMs.toFixed(1)}ms`);
          continue;
        }
        if (mode === 'unconfigured-delayed-startup') {
          const spawned = await caller.waitFor('supervisor-spawned', 1_000);
          supervisorPID = Number(spawned.pid);
          assert.ok(Number.isSafeInteger(supervisorPID) && supervisorPID > 0);
          statusObserver = await observeDarwinProcessExit(supervisorPID);
          let runtimeStarted: Record<string, unknown>;
          try {
            runtimeStarted = await waitForValue(() => {
              const record = caller!.observerSupervisor().split('\n').map(line => {
                try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
              }).find(value => value?.type === 'supervisor-runtime-started');
              return record || undefined;
            }, 2_000, 'delayed supervisor runtime did not start while its caller remained unconfigured');
          } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; observer=${caller.observerSupervisor()}; stderr=${caller.stderr()}; process=${JSON.stringify(process)}`, { cause: error });
          }
          assert.equal(runtimeStarted.pid, supervisorPID);
          let armed: Record<string, unknown>;
          try {
            armed = await waitForValue(() => {
              const records = caller!.observerSupervisor().split('\n').map(line => {
                try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
              });
              return records.find(value => value?.type === 'supervisor-deadlines-armed') || undefined;
            }, 1_000, 'supervisor did not expose its original startup deadline through the live observer');
          } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; observer=${caller.observerSupervisor()}; stderr=${caller.stderr()}; process=${JSON.stringify(process)}`, { cause: error });
          }
          const remainingMs = Number((BigInt(String(armed.hardDeadlineNs)) - BigInt(String(armed.clockNs))) / 1_000_000n);
          assert.ok(remainingMs > 0 && remainingMs < 1_200, `delayed startup reset the hard allowance: ${remainingMs}ms remain`);
          assert.equal(caller.child.kill('SIGSTOP'), true);
          let status: DarwinProcessExitStatus;
          try { status = statusObserver.wait(3_000); } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; elapsedMs=${(Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000).toFixed(1)}; supervisorAlive=${pidExists(supervisorPID)}; process=${JSON.stringify(process)}; observer=${caller.observerSupervisor()}`, { cause: error });
          }
          assert.equal(status.pid, supervisorPID);
          assert.equal(status.waitStatus, 0);
          assert.equal(status.exitCode, 0);
          assert.equal(status.signal, 0);
          const deadlineEvent = await waitForValue(() => caller!.observerSupervisor().split('\n').map(line => {
            try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
          }).find(record => record?.type === 'supervisor-hard-deadline'), 1_000, 'supervisor hard deadline was not observed');
          assert.equal(deadlineEvent.retirementSent, true);
          assert.equal(deadlineEvent.inputClosed, true);
          assert.equal(deadlineEvent.targetSpawned, false);
          assert.ok(BigInt(String(deadlineEvent.clockNs)) >= BigInt(String(armed.hardDeadlineNs)));
          const elapsedMs = Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000;
          assert.ok(elapsedMs <= timeoutMs + 350, `unconfigured supervisor exceeded the original ${timeoutMs}ms hard deadline: ${elapsedMs.toFixed(1)}ms`);
          assert.equal(caller.child.kill('SIGCONT'), true);
          const closed = await terminateLifetimeCaller(caller, 'SIGKILL');
          assert.equal(closed.signal, 'SIGKILL');
          assert.equal(await waitForPidAbsent(supervisorPID, 1_000), true);
          continue;
        }
        if (mode === 'initialization') {
          await caller.waitFor('initialized', 3_000);
          const supervisor = await waitForValue(() => lifetimeSupervisor(caller!.child.pid!), 2_000, 'unconfigured supervisor was not observed');
          supervisorPID = supervisor.pid;
        } else {
          const started = await caller.waitFor('target-started', 4_000);
          const binding = started.binding as { targetPID: number; spawnParentPID: number; groupID: number; anchorPID: number };
          const identity = await waitForFileValue(targetReadyPath, text => JSON.parse(text) as { pid: number; ppid: number; execPath: string; execArgv: string[] }, 2_000);
          targetPID = identity.pid;
          assert.equal(identity.pid, binding.targetPID);
          assert.equal(identity.ppid, binding.spawnParentPID);
          assert.equal(identity.execPath, nodePath);
          assert.deepEqual(identity.execArgv, ['-e', targetCode]);
          const group = await waitForValue(() => lifetimeOwnedGroup(caller!.child.pid!), 2_000, 'release handoff did not retain a live anchor and target group');
          supervisorPID = group.supervisor.pid;
          groupID = group.anchor.pid;
          assert.equal(group.manager.pid, binding.spawnParentPID);
          assert.equal(group.anchor.pid, binding.groupID);
          assert.equal(group.target?.pid, targetPID);
          assert.equal(processGroupExists(groupID), true);
          await writeFile(releaseTriggerPath, 'release');
          const release = await caller.waitFor('release-handoff', 3_000);
          const evidence = release.evidence as RetirementEvidence;
          assert.equal((release.exit as { code: number }).code, 0);
          assert.equal(evidence.targetExitObserved, true);
          assert.equal(evidence.targetCloseObserved, true);
          assert.equal(evidence.anchorExitObserved, true);
          assert.equal(evidence.anchorCloseObserved, true);
          assert.equal(evidence.groupAbsent, true);
          assert.equal(evidence.stdoutNaturalEnd, true);
          assert.equal(evidence.stderrNaturalEnd, true);
          assert.equal(processGroupExists(groupID), false);
          assert.equal(pidExists(supervisorPID), true, 'supervisor remains live during its release handoff');
          assert.deepEqual(caller.observerStreams(), {
            stdoutEnded: false, stdoutClosed: false, stderrEnded: false, stderrClosed: false,
            supervisorEnded: false, supervisorClosed: false, childCloseObserved: false,
          });
        }
        const expectedSignal = mode === 'initialization' ? 'SIGTERM' : 'SIGKILL';
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        assert.equal(caller.child.kill(expectedSignal), true);
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, expectedSignal, hardDeadlineNs);
      } finally {
        statusObserver?.close();
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) {
          if (mode === 'unconfigured-delayed-startup') caller.child.kill('SIGCONT');
          await terminateLifetimeCaller(caller, mode === 'unconfigured-delayed-startup' ? 'SIGKILL' : 'SIGTERM');
        }
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
      }
      }
      await verifyNodeSupervisorBunAnchorStartup(fixture, nodePath);
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('HOST lease default and injected collectors retire on caller loss', async () => {
    if (process.platform !== 'darwin') return 'independent supervisor wait-status witness requires Darwin EVFILT_PROC NOTE_EXITSTATUS';
    const fixture = await harness.createFixture();
    const fakeAdbEnvironment = await installFakeAdbLifetimeBackstop(fixture);
    const measurementModule = repositoryPath('tests/mobile/android-measurement.ts');
    const budgetModule = repositoryPath('tests/mobile/support/budget.ts');
    const ownedModule = repositoryPath('tests/mobile/support/owned-process.ts');
    const toolchains = repositoryPath('tests/mobile/toolchains.json');
    try {
      for (const mode of ['default', 'injected-failed-begin'] as const) {
      const outputDir = join(fixture.root, `lifetime-${mode}-output`);
      const observer = "collectorObserverStdout:createWriteStream('',{fd:3,autoClose:false}),collectorObserverStderr:createWriteStream('',{fd:4,autoClose:false}),collectorObserverSupervisor:createWriteStream('',{fd:5,autoClose:false})";
      const injected = mode === 'injected-failed-begin'
        ? `spawnCollector:()=>spawnOwnedProcess('adb',['-s','emulator-5554','logcat','-b','main','-b','system','-v','epoch','-T','1'],{timeoutMs:budget.remainingMs,cleanupReservationMs:1000,observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})}),stopProcess:async collector=>{process.stdout.write(JSON.stringify({event:'failed-begin-cleanup',atNs:process.hrtime.bigint().toString()})+'\\n');await new Promise(resolve=>setTimeout(resolve,5000));return collector.retire('measurement-finished')},boundedCommand:async(binary,args,timeout,options)=>{if(args.includes('snapshot'))throw new Error('controlled public failed-begin after collector startup');return boundedCommand(binary,args,timeout,options)}`
        : '';
      const io = mode === 'default' ? `{${observer}}` : `{${observer},${injected}}`;
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {AndroidEnvironmentMeasurement}=await import(${JSON.stringify(measurementModule)});const {PhaseBudget}=await import(${JSON.stringify(budgetModule)});const {spawnOwnedProcess}=await import(${JSON.stringify(ownedModule)});const {boundedCommand}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/bounded-process.ts'))});const budget=new PhaseBudget('caller-loss-${mode}',{timeoutMs:10000,recoveryLimit:0});const measurement=new AndroidEnvironmentMeasurement('emulator-5554',${JSON.stringify(outputDir)},${JSON.stringify(toolchains)},${io},budget);measurement.bind({runId:'35556703407',attempt:'2',suite:'smoke',platform:'android',baseline:'0.20.10',scenario:'historical',sourceCommit:'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',sourceRunHeadSha:'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',candidateWebHash:'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',candidateBuild:'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',measurementId:measurement.id});const log=(event,value)=>process.stdout.write(JSON.stringify({event,atNs:process.hrtime.bigint().toString(),value})+String.fromCharCode(10));${mode === 'default' ? "try{await measurement.begin();log('measurement-ready',{id:measurement.id})}catch(error){log('begin-error',{message:String(error),result:error?.result})}" : "measurement.begin().then(()=>log('unexpected-begin-success'),error=>log('begin-error',{message:String(error),result:error?.result}));"}setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      try {
        caller = startLifetimeCaller(ownerSource, fakeAdbEnvironment);
        assert.ok(caller.child.pid);
        if (mode === 'default') await caller.waitFor('measurement-ready', 7_000);
        else await caller.waitFor('failed-begin-cleanup', 7_000);
        await waitForValue(() => {
          try { return readFileSync(fixture.log, 'utf8').includes('logcat -b main -b system -v epoch -T 1') ? true : undefined; } catch { return undefined; }
        }, 2_000, 'public measurement did not start the fake adb collector');
        const group = await waitForValue(() => {
          const owned = lifetimeOwnedGroup(caller!.child.pid!);
          return owned?.target ? owned : undefined;
        }, 3_000, 'collector supervisor, group, and target were not observed');
        supervisorPID = group.supervisor.pid;
        groupID = group.anchor.pid;
        targetPID = group.target?.pid;
        assert.ok(targetPID);
        assert.equal(group.target!.ppid, group.manager.pid);
        assert.equal(group.target!.pgid, groupID);
        if (mode === 'default') {
          await waitForValue(() => /HerdrMeasure:.*START/u.test(caller!.observerOutput()) ? true : undefined,
            1_000, 'default collector output observer did not receive its measurement marker');
        }
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        assert.equal(caller.child.kill(mode === 'default' ? 'SIGTERM' : 'SIGKILL'), true);
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, mode === 'default' ? 'SIGTERM' : 'SIGKILL', hardDeadlineNs);
        assert.equal(await waitForPidAbsent(targetPID!, 1_000), true);
        const diagnostic = await waitForValue(() => {
          try {
            const value = JSON.parse(readFileSync(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
            return value.children.flatMap(child => child.events).find(event => event.stage === 'fake-child-exit' && event.childExitCategory === 'signal');
          } catch { return undefined; }
        }, 1_000, 'fake adb collector did not record its actual signal exit');
        assert.equal(diagnostic.stage, 'fake-child-exit');
      } finally {
        statusObserver?.close();
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
      }
      }
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  tests.push(['Android retained production measurement collector honors enclosing fixture lifetime', async () => {
    const fixture = await harness.createFixture();
    let collector: OwnedProcessHandle | undefined;
    let groupID: number | undefined;
    await withAndroidRetainedLifecycleFixture(fixture, async ({ measurement }) => {
      collector = (measurement as unknown as { collector?: OwnedProcessHandle }).collector;
      assert.ok(collector, 'retained fixture must use the production default collector');
      await collector.ready;
      const binding = await collector.started;
      assert.ok(binding, 'production collector target must start');
      groupID = binding.groupID;
      assert.equal(processGroupAbsent(groupID), false, 'production collector group must remain active during the fixture');
    });
    assert.ok(collector);
    assert.ok(groupID);
    const retirement = await collector.retirement;
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
    assert.equal(collector.supervisor.exitCode, 0);
    assert.equal(collector.supervisor.signalCode, null);
    assert.equal(collector.isSupervisorClosed, true);
    assert.equal(processGroupAbsent(groupID), true);
    const summary = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
    assert.ok(summary.bytes > 0, 'production collector must persist bytes acquired during the fixture');
  }]);
  test('recorded isolated UID death retains reason and requesting PID without an exemption', async () => {
    const isolated = '1789100452.864 546 1761 I ActivityManager: Killing 5328:com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0/u0a146i-9000 (adj 0): isolated not needed';
    const stopped = '1789100452.274 546 1761 I ActivityManager: Killing 5358:com.android.chrome:privileged_process0/u0a146 (adj 0): stop com.android.chrome due to from pid 5777';
    assert.deepEqual(androidLogEvents(`${isolated}\n${stopped}\n`), [isolated, stopped]);
    assert.equal(androidEventDetails(isolated).uid, 'u0a146i-9000');
    assert.equal(androidEventDetails(isolated).reason, 'isolated not needed');
    assert.equal(androidEventDetails(isolated).kind, 'process-death');
    assert.equal(androidEventDetails(isolated).initiatorPid, undefined);
    assert.equal(androidEventDetails(stopped).initiatorPid, '5777');
    assert.equal(androidEventDetails('1789100452.274 546 1761 I ChimeraCfgMgr: Updating module config: old -> new').kind, 'module-config');
  });
  for (const observationFails of [false, true]) for (const fails of [false, true]) test(`bootstrap close bounded observations preserve settlement and original error ${fails} observation failure ${observationFails}`, async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await measurement.begin();
      if (observationFails) await writeFile(join(fixture.fixtureDirectory, 'valid-vending.json'), JSON.stringify({ processListFail: true }));
      let deletes = 0;
      const driver = new AppiumClient('http://fixture.test', 1_000, async (input, init) => {
        if (init?.method === 'DELETE') {
          deletes++;
          if (fails) throw new Error('synthetic close failure');
        }
        return Response.json({ value: new URL(String(input)).pathname === '/session' ? { sessionId: 'bootstrap' } : null });
      });
      await driver.create({ capabilities: {} });
      for (let index = 0; index < 55; index++) await driver.activeAppInfo();
      if (fails) await assert.rejects(() => measurement.observeBootstrapClose(driver), /synthetic close failure/u);
      else await measurement.observeBootstrapClose(driver);
      assert.equal(deletes, 1);
      const trace = JSON.parse(await readFile(join(fixture.root, 'android-environment-bootstrap-close.json'), 'utf8'));
      assert.equal(trace.qualifiesPlannedTermination, false);
      assert.equal(trace.BEGINMarkerSettled, observationFails ? undefined : true);
      assert.equal(trace.ENDMarkerSettled, observationFails ? undefined : true);
      if (observationFails) {
        assert.equal(trace.BEGINObservationFailed, true);
        assert.equal(trace.ENDObservationFailed, true);
      }
      assert.equal(trace.sessionPresentBefore, true);
      assert.equal(trace.sessionPresentAfter, fails);
      assert.equal(trace.deleteCommands.length, 1);
      assert.equal(trace.deleteCommands[0].failed, fails);
      assert.deepEqual(JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8')), []);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('retained initial and warm relaunch emit no fabricated close evidence or planned operations', async () => {
    let completed = 0;
    for (const group of Object.keys(androidLifecycleCases) as Array<keyof typeof androidLifecycleCases>) {
      for (const name of androidLifecycleCases[group]) {
        const fixture = await harness.createFixture();
        await runAndroidLifecycleCase(fixture, group, name);
        assert.equal(existsSync(join(fixture.root, 'android-environment-bootstrap-close.json')), false);
        assert.doesNotMatch(await readFile(fixture.log, 'utf8'), /CLOSE_(?:BEGIN|END)/u);
        completed++;
        console.log(`PASS SM56 lifecycle ${group}/${name}`);
      }
    }
    assert.equal(completed, 54);
  });
  test('SM56 warm lifecycle keeps independent and combined signal, stop and component drift fatal', async () => {
    for (const name of ['healthy-warm', 'early-isolated-signal9-only', 'unknown-stop-only', 'stable-binary-gms-components-only', 'signal9-stop-components-combined', 'package-identity-replacement-only']) {
      const fixture = await harness.createFixture();
      await withAndroidRetainedLifecycleFixture(fixture, async ({ platform, client, measurement, calls, operations }) => {
        const owner = client.retainSessionOwner();
        const window = platform.evidenceSnapshot().selectedInstalledWindow;
        const before = JSON.parse(await readFile(join(fixture.root, 'android-environment-before.json'), 'utf8')) as AndroidEnvironmentSnapshot;
        const combined = name === 'signal9-stop-components-combined';
        const signal = combined || name === 'early-isolated-signal9-only';
        const stop = combined || name === 'unknown-stop-only';
        const components = combined || name === 'stable-binary-gms-components-only';
        const replacement = name === 'package-identity-replacement-only';
        const expectedEvents: string[] = [];
        const native = (tag: string, message: string, fatal = false) => {
          const line = `${(Date.now() / 1000).toFixed(3)} 546 1761 I ${tag}: ${message}\n`;
          if (fatal) expectedEvents.push(line.trimEnd());
          return line;
        };
        let events = '';
        if (signal) {
          const child = 'com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:8';
          events += native('ActivityManager', `Start proc 5835:${child}/u0a146i-9000 for service {com.android.chrome/org.chromium.content.app.SandboxedProcessService0:8}`);
          events += native('ActivityManager', `Killing 5835:${child}/u0a146i-9000 (adj 0): isolated not needed`, true);
          events += native('Process', 'Sending signal. PID: 5835 SIG: 9', true);
        }
        if (stop) {
          assert.equal(before.measurement?.processes['6538'], 'com.android.chrome');
          events += native('ActivityManager', 'Force stopping com.android.chrome appid=10123 user=0: from pid 9864');
          events += native('ActivityManager', 'Killing 6538:com.android.chrome/u0a123 (adj 0): stop com.android.chrome due to from pid 9864', true);
        }
        if (replacement) events += native('PackageManager', 'Replacing package com.android.chrome', true);
        if (events) await appendFile(join(fixture.fixtureDirectory, 'native.log'), events);
        if (components) {
          const file = join(fixture.fixtureDirectory, 'valid-gms.dump');
          const dump = await readFile(file, 'utf8');
          assert.ok(dump.includes('Queries:'));
          await writeFile(file, dump.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.ComponentChanged\nQueries:'));
        }
        if (replacement) {
          const file = join(fixture.fixtureDirectory, 'valid-chrome.dump');
          const dump = await readFile(file, 'utf8');
          await writeFile(file, dump.replace(/lastUpdateTime=[^\n]+/u, 'lastUpdateTime=2026-09-20 12:00:00'));
        }
        await platform.relaunchInstalledApp();
        await platform.backgroundApp();
        await platform.relaunchInstalledApp();
        const candidate = await platform.readRunningIdentity();
        assert.equal(candidate.version, '0.21.0');
        assert.equal(candidate.assets, 380);
        assert.equal(candidate.buildFromApplication, true);
        owner();
        assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, window);
        assert.equal(calls.filter(call => call.path === '/session').length, 1);
        assert.equal(calls.filter(call => call.method === 'DELETE').length, 0);
        assert.deepEqual(await operations(), []);
        (measurement as any).identity = measurementIdentity({ measurementId: measurement.id });
        const outcome = await measurement.finish();
        assert.deepEqual(outcome.errors, []);
        assert.equal(outcome.assessment?.status, name === 'healthy-warm' ? 'PASS' : 'FAIL');
        const result = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
        assert.equal(result.passed, name === 'healthy-warm');
        assert.deepEqual(result.normalRetirements, []);
        assert.deepEqual(result.boundaryDiscordances, []);
        assert.ok((await readFile(join(fixture.root, 'android-qualification-logcat.log'), 'utf8')).includes(events));
        assert.deepEqual(result.forcedRestartEvents, expectedEvents);
        assert.deepEqual(result.nativeEvents.map((event: { line: string }) => event.line), expectedEvents);
        assert.deepEqual(result.eventCounts, {
          rawEvents: expectedEvents.length,
          distinctDeathPids: Number(signal) + Number(stop),
          fatalEvents: expectedEvents.length,
          normalRetirementPids: 0,
        });
        assert.deepEqual(result.issues, [
          ...(components ? [
            'com.google.android.gms dependencyConfigSha256 changed',
            'com.google.android.gms identitySha256 changed',
            'com.google.android.gms dependency configuration changed',
          ] : []),
          ...(replacement ? ['com.android.chrome lastUpdateTime changed', 'com.android.chrome identitySha256 changed'] : []),
          ...(expectedEvents.length ? ['native process death, dependency configuration change or package replacement was observed'] : []),
        ]);
        if (signal) assert.ok(result.forcedRestartEvents.some((line: string) => line.includes('5835 SIG: 9')));
        if (stop) assert.ok(result.forcedRestartEvents.some((line: string) => line.includes('from pid 9864')));
        if (stop) {
          const stopped = result.nativeEvents.find((event: { initiatorPid?: string }) => event.initiatorPid === '9864');
          assert.ok(stopped);
          assert.equal(stopped.kind, 'process-death');
          assert.equal(stopped.pid, '6538');
          assert.equal(stopped.processName, 'com.android.chrome');
          assert.equal(stopped.uid, 'u0a123');
          assert.equal(stopped.reason, 'stop com.android.chrome due to from pid 9864');
        }
        if (components) {
          assert.ok(result.issues.includes('com.google.android.gms dependency configuration changed'));
          const after = JSON.parse(await readFile(join(fixture.root, 'android-environment-after.json'), 'utf8')) as AndroidEnvironmentSnapshot;
          for (const key of ['versionCode', 'versionName', 'codePath', 'apkPaths', 'lastUpdateTime'] as const) {
            assert.deepEqual(after.packages['com.google.android.gms'][key], before.packages['com.google.android.gms'][key]);
          }
        }
        if (replacement) assert.ok(result.issues.some((issue: string) => /com.android.chrome.*changed/u.test(issue)));
        assert.deepEqual(await operations(), []);
        assert.equal(existsSync(join(fixture.root, 'android-environment-bootstrap-close.json')), false);
        assert.doesNotMatch(await readFile(fixture.log, 'utf8'), /force-stop|disable-user|CLOSE_(?:BEGIN|END)/u);
      });
      console.log(`PASS SM56 environment ${name}`);
    }
  });
  const prepare = (fixture: Fixture) => cli(fixture, ['prepare', '--serial', 'emulator-5554', '--toolchains', process.env.ANDROID_ENVIRONMENT_TOOLCHAINS || repositoryPath('tests/mobile/toolchains.json'), '--output', join(fixture.root, 'preparation.json'), '--adb-timeout-ms', '1000']);
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
  for (const [name, vending, mutations] of [
    ['absent', { absent: true }, 0], ['disabled ordinary-listed', { enabled: 3 }, 0],
    ['default preparation', { enabled: 0 }, 1], ['enabled preparation', { enabled: 1 }, 1],
    ['component-only preparation', { enabled: 0, components: true }, 1],
    ['absent for user 0 but package remains', { installed: false, enabled: 0 }, 0],
  ] as const) test(`prepare, independent readback, snapshots and persistence: ${name}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, vending);
    const result = prepare(fixture);
    assert.equal(result.passed, true, result.stderr);
    const observation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    assert.ok(observation.provenance.avdConfig && observation.provenance.sdkRevision && observation.system['ro.build.fingerprint']);
    assert.equal(observation.mutation, mutations ? 'disable-user-0' : 'none');
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.split('\n').filter((line) => line.includes('disable-user')).length, mutations);
    if (mutations) {
      assert.ok(requests.indexOf('dumpsys package com.android.vending') < requests.indexOf('disable-user'));
      assert.ok(requests.lastIndexOf('dumpsys package com.android.vending') > requests.indexOf('disable-user'));
    }
    const { before, after } = await snapshots(fixture);
    const persisted = JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot;
    assert.equal(persisted.vending.presence, observation.after?.presence);
    assert.equal(persisted.vending.ordinaryListed, observation.after?.presence === 'installed');
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    assert.equal(requests.includes('disable-user --user 0 com.google.android.gms'), false);
  });
  test('smoke53 stable package versions and paths do not excuse normalized GMS component drift', async () => {
    // Normalized delta from run 35296215928, environment-delta.json (not a
    // device dump). Unchanged members are synthetic padding to the observed
    // 186 -> 245 enabled / 80 -> 77 disabled counts; added/removed names are observed.
    const enabledAdded = [
      'com.google.android.gms.family.v2.create.FamilyCreationActivity',
      'com.google.android.gms.family.v2.invites.SendInvitationsActivity',
      'com.google.android.gms.family.v2.manage.DeleteMemberActivity',
      'com.google.android.gms.family.v2.manage.FamilyManagementActivity',
      'com.google.android.gms.family.v2.tos.TosActivity',
      'com.google.android.gms.family.webview.FamilyWebViewActivity',
      'com.google.android.gms.findmydevice.spot.locationreporting.taptoid.TapToIdHalfSheetActivity',
      'com.google.android.gms.fonts.provider.FontsProvider',
      'com.google.android.gms.fonts.update.UpdateSchedulerService',
      'com.google.android.gms.googlehelp.GcmBroadcastReceiver',
      'com.google.android.gms.googlehelp.gcm.InvalidateGcmTokenGcmTaskService',
      'com.google.android.gms.googlehelp.helpactivities.OpenHelpRtcActivity',
      'com.google.android.gms.googlehelp.webview.GoogleHelpRenderingApiWebViewActivity',
      'com.google.android.gms.googlehelp.webview.GoogleHelpSupportWebViewActivity',
      'com.google.android.gms.growth.featuredrops.activity.FeatureDropsActivity',
      'com.google.android.gms.growth.featuredrops.activity.FeatureDropsProofingActivity',
      'com.google.android.gms.growth.notifications.GcmBroadcastReceiver',
      'com.google.android.gms.growth.notifications.NotificationActionActivity',
      'com.google.android.gms.growth.surveys.activity.GmsSurveyActivity',
      'com.google.android.gms.growth.ui.GrowthDebugActivity',
      'com.google.android.gms.growth.ui.webview.GrowthWebViewActivity',
      'com.google.android.gms.ipa.base.IpaGcmTaskService',
      'com.google.android.gms.languageprofile.GcmReceiverService',
      'com.google.android.gms.languageprofile.GcmTaskService',
      'com.google.android.gms.lockbox.service.LockboxBrokerService',
      'com.google.android.gms.octarine.ui.OctarineWebviewActivity',
      'com.google.android.gms.pay.deeplink.AliasSavePkPassActivity',
      'com.google.android.gms.pay.deeplink.AliasSaveSmartHealthCardActivity',
      'com.google.android.gms.people.sync.coreui.ContactsSyncCoreActivity',
      'com.google.android.gms.recaptcha.RecaptchaActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreContactsActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreDialogActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreSettingsActivity',
      'com.google.android.gms.security.provider.SecurityProvider',
      'com.google.android.gms.security.recaptcha.RecaptchaActivity',
      'com.google.android.gms.security.settings.VerifyAppsSettingsActivity',
      'com.google.android.gms.semanticlocationhistory.service.OnDeviceSettingsInjectorService',
      'com.google.android.gms.semanticlocationhistory.settings.OnDeviceSettingsActivity',
      'com.google.android.gms.setupservices.GoogleServicesActivity',
      'com.google.android.gms.smartdevice.d2d.ui.ForwardingActivity',
      'com.google.android.gms.smartdevice.d2d.ui.TargetDirectTransferActivity',
      'com.google.android.gms.smartdevice.magicwand.MagicWandActivity',
      'com.google.android.gms.smartdevice.setup.ui.AccountChallengeActivity',
      'com.google.android.gms.trustlet.place.ui.TrustedPlacesSettingsActivity',
      'com.google.android.gms.udc.gcm.GcmBroadcastReceiver',
      'com.google.android.gms.udc.service.UdcContextInitService',
      'com.google.android.gms.udc.ui.AuthenticatingWebViewActivity',
      'com.google.android.gms.udc.ui.UdcSettingsListActivity',
      'com.google.android.gms.update.OtaSuggestionSummaryProvider',
      'com.google.android.gms.update.SystemUpdateActivity',
      'com.google.android.gms.update.SystemUpdateV2Activity',
      'com.google.android.gms.update.UpdateFromSdCardActivity',
      'com.google.android.gms.vision.DependencyBroadcastReceiverProxy',
      'com.google.android.gms.wallet.ocr.CardRecognitionShimProxyActivity',
      'com.google.android.personalsafety.settings.BleTagPlatformSettingsActivity',
      'com.google.android.personalsafety.settings.BleTagSettingsActivity',
      'com.google.firebase.auth.api.gms.service.FirebaseAuthService',
      'com.google.firebase.auth.api.gms.ui.BrowserSignInResponseHandlerActivity',
      'com.google.firebase.auth.api.gms.ui.BrowserSignInStarterActivity',
    ];
    const disabledRemoved = [
      'com.google.android.gms.pay.deeplink.AliasSaveSmartHealthCardActivity',
      'com.google.android.gms.semanticlocationhistory.service.OnDeviceSettingsInjectorService',
      'com.google.android.gms.semanticlocationhistory.settings.OnDeviceSettingsActivity',
      'com.google.android.personalsafety.settings.BleTagPlatformSettingsActivity',
    ];
    const disabledAdded = ['com.google.android.gms.findmydevice.spot.suw.SetupWizardActivity'];
    const enabled = Array.from({ length: 186 }, (_, index) => `fixture.gms.Enabled${index}`);
    const disabled = Array.from({ length: 76 }, (_, index) => `fixture.gms.Disabled${index}`);
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const dump = (enabled: string[], disabled: string[]) => original.replace('Queries:',
      `      enabledComponents:\n${enabled.map(name => `        ${name}\n`).join('')}      disabledComponents:\n${disabled.map(name => `        ${name}\n`).join('')}Queries:`);
    await writeFile(path, dump(enabled, [...disabled, ...disabledRemoved]));
    const { before, after } = await snapshots(fixture);
    const identity = async (path: string) => (JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot).packages['com.google.android.gms'];
    const first = await identity(before);
    assert.equal(first.dependencyConfig.enabledComponents.length, 186);
    assert.equal(first.dependencyConfig.disabledComponents.length, 80);
    // Reorder sections' members and change an object ID: dump hash changes,
    // normalized component identity must not. No new baseline is taken.
    await writeFile(path, dump([...enabled].reverse(), [...disabled, ...disabledRemoved].reverse()).replace('(fixture)', '(noise)'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'noise-diagnostics.json'))).passed, true);
    const noise = await identity(after);
    assert.notEqual(noise.dumpSha256, first.dumpSha256);
    assert.equal(noise.identitySha256, first.identitySha256);
    assert.equal(noise.dependencyConfigSha256, first.dependencyConfigSha256);
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    await writeFile(path, dump([...enabled, ...enabledAdded].reverse(), [...disabled, ...disabledAdded].reverse()));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'drift-diagnostics.json'))).passed, true);
    const last = await identity(after);
    assert.equal(last.dependencyConfig.enabledComponents.length, 245);
    assert.equal(last.dependencyConfig.disabledComponents.length, 77);
    assert.deepEqual(last.dependencyConfig.enabledComponents.filter(name => !first.dependencyConfig.enabledComponents.includes(name)), [...enabledAdded].sort());
    assert.deepEqual(first.dependencyConfig.disabledComponents.filter(name => !last.dependencyConfig.disabledComponents.includes(name)), [...disabledRemoved].sort());
    assert.deepEqual(last.dependencyConfig.disabledComponents.filter(name => !first.dependencyConfig.disabledComponents.includes(name)), disabledAdded);
    for (const key of Object.keys(first) as Array<keyof typeof first>) {
      if (['dependencyConfig', 'dependencyConfigSha256', 'identitySha256', 'dumpSha256'].includes(key)) continue;
      assert.deepEqual(last[key], first[key], `stable package field ${key}`);
    }
    assert.notEqual(last.dependencyConfigSha256, first.dependencyConfigSha256);
    assert.notEqual(last.identitySha256, first.identitySha256);
    const checked = await harness.check(fixture, before, after);
    assert.equal(checked.passed, false);
    assert.deepEqual(checked.issues, [
      'com.google.android.gms dependencyConfigSha256 changed',
      'com.google.android.gms identitySha256 changed',
      'com.google.android.gms dependency configuration changed',
    ]);
  });
  for (const violation of ['earlier SIGKILL', 'unknown close force-stop', 'both'] as const) {
    test(`smoke53 successful scenario and session close cannot qualify ${violation}`, async () => {
      const fixture = await harness.createFixture();
      const child = 'com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0';
      await state(fixture, { absent: true, children: { '5759': child } });
      const { before, after } = await snapshots(fixture);
      let deletes = 0;
      const driver = new AppiumClient('http://fixture.test', 1_000, async (input, init) => {
        if (init?.method === 'DELETE') deletes++;
        return Response.json({ value: new URL(String(input)).pathname === '/session' ? { sessionId: 'scenario' } : true });
      });
      await driver.create({ capabilities: {} });
      assert.equal(await driver.execute('return true'), true, 'synthetic scenario assertion completes');
      await driver.close();
      assert.equal(deletes, 1);
      assert.equal(driver.snapshot().sessionId, '');
      assert.equal(driver.snapshot().unusable, false);
      // Source-derived event shapes with fixture times/PIDs. The observed
      // initiator 9970 stays UNKNOWN; DELETE is not an am force-stop receipt.
      const early = `09-10 08:45:09.265 546 1761 I ActivityManager: Killing 5759:${child}/u0a146i-9000 (adj 0): isolated not needed\n`
        + '09-10 08:45:09.401 5759 5759 I Process: Sending signal. PID: 5759 SIG: 9\n';
      const close = marker('20.000', 'CLOSE_BEGIN bootstrap')
        + '09-10 08:45:21.180 546 1761 I ActivityManager: Force stopping com.android.chrome appid=10146 user=0: from pid 9970\n'
        + '09-10 08:45:21.181 546 1761 I ActivityManager: Killing 6538:com.android.chrome/u0a146 (adj 0): stop com.android.chrome due to from pid 9970\n'
        + marker('22.000', 'CLOSE_END bootstrap');
      const checked = await harness.check(fixture, before, after, (violation !== 'unknown close force-stop' ? early : '') + (violation !== 'earlier SIGKILL' ? close : ''));
      assert.equal(checked.passed, false);
      assert.ok(checked.issues.includes('native process death, dependency configuration change or package replacement was observed'));
      const report = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
      if (violation !== 'unknown close force-stop') assert.ok(report.forcedRestartEvents.some((line: string) => line.includes('5759 SIG: 9')));
      if (violation !== 'earlier SIGKILL') assert.ok(report.forcedRestartEvents.some((line: string) => line.includes('from pid 9970')));
      assert.deepEqual(JSON.parse(await readFile(join(fixture.root, 'operations.json'), 'utf8')), [], 'never fabricate device initiator ownership from a host session close');
      assert.equal(report.normalRetirements.length, 0);
    });
  }
  test('headless version acquisition avoids the GUI runtime and preserves measured identity', async () => {
    const fixture = await harness.createFixture();
    assert.throws(() => execFileSync('emulator', ['-version'], { env: fixture.environment, stdio: 'pipe' }), /libpulse.so.0/u);
    assert.equal(execFileSync('emulator', ['-no-window', '-version'], { env: fixture.environment, encoding: 'utf8' }).trim(), 'Android emulator version 35.0.2.0');
    const { before, after } = await snapshots(fixture);
    assert.equal((JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot).emulatorVersion, 'Android emulator version 35.0.2.0');
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    await writeFile(join(fixture.root, 'bin', 'emulator'), '#!/bin/sh\nexit 127\n');
    const output = join(fixture.root, 'failed-version.json');
    assert.equal((await harness.snapshot(fixture, 'valid', output, join(fixture.root, 'failed-version-diagnostics.json'))).passed, false);
    assert.equal(existsSync(output), false);
  });
  test('acquires only eight named identity properties despite legal ambiguous dump records beyond preview', async () => {
    const fixture = await harness.createFixture();
    const dump = await readFile(join(fixture.fixtureDirectory, 'getprop'), 'utf8');
    assert.ok(dump.indexOf('[source-derived]') > 4000);
    assert.ok(dump.includes('first]\n[ro.synthetic.other]: [second'));
    assert.equal(prepare(fixture).passed, true);
    const preparation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    const { before, after } = await snapshots(fixture);
    const snapshot = JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot;
    assert.deepEqual(snapshot.system, preparation.system);
    assert.equal(Object.keys(snapshot.system).length, 7);
    const requests = (await readFile(fixture.log, 'utf8')).trim().split('\n');
    const properties = requests.filter((line) => line.includes('getprop'));
    assert.equal(properties.length, 8);
    assert.ok(properties.includes('-s emulator-5554 shell getprop ro.kernel.qemu'));
    assert.ok(properties.every((line) => /^-s emulator-5554 shell getprop ro\.[a-z.]+$/u.test(line)));
    const diagnostics = JSON.parse(await readFile(after.replace('.json', '-diagnostics.json'), 'utf8'));
    assert.ok(diagnostics.commands.filter((entry: { args: string[] }) => entry.args.includes('getprop')).every((entry: { stdoutPreview?: string }) => entry.stdoutPreview === undefined));
    assert.equal((await harness.check(fixture, before, after)).passed, true);
  });
  for (const [name, value] of [
    ['empty', ''], ['missing', '\n'], ['truncated', 'fixture'], ['multiline', 'first\nsecond\n'],
    ['framed dump', '[ro.build.id]: [AP4A]\n[ro.build.id]: [AP4A]\n'],
    ['oversized', 'x'.repeat(4096) + '\n'], ['control', 'fixture\u0000\n'], ['padded', ' fixture\n'],
  ]) test(`rejects ${name} required property in preparation and snapshot without mutation`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 0, propertyResponses: { 'ro.build.id': value } });
    const preparation = prepare(fixture);
    assert.equal(preparation.passed, false, name);
    assert.equal(preparation.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    const snapshot = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(snapshot.passed, false, name);
    assert.equal(snapshot.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
  });
  for (const [name, value] of [['ro.build.version.sdk', '34\n'], ['ro.kernel.qemu', '0\n']]) {
    test(`rejects wrong ${name} before preparation mutation and during snapshot`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, propertyResponses: { [name]: value } });
      assert.equal(prepare(fixture).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      assert.equal((await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'))).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    });
  }
  for (const failure of ['propertyTimeout', 'propertyFailure', 'propertyStderr']) {
    test(`rejects named acquisition ${failure} before any preparation mutation and snapshot`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, [failure]: 'ro.build.id' });
      assert.equal(prepare(fixture).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      assert.equal((await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'), 1000)).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      const diagnostics = JSON.parse(await readFile(join(fixture.root, 'diagnostics.json'), 'utf8'));
      assert.equal(diagnostics.failure.stage, 'read required system property ro.build.id');
      if (failure === 'propertyTimeout') assert.equal(diagnostics.failure.timedOut, true);
    });
  }
  test('requires every persisted identity field and qemu before preparation mutation', async () => {
    const keys = ['ro.build.fingerprint', 'ro.build.id', 'ro.build.version.incremental', 'ro.build.version.release', 'ro.build.version.sdk', 'ro.product.name', 'ro.product.device', 'ro.kernel.qemu'];
    for (const key of keys) {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, propertyResponses: { [key]: '\n' } });
      assert.equal(prepare(fixture).passed, false, key);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false, key);
    }
  });
  test('independent preparation readback rejects changed system identity without repair', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 0, propertyChangeOnDisable: { 'ro.build.id': 'CHANGED\n' } });
    const result = prepare(fixture);
    assert.equal(result.passed, false);
    assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
    assert.equal((await readFile(fixture.log, 'utf8')).split('\n').filter((line) => line.includes('disable-user')).length, 1);
  });
  test('rejects changed named system identity after measurement', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    await state(fixture, { absent: true, propertyResponses: { 'ro.build.id': 'CHANGED\n' } });
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
  for (const enabled of [2, 4]) test(`review: shell denies preparation from enabled=${enabled} without additional mutation`, async () => {
    const fixture = await harness.createFixture();
    const value = { enabled };
    await state(fixture, value);
    const result = prepare(fixture);
    assert.equal(result.passed, false, result.stderr);
    assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
    assert.deepEqual(JSON.parse(await readFile(join(fixture.fixtureDirectory, 'valid-vending.json'), 'utf8')), value);
    const preparation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    assert.equal(preparation.before.identity?.enabled, String(enabled));
    assert.equal(preparation.mutation, 'disable-user-0');
    assert.equal(preparation.after, undefined);
    const requests = (await readFile(fixture.log, 'utf8')).trim().split('\n');
    assert.equal(requests.filter((line) => /disable-user/u.test(line)).length, 1);
    assert.equal(requests.at(-1), '-s emulator-5554 shell pm disable-user --user 0 com.android.vending');
    assert.equal(requests.some((line) => /uninstall|enable |disable-user.*gms|root|remount/u.test(line)), false);
    const diagnostics = JSON.parse(await readFile(join(fixture.root, 'preparation-diagnostics.json'), 'utf8'));
    assert.equal(diagnostics.failure.exitCode, 1);
    assert.equal(diagnostics.failure.stage, 'disable Vending for owned emulator user 0');
  });
  for (const [name, vending] of [
    ['wrong foreground user', { foregroundUser: 10 }], ['wrong AVD', { avd: 'unowned-avd' }],
    ['wrong package user', { user: 10 }], ['malformed enabled state', { enabled: 9 }],
    ['malformed listing', { list: 'Failure [denied]\n' }], ['truncated listing', { list: 'package:com.android.vending' }],
    ['substring is not absence', { list: 'package:com.android.vending.other\n' }],
    ['empty dump is not absence', { dump: '' }], ['unknown dump is not absence', { dump: 'Unknown package\n' }],
    ['failed disable command', { disableFail: true }], ['failed disable readback', { readbackFail: true }],
    ['false disable response', { disableOutput: 'Success\n' }],
  ] as const) test(`rejects preparation ${name}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, vending);
    const result = prepare(fixture);
    assert.equal(result.passed, false, name);
    const allowed = name.includes('disable');
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.includes('disable-user'), allowed, result.stderr);
    if (name === 'wrong AVD') {
      assert.equal(requests, '-s emulator-5554 emu avd name\n');
      const diagnostic = JSON.parse(await readFile(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
      assert.doesNotThrow(() => validateFakeAdbDiagnostic(diagnostic));
    }
  });
  test('refuses missing ownership, wrong SDK and ambiguous AVD config before mutation', async () => {
    for (const change of ['ownership', 'sdk', 'avd']) {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0 });
      if (change === 'ownership') await writeFile(join(fixture.root, 'ownership'), 'android:emulator-5556\n');
      if (change === 'sdk') await writeFile(join(fixture.root, 'sdk/system-images/android-35/google_apis/x86_64/source.properties'), 'Pkg.Revision=\n');
      if (change === 'avd') await writeFile(join(fixture.root, 'avd/herdr-mobile-ci-fixture.avd/config.ini'), 'image.sysdir.1=wrong\nimage.sysdir.1=also-wrong\n');
      assert.equal(prepare(fixture).passed, false);
      assert.equal(existsSync(fixture.log) && (await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    }
  });
  for (const enabled of [0, 1, 2, 4]) test(`snapshot refuses Vending state ${enabled} without repair`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled });
    const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(result.passed, false);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
  });
  for (const [name, changed] of [['version', { version: '456' }], ['path', { path: '/product/priv-app/Changed' }], ['presence', { absent: true }]] as const) {
    test(`post-run Vending ${name} changes fail without repair`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 3 });
      const { before, after } = await snapshots(fixture);
      await state(fixture, { enabled: 3, ...changed });
      const result = await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
      const check = await harness.check(fixture, before, after);
      assert.equal(check.passed, false);
      assert.match(check.issues.join(';'), /Vending/u);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    });
  }
  for (const versionName of ['24.23.35 (190800-646585959)', ' 24.23.35 (190800-646585959)  ', 'release candidate\tβ']) {
    test(`review: preserves and compares complete source-derived versionName ${JSON.stringify(versionName)}`, async () => {
      const fixture = await harness.createFixture();
      const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
      const original = await readFile(path, 'utf8');
      await writeFile(path, original.replace('versionName=24.23.35\n', `versionName=${versionName}\n`));
      const { before, after } = await snapshots(fixture);
      const identity = (JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot).packages['com.google.android.gms'];
      assert.equal(identity.versionName, versionName);
      assert.equal((await harness.check(fixture, before, after)).passed, true);
      await writeFile(path, original.replace('versionName=24.23.35\n', `versionName=${versionName} changed\n`));
      const result = await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
      const checked = await harness.check(fixture, before, after);
      assert.equal(checked.passed, false);
      assert.ok(checked.issues.includes('com.google.android.gms versionName changed'));
      assert.ok(checked.issues.includes('com.google.android.gms identitySha256 changed'));
    });
  }
  for (const [name, mutate] of [
    ['empty version', (text: string) => text.replace('versionName=24.23.35', 'versionName=')],
    ['missing version', (text: string) => text.replace('    versionName=24.23.35\n', '')],
    ['duplicate version', (text: string) => text.replace('    versionName=24.23.35', '    versionName=24.23.35\n    versionName=24.23.35')],
    ['missing user field', (text: string) => text.replace('installed=true ', '')],
    ['duplicate user field', (text: string) => text.replace('installed=true ', 'installed=true installed=true ')],
    ['missing firstInstallTime', (text: string) => text.replace('      firstInstallTime=2026-01-01 00:00:00\n', '')],
    ['missing flags', (text: string) => text.replace('    flags=[ SYSTEM HAS_CODE ]\n', '')],
    ['ambiguous user record', (text: string) => text.replace('Queries:', '    User 0: installed=true hidden=false suspended=false enabled=0\n      firstInstallTime=2026-01-01 00:00:00\nQueries:')],
    ['malformed dependency values', (text: string) => text.replace('    flags=', '    usesLibraryFiles:\n      missing-absolute-path\n    flags=')],
    ['truncated dump', (text: string) => text.trimEnd()],
    ['missing dump footer', (text: string) => text.split('Queries:')[0]],
    ['oversized dump', (text: string) => text + 'x'.repeat(2_000_001)],
    ['duplicate active record', (text: string) => text + text],
    ['hidden only', (text: string) => text.replace('Packages:', 'Hidden system packages:')],
  ] as const) test(`rejects ${name} through snapshot`, async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    await writeFile(path, mutate(await readFile(path, 'utf8')));
    const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(result.passed, false, name);
  });
  test('ignores resolver/hidden/object-ID/order noise but compares all dependency section values beyond 500 lines', async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const values = Array.from({ length: 650 }, (_, index) => `      /system/framework/library-${index}.jar\n`).join('');
    const expanded = original.replace('    flags=', `    usesLibraryFiles:\n${values}    flags=`);
    await writeFile(path, expanded);
    const { before, after } = await snapshots(fixture);
    const irrelevant = `Activity Resolver Table:\n  module config object-id=changed\n${expanded.replace('(fixture)', '(different)').replace(values, values.trimEnd().split('\n').reverse().join('\n') + '\n')}Hidden system packages:\n  Package [com.google.android.gms] (old):\n    versionName=wrong\n`;
    await writeFile(path, irrelevant);
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.deepEqual((await harness.check(fixture, before, after)).issues, []);
    await writeFile(path, irrelevant.replace('library-649.jar', 'library-649-changed.jar'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
  test('detects a dependency change beyond the old 500 matching-line cap independently of resolver noise', async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const values = Array.from({ length: 650 }, (_, index) => `      /data/app/module-${index}/base.apk\n`).join('');
    const expanded = original.replace('    flags=', `    usesLibraryFiles:\n${values}    flags=`);
    await writeFile(path, expanded);
    const { before, after } = await snapshots(fixture);
    await writeFile(path, expanded.replace('module-649/', 'module-649-changed/'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
  test('synthetic saved normal-helper evidence qualifies only with unchanged valid snapshots', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const first = JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot;
    first.measurement!.processes['100'] = 'com.android.chrome_zygote';
    first.measurement!.processes['559'] = 'system_server';
    await writeFile(before, JSON.stringify(first));
    const name = 'com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:2';
    const record = (time: string, pid: string, tag: string, message: string) => `09-10 08:45:${time} ${pid} ${pid} I ${tag}: ${message}\n`;
    const body = record('09.100', '100', 'Zygote', 'Forked child process 200')
      + record('09.200', '559', 'ActivityManager', `Start proc 200:${name}/u0ai2 for  {com.android.chrome/org.chromium.content.app.SandboxedProcessService0:2}`)
      + record('09.300', '200', 'chromium', '[INFO:child_process_service.cc(72)] ChildProcessService: Exiting child process.')
      + record('09.400', '559', 'ActivityManager', `Killing 200:${name}/u0a145i-8998 (adj 0): isolated not needed`)
      + record('09.500', '100', 'Zygote', 'Process 200 exited cleanly (0)');
    const logFile = join(fixture.root, 'normal.log');
    const operations = join(fixture.root, 'normal-operations.json');
    const output = join(fixture.root, 'normal-check.json');
    await writeFile(operations, '[]');
    const check = async (log: string) => {
      await writeFile(logFile, log);
      const result = checkCLI(fixture, { before, after, log: logFile, operations, output });
      return { ...result, report: JSON.parse(await readFile(output, 'utf8')) };
    };
    const log = marker('08.000', 'START') + body + marker('10.000', 'END');
    const normal = await check(log);
    assert.equal(normal.passed, true, normal.stderr);
    assert.deepEqual(normal.report.forcedRestartEvents, []);
    assert.equal(normal.report.nativeEvents.length, 1);
    assert.equal(normal.report.normalRetirements.length, 1);
    assert.deepEqual(normal.report.eventCounts, { rawEvents: 1, distinctDeathPids: 1, fatalEvents: 0, normalRetirementPids: 1 });
    const audit = record('09.300', '200', 'ThreadPoolForeg', 'type=1400 audit(0.0:238): avc:  denied  { setattr } for  name="arbitrary.txt" dev="dm-46" ino=65621 scontext=u:r:isolated_app:s0:c512,c768 tcontext=u:object_r:app_data_file:s0:c145,c256,c512,c768 tclass=file permissive=0').replace(' I ', ' W ');
    const uid = record('09.250', '200', 'CompatChangeReporter', 'Compat change id reported: 242716250; UID 90002; state: ENABLED');
    const auditLog = log.replace(body, body.replace(record('09.300', '200', 'chromium', '[INFO:child_process_service.cc(72)] ChildProcessService: Exiting child process.'), uid + record('09.300', '200', 'chromium', '[INFO:child_process_service.cc(72)] ChildProcessService: Exiting child process.')) + audit);
    const delayed = await check(auditLog);
    assert.equal(delayed.passed, true, delayed.stderr);
    assert.equal(delayed.report.normalRetirements[0].auditSubjects[0].attribution, 'logd-audit-subject');
    assert.equal(delayed.report.normalRetirements[0].auditSubjects[0].line, audit.trimEnd());
    const conflictingAudit = audit.replace('0.0:238', '0.0:239').replace('scontext=u:r:isolated_app:', 'scontext=u:r:untrusted_app:');
    const childRecord = record('09.300', '200', 'chromium', '[INFO:child_process_service.cc(72)] ChildProcessService: Exiting child process.');
    for (const identity of [
      uid.replace('UID 90002', 'UID 90003'),
      record('09.250', '200', 'cr_SplitCompatApp', 'version=1 processName=com.example isIsolatedProcess=true'),
      record('09.250', '200', 'cr_SplitCompatApp', `version=1 processName=${name} isIsolatedProcess=false`),
    ]) {
      const rejected = await check(log.replace(childRecord, identity + childRecord));
      assert.equal(rejected.passed, false, 'recovery F002 direct identity without audit');
      assert.equal(rejected.report.normalRetirements.length, 0);
      assert.equal(rejected.report.forcedRestartEvents.length, 1);
      assert.equal(rejected.report.passed, false);
    }
    const consistentIdentity = uid + record('09.250', '200', 'cr_SplitCompatApp', `version=1 processName=${name} isIsolatedProcess=true`);
    assert.equal((await check(log.replace(childRecord, consistentIdentity + childRecord))).passed, true);
    const conflicting = await check(auditLog.replace(childRecord, conflictingAudit + childRecord));
    assert.equal(conflicting.passed, false, 'recovery F001 in-lifetime conflicting subject');
    assert.equal(conflicting.report.normalRetirements.length, 0);
    assert.equal(conflicting.report.forcedRestartEvents.length, 1);
    assert.equal(conflicting.report.passed, false);
    for (const altered of [
      auditLog.replace('200 200 W', '200 201 W'), auditLog.replace('permissive=0', 'permissive=1'),
      auditLog.replace('UID 90002', 'UID 90003'), auditLog.replace(uid, ''),
      auditLog.replace(audit, audit + audit), auditLog.replace('arbitrary.txt', 'fatal-signal.txt'),
      auditLog.replace(audit, audit + record('09.300', '200', 'Other', 'unknown post-exit execution')),
    ]) {
      const rejected = await check(altered);
      assert.equal(rejected.passed, false);
      assert.equal(rejected.report.normalRetirements.length, 0);
      assert.equal(rejected.report.forcedRestartEvents.length, 1);
    }
    const deathRecord = record('09.400', '559', 'ActivityManager', `Killing 200:${name}/u0a145i-8998 (adj 0): isolated not needed`);
    for (const [finding, adverse] of [
      ['F001 forward clock', record('09.800', '559', 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: from pid 50')],
      ['F001 truncated attribution', record('09.300', '559', 'ActivityManager', 'Force stopping com.android.chrome')],
      ['F002 reused producer', record('09.000', '42', 'ActivityManager', 'Start proc 559:com.example/u0a123 for service')],
      ['F002 terminated ActivityManager producer', record('09.000', '42', 'Zygote', 'Process 559 exited due to signal 9 (Killed)')],
      ['F002 terminated Zygote producer', record('09.000', '42', 'Zygote', 'Process 100 exited due to signal 9 (Killed)')],
    ]) {
      const altered = finding.startsWith('F002') ? log.replace(body, adverse + body) : log.replace(deathRecord, adverse + deathRecord);
      const rejected = await check(altered);
      assert.equal(rejected.passed, false, finding);
      assert.equal(rejected.report.normalRetirements.length, 0, finding);
      assert.equal(rejected.report.forcedRestartEvents.length, 1, finding);
      assert.equal(rejected.report.passed, false, finding);
    }
    for (const position of ['before fork', 'after exit']) {
      const observation = record('09.300', '200', 'Other', `unattributed lifetime ${position}`);
      const altered = log.replace(body, position === 'before fork' ? observation + body : body + observation);
      const rejected = await check(altered);
      assert.equal(rejected.passed, false, `F003 ${position}`);
      assert.equal(rejected.report.normalRetirements.length, 0, `F003 ${position}`);
      assert.equal(rejected.report.forcedRestartEvents.length, 1, `F003 ${position}`);
      assert.equal(rejected.report.passed, false, `F003 ${position}`);
    }
    for (const [time, user, position] of [['09.000', '0', 'before'], ['09.300', '1', 'during'], ['09.800', '0', 'after']]) {
      const unrelated = record(time, '559', 'ActivityManager', `Force stopping com.android.chrome appid=10145 user=${user}: from pid 50`);
      const altered = position === 'before' ? log.replace(body, unrelated + body)
        : position === 'after' ? log.replace(body, body + unrelated) : log.replace(deathRecord, unrelated + deathRecord);
      assert.equal((await check(altered)).passed, true, position);
    }
    const ordinaryInversion = record('07.984', '888', 'OtherProducer', 'ordinary record');
    const inverted = await check(log.replace(body, ordinaryInversion + body));
    assert.equal(inverted.passed, true);
    assert.equal(inverted.report.normalRetirements.length, 1);
    assert.equal(inverted.report.boundaryDiscordances.length, 1);
    const spilled = await check(log + record('09.400', '559', 'Process', 'Sending signal. PID: 200 SIG: 9'));
    assert.equal(spilled.passed, false);
    assert.equal(spilled.report.normalRetirements.length, 0);
    assert.equal(spilled.report.forcedRestartEvents.length, 2);
    const mixed = await check(log.replace(marker('10.000', 'END'), chromeDeath + marker('10.000', 'END')));
    assert.equal(mixed.passed, false);
    assert.equal(mixed.report.normalRetirements.length, 1);
    assert.equal(mixed.report.forcedRestartEvents.length, 1);
    const last = JSON.parse(await readFile(after, 'utf8')) as AndroidEnvironmentSnapshot;
    last.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['synthetic.config.change'];
    rehashPackage(last.packages['com.google.android.gms']);
    await writeFile(after, JSON.stringify(last));
    const drift = await check(log);
    assert.equal(drift.passed, false);
    assert.equal(drift.report.normalRetirements.length, 1);
    assert.ok(drift.report.issues.some((issue: string) => issue.includes('com.google.android.gms')));
  });
  test('padded epoch measurement framing preserves event attribution and rejects invalid records', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const recordedStart = '         1789081242.368  5536  5536 I HerdrMeasure: f2965e32-5a0d-48c2-beb6-e3a259a020f4 START\n';
    const recordedEnd = '         1789081278.547  6233  6233 I HerdrMeasure: f2965e32-5a0d-48c2-beb6-e3a259a020f4 END\n';
    for (const [path, boundary] of [[before, 'start'], [after, 'end']]) {
      const snapshot = JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot;
      snapshot.measurement = { ...snapshot.measurement!, id: 'f2965e32-5a0d-48c2-beb6-e3a259a020f4', boundary: boundary as 'start' | 'end' };
      await writeFile(path, JSON.stringify(snapshot));
    }
    const operations = join(fixture.root, 'operations.json');
    const logFile = join(fixture.root, 'epoch.log');
    const output = join(fixture.root, 'check.json');
    await writeFile(operations, '[]');
    const check = async (log: string) => {
      await writeFile(logFile, log);
      const result = checkCLI(fixture, { before, after, log: logFile, operations, output });
      return { ...result, report: JSON.parse(await readFile(output, 'utf8')) };
    };
    const recordedDex = '         1789081242.806  5192  5192 I artd    : Dex parent of /product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk is not writable: Read-only file system\n';
    for (const padding of ['         ', '', '\t ', ' \t']) {
      const log = (recordedStart + recordedDex + recordedEnd).replace(/^ +/gmu, padding);
      assert.equal((await check(log)).passed, true, JSON.stringify(padding));
    }
    const death = '         1789081250.000 546 1761 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP\n';
    const module = '\t1789081251.000 1427 7277 I ChimeraCfgMgr: Updating module config: old -> new\n';
    const changed = await check(recordedStart + death + module + recordedEnd);
    assert.equal(changed.passed, false);
    assert.deepEqual(changed.report.forcedRestartEvents, [death.trimEnd(), module.trimEnd()]);
    assert.deepEqual(changed.report.issues, ['native process death, dependency configuration change or package replacement was observed']);
    const unrelated = module.replace('1427', '1486');
    assert.equal((await check(death + recordedStart + unrelated + recordedEnd)).passed, false);
    for (const log of [
      recordedStart, recordedEnd, recordedStart + recordedStart + recordedEnd,
      recordedStart + recordedEnd + recordedEnd, recordedEnd + recordedStart,
      recordedStart + recordedEnd.replace('1789081278.547', '1789081241.000'),
      (recordedStart + recordedEnd).trimEnd(),
      ...['broken log record\n', death.replace('1789081250.000', '1789081250.x00'),
        death.replace('546 1761', 'pid 1761'), death.replace('546 1761', '546 tid'),
        death.replace('1789081250.000', '1789081280.000')].map((body) => recordedStart + body + recordedEnd),
      ...['prefix ', '\v', '\f', '\u00a0'].map((prefix) => prefix + recordedStart + recordedEnd),
      recordedStart.replace('I HerdrMeasure:', 'I Other: HerdrMeasure:') + recordedEnd,
    ]) assert.equal((await check(log)).passed, false, JSON.stringify(log));
    const snapshot = JSON.parse(await readFile(after, 'utf8')) as AndroidEnvironmentSnapshot;
    snapshot.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['com.google.android.gms.fonts.provider.FontsProvider'];
    rehashPackage(snapshot.packages['com.google.android.gms']);
    await writeFile(after, JSON.stringify(snapshot));
    const persistent = await check(recordedStart + recordedEnd);
    assert.equal(persistent.passed, false);
    assert.ok(persistent.report.issues.includes('com.google.android.gms dependency configuration changed'));
  });
  test('recorded push and PR setup notifications are outside the authoritative interval', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operations = join(fixture.root, 'operations.json');
    await writeFile(operations, '[]');
    for (const run of ['34488014724', '34488020101']) {
      const log = join(fixture.root, `${run}.log`);
      const setup = await readFile(repositoryPath(`tests/mobile/unit/fixtures/android-recorded-${run}-setup.log`), 'utf8');
      await writeFile(log, `${setup}09-10 14:25:00.000 2000 2000 I HerdrMeasure: android-test START\n09-10 14:26:00.000 2000 2000 I HerdrMeasure: android-test END\n`);
      const result = checkCLI(fixture, { before, after, log, operations, output: join(fixture.root, 'check.json') });
      assert.equal(result.passed, true, result.stderr);
    }
  });
  for (const [name, log, passed] of [
    ['direct Chrome death', chromeDeath, false], ['benign stack', benign, true],
    ['unrelated module and SIG9 interleaving', '09-10 08:45:09.464 1486 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n09-10 08:45:09.465 1486 7277 I Process : Sending signal. PID: 1486 SIG: 9\n', true],
    ['hypothetical explicit package replacement', '09-10 08:45:09.464 546 7277 I PackageManager: Replacing package com.google.android.gms\n', false],
    ['source-derived installation force-stop', '09-10 08:45:09.464 546 7277 I ActivityManager: Force stopping com.google.android.gms appid=10143 user=-1: installPackageLI\n', false],
    ['package observer', '09-10 08:45:09.464 1073 1073 D ActivityThread: Package [com.android.chrome] reported as REPLACED, but missing application info. Assuming REMOVED.\n', true],
    ['equal config update', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: container:2423359190800 -> container:2423359190800\n', true],
    ['real config update', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: container:2423359190800 -> container:2633329260800\n', false],
    ['new config inside qualification', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: <no config> -> container:2633329260800\n', false],
    ['PID reuse by unrelated process', '09-10 08:45:09.000 546 7277 I ActivityManager: Start proc 6538:com.example.other/u0a200 for service\n09-10 08:45:09.464 6538 7277 I Process : Sending signal. PID: 6538 SIG: 9\n', true],
    ['new Chrome PID death', '09-10 08:45:09.000 546 7277 I ActivityManager: Start proc 7777:com.android.chrome/u0a146 for activity\n09-10 08:45:09.464 7777 7277 I Process : Sending signal. PID: 7777 SIG: 9\n', false],
    ['malformed interval record', 'broken log record\n', false],
  ] as const) test(`check classifies ${name}`, async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    assert.equal((await harness.check(fixture, before, after, log)).passed, passed, name);
  });
  test('retains attributed recorded googlecertificates kill and ignores unrelated interleaved PIDs', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const log = await readFile(repositoryPath('tests/mobile/unit/fixtures/android-recorded-34455964640-googlecertificates.log'), 'utf8');
    const result = await harness.check(fixture, before, after, log);
    assert.equal(result.passed, false);
    const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8')) as { forcedRestartEvents: string[] };
    assert.ok(check.forcedRestartEvents.some((line) => line.includes('PID: 6538 SIG: 9')));
    assert.equal(check.forcedRestartEvents.some((line) => line.includes('PID: 1486 SIG: 9')), false);
  });
  test('missing/unreadable logs, missing/duplicate markers and captured setup deaths fail closed', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operations = join(fixture.root, 'operations.json');
    await writeFile(operations, '[]');
    const check = (log: string) => checkCLI(fixture, { before, after, log, operations, output: join(fixture.root, 'check.json') });
    for (const path of [join(fixture.root, 'missing.log'), fixture.root]) assert.equal(check(path).passed, false);
    const logFile = join(fixture.root, 'interval.log');
    for (const log of ['', benign, marker('00.000', 'START') + marker('00.100', 'START') + marker('30.000', 'END')]) {
      await writeFile(logFile, log);
      assert.equal(check(logFile).passed, false);
    }
    await writeFile(logFile, chromeDeath + marker('10.000', 'START') + marker('30.000', 'END'));
    assert.equal(check(logFile).passed, false);
  });
  test('F001 snapshot attribution and F002 operation reuse survive discordant capture records', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    for (const [path, boundary] of [[before, 'start'], [after, 'end']] as const) {
      const snapshot = JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot;
      snapshot.measurement = { id: 'synthetic', boundary, processes: { '300': 'com.android.chrome' } };
      await writeFile(path, JSON.stringify(snapshot));
    }
    const record = (second: number, tag: string, message: string) => `${(1789106370 + second).toFixed(3)} 559 559 I ${tag}: ${message}\n`;
    const start = record(0, 'HerdrMeasure', 'synthetic START');
    const end = record(9, 'HerdrMeasure', 'synthetic END');
    const signal = record(4, 'Process', 'Sending signal. PID: 300 SIG: 9');
    const history = record(-1, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service');
    const operation = { id: 'stop', measurementId: 'synthetic', packageName: 'com.android.chrome', pid: '300',
      processes: { '300': 'com.android.chrome' }, succeeded: true,
      command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'] };
    const begin = record(2, 'HerdrMeasure', 'synthetic OP_BEGIN stop com.android.chrome 300');
    const finish = record(6, 'HerdrMeasure', 'synthetic OP_END stop com.android.chrome 300');
    const kill = record(3, 'ActivityManager', 'Killing 300:com.android.chrome/u0a145 (adj 0): stop com.android.chrome due to from pid 50');
    const reuse = record(8, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity');
    const cases = [
      { name: 'F001 interior signal', log: history + start + signal + end, operations: [], fatal: 1 },
      { name: 'F001 post-END signal', log: history + start + end + signal, operations: [], fatal: 1 },
      { name: 'F001 delayed historical identity', log: start + history + signal + end, operations: [], fatal: 1 },
      { name: 'unrelated in-window reuse', log: start + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 0 },
      { name: 'F003 post-END reuse before delayed signal', log: start + end + history.replace('1789106369', '1789106380') + signal, operations: [], fatal: 1 },
      { name: 'F003 interior reuse before delayed signal', log: start + history.replace('1789106369', '1789106380') + signal + end, operations: [], fatal: 1 },
      { name: 'F004 post-END delayed relevant identity', log: start + history.replace('1789106369', '1789106372') + end + signal + reuse.replace('1789106378', '1789106373'), operations: [], fatal: 1 },
      { name: 'F004 interior delayed relevant identity', log: start + history.replace('1789106369', '1789106372') + signal + reuse.replace('1789106378', '1789106373') + end, operations: [], fatal: 1 },
      { name: 'F003 discordant identity retirement post-END', log: start + reuse.replace('1789106378', '1789106373') + history.replace('1789106369', '1789106372') + end + signal, operations: [], fatal: 1 },
      { name: 'F003 discordant identity retirement interior', log: start + reuse.replace('1789106378', '1789106373') + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 1 },
      { name: 'ordered Chrome to unrelated identity retirement', log: start + reuse.replace('1789106378', '1789106371') + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 0 },
      { name: 'valid planned operation', log: start + begin + kill + signal + finish + end, operations: [operation], fatal: 0 },
      { name: 'F002 discordant reuse', log: start + begin + reuse + kill + signal + finish + end, operations: [operation], fatal: 2 },
    ];
    for (const entry of cases) {
      const logFile = join(fixture.root, 'boundary.log');
      const operations = join(fixture.root, 'operations.json');
      const output = join(fixture.root, 'check.json');
      await writeFile(logFile, entry.log);
      await writeFile(operations, JSON.stringify(entry.operations));
      const result = checkCLI(fixture, { before, after, log: logFile, operations, output });
      const report = JSON.parse(await readFile(output, 'utf8'));
      assert.equal(result.passed, entry.fatal === 0, entry.name);
      assert.equal(report.forcedRestartEvents.length, entry.fatal, entry.name);
      assert.equal(report.eventCounts.fatalEvents, entry.fatal, entry.name);
      assert.equal(report.normalRetirements.length, 0, entry.name);
    }
  });
  test('planned termination exemption requires the actual operation, matching PID/package and time, never a release-suite label', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operationsFile = join(fixture.root, 'operations.json');
    const logFile = join(fixture.root, 'interval.log');
    const operation = { id: 'cold', measurementId: 'android-test', packageName: 'com.android.chrome', pid: '6538', processes: { '6538': 'com.android.chrome' }, command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'], succeeded: true };
    const termination = marker('08.000', 'OP_BEGIN cold com.android.chrome 6538')
      + '09-10 08:45:09.000 546 1761 I ActivityManager: Killing 6538:com.android.chrome/u0a146 (adj 0): stop com.android.chrome due to from pid 2000\n'
      + chromeDeath + marker('10.000', 'OP_END cold com.android.chrome 6538');
    const check = () => checkCLI(fixture, { before, after, log: logFile, operations: operationsFile, output: join(fixture.root, 'check.json') });
    for (const [operations, log, passed] of [
      [[operation], termination, true], [[], termination, false], [[{ ...operation, succeeded: false }], termination, false],
      [[{ ...operation, pid: '7777' }], termination, false], [[{ ...operation, packageName: 'com.example.other' }], termination, false],
      [[operation], termination + chromeDeath.replace('09.613', '12.613'), false],
      [[operation], termination.replace('09-10 08:45:09.000', chromeDeath.replace('09.613', '08.500') + '09-10 08:45:09.000'), false],
      [[operation], termination.replace('stop com.android.chrome due to from pid 2000', 'crash'), false],
      [[operation], termination + '09-10 08:45:09.464 6538 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n', false],
    ] as const) {
      await writeFile(operationsFile, JSON.stringify(operations));
      const interval = marker('00.000', 'START') + log + marker('30.000', 'END');
      for (const framed of [interval, interval.replace(/^09-10 08:45:(\d{2}\.\d{3})/gmu, (_, seconds: string) => `         ${(1789081200 + Number(seconds)).toFixed(3)}`)]) {
        await writeFile(logFile, framed);
        assert.equal(check().passed, passed);
      }
    }
  });
  for (const packageName of ['com.android.chrome', 'org.chromium.webapk.fixture', 'com.google.android.webapk.fixture']) {
    for (const variant of ['planned', 'unplanned', 'unobserved child', 'missing Killing', 'wrong reason', 'wrong user', 'before Killing', 'delayed earlier death', 'before operation', 'after operation', 'reused PID', 'wrong observed name', 'missing process set', 'empty process set', 'malformed process set', 'module change', 'GMS death']) {
      test(`review: ${packageName} process-set check ${variant}`, async () => {
        const fixture = await harness.createFixture();
        const pid = packageName === 'com.android.chrome' ? '6538' : '6600';
        const childName = `${packageName}:renderer`;
        await state(fixture, { absent: true, ...(pid === '6600' ? { foregroundPackage: packageName } : {}), children: { '6700': childName } });
        const { before, after } = await snapshots(fixture);
        const operation = {
          id: 'cold', measurementId: 'android-test', packageName, pid,
          processes: { [pid]: packageName, '6700': childName },
          command: ['shell', 'am', 'force-stop', '--user', '0', packageName], succeeded: true,
        };
        const line = (time: string, tag: string, message: string) => `09-10 08:45:${time} 546 1761 I ${tag}: ${message}\n`;
        const killed = (targetPid: string, name: string) => line(targetPid === pid ? '09.000' : '09.150', 'ActivityManager', `Killing ${targetPid}:${name}/u0a146 (adj 0): stop ${packageName} due to from pid 2000`);
        const death = (time: string, targetPid: string, name: string) => line(time, 'ActivityManager', `Process ${name} (pid ${targetPid}) has died: fg TOP`);
        let body = killed(pid, packageName) + death('09.100', pid, packageName)
          + killed('6700', childName) + death('09.200', '6700', childName)
          + line('09.300', 'Process', 'Sending signal. PID: 6700 SIG: 9')
          + line('09.400', 'Zygote', 'Process 6700 exited due to signal 9 (Killed)');
        if (variant === 'unobserved child') body += killed('6701', `${packageName}:unobserved`);
        if (variant === 'missing Killing') body = body.replace(killed('6700', childName), '');
        if (variant === 'wrong reason') body = body.replaceAll(`stop ${packageName} due to from pid 2000`, 'crash');
        if (variant === 'wrong user') body = body.replaceAll('/u0a146', '/u10a146');
        if (variant === 'before Killing') body = death('08.500', '6700', childName) + body;
        if (variant === 'delayed earlier death') body += death('08.500', '6700', childName);
        if (variant === 'reused PID') body += line('09.500', 'ActivityManager', `Start proc 6700:${childName}/u0a146 for service`) + death('09.600', '6700', childName);
        if (variant === 'module change') body += `09-10 08:45:09.500 6700 6700 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n`;
        if (variant === 'GMS death') body += death('09.500', '1427', 'com.google.android.gms');
        const operations = variant === 'unplanned' ? [] : [{ ...operation,
          ...(variant === 'wrong observed name' ? { processes: { [pid]: packageName, '6700': `${packageName}:other` } } : {}),
          ...(variant === 'missing process set' ? { processes: undefined } : {}),
          ...(variant === 'empty process set' ? { processes: {} } : {}),
          ...(variant === 'malformed process set' ? { processes: { [pid]: packageName, '6700': null } } : {}),
        }];
        const log = marker('00.000', 'START') + (variant === 'before operation' ? death('07.000', pid, packageName) : '') + (variant === 'unplanned' ? body : marker('08.000', `OP_BEGIN cold ${packageName} ${pid}`) + body + marker('10.000', `OP_END cold ${packageName} ${pid}`))
          + (variant === 'after operation' ? death('12.000', '6700', childName) : '') + marker('30.000', 'END');
        const logFile = join(fixture.root, 'interval.log');
        const operationsFile = join(fixture.root, 'operations.json');
        await writeFile(logFile, log);
        await writeFile(operationsFile, JSON.stringify(operations));
        const result = checkCLI(fixture, { before, after, log: logFile, operations: operationsFile, output: join(fixture.root, 'check.json') });
        const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
        assert.equal(result.passed, variant === 'planned', JSON.stringify(check));
        if (variant === 'planned') assert.deepEqual(check.forcedRestartEvents, []);
        else if (variant !== 'missing process set') assert.ok(check.forcedRestartEvents.length > 0, JSON.stringify(check));
      });
    }
    for (const event of ['direct death', 'new PID signal']) test(`review: unplanned ${packageName} ${event} cannot pass without a termination journal`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { absent: true, ...(packageName === 'com.android.chrome' ? {} : { foregroundPackage: packageName }) });
      const { before, after } = await snapshots(fixture);
      const pid = packageName === 'com.android.chrome' ? '6538' : '6600';
      const log = event === 'new PID signal'
        ? `09-10 08:45:09.000 546 1761 I ActivityManager: Start proc 6800:${packageName}:renderer/u0a146 for service\n09-10 08:45:09.100 6800 6800 I Process: Sending signal. PID: 6800 SIG: 9\n`
        : `09-10 08:45:09.000 546 1761 I ActivityManager: Process ${packageName} (pid ${pid}) has died: fg TOP\n`;
      assert.equal((await harness.check(fixture, before, after, log)).passed, false);
      const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
      assert.equal(check.forcedRestartEvents.length, 1);
    });
  }
  test('review: hypothetical initial WebAPK publication inside installation is not a dependency replacement, but its new process death is measured', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const publication = '09-10 08:45:09.000 546 1761 I PackageManager: Successfully installed package org.chromium.webapk.fixture\n'
      + '09-10 08:45:09.100 546 1761 I ActivityManager: Start proc 6600:org.chromium.webapk.fixture/u0a146 for activity\n';
    assert.equal((await harness.check(fixture, before, after, publication)).passed, true);
    assert.equal((await harness.check(fixture, before, after, publication + '09-10 08:45:09.200 6600 6600 I Process: Sending signal. PID: 6600 SIG: 9\n')).passed, false);
  });
  test('current recorded static-library evidence retains complete records and sanitized PR path labels', async () => {
    const entries = JSON.parse(await readFile(repositoryPath('tests/mobile/unit/fixtures/android-iteration-13-evidence.json'), 'utf8')) as Array<{ file: string; sha256: string; classification: string }>;
    for (const entry of entries) assert.equal(await fileSha256(repositoryPath(`tests/mobile/unit/fixtures/${entry.file}`)), entry.sha256);
    for (const run of ['34488014724', '34488020101']) {
      const fixture = await harness.createFixture();
      for (const [suffix, recorded] of [['dump', 'trichrome.dump'], ['list', 'libraries.list']]) {
        await writeFile(join(fixture.fixtureDirectory, `valid-trichrome.${suffix}`), await readFile(repositoryPath(`tests/mobile/unit/fixtures/android-recorded-${run}-${recorded}`)));
      }
      const dump = await readFile(join(fixture.fixtureDirectory, 'valid-trichrome.dump'), 'utf8');
      const path = dump.match(/codePath=(\S+)/u)![1] + '/base.apk';
      await writeFile(join(fixture.fixtureDirectory, 'valid-trichrome.file'), `'${path}'`);
      const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
      if (run === '34488014724') assert.equal(result.passed, true, result.stderr);
      else {
        assert.equal(result.passed, false);
        assert.ok(dump.includes('[REDACTED]'));
        assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
        assert.ok(entries.filter((entry) => entry.file.includes(run) && /trichrome|libraries/u.test(entry.file)).every((entry) => entry.classification === 'recorded-sanitized'));
        const alias = '/data/app/hypothetical-pr-library';
        for (const suffix of ['dump', 'list']) {
          const filename = join(fixture.fixtureDirectory, `valid-trichrome.${suffix}`);
          const text = await readFile(filename, 'utf8');
          await writeFile(filename, text.replace(/\/data\/app\/~~40L0KnTK29Prxai8pgaj7g==\/com\.google\.android\.\d+\[REDACTED\]-tEf5g==/gu, alias));
        }
        await writeFile(join(fixture.fixtureDirectory, 'valid-trichrome.file'), `'${alias}/base.apk'`);
        const hypothetical = await harness.snapshot(fixture, 'valid', join(fixture.root, 'hypothetical-before.json'), join(fixture.root, 'hypothetical-diagnostics.json'));
        assert.equal(hypothetical.passed, true, `Hypothetical consistent alias, not recovered PR paths: ${hypothetical.stderr}`);
      }
    }
  });
  for (const packageName of ['com.android.chrome', 'org.chromium.webapk.fixture', 'com.google.android.webapk.fixture']) test(`review: measurement collector and real planned ${packageName} process-set force-stop persist a checkable interval without rebaseline`, async () => {
    const fixture = await harness.createFixture();
    const childName = `${packageName}:renderer`;
    const initial = { absent: true, ...(packageName === 'com.android.chrome' ? {} : { foregroundPackage: packageName }) };
    await state(fixture, initial);
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await measurement.begin();
      await assert.rejects(measurement.begin(), /rebaseline/u);
      const baseline = JSON.parse(await readFile(join(fixture.root, 'android-environment-before.json'), 'utf8'));
      assert.equal(baseline.measurement.processes['6700'], undefined);
      await state(fixture, { ...initial, children: { '6700': childName } });
      await assert.rejects(measurement.terminate(packageName, '7777'), /PID changed/u);
      await measurement.terminate(packageName, packageName === 'com.android.chrome' ? '6538' : '6600');
      await measurement.finish();
      const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
      assert.equal(check.passed, true, JSON.stringify(check));
      const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
      assert.equal(operations.length, 1);
      assert.equal(operations[0].succeeded, true);
      assert.equal(operations[0].pid, packageName === 'com.android.chrome' ? '6538' : '6600');
      assert.equal(operations[0].packageName, packageName);
      assert.deepEqual(operations[0].processes, { [operations[0].pid]: packageName, '6700': childName });
      assert.deepEqual(check.forcedRestartEvents, []);
      const requests = await readFile(fixture.log, 'utf8');
      const stop = requests.indexOf(`shell am force-stop --user 0 ${packageName}`);
      assert.ok(requests.lastIndexOf('shell ps -A -o PID,NAME', stop) > requests.lastIndexOf(`shell pidof ${packageName}`, stop));
      assert.ok(requests.indexOf('shell ps -A -o PID,NAME', stop) > stop);
      assert.equal(requests.includes('disable-user'), false);
      await assert.rejects(measurement.terminate('com.android.chrome', '6538'), /outside measurement/u);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  for (const variant of ['inventory denied', 'malformed inventory', 'main PID changed', 'remaining child', 'new remaining child', 'force-stop denied', 'missing child Killing', 'unobserved child']) {
    test(`review: measured termination fails closed for ${variant}`, async () => {
      const fixture = await harness.createFixture();
      const childName = 'com.android.chrome:renderer';
      const children = { '6700': childName };
      await state(fixture, { absent: true, children });
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
      try {
        await measurement.begin();
        const changes = {
          'inventory denied': { processListFail: true },
          'malformed inventory': { processList: 'PID NAME\nmalformed\n' },
          'main PID changed': { processList: 'PID NAME\n6539 com.android.chrome\n6700 com.android.chrome:renderer\n' },
          'remaining child': { remainingChildren: children },
          'new remaining child': { remainingChildren: { '6701': 'com.android.chrome:new' } },
          'force-stop denied': { forceStopFail: true },
          'missing child Killing': { omitKillingPid: '6700' },
          'unobserved child': { unobservedChildren: { '6701': 'com.android.chrome:new' } },
        }[variant];
        await state(fixture, { absent: true, children, ...changes });
        if (variant === 'missing child Killing' || variant === 'unobserved child') {
          await measurement.terminate('com.android.chrome', '6538');
          const outcome = await measurement.finish();
          assert.equal(outcome.assessment?.status, 'FAIL');
          const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
          assert.equal(check.passed, false);
          assert.ok(check.forcedRestartEvents.some((line: string) => line.includes(variant === 'unobserved child' ? '6701' : '6700')));
          return;
        }
        await assert.rejects(measurement.terminate('com.android.chrome', '6538'));
        const requests = await readFile(fixture.log, 'utf8');
        const dispatched = !['inventory denied', 'malformed inventory', 'main PID changed'].includes(variant);
        assert.equal(requests.includes('shell am force-stop --user 0'), dispatched);
        const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
        assert.equal(operations.length, dispatched ? 1 : 0);
        if (dispatched) {
          assert.equal(operations[0].succeeded, false);
          assert.deepEqual(operations[0].processes, { '6538': 'com.android.chrome', ...children });
          assert.doesNotMatch(await readFile(join(fixture.fixtureDirectory, 'native.log'), 'utf8'), /OP_END/u);
        }
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    });
  }
  test('measurement collector failure is fatal and cannot leave an active descendant', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { absent: true, logcatFail: true });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await assert.rejects(measurement.begin(), /collector/u);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.some(error => /no active measurement/u.test(String(error))));
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.ok(collector.failure);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS failed-begin cleanup retires the owned collector tree and freezes its callbacks', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const readyPath = join(fixture.root, 'collector-descendant-ready');
    const latePath = join(fixture.root, 'collector-descendant-late-write');
    const childCode = `process.on('SIGTERM',()=>{});const fs=require('node:fs');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.stdout.write('late\\n')},1500);setTimeout(()=>process.exit(0),6000);setInterval(()=>process.stdout.write('streaming\\n'),20)`;
    const parentCode = `const fs=require('node:fs');const cp=require('node:child_process');cp.spawn(process.execPath,['-e',${JSON.stringify(childCode)}],{stdio:'inherit'});fs.writeFileSync(${JSON.stringify(readyPath)},'ready');process.on('SIGTERM',()=>process.exit(0));setTimeout(()=>process.exit(0),6000);setInterval(()=>{},1000)`;
    let collectorPid: number | undefined;
    let collectorClosed = false;
    let collectorOwned: ReturnType<typeof spawnOwnedProcess> | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      spawnCollector: () => {
        collectorOwned = spawnOwnedProcess(process.execPath, ['-e', parentCode], { timeoutMs: 10_000, cleanupReservationMs: 1_000 });
        collectorOwned.supervisor.once('close', () => { collectorClosed = true; });
        collectorOwned.started.then(binding => { collectorPid = binding?.groupID; });
        return collectorOwned;
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot')) {
          const readyDeadline = Date.now() + 2_000;
          while (!existsSync(readyPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
          assert.ok(existsSync(readyPath));
          throw new Error('synthetic failed-begin snapshot after collector launch');
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-collector-tree', { timeoutMs: 15_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-collector-tree', { timeoutMs: 15_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot after collector launch/u);
      assert.ok(collectorPid);
      assert.equal(collectorClosed, true);
      assert.throws(() => process.kill(-collectorPid!, 0), { code: 'ESRCH' });
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.some(error => String(error).includes('synthetic failed-begin snapshot after collector launch')));
      const collectorPath = join(fixture.root, 'android-environment-collector.json');
      const logPath = join(fixture.root, 'android-qualification-logcat.log');
      const collectorBytes = await readFile(collectorPath);
      const logBytes = await readFile(logPath);
      await new Promise(resolve => setTimeout(resolve, 1_600));
      assert.equal(existsSync(latePath), false);
      assert.deepEqual(await readFile(collectorPath), collectorBytes);
      assert.deepEqual(await readFile(logPath), logBytes);
      assert.throws(() => process.kill(-collectorPid!, 0), { code: 'ESRCH' });
    } finally {
      await measurement.finish().catch(() => undefined);
      await collectorOwned?.retire('test-cleanup').catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('canonical GMS parser rejects malformed headings and contradictory component sections', async () => {
    const fixture = await harness.createFixture();
    const source = await readFile(join(fixture.fixtureDirectory, 'valid-gms.dump'), 'utf8');
    assert.deepEqual(parseAndroidGmsComponentState(source).enabledComponents, []);
    const missingColon = source.replace('Queries:', '      enabledComponents\n        com.google.android.gms.fixture.Component\nQueries:');
    const equalsHeading = source.replace('Queries:', '      enabledComponents=\n        com.google.android.gms.fixture.Component\nQueries:');
    const firstInstallLine = source.split('\n').find(line => line.includes('firstInstallTime='))!;
    const misplacedHeading = source.replace(firstInstallLine,
      `${firstInstallLine}\n    enabledComponents:\n      com.google.android.gms.fixture.Component`);
    const packageBoundaryHeading = source.replace('Queries:',
      '  enabledComponents:\n    com.google.android.gms.fixture.Component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(missingColon), /malformed enabledComponents section heading/u);
    assert.throws(() => parseAndroidGmsComponentState(equalsHeading), /malformed enabledComponents section heading/u);
    assert.throws(() => parseAndroidGmsComponentState(misplacedHeading), /outside User 0/u);
    assert.throws(() => parseAndroidGmsComponentState(packageBoundaryHeading), /malformed or outside its package record/u);
    const contradictory = source.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.Component\n      disabledComponents:\n        com.google.android.gms.fixture.Component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(contradictory), /contradict/u);
    const invalidMember = source.replace('Queries:', '      enabledComponents:\n        not-a-component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(invalidMember), /malformed enabledComponents component names/u);
  });
  test('GMS ownership failures are excluded from dump output and never dispatch dumpsys', async () => {
    for (const failure of ['wrong-owner', 'timeout'] as const) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      if (failure === 'wrong-owner') await writeFile(fixture.environment.MOBILE_DEVICE_OWNERSHIP_FILE!, 'android:another-device');
      let ownershipFailure: BoundedCommandError | undefined;
      const observer = failure === 'timeout' ? createBoundedSupervisorObserver(fixture.root, 'gms-ownership-supervisor') : undefined;
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        boundedCommand: async (binary, args, timeout, options) => {
          if (failure !== 'timeout' || !args.includes('--gms-diagnostic-ownership')) return boundedCommand(binary, args, timeout, options);
          const owner = `process.on('SIGTERM',()=>{});process.stdout.write('OWNERSHIP-ONLY\\n');process.stderr.write('owner detail\\n');setInterval(()=>{},1000)`;
          try { return await boundedCommand('node', ['-e', owner], timeout, { ...options, observerSupervisor: observer!.stream }); }
          catch (error) { if (error instanceof BoundedCommandError) ownershipFailure = error; throw error; }
        },
      });
      try {
        measurement.requireGmsObservations();
        await measurement.observeGmsStage('after-device-start', new PhaseBudget(`gms-owner-${failure}`, { timeoutMs: 10_000 }));
        await measurement.finalizeGmsObservations();
        const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        const stage = observations.stages[0];
        await observer?.close();
        const details = JSON.stringify({ failure, stage: {
          ownershipChildExited: stage.ownershipChildExited, ownershipProcessesExited: stage.ownershipProcessesExited,
          ownershipStdioClosed: stage.ownershipStdioClosed,
        }, command: boundedCommandFailureDetails(ownershipFailure?.result, observer?.records()) });
        const requests = existsSync(fixture.log) ? await readFile(fixture.log, 'utf8') : '';
        assert.equal(requests.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, 0, failure);
        assert.equal(observations.totalCommandOutputBytes, 0, failure);
        assert.equal(observations.totalRetainedCommandOutputBytes, 0, failure);
        assert.equal(stage.stdoutBytes, undefined, failure);
        assert.equal(stage.stderrBytes, undefined, failure);
        assert.equal(stage.stdoutSha256, undefined, failure);
        assert.ok(stage.ownershipChildExited, details);
        assert.ok(stage.ownershipProcessesExited, details);
        assert.ok(stage.ownershipStdioClosed, details);
        if (failure === 'timeout') {
          assert.ok(ownershipFailure instanceof BoundedCommandError, details);
          assert.equal(ownershipFailure.result.timedOut, true, details);
          assert.ok(ownershipFailure.result.stdoutBytes > 0, details);
          assert.ok(ownershipFailure.result.stderrBytes > 0, details);
        }
      } finally {
        await observer?.close();
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('GMS stages require premeasurement calls, a successful begin, and an active measurement', async () => {
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    let persistenceCalls = 0;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--gms-diagnostic-persist')) persistenceCalls++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      for (const stage of stages.slice(0, 2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-no-begin', { timeoutMs: 10_000 }));
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-no-begin', { timeoutMs: 10_000 }));
      await measurement.finalizeGmsObservations();
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.deepEqual(observation.stages.map((entry: any) => entry.stage), stages, JSON.stringify({
        stages: observation.stages, errors: observation.errors, persistenceCalls,
      }));
      assert.equal(observation.stages.filter((entry: any) => entry.outcome === 'collected').length, 2, JSON.stringify(observation.stages));
      assert.ok(observation.stages.slice(2).every((entry: any) => entry.outcome === 'failed'));
      assert.ok(observation.errors.length > 0);
      assert.equal(persistenceCalls, 3);
      const requests = await readFile(fixture.log, 'utf8');
      assert.equal(requests.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, 2);
      assert.ok(measurement.failureOutcome().errors.length > 0);
      await assert.rejects(measurement.observeGmsStage(stages[2], new PhaseBudget('gms-after-finalize', { timeoutMs: 1_000 })), /finalized/u);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }

    for (const failBegin of [false, true]) {
      const current = await harness.createFixture();
      const prior = { ...process.env };
      Object.assign(process.env, current.environment);
      const io: Partial<AndroidMeasurementIO> = failBegin ? {
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('snapshot')) throw new Error('synthetic begin snapshot failure');
          return boundedCommand(binary, args, timeout, options);
        },
      } : {};
      const guarded = new AndroidEnvironmentMeasurement('emulator-5554', current.root, repositoryPath('tests/mobile/toolchains.json'), io);
      try {
        guarded.requireGmsObservations();
        if (failBegin) {
          for (const stage of stages.slice(0, 2)) await guarded.observeGmsStage(stage, new PhaseBudget('gms-failed-begin', { timeoutMs: 10_000 }));
          await assert.rejects(guarded.begin(), /synthetic begin snapshot failure/u);
        } else await assert.rejects(guarded.begin(), /requires both ordered pre-measurement/u);
        const before = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
        const gmsReadsBefore = before.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length;
        await guarded.observeGmsStage(failBegin ? stages[2] : stages[0], new PhaseBudget('gms-rejected-post', { timeoutMs: 10_000 }));
        await guarded.finalizeGmsObservations();
        const after = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
        assert.equal(after.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, gmsReadsBefore);
        const observation = JSON.parse(await readFile(join(current.root, 'android-environment-gms-observations.json'), 'utf8'));
        assert.ok(observation.errors.length > 0);
        assert.equal(observation.stages.at(-1).outcome, 'failed');
      } finally {
        for (const key of Object.keys(process.env)) if (!(key in prior)) delete process.env[key];
        Object.assign(process.env, prior);
      }
    }

    const duplicateFixture = await harness.createFixture();
    const duplicateEnv = { ...process.env };
    Object.assign(process.env, duplicateFixture.environment);
    let duplicateDispatches = 0;
    const duplicate = new AndroidEnvironmentMeasurement('emulator-5554', duplicateFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('dumpsys')) duplicateDispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      duplicate.requireGmsObservations();
      await duplicate.observeGmsStage(stages[0], new PhaseBudget('gms-duplicate', { timeoutMs: 10_000 }));
      await duplicate.observeGmsStage(stages[0], new PhaseBudget('gms-duplicate', { timeoutMs: 10_000 }));
      await duplicate.finalizeGmsObservations();
      assert.equal(duplicateDispatches, 1);
      const observation = JSON.parse(await readFile(join(duplicateFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.deepEqual(observation.stages.map((entry: any) => entry.outcome), ['collected', 'failed']);
      assert.ok(observation.errors.length > 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in duplicateEnv)) delete process.env[key];
      Object.assign(process.env, duplicateEnv);
    }

    const concurrentFixture = await harness.createFixture();
    const concurrentEnv = { ...process.env };
    Object.assign(process.env, concurrentFixture.environment);
    let entered!: () => void;
    let release!: () => void;
    const commandEntered = new Promise<void>(resolve => { entered = resolve; });
    const commandGate = new Promise<void>(resolve => { release = resolve; });
    let concurrentDispatches = 0;
    const concurrent = new AndroidEnvironmentMeasurement('emulator-5554', concurrentFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('dumpsys')) {
          concurrentDispatches++;
          entered();
          await commandGate;
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    let first: Promise<void> | undefined;
    try {
      concurrent.requireGmsObservations();
      first = concurrent.observeGmsStage(stages[0], new PhaseBudget('gms-concurrent', { timeoutMs: 10_000 }));
      await commandEntered;
      await concurrent.observeGmsStage(stages[1], new PhaseBudget('gms-concurrent', { timeoutMs: 10_000 }));
      release();
      await first;
      await concurrent.finalizeGmsObservations();
      assert.equal(concurrentDispatches, 1);
      const observation = JSON.parse(await readFile(join(concurrentFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.ok(observation.errors.some((error: string) => /cannot overlap/u.test(error)));
    } finally {
      release();
      await first?.catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in concurrentEnv)) delete process.env[key];
      Object.assign(process.env, concurrentEnv);
    }

    const finishedFixture = await harness.createFixture();
    const finishedEnv = { ...process.env };
    Object.assign(process.env, finishedFixture.environment);
    let finishedSpawns = 0;
    const finished = new AndroidEnvironmentMeasurement('emulator-5554', finishedFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      spawnCollector: () => { finishedSpawns++; throw new Error('collector must not start after finish'); },
    });
    try {
      finished.requireGmsObservations();
      for (const stage of stages.slice(0, 2)) await finished.observeGmsStage(stage, new PhaseBudget('gms-finish-before-begin', { timeoutMs: 10_000 }));
      await finished.finish();
      await assert.rejects(finished.begin(), /cannot begin after start or finalization/u);
      assert.equal(finishedSpawns, 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in finishedEnv)) delete process.env[key];
      Object.assign(process.env, finishedEnv);
    }

    const interruptedFixture = await harness.createFixture();
    const interruptedEnv = { ...process.env };
    Object.assign(process.env, interruptedFixture.environment);
    let deferOwnership = false;
    let beginOwnerEntered!: () => void;
    const ownerEntered = new Promise<void>(resolve => { beginOwnerEntered = resolve; });
    let beginSpawns = 0;
    let ownerFailure: BoundedCommandError | undefined;
    const interrupted = new AndroidEnvironmentMeasurement('emulator-5554', interruptedFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!deferOwnership || !args.includes('--android-measurement-owner')) return boundedCommand(binary, args, timeout, options);
        const stalledOwner = `process.on('SIGTERM',()=>{});require('node:fs').writeFileSync(${JSON.stringify(join(interruptedFixture.root, 'begin-owner-ready'))},'ready');setInterval(()=>{},1000)`;
        beginOwnerEntered();
        try { return await boundedCommand('node', ['-e', stalledOwner], timeout, options); }
        catch (error) { if (error instanceof BoundedCommandError) ownerFailure = error; throw error; }
      },
      spawnCollector: () => { beginSpawns++; throw new Error('collector must not start after concurrent finalization'); },
    });
    let pendingBegin: Promise<void> | undefined;
    try {
      interrupted.requireGmsObservations();
      for (const stage of stages.slice(0, 2)) await interrupted.observeGmsStage(stage, new PhaseBudget('gms-concurrent-begin', { timeoutMs: 10_000 }));
      deferOwnership = true;
      pendingBegin = interrupted.begin();
      await ownerEntered;
      const readyDeadline = Date.now() + 2_000;
      while (!existsSync(join(interruptedFixture.root, 'begin-owner-ready')) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      assert.ok(existsSync(join(interruptedFixture.root, 'begin-owner-ready')));
      const finishPromise = interrupted.finish();
      await assert.rejects(pendingBegin, /BOUNDED_COMMAND_FAILED/u);
      await finishPromise;
      assert.equal(beginSpawns, 0);
      assert.ok(ownerFailure instanceof BoundedCommandError);
      assert.equal(ownerFailure.result.ownedProcessesExited, true);
      assert.equal(ownerFailure.result.stdioClosed, true);
      assert.equal(interrupted.failureOutcome().errors.length > 0, true);
    } finally {
      await pendingBegin?.catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in interruptedEnv)) delete process.env[key];
      Object.assign(process.env, interruptedEnv);
    }
  });
  test('GMS finalization retires the owned snapshot subprocess tree before begin settles', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin'] as const;
    const readyPath = join(fixture.root, 'snapshot-child-ready');
    const latePath = join(fixture.root, 'snapshot-child-late');
    let entered!: () => void;
    const snapshotEntered = new Promise<void>(resolve => { entered = resolve; });
    const descendant = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1200);setInterval(()=>{},1000)`;
    const parent = `process.on('SIGTERM',()=>{});const cp=require('node:child_process');cp.spawn(process.execPath,['-e',${JSON.stringify(descendant)}],{stdio:'inherit'});require('node:fs').writeFileSync(${JSON.stringify(readyPath)},'ready');setInterval(()=>{},1000)`;
    let snapshotFailure: BoundedCommandError | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('snapshot')) return boundedCommand(binary, args, timeout, options);
        entered();
        try { return await boundedCommand('node', ['-e', parent], timeout, options); }
        catch (error) { if (error instanceof BoundedCommandError) snapshotFailure = error; throw error; }
      },
    });
    let pendingBegin: Promise<void> | undefined;
    try {
      measurement.requireGmsObservations();
      for (const stage of stages) await measurement.observeGmsStage(stage, new PhaseBudget('gms-snapshot-retirement', { timeoutMs: 20_000 }));
      pendingBegin = measurement.begin();
      await snapshotEntered;
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(readyPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      assert.ok(existsSync(readyPath));
      const finish = measurement.finish();
      await assert.rejects(pendingBegin, /BOUNDED_COMMAND_FAILED/u);
      await finish;
      assert.ok(snapshotFailure instanceof BoundedCommandError);
      assert.equal(snapshotFailure.result.childExited, true);
      assert.equal(snapshotFailure.result.ownedProcessesExited, true);
      assert.equal(snapshotFailure.result.stdioClosed, true);
      await new Promise(resolve => setTimeout(resolve, 1_300));
      assert.equal(existsSync(latePath), false);
      const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.ok(observations.errors.length > 0);
    } finally {
      await pendingBegin?.catch(() => undefined);
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('failed-begin collector publication waits for terminal GMS evidence and records its result', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin'] as const;
    const output = fixture.root;
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let entered!: () => void;
    const collectorWriteEntered = new Promise<void>(resolve => { entered = resolve; });
    const finalWriteDelayMs = 90;
    let terminalWriteAttempts = 0;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', output, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot')) throw new Error('synthetic failed-begin snapshot');
        if (args.includes('--gms-diagnostic-persist') && String(options.input || '').includes('"finalized":true')) {
          terminalWriteAttempts++;
          await new Promise(resolve => setTimeout(resolve, finalWriteDelayMs));
        }
        if (args.includes('--android-measurement-persist-json')) {
          const request = JSON.parse(String(options.input));
          if (request.target.endsWith('android-environment-collector.json')) {
            entered();
            await gate;
          }
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      for (const stage of stages) await measurement.observeGmsStage(stage, new PhaseBudget('failed-begin-publication', { timeoutMs: 15_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot/u);
      const collectorPath = join(output, 'android-environment-collector.json');
      assert.equal(existsSync(collectorPath), false);
      const observationPath = join(output, 'android-environment-gms-observations.json');
      const prior = await readFile(observationPath);
      const finalization = measurement.finalizeGmsObservations();
      await collectorWriteEntered;
      let firstCompleted = false;
      let secondCompleted = false;
      let finishCompleted = false;
      const secondFinalization = measurement.finalizeGmsObservations().then(() => { secondCompleted = true; });
      const finish = measurement.finish().then(() => { finishCompleted = true; });
      void finalization.then(() => { firstCompleted = true; });
      await new Promise(resolve => setTimeout(resolve, 40));
      assert.equal(firstCompleted, false);
      assert.equal(secondCompleted, false);
      assert.equal(finishCompleted, false);
      assert.equal(existsSync(collectorPath), false);
      const terminalObservations = JSON.parse(await readFile(observationPath, 'utf8'));
      assert.equal(terminalObservations.finalized, true);
      assert.equal(terminalObservations.terminalPublication, 'awaiting-collector-confirmation');
      assert.equal(terminalWriteAttempts, 1);
      assert.notDeepEqual(await readFile(observationPath), prior);
      release();
      await Promise.all([finalization, secondFinalization, finish]);
      const observations = JSON.parse(await readFile(observationPath, 'utf8'));
      const collector = JSON.parse(await readFile(collectorPath, 'utf8'));
      assert.equal(observations.finalized, true);
      assert.ok(observations.errors.length > 0);
      assert.equal(collector.gmsObservations.finalized, true);
      assert.equal(collector.gmsObservations.errors, observations.errors.length);
      assert.equal(collector.gmsObservations.persistenceAttemptMsThroughFinalization >= observations.priorPersistenceAttemptMs + finalWriteDelayMs, true);
      assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
      assert.equal(collector.gmsObservations.terminalConfirmation, 'confirmed');
      assert.ok(collector.failure);
    } finally {
      release();
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS failed-begin summary write failure settles every concurrent finalizer with sticky errors', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let entered!: () => void;
    const writerEntered = new Promise<void>(resolve => { entered = resolve; });
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot')) throw new Error('synthetic failed-begin snapshot');
        if (args.includes('--android-measurement-persist-json')) {
          const request = JSON.parse(String(options.input));
          if (request.target.endsWith('android-environment-collector.json')) {
            entered();
            await gate;
            throw new Error('synthetic terminal collector write failure');
          }
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('failed-begin-summary-failure', { timeoutMs: 15_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('failed-begin-summary-failure', { timeoutMs: 15_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot/u);
      let firstReturned = false;
      let secondReturned = false;
      let finishReturned = false;
      const first = measurement.finalizeGmsObservations().finally(() => { firstReturned = true; });
      await writerEntered;
      const second = measurement.finalizeGmsObservations().finally(() => { secondReturned = true; });
      const settledFinalizers = Promise.allSettled([first, second]);
      const finish = measurement.finish().then(() => { finishReturned = true; });
      await new Promise(resolve => setTimeout(resolve, 40));
      assert.equal(firstReturned, false);
      assert.equal(secondReturned, false);
      assert.equal(finishReturned, false);
      release();
      await finish;
      const finalizers = await settledFinalizers;
      assert.ok(finalizers.every(result => result.status === 'rejected' && /terminal collector summary publication failed/u.test(String(result.reason))));
      assert.equal(existsSync(join(fixture.root, 'android-environment-collector.json')), false);
      const errors = measurement.failureOutcome().errors.map(error => String(error));
      assert.ok(errors.some(message => message.includes('synthetic failed-begin snapshot')));
      assert.ok(errors.some(message => message.includes('synthetic terminal collector write failure')));
    } finally {
      release();
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS failed-begin cleanup and terminal summary workers retire without late mutations', async () => {
    const cases = [
      { mode: '--android-measurement-persist-text', phase: 'cleanup' },
      { mode: '--android-transport-observation-trigger', phase: 'cleanup' },
      { mode: '--android-measurement-persist-json', phase: 'completion' },
      { mode: '--android-measurement-persist-json', phase: 'collector' },
    ] as const;
    for (const { mode, phase } of cases) {
      const fixture = await harness.createFixture();
      const endpoint = mode === '--android-transport-observation-trigger' ? await createAndroidTransportTestEndpoint() : undefined;
      const transportFixture = endpoint ? await createAndroidTransportFixture(fixture.root, endpoint.port, 'present-host') : undefined;
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const readyPath = join(fixture.root, `failed-begin-${phase}-${mode.slice(2)}-ready`);
      const collectorReadyPath = join(fixture.root, `failed-begin-${phase}-${mode.slice(2)}-collector-ready`);
      const latePath = join(fixture.root, `failed-begin-${phase}-${mode.slice(2)}-late`);
      const observer = createBoundedSupervisorObserver(fixture.root, `failed-begin-${phase}-${mode.slice(2)}-supervisor`);
      let failure: BoundedCommandError | undefined;
      let collectorSpawns = 0;
      const startupDetails = () => JSON.stringify({ mode, phase, collectorReady: existsSync(collectorReadyPath),
        writerReady: existsSync(readyPath), transportEndpointRequests: endpoint?.requests,
        transportObservationFailure: (measurement as unknown as { transportObservationFailure?: string }).transportObservationFailure,
        failure: boundedCommandFailureDetails(failure?.result, observer.records()) });
      const measurement: AndroidEnvironmentMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        transportObservationFixture: transportFixture,
        spawnCollector: () => {
          collectorSpawns++;
          if (mode === '--android-transport-observation-trigger') {
            const code = `require('node:fs').writeFileSync(${JSON.stringify(collectorReadyPath)},'ready');const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n'+epoch+' 77 77 I adbd: timeout expired while flushing socket, closing\\n');setTimeout(()=>process.exit(0),5000);setInterval(()=>{},1000)`;
            return spawnOwnedProcess(process.execPath, ['-e', code], { timeoutMs: 8_000, cleanupReservationMs: 1_000 });
          }
          return spawnOwnedProcess('adb', ['-s', 'emulator-5554', 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1'], { timeoutMs: 8_000, cleanupReservationMs: 1_000 });
        },
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('snapshot')) {
            if (mode === '--android-transport-observation-trigger') {
              const writerDeadline = Date.now() + 5_000;
              while (!existsSync(readyPath) && Date.now() < writerDeadline) await new Promise(resolve => setTimeout(resolve, 5));
              if (!existsSync(readyPath)) assert.fail(startupDetails());
            }
            throw new Error('synthetic failed-begin snapshot');
          }
          let matches = args.includes(mode);
          if (matches && phase !== 'cleanup' && mode === '--android-measurement-persist-json') {
            const request = JSON.parse(String(options.input));
            matches = request.target.endsWith(`android-environment-${phase}.json`);
          }
          if (!matches) return boundedCommand(binary, args, timeout, options);
          const target = mode === '--android-transport-observation-trigger'
            ? join(fixture.root, 'android-transport-observation.json')
            : (() => {
              try { return JSON.parse(String(options.input)).target as string; }
              catch { return join(fixture.root, 'android-transport-observation.json'); }
            })();
          const child = `process.on('SIGTERM',()=>{});const fs=require('node:fs');fs.writeFileSync(${JSON.stringify(readyPath)},'ready');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(target)},'late');fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1300);setInterval(()=>{},1000)`;
          try { return await boundedCommand(process.execPath, ['-e', child], timeout, { ...options, observerSupervisor: observer.stream }); }
          catch (error) { if (error instanceof BoundedCommandError) failure = error; throw error; }
        },
      });
      let pendingBegin: Promise<void> | undefined;
      try {
        measurement.requireGmsObservations();
        await measurement.observeGmsStage('after-device-start', new PhaseBudget(`failed-begin-${phase}`, { timeoutMs: 15_000 }));
        await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget(`failed-begin-${phase}`, { timeoutMs: 15_000 }));
        if (phase === 'collector') await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot/u);
        else pendingBegin = measurement.begin();
        const startDeadline = Date.now() + 3_000;
        while (!existsSync(readyPath) && Date.now() < startDeadline) await new Promise(resolve => setTimeout(resolve, 5));
        if (phase === 'collector') {
          const finalization = measurement.finalizeGmsObservations();
          const writerDeadline = Date.now() + 3_000;
          while (!existsSync(readyPath) && Date.now() < writerDeadline) await new Promise(resolve => setTimeout(resolve, 5));
          if (!existsSync(readyPath)) assert.fail(startupDetails());
          const started = performance.now();
          await assert.rejects(finalization, /terminal collector summary publication failed/u);
          assert.ok(performance.now() - started < 2_500, phase);
        } else {
          if (!existsSync(readyPath)) assert.fail(startupDetails());
          const finalization = measurement.finalizeGmsObservations();
          const started = performance.now();
          assert.ok(pendingBegin, phase);
          await assert.rejects(pendingBegin, /synthetic failed-begin snapshot/u);
          await finalization;
          assert.ok(performance.now() - started < 2_500, phase);
        }
        await observer.close();
        const details = JSON.stringify({ phase, mode, command: boundedCommandFailureDetails(failure?.result, observer.records()) });
        assert.ok(failure instanceof BoundedCommandError, details);
        assert.equal(failure.result.childExited, true, details);
        assert.equal(failure.result.ownedProcessesExited, true, details);
        assert.equal(failure.result.stdioClosed, true, details);
        assert.equal(collectorSpawns, 1, phase);
        await new Promise(resolve => setTimeout(resolve, 1_400));
        assert.equal(existsSync(latePath), false, phase);
        if (phase === 'completion') assert.equal(existsSync(join(fixture.root, 'android-environment-completion.json')), false, phase);
        if (phase === 'collector') assert.equal(existsSync(join(fixture.root, 'android-environment-collector.json')), false);
      } finally {
        await observer.close();
        await pendingBegin?.catch(() => undefined);
        await measurement.finish().catch(() => undefined);
        await endpoint?.stop();
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('GMS begin ownership and filesystem workers are retired within finalization with no late writes', async () => {
    const cases = [
      { mode: '--android-measurement-owner', artifact: 'android-environment-session.json' },
      { mode: '--android-measurement-mkdir', artifact: 'android-environment-session.json' },
      { mode: '--android-measurement-session', artifact: 'android-environment-session.json' },
      { mode: '--android-measurement-operations', artifact: 'android-environment-operations.json' },
    ];
    for (const { mode, artifact } of cases) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const output = join(fixture.root, 'bounded-begin-output');
      const readyPath = join(fixture.root, `${mode.slice(2)}-ready`);
      const latePath = join(fixture.root, `${mode.slice(2)}-late`);
      let failure: BoundedCommandError | undefined;
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', output, repositoryPath('tests/mobile/toolchains.json'), {
        boundedCommand: async (binary, args, timeout, options) => {
          if (!args.includes(mode)) return boundedCommand(binary, args, timeout, options);
          const request = options.input === undefined ? undefined : JSON.parse(String(options.input));
          const lateMutation = mode === '--android-measurement-mkdir'
            ? `fs.mkdirSync(request.path,{recursive:true});fs.writeFileSync(${JSON.stringify(latePath)},'late')`
            : mode === '--android-measurement-session' || mode === '--android-measurement-operations'
              ? `fs.writeFileSync(request.path,'late');fs.writeFileSync(${JSON.stringify(latePath)},'late')`
              : `fs.writeFileSync(${JSON.stringify(latePath)},'late')`;
          const child = `process.on('SIGTERM',()=>{});const fs=require('node:fs');const request=${JSON.stringify(request)};fs.writeFileSync(${JSON.stringify(readyPath)},'ready');setTimeout(()=>{${lateMutation};process.exit(0)},1200);setInterval(()=>{},1000)`;
          try { return await boundedCommand('node', ['-e', child], timeout, options); }
          catch (error) { if (error instanceof BoundedCommandError) failure = error; throw error; }
        },
      });
      let pendingBegin: Promise<void> | undefined;
      try {
        measurement.requireGmsObservations();
        await measurement.observeGmsStage('after-device-start', new PhaseBudget('bounded-begin', { timeoutMs: 15_000 }));
        await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('bounded-begin', { timeoutMs: 15_000 }));
        await rm(output, { recursive: true, force: true });
        pendingBegin = measurement.begin();
        const readyDeadline = Date.now() + 3_000;
        while (!existsSync(readyPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
        assert.ok(existsSync(readyPath), mode);
        const finalization = measurement.finalizeGmsObservations();
        await assert.rejects(pendingBegin, /BOUNDED_COMMAND_FAILED/u);
        await finalization;
        assert.ok(failure instanceof BoundedCommandError, mode);
        assert.equal(failure.result.childExited, true, mode);
        assert.equal(failure.result.ownedProcessesExited, true, mode);
        assert.equal(failure.result.stdioClosed, true, mode);
        await new Promise(resolve => setTimeout(resolve, 1_300));
        assert.equal(existsSync(latePath), false, mode);
        assert.equal(existsSync(join(output, artifact)), false, mode);
        const observations = JSON.parse(await readFile(join(output, 'android-environment-gms-observations.json'), 'utf8'));
        assert.equal(observations.finalized, true, mode);
      } finally {
        await pendingBegin?.catch(() => undefined);
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('diagnostic GMS samples use exact bounded stages without changing either measurement snapshot', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const parserObserverPath = join(fixture.root, 'gms-parser-supervisor.jsonl');
    const parserObserverFD = openSync(parserObserverPath, 'wx', 0o600);
    const parserObserver = createWriteStream('', { fd: parserObserverFD, autoClose: false });
    const parserRuns: Array<Record<string, unknown>> = [];
    let parserObserverClosed = false;
    const closeParserObserver = async () => {
      if (parserObserverClosed) return;
      parserObserverClosed = true;
      await new Promise<void>(resolve => parserObserver.end(resolve));
      closeSync(parserObserverFD);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        const evalIndex = args.indexOf('--eval');
        const parserSource = evalIndex < 0 ? '' : String(args[evalIndex + 1] || '');
        if (!parserSource.includes('PARSER_STARTED')) return boundedCommand(binary, args, timeout, options);
        const input = Buffer.from(options.input || '');
        const startedAtNs = monotonicNowNs();
        const record = (result: BoundedCommandError['result']) => parserRuns.push({
          startedAtNs: startedAtNs.toString(), endedAtNs: monotonicNowNs().toString(), timeoutMs: timeout,
          cleanupReservationMs: options.cleanupReservationMs, inputBytes: input.byteLength,
          inputSha256: hashText(input.toString('utf8')), targetPID: result.targetPID,
          targetCloseObserved: result.targetCloseObserved, childExited: result.childExited,
          ownedProcessesExited: result.ownedProcessesExited, stdioClosed: result.stdioClosed,
          retirement: boundedCommandFailureDetails(result, observerRecords(parserObserverPath)),
          timedOut: result.timedOut, durationMs: result.durationMs, exitCode: result.code, stdoutBytes: result.stdoutBytes,
          stdoutSha256: result.stdoutSha256,
        });
        try {
          const result = await boundedCommand(binary, args, timeout, { ...options, observerSupervisor: parserObserver });
          record(result);
          return result;
        } catch (error) {
          if (error instanceof BoundedCommandError) record(error.result);
          throw error;
        }
      },
    });
    const gmsDumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const gmsDump = await readFile(gmsDumpPath, 'utf8');
    await writeFile(gmsDumpPath, gmsDump.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.EnabledService\n      disabledComponents:\n        com.google.firebase.auth.api.gms.fixture.DisabledReceiver\nQueries:'));
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const budget = () => new PhaseBudget('gms-observation-test', { timeoutMs: 30_000 });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], budget());
      await measurement.observeGmsStage(stages[1], budget());
      await measurement.begin();
      const beforePath = join(fixture.root, 'android-environment-before.json');
      const beforeHash = await fileSha256(beforePath);
      const before = JSON.parse(await readFile(beforePath, 'utf8')) as AndroidEnvironmentSnapshot;
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, budget());
      const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
      const observations = JSON.parse(await readFile(observationsPath, 'utf8'));
      assert.deepEqual(observations.stages.map((entry: any) => entry.stage), stages);
      assert.ok(observations.stages.every((entry: any) => entry.outcome === 'collected' && Date.parse(entry.startedAt) <= Date.parse(entry.endedAt)),
        JSON.stringify(observations.stages.map((entry: any) => ({ stage: entry.stage, outcome: entry.outcome, startedAt: entry.startedAt,
          endedAt: entry.endedAt, durationMs: entry.durationMs, error: entry.error, timeoutMs: entry.timeoutMs,
          ownershipDurationMs: entry.ownershipDurationMs, ownershipChildExited: entry.ownershipChildExited,
          ownershipProcessesExited: entry.ownershipProcessesExited, ownershipStdioClosed: entry.ownershipStdioClosed,
          parseDurationMs: entry.parseDurationMs, parseTimeoutMs: entry.parseTimeoutMs,
          parseCleanupReservationMs: entry.parseCleanupReservationMs, parserStarted: entry.parserStarted,
          parserReady: entry.parserReady, parserInputBytes: entry.parserInputBytes,
          parserInputSha256: entry.parserInputSha256, parserOutputProduced: entry.parserOutputProduced,
          parserStartupMs: entry.parserStartupMs, parseTargetStarted: entry.parseTargetStarted,
          parseTargetClosed: entry.parseTargetClosed, normalizationTimedOut: entry.normalizationTimedOut,
          parseChildExited: entry.parseChildExited, parseOwnedProcessesExited: entry.parseOwnedProcessesExited,
          parseStdioClosed: entry.parseStdioClosed, childExited: entry.childExited,
          ownedProcessesExited: entry.ownedProcessesExited, stdioClosed: entry.stdioClosed,
          exitCode: entry.exitCode, timedOut: entry.timedOut, signal: entry.signal }))));
      const state = observations.stages[0].state;
      assert.deepEqual(state.enabledComponents, ['com.google.android.gms.fixture.EnabledService']);
      assert.deepEqual(state.disabledComponents, ['com.google.firebase.auth.api.gms.fixture.DisabledReceiver']);
      assert.equal(state.packageName, 'com.google.android.gms');
      assert.equal(state.hashEncoding, 'sha256-hex-dot-separated-v1');
      assert.equal(decodeRetainedHash(state.componentStateSha256), hashText(JSON.stringify({
        enabledComponents: state.enabledComponents,
        disabledComponents: state.disabledComponents,
      })));
      assert.equal(decodeRetainedHash(state.componentStateSha256), decodeRetainedHash(state.sourceHashes.componentStateSha256));
      assert.equal(decodeRetainedHash(state.packageIdentitySha256), decodeRetainedHash(state.sourceHashes.packageIdentitySha256));
      assert.equal(decodeRetainedHash(state.dumpSha256), decodeRetainedHash(state.sourceHashes.dumpSha256));
      assert.equal(await fileSha256(beforePath), beforeHash);
      const outcome = await measurement.finish();
      assert.deepEqual(outcome.errors, []);
      await closeParserObserver();
      const parserEvents = observerRecords(parserObserverPath);
      assert.equal(parserRuns.length, stages.length);
      assert.ok(parserRuns.every(run => run.timeoutMs === 250 && run.cleanupReservationMs === 40
        && Number(run.targetPID) > 0 && run.targetCloseObserved === true && run.childExited === true
        && run.ownedProcessesExited === true && run.stdioClosed === true && run.timedOut === false
        && Number(run.inputBytes) > 0 && /^[a-f0-9]{64}$/u.test(String(run.inputSha256))
        && BigInt(String(run.startedAtNs)) < BigInt(String(run.endedAtNs))), JSON.stringify(parserRuns));
      for (const stage of observations.stages) {
        assert.equal(stage.parseTimeoutMs, 250);
        assert.equal(stage.parseCleanupReservationMs, 40);
        assert.equal(stage.parserStarted, true);
        assert.equal(stage.parserReady, true);
        assert.equal(stage.parserInputBytes, stage.stdoutBytes);
        assert.equal(stage.parserInputSha256, decodeRetainedHash(stage.stdoutSha256));
        assert.equal(stage.parserOutputProduced, true);
        assert.equal(stage.parseTargetStarted, true);
        assert.equal(stage.parseTargetClosed, true);
        assert.ok(Number.isSafeInteger(stage.parserStartupMs) && stage.parserStartupMs >= 0);
      }
      for (const type of ['supervisor-runtime-started', 'supervisor-deadlines-armed', 'supervisor-target-exit',
        'supervisor-target-close', 'supervisor-retirement-ready']) {
        assert.equal(parserEvents.filter((event: any) => event.type === type).length, stages.length, type);
      }
      const after = JSON.parse(await readFile(join(fixture.root, 'android-environment-after.json'), 'utf8')) as AndroidEnvironmentSnapshot;
      assert.deepEqual(after.packages['com.google.android.gms'], before.packages['com.google.android.gms']);
      assert.equal(await fileSha256(beforePath), beforeHash);
      const persistedObservations = JSON.parse(await readFile(observationsPath, 'utf8'));
      const persistedState = persistedObservations.stages[0].state;
      assert.equal(decodeRetainedHash(persistedState.componentStateSha256), hashText(JSON.stringify({
        enabledComponents: persistedState.enabledComponents,
        disabledComponents: persistedState.disabledComponents,
      })));
      assert.equal(decodeRetainedHash(persistedState.sourceHashes.componentStateSha256), decodeRetainedHash(persistedState.componentStateSha256));
      const commands = (await readFile(fixture.log, 'utf8')).split('\n').filter((line) => line.includes('shell dumpsys package com.google.android.gms'));
      assert.equal(commands.length, stages.length + 2, 'only the existing before/after snapshots add their two GMS package reads');
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.deepEqual(collector.gmsObservations.stages.map((entry: any) => entry.stage), stages);
      assert.equal(collector.failure, undefined);

      const delayedFixture = await harness.createFixture();
      const delayedSaved = { ...process.env };
      Object.assign(process.env, delayedFixture.environment);
      const delayedObserverPath = join(delayedFixture.root, 'gms-parser-supervisor.jsonl');
      const delayedObserverFD = openSync(delayedObserverPath, 'wx', 0o600);
      const delayedObserver = createWriteStream('', { fd: delayedObserverFD, autoClose: false });
      let delayedObserverClosed = false;
      const closeDelayedObserver = async () => {
        if (delayedObserverClosed) return;
        delayedObserverClosed = true;
        await new Promise<void>(resolve => delayedObserver.end(resolve));
        closeSync(delayedObserverFD);
      };
      let delayedParserFailure: BoundedCommandError | undefined;
      let delayedParserReceipt: Record<string, unknown> | undefined;
      const delayedMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', delayedFixture.root,
        repositoryPath('tests/mobile/toolchains.json'), {
          boundedCommand: async (binary, args, timeout, options) => {
            const evalIndex = args.indexOf('--eval');
            const parserSource = evalIndex < 0 ? '' : String(args[evalIndex + 1] || '');
            if (!parserSource.includes('PARSER_STARTED')) return boundedCommand(binary, args, timeout, options);
            const delayedSource = parserSource.replace('const parser = await import(',
              'await new Promise(resolve => setTimeout(resolve, 400));\nconst parser = await import(');
            assert.notEqual(delayedSource, parserSource);
            const input = Buffer.from(options.input || '');
            const startedAtNs = monotonicNowNs();
            try {
              return await boundedCommand(binary, args.map((value, index) => index === evalIndex + 1 ? delayedSource : value), timeout,
                { ...options, observerSupervisor: delayedObserver });
            } catch (error) {
              if (error instanceof BoundedCommandError) {
                delayedParserFailure = error;
                delayedParserReceipt = {
                  startedAtNs: startedAtNs.toString(), endedAtNs: monotonicNowNs().toString(), timeoutMs: timeout,
                  cleanupReservationMs: options.cleanupReservationMs, inputBytes: input.byteLength,
                  inputSha256: hashText(input.toString('utf8')), targetPID: error.result.targetPID,
                  targetCloseObserved: error.result.targetCloseObserved, childExited: error.result.childExited,
                  ownedProcessesExited: error.result.ownedProcessesExited, stdioClosed: error.result.stdioClosed,
                  timedOut: error.result.timedOut, durationMs: error.result.durationMs,
                };
              }
              throw error;
            }
          },
        });
      try {
        delayedMeasurement.requireGmsObservations();
        await delayedMeasurement.observeGmsStage('after-device-start',
          new PhaseBudget('gms-parser-delayed-control', { timeoutMs: 15_000 }));
        await delayedMeasurement.finalizeGmsObservations();
        await closeDelayedObserver();
        const delayedEvents = observerRecords(delayedObserverPath);
        const delayedObservations = JSON.parse(await readFile(join(delayedFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        const delayedStage = delayedObservations.stages[0];
        const delayedDetails = JSON.stringify({ stage: {
          parserStarted: delayedStage.parserStarted, parserReady: delayedStage.parserReady,
          parseTargetStarted: delayedStage.parseTargetStarted, parseTargetClosed: delayedStage.parseTargetClosed,
          parseChildExited: delayedStage.parseChildExited, parseOwnedProcessesExited: delayedStage.parseOwnedProcessesExited,
          parseStdioClosed: delayedStage.parseStdioClosed,
        }, receipt: delayedParserReceipt, command: boundedCommandFailureDetails(delayedParserFailure?.result, delayedEvents) });
        assert.equal(delayedStage.outcome, 'failed');
        assert.equal(delayedStage.normalizationTimedOut, true);
        assert.equal(delayedStage.parseTimeoutMs, 250);
        assert.equal(delayedStage.parseCleanupReservationMs, 40);
        assert.equal(delayedStage.parserStarted, true);
        assert.equal(delayedStage.parserReady, false);
        assert.equal(delayedStage.parserInputBytes, undefined);
        assert.equal(delayedStage.parserOutputProduced, false);
        assert.equal(delayedStage.parseTargetStarted, true);
        assert.equal(delayedStage.parseTargetClosed, true);
        assert.equal(delayedStage.parseChildExited, true);
        assert.equal(delayedStage.parseOwnedProcessesExited, true);
        assert.equal(delayedStage.parseStdioClosed, true);
        assert.ok(delayedStage.parseDurationMs <= 250);
        assert.equal(delayedStage.state, undefined);
        assert.ok(delayedParserFailure instanceof BoundedCommandError);
        assert.equal(delayedParserFailure.result.timedOut, true);
        assert.match(delayedParserFailure.result.stdout, /^PARSER_STARTED \d+\n/u);
        assert.doesNotMatch(delayedParserFailure.result.stdout, /PARSER_READY|PARSER_INPUT|PARSER_OUTPUT/u);
        assert.ok(delayedParserReceipt);
        assert.equal(delayedParserReceipt.timeoutMs, 250);
        assert.equal(delayedParserReceipt.cleanupReservationMs, 40);
        assert.equal(delayedParserReceipt.inputBytes, delayedStage.stdoutBytes);
        assert.equal(delayedParserReceipt.inputSha256, decodeRetainedHash(delayedStage.stdoutSha256));
        assert.ok(Number(delayedParserReceipt.targetPID) > 0);
        assert.equal(delayedParserReceipt.targetCloseObserved, true);
        assert.equal(delayedParserReceipt.childExited, true);
        assert.equal(delayedParserReceipt.ownedProcessesExited, true, delayedDetails);
        assert.equal(delayedParserReceipt.stdioClosed, true);
        assert.equal(delayedParserReceipt.timedOut, true);
        assert.ok(BigInt(String(delayedParserReceipt.startedAtNs)) < BigInt(String(delayedParserReceipt.endedAtNs)));
        const deadlines = delayedEvents.find((event: any) => event.type === 'supervisor-deadlines-armed');
        const executionDeadline = delayedEvents.find((event: any) => event.type === 'supervisor-execution-deadline');
        assert.ok(deadlines && executionDeadline);
        assert.equal(BigInt(String(deadlines.hardDeadlineNs)) - BigInt(String(deadlines.executionDeadlineNs)), 40_000_000n);
        const deadlineObservedAt = BigInt(String(executionDeadline.clockNs));
        const executionDeadlineNs = BigInt(String(deadlines.executionDeadlineNs));
        assert.ok(deadlineObservedAt >= executionDeadlineNs - 5_000_000n
          && deadlineObservedAt <= BigInt(String(deadlines.hardDeadlineNs)));
        for (const type of ['supervisor-runtime-started', 'supervisor-target-exit', 'supervisor-target-close',
          'supervisor-input-close', 'supervisor-group-absent', 'supervisor-retirement-ready']) {
          const count = delayedEvents.filter((event: any) => event.type === type).length;
          assert.ok(type === 'supervisor-group-absent' ? count >= 1 : count === 1, type);
        }
        await writeFile(join(fixture.root, 'gms-parser-process-controls.json'), JSON.stringify({
          positive: parserRuns,
          positiveStages: observations.stages.map((stage: any) => ({ stage: stage.stage, timeoutMs: stage.parseTimeoutMs,
            cleanupReservationMs: stage.parseCleanupReservationMs, parserStarted: stage.parserStarted, parserReady: stage.parserReady,
            inputBytes: stage.parserInputBytes, inputSha256: stage.parserInputSha256, outputProduced: stage.parserOutputProduced,
            startupMs: stage.parserStartupMs, parseDurationMs: stage.parseDurationMs, targetStarted: stage.parseTargetStarted,
            targetClosed: stage.parseTargetClosed, childExited: stage.parseChildExited,
            ownedProcessesExited: stage.parseOwnedProcessesExited, stdioClosed: stage.parseStdioClosed })),
          delayed: delayedParserReceipt,
          delayedStage: { timeoutMs: delayedStage.parseTimeoutMs, cleanupReservationMs: delayedStage.parseCleanupReservationMs,
            parserStarted: delayedStage.parserStarted, parserReady: delayedStage.parserReady, inputBytes: delayedStage.parserInputBytes,
            outputProduced: delayedStage.parserOutputProduced, parseDurationMs: delayedStage.parseDurationMs,
            targetStarted: delayedStage.parseTargetStarted, targetClosed: delayedStage.parseTargetClosed,
            childExited: delayedStage.parseChildExited, ownedProcessesExited: delayedStage.parseOwnedProcessesExited,
            stdioClosed: delayedStage.parseStdioClosed },
          deadlineEvents: delayedEvents.filter((event: any) => ['supervisor-deadlines-armed',
            'supervisor-execution-deadline', 'supervisor-retirement-ready'].includes(event.type)),
        }, null, 2));
      } finally {
        await delayedMeasurement.finish().catch(() => undefined);
        await closeDelayedObserver();
        for (const key of Object.keys(process.env)) if (!(key in delayedSaved)) delete process.env[key];
        Object.assign(process.env, delayedSaved);
      }
    } catch (error) {
      const readJsonIfPresent = async (name: string) => {
        const path = join(fixture.root, name);
        return existsSync(path) ? JSON.parse(await readFile(path, 'utf8')) : undefined;
      };
      await writeFile(join(fixture.root, 'diagnostic-gms-stage-failure.json'), JSON.stringify({
        error: error instanceof Error ? { name: error.name, message: error.message, stack: error.stack } : String(error),
        measurementErrors: measurement.failureOutcome().errors.map(value => value instanceof Error ? value.message : String(value)),
        observations: await readJsonIfPresent('android-environment-gms-observations.json'),
        collector: await readJsonIfPresent('android-environment-collector.json'),
        completion: await readJsonIfPresent('android-environment-completion.json'),
        check: await readJsonIfPresent('android-environment-check.json'),
      }, null, 2)).catch(() => undefined);
      throw error;
    } finally {
      await measurement.finish().catch(() => undefined);
      await closeParserObserver();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('timed-out GMS persistence retires its file worker before finalization returns', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
    const latePath = join(fixture.root, 'late-gms-persistence-write');
    const observer = createBoundedSupervisorObserver(fixture.root, 'gms-persistence-supervisor');
    let entered!: () => void;
    const writerEntered = new Promise<void>(resolve => { entered = resolve; });
    let writeFailure: BoundedCommandError | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--gms-diagnostic-persist') || !String(options.input || '').includes('"finalized":true')) {
          return boundedCommand(binary, args, timeout, options);
        }
        entered();
        const lateCode = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1500);setInterval(()=>{},1000)`;
        try {
          return await boundedCommand('node', ['-e', lateCode], timeout, { ...options, input: undefined, observerSupervisor: observer.stream });
        } catch (error) {
          if (error instanceof BoundedCommandError) writeFailure = error;
          throw error;
        }
      },
    });
    try {
      measurement.requireGmsObservations();
      for (const stage of stages.slice(0, 2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-persist-timeout', { timeoutMs: 30_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-persist-timeout', { timeoutMs: 30_000 }));
      const priorBytes = await readFile(observationsPath);
      const finalization = measurement.finalizeGmsObservations();
      await writerEntered;
      const finish = measurement.finish();
      await assert.rejects(finalization, /terminal collector confirmation failed/u);
      const outcome = await finish;
      const resultBytes = await readFile(observationsPath);
      const result = JSON.parse(resultBytes.toString('utf8'));
      assert.equal(result.stages.length, stages.length);
      assert.equal(result.finalized, undefined);
      assert.deepEqual(resultBytes, priorBytes);
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(writeFailure?.result, observer.records()) });
      assert.ok(writeFailure instanceof BoundedCommandError, details);
      assert.equal(writeFailure.result.timedOut, true, details);
      assert.equal(writeFailure.result.childExited, true, details);
      assert.equal(writeFailure.result.ownedProcessesExited, true, details);
      assert.equal(writeFailure.result.stdioClosed, true, details);
      const writerErrors = outcome.errors.map(error => String(error)).filter(message => message.includes('GMS writer boundary'));
      assert.equal(writerErrors.length, 1, JSON.stringify(outcome.errors.map(error => String(error))));
      const writerTimeoutMs = Number(writerErrors[0]!.match(/ writerTimeoutMs=(\d+) dispatchedAfterMs=\d+ /u)?.[1]);
      assert.ok(writerTimeoutMs > 0 && writerTimeoutMs <= 775, writerErrors[0]);
      assert.ok(writerErrors[0]!.includes(` durationMs=${writeFailure.result.durationMs} targetStarted=true timedOut=true aborted=false`
        + ' childExited=true targetClosed=true ownedProcessesExited=true stdioClosed=true '), writerErrors[0]);
      assert.match(writerErrors[0]!, / exitCode=\S+ signal=\S+ progress=none: BOUNDED_COMMAND_FAILED: node -e /u);
      await new Promise(resolve => setTimeout(resolve, 600));
      assert.equal(existsSync(latePath), false);
      assert.deepEqual(await readFile(observationsPath), resultBytes);
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.ok(collector.gmsObservations.errors > 0);
      assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
      assert.equal(collector.gmsObservations.temporaryEvidenceCleanup, 'removed-after-failure');
    } finally {
      await observer.close();
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('a staged terminal rename followed by a stalled writer cannot publish unconfirmed GMS success', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
    const observer = createBoundedSupervisorObserver(fixture.root, 'gms-publication-supervisor');
    const writers = gmsWriterDiagnostics(fixture.root);
    let writerFailure: BoundedCommandError | undefined;
    const script = repositoryPath('tests/mobile/android-measurement.ts');
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--gms-diagnostic-persist')) return boundedCommand(binary, args, timeout, options);
        if (!String(options.input || '').includes('"finalized":true')) return writers.run(binary, args, timeout, options);
        try {
          return await boundedCommand(process.execPath, [script, '--gms-diagnostic-persist-stall-after-publication'], timeout,
            { ...options, observerSupervisor: observer.stream });
        } catch (error) { if (error instanceof BoundedCommandError) writerFailure = error; throw error; }
      },
    });
    let terminalSettled: Promise<unknown> | undefined;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('unconfirmed-publication', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('unconfirmed-publication', { timeoutMs: 20_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('unconfirmed-publication', { timeoutMs: 20_000 }));
      const prior = await readFile(observationsPath);
      const staged = JSON.parse(prior.toString('utf8'));
      const stagedDetails = () => JSON.stringify({
        stages: staged.stages?.map(({ stage, outcome, error }: any) => ({ stage, outcome, error })), errors: staged.errors,
        measurementErrors: measurement.failureOutcome().errors.map(error => String(error)), writerFailures: writers.failures,
      });
      assert.deepEqual(staged.stages?.map(({ stage, outcome }: any) => [stage, outcome]), stages.map(stage => [stage, 'collected']), stagedDetails());
      assert.deepEqual(staged.errors, [], stagedDetails());
      const terminal = measurement.finalizeGmsObservations();
      terminalSettled = terminal.then(() => undefined, (error: unknown) => error);
      const publicationDeadline = Date.now() + 3_000;
      let publicObservation: any;
      while (Date.now() < publicationDeadline) {
        try { publicObservation = JSON.parse(await readFile(observationsPath, 'utf8')); } catch { publicObservation = undefined; }
        if (publicObservation?.finalized === true) break;
        await new Promise(resolve => setTimeout(resolve, 5));
      }
      const publicationDetails = () => JSON.stringify({
        publicObservation: publicObservation && { finalized: publicObservation.finalized,
          terminalPublication: publicObservation.terminalPublication, stages: publicObservation.stages?.length, errors: publicObservation.errors },
        measurementErrors: measurement.failureOutcome().errors.map(error => String(error)),
        command: boundedCommandFailureDetails(writerFailure?.result, observer.records()),
      });
      assert.equal(publicObservation?.finalized, true, publicationDetails());
      assert.equal(publicObservation.terminalPublication, 'awaiting-collector-confirmation', publicationDetails());
      assert.deepEqual(publicObservation.errors, [], publicationDetails());
      const finish = measurement.finish();
      await assert.rejects(terminal, /terminal collector confirmation failed/u);
      const outcome = await finish;
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(writerFailure?.result, observer.records()) });
      assert.ok(writerFailure instanceof BoundedCommandError, details);
      assert.equal(writerFailure.result.timedOut, true, details);
      assert.equal(writerFailure.result.childExited, true, details);
      assert.equal(writerFailure.result.ownedProcessesExited, true, details);
      assert.equal(writerFailure.result.stdioClosed, true, details);
      const writerBoundary = outcome.errors.map(error => String(error)).find(message => message.includes('GMS writer boundary'));
      assert.match(writerBoundary ?? '', / progress=ready@\d+,input@\d+:\d+,directory@\d+,staged@\d+: BOUNDED_COMMAND_FAILED: /u,
        JSON.stringify(outcome.errors.map(error => String(error))));
      assert.notDeepEqual(await readFile(observationsPath), prior);
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
      assert.equal(collector.gmsObservations.temporaryEvidenceCleanup, 'removed-after-failure');
      assert.equal(collector.gmsObservations.terminalConfirmation, 'unconfirmed');
      assert.equal(collector.gmsObservations.observationSha256, undefined);
      assert.ok(collector.gmsObservations.errors > 0);
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      assert.ok(outcome.errors.length > 0);
    } finally {
      await observer.close();
      await measurement.finish().catch(() => undefined);
      await terminalSettled;
      await writers.close();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS collector confirmation publication failure invalidates public success for the assessment consumer', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const script = repositoryPath('tests/mobile/android-measurement.ts');
    const observer = createBoundedSupervisorObserver(fixture.root, 'gms-collector-confirmation-supervisor');
    let writerFailure: BoundedCommandError | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--android-measurement-persist-json')) {
          const request = JSON.parse(String(options.input));
          if (request.target.endsWith('android-environment-collector.json')) {
            try {
              return await boundedCommand(process.execPath, [script, '--android-measurement-persist-json-stall-after-publication'], timeout,
                { ...options, observerSupervisor: observer.stream });
            } catch (error) {
              if (error instanceof BoundedCommandError) writerFailure = error;
              throw error;
            }
          }
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-collector-confirmation', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-collector-confirmation', { timeoutMs: 20_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-collector-confirmation', { timeoutMs: 20_000 }));
      const identity = measurementIdentity({ measurementId: measurement.id });
      const outcome = await measurement.finish();
      const collectorPath = join(fixture.root, 'android-environment-collector.json');
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(writerFailure?.result, observer.records()) });
      assert.ok(writerFailure instanceof BoundedCommandError, details);
      assert.equal(writerFailure.result.timedOut, true, details);
      assert.equal(writerFailure.result.childExited, true, details);
      assert.equal(writerFailure.result.ownedProcessesExited, true, details);
      assert.equal(writerFailure.result.stdioClosed, true, details);
      assert.equal(existsSync(collectorPath), false);
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      assert.ok(outcome.errors.length > 0);
      const bundlePath = join(fixture.root, 'bundle-set.json');
      await writeFile(bundlePath, JSON.stringify({ candidate: {
        provenance: { sourceCommit: identity.sourceCommit },
        identity: { webHash: identity.candidateWebHash, build: identity.candidateBuild },
      } }));
      const args = ['check', '--contract', 'report', '--directory', fixture.root,
        '--identity', join(fixture.root, 'android-environment-identity.json'), '--bundle-set', bundlePath,
        '--run-id', identity.runId, '--attempt', identity.attempt, '--suite', identity.suite, '--baseline', identity.baseline,
        '--scenario', identity.scenario, '--source-run-head-sha', identity.sourceRunHeadSha];
      assert.throws(() => execFileSync(process.execPath, [repositoryPath('tests/mobile/android-environment.ts'), ...args], {
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
      }));
      const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
      assert.equal(check.collection.status, 'UNKNOWN');
    } finally {
      await observer.close();
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport diagnostics capture host and ADB state at the live flush trigger', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const triggerPath = join(fixture.root, 'emit-transport-trigger');
    const endPath = join(fixture.root, 'emit-measurement-end');
    const observationPath = join(fixture.root, 'android-transport-observation.json');
    let activeMeasurement = false;
    let finishStarted = false;
    let resolveWorkerStarted!: () => void;
    const workerStarted = new Promise<void>(resolve => { resolveWorkerStarted = resolve; });
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let capturedRequest: any;
    let workerDispatches = 0;
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportObservationFixture: transportFixture,
      spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        assert.equal(activeMeasurement, true);
        assert.equal(finishStarted, false);
        workerDispatches++;
        capturedRequest = JSON.parse(String(options.input));
        resolveWorkerStarted();
        try { return await boundedCommand(binary, args, timeout, options); }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I adbd: timeout expired while flushing socket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await measurement.begin();
      activeMeasurement = true;
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await writeFile(triggerPath, 'trigger');
      await workerStarted;
      assert.equal(capturedRequest.serial, 'emulator-5554');
      assert.ok(Number.isFinite(Number(capturedRequest.guestEpochSeconds)));
      assert.ok(Date.parse(capturedRequest.triggeredAt) <= Date.now());
      await waitForValue(() => {
        const acquired = existsSync(transportFixture.acquisitionLogFile)
          ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').filter(Boolean) : [];
        return acquired.length === 3 && endpoint.completed.length === 3 ? true : undefined;
      }, 10_000, 'real transport acquisition did not complete all five host and ADB observations');
      await workerFinished;
      assert.equal(workerDispatches, 1);
      assert.deepEqual(endpoint.completed.sort(), ['adb-state', 'guest-clock', 'guest-uptime']);
      await writeFile(transportFixture.stateFile, 'shutdown-time-state');
      finishStarted = true;
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      await finish;
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.transportObservationFailure, undefined);
      const observation = JSON.parse(await readFile(observationPath, 'utf8'));
      assert.equal(observation.trigger, 'guest-adbd-flush-timeout');
      assert.equal(observation.acquisitionStatus, 'collected');
      assert.equal(observation.triggerValidation.absoluteEmissionProven, false);
      assert.deepEqual(observation.observations.map((entry: any) => entry.name), [
        'host-processes', 'host-memory', 'host-tcp', 'adb-state', 'guest-uptime',
      ]);
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-processes').stdout, 'trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-memory').stdout, 'MemAvailable: trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-tcp').stdout, 'State: trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'adb-state').stdout, 'device');
      assert.match(observation.observations.find((entry: any) => entry.name === 'guest-uptime').stdout, /^1234\.56/u);
      assert.deepEqual(endpoint.requests, ['host:transport:emulator-5554', 'shell:date +%s.%N',
        'host-serial:emulator-5554:get-state', 'host:transport:emulator-5554',
        'shell:echo transport-observation; cat /proc/uptime']);
      assert.deepEqual(existsSync(transportFixture.acquisitionLogFile)
        ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').sort() : [],
      ['host-memory', 'host-processes', 'host-tcp']);
      assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
      assert.equal(workerDispatches, 1);
    } finally {
      finishStarted = true;
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS ADB live acquisition rejects empty and incomplete state or uptime replies', async () => {
    const cases = [
      { name: 'empty-state', service: 'get-state' as const, stateFrame: Buffer.from('OKAY0000') },
      { name: 'partial-state', service: 'get-state' as const, stateFrame: Buffer.from('OKAY0006dev') },
      { name: 'empty-uptime', service: 'uptime' as const, uptimeFrame: Buffer.from('OKAY') },
      { name: 'partial-uptime', service: 'uptime' as const, uptimeFrame: Buffer.from('OKAY1234.56 ') },
    ];
    for (const current of cases) {
      const endpoint = await createAndroidTransportTestEndpoint(false, current);
      try {
        const result = await transportAdbObservation('emulator-5554', current.service, endpoint.port, 1_500);
        assert.equal(result.unavailable, true, current.name);
        assert.equal(result.exitCode, null, current.name);
        assert.equal(result.timedOut, false, current.name);
        assert.equal(result.truncated, false, current.name);
        assert.deepEqual(endpoint.requests, current.service === 'get-state'
          ? ['host-serial:emulator-5554:get-state']
          : ['host:transport:emulator-5554', 'shell:echo transport-observation; cat /proc/uptime']);
        const fixture = await harness.createFixture();
        const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'present-host');
        const runHost = async (): Promise<AndroidTransportCommandResult> => {
          const timestamp = new Date().toISOString();
          return { startedAt: timestamp, endedAt: timestamp, stdout: 'present-host\n', stderr: '',
            exitCode: 0, signal: null, timedOut: false, truncated: false, unavailable: false };
        };
        const observation = new AndroidTransportObservation('emulator-5554', fixture.root, runHost, transportAdbObservation, transportFixture);
        await observation.observeTrigger({ guestEpochSeconds: '123.456', triggeredAt: new Date(Date.now() - 1_000).toISOString() });
        assert.equal(observation.failure, 'transport acquisition was unavailable or failed');
        const record = JSON.parse(await readFile(join(fixture.root, 'android-transport-observation.json'), 'utf8'));
        assert.equal(record.acquisitionStatus, 'failed', current.name);
        assert.equal(record.observations.find((entry: { name: string }) => entry.name === (current.service === 'get-state' ? 'adb-state' : 'guest-uptime')).unavailable, true);
      } finally {
        await endpoint.stop();
      }
      assert.equal(endpoint.closed, endpoint.connected, `${current.name} leaves no ADB transport connection alive`);
    }
  });
  test('GMS transport permits one first post-MAIN SYSTEM divider while fencing replay and cursor restarts', async () => {
    const id = 'ca597b37-30b3-4d90-9127-a0c30047c6a0';
    const marker = (epoch: string, name: string) => `${epoch} 77 77 I HerdrMeasure: ${id} ${name}\n`;
    const flush = (epoch: string) => `${epoch} 77 77 I adbd: timeout expired while flushing socket, closing\n`;
    const replay = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(replay.observe(Buffer.from('--------- beginning of system\n'), 99), []);
    assert.deepEqual(replay.observe(Buffer.from(flush('1790000000.900')), 100), []);
    assert.deepEqual(replay.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    const replayFenced = replay.observe(Buffer.from(flush('1790000000.200')), 102);
    assert.equal(replayFenced.length, 1);
    assert.equal(replayFenced[0].guestEpochSeconds, '1790000000.200');
    assert.equal(replayFenced[0].anchorGuestEpochSeconds, '1790000000.100');
    assert.equal(replayFenced[0].anchorMonotonicMs, 101);

    const firstSystem = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(firstSystem.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    assert.deepEqual(firstSystem.observe(Buffer.from('--------- beginning of system\n'), 102), []);
    const candidates = firstSystem.observe(Buffer.from(flush('1790000000.200')), 103);
    assert.equal(candidates.length, 1);
    assert.equal(candidates[0].anchorGuestEpochSeconds, '1790000000.100');
    assert.equal(candidates[0].anchorMonotonicMs, 101);
    assert.equal(firstSystem.accept(), true);
    assert.deepEqual(firstSystem.observe(Buffer.from(flush('1790000000.300')), 104), []);
    assert.deepEqual(firstSystem.observe(Buffer.from('--------- beginning of system\n'), 105), []);
    assert.equal(firstSystem.failure, 'collector log buffer cursor restarted after admission');

    const repeatedSystem = new AndroidTransportTriggerDetector(id);
    repeatedSystem.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    repeatedSystem.observe(Buffer.from('--------- beginning of system\n'), 102);
    assert.deepEqual(repeatedSystem.observe(Buffer.from('--------- beginning of system\n'), 103), []);
    assert.equal(repeatedSystem.failure, 'collector log buffer cursor restarted after admission');

    const restartedMain = new AndroidTransportTriggerDetector(id);
    restartedMain.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    assert.deepEqual(restartedMain.observe(Buffer.from('--------- beginning of main\n'), 102), []);
    assert.equal(restartedMain.failure, 'collector log buffer cursor restarted after admission');

    const before = new AndroidTransportTriggerDetector(id);
    before.observe(Buffer.from('--------- beginning of system\n'), 99);
    assert.deepEqual(before.observe(Buffer.from('--------- beginning of system\n'), 100), []);
    assert.equal(before.failure, 'collector log buffer cursor restarted after admission');
    const duplicated = new AndroidTransportTriggerDetector(id);
    duplicated.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    assert.deepEqual(duplicated.observe(Buffer.from(marker('1790000000.100', 'START')), 102), []);
    assert.equal(duplicated.failure, 'collector measurement start marker was malformed or duplicated');
    const malformed = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(malformed.observe(Buffer.from(`1790000000.100 77 77 I HerdrMeasure: ${id} INVALID START\n`), 101), []);
    assert.equal(malformed.failure, 'collector measurement start marker was malformed or duplicated');
    for (const suffix of ['START ', 'START extra', 'SYSTEM_START']) {
      const trailing = new AndroidTransportTriggerDetector(id);
      trailing.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
      assert.deepEqual(trailing.observe(Buffer.from(marker('1790000000.200', suffix)), 102), []);
      assert.equal(trailing.failure, 'collector measurement start marker was malformed or duplicated');
    }
    const unrelated = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(unrelated.observe(Buffer.from(flush('1790000000.900').replace('77 77 I adbd:', 'malformed adbd:')), 100), []);
    assert.equal(unrelated.failure, undefined);
    assert.deepEqual(unrelated.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    assert.deepEqual(unrelated.observe(Buffer.from(marker('1790000000.150', 'OP_BEGIN operation app 77')), 102), []);
  });
  test('Android15 fake ADB rejects unsupported system-buffer marker writing', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    try {
      await assert.rejects(
        boundedCommand('adb', ['-s', 'emulator-5554', 'shell', 'log', '-b', 'system', '-p', 'i', '-t', 'HerdrMeasure', "'unsupported SYSTEM_START'"],
          3_000, { maxBytes: 1_024 }),
        (error: unknown) => error instanceof BoundedCommandError && error.result.code !== 0
          && error.result.childExited && error.result.ownedProcessesExited && error.result.stdioClosed,
      );
      assert.match(await readFile(fixture.log, 'utf8'), /shell log -b system -p i -t HerdrMeasure/u);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS future-dated pre-MAIN SYSTEM replay never dispatches a transport acquisition', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const endPath = join(fixture.root, 'emit-measurement-end');
    let dispatches = 0;
    let collectorCode = '';
    let replayEpoch = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportObservationFixture: transportFixture,
      spawnCollector: () => {
        replayEpoch = ((Date.now() + 500) / 1000).toFixed(3);
        collectorCode = `const fs=require('node:fs');setTimeout(()=>{process.stdout.write('--------- beginning of system\\n'+${JSON.stringify(replayEpoch)}+' 77 77 I adbd: timeout expired while flushing socket, closing\\n'+(Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
        return spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 });
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--android-transport-observation-trigger')) dispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      await measurement.begin();
      assert.ok(Math.abs(Number(replayEpoch) * 1_000 - Date.now()) < 2_000);
      const sample = await transportAdbObservation('emulator-5554', 'clock', endpoint.port, 1_500);
      assert.equal(sample.unavailable, false);
      assert.ok(Math.abs(Number(sample.stdout.trim()) - Number(replayEpoch)) < 2);
      for (const stage of ['after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const) {
        await measurement.observeGmsStage(stage, new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      }
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      const outcome = await finish;
      assert.equal(dispatches, 0);
      assert.deepEqual(endpoint.completed, ['guest-clock']);
      assert.equal(existsSync(transportFixture.acquisitionLogFile), false);
      assert.equal(outcome.assessment?.collection.status, 'PASS');
    } finally {
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport trigger framing starts one live acquisition', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stalePath = join(fixture.root, 'emit-stale-transport-trigger');
    const releasePath = join(fixture.root, 'release-split-transport-trigger');
    const completePath = join(fixture.root, 'complete-split-transport-trigger');
    const endPath = join(fixture.root, 'emit-measurement-end');
    const staleLine = '123.456 77 77 I adbd: timeout expired while flushing socket, closing';
    const freshPrefix = ' 77 77 I adbd: timeout expired while flushing soc';
    const observedChunks: string[] = [];
    const observedStderr: string[] = [];
    let active = false;
    let finishStarted = false;
    let workerDispatches = 0;
    let capturedRequest: Record<string, any> | undefined;
    let resolveWorkerStarted!: () => void;
    const workerStarted = new Promise<void>(resolve => { resolveWorkerStarted = resolve; });
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportObservationFixture: transportFixture,
      spawnCollector: () => {
        const collector = spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 });
        collector.stdout.on('data', chunk => { observedChunks.push(String(chunk)); });
        collector.stderr.on('data', chunk => { observedStderr.push(String(chunk)); });
        return collector;
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        assert.equal(active, true);
        assert.equal(finishStarted, false);
        workerDispatches++;
        capturedRequest = JSON.parse(String(options.input));
        resolveWorkerStarted();
        try { return await boundedCommand(binary, args, timeout, options); }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');let split=0;let fresh='';setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n--------- beginning of system\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(stalePath)})){fs.rmSync(${JSON.stringify(stalePath)},{force:true});process.stdout.write(${JSON.stringify(staleLine)}+'\\n')}if(fs.existsSync(${JSON.stringify(releasePath)})){fs.rmSync(${JSON.stringify(releasePath)},{force:true});split=1;fresh=(Date.now()/1000).toFixed(3);process.stdout.write(fresh+${JSON.stringify(freshPrefix)})}if(split===1&&fs.existsSync(${JSON.stringify(completePath)})){fs.rmSync(${JSON.stringify(completePath)},{force:true});split=2;process.stdout.write('ket, closing\\n'+fresh+${JSON.stringify(freshPrefix)}+'ket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await measurement.begin();
      active = true;
      assert.ok(observedChunks.some(chunk => chunk.includes('--------- beginning of system\n')),
        'collector must deliver the first SYSTEM divider after measurement START');
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await writeFile(stalePath, 'stale');
      await waitForValue(() => observedChunks.some(chunk => chunk.includes(staleLine)) ? true : undefined,
        5_000, 'collector did not deliver the stale replay after measurement START');
      assert.equal(workerDispatches, 0);
      await writeFile(releasePath, 'release');
      await waitForValue(() => observedChunks.some(chunk => chunk.includes(freshPrefix)) ? true : undefined,
        5_000, 'collector did not deliver the fresh split trigger prefix');
      assert.equal(workerDispatches, 0);
      await writeFile(completePath, 'complete');
      try {
        await waitForValue(() => workerDispatches === 1 ? true : undefined, 5_000, 'fresh split trigger did not dispatch');
      } catch (error) {
        throw new Error(`fresh split trigger did not dispatch: ${endpoint.requests.join(', ')}; complete=${existsSync(completePath)}; chunks=${JSON.stringify(observedChunks.slice(-4))}; stderr=${JSON.stringify(observedStderr)}`, { cause: error });
      }
      await workerStarted;
      assert.equal(workerDispatches, 1);
      assert.equal(capturedRequest?.guestClockSample?.guestEpochSeconds !== undefined, true);
      await workerFinished;
      assert.ok(observedChunks.some(chunk => chunk.includes('ket, closing')));
      await waitForValue(() => {
        const acquired = existsSync(transportFixture.acquisitionLogFile)
          ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').filter(Boolean) : [];
        return acquired.length === 3 && endpoint.completed.length === 3 ? true : undefined;
      }, 10_000, 'fresh transport trigger did not complete one five-source acquisition');
      await writeFile(transportFixture.stateFile, 'shutdown-time-state');
      finishStarted = true;
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      await finish;
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-transport-observation.json'), 'utf8'));
      assert.equal(observation.acquisitionStatus, 'collected');
      assert.deepEqual(observation.observations.map((entry: any) => entry.name), [
        'host-processes', 'host-memory', 'host-tcp', 'adb-state', 'guest-uptime',
      ]);
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-processes').stdout, 'trigger-time-state\n');
      assert.deepEqual(existsSync(transportFixture.acquisitionLogFile)
        ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').sort() : [],
      ['host-memory', 'host-processes', 'host-tcp']);
      assert.deepEqual(endpoint.completed.sort(), ['adb-state', 'guest-clock', 'guest-uptime']);
      assert.deepEqual(endpoint.requests.slice(0, 2), ['host:transport:emulator-5554', 'shell:date +%s.%N']);
      assert.equal(observation.triggerValidation.absoluteEmissionProven, false);
      assert.equal(observation.triggerValidation.freshnessLimitMs, 2_000);
      assert.equal(observation.observations.length, 5);
      assert.equal(workerDispatches, 1);
      assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
    } finally {
      finishStarted = true;
      await writeFile(completePath, 'complete');
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport acquisition cancellation retires workers and sockets', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint(true);
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'block-processes');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const endPath = join(fixture.root, 'cancelled-measurement-end');
    const triggerPath = join(fixture.root, 'cancelled-measurement-trigger');
    const observationPath = join(fixture.root, 'android-transport-observation.json');
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let workerFailure: BoundedCommandError | undefined;
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportObservationFixture: transportFixture,
      spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot') && args[args.indexOf('--boundary') + 1] === 'start') {
          await boundedCommand(binary, args, timeout, options);
          await writeFile(triggerPath, 'trigger');
          await waitForValue(() => {
            const acquisitions = existsSync(transportFixture.acquisitionLogFile)
              ? readFileSync(transportFixture.acquisitionLogFile, 'utf8') : '';
            return acquisitions.includes('host-processes') && endpoint.connected >= 3 ? true : undefined;
          }, 5_000, 'transport descendants and isolated ADB sockets did not start during begin');
          await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
          await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
          throw new Error('synthetic failed-begin snapshot after transport acquisition started');
        }
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        try { return await boundedCommand(binary, args, timeout, options); }
        catch (error) { if (error instanceof BoundedCommandError) workerFailure = error; throw error; }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I adbd: timeout expired while flushing socket, closing\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-transport-cancellation', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-transport-cancellation', { timeoutMs: 20_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot after transport acquisition started/u);
      await workerFinished;
      assert.ok(workerFailure instanceof BoundedCommandError);
      assert.equal(workerFailure.result.aborted, true);
      assert.equal(workerFailure.result.childExited, true);
      assert.equal(workerFailure.result.ownedProcessesExited, true);
      assert.equal(workerFailure.result.stdioClosed, true);
      const commandPID = await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
      const descendantPID = await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
      assert.equal(await waitForPidAbsent(commandPID, 1_000), true);
      assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      await waitForValue(() => endpoint.closed === 3 ? true : undefined, 2_000, 'cancelled ADB observation sockets remained open');
      assert.equal(existsSync(observationPath), false);
      assert.equal(existsSync(transportFixture.lateWriteFile), false);
      await measurement.finalizeGmsObservations();
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.transportObservationFailure, 'transport observation could not be saved');
      assert.equal(existsSync(observationPath), false);
      assert.equal(existsSync(transportFixture.lateWriteFile), false);
    } finally {
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport acquisition failure stays unavailable without late success', async () => {
    for (const mode of ['unavailable', 'nonzero', 'timeout'] as const) {
      const fixture = await harness.createFixture();
      const endpoint = await createAndroidTransportTestEndpoint(false,
        mode === 'unavailable' ? { stateFrame: Buffer.from('FAIL0000') } : {});
      const adbPort = endpoint.port;
      const state = mode === 'nonzero' ? 'nonzero-tcp' : mode === 'timeout' ? 'timeout-memory' : 'trigger-time-state';
      const transportFixture = await createAndroidTransportFixture(fixture.root, adbPort, state);
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const triggerPath = join(fixture.root, 'emit-failed-transport-trigger');
      const endPath = join(fixture.root, 'failed-transport-end');
      const observationPath = join(fixture.root, 'android-transport-observation.json');
      let resolveWorkerFinished!: () => void;
      const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
      let workerDispatches = 0;
      let collectorCode = '';
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        transportObservationFixture: transportFixture,
        spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
        boundedCommand: async (binary, args, timeout, options) => {
          if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
          workerDispatches++;
          try { return await boundedCommand(binary, args, timeout, options); }
          finally { resolveWorkerFinished(); }
        },
      });
      collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I adbd: timeout expired while flushing socket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
      try {
        const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
        measurement.requireGmsObservations();
        await measurement.observeGmsStage(stages[0], new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await measurement.observeGmsStage(stages[1], new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await measurement.begin();
        for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await writeFile(triggerPath, 'trigger');
        await waitForValue(() => workerDispatches === 1 ? true : undefined, 5_000,
          'fresh transport trigger did not dispatch before acquisition failure');
        await workerFinished;
        assert.equal(workerDispatches, 1);
        if (mode === 'timeout') {
          const commandPID = await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
          const descendantPID = await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
          assert.equal(await waitForPidAbsent(commandPID, 1_000), true);
          assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
          assert.equal(existsSync(transportFixture.lateWriteFile), false);
        }
        if (endpoint) await waitForValue(() => endpoint.closed === 3 ? true : undefined, 2_000, 'ADB acquisition sockets did not close');
        await writeFile(transportFixture.stateFile, 'shutdown-time-state');
        const finish = measurement.finish();
        await writeFile(endPath, 'end');
        const outcome = await finish;
        const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
        const bytes = await readFile(observationPath);
        const observation = JSON.parse(bytes.toString('utf8'));
        assert.equal(collector.transportObservationFailure, 'transport acquisition was unavailable or failed');
        assert.equal(observation.acquisitionStatus, 'failed');
        assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
        assert.ok(observation.observations.some((entry: any) => entry.unavailable || entry.timedOut || entry.truncated || entry.exitCode !== 0));
        assert.ok(outcome.errors.length > 0);
        assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
        if (mode === 'unavailable') assert.ok(observation.observations.some((entry: any) => entry.name === 'adb-state' && entry.unavailable));
        if (mode === 'nonzero') assert.equal(observation.observations.find((entry: any) => entry.name === 'host-tcp').exitCode, 7);
        if (mode === 'timeout') assert.equal(observation.observations.find((entry: any) => entry.name === 'host-memory').timedOut, true);
        assert.deepEqual(await readFile(observationPath), bytes);
        assert.equal(workerDispatches, 1);
        assert.equal(existsSync(transportFixture.lateWriteFile), false);
      } finally {
        await writeFile(endPath, 'end');
        await measurement.finish().catch(() => undefined);
        await endpoint?.stop();
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('concurrent GMS finalizers and finish share one terminal persistence operation', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let entered!: () => void;
    const finalWriteEntered = new Promise<void>(resolve => { entered = resolve; });
    let finalWrites = 0;
    const writers = gmsWriterDiagnostics(fixture.root);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--gms-diagnostic-persist')) return boundedCommand(binary, args, timeout, options);
        if (String(options.input || '').includes('"finalized":true')) {
          finalWrites++;
          entered();
          await gate;
        }
        return writers.run(binary, args, timeout, options);
      },
    });
    const diagnostics = () => ({ measurementErrors: measurement.failureOutcome().errors.map(error => String(error)), writerFailures: writers.failures });
    const details = () => JSON.stringify(diagnostics());
    let firstSettlement: Promise<PromiseSettledResult<void>> | undefined;
    let secondSettlement: Promise<PromiseSettledResult<void>> | undefined;
    let finishSettlement: Promise<PromiseSettledResult<unknown>> | undefined;
    let primaryFailure: unknown;
    let failed = false;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-finalizer-concurrency', { timeoutMs: 30_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-finalizer-concurrency', { timeoutMs: 30_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-finalizer-concurrency', { timeoutMs: 30_000 }));
      const precondition = await gmsStagePersistencePrecondition(fixture.root, stages, measurement);
      assert.deepEqual(precondition.violations, [], JSON.stringify({ ...precondition, writerFailures: writers.failures }));
      const first = measurement.finalizeGmsObservations();
      firstSettlement = settledResult(first);
      await awaitGatedEntry(finalWriteEntered, internalGmsFinalization(measurement), 'the terminal GMS writer', 'GMS finalization', diagnostics);
      let secondReturned = false;
      let finishReturned = false;
      const second = measurement.finalizeGmsObservations().then(() => { secondReturned = true; });
      secondSettlement = settledResult(second);
      const finish = measurement.finish().then(() => { finishReturned = true; });
      finishSettlement = settledResult(finish);
      await new Promise(resolve => setTimeout(resolve, 50));
      assert.equal(secondReturned, false, details());
      assert.equal(finishReturned, false, details());
      assert.equal(finalWrites, 1, details());
      release();
      await Promise.all([first, second, finish]);
      const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
      const bytes = await readFile(observationsPath);
      const observations = JSON.parse(bytes.toString('utf8'));
      assert.equal(observations.finalized, true, details());
      assert.equal(observations.errors.length, 0, details());
      assert.equal(finalWrites, 1, details());
      const state = JSON.stringify(measurement.failureOutcome().errors.map(error => error instanceof Error ? error.message : String(error)));
      await new Promise(resolve => setTimeout(resolve, 50));
      assert.deepEqual(await readFile(observationsPath), bytes, details());
      assert.equal(JSON.stringify(measurement.failureOutcome().errors.map(error => error instanceof Error ? error.message : String(error))), state, details());
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.gmsObservations.errors, 0, details());
    } catch (error) {
      failed = true;
      primaryFailure = error;
    }
    release();
    finishSettlement ??= settledResult(measurement.finish());
    await Promise.all([finishSettlement, firstSettlement, secondSettlement]);
    const failureDetails = details();
    let cleanupFailure: unknown;
    try { await writers.close(); } catch (error) { cleanupFailure = error; }
    for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
    Object.assign(process.env, saved);
    if (failed || cleanupFailure !== undefined) {
      throw new AggregateError([...(failed ? [primaryFailure] : []), ...(cleanupFailure === undefined ? [] : [cleanupFailure])],
        `GMS concurrent finalizers failed: ${String(primaryFailure ?? cleanupFailure)}: ${failureDetails}`);
    }
  });
  test('GMS finalizer-first barriers fail promptly when an earlier stage persistence prevents the terminal writer', async () => {
    const entryDetails = () => ({ case: 'barrier' });
    const entered = groupPollTestTimers();
    let enter!: () => void;
    const entry = new Promise<void>(resolve => { enter = resolve; });
    const enteredBarrier = awaitGatedEntry(entry, new Promise<void>(() => undefined), 'the writer', 'finalization', entryDetails, entered);
    assert.equal(entered.timers.length, 1);
    assert.equal(entered.timers[0]!.delayMs, 20_000);
    enter();
    await enteredBarrier;
    assert.equal(entered.timers[0]!.cancelled, true);
    assert.deepEqual(entered.pending(), []);

    const settledEarly = groupPollTestTimers();
    let lateEnter!: () => void;
    const lateEntry = new Promise<void>(resolve => { lateEnter = resolve; });
    const earlyBarrier = awaitGatedEntry(lateEntry, Promise.resolve(), 'the writer', 'finalization', entryDetails, settledEarly);
    await assert.rejects(earlyBarrier, (error: unknown) => error instanceof assert.AssertionError
      && /^finalization settled before the writer was entered: \{"case":"barrier"\}$/u.test(error.message));
    lateEnter();
    assert.deepEqual(settledEarly.pending(), []);

    const failedEarly = groupPollTestTimers();
    await assert.rejects(awaitGatedEntry(new Promise<void>(() => undefined), Promise.reject(new Error('synthetic finalization failure')),
      'the writer', 'finalization', entryDetails, failedEarly), (error: unknown) => error instanceof assert.AssertionError
      && /^finalization failed before the writer was entered \(synthetic finalization failure\): /u.test(error.message));
    assert.deepEqual(failedEarly.pending(), []);

    const unentered = groupPollTestTimers();
    const watchdog: { outcome: unknown } = { outcome: 'pending' };
    const watchdogBarrier = awaitGatedEntry(new Promise<void>(() => undefined), new Promise<void>(() => undefined),
      'the writer', 'finalization', entryDetails, unentered).then(() => { watchdog.outcome = 'entered'; }, error => { watchdog.outcome = error; });
    await new Promise(resolve => setImmediate(resolve));
    const beforeDelivery = watchdog.outcome;
    assert.equal(beforeDelivery, 'pending');
    unentered.deliver(unentered.timers[0]!);
    await watchdogBarrier;
    const watchdogFailure = watchdog.outcome;
    assert.ok(watchdogFailure instanceof assert.AssertionError, String(watchdogFailure));
    assert.match(watchdogFailure.message, /^the writer was not entered within 20000 ms: /u);

    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    let terminalWriterInvoked = false;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--gms-diagnostic-persist')) {
          if (String(options.input || '').includes('"finalized":true')) terminalWriterInvoked = true;
          else throw new Error('synthetic earlier stage persistence failure');
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    let finalizationSettlement: Promise<PromiseSettledResult<void>> | undefined;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-finalizer-barrier', { timeoutMs: 20_000 }));
      const precondition = await gmsStagePersistencePrecondition(fixture.root, stages, measurement);
      assert.match(precondition.violations[0] ?? '', /^GMS observations are missing or unreadable: /u, JSON.stringify(precondition));
      assert.ok(precondition.measurementErrors.some(error => error.includes('synthetic earlier stage persistence failure')), JSON.stringify(precondition));
      assert.ok(precondition.violations.some(violation => violation.startsWith('measurement error: ')
        && violation.includes('synthetic earlier stage persistence failure')), JSON.stringify(precondition));
      const timers = groupPollTestTimers();
      let writerEntered!: () => void;
      const writerEntry = new Promise<void>(resolve => { writerEntered = resolve; });
      finalizationSettlement = settledResult(measurement.finalizeGmsObservations());
      let barrierOutcome: unknown = 'pending';
      const barrier = awaitGatedEntry(writerEntry, internalGmsFinalization(measurement), 'the terminal GMS writer', 'GMS finalization',
        () => precondition, timers).then(() => { barrierOutcome = 'entered'; }, error => { barrierOutcome = error; });
      await internalGmsFinalization(measurement);
      await new Promise(resolve => setImmediate(resolve));
      assert.equal(terminalWriterInvoked, false);
      assert.ok(barrierOutcome instanceof assert.AssertionError, `a bare writer-entry wait stays ${String(barrierOutcome)} here`);
      assert.match(barrierOutcome.message, /^GMS finalization settled before the terminal GMS writer was entered: .*synthetic earlier stage persistence failure/u);
      assert.equal(timers.timers.length, 1);
      assert.deepEqual(timers.pending(), []);
      writerEntered();
      await barrier;
      assert.equal((await finalizationSettlement).status, 'fulfilled');
    } finally {
      await finalizationSettlement;
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS finalizers started before finish wait through collector confirmation success and failure', async () => {
    for (const failCollectorWrite of [false, true]) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      let releaseObservation!: () => void;
      const observationGate = new Promise<void>(resolve => { releaseObservation = resolve; });
      let observationEntered!: () => void;
      const observationWriterEntered = new Promise<void>(resolve => { observationEntered = resolve; });
      let releaseCollector!: () => void;
      const collectorGate = new Promise<void>(resolve => { releaseCollector = resolve; });
      let collectorEntered!: () => void;
      const collectorWriterEntered = new Promise<void>(resolve => { collectorEntered = resolve; });
      const writers = gmsWriterDiagnostics(fixture.root);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('--gms-diagnostic-persist')) {
            if (String(options.input || '').includes('"finalized":true')) {
              observationEntered();
              await observationGate;
            }
            return writers.run(binary, args, timeout, options);
          }
          if (args.includes('--android-measurement-persist-json')) {
            const request = JSON.parse(String(options.input));
            if (request.target.endsWith('android-environment-collector.json')) {
              collectorEntered();
              await collectorGate;
              if (failCollectorWrite) throw new Error('synthetic finalizer-first collector write failure');
            }
          }
          return boundedCommand(binary, args, timeout, options);
        },
      });
      const diagnostics = () => ({ measurementErrors: measurement.failureOutcome().errors.map(error => String(error)), writerFailures: writers.failures });
      let firstSettlement: Promise<PromiseSettledResult<void>> | undefined;
      let secondSettlement: Promise<PromiseSettledResult<void>> | undefined;
      let finishSettlement: Promise<PromiseSettledResult<unknown>> | undefined;
      let primaryFailure: unknown;
      let failed = false;
      try {
        const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
        measurement.requireGmsObservations();
        await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-finalizer-first', { timeoutMs: 20_000 }));
        await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-finalizer-first', { timeoutMs: 20_000 }));
        await measurement.begin();
        for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-finalizer-first', { timeoutMs: 20_000 }));
        const precondition = await gmsStagePersistencePrecondition(fixture.root, stages, measurement);
        assert.deepEqual(precondition.violations, [], JSON.stringify({ ...precondition, writerFailures: writers.failures }));
        let firstReturned = false;
        let secondReturned = false;
        let finishReturned = false;
        const first = measurement.finalizeGmsObservations().then(() => { firstReturned = true; });
        firstSettlement = settledResult(first);
        await awaitGatedEntry(observationWriterEntered, internalGmsFinalization(measurement), 'the terminal GMS writer',
          'GMS finalization', diagnostics);
        const second = measurement.finalizeGmsObservations().then(() => { secondReturned = true; });
        secondSettlement = settledResult(second);
        const finish = measurement.finish().then(outcome => { finishReturned = true; return outcome; });
        finishSettlement = settledResult(finish);
        await new Promise(resolve => setTimeout(resolve, 40));
        assert.equal(firstReturned, false);
        assert.equal(secondReturned, false);
        assert.equal(finishReturned, false);
        releaseObservation();
        await awaitGatedEntry(collectorWriterEntered, finish, 'the collector summary writer', 'measurement finish', diagnostics);
        await new Promise(resolve => setTimeout(resolve, 40));
        assert.equal(firstReturned, false);
        assert.equal(secondReturned, false);
        assert.equal(finishReturned, false);
        releaseCollector();
        const outcome = await finish;
        const finalizers = await Promise.all([firstSettlement, secondSettlement]);
        const settledErrors = measurement.failureOutcome().errors.map(error => String(error));
        await new Promise(resolve => setTimeout(resolve, 30));
        assert.deepEqual(measurement.failureOutcome().errors.map(error => String(error)), settledErrors);
        if (failCollectorWrite) {
          assert.ok(outcome.errors.some(error => String(error).includes('synthetic finalizer-first collector write failure')));
          assert.ok(finalizers.every(result => result.status === 'rejected' && /terminal collector confirmation failed/u.test(String(result.reason))));
          assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
          assert.equal(existsSync(join(fixture.root, 'android-environment-collector.json')), false);
        } else {
          assert.deepEqual(outcome.errors, [], JSON.stringify({ errors: outcome.errors.map(error => String(error)), writerFailures: writers.failures }));
          assert.ok(finalizers.every(result => result.status === 'fulfilled'));
          assert.equal(outcome.assessment?.collection.status, 'PASS');
          const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
          assert.equal(collector.gmsObservations.terminalConfirmation, 'confirmed');
          assert.equal(JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8')).finalized, true);
        }
      } catch (error) {
        failed = true;
        primaryFailure = error;
      }
      releaseObservation();
      releaseCollector();
      finishSettlement ??= settledResult(measurement.finish());
      await Promise.all([finishSettlement, firstSettlement, secondSettlement]);
      let cleanupFailure: unknown;
      try { await writers.close(); } catch (error) { cleanupFailure = error; }
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
      if (cleanupFailure !== undefined) {
        const primary = !failed ? undefined : primaryFailure instanceof Error ? primaryFailure.message : String(primaryFailure);
        throw new AggregateError(failed ? [primaryFailure, cleanupFailure] : [cleanupFailure],
          `${primary ? `${primary}; ` : ''}GMS writer observer cleanup failed: ${String(cleanupFailure)}`);
      }
      if (failed) throw primaryFailure;
    }
    const noFinishFixture = await harness.createFixture();
    const noFinishStages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const noFinishEnvironment = { ...process.env };
    Object.assign(process.env, noFinishFixture.environment);
    const endPath = join(noFinishFixture.root, 'no-finish-end-marker');
    let collectorCode = '';
    let finalPersisted!: () => void;
    const finalPersistence = new Promise<void>(resolve => { finalPersisted = resolve; });
    const noFinishWriters = gmsWriterDiagnostics(noFinishFixture.root);
    const noFinishMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', noFinishFixture.root,
      repositoryPath('tests/mobile/toolchains.json'), {
        spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode],
          { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
        boundedCommand: async (binary, args, timeout, options) => {
          if (!args.includes('--gms-diagnostic-persist')) return boundedCommand(binary, args, timeout, options);
          const result = await noFinishWriters.run(binary, args, timeout, options);
          if (String(options.input).includes('"finalized":true')) finalPersisted();
          return result;
        },
      });
    const noFinishDiagnostics = () => ({ measurementErrors: noFinishMeasurement.failureOutcome().errors.map(error => String(error)),
      writerFailures: noFinishWriters.failures });
    let noFinishFinalizationSettlement: Promise<PromiseSettledResult<void>> | undefined;
    let noFinishFinishSettlement: Promise<PromiseSettledResult<unknown>> | undefined;
    let noFinishPrimaryFailure: unknown;
    let noFinishFailed = false;
    collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${noFinishMeasurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${noFinishMeasurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      noFinishMeasurement.requireGmsObservations();
      await noFinishMeasurement.observeGmsStage(noFinishStages[0], new PhaseBudget('gms-no-finish', { timeoutMs: 20_000 }));
      await noFinishMeasurement.observeGmsStage(noFinishStages[1], new PhaseBudget('gms-no-finish', { timeoutMs: 20_000 }));
      await noFinishMeasurement.begin();
      for (const stage of noFinishStages.slice(2)) await noFinishMeasurement.observeGmsStage(stage, new PhaseBudget('gms-no-finish', { timeoutMs: 20_000 }));
      const precondition = await gmsStagePersistencePrecondition(noFinishFixture.root, noFinishStages, noFinishMeasurement);
      assert.deepEqual(precondition.violations, [], JSON.stringify({ ...precondition, writerFailures: noFinishWriters.failures }));
      let finalized = false;
      const finalization = noFinishMeasurement.finalizeGmsObservations().then(() => { finalized = true; });
      noFinishFinalizationSettlement = settledResult(finalization);
      await awaitGatedEntry(finalPersistence, internalGmsFinalization(noFinishMeasurement), 'the completed terminal GMS writer',
        'GMS finalization', noFinishDiagnostics);
      assert.equal(JSON.parse(await readFile(join(noFinishFixture.root, 'android-environment-gms-observations.json'), 'utf8')).finalized, true);
      assert.equal(existsSync(join(noFinishFixture.root, 'android-environment-collector.json')), false);
      assert.equal(finalized, false);
      await writeFile(endPath, 'end');
      const finish = noFinishMeasurement.finish();
      noFinishFinishSettlement = settledResult(finish);
      const outcome = await finish;
      await finalization;
      assert.deepEqual(outcome.errors, [], JSON.stringify({ errors: outcome.errors.map(error => String(error)), writerFailures: noFinishWriters.failures }));
      assert.equal(outcome.assessment?.collection.status, 'PASS');
      assert.equal(finalized, true);
    } catch (error) {
      noFinishFailed = true;
      noFinishPrimaryFailure = error;
    }
    let noFinishCleanupFailure: unknown;
    try { await writeFile(endPath, 'end'); } catch (error) { noFinishCleanupFailure = error; }
    noFinishFinishSettlement ??= settledResult(noFinishMeasurement.finish());
    await Promise.all([noFinishFinishSettlement, noFinishFinalizationSettlement]);
    try { await noFinishWriters.close(); } catch (error) { noFinishCleanupFailure ??= error; }
    for (const key of Object.keys(process.env)) if (!(key in noFinishEnvironment)) delete process.env[key];
    Object.assign(process.env, noFinishEnvironment);
    if (noFinishCleanupFailure !== undefined) {
      const primary = !noFinishFailed ? undefined
        : noFinishPrimaryFailure instanceof Error ? noFinishPrimaryFailure.message : String(noFinishPrimaryFailure);
      throw new AggregateError(noFinishFailed ? [noFinishPrimaryFailure, noFinishCleanupFailure] : [noFinishCleanupFailure],
        `${primary ? `${primary}; ` : ''}GMS no-finish cleanup failed: ${String(noFinishCleanupFailure)}`);
    }
    if (noFinishFailed) throw noFinishPrimaryFailure;
  });
  test('GMS successful-begin finalizers share finish through terminal collector confirmation', async () => {
    for (const failCollectorWrite of [false, true]) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      let release!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      let entered!: () => void;
      const writerEntered = new Promise<void>(resolve => { entered = resolve; });
      let collectorWrites = 0;
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('--android-measurement-persist-json')) {
            const request = JSON.parse(String(options.input));
            if (request.target.endsWith('android-environment-collector.json')) {
              collectorWrites++;
              entered();
              await gate;
              if (failCollectorWrite) throw new Error('synthetic successful-begin collector write failure');
            }
          }
          return boundedCommand(binary, args, timeout, options);
        },
      });
      try {
        const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
        measurement.requireGmsObservations();
        await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-successful-terminal', { timeoutMs: 20_000 }));
        await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-successful-terminal', { timeoutMs: 20_000 }));
        await measurement.begin();
        for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-successful-terminal', { timeoutMs: 20_000 }));
        let finishReturned = false;
        const finish = measurement.finish().then(outcome => { finishReturned = true; return outcome; });
        await writerEntered;
        let firstReturned = false;
        let secondReturned = false;
        const first = measurement.finalizeGmsObservations().then(() => { firstReturned = true; });
        const second = measurement.finalizeGmsObservations().then(() => { secondReturned = true; });
        const settledFinalizers = Promise.allSettled([first, second]);
        await new Promise(resolve => setTimeout(resolve, 40));
        assert.equal(finishReturned, false);
        assert.equal(firstReturned, false);
        assert.equal(secondReturned, false);
        assert.equal(collectorWrites, 1);
        release();
        const outcome = await finish;
        const finalizers = await settledFinalizers;
        assert.equal(finishReturned, true);
        assert.equal(collectorWrites, 1);
        if (failCollectorWrite) {
          assert.ok(outcome.errors.some(error => String(error).includes('synthetic successful-begin collector write failure')));
          assert.ok(finalizers.every(result => result.status === 'rejected' && /terminal collector confirmation failed/u.test(String(result.reason))));
          assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
        } else {
          assert.deepEqual(outcome.errors, []);
          assert.ok(finalizers.every(result => result.status === 'fulfilled'));
          assert.equal(outcome.assessment?.collection.status, 'PASS');
          const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
          assert.equal(collector.gmsObservations.terminalConfirmation, 'confirmed');
        }
      } finally {
        release();
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('final GMS publication reports an explicitly scoped prior-persistence total and bound', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const finalWriteDelayMs = 90;
    let finalWrites = 0;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--gms-diagnostic-persist') && String(options.input || '').includes('"finalized":true')) {
          finalWrites++;
          await new Promise(resolve => setTimeout(resolve, finalWriteDelayMs));
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-persistence-accounting', { timeoutMs: 30_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-persistence-accounting', { timeoutMs: 30_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-persistence-accounting', { timeoutMs: 30_000 }));
      await measurement.finish();
      assert.equal(finalWrites, 1);
      const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      const persistedPrior = observations.priorPersistenceAttemptMs;
      const completedAll = collector.gmsObservations.persistenceAttemptMsThroughFinalization;
      assert.ok(persistedPrior > 0);
      assert.ok(completedAll - persistedPrior >= finalWriteDelayMs);
      assert.equal(observations.persistenceAccounting,
        'priorPersistenceAttemptMs includes prior bounded publication attempts; the current attempt is excluded and bounded by persistenceOperationTimeoutMs');
      assert.equal(observations.limits.persistenceOperationTimeoutMs, 1_000);
      assert.equal(observations.temporaryEvidenceCleanup, 'not-needed');
      assert.equal(collector.gmsObservations.persistenceAttemptMsThroughFinalization, completedAll);
      assert.match(collector.gmsObservations.persistenceAccounting, /includes all bounded observation-publication attempts through finalization/u);
      assert.equal(collector.gmsObservations.temporaryEvidenceCleanup, 'not-needed');
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('bounded stdin relays preserve multi-buffer bytes and natural EOF after delayed target consumption', async () => {
    const fixture = await harness.createFixture();
    const input = Buffer.alloc(256 * 1024);
    for (let index = 0; index < input.length; index++) input[index] = index % 251;
    const sha256 = createHash('sha256').update(input).digest('hex');
    const code = `setTimeout(async()=>{const hash=require('node:crypto').createHash('sha256');let bytes=0;for await(const chunk of process.stdin){bytes+=chunk.length;hash.update(chunk)}process.stdout.write(JSON.stringify({bytes,sha256:hash.digest('hex'),eof:process.stdin.readableEnded})+'\\n')},200)`;
    for (const [name, binary, args] of [['bun', process.execPath, ['--no-env-file', '-e', code]], ['node', 'node', ['-e', code]]] as const) {
      const observer = createBoundedSupervisorObserver(fixture.root, `delayed-input-${name}-supervisor`);
      try {
        const result = await boundedCommand(binary, [...args], 5_000, {
          input, cleanupReservationMs: 1_000, maxBytes: 1_024, observerSupervisor: observer.stream,
        });
        const records = observer.records();
        const details = JSON.stringify({ command: boundedCommandFailureDetails(result, records), records });
        assert.deepEqual(JSON.parse(result.stdout), { bytes: input.length, sha256, eof: true }, details);
        assert.equal(result.childExited, true, details);
        assert.equal(result.ownedProcessesExited, true, details);
        assert.equal(result.stdioClosed, true, details);
        assert.equal(result.timedOut, false, details);
        for (const type of ['supervisor-input-observation', 'supervisor-manager-input-observation']) {
          const observations = records.filter(record => record.type === type);
          assert.ok(observations.length <= 9, details);
          for (const event of ['connected', 'sourceEnd', 'sourceClose', 'destinationFinish', 'destinationClose']) {
            assert.equal(observations.filter(record => record.event === event).length, 1, details);
          }
          assert.equal(observations.some(record => record.event === 'sourceError' || record.event === 'destinationError'), false, details);
          const ended = observations.find(record => record.event === 'sourceEnd')!.summary as Record<string, unknown>;
          assert.equal(ended.sourceBytes, input.length, details);
          assert.equal(ended.countSaturated, false, details);
          assert.ok(BigInt(String(ended.firstByteNs)) <= BigInt(String(ended.lastByteNs)), details);
          assert.ok(BigInt(String(ended.lastByteNs)) <= BigInt(String(ended.sourceEndNs)), details);
          const finished = observations.find(record => record.event === 'destinationFinish')!.summary as Record<string, unknown>;
          assert.equal(finished.destinationWritableLength, 0, details);
          assert.equal(finished.destinationFinished, true, details);
        }
        const teardown = records.find(record => record.type === 'supervisor-teardown-started');
        assert.deepEqual(teardown?.unmetRetirementPrerequisites, [], details);
        assert.ok(Buffer.byteLength(JSON.stringify(records)) < 32_768, details);
      } finally {
        await observer.close();
      }
    }
  });
  test('bounded child commands count mixed output on failure, stop overflow and kill SIGTERM-resistant children', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v24.21.0');
    const code = `const fs=require('node:fs');const chunk=Buffer.alloc(1100000,65);const write=fd=>{let offset=0;while(offset<chunk.length)offset+=fs.writeSync(fd,chunk,offset,chunk.length-offset)};write(1);write(2);setTimeout(()=>{process.exitCode=7},250)`;
    let failed: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', code], 5_000, { maxBytes: 3_000_000, maxBytesPerStream: 2_000_000 });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      failed = error;
    }
    assert.ok(failed);
    assert.equal(failed.result.code, 7);
    assert.equal(failed.result.stdoutBytes, 1_100_000, JSON.stringify({ code: failed.result.code,
      durationMs: failed.result.durationMs, timedOut: failed.result.timedOut, aborted: failed.result.aborted,
      stdoutBytes: failed.result.stdoutBytes, stderrBytes: failed.result.stderrBytes,
      childExited: failed.result.childExited, ownedProcessesExited: failed.result.ownedProcessesExited,
      stdioClosed: failed.result.stdioClosed }));
    assert.equal(failed.result.stderrBytes, 1_100_000);
    assert.equal(failed.result.stdoutRetainedBytes, 1_100_000);
    assert.equal(failed.result.stderrRetainedBytes, 1_100_000);
    assert.equal(failed.result.childExited, true);
    assert.equal(failed.result.stdoutSha256, createHash('sha256').update(Buffer.alloc(1_100_000, 65)).digest('hex'));

    let overflow: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', code], 3_000, { maxBytes: 1_000_000, maxBytesPerStream: 2_000_000 });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      overflow = error;
    }
    assert.ok(overflow);
    assert.equal(overflow.result.outputLimitExceeded, true);
    assert.ok(overflow.result.stdoutBytes + overflow.result.stderrBytes > 1_000_000);
    assert.ok(overflow.result.stdoutRetainedBytes + overflow.result.stderrRetainedBytes <= 1_000_000);
    assert.equal(overflow.result.childExited, true);

    const fixture = await harness.createFixture();
    const sentinel = join(fixture.root, 'late-child-sentinel');
    const controller = new AbortController();
    const lateCode = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(sentinel)},'late');process.exit(0)},500)`;
    const timeoutObserver = createBoundedSupervisorObserver(fixture.root, 'bounded-timeout-supervisor');
    const watchdog = setTimeout(() => controller.abort(), 1_500);
    try {
      let timedOut: BoundedCommandError | undefined;
      try {
        await boundedCommand('node', ['-e', lateCode], 250, {
          maxBytes: 1_000, signal: controller.signal, observerSupervisor: timeoutObserver.stream,
        });
      } catch (error) {
        assert.ok(error instanceof BoundedCommandError);
        timedOut = error;
      }
      await timeoutObserver.close();
      const timeoutDetails = JSON.stringify({ command: boundedCommandFailureDetails(timedOut?.result, timeoutObserver.records()) });
      assert.ok(timedOut, timeoutDetails);
      assert.equal(timedOut.result.timedOut, true, timeoutDetails);
      assert.equal(timedOut.result.childExited, true, timeoutDetails);
      assert.ok(timedOut.result.durationMs < 1_000, timeoutDetails);
      const settledEvidence = JSON.stringify(timedOut.result);
      await new Promise(resolve => setTimeout(resolve, 350));
      assert.equal(existsSync(sentinel), false);
      assert.equal(JSON.stringify(timedOut.result), settledEvidence);
    } finally {
      await timeoutObserver.close();
      clearTimeout(watchdog);
    }

    const descendantSentinel = join(fixture.root, 'late-descendant-sentinel');
    const descendantCode = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(descendantSentinel)},'late');process.exit(0)},500);setInterval(()=>{},1000)`;
    const parentCode = `require('node:child_process').spawn(process.execPath,['-e',${JSON.stringify(descendantCode)}],{stdio:'inherit'});process.exit(0)`;
    const descendantObserver = createBoundedSupervisorObserver(fixture.root, 'bounded-descendant-supervisor');
    let descendantResult: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', parentCode], 250, {
        maxBytes: 1_000, cleanupReservationMs: 150, observerSupervisor: descendantObserver.stream,
      });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      descendantResult = error;
    }
    await descendantObserver.close();
    const descendantDetails = JSON.stringify({ command: boundedCommandFailureDetails(descendantResult?.result, descendantObserver.records()) });
    assert.ok(descendantResult, descendantDetails);
    assert.equal(descendantResult.result.timedOut, true, descendantDetails);
    assert.equal(descendantResult.result.childExited, true, descendantDetails);
    assert.equal(descendantResult.result.ownedProcessesExited, true, descendantDetails);
    assert.equal(descendantResult.result.stdioClosed, true, descendantDetails);
    await new Promise(resolve => setTimeout(resolve, 400));
    assert.equal(existsSync(descendantSentinel), false);

    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const fastSignal = spawnOwnedProcess(nodePath, ['-e', "process.kill(process.pid,'SIGTERM')"], { timeoutMs: 5_000, cleanupReservationMs: 1_000 });
    let fastOutput = '';
    fastSignal.stdout.on('data', chunk => { fastOutput += String(chunk); });
    try {
      await fastSignal.ready;
      fastSignal.start();
      const binding = await fastSignal.started;
      assert.ok(binding, 'close-on-exec status EOF must preserve admission before an immediate target signal');
      const exit = await fastSignal.targetExit;
      const close = await fastSignal.targetClose;
      assert.ok(exit);
      assert.ok(close);
      assert.equal(exit.code, null);
      assert.equal(exit.signal, 'SIGTERM');
      assert.equal(exit.launchError, undefined);
      assert.equal(close.code, null);
      assert.equal(close.signal, 'SIGTERM');
      assert.equal(close.launchError, undefined);
      assert.equal(fastOutput, '');
      const retirement = await fastSignal.retire('fast-signaled-target');
      assert.equal(retirement.groupAbsent, true);
      assert.equal(retirement.anchorExitObserved, true);
      assert.equal(retirement.anchorCloseObserved, true);
      assert.equal(retirement.stdoutCloseObserved, true);
      assert.equal(retirement.stderrCloseObserved, true);
    } finally {
      if (!fastSignal.isSupervisorClosed) await fastSignal.retire('fast-signaled-target-cleanup');
    }

    const delayedRuntimeDirectory = join(fixture.root, 'bounded-delayed-runtime-bin');
    const wrapperPIDPath = join(fixture.root, 'bounded-delayed-runtime-pid');
    const startupDelayPath = join(fixture.root, 'bounded-supervisor-startup-delay.mjs');
    const startupObserverPath = join(fixture.root, 'bounded-supervisor-startup-observer.jsonl');
    await mkdir(delayedRuntimeDirectory, { recursive: true });
    await writeFile(startupDelayPath, 'Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,900);');
    const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
    const realBun = execFileSync('which', ['bun'], { encoding: 'utf8' }).trim();
    const wrapper = `#!/bin/sh\nfor argument do\n  if [ "$argument" = '--supervisor' ]; then\n    printf '%s\\n' "$$" > ${quote(wrapperPIDPath)}\n    exec ${quote(realBun)} --no-env-file --preload ${quote(startupDelayPath)} "$@"\n  fi\ndone\nexec ${quote(realBun)} --no-env-file "$@"\n`;
    await writeFile(join(delayedRuntimeDirectory, 'bun'), wrapper, { mode: 0o700 });
    const observerFD = openSync(startupObserverPath, 'w');
    const observerSupervisor = createWriteStream('', { fd: observerFD, autoClose: false });
    const previousPath = process.env.PATH;
    let startupTimeout: BoundedCommandError | undefined;
    try {
      process.env.PATH = `${delayedRuntimeDirectory}:${previousPath || ''}`;
      await boundedCommand('node', ['-e', 'process.exit(0)'], 1_800, {
        maxBytes: 1_024, cleanupReservationMs: 600, observerSupervisor,
      });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      startupTimeout = error;
    } finally {
      if (previousPath === undefined) delete process.env.PATH;
      else process.env.PATH = previousPath;
      await new Promise<void>(resolve => observerSupervisor.end(resolve));
      closeSync(observerFD);
    }
    assert.ok(startupTimeout);
    assert.equal(startupTimeout.result.timedOut, true);
    assert.equal(startupTimeout.result.aborted, false);
    assert.equal(startupTimeout.result.stdioClosed, true);
    assert.ok(startupTimeout.result.durationMs < 1_800, JSON.stringify(startupTimeout.result));
    const delayedSupervisorPID = await waitForFileValue(wrapperPIDPath, text => Number(text), 1_000);
    assert.ok(Number.isSafeInteger(delayedSupervisorPID) && delayedSupervisorPID > 0);
    assert.equal(await waitForPidAbsent(delayedSupervisorPID, 1_000), true);
    assert.equal(await waitForGroupAbsent(delayedSupervisorPID, 1_000), true);
    const startupRecords = (await readFile(startupObserverPath, 'utf8')).trim().split('\n')
      .map(line => JSON.parse(line) as { type?: string; clockNs?: string; executionDeadlineNs?: string; hardDeadlineNs?: string; evidence?: Record<string, unknown> });
    const runtimeStarted = startupRecords.find(record => record.type === 'supervisor-runtime-started');
    const deadlines = startupRecords.find(record => record.type === 'supervisor-deadlines-armed');
    assert.ok(runtimeStarted && deadlines);
    assert.ok(BigInt(runtimeStarted.clockNs!) > BigInt(deadlines.executionDeadlineNs!), 'supervisor must initialize after the original execution deadline');
    assert.ok(BigInt(runtimeStarted.clockNs!) < BigInt(deadlines.hardDeadlineNs!), 'supervisor initialization must remain inside the original hard deadline');
    const retirementRecord = startupRecords.find(record => record.type === 'supervisor-retirement-ready');
    assert.ok(retirementRecord);
    assert.equal(retirementRecord.evidence?.anchorCloseObserved, true);
    assert.equal(retirementRecord.evidence?.groupAbsent, true);
    assert.equal(retirementRecord.evidence?.targetExitObserved, false);
    assert.equal(startupRecords.some(record => record.type === 'anchor-process-started' || record.type === 'target-process-started'
      || record.type === 'target-started'), false);
    assert.equal(startupRecords.some(record => record.type === 'supervisor-teardown-started'), true);
  });
  test('bounded explicit abort remains aborted through owned process retirement', async () => {
    const fixture = await harness.createFixture();
    const readyPath = join(fixture.root, 'bounded-abort-target-ready');
    const controller = new AbortController();
    const observer = createBoundedSupervisorObserver(fixture.root, 'bounded-abort-supervisor');
    const code = `process.on('SIGTERM',()=>{});require('node:fs').writeFileSync(${JSON.stringify(readyPath)},'ready');setInterval(()=>{},1000)`;
    const command = boundedCommand(process.execPath, ['-e', code], 5_000, {
      maxBytes: 1_024, cleanupReservationMs: 1_000, signal: controller.signal, observerSupervisor: observer.stream,
    }).then(result => ({ result }), error => ({ error }));
    try {
      await waitForFileValue(readyPath, text => text, 2_000);
      controller.abort(new Error('unit test cancellation'));
      const outcome = await command;
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(
        'error' in outcome && outcome.error instanceof BoundedCommandError ? outcome.error.result : undefined, observer.records()) });
      assert.ok('error' in outcome && outcome.error instanceof BoundedCommandError, details);
      assert.equal(outcome.error.result.aborted, true, details);
      assert.equal(outcome.error.result.timedOut, false, details);
      assert.equal(outcome.error.result.outputLimitExceeded, false, details);
      assert.equal(outcome.error.result.childExited, true, details);
      assert.equal(outcome.error.result.ownedProcessesExited, true, details);
      assert.equal(outcome.error.result.stdioClosed, true, details);
      const retirement = observer.records().find(record => record.type === 'supervisor-retirement-ready');
      assert.ok(retirement, details);
      const evidence = retirement.evidence as Record<string, unknown>;
      assert.equal(evidence.stopReason, 'aborted', details);
    } finally {
      if (!controller.signal.aborted) controller.abort(new Error('unit test cleanup'));
      await command;
      await observer.close();
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('failed GMS commands account for both streams against the aggregate byte ceiling', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v24.21.0');
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const code = `const fs=require('node:fs');const chunk=Buffer.alloc(1100000,65);const write=fd=>{let offset=0;while(offset<chunk.length)offset+=fs.writeSync(fd,chunk,offset,chunk.length-offset)};write(1);write(2);setTimeout(()=>{process.exitCode=7},250)`;
    let dispatches = 0;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      return boundedCommand('node', ['-e', code], timeout, options);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      assert.equal(dispatches, stages.length);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const acquired = observations.stages.reduce((total: number, entry: any) => total + (entry.stdoutBytes || 0) + (entry.stderrBytes || 0), 0);
      const retained = observations.stages.reduce((total: number, entry: any) => total + (entry.stdoutRetainedBytes || 0) + (entry.stderrRetainedBytes || 0), 0);
      assert.equal(observations.totalCommandOutputBytes, acquired);
      assert.equal(observations.totalRetainedCommandOutputBytes, retained);
      assert.ok(acquired > observations.limits.totalCommandOutputBytes);
      assert.ok(acquired <= observations.limits.totalCommandOutputBytes + 2_200_000);
      assert.ok(retained <= observations.limits.totalCommandOutputBytes);
      assert.ok(observations.stages.every((entry: any) => entry.outcome === 'failed'));
      assert.equal(observations.stages.at(-1).outputLimitExceeded, true);
      assert.equal(observations.stages.at(-1).childExited, true);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS writer progress reports each reached persistence step from bounded stderr markers only', async () => {
    assert.equal(gmsWriterProgress(''), 'none');
    assert.equal(gmsWriterProgress('GMS persistence request is malformed\n'), 'none');
    assert.equal(gmsWriterProgress('GMS_WRITER_PROGRESS ready 412\n'), 'ready@412');
    assert.equal(gmsWriterProgress('GMS_WRITER_PROGRESS ready 41\nGMS_WRITER_PROGRESS input 44 11532\nGMS_WRITER_PROGRESS directory 45\nGMS_WRITER_PROGRESS staged 47\n'),
      'ready@41,input@44:11532,directory@45,staged@47');
    assert.equal(gmsWriterProgress('GMS_WRITER_PROGRESS ready 41\nGMS persistence request digest is inconsistent\n'), 'ready@41');
    for (const forged of ['GMS_WRITER_PROGRESS ready /private/path', 'GMS_WRITER_PROGRESS renamed 50', ' GMS_WRITER_PROGRESS ready 41',
      'GMS_WRITER_PROGRESS ready 12345678', 'GMS_WRITER_PROGRESS input 44 1234567890', 'GMS_WRITER_PROGRESS ready 41 secret=value']) {
      assert.equal(gmsWriterProgress(`${forged}\n`), 'none', forged);
    }
    const repeated = 'GMS_WRITER_PROGRESS ready 1\n'.repeat(100);
    assert.equal(gmsWriterProgress(repeated), 'ready@1,ready@1,ready@1,ready@1');
  });
  test('event-loop stall window crosses the execution deadline and leaves a cleanup-derived drain allowance', async () => {
    const command = { commandStartedMs: 1_000, timeoutMs: 700, cleanupMs: 200 };
    for (const markerAfterMs of [89, 139, 164, 499]) {
      const admitted = eventLoopStallWindow({ ...command, markerMs: 1_000 + markerAfterMs });
      assert.deepEqual(admitted, { executionDeadlineMs: 1_500, hardDeadlineMs: 1_700, stallUntilMs: 1_550, drainAllowanceMs: 150 }, String(markerAfterMs));
      assert.ok(admitted.stallUntilMs! > admitted.executionDeadlineMs! && admitted.drainAllowanceMs! >= command.cleanupMs / 2);
    }
    const recordedMarkerAfterMs = 164;
    assert.ok(recordedMarkerAfterMs + 550 > command.timeoutMs, 'the former fixed 550 ms stall after the recorded 164 ms marker ends after the hard deadline');
    assert.deepEqual(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_550 }),
      { executionDeadlineMs: 1_500, hardDeadlineMs: 1_700, stallUntilMs: 1_550, drainAllowanceMs: 150 });
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_600 }).violation, undefined);
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_600.5 }).violation,
      'stall ended 600.5 ms after the command started, leaving less than half of the 200 ms cleanup reservation to drain before 700 ms');
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_549 }).violation,
      'stall ended 549 ms after the command started, before crossing to 550 ms');
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_500 }).violation,
      'child marker arrived 500 ms after the command started, at or after its 500 ms execution deadline; the stall cannot cross it');
    assert.match(eventLoopStallWindow({ ...command, markerMs: 1_650, stallEndedMs: 1_660 }).violation ?? '', /at or after its 500 ms execution deadline/u);
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 999 }).violation, 'child marker precedes the command start');
    for (const cleanupMs of [0, -1, 700, 701]) {
      assert.match(eventLoopStallWindow({ ...command, cleanupMs, markerMs: 1_100 }).violation ?? '', /must be positive and shorter than/u, String(cleanupMs));
    }
    for (const invalid of [{ markerMs: Number.NaN }, { markerMs: undefined }, { markerMs: '1100' }, { markerMs: 1_100, stallEndedMs: Number.POSITIVE_INFINITY },
      { markerMs: 1_100, stallEndedMs: undefined }, { markerMs: 1_100, timeoutMs: '700' }]) {
      const result = eventLoopStallWindow({ ...command, ...invalid });
      assert.equal(result.violation, 'stall window inputs must be finite numbers', JSON.stringify(invalid));
      assert.equal(result.stallUntilMs, undefined);
    }
  });
  test('timed-out GMS child bytes remain in the aggregate allowance after an event-loop stall', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v24.21.0');
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const sourcePath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const source = await readFile(sourcePath, 'utf8');
    const padded = source.replace('Compiler stats:', `${' '.repeat(1_200_000)}\nCompiler stats:`);
    await writeFile(sourcePath, padded);
    const markerPath = join(fixture.root, 'stall-child-ready');
    const latePath = join(fixture.root, 'stall-child-late');
    const childCode = `(async()=>{process.on('SIGTERM',()=>{});const fs=require('node:fs');const chunk=Buffer.alloc(1800000,65);await Promise.all([new Promise(resolve=>process.stdout.write(chunk,resolve)),new Promise(resolve=>process.stderr.write(chunk,resolve))]);fs.writeFileSync(${JSON.stringify(markerPath)},'ready');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1000);setInterval(()=>{},1000)})()`;
    const maxBytes: number[] = [];
    const observerPath = join(fixture.root, 'stall-owned-process-observer.jsonl');
    const observerFD = openSync(observerPath, 'w');
    const observerSupervisor = createWriteStream('', { fd: observerFD, autoClose: false });
    let dispatches = 0;
    let stallWindow: Record<string, unknown> | undefined;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      maxBytes.push(options.maxBytes);
      if (dispatches !== 1) return boundedCommand(binary, args, timeout, options);
      const commandStartedMs = performance.now();
      const command = boundedCommand('node', ['-e', childCode], 700, {
        ...options, cleanupReservationMs: 200, observerSupervisor,
      });
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(markerPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      const markerMs = performance.now();
      assert.ok(existsSync(markerPath), 'child emitted bytes before watchdog');
      const admitted = eventLoopStallWindow({ commandStartedMs, timeoutMs: 700, cleanupMs: 200, markerMs });
      stallWindow = { ...admitted, markerAfterMs: markerMs - commandStartedMs };
      if (admitted.violation) return command;
      let stallIterations = 0;
      while (performance.now() < admitted.stallUntilMs!) stallIterations++;
      const stallEndedMs = performance.now();
      assert.ok(stallIterations > 0);
      const completed = eventLoopStallWindow({ commandStartedMs, timeoutMs: 700, cleanupMs: 200, markerMs, stallEndedMs });
      stallWindow = { ...completed, markerAfterMs: markerMs - commandStartedMs, stallEndedAfterMs: stallEndedMs - commandStartedMs };
      return command;
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.finish();
      assert.ok(stallWindow, 'the first GMS command was not stalled');
      assert.equal(stallWindow.violation, undefined, JSON.stringify(stallWindow));
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const first = observation.stages[0];
      assert.equal(first.timedOut, true);
      assert.equal(first.outcome, 'failed');
      assert.ok(first.stdoutBytes + first.stderrBytes >= 3_500_000, JSON.stringify({ stallWindow, first }));
      const ownedRecords = observerRecords(observerPath);
      const admitted = ownedRecords.find(record => record.type === 'supervisor-stopping' && record.reason === 'timeout');
      assert.ok(admitted?.targetSpawned === true && admitted.targetExecConfirmed === true,
        JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.childExited, true, JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.ownedProcessesExited, true, JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.stdioClosed, true, JSON.stringify({ ownedRecords, result: first }));
      assert.ok(ownedRecords.some(record => record.type === 'supervisor-retirement-ready'), JSON.stringify({ ownedRecords, result: first }));
      assert.equal(observation.totalCommandOutputBytes, observation.stages.reduce((sum: number, stage: any) => sum + (stage.stdoutBytes || 0) + (stage.stderrBytes || 0), 0));
      const priorBytes = observation.stages.slice(0, -1).reduce((sum: number, stage: any) => sum + (stage.stdoutBytes || 0) + (stage.stderrBytes || 0), 0);
      assert.equal(maxBytes.length, stages.length);
      assert.equal(maxBytes.at(-1), observation.limits.totalCommandOutputBytes - priorBytes);
      assert.ok(maxBytes.at(-1)! < 4_000_000, 'the fifth stage receives only the remaining aggregate allowance');
      await new Promise(resolve => setTimeout(resolve, 400));
      assert.equal(existsSync(latePath), false);
    } finally {
      await measurement.finish().catch(() => undefined);
      observerSupervisor.end();
      closeSync(observerFD);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS finalization aborts and retires the owned ownership-check process before command dispatch', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const supervisorObserverPath = join(fixture.root, 'ownership-target-supervisor.jsonl');
    const lateWritePath = join(fixture.root, 'ownership-target-late-write');
    const supervisorObserverFD = openSync(supervisorObserverPath, 'w');
    const observerSupervisor = createWriteStream('', { fd: supervisorObserverFD, autoClose: false });
    const observerServer = createNetServer();
    let resolveObserverWriter!: (socket: Socket) => void;
    const observerWriterReady = new Promise<Socket>(resolve => { resolveObserverWriter = resolve; });
    observerServer.on('connection', resolveObserverWriter);
    let observerWriter: Socket & { fd: number } | undefined;
    let observerReader: Socket | undefined;
    let resolveOwnedOutput!: () => void;
    let rejectOwnedOutput!: (error: Error) => void;
    let stageStartedAt: number;
    let ownedOutputObservedAt: number | undefined;
    let observedTargetOutput = '';
    const ownedOutput = new Promise<void>((resolve, reject) => {
      resolveOwnedOutput = resolve;
      rejectOwnedOutput = reject;
    });
    void ownedOutput.catch(() => undefined);
    let dispatches = 0;
    let ownershipFailure: BoundedCommandError | undefined;
    const ownerCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});process.stdout.write('OWNED\\n');setTimeout(()=>fs.writeFileSync(${JSON.stringify(lateWritePath)},'late'),1500);setInterval(()=>{},1000)`;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--gms-diagnostic-ownership')) {
          try { return await boundedCommand('node', ['-e', ownerCode], timeout, { ...options, observerStdout: observerWriter, observerSupervisor }); }
          catch (error) { if (error instanceof BoundedCommandError) ownershipFailure = error; throw error; }
        }
        if (args.includes('dumpsys')) dispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    let stage: Promise<void> | undefined;
    try {
      await new Promise<void>((resolve, reject) => {
        observerServer.once('error', reject);
        observerServer.listen(0, '127.0.0.1', resolve);
      });
      const address = observerServer.address();
      assert.ok(address && typeof address !== 'string');
      observerReader = createNetConnection(address.port, '127.0.0.1');
      const readerConnected = new Promise<void>((resolve, reject) => {
        observerReader!.once('connect', resolve);
        observerReader!.once('error', reject);
      });
      observerReader.on('data', chunk => {
        observedTargetOutput += Buffer.from(chunk).toString('utf8');
        if (observedTargetOutput === 'OWNED\n') {
          ownedOutputObservedAt ??= performance.now();
          resolveOwnedOutput();
        } else if (!'OWNED\n'.startsWith(observedTargetOutput)) {
          rejectOwnedOutput(new Error('ownership target emitted unexpected stdout before admission'));
        }
      });
      observerReader.once('error', rejectOwnedOutput);
      observerWriter = await observerWriterReady as Socket & { fd: number };
      const observerWriterDescriptor = (observerWriter as unknown as { _handle?: { fd?: number } })._handle?.fd;
      assert.ok(Number.isSafeInteger(observerWriterDescriptor) && observerWriterDescriptor! > 0);
      observerWriter.fd = observerWriterDescriptor!;
      await readerConnected;
      measurement.requireGmsObservations();
      stageStartedAt = performance.now();
      stage = measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-owner-retirement', { timeoutMs: 10_000 }));
      const admission = await Promise.race([
        ownedOutput.then(() => 'target-output' as const),
        stage.then(() => 'stage-settled' as const),
      ]);
      if (admission !== 'target-output') {
        await measurement.finalizeGmsObservations();
        await stage;
        const failedObservation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        assert.fail(`ownership target did not emit its exact OWNED byte before stage settlement: ${JSON.stringify({
          stage: failedObservation.stages[0], ownershipFailure: ownershipFailure?.result,
        })}`);
      }
      assert.equal(observedTargetOutput, 'OWNED\n');
      await measurement.finalizeGmsObservations();
      await stage;
      const observationPath = join(fixture.root, 'android-environment-gms-observations.json');
      const observationBytes = await readFile(observationPath);
      const observation = JSON.parse(observationBytes.toString('utf8'));
      assert.equal(dispatches, 0);
      assert.equal(observation.stages[0].outcome, 'failed');
      assert.match(observation.stages[0].error, /interrupted by finalization/u);
      assert.ok(observation.stages[0].durationMs > 0);
      assert.ok(ownedOutputObservedAt !== undefined);
      assert.ok(ownedOutputObservedAt - stageStartedAt < observation.limits.ownershipTimeoutMs);
      assert.ok(observation.stages[0].ownershipDurationMs <= observation.limits.ownershipTimeoutMs);
      assert.equal(observation.totalCollectionMs, observation.stages[0].durationMs);
      assert.ok(ownershipFailure instanceof BoundedCommandError);
      const ownedRecords = observerRecords(supervisorObserverPath);
      const stopping = ownedRecords.find(record => record.type === 'supervisor-stopping' && record.reason === 'aborted');
      const retirement = ownedRecords.find(record => record.type === 'supervisor-retirement-ready');
      assert.ok(stopping && retirement, JSON.stringify({ ownedRecords, result: ownershipFailure.result }));
      assert.equal(stopping.targetSpawned, true);
      assert.equal(stopping.targetExecConfirmed, true);
      const retirementEvidence = retirement.evidence as Record<string, unknown>;
      for (const field of ['targetExitObserved', 'targetCloseObserved', 'anchorExitObserved', 'anchorCloseObserved', 'groupAbsent', 'inputClosedObserved',
        'stdoutNaturalEnd', 'stdoutCloseObserved', 'stderrNaturalEnd', 'stderrCloseObserved']) {
        assert.equal(retirementEvidence[field], true, JSON.stringify({ ownedRecords, result: ownershipFailure.result }));
      }
      assert.equal(retirementEvidence.stopReason, 'aborted');
      assert.equal(stopping.reason, 'aborted');
      assert.equal(ownershipFailure.result.aborted, true);
      assert.equal(ownershipFailure.result.timedOut, false);
      assert.equal(ownershipFailure.result.targetCloseObserved, true);
      assert.equal(ownershipFailure.result.childExited, true, JSON.stringify({ ownedRecords, result: ownershipFailure.result }));
      assert.equal(ownershipFailure.result.ownedProcessesExited, true, JSON.stringify({ ownedRecords, result: ownershipFailure.result }));
      assert.equal(ownershipFailure.result.stdioClosed, true, JSON.stringify({ ownedRecords, result: ownershipFailure.result }));
      assert.equal(observedTargetOutput, 'OWNED\n');
      assert.ok(observation.errors.some((error: string) => /interrupted by finalization/u.test(error)));
      assert.ok(measurement.failureOutcome().errors.length > 0);
      assert.equal(observation.totalCommandOutputBytes, 0);
      assert.equal(observation.totalRetainedCommandOutputBytes, 0);
      assert.equal(dispatches, 0);
      await new Promise(resolve => setTimeout(resolve, 1_600));
      assert.equal(existsSync(lateWritePath), false);
      assert.deepEqual(await readFile(observationPath), observationBytes);
    } finally {
      const finishObserver = async () => {
        try {
          if (!observerSupervisor.writableFinished) {
            const finished = new Promise<void>((resolve, reject) => {
              observerSupervisor.once('finish', resolve);
              observerSupervisor.once('error', reject);
            });
            observerSupervisor.end();
            await finished;
          }
        } finally {
          closeSync(supervisorObserverFD);
        }
      };
      const closeSocket = async (socket: Socket | undefined) => {
        if (!socket || socket.closed) return;
        const closed = new Promise<void>(resolve => socket.once('close', resolve));
        socket.destroy();
        await closed;
      };
      try {
        await stage?.catch(() => undefined);
        await measurement.finish().catch(() => undefined);
      } finally {
        try {
          await Promise.all([closeSocket(observerWriter), closeSocket(observerReader)]);
        } finally {
          try {
            if (observerServer.listening) await new Promise<void>((resolve, reject) => {
              observerServer.close(error => error ? reject(error) : resolve());
            });
          } finally {
            try { await finishObserver(); }
            finally {
              for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
              Object.assign(process.env, saved);
            }
          }
        }
      }
    }
  });
  test('GMS ownership success returned after its monotonic deadline is rejected before command dispatch', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    let dispatches = 0;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--gms-diagnostic-ownership')) {
          const result = await boundedCommand(binary, args, timeout, options);
          const deadline = performance.now() + 550;
          let now = performance.now();
          while (now < deadline) now = performance.now();
          return result;
        }
        if (args.includes('dumpsys')) dispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-late-ownership', { timeoutMs: 10_000 }));
      await measurement.finalizeGmsObservations();
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const stage = observation.stages[0];
      assert.equal(stage.outcome, 'failed');
      assert.match(stage.error, /exceeded its monotonic time bound/u);
      assert.ok(stage.ownershipDurationMs > observation.limits.ownershipTimeoutMs);
      assert.equal(stage.ownershipChildExited, true);
      assert.equal(stage.ownershipProcessesExited, true);
      assert.equal(stage.ownershipStdioClosed, true);
      assert.equal(dispatches, 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS finalization records interrupted command bytes and monotonic stage duration', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const markerPath = join(fixture.root, 'interrupted-gms-command-ready');
    let entered!: () => void;
    const commandEntered = new Promise<void>(resolve => { entered = resolve; });
    let dispatches = 0;
    const childCode = `const fs=require('node:fs');process.stdout.write(Buffer.alloc(65536,65));fs.writeFileSync(${JSON.stringify(markerPath)},'ready');setInterval(()=>{},1000)`;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      entered();
      return boundedCommand('node', ['-e', childCode], timeout, options);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    let stage: Promise<void> | undefined;
    try {
      measurement.requireGmsObservations();
      stage = measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-finalize-interruption', { timeoutMs: 20_000 }));
      await commandEntered;
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(markerPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      assert.ok(existsSync(markerPath));
      const finalizeStarted = performance.now();
      await measurement.finalizeGmsObservations();
      assert.ok(performance.now() - finalizeStarted < 1_000);
      await stage;
      const path = join(fixture.root, 'android-environment-gms-observations.json');
      const bytes = await readFile(path);
      const observation = JSON.parse(bytes.toString('utf8'));
      const entry = observation.stages[0];
      assert.equal(dispatches, 1);
      assert.equal(entry.outcome, 'failed');
      assert.match(entry.error, /interrupted by finalization/u);
      assert.ok(entry.stdoutBytes > 0);
      assert.equal(entry.childExited, true);
      assert.equal(entry.ownedProcessesExited, true);
      assert.equal(entry.stdioClosed, true);
      assert.ok(entry.durationMs > 0);
      assert.ok(Date.parse(entry.endedAt) > Date.parse(entry.startedAt));
      assert.ok(entry.parseDurationMs === undefined);
      assert.equal(observation.totalCollectionMs, entry.durationMs);
      await new Promise(resolve => setTimeout(resolve, 100));
      assert.deepEqual(await readFile(path), bytes);
    } finally {
      await stage?.catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS names altered by redaction are withheld without a collected success', async () => {
    const fixture = await harness.createFixture();
    const dumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const source = await readFile(dumpPath, 'utf8');
    const secretLikeName = `com.google.android.gms.${'A'.repeat(43)}`;
    await writeFile(dumpPath, source.replace('Queries:', `      enabledComponents:\n        ${secretLikeName}\nQueries:`));
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      measurement.requireGmsObservations();
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-redaction', { timeoutMs: 10_000 }));
      await measurement.finalizeGmsObservations();
      const bytes = await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8');
      const observations = JSON.parse(bytes);
      assert.equal(observations.stages[0].outcome, 'failed');
      assert.equal(observations.stages[0].state, undefined);
      assert.match(observations.stages[0].error, /cannot be retained canonically/u);
      assert.doesNotMatch(bytes, /A{43}/u);
      assert.ok(measurement.failureOutcome().errors.length > 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('diagnostic GMS attack timing bounds normalization and collection by declared limits, not a composite literal', async () => {
    const limits = { ownershipTimeoutMs: 500, commandTimeoutMs: 5_000, parseReserveMs: 250, persistenceOperationTimeoutMs: 1_000 };
    const quietWindowStage = { durationMs: 681, parseDurationMs: 106 };
    const quietWindowElapsedMs = 1_054.7;
    assert.ok(quietWindowElapsedMs >= 1_000, 'the recorded quiet-window attack is rejected by the former 1000 ms composite literal');
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, quietWindowElapsedMs),
      { violations: [], stageAllowanceMs: 5_750, compositeCeilingMs: 7_750 });
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 5_750, parseDurationMs: 250 }, 7_750).violations, []);
    const slowParserElapsedMs = 900;
    assert.ok(slowParserElapsedMs < 1_000, 'the former composite literal accepts this slow normalization');
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 600, parseDurationMs: 251 }, slowParserElapsedMs).violations,
      ['normalization took 251 ms, beyond its 250 ms parse reserve']);
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 5_751, parseDurationMs: 100 }, 6_000).violations,
      ['collection took 5751 ms, beyond its 5750 ms stage allowance']);
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, 7_750.5).violations,
      ['collection and both persistence operations took 7750.5 ms, beyond 7750 ms']);
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, 680).violations,
      ['elapsed 680 ms is shorter than the 681 ms collection it contains']);
    const invalidMeasurements: Array<[unknown, unknown]> = [
      [{ durationMs: 681 }, 1_000], [{ durationMs: 681, parseDurationMs: 0 }, 1_000],
      [{ durationMs: 681, parseDurationMs: Number.NaN }, 1_000], [{ parseDurationMs: 106 }, 1_000],
      [{ durationMs: -1, parseDurationMs: 106 }, 1_000], [{ durationMs: '681', parseDurationMs: 106 }, 1_000],
      [quietWindowStage, Number.POSITIVE_INFINITY], [quietWindowStage, undefined], [quietWindowStage, 0], [undefined, 1_000],
    ];
    for (const [stage, elapsedMs] of invalidMeasurements) {
      const result = gmsAttackTimingRule(limits, stage, elapsedMs);
      assert.ok(result.violations.length > 0, JSON.stringify({ stage, elapsedMs }));
      assert.equal(result.compositeCeilingMs, undefined, JSON.stringify({ stage, elapsedMs }));
    }
    const invalidLimits: unknown[] = [undefined, {}, { ...limits, parseReserveMs: 0 }, { ...limits, commandTimeoutMs: 5_000.5 },
      { ...limits, ownershipTimeoutMs: -500 }, { ...limits, persistenceOperationTimeoutMs: '1000' }];
    for (const declared of invalidLimits) {
      const result = gmsAttackTimingRule(declared, quietWindowStage, quietWindowElapsedMs);
      assert.ok(result.violations.length > 0, JSON.stringify(declared));
      assert.equal(result.stageAllowanceMs, undefined, JSON.stringify(declared));
    }
  });
  test('diagnostic GMS errors are bounded, explicit and fail closed at the collection layer', async () => {
    const fixture = await harness.createFixture();
    const dumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const originalDump = await readFile(dumpPath, 'utf8');
    assert.throws(() => parseAndroidGmsComponentState(`${originalDump}${' '.repeat(2_000_000)}`), /exceeds parsing limit/u);
    const continuationCount = 65_537 - originalDump.split('\n').length;
    const continuationAttack = originalDump.replace('Queries:', `${' optional:true\n'.repeat(continuationCount)}Queries:`);
    assert.ok(Buffer.byteLength(continuationAttack) < 2_000_000);
    const parseStarted = performance.now();
    assert.throws(() => parseAndroidGmsComponentState(continuationAttack), /exceeds line limit/u);
    assert.ok(performance.now() - parseStarted < 1_000);
    const attackFixture = await harness.createFixture();
    const attackEnvironment = { ...process.env };
    Object.assign(process.env, attackFixture.environment);
    const compactAttack = originalDump.replace('Queries:', `${' optional:true\n'.repeat(512)}${'\n'.repeat(continuationCount - 512)}Queries:`);
    await writeFile(join(attackFixture.fixtureDirectory, 'valid-gms.dump'), compactAttack);
    let attackCommandFailure: BoundedCommandError | undefined;
    let attackParserFailure: BoundedCommandError | undefined;
    const failureMetadata = (failure: BoundedCommandError | undefined) => {
      if (!failure) return undefined;
      return Object.fromEntries(Object.entries(failure.result).filter(([key]) => key !== 'stdout' && key !== 'stderr'));
    };
    const attackMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', attackFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        try { return await boundedCommand(binary, args, timeout, options); }
        catch (error) {
          if (error instanceof BoundedCommandError) {
            if (args.includes('dumpsys')) attackCommandFailure = error;
            if (args.includes('--eval')) attackParserFailure = error;
          }
          throw error;
        }
      },
    });
    try {
      attackMeasurement.requireGmsObservations();
      const attackStarted = performance.now();
      await attackMeasurement.observeGmsStage('after-device-start', new PhaseBudget('gms-parser-bound', { timeoutMs: 10_000 }));
      await attackMeasurement.finalizeGmsObservations();
      const attackElapsedMs = performance.now() - attackStarted;
      const attackResult = JSON.parse(await readFile(join(attackFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.equal(attackResult.stages[0].outcome, 'failed');
      assert.match(attackResult.stages[0].error, /exceeds line limit/u,
        JSON.stringify({ stage: attackResult.stages[0], commandResult: failureMetadata(attackCommandFailure) }));
      assert.ok(attackResult.stages[0].parseDurationMs > 0);
      assert.deepEqual([attackResult.limits.ownershipTimeoutMs, attackResult.limits.commandTimeoutMs, attackResult.limits.parseReserveMs,
        attackResult.limits.persistenceOperationTimeoutMs, attackResult.limits.totalCollectionMs], [500, 5_000, 250, 1_000, 5 * 5_750]);
      const attackTiming = gmsAttackTimingRule(attackResult.limits, attackResult.stages[0], attackElapsedMs);
      assert.deepEqual(attackTiming.violations, [], JSON.stringify({ attackElapsedMs, stage: attackResult.stages[0],
        commandResult: failureMetadata(attackCommandFailure), parserResult: failureMetadata(attackParserFailure) }));
      assert.equal(attackTiming.stageAllowanceMs, 5_750);
      assert.equal(attackTiming.compositeCeilingMs, 7_750);
      assert.equal(attackResult.totalCollectionMs, attackResult.stages[0].durationMs);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in attackEnvironment)) delete process.env[key];
      Object.assign(process.env, attackEnvironment);
    }
    const cases = ['malformed', 'timeout', 'wrong-owner', 'stage-order'] as const;
    for (const failure of cases) {
      const current = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, current.environment);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', current.root, repositoryPath('tests/mobile/toolchains.json'));
      const budget = new PhaseBudget(`gms-${failure}`, { timeoutMs: failure === 'timeout' ? 1_500 : 5_000 });
      try {
        measurement.requireGmsObservations();
        if (failure === 'malformed') await writeFile(join(current.fixtureDirectory, 'valid-gms.dump'), originalDump.slice(0, -10));
        if (failure === 'timeout') await writeFile(join(current.fixtureDirectory, 'valid-vending.json'), JSON.stringify({ gmsTimeout: true }));
        if (failure === 'wrong-owner') await writeFile(current.environment.MOBILE_DEVICE_OWNERSHIP_FILE!, 'android:other-device');
        if (failure === 'stage-order') await measurement.observeGmsStage('after-setup-url', budget);
        else await measurement.observeGmsStage('after-device-start', budget);
        await measurement.finalizeGmsObservations();
        const observation = JSON.parse(await readFile(join(current.root, 'android-environment-gms-observations.json'), 'utf8'));
        assert.equal(observation.stages[0].outcome, 'failed', failure);
        assert.ok(observation.errors.length > 0, failure);
        assert.ok(measurement.failureOutcome().errors.length > 0, failure);
        if (failure === 'wrong-owner') {
          const adbLog = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
          assert.doesNotMatch(adbLog, /shell dumpsys package com\.google\.android\.gms/u);
        }
        if (failure === 'timeout') {
          assert.ok(observation.stages[0].timeoutMs <= 500);
          assert.ok(observation.stages[0].timedOut);
        }
        if (failure === 'stage-order') assert.match(observation.stages[0].error, /stage order mismatch/u);
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('post-baseline re-enable fails AFTER acquisition without any repair', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 3 });
    const result = prepare(fixture);
    assert.equal(result.passed, true, result.stderr);
    const before = join(fixture.root, 'before.json');
    assert.equal((await harness.snapshot(fixture, 'valid', before, join(fixture.root, 'before-diagnostics.json'))).passed, true);
    await state(fixture, { enabled: 1 });
    const after = join(fixture.root, 'after.json');
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, false);
    assert.equal(existsSync(after), false);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    const check = await harness.check(fixture, before, after);
    assert.equal(check.passed, false);
    assert.match(check.issues.join(';'), /input unavailable|snapshot invalid/u);
  });
  test('SM59 report and qualification preserve the immutable assessment and current row binding', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const snapshot = JSON.parse(await readFile(after, 'utf8')) as AndroidEnvironmentSnapshot;
    snapshot.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['com.google.android.gms.fixture.ComponentChanged'];
    rehashPackage(snapshot.packages['com.google.android.gms']);
    await writeFile(after, JSON.stringify(snapshot));
    const log = join(fixture.root, 'report.log'), operations = join(fixture.root, 'report-operations.json'), output = join(fixture.root, 'report-check.json');
    await writeFile(log, marker('00.000', 'START') + marker('30.000', 'END')); await writeFile(operations, '[]');
    const report = checkCLI(fixture, { before, after, log, operations, output }, 'report');
    assert.equal(report.passed, true, report.stderr);
    const strict = checkCLI(fixture, { before, after, log, operations, output }, 'qualification');
    assert.equal(strict.passed, false);
    const checkPath = join(report.directory, 'android-environment-check.json');
    const original = await readFile(checkPath, 'utf8');
    const alteredArgs = [...report.args]; alteredArgs[alteredArgs.indexOf('--attempt') + 1] = '1';
    assert.equal(cli(fixture, alteredArgs).passed, false);
    assert.equal(await readFile(checkPath, 'utf8'), original);
    for (const field of ['status', 'schema', 'counts']) {
      const changed = JSON.parse(original);
      if (field === 'status') changed.status = 'PASS';
      if (field === 'schema') changed.schema = 2;
      if (field === 'counts') changed.eventCounts.rawEvents++;
      const bytes = JSON.stringify(changed); await writeFile(checkPath, bytes);
      assert.equal(cli(fixture, report.args).passed, false, field);
      assert.equal(await readFile(checkPath, 'utf8'), bytes);
    }
  });
  test('Android-only declarations and both setup paths preserve policy, pinning and read-only postchecks', async () => {
    const policy = JSON.parse(await readFile(repositoryPath('tests/mobile/toolchains.json'), 'utf8')).android;
    assert.equal(policy.vendingPolicy, 'absent-or-disabled-user-0');
    assert.equal(policy.systemImagePolicy, 'owned-google-apis-emulator');
    assert.equal('playStore' in policy, false);
    assert.equal(policy.browserSha256, '261439a1ed20090f9f2f9aeef64024c1cfb9f242d4770f8f7c8f2e777843a35a');
    assert.equal(policy.trichromeLibrarySha256, 'f7d82fa76a99f13980c4205484f4ae78742755d2e9b7276fd60c20e3cd7f9090');
    for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const source = await readFile(repositoryPath(path), 'utf8');
      assert.match(source, /android-environment\.ts prepare/u);
      assert.doesNotMatch(source, /android-environment\.ts snapshot|logcat -c|google-apis-without-play-store/u);
      const assertPostcheck = (text: string) => {
        const start = text.indexOf('name: Validate immutable Android environment report');
        const end = text.indexOf('name: Sanitize bounded diagnostics');
        assert.ok(start >= 0 && end > start);
        const post = text.slice(start, end);
        assert.match(post, /android-environment\.ts check --contract report/u);
        assert.match(post, /--directory "\$MOBILE_OUTPUT"/u);
        assert.match(post, /--identity "\$MOBILE_OUTPUT\/android-environment-identity\.json"/u);
        assert.doesNotMatch(post, /--output|--operations|disable-user|adb |prepare|>.*android-environment-check/u);
      };
      assertPostcheck(source);
      for (const binding of ['--directory "$MOBILE_OUTPUT"', '--identity "$MOBILE_OUTPUT/android-environment-identity.json"']) assert.throws(() => assertPostcheck(source.replace(binding, '--removed binding')));
      assert.throws(() => assertPostcheck(source.replace('check --contract report', 'check --contract report --output overwrite.json')));
      assert.throws(() => assertPostcheck(source.replace('name: Validate immutable Android environment report', 'name: Removed')));
      assert.match(source, /apksigner verify --verbose --print-certs/u);
      assert.match(source, /sha256sum --check --status/u);

    }
  });
  return tests;
}

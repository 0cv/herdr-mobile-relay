import { createHash, type Hash } from 'node:crypto';
import { redactText } from './diagnostics';
import { PhaseBudget } from './budget';
import { spawnOwnedProcess, verifyOwnedRetirement, type OwnedProcessHandle } from './owned-process';
import type { Writable } from 'node:stream';

export interface BoundedCommandResult {
  stdout: string;
  stderr: string;
  code: number;
  durationMs: number;
  timedOut: boolean;
  outputLimitExceeded: boolean;
  aborted: boolean;
  childExited: boolean;
  targetPID?: number;
  targetCloseObserved: boolean;
  targetStdoutNaturalEnd?: boolean;
  targetStdoutCloseObserved?: boolean;
  targetStderrNaturalEnd?: boolean;
  targetStderrCloseObserved?: boolean;
  managerStdoutNaturalEnd?: boolean;
  managerStdoutCloseObserved?: boolean;
  managerStderrNaturalEnd?: boolean;
  managerStderrCloseObserved?: boolean;
  callerStdoutNaturalEnd?: boolean;
  callerStdoutCloseObserved?: boolean;
  callerStderrNaturalEnd?: boolean;
  callerStderrCloseObserved?: boolean;
  targetOutputRelayHealthy?: boolean;
  ownedProcessesExited: boolean;
  stdioClosed: boolean;
  signal?: string;
  launchError?: string;
  stdoutBytes: number;
  stderrBytes: number;
  stdoutRetainedBytes: number;
  stderrRetainedBytes: number;
  stdoutSha256: string;
  stderrSha256: string;
}

interface BoundedCommandOptions {
  budget?: PhaseBudget;
  label?: string;
  maxBytes: number;
  maxBytesPerStream?: number;
  cleanupReservationMs?: number;
  signal?: AbortSignal;
  input?: Buffer | string;
  observerStdout?: Writable;
  observerStderr?: Writable;
  observerSupervisor?: Writable;
}

export class BoundedCommandError extends Error {
  readonly result: BoundedCommandResult;

  constructor(binary: string, args: string[], result: BoundedCommandResult) {
    super(`BOUNDED_COMMAND_FAILED: ${redactText([binary, ...args].join(' ')).slice(0, 300)}`);
    this.name = 'BoundedCommandError';
    this.result = result;
  }
}

type StopReason = 'timeout' | 'output-limit' | 'aborted';
type RetirementEvidence = Awaited<OwnedProcessHandle['retirement']>;

interface BoundedState {
  binary: string;
  args: string[];
  options: BoundedCommandOptions;
  handle: OwnedProcessHandle;
  startedAt: number;
  allowedMs: number;
  cleanupMs: number;
  maxBytesPerStream: number;
  output: { stdout: Buffer[]; stderr: Buffer[] };
  observed: { stdout: number; stderr: number };
  retained: { stdout: number; stderr: number };
  hashes: { stdout: Hash; stderr: Hash };
  code: number;
  signal: string | undefined;
  launchError: string | undefined;
  stopReason: StopReason | undefined;
  targetExited: boolean;
  targetClosed: boolean;
  stdoutEnded: boolean;
  stderrEnded: boolean;
  stdoutClosed: boolean;
  stderrClosed: boolean;
  stdinClosed: boolean;
  settled: boolean;
  ownedFailure: Error | undefined;
  timeoutTimer: ReturnType<typeof setTimeout> | undefined;
  cleanupTask: Promise<void> | undefined;
  detachOutput: () => void;
  abortListener: () => void;
  resolveCommand: (value: BoundedCommandResult) => void;
  rejectCommand: (error: Error) => void;
}

export function boundedCommand(
  binary: string,
  args: string[],
  timeoutMs: number,
  options: BoundedCommandOptions,
): Promise<BoundedCommandResult> {
  options.budget?.assertAvailable(options.label || binary);
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1
    || !Number.isSafeInteger(options.maxBytes) || options.maxBytes < 1
    || (options.maxBytesPerStream !== undefined && (!Number.isSafeInteger(options.maxBytesPerStream) || options.maxBytesPerStream < 1))) {
    throw new Error('BOUNDED_COMMAND: invalid time or output limit');
  }
  if (options.signal?.aborted) throw new Error('BOUNDED_COMMAND: command was aborted before dispatch');

  const state = boundedSpawn(binary, args, timeoutMs, options);
  boundedCollectOutput(state);
  boundedWatchInput(state);
  boundedWatchTarget(state);
  state.timeoutTimer = setTimeout(() => boundedStop(state, 'timeout'), state.allowedMs - state.cleanupMs);
  state.abortListener = () => boundedStop(state, 'aborted');
  options.signal?.addEventListener('abort', state.abortListener, { once: true });
  if (options.signal?.aborted) boundedStop(state, 'aborted');

  return new Promise<BoundedCommandResult>((resolve, reject) => {
    state.resolveCommand = resolve;
    state.rejectCommand = reject;
    boundedDispatch(state);
  });
}

function boundedSpawn(binary: string, args: string[], timeoutMs: number, options: BoundedCommandOptions): BoundedState {
  const startedAt = performance.now();
  const startNs = process.hrtime.bigint();
  const allowedMs = Math.max(1, Math.floor(Math.min(timeoutMs, options.budget?.remainingMs ?? timeoutMs)));
  const cleanupMs = Math.min(options.cleanupReservationMs ?? 200, Math.max(0, Math.floor(allowedMs / 3)));
  const hardDeadlineNs = startNs + BigInt(allowedMs) * 1_000_000n;
  const handle = spawnOwnedProcess(binary, args, {
    hardDeadlineNs,
    cleanupReservationMs: cleanupMs,
    input: options.input,
    signal: options.signal,
    observerStdout: options.observerStdout,
    observerStderr: options.observerStderr,
    observerSupervisor: options.observerSupervisor,
  });
  return {
    binary,
    args,
    options,
    handle,
    startedAt,
    allowedMs,
    cleanupMs,
    maxBytesPerStream: options.maxBytesPerStream ?? options.maxBytes,
    output: { stdout: [], stderr: [] },
    observed: { stdout: 0, stderr: 0 },
    retained: { stdout: 0, stderr: 0 },
    hashes: { stdout: createHash('sha256'), stderr: createHash('sha256') },
    code: 1,
    signal: undefined,
    launchError: undefined,
    stopReason: undefined,
    targetExited: false,
    targetClosed: false,
    stdoutEnded: false,
    stderrEnded: false,
    stdoutClosed: false,
    stderrClosed: false,
    stdinClosed: false,
    settled: false,
    ownedFailure: undefined,
    timeoutTimer: undefined,
    cleanupTask: undefined,
    detachOutput: () => undefined,
    abortListener: () => undefined,
    resolveCommand: () => undefined,
    rejectCommand: () => undefined,
  };
}

function boundedDispatch(state: BoundedState) {
  const { handle, options } = state;
  handle.ready.then(() => {
    if (state.stopReason || options.signal?.aborted) {
      if (!state.stopReason) boundedStop(state, 'aborted');
      return;
    }
    handle.start();
  }, error => {
    state.ownedFailure ||= error instanceof Error ? error : new Error('owned supervisor failed to initialize');
    state.launchError ||= redactText(state.ownedFailure.message).slice(0, 500);
    const supervisorTimedOut = /^OWNED_PROCESS: dispatch stopped \((?:timeout|hard-deadline|startup-hard-deadline)\)$/u.test(state.ownedFailure.message);
    state.stopReason ||= options.signal?.aborted ? 'aborted' : supervisorTimedOut ? 'timeout' : 'aborted';
    void boundedFinish(state, true);
  });
  handle.supervisor.once('close', () => {
    if (!state.cleanupTask && !state.settled) {
      state.ownedFailure ||= new Error('owned supervisor closed before command retirement was confirmed');
      void boundedFinish(state, true);
    }
  });
  if (options.signal?.aborted) boundedStop(state, 'aborted');
}

function boundedNaturalStreamsClosed(state: BoundedState): boolean {
  return state.stdoutEnded && state.stderrEnded && state.stdoutClosed && state.stderrClosed && state.stdinClosed;
}

function boundedMaybeFinish(state: BoundedState) {
  if (state.settled || state.cleanupTask) return;
  if (!state.stopReason && state.targetClosed && boundedNaturalStreamsClosed(state)) void boundedFinish(state, false);
}

function boundedOnData(state: BoundedState, stream: 'stdout' | 'stderr', value: Buffer) {
  if (state.settled) return;
  const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
  state.observed[stream] += chunk.length;
  state.hashes[stream].update(chunk);
  if (state.stopReason) return;
  const combinedRetained = state.retained.stdout + state.retained.stderr;
  const remaining = Math.max(0, state.options.maxBytes - combinedRetained);
  const keep = Math.min(chunk.length, remaining);
  if (keep) {
    state.output[stream].push(chunk.subarray(0, keep));
    state.retained[stream] += keep;
  }
  if (state.observed.stdout + state.observed.stderr > state.options.maxBytes || state.observed[stream] > state.maxBytesPerStream) {
    boundedStop(state, 'output-limit');
    return;
  }
  if (performance.now() >= state.startedAt + state.allowedMs || state.options.budget?.exhausted) boundedStop(state, 'timeout');
}

function boundedCollectOutput(state: BoundedState) {
  const { handle } = state;
  const onStdout = (chunk: Buffer) => boundedOnData(state, 'stdout', chunk);
  const onStderr = (chunk: Buffer) => boundedOnData(state, 'stderr', chunk);
  const onStdoutEnd = () => { state.stdoutEnded = true; boundedMaybeFinish(state); };
  const onStderrEnd = () => { state.stderrEnded = true; boundedMaybeFinish(state); };
  const onStdoutClose = () => { state.stdoutClosed = true; boundedMaybeFinish(state); };
  const onStderrClose = () => { state.stderrClosed = true; boundedMaybeFinish(state); };
  state.detachOutput = () => {
    handle.stdout.removeListener('data', onStdout);
    handle.stderr.removeListener('data', onStderr);
    handle.stdout.removeListener('end', onStdoutEnd);
    handle.stderr.removeListener('end', onStderrEnd);
    handle.stdout.removeListener('close', onStdoutClose);
    handle.stderr.removeListener('close', onStderrClose);
  };
  handle.stdout.on('data', onStdout);
  handle.stderr.on('data', onStderr);
  handle.stdout.once('end', onStdoutEnd);
  handle.stderr.once('end', onStderrEnd);
  handle.stdout.once('close', onStdoutClose);
  handle.stderr.once('close', onStderrClose);
}

function boundedWatchInput(state: BoundedState) {
  const { handle } = state;
  handle.inputClosed.then(() => { state.stdinClosed = true; boundedMaybeFinish(state); });
  if (handle.stdin.closed) state.stdinClosed = true;
}

function boundedWatchTarget(state: BoundedState) {
  const { handle } = state;
  handle.targetExit.then(value => {
    if (!value) return;
    state.targetExited = Boolean(handle.binding && !value.launchError);
    state.code = value.code ?? 1;
    state.signal = value.signal || undefined;
    if (value.launchError) {
      state.launchError = redactText(value.launchError).slice(0, 500);
      handle.stop('target-launch-error');
      void boundedFinish(state, true);
      return;
    }
    boundedMaybeFinish(state);
  });
  handle.targetClose.then(value => {
    if (value && handle.binding && !value.launchError) {
      state.targetClosed = true;
      if (!state.targetExited) {
        state.targetExited = true;
        state.code = value.code ?? 1;
        state.signal = value.signal || undefined;
      }
    }
    boundedMaybeFinish(state);
  });
  handle.failure.then(error => {
    state.ownedFailure ||= error;
    if (!handle.binding && !state.launchError) state.launchError = redactText(error.message).slice(0, 500);
    if (handle.isSupervisorClosed) void boundedFinish(state, true);
  });
  handle.stopCause.then(reason => {
    if (reason === 'normal-retirement' || reason === 'target-launch-error' || state.stopReason || state.settled) return;
    if (reason === 'timeout' || reason === 'hard-deadline' || reason === 'startup-hard-deadline') {
      boundedStop(state, 'timeout');
      return;
    }
    state.ownedFailure ||= new Error(`owned process stopped before normal completion (${reason})`);
    boundedStop(state, 'aborted');
  });
}

function boundedProcessRetired(handle: OwnedProcessHandle, evidence?: RetirementEvidence): boolean {
  if (!evidence || !evidence.anchorExitObserved || !evidence.anchorCloseObserved || !evidence.groupAbsent
    || !evidence.inputClosedObserved || !evidence.supervisorExitObserved || !evidence.supervisorCloseObserved
    || evidence.supervisorExitCode !== 0 || evidence.supervisorSignal !== null
    || !evidence.supervisorStdoutFinished || !evidence.supervisorStdoutCloseObserved
    || !evidence.supervisorStderrFinished || !evidence.supervisorStderrCloseObserved
    || !evidence.callerStdoutNaturalEnd || !evidence.callerStdoutCloseObserved
    || !evidence.callerStderrNaturalEnd || !evidence.callerStderrCloseObserved
    || ((evidence.managerProcessCreated || evidence.targetDispatchRequested || evidence.targetProcessCreated
      || evidence.targetNoChildObserved) && (!evidence.managerStdoutNaturalEnd || !evidence.managerStdoutCloseObserved
      || !evidence.managerStderrNaturalEnd || !evidence.managerStderrCloseObserved))) return false;
  if (!evidence.targetDispatchRequested) {
    if (evidence.targetProcessCreated || evidence.targetNoChildObserved) return false;
  } else if (evidence.targetNoChildObserved) {
    if (evidence.targetProcessCreated || evidence.targetExecConfirmed) return false;
  } else if (!evidence.targetProcessCreated || !evidence.stdoutNaturalEnd || !evidence.stdoutCloseObserved
    || !evidence.stderrNaturalEnd || !evidence.stderrCloseObserved) return false;
  else if (evidence.targetPIDObserved === false) {
    if (evidence.targetExecConfirmed || !evidence.targetUnconfirmedCloseObserved
      || evidence.targetExitObserved || evidence.targetCloseObserved
      || evidence.targetStdoutNaturalEnd || evidence.targetStdoutCloseObserved
      || evidence.targetStderrNaturalEnd || evidence.targetStderrCloseObserved) return false;
  } else if (evidence.targetPIDObserved === true) {
    if (!evidence.targetExecConfirmed || !evidence.targetStdoutNaturalEnd || !evidence.targetStdoutCloseObserved
      || !evidence.targetStderrNaturalEnd || !evidence.targetStderrCloseObserved
      || !evidence.targetExitObserved || !evidence.targetCloseObserved || !evidence.targetOutputRelayHealthy) return false;
  } else return false;
  return !handle.binding || !processGroupExistsNow(handle);
}

function boundedResult(state: BoundedState, evidence?: RetirementEvidence): BoundedCommandResult {
  return {
    stdout: Buffer.concat(state.output.stdout).toString('utf8'),
    stderr: Buffer.concat(state.output.stderr).toString('utf8'),
    code: state.code,
    durationMs: Math.ceil(performance.now() - state.startedAt),
    timedOut: state.stopReason === 'timeout',
    outputLimitExceeded: state.stopReason === 'output-limit',
    aborted: state.stopReason === 'aborted',
    childExited: state.targetExited,
    targetPID: state.handle.targetPID,
    targetCloseObserved: state.targetClosed,
    targetStdoutNaturalEnd: evidence?.targetStdoutNaturalEnd ?? false,
    targetStdoutCloseObserved: evidence?.targetStdoutCloseObserved ?? false,
    targetStderrNaturalEnd: evidence?.targetStderrNaturalEnd ?? false,
    targetStderrCloseObserved: evidence?.targetStderrCloseObserved ?? false,
    managerStdoutNaturalEnd: evidence?.managerStdoutNaturalEnd ?? false,
    managerStdoutCloseObserved: evidence?.managerStdoutCloseObserved ?? false,
    managerStderrNaturalEnd: evidence?.managerStderrNaturalEnd ?? false,
    managerStderrCloseObserved: evidence?.managerStderrCloseObserved ?? false,
    callerStdoutNaturalEnd: evidence?.callerStdoutNaturalEnd ?? false,
    callerStdoutCloseObserved: evidence?.callerStdoutCloseObserved ?? false,
    callerStderrNaturalEnd: evidence?.callerStderrNaturalEnd ?? false,
    callerStderrCloseObserved: evidence?.callerStderrCloseObserved ?? false,
    targetOutputRelayHealthy: evidence?.targetOutputRelayHealthy ?? false,
    ownedProcessesExited: boundedProcessRetired(state.handle, evidence),
    stdioClosed: boundedNaturalStreamsClosed(state),
    signal: state.signal,
    launchError: state.launchError,
    stdoutBytes: state.observed.stdout,
    stderrBytes: state.observed.stderr,
    stdoutRetainedBytes: state.retained.stdout,
    stderrRetainedBytes: state.retained.stderr,
    stdoutSha256: state.hashes.stdout.copy().digest('hex'),
    stderrSha256: state.hashes.stderr.copy().digest('hex'),
  };
}

function boundedStop(state: BoundedState, reason: StopReason) {
  if (state.settled || state.stopReason) return;
  state.stopReason = reason;
  state.handle.stop(reason);
  void boundedFinish(state, true);
}

async function boundedRetire(state: BoundedState, forceFailure: boolean): Promise<RetirementEvidence | undefined> {
  const { handle } = state;
  let evidence: RetirementEvidence | undefined;
  if (forceFailure && !handle.isSupervisorClosed) {
    try { handle.release(); } catch (error) { state.ownedFailure ||= error instanceof Error ? error : new Error('owned supervisor release failed'); }
  }
  try {
    evidence = await verifyOwnedRetirement(handle);
  } catch (error) {
    state.ownedFailure ||= error instanceof Error ? error : new Error('owned process retirement failed');
  }
  if (!evidence?.groupAbsent || !evidence.anchorExitObserved) handle.release();
  return evidence;
}

function boundedFinish(state: BoundedState, forceFailure: boolean): Promise<void> {
  if (state.cleanupTask) return state.cleanupTask;
  state.cleanupTask = (async () => {
    const evidence = await boundedRetire(state, forceFailure);
    if (state.settled) return;
    state.settled = true;
    if (state.timeoutTimer) clearTimeout(state.timeoutTimer);
    state.timeoutTimer = undefined;
    state.options.signal?.removeEventListener('abort', state.abortListener);
    state.detachOutput();
    state.handle.targetExit.then(() => undefined);
    const value = boundedResult(state, evidence);
    if (forceFailure || value.timedOut || value.outputLimitExceeded || value.aborted || !value.ownedProcessesExited
      || !value.stdioClosed || !value.childExited || value.code !== 0 || value.launchError || state.ownedFailure) {
      state.rejectCommand(new BoundedCommandError(state.binary, state.args, value));
    } else state.resolveCommand(value);
  })();
  return state.cleanupTask;
}

function processGroupExistsNow(handle: OwnedProcessHandle): boolean {
  return handle.binding ? !requireGroupAbsent(handle.binding.groupID) : false;
}

function requireGroupAbsent(groupID: number): boolean {
  try { process.kill(-groupID, 0); return false; } catch (error) {
    return (error as NodeJS.ErrnoException).code === 'ESRCH';
  }
}

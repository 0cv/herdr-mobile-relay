import { createHash } from 'node:crypto';
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

export interface BoundedCommandOptions {
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

  const startedAt = performance.now();
  const startNs = process.hrtime.bigint();
  const allowedMs = Math.max(1, Math.floor(Math.min(timeoutMs, options.budget?.remainingMs ?? timeoutMs)));
  const cleanupMs = Math.min(options.cleanupReservationMs ?? 200, Math.max(0, Math.floor(allowedMs / 3)));
  const hardDeadlineNs = startNs + BigInt(allowedMs) * 1_000_000n;
  const maxBytesPerStream = options.maxBytesPerStream ?? options.maxBytes;
  const output = { stdout: [] as Buffer[], stderr: [] as Buffer[] };
  const observed = { stdout: 0, stderr: 0 };
  const retained = { stdout: 0, stderr: 0 };
  const hashes = { stdout: createHash('sha256'), stderr: createHash('sha256') };
  let code = 1;
  let signal: string | undefined;
  let launchError: string | undefined;
  let stopReason: 'timeout' | 'output-limit' | 'aborted' | undefined;
  let targetExited = false;
  let targetClosed = false;
  let stdoutEnded = false;
  let stderrEnded = false;
  let stdoutClosed = false;
  let stderrClosed = false;
  let stdinClosed = false;
  let settled = false;
  let ownedFailure: Error | undefined;
  let timeoutTimer: ReturnType<typeof setTimeout> | undefined;
  let cleanupTask: Promise<void> | undefined;
  const handle = spawnOwnedProcess(binary, args, {
    hardDeadlineNs,
    cleanupReservationMs: cleanupMs,
    input: options.input,
    signal: options.signal,
    observerStdout: options.observerStdout,
    observerStderr: options.observerStderr,
    observerSupervisor: options.observerSupervisor,
  });
  let resolveCommand!: (value: BoundedCommandResult) => void;
  let rejectCommand!: (error: Error) => void;

  const childExited = () => targetExited;
  const naturalStreamsClosed = () => stdoutEnded && stderrEnded && stdoutClosed && stderrClosed && stdinClosed;
  const processRetired = (evidence?: Awaited<OwnedProcessHandle['retirement']>) => {
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
  };
  const result = (evidence?: Awaited<OwnedProcessHandle['retirement']>): BoundedCommandResult => ({
      stdout: Buffer.concat(output.stdout).toString('utf8'),
      stderr: Buffer.concat(output.stderr).toString('utf8'),
      code,
      durationMs: Math.ceil(performance.now() - startedAt),
      timedOut: stopReason === 'timeout',
      outputLimitExceeded: stopReason === 'output-limit',
      aborted: stopReason === 'aborted',
      childExited: childExited(),
      targetPID: handle.targetPID,
      targetCloseObserved: targetClosed,
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
      ownedProcessesExited: processRetired(evidence),
      stdioClosed: naturalStreamsClosed(),
      signal,
      launchError,
      stdoutBytes: observed.stdout,
      stderrBytes: observed.stderr,
      stdoutRetainedBytes: retained.stdout,
      stderrRetainedBytes: retained.stderr,
      stdoutSha256: hashes.stdout.copy().digest('hex'),
      stderrSha256: hashes.stderr.copy().digest('hex'),
    });

  const onAbort = () => stop('aborted');
  const clearTimers = () => {
    if (timeoutTimer) clearTimeout(timeoutTimer);
    timeoutTimer = undefined;
    options.signal?.removeEventListener('abort', onAbort);
  };
  const stop = (reason: NonNullable<typeof stopReason>) => {
    if (settled || stopReason) return;
    stopReason = reason;
    handle.stop(reason);
    void finish(true);
  };
  const finish = async (forceFailure: boolean) => {
    if (cleanupTask) return cleanupTask;
    cleanupTask = (async () => {
      let evidence: Awaited<OwnedProcessHandle['retirement']> | undefined;
      if (forceFailure && !handle.isSupervisorClosed) {
        try { handle.release(); } catch (error) { ownedFailure ||= error instanceof Error ? error : new Error('owned supervisor release failed'); }
      }
      try {
        evidence = await verifyOwnedRetirement(handle);
      } catch (error) {
        ownedFailure ||= error instanceof Error ? error : new Error('owned process retirement failed');
      }
      if (!evidence?.groupAbsent || !evidence.anchorExitObserved) handle.release();
      if (settled) return;
      settled = true;
      clearTimers();
      handle.stdout.removeListener('data', onStdout);
      handle.stderr.removeListener('data', onStderr);
      handle.stdout.removeListener('end', onStdoutEnd);
      handle.stderr.removeListener('end', onStderrEnd);
      handle.stdout.removeListener('close', onStdoutClose);
      handle.stderr.removeListener('close', onStderrClose);
      handle.targetExit.then(() => undefined);
      const value = result(evidence);
      if (forceFailure || value.timedOut || value.outputLimitExceeded || value.aborted || !value.ownedProcessesExited
        || !value.stdioClosed || !value.childExited || value.code !== 0 || value.launchError || ownedFailure) {
        rejectCommand(new BoundedCommandError(binary, args, value));
      } else resolveCommand(value);
    })();
    return cleanupTask;
  };
  function onStdout(chunk: Buffer): void { onData('stdout', chunk); }
  function onStderr(chunk: Buffer): void { onData('stderr', chunk); }
  function onStdoutEnd(): void { stdoutEnded = true; maybeFinish(); }
  function onStderrEnd(): void { stderrEnded = true; maybeFinish(); }
  function onStdoutClose(): void { stdoutClosed = true; maybeFinish(); }
  function onStderrClose(): void { stderrClosed = true; maybeFinish(); }
  function onData(stream: 'stdout' | 'stderr', value: Buffer): void {
    if (settled) return;
    const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
    observed[stream] += chunk.length;
    hashes[stream].update(chunk);
    if (stopReason) return;
    const combinedRetained = retained.stdout + retained.stderr;
    const remaining = Math.max(0, options.maxBytes - combinedRetained);
    const keep = Math.min(chunk.length, remaining);
    if (keep) {
      output[stream].push(chunk.subarray(0, keep));
      retained[stream] += keep;
    }
    if (observed.stdout + observed.stderr > options.maxBytes || observed[stream] > maxBytesPerStream) {
      stop('output-limit');
      return;
    }
    if (performance.now() >= startedAt + allowedMs || options.budget?.exhausted) stop('timeout');
  }
  function maybeFinish(): void {
    if (settled || cleanupTask) return;
    if (!stopReason && targetClosed && naturalStreamsClosed()) void finish(false);
  }

  handle.stdout.on('data', onStdout);
  handle.stderr.on('data', onStderr);
  handle.stdout.once('end', onStdoutEnd);
  handle.stderr.once('end', onStderrEnd);
  handle.stdout.once('close', onStdoutClose);
  handle.stderr.once('close', onStderrClose);
  handle.inputClosed.then(() => { stdinClosed = true; maybeFinish(); });
  if (handle.stdin.closed) stdinClosed = true;
  handle.targetExit.then(value => {
    if (!value) return;
    targetExited = Boolean(handle.binding && !value.launchError);
    code = value.code ?? 1;
    signal = value.signal || undefined;
    if (value.launchError) {
      launchError = redactText(value.launchError).slice(0, 500);
      handle.stop('target-launch-error');
      void finish(true);
      return;
    }
    maybeFinish();
  });
  handle.targetClose.then(value => {
    if (value && handle.binding && !value.launchError) {
      targetClosed = true;
      if (!targetExited) {
        targetExited = true;
        code = value.code ?? 1;
        signal = value.signal || undefined;
      }
    }
    maybeFinish();
  });
  handle.failure.then(error => {
    ownedFailure ||= error;
    if (!handle.binding && !launchError) launchError = redactText(error.message).slice(0, 500);
    if (handle.isSupervisorClosed) void finish(true);
  });
  handle.stopCause.then(reason => {
    if (reason === 'normal-retirement' || reason === 'target-launch-error' || stopReason || settled) return;
    if (reason === 'timeout' || reason === 'hard-deadline' || reason === 'startup-hard-deadline') {
      stop('timeout');
      return;
    }
    ownedFailure ||= new Error(`owned process stopped before normal completion (${reason})`);
    stop('aborted');
  });
  timeoutTimer = setTimeout(() => stop('timeout'), allowedMs - cleanupMs);
  options.signal?.addEventListener('abort', onAbort, { once: true });
  if (options.signal?.aborted) onAbort();

  return new Promise<BoundedCommandResult>((resolve, reject) => {
    resolveCommand = resolve;
    rejectCommand = reject;
    handle.ready.then(() => {
      if (stopReason || options.signal?.aborted) {
        if (!stopReason) stop('aborted');
        return;
      }
      handle.start();
    }, error => {
      ownedFailure ||= error instanceof Error ? error : new Error('owned supervisor failed to initialize');
      launchError ||= redactText(ownedFailure.message).slice(0, 500);
      const supervisorTimedOut = /^OWNED_PROCESS: dispatch stopped \((?:timeout|hard-deadline|startup-hard-deadline)\)$/u.test(ownedFailure.message);
      stopReason ||= options.signal?.aborted ? 'aborted' : supervisorTimedOut ? 'timeout' : 'aborted';
      void finish(true);
    });
    handle.supervisor.once('close', () => {
      if (!cleanupTask && !settled) {
        ownedFailure ||= new Error('owned supervisor closed before command retirement was confirmed');
        void finish(true);
      }
    });
    if (options.signal?.aborted) stop('aborted');
  });
}

function processGroupExistsNow(handle: OwnedProcessHandle): boolean {
  return handle.binding ? !requireGroupAbsent(handle.binding.groupID) : false;
}

function requireGroupAbsent(groupID: number): boolean {
  try { process.kill(-groupID, 0); return false; } catch (error) {
    return (error as NodeJS.ErrnoException).code === 'ESRCH';
  }
}

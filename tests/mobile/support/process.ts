import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { redactText } from './diagnostics';
import { PhaseBudget, PhaseBudgetError } from './budget';

export interface CommandResult {
  stdout: string;
  stderr: string;
  code: number;
  durationMs: number;
  timedOut: boolean;
  signal?: string;
}

export interface CommandOptions {
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  budget?: PhaseBudget;
  label?: string;
}

export class CommandError extends Error {
  readonly code = 'COMMAND_FAILED';
  readonly binary: string;
  readonly args: string[];
  readonly durationMs: number;
  readonly exitCode: number;
  readonly timedOut: boolean;
  readonly signal?: string;
  readonly stdout: string;
  readonly stderr: string;
  readonly budgetError?: PhaseBudgetError;

  constructor(binary: string, args: string[], result: CommandResult, budgetError?: PhaseBudgetError) {
    const detail = redactText(result.stderr || result.stdout).slice(0, 1_000);
    super(`${budgetError?.code || 'COMMAND_FAILED'}: ${formatCommand(binary, args)}${detail ? `: ${detail}` : ''}`);
    this.name = 'CommandError';
    this.binary = binary;
    this.args = args.map((value) => redactText(value).slice(0, 300));
    this.durationMs = result.durationMs;
    this.exitCode = result.code;
    this.timedOut = result.timedOut;
    this.signal = result.signal;
    this.stdout = redactText(result.stdout).slice(0, 8_000);
    this.stderr = redactText(result.stderr).slice(0, 8_000);
    this.budgetError = budgetError;
  }
}

function formatCommand(binary: string, args: string[]): string {
  return redactText([binary, ...args].map((value) => JSON.stringify(value)).join(' ')).slice(0, 500);
}

function resultFor(stdout: string, stderr: string, code: number, startedAt: number, error: NodeJS.ErrnoException | null): CommandResult {
  const processError = error as (NodeJS.ErrnoException & { killed?: boolean; signal?: string }) | null;
  return {
    stdout: String(stdout),
    stderr: String(stderr),
    code,
    durationMs: Date.now() - startedAt,
    timedOut: Boolean(processError && (processError.code === 'ETIMEDOUT' || processError.killed)),
    signal: processError?.signal || undefined,
  };
}

export function command(
  binary: string,
  args: string[],
  timeoutMs = 30_000,
  options: CommandOptions = {},
): Promise<CommandResult> {
  options.budget?.assertAvailable(options.label || binary);
  const requestTimeoutMs = Math.max(1, Math.min(timeoutMs, options.budget?.remainingMs ?? timeoutMs));
  return new Promise((resolve, reject) => {
    const startedAt = Date.now();
    const child = execFile(binary, args, {
      timeout: requestTimeoutMs,
      maxBuffer: 8 * 1024 * 1024,
      cwd: options.cwd,
      env: options.env,
    }, (error, stdout, stderr) => {
      const errno = error as NodeJS.ErrnoException | null;
      const code = errno && typeof errno.code === 'number' ? Number(errno.code) : errno ? 1 : 0;
      const result = resultFor(String(stdout), String(stderr), code, startedAt, errno);
      const budgetError = options.budget?.exhausted
        ? new PhaseBudgetError('PHASE_BUDGET_EXHAUSTED', options.budget.phase, options.label || binary, result.durationMs, options.budget.recoveryCount)
        : undefined;
      if (code !== 0 || budgetError) {
        reject(new CommandError(binary, args, result, budgetError as PhaseBudgetError | undefined));
        return;
      }
      resolve(result);
    });
    child.on('error', (error) => {
      const result = resultFor('', String(error), 1, startedAt, error as NodeJS.ErrnoException);
      reject(new CommandError(binary, args, result));
    });
  });
}

export function startCommand(
  binary: string,
  args: string[],
  output?: (chunk: string) => void,
  options: CommandOptions = {},
): ChildProcess {
  options.budget?.assertAvailable(options.label || binary);
  const child = spawn(binary, args, {
    stdio: ['ignore', 'pipe', 'pipe'],
    cwd: options.cwd,
    env: options.env,
  });
  child.stdout?.on('data', (chunk: Buffer) => output?.(redactText(chunk.toString())));
  child.stderr?.on('data', (chunk: Buffer) => output?.(redactText(chunk.toString())));
  return child;
}

export async function commandOutput(binary: string, args: string[], timeoutMs = 30_000, options: CommandOptions = {}): Promise<string> {
  return (await command(binary, args, timeoutMs, options)).stdout;
}

export async function stopProcess(child: ChildProcess, timeoutMs = 5_000, budget?: PhaseBudget): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const limit = Math.min(timeoutMs, budget?.remainingMs ?? timeoutMs);
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
  child.kill('SIGTERM');
  await Promise.race([exited, new Promise<void>((resolve) => setTimeout(resolve, Math.max(1, limit)))]);
  if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
}

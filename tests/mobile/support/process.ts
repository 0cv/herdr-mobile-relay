import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { delay } from './webdriver';
import { redactText } from './diagnostics';

export interface CommandResult {
  stdout: string;
  stderr: string;
  code: number;
}

export function command(
  binary: string,
  args: string[],
  timeoutMs = 30_000,
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): Promise<CommandResult> {
  return new Promise((resolve, reject) => {
    const child = execFile(binary, args, { timeout: timeoutMs, maxBuffer: 8 * 1024 * 1024, ...options }, (error, stdout, stderr) => {
      const code = error && typeof (error as NodeJS.ErrnoException).code === 'number'
        ? Number((error as NodeJS.ErrnoException).code)
        : error
          ? 1
          : 0;
      const result = { stdout: String(stdout), stderr: String(stderr), code };
      if (code !== 0) {
        reject(new Error(`COMMAND_FAILED: ${binary}: ${redactText(result.stderr || result.stdout).slice(0, 1_000)}`));
        return;
      }
      resolve(result);
    });
    child.on('error', reject);
  });
}

export function startCommand(
  binary: string,
  args: string[],
  output?: (chunk: string) => void,
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): ChildProcess {
  const child = spawn(binary, args, { stdio: ['ignore', 'pipe', 'pipe'], ...options });
  child.stdout?.on('data', (chunk: Buffer) => output?.(redactText(chunk.toString())));
  child.stderr?.on('data', (chunk: Buffer) => output?.(redactText(chunk.toString())));
  return child;
}

export async function commandOutput(binary: string, args: string[], timeoutMs = 30_000): Promise<string> {
  return (await command(binary, args, timeoutMs)).stdout;
}

export async function stopProcess(child: ChildProcess, timeoutMs = 5_000): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
  child.kill('SIGTERM');
  await Promise.race([exited, delay(timeoutMs)]);
  if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
}

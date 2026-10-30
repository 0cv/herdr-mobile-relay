import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { afterEach, describe, expect, it } from 'vitest';

const runner = resolve(import.meta.dirname, '../../scripts/run-attention-tests.sh');
const temporaryDirectories: string[] = [];
interface Invocation { command: string; args: string[]; endpoint?: string }

function run(environment: Record<string, string> = {}, args: string[] = []) {
  const directory = mkdtempSync(join(tmpdir(), 'herdr-attention-runner-'));
  temporaryDirectories.push(directory);
  const log = join(directory, 'calls.jsonl');
  writeFileSync(log, '');
  const executable = (name: string, body: string) => writeFileSync(join(directory, name),
    `#!${process.execPath}\n${body}\n`, { mode: 0o755 });
  const record = `
const { appendFileSync } = require('node:fs');
const { basename } = require('node:path');
const args = process.argv.slice(2);
appendFileSync(process.env.RUNNER_LOG, JSON.stringify({
  command: basename(process.argv[1]), args, endpoint: process.env.HERDR_WEBKIT_WS_ENDPOINT || '',
}) + '\\n');
`;
  executable('bun', `${record}
if (args[0] === '-e') {
  if (args[1].includes('devDependencies')) console.log(process.env.FAKE_VERSION || '1.62.1');
  else process.exit(Number(process.env.FAKE_READY_EXIT || 0));
} else process.exit(Number(process.env.FAKE_TEST_EXIT || 0));
`);
  const runtime = `${record}
if (args[0] === 'run') console.log('owned-test-container');
if (args[0] === 'port') console.log(process.env.FAKE_BIND || '127.0.0.1:43210');
`;
  executable('docker', runtime);
  executable('podman', runtime);
  executable('grep', 'process.exit(process.env.FAKE_FEDORA === "1" ? 0 : 1);');
  const result = spawnSync('bash', [runner, ...args], {
    env: {
      ...process.env,
      PATH: `${directory}:${process.env.PATH}`,
      RUNNER_LOG: log,
      HERDR_WEBKIT_CONTAINER: '1',
      HERDR_WEBKIT_WS_ENDPOINT: '',
      ...environment,
    },
    encoding: 'utf8',
    timeout: 10_000,
  });
  const calls: Invocation[] = readFileSync(log, 'utf8').trim().split('\n').filter(Boolean)
    .map((line: string) => JSON.parse(line) as Invocation);
  return { ...result, calls };
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

describe('attention browser runner', () => {
  it('uses the cached version-matched browser server, forwards arguments, and removes only its container', () => {
    const result = run({}, ['--project=webkit-attention', '--grep', 'full conversation']);
    expect(result.status, result.stderr).toBe(0);
    const launched = result.calls.find((call) => call.command === 'docker' && call.args[0] === 'run')!;
    expect(launched.args).toContain('mcr.microsoft.com/playwright:v1.62.1-noble');
    expect(launched.args).toContain('127.0.0.1::3000');
    expect(launched.args.some((arg) => arg.endsWith(':/work/frontend:ro'))).toBe(true);
    expect(launched.args).toContain('node_modules/playwright/cli.js');
    const tests = result.calls.find((call) => call.command === 'bun' && call.args[0] === 'x')!;
    expect(tests.args).toEqual(['x', 'playwright', 'test', '--config', 'playwright.attention.config.ts',
      '--project=webkit-attention', '--grep', 'full conversation']);
    expect(tests.endpoint).toBe('ws://127.0.0.1:43210/');
    expect(result.calls.filter((call) => call.args[0] === 'rm').map((call) => call.args))
      .toEqual([['rm', '-f', 'owned-test-container']]);
  });

  it('automatically uses Podman with a read-only SELinux-compatible mount on Fedora', () => {
    const result = run({ HERDR_WEBKIT_CONTAINER: '', FAKE_FEDORA: '1' });
    expect(result.status, result.stderr).toBe(0);
    expect(result.calls.some((call) => call.command === 'docker')).toBe(false);
    expect(result.calls.find((call) => call.command === 'podman' && call.args[0] === 'run')?.args)
      .toContain('label=disable');
  });

  it('preserves native execution on supported hosts', () => {
    const result = run({ HERDR_WEBKIT_CONTAINER: '', FAKE_FEDORA: '0' }, ['--list']);
    expect(result.status, result.stderr).toBe(0);
    expect(result.calls).toEqual([{
      command: 'bun', endpoint: '',
      args: ['x', 'playwright', 'test', '--config', 'playwright.attention.config.ts', '--list'],
    }]);
  });

  it('preserves test failures while cleaning up the browser server', () => {
    const result = run({ FAKE_TEST_EXIT: '7' });
    expect(result.status).toBe(7);
    expect(result.calls.at(-1)?.args).toEqual(['rm', '-f', 'owned-test-container']);
  });

  it('reports startup failure and cleans up without starting the test fixture', () => {
    const result = run({ FAKE_READY_EXIT: '1' });
    expect(result.status).toBe(1);
    expect(result.calls.some((call) => call.args[0] === 'logs')).toBe(true);
    expect(result.calls.some((call) => call.command === 'bun' && call.args[0] === 'x')).toBe(false);
    expect(result.calls.at(-1)?.args).toEqual(['rm', '-f', 'owned-test-container']);
  });

  it('rejects a non-loopback published address', () => {
    const result = run({ FAKE_BIND: '0.0.0.0:43210' });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('loopback-only');
    expect(result.calls.at(-1)?.args).toEqual(['rm', '-f', 'owned-test-container']);
  });

  it('does not launch an unpinned browser image', () => {
    const result = run({ FAKE_VERSION: '^1.62.1' });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('exact Playwright version');
    expect(result.calls.some((call) => call.command === 'docker')).toBe(false);
  });
});

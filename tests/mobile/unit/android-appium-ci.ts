import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { redactText } from '../support/diagnostics';

const root = resolve(import.meta.dirname, '../../..');
const runBash = (args: string[], environment: NodeJS.ProcessEnv) =>
  spawnSync('bash', args, { cwd: root, env: environment, encoding: 'utf8' });
const MAX_SHELL_DIAGNOSTIC_BYTES = 4096;
const SHELL_DIAGNOSTIC_TRUNCATION = '...[truncated]';
const shellDiagnostic = (value: unknown): string => {
  const redacted = redactText(String(value ?? ''));
  const bytes = Buffer.from(redacted, 'utf8');
  if (bytes.byteLength <= MAX_SHELL_DIAGNOSTIC_BYTES) return redacted;
  const limit = MAX_SHELL_DIAGNOSTIC_BYTES - Buffer.byteLength(SHELL_DIAGNOSTIC_TRUNCATION, 'utf8');
  let truncated = bytes.subarray(0, limit).toString('utf8');
  while (Buffer.byteLength(truncated, 'utf8') > limit) truncated = truncated.slice(0, -1);
  return `${truncated}${SHELL_DIAGNOSTIC_TRUNCATION}`;
};
const shellDiagnostics = (result: ReturnType<typeof runBash>): string => [
  `status=${result.status ?? 'null'} signal=${result.signal ?? 'none'}`,
  `error=${result.error ? shellDiagnostic(result.error) : 'none'}`,
  `stdout=${shellDiagnostic(result.stdout)}`,
  `stderr=${shellDiagnostic(result.stderr)}`,
].join('\n');
const assertExit = (result: ReturnType<typeof runBash>, expected: number, context: string): void => {
  assert.equal(result.status, expected, `${context}\n${shellDiagnostics(result)}`);
};
const assertFailure = (result: ReturnType<typeof runBash>, context: string): void => {
  assert.notEqual(result.status, 0, `${context}\n${shellDiagnostics(result)}`);
};
const assertShellDiagnosticBoundaries = (): void => {
  const tokenDiagnostic = shellDiagnostic(`${'.'.repeat(4060)}${'A'.repeat(43)}`);
  assert.equal(tokenDiagnostic.includes('A'.repeat(36)), false);
  assert.match(tokenDiagnostic, /\[REDACTED\]/u);
  assert.ok(Buffer.byteLength(tokenDiagnostic, 'utf8') <= MAX_SHELL_DIAGNOSTIC_BYTES);

  const unicodeDiagnostic = shellDiagnostic('界'.repeat(4096));
  assert.ok(Buffer.byteLength(unicodeDiagnostic, 'utf8') <= MAX_SHELL_DIAGNOSTIC_BYTES);

  const expandedDiagnostic = shellDiagnostic(`#setup=x#${'.'.repeat(4090)}`);
  assert.match(expandedDiagnostic, /#setup=\[REDACTED\]/u);
  assert.ok(Buffer.byteLength(expandedDiagnostic, 'utf8') <= MAX_SHELL_DIAGNOSTIC_BYTES);
};
assertShellDiagnosticBoundaries();
const scratch = await mkdtemp(join(tmpdir(), 'android-appium-ci-'));
try {
  const bin = join(scratch, 'bin');
  await mkdir(bin);
  for (const tool of ['node', 'npm', 'appium', 'curl']) {
    await writeFile(join(bin, tool), `#!/bin/bash
printf '%s\\n' '${tool}'" $*" >> "$TRACE"
if [[ "$*" == *" $FAIL_MODE "* ]]; then exit 41; fi
if [[ "$*" == *"main.js --address"* ]]; then exit 42; fi
`, { mode: 0o755 });
  }
  const trace = join(scratch, 'trace');
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, APPIUM_HOME: join(scratch, 'home'),
    RUNNER_TEMP: scratch, TRACE: trace, MOBILE_PLATFORM: 'android', IOS_XCTEST_STATE_DIR: join(scratch, 'ios-xctest'),
    GITHUB_ENV: join(scratch, 'env') };
  for (const filename of ['.github/actions/mobile-device-run/action.yml', '.github/workflows/mobile-ci.yml']) {
    const text = await readFile(join(root, filename), 'utf8');
    const steps = text.split(/(?=^\s*- name: )/mu);
    const extract = (name: string): string => {
      const step = steps.find(value => value.includes(`- name: ${name}\n`));
      assert.ok(step);
      const lines = step.split('\n');
      const run = lines.findIndex(line => /run: \|$/u.test(line));
      assert.ok(run >= 0);
      const indent = lines[run].search(/\S/u) + 2;
      return lines.slice(run + 1).filter(line => line.slice(0, indent).trim() === '')
        .map(line => line.slice(indent)).join('\n');
    };
    const install = extract('Install pinned Appium and platform driver');
    const start = extract('Start Appium');
    for (const mode of ['configure', 'patch']) {
      await writeFile(trace, '');
      const result = runBash(['-c', install], { ...env, FAIL_MODE: mode });
      assertFailure(result, `${filename}: ${mode} failure ignored`);
      const calls = await readFile(trace, 'utf8');
      assert.ok(!calls.includes('appium '));
      if (mode === 'configure') assert.ok(!calls.includes('npm '));
    }
    await writeFile(trace, '');
    assertExit(runBash(['-c', install], { ...env, FAIL_MODE: 'never' }), 0, `${filename}: successful install`);
    const calls = (await readFile(trace, 'utf8')).trim().split('\n');
    assert.equal(calls.length, 4);
    assert.match(calls[0], /gate.ts configure/u);
    assert.match(calls[1], /npm ci .*--ignore-scripts.*--engine-strict/u);
    assert.match(calls[2], /gate.ts patch/u);
    assert.match(calls[3], /node .*\/home\/node_modules\/appium\/build\/lib\/main.js driver list --installed/u);
    await writeFile(trace, '');
    assertFailure(runBash(['-c', start], { ...env, FAIL_MODE: 'verify' }), `${filename}: startup verification refusal`);
    const failedStart = await readFile(trace, 'utf8');
    assert.match(failedStart, /gate.ts verify/u);
    assert.ok(!failedStart.includes('main.js') && !failedStart.includes('curl '));
    const capture = extract('Sanitize bounded diagnostics');
    const output = join(scratch, 'herdr-mobile-output');
    const integrity = join(env.APPIUM_HOME, 'retained-owner-integrity.json');
    const artifact = join(output, 'android-appium-integrity.json');
    await mkdir(env.APPIUM_HOME, { recursive: true });
    const proof = JSON.stringify({ appium: { version: '3.1.1', files: { 'build/lib/main.js': 'reviewed-hash' } } });
    await writeFile(integrity, proof);
    assertExit(runBash(['-c', capture], { ...env, MOBILE_OUTPUT: output }), 0, `${filename}: Android integrity export`);
    assert.equal(await readFile(artifact, 'utf8'), proof);
    assert.ok(text.indexOf('- name: Sanitize bounded diagnostics') < text.indexOf('uses: actions/upload-artifact@v4', text.indexOf('- name: Sanitize bounded diagnostics')));
    assert.ok(text.indexOf('- name: Upload mobile evidence') < text.indexOf('- name: Stop owned mobile processes and devices'));
    assert.match(text.slice(text.indexOf('- name: Upload mobile evidence')), /path: \$\{\{ runner.temp \}\}\/herdr-mobile-output/u);
    await rm(artifact);
    await writeFile(integrity, 'x'.repeat(1048577));
    assertFailure(runBash(['-c', capture], { ...env, MOBILE_OUTPUT: output }), `${filename}: oversized Android integrity export refusal`);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    assertExit(runBash(['-c', capture], { ...env, MOBILE_OUTPUT: output, MOBILE_PLATFORM: 'ios' }), 0, `${filename}: iOS diagnostic export skips Android integrity`);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    await rm(integrity);
    assertExit(runBash(['-c', capture], { ...env, MOBILE_OUTPUT: output }), 0, `${filename}: missing Android integrity export`);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    console.log(`PASS ${filename}: scoped installation, startup refusal and bounded Android integrity artifact routing`);
  }
  await writeFile(trace, '');
  assertFailure(runBash(['tests/mobile/android-appium/ci.sh', 'start', '--address', '127.0.0.1'],
    { ...env, FAIL_MODE: 'verify' }), 'launch wrapper refuses failed verification');
  assert.ok(!(await readFile(trace, 'utf8')).includes('main.js'));
  await writeFile(trace, '');
  assertExit(runBash(['tests/mobile/android-appium/ci.sh', 'start', '--address', '127.0.0.1'],
    { ...env, FAIL_MODE: 'never' }), 42, 'launch wrapper executes the verified Appium');
  const startCalls = (await readFile(trace, 'utf8')).trim().split('\n');
  assert.equal(startCalls.length, 2);
  assert.match(startCalls[0], /gate.ts verify/u);
  assert.match(startCalls[1], /\/home\/node_modules\/appium\/build\/lib\/main.js --address/u);
  console.log('PASS launch wrapper verifies immediately before exec of the same local Appium');
} finally {
  await rm(scratch, { recursive: true, force: true });
}

import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

const root = resolve(import.meta.dirname, '../../..');
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
    RUNNER_TEMP: scratch, TRACE: trace, MOBILE_PLATFORM: 'android', GITHUB_ENV: join(scratch, 'env') };
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
      const result = spawnSync('bash', ['-c', install], { cwd: root, env: { ...env, FAIL_MODE: mode }, encoding: 'utf8' });
      assert.notEqual(result.status, 0, `${filename}: ${mode} failure ignored`);
      const calls = await readFile(trace, 'utf8');
      assert.ok(!calls.includes('appium '));
      if (mode === 'configure') assert.ok(!calls.includes('npm '));
    }
    await writeFile(trace, '');
    assert.equal(spawnSync('bash', ['-c', install], { cwd: root, env: { ...env, FAIL_MODE: 'never' } }).status, 0);
    const calls = (await readFile(trace, 'utf8')).trim().split('\n');
    assert.equal(calls.length, 4);
    assert.match(calls[0], /gate.ts configure/u);
    assert.match(calls[1], /npm ci .*--ignore-scripts.*--engine-strict/u);
    assert.match(calls[2], /gate.ts patch/u);
    assert.match(calls[3], /node .*\/home\/node_modules\/appium\/build\/lib\/main.js driver list --installed/u);
    await writeFile(trace, '');
    assert.notEqual(spawnSync('bash', ['-c', start], { cwd: root, env: { ...env, FAIL_MODE: 'verify' } }).status, 0);
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
    assert.equal(spawnSync('bash', ['-c', capture], { cwd: root, env: { ...env, MOBILE_OUTPUT: output } }).status, 0);
    assert.equal(await readFile(artifact, 'utf8'), proof);
    assert.ok(text.indexOf('- name: Sanitize bounded diagnostics') < text.indexOf('uses: actions/upload-artifact@v4', text.indexOf('- name: Sanitize bounded diagnostics')));
    assert.ok(text.indexOf('- name: Upload mobile evidence') < text.indexOf('- name: Stop owned mobile processes and devices'));
    assert.match(text.slice(text.indexOf('- name: Upload mobile evidence')), /path: \$\{\{ runner.temp \}\}\/herdr-mobile-output/u);
    await rm(artifact);
    await writeFile(integrity, 'x'.repeat(1048577));
    assert.notEqual(spawnSync('bash', ['-c', capture], { cwd: root, env: { ...env, MOBILE_OUTPUT: output } }).status, 0);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    assert.equal(spawnSync('bash', ['-c', capture], { cwd: root, env: { ...env, MOBILE_OUTPUT: output, MOBILE_PLATFORM: 'ios' } }).status, 0);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    await rm(integrity);
    assert.equal(spawnSync('bash', ['-c', capture], { cwd: root, env: { ...env, MOBILE_OUTPUT: output } }).status, 0);
    await assert.rejects(() => readFile(artifact), /ENOENT/u);
    console.log(`PASS ${filename}: scoped installation, startup refusal and bounded Android integrity artifact routing`);
  }
  await writeFile(trace, '');
  assert.notEqual(spawnSync('bash', ['tests/mobile/android-appium/ci.sh', 'start', '--address', '127.0.0.1'],
    { cwd: root, env: { ...env, FAIL_MODE: 'verify' } }).status, 0);
  assert.ok(!(await readFile(trace, 'utf8')).includes('main.js'));
  await writeFile(trace, '');
  assert.equal(spawnSync('bash', ['tests/mobile/android-appium/ci.sh', 'start', '--address', '127.0.0.1'],
    { cwd: root, env: { ...env, FAIL_MODE: 'never' } }).status, 42);
  const startCalls = (await readFile(trace, 'utf8')).trim().split('\n');
  assert.equal(startCalls.length, 2);
  assert.match(startCalls[0], /gate.ts verify/u);
  assert.match(startCalls[1], /\/home\/node_modules\/appium\/build\/lib\/main.js --address/u);
  console.log('PASS launch wrapper verifies immediately before exec of the same local Appium');
} finally {
  await rm(scratch, { recursive: true, force: true });
}

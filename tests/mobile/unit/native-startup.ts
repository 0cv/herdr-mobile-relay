import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { createConnection } from 'node:net';
import { AndroidStartupLog } from '../support/android-startup-log';
import { managedWdaCapabilities, validWdaStatus } from '../support/ios-xctest';
import { AndroidPlatform } from '../platforms/android';

class Socket extends EventEmitter {
  writes: string[] = [];
  destroyed = false;
  write(value: Buffer) { this.writes.push(value.toString()); return true; }
  destroy() { this.destroyed = true; return this; }
}
function frame(id: number, body: Buffer) {
  const header = Buffer.alloc(5);
  header[0] = id;
  header.writeUInt32LE(body.length, 1);
  return Buffer.concat([header, body]);
}

const stepBlock = (source: string, name: string): string => {
  const lines = source.split('\n');
  const marker = lines.findIndex(line => line.includes(`name: ${name}`));
  const run = lines.findIndex((line, index) => index > marker && line.trim() === 'run: |');
  const runIndent = lines[run].match(/^\s*/u)?.[0].length || 0;
  const shell = lines.findIndex((line, index) => index > run && line.trim() === 'shell: bash');
  const nextStep = lines.findIndex((line, index) => index > run
    && line.trimStart().startsWith('- name:')
    && (line.match(/^\s*/u)?.[0].length || 0) === runIndent - 2);
  const end = Math.min(shell >= 0 ? shell : Number.POSITIVE_INFINITY, nextStep >= 0 ? nextStep : Number.POSITIVE_INFINITY);
  const body = lines.slice(run + 1, end);
  const indent = body.find(line => line.trim())?.match(/^\s*/u)?.[0].length || 0;
  return body.map(line => line.slice(indent)).join('\n');
};

const exportBlock = (source: string): string => stepBlock(source, 'Sanitize bounded diagnostics');
const createIosBlock = (source: string): string => stepBlock(source, 'Create and boot iOS simulator');

const runShellBlock = (script: string, cwd: string, env: NodeJS.ProcessEnv): Promise<{ code: number | null; stderr: string }> => new Promise((resolve, reject) => {
  const child = spawn('bash', ['-euo', 'pipefail', '-c', script], { cwd, env, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  child.on('error', reject);
  child.on('close', code => resolve({ code, stderr }));
});

const runExportBlock = (script: string, cwd: string, env: NodeJS.ProcessEnv): Promise<{ code: number | null; stderr: string }> => runShellBlock(script, cwd, env);

const createIosRuntimeFixture = async (inventory: string) => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-ios-runtime-'));
  const bin = join(root, 'bin');
  const runnerTemp = join(root, 'runner-temp');
  const envFile = join(root, 'github-env');
  const ownershipFile = join(root, 'owned');
  const xcrunLog = join(root, 'xcrun.log');
  await mkdir(bin, { recursive: true });
  await mkdir(runnerTemp, { recursive: true });
  const scripts: Record<string, string> = {
    sudo: '#!/bin/sh\nexit 0\n',
    xcodebuild: '#!/bin/sh\nprintf "Xcode 16.4\\nBuild version 16F6\\n"\n',
    defaults: '#!/bin/sh\nexit 0\n',
    open: '#!/bin/sh\nexit 0\n',
    pgrep: '#!/bin/sh\nexit 0\n',
    xcrun: '#!/bin/sh\nprintf "%s\\n" "$*" >> "$MOCK_XCRUN_LOG"\nif [ "$1" = simctl ] && [ "$2" = list ] && [ "$3" = runtimes ]; then cat "$MOCK_RUNTIME_INVENTORY"; exit 0; fi\nif [ "$1" = simctl ] && [ "$2" = list ] && [ "$3" = devicetypes ]; then printf "%s\\n" "iPhone 16 (com.apple.CoreSimulator.SimDeviceType.iPhone-16)"; exit 0; fi\nif [ "$1" = simctl ] && [ "$2" = create ]; then printf "%s\\n" fake-udid; exit 0; fi\nexit 0\n',
  };
  await Promise.all(Object.entries(scripts).map(async ([name, script]) => writeFile(join(bin, name), script, { mode: 0o700 })));
  const inventoryFile = join(root, 'inventory.txt');
  await writeFile(inventoryFile, inventory);
  await writeFile(envFile, '');
  return {
    root,
    runnerTemp,
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH || ''}`,
      RUNNER_TEMP: runnerTemp,
      GITHUB_ENV: envFile,
      MOBILE_DEVICE_OWNERSHIP_FILE: ownershipFile,
      IOS_PLATFORM_VERSION: '18.6',
      GITHUB_RUN_ID: 'fixture',
      GITHUB_RUN_ATTEMPT: '1',
      MOCK_RUNTIME_INVENTORY: inventoryFile,
      MOCK_XCRUN_LOG: xcrunLog,
    },
    xcrunLog,
  };
};

export const nativeStartupTests: Array<[string, () => Promise<void>]> = [
  ['Native startup diagnostic shell-v2 fragmented output and remote exit', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-startup-'));
    const socket = new Socket();
    let connects = 0;
    const capture = new AndroidStartupLog(root, (() => { connects++; return socket; }) as unknown as typeof createConnection);
    try {
      capture.start('emulator-5554');
      socket.emit('connect');
      socket.emit('data', Buffer.from('OK'));
      socket.emit('data', Buffer.from('AY'));
      socket.emit('data', Buffer.from('OKAY'));
      const data = Buffer.concat([frame(1, Buffer.from('preceding failure\n')), frame(2, Buffer.from('transport error\n')), frame(3, Buffer.from([255]))]);
      for (const byte of data) socket.emit('data', Buffer.from([byte]));
      socket.emit('close');
      await capture.waitForHandshake();
      await capture.finish();
      const result = JSON.parse(await readFile(join(root, 'android-startup-logcat.json'), 'utf8'));
      assert.equal(connects, 1);
      assert.equal(result.exitCode, 255);
      assert.equal(result.frames, 3);
      assert.equal(result.exhaustiveCoverage, false);
      assert.equal(socket.writes.length, 2);
      assert.ok(socket.writes[1].endsWith('shell,v2,raw:logcat -b all -v threadtime'));
      assert.equal(await readFile(join(root, 'android-startup-logcat.log'), 'utf8'), 'preceding failure\n');
      assert.equal(await readFile(join(root, 'android-startup-logcat-stderr.log'), 'utf8'), 'transport error\n');
      assert.ok(socket.destroyed);
    } finally { await capture.finish(); await rm(root, { recursive: true, force: true }); }
  }],
  ['Native startup waits for the bounded shell handshake before preparation', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-startup-'));
    const socket = new Socket();
    const capture = new AndroidStartupLog(root, (() => socket) as unknown as typeof createConnection, 1_000, 100);
    let prepared = false;
    try {
      capture.start('emulator-5554');
      const waiting = capture.waitForHandshake().then(() => { prepared = true; });
      socket.emit('connect');
      await new Promise(resolve => setTimeout(resolve, 10));
      assert.equal(prepared, false);
      socket.emit('data', Buffer.from('OKAY'));
      await new Promise(resolve => setTimeout(resolve, 10));
      assert.equal(prepared, false);
      await new Promise(resolve => setTimeout(resolve, 25));
      socket.emit('data', Buffer.from('OKAY'));
      await waiting;
      assert.equal(prepared, true);
    } finally {
      await capture.finish();
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['Native startup export sanitizes failed-owner WDA logs on both CI paths', async () => {
    const repo = join(import.meta.dirname, '../../..');
    const secret = 'https://example.test/?token=export-secret';
    for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const root = await mkdtemp(join(tmpdir(), 'herdr-export-'));
      const state = join(root, 'state');
      const runnerTemp = join(root, 'runner-temp');
      const output = join(root, 'output');
      await Promise.all([mkdir(state, { recursive: true }), mkdir(runnerTemp, { recursive: true })]);
      const listenerValidation = {
        endpoints: { wda: { status: 'evaluated', count: 1, pids: ['1234'] }, mjpeg: { status: 'evaluated', count: 0, pids: [] } },
        commandStatus: [{ port: 8100, status: 0 }], runnerEvidencePresent: 'absent', runnerAssociation: 'not-evaluated', mjpegAssociation: 'not-evaluated',
        bundleId: { status: 'not-evaluated' }, executableHash: { status: 'not-evaluated' },
        failureStage: 'wda-pid-count', errorCategory: 'listener-endpoint-cardinality',
      };
      await writeFile(join(state, 'owner.json'), JSON.stringify({ ready: false, error: 'XCTEST: supervisor unfinished', listenerValidation }));
      await writeFile(join(state, 'owner-private.json'), JSON.stringify({ ready: false, error: secret }));
      await writeFile(join(state, 'wda-preflight-private.log'), `failed launch ${secret}\n`);
      await writeFile(join(runnerTemp, 'wda-preflight.log'), `failed fallback ${secret}\n`);
      await writeFile(join(runnerTemp, 'ios-runtime-inventory.txt'), 'iOS 18.6 (18.6 - 22G86) - com.apple.CoreSimulator.SimRuntime.iOS-18-6\n');
      await writeFile(join(runnerTemp, 'ios-runtime-selection.txt'), 'expected_platform_version=18.6\nselected_identifier=com.apple.CoreSimulator.SimRuntime.iOS-18-6\nselected_version=18.6\nselected_build=22G86\n');
      const source = await readFile(join(repo, path), 'utf8');
      const result = await runExportBlock(exportBlock(source), repo, {
        ...process.env,
        MOBILE_PLATFORM: 'ios',
        IOS_XCTEST_STATE_DIR: state,
        MOBILE_OUTPUT: output,
        RUNNER_TEMP: runnerTemp,
      });
      try {
        assert.equal(result.code, 0, result.stderr);
        const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.equal(owner.ready, false);
        assert.equal(owner.error, 'XCTEST: supervisor unfinished');
        assert.deepEqual(owner.listenerValidation, listenerValidation);
        const exportedOwner = JSON.parse(await readFile(join(output, 'ios-xctest/owner.json'), 'utf8'));
        assert.deepEqual(exportedOwner.listenerValidation, listenerValidation);
        for (const artifact of [join(output, 'ios-xctest/wda-preflight.log'), join(output, 'wda-preflight.log')]) {
          const uploaded = await readFile(artifact, 'utf8');
          assert.equal(uploaded.includes(secret), false);
          assert.ok(uploaded.includes('[REDACTED]'));
        }
        assert.equal(await readFile(join(output, 'ios-runtime-inventory.txt'), 'utf8'), await readFile(join(runnerTemp, 'ios-runtime-inventory.txt'), 'utf8'));
        assert.equal(await readFile(join(output, 'ios-runtime-selection.txt'), 'utf8'), await readFile(join(runnerTemp, 'ios-runtime-selection.txt'), 'utf8'));
      } finally {
        await rm(root, { recursive: true, force: true });
      }
    }
  }],
  ['Native startup iOS runtime capture records selected build before provisioning on both CI paths', async () => {
    const repo = join(import.meta.dirname, '../../..');
    const inventory = [
      '== Runtimes ==',
      'iOS 18.6 (18.6 - 22G86) - com.apple.CoreSimulator.SimRuntime.iOS-18-6',
      'iOS 18.5 (18.5 - 22F76) - com.apple.CoreSimulator.SimRuntime.iOS-18-5',
      '',
    ].join('\n');
    for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const fixture = await createIosRuntimeFixture(inventory);
      try {
        const source = await readFile(join(repo, path), 'utf8');
        const result = await runShellBlock(createIosBlock(source), repo, fixture.env);
        assert.equal(result.code, 0, `${path}: ${result.stderr}`);
        assert.equal(await readFile(join(fixture.runnerTemp, 'ios-runtime-inventory.txt'), 'utf8'), inventory);
        const selection = await readFile(join(fixture.runnerTemp, 'ios-runtime-selection.txt'), 'utf8');
        assert.match(selection, /^expected_platform_version=18\.6$/mu);
        assert.match(selection, /^inventory_line=iOS 18\.6 \(18\.6 - 22G86\) - com\.apple\.CoreSimulator\.SimRuntime\.iOS-18-6$/mu);
        assert.match(selection, /^selected_identifier=com\.apple\.CoreSimulator\.SimRuntime\.iOS-18-6$/mu);
        assert.match(selection, /^selected_version=18\.6$/mu);
        assert.match(selection, /^selected_build=22G86$/mu);
        const requests = await readFile(fixture.xcrunLog, 'utf8');
        assert.ok(requests.indexOf('simctl list runtimes') >= 0);
        assert.ok(requests.indexOf('simctl list runtimes') < requests.indexOf('simctl create'));
        assert.match(await readFile(join(fixture.root, 'github-env'), 'utf8'), /IOS_SIMULATOR_UDID=fake-udid/u);
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  }],
  ['Native startup iOS runtime capture rejects missing and unavailable runtimes before provisioning', async () => {
    const repo = join(import.meta.dirname, '../../..');
    const cases = [
      ['missing', '== Runtimes ==\niOS 18.5 (18.5 - 22F76) - com.apple.CoreSimulator.SimRuntime.iOS-18-5\n'],
      ['unavailable', '== Runtimes ==\niOS 18.6 (18.6 - 22G86) - com.apple.CoreSimulator.SimRuntime.iOS-18-6 (unavailable, runtime profile not found)\n'],
    ] as const;
    for (const [name, inventory] of cases) for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const fixture = await createIosRuntimeFixture(inventory);
      try {
        const source = await readFile(join(repo, path), 'utf8');
        const result = await runShellBlock(createIosBlock(source), repo, fixture.env);
        assert.notEqual(result.code, 0, `${path}: ${name}`);
        assert.equal(await readFile(join(fixture.runnerTemp, 'ios-runtime-inventory.txt'), 'utf8'), inventory);
        const selection = await readFile(join(fixture.runnerTemp, 'ios-runtime-selection.txt'), 'utf8');
        assert.match(selection, /^expected_platform_version=18\.6$/mu);
        assert.match(selection, /^inventory_line=$/mu);
        assert.match(selection, /^selected_identifier=$/mu);
        assert.match(selection, /^selected_version=$/mu);
        assert.match(selection, /^selected_build=$/mu);
        const requests = await readFile(fixture.xcrunLog, 'utf8');
        assert.equal(requests.includes('simctl create'), false, `${path}: ${name} provisioned a simulator`);
        await assert.rejects(readFile(join(fixture.root, 'owned')), /ENOENT/u);
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  }],
  ['Native startup diagnostic missing server, malformed frames, output and lifetime bounds never reconnect', async () => {
    for (const mode of ['missing', 'closure', 'malformed', 'overflow', 'lifetime', 'trailing']) {
      const root = await mkdtemp(join(tmpdir(), 'herdr-startup-'));
      const socket = new Socket();
      let connects = 0;
      const capture = new AndroidStartupLog(root, (() => { connects++; return socket; }) as unknown as typeof createConnection, 25);
      try {
        capture.start('emulator-5554');
        socket.emit('connect');
        if (mode === 'missing') socket.emit('error', new Error('ECONNREFUSED'));
        else {
          socket.emit('data', Buffer.from('OKAY'));
          socket.emit('data', Buffer.from('OKAY'));
          if (mode === 'closure') socket.emit('close');
          if (mode === 'malformed') socket.emit('data', frame(9, Buffer.alloc(0)));
          if (mode === 'overflow') socket.emit('data', Buffer.alloc(1_048_582));
          if (mode === 'trailing') socket.emit('data', Buffer.concat([frame(3, Buffer.from([0])), Buffer.from([1])]));
          if (mode === 'lifetime') await new Promise(resolve => setTimeout(resolve, 40));
        }
        await capture.waitForHandshake();
        await capture.finish();
        const result = JSON.parse(await readFile(join(root, 'android-startup-logcat.json'), 'utf8'));
        assert.equal(connects, 1, mode);
        assert.ok(result.error, mode);
        assert.ok(result.endedAt, mode);
        assert.equal(result.truncated, mode === 'overflow', mode);
        assert.ok(socket.destroyed, mode);
      } finally { await capture.finish(); await rm(root, { recursive: true, force: true }); }
    }
  }],
  ['Native startup actual Android preparation starts diagnostics before the first session and retains startup failure', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-startup-'));
    const savedAdb = process.env.ADB;
    const savedOwnership = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
    const start = AndroidStartupLog.prototype.start;
    const operations: string[] = [];
    AndroidStartupLog.prototype.start = function () { operations.push('diagnostic'); };
    try {
      await writeFile(join(root, 'owned'), 'android:emulator-5554');
      await writeFile(join(root, 'adb'), '#!/bin/sh\nif [ "$1" = devices ]; then printf "emulator-5554\\tdevice\\n"; fi\n', { mode: 0o700 });
      process.env.ADB = join(root, 'adb');
      process.env.MOBILE_DEVICE_OWNERSHIP_FILE = join(root, 'owned');
      const platform = new AndroidPlatform({ deviceId: 'emulator-5554', origin: 'https://example.test', setupUrl: 'https://example.test/setup', outputDir: root, appiumUrl: 'http://127.0.0.1:1', certificate: '/unused' });
      platform.driver.create = async () => { operations.push('first session'); throw new Error('startup session failure'); };
      platform.driver.close = async () => {};
      await assert.rejects(platform.startFreshDevice(), /startup session failure/u);
      await platform.stopOwnedResources();
      assert.deepEqual(operations, ['diagnostic', 'first session']);
      assert.ok(JSON.parse(await readFile(join(root, 'android-startup-logcat.json'), 'utf8')).endedAt);
    } finally {
      AndroidStartupLog.prototype.start = start;
      if (savedAdb === undefined) delete process.env.ADB; else process.env.ADB = savedAdb;
      if (savedOwnership === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE; else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = savedOwnership;
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['Native startup finalizes Android diagnostics when session teardown fails', async () => {
    const platform = new AndroidPlatform({ deviceId: 'emulator-5554', origin: 'https://example.test', setupUrl: 'https://example.test/setup', outputDir: '/unused', appiumUrl: 'http://127.0.0.1:1', certificate: '/unused' });
    let finalized = false;
    const internals = platform as unknown as { startupLog: { finish(): Promise<void> } };
    internals.startupLog = { async finish() { finalized = true; } };
    platform.driver.close = async () => { throw new Error('original teardown'); };
    await assert.rejects(platform.stopOwnedResources(), /original teardown/u);
    assert.ok(finalized);
  }],
  ['Native startup WDA status identity and absent managed evidence refuse fallback', async () => {
    const status = { value: { ready: true, state: 'success', build: { version: '16.12.1', productBundleIdentifier: 'com.facebook.WebDriverAgentRunner' }, os: { version: '18.6' } } };
    assert.ok(validWdaStatus(status, '18.6'));
    for (const invalid of [{}, { value: { ready: true } }, { value: { ...status.value, ready: false } }, { value: { ...status.value, build: { ...status.value.build, version: 'wrong' } } }]) assert.equal(validWdaStatus(invalid, '18.6'), false);
    assert.equal(validWdaStatus(status, '18.5'), false);
    const old = process.env.IOS_XCTEST_STATE_DIR;
    delete process.env.IOS_XCTEST_STATE_DIR;
    try { await assert.rejects(managedWdaCapabilities('82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40'), /missing IOS_XCTEST_STATE_DIR/u); }
    finally { if (old === undefined) delete process.env.IOS_XCTEST_STATE_DIR; else process.env.IOS_XCTEST_STATE_DIR = old; }
  }],
  ['Native startup both CI paths use the shared owner and preserve pre-session ordering', async () => {
    for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const source = await readFile(join(import.meta.dirname, '../../..', path), 'utf8');
      assert.equal(source.split('bun tests/mobile/support/ios-xctest.ts start').length - 1, 1);
      assert.equal(source.split('bun tests/mobile/support/ios-xctest.ts stop').length - 1, 1);
      assert.ok(!source.includes('simctl launch --terminate-running-process'));
      assert.ok(!source.includes("-name 'WebDriverAgentRunner_*.xctestrun' -print -quit"));
      assert.ok(source.includes('IOS_PLATFORM_VERSION=18.6'));
      assert.ok(source.includes('IPHONEOS_DEPLOYMENT_TARGET=18.5'));
      assert.ok(source.indexOf('name: Finalize owned XCTest diagnostics') < source.indexOf('name: Sanitize bounded diagnostics'));
    }
    const source = await readFile(join(import.meta.dirname, '../platforms/android.ts'), 'utf8');
    const start = source.slice(source.indexOf('async startFreshDevice()'), source.indexOf('async openSetupURL('));
    assert.ok(start.indexOf("requireOwnedDevice('android'") < start.indexOf('this.startupLog.start'));
    assert.ok(start.indexOf('this.startupLog.start') < start.indexOf('await this.startupLog.waitForHandshake()'));
    assert.ok(start.indexOf('await this.startupLog.waitForHandshake()') < start.indexOf('this.driver.create'));
    assert.ok(start.indexOf('await this.startupLog.waitForHandshake()') < start.indexOf("commandOutput(process.env.ADB"));
    const ios = await readFile(join(import.meta.dirname, '../platforms/ios.ts'), 'utf8');
    assert.ok(!ios.includes('usePreinstalledWDA'));
    assert.ok(ios.includes('...await managedWdaCapabilities(this.udid)'));
  }],
];

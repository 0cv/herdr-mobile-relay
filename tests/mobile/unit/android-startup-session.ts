import { strict as assert } from 'node:assert';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform } from '../platforms/android';
import { AndroidStartupLog } from '../support/android-startup-log';
import { PhaseBudget } from '../support/budget';
import { AppiumClient, WebDriverError } from '../support/webdriver';
import { androidSetupReverseLossTests } from './android-setup-reverse-loss';

type Mode = 'reconnect' | 'repeated' | 'timeout' | 'other' | 'active-session' | 'sticky-driver' | 'expired-budget' | 'restore-failure';

async function replay(mode: Mode): Promise<void> {
  const recorded = JSON.parse(await readFile(join(import.meta.dirname, 'fixtures/android-startup-adb-offline.json'), 'utf8'));
  const root = await mkdtemp(join(tmpdir(), 'android-startup-session-'));
  const saved = { adb: process.env.ADB, ownership: process.env.MOBILE_DEVICE_OWNERSHIP_FILE };
  const start = AndroidStartupLog.prototype.start;
  AndroidStartupLog.prototype.start = function () {};
  let now = 0;
  const budget = new PhaseBudget('startup-replay', { timeoutMs: 300_000, now: () => now });
  const trace: string[] = [];
  const log = join(root, 'adb.log');
  let chromeAttempts = 0;
  const calls = async () => (await readFile(log, 'utf8')).trim().split('\n');
  const offline = () => new WebDriverError({
    code: 'APPIUM_COMMAND', status: recorded.status, path: '/session', method: 'POST', durationMs: 777,
    selectedContext: 'NATIVE_APP', selectedWindow: '', message: recorded.value.message,
  });
  const client = new AppiumClient('http://startup.invalid', 5_000, async (input, init) => {
    const path = new URL(String(input)).pathname;
    if (init?.method === 'DELETE') {
      trace.push('close');
      return Response.json({ value: null });
    }
    assert.equal(path, '/session');
    const caps = JSON.parse(String(init?.body)).capabilities.alwaysMatch;
    if (caps.browserName !== 'Chrome') {
      trace.push('settings');
      return Response.json({ value: { sessionId: 'settings', capabilities: {} } });
    }
    trace.push('chrome');
    chromeAttempts++;
    if (mode === 'timeout') throw new DOMException('recorded request timed out', 'TimeoutError');
    if (mode === 'expired-budget') now = 300_001;
    if (mode === 'active-session') return Response.json({ value: { sessionId: 'ambiguous', capabilities: {} } });
    if (mode !== 'reconnect' || chromeAttempts === 1) {
      return Response.json(mode === 'other' ? { value: { error: 'unknown error', message: 'instrumentation crashed' } } : { value: recorded.value }, { status: recorded.status });
    }
    trace.push('chrome-ready');
    return Response.json({ value: { sessionId: 'chrome', capabilities: {} } });
  });
  client.setBudget(budget);
  const platform = new AndroidPlatform({
    deviceId: 'emulator-5554', origin: 'https://fixture.test', setupUrl: 'https://fixture.test/setup',
    outputDir: root, appiumUrl: 'http://startup.invalid', certificate: '/unused', budget,
  });
  Object.assign(platform, { driver: client });
  const internals = platform as unknown as {
    configureNativeSettings(): Promise<void>;
    installCertificate(): Promise<void>;
    readChromeProcess(): Promise<{ pid: string; startTime: string }>;
    verifyFixtureEndpoint(): Promise<void>;
    createChromeSession(attach: boolean): Promise<void>;
  };
  internals.configureNativeSettings = async () => { trace.push('native-settings'); };
  internals.installCertificate = async () => { trace.push('certificate'); };
  internals.readChromeProcess = async () => { trace.push('retain-process'); return { pid: '42', startTime: '56' }; };
  internals.verifyFixtureEndpoint = async () => { trace.push('endpoint'); };
  if (mode === 'active-session') {
    const create = client.create.bind(client);
    client.create = async options => {
      const result = await create(options);
      if (options.capabilities.browserName === 'Chrome') throw offline();
      return result;
    };
  }
  if (mode === 'sticky-driver') {
    const create = client.create.bind(client);
    const snapshot = client.snapshot.bind(client);
    client.create = async options => {
      try { return await create(options); } catch (error) {
        client.snapshot = () => ({ ...snapshot(), unusable: true, firstFatal: {
          code: 'APPIUM_TIMEOUT', path: '/session', method: 'POST', at: Date.now(), detail: 'prior fatal request',
        } });
        throw error;
      }
    };
  }
  try {
    await writeFile(join(root, 'owned'), 'android:emulator-5554');
    await writeFile(join(root, 'adb'), [
      '#!/bin/sh',
      `printf '%s\\n' "$*" >> '${log}'`,
      'if [ "$1" = devices ]; then printf "emulator-5554\\tdevice\\n"; fi',
      'if [ "$3" = reverse ] && [ "$4" = --list ]; then printf "host-17 tcp:44323 tcp:44323\\nhost-17 tcp:33251 tcp:33251\\n"; fi',
      mode === 'restore-failure' ? 'if [ "$3" = reverse ] && [ "$4" = tcp:44323 ]; then exit 1; fi' : '',
      '',
    ].join('\n'), { mode: 0o700 });
    process.env.ADB = join(root, 'adb');
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = join(root, 'owned');
    if (mode === 'reconnect') {
      await platform.startFreshDevice();
      assert.equal(chromeAttempts, 2);
      assert.deepEqual(trace, ['settings', 'native-settings', 'certificate', 'close', 'chrome', 'chrome', 'chrome-ready', 'native-settings', 'retain-process', 'endpoint']);
      assert.deepEqual((await calls()).slice(-3), [
        '-s emulator-5554 wait-for-device',
        '-s emulator-5554 reverse tcp:44323 tcp:44323',
        '-s emulator-5554 reverse tcp:33251 tcp:33251',
      ]);
      assert.equal(budget.recoveryCount, 1);
      // Cold reattachment belongs to the measured scenario: it must not use setup recovery.
      await client.close();
      const before = await calls();
      client.create = async () => { chromeAttempts++; throw offline(); };
      await assert.rejects(internals.createChromeSession(true), /device offline/u);
      assert.equal(chromeAttempts, 3);
      assert.deepEqual(await calls(), before);
      return;
    }
    await assert.rejects(platform.startFreshDevice());
    assert.equal(chromeAttempts, mode === 'repeated' ? 2 : 1);
    assert.ok(!trace.includes('retain-process'));
    assert.ok(!trace.includes('endpoint'));
    const restored = (await calls()).filter(call => call === '-s emulator-5554 reverse tcp:44323 tcp:44323');
    assert.equal(restored.length, mode === 'repeated' || mode === 'restore-failure' ? 1 : 0);
    if (mode === 'timeout') assert.equal(client.snapshot().firstFatal?.code, 'APPIUM_TIMEOUT');
    const before = await calls();
    await assert.rejects(platform.startFreshDevice());
    assert.deepEqual(await calls(), before);
  } finally {
    try {
      const stopping = platform.stopOwnedResources();
      if (mode === 'timeout') {
        const before = [...trace];
        const fatal = client.snapshot().firstFatal;
        await assert.rejects(stopping, /APPIUM_SESSION_UNUSABLE/u);
        assert.equal(platform.stopOwnedResources(), stopping);
        await assert.rejects(platform.stopOwnedResources(), /APPIUM_SESSION_UNUSABLE/u);
        assert.deepEqual(trace, before);
        assert.equal(client.snapshot().firstFatal, fatal);
      } else await stopping;
    } finally {
      AndroidStartupLog.prototype.start = start;
      if (saved.adb === undefined) delete process.env.ADB; else process.env.ADB = saved.adb;
      if (saved.ownership === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE; else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = saved.ownership;
      await rm(root, { recursive: true, force: true });
    }
  }
}

export const androidStartupSessionTests: Array<[string, () => Promise<void>]> = (
  ['reconnect', 'repeated', 'timeout', 'other', 'active-session', 'sticky-driver', 'expired-budget', 'restore-failure'] as const
).map<[string, () => Promise<void>]>(mode => [`Native startup Android Chrome session ${mode} preserves startup recovery boundaries`, () => replay(mode)]).concat(androidSetupReverseLossTests);

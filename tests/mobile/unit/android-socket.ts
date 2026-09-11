import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform, androidChromeCapabilities } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';
import { PhaseBudget } from '../support/budget';
import type { DiagnosticRecorder } from '../support/diagnostics';

export async function runAndroidSocketRegressions(): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'android-socket-'));
  const previousAdb = process.env.ADB;
  const adb = join(root, 'adb');
  const state = join(root, 'state');
  await writeFile(adb, `#!/bin/sh\nexec /bin/cat '${state}'\n`, { mode: 0o755 });
  process.env.ADB = adb;
  try {
    for (const fault of ['search', 'browser', 'other', 'origin', 'document', 'native-failure', 'missing', 'empty-valid']) {
      const foreground = async (pid: string, pkg = 'com.android.chrome', activity = 'org.chromium.chrome.browser.webapps.WebappActivity') => {
        await writeFile(state, `mResumedActivity: ActivityRecord{123 u0 ${pkg}/${activity} pid=${pid}}\n`);
      };
      await foreground('123');
      let selected = '';
      let broken = false;
      const calls: string[] = [];
      const windows: string[] = [];
      const metadataArgs: unknown[] = [];
      const socketMetadata = [{ proc: '@chrome_devtools_remote', webview: 'CHROMIUM', webviewName: 'CHROMIUM', info: { Browser: 'Chrome/123' }, pages: [{ id: 'page' }] }];
      const response = (value: unknown) => new Response(JSON.stringify({ value, sessionId: 'socket-session' }));
      const client = new AppiumClient('http://fake.test', 2_000, async (input, init) => {
        const path = new URL(String(input)).pathname;
        const body = init?.body ? JSON.parse(String(init.body)) : {};
        calls.push(path);
        if (path === '/session') return response({});
        if (path.endsWith('/contexts')) return response(['NATIVE_APP', 'CHROMIUM']);
        if (path.endsWith('/window/handles')) return response(['browser', 'installed', 'other-good']);
        if (path.endsWith('/window')) { selected = body.handle; windows.push(selected); return response(null); }
        if (path.endsWith('/url')) return response(broken && fault === 'origin' ? 'https://wrong.test/' : 'https://fixture.test/');
        if (path.endsWith('/execute/sync')) {
          if (body.script === 'mobile: getContexts') {
            metadataArgs.push(body.args);
            return response(broken && ['missing', 'empty-valid'].includes(fault) ? [] : socketMetadata);
          }
          return response({ origin: 'https://fixture.test', standalone: selected !== 'browser' && !(broken && ['document', 'missing'].includes(fault)), provider: selected === 'browser' ? 'browser' : 'android-standalone' });
        }
        return response(null);
      });
      await client.create({ capabilities: androidChromeCapabilities('emulator-5554', true) });
      const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: root,
        certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget: new PhaseBudget('socket-test', { timeoutMs: 10_000, recoveryLimit: 1 }) });
      Object.assign(platform, { driver: client, installedPackage: 'com.android.chrome', installedTarget: { packageName: 'com.android.chrome', activity: 'org.chromium.chrome.browser.webapps.WebappActivity' } });
      const observedMetadata = () => (platform as unknown as { diagnostics: DiagnosticRecorder }).diagnostics.snapshot()
        .filter(event => event.operation === 'context-metadata-observed');
      await platform.attachToInstalledView(2_000);
      assert.deepEqual(metadataArgs, [{}]);
      assert.deepEqual(observedMetadata().map(event => event.detail), [socketMetadata]);
      console.log(JSON.stringify({ fault, event: observedMetadata()[0] }));
      assert.deepEqual(windows, ['browser', 'installed']);
      await foreground('456');
      assert.equal((await platform.readRunningIdentity()).nativePid, '456');
      broken = true;
      if (fault === 'search') await foreground('789', 'com.google.android.googlequicksearchbox');
      if (fault === 'browser') await foreground('789', 'com.android.chrome', 'com.google.android.apps.chrome.Main');
      if (fault === 'other') await foreground('789', 'com.example.other');
      if (fault === 'native-failure') await rm(state);
      const count = calls.length;
      const windowCount = windows.length;
      if (fault === 'empty-valid') {
        await platform.attachToInstalledView(700);
        assert.deepEqual(observedMetadata().at(-1)?.detail, []);
        assert.deepEqual(windows.slice(windowCount), ['installed']);
        assert.ok(metadataArgs.every(args => JSON.stringify(args) === '{}'));
        continue;
      }
      await assert.rejects(platform.attachToInstalledView(700), fault === 'native-failure' ? /ANDROID_CONTEXT/ : /ANDROID_CONTEXT_OWNERSHIP/);
      if (fault === 'missing') {
        assert.deepEqual(observedMetadata().at(-1)?.detail, []);
        console.log(JSON.stringify({ fault, event: observedMetadata().at(-1) }));
      }
      assert.ok(metadataArgs.every(args => JSON.stringify(args) === '{}'));
      if (['search', 'browser', 'other', 'native-failure'].includes(fault)) assert.equal(calls.length, count);
      if (['search', 'browser', 'other', 'origin', 'document', 'missing'].includes(fault)) {
        const after = calls.length;
        await assert.rejects(platform.attachToInstalledView(700), /ANDROID_CONTEXT_OWNERSHIP/);
        assert.equal(calls.length, after);
      }
      if (['origin', 'document', 'missing'].includes(fault)) assert.deepEqual(windows.slice(windowCount), ['installed']);
    }
  } finally {
    if (previousAdb === undefined) delete process.env.ADB;
    else process.env.ADB = previousAdb;
    await rm(root, { recursive: true, force: true });
  }
}

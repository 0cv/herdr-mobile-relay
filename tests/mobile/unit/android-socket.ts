import assert from 'node:assert/strict';
import { AndroidPlatform, androidChromeCapabilities } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';
import { PhaseBudget } from '../support/budget';
import { retainedFixture } from './android-retained-fixture';

export async function runAndroidSocketRegressions(): Promise<void> {
  for (const fault of ['search', 'browser', 'other', 'origin', 'document', 'native-failure', 'missing', 'empty-valid']) {
    const bootstrap = '2E26E8C2C4CFF68B69AA865CD8132F98';
    const installed = '753D4398F5ABC414D3DAABBF0B329743';
    const startedAt = Date.now();
    let selected = bootstrap;
    let broken = false;
    const calls: string[] = [];
    const windows: string[] = [];
    const metadataArgs: unknown[] = [];
    const response = (value: unknown) => Response.json({ value, sessionId: 'original' });
    const client = new AppiumClient('http://fake.test', 30_000, async (input, init) => {
      const path = new URL(String(input)).pathname;
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      calls.push(path);
      if (path === '/session') return response({});
      if (path.endsWith('/window')) {
        if (init?.method === 'POST') { selected = body.handle; windows.push(selected); }
        return response(selected);
      }
      if (path.endsWith('/execute/sync')) {
        if (body.script === 'mobile: getContexts') { metadataArgs.push(body.args); return response([]); }
        if (body.script === 'mobile: inspectRetainedChromeTargets') {
          const result = retainedFixture([bootstrap, installed], selected, installed, startedAt);
          if (broken) {
            if (fault === 'native-failure') return Response.json({ value: { error: 'unknown error', message: 'native observation unavailable' } }, { status: 500 });
            if (['search', 'browser', 'other'].includes(fault)) {
              result.after.native.pid = '789';
              result.after.native.activity = fault === 'browser' ? 'com.google.android.apps.chrome.Main' : 'other.Activity';
            }
            if (fault === 'origin') result.after.document.origin = 'https://wrong.test';
            if (fault === 'document') result.after.document.standalone = false;
            if (fault === 'missing') result.observations = [];
          }
          return response(result);
        }
        return response({ navigationId: String(startedAt - 1), origin: 'https://fixture.test', standalone: true, provider: 'android-standalone' });
      }
      return response(null);
    });
    await client.create({ capabilities: androidChromeCapabilities('emulator-5554', true) });
    const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: '/tmp',
      certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget: new PhaseBudget('socket-test', { timeoutMs: 90_000, recoveryLimit: 0 }) });
    Object.assign(platform, { driver: client, installedPackage: 'com.android.chrome', installedTarget: { packageName: 'com.android.chrome', shortcut: { scope: 'https://fixture.test/' } },
      retainedOwner: { driver: client, assertSession: client.retainSessionOwner(), ...retainedFixture([bootstrap, installed], bootstrap, installed, startedAt).original } });
    await platform.attachToInstalledView();
    assert.deepEqual(metadataArgs, []);
    assert.deepEqual(windows, [installed]);
    assert.equal((await platform.readRunningIdentity()).nativePid, '123');
    broken = true;
    const windowCount = windows.length;
    if (fault === 'empty-valid') await platform.attachToInstalledView();
    else {
      await assert.rejects(platform.attachToInstalledView(), /ANDROID_CONTEXT_OWNERSHIP/u);
      const after = calls.length;
      await assert.rejects(platform.attachToInstalledView(), /ANDROID_CONTEXT_OWNERSHIP/u);
      assert.equal(calls.length, after);
    }
    assert.deepEqual(metadataArgs, []);
    assert.deepEqual(windows.slice(windowCount), []);
    console.log(`PASS retained socket ownership ${fault}`);
  }
}

import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';
import { retainedFixture } from './android-retained-fixture';

export async function runAndroidRetainedResponseRegressions(): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'android-retained-responses-'));
  const saved = process.env.ADB;
  const savedOwnership = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
  const adb = join(root, 'adb');
  await writeFile(adb, '#!/bin/sh\nprintf "%s\\n" "ResumedActivity: ActivityRecord{x u0 com.android.chrome/org.chromium.chrome.browser.webapps.WebappActivity pid=123}"\n', { mode: 0o700 });
  process.env.ADB = adb;
  try {
    const cases = [
      ...['identity', 'completion', 'agent', 'dialog'].flatMap(operation => ['http', 'session', 'transport'].map(fault => ({ operation, fault }))),
      ...['contexts', 'handles', 'window', 'url', 'proof'].flatMap(operation => ['missing', 'null', 'object', 'number', 'array-item', 'empty-array'].map(fault => ({ operation, fault }))),
    ];
    for (const { operation, fault } of cases) {
      let failed = false;
      let armed = false;
      let calls = 0;
      const handle = '753D4398F5ABC414D3DAABBF0B329743';
      const startedAt = Date.now();
      let requestsAtFault = 0;
      const response = (value: unknown, status = 200) => Response.json({ value, sessionId: 'original' }, { status });
      const client = new AppiumClient('http://responses.invalid', 2_000, async (input, init) => {
        calls++;
        const path = new URL(String(input)).pathname.replace('/session/original', '');
        const body = init?.body ? JSON.parse(String(init.body)) : {};
        if (path === '/session') return response({});
        const inspection = body.script === 'mobile: inspectRetainedChromeTargets';
        const shape = ['contexts', 'handles', 'window', 'url', 'proof'].includes(operation);
        const target = shape ? inspection
          : operation === 'agent' ? path === '/url'
            : operation === 'dialog' ? path === '/elements'
              : path === '/execute/sync' && !inspection && body.script !== 'mobile: getContexts';
        if (armed && !failed && target) {
          failed = true;
          requestsAtFault = calls;
          if (fault === 'transport') throw new Error('connection refused after attachment');
          if (fault === 'http') return new Response('upstream refusal', { status: 500 });
          if (fault === 'session') return response({ error: 'invalid session id', message: 'original session disappeared' }, 500);
          const malformed = fault === 'missing' ? undefined : fault === 'null' ? null : fault === 'object' ? {} : fault === 'number' ? 123 : fault === 'empty-array' ? [] : [null];
          const result = retainedFixture([handle], handle, handle, startedAt);
          const targetObject: any = operation === 'contexts' ? result : operation === 'proof' ? result.before : result.after;
          const key = operation === 'contexts' ? 'targets' : operation === 'handles' ? 'handles' : operation === 'window' ? 'selectedHandle' : operation === 'url' ? 'document' : 'document';
          targetObject[key] = malformed;
          return response(result);
        }
        if (inspection) return response(retainedFixture([handle], handle, handle, startedAt));
        if (path === '/contexts') return response(['NATIVE_APP', 'CHROMIUM']);
        if (path === '/window/handles') return response(['installed']);
        if (path === '/window') return response('installed');
        if (path === '/url') return response('https://fixture.test/');
        if (path === '/execute/sync') return response(body.script === 'mobile: getContexts' ? [] : { origin: 'https://fixture.test', standalone: true, provider: 'android-standalone' });
        if (path === '/elements') return response([]);
        return response(null);
      });
      await client.create({ capabilities: {} });
      const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://responses.invalid', outputDir: root, certificate: '', setupUrl: '', deviceId: 'emulator-5554' });
      Object.assign(platform, { driver: client, installedTarget: { packageName: 'com.android.chrome', shortcut: { scope: 'https://fixture.test/' } },
        retainedOwner: { driver: client, assertSession: client.retainSessionOwner(), ...retainedFixture([handle], handle, handle, startedAt).original } });
      await platform.attachToInstalledView();
      armed = true;
      if (operation === 'agent') {
        const owned = join(root, 'owned');
        await writeFile(owned, 'android:emulator-5554\n');
        process.env.MOBILE_DEVICE_OWNERSHIP_FILE = owned;
      }
      const action = operation === 'identity' ? () => platform.readRunningIdentity()
        : operation === 'completion' ? () => platform.readUpdateCompletion()
          : operation === 'agent' ? () => platform.openFixtureAgent('fixture')
            : operation === 'dialog' ? () => platform.clickDialogText('dialog', 'Confirm')
              : () => platform.attachToInstalledView();
      let first: unknown;
      await assert.rejects(action, error => { first = error; return /ANDROID_CONTEXT_OWNERSHIP/u.test(String(error)); });
      assert.ok(failed, `${operation}/${fault}`);
      const count = calls;
      assert.equal(count, requestsAtFault, 'no discovery request may follow a refusal or missing value');
      await assert.rejects(action, error => error === first);
      await assert.rejects(() => platform.readRunningIdentity(), error => error === first);
      await assert.rejects(() => platform.readUpdateCompletion(), error => error === first);
      await assert.rejects(() => platform.relaunchInstalledApp(), error => error === first);
      await assert.rejects(() => client.currentUrl(), error => error === first);
      assert.equal(calls, count, 'healthy follow-up responses must never be requested');
      console.log(`PASS retained response ${operation}/${fault}`);
    }
  } finally {
    if (saved === undefined) delete process.env.ADB; else process.env.ADB = saved;
    if (savedOwnership === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE; else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = savedOwnership;
    await rm(root, { recursive: true, force: true });
  }
}

if (import.meta.main) await runAndroidRetainedResponseRegressions();

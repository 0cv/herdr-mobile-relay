import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';

export async function runAndroidAttachmentRefusalRegressions(): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'android-attachment-refusal-'));
  const previousAdb = process.env.ADB;
  const adb = join(root, 'adb');
  await writeFile(adb, '#!/bin/sh\nprintf "%s\\n" "mResumedActivity: ActivityRecord{123 u0 com.android.chrome/org.chromium.chrome.browser.webapps.WebappActivity pid=123}"\n', { mode: 0o700 });
  process.env.ADB = adb;
  try {
    for (const failurePath of ['/contexts', '/execute/sync', '/context', '/window/handles', '/window', '/url', 'document']) {
      for (const failureKind of ['refusal', 'session', 'window', 'transport', 'malformed']) {
        const calls: string[] = [];
        let failed = false;
        let active = 0;
        let maxActive = 0;
        const response = (value: unknown, status = 200) => new Response(JSON.stringify({ value, sessionId: 'original' }), { status });
        const client = new AppiumClient('http://attachment.invalid', 2_000, async (input, init) => {
          const path = new URL(String(input)).pathname.replace('/session/original', '');
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          calls.push(path);
          active++;
          maxActive = Math.max(maxActive, active);
          try {
            if (path === '/session') return response({});
            const operation = path === '/execute/sync' && body.script !== 'mobile: getContexts' ? 'document' : path;
            if (!failed && operation === failurePath) {
              failed = true;
              if (failureKind === 'transport') throw new Error('connection lost');
              if (failureKind === 'malformed') return new Response('{', { status: 200 });
              const error = failureKind === 'session' ? 'invalid session id' : failureKind === 'window' ? 'no such window' : 'unknown error';
              return response({ error, message: 'retained producer refused ownership' }, 500);
            }
            if (path === '/contexts') return response(['NATIVE_APP', 'CHROMIUM', 'WEBVIEW_alternate']);
            if (path === '/window/handles') return response(['original-window']);
            if (path === '/url') return response('https://fixture.test/');
            if (path === '/execute/sync') return response(body.script === 'mobile: getContexts' ? [] : {
              origin: 'https://fixture.test', standalone: true, provider: 'android-standalone',
            });
            return response(null);
          } finally {
            active--;
          }
        });
        await client.create({ capabilities: {} });
        const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://attachment.invalid', outputDir: root,
          certificate: '', setupUrl: '', deviceId: 'emulator-5554' });
        Object.assign(platform, { driver: client, installedPackage: 'com.android.chrome', installedTarget: { packageName: 'com.android.chrome' } });
        let first: unknown;
        await assert.rejects(() => platform.attachToInstalledView(2_000), error => {
          first = error;
          return /ANDROID_CONTEXT_OWNERSHIP/u.test(String(error));
        });
        assert.equal(failed, true, `${failurePath}/${failureKind}`);
        const count = calls.length;
        await assert.rejects(() => platform.attachToInstalledView(2_000), error => error === first);
        await assert.rejects(() => platform.readRunningIdentity(), error => error === first);
        await assert.rejects(() => platform.launchInstalledApp(), error => error === first);
        await assert.rejects(() => platform.relaunchInstalledApp(), error => error === first);
        assert.equal(calls.length, count);
        assert.equal(calls.filter(path => path === '/session').length, 1);
        assert.equal(calls.filter(path => path === '/contexts').length, 1);
        assert.equal(maxActive, 1);
        assert.equal(active, 0);
      }
    }
  } finally {
    if (previousAdb === undefined) delete process.env.ADB;
    else process.env.ADB = previousAdb;
    await rm(root, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  await runAndroidAttachmentRefusalRegressions();
  console.log('Android attachment refusal regressions passed (35 cases)');
}

import assert from 'node:assert/strict';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {waitForPhoneCompletion, assertCandidateFailureObserved} from '../run';
import {QualificationFailureLatch} from '../support/oracle';
import type {BundleSet} from '../support/artifacts';
import { AndroidPlatform } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';
import { PhaseBudget } from '../support/budget';
import { retainedFixture } from './android-retained-fixture';
import type { RetainedInspectionResult } from '../android-appium/retained-inspection.cjs';

export async function runAndroidRetainedInspectionRegressions(): Promise<void> {
  const bootstrap = '2E26E8C2C4CFF68B69AA865CD8132F98';
  const installed = '753D4398F5ABC414D3DAABBF0B329743';
  const changes: Record<string, (r: RetainedInspectionResult) => void> = {
    kind: r => { Object.assign(r, { kind: 'cached' }); },
    'original-pid': r => { r.original.pid = '124'; },
    'original-start': r => { r.original.startTime = '457'; },
    'original-boot': r => { r.original.bootId = '22222222-1111-1111-1111-111111111111'; },
    'kernel-mode': r => { r.original.kernelCapability.mode = 'disabled'; },
    'kernel-source': r => { Object.assign(r.original.kernelCapability, {source: '/tmp/config.gz'}); },
    'kernel-digest': r => { r.original.kernelCapability.sha256 = '0'.repeat(64); },
    'kernel-bounds': r => { Object.assign(r.original.kernelCapability, {compressedLimit: 262145}); },
    'kernel-size': r => { r.original.kernelCapability.configBytes++; },
    'kernel-boot': r => { r.original.kernelCapability.bootId = '22222222-1111-1111-1111-111111111111'; },
    'kernel-time': r => { r.original.kernelCapability.acquiredFinishedAt = Date.now() + 10_000; },
    'kernel-snapshot': r => { r.after.native.kernelCapability.mode = 'disabled'; },
    'kernel-cache-time': r => { r.after.native.kernelCapability.acquiredStartedAt++; },
    'original-namespace': r => { Object.assign(r.original, { namespace: 'unknown' }); },
    'original-session': r => { r.original.sessionId = 'replacement'; },
    'native-pid': r => { r.after.native.pid = '124'; },
    'native-start': r => { r.before.nativeBefore.startTime = '457'; },
    'native-boot': r => { r.before.native.bootId = '22222222-1111-1111-1111-111111111111'; },
    'native-namespace': r => { Object.assign(r.after.nativeBefore, { namespace: 'unknown' }); },
    'chrome-main': r => { r.after.native.activity = 'com.google.android.apps.chrome.Main'; r.after.native.provider = 'browser'; },
    'native-provider': r => { r.before.nativeBefore.provider = 'browser'; },
    'current-document': r => { r.after.document.backendNodeId++; },
    'current-provider': r => { r.before.document.provider = 'browser'; },
    'current-window': r => { r.selectedHandle = bootstrap; r.before.selectedHandle = bootstrap; r.after.selectedHandle = bootstrap; },
    second: r => { r.observations[0].document.standalone = true; r.observations[0].document.provider = 'android-standalone'; },
    unknown: r => { r.targets[0].type = 'unknown'; },
    iframe: r => { r.targets[0].type = 'iframe'; },
    'background-page': r => { r.targets[0].type = 'background_page'; },
    'malformed-url': r => { r.observations[0].document.href = 'invalid'; },
    'duplicate-observation': r => { r.observations[1] = r.observations[0]; },
    missing: r => { r.observations.pop(); },
    duplicate: r => { r.targets.push(r.targets[0]); },
    malformed: r => { Object.assign(r.observations[0], { document: null }); },
    replaced: r => { r.targets[1].targetId = 'A'.repeat(32); },
    'association-pid': r => { r.processAssociation.after.pid++; },
    'association-connection': r => { r.processAssociation.after.connectionId = 'different'; },
    'association-order': r => { r.processAssociation.after.requestId = 1; },
    'association-time': r => { r.processAssociation.before.startedAt = 1; },
    'stale-bound': r => { r.before.startedAt = 1; },
    'endpoint-drift': r => { r.after.endpoint.port++; },
    'forward-drift': r => { r.after.forward.inode = '43'; },
    'document-scope': r => { r.observations[1].document.href = 'https://wrong.test/'; r.observations[1].document.origin = 'https://wrong.test'; },
  };
  for (const scenario of [...Object.keys(changes), 'route-error', 'route-timeout', 'overlap', 'fresh-navigation', 'service-worker', 'mutation-postcheck', 'parent-admission', 'identity-document-race', 'completion-document-race', 'preference-native-race', 'agent-native-race', 'composer-native-race', 'completion-parent-admission', 'completion-parent-expiry', 'failure-parent-admission', 'failure-parent-expiry', 'composer-type-native-race']) {
    let armed = false;
    let calls = 0;
    let selected = bootstrap;
    let href = 'https://fixture.test/';
    let mutations = 0;
    let readCalls = 0;
    let elapsed = 0;
    const clock = Date.now;
    Date.now = () => clock() + elapsed;
    try {
    let timeOrigin = Date.now() - 10;
    const switches: string[] = [];
    const startedAt = Date.now();
    let pending: Promise<Response> | undefined;
    const client = new AppiumClient('http://inspection.invalid', 30_000, async (input, init) => {
      calls++;
      const path = new URL(String(input)).pathname.replace('/session/original', '');
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      const response = (value: unknown) => Response.json({ value, sessionId: 'original' });
      if (path === '/session') return response({});
      if (path === '/window' && init?.method === 'POST') { selected = body.handle; switches.push(selected); return response(null); }
      if (path === '/url' && init?.method === 'GET') return response(href);
      if (path === '/url' && init?.method === 'POST') { href = body.url; timeOrigin++; mutations++; return response(null); }
      if (path.endsWith('/click') || path.endsWith('/value')) { mutations++; return response(null); }
      if (path === '/element') return response({ 'element-6066-11e4-a52e-4f735466cecf': 'settings' });
      if (path === '/elements') return response([{ 'element-6066-11e4-a52e-4f735466cecf': 'settings' }]);
      if (path.endsWith('/rect')) return response({ x: 0, y: 0, width: 100, height: 100 });
      if (path === '/execute/sync') {
        if (body.script !== 'mobile: inspectRetainedChromeTargets') {
          readCalls++;
          const value = { navigationId: String(timeOrigin), origin: 'https://fixture.test', standalone: true, provider: 'android-standalone', phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true, rawPlanPresent: true };
          if (armed && ['identity-document-race', 'completion-document-race'].includes(scenario)) { timeOrigin++; mutations++; }
          if (armed && scenario === 'preference-native-race') mutations++;
          if (armed && ['completion-parent-expiry', 'failure-parent-expiry'].includes(scenario)) elapsed += 15_000;
          return response(scenario === 'preference-native-race' ? 'state' : value);
        }
        if (armed && ['completion-parent-expiry', 'failure-parent-expiry'].includes(scenario)) elapsed += 9_000;
        assert.deepEqual(Object.keys(body.args[0]), ['deadline']);
        const result = retainedFixture([bootstrap, installed], selected, installed, startedAt);
        for (const d of [result.observations[1].document, ...(selected === installed ? [result.before.document, result.after.document] : [])]) {
          d.href = href; d.timeOrigin = timeOrigin; d.backendNodeId += mutations;
        }
        result.targets[1].url = href;
        if (scenario === 'service-worker') result.targets.push({ targetId: 'service-worker', type: 'service_worker', title: '', url: 'https://fixture.test/sw.js' });
        if (['mutation-postcheck', 'preference-native-race', 'agent-native-race', 'composer-native-race'].includes(scenario) && mutations) result.after.native.activity = 'com.google.android.apps.chrome.Main';
        if (scenario === 'composer-type-native-race' && mutations === 2) result.after.native.activity = 'com.google.android.apps.chrome.Main';
        if (armed && changes[scenario]) changes[scenario](result);
        if (armed && scenario === 'route-error') return Response.json({ value: { error: 'unknown error', message: 'secret endpoint must not be projected' } }, { status: 500 });
        if (armed && scenario === 'route-timeout') {
          pending = new Promise<Response>(resolve => init?.signal?.addEventListener('abort', () => setTimeout(() => resolve(response(result)), 20), { once: true }));
          return await pending;
        }
        if (armed && scenario === 'overlap') await new Promise(resolve => setTimeout(resolve, 20));
        return response(result);
      }
      return response(null);
    });
    await client.create({ capabilities: {} });
    const budget = new PhaseBudget('inspection-regression', { timeoutMs: 90_000, recoveryLimit: 0 });
    client.setBudget(budget);
    const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://inspection.invalid', outputDir: '/tmp', certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget });
    Object.assign(platform, { driver: client, installedPackage: 'com.android.chrome', installedTarget: { packageName: 'com.android.chrome', shortcut: { scope: 'https://fixture.test/' } },
      retainedOwner: { driver: client, assertSession: client.retainSessionOwner(), ...retainedFixture([bootstrap, installed], bootstrap, installed, startedAt).original } });
    await platform.attachToInstalledView();
    assert.deepEqual(switches, [installed]);
    armed = true;
    if (scenario === 'fresh-navigation' || scenario === 'service-worker') {
      await platform.readRunningIdentity();
      await platform.clickWebText('Settings');
      await platform.openSetupURLInInstalledApp('https://fixture.test/pair#invitation');
      await platform.readRunningIdentity();
      assert.deepEqual(switches, [installed]);
    } else {
      let first: unknown;
      if (scenario === 'overlap') {
        const results = await Promise.allSettled([platform.attachToInstalledView(), platform.attachToInstalledView()]);
        assert.ok(results.every(r => r.status === 'rejected'));
        first = (results[0] as PromiseRejectedResult).reason;
      } else {
        const fixture = await mkdtemp('/tmp/herdr-adapter-qualification.');
        const previousMarker = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
        process.env.MOBILE_DEVICE_OWNERSHIP_FILE = `${fixture}/owned`;
        await writeFile(process.env.MOBILE_DEVICE_OWNERSHIP_FILE, 'android:emulator-5554');
        try {
          const beforeCalls = calls;
          await assert.rejects(async () => {
            if (scenario === 'identity-document-race') return platform.readRunningIdentity();
            if (scenario === 'completion-document-race') return platform.readUpdateCompletion();
            if (scenario === 'preference-native-race') return platform.preferenceValue();
            if (scenario === 'agent-native-race') return platform.openFixtureAgent('fixture');
            if (scenario === 'composer-native-race' || scenario === 'composer-type-native-race') return platform.showKeyboardOnComposer();
            if (scenario.startsWith('failure-parent-')) {
              const fetch = globalThis.fetch;
              globalThis.fetch = (async () => Response.json({requests: [{path: '/app.js', fault: 'fail', fault_id: 'owned', fault_generation: '1'}], faults: [{path: '/app.js', kind: 'fail', id: 'owned', generation: '1', remaining: 1}]})) as typeof fetch;
              try {
                return await assertCandidateFailureObserved(platform, {} as BundleSet['candidate']['identity'], 'https://fixture.test',
                  {identity: {version: '0.21.0'}} as BundleSet['baselines'][number], new Set(),
                  {app_url: '', relay_urls: [], setup_urls: [], control_url: 'http://fixture.invalid', control_secret: '', ca_certificate: '', old_release: '', candidate_release: ''},
                  '/app.js', 'fail', 'owned', '1', new QualificationFailureLatch(),
                  new PhaseBudget('failure-parent', {timeoutMs: scenario.endsWith('admission') ? 47_000 : 60_000}));
              } finally { globalThis.fetch = fetch; }
            }
            if (scenario.startsWith('completion-parent-')) return waitForPhoneCompletion(platform,
              {identity: {version: '0.21.0'}} as BundleSet['baselines'][number], new Set(), new QualificationFailureLatch(),
              new PhaseBudget('completion-parent', {timeoutMs: scenario.endsWith('admission') ? 23_000 : 30_000}));
            return scenario === 'mutation-postcheck' ? platform.openSetupURLInInstalledApp('https://fixture.test/pair')
              : platform.attachToInstalledView(scenario === 'parent-admission' ? 10_000 : 30_000);
          }, error => { first = error; return true; });
          if (scenario.endsWith('parent-admission')) assert.equal(calls, beforeCalls);
          if (scenario === 'failure-parent-expiry') assert.equal(readCalls, 2);
          if (scenario === 'composer-type-native-race') assert.equal(mutations, 2);
          if (scenario === 'completion-parent-expiry') assert.equal(readCalls, 1);
          if (['mutation-postcheck', 'identity-document-race', 'completion-document-race', 'preference-native-race', 'agent-native-race', 'composer-native-race'].includes(scenario)) assert.equal(mutations, 1);
        } finally {
          if (previousMarker === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
          else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = previousMarker;
          await rm(fixture, {recursive: true, force: true});
        }
      }
      await pending;
      if (scenario.startsWith('failure-parent-')) await assert.rejects(platform.attachToInstalledView(), error => { first = error; return true; });
      const count = calls;
      await assert.rejects(() => platform.attachToInstalledView(), error => error === first);
      await assert.rejects(() => client.screenshot(), error => error === first);
      await assert.rejects(() => client.create({ capabilities: {} }), error => error === first);
      assert.equal(calls, count);
      assert.deepEqual(switches, [installed]);
    }
    const diagnostic = JSON.stringify(platform.evidenceSnapshot());
    assert.equal(diagnostic.includes('/devtools/browser/'), false);
    assert.equal(diagnostic.includes('backend-original'), false);
    assert.equal(diagnostic.includes('secret endpoint must not be projected'), false);
    console.log(`PASS retained inspection ${scenario}`);
    } finally { Date.now = clock; }
  }
}

if (import.meta.main) await runAndroidRetainedInspectionRegressions();

import assert from 'node:assert/strict';
import { execFileSync, type ChildProcess } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { createHash } from 'node:crypto';
import { readFile, mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { setTimeout as wait } from 'node:timers/promises';
import { IOSPlatform, iosOpenURLProcessEvidence, nativeActionListEvidence } from '../platforms/ios';
import { AppiumClient, isRetryableElementLookupError, type WebDriverSnapshot } from '../support/webdriver';
import { collectIOSLaunchReceipt, IOSLaunchObservation, IOS_LAUNCH_LIMITS, IOS_LAUNCH_PREDICATE, iosLaunchQuery, iosReceiptError, type IOSLaunchFailure, type IOSReceiptClock, type IOSReceiptHooks } from '../support/ios-launch-receipt';
import { PhaseBudget } from '../support/budget';
import { writeSanitizedJson } from '../support/diagnostics';
import { CommandError, command } from '../support/process';
import recorded from './fixtures/ios/publication.json';
import recordedConfirmation from './fixtures/ios/ios-confirmation-recorded.json';
import recordedIteration13 from './fixtures/ios/ios-iteration13-recorded.json';

const origin = 'https://localhost:52101';
const fixtureDir = fileURLToPath(new URL('./fixtures/ios/', import.meta.url));
const outputRoot = process.env.IOS_TEST_OUTPUT || join(tmpdir(), 'herdr-mobile-ci-ios-unit');
const value = (data: unknown) => Response.json({ value: data });
const element = (id: string) => ({ 'element-6066-11e4-a52e-4f735466cecf': id });
const missing = () => Response.json({ value: { error: 'no such element', message: 'No such element' } }, { status: 404 });
const installed = recorded.contexts.find((context) => context.bundleId === 'com.apple.SafariViewService')!;
const published = { ...installed, url: recorded.publishedPages[0].url, title: recorded.publishedPages[0].title };
type Request = { path: string; body: any; method: string; signal?: AbortSignal | null };
type TestOutcome = void | string;
const tests: Array<[string, () => Promise<TestOutcome>]> = [];
const test = (name: string, body: () => Promise<TestOutcome>) => tests.push([name, body]);

async function adapter(name: string, handler: (request: Request) => Response | Promise<Response>, now?: () => number, nativeDefaults = true) {
  await mkdir(outputRoot, { recursive: true });
  const outputDir = await mkdtemp(join(outputRoot, `${name}-`));
  const budget = new PhaseBudget(name, { timeoutMs: 120_000, recoveryLimit: 0, now });
  const platform = new IOSPlatform({ origin, appiumUrl: 'http://protocol.invalid', outputDir, certificate: '', setupUrl: '', deviceId: 'protocol-only', budget });
  const requests: Request[] = [];
  let inFlight = 0;
  let settings: Record<string, unknown> = { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 };
  const driver = new AppiumClient('http://protocol.invalid', 30_000, async (input, init) => {
    assert.equal(++inFlight, 1, 'Appium requests must not overlap');
    try {
      const request = { path: new URL(String(input)).pathname, body: init?.body ? JSON.parse(String(init.body)) : {}, method: init?.method || 'GET', signal: init?.signal };
      requests.push(request);
      if (request.path === '/session') return Response.json({ value: {}, sessionId: 'protocol' });
      if (nativeDefaults) {
        if (request.path.endsWith('/appium/settings')) {
          if (request.method === 'GET') return value(settings);
          settings = { ...settings, ...request.body.settings };
          return value(null);
        }
        if (request.body.script === 'mobile: queryAppState') return value(request.body.args.bundleId === 'com.apple.springboard' ? 2 : 4);
        if (request.path.endsWith('/alert/text')) return Response.json({ value: { error: 'no such alert', message: 'No alert is open' } }, { status: 404 });
        if (request.path.endsWith('/element/springboard-root/elements')) return value([element('springboard-root')]);
      }
      return await handler(request);
    } finally {
      inFlight -= 1;
    }
  });
  await driver.create({ capabilities: {} });
  driver.setBudget(budget);
  (platform as any).driver = driver;
  (platform as any).installedBundleId = 'com.apple.webapp';
  (platform as any).springBoardRoot = 'springboard-root';
  return { platform, driver, requests, budget, outputDir };
}

async function attachment(name: string, overrides: {
  contexts?: () => unknown;
  foreground?: () => unknown;
  url?: () => string;
  document?: () => unknown;
  switchContext?: (id: string) => Response | undefined;
  now?: () => number;
} = {}) {
  return adapter(name, ({ path, body }) => {
    if (body.script === 'mobile: getContexts') return value(overrides.contexts?.() ?? [published]);
    if (body.script === 'mobile: activeAppInfo') return value(overrides.foreground?.() ?? recorded.foreground);
    if (path.endsWith('/context')) return overrides.switchContext?.(body.name) ?? value(null);
    if (path.endsWith('/url') && !body.url) return value(overrides.url?.() ?? `${origin}/`);
    if (body.script?.startsWith('return {')) {
      assert.match(body.script, /origin: location.origin/u);
      assert.match(body.script, /navigator.standalone === true/u);
      return value(overrides.document?.() ?? { origin, standalone: true, applicationInitialized: true });
    }
    throw new Error(`unexpected attachment operation ${path} ${JSON.stringify(body)}`);
  }, overrides.now);
}

async function assertPermanentFailure(platform: IOSPlatform, requests: Request[], operation: () => Promise<void>, pattern = /IOS_CONTEXT_OWNERSHIP/u) {
  let first: unknown;
  await assert.rejects(operation, (error) => { first = error; return pattern.test(String(error)); });
  const count = requests.length;
  await assert.rejects(() => platform.attachToInstalledView(), (error) => error === first);
  assert.equal(requests.length, count, 'latched ownership failure must not admit another command');
}

for (const mode of ['1ms', '249ms', '250ms', 'timeout', 'interrupted'] as const) {
  test(`iOS attachment timing second native bracket ${mode} preserves original admission and late-settlement fences`, async () => {
    let now = 0;
    let activeObservations = 0;
    let alerts = 0;
    let roots = 0;
    let appStates = 0;
    let discoveries = 0;
    let publications = [published];
    let releaseBody!: (body: string) => void;
    const lateBody = new Promise<string>(resolve => { releaseBody = resolve; });
    const { platform, driver, requests, budget } = await adapter(`attachment-timing-${mode}`, ({ path, body, method }) => {
      if (body.script === 'mobile: getContexts') {
        discoveries++;
        now += 2_000;
        return value(publications);
      }
      if (body.script === 'mobile: queryAppState') {
        assert.equal(body.args.bundleId, 'com.apple.webapp');
        now += ++appStates === 1 ? 1_000 : 3_000;
        return value(4);
      }
      if (path.endsWith('/element/springboard-root/elements')) {
        assert.equal(body.using, 'xpath');
        assert.match(body.value, /XCUIElementTypeAlert/u);
        now += ++roots === 1 ? 1_000 : 4_000;
        return value([element('springboard-root')]);
      }
      if (path.endsWith('/alert/text')) {
        assert.equal(method, 'GET');
        now += ++alerts === 1 ? 1_000 : 13_000;
        return Response.json({ value: { error: 'no such alert', message: 'No alert is open' } }, { status: 404 });
      }
      if (body.script === 'mobile: activeAppInfo') {
        activeObservations++;
        if (activeObservations === 1) {
          now += 1_000;
          return value({ bundleId: 'com.apple.webapp', pid: 29073 });
        }
        assert.equal(now, 28_000, 'last native observation is admitted with 2000ms, not an already-expired budget');
        if (mode === 'timeout' || mode === 'interrupted') {
          const response = new Response(null);
          response.text = () => mode === 'timeout' ? lateBody : Promise.reject(new TypeError('native body interrupted'));
          return response;
        }
        now = 30_000 - Number.parseInt(mode, 10);
        return value({ bundleId: 'com.apple.webapp', pid: 29073 });
      }
      if (path.endsWith('/context')) {
        if (body.name === 'NATIVE_APP') now += 2_000;
        else {
          assert.equal(mode, '250ms', 'sub-250ms switch must never reach transport');
          assert.equal(body.name, published.id);
          assert.equal(method, 'POST');
        }
        return value(null);
      }
      if (path.endsWith('/url')) {
        assert.equal(mode, '250ms');
        return value(`${origin}/`);
      }
      throw new Error(`unexpected attachment timing operation ${path}`);
    }, () => now, false); // No default handler may silently satisfy a native fence.
    const fatal = mode === 'timeout' || mode === 'interrupted';
    await assert.rejects(() => platform.attachToInstalledView(), fatal
      ? mode === 'timeout' ? /APPIUM_TIMEOUT/u : /APPIUM_INTERRUPTED/u
      : /APPIUM_COMMAND_NOT_ADMITTED/u);
    assert.equal(discoveries, 1);
    assert.equal(appStates, 2);
    assert.equal(roots, 2);
    assert.equal(alerts, 2);
    assert.equal(activeObservations, 2);
    assert.equal(budget.recoveryCount, 0);
    assert.equal((driver as any).budget, budget, 'diagnostic scope must not replace the enforcement budget');
    assert.equal((driver as any).timingScope, undefined, 'scope restored on every failure path');
    const native = driver.snapshot().commands.filter(command => command.timing?.scope.endsWith('-native'));
    assert.deepEqual(native.map(command => [command.timing!.scope, command.timing!.operation]), [
      ...['app-state', 'native-elements', 'alert', 'active-app'].map(operation => ['ios-attachment-discovery-native', operation]),
      ...['context', 'app-state', 'native-elements', 'alert', 'active-app'].map(operation => ['ios-attachment-pre-attachment-native', operation]),
    ]);
    assert.ok(native.every(command => command.timing!.admitted && command.timing!.sent));
    assert.ok(native.every(command => command.timing!.phaseRemainingAtDispatchMs! >= (command.timing!.operation === 'alert' || command.timing!.operation === 'context' ? 250 : 1_000)));
    assert.equal(native.at(-1)!.timing!.phaseRemainingAtEntryMs, 2_000);
    assert.equal(native.filter(command => command.timing!.operation === 'alert').every(command => command.timing!.status === 404 && command.timing!.completed), true);
    const snapshot = platform.evidenceSnapshot();
    assert.equal(snapshot.selectedInstalledContext, '');
    assert.equal(snapshot.installedDocumentBound, false);
    assert.equal(snapshot.installedBindingState, mode === '250ms' ? 'inspecting' : 'unselected');
    assert.equal(snapshot.lastIdentity, undefined);
    assert.equal(snapshot.lastCompletion, undefined);
    assert.equal(snapshot.ownershipFailure, undefined);
    assert.equal(requests.some(request => request.body.script?.startsWith('return {')), false, 'no document/runtime acceptance');
    assert.equal(requests.filter(request => request.path.endsWith('/context') && request.body.name !== 'NATIVE_APP').length, mode === '250ms' ? 1 : 0);
    assert.equal(requests.filter(request => request.path.endsWith('/url')).length, mode === '250ms' ? 1 : 0);
    assert.equal(driver.snapshot().selectedContext, mode === '250ms' ? published.id : 'NATIVE_APP');
    assert.equal(driver.snapshot().unusable, fatal);
    assert.equal(driver.snapshot().firstFatal?.code, fatal ? mode === 'timeout' ? 'APPIUM_TIMEOUT' : 'APPIUM_INTERRUPTED' : undefined);
    if (!fatal) {
      const refusal = snapshot.attachmentFirstRefusal as any;
      assert.equal(Object.isFrozen(refusal), true);
      assert.equal(refusal.outcome, 'not-admitted');
      assert.equal(refusal.scope, 'ios-attachment-validation');
      assert.equal(refusal.phaseRemainingAtEntryMs, Number.parseInt(mode, 10));
      assert.equal(refusal.phaseRemainingAtSettlementMs, Number.parseInt(mode, 10));
      assert.equal(refusal.dispatchMs, undefined);
      assert.equal(refusal.dispatchedOrdinal, undefined);
      assert.equal(refusal.sent, false);
      if (mode === '250ms') {
        const switchReceipt = driver.snapshot().commands.find(command => command.timing?.scope === 'ios-attachment-validation' && command.timing.operation === 'context')!;
        assert.equal(switchReceipt.timing!.sent, true);
        assert.equal(switchReceipt.timeoutMs, 250);
      } else {
        assert.equal(refusal.operation, 'context');
        assert.equal(native.at(-1)!.timing!.phaseRemainingAtSettlementMs, Number.parseInt(mode, 10));
      }
    } else assert.equal(snapshot.attachmentFirstRefusal, undefined);
    const saved = structuredClone(snapshot);
    const count = requests.length;
    publications = [{ ...published, title: 'late publication' }];
    now = 30_001;
    releaseBody(JSON.stringify({ value: { bundleId: 'com.apple.webapp', pid: 29073 } }));
    await lateBody;
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(requests.length, count, 'late publication/body must not implicitly issue another command');
    assert.deepEqual(platform.evidenceSnapshot(), saved, 'first refusal/fatal, binding and history remain immutable');
    // An admission refusal is only this attempt's refusal, not a permanent
    // attachment latch. Only fatal body failures prohibit a new invocation.
    if (fatal) {
      await assert.rejects(() => platform.attachToInstalledView(), /APPIUM_SESSION_UNUSABLE/u);
      assert.equal(requests.length, count);
      assert.deepEqual(driver.snapshot().firstFatal, (saved.driver as any).firstFatal);
    }
  });
}

test('recorded initial publication waits passively for the same page before actual document binding', async () => {
  assert.equal(recorded.remoteDebuggerListing[0], `PID:${installed.id.split('_')[1].split('.')[0]}`);
  assert.equal((recorded.remoteDebuggerListing[1] as any)['2'].WIRHostApplicationIdentifierKey, `PID:${recorded.foreground.pid}`);
  assert.equal('WIRHostApplicationIdentifierKey' in installed, false, 'getContexts did not expose the debugger host key');
  let discoveries = 0;
  const { platform, requests, driver, budget } = await attachment('publication', {
    contexts: () => {
      discoveries += 1;
      assert.equal(platform.evidenceSnapshot().selectedInstalledContext, '');
      assert.equal(platform.evidenceSnapshot().ownershipFailure, undefined);
      assert.equal(requests.filter((request) => request.path.endsWith('/url')).length, 0);
      return discoveries === 1 ? recorded.contexts : [recorded.contexts[0], recorded.contexts[1], published];
    },
    document: () => {
      assert.equal(discoveries, 2);
      assert.equal(platform.evidenceSnapshot().selectedInstalledContext, '');
      assert.equal(requests.filter((request) => request.path.endsWith('/url')).length, 1);
      return { origin, standalone: true, applicationInitialized: true };
    },
  });
  await platform.attachToInstalledView();
  assert.equal(discoveries, 2);
  assert.equal(platform.evidenceSnapshot().selectedInstalledContext, installed.id);
  assert.equal(platform.evidenceSnapshot().installedDocumentBound, true);
  assert.equal(requests.filter((request) => request.body.name === installed.id).length, 1);
  assert.equal(requests.some((request) => request.body.script === 'mobile: activateApp'), false);
  assert.equal(budget.recoveryCount, 0);
  const pending = (platform.evidenceSnapshot().events as any[]).find((event) => event.operation === 'initial-publication-pending');
  assert.equal(pending.nativeProvider, 'com.apple.webapp');
  assert.equal(pending.detail.nativePid, String(recorded.foreground.pid));
  assert.equal(driver.snapshot().unusable, false);
});

for (const mode of ['delayed', 'hung'] as const) {
  test(`${mode} initial publication preserves complete Appium transaction admission and failure evidence`, async () => {
    let discoveries = 0;
    let completed = 0;
    let failure = '';
    const { platform, driver, requests, budget, outputDir } = await adapter(`publication-${mode}`, async ({ body, signal }) => {
      if (body.script === 'mobile: activeAppInfo') return value(recorded.foreground);
      assert.equal(body.script, 'mobile: getContexts', 'pending publication must remain passive');
      discoveries += 1;
      if (mode === 'hung') {
        return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
      }
      await wait(4_900, undefined, { signal: signal! });
      completed += 1;
      return value(recorded.contexts);
    });
    try {
      await assert.rejects(() => platform.attachToInstalledView(), (error) => {
        failure = String(error);
        return mode === 'hung' ? /APPIUM_TIMEOUT/u.test(failure) : /IOS_CONTEXT: no installed.*initial page publication is pending/u.test(failure);
      });
      assert.equal(discoveries, 1);
      assert.equal(completed, mode === 'hung' ? 0 : discoveries);
      assert.equal(budget.recoveryCount, 0);
      assert.equal(platform.evidenceSnapshot().selectedInstalledContext, '');
      assert.equal(platform.evidenceSnapshot().installedDocumentBound, false);
      assert.equal(platform.evidenceSnapshot().ownershipFailure, undefined);
      const commands = driver.snapshot().commands.filter((_entry, index) => requests[index]?.body.script === 'mobile: getContexts');
      assert.equal(commands.length, discoveries);
      assert.ok(commands.every((entry) => entry.timeoutMs === 20_000), 'every dispatched discovery needs the complete WebKit allowance');
      if (mode === 'hung') {
        assert.equal(driver.snapshot().unusable, true);
        const first = driver.snapshot().firstFatal;
        assert.equal(first?.code, 'APPIUM_TIMEOUT');
        const count = requests.length;
        await assert.rejects(() => platform.attachToInstalledView(), /APPIUM_SESSION_UNUSABLE/u);
        await assert.rejects(() => driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
        assert.equal(requests.length, count);
        assert.deepEqual(driver.snapshot().firstFatal, first);
        return;
      }
      assert.equal(driver.snapshot().unusable, false);
      assert.equal(driver.snapshot().firstFatal, undefined);
      assert.ok(driver.snapshot().commands.every((entry) => !entry.timedOut && (!entry.error || entry.error.includes('"error":"no such alert"'))));
      const pending = (platform.evidenceSnapshot().events as any[]).filter((event) => event.operation === 'initial-publication-pending');
      assert.equal(pending.length, discoveries);
      assert.ok(pending.every((event) => event.context === installed.id && event.detail.nativePid === String(recorded.foreground.pid)));
      assert.equal(requests.filter((request) => request.body.script === 'mobile: activeAppInfo').length, discoveries);
      assert.deepEqual(await driver.activeAppInfo(), recorded.foreground, 'ordinary delayed publication must leave the session usable');
    } finally {
      await writeSanitizedJson(join(outputDir, 'publication-result.json'), { failure, discoveries, completed, evidence: platform.evidenceSnapshot() });
    }
  });
}

test('recorded 18302ms native-only discovery settles before subsequent Safari publication within original phase', async () => {
  let discoveries = 0;
  const { platform, driver, requests, budget } = await adapter('recorded-safari-discovery', async ({ path, body, signal }) => {
    if (body.script === 'mobile: getContexts') {
      discoveries += 1;
      if (discoveries === 1) {
        await wait(18_302, undefined, { signal: signal! });
        return value([{ id: 'NATIVE_APP' }]);
      }
      return value([{ id: 'WEBVIEW_18099.1', bundleId: 'com.apple.mobilesafari', url: `${origin}/` }]);
    }
    if (path.endsWith('/context')) return value(null);
    if (path.endsWith('/url') && !body.url) return value(`${origin}/`);
    throw new Error(`unexpected Safari operation ${path}`);
  });
  const start = Date.now();
  await (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget.phaseView('navigation', 86_000));
  assert.equal(discoveries, 2);
  assert.ok(Date.now() - start < 46_000);
  assert.equal(driver.snapshot().unusable, false);
  assert.equal(driver.snapshot().firstFatal, undefined);
  assert.equal(requests.some((request) => request.body.url || request.body.script === 'mobile: activateApp'), false);
  assert.ok(driver.snapshot().commands.filter((entry) => entry.path.endsWith('/execute/sync')).every((entry) => entry.timeoutMs === 20_000 && !entry.timedOut));
});

test('cold Safari attach consumes recorded backend duration plus synthetic send and full-body overhead', async () => {
  const { platform, driver, requests, budget } = await adapter('cold-safari-attach', async ({ path, body, signal }) => {
    if (body.script === 'mobile: getContexts') return value([{ id: 'WEBVIEW_23086.1', bundleId: 'com.apple.mobilesafari' }]);
    if (path.endsWith('/context') && body.name !== 'NATIVE_APP') {
      await wait(500, undefined, { signal: signal! });
      await wait(10_927, undefined, { signal: signal! });
      return new Response(new ReadableStream({ async start(controller) {
        await wait(500);
        controller.enqueue(new TextEncoder().encode('{"value":null}'));
        controller.close();
      } }));
    }
    if (path.endsWith('/context')) return value(null);
    if (path.endsWith('/url')) return value(`${origin}/`);
    throw new Error(`unexpected cold attach operation ${path}`);
  });
  await (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget);
  const switches = driver.snapshot().commands.filter(entry => entry.path.endsWith('/context'));
  assert.deepEqual(switches.map(entry => entry.timeoutMs), [15_000, 1_000]);
  assert.equal(requests.filter(request => request.body.name === 'WEBVIEW_23086.1').length, 1);
  assert.equal(driver.snapshot().unusable, false);
});

for (const mode of ['insufficient', 'interrupted', 'late'] as const) {
  test(`cold Safari attach ${mode} preserves admission and quarantine`, async () => {
    let now = 0;
    let settled = false;
    const { platform, driver, requests, budget } = await adapter(`cold-attach-${mode}`, async ({ path, body }) => {
      if (body.script === 'mobile: getContexts') {
        if (mode === 'insufficient') now = 30_000;
        return value([{ id: 'WEBVIEW_23086.1', bundleId: 'com.apple.mobilesafari' }]);
      }
      assert.ok(path.endsWith('/context'));
      if (mode === 'interrupted') return new Response(new ReadableStream({ start(controller) { controller.error(new TypeError('interrupted attach body')); } }));
      await wait(15_300);
      settled = true;
      return value(null);
    }, mode === 'insufficient' ? () => now : undefined);
    await assert.rejects(() => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget), mode === 'insufficient' ? /IOS_NAVIGATION/u : /APPIUM_/u);
    const count = requests.length;
    assert.equal(requests.filter(request => request.path.endsWith('/context')).length, mode === 'insufficient' ? 0 : 1);
    assert.equal(requests.some(request => request.path.endsWith('/url')), false);
    if (mode === 'insufficient') {
      assert.equal(driver.snapshot().unusable, false);
      return;
    }
    const first = driver.snapshot().firstFatal;
    if (mode === 'late') {
      await wait(500);
      assert.equal(settled, true);
    }
    await assert.rejects(() => driver.switchContext('NATIVE_APP', 1_000), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests.length, count);
    assert.deepEqual(driver.snapshot().firstFatal, first);
  });
}

test('bounded discovery admits initial app wait plus settled RPC and completion work', async () => {
  const { platform, driver, budget } = await adapter('discovery-envelope', async ({ path, body, signal }) => {
    if (body.script === 'mobile: getContexts') {
      await wait(5_000, undefined, { signal: signal! });
      await wait(14_500, undefined, { signal: signal! });
      return value([{ id: 'WEBVIEW_18099.1', bundleId: 'com.apple.mobilesafari', url: `${origin}/` }]);
    }
    if (path.endsWith('/context')) return value(null);
    if (path.endsWith('/url')) return value(`${origin}/`);
    throw new Error(`unexpected bounded discovery operation ${path}`);
  });
  await (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget);
  assert.equal(driver.snapshot().unusable, false);
  assert.ok(driver.snapshot().commands.every((entry) => !entry.timedOut));
});

for (const mode of ['parent', 'absent', 'backend-error', 'interrupted-body'] as const) {
  test(`Safari discovery ${mode} cannot turn missing evidence into readiness`, async () => {
    let now = 0;
    const { platform, driver, requests, budget } = await adapter(`safari-discovery-${mode}`, ({ body }) => {
      assert.equal(body.script, 'mobile: getContexts');
      if (mode === 'interrupted-body') return new Response(new ReadableStream({ start(controller) { controller.error(new TypeError('interrupted discovery body')); } }));
      now += 20_000;
      if (mode === 'backend-error') return Response.json({ value: { error: 'unknown error', message: 'discovery backend unavailable' } }, { status: 500 });
      return value([{ id: 'NATIVE_APP' }]);
    }, () => now);
    const phase = budget.phaseView('navigation', mode === 'parent' ? 25_999 : 46_000);
    await assert.rejects(() => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, phase), mode === 'interrupted-body' ? /APPIUM_/u : /IOS_NAVIGATION/u);
    assert.equal(requests.filter((request) => request.body.script === 'mobile: getContexts').length, mode === 'parent' ? 0 : mode === 'interrupted-body' ? 1 : 2);
    assert.equal(requests.some((request) => request.path.endsWith('/context') || request.body.url), false);
    assert.equal(driver.snapshot().unusable, mode === 'interrupted-body');
    assert.equal(budget.recoveryCount, 0);
  });
}

for (const late of [[{ id: 'NATIVE_APP' }], [{ id: 'WEBVIEW_18099.1', bundleId: 'com.apple.mobilesafari', url: `${origin}/` }]]) {
  test(`late discovery ${late[0].id} cannot clear timeout quarantine`, async () => {
    let settled = false;
    const { platform, driver, requests, budget } = await adapter('late-safari-discovery', async ({ body }) => {
      assert.equal(body.script, 'mobile: getContexts');
      await wait(20_300);
      settled = true;
      return value(late);
    });
    await assert.rejects(() => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget), /APPIUM_TIMEOUT/u);
    const first = driver.snapshot().firstFatal;
    const count = requests.length;
    await wait(500);
    assert.equal(settled, true);
    assert.equal(driver.snapshot().unusable, true);
    assert.deepEqual(driver.snapshot().firstFatal, first);
    await assert.rejects(() => driver.contextMetadata(20_000), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests.length, count);
  });
}

test('Safari native-only discovery retains the first cause and reserve boundary after late publication', async () => {
  let now = 0;
  let published = false;
  let discoveries = 0;
  let availableContexts = [{ id: 'NATIVE_APP' }];
  const lateContexts = [{ id: 'WEBVIEW_18099.1', bundleId: 'com.apple.mobilesafari', url: `${origin}/` }];
  let releasePublication!: () => void;
  const publication = new Promise<void>((resolve) => {
    releasePublication = () => {
      published = true;
      availableContexts = lateContexts;
      resolve();
    };
  });
  const { platform, driver, requests, budget } = await adapter('safari-boundary-publication', ({ body }) => {
    if (body.script === 'mobile: getContexts') {
      assert.equal(published, false, 'late page publication must not trigger another discovery');
      discoveries += 1;
      now = discoveries === 1 ? 20_000 : 20_001;
      return value(availableContexts);
    }
    throw new Error(`unexpected late-publication operation ${JSON.stringify(body)}`);
  }, () => now);

  const originalSetTimeout = globalThis.setTimeout;
  (globalThis as any).setTimeout = (callback: (...args: any[]) => void, _delay: number, ...args: any[]) => originalSetTimeout(callback, 0, ...args);
  let failure = '';
  try {
    await assert.rejects(
      () => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget),
      (error) => {
        failure = String(error);
        return /not enough time for WebKit discovery and Safari observation \(25999ms remains\)/u.test(failure);
      },
    );
  } finally {
    (globalThis as any).setTimeout = originalSetTimeout;
  }
  const beforeLatePublication = structuredClone(platform.evidenceSnapshot().safariNavigation);
  const eventsBeforeLatePublication = (platform.evidenceSnapshot().events as any[])
    .filter((event) => event.operation === 'safari-navigation-first-failure');
  assert.equal(eventsBeforeLatePublication.length, 1);
  assert.equal((beforeLatePublication as any).lastDiscovery.result, 'native-only');
  assert.equal((beforeLatePublication as any).lastDiscovery.cause, 'Safari did not publish a web context');
  assert.equal(discoveries, 2);
  assert.equal((beforeLatePublication as any).lastDiscovery.remainingMs, 25_999);
  assert.equal((beforeLatePublication as any).reserveExhaustion.reason, 'discovery-observation');
  assert.equal((beforeLatePublication as any).reserveExhaustion.remainingMs, 25_999);
  assert.equal((beforeLatePublication as any).firstFailure.kind, 'discovery');
  assert.equal((beforeLatePublication as any).firstFailure.cause, 'Safari did not publish a web context');
  assert.equal(requests.filter((request) => request.body.script === 'mobile: getContexts').length, 2);
  assert.equal(requests.some((request) => request.path.endsWith('/context')
    || request.path.endsWith('/url')
    || request.body.script === 'mobile: activeAppInfo'
    || request.body.script?.startsWith('return {')), false);
  assert.equal((platform.evidenceSnapshot() as any).installedBundleId, 'com.apple.webapp');
  assert.equal((platform.evidenceSnapshot() as any).installedBindingState, 'unselected');
  assert.equal((platform.evidenceSnapshot() as any).lastIdentity, undefined);
  assert.match(failure, /Safari did not publish a web context/u);

  releasePublication();
  await publication;
  assert.deepEqual(availableContexts, lateContexts);
  const afterLatePublication = platform.evidenceSnapshot();
  assert.deepEqual(afterLatePublication.safariNavigation, beforeLatePublication);
  assert.equal(afterLatePublication.installedBindingState, 'unselected');
  assert.equal(afterLatePublication.lastIdentity, undefined);
  assert.equal(requests.filter((request) => request.body.script === 'mobile: getContexts').length, 2);
  assert.equal((afterLatePublication.events as any[])
    .filter((event) => event.operation === 'safari-navigation-first-failure').length, 1);
  assert.equal(driver.snapshot().unusable, false);
});

test('Safari diagnostic inspection caps oversized metadata without changing selection work', async () => {
  let now = 0;
  let discoveries = 0;
  const hugeUrl = 'x'.repeat(100_000);
  const oversizedMetadata = [
    { id: 'WEBVIEW-oversized', bundleId: 'not-safari', url: hugeUrl },
    ...Array.from({ length: 4_096 }, () => ({ id: 'NATIVE_APP' })),
  ];
  const { platform, driver, requests, budget } = await adapter('safari-oversized-diagnostics', ({ body }) => {
    if (body.script === 'mobile: getContexts') {
      discoveries += 1;
      now = 20_001;
      return value(oversizedMetadata);
    }
    throw new Error(`unexpected oversized-diagnostics operation ${JSON.stringify(body)}`);
  }, () => now);
  const originalSetTimeout = globalThis.setTimeout;
  (globalThis as any).setTimeout = (callback: (...args: any[]) => void, _delay: number, ...args: any[]) => originalSetTimeout(callback, 0, ...args);
  try {
    await assert.rejects(
      () => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget),
      /not enough time for WebKit discovery and Safari observation \(25999ms remains\)/u,
    );
  } finally {
    (globalThis as any).setTimeout = originalSetTimeout;
  }
  const navigation = platform.evidenceSnapshot().safariNavigation as any;
  assert.equal(discoveries, 1);
  assert.equal(navigation.lastDiscovery.result, 'inspection-truncated');
  assert.equal(navigation.lastDiscovery.contextCount, 64);
  assert.equal(navigation.lastDiscovery.inspectedContextCount, 64);
  assert.equal(navigation.lastDiscovery.contextInspectionTruncated, true);
  assert.equal(navigation.lastDiscovery.stringInspectionTruncated, true);
  assert.equal(JSON.stringify(navigation).includes(hugeUrl), false);
  assert.equal(requests.filter((request) => request.body.script === 'mobile: getContexts').length, 1);
  assert.equal(requests.some((request) => request.path.endsWith('/context') || request.path.endsWith('/url')), false);
  assert.equal(driver.snapshot().unusable, false);
});

test('Safari discovery and observation evidence remain separate at reserve exhaustion', async () => {
  let now = 0;
  const { platform, requests, budget } = await adapter('safari-observation-reserve', ({ path, body }) => {
    if (body.script === 'mobile: getContexts') return value([{ id: 'WEBVIEW-18099.1', bundleId: 'com.apple.mobilesafari' }]);
    if (path.endsWith('/context')) return value(null);
    if (path.endsWith('/url') && !body.url) {
      now = 30_001;
      return value('https://other.invalid/');
    }
    if (path.endsWith('/url') && body.url) return value(null);
    throw new Error(`unexpected observation-reserve operation ${path} ${JSON.stringify(body)}`);
  }, () => now);

  await assert.rejects(
    () => (platform as any).waitForSafariFixturePage(`${origin}/`, 46_000, budget),
    /IOS_NAVIGATION: Safari fixture page was not ready/u,
  );
  const navigation = platform.evidenceSnapshot().safariNavigation as any;
  assert.equal(navigation.lastDiscovery.result, 'safari-context');
  assert.equal(navigation.lastDiscovery.cause, 'Safari web context was published');
  assert.equal(navigation.lastDiscovery.elapsedMs, 0);
  assert.equal(navigation.lastDiscovery.remainingMs, 46_000);
  assert.equal(navigation.lastObservation.result, 'origin-mismatch');
  assert.equal(navigation.lastObservation.cause, 'Safari page did not report the fixture origin');
  assert.equal(navigation.lastObservation.remainingMs, 15_999);
  assert.equal(navigation.reserveExhaustion.reason, 'discovery-observation');
  assert.equal(navigation.firstFailure.kind, 'observation');
  assert.equal(navigation.firstFailure.cause, 'Safari page did not report the fixture origin');
  assert.equal(requests.filter((request) => request.path.endsWith('/context')).length, 1);
  assert.equal(requests.filter((request) => request.path.endsWith('/url')).length, 2);
});

for (const budgetSource of ['attachment', 'parent'] as const) {
  test(`insufficient ${budgetSource} budget does not dispatch initial publication discovery`, async () => {
    let now = 0;
    const { platform, driver, requests } = await adapter(`publication-budget-${budgetSource}`, () => {
      throw new Error('no partial discovery is admissible');
    }, () => now);
    if (budgetSource === 'parent') now = 103_000;
    await assert.rejects(() => platform.attachToInstalledView(budgetSource === 'attachment' ? 17_000 : undefined), /IOS_CONTEXT: no installed.*not enough time for WebKit discovery/u);
    assert.equal(requests.length, 1);
    assert.equal(driver.snapshot().unusable, false);
    assert.equal(driver.snapshot().firstFatal, undefined);
  });
}

for (const metadata of ['blank', 'absent', 'unrelated-browser'] as const) {
  test(`initial ${metadata} never accepts or reactivates an unpublished installed page`, async () => {
    let now = 0;
    let discoveries = 0;
    const { platform, driver, requests } = await attachment(`never-${metadata}`, {
      now: () => now,
      contexts: () => { discoveries += 1; now += 10_000; return metadata === 'blank' ? [installed] : metadata === 'absent' ? [] : [recorded.contexts[1]]; },
    });
    const reason = metadata === 'blank' ? /initial page publication is pending/u : metadata === 'absent' ? /installed page metadata is unavailable/u : /no installed page for/u;
    await assert.rejects(() => platform.attachToInstalledView(), (error) => /IOS_CONTEXT: no installed/u.test(String(error)) && reason.test(String(error)));
    assert.equal(discoveries, 1);
    assert.equal(driver.snapshot().unusable, false);
    assert.equal(driver.snapshot().firstFatal, undefined);
    assert.equal(platform.evidenceSnapshot().ownershipFailure, undefined);
    assert.equal(requests.filter((request) => request.path.endsWith('/url')).length, 0);
    assert.equal(requests.some((request) => request.body.script === 'mobile: activateApp'), false);
  });
}

for (const mode of ['foreground-blank', 'foreground-absent', 'wrong-url', 'document-origin', 'initialized-browser', 'metadata-origin', 'null-data-url', 'wrong-provider', 'named-blank'] as const) {
  test(`initial publication does not excuse ${mode} and ownership failure is permanent`, async () => {
    const { platform, requests } = await attachment(mode, {
      contexts: () => mode === 'foreground-absent' ? [] : [{
        ...published,
        url: mode === 'metadata-origin' ? 'https://other.test/' : mode === 'null-data-url' ? 'data:text/html,blank' : mode === 'foreground-blank' || mode === 'named-blank' ? 'about:blank' : published.url,
        title: mode === 'foreground-blank' ? '' : published.title,
        bundleId: mode === 'wrong-provider' ? 'com.example.unrelated' : published.bundleId,
      }],
      foreground: () => ({ ...recorded.foreground, bundleId: mode.startsWith('foreground') ? 'com.apple.mobilesafari' : 'com.apple.webapp' }),
      url: () => mode === 'wrong-url' ? 'about:blank' : `${origin}/`,
      document: () => ({ origin: mode === 'document-origin' ? 'https://other.test' : origin, standalone: mode !== 'initialized-browser', applicationInitialized: true }),
    });
    await assertPermanentFailure(platform, requests, () => platform.attachToInstalledView());
  });
}

for (const mode of ['cached-blank', 'cached-origin', 'cached-uninitialized', 'stale-blank', 'stale-origin', 'stale-valid'] as const) {
  test(`permanent binding survives ${mode} cache validation or invalidation`, async () => {
    let bound = false;
    let staleReturned = false;
    const { platform, requests } = await attachment(mode, {
      contexts: () => [bound && mode === 'stale-blank' ? installed : bound && mode === 'stale-origin' ? { ...published, url: 'https://other.test/' } : published],
      url: () => bound && mode === 'cached-blank' ? 'about:blank' : bound && mode === 'cached-origin' ? 'https://other.test/' : `${origin}/`,
      document: () => ({ origin, standalone: !(bound && mode === 'cached-uninitialized'), applicationInitialized: false }),
      switchContext: (id) => {
        if (bound && mode.startsWith('stale') && !staleReturned && id === installed.id) {
          staleReturned = true;
          return Response.json({ value: { error: 'no such context', message: 'no such context' } }, { status: 404 });
        }
        return undefined;
      },
    });
    await platform.attachToInstalledView();
    assert.equal(platform.evidenceSnapshot().installedDocumentBound, true);
    bound = true;
    if (mode === 'stale-valid') {
      await platform.attachToInstalledView();
    } else {
      await assertPermanentFailure(platform, requests, () => platform.attachToInstalledView());
    }
    assert.equal(platform.evidenceSnapshot().installedDocumentBound, true);
  });
}

test('a page already inspected during initial binding cannot regain the blank-publication exception', async () => {
  let discoveries = 0;
  const { platform, requests } = await attachment('inspected-blank', {
    contexts: () => {
      discoveries += 1;
      return [discoveries === 1 ? published : discoveries === 2 ? installed : { ...published, url: 'https://other.test/' }];
    },
    document: () => ({ origin, standalone: false, applicationInitialized: false }),
  });
  await assertPermanentFailure(platform, requests, () => platform.attachToInstalledView());
  assert.equal(discoveries, 2);
});

const before = await readFile(join(fixtureDir, 'ios-share-0-hierarchy.xml'), 'utf8');
const after = await readFile(join(fixtureDir, 'ios-share-1-hierarchy.xml'), 'utf8');
const browser = await readFile(join(fixtureDir, 'ios-before-share-hierarchy.xml'), 'utf8');
const hypotheticalConfirmation = await readFile(join(fixtureDir, 'ios-confirmation-hypothetical.xml'), 'utf8');

function xpathCount(source: string, xpath: string): number {
  return Number(execFileSync('xmllint', ['--xpath', `count(${xpath})`, '-'], { input: source, encoding: 'utf8' }).trim());
}

function requireXmlLint(): undefined {
  try { execFileSync('xmllint', ['--version'], { stdio: 'pipe' }); }
  catch (cause) {
    throw new Error('xmllint is required for XML XPath protocol checks; install libxml2-utils on Ubuntu before running the mobile tests', { cause });
  }
}

const confirmationProfiles: Record<string, number[]> = {
  'latest-push': [364, 333],
  'earlier-pr': [354, 281],
  'earlier-push': [4_378],
  'latest-pr': [1_488, 4_587],
};

for (const mode of ['success', 'disabled', 'dismissed', 'limit', 'eighth', 'latest-push', 'earlier-pr', 'earlier-push', 'latest-pr', 'add-missing', 'add-disabled', 'add-hidden', 'add-not-hittable', 'add-budget', 'add-hung'] as const) {
  test(`recorded Share sheet protocol ${mode} keeps scoped controls and bounded gestures`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    let sheet = mode === 'limit' || mode === 'eighth';
    let scrolls = 0;
    let reads = 0;
    let source = browser;
    const clicks: string[] = [];
    let confirmationLookups = 0;
    let now = 0;
    const scrolling = mode === 'limit' || mode === 'eighth';
    const ready = () => scrolling ? scrolls >= (mode === 'eighth' ? 8 : 9) : scrolls > 0;
    const currentSource = () => {
      if (!sheet || (mode === 'dismissed' && scrolls > 0) || (!scrolling && reads++ === 0)) return browser;
      if (ready()) return after;
      if (!scrolling) return before;
      return before.replace(/\by="(-?\d+)"/gu, (match, y) => Number(y) >= 645 ? `y="${Number(y) - scrolls * 2}"` : match);
    };
    const { platform, driver, requests } = await adapter(`share-${mode}`, async ({ path, body, signal }) => {
      if (path.endsWith('/context')) return value(null);
      if (body.script === 'mobile: activeAppInfo') return value({ bundleId: 'com.apple.mobilesafari', pid: 20640 });
      if (path.endsWith('/source')) { source = currentSource(); return value(source); }
      if (path.endsWith('/screenshot')) return value('');
      if (body.script === 'mobile: scroll') {
        assert.deepEqual(body.args, { element: `container-${scrolls}`, direction: 'down', distance: 0.75 });
        assert.ok(nativeActionListEvidence(source, 'Add to Home Screen'));
        scrolls += 1;
        return value(null);
      }
      if (path.endsWith('/elements')) {
        assert.equal(body.using, 'xpath');
        if (body.value.includes('XCUIElementTypeNavigationBar')) {
          assert.equal(xpathCount(hypotheticalConfirmation, body.value), 1);
          return value([element('add')]);
        }
        assert.ok(xpathCount(source, body.value) > 0, 'adapter XPath must match the recorded hierarchy');
        return value([element(`${body.value.includes('Add to Home Screen') ? 'target' : 'container'}-${scrolls}`)]);
      }
      if (path.endsWith('/element')) {
        if (body.value === 'ShareButton') return value(element('share'));
        assert.ok(!body.value.includes('Open as Web App'), 'the pinned confirmation must not probe optional controls');
        if (body.value === 'Add') {
          assert.equal(body.using, 'accessibility id');
          confirmationLookups += 1;
          if (mode === 'add-hung') return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
          if (mode === 'add-missing') { now += 5_000; return missing(); }
          const latency = confirmationProfiles[mode]?.[confirmationLookups - 1];
          if (latency !== undefined) {
            await wait(latency, undefined, { signal: signal! });
            return missing();
          }
          return value(element('add'));
        }
        return missing();
      }
      if (path.endsWith('/rect')) {
        assert.ok(path.includes(`-${scrolls}/`), 'the adapter must refresh element references after a gesture');
        const list = nativeActionListEvidence(source, 'Add to Home Screen')!;
        return value(path.includes('/container-') ? list.collection.bounds : list.targetRows[0].bounds);
      }
      if (path.includes('/attribute/')) {
        if (path.includes('/add/')) {
          if (mode === 'add-disabled' && path.endsWith('/enabled')) { now += 5_000; return value('false'); }
          if (mode === 'add-hidden' && path.endsWith('/visible')) { now += 5_000; return value('false'); }
          if (mode === 'add-not-hittable' && path.endsWith('/hittable')) { now += 5_000; return value('false'); }
        }
        if (path.includes('/target-') && path.endsWith('/enabled') && mode === 'disabled') return value('false');
        if (path.includes('/target-') && /\/(?:visible|hittable)$/u.test(path)) return value(String(ready()));
        return value('true');
      }
      if (path.endsWith('/click')) {
        const id = path.split('/element/')[1].split('/')[0];
        if (id === 'share') sheet = true;
        else if (id.startsWith('target')) {
          assert.ok(ready());
          if (mode === 'add-budget') now = 116_000;
        }
        clicks.push(id);
        return value(null);
      }
      throw new Error(`unexpected Share command ${path} ${JSON.stringify(body)}`);
    }, mode.startsWith('add-') && mode !== 'add-hung' ? () => now : undefined);
    if (scrolling) {
      const operation = () => (platform as any).findNativeScrollable([{ using: 'xpath', value: "//*[@name='ActivityListView']//*[@name='activityCollectionView']//*[@name='actionGroupCell' and contains(@label, 'Add to Home Screen')]" }], 'Add to Home Screen', 60_000);
      if (mode === 'limit') await assert.rejects(operation, /scroll limit/u);
      else assert.equal(await operation(), 'target-8');
      assert.equal(scrolls, 8);
      assert.deepEqual(clicks, []);
    } else if (mode === 'success' || mode in confirmationProfiles) {
      await platform.installFromBrowser();
      assert.deepEqual(clicks, ['share', 'target-1', 'add']);
      assert.equal(scrolls, 1);
      assert.equal(confirmationLookups, (confirmationProfiles[mode]?.length ?? 0) + 1);
    } else if (mode.startsWith('add-')) {
      await assert.rejects(() => platform.installFromBrowser(), mode === 'add-hung' ? /APPIUM_TIMEOUT/u : /confirmation control was not ready/u);
      assert.deepEqual(clicks, ['share', 'target-1']);
      if (mode === 'add-budget') assert.equal(confirmationLookups, 0, 'do not dispatch a partial confirmation lookup');
      if (mode === 'add-hung') {
        const count = requests.length;
        await assert.rejects(() => driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
        assert.equal(requests.length, count);
      }
    } else {
      await assert.rejects(() => platform.installFromBrowser(), mode === 'disabled' ? /disabled/u : /dismissed or replaced/u);
      assert.deepEqual(clicks, ['share']);
      assert.equal(scrolls, mode === 'disabled' ? 0 : 1);
    }
    const commands = driver.snapshot().commands;
    const lookups = commands.filter((_entry, index) => requests[requests.length - commands.length + index]?.body.value === 'Add');
    assert.ok(lookups.every((entry) => entry.timeoutMs === 8_000), 'confirmation probes must receive a complete native transaction, never optional-loop leftovers');
    assert.equal(driver.snapshot().unusable, mode === 'add-hung');
  });
}

type ConfirmationState = 'ready' | 'missing' | 'stale-lookup' | 'stale-enabled' | 'stale-visible' | 'stale-hittable' | 'disabled' | 'hidden' | 'not-hittable' | 'indeterminate';
type ConfirmationFault = 'stale' | 'unrelated' | 'invalid-session' | 'misleading-404' | 'malformed' | 'malformed-value' | 'transport' | 'interrupted' | 'hung' | 'body-hung' | 'body-interrupted' | 'body-late';
type ConfirmationBoundary = 'lookup' | 'identity' | 'enabled' | 'visible' | 'hittable' | 'click';

async function confirmationReplay(name: string, options: {
  recorded?: typeof recordedConfirmation[number];
  states?: ConfirmationState[];
  persistent?: boolean;
  dialog?: 'different' | 'hidden' | 'unrelated-add' | 'ambiguous' | 'ambiguous-after-ready' | 'replaced' | 'replaced-after-ready';
  staleIdentityRead?: number;
  replaceReadyIdentity?: 'once' | 'always';
  replaceFirstIdentity?: boolean;
  oldIdentityPolicy?: boolean;
  foreground?: string;
  fault?: ConfirmationFault;
  faultAt?: ConfirmationBoundary;
  faultOnIdentityRead?: number;
  fourthReadStale?: boolean;
  after?: (boundary: string, lookup: number) => number;
  remainingAt?: (boundary: string, lookup: number, identityRead: number) => number | undefined;
  initialConfirmationRemaining?: number;
  activityDuration?: number;
  afterConfirmationValidation?: number;
  parentRemaining?: number;
  latency?: Partial<Record<ConfirmationBoundary, number>>;
} = {}) {
  const fixture = options.recorded;
  const shareSources = fixture ? await Promise.all(Object.values(fixture.sources).map((path) => readFile(join(fixtureDir, path), 'utf8'))) : [browser, before, after];
  let now = 0;
  let confirmationStart = 0;
  let sheet = false;
  let confirming = false;
  let scrolls = 0;
  let lookups = 0;
  let identityReads = 0;
  let source = shareSources[0];
  const clicks: string[] = [];
  const attributes: string[] = [];
  const observations: Array<{ boundary: string; lookup: number; now: number }> = [];
  const states = options.states || ['ready'];
  const state = () => states[Math.min(lookups - 1, states.length - 1)];
  const stale = () => Response.json({ value: (fixture || recordedConfirmation[0]).response.value }, { status: 404 });
  const advance = (boundary: string) => {
    now += options.after?.(boundary, lookups) || 0;
    const remaining = options.remainingAt?.(boundary, lookups, identityReads);
    if (remaining !== undefined) now = confirmationStart + 75_000 - remaining;
    observations.push({ boundary, lookup: lookups, now });
  };
  const { platform, driver, requests, budget, outputDir } = await adapter(`confirmation-${name}`, async ({ path, body, signal }) => {
    const responseBody = (kind: 'hung' | 'interrupted' | 'late') => new Response(new ReadableStream({
      start(controller) {
        if (kind === 'interrupted') {
          controller.enqueue(new TextEncoder().encode('{"value":'));
          queueMicrotask(() => controller.error(new Error('synthetic response body interruption')));
        } else if (kind === 'late') {
          setTimeout(() => {
            controller.enqueue(new TextEncoder().encode(JSON.stringify({ value: element('late') })));
            controller.close();
          }, 12_500);
        }
      },
    }));
    const fault = () => {
      if (options.fault === 'hung') return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
      if (options.fault === 'body-hung') return responseBody('hung');
      if (options.fault === 'body-interrupted') return responseBody('interrupted');
      if (options.fault === 'body-late') return responseBody('late');
      if (options.fault === 'transport') throw new Error('synthetic connection reset');
      if (options.fault === 'interrupted') throw new DOMException('synthetic interrupted transport', 'TimeoutError');
      if (options.fault === 'malformed') return new Response('not-json', { status: 200 });
      if (options.fault === 'malformed-value') return value({ unexpected: true });
      if (options.fault === 'stale') return stale();
      const error = options.fault === 'invalid-session' ? 'invalid session id' : options.fault === 'misleading-404' ? 'unknown command' : 'invalid argument';
      return Response.json({ value: { error, message: options.fault === 'misleading-404' ? 'stale element reference is not this error code' : 'synthetic protocol failure' } }, { status: options.fault === 'unrelated' ? 400 : 404 });
    };
    if (path.endsWith('/context')) return value(null);
    if (body.script === 'mobile: activeAppInfo') {
      if (confirming) advance('foreground');
      return value({ bundleId: confirming ? options.foreground ?? 'com.apple.mobilesafari' : 'com.apple.mobilesafari', pid: 20640 });
    }
    if (path.endsWith('/source')) {
      source = !sheet ? shareSources[0] : scrolls ? shareSources[2] : shareSources[1];
      return value(source);
    }
    if (path.endsWith('/screenshot')) return value('');
    if (body.script === 'mobile: scroll') {
      assert.deepEqual(body.args, { element: `container-${scrolls}`, direction: 'down', distance: 0.75 });
      scrolls++;
      return value(null);
    }
    if (path.endsWith('/elements')) {
      assert.equal(body.using, 'xpath');
      if (confirming) {
        await wait(options.latency?.identity || 0);
        identityReads++;
        advance('identity');
        if (identityReads === options.staleIdentityRead) return stale();
        if (options.faultAt === 'identity' && (options.faultOnIdentityRead === undefined || options.faultOnIdentityRead === identityReads)) return fault();
        let xml = hypotheticalConfirmation;
        const replaced = (options.dialog === 'replaced' && lookups > 1) || (options.dialog === 'replaced-after-ready' && identityReads > 1);
        if (options.dialog === 'different' || replaced) xml = xml.replace('name="Add to Home Screen"', 'name="Add Bookmark"');
        if (options.dialog === 'hidden') xml = xml.replace('name="Add to Home Screen" visible="true"', 'name="Add to Home Screen" visible="false"');
        const count = xpathCount(xml, body.value);
        assert.equal(count, options.dialog === 'different' || options.dialog === 'hidden' || replaced ? 0 : 1, 'only Add in the visible expected navigation bar may match');
        if (!count) return value([]);
        if (options.dialog === 'ambiguous' || (options.dialog === 'ambiguous-after-ready' && identityReads > 1)) return value([element(`add-${lookups}`), element('other-add')]);
        const replacedAdd = (options.replaceFirstIdentity && identityReads === 1) || (options.replaceReadyIdentity && identityReads % 2 === 0 && (options.replaceReadyIdentity === 'always' || identityReads === 2));
        return value([element(options.dialog === 'unrelated-add' ? 'expected-add' : `add-${lookups + (replacedAdd ? 1 : 0)}`)]);
      }
      assert.ok(xpathCount(source, body.value) > 0, 'real adapter selector must match recorded Share XML');
      const target = body.value.includes('Add to Home Screen');
      if (target && options.parentRemaining !== undefined && !confirming) now = 120_000 - options.parentRemaining;
      return value([element(`${target ? 'target' : 'container'}-${scrolls}`)]);
    }
    if (path.endsWith('/element')) {
      if (body.value === 'ShareButton') return value(element('share'));
      assert.ok(confirming, 'confirmation lookup cannot precede the verified activity click');
      assert.deepEqual(body, { using: 'accessibility id', value: 'Add' }, 'no optional or unscoped replacement selectors');
      lookups++;
      await wait(options.latency?.lookup || 0);
      advance('lookup');
      if (options.persistent || (options.fault && lookups > 1)) now += 3_000;
      if (fixture && lookups === 1) await wait(fixture.lookup.endedAt - fixture.lookup.startedAt, undefined, { signal: signal! });
      if (options.faultAt === 'lookup') return fault();
      if (state() === 'missing') return missing();
      if (state() === 'stale-lookup') return stale();
      return value(element(`add-${lookups}`));
    }
    if (path.endsWith('/rect')) {
      const list = nativeActionListEvidence(source, 'Add to Home Screen')!;
      return value(path.includes('/container-') ? list.collection.bounds : list.targetRows[0].bounds);
    }
    if (path.includes('/attribute/')) {
      if (path.includes('/add-')) {
        const attribute = path.split('/attribute/')[1];
        const id = path.split('/element/')[1].split('/')[0];
        assert.equal(id, `add-${lookups}`, 'readiness must use the freshly acquired candidate');
        attributes.push(`${id}:${attribute}`);
        advance(attribute);
        if (options.faultAt === attribute) return fault();
        if (options.fourthReadStale && attributes.length === 4) return stale();
        if (state() === `stale-${attribute}`) {
          if (fixture && lookups === 1) await wait(fixture.attribute.durationMs, undefined, { signal: signal! });
          return stale();
        }
        if (state() === 'disabled' && attribute === 'enabled') return value('false');
        if (state() === 'hidden' && attribute === 'visible') return value('false');
        if (state() === 'not-hittable' && attribute === 'hittable') return value('false');
        if (state() === 'indeterminate' && attribute === 'enabled') return value(null);
      }
      if (path.includes('/target-') && /\/(?:visible|hittable)$/u.test(path)) return value(String(scrolls > 0));
      return value('true');
    }
    if (path.endsWith('/click')) {
      const id = path.split('/element/')[1].split('/')[0];
      clicks.push(id);
      if (id === 'share') sheet = true;
      if (id.startsWith('target-')) {
        assert.equal(scrolls, 1);
        now += options.activityDuration || 0;
        confirmationStart = now;
        confirming = true;
      }
      if (id.startsWith('add-')) {
        advance('click');
        if (options.faultAt === 'click') return fault();
      }
      return value(null);
    }
    throw new Error(`unexpected confirmation request ${path} ${JSON.stringify(body)}`);
  }, fixture || options.latency ? undefined : () => now);
  if (options.oldIdentityPolicy) {
    const original = driver.command.bind(driver);
    driver.command = (path, method, body, timeoutMs) => original(path, method, body, path === '/elements' && confirming ? 8_000 : timeoutMs);
  }
  if (options.initialConfirmationRemaining !== undefined || options.afterConfirmationValidation !== undefined) {
    const original = (platform as any).waitForInstallConfirmation.bind(platform);
    (platform as any).waitForInstallConfirmation = async (phase: PhaseBudget) => {
      if (options.initialConfirmationRemaining !== undefined) now = confirmationStart + 75_000 - options.initialConfirmationRemaining;
      const result = await original(phase);
      now += options.afterConfirmationValidation || 0;
      return result;
    };
  }
  (platform as any).installedBundleId = '';
  let error: unknown;
  try { await platform.installFromBrowser(); } catch (caught) { error = caught; }
  await writeSanitizedJson(join(outputDir, 'confirmation-result.json'), {
    proofKind: fixture?.proofKind || 'Hypothetical protocol boundary and native confirmation hierarchy; not observed native recovery.',
    error: String(error || ''), lookups, attributes, clicks, observations, evidence: platform.evidenceSnapshot(),
  });
  assert.equal(budget.recoveryCount, 0);
  assert.equal(platform.evidenceSnapshot().installedBindingState, 'unselected');
  assert.equal(platform.evidenceSnapshot().installedDocumentBound, false);
  assert.equal(requests.filter((request) => request.path.endsWith('/context')).length, 1, 'no context reset during reacquisition');
  assert.equal(requests.some((request) => /activateApp|pressButton/u.test(request.body.script || '') || request.path.endsWith('/url')), false, 'never restart installation or navigation');
  const targetClickIndex = requests.findIndex((request) => request.path.endsWith('/element/target-1/click'));
  if (targetClickIndex >= 0) assert.deepEqual(clicks.slice(0, 2), ['share', 'target-1']);
  else assert.deepEqual(clicks, ['share']);
  assert.ok(clicks.length <= 3, 'final Add must be dispatched at most once');
  const settingsWrites = requests.filter(request => request.path.endsWith('/appium/settings') && 'waitForIdleTimeout' in (request.body.settings || {}));
  if (settingsWrites.length && targetClickIndex >= 0) {
    assert.ok(requests.indexOf(settingsWrites[0]) < targetClickIndex, 'confirmation settings must be established before the activity click');
  }
  if (!error) assert.equal(settingsWrites.length, 2, 'a successful confirmation must apply and restore the scoped backend policy');
  if (settingsWrites.length) {
    assert.deepEqual(settingsWrites[0].body.settings, { waitForIdleTimeout: 1, animationCoolOffTimeout: 0.2 });
    assert.equal(settingsWrites.length, driver.snapshot().firstFatal ? 1 : 2);
    if (!driver.snapshot().firstFatal) assert.deepEqual(settingsWrites[1].body.settings, { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 });
  }
  return { platform, driver, requests, error, lookups, attributes, clicks, observations, now };
}

test('confirmation cycle13 old 8s policy rejects recorded 8650ms aggregate plus synthetic 50ms overhead', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('cycle13-old-8650', { latency: { identity: 8_650 + 50 }, replaceFirstIdentity: true, oldIdentityPolicy: true });
  assert.match(String(replay.error), /APPIUM_TIMEOUT/u);
  assert.deepEqual(replay.clicks, ['share', 'target-1']);
  assert.equal(replay.driver.snapshot().commands.find(entry => entry.timedOut)?.timeoutMs, 8_000);
  const count = replay.requests.length;
  await wait(800);
  await assert.rejects(() => replay.driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
  assert.equal(replay.requests.length, count);
});

test('confirmation cycle13 recorded 8650ms aggregate plus synthetic 50ms response overhead reacquires changed ID', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('cycle13-8650', { latency: { identity: 8_650 + 50 }, replaceFirstIdentity: true });
  assert.equal(replay.error, undefined);
  assert.equal(replay.lookups, 2);
  assert.deepEqual(replay.attributes, ['add-2:enabled', 'add-2:visible', 'add-2:hittable']);
  assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-2']);
  assert.equal(replay.driver.snapshot().firstFatal, undefined);
});

for (const fixture of recordedConfirmation) {
  test(`confirmation replays recorded ${fixture.run} enabled-stale then hypothetical ready replacement`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(fixture.run, { recorded: fixture, states: ['stale-enabled', 'ready'] });
    assert.equal(replay.error, undefined);
    assert.equal(replay.lookups, 2);
    assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-2:enabled', 'add-2:visible', 'add-2:hittable']);
    assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-2']);
    assert.equal(replay.driver.snapshot().unusable, false);
    const failed = replay.driver.snapshot().commands.find((entry) => entry.error?.includes('kAXErrorInvalidUIElement'));
    assert.ok(failed && !failed.timedOut);
  });
}

for (const initial of ['missing', 'stale-lookup', 'stale-visible', 'stale-hittable', 'disabled', 'hidden', 'not-hittable', 'indeterminate'] as const) {
  test(`confirmation hypothetical ${initial} reacquires all readiness attributes on a new ID`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(initial, { states: [initial, 'ready'] });
    assert.equal(replay.error, undefined);
    assert.equal(replay.lookups, 2);
    assert.deepEqual(replay.attributes.filter((read) => read.startsWith('add-2:')), ['add-2:enabled', 'add-2:visible', 'add-2:hittable']);
    assert.equal(replay.clicks.at(-1), 'add-2');
  });
}

test('confirmation hypothetical stale then disabled loading then ready clicks only the third candidate', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('stale-disabled-ready', { states: ['stale-enabled', 'disabled', 'ready'] });
  assert.equal(replay.error, undefined);
  assert.equal(replay.lookups, 3);
  assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-2:enabled', 'add-3:enabled', 'add-3:visible', 'add-3:hittable']);
  assert.equal(replay.clicks.at(-1), 'add-3');
});

test('confirmation admits a synthetic late enabled prefix without an early click', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('late-ready-27s', {
    states: [...Array.from({ length: 8 }, () => 'disabled' as const), 'ready'],
    after: (boundary) => boundary === 'lookup' ? 3_000 : 0,
  });
  assert.equal(replay.error, undefined);
  assert.equal(replay.lookups, 9);
  assert.deepEqual(replay.attributes, [
    ...Array.from({ length: 8 }, (_value, index) => `add-${index + 1}:enabled`),
    'add-9:enabled', 'add-9:visible', 'add-9:hittable',
  ]);
  assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-9']);
  assert.ok(replay.observations.some((entry) => entry.lookup === 9 && entry.boundary === 'enabled' && entry.now >= 25_000 && entry.now <= 30_000));
  const readyEnabled = replay.observations.findIndex((entry) => entry.lookup === 9 && entry.boundary === 'enabled');
  assert.deepEqual(replay.observations.slice(readyEnabled, readyEnabled + 4).map((entry) => entry.boundary), ['enabled', 'visible', 'hittable', 'identity']);
  assert.equal(replay.driver.snapshot().unusable, false);
});

test('confirmation child starts after activity click and keeps its full completion window', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('activity-click-duration', {
    states: [...Array.from({ length: 15 }, () => 'disabled' as const), 'ready'],
    activityDuration: 4_000,
    after: (boundary) => boundary === 'lookup' ? 3_000 : 0,
  });
  assert.equal(replay.error, undefined);
  assert.equal(replay.lookups, 16);
  assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-16']);
  assert.ok(replay.observations.some((entry) => entry.lookup === 16 && entry.boundary === 'enabled' && entry.now === 52_000));
  assert.equal(replay.driver.snapshot().unusable, false);
});

test('confirmation has no fourth-read duplicate validation outside its observation boundary', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('fourth-read', { fourthReadStale: true });
  assert.equal(replay.error, undefined);
  assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
  assert.equal(replay.lookups, 1);
  assert.equal(replay.clicks.at(-1), 'add-1');
});

for (const state of ['missing', 'stale-enabled', 'stale-visible', 'stale-hittable', 'disabled', 'hidden', 'not-hittable', 'indeterminate'] as const) {
  test(`confirmation persistent hypothetical ${state} observes while complete prefixes fit, then stops without a click`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`persistent-${state}`, { states: [state], persistent: true });
    assert.match(String(replay.error), /Add: confirmation control was not ready/u);
    assert.match(String(replay.error), state === 'missing' ? /no such element/u : state.startsWith('stale') ? /stale element reference/u : new RegExp(state, 'u'));
    assert.ok(replay.lookups > 8 && replay.lookups <= 24);
    assert.ok(replay.now > 30_000 && replay.now <= 75_000);
    assert.equal(replay.clicks.length, 2);
    assert.equal(replay.driver.snapshot().unusable, false);
  });
}

for (const prefix of ['foreground', 'lookup', 'identity', 'enabled'] as const) {
  for (const exact of [true, false]) {
    test(`confirmation ${prefix} prefix ${exact ? 'threshold' : 'threshold-minus-one'} admits only complete allowances`, async () => {
      const skip = requireXmlLint();
      if (skip) return skip;
      const replay = await confirmationReplay(`prefix-${prefix}-${exact ? 'exact' : 'minus-one'}`, {
        initialConfirmationRemaining: prefix === 'foreground' ? exact ? 5_000 : 4_999 : 20_000,
        remainingAt: (boundary, _lookup, identityRead) => {
          if (prefix === 'lookup' && boundary === 'foreground') return exact ? 8_000 : 7_999;
          if (prefix === 'identity' && boundary === 'lookup') return exact ? 12_000 : 11_999;
          if (prefix === 'identity' && boundary === 'identity' && identityRead === 1) return 4_999;
          if (prefix === 'enabled' && boundary === 'identity' && identityRead === 1) return exact ? 5_000 : 4_999;
          return undefined;
        },
      });
      assert.match(String(replay.error), /Add: confirmation control was not ready/u);
      assert.deepEqual(replay.clicks, ['share', 'target-1']);
      const commands = replay.driver.snapshot().commands;
      const targetClick = commands.findIndex((entry) => entry.path.endsWith('/target-1/click'));
      const confirmation = commands.slice(targetClick + 1).filter((entry) => !entry.path.endsWith('/appium/settings'));
      assert.ok(confirmation.every((entry) => !entry.error && !entry.timedOut));
      assert.ok(confirmation.every((entry) => entry.path.endsWith('/execute/sync') ? entry.timeoutMs === 5_000
        : entry.path.endsWith('/element') ? entry.timeoutMs === 8_000
        : entry.path.endsWith('/elements') ? entry.timeoutMs === 12_000
        : entry.path.includes('/attribute/') ? entry.timeoutMs === 5_000
        : true));
      const afterTarget = replay.requests.slice(targetClick + 1);
      const foregroundReads = afterTarget.filter((request) => request.body.script === 'mobile: activeAppInfo');
      const canonicalReads = afterTarget.filter((request) => request.path.endsWith('/elements'));
      if (prefix === 'foreground') {
        assert.equal(foregroundReads.length, exact ? 1 : 0);
        assert.equal(replay.lookups, 0);
      }
      if (prefix === 'lookup') {
        assert.equal(foregroundReads.length, 1);
        assert.equal(replay.lookups, exact ? 1 : 0);
        assert.equal(canonicalReads.length, 0);
      }
      if (prefix === 'identity') {
        assert.equal(foregroundReads.length, 1);
        assert.equal(replay.lookups, 1);
        assert.equal(canonicalReads.length, exact ? 1 : 0);
      }
      if (prefix === 'enabled') assert.deepEqual(replay.attributes, exact ? ['add-1:enabled'] : []);
      assert.ok(replay.observations.every((entry, index, all) => index === 0 || entry.now >= all[index - 1].now));
      assert.equal(replay.driver.snapshot().unusable, false);
    });
  }
}

for (const suffix of ['enabled', 'visible', 'hittable', 'identity', 'click'] as const) {
  for (const exact of [true, false]) {
    test(`confirmation ${suffix} completion ${exact ? 'threshold' : 'threshold-minus-one'} preserves the final click boundary`, async () => {
      const skip = requireXmlLint();
      if (skip) return skip;
      const replay = await confirmationReplay(`completion-${suffix}-${exact ? 'exact' : 'minus-one'}`, {
        initialConfirmationRemaining: 40_000,
        afterConfirmationValidation: suffix === 'click' && !exact ? 41_000 : undefined,
        remainingAt: (boundary, _lookup, identityRead) => {
          if (boundary === 'enabled') return suffix === 'enabled' ? exact ? 27_000 : 26_999 : 27_000;
          if (boundary === 'visible') return suffix === 'visible' ? exact ? 22_000 : 21_999 : 22_000;
          if (boundary === 'hittable') return suffix === 'hittable' ? exact ? 17_000 : 16_999 : 17_000;
          if (boundary === 'identity' && identityRead === 2) return suffix === 'identity' ? exact ? 5_000 : 4_999 : 5_000;
          return undefined;
        },
      });
      const expectedAttributes = suffix === 'enabled'
        ? ['add-1:enabled']
        : suffix === 'visible'
          ? ['add-1:enabled', 'add-1:visible']
          : ['add-1:enabled', 'add-1:visible', 'add-1:hittable'];
      if (exact) {
        assert.equal(replay.error, undefined);
        assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
        assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-1']);
      } else {
        assert.ok(replay.error);
        if (suffix === 'click') assert.match(String(replay.error), /insufficient time to complete confirmation click/u);
        assert.deepEqual(replay.attributes, expectedAttributes);
        assert.deepEqual(replay.clicks, ['share', 'target-1']);
      }
      const commands = replay.driver.snapshot().commands;
      const targetClick = commands.findIndex((entry) => entry.path.endsWith('/target-1/click'));
      const confirmation = commands.slice(targetClick + 1).filter((entry) => !entry.path.endsWith('/appium/settings'));
      assert.ok(confirmation.every((entry) => !entry.error && !entry.timedOut));
      assert.ok(confirmation.every((entry) => entry.path.endsWith('/element') ? entry.timeoutMs === 8_000
        : entry.path.endsWith('/elements') ? entry.timeoutMs === 12_000
        : entry.path.includes('/attribute/') || entry.path.endsWith('/click') || entry.path.endsWith('/execute/sync') ? entry.timeoutMs === 5_000
        : true));
    });
  }
}

for (const identityRead of [1, 2]) {
  test(`confirmation stale identity read ${identityRead} restarts the complete candidate validation`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`identity-stale-${identityRead}`, { staleIdentityRead: identityRead });
    assert.equal(replay.error, undefined);
    assert.equal(replay.lookups, 2);
    assert.deepEqual(replay.attributes.filter((read) => read.startsWith('add-2:')), ['add-2:enabled', 'add-2:visible', 'add-2:hittable']);
    assert.equal(replay.attributes.length, identityRead === 1 ? 3 : 6);
    assert.equal(replay.clicks.at(-1), 'add-2');
  });
}

test('confirmation hypothetical same-dialog Add replacement reacquires a complete fresh triplet before clicking once', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('same-dialog-replacement', { replaceReadyIdentity: 'once', after: () => 500 });
  assert.equal(replay.error, undefined);
  assert.equal(replay.lookups, 2);
  assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable', 'add-2:enabled', 'add-2:visible', 'add-2:hittable']);
  assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-2']);
  assert.deepEqual(replay.observations.map((entry) => `${entry.lookup}:${entry.boundary}`), [
    '0:foreground', '1:lookup', '1:identity', '1:enabled', '1:visible', '1:hittable', '1:identity',
    '1:foreground', '2:lookup', '2:identity', '2:enabled', '2:visible', '2:hittable', '2:identity', '2:click',
  ]);
  assert.ok(replay.now < 15_000);
  assert.equal(replay.driver.snapshot().unusable, false);
});

for (const remaining of [74_000, 76_000]) {
  test(`confirmation hypothetical repeated same-dialog Add replacements do not renew the ${remaining}ms child deadline`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`same-dialog-deadline-${remaining}`, {
      replaceReadyIdentity: 'always', parentRemaining: remaining,
      after: (boundary) => boundary === 'identity' ? 1_000 : 0,
    });
    assert.match(String(replay.error), /confirmation control was not ready.*Add control was replaced/u);
    assert.ok(replay.lookups > 8 && replay.lookups <= 24);
    assert.deepEqual(replay.clicks, ['share', 'target-1']);
    assert.ok(replay.now - (120_000 - remaining) < 75_000);
    const commands = replay.driver.snapshot().commands;
    const targetClick = commands.findIndex((entry) => entry.path.endsWith('/target-1/click'));
    assert.ok(commands.slice(targetClick + 1).every((entry) => entry.timeoutMs === (entry.path.endsWith('/appium/settings') ? 2_000 : entry.path.endsWith('/elements') ? 12_000 : entry.path.endsWith('/element') ? 8_000 : 5_000) && !entry.error && !entry.timedOut));
    assert.equal(replay.driver.snapshot().unusable, false);
  });
}

for (const dialog of ['replaced-after-ready', 'ambiguous-after-ready'] as const) {
  test(`confirmation hypothetical ${dialog} identity after the ready triplet still prevents the final click`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(dialog, { dialog });
    assert.match(String(replay.error), /confirmation identity was replaced/u);
    assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
    assert.equal(replay.lookups, 1);
    assert.equal(replay.clicks.length, 2);
  });
}

for (const dialog of ['different', 'hidden', 'unrelated-add', 'ambiguous', 'replaced'] as const) {
  test(`confirmation hypothetical ${dialog} dialog cannot authorize a same-labelled Add`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`dialog-${dialog}`, { dialog, states: dialog === 'replaced' ? ['stale-enabled', 'ready'] : undefined, persistent: true });
    assert.match(String(replay.error), /confirmation.*(?:identity|replaced)/u);
    assert.equal(replay.clicks.length, 2);
    assert.equal(replay.attributes.some((read) => dialog === 'replaced' ? read.startsWith('add-2') : true), false);
  });
}

for (const foreground of ['com.example.other', '', 'com.apple.SafariViewService']) {
  test(`confirmation native foreground ${foreground || 'unknown'} remains identity-bound`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`foreground-${foreground || 'unknown'}`, { foreground });
    if (foreground === 'com.apple.SafariViewService') {
      assert.equal(replay.error, undefined);
      assert.equal(replay.clicks.length, 3);
    } else {
      assert.match(String(replay.error), /IOS_SHARE:.*foreground/u);
      assert.equal(replay.clicks.length, 2);
    }
  });
}

for (const boundary of ['lookup', 'identity', 'enabled', 'visible', 'hittable'] as const) {
  for (const fault of ['unrelated', 'invalid-session', 'misleading-404', 'malformed', 'malformed-value', 'transport'] as const) {
    test(`confirmation ${boundary} ${fault} propagates without candidate retries`, async () => {
      const skip = requireXmlLint();
      if (skip) return skip;
      const replay = await confirmationReplay(`${boundary}-${fault}`, { fault, faultAt: boundary });
      assert.ok(replay.error);
      assert.equal(replay.lookups, 1);
      assert.equal(replay.clicks.length, 2);
      assert.match(String(replay.error), fault === 'malformed-value' ? /(?:malformed|invalid).*response/u : fault === 'transport' ? /connection reset/u : /APPIUM_(?:COMMAND|HTTP)/u);
    });
  }
}

for (const boundary of ['lookup', 'identity', 'enabled', 'visible', 'hittable'] as const) {
  test(`confirmation ${boundary} receives a complete allowance or no command at child exhaustion`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`admission-${boundary}`, { after: (current) => current === boundary ? 70_001 : 0 });
    assert.match(String(replay.error), /Add: confirmation control was not ready/u);
    assert.equal(replay.clicks.length, 2);
    const commands = replay.driver.snapshot().commands;
    const targetClick = commands.findIndex((entry) => entry.path.endsWith('/target-1/click'));
    const confirmation = commands.slice(targetClick + 1);
    assert.ok(confirmation.every((entry) => !entry.error && !entry.timedOut));
    assert.ok(confirmation.filter((entry) => /\/element(?:s|\/add-1\/attribute\/\w+)?$/u.test(entry.path)).every((entry) => entry.timeoutMs === (entry.path.endsWith('/elements') ? 12_000 : entry.path.endsWith('/element') ? 8_000 : 5_000)), 'never dispatch a shortened lookup/read');
    assert.equal(replay.lookups, 1);
    if (boundary === 'lookup') assert.equal(replay.observations.some((entry) => entry.boundary === 'identity'), false);
    if (boundary === 'enabled') assert.deepEqual(replay.attributes, ['add-1:enabled']);
    if (boundary === 'visible') assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible']);
    assert.equal(replay.driver.snapshot().unusable, false);
  });
}

test('confirmation foreground read cannot consume the allowance of the next lookup', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('foreground-admission', { after: (boundary) => boundary === 'foreground' ? 70_001 : 0 });
  assert.match(String(replay.error), /Add: confirmation control was not ready/u);
  assert.equal(replay.lookups, 0);
  assert.equal(replay.clicks.length, 2);
  assert.equal(replay.driver.snapshot().unusable, false);
});

for (const remaining of [4_999, 7_999, 8_000, 66_999, 71_999]) {
  test(`confirmation parent budget ${remaining} never admits a partial tail observation`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`parent-${remaining}`, { parentRemaining: remaining, after: (boundary) => boundary === 'lookup' ? 1_001 : 0 });
    assert.match(String(replay.error), /insufficient (?:whole transaction allowance|time to complete native scroll and hierarchy verification)/u);
    assert.equal(replay.lookups, 0);
    assert.deepEqual(replay.attributes, []);
    assert.equal(replay.clicks.length, 1);
    assert.equal(replay.driver.snapshot().unusable, false);
  });
}

test('confirmation exhaustion retains the last meaningful loading observation', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  const replay = await confirmationReplay('last-loading-state', {
    states: ['disabled', 'ready'], after: (boundary, lookup) => lookup === 2 && boundary === 'enabled' ? 70_001 : 0,
  });
  assert.match(String(replay.error), /confirmation control was not ready.*disabled/u);
  assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-2:enabled']);
  assert.equal(replay.lookups, 2);
  assert.equal(replay.clicks.length, 2);
});

test('confirmation final click requires its complete parent allowance after all reads', async () => {
  const skip = requireXmlLint();
  if (skip) return skip;
  let identityReads = 0;
  const replay = await confirmationReplay('click-admission', {
    parentRemaining: 74_000, after: (boundary) => boundary === 'identity' && ++identityReads === 2 ? 65_001 : 0,
  });
  assert.match(String(replay.error), /insufficient time to complete confirmation click/u);
  assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
  assert.equal(replay.lookups, 1);
  assert.equal(replay.clicks.length, 2);
});

for (const boundary of ['lookup', 'identity', 'enabled', 'click'] as const) {
  for (const fault of ['hung', 'interrupted'] as const) {
    test(`confirmation ${fault} ${boundary} preserves single-flight, first fatal evidence and quarantine`, async () => {
      const skip = requireXmlLint();
      if (skip) return skip;
      const replay = await confirmationReplay(`${fault}-${boundary}`, { fault, faultAt: boundary });
      assert.match(String(replay.error), /APPIUM_TIMEOUT/u);
      assert.equal(replay.lookups, 1);
      assert.equal(replay.clicks.length, boundary === 'click' ? 3 : 2);
      assert.equal(replay.driver.snapshot().unusable, true);
      const first = replay.driver.snapshot().firstFatal;
      assert.equal(first?.code, 'APPIUM_TIMEOUT');
      const count = replay.requests.length;
      await assert.rejects(() => replay.driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
      await assert.rejects(() => replay.platform.installFromBrowser(), /APPIUM_SESSION_UNUSABLE/u);
      assert.equal(replay.requests.length, count);
      assert.deepEqual(replay.driver.snapshot().firstFatal, first);
    });
  }
}

for (const scenario of [
  { fault: 'body-hung' as const, boundary: 'lookup' as const, code: 'APPIUM_TIMEOUT', identityRead: undefined },
  { fault: 'body-hung' as const, boundary: 'identity' as const, code: 'APPIUM_TIMEOUT', identityRead: 2 },
  { fault: 'body-interrupted' as const, boundary: 'lookup' as const, code: 'APPIUM_INTERRUPTED', identityRead: undefined },
  { fault: 'body-interrupted' as const, boundary: 'identity' as const, code: 'APPIUM_INTERRUPTED', identityRead: 2 },
  { fault: 'body-late' as const, boundary: 'identity' as const, code: 'APPIUM_TIMEOUT', identityRead: 2 },
]) {
  test(`confirmation ${scenario.fault} ${scenario.boundary} body preserves fatal quarantine and no later work`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`${scenario.fault}-${scenario.boundary}-${scenario.identityRead || 'pending'}`, {
      fault: scenario.fault, faultAt: scenario.boundary, faultOnIdentityRead: scenario.identityRead,
    });
    assert.match(String(replay.error), new RegExp(scenario.code, 'u'));
    assert.equal(replay.driver.snapshot().unusable, true);
    assert.equal(replay.driver.snapshot().firstFatal?.code, scenario.code);
    assert.deepEqual(replay.clicks, ['share', 'target-1']);
    assert.equal(replay.requests.some((request) => request.path.endsWith('/element/add-1/click')), false);
    const settingsWrites = replay.requests.filter((request) => request.path.endsWith('/appium/settings') && 'waitForIdleTimeout' in (request.body.settings || {}));
    assert.equal(settingsWrites.length, 1);
    const first = replay.driver.snapshot().firstFatal;
    const count = replay.requests.length;
    await assert.rejects(() => replay.driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
    await assert.rejects(() => replay.platform.installFromBrowser(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(replay.requests.length, count);
    assert.deepEqual(replay.driver.snapshot().firstFatal, first);
    if (scenario.fault === 'body-late') {
      await wait(1_000);
      assert.equal(replay.requests.length, count);
      assert.deepEqual(replay.driver.snapshot().firstFatal, first);
    }
  });
}

for (const boundary of ['lookup', 'identity'] as const) {
  test(`confirmation cycle03 delayed ${boundary} completes the full readiness transaction once`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`cycle03-${boundary}`, { latency: { [boundary]: 6_000 } });
    assert.equal(replay.error, undefined);
    assert.equal(replay.lookups, 1);
    assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
    assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-1']);
    assert.equal(replay.driver.snapshot().unusable, false);
    const commands = replay.driver.snapshot().commands;
    const start = commands.findIndex((entry) => entry.path.endsWith('/target-1/click'));
    assert.ok(commands.slice(start + 1).filter((entry) => /\/elements?$/u.test(entry.path)).every((entry) => entry.timeoutMs === (entry.path.endsWith('/elements') ? 12_000 : 8_000)));
  });
}

for (const boundary of ['lookup', 'identity'] as const) {
  for (const state of ['ready', 'missing'] as const) {
    test(`confirmation cycle03 late ${boundary} ${state} cannot clear quarantine or retry an action`, async () => {
      const skip = requireXmlLint();
      if (skip) return skip;
      const replay = await confirmationReplay(`cycle03-late-${boundary}-${state}`, {
        latency: { [boundary]: boundary === 'identity' ? 12_200 : 8_200 }, states: [boundary === 'lookup' ? state : 'ready'], dialog: boundary === 'identity' && state === 'missing' ? 'different' : undefined,
      });
      assert.match(String(replay.error), /APPIUM_TIMEOUT/u);
      assert.equal(replay.lookups, 1);
      assert.deepEqual(replay.clicks, ['share', 'target-1']);
      const first = replay.driver.snapshot().firstFatal;
      const count = replay.requests.length;
      await wait(300);
      await assert.rejects(() => replay.platform.installFromBrowser(), /APPIUM_SESSION_UNUSABLE/u);
      assert.deepEqual(replay.driver.snapshot().firstFatal, first);
      assert.equal(replay.requests.length, count);
    });
  }
}

for (const fault of ['stale', 'transport'] as const) {
  test(`confirmation final click ${fault} is never retried or restarted`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`click-${fault}`, { fault, faultAt: 'click' });
    assert.ok(replay.error);
    assert.equal(replay.lookups, 1);
    assert.deepEqual(replay.attributes, ['add-1:enabled', 'add-1:visible', 'add-1:hittable']);
    assert.deepEqual(replay.clicks, ['share', 'target-1', 'add-1']);
    if (fault === 'stale') assert.equal(isRetryableElementLookupError(replay.error), true, 'generic classification must not authorize action retries');
  });
}

test('navigation reuses the required simulator boot observation instead of enumerating again', async () => {
  const { platform, requests } = await adapter('navigation-reused-boot', () => {
    throw new Error('no optional navigation command is admissible');
  });
  (platform as any).simulatorReadyAt = '2026-01-01T00:00:00.000Z';
  await (platform as any).captureNavigationState('before');
  assert.equal(requests.length, 1);
  const event = (platform.evidenceSnapshot().events as any[]).find((candidate) => candidate.operation === 'before-openurl-boot-state');
  assert.equal(event.detail.outcome, 'reused');
  assert.equal(event.detail.observedAt, '2026-01-01T00:00:00.000Z');
});

test('recorded openurl timeout preserves signal and duration without inventing an application exit status', async () => {
  const process = recorded.openurlFailure.detail.process;
  const error = new CommandError('xcrun', ['simctl', 'openurl', 'protocol-only', `${origin}/`], {
    ...process, code: process.exitCode,
  });
  const evidence = iosOpenURLProcessEvidence(error);
  assert.equal(evidence.timedOut, true);
  assert.equal(evidence.signal, 'SIGTERM');
  assert.equal(evidence.durationMs, 35085);
  assert.equal(evidence.normalizedExitCode, 1);
  assert.equal('exitCode' in evidence, false);
  assert.equal(evidence.stdout, '');
  assert.equal(evidence.stderr, '');
});

for (const mode of ['recorded-timeout', 'uncertain-failure', 'real-process-timeout', 'diagnostic-failure', 'discovery-interrupted', 'pre-diagnostic-timeout', 'post-simulator-timeout'] as const) {
  test(`openurl ${mode} records bounded serial diagnostics without reissuing navigation`, async () => {
    let processActive = false;
    const { platform, requests, outputDir } = await adapter(`navigation-${mode}`, ({ path, body, signal }) => {
      assert.equal(processActive, false, 'no Appium request while a process is unsettled');
      if (path.endsWith('/context')) return value(null);
      if (path.endsWith('/source')) return value(browser);
      if (body.script === 'mobile: activeAppInfo') return value({ bundleId: 'com.apple.mobilesafari', pid: 20640 });
      if (body.script === 'mobile: getContexts') {
        if (mode === 'discovery-interrupted') {
          return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
        }
        return value([recorded.contexts[0], { ...recorded.contexts[1], url: 'about:blank' }]);
      }
      throw new Error(`unexpected navigation diagnostic ${path} ${JSON.stringify(body)}`);
    });
    assert.equal(typeof (platform as any).navigationCommand, 'function', 'diagnostics must be available before invoking any process');
    if (mode === 'discovery-interrupted') {
      const discover = platform.driver.contextMetadata.bind(platform.driver);
      platform.driver.contextMetadata = () => discover(1_000);
    }
    const processes: Array<{ binary: string; args: string[]; timeoutMs: number }> = [];
    let listCalls = 0;
    let navigationError: unknown;
    (platform as any).navigationCommand = async (binary: string, args: string[], timeoutMs: number) => {
      assert.equal(processActive, false);
      processActive = true;
      processes.push({ binary, args, timeoutMs });
      try {
        if (args.includes('openurl')) {
          assert.equal(timeoutMs, 30_000);
          if (mode === 'real-process-timeout') {
            try { return await command('/bin/sleep', ['2'], 40); }
            catch (error) { navigationError = error; throw error; }
          }
          navigationError = new CommandError('xcrun', args, {
            stdout: '', stderr: '', code: 1, durationMs: 35085,
            timedOut: mode !== 'uncertain-failure', signal: mode === 'uncertain-failure' ? undefined : 'SIGTERM',
          });
          throw navigationError;
        }
        if (binary === '/usr/bin/log') {
          assert.ok(args.includes('--start') && args.includes('--end'));
          assert.ok(timeoutMs <= 5_000);
          if (mode === 'diagnostic-failure') throw new Error('host log unavailable');
          return command('/usr/bin/printf', ['%s', 'host trace #invite=fixture-secret'], timeoutMs);
        }
        assert.deepEqual(args, ['simctl', 'list', 'devices', 'available', '--json']);
        assert.ok(timeoutMs <= 3_000);
        listCalls += 1;
        if ((mode === 'pre-diagnostic-timeout' && listCalls === 1) || (mode === 'post-simulator-timeout' && listCalls === 2)) {
          return command('/bin/sleep', ['2'], 40);
        }
        return command('/usr/bin/printf', ['%s', JSON.stringify({ devices: { runtime: [{ udid: 'protocol-only', state: 'Booted', isAvailable: true }] } })], timeoutMs);
      } finally {
        processActive = false;
      }
    };
    await assert.rejects(() => platform.openSetupURL(`${origin}/#invite=fixture-secret`), (error) => error instanceof Error && /IOS_NAVIGATION/u.test(error.message) && error.cause === navigationError);
    assert.equal(processes.filter((process) => process.args.includes('openurl')).length, 1);
    assert.equal(requests.some((request) => request.path.endsWith('/url') || /activateApp|navigate/u.test(request.body.script || '')), false);
    const events = platform.evidenceSnapshot().events as any[];
    const start = events.find((event) => event.operation === 'simctl openurl started');
    const failure = events.find((event) => event.operation === 'simctl openurl failed');
    assert.ok(start && failure);
    assert.ok(Date.parse(start.at) <= Date.parse(failure.at));
    assert.equal(failure.detail.process.normalizedExitCode, 1);
    assert.equal(failure.detail.process.timedOut, mode !== 'uncertain-failure');
    assert.equal(failure.detail.startedAt, start.detail.startedAt);
    assert.ok(events.some((event) => event.operation === 'before-openurl-boot-state'
      && event.detail.outcome === (mode === 'pre-diagnostic-timeout' ? 'unavailable' : 'collected')));
    assert.ok(events.some((event) => event.operation === 'after-openurl-foreground' && event.detail.outcome === 'collected'));
    if (mode === 'post-simulator-timeout') {
      assert.ok(events.some((event) => event.operation === 'after-openurl-boot-state' && event.detail.outcome === 'unavailable'));
    }
    assert.ok(events.some((event) => event.operation === 'after-openurl-pages' && event.detail.outcome === (mode === 'discovery-interrupted' ? 'failed' : 'collected')));
    if (mode === 'discovery-interrupted') {
      assert.equal(platform.driver.snapshot().unusable, true);
      const first = platform.driver.snapshot().firstFatal;
      const count = requests.length;
      await assert.rejects(() => platform.attachToInstalledView(), /APPIUM_SESSION_UNUSABLE/u);
      assert.equal(requests.length, count);
      assert.deepEqual(platform.driver.snapshot().firstFatal, first);
      assert.equal(processes.some((process) => process.binary === '/usr/bin/log'), true);
      return;
    }
    assert.match(await readFile(join(outputDir, 'ios-after-openurl-hierarchy.xml'), 'utf8'), /XCUIElementType/u);
    if (mode !== 'diagnostic-failure') {
      const text = await readFile(join(outputDir, 'ios-openurl-host.log'), 'utf8');
      assert.match(text, /host trace/u);
      assert.equal(text.includes('fixture-secret'), false);
    }
    assert.ok(events.some((event) => event.operation === 'openurl-host-log' && event.detail.outcome === (mode === 'diagnostic-failure' ? 'unavailable' : 'collected')));
  });
}

test('an insufficient navigation budget records prerequisite skips without admitting a command', async () => {
  let now = 0;
  const { platform, requests } = await adapter('navigation-budget', () => { throw new Error('no diagnostic request is admissible'); }, () => now);
  (platform as any).navigationCommand = () => { throw new Error('no process is admissible'); };
  now = 119_900;
  await assert.rejects(() => platform.openSetupURL(`${origin}/`), /insufficient time for a complete openurl command/u);
  assert.equal(requests.length, 1);
  const events = platform.evidenceSnapshot().events as any[];
  assert.ok(events.length > 0 && events.every((event) => event.detail.outcome === 'skipped'));
});

test('Plan13 recorded provenance retains its historical tool tuple and run identities', async () => {
  assert.deepEqual({
    sourceCommit: recordedIteration13.sourceCommit,
    versions: recordedIteration13.versions,
    runs: {
      safariAUTFailure: recordedIteration13.safariAUTFailure.run,
      share: recordedIteration13.share.run,
      confirmation: recordedIteration13.confirmation.run,
    },
  }, {
    sourceCommit: 'f35ab6ffa9defa57ef653eb39a5f351bda4dd23c',
    versions: { appium: '3.1.1', xcuitest: '12.10.0', wda: '16.12.1' },
    runs: { safariAUTFailure: '34488014724', share: '34488020101', confirmation: '34488014724' },
  });
  for (const [file, expected] of Object.entries(recordedIteration13.files)) {
    assert.equal(createHash('sha256').update(await readFile(join(fixtureDir, file))).digest('hex'), expected);
  }
});

const lifecycleError = recordedIteration13.safariAUTFailure.response;
type HomeLaunchMode = 'ready' | 'duplicate-reference' | 'ambiguous' | 'missing' | 'loading' | 'timeout' | 'readiness-transition' | 'not-ready' | 'malformed-reference';

async function lifecycleReplay(name: string, options: { foreground?: string; home?: HomeLaunchMode; page?: string } = {}) {
  const homeMode = options.home || 'ready';
  const state = {
    safariRunning: true, foreground: options.foreground ?? 'com.apple.mobilesafari', installed: false, pid: 16089 as unknown,
    overlay: '', documentOrigin: origin, standalone: true, url: `${origin}/`,
    settingsFault: '', stateFault: undefined as unknown, activeFault: undefined as unknown,
    alertFault: false, ignoreHome: false, wrongLaunch: '', systemObservationFault: '', slowInstalledObservation: false,
    systemName: 'SpringBoard', systemRootCount: 1,
  };
  let settings: Record<string, unknown> = { defaultActiveApplication: 'auto', respectSystemAlerts: false };
  let homeObservations = 0;
  const appState = (bundle: string) => {
    if (bundle === 'com.apple.webapp' && state.stateFault !== undefined) return state.stateFault;
    if (bundle === state.foreground) return 4;
    if (bundle === 'com.apple.springboard') return state.overlay && state.overlay !== 'app-dialog' ? 4 : state.foreground === 'com.apple.webapp' ? 4 : 2;
    if (bundle === 'com.apple.webapp') return state.installed ? 2 : 1;
    return state.safariRunning ? 2 : 1;
  };
  const active = () => {
    const target = String(settings.defaultActiveApplication);
    if (target !== 'auto' && appState(target) === 4) return target;
    if (state.safariRunning && state.foreground === 'com.apple.mobilesafari') {
      return settings.respectSystemAlerts && state.overlay ? 'com.apple.springboard' : 'com.apple.mobilesafari';
    }
    if (!state.safariRunning) return '';
    return state.overlay ? 'com.apple.springboard' : state.foreground;
  };
  const replay = await adapter(`lifecycle-${name}`, async ({ path, body, method, signal }) => {
    if (path.endsWith('/appium/settings')) {
      if (method === 'GET') return value(state.settingsFault === 'readback' ? { ...settings, defaultActiveApplication: 'auto' } : state.settingsFault === 'malformed-readback' ? [] : settings);
      if (state.settingsFault === 'unsupported') return Response.json({ value: { error: 'invalid argument', message: 'unsupported setting' } }, { status: 400 });
      settings = { ...settings, ...body.settings };
      return value(state.settingsFault === 'malformed-update' ? {} : null);
    }
    if (path.endsWith('/context')) return value(null);
    if (body.script === 'mobile: queryAppState') return value(appState(body.args.bundleId));
    if (body.script === 'mobile: activeAppInfo') {
      const bundleId = active();
      if (!bundleId) return Response.json({ value: lifecycleError }, { status: 400 });
      return value(state.activeFault ?? { bundleId, pid: bundleId === 'com.apple.webapp' ? state.pid : 42 });
    }
    if (path.endsWith('/alert/text')) {
      if (state.slowInstalledObservation && state.foreground === 'com.apple.webapp') await wait(2_300);
      if (!active()) return Response.json({ value: lifecycleError }, { status: 400 });
      if (state.alertFault) return Response.json({ value: { error: 'unknown command', message: 'no such alert is not the error code' } }, { status: 404 });
      if (state.overlay === 'alert' || state.overlay === 'app-dialog') return value('System or native dialog');
      return Response.json({ value: { error: 'no such alert', message: 'No alert is open' } }, { status: 404 });
    }
    if (body.script === 'mobile: pressButton') {
      assert.equal(body.args.name, 'home');
      if (!state.ignoreHome) state.foreground = 'com.apple.springboard';
      return value(null);
    }
    if (body.script === 'mobile: swipe') {
      assert.equal(active(), 'com.apple.springboard');
      return value(null);
    }
    if (body.script === 'mobile: activateApp') {
      state.foreground = body.args.bundleId;
      return value(null);
    }
    if (body.script === 'mobile: terminateApp') {
      assert.equal(body.args.bundleId, 'com.apple.webapp');
      state.installed = false;
      state.foreground = 'com.apple.springboard';
      return value(true);
    }
    if (path.endsWith('/element/springboard-root/elements')) {
      if (state.slowInstalledObservation && state.foreground === 'com.apple.webapp') await wait(2_800);
      assert.equal(body.using, 'xpath');
      if (state.systemObservationFault === 'empty') return value([]);
      if (state.systemObservationFault === 'malformed') return value({});
      if (state.systemObservationFault === 'replaced') return value([element('different-root')]);
      const overlay = state.overlay && state.overlay !== 'app-dialog';
      const xml = `<XCUIElementTypeApplication name="${state.systemName}">${overlay ? state.overlay === 'alert' ? '<XCUIElementTypeAlert/>' : `<XCUIElementTypeOther name="${state.overlay}"/>` : ''}</XCUIElementTypeApplication>`;
      const scopedXPath = String(body.value).split(' | ').map((part) => `/XCUIElementTypeApplication/${part}`).join(' | ');
      assert.equal(xpathCount(xml, scopedXPath), overlay ? 2 : 1);
      return value([element('springboard-root'), ...(overlay ? [element('system-overlay')] : [])]);
    }
    if (path.endsWith('/elements')) {
      assert.equal(active(), 'com.apple.springboard');
      if (body.value.includes('XCUIElementTypePageIndicator')) {
        if (state.foreground !== 'com.apple.springboard') return value([]);
        if (homeMode === 'timeout') return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
        if (homeMode === 'loading') return value([]);
        homeObservations += 1;
        const icons = homeMode === 'missing' ? [] : homeMode === 'ambiguous' ? [element('home-icon'), element('home-icon-2')] : [element('home-icon')];
        const references: unknown[] = [element('home-container'), ...icons, element('home-page'), ...(homeMode === 'duplicate-reference' ? [element('home-icon')] : [])];
        if (homeMode === 'malformed-reference') references.push({});
        return value(references);
      }
      if (body.value.includes('XCUIElementTypeApplication')) {
        const xml = `<AppiumAUT>${Array.from({ length: state.systemRootCount }, () => `<XCUIElementTypeApplication name="${state.systemName}"/>`).join('')}</AppiumAUT>`;
        return value(Array.from({ length: xpathCount(xml, String(body.value)) }, (_, index) => element(index ? `other-root-${index}` : 'springboard-root')));
      }
      return value([element('home-icon')]);
    }
    if (path.includes('/home-container/attribute/')) {
      const attribute = path.split('/attribute/')[1];
      return value(attribute === 'type' ? 'XCUIElementTypeOther' : attribute === 'name' ? 'Home screen icons' : null);
    }
    if (path.includes('/home-icon/attribute/') || path.includes('/home-icon-2/attribute/')) {
      const attribute = path.split('/attribute/')[1];
      const ready = homeMode !== 'not-ready' && (homeMode !== 'readiness-transition' || homeObservations > 1);
      return value(attribute === 'type' ? 'XCUIElementTypeIcon' : attribute === 'name' ? 'Herdr Mobile Relay' : attribute === 'enabled' ? ready ? 'true' : 'false' : attribute === 'visible' || attribute === 'hittable' ? 'true' : null);
    }
    if (path.includes('/home-page/attribute/')) {
      const attribute = path.split('/attribute/')[1];
      return value(attribute === 'type' ? 'XCUIElementTypePageIndicator' : attribute === 'name' ? 'Page control' : attribute === 'value' ? options.page || 'Page 1 of 1' : null);
    }
    if (path.endsWith('/home-icon/click')) {
      state.foreground = state.wrongLaunch || 'com.apple.webapp';
      state.installed = true;
      return value(null);
    }
    if (body.script === 'mobile: getContexts') return value([published]);
    if (path.endsWith('/url')) return value(state.url);
    if (body.script?.startsWith('return {')) return value({ origin: state.documentOrigin, standalone: state.standalone, applicationInitialized: true });
    throw new Error(`unexpected lifecycle request ${path} ${JSON.stringify(body)}`);
  }, undefined, false);
  (replay.platform as any).installedBundleId = '';
  (replay.platform as any).springBoardRoot = '';
  const marker = join(replay.outputDir, 'ios-ownership');
  await writeFile(marker, 'ios:protocol-only\n');
  const owned = async (operation: () => Promise<void>) => {
    const previous = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = marker;
    try { await operation(); } finally {
      if (previous === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
      else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = previous;
    }
  };
  return { ...replay, state, settings: () => settings, owned };
}

test('Cycle4 unnamed system application retains native ownership and overlay observation', async () => {
  const a = await lifecycleReplay('unnamed-system');
  a.state.systemName = '';
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  a.state.overlay = 'SBTransientOverlayWindow';
  await assert.rejects(() => a.platform.attachToInstalledView(), /IOS_CONTEXT_OWNERSHIP/u);
});

for (const count of [0, 2]) {
  test(`Cycle4 system application root count ${count} fails before launch without retry`, async () => {
    const a = await lifecycleReplay(`root-count-${count}`);
    a.state.systemRootCount = count;
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), /SpringBoard observation root is not unique/u);
    const stopped = a.requests.length;
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), /IOS_CONTEXT_OWNERSHIP/u);
    assert.equal(a.requests.length, stopped);
    assert.equal(a.requests.some((request) => request.path.endsWith('/home-icon/click')), false);
  });
}

test('current-page-first launch clicks a ready Page 2 icon without Home navigation', async () => {
  const a = await lifecycleReplay('current-page2', { foreground: 'com.apple.springboard', page: 'Page 2 of 2' });
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: pressButton').length, 0);
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  const homeQueries = a.requests.filter((request) => request.path.endsWith('/elements') && request.body.value.includes('XCUIElementTypePageIndicator'));
  assert.equal(homeQueries.length, 1);
  assert.ok(homeQueries[0].body.value.includes("not(ancestor::*[@visible='false'])"));
  assert.equal(homeQueries[0].body.value.includes('contains('), false);
  assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 1);
  assert.equal(a.driver.snapshot().unusable, false);
});

test('non-Home foreground receives one verified Home navigation before page inspection', async () => {
  const a = await lifecycleReplay('non-home-current-page', { foreground: 'com.apple.mobilesafari' });
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: pressButton').length, 1);
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
});

test('provider UI is observed before pinning SpringBoard during retained relaunch', async () => {
  const a = await lifecycleReplay('provider-ui-retained-target');
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  assert.equal(a.platform.evidenceSnapshot().installedBundleId, 'com.apple.webapp');
  const beforeRelaunch = a.requests.length;
  await a.owned(() => a.platform.relaunchInstalledApp());
  const relaunchRequests = a.requests.slice(beforeRelaunch);
  const foregroundRead = relaunchRequests.findIndex((request) => request.body.script === 'mobile: activeAppInfo');
  const springBoardTarget = relaunchRequests.findIndex((request) => request.path.endsWith('/appium/settings')
    && request.method === 'POST' && request.body.settings.defaultActiveApplication === 'com.apple.springboard');
  assert.ok(foregroundRead >= 0);
  assert.ok(springBoardTarget >= 0);
  assert.ok(foregroundRead < springBoardTarget, 'current foreground must be observed before changing the observation target');
  assert.equal(relaunchRequests.filter((request) => request.body.script === 'mobile: pressButton').length, 1);
  assert.equal(relaunchRequests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  assert.equal(relaunchRequests.filter((request) => request.path.endsWith('/elements') && request.body.value.includes('XCUIElementTypePageIndicator')).length, 1);
  assert.equal(relaunchRequests.filter((request) => request.path.endsWith('/home-icon/click')).length, 1);
  assert.equal(relaunchRequests.some((request) => request.body.script === 'mobile: getContexts'), true);
  assert.equal(relaunchRequests.some((request) => request.path.endsWith('/url') && !request.body.url), true);
  assert.equal(relaunchRequests.some((request) => request.path.endsWith('/execute/sync') && request.body.script?.startsWith('return {')), true);
  assert.equal(a.platform.evidenceSnapshot().installedBundleId, 'com.apple.webapp');
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  assert.ok(a.platform.evidenceSnapshot().selectedInstalledContext);
  assert.equal(a.settings().defaultActiveApplication, 'com.apple.webapp');
});

test('current-page readiness transition is polled without page navigation', async () => {
  const a = await lifecycleReplay('current-page-readiness-transition', { home: 'readiness-transition' });
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 1);
  assert.equal(a.requests.filter((request) => request.path.endsWith('/elements') && request.body.value.includes('XCUIElementTypePageIndicator')).length, 2);
});

test('malformed current foreground identity fails before Home navigation', async () => {
  const a = await lifecycleReplay('malformed-current-foreground');
  a.state.activeFault = { bundleId: 42, pid: 42 };
  let first: unknown;
  await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => {
    first = error;
    return /IOS_CONTEXT_OWNERSHIP/u.test(String(error));
  });
  const stopped = a.requests.length;
  assert.equal(a.requests.some((request) => request.body.script === 'mobile: pressButton' || request.body.script === 'mobile: swipe' || request.path.endsWith('/home-icon/click')), false);
  await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), (error) => error === first);
  assert.equal(a.requests.length, stopped);
});

test('unexpected relaunch foreground fails closed before Home navigation', async () => {
  const a = await lifecycleReplay('unexpected-relaunch-foreground');
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  a.state.foreground = 'com.example.other';
  const beforeRelaunch = a.requests.length;
  let first: unknown;
  await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), (error) => {
    first = error;
    return /allowed iOS lifecycle identity/u.test(String(error));
  });
  const attempted = a.requests.slice(beforeRelaunch);
  assert.equal(attempted.filter((request) => request.path.endsWith('/appium/settings') && request.method === 'POST').length, 0);
  assert.equal(attempted.some((request) => request.body.script === 'mobile: pressButton'
    || request.body.script === 'mobile: swipe' || request.path.endsWith('/home-icon/click')), false);
  assert.equal(a.platform.evidenceSnapshot().installedBundleId, 'com.apple.webapp');
  const stopped = a.requests.length;
  await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), (error) => error === first);
  assert.equal(a.requests.length, stopped);
});

test('same Home element reference in the union is deduplicated before classification', async () => {
  const a = await lifecycleReplay('duplicate-home-reference', { home: 'duplicate-reference' });
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 1);
  assert.equal(a.requests.filter((request) => request.path.endsWith('/elements') && request.body.value.includes('XCUIElementTypePageIndicator')).length, 1);
});

test('distinct same-label Home icons fail closed before any launch action', async () => {
  const a = await lifecycleReplay('ambiguous-home-icons', { home: 'ambiguous' });
  let first: unknown;
  await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => {
    first = error;
    return /Home screen icon is ambiguous/u.test(String(error));
  });
  const stopped = a.requests.length;
  assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 0);
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => error === first);
  assert.equal(a.requests.length, stopped);
});

test('malformed extra Home element reference fails closed before any launch action', async () => {
  const a = await lifecycleReplay('malformed-home-reference', { home: 'malformed-reference' });
  let first: unknown;
  await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => {
    first = error;
    return /Home observation element reference .* malformed/u.test(String(error));
  });
  const stopped = a.requests.length;
  assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 0);
  assert.equal(a.requests.filter((request) => request.body.script === 'mobile: swipe').length, 0);
  await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), (error) => error === first);
  assert.equal(a.requests.length, stopped);
});

for (const mode of ['missing', 'loading'] as const) {
  test(`Home ${mode} never authorizes an arbitrary icon click`, async () => {
    const a = await lifecycleReplay(`home-${mode}`, { home: mode });
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), /IOS_CONTEXT/u);
    assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon/click')).length, 0);
    assert.equal(a.requests.filter((request) => request.path.endsWith('/home-icon-2/click')).length, 0);
    const swipes = a.requests.filter((request) => request.body.script === 'mobile: swipe');
    if (mode === 'loading') {
      assert.equal(swipes.length, 0);
    } else {
      assert.equal(swipes.filter((request) => request.body.args.direction === 'right').length, 8);
      assert.equal(swipes.filter((request) => request.body.args.direction === 'left').length, 7);
    }
    assert.equal(a.driver.snapshot().unusable, false);
  });
}

test('Home observation timeout quarantines the Appium session without late launch work', async () => {
  const a = await lifecycleReplay('home-timeout', { home: 'timeout' });
  await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), /APPIUM_TIMEOUT/u);
  const stopped = a.requests.length;
  const first = a.driver.snapshot().firstFatal;
  await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), /APPIUM_SESSION_UNUSABLE/u);
  assert.equal(a.requests.length, stopped);
  assert.deepEqual(a.driver.snapshot().firstFatal, first);
});

test('Plan13 lifecycle supported handoff survives obsolete Safari through background cold termination and relaunch', async () => {
  const a = await lifecycleReplay('complete');
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  a.state.safariRunning = false;
  await a.platform.attachToInstalledView();
  assert.equal(a.platform.evidenceSnapshot().nativePid, '16089');
  await a.owned(() => a.platform.backgroundApp());
  assert.equal(a.settings().defaultActiveApplication, 'com.apple.springboard');
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  await a.owned(() => a.platform.relaunchInstalledApp());
  await a.owned(() => a.platform.terminateInstalledApp());
  a.state.pid = 17001;
  await a.owned(() => a.platform.relaunchInstalledApp());
  assert.equal(a.settings().defaultActiveApplication, 'com.apple.webapp');
  assert.equal(a.platform.evidenceSnapshot().nativePid, '17001');
  const transitions = a.requests.filter((r) => r.path.endsWith('/appium/settings') && r.method === 'POST');
  assert.deepEqual(transitions.map((r) => r.body.settings.defaultActiveApplication), [
    'com.apple.springboard', 'com.apple.webapp', 'com.apple.springboard',
    'com.apple.springboard', 'com.apple.webapp', 'com.apple.springboard',
    'com.apple.springboard', 'com.apple.webapp',
  ]);
  for (const transition of transitions) {
    const index = a.requests.indexOf(transition);
    assert.equal(a.requests[index + 1].method, 'GET');
    assert.ok(a.requests[index + 1].path.endsWith('/appium/settings'));
    assert.ok(/activeAppInfo|queryAppState|pressButton|terminateApp/u.test(a.requests[index + 2].body.script || '')
      || a.requests[index + 2].path.endsWith('/context') || a.requests[index + 2].path.endsWith('/home-icon/click'));
  }
  assert.equal(a.requests.some((r) => /activateApp|launchApp/u.test(r.body.script || '') || (r.path.endsWith('/url') && r.method === 'POST')), false);
  assert.equal(a.driver.snapshot().unusable, false);
  await writeSanitizedJson(join(a.outputDir, 'ios-lifecycle-result.json'), { proofKind: 'Source-derived WDA 16.12.8 branch model with hypothetical lifecycle and system-state replies; actual adapter/client, no native execution.', requests: a.requests, evidence: a.platform.evidenceSnapshot() });
});

test('Plan13 lifecycle complete native proof is not cut off by the former five-second provider phase', async () => {
  const a = await lifecycleReplay('slow-native-observation');
  a.state.slowInstalledObservation = true;
  await a.owned(() => a.platform.launchInstalledApp());
  assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  assert.equal(a.settings().defaultActiveApplication, 'com.apple.webapp');
  assert.equal(a.driver.snapshot().unusable, false);
  assert.ok(a.driver.snapshot().commands.every((entry) => entry.timeoutMs <= 120_000 && !entry.timedOut));
});

for (const mode of ['wrong-foreground', 'missing-foreground', 'missing-pid', 'invalid-pid', 'wrong-native-info', 'alert', 'SBTransientOverlayWindow', 'NotificationShortLookView', 'app-dialog', 'wrong-origin', 'wrong-document', 'not-standalone', 'malformed-state', 'alert-protocol', 'system-empty', 'system-malformed', 'system-replaced'] as const) {
  test(`Plan13 lifecycle bound ${mode} cannot be hidden by a configured target or cached document`, async () => {
    const a = await lifecycleReplay(mode);
    await a.owned(() => a.platform.launchInstalledApp());
    a.state.safariRunning = false;
    if (mode === 'wrong-foreground') a.state.foreground = 'com.example.other';
    if (mode === 'missing-foreground') a.state.foreground = '';
    if (mode === 'missing-pid') a.state.pid = undefined;
    if (mode === 'invalid-pid') a.state.pid = -1;
    if (mode === 'wrong-native-info') a.state.activeFault = { bundleId: 'com.example.other', pid: 42 };
    if (['alert', 'SBTransientOverlayWindow', 'NotificationShortLookView', 'app-dialog'].includes(mode)) a.state.overlay = mode;
    if (mode === 'wrong-origin') a.state.url = 'https://other.test/';
    if (mode === 'wrong-document') a.state.documentOrigin = 'https://other.test';
    if (mode === 'not-standalone') a.state.standalone = false;
    if (mode === 'malformed-state') a.state.stateFault = '4';
    if (mode === 'alert-protocol') a.state.alertFault = true;
    if (mode.startsWith('system-')) a.state.systemObservationFault = mode.slice('system-'.length);
    const count = a.requests.length;
    let failure: unknown;
    await assert.rejects(() => a.platform.attachToInstalledView(), (error) => { failure = error; return /IOS_CONTEXT_OWNERSHIP|APPIUM_COMMAND/u.test(String(error)); });
    const stopped = a.requests.length;
    await assert.rejects(() => a.platform.attachToInstalledView(), (error) => error === failure);
    await assert.rejects(() => a.owned(() => a.platform.relaunchInstalledApp()), (error) => error === failure);
    assert.equal(a.requests.length, stopped);
    assert.equal(a.requests.slice(count).some((r) => r.method === 'POST' && r.path.endsWith('/appium/settings')), false);
    assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
  });
}

for (const fault of ['unsupported', 'readback', 'malformed-update', 'malformed-readback']) {
  test(`Plan13 lifecycle ${fault} settings fail before a planned native mutation`, async () => {
    const a = await lifecycleReplay(fault);
    a.state.settingsFault = fault;
    let first: unknown;
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => { first = error; return /IOS_NATIVE_SETTINGS|APPIUM_COMMAND/u.test(String(error)); });
    const count = a.requests.length;
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), (error) => error === first);
    assert.equal(a.requests.length, count);
    assert.equal(a.requests.some((r) => /pressButton|activateApp/u.test(r.body.script || '')), false);
  });
}

for (const fault of ['wrong-launch', 'springboard-overlay', 'home-failed']) {
  test(`Plan13 lifecycle first identification ${fault} is not repaired by activation`, async () => {
    const a = await lifecycleReplay(fault);
    if (fault === 'wrong-launch') a.state.wrongLaunch = 'com.example.other';
    if (fault === 'springboard-overlay') a.state.overlay = 'SBTransientOverlayWindow';
    if (fault === 'home-failed') a.state.ignoreHome = true;
    await assert.rejects(() => a.owned(() => a.platform.launchInstalledApp()), /IOS_CONTEXT_OWNERSHIP/u);
    assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, false);
    assert.equal(a.requests.some((r) => r.body.script === 'mobile: activateApp'), false);
  });
}

async function hierarchyReplay(mode: string) {
  const sources = await Promise.all(['ios-34488020101-before-share-hierarchy.xml', 'ios-34488020101-share-0-hierarchy.xml', 'ios-34488014724-share-1-hierarchy.xml'].map((file) => readFile(join(fixtureDir, file), 'utf8')));
  let source = sources[0];
  let now = 0;
  let sheet = false;
  let scrolls = 0;
  let swipes = 0;
  let reads = 0;
  let sheetReads = 0;
  let confirming = false;
  let lookups = 0;
  let identities = 0;
  const clicks: string[] = [];
  const pending: Promise<unknown>[] = [];
  const timed = mode === 'recorded-latency' || mode.startsWith('slow-');
  const waitFor = async (ms: number) => {
    const promise = wait(ms);
    pending.push(promise);
    await promise;
  };
  const a = await adapter(`hierarchy-${mode}`, async ({ path, body, signal }) => {
    if (path.endsWith('/context')) {
      if (mode === 'parent-initial') now = 112_001;
      return value(null);
    }
    if (body.script === 'mobile: activeAppInfo') return value({ bundleId: 'com.apple.mobilesafari', pid: 20640 });
    if (path.endsWith('/source')) {
      reads++;
      if (sheet) sheetReads++;
      source = !sheet ? sources[0] : scrolls ? sources[2] : sources[1];
      if (mode === 'recorded-latency' && sheet && sheetReads < 3) source = sources[0];
      if (mode === 'publication-tail' && sheetReads === 1) { source = sources[0]; now += 7_001; }
      if (mode === 'search-tail' && sheetReads === 1) now += 52_001;
      if (mode === 'fallback-tail' && scrolls === 1) now = 52_001;
      if (mode === 'fallback-reserve' && scrolls === 1) now = 47_001;
      if (mode === 'malformed-source' && scrolls === 1) return value(source.slice(0, -100));
      if (mode === 'hung-source' && scrolls === 1) return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
      if (mode === 'late-source' && scrolls === 1) await waitFor(8_200);
      if (mode.startsWith('interrupted-') && scrolls === 1) {
        const error = mode === 'interrupted-reset' ? new TypeError('hypothetical source connection reset')
          : new DOMException('interrupted source body', mode === 'interrupted-abort' ? 'AbortError' : 'TimeoutError');
        return new Response(new ReadableStream({ start(controller) { controller.error(error); } }));
      }
      if ((mode === 'slow-body' || mode === 'late-body') && scrolls === 1 && sheetReads === 3) {
        const payload = JSON.stringify({ value: source.replace('</AppiumAUT>', `${' '.repeat(80_000)}</AppiumAUT>`) });
        return new Response(new ReadableStream({
          start(controller) {
            controller.enqueue(new TextEncoder().encode(payload.slice(0, 50_000)));
            const completion = wait(mode === 'slow-body' ? 5_764 : 8_200).then(() => {
              controller.enqueue(new TextEncoder().encode(payload.slice(50_000)));
              controller.close();
            });
            pending.push(completion);
          },
        }));
      }
      if ((mode === 'slow-initial' && !sheet) || (mode === 'slow-publication' && sheetReads === 1)
        || (mode === 'slow-search' && sheetReads === 2) || (mode === 'slow-fallback' && sheetReads === 3)) {
        await waitFor(recordedIteration13.share.sourceBackendMs);
      }
      if (mode === 'recorded-latency') await waitFor(!sheet ? recordedIteration13.share.initialSourceMs : scrolls ? recordedIteration13.share.sourceBackendMs : recordedIteration13.share.sourceMs[sheetReads - 1] || 0);
      return value(source);
    }
    if (path.endsWith('/screenshot')) return value('');
    if (body.script === 'mobile: scroll') {
      assert.deepEqual(body.args, { element: `container-${scrolls}`, direction: 'down', distance: 0.75 });
      scrolls++;
      if (mode === 'recorded-latency') await waitFor(recordedIteration13.share.scrollClientMs);
      if (mode === 'fallback-source-admission') now = 52_001;
      if (mode.startsWith('fallback') || mode === 'slow-fallback') return Response.json({ value: { error: 'unknown error', message: 'completed scroll failure' } }, { status: 500 });
      if (mode === 'post-source-admission') now = 112_001;
      return value(null);
    }
    if (body.script === 'mobile: swipe') { swipes++; return value(null); }
    if (path.endsWith('/elements')) {
      const xml = confirming ? hypotheticalConfirmation : source;
      assert.ok(xpathCount(xml, body.value) > 0);
      if (confirming) {
        identities++;
        if (mode === 'recorded-latency') await waitFor(recordedIteration13.confirmation.identityMs[identities - 1]);
        return value([element('add')]);
      }
      return value([element(`${body.value.includes('Add to Home Screen') ? 'target' : 'container'}-${scrolls}`)]);
    }
    if (path.endsWith('/element')) {
      if (body.value === 'ShareButton') return value(element('share'));
      assert.equal(body.value, 'Add');
      lookups++;
      if (mode === 'recorded-latency') {
        await waitFor(recordedIteration13.confirmation.lookupMs[lookups - 1]);
        if (lookups === 1) return missing();
      }
      return value(element('add'));
    }
    if (path.endsWith('/rect')) {
      assert.ok(path.includes(`-${scrolls}/`));
      const list = nativeActionListEvidence(source, 'Add to Home Screen')!;
      return value(path.includes('/container-') ? list.collection.bounds : list.targetRows[0].bounds);
    }
    if (path.includes('/attribute/')) {
      if (mode === 'recorded-latency' && path.includes('/add/')) {
        const attribute = path.split('/attribute/')[1] as keyof typeof recordedIteration13.confirmation.attributeMs;
        await waitFor(recordedIteration13.confirmation.attributeMs[attribute]);
      }
      if (path.includes('/container-') && path.endsWith('/visible')) {
        if (mode === 'parent-gesture') now = 107_001;
        if (mode === 'child-gesture') now = 47_001;
        if (mode === 'near-gesture') now = 46_500;
      }
      return value(path.includes('/target-') && /\/(visible|hittable)$/u.test(path) ? String(scrolls > 0) : 'true');
    }
    if (path.endsWith('/click')) {
      const id = path.split('/element/')[1].split('/')[0];
      clicks.push(id);
      if (id === 'share') sheet = true;
      if (id.startsWith('target')) confirming = true;
      if (id === 'add' && mode === 'recorded-latency') await waitFor(recordedIteration13.confirmation.clickMs);
      return value(null);
    }
    throw new Error(`unexpected hierarchy request ${path} ${JSON.stringify(body)}`);
  }, timed || /^(?:hung|late|interrupted)-/u.test(mode) ? undefined : () => now);
  let error: unknown;
  try { await a.platform.installFromBrowser(); } catch (caught) { error = caught; }
  const stopped = a.requests.length;
  const first = a.driver.snapshot().firstFatal;
  if (/^(?:hung|late|interrupted)-/u.test(mode)) {
    assert.match(String(error), /APPIUM_(?:TIMEOUT|INTERRUPTED)/u);
    await assert.rejects(() => a.driver.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
    await Promise.allSettled(pending);
    await assert.rejects(() => a.platform.installFromBrowser(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(a.requests.length, stopped);
    assert.deepEqual(a.driver.snapshot().firstFatal, first);
  } else await Promise.allSettled(pending);
  await writeSanitizedJson(join(a.outputDir, 'ios-hierarchy-result.json'), {
    proofKind: 'Actual-client protocol replay. PR pre-scroll XML and latencies recorded; completing post-scroll XML from push, confirmation XML hypothetical. Budget, body and hung controls hypothetical. No native acceptance.',
    error: String(error || ''), clicks, reads, sheetReads, scrolls, swipes, lookups, evidence: a.platform.evidenceSnapshot(),
  });
  return { ...a, error, clicks, reads, sheetReads, scrolls, swipes, lookups };
}

for (const mode of ['recorded-latency', 'slow-body', 'slow-initial', 'slow-publication', 'slow-search', 'slow-fallback', 'near-gesture', 'fallback-success']) {
  test(`Plan13 hierarchy full install ${mode} completes with full source allowances and one final Add`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const a = await hierarchyReplay(mode);
    assert.equal(a.error, undefined);
    assert.deepEqual(a.clicks, ['share', 'target-1', 'add']);
    assert.equal(a.scrolls, 1);
    assert.equal(a.swipes, mode.includes('fallback') ? 1 : 0);
    assert.equal(a.driver.snapshot().unusable, false);
    assert.ok(a.driver.snapshot().commands.filter((r) => r.path.endsWith('/source')).every((r) => r.timeoutMs === 8_000));
  });
}

for (const mode of ['parent-initial', 'publication-tail', 'search-tail', 'parent-gesture', 'child-gesture', 'fallback-tail', 'fallback-reserve', 'fallback-source-admission', 'post-source-admission', 'malformed-source']) {
  test(`Plan13 hierarchy full install ${mode} never dispatches a short source or an unverifiable gesture`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const a = await hierarchyReplay(mode);
    assert.ok(a.error);
    assert.ok(a.clicks.length <= 1);
    assert.equal(a.scrolls, /fallback|post-source|malformed/u.test(mode) ? 1 : 0);
    assert.equal(a.swipes, 0);
    if (mode === 'parent-initial') assert.equal(a.reads, 0);
    if (mode === 'publication-tail' || mode === 'search-tail') assert.equal(a.sheetReads, 1);
    if (mode === 'fallback-source-admission' || mode === 'post-source-admission') assert.equal(a.sheetReads, 2);
    assert.ok(a.driver.snapshot().commands.filter((r) => r.path.endsWith('/source')).every((r) => r.timeoutMs === 8_000 && !r.timedOut));
    assert.equal(a.driver.snapshot().unusable, false);
  });
}

for (const mode of ['hung-source', 'late-source', 'late-body', 'interrupted-body', 'interrupted-reset', 'interrupted-abort']) {
  test(`Plan13 hierarchy full install ${mode} preserves quarantine first failure and no late work`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const a = await hierarchyReplay(mode);
    assert.deepEqual(a.clicks, ['share']);
    assert.equal(a.scrolls, 1);
    assert.equal(a.swipes, 0);
    assert.equal(a.lookups, 0);
    assert.equal(a.driver.snapshot().firstFatal?.code, /interrupted-(?:reset|abort)/u.test(mode) ? 'APPIUM_INTERRUPTED' : 'APPIUM_TIMEOUT');
  });
}

class ReceiptClock implements IOSReceiptClock {
  elapsed = 0;
  readonly epoch = Date.UTC(2026, 8, 20, 23, 59, 59, 250);
  private next = 0;
  readonly timers = new Map<number, { at: number; callback: () => void }>();
  wall = () => this.epoch + this.elapsed;
  mono = () => this.elapsed;
  timer = (callback: () => void, ms: number) => {
    const id = ++this.next;
    this.timers.set(id, { at: this.elapsed + ms, callback });
    return id as unknown as ReturnType<typeof setTimeout>;
  };
  clear = (id: ReturnType<typeof setTimeout>) => { this.timers.delete(id as unknown as number); };
  advance(ms: number): void {
    const end = this.elapsed + ms;
    for (;;) {
      const next = [...this.timers].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      this.elapsed = Math.max(this.elapsed, next[1].at);
      this.timers.delete(next[0]);
      next[1].callback();
    }
    this.elapsed = end;
  }
}

class ReceiptPipe extends EventEmitter {
  destroyed = false;
  destroy(): this { this.destroyed = true; return this; }
}
class ReceiptChild extends EventEmitter {
  pid: number | undefined = 42;
  exitCode: number | null = null;
  signalCode: NodeJS.Signals | null = null;
  readonly stdout = new ReceiptPipe();
  readonly stderr = new ReceiptPipe();
  readonly signals: string[] = [];
  acceptsSignal = true;
  kill(signal: string): boolean { this.signals.push(signal); return this.acceptsSignal; }
  exit(code: number | null, signal: string | null = null): void {
    this.exitCode = code; this.signalCode = signal as NodeJS.Signals | null; this.emit('exit', code, signal);
  }
  close(code: number | null = 0, signal: string | null = null): void {
    this.stdout.emit('end'); this.stderr.emit('end');
    this.stdout.emit('close'); this.stderr.emit('close');
    this.exit(code, signal); this.emit('close', code, signal);
  }
}
const receiptUdid = '12345678-1234-1234-1234-123456789ABC';
const receiptClip = 'ADD6F23D30D34C62A8E065D19753E4AF';
const receiptSession = '12345678-ABCD-1234-ABCD-123456789ABC';
const receiptLine = (message: string, subsystem = 'com.apple.mobilesafari', category = 'WebApp', producer = 'SafariViewService') =>
  `2026-09-21 00:00:01.000 Df ${producer}[42:abcd] [${subsystem}:${category}] ${message}\n`;
const receiptLoad = (clip = receiptClip) => receiptLine(`Loading UIWebClip with identifier '${clip}'; version: 1`);
function receiptFailure(clock: ReceiptClock): IOSLaunchFailure {
  return { eligible: true, reason: 'single-acknowledged-tap', stage: 'provider', invocation: 1, taps: 1,
    bundle: 'com.apple.webapp', readiness: 'ready', page: 1, remainingMs: 120_000,
    tap: { wallMs: clock.wall(), monoMs: clock.mono() }, settled: { wallMs: clock.wall(), monoMs: clock.mono() },
    before: null, click: null, fatal: { wallMs: clock.wall() + 30_000, monoMs: clock.mono() + 30_000 } };
}
function receiptRun(clock = new ReceiptClock(), failure = receiptFailure(clock), signal?: AbortSignal) {
  const child = new ReceiptChild();
  const commands: Array<{ binary: string; argv: string[] }> = [];
  const result = collectIOSLaunchReceipt(receiptUdid, failure, origin, { clock, signal, spawn: (binary, argv) => {
    commands.push({ binary, argv }); return child as unknown as ChildProcess;
  } });
  child.emit('spawn');
  return { clock, child, commands, result };
}
async function receiptSettled<T>(result: Promise<T>, clock: ReceiptClock): Promise<T> {
  let settled = false;
  void result.then(() => { settled = true; }, () => { settled = true; });
  for (let turn = 0; turn < 100 && !settled; turn++) await Promise.resolve();
  if (!settled) clock.advance(20_000);
  for (let turn = 0; turn < 100 && !settled; turn++) await Promise.resolve();
  assert.equal(settled, true, 'collector must settle within the bounded fake clock control');
  assert.equal(clock.timers.size, 0);
  return result;
}
function receiptSnapshot(attemptId = 1, path = '/session/mock/element/icon/click'): WebDriverSnapshot {
  return { sessionId: '[active]', selectedContext: 'NATIVE_APP', selectedWindow: '', unusable: false, commands: [], lookups: [],
    lastCommand: { command: 'POST', method: 'POST', path, durationMs: 1, timeoutMs: 1_000, timedOut: false, selectedContext: 'NATIVE_APP', selectedWindow: '',
      timing: { attemptId, dispatchedOrdinal: attemptId, sessionGeneration: 1, operation: 'other', scope: 'unscoped', entryMs: attemptId,
        dispatchMs: attemptId, settlementMs: attemptId + 1, phaseRemainingAtEntryMs: 1_000, phaseRemainingAtDispatchMs: 1_000,
        phaseRemainingAtSettlementMs: 999, admitted: true, sent: true, completed: true, outcome: 'success' } } };
}

test('SM55 receipt query literal UTC window and refusals', async () => {
  const clock = new ReceiptClock();
  const failure = receiptFailure(clock);
  const expected = ['simctl', 'spawn', receiptUdid, 'log', 'show', '--style', 'compact', '--start', '2026-09-20 23:59:54+0000', '--end', '2026-09-21 00:00:30+0000', '--predicate', IOS_LAUNCH_PREDICATE];
  assert.equal(IOS_LAUNCH_PREDICATE, '((process == "SpringBoard" OR process == "Web" OR process == "runningboardd" OR process == "frontboardd" OR process == "launchd" OR process == "lsd") AND (eventMessage CONTAINS[c] "com.apple.webapp" OR eventMessage CONTAINS[c] "WebClip" OR eventMessage CONTAINS[c] "com.apple.WebKit.PushBundle." OR eventMessage CONTAINS[c] "com.apple.SafariViewService")) OR (process == "Web" AND (subsystem == "com.apple.UIKit" OR subsystem == "com.apple.runningboard" OR subsystem == "com.apple.FrontBoard")) OR (process == "SafariViewService" AND ((subsystem == "com.apple.UIKit" AND (category == "ViewServiceSessionManager" OR category == "ViewServices" OR category == "AppLifecycle")) OR (subsystem == "com.apple.mobilesafari" AND (category == "WebApp" OR category == "WebPush")) OR (subsystem == "com.apple.runningboard" AND category == "monitor")))');
  assert.deepEqual(iosLaunchQuery(receiptUdid, failure)?.argv, expected);
  const valid = receiptRun(clock, failure);
  valid.child.close();
  assert.deepEqual(valid.commands, [{ binary: 'xcrun', argv: expected }]);
  assert.deepEqual((await valid.result).receipt.query?.argv, expected);
  assert.equal(expected.filter(arg => arg === IOS_LAUNCH_PREDICATE).length, 1);
  assert.equal(IOS_LAUNCH_PREDICATE.includes('TRUEPREDICATE'), false);
  for (const fatal of [
    { wallMs: NaN, monoMs: 1 }, { wallMs: failure.tap!.wallMs - 1, monoMs: 1 },
    { wallMs: failure.tap!.wallMs + 1_001, monoMs: 0 },
    { wallMs: failure.tap!.wallMs + 121_001, monoMs: 121_001 },
  ]) assert.equal(iosLaunchQuery(receiptUdid, { ...failure, fatal }), null);
  for (const udid of ['', 'protocol-only', `${receiptUdid}; log show`, `${receiptUdid}\n`]) assert.equal(iosLaunchQuery(udid, failure), null);
  const exact = { ...failure, tap: { wallMs: Date.UTC(2026, 0, 1), monoMs: 0 }, fatal: { wallMs: Date.UTC(2026, 0, 1) + 120_000, monoMs: 120_000 } };
  assert.equal(iosLaunchQuery(receiptUdid, exact)?.endMs, exact.fatal.wallMs);
  const a = receiptRun(clock, { ...failure, eligible: false });
  assert.equal((await a.result).receipt.outcome, 'not-eligible');
  assert.equal(a.commands.length, 0);
  assert.equal(clock.timers.size, 0);
});

test('SM55 receipt eligibility command boundaries and ring eviction', async () => {
  for (const mode of ['eligible', 'no-tap', 'unsent', 'unacknowledged', 'multiple', 'path', 'method', 'generation', 'earlier', 'later', 'unresolved', 'ring-eviction']) {
    const clock = new ReceiptClock();
    const observation = new IOSLaunchObservation(clock);
    observation.begin();
    if (mode === 'earlier') observation.begin();
    if (mode !== 'no-tap') observation.arm(receiptSnapshot(), 1, 120_000);
    const after = receiptSnapshot(2);
    if (mode === 'unsent') after.lastCommand!.timing = { ...after.lastCommand!.timing!, sent: false, admitted: false, dispatchedOrdinal: undefined };
    if (mode === 'unacknowledged') after.lastCommand!.timing = { ...after.lastCommand!.timing!, completed: false, outcome: 'timeout' };
    if (mode === 'path') after.lastCommand!.path = '/session/mock/element/other/click';
    if (mode === 'method') after.lastCommand!.method = 'GET';
    if (mode === 'generation') after.lastCommand!.timing = { ...after.lastCommand!.timing!, sessionGeneration: 2 };
    observation.acknowledge(after, 'icon');
    if (mode === 'multiple') observation.arm(after, 2, 60_000);
    observation.enter('provider');
    if (mode === 'ring-eviction') {
      for (let index = 0; index < 1_100; index++) after.commands.push(receiptSnapshot(index + 3).lastCommand!);
      after.commands = after.commands.slice(-50);
      after.lastCommand = after.commands.at(-1);
    }
    if (mode === 'unresolved') after.lastCommand!.timing = { ...after.lastCommand!.timing!, completed: false };
    if (mode === 'later') observation.enter('attachment');
    const fatal = new Error('synthetic-first-fatal');
    observation.freeze(fatal, after);
    const frozen = observation.failure;
    observation.freeze(new Error('synthetic-later-fatal'), after);
    assert.equal(observation.failure, frozen);
    assert.equal(frozen?.eligible === true, mode === 'eligible' || mode === 'ring-eviction');
    if (mode === 'earlier' || mode === 'later') assert.equal(frozen, undefined);
    else assert.equal((observation as any).originalFatal, fatal);
    if (frozen) assert.ok(Buffer.byteLength(JSON.stringify(frozen)) < IOS_LAUNCH_LIMITS.armBytes);
  }
});

test('SM55 receipt process exit signal errno and callback permutations', async () => {
  for (const mode of ['zero', 'nonzero-empty', 'signal', 'spawn-error', 'stream-error', 'error-exit-close', 'exit-before-kill', 'rejected-signal', 'missing-pid', 'lingering-pipe']) {
    const a = receiptRun();
    if (mode === 'zero') a.child.close();
    if (mode === 'nonzero-empty') a.child.close(64);
    if (mode === 'signal') a.child.close(null, 'SIGKILL');
    if (mode === 'spawn-error' || mode === 'error-exit-close') {
      a.child.emit('error', Object.assign(new Error('synthetic-private-message'), { code: 'ENOENT', path: 'synthetic-private-path', cause: new Error('secret') }));
      if (mode === 'error-exit-close') a.child.close(64);
      else a.clock.advance(2_000);
    }
    if (mode === 'stream-error') { a.child.stdout.emit('error', { code: 'EIO', message: 'secret' }); a.child.close(); }
    if (mode === 'exit-before-kill') { a.clock.advance(17_000); a.child.exit(0); a.clock.advance(2_000); }
    if (mode === 'rejected-signal') { a.child.acceptsSignal = false; a.clock.advance(19_000); }
    if (mode === 'missing-pid') { a.child.pid = undefined; a.child.emit('spawn'); a.clock.advance(2_000); }
    if (mode === 'lingering-pipe') { a.child.exit(0); a.clock.advance(2_000); }
    const result = await receiptSettled(a.result, a.clock);
    assert.equal(a.commands.length, 1);
    assert.equal(a.clock.timers.size, 0);
    assert.equal(a.child.listenerCount('exit'), 0);
    assert.equal(a.child.listenerCount('close'), 0);
    assert.equal(a.child.stdout.listenerCount('data'), 0);
    assert.equal(a.child.listenerCount('error'), 1, 'only a stateless late error consumer remains');
    assert.equal(result.receipt.process.simulatorWorker, 'unknown');
    assert.equal(result.receipt.process.hostDescendants, 'unknown');
    assert.equal(result.receipt.stderr.received, 0);
    assert.equal(result.receiptText.includes('secret') || result.receiptText.includes('synthetic-private'), false);
    if (mode === 'nonzero-empty') assert.equal(result.receipt.process.exitCode, 64);
    if (mode === 'signal') { assert.equal(result.receipt.process.exitCode, null); assert.equal(result.receipt.flags.deadline, false); assert.equal(result.receipt.process.signal, 'SIGKILL'); assert.equal(result.receipt.process.signalState, 'recognized'); }
    if (mode === 'spawn-error') { assert.equal(result.receipt.process.exitCode, null); assert.equal(result.receipt.process.error, 'ENOENT'); }
    if (mode === 'exit-before-kill' || mode === 'rejected-signal') assert.deepEqual(a.child.signals, ['SIGTERM']);
    if (mode === 'lingering-pipe' || mode === 'missing-pid' || mode === 'spawn-error') assert.deepEqual(a.child.signals, []);
    if (['lingering-pipe', 'exit-before-kill', 'rejected-signal'].includes(mode)) assert.equal(result.blockNativeDiagnostics, true);
    const frozen = result.receiptText;
    a.child.emit('error', new Error('late-secret'));
    a.child.close();
    a.child.stdout.emit('data', Buffer.from(receiptLoad()));
    a.clock.advance(30_000);
    assert.equal(result.receiptText, frozen);
  }
  const clock = new ReceiptClock();
  const failed = await collectIOSLaunchReceipt(receiptUdid, receiptFailure(clock), origin, { clock, spawn: () => { throw { code: 'EACCES', message: 'secret' }; } });
  assert.equal(failed.receipt.process.spawned, false);
  assert.equal(failed.receipt.process.exitCode, null);
  assert.equal(failed.receipt.process.error, 'EACCES');
  assert.equal(iosReceiptError(new Proxy({}, { getOwnPropertyDescriptor() { throw new Error('secret'); } })), 'unknown-withheld');
});

test('SM55 receipt spawn failure close-only and conflicting status stay distinct', async () => {
  for (const mode of ['before-spawn', 'close-only', 'conflicting-exit', 'bad-chunk', 'pipe-error', 'delayed-kill', 'SIGEMT', 'unknown-signal', 'close-unknown', 'duplicate-unknown']) {
    const clock = new ReceiptClock();
    const child = new ReceiptChild();
    const resultPromise = collectIOSLaunchReceipt(receiptUdid, receiptFailure(clock), origin, { clock, spawn: () => child as unknown as ChildProcess });
    if (mode === 'before-spawn') child.emit('error', { code: 'ENOENT', message: 'synthetic-secret' });
    else child.emit('spawn');
    if (mode === 'SIGEMT' || mode === 'unknown-signal') child.close(null, mode === 'SIGEMT' ? 'SIGEMT' : 'synthetic-private-signal');
    else if (mode === 'close-unknown') { child.exit(0); child.emit('close', 0, 'synthetic-private-signal'); }
    else if (mode === 'duplicate-unknown') { child.exit(0); child.exit(0, 'synthetic-private-signal'); child.emit('close', 0, null); }
    else if (mode === 'close-only') child.emit('close', 64, null);
    else if (mode === 'conflicting-exit') { child.exit(64); child.exit(0); child.emit('close', 64, null); }
    else if (mode === 'bad-chunk') { child.stdout.emit('data', { message: 'synthetic-secret' }); child.close(); }
    else if (mode === 'pipe-error') { child.stderr.emit('error', new Error('synthetic-secret')); child.close(); }
    else if (mode === 'delayed-kill') {
      clock.advance(17_000);
      clock.elapsed = 19_500;
      clock.advance(0);
    } else clock.advance(2_000);
    const result = await receiptSettled(resultPromise, clock);
    assert.equal(result.receipt.outcome, 'incomplete');
    if (['SIGEMT', 'unknown-signal', 'close-unknown', 'duplicate-unknown'].includes(mode)) {
      assert.equal(result.receipt.process.unknownSignalObserved, true);
      assert.equal(result.receipt.process.signal, null);
      assert.equal(result.receipt.flags.deadline, false);
      assert.equal(result.receipt.process.exitCode, mode === 'close-unknown' || mode === 'duplicate-unknown' ? 0 : null);
      assert.equal(result.receipt.process.statusConflict, true);
      assert.equal(result.receipt.process.signalState, mode === 'close-unknown' || mode === 'duplicate-unknown' ? 'absent' : 'unknown-withheld');
      assert.equal(result.receiptText.includes('SIGEMT') || result.receiptText.includes('synthetic-private-signal'), false);
    }
    assert.equal(result.receiptText.includes('synthetic-secret'), false);
    if (mode === 'before-spawn') { assert.equal(result.receipt.process.spawnError, 'ENOENT'); assert.equal(result.receipt.process.spawned, false); }
    if (mode === 'close-only') { assert.equal(result.receipt.process.exitCode, null); assert.equal(result.receipt.process.closeCode, 64); }
    if (mode === 'conflicting-exit') { assert.equal(result.receipt.process.exitCode, 64); assert.equal(result.receipt.process.statusConflict, true); }
    if (mode === 'delayed-kill') assert.deepEqual(child.signals, ['SIGTERM']);
    assert.equal(clock.timers.size, 0);
    child.emit('error', new Error('late-secret'));
    child.stderr.emit('error', new Error('late-secret'));
  }
});

test('SM55 receipt absolute deadline cancellation caps and scheduling overrun', async () => {
  for (const at of [16_999, 17_000, 17_500, 20_001]) {
    const a = receiptRun();
    a.child.stdout.emit('data', Buffer.from(receiptLoad()));
    a.clock.elapsed = at;
    a.child.exit(0);
    a.child.emit('close', 0, null);
    const result = await receiptSettled(a.result, a.clock);
    assert.equal(result.receipt.flags.deadline, at >= 17_000);
    assert.equal(result.receipt.flags.overrun, at > 20_000);
    assert.equal(result.receipt.process.exitCode, 0);
    assert.equal(result.receipt.process.signal, null);
    if (at < 19_000) assert.equal(result.receipt.process.closeCode, 0);
    assert.equal(result.receipt.outcome, at < 17_000 ? 'public-projection-unreviewed' : 'incomplete');
    assert.deepEqual(a.child.signals, []);
  }
  for (const at of [999, 1_000, 1_500, 2_000, 2_001]) {
    const a = receiptRun();
    a.child.stdout.emit('data', Buffer.from(receiptLoad()));
    a.child.exit(0);
    a.clock.elapsed = at;
    a.child.emit('close', 0, null);
    const result = await receiptSettled(a.result, a.clock);
    assert.equal(result.receipt.flags.deadline, at >= 1_000);
    assert.equal(result.receipt.process.exitCode, 0);
    assert.equal(result.receipt.process.closeCode, 0);
    assert.equal(result.receipt.process.localStop, 'confirmed');
    assert.deepEqual(a.child.signals, []);
  }
  for (const setup of ['abort-inside-spawn', 'abort-without-spawn-event', 'abort-overrun', 'abort-then-throw', 'setup-error', 'setup-error-after-spawn']) {
    const clock = new ReceiptClock();
    const child = new ReceiptChild();
    const controller = new AbortController();
    if (setup.startsWith('setup-error')) {
      const onOutput = child.stdout.on.bind(child.stdout);
      child.stdout.on = function (event, listener) {
        if (event === 'data') throw { code: 'EIO', message: 'synthetic-setup-secret' };
        return onOutput(event, listener);
      };
      if (setup === 'setup-error-after-spawn') {
        const on = child.on.bind(child);
        child.on = function (event, listener) {
          on(event, listener);
          if (event === 'spawn') child.emit('spawn');
          return child;
        };
      }
    }
    const promise = collectIOSLaunchReceipt(receiptUdid, receiptFailure(clock), origin, { clock, signal: controller.signal, spawn: () => {
      if (setup === 'abort-overrun') clock.elapsed = 20_001;
      if (setup.startsWith('abort')) controller.abort(new Error('synthetic-setup-secret'));
      if (setup === 'abort-then-throw') throw { code: 'ENOENT', message: 'synthetic-setup-secret' };
      return child as unknown as ChildProcess;
    } });
    if (setup === 'abort-inside-spawn') child.emit('spawn');
    clock.advance(2_000);
    const result = await receiptSettled(promise, clock);
    assert.equal(result.receipt.outcome, 'incomplete');
    assert.equal(result.blockNativeDiagnostics, true);
    assert.equal(result.receipt.process.exitCode, null);
    assert.equal(result.receipt.process.localStop, 'unconfirmed');
    assert.deepEqual(child.signals, setup === 'abort-inside-spawn' || setup === 'abort-without-spawn-event' ? ['SIGTERM', 'SIGKILL'] : []);
    if (setup !== 'abort-then-throw') {
      assert.equal(child.stdout.destroyed, true);
      assert.equal(child.stderr.destroyed, true);
    }
    assert.equal(child.listenerCount('exit'), 0);
    assert.equal(child.listenerCount('close'), 0);
    assert.equal(child.stdout.listenerCount('data'), 0);
    assert.equal(result.receiptText.includes('synthetic-setup-secret'), false);
    assert.equal(result.receipt.process.spawnError, setup === 'abort-then-throw' ? 'ENOENT' : setup === 'setup-error' ? 'EIO' : null);
    assert.equal(clock.timers.size, 0);
  }
  for (const mode of ['deadline', 'before-cancel', 'after-cancel', 'cap-deadline', 'overrun', 'overrun-after-term']) {
    const clock = new ReceiptClock();
    const controller = new AbortController();
    if (mode === 'before-cancel') controller.abort(new Error('synthetic-private-cancel'));
    const a = receiptRun(clock, receiptFailure(clock), controller.signal);
    if (mode === 'after-cancel') controller.abort(new Error('synthetic-private-cancel'));
    if (mode === 'cap-deadline') {
      clock.elapsed = 17_000;
      a.child.stdout.emit('data', Buffer.alloc(IOS_LAUNCH_LIMITS.stdoutBytes + 7, 120));
    }
    if (mode === 'overrun' || mode === 'overrun-after-term') {
      if (mode === 'overrun-after-term') clock.advance(17_000);
      clock.elapsed = 20_001;
      a.child.stdout.emit('data', Buffer.from(receiptLoad()));
    }
    clock.advance(20_000);
    const result = await receiptSettled(a.result, clock);
    assert.equal(result.receipt.outcome, 'incomplete');
    assert.equal(result.receiptText.includes('synthetic-private'), false);
    assert.equal(clock.timers.size, 0);
    if (mode === 'before-cancel') assert.equal(a.commands.length, 0);
    if (mode === 'cap-deadline') { assert.equal(result.receipt.flags.receiveCap, true); assert.equal(result.receipt.flags.deadline, true); assert.equal(result.receipt.stdout.overshoot, 7); }
    if (mode === 'overrun' || mode === 'overrun-after-term') {
      assert.equal(result.receipt.flags.overrun, true);
      assert.ok(result.receipt.collection.overrunMs > 0);
      assert.deepEqual(a.child.signals, mode === 'overrun' ? [] : ['SIGTERM']);
    }
  }
});

test('SM55 receipt byte UTF8 grammar and hostile secret withholding', async () => {
  const a = receiptRun();
  const secret = 'SYNTHETIC_SECRET';
  const text = Buffer.from(`unknown € https://user:${secret}@${secret}.invalid/path?key=${secret}#${secret}\n`);
  for (const byte of text) a.child.stdout.emit('data', Buffer.from([byte]));
  a.child.stdout.emit('data', Buffer.from([0xc3, 0x28, 10]));
  a.child.stdout.emit('data', Buffer.from('x'.repeat(8_193) + '\n'));
  a.child.stdout.emit('data', Buffer.from(receiptLoad()));
  a.child.stdout.emit('data', Buffer.from(receiptLine(`Loading UIWebClip with identifier '${receiptClip}'; version: 1 ${secret}`)));
  a.child.stdout.emit('data', Buffer.from(receiptLine(`Loading UIWebClip with identifier '<private>'; version: 1`)));
  a.child.stdout.emit('data', Buffer.from(receiptLine(`Loading UIWebClip with identifier '${receiptClip}'; version: 1`, 'com.apple.UIKit', 'unknown')));
  a.child.stderr.emit('data', Buffer.from(`log: Permission denied\nlog: Invalid predicate\nlog: Invalid start date\nsimctl: No such file or directory\n${secret}\n`));
  a.child.stdout.emit('data', Buffer.from('incomplete-' + secret));
  a.child.close();
  const result = await a.result;
  assert.equal(result.receipt.outcome, 'incomplete');
  assert.equal(result.receipt.stdout.invalidUTF8, 1);
  assert.equal(result.receipt.stdout.overlong, 1);
  assert.equal(result.receipt.stdout.incompleteTail, 1);
  assert.equal(result.receipt.eventCount, 1);
  assert.deepEqual(result.receipt.stderrClasses, ['permission-denied', 'unsupported-predicate', 'unsupported-date', 'missing-executable']);
  assert.equal((result.receiptText + result.eventsText).includes(secret), false);
  assert.equal(result.eventsText.includes(receiptClip), true);
  assert.equal(result.receipt.stdout.received, result.receipt.stdout.retained + result.receipt.stdout.discarded);
  const b = receiptRun();
  const invalid = Buffer.from(receiptLoad() + receiptInvalidOrigin() + 'SYNTHETIC_FAULT_SECRET\n');
  b.child.stdout.emit('data', invalid);
  b.child.close();
  const withheld = await receiptSettled(b.result, b.clock);
  assert.equal(withheld.receipt.stdout.received, invalid.byteLength);
  assert.equal(withheld.receipt.stdout.retained, invalid.byteLength);
  assert.equal(withheld.receipt.stdout.discarded, 0);
  assert.equal(withheld.receipt.stdout.withheld, 2);
  assert.equal(withheld.receipt.eventCount, 1);
  assert.equal(withheld.receipt.flags.handlerFault, false);
  assert.equal((withheld.receiptText + withheld.eventsText).includes('SYNTHETIC_FAULT_SECRET'), false);
  const c = receiptRun();
  let expected!: ReturnType<typeof receiptFaultChunk>;
  queueMicrotask(() => { expected = receiptFaultChunk(c.child); c.clock.advance(2_000); });
  const faulted = await receiptSettled(c.result, c.clock);
  for (const key of ['received', 'retained', 'discarded'] as const) assert.equal(faulted.receipt.stdout[key], expected[key]);
  assert.equal(faulted.receipt.stdout.withheld, 2);
  assert.equal(faulted.receipt.eventCount, 1);
  assert.equal(faulted.receipt.flags.handlerFault, true);
  assert.equal(faulted.receipt.outcome, 'incomplete');
  assert.deepEqual(c.child.signals, []);
  assert.equal((faulted.receiptText + faulted.eventsText).includes('SYNTHETIC_FAULT_SECRET'), false);
});

test('SM55 receipt receive line record and public byte cap boundaries', async () => {
  for (const kind of ['stdout', 'stderr'] as const) {
    const cap = kind === 'stdout' ? IOS_LAUNCH_LIMITS.stdoutBytes : IOS_LAUNCH_LIMITS.stderrBytes;
    for (const delta of [-1, 0, 1]) {
      const a = receiptRun();
      a.child[kind].emit('data', Buffer.alloc(cap + delta, 120));
      a.child.close();
      const result = await a.result;
      assert.equal(result.receipt[kind].received, cap + delta);
      assert.equal(result.receipt.flags.receiveCap, delta >= 0);
      assert.equal(result.receipt[kind].overshoot, Math.max(0, delta));
      if (delta >= 0) { assert.equal(result.receipt[kind].retained, 0); assert.equal(result.receipt[kind].discarded, cap + delta); }
    }
  }
  for (const length of [8_191, 8_192, 8_193]) {
    const a = receiptRun();
    a.child.stdout.emit('data', Buffer.from('x'.repeat(length) + '\n'));
    a.child.close();
    assert.equal((await a.result).receipt.stdout.overlong, length > 8_192 ? 1 : 0);
  }
  const a = receiptRun();
  for (let index = 0; index < 4_097; index++) a.child.stdout.emit('data', Buffer.from(receiptLoad()));
  a.child.close();
  const result = await a.result;
  assert.equal(result.receipt.flags.outputCap || result.receipt.flags.recordCap, true);
  assert.ok(result.receipt.eventCount <= 4_096);
  assert.ok(Buffer.byteLength(result.eventsText) <= 524_288);
  assert.ok(Buffer.byteLength(result.receiptText) <= 32_768);
  assert.ok(Buffer.byteLength(result.receipt.stderrClasses.join('\n')) <= 8_192);
});

test('SM55 receipt conflicting native identities retain only explicit relations', async () => {
  const a = receiptRun();
  for (const clip of [receiptClip, 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB']) a.child.stdout.emit('data', Buffer.from(receiptLoad(clip)));
  for (const host of [101, 202]) a.child.stdout.emit('data', Buffer.from(receiptLine(`Configuring connection on service com.apple.uikit.viewservice.com.apple.SafariViewService to host pid ${host} for session ${receiptSession}`, 'com.apple.UIKit', 'ViewServiceSessionManager')));
  a.child.stdout.emit('data', Buffer.from(receiptLine(`sceneOfRecord: sceneID: sceneID:com.apple.SafariViewService-default  persistentID: ${receiptSession}`, 'com.apple.UIKit', 'AppLifecycle')));
  for (const host of ['localhost', 'synthetic-secret.invalid']) a.child.stdout.emit('data', Buffer.from(receiptLine(`Web Clip with identifier '${receiptClip}', script from origin <WKSecurityOrigin: 0x1234; protocol = https; host = ${host}; port = 52101> updated app badge count to 0`, 'com.apple.mobilesafari', 'WebPush')));
  a.child.stdout.emit('data', Buffer.from(receiptLine('Received state update for 101 (app<com.apple.webapp((null))>, running-Foreground', 'com.apple.runningboard', 'monitor', 'Web')));
  a.child.stdout.emit('data', Buffer.from(receiptLine(`request=${receiptSession} token=${receiptClip} https://synthetic-secret.invalid/`, 'com.apple.FrontBoard', 'unknown', 'SpringBoard')));
  a.child.stdout.emit('data', Buffer.from(receiptLoad().replace('2026-09-21', '2020-01-01')));
  a.child.close();
  const result = await a.result;
  const records = result.eventsText.trim().split('\n').map(line => JSON.parse(line));
  assert.equal(result.receipt.outcome, 'public-projection-unreviewed');
  assert.deepEqual(records.filter(record => record.hostPid).map(record => record.hostPid), [101, 202]);
  assert.deepEqual(records.filter(record => record.origin).map(record => record.origin), ['fixture-origin', 'other-origin']);
  assert.ok(records.every(record => record.clockMapping === 'unknown' && record.temporalRelationToFatal === 'unavailable'));
  assert.equal(result.receipt.coverage.tapToRequest, 'unproved');
  assert.equal(result.receipt.coverage.clipToHost, 'unproved');
  assert.equal(result.eventsText.includes('synthetic-secret'), false);
  assert.equal(records.length, 9);
  assert.equal(records.at(-1).temporalRelationToFatal, 'unavailable', 'no host/native clock mapping is invented for an outlying native timestamp');
});

async function receiptCaptureAdapter(name: string, clock = new ReceiptClock()) {
  const a = await adapter(`receipt-${name}`, ({ path }) => {
    if (path.endsWith('/screenshot')) return value('');
    throw new Error('unexpected receipt gameplay command');
  }, clock.mono);
  const internal = a.platform as any;
  internal.udid = receiptUdid;
  internal.launchObservation = new IOSLaunchObservation(clock);
  internal.launchObservation.failure = Object.freeze(receiptFailure(clock));
  const native: string[][] = [];
  internal.diagnosticCommand = async (_binary: string, argv: string[]) => { native.push(argv); return ''; };
  const writes: Array<{ path: string; text: string; options: unknown }> = [];
  internal.launchMkdir = async () => undefined;
  internal.launchWrite = async (path: string, text: string, options: unknown) => { writes.push({ path, text, options }); };
  const child = new ReceiptChild();
  let queries = 0;
  internal.launchCollector = (udid: string, failure: IOSLaunchFailure, fixture: string, hooks: IOSReceiptHooks) => collectIOSLaunchReceipt(udid, failure, fixture, {
    ...hooks, clock, spawn: () => {
      queries += 1;
      queueMicrotask(() => { child.emit('spawn'); child.stdout.emit('data', Buffer.from(receiptLoad())); child.close(); });
      return child as unknown as ChildProcess;
    },
  });
  return { ...a, internal, clock, native, writes, child, queries: () => queries };
}

type ReceiptLaunchMode = 'failure' | 'success' | 'unrelated' | '99' | 'driver-fatal';
async function receiptLaunchAdapter(name: string, initialMode: ReceiptLaunchMode = 'failure') {
  const clock = new ReceiptClock();
  const trace: string[] = [];
  let mode = initialMode;
  let tapped = false;
  let rounds = 0;
  let contexts = 0;
  const a = await adapter(`receipt-launch-${name}`, ({ path, body }) => {
    if (path.endsWith('/screenshot')) { trace.push('screenshot'); return value(''); }
    if (path.endsWith('/context')) {
      if (tapped && mode === '99') { contexts += 1; clock.elapsed += 11; }
      return value(null);
    }
    if (path.endsWith('/click')) { tapped = true; trace.push('click'); return value(null); }
    if (body.script === 'mobile: queryAppState') {
      if (mode === '99') {
        const web = body.args.bundleId === 'com.apple.webapp';
        trace.push(web ? '1' : '4');
        if (!web) rounds += 1;
        return value(web ? 1 : 4);
      }
      if (mode === 'failure') return Response.json({ value: { error: 'unknown error', message: 'synthetic provider refusal' } }, { status: 500 });
      if (mode === 'driver-fatal') {
        const response = new Response(null);
        response.text = async () => { throw new TypeError('synthetic body interrupted'); };
        return response;
      }
      return value(4);
    }
    if (path.endsWith('/alert/text')) return Response.json({ value: { error: 'no such alert', message: 'No alert' } }, { status: 404 });
    if (path.endsWith('/element/springboard-root/elements')) return value([element('springboard-root')]);
    if (body.script === 'mobile: activeAppInfo') { trace.push('foreground'); return value({ bundleId: 'com.apple.webapp', pid: 42 }); }
    if (body.script === 'mobile: getContexts') {
      assert.equal((a.platform as any).launchObservation.queryEligible, false);
      trace.push('attachment');
      return value([published]);
    }
    if (path.endsWith('/url')) return value(`${origin}/`);
    if (body.script?.startsWith('return {')) return value({ origin, standalone: true, applicationInitialized: true });
    throw new Error('unexpected receipt launch command');
  }, clock.mono, false);
  const internal = a.platform as any;
  internal.udid = receiptUdid;
  internal.launchObservation = new IOSLaunchObservation(clock);
  internal.observeCurrentNativeForeground = async () => 'com.apple.springboard';
  internal.setNativeObservationTarget = async () => undefined;
  internal.ensureSpringBoardForeground = async () => undefined;
  internal.observeHomeIcon = async () => {
    if (mode === 'unrelated') throw new Error('synthetic unrelated preparation failure');
    return { state: 'ready', icon: 'icon', page: { current: 1, total: 1, raw: 'Page 1 of 1' } };
  };
  internal.waitForHomeIconReadiness = async (observation: unknown) => observation;
  const native: string[][] = [];
  internal.diagnosticCommand = async (_binary: string, argv: string[]) => { trace.push('native'); native.push(argv); return ''; };
  const writes: Array<{ path: string; text: string; options: unknown }> = [];
  internal.launchMkdir = async () => { trace.push('mkdir'); };
  internal.launchWrite = async (path: string, text: string, options: unknown) => { trace.push('write'); writes.push({ path, text, options }); };
  const child = new ReceiptChild();
  let queries = 0;
  let collect: () => void = () => { child.stdout.emit('data', Buffer.from(receiptLoad())); child.close(); };
  internal.launchCollector = (udid: string, failure: IOSLaunchFailure, fixture: string, hooks: IOSReceiptHooks) => {
    trace.push('collector');
    return receiptSettled(collectIOSLaunchReceipt(udid, failure, fixture, { ...hooks, clock, spawn: () => {
      queries += 1;
      queueMicrotask(() => { child.emit('spawn'); collect(); });
      return child as unknown as ChildProcess;
    } }), clock);
  };
  const marker = join(a.outputDir, 'synthetic-receipt-ownership');
  await writeFile(marker, `ios:${receiptUdid}\n`);
  const launch = async () => {
    const previousMarker = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
    const previousWait = PhaseBudget.prototype.wait;
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = marker;
    PhaseBudget.prototype.wait = async function (ms: number) {
      if (mode !== '99') return;
      if (!tapped) return;
      clock.elapsed += ms;
      if (rounds === 99) clock.elapsed = 29_247;
    };
    try { await a.platform.launchInstalledApp(); }
    finally {
      PhaseBudget.prototype.wait = previousWait;
      if (previousMarker === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
      else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = previousMarker;
    }
  };
  return { ...a, internal, clock, trace, native, writes, child, launch, queries: () => queries, rounds: () => rounds, contexts: () => contexts,
    setMode: (next: ReceiptLaunchMode) => { mode = next; tapped = false; }, collect: (callback: () => void) => { collect = callback; } };
}

const receiptInvalidOrigin = () => receiptLine(`Web Clip with identifier '${receiptClip}', script from origin <WKSecurityOrigin: 0x1234; protocol = https; host = 999.999.999.999; port = 0> updated app badge count to 0`, 'com.apple.mobilesafari', 'WebPush');
function receiptFaultChunk(child: ReceiptChild) {
  const prefix = receiptLoad() + receiptInvalidOrigin();
  const fault = receiptLoad('CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC');
  const tail = 'https://synthetic:SYNTHETIC_FAULT_SECRET@secret.invalid/private?key=SYNTHETIC_FAULT_SECRET\n';
  const chunk = Buffer.from(prefix + fault + tail);
  const stringify = JSON.stringify;
  JSON.stringify = ((record: any) => {
    if (record?.webClip === 'CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC') throw Object.assign(new Error('SYNTHETIC_FAULT_SECRET'), { path: tail, cause: new Error(tail) });
    return stringify(record);
  }) as typeof JSON.stringify;
  try { child.stdout.emit('data', chunk); }
  finally { JSON.stringify = stringify; }
  return { received: chunk.byteLength, retained: Buffer.byteLength(prefix + fault), discarded: Buffer.byteLength(tail) };
}

test('SM55 receipt ordinary delayed writes stay owned and frozen on reentry', async () => {
  const a = await receiptCaptureAdapter('delayed');
  let release!: () => void;
  let started!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  const writing = new Promise<void>(resolve => { started = resolve; });
  a.internal.launchWrite = async (path: string, text: string, options: unknown) => {
    a.writes.push({ path, text, options });
    if (a.writes.length === 1) { started(); await pending; }
  };
  const first = a.platform.captureSanitizedEvidence('failure');
  assert.equal(a.platform.captureSanitizedEvidence('failure'), first);
  await writing;
  const frozen = a.internal.launchReceipt;
  const payload = frozen.receiptText;
  assert.ok(Object.isFrozen(frozen.receipt.flags));
  let settled = false;
  void first.then(() => { settled = true; }, () => { settled = true; });
  assert.equal(a.platform.captureSanitizedEvidence('candidate'), first);
  a.clock.advance(25_000);
  a.child.stdout.emit('data', Buffer.from(receiptLoad('CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC')));
  a.child.emit('error', { code: 'ENOENT', message: 'late-secret' });
  a.child.close();
  await Promise.resolve();
  assert.equal(settled, false);
  assert.equal(a.internal.launchPersistence.events.state, 'pending');
  assert.equal(a.internal.launchReceipt, frozen);
  assert.equal(frozen.receiptText, payload);
  assert.equal(a.requests.some(request => request.path.endsWith('/screenshot')), false);
  release();
  await first;
  assert.equal(a.queries(), 1);
  assert.equal(a.writes.length, 2);
  assert.equal(a.internal.launchPersistence.events.elapsedMs, 25_000);
  assert.equal(a.internal.launchPersistence.receipt.state, 'fulfilled');
  assert.equal(JSON.parse(a.writes[1].text).persistenceCompletion, 'unavailable-pre-write');
  assert.deepEqual(a.writes.map(write => write.path.split('/').at(-1)), ['ios-launch-owner-events.jsonl', 'ios-launch-receipt.json']);
  assert.ok(a.writes.every(write => JSON.stringify(write.options) === JSON.stringify({ flag: 'wx', mode: 0o600 })));
  assert.equal(a.native.some(argv => argv.includes('log')), false);
  assert.equal(a.native.length, 2);
  await a.platform.captureSanitizedEvidence('failure');
  assert.equal(a.writes.length, 2);
});

test('SM55 receipt rejected partial writes preserve first fatal without retry', async () => {
  for (const rejected of [1, 2]) {
    const a = await receiptLaunchAdapter(`reject-${rejected}`);
    let fatal: unknown;
    await assert.rejects(a.launch, error => { fatal = error; return true; });
    const failure = a.internal.launchObservation.failure;
    const driverFatal = a.driver.snapshot().firstFatal;
    assert.equal(failure.eligible, true);
    a.internal.launchWrite = async (path: string, text: string, options: unknown) => {
      a.writes.push({ path, text, options });
      if (a.writes.length === rejected) { a.clock.advance(25_000); throw Object.assign(new Error('synthetic-private-path'), { code: 'ENOSPC', cause: new Error('secret') }); }
    };
    await Promise.all([a.platform.captureSanitizedEvidence('failure'), a.platform.captureSanitizedEvidence('failure')]);
    assert.equal(a.queries(), 1);
    assert.equal(a.writes.length, rejected);
    assert.equal(a.internal.launchObservation.originalFatal, fatal);
    assert.equal(a.internal.launchObservation.failure, failure);
    assert.equal(a.driver.snapshot().firstFatal, driverFatal);
    assert.equal(a.clock.timers.size, 0);
    const status = a.platform.evidenceSnapshot().launchPersistence as any;
    assert.equal(status[rejected === 1 ? 'events' : 'receipt'].error, 'ENOSPC');
    assert.equal(JSON.stringify(status).includes('synthetic-private'), false);
    assert.equal(a.native.length, 0);
    await a.platform.captureSanitizedEvidence('failure');
    assert.equal(a.writes.length, rejected);
  }
});

test('SM55 receipt pending rejection and directory refusal are owned once', async () => {
  const a = await receiptCaptureAdapter('pending-rejection');
  let reject!: (error: unknown) => void;
  let started!: () => void;
  const pending = new Promise<void>((_resolve, rejectWrite) => { reject = rejectWrite; });
  const writing = new Promise<void>(resolve => { started = resolve; });
  a.internal.launchWrite = async (path: string, text: string, options: unknown) => {
    a.writes.push({ path, text, options }); started(); await pending;
  };
  const capture = a.platform.captureSanitizedEvidence('failure');
  await writing;
  const frozen = a.internal.launchReceipt.receiptText;
  a.clock.advance(30_000);
  assert.equal(a.platform.captureSanitizedEvidence('paired'), capture);
  assert.equal(a.internal.launchPersistence.events.state, 'pending');
  reject(Object.assign(new Error('synthetic-secret-path'), { code: 'EIO', cause: new Error('secret') }));
  await capture;
  assert.equal(a.internal.launchPersistence.events.state, 'rejected');
  assert.equal(a.internal.launchPersistence.events.elapsedMs, 30_000);
  assert.equal(a.platform.evidenceSnapshot().launchCollectionAndPersistenceElapsedMs, 30_000);
  assert.equal(a.internal.launchReceipt.receiptText, frozen);
  assert.equal(a.writes.length, 1);
  assert.equal(a.queries(), 1);
  assert.equal(a.native.length, 0);
  assert.equal(JSON.stringify(a.platform.evidenceSnapshot().launchPersistence).includes('synthetic-secret'), false);
  const b = await receiptCaptureAdapter('directory-refusal');
  b.internal.launchMkdir = async () => { throw { code: 'EEXIST', path: 'synthetic-secret' }; };
  await b.platform.captureSanitizedEvidence('failure');
  await b.platform.captureSanitizedEvidence('failure');
  assert.equal(b.queries(), 1);
  assert.equal(b.writes.length, 0);
  assert.equal(b.internal.launchPersistence.directory.error, 'EEXIST');
  const c = await receiptCaptureAdapter('collector-refusal');
  let attempts = 0;
  c.internal.launchCollector = async () => { attempts += 1; throw new Error('synthetic-private-callback'); };
  await c.platform.captureSanitizedEvidence('failure');
  await c.platform.captureSanitizedEvidence('failure');
  assert.equal(attempts, 1);
  assert.equal(c.writes.length, 0);
  assert.equal(c.native.length, 0);
  assert.equal(c.platform.evidenceSnapshot().nativeDiagnosticsBlocked, true);
  assert.equal(JSON.stringify(c.platform.evidenceSnapshot()).includes('synthetic-private-callback'), false);
});

test('SM55 receipt unconfirmed stop fences all native diagnostics before persistence', async () => {
  for (const mode of ['deadline', 'callback-error', 'abort-inside-spawn']) {
    const a = await receiptLaunchAdapter(`unconfirmed-${mode}`);
    let fatal: unknown;
    await assert.rejects(a.launch, error => { fatal = error; return true; });
    const count = a.requests.length;
    a.collect(() => {
      if (mode === 'callback-error') a.child.emit('error', { code: 'EIO', message: 'synthetic-private-error' });
      a.clock.advance(mode === 'deadline' ? 19_000 : 2_000);
    });
    if (mode === 'abort-inside-spawn') {
      const controller = new AbortController();
      a.internal.launchCollector = (udid: string, failure: IOSLaunchFailure, fixture: string, hooks: IOSReceiptHooks) => receiptSettled(collectIOSLaunchReceipt(udid, failure, fixture, {
        ...hooks, signal: controller.signal, spawn: () => {
          controller.abort();
          queueMicrotask(() => a.clock.advance(2_000));
          return a.child as unknown as ChildProcess;
        },
      }), a.clock);
    }
    let release!: () => void;
    let writing = false;
    const pending = new Promise<void>(resolve => { release = resolve; });
    a.internal.launchWrite = async (path: string, text: string, options: unknown) => {
      a.writes.push({ path, text, options });
      if (a.writes.length === 1) { writing = true; await pending; }
    };
    const first = a.platform.captureSanitizedEvidence('failure');
    try {
      for (let turn = 0; turn < 100 && !writing; turn++) await Promise.resolve();
      assert.equal(writing, true, 'bounded collector must reach the held ordinary write');
      assert.equal(a.internal.launchPersistence.events.state, 'pending');
      assert.equal(a.platform.evidenceSnapshot().nativeDiagnosticsBlocked, true);
      assert.equal(a.platform.captureSanitizedEvidence('candidate'), first);
      assert.equal(a.platform.captureSanitizedEvidence('failure'), first);
      const frozen = a.internal.launchReceipt.receiptText;
      a.clock.advance(25_000);
      a.child.emit('spawn');
      a.child.close();
      a.child.stdout.emit('data', Buffer.from(receiptLoad()));
      assert.equal(a.internal.launchReceipt.receiptText, frozen);
      assert.equal(a.native.length, 0);
      assert.equal(a.requests.length, count);
      assert.equal(a.writes.length, 1);
      assert.equal(a.internal.launchObservation.originalFatal, fatal);
    } finally { release(); await first; }
    assert.equal(a.native.length, 0);
    assert.equal(a.requests.length, count);
    assert.equal(a.writes.length, 2);
    assert.equal(a.clock.timers.size, 0);
    assert.deepEqual(a.child.signals, mode === 'callback-error' ? [] : ['SIGTERM', 'SIGKILL']);
  }
});

test('SM55 receipt ineligible failure omits broad fallback and success adds no query', async () => {
  const a = await receiptCaptureAdapter('ineligible');
  a.internal.launchObservation.failure = { ...receiptFailure(a.clock), eligible: false };
  await a.platform.captureSanitizedEvidence('failure');
  assert.equal(a.queries(), 0);
  assert.equal(a.internal.launchReceipt.receipt.outcome, 'not-eligible');
  assert.equal(a.native.some(argv => argv.includes('log')), false);
  const b = await receiptLaunchAdapter('success', 'success');
  await b.launch();
  assert.equal(b.platform.evidenceSnapshot().installedDocumentBound, true);
  assert.equal(b.internal.launchObservation.queryEligible, false);
  assert.equal(b.internal.launchObservation.failure, undefined);
  await b.platform.captureSanitizedEvidence('paired');
  await b.platform.captureSanitizedEvidence('candidate');
  assert.equal(b.queries(), 0);
  assert.equal(b.writes.length, 0);
  assert.equal(b.trace.includes('collector'), false);
  assert.equal(b.native.length, 8, 'existing checkpoint commands only');
  b.setMode('unrelated');
  await assert.rejects(b.launch);
  await b.platform.captureSanitizedEvidence('failure');
  assert.equal(b.queries(), 0);
  assert.equal(b.trace.includes('collector'), false);
  assert.equal(b.internal.launchObservation.failure, undefined);
  for (const later of ['failure', 'success', 'success-unrelated']) {
    const c = await receiptLaunchAdapter(`revoked-${later}`);
    let fatal: unknown;
    await assert.rejects(c.launch, error => { fatal = error; return true; });
    const failure = c.internal.launchObservation.failure;
    const facts = JSON.stringify(failure);
    assert.equal(failure.eligible, true);
    assert.equal(c.internal.launchObservation.queryEligible, true);
    c.setMode(later === 'failure' ? 'failure' : 'success');
    if (later === 'failure') await assert.rejects(c.launch, error => error !== fatal);
    else await c.launch();
    assert.equal(c.internal.launchObservation.queryEligible, false);
    if (later === 'success-unrelated') {
      c.setMode('unrelated');
      await assert.rejects(c.launch, error => error !== fatal);
    }
    const first = c.platform.captureSanitizedEvidence('failure');
    assert.equal(c.platform.captureSanitizedEvidence('candidate'), first);
    await first;
    assert.equal(c.queries(), 0);
    assert.equal(c.internal.launchReceipt.receipt.outcome, 'not-eligible');
    assert.equal(c.internal.launchReceipt.receipt.query, null);
    assert.equal(c.internal.launchObservation.failure, failure);
    assert.equal(JSON.stringify(failure), facts);
    assert.equal(c.internal.launchObservation.originalFatal, fatal);
    assert.equal(c.native.some(argv => argv.includes('log')), false);
    assert.equal(c.native.length, 2);
    assert.equal(c.trace.filter(event => event === 'collector').length, 1);
    assert.equal(c.clock.timers.size, 0);
  }
  const d = await receiptLaunchAdapter('driver-fatal', 'driver-fatal');
  let fatal: unknown;
  await assert.rejects(d.launch, error => { fatal = error; return /APPIUM_INTERRUPTED/u.test(String(error)); });
  const driverFatal = d.driver.snapshot().firstFatal;
  assert.equal(driverFatal?.code, 'APPIUM_INTERRUPTED');
  await d.platform.captureSanitizedEvidence('failure');
  assert.equal(d.queries(), 0);
  assert.equal(d.internal.launchObservation.originalFatal, fatal);
  assert.equal(d.driver.snapshot().firstFatal, driverFatal);
  assert.equal(d.driver.snapshot().unusable, true);
  assert.equal(d.native.some(argv => argv.includes('log')), false);
  assert.equal(d.clock.timers.size, 0);
});

test('SM55 receipt original 99 state pairs 753 context 742 unsent and ordering', async () => {
  for (const outcome of ['zero', 'nonzero', 'timeout', 'cap', 'late', 'projection-fault']) {
    const a = await receiptLaunchAdapter(`provider-${outcome}`, '99');
    let fatal: unknown;
    await assert.rejects(a.launch, error => { fatal = error; return /APPIUM_COMMAND_NOT_ADMITTED/u.test(String(error)); });
    assert.equal(a.rounds(), 99);
    assert.equal(a.contexts(), 100);
    const launchTrace = ['click', ...Array.from({ length: 99 }, () => ['1', '4']).flat()];
    assert.deepEqual(a.trace, launchTrace);
    const commands = a.driver.snapshot().commands;
    assert.equal(commands.at(-2)!.timeoutMs, 753);
    assert.equal(commands.at(-1)!.timeoutMs, 742);
    assert.equal(commands.at(-1)!.timing!.sent, false);
    const failure = a.internal.launchObservation.failure;
    const driverFatal = a.driver.snapshot().firstFatal;
    const facts = JSON.stringify(failure);
    const count = a.requests.length;
    assert.equal(failure.eligible, true);
    assert.equal(a.internal.launchObservation.originalFatal, fatal);
    assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, false);
    assert.equal(a.requests.filter(request => request.path.endsWith('/click')).length, 1);
    assert.equal(a.budget.recoveryCount, 0);
    assert.equal(a.requests.some(request => request.path.endsWith('/appium/settings')), false);
    let faultCounts: ReturnType<typeof receiptFaultChunk> | undefined;
    a.collect(() => {
      if (outcome === 'projection-fault') {
        faultCounts = receiptFaultChunk(a.child);
        a.clock.advance(2_000);
        return;
      }
      if (outcome === 'timeout') { a.clock.advance(19_000); return; }
      if (outcome === 'cap') a.child.stdout.emit('data', Buffer.alloc(IOS_LAUNCH_LIMITS.stdoutBytes + 1));
      else a.child.stdout.emit('data', Buffer.from(receiptLoad()));
      a.child.close(outcome === 'nonzero' ? 64 : 0);
    });
    const capture = a.platform.captureSanitizedEvidence('failure');
    assert.equal(a.platform.captureSanitizedEvidence('candidate'), capture);
    await capture;
    const result = a.internal.launchReceipt;
    const frozen = result.receiptText;
    if (outcome === 'late') {
      a.child.stdout.emit('data', Buffer.from(receiptLoad('BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB')));
      a.child.close();
      a.clock.advance(25_000);
    }
    assert.equal(a.queries(), 1);
    assert.equal(a.writes.length, 2);
    assert.equal(result.receipt.failure, failure);
    assert.equal(result.receiptText, frozen);
    assert.equal(JSON.stringify(failure), facts);
    assert.equal(a.internal.launchObservation.originalFatal, fatal);
    assert.equal(a.driver.snapshot().firstFatal, driverFatal);
    assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, false);
    assert.equal(a.budget.recoveryCount, 0);
    assert.equal(a.requests.slice(count).every(request => request.path.endsWith('/screenshot')), true);
    assert.equal(a.native.some(argv => argv.includes('log')), false);
    const blocked = outcome === 'timeout' || outcome === 'projection-fault';
    assert.deepEqual(a.trace.slice(launchTrace.length), blocked
      ? ['collector', 'mkdir', 'write', 'write']
      : ['collector', 'mkdir', 'write', 'write', 'screenshot', 'native', 'native']);
    assert.equal(a.requests.length, count + (blocked ? 0 : 1));
    assert.equal(result.receipt.outcome, outcome === 'zero' || outcome === 'late' ? 'public-projection-unreviewed' : 'incomplete');
    if (faultCounts) {
      for (const key of ['received', 'retained', 'discarded'] as const) assert.equal(result.receipt.stdout[key], faultCounts[key]);
      assert.equal(result.receipt.stdout.received, result.receipt.stdout.retained + result.receipt.stdout.discarded);
      assert.equal(result.receipt.stdout.withheld, 2);
      assert.equal(result.receipt.eventCount, 1);
      assert.equal(result.receipt.flags.handlerFault, true);
      assert.equal((result.receiptText + result.eventsText).includes('SYNTHETIC_FAULT_SECRET'), false);
    }
    await a.platform.captureSanitizedEvidence('failure');
    assert.equal(a.writes.length, 2);
    assert.equal(a.clock.timers.size, 0);
  }
});

export async function runIOSRegressions(): Promise<void> {
  let failures = 0;
  for (const [name, body] of tests) {
    if (process.env.IOS_TEST_FILTER && !new RegExp(process.env.IOS_TEST_FILTER, 'u').test(name)) continue;
    try {
      const outcome = await body();
      process.stdout.write(outcome ? `ok - iOS ${name} # SKIP ${outcome}\n` : `ok - iOS ${name}\n`);
    } catch (error) {
      failures += 1;
      process.stderr.write(`not ok - iOS ${name}: ${error instanceof Error ? error.stack : String(error)}\n`);
    }
  }
  if (failures) throw new Error(`iOS regressions: ${failures} failures`);
}

if (import.meta.main) await runIOSRegressions();

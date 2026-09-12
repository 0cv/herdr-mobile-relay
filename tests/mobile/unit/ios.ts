import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFile, mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { setTimeout as wait } from 'node:timers/promises';
import { IOSPlatform, iosOpenURLProcessEvidence, nativeActionListEvidence } from '../platforms/ios';
import { AppiumClient, isRetryableElementLookupError } from '../support/webdriver';
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
      await assert.rejects(() => platform.installFromBrowser(), mode === 'add-hung' ? /APPIUM_TIMEOUT/u : mode === 'add-budget' ? /insufficient whole transaction allowance/u : /Add: confirmation control was not ready/u);
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
type ConfirmationFault = 'stale' | 'unrelated' | 'invalid-session' | 'misleading-404' | 'malformed' | 'malformed-value' | 'transport' | 'interrupted' | 'hung';
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
  fourthReadStale?: boolean;
  after?: (boundary: string, lookup: number) => number;
  parentRemaining?: number;
  latency?: Partial<Record<ConfirmationBoundary, number>>;
} = {}) {
  const fixture = options.recorded;
  const shareSources = fixture ? await Promise.all(Object.values(fixture.sources).map((path) => readFile(join(fixtureDir, path), 'utf8'))) : [browser, before, after];
  let now = 0;
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
    observations.push({ boundary, lookup: lookups, now });
  };
  const { platform, driver, requests, budget, outputDir } = await adapter(`confirmation-${name}`, async ({ path, body, signal }) => {
    const fault = () => {
      if (options.fault === 'hung') return new Promise<Response>((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
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
        advance('identity');
        identityReads++;
        if (identityReads === options.staleIdentityRead) return stale();
        if (options.faultAt === 'identity') return fault();
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
      return value([element(`${body.value.includes('Add to Home Screen') ? 'target' : 'container'}-${scrolls}`)]);
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
        confirming = true;
        if (options.parentRemaining !== undefined) now = 120_000 - options.parentRemaining;
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
  assert.deepEqual(clicks.slice(0, 2), ['share', 'target-1']);
  assert.ok(clicks.length <= 3, 'final Add must be dispatched at most once');
  const settingsWrites = requests.filter(request => request.path.endsWith('/appium/settings') && 'waitForIdleTimeout' in (request.body.settings || {}));
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
  test(`confirmation persistent hypothetical ${state} exhausts the original deadline without a click`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`persistent-${state}`, { states: [state], persistent: true });
    assert.match(String(replay.error), /Add: confirmation control was not ready/u);
    assert.match(String(replay.error), state === 'missing' ? /no such element/u : state.startsWith('stale') ? /stale element reference/u : new RegExp(state, 'u'));
    assert.ok(replay.lookups >= 2 && replay.lookups <= 8);
    assert.ok(replay.now <= 30_000);
    assert.equal(replay.clicks.length, 2);
    assert.equal(replay.driver.snapshot().unusable, false);
  });
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

for (const remaining of [68_000, 70_000]) {
  test(`confirmation hypothetical repeated same-dialog Add replacements keep the original ${remaining}ms deadline`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`same-dialog-deadline-${remaining}`, {
      replaceReadyIdentity: 'always', parentRemaining: remaining,
      after: (boundary) => boundary === 'identity' ? 1_000 : 0,
    });
    assert.match(String(replay.error), /confirmation control was not ready.*Add control was replaced/u);
    assert.equal(replay.lookups, remaining === 68_000 ? 4 : 5);
    assert.deepEqual(replay.clicks, ['share', 'target-1']);
    assert.equal(replay.now - (120_000 - remaining), remaining === 68_000 ? 8_000 : 10_000);
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

for (const remaining of [4_999, 7_999, 8_000, 66_999]) {
  test(`confirmation parent budget ${remaining} never admits a partial tail observation`, async () => {
    const skip = requireXmlLint();
    if (skip) return skip;
    const replay = await confirmationReplay(`parent-${remaining}`, { parentRemaining: remaining, after: (boundary) => boundary === 'lookup' ? 1_001 : 0 });
    assert.match(String(replay.error), /insufficient whole transaction allowance/u);
    assert.equal(replay.lookups, 0);
    assert.deepEqual(replay.attributes, []);
    assert.equal(replay.clicks.length, 2);
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
    parentRemaining: 67_000, after: (boundary) => boundary === 'identity' && ++identityReads === 2 ? 58_001 : 0,
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

test('Plan13 recorded hierarchy inputs retain their exact evidence hashes', async () => {
  for (const [file, expected] of Object.entries(recordedIteration13.files)) {
    assert.equal(createHash('sha256').update(await readFile(join(fixtureDir, file))).digest('hex'), expected);
  }
});

const lifecycleError = recordedIteration13.safariAUTFailure.response;

async function lifecycleReplay(name: string) {
  const state = {
    safariRunning: true, foreground: 'com.apple.mobilesafari', installed: false, pid: 16089 as unknown,
    overlay: '', documentOrigin: origin, standalone: true, url: `${origin}/`,
    settingsFault: '', stateFault: undefined as unknown, activeFault: undefined as unknown,
    alertFault: false, ignoreHome: false, wrongLaunch: '', systemObservationFault: '', slowInstalledObservation: false,
    systemName: 'SpringBoard', systemRootCount: 1,
  };
  let settings: Record<string, unknown> = { defaultActiveApplication: 'auto', respectSystemAlerts: false };
  const appState = (bundle: string) => {
    if (bundle === 'com.apple.webapp' && state.stateFault !== undefined) return state.stateFault;
    if (bundle === state.foreground) return 4;
    if (bundle === 'com.apple.springboard') return state.overlay && state.overlay !== 'app-dialog' ? 4 : 2;
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
  const replay = await adapter(`lifecycle-${name}`, async ({ path, body, method }) => {
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
      if (body.value.includes('XCUIElementTypeApplication')) {
        const xml = `<AppiumAUT>${Array.from({ length: state.systemRootCount }, () => `<XCUIElementTypeApplication name="${state.systemName}"/>`).join('')}</AppiumAUT>`;
        return value(Array.from({ length: xpathCount(xml, String(body.value)) }, (_, index) => element(index ? `other-root-${index}` : 'springboard-root')));
      }
      return value([element('home-icon')]);
    }
    if (path.endsWith('/home-icon/attribute/hittable')) return value('true');
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
    assert.ok(/pressButton|terminateApp/u.test(a.requests[index + 2].body.script || '') || a.requests[index + 2].path.endsWith('/home-icon/click'));
  }
  assert.equal(a.requests.some((r) => /activateApp|launchApp/u.test(r.body.script || '') || (r.path.endsWith('/url') && r.method === 'POST')), false);
  assert.equal(a.driver.snapshot().unusable, false);
  await writeSanitizedJson(join(a.outputDir, 'ios-lifecycle-result.json'), { proofKind: 'Source-derived WDA 16.12.1 branch model with hypothetical lifecycle and system-state replies; actual adapter/client, no native execution.', requests: a.requests, evidence: a.platform.evidenceSnapshot() });
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

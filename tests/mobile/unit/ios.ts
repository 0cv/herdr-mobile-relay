import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFile, mkdtemp, mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { setTimeout as wait } from 'node:timers/promises';
import { IOSPlatform, iosOpenURLProcessEvidence, nativeActionListEvidence } from '../platforms/ios';
import { AppiumClient } from '../support/webdriver';
import { PhaseBudget } from '../support/budget';
import { writeSanitizedJson } from '../support/diagnostics';
import { CommandError, command } from '../support/process';
import recorded from './fixtures/ios/publication.json';

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

async function adapter(name: string, handler: (request: Request) => Response | Promise<Response>, now?: () => number) {
  await mkdir(outputRoot, { recursive: true });
  const outputDir = await mkdtemp(join(outputRoot, `${name}-`));
  const budget = new PhaseBudget(name, { timeoutMs: 120_000, recoveryLimit: 0, now });
  const platform = new IOSPlatform({ origin, appiumUrl: 'http://protocol.invalid', outputDir, certificate: '', setupUrl: '', deviceId: 'protocol-only', budget });
  const requests: Request[] = [];
  let inFlight = 0;
  const driver = new AppiumClient('http://protocol.invalid', 30_000, async (input, init) => {
    assert.equal(++inFlight, 1, 'Appium requests must not overlap');
    try {
      const request = { path: new URL(String(input)).pathname, body: init?.body ? JSON.parse(String(init.body)) : {}, method: init?.method || 'GET', signal: init?.signal };
      requests.push(request);
      if (request.path === '/session') return Response.json({ value: {}, sessionId: 'protocol' });
      return await handler(request);
    } finally {
      inFlight -= 1;
    }
  });
  await driver.create({ capabilities: {} });
  driver.setBudget(budget);
  (platform as any).driver = driver;
  (platform as any).installedBundleId = 'com.apple.webapp';
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
      assert.equal(discoveries, mode === 'hung' ? 1 : 3);
      assert.equal(completed, mode === 'hung' ? 0 : discoveries);
      assert.equal(budget.recoveryCount, 0);
      assert.equal(platform.evidenceSnapshot().selectedInstalledContext, '');
      assert.equal(platform.evidenceSnapshot().installedDocumentBound, false);
      assert.equal(platform.evidenceSnapshot().ownershipFailure, undefined);
      const commands = driver.snapshot().commands.filter((_entry, index) => requests[index]?.body.script === 'mobile: getContexts');
      assert.equal(commands.length, discoveries);
      assert.ok(commands.every((entry) => entry.timeoutMs === 18_000), 'every dispatched discovery needs the complete WebKit allowance');
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
      assert.ok(driver.snapshot().commands.every((entry) => !entry.timedOut && !entry.error));
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
    assert.equal(discoveries, 2);
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

function xpathCount(source: string, xpath: string): number {
  return Number(execFileSync('xmllint', ['--xpath', `count(${xpath})`, '-'], { input: source, encoding: 'utf8' }).trim());
}

function requireXmlLint(): string | undefined {
  try { execFileSync('xmllint', ['--version'], { stdio: 'pipe' }); return undefined; }
  catch { return 'xmllint is required for recorded XML XPath protocol checks'; }
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
      if (body.script === 'mobile: activeAppInfo') return value({ bundleId: 'com.apple.mobilesafari' });
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
          if (mode === 'add-disabled' && path.endsWith('/enabled')) return value('false');
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
      await assert.rejects(() => platform.installFromBrowser(), mode === 'add-hung' ? /APPIUM_TIMEOUT/u : mode === 'add-disabled' ? /Add: control is disabled/u : /Add: confirmation control was not ready/u);
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
    const lookups = driver.snapshot().commands.filter((_entry, index) => requests[index]?.body.value === 'Add');
    assert.ok(lookups.every((entry) => entry.timeoutMs >= 4_900 && entry.timeoutMs <= 5_000), 'confirmation probes must receive a complete native transaction, never optional-loop leftovers');
    assert.equal(driver.snapshot().unusable, mode === 'add-hung');
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

export async function runIOSRegressions(): Promise<void> {
  let failures = 0;
  for (const [name, body] of tests) {
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

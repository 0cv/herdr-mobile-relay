import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { IOSPlatform } from '../platforms/ios';
import { PhaseBudget, timingScale } from '../support/budget';
import { AppiumClient, type RequestEnforcementClock } from '../support/webdriver';
import recorded from './fixtures/ios/publication.json';

const origin = 'https://localhost:52101';
const value = (data: unknown) => Response.json({ value: data });
const element = (id: string) => ({ 'element-6066-11e4-a52e-4f735466cecf': id });
const published = { ...recorded.contexts.find(context => context.bundleId === 'com.apple.SafariViewService')!, url: recorded.publishedPages[0].url, title: recorded.publishedPages[0].title };
type Test = [string, () => Promise<void>];
type Mode = 'boundary' | 'early' | 'headers' | 'body' | 'parse' | 'invalid' | 'pending' | 'pending-home' | 'too-short'
  | 'owner' | 'session' | 'generation' | 'context' | 'reuse' | 'missing-pid' | 'wrong-bundle' | 'overlay' | 'alert'
  | 'launch-gap' | 'scenario-gap' | 'launch-proof' | 'scenario-proof';

async function fixture(mode: Mode) {
  let now = 0;
  const phases = new Map<string, PhaseBudget>();
  const requests: Array<{ path: string; method: string; body: any }> = [];
  const timers = new Map<ReturnType<typeof setTimeout>, number>();
  let timerId = 0;
  const clock: RequestEnforcementClock = {
    now: () => now,
    timer: (_callback, ms) => {
      const id = ++timerId as unknown as ReturnType<typeof setTimeout>;
      timers.set(id, ms);
      return id;
    },
    clear: id => { timers.delete(id); },
  };
  const budget = new PhaseBudget('handoff-scenario', { timeoutMs: 45 * 60_000, now: clock.now, recoveryLimit: 0 });
  const phaseView = budget.phaseView.bind(budget);
  budget.phaseView = (name, timeout, reserve) => {
    const phase = phaseView(name, timeout, reserve);
    phases.set(name, phase);
    const childView = phase.phaseView.bind(phase);
    phase.phaseView = (childName, childTimeout, childReserve) => {
      const child = childView(childName, childTimeout, childReserve);
      phases.set(childName, child);
      return child;
    };
    return phase;
  };
  const outputDir = await mkdtemp(join(tmpdir(), 'ios-provider-handoff-'));
  const platform = new IOSPlatform({ origin, appiumUrl: 'http://protocol.invalid', outputDir, certificate: '', setupUrl: '', deviceId: 'protocol-only', budget });
  const native = platform as any;
  let clicked = false;
  let ready = false;
  let readinessQueries = 0;
  let foregroundProofs = 0;
  let attachmentEntries = 0;
  let providerPolicy: ReturnType<AppiumClient['nativeRequestPolicy']> | undefined;
  let providerDeadline = 0;
  let validatedAt = 0;
  let queryTimerMs = 0;
  let abortSignal: AbortSignal | null | undefined;
  let parsePending = false;
  const remaining = timingScale === 1 ? 1_000 : 1_261;
  const driver = new AppiumClient('http://protocol.invalid', 30_000, async (input, init) => {
    const path = new URL(String(input)).pathname;
    const body = init?.body ? JSON.parse(String(init.body)) : {};
    requests.push({ path, method: init?.method || 'GET', body });
    if (path === '/session') return Response.json({ value: {}, sessionId: 'protocol' });
    if (path.endsWith('/context')) {
      if (clicked && !ready) {
        const provider = phases.get('ios-provider')!;
        providerDeadline = now + provider.remainingMs;
        now = mode === 'early' ? now : providerDeadline - (mode === 'too-short' ? 999 : remaining);
      }
      return value(null);
    }
    if (path.endsWith('/home-icon/click')) { clicked = true; return value(null); }
    if (body.script === 'mobile: queryAppState') {
      assert.equal(body.args.bundleId, 'com.apple.webapp', 'no fallback query is needed after readiness');
      if (!ready) {
        readinessQueries++;
        abortSignal = init?.signal;
        if (mode === 'headers') now = providerDeadline + 1;
        const state = mode === 'invalid' ? null : mode === 'pending' || mode === 'pending-home' ? 2 : 4;
        const response = value(state);
        response.text = async () => {
          queryTimerMs = [...timers.values()].at(-1)!;
          if (mode === 'body') now = providerDeadline + 1;
          else if (mode !== 'headers') now += mode === 'pending-home' ? 760 : mode === 'early' ? 10 : remaining - 35;
          parsePending = mode === 'parse';
          return JSON.stringify({ value: state });
        };
        return response;
      }
      foregroundProofs++;
      assert.ok(phases.get('ios-native-foreground')!.remainingMs <= 30_000 * timingScale + 0.001, 'foreground keeps its original cap');
      assert.ok(phases.get('ios-native-foreground')!.remainingMs <= phases.get('ios-launch')!.remainingMs, 'foreground is a child of the original launch');
      return value(4);
    }
    if (path.endsWith('/element/springboard-root/elements')) return value(mode === 'overlay' ? [element('springboard-root'), element('overlay')] : [element('springboard-root')]);
    if (path.endsWith('/alert/text')) return mode === 'alert' ? value('dialog')
      : Response.json({ value: { error: 'no such alert' } }, { status: 404 });
    if (body.script === 'mobile: activeAppInfo') {
      if (mode === 'launch-proof') now += phases.get('ios-launch')!.remainingMs + 1;
      if (mode === 'scenario-proof') now += budget.remainingMs + 1;
      return value({ bundleId: mode === 'wrong-bundle' ? 'com.apple.mobilesafari' : 'com.apple.webapp', ...(mode === 'missing-pid' ? {} : { pid: recorded.foreground.pid }) });
    }
    if (body.script === 'mobile: installedContextContract') return value({ contract: 'herdr-installed-contexts-v1', selector: 'herdr-installed-contexts-v1' });
    if (body.script === 'mobile: getInstalledContexts') {
      assert.equal(foregroundProofs, 1, 'the scheduled proof must precede installed metadata');
      return value([published]);
    }
    if (path.endsWith('/url')) return value(`${origin}/`);
    if (body.script?.startsWith('return {')) return value({ origin, standalone: true, applicationInitialized: true });
    throw new Error(`unexpected handoff request ${path}`);
  }, clock.now, clock);
  await driver.create({ capabilities: {} });
  driver.setBudget(budget);
  native.driver = driver;
  native.springBoardRoot = 'springboard-root';
  const observeCurrent = native.observeCurrentNativeForeground.bind(platform);
  native.observeCurrentNativeForeground = async (...args: unknown[]) => clicked ? observeCurrent(...args) : 'com.apple.springboard';
  if (mode === 'pending' || mode === 'pending-home' || mode === 'too-short') native.installedBindingState = 'bound';
  native.setNativeObservationTarget = async () => undefined;
  native.ensureSpringBoardForeground = async () => undefined;
  native.observeHomeIcon = async () => ({ state: 'ready', icon: 'home-icon', page: { current: 1 } });
  const nativePolicy = driver.nativeRequestPolicy.bind(driver);
  driver.nativeRequestPolicy = phase => {
    const policy = nativePolicy(phase);
    if (phase.phase === 'ios-provider') {
      providerPolicy = policy;
      assert.equal(policy.finalAction, undefined);
    }
    return policy;
  };
  const mobile = driver.mobile.bind(driver);
  driver.mobile = async (command, args, timeout, policy) => {
    const result = await mobile(command, args, timeout, policy);
    if (command === 'queryAppState' && !ready) {
      validatedAt = now;
      assert.equal(driver.snapshot().lastCommand!.timing!.outcome, 'success');
      if (mode === 'owner' || mode === 'reuse') native.driver['sessionOwner'] = {};
      if (mode === 'reuse') native.driver['sessionGeneration']++;
      if (mode === 'session') native.driver['sessionId'] = 'replacement';
      if (mode === 'generation') native.driver['sessionGeneration']++;
      if (mode === 'context') native.driver['selectedContext'] = 'WEBVIEW_other';
      ready = true;
      if (mode === 'launch-gap') now += phases.get('ios-launch')!.remainingMs + 1;
      else if (mode === 'scenario-gap') now += budget.remainingMs + 1;
      else if (mode !== 'early' && mode !== 'pending-home') now += 82.67;
    }
    return result;
  };
  const attach = platform.attachToInstalledView.bind(platform);
  platform.attachToInstalledView = async (timeout = 30_000) => {
    attachmentEntries++;
    assert.equal(foregroundProofs, 1, 'exactly one full proof is scheduled between readiness and attachment');
    assert.ok(phases.get('ios-launch')!.remainingMs > 0);
    assert.ok(budget.remainingMs > 0);
    assert.ok(timeout <= 60_000);
    await attach(timeout);
  };
  const parse = JSON.parse;
  const run = async () => {
    const marker = join(outputDir, 'ios-ownership');
    await writeFile(marker, 'ios:protocol-only\n');
    const previous = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = marker;
    JSON.parse = (text, reviver) => {
      if (parsePending) { parsePending = false; now = providerDeadline + 1; }
      return parse(text, reviver);
    };
    try { await platform.launchInstalledApp(); } finally {
      JSON.parse = parse;
      if (previous === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
      else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = previous;
    }
  };
  return { platform, driver, requests, budget, phases, timers, run, providerPolicy: () => providerPolicy,
    observations: () => ({ readinessQueries, foregroundProofs, attachmentEntries, providerDeadline, validatedAt, queryTimerMs, now, abortSignal }) };
}

export const iosProviderHandoffCases: Test[] = [];

for (const mode of ['boundary', 'early'] as const) {
  iosProviderHandoffCases.push([`provider handoff ${mode} validates readiness once then proves foreground before attachment`, async () => {
    const a = await fixture(mode);
    await a.run();
    const observed = a.observations();
    assert.equal(observed.readinessQueries, 1);
    assert.ok(observed.validatedAt < observed.providerDeadline);
    if (mode === 'boundary') {
      assert.equal(observed.providerDeadline - observed.validatedAt, 35);
      assert.ok(Math.abs(observed.now - observed.providerDeadline - 47.67) < 0.001);
      assert.equal(observed.queryTimerMs, Math.floor((timingScale === 1 ? 1_000 : 1_261) / timingScale) * timingScale);
    }
    assert.equal(observed.attachmentEntries, 1);
    assert.equal(a.platform.evidenceSnapshot().installedDocumentBound, true);
    assert.equal(a.driver.snapshot().unusable, false);
    assert.equal(a.driver.snapshot().firstFatal, undefined);
    assert.equal(a.phases.get('ios-provider')!.snapshot().deadline, new Date(30_000 * timingScale).toISOString());
    assert.equal(a.phases.get('ios-launch')!.snapshot().deadline, new Date(180_000 * timingScale).toISOString());
    assert.equal(a.budget.recoveryCount, 0);
    assert.equal(a.timers.size, 0);
    assert.equal(a.requests.filter(request => request.path.endsWith('/home-icon/click')).length, 1);
    assert.equal(a.requests.some(request => request.body.script === 'mobile: activateApp' || request.body.script === 'mobile: getContexts'), false);
  }]);
}

iosProviderHandoffCases.push(['provider handoff pending-home recomputes SpringBoard allowance before dispatch', async () => {
  const a = await fixture('pending-home');
  await assert.rejects(a.run);
  const observed = a.observations();
  assert.equal(a.requests.filter(request => request.body.script === 'mobile: queryAppState' && request.body.args.bundleId === 'com.apple.springboard').length, 0);
  assert.equal(observed.readinessQueries, 1);
  assert.equal(observed.providerDeadline - observed.validatedAt, timingScale === 1 ? 240 : 501);
  assert.equal(observed.now, observed.validatedAt);
  if (timingScale === 3) {
    assert.equal(observed.queryTimerMs, 1_260);
    assert.match(a.driver.snapshot().lastCommand?.error ?? '', /APPIUM_COMMAND_NOT_ADMITTED/u);
    assert.equal(a.driver.snapshot().lastCommand?.timing?.sent, false);
  }
  assert.equal(observed.foregroundProofs, 0);
  assert.equal(observed.attachmentEntries, 0);
  assert.equal(a.requests.filter(request => request.path.endsWith('/home-icon/click')).length, 1);
  assert.equal(a.requests.some(request => request.body.script === 'mobile: getInstalledContexts' || request.body.script === 'mobile: getContexts'
    || request.body.script === 'mobile: activateApp' || request.body.script === 'mobile: pressButton' || request.body.script === 'mobile: swipe'), false);
  assert.equal(a.driver.snapshot().firstFatal, undefined);
  assert.equal(a.driver.snapshot().unusable, false);
  assert.equal(a.budget.recoveryCount, 0);
  assert.equal(a.timers.size, 0);
}]);

for (const mode of ['headers', 'body', 'parse'] as const) {
  iosProviderHandoffCases.push([`provider handoff ${mode} crossing the original readiness deadline is fatal before foreground`, async () => {
    const a = await fixture(mode);
    await assert.rejects(a.run, /APPIUM_TIMEOUT/u);
    assert.equal(a.observations().readinessQueries, 1);
    assert.equal(a.observations().foregroundProofs, 0);
    assert.equal(a.observations().attachmentEntries, 0);
    assert.equal(a.observations().abortSignal?.aborted, true);
    const snapshot = structuredClone(a.driver.snapshot());
    assert.equal(snapshot.firstFatal?.code, 'APPIUM_TIMEOUT');
    const count = a.requests.length;
    for (const followup of [() => a.driver.close(), () => a.driver.command('/status', 'GET'), () => a.driver.create({ capabilities: {} }), () => a.driver.mobile('queryAppState')]) {
      await assert.rejects(followup, /APPIUM_SESSION_UNUSABLE/u);
    }
    assert.equal(a.requests.length, count);
    assert.deepEqual(a.driver.snapshot(), snapshot);
    assert.equal(a.timers.size, 0);
  }]);
}

for (const mode of ['invalid', 'pending', 'too-short', 'owner', 'session', 'generation', 'context', 'reuse', 'missing-pid', 'wrong-bundle', 'overlay', 'alert', 'launch-gap', 'scenario-gap', 'launch-proof', 'scenario-proof'] as const) {
  iosProviderHandoffCases.push([`provider handoff ${mode} fails closed before attachment without another launch`, async () => {
    const a = await fixture(mode);
    await assert.rejects(a.run, mode === 'invalid' ? /IOS_CONTEXT_OWNERSHIP/u
      : ['owner', 'session', 'generation', 'context', 'reuse'].includes(mode) ? /APPIUM_SESSION/u
      : ['missing-pid', 'wrong-bundle', 'overlay', 'alert'].includes(mode) ? /IOS_CONTEXT_OWNERSHIP/u
      : /PHASE_BUDGET_EXHAUSTED|IOS_CONTEXT|APPIUM_COMMAND_NOT_ADMITTED/u);
    assert.equal(a.observations().attachmentEntries, 0);
    assert.equal(a.requests.filter(request => request.path.endsWith('/home-icon/click')).length, 1);
    assert.equal(a.requests.some(request => request.body.script === 'mobile: getInstalledContexts' || request.body.script === 'mobile: activateApp'), false);
    if (['invalid', 'pending', 'too-short', 'owner', 'session', 'generation', 'context', 'reuse', 'launch-gap', 'scenario-gap'].includes(mode)) assert.equal(a.observations().foregroundProofs, 0);
    if (['missing-pid', 'wrong-bundle', 'overlay', 'alert'].includes(mode)) {
      const count = a.requests.length;
      await assert.rejects(() => a.platform.attachToInstalledView(), /IOS_CONTEXT_OWNERSHIP/u);
      assert.equal(a.requests.length, count);
    }
    assert.equal(a.budget.recoveryCount, 0);
    assert.equal(a.timers.size, 0);
  }]);
}

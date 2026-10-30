import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import { AppiumClient as PolicyClient, WebDriverError as PolicyError, type RequestEnforcementClock, type WebDriverError, type WebDriverRequestTiming } from '../support/webdriver';
import { PhaseBudget } from '../support/budget';

type Test = [string, () => Promise<void>];

async function clientSource(): Promise<typeof import('../support/webdriver')> {
  const source = process.env.WEBDRIVER_TEST_SOURCE;
  return import(source ? pathToFileURL(source).href : new URL('../support/webdriver.ts', import.meta.url).href);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function assertTimingOrder(timing: Readonly<WebDriverRequestTiming>) {
  const stages = [timing.entryMs, timing.dispatchMs, timing.responseAvailableMs, timing.bodyReadStartMs,
    timing.bodyReadCompleteMs, timing.parseStartMs, timing.parseCompleteMs, timing.historyStartMs,
    timing.redactionCompleteMs, timing.historyCompleteMs, timing.settlementMs].filter((stage): stage is number => stage !== undefined);
  assert.ok(stages.every(stage => Number.isFinite(stage) && stage >= 0));
  assert.deepEqual(stages, [...stages].sort((a, b) => a - b));
  assert.equal(new Set(stages).size, stages.length, 'injected diagnostic clock distinguishes each observed stage');
  assert.equal(Object.isFrozen(timing), true);
}

async function policyClient(handler: (init?: RequestInit) => Promise<Response>) {
  let now = 0;
  let sent = 0;
  let signal: AbortSignal | null | undefined;
  let onNow: (() => void) | undefined;
  let timerMode: 'normal' | 'inline' | 'throw' | 'inline-throw' = 'normal';
  const wire: { url: string; method?: string }[] = [];
  const timers = new Map<ReturnType<typeof setTimeout>, () => void>();
  let ordinal = 0;
  const clock: RequestEnforcementClock = {
    now: () => { onNow?.(); return now; },
    timer: (callback) => {
      if (timerMode === 'throw') throw new Error('injected timer setup failure');
      if (timerMode === 'inline' || timerMode === 'inline-throw') callback();
      if (timerMode === 'inline-throw') throw new Error('injected failure after inline timeout');
      const id = ++ordinal as unknown as ReturnType<typeof setTimeout>;
      timers.set(id, callback);
      return id;
    },
    clear: id => { timers.delete(id); },
  };
  const client = new PolicyClient('http://policy.invalid', 30_000, async (input, init) => {
    wire.push({ url: String(input), method: init?.method });
    if (String(input).endsWith('/session')) return Response.json({ value: {}, sessionId: 'policy' });
    sent++;
    signal = init?.signal;
    assert.equal(init?.body, undefined);
    return handler(init);
  }, undefined, clock);
  await client.create({ capabilities: {} });
  const root = new PhaseBudget('policy-root', { timeoutMs: 120_000, now: clock.now });
  client.setBudget(root);
  const phase = root.phaseView('policy-confirmation', 75_000);
  const policy = Object.freeze({ ...client.nativeRequestPolicy(phase), finalAction: true });
  return { client, root, phase, policy, clock, timers, wire, sent: () => sent, signal: () => signal,
    observeNow: (callback: () => void) => { onNow = callback; },
    timerMode: (mode: 'normal' | 'inline' | 'throw' | 'inline-throw') => { timerMode = mode; },
    advance: (ms: number) => { now += ms; },
    expire: (ms: number) => { now += ms; for (const callback of [...timers.values()]) callback(); },
  };
}

export const webdriverInterruptionTests: Test[] = [];

webdriverInterruptionTests.push(['Appium request timing SM57 original5000 timeout retains first fatal after late200', async () => {
  const headers = deferred<Response>();
  const a = await policyClient(() => headers.promise);
  const action = a.client.click('add', 5_000, a.policy);
  let original: unknown;
  const rejected = assert.rejects(action, error => { original = error; return error instanceof PolicyError && error.code === 'APPIUM_TIMEOUT'; });
  a.expire(5_000);
  await rejected;
  const fatal = a.client.snapshot().firstFatal;
  assert.ok(fatal);
  assert.ok(original instanceof PolicyError);
  assert.equal(fatal.code, original.code);
  assert.equal(fatal.path, original.path);
  assert.equal(fatal.detail, original.message);
  assert.equal(a.signal()!.reason, original.cause);
  const saved = structuredClone(a.client.snapshot());
  const receipt = a.client.snapshot().lastCommand!.timing!;
  const settlement = (a.client as any).previousSettlementMs;
  assert.equal(Object.isFrozen(receipt), true);
  assert.equal(receipt.sent, true);
  assert.equal(receipt.outcome, 'timeout');
  assert.equal(a.client.snapshot().lastCommand!.timeoutMs, 5_000);
  headers.resolve(Response.json({ value: null }));
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(a.client.snapshot(), saved);
  assert.equal(a.sent(), 1);
  assert.equal((a.client as any).previousSettlementMs, settlement);
  assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing?.outcome === 'success').length, 0);
  await assert.rejects(() => a.client.click('add', 12_000, a.policy), /APPIUM_SESSION_UNUSABLE/u);
  assert.deepEqual(a.client.snapshot(), saved);
  assert.equal(a.client.snapshot().firstFatal, fatal);
  assert.equal(original.timedOut, true);
  assert.equal(a.timers.size, 0);
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 scoped final Add rejects late headers before delayed timer', async () => {
  for (const stage of ['headers', 'body', 'parse'] as const) {
    for (const elapsed of [11_999, 12_000]) {
      const headers = deferred<Response>();
      const a = await policyClient(() => headers.promise);
      const text = '{"value":null}';
      const parse = JSON.parse;
      let replacementCalls = 0;
      JSON.parse = ((input: string, reviver?: Parameters<typeof JSON.parse>[1]) => {
        if (stage === 'parse' && input === text) a.advance(elapsed);
        return parse(input, reviver);
      }) as typeof JSON.parse;
      try {
        const action = a.client.click('add', 12_000, a.policy);
        const settled = elapsed < 12_000 ? action : assert.rejects(action, /APPIUM_TIMEOUT/u);
        assert.equal(a.timers.size, 1);
        a.clock.now = () => { replacementCalls++; return 0; };
        a.clock.clear = () => { replacementCalls++; };
        a.clock.timer = () => { replacementCalls++; throw new Error('replacement timer must not run'); };
        if (stage === 'headers') a.advance(elapsed);
        const response = new Response(null);
        response.text = async () => {
          if (stage === 'body') a.advance(elapsed);
          return text;
        };
        headers.resolve(response);
        await settled;
        assert.equal(replacementCalls, 0);
        assert.equal(a.sent(), 1);
        assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, elapsed < 12_000 ? 1 : 0);
        assert.equal(a.client.snapshot().lastCommand!.timeoutMs, 12_000);
        assert.equal(a.client.snapshot().unusable, elapsed >= 12_000);
        assert.equal(a.signal()!.aborted, elapsed >= 12_000);
        assert.equal(a.timers.size, 0);
        assert.equal(a.root.remainingMs, 120_000 - elapsed);
        assert.equal(a.phase.remainingMs, 75_000 - elapsed);
      } finally { JSON.parse = parse; }
    }
  }
  for (const elapsed of [11_999, 12_000, 12_001]) {
    const a = await policyClient(async () => { a.advance(elapsed); return Response.json({ value: null }); });
    const action = a.client.click('add', 12_000, a.policy);
    if (elapsed < 12_000) await action;
    else await assert.rejects(action, /APPIUM_TIMEOUT/u);
    assert.equal(a.sent(), 1);
    assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, elapsed < 12_000 ? 1 : 0);
    assert.equal(a.client.snapshot().lastCommand!.timing!.outcome, elapsed < 12_000 ? 'success' : 'timeout');
    assert.equal(a.client.snapshot().unusable, elapsed >= 12_000);
    assert.equal(a.signal()?.aborted, elapsed >= 12_000);
    assert.equal(a.timers.size, 0);
  }
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 scoped final Add rejects late body before delayed timer', async () => {
  for (const malformed of [false, true]) {
    const a = await policyClient(async () => {
      const response = new Response(null, { status: malformed ? 500 : 200 });
      response.text = async () => { a.advance(12_000); return malformed ? 'invalid JSON' : '{"value":null}'; };
      return response;
    });
    await assert.rejects(() => a.client.click('add', 12_000, a.policy), /APPIUM_TIMEOUT/u);
    const saved = structuredClone(a.client.snapshot());
    assert.equal(saved.firstFatal?.code, 'APPIUM_TIMEOUT');
    assert.equal(saved.lastCommand!.timing!.completed, false);
    assert.equal(saved.lastCommand!.timing!.parseStartMs, undefined);
    await assert.rejects(() => a.client.settings(), /APPIUM_SESSION_UNUSABLE/u);
    assert.deepEqual(a.client.snapshot(), saved);
    assert.equal(a.sent(), 1);
  }
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 scoped final Add rejects late parse and invalid acknowledgement', async () => {
  for (const text of ['{"value":null}', '{"value":{}}', '{}', 'null', 'bad JSON', '{"value":{"error":"stale element reference"}}']) {
    for (const late of [false, true]) {
      const a = await policyClient(async () => new Response(text));
      const parse = JSON.parse;
      JSON.parse = ((input: string, reviver?: Parameters<typeof JSON.parse>[1]) => {
        if (input === text && late) a.advance(12_000);
        return parse(input, reviver);
      }) as typeof JSON.parse;
      try {
        const action = a.client.click('add', 12_000, a.policy);
        if (!late && text === '{"value":null}') await action;
        else await assert.rejects(action, late ? /APPIUM_TIMEOUT/u : /APPIUM_HTTP|APPIUM_COMMAND/u);
      } finally { JSON.parse = parse; }
      assert.equal(a.sent(), 1);
      assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, !late && text === '{"value":null}' ? 1 : 0);
      assert.equal(a.client.snapshot().unusable, late || text !== '{"value":null}');
      assert.equal(Object.isFrozen(a.client.snapshot().lastCommand!.timing), true);
      assert.equal(a.timers.size, 0);
    }
  }
  const closeBody = deferred<string>();
  const closeStarted = deferred<void>();
  const addHeaders = deferred<Response>();
  const a = await policyClient(async init => {
    if (init?.method !== 'DELETE') return addHeaders.promise;
    const response = new Response(null);
    response.text = () => { closeStarted.resolve(); return closeBody.promise; };
    return response;
  });
  const cause = new TypeError('original close body interruption');
  let original: unknown;
  const closing = a.client.close();
  const rejectedClose = assert.rejects(closing, error => { original = error; return error instanceof PolicyError && error.cause === cause; });
  await closeStarted.promise;
  const action = a.client.click('add', 12_000, a.policy);
  const rejectedAdd = assert.rejects(action, /APPIUM_TIMEOUT/u);
  closeBody.reject(cause);
  await rejectedClose;
  assert.ok(original instanceof PolicyError);
  assert.equal(original.code, 'APPIUM_INTERRUPTED');
  const fatal = a.client.snapshot().firstFatal;
  assert.ok(fatal);
  assert.equal(fatal.code, original.code);
  assert.equal(fatal.method, 'DELETE');
  assert.equal(fatal.path, original.path);
  assert.equal(fatal.detail, original.message);
  const savedFatal = structuredClone(fatal);
  a.advance(12_000);
  addHeaders.resolve(new Response('malformed'));
  await rejectedAdd;
  assert.equal(a.client.snapshot().firstFatal, fatal);
  assert.deepEqual(fatal, savedFatal);
  assert.equal(original.cause, cause);
  assert.equal(a.wire.filter(request => request.url.endsWith('/click')).length, 1);
  assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, 0);
  assert.equal(a.timers.size, 0);
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 dispatch gap preserves full allocation or remains unsent', async () => {
  for (const provider of ['enforcement', 'budget'] as const) {
    for (const mutation of ['create', 'context'] as const) {
      const a = await policyClient(async () => Response.json({ value: null }));
      let armed = false;
      let attempts = 0;
      const followups: Promise<void>[] = [];
      const attempt = () => {
        if (!armed) return;
        armed = false;
        attempts++;
        followups.push(assert.rejects(mutation === 'create'
          ? a.client.create({ capabilities: {} }) : a.client.switchContext('WEBVIEW-overlap'), /APPIUM_COMMAND_NOT_ADMITTED/u));
      };
      const budget = new PhaseBudget('same-clock-origin', {
        timeoutMs: 75_000,
        now: () => { if (provider === 'budget') attempt(); return a.clock.now(); },
      });
      if (provider === 'enforcement') a.observeNow(attempt);
      const retained = a.client.nativeRequestPolicy(budget);
      const policy = { ...retained, finalAction: true, beforeDispatch: () => { retained.beforeDispatch(); armed = true; } };
      await assert.rejects(() => a.client.click('add', 12_000, policy), /APPIUM_COMMAND_NOT_ADMITTED/u);
      await Promise.all(followups);
      assert.equal(attempts, 1);
      assert.equal(followups.length, 1);
      assert.equal(a.wire.length, 1);
      assert.equal(a.wire[0].method, 'POST');
      assert.equal(a.sent(), 0);
      assert.equal(a.client.snapshot().lastCommand!.timing!.sent, false);
      assert.equal(a.client.snapshot().lastCommand!.timing!.outcome, 'not-admitted');
      assert.equal(Object.isFrozen(a.client.snapshot().lastCommand!.timing), true);
      assert.equal(a.client.snapshot().lastCommand!.timing!.sessionGeneration, 1);
      assert.equal(a.client.snapshot().selectedContext, 'NATIVE_APP');
      assert.equal(a.client.snapshot().unusable, false);
      assert.equal(a.client.snapshot().firstFatal, undefined);
      assert.equal(a.timers.size, 0);
      await a.client.settings(2_000);
      assert.equal(a.client.snapshot().lastCommand!.timing!.sessionGeneration, 1);
      assert.equal(a.client.snapshot().lastCommand!.path, '/session/policy/appium/settings');
      assert.equal(a.wire.filter(request => request.url.endsWith('/click')).length, 0);
      assert.equal(a.wire.length, 2);
    }
  }
  {
    let reads = 0;
    const a = await policyClient(async () => ++reads === 1
      ? Response.json({ value: { error: 'stale element reference' } }, { status: 404 })
      : Response.json({ value: null }));
    await assert.rejects(() => a.client.command('/elements', 'GET', undefined, 5_000, a.client.nativeRequestPolicy(a.phase)), error => {
      assert.ok(error instanceof PolicyError);
      assert.equal(error.code, 'APPIUM_COMMAND');
      assert.equal(error.status, 404);
      return true;
    });
    assert.equal(a.client.snapshot().unusable, false);
    assert.equal(a.client.snapshot().firstFatal, undefined);
    await a.client.click('add', 12_000, a.policy);
    assert.equal(a.sent(), 2);
    assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, 1);
  }
  for (const limiting of ['root', 'operation', 'confirmation'] as const) {
    for (const remaining of [12_000, 11_999]) {
      const a = await policyClient(async () => Response.json({ value: null }));
      const root = new PhaseBudget('root', { timeoutMs: limiting === 'root' ? remaining : 120_000, now: a.clock.now });
      const operation = root.phaseView('operation', limiting === 'operation' ? remaining : 80_000);
      const confirmation = operation.phaseView('confirmation', limiting === 'confirmation' ? remaining : 75_000);
      a.client.setBudget(root);
      const policy = Object.freeze({ ...a.client.nativeRequestPolicy(confirmation), finalAction: true });
      if (remaining === 12_000) await a.client.click('add', 12_000, policy);
      else await assert.rejects(() => a.client.click('add', 12_000, policy), /APPIUM_COMMAND_NOT_ADMITTED/u);
      assert.equal(a.sent(), remaining === 12_000 ? 1 : 0);
      assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, remaining === 12_000 ? 1 : 0);
      assert.equal(a.client.snapshot().lastCommand!.timeoutMs, 12_000);
    }
  }
  const a = await policyClient(async () => Response.json({ value: null }));
  const policy = Object.freeze({ ...a.policy, budget: a.root.phaseView('exact', 12_000), beforeDispatch: () => { a.policy.beforeDispatch(); a.advance(1); } });
  await assert.rejects(() => a.client.click('add', 12_000, policy), /APPIUM_COMMAND_NOT_ADMITTED/u);
  assert.equal(a.sent(), 0);
  assert.equal(a.client.snapshot().lastCommand!.timing!.sent, false);
  assert.equal(a.timers.size, 0);
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 remote work surviving abort stays unknown and quarantined', async () => {
  for (const expiration of ['timer', 'late-headers'] as const) {
    const headers = deferred<Response>();
    const followups: Promise<void>[] = [];
    let aborts = 0;
    let fatalAtAbort: ReturnType<PolicyClient['snapshot']>['firstFatal'];
    const a = await policyClient(init => {
      init!.signal!.addEventListener('abort', () => {
        aborts++;
        fatalAtAbort = a.client.snapshot().firstFatal;
        assert.ok(fatalAtAbort);
        assert.equal(a.client.snapshot().unusable, true);
        followups.push(assert.rejects(a.client.updateSettings({ waitForIdleTimeout: 0 }), /APPIUM_SESSION_UNUSABLE/u));
        followups.push(assert.rejects(a.client.click('add', 12_000, a.policy), /APPIUM_SESSION_UNUSABLE/u));
      }, { once: true });
      return headers.promise;
    });
    const action = a.client.click('add', 12_000, a.policy);
    let original: unknown;
    const rejected = assert.rejects(action, error => { original = error; return error instanceof PolicyError && error.code === 'APPIUM_TIMEOUT'; });
    if (expiration === 'timer') a.expire(12_000);
    else { a.advance(12_000); headers.resolve(Response.json({ value: null })); }
    await rejected;
    await Promise.all(followups);
    assert.equal(aborts, 1);
    assert.equal(followups.length, 2);
    assert.ok(original instanceof PolicyError);
    assert.ok(fatalAtAbort);
    assert.equal(a.client.snapshot().firstFatal, fatalAtAbort);
    assert.equal(fatalAtAbort.code, original.code);
    assert.equal(fatalAtAbort.path, original.path);
    assert.equal(fatalAtAbort.detail, original.message);
    assert.equal(a.signal()!.reason, original.cause);
    const saved = structuredClone(a.client.snapshot());
    const receipt = a.client.snapshot().lastCommand!.timing!;
    const settlement = (a.client as any).previousSettlementMs;
    assert.equal(Object.isFrozen(receipt), true);
    headers.resolve(Response.json({ value: null }));
    await Promise.resolve();
    await Promise.resolve();
    await assert.rejects(action, error => error === original);
    assert.deepEqual(a.client.snapshot(), saved);
    assert.equal(a.client.snapshot().lastCommand!.timing, receipt);
    assert.equal((a.client as any).previousSettlementMs, settlement);
    assert.equal(a.sent(), 1);
    assert.equal(a.wire.length, 2);
    assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, 0);
    assert.equal(a.timers.size, 0);
  }
  for (const mode of ['inline', 'throw', 'inline-throw'] as const) {
    const headers = deferred<Response>();
    const a = await policyClient(() => headers.promise);
    a.timerMode(mode);
    let original: unknown;
    await assert.rejects(a.client.click('add', 12_000, a.policy), error => {
      original = error;
      assert.ok(error instanceof PolicyError);
      assert.equal(error.code, mode === 'throw' ? 'APPIUM_HTTP' : 'APPIUM_TIMEOUT');
      return true;
    });
    assert.ok(original instanceof PolicyError);
    const fatal = a.client.snapshot().firstFatal;
    assert.ok(fatal);
    assert.equal(fatal.code, original.code);
    assert.equal(fatal.detail, original.message);
    const saved = structuredClone(a.client.snapshot());
    headers.reject(new TypeError('losing transport rejection'));
    await Promise.resolve();
    await Promise.resolve();
    assert.deepEqual(a.client.snapshot(), saved);
    assert.equal(a.client.snapshot().firstFatal, fatal);
    assert.equal(a.sent(), 1);
    assert.equal(a.client.snapshot().commands.filter(command => command.path.endsWith('/click') && command.timing!.outcome === 'success').length, 0);
    assert.equal(a.signal()!.aborted, true);
    assert.equal(a.timers.size, 0);
  }
  const native = deferred<Response>();
  let nativeCompleted = false;
  const nativeSettlement = native.promise.then(() => { nativeCompleted = true; });
  const a = await policyClient(() => native.promise);
  const action = a.client.click('add', 12_000, a.policy);
  const rejected = assert.rejects(action, /APPIUM_TIMEOUT/u);
  a.expire(12_000);
  await rejected;
  assert.equal(a.signal()?.aborted, true);
  assert.equal(nativeCompleted, false);
  const snapshot = structuredClone(a.client.snapshot());
  native.resolve(Response.json({ value: null }));
  await nativeSettlement;
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(nativeCompleted, true);
  assert.deepEqual(a.client.snapshot(), snapshot);
  for (const followup of [() => a.client.settings(), () => a.client.activeAppInfo(), () => a.client.click('add'), () => a.client.create({ capabilities: {} })]) {
    await assert.rejects(followup, /APPIUM_SESSION_UNUSABLE/u);
  }
  assert.equal(a.sent(), 1);
  assert.deepEqual(a.client.snapshot(), snapshot);
  await a.client.close();
  assert.equal(a.sent(), 2);
  assert.equal(a.client.snapshot().unusable, true);
  assert.deepEqual(a.client.snapshot().firstFatal, snapshot.firstFatal);
  await assert.rejects(() => a.client.create({ capabilities: {} }), /APPIUM_SESSION_UNUSABLE/u);
  assert.equal(a.sent(), 2);
}]);

webdriverInterruptionTests.push(['Appium request timing SM57 unscoped defaults and diagnostic annotations stay observational', async () => {
  const a = await policyClient(async () => { a.advance(13_000); return Response.json({ value: null }); });
  const diagnostic = a.root.phaseView('diagnostic', 8_000);
  const restore = a.client.setRequestTimingScope('ios-attachment-pre-attachment-native', diagnostic);
  await a.client.command('/alert/text', 'GET', undefined, 5_000);
  restore();
  assert.equal(a.client.snapshot().lastCommand!.timeoutMs, 5_000);
  assert.equal(a.client.snapshot().lastCommand!.timing!.phaseRemainingAtSettlementMs, 0);
  assert.equal(a.client.snapshot().lastCommand!.timing!.outcome, 'success');
  await a.client.command('/unscoped', 'GET');
  assert.equal(a.client.snapshot().lastCommand!.timeoutMs, 30_000);
  assert.equal(a.client.snapshot().firstFatal, undefined);
  assert.equal((a.client as any).budget, a.root);
}]);

for (const reply of ['success', 'no-alert', 'malformed'] as const) {
  webdriverInterruptionTests.push([`Appium request timing separates response, body and local settlement for ${reply}`, async () => {
    const { AppiumClient, WebDriverError, isFatalDriverError } = await clientSource();
    const responseReady = deferred<Response>();
    const bodyReady = deferred<string>();
    const bodyStarted = deferred<void>();
    let ticks = 0;
    let phaseNow = 0;
    const phase = new PhaseBudget('original-phase', { timeoutMs: 30_000, now: () => phaseNow });
    let sends = 0;
    let reads = 0;
    let wire: { input: string; init?: RequestInit } | undefined;
    const client = new AppiumClient('http://protocol.invalid', 5_000, (input, init) => {
      sends++;
      if (sends === 1) return Promise.resolve(Response.json({ value: {}, sessionId: 'secret-session' }));
      wire = { input: String(input), init };
      return responseReady.promise;
    }, () => ++ticks);
    await client.create({ capabilities: {} });
    const previous = client.snapshot().lastCommand!.timing!;
    client.setBudget(phase);
    const restore = client.setRequestTimingScope('ios-attachment-pre-attachment-native', phase);
    const action = client.command('/alert/text', 'GET');
    const settled = reply === 'success' ? action : assert.rejects(action, (error: unknown) => {
      assert.ok(error instanceof WebDriverError);
      assert.equal(error.code, reply === 'no-alert' ? 'APPIUM_COMMAND' : 'APPIUM_HTTP');
      assert.equal(error.status, reply === 'no-alert' ? 404 : 200);
      assert.equal(isFatalDriverError(error), false);
      return true;
    });
    const response = new Response(null, { status: reply === 'no-alert' ? 404 : 200 });
    response.text = () => { reads++; bodyStarted.resolve(); return bodyReady.promise; };
    phaseNow = 1_000;
    ticks += 100;
    responseReady.resolve(response);
    await bodyStarted.promise;
    assert.equal(client.snapshot().lastCommand, client.snapshot().commands[0], 'no unfinished receipt is published');
    phaseNow = 4_000;
    ticks += 200;
    bodyReady.resolve(reply === 'malformed' ? 'not JSON https://secret.invalid/?token=secret'
      : JSON.stringify({ value: reply === 'no-alert' ? { error: 'no such alert', message: 'No alert https://secret.invalid/?token=secret' } : 'secret-body' }));
    await settled;
    restore();
    const command = client.snapshot().lastCommand!;
    const timing = command.timing!;
    assertTimingOrder(timing);
    assert.equal(sends, 2);
    assert.equal(reads, 1);
    assert.equal(wire?.input, 'http://protocol.invalid/session/secret-session/alert/text');
    assert.deepEqual(Object.keys(wire!.init!).sort(), ['body', 'headers', 'method', 'signal']);
    assert.equal(wire?.init?.method, 'GET');
    assert.equal(wire?.init?.headers, undefined);
    assert.equal(wire?.init?.body, undefined);
    assert.equal(wire?.init?.signal?.aborted, false);
    assert.equal(command.timeoutMs, 5_000);
    assert.equal(timing.attemptId, 2);
    assert.equal(timing.dispatchedOrdinal, 2);
    assert.equal(timing.sessionGeneration, 1);
    assert.equal(timing.operation, 'alert');
    assert.equal(timing.scope, 'ios-attachment-pre-attachment-native');
    assert.equal(timing.outcome, reply === 'success' ? 'success' : reply === 'no-alert' ? 'command-error' : 'invalid-json');
    assert.equal(timing.admitted && timing.sent && timing.completed, true);
    assert.equal(timing.phaseRemainingAtEntryMs, 30_000);
    assert.equal(timing.phaseRemainingAtDispatchMs, 30_000);
    assert.equal(timing.phaseRemainingAtSettlementMs, 26_000);
    assert.equal(timing.priorSettlementToEntryMs, timing.entryMs - previous.settlementMs!);
    assert.equal(timing.priorSettlementToDispatchMs, timing.dispatchMs! - previous.settlementMs!);
    assert.ok(timing.responseAvailableMs! - timing.dispatchMs! >= 100);
    assert.ok(timing.bodyReadCompleteMs! - timing.bodyReadStartMs! >= 200);
    assert.doesNotMatch(JSON.stringify(timing), /secret|protocol|https?:|session\/|no such alert/u);
    assert.ok(JSON.stringify(timing).length < 1_500);
    assert.equal(client.snapshot().unusable, false);
    assert.equal(client.snapshot().firstFatal, undefined);
  }]);
}

webdriverInterruptionTests.push(['Appium request timing refusal is unsent and history remains bounded across session generations', async () => {
  const { AppiumClient } = await clientSource();
  let ticks = 0;
  let sends = 0;
  let phaseNow = 0;
  const client = new AppiumClient('http://protocol.invalid', 5_000, async () => {
    sends++;
    return Response.json({ value: {}, sessionId: 'secret-session' });
  }, () => ++ticks);
  await client.create({ capabilities: {} });
  const phase = new PhaseBudget('original-phase', { timeoutMs: 30_000, now: () => phaseNow });
  client.setBudget(phase);
  phaseNow = 29_751;
  await assert.rejects(() => client.switchContext('WEBVIEW-secret'), /APPIUM_COMMAND_NOT_ADMITTED/u);
  const refusal = client.snapshot().lastCommand!.timing!;
  assertTimingOrder(refusal);
  assert.equal(sends, 1);
  assert.equal(refusal.admitted || refusal.sent || refusal.completed, false);
  assert.equal(refusal.dispatchMs, undefined);
  assert.equal(refusal.dispatchedOrdinal, undefined);
  assert.equal(refusal.phaseRemainingAtEntryMs, 249);
  assert.equal(refusal.phaseRemainingAtDispatchMs, null);
  assert.equal(refusal.phaseRemainingAtSettlementMs, 249);
  assert.equal(refusal.outcome, 'not-admitted');
  assert.equal(client.snapshot().unusable, false);
  assert.equal(client.snapshot().firstFatal, undefined);
  client.setBudget(undefined);
  await client.close();
  await client.create({ capabilities: {} });
  assert.equal(client.snapshot().lastCommand!.timing!.sessionGeneration, 2);
  for (let index = 0; index < 105; index++) await client.command('/untrusted-secret-path', 'GET');
  assert.equal(client.snapshot().commands.length, 50);
  assert.equal((client as any).history.length, 100);
  assert.equal(client.snapshot().lastCommand!.timing!.operation, 'other');
  assert.equal(client.snapshot().lastCommand!.timing!.attemptId, sends + 1);
  assert.equal(client.snapshot().lastCommand!.timing!.dispatchedOrdinal, sends);
  assert.equal(refusal.outcome, 'not-admitted');
  assert.doesNotMatch(JSON.stringify(client.snapshot().commands.map(item => item.timing)), /secret|protocol/u);
}]);

for (const failure of ['interrupted', 'timeout'] as const) {
  webdriverInterruptionTests.push([`Appium request timing ${failure} body cannot mutate after settlement`, async () => {
    const { AppiumClient } = await clientSource();
    const body = deferred<string>();
    const started = deferred<void>();
    let ticks = 0;
    let sends = 0;
    let reads = 0;
    const client = new AppiumClient('http://protocol.invalid', 5_000, async () => {
      sends++;
      if (sends === 1) return Response.json({ value: {}, sessionId: 'secret-session' });
      const response = new Response(null);
      response.text = () => { reads++; started.resolve(); return body.promise; };
      return response;
    }, () => ++ticks);
    await client.create({ capabilities: {} });
    const action = client.pageSource(failure === 'timeout' ? 10 : 5_000);
    const rejected = assert.rejects(action, failure === 'timeout' ? /APPIUM_TIMEOUT/u : /APPIUM_INTERRUPTED/u);
    await started.promise;
    if (failure === 'interrupted') body.reject(new TypeError('secret body failure'));
    await rejected;
    const snapshot = structuredClone(client.snapshot());
    const timing = client.snapshot().lastCommand!.timing!;
    assertTimingOrder(timing);
    assert.equal(timing.outcome, failure);
    assert.equal(timing.status, 200);
    assert.equal(timing.admitted && timing.sent, true);
    assert.equal(timing.completed, false);
    assert.equal(timing.bodyReadCompleteMs, undefined);
    assert.equal(timing.parseStartMs, undefined);
    assert.equal(sends, 2);
    assert.equal(reads, 1);
    body.resolve('{"value":"late-secret"}');
    await Promise.resolve();
    await Promise.resolve();
    assert.deepEqual(client.snapshot(), snapshot);
    await assert.rejects(() => client.switchContext('WEBVIEW-late'), /APPIUM_SESSION_UNUSABLE/u);
    assert.deepEqual(client.snapshot(), snapshot);
    assert.equal(sends, 2);
  }]);
}

for (const kind of ['TypeError', 'AbortError'] as const) {
  for (const operation of ['source', 'lookup', 'create', 'close-404', 'chrome-details', 'chrome-handles'] as const) {
    webdriverInterruptionTests.push([`Appium ${kind} body interruption quarantines ${operation} without inventing a timeout`, async () => {
      const clientModule = await clientSource();
      const cause = kind === 'TypeError' ? new TypeError('hypothetical response connection reset') : new DOMException('hypothetical interrupted body', 'AbortError');
      let requests = 0;
      let signal: AbortSignal | null | undefined;
      const client = new clientModule.AppiumClient('http://protocol.invalid', 5000, async (_input, init) => {
        requests++;
        if (requests === 1 && operation !== 'create') return Response.json({ value: {}, sessionId: 'protocol' });
        signal = init?.signal;
        return new Response(new ReadableStream({ start(controller) { controller.error(cause); } }), { status: operation === 'close-404' ? 404 : 200 });
      });
      if (operation !== 'create') await client.create({ capabilities: {} });
      const action = () => operation === 'create' ? client.create({ capabilities: {} })
        : operation === 'close-404' ? client.close()
        : operation === 'lookup' ? client.findAny([{ using: 'accessibility id', value: 'Add' }])
        : operation === 'chrome-details' ? client.contextMetadataRaw()
        : operation === 'chrome-handles' ? client.windowHandles()
        : client.pageSource();
      let failure: unknown;
      await assert.rejects(action, (error: unknown) => { failure = error; return error instanceof clientModule.WebDriverError && error.cause === cause; });
      assert.equal(client.snapshot().unusable, true);
      assert.equal(clientModule.isFatalDriverError(failure), true);
      assert.equal((failure as WebDriverError).timedOut, false);
      assert.equal((failure as WebDriverError).code, 'APPIUM_INTERRUPTED');
      assert.equal(signal?.aborted, true);
      const fatal = client.snapshot().firstFatal;
      assert.equal(fatal?.code, 'APPIUM_INTERRUPTED');
      const count = requests;
      await assert.rejects(() => client.activeAppInfo(), /APPIUM_SESSION_UNUSABLE/u);
      await assert.rejects(() => client.create({ capabilities: {} }), /APPIUM_SESSION_UNUSABLE/u);
      assert.equal(requests, count);
      assert.deepEqual(client.snapshot().firstFatal, fatal);
    }]);
  }
}

for (const operation of ['chrome-details', 'chrome-handles'] as const) {
  webdriverInterruptionTests.push([`Appium timeout quarantines ${operation}`, async () => {
    const { AppiumClient } = await clientSource();
    let requests = 0;
    const client = new AppiumClient('http://protocol.invalid', 200, async (_input, init) => {
      requests++;
      if (requests === 1) return Response.json({ value: {}, sessionId: 'protocol' });
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
      });
    });
    await client.create({ capabilities: {} });
    await assert.rejects(() => operation === 'chrome-details' ? client.contextMetadataRaw() : client.windowHandles());
    const fatal = client.snapshot().firstFatal;
    assert.ok(fatal);
    assert.equal(client.snapshot().unusable, true);
    const count = requests;
    await assert.rejects(() => client.windowHandles(), /APPIUM_SESSION_UNUSABLE/u);
    await assert.rejects(() => client.contextMetadataRaw(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests, count);
    assert.deepEqual(client.snapshot().firstFatal, fatal);
  }]);
}

webdriverInterruptionTests.push(['Appium completed protocol and malformed JSON replies are not body interruptions', async () => {
  const { AppiumClient } = await clientSource();
  for (const malformed of [false, true]) {
    let requests = 0;
    const client = new AppiumClient('http://protocol.invalid', 5000, async () => {
      requests++;
      if (requests === 1) return Response.json({ value: {}, sessionId: 'protocol' });
      if (requests > 2) return Response.json({ value: { bundleId: 'com.apple.webapp', pid: 42 } });
      return malformed ? new Response('not JSON') : Response.json({ value: { error: 'invalid element state' } }, { status: 400 });
    });
    await client.create({ capabilities: {} });
    await assert.rejects(() => client.pageSource(), malformed ? /APPIUM_HTTP/u : /APPIUM_COMMAND/u);
    assert.equal(client.snapshot().unusable, false);
    assert.equal(client.snapshot().firstFatal, undefined);
    await client.activeAppInfo();
    assert.equal(requests, 3);
  }
}]);

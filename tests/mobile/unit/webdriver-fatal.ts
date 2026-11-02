import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import { AppiumClient, type RequestEnforcementClock, type WebDriverRequestPolicy } from '../support/webdriver';
import { PhaseBudget, PhaseBudgetError } from '../support/budget';
import { IOSPlatform } from '../platforms/ios';
import { AndroidPlatform } from '../platforms/android';

type Test = [string, () => Promise<void>];

async function clientSource(): Promise<typeof import('../support/webdriver')> {
  const source = process.env.WEBDRIVER_TEST_SOURCE;
  return import(source ? pathToFileURL(source).href : new URL('../support/webdriver.ts', import.meta.url).href);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(yes => { resolve = yes; });
  return { promise, resolve };
}

function manualClock() {
  const timers = new Map<ReturnType<typeof setTimeout>, () => void>();
  let ordinal = 0;
  const clock: RequestEnforcementClock = {
    now: () => 0,
    timer: callback => { const id = ++ordinal as unknown as ReturnType<typeof setTimeout>; timers.set(id, callback); return id; },
    clear: id => { timers.delete(id); },
  };
  return { clock, expire: () => { for (const callback of [...timers.values()]) callback(); } };
}

function ownership(client: AppiumClient) {
  const internal = client as any;
  return { id: internal.sessionId, owner: internal.sessionOwner, generation: internal.sessionGeneration };
}

async function assertQuarantined(client: AppiumClient, sent: () => number, policy: WebDriverRequestPolicy) {
  const snapshot = structuredClone(client.snapshot());
  const fatal = client.snapshot().firstFatal;
  const identity = ownership(client);
  const count = sent();
  assert.ok(fatal, 'an observed fatal must be retained');
  assert.equal(client.snapshot().unusable, true);
  for (let repeat = 0; repeat < 2; repeat++) {
    for (const followup of [
      () => client.close(),
      () => client.create({ capabilities: {} }),
      () => client.command('/status', 'GET'),
      () => client.command('/arbitrary/status', 'GET'),
      () => client.command('/session', 'POST'),
      () => client.command('/arbitrary', 'GET'),
      () => client.find({ using: 'accessibility id', value: 'synthetic' }),
      () => client.findAny([]),
      () => client.findAnyOnce([]),
      () => client.command('/alert/text', 'GET', undefined, 100, policy),
      // Exercise the shared boundary independently of session/budget flags.
      () => (client as any).request('/status', 'GET', undefined, 100, false, false),
      () => (client as any).request('/session', 'POST', {}, 100, false, false),
    ]) {
      await assert.rejects(followup, /APPIUM_SESSION_UNUSABLE/u, 'every post-fatal operation must reject, including close');
      assert.equal(sent(), count, 'quarantine must prevent every further transport dispatch');
      assert.equal(client.snapshot().firstFatal, fatal);
      assert.equal(ownership(client).owner, identity.owner);
      assert.deepEqual(ownership(client), identity);
      assert.deepEqual(client.snapshot(), snapshot, 'blocked operations must not claim remote closure or alter the fatal receipt');
    }
  }
}

export const webdriverFatalTests: Test[] = [];

for (const kind of ['timeout', 'status-timeout', 'body-interruption', 'create-timeout', 'create-body-interruption', 'unexpected-delete', 'delete-invalid-json', 'delete-error-200', 'delete-wrong-404', 'delete-incomplete-404', 'scoped-final-action'] as const) {
  webdriverFatalTests.push([`Appium strict quarantine ${kind} rejects all post-fatal dispatches`, async () => {
    const { AppiumClient: Client } = await clientSource();
    let sent = 0;
    let failing = false;
    const client = new Client('http://protocol.invalid', 1_000, async () => {
      sent++;
      if (!failing) return Response.json({ value: {}, sessionId: 'original' });
      if (kind.includes('timeout')) throw new DOMException('synthetic timeout', 'TimeoutError');
      if (kind.includes('interruption') || kind === 'delete-incomplete-404') {
        const response = new Response(null, { status: kind === 'delete-incomplete-404' ? 404 : 200 });
        response.text = async () => { throw new TypeError('synthetic body interruption'); };
        return response;
      }
      if (kind === 'delete-invalid-json') return new Response('not JSON');
      return Response.json({ value: { error: kind === 'delete-wrong-404' ? 'unknown command' : 'unknown error' } },
        { status: kind === 'delete-error-200' ? 200 : kind === 'delete-wrong-404' ? 404 : 500 });
    });
    if (!kind.startsWith('create-')) await client.create({ capabilities: {} });
    const budget = new PhaseBudget('strict-policy', { timeoutMs: 30_000 });
    const policy = { budget, beforeDispatch: () => {} };
    failing = true;
    const action = kind.startsWith('create-') ? () => client.create({ capabilities: {} })
      : kind.startsWith('delete-') || kind === 'unexpected-delete' ? () => client.close()
      : kind === 'scoped-final-action' ? () => client.click('synthetic', 100, { ...client.nativeRequestPolicy(budget), finalAction: true })
      : kind === 'status-timeout' ? () => client.command('/status', 'GET') : () => client.contextMetadataRaw();
    await assert.rejects(action, /APPIUM_(?:TIMEOUT|INTERRUPTED|COMMAND|HTTP)/u);
    assert.equal(client.snapshot().sessionId, kind.startsWith('create-') ? '' : '[active]');
    await assertQuarantined(client, () => sent, policy);
    assert.equal(Object.isFrozen(client.snapshot().firstFatal), true);
  }]);
}

for (const preflight of ['create-budget', 'command-budget', 'find', 'findAny', 'findAnyOnce', 'create-guard', 'command-guard', 'policy-callback'] as const) {
  webdriverFatalTests.push([`Appium strict quarantine records fatal preflight ${preflight}`, async () => {
    const { AppiumClient: Client } = await clientSource();
    let sent = 0;
    let now = 0;
    const budget = new PhaseBudget('preflight', { timeoutMs: 30_000, now: () => now });
    const client = new Client('http://protocol.invalid', 1_000, async () => { sent++; return Response.json({ value: {}, sessionId: 'original' }); });
    await client.create({ capabilities: {} });
    const retained = client.nativeRequestPolicy(budget);
    now = 30_001;
    const fatal = new PhaseBudgetError('PHASE_BUDGET_EXHAUSTED', 'preflight', 'callback', now, 0);
    if (preflight.endsWith('guard')) client.setCommandGuard({ before: () => { throw fatal; }, failure: () => {} });
    if (!preflight.startsWith('create-')) client.setBudget(budget);
    const action = preflight === 'create-budget' ? () => client.create({ capabilities: {}, budget })
      : preflight === 'create-guard' ? () => client.create({ capabilities: {} })
      : preflight === 'policy-callback' ? () => { client.setBudget(undefined); return client.command('/alert/text', 'GET', undefined, 100, { ...retained, beforeDispatch: () => { throw fatal; } }); }
      : preflight === 'find' ? () => client.find({ using: 'accessibility id', value: 'synthetic' })
      : preflight === 'findAny' ? () => client.findAny([{ using: 'accessibility id', value: 'synthetic' }])
      : preflight === 'findAnyOnce' ? () => client.findAnyOnce([{ using: 'accessibility id', value: 'synthetic' }])
      : () => client.contexts();
    await assert.rejects(action, /PHASE_BUDGET_EXHAUSTED/u);
    assert.equal(sent, 1);
    await assertQuarantined(client, () => sent, retained);
  }]);
}

for (const operation of ['create', 'close', 'context', 'window'] as const) {
  for (const stage of ['headers', 'body'] as const) {
    webdriverFatalTests.push([`Appium strict quarantine overlapping late ${operation} ${stage} cannot change ownership`, async () => {
      const { AppiumClient: Client } = await clientSource();
      const headers = deferred<Response>();
      const body = deferred<string>();
      const reading = deferred<void>();
      let sent = 0;
      const client = new Client('http://protocol.invalid', 30_000, async () => {
        sent++;
        if (sent === 1) return Response.json({ value: {}, sessionId: 'original' });
        if (sent === 2) return headers.promise;
        throw new DOMException('synthetic concurrent fatal', 'TimeoutError');
      });
      await client.create({ capabilities: {} });
      const policy = client.nativeRequestPolicy(new PhaseBudget('late', { timeoutMs: 30_000 }));
      const late = operation === 'create' ? client.create({ capabilities: {} }) : operation === 'close' ? client.close()
        : operation === 'context' ? client.switchContext('WEBVIEW-late') : client.switchWindow('late-window');
      const rejected = assert.rejects(late, /APPIUM_SESSION_UNUSABLE/u);
      const response = new Response(null);
      response.text = () => { reading.resolve(); return body.promise; };
      if (stage === 'body') { headers.resolve(response); await reading.promise; }
      await assert.rejects(() => client.contexts(), /APPIUM_TIMEOUT/u);
      const identity = ownership(client);
      const fatal = client.snapshot().firstFatal;
      const receipt = client.snapshot().lastCommand!.timing!;
      const saved = structuredClone(client.snapshot());
      headers.resolve(response);
      body.resolve(JSON.stringify({ value: null, sessionId: 'replacement' }));
      await rejected;
      assert.deepEqual(ownership(client), identity);
      assert.equal(ownership(client).owner, identity.owner);
      assert.equal(client.snapshot().firstFatal, fatal);
      assert.deepEqual(fatal, saved.firstFatal);
      assert.equal(client.snapshot().selectedContext, saved.selectedContext);
      assert.equal(client.snapshot().selectedWindow, saved.selectedWindow);
      assert.deepEqual(client.snapshot(), saved, 'overlapping late success must not alter the fatal snapshot');
      assert.equal(Object.isFrozen(receipt), true);
      assert.equal(receipt.outcome, 'timeout');
      await assertQuarantined(client, () => sent, policy);
    }]);
  }
}

for (const stage of ['headers', 'body'] as const) {
  webdriverFatalTests.push([`Appium strict quarantine timeout late ${stage} retains frozen settlement`, async () => {
    const { AppiumClient: Client } = await clientSource();
    const headers = deferred<Response>();
    const body = deferred<string>();
    const reading = deferred<void>();
    const clock = manualClock();
    let sent = 0;
    const client = new Client('http://protocol.invalid', 1_000, async () => {
      sent++;
      return sent === 1 ? Response.json({ value: {}, sessionId: 'original' }) : headers.promise;
    }, undefined, clock.clock);
    await client.create({ capabilities: {} });
    const policy = client.nativeRequestPolicy(new PhaseBudget('late', { timeoutMs: 30_000 }));
    const request = client.contextMetadataRaw();
    const rejected = assert.rejects(request, /APPIUM_TIMEOUT/u);
    const response = new Response(null);
    response.text = () => { reading.resolve(); return body.promise; };
    if (stage === 'body') { headers.resolve(response); await reading.promise; }
    clock.expire();
    await rejected;
    const snapshot = structuredClone(client.snapshot());
    const receipt = client.snapshot().lastCommand!.timing;
    headers.resolve(response);
    body.resolve('{"value":[]}');
    await Promise.resolve(); await Promise.resolve();
    assert.deepEqual(client.snapshot(), snapshot);
    assert.equal(Object.isFrozen(receipt), true);
    await assertQuarantined(client, () => sent, policy);
  }]);
}

webdriverFatalTests.push(['Appium strict quarantine unscoped timeout blocks reentrant abort cleanup', async () => {
  const { AppiumClient: Client } = await clientSource();
  const headers = deferred<Response>();
  const clock = manualClock();
  const followups: Promise<void>[] = [];
  let sent = 0;
  let fatalAtAbort: ReturnType<AppiumClient['snapshot']>['firstFatal'];
  const client = new Client('http://protocol.invalid', 1_000, async (_input, init) => {
    sent++;
    if (sent === 1) return Response.json({ value: {}, sessionId: 'original' });
    init!.signal!.addEventListener('abort', () => {
      fatalAtAbort = client.snapshot().firstFatal;
      followups.push(assert.rejects(() => client.close(), /APPIUM_SESSION_UNUSABLE/u));
      followups.push(assert.rejects(() => client.command('/status', 'GET'), /APPIUM_SESSION_UNUSABLE/u));
    }, { once: true });
    return headers.promise;
  }, undefined, clock.clock);
  await client.create({ capabilities: {} });
  const request = assert.rejects(() => client.contextMetadataRaw(), /APPIUM_TIMEOUT/u);
  clock.expire();
  await request;
  headers.resolve(Response.json({ value: null }));
  await Promise.all(followups);
  assert.ok(fatalAtAbort, 'quarantine must be installed before abort listeners run');
  assert.equal(client.snapshot().firstFatal, fatalAtAbort);
  assert.equal(sent, 2);
}]);

webdriverFatalTests.push(['Appium strict quarantine dispatch boundary rechecks after callbacks', async () => {
  const { AppiumClient: Client } = await clientSource();
  let sent = 0;
  const client = new Client('http://protocol.invalid', 1_000, async () => { sent++; return Response.json({ value: {}, sessionId: 'original' }); });
  await client.create({ capabilities: {} });
  const policy = client.nativeRequestPolicy(new PhaseBudget('callback', { timeoutMs: 30_000 }));
  const fatal = new PhaseBudgetError('PHASE_BUDGET_EXHAUSTED', 'callback', 'synthetic', 1, 0);
  await assert.rejects(() => client.command('/alert/text', 'GET', undefined, 100, { ...policy,
    beforeDispatch: () => { (client as any).recordFatal(fatal); },
  }), /APPIUM_SESSION_UNUSABLE/u);
  assert.equal(sent, 1);
  await assertQuarantined(client, () => sent, policy);
}]);

for (const text of ['{}', 'null', '{"error":"unknown error"}', '{"value":{}}', '{"value":false}']) {
  webdriverFatalTests.push([`Appium strict quarantine DELETE requires a protocol acknowledgement ${text}`, async () => {
    const { AppiumClient: Client } = await clientSource();
    let sent = 0;
    const client = new Client('http://protocol.invalid', 1_000, async () => {
      sent++;
      return sent === 1 ? Response.json({ value: {}, sessionId: 'original' }) : new Response(text);
    });
    await client.create({ capabilities: {} });
    const policy = client.nativeRequestPolicy(new PhaseBudget('acknowledgement', { timeoutMs: 30_000 }));
    await assert.rejects(() => client.close(), /APPIUM_HTTP/u);
    await assertQuarantined(client, () => sent, policy);
  }]);
}

webdriverFatalTests.push(['Appium strict quarantine healthy close absence and completed errors remain usable', async () => {
  const { AppiumClient: Client } = await clientSource();
  for (const closeResponse of [() => Response.json({ value: null }), () => new Response(null, { status: 204 }),
    () => Response.json({ value: { error: 'invalid session id' } }, { status: 404 })]) {
    let sent = 0;
    let now = 0;
    const budget = new PhaseBudget('healthy-expired', { timeoutMs: 30_000, now: () => now });
    const client = new Client('http://protocol.invalid', 1_000, async (_input, init) => {
      sent++;
      if (init?.method === 'DELETE') return closeResponse();
      if (sent === 1) return Response.json({ value: { error: 'session not created' } }, { status: 500 });
      if (sent === 3) return Response.json({ value: { error: 'no such element' } }, { status: 404 });
      return Response.json({ value: {}, sessionId: 'original' });
    });
    await assert.rejects(() => client.create({ capabilities: {} }), /APPIUM_COMMAND/u);
    assert.equal(client.snapshot().firstFatal, undefined, 'completed Android setup HTTP 500 may recover');
    await client.create({ capabilities: {}, budget });
    await assert.rejects(() => client.contexts(), /APPIUM_COMMAND/u);
    assert.equal(client.snapshot().firstFatal, undefined);
    now = 30_001; // Expiration alone is not an observed fatal.
    await client.close();
    await client.close();
    assert.equal(sent, 4);
    assert.equal(client.snapshot().sessionId, '');
    assert.equal(client.snapshot().unusable, false);
    assert.equal(client.snapshot().firstFatal, undefined);
    client.setBudget(undefined);
    await client.create({ capabilities: {} });
    assert.equal(sent, 5);
  }
}]);

webdriverFatalTests.push(['Appium strict quarantine platform teardown rejects and Android finalizes once', async () => {
  const { AppiumClient: Client } = await clientSource();
  for (const platformName of ['ios', 'android'] as const) {
    let sent = 0;
    let finished = 0;
    const driver = new Client('http://protocol.invalid', 1_000, async () => {
      sent++;
      if (sent === 1) return Response.json({ value: {}, sessionId: 'original' });
      throw new DOMException('synthetic platform fatal', 'TimeoutError');
    });
    await driver.create({ capabilities: {} });
    const policy = driver.nativeRequestPolicy(new PhaseBudget('platform', { timeoutMs: 30_000 }));
    await assert.rejects(() => driver.contexts(), /APPIUM_TIMEOUT/u);
    const platform = platformName === 'ios'
      ? new IOSPlatform({ deviceId: 'synthetic', origin: 'https://fixture.invalid', setupUrl: 'https://fixture.invalid/setup', outputDir: '/unused', appiumUrl: 'http://protocol.invalid', certificate: '/unused' })
      : new AndroidPlatform({ deviceId: 'synthetic', origin: 'https://fixture.invalid', setupUrl: 'https://fixture.invalid/setup', outputDir: '/unused', appiumUrl: 'http://protocol.invalid', certificate: '/unused' });
    (platform as any).driver = driver;
    (platform as any).startupLog = { finish: async () => { finished++; } };
    const stopping = platform.stopOwnedResources();
    await assert.rejects(stopping, /APPIUM_SESSION_UNUSABLE/u);
    if (platformName === 'android') assert.equal(platform.stopOwnedResources(), stopping);
    await assert.rejects(platform.stopOwnedResources(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(finished, platformName === 'android' ? 1 : 0);
    assert.equal(sent, 2);
    await assertQuarantined(driver, () => sent, policy);
  }
}]);

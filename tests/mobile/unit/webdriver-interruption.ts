import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import type { WebDriverError, WebDriverRequestTiming } from '../support/webdriver';
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

export const webdriverInterruptionTests: Test[] = [];

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

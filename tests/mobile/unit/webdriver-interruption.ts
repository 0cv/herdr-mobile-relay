import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import type { WebDriverError } from '../support/webdriver';

type Test = [string, () => Promise<void>];

async function clientSource(): Promise<typeof import('../support/webdriver')> {
  const source = process.env.WEBDRIVER_TEST_SOURCE;
  return import(source ? pathToFileURL(source).href : new URL('../support/webdriver.ts', import.meta.url).href);
}

export const webdriverInterruptionTests: Test[] = [];
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

import assert from 'node:assert/strict';
import { PhaseBudget } from '../support/budget';
import { AppiumClient } from '../support/webdriver';
import initialAppiumSettings from './fixtures/ios-appium-settings.json';
import { initializeIOSConfirmationSettings, IOS_SESSION_SETTINGS, IOS_CONFIRMATION_SETTINGS, withIOSConfirmationSettings } from '../support/confirmation-settings';

export const confirmationSettingsTests: Array<[string, () => Promise<void>]> = [];
for (const mode of ['success', 'error', 'restore-error', 'readback', 'missing', 'malformed', 'unsupported', 'tail', 'interrupted', 'late', 'restore-interrupted'] as const) {
  confirmationSettingsTests.push([`iOS synthetic scoped settings ${mode}`, async () => {
    const saved = { waitForIdleTimeout: 10, animationCoolOffTimeout: 2, unrelated: 'preserved' };
    let settings: Record<string, unknown> = { ...saved };
    if (mode === 'missing') delete settings.waitForIdleTimeout;
    if (mode === 'malformed') settings.animationCoolOffTimeout = '2';
    let updates = 0;
    let actions = 0;
    const paths: string[] = [];
    const bodies: unknown[] = [];
    let late: Promise<Response> | undefined;
    const interrupted = () => {
      const response = new Response('');
      response.text = async () => { throw new DOMException('synthetic interrupted response body', 'AbortError'); };
      return response;
    };
    const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
      const path = new URL(String(input)).pathname;
      if (path === '/session') return Response.json({ value: {}, sessionId: 'settings' });
      paths.push(path);
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      if (path.endsWith('/appium/settings')) {
        if (init?.method === 'POST') {
          updates++;
          bodies.push(body.settings);
          if (updates === 2 && mode === 'restore-interrupted') return interrupted();
          if ((updates === 2 && mode === 'restore-error') || mode === 'unsupported') return Response.json({ value: { error: 'invalid argument', message: 'synthetic unsupported settings' } }, { status: 400 });
          settings = { ...settings, ...body.settings };
          return Response.json({ value: null });
        }
        return Response.json({ value: mode === 'readback' && updates === 1 ? saved : settings });
      }
      actions++;
      if (mode === 'interrupted') return interrupted();
      if (mode === 'late') {
        late = new Promise<Response>(resolve => init!.signal!.addEventListener('abort', () => setTimeout(() => resolve(Response.json({ value: null })), 20), { once: true }));
        return late;
      }
      return Response.json({ value: null });
    });
    await driver.create({ capabilities: {} });
    const budget = new PhaseBudget('synthetic-settings', { timeoutMs: mode === 'tail' ? 58_999 : 80_000 });
    driver.setBudget(budget);
    const cause = new Error('original settled observation failure');
    let error: unknown;
    try {
      await withIOSConfirmationSettings(driver, budget, 49_000, async phase => {
        assert.equal(Date.parse(budget.snapshot().deadline) - Date.parse(phase.snapshot().deadline), 4_000);
        assert.deepEqual(settings, { ...saved, ...IOS_CONFIRMATION_SETTINGS });
        if (mode === 'error' || mode === 'restore-error' || mode === 'restore-interrupted') throw cause;
        await driver.command('/element/confirmed/click', 'POST', {}, mode === 'late' ? 1_000 : 2_000);
      });
    } catch (caught) { error = caught; }
    if (late) await late;
    if (mode === 'success') assert.equal(error, undefined);
    else assert.ok(error);
    if (['error', 'restore-error', 'restore-interrupted'].includes(mode)) assert.equal(error, cause);
    if (['interrupted', 'late'].includes(mode)) {
      assert.equal(updates, 1);
      assert.equal(driver.snapshot().unusable, true);
      const count = paths.length;
      await assert.rejects(() => driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
      assert.equal(paths.length, count);
    } else if (mode === 'tail' || mode === 'missing' || mode === 'malformed') {
      assert.equal(updates, 0);
      assert.equal(actions, 0);
      if (mode === 'tail') assert.equal(paths.length, 0);
    } else {
      assert.equal(updates, 2);
      assert.deepEqual(bodies, [IOS_CONFIRMATION_SETTINGS, { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 }]);
      if (!['restore-error', 'restore-interrupted', 'unsupported'].includes(mode)) assert.deepEqual(settings, saved);
    }
    assert.ok(actions <= 1);
  }]);
}

for (const boundary of [1, 2, 3, 4, 5]) {
  confirmationSettingsTests.push([`iOS synthetic interrupted settings request ${boundary} is the last normal driver command`, async () => {
    let requests = 0;
    let actions = 0;
    let settings = { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 };
    const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
      if (new URL(String(input)).pathname === '/session') return Response.json({ value: {}, sessionId: 'settings' });
      requests++;
      if (requests === boundary) {
        const response = new Response('');
        response.text = async () => { throw new Error('synthetic interrupted settings response'); };
        return response;
      }
      if (init?.method === 'POST') settings = JSON.parse(String(init.body)).settings;
      return Response.json({ value: settings });
    });
    await driver.create({ capabilities: {} });
    const budget = new PhaseBudget('settings-boundaries', { timeoutMs: 80_000 });
    driver.setBudget(budget);
    await assert.rejects(() => withIOSConfirmationSettings(driver, budget, 49_000, async () => { actions++; }), /APPIUM_INTERRUPTED/u);
    assert.equal(requests, boundary);
    assert.equal(actions, boundary > 3 ? 1 : 0);
    const fatal = driver.snapshot().firstFatal;
    assert.equal(fatal?.code, 'APPIUM_INTERRUPTED');
    await assert.rejects(() => driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests, boundary);
    assert.deepEqual(driver.snapshot().firstFatal, fatal);
  }]);
}

for (const operationMs of [49_000, 57_000]) confirmationSettingsTests.push([`iOS synthetic exact ${operationMs}ms whole settings transaction is admissible and reserves the absolute parent tail`, async () => {
  let now = 0;
  let settings = { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 };
  const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
    if (new URL(String(input)).pathname === '/session') return Response.json({ value: {}, sessionId: 'settings' });
    now += 2_000;
    if (init?.method === 'POST') settings = JSON.parse(String(init.body)).settings;
    return Response.json({ value: settings });
  });
  await driver.create({ capabilities: {} });
  const budget = new PhaseBudget('exact-settings', { timeoutMs: operationMs + 10_000, now: () => now });
  driver.setBudget(budget);
  await withIOSConfirmationSettings(driver, budget, operationMs, async phase => {
    assert.equal(phase.remainingMs, operationMs);
    now += operationMs;
  });
  assert.equal(budget.remainingMs, 0);
  assert.deepEqual(settings, { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 });
}]);

for (const mode of ['success', 'missing', 'malformed', 'shadowed', 'partial-update', 'short-parent', 'short-readback', 'bad-acknowledgement', 'interrupted-update', 'interrupted-readback'] as const) {
  confirmationSettingsTests.push([`iOS recorded omission and producer-shaped initialization ${mode}`, async () => {
    const cache: Record<string, unknown> = { ...initialAppiumSettings };
    const wda: Record<string, unknown> = { waitForIdleTimeout: 23, animationCoolOffTimeout: 5 };
    const writes: unknown[] = [];
    let requests = 0;
    let initializing = false;
    let now = 0;
    const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
      if (new URL(String(input)).pathname === '/session') return Response.json({ value: {}, sessionId: 'settings' });
      requests++;
      if (initializing && ((mode === 'interrupted-update' && init?.method === 'POST') || (mode === 'interrupted-readback' && init?.method === 'GET'))) {
        const response = new Response('');
        response.text = async () => { throw new Error('interrupted initialization'); };
        return response;
      }
      if (init?.method === 'POST') {
        const settings = JSON.parse(String(init.body)).settings;
        writes.push(settings);
        for (const [key, value] of Object.entries(settings)) {
          if (cache[key] !== undefined && cache[key] === value) continue;
          if (mode === 'partial-update' && key === 'animationCoolOffTimeout') return Response.json({ value: { error: 'invalid argument', message: 'rejected second setting' } }, { status: 400 });
          wda[key] = value;
          cache[key] = value;
        }
        if (mode === 'short-readback') now = 75_001;
        return Response.json({ value: mode === 'bad-acknowledgement' ? {} : null });
      }
      const result = { ...cache };
      if (initializing && mode === 'missing') delete result.waitForIdleTimeout;
      if (initializing && mode === 'malformed') result.animationCoolOffTimeout = '2';
      if (initializing && mode === 'shadowed') result.waitForIdleTimeout = 23;
      return Response.json({ value: result });
    });
    await driver.create({ capabilities: {} });
    const parent = new PhaseBudget('owned-session-settings', { timeoutMs: 80_000, now: () => now });
    driver.setBudget(parent);
    assert.deepEqual(await driver.settings(), initialAppiumSettings);
    await assert.rejects(() => withIOSConfirmationSettings(driver, parent, 49_000, async () => assert.fail('must not act before establishment')), /unsupported or malformed waitForIdleTimeout/u);
    assert.deepEqual(writes, []);
    initializing = true;
    const initialization = initializeIOSConfirmationSettings(driver, mode === 'short-parent' ? new PhaseBudget('short', { timeoutMs: 9_999 }) : parent);
    if (mode !== 'success') {
      await assert.rejects(() => initialization);
      assert.equal(writes.length, ['short-parent', 'interrupted-update'].includes(mode) ? 0 : 1);
      if (mode === 'short-readback' || mode === 'bad-acknowledgement') assert.equal(requests, 3);
      if (mode.startsWith('interrupted')) {
        const count = requests;
        await assert.rejects(() => driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
        assert.equal(requests, count);
      }
      return;
    }
    await initialization;
    initializing = false;
    assert.deepEqual(await driver.settings(), { ...initialAppiumSettings, ...IOS_SESSION_SETTINGS });
    assert.deepEqual(wda, IOS_SESSION_SETTINGS);
    await driver.updateSettings({ waitForIdleTimeout: 7, animationCoolOffTimeout: 0.8 });
    await withIOSConfirmationSettings(driver, parent, 49_000, async () => {
      assert.deepEqual(wda, IOS_CONFIRMATION_SETTINGS);
    });
    assert.deepEqual(wda, { waitForIdleTimeout: 7, animationCoolOffTimeout: 0.8 });
    assert.deepEqual(await driver.settings(), { ...initialAppiumSettings, ...wda });
    assert.deepEqual(writes, [IOS_SESSION_SETTINGS, wda, IOS_CONFIRMATION_SETTINGS, wda]);
  }]);
}

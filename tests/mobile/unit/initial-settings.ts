import assert from 'node:assert/strict';
import { PhaseBudget } from '../support/budget';
import { AppiumClient } from '../support/webdriver';
import { initializeIOSConfirmationSettings, IOS_SESSION_SETTINGS } from '../support/confirmation-settings';

export const initialSettingsTests: Array<[string, () => Promise<void>]> = [];
const wait = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms));

for (const mode of ['recorded-profile', 'slow-body', 'late-response', 'hung-body', 'parent-exhausted', 'exact-admission'] as const) {
  initialSettingsTests.push([`iOS cold initialization ${mode} with synthetic transport/body overhead`, async () => {
    let requests = 0;
    let now = 0;
    let late: Promise<Response> | undefined;
    let settings = {};
    const driver = new AppiumClient('http://initial-settings.invalid', 30_000, async (input, init) => {
      if (new URL(String(input)).pathname === '/session') return Response.json({ value: {}, sessionId: 'initial' });
      requests++;
      if (mode === 'exact-admission') now += 5_000;
      if (init?.method === 'GET') return Response.json({ value: settings });
      settings = { ...settings, ...JSON.parse(String(init?.body)).settings };
      if (mode === 'late-response') {
        late = new Promise<Response>(resolve => init!.signal!.addEventListener('abort', () => {
          setTimeout(() => resolve(Response.json({ value: null })), 20);
        }, { once: true }));
        return late;
      }
      if (mode === 'recorded-profile') {
        await wait(1_209);
        await wait(844);
      }
      if (mode === 'parent-exhausted') now = 10_000;
      const response = Response.json({ value: null });
      const text = response.text.bind(response);
      response.text = async () => {
        if (mode === 'hung-body') return new Promise<string>(() => {});
        await wait(mode === 'slow-body' ? 2_200 : mode === 'recorded-profile' ? 100 : 0);
        return text();
      };
      return response;
    });
    await driver.create({ capabilities: {} });
    const parent = new PhaseBudget('initial-settings', { timeoutMs: ['parent-exhausted', 'exact-admission'].includes(mode) ? 10_000 : 15_000, ...(['parent-exhausted', 'exact-admission'].includes(mode) ? { now: () => now } : {}) });
    driver.setBudget(parent);
    if (mode === 'recorded-profile' || mode === 'slow-body' || mode === 'exact-admission') {
      await initializeIOSConfirmationSettings(driver, parent);
      assert.equal(requests, 2);
      assert.deepEqual(settings, IOS_SESSION_SETTINGS);
      assert.equal(driver.snapshot().firstFatal, undefined);
      return;
    }
    await assert.rejects(() => initializeIOSConfirmationSettings(driver, parent), mode === 'parent-exhausted' ? /insufficient initialization readback allowance/u : /APPIUM_TIMEOUT/u);
    if (late) await late;
    assert.equal(requests, 1);
    if (mode === 'parent-exhausted') return;
    const fatal = driver.snapshot().firstFatal;
    assert.ok(fatal);
    await assert.rejects(() => driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
    await assert.rejects(() => driver.command('/element/next/click', 'POST', {}), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests, 1);
    assert.deepEqual(driver.snapshot().firstFatal, fatal);
  }]);
}

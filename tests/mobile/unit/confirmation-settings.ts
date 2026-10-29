import assert from 'node:assert/strict';
import { PhaseBudget } from '../support/budget';
import { AppiumClient, WebDriverError } from '../support/webdriver';
import initialAppiumSettings from './fixtures/ios-appium-settings.json';
import { CONFIRMATION_RESTORE_MS, CONFIRMATION_SETTINGS_COMMAND_MS, initializeIOSConfirmationSettings, IOS_SESSION_SETTINGS, IOS_CONFIRMATION_SETTINGS, withIOSConfirmationSettings } from '../support/confirmation-settings';

async function policySettings(remaining: number, request?: (ordinal: number, advance: (ms: number) => void) => Response | undefined) {
  let now = 0;
  let requests = 0;
  const durations: number[] = [];
  let settings: Record<string, unknown> = { ...IOS_SESSION_SETTINGS };
  const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
    if (String(input).endsWith('/session')) return Response.json({ value: {}, sessionId: 'policy-settings' });
    requests++;
    const override = request?.(requests, ms => { now += ms; });
    if (override) return override;
    if (init?.method === 'POST') {
      settings = { ...settings, ...JSON.parse(String(init.body)).settings };
      return Response.json({ value: null });
    }
    return Response.json({ value: settings });
  }, undefined, { now: () => now, timer: () => 0 as unknown as ReturnType<typeof setTimeout>, clear: () => undefined });
  await driver.create({ capabilities: {} });
  const root = new PhaseBudget('root', { timeoutMs: remaining, now: () => now });
  driver.setBudget(root);
  const install = root.phaseView('install', 120_000);
  const transaction = install.phaseView('transaction', install.remainingMs, 1_500);
  const policy = driver.nativeRequestPolicy(transaction);
  const original = driver.command.bind(driver);
  driver.command = async (path, method, body, timeout, scoped) => {
    durations.push(timeout!);
    return await original(path, method, body, timeout, scoped) as any;
  };
  return { driver, install, transaction, policy, durations, requests: () => requests,
    settings: () => settings, advance: (ms: number) => { now += ms; } };
}

export const confirmationSettingsTests: Array<[string, () => Promise<void>]> = [];

confirmationSettingsTests.push(['iOS synthetic scoped settings SM5780500 admission and80499 refusal reserve5500 exactly once', async () => {
  for (const remaining of [80_500, 80_499]) {
    const a = await policySettings(remaining);
    let operations = 0;
    const action = withIOSConfirmationSettings(a.driver, a.transaction, 69_000, async operation => {
      operations++;
      assert.equal(Date.parse(a.install.snapshot().deadline) - Date.parse(operation.snapshot().deadline), 5_500);
      assert.equal(Date.parse(a.transaction.snapshot().deadline) - Date.parse(operation.snapshot().deadline), 4_000);
      const confirmation = operation.phaseView('confirmation', 75_000);
      assert.equal(Date.parse(confirmation.snapshot().deadline), 75_000);
    }, a.policy);
    if (remaining === 80_500) await action;
    else await assert.rejects(action, /insufficient whole transaction allowance/u);
    assert.equal(operations, remaining === 80_500 ? 1 : 0);
    assert.equal(a.requests(), remaining === 80_500 ? 5 : 0);
    assert.deepEqual(a.durations, remaining === 80_500 ? [2_000, 2_000, 2_000, 4_000, 4_000] : []);
    assert.deepEqual(a.settings(), IOS_SESSION_SETTINGS);
  }
}]);

confirmationSettingsTests.push(['iOS scoped restoration retains the original 4000ms minimum tail', async () => {
  const a = await policySettings(20_000);
  await assert.rejects(
    () => withIOSConfirmationSettings(a.driver, a.transaction, 1_000, async operation => {
      a.advance(operation.remainingMs + 1);
    }, a.policy),
    /insufficient restoration allowance/u,
  );
  assert.equal(a.requests(), 3);
  assert.deepEqual(a.durations, [2_000, 2_000, 2_000]);
  assert.equal(a.driver.snapshot().firstFatal, undefined);
}]);

confirmationSettingsTests.push(['iOS scoped restoration captures the 8000ms allowance once for both commands', async () => {
  for (const slack of [7_999, 8_000]) {
    const a = await policySettings(20_000, (ordinal, advance) => {
      if (ordinal === 4) advance(2_865);
      return undefined;
    });
    const action = withIOSConfirmationSettings(a.driver, a.transaction, 1_000, async () => {
      a.advance(a.transaction.remainingMs - slack);
    }, a.policy);
    if (slack === 8_000) await action;
    else await assert.rejects(action, /APPIUM_TIMEOUT/u);
    assert.deepEqual(a.durations, slack === 8_000
      ? [2_000, 2_000, 2_000, 4_000, 4_000]
      : [2_000, 2_000, 2_000, 2_000]);
    assert.equal(a.requests(), slack === 8_000 ? 5 : 4);
    if (slack === 8_000) {
      assert.deepEqual(a.settings(), IOS_SESSION_SETTINGS);
      assert.equal(a.driver.snapshot().firstFatal, undefined);
    } else {
      assert.equal(a.driver.snapshot().firstFatal?.code, 'APPIUM_TIMEOUT');
      assert.equal(a.driver.snapshot().unusable, true);
    }
  }
}]);

confirmationSettingsTests.push(['iOS scoped restoration times out at the full captured cap before readback', async () => {
  for (const stage of ['transport', 'body'] as const) {
    const a = await policySettings(20_000, (ordinal, advance) => {
      if (ordinal !== 4) return undefined;
      if (stage === 'transport') {
        advance(4_000);
        return undefined;
      }
      const response = Response.json({ value: null });
      const text = response.text.bind(response);
      response.text = async () => {
        const body = await text();
        advance(4_000);
        return body;
      };
      return response;
    });
    await assert.rejects(
      () => withIOSConfirmationSettings(a.driver, a.transaction, 1_000, async () => undefined, a.policy),
      /APPIUM_TIMEOUT/u,
    );
    assert.equal(a.requests(), 4);
    assert.deepEqual(a.durations, [2_000, 2_000, 2_000, 4_000]);
    const firstFatal = a.driver.snapshot().firstFatal;
    assert.equal(firstFatal?.code, 'APPIUM_TIMEOUT');
    assert.equal(a.driver.snapshot().unusable, true);
    await assert.rejects(() => a.driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(a.requests(), 4);
    assert.equal(a.driver.snapshot().firstFatal, firstFatal);
  }
}]);

confirmationSettingsTests.push(['iOS scoped restoration refuses a readback without shortening the captured allowance', async () => {
  const a = await policySettings(20_000);
  const updateSettings = a.driver.updateSettings.bind(a.driver);
  let updates = 0;
  a.driver.updateSettings = async (...args: Parameters<AppiumClient['updateSettings']>) => {
    const acknowledgement = await updateSettings(...args);
    if (++updates === 2) a.advance(4_001);
    return acknowledgement;
  };
  await assert.rejects(
    () => withIOSConfirmationSettings(a.driver, a.transaction, 1_000, async () => {
      a.advance(a.transaction.remainingMs - 8_000);
    }, a.policy),
    /insufficient restoration readback allowance/u,
  );
  assert.equal(a.requests(), 4);
  assert.deepEqual(a.durations, [2_000, 2_000, 2_000, 4_000]);
  assert.equal(a.driver.snapshot().firstFatal, undefined);
  assert.equal(a.driver.snapshot().unusable, false);
}]);

confirmationSettingsTests.push(['iOS scoped restoration keeps root admission for the captured readback allowance', async () => {
  let now = 0;
  let requests = 0;
  let settings: Record<string, unknown> = { ...IOS_SESSION_SETTINGS };
  const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
    if (String(input).endsWith('/session')) return Response.json({ value: {}, sessionId: 'root-settings' });
    requests++;
    if (init?.method === 'POST') {
      settings = { ...settings, ...JSON.parse(String(init.body)).settings };
      return Response.json({ value: null });
    }
    return Response.json({ value: settings });
  }, undefined, { now: () => now, timer: () => 0 as unknown as ReturnType<typeof setTimeout>, clear: () => undefined });
  await driver.create({ capabilities: {} });
  const root = new PhaseBudget('root-settings', { timeoutMs: 10_000, now: () => now });
  const transaction = new PhaseBudget('independent-transaction', { timeoutMs: 20_000, now: () => now });
  driver.setBudget(root);
  const policy = driver.nativeRequestPolicy(transaction);
  const updateSettings = driver.updateSettings.bind(driver);
  let updates = 0;
  driver.updateSettings = async (...args: Parameters<AppiumClient['updateSettings']>) => {
    const acknowledgement = await updateSettings(...args);
    if (++updates === 2) now += 1_001;
    return acknowledgement;
  };
  await assert.rejects(() => withIOSConfirmationSettings(driver, transaction, 1_000, async () => { now += 5_000; }, policy), /APPIUM_COMMAND_NOT_ADMITTED/u);
  assert.equal(requests, 4);
  assert.equal(driver.snapshot().lastCommand?.timeoutMs, 4_000);
  assert.equal(driver.snapshot().lastCommand?.timing?.sent, false);
  assert.equal(driver.snapshot().firstFatal, undefined);
  assert.equal(driver.snapshot().unusable, false);
}]);

confirmationSettingsTests.push(['iOS scoped restoration requires a null acknowledgement and exact readback', async () => {
  for (const [kind, response] of [
    ['non-null', Response.json({ value: {} })],
    ['missing-value', Response.json({})],
    ['invalid-json', new Response('invalid JSON')],
  ] as const) {
    const a = await policySettings(20_000, ordinal => ordinal === 4 ? response : undefined);
    await assert.rejects(
      () => withIOSConfirmationSettings(a.driver, a.transaction, 1_000, async () => undefined, a.policy),
      error => kind === 'invalid-json'
        ? error instanceof WebDriverError && error.code === 'APPIUM_HTTP'
        : error instanceof Error && /malformed restoration acknowledgement/u.test(error.message),
    );
    assert.equal(a.requests(), 4);
    assert.deepEqual(a.durations, [2_000, 2_000, 2_000, 4_000]);
  }
  const mismatch = await policySettings(20_000, ordinal => ordinal === 5
    ? Response.json({ value: { ...IOS_SESSION_SETTINGS, waitForIdleTimeout: 9 } })
    : undefined);
  await assert.rejects(
    () => withIOSConfirmationSettings(mismatch.driver, mismatch.transaction, 1_000, async () => undefined, mismatch.policy),
    /settings readback mismatch/u,
  );
  assert.equal(mismatch.requests(), 5);
  assert.deepEqual(mismatch.settings(), IOS_SESSION_SETTINGS);
}]);

confirmationSettingsTests.push(['iOS synthetic scoped settings SM57 restoration has4000 and post-tail1500 without renewal', async () => {
  const a = await policySettings(80_500, (ordinal, advance) => {
    if (ordinal <= 5) advance(1_999);
    return undefined;
  });
  await withIOSConfirmationSettings(a.driver, a.transaction, 69_000, async operation => {
    assert.equal(operation.remainingMs, 69_003);
    const deadline = operation.snapshot().deadline;
    a.advance(operation.remainingMs - 1);
    assert.equal(operation.snapshot().deadline, deadline);
    assert.equal(a.transaction.remainingMs, 4_001);
    assert.equal(a.install.remainingMs, 5_501);
  }, a.policy);
  assert.equal(a.requests(), 5);
  assert.equal(a.transaction.remainingMs, 3);
  assert.equal(a.install.remainingMs, 1_503);
  assert.deepEqual(a.settings(), IOS_SESSION_SETTINGS);
  assert.equal(a.driver.snapshot().firstFatal, undefined);
  const initial = await policySettings(80_500);
  await initializeIOSConfirmationSettings(initial.driver, initial.install);
  assert.equal(initial.requests(), 2);
  assert.deepEqual(initial.durations, [5_000, 5_000]);
}]);

confirmationSettingsTests.push(['iOS synthetic scoped settings SM57 late request or final-action failure prevents normal restoration', async () => {
  for (const boundary of [1, 2, 3, 4, 5]) {
    const a = await policySettings(80_500, (ordinal, advance) => {
      if (ordinal === boundary) advance(boundary >= 4 ? 4_000 : 2_000);
      return undefined;
    });
    await assert.rejects(() => withIOSConfirmationSettings(a.driver, a.transaction, 69_000, async () => undefined, a.policy), /APPIUM_TIMEOUT/u);
    assert.equal(a.requests(), boundary);
    const first = a.driver.snapshot().firstFatal;
    assert.equal(first?.code, 'APPIUM_TIMEOUT');
    await assert.rejects(() => a.driver.settings(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(a.driver.snapshot().firstFatal, first);
    assert.equal(a.requests(), boundary);
  }
  for (const text of ['{"value":{"error":"stale element reference"}}', '{"value":{}}', 'invalid JSON']) {
    const a = await policySettings(80_500, ordinal => ordinal === 4 ? new Response(text, { status: text.includes('stale') ? 404 : 200 }) : undefined);
    let original: unknown;
    await assert.rejects(() => withIOSConfirmationSettings(a.driver, a.transaction, 69_000, async operation => {
      try { await a.driver.click('add', 12_000, { ...a.policy, budget: operation, finalAction: true }); }
      catch (error) { original = error; throw error; }
    }, a.policy), error => error === original && error instanceof WebDriverError);
    assert.equal(a.requests(), 4);
    assert.equal(a.driver.snapshot().firstFatal?.code, (original as WebDriverError).code);
    assert.equal(a.driver.snapshot().unusable, true);
  }
  const a = await policySettings(80_500, ordinal => ordinal === 4 ? Response.json({ value: { error: 'invalid argument' } }, { status: 400 }) : undefined);
  const original = new Error('settled operation failure');
  await assert.rejects(() => withIOSConfirmationSettings(a.driver, a.transaction, 69_000, async () => { throw original; }, a.policy), error => error === original);
  assert.equal(a.requests(), 4);
  assert.equal(a.driver.snapshot().firstFatal, undefined);
}]);
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
        assert.equal(Date.parse(budget.snapshot().deadline) - Date.parse(phase.snapshot().deadline), CONFIRMATION_RESTORE_MS);
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
      if (init?.method === 'POST') {
        settings = JSON.parse(String(init.body)).settings;
        return Response.json({ value: null });
      }
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
    now += CONFIRMATION_SETTINGS_COMMAND_MS;
    if (init?.method === 'POST') {
      settings = JSON.parse(String(init.body)).settings;
      return Response.json({ value: null });
    }
    return Response.json({ value: settings });
  });
  await driver.create({ capabilities: {} });
  const budget = new PhaseBudget('exact-settings', { timeoutMs: operationMs + 5 * CONFIRMATION_SETTINGS_COMMAND_MS, now: () => now });
  driver.setBudget(budget);
  await withIOSConfirmationSettings(driver, budget, operationMs, async phase => {
    assert.equal(phase.remainingMs, operationMs);
    now += operationMs;
  });
  assert.equal(budget.remainingMs, 0);
  assert.deepEqual(settings, { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 });
}]);

confirmationSettingsTests.push(['iOS recorded 3312ms settings update remains outside the original command allowance', async () => {
  let updates = 0;
  let settings: Record<string, unknown> = { waitForIdleTimeout: 10, animationCoolOffTimeout: 2 };
  const driver = new AppiumClient('http://settings.invalid', 30_000, async (input, init) => {
    const path = new URL(String(input)).pathname;
    if (path === '/session') return Response.json({ value: {}, sessionId: 'settings' });
    if (path.endsWith('/appium/settings')) {
      if (init?.method === 'POST') {
        updates++;
        settings = { ...settings, ...JSON.parse(String(init.body)).settings };
        if (updates === 1) await new Promise<void>(resolve => setTimeout(resolve, 3_312));
        return Response.json({ value: null });
      }
      return Response.json({ value: settings });
    }
    return Response.json({ value: null });
  });
  await driver.create({ capabilities: {} });
  const budget = new PhaseBudget('recorded-settings-delay', { timeoutMs: 30_000 });
  driver.setBudget(budget);
  await assert.rejects(() => withIOSConfirmationSettings(driver, budget, 1_000, async () => {
    assert.fail('the delayed settings response must not authorize the operation');
  }), /APPIUM_TIMEOUT/u);
  assert.equal(updates, 1);
  assert.equal(driver.snapshot().unusable, true);
  const update = driver.snapshot().commands.find(command => command.method === 'POST' && command.path.endsWith('/appium/settings'));
  assert.equal(update?.timeoutMs, CONFIRMATION_SETTINGS_COMMAND_MS);
  assert.equal(update?.timedOut, true);
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

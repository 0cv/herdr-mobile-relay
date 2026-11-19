import { expect, test, type Browser, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';

const appOrigin = 'http://127.0.0.1:4173';
const demoOrigin = 'http://127.0.0.1:4174';
const release = JSON.parse(readFileSync('dist/release.json', 'utf8')) as {
  build: string;
  files: { javascript: { path: string }; stylesheet: { path: string } };
};
const demoLoader = '/assets/demo-loader.js';
const demoScript = '/assets/demo.js';
const demoAssets = new Map([
  ['/', 'document'],
  [demoLoader, 'script'],
  [demoScript, 'script'],
  [`/${release.files.stylesheet.path}`, 'stylesheet'],
  ['/fonts/nerd-symbols-mono-v3.4.0.woff2', 'font'],
]);

async function watchDemo(page: Page, options: { block?: string; simulatedPermission?: NotificationPermission } = {}) {
  const unexpected: string[] = [];
  const storage: string[] = [];
  const permissions: string[] = [];
  const blocked: string[] = [];
  page.on('pageerror', (error) => unexpected.push(error.message));
  page.on('websocket', (socket) => unexpected.push(socket.url()));
  page.on('console', (message) => {
    const [kind, value] = message.text().split(': ', 2);
    if (kind === 'Unexpected demo access') unexpected.push(value);
    if (kind === 'Demo origin storage') storage.push(value);
    if (kind === 'Demo origin permission') permissions.push(value);
  });
  await page.route('**/*', (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== demoOrigin || url.search || request.method() !== 'GET'
      || demoAssets.get(url.pathname) !== request.resourceType()) {
      unexpected.push(`${request.method()} ${request.url()}`);
      return route.abort();
    }
    if (url.pathname !== options.block) return route.continue();
    blocked.push(url.pathname);
    return route.abort();
  });
  await page.addInitScript((simulatedPermission) => {
    const debug = console.debug.bind(console);
    const NotificationApi = window.Notification;
    debug(`Demo origin storage: ${localStorage.length},${sessionStorage.length}`);
    debug(`Demo origin permission: ${NotificationApi?.permission ?? 'unavailable'}`);
    function unexpected(name: string): never {
      debug(`Unexpected demo access: ${name}`);
      throw new Error(`Unexpected demo access: ${name}`);
    }
    function guard(object: object, ...names: string[]) {
      for (const name of names) Object.defineProperty(object, name, { configurable: true, get: () => unexpected(name) });
    }
    guard(window, 'localStorage', 'sessionStorage', 'indexedDB', 'caches', 'fetch', 'XMLHttpRequest', 'WebSocket', 'RTCPeerConnection');
    guard(navigator, 'permissions', 'sendBeacon');
    if (navigator.serviceWorker) guard(navigator.serviceWorker, 'register');
    Object.defineProperty(window, 'Notification', { configurable: true, value: new Proxy(NotificationApi ?? function () {}, {
      get(target, property) {
        if (property === 'permission') {
          debug('Unexpected demo access: Notification.permission');
          return simulatedPermission ?? Reflect.get(target, property);
        }
        if (property === 'requestPermission') return unexpected('Notification.requestPermission');
        return Reflect.get(target, property);
      },
      construct: () => unexpected('new Notification'),
    }) });
  }, options.simulatedPermission);
  return { unexpected, storage, permissions, blocked };
}

async function expectSampleHome(page: Page) {
  await expect(page).toHaveURL(`${demoOrigin}/`);
  await expect(page.getByRole('banner').getByRole('heading', { name: 'Demo — sample data' })).toBeVisible();
  await expect(page.getByText('Notification delivery: Off (unavailable in demo)')).toBeVisible();
  const agents = page.getByRole('main', { name: 'Agents' });
  await expect(agents.getByRole('button', { name: 'Atlas', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(agents.getByRole('button', { name: 'Birch', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(agents.getByText('Approval required', { exact: true })).toBeVisible();
  await expect(agents.getByText('make test', { exact: true })).toBeVisible();
  await expect(agents.getByText('Plan question', { exact: true })).toBeVisible();
  await expect(agents.getByText('How should we roll out the new search page?')).toBeVisible();
  for (const name of ['Patch review on Atlas', 'Release plan on Atlas', 'Release notes on Atlas', 'Docs preview on Birch']) {
    await expect(agents.getByRole('button', { name: `Open ${name}`, exact: true, includeHidden: true })).toBeDisabled();
  }
  await expect(page.getByRole('button', { name: /Settings|Allow once|Decline|Choose answer/ })).toHaveCount(0);
  await expect(page.locator('link[rel="manifest"]')).toHaveCount(0);
}

const target = { relay_id: 'saved', server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'saved-term', generation: 1, agent_session_id: 'saved-session' };
const draftIdentity = JSON.stringify([target, 'codex', '/sample/project']);
const draftKey = `herdr_prompt_draft_v1:${encodeURIComponent(draftIdentity)}`;

// Headless Shell can report denied after a grant. Use full pinned Chromium for these tests.
const permissionTest = test.extend<object, { browser: Browser }>({
  browser: [async ({ browser, browserName, playwright }, use) => {
    if (browserName !== 'chromium') return use(browser);
    const chromium = await playwright.chromium.launch({ channel: 'chromium' });
    try {
      await use(chromium);
    } finally {
      await chromium.close();
    }
  }, { scope: 'worker' }],
});
permissionTest.use({ serviceWorkers: 'allow' });

for (const permission of ['granted', 'denied'] as const) {
  permissionTest(`demo home is isolated with notifications ${permission}`, async ({ context, page: realApp, browserName }) => {
    const sockets: string[] = [];
    await context.routeWebSocket('**/*', async (socket) => {
      sockets.push(socket.url());
      await socket.close();
    });
    await context.route('**/*', (route) => new URL(route.request().url()).origin === appOrigin
      ? route.continue() : route.abort());
    await realApp.goto(appOrigin);
    await expect(realApp.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
    await realApp.evaluate(async () => { await navigator.serviceWorker.register('/sw.js'); });
    const realController = () => realApp.evaluate(() => {
      const worker = navigator.serviceWorker.controller;
      return worker && { url: worker.scriptURL, state: worker.state };
    });
    const expectedController = { url: `${appOrigin}/sw.js`, state: 'activated' };
    await expect.poll(realController).toEqual(expectedController);

    const local = {
      herdr_relays: JSON.stringify([{ id: 'saved', label: 'Saved relay', url: 'wss://saved-relay.invalid', token: '', paired: true }]),
      herdr_device_auth_v1: JSON.stringify({ version: 1, relays: { saved: {
        kind: 'credential', id: 'saved-credential', deviceId: 'saved-device', version: 1,
        secret: 'A'.repeat(43), locale: 'en', issuedAt: Date.now(), role: 'controller',
      } } }),
      [draftKey]: JSON.stringify({ version: 1, identity: draftIdentity, text: 'Disposable draft', updatedAt: Date.now() }),
      herdr_hidden_servers: '["demo-atlas"]',
      herdr_theme: 'light',
      herdr_push_enabled: 'true',
    };
    const session = { herdr_pending_relay_updates: '{"sentinel":true}' };
    await realApp.evaluate(({ local, session }) => {
      for (const [key, value] of Object.entries(local)) localStorage.setItem(key, value);
      for (const [key, value] of Object.entries(session)) sessionStorage.setItem(key, value);
    }, { local, session });

    // Playwright's mobile WebKit has no Notification API; simulate its getter there only.
    const simulatedPermission = browserName === 'webkit' ? permission : undefined;
    if (simulatedPermission) {
      test.info().annotations.push({ type: 'permission simulation', description: 'Notification API unavailable in mobile WebKit' });
    } else {
      await context.grantPermissions(permission === 'granted' ? ['notifications'] : [], { origin: demoOrigin });
    }
    const popup = realApp.waitForEvent('popup');
    await realApp.evaluate(() => { window.open('about:blank'); });
    const demo = await popup;
    expect(await demo.evaluate(() => sessionStorage.getItem('herdr_pending_relay_updates'))).toBe(session.herdr_pending_relay_updates);
    const observation = await watchDemo(demo, { simulatedPermission });
    await demo.goto(demoOrigin);
    await expectSampleHome(demo);
    await expect(demo.locator('html')).toHaveAttribute('data-theme', 'nord');
    await expect(demo.getByText('Saved relay')).toHaveCount(0);
    expect(await demo.evaluate(async () => ({
      controlled: Boolean(navigator.serviceWorker.controller),
      registrations: (await navigator.serviceWorker.getRegistrations()).length,
    }))).toEqual({ controlled: false, registrations: 0 });

    await demo.getByRole('button', { name: 'Atlas', exact: true }).click();
    await expect(demo.getByText('Approval required')).toHaveCount(0);
    await expect(demo.getByRole('heading', { name: 'Demo — sample data' })).toBeVisible();
    await demo.reload();
    await expectSampleHome(demo);
    expect(observation.storage).toEqual(['0,0', '0,0']);
    const expectedPermission = simulatedPermission ? 'unavailable' : permission;
    expect(observation.permissions).toEqual([expectedPermission, expectedPermission]);
    expect(observation.unexpected).toEqual([]);
    expect(sockets).toEqual([]);

    const stored = await realApp.evaluate(({ localKeys, sessionKeys }) => ({
      local: Object.fromEntries(localKeys.map((key) => [key, localStorage.getItem(key)])),
      session: Object.fromEntries(sessionKeys.map((key) => [key, sessionStorage.getItem(key)])),
    }), { localKeys: Object.keys(local), sessionKeys: Object.keys(session) });
    expect(stored).toEqual({ local, session });
    expect(await realController()).toEqual(expectedController);
  });
}

test('the real app ignores a demo query and fetches no demo code', async ({ page }) => {
  const requests: string[] = [];
  page.on('request', (request) => requests.push(new URL(request.url()).pathname));
  await page.goto(`${appOrigin}/?demo=1`);
  await expect(page.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
  await expect(page.getByRole('button', { name: 'Settings', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Demo — sample data' })).toHaveCount(0);
  expect(requests).toContain(`/${release.files.javascript.path}`);
  expect(requests.filter((path) => /demo/i.test(path))).toEqual([]);
});

for (const block of [demoLoader, demoScript]) {
  test(`the demo reports a load failure and recovers on reload when ${block} is blocked`, async ({ page, request }) => {
    const options = { block };
    const observation = await watchDemo(page, options);
    await page.goto(demoOrigin);
    await expect(page).toHaveURL(`${demoOrigin}/`);
    await expect(page.getByRole('alert')).toHaveText('Demo could not load. Reload this page to try again.');
    await expect(page.getByRole('heading', { name: 'Demo — sample data' })).toBeVisible();
    await expect(page.getByRole('button', { name: /Settings|Open app home/ })).toHaveCount(0);
    expect(observation.blocked).toEqual([block]);
    expect((await request.get(`${demoOrigin}/sw.js`)).status()).toBe(404);
    options.block = '';
    await page.reload();
    await expectSampleHome(page);
    await expect(page.locator('#demo-startup')).toHaveCount(0);
    expect(observation.unexpected).toEqual([]);
  });
}

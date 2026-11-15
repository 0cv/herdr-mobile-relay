import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { APP_METADATA, APP_RELEASE, emulateStandalone, webRoot } from '../support/journey-device';
import { boot, closeSocket, commands, commandsForSocket, connectFedora, fedora, handshake, openAgent, server, setAutoCommands, socketCount } from '../support/journey-relay';

async function updateProgressPlan(page: Page): Promise<Record<string, unknown> | null> {
  try {
    return await page.evaluate(() => JSON.parse(sessionStorage.getItem('herdr_update_progress') || 'null'));
  } catch (error) {
    if (error instanceof Error && error.message.includes('Execution context was destroyed')) return null;
    throw error;
  }
}

test('loads a deployed phone app and preserves pending relay updates', async ({ page }) => {
  const deployedAssets = APP_METADATA.assets + 1;
  const availableUpdate = {
    state: 'available',
    current_version: '0.14.0',
    current_revision: 'a'.repeat(40),
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(12),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    checked_at: 124,
    can_install: true,
    mode: 'local',
  };
  await page.route('**/version.json?*', async (route) => {
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ version: APP_RELEASE, assets: deployedAssets }),
    });
  });
  await boot(page, [fedora]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.14.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  const loadUpdate = page.getByRole('button', { name: 'Load Update', exact: true }).first();
  await expect(loadUpdate).toBeEnabled();
  await loadUpdate.click();
  const dialog = page.getByRole('dialog', { name: 'Load Update' });
  // The pre-reload URL is already "/", so polling the URL alone can be
  // satisfied before the reload commits, leaving later evaluations racing the
  // context swap. Mark this document: only the reloaded one lacks the mark.
  await page.evaluate(() => {
    (window as unknown as { __herdrPreReload?: boolean }).__herdrPreReload = true;
  });
  const reloadRequest = page.waitForRequest((request) =>
    new URL(request.url()).searchParams.has('herdr_reload'));
  const reloadNavigation = page.waitForNavigation({ waitUntil: 'domcontentloaded' });
  await dialog.getByRole('button', { name: 'Load Update', exact: true }).click();

  const reloadUrl = new URL((await reloadRequest).url());
  expect(reloadUrl.pathname).toBe('/index.html');
  expect(reloadUrl.searchParams.get('herdr_reload'))
    .toMatch(new RegExp(`^${APP_RELEASE.replaceAll('.', '\\.')}-\\d+$`));
  await reloadNavigation;
  await page.waitForFunction(() =>
    !(window as unknown as { __herdrPreReload?: boolean }).__herdrPreReload);
  await expect.poll(() => updateProgressPlan(page)).toMatchObject({
    targetVersion: APP_RELEASE,
    relayIds: ['fedora'],
    startedRelayIds: ['fedora'],
    phoneAppRequired: true,
    phoneAcknowledged: false,
    phoneReloadAttempts: 2,
    phoneState: 'failed',
  });

  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.14.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });
  await expect.poll(async () => (await commandsForSocket(page, 0)).some(
    (command) => command.type === 'check_update' || command.type === 'install_update',
  )).toBe(true);
  const checkUpdate = (await commandsForSocket(page, 0)).find((command) => command.type === 'check_update');
  if (checkUpdate) {
    await server(page, 0, {
      type: 'command_result',
      request_id: checkUpdate.request_id,
      ok: true,
      phase: 'completed',
      data: { update: availableUpdate },
    });
  }
  await expect.poll(async () => (await commandsForSocket(page, 0)).some(
    (command) => command.type === 'install_update',
  )).toBe(true);
});

const STALE_ENTRY = '/builds/0.0.1-1-0123456789abcdef/index.html';

const STALE_DOCUMENT = '<!doctype html><html><head><script src="/manifest-loader.js"></script></head>'
  + '<body><div id="app"></div><p id="stale-build">stale build</p></body></html>';

for (const search of ['', '?herdr_reload=update-1'] as const) {
  test(`a Home Screen copy started on an outdated build${search ? ' after a reload' : ''} opens the deployed build`, async ({ page }) => {
    const deployed = JSON.parse(readFileSync(resolve(webRoot, 'version.json'), 'utf8')) as { entry: string; script: string };
    const documentRequests: string[] = [];
    page.on('request', (request) => {
      if (request.resourceType() === 'document') documentRequests.push(new URL(request.url()).pathname);
    });
    await page.route(`**${STALE_ENTRY}*`, (route) => route.fulfill({ contentType: 'text/html', body: STALE_DOCUMENT }));
    await boot(page, [], `${STALE_ENTRY}${search}#settings`);
    await expect(page.locator('#stale-build')).toHaveCount(0);
    await expect(page.locator('script[type="module"][src*="/assets/app-"]')).toHaveAttribute('src', deployed.script);
    await expect.poll(() => new URL(page.url()).pathname).toBe('/');
    expect(new URL(page.url()).searchParams.has('herdr_reload')).toBe(false);
    expect(new URL(page.url()).hash).toBe('#settings');
    expect(documentRequests).toEqual([STALE_ENTRY, '/index.html', deployed.entry]);
  });
}

test('an outdated build is not redirected again when the redirect lands on an outdated build', async ({ page }) => {
  await page.clock.install();
  const documentRequests: string[] = [];
  let versionRequests = 0;
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (request.resourceType() === 'document') documentRequests.push(path);
    if (path === '/version.json') versionRequests += 1;
  });
  await page.route(`**${STALE_ENTRY}*`, (route) => route.fulfill({ contentType: 'text/html', body: STALE_DOCUMENT }));
  await page.route((url) => url.pathname === '/index.html', (route) => route.fulfill({
    contentType: 'text/html',
    body: `<!doctype html><script>location.replace(${JSON.stringify(STALE_ENTRY)} + location.search)</script>`,
  }));
  await boot(page, [], STALE_ENTRY);
  await expect.poll(() => versionRequests).toBe(2);
  await page.clock.runFor(500);
  await expect(page.locator('#stale-build')).toHaveCount(1);
  expect(new URL(page.url()).searchParams.get('herdr_reload')).toMatch(/^stale-/u);
  expect(documentRequests).toEqual([STALE_ENTRY, '/index.html', STALE_ENTRY]);
});

test('finalizes the loaded phone with a cached legacy manifest bootstrap', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
  // The 0.20.8 bootstrap installs the manifest but emits no asset-ready flags.
  await page.route('**/manifest-loader.js', (route) => route.fulfill({
    contentType: 'application/javascript',
    body: `const setupToken = new URLSearchParams(location.hash.slice(1)).get('setup') || '';
const manifestLink = document.createElement('link');
manifestLink.rel = 'manifest';
manifestLink.href = navigator.standalone === false && setupToken.length >= 16 && setupToken.length <= 512
  ? 'setup.webmanifest'
  : 'manifest.webmanifest';
document.head.append(manifestLink);`,
  }));
  await page.addInitScript(({ metadata, relayIds }) => {
    localStorage.setItem('herdr_theme', 'light');
    sessionStorage.setItem('herdr_update_progress', JSON.stringify({
      targetVersion: metadata.version,
      relayIds,
      startedRelayIds: relayIds,
      appRelayId: '',
      phoneAppRequired: true,
      phoneTarget: metadata,
      phoneState: 'loading',
      phoneAcknowledged: false,
      phoneReloadAttempts: 1,
      startedAt: Date.now(),
    }));
  }, { metadata: APP_METADATA, relayIds: [fedora.id, mac.id] });
  await emulateStandalone(page);
  await boot(page, [fedora, mac], '/?herdr_reload=resume#settings');
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 0, { release_version: APP_RELEASE });
  await handshake(page, 1, { release_version: APP_RELEASE });

  const dialog = page.getByRole('dialog', { name: 'Update complete', exact: true });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('progressbar')).toHaveAttribute('value', '100');
  await dialog.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(page.getByText(`Phone app version ${APP_RELEASE}`, { exact: true })).toBeVisible();
  expect(await page.evaluate(() => ({
    relays: JSON.parse(localStorage.getItem('herdr_relays') || '[]'),
    theme: localStorage.getItem('herdr_theme'),
  }))).toEqual({ relays: [fedora, mac], theme: 'light' });
});

test('checks every self-updating relay automatically after connection', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
  await boot(page, [fedora, mac]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(2);
  const staleUpdate = {
    state: 'current',
    current_version: '0.0.1',
    current_revision: 'abc1234',
    upstream_version: '0.0.1',
    checked_at: 123,
    can_install: false,
    mode: 'local',
  };
  await handshake(page, 0, {
    release_version: '0.0.1',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: staleUpdate,
  });
  await handshake(page, 1, {
    release_version: '0.0.1',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: staleUpdate,
  });

  await expect.poll(async () => (await commands(page)).filter(
    (command) => command.type === 'check_update',
  ).length).toBe(2);
  for (const index of [0, 1]) {
    const check = (await commandsForSocket(page, index)).find(
      (command) => command.type === 'check_update',
    )!;
    await server(page, index, {
      type: 'command_result',
      request_id: check.request_id,
      ok: true,
      phase: 'confirmed',
      data: {
        update: {
          state: 'available',
          current_version: '0.0.1',
          current_revision: 'abc1234',
          available_version: APP_RELEASE,
          available_revision: 'f'.repeat(12),
          target_revision: 'f'.repeat(40),
          upstream_version: APP_RELEASE,
          checked_at: 124,
          can_install: true,
          mode: 'local',
        },
      },
    });
  }

  await page.getByRole('button', { name: 'Settings, relay update available' }).click();
  await expect(page.getByText(`Phone app is current at v${APP_RELEASE}.`)).toBeVisible();
  await expect(page.getByText('2 relay updates are available.')).toBeVisible();
  await expect(page.getByText(`Update v${APP_RELEASE} available`)).toHaveCount(2);
  await expect(page.getByRole('button', { name: 'Update Relays' })).toBeEnabled();
  await expect(page.getByRole('button', { name: /^Update (Fedora|Mac)/ })).toHaveCount(0);
});

test('keeps update controls steady while app and relay checks are in flight', async ({ page }) => {
  const availableUpdate = {
    state: 'available',
    current_version: '0.12.0',
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(12),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    checked_at: 124,
    can_install: true,
    mode: 'local',
  };
  await boot(page, [fedora]);
  await setAutoCommands(page, false);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { update: availableUpdate });

  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(`Phone app is current at v${APP_RELEASE}.`)).toBeVisible();
  await expect(page.getByText(`Update v${APP_RELEASE} available`)).toBeVisible();

  const checkButton = page.locator('button.update-check-button');
  const aboutCard = page.locator('section.card').filter({ has: checkButton });
  await checkButton.scrollIntoViewIfNeeded();
  const beforeButton = await checkButton.boundingBox();
  const beforeCard = await aboutCard.boundingBox();
  expect(beforeButton).not.toBeNull();
  expect(beforeCard).not.toBeNull();

  await page.route('**/version.json?*', async (route) => {
    await page.waitForTimeout(250);
    await route.continue();
  });
  await checkButton.click();
  await expect(checkButton).toBeDisabled();
  await expect(checkButton).toHaveAttribute('aria-busy', 'true');
  await expect(checkButton).toHaveText('Check for Updates');
  expect(await checkButton.evaluate((button) => getComputedStyle(button).opacity)).toBe('1');

  const duringButton = await checkButton.boundingBox();
  const duringCard = await aboutCard.boundingBox();
  expect(duringButton).not.toBeNull();
  expect(duringCard).not.toBeNull();
  // Browser engines may adjust page scroll when the focused control changes.
  expect(duringButton!.x).toBeCloseTo(beforeButton!.x, 1);
  expect(duringButton!.y - duringCard!.y).toBeCloseTo(beforeButton!.y - beforeCard!.y, 1);
  expect(duringButton!.width).toBeCloseTo(beforeButton!.width, 1);
  expect(duringButton!.height).toBeCloseTo(beforeButton!.height, 1);
  expect(duringCard!.x).toBeCloseTo(beforeCard!.x, 1);
  expect(duringCard!.width).toBeCloseTo(beforeCard!.width, 1);
  expect(duringCard!.height).toBeCloseTo(beforeCard!.height, 1);

  const check = (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'check_update')
    .at(-1)!;
  await server(page, 0, {
    type: 'command_result',
    request_id: check.request_id,
    ok: true,
    phase: 'confirmed',
    data: { update: availableUpdate },
  });
  await expect(checkButton).toBeEnabled();
  await expect(checkButton).toHaveText('Check for Updates');
});

test('confirms and tracks one relay update through its verified reconnect', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '0.7.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'available',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      available_revision: 'f'.repeat(12),
      target_revision: 'f'.repeat(40),
      checked_at: 123,
      can_install: true,
      mode: 'local',
    },
  });

  await page.getByRole('button', { name: 'Settings, relay update available' }).click();
  await expect(page.getByText('Update v0.8.0 available')).toBeVisible();
  await expect(page.getByText(`Phone app version ${APP_RELEASE}`)).toBeVisible();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Relays' });
  await expect(dialog).toContainText('Update Fedora first');
  await setAutoCommands(page, false);
  await dialog.getByRole('button', { name: 'Start Update' }).click();
  const progress = page.getByRole('dialog', { name: 'Updating Herdr' });

  await expect.poll(async () =>
    (await commands(page)).filter((command) => command.type === 'install_update').length).toBe(1);
  const install = (await commands(page)).find((command) => command.type === 'install_update')!;
  expect(install).toMatchObject({
    expected_version: '0.8.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
    protocol: 3,
  });
  await server(page, 0, {
    type: 'update_status',
    update: {
      state: 'scheduled',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      target_revision: 'f'.repeat(40),
      mode: 'local',
    },
  });
  await server(page, 0, {
    type: 'command_result',
    request_id: install.request_id,
    ok: true,
    phase: 'scheduled',
    data: {
      update: {
        state: 'scheduled',
        current_version: '0.7.0',
        current_revision: 'abc1234',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
        mode: 'local',
      },
    },
  });
  await expect(progress).toContainText('Update scheduled…');

  await server(page, 0, {
    type: 'update_status',
    update: {
      state: 'restarting',
      current_version: '0.7.0',
      current_revision: 'abc1234',
      available_version: '0.8.0',
      target_revision: 'f'.repeat(40),
      mode: 'local',
    },
  });
  await expect(progress).toContainText('Restarting relay…');
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1, {
    host: 'fedora',
    release_version: '0.8.0',
    revision: 'f'.repeat(12),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: '0.8.0',
      current_revision: 'f'.repeat(12),
      checked_at: 124,
      can_install: false,
      mode: 'local',
    },
  });

  const complete = page.getByRole('dialog', { name: 'Update complete' });
  await expect(complete).toContainText('Updated to v0.8.0');
  await expect(complete.getByRole('button', { name: 'Close' })).toBeVisible();
});

test('resumes fleet progress and updates the second relay automatically', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
  const previousVersion = '0.12.0';
  const availableUpdate = {
    state: 'available',
    current_version: previousVersion,
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(40),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    can_install: true,
    mode: 'plugin',
  };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });
  await handshake(page, 1, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  const confirmation = page.getByRole('dialog', { name: 'Update Relays' });
  await expect(confirmation).toContainText('Update Fedora first');
  await confirmation.getByRole('button', { name: 'Start Update' }).click();

  let progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText('Starting update…');
  await expect(progress).toContainText('Verify release');
  await expect(progress).toContainText('Install relay');
  await expect(progress).toContainText('Reconnect');
  await expect(progress.getByRole('button', { name: 'Finish Later' })).toHaveCount(0);
  expect((await commandsForSocket(page, 0)).find((command) => command.type === 'install_update')).toMatchObject({
    expected_version: APP_RELEASE,
    expected_revision: 'f'.repeat(40),
  });

  await server(page, 0, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'preparing' },
  });
  await expect(progress).toContainText('Verifying release…');
  await server(page, 0, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'restarting' },
  });
  await expect(progress).toContainText('Restarting relay…');
  await closeSocket(page, 0);
  await expect.poll(() => socketCount(page)).toBe(3);
  await handshake(page, 2, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });

  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'install_update')).toBe(true);
  progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText(`Updated to v${APP_RELEASE}`);
  await expect(progress).toContainText('Mac');
  await expect(progress.getByRole('button', { name: 'Update', exact: true })).toHaveCount(0);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect.poll(() => socketCount(page)).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });
  await handshake(page, 1, {
    release_version: previousVersion,
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  progress = page.getByRole('dialog', { name: 'Updating Herdr' });
  await expect(progress).toContainText('updates relays one at a time');
  await server(page, 1, {
    type: 'update_status',
    update: { ...availableUpdate, state: 'installing' },
  });
  await expect(progress).toContainText('Installing relay…');
  await handshake(page, 1, {
    release_version: APP_RELEASE,
    revision: 'f'.repeat(40),
    capabilities: ['directory_browser', 'self_update'],
    update: {
      state: 'succeeded',
      current_version: APP_RELEASE,
      current_revision: 'f'.repeat(40),
      can_install: false,
      mode: 'plugin',
    },
  });

  progress = page.getByRole('dialog', { name: 'Update complete' });
  await expect(progress).toContainText('2 of 2 update items complete');
  await progress.getByRole('button', { name: 'Close' }).click();
  await expect(progress).toHaveCount(0);
});

test('keeps a failed relay online and offers an explicit close action', async ({ page }) => {
  const availableUpdate = {
    state: 'available',
    current_version: '0.12.0',
    current_revision: 'abc1234',
    available_version: APP_RELEASE,
    available_revision: 'f'.repeat(40),
    target_revision: 'f'.repeat(40),
    upstream_version: APP_RELEASE,
    can_install: true,
    mode: 'plugin',
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await setAutoCommands(page, true);
  await handshake(page, 0, {
    release_version: '0.12.0',
    capabilities: ['directory_browser', 'self_update'],
    update: availableUpdate,
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Relays' }).click();
  await page.getByRole('dialog', { name: 'Update Relays' }).getByRole('button', { name: 'Start Update' }).click();
  await expect(page.getByRole('dialog', { name: 'Updating Herdr' })).toBeVisible();
  await server(page, 0, {
    type: 'update_status',
    update: {
      ...availableUpdate,
      state: 'failed',
      error: 'Release signature did not match; the current relay is still running.',
    },
  });

  const progress = page.getByRole('dialog', { name: 'Update needs attention' });
  await expect(progress.getByRole('alert')).toContainText('Release signature did not match; the current relay is still running.');
  await expect(progress.getByRole('button', { name: 'Close' })).toBeVisible();
  await progress.getByRole('button', { name: 'Close' }).click();
  await expect(progress).toHaveCount(0);
  await expect(page.getByRole('img', { name: 'Fedora relay connected' })).toBeVisible();
});

test('does not poll the app origin after its deployment target is loaded', async ({ page }) => {
  const versionRequests: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/version.json') versionRequests.push(request.url());
  });
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: APP_RELEASE,
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'f'.repeat(40),
      state: 'succeeded',
      target_version: APP_RELEASE,
    },
  });
  await page.waitForTimeout(300);
  const settledRequestCount = versionRequests.length;

  await page.waitForTimeout(2_200);

  expect(versionRequests).toHaveLength(settledRequestCount);
});

test('confirms deployment when an authorized relay has the upstream app bundle', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '9.0.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    update: {
      state: 'current',
      current_version: '9.0.0',
      upstream_version: '9.0.0',
    },
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'f'.repeat(40),
      state: 'idle',
    },
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(`Version 9.0.0 is released, but this app origin still serves ${APP_RELEASE}.`)).toBeVisible();
  await page.getByRole('button', { name: 'Update Herdr' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Herdr' });
  await expect(dialog).toContainText('Publish the phone app from Fedora');
  await dialog.getByRole('button', { name: 'Start Update' }).click();

  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'deploy_app_update')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'deploy_app_update')).toMatchObject({
    expected_version: '9.0.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
  });
  const publishing = 'Publishing v9.0.0 from Fedora and waiting for this app origin to update. This can take up to two minutes.';
  for (const state of ['scheduled', 'deploying']) {
    await server(page, 0, {
      type: 'app_deploy_status',
      app_deploy: {
        configured: true,
        origin,
        project: 'herdr-app',
        branch: 'main',
        revision: 'f'.repeat(40),
        state,
        target_version: '9.0.0',
      },
    });
    await expect(page.getByText(publishing)).toBeVisible();
  }
});

test('deploys a Pages app before updating its owner relay', async ({ page }) => {
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    release_version: '8.0.0',
    revision: 'abc1234',
    capabilities: ['directory_browser', 'self_update', 'app_deploy'],
    update: {
      state: 'available',
      current_version: '8.0.0',
      current_revision: 'abc1234',
      available_version: '9.0.0',
      available_revision: 'f'.repeat(12),
      target_revision: 'f'.repeat(40),
      upstream_version: '9.0.0',
      can_install: true,
      mode: 'plugin',
    },
    app_deploy: {
      configured: true,
      origin,
      project: 'herdr-app',
      branch: 'main',
      revision: 'abc1234',
      state: 'idle',
    },
  });

  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Update Herdr' }).click();
  const dialog = page.getByRole('dialog', { name: 'Update Herdr' });
  await expect(dialog).toContainText('Publish the phone app first, then update Fedora');
  await dialog.getByRole('button', { name: 'Start Update' }).click();

  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'install_update')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'install_update')).toMatchObject({
    expected_version: '9.0.0',
    expected_revision: 'f'.repeat(40),
    expected_origin: origin,
  });
  expect((await commands(page)).some((command) => command.type === 'deploy_app_update')).toBe(false);
});

// A deploy since the page loaded removes the old build's lazily loaded files.
async function removeLazyFile(page: Page, file: string, { newerBuild, missingRequests = Infinity }: {
  newerBuild: boolean;
  missingRequests?: number;
}) {
  const deploy = { fileRequests: 0, versionRequests: 0, reloads: [] as string[] };
  await page.route(`**/assets/${file}-*.js`, (route) => {
    deploy.fileRequests += 1;
    return deploy.fileRequests <= missingRequests ? route.fulfill({ status: 404, body: '' }) : route.continue();
  });
  await page.route('**/version.json?*', (route) => {
    deploy.versionRequests += 1;
    return route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ version: APP_RELEASE, assets: APP_METADATA.assets + (newerBuild ? 1 : 0) }),
    });
  });
  page.on('request', (request) => {
    if (new URL(request.url()).searchParams.has('herdr_reload')) deploy.reloads.push(request.url());
  });
  return deploy;
}

function deployedAppAgents(status = 'working') {
  return {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status, project: 'Deployed app', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  };
}

async function connectDeployedApp(page: Page, status = 'working') {
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, deployedAppAgents(status));
}

async function markCurrentDocument(page: Page) {
  await page.evaluate(() => { (window as any).__beforeReload = true; });
}

async function waitForReloadedDocument(page: Page) {
  await expect.poll(async () => {
    try {
      return await page.evaluate(() => !(window as any).__beforeReload && window.__relaySockets?.length === 1);
    } catch {
      return false;
    }
  }).toBe(true);
}

test('opens the deployed build when a deploy removed the conversation view', async ({ page }) => {
  const deploy = await removeLazyFile(page, 'ConversationHistory', { newerBuild: true, missingRequests: 1 });
  await boot(page, [fedora]);
  await connectDeployedApp(page);
  await markCurrentDocument(page);
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  await waitForReloadedDocument(page);
  expect(new URL(deploy.reloads[0]).searchParams.get('herdr_reload')).toContain(APP_RELEASE);
  await connectDeployedApp(page);
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByRole('alert').filter({ hasText: 'could not be loaded' })).toHaveCount(0);
  expect(deploy.fileRequests).toBe(2);
});

test('opens Terminal when the conversation view cannot load without a deploy', async ({ page }) => {
  await page.clock.install();
  const deploy = await removeLazyFile(page, 'ConversationHistory', { newerBuild: false });
  await boot(page, [fedora]);
  await connectDeployedApp(page);
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Deployed app' })).toBeVisible();

  // Asking for Conversation explicitly shows the error, and agent updates do not check for a deploy again.
  const checksBefore = deploy.versionRequests;
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'Conversation history could not be loaded' })).toBeVisible();
  for (const status of ['idle', 'working', 'idle', 'working', 'idle']) await server(page, 0, deployedAppAgents(status));
  await page.clock.runFor(500);
  expect(deploy.versionRequests).toBe(checksBefore + 1);

  // The error belongs to that opening: opening the agent again from Search falls back to Terminal.
  await page.getByRole('button', { name: 'Search all agents' }).click();
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Deployed app');
  await jump.getByRole('button', { name: /Deployed app/ }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Deployed app' })).toBeVisible();
  expect(deploy.versionRequests).toBe(checksBefore + 2);
  expect(deploy.reloads).toEqual([]);
});

test('opens Terminal instead of reloading again once the automatic reloads are used up', async ({ page }) => {
  const deploy = await removeLazyFile(page, 'ConversationHistory', { newerBuild: true });
  await page.addInitScript((version) => {
    sessionStorage.setItem('herdr_app_reload_target', version);
    sessionStorage.setItem('herdr_app_reload_attempts', JSON.stringify({ [version]: 2 }));
  }, APP_RELEASE);
  await boot(page, [fedora]);
  await connectDeployedApp(page);
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Deployed app' })).toBeVisible();
  expect(deploy.reloads).toEqual([]);
});

for (const stage of ['version check', 'deploy check'] as const) {
  test(`does not reload a removed conversation view when the user leaves during the ${stage}`, async ({ page }) => {
    await page.clock.install();
    await page.route('**/assets/ConversationHistory-*.js', (route) => route.fulfill({ status: 404, body: '' }));
    let releaseHeld: () => void = () => {};
    const held = new Promise<void>((resolve) => { releaseHeld = resolve; });
    let holdFrom = Infinity;
    let versionRequests = 0;
    let versionResponses = 0;
    await page.route('**/version.json?*', async (route) => {
      versionRequests += 1;
      if (versionRequests >= holdFrom) await held;
      await route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({ version: APP_RELEASE, assets: APP_METADATA.assets + 1 }),
      });
      versionResponses += 1;
    });
    const reloads: string[] = [];
    page.on('request', (request) => {
      if (new URL(request.url()).searchParams.has('herdr_reload')) reloads.push(request.url());
    });
    await boot(page, [fedora]);
    await connectDeployedApp(page);
    await expect.poll(() => versionResponses).toBeGreaterThan(0);
    // The version check is the first request after the failed load; the deploy check comes next.
    holdFrom = versionRequests + (stage === 'version check' ? 1 : 2);
    await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
    await expect.poll(() => versionRequests).toBe(holdFrom);
    await page.goBack();
    await expect(page.getByRole('button', { name: 'Open Deployed app on Fedora' })).toBeVisible();
    releaseHeld();
    // The deploy check reads the version twice before it decides to reload.
    const lastRequest = stage === 'version check' ? holdFrom : holdFrom + 1;
    await expect.poll(() => versionResponses).toBe(lastRequest);
    await page.clock.runFor(500);
    expect(versionRequests).toBe(lastRequest);
    expect(reloads).toEqual([]);
  });
}

test('opens the deployed build when a deploy removed the launch form', async ({ page }) => {
  const deploy = await removeLazyFile(page, 'LaunchView', { newerBuild: true, missingRequests: 1 });
  await connectFedora(page);
  await markCurrentDocument(page);
  await page.getByRole('button', { name: 'Start agent' }).click();
  await waitForReloadedDocument(page);
  await handshake(page, 0);
  await expect(page.getByRole('heading', { name: 'Start Agent', level: 2 })).toBeVisible();
  expect(deploy.fileRequests).toBe(2);
});

test('opens the deployed build when a deploy removed the terminal input controls', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  const deploy = await removeLazyFile(page, 'ManualTerminalInput', { newerBuild: true, missingRequests: 1 });
  await boot(page, [fedora]);
  // The Keyboard button is offered only while the composer cannot send, such as for a pane that needs inspection.
  await connectDeployedApp(page, 'blocked');
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  await markCurrentDocument(page);
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await waitForReloadedDocument(page);
  await connectDeployedApp(page, 'blocked');
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Terminal text', exact: true })).toBeVisible();
  expect(deploy.fileRequests).toBe(2);
});

test('keeps an unsaved draft instead of reloading when a deploy removed the terminal input controls', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  const deploy = await removeLazyFile(page, 'ManualTerminalInput', { newerBuild: true });
  await boot(page, [fedora]);
  await connectDeployedApp(page, 'blocked');
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  const prompt = page.getByLabel('Prompt', { exact: true });
  await prompt.fill('draft this phone has not saved');
  await expect(page.getByText(/^Not saved on this phone/)).toBeVisible();
  await markCurrentDocument(page);
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await expect(page.getByText(/^A newer app version is ready but was not loaded automatically\. A draft is not saved on this phone/)).toBeVisible();
  await page.clock.runFor(500);
  expect(deploy.reloads).toEqual([]);
  expect(await page.evaluate(() => (window as any).__beforeReload)).toBe(true);
  await expect(prompt).toHaveValue('draft this phone has not saved');
});

test('keeps an unconfirmed terminal send fenced instead of reloading when a deploy removed the conversation view', async ({ page }) => {
  await page.clock.install();
  const button = (name: string) => page.getByRole('button', { name, exact: true });
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  const deploy = await removeLazyFile(page, 'ConversationHistory', { newerBuild: true });
  await connectFedora(page, { capabilities: ['conversation_history', 'manual_terminal_input'] });
  await server(page, 0, deployedAppAgents('blocked'));
  await button('Open Deployed app on Fedora').click();
  await button('Keyboard').click();
  await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
  const read = (await commands(page)).findLast(command => command.type === 'read_pane');
  expect(read).toBeTruthy();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read!.request_id, target: read!.target, content: '> ', format: 'plain', no_echo: false });
  await expect(button('Enter')).toBeEnabled();
  await setAutoCommands(page, false);
  await page.getByLabel('Terminal text', { exact: true }).fill('sent at most once');
  await button('Send text').click();
  const attempt = (await commands(page)).findLast(command => command.type === 'send_terminal_text')!;
  await server(page, 0, { type: 'command_result', request_id: attempt.request_id, ok: true, phase: 'dispatched_unknown' });
  await setAutoCommands(page, true);
  await expect(page.getByText(/^Delivery is uncertain\./)).toBeVisible();
  await markCurrentDocument(page);
  await button('Conversation history').click();
  const held = 'A newer app version is ready but was not loaded automatically. A send to a terminal is not confirmed yet';
  await expect(page.getByRole('alert').filter({ hasText: held })).toBeVisible();
  await page.clock.runFor(500);
  expect(deploy.reloads).toEqual([]);
  expect(await page.evaluate(() => (window as any).__beforeReload)).toBe(true);

  // Loading the update stays an explicit choice that names what it forgets.
  await page.getByRole('button', { name: /^Settings, phone app update available/ }).click();
  await page.getByRole('button', { name: 'Load Update', exact: true }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Load Update' });
  await expect(dialog.getByRole('alert')).toHaveText('A send to a terminal is not confirmed yet, and loading the app update forgets it. Check the terminal before sending again.');
  await dialog.getByRole('button', { name: 'Load Update Anyway', exact: true }).click();
  await expect.poll(() => deploy.reloads.length).toBeGreaterThan(0);
  expect(new URL(deploy.reloads[0]).searchParams.get('herdr_reload')).toContain(APP_RELEASE);
});

test('explains why a finished deploy did not reload over an unsaved draft', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await page.route('**/version.json?*', (route) => route.fulfill({
    contentType: 'application/json',
    body: JSON.stringify({ version: APP_RELEASE, assets: APP_METADATA.assets + 1 }),
  }));
  const reloads: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).searchParams.has('herdr_reload')) reloads.push(request.url());
  });
  await boot(page, [fedora]);
  const origin = new URL(page.url()).origin;
  const appDeploy = { configured: true, origin, project: 'herdr-app', branch: 'main', revision: 'f'.repeat(40) };
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'app_deploy'], app_deploy: { ...appDeploy, state: 'idle' } });
  await server(page, 0, deployedAppAgents());
  await page.getByRole('button', { name: 'Open Deployed app on Fedora' }).click();
  const prompt = page.getByLabel('Prompt', { exact: true });
  await prompt.fill('draft this phone has not saved');
  await expect(page.getByText(/^Not saved on this phone/)).toBeVisible();
  await expect(page.getByRole('button', { name: /^Settings, phone app update available/ })).toBeVisible();
  await markCurrentDocument(page);
  await server(page, 0, { type: 'app_deploy_status', app_deploy: { ...appDeploy, state: 'succeeded', target_version: APP_RELEASE } });
  await expect(page.getByText(/^A newer app version is ready but was not loaded automatically\. A draft is not saved on this phone/)).toBeVisible();
  await page.clock.runFor(500);
  expect(reloads).toEqual([]);
  expect(await page.evaluate(() => (window as any).__beforeReload)).toBe(true);
  await expect(prompt).toHaveValue('draft this phone has not saved');
});

test('opens the deployed build when a deploy removed the question controls', async ({ page }) => {
  const interaction = {
    id: 'deploy-question', kind: 'single_select', question: 'Which build should run?',
    options: [{ index: 0, label: 'Deployed', description: '' }, { index: 1, label: 'Previous', description: '' }],
    submit_label: 'Submit', can_go_back: false, question_index: 1, question_total: 1,
  };
  const askQuestion = async () => {
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0);
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Question app', agent: 'codex' }] });
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Which build?', content_fingerprint: 'question' });
    await server(page, 0, {
      type: 'pane_delta', pane_id: 'w1:p1', format: 'plain', base_fingerprint: 'question', content_fingerprint: 'question',
      segments: [], attention_kind: 'question', interaction,
    });
  };
  const deploy = await removeLazyFile(page, 'QuestionForm', { newerBuild: true, missingRequests: 1 });
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Question app', agent: 'codex' });
  await markCurrentDocument(page);
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Which build?', content_fingerprint: 'question' });
  await server(page, 0, {
    type: 'pane_delta', pane_id: 'w1:p1', format: 'plain', base_fingerprint: 'question', content_fingerprint: 'question',
    segments: [], attention_kind: 'question', interaction,
  });
  await waitForReloadedDocument(page);
  await askQuestion();
  await expect(page.getByRole('radio', { name: 'Deployed' })).toBeVisible();
  expect(deploy.fileRequests).toBe(2);
});

import { expect, test } from '@playwright/test';
import { emulateIphoneSafari, emulateStandalone } from '../support/journey-device';
import { boot, closeSocket, commands, commandsForSocket, fedora, handshake, server, setAutoCommands, socketCount, type RelayFixture } from '../support/journey-relay';

test('guides an unpairable relay without ever dialing it', async ({ page }) => {
  await page.clock.install();
  // A relay key that is not a usable relay key, with no enrolled credential:
  // nothing can be presented, so the app must not spend a dial finding out.
  await boot(page, [{ ...fedora, token: 'truncated-key' }]);

  await expect(page.getByText('Fedora needs pairing. Import a device invitation link.')).toBeVisible();
  expect(await socketCount(page)).toBe(0);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.clock.runFor(80);
  expect(await socketCount(page)).toBe(0);

  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByText('This computer needs pairing. Import a device invitation link, or remove and add the relay.')).toBeVisible();
});

test('keeps device verification modal until native authentication succeeds', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_require_device_unlock', 'true');
    localStorage.setItem('herdr_device_unlock_credential', 'AQ');
    Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class {} });
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: {
        get: () => new Promise((resolve) => {
          Object.assign(window, { __resolveDeviceVerification: () => resolve({}) });
        }),
      },
    });
  });
  await boot(page, [fedora]);

  const unlockDialog = page.getByRole('dialog', { name: 'Unlock Herdr' });
  await expect(unlockDialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(unlockDialog).toBeVisible();
  expect(await socketCount(page)).toBe(0);

  // The session behind this dialog stays connected while locked, so the gate
  // has to hide the page rather than dim it: a translucent scrim would leave
  // live agent names and status changes legible before verification.
  const backdrop = await unlockDialog.evaluate(
    (element) => getComputedStyle(element, '::backdrop').backgroundColor,
  );
  const alpha = /^rgba?\((?:[^,]+,){3}\s*([\d.]+)\s*\)$/.exec(backdrop)?.[1];
  expect({ backdrop, alpha: alpha === undefined ? 1 : Number(alpha) }).toEqual({ backdrop, alpha: 1 });

  await page.evaluate(() => (window as any).__resolveDeviceVerification());
  await expect(unlockDialog).toBeHidden();
  await expect.poll(() => socketCount(page)).toBe(1);
});

test('keeps an iOS setup link unredeemed for Home Screen installation', async ({ page }) => {
  await page.clock.install();
  const setupHash = '#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation&relay=wss%3A%2F%2Frelay-fedora.example.com';
  await emulateIphoneSafari(page);
  await boot(page, [], `/${setupHash}`);

  // The bootstrap key is one-use and the installed copy has its own storage:
  // a Safari tab that dialled would spend it before the Home Screen app opens.
  await expect(page.getByRole('status').filter({ hasText: 'Add Herdr to the iPhone or iPad Home Screen' })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'This browser tab keeps the setup link unused' })).toBeVisible();
  expect(await page.locator('link[rel="manifest"]').getAttribute('href')).toBe('/setup.webmanifest');
  expect(await page.evaluate(() => location.hash)).toBe(setupHash);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[0]))
    .toMatchObject({
      label: 'Fedora Workstation',
      url: 'wss://relay-fedora.example.com',
      token: '0123456789abcdef0123456789abcdef',
    });
  await page.clock.runFor(500);
  expect(await socketCount(page)).toBe(0);

  // Focus and reconnect requests must not spend the key either.
  await page.evaluate(() => {
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('focus'));
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Waiting for the Home Screen app' })).toBeVisible();
  await page.getByRole('button', { name: 'Reconnect All' }).click();
  await page.clock.runFor(500);
  expect(await socketCount(page)).toBe(0);
});

test('an iOS browser tab without pairing material still connects', async ({ page }) => {
  await emulateIphoneSafari(page);
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
});

test('imports quick setup and merges agents from multiple relays', async ({ page }) => {
  await emulateStandalone(page, { navigatorStandalone: true });
  await boot(
    page,
    [],
    '/#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation&relay=wss%3A%2F%2Frelay-fedora.example.com',
  );
  await expect(page.getByRole('button', { name: 'Activity history' }).locator('svg')).toBeVisible();
  await expect.poll(() => socketCount(page)).toBe(1);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[0]))
    .toMatchObject({
      label: 'Fedora Workstation',
      url: 'wss://relay-fedora.example.com',
      token: '0123456789abcdef0123456789abcdef',
    });
  expect(await page.evaluate(() => location.hash)).toBe('');
  expect(await page.locator('link[rel="manifest"]').getAttribute('href')).toBe('/manifest.webmanifest');

  await page.evaluate(() => {
    location.hash = '#setup=abcdef0123456789abcdef0123456789&label=Mac&relay=wss%3A%2F%2Fmac.example';
  });
  await expect.poll(() => socketCount(page)).toBe(3);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]')[1]))
    .toMatchObject({
      label: 'Mac',
      url: 'wss://mac.example',
      token: 'abcdef0123456789abcdef0123456789',
    });
  expect(await page.evaluate(() => location.hash)).toBe('');
  await page.evaluate(() => {
    const relays = JSON.parse(localStorage.getItem('herdr_relays') || '[]');
    localStorage.setItem('herdr_relays', JSON.stringify(relays.map((relay: RelayFixture, index: number) => ({
      ...relay,
      url: `ws://127.0.0.1:${8871 + index}`,
      token: '',
    }))));
    localStorage.removeItem('herdr_device_auth_v1');
  });
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(2);
  const base = 0;
  await handshake(page, base);
  await handshake(page, base + 1);
  expect((await commandsForSocket(page, base)).some((command) =>
    command.type === 'register_app_origin')).toBe(false);
  expect((await commandsForSocket(page, base + 1)).some((command) =>
    command.type === 'register_app_origin')).toBe(false);
  await server(page, base, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app', agent: 'codex' }] });
  await server(page, base + 1, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Mac app', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  const headerBox = await page.getByRole('banner').boundingBox();
  const connectionBox = await page.getByRole('img', { name: /relays connected/ }).boundingBox();
  const settingsBox = await page.getByRole('button', { name: 'Settings' }).boundingBox();
  expect(headerBox && connectionBox && settingsBox).toBeTruthy();
  const leadingInset = connectionBox!.x + connectionBox!.width / 2 - headerBox!.x;
  const trailingInset = headerBox!.x + headerBox!.width - settingsBox!.x - settingsBox!.width / 2;
  expect(Math.abs(leadingInset - trailingInset)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Open Fedora app on Fedora Workstation' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Open Mac app on Mac' })).toBeVisible();
});

test('rechecks Herdr terminal compatibility and wraps actual failures on mobile', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    herdr_status: {
      server_version: '0.9.0',
      generation: 1,
      features: {
        direct_terminal: { state: 'unknown', reason: 'not_checked', generation: 1 },
        'pane.read': { state: 'unknown', reason: 'reconnect_required', generation: 1 },
        'client_shell.endpoint': { state: 'unknown', reason: 'not_advertised', generation: 1 },
      },
    },
  });
  await page.getByRole('button', { name: /Settings/ }).click();
  await expect(page.getByText(/Herdr server: 0\.9\.0/)).toBeVisible();
  await expect(page.getByText('Terminal reads: Rechecking after Herdr reconnect')).toBeVisible();
  await expect(page.getByText(/Could not check|Server upgrade needed|Server feature unavailable/)).toHaveCount(0);

  await server(page, 0, {
    type: 'herdr_status',
    status: {
      server_version: '0.9.0',
      generation: 2,
      features: {
        direct_terminal: { state: 'unknown', reason: 'not_checked', generation: 2 },
        'pane.read': { state: 'unknown', reason: 'timeout', generation: 2 },
        'workspace.move_block': { state: 'unsupported', reason: 'method_not_supported', generation: 2 },
      },
    },
  });
  const warning = page.getByRole('status').filter({ hasText: 'Terminal reads: Could not check' });
  await expect(warning).toHaveText('Terminal reads: Could not check · Workspace group reorder: Server upgrade needed');
  await expect(warning).toHaveCSS('white-space', 'normal');
  expect(await warning.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);

  await server(page, 0, {
    type: 'herdr_status',
    status: {
      server_version: '0.9.0',
      generation: 3,
      features: {
        'pane.read': { state: 'supported', reason: 'recognized_validation_refusal', generation: 3 },
        'workspace.move_block': { state: 'supported', reason: 'recognized_validation_refusal', generation: 3 },
      },
    },
  });
  await expect(page.locator('.herdr-feature-warning')).toHaveCount(0);
});

test('reconnects and blocks mutations for an incompatible relay protocol', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    protocol: 1,
    version: 'old',
    capabilities: ['attention_classification', 'clear_activities', 'directory_browser', 'structured_questions', 'slash_commands'],
  });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Old relay', agent: 'codex', options: ['Approve once', 'Deny'] }] });
  await page.getByRole('button', { name: 'Settings, relay update needs attention' }).click();
  await expect(page.getByText(/Relay outdated/)).toBeVisible();
  await page.getByRole('button', { name: 'How to update Fedora' }).click();
  const updateHelp = page.getByRole('dialog', { name: 'Update Fedora' });
  await expect(updateHelp).toContainText('herdr plugin install 0cv/herdr-mobile-relay');
  await updateHelp.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Remove Fedora' }).click();
  const removeDialog = page.getByRole('dialog', { name: 'Remove Fedora?' });
  await expect(removeDialog).toContainText('You will need its setup link or connection details to add it again.');
  await removeDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(page.getByText(fedora.url, { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Approve once' }).click();
  await expect(page.getByRole('status').filter({ hasText: /protocol v1/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  await closeSocket(page, 0);
  await expect.poll(() => socketCount(page)).toBe(2);
});

test('replaces a half-open socket immediately when a sleeping phone resumes', async ({ page }) => {
  await page.clock.install();
  await boot(page, [fedora], '/', { answerHealthPings: false });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Resume app on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: 'cached terminal output', content_fingerprint: 'resume-cache-1',
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('cached terminal output');
  const cachedScreen = await terminal.locator('.term-screen').innerHTML();

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.clock.runFor(5_100);
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Agent unavailable' })).toBeHidden();
  await expect(page.getByRole('log')).toContainText('cached terminal output');
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();

  // A half-open socket answers nothing, including refocus reads.
  await setAutoCommands(page, false);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(() => socketCount(page), { timeout: 12_000 }).toBe(2);
  await setAutoCommands(page, true);
  await handshake(page, 1, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await expect.poll(async () =>
    (await commandsForSocket(page, 1)).some((command) => command.type === 'read_pane')).toBe(true);
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  await server(page, 1, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: 'fresh output after resume', content_fingerprint: 'resume-cache-2',
  });
  await expect(page.getByRole('log')).toContainText('fresh output after resume');
  await server(page, 1, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume app', agent: 'codex' }],
  });
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  await expect(page.getByRole('main', { name: 'Terminal for Resume app' })).toBeVisible();
});

test('replaces a half-open socket when the browser reports a network handoff', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'connection', {
      configurable: true,
      value: new EventTarget(),
    });
  });
  await boot(page, [fedora], '/', { answerHealthPings: false });
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const refreshesBefore = (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'refresh_agents').length;

  await setAutoCommands(page, false);
  await page.evaluate(() => {
    (navigator as Navigator & { connection: EventTarget }).connection.dispatchEvent(new Event('change'));
  });

  await expect.poll(async () =>
    (await commandsForSocket(page, 0))
      .filter((command) => command.type === 'refresh_agents').length).toBe(refreshesBefore + 1);
  // The old socket deliberately never answers the probe. The foreground
  // health deadline replaces it without waiting for WebSocket/TCP timeouts.
  await expect.poll(() => socketCount(page), { timeout: 12_000 }).toBe(2);
});

test('followup remote tokenless relay refuses plaintext', async ({ page }) => {
  await page.clock.install();
  await boot(page, [{ ...fedora, url: 'wss://remote.example' }]);
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Settings', level: 2, exact: true })).toBeVisible();
  expect(await socketCount(page)).toBe(0);
  await page.getByRole('button', { name: 'Reconnect All' }).click();
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.clock.runFor(80);
  expect(await socketCount(page)).toBe(0);
  expect(await commands(page)).toHaveLength(0);
});

test('followup direct TLS device rejection stops retrying', async ({ page }) => {
  await page.clock.install();
  const relay = { id: 'rejected-direct', label: 'Rejected direct', url: 'wss://relay.example.invalid', token: '', paired: true };
  await page.addInitScript(id => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: {
    [id]: { kind: 'credential', id: 'synthetic-credential', deviceId: 'synthetic-device', version: 1, secret: 'A'.repeat(43), role: 'controller', locale: 'en', issuedAt: Date.now() },
  } })), relay.id);
  await boot(page, [relay]);
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'e2ee_client_hello').length).toBe(1);
  const before = await page.evaluate(() => localStorage.getItem('herdr_device_auth_v1'));
  await closeSocket(page, 0, 4401);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByText('This computer refused this device. Import a new invitation link, or remove and add the relay.', { exact: true })).toBeVisible();
  await page.evaluate(() => {
    window.dispatchEvent(new Event('focus'));
    window.dispatchEvent(new Event('online'));
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.clock.runFor(1_500);
  expect(await socketCount(page)).toBe(1);
  expect((await commands(page)).map(command => command.type)).toEqual(['e2ee_client_hello']);
  expect(await page.evaluate(saved => localStorage.getItem('herdr_device_auth_v1') === saved, before)).toBe(true);
});

test('followup raw development close stays recoverable', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await closeSocket(page, 0, 4401);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByText('This computer refused this device. Import a new invitation link, or remove and add the relay.', { exact: true })).toHaveCount(0);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_secret', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

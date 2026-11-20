import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '@playwright/test';
import { createServer, encryptServerFinish } from '../support/e2ee-server';
import { APP_METADATA, emulateIphoneSafari, emulateStandalone, setPageVisibility } from '../support/journey-device';
import { boot, lastCommand, server, setAutoCommands, socketCount } from '../support/journey-relay';
import { deferred } from '../support/relay';
import { directSetupLink, gatewayInvitationLink, qrPixels } from '../support/setup-qr';

declare global {
  interface Window {
    __scannerTracks: MediaStreamTrack[];
    __scannerPolicyViolations: string[];
  }
}

async function fakeCamera(page: Page, source: 'denied' | 'live' | 'setup-qr-direct' | 'setup-qr-invitation' | 'other-qr') {
  await emulateStandalone(page, { navigatorStandalone: true });
  const frame = source === 'live' || source === 'denied' ? undefined : qrPixels(source);
  await page.addInitScript(({ source, frame }) => {
    window.__scannerTracks = [];
    function cameraStream() {
      if (!frame) {
        if (source === 'denied') throw new DOMException('Fake camera denial', 'NotAllowedError');
        return new AudioContext().createMediaStreamDestination().stream;
      }
      const canvas = document.createElement('canvas');
      canvas.width = frame.width;
      canvas.height = frame.height;
      const context = canvas.getContext('2d')!;
      const image = new ImageData(new Uint8ClampedArray(frame.data), frame.width, frame.height);
      context.putImageData(image, 0, 0);
      const stream = canvas.captureStream(10);
      const timer = window.setInterval(() => {
        if (!stream.active) window.clearInterval(timer);
        else context.putImageData(image, 0, 0);
      }, 100);
      return stream;
    }
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        async getUserMedia() {
          const stream = cameraStream();
          window.__scannerTracks.push(...stream.getTracks());
          return stream;
        },
      },
    });
  }, { source, frame: frame && { ...frame, data: Array.from(frame.data) } });
}

const trigger = (page: Page) => page.getByRole('button', { name: 'Scan QR Code' });
const scanner = (page: Page) => page.getByRole('dialog', { name: 'Scan Setup Code' });
const trackStates = (page: Page) => page.evaluate(() => window.__scannerTracks.map(track => track.readyState));
const savedRelays = (page: Page) => page.evaluate(() => JSON.parse(localStorage.getItem('herdr_relays') || '[]'));
const isScannerChunk = (url: string) => /^\/assets\/SetupScanner-\d+\.js$/.test(new URL(url).pathname);
const mac = { label: 'Mac', url: 'wss://mac.example', token: 'abcdef0123456789abcdef0123456789' };

async function openSettings(page: Page) {
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Relays' })).toBeVisible();
}

async function enrollMac(page: Page) {
  const authentication = { kind: 'invitation', id: 'bootstrap', version: 1, secret: Buffer.from(mac.token).toString('base64url') } as const;
  const hello = await lastCommand(page, 'e2ee_client_hello', 0);
  expect(hello).toMatchObject({ auth_kind: authentication.kind, auth_id: authentication.id, auth_version: authentication.version });
  const relay = await createServer({ nonce: String(hello.nonce), public_key: String(hello.public_key) }, authentication);
  await server(page, 0, relay.hello);
  await lastCommand(page, 'e2ee', 0);
  const secret = Buffer.alloc(32, 4).toString('base64url');
  const finish = await encryptServerFinish(relay.sendKey, {
    type: 'e2ee_server_finish', version: 2,
    device_id: 'device-mac', credential_id: 'credential-mac', credential_version: 1,
    credential_secret: secret, role: 'controller', locale: 'en',
  });
  await server(page, 0, JSON.parse(finish));
  await expect.poll(() => savedRelays(page)).toMatchObject([{ ...mac, paired: true }]);
  const [{ id }] = await savedRelays(page);
  expect(await page.evaluate(relayId => JSON.parse(localStorage.getItem('herdr_device_auth_v1')!).relays[relayId], id)).toMatchObject({
    kind: 'credential', id: 'credential-mac', deviceId: 'device-mac', secret, role: 'controller', invitationId: 'bootstrap',
  });
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    window.__scannerPolicyViolations = [];
    document.addEventListener('securitypolicyviolation', event => {
      window.__scannerPolicyViolations.push(`${event.violatedDirective}: ${event.blockedURI}`);
    });
  });
});

test.afterEach(async ({ page }) => {
  if (!page.isClosed() && page.url() !== 'about:blank') {
    expect(await page.evaluate(() => window.__scannerPolicyViolations)).toEqual([]);
  }
});

test('loads the scanner on demand and pairs without leaving home or Settings', async ({ page }) => {
  const chunkRequests: string[] = [];
  page.on('request', request => {
    if (isScannerChunk(request.url())) chunkRequests.push(request.url());
  });
  await fakeCamera(page, 'setup-qr-direct');
  await boot(page);
  await setAutoCommands(page, false);
  await expect(trigger(page)).toBeVisible();
  expect(chunkRequests).toEqual([]);
  const homeUrl = page.url();
  const fetchedChunk = page.waitForResponse(response => response.ok() && isScannerChunk(response.url()));
  await trigger(page).click();
  await fetchedChunk;
  await expect.poll(() => savedRelays(page)).toMatchObject([mac]);
  await expect.poll(() => socketCount(page)).toBe(1);
  expect(await page.evaluate(() => window.__relaySockets[0])).toMatchObject({ url: expect.stringContaining(mac.url) });
  await expect.poll(() => trackStates(page)).toEqual(['ended']);
  await expect(scanner(page)).toHaveCount(0);
  await expect(page).toHaveURL(homeUrl);
  await enrollMac(page);

  await openSettings(page);
  const settingsUrl = page.url();
  expect(settingsUrl).toMatch(/#settings$/);
  await trigger(page).click();
  await expect.poll(() => trackStates(page)).toEqual(['ended', 'ended']);
  await expect(scanner(page)).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Relays' })).toBeVisible();
  await expect(page).toHaveURL(settingsUrl);
  expect(await savedRelays(page)).toMatchObject([mac]);
});

test('retries the scanner after going offline without reloading the page', async ({ context, page }) => {
  await fakeCamera(page, 'denied');
  await boot(page);
  const originalDocument = await page.evaluateHandle(() => document);
  await context.setOffline(true);
  const failedChunk = page.waitForEvent('requestfailed', request => isScannerChunk(request.url()));
  await trigger(page).click();
  const firstUrl = new URL((await failedChunk).url());
  await expect(page.getByText('The scanner could not be loaded. Check the connection and try again.')).toBeVisible();
  await expect(scanner(page)).toHaveCount(0);

  await context.setOffline(false);
  const fetchedChunk = page.waitForResponse(response => response.ok() && isScannerChunk(response.url()));
  await trigger(page).click();
  await expect(scanner(page)).toBeVisible();
  const retryUrl = new URL((await fetchedChunk).url());
  expect(retryUrl.pathname).toBe(firstUrl.pathname);
  expect(retryUrl.search).not.toBe(firstUrl.search);
  expect(await page.evaluate(original => original === document, originalDocument)).toBe(true);
});

test('refuses an unrelated QR code without visiting it or adding a relay', async ({ context, page }) => {
  const requests: string[] = [];
  const openedPages: Page[] = [];
  context.on('request', request => {
    if (new URL(request.url()).hostname === 'example.com') requests.push(request.url());
  });
  context.on('page', opened => openedPages.push(opened));
  await fakeCamera(page, 'other-qr');
  await boot(page);
  const href = page.url();
  await trigger(page).click();
  await expect(scanner(page).getByRole('alert')).toHaveText('This is not a Herdr setup code.');
  await expect(scanner(page)).toBeVisible();
  await expect(page).toHaveURL(href);
  expect(await savedRelays(page)).toEqual([]);
  expect(await socketCount(page)).toBe(0);
  expect(requests).toEqual([]);
  expect(openedPages).toEqual([]);
});

for (const expireOnRead of [1, 2]) {
  test(`refuses an invitation that expires on credential read ${expireOnRead}`, async ({ page }) => {
    const expiresAt = Number(new URLSearchParams(new URL(gatewayInvitationLink).hash.slice(1)).get('invite_expires'));
    await page.addInitScript(({ expiresAt, expireOnRead }) => {
      let now = expiresAt - 1;
      let reads = 0;
      Date.now = () => now;
      const getItem = Storage.prototype.getItem;
      Storage.prototype.getItem = function (key) {
        if (key === 'herdr_device_auth_v1' && document.querySelector('#setup-scanner[open]') && ++reads === expireOnRead) {
          now = expiresAt;
        }
        return getItem.call(this, key);
      };
    }, { expiresAt, expireOnRead });
    await fakeCamera(page, 'setup-qr-invitation');
    await boot(page);
    const storage = await page.evaluate(() => Object.entries(localStorage));
    await trigger(page).click();
    await expect(scanner(page).getByRole('alert')).toHaveText('This invitation has expired. Create a new one and scan it.');
    expect(await page.evaluate(() => Date.now())).toBe(expiresAt);
    expect(await page.evaluate(() => Object.entries(localStorage))).toEqual(storage);
    expect(await socketCount(page)).toBe(0);
    await scanner(page).getByRole('button', { name: 'Cancel' }).click();
    await openSettings(page);
    await expect(page.getByText('Test Workstation', { exact: true })).toHaveCount(0);
  });
}

test('keeps scanning without saving a computer or invitation when relay storage is full', async ({ page }) => {
  await fakeCamera(page, 'setup-qr-invitation');
  await boot(page);
  await openSettings(page);
  const storage = await page.evaluate(() => Object.entries(localStorage));
  const readCount = await page.evaluateHandle(() => {
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key === 'herdr_relays') throw new DOMException('Relay storage is full', 'QuotaExceededError');
      setItem.call(this, key, value);
    };
    let frames = 0;
    const getImageData = CanvasRenderingContext2D.prototype.getImageData;
    CanvasRenderingContext2D.prototype.getImageData = function (...args) {
      frames += 1;
      return getImageData.apply(this, args);
    };
    return () => frames;
  });
  await trigger(page).click();
  await expect(scanner(page).getByRole('alert')).toHaveText('This code could not be used. Print or create a new one and scan it.');
  const framesAtFailure = await readCount.evaluate(count => count());
  await expect.poll(() => readCount.evaluate(count => count())).toBeGreaterThan(framesAtFailure);
  expect(await trackStates(page)).toEqual(['live']);
  expect(await page.evaluate(() => Object.entries(localStorage))).toEqual(storage);
  expect(await socketCount(page)).toBe(0);
  await scanner(page).getByRole('button', { name: 'Cancel' }).click();
  await expect(page.getByText('Test Workstation', { exact: true })).toHaveCount(0);
});

test('explains denied permission and still pairs by setup link', async ({ page }) => {
  await fakeCamera(page, 'denied');
  await boot(page);
  await setAutoCommands(page, false);
  await trigger(page).press('Enter');
  await expect(scanner(page).getByRole('alert')).toContainText('Camera access was denied.');
  await expect(scanner(page).getByRole('alert')).toContainText('You can still open the setup link, or add the relay in Settings.');
  expect(await savedRelays(page)).toEqual([]);
  await scanner(page).getByRole('button', { name: 'Cancel' }).press('Enter');
  await expect(scanner(page)).toHaveCount(0);
  await expect(trigger(page)).toBeFocused();
  await page.evaluate(hash => { location.hash = hash; }, new URL(directSetupLink).hash);
  await enrollMac(page);
});

test('stops the camera when the app is hidden without restarting on return', async ({ page }) => {
  await fakeCamera(page, 'live');
  await boot(page);
  await trigger(page).click();
  await expect(scanner(page)).toBeVisible();
  await expect.poll(() => trackStates(page)).toEqual(['live']);
  await setPageVisibility(page, 'hidden');
  await expect.poll(() => trackStates(page)).toEqual(['ended']);
  await expect(scanner(page)).toHaveCount(0);
  await setPageVisibility(page, 'visible');
  await expect(trigger(page)).toBeVisible();
  expect(await trackStates(page)).toEqual(['ended']);
});

for (const dismissal of ['hidden', 'pagehide', 'freeze', 'hidden with verification']) {
  test(`cancels a delayed scanner open on ${dismissal}`, async ({ page }) => {
    await fakeCamera(page, 'live');
    await boot(page);
    const verifies = dismissal === 'freeze' || dismissal === 'hidden with verification';
    if (verifies) {
      await page.evaluate(() => {
        localStorage.setItem('herdr_require_device_unlock', 'true');
        localStorage.setItem('herdr_device_unlock_credential', 'AQ');
        Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class {} });
        Object.defineProperty(navigator, 'credentials', {
          configurable: true,
          value: { get: async () => Reflect.get(window, '__allowDeviceVerification') ? {} : null },
        });
      });
    }

    const held = deferred();
    await page.route('**/assets/SetupScanner-*.js', async route => {
      await held.promise;
      await route.continue();
    });
    const requested = page.waitForRequest(request => isScannerChunk(request.url()));
    await trigger(page).click();
    const chunk = await requested;
    if (dismissal.startsWith('hidden')) {
      await setPageVisibility(page, 'hidden');
      await setPageVisibility(page, 'visible');
    } else {
      await page.evaluate(event => {
        (event === 'freeze' ? document : window).dispatchEvent(new Event(event));
      }, dismissal);
    }
    const unlock = page.getByRole('dialog', { name: 'Unlock Herdr' });
    if (verifies) await expect(unlock).toBeVisible();
    held.resolve();
    await page.evaluate(async url => {
      await import(url);
      await new Promise(requestAnimationFrame);
    }, chunk.url());

    await expect(page.locator('#setup-scanner')).toHaveCount(0);
    expect(await trackStates(page)).toEqual([]);
    if (verifies) {
      const unlockButton = unlock.getByRole('button', { name: 'Unlock' });
      await expect(unlockButton).toBeFocused();
      await page.evaluate(() => Reflect.set(window, '__allowDeviceVerification', true));
      await unlockButton.click();
      await expect(unlock).toHaveCount(0);
      await expect(scanner(page)).toHaveCount(0);
      expect(await trackStates(page)).toEqual([]);
    }
    await trigger(page).click();
    await expect(scanner(page)).toBeVisible();
    await expect.poll(() => trackStates(page)).toEqual(['live']);
  });
}

test('hides QR scanning in an iPhone browser tab on home and in Settings', async ({ page }) => {
  await emulateIphoneSafari(page);
  await boot(page);
  await expect(page.getByRole('heading', { name: 'Herdr Mobile Relay' })).toBeVisible();
  await expect(trigger(page)).toHaveCount(0);
  await openSettings(page);
  await expect(trigger(page)).toHaveCount(0);
});

test('serves the exact MIT notice from the pinned decoder', async ({ page, request }) => {
  expect(JSON.parse(readFileSync('node_modules/qr/package.json', 'utf8')).version).toBe('0.7.2');
  await page.goto('/');
  const notice = readFileSync('node_modules/qr/LICENSE-MIT');
  const response = await request.get('/licenses/qr.txt');
  expect(response.status()).toBe(200);
  expect(await response.body()).toEqual(notice);
  const chunk = await request.get(`/assets/SetupScanner-${APP_METADATA.assets}.js`);
  expect(chunk.status()).toBe(200);
  expect((await chunk.text()).startsWith(`/*!\n${notice.toString('utf8')}*/\n`)).toBe(true);
});

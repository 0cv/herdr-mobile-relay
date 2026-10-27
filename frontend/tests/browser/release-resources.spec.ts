import { devices, expect, test, type BrowserContext } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pagesHeaders } from '../../scripts/pages-policy';

const root = resolve(process.env.HERDR_WEB_ROOT || 'dist');
const notFoundBody = readFileSync(resolve(root, '404.html'), 'utf8');
const release = JSON.parse(readFileSync(resolve(root, 'release.json'), 'utf8')) as {
  build: string;
  entry: string;
  files: Record<string, { path: string; sha256: string }>;
};

test('canonical modules and styles mount the descriptor identity with browser policy acceptance', async ({ page, request }) => {
  const failures: string[] = [];
  page.on('pageerror', (error) => failures.push(error.message));
  await page.goto('/');
  await expect(page.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
  await expect(page.locator('#herdr-load-recovery')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.dataset.herdrCssReady)).toBe('1');
  expect(failures).toEqual([]);

  for (const [kind, file] of Object.entries(release.files)) {
    const contentType = { entry: 'text/html', javascript: 'text/javascript', stylesheet: 'text/css' }[kind];
    for (const encoding of ['identity', 'br']) {
      const response = await request.get(`/${file.path}`, { headers: { 'Accept-Encoding': encoding } });
      expect(response.status()).toBe(200);
      expect(response.headers()['content-type']).toContain(contentType);
      expect(response.headers()['cache-control']).toBe('no-cache');
      expect(createHash('sha256').update(await response.body()).digest('hex')).toBe(file.sha256);
      if (encoding === 'br') expect(response.headers()['content-encoding']).toBe('br');
      if (kind !== 'entry') {
        const conditional = await request.get(`/${file.path}`, { headers: {
          'Accept-Encoding': encoding,
          'If-None-Match': response.headers().etag,
        } });
        expect(conditional.status()).toBe(304);
        expect((await conditional.body()).length).toBe(0);
        expect(conditional.headers()['cache-control']).toBe('no-cache');
      }
    }
  }
});

test('root and index bootstrap keep same-origin query and fragment navigation', async ({ page }) => {
  for (const path of ['/', '/index.html']) {
    const entryResponse = page.waitForResponse((response) => new URL(response.url()).pathname === release.entry
      && response.request().resourceType() === 'document');
    await page.goto(`${path}?fixture=preserved#settings`);
    expect([200, 304]).toContain((await entryResponse).status());
    await expect(page.locator('header[data-app-build]')).toHaveAttribute('data-app-build', release.build);
    const selected = new URL(page.url());
    expect(selected.origin).toBe('http://127.0.0.1:4173');
    expect(selected.pathname).toBe('/');
    expect(selected.searchParams.get('fixture')).toBe('preserved');
    expect(selected.hash).toBe('#settings');
  }
});

test('removed application resources and direct old entries return static 404s requiring revalidation', async ({ page, request }) => {
  for (const path of [
    '/assets/removed.js', '/assets/removed.css', '/assets/ConversationHistory-0.js',
    '/assets/attachment-hash.worker-removed.js', '/builds/0.0.0-0-0000000000000000/index.html',
  ]) {
    for (const encoding of ['identity', 'br']) {
      const response = await request.get(path, { headers: { 'Accept-Encoding': encoding, 'If-None-Match': '*' } });
      expect(response.status()).toBe(404);
      expect(response.headers()['cache-control']).toBe('no-store');
      expect(response.headers()['content-type']).toContain('text/html');
      expect(response.headers().etag).toBeUndefined();
      expect(await response.text()).toBe(notFoundBody);
      expect(await response.text()).not.toContain('herdr-bootstrap.js');
    }
  }
  const response = await page.goto('/builds/0.0.0-0-0000000000000000/index.html');
  expect(response?.status()).toBe(404);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Page not found');
  await expect(page.getByRole('link', { name: 'Open Herdr' })).toHaveAttribute('href', '/');
  await expect(page.locator('script, #app')).toHaveCount(0);
});

test('the error document offers a same-origin home link without executing the app', async ({ page }) => {
  const response = await page.goto('/404.html');
  expect(response?.headers()['cache-control']).toBe('no-store');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Page not found');
  await expect(page.getByRole('link', { name: 'Open Herdr' })).toHaveAttribute('href', '/');
  await expect(page.locator('script, #app')).toHaveCount(0);
  await page.getByRole('link', { name: 'Open Herdr' }).click();
  await expect(page.locator('[data-app-build]')).toHaveAttribute('data-app-build', release.build);
});

test('normal HTTP caching revalidates unchanged bytes and replaces a repaired wrong response', async ({ playwright, browserName }) => {
  let body = '<!doctype html><script src="/herdr-bootstrap.js"></script>\n';
  let missing = false;
  const requests: Array<{ conditional: string; status: number }> = [];
  const assetPolicy = pagesHeaders().match(/\/assets\/\*\n {2}Cache-Control: ([^\n]+)/)?.[1];
  expect(assetPolicy).toBe('no-cache');
  const server = createServer((request, response) => {
    if (request.url !== '/assets/app-test.js') {
      response.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' }).end('<!doctype html><title>Cache fixture</title>');
      return;
    }
    const conditional = request.headers['if-none-match'] || '';
    const etag = `"${createHash('sha256').update(body).digest('hex')}"`;
    const status = missing ? 404 : conditional === etag ? 304 : 200;
    requests.push({ conditional, status });
    response.writeHead(status, {
      'Cache-Control': missing ? 'no-store' : assetPolicy,
      'Content-Type': missing || body.startsWith('<') ? 'text/html' : 'text/javascript',
      ...(!missing ? { ETag: etag } : {}),
    }).end(status === 304 ? undefined : missing ? notFoundBody : body);
  });
  const profile = await mkdtemp(join(tmpdir(), 'herdr-revalidation-browser-'));
  let context: BrowserContext | undefined;
  try {
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    if (!address || typeof address === 'string') throw new Error('Cache fixture did not bind a loopback port');
    context = await playwright[browserName].launchPersistentContext(profile, {
      ...(browserName === 'chromium' ? devices['Pixel 7'] : devices['iPhone 15']),
      headless: true,
      serviceWorkers: 'block',
    });
    const page = await context.newPage();
    await page.goto(`http://127.0.0.1:${address.port}/`);
    const fetchAsset = () => page.evaluate(async () => {
      const response = await fetch('/assets/app-test.js', { cache: 'default' });
      return {
        status: response.status,
        body: await response.text(),
        mime: response.headers.get('Content-Type'),
        cache: response.headers.get('Cache-Control'),
      };
    });
    expect((await fetchAsset()).body).toBe(body);
    const poisonETag = `"${createHash('sha256').update(body).digest('hex')}"`;
    await expect.poll(async () => {
      expect((await fetchAsset()).body).toBe(body);
      return requests.at(-1);
    }).toEqual({ conditional: poisonETag, status: 304 });
    body = 'export const repaired = true;';
    expect(await fetchAsset()).toEqual({ status: 200, body, mime: 'text/javascript', cache: 'no-cache' });
    expect(requests.at(-1)).toEqual({ conditional: poisonETag, status: 200 });
    await expect.poll(async () => {
      expect((await fetchAsset()).body).toBe(body);
      return requests.at(-1)?.status;
    }).toBe(304);
    expect(requests.at(-1)?.conditional).not.toBe(poisonETag);
    missing = true;
    expect(await fetchAsset()).toEqual({ status: 404, body: notFoundBody, mime: 'text/html', cache: 'no-store' });
    expect(requests.at(-1)?.status).toBe(404);
    expect(requests.at(-1)?.conditional).not.toBe('');
  } finally {
    await context?.close();
    await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    await rm(profile, { recursive: true, force: true });
  }
});

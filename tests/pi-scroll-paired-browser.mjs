import { chromium, expect } from '../frontend/node_modules/@playwright/test/index.mjs';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

const root = process.env.HERDR_P1_FIXTURE_ROOT;
if (!root || process.env.PI_OFFLINE !== '1') throw new Error('owned offline qualification required');
const { url, token } = JSON.parse(await readFile(join(root, 'browser-ready.json'), 'utf8'));
if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(url)) throw new Error('localhost relay required');
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ viewport: { width: 412, height: 915 }, serviceWorkers: 'block' });
const page = await context.newPage();
const record = { kind: 'genuine encrypted paired browser → production relay → real Pi', engine: 'chromium', result: 'failed', console: [], frames: [] };
page.on('console', message => record.console.push(message.text()));
page.on('pageerror', error => record.console.push(String(error)));
page.on('websocket', socket => {
  socket.on('framesent', frame => record.frames.push({ direction: 'sent', payload: typeof frame.payload === 'string' ? frame.payload : frame.payload.toString('base64') }));
  socket.on('framereceived', frame => record.frames.push({ direction: 'received', payload: typeof frame.payload === 'string' ? frame.payload : frame.payload.toString('base64') }));
});
await context.route('**/*', route => {
  const target = new URL(route.request().url());
  return target.hostname === '127.0.0.1' || target.hostname === 'localhost' ? route.continue() : route.abort('blockedbyclient');
});
try {
  await page.goto(`${url}/#setup=${token}&label=P1%20native&relay=${encodeURIComponent(url.replace('http:', 'ws:') + '/ws')}`);
  await expect(page.getByRole('button', { name: /^Open .* on P1 native$/ }).first()).toBeVisible();
  await page.getByRole('button', { name: /^Open .* on P1 native$/ }).first().click();
  const output = page.getByRole('log', { name: 'Agent terminal output' });
  const up = page.getByRole('button', { name: 'Scroll agent up', exact: true });
  const down = page.getByRole('button', { name: 'Scroll agent down', exact: true });
  await expect(up).toBeEnabled();
  const markers = async () => [...(await output.innerText()).matchAll(/P1 TEST USER \[(\d{3})\]/g)].map(match => Number(match[1]));
  await expect.poll(async () => (await markers()).length).toBeGreaterThan(0);
  record.before = await markers();
  record.upStarted = new Date().toISOString();
  await up.click();
  await expect.poll(async () => (await markers())[0]).toBeLessThan(record.before[0]);
  record.afterUp = await markers();
  record.upObserved = new Date().toISOString();
  await expect(down).toBeEnabled();
  record.downStarted = new Date().toISOString();
  await down.click();
  await expect.poll(async () => (await markers())[0]).toBeGreaterThan(record.afterUp[0]);
  record.afterDown = await markers();
  record.downObserved = new Date().toISOString();
  await page.getByLabel('Pi scroll selection', { exact: true }).selectOption('local');
  await expect(up).toBeDisabled();
  await output.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await page.getByLabel('Pi scroll selection', { exact: true }).selectOption('auto');
  await expect(up).toBeEnabled();
  const auth = await page.evaluate(() => JSON.parse(localStorage.getItem('herdr_device_auth_v1') || '{}'));
  if (!Object.values(auth.relays || {}).some(value => value.kind === 'credential' && value.role === 'controller')) throw new Error('browser did not enroll a disposable controller credential');
  if (!record.frames.length || record.frames.some(frame => /"type":"scroll_agent"/.test(frame.payload))) throw new Error('missing encrypted wire capture or plaintext button command');
  record.result = 'passed';
} catch (error) {
  record.error = String(error.stack || error);
  record.body = await page.locator('body').innerText().catch(() => 'body unavailable');
  await page.screenshot({ path: join(root, 'logs', 'paired-browser-failure.png') }).catch(() => {});
  process.exitCode = 1;
} finally {
  await context.close();
  await browser.close();
  await writeFile(join(root, 'logs', 'paired-browser.json'), JSON.stringify(record, null, 2), { mode: 0o600 });
  await writeFile(join(root, 'browser-done.json'), JSON.stringify({ result: record.result }), { mode: 0o600 });
}

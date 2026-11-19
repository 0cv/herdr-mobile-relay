import { chromium, expect } from '@playwright/test';
import { DEMO_TIME_MS } from '../src/demo/scene.ts';

const origin = 'http://127.0.0.1:4174';
await expect.poll(async () => {
  const response = await fetch(origin, { signal: AbortSignal.timeout(1_000) }).catch(() => null);
  return response?.status;
}, { message: 'Demo server should be ready on loopback', timeout: 10_000 }).toBe(200);

const browser = await chromium.launch();
try {
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    deviceScaleFactor: 2,
    isMobile: true,
    hasTouch: true,
    locale: 'en-US',
    timezoneId: 'UTC',
    colorScheme: 'dark',
    reducedMotion: 'reduce',
  });
  const page = await context.newPage();
  await page.clock.setFixedTime(DEMO_TIME_MS);
  await page.goto(origin);

  await expect(page.locator('html')).toHaveAttribute('data-theme', 'nord');
  await expect(page.getByRole('banner').getByRole('heading', { name: 'Demo — sample data', exact: true })).toBeVisible();
  const agents = page.getByRole('main', { name: 'Agents' });
  for (const name of ['Atlas', 'Birch']) {
    const computer = agents.getByRole('button', { name, exact: true });
    await expect(computer).toBeVisible();
    await expect(computer).toHaveAttribute('aria-pressed', 'true');
  }
  for (const waiting of ['Approval required', 'Plan question']) {
    await expect(agents.getByText(waiting, { exact: true })).toBeVisible();
  }
  await page.evaluate(async () => {
    const faces = await document.fonts.load('16px "Herdr Nerd Symbols"', '⚡');
    if (!faces.length) throw new Error('The Herdr Nerd Symbols font is missing.');
    await document.fonts.ready;
  });
  process.stdout.write(await page.screenshot({
    type: 'jpeg', quality: 90, scale: 'device', animations: 'disabled', caret: 'hide',
  }));
} finally {
  await browser.close();
}

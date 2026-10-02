import { expect, test, type Page } from '@playwright/test';
import { fixtureRelay, resumeFixtureInit, type FixtureConfig } from './resume-fixture.mjs';

const CACHE = 'herdr_last_known_session_v1';
async function boot(page: Page, overrides: Partial<FixtureConfig> = {}) {
  const config: FixtureConfig = {
    relays: [fixtureRelay(1, 'wss')], seed: 321, latencyMs: [10, 11],
    faults: { delayRefreshMs: 1_500 }, ...overrides,
  };
  await page.addInitScript(resumeFixtureInit, config);
  await page.goto('/');
  await page.waitForFunction(() => Boolean((window as any).__resumeFixture?.ready()));
  await expect(page.locator('article.agent-card')).toHaveCount(1);
}
async function enable(page: Page) {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('switch', { name: 'Keep an Encrypted Last-known Summary' }).check();
  await expect.poll(() => page.evaluate((key) => sessionStorage.getItem(key) !== null, CACHE)).toBe(true);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
}
async function discard(page: Page) {
  await page.evaluate(() => (window as any).__resumeFixture.prepareDiscard());
  await page.goto('/');
  await page.waitForFunction(() => Boolean((window as any).__resumeFixture?.ready()));
}

test('B2 default-off writes no summary; opt-in retains ciphertext only', async ({ page }) => {
  await boot(page);
  expect(await page.evaluate((key) => sessionStorage.getItem(key), CACHE)).toBeNull();
  await enable(page);
  const raw = await page.evaluate((key) => sessionStorage.getItem(key), CACHE);
  expect(raw).toBeTruthy();
  expect(raw).not.toContain('fixture-ws-1');
  expect(raw).not.toContain('/home/fixture-private/project');
  expect(raw).not.toContain(fixtureRelay(1, 'wss').secret);
});

test('B2 same-tab restoration renders read-only summary before correlated inventory, never a live card', async ({ page }) => {
  await boot(page); await enable(page); await discard(page);
  const summary = page.locator('[data-last-known]');
  await expect(summary).toBeVisible();
  await expect(summary).toContainText('reconnecting; read-only');
  await expect(summary.locator('button, a, article.agent-card, .agent-open')).toHaveCount(0);
  expect(await summary.innerText()).not.toContain('/home/fixture-private/project');
  await expect(page.locator('article.agent-card')).toHaveCount(0);
  await expect(page.locator('article.agent-card')).toHaveCount(1);
  await expect(summary).toHaveCount(0);
});

test('B2 opt-out and Forget leave credentials paired and cannot restore old summaries', async ({ page }) => {
  await boot(page); await enable(page);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const before = await page.evaluate(() => localStorage.getItem('herdr_device_auth_v1'));
  await page.getByRole('button', { name: 'Forget last-known data' }).click();
  await expect.poll(() => page.evaluate((key) => sessionStorage.getItem(key), CACHE)).toBeNull();
  expect(await page.evaluate(() => localStorage.getItem('herdr_device_auth_v1'))).toBe(before);
  await page.getByRole('switch', { name: 'Keep an Encrypted Last-known Summary' }).uncheck();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await discard(page);
  await expect(page.locator('[data-last-known]')).toHaveCount(0);
  await expect(page.locator('article.agent-card')).toHaveCount(1);
  expect(await page.evaluate((key) => sessionStorage.getItem(key), CACHE)).toBeNull();
});

test('B2 cancelled unlock removes summary DOM from the first render', async ({ page }) => {
  await boot(page, { lock: { delayMs: 10, cancel: false } });
  await enable(page);
  await page.evaluate(() => {
    (window as any).__resumeFixture.setUnlock({ delayMs: 10, cancel: true });
    (window as any).__resumeFixture.prepareDiscard();
  });
  await page.goto('/');
  await page.waitForFunction(() => Boolean((window as any).__resumeFixture?.ready()));
  // During the verification gate, no summary or live-agent text is in the DOM.
  await expect(page.locator('#unlock-dialog')).toBeVisible();
  await expect(page.locator('[data-last-known]')).toHaveCount(0);
});

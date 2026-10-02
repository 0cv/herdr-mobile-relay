import { expect, test, type Page } from '@playwright/test';
import { fixtureRelay, resumeFixtureInit, type FixtureConfig } from './resume-fixture.mjs';

const CHUNK = '**/assets/workspace-tools-*.js';
const CACHE = 'herdr_last_known_session_v1';
async function boot(page: Page) {
  const config: FixtureConfig = { relays: [fixtureRelay(1, 'wss')], workspaceTools: true,
    seed: 321, latencyMs: [10, 11], faults: { delayRefreshMs: 1_500 } };
  await page.addInitScript(resumeFixtureInit, config);
  await page.goto('/');
  await expect(page.locator('article.agent-card')).toHaveCount(1);
}
async function toolsRetry(page: Page) {
  const retry = page.getByRole('button', { name: 'Retry workspace tools' });
  if (await retry.isVisible()) await retry.click();
}
const commands = (page: Page) => page.evaluate(() => (window as any).__resumeFixture.toolCommands() as string[]);

test('B2 blocked workspace chunk is absent from eager summary/Settings paths and cannot revive a cancelled dialog', async ({ page }) => {
  let requests = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route(CHUNK, async (route) => { requests++; await gate; await route.continue(); });
  await boot(page);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('switch', { name: 'Keep an Encrypted Last-known Summary' }).check();
  await expect.poll(() => page.evaluate((key) => sessionStorage.getItem(key) !== null, CACHE)).toBe(true);
  await page.evaluate(() => (window as any).__resumeFixture.prepareDiscard());
  await page.goto('/');
  await expect(page.locator('[data-last-known]')).toBeVisible();
  expect(requests).toBe(0);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Forget last-known data' })).toBeEnabled();
  await page.getByRole('button', { name: 'Forget last-known data' }).click();
  await page.getByRole('switch', { name: 'Keep an Encrypted Last-known Summary' }).uncheck();
  await expect.poll(() => page.evaluate((key) => sessionStorage.getItem(key), CACHE)).toBeNull();
  expect(requests).toBe(0);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(page.locator('article.agent-card')).toHaveCount(1);
  await page.locator('.agent-open').first().click();
  await page.getByRole('button', { name: 'Manage agent', exact: true }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Opening workspace tools' })).toBeVisible();
  await expect.poll(() => requests).toBe(1);
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  const before = await commands(page);
  release();
  await expect.poll(() => page.evaluate(() => performance.getEntriesByType('resource')
    .some((entry) => /workspace-tools-.*\.js/.test(entry.name)))).toBe(true);
  await expect(page.locator('#manage-agent-dialog')).toHaveCount(0);
  expect(await commands(page)).toEqual(before);
});

test('B2 deferred surfaces load on demand; inspector custom properties agree with packaged CSS', async ({ page }) => {
  await boot(page);
  await page.getByRole('button', { name: 'Manage workspaces', exact: true }).click();
  await toolsRetry(page);
  await expect(page.getByRole('heading', { name: 'Workspaces', exact: true })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Herdr workspaces' })).toContainText('fixture-ws-1');
  await page.getByRole('button', { name: 'Start agent', exact: true }).click();
  await expect(page.locator('#launch-profile')).toBeVisible();
  expect((await commands(page)).some((command) => command === 'agent_start')).toBe(false);
  await page.getByRole('button', { name: 'Back', exact: true }).first().click();
  // Routing may return to the workspaces surface; return to agents explicitly.
  if (await page.getByRole('heading', { name: 'Workspaces', exact: true }).isVisible()) {
    await page.getByRole('button', { name: 'Back', exact: true }).first().click();
  }
  await page.locator('.agent-open').first().click();
  await page.getByRole('button', { name: 'Manage agent', exact: true }).click();
  await expect(page.locator('#manage-agent-dialog')).toBeVisible();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await page.getByRole('button', { name: 'Inspect workspace', exact: true }).click();
  const inspector = page.getByRole('dialog', { name: 'Workspace' });
  await expect(inspector).toBeVisible();
  const file = inspector.getByRole('button', { name: 'main.ts', exact: true });
  await expect(file).toBeVisible();
  expect(await file.evaluate((element) => parseFloat(getComputedStyle(element).paddingLeft))).toBeGreaterThan(0);
  await inspector.getByRole('tab', { name: 'Changes' }).click();
  await inspector.getByRole('button', { name: /main.ts/ }).click();
  const diff = inspector.getByLabel('Diff for src/main.ts');
  await expect(diff).toContainText('+new');
  const initial = await diff.evaluate((element) => parseFloat(getComputedStyle(element).fontSize));
  await inspector.getByRole('button', { name: 'Zoom in diff' }).click();
  await expect.poll(() => diff.evaluate((element) => parseFloat(getComputedStyle(element).fontSize))).toBeGreaterThan(initial);
  await inspector.getByRole('button', { name: 'Reset diff zoom' }).click();
  await expect.poll(() => diff.evaluate((element) => parseFloat(getComputedStyle(element).fontSize))).toBe(initial);
  await inspector.getByRole('button', { name: 'Close workspace inspector' }).click();
});

test('B2 rejected workspace chunk has explicit failure/back UI without automatic command replay', async ({ page }) => {
  await page.route(CHUNK, (route) => route.abort());
  await boot(page);
  const before = await commands(page);
  await page.getByRole('button', { name: 'Manage workspaces', exact: true }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'Workspace tools could not be loaded' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Retry workspace tools' })).toBeVisible();
  await page.getByRole('button', { name: 'Back', exact: true }).first().click();
  await expect(page.locator('article.agent-card')).toHaveCount(1);
  expect(await commands(page)).toEqual(before);
});

import { defineConfig, devices } from '@playwright/test';
import { resolve } from 'node:path';

const webRoot = resolve(process.env.HERDR_WEB_ROOT || 'dist');

export default defineConfig({
  testDir: './tests/browser',
  testIgnore: 'attention-relay.spec.ts',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: true,
  // Two different, unrelated tests have crashed WebKit mid-run on GitHub's
  // shared runners ("Target page, context or browser has been closed") on
  // consecutive releases with no reproduction locally or in the pinned
  // Playwright/WebKit container (single-test and full-suite reruns both
  // pass cleanly there). Each retry gets a fresh, isolated browser context,
  // so it absorbs this transient crash without tolerating a real failure —
  // a deterministic bug still fails every attempt. Local runs (no
  // GITHUB_ACTIONS) stay at zero retries, so `make check` remains a strict
  // gate before every push.
  retries: process.env.GITHUB_ACTIONS ? 2 : 0,
  use: {
    baseURL: 'http://127.0.0.1:4173',
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium-mobile', use: { ...devices['Pixel 7'] } },
    { name: 'webkit-mobile', use: { ...devices['iPhone 15'] } },
  ],
  webServer: {
    command: `node scripts/browser-server.mjs ${JSON.stringify(webRoot)}`,
    port: 4173,
    reuseExistingServer: false,
  },
});

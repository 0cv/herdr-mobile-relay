import { defineConfig, devices } from '@playwright/test';
import { resolve } from 'node:path';

const webRoot = resolve(process.env.HERDR_WEB_ROOT || 'dist');

export default defineConfig({
  testDir: './tests/browser',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: true,
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

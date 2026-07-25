import { defineConfig, devices } from '@playwright/test';
import { resolve } from 'node:path';

const webRoot = resolve(process.env.HERDR_WEB_ROOT || 'dist');

export default defineConfig({
  testDir: './tests/browser',
  testMatch: 'attention-relay.spec.ts',
  timeout: 45_000,
  expect: { timeout: 8_000 },
  fullyParallel: false,
  workers: 1,
  globalSetup: './tests/browser/attention-relay.setup.ts',
  use: {
    baseURL: 'http://127.0.0.1:4173',
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium-attention', use: { ...devices['Pixel 7'] } },
  ],
  webServer: {
    command: `node scripts/browser-server.mjs ${JSON.stringify(webRoot)}`,
    port: 4173,
    reuseExistingServer: false,
  },
});

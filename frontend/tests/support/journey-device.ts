import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { Page } from '@playwright/test';

export const webRoot = process.env.HERDR_WEB_ROOT || 'dist';

export const APP_METADATA = JSON.parse(
  readFileSync(resolve(webRoot, 'version.json'), 'utf8'),
) as { version: string; assets: number; build: string };

export const APP_RELEASE = APP_METADATA.version;

const IPHONE_SAFARI_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1';

export async function emulateStandalone(page: Page, options: { navigatorStandalone?: boolean } = {}) {
  await page.addInitScript((setNavigatorStandalone) => {
    if (setNavigatorStandalone) {
      Object.defineProperty(navigator, 'standalone', { configurable: true, value: true });
    }
    const nativeMatchMedia = window.matchMedia.bind(window);
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value(query: string) {
        const result = nativeMatchMedia(query);
        if (query === '(display-mode: standalone)') {
          Object.defineProperty(result, 'matches', { configurable: true, value: true });
        }
        return result;
      },
    });
  }, options.navigatorStandalone ?? false);
}

export async function emulateIphoneSafari(page: Page) {
  await page.addInitScript((agent) => {
    Object.defineProperty(navigator, 'standalone', { configurable: true, value: false });
    Object.defineProperty(navigator, 'userAgent', { configurable: true, value: agent });
  }, IPHONE_SAFARI_UA);
}

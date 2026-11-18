// @vitest-environment node
import { readFileSync } from 'node:fs';
import { URL } from 'node:url';
import { expect, it } from 'vitest';

it('keeps the browser server CSP aligned with the relay except for test WebSockets', () => {
  const handler = readFileSync(new URL('../../../internal/web/handler.go', import.meta.url), 'utf8');
  const browserServer = readFileSync(new URL('../../scripts/browser-server.mjs', import.meta.url), 'utf8');
  const relayPolicy = handler.match(/\.Set\("Content-Security-Policy", "([^"]+)"\)/)?.[1];
  const browserPolicy = browserServer.match(/'Content-Security-Policy': "([^"]+)"/)?.[1];

  expect(relayPolicy).toBeTypeOf('string');
  expect(browserPolicy).toBeTypeOf('string');
  expect(browserPolicy?.replace(/(connect-src[^;]*?) ws:(?=[ ;])/, '$1')).toBe(relayPolicy);
});

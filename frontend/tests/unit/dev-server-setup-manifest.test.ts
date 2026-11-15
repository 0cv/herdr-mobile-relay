// @vitest-environment node
import { readFileSync } from 'node:fs';
import { URL } from 'node:url';
import { createServer, type Plugin } from 'vite';
import { describe, expect, it } from 'vitest';
import config from '../../vite.config.ts';

describe('Vite dev server', () => {
  it('serves the generated setup manifest during development', async () => {
    const plugin = config.plugins?.find((item): item is Plugin => item !== null
      && typeof item === 'object' && !Array.isArray(item)
      && 'name' in item && item.name === 'stable-release-assets');
    const server = await createServer({ configFile: false, plugins: [plugin], server: { host: '127.0.0.1', port: 0 } });
    try {
      await server.listen();
      const address = server.httpServer?.address();
      if (!address || typeof address === 'string') throw new Error('Missing dev server address');
      const response = await fetch(`http://127.0.0.1:${address.port}/setup.webmanifest?setup=1`);
      expect(response.status).toBe(200);
      expect(response.headers.get('content-type')).toBe('application/manifest+json; charset=utf-8');
      expect(response.headers.get('cache-control')).toBe('no-cache, no-store');
      const manifest = JSON.parse(readFileSync(new URL('../../public/manifest.webmanifest', import.meta.url), 'utf8'));
      delete manifest.start_url;
      expect(await response.json()).toEqual(manifest);
    } finally {
      await server.close();
    }
  });
});

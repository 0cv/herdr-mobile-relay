// @vitest-environment node
import { readFileSync } from 'node:fs';
import { URL } from 'node:url';
import { createServer, type Plugin } from 'vite';
import { describe, expect, it } from 'vitest';
import config from '../../vite.config.ts';

type Chunk = { type: 'chunk'; fileName: string; code: string };
type Asset = { type: 'asset'; fileName: string; source: string | Uint8Array };
type Bundle = Record<string, Chunk | Asset>;

function fixture(code: string, css: string, html: string) {
  const javascript: Chunk = { type: 'chunk', fileName: 'assets/app.js', code };
  const stylesheet: Asset = { type: 'asset', fileName: 'assets/app.css', source: css };
  const entry: Asset = { type: 'asset', fileName: 'index.html', source: html };
  const bundle: Bundle = { 'assets/app.js': javascript, 'assets/app.css': stylesheet, 'index.html': entry };
  const emitted: Asset[] = [];
  const errors: string[] = [];
  return { javascript, stylesheet, entry, bundle, emitted, errors };
}

function publish(f: ReturnType<typeof fixture>) {
  const plugin = config.plugins?.find((item): item is Plugin => item !== null
    && typeof item === 'object' && !Array.isArray(item)
    && 'name' in item && item.name === 'stable-release-assets');
  if (!plugin) throw new Error('Missing actual stable-release-assets plugin');
  const hook = plugin.generateBundle;
  if (!hook) throw new Error('Missing actual generateBundle hook');
  const handler = typeof hook === 'function' ? hook : hook.handler;
  // Only the two used Rollup context callbacks are adapted; this is not a bundler fixture.
  return Reflect.apply(handler, {
    error(message: string): never {
      f.errors.push(message);
      throw new Error(message);
    },
    emitFile(asset: Asset) {
      f.emitted.push(asset);
      return String(f.emitted.length);
    },
  }, [{}, f.bundle, false]);
}

describe('actual Vite release identity', () => {
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

  it('rejects a missing build marker after compaction before publication', () => {
    const originalJavascript = 'const color="--primary-foreground";const snow="雪";\n';
    const compactedJavascript = 'const color="--pf";const snow="雪";\n';
    const originalStylesheet = '.leaf{color:var(--primary-foreground)}\n';
    const compactedStylesheet = '.leaf{color:var(--pf)}\n';
    const originalHtml = '<link href="assets/app.css"><script src="assets/app.js"></script>\n';
    const f = fixture(originalJavascript, originalStylesheet, originalHtml);
    expect(() => publish(f)).toThrowError(new Error('Application bundle is missing its build identity marker'));
    expect(f.errors).toEqual(['Application bundle is missing its build identity marker']);
    expect(f.emitted).toEqual([]);
    expect(f.bundle).toEqual({
      'assets/app.js': { type: 'chunk', fileName: 'assets/app.js', code: compactedJavascript },
      'assets/app.css': { type: 'asset', fileName: 'assets/app.css', source: compactedStylesheet },
      'index.html': { type: 'asset', fileName: 'index.html', source: originalHtml },
    });
  });
});

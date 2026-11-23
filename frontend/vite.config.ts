import { readFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';
import versions from './build-versions.json' with { type: 'json' };
import { pagesHeaders } from './scripts/pages-policy.ts';
import { productVersionFromManifest } from './scripts/release-rules.ts';

const manifest = readFileSync(fileURLToPath(new URL('../herdr-plugin.toml', import.meta.url)), 'utf8');
const productVersion = productVersionFromManifest(manifest);
const versionMetadata = `${JSON.stringify({ version: productVersion, assets: versions.assets })}\n`;
const BUILD_ID_MARKER = '__HERDR_BUILD_ID__';

type BundleChunk = {
  type: 'chunk';
  fileName: string;
  code: string;
};
type BundleAsset = {
  type: 'asset';
  fileName: string;
  source: string | Uint8Array;
};
type Bundle = Record<string, BundleChunk | BundleAsset>;

const releaseStyleNames = [
  ['--primary-foreground', '--pf'], ['--terminal-link-visited', '--tlv'], ['--composer-padding-block', '--cpb'],
  ['--terminal-content-width', '--tcw2'], ['--app-header-height', '--ah'], ['--terminal-controls-height', '--tch'],
  ['--terminal-viewport-height', '--tvh'], ['--terminal-row-height', '--trh'], ['--terminal-line-height', '--tlh'],
  ['--terminal-cell-gap', '--tcg'], ['--composer-max-height', '--cmh'],
  ['--composer-min-height', '--cmin'], ['--safe-area-inset-bottom', '--sab'], ['--safe-area-inset-top', '--sat'],
  ['--terminal-columns', '--tcols'], ['--terminal-rows', '--trows'], ['--diff-zoom', '--dz'], ['--path-depth', '--pd'],
  ['--terminal-link', '--tl'], ['--terminal-text', '--tt'], ['--background', '--bg'], ['--foreground', '--fg'],
  ['--card-hover', '--ch'], ['--secondary', '--s2'], ['--border', '--bd'], ['--primary', '--p'], ['--input', '--in'],
  ['--danger', '--d'], ['--success', '--s'], ['--warning', '--w'], ['--trust', '--tr'],
  ['--card', '--c'], ['--muted', '--m'], ['--tree-line', '--tree'],
] as const;

function compactReleaseStyleNames(source: string): string {
  for (const [from, to] of releaseStyleNames) source = source.replaceAll(from, to);
  return source;
}

function setupManifestSource(): string {
  const manifest = JSON.parse(readFileSync(new URL('./public/manifest.webmanifest', import.meta.url), 'utf8'));
  delete manifest.start_url;
  return `${JSON.stringify(manifest, null, 2)}\n`;
}

function stableReleaseAssets(): Plugin {
  const serveReleaseMetadata = (
    request: { url?: string },
    response: { setHeader(name: string, value: string): void; end(body: string): void },
    next: () => void,
  ) => {
    const pathname = new URL(request.url || '/', 'http://vite.local').pathname;
    if (pathname !== '/version.json' && pathname !== '/setup.webmanifest') {
      next();
      return;
    }
    response.setHeader('Content-Type', pathname === '/setup.webmanifest'
      ? 'application/manifest+json; charset=utf-8' : 'application/json; charset=utf-8');
    response.setHeader('Cache-Control', 'no-cache, no-store');
    response.end(pathname === '/setup.webmanifest' ? setupManifestSource() : versionMetadata);
  };
  return {
    name: 'stable-release-assets',
    enforce: 'post',
    configureServer(server) {
      server.middlewares.use(serveReleaseMetadata);
    },
    configurePreviewServer(server) {
      server.middlewares.use(serveReleaseMetadata);
    },
    generateBundle(_options, bundle: Bundle) {
      const appJavascript = Object.values(bundle).find(
        (item): item is BundleChunk => item.type === 'chunk' && item.fileName === 'assets/app.js',
      );
      const appStylesheet = Object.values(bundle).find(
        (item): item is BundleAsset => item.type === 'asset'
          && item.fileName === 'assets/app.css'
          && typeof item.source === 'string',
      );
      if (!appJavascript || !appStylesheet) {
        this.error(`Expected assets/app.js and assets/app.css; found ${Object.keys(bundle).join(', ')}`);
        return;
      }

      appJavascript.code = compactReleaseStyleNames(appJavascript.code);
      appStylesheet.source = compactReleaseStyleNames(appStylesheet.source as string);

      const whitespaceTable = '` \t\n\\r\\f\\xA0\\v\uFEFF`';
      const escapedWhitespaceTable = '" \\t\\n\\r\\f\\xA0\\v\\uFEFF"';
      appJavascript.code = appJavascript.code.replaceAll(whitespaceTable, escapedWhitespaceTable);

      const scanner = bundle[`assets/SetupScanner-${versions.assets}.js`];
      if (scanner?.type === 'chunk') {
        const notice = readFileSync(new URL('./public/licenses/qr.txt', import.meta.url), 'utf8');
        scanner.code = `/*!\n${notice}*/\n${scanner.code}`;
      }

      this.emitFile({ type: 'asset', fileName: '_headers', source: pagesHeaders() });
      this.emitFile({ type: 'asset', fileName: 'setup.webmanifest', source: setupManifestSource() });
    },
  };
}

// HERDR_DEV_RUNTIME=1 builds the bundle with Svelte's dev runtime, whose
// errors carry the failing data - a keyed each names its duplicate key and
// indexes. It exists for on-device debugging through `make dev-tunnel`;
// phones have no console, so a production error is otherwise an opaque code.
const devRuntime = process.env.HERDR_DEV_RUNTIME === '1';

export default defineConfig({
  plugins: [svelte(devRuntime ? { compilerOptions: { dev: true } } : {}), stableReleaseAssets()],
  resolve: {
    alias: {
      $lib: fileURLToPath(new URL('./src/lib', import.meta.url)),
      $components: fileURLToPath(new URL('./src/components', import.meta.url)),
    },
    conditions: devRuntime ? ['browser', 'development'] : ['browser'],
  },
  build: {
    cssCodeSplit: false,
    modulePreload: { polyfill: false },
    emptyOutDir: true,
    outDir: 'dist',
    rollupOptions: {
      output: {
        // The relay serves one initial script. Keep dependencies shared with lazy views in it.
        codeSplitting: { groups: [{ name: 'app', tags: ['$initial'] }] },
        assetFileNames: (asset) => {
          const names = asset.names ?? [];
          return names.some((name) => name.endsWith('.css')) ? 'assets/app.css' : 'assets/[name][extname]';
        },
        // Lazy chunks inherit the release asset version so each release gets a
        // new immutable URL without participating in the entry digest cycle.
        chunkFileNames: `assets/[name]-${versions.assets}.js`,
        entryFileNames: 'assets/app.js',
      },
    },
    target: 'esnext',
  },
  define: {
    __APP_PROTOCOL_VERSION__: '3',
    __APP_VERSION__: JSON.stringify(productVersion),
    __APP_ASSET_VERSION__: JSON.stringify(versions.assets),
    __APP_BUILD_ID__: JSON.stringify(BUILD_ID_MARKER),
    __SERVICE_WORKER_URL__: JSON.stringify(`/sw.js?v=${versions.serviceWorker}`),
  },
  test: {
    environment: 'jsdom',
    include: ['tests/unit/**/*.test.ts'],
    setupFiles: ['./tests/setup.ts'],
  },
});

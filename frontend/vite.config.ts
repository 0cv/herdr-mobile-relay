import { readFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';
import tailwindcss from '@tailwindcss/vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';
import versions from './build-versions.json';

const manifest = readFileSync(fileURLToPath(new URL('../herdr-plugin.toml', import.meta.url)), 'utf8');
const productVersion = manifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
if (!productVersion) throw new Error('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
const versionMetadata = `${JSON.stringify({ version: productVersion, assets: versions.assets })}\n`;

function stableReleaseAssets(): Plugin {
  const serveVersionMetadata = (
    request: { url?: string },
    response: { setHeader(name: string, value: string): void; end(body: string): void },
    next: () => void,
  ) => {
    const pathname = new URL(request.url || '/', 'http://vite.local').pathname;
    if (pathname !== '/version.json') {
      next();
      return;
    }
    response.setHeader('Content-Type', 'application/json; charset=utf-8');
    response.setHeader('Cache-Control', 'no-cache');
    response.end(versionMetadata);
  };
  return {
    name: 'stable-release-assets',
    enforce: 'post',
    configureServer(server) {
      server.middlewares.use(serveVersionMetadata);
    },
    configurePreviewServer(server) {
      server.middlewares.use(serveVersionMetadata);
    },
    generateBundle(_options, bundle) {
      this.emitFile({
        type: 'asset',
        fileName: 'version.json',
        source: versionMetadata,
      });
      const javascript = Object.values(bundle).filter(
        (item) => item.type === 'chunk' && item.fileName.endsWith('.js'),
      );
      const stylesheets = Object.values(bundle).filter(
        (item) => item.type === 'asset' && item.fileName.endsWith('.css'),
      );
      if (javascript.length !== 1 || javascript[0]?.fileName !== 'assets/app.js') {
        this.error(`Expected only assets/app.js; found ${javascript.map((item) => item.fileName).join(', ')}`);
      }
      const appJavascript = javascript[0];
      if (!appJavascript || appJavascript.type !== 'chunk') {
        this.error('Vite did not emit assets/app.js as a JavaScript chunk');
      }
      if (stylesheets.length !== 1 || stylesheets[0]?.fileName !== 'assets/app.css') {
        this.error(`Expected only assets/app.css; found ${stylesheets.map((item) => item.fileName).join(', ')}`);
      }

      const whitespaceTable = '` \t\n\\r\\f\\xA0\\v\uFEFF`';
      const escapedWhitespaceTable = '" \\t\\n\\r\\f\\xA0\\v\\uFEFF"';
      appJavascript.code = appJavascript.code.replaceAll(whitespaceTable, escapedWhitespaceTable);

      const html = bundle['index.html'];
      if (!html || html.type !== 'asset' || typeof html.source !== 'string') {
        this.error('Vite did not emit index.html');
      }
      const versioned = html.source
        .replace(/(assets\/app\.js)(?!\?v=)/g, `$1?v=${versions.assets}`)
        .replace(/(assets\/app\.css)(?!\?v=)/g, `$1?v=${versions.assets}`);
      if (!versioned.includes(`assets/app.js?v=${versions.assets}`)) {
        this.error('Generated index.html does not reference the versioned application script');
      }
      if (!versioned.includes(`assets/app.css?v=${versions.assets}`)) {
        this.error('Generated index.html does not reference the versioned application stylesheet');
      }
      html.source = versioned;
    },
  };
}

export default defineConfig({
  plugins: [tailwindcss(), svelte(), stableReleaseAssets()],
  resolve: {
    alias: {
      $lib: fileURLToPath(new URL('./src/lib', import.meta.url)),
      $components: fileURLToPath(new URL('./src/components', import.meta.url)),
    },
    conditions: ['browser'],
  },
  build: {
    cssCodeSplit: false,
    emptyOutDir: true,
    outDir: 'dist',
    rollupOptions: {
      output: {
        assetFileNames: (asset) => {
          const names = asset.names ?? [];
          return names.some((name) => name.endsWith('.css')) ? 'assets/app.css' : 'assets/[name][extname]';
        },
        chunkFileNames: 'assets/[name].js',
        entryFileNames: 'assets/app.js',
      },
    },
    target: 'es2022',
  },
  define: {
    __APP_PROTOCOL_VERSION__: '2',
    __APP_VERSION__: JSON.stringify(productVersion),
    __APP_ASSET_VERSION__: JSON.stringify(versions.assets),
    __SERVICE_WORKER_URL__: JSON.stringify(`sw.js?v=${versions.serviceWorker}`),
  },
  test: {
    environment: 'jsdom',
    include: ['tests/unit/**/*.test.ts'],
    setupFiles: ['./tests/setup.ts'],
  },
});

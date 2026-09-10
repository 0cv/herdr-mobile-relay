import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';
import versions from './build-versions.json' with { type: 'json' };

const manifest = readFileSync(fileURLToPath(new URL('../herdr-plugin.toml', import.meta.url)), 'utf8');
const productVersion = manifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
if (!productVersion) throw new Error('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
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
type BundleItem = BundleChunk | BundleAsset;
type Bundle = Record<string, BundleItem>;

function digest(source: string | Uint8Array): { sha256: string; integrity: string } {
  const hash = createHash('sha256').update(source).digest();
  return {
    sha256: hash.toString('hex'),
    integrity: `sha256-${hash.toString('base64')}`,
  };
}

function releaseBootstrap(): string {
  return '<!doctype html><script src="/herdr-bootstrap.js"></script>\n';
}

function bootstrapScript(entryPath: string): string {
  return `const e = new URL(window.__HERDR_ENTRY__ || ${JSON.stringify(`/${entryPath}`)}, location);\n  e.search = location.search;\n  e.hash = location.hash;\n  location.replace(e);\n`;
}

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

function immutableHeaders(): string {
  return `/* Stable bootstrap and metadata are deliberately revalidated. Only\n * digest-addressed build entries and assets may be stored indefinitely. */\n/herdr-bootstrap.js\n  Cache-Control: no-cache, no-store\n\n/manifest-loader.js\n  Cache-Control: no-cache, no-store\n\n/manifest.webmanifest\n  Cache-Control: no-cache, no-store\n\n/setup.webmanifest\n  Cache-Control: no-cache, no-store\n\n/sw.js\n  Cache-Control: no-cache, no-store\n\n/version.json\n  Cache-Control: no-cache, no-store\n\n/release.json\n  Cache-Control: no-cache, no-store\n\n/\n  Cache-Control: no-cache, no-store\n\n/index.html\n  Cache-Control: no-cache, no-store\n\n/builds/*\n  Cache-Control: public, max-age=31536000, immutable\n\n/assets/*\n  Cache-Control: public, max-age=31536000, immutable\n`;
}

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
    response.setHeader('Cache-Control', 'no-cache, no-store');
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
      if (!appJavascript.code.includes(BUILD_ID_MARKER)) {
        this.error('Application bundle is missing its build identity marker');
      }
      // The build identity is derived from canonical bundle bytes with the
      // marker still present. It is therefore stable even though the identity
      // is injected into the running bundle before its final content digest is
      // used as the URL and descriptor identity.
      const canonical = `${appJavascript.code}\u0000${appStylesheet.source}`;
      const build = digest(canonical).sha256;
      appJavascript.code = appJavascript.code.replaceAll(BUILD_ID_MARKER, build);

      const javascriptIdentity = digest(appJavascript.code);
      const stylesheetIdentity = digest(appStylesheet.source);
      appJavascript.fileName = `assets/app-${javascriptIdentity.sha256}.js`;
      appStylesheet.fileName = `assets/app-${stylesheetIdentity.sha256}.css`;

      const html = bundle['index.html'];
      if (!html || html.type !== 'asset' || typeof html.source !== 'string') {
        this.error('Vite did not emit index.html');
      }
      let entrySource = html.source
        .replaceAll('assets/app.js', appJavascript.fileName)
        .replaceAll('assets/app.css', appStylesheet.fileName)
        .replaceAll('src="manifest-loader.js"', 'src="/manifest-loader.js"')
        .replaceAll('href="icons/', 'href="/icons/')
        .replace(/>\s+</g, '><')
        .replace(/\s{2,}/g, ' ')
        .replace(/\s+\/>/g, '/>')
        .replace(/(\s)((?!(?:src|href|integrity)=")[A-Za-z_:][\w:.-]*)="([A-Za-z0-9_./:#?-]+)"/g, '$1$2=$3')
        .replace(/\/>/g, '>');
      entrySource = entrySource.replace(
        new RegExp(`(<script[^>]*src=["']/?${appJavascript.fileName.replaceAll('/', '\\/')}["'][^>]*)>`),
        (match: string, prefix: string) => `${prefix} integrity="${javascriptIdentity.integrity}"${/\bcrossorigin(?:[=\s]|$)/i.test(prefix) ? '' : ' crossorigin="anonymous"'}>`,
      );
      entrySource = entrySource.replace(
        new RegExp(`(<link[^>]*href=["']/?${appStylesheet.fileName.replaceAll('/', '\\/')}["'][^>]*)>`),
        (match: string, prefix: string) => `${prefix} integrity="${stylesheetIdentity.integrity}"${/\bcrossorigin(?:[=\s]|$)/i.test(prefix) ? '' : ' crossorigin="anonymous"'}>`,
      );

      const entryPath = `builds/${productVersion}-${versions.assets}-${build.slice(0, 16)}/index.html`;
      const entryIdentity = digest(entrySource);
      const files = {
        entry: { path: entryPath, sha256: entryIdentity.sha256, integrity: entryIdentity.integrity },
        javascript: { path: appJavascript.fileName, sha256: javascriptIdentity.sha256, integrity: javascriptIdentity.integrity },
        stylesheet: { path: appStylesheet.fileName, sha256: stylesheetIdentity.sha256, integrity: stylesheetIdentity.integrity },
      };
      const descriptor = {
        schema: 1,
        version: productVersion,
        assets: versions.assets,
        build,
        entry: `/${entryPath}`,
        files,
      };
      const serializedDescriptor = `${JSON.stringify(descriptor, null, 2)}\n`;
      const serializedVersion = `${JSON.stringify({
        version: productVersion,
        assets: versions.assets,
        build,
        entry: descriptor.entry,
        script: `/${appJavascript.fileName}`,
        style: `/${appStylesheet.fileName}`,
        script_sha256: javascriptIdentity.sha256,
        style_sha256: stylesheetIdentity.sha256,
      })}\n`;

      this.emitFile({ type: 'asset', fileName: entryPath, source: entrySource });
      html.source = releaseBootstrap();
      this.emitFile({ type: 'asset', fileName: 'herdr-bootstrap.js', source: bootstrapScript(entryPath) });
      this.emitFile({ type: 'asset', fileName: 'release.json', source: serializedDescriptor });
      this.emitFile({ type: 'asset', fileName: 'version.json', source: serializedVersion });
      this.emitFile({
        type: 'asset',
        fileName: '_redirects',
        source: `/ /${entryPath} 302\n/index.html /${entryPath} 302\n`,
      });
      const headers = bundle['_headers'];
      if (!headers || headers.type !== 'asset') {
        this.emitFile({ type: 'asset', fileName: '_headers', source: immutableHeaders() });
      } else {
        headers.source = immutableHeaders();
      }
    },
  };
}

export function assetContentVersion(source: string | Uint8Array): string {
  return digest(source).sha256.slice(0, 16);
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
        assetFileNames: (asset) => {
          const names = asset.names ?? [];
          return names.some((name) => name.endsWith('.css')) ? 'assets/app.css' : 'assets/[name][extname]';
        },
        chunkFileNames: 'assets/[name].js',
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

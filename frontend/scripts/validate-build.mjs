import { readFile, readdir, stat } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { brotliDecompressSync } from 'node:zlib';
import versions from '../build-versions.json' with { type: 'json' };
import { compressedAssets } from './compressed-assets.mjs';

const root = resolve(process.argv[2] || 'dist');
const pluginManifest = await readFile(new URL('../../herdr-plugin.toml', import.meta.url), 'utf8');
const productVersion = pluginManifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
if (!productVersion) throw new Error('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
const required = [
  '_headers',
  'index.html',
  'manifest.webmanifest',
  'notification-icons.js',
  'sw.js',
  'version.json',
  'assets/app.js',
  'assets/app.css',
  'icons/icon.svg',
  'icons/icon-192.png',
  'icons/icon-512.png',
  'icons/icon-maskable-512.png',
  'icons/apple-touch-icon.png',
];

for (const relative of required) {
  const file = join(root, relative);
  if (!(await stat(file).catch(() => null))?.isFile()) {
    throw new Error(`Required release file is missing: ${relative}`);
  }
}

for (const relative of compressedAssets) {
  const source = await readFile(join(root, relative));
  const compressed = await readFile(join(root, `${relative}.br`));
  const decompressed = brotliDecompressSync(compressed);
  if (!decompressed.equals(source)) {
    throw new Error(`Brotli asset does not match its source: ${relative}.br`);
  }
}

const assets = await readdir(join(root, 'assets'));
const scripts = assets.filter((name) => name.endsWith('.js'));
const styles = assets.filter((name) => name.endsWith('.css'));
if (scripts.length !== 1 || scripts[0] !== 'app.js') {
  throw new Error(`Expected only assets/app.js; found ${scripts.join(', ')}`);
}
if (styles.length !== 1 || styles[0] !== 'app.css') {
  throw new Error(`Expected only assets/app.css; found ${styles.join(', ')}`);
}

const html = await readFile(join(root, 'index.html'), 'utf8');
for (const reference of [`assets/app.js?v=${versions.assets}`, `assets/app.css?v=${versions.assets}`]) {
  if (!html.includes(reference)) throw new Error(`index.html is missing ${reference}`);
}
if (/assets\/app\.(?:js|css)(?!\?v=)/.test(html)) {
  throw new Error('Application asset references must carry the manual cache-busting version');
}

const appVersion = JSON.parse(await readFile(join(root, 'version.json'), 'utf8'));
if (appVersion.version !== productVersion || appVersion.assets !== versions.assets) {
  throw new Error('version.json differs from herdr-plugin.toml or build-versions.json');
}

const headers = await readFile(join(root, '_headers'), 'utf8');
for (const route of ['/sw.js', '/', '/index.html', '/version.json']) {
  const block = new RegExp(`(?:^|\\n)${route.replace('.', '\\.')}\\n(?:[ \\t]+[^\\n]+\\n)*[ \\t]+Cache-Control: no-cache(?:\\n|$)`);
  if (!block.test(headers)) throw new Error(`_headers does not preserve no-cache for ${route}`);
}

const serviceWorker = await readFile(join(root, 'sw.js'), 'utf8');
if (!serviceWorker.includes(`notification-icons.js?v=${versions.notificationIcons}`)) {
  throw new Error('sw.js notification icon version differs from build-versions.json');
}

const manifest = JSON.parse(await readFile(join(root, 'manifest.webmanifest'), 'utf8'));
if (manifest.start_url !== './' || manifest.scope !== './' || manifest.display !== 'standalone') {
  throw new Error('PWA manifest start_url, scope, or display contract changed');
}
if (!Array.isArray(manifest.icons) || manifest.icons.length < 3) {
  throw new Error('PWA manifest icons are incomplete');
}

console.log(`Validated release structure in ${root}`);

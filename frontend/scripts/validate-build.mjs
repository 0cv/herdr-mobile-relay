import { createHash } from 'node:crypto';
import { readFile, readdir, stat } from 'node:fs/promises';
import { join, normalize, resolve } from 'node:path';
import { brotliDecompressSync } from 'node:zlib';
import versions from '../build-versions.json' with { type: 'json' };
import { releaseCompressedAssets } from './compressed-assets.mjs';

const root = resolve(process.argv[2] || 'dist');
const pluginManifest = await readFile(new URL('../../herdr-plugin.toml', import.meta.url), 'utf8');
const productVersion = pluginManifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
if (!productVersion) throw new Error('herdr-plugin.toml must declare a MAJOR.MINOR.PATCH version');
const required = [
  '_headers',
  '_redirects',
  'index.html',
  'herdr-bootstrap.js',
  'manifest-loader.js',
  'manifest.webmanifest',
  'setup.webmanifest',
  'notification-icons.js',
  'sw.js',
  'version.json',
  'release.json',
  'fonts/nerd-symbols-mono-v3.4.0.woff2',
  'fonts/nerd-symbols-mono-v3.4.0.license.txt',
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

const compressedAssets = await releaseCompressedAssets(root);
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
const workerScripts = scripts.filter((name) => /^attachment-hash\.worker-[A-Za-z0-9_-]+\.js$/.test(name));
const applicationScripts = scripts.filter((name) => /^app-[a-f0-9]{64}\.js$/.test(name));
const lazyScripts = scripts.filter((name) => /^[A-Za-z0-9_.-]+-[0-9]+\.js$/.test(name));
if (lazyScripts.some((name) => !name.endsWith(`-${versions.assets}.js`))) {
  throw new Error(`Lazy chunk names must use the current asset version ${versions.assets}`);
}
const styles = assets.filter((name) => name.endsWith('.css'));
const applicationStyles = styles.filter((name) => /^app-[a-f0-9]{64}\.css$/.test(name));
const unexpectedScripts = scripts.filter((name) => !workerScripts.includes(name)
  && !applicationScripts.includes(name) && !lazyScripts.includes(name));
if (applicationScripts.length !== 1 || workerScripts.length !== 1 || unexpectedScripts.length !== 0) {
  throw new Error(`Expected one content-addressed app script and one attachment hash worker; found ${scripts.join(', ')}`);
}
const lazyReferencePattern = /import\(\s*[`'"]\.\/([A-Za-z0-9_.-]+-[0-9]+\.js)[`'"]\s*\)/g;
const applicationSource = await readFile(join(root, 'assets', applicationScripts[0]), 'utf8');
const referencedLazyScripts = new Set();
for (const match of applicationSource.matchAll(lazyReferencePattern)) {
  const name = match[1];
  if (!name.endsWith(`-${versions.assets}.js`) || !lazyScripts.includes(name)) {
    throw new Error(`Application references an invalid lazy chunk: ${name}`);
  }
  referencedLazyScripts.add(name);
}
if (lazyScripts.some((name) => !referencedLazyScripts.has(name))) {
  throw new Error(`Lazy chunks must be referenced by the application: ${lazyScripts.join(', ')}`);
}
if (applicationStyles.length !== 1 || styles.length !== 1) {
  throw new Error(`Expected one content-addressed app stylesheet; found ${styles.join(', ')}`);
}

async function fileDigest(relative) {
  return createHash('sha256').update(await readFile(join(root, relative))).digest('hex');
}

function integrityFor(hex) {
  return `sha256-${Buffer.from(hex, 'hex').toString('base64')}`;
}

function safeRelativePath(value) {
  return typeof value === 'string'
    && value.length > 0
    && !value.startsWith('/')
    && !value.includes('\\')
    && normalize(value) === value
    && value !== '.'
    && value !== '..'
    && !value.startsWith('../');
}

const descriptor = JSON.parse(await readFile(join(root, 'release.json'), 'utf8'));
if (descriptor.schema !== 1
  || descriptor.version !== productVersion
  || !Number.isInteger(descriptor.assets)
  || descriptor.assets !== versions.assets
  || !/^[a-f0-9]{64}$/.test(descriptor.build)
  || typeof descriptor.entry !== 'string'
  || !/^\/builds\/[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-f0-9]{16,64}\/index\.html$/.test(descriptor.entry)) {
  throw new Error('release.json has an invalid build identity');
}
const descriptorFiles = descriptor.files;
const descriptorKinds = ['entry', 'javascript', 'stylesheet'];
if (!descriptorFiles || typeof descriptorFiles !== 'object'
  || Array.isArray(descriptorFiles)
  || Object.keys(descriptorFiles).sort().join(',') !== descriptorKinds.sort().join(',')) {
  throw new Error('release.json must contain exactly entry, javascript, and stylesheet files');
}
const entryPath = descriptorFiles.entry?.path;
const scriptPath = descriptorFiles.javascript?.path;
const stylePath = descriptorFiles.stylesheet?.path;
if (entryPath !== descriptor.entry.slice(1)
  || !/^builds\/.+\/index\.html$/.test(entryPath || '')
  || !/^assets\/app-[a-f0-9]{64}\.js$/.test(scriptPath || '')
  || !/^assets\/app-[a-f0-9]{64}\.css$/.test(stylePath || '')) {
  throw new Error('release.json file paths are not content-addressed');
}
for (const kind of descriptorKinds) {
  const value = descriptorFiles[kind];
  if (!value || typeof value !== 'object' || !safeRelativePath(value.path)
    || !/^[a-f0-9]{64}$/.test(value.sha256)
    || value.integrity !== integrityFor(value.sha256)) {
    throw new Error(`release.json has an invalid ${kind} descriptor`);
  }
  const digest = await fileDigest(value.path);
  if (digest !== value.sha256) throw new Error(`${kind} digest does not match ${value.path}`);
  if (!/^[a-f0-9]{64}$/.test(value.brotli_sha256)
    || value.brotli_integrity !== integrityFor(value.brotli_sha256)) {
    throw new Error(`${kind} Brotli digest is missing or invalid`);
  }
  const compressedDigest = await fileDigest(`${value.path}.br`);
  if (compressedDigest !== value.brotli_sha256) {
    throw new Error(`${kind} Brotli digest does not match ${value.path}.br`);
  }
}
const entrySource = await readFile(join(root, entryPath), 'utf8');
if (!entrySource.includes(`src="/${scriptPath}"`)
  || !entrySource.includes(`href="/${stylePath}"`)
  || !entrySource.includes(`integrity="${descriptorFiles.javascript.integrity}"`)
  || !entrySource.includes(`integrity="${descriptorFiles.stylesheet.integrity}"`)) {
  throw new Error('build entry does not reference its integrity-checked application assets');
}
if (/assets\/app\.(?:js|css)(?:["?]|$)/.test(entrySource)) {
  throw new Error('build entry references a stable application asset name');
}

const version = JSON.parse(await readFile(join(root, 'version.json'), 'utf8'));
if (version.version !== productVersion
  || version.assets !== versions.assets
  || version.build !== descriptor.build
  || version.entry !== descriptor.entry
  || version.script !== `/${scriptPath}`
  || version.style !== `/${stylePath}`
  || version.script_sha256 !== descriptorFiles.javascript.sha256
  || version.style_sha256 !== descriptorFiles.stylesheet.sha256) {
  throw new Error('version.json differs from release.json, herdr-plugin.toml, or build-versions.json');
}

const headers = await readFile(join(root, '_headers'), 'utf8');
const headerLines = headers.split(/\r?\n/);
for (const route of ['/', '/index.html', '/version.json', '/release.json', '/herdr-bootstrap.js']) {
  const routeIndex = headerLines.findIndex((line) => line === route);
  const cacheLine = routeIndex >= 0
    ? headerLines.slice(routeIndex + 1, routeIndex + 5).find((line) => line.trim() === 'Cache-Control: no-cache, no-store')
    : undefined;
  if (!cacheLine) throw new Error(`_headers does not preserve no-cache for ${route}`);
}
if (!headers.includes('/builds/*') || !headers.includes('public, max-age=31536000, immutable')
  || !headers.includes('/assets/*')) {
  throw new Error('_headers does not mark only digest-addressed resources immutable');
}
const redirects = await readFile(join(root, '_redirects'), 'utf8');
if (!redirects.includes(`/ ${descriptor.entry} 302`) || !redirects.includes(`/index.html ${descriptor.entry} 302`)) {
  throw new Error('_redirects does not route stable bootstrap paths to the current build entry');
}

const serviceWorker = await readFile(join(root, 'sw.js'), 'utf8');
if (!serviceWorker.includes(`notification-icons.js?v=${versions.notificationIcons}`)) {
  throw new Error('sw.js notification icon version differs from build-versions.json');
}

const manifest = JSON.parse(await readFile(join(root, 'manifest.webmanifest'), 'utf8'));
if (manifest.id !== './' || manifest.start_url !== './' || manifest.scope !== './' || manifest.display !== 'standalone') {
  throw new Error('PWA manifest id, start_url, scope, or display contract changed');
}
if (!Array.isArray(manifest.icons) || manifest.icons.length < 3) {
  throw new Error('PWA manifest icons are incomplete');
}
const setupManifest = JSON.parse(await readFile(join(root, 'setup.webmanifest'), 'utf8'));
const expectedSetupManifest = JSON.parse(JSON.stringify(manifest));
delete expectedSetupManifest.start_url;
if (JSON.stringify(setupManifest) !== JSON.stringify(expectedSetupManifest)) {
  throw new Error('Setup manifest must match the PWA manifest without start_url');
}

console.log(`Validated release structure in ${root}`);

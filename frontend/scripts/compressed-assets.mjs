import { readdir } from 'node:fs/promises';
import { join } from 'node:path';
import { appScriptPattern, appStylePattern, lazyScriptPattern } from './release-rules.ts';

// Stable resources are compressed explicitly. The application entry resources
// are digest-addressed, so discover their names instead of reintroducing a
// stable app.js/app.css scheme here.
export const compressedAssets = [
  'index.html',
  'herdr-bootstrap.js',
  'notification-icons.js',
  'sw.js',
  'manifest-loader.js',
  'manifest.webmanifest',
  'setup.webmanifest',
  'version.json',
  'release.json',
  'icons/icon.svg',
];

export async function releaseCompressedAssets(root, entry) {
  const assets = await readdir(join(root, 'assets'));
  const contentAddressed = assets
    .filter((name) => appScriptPattern.test(name) || appStylePattern.test(name))
    .map((name) => `assets/${name}`);
  const lazyScripts = assets
    .filter((name) => lazyScriptPattern.test(name))
    .map((name) => `assets/${name}`);
  return [...new Set([...compressedAssets, entry, ...contentAddressed, ...lazyScripts])];
}

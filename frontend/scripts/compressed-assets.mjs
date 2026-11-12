import { readFile, readdir } from 'node:fs/promises';
import { join } from 'node:path';
import { appScriptPattern, appStylePattern, lazyScriptPattern, releaseEntryPath } from './release-rules.ts';

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

export async function releaseCompressedAssets(root) {
  const assets = await readdir(join(root, 'assets'));
  const descriptor = JSON.parse(await readFile(join(root, 'release.json'), 'utf8'));
  const entry = releaseEntryPath(descriptor);
  const contentAddressed = assets
    .filter((name) => appScriptPattern.test(name) || appStylePattern.test(name))
    .map((name) => `assets/${name}`);
  const lazyScripts = assets
    .filter((name) => lazyScriptPattern.test(name))
    .map((name) => `assets/${name}`);
  return [...new Set([...compressedAssets, entry, ...contentAddressed, ...lazyScripts])];
}

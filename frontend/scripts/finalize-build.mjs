import { assetIdentity } from './asset-identity.mjs';
import { readdir, readFile, rename, writeFile } from 'node:fs/promises';
import { basename, join, resolve } from 'node:path';
import { brotliCompressSync, constants } from 'node:zlib';
import { releaseCompressedAssets } from './compressed-assets.mjs';
import { releaseEntryPath } from './release-rules.ts';

function compress(source) {
  return brotliCompressSync(source, {
    params: {
      [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_QUALITY]: 11,
      [constants.BROTLI_PARAM_SIZE_HINT]: source.length,
    },
  });
}

const root = resolve(process.argv[2] || 'dist');
const descriptorPath = join(root, 'release.json');
const descriptor = JSON.parse(await readFile(descriptorPath, 'utf8'));
const javascript = descriptor?.files?.javascript;
if (!javascript || typeof javascript.path !== 'string' || typeof javascript.sha256 !== 'string') {
  throw new Error('release.json does not describe an application script');
}

const sourcePath = join(root, javascript.path);
const source = await readFile(sourcePath);
const { sha256, integrity } = assetIdentity(source);
const nextPath = `assets/app-${sha256}.js`;
const oldPath = javascript.path;
if (oldPath !== nextPath) await rename(sourcePath, join(root, nextPath));
const oldScriptName = basename(oldPath);
const nextScriptName = basename(nextPath);
for (const name of await readdir(join(root, 'assets'))) {
  if (!name.endsWith('.js') || name === nextScriptName || name === oldScriptName) continue;
  const lazyPath = join(root, 'assets', name);
  const lazySource = await readFile(lazyPath, 'utf8');
  const rewritten = lazySource.replaceAll(`./${oldScriptName}`, `./${nextScriptName}`).replaceAll('./app.js', `./${nextScriptName}`);
  if (rewritten !== lazySource) await writeFile(lazyPath, rewritten);
}

const entryPath = descriptor?.files?.entry?.path;
if (typeof entryPath !== 'string') throw new Error('release.json does not describe a build entry');
const entryFile = join(root, entryPath);
const entry = await readFile(entryFile, 'utf8');
const nextEntry = entry
  .replaceAll(`/${oldPath}`, `/${nextPath}`)
  .replaceAll(javascript.integrity, integrity);
await writeFile(entryFile, nextEntry);
const { sha256: entrySha256, integrity: entryIntegrity } = assetIdentity(nextEntry);

javascript.path = nextPath;
javascript.sha256 = sha256;
javascript.integrity = integrity;
descriptor.files.entry.sha256 = entrySha256;
descriptor.files.entry.integrity = entryIntegrity;

const versionPath = join(root, 'version.json');
const version = JSON.parse(await readFile(versionPath, 'utf8'));
version.script = `/${nextPath}`;
version.script_sha256 = sha256;
await writeFile(versionPath, `${JSON.stringify(version)}\n`);

const compressedAssets = await releaseCompressedAssets(root, releaseEntryPath(descriptor));
const compressed = new Map();
for (const relative of compressedAssets) {
  if (relative === 'release.json') continue;
  const output = compress(await readFile(join(root, relative)));
  compressed.set(relative, output);
  await writeFile(join(root, `${relative}.br`), output);
}

// Record the encoded representation alongside the decoded asset identity. The
// deployment verifier can therefore check a CDN's Brotli response without
// trusting a Content-Encoding header or silently accepting a corrupt sidecar.
for (const file of Object.values(descriptor.files)) {
  const relative = file.path.replace(/^\//, '');
  const output = compressed.get(relative);
  if (!output) throw new Error(`No Brotli representation was created for ${relative}`);
  const identity = assetIdentity(output);
  file.brotli_sha256 = identity.sha256;
  file.brotli_integrity = identity.integrity;
}
const serializedDescriptor = `${JSON.stringify(descriptor, null, 2)}\n`;
await writeFile(descriptorPath, serializedDescriptor);
await writeFile(`${descriptorPath}.br`, compress(Buffer.from(serializedDescriptor)));

console.log(`Created ${compressedAssets.length} Brotli assets in ${root}`);

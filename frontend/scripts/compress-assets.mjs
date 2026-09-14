import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { brotliCompressSync, constants } from 'node:zlib';
import { releaseCompressedAssets } from './compressed-assets.mjs';

const root = resolve(process.argv[2] || 'dist');

function compress(source) {
  return brotliCompressSync(source, {
    params: {
      [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_QUALITY]: 11,
      [constants.BROTLI_PARAM_SIZE_HINT]: source.length,
    },
  });
}

function sha256(source) {
  return createHash('sha256').update(source).digest('hex');
}

function integrity(hex) {
  return `sha256-${Buffer.from(hex, 'hex').toString('base64')}`;
}

const compressedAssets = await releaseCompressedAssets(root);
const compressed = new Map();
for (const relative of compressedAssets) {
  if (relative === 'release.json') continue;
  const source = await readFile(join(root, relative));
  const output = compress(source);
  compressed.set(relative, output);
  await writeFile(join(root, `${relative}.br`), output);
}

// Record the encoded representation alongside the decoded asset identity. The
// deployment verifier can therefore check a CDN's Brotli response without
// trusting a Content-Encoding header or silently accepting a corrupt sidecar.
const descriptorPath = join(root, 'release.json');
const descriptor = JSON.parse(await readFile(descriptorPath, 'utf8'));
for (const file of Object.values(descriptor.files || {})) {
  const relative = typeof file.path === 'string' ? file.path.replace(/^\//, '') : '';
  const output = compressed.get(relative);
  if (!output) throw new Error(`No Brotli representation was created for ${relative}`);
  const digest = sha256(output);
  file.brotli_sha256 = digest;
  file.brotli_integrity = integrity(digest);
}
const serializedDescriptor = `${JSON.stringify(descriptor, null, 2)}\n`;
await writeFile(descriptorPath, serializedDescriptor);
const descriptorCompressed = compress(Buffer.from(serializedDescriptor));
await writeFile(`${descriptorPath}.br`, descriptorCompressed);
compressed.set('release.json', descriptorCompressed);

console.log(`Created ${compressedAssets.length} Brotli assets in ${root}`);

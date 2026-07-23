import { readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { brotliCompressSync, constants } from 'node:zlib';
import { compressedAssets } from './compressed-assets.mjs';

const root = resolve(process.argv[2] || 'dist');

for (const relative of compressedAssets) {
  const source = await readFile(join(root, relative));
  const compressed = brotliCompressSync(source, {
    params: {
      [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_QUALITY]: 11,
      [constants.BROTLI_PARAM_SIZE_HINT]: source.length,
    },
  });
  await writeFile(join(root, `${relative}.br`), compressed);
}

console.log(`Created ${compressedAssets.length} Brotli assets in ${root}`);

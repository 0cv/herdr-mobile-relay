#!/usr/bin/env node
import { readFile, writeFile } from 'node:fs/promises';
import { brotliCompressSync, constants } from 'node:zlib';

const [filename, version, revision] = process.argv.slice(2);
if (!filename || !version || !revision) {
  throw new Error('usage: stamp-web-version.mjs VERSION_JSON VERSION REVISION');
}

const metadata = JSON.parse(await readFile(filename, 'utf8'));
metadata.version = version;
metadata.release_version = version;
metadata.revision = revision;
const serialized = `${JSON.stringify(metadata)}\n`;
await writeFile(filename, serialized);
await writeFile(`${filename}.br`, brotliCompressSync(Buffer.from(serialized), {
  params: {
    [constants.BROTLI_PARAM_QUALITY]: 11,
  },
}));

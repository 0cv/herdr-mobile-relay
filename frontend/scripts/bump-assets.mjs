import { readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const frontend = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const versionsFile = join(frontend, 'build-versions.json');
const versions = JSON.parse(await readFile(versionsFile, 'utf8'));
versions.assets += 1;
await writeFile(versionsFile, `${JSON.stringify(versions, null, 2)}\n`);
console.log(`Bumped assets cache-busting version to ${versions.assets}`);

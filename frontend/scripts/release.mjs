import { cp, mkdir, rm, stat } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const frontend = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const source = join(frontend, 'dist');
const destination = resolve(frontend, '..', 'web');
if (!(await stat(join(source, 'index.html')).catch(() => null))?.isFile()) {
  throw new Error('frontend/dist is missing; run npm run build before creating a release bundle');
}

const staging = resolve(frontend, '..', '.web-release-staging');
await rm(staging, { recursive: true, force: true });
await mkdir(staging, { recursive: true });
await cp(source, staging, { recursive: true });

await rm(destination, { recursive: true, force: true });
await cp(staging, destination, { recursive: true });
await rm(staging, { recursive: true, force: true });
console.log(`Replaced committed release bundle at ${destination}`);

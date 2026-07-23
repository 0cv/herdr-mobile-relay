import { readFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { constants, gzipSync } from 'node:zlib';

const limit = 81_920;
const root = resolve(process.argv[2] || 'dist');
const files = ['index.html', 'assets/app.js', 'assets/app.css', 'notification-icons.js'];
let totalRaw = 0;
let totalGzip = 0;
let totalBrotli = 0;

console.log('Initial payload budget:');
for (const relative of files) {
  const source = await readFile(join(root, relative));
  const brotli = await readFile(join(root, `${relative}.br`));
  const gzip = gzipSync(source, {
    level: 9,
    memLevel: 8,
    strategy: constants.Z_DEFAULT_STRATEGY,
    windowBits: 15,
  });
  totalRaw += source.length;
  totalGzip += gzip.length;
  totalBrotli += brotli.length;
  console.log(`${relative.padEnd(28)} raw ${String(source.length).padStart(8)} B  gzip ${String(gzip.length).padStart(7)} B  br ${String(brotli.length).padStart(7)} B`);
}
console.log(`${'TOTAL'.padEnd(28)} raw ${String(totalRaw).padStart(8)} B  gzip ${String(totalGzip).padStart(7)} B / ${limit} B  br ${String(totalBrotli).padStart(7)} B`);
if (totalGzip > limit) {
  throw new Error(`Initial payload exceeds the 80 KiB gzip ceiling by ${totalGzip - limit} bytes`);
}

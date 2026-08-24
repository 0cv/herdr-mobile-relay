import { readFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { constants, gzipSync } from 'node:zlib';

// Release guard, not a platform limit: catch accidental bootstrap growth.
// Push-only notification art loads inside the service worker, not the page.
// Raised from 108 KiB for the hybrid transport (gateway pairing, DataChannel
// framing, path manager): the release contract ships one `assets/app.js`, so
// the direct-upgrade code cannot be split out of the bootstrap payload.
// Raised from 112 KiB for connection-path visibility and reconnect-code
// handling in 0.17.0, after removing the dead CSS that padded the old budget.
// Raised from 113 KiB for the no-echo prompt route (masked secret input, its
// relay capability gate), the F1-F12 pad, and GFM tables in conversation
// history: all three land in the single bootstrap `assets/app.js`.
// Raised from 115 KiB for Resize Session row leasing and the px-derived
// terminal width caps (issue #11), which also land in the bootstrap payload.
// Raised from 116 KiB for the conversation-history bottom pin (issue #12): the
// observer that owns the pin, its scroll tracker and the stream box it watches
// measure +126 B gzip in `assets/app.js` and +2 B in `assets/app.css`, and the
// released 0.17.4 payload left only 107 B under the old ceiling.
// Raised from 117 KiB for the Conversation History prompt composer (issue #13):
// prompt dispatch safety, interaction locks and image attachment add 1,525 B
// gzip to the released 0.17.5 baseline, which had 1,208 B of headroom.
// Raised from 118 KiB for authoritative workspace topology and the complete
// workspace/worktree manager (issue #14): empty workspace cards, safe
// destructive confirmations, directory selection, and worktree create/open
// flows add one project-level control surface to the single application bundle.
// Raised from 123 KiB for the native-style workspace follow-up: shared
// long-press drag ordering, modal create/worktree flows, and nested linked
// worktrees add 2,750 B gzip to the 0.17.6 bundle.
// Raised from 126 KiB for the worktree tree presentation (issue #14 review):
// connector rails, informative-only provenance/path rows, and the flat-Git
// worktree gating add 482 B gzip to the 0.17.8 bundle, 19 B past the ceiling.
// The gzip figure is the measuring runtime's zlib, not a property of the
// bundle: the same bytes measure ~300 B larger under Bun than under Node, so
// compare numbers only across runs on the same runtime (the repo uses Bun).
const limitKiB = 127;
const limit = limitKiB * 1024;
const root = resolve(process.argv[2] || 'dist');
const files = ['index.html', 'assets/app.js', 'assets/app.css'];
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
  throw new Error(`Initial payload exceeds the ${limitKiB} KiB gzip ceiling by ${totalGzip - limit} bytes`);
}

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
// Raised from 127 KiB for compact conversation tool cards and pending-lease
// terminal recovery: payload formatting/clamping and bounded resize handling
// both ship in the single bootstrap bundle.
// Raised from 128 KiB for paired-device administration, durable notification
// policy, validated attachment batches, exact target routing, local speech,
// wake lock, and OMO plan UI added to the required single application bundle.
// Raised from 150 KiB for stale-width terminal tables: rows wider than the
// leased pane keep their cell grid inside per-row lockstep scrollers instead
// of wrapping into misaligned fragments.
// Raised from 151 KiB for on-device diagnosability and the speak flow:
// uncaught errors and store invariant violations report on a raw-DOM banner
// (phones have no console), and reading responses aloud gained sanitized
// speech text, an automatic phone-language voice, and failure toasts.
// Raised from 152 KiB for screen-off speech: a near-silent keepalive stream
// keeps the tab audible so reading aloud survives the phone sleeping, with
// lock-screen Stop controls and sentence-chunked utterances under the
// Android TTS input limit.
// Raised from 153 KiB for the relay computer voice: sentence fragments are
// synthesized on the relay and streamed here encrypted, then played as real
// media - the only speech path Android keeps alive with the screen off.
// The pairing QR is encoded by the relay, so the app only unpacks a bitmap.
// Raised from 154 KiB for phone-managed speech voices and for the recovery
// path that stops a refused pairing from reconnecting forever.
// Raised from 155 KiB for the review hardening cutover: invitation-safe pairing,
// exact push/watch routing, cancellable relay speech, and upload recovery add
// 217 B gzip after moving attachment hashing out of the UI thread.
// Raised from 156 KiB for the iOS Home Screen handoff, which holds the one-use
// setup key for the installed copy and explains the waiting state, and for
// reader speech from the agent's transcript instead of the relay copy command.
// Raised from 157 KiB for home-screen directory rows: each agent card now
// prints its working directory against the relay's home directory instead of
// repeating the computer name, which adds 395 B gzip to the 0.20.5 bundle and
// lands the 0.20.6 payload exactly on the old ceiling. Script and stylesheet
// names carry a content hash, so index.html's compressed size drifts a couple
// of bytes per build: a release sitting on the limit fails the next one.
const limitKiB = 158;
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

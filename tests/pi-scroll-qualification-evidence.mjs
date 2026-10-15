import { createHash } from "node:crypto";
import { lstat, readFile, realpath, readdir } from "node:fs/promises";
import { dirname, join, resolve, sep } from "node:path";

export const matrix = Object.freeze({
  H1: { phases: ["N", "B"], kinds: ["real"], producer: "real.spec.ts:regularHistory", evidence: ["native-session", "recent-history", "dom-older-marker"] },
  H2: { phases: ["N", "B"], kinds: ["real"], producer: "real.spec.ts:unknownHistory", evidence: ["bridge-withhold", "dom-older-marker", "receiver-no-wheel"] },
  H3: { phases: ["B"], kinds: ["real"], producer: "real.spec.ts:historyTransitions", evidence: ["regular-prior", "fullscreen-redraws", "local-dom", "automatic-dom"] },
  H4: { phases: ["B"], kinds: ["real"], producer: "real.spec.ts:conversationHistory", evidence: ["native-session", "history-dom", "session-reference"] },
  W1: { phases: ["N"], kinds: ["real"], producer: "TestP1CapturedWireSchema", evidence: ["consumed-schema", "selected-status", "unary-bytes"] },
  W2: { phases: ["U", "N"], kinds: ["derived-wire-negatives"], producer: "TestP1OfflineDerivedWireNegatives/TestP1CapturedWireSchema", evidence: ["captured-document", "derived-refusals"] },
  W3: { phases: ["N", "B"], kinds: ["real"], producer: "pi-scroll-evidence-extension.mjs/real.spec.ts", evidence: ["startup-output", "isolated-button-window", "request-write", "pi-received-bytes"] },
  W4: { phases: ["U"], kinds: ["controlled-socket-faults"], producer: "TestP1OfflineRawFailureDispositions", evidence: ["prewrite-faults", "short-write", "malformed-and-late-ack", "no-retry"] },
  G1: { phases: ["N"], kinds: ["real"], producer: "TestDisposablePiScrollFoundation", evidence: ["process-bracket", "ps-stty", "grid", "pi-terminal-dimensions"] },
  G2: { phases: ["N"], kinds: ["real"], producer: "TestDisposablePiRowLeaseScrollIntegration", evidence: ["row-lease", "restoration", "callbacks", "stale-refusals", "receiver-no-wheel"] },
  G3: { phases: ["N", "B"], kinds: ["real"], producer: "TestDisposablePiRowLeaseScrollIntegration/real.spec.ts:movement", evidence: ["four-raw-frames", "ordered-markers", "independent-centers", "pi-received-bytes"] },
  G4: { phases: ["B"], kinds: ["real"], producer: "real.spec.ts:viewportLease", evidence: ["viewport-change", "actual-lease-state", "measured-cells", "restoration"] },
  L1: { phases: ["N"], kinds: ["real"], producer: "pi-scroll-qualification.sh:lifecycle", evidence: ["saved-and-cli-mode", "live-settings", "overlay-refusal", "reload", "shutdown-relaunch"] },
  L2: { phases: ["N", "U"], kinds: ["real", "controlled-identity-negatives"], producer: "pi-scroll-qualification.sh:bridgeLifecycle", evidence: ["bridge-absence", "mismatched-session", "editor-group", "widget-zero-rows", "catalog"] },
  A1: { phases: ["B", "U"], kinds: ["real", "controlled-target-negatives"], producer: "real.spec.ts:readerAuthorization", evidence: ["paired-reader", "disabled-controls", "server-denial", "target-refusals"] },
  Q1: { phases: ["M", "U", "N"], kinds: ["controlled-queue-faults", "real"], producer: "TestP1OfflineQueue/latency-fixture", evidence: ["active-pending-latest", "cancellations", "ordinary-tick-counts"] },
  P1: { phases: ["N"], kinds: ["real"], producer: "TestDisposablePiScrollFoundation/TestDisposablePiReadOnlyLatency", evidence: ["six-foundation", "sixteen-latency", "deadlines", "distributions-drops"] },
  S1: { phases: ["M", "U", "N", "B"], kinds: ["controlled-read-watch-faults", "real"], producer: "real.spec.ts:reconnect/TestP1OfflineReadWatch", evidence: ["initial-full-delta", "mode-only", "delayed-read", "resize", "reconnect"] },
  S2: { phases: ["B"], kinds: ["real"], producer: "real.spec.ts:noIncidentalInput", evidence: ["dom-scroll", "pan-selection", "jump-bottom", "history-navigation", "receiver-no-wheel"] },
  D1: { phases: ["S"], kinds: ["source"], producer: "source-scope", evidence: ["Pi-buttons-only", "shared-view-menu-center-races", "no-P2-P3"] },
});

export const slice2SourceGaps = Object.freeze([
  "H1/H2/H3/H4 DOM and Conversation History journeys; W3 and G3 browser windows; G4 viewport lease; A1 paired reader and its target refusals; S1 read/watch and reconnect; S2 no incidental input",
  "Private Vite build config",
  "Pinned private Playwright derivation and the proof of every reachable close path",
  "Final cross-producer matrix integration",
]);

export const sourceGaps = Object.freeze([
  ...slice2SourceGaps,
  "slice1 W3/R8: pre-factory startup output exclusion and final terminal shutdown output are not proved by the passive observer",
  "slice1 R1/R3/R4/R5: launch-input emission, receipt-pinned binary dispatch/0400-to-0500 transition and incarnation driver wiring remain incomplete",
  "slice1 R7/R11/R12/R13/R14: incremental receiver chunks, quiet sender windows, native history/bridge controls and gate-recomputed derivations remain incomplete",
  "slice1 L1/L2/Q1: exact settings/overlay/editor controls, widget geometry, latest-pending cancellation and ordinary-tick integration remain incomplete",
  "slice1 offline contracts: default/owned/order validation fixtures and the complete nineteen-case JS selection remain incomplete",
]);

export function requireSourceCandidateComplete() {
  if (sourceGaps.length) fail(`incomplete P1 source candidate: ${sourceGaps.join("; ")}`);
}

const roles = ["tmux-server", "tmux-bootstrap", "herdr-launcher", "herdr-server", "pi-agent"];
const sha256 = bytes => createHash("sha256").update(bytes).digest("hex");
const fail = message => { throw new Error(message); };
export function exactKeys(value, keys, name) {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).sort().join("\0") !== [...keys].sort().join("\0")) fail(`${name}: unexpected or missing fields`);
}
export function overlaps(a, b) { return a === b || a.startsWith(`${b}${sep}`) || b.startsWith(`${a}${sep}`); }

export function validateSuccessorPin(pin) {
  exactKeys(pin, ["stageRoot", "stageId", "runId", "ownerSha256", "ledgerSha256", "dispositionPath", "dispositionSha256"], "successor pin");
  for (const key of ["ownerSha256", "ledgerSha256", "dispositionSha256"]) if (!/^[a-f0-9]{64}$/.test(pin[key])) fail("successor pin digest malformed");
  for (const key of ["stageRoot", "dispositionPath"]) if (typeof pin[key] !== "string" || resolve(pin[key]) !== pin[key] || !/^[A-Za-z0-9_./-]+$/.test(pin[key])) fail("successor pin path malformed");
  if (!pin.stageId || !pin.runId || overlaps(pin.stageRoot, pin.dispositionPath)) fail("successor pin identity/namespace invalid");
  return pin;
}

export const evidenceRootLimits = Object.freeze({ evidence: 4*1024*1024, "evidence-pi": 2*1024*1024, "evidence-browser": 2*1024*1024 });

export function validateEvidenceInventory(roots, uid) {
  exactKeys(roots, Object.keys(evidenceRootLimits), "evidence roots");
  let combined = 0;
  for (const [name, cap] of Object.entries(evidenceRootLimits)) {
    const root = roots[name];
    if (root.uid !== uid || root.mode !== 0o700 || root.symlink || !root.directory || !Array.isArray(root.files)) fail("unsafe evidence root");
    let total = 0;
    const names = new Set();
    for (const file of root.files) {
      if (!/^[A-Za-z0-9][A-Za-z0-9._-]*\.json$/.test(file.name) || names.has(file.name) || file.uid !== uid || file.mode !== 0o600 || file.symlink || !file.regular || !Number.isSafeInteger(file.bytes) || file.bytes < 0 || file.bytes > 512*1024) fail("unsafe, duplicate or oversized evidence file");
      names.add(file.name);
      total += file.bytes;
    }
    if (total > cap) fail("producer evidence partition exceeded");
    combined += total;
  }
  if (combined > 8*1024*1024) fail("combined evidence limit exceeded");
  return combined;
}

export function validateBinaryTransition(compiled, transition, current, compileReceiptSha256, uid) {
  exactKeys(transition, ["schema_version", "kind", "phase", "binaryPath", "binarySha256", "compileReceiptSha256", "fromMode", "toMode"], "binary execution transition");
  if (transition.schema_version !== 1 || transition.kind !== "p1-binary-execution-transition" || !["U", "N"].includes(transition.phase) || transition.fromMode !== 0o400 || transition.toMode !== 0o500 || transition.compileReceiptSha256 !== compileReceiptSha256 || !/^[a-f0-9]{64}$/.test(compileReceiptSha256)) fail("invalid compile-to-execution receipt transition");
  if (compiled.mode !== 0o400 || !/^[a-f0-9]{64}$/.test(compiled.binarySha256) || transition.binarySha256 !== compiled.binarySha256 || transition.binaryPath !== compiled.binaryPath || resolve(transition.binaryPath) !== transition.binaryPath) fail("compiled binary pin changed");
  if (current.path !== transition.binaryPath || current.sha256 !== transition.binarySha256 || current.mode !== 0o500 || current.uid !== uid || current.symlink || current.regular !== true) fail("execution binary is not the same pinned bytes under execution-owned0500");
  return current.path;
}

export function keepPhaseFailure(state, phase, error, cleanup = false) {
  if (!error) return state;
  const failure = { phase, error: String(error) };
  if (cleanup) return { ...state, cleanupErrors: [...state.cleanupErrors, failure] };
  return state.firstFailure === null ? { ...state, firstFailure: failure } : state;
}

export function capturedRoles(bytes) {
  const lines = bytes.toString("utf8").trimEnd().split("\n");
  if (lines.shift() !== "role\tpid\tstart_identity\tcommand_identity") fail("historical process ledger header changed");
  const captured = [], pids = new Set();
  for (const line of lines) {
    const [role, pid, start, command, ...extra] = line.split("\t");
    if (!roles.includes(role) || captured.includes(role) || !/^[1-9][0-9]*$/.test(pid) || !Number.isSafeInteger(Number(pid)) || pids.has(pid) || !start || !command || extra.length) fail("historical process identity malformed");
    pids.add(pid);
    captured.push(role);
  }
  return captured.sort();
}

export async function validateHistoricalDisposition(disposition, context, io) {
  exactKeys(disposition, ["schema_version", "kind", "historicalClosureProved", "predecessor", "records", "allocations", "recoveries", "decision", "namespace", "cleanupContract"], "historical disposition");
  if (disposition.schema_version !== 1 || disposition.kind !== "successor-historical-disposition" || disposition.historicalClosureProved !== false) fail("historical uncertainty must remain explicit");
  const { pin, owner, records, allocations, terminal, uid } = context;
  exactKeys(disposition.predecessor, ["stageRoot", "stageId", "runId", "ownerSha256", "ledgerSha256"], "predecessor");
  for (const key of Object.keys(disposition.predecessor)) if (disposition.predecessor[key] !== pin[key]) fail(`predecessor ${key} changed`);
  exactKeys(disposition.records, ["count", "sha256", "sequence"], "historical records");
  if (records.length !== 14 || disposition.records.count !== 14 || disposition.records.sha256 !== pin.ledgerSha256 || JSON.stringify(disposition.records.sequence) !== JSON.stringify(records)) fail("immutable fourteen-record history changed");
  if (!Array.isArray(disposition.allocations) || disposition.allocations.length !== 6) fail("historical six-allocation accounting changed");
  for (const [index, allocation] of allocations.entries()) {
    const entry = disposition.allocations[index], outcome = terminal.get(allocation.attempt_id);
    exactKeys(entry, ["allocation", "originalOutcome", "sourceFiles", "capturedRoles", "uncapturedRoles"], "historical allocation");
    if (JSON.stringify(entry.allocation) !== JSON.stringify(allocation) || JSON.stringify(entry.originalOutcome) !== JSON.stringify(outcome)) fail("original allocation/outcome changed");
    const expectedFiles = [join(pin.stageRoot, "attempts", allocation.attempt_id, "attempt-owner.json")];
    if (outcome.root_created) expectedFiles.push(join(pin.stageRoot, "attempts", allocation.attempt_id, "root-created.json"), join(allocation.root_path, "owned-processes.tsv"));
    if (!Array.isArray(entry.sourceFiles) || entry.sourceFiles.length !== expectedFiles.length) fail("historical marker/process bindings incomplete");
    let captured = [];
    for (const [fileIndex, path] of expectedFiles.entries()) {
      const source = entry.sourceFiles[fileIndex];
      exactKeys(source, ["path", "sha256"], "historical source");
      if (source.path !== path || !/^[a-f0-9]{64}$/.test(source.sha256)) fail("historical source namespace changed");
      const bytes = await io.readOwnedBytes(path, uid);
      if (sha256(bytes) !== source.sha256) fail("historical source bytes changed");
      if (path.endsWith("owned-processes.tsv")) captured = capturedRoles(bytes);
    }
    const uncaptured = Object.fromEntries(roles.filter(role => !captured.includes(role)).map(role => [role, "unknown"]));
    if (JSON.stringify(entry.capturedRoles) !== JSON.stringify(captured) || JSON.stringify(entry.uncapturedRoles) !== JSON.stringify(uncaptured)) fail("historical missing identities reconstructed");
  }
  if (!Array.isArray(disposition.recoveries) || disposition.recoveries.length === 0) fail("scoped historical recovery bindings missing");
  const recoveryDirectories = [];
  for (const recovery of disposition.recoveries) {
    exactKeys(recovery, ["path", "sha256", "scope", "observedAt"], "scoped recovery");
    if (!recovery.scope || !Number.isFinite(Date.parse(recovery.observedAt)) || !/^[a-f0-9]{64}$/.test(recovery.sha256) || resolve(recovery.path) !== recovery.path) fail("invalid scoped recovery");
    if (sha256(await io.readHistoricalBytes(recovery.path, uid)) !== recovery.sha256) fail("scoped recovery changed");
    recoveryDirectories.push(dirname(recovery.path));
  }
  exactKeys(disposition.namespace, ["stage", "fixture", "bounds", "inputs", "outputs", "home", "config", "tmp", "profiles", "cache", "data", "state", "runtime", "localAllocationLimit", "historicalCount", "cumulativeCount"], "new namespace");
  const namespace = disposition.namespace;
  if (namespace.stage !== owner.stage_root || namespace.fixture !== join(owner.fixture_root_parent, `${owner.fixture_root_prefix}attempt-001`) || namespace.localAllocationLimit !== 1 || namespace.historicalCount !== 6 || namespace.cumulativeCount !== 7) fail("successor accounting changed");
  for (const key of ["home", "config", "tmp", "profiles", "cache", "data", "state", "runtime"]) if (namespace[key] !== join(namespace.fixture, key)) fail("private fixture path changed");
  const roots = [namespace.stage, namespace.fixture, namespace.bounds, namespace.inputs, namespace.outputs];
  const protectedRoots = [pin.stageRoot, ...allocations.map(a => a.root_path), ...recoveryDirectories, "/private/tmp/herdr-p1-go-compile-proposal-55dc0c8d/qualification", "/private/tmp/herdr-p1-seam-unit-qualification", "/private/tmp/herdr-p1-evidence-compile"];
  for (const [index, path] of roots.entries()) {
    if (typeof path !== "string" || resolve(path) !== path || !/^[A-Za-z0-9_./-]+$/.test(path) || protectedRoots.some(root => overlaps(path, root)) || roots.slice(index+1).some(root => overlaps(path, root))) fail("new namespace overlaps existing authority/effects");
    await io.canonicalDirectory(dirname(path), uid, false);
  }
  for (const socket of [join(namespace.fixture, "config/herdr/herdr.sock"), join(namespace.fixture, "config/herdr/herdr-client.sock"), join(namespace.fixture, `tmux/tmux-${uid}/default`)]) if (Buffer.byteLength(socket) >= 104) fail("new socket exceeds native path limit");
  exactKeys(disposition.cleanupContract, ["scope", "historicalSignals", "unknownIdentities", "ownClosureRequired"], "new-only cleanup");
  if (disposition.cleanupContract.scope !== "fresh-positively-owned-only" || disposition.cleanupContract.historicalSignals !== false || disposition.cleanupContract.unknownIdentities !== "retain-and-fail" || disposition.cleanupContract.ownClosureRequired !== true) fail("new cleanup contract widened");
  exactKeys(disposition.decision, ["path", "sha256"], "runtime decision pin");
  if (typeof disposition.decision.path !== "string" || resolve(disposition.decision.path) !== disposition.decision.path || disposition.decision.path === pin.dispositionPath || !disposition.decision.path.startsWith(`${namespace.inputs}${sep}`) || !/^[a-f0-9]{64}$/.test(disposition.decision.sha256)) fail("runtime decision must be separately pinned inside new inputs");
  const decisionBytes = await io.readPinnedEvidenceBytes(disposition.decision.path, uid);
  if (sha256(decisionBytes) !== disposition.decision.sha256) fail("runtime decision pin changed");
  const decision = JSON.parse(decisionBytes);
  exactKeys(decision, ["kind", "phase", "sourceOnly", "stageId", "runId", "historicalClosureProved", "localAllocationLimit", "historicalCount", "cumulativeCount", "namespace"], "native runtime decision");
  if (decision.kind !== "p1-native-runtime-authority" || decision.phase !== "N" || decision.sourceOnly !== false || decision.stageId !== owner.stage_id || decision.runId !== owner.run_id || decision.historicalClosureProved !== false || decision.localAllocationLimit !== 1 || decision.historicalCount !== 6 || decision.cumulativeCount !== 7 || JSON.stringify(decision.namespace) !== JSON.stringify(namespace)) fail("source-only approval cannot authorize native execution");
  return { historicalClosureProved: false, local_allocation_limit: 1, historical_count: 6, cumulative_count: 7, namespace, runtime_decision_path: disposition.decision.path, runtime_decision_sha256: disposition.decision.sha256 };
}

export async function readEvidenceFile(path, root, uid = process.getuid()) {
  if (resolve(path) !== path || !path.startsWith(`${root}${sep}`) || await realpath(path) !== path) fail("evidence path escaped owned root");
  const stat = await lstat(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600 || stat.size > 512*1024) fail("unsafe or oversized evidence");
  const bytes = await readFile(path);
  if (bytes.length > 512*1024) fail("evidence grew beyond cap");
  return { bytes, value: JSON.parse(bytes), sha256: sha256(bytes) };
}

export function checkWheelWindow(window) {
  exactKeys(window, ["source_kind", "direction", "columns", "rows", "pasteEnabled", "request", "receiver", "complete", "overflow", "ambiguous"], "isolated wheel window");
  if (window.source_kind !== "real" || window.complete !== true || window.overflow !== false || window.ambiguous !== false || window.pasteEnabled !== true || !["up", "down"].includes(window.direction) || !Number.isSafeInteger(window.columns) || !Number.isSafeInteger(window.rows) || window.columns < 1 || window.rows < 1) fail("wheel window is incomplete/ambiguous");
  const expected = `\x1b[<${window.direction === "up" ? 64 : 65};${Math.floor((window.columns+1)/2)};${Math.floor((window.rows+1)/2)}M`;
  const received = Buffer.from(window.receiver.base64, "base64");
  if (received.toString("base64") !== window.receiver.base64 || received.length !== window.receiver.byteCount || !received.equals(Buffer.from(expected)) || window.request.method !== "pane.send_text" || window.request.text !== expected || window.request.writeCalls !== 1 || window.request.written !== window.request.payloadBytes || !window.request.requestId || window.request.retries !== 0 || window.request.cliWriters !== 0) fail("sender/receiver/independent center mismatch");
  return expected;
}

function requireRawRecord(value, kind) {
  if (!value || value.schema_version !== 1 || value.kind !== kind || value.source_kind !== "real" || value.complete !== true || value.truncated !== false || value.ambiguous !== false || value.skipped !== false) fail(`${kind}: incomplete raw producer record`);
}

function rawBytes(value) {
  if (typeof value !== "string" || value.length > Math.ceil(512*1024/3)*4) fail("invalid raw byte encoding");
  const bytes = Buffer.from(value, "base64");
  if (bytes.toString("base64") !== value || bytes.length > 512*1024) fail("noncanonical or oversized raw bytes");
  return bytes;
}

function monotonicTime(value) {
  if (typeof value !== "string" || !/^[1-9][0-9]{0,29}$/.test(value)) fail("invalid monotonic observation time");
  return BigInt(value);
}

function wallTime(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value) || !Number.isFinite(Date.parse(value))) fail("invalid UTC sender observation time");
  return Date.parse(value);
}

export function deriveWheelWindow(sender, wireRecords, receiver, output, commands) {
  requireRawRecord(sender, "p1-sender-window");
  requireRawRecord(receiver, "p1-pi-received-bytes");
  requireRawRecord(output, "p1-pi-output-modes");
  exactKeys(sender.grid, ["columns", "rows", "epoch", "revision"], "sender grid");
  if (!sender.windowId || !sender.requestId || !sender.wireRecordFile || !["up", "down"].includes(sender.direction) || !Number.isSafeInteger(sender.grid.epoch) || sender.grid.epoch < 1 || !sender.grid.revision || [sender.grid.columns, sender.grid.rows].some(value => !Number.isSafeInteger(value) || value < 1 || value > 65535)) fail("unbound sender geometry/window");
  for (const frames of [sender.framesBefore, sender.framesAfter]) if (!Array.isArray(frames) || !frames.length || frames.some(path => typeof path !== "string" || !path)) fail("missing raw frame references");
  if (![sender.quietBeforeMs, sender.quietAfterMs].every(value => Number.isSafeInteger(value) && value > 0)) fail("missing quiet sender bounds");
  const startedMs = wallTime(sender.startedWall), endedMs = wallTime(sender.endedWall);
  if (endedMs < startedMs) fail("sender time order is ambiguous");
  const quietStartMs = startedMs - sender.quietBeforeMs;
  const quietEndMs = endedMs + 1 + sender.quietAfterMs;
  if (!Number.isSafeInteger(receiver.incarnation) || ![1, 2].includes(receiver.incarnation) || !Number.isSafeInteger(receiver.pid) || receiver.pid < 1 || receiver.incarnation !== output.incarnation || receiver.pid !== output.pid || receiver.firstSeq !== 1 || receiver.listenerRemovedNs !== null || receiver.droppedChunks !== 0 || receiver.overflow !== false || output.installedBeforeFirstWrite !== true || output.forwardedOnce !== true) fail("receiver/output lifetime is incomplete");
  if (!Array.isArray(receiver.chunks) || !receiver.chunks.length || receiver.chunks.length > 1024 || !Array.isArray(output.events) || output.events.length > 1024) fail("raw observer sequence is missing or oversized");
  let previousNs = monotonicTime(receiver.listenerInstalledNs), previousWallMs = -Infinity, totalBytes = 0;
  const selected = [];
  for (const [index, chunk] of receiver.chunks.entries()) {
    const observedNs = monotonicTime(chunk.hrtimeNs), bytes = rawBytes(chunk.base64);
    if (chunk.seq !== index+1 || observedNs <= previousNs || !Number.isSafeInteger(chunk.wallMs) || chunk.wallMs < previousWallMs || chunk.asciiExact !== true || chunk.byteCount !== bytes.length || !bytes.length) fail("receiver gap, loss or ambiguous order");
    previousNs = observedNs;
    previousWallMs = chunk.wallMs;
    totalBytes += bytes.length;
    if (chunk.wallMs >= quietStartMs && chunk.wallMs <= quietEndMs) selected.push({ bytes, observedNs });
  }
  if (totalBytes !== receiver.totalBytes || totalBytes > 256*1024 || !selected.length) fail("receiver totals/window do not match raw chunks");
  let pasteEnabled = false, previousModeNs = 0n;
  const modes = { "bracketed-paste": "2004", "sgr-mouse": "1006", "alt-screen": "1049" };
  for (const [index, event] of output.events.entries()) {
    const observedNs = monotonicTime(event.hrtimeNs), code = modes[event.mode];
    if (event.seq !== index+1 || observedNs <= previousModeNs || !code || typeof event.enabled !== "boolean" || !rawBytes(event.sequenceBase64).equals(Buffer.from(`\x1b[?${code}${event.enabled ? "h" : "l"}`))) fail("output mode derivation is ambiguous");
    previousModeNs = observedNs;
    if (observedNs >= selected[0].observedNs && observedNs <= selected.at(-1).observedNs) fail("terminal modes changed during received input");
    if (event.mode === "bracketed-paste" && observedNs < selected[0].observedNs) pasteEnabled = event.enabled;
  }
  if (!Array.isArray(wireRecords) || !wireRecords.length || wireRecords.length > 1024 || !Array.isArray(commands) || commands.length < 2 || commands.length > 128) fail("missing bounded sender/command observations");
  for (const command of commands) {
    if (!["ps", "stty"].includes(command.name) || !Array.isArray(command.args) || command.args.some(arg => typeof arg !== "string") || command.error || !Number.isSafeInteger(command.elapsed_ns) || command.elapsed_ns < 0) fail("CLI writer or incomplete geometry command observation");
    wallTime(command.started);
  }
  const writes = [], requestIds = new Set();
  for (const record of wireRecords) {
    if (record.kind !== "unary" || record.method !== "pane.send_text") continue;
    const recordStartedMs = wallTime(record.started);
    if (record.overflow !== false || !Number.isSafeInteger(record.duration_ns) || record.duration_ns < 0 || !record.request_id || requestIds.has(record.request_id)) fail("sender raw observations are incomplete or retried");
    requestIds.add(record.request_id);
    if (recordStartedMs <= quietEndMs && recordStartedMs + record.duration_ns/1e6 >= quietStartMs) writes.push(record);
  }
  if (writes.length !== 1) fail("quiet window does not contain exactly one sender request");
  const record = writes[0], payload = rawBytes(record.request_base64), responseBytes = rawBytes(record.response_base64);
  const request = JSON.parse(payload.toString("utf8")), response = JSON.parse(responseBytes.toString("utf8"));
  exactKeys(request, ["id", "method", "params"], "raw unary request");
  exactKeys(request.params, ["pane_id", "text"], "raw wheel params");
  exactKeys(response, ["id", "result"], "raw unary acknowledgement");
  exactKeys(response.result, ["type"], "raw wheel result");
  if (payload.at(-1) !== 10 || responseBytes.at(-1) !== 10 || request.id !== record.request_id || record.request_id !== sender.requestId || request.method !== "pane.send_text" || typeof request.params.pane_id !== "string" || !request.params.pane_id || record.write_calls !== 1 || record.written !== payload.length || record.wrote !== true || record.error || response.id !== request.id || response.result.type !== "ok" || wallTime(record.started) < startedMs || wallTime(record.started) + record.duration_ns/1e6 > endedMs+1) fail("raw sender/write/acknowledgement mismatch");
  const received = Buffer.concat(selected.map(chunk => chunk.bytes));
  const window = { source_kind: "real", direction: sender.direction, columns: sender.grid.columns, rows: sender.grid.rows, pasteEnabled, request: { method: request.method, text: request.params.text, writeCalls: record.write_calls, written: record.written, payloadBytes: payload.length, requestId: record.request_id, retries: writes.length-1, cliWriters: 0 }, receiver: { base64: received.toString("base64"), byteCount: received.length }, complete: true, overflow: false, ambiguous: false };
  const expected = checkWheelWindow(window);
  if (!rawBytes(sender.expectedSgrBase64).equals(Buffer.from(expected))) fail("raw sender expected center disagrees with independently measured grid");
  return window;
}

export async function checkMatrix(receipt, root) {
  requireSourceCandidateComplete();
  exactKeys(receipt, ["kind", "schema_version", "stageId", "runId", "rows", "cleanupComplete", "preservationMatched"], "matrix receipt");
  if (receipt.kind !== "p1-complete-evidence" || receipt.schema_version !== 1 || !receipt.stageId || !receipt.runId || receipt.cleanupComplete !== true || receipt.preservationMatched !== true) fail("qualification or own cleanup incomplete");
  exactKeys(receipt.rows, Object.keys(matrix), "matrix rows");
  const files = await readdir(root);
  let total = 0;
  for (const file of files) { const stat = await lstat(join(root, file)); if (!stat.isFile() || stat.size > 512*1024) fail("evidence directory contains unexpected/oversized entries"); total += stat.size; }
  if (total > 8*1024*1024) fail("total raw evidence cap exceeded");
  for (const [id, requirement] of Object.entries(matrix)) {
    const row = receipt.rows[id];
    exactKeys(row, ["result", "sourceKinds", "phases", "observations", "complete", "skipped", "truncated", "ambiguous", "gaps"], id);
    if (row.result !== "PASS" || row.complete !== true || row.skipped !== false || row.truncated !== false || row.ambiguous !== false || !Array.isArray(row.gaps) || row.gaps.length || !Array.isArray(row.sourceKinds) || requirement.kinds.some(kind => !row.sourceKinds.includes(kind)) || row.sourceKinds.some(kind => !requirement.kinds.includes(kind)) || JSON.stringify(row.phases) !== JSON.stringify(requirement.phases)) fail(`${id}: missing/skipped/truncated/ambiguous/mock-for-real is not PASS`);
    exactKeys(row.observations, requirement.evidence, `${id} observations`);
    for (const name of requirement.evidence) {
      const observation = await readEvidenceFile(row.observations[name], root);
      if (observation.value.complete !== true || !requirement.kinds.includes(observation.value.source_kind) || observation.value.truncated || observation.value.ambiguous || observation.value.skipped) fail(`${id}/${name}: raw observation unavailable`);
      if (name === "isolated-button-window") checkWheelWindow(observation.value);
    }
  }
  return { complete: true, rows: Object.keys(matrix), historicalClosureProved: false };
}

import { test, expect } from "bun:test";
import { createHash } from "node:crypto";
import { capturedRoles, checkWheelWindow, exactKeys, matrix, overlaps, requireSourceCandidateComplete, sourceGaps, validateHistoricalDisposition, validateSuccessorPin, validateEvidenceInventory, validateBinaryTransition, keepPhaseFailure, deriveWheelWindow } from "./pi-scroll-qualification-evidence.mjs";
import { requireLaunchArgv } from "./pi-scroll-qualification-identity.mjs";
import { EventEmitter } from "node:events";
import { createPassiveObservers } from "./pi-scroll-evidence-extension.mjs";

const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const clone = value => JSON.parse(JSON.stringify(value));

function syntheticDisposition() {
  const files = new Map();
  const pin = { stageRoot: "/private/tmp/p1-mock-old", stageId: "old", runId: "old-run", ownerSha256: "a".repeat(64), ledgerSha256: "b".repeat(64) };
  const owner = { stage_root: "/private/tmp/p1-mock-new", stage_id: "new", run_id: "new-run", fixture_root_parent: "/private/tmp", fixture_root_prefix: "herdr-scroll-p1-new-" };
  const allocations = [], terminal = new Map(), records = [], entries = [];
  for (let i = 1; i <= 6; i++) {
    const id = `attempt-${String(i).padStart(3, "0")}`;
    const allocation = { seq: records.length+1, kind: "allocation", stage_id: pin.stageId, run_id: pin.runId, attempt_number: i, attempt_id: id, root_path: `/private/tmp/p1-mock-old-${id}` };
    records.push(allocation);
    const outcome = { seq: records.length+1, kind: "outcome", stage_id: pin.stageId, run_id: pin.runId, attempt_id: id, result: "blocked", root_created: true, cleanup_complete: i === 4 };
    records.push(outcome);
    if (i <= 2) records.push({ seq: records.length+1, kind: "cleanup-recovery", stage_id: pin.stageId, run_id: pin.runId, attempt_id: id, original_outcome_seq: outcome.seq });
    allocations.push(allocation);
    terminal.set(id, outcome);
    const sourceFiles = [];
    for (const [path, bytes] of [
      [`${pin.stageRoot}/attempts/${id}/attempt-owner.json`, Buffer.from(JSON.stringify(allocation))],
      [`${pin.stageRoot}/attempts/${id}/root-created.json`, Buffer.from(JSON.stringify(allocation))],
      [`${allocation.root_path}/owned-processes.tsv`, Buffer.from(`role\tpid\tstart_identity\tcommand_identity\ntmux-server\t${100+i}\t2026-10-06 12:00:00\tsynthetic-tmux\n`)],
    ]) {
      files.set(path, bytes);
      sourceFiles.push({ path, sha256: hash(bytes) });
    }
    entries.push({ allocation, originalOutcome: outcome, sourceFiles, capturedRoles: ["tmux-server"], uncapturedRoles: { "tmux-bootstrap": "unknown", "herdr-launcher": "unknown", "herdr-server": "unknown", "pi-agent": "unknown" } });
  }
  const fixture = `${owner.fixture_root_parent}/${owner.fixture_root_prefix}attempt-001`;
  const namespace = { stage: owner.stage_root, fixture, bounds: "/private/tmp/p1-mock-bounds", inputs: "/private/tmp/p1-mock-inputs", outputs: "/private/tmp/p1-mock-outputs", ...Object.fromEntries(["home", "config", "tmp", "profiles", "cache", "data", "state", "runtime"].map(name => [name, `${fixture}/${name}`])), localAllocationLimit: 1, historicalCount: 6, cumulativeCount: 7 };
  const recovery = { path: "/private/tmp/p1-mock-recovery/scoped.txt", sha256: hash(Buffer.from("synthetic scoped observation")), scope: "synthetic mock observation, not historical closure", observedAt: "2026-10-07T01:00:00Z" };
  files.set(recovery.path, Buffer.from("synthetic scoped observation"));
  const decision = { kind: "p1-native-runtime-authority", phase: "N", sourceOnly: false, stageId: owner.stage_id, runId: owner.run_id, historicalClosureProved: false, localAllocationLimit: 1, historicalCount: 6, cumulativeCount: 7, namespace };
  const decisionPath = `${namespace.inputs}/synthetic-decision.json`;
  files.set(decisionPath, Buffer.from(JSON.stringify(decision)));
  const disposition = { schema_version: 1, kind: "successor-historical-disposition", historicalClosureProved: false, predecessor: pin, records: { count: 14, sha256: pin.ledgerSha256, sequence: records }, allocations: entries, recoveries: [recovery], decision: { path: decisionPath, sha256: hash(files.get(decisionPath)) }, namespace, cleanupContract: { scope: "fresh-positively-owned-only", historicalSignals: false, unknownIdentities: "retain-and-fail", ownClosureRequired: true } };
  const read = async path => { if (!files.has(path)) throw new Error("unexpected synthetic path"); return files.get(path); };
  return { disposition, context: { pin, owner, records, allocations, terminal, uid: 502 }, files, io: { canonicalDirectory: async () => {}, readOwnedBytes: read, readHistoricalBytes: read, readPinnedEvidenceBytes: read } };
}

test("exact incarnation argv", () => {
  const bridge = "/private/tmp/fixture/pi/bridge.mjs";
  const first = ["pi", "--offline", "--tui-mode", "regular", "--extension", bridge];
  const second = ["pi", "--offline", "--extension", bridge];
  expect(requireLaunchArgv(first, bridge, 1)).toEqual(first);
  expect(requireLaunchArgv(second, bridge, 2)).toEqual(second);
  for (const [argv, incarnation] of [[first, 2], [second, 1], [[...first, "--print"], 1], [first, 0], [first, 3]]) expect(() => requireLaunchArgv(argv, bridge, incarnation)).toThrow();
});

test("disposition binds immutable history and keeps uncaptured roles unknown", async () => {
  const fixture = syntheticDisposition();
  const result = await validateHistoricalDisposition(fixture.disposition, fixture.context, fixture.io);
  expect(result.historicalClosureProved).toBe(false);
  expect(result.cumulative_count).toBe(7);
  expect(fixture.disposition.allocations[5].originalOutcome.cleanup_complete).toBe(false);
  expect(fixture.disposition.allocations.every(a => a.uncapturedRoles["pi-agent"] === "unknown")).toBe(true);
});

test("disposition rejects fabricated closure, role reconstruction, changed records and source-only decisions", async () => {
  for (const mutation of [
    d => { d.historicalClosureProved = true; },
    d => { d.allocations[5].originalOutcome.cleanup_complete = true; },
    d => { d.allocations[0].uncapturedRoles["pi-agent"] = "absent"; },
    d => { d.allocations[0].capturedRoles.push("pi-agent"); },
    d => { d.records.sequence.pop(); },
    d => { d.records.count = 13; },
    d => { d.allocations[0].sourceFiles[0].sha256 = "0".repeat(64); },
    d => { d.namespace.localAllocationLimit = 6; },
    d => { d.namespace.cumulativeCount = 1; },
    d => { d.namespace.inputs = d.namespace.outputs; },
    d => { d.namespace.stage = d.predecessor.stageRoot; },
    d => { d.cleanupPath = "/old/fallback"; },
  ]) {
    const fixture = syntheticDisposition(), value = clone(fixture.disposition);
    mutation(value);
    await expect(validateHistoricalDisposition(value, fixture.context, fixture.io)).rejects.toThrow();
  }
  const fixture = syntheticDisposition();
  const decision = JSON.parse(fixture.files.get(fixture.disposition.decision.path));
  decision.phase = "S";
  decision.sourceOnly = true;
  const bytes = Buffer.from(JSON.stringify(decision));
  fixture.files.set(fixture.disposition.decision.path, bytes);
  fixture.disposition.decision.sha256 = hash(bytes);
  await expect(validateHistoricalDisposition(fixture.disposition, fixture.context, fixture.io)).rejects.toThrow("source-only");
});

test("captured roles require actual unique complete identity rows but do not infer omitted roles", () => {
  expect(capturedRoles(Buffer.from("role\tpid\tstart_identity\tcommand_identity\n"))).toEqual([]);
  for (const row of ["pi-agent\t0\tstart\tpi", "pi-agent\t10\t\tpi", "invented\t10\tstart\tpi", "pi-agent\t10\tstart\tpi\npi-agent\t11\tstart\tpi"]) expect(() => capturedRoles(Buffer.from(`role\tpid\tstart_identity\tcommand_identity\n${row}\n`))).toThrow();
  expect(overlaps("/private/tmp/old", "/private/tmp/old-new")).toBe(false);
  expect(overlaps("/private/tmp/old", "/private/tmp/old/new")).toBe(true);
  expect(() => exactKeys({ kind: "source", extra: true }, ["kind"], "mock")).toThrow();
});

function wheelWindow() {
  const text = "\x1b[<64;50;18M";
  return { source_kind: "real", direction: "up", columns: 100, rows: 36, pasteEnabled: true, request: { method: "pane.send_text", text, writeCalls: 1, written: 100, payloadBytes: 100, requestId: "synthetic-request", retries: 0, cliWriters: 0 }, receiver: { base64: Buffer.from(text).toString("base64"), byteCount: Buffer.byteLength(text) }, complete: true, overflow: false, ambiguous: false };
}

test("controlled window parser compares received bytes to an independent integer center", () => {
  expect(checkWheelWindow(wheelWindow())).toBe("\x1b[<64;50;18M");
  for (const mutation of [
    w => { w.request.writeCalls = 2; },
    w => { w.request.retries = 1; },
    w => { w.request.cliWriters = 1; },
    w => { w.receiver.base64 = Buffer.from("\x1b[200~\x1b[<64;50;18M\x1b[201~").toString("base64"); },
    w => { w.pasteEnabled = false; },
    w => { w.source_kind = "mock"; },
    w => { w.overflow = true; },
    w => { w.ambiguous = true; },
    w => { w.complete = false; },
    w => { w.columns = 102; },
  ]) { const window = wheelWindow(); mutation(window); expect(() => checkWheelWindow(window)).toThrow(); }
});

function syntheticRawWheelWindow(direction = "up") {
  const text = `\x1b[<${direction === "up" ? 64 : 65};50;18M`;
  const start = "2026-10-06T12:00:00.100Z", end = "2026-10-06T12:00:00.120Z";
  const requestId = "synthetic-wheel-request";
  const payload = Buffer.from(`${JSON.stringify({ id: requestId, method: "pane.send_text", params: { pane_id: "synthetic-pane", text } })}\n`);
  const response = Buffer.from(`${JSON.stringify({ id: requestId, result: { type: "ok" } })}\n`);
  const raw = (kind, fields) => ({ schema_version: 1, kind, source_kind: "real", complete: true, truncated: false, ambiguous: false, skipped: false, ...fields });
  const sender = raw("p1-sender-window", { windowId: "synthetic-window", direction, grid: { columns: 100, rows: 36, epoch: 1, revision: "synthetic-revision" }, expectedSgrBase64: Buffer.from(text).toString("base64"), requestId, wireRecordFile: "synthetic-wire.json", startedWall: start, endedWall: end, quietBeforeMs: 20, quietAfterMs: 20, framesBefore: ["synthetic-before.json"], framesAfter: ["synthetic-after.json"] });
  const wire = [{ kind: "unary", method: "pane.send_text", request_id: requestId, started: start, duration_ns: 10*1e6, request_base64: payload.toString("base64"), response_base64: response.toString("base64"), write_calls: 1, written: payload.length, wrote: true, overflow: false }];
  const receiver = raw("p1-pi-received-bytes", { incarnation: 1, pid: 123, listenerInstalledNs: "1", listenerRemovedNs: null, firstSeq: 1, chunks: [{ seq: 1, hrtimeNs: "100", wallMs: Date.parse(start)+5, byteCount: Buffer.byteLength(text), base64: Buffer.from(text).toString("base64"), asciiExact: true }], droppedChunks: 0, totalBytes: Buffer.byteLength(text), overflow: false });
  const output = raw("p1-pi-output-modes", { incarnation: 1, pid: 123, installedBeforeFirstWrite: true, forwardedOnce: true, events: [{ seq: 1, hrtimeNs: "10", mode: "bracketed-paste", enabled: true, sequenceBase64: Buffer.from("\x1b[?2004h").toString("base64") }] });
  const commands = ["ps", "stty"].map(name => ({ name, args: [], output_base64: "", started: start, elapsed_ns: 1 }));
  return { text, sender, wire, receiver, output, commands };
}

function deriveSyntheticWindow(fixture) {
  return deriveWheelWindow(fixture.sender, fixture.wire, fixture.receiver, fixture.output, fixture.commands);
}

function replaceSyntheticReceived(fixture, text) {
  const bytes = Buffer.from(text);
  fixture.receiver.chunks[0].base64 = bytes.toString("base64");
  fixture.receiver.chunks[0].byteCount = bytes.length;
  fixture.receiver.totalBytes = bytes.length;
}

test("receiver window accepts exactly one expected report", () => {
  for (const direction of ["up", "down"]) {
    const fixture = syntheticRawWheelWindow(direction), window = deriveSyntheticWindow(fixture);
    expect(checkWheelWindow(window)).toBe(fixture.text);
    expect(window.request.payloadBytes).toBe(Buffer.from(fixture.wire[0].request_base64, "base64").length);
    expect(window.request.requestId).toBe(fixture.wire[0].request_id);
    expect(window.request.writeCalls).toBe(fixture.wire[0].write_calls);
    expect(window.request.retries).toBe(0);
    expect(window.request.cliWriters).toBe(0);
    const bytes = Buffer.from(fixture.text), split = 6;
    fixture.receiver.chunks = [bytes.subarray(0, split), bytes.subarray(split)].map((part, index) => ({ seq: index+1, hrtimeNs: String(100+index), wallMs: Date.parse(fixture.sender.startedWall)+5+index, byteCount: part.length, base64: part.toString("base64"), asciiExact: true }));
    expect(checkWheelWindow(deriveSyntheticWindow(fixture))).toBe(fixture.text);
  }
});

test("receiver window rejects extra, release, enabling or paste-delimited bytes", () => {
  for (const transform of [text => text+text, text => text+"\x1b[<64;50;18m", text => "\x1b[?1006h"+text, text => `\x1b[200~${text}\x1b[201~`, text => text+"unexpected input"]) {
    const fixture = syntheticRawWheelWindow();
    replaceSyntheticReceived(fixture, transform(fixture.text));
    expect(() => deriveSyntheticWindow(fixture)).toThrow();
  }
});

test("receiver window rejects overflow, listener gaps and ambiguous order", () => {
  for (const mutate of [
    f => { f.receiver.overflow = true; },
    f => { f.receiver.droppedChunks = 1; },
    f => { f.receiver.listenerRemovedNs = "200"; },
    f => { f.receiver.listenerInstalledNs = "100"; },
    f => { f.receiver.firstSeq = 2; },
    f => { f.receiver.chunks[0].seq = 2; },
    f => { f.receiver.chunks[0].asciiExact = false; },
    f => { f.receiver.chunks[0].byteCount++; },
    f => { f.receiver.totalBytes++; },
    f => { f.receiver.chunks[0].base64 += "="; },
    f => { f.receiver.chunks[0].wallMs = Date.parse(f.sender.endedWall)+100; },
    f => { f.sender.quietBeforeMs = 0; },
    f => { f.sender.endedWall = "2026-10-06T12:00:00.090Z"; },
    f => { f.output.incarnation = 2; },
    f => { f.output.pid++; },
    f => { f.output.installedBeforeFirstWrite = null; },
    f => { f.output.events = []; },
    f => { f.output.events[0].enabled = false; },
    f => { f.output.events[0].sequenceBase64 = Buffer.from("\x1b[?2004l").toString("base64"); },
    f => { f.output.events[0].hrtimeNs = "100"; },
    f => { f.output.forwardedOnce = false; },
    f => { f.wire.push(clone(f.wire[0])); },
    f => { f.wire.push({ ...clone(f.wire[0]), request_id: "extra-request" }); },
    f => { f.wire[0].write_calls = 2; },
    f => { f.wire[0].written--; },
    f => { f.wire[0].overflow = true; },
    f => { f.wire[0].duration_ns = 50*1e6; },
    f => { f.wire[0].response_base64 = Buffer.from('{"id":"wrong","result":{"type":"ok"}}\n').toString("base64"); },
    f => { f.wire[0].request_base64 = Buffer.from('{"id":"wrong","method":"pane.send_text","params":{"pane_id":"synthetic-pane","text":"wrong"}}\n').toString("base64"); },
    f => { f.commands.push({ name: "herdr", args: ["pane", "send-text"], started: f.sender.startedWall, elapsed_ns: 1 }); },
    f => { f.sender.expectedSgrBase64 = Buffer.from("\x1b[<64;51;18M").toString("base64"); },
  ]) {
    const fixture = syntheticRawWheelWindow();
    mutate(fixture);
    expect(() => deriveSyntheticWindow(fixture)).toThrow();
  }
});

test("extension observers are transparent and forward output once", () => {
  const input = new EventEmitter(), output = new EventEmitter();
  output.columns = 100; output.rows = 36;
  const forwarded = [], callback = () => {};
  output.write = function (...args) { forwarded.push({ receiver: this, args }); return false; };
  const original = output.write;
  let clock = 0;
  const observers = createPassiveObservers(input, output, { nowNs: () => String(++clock), wallMs: () => clock, incarnation: 1, pid: 123 });
  expect(output.write("\x1b[?20", "utf8", callback)).toBe(false);
  expect(output.write("04h")).toBe(false);
  input.emit("data", "\x1b[<64;50;18M");
  const records = observers.snapshot();
  expect(forwarded.length).toBe(2);
  expect(forwarded[0].receiver).toBe(output);
  expect(forwarded[0].args).toEqual(["\x1b[?20", "utf8", callback]);
  expect(records.output.events).toHaveLength(1);
  expect(records.output.events[0].mode).toBe("bracketed-paste");
  expect(records.receiver.chunks[0].base64).toBe(Buffer.from("\x1b[<64;50;18M").toString("base64"));
  expect(records.output.installedBeforeFirstWrite).toBeNull();
  observers.remove();
  expect(output.write).toBe(original);
  expect(input.listenerCount("data")).toBe(0);
  expect(observers.snapshot().receiver.complete).toBe(false);
});

test("successor pin requires disposition fields and rejects cleanupPath", () => {
  const pin = { stageRoot: "/private/tmp/mock-old", stageId: "old", runId: "old-run", ownerSha256: "a".repeat(64), ledgerSha256: "b".repeat(64), dispositionPath: "/private/tmp/mock-inputs/disposition.json", dispositionSha256: "c".repeat(64) };
  expect(validateSuccessorPin(pin)).toEqual(pin);
  for (const mutation of [
    p => { p.cleanupPath = p.dispositionPath; },
    p => { p.cleanupSha256 = p.dispositionSha256; },
    p => { delete p.dispositionPath; },
    p => { p.dispositionSha256 = "invalid"; },
    p => { p.dispositionPath = `${p.stageRoot}/disposition.json`; },
  ]) { const value = clone(pin); mutation(value); expect(() => validateSuccessorPin(value)).toThrow(); }
});

test("binary transition binds compile-owned0400 to execution-owned0500 identical bytes", () => {
  const compiled = { binaryPath: "/private/tmp/mock-compile/app.test", binarySha256: "a".repeat(64), mode: 0o400 };
  const receiptHash = "b".repeat(64);
  const transition = { schema_version: 1, kind: "p1-binary-execution-transition", phase: "N", binaryPath: compiled.binaryPath, binarySha256: compiled.binarySha256, compileReceiptSha256: receiptHash, fromMode: 0o400, toMode: 0o500 };
  const current = { path: compiled.binaryPath, sha256: compiled.binarySha256, mode: 0o500, uid: 502, regular: true, symlink: false };
  expect(validateBinaryTransition(compiled, transition, current, receiptHash, 502)).toBe(compiled.binaryPath);
  for (const mutation of [v => { v.sha256 = "c".repeat(64); }, v => { v.mode = 0o400; }, v => { v.mode = 0o700; }, v => { v.uid = 0; }, v => { v.symlink = true; }]) {
    const value = clone(current); mutation(value);
    expect(() => validateBinaryTransition(compiled, transition, value, receiptHash, 502)).toThrow();
  }
  expect(() => validateBinaryTransition(compiled, transition, current, "c".repeat(64), 502)).toThrow();
});

test("driver phase receipts keep the first failure and later cleanup errors separately", () => {
  const initial = { firstFailure: null, cleanupErrors: [] };
  const failed = keepPhaseFailure(initial, "N", "first raw error");
  const later = keepPhaseFailure(failed, "N", "second raw error");
  const cleanup = keepPhaseFailure(later, "cleanup", "unresolved owned child", true);
  expect(cleanup.firstFailure).toEqual({ phase: "N", error: "first raw error" });
  expect(cleanup.cleanupErrors).toEqual([{ phase: "cleanup", error: "unresolved owned child" }]);
  expect(initial).toEqual({ firstFailure: null, cleanupErrors: [] });
});

test("evidence roots enforce mode, symlink, per-file and combined caps", () => {
  const inventory = () => Object.fromEntries(["evidence", "evidence-pi", "evidence-browser"].map(name => [name, { uid: 502, mode: 0o700, directory: true, symlink: false, files: [{ name: "raw.json", uid: 502, mode: 0o600, regular: true, symlink: false, bytes: 512*1024 }] }]));
  expect(validateEvidenceInventory(inventory(), 502)).toBe(3*512*1024);
  for (const mutation of [
    r => { r.evidence.mode = 0o755; },
    r => { r.evidence.uid = 0; },
    r => { r.evidence.symlink = true; },
    r => { r.evidence.files[0].bytes++; },
    r => { r.evidence.files[0].mode = 0o644; },
    r => { r.evidence.files[0].symlink = true; },
    r => { r.evidence.files[0].name = "../raw.json"; },
    r => { r.evidence.files.push(clone(r.evidence.files[0])); },
    r => { r["evidence-pi"].files = Array.from({ length: 5 }, (_, i) => ({ ...r["evidence-pi"].files[0], name: `raw-${i}.json` })); },
    r => { r.evidence.files = Array.from({ length: 9 }, (_, i) => ({ ...r.evidence.files[0], name: `raw-${i}.json` })); },
    r => { r["evidence-browser"].files = Array.from({ length: 5 }, (_, i) => ({ ...r["evidence-browser"].files[0], name: `raw-${i}.json` })); },
  ]) { const roots = inventory(); mutation(roots); expect(() => validateEvidenceInventory(roots, 502)).toThrow(); }
});

test("matrix refuses while slice-2 source gaps remain even with complete native rows", () => {
  expect(() => requireSourceCandidateComplete()).toThrow("incomplete P1 source candidate");
  expect(sourceGaps.length).toBeGreaterThan(0);
  expect(Object.keys(matrix)).toEqual(["H1", "H2", "H3", "H4", "W1", "W2", "W3", "W4", "G1", "G2", "G3", "G4", "L1", "L2", "A1", "Q1", "P1", "S1", "S2", "D1"]);
  for (const id of ["H1", "H2", "H3", "H4", "W1", "W3", "G1", "G2", "G3", "G4", "L1", "P1", "S2"]) expect(matrix[id].kinds).toEqual(["real"]);
  expect(matrix.W2.kinds).toEqual(["derived-wire-negatives"]);
  expect(matrix.W4.kinds).toEqual(["controlled-socket-faults"]);
});

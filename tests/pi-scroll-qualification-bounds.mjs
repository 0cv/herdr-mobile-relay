import { createHash } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { appendFileSync, lstatSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, rmdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { maximumBounds, validateBounds } from "./pi-scroll-qualification-limits.mjs";
export { maximumBounds, validateBounds };
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const load = path => JSON.parse(readFileSync(path, "utf8"));
function save(path, value) {
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(value)}\n`, { flag: "wx", mode: 0o600 });
  renameSync(temporary, path);
}
function privateDirectory(path) {
  const stat = lstatSync(path);
  if (realpathSync(path) !== path || stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o700) throw new Error("unsafe bounds directory");
}
export function reserveBounds(directory, bounds = maximumBounds, now = Date.now()) {
  privateDirectory(directory);
  validateBounds(bounds);
  const state = { bounds, phase: "work", workDeadline: now + bounds.workMs, cleanupDeadline: null, cleanupComplete: false };
  writeFileSync(join(directory, "state.json"), `${JSON.stringify(state)}\n`, { flag: "wx", mode: 0o600 });
  for (const phase of ["work", "cleanup"]) writeFileSync(join(directory, `${phase}-output.json`), "0\n", { flag: "wx", mode: 0o600 });
  return state;
}
export function beginCleanup(directory, now = Date.now()) {
  privateDirectory(directory);
  const state = load(join(directory, "state.json"));
  if (state.phase === "work") {
    state.phase = "cleanup";
    state.cleanupDeadline = now + state.bounds.cleanupMs;
    save(join(directory, "state.json"), state);
  }
  return state;
}
export function uncertainCommandEffects(directory) {
  privateDirectory(directory);
  try { lstatSync(join(directory, "command.lock")); return true; } catch (error) { if (error.code !== "ENOENT") throw error; }
  const commands = join(directory, "commands");
  try { return readdirSync(commands).filter(name => name.endsWith(".json")).some(name => load(join(commands, name)).possiblyLiveEffects === true); }
  catch (error) { if (error.code === "ENOENT") return false; throw error; }
}
export function completeBounds(directory, requestedComplete, now = Date.now()) {
  privateDirectory(directory);
  const state = load(join(directory, "state.json"));
  if (state.phase !== "cleanup" || now >= state.cleanupDeadline) throw new Error("cleanup reserve expired");
  state.cleanupComplete = requestedComplete === true && !uncertainCommandEffects(directory);
  save(join(directory, "state.json"), state);
  return state.cleanupComplete;
}
function commandWindow(directory) {
  privateDirectory(directory);
  const state = load(join(directory, "state.json"));
  const deadline = state.phase === "work" ? state.workDeadline : state.cleanupDeadline;
  return { state, deadline, outputPath: join(directory, `${state.phase}-output.json`), maxBytes: state.bounds[`${state.phase}Bytes`] };
}
export async function waitBefore(promise, deadline, label) {
  if (!Number.isFinite(deadline) || Date.now() >= deadline) {
    Promise.resolve(promise).catch(() => {});
    throw new Error(`${label}: deadline expired`);
  }
  let timer;
  try {
    const result = await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${label}: deadline expired`)), deadline - Date.now()); })]);
    if (Date.now() >= deadline) throw new Error(`${label}: deadline expired`);
    return result;
  } finally { clearTimeout(timer); }
}
const evidenceDirectory = process.env.HERDR_A7_EVIDENCE_DIR;
export function lifecycleEvidence(update = () => {}) {
  if (!evidenceDirectory) return null;
  privateDirectory(evidenceDirectory);
  const path = join(evidenceDirectory, "helpers.json"), lock = join(evidenceDirectory, "helpers.lock"), deadline = Date.now() + 250;
  while (true) {
    try { mkdirSync(lock, { mode: 0o700 }); break; }
    catch (error) {
      if (error.code !== "EEXIST") throw error;
      if (Date.now() >= deadline) throw new Error("helper evidence lock deadline expired; retained evidence is uncertain");
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Math.min(5, deadline - Date.now()));
    }
  }
  try {
    let state = load(path);
    if (state.status === "not_invoked") state = { status: "running", phase: "bootstrap", deadlines: [], firstFailure: null, secondaryErrors: [], helpers: [], rawBytes: { stdout: 0, stderr: 0 }, rawOffsets: [], cleanupComplete: false, possiblyLiveEffects: true, limits: { rawBytes: 65536, metadataBytes: 262144, scope: "Only this invocation; synchronous leaf ps probes and driver path-resolution utilities are joined, not historical closure or a network sandbox" } };
    update(state);
    const bytes = Buffer.from(`${JSON.stringify(state)}\n`);
    if (bytes.length > 262144) throw new Error("helper metadata cap exceeded");
    save(path, state);
    return state;
  } finally { rmdirSync(lock); }
}
export function lifecycleFailure(error, secondary = false) {
  lifecycleEvidence(state => {
    const failure = { message: error.message, stack: error.stack ?? null, at: new Date().toISOString(), phase: state.phase };
    if (state.firstFailure) state.secondaryErrors.push({ ...failure, cleanup: secondary });
    else state.firstFailure = failure;
    state.status = "failed";
    state.cleanupComplete = false;
    if (error.helperEffectsUncertain || /deadline|unavailable|not proven|not_proven|cap exceeded|potentially live/.test(error.message)) state.uncertainEffects = true;
  });
}
function helperRecord(role, pid, parentPid, descendants) {
  let id;
  lifecycleEvidence(state => {
    id = `${role}-${state.helpers.length + 1}`;
    state.helpers.push({ id, role, createdByInvocation: true, pid: pid ?? null, expectedParentPid: parentPid, identity: null, absenceProved: false, descendants, observations: [], exit: null, streamsClosed: false });
  });
  return id;
}
function updateHelper(id, update) {
  if (id) lifecycleEvidence(state => update(state.helpers.find(helper => helper.id === id)));
}
export function observeHelper(id, observed, purpose, closureEvidence = null) {
  updateHelper(id, helper => {
    helper.observations.push({ at: new Date().toISOString(), purpose, observed, probe: observed?.probe ?? null });
    if (closureEvidence) helper.closureEvidence = closureEvidence;
    if (helper.observations.length > 12) helper.observations.splice(1, 1);
    if (!helper.identity && observed && !observed.absent && observed.parentPid === helper.expectedParentPid && observed.uid === process.getuid()) helper.identity = observed;
    if (observed?.absent && helper.identity) helper.absenceProved = true;
  });
}
function helperBytes(id, stream, bytes) {
  if (!evidenceDirectory || !bytes.length) return;
  lifecycleEvidence(state => {
    const available = 65536 - state.rawBytes.stdout - state.rawBytes.stderr;
    const kept = bytes.subarray(0, Math.max(0, available));
    appendFileSync(join(evidenceDirectory, `helpers.${stream}.txt`), kept);
    state.rawOffsets.push({ helper: id, stream, offset: state.rawBytes[stream], length: kept.length });
    state.rawBytes[stream] += kept.length;
    if (kept.length !== bytes.length) {
      state.firstFailure ??= { message: "helper raw output cap exceeded", at: new Date().toISOString(), phase: state.phase };
      state.status = "failed";
      state.uncertainEffects = true;
      state.possiblyLiveEffects = true;
    }
  });
}
export function collectOwnedHelper(child, role, { parentPid = process.pid, descendants = "unknown" } = {}) {
  const stdout = [], stderr = [];
  let id, retained = 0, result = null, exit = null;
  const closed = new Promise(resolve => {
    child.once("error", error => { exit = { spawnError: error.message }; updateHelper(id, helper => { helper.exit = exit; }); });
    child.once("exit", (code, signal) => { exit = { code, signal }; updateHelper(id, helper => { helper.exit = exit; }); });
    child.once("close", (code, signal) => { result = { code, signal }; updateHelper(id, helper => { helper.streamsClosed = true; }); resolve(result); });
  });
  for (const stream of ["stdout", "stderr"]) child[stream]?.on("data", bytes => {
    const kept = bytes.subarray(0, Math.max(0, 65536 - retained));
    retained += kept.length;
    (stream === "stdout" ? stdout : stderr).push(kept);
    helperBytes(id, stream, bytes);
  });
  try { id = helperRecord(role, child.pid, parentPid, descendants); }
  catch (error) {
    child.stdio[3]?.destroy();
    child.stdin?.destroy();
    child.stdout?.destroy();
    child.stderr?.destroy();
    child.unref();
    error.helperEffectsUncertain = true;
    error.message = `new ${role} PID ${child.pid ?? "unknown"} evidence unavailable: ${error.message}`;
    throw error;
  }
  return { id, child, stdout, stderr, closed, get result() { return result; }, get exit() { return exit; }, identity: null, descendants };
}
export async function captureHelper(collected, deadline, commandIdentity = null) {
  while (Date.now() < deadline) {
    const observed = processIdentity(collected.child.pid, deadline);
    observeHelper(collected.id, observed, "capture");
    if (observed && !observed.absent && observed.parentPid === process.pid && observed.uid === process.getuid() && (!commandIdentity || observed.commandIdentity === commandIdentity)) {
      collected.identity = observed;
      return observed;
    }
    if (collected.exit || observed?.absent) break;
    await waitBefore(sleep(5), deadline, "identity capture");
  }
  throw new Error("created helper actual identity unavailable");
}
export function captureDescendant(pid, parentPid, role, deadline, commandIdentity) {
  const id = helperRecord(role, pid, parentPid, "source-known builtin-only leaf"), observed = processIdentity(pid, deadline);
  observeHelper(id, observed, "readiness PID capture");
  if (!observed || observed.absent || observed.parentPid !== parentPid || observed.uid !== process.getuid() || observed.commandIdentity !== commandIdentity) throw new Error("readiness descendant ownership unavailable");
  return { id, identity: observed };
}
export async function closeOwnedHelper(collected, deadline, { closeControl = false, descendantsComplete = false } = {}) {
  try {
    if (closeControl && collected.child.stdio[3]) collected.child.stdio[3].end();
    try { await waitBefore(collected.closed, Math.min(deadline, Date.now() + 250), "helper exit/stream closure"); }
    catch {
      if (collected.descendants !== "source-known leaf" && !(descendantsComplete && collected.safeToTerminate)) throw new Error("descendant closure unavailable; owned driver termination refused");
      const stopped = await stopOwnedChild(collected.child, collected.identity, deadline);
      updateHelper(collected.id, helper => { helper.termination = stopped; });
      if (!stopped.directChildAbsent) throw new Error(`helper cleanup refused: ${stopped.refusal}`);
      await waitBefore(collected.closed, deadline, "terminated helper stream closure");
    }
    const observed = processIdentity(collected.child.pid, deadline);
    observeHelper(collected.id, observed, "post-join absence", { exit: collected.exit, streamClose: collected.result, descendantsComplete: descendantsComplete || collected.descendants === "source-known leaf" });
    if (!collected.identity || !observed?.absent || !(descendantsComplete || collected.descendants === "source-known leaf")) throw new Error("helper or descendant closure not proven");
    collected.child.stdio[3]?.destroy();
    return collected.result;
  } catch (error) {
    collected.child.stdio[3]?.destroy();
    collected.child.stdout?.destroy();
    collected.child.stderr?.destroy();
    collected.child.unref();
    updateHelper(collected.id, helper => { helper.closureRefusal = error.message; });
    throw error;
  }
}
export function processIdentity(pid, deadline = Date.now() + 500) {
  if (!Number.isSafeInteger(pid) || pid < 1 || Date.now() >= deadline) return null;
  const timeoutMs = Math.max(1, Math.min(500, deadline - Date.now()));
  const probe = spawnSync("/bin/ps", ["-p", String(pid), "-o", "pid=,ppid=,uid=,lstart=,command="], { encoding: "utf8", timeout: timeoutMs, maxBuffer: 8192, env: { PATH: "/usr/bin:/bin", LC_ALL: "C" } });
  const withProbe = value => Object.defineProperty(value, "probe", { value: { executable: "/bin/ps", argv: ["-p", String(pid), "-o", "pid=,ppid=,uid=,lstart=,command="], status: probe.status, signal: probe.signal, stdout: probe.stdout, stderr: probe.stderr, timeoutMs } });
  if (probe.status === 1 && !probe.error && !probe.stdout.trim() && !probe.stderr.trim()) return withProbe({ absent: true, pid });
  const match = probe.stdout?.trim().match(/^(\d+)\s+(\d+)\s+(\d+)\s+(\S+\s+\S+\s+\S+\s+\S+\s+\S+)\s+(.+)$/);
  if (probe.error || probe.status !== 0 || probe.stderr.trim() || !match) return null;
  return withProbe({ pid: Number(match[1]), parentPid: Number(match[2]), uid: Number(match[3]), startIdentity: match[4].replace(/\s+/g, " "), commandIdentity: match[5] });
}
export function ownedIdentity(expected, current, parentPid = process.pid, uid = process.getuid()) {
  return Boolean(expected && current && !expected.absent && !current.absent && expected.pid > 0 && expected.parentPid === parentPid && expected.uid === uid && expected.startIdentity && expected.commandIdentity && ["pid", "parentPid", "uid", "startIdentity", "commandIdentity"].every(key => expected[key] === current[key]));
}
export async function stopOwnedChild(child, identity, deadline, inspect = processIdentity, parentPid = process.pid) {
  const result = { signalSent: false, directChildAbsent: false, refusal: null };
  for (const signal of ["SIGTERM", "SIGKILL"]) {
    if (Date.now() >= deadline) return { ...result, refusal: "cleanup_deadline" };
    const invocationOwned = !evidenceDirectory || lifecycleEvidence().helpers.some(helper => helper.createdByInvocation && helper.identity && ownedIdentity(helper.identity, identity, parentPid));
    const current = inspect(child.pid, deadline);
    if (current?.absent) return { ...result, directChildAbsent: true };
    if (!ownedIdentity(identity, current, parentPid) || !invocationOwned || Date.now() >= deadline) return { ...result, refusal: "ownership_not_proven" };
    try { child.kill(signal); result.signalSent = true; } catch { return { ...result, refusal: "signal_failed" }; }
    lifecycleEvidence(state => {
      const helper = state.helpers.find(item => item.identity?.pid === child.pid && item.identity.startIdentity === identity.startIdentity);
      if (helper) {
        helper.signalObservations ??= [];
        helper.signalObservations.push({ expected: identity, observed: current, probe: current?.probe ?? null, signal, sent: true, at: new Date().toISOString() });
      }
    });
    for (let i = 0; i < 10 && Date.now() < deadline; i++) {
      await sleep(Math.min(10, Math.max(0, deadline - Date.now())));
      if (Date.now() < deadline && inspect(child.pid, deadline)?.absent) return { ...result, directChildAbsent: true };
    }
  }
  return { ...result, refusal: "absence_not_proven" };
}
export async function runBoundedCommand(command, args, { deadline, cleanupDeadline = deadline + 500, maxBytes, env = process.env, cwd, onOutput, meter, input, leafOnly = false } = {}) {
  if (!Number.isFinite(deadline) || !Number.isSafeInteger(maxBytes) || maxBytes < 0) throw new Error("invalid command bounds");
  if (Date.now() >= deadline || maxBytes === 0) return { reason: maxBytes === 0 ? "output_cap" : "timeout", started: false, stdout: Buffer.alloc(0), stderr: Buffer.alloc(0), cleanup: { refusal: "not_started" }, possiblyLiveEffects: false };
  if (!Number.isFinite(cleanupDeadline) || cleanupDeadline <= deadline) throw new Error("invalid cleanup deadline");
  if (Date.now() >= deadline) return { reason: "timeout", started: false, stdout: Buffer.alloc(0), stderr: Buffer.alloc(0), cleanup: { refusal: "not_started" }, possiblyLiveEffects: false };
  const child = spawn(command, args, { env, cwd, detached: false, stdio: [input === undefined ? "inherit" : "pipe", "pipe", "pipe"] });
  const collected = collectOwnedHelper(child, "bounded-command", { descendants: leafOnly ? "source-known leaf" : "unknown" });
  child.stdin?.on("error", () => {});
  let exited = false, exitCode = null, signal = null, spawnError = null;
  const closed = new Promise(resolve => {
    child.once("error", error => { spawnError = error.message; exited = true; resolve(); });
    child.once("exit", (code, value) => { exitCode = code; signal = value; exited = true; });
    child.once("close", resolve);
  });
  let retained = 0, reason = null;
  const output = { stdout: [], stderr: [] };
  const collect = stream => bytes => {
    if (reason) return;
    const available = Math.min(maxBytes - retained, meter ? meter.remaining() : maxBytes);
    const kept = bytes.subarray(0, Math.max(0, available));
    retained += kept.length;
    output[stream].push(kept);
    if (meter) meter.consume(kept.length);
    if (onOutput) onOutput(stream, kept);
    if (kept.length !== bytes.length) reason = "output_cap";
  };
  child.stdout.on("data", collect("stdout"));
  child.stderr.on("data", collect("stderr"));
  let identity = null;
  try { identity = await captureHelper(collected, Math.min(deadline, Date.now() + 500)); }
  catch (error) { reason ??= "identity_unavailable"; if (evidenceDirectory) lifecycleFailure(error); }
  if (identity && Date.now() < deadline && input !== undefined) child.stdin.end(input);
  else child.stdin?.end();
  while (!exited && !reason && Date.now() < deadline) await sleep(Math.min(10, Math.max(0, deadline - Date.now())));
  if (!exited && !reason) reason = "timeout";
  let cleanup = { signalSent: false, directChildAbsent: false, refusal: null };
  if (!exited) cleanup = await stopOwnedChild(child, identity, cleanupDeadline);
  try { await waitBefore(closed, cleanupDeadline, "bounded command stream closure"); }
  catch (error) { cleanup.refusal ??= "stream_closure_not_proven"; if (evidenceDirectory) lifecycleFailure(error); }
  const absent = processIdentity(child.pid, cleanupDeadline);
  observeHelper(collected.id, absent, "bounded command post-join absence", { exit: collected.exit, streamClose: collected.result, sourceKnownLeaf: leafOnly });
  cleanup.directChildAbsent = Boolean(identity && absent?.absent && collected.result);
  if (evidenceDirectory && (!cleanup.directChildAbsent || !leafOnly)) lifecycleFailure(new Error("bounded command helper or descendants not proven closed"));
  child.stdin?.destroy();
  child.stdout.destroy();
  child.stderr.destroy();
  if (!exited) child.unref();
  return { reason: reason ?? (spawnError ? "spawn_error" : exitCode === 0 ? null : "exit_failure"), started: true, pid: child.pid, identity, exitCode, signal, spawnError, stdout: Buffer.concat(output.stdout), stderr: Buffer.concat(output.stderr), retainedBytes: retained, cleanup, possiblyLiveEffects: !cleanup.directChildAbsent || Boolean(reason) };
}
async function boundedCLI(directory, command, args) {
  const window = commandWindow(directory);
  const lock = join(directory, "command.lock");
  mkdirSync(lock, { mode: 0o700 });
  const startedAt = new Date().toISOString();
  const meter = {
    remaining: () => window.maxBytes - load(window.outputPath),
    consume: bytes => save(window.outputPath, load(window.outputPath) + bytes),
  };
  const result = await runBoundedCommand(command, args, { deadline: window.deadline, maxBytes: meter.remaining(), meter, onOutput: (stream, bytes) => process[stream].write(bytes) });
  const receipt = { ...result, stdout: undefined, stderr: undefined, command, args, startedAt, finishedAt: new Date().toISOString(), phase: window.state.phase };
  rmdirSync(lock);
  const directoryPath = join(directory, "commands");
  try { mkdirSync(directoryPath, { mode: 0o700 }); } catch (error) { if (error.code !== "EEXIST") throw error; }
  writeFileSync(join(directoryPath, `${process.pid}.json`), `${JSON.stringify(receipt)}\n`, { flag: "wx", mode: 0o600 });
  if (result.reason) process.exitCode = 1;
}
const driverPath = fileURLToPath(new URL("./pi-scroll-qualification.sh", import.meta.url));
const driverCwd = dirname(dirname(driverPath));
const sha256 = bytes => createHash("sha256").update(bytes).digest("hex");
function contextBytes(directory) {
  privateDirectory(directory);
  const path = join(directory, "supervisor-context.json"), stat = lstatSync(path);
  if (realpathSync(path) !== path || stat.isSymbolicLink() || !stat.isFile() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o400 || stat.size > 65536) throw new Error("unsafe supervisor context");
  return readFileSync(path);
}
async function pinnedConfiguration(path, deadline) {
  if (!path) return { config: null, hash: null };
  const { readSuccessorConfiguration } = await waitBefore(import("./pi-scroll-qualification-ledger.mjs"), deadline, "configuration import");
  const before = readFileSync(path);
  const config = await waitBefore(readSuccessorConfiguration(path), deadline, "configuration validation");
  const after = readFileSync(path);
  if (!before.equals(after)) throw new Error("supervisor configuration changed during validation");
  return { config, hash: sha256(before) };
}
export async function readSupervisorContext(directory, driverPid, contextHash, deadline = Date.now() + 2000) {
  const bytes = contextBytes(directory);
  if (!/^[a-f0-9]{64}$/.test(contextHash) || sha256(bytes) !== contextHash) throw new Error("unbound supervisor context");
  const context = JSON.parse(bytes);
  const keys = ["schema", "directory", "driverPath", "cwd", "bunPath", "mode", "configPath", "configHash", "bounds", "supervisor", "driver"];
  if (Object.keys(context).sort().join() !== keys.sort().join() || context.schema !== 1 || context.directory !== directory || context.driverPath !== driverPath || context.cwd !== driverCwd || context.bunPath !== realpathSync(process.execPath) || !["qualification", "identity-probe"].includes(context.mode)) throw new Error("invalid supervisor context");
  if (context.mode === "identity-probe" && process.env.NODE_ENV !== "test") throw new Error("identity probe is offline only");
  const supervisor = processIdentity(context.supervisor?.pid, deadline), driver = processIdentity(driverPid, deadline);
  if (!ownedIdentity(context.supervisor, supervisor, context.supervisor?.parentPid) || context.driver?.pid !== driverPid || !ownedIdentity(context.driver, driver, context.supervisor.pid) || driver.commandIdentity !== `bash ${driverPath}`) throw new Error("supervisor ancestry or driver identity changed");
  if (process.pid !== context.supervisor.pid && process.ppid !== driverPid) throw new Error("context reader is not the supervised driver child");
  const state = load(join(directory, "state.json"));
  validateBounds(context.bounds);
  if (JSON.stringify(state.bounds) !== JSON.stringify(context.bounds) || state.phase !== "work" || !Number.isFinite(state.workDeadline) || Date.now() >= state.workDeadline) throw new Error("supervisor work reservation changed or expired");
  if (typeof context.configPath !== "string") throw new Error("invalid supervisor configuration path");
  const pinned = await pinnedConfiguration(context.configPath, Math.min(deadline, state.workDeadline));
  if (pinned.hash !== context.configHash || JSON.stringify(pinned.config?.bounds ?? maximumBounds) !== JSON.stringify(context.bounds)) throw new Error("supervisor configuration changed");
  return { ...context, configuration: pinned.config };
}
export function supervisedDriverInvocation(directory) {
  return { command: "/bin/bash", args: [driverPath], options: { argv0: "bash", cwd: driverCwd, env: { ...process.env, PATH: `${dirname(process.execPath)}:${process.env.PATH}`, HERDR_P1_BOUNDS_DIR: directory }, stdio: ["inherit", "pipe", "pipe", "pipe"], detached: false } };
}
export async function launchSupervisedDriver(directory, configPath = "", { identityProbe = false, deadline = Date.now() + 2000, cleanupDeadline = deadline + 1000, onCreated = () => {} } = {}) {
  privateDirectory(directory);
  if (typeof configPath !== "string" || typeof identityProbe !== "boolean" || (identityProbe && process.env.NODE_ENV !== "test") || !Number.isFinite(deadline) || !Number.isFinite(cleanupDeadline) || cleanupDeadline <= deadline) throw new Error("invalid supervised launch");
  const pinned = await pinnedConfiguration(configPath, deadline);
  if (Date.now() >= deadline) throw new Error("bootstrap deadline expired before reservation");
  const bounds = pinned.config?.bounds ?? maximumBounds;
  reserveBounds(directory, bounds);
  const supervisor = processIdentity(process.pid, deadline);
  if (!supervisor || supervisor.absent || supervisor.uid !== process.getuid()) throw new Error("supervisor identity unavailable");
  if (Date.now() >= deadline) throw new Error("bootstrap deadline expired before spawn");
  const invocation = supervisedDriverInvocation(directory);
  const child = spawn(invocation.command, invocation.args, invocation.options);
  const collected = collectOwnedHelper(child, "supervised-driver", { descendants: identityProbe ? "readiness-gated background and recorded context validator" : "unknown qualification descendants" });
  child.stdio[3].on("error", () => {});
  let contextSent = false;
  try {
    onCreated(collected);
    const identity = await captureHelper(collected, deadline, `bash ${driverPath}`);
    const context = { schema: 1, directory, driverPath, cwd: driverCwd, bunPath: realpathSync(process.execPath), mode: identityProbe ? "identity-probe" : "qualification", configPath, configHash: pinned.hash, bounds, supervisor, driver: identity };
    const bytes = Buffer.from(`${JSON.stringify(context)}\n`), contextHash = sha256(bytes);
    if (Date.now() >= deadline) throw new Error("bootstrap deadline expired before context write");
    writeFileSync(join(directory, "supervisor-context.json"), bytes, { flag: "wx", mode: 0o400 });
    contextSent = true;
    await waitBefore(new Promise((resolve, reject) => child.stdio[3].write(`${contextHash}\n`, error => error ? reject(error) : resolve())), deadline, "context FD write");
    return { child, identity, collected, context, contextHash, bounds };
  } catch (error) {
    lifecycleFailure(error);
    try { await closeOwnedHelper(collected, cleanupDeadline, { closeControl: true, descendantsComplete: !contextSent }); }
    catch (cleanupError) { lifecycleFailure(cleanupError, true); }
    throw error;
  }
}
async function supervise(directory, configPath) {
  const { child, identity, bounds } = await launchSupervisedDriver(directory, configPath);
  let exited = false, code = null, error = null, reason = null, retained = 0, cleanup = null;
  child.once("error", value => { error = value.message; exited = true; });
  child.once("exit", value => { code = value; exited = true; });
  const capture = stream => bytes => {
    const state = load(join(directory, "state.json"));
    const cap = bounds.workBytes + (state.phase === "cleanup" ? bounds.cleanupBytes : 0);
    const kept = bytes.subarray(0, Math.max(0, cap - retained));
    retained += kept.length;
    process[stream].write(kept);
    if (kept.length !== bytes.length) reason ??= "output_cap";
  };
  child.stdout.on("data", capture("stdout"));
  child.stderr.on("data", capture("stderr"));
  let signaledWork = false;
  while (!exited) {
    let state = load(join(directory, "state.json"));
    if (state.phase === "work" && (reason || Date.now() >= state.workDeadline)) {
      reason ??= "work_timeout";
      state = beginCleanup(directory);
      if (!ownedIdentity(identity, processIdentity(child.pid))) { cleanup = { refusal: "driver_ownership_not_proven" }; break; }
      child.kill("SIGTERM");
      signaledWork = true;
    }
    if (state.phase === "cleanup" && Date.now() >= state.cleanupDeadline) {
      cleanup = await stopOwnedChild(child, identity, Date.now() + 500);
      reason ??= "cleanup_timeout";
      break;
    }
    await sleep(10);
  }
  child.stdout.destroy();
  child.stderr.destroy();
  if (!exited) child.unref();
  const state = load(join(directory, "state.json"));
  let commandFailures = [];
  try { commandFailures = readdirSync(join(directory, "commands")).filter(name => name.endsWith(".json")).map(name => ({ path: join(directory, "commands", name), ...load(join(directory, "commands", name)) })).filter(item => item.reason).sort((a, b) => a.startedAt.localeCompare(b.startedAt)); } catch (value) { if (value.code !== "ENOENT") throw value; }
  const receipt = { reason, code, error, retainedBytes: retained, driverIdentity: identity, signaledWork, cleanup, firstWorkFailure: commandFailures.find(item => item.phase === "work") ?? null, cleanupFailures: commandFailures.filter(item => item.phase === "cleanup"), driverExited: exited, cleanupComplete: exited && !error && state.cleanupComplete === true && !uncertainCommandEffects(directory), possiblyLiveEffects: !exited || state.cleanupComplete !== true || uncertainCommandEffects(directory), cleanupGuaranteed: false, limits: bounds, serverLogBytesLimit: 65536, noNetworkSandbox: true };
  save(join(directory, "supervisor.json"), receipt);
  if (reason || code !== 0 || !receipt.cleanupComplete) process.exitCode = 1;
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  let contextHelperId;
  try {
    const [action, directory, ...args] = process.argv.slice(2);
    if (action === "supervise") {
      if (args.length !== 1) throw new Error("unexpected supervised arguments");
      await supervise(directory, args[0]);
    } else if (action === "context") {
      if (args.length !== 2) throw new Error("unexpected context arguments");
      if (process.cwd() !== driverCwd) throw new Error("supervised driver working directory changed");
      if (evidenceDirectory) {
        contextHelperId = helperRecord("context-validator", process.pid, process.ppid, "source-known leaf: read-only validation and synchronous joined ps probes");
        const observed = processIdentity(process.pid);
        observeHelper(contextHelperId, observed, "context validator self-capture before validation");
        if (!observed || observed.absent || observed.parentPid !== process.ppid || observed.uid !== process.getuid()) throw new Error("context validator identity unavailable");
      }
      const context = await readSupervisorContext(directory, Number(args[0]), args[1]);
      const output = `${context.mode}\t${JSON.stringify(context.configuration)}\n`;
      helperBytes(contextHelperId, "stdout", Buffer.from(output));
      process.stdout.write(output);
    } else if (action === "command") await boundedCLI(directory, args[0], args.slice(1));
    else if (action === "begin-cleanup") beginCleanup(directory);
    else if (action === "check-effects") { if (uncertainCommandEffects(directory)) process.exitCode = 1; }
    else if (action === "complete") { if (!completeBounds(directory, args[0] === "1")) process.exitCode = 1; }
    else throw new Error("unsupported bounds action");
  } catch (error) {
    const output = `${JSON.stringify({ boundsFailure: error.message })}\n`;
    if (contextHelperId) helperBytes(contextHelperId, "stderr", Buffer.from(output));
    process.stderr.write(output);
    process.exitCode = 1;
  }
}

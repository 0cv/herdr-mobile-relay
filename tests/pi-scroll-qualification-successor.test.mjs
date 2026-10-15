import { afterAll, expect, test as bunTest } from "bun:test";
import { AsyncLocalStorage } from "node:async_hooks";
import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import * as fileOperations from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as ledgerOperations from "./pi-scroll-qualification-ledger.mjs";
import { beginCleanup, captureDescendant, captureHelper, closeOwnedHelper, collectOwnedHelper, completeBounds, launchSupervisedDriver, lifecycleEvidence, lifecycleFailure, observeHelper, ownedIdentity, processIdentity, readSupervisorContext, reserveBounds, runBoundedCommand, stopOwnedChild, supervisedDriverInvocation, uncertainCommandEffects, validateBounds, waitBefore } from "./pi-scroll-qualification-bounds.mjs";
import { validateHerdrSocketProof } from "./pi-scroll-qualification-herdr-proof.mjs";

const testWindows = new AsyncLocalStorage();
function boundedOperation(name, operation, cleanup = false) {
  return (...args) => {
    const window = testWindows.getStore(), deadline = window?.[cleanup ? "cleanupDeadline" : "workDeadline"] ?? Date.now() + 2000;
    const pending = Promise.resolve().then(() => {
      if (window?.expired || Date.now() >= deadline) throw new Error(`${name}: deadline expired; pending synthetic effects retained`);
      return operation(...args);
    });
    return waitBefore(pending, deadline, name).catch(error => {
      if (/deadline expired/.test(error.message)) {
        if (window) window.expired = true;
        lifecycleFailure(error);
      }
      throw error;
    });
  };
}
const [chmod, lstat, mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile] = ["chmod", "lstat", "mkdir", "mkdtemp", "readFile", "realpath", "rm", "symlink", "writeFile"].map(name => boundedOperation(`filesystem ${name}`, fileOperations[name], name === "rm"));
const [acquireStageLock, allocateAttempt, initializeStage, markRootCreated, readSuccessorConfiguration, recordAttemptOutcome, releaseStageLock, stageSummary, validateFixtureContract] = ["acquireStageLock", "allocateAttempt", "initializeStage", "markRootCreated", "readSuccessorConfiguration", "recordAttemptOutcome", "releaseStageLock", "stageSummary", "validateFixtureContract"].map(name => boundedOperation(`synthetic ledger ${name}`, ledgerOperations[name]));
function test(name, run, timeoutMs = 5000) {
  bunTest(name, async () => {
    const started = Date.now(), cleanupDeadline = started + Math.min(innerTotalBudgetMs, timeoutMs - 500);
    const window = { workDeadline: timeoutMs === 15000 ? cleanupDeadline - cleanupBudgetMs : started + 2000, cleanupDeadline, expired: false };
    await testWindows.run(window, async () => {
      try { await waitBefore(run(), cleanupDeadline, `${name}: complete lifecycle`); }
      catch (error) { window.expired = true; lifecycleFailure(error); throw error; }
    });
  }, timeoutMs);
}
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const json = (path, value, mode = 0o600) => writeFile(path, `${JSON.stringify(value)}\n`, { mode });
const innerTotalBudgetMs = 11500, cleanupBudgetMs = 3000;
function lifecycle(label, workMs = innerTotalBudgetMs - cleanupBudgetMs) {
  const started = Date.now(), window = testWindows.getStore();
  const workDeadline = Math.min(started + workMs, window?.workDeadline ?? Infinity), cleanupDeadline = Math.min(workDeadline + cleanupBudgetMs, window?.cleanupDeadline ?? Infinity);
  lifecycleEvidence(state => {
    if (state.firstFailure) throw new Error("earlier lifecycle failure; no additional helpers admitted");
    state.phase = `${label}: work`;
    state.deadlines.push({ label, started, workDeadline, cleanupDeadline });
  });
  return { workDeadline, cleanupDeadline, work: (promise, phase = label) => waitBefore(promise, workDeadline, phase) };
}
function cleanupPhase(label) { lifecycleEvidence(state => { state.phase = `${label}: cleanup`; }); }
async function readyLine(collected, deadline, pattern) {
  while (Date.now() < deadline) {
    const output = Buffer.concat(collected.stdout).toString();
    const match = output.match(pattern);
    if (match) return match;
    if (collected.result) throw new Error(`helper exited before verified readiness: ${JSON.stringify(collected.result)}`);
    await waitBefore(new Promise(resolve => setTimeout(resolve, 5)), deadline, "readiness output");
  }
  throw new Error("driver readiness deadline expired");
}
function proveValidatorClosure(driverPid, deadline) {
  const validators = lifecycleEvidence()?.helpers.filter(helper => helper.role === "context-validator" && helper.expectedParentPid === driverPid) ?? [];
  if (process.env.HERDR_A7_EVIDENCE_DIR && validators.length !== 1) throw new Error("context validator ownership unavailable");
  for (const helper of validators) {
    const observed = processIdentity(helper.identity?.pid, deadline);
    observeHelper(helper.id, observed, "driver readiness/exit joined context validator; fresh absence", { parentPid: driverPid, commandSubstitutionJoined: true });
    if (!helper.identity || !observed?.absent) throw new Error("context validator closure unavailable");
  }
}
afterAll(() => {
  const state = lifecycleEvidence();
  if (!state) return;
  const complete = !state.uncertainEffects && !state.fixtureRoots?.some(root => root.retained) && state.helpers.length > 0 && state.helpers.every(helper => helper.identity && helper.absenceProved && helper.closureEvidence && helper.descendants !== "unknown");
  if (!complete && !state.firstFailure) lifecycleFailure(new Error("created helper identity or descendant closure incomplete"));
  lifecycleEvidence(value => { value.phase = "finished"; value.cleanupComplete = complete; value.possiblyLiveEffects = !complete; value.status = value.firstFailure ? "failed" : "passed"; });
  expect(state.firstFailure).toBeNull();
  expect(complete).toBe(true);
});
async function sandbox() {
  const path = await realpath(await mkdtemp(join(tmpdir(), "s-")));
  expect(path.startsWith(`${await realpath(process.env.TMPDIR)}/`)).toBe(true);
  return path;
}
const capturedRoles = ["tmux-server", "tmux-bootstrap", "herdr-launcher", "herdr-server", "pi-agent"];
function syntheticAbsentPids() {
  const pids = [];
  for (let pid = process.pid + 1000; pid < process.pid + 1064 && pids.length < 5; pid++) if (processIdentity(pid)?.absent) pids.push(pid);
  if (pids.length !== 5) throw new Error("synthetic absent identities unavailable");
  return pids;
}
async function predecessor(base, { createdRoles = null, launchLogs = true, externalClosure = null } = {}) {
  const stageRoot = join(base, "p"), fixtureRootParent = join(base, "r");
  await mkdir(fixtureRootParent, { mode: 0o700 });
  await initializeStage(stageRoot, { stageId: "p", runId: "predecessor-run", fixtureRootParent });
  const token = "p-lock";
  await acquireStageLock(stageRoot, { token, runId: "predecessor-run", shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" });
  const allocations = [], pids = createdRoles ? syntheticAbsentPids() : [];
  for (let number = 1; number <= 6; number++) {
    const allocation = await allocateAttempt(stageRoot, { token, requestedId: `attempt-${String(number).padStart(3, "0")}` });
    const created = number === 1 && createdRoles !== null, source_files = [];
    if (created) {
      const root = allocation.root_path;
      await mkdir(root, { mode: 0o700 });
      await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
      await json(join(root, ".stage-owner.json"), { stage_id: "p", run_id: "predecessor-run", attempt_id: allocation.attempt_id, root_path: root, uid: process.getuid() });
      await mkdir(join(root, "logs"), { mode: 0o700 });
      if (launchLogs) for (const name of ["tmux-start.log", "herdr-server.log", "pi-start.json"]) await json(join(root, "logs", name), { synthetic_launch_evidence: true });
      const processesPath = join(root, "owned-processes.tsv");
      await writeFile(processesPath, ["role\tpid\tstart_identity\tcommand_identity", ...createdRoles.map(role => `${role}\t${pids[capturedRoles.indexOf(role)]}\tWed Oct 7 00:28:18 2026\tsynthetic-${role}`)].join("\n") + "\n", { mode: 0o600 });
      source_files.push({ path: processesPath, sha256: hash(await readFile(processesPath)) });
      if (externalClosure) {
        const path = join(root, "logs/external-closure.json");
        await json(path, externalClosure);
        source_files.push({ path, sha256: hash(await readFile(path)) });
      }
      await markRootCreated(stageRoot, { token, attemptId: allocation.attempt_id });
    }
    const outcome = await recordAttemptOutcome(stageRoot, { token, attemptId: allocation.attempt_id, result: "blocked", rootCreated: created, cleanupComplete: true });
    allocations.push({ attempt_id: allocation.attempt_id, root_path: allocation.root_path, original_outcome_seq: outcome.seq, closed: true, source_files });
  }
  await releaseStageLock(stageRoot, { token });
  const ownerPath = join(stageRoot, "stage-owner.json"), ledgerPath = join(stageRoot, "allocation-ledger.jsonl"), cleanupPath = join(base, "closure.json");
  const ownerBytes = await readFile(ownerPath), ledgerBytes = await readFile(ledgerPath);
  const cleanup = { schema_version: 1, kind: "six-allocation-closure-observation", stage_root: stageRoot, stage_id: "p", run_id: "predecessor-run", owner_sha256: hash(ownerBytes), ledger_sha256: hash(ledgerBytes), observed_at_utc: new Date().toISOString(), allocations };
  await json(cleanupPath, cleanup, 0o400);
  const successor = { stageRoot, stageId: "p", runId: "predecessor-run", ownerSha256: hash(ownerBytes), ledgerSha256: hash(ledgerBytes), cleanupPath, cleanupSha256: hash(await readFile(cleanupPath)) };
  return { successor, cleanup, ownerPath, ownerBytes, ledgerPath, ledgerBytes, cleanupPath };
}
async function successorConfig(base, previous) {
  const fixtureRootParent = join(base, "f");
  await mkdir(fixtureRootParent, { mode: 0o700 });
  return { schema_version: 1, stageRoot: join(base, "n"), stageId: "n", runId: "successor-run", fixtureRootParent, outputDirectory: join(base, "o"), successor: previous.successor, bounds: { workMs: 1000, cleanupMs: 500, workBytes: 4096, cleanupBytes: 1024 } };
}

test("a prospective successor consumes one local slot and honestly reports cumulative seven without rewriting six", async () => {
  const base = await sandbox();
  try {
    const previous = await predecessor(base), config = await successorConfig(base, previous), configPath = join(base, "config.json");
    await json(configPath, config, 0o400);
    expect(await readSuccessorConfiguration(configPath)).toEqual(config);
    await initializeStage(config.stageRoot, config);
    const token = "n-lock";
    await acquireStageLock(config.stageRoot, { token, runId: config.runId, shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" });
    const before = await stageSummary(config.stageRoot, {});
    expect(before.max_attempts).toBe(1);
    expect(before.namespace_slots).toBe(6);
    expect(before.local_allocation_count).toBe(0);
    expect(before.cumulative_allocation_count).toBe(6);
    expect(before.predecessor.authorization_proved).toBe(false);
    const allocation = await allocateAttempt(config.stageRoot, { token, requestedId: "attempt-001" });
    expect(allocation.local_allocation_count).toBe(1);
    expect(allocation.cumulative_allocation_count).toBe(7);
    await recordAttemptOutcome(config.stageRoot, { token, attemptId: allocation.attempt_id, result: "blocked", rootCreated: false, cleanupComplete: true });
    await expect(allocateAttempt(config.stageRoot, { token, requestedId: "attempt-002" })).rejects.toMatchObject({ code: "attempt-limit" });
    const summary = await stageSummary(config.stageRoot, {});
    expect(summary.allocation_count).toBe(1);
    expect(summary.local_allocation_count).toBe(1);
    expect(summary.predecessor_allocation_count).toBe(6);
    expect(summary.cumulative_allocation_count).toBe(7);
    expect(await readFile(previous.ownerPath)).toEqual(previous.ownerBytes);
    expect(await readFile(previous.ledgerPath)).toEqual(previous.ledgerBytes);
    const ledgerPath = join(config.stageRoot, "allocation-ledger.jsonl"), bytes = await readFile(ledgerPath);
    const records = bytes.toString().trimEnd().split("\n").map(JSON.parse);
    records[0].cumulative_allocation_count = 1;
    await writeFile(ledgerPath, `${records.map(JSON.stringify).join("\n")}\n`);
    await expect(stageSummary(config.stageRoot, {})).rejects.toMatchObject({ code: "invalid_allocation" });
    await writeFile(ledgerPath, bytes);
    await releaseStageLock(config.stageRoot, { token });
  } catch (error) { lifecycleFailure(error); throw error; }
  finally { await waitBefore(rm(base, { recursive: true, force: true }), Date.now() + 1000, "successor fixture removal"); }
});

test("successor rejects forged counts, hashes, identities, overlapping namespaces and incomplete or changed closure", async () => {
  const base = await sandbox();
  try {
    const previous = await predecessor(base), config = await successorConfig(base, previous);
    const initialize = successor => initializeStage(config.stageRoot, { ...config, successor });
    for (const mutation of [{ allocationCount: 6 }, { cumulativeCount: 6 }]) await expect(initialize({ ...previous.successor, ...mutation })).rejects.toMatchObject({ code: "successor_configuration" });
    await expect(initialize({ ...previous.successor, ledgerSha256: "0".repeat(64) })).rejects.toMatchObject({ code: "predecessor_hash" });
    await expect(initialize({ ...previous.successor, ownerSha256: "0".repeat(64) })).rejects.toMatchObject({ code: "predecessor_identity" });
    await expect(initialize({ ...previous.successor, runId: "forged-run" })).rejects.toMatchObject({ code: "predecessor_identity" });
    await expect(initializeStage(previous.successor.stageRoot, { ...config, stageRoot: previous.successor.stageRoot })).rejects.toMatchObject({ code: "predecessor_overlap" });
    await expect(initializeStage(join(previous.successor.stageRoot, "nested"), { ...config })).rejects.toMatchObject({ code: "predecessor_overlap" });
    const records = previous.ledgerBytes.toString().trimEnd().split("\n").map(JSON.parse);
    records[0].attempt_number = 2;
    const forged = Buffer.from(`${records.map(JSON.stringify).join("\n")}\n`);
    await writeFile(previous.ledgerPath, forged);
    await expect(initialize({ ...previous.successor, ledgerSha256: hash(forged) })).rejects.toMatchObject({ code: "predecessor_count" });
    await writeFile(previous.ledgerPath, previous.ledgerBytes);
    const owner = JSON.parse(previous.ownerBytes);
    owner.successor = previous.successor;
    owner.allocation_limit = 1;
    await json(previous.ownerPath, owner);
    await expect(initialize({ ...previous.successor, ownerSha256: hash(await readFile(previous.ownerPath)) })).rejects.toMatchObject({ code: "predecessor_identity" });
    await writeFile(previous.ownerPath, previous.ownerBytes);
    await acquireStageLock(previous.successor.stageRoot, { token: "reopened", runId: "predecessor-run", shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" });
    await expect(initialize(previous.successor)).rejects.toMatchObject({ code: "predecessor_open" });
    await releaseStageLock(previous.successor.stageRoot, { token: "reopened" });
    await chmod(previous.cleanupPath, 0o600);
    await expect(initialize(previous.successor)).rejects.toMatchObject({ code: "unsafe_evidence" });
    await json(previous.cleanupPath, { ...previous.cleanup, allocations: previous.cleanup.allocations.slice(1) });
    await chmod(previous.cleanupPath, 0o400);
    await expect(initialize({ ...previous.successor, cleanupSha256: hash(await readFile(previous.cleanupPath)) })).rejects.toMatchObject({ code: "predecessor_cleanup" });
    await chmod(previous.cleanupPath, 0o600);
    await json(previous.cleanupPath, { ...previous.cleanup, run_id: "another" });
    await chmod(previous.cleanupPath, 0o400);
    await expect(initialize({ ...previous.successor, cleanupSha256: hash(await readFile(previous.cleanupPath)) })).rejects.toMatchObject({ code: "predecessor_cleanup" });
    await expect(initialize(previous.successor)).rejects.toMatchObject({ code: "predecessor_hash" });
  } catch (error) { lifecycleFailure(error); throw error; }
  finally { await waitBefore(rm(base, { recursive: true, force: true }), Date.now() + 1000, "forged fixture removal"); }
});

test("created predecessor closure refuses missing launched identities even with absent sockets, missing logs or hash-bound external closed claims", async () => {
  const cases = [
    { createdRoles: capturedRoles.slice(0, 2) },
    { createdRoles: capturedRoles.filter(role => role !== "herdr-launcher") },
    { createdRoles: capturedRoles.filter(role => role !== "herdr-server") },
    { createdRoles: capturedRoles.filter(role => role !== "pi-agent") },
    { createdRoles: capturedRoles.filter(role => role !== "tmux-server") },
    { createdRoles: capturedRoles.filter(role => role !== "tmux-bootstrap") },
    { createdRoles: capturedRoles.slice(0, 2), launchLogs: false },
    { createdRoles: capturedRoles.filter(role => role !== "pi-agent"), launchLogs: false },
    { createdRoles: capturedRoles.slice(0, 2), externalClosure: { closed: true, processes_absent: true, sockets_absent: true } },
    { createdRoles: capturedRoles.slice(0, 2), externalClosure: { authenticated: true, ownership_reconstructed: false, herdr_server: { absent: true } } },
    { createdRoles: capturedRoles },
  ];
  for (const specification of cases) {
    const base = await sandbox();
    try {
      const previous = await predecessor(base, specification), config = await successorConfig(base, previous);
      const root = previous.cleanup.allocations[0].root_path;
      for (const path of [join(root, "config/herdr/herdr.sock"), join(root, "config/herdr/herdr-client.sock"), join(root, `tmux/tmux-${process.getuid()}/default`)]) await expect(lstat(path)).rejects.toMatchObject({ code: "ENOENT" });
      if (specification.createdRoles.length === 5) {
        const state = await initializeStage(config.stageRoot, config);
        expect(state.predecessor.allocation_count).toBe(6);
        expect(state.predecessor.authorization_proved).toBe(false);
        expect(state.allocations).toHaveLength(0);
      } else {
        await expect(initializeStage(config.stageRoot, config)).rejects.toMatchObject({ code: "predecessor_cleanup" });
        await expect(lstat(config.stageRoot)).rejects.toMatchObject({ code: "ENOENT" });
      }
      expect(await readFile(previous.ownerPath)).toEqual(previous.ownerBytes);
      expect(await readFile(previous.ledgerPath)).toEqual(previous.ledgerBytes);
    } catch (error) { lifecycleFailure(error); throw error; }
    finally { await waitBefore(rm(base, { recursive: true, force: true }), Date.now() + 1000, "predecessor fixture removal"); }
  }
});

async function listen(path, deadline = Date.now() + 1000) {
  if (Date.now() >= deadline) throw new Error("owned socket bootstrap deadline expired");
  expect(Buffer.byteLength(path)).toBeLessThan(104);
  const socket = net.createServer(), control = new AbortController();
  try {
    await waitBefore(new Promise((resolve, reject) => { socket.once("error", reject); socket.listen({ path, signal: control.signal }, resolve); }), deadline, "owned socket listen");
    await waitBefore(chmod(path, 0o600), deadline, "owned socket permissions");
    return socket;
  } catch (error) {
    lifecycleFailure(error);
    control.abort();
    try { await close(socket, Date.now() + 500); }
    catch (cleanupError) { lifecycleFailure(cleanupError, true); }
    throw error;
  }
}
async function close(socket, deadline = Date.now() + 1000) { if (socket?.listening) await waitBefore(new Promise((resolve, reject) => socket.close(error => error ? reject(error) : resolve())), deadline, "owned socket close"); }

const driverPath = fileURLToPath(new URL("./pi-scroll-qualification.sh", import.meta.url));
async function shellLauncherGuard(guard, launcher, clock) {
  const singleLineGuard = guard.replace(/then\n\s*/, "then ").replace(/\n\s*/g, "; ");
  const result = await clock.work(runBoundedCommand("/bin/bash", ["--noprofile", "--norc", "-c", `builtin read -r; set -euo pipefail; ${singleLineGuard}; builtin printf accepted`], {
    env: { PATH: "/nonexistent", HOME: process.env.HOME, LC_ALL: "C", SCRIPT_DIR: dirname(driverPath), HERDR_PID: String(launcher.pid), HERDR_LAUNCHER_PARENT: String(launcher.parentPid), HERDR_START: launcher.startIdentity, HERDR_COMMAND: launcher.commandIdentity },
    cwd: process.env.HOME, deadline: Math.min(clock.workDeadline, Date.now() + 1000), cleanupDeadline: clock.cleanupDeadline, maxBytes: 4096, input: "\n", leafOnly: true,
  }));
  expect(result.spawnError).toBeNull();
  expect(result.signal).toBeNull();
  expect(result.stderr.toString()).toBe("");
  expect(result.cleanup.directChildAbsent).toBe(true);
  expect([null, "exit_failure"]).toContain(result.reason);
  return result.exitCode;
}
async function syntheticServerProof(base, launcher, clock, { expectedCommandRefusal = false } = {}) {
  const stageRoot = join(base, "q"), fixtureRootParent = join(base, "z"), stageId = "h", runId = "synthetic-proof", lockToken = "synthetic-lock", uid = process.getuid();
  await clock.work(mkdir(fixtureRootParent, { mode: 0o700 }));
  await clock.work(initializeStage(stageRoot, { stageId, runId, fixtureRootParent }));
  await clock.work(acquireStageLock(stageRoot, { token: lockToken, runId, shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" }));
  const allocation = await clock.work(allocateAttempt(stageRoot, { token: lockToken, requestedId: "attempt-001" })), root = allocation.root_path;
  await clock.work(mkdir(join(root, "config/herdr"), { recursive: true, mode: 0o700 }));
  await clock.work(writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 }));
  await clock.work(json(join(root, ".stage-owner.json"), { stage_id: stageId, run_id: runId, attempt_id: allocation.attempt_id, root_path: root, uid }));
  const configPath = join(root, "config/herdr/config.toml");
  await clock.work(writeFile(configPath, "synthetic-config\n", { mode: 0o600 }));
  await clock.work(markRootCreated(stageRoot, { token: lockToken, attemptId: allocation.attempt_id }));
  const herdrBin = await clock.work(realpath(process.execPath)), server = { pid: 910001, parentPid: launcher.pid, uid, startIdentity: "Wed Oct 7 00:28:18 2026", commandIdentity: `${herdrBin} server` };
  await clock.work(writeFile(join(root, "owned-processes.tsv"), ["role\tpid\tstart_identity\tcommand_identity", `herdr-launcher\t${launcher.pid}\t${launcher.startIdentity}\t${launcher.commandIdentity}`, `herdr-server\t${server.pid}\t${server.startIdentity}\t${server.commandIdentity}`].join("\n") + "\n", { mode: 0o600 }));
  const socketPaths = [join(root, "config/herdr/herdr.sock"), join(root, "config/herdr/herdr-client.sock")], sockets = [];
  try {
    for (const path of socketPaths) sockets.push(await clock.work(listen(path, clock.workDeadline)));
    const stats = await clock.work(Promise.all(socketPaths.map(path => lstat(path))));
    const input = { stageRoot, stageId, runId, attemptId: allocation.attempt_id, root, uid, configPath, herdrBin, lsofBin: "/usr/sbin/lsof", lockToken, socketPaths, launcher, server };
    const observed = { launcher: processIdentity(launcher.pid, clock.workDeadline), server, sockets: socketPaths.map((path, index) => ({ path, uid, mode: 0o600, device: stats[index].dev, inode: stats[index].ino, listenerPids: [String(server.pid)] })) };
    return await clock.work(validateHerdrSocketProof(input, observed));
  } catch (error) {
    if (!expectedCommandRefusal || error.code !== "herdr_command_identity") lifecycleFailure(error);
    throw error;
  } finally {
    for (const socket of sockets) {
      try { await close(socket, clock.cleanupDeadline); }
      catch (error) { lifecycleFailure(error, true); throw error; }
    }
  }
}

test("real harmless supervised and background Bash identities satisfy both unchanged guards for baseline and successor, refusing near paths and unbound contexts", async () => {
  const clock = lifecycle("owned Bash integration"), source = await clock.work(readFile(driverPath, "utf8"));
  const guards = [...source.matchAll(/^if (\[\[ [^\n]*"\$HERDR_COMMAND"[^\n]* \]\]); then\n    FAILURE_REASON="Herdr fixture launcher identity was incomplete or unexpected"\n    exit 1\nfi$/gm)];
  expect(guards).toHaveLength(1);
  const evidence = [];
  let firstError = null;
  const fail = error => { firstError ??= error; lifecycleFailure(error); };
  for (const successor of [false, true]) {
    lifecycleEvidence(state => { state.phase = `owned Bash integration: ${successor ? "successor" : "baseline"} work`; });
    const base = await clock.work(sandbox());
    let collected, background, launchSent = false, cleanupComplete = false;
    try {
      let configPath = "";
      if (successor) {
        const previous = await clock.work(predecessor(base)), config = await clock.work(successorConfig(base, previous));
        configPath = join(base, "config.json");
        await clock.work(json(configPath, { ...config, bounds: { ...config.bounds, workMs: 10000 } }, 0o400));
      }
      const directory = join(base, "b");
      await clock.work(mkdir(directory, { mode: 0o700 }));
      const launched = await launchSupervisedDriver(directory, configPath, { identityProbe: true, deadline: clock.workDeadline, cleanupDeadline: clock.cleanupDeadline, onCreated: helper => { collected = helper; } });
      const child = launched.child;
      await readyLine(collected, clock.workDeadline, /^ready\n/);
      proveValidatorClosure(child.pid, clock.workDeadline);
      if (Date.now() >= clock.workDeadline) throw new Error("work expired before background admission");
      launchSent = true;
      await clock.work(new Promise((resolve, reject) => child.stdio[3].write("launch\n", error => error ? reject(error) : resolve())));
      const readiness = await readyLine(collected, clock.workDeadline, /^ready\n(\d+)\n/);
      background = captureDescendant(Number(readiness[1]), child.pid, "background-Bash", clock.workDeadline, `bash ${driverPath}`);
      const launcher = background.identity;
      const context = await readSupervisorContext(directory, child.pid, launched.contextHash, clock.workDeadline);
      expect(context.configPath).toBe(configPath);
      expect(context.mode).toBe("identity-probe");
      expect(context.configuration?.runId ?? null).toBe(successor ? "successor-run" : null);
      expect(ownedIdentity(launcher, processIdentity(launcher.pid, clock.workDeadline), child.pid)).toBe(true);
      if (successor) {
        const bytes = await clock.work(readFile(configPath)), changed = JSON.parse(bytes);
        changed.runId = "changed-successor-run";
        await clock.work(chmod(configPath, 0o600));
        await clock.work(json(configPath, changed));
        await clock.work(chmod(configPath, 0o400));
        await clock.work(expect(readSupervisorContext(directory, child.pid, launched.contextHash, clock.workDeadline)).rejects.toThrow("supervisor configuration changed"));
        await clock.work(chmod(configPath, 0o600));
        await clock.work(writeFile(configPath, bytes));
        await clock.work(chmod(configPath, 0o400));
      }
      await clock.work(expect(readSupervisorContext(directory, child.pid, "0".repeat(64), clock.workDeadline)).rejects.toThrow("unbound supervisor context"));
      await clock.work(expect(readSupervisorContext(directory, process.pid, launched.contextHash, clock.workDeadline)).rejects.toThrow("supervisor ancestry or driver identity changed"));
      expect(await shellLauncherGuard(guards[0][0], launcher, clock)).toBe(0);
      const proof = await clock.work(syntheticServerProof(base, launcher, clock));
      expect(proof.verified).toBe(true);
      expect(proof.launcher).toEqual(launcher);
      expect(ownedIdentity(launcher, processIdentity(launcher.pid, clock.workDeadline), child.pid)).toBe(true);
      evidence.push({ form: successor ? "successor" : "baseline", supervisor: context.supervisor, driver: launched.identity, launcher, contextHash: launched.contextHash, configHash: context.configHash, verifiedReadinessBeforeTamper: true, shellGuardAccepted: true, proofGuardAccepted: proof.verified, syntheticFields: ["Herdr server identity and listener PID", "stage and root metadata", "fixture config"], realFields: ["driver, validator and background Bash OS identities", "private local socket nodes"] });
    } catch (error) { fail(error); }
    finally {
      cleanupPhase("owned Bash integration");
      try {
        if (collected) {
          collected.child.stdio[3].end();
          try { await waitBefore(collected.closed, Math.min(clock.cleanupDeadline, Date.now() + 500), "driver cooperative close"); }
          catch {
            if (launchSent && !background) throw new Error("background ownership unavailable; potentially live descendants retained");
            if (background && !processIdentity(background.identity.pid, clock.cleanupDeadline)?.absent) {
              const stopped = await stopOwnedChild({ pid: background.identity.pid, kill: signal => process.kill(background.identity.pid, signal) }, background.identity, clock.cleanupDeadline, processIdentity, collected.child.pid);
              if (!stopped.directChildAbsent) throw new Error(`background cleanup refused: ${stopped.refusal}`);
            }
            proveValidatorClosure(collected.child.pid, clock.cleanupDeadline);
            collected.safeToTerminate = true;
            await closeOwnedHelper(collected, clock.cleanupDeadline, { descendantsComplete: true });
          }
          proveValidatorClosure(collected.child.pid, clock.cleanupDeadline);
          if (launchSent && !background) throw new Error("background ownership unavailable after driver exit");
          if (background) {
            const absent = processIdentity(background.identity.pid, clock.cleanupDeadline);
            observeHelper(background.id, absent, "driver wait joined background; fresh absence", { parentExit: collected.exit, parentStreamClose: collected.result, ownedControlClosed: true });
            expect(absent?.absent).toBe(true);
          }
          const result = await closeOwnedHelper(collected, clock.cleanupDeadline, { descendantsComplete: true });
          if (!firstError) expect(result).toEqual({ code: 0, signal: null });
          expect(Buffer.concat(collected.stderr).toString()).toBe("");
          expect(Buffer.concat(collected.stdout).length).toBeLessThan(64);
        }
        if (firstError || lifecycleEvidence()?.uncertainEffects) throw new Error("failed bootstrap/work fixture retained; closure cannot be promoted to success");
        await waitBefore(rm(base, { recursive: true, force: true }), clock.cleanupDeadline, "closed fixture removal");
        cleanupComplete = true;
      } catch (error) { lifecycleFailure(error, true); firstError ??= error; }
      lifecycleEvidence(state => { state.fixtureRoots ??= []; state.fixtureRoots.push({ path: base, cleanupComplete, retained: !cleanupComplete }); });
    }
    if (firstError) throw firstError;
  }
  lifecycleEvidence(state => { state.phase = "negative Bash integration: work"; });
  const base = await clock.work(sandbox());
  let nearCollected, unboundCollected, unboundSent = false, cleanupComplete = false;
  try {
    const directory = join(base, "b");
    await clock.work(mkdir(directory, { mode: 0o700 }));
    const nearPath = join(base, "pi-scroll-qualification.sh.bak");
    await clock.work(writeFile(nearPath, "builtin printf 'near-ready\\n'\nbuiltin read -r -t 10 <&3 || true\n", { mode: 0o600 }));
    const invocation = supervisedDriverInvocation(directory);
    if (Date.now() >= clock.workDeadline) throw new Error("negative bootstrap deadline expired");
    const nearChild = spawn(invocation.command, [nearPath], invocation.options);
    nearCollected = collectOwnedHelper(nearChild, "near-path-Bash", { descendants: "source-known leaf" });
    nearChild.stdio[3].on("error", () => {});
    const nearIdentity = await captureHelper(nearCollected, clock.workDeadline, `bash ${nearPath}`);
    await readyLine(nearCollected, clock.workDeadline, /^near-ready\n/);
    expect(ownedIdentity(nearIdentity, processIdentity(nearChild.pid, clock.workDeadline))).toBe(true);
    expect(await shellLauncherGuard(guards[0][0], nearIdentity, clock)).toBe(1);
    await clock.work(expect(syntheticServerProof(base, nearIdentity, clock, { expectedCommandRefusal: true })).rejects.toMatchObject({ code: "herdr_command_identity" }));
    expect(await closeOwnedHelper(nearCollected, clock.cleanupDeadline, { closeControl: true })).toEqual({ code: 0, signal: null });
    if (Date.now() >= clock.workDeadline) throw new Error("unbound bootstrap deadline expired");
    const unbound = spawn(invocation.command, invocation.args, invocation.options);
    unboundCollected = collectOwnedHelper(unbound, "unbound-driver", { descendants: "only context validator; rejected before background admission" });
    unbound.stdio[3].on("error", () => {});
    await captureHelper(unboundCollected, clock.workDeadline, `bash ${driverPath}`);
    if (Date.now() >= clock.workDeadline) throw new Error("unbound context deadline expired");
    unboundSent = true;
    await clock.work(new Promise((resolve, reject) => unbound.stdio[3].end(`${"0".repeat(64)}\n`, error => error ? reject(error) : resolve())));
    await clock.work(unboundCollected.closed);
    proveValidatorClosure(unbound.pid, clock.workDeadline);
    expect(await closeOwnedHelper(unboundCollected, clock.cleanupDeadline, { descendantsComplete: true })).toEqual({ code: 1, signal: null });
    expect(Buffer.concat(unboundCollected.stdout).length).toBe(0);
    expect(Buffer.concat(unboundCollected.stderr).length).toBeLessThan(4096);
    expect(Buffer.concat(unboundCollected.stderr).toString()).toContain("boundsFailure");
    evidence.push({ form: "near-path", launcher: nearIdentity, shellGuardAccepted: false, proofGuardAccepted: false, unboundContextRefusedBeforeNativeResolution: true });
  } catch (error) { fail(error); }
  finally {
    cleanupPhase("negative Bash integration");
    try {
      if (nearCollected) await closeOwnedHelper(nearCollected, clock.cleanupDeadline, { closeControl: true });
      if (unboundCollected) {
        unboundCollected.child.stdio[3].end();
        await waitBefore(unboundCollected.closed, clock.cleanupDeadline, "unbound driver closure");
        if (unboundSent) proveValidatorClosure(unboundCollected.child.pid, clock.cleanupDeadline);
        await closeOwnedHelper(unboundCollected, clock.cleanupDeadline, { descendantsComplete: true });
      }
      if (firstError || lifecycleEvidence()?.uncertainEffects) throw new Error("failed negative fixture retained; closure cannot be promoted to success");
      await waitBefore(rm(base, { recursive: true, force: true }), clock.cleanupDeadline, "negative fixture removal");
      cleanupComplete = true;
    } catch (error) { lifecycleFailure(error, true); firstError ??= error; }
    lifecycleEvidence(state => { state.fixtureRoots ??= []; state.fixtureRoots.push({ path: base, cleanupComplete, retained: !cleanupComplete }); });
  }
  if (firstError) throw firstError;
  console.log(`owned_bash_integration=${JSON.stringify(evidence)}`);
}, 15000);

test("the actual shared driver/fixture contract binds the root marker, UID, stage, attempt and exact private socket boundary", async () => {
  const base = await sandbox(), sockets = [];
  try {
    const stageRoot = join(base, "n"), fixtureRootParent = join(base, "f"), stageId = "n", runId = "fixture-run", token = "fixture-lock";
    await mkdir(fixtureRootParent, { mode: 0o700 });
    await initializeStage(stageRoot, { stageId, runId, fixtureRootParent });
    await acquireStageLock(stageRoot, { token, runId, shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" });
    const allocation = await allocateAttempt(stageRoot, { token, requestedId: "attempt-001" }), root = allocation.root_path, uid = process.getuid();
    await mkdir(join(root, "config/herdr"), { recursive: true, mode: 0o700 });
    await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
    const markerPath = join(root, ".stage-owner.json"), marker = { stage_id: stageId, run_id: runId, attempt_id: allocation.attempt_id, root_path: root, uid };
    await json(markerPath, marker);
    await markRootCreated(stageRoot, { token, attemptId: allocation.attempt_id });
    const socketPath = join(root, "config/herdr/herdr.sock");
    sockets.push(await listen(socketPath));
    sockets.push(await listen(join(root, "config/herdr/herdr-client.sock")));
    const input = { stageRoot, stageId, runId, attemptId: allocation.attempt_id, root, socketPath, lockToken: token, uid };
    expect((await validateFixtureContract(input)).marker_path).toBe(join(root, "fixture-owner"));
    for (const mutation of [{ root_path: join(base, "copied-root") }, { uid: uid + 1 }, { stage_id: "other" }, { run_id: "other" }, { attempt_id: "attempt-002" }]) {
      await json(markerPath, { ...marker, ...mutation });
      await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "root_owner_mismatch" });
    }
    await json(markerPath, marker);
    await expect(validateFixtureContract({ ...input, uid: uid + 1 })).rejects.toMatchObject({ code: "unsafe_directory" });
    await expect(validateFixtureContract({ ...input, root: join(base, "escape") })).rejects.toMatchObject({ code: "fixture_identity" });
    await expect(validateFixtureContract({ ...input, socketPath: join(root, "herdr.sock") })).rejects.toMatchObject({ code: "wrong_socket_path" });
    await expect(validateFixtureContract({ ...input, lockToken: "forged" })).rejects.toMatchObject({ code: "stage_locked" });
    await writeFile(join(root, "fixture-owner"), "wrong\n");
    await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "fixture_marker_mismatch" });
    await rm(join(root, "fixture-owner"));
    await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "unsafe_file" });
    await symlink(markerPath, join(root, "fixture-owner"));
    await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "unsafe_file" });
    await rm(join(root, "fixture-owner"));
    await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
    const link = join(base, "link");
    await symlink(stageRoot, link);
    await expect(validateFixtureContract({ ...input, stageRoot: link })).rejects.toMatchObject({ code: "noncanonical_path" });
    await chmod(join(root, "config/herdr"), 0o755);
    await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "unsafe_directory" });
    await chmod(join(root, "config/herdr"), 0o700);
    await chmod(socketPath, 0o660);
    await expect(validateFixtureContract(input)).rejects.toMatchObject({ code: "unsafe_socket" });
  } catch (error) { lifecycleFailure(error); throw error; }
  finally {
    const deadline = Date.now() + 1000;
    try {
      for (const socket of sockets) await close(socket, deadline);
      await waitBefore(rm(base, { recursive: true, force: true }), deadline, "shared fixture removal");
    } catch (error) { lifecycleFailure(error, true); throw error; }
  }
});

test("bounds reserve cleanup separately and enforce actual helper timeout and output limits with retained partial bytes", async () => {
  const clock = lifecycle("bounded command regressions", 1500), base = await clock.work(sandbox());
  try {
    const limits = { workMs: 100, cleanupMs: 80, workBytes: 512, cleanupBytes: 256 };
    const reservation = reserveBounds(base, limits, 1000);
    expect(reservation.workDeadline).toBe(1100);
    expect(reservation.cleanupDeadline).toBeNull();
    expect(beginCleanup(base, 1090).cleanupDeadline).toBe(1170);
    expect(beginCleanup(base, 1150).cleanupDeadline).toBe(1170);
    expect(completeBounds(base, true, 1160)).toBe(true);
    expect(() => completeBounds(base, true, 1170)).toThrow("cleanup reserve expired");
    await mkdir(join(base, "commands"), { mode: 0o700 });
    await json(join(base, "commands/uncertain.json"), { possiblyLiveEffects: true });
    expect(uncertainCommandEffects(base)).toBe(true);
    expect(completeBounds(base, true, 1160)).toBe(false);
    expect(JSON.parse(await readFile(join(base, "state.json"))).cleanupComplete).toBe(false);
    expect(() => validateBounds({ ...limits, cleanupMs: 0 })).toThrow("invalid qualification bounds");
    const timeout = await clock.work(runBoundedCommand("/bin/sleep", ["2"], { deadline: Date.now() + 150, cleanupDeadline: clock.cleanupDeadline, maxBytes: 512, leafOnly: true }));
    expect(timeout.reason).toBe("timeout");
    expect(timeout.cleanup.directChildAbsent).toBe(true);
    expect(timeout.cleanup.signalSent).toBe(true);
    expect(timeout.possiblyLiveEffects).toBe(true);
    const capped = await clock.work(runBoundedCommand("/bin/bash", ["--noprofile", "--norc", "-c", 'builtin read -r payload; builtin printf "%s" "$payload"'], { deadline: Math.min(clock.workDeadline, Date.now() + 1000), cleanupDeadline: clock.cleanupDeadline, maxBytes: 64, input: `${"x".repeat(8192)}\n`, leafOnly: true }));
    expect(capped.reason).toBe("output_cap");
    expect(capped.retainedBytes).toBe(64);
    expect(capped.stdout.toString()).toBe("x".repeat(64));
    expect(capped.stderr.length).toBe(0);
    const expired = await clock.work(runBoundedCommand("/bin/sleep", ["2"], { deadline: Date.now() - 1, maxBytes: 512, leafOnly: true }));
    expect(expired.started).toBe(false);
    expect(expired.possiblyLiveEffects).toBe(false);
    const noOutputBudget = await clock.work(runBoundedCommand("/bin/sleep", ["2"], { deadline: Date.now() + 1000, maxBytes: 0, leafOnly: true }));
    expect(noOutputBudget.started).toBe(false);
    expect(noOutputBudget.reason).toBe("output_cap");
  } catch (error) { lifecycleFailure(error); throw error; }
  finally {
    cleanupPhase("bounded command regressions");
    const uncertain = lifecycleEvidence()?.helpers.some(helper => !helper.identity || !helper.absenceProved);
    if (!uncertain) await waitBefore(rm(base, { recursive: true, force: true }), clock.cleanupDeadline, "bounded command fixture removal");
  }
});

test("cleanup refuses absent, replaced or wrong-UID ownership and expiry without a signal; owned helper cleanup proves absence", async () => {
  const clock = lifecycle("owned cleanup regressions", 1500);
  let signals = 0;
  const fake = { pid: 123456, kill: () => { signals++; } };
  const identity = { pid: fake.pid, parentPid: process.pid, uid: process.getuid(), startIdentity: "synthetic-start", commandIdentity: "synthetic-helper" };
  expect(ownedIdentity(identity, { ...identity, uid: identity.uid + 1 })).toBe(false);
  for (const [expected, observed] of [[null, identity], [identity, null], [identity, { ...identity, startIdentity: "replaced" }], [identity, { ...identity, commandIdentity: "other" }], [identity, { ...identity, uid: identity.uid + 1 }]]) {
    const refused = await clock.work(stopOwnedChild(fake, expected, Date.now() + 100, () => observed));
    expect(refused.refusal).toBe("ownership_not_proven");
    expect(refused.directChildAbsent).toBe(false);
  }
  expect((await clock.work(stopOwnedChild(fake, identity, Date.now() - 1, () => identity))).refusal).toBe("cleanup_deadline");
  expect(signals).toBe(0);
  if (Date.now() >= clock.workDeadline) throw new Error("cleanup regression bootstrap expired");
  const child = spawn("/bin/sleep", ["2"], { detached: false, stdio: "ignore", env: { PATH: "/usr/bin:/bin", HOME: process.env.HOME, LC_ALL: "C" } });
  const collected = collectOwnedHelper(child, "cleanup-sleep", { descendants: "source-known leaf" });
  let firstError = null;
  try {
    const captured = await captureHelper(collected, clock.workDeadline);
    expect(ownedIdentity(captured, processIdentity(child.pid, clock.workDeadline))).toBe(true);
    const stopped = await clock.work(stopOwnedChild(child, captured, Math.min(clock.cleanupDeadline, Date.now() + 1000)));
    expect(stopped.directChildAbsent).toBe(true);
    expect(stopped.signalSent).toBe(true);
  } catch (error) { firstError = error; lifecycleFailure(error); }
  finally {
    cleanupPhase("owned cleanup regressions");
    try { await closeOwnedHelper(collected, clock.cleanupDeadline); }
    catch (error) { lifecycleFailure(error, true); firstError ??= error; }
  }
  if (firstError) throw firstError;
});

test("all three Go entry points invoke the same live contract, and driver preserves the exact accepted launch/proof predicates", async () => {
  const app = await readFile(new URL("../internal/app/pi_scroll_fixture_test.go", import.meta.url), "utf8");
  const latency = await readFile(new URL("../internal/coordinator/scroll_latency_fixture_test.go", import.meta.url), "utf8");
  const row = await readFile(new URL("../internal/coordinator/scroll_row_lease_fixture_test.go", import.meta.url), "utf8");
  for (const source of [app, latency, row]) {
    expect(source).toContain("verifyDisposablePiFixture(t, socket)");
    expect(source).not.toContain("strings.HasPrefix");
    expect(source).not.toContain('filepath.Dir(socket), "fixture-owner"');
  }
  for (const source of [app, latency]) expect(source).toContain('"../../tests/pi-scroll-qualification-ledger.mjs"');
  const driver = await readFile(new URL("./pi-scroll-qualification.sh", import.meta.url), "utf8");
  expect(driver).toContain('HERDR_P1_CONTRACT_JSON="$FIXTURE_CONTRACT_JSON"');
  expect(driver).toContain('supervise "$BOUNDS_DIR" "$SUCCESSOR_CONFIG"');
  expect(driver).toContain('begin-cleanup "$BOUNDS_DIR"');
  expect(driver).toContain('ulimit -f 64');
  const guarded = driver.match(/herdr\(\) \{([\s\S]*?)\n\}/)[1];
  expect(guarded.indexOf("tmux_proof_call verify >/dev/null || return 1")).toBeLessThan(guarded.indexOf("herdr_proof_call verify >/dev/null || return 1"));
  expect(guarded.indexOf("herdr_proof_call verify >/dev/null || return 1")).toBeLessThan(guarded.indexOf('clean_env "$HERDR_BIN" "$@"'));
  expect(driver).toContain('HERDR_COMMAND" != "bash tests/pi-scroll-qualification.sh" && "$HERDR_COMMAND" != "bash $SCRIPT_DIR/pi-scroll-qualification.sh"');
  expect(driver.indexOf('capture_pi_snapshot "$PI_PID" second')).toBeLessThan(driver.indexOf('record_process pi-agent "$PI_PID"'));
});

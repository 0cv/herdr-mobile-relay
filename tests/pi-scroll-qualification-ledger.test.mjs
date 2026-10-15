import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  acquireStageLock,
  allocateAttempt,
  claimStageLock,
  initializeStage,
  markRootCreated,
  recordAttemptOutcome,
  releaseStageLock,
  stageSummary,
} from "./pi-scroll-qualification-ledger.mjs";

test("stage allocations ignore unrelated roots and reject invalid or exhausted allocation without fixture creation", async () => {
  const sandbox = await realpath(await mkdtemp(join(tmpdir(), "pi-scroll-counter-regression-")));
  try {
    const stageRoot = join(sandbox, "stage");
    const fixtureRootParent = join(sandbox, "fixture-roots");
    const historyRoot = join(sandbox, "unrelated-history");
    await mkdir(fixtureRootParent, { mode: 0o700 });
    await mkdir(historyRoot, { mode: 0o700 });
    for (let index = 1; index <= 105; index++) {
      await mkdir(join(historyRoot, `herdr-scroll-p1-historical-${String(index).padStart(3, "0")}`), { mode: 0o700 });
    }
    const identity = {
      stageId: "counter-regression-stage",
      runId: "counter-regression-run",
      fixtureRootParent,
    };
    const initial = await initializeStage(stageRoot, identity);
    expect(initial.allocations).toHaveLength(0);
    expect(initial.owner.max_attempts).toBe(6);
    const token = "counter-regression-lock";
    await acquireStageLock(stageRoot, { token, runId: identity.runId, shellPid: process.pid, shellStart: "test", shellCommand: "bun test" });
    await expect(allocateAttempt(stageRoot, { token, requestedId: "attempt-999" })).rejects.toMatchObject({ code: "unknown-attempt-id" });
    expect((await stageSummary(stageRoot, {})).allocation_count).toBe(0);
    const first = await allocateAttempt(stageRoot, { token, requestedId: "attempt-001" });
    await expect(allocateAttempt(stageRoot, { token, requestedId: "attempt-001" })).rejects.toMatchObject({ code: "duplicate-attempt-id" });
    await recordAttemptOutcome(stageRoot, { token, attemptId: first.attempt_id, result: "blocked", rootCreated: false, cleanupComplete: true });
    for (let number = 2; number <= 6; number++) {
      const allocation = await allocateAttempt(stageRoot, { token, requestedId: `attempt-${String(number).padStart(3, "0")}` });
      await recordAttemptOutcome(stageRoot, { token, attemptId: allocation.attempt_id, result: "blocked", rootCreated: false, cleanupComplete: true });
    }
    await expect(allocateAttempt(stageRoot, { token, requestedId: "attempt-007" })).rejects.toMatchObject({ code: "attempt-limit" });
    const summary = await stageSummary(stageRoot, {});
    expect(summary.allocation_count).toBe(6);
    expect(summary.local_allocation_count).toBe(6);
    expect(summary.predecessor_allocation_count).toBe(0);
    expect(summary.cumulative_allocation_count).toBe(6);
    expect(summary.max_attempts).toBe(6);
    expect(summary.refusal_count).toBe(3);
    expect((await readdir(join(stageRoot, "attempts"))).sort()).toEqual([
      "attempt-001",
      "attempt-002",
      "attempt-003",
      "attempt-004",
      "attempt-005",
      "attempt-006",
    ]);
    expect(await readdir(fixtureRootParent)).toEqual([]);
    await releaseStageLock(stageRoot, { token });
    const afterRelease = await stageSummary(stageRoot, {});
    expect(afterRelease.allocation_count).toBe(6);
    expect(afterRelease.refusal_count).toBe(3);
  } finally {
    await rm(sandbox, { recursive: true, force: true });
  }
});

test("omitting the fixture parent selects the canonical platform default without relaxing explicit paths or the six-slot cap", async () => {
  const sandbox = await realpath(await mkdtemp(join(tmpdir(), "pi-scroll-default-")));
  try {
    const stageRoot = join(sandbox, "stage"), stageId = `default-${sandbox.split("/").at(-1)}`, runId = `${stageId}-run`;
    const initial = await initializeStage(stageRoot, { stageId, runId });
    expect(initial.owner.fixture_root_parent).toBe(await realpath("/tmp"));
    expect(await realpath(initial.owner.fixture_root_parent)).toBe(initial.owner.fixture_root_parent);
    expect(initial.owner.max_attempts).toBe(6);
    expect(initial.owner.successor).toBeUndefined();
    const explicitParent = join(sandbox, "roots"), link = join(sandbox, "link");
    await mkdir(explicitParent, { mode: 0o700 });
    await symlink(explicitParent, link);
    for (const fixtureRootParent of [link, `${explicitParent}/../roots`, `${explicitParent}/`]) {
      await expect(initializeStage(join(sandbox, "refused"), { stageId: "refused", runId, fixtureRootParent })).rejects.toMatchObject({ code: "noncanonical_path" });
    }
    if (process.platform === "darwin") {
      expect(initial.owner.fixture_root_parent).toBe("/private/tmp");
      await expect(initializeStage(join(sandbox, "refused"), { stageId: "refused", runId, fixtureRootParent: "/tmp" })).rejects.toMatchObject({ code: "noncanonical_path" });
    }
    const token = "default-lock";
    await acquireStageLock(stageRoot, { token, runId, shellPid: process.pid, shellStart: "synthetic", shellCommand: "bun test" });
    for (let number = 1; number <= 6; number++) {
      const allocation = await allocateAttempt(stageRoot, { token, requestedId: `attempt-${String(number).padStart(3, "0")}` });
      await recordAttemptOutcome(stageRoot, { token, attemptId: allocation.attempt_id, result: "blocked", rootCreated: false, cleanupComplete: true });
    }
    await expect(allocateAttempt(stageRoot, { token, requestedId: "attempt-007" })).rejects.toMatchObject({ code: "attempt-limit" });
    const summary = await stageSummary(stageRoot, {});
    expect(summary.max_attempts).toBe(6);
    expect(summary.allocation_count).toBe(6);
    expect(summary.cumulative_allocation_count).toBe(6);
    expect(summary.predecessor).toBeNull();
    await releaseStageLock(stageRoot, { token });
  } finally { await rm(sandbox, { recursive: true, force: true }); }
});

function hash(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function privateJson(path, value) {
  await writeFile(path, JSON.stringify(value) + "\n", { mode: 0o600 });
}

function absentPids(count) {
  const pids = [];
  for (let pid = Math.max(10000, process.pid + 100); pid < 99990 && pids.length < count; pid++) {
    const probe = spawnSync("/bin/ps", ["-p", String(pid), "-o", "lstart="], { encoding: "utf8" });
    if (probe.status === 1 && !probe.stdout.trim() && !probe.stderr.trim()) pids.push(pid);
  }
  if (pids.length !== count) throw new Error("could not reserve synthetic absent process identities");
  return pids;
}

async function syntheticExternalRecovery(sandbox, { liveOwner = false } = {}) {
  const stageRoot = join(sandbox, "stage");
  const fixtureRootParent = join(sandbox, "roots");
  const stageId = "synthetic-external-recovery";
  const runId = "synthetic-external-recovery-run";
  const uid = process.getuid();
  const [tmuxPid, bootstrapPid, launcherPid, herdrPid, absentOwnerPid] = absentPids(5);
  const oldOwnerPid = liveOwner ? process.pid : absentOwnerPid;
  const oldToken = "synthetic-old-owner";
  const start = "Wed Oct 7 01:19:14 2026";
  await mkdir(fixtureRootParent, { mode: 0o700 });
  await initializeStage(stageRoot, { stageId, runId, fixtureRootParent });
  await acquireStageLock(stageRoot, { token: oldToken, runId, shellPid: oldOwnerPid, shellStart: "Wed Oct 7 01:19:13 2026", shellCommand: "pi-scroll-qualification.sh" });
  for (let number = 1; number <= 2; number++) {
    const id = `attempt-${String(number).padStart(3, "0")}`;
    await allocateAttempt(stageRoot, { token: oldToken, requestedId: id });
    await recordAttemptOutcome(stageRoot, { token: oldToken, attemptId: id, result: "blocked", rootCreated: false, cleanupComplete: true });
    await expect(allocateAttempt(stageRoot, { token: oldToken, requestedId: "attempt-999" })).rejects.toMatchObject({ code: "unknown-attempt-id" });
  }
  const allocation = await allocateAttempt(stageRoot, { token: oldToken, requestedId: "attempt-003" });
  const root = allocation.root_path;
  const logs = join(root, "logs");
  const socketPath = join(root, "tmux", `tmux-${uid}`, "default");
  const herdrSockets = [join(root, "config", "herdr", "herdr.sock"), join(root, "config", "herdr", "herdr-client.sock")];
  await mkdir(root, { mode: 0o700 });
  for (const path of [logs, join(root, "tmux"), join(root, "tmux", `tmux-${uid}`), join(root, "config"), join(root, "config", "herdr")]) await mkdir(path, { mode: 0o700 });
  await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
  await privateJson(join(root, ".stage-owner.json"), { stage_id: stageId, run_id: runId, attempt_id: "attempt-003", root_path: root, uid });
  await writeFile(join(root, "tmux.conf"), "set-option -g default-size 100x36\n", { mode: 0o600 });
  const tmuxCommand = `/synthetic/tmux -f ${join(root, "tmux.conf")} new-session -d -s ${root.split("/").at(-1)} -x 100 -y 36 exec /bin/sleep 86400`;
  const launcherCommand = "bash tests/pi-scroll-qualification.sh";
  const herdrCommand = "/synthetic/herdr server";
  await writeFile(join(root, "owned-processes.tsv"), [
    "role\tpid\tstart_identity\tcommand_identity",
    `tmux-server\t${tmuxPid}\t${start}\t${tmuxCommand}`,
    `tmux-bootstrap\t${bootstrapPid}\t${start}\t/bin/sleep 86400`,
    `herdr-launcher\t${launcherPid}\t${start}\t${launcherCommand}`,
  ].join("\n") + "\n", { mode: 0o600 });
  await privateJson(join(logs, "tmux-discovery.json"), { synthetic: true });
  await privateJson(join(logs, "tmux-socket-proof.json"), { synthetic: true });
  const herdrProof = {
    stage_id: stageId, run_id: runId, attempt_id: "attempt-003", root,
    launch_process: { pid: launcherPid }, server_process: { pid: herdrPid, start_identity: start, command: herdrCommand, parent_pid: launcherPid },
    sockets: herdrSockets.map(path => ({ path, uid, mode: 0o600, listener_pids: [String(herdrPid)] })),
  };
  const herdrProofPath = join(logs, "herdr-postfailure-cleanup-proof.json");
  await privateJson(herdrProofPath, herdrProof);
  await privateJson(join(logs, "herdr-postfailure-cleanup.json"), {
    ownership_proof_sha256: hash(await readFile(herdrProofPath)),
    stop_status: 0,
    process_absence_checks: [{ pid: herdrPid, absent: true }, { pid: launcherPid, absent: true }],
    socket_reinspection: herdrSockets.map(path => ({ path, node_absent: true, listener_probe_status: 1, listener_pids: [] })),
  });
  await markRootCreated(stageRoot, { token: oldToken, attemptId: "attempt-003" });
  await recordAttemptOutcome(stageRoot, { token: oldToken, attemptId: "attempt-003", result: "blocked", rootCreated: true, cleanupComplete: false });
  const ledgerPath = join(stageRoot, "allocation-ledger.jsonl");
  const frozenLedger = await readFile(ledgerPath);
  const oldOwnerPath = join(stageRoot, "active.lock", "owner.json");
  const sourcePaths = {
    "stage-owner": join(stageRoot, "stage-owner.json"),
    "active-lock-owner": oldOwnerPath,
    "allocation-ledger": ledgerPath,
    "attempt-owner": join(stageRoot, "attempts", "attempt-003", "attempt-owner.json"),
    "root-created": join(stageRoot, "attempts", "attempt-003", "root-created.json"),
    "attempt-root-owner": join(root, ".stage-owner.json"),
    "fixture-owner": join(root, "fixture-owner"),
    "owned-processes": join(root, "owned-processes.tsv"),
    "tmux-config": join(root, "tmux.conf"),
    "tmux-discovery": join(logs, "tmux-discovery.json"),
    "tmux-socket-proof": join(logs, "tmux-socket-proof.json"),
    "herdr-cleanup-proof": herdrProofPath,
    "herdr-cleanup-result": join(logs, "herdr-postfailure-cleanup.json"),
  };
  const sources = Object.fromEntries(await Promise.all(Object.entries(sourcePaths).map(async ([name, path]) => [name, { absolute_path: path, sha256: hash(await readFile(path)) }])));
  const probes = [oldOwnerPid, tmuxPid, bootstrapPid, launcherPid, herdrPid].map(pid => ({ pid, status: 1, stdout: "", stderr: "" }));
  const identities = [
    { role: "tmux-server", pid: tmuxPid, startIdentity: start, commandIdentity: tmuxCommand, absent: true },
    { role: "tmux-bootstrap", pid: bootstrapPid, startIdentity: start, commandIdentity: "/bin/sleep 86400", absent: true },
    { role: "herdr-launcher", pid: launcherPid, start_identity: start, command_identity: launcherCommand, absent: true },
    { role: "herdr-server", pid: herdrPid, start_identity: start, command_identity: herdrCommand, absent: true },
  ];
  const precheck = {
    stage_id: stageId, stage_run_id: runId, attempt_id: "attempt-003", original_outcome_seq: 8,
    stage_lock: { path: join(stageRoot, "active.lock"), owner_pid: oldOwnerPid, owner_start_identity: "Wed Oct 7 01:19:13 2026", owner_process_absent: true },
    socket: { path: socketPath, uid, mode: 0o660, device: 1, inode: 2, canonical_path: true },
    original_owned_process_identities: identities, process_absence_probes: probes, hashes: sources,
  };
  const precheckPath = join(logs, "tmux-postfailure-recovery-precheck.json");
  await privateJson(precheckPath, precheck);
  const command = {
    stage_id: stageId, attempt_id: "attempt-003",
    input_identity: { root, socket_path: socketPath, uid, server_pid: tmuxPid, bootstrap_pid: bootstrapPid, server_start_identity: start, bootstrap_start_identity: start },
    status: 0, command_argv: ["/bin/unlink", socketPath],
    pre_removal_revalidation: { socket: { uid, mode: 0o660, device: 1, inode: 2 }, processes: probes },
  };
  const commandPath = join(logs, "tmux-postfailure-recovery-command.json");
  await privateJson(commandPath, command);
  const proofPath = join(logs, "tmux-postfailure-recovery-proof.json");
  const proof = {
    schema_version: 1, kind: "attempt-003-stale-tmux-socket-cleanup-recovery-proof",
    stage_id: stageId, stage_run_id: runId, attempt_id: "attempt-003", linked_allocation_seq: 7, linked_original_outcome_seq: 8,
    precheck_path: precheckPath, precheck_sha256: hash(await readFile(precheckPath)),
    cleanup_command_path: commandPath, cleanup_command_sha256: hash(await readFile(commandPath)),
    cleanup_command_status: 0, cleanup_command_result: { removed: true, socket_path: socketPath },
    post_cleanup_process_absence_probes: probes, post_cleanup_socket_node: { present: false },
    original_ledger_outcome_preserved: { result: "blocked", cleanup_complete: false }, source_hashes: sources,
  };
  await privateJson(proofPath, proof);
  return { stageRoot, root, proofPath, proof, oldOwnerPath, oldOwnerPid, frozenLedger, runId, uid, socketPath };
}

test("external attempt-003 proof permits only verified sequential recovery without rewriting history", async () => {
  const sandbox = await realpath(await mkdtemp(join(tmpdir(), "pi-scroll-external-recovery-")));
  try {
    const fixture = await syntheticExternalRecovery(sandbox);
    const { stageRoot, proofPath, proof, oldOwnerPath, frozenLedger, runId, socketPath } = fixture;
    const claim = { token: "synthetic-new-owner", runId, shellPid: process.pid, shellStart: "test", shellCommand: "pi-scroll-qualification.sh", externalRecoveryProofPath: proofPath };
    await expect(claimStageLock(stageRoot, { ...claim, externalRecoveryProofPath: "" })).rejects.toMatchObject({ code: "unsafe_prior_outcome" });
    await expect(claimStageLock(stageRoot, { ...claim, externalRecoveryProofPath: `${proofPath}.other` })).rejects.toMatchObject({ code: "external_recovery_path" });
    await privateJson(proofPath, { ...proof, stage_id: "other-stage" });
    await expect(claimStageLock(stageRoot, claim)).rejects.toMatchObject({ code: "external_recovery_proof" });
    await privateJson(proofPath, proof);
    await privateJson(proofPath, { ...proof, attempt_id: "attempt-002" });
    await expect(claimStageLock(stageRoot, claim)).rejects.toMatchObject({ code: "external_recovery_proof" });
    await privateJson(proofPath, proof);
    await writeFile(socketPath, "not a socket", { mode: 0o600 });
    await expect(claimStageLock(stageRoot, claim)).rejects.toMatchObject({ code: "external_recovery_socket_live" });
    await rm(socketPath);
    const ownerBytes = await readFile(oldOwnerPath);
    await privateJson(oldOwnerPath, { token: "unrelated", run_id: runId, shell_pid: fixture.oldOwnerPid, shell_start: "test", shell_command: "pi-scroll-qualification.sh" });
    await expect(claimStageLock(stageRoot, claim)).rejects.toMatchObject({ code: "external_recovery_source" });
    await writeFile(oldOwnerPath, ownerBytes);
    const proofBytes = await readFile(proofPath);
    await rm(proofPath);
    await symlink(join(fixture.root, "fixture-owner"), proofPath);
    await expect(claimStageLock(stageRoot, claim)).rejects.toMatchObject({ code: "unsafe_file" });
    await rm(proofPath);
    await writeFile(proofPath, proofBytes, { mode: 0o600 });
    const result = await claimStageLock(stageRoot, claim);
    expect(result.recovered).toBe(true);
    expect(await readFile(join(fixture.root, "logs", "stage-lock-owner-before-recovery.json"))).toEqual(ownerBytes);
    expect(await readFile(join(stageRoot, "allocation-ledger.jsonl"))).toEqual(frozenLedger);
    const next = await allocateAttempt(stageRoot, { token: claim.token, requestedId: "attempt-004", externalRecoveryProofPath: proofPath });
    expect(next.attempt_number).toBe(4);
    expect(next.seq).toBe(9);
    expect((await readFile(join(stageRoot, "allocation-ledger.jsonl"))).subarray(0, frozenLedger.length)).toEqual(frozenLedger);
    await recordAttemptOutcome(stageRoot, { token: claim.token, attemptId: "attempt-004", result: "blocked", rootCreated: false, cleanupComplete: true, externalRecoveryProofPath: proofPath });
    const summary = await stageSummary(stageRoot, { externalRecoveryProofPath: proofPath });
    expect(summary.allocation_count).toBe(4);
    expect(summary.max_attempts).toBe(6);
    expect(summary.allocations[2].outcome.cleanup_complete).toBe(false);
    expect(summary.allocations[3].outcome.cleanup_complete).toBe(true);
    expect((await readFile(join(stageRoot, "allocation-ledger.jsonl"))).subarray(0, frozenLedger.length)).toEqual(frozenLedger);
    await expect(allocateAttempt(stageRoot, { token: claim.token, requestedId: "attempt-005", externalRecoveryProofPath: proofPath })).rejects.toMatchObject({ code: "attempt005_gate_stage" });
    const liveSandbox = await realpath(await mkdtemp(join(sandbox, "live-")));
    const liveFixture = await syntheticExternalRecovery(liveSandbox, { liveOwner: true });
    await expect(claimStageLock(liveFixture.stageRoot, { ...claim, externalRecoveryProofPath: liveFixture.proofPath })).rejects.toMatchObject({ code: "external_recovery_process_live" });
  } finally {
    await rm(sandbox, { recursive: true, force: true });
  }
});

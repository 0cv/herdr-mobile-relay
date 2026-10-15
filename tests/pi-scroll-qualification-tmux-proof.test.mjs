import { expect, test } from "bun:test";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { chmod, lstat, mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import { allocateAttempt, acquireStageLock, initializeStage, markRootCreated } from "./pi-scroll-qualification-ledger.mjs";
import { removeStaleTmuxSocket, validateTmuxProof } from "./pi-scroll-qualification-tmux-proof.mjs";

function unusedPids(count) {
  const pids = [];
  for (let pid = Math.max(10000, process.pid + 100); pid < 99990 && pids.length < count; pid += 1) {
    const result = spawnSync("/bin/ps", ["-p", String(pid), "-o", "lstart="], { encoding: "utf8" });
    if (result.status === 1 && !result.stdout.trim() && !result.stderr.trim()) pids.push(pid);
  }
  if (pids.length !== count) throw new Error("could not reserve absent synthetic process identities");
  return pids;
}

async function createFixture() {
  const sandbox = await realpath(await mkdtemp(join(tmpdir(), "pi-scroll-tmux-proof-")));
  const stageId = "socket-proof-regression";
  const runId = "socket-proof-run";
  const stageRoot = join(sandbox, "stage");
  const fixtureRootParent = join(sandbox, "fixtures");
  await mkdir(fixtureRootParent, { mode: 0o700 });
  await initializeStage(stageRoot, { stageId, runId, fixtureRootParent });
  const lockToken = "socket-proof-lock";
  await acquireStageLock(stageRoot, { token: lockToken, runId, shellPid: process.pid, shellStart: "test-only", shellCommand: "bun test" });
  const allocation = await allocateAttempt(stageRoot, { token: lockToken, requestedId: "attempt-001" });
  const root = allocation.root_path;
  const uid = process.getuid();
  const rootTmuxPath = join(root, "tmux");
  const socketDirectory = join(rootTmuxPath, `tmux-${uid}`);
  await mkdir(root, { mode: 0o700 });
  await mkdir(rootTmuxPath, { mode: 0o700 });
  await mkdir(socketDirectory, { mode: 0o700 });
  await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
  await writeFile(join(root, ".stage-owner.json"), `${JSON.stringify({ stage_id: stageId, run_id: runId, attempt_id: allocation.attempt_id, root_path: root, uid })}\n`, { mode: 0o600 });
  const tmuxBin = await realpath(process.execPath);
  const [serverPid, bootstrapPid] = unusedPids(2);
  const server = {
    pid: serverPid,
    startIdentity: "Wed 7 Oct 00:28:18 2026",
    commandIdentity: `${tmuxBin} -f ${join(root, "tmux.conf")} new-session -d -s ${basename(root)} -x 100 -y 36 exec /bin/sleep 86400`,
  };
  const bootstrap = {
    pid: bootstrapPid,
    startIdentity: "Wed 7 Oct 00:28:18 2026",
    commandIdentity: "/bin/sleep 86400",
    parentPid: server.pid,
  };
  await writeFile(join(root, "tmux.conf"), "set-option -g default-size 100x36\n", { mode: 0o600 });
  await markRootCreated(stageRoot, { token: lockToken, attemptId: allocation.attempt_id });
  await writeFile(join(root, "owned-processes.tsv"), [
    "role\tpid\tstart_identity\tcommand_identity",
    `tmux-server\t${server.pid}\t${server.startIdentity}\t${server.commandIdentity}`,
    `tmux-bootstrap\t${bootstrap.pid}\t${bootstrap.startIdentity}\t${bootstrap.commandIdentity}`,
  ].join("\n") + "\n", { mode: 0o600 });

  const socketPath = join(socketDirectory, "default");
  const socketServer = net.createServer();
  await new Promise((resolve, reject) => {
    socketServer.once("error", reject);
    socketServer.listen(socketPath, resolve);
  });
  await chmod(socketPath, 0o600);
  const input = { stageRoot, root, stageId, runId, attemptId: allocation.attempt_id, uid, socketPath, lockToken, tmuxBin, lsofBin: "/usr/sbin/lsof", server, bootstrap };
  const observed = {
    server: { ...server, startIdentity: "Wed Oct 7 00:28:18 2026", parentPid: 1, uid },
    bootstrap: { ...bootstrap, startIdentity: "Wed Oct 7 00:28:18 2026", uid },
    socketOwnerPids: [String(server.pid)],
  };
  return { sandbox, root, socketPath, socketServer, input, observed };
}

test("isolated tmux socket proof accepts safe modes and rejects unsafe ownership, paths, modes, and process identities", async () => {
  const fixture = await createFixture();
  let abandonedSocketOwner;
  try {
    expect((await validateTmuxProof(fixture.input, fixture.observed)).socket_mode).toBe(0o600);
    await chmod(fixture.socketPath, 0o660);
    expect((await validateTmuxProof(fixture.input, fixture.observed)).socket_mode).toBe(0o660);

    await chmod(fixture.socketPath, 0o644);
    await expect(validateTmuxProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "unsafe_socket" });
    await chmod(fixture.socketPath, 0o600);

    await expect(validateTmuxProof({ ...fixture.input, uid: fixture.input.uid + 1 }, fixture.observed)).rejects.toMatchObject({ code: "unsafe_file" });
    await expect(validateTmuxProof({ ...fixture.input, socketPath: `${fixture.socketPath}.other` }, fixture.observed)).rejects.toMatchObject({ code: "wrong_socket_path" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, socketOwnerPids: [String(fixture.input.server.pid), "910003"] })).rejects.toMatchObject({ code: "tmux_server_count" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, pid: fixture.input.server.pid + 1 } })).rejects.toMatchObject({ code: "tmux_server_identity" });
    await expect(validateTmuxProof({ ...fixture.input, server: { ...fixture.input.server, startIdentity: "Thu 8 Oct 00:28:18 2026" } }, fixture.observed)).rejects.toMatchObject({ code: "tmux_server_identity" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, commandIdentity: `${fixture.observed.server.commandIdentity} --changed` } })).rejects.toMatchObject({ code: "tmux_server_identity" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, uid: fixture.input.uid + 1 } })).rejects.toMatchObject({ code: "tmux_server_identity" });
    await expect(validateTmuxProof({ ...fixture.input, bootstrap: { ...fixture.input.bootstrap, startIdentity: "Thu 8 Oct 00:28:18 2026" } }, fixture.observed)).rejects.toMatchObject({ code: "tmux_bootstrap_identity" });
    await expect(validateTmuxProof({ ...fixture.input, bootstrap: { ...fixture.input.bootstrap, pid: fixture.input.bootstrap.pid + 1 } }, fixture.observed)).rejects.toMatchObject({ code: "tmux_bootstrap_identity" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, bootstrap: { ...fixture.observed.bootstrap, commandIdentity: "/bin/sleep 123" } })).rejects.toMatchObject({ code: "tmux_bootstrap_identity" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, bootstrap: { ...fixture.observed.bootstrap, uid: fixture.input.uid + 1 } })).rejects.toMatchObject({ code: "tmux_bootstrap_identity" });
    await expect(validateTmuxProof({ ...fixture.input, server: { ...fixture.input.server, startIdentity: "Wed 32 Oct 00:28:18 2026" } }, fixture.observed)).rejects.toMatchObject({ code: "invalid_process_start_identity" });
    await expect(validateTmuxProof(fixture.input, { ...fixture.observed, bootstrap: { ...fixture.observed.bootstrap, startIdentity: "Wed Oct 7 00:28:18 2026 trailing" } })).rejects.toMatchObject({ code: "invalid_process_start_identity" });
    await expect(validateTmuxProof({ ...fixture.input, server: { ...fixture.input.server, startIdentity: "Wed 7 10 00:28:18 2026" } }, fixture.observed)).rejects.toMatchObject({ code: "invalid_process_start_identity" });
    await expect(validateTmuxProof({ ...fixture.input, bootstrap: { ...fixture.input.bootstrap, parentPid: 910003 } }, { ...fixture.observed, bootstrap: { ...fixture.observed.bootstrap, parentPid: 910003 } })).rejects.toMatchObject({ code: "tmux_bootstrap_identity" });

    await chmod(join(fixture.root, "tmux"), 0o755);
    await expect(validateTmuxProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "unsafe_directory" });
    await chmod(join(fixture.root, "tmux"), 0o700);

    await new Promise(resolve => fixture.socketServer.close(resolve));
    abandonedSocketOwner = spawn(process.execPath, ["-e", "import net from 'node:net';import {chmodSync} from 'node:fs';const server=net.createServer();server.listen(process.env.SOCKET_PATH,()=>{chmodSync(process.env.SOCKET_PATH,0o660);process.stdout.write('ready\\n')})"], {
      env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: fixture.sandbox, LC_ALL: "C", SOCKET_PATH: fixture.socketPath },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let childOutput = "";
    const ready = new Promise((resolve, reject) => {
      abandonedSocketOwner.stdout.setEncoding("utf8");
      abandonedSocketOwner.stdout.on("data", chunk => {
        childOutput += chunk;
        if (childOutput.includes("ready\n")) resolve();
      });
      abandonedSocketOwner.once("error", reject);
      abandonedSocketOwner.once("exit", (code, signal) => reject(new Error(`synthetic socket owner exited before ready: ${code} ${signal}`)));
    });
    let readyTimeout;
    try {
      await Promise.race([ready, new Promise((_, reject) => { readyTimeout = setTimeout(() => reject(new Error("synthetic socket owner did not become ready")), 5000); })]);
    } finally {
      clearTimeout(readyTimeout);
    }
    abandonedSocketOwner.kill("SIGKILL");
    const [childExitCode, childExitSignal] = await once(abandonedSocketOwner, "exit");
    expect(childExitCode).toBeNull();
    expect(childExitSignal).toBe("SIGKILL");
    abandonedSocketOwner = null;
    const staleSocketProof = await removeStaleTmuxSocket(fixture.input);
    expect(staleSocketProof).toMatchObject({ removed: true, socket_path: fixture.socketPath, socket_stat_fallback: false });
    await expect(lstat(fixture.socketPath)).rejects.toMatchObject({ code: "ENOENT" });

    const targetPath = `${fixture.socketPath}.target`;
    const targetServer = net.createServer();
    try {
      await new Promise((resolve, reject) => {
        targetServer.once("error", reject);
        targetServer.listen(targetPath, resolve);
      });
      await chmod(targetPath, 0o600);
      await symlink(targetPath, fixture.socketPath);
      await expect(validateTmuxProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "unsafe_socket" });
    } finally {
      if (targetServer.listening) await new Promise(resolve => targetServer.close(resolve));
    }
  } finally {
    if (abandonedSocketOwner && abandonedSocketOwner.exitCode === null) {
      abandonedSocketOwner.kill("SIGKILL");
      await once(abandonedSocketOwner, "exit");
    }
    if (fixture.socketServer.listening) await new Promise(resolve => fixture.socketServer.close(resolve));
    await rm(fixture.sandbox, { recursive: true, force: true });
  }
});

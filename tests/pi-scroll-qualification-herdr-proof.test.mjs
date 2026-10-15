import { expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { chmod, lstat, mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { allocateAttempt, acquireStageLock, initializeStage, markRootCreated } from "./pi-scroll-qualification-ledger.mjs";
import { validateHerdrSocketProof } from "./pi-scroll-qualification-herdr-proof.mjs";

const driverPath = fileURLToPath(new URL("./pi-scroll-qualification.sh", import.meta.url));
const allowedLauncherCommands = ["bash tests/pi-scroll-qualification.sh", `bash ${driverPath}`];
const whitespaceLauncherCommands = ["  bash   tests/pi-scroll-qualification.sh  ", `  bash   ${driverPath}  `];
const rejectedLauncherCommands = [
  "bash other-driver.sh",
  `bash ${join(dirname(driverPath), "other", basename(driverPath))}`,
  `bash ${driverPath}.bak`,
  `bash ${join(dirname(driverPath), `not-${basename(driverPath)}`)}`,
  `bash ${driverPath} --other`,
  `bash --norc ${driverPath}`,
  `/bin/bash ${driverPath}`,
  `sh ${driverPath}`,
  "bash tests/pi-scroll-qualification.sh --other",
  "bash ./tests/pi-scroll-qualification.sh",
];

async function listen(path) {
  expect(Buffer.byteLength(path)).toBeLessThan(104);
  const server = net.createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(path, resolve);
  });
  await chmod(path, 0o600);
  return server;
}

async function close(server) {
  if (server?.listening) await new Promise(resolve => server.close(resolve));
}

async function writeProcessLedger(root, launcher, server) {
  await writeFile(join(root, "owned-processes.tsv"), [
    "role\tpid\tstart_identity\tcommand_identity",
    `herdr-launcher\t${launcher.pid}\t${launcher.startIdentity}\t${launcher.commandIdentity}`,
    `herdr-server\t${server.pid}\t${server.startIdentity}\t${server.commandIdentity}`,
  ].join("\n") + "\n", { mode: 0o600 });
}

async function launcherCommandCase(fixture, commandIdentity) {
  const launcher = { ...fixture.input.launcher, commandIdentity };
  const input = { ...fixture.input, launcher };
  const observed = { ...fixture.observed, launcher: { ...launcher } };
  await writeProcessLedger(fixture.root, launcher, input.server);
  return { input, observed };
}

async function createFixture() {
  const sandbox = await realpath(await mkdtemp(join(tmpdir(), "hp-")));
  const stageId = "h";
  const runId = "herdr-socket-proof-run";
  const stageRoot = join(sandbox, "stage");
  const fixtureRootParent = join(sandbox, "f");
  await mkdir(fixtureRootParent, { mode: 0o700 });
  await initializeStage(stageRoot, { stageId, runId, fixtureRootParent });
  const lockToken = "herdr-socket-proof-lock";
  await acquireStageLock(stageRoot, { token: lockToken, runId, shellPid: process.pid, shellStart: "test-only", shellCommand: "bun test" });
  const allocation = await allocateAttempt(stageRoot, { token: lockToken, requestedId: "attempt-001" });
  const root = allocation.root_path;
  const uid = process.getuid();
  const configRoot = join(root, "config");
  const herdrConfigRoot = join(configRoot, "herdr");
  const socketPaths = [join(herdrConfigRoot, "herdr.sock"), join(herdrConfigRoot, "herdr-client.sock")];
  await mkdir(herdrConfigRoot, { recursive: true, mode: 0o700 });
  await mkdir(join(root, "logs"), { mode: 0o700 });
  await writeFile(join(root, "fixture-owner"), "disposable-scroll-fixture\n", { mode: 0o600 });
  await writeFile(join(root, ".stage-owner.json"), `${JSON.stringify({ stage_id: stageId, run_id: runId, attempt_id: allocation.attempt_id, root_path: root, uid })}\n`, { mode: 0o600 });
  const configPath = join(herdrConfigRoot, "config.toml");
  await writeFile(configPath, "[update]\nversion_check = false\nmanifest_check = false\n", { mode: 0o600 });
  await markRootCreated(stageRoot, { token: lockToken, attemptId: allocation.attempt_id });
  const herdrBin = process.execPath;
  const launcher = {
    pid: 910002,
    startIdentity: "Wed 7 Oct 00:28:18 2026",
    commandIdentity: "bash tests/pi-scroll-qualification.sh",
    parentPid: 1,
    uid,
  };
  const server = {
    pid: 910001,
    startIdentity: "Wed Oct 7 00:28:19 2026",
    commandIdentity: `${herdrBin} server`,
    parentPid: launcher.pid,
    uid,
  };
  await writeProcessLedger(root, launcher, server);
  const servers = await Promise.all(socketPaths.map(listen));
  const socketStats = await Promise.all(socketPaths.map(path => lstat(path)));
  const input = { stageRoot, root, stageId, runId, attemptId: allocation.attempt_id, uid, configPath, herdrBin, lsofBin: "/usr/sbin/lsof", socketPaths, lockToken, launcher, server };
  const observed = {
    launcher: { ...launcher },
    server: { ...server },
    sockets: socketPaths.map((path, index) => ({ path, uid, mode: 0o600, device: socketStats[index].dev, inode: socketStats[index].ino, listenerPids: [String(server.pid)] })),
  };
  return { sandbox, root, herdrConfigRoot, socketPaths, servers, input, observed };
}

test("Herdr proof accepts only its two private sockets owned by one verified server and launcher", async () => {
  const fixture = await createFixture();
  let extraServer;
  const [apiSocket, clientSocket] = fixture.socketPaths;
  try {
    const proof = await validateHerdrSocketProof(fixture.input, fixture.observed);
    const socketStats = await Promise.all(fixture.socketPaths.map(path => lstat(path)));
    expect(proof.verified).toBe(true);
    expect(proof.sockets).toEqual(fixture.socketPaths.map((path, index) => ({ path, uid: fixture.input.uid, mode: 0o600, device: socketStats[index].dev, inode: socketStats[index].ino, listener_pids: [String(fixture.input.server.pid)] })));

    await expect(validateHerdrSocketProof({ ...fixture.input, socketPaths: [join(fixture.herdrConfigRoot, "other.sock"), clientSocket] }, fixture.observed)).rejects.toMatchObject({ code: "wrong_socket_path" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, sockets: fixture.observed.sockets.slice(1) })).rejects.toMatchObject({ code: "herdr_socket_listener_count" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, sockets: fixture.observed.sockets.map((socket, index) => index === 1 ? { ...socket, uid: fixture.input.uid + 1 } : socket) })).rejects.toMatchObject({ code: "herdr_socket_listener_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, sockets: fixture.observed.sockets.map((socket, index) => index === 1 ? { ...socket, mode: 0o660 } : socket) })).rejects.toMatchObject({ code: "herdr_socket_listener_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, sockets: fixture.observed.sockets.map((socket, index) => index === 1 ? { ...socket, listenerPids: ["910099"] } : socket) })).rejects.toMatchObject({ code: "herdr_socket_listener_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, pid: fixture.input.server.pid + 1 } })).rejects.toMatchObject({ code: "server_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, startIdentity: "Thu 8 Oct 00:28:19 2026" } })).rejects.toMatchObject({ code: "server_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, commandIdentity: `${fixture.observed.server.commandIdentity} --other` } })).rejects.toMatchObject({ code: "server_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, parentPid: 910099 } })).rejects.toMatchObject({ code: "server_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, uid: fixture.input.uid + 1 } })).rejects.toMatchObject({ code: "server_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, launcher: { ...fixture.observed.launcher, pid: fixture.input.launcher.pid + 1 } })).rejects.toMatchObject({ code: "launcher_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, launcher: { ...fixture.observed.launcher, commandIdentity: "bash other-driver.sh" } })).rejects.toMatchObject({ code: "launcher_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, launcher: { ...fixture.observed.launcher, uid: fixture.input.uid + 1 } })).rejects.toMatchObject({ code: "launcher_identity" });
    await expect(validateHerdrSocketProof({ ...fixture.input, server: { ...fixture.input.server, startIdentity: "Wed 32 Oct 00:28:19 2026" } }, fixture.observed)).rejects.toMatchObject({ code: "process_ledger_mismatch" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, server: { ...fixture.observed.server, startIdentity: "Wed 32 Oct 00:28:19 2026" } })).rejects.toMatchObject({ code: "invalid_process_start_identity" });
    await expect(validateHerdrSocketProof(fixture.input, { ...fixture.observed, launcher: { ...fixture.observed.launcher, startIdentity: "Wed Oct 7 00:28:18 2026 trailing" } })).rejects.toMatchObject({ code: "invalid_process_start_identity" });

    await chmod(apiSocket, 0o660);
    await expect(validateHerdrSocketProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "unsafe_herdr_socket" });
    await chmod(apiSocket, 0o600);
    await chmod(fixture.herdrConfigRoot, 0o755);
    await expect(validateHerdrSocketProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "unsafe_directory" });
    await chmod(fixture.herdrConfigRoot, 0o700);

    extraServer = await listen(join(fixture.herdrConfigRoot, "extra.sock"));
    await expect(validateHerdrSocketProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "herdr_socket_set" });
    await close(extraServer);
    extraServer = null;

    await close(fixture.servers[0]);
    await expect(validateHerdrSocketProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "herdr_socket_set" });
    await symlink(clientSocket, apiSocket);
    await expect(validateHerdrSocketProof(fixture.input, fixture.observed)).rejects.toMatchObject({ code: "herdr_socket_symlink" });
  } finally {
    await close(extraServer);
    for (const server of fixture.servers) await close(server);
    await rm(fixture.sandbox, { recursive: true, force: true });
  }
});

test("Herdr launcher commands use the actual neighboring driver whitelist and exact stored identities", async () => {
  const fixture = await createFixture();
  try {
    for (const commandIdentity of [...allowedLauncherCommands, ...whitespaceLauncherCommands]) {
      const { input, observed } = await launcherCommandCase(fixture, commandIdentity);
      const proof = await validateHerdrSocketProof(input, observed);
      expect(proof.verified).toBe(true);
      expect(proof.launcher.commandIdentity).toBe(commandIdentity);
    }
    for (const commandIdentity of rejectedLauncherCommands) {
      const { input, observed } = await launcherCommandCase(fixture, commandIdentity);
      await expect(validateHerdrSocketProof(input, observed)).rejects.toMatchObject({ code: "herdr_command_identity" });
    }

    const { input, observed } = await launcherCommandCase(fixture, allowedLauncherCommands[1]);
    for (const commandIdentity of [allowedLauncherCommands[0], whitespaceLauncherCommands[1]]) {
      await expect(validateHerdrSocketProof({ ...input, launcher: { ...input.launcher, commandIdentity } }, observed)).rejects.toMatchObject({ code: "process_ledger_mismatch" });
      await expect(validateHerdrSocketProof(input, { ...observed, launcher: { ...observed.launcher, commandIdentity } })).rejects.toMatchObject({ code: "launcher_identity" });
    }
    const startIdentity = "Wed Oct 7 00:28:18 2026";
    await expect(validateHerdrSocketProof({ ...input, launcher: { ...input.launcher, startIdentity } }, observed)).rejects.toMatchObject({ code: "process_ledger_mismatch" });
    await expect(validateHerdrSocketProof(input, { ...observed, launcher: { ...observed.launcher, startIdentity } })).rejects.toMatchObject({ code: "launcher_identity" });
  } finally {
    for (const server of fixture.servers) await close(server);
    await rm(fixture.sandbox, { recursive: true, force: true });
  }
});

test("the source-extracted launcher shell guard accepts only exact commands and complete synthetic identities", async () => {
  const driver = await readFile(driverPath, "utf8");
  const matches = [...driver.matchAll(/^if (\[\[ [^\n]*"\$HERDR_COMMAND"[^\n]* \]\]); then\n    FAILURE_REASON="Herdr fixture launcher identity was incomplete or unexpected"\n    exit 1\nfi$/gm)];
  expect(matches).toHaveLength(1);
  const [guard, condition] = matches[0];
  expect(condition).not.toMatch(/[`\\;<>]|\$\(|\$\{|\$\[/);
  expect(condition.slice(2, -2)).not.toMatch(/\[\[|\]\]/);
  expect([...condition.matchAll(/\$([A-Za-z_]\w*)/g)].map(([, name]) => name).sort()).toEqual([
    "HERDR_COMMAND", "HERDR_COMMAND", "HERDR_LAUNCHER_PARENT", "HERDR_PID", "HERDR_START", "SCRIPT_DIR",
  ]);
  const script = `set -euo pipefail\n${guard}\nbuiltin printf '%s\\n' accepted\n`;
  const cases = [
    ...allowedLauncherCommands.map(HERDR_COMMAND => [{ HERDR_COMMAND }, 0]),
    ...whitespaceLauncherCommands.map(HERDR_COMMAND => [{ HERDR_COMMAND }, 1]),
    ...rejectedLauncherCommands.map(HERDR_COMMAND => [{ HERDR_COMMAND }, 1]),
    [{ HERDR_COMMAND: allowedLauncherCommands[0], HERDR_PID: "unrecorded" }, 1],
    [{ HERDR_COMMAND: allowedLauncherCommands[0], HERDR_LAUNCHER_PARENT: "unrecorded" }, 1],
    [{ HERDR_COMMAND: allowedLauncherCommands[0], HERDR_START: "" }, 1],
  ];
  for (const [variables, exitCode] of cases) {
    const result = spawnSync("/bin/bash", ["--noprofile", "--norc", "-c", script], {
      cwd: process.env.HOME,
      env: {
        PATH: "/nonexistent",
        HOME: process.env.HOME,
        XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME,
        XDG_DATA_HOME: process.env.XDG_DATA_HOME,
        XDG_STATE_HOME: process.env.XDG_STATE_HOME,
        XDG_RUNTIME_DIR: process.env.XDG_RUNTIME_DIR,
        XDG_CACHE_HOME: process.env.XDG_CACHE_HOME,
        TMPDIR: process.env.TMPDIR,
        LC_ALL: "C",
        LANG: "C",
        HERDR_PID: "910002",
        HERDR_LAUNCHER_PARENT: "1",
        HERDR_START: "Wed 7 Oct 00:28:18 2026",
        SCRIPT_DIR: dirname(driverPath),
        ...variables,
      },
      encoding: "utf8",
      timeout: 2000,
      maxBuffer: 4096,
    });
    expect(result.error).toBeUndefined();
    expect(result.signal).toBeNull();
    expect(result.status).toBe(exitCode);
    expect(result.stdout).toBe(exitCode === 0 ? "accepted\n" : "");
    expect(result.stderr).toBe("");
  }
});

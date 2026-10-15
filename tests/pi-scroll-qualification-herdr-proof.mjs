import { lstat, readFile, readdir, realpath } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { basename, dirname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export class HerdrProofError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "HerdrProofError";
    this.code = code;
  }
}

function fail(code, message) {
  throw new HerdrProofError(code, message);
}

async function lstatOrNull(path) {
  try {
    return await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
}

function assertFile(stat, path, uid) {
  if (!stat || stat.isSymbolicLink() || !stat.isFile() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600) {
    fail("unsafe_file", `unsafe or unowned Herdr proof file: ${path}`);
  }
}

function assertDirectory(stat, path, uid) {
  if (!stat || stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== uid || (stat.mode & 0o777) !== 0o700) {
    fail("unsafe_directory", `unsafe or unowned Herdr proof directory: ${path}`);
  }
}

async function assertCanonicalPath(path) {
  if (resolve(path) !== path || await realpath(path) !== path) fail("noncanonical_path", `Herdr proof path is not canonical: ${path}`);
  const resolved = resolve(path);
  const root = resolved.slice(0, resolved.indexOf(sep, 1) + 1) || sep;
  let cursor = root;
  for (const component of resolved.slice(root.length).split(sep).filter(Boolean)) {
    cursor = join(cursor, component);
    const stat = await lstatOrNull(cursor);
    if (!stat || stat.isSymbolicLink()) fail("symlink_path", `Herdr proof path contains a missing or symbolic component: ${cursor}`);
  }
}

async function readPrivateJson(path, uid) {
  const stat = await lstatOrNull(path);
  assertFile(stat, path, uid);
  try {
    return JSON.parse(await readFile(path, "utf8"));
  } catch {
    fail("invalid_json", `invalid Herdr proof metadata: ${path}`);
  }
}

function normalizeCommand(command) {
  return typeof command === "string" ? command.trim().replace(/\s+/g, " ") : "";
}

function normalizeProcessStart(identity) {
  const parts = typeof identity === "string" ? identity.trim().split(/\s+/) : [];
  const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const monthNumbers = new Map(["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"].map((month, index) => [month, index + 1]));
  if (parts.length !== 5 || !weekdays.includes(parts[0])) fail("invalid_process_start_identity", "Herdr process start identity must use one complete five-field ps layout");
  let month;
  let day;
  let time;
  let year;
  if (monthNumbers.has(parts[1])) [month, day, time, year] = [monthNumbers.get(parts[1]), parts[2], parts[3], parts[4]];
  else if (/^\d{1,2}$/.test(parts[1]) && monthNumbers.has(parts[2])) [month, day, time, year] = [monthNumbers.get(parts[2]), parts[1], parts[3], parts[4]];
  else fail("invalid_process_start_identity", "Herdr process start identity uses an unrecognized ps layout");
  if (!/^\d{1,2}$/.test(day) || !/^\d{2}:\d{2}:\d{2}$/.test(time) || !/^\d{4}$/.test(year)) fail("invalid_process_start_identity", "Herdr process start identity contains malformed date fields");
  const numericDay = Number(day);
  const numericYear = Number(year);
  const [hours, minutes, seconds] = time.split(":").map(Number);
  const date = new Date(Date.UTC(numericYear, month - 1, numericDay));
  if (numericYear < 1000 || numericDay < 1 || date.getUTCFullYear() !== numericYear || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== numericDay || hours > 23 || minutes > 59 || seconds > 59 || weekdays[date.getUTCDay()] !== parts[0]) {
    fail("invalid_process_start_identity", "Herdr process start identity contains an invalid date or time");
  }
  return `${year}-${String(month).padStart(2, "0")}-${String(numericDay).padStart(2, "0")} ${time}`;
}

function runProcess(command, args) {
  const result = spawnSync(command, args, { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  if (result.error) fail("proof_probe_failed", `could not run ${command}: ${result.error.message}`);
  return result;
}

function processSnapshot(pid) {
  const startResult = runProcess("/bin/ps", ["-p", String(pid), "-o", "lstart="]);
  if (startResult.status === 1 && !startResult.stdout.trim() && !startResult.stderr.trim()) return null;
  if (startResult.status !== 0 || !startResult.stdout.trim()) fail("proof_probe_failed", `could not establish the start identity of process ${pid}`);
  const commandResult = runProcess("/bin/ps", ["-p", String(pid), "-o", "command="]);
  const parentResult = runProcess("/bin/ps", ["-p", String(pid), "-o", "ppid="]);
  const uidResult = runProcess("/bin/ps", ["-p", String(pid), "-o", "uid="]);
  if (commandResult.status !== 0 || parentResult.status !== 0 || uidResult.status !== 0) fail("proof_probe_failed", `could not inspect live process ${pid}`);
  return {
    pid,
    startIdentity: normalizeCommand(startResult.stdout),
    commandIdentity: normalizeCommand(commandResult.stdout),
    parentPid: Number(parentResult.stdout.trim()),
    uid: Number(uidResult.stdout.trim()),
  };
}

function socketOwners(lsofBin, socketPath) {
  const result = runProcess(lsofBin, ["-n", "-t", socketPath]);
  const values = result.stdout.split(/\s+/).filter(Boolean);
  if (result.status !== 0 || result.stderr.trim() || values.length !== 1 || !/^\d+$/.test(values[0])) {
    fail("lsof_probe_failed", `could not prove exactly one Herdr socket listener at ${socketPath}: ${result.stderr.trim()}`);
  }
  return values;
}

function parseProcessLedger(text) {
  const lines = text.trimEnd().split("\n");
  if (lines[0] !== "role\tpid\tstart_identity\tcommand_identity") fail("process_ledger_header", "owned-processes.tsv header is invalid");
  const byRole = new Map();
  for (const line of lines.slice(1)) {
    const fields = line.split("\t");
    if (fields.length !== 4 || !fields[0] || !/^\d+$/.test(fields[1]) || !fields[2] || !fields[3]) fail("process_ledger_record", "owned-processes.tsv contains an invalid process record");
    const records = byRole.get(fields[0]) ?? [];
    records.push({ pid: Number(fields[1]), startIdentity: fields[2], commandIdentity: fields[3] });
    byRole.set(fields[0], records);
  }
  return byRole;
}

function exactRecord(records, role, expected) {
  const matches = records.get(role) ?? [];
  if (matches.length !== 1 || matches[0].pid !== expected.pid || matches[0].startIdentity !== expected.startIdentity || matches[0].commandIdentity !== expected.commandIdentity) {
    fail("process_ledger_mismatch", `owned process record does not match current ${role} identity`);
  }
}

function validateProcessPair(input, observed) {
  const { server, launcher } = input;
  if (!server || !launcher || !observed?.server || !observed?.launcher || !Number.isSafeInteger(server.pid) || server.pid < 1 || !Number.isSafeInteger(launcher.pid) || launcher.pid < 1 || server.pid === launcher.pid || !Number.isSafeInteger(server.parentPid) || server.parentPid !== launcher.pid) {
    fail("invalid_process_proof", "Herdr server and its launch process identities are incomplete");
  }
  const serverCommand = `${input.herdrBin} server`;
  const launcherCommands = ["bash tests/pi-scroll-qualification.sh", `bash ${fileURLToPath(new URL("./pi-scroll-qualification.sh", import.meta.url))}`];
  if (normalizeCommand(server.commandIdentity) !== serverCommand || !launcherCommands.includes(normalizeCommand(launcher.commandIdentity))) {
    fail("herdr_command_identity", "Herdr server or fixture launcher command is not the expected isolated command");
  }
  for (const [name, expected, current] of [["server", server, observed.server], ["launcher", launcher, observed.launcher]]) {
    if (!current || current.pid !== expected.pid || current.uid !== input.uid || current.commandIdentity !== expected.commandIdentity || current.parentPid !== expected.parentPid || normalizeProcessStart(current.startIdentity) !== normalizeProcessStart(expected.startIdentity) || current.startIdentity !== expected.startIdentity) {
      fail(`${name}_identity`, `Herdr ${name} PID, start identity, command, owner, or ancestry changed`);
    }
  }
  if (observed.server.parentPid !== observed.launcher.pid || observed.server.uid !== observed.launcher.uid || observed.launcher.uid !== input.uid) {
    fail("herdr_launch_ancestry", "Herdr server is not a child of the verified fixture launcher");
  }
}

async function inspectConfigTree(input, { allowNoSockets = false } = {}) {
  const { stageRoot, root, stageId, runId, attemptId, uid, configPath, herdrBin, lsofBin, socketPaths, lockToken } = input;
  if (![stageRoot, root, configPath, herdrBin, lsofBin].every(value => typeof value === "string" && value.length > 0) || !Array.isArray(socketPaths) || !Number.isSafeInteger(uid) || uid < 0) {
    fail("invalid_proof_input", "Herdr proof paths or owner identity are invalid");
  }
  if (await realpath(herdrBin) !== herdrBin || await realpath(lsofBin) !== lsofBin) fail("invalid_proof_tool", "Herdr or lsof executable path is not canonical");
  for (const tool of [herdrBin, lsofBin]) {
    const stat = await lstatOrNull(tool);
    if (!stat || stat.isSymbolicLink() || !stat.isFile()) fail("invalid_proof_tool", `Herdr proof tool is not a regular file: ${tool}`);
  }
  await assertCanonicalPath(stageRoot);
  await assertCanonicalPath(root);
  const stageOwner = await readPrivateJson(join(stageRoot, "stage-owner.json"), uid);
  if (stageOwner.stage_root !== stageRoot || stageOwner.stage_id !== stageId || stageOwner.run_id !== runId || stageOwner.owner_uid !== uid || stageOwner.max_attempts !== 6 || stageOwner.fixture_root_prefix !== `herdr-scroll-p1-${stageId}-` || dirname(root) !== stageOwner.fixture_root_parent) {
    fail("stage_identity_mismatch", "Herdr proof stage marker does not match the attempt namespace");
  }
  const number = Number(attemptId?.match(/^attempt-(\d{3})$/)?.[1]);
  if (!Number.isSafeInteger(number) || number < 1 || number > stageOwner.max_attempts || basename(root) !== `${stageOwner.fixture_root_prefix}${attemptId}`) {
    fail("attempt_identity_mismatch", "Herdr proof root is outside the allocated stage namespace");
  }
  const attemptPath = join(stageRoot, "attempts", attemptId);
  const configRoot = join(root, "config");
  const herdrConfigRoot = join(configRoot, "herdr");
  const expectedConfigPath = join(herdrConfigRoot, "config.toml");
  const lockPath = join(stageRoot, "active.lock");
  for (const path of [stageRoot, join(stageRoot, "attempts"), attemptPath, lockPath, root, configRoot, herdrConfigRoot]) {
    await assertCanonicalPath(path);
    assertDirectory(await lstatOrNull(path), path, uid);
  }
  if (configPath !== expectedConfigPath) fail("wrong_config_path", "Herdr config path is not the exact attempt-private config");
  const attemptOwner = await readPrivateJson(join(attemptPath, "attempt-owner.json"), uid);
  const rootOwner = await readPrivateJson(join(root, ".stage-owner.json"), uid);
  const rootCreated = await readPrivateJson(join(attemptPath, "root-created.json"), uid);
  if (attemptOwner.stage_id !== stageId || attemptOwner.run_id !== runId || attemptOwner.attempt_id !== attemptId || attemptOwner.root_path !== root || rootOwner.stage_id !== stageId || rootOwner.run_id !== runId || rootOwner.attempt_id !== attemptId || rootOwner.root_path !== root || rootOwner.uid !== uid || rootCreated.stage_id !== stageId || rootCreated.run_id !== runId || rootCreated.attempt_id !== attemptId || rootCreated.root_path !== root) {
    fail("attempt_owner_mismatch", "Herdr attempt allocation, root marker, and stage identity do not agree");
  }
  const markerPath = join(root, "fixture-owner");
  assertFile(await lstatOrNull(markerPath), markerPath, uid);
  if (await readFile(markerPath, "utf8") !== "disposable-scroll-fixture\n") fail("fixture_marker_mismatch", "Herdr fixture root marker is invalid");
  assertFile(await lstatOrNull(configPath), configPath, uid);
  const lockOwner = await readPrivateJson(join(lockPath, "owner.json"), uid);
  if (lockOwner.token !== lockToken || lockOwner.run_id !== runId) fail("lock_owner_mismatch", "active stage lock does not belong to this Herdr proof invocation");
  const ledgerPath = join(stageRoot, "allocation-ledger.jsonl");
  assertFile(await lstatOrNull(ledgerPath), ledgerPath, uid);
  const allocations = (await readFile(ledgerPath, "utf8")).trimEnd().split("\n").filter(Boolean).map(line => JSON.parse(line)).filter(record => record.kind === "allocation" && record.attempt_id === attemptId);
  if (allocations.length !== 1 || allocations[0].stage_id !== stageId || allocations[0].run_id !== runId || allocations[0].attempt_number !== number || allocations[0].root_path !== root) {
    fail("allocation_mismatch", "Herdr attempt root is not backed by its unique stage allocation");
  }
  const processLedgerPath = join(root, "owned-processes.tsv");
  assertFile(await lstatOrNull(processLedgerPath), processLedgerPath, uid);
  const processLedger = parseProcessLedger(await readFile(processLedgerPath, "utf8"));
  const expectedPaths = [join(herdrConfigRoot, "herdr.sock"), join(herdrConfigRoot, "herdr-client.sock")];
  if (socketPaths.length !== 2 || new Set(socketPaths).size !== 2 || socketPaths.some(path => !expectedPaths.includes(path)) || expectedPaths.some(path => !socketPaths.includes(path))) {
    fail("wrong_socket_path", "Herdr proof must use exactly the two fixed API and client socket paths");
  }
  for (const path of expectedPaths) {
    if (resolve(path) !== path) fail("noncanonical_path", `Herdr socket path is not canonical: ${path}`);
  }
  const foundSockets = [];
  async function walk(directory) {
    const entries = await readdir(directory);
    for (const entry of entries) {
      const path = join(directory, entry);
      const stat = await lstatOrNull(path);
      if (!stat) fail("config_tree_changed", `Herdr config entry disappeared during proof: ${path}`);
      if (stat.isSymbolicLink()) fail("herdr_socket_symlink", `Herdr config contains a symlink: ${path}`);
      if (stat.isSocket()) foundSockets.push(path);
      if (stat.isDirectory()) await walk(path);
    }
  }
  await walk(configRoot);
  const sortedFound = foundSockets.toSorted();
  const sortedExpected = expectedPaths.toSorted();
  if (allowNoSockets) {
    if (sortedFound.length !== 0) fail("herdr_socket_node_present", "Herdr sockets remain after the isolated server stop");
  } else if (JSON.stringify(sortedFound) !== JSON.stringify(sortedExpected)) {
    fail("herdr_socket_set", "Herdr config must contain exactly the expected API and client sockets");
  }
  const socketStats = new Map();
  for (const path of expectedPaths) {
    const stat = await lstatOrNull(path);
    if (!stat) {
      if (allowNoSockets) continue;
      fail("missing_herdr_socket", `expected Herdr socket is missing: ${path}`);
    }
    if (stat.isSymbolicLink() || !stat.isSocket() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600) {
      fail("unsafe_herdr_socket", `Herdr socket type, owner, or mode is unsafe: ${path}`);
    }
    socketStats.set(path, stat);
  }
  if (processLedger.get("herdr-launcher")?.length !== 1) fail("launcher_ledger_mismatch", "Herdr fixture launcher must have exactly one owned process record");
  return { configRoot, herdrConfigRoot, expectedPaths, socketStats, processLedger };
}

function validateProcessRecords(input, fixture, { allowMissingServer = false } = {}) {
  exactRecord(fixture.processLedger, "herdr-launcher", input.launcher);
  if (allowMissingServer) {
    if ((fixture.processLedger.get("herdr-server") ?? []).length !== 0) fail("process_ledger_mismatch", "Herdr server discovery found an unexpected prior server record");
    return;
  }
  exactRecord(fixture.processLedger, "herdr-server", input.server);
}

export async function validateHerdrSocketProof(input, observed, { allowMissingServerRecord = false } = {}) {
  const fixture = await inspectConfigTree(input);
  validateProcessRecords(input, fixture, { allowMissingServer: allowMissingServerRecord });
  validateProcessPair(input, observed);
  const sockets = observed.sockets;
  if (!Array.isArray(sockets) || sockets.length !== 2) fail("herdr_socket_listener_count", "Herdr proof must contain exactly two socket listener observations");
  const byPath = new Map();
  for (const socket of sockets) {
    if (!socket || typeof socket.path !== "string" || byPath.has(socket.path)) fail("herdr_socket_set", "Herdr listener observations contain an invalid or duplicate socket path");
    byPath.set(socket.path, socket);
  }
  if (byPath.size !== 2 || fixture.expectedPaths.some(path => !byPath.has(path))) fail("herdr_socket_set", "Herdr listener observations do not match the two exact socket paths");
  for (const path of fixture.expectedPaths) {
    const socket = byPath.get(path);
    const stat = fixture.socketStats.get(path);
    if (!stat || socket.uid !== input.uid || socket.mode !== 0o600 || socket.device !== stat.dev || socket.inode !== stat.ino || socket.listenerPids?.length !== 1 || socket.listenerPids[0] !== String(input.server.pid)) {
      fail("herdr_socket_listener_identity", `Herdr socket owner, mode, or listener does not match the verified server: ${path}`);
    }
  }
  return {
    verified: true,
    config_root: fixture.configRoot,
    sockets: fixture.expectedPaths.map(path => {
      const stat = fixture.socketStats.get(path);
      return { path, uid: input.uid, mode: 0o600, device: stat.dev, inode: stat.ino, listener_pids: [String(input.server.pid)] };
    }),
    server: input.server,
    launcher: input.launcher,
  };
}

async function collectSocketListeners(input) {
  const fixture = await inspectConfigTree(input);
  const sockets = fixture.expectedPaths.map(path => {
    const ownerPids = socketOwners(input.lsofBin, path);
    const stat = fixture.socketStats.get(path);
    return { path, uid: stat.uid, mode: stat.mode & 0o777, device: stat.dev, inode: stat.ino, listenerPids: ownerPids };
  });
  const ownerPids = sockets.flatMap(socket => socket.listenerPids);
  if (ownerPids.some(pid => pid !== ownerPids[0]) || new Set(ownerPids).size !== 1) fail("herdr_socket_listener_mismatch", "Herdr API and client sockets do not share exactly one listener process");
  const server = processSnapshot(Number(ownerPids[0]));
  const launcher = processSnapshot(input.launcher.pid);
  if (!server || !launcher) fail("herdr_process_absent", "Herdr server or its fixture launcher is absent while sockets are present");
  return { server, launcher, sockets };
}

export async function discoverHerdrServer(input) {
  const observed = await collectSocketListeners(input);
  const proofInput = { ...input, server: observed.server };
  const result = await validateHerdrSocketProof(proofInput, observed, { allowMissingServerRecord: true });
  return result;
}

export async function verifyCurrentHerdrProof(input) {
  const observed = await collectSocketListeners(input);
  return await validateHerdrSocketProof(input, observed);
}

export async function verifyHerdrStopped(input) {
  const fixture = await inspectConfigTree(input, { allowNoSockets: true });
  validateProcessRecords(input, fixture);
  const server = processSnapshot(input.server.pid);
  const launcher = processSnapshot(input.launcher.pid);
  if (server || launcher) fail("herdr_process_still_live", "recorded Herdr server or launcher remains live after cleanup");
  return { stopped: true, config_root: fixture.configRoot, sockets_absent: fixture.expectedPaths, server_pid: input.server.pid, server_start_identity: input.server.startIdentity, launcher_pid: input.launcher.pid, launcher_start_identity: input.launcher.startIdentity };
}

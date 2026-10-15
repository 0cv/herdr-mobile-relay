import { lstat, readFile, realpath, unlink } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { basename, dirname, join, resolve, sep } from "node:path";

export class TmuxProofError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "TmuxProofError";
    this.code = code;
  }
}

function fail(code, message) {
  throw new TmuxProofError(code, message);
}

async function lstatOrNull(path) {
  try {
    return await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") return null;
    if (error.code !== "EOPNOTSUPP" && error.code !== "ENOTSUP") throw error;
    const testBin = process.platform === "darwin" ? "/bin/test" : "/usr/bin/test";
    const statArgs = process.platform === "darwin" ? ["-f", "%HT|%u|%Lp|%d|%i", path] : ["-c", "%F|%u|%a|%d|%i", path];
    const linkResult = runProcess(testBin, ["-L", path]);
    if (linkResult.status === 0) return { uid: -1, mode: 0, dev: -1, ino: -1, isSymbolicLink: () => true, isSocket: () => false, isDirectory: () => false, isFile: () => false, nativeFallback: true };
    if (linkResult.status !== 1 || linkResult.stderr.trim()) fail("socket_stat_probe_failed", `could not prove the stale tmux socket is not a symlink: ${path}`);
    const result = runProcess("/usr/bin/stat", statArgs);
    if (result.status !== 0 || result.stderr.trim()) fail("socket_stat_probe_failed", `could not inspect the stale tmux socket node: ${result.stderr.trim()}`);
    const fields = result.stdout.trim().split("|");
    if (fields.length !== 5) fail("socket_stat_probe_failed", `native socket inspection was incomplete: ${path}`);
    const [type, owner, mode, device, inode] = fields;
    const normalizedType = type.toLowerCase();
    const uid = Number(owner);
    const permissions = Number.parseInt(mode, 8);
    const dev = Number(device);
    const ino = Number(inode);
    if (!Number.isSafeInteger(uid) || !Number.isSafeInteger(permissions) || !Number.isSafeInteger(dev) || !Number.isSafeInteger(ino)) fail("socket_stat_probe_failed", `native socket identity was invalid: ${path}`);
    return {
      uid,
      mode: permissions,
      dev,
      ino,
      isSymbolicLink: () => false,
      isSocket: () => normalizedType === "socket",
      isDirectory: () => normalizedType === "directory",
      isFile: () => normalizedType === "regular file",
      nativeFallback: true,
    };
  }
}

function assertFile(stat, path, uid) {
  if (!stat || stat.isSymbolicLink() || !stat.isFile() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600) {
    fail("unsafe_file", `unsafe or unowned proof file: ${path}`);
  }
}

function assertDirectory(stat, path, uid) {
  if (!stat || stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== uid || (stat.mode & 0o777) !== 0o700) {
    fail("unsafe_directory", `unsafe or unowned proof directory: ${path}`);
  }
}

async function assertCanonicalPath(path) {
  if (resolve(path) !== path || await realpath(path) !== path) fail("noncanonical_path", `proof path is not canonical: ${path}`);
  const parsed = resolve(path);
  const root = parsed.slice(0, parsed.indexOf(sep, 1) + 1) || sep;
  let cursor = root;
  for (const component of parsed.slice(root.length).split(sep).filter(Boolean)) {
    cursor = join(cursor, component);
    const stat = await lstatOrNull(cursor);
    if (!stat || stat.isSymbolicLink()) fail("symlink_path", `proof path contains a missing or symbolic component: ${cursor}`);
  }
}

async function readPrivateJson(path, uid) {
  const stat = await lstatOrNull(path);
  assertFile(stat, path, uid);
  try {
    return JSON.parse(await readFile(path, "utf8"));
  } catch {
    fail("invalid_json", `invalid proof metadata: ${path}`);
  }
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

async function validateFixture(input, { requireSocket = true } = {}) {
  const { stageRoot, root, stageId, runId, attemptId, uid, socketPath, lockToken } = input;
  if (![stageRoot, root, socketPath].every(path => typeof path === "string" && path.length > 0) || !Number.isSafeInteger(uid) || uid < 0) fail("invalid_proof_input", "tmux proof paths or owner identity are invalid");
  await assertCanonicalPath(stageRoot);
  await assertCanonicalPath(root);
  if (resolve(stageRoot) !== stageRoot || resolve(root) !== root) fail("noncanonical_path", "stage and attempt paths must be canonical");

  const stageOwner = await readPrivateJson(join(stageRoot, "stage-owner.json"), uid);
  if (stageOwner.stage_root !== stageRoot || stageOwner.stage_id !== stageId || stageOwner.run_id !== runId || stageOwner.owner_uid !== uid || stageOwner.max_attempts !== 6 || stageOwner.fixture_root_prefix !== `herdr-scroll-p1-${stageId}-` || dirname(root) !== stageOwner.fixture_root_parent) {
    fail("stage_identity_mismatch", "stage owner metadata does not match the expected stage and run");
  }
  const attemptNumber = Number(attemptId?.match(/^attempt-(\d{3})$/)?.[1]);
  if (!Number.isSafeInteger(attemptNumber) || attemptNumber < 1 || attemptNumber > stageOwner.max_attempts || basename(root) !== `${stageOwner.fixture_root_prefix}${attemptId}`) {
    fail("attempt_identity_mismatch", "attempt root is outside the allocated stage namespace");
  }

  const attemptsPath = join(stageRoot, "attempts");
  const attemptPath = join(attemptsPath, attemptId);
  const rootTmuxPath = join(root, "tmux");
  const socketDirectory = join(rootTmuxPath, `tmux-${uid}`);
  for (const path of [stageRoot, attemptsPath, attemptPath, root, rootTmuxPath, socketDirectory]) {
    await assertCanonicalPath(path);
    assertDirectory(await lstatOrNull(path), path, uid);
  }
  const attemptOwner = await readPrivateJson(join(attemptPath, "attempt-owner.json"), uid);
  const rootOwner = await readPrivateJson(join(root, ".stage-owner.json"), uid);
  const rootCreated = await readPrivateJson(join(attemptPath, "root-created.json"), uid);
  if (attemptOwner.stage_id !== stageId || attemptOwner.run_id !== runId || attemptOwner.attempt_id !== attemptId || attemptOwner.root_path !== root || rootOwner.stage_id !== stageId || rootOwner.run_id !== runId || rootOwner.attempt_id !== attemptId || rootOwner.root_path !== root || rootOwner.uid !== uid || rootCreated.stage_id !== stageId || rootCreated.run_id !== runId || rootCreated.attempt_id !== attemptId || rootCreated.root_path !== root) {
    fail("attempt_owner_mismatch", "attempt allocation, root marker, and stage identity do not agree");
  }

  const markerPath = join(root, "fixture-owner");
  assertFile(await lstatOrNull(markerPath), markerPath, uid);
  if (await readFile(markerPath, "utf8") !== "disposable-scroll-fixture\n") fail("fixture_marker_mismatch", "fixture root marker is invalid");
  const tmuxConfig = join(root, "tmux.conf");
  assertFile(await lstatOrNull(tmuxConfig), tmuxConfig, uid);
  const lockPath = join(stageRoot, "active.lock");
  await assertCanonicalPath(lockPath);
  assertDirectory(await lstatOrNull(lockPath), lockPath, uid);
  const lockOwner = await readPrivateJson(join(lockPath, "owner.json"), uid);
  if (lockOwner.token !== lockToken || lockOwner.run_id !== runId) fail("lock_owner_mismatch", "active stage lock does not belong to this invocation");

  const ledgerPath = join(stageRoot, "allocation-ledger.jsonl");
  assertFile(await lstatOrNull(ledgerPath), ledgerPath, uid);
  const records = (await readFile(ledgerPath, "utf8")).trimEnd().split("\n").filter(Boolean).map(line => JSON.parse(line));
  const allocations = records.filter(record => record.kind === "allocation" && record.attempt_id === attemptId);
  if (allocations.length !== 1 || allocations[0].stage_id !== stageId || allocations[0].run_id !== runId || allocations[0].attempt_number !== attemptNumber || allocations[0].root_path !== root) fail("allocation_mismatch", "attempt root is not backed by its unique stage allocation");

  const processLedgerPath = join(root, "owned-processes.tsv");
  assertFile(await lstatOrNull(processLedgerPath), processLedgerPath, uid);
  const processLedger = parseProcessLedger(await readFile(processLedgerPath, "utf8"));
  const expectedSocket = join(socketDirectory, "default");
  if (socketPath !== expectedSocket) fail("wrong_socket_path", "tmux socket path does not match the attempt-private namespace");
  const socketStat = await lstatOrNull(socketPath);
  if (requireSocket) {
    if (!socketStat || socketStat.isSymbolicLink() || !socketStat.isSocket() || socketStat.uid !== uid || ![0o600, 0o660].includes(socketStat.mode & 0o777)) {
      fail("unsafe_socket", `tmux socket type, owner, or mode is unsafe: ${socketPath}`);
    }
  } else if (socketStat && (socketStat.isSymbolicLink() || !socketStat.isSocket() || socketStat.uid !== uid || ![0o600, 0o660].includes(socketStat.mode & 0o777))) {
    fail("unsafe_socket", `stale tmux socket type, owner, or mode is unsafe: ${socketPath}`);
  }
  return { processLedger, socketStat, socketPath, tmuxConfig };
}

function normalizeCommand(command) {
  return command.trim().replace(/\s+/g, " ");
}

function normalizeProcessStart(identity) {
  const parts = typeof identity === "string" ? identity.trim().split(/\s+/) : [];
  const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const monthNumbers = new Map(["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"].map((month, index) => [month, index + 1]));
  if (parts.length !== 5 || !weekdays.includes(parts[0])) fail("invalid_process_start_identity", "process start identity must use one complete five-field ps layout");
  let month;
  let day;
  let time;
  let year;
  if (monthNumbers.has(parts[1])) [month, day, time, year] = [monthNumbers.get(parts[1]), parts[2], parts[3], parts[4]];
  else if (/^\d{1,2}$/.test(parts[1]) && monthNumbers.has(parts[2])) [month, day, time, year] = [monthNumbers.get(parts[2]), parts[1], parts[3], parts[4]];
  else fail("invalid_process_start_identity", "process start identity uses an unrecognized ps layout");
  if (!/^\d{1,2}$/.test(day) || !/^\d{2}:\d{2}:\d{2}$/.test(time) || !/^\d{4}$/.test(year)) fail("invalid_process_start_identity", "process start identity contains malformed date fields");
  const numericDay = Number(day);
  const numericYear = Number(year);
  const [hours, minutes, seconds] = time.split(":").map(Number);
  const date = new Date(Date.UTC(numericYear, month - 1, numericDay));
  if (numericYear < 1000 || numericDay < 1 || date.getUTCFullYear() !== numericYear || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== numericDay || hours > 23 || minutes > 59 || seconds > 59 || weekdays[date.getUTCDay()] !== parts[0]) {
    fail("invalid_process_start_identity", "process start identity contains an invalid date or time");
  }
  return `${year}-${String(month).padStart(2, "0")}-${String(numericDay).padStart(2, "0")} ${time}`;
}

async function validateTools(input) {
  for (const [path, label] of [[input.tmuxBin, "tmux"], [input.lsofBin, "lsof"]]) {
    if (typeof path !== "string" || await realpath(path) !== path) fail("invalid_tmux_tool", `${label} executable path is not canonical`);
    const stat = await lstatOrNull(path);
    if (!stat || stat.isSymbolicLink() || !stat.isFile()) fail("invalid_tmux_tool", `${label} executable is not a regular file`);
  }
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

function socketOwners(lsofBin, socketPath, socketExists) {
  if (!socketExists) return [];
  const result = runProcess(lsofBin, ["-n", "-t", socketPath]);
  const values = result.stdout.split(/\s+/).filter(Boolean);
  const noListener = result.status === 1 && values.length === 0 && !result.stderr.trim();
  if (result.stderr.trim() || (result.status === 0 && values.length === 0) || (result.status !== 0 && !noListener)) fail("lsof_probe_failed", `could not verify the tmux socket listener: ${result.stderr.trim()}`);
  if (values.some(value => !/^\d+$/.test(value))) fail("lsof_probe_failed", "lsof returned an invalid tmux socket process identity");
  return [...new Set(values)];
}

function validateServerCommand(command, input) {
  return command.includes(input.tmuxBin) && command.includes(`-f ${join(input.root, "tmux.conf")}`) && command.includes("new-session") && command.includes(`-s ${basename(input.root)}`);
}

export async function discoverTmuxServer(input) {
  const fixture = await validateFixture(input);
  await validateTools(input);
  const ownerPids = socketOwners(input.lsofBin, fixture.socketPath, true);
  if (ownerPids.length !== 1) fail("tmux_server_count", "private socket did not identify exactly one tmux server");
  const serverPid = Number(ownerPids[0]);
  const server = processSnapshot(serverPid);
  if (!server || server.uid !== input.uid || !validateServerCommand(server.commandIdentity, input)) fail("tmux_server_identity", "private socket owner is not the expected isolated tmux server");
  const childList = runProcess("/bin/ps", ["-axo", "pid=,ppid="]);
  if (childList.status !== 0) fail("proof_probe_failed", "could not inspect tmux bootstrap ancestry");
  const children = childList.stdout.split("\n").map(line => line.trim().split(/\s+/)).filter(fields => fields.length === 2 && fields[1] === String(serverPid)).map(fields => Number(fields[0]));
  const childSnapshots = children.map(processSnapshot).filter(Boolean);
  if (childSnapshots.length !== 1 || childSnapshots[0].uid !== input.uid || childSnapshots[0].commandIdentity !== "/bin/sleep 86400" || childSnapshots[0].parentPid !== serverPid) {
    fail("tmux_bootstrap_identity", "tmux server does not have exactly one expected bootstrap child");
  }
  return { socket_path: fixture.socketPath, socket_mode: fixture.socketStat.mode & 0o777, socket_owner_pids: ownerPids, server, bootstrap: childSnapshots[0] };
}

export async function validateTmuxProof(input, observed) {
  const fixture = await validateFixture(input);
  if (!input.server || !input.bootstrap || !Array.isArray(observed.socketOwnerPids) || !observed.server || !observed.bootstrap || !Number.isSafeInteger(input.server.pid) || input.server.pid < 1 || !Number.isSafeInteger(input.bootstrap.pid) || input.bootstrap.pid < 1 || input.server.pid === input.bootstrap.pid || !input.server.startIdentity || !input.bootstrap.startIdentity) fail("invalid_proof_input", "tmux process proof is incomplete");
  if (!validateServerCommand(input.server.commandIdentity, input) || input.server.pid !== observed.server.pid || normalizeProcessStart(input.server.startIdentity) !== normalizeProcessStart(observed.server.startIdentity) || input.server.commandIdentity !== observed.server.commandIdentity || observed.server.uid !== input.uid) {
    fail("tmux_server_identity", "tmux server PID, start identity, or command changed");
  }
  if (input.bootstrap.pid !== observed.bootstrap.pid || normalizeProcessStart(input.bootstrap.startIdentity) !== normalizeProcessStart(observed.bootstrap.startIdentity) || input.bootstrap.commandIdentity !== observed.bootstrap.commandIdentity || input.bootstrap.parentPid !== observed.bootstrap.parentPid || input.bootstrap.parentPid !== input.server.pid || input.bootstrap.commandIdentity !== "/bin/sleep 86400" || observed.bootstrap.uid !== input.uid) {
    fail("tmux_bootstrap_identity", "tmux bootstrap process identity or parent changed");
  }
  const serverRecords = fixture.processLedger.get("tmux-server") ?? [];
  const bootstrapRecords = fixture.processLedger.get("tmux-bootstrap") ?? [];
  exactRecord(fixture.processLedger, "tmux-server", input.server);
  exactRecord(fixture.processLedger, "tmux-bootstrap", input.bootstrap);
  if (serverRecords.length !== 1 || bootstrapRecords.length !== 1) fail("process_ledger_mismatch", "tmux process ownership records are duplicated");
  if (observed.socketOwnerPids.length !== 1 || observed.socketOwnerPids[0] !== String(input.server.pid)) fail("tmux_server_count", "private socket does not identify exactly one matching tmux server");
  return { verified: true, socket_path: fixture.socketPath, socket_mode: fixture.socketStat.mode & 0o777, server_pid: input.server.pid, server_start_identity: input.server.startIdentity, bootstrap_pid: input.bootstrap.pid, bootstrap_start_identity: input.bootstrap.startIdentity };
}

export async function verifyCurrentTmuxProof(input) {
  await validateTools(input);
  const server = processSnapshot(input.server.pid);
  const bootstrap = processSnapshot(input.bootstrap.pid);
  const socketStat = await lstatOrNull(input.socketPath);
  const ownerPids = socketOwners(input.lsofBin, input.socketPath, Boolean(socketStat));
  return await validateTmuxProof(input, { server, bootstrap, socketOwnerPids: ownerPids });
}

export async function removeStaleTmuxSocket(input) {
  await validateTools(input);
  const fixture = await validateFixture(input, { requireSocket: false });
  exactRecord(fixture.processLedger, "tmux-server", input.server);
  exactRecord(fixture.processLedger, "tmux-bootstrap", input.bootstrap);
  if (!validateServerCommand(input.server.commandIdentity, input) || input.bootstrap.parentPid !== input.server.pid || input.bootstrap.commandIdentity !== "/bin/sleep 86400") {
    fail("tmux_cleanup_identity", "cleanup identities do not match the recorded tmux server and bootstrap relationship");
  }
  const server = processSnapshot(input.server.pid);
  const bootstrap = processSnapshot(input.bootstrap.pid);
  if (server || bootstrap) fail("tmux_process_still_live", "recorded tmux server or bootstrap process still exists during cleanup");
  const ownerPids = socketOwners(input.lsofBin, fixture.socketPath, Boolean(fixture.socketStat));
  if (ownerPids.length !== 0) fail("tmux_listener_still_live", "tmux socket still has a listener during cleanup");
  if (!fixture.socketStat) return { removed: false, socket_path: fixture.socketPath, socket_stat_fallback: false, removed_at_utc: new Date().toISOString() };
  const currentStat = await lstatOrNull(fixture.socketPath);
  if (!currentStat || !currentStat.isSocket() || currentStat.uid !== input.uid || currentStat.dev !== fixture.socketStat.dev || currentStat.ino !== fixture.socketStat.ino) {
    fail("socket_identity_changed", "stale tmux socket node changed before cleanup");
  }
  await unlink(fixture.socketPath);
  return { removed: true, socket_path: fixture.socketPath, socket_stat_fallback: Boolean(fixture.socketStat.nativeFallback), removed_at_utc: new Date().toISOString() };
}

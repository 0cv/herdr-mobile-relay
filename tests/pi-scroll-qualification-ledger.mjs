import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { constants } from "node:fs";
import { lstat, mkdir, open, readFile, readdir, realpath, rename, rmdir, unlink } from "node:fs/promises";
import { basename, dirname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { validateHistoricalDisposition, validateSuccessorPin } from "./pi-scroll-qualification-evidence.mjs";

const maxLedgerBytes = 65536;
const observationIds = new Set(["prior-global-prefix-refusal", "prior-fixture-test-skip"]);
const refusalReasons = new Set(["unknown-attempt-id", "duplicate-attempt-id", "attempt-limit", "root-path-collision", "attempt-directory-collision"]);
const outcomes = new Set(["qualified", "blocked", "uncertain"]);
const attempt003Anchor = {
  stageRoot: "/private/tmp/herdr-scroll-p1-e90d2905-20261006",
  proofHash: "8987c341e16e95875276f34c64f3ec82ad316c9073cb7e549add64e866328b41",
  ledgerPrefixHash: "b2bbded875b0a238c23c308d135b80578ed31ff386a04fd05ca4dc2b00bda173",
  oldLockHash: "5d5fe516b0b5a2ccec03c005af332dc366499d82b8930c913d2a4b1690092f59",
};
const attempt005Gate = {
  stageRoot: "/private/tmp/herdr-scroll-p1-e90d2905-20261006",
  ledgerRecords: 10,
  ledgerHash: "1fca7b6679d967013ec46a1c43503db3c8763a6e532637d8b227bbe1215a7825",
  receiptPath: "/private/tmp/herdr-p1-pi-qualification-20261006/latest/receipt.json",
  attempt004Evidence: {
    "owned-processes.tsv": "4f830085967c107a0e8265dff0207f485eba3c8a5fa6ed89ab59faada41f03d7",
    "logs/tmux-socket-cleanup.json": "45d1539081dc1421a18131dfbda50e0b1bb082ba0cff08a4d42ef4b1351c18e3",
    "logs/herdr-post-stop-proof.json": "ac8767444d9956a4816ed47496aef8ea6d3292bff022fe014a4bde0b716408e6",
    "logs/herdr-socket-proof.json": "ded5c1e574492470bd85701b271beff9215eb0216a97b65f6d7e7f2299e10572",
    "logs/tmux-socket-proof.json": "9c7a9b137cdd00c1f7033dbb70a5789fe4bdc643b2d7611a2e055e73868c8b9e",
  },
};
const final006Gate = {
  stageRoot: "/private/tmp/herdr-scroll-p1-e90d2905-20261006",
  postcheckPath: "/private/tmp/herdr-scroll-p1-e90d2905-20261006-attempt-005-recovery/postcheck.txt",
  ledgerRecords: 12,
  ledgerHash: "01217235ff7cd76967e645696ce8265ab5883650075cf1bb6f61411bb7e42ae3",
  postcheckHash: "5a4abf9f7ddc8b68aa14b9a5f465981cc50af124747bed71ca3feaa984da3c6d",
  evidenceFiles: {
    "precheck.txt": "bde077847b692b50581366e1bd299712c470d6ace5c02d6e5683ccffbd9e95eb",
    "archive-check.txt": "58ed4734a336126a4c8f923bca48c4f029ae0091a29f3a0163d2eece81b10a68",
  },
  attempt005ProcessesHash: "3cfc101c2d09a92dd2f8934cd8f4e56f86af4ed4970ba976b25e5e48eba54956",
  receiptPath: "/private/tmp/herdr-p1-pi-qualification-20261006/latest/receipt.json",
};

export class LedgerError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "LedgerError";
    this.code = code;
  }
}

function fail(code, message) {
  throw new LedgerError(code, message);
}

function attemptId(number) {
  return `attempt-${String(number).padStart(3, "0")}`;
}

function rootPath(owner, number) {
  return join(owner.fixture_root_parent, `${owner.fixture_root_prefix}${attemptId(number)}`);
}

async function lstatOrNull(path) {
  try {
    return await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
}

function assertDirectory(stat, path, uid, mode = 0o700) {
  if (!stat || stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== uid || (stat.mode & 0o777) !== mode) {
    fail("unsafe_directory", `unsafe or unowned directory: ${path}`);
  }
}

function assertRegularFile(stat, path, uid, mode = 0o600) {
  if (!stat || stat.isSymbolicLink() || !stat.isFile() || stat.uid !== uid || (stat.mode & 0o777) !== mode || stat.size > maxLedgerBytes) {
    fail("unsafe_file", `unsafe, unowned, or oversized file: ${path}`);
  }
}

async function readOwnedJson(path, uid) {
  const bytes = await readOwnedBytes(path, uid);
  try {
    return JSON.parse(bytes);
  } catch {
    fail("invalid_json", `invalid JSON record: ${path}`);
  }
}

async function readBoundedRegular(path, uid, modes) {
  await canonicalDirectory(dirname(path), uid);
  const before = await lstatOrNull(path);
  const acceptable = stat => stat && !stat.isSymbolicLink() && stat.isFile() && stat.uid === uid && modes.includes(stat.mode & 0o777) && stat.size <= maxLedgerBytes;
  if (!acceptable(before)) fail("unsafe_evidence", `unsafe or oversized owned file: ${path}`);
  const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = await file.stat();
    if (!acceptable(opened) || opened.dev !== before.dev || opened.ino !== before.ino) fail("unsafe_evidence", "owned file identity changed while opening");
    const buffer = Buffer.alloc(maxLedgerBytes+1);
    let length = 0;
    for (;;) {
      const { bytesRead } = await file.read(buffer, length, buffer.length-length, null);
      if (!bytesRead) break;
      length += bytesRead;
      if (length > maxLedgerBytes) fail("unsafe_evidence", "owned file grew beyond bounded read");
    }
    const after = await file.stat(), named = await lstatOrNull(path);
    if (!acceptable(named) || named.dev !== opened.dev || named.ino !== opened.ino || ["size", "mtimeMs", "ctimeMs"].some(key => opened[key] !== after[key] || after[key] !== named[key])) fail("unsafe_evidence", "owned file changed during bounded read");
    return buffer.subarray(0, length);
  } finally { await file.close(); }
}

async function readOwnedBytes(path, uid) {
  return readBoundedRegular(path, uid, [0o600]);
}

async function readPinnedEvidenceBytes(path, uid) {
  return readBoundedRegular(path, uid, [0o400]);
}

function digest(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function readJsonLines(path, uid) {
  const stat = await lstatOrNull(path);
  assertRegularFile(stat, path, uid);
  const content = await readFile(path, "utf8");
  if (content.length > 0 && !content.endsWith("\n")) fail("incomplete_ledger", `ledger has an incomplete final record: ${path}`);
  if (content.length === 0) return [];
  return content.slice(0, -1).split("\n").map((line, index) => {
    try {
      return JSON.parse(line);
    } catch {
      fail("invalid_ledger_record", `invalid ledger record ${index + 1}: ${path}`);
    }
  });
}

const cleanupRecoverySources = [
  "logs/tmux-recovery-inspection.txt",
  "logs/tmux-recovery-stop.log",
  "logs/tmux-recovery-result.txt",
  "logs/tmux-recovery.json",
  "owned-processes.tsv",
];

function processStartIdentity(pid) {
  const result = spawnSync("/bin/ps", ["-p", String(pid), "-o", "lstart="], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  if (result.error) fail("process_identity_unavailable", `could not inspect process ${pid}: ${result.error.message}`);
  return result.status === 0 ? result.stdout.trim().replace(/\s+/g, " ") : "";
}

function requireNoLiveIdentity(pid, startIdentity) {
  const current = processStartIdentity(pid);
  if (current === startIdentity) fail("owned_process_still_live", `recorded process remains live: ${pid}`);
  if (current) fail("process_identity_changed", `recorded PID now has a different start identity: ${pid}`);
}

function requireNoProcessCommand(target, uid) {
  const ancestors = new Set();
  let pid = process.pid;
  while (pid > 1 && !ancestors.has(pid)) {
    ancestors.add(pid);
    const parent = spawnSync("/bin/ps", ["-p", String(pid), "-o", "ppid="], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
    if (parent.error || parent.status !== 0 || !/^\s*\d+\s*$/.test(parent.stdout)) fail("process_scan_ambiguous", `could not inspect process ancestry for ${pid}`);
    const parentPid = Number(parent.stdout.trim());
    if (!Number.isSafeInteger(parentPid) || parentPid <= 1) break;
    pid = parentPid;
  }
  const scan = spawnSync("/bin/ps", ["-axo", "pid=,ppid=,uid=,command="], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  if (scan.error || scan.status !== 0) fail("process_scan_ambiguous", "could not inspect current process commands");
  for (const line of scan.stdout.split("\n")) {
    if (!line.trim()) continue;
    const match = line.match(/^\s*(\d+)\s+(\d+)\s+(\d+)\s+(.+)$/);
    if (!match) fail("process_scan_ambiguous", "current process command list contains an unreadable row");
    const processPid = Number(match[1]);
    if (Number(match[3]) === uid && !ancestors.has(processPid) && match[4].includes(target)) {
      fail("attempt005_process_command_live", `process command still references attempt-005: ${processPid}`);
    }
  }
}

async function readCleanupRecoverySources(owner, allocation, uid) {
  const root = allocation.root_path;
  for (const directory of [root, join(root, "logs"), join(root, "tmux"), join(root, "tmux", `tmux-${uid}`)]) {
    assertDirectory(await lstatOrNull(directory), directory, uid);
  }
  const files = [];
  const contents = new Map();
  for (const relativePath of cleanupRecoverySources) {
    const path = join(root, relativePath);
    const stat = await lstatOrNull(path);
    assertRegularFile(stat, path, uid);
    const content = await readFile(path);
    files.push({
      path: relativePath,
      sha256: createHash("sha256").update(content).digest("hex"),
      modified_at_utc: stat.mtime.toISOString(),
    });
    contents.set(relativePath, content.toString("utf8"));
  }
  const fileByPath = new Map(files.map(file => [file.path, file]));
  const recovery = JSON.parse(contents.get("logs/tmux-recovery.json"));
  const resultText = contents.get("logs/tmux-recovery-result.txt");
  const inspection = contents.get("logs/tmux-recovery-inspection.txt");
  const processLines = contents.get("owned-processes.tsv").trimEnd().split("\n");
  const serverFields = processLines.map(line => line.split("\t")).filter(fields => fields[0] === "tmux-server");
  const bootstrapFields = processLines.map(line => line.split("\t")).filter(fields => fields[0] === "tmux-bootstrap");
  const server = recovery.later_cleanup;
  const socketPath = join(root, "tmux", `tmux-${uid}`, "default");
  const removalMatch = resultText.match(/^stale_private_socket_node_removed_at=(.+)$/m);
  const removalAt = removalMatch?.[1] ?? "";
  if (recovery.stage_id !== owner.stage_id || recovery.run_uuid !== owner.run_id || recovery.attempt_id !== allocation.attempt_id || recovery.driver_outcome !== "blocked" || recovery.driver_cleanup_complete !== false || server?.method !== "tmux kill-server through the exact private socket" || server.socket !== socketPath || ![`${uid}:600`, `${uid}:660`].includes(server.socket_owner_mode_before_stop) || server.socket_directory_owner_mode !== `${uid}:700` || server.server_process_absent_after_stop !== true || server.pane_process_absent_after_stop !== true || server.socket_listener_absent_after_stop !== true || !Number.isSafeInteger(server.server_pid) || !Number.isSafeInteger(server.pane_pid) || !server.server_start_identity || !server.pane_start_identity || !server.executable || !Date.parse(recovery.recorded_at_utc) || resultText.split("\n")[0] !== "stopped_via_exact_private_socket" || !resultText.includes(`path=${socketPath}; listener and recorded processes absent before unlink`) || !Date.parse(removalAt) || Date.parse(removalAt) < Date.parse(recovery.recorded_at_utc)) {
    fail("cleanup_recovery_evidence_mismatch", "cleanup recovery source observations do not match the exact attempt");
  }
  const serverCommand = serverFields[0]?.[3] ?? "";
  const paneCommand = bootstrapFields[0]?.[3] ?? "";
  if (serverFields.length !== 1 || bootstrapFields.length !== 1 || serverFields[0][1] !== String(server.server_pid) || serverFields[0][2] !== server.server_start_identity || bootstrapFields[0][1] !== String(server.pane_pid) || bootstrapFields[0][2] !== server.pane_start_identity || !serverCommand.includes(server.executable) || !serverCommand.includes(join(root, "tmux.conf")) || paneCommand !== "/bin/sleep 86400") {
    fail("cleanup_recovery_process_ledger_mismatch", "recorded cleanup process identities do not match the ownership ledger");
  }
  if (!inspection.includes(`socket=${socketPath}\n`) || !inspection.includes(`socket_owner_mode=${server.socket_owner_mode_before_stop}\n`) || !inspection.includes(`server_pid=${server.server_pid}\n`) || !inspection.includes(`server_start_identity=${server.server_start_identity}\n`) || !inspection.includes(`server_command=${serverCommand}\n`) || !inspection.includes(`bootstrap_pid=${server.pane_pid}\n`) || !inspection.includes(`bootstrap_start_identity=${server.pane_start_identity}\n`) || !inspection.includes(`bootstrap_command=${paneCommand}\n`) || !inspection.includes(`socket_owner_pids=${server.server_pid}\n`) || !/^tmux\s+\d+\s/m.test(inspection)) {
    fail("cleanup_recovery_inspection_mismatch", "private socket inspection does not match the recorded cleanup identities");
  }
  const expectedObservations = [
    {
      type: "private_socket_shutdown",
      observed_at_utc: recovery.recorded_at_utc,
      socket_path: socketPath,
      method: server.method,
      server_pid: server.server_pid,
      server_start_identity: server.server_start_identity,
      server_command: serverCommand,
      source_hashes: Object.fromEntries(["logs/tmux-recovery-inspection.txt", "logs/tmux-recovery-stop.log", "logs/tmux-recovery.json", "owned-processes.tsv"].map(path => [path, fileByPath.get(path).sha256])),
    },
    {
      type: "server_process_absence",
      observed_at_utc: recovery.recorded_at_utc,
      pid: server.server_pid,
      start_identity: server.server_start_identity,
      absent: true,
      source_hashes: Object.fromEntries(["logs/tmux-recovery.json", "logs/tmux-recovery-result.txt", "owned-processes.tsv"].map(path => [path, fileByPath.get(path).sha256])),
    },
    {
      type: "bootstrap_process_absence",
      observed_at_utc: recovery.recorded_at_utc,
      pid: server.pane_pid,
      start_identity: server.pane_start_identity,
      command: paneCommand,
      absent: true,
      source_hashes: Object.fromEntries(["logs/tmux-recovery.json", "logs/tmux-recovery-result.txt", "owned-processes.tsv"].map(path => [path, fileByPath.get(path).sha256])),
    },
    {
      type: "private_socket_listener_absence",
      observed_at_utc: recovery.recorded_at_utc,
      socket_path: socketPath,
      absent: true,
      source_hashes: Object.fromEntries(["logs/tmux-recovery.json", "logs/tmux-recovery-result.txt"].map(path => [path, fileByPath.get(path).sha256])),
    },
    {
      type: "stale_socket_node_removal",
      observed_at_utc: removalAt,
      socket_path: socketPath,
      removed: true,
      source_hashes: Object.fromEntries(["logs/tmux-recovery-result.txt", "logs/tmux-recovery.json"].map(path => [path, fileByPath.get(path).sha256])),
    },
  ];
  return { files, observations: expectedObservations, recovery, server, removalAt };
}

function canonicalCleanupProcessStart(identity) {
  const parts = typeof identity === "string" ? identity.trim().split(/\s+/) : [];
  const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const months = new Map(["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"].map((month, index) => [month, index + 1]));
  if (parts.length !== 5 || !weekdays.includes(parts[0])) fail("cleanup_recovery_process_identity", "cleanup process start identity is not a complete ps timestamp");
  let month;
  let day;
  let time;
  let year;
  if (months.has(parts[1])) [month, day, time, year] = [months.get(parts[1]), parts[2], parts[3], parts[4]];
  else if (/^\d{1,2}$/.test(parts[1]) && months.has(parts[2])) [month, day, time, year] = [months.get(parts[2]), parts[1], parts[3], parts[4]];
  else fail("cleanup_recovery_process_identity", "cleanup process start identity has an unrecognized ps layout");
  if (!/^\d{1,2}$/.test(day) || !/^\d{2}:\d{2}:\d{2}$/.test(time) || !/^\d{4}$/.test(year)) fail("cleanup_recovery_process_identity", "cleanup process start identity has malformed fields");
  const numericDay = Number(day);
  const numericYear = Number(year);
  const [hours, minutes, seconds] = time.split(":").map(Number);
  const date = new Date(Date.UTC(numericYear, month - 1, numericDay));
  if (numericYear < 1000 || numericDay < 1 || date.getUTCFullYear() !== numericYear || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== numericDay || hours > 23 || minutes > 59 || seconds > 59 || weekdays[date.getUTCDay()] !== parts[0]) {
    fail("cleanup_recovery_process_identity", "cleanup process start identity has an invalid date or time");
  }
  return `${year}-${String(month).padStart(2, "0")}-${String(numericDay).padStart(2, "0")} ${time}`;
}

async function readAttempt002CleanupRecoverySources(owner, allocation, uid) {
  const root = allocation.root_path;
  const socketPath = join(root, "tmux", `tmux-${uid}`, "default");
  for (const directory of [root, join(root, "logs"), join(root, "tmux"), dirname(socketPath)]) {
    const stat = await lstatOrNull(directory);
    assertDirectory(stat, directory, uid);
    if (await realpath(directory) !== directory) fail("cleanup_recovery_directory_path", `cleanup evidence directory is not canonical: ${directory}`);
  }
  const externalDriverOutput = "/private/tmp/pi-scroll-p1-attempt-002-driver-e90d2905.log";
  const externalCleanupOutput = "/private/tmp/pi-scroll-p1-attempt-002-postfailure-cleanup-e90d2905.log";
  const specifications = [
    { path: "attempt:owned-processes.tsv", diskPath: join(root, "owned-processes.tsv"), mode: 0o600 },
    { path: "attempt:tmux.conf", diskPath: join(root, "tmux.conf"), mode: 0o600 },
    { path: "attempt:.stage-owner.json", diskPath: join(root, ".stage-owner.json"), mode: 0o600 },
    { path: "attempt:logs/tmux-discovery.json", diskPath: join(root, "logs/tmux-discovery.json"), mode: 0o600 },
    { path: "attempt:logs/tmux-stop.log", diskPath: join(root, "logs/tmux-stop.log"), mode: 0o600 },
    { path: "attempt:logs/tmux-postfailure-cleanup.mjs", diskPath: join(root, "logs/tmux-postfailure-cleanup.mjs"), mode: 0o644 },
    { path: "attempt:logs/tmux-postfailure-ownership-proof.json", diskPath: join(root, "logs/tmux-postfailure-ownership-proof.json"), mode: 0o600 },
    { path: "attempt:logs/tmux-postfailure-stop.log", diskPath: join(root, "logs/tmux-postfailure-stop.log"), mode: 0o600 },
    { path: "attempt:logs/tmux-postfailure-cleanup.json", diskPath: join(root, "logs/tmux-postfailure-cleanup.json"), mode: 0o600 },
    { path: "attempt:logs/tmux-postfailure-absence-reinspection.json", diskPath: join(root, "logs/tmux-postfailure-absence-reinspection.json"), mode: 0o600 },
    { path: "stage:stage-owner.json", diskPath: join(owner.stage_root, "stage-owner.json"), mode: 0o600 },
    { path: "stage:attempts/attempt-002/attempt-owner.json", diskPath: join(owner.stage_root, "attempts", "attempt-002", "attempt-owner.json"), mode: 0o600 },
    { path: "stage:attempts/attempt-002/root-created.json", diskPath: join(owner.stage_root, "attempts", "attempt-002", "root-created.json"), mode: 0o600 },
    { path: `external:${externalDriverOutput}`, diskPath: externalDriverOutput, mode: 0o600 },
    { path: `external:${externalCleanupOutput}`, diskPath: externalCleanupOutput, mode: 0o600 },
  ];
  const files = [];
  const contents = new Map();
  const modesByPath = new Map();
  for (const specification of specifications) {
    const stat = await lstatOrNull(specification.diskPath);
    assertRegularFile(stat, specification.diskPath, uid, specification.mode);
    if (specification.diskPath.startsWith("/private/tmp/") && await realpath(dirname(specification.diskPath)) !== dirname(specification.diskPath)) fail("cleanup_recovery_source_path", `cleanup source parent is not canonical: ${specification.diskPath}`);
    const content = await readFile(specification.diskPath);
    const entry = { path: specification.path, sha256: createHash("sha256").update(content).digest("hex"), modified_at_utc: stat.mtime.toISOString() };
    files.push(entry);
    modesByPath.set(specification.path, stat.mode & 0o777);
    contents.set(specification.path, content.toString("utf8"));
  }
  const fileByPath = new Map(files.map(file => [file.path, file]));
  const attemptContent = relativePath => contents.get(`attempt:${relativePath}`);
  const proof = JSON.parse(attemptContent("logs/tmux-postfailure-ownership-proof.json"));
  const cleanup = JSON.parse(attemptContent("logs/tmux-postfailure-cleanup.json"));
  const discovery = JSON.parse(attemptContent("logs/tmux-discovery.json"));
  const reinspection = JSON.parse(attemptContent("logs/tmux-postfailure-absence-reinspection.json"));
  const script = attemptContent("logs/tmux-postfailure-cleanup.mjs");
  const cleanupOutput = contents.get(`external:${externalCleanupOutput}`);
  const driverOutput = contents.get(`external:${externalDriverOutput}`);
  const ledgerRecords = await readJsonLines(join(owner.stage_root, "allocation-ledger.jsonl"), uid);
  const outcome = ledgerRecords.find(record => record.kind === "outcome" && record.attempt_id === allocation.attempt_id);
  const processLedger = attemptContent("owned-processes.tsv").trimEnd().split("\n");
  if (processLedger[0] !== "role\tpid\tstart_identity\tcommand_identity" || processLedger.length !== 3) fail("cleanup_recovery_process_ledger_mismatch", "attempt-002 owned process ledger is incomplete");
  const serverFields = processLedger[1].split("\t");
  const bootstrapFields = processLedger[2].split("\t");
  if (serverFields.length !== 4 || bootstrapFields.length !== 4 || serverFields[0] !== "tmux-server" || bootstrapFields[0] !== "tmux-bootstrap") fail("cleanup_recovery_process_ledger_mismatch", "attempt-002 process ledger roles or fields are invalid");
  const serverPid = Number(serverFields[1]);
  const bootstrapPid = Number(bootstrapFields[1]);
  if (serverPid !== 67968 || bootstrapPid !== 67970) fail("cleanup_recovery_process_ledger_mismatch", "attempt-002 process ledger PIDs differ from the retained cleanup evidence");
  const serverCommand = serverFields[3];
  const bootstrapCommand = bootstrapFields[3];
  const tmuxBin = "/opt/homebrew/Cellar/tmux/3.7c/bin/tmux";
  const serverStart = canonicalCleanupProcessStart(serverFields[2]);
  const bootstrapStart = canonicalCleanupProcessStart(bootstrapFields[2]);
  const proofStartSources = ["owned-processes.tsv", "tmux.conf", "logs/tmux-discovery.json", "logs/tmux-stop.log", ".stage-owner.json"].map(path => {
    const entry = fileByPath.get(`attempt:${path}`);
    if (!entry) fail("cleanup_recovery_hash_mismatch", `missing attempt-002 proof source hash: ${path}`);
    return { path, sha256: entry.sha256, modified_at_utc: entry.modified_at_utc };
  });
  const proofHash = fileByPath.get("attempt:logs/tmux-postfailure-ownership-proof.json").sha256;
  const stopHash = fileByPath.get("attempt:logs/tmux-postfailure-stop.log").sha256;
  const cleanupStdout = JSON.parse(cleanupOutput);
  const driverLines = driverOutput.trimEnd().split("\n");
  const driverFailure = JSON.parse(driverLines.at(-1) ?? "null");
  const proofTimestamp = Date.parse(proof.observed_at_utc);
  const cleanupTimestamp = Date.parse(cleanup.cleanup_observed_at_utc);
  const reinspectionTimestamp = Date.parse(reinspection.observed_at_utc);
  const timestamp = value => Number.isFinite(Date.parse(value)) && new Date(Date.parse(value)).toISOString() === value;
  const exactCommand = `${tmuxBin} -S ${socketPath} kill-server`;
  const reinspectionSourcesMatch = Array.isArray(reinspection.source_files) && reinspection.source_files.length === 9 && new Set(reinspection.source_files.map(source => source.path)).size === 9 && reinspection.source_files.every(source => {
    const current = fileByPath.get(`attempt:${source.path}`);
    return current && source.sha256 === current.sha256 && source.modified_at_utc === current.modified_at_utc && source.mode === modesByPath.get(`attempt:${source.path}`);
  });
  const expectedDirectories = [root, join(root, "logs"), join(root, "tmux"), dirname(socketPath)].map(path => ({ path, uid, mode: 0o700 }));
  const processAbsence = pid => {
    const process = reinspection.processes?.find(item => item.pid === pid && item.absent === true);
    if (!process) return false;
    const expectedProbes = {
      start: ["-p", String(pid), "-o", "lstart="],
      command: ["-p", String(pid), "-o", "command="],
      parent: ["-p", String(pid), "-o", "ppid="],
      uid: ["-p", String(pid), "-o", "uid="],
    };
    return Object.entries(expectedProbes).every(([name, args]) => {
      const probe = process.probes?.[name];
      return probe?.status === 1 && probe.stdout === "" && JSON.stringify(probe.command) === JSON.stringify(["/bin/ps", ...args]);
    });
  };
  const server = proof.server;
  const bootstrap = proof.bootstrap;
  const stageOwner = JSON.parse(contents.get("stage:stage-owner.json"));
  const attemptOwner = JSON.parse(contents.get("stage:attempts/attempt-002/attempt-owner.json"));
  const rootCreated = JSON.parse(contents.get("stage:attempts/attempt-002/root-created.json"));
  const rootOwner = JSON.parse(attemptContent(".stage-owner.json"));
  const commandCheck = script.includes(`const stageId = "${owner.stage_id}";`) && script.includes(`const runId = "${owner.run_id}";`) && script.includes('const attemptId = "attempt-002";') && script.includes(`const tmuxBin = "${tmuxBin}";`) && script.includes('const socketPath = `${root}/tmux/tmux-${uid}/default`;') && script.includes('run(tmuxBin, ["-S", socketPath, "kill-server"]') && script.includes("if (stop.status !== 0) fail") && script.includes("if (serverAfter || bootstrapAfter) fail") && script.includes("const afterOwners = listeners();") && script.includes("await unlink(socketPath);") && script.includes("removed = true;") && script.includes("if (await stat(socketPath)) fail(\"private socket node remains after cleanup\")");
  const lsof = spawnSync("/usr/sbin/lsof", ["-n", "-t", socketPath], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  const lsofMissingPath = lsof.status === 1 && !lsof.stdout.trim() && lsof.stderr.startsWith(`lsof: status error on ${socketPath}: No such file or directory`);
  const currentSocket = await lstatOrNull(socketPath);
  if (outcome?.result !== "blocked" || outcome.cleanup_complete !== false || outcome.failure_reason !== "tmux stop through its verified private socket failed" || stageOwner.stage_id !== owner.stage_id || stageOwner.run_id !== owner.run_id || attemptOwner.stage_id !== owner.stage_id || attemptOwner.run_id !== owner.run_id || attemptOwner.attempt_id !== "attempt-002" || attemptOwner.root_path !== root || rootCreated.stage_id !== owner.stage_id || rootCreated.run_id !== owner.run_id || rootCreated.attempt_id !== "attempt-002" || rootCreated.root_path !== root || rootOwner.stage_id !== owner.stage_id || rootOwner.run_id !== owner.run_id || rootOwner.attempt_id !== "attempt-002" || rootOwner.root_path !== root || rootOwner.uid !== uid || proof.stage_id !== owner.stage_id || proof.run_id !== owner.run_id || proof.attempt_id !== "attempt-002" || proof.socket_path !== socketPath || proof.socket_owner_uid !== uid || ![0o600, 0o660].includes(proof.socket_mode) || proof.socket_mode !== discovery.socket_mode || !Array.isArray(proof.socket_listener_pids) || proof.socket_listener_pids.length !== 1 || proof.socket_listener_pids[0] !== String(serverPid) || server.pid !== serverPid || bootstrap.pid !== bootstrapPid || server.uid !== uid || bootstrap.uid !== uid || server.parent_pid !== 1 || bootstrap.parent_pid !== serverPid || server.recorded_start_identity !== serverFields[2] || bootstrap.recorded_start_identity !== bootstrapFields[2] || server.canonical_start_identity !== serverStart || bootstrap.canonical_start_identity !== bootstrapStart || serverStart !== "2026-10-07 00:28:18" || bootstrapStart !== "2026-10-07 00:28:18" || canonicalCleanupProcessStart(server.observed_start_identity) !== serverStart || canonicalCleanupProcessStart(bootstrap.observed_start_identity) !== bootstrapStart || canonicalCleanupProcessStart(discovery.server.startIdentity) !== serverStart || canonicalCleanupProcessStart(discovery.bootstrap.startIdentity) !== bootstrapStart || server.command_identity !== serverCommand || bootstrap.command_identity !== bootstrapCommand || discovery.server.pid !== serverPid || discovery.bootstrap.pid !== bootstrapPid || discovery.server.commandIdentity !== serverCommand || discovery.bootstrap.commandIdentity !== bootstrapCommand || discovery.server.uid !== uid || discovery.bootstrap.uid !== uid || discovery.server.parentPid !== 1 || discovery.bootstrap.parentPid !== serverPid || discovery.socket_path !== socketPath || JSON.stringify(discovery.socket_owner_pids) !== JSON.stringify([String(serverPid)]) || serverCommand !== `${tmuxBin} -f ${join(root, "tmux.conf")} new-session -d -s ${basename(root)} -x 100 -y 36 exec /bin/sleep 86400` || bootstrapCommand !== "/bin/sleep 86400" || !commandCheck || !Array.isArray(proof.source_files) || JSON.stringify(proof.source_files) !== JSON.stringify(proofStartSources) || !timestamp(proof.observed_at_utc) || !timestamp(cleanup.cleanup_observed_at_utc) || !timestamp(reinspection.observed_at_utc) || proofTimestamp > cleanupTimestamp || cleanupTimestamp > reinspectionTimestamp || Date.parse(fileByPath.get("attempt:logs/tmux-postfailure-cleanup.mjs").modified_at_utc) > proofTimestamp || Date.parse(fileByPath.get("attempt:logs/tmux-postfailure-ownership-proof.json").modified_at_utc) < proofTimestamp || Date.parse(fileByPath.get("attempt:logs/tmux-postfailure-stop.log").modified_at_utc) < proofTimestamp || Date.parse(fileByPath.get("attempt:logs/tmux-postfailure-cleanup.json").modified_at_utc) < cleanupTimestamp || Date.parse(fileByPath.get(`external:${externalCleanupOutput}`).modified_at_utc) < cleanupTimestamp || Date.parse(fileByPath.get(`external:${externalDriverOutput}`).modified_at_utc) > proofTimestamp || Date.parse(fileByPath.get("attempt:logs/tmux-postfailure-absence-reinspection.json").modified_at_utc) < reinspectionTimestamp || cleanup.server_process_absent !== true || cleanup.bootstrap_process_absent !== true || cleanup.socket_listener_absent !== true || cleanup.stale_socket_node_removed !== true || cleanup.socket_path !== socketPath || cleanup.ownership_proof_path !== "logs/tmux-postfailure-ownership-proof.json" || cleanup.ownership_proof_sha256 !== proofHash || cleanup.stop_log_sha256 !== stopHash || JSON.stringify(cleanupStdout) !== JSON.stringify(cleanup) || driverFailure?.code !== "tmux_server_identity" || !processAbsence(serverPid) || !processAbsence(bootstrapPid) || !reinspection.private_socket?.listener_probe?.absent || reinspection.private_socket.listener_probe.status !== 1 || reinspection.private_socket.listener_probe.stdout !== "" || !reinspection.private_socket.listener_probe.stderr.startsWith(`lsof: status error on ${socketPath}: No such file or directory`) || !reinspection.private_socket.listener_probe.stderr.includes("usage:") || JSON.stringify(reinspection.private_socket.listener_probe.command) !== JSON.stringify(["/usr/sbin/lsof", "-n", "-t", socketPath]) || reinspection.private_socket.path !== socketPath || reinspection.private_socket.node_absent !== true || reinspection.processes?.length !== 2 || JSON.stringify(reinspection.private_directories) !== JSON.stringify(expectedDirectories) || !reinspectionSourcesMatch || lsof.error || !lsofMissingPath || currentSocket) {
    fail("cleanup_recovery_attempt_002_evidence_mismatch", "attempt-002 cleanup evidence, identities, timestamps, hashes, or current absence checks do not reconcile");
  }
  const sourceHashes = Object.fromEntries(files.map(file => [file.path, file.sha256]));
  const observations = [
    {
      type: "private_socket_owner_before_stop",
      observed_at_utc: proof.observed_at_utc,
      socket_path: socketPath,
      socket_owner_uid: proof.socket_owner_uid,
      socket_mode: proof.socket_mode,
      listener_pids: proof.socket_listener_pids,
      server_pid: serverPid,
      server_start_identity: server.recorded_start_identity,
      server_canonical_start_identity: serverStart,
      server_command: serverCommand,
      server_uid: server.uid,
      server_parent_pid: server.parent_pid,
      bootstrap_pid: bootstrapPid,
      bootstrap_start_identity: bootstrap.recorded_start_identity,
      bootstrap_canonical_start_identity: bootstrapStart,
      bootstrap_command: bootstrapCommand,
      bootstrap_uid: bootstrap.uid,
      bootstrap_parent_pid: bootstrap.parent_pid,
      source_hashes: sourceHashes,
    },
    {
      type: "private_socket_shutdown",
      observed_at_utc: cleanup.cleanup_observed_at_utc,
      socket_path: socketPath,
      command_argv: [tmuxBin, "-S", socketPath, "kill-server"],
      command_text: exactCommand,
      exit_status: 0,
      source_hashes: sourceHashes,
    },
    {
      type: "server_process_absence",
      observed_at_utc: reinspection.observed_at_utc,
      pid: serverPid,
      start_identity: server.recorded_start_identity,
      canonical_start_identity: serverStart,
      command: serverCommand,
      uid: uid,
      parent_pid: 1,
      absent: true,
      source_hashes: sourceHashes,
    },
    {
      type: "bootstrap_process_absence",
      observed_at_utc: reinspection.observed_at_utc,
      pid: bootstrapPid,
      start_identity: bootstrap.recorded_start_identity,
      canonical_start_identity: bootstrapStart,
      command: bootstrapCommand,
      uid: uid,
      parent_pid: serverPid,
      absent: true,
      source_hashes: sourceHashes,
    },
    {
      type: "private_socket_listener_absence",
      observed_at_utc: reinspection.observed_at_utc,
      socket_path: socketPath,
      command_argv: ["/usr/sbin/lsof", "-n", "-t", socketPath],
      exit_status: reinspection.private_socket.listener_probe.status,
      listener_pids: [],
      socket_node_absent: true,
      probe_result: "socket_path_missing",
      probe_stderr: reinspection.private_socket.listener_probe.stderr,
      absent: true,
      source_hashes: sourceHashes,
    },
    {
      type: "stale_socket_node_removal",
      observed_at_utc: cleanup.cleanup_observed_at_utc,
      socket_path: socketPath,
      removed: true,
      rechecked_absent_at_utc: reinspection.observed_at_utc,
      source_hashes: sourceHashes,
    },
  ];
  return { files, observations, server_pid: serverPid, server_start_identity: server.recorded_start_identity, bootstrap_pid: bootstrapPid, bootstrap_start_identity: bootstrap.recorded_start_identity, socket_path: socketPath };
}

async function validateCleanupRecovery(owner, allocation, outcome, record, uid) {
  if (!outcome || outcome.result !== "blocked" || outcome.cleanup_complete !== false || outcome.root_created !== true || record.original_outcome_seq !== outcome.seq) {
    fail("cleanup_recovery_outcome_mismatch", `cleanup recovery does not preserve a blocked cleanup outcome: ${allocation.attempt_id}`);
  }
  const evidence = allocation.attempt_id === "attempt-001"
    ? await readCleanupRecoverySources(owner, allocation, uid)
    : allocation.attempt_id === "attempt-002"
      ? await readAttempt002CleanupRecoverySources(owner, allocation, uid)
      : fail("cleanup_recovery_not_applicable", `no cleanup recovery validator exists for ${allocation.attempt_id}`);
  if (JSON.stringify(record.source_files) !== JSON.stringify(evidence.files) || JSON.stringify(record.observations) !== JSON.stringify(evidence.observations)) {
    fail("cleanup_recovery_hash_mismatch", `cleanup recovery source hashes or timestamps changed: ${allocation.attempt_id}`);
  }
  const expectedSocket = evidence.socket_path ?? join(allocation.root_path, "tmux", `tmux-${uid}`, "default");
  if (await lstatOrNull(expectedSocket)) fail("cleanup_socket_node_present", `recovered tmux socket node still exists: ${expectedSocket}`);
  const listener = spawnSync("/usr/sbin/lsof", ["-n", "-t", expectedSocket], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  if (listener.error || listener.status !== 1 || listener.stdout.trim() || !listener.stderr.startsWith(`lsof: status error on ${expectedSocket}: No such file or directory`)) fail("cleanup_socket_listener_present", `recovered tmux socket still has a listener: ${expectedSocket}`);
  if (evidence.server_pid) {
    requireNoLiveIdentity(evidence.server_pid, evidence.server_start_identity);
    requireNoLiveIdentity(evidence.bootstrap_pid, evidence.bootstrap_start_identity);
  } else {
    requireNoLiveIdentity(evidence.server.server_pid, evidence.server.server_start_identity);
    requireNoLiveIdentity(evidence.server.pane_pid, evidence.server.pane_start_identity);
  }
}

async function writeExclusive(path, content) {
  const handle = await open(path, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try {
    await handle.writeFile(content);
    await handle.sync();
  } finally {
    await handle.close();
  }
}

async function appendExclusiveLedger(path, content) {
  const stat = await lstatOrNull(path);
  assertRegularFile(stat, path, process.getuid());
  if (stat.size + Buffer.byteLength(content) > maxLedgerBytes) fail("ledger_limit", "stage allocation ledger reached its storage limit");
  const handle = await open(path, constants.O_WRONLY | constants.O_APPEND | constants.O_NOFOLLOW);
  try {
    await handle.writeFile(content);
    await handle.sync();
  } finally {
    await handle.close();
  }
}

function observationRecords(stageId, runId, now) {
  return [
    {
      event_id: "prior-global-prefix-refusal",
      stage_id: stageId,
      run_id: runId,
      kind: "observation",
      allocated_attempt: false,
      observed_at_utc: now,
      evidence: "Prior driver preflight counted 105 broad-prefix historical roots against six; it stopped before creating a fixture root.",
    },
    {
      event_id: "prior-fixture-test-skip",
      stage_id: stageId,
      run_id: runId,
      kind: "observation",
      allocated_attempt: false,
      observed_at_utc: now,
      evidence: "The targeted Go fixture test skipped because no owned disposable Herdr/Pi fixture was supplied.",
    },
  ];
}

function allocationLimit(owner) {
  return owner.successor ? 1 : 6;
}

function exactKeys(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) fail(code, "unexpected or missing configuration fields");
}

async function canonicalDirectory(path, uid, privateMode = true) {
  if (typeof path !== "string" || resolve(path) !== path || await realpath(path) !== path) fail("noncanonical_path", `noncanonical directory: ${path}`);
  let cursor = sep;
  for (const component of path.split(sep).filter(Boolean)) {
    cursor = join(cursor, component);
    const stat = await lstatOrNull(cursor);
    if (!stat || stat.isSymbolicLink() || !stat.isDirectory()) fail("noncanonical_path", `unsafe directory component: ${cursor}`);
  }
  if (privateMode) assertDirectory(await lstatOrNull(path), path, uid);
}

async function validateStageNamespace(owner, uid) {
  if (typeof owner.stage_id !== "string" || typeof owner.run_id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(owner.stage_id) || !/^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(owner.run_id) || owner.owner_uid !== uid || owner.max_attempts !== 6 || owner.fixture_root_prefix !== `herdr-scroll-p1-${owner.stage_id}-` || resolve(owner.stage_root) !== owner.stage_root) fail("stage_identity_mismatch", "invalid stage namespace");
  await canonicalDirectory(dirname(owner.stage_root), uid, false);
  await canonicalDirectory(owner.fixture_root_parent, uid, false);
  const parent = await lstat(owner.fixture_root_parent);
  if (parent.uid !== uid && (parent.mode & 0o1000) === 0) fail("unsafe_stage_parent", "fixture parent is neither owned nor sticky");
  if (owner.successor && owner.allocation_limit !== 1) fail("successor_limit", "successor has exactly one local allocation");
  if (!owner.successor && owner.allocation_limit !== undefined) fail("successor_limit", "default stage cannot override its six-slot cap");
}

async function validateRootMarker(owner, allocation, uid) {
  await canonicalDirectory(allocation.root_path, uid);
  const markerPath = join(allocation.root_path, "fixture-owner");
  assertRegularFile(await lstatOrNull(markerPath), markerPath, uid);
  if (await readFile(markerPath, "utf8") !== "disposable-scroll-fixture\n") fail("fixture_marker_mismatch", "fixture marker content is invalid");
  const marker = await readOwnedJson(join(allocation.root_path, ".stage-owner.json"), uid);
  if (marker.stage_id !== owner.stage_id || marker.run_id !== owner.run_id || marker.attempt_id !== allocation.attempt_id || marker.root_path !== allocation.root_path || marker.uid !== uid) fail("root_owner_mismatch", "fixture marker does not bind this exact stage, run, attempt, root and UID");
}

function overlaps(a, b) {
  return a === b || a.startsWith(`${b}${sep}`) || b.startsWith(`${a}${sep}`);
}

async function validatePredecessor(owner, uid) {
  const pin = owner.successor;
  validateSuccessorPin(pin);
  await canonicalDirectory(pin.stageRoot, uid);
  if (owner.stage_id === pin.stageId || owner.run_id === pin.runId || overlaps(owner.stage_root, pin.stageRoot)) fail("predecessor_overlap", "successor and predecessor stage identities overlap");
  const predecessor = await readOwnedJson(join(pin.stageRoot, "stage-owner.json"), uid);
  await validateStageNamespace(predecessor, uid);
  if (predecessor.successor || predecessor.stage_root !== pin.stageRoot || predecessor.stage_id !== pin.stageId || predecessor.run_id !== pin.runId || digest(await readOwnedBytes(join(pin.stageRoot, "stage-owner.json"), uid)) !== pin.ownerSha256) fail("predecessor_identity", "predecessor owner identity or digest changed");
  await checkOwner(pin.stageRoot, predecessor, uid);
  if (await lstatOrNull(join(pin.stageRoot, "active.lock"))) fail("predecessor_open", "predecessor still has an active or unresolved lock");
  const ledgerPath = join(pin.stageRoot, "allocation-ledger.jsonl");
  if (digest(await readOwnedBytes(ledgerPath, uid)) !== pin.ledgerSha256) fail("predecessor_hash", "immutable predecessor ledger changed");
  const records = await readJsonLines(ledgerPath, uid);
  const allocations = [], terminal = new Map(), recoveries = new Set();
  for (const [index, record] of records.entries()) {
    if (record.seq !== index + 1 || record.stage_id !== pin.stageId || record.run_id !== pin.runId) fail("predecessor_sequence", "predecessor record identity or sequence changed");
    if (record.kind === "allocation") {
      const number = allocations.length + 1;
      if (number > 6 || record.attempt_number !== number || record.attempt_id !== attemptId(number) || record.root_path !== rootPath(predecessor, number)) fail("predecessor_count", "predecessor allocation count or namespace is forged");
      if (overlaps(record.root_path, rootPath(owner, 1)) || overlaps(owner.stage_root, record.root_path) || overlaps(pin.stageRoot, rootPath(owner, 1))) fail("predecessor_overlap", "fixture namespaces overlap");
      allocations.push(record);
    } else if (record.kind === "outcome") {
      if (!allocations.some(a => a.attempt_id === record.attempt_id) || terminal.has(record.attempt_id) || !outcomes.has(record.result) || typeof record.root_created !== "boolean" || typeof record.cleanup_complete !== "boolean") fail("predecessor_outcome", "predecessor terminal record is invalid");
      terminal.set(record.attempt_id, record);
    } else if (record.kind === "cleanup-recovery") {
      if (!terminal.has(record.attempt_id) || recoveries.has(record.attempt_id) || record.original_outcome_seq !== terminal.get(record.attempt_id).seq || !Array.isArray(record.source_files) || !Array.isArray(record.observations)) fail("predecessor_outcome", "predecessor recovery record is invalid");
      recoveries.add(record.attempt_id);
    } else if (record.kind !== "refusal" || !refusalReasons.has(record.reason) || typeof record.requested_id !== "string") fail("predecessor_sequence", "unknown predecessor record");
  }
  if (allocations.length !== 6 || terminal.size !== 6) fail("predecessor_count", "predecessor must have six allocations and six original terminal outcomes");
  const entries = (await readdir(join(pin.stageRoot, "attempts"))).sort();
  if (JSON.stringify(entries) !== JSON.stringify(allocations.map(a => a.attempt_id))) fail("predecessor_identity", "predecessor attempt directory set changed");
  for (const allocation of allocations) {
    const outcome = terminal.get(allocation.attempt_id);
    const attemptRoot = join(pin.stageRoot, "attempts", allocation.attempt_id);
    await canonicalDirectory(attemptRoot, uid);
    const metadata = await readOwnedJson(join(attemptRoot, "attempt-owner.json"), uid);
    if (metadata.stage_id !== pin.stageId || metadata.run_id !== pin.runId || metadata.attempt_id !== allocation.attempt_id || metadata.root_path !== allocation.root_path || !Number.isFinite(Date.parse(outcome.recorded_at_utc))) fail("predecessor_identity", "predecessor attempt marker/outcome changed");
    if (!outcome.root_created) {
      if (await lstatOrNull(join(attemptRoot, "root-created.json"))) fail("predecessor_identity", "uncreated fixture has a created-root record");
      continue;
    }
    await validateRootMarker(predecessor, allocation, uid);
    const created = await readOwnedJson(join(attemptRoot, "root-created.json"), uid);
    if (created.root_path !== allocation.root_path || created.stage_id !== pin.stageId || created.run_id !== pin.runId || created.attempt_id !== allocation.attempt_id) fail("predecessor_identity", "predecessor root-created record changed");
  }
  if (overlaps(pin.dispositionPath, pin.stageRoot) || allocations.some(a => overlaps(pin.dispositionPath, a.root_path)) || overlaps(pin.dispositionPath, owner.stage_root)) fail("predecessor_overlap", "disposition must be separate from historical and new stage roots");
  await canonicalDirectory(dirname(pin.dispositionPath), uid);
  const dispositionBytes = await readPinnedEvidenceBytes(pin.dispositionPath, uid);
  if (digest(dispositionBytes) !== pin.dispositionSha256) fail("predecessor_hash", "historical disposition changed");
  const admission = await validateHistoricalDisposition(JSON.parse(dispositionBytes), { pin, owner, records, allocations, terminal, uid }, {
    canonicalDirectory,
    readOwnedBytes,
    readPinnedEvidenceBytes,
    readHistoricalBytes: async (path, uid) => {
      await canonicalDirectory(dirname(path), uid);
      const stat = await lstatOrNull(path);
      if (!stat || stat.isSymbolicLink() || !stat.isFile() || stat.uid !== uid || (stat.mode & 0o022) !== 0 || stat.size > maxLedgerBytes) fail("unsafe_evidence", "unsafe scoped historical recovery");
      return readBoundedRegular(path, uid, [stat.mode & 0o777]);
    },
  });
  return { stage_root: pin.stageRoot, stage_id: pin.stageId, run_id: pin.runId, allocation_count: allocations.length, owner_sha256: pin.ownerSha256, ledger_sha256: pin.ledgerSha256, disposition_path: pin.dispositionPath, disposition_sha256: pin.dispositionSha256, ...admission, authorization_proved: false };
}

export async function validateFixtureContract(input) {
  const { stageRoot, stageId, runId, attemptId: id, root, socketPath, lockToken, uid = process.getuid() } = input;
  await canonicalDirectory(stageRoot, uid);
  const owner = await readOwnedJson(join(stageRoot, "stage-owner.json"), uid);
  if (owner.stage_root !== stageRoot || owner.stage_id !== stageId || owner.run_id !== runId || owner.owner_uid !== uid) fail("stage_identity_mismatch", "fixture configuration does not match its stage owner");
  const state = await parseStage(stageRoot, owner, uid, { externalRecoveryProofPath: input.externalRecoveryProofPath, attempt005PostcheckPath: input.attempt005PostcheckPath });
  const { data } = await lockData(stageRoot, lockToken, uid);
  if (data.run_id !== runId) fail("stage_identity_mismatch", "fixture active lock belongs to another run");
  const allocation = state.allocations.find(a => a.attempt_id === id);
  if (!allocation || allocation.root_path !== root || state.outcomes.has(id)) fail("fixture_identity", "fixture must use its exact active allocated root");
  await validateRootMarker(owner, allocation, uid);
  for (const directory of [join(root, "config"), join(root, "config/herdr")]) await canonicalDirectory(directory, uid);
  if (socketPath !== join(root, "config/herdr/herdr.sock")) fail("wrong_socket_path", "fixture API socket is outside the exact root contract");
  for (const path of [socketPath, join(root, "config/herdr/herdr-client.sock")]) {
    const stat = await lstatOrNull(path);
    if (!stat || stat.isSymbolicLink() || !stat.isSocket() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600) fail("unsafe_socket", "fixture sockets must be private owned Unix sockets");
  }
  return { verified: true, root, marker_path: join(root, "fixture-owner"), socket_path: socketPath };
}

export async function readSuccessorConfiguration(path, uid = process.getuid()) {
  await canonicalDirectory(dirname(path), uid);
  const bytes = await readPinnedEvidenceBytes(path, uid);
  let config;
  try { config = JSON.parse(bytes); } catch { fail("successor_configuration", "invalid successor configuration"); }
  exactKeys(config, ["schema_version", "stageRoot", "stageId", "runId", "fixtureRootParent", "outputDirectory", "successor", "bounds"], "successor_configuration");
  if (config.schema_version !== 1 || typeof config.outputDirectory !== "string" || resolve(config.outputDirectory) !== config.outputDirectory) fail("successor_configuration", "invalid successor configuration identity");
  for (const entry of [config.stageRoot, config.fixtureRootParent, config.outputDirectory, path]) if (typeof entry !== "string" || !/^[A-Za-z0-9_./-]+$/.test(entry)) fail("successor_configuration", "configuration paths must be safe canonical shell fields");
  const { validateBounds } = await import("./pi-scroll-qualification-limits.mjs");
  validateBounds(config.bounds);
  const owner = expectedOwner(config.stageRoot, { ...config, uid });
  await validateStageNamespace(owner, uid);
  await validatePredecessor(owner, uid);
  const predecessorOwner = await readOwnedJson(join(config.successor.stageRoot, "stage-owner.json"), uid);
  if (overlaps(config.outputDirectory, config.stageRoot) || overlaps(config.outputDirectory, config.successor.stageRoot) || overlaps(config.outputDirectory, rootPath(owner, 1)) || Array.from({ length: 6 }, (_, i) => rootPath(predecessorOwner, i + 1)).some(root => overlaps(config.outputDirectory, root))) fail("predecessor_overlap", "output directory overlaps stage history");
  return config;
}

function expectedOwner(stageRoot, { stageId, runId, uid, fixtureRootParent, successor }) {
  return {
    schema_version: 1,
    stage_id: stageId,
    run_id: runId,
    stage_root: stageRoot,
    owner_uid: uid,
    max_attempts: 6,
    fixture_root_parent: fixtureRootParent,
    fixture_root_prefix: `herdr-scroll-p1-${stageId}-`,
    ...(successor ? { successor, allocation_limit: 1 } : {}),
  };
}

async function assertFixtureRootAbsent(path) {
  const stat = await lstatOrNull(path);
  if (stat) fail("root_path_collision", `expected fresh fixture root already exists: ${path}`);
}

async function checkOwner(stageRoot, expected, uid) {
  const stageStat = await lstatOrNull(stageRoot);
  assertDirectory(stageStat, stageRoot, uid);
  const parentPath = dirname(stageRoot);
  const parentStat = await lstatOrNull(parentPath);
  if (!parentStat || parentStat.isSymbolicLink() || !parentStat.isDirectory()) fail("unsafe_stage_parent", `unsafe stage parent: ${parentPath}`);
  const owner = await readOwnedJson(join(stageRoot, "stage-owner.json"), uid);
  for (const [key, value] of Object.entries(expected)) {
    if (JSON.stringify(owner[key]) !== JSON.stringify(value)) fail("stage_identity_mismatch", `stage owner field mismatch: ${key}`);
  }
  const stageEntries = new Set(await readdir(stageRoot));
  const required = ["stage-owner.json", "attempts", "allocation-ledger.jsonl", "pre-stage-observations.jsonl"];
  for (const name of required) if (!stageEntries.has(name)) fail("stage_history_incomplete", `missing stage record: ${name}`);
  for (const name of stageEntries) {
    if (![...required, "active.lock"].includes(name)) fail("unknown_stage_entry", `unknown stage entry: ${name}`);
  }
  const attemptsPath = join(stageRoot, "attempts");
  assertDirectory(await lstatOrNull(attemptsPath), attemptsPath, uid);
  return owner;
}

async function privateRecoveryDirectory(path, uid) {
  if (resolve(path) !== path || await realpath(path) !== path) fail("external_recovery_path", `noncanonical recovery directory: ${path}`);
  assertDirectory(await lstatOrNull(path), path, uid);
}

function freshProcessAbsent(pid) {
  const probe = spawnSync("/bin/ps", ["-p", String(pid), "-o", "lstart="], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
  if (probe.error || probe.status !== 1 || probe.stdout.trim() || probe.stderr.trim()) fail("external_recovery_process_live", `process absence is ambiguous: ${pid}`);
}

function retainedProcessAbsent(probes, pid) {
  const probe = probes?.find(item => item.pid === pid);
  if (probe?.status !== 1 || probe.stdout !== "" || probe.stderr !== "") fail("external_recovery_process_proof", `retained absence is incomplete: ${pid}`);
}

async function validateAttempt003Proof(owner, allocations, outcomesById, records, uid, proofPath) {
  const allocation = allocations[2];
  const outcome = outcomesById.get("attempt-003");
  const root = allocation?.root_path;
  if (!allocation || allocation.seq !== 7 || allocation.attempt_id !== "attempt-003" || root !== rootPath(owner, 3) || !outcome || outcome.seq !== 8 || outcome.result !== "blocked" || outcome.root_created !== true || outcome.cleanup_complete !== false || records.length < 8) fail("external_recovery_identity", "attempt-003 does not have the original blocked allocation and outcome");
  const logs = join(root, "logs");
  const expectedProof = join(logs, "tmux-postfailure-recovery-proof.json");
  if (proofPath !== expectedProof) fail("external_recovery_path", "recovery proof is outside the attempt-003 root");
  const attemptPath = join(owner.stage_root, "attempts", "attempt-003");
  const lockPath = join(owner.stage_root, "active.lock");
  const socketPath = join(root, "tmux", `tmux-${uid}`, "default");
  const herdrSockets = [join(root, "config", "herdr", "herdr.sock"), join(root, "config", "herdr", "herdr-client.sock")];
  const savedOwnerPath = join(logs, "stage-lock-owner-before-recovery.json");
  const activeOwnerPath = join(lockPath, "owner.json");
  const savedOwner = await lstatOrNull(savedOwnerPath);
  const originalOwnerPath = savedOwner ? savedOwnerPath : activeOwnerPath;
  const directories = [owner.stage_root, join(owner.stage_root, "attempts"), attemptPath, root, logs, join(root, "tmux"), dirname(socketPath), join(root, "config"), dirname(herdrSockets[0])];
  if (await lstatOrNull(lockPath)) directories.push(lockPath);
  for (const directory of directories) await privateRecoveryDirectory(directory, uid);
  const proofBytes = await readOwnedBytes(proofPath, uid);
  const proofHash = digest(proofBytes);
  if (owner.stage_root === attempt003Anchor.stageRoot && proofHash !== attempt003Anchor.proofHash) fail("external_recovery_hash", "retained attempt-003 proof bytes changed");
  let proof;
  try { proof = JSON.parse(proofBytes); } catch { fail("external_recovery_proof", "recovery proof is invalid JSON"); }
  const precheckPath = join(logs, "tmux-postfailure-recovery-precheck.json");
  const commandPath = join(logs, "tmux-postfailure-recovery-command.json");
  const precheckBytes = await readOwnedBytes(precheckPath, uid);
  const commandBytes = await readOwnedBytes(commandPath, uid);
  if (proof.schema_version !== 1 || proof.kind !== "attempt-003-stale-tmux-socket-cleanup-recovery-proof" || proof.stage_id !== owner.stage_id || proof.stage_run_id !== owner.run_id || proof.attempt_id !== allocation.attempt_id || proof.linked_allocation_seq !== allocation.seq || proof.linked_original_outcome_seq !== outcome.seq || proof.precheck_path !== precheckPath || proof.precheck_sha256 !== digest(precheckBytes) || proof.cleanup_command_path !== commandPath || proof.cleanup_command_sha256 !== digest(commandBytes) || proof.cleanup_command_status !== 0 || proof.cleanup_command_result?.removed !== true || proof.cleanup_command_result.socket_path !== socketPath || proof.post_cleanup_socket_node?.present !== false || proof.original_ledger_outcome_preserved?.result !== "blocked" || proof.original_ledger_outcome_preserved.cleanup_complete !== false) fail("external_recovery_proof", "retained proof does not match attempt-003");
  const precheck = JSON.parse(precheckBytes);
  const command = JSON.parse(commandBytes);
  const ledgerBytes = await readOwnedBytes(join(owner.stage_root, "allocation-ledger.jsonl"), uid);
  const prefix = Buffer.from(ledgerBytes.toString("utf8").split("\n").slice(0, 8).join("\n") + "\n");
  const prefixHash = digest(prefix);
  if (proof.source_hashes?.["allocation-ledger"]?.sha256 !== prefixHash || precheck.hashes?.["allocation-ledger"]?.sha256 !== prefixHash || (owner.stage_root === attempt003Anchor.stageRoot && prefixHash !== attempt003Anchor.ledgerPrefixHash)) fail("external_recovery_ledger", "frozen ledger prefix changed");
  const sources = {
    "stage-owner": join(owner.stage_root, "stage-owner.json"),
    "active-lock-owner": activeOwnerPath,
    "attempt-owner": join(attemptPath, "attempt-owner.json"),
    "root-created": join(attemptPath, "root-created.json"),
    "attempt-root-owner": join(root, ".stage-owner.json"),
    "fixture-owner": join(root, "fixture-owner"),
    "owned-processes": join(root, "owned-processes.tsv"),
    "tmux-config": join(root, "tmux.conf"),
    "tmux-discovery": join(logs, "tmux-discovery.json"),
    "tmux-socket-proof": join(logs, "tmux-socket-proof.json"),
    "herdr-cleanup-proof": join(logs, "herdr-postfailure-cleanup-proof.json"),
    "herdr-cleanup-result": join(logs, "herdr-postfailure-cleanup.json"),
  };
  for (const [name, originalPath] of Object.entries(sources)) {
    const source = proof.source_hashes?.[name];
    const currentPath = name === "active-lock-owner" ? originalOwnerPath : originalPath;
    if (source?.absolute_path !== originalPath || source.sha256 !== digest(await readOwnedBytes(currentPath, uid)) || precheck.hashes?.[name]?.sha256 !== source.sha256) fail("external_recovery_source", `retained source changed: ${name}`);
  }
  const oldOwnerHash = proof.source_hashes["active-lock-owner"].sha256;
  if (owner.stage_root === attempt003Anchor.stageRoot && oldOwnerHash !== attempt003Anchor.oldLockHash) fail("external_recovery_lock", "original lock owner hash changed");
  const oldOwner = JSON.parse(await readOwnedBytes(originalOwnerPath, uid));
  if (oldOwner.run_id !== owner.run_id || oldOwner.shell_command !== "pi-scroll-qualification.sh" || !Number.isSafeInteger(oldOwner.shell_pid) || oldOwner.shell_pid < 1 || oldOwner.shell_pid !== precheck.stage_lock?.owner_pid || oldOwner.shell_start !== precheck.stage_lock.owner_start_identity || precheck.stage_lock.path !== lockPath || precheck.stage_lock.owner_process_absent !== true) fail("external_recovery_lock", "original lock owner identity changed");
  if (precheck.stage_id !== owner.stage_id || precheck.stage_run_id !== owner.run_id || precheck.attempt_id !== allocation.attempt_id || precheck.original_outcome_seq !== outcome.seq || precheck.socket?.path !== socketPath || precheck.socket.uid !== uid || ![0o600, 0o660].includes(precheck.socket.mode) || precheck.socket.canonical_path !== true || command.stage_id !== owner.stage_id || command.attempt_id !== allocation.attempt_id || command.input_identity?.root !== root || command.input_identity.socket_path !== socketPath || command.input_identity.uid !== uid || command.status !== 0 || command.command_argv?.[0] !== "/bin/unlink" || command.command_argv[1] !== socketPath || command.pre_removal_revalidation?.socket?.device !== precheck.socket.device || command.pre_removal_revalidation.socket.inode !== precheck.socket.inode || command.pre_removal_revalidation.socket.uid !== uid || command.pre_removal_revalidation.socket.mode !== precheck.socket.mode) fail("external_recovery_socket_proof", "socket removal does not match its private precheck");
  const lines = (await readFile(sources["owned-processes"], "utf8")).trimEnd().split("\n").map(line => line.split("\t"));
  if (lines[0]?.join("\t") !== "role\tpid\tstart_identity\tcommand_identity" || lines.length !== 4) fail("external_recovery_process_proof", "owned process ledger is incomplete");
  const byRole = new Map(lines.slice(1).map(fields => [fields[0], fields]));
  const roles = ["tmux-server", "tmux-bootstrap", "herdr-launcher"];
  const identities = precheck.original_owned_process_identities;
  if (!Array.isArray(identities) || identities.length !== 4 || byRole.size !== 3) fail("external_recovery_process_proof", "retained process identities are incomplete");
  const pids = [oldOwner.shell_pid];
  for (const role of roles) {
    const identity = identities.find(item => item.role === role);
    const fields = byRole.get(role);
    if (!identity || !fields || fields.length !== 4 || Number(fields[1]) !== identity.pid || fields[2] !== (identity.startIdentity ?? identity.start_identity) || fields[3] !== (identity.commandIdentity ?? identity.command_identity) || identity.absent !== true) fail("external_recovery_process_proof", `retained process changed: ${role}`);
    pids.push(identity.pid);
  }
  if (command.input_identity.server_pid !== pids[1] || command.input_identity.bootstrap_pid !== pids[2] || command.input_identity.server_start_identity !== byRole.get("tmux-server")[2] || command.input_identity.bootstrap_start_identity !== byRole.get("tmux-bootstrap")[2]) fail("external_recovery_process_proof", "removal command does not bind the tmux processes");
  const herdrProof = JSON.parse(await readOwnedBytes(sources["herdr-cleanup-proof"], uid));
  const herdrResult = JSON.parse(await readOwnedBytes(sources["herdr-cleanup-result"], uid));
  const herdrIdentity = identities.find(item => item.role === "herdr-server");
  if (!herdrIdentity || herdrIdentity.pid !== herdrProof.server_process?.pid || herdrIdentity.start_identity !== herdrProof.server_process.start_identity || herdrIdentity.command_identity !== herdrProof.server_process.command || herdrIdentity.absent !== true) fail("external_recovery_process_proof", "retained Herdr server identity changed");
  pids.push(herdrIdentity.pid);
  if (herdrProof.stage_id !== owner.stage_id || herdrProof.run_id !== owner.run_id || herdrProof.attempt_id !== allocation.attempt_id || herdrProof.root !== root || herdrProof.launch_process?.pid !== pids[3] || herdrProof.server_process?.pid !== pids[4] || herdrProof.server_process.parent_pid !== pids[3] || herdrResult.ownership_proof_sha256 !== proof.source_hashes["herdr-cleanup-proof"].sha256 || herdrResult.stop_status !== 0 || herdrResult.process_absence_checks?.length !== 2 || herdrResult.socket_reinspection?.length !== 2 || herdrProof.sockets?.length !== 2 || herdrSockets.some(path => !herdrProof.sockets.some(socket => socket.path === path && socket.uid === uid && socket.mode === 0o600 && socket.listener_pids?.[0] === String(pids[4]))) || herdrSockets.some(path => !herdrResult.socket_reinspection.some(socket => socket.path === path && socket.node_absent === true && socket.listener_probe_status === 1 && socket.listener_pids?.length === 0))) fail("external_recovery_herdr_proof", "Herdr cleanup does not bind both private sockets and owned processes");
  for (const probes of [precheck.process_absence_probes, command.pre_removal_revalidation.processes, proof.post_cleanup_process_absence_probes]) {
    if (!Array.isArray(probes) || probes.length !== pids.length) fail("external_recovery_process_proof", "retained absence probes are incomplete");
    for (const pid of pids) retainedProcessAbsent(probes, pid);
  }
  for (const pid of pids) freshProcessAbsent(pid);
  for (const path of [socketPath, ...herdrSockets]) {
    if (await lstatOrNull(path)) fail("external_recovery_socket_live", `socket node is present: ${path}`);
    const probe = spawnSync("/usr/sbin/lsof", ["-n", "-t", path], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
    if (probe.error || probe.status !== 1 || probe.stdout.trim() || !probe.stderr.startsWith(`lsof: status error on ${path}: No such file or directory`)) fail("external_recovery_socket_live", `socket listener absence is ambiguous: ${path}`);
  }
  return { proof_sha256: proofHash, frozen_ledger_sha256: prefixHash, old_lock_sha256: oldOwnerHash, saved_owner_path: savedOwnerPath };
}

async function validateAttempt005Postcheck(owner, records, uid, proofPath) {
  if (!proofPath) return false;
  if (owner.stage_root !== final006Gate.stageRoot || owner.max_attempts !== 6 || owner.owner_uid !== uid || resolve(proofPath) !== final006Gate.postcheckPath) {
    fail("attempt006_postcheck_identity", "attempt-005 postcheck is not bound to the pinned final-006 stage");
  }
  const proofBytes = await readPinnedEvidenceBytes(proofPath, uid);
  if (digest(proofBytes) !== final006Gate.postcheckHash) fail("attempt006_postcheck_hash", "attempt-005 postcheck changed");
  for (const [name, hash] of Object.entries(final006Gate.evidenceFiles)) {
    if (digest(await readPinnedEvidenceBytes(join(dirname(proofPath), name), uid)) !== hash) fail("attempt006_postcheck_evidence", `attempt-005 postcheck evidence changed: ${name}`);
  }
  const proof = proofBytes.toString("utf8");
  for (const line of [
    "phase=attempt-005-stale-resource-recovery-postcheck",
    `ledger_sha256=${final006Gate.ledgerHash} bytes=22994 lines=12`,
    "records_11_and_12_preserved=verbatim",
    "active_lock_absent=yes",
    `receipt_path=${final006Gate.receiptPath} state=absent`,
    "attempt_root_regular_file_inventory_unchanged=yes",
    "attempt_root_paths_unchanged_except_exact_socket=yes",
    "missing_pi_process_identity=not reconstructed",
    "ledger outcome remains result=blocked cleanup_complete=false",
    "no process was killed; no server/Pi/test was launched; no attempt was allocated",
  ]) {
    if (!proof.includes(line)) fail("attempt006_postcheck_content", `attempt-005 postcheck is missing: ${line}`);
  }
  const processScans = proof.split("\n").filter(line => line.startsWith("process_command_scan_"));
  if (processScans.length !== 2 || processScans.some(line => !line.endsWith('"matches":[]}'))) fail("attempt006_postcheck_processes", "attempt-005 process scans do not prove no matching process before and after recovery");
  const ledgerBytes = await readOwnedBytes(join(owner.stage_root, "allocation-ledger.jsonl"), uid);
  const ledgerLines = ledgerBytes.toString("utf8").split("\n");
  if (ledgerLines.at(-1) !== "" || ledgerLines.length < final006Gate.ledgerRecords + 1) fail("attempt006_ledger_prefix", "frozen attempt-005 ledger prefix is incomplete");
  const frozenPrefix = Buffer.from(`${ledgerLines.slice(0, final006Gate.ledgerRecords).join("\n")}\n`);
  if (digest(frozenPrefix) !== final006Gate.ledgerHash || ![12, 13, 14].includes(records.length)) fail("attempt006_ledger_prefix", "frozen attempt-005 ledger prefix changed or has an unexpected extension");
  if (records.length === final006Gate.ledgerRecords && digest(ledgerBytes) !== final006Gate.ledgerHash) fail("attempt006_ledger_hash", "the frozen twelve-record ledger changed");
  if (records.length >= 13) {
    const allocation = records[12];
    if (allocation.kind !== "allocation" || allocation.seq !== 13 || allocation.attempt_number !== 6 || allocation.attempt_id !== "attempt-006" || allocation.root_path !== rootPath(owner, 6)) fail("attempt006_ledger_extension", "ledger extension is not the sole final-006 allocation");
  }
  if (records.length === 14 && (records[13].kind !== "outcome" || records[13].seq !== 14 || records[13].attempt_id !== "attempt-006")) fail("attempt006_ledger_extension", "ledger extension contains a non-final-006 outcome");
  return true;
}

async function parseStage(stageRoot, expected, uid, { allowUnmarkedRootId = "", allowUnrecoveredCleanup = false, externalRecoveryProofPath = "", attempt005PostcheckPath = "" } = {}) {
  const owner = await checkOwner(stageRoot, expected, uid);
  await validateStageNamespace(owner, uid);
  const predecessor = owner.successor ? await validatePredecessor(owner, uid) : null;
  const observations = await readJsonLines(join(stageRoot, "pre-stage-observations.jsonl"), uid);
  if (observations.length !== 2 || new Set(observations.map(event => event.event_id)).size !== 2) fail("observation_history_mismatch", "pre-stage observations are missing or duplicated");
  for (const event of observations) {
    if (!observationIds.has(event.event_id) || event.stage_id !== owner.stage_id || event.run_id !== owner.run_id || event.allocated_attempt !== false) {
      fail("observation_history_mismatch", "pre-stage observation ownership mismatch");
    }
  }
  const records = await readJsonLines(join(stageRoot, "allocation-ledger.jsonl"), uid);
  if (records.length > 32) fail("ledger_limit", "stage allocation ledger has too many records");
  const allocations = [];
  const outcomesById = new Map();
  const cleanupRecoveries = new Map();
  const refusals = [];
  for (let index = 0; index < records.length; index++) {
    const record = records[index];
    if (record.seq !== index + 1 || record.stage_id !== owner.stage_id || record.run_id !== owner.run_id) {
      fail("invalid_ledger_sequence", `allocation ledger sequence or identity mismatch at record ${index + 1}`);
    }
    if (record.kind === "allocation") {
      const number = allocations.length + 1;
      const id = attemptId(number);
      if (number > allocationLimit(owner) || record.attempt_number !== number || record.attempt_id !== id || record.root_path !== rootPath(owner, number) || (predecessor && (record.local_allocation_count !== number || record.cumulative_allocation_count !== 6 + number))) {
        fail("invalid_allocation", `unknown, duplicate, or out-of-order allocation at record ${index + 1}`);
      }
      if (allocations.some(item => item.attempt_id === record.attempt_id)) fail("duplicate_allocation", `duplicate allocation: ${record.attempt_id}`);
      allocations.push(record);
      continue;
    }
    if (record.kind === "outcome") {
      if (!allocations.some(item => item.attempt_id === record.attempt_id) || outcomesById.has(record.attempt_id) || !outcomes.has(record.result) || typeof record.cleanup_complete !== "boolean" || typeof record.root_created !== "boolean") {
        fail("invalid_outcome", `unknown or duplicate attempt outcome: ${record.attempt_id}`);
      }
      outcomesById.set(record.attempt_id, record);
      continue;
    }
    if (record.kind === "cleanup-recovery") {
      const outcome = outcomesById.get(record.attempt_id);
      if (!allocations.some(item => item.attempt_id === record.attempt_id) || !outcome || cleanupRecoveries.has(record.attempt_id) || record.original_outcome_seq !== outcome.seq || !Array.isArray(record.source_files) || !Array.isArray(record.observations)) {
        fail("invalid_cleanup_recovery", `unknown, duplicate, or out-of-order cleanup recovery: ${record.attempt_id}`);
      }
      cleanupRecoveries.set(record.attempt_id, record);
      continue;
    }
    if (record.kind === "refusal") {
      if (!refusalReasons.has(record.reason) || typeof record.requested_id !== "string") fail("invalid_refusal", `invalid allocation refusal at record ${index + 1}`);
      refusals.push(record);
      continue;
    }
    fail("unknown_ledger_record", `unknown allocation ledger record kind at ${index + 1}`);
  }
  for (const allocation of allocations) {
    const recovery = cleanupRecoveries.get(allocation.attempt_id);
    if (recovery) await validateCleanupRecovery(owner, allocation, outcomesById.get(allocation.attempt_id), recovery, uid);
  }
  const externalRecovery = externalRecoveryProofPath
    ? await validateAttempt003Proof(owner, allocations, outcomesById, records, uid, externalRecoveryProofPath)
    : null;
  const attempt005Postcheck = await validateAttempt005Postcheck(owner, records, uid, attempt005PostcheckPath);
  const recovered = id => cleanupRecoveries.has(id) || (id === "attempt-003" && externalRecovery !== null) || (id === "attempt-005" && attempt005Postcheck);
  const attemptsPath = join(stageRoot, "attempts");
  const attemptEntries = new Set(await readdir(attemptsPath));
  const hasActiveLock = Boolean(await lstatOrNull(join(stageRoot, "active.lock")));
  for (const allocation of allocations) {
    const expectedId = allocation.attempt_id;
    const attemptPath = join(attemptsPath, expectedId);
    const attemptStat = await lstatOrNull(attemptPath);
    assertDirectory(attemptStat, attemptPath, uid);
    attemptEntries.delete(expectedId);
    const metadataPath = join(attemptPath, "attempt-owner.json");
    const metadata = await readOwnedJson(metadataPath, uid);
    if (metadata.stage_id !== owner.stage_id || metadata.run_id !== owner.run_id || metadata.attempt_id !== expectedId || metadata.root_path !== allocation.root_path) {
      fail("attempt_owner_mismatch", `attempt ownership mismatch: ${expectedId}`);
    }
    const attemptFiles = new Set(await readdir(attemptPath));
    if (!attemptFiles.has("attempt-owner.json")) fail("attempt_history_incomplete", `missing owner record: ${expectedId}`);
    const rootCreatedPath = join(attemptPath, "root-created.json");
    const rootCreatedExists = attemptFiles.has("root-created.json");
    for (const name of attemptFiles) if (!["attempt-owner.json", "root-created.json"].includes(name)) fail("unknown_attempt_entry", `unknown attempt entry for ${expectedId}: ${name}`);
    const rootStat = await lstatOrNull(allocation.root_path);
    const outcome = outcomesById.get(expectedId);
    if (rootCreatedExists) {
      const rootRecord = await readOwnedJson(rootCreatedPath, uid);
      if (rootRecord.stage_id !== owner.stage_id || rootRecord.run_id !== owner.run_id || rootRecord.attempt_id !== expectedId || rootRecord.root_path !== allocation.root_path) {
        fail("root_record_mismatch", `fixture root record mismatch: ${expectedId}`);
      }
      if (!rootStat) fail("root_history_mismatch", `recorded fixture root is missing: ${expectedId}`);
      assertDirectory(rootStat, allocation.root_path, uid);
      await validateRootMarker(owner, allocation, uid);
      if (outcome && (!outcome.root_created || (!outcome.cleanup_complete && !recovered(expectedId) && !allowUnrecoveredCleanup))) fail("unsafe_prior_outcome", `prior fixture attempt did not prove complete cleanup: ${expectedId}`);
      if (!outcome && !hasActiveLock) fail("unresolved_fixture_root", `fixture root has no terminal record or active stage lock: ${expectedId}`);
    } else {
      if (rootStat && expectedId !== allowUnmarkedRootId) fail("unrecorded_fixture_root", `fixture root exists without its creation record: ${expectedId}`);
      if (rootStat && !hasActiveLock) fail("unresolved_fixture_root", `unmarked fixture root has no active stage lock: ${expectedId}`);
      if (rootStat && outcome && !outcome.root_created) fail("root_history_mismatch", `outcome does not record the existing fixture root: ${expectedId}`);
      if (outcome?.root_created) fail("root_history_mismatch", `outcome records an absent fixture root: ${expectedId}`);
      if (outcome && !outcome.cleanup_complete && !recovered(expectedId) && !allowUnrecoveredCleanup) fail("unsafe_prior_outcome", `prior allocation did not prove cleanup: ${expectedId}`);
    }
  }
  if (attemptEntries.size > 0) fail("unknown_attempt_directory", `unrecorded attempt directory: ${[...attemptEntries][0]}`);
  for (let number = allocations.length + 1; number <= owner.max_attempts; number++) {
    const path = rootPath(owner, number);
    if (await lstatOrNull(path)) fail("unrecorded_fixture_root", `fixture root exists without an allocation record: ${path}`);
  }
  for (const [id, outcome] of outcomesById) {
    if (!outcome.cleanup_complete && !recovered(id) && !allowUnrecoveredCleanup) fail("unsafe_prior_outcome", `prior allocation did not prove cleanup: ${id}`);
  }
  return { owner, predecessor, observations, records, allocations, outcomes: outcomesById, cleanupRecoveries, externalRecovery, attempt005Postcheck, refusals };
}

async function frozenAttempt005Ledger(stageRoot, uid) {
  if (stageRoot !== attempt005Gate.stageRoot) return false;
  return digest(await readOwnedBytes(join(stageRoot, "allocation-ledger.jsonl"), uid)) === attempt005Gate.ledgerHash;
}

async function assertAttempt005Gate(owner, state, requestedId, uid) {
  if (owner.stage_root !== attempt005Gate.stageRoot) fail("attempt005_gate_stage", "attempt-005 is gated to its pinned stage");
  if (requestedId !== "attempt-005") fail("attempt005_gate_request", "only attempt-005 may be requested in the frozen state");
  if (state.records.length !== attempt005Gate.ledgerRecords || !(await frozenAttempt005Ledger(owner.stage_root, uid))) fail("attempt005_gate_ledger", "allocation ledger is not the frozen ten-record state");
  if (!state.externalRecovery || state.externalRecovery.old_lock_sha256 !== attempt003Anchor.oldLockHash) fail("attempt005_gate_recovery", "pinned attempt-003 proof is not in force");
  if (digest(await readOwnedBytes(state.externalRecovery.saved_owner_path, uid)) !== attempt003Anchor.oldLockHash) fail("attempt005_gate_owner", "archived attempt-003 lock owner differs");
  const allocation = state.allocations[3];
  const outcome = state.outcomes.get("attempt-004");
  if (state.allocations.length !== 4 || allocation?.attempt_id !== "attempt-004" || allocation.seq !== 9 || allocation.root_path !== rootPath(owner, 4) || outcome?.seq !== 10 || outcome.result !== "blocked" || outcome.root_created !== true || outcome.cleanup_complete !== true) fail("attempt005_gate_attempt004", "attempt-004 is not the recorded blocked outcome with complete cleanup");
  for (const [name, hash] of Object.entries(attempt005Gate.attempt004Evidence)) {
    if (digest(await readOwnedBytes(join(allocation.root_path, name), uid)) !== hash) fail("attempt005_gate_evidence", `attempt-004 cleanup evidence changed: ${name}`);
  }
  const processes = (await readOwnedBytes(join(allocation.root_path, "owned-processes.tsv"), uid)).toString("utf8").trimEnd().split("\n").slice(1).map(row => row.split("\t"));
  if (processes.length !== 4) fail("attempt005_gate_evidence", "attempt-004 owned process ledger is incomplete");
  for (const [, pid, startIdentity] of processes) requireNoLiveIdentity(Number(pid), startIdentity);
  for (const socket of [join(allocation.root_path, "tmux", `tmux-${uid}`, "default"), join(allocation.root_path, "config", "herdr", "herdr.sock"), join(allocation.root_path, "config", "herdr", "herdr-client.sock")]) {
    if (await lstatOrNull(socket)) fail("attempt005_gate_socket", `attempt-004 socket node exists: ${socket}`);
    const probe = spawnSync("/usr/sbin/lsof", ["-n", "-t", socket], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
    if (probe.error || probe.status !== 1 || probe.stdout.trim() || !probe.stderr.startsWith(`lsof: status error on ${socket}: No such file or directory`)) fail("attempt005_gate_socket_listener", `attempt-004 socket listener absence is ambiguous: ${socket}`);
  }
  for (const path of [rootPath(owner, 5), join(owner.stage_root, "attempts", "attempt-005"), attempt005Gate.receiptPath]) {
    if (await lstatOrNull(path)) fail("attempt005_gate_absent", `attempt-005 state already exists: ${path}`);
  }
}

async function assertFinal006Gate(owner, state, requestedId, uid) {
  const outcome = state.outcomes.get("attempt-005");
  if (owner.stage_root !== final006Gate.stageRoot || owner.max_attempts !== 6) fail("attempt006_gate_stage", "attempt-006 is pinned to its six-attempt stage");
  if (requestedId !== "attempt-006") fail("attempt006_gate_request", "only attempt-006 may be requested from the recovered attempt-005 state");
  if (!state.attempt005Postcheck || state.records.length !== 12 || state.allocations.length !== 5 || !state.externalRecovery) fail("attempt006_gate_history", "attempt-005 postcheck, external recovery, and exact twelve-record history are required");
  if (state.allocations[4]?.attempt_id !== "attempt-005" || state.allocations[4].seq !== 11 || outcome?.seq !== 12 || outcome.result !== "blocked" || outcome.root_created !== true || outcome.cleanup_complete !== false || state.cleanupRecoveries.has("attempt-005")) fail("attempt006_gate_attempt005", "attempt-005 blocked outcome and cleanup state must remain unchanged");
  if (digest(await readOwnedBytes(join(owner.stage_root, "allocation-ledger.jsonl"), uid)) !== final006Gate.ledgerHash) fail("attempt006_gate_ledger", "the frozen twelve-record attempt-005 ledger changed");
  const attemptRoot = rootPath(owner, 5);
  requireNoProcessCommand(attemptRoot, uid);
  const processPath = join(attemptRoot, "owned-processes.tsv");
  const processBytes = await readOwnedBytes(processPath, uid);
  if (digest(processBytes) !== final006Gate.attempt005ProcessesHash) fail("attempt006_gate_processes", "attempt-005 owned-process evidence changed");
  const processRows = processBytes.toString("utf8").trimEnd().split("\n");
  if (processRows[0] !== "role\tpid\tstart_identity\tcommand_identity" || processRows.length !== 5) fail("attempt006_gate_processes", "attempt-005 process identities are incomplete or ambiguous");
  for (const row of processRows.slice(1)) {
    const [role, pidText, startIdentity, commandIdentity, ...extra] = row.split("\t");
    if (extra.length || !["tmux-server", "tmux-bootstrap", "herdr-launcher", "herdr-server"].includes(role) || !/^\d+$/.test(pidText) || !startIdentity || !commandIdentity) fail("attempt006_gate_processes", "attempt-005 captured process identity is malformed");
    requireNoLiveIdentity(Number(pidText), startIdentity);
  }
  for (const socket of [join(attemptRoot, "tmux", `tmux-${uid}`, "default"), join(attemptRoot, "config", "herdr", "herdr.sock"), join(attemptRoot, "config", "herdr", "herdr-client.sock")]) {
    if (await lstatOrNull(socket)) fail("attempt006_gate_socket", `attempt-005 socket node exists: ${socket}`);
    const probe = spawnSync("/usr/sbin/lsof", ["-n", "-t", socket], { encoding: "utf8", env: { ...process.env, LC_ALL: "C" } });
    if (probe.error || probe.status !== 1 || probe.stdout.trim() || !probe.stderr.startsWith(`lsof: status error on ${socket}: No such file or directory`)) fail("attempt006_gate_socket_listener", `attempt-005 socket listener absence is ambiguous: ${socket}`);
  }
  for (const path of [rootPath(owner, 6), join(owner.stage_root, "attempts", "attempt-006"), final006Gate.receiptPath]) {
    if (await lstatOrNull(path)) fail("attempt006_gate_absent", `final attempt state already exists: ${path}`);
  }
}

async function lockData(stageRoot, token, uid) {
  const lockPath = join(stageRoot, "active.lock");
  assertDirectory(await lstatOrNull(lockPath), lockPath, uid);
  const data = await readOwnedJson(join(lockPath, "owner.json"), uid);
  if (data.token !== token) fail("stage_locked", "stage is locked by another invocation");
  return { lockPath, data };
}

export async function initializeStage(stageRoot, { stageId, runId, uid = process.getuid(), fixtureRootParent, successor, allowUnrecoveredCleanup = false, externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  if (fixtureRootParent === undefined) fixtureRootParent = await realpath("/tmp");
  const root = resolve(stageRoot);
  const parent = dirname(root);
  const expected = expectedOwner(root, { stageId, runId, uid, fixtureRootParent, successor });
  await validateStageNamespace(expected, uid);
  if (successor) await validatePredecessor(expected, uid);
  const existing = await lstatOrNull(root);
  if (!existing && attempt005PostcheckPath) fail("attempt006_stage_missing", "proof-bound final-attempt admission cannot create or initialize stage history");
  if (!existing) {
    const parentReal = await realpath(parent);
    if (parentReal !== parent) fail("unsafe_stage_parent", `stage parent is not canonical: ${parent}`);
    const parentStat = await lstat(parent);
    if (parentStat.isSymbolicLink() || !parentStat.isDirectory() || (parentStat.uid !== uid && (parentStat.mode & 0o1000) === 0)) fail("unsafe_stage_parent", `stage parent is not a private owner directory or sticky system temp directory: ${parent}`);
    for (let number = 1; number <= expected.max_attempts; number++) await assertFixtureRootAbsent(rootPath(expected, number));
    await mkdir(root, { mode: 0o700 });
    const now = new Date().toISOString();
    await writeExclusive(join(root, "stage-owner.json"), `${JSON.stringify({ ...expected, created_at_utc: now })}\n`);
    await mkdir(join(root, "attempts"), { mode: 0o700 });
    await writeExclusive(join(root, "allocation-ledger.jsonl"), "");
    const observations = observationRecords(stageId, runId, now);
    await writeExclusive(join(root, "pre-stage-observations.jsonl"), `${observations.map(event => JSON.stringify(event)).join("\n")}\n`);
  }
  return await parseStage(root, expected, uid, { allowUnrecoveredCleanup, externalRecoveryProofPath, attempt005PostcheckPath });
}

export async function acquireStageLock(stageRoot, { token, runId, shellPid, shellStart, shellCommand, uid = process.getuid() }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  if (owner.run_id !== runId) fail("stage_identity_mismatch", "lock run identity does not match stage owner");
  const lockPath = join(root, "active.lock");
  try {
    await mkdir(lockPath, { mode: 0o700 });
  } catch (error) {
    if (error.code === "EEXIST") fail("stage_locked", "stage already has an active or unresolved lock");
    throw error;
  }
  await writeExclusive(join(lockPath, "owner.json"), `${JSON.stringify({ token, run_id: runId, shell_pid: shellPid, shell_start: shellStart, shell_command: shellCommand, created_at_utc: new Date().toISOString() })}\n`);
  return { locked: true, shell_pid: shellPid };
}

export async function recordCleanupRecovery(stageRoot, { attemptId: id = "attempt-001", stageId, runId, uid = process.getuid(), dryRun = false }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  if (owner.stage_id !== stageId || owner.run_id !== runId) fail("stage_identity_mismatch", "cleanup recovery identity does not match the stage owner");
  const state = await parseStage(root, { ...owner }, uid, { allowUnrecoveredCleanup: true });
  const lockPath = join(root, "active.lock");
  assertDirectory(await lstatOrNull(lockPath), lockPath, uid);
  const lockEntries = await readdir(lockPath);
  if (lockEntries.length !== 1 || lockEntries[0] !== "owner.json") fail("lock_history_mismatch", "stage lock contains unknown entries");
  const lockOwner = await readOwnedJson(join(lockPath, "owner.json"), uid);
  if (lockOwner.run_id !== runId || lockOwner.shell_command !== "pi-scroll-qualification.sh" || !Number.isSafeInteger(lockOwner.shell_pid) || !lockOwner.shell_start) fail("lock_owner_mismatch", "stale stage lock identity is incomplete or unexpected");
  requireNoLiveIdentity(lockOwner.shell_pid, lockOwner.shell_start);
  const allocation = state.allocations.find(item => item.attempt_id === id);
  const outcome = state.outcomes.get(id);
  if (!["attempt-001", "attempt-002"].includes(id) || !allocation || !outcome || outcome.result !== "blocked" || outcome.root_created !== true || outcome.cleanup_complete !== false) {
    fail("cleanup_recovery_not_applicable", `no recoverable cleanup failure exists for ${id}`);
  }
  const existing = state.cleanupRecoveries.get(id);
  if (existing) return { recorded: false, existing: true, attempt_id: id, record_seq: existing.seq };
  const evidence = id === "attempt-001"
    ? await readCleanupRecoverySources(owner, allocation, uid)
    : await readAttempt002CleanupRecoverySources(owner, allocation, uid);
  const socketPath = evidence.socket_path ?? join(allocation.root_path, "tmux", `tmux-${uid}`, "default");
  if (await lstatOrNull(socketPath)) fail("cleanup_socket_node_present", `stale tmux socket node still exists: ${socketPath}`);
  const serverPid = evidence.server_pid ?? evidence.server.server_pid;
  const serverStartIdentity = evidence.server_start_identity ?? evidence.server.server_start_identity;
  const bootstrapPid = evidence.bootstrap_pid ?? evidence.server.pane_pid;
  const bootstrapStartIdentity = evidence.bootstrap_start_identity ?? evidence.server.pane_start_identity;
  requireNoLiveIdentity(serverPid, serverStartIdentity);
  requireNoLiveIdentity(bootstrapPid, bootstrapStartIdentity);
  const record = {
    seq: state.records.length + 1,
    stage_id: owner.stage_id,
    run_id: owner.run_id,
    kind: "cleanup-recovery",
    attempt_id: id,
    original_outcome_seq: outcome.seq,
    recorded_at_utc: new Date().toISOString(),
    observations: evidence.observations,
    source_files: evidence.files,
  };
  await validateCleanupRecovery(owner, allocation, outcome, record, uid);
  if (dryRun) return { dry_run: true, record };
  await appendExclusiveLedger(join(root, "allocation-ledger.jsonl"), `${JSON.stringify(record)}\n`);
  await parseStage(root, { ...owner }, uid);
  return { recorded: true, existing: false, attempt_id: id, record_seq: record.seq };
}

export async function claimStageLock(stageRoot, { token, runId, shellPid, shellStart, shellCommand, uid = process.getuid(), externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  if (owner.run_id !== runId) fail("stage_identity_mismatch", "lock run identity does not match stage owner");
  const state = await parseStage(root, { ...owner }, uid, { externalRecoveryProofPath, attempt005PostcheckPath });
  const lockPath = join(root, "active.lock");
  const lockStat = await lstatOrNull(lockPath);
  if (state.attempt005Postcheck) {
    if (lockStat) fail("attempt006_gate_lock", "final-attempt admission requires the stage lock to be absent");
    await assertFinal006Gate(owner, state, "attempt-006", uid);
    return await acquireStageLock(root, { token, runId, shellPid, shellStart, shellCommand, uid });
  }
  if (!lockStat) return await acquireStageLock(root, { token, runId, shellPid, shellStart, shellCommand, uid });
  if (await frozenAttempt005Ledger(root, uid)) fail("attempt005_gate_lock", "the frozen attempt-005 state requires no pre-existing stage lock");
  assertDirectory(lockStat, lockPath, uid);
  const entries = await readdir(lockPath);
  if (entries.length !== 1 || entries[0] !== "owner.json") fail("lock_history_mismatch", "stage lock contains unknown entries");
  const previous = await readOwnedJson(join(lockPath, "owner.json"), uid);
  if (previous.run_id !== runId || previous.shell_command !== "pi-scroll-qualification.sh" || !Number.isSafeInteger(previous.shell_pid) || !previous.shell_start) fail("lock_owner_mismatch", "existing stage lock identity is incomplete or unexpected");
  if (!state.externalRecovery && ![...state.cleanupRecoveries.keys()].some(id => state.outcomes.get(id)?.cleanup_complete === false)) fail("stage_locked", "existing stage lock has no verified cleanup recovery record");
  const oldOwnerBytes = await readOwnedBytes(join(lockPath, "owner.json"), uid);
  if (state.externalRecovery && digest(oldOwnerBytes) !== state.externalRecovery.old_lock_sha256) fail("external_recovery_lock", "current lock is not the retained attempt-003 owner");
  requireNoLiveIdentity(previous.shell_pid, previous.shell_start);
  if (state.externalRecovery) {
    const saved = await lstatOrNull(state.externalRecovery.saved_owner_path);
    if (saved) {
      if (digest(await readOwnedBytes(state.externalRecovery.saved_owner_path, uid)) !== digest(oldOwnerBytes)) fail("external_recovery_lock", "archived lock owner differs from the original");
    } else {
      await writeExclusive(state.externalRecovery.saved_owner_path, oldOwnerBytes);
    }
    if (digest(await readOwnedBytes(state.externalRecovery.saved_owner_path, uid)) !== state.externalRecovery.old_lock_sha256) fail("external_recovery_lock", "original lock owner was not preserved");
  }
  const newOwner = { token, run_id: runId, shell_pid: shellPid, shell_start: shellStart, shell_command: shellCommand, created_at_utc: new Date().toISOString() };
  const ownerPath = join(lockPath, "owner.json");
  const temporaryOwnerPath = join(lockPath, `.owner-${token}.tmp`);
  await writeExclusive(temporaryOwnerPath, `${JSON.stringify(newOwner)}\n`);
  await rename(temporaryOwnerPath, ownerPath);
  return { locked: true, recovered: true, previous_shell_pid: previous.shell_pid, shell_pid: shellPid };
}

export async function allocateAttempt(stageRoot, { token, requestedId, uid = process.getuid(), externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  await lockData(root, token, uid);
  const state = await parseStage(root, { ...owner }, uid, { externalRecoveryProofPath, attempt005PostcheckPath });
  const nextNumber = state.allocations.length + 1;
  if (state.attempt005Postcheck) {
    if (nextNumber !== 6) fail("attempt006_gate_history", "attempt-005 postcheck authorizes only the final attempt-006 allocation");
    await assertFinal006Gate(owner, state, requestedId, uid);
  } else if (state.externalRecovery && nextNumber === 5) await assertAttempt005Gate(owner, state, requestedId, uid);
  else if (state.externalRecovery && nextNumber !== 4) fail("external_recovery_attempt_limit", "attempt-003 proof authorizes only attempt-004 and the gated attempt-005");
  const expectedId = attemptId(nextNumber);
  let refusal;
  if (nextNumber > allocationLimit(owner)) refusal = "attempt-limit";
  else if (requestedId !== expectedId && state.allocations.some(item => item.attempt_id === requestedId)) refusal = "duplicate-attempt-id";
  else if (requestedId !== expectedId) refusal = "unknown-attempt-id";
  const targetRoot = nextNumber <= allocationLimit(owner) ? rootPath(owner, nextNumber) : "";
  if (!refusal && await lstatOrNull(targetRoot)) refusal = "root-path-collision";
  const targetAttempt = nextNumber <= allocationLimit(owner) ? join(root, "attempts", expectedId) : "";
  if (!refusal && await lstatOrNull(targetAttempt)) refusal = "attempt-directory-collision";
  if (refusal) {
    const record = { seq: state.records.length + 1, stage_id: owner.stage_id, run_id: owner.run_id, kind: "refusal", reason: refusal, requested_id: requestedId, observed_at_utc: new Date().toISOString() };
    await appendExclusiveLedger(join(root, "allocation-ledger.jsonl"), `${JSON.stringify(record)}\n`);
    fail(refusal, `attempt allocation refused: ${refusal}`);
  }
  const allocation = { seq: state.records.length + 1, stage_id: owner.stage_id, run_id: owner.run_id, kind: "allocation", attempt_number: nextNumber, attempt_id: expectedId, root_path: targetRoot, allocated_at_utc: new Date().toISOString(), ...(state.predecessor ? { local_allocation_count: nextNumber, cumulative_allocation_count: 6 + nextNumber } : {}) };
  await appendExclusiveLedger(join(root, "allocation-ledger.jsonl"), `${JSON.stringify(allocation)}\n`);
  await mkdir(targetAttempt, { mode: 0o700 });
  await writeExclusive(join(targetAttempt, "attempt-owner.json"), `${JSON.stringify({ stage_id: owner.stage_id, run_id: owner.run_id, attempt_id: expectedId, root_path: targetRoot, allocated_at_utc: allocation.allocated_at_utc })}\n`);
  return allocation;
}

export async function markRootCreated(stageRoot, { token, attemptId: id, uid = process.getuid(), externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  await lockData(root, token, uid);
  const state = await parseStage(root, { ...owner }, uid, { allowUnmarkedRootId: id, externalRecoveryProofPath, attempt005PostcheckPath });
  const allocation = state.allocations.find(item => item.attempt_id === id);
  if (!allocation) fail("unknown_attempt_id", `unknown attempt: ${id}`);
  const attemptPath = join(root, "attempts", id);
  const rootStat = await lstatOrNull(allocation.root_path);
  assertDirectory(rootStat, allocation.root_path, uid);
  await validateRootMarker(owner, allocation, uid);
  const path = join(attemptPath, "root-created.json");
  if (await lstatOrNull(path)) fail("duplicate_root_record", `root creation already recorded: ${id}`);
  await writeExclusive(path, `${JSON.stringify({ stage_id: owner.stage_id, run_id: owner.run_id, attempt_id: id, root_path: allocation.root_path, created_at_utc: new Date().toISOString() })}\n`);
  return { attempt_id: id, root_path: allocation.root_path };
}

export async function recordAttemptOutcome(stageRoot, { token, attemptId: id, result, rootCreated, cleanupComplete, failureReason = "", uid = process.getuid(), externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  await lockData(root, token, uid);
  const state = await parseStage(root, { ...owner }, uid, { allowUnmarkedRootId: id, externalRecoveryProofPath, attempt005PostcheckPath });
  const allocation = state.allocations.find(item => item.attempt_id === id);
  if (!allocation) fail("unknown_attempt_id", `unknown attempt outcome: ${id}`);
  if (state.outcomes.has(id)) fail("duplicate_outcome", `attempt already has a terminal outcome: ${id}`);
  if (!outcomes.has(result) || typeof rootCreated !== "boolean" || typeof cleanupComplete !== "boolean") fail("invalid_outcome", "attempt outcome fields are invalid");
  const rootExists = Boolean(await lstatOrNull(allocation.root_path));
  const rootRecordExists = Boolean(await lstatOrNull(join(root, "attempts", id, "root-created.json")));
  if (rootExists !== rootCreated || rootRecordExists !== rootCreated) fail("root_outcome_mismatch", `root creation outcome does not match recorded fixture state: ${id}`);
  const record = { seq: state.records.length + 1, stage_id: owner.stage_id, run_id: owner.run_id, kind: "outcome", attempt_id: id, result, root_created: rootCreated, cleanup_complete: cleanupComplete, failure_reason: failureReason, recorded_at_utc: new Date().toISOString() };
  await appendExclusiveLedger(join(root, "allocation-ledger.jsonl"), `${JSON.stringify(record)}\n`);
  return record;
}

export async function stageSummary(stageRoot, { uid = process.getuid(), allowUnrecoveredCleanup = false, externalRecoveryProofPath = "", attempt005PostcheckPath = "" }) {
  const root = resolve(stageRoot);
  const owner = await readOwnedJson(join(root, "stage-owner.json"), uid);
  const state = await parseStage(root, { ...owner }, uid, { allowUnrecoveredCleanup, externalRecoveryProofPath, attempt005PostcheckPath });
  return {
    stage_id: owner.stage_id,
    run_id: owner.run_id,
    stage_root: root,
    max_attempts: allocationLimit(owner),
    namespace_slots: owner.max_attempts,
    local_allocation_count: state.allocations.length,
    predecessor_allocation_count: state.predecessor ? 6 : 0,
    cumulative_allocation_count: (state.predecessor ? 6 : 0) + state.allocations.length,
    predecessor: state.predecessor,
    allocations: state.allocations.map(allocation => ({ ...allocation, outcome: state.outcomes.get(allocation.attempt_id) ?? null })),
    allocation_count: state.allocations.length,
    refusal_count: state.refusals.length,
    refusals: state.refusals,
    cleanup_recoveries: [...state.cleanupRecoveries.values()],
    external_recovery: state.externalRecovery,
    pre_stage_observations: state.observations,
    ledger_record_count: state.records.length,
  };
}

export async function verifyLiveFixtureContract(input) {
  const contract = await validateFixtureContract(input);
  const { verifyCurrentHerdrProof } = await import("./pi-scroll-qualification-herdr-proof.mjs");
  const { verifyCurrentTmuxProof } = await import("./pi-scroll-qualification-tmux-proof.mjs");
  for (const proof of [input.tmuxProof, input.herdrProof]) {
    if (!proof || proof.root !== input.root || proof.stageRoot !== input.stageRoot || proof.stageId !== input.stageId || proof.runId !== input.runId || proof.attemptId !== input.attemptId || proof.uid !== input.uid || proof.lockToken !== input.lockToken) fail("fixture_identity", "live proof does not bind the fixture contract");
  }
  await verifyCurrentTmuxProof(input.tmuxProof);
  await verifyCurrentHerdrProof(input.herdrProof);
  return contract;
}

export async function releaseStageLock(stageRoot, { token, uid = process.getuid() }) {
  const root = resolve(stageRoot);
  const { lockPath } = await lockData(root, token, uid);
  const entries = await readdir(lockPath);
  if (entries.length !== 1 || entries[0] !== "owner.json") fail("lock_history_mismatch", "stage lock contains unknown entries");
  await unlink(join(lockPath, "owner.json"));
  await rmdir(lockPath);
  return { released: true };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv[2] === "fixture") {
      const input = await Bun.stdin.json();
      process.stdout.write(`${JSON.stringify(await verifyLiveFixtureContract(input))}\n`);
    } else if (process.argv[2] === "successor-config") {
      process.stdout.write(`${JSON.stringify(await readSuccessorConfiguration(process.argv[3]))}\n`);
    } else fail("invalid_action", "unsupported qualification contract action");
  } catch (error) {
    process.stderr.write(`${JSON.stringify({ code: error.code ?? "contract_error", message: error.message })}\n`);
    process.exitCode = 1;
  }
}

import { createHash } from "node:crypto";
import { open, lstat, readFile, realpath } from "node:fs/promises";
import { join } from "node:path";

function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requireRecord(value, name) {
  if (!isRecord(value)) throw new Error(`${name} must be an object`);
  return value;
}

function requireString(value, name) {
  if (typeof value !== "string" || value.length === 0) throw new Error(`${name} is missing`);
  return value;
}

function requirePositiveInteger(value, name) {
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${name} is invalid`);
  return value;
}

function requireNonNegativeInteger(value, name) {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`${name} is invalid`);
  return value;
}

export function requireLaunchArgv(argv, bridgePath, incarnation) {
  if (![1, 2].includes(incarnation)) throw new Error("Pi incarnation must be exactly 1 or 2");
  const expected = incarnation === 1
    ? ["pi", "--offline", "--tui-mode", "regular", "--extension", bridgePath]
    : ["pi", "--offline", "--extension", bridgePath];
  if (!Array.isArray(argv) || JSON.stringify(argv) !== JSON.stringify(expected)) {
    throw new Error("Pi launch argv does not match its exact configured incarnation");
  }
  return argv;
}

function currentIncarnation() {
  const value = process.env.HERDR_P1_PI_INCARNATION;
  if (value !== "1" && value !== "2") throw new Error("missing exact Pi incarnation");
  return Number(value);
}

function piEvidenceName(name) {
  return `pi-${currentIncarnation()}-${name}`;
}

function requirePaneIdentity(agent, expected, source) {
  if (agent.pane_id !== expected.paneId) throw new Error(`${source} pane id mismatch`);
  if (agent.terminal_id !== expected.terminalId) throw new Error(`${source} terminal id mismatch`);
  if (agent.workspace_id !== expected.workspaceId) throw new Error(`${source} workspace id mismatch`);
  if (agent.cwd !== expected.cwd || agent.foreground_cwd !== expected.cwd) {
    throw new Error(`${source} cwd mismatch`);
  }
  if (agent.agent !== "pi") throw new Error(`${source} agent kind mismatch`);
  return agent;
}

export function decodeProcessInfo(envelope, expected) {
  const result = requireRecord(envelope?.result, "process-info result");
  if (result.type !== "pane_process_info") throw new Error("process-info result type is unsupported");
  const info = requireRecord(result.process_info, "process-info payload");
  if (info.pane_id !== expected.paneId) throw new Error("process-info pane id mismatch");
  if (!Array.isArray(info.foreground_processes) || info.foreground_processes.length !== 1) {
    throw new Error("foreground process identity is missing or ambiguous");
  }
  const foreground = requireRecord(info.foreground_processes[0], "foreground process");
  const pid = requirePositiveInteger(foreground.pid, "foreground process pid");
  const shellPid = requirePositiveInteger(info.shell_pid, "pane shell pid");
  const foregroundProcessGroupId = requirePositiveInteger(info.foreground_process_group_id, "foreground process group id");
  if (shellPid === pid || foregroundProcessGroupId !== pid) {
    throw new Error("foreground PID is not the unique non-shell process group leader");
  }
  const name = requireString(foreground.name, "foreground process name");
  if (name !== "node" || foreground.argv0 !== "pi") {
    throw new Error("foreground process command identity mismatch");
  }
  if (foreground.cwd !== expected.cwd) throw new Error("foreground process cwd mismatch");
  return { paneId: info.pane_id, pid, shellPid, foregroundProcessGroupId, name, cwd: foreground.cwd, argv0: foreground.argv0 };
}

export function validateProcessInfoBracket(firstEnvelope, secondEnvelope, expected) {
  const first = decodeProcessInfo(firstEnvelope, expected);
  const second = decodeProcessInfo(secondEnvelope, expected);
  const fields = ["paneId", "pid", "shellPid", "foregroundProcessGroupId", "name", "cwd", "argv0"];
  if (fields.some(field => first[field] !== second[field])) {
    throw new Error("Herdr process-info identity drifted between bracket observations");
  }
  return first;
}

export function decodeProcessSnapshot(snapshotText, expected) {
  if (typeof snapshotText !== "string" || !snapshotText.endsWith("\n")) {
    throw new Error("OS process snapshot is incomplete");
  }
  const line = snapshotText.slice(0, -1);
  if (!line || line.includes("\n")) throw new Error("OS process snapshot is ambiguous");
  const fields = line.split("\t");
  if (fields.length !== 4 || !/^\d+$/.test(fields[0]) || !/^\d+$/.test(fields[2])) {
    throw new Error("OS process snapshot fields are invalid");
  }
  const snapshot = {
    pid: requirePositiveInteger(Number(fields[0]), "OS process pid"),
    startIdentity: requireString(fields[1], "OS process start identity"),
    uid: requireNonNegativeInteger(Number(fields[2]), "OS process uid"),
    title: requireString(fields[3], "OS process title"),
  };
  if (snapshot.pid !== expected.pid || snapshot.uid !== expected.uid || snapshot.title !== "pi") {
    throw new Error("OS process snapshot does not match the expected Pi PID, fixture UID, and title");
  }
  return snapshot;
}

export function validateProcessSnapshotBracket(firstSnapshotText, secondSnapshotText, expected) {
  const first = decodeProcessSnapshot(firstSnapshotText, expected);
  const second = decodeProcessSnapshot(secondSnapshotText, expected);
  if (first.pid !== second.pid || first.startIdentity !== second.startIdentity ||
      first.uid !== second.uid || first.title !== second.title) {
    throw new Error("OS process identity drifted between bracket observations");
  }
  return first;
}

export function decodeAgentStarted(envelope, expected) {
  if (envelope?.id !== "cli:agent:start") throw new Error("agent-start envelope is unsupported");
  const result = requireRecord(envelope.result, "agent-start result");
  if (result.type !== "agent_started") throw new Error("agent-start result type is unsupported");
  const agent = requireRecord(result.agent, "started agent");
  const terminalId = requireString(agent.terminal_id, "agent-start terminal id");
  if (terminalId !== expected.terminalId) throw new Error("agent-start terminal id mismatch");
  requirePaneIdentity(agent, expected, "agent-start");
  const argv = requireLaunchArgv(result.argv, expected.bridgePath, expected.incarnation ?? currentIncarnation());
  if (agent.agent_status !== "idle" || agent.interactive_ready !== true) {
    throw new Error("agent-start did not report an interactive idle Pi");
  }
  return { terminalId, argv };
}

export function decodePaneList(envelope, expected) {
  if (envelope?.id !== "cli:pane:list" || envelope.result?.type !== "pane_list") {
    throw new Error("pane-list envelope is unsupported");
  }
  const panes = envelope.result.panes;
  if (!Array.isArray(panes) || panes.length !== 1) {
    throw new Error("isolated workspace pane identity is missing or ambiguous");
  }
  return requirePaneIdentity(requireRecord(panes[0], "pane-list row"), expected, "pane-list");
}

export function decodeAgentInfo(envelope, expected) {
  if (envelope?.id !== "cli:agent:get") throw new Error("agent-get envelope is unsupported");
  const result = requireRecord(envelope.result, "agent-get result");
  if (result.type !== "agent_info") throw new Error("agent-get result type is unsupported");
  const agent = requirePaneIdentity(requireRecord(result.agent, "agent-get pane"), expected, "agent-get");
  const session = requireRecord(agent.agent_session, "native agent session");
  if (session.source !== "herdr:pi" || session.agent !== "pi") {
    throw new Error("native agent session identity is unsupported");
  }
  if (session.kind !== "id" && session.kind !== "path") {
    throw new Error("native agent session reference kind is unsupported");
  }
  return { sessionKind: session.kind, sessionRef: requireString(session.value, "native agent session reference") };
}

async function assertPrivateDirectory(path, uid) {
  const stat = await lstat(path);
  if (stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== uid || (stat.mode & 0o777) !== 0o700 || await realpath(path) !== path) {
    throw new Error(`identity directory is not private and canonical: ${path}`);
  }
}

async function assertPrivateFile(path, uid) {
  const stat = await lstat(path);
  if (stat.isSymbolicLink() || !stat.isFile() || stat.uid !== uid || (stat.mode & 0o777) !== 0o600 || stat.size > 512 * 1024 || await realpath(path) !== path) {
    throw new Error(`identity evidence file is not private and canonical: ${path}`);
  }
}

async function readEvidence(root, name, uid) {
  const path = join(root, "logs", name);
  await assertPrivateFile(path, uid);
  const bytes = await readFile(path);
  return {
    value: JSON.parse(boundedBytes(bytes).toString("utf8")),
    sha256: createHash("sha256").update(bytes).digest("hex"),
  };
}

async function readTextEvidence(root, name, uid) {
  const path = join(root, "logs", name);
  await assertPrivateFile(path, uid);
  const bytes = await readFile(path);
  return {
    value: boundedBytes(bytes).toString("utf8"),
    sha256: createHash("sha256").update(bytes).digest("hex"),
  };
}

function validateWorkspaceEvidence(envelope, expected) {
  if (envelope?.id !== "cli:workspace:create" || envelope.result?.type !== "workspace_created") {
    throw new Error("workspace evidence envelope is unsupported");
  }
  const workspace = requireRecord(envelope.result.workspace, "workspace evidence");
  const tab = requireRecord(envelope.result.tab, "workspace tab evidence");
  const pane = requireRecord(envelope.result.root_pane, "workspace root pane evidence");
  const tabId = requireString(tab.tab_id, "workspace tab id");
  if (workspace.workspace_id !== expected.workspaceId || tab.workspace_id !== expected.workspaceId ||
      pane.workspace_id !== expected.workspaceId || pane.pane_id !== expected.paneId ||
      pane.terminal_id !== expected.terminalId || pane.tab_id !== tabId ||
      pane.cwd !== expected.cwd || pane.foreground_cwd !== expected.cwd) {
    throw new Error("workspace creation evidence does not match the owned pane identities");
  }
}

function validateHerdrProof(proofValue, root, uid) {
  const proof = requireRecord(proofValue, "Herdr socket proof");
  if (proof.verified !== true || proof.config_root !== join(root, "config")) {
    throw new Error("Herdr proof does not identify the private fixture config");
  }
  const server = requireRecord(proof.server, "Herdr server proof");
  const launcher = requireRecord(proof.launcher, "Herdr launcher proof");
  const serverPid = requirePositiveInteger(server.pid, "Herdr server pid");
  requireString(server.startIdentity, "Herdr server start identity");
  requireString(server.commandIdentity, "Herdr server command identity");
  requirePositiveInteger(server.parentPid, "Herdr server parent pid");
  if (server.uid !== uid || server.parentPid !== launcher.pid || launcher.uid !== uid) {
    throw new Error("Herdr server proof is not owned by the fixture invocation");
  }
  requirePositiveInteger(launcher.pid, "Herdr launcher pid");
  requirePositiveInteger(launcher.parentPid, "Herdr launcher parent pid");
  requireString(launcher.startIdentity, "Herdr launcher start identity");
  requireString(launcher.commandIdentity, "Herdr launcher command identity");
  const expectedPaths = [
    join(root, "config", "herdr", "herdr.sock"),
    join(root, "config", "herdr", "herdr-client.sock"),
  ].toSorted();
  if (!Array.isArray(proof.sockets) || proof.sockets.length !== expectedPaths.length) {
    throw new Error("Herdr proof does not contain exactly the private API and client sockets");
  }
  const sockets = proof.sockets.toSorted((left, right) => left.path.localeCompare(right.path));
  for (const [index, socket] of sockets.entries()) {
    if (socket.path !== expectedPaths[index] || socket.uid !== uid || socket.mode !== 0o600 ||
        !Number.isSafeInteger(socket.device) || !Number.isSafeInteger(socket.inode) ||
        JSON.stringify(socket.listener_pids) !== JSON.stringify([String(serverPid)])) {
      throw new Error("Herdr socket listener identity does not match the private server");
    }
  }
  return {
    server: {
      pid: serverPid,
      startIdentity: server.startIdentity,
      commandIdentity: server.commandIdentity,
      parentPid: server.parentPid,
      uid: server.uid,
    },
    launcher: {
      pid: launcher.pid,
      startIdentity: launcher.startIdentity,
      commandIdentity: launcher.commandIdentity,
      parentPid: launcher.parentPid,
      uid: launcher.uid,
    },
    sockets,
  };
}

function boundedBytes(bytes) {
  if (bytes.length > 512 * 1024) throw new Error("Pi identity evidence exceeds file cap");
  return bytes;
}

async function boundedStdin(limit) {
  const chunks = [];
  let count = 0;
  for await (const chunk of process.stdin) {
    const bytes = Buffer.from(chunk);
    count += bytes.length;
    if (count > limit) throw new Error("Pi identity stdin exceeds cap");
    chunks.push(bytes);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function buildPiIdentityRecord(root, expected, requestedIdentity = {}) {
  if (expected.incarnation !== undefined && expected.incarnation !== currentIncarnation()) throw new Error("configured Pi incarnation changed");
  expected = { ...expected, incarnation: currentIncarnation() };
  const uid = requireNonNegativeInteger(requestedIdentity.uid ?? process.getuid(), "Pi process uid");
  if (process.getuid() !== uid) throw new Error("Pi OS process identity is not owned by this fixture");
  if (await realpath(root) !== root || expected.cwd !== join(root, "work") || expected.bridgePath !== join(root, "pi", "bridge.mjs")) {
    throw new Error("Pi identity paths are not the exact private fixture paths");
  }
  await assertPrivateDirectory(root, uid);
  await assertPrivateDirectory(join(root, "logs"), uid);
  const [herdrEvidence, workspaceEvidence, launchEvidence, paneEvidence, firstProcessEvidence, secondProcessEvidence, firstSnapshotEvidence, secondSnapshotEvidence] = await Promise.all([
    readEvidence(root, piEvidenceName("prestart-proof.json"), uid),
    readEvidence(root, "workspace-create.json", uid),
    readEvidence(root, piEvidenceName("start.json"), uid),
    readEvidence(root, piEvidenceName("pane-list.json"), uid),
    readEvidence(root, piEvidenceName("process-info.json"), uid),
    readEvidence(root, piEvidenceName("process-info-confirmation.json"), uid),
    readTextEvidence(root, piEvidenceName("process-snapshot-first.tsv"), uid),
    readTextEvidence(root, piEvidenceName("process-snapshot-second.tsv"), uid),
  ]);
  validateWorkspaceEvidence(workspaceEvidence.value, expected);
  const launch = decodeAgentStarted(launchEvidence.value, expected);
  const pane = decodePaneList(paneEvidence.value, expected);
  if (pane.pane_id !== expected.paneId || pane.terminal_id !== expected.terminalId) {
    throw new Error("pane-list evidence does not match the started Pi pane");
  }
  const foreground = validateProcessInfoBracket(firstProcessEvidence.value, secondProcessEvidence.value, expected);
  if (requestedIdentity.pid !== undefined && foreground.pid !== requirePositiveInteger(requestedIdentity.pid, "expected Pi process pid")) {
    throw new Error("OS PID does not match the unique Herdr foreground PID");
  }
  const osProcess = validateProcessSnapshotBracket(firstSnapshotEvidence.value, secondSnapshotEvidence.value, { pid: foreground.pid, uid });
  if ((requestedIdentity.startIdentity !== undefined && requestedIdentity.startIdentity !== osProcess.startIdentity) ||
      (requestedIdentity.title !== undefined && requestedIdentity.title !== osProcess.title)) {
    throw new Error("current Pi OS process identity does not match the recorded bracket");
  }
  const herdr = validateHerdrProof(herdrEvidence.value, root, uid);
  const record = {
    schema: "herdr-pi-process-identity-v2",
    incarnation: currentIncarnation(),
    herdr,
    workspace: {
      workspaceId: expected.workspaceId,
      paneId: expected.paneId,
      terminalId: expected.terminalId,
      cwd: expected.cwd,
    },
    launch: {
      argv: launch.argv,
      agent: "pi",
    },
    foreground,
    osProcess,
    evidenceSha256: {
      herdrPiPrestartProof: herdrEvidence.sha256,
      workspaceCreate: workspaceEvidence.sha256,
      agentStart: launchEvidence.sha256,
      paneList: paneEvidence.sha256,
      processInfoFirst: firstProcessEvidence.sha256,
      processInfoSecond: secondProcessEvidence.sha256,
      osSnapshotFirst: firstSnapshotEvidence.sha256,
      osSnapshotSecond: secondSnapshotEvidence.sha256,
    },
  };
  if (requestedIdentity.uid !== undefined && requestedIdentity.uid !== osProcess.uid) {
    throw new Error("current Pi OS UID does not match the recorded bracket");
  }
  return record;
}

async function captureOsSnapshot(root, observation, pid, uid) {
  if (!new Set(["first", "second"]).has(observation)) throw new Error("OS snapshot observation label is unsupported");
  if (process.getuid() !== uid) throw new Error("OS snapshot uid does not match the fixture runner");
  const snapshot = await boundedStdin(8192);
  decodeProcessSnapshot(snapshot, { pid, uid });
  await assertPrivateDirectory(root, uid);
  await assertPrivateDirectory(join(root, "logs"), uid);
  const path = join(root, "logs", piEvidenceName(`process-snapshot-${observation}.tsv`));
  const file = await open(path, "wx", 0o600);
  try {
    await file.writeFile(snapshot, "utf8");
    await file.sync();
  } finally {
    await file.close();
  }
  await assertPrivateFile(path, uid);
}

async function recordPiIdentity(root, expected, pid, uid) {
  const record = await buildPiIdentityRecord(root, expected, { pid, uid });
  const path = join(root, "logs", piEvidenceName("process-identity.json"));
  const contents = `${JSON.stringify(record)}\n`;
  const file = await open(path, "wx", 0o600);
  try {
    await file.writeFile(contents, "utf8");
    await file.sync();
  } finally {
    await file.close();
  }
  await assertPrivateFile(path, uid);
  const { pid: observedPid, startIdentity, uid: observedUid, title } = record.osProcess;
  process.stdout.write(`${observedPid}\t${startIdentity}\t${observedUid}\t${title}\n`);
}

async function verifyPiIdentity(root, expected, processIdentity) {
  const path = join(root, "logs", piEvidenceName("process-identity.json"));
  await assertPrivateFile(path, processIdentity.uid);
  const recorded = JSON.parse(await readFile(path, "utf8"));
  const current = await buildPiIdentityRecord(root, expected, processIdentity);
  if (JSON.stringify(recorded) !== JSON.stringify(current)) {
    throw new Error("owned Pi identity record or bound evidence changed");
  }
}

async function readJsonStdin() {
  return JSON.parse(await boundedStdin(512 * 1024));
}

function identityArguments(values) {
  const [root, paneId, workspaceId, terminalId, cwd, bridgePath, pidText, startIdentity, uidText, title] = values;
  if (!/^\d+$/.test(pidText ?? "") || !/^\d+$/.test(uidText ?? "")) throw new Error("Pi OS process identity numbers are invalid");
  return {
    root,
    expected: { paneId, workspaceId, terminalId, cwd, bridgePath },
    processIdentity: { pid: Number(pidText), startIdentity, uid: Number(uidText), title },
  };
}

async function main(args) {
  const [mode, ...values] = args;
  if (mode === "process-info") {
    const [paneId, cwd] = values;
    const decoded = decodeProcessInfo(await readJsonStdin(), { paneId, cwd });
    process.stdout.write(`${decoded.pid}\n`);
    return;
  }
  if (mode === "agent-start") {
    const [paneId, workspaceId, terminalId, cwd, bridgePath] = values;
    const decoded = decodeAgentStarted(await readJsonStdin(), { paneId, workspaceId, terminalId, cwd, bridgePath });
    process.stdout.write(`${decoded.terminalId}\n`);
    return;
  }
  if (mode === "pane-list") {
    const [paneId, terminalId, workspaceId, cwd] = values;
    decodePaneList(await readJsonStdin(), { paneId, terminalId, workspaceId, cwd });
    return;
  }
  if (mode === "capture-os-snapshot") {
    const [root, observation, pidText, uidText] = values;
    if (!/^\d+$/.test(pidText ?? "") || !/^\d+$/.test(uidText ?? "")) throw new Error("OS snapshot identity numbers are invalid");
    await captureOsSnapshot(root, observation, Number(pidText), Number(uidText));
    return;
  }
  if (mode === "record-pi") {
    const [root, paneId, workspaceId, terminalId, cwd, bridgePath, pidText, uidText] = values;
    if (!/^\d+$/.test(pidText ?? "") || !/^\d+$/.test(uidText ?? "")) throw new Error("Pi process identity numbers are invalid");
    await recordPiIdentity(root, { paneId, workspaceId, terminalId, cwd, bridgePath }, Number(pidText), Number(uidText));
    return;
  }
  if (mode === "verify-pi") {
    const identity = identityArguments(values);
    await verifyPiIdentity(identity.root, identity.expected, identity.processIdentity);
    return;
  }
  if (mode === "agent-info") {
    const [paneId, terminalId, workspaceId, cwd] = values;
    const decoded = decodeAgentInfo(await readJsonStdin(), { paneId, terminalId, workspaceId, cwd });
    process.stdout.write(`${decoded.sessionRef}\0`);
    return;
  }
  throw new Error("expected process-info, agent-start, pane-list, capture-os-snapshot, record-pi, verify-pi, or agent-info");
}

if (import.meta.main) {
  try {
    await main(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`${JSON.stringify({ code: "identity_response_invalid", message: error.message })}\n`);
    process.exitCode = 1;
  }
}

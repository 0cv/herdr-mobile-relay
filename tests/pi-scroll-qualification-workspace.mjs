import { readFile } from "node:fs/promises";
import { isAbsolute, resolve } from "node:path";

function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requireRecord(value, name) {
  if (!isRecord(value)) throw new Error(`${name} must be an object`);
  return value;
}

function requireIdentity(value, name) {
  if (typeof value !== "string" || !/^[A-Za-z0-9][A-Za-z0-9:_-]*$/.test(value)) {
    throw new Error(`${name} is missing or malformed`);
  }
  return value;
}

export function decodeWorkspaceCreatedResponse(envelope, expectedCwd) {
  if (!isRecord(envelope) || envelope.id !== "cli:workspace:create") {
    throw new Error("workspace response is not the supported CLI result envelope");
  }
  if (!isAbsolute(expectedCwd) || resolve(expectedCwd) !== expectedCwd) {
    throw new Error("owned fixture cwd is not canonical");
  }

  const result = requireRecord(envelope.result, "workspace result");
  if (result.type !== "workspace_created") throw new Error("workspace result type is not workspace_created");

  const rootPane = requireRecord(result.root_pane, "root pane");
  const workspace = requireRecord(result.workspace, "workspace");
  const tab = requireRecord(result.tab, "tab");
  const workspaceId = requireIdentity(rootPane.workspace_id, "root pane workspace id");
  const paneId = requireIdentity(rootPane.pane_id, "root pane id");
  const terminalId = requireIdentity(rootPane.terminal_id, "root pane terminal id");
  const rootTabId = requireIdentity(rootPane.tab_id, "root pane tab id");
  const workspaceRecordId = requireIdentity(workspace.workspace_id, "workspace id");
  const tabWorkspaceId = requireIdentity(tab.workspace_id, "tab workspace id");
  const tabId = requireIdentity(tab.tab_id, "tab id");

  if (workspaceId !== workspaceRecordId || workspaceId !== tabWorkspaceId || rootTabId !== tabId) {
    throw new Error("workspace, tab, and root pane identities conflict");
  }
  if (rootPane.cwd !== expectedCwd || rootPane.foreground_cwd !== expectedCwd) {
    throw new Error("root pane cwd does not match the owned fixture cwd");
  }

  return { workspaceId, paneId, terminalId };
}

if (import.meta.main) {
  const [responsePath, expectedCwd, ...extraArguments] = process.argv.slice(2);
  try {
    if (!responsePath || !expectedCwd || extraArguments.length > 0) throw new Error("expected a response path and owned fixture cwd");
    const response = JSON.parse(await readFile(responsePath, "utf8"));
    const { workspaceId, paneId, terminalId } = decodeWorkspaceCreatedResponse(response, expectedCwd);
    process.stdout.write(`${workspaceId}\t${paneId}\t${terminalId}\n`);
  } catch (error) {
    process.stderr.write(`${JSON.stringify({ code: "workspace_response_invalid", message: error.message })}\n`);
    process.exitCode = 1;
  }
}

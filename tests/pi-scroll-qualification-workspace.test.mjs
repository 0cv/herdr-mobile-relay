import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { decodeWorkspaceCreatedResponse } from "./pi-scroll-qualification-workspace.mjs";

const responsePath = "/private/tmp/herdr-scroll-p1-e90d2905-20261006-attempt-004/logs/workspace-create.json";
const responseSha256 = "fd1d7f6983f3f0817f16534ee01781388f4238f4fe425b1d681d26dc22e58fa4";
const fixtureCwd = "/private/tmp/herdr-scroll-p1-e90d2905-20261006-attempt-004/work";

function copyResponse(response) {
  return structuredClone(response);
}

test("workspace creation decoding accepts only the captured owned workspace schema", async () => {
  const bytes = await readFile(responsePath);
  expect(createHash("sha256").update(bytes).digest("hex")).toBe(responseSha256);
  const response = JSON.parse(bytes.toString("utf8"));
  expect(decodeWorkspaceCreatedResponse(response, fixtureCwd)).toEqual({ workspaceId: "w1", paneId: "w1:p1" });

  const cliResult = spawnSync(process.execPath, [
    fileURLToPath(new URL("./pi-scroll-qualification-workspace.mjs", import.meta.url)),
    responsePath,
    fixtureCwd,
  ], { encoding: "utf8", env: { PATH: process.env.PATH ?? "/usr/bin:/bin", LC_ALL: "C" } });
  expect(cliResult.status).toBe(0);
  expect(cliResult.stdout).toBe("w1\tw1:p1\n");
  expect(cliResult.stderr).toBe("");

  const mismatchedWorkspace = copyResponse(response);
  mismatchedWorkspace.result.tab.workspace_id = "w2";
  expect(() => decodeWorkspaceCreatedResponse(mismatchedWorkspace, fixtureCwd)).toThrow();

  const mismatchedTab = copyResponse(response);
  mismatchedTab.result.tab.tab_id = "w1:t2";
  expect(() => decodeWorkspaceCreatedResponse(mismatchedTab, fixtureCwd)).toThrow();

  const mismatchedCwd = copyResponse(response);
  mismatchedCwd.result.root_pane.cwd = `${fixtureCwd}/other`;
  expect(() => decodeWorkspaceCreatedResponse(mismatchedCwd, fixtureCwd)).toThrow();

  const missingPaneId = copyResponse(response);
  delete missingPaneId.result.root_pane.pane_id;
  expect(() => decodeWorkspaceCreatedResponse(missingPaneId, fixtureCwd)).toThrow();

  const malformedPaneId = copyResponse(response);
  malformedPaneId.result.root_pane.pane_id = "w1:p1\tother";
  expect(() => decodeWorkspaceCreatedResponse(malformedPaneId, fixtureCwd)).toThrow();

  const wrongEnvelope = copyResponse(response);
  wrongEnvelope.id = "cli:other";
  expect(() => decodeWorkspaceCreatedResponse(wrongEnvelope, fixtureCwd)).toThrow();

  const wrongType = copyResponse(response);
  wrongType.result.type = "workspace_changed";
  expect(() => decodeWorkspaceCreatedResponse(wrongType, fixtureCwd)).toThrow();

  const nestedEnvelope = { id: "cli:workspace:create", result: { result: response.result } };
  expect(() => decodeWorkspaceCreatedResponse(nestedEnvelope, fixtureCwd)).toThrow();
});

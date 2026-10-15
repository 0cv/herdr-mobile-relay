import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "bun:test";
import {
  decodeAgentStarted,
  decodeProcessInfo,
  validateProcessInfoBracket,
  validateProcessSnapshotBracket,
} from "./pi-scroll-qualification-identity.mjs";

const candidateTests = dirname(fileURLToPath(import.meta.url));
const cwd = "/private/tmp/herdr-scroll-p1-006-fixture/work";
const bridgePath = "/private/tmp/herdr-scroll-p1-006-fixture/pi/bridge.mjs";
const expected = { paneId: "w1:p1", workspaceId: "w1", terminalId: "term-fixture", cwd, bridgePath };
const uid = 502;

function processInfo({ paneId = expected.paneId, shellPid = 4100, pid = 4200, group = pid, processCwd = cwd } = {}) {
  return {
    id: "cli:pane:process_info",
    result: {
      type: "pane_process_info",
      process_info: {
        pane_id: paneId,
        shell_pid: shellPid,
        foreground_process_group_id: group,
        foreground_processes: [{ pid, name: "node", argv0: "pi", cwd: processCwd }],
      },
    },
  };
}

function osSnapshot({ pid = 4200, start = "Wed Oct 7 07:12:24 2026", snapshotUid = uid, title = "pi" } = {}) {
  return `${pid}\t${start}\t${snapshotUid}\t${title}\n`;
}

test("final006 process identity requires an unchanged private Herdr and OS snapshot bracket", async () => {
  const firstInfo = processInfo();
  const foreground = validateProcessInfoBracket(firstInfo, processInfo(), expected);
  assert.equal(Object.hasOwn(firstInfo.result.process_info.foreground_processes[0], "argv"), false);
  assert.deepEqual(foreground, {
    paneId: expected.paneId,
    pid: 4200,
    shellPid: 4100,
    foregroundProcessGroupId: 4200,
    name: "node",
    cwd,
    argv0: "pi",
  });
  assert.deepEqual(
    validateProcessSnapshotBracket(osSnapshot(), osSnapshot(), { pid: foreground.pid, uid }),
    { pid: 4200, startIdentity: "Wed Oct 7 07:12:24 2026", uid, title: "pi" },
  );

  assert.throws(
    () => validateProcessInfoBracket(processInfo(), processInfo({ shellPid: 4101 }), expected),
    /drifted between bracket observations/,
  );
  assert.throws(
    () => validateProcessInfoBracket(processInfo(), processInfo({ pid: 4201, group: 4201 }), expected),
    /drifted between bracket observations/,
  );
  assert.throws(
    () => validateProcessInfoBracket(processInfo(), processInfo({ paneId: "other-pane" }), expected),
    /pane id mismatch/,
  );
  assert.throws(
    () => validateProcessSnapshotBracket(osSnapshot(), osSnapshot({ pid: 4201 }), { pid: 4200, uid }),
    /expected Pi PID/,
  );
  assert.throws(
    () => validateProcessSnapshotBracket(osSnapshot(), osSnapshot({ start: "Wed Oct 7 07:12:25 2026" }), { pid: 4200, uid }),
    /drifted between bracket observations/,
  );
  assert.throws(
    () => validateProcessSnapshotBracket(osSnapshot(), osSnapshot({ snapshotUid: uid + 1 }), { pid: 4200, uid }),
    /fixture UID/,
  );
  assert.throws(
    () => validateProcessSnapshotBracket(osSnapshot(), osSnapshot({ title: "node" }), { pid: 4200, uid }),
    /fixture UID, and title/,
  );
  assert.throws(
    () => decodeProcessInfo(processInfo({ processCwd: "/other/work" }), expected),
    /cwd mismatch/,
  );

  const start = {
    id: "cli:agent:start",
    result: {
      type: "agent_started",
      agent: {
        agent: "pi",
        agent_status: "idle",
        interactive_ready: true,
        pane_id: expected.paneId,
        terminal_id: expected.terminalId,
        workspace_id: expected.workspaceId,
        cwd,
        foreground_cwd: cwd,
      },
      argv: ["pi", "--offline", "--extension", bridgePath],
    },
  };
  assert.deepEqual(decodeAgentStarted(start, expected).argv, start.result.argv);

  const driver = await readFile(join(candidateTests, "pi-scroll-qualification.sh"), "utf8");
  const identity = await readFile(join(candidateTests, "pi-scroll-qualification-identity.mjs"), "utf8");
  assert.match(identity, /processInfoFirst:/);
  assert.match(identity, /processInfoSecond:/);
  assert.match(identity, /osSnapshotFirst:/);
  assert.match(identity, /osSnapshotSecond:/);
  assert.match(identity, /open\(path, "wx", 0o600\)/);
  const guard = driver.match(/herdr\(\) \{([\s\S]*?)\n\}/)?.[1];
  assert.ok(guard);
  const tmuxCheck = guard.indexOf("tmux_proof_call verify >/dev/null || return 1");
  const herdrCheck = guard.indexOf("herdr_proof_call verify >/dev/null || return 1");
  const command = guard.indexOf('clean_env "$HERDR_BIN" "$@"');
  assert.ok(tmuxCheck >= 0 && herdrCheck > tmuxCheck && command > herdrCheck);
  assert.doesNotMatch(driver, /herdr_private/);
  const directHerdrCommands = [...driver.matchAll(/clean_env "\$HERDR_BIN"[^\n]*/g)].map(match => match[0]).sort();
  assert.deepEqual(directHerdrCommands, [
    'clean_env "$HERDR_BIN" "$@"',
    'clean_env "$HERDR_BIN" server > "$ROOT/logs/herdr-server.log" 2>&1 &',
  ].sort());
  assert.equal((driver.match(/^herdr pane process-info --pane /gm) ?? []).length, 2);
  const order = [
    driver.indexOf('herdr agent start p1-scroll-fixture'),
    driver.indexOf('herdr pane process-info --pane "$PANE_ID" > "$ROOT/logs/pi-process-info.json"'),
    driver.indexOf('capture_pi_snapshot "$PI_PID" first'),
    driver.indexOf('capture_pi_snapshot "$PI_PID" second'),
    driver.indexOf('herdr pane process-info --pane "$PANE_ID" > "$ROOT/logs/pi-process-info-confirmation.json"'),
    driver.indexOf('record_process pi-agent "$PI_PID"'),
    driver.indexOf('process_matches_pi "$PI_PID" "$PI_START" "$PI_UID" "$PI_COMMAND" || { FAILURE_REASON="Pi PID, start, UID, title, or bound launch evidence changed before session lookup"'),
    driver.indexOf('herdr agent get "$PANE_ID"'),
  ];
  assert.ok(order.every((index, at) => index >= 0 && (at === 0 || order[at - 1] < index)));
});

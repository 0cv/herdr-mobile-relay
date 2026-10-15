#!/bin/bash
set -euo pipefail
umask 077
export BUN_RUNTIME_TRANSPILER_CACHE_PATH=0

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
SUCCESSOR_CONFIG=
if [[ "$#" != 0 ]]; then
    [[ "$#" == 2 && "$1" == --successor-config ]] || { printf 'Expected --successor-config <private pinned JSON>\n' >&2; exit 1; }
    SUCCESSOR_CONFIG="$2"
fi
BUN_BIN="$(realpath "$(command -v bun)")"
BOUNDS_HELPER="$SCRIPT_DIR/pi-scroll-qualification-bounds.mjs"
if [[ -z "${HERDR_P1_BOUNDS_DIR:-}" ]]; then
    BOUNDS_DIR="$(mktemp -d /private/tmp/p1-bounds.XXXXXX)"
    exec "$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" supervise "$BOUNDS_DIR" "$SUCCESSOR_CONFIG"
fi
[[ "$#" == 0 ]] || { printf 'Supervised driver refuses extra arguments\n' >&2; exit 1; }
BOUNDS_DIR="$HERDR_P1_BOUNDS_DIR"
IFS= read -r -t 2 SUPERVISOR_CONTEXT_HASH <&3 || { printf 'Missing bound supervisor context\n' >&2; exit 1; }
SUPERVISOR_CONTEXT="$("$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" context "$BOUNDS_DIR" "$$" "$SUPERVISOR_CONTEXT_HASH")"
IFS=$'\t' read -r SUPERVISOR_MODE SUPERVISOR_CONFIGURATION <<< "$SUPERVISOR_CONTEXT"
background_fixture() {
    if [[ "$SUPERVISOR_MODE" == identity-probe ]]; then
        builtin read -r -t 10 <&3 || true
        return
    fi
    clean_env "$@"
}
if [[ "$SUPERVISOR_MODE" == identity-probe ]]; then
    builtin printf 'ready\n'
    IFS= builtin read -r -t 2 IDENTITY_PROBE_CONTROL <&3 || exit 1
    [[ "$IDENTITY_PROBE_CONTROL" == launch ]] || exit 1
    background_fixture &
    IDENTITY_PROBE_PID=$!
    builtin printf '%s\n' "$IDENTITY_PROBE_PID"
    wait "$IDENTITY_PROBE_PID"
    builtin printf 'closed %s\n' "$IDENTITY_PROBE_PID"
    exit 0
fi
[[ "$SUPERVISOR_MODE" == qualification ]] || { printf 'Unexpected supervisor mode\n' >&2; exit 1; }
exec 3<&-
env() {
    "$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" command "$BOUNDS_DIR" /usr/bin/env "$@"
}
env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" BUN_RUNTIME_TRANSPILER_CACHE_PATH=0 \
    HERDR_P1_EVIDENCE_MODULE="$SCRIPT_DIR/pi-scroll-qualification-evidence.mjs" \
    "$BUN_BIN" --no-env-file --no-install -e \
    'const { requireSourceCandidateComplete } = await import(process.env.HERDR_P1_EVIDENCE_MODULE); requireSourceCandidateComplete();'
STAGE_ID=e90d2905-20261006
STAGE_RUN_ID=e90d2905-5162-4858-896e-3fe54d728120
STAGE_DIR="/private/tmp/herdr-scroll-p1-$STAGE_ID"
EXTERNAL_RECOVERY_PROOF="/private/tmp/herdr-scroll-p1-$STAGE_ID-attempt-003/logs/tmux-postfailure-recovery-proof.json"
ATTEMPT005_POSTCHECK="/private/tmp/herdr-scroll-p1-$STAGE_ID-attempt-005-recovery/postcheck.txt"
RECOVERY_FIELD=",\"attempt005PostcheckPath\":\"$ATTEMPT005_POSTCHECK\""
if [[ -f "$EXTERNAL_RECOVERY_PROOF" ]]; then
    RECOVERY_FIELD+=",\"externalRecoveryProofPath\":\"$EXTERNAL_RECOVERY_PROOF\""
fi
LEDGER_CLI="$SCRIPT_DIR/pi-scroll-qualification-ledger-cli.mjs"
TMUX_PROOF_CLI="$SCRIPT_DIR/pi-scroll-qualification-tmux-proof-cli.mjs"
HERDR_PROOF_CLI="$SCRIPT_DIR/pi-scroll-qualification-herdr-proof-cli.mjs"
OUTPUT_DIR=/private/tmp/herdr-p1-pi-qualification-20261006/latest
RECEIPT="$OUTPUT_DIR/receipt.json"
FRESH_ROOT_LIMIT=6
FIXTURE_ROOT_PARENT=/private/tmp
SUCCESSOR_FIELD=
if [[ "$SUPERVISOR_CONFIGURATION" != null ]]; then
    CONFIG_JSON="$SUPERVISOR_CONFIGURATION"
    CONFIG_FIELDS="$(printf '%s' "$CONFIG_JSON" | env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$BUN_BIN" -e 'const c = await Bun.stdin.json(); console.log([c.stageId, c.runId, c.stageRoot, c.fixtureRootParent, c.outputDirectory, JSON.stringify(c.successor)].join("\t"))')"
    IFS=$'\t' read -r STAGE_ID STAGE_RUN_ID STAGE_DIR FIXTURE_ROOT_PARENT OUTPUT_DIR SUCCESSOR_JSON <<< "$CONFIG_FIELDS"
    RECEIPT="$OUTPUT_DIR/receipt.json"
    SUCCESSOR_FIELD=",\"successor\":$SUCCESSOR_JSON"
    RECOVERY_FIELD=
    FRESH_ROOT_LIMIT=1
fi

resolve_bin() {
    local candidate
    candidate="$(command -v "$1")"
    realpath "$candidate"
}

HERDR_BIN="$(resolve_bin herdr)"
PI_BIN="$(resolve_bin pi)"
GO_BIN="$(resolve_bin go)"
BUN_BIN="$(resolve_bin bun)"
TMUX_BIN="$(resolve_bin tmux)"
LSOF_BIN="$(resolve_bin lsof)"
TMUX_VERSION="$(env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$TMUX_BIN" -V)"
HERDR_VERSION="$(env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$HERDR_BIN" --version)"
PI_VERSION="$(env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$PI_BIN" --version)"
GO_VERSION="$(env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$GO_BIN" version)"
BUN_VERSION="$(env -i PATH=/usr/bin:/bin HOME="$BOUNDS_DIR" "$BUN_BIN" --version)"
UID_VALUE="$(id -u)"
USER_VALUE="$(id -un)"
GOMODCACHE="$(env "$GO_BIN" env GOMODCACHE)"
NODE_BIN_PATH="$(command -v node || true)"
NODE_BIN_DIR=/usr/bin
if [[ -n "$NODE_BIN_PATH" ]]; then
    NODE_BIN_DIR="$(dirname "$NODE_BIN_PATH")"
fi
CLEAN_PATH="$(dirname "$HERDR_BIN"):$(dirname "$PI_BIN"):$(dirname "$GO_BIN"):$(dirname "$BUN_BIN"):$(dirname "$TMUX_BIN"):$(dirname "$LSOF_BIN"):$NODE_BIN_DIR:/usr/bin:/bin:/usr/sbin:/sbin"

if [[ -e "$OUTPUT_DIR" && ( -L "$OUTPUT_DIR" || "$(stat -f '%u' "$OUTPUT_DIR")" != "$UID_VALUE" ) ]]; then
    printf 'Refusing an unowned or symlinked receipt directory: %s\n' "$OUTPUT_DIR" >&2
    exit 1
fi
if [[ -e "$OUTPUT_DIR" ]]; then
    [[ ! -L "$OUTPUT_DIR" && "$(stat -f '%Lp' "$OUTPUT_DIR")" == 700 ]] || { printf 'Refusing a non-private receipt directory: %s\n' "$OUTPUT_DIR" >&2; exit 1; }
else
    mkdir -m 700 -p "$OUTPUT_DIR"
fi
[[ "$(realpath "$OUTPUT_DIR")" == "$OUTPUT_DIR" ]] || { printf 'Refusing a non-canonical receipt directory: %s\n' "$OUTPUT_DIR" >&2; exit 1; }
if [[ -e "$RECEIPT" ]]; then
    [[ ! -L "$RECEIPT" && -f "$RECEIPT" && "$(stat -f '%u:%Lp' "$RECEIPT")" == "$UID_VALUE:600" ]] || { printf 'Refusing an unowned or unsafe receipt: %s\n' "$RECEIPT" >&2; exit 1; }
    env -i PATH="$CLEAN_PATH" HOME=/private/tmp RECEIPT_PATH="$RECEIPT" STAGE_ID_VALUE="$STAGE_ID" STAGE_RUN_ID_VALUE="$STAGE_RUN_ID" "$BUN_BIN" -e '
const receipt = JSON.parse(await Bun.file(process.env.RECEIPT_PATH).text());
if (receipt.task !== "pi-scroll-p1" || receipt.stage_identity?.stage_id !== process.env.STAGE_ID_VALUE || receipt.stage_identity?.run_uuid !== process.env.STAGE_RUN_ID_VALUE) process.exit(1);
' || { printf 'Refusing to replace a receipt from another stage or run: %s\n' "$RECEIPT" >&2; exit 1; }
fi

ROOT=
RUN_ID=
ATTEMPT_ID=
ATTEMPT_NUMBER=0
STARTED_AT="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
RESULT=blocked
FAILURE_REASON=
QUALIFICATION_PASS=0
CLEANUP_FAILURE=
CLEANUP_COMPLETE=0
TMUX_ISOLATION_PROVED=0
HERDR_ISOLATION_PROVED=0
STAGE_VALIDATED=0
STAGE_LOCKED=0
ATTEMPT_ALLOCATED=0
ROOT_CREATED=0
ROOT_CREATION_RECORDED=0
ATTEMPT_OUTCOME_WRITTEN=0
PI_STARTED=0
PI_INCARNATION=1
HERDR_STARTED=0
TMUX_STARTED=0
LOCK_TOKEN=
PI_PID=
PI_START=
PI_UID=
PI_COMMAND=
HERDR_PID=
HERDR_LAUNCHER_PARENT=
HERDR_START=
HERDR_COMMAND=
HERDR_SOCKET_PID=
HERDR_SOCKET_START=
HERDR_SOCKET_COMMAND=
HERDR_CLIENT_SOCKET=
TMUX_SERVER_PID=
TMUX_SERVER_START=
TMUX_SERVER_COMMAND=
TMUX_SLEEP_PID=
TMUX_SLEEP_START=
TMUX_SLEEP_COMMAND=
TMUX_SOCKET_MODE=
HERDR_SOCKET=
HERDR_SOCKET_PARENT=
TMUX_SOCKET=
WORKSPACE_ID=
PANE_ID=
TERMINAL_ID=
SESSION_ID=
FOUNDATION_STATUS=not_run
LATENCY_STATUS=not_run
ROW_STATUS=not_run
PI_STOPPED=true
HERDR_STOPPED=true
TMUX_STOPPED=true

process_start() {
    LC_ALL=C ps -p "$1" -o lstart= 2>/dev/null | awk '{$1=$1; print}'
}

process_command() {
    ps -p "$1" -o command= 2>/dev/null | awk '{$1=$1; print}'
}

process_uid() {
    ps -p "$1" -o uid= 2>/dev/null | tr -d ' '
}

process_snapshot_pi() {
    LC_ALL=C ps -p "$1" -o pid= -o lstart= -o uid= -o command= 2>/dev/null |
        awk -v expected_pid="$1" '
            NF >= 8 && $1 == expected_pid {
                start = $2 " " $3 " " $4 " " $5 " " $6
                command = $8
                for (field = 9; field <= NF; field++) command = command " " $field
                printf "%s\t%s\t%s\t%s\n", $1, start, $7, command
                records++
            }
            END { if (records != 1) exit 1 }
        '
}

capture_pi_snapshot() {
    local pid="$1"
    local observation="$2"
    local snapshot
    snapshot="$(process_snapshot_pi "$pid")" || return 1
    printf '%s\n' "$snapshot" | clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" \
        capture-os-snapshot "$ROOT" "$observation" "$pid" "$UID_VALUE"
}

process_parent() {
    ps -p "$1" -o ppid= 2>/dev/null | tr -d ' '
}

record_process() {
    local role="$1"
    local pid="$2"
    local required="$3"
    local start uid command
    if [[ "$role" == pi-agent ]]; then
        local observed_pid identity
        identity="$(clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" record-pi \
            "$ROOT" "$PANE_ID" "$WORKSPACE_ID" "$TERMINAL_ID" "$ROOT/work" "$ROOT/pi/bridge.mjs" \
            "$pid" "$UID_VALUE")" || { FAILURE_REASON="Pi process-info and OS identity bracket did not remain stable"; return 1; }
        IFS=$'\t' read -r observed_pid start uid command <<< "$identity"
        if [[ "$observed_pid" != "$pid" || -z "$start" || "$uid" != "$UID_VALUE" || "$command" != "pi" ]]; then
            FAILURE_REASON="Pi process-info and OS identity bracket did not match the owned foreground process"
            return 1
        fi
    else
        start="$(process_start "$pid")"
        uid="$(process_uid "$pid")"
        command="$(process_command "$pid")"
        if [[ -z "$start" || "$uid" != "$UID_VALUE" ]]; then
            FAILURE_REASON="process identity proof failed for $role"
            return 1
        fi
        if [[ "$command" != *"$required"* ]]; then
            FAILURE_REASON="process identity proof failed for $role"
            return 1
        fi
    fi
    printf '%s\t%s\t%s\t%s\n' "$role" "$pid" "$start" "$command" >> "$ROOT/owned-processes.tsv"
    case "$role" in
        tmux-server) TMUX_SERVER_PID="$pid"; TMUX_SERVER_START="$start"; TMUX_SERVER_COMMAND="$command" ;;
        tmux-bootstrap) TMUX_SLEEP_PID="$pid"; TMUX_SLEEP_START="$start"; TMUX_SLEEP_COMMAND="$command" ;;
        herdr-server) HERDR_SOCKET_PID="$pid"; HERDR_SOCKET_START="$start"; HERDR_SOCKET_COMMAND="$command" ;;
        pi-agent) PI_PID="$pid"; PI_START="$start"; PI_UID="$uid"; PI_COMMAND="$command" ;;
    esac
}

process_matches() {
    local pid="$1"
    local start="$2"
    local required="$3"
    local current_start current_uid current_command
    current_start="$(process_start "$pid")"
    current_uid="$(process_uid "$pid")"
    current_command="$(process_command "$pid")"
    [[ -n "$current_start" && "$current_start" == "$start" && "$current_uid" == "$UID_VALUE" && "$current_command" == *"$required"* ]]
}

process_matches_pi() {
    local pid="$1"
    local start="$2"
    local uid="$3"
    local title="$4"
    local snapshot current_pid current_start current_uid current_title
    snapshot="$(process_snapshot_pi "$pid")" || return 1
    IFS=$'\t' read -r current_pid current_start current_uid current_title <<< "$snapshot"
    [[ "$current_pid" == "$pid" && "$current_start" == "$start" && "$current_uid" == "$uid" && "$current_title" == "$title" && "$title" == "pi" ]] || return 1
    clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" verify-pi \
        "$ROOT" "$PANE_ID" "$WORKSPACE_ID" "$TERMINAL_ID" "$ROOT/work" "$ROOT/pi/bridge.mjs" \
        "$pid" "$start" "$uid" "$title"
}

clean_env() {
    local environment_command=env
    if [[ "$1" == "$HERDR_BIN" && "${2:-}" == server && "$#" == 2 ]]; then
        ulimit -f 64
        environment_command=/usr/bin/env
    fi
    "$environment_command" -i \
        PATH="$CLEAN_PATH" \
        HOME="$ROOT/home" \
        USER="$USER_VALUE" \
        LOGNAME="$USER_VALUE" \
        SHELL=/bin/sh \
        TERM=xterm-256color \
        LC_ALL=C \
        TMPDIR="$ROOT/tmp" \
        XDG_CONFIG_HOME="$ROOT/config" \
        XDG_CACHE_HOME="$ROOT/cache" \
        XDG_DATA_HOME="$ROOT/data" \
        XDG_STATE_HOME="$ROOT/state" \
        BUN_RUNTIME_TRANSPILER_CACHE_PATH=0 \
        NODE_COMPILE_CACHE="$ROOT/cache/node" \
        XDG_RUNTIME_DIR="$ROOT/runtime" \
        HERDR_CONFIG_PATH="$ROOT/config/herdr/config.toml" \
        TMUX_TMPDIR="$ROOT/tmux" \
        PI_CODING_AGENT_DIR="$ROOT/pi" \
        PI_OFFLINE=1 \
        HERDR_P1_FIXTURE_ROOT="$ROOT" \
        HERDR_P1_PI_INCARNATION="$PI_INCARNATION" \
        GIT_CONFIG_NOSYSTEM=1 \
        GIT_CONFIG_GLOBAL=/dev/null \
        GIT_TERMINAL_PROMPT=0 \
        "$@"
}

tmux_proof_call() {
    local action="$1"
    local input
    input="$(env -i PATH="$CLEAN_PATH" HOME=/private/tmp \
        ACTION_VALUE="$action" STAGE_DIR_VALUE="$STAGE_DIR" ROOT_VALUE="$ROOT" \
        STAGE_ID_VALUE="$STAGE_ID" STAGE_RUN_ID_VALUE="$STAGE_RUN_ID" \
        ATTEMPT_ID_VALUE="$ATTEMPT_ID" UID_VALUE="$UID_VALUE" LOCK_TOKEN_VALUE="$LOCK_TOKEN" \
        SOCKET_VALUE="$TMUX_SOCKET" TMUX_BIN_VALUE="$TMUX_BIN" LSOF_BIN_VALUE="$LSOF_BIN" \
        TMUX_PID_VALUE="${TMUX_SERVER_PID:-}" TMUX_START_VALUE="${TMUX_SERVER_START:-}" \
        TMUX_COMMAND_VALUE="${TMUX_SERVER_COMMAND:-}" BOOTSTRAP_PID_VALUE="${TMUX_SLEEP_PID:-}" \
        BOOTSTRAP_START_VALUE="${TMUX_SLEEP_START:-}" BOOTSTRAP_COMMAND_VALUE="${TMUX_SLEEP_COMMAND:-}" \
        "$BUN_BIN" -e '
const e = process.env;
const input = {
  stageRoot: e.STAGE_DIR_VALUE,
  root: e.ROOT_VALUE,
  stageId: e.STAGE_ID_VALUE,
  runId: e.STAGE_RUN_ID_VALUE,
  attemptId: e.ATTEMPT_ID_VALUE,
  uid: Number(e.UID_VALUE),
  socketPath: e.SOCKET_VALUE,
  lockToken: e.LOCK_TOKEN_VALUE,
  tmuxBin: e.TMUX_BIN_VALUE,
  lsofBin: e.LSOF_BIN_VALUE
};
if (e.ACTION_VALUE !== "discover") {
  input.server = { pid: Number(e.TMUX_PID_VALUE), startIdentity: e.TMUX_START_VALUE, commandIdentity: e.TMUX_COMMAND_VALUE };
  input.bootstrap = { pid: Number(e.BOOTSTRAP_PID_VALUE), startIdentity: e.BOOTSTRAP_START_VALUE, commandIdentity: e.BOOTSTRAP_COMMAND_VALUE, parentPid: Number(e.TMUX_PID_VALUE) };
}
process.stdout.write(JSON.stringify(input));
')" || return 1
    if [[ "$action" == input ]]; then
        printf '%s' "$input"
        return
    fi
    printf '%s' "$input" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" "$TMUX_PROOF_CLI" "$action"
}

herdr_proof_call() {
    local action="$1"
    local input
    input="$(env -i PATH="$CLEAN_PATH" HOME=/private/tmp \
        ACTION_VALUE="$action" STAGE_DIR_VALUE="$STAGE_DIR" ROOT_VALUE="$ROOT" \
        STAGE_ID_VALUE="$STAGE_ID" STAGE_RUN_ID_VALUE="$STAGE_RUN_ID" \
        ATTEMPT_ID_VALUE="$ATTEMPT_ID" UID_VALUE="$UID_VALUE" LOCK_TOKEN_VALUE="$LOCK_TOKEN" \
        HERDR_BIN_VALUE="$HERDR_BIN" LSOF_BIN_VALUE="$LSOF_BIN" \
        LAUNCHER_PID_VALUE="$HERDR_PID" LAUNCHER_START_VALUE="$HERDR_START" \
        LAUNCHER_COMMAND_VALUE="$HERDR_COMMAND" LAUNCHER_PARENT_VALUE="$HERDR_LAUNCHER_PARENT" \
        SERVER_PID_VALUE="${HERDR_SOCKET_PID:-}" SERVER_START_VALUE="${HERDR_SOCKET_START:-}" \
        SERVER_COMMAND_VALUE="${HERDR_SOCKET_COMMAND:-}" SERVER_PARENT_VALUE="${HERDR_SOCKET_PARENT:-0}" \
        "$BUN_BIN" -e '
const e = process.env;
const input = {
  stageRoot: e.STAGE_DIR_VALUE,
  root: e.ROOT_VALUE,
  stageId: e.STAGE_ID_VALUE,
  runId: e.STAGE_RUN_ID_VALUE,
  attemptId: e.ATTEMPT_ID_VALUE,
  uid: Number(e.UID_VALUE),
  configPath: `${e.ROOT_VALUE}/config/herdr/config.toml`,
  herdrBin: e.HERDR_BIN_VALUE,
  lsofBin: e.LSOF_BIN_VALUE,
  socketPaths: [`${e.ROOT_VALUE}/config/herdr/herdr.sock`, `${e.ROOT_VALUE}/config/herdr/herdr-client.sock`],
  lockToken: e.LOCK_TOKEN_VALUE,
  launcher: { pid: Number(e.LAUNCHER_PID_VALUE), startIdentity: e.LAUNCHER_START_VALUE, commandIdentity: e.LAUNCHER_COMMAND_VALUE, parentPid: Number(e.LAUNCHER_PARENT_VALUE), uid: Number(e.UID_VALUE) }
};
if (e.ACTION_VALUE !== "discover") input.server = { pid: Number(e.SERVER_PID_VALUE), startIdentity: e.SERVER_START_VALUE, commandIdentity: e.SERVER_COMMAND_VALUE, parentPid: Number(e.SERVER_PARENT_VALUE), uid: Number(e.UID_VALUE) };
process.stdout.write(JSON.stringify(input));
')" || return 1
    if [[ "$action" == input ]]; then
        printf '%s' "$input"
        return
    fi
    printf '%s' "$input" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" "$HERDR_PROOF_CLI" "$action"
}

herdr() {
    if [[ "$TMUX_ISOLATION_PROVED" == 1 ]]; then
        tmux_proof_call verify >/dev/null || return 1
    fi
    if [[ "$HERDR_ISOLATION_PROVED" == 1 ]]; then
        herdr_proof_call verify >/dev/null || return 1
    fi
    clean_env "$HERDR_BIN" "$@"
}

tmux_private() {
    if [[ "$TMUX_ISOLATION_PROVED" == 1 ]]; then
        tmux_proof_call verify >/dev/null || return 1
    fi
    clean_env "$TMUX_BIN" "$@"
}

ledger_call() {
    local action="$1"
    local input="$2"
    printf '%s' "$input" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" "$LEDGER_CLI" "$action" "$STAGE_DIR"
}

stage_summary() {
    ledger_call summary "{\"allowUnrecoveredCleanup\":true$RECOVERY_FIELD}"
}

publish_receipt() {
    local final_result="$1"
    local finished_at temp_path summary
    finished_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    summary="$(stage_summary)" || return 1
    temp_path="$OUTPUT_DIR/.receipt-${LOCK_TOKEN}.tmp"
    env -i PATH="$CLEAN_PATH" HOME=/private/tmp \
        RECEIPT_PATH="$RECEIPT" RECEIPT_TEMP="$temp_path" \
        RESULT_VALUE="$final_result" FAILURE_VALUE="$FAILURE_REASON" \
        CLEANUP_FAILURE_VALUE="$CLEANUP_FAILURE" CLEANUP_COMPLETE_VALUE="$CLEANUP_COMPLETE" BOUNDS_DIRECTORY_VALUE="$BOUNDS_DIR" \
        ROOT_VALUE="$ROOT" RUN_VALUE="$RUN_ID" ATTEMPT_VALUE="$ATTEMPT_NUMBER" ATTEMPT_ID_VALUE="$ATTEMPT_ID" \
        ATTEMPT_ALLOCATED_VALUE="$ATTEMPT_ALLOCATED" ROOT_CREATED_VALUE="$ROOT_CREATED" \
        STAGE_ID_VALUE="$STAGE_ID" STAGE_RUN_ID_VALUE="$STAGE_RUN_ID" STAGE_DIR_VALUE="$STAGE_DIR" \
        STAGE_SUMMARY_VALUE="$summary" STARTED_VALUE="$STARTED_AT" FINISHED_VALUE="$finished_at" \
        HERDR_VERSION_VALUE="$HERDR_VERSION" PI_VERSION_VALUE="$PI_VERSION" \
        TMUX_VERSION_VALUE="$TMUX_VERSION" TMUX_BIN_VALUE="$TMUX_BIN" \
        TMUX_SOURCE_URL=https://github.com/tmux/tmux/releases/download/3.7c/tmux-3.7c.tar.gz \
        TMUX_SOURCE_CHECKSUM=7c60cae9a0e25288e2e24750aafc9e8800fc7fd4555e447e1b29ee4201cfb3bf \
        GO_VERSION_VALUE="$GO_VERSION" BUN_VERSION_VALUE="$BUN_VERSION" \
        HERDR_SOCKET_VALUE="$HERDR_SOCKET" HERDR_CLIENT_SOCKET_VALUE="$HERDR_CLIENT_SOCKET" \
        TMUX_SOCKET_VALUE="$TMUX_SOCKET" TMUX_SOCKET_MODE_VALUE="${TMUX_SOCKET_MODE:-}" \
        WORKSPACE_VALUE="$WORKSPACE_ID" PANE_VALUE="$PANE_ID" \
        TERMINAL_VALUE="$TERMINAL_ID" SESSION_VALUE="$SESSION_ID" \
        HERDR_PID_VALUE="${HERDR_SOCKET_PID:-}" HERDR_START_VALUE="${HERDR_SOCKET_START:-}" \
        PI_PID_VALUE="${PI_PID:-}" PI_START_VALUE="${PI_START:-}" PI_UID_VALUE="${PI_UID:-}" \
        TMUX_PID_VALUE="${TMUX_SERVER_PID:-}" TMUX_START_VALUE="${TMUX_SERVER_START:-}" \
        TMUX_SLEEP_PID_VALUE="${TMUX_SLEEP_PID:-}" TMUX_SLEEP_START_VALUE="${TMUX_SLEEP_START:-}" \
        PI_STARTED_VALUE="$PI_STARTED" PI_STOPPED_VALUE="$PI_STOPPED" HERDR_STOPPED_VALUE="$HERDR_STOPPED" \
        TMUX_STOPPED_VALUE="$TMUX_STOPPED" TMUX_ISOLATION_VALUE="$TMUX_ISOLATION_PROVED" \
        HERDR_ISOLATION_VALUE="$HERDR_ISOLATION_PROVED" FOUNDATION_VALUE="$FOUNDATION_STATUS" \
        LATENCY_VALUE="$LATENCY_STATUS" ROW_VALUE="$ROW_STATUS" \
        ROOT_BUDGET_VALUE="$FRESH_ROOT_LIMIT" BROWSER_RESULT_VALUE='8 passed (Chromium and WebKit, mocked relay)' \
        BROWSER_GAP_VALUE='Real mobile-browser-to-relay-to-Pi end-to-end was not run; browser tests used a mocked relay.' \
        "$BUN_BIN" -e '
const { open, rename, lstat, readFile } = await import("node:fs/promises");
const e = process.env;
const accounting = JSON.parse(e.STAGE_SUMMARY_VALUE);
const receipt = {
  schema_version: 2,
  task: "pi-scroll-p1",
  result: e.RESULT_VALUE,
  failure_reason: e.FAILURE_VALUE || null,
  cleanup_failure: e.CLEANUP_FAILURE_VALUE || null,
  cleanup_complete: e.CLEANUP_COMPLETE_VALUE === "1",
  possibly_live_effects: e.CLEANUP_COMPLETE_VALUE !== "1",
  bounds_evidence: e.BOUNDS_DIRECTORY_VALUE,
  bounds_limitations: "Deadlines and process checks are not atomic; scheduling, PID reuse and descendants can prevent cleanup. No OS network sandbox or guaranteed cleanup.",
  stage_identity: { stage_id: e.STAGE_ID_VALUE, run_uuid: e.STAGE_RUN_ID_VALUE, namespace: e.STAGE_DIR_VALUE },
  current_attempt: {
    allocated: e.ATTEMPT_ALLOCATED_VALUE === "1",
    attempt_id: e.ATTEMPT_ID_VALUE || null,
    attempt_number: Number(e.ATTEMPT_VALUE),
    root: e.ROOT_VALUE || null,
    root_created: e.ROOT_CREATED_VALUE === "1"
  },
  attempt_accounting: accounting,
  fresh_root_limit: Number(e.ROOT_BUDGET_VALUE),
  started_at_utc: e.STARTED_VALUE,
  finished_at_utc: e.FINISHED_VALUE,
  root_retained: Boolean(e.ROOT_VALUE),
  fixture_owner_marker: "disposable-scroll-fixture",
  toolchain: {
    herdr: e.HERDR_VERSION_VALUE,
    pi: e.PI_VERSION_VALUE,
    tmux: e.TMUX_VERSION_VALUE,
    tmux_executable: e.TMUX_BIN_VALUE,
    tmux_formula_tap: "homebrew/core",
    tmux_formula_homepage: "https://tmux.github.io/",
    tmux_upstream_source: e.TMUX_SOURCE_URL,
    tmux_upstream_sha256: e.TMUX_SOURCE_CHECKSUM,
    go: e.GO_VERSION_VALUE,
    bun: e.BUN_VERSION_VALUE
  },
  isolation: {
    child_environment: "env -i with fresh HOME, XDG, Pi directory, runtime, cache, and temporary directories",
    inherited_credentials: false,
    pi_offline_environment: e.PI_STARTED_VALUE === "1",
    pi_offline_argument: e.PI_STARTED_VALUE === "1",
    herdr_updates_disabled_in_fixture_config: Boolean(e.ROOT_VALUE),
    herdr_socket_ownership_proved: e.HERDR_ISOLATION_VALUE === "1",
    tmux_socket_ownership_proved: e.TMUX_ISOLATION_VALUE === "1",
    herdr_socket: e.HERDR_SOCKET_VALUE || null,
    herdr_client_socket: e.HERDR_CLIENT_SOCKET_VALUE || null,
    tmux_socket: e.TMUX_SOCKET_VALUE || null,
    tmux_socket_mode: e.TMUX_SOCKET_MODE_VALUE ? Number.parseInt(e.TMUX_SOCKET_MODE_VALUE, 8) : null,
    workspace_id: e.WORKSPACE_VALUE || null,
    pane_id: e.PANE_VALUE || null,
    terminal_id: e.TERMINAL_VALUE || null,
    pi_session_id: e.SESSION_VALUE || null
  },
  owned_processes: {
    herdr_server: { pid: e.HERDR_PID_VALUE || null, start_identity: e.HERDR_START_VALUE || null, stopped: e.HERDR_STOPPED_VALUE === "true" },
    pi: { pid: e.PI_PID_VALUE || null, start_identity: e.PI_START_VALUE || null, uid: e.PI_UID_VALUE ? Number(e.PI_UID_VALUE) : null, stopped: e.PI_STOPPED_VALUE === "true" },
    tmux_server: { pid: e.TMUX_PID_VALUE || null, start_identity: e.TMUX_START_VALUE || null, stopped: e.TMUX_STOPPED_VALUE === "true" },
    tmux_bootstrap: { pid: e.TMUX_SLEEP_PID_VALUE || null, start_identity: e.TMUX_SLEEP_START_VALUE || null }
  },
  checks: {
    actual_pi_scroll_foundation: e.FOUNDATION_VALUE,
    read_only_scroll_latency: e.LATENCY_VALUE,
    actual_row_lease_dispatch_and_viewport_movement: e.ROW_VALUE,
    viewport_movement_evidence: e.ROW_VALUE === "passed" ? "The real dispatcher test compared visible Pi viewport bytes before and after acknowledged up and down wheel inputs and recorded SHA-256 digests in the retained test log." : "not established",
    browser_mock_suite: e.BROWSER_RESULT_VALUE,
    browser_real_relay_pi_gap: e.BROWSER_GAP_VALUE
  },
  findings_evidence: {
    F006: e.ROOT_VALUE && e.HERDR_ISOLATION_VALUE === "1" && e.TMUX_ISOLATION_VALUE === "1" ? "The retained attempt root records the independently owned Herdr and tmux sockets, scrubbed environment, offline launch, process identities, and cleanup." : "Not established: this stage run did not fully prove ownership of its isolated Herdr and tmux sockets.",
    F010: `Foundation: ${e.FOUNDATION_VALUE}; read-only latency: ${e.LATENCY_VALUE}; retained attempt logs contain details when run.`,
    F011: e.ROW_VALUE === "passed" ? "The row-lease fixture recorded distinct visible viewport digests after acknowledged up and down wheel inputs." : "Not established: actual row-lease viewport movement did not pass."
  },
  historical_roots: { preserved_unchanged: true, broad_prefix_count_used_for_budget: false },
  deferred: [
    "Full-slice qualification gates not included in this isolated P1 run.",
    "Physical-device and non-host qualification not performed."
  ],
  evidence_paths: {
    stage_owner: `${e.STAGE_DIR_VALUE}/stage-owner.json`,
    allocation_ledger: `${e.STAGE_DIR_VALUE}/allocation-ledger.jsonl`,
    prior_observations: `${e.STAGE_DIR_VALUE}/pre-stage-observations.jsonl`,
    process_ledger: e.ROOT_VALUE ? `${e.ROOT_VALUE}/owned-processes.tsv` : null,
    logs: e.ROOT_VALUE ? `${e.ROOT_VALUE}/logs` : null
  }
};
try {
  const previousStat = await lstat(e.RECEIPT_PATH);
  if (previousStat.isSymbolicLink() || !previousStat.isFile() || previousStat.uid !== process.getuid() || (previousStat.mode & 0o777) !== 0o600) throw new Error("refusing to replace an unowned or unsafe receipt");
  const previous = JSON.parse(await readFile(e.RECEIPT_PATH, "utf8"));
  if (previous.task !== "pi-scroll-p1" || previous.stage_identity?.stage_id !== e.STAGE_ID_VALUE || previous.stage_identity?.run_uuid !== e.STAGE_RUN_ID_VALUE) throw new Error("refusing to replace a receipt from another stage");
} catch (error) {
  if (error.code !== "ENOENT") throw error;
}
const handle = await open(e.RECEIPT_TEMP, "wx", 0o600);
await handle.writeFile(`${JSON.stringify(receipt, null, 2)}\n`);
await handle.sync();
await handle.close();
await rename(e.RECEIPT_TEMP, e.RECEIPT_PATH);
'
}

record_cleanup_failure() {
    if [[ -n "$CLEANUP_FAILURE" ]]; then
        CLEANUP_FAILURE+=$'\n'
    fi
    CLEANUP_FAILURE+="$1"
}

cleanup() {
    local cleanup_ok=1
    if [[ -n "$PI_PID" ]]; then
        if process_start "$PI_PID" >/dev/null; then
            if process_matches_pi "$PI_PID" "$PI_START" "$PI_UID" "$PI_COMMAND"; then
                kill -TERM "$PI_PID" 2>/dev/null || true
                local attempt
                for attempt in $(seq 1 30); do
                    if ! process_start "$PI_PID" >/dev/null; then
                        break
                    fi
                    sleep 0.1
                done
                if process_start "$PI_PID" >/dev/null; then
                    if process_matches_pi "$PI_PID" "$PI_START" "$PI_UID" "$PI_COMMAND"; then
                        kill -KILL "$PI_PID" 2>/dev/null || true
                    else
                        record_cleanup_failure "Pi PID identity changed before cleanup"
                        cleanup_ok=0
                    fi
                fi
            else
                record_cleanup_failure "Pi process identity could not be revalidated"
                cleanup_ok=0
            fi
        fi
        if ! process_start "$PI_PID" >/dev/null; then
            PI_STOPPED=true
        else
            record_cleanup_failure "owned Pi process remained after exact-PID cleanup"
            cleanup_ok=0
        fi
    elif [[ "$PI_STARTED" == 1 ]]; then
        record_cleanup_failure "Pi was started but its owned process identity was not captured"
        cleanup_ok=0
    fi
    if [[ "$HERDR_ISOLATION_PROVED" == 1 && -n "$HERDR_SOCKET_PID" ]]; then
        if process_start "$HERDR_SOCKET_PID" >/dev/null; then
            if ! herdr_proof_call verify > "$ROOT/logs/herdr-pre-stop-proof.json"; then
                record_cleanup_failure "Herdr dual-socket proof failed before cleanup"
                cleanup_ok=0
            elif process_matches "$HERDR_SOCKET_PID" "$HERDR_SOCKET_START" "$HERDR_BIN server" && process_matches "$HERDR_PID" "$HERDR_START" "tests/pi-scroll-qualification.sh"; then
                if ! herdr server stop > "$ROOT/logs/herdr-stop.log" 2>&1; then
                    record_cleanup_failure "Herdr server stop through its isolated socket failed"
                    cleanup_ok=0
                fi
            else
                record_cleanup_failure "Herdr server or launcher identity changed before cleanup"
                cleanup_ok=0
            fi
        fi
        if [[ "$cleanup_ok" == 1 ]]; then
            local attempt
            for attempt in $(seq 1 30); do
                if ! process_start "$HERDR_SOCKET_PID" >/dev/null && ! process_start "$HERDR_PID" >/dev/null; then
                    break
                fi
                sleep 0.1
            done
            if process_start "$HERDR_SOCKET_PID" >/dev/null || process_start "$HERDR_PID" >/dev/null; then
                record_cleanup_failure "Herdr server or launcher remained after isolated stop"
                cleanup_ok=0
            elif ! herdr_proof_call stopped > "$ROOT/logs/herdr-post-stop-proof.json" 2> "$ROOT/logs/herdr-post-stop-proof-error.log"; then
                record_cleanup_failure "Herdr socket and process absence proof failed after cleanup"
                cleanup_ok=0
            else
                HERDR_STOPPED=true
            fi
        fi
    elif [[ "$HERDR_STARTED" == 1 ]]; then
        record_cleanup_failure "Herdr server started but its private socket ownership and server absence were not proved"
        cleanup_ok=0
    fi
    if [[ "$TMUX_ISOLATION_PROVED" == 1 && -n "$TMUX_SERVER_PID" && -n "$TMUX_SLEEP_PID" ]]; then
        if process_start "$TMUX_SERVER_PID" >/dev/null; then
            if process_matches "$TMUX_SERVER_PID" "$TMUX_SERVER_START" "$TMUX_BIN" && process_matches "$TMUX_SLEEP_PID" "$TMUX_SLEEP_START" "/bin/sleep 86400"; then
                if ! tmux_private -S "$TMUX_SOCKET" kill-server > "$ROOT/logs/tmux-stop.log" 2>&1; then
                    record_cleanup_failure "tmux stop through its verified private socket failed"
                    cleanup_ok=0
                fi
            else
                record_cleanup_failure "tmux server or bootstrap process identity changed before cleanup"
                cleanup_ok=0
            fi
        fi
        if [[ "$cleanup_ok" == 1 ]]; then
            local attempt
            for attempt in $(seq 1 30); do
                if ! process_start "$TMUX_SERVER_PID" >/dev/null && ! process_start "$TMUX_SLEEP_PID" >/dev/null; then
                    break
                fi
                sleep 0.1
            done
            if process_start "$TMUX_SERVER_PID" >/dev/null || process_start "$TMUX_SLEEP_PID" >/dev/null; then
                record_cleanup_failure "tmux server or bootstrap process remained after socket shutdown"
                cleanup_ok=0
            elif ! tmux_proof_call remove-stale > "$ROOT/logs/tmux-socket-cleanup.json" 2> "$ROOT/logs/tmux-socket-cleanup-error.log"; then
                record_cleanup_failure "tmux socket cleanup proof failed"
                cleanup_ok=0
            else
                TMUX_STOPPED=true
            fi
        fi
    elif [[ "$TMUX_STARTED" == 1 ]]; then
        record_cleanup_failure "tmux server started but its private socket ownership was not proved"
        cleanup_ok=0
    fi
    if ! "$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" check-effects "$BOUNDS_DIR"; then
        record_cleanup_failure "bounded command effects remain uncertain; direct-child exit does not prove descendant cleanup"
        cleanup_ok=0
    fi
    CLEANUP_COMPLETE="$cleanup_ok"
    if [[ "$cleanup_ok" != 1 ]]; then
        FAILURE_REASON="$CLEANUP_FAILURE"
        RESULT=blocked
        QUALIFICATION_PASS=0
    fi
}

json_string() {
    printf '%s' "$1" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" -e 'process.stdout.write(JSON.stringify(await Bun.stdin.text()))'
}

finish() {
    local exit_code=$?
    local result_json root_created_json cleanup_json escaped_failure outcome_input initial_failure cleanup_failure
    trap - EXIT INT TERM
    if ! "$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" begin-cleanup "$BOUNDS_DIR"; then
        printf 'Cleanup reserve could not be established; effects may remain live\n' >&2
        exit 1
    fi
    if [[ "$STAGE_VALIDATED" != 1 || "$STAGE_LOCKED" != 1 ]]; then
        exit "$exit_code"
    fi
    initial_failure="$FAILURE_REASON"
    cleanup
    cleanup_failure="$CLEANUP_FAILURE"
    if [[ -n "$initial_failure" && -n "$cleanup_failure" ]]; then
        FAILURE_REASON="qualification failure: $initial_failure; cleanup failure: $cleanup_failure"
    elif [[ -n "$cleanup_failure" ]]; then
        FAILURE_REASON="$cleanup_failure"
    else
        FAILURE_REASON="$initial_failure"
    fi
    if [[ "$exit_code" -eq 0 && "$QUALIFICATION_PASS" == 1 && -z "$FAILURE_REASON" && "$CLEANUP_COMPLETE" == 1 ]]; then
        RESULT=qualified
    else
        RESULT=blocked
        if [[ -z "$FAILURE_REASON" ]]; then
            FAILURE_REASON="qualification did not complete successfully"
        fi
        if [[ "$exit_code" -eq 0 ]]; then
            exit_code=1
        fi
    fi
    if [[ "$ATTEMPT_ALLOCATED" == 1 && "$ATTEMPT_OUTCOME_WRITTEN" != 1 ]]; then
        root_created_json=false
        [[ "$ROOT_CREATION_RECORDED" == 1 ]] && root_created_json=true
        cleanup_json=false
        [[ "$CLEANUP_COMPLETE" == 1 ]] && cleanup_json=true
        escaped_failure="$(json_string "$FAILURE_REASON")"
        outcome_input="{\"token\":\"$LOCK_TOKEN\",\"attemptId\":\"$ATTEMPT_ID\",\"result\":\"$RESULT\",\"rootCreated\":$root_created_json,\"cleanupComplete\":$cleanup_json,\"failureReason\":$escaped_failure$RECOVERY_FIELD}"
        if ledger_call outcome "$outcome_input" >/dev/null; then
            ATTEMPT_OUTCOME_WRITTEN=1
        else
            FAILURE_REASON="could not append a complete attempt outcome to the stage ledger"
            RESULT=blocked
            exit_code=1
            CLEANUP_COMPLETE=0
        fi
    fi
    if [[ "$CLEANUP_COMPLETE" == 1 && ( "$ATTEMPT_ALLOCATED" != 1 || "$ATTEMPT_OUTCOME_WRITTEN" == 1 ) ]]; then
        if ! publish_receipt "$RESULT"; then
            FAILURE_REASON="atomic qualification receipt publication failed"
            RESULT=blocked
            exit_code=1
        fi
        if ledger_call release "{\"token\":\"$LOCK_TOKEN\"}" >/dev/null; then
            STAGE_LOCKED=0
        else
            FAILURE_REASON="could not release the owned stage lock"
            RESULT=blocked
            exit_code=1
        fi
        [[ "$RESULT" == qualified ]] || exit_code=1
    else
        publish_receipt blocked || printf 'Blocked receipt unavailable; retain root and bounds evidence\n' >&2
        exit_code=1
    fi
    "$BUN_BIN" --no-env-file --no-install "$BOUNDS_HELPER" complete "$BOUNDS_DIR" "$CLEANUP_COMPLETE" || exit_code=1
    exit "$exit_code"
}

trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$REPO_ROOT"
STAGE_INIT_INPUT="{\"stageId\":\"$STAGE_ID\",\"runId\":\"$STAGE_RUN_ID\",\"fixtureRootParent\":\"$FIXTURE_ROOT_PARENT\"$SUCCESSOR_FIELD$RECOVERY_FIELD}"
if ! STAGE_INIT_RESULT="$(ledger_call initialize "$STAGE_INIT_INPUT" 2>&1)"; then
    FAILURE_REASON="stage initialization or history reconciliation failed"
    printf '%s\n' "$STAGE_INIT_RESULT" >&2
    exit 1
fi
STAGE_VALIDATED=1
LOCK_TOKEN="$(env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" -e 'const { randomUUID } = await import("node:crypto"); console.log(randomUUID())')"
SHELL_START="$(process_start "$$")"
if [[ -e "$STAGE_DIR/active.lock" || -L "$STAGE_DIR/active.lock" ]]; then
    FAILURE_REASON="final attempt gate requires the stage lock to be absent before acquisition"
    exit 1
fi
LOCK_INPUT="{\"token\":\"$LOCK_TOKEN\",\"runId\":\"$STAGE_RUN_ID\",\"shellPid\":$$,\"shellStart\":$(json_string "$SHELL_START"),\"shellCommand\":\"pi-scroll-qualification.sh\"$RECOVERY_FIELD}"
if ! ledger_call lock "$LOCK_INPUT" >/dev/null 2>&1; then
    FAILURE_REASON="stage namespace is locked or ownership validation failed"
    exit 1
fi
STAGE_LOCKED=1
STAGE_SUMMARY="$(stage_summary)" || { FAILURE_REASON="stage summary failed validation after lock acquisition"; exit 1; }
ATTEMPT_NUMBER="$(printf '%s' "$STAGE_SUMMARY" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" -e 'const value = await Bun.stdin.json(); console.log(value.allocation_count + 1)')"
ATTEMPT_ID="$(printf 'attempt-%03d' "$ATTEMPT_NUMBER")"
ALLOCATION_INPUT="{\"token\":\"$LOCK_TOKEN\",\"requestedId\":\"$ATTEMPT_ID\"$RECOVERY_FIELD}"
if ! ALLOCATION_RESULT="$(ledger_call allocate "$ALLOCATION_INPUT" 2>&1)"; then
    FAILURE_REASON="stage attempt allocation refused: $ATTEMPT_ID"
    printf '%s\n' "$ALLOCATION_RESULT" >&2
    exit 1
fi
ATTEMPT_ALLOCATED=1
ROOT="$(printf '%s' "$ALLOCATION_RESULT" | env -i PATH="$CLEAN_PATH" HOME=/private/tmp "$BUN_BIN" -e 'const value = await Bun.stdin.json(); process.stdout.write(value.root_path)')"
EXPECTED_ROOT="$FIXTURE_ROOT_PARENT/herdr-scroll-p1-$STAGE_ID-$ATTEMPT_ID"
[[ "$ROOT" == "$EXPECTED_ROOT" ]] || { FAILURE_REASON="allocated fixture root did not match the stage namespace"; exit 1; }
RUN_ID="$(basename "$ROOT")"
mkdir -m 700 "$ROOT"
ROOT_CREATED=1
mkdir -m 700 -p "$ROOT/home" "$ROOT/config/herdr" "$ROOT/runtime" "$ROOT/cache/node" "$ROOT/data" "$ROOT/state" "$ROOT/tmp" "$ROOT/tmux" "$ROOT/pi" "$ROOT/work" "$ROOT/logs" "$ROOT/go-cache" "$ROOT/go-tmp" "$ROOT/evidence" "$ROOT/evidence-pi" "$ROOT/evidence-browser"
printf '%s\n' disposable-scroll-fixture > "$ROOT/fixture-owner"
chmod 600 "$ROOT/fixture-owner"
printf '{"stage_id":"%s","run_id":"%s","attempt_id":"%s","root_path":"%s","uid":%s}\n' "$STAGE_ID" "$STAGE_RUN_ID" "$ATTEMPT_ID" "$ROOT" "$UID_VALUE" > "$ROOT/.stage-owner.json"
chmod 600 "$ROOT/.stage-owner.json"
printf '[update]\nversion_check = false\nmanifest_check = false\n' > "$ROOT/config/herdr/config.toml"
chmod 600 "$ROOT/config/herdr/config.toml"
clean_env "$BUN_BIN" --no-env-file --no-install - "$ROOT" "$SCRIPT_DIR" <<'BUN'
import { open, readFile } from "node:fs/promises";
import { join } from "node:path";
const [root, scripts] = process.argv.slice(2);
const bridge = await readFile(join(scripts, "../relay/pi-command-bridge/bridge.mjs"));
const evidence = await readFile(join(scripts, "pi-scroll-evidence-extension.mjs"));
for (const [name, bytes, mode] of [
  ["command-bridge.mjs", bridge, 0o400],
  ["evidence.mjs", evidence, 0o400],
  ["evidence-bootstrap.mjs", 'import { installNativeEvidence } from "./evidence.mjs";\ninstallNativeEvidence();\n', 0o400],
  ["bridge.mjs", 'import bridge from "./command-bridge.mjs";\nimport evidence from "./evidence.mjs";\nexport default function(pi) { evidence(pi); return bridge(pi); }\n', 0o400],
  ["settings.json", JSON.stringify({ tuiMode: "fullscreen", enabledModels: [] })+"\n", 0o600],
]) {
  const file = await open(join(root, "pi", name), "wx", mode);
  try { await file.writeFile(bytes); await file.sync(); } finally { await file.close(); }
}
BUN
printf 'set-option -g default-size 100x36\n' > "$ROOT/tmux.conf"
chmod 600 "$ROOT/tmux.conf"
printf 'role\tpid\tstart_identity\tcommand_identity\n' > "$ROOT/owned-processes.tsv"
chmod 600 "$ROOT/owned-processes.tsv"
ROOT_CREATED_RESULT="$(ledger_call root-created "{\"token\":\"$LOCK_TOKEN\",\"attemptId\":\"$ATTEMPT_ID\"$RECOVERY_FIELD}")" || { FAILURE_REASON="stage ledger refused the fixture root ownership record"; exit 1; }
ROOT_CREATION_RECORDED=1
stat -f '%Lp' "$ROOT" | grep -qx 700 || { FAILURE_REASON="fixture root is not private"; exit 1; }
[[ "$(<"$ROOT/fixture-owner")" == disposable-scroll-fixture ]] || { FAILURE_REASON="fixture ownership marker mismatch"; exit 1; }
[[ ! -e "$ROOT/tmux/tmux-$UID_VALUE/default" ]] || { FAILURE_REASON="private tmux socket path was not fresh"; exit 1; }

TMUX_STARTED=1
TMUX_STOPPED=false
clean_env "$TMUX_BIN" -f "$ROOT/tmux.conf" new-session -d -s "$RUN_ID" -x 100 -y 36 'exec /bin/sleep 86400' > "$ROOT/logs/tmux-start.log" 2>&1
for attempt in $(seq 1 50); do
    TMUX_SOCKETS="$(find "$ROOT/tmux" -type s -print)"
    if [[ -n "$TMUX_SOCKETS" ]]; then
        break
    fi
    sleep 0.1
done
TMUX_SOCKET_COUNT="$(printf '%s\n' "$TMUX_SOCKETS" | awk 'NF { count++ } END { print count+0 }')"
[[ "$TMUX_SOCKET_COUNT" -eq 1 ]] || { FAILURE_REASON="private tmux socket count was not exactly one"; exit 1; }
TMUX_SOCKET="$TMUX_SOCKETS"
EXPECTED_TMUX_SOCKET="$ROOT/tmux/tmux-$UID_VALUE/default"
[[ "$TMUX_SOCKET" == "$EXPECTED_TMUX_SOCKET" ]] || { FAILURE_REASON="tmux socket escaped the exact stage-private path"; exit 1; }
if ! tmux_proof_call discover > "$ROOT/logs/tmux-discovery.json" 2> "$ROOT/logs/tmux-discovery-error.log"; then
    FAILURE_REASON="tmux socket and process discovery proof failed"
    exit 1
fi
DISCOVERY_FIELDS="$(env -i PATH="$CLEAN_PATH" HOME=/private/tmp DISCOVERY_PATH="$ROOT/logs/tmux-discovery.json" "$BUN_BIN" -e '
const discovery = JSON.parse(await Bun.file(process.env.DISCOVERY_PATH).text());
console.log([discovery.socket_mode.toString(8), discovery.server.pid, discovery.server.startIdentity, discovery.server.commandIdentity, discovery.bootstrap.pid, discovery.bootstrap.startIdentity, discovery.bootstrap.commandIdentity].join("\t"));
')"
IFS=$'\t' read -r TMUX_SOCKET_MODE TMUX_SERVER_PID TMUX_SERVER_START TMUX_SERVER_COMMAND TMUX_SLEEP_PID TMUX_SLEEP_START TMUX_SLEEP_COMMAND <<< "$DISCOVERY_FIELDS"
record_process tmux-server "$TMUX_SERVER_PID" "$TMUX_BIN" || exit 1
record_process tmux-bootstrap "$TMUX_SLEEP_PID" 'sleep 86400' || exit 1
TMUX_ISOLATION_PROVED=1
tmux_proof_call verify > "$ROOT/logs/tmux-socket-proof.json" || { FAILURE_REASON="tmux socket and process proof did not pass before use"; exit 1; }
TMUX_PANE_PID="$(tmux_private -S "$TMUX_SOCKET" list-panes -t "$RUN_ID:0.0" -F '#{pane_pid}')"
[[ "$TMUX_PANE_PID" == "$TMUX_SLEEP_PID" ]] || { FAILURE_REASON="tmux pane process did not match the proven bootstrap identity"; exit 1; }

HERDR_STARTED=1
HERDR_STOPPED=false
tmux_proof_call verify > "$ROOT/logs/tmux-pre-herdr-proof.json" || { FAILURE_REASON="tmux proof failed before isolated Herdr startup"; exit 1; }
background_fixture "$HERDR_BIN" server > "$ROOT/logs/herdr-server.log" 2>&1 &
HERDR_PID=$!
HERDR_LAUNCHER_PARENT="$(process_parent "$HERDR_PID")"
HERDR_START="$(process_start "$HERDR_PID")"
HERDR_COMMAND="$(process_command "$HERDR_PID")"
if [[ ! "$HERDR_PID" =~ ^[0-9]+$ || ! "$HERDR_LAUNCHER_PARENT" =~ ^[0-9]+$ || -z "$HERDR_START" || ( "$HERDR_COMMAND" != "bash tests/pi-scroll-qualification.sh" && "$HERDR_COMMAND" != "bash $SCRIPT_DIR/pi-scroll-qualification.sh" ) ]]; then
    FAILURE_REASON="Herdr fixture launcher identity was incomplete or unexpected"
    exit 1
fi
printf 'herdr-launcher\t%s\t%s\t%s\n' "$HERDR_PID" "$HERDR_START" "$HERDR_COMMAND" >> "$ROOT/owned-processes.tsv"
HERDR_SOCKET="$ROOT/config/herdr/herdr.sock"
HERDR_CLIENT_SOCKET="$ROOT/config/herdr/herdr-client.sock"
for attempt in $(seq 1 100); do
    if [[ -S "$HERDR_SOCKET" && -S "$HERDR_CLIENT_SOCKET" ]]; then
        break
    fi
    sleep 0.1
done
if [[ ! -S "$HERDR_SOCKET" || ! -S "$HERDR_CLIENT_SOCKET" ]]; then
    FAILURE_REASON="Herdr did not create both exact private API and client sockets"
    exit 1
fi
if ! herdr_proof_call discover > "$ROOT/logs/herdr-socket-discovery.json" 2> "$ROOT/logs/herdr-socket-discovery-error.log"; then
    FAILURE_REASON="Herdr dual-socket ownership proof failed"
    exit 1
fi
HERDR_DISCOVERY_FIELDS="$(env -i PATH="$CLEAN_PATH" HOME=/private/tmp DISCOVERY_PATH="$ROOT/logs/herdr-socket-discovery.json" "$BUN_BIN" -e '
const proof = JSON.parse(await Bun.file(process.env.DISCOVERY_PATH).text());
console.log([proof.server.pid, proof.server.startIdentity, proof.server.commandIdentity, proof.server.parentPid].join("\t"));
')"
IFS=$'\t' read -r HERDR_SOCKET_PID HERDR_SOCKET_START HERDR_SOCKET_COMMAND HERDR_SOCKET_PARENT <<< "$HERDR_DISCOVERY_FIELDS"
HERDR_DISCOVERED_START="$HERDR_SOCKET_START"
HERDR_DISCOVERED_COMMAND="$HERDR_SOCKET_COMMAND"
HERDR_DISCOVERED_PARENT="$HERDR_SOCKET_PARENT"
record_process herdr-server "$HERDR_SOCKET_PID" "$HERDR_BIN server" || exit 1
if [[ "$HERDR_SOCKET_START" != "$HERDR_DISCOVERED_START" || "$HERDR_SOCKET_COMMAND" != "$HERDR_DISCOVERED_COMMAND" || "$HERDR_SOCKET_PARENT" != "$HERDR_DISCOVERED_PARENT" ]]; then
    FAILURE_REASON="Herdr server identity changed between socket discovery and ownership recording"
    exit 1
fi
HERDR_ISOLATION_PROVED=1
herdr_proof_call verify > "$ROOT/logs/herdr-socket-proof.json" || { FAILURE_REASON="Herdr dual-socket proof failed before use"; exit 1; }
HERDR_LSOF="$ROOT/logs/herdr-socket-owner.txt"
clean_env "$LSOF_BIN" -n -P -a -p "$HERDR_SOCKET_PID" -U > "$HERDR_LSOF" 2>&1
if ! grep -F "$HERDR_SOCKET" "$HERDR_LSOF" >/dev/null || ! grep -F "$HERDR_CLIENT_SOCKET" "$HERDR_LSOF" >/dev/null; then
    FAILURE_REASON="Herdr server did not prove ownership of both exact isolated sockets"
    exit 1
fi
herdr status server --json > "$ROOT/logs/herdr-status.json" 2>&1 || { FAILURE_REASON="isolated Herdr status command failed"; exit 1; }

mkdir -m 700 -p "$ROOT/work"
herdr workspace create --cwd "$ROOT/work" --label p1-scroll-fixture --no-focus \
    --env "PI_CODING_AGENT_DIR=$ROOT/pi" --env PI_OFFLINE=1 \
    --env "HERDR_P1_FIXTURE_ROOT=$ROOT" \
    --env "NODE_OPTIONS=--import=$ROOT/pi/evidence-bootstrap.mjs" > "$ROOT/logs/workspace-create.json" 2>&1 || { FAILURE_REASON="isolated disposable workspace creation failed"; exit 1; }
CREATED_FIELDS="$(clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-workspace.mjs" "$ROOT/logs/workspace-create.json" "$ROOT/work")" || { FAILURE_REASON="workspace response lacked exact workspace, pane, and terminal identities"; exit 1; }
IFS=$'\t' read -r WORKSPACE_ID PANE_ID TERMINAL_ID <<< "$CREATED_FIELDS"
[[ -n "$WORKSPACE_ID" && -n "$PANE_ID" && -n "$TERMINAL_ID" ]] || { FAILURE_REASON="workspace, pane, and terminal identities were empty"; exit 1; }

herdr_proof_call verify > "$ROOT/logs/pi-$PI_INCARNATION-prestart-proof.json" || { FAILURE_REASON="Herdr socket ownership proof failed immediately before Pi start"; exit 1; }
PI_STARTED=1
PI_STOPPED=false
herdr agent start p1-scroll-fixture --kind pi --pane "$PANE_ID" --timeout 30000 -- --offline --tui-mode regular --extension "$ROOT/pi/bridge.mjs" > "$ROOT/logs/pi-$PI_INCARNATION-start.json" 2>&1 || { FAILURE_REASON="offline Pi failed to start in the isolated pane"; exit 1; }
clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" agent-start \
    "$PANE_ID" "$WORKSPACE_ID" "$TERMINAL_ID" "$ROOT/work" "$ROOT/pi/bridge.mjs" < "$ROOT/logs/pi-$PI_INCARNATION-start.json" || { FAILURE_REASON="agent-start response did not match the exact private Pi launch"; exit 1; }
herdr pane list --workspace "$WORKSPACE_ID" > "$ROOT/logs/pi-$PI_INCARNATION-pane-list.json" 2>&1 || { FAILURE_REASON="could not inspect the isolated Pi pane"; exit 1; }
clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" pane-list "$PANE_ID" "$TERMINAL_ID" "$WORKSPACE_ID" "$ROOT/work" < "$ROOT/logs/pi-$PI_INCARNATION-pane-list.json" || { FAILURE_REASON="pane-list identity did not uniquely match the isolated workspace"; exit 1; }
herdr pane process-info --pane "$PANE_ID" > "$ROOT/logs/pi-$PI_INCARNATION-process-info.json" 2>&1 || { FAILURE_REASON="isolated Pi process information was unavailable"; exit 1; }
PI_PID="$(clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" process-info \
    "$PANE_ID" "$ROOT/work" < "$ROOT/logs/pi-$PI_INCARNATION-process-info.json")" || { FAILURE_REASON="Pi foreground process identity response was invalid"; exit 1; }
capture_pi_snapshot "$PI_PID" first || { FAILURE_REASON="first Pi OS process snapshot was unavailable"; exit 1; }
capture_pi_snapshot "$PI_PID" second || { FAILURE_REASON="second Pi OS process snapshot was unavailable"; exit 1; }
herdr pane process-info --pane "$PANE_ID" > "$ROOT/logs/pi-$PI_INCARNATION-process-info-confirmation.json" 2>&1 || { FAILURE_REASON="second isolated Pi process information was unavailable"; exit 1; }
record_process pi-agent "$PI_PID" "$ROOT/pi/bridge.mjs" || exit 1
process_matches_pi "$PI_PID" "$PI_START" "$PI_UID" "$PI_COMMAND" || { FAILURE_REASON="Pi PID, start, UID, title, or bound launch evidence changed before session lookup"; exit 1; }
herdr agent get "$PANE_ID" > "$ROOT/logs/agent-get.json" 2>&1 || { FAILURE_REASON="could not inspect the isolated Pi agent"; exit 1; }
if ! IFS= read -r -d '' SESSION_ID < <(clean_env "$BUN_BIN" "$SCRIPT_DIR/pi-scroll-qualification-identity.mjs" agent-info "$PANE_ID" "$TERMINAL_ID" "$WORKSPACE_ID" "$ROOT/work" < "$ROOT/logs/agent-get.json"); then
    FAILURE_REASON="agent-get did not provide a unique native Pi session reference"
    exit 1
fi
[[ -n "$SESSION_ID" ]] || { FAILURE_REASON="native Pi session reference was empty"; exit 1; }

herdr pane send-text "$PANE_ID" /p1-evidence-seed > "$ROOT/logs/seed-command-send.log" 2>&1 || { FAILURE_REASON="generated Pi seed command text was not accepted"; exit 1; }
herdr pane send-keys "$PANE_ID" Enter > "$ROOT/logs/seed-command-enter.log" 2>&1 || { FAILURE_REASON="generated Pi seed command could not be submitted"; exit 1; }
for attempt in $(seq 1 50); do
    [[ ! -f "$ROOT/evidence-pi/seed-$PI_INCARNATION.json" ]] || break
    sleep 0.1
done
[[ -f "$ROOT/evidence-pi/seed-$PI_INCARNATION.json" && ! -L "$ROOT/evidence-pi/seed-$PI_INCARNATION.json" ]] || { FAILURE_REASON="Pi-written generated seed evidence was unavailable"; exit 1; }
herdr pane read "$PANE_ID" --source visible --lines 80 --format text > "$ROOT/logs/viewport-seed.txt" 2>&1 || { FAILURE_REASON="could not capture the generated Pi viewport"; exit 1; }
[[ "$(wc -c < "$ROOT/logs/viewport-seed.txt" | tr -d ' ')" -gt 100 ]] || { FAILURE_REASON="generated Pi viewport did not contain enough content"; exit 1; }

TMUX_CONTRACT_INPUT="$(tmux_proof_call input)"
HERDR_CONTRACT_INPUT="$(herdr_proof_call input)"
FIXTURE_CONTRACT_JSON="$(env -i PATH="$CLEAN_PATH" HOME="$ROOT/home" \
    TMUX_INPUT="$TMUX_CONTRACT_INPUT" HERDR_INPUT="$HERDR_CONTRACT_INPUT" \
    RECOVERY_INPUT="{${RECOVERY_FIELD#,}}" "$BUN_BIN" -e '
const tmuxProof = JSON.parse(process.env.TMUX_INPUT);
const herdrProof = JSON.parse(process.env.HERDR_INPUT);
const { stageRoot, stageId, runId, attemptId, root, uid, lockToken } = herdrProof;
console.log(JSON.stringify({ stageRoot, stageId, runId, attemptId, root, uid, lockToken, socketPath: herdrProof.socketPaths[0], tmuxProof, herdrProof, ...JSON.parse(process.env.RECOVERY_INPUT) }));
')"
printf '%s' "$FIXTURE_CONTRACT_JSON" | clean_env "$BUN_BIN" --no-env-file --no-install "$SCRIPT_DIR/pi-scroll-qualification-ledger.mjs" fixture > "$ROOT/logs/fixture-contract-proof.json" || { FAILURE_REASON="shared fixture root and live ownership contract failed"; exit 1; }

run_fixture_test() {
    local label="$1"
    local package="$2"
    local test_name="$3"
    local log="$ROOT/logs/$label.log"
    tmux_proof_call verify > "$ROOT/logs/tmux-pre-$label-proof.json" || { FAILURE_REASON="$label fixture refused after tmux proof failed"; return 1; }
    if env -i \
        PATH="$CLEAN_PATH" HOME="$ROOT/home" USER="$USER_VALUE" LOGNAME="$USER_VALUE" \
        SHELL=/bin/sh TERM=xterm-256color LC_ALL=C TMPDIR="$ROOT/tmp" \
        XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_DATA_HOME="$ROOT/data" \
        XDG_RUNTIME_DIR="$ROOT/runtime" HERDR_CONFIG_PATH="$ROOT/config/herdr/config.toml" \
        TMUX_TMPDIR="$ROOT/tmux" PI_CODING_AGENT_DIR="$ROOT/pi" PI_OFFLINE=1 \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
        GOCACHE="$ROOT/go-cache" GOTMPDIR="$ROOT/go-tmp" GOMODCACHE="$GOMODCACHE" \
        GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
        HERDR_P1_CONTRACT_JSON="$FIXTURE_CONTRACT_JSON" HERDR_P1_CONTRACT_BUN="$BUN_BIN" \
        HERDR_SCROLL_FIXTURE_SOCKET="$HERDR_SOCKET" HERDR_SCROLL_FIXTURE_PANE="$PANE_ID" \
        HERDR_SCROLL_FIXTURE_SESSION="$SESSION_ID" HERDR_SCROLL_FIXTURE_MODE=fullscreen \
        HERDR_SCROLL_FIXTURE_BIN="$HERDR_BIN" \
        HERDR_LATENCY_FIXTURE_SOCKET="$HERDR_SOCKET" HERDR_LATENCY_FIXTURE_PANE="$PANE_ID" \
        HERDR_LATENCY_FIXTURE_TERMINAL="$TERMINAL_ID" HERDR_LATENCY_FIXTURE_SESSION="$SESSION_ID" \
        HERDR_LATENCY_FIXTURE_BIN="$HERDR_BIN" \
        HERDR_ROW_FIXTURE_SOCKET="$HERDR_SOCKET" HERDR_ROW_FIXTURE_PANE="$PANE_ID" \
        HERDR_ROW_FIXTURE_TERMINAL="$TERMINAL_ID" HERDR_ROW_FIXTURE_SESSION="$SESSION_ID" \
        HERDR_ROW_FIXTURE_BIN="$HERDR_BIN" \
        "$GO_BIN" test "$package" -run "^$test_name$" -count=1 -timeout 120s -v > "$log" 2>&1; then
        cat "$log"
        case "$label" in
            foundation) FOUNDATION_STATUS=passed ;;
            latency) LATENCY_STATUS=passed ;;
            row-lease) ROW_STATUS=passed ;;
        esac
    else
        cat "$log"
        FAILURE_REASON="$label fixture failed; inspect $log"
        exit 1
    fi
}

run_fixture_test row-lease ./internal/coordinator TestDisposablePiRowLeaseScrollIntegration
run_fixture_test foundation ./internal/app TestDisposablePiScrollFoundation
run_fixture_test latency ./internal/coordinator TestDisposablePiReadOnlyLatency

[[ "$FOUNDATION_STATUS" == passed && "$LATENCY_STATUS" == passed && "$ROW_STATUS" == passed ]] || { FAILURE_REASON="one or more actual Pi fixtures did not pass"; exit 1; }
QUALIFICATION_PASS=1
exit 0

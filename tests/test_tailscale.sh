#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-tailscale-test.XXXXXX")"
TOOLS_DIR="$WORK_DIR/tools"
CORE_DIR="$WORK_DIR/core"
mkdir -p "$TOOLS_DIR" "$CORE_DIR"
FIXTURE_OWNER_MARKER="$WORK_DIR/.ts-test-fixture-owned"
FIXTURE_OWNER_TOKEN="$$:${BASHPID:-$$}"
printf '%s\n' "$FIXTURE_OWNER_TOKEN" > "$FIXTURE_OWNER_MARKER"

PASSED=0
CASES=0
ACTIVE_FOREIGN_PID=""
ACTIVE_FOREIGN_START_TIME=""
ACTIVE_FOREIGN_IDENTITY_FILE=""
ACTIVE_FOREIGN_READY_FILE=""
ACTIVE_FOREIGN_OWNER_FILE=""
ACTIVE_FOREIGN_OWNER_TOKEN=""
ACTIVE_OWNED_PROCESSES=()

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

process_start_time() {
    local pid="$1"
    local stat_line
    local -a stat_fields=()

    [ -r "/proc/$pid/stat" ] || return 1
    IFS= read -r stat_line < "/proc/$pid/stat" || return 1
    stat_line="${stat_line#*) }"
    read -r -a stat_fields <<< "$stat_line"
    [ "${#stat_fields[@]}" -ge 20 ] || return 1
    printf '%s\n' "${stat_fields[19]}"
}

process_state() {
    local pid="$1"
    local stat_line
    local -a stat_fields=()

    [ -r "/proc/$pid/stat" ] || return 1
    IFS= read -r stat_line < "/proc/$pid/stat" || return 1
    stat_line="${stat_line#*) }"
    read -r -a stat_fields <<< "$stat_line"
    [ "${#stat_fields[@]}" -ge 1 ] || return 1
    printf '%s\n' "${stat_fields[0]}"
}

process_liveness() {
    local pid="$1"
    local state
    local stat_line
    local -a stat_fields=()

    if [ ! -e "/proc/$pid/stat" ]; then
        if [ -e "/proc/$pid" ]; then
            printf '%s\n' unknown
        else
            printf '%s\n' absent
        fi
        return 0
    fi
    if ! IFS= read -r stat_line < "/proc/$pid/stat"; then
        printf '%s\n' unknown
        return 0
    fi
    stat_line="${stat_line#*) }"
    read -r -a stat_fields <<< "$stat_line"
    if [ "${#stat_fields[@]}" -lt 1 ]; then
        printf '%s\n' unknown
        return 0
    fi
    state="${stat_fields[0]}"
    if [ "$state" = Z ]; then
        printf '%s\n' zombie
    else
        printf '%s\n' live
    fi
}

process_is_live() {
    [ "$(process_liveness "$1" 2>/dev/null || true)" = live ]
}

foreign_record_matches() {
    local pid="$1"
    local start_time="$2"
    local identity_file="$3"
    local owner_file="$4"
    local owner_token="$5"
    local ready_file="${identity_file}.ready"
    local recorded_pid
    local recorded_start_time
    local ready_pid
    local ready_start_time

    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ -n "$start_time" ] || return 1
    [ -s "$owner_file" ] || return 1
    [ "$(cat "$owner_file" 2>/dev/null || true)" = "$owner_token" ] || return 1
    [ -s "$identity_file" ] || return 1
    IFS='|' read -r recorded_pid recorded_start_time < "$identity_file" || return 1
    [ "$recorded_pid" = "$pid" ] || return 1
    [ "$recorded_start_time" = "$start_time" ] || return 1
    [ -s "$ready_file" ] || return 1
    IFS='|' read -r ready_pid ready_start_time < "$ready_file" || return 1
    [ "$ready_pid" = "$pid" ] || return 1
    [ "$ready_start_time" = "$start_time" ]
}

foreign_identity_matches() {
    local pid="$1"
    local start_time="$2"
    local identity_file="$3"
    local owner_file="$4"
    local owner_token="$5"
    local current_start_time

    foreign_owner_and_identity_matches "$pid" "$start_time" "$identity_file" \
        "$owner_file" "$owner_token" || return 1
    current_start_time="$(process_start_time "$pid" 2>/dev/null || true)"
    [ "$current_start_time" = "$start_time" ] || return 1
    process_is_live "$pid"
}

foreign_owner_and_identity_matches() {
    local pid="$1"
    local start_time="$2"
    local identity_file="$3"
    local owner_file="$4"
    local owner_token="$5"
    local recorded_pid
    local recorded_start_time

    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ -n "$start_time" ] || return 1
    [ -s "$owner_file" ] || return 1
    [ "$(cat "$owner_file" 2>/dev/null || true)" = "$owner_token" ] || return 1
    [ -s "$identity_file" ] || return 1
    IFS='|' read -r recorded_pid recorded_start_time < "$identity_file" || return 1
    [ "$recorded_pid" = "$pid" ] || return 1
    [ "$recorded_start_time" = "$start_time" ]
}

foreign_identity_debug() {
    local pid="$1"
    local start_time="$2"
    local identity_file="$3"
    local owner_file="$4"
    local owner_token="$5"
    local identity_record
    local owner_record
    local current_start_time
    local state

    identity_record="$(cat "$identity_file" 2>/dev/null || true)"
    owner_record="$(cat "$owner_file" 2>/dev/null || true)"
    current_start_time="$(process_start_time "$pid" 2>/dev/null || true)"
    state="$(process_state "$pid" 2>/dev/null || true)"
    printf 'foreign_identity_debug pid=%s expected_start=%s record=%s current_start=%s owner=%s expected_owner=%s state=%s\n' \
        "$pid" "$start_time" "$identity_record" "$current_start_time" \
        "$owner_record" "$owner_token" "$state" >&2
}

clear_foreign_handle() {
    ACTIVE_FOREIGN_PID=""
    ACTIVE_FOREIGN_START_TIME=""
    ACTIVE_FOREIGN_IDENTITY_FILE=""
    ACTIVE_FOREIGN_READY_FILE=""
    ACTIVE_FOREIGN_OWNER_FILE=""
    ACTIVE_FOREIGN_OWNER_TOKEN=""
}

start_foreign_process() {
    local signal_file="$1"
    local identity_file="$2"
    local owner_file="$3"
    local owner_token="$4"
    local startup_mode="${5:-normal}"
    local ignore_term="${6:-0}"
    local ready_file="${identity_file}.ready"
    local pid
    local start_time
    local recorded_pid
    local recorded_start_time
    local current_start_time
    local state
    local parent_delay="${FAKE_FOREIGN_PARENT_DELAY_TENTHS:-0}"
    local startup_ok=0

    case "$parent_delay" in
        ''|*[!0-9]*)
            printf 'FAIL: FAKE_FOREIGN_PARENT_DELAY_TENTHS must be a nonnegative integer\n' >&2
            return 1
            ;;
    esac
    rm -f "$identity_file" "$ready_file" "$owner_file" \
        "${identity_file}."*.tmp "${owner_file}."*.tmp "$ready_file."*.tmp
    (
        local child_pid="$BASHPID"
        local child_start
        local identity_tmp="${identity_file}.${child_pid}.tmp"
        local owner_tmp="${owner_file}.${child_pid}.tmp"
        local ready_tmp="${ready_file}.${child_pid}.tmp"

        on_child_signal() {
            rm -f "$identity_tmp" "$owner_tmp" "$ready_tmp"
            printf '%s\n' signaled > "$signal_file"
            exit 0
        }
        if [ "$ignore_term" = 1 ]; then
            trap ':' INT TERM
        else
            trap on_child_signal INT TERM
        fi
        if [ "$startup_mode" = fail ]; then
            exit 97
        fi
        if [ "$startup_mode" = delayed ]; then
            for ((attempt = 0; attempt < 5; attempt++)); do
                sleep 0.1
            done
        fi
        child_start="$(process_start_time "$child_pid" 2>/dev/null || true)"
        [ -n "$child_start" ] || exit 98
        printf '%s|%s\n' "$child_pid" "$child_start" > "$identity_tmp" || exit 99
        mv -f "$identity_tmp" "$identity_file" || exit 100
        printf '%s\n' "$owner_token" > "$owner_tmp" || exit 101
        mv -f "$owner_tmp" "$owner_file" || exit 102
        printf '%s|%s\n' "$child_pid" "$child_start" > "$ready_tmp" || exit 103
        mv -f "$ready_tmp" "$ready_file" || exit 104
        for ((attempt = 0; attempt < 600; attempt++)); do
            sleep 0.1
        done
    ) &
    pid=$!

    for ((attempt = 0; attempt < parent_delay; attempt++)); do
        sleep 0.1
    done
    for ((attempt = 0; attempt < 80; attempt++)); do
        if [ -s "$identity_file" ]; then
            IFS='|' read -r recorded_pid recorded_start_time < "$identity_file" || true
            if [ "$recorded_pid" = "$pid" ] &&
                [ -n "$recorded_start_time" ] &&
                foreign_record_matches "$pid" "$recorded_start_time" \
                    "$identity_file" "$owner_file" "$owner_token"; then
                current_start_time="$(process_start_time "$pid" 2>/dev/null || true)"
                if [ "$current_start_time" = "$recorded_start_time" ] && process_is_live "$pid"; then
                    start_time="$recorded_start_time"
                    startup_ok=1
                    break
                fi
            fi
        fi
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie) break ;;
            unknown|live) sleep 0.05 ;;
        esac
    done
    if [ "$startup_ok" -ne 1 ]; then
        state="$(process_liveness "$pid")"
        if [ "$state" = absent ] || [ "$state" = zombie ]; then
            wait "$pid" 2>/dev/null || true
            rm -f "$identity_file" "$ready_file" "$owner_file" \
                "${identity_file}."*.tmp "${owner_file}."*.tmp "$ready_file."*.tmp
        fi
        printf 'FAIL: foreign fixture process did not complete its child-owned startup handshake for pid %s\n' "$pid" >&2
        return 1
    fi
    ACTIVE_FOREIGN_PID="$pid"
    ACTIVE_FOREIGN_START_TIME="$start_time"
    ACTIVE_FOREIGN_IDENTITY_FILE="$identity_file"
    ACTIVE_FOREIGN_READY_FILE="$ready_file"
    ACTIVE_FOREIGN_OWNER_FILE="$owner_file"
    ACTIVE_FOREIGN_OWNER_TOKEN="$owner_token"
}

retire_foreign_process() {
    if ! rm -f "$ACTIVE_FOREIGN_IDENTITY_FILE" "$ACTIVE_FOREIGN_READY_FILE" \
        "$ACTIVE_FOREIGN_OWNER_FILE"; then
        printf 'FAIL: could not retire the terminated foreign fixture identity record\n' >&2
        return 1
    fi
    clear_foreign_handle
}

retire_dead_foreign_process() {
    local pid="$1"
    local start_time="$ACTIVE_FOREIGN_START_TIME"
    local identity_file="$ACTIVE_FOREIGN_IDENTITY_FILE"
    local owner_file="$ACTIVE_FOREIGN_OWNER_FILE"
    local owner_token="$ACTIVE_FOREIGN_OWNER_TOKEN"
    local state

    foreign_record_matches "$pid" "$start_time" "$identity_file" \
        "$owner_file" "$owner_token" || return 1
    state="$(process_liveness "$pid")"
    case "$state" in
        absent|zombie) ;;
        *) return 1 ;;
    esac
    wait "$pid" 2>/dev/null || true
    state="$(process_liveness "$pid")"
    case "$state" in
        absent|zombie) retire_foreign_process ;;
        *) return 1 ;;
    esac
}

stop_foreign_process() {
    local pid="$ACTIVE_FOREIGN_PID"
    local start_time="$ACTIVE_FOREIGN_START_TIME"
    local identity_file="$ACTIVE_FOREIGN_IDENTITY_FILE"
    local owner_file="$ACTIVE_FOREIGN_OWNER_FILE"
    local owner_token="$ACTIVE_FOREIGN_OWNER_TOKEN"
    local attempt
    local state

    [ -n "$pid" ] || return 0
    if ! foreign_record_matches "$pid" "$start_time" "$identity_file" \
        "$owner_file" "$owner_token"; then
        foreign_identity_debug "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"
        printf 'FAIL: refusing to signal foreign fixture pid %s because its startup identity/owner record is stale\n' "$pid" >&2
        return 1
    fi
    state="$(process_liveness "$pid")"
    case "$state" in
        absent|zombie)
            if ! retire_dead_foreign_process "$pid"; then
                printf 'FAIL: foreign fixture process did not terminate while being reaped: %s\n' "$pid" >&2
                return 1
            fi
            return 0
            ;;
        unknown)
            printf 'FAIL: refusing to signal foreign fixture pid %s because its process state is unknown\n' "$pid" >&2
            return 1
            ;;
    esac
    if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
        foreign_identity_debug "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"
        printf 'FAIL: refusing to signal foreign fixture pid %s because its startup identity/owner record is stale\n' "$pid" >&2
        return 1
    fi
    if ! kill -TERM "$pid" 2>/dev/null; then
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie) ;;
            *)
                printf 'FAIL: could not TERM foreign fixture pid %s while its identity was valid\n' "$pid" >&2
                return 1
                ;;
        esac
    fi
    for attempt in {1..20}; do
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie)
                wait "$pid" 2>/dev/null || true
                if ! retire_foreign_process; then
                    return 1
                fi
                return 0
                ;;
            unknown)
                printf 'FAIL: foreign fixture pid %s became uninspectable during TERM cleanup\n' "$pid" >&2
                return 1
                ;;
        esac
        if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
            if retire_dead_foreign_process "$pid"; then
                return 0
            fi
            foreign_identity_debug "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"
            printf 'FAIL: refusing to escalate foreign fixture pid %s after its identity/owner record changed\n' "$pid" >&2
            return 1
        fi
        sleep 0.1
    done
    if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
        if retire_dead_foreign_process "$pid"; then
            return 0
        fi
        foreign_identity_debug "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"
        printf 'FAIL: foreign fixture identity changed before KILL: %s\n' "$pid" >&2
        return 1
    fi
    if ! kill -KILL "$pid" 2>/dev/null; then
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie) ;;
            *)
                printf 'FAIL: could not KILL foreign fixture pid %s while its identity was valid\n' "$pid" >&2
                return 1
                ;;
        esac
    fi
    for attempt in {1..20}; do
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie)
                wait "$pid" 2>/dev/null || true
                if ! retire_foreign_process; then
                    return 1
                fi
                return 0
                ;;
            unknown) break ;;
            live) sleep 0.1 ;;
        esac
    done
    printf 'FAIL: foreign fixture process did not terminate within bounded cleanup: %s\n' "$pid" >&2
    return 1
}

register_owned_process() {
    local pid_file="$1"
    local owner_file="$2"
    local owner_token="$3"
    local identity_file="$4"
    local pid
    local start_time
    local recorded_pid
    local recorded_start_time

    [ -s "$pid_file" ] || return 0
    [ -s "$owner_file" ] || return 0
    [ "$(cat "$owner_file")" = "$owner_token" ] || return 0
    pid="$(cat "$pid_file")"
    case "$pid" in
        ''|*[!0-9]*) return 0 ;;
    esac
    kill -0 "$pid" 2>/dev/null || return 0
    IFS='|' read -r recorded_pid recorded_start_time < "$identity_file" || return 0
    [ "$recorded_pid" = "$pid" ] || return 0
    start_time="$(process_start_time "$pid" || true)"
    [ -n "$start_time" ] || return 0
    [ "$recorded_start_time" = "$start_time" ] || return 0
    ACTIVE_OWNED_PROCESSES+=("$pid|$start_time|$pid_file|$owner_file|$owner_token|$identity_file")
}

retire_owned_process() {
    local pid_file="$1"
    local owner_file="$2"
    local identity_file="$3"
    local entry
    local entry_pid
    local entry_start
    local entry_file
    local entry_owner
    local entry_token
    local entry_identity
    local -a remaining=()

    for entry in "${ACTIVE_OWNED_PROCESSES[@]}"; do
        IFS='|' read -r entry_pid entry_start entry_file entry_owner entry_token entry_identity <<< "$entry"
        if [ "$entry_file" != "$pid_file" ]; then
            remaining+=("$entry")
        fi
    done
    ACTIVE_OWNED_PROCESSES=("${remaining[@]}")
    if ! rm -f "$pid_file" "$owner_file" "$identity_file"; then
        return 1
    fi
}

cleanup_owned_processes() {
    local entry
    local pid
    local start_time
    local pid_file
    local owner_file
    local owner_token
    local identity_file
    local break_if_stopped
    local state
    local cleanup_status=0
    local -a remaining=()

    if ! stop_foreign_process; then
        cleanup_status=1
    fi

    # Only entries registered while their per-process owner marker was live are
    # eligible. Completed PID files are retired by assert_process_stopped; do
    # not rediscover them from the whole fixture tree during EXIT cleanup.
    for entry in "${ACTIVE_OWNED_PROCESSES[@]}"; do
        IFS='|' read -r pid start_time pid_file owner_file owner_token identity_file <<< "$entry"
        [ -n "$pid" ] || continue
        if ! foreign_owner_and_identity_matches "$pid" "$start_time" \
            "$identity_file" "$owner_file" "$owner_token"; then
            printf 'FAIL: refusing to retire owned fixture pid %s because its startup identity/owner record is stale\n' "$pid" >&2
            remaining+=("$entry")
            cleanup_status=1
            continue
        fi
        state="$(process_liveness "$pid")"
        case "$state" in
            absent|zombie)
                wait "$pid" 2>/dev/null || true
                if ! rm -f "$pid_file" "$owner_file" "$identity_file"; then
                    remaining+=("$entry")
                    cleanup_status=1
                fi
                continue
                ;;
            unknown)
                printf 'FAIL: refusing to retire owned fixture pid %s because its process state is unknown\n' "$pid" >&2
                remaining+=("$entry")
                cleanup_status=1
                continue
                ;;
        esac
        if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
            printf 'FAIL: refusing to signal owned fixture pid %s because its startup identity/owner record is stale\n' "$pid" >&2
            remaining+=("$entry")
            cleanup_status=1
            continue
        fi
        if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
            printf 'FAIL: owned fixture identity changed before TERM: %s\n' "$pid" >&2
            remaining+=("$entry")
            cleanup_status=1
            continue
        fi
        if ! kill -TERM "$pid" 2>/dev/null; then
            state="$(process_liveness "$pid")"
            case "$state" in
                absent|zombie) ;;
                *)
                    printf 'FAIL: could not TERM owned fixture pid %s while its identity was valid\n' "$pid" >&2
                    remaining+=("$entry")
                    cleanup_status=1
                    continue
                    ;;
            esac
        fi
        break_if_stopped=0
        for attempt in {1..20}; do
            if ! process_is_live "$pid"; then
                wait "$pid" 2>/dev/null || true
                break_if_stopped=1
                break
            fi
            sleep 0.1
        done
        if [ "$break_if_stopped" -eq 1 ]; then
            if ! rm -f "$pid_file" "$owner_file" "$identity_file"; then
                remaining+=("$entry")
                cleanup_status=1
            fi
            continue
        fi
        if ! foreign_identity_matches "$pid" "$start_time" "$identity_file" "$owner_file" "$owner_token"; then
            if ! process_is_live "$pid"; then
                wait "$pid" 2>/dev/null || true
                if ! rm -f "$pid_file" "$owner_file" "$identity_file"; then
                    remaining+=("$entry")
                    cleanup_status=1
                fi
                continue
            fi
            printf 'FAIL: refusing to escalate owned fixture pid %s after its identity/owner record changed\n' "$pid" >&2
            remaining+=("$entry")
            cleanup_status=1
            continue
        fi
        kill -KILL "$pid" 2>/dev/null || true
        for attempt in {1..20}; do
            if ! process_is_live "$pid"; then
                wait "$pid" 2>/dev/null || true
                break
            fi
            sleep 0.1
        done
        if process_is_live "$pid"; then
            printf 'FAIL: owned fixture process did not terminate within bounded cleanup: %s\n' "$pid" >&2
            remaining+=("$entry")
            cleanup_status=1
        else
            wait "$pid" 2>/dev/null || true
            if ! rm -f "$pid_file" "$owner_file" "$identity_file"; then
                remaining+=("$entry")
                cleanup_status=1
            fi
        fi
    done
    ACTIVE_OWNED_PROCESSES=("${remaining[@]}")
    if [ "$cleanup_status" -eq 0 ]; then
        rm -rf "$WORK_DIR"
    else
        printf 'FAIL: retaining fixture recovery state after unsafe or incomplete process cleanup: %s\n' "$WORK_DIR" >&2
    fi
    return "$cleanup_status"
}

cleanup_on_exit() {
    local status=$?
    trap - EXIT
    if ! cleanup_owned_processes; then
        [ "$status" -eq 0 ] && status=1
    fi
    exit "$status"
}
trap cleanup_on_exit EXIT
[ -r /proc/self/stat ] || fail 'process-identity fixture requires Linux /proc support'

# Optional verifier-controlled sentinel. The child publishes its own PID and
# /proc start time before the parent accepts the handle; the parent never
# derives startup identity from its asynchronous $! observation.
if [ -n "${FAKE_STALE_PID_FILE:-}" ] &&
    [ -n "${FAKE_STALE_IDENTITY_FILE:-}" ] &&
    [ -n "${FAKE_STALE_RECORD_IDENTITY_FILE:-}" ] &&
    [ -n "${FAKE_STALE_OWNER_FILE:-}" ] &&
    [ -n "${FAKE_STALE_SIGNAL_FILE:-}" ]; then
    mkdir -p "$WORK_DIR/cases/stale"
    start_foreign_process \
        "$FAKE_STALE_SIGNAL_FILE" "$FAKE_STALE_IDENTITY_FILE" \
        "$FAKE_STALE_OWNER_FILE" stale-owner-token normal \
        "${FAKE_STALE_IGNORE_TERM:-0}" ||
        fail 'could not complete the delegated sentinel startup handshake'
    STALE_SENTINEL_PID="$ACTIVE_FOREIGN_PID"
    STALE_SENTINEL_START_TIME="$ACTIVE_FOREIGN_START_TIME"
    printf '%s|reused-before-registration\n' "$STALE_SENTINEL_PID" > "$FAKE_STALE_RECORD_IDENTITY_FILE"
    printf '%s\n' "$STALE_SENTINEL_PID" > "$FAKE_STALE_PID_FILE"
    printf '%s\n' "$STALE_SENTINEL_PID" > "$WORK_DIR/cases/stale/stale.pid"
    printf '%s\n' stale-owner-token > "$WORK_DIR/cases/stale/stale.owner"
    printf '%s|reused-before-registration\n' "$STALE_SENTINEL_PID" > "$WORK_DIR/cases/stale/stale.identity"
    if [ -n "${FAKE_STALE_SNAPSHOT_FILE:-}" ]; then
        printf '%s|%s\n' "$STALE_SENTINEL_PID" "$STALE_SENTINEL_START_TIME" > "$FAKE_STALE_SNAPSHOT_FILE"
    fi
    clear_foreign_handle
fi

# Supplemental cleanup-safety tests, not Layer 1. The launcher gets only
# ordinary utilities and deliberate adapters. Prohibited tools are represented
# by fail-on-use sentinels below; missing commands must not silently pass.
for tool in bash cat chmod cmp cp dirname env grep head ln mkdir mktemp mv printenv rm sed seq sleep timeout tr uname; do
    source_path="$(command -v "$tool" || true)"
    [ -n "$source_path" ] || fail "required utility is unavailable: $tool"
    ln -s "$source_path" "$CORE_DIR/$tool"
done
TIMEOUT_BIN="$(command -v timeout)"

cat > "$TOOLS_DIR/relay" <<'EOF'
#!/bin/bash
set -u

printf '%s\n' "$*" >> "$FAKE_RELAY_LOG"
command_name="${1:-}"
case "$command_name" in
    supervise)
        shift
        if [ "${1:-}" = --grace ]; then shift 2; fi
        [ "${1:-}" = -- ] || exit 96
        shift
        exec "$@"
        ;;
    check-port)
        exit 0
        ;;
    version)
        printf '{"version":"fixture","revision":"fixture-revision"}\n'
        ;;
    tailscale)
        if [ -e "$FAKE_STOP_MARKER" ]; then
            printf '%s\n' "$*" >> "$FAKE_POST_INSPECTION_LOG"
        fi
        if [ "${FAKE_INSPECTION_FAIL_AFTER_STOP:-0}" = 1 ] &&
            [ -e "$FAKE_STOP_MARKER" ]; then
            exit 78
        fi
        cat "$FAKE_STATE_FILE"
        ;;
    serve)
        printf '%s\n' "$$" > "$FAKE_RELAY_PID_FILE"
        fixture_start_time() {
            local stat_line
            local -a stat_fields=()
            IFS= read -r stat_line < "/proc/$$/stat" || return 1
            stat_line="${stat_line#*) }"
            read -r -a stat_fields <<< "$stat_line"
            [ "${#stat_fields[@]}" -ge 20 ] || return 1
            printf '%s\n' "${stat_fields[19]}"
        }
        printf '%s|%s\n' "$$" "$(fixture_start_time)" > "$FAKE_RELAY_IDENTITY_FILE"
        printf '%s\n' "$FAKE_OWNER_TOKEN" > "$FAKE_RELAY_OWNER_FILE"
        stop_relay() {
            rm -f "$FAKE_RELAY_OWNER_FILE"
            : > "$FAKE_RELAY_STOP_MARKER"
            exit 0
        }
        trap stop_relay INT TERM
        for ((attempt = 1; attempt <= 100; attempt++)); do
            if [ -e "$FAKE_ARM_FAILURE_MARKER" ]; then
                stop_relay
            fi
            sleep 0.1
        done
        rm -f "$FAKE_RELAY_OWNER_FILE"
        printf 'relay fixture timed out\n' >> "$FAKE_UNEXPECTED_LOG"
        exit 94
        ;;
    pairing-control)
        shift
        operation=""
        run_id=""
        instance=""
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --operation) operation="${2:-}" ; shift 2 ;;
                --run-id) run_id="${2:-}" ; shift 2 ;;
                --instance) instance="${2:-}" ; shift 2 ;;
                --socket) shift 2 ;;
                *) printf 'relay %s\n' "$*" >> "$FAKE_UNEXPECTED_LOG"; exit 97 ;;
            esac
        done
        case "$operation" in
            status)
                printf 'pairing-status-response {"ready":true}\n' >> "$FAKE_RELAY_LOG"
                printf '{"ready":true}\n'
                ;;
            arm_bootstrap)
                if [ "${FAKE_ARM_FAILURE:-0}" = 1 ]; then
                    : > "$FAKE_ARM_FAILURE_MARKER"
                    exit 77
                fi
                printf '{"ok":true,"run_id":"%s","instance":"%s","invitation_armed":true,"invitation_expires_at":"2099-01-01T00:00:00Z"}\n' "$run_id" "$instance"
                ;;
            *) printf 'relay pairing-control %s\n' "$operation" >> "$FAKE_UNEXPECTED_LOG"; exit 97 ;;
        esac
        ;;
    normalize-origin)
        last=""
        for argument in "$@"; do
            last="$argument"
        done
        printf '%s\n' "$last"
        ;;
    setup-fragment)
        printf 'relay=fake&host=fake\n'
        ;;
    qr)
        exit 0
        ;;
    *)
        printf 'relay %s\n' "$*" >> "$FAKE_UNEXPECTED_LOG"
        exit 97
        ;;
esac
EOF

cat > "$TOOLS_DIR/tailscale" <<'EOF'
#!/bin/bash
set -u

printf '%s\n' "$*" >> "$FAKE_TAILSCALE_LOG"
unexpected_tailscale_command() {
    printf '%s\n' "$*" >> "$FAKE_UNEXPECTED_LOG"
    exit 91
}
if [ "$#" -ne 3 ] ||
    [ "${1:-}" != serve ] ||
    [ "${2:-}" != "${FAKE_EXPECTED_SERVE_ARG:?}" ] ||
    [ "${3:-}" != "${FAKE_EXPECTED_SERVE_TARGET:?}" ]; then
    unexpected_tailscale_command "$*"
fi

printf '%s\n' "$$" > "$FAKE_TAILSCALE_PID_FILE"
fixture_start_time() {
    local stat_line
    local -a stat_fields=()
    IFS= read -r stat_line < "/proc/$$/stat" || return 1
    stat_line="${stat_line#*) }"
    read -r -a stat_fields <<< "$stat_line"
    [ "${#stat_fields[@]}" -ge 20 ] || return 1
    printf '%s\n' "${stat_fields[19]}"
}
printf '%s|%s\n' "$$" "$(fixture_start_time)" > "$FAKE_TAILSCALE_IDENTITY_FILE"
printf '%s\n' "$FAKE_OWNER_TOKEN" > "$FAKE_TAILSCALE_OWNER_FILE"
cat "$FAKE_RUNNING_STATE_FILE" > "$FAKE_STATE_FILE"
stop_tailscale() {
    cat "$FAKE_POST_STATE_FILE" > "$FAKE_STATE_FILE"
    rm -f "$FAKE_TAILSCALE_OWNER_FILE"
    : > "$FAKE_STOP_MARKER"
    exit 0
}
trap stop_tailscale INT TERM
for ((attempt = 1; attempt <= 100; attempt++)); do
    if [ -e "$FAKE_ARM_FAILURE_MARKER" ]; then
        stop_tailscale
    fi
    sleep 0.1
done
rm -f "$FAKE_TAILSCALE_OWNER_FILE"
printf 'tailscale fixture timed out\n' >> "$FAKE_UNEXPECTED_LOG"
exit 94
EOF

cat > "$TOOLS_DIR/curl" <<'EOF'
#!/bin/bash
set -u
url="${!#}"
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
case "$url" in
    http://127.0.0.1:*/healthz)
        printf '{"status":"ok","instance":"%s","version":"fixture","revision":"fixture-revision","bundle_hash":"fixture-bundle-hash","bundle_version":"fixture","bundle_revision":"fixture-revision","protocol":"test"}\n' "${HERDR_RELAY_INSTANCE_ID:-instance-1}"
        ;;
    https://relay.ts.net/healthz)
        printf '{"status":"ok","instance":"%s","version":"fixture","revision":"fixture-revision","bundle_hash":"fixture-bundle-hash","bundle_version":"fixture","bundle_revision":"fixture-revision","managed_run_id":"%s","transport":"tailscale","tailscale_origin":"https://relay.ts.net"}\n' \
            "${HERDR_RELAY_INSTANCE_ID:-instance-1}" "${HERDR_RELAY_RUN_ID:-}"
        ;;
    *)
        printf 'curl %s\n' "$*" >> "$FAKE_UNEXPECTED_LOG"
        exit 22
        ;;
esac
EOF

cat > "$TOOLS_DIR/openssl" <<'EOF'
#!/bin/sh
if [ "${1:-}" = rand ] && [ "${2:-}" = -hex ] && [ "${3:-}" = 16 ]; then
    printf '11111111111111111111111111111111\n'
    exit 0
fi
printf 'openssl %s\n' "$*" >> "${FAKE_UNEXPECTED_LOG:?}"
exit 93
EOF

cat > "$TOOLS_DIR/hostname" <<'EOF'
#!/bin/sh
printf 'fixture-relay\n'
EOF

# systemctl is the one intentional service-manager adapter: the launcher only
# probes whether its user service is active, and this stub always reports no.
cat > "$TOOLS_DIR/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${FAKE_SERVICE_LOG:?}"
exit 1
EOF

# These sentinels make accidental production-tool use observable even when the
# caller ignores a command failure. curl, openssl, hostname, tailscale and relay
# are deliberate adapters above; all other listed tools must never be invoked.
for sentinel in herdr cloudflared launchctl service rc-service systemd-run plutil open xdg-open sudo ssh scp socat nc netcat wget python python3 uv go git bun node npm npx pkill killall networksetup ifconfig ip route lsof uuidgen date ls tail sort awk cut find tee id; do
    cat > "$TOOLS_DIR/$sentinel" <<'EOF'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "${FAKE_UNEXPECTED_LOG:?}"
exit 97
EOF
    chmod 700 "$TOOLS_DIR/$sentinel"
done
chmod 700 "$TOOLS_DIR/relay" "$TOOLS_DIR/tailscale" "$TOOLS_DIR/curl" "$TOOLS_DIR/openssl" "$TOOLS_DIR/hostname" "$TOOLS_DIR/systemctl"

assert_contains() {
    local file="$1"
    local text="$2"
    grep -F -- "$text" "$file" >/dev/null || fail "expected '$text' in $file"
}

assert_not_contains() {
    local file="$1"
    local text="$2"
    if grep -F -- "$text" "$file" >/dev/null 2>&1; then
        fail "did not expect '$text' in $file"
    fi
}

assert_same_file() {
    local expected="$1"
    local actual="$2"
    if ! cmp -s "$expected" "$actual"; then
        echo "expected state:" >&2
        cat "$expected" >&2 || true
        echo "actual state:" >&2
        cat "$actual" >&2 || true
        fail "state changed unexpectedly: $actual"
    fi
}

assert_empty_log() {
    local log_file="$1"
    [ ! -s "$log_file" ] || {
        echo "unexpected fixture invocation log: $log_file" >&2
        cat "$log_file" >&2
        fail "hermetic tool sentinel was invoked"
    }
}

assert_single_line() {
    local file="$1"
    local expected="$2"
    [ -s "$file" ] || fail "expected one line in $file"
    mapfile -t lines < "$file"
    [ "${#lines[@]}" -eq 1 ] || fail "expected one line in $file, got ${#lines[@]}"
    [ "${lines[0]}" = "$expected" ] ||
        fail "unexpected line in $file: ${lines[0]}"
}

assert_tailscale_allowlist() {
    local log_file="$1"
    local expected_start="$2"
    local negative_control="$3"

    [ -s "$log_file" ] || fail "foreground Serve command was not invoked"
    mapfile -t lines < "$log_file"
    if [ "$negative_control" = 1 ]; then
        [ "${#lines[@]}" -eq 2 ] || fail "negative control expected two Tailscale commands"
        [ "${lines[0]}" = "$expected_start" ] || fail "unexpected foreground Serve argv: ${lines[0]}"
        [ "${lines[1]}" = 'serve off' ] || fail "negative control did not attempt serve off"
    else
        [ "${#lines[@]}" -eq 1 ] || fail "unexpected extra Tailscale command(s)"
        [ "${lines[0]}" = "$expected_start" ] || fail "unexpected foreground Serve argv: ${lines[0]}"
    fi
}

assert_process_stopped() {
    local pid_file="$1"
    local owner_file="$2"
    local identity_file="$3"
    [ -s "$pid_file" ] || fail "owned process did not record its pid: $pid_file"
    local pid
    pid="$(cat "$pid_file")"
    if kill -0 "$pid" 2>/dev/null; then
        fail "owned fixture process is still running: $pid"
    fi
    retire_owned_process "$pid_file" "$owner_file" "$identity_file"
}

make_old_launcher() {
    local old_dir="$WORK_DIR/old-relay"
    local old_launcher="$old_dir/tailscale.sh"
    local content
    local new_block
    local old_block

    mkdir -p "$old_dir"
    cp "$ROOT/relay/common.sh" "$old_dir/common.sh"
    cp "$ROOT/relay/tailscale.sh" "$old_launcher"

    new_block="$(cat <<'EOF'
    if [ -n "$SERVE_PID" ]; then
        if ! POST="$("$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null)"; then
            echo "✗ Could not verify Tailscale Serve/Funnel state during cleanup." >&2
            echo "  The foreground session record was retained; inspect it before retrying." >&2
            status=1
        elif [ "$(json_bool_field "$POST" serve_inspected)" != true ] ||
            [ "$(json_bool_field "$POST" exposure_complete)" != true ] ||
            [ "$(json_bool_field "$POST" serve_configured)" != false ] ||
            [ "$(json_bool_field "$POST" funnel_configured)" != false ]; then
            echo "✗ Tailscale Serve/Funnel configuration remains after the foreground session stopped; it was left unchanged." >&2
            echo "  Inspect the recorded session and remove residual configuration only after confirming its owner." >&2
            status=1
        fi
    fi
EOF
)"
    new_block+=$'\n'
    old_block="$(cat <<'EOF'
    if [ -n "$SERVE_PID" ]; then
        if ! POST="$($RELAY_BIN tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null)"; then
            echo "✗ Could not verify Tailscale Serve state during cleanup." >&2
            status=1
        else
            ROUTE_COUNT="$(json_number_field "$POST" serve_route_count)"
            if [ "$(json_bool_field "$POST" serve_configured)" != true ] &&
                [ "$(json_bool_field "$POST" funnel_configured)" != true ]; then
                : # The foreground CLI may have removed its route while exiting.
            elif [ "$(json_bool_field "$POST" funnel_configured)" = true ] ||
                [ "$ROUTE_COUNT" != 1 ] ||
                [ "$(json_bool_field "$POST" serve_route_owned)" != true ]; then
                echo "✗ Refusing to remove an unverified Tailscale Serve route; inspect it before retrying." >&2
                status=1
            elif ! "$TS_BIN" serve off >/dev/null 2>&1; then
                echo "✗ Could not remove the owned Tailscale Serve session." >&2
                status=1
            elif ! POST="$($RELAY_BIN tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null)"; then
                echo "✗ Could not verify Tailscale Serve cleanup." >&2
                status=1
            elif [ "$(json_bool_field "$POST" serve_configured)" = true ] ||
                [ "$(json_bool_field "$POST" funnel_configured)" = true ]; then
                echo "✗ Tailscale Serve configuration remains after cleanup; inspect it before retrying." >&2
                status=1
            fi
        fi
    fi
EOF
)"
    old_block+=$'\n'

    content="$(cat "$old_launcher")"
    case "$content" in
        *"$new_block"*) ;;
        *) fail "could not prepare the disposable pre-patch launcher" ;;
    esac
    content="${content/"$new_block"/"$old_block"}"
    printf '%s\n' "$content" > "$old_launcher"
    chmod 700 "$old_launcher"
    printf '%s\n' "$old_launcher"
}

run_mode_precedence_case() {
    local mode_dir="$WORK_DIR/mode"
    local env_file="$mode_dir/relay.env"
    mkdir -p "$mode_dir"
    printf '%s\n' \
        "HERDR_RELAY_TOKEN='0123456789abcdef0123456789abcdef'" \
        "HERDR_RELAY_INSTANCE_ID='instance-1'" \
        "HERDR_RELAY_TRANSPORT='tailscale'" > "$env_file"
    mkdir -p "$mode_dir/home" "$mode_dir/xdg-config" "$mode_dir/xdg-data" \
        "$mode_dir/xdg-cache" "$mode_dir/xdg-state" "$mode_dir/xdg-runtime" "$mode_dir/tmp"
    env -i \
        HOME="$mode_dir/home" \
        XDG_CONFIG_HOME="$mode_dir/xdg-config" \
        XDG_DATA_HOME="$mode_dir/xdg-data" \
        XDG_CACHE_HOME="$mode_dir/xdg-cache" \
        XDG_STATE_HOME="$mode_dir/xdg-state" \
        XDG_RUNTIME_DIR="$mode_dir/xdg-runtime" \
        TMPDIR="$mode_dir/tmp" \
        PATH="$TOOLS_DIR:$CORE_DIR" \
        HERDR_RELAY_TRANSPORT=cloudflare \
        HERDR_RELAY_ENV="$env_file" \
        /bin/bash -c '. "$1/relay/common.sh"; test "$(relay_transport_mode "$2")" = tailscale' \
        bash "$ROOT" "$env_file"
    echo "PASS persisted transport beats stale inherited mode"
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

run_case() {
    local name="$1"
    local initial_state="$2"
    local running_state="$3"
    local post_state="$4"
    local expected_status="$5"
    local expected_marker="$6"
    local expected_message="$7"
    local https_port="$8"
    local inspection_failure="$9"
    local launcher="${10}"
    local negative_control="${11:-0}"
    local case_dir="$WORK_DIR/cases/$name"
    local config_dir="$case_dir/config"
    local env_file="$config_dir/relay.env"
    local output_file="$case_dir/output.txt"
    local state_file="$case_dir/state.json"
    local running_state_file="$case_dir/running.json"
    local post_state_file="$case_dir/post.json"
    local original_env="$case_dir/original.env"
    local foreign_signal="$case_dir/foreign.signal"
    local foreign_identity_file="$case_dir/foreign.identity"
    local foreign_live_identity_file="$case_dir/foreign.live.identity"
    local foreign_owner_file="$case_dir/foreign.owner"
    local owner_token="$case_dir/owner-token"
    local relay_owner_file="$case_dir/relay.owner"
    local tailscale_owner_file="$case_dir/tailscale.owner"
    local relay_identity_file="$case_dir/relay.identity"
    local tailscale_identity_file="$case_dir/tailscale.identity"
    local unexpected_log="$case_dir/unexpected.log"
    local service_log="$case_dir/service.log"
    local post_inspection_log="$case_dir/post-inspection.log"
    local arm_failure_marker="$case_dir/arm-failure.marker"
    local foreign_pid
    local status
    local token='0123456789abcdef0123456789abcdef'
    local expected_ts_log="$case_dir/tailscale.log"
    local expected_relay_log="$case_dir/relay.log"
    local expected_serve_command="serve --https=$https_port http://127.0.0.1:8375"

    mkdir -p "$config_dir" "$case_dir/home" "$case_dir/tmp" \
        "$case_dir/xdg-config" "$case_dir/xdg-data" "$case_dir/xdg-cache" \
        "$case_dir/xdg-state" "$case_dir/xdg-runtime"
    printf '%s\n' "$initial_state" > "$state_file"
    printf '%s\n' "$running_state" > "$running_state_file"
    printf '%s\n' "$post_state" > "$post_state_file"
    {
        printf "HERDR_RELAY_TOKEN='%s'\n" "$token"
        printf "HERDR_RELAY_INSTANCE_ID='instance-1'\n"
        printf "HERDR_RELAY_TRANSPORT='tailscale'\n"
        printf "HERDR_TAILSCALE_ORIGIN='https://relay.ts.net'\n"
        if [ "$https_port" != 443 ]; then
            printf "HERDR_TAILSCALE_HTTPS_PORT='%s'\n" "$https_port"
        fi
    } > "$env_file"
    cp "$env_file" "$original_env"

    start_foreign_process "$foreign_signal" "$foreign_identity_file" "$foreign_owner_file" "$owner_token" ||
        fail "$name could not start its foreign fixture sentinel"
    foreign_pid="$ACTIVE_FOREIGN_PID"
    cp "$foreign_identity_file" "$foreign_live_identity_file"

    set +e
    env -i \
        HOME="$case_dir/home" \
        XDG_CONFIG_HOME="$case_dir/xdg-config" \
        XDG_DATA_HOME="$case_dir/xdg-data" \
        XDG_CACHE_HOME="$case_dir/xdg-cache" \
        XDG_STATE_HOME="$case_dir/xdg-state" \
        XDG_RUNTIME_DIR="$case_dir/xdg-runtime" \
        TMPDIR="$case_dir/tmp" \
        PATH="$TOOLS_DIR:$CORE_DIR" \
        TERM=dumb \
        NO_COLOR=1 \
        HERDR_RELAY_ENV="$env_file" \
        HERDR_RELAY_BIN="$TOOLS_DIR/relay" \
        HERDR_TAILSCALE_BIN="$TOOLS_DIR/tailscale" \
        HERDR_TAILSCALE_YES=1 \
        HERDR_PHONE_APP_URL='https://app.example.test' \
        FAKE_STATE_FILE="$state_file" \
        FAKE_RUNNING_STATE_FILE="$running_state_file" \
        FAKE_POST_STATE_FILE="$post_state_file" \
        FAKE_RELAY_LOG="$expected_relay_log" \
        FAKE_TAILSCALE_LOG="$expected_ts_log" \
        FAKE_CURL_LOG="$case_dir/curl.log" \
        FAKE_UNEXPECTED_LOG="$unexpected_log" \
        FAKE_SERVICE_LOG="$service_log" \
        FAKE_POST_INSPECTION_LOG="$post_inspection_log" \
        FAKE_ARM_FAILURE_MARKER="$arm_failure_marker" \
        FAKE_EXPECTED_SERVE_ARG="--https=$https_port" \
        FAKE_EXPECTED_SERVE_TARGET='http://127.0.0.1:8375' \
        FAKE_OWNER_TOKEN="$owner_token" \
        FAKE_RELAY_OWNER_FILE="$relay_owner_file" \
        FAKE_TAILSCALE_OWNER_FILE="$tailscale_owner_file" \
        FAKE_RELAY_IDENTITY_FILE="$relay_identity_file" \
        FAKE_TAILSCALE_IDENTITY_FILE="$tailscale_identity_file" \
        FAKE_RELAY_PID_FILE="$case_dir/relay.pid" \
        FAKE_TAILSCALE_PID_FILE="$case_dir/tailscale.pid" \
        FAKE_RELAY_STOP_MARKER="$case_dir/relay.stopped" \
        FAKE_STOP_MARKER="$case_dir/tailscale.stopped" \
        FAKE_ARM_FAILURE=1 \
        FAKE_INSPECTION_FAIL_AFTER_STOP="$inspection_failure" \
        "$TIMEOUT_BIN" 25 /bin/bash "$launcher" --confirm-serve > "$output_file" 2>&1
    status=$?
    set -e
    register_owned_process "$case_dir/relay.pid" "$relay_owner_file" "$owner_token" "$relay_identity_file"
    register_owned_process "$case_dir/tailscale.pid" "$tailscale_owner_file" "$owner_token" "$tailscale_identity_file"

    if [ "$status" -ne "$expected_status" ]; then
        echo "bounded launcher output for $name:" >&2
        sed -n '1,120p' "$output_file" >&2 || true
        fail "$name returned $status, expected $expected_status (bounded output in $output_file)"
    fi

    case "$name" in
        pre-existing-route-refusal|incomplete-inspection-refusal|fixture-mutant-missing-exposure-complete)
            [ ! -e "$case_dir/relay.pid" ] || fail "$name started the relay child"
            [ ! -e "$case_dir/tailscale.pid" ] || fail "$name started the Serve child"
            [ ! -e "$post_inspection_log" ] || fail "$name performed post-stop inspection"
            [ ! -e "$arm_failure_marker" ] || fail "$name armed an invitation"
            ;;
        *)
        [ -e "$case_dir/relay.pid" ] || fail "$name never started the relay child"
        [ -e "$case_dir/tailscale.pid" ] || fail "$name never started the Serve child"
        assert_process_stopped "$case_dir/relay.pid" "$relay_owner_file" "$relay_identity_file"
        assert_process_stopped "$case_dir/tailscale.pid" "$tailscale_owner_file" "$tailscale_identity_file"
        [ ! -e "$case_dir/relay.pid" ] || fail "$name left a completed relay PID record"
        [ ! -e "$case_dir/tailscale.pid" ] || fail "$name left a completed Serve PID record"
        [ -e "$case_dir/relay.stopped" ] || fail "$name did not stop the owned relay child"
        [ -e "$case_dir/tailscale.stopped" ] || fail "$name did not stop the owned Serve child"
        [ ! -e "$relay_owner_file" ] || fail "$name left a relay ownership marker"
        [ ! -e "$tailscale_owner_file" ] || fail "$name left a Serve ownership marker"
        [ ! -e "$relay_identity_file" ] || fail "$name left a relay identity record"
        [ ! -e "$tailscale_identity_file" ] || fail "$name left a Serve identity record"
            [ -e "$arm_failure_marker" ] || fail "$name did not reach deterministic post-start failure"
            assert_single_line "$post_inspection_log" "tailscale inspect --binary $TOOLS_DIR/tailscale --https-port $https_port"
            ;;
    esac
    assert_single_line "$service_log" '--user is-active --quiet herdr-mobile-relay.service'
    if [ "$negative_control" = 1 ]; then
        assert_single_line "$unexpected_log" 'serve off'
    else
        assert_empty_log "$unexpected_log"
    fi
    if [ "${FAKE_FIXTURE_CLEANUP_REFUSAL:-0}" = 1 ] &&
        [ "$name" = residual-single-route ]; then
        printf '%s|fixture-stale\n' "$foreign_pid" > "$foreign_identity_file"
    fi
    if ! foreign_identity_matches "$foreign_pid" "$ACTIVE_FOREIGN_START_TIME" \
        "$foreign_identity_file" "$foreign_owner_file" "$owner_token"; then
        fail "$name signaled or replaced the foreign fixture process"
    fi
    [ ! -e "$foreign_signal" ] || fail "$name delivered a signal to the foreign fixture process"
    stop_foreign_process || fail "$name could not safely stop its own foreign fixture process"
    [ ! -e "$foreign_identity_file" ] || fail "$name left a foreign identity record"
    [ ! -e "${foreign_identity_file}.ready" ] || fail "$name left a foreign readiness record"
    [ ! -e "$foreign_owner_file" ] || fail "$name left a foreign ownership marker"

    if ! cmp -s "$post_state_file" "$state_file"; then
        echo "relay log for $name:" >&2
        cat "$expected_relay_log" >&2 || true
        echo "tailscale log for $name:" >&2
        cat "$expected_ts_log" >&2 || true
        sed -n '1,120p' "$output_file" >&2 || true
    fi
    assert_same_file "$post_state_file" "$state_file"
    if [ "$name" = positively-empty-post-stop ]; then
        [ "$(cat "$state_file")" = "$EMPTY_STATE" ] ||
            fail "$name did not record explicit empty Serve/Funnel post-stop state"
    fi
    assert_same_file "$original_env" "$env_file"
    [ ! -e "$config_dir/phone-app-origin-configured" ] ||
        fail "$name left a rolled-back phone-app origin"

    if [ "$expected_marker" = 1 ]; then
        [ -e "$config_dir/tailscale-session.env" ] ||
            fail "$name did not retain the recovery marker"
    else
        [ ! -e "$config_dir/tailscale-session.env" ] ||
            fail "$name retained a marker after positively empty inspection"
    fi
    [ -n "$expected_message" ] && assert_contains "$output_file" "$expected_message"
    assert_not_contains "$output_file" 'Scan this QR code'
    assert_not_contains "$output_file" 'Open this private setup link'
    assert_not_contains "$output_file" "$token"
    case "$name" in
        pre-existing-route-refusal|incomplete-inspection-refusal|fixture-mutant-missing-exposure-complete)
            [ ! -e "$expected_ts_log" ] || fail "$name invoked the Tailscale adapter"
            [ ! -e "$arm_failure_marker" ] || fail "$name armed an invitation"
            assert_not_contains "$expected_relay_log" 'pairing-control'
            assert_not_contains "$expected_relay_log" 'serve'
            ;;
        *)
            assert_tailscale_allowlist "$expected_ts_log" "$expected_serve_command" "$negative_control"
            ;;
    esac
    if [ "$https_port" != 443 ] && [ "$negative_control" = 0 ]; then
        assert_contains "$expected_ts_log" "--https=$https_port"
        assert_contains "$expected_relay_log" "--https-port $https_port"
        assert_not_contains "$expected_ts_log" '--https=443'
        assert_not_contains "$expected_relay_log" '--https-port 443'
    fi

    echo "PASS $name (status=$status)"
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

EMPTY_STATE='{"backend_state":"Running","logged_in":true,"origin":"https://relay.ts.net","serve_configured":false,"funnel_configured":false,"serve_route_count":0,"serve_route_owned":false,"serve_inspected":true,"exposure_complete":true}'
RUNNING_ROUTE='{"backend_state":"Running","logged_in":true,"origin":"https://relay.ts.net","serve_configured":true,"funnel_configured":false,"serve_route_count":1,"serve_route_owned":true,"serve_route_origin":"https://relay.ts.net","serve_inspected":true,"exposure_complete":true}'
RESIDUAL_ROUTE="$RUNNING_ROUTE"
REPLACEMENT_ROUTE='{"backend_state":"Running","logged_in":true,"origin":"https://relay.ts.net","serve_configured":true,"funnel_configured":false,"serve_route_count":1,"serve_route_owned":true,"serve_route_origin":"https://other.ts.net","serve_inspected":true,"exposure_complete":true}'
MULTIPLE_FOREIGN='{"backend_state":"Running","logged_in":true,"origin":"https://relay.ts.net","serve_configured":true,"funnel_configured":false,"serve_route_count":2,"serve_route_owned":false,"serve_route_origin":"https://foreign.ts.net","serve_inspected":true,"exposure_complete":true}'
FUNNEL_RESIDUAL='{"backend_state":"Running","logged_in":true,"origin":"https://relay.ts.net","serve_configured":false,"funnel_configured":true,"serve_route_count":0,"serve_route_owned":false,"serve_inspected":true,"exposure_complete":true}'
INCOMPLETE_FALSE_STATE="$(printf '%s' "$EMPTY_STATE" | sed 's/\"serve_inspected\":true/\"serve_inspected\":false/')"
INCOMPLETE_MISSING_EXPOSURE="$(printf '%s' "$EMPTY_STATE" | sed 's/,\"exposure_complete\":true//')"

run_mode_precedence_case

run_case \
    pre-existing-route-refusal \
    "$RUNNING_ROUTE" \
    "$RUNNING_ROUTE" \
    "$RUNNING_ROUTE" \
    1 0 \
    'Existing Tailscale Serve configuration was found' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    incomplete-inspection-refusal \
    "$INCOMPLETE_FALSE_STATE" \
    "$INCOMPLETE_FALSE_STATE" \
    "$INCOMPLETE_FALSE_STATE" \
    1 0 \
    'Tailscale exposure inspection is incomplete' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    fixture-mutant-missing-exposure-complete \
    "$INCOMPLETE_MISSING_EXPOSURE" \
    "$INCOMPLETE_MISSING_EXPOSURE" \
    "$INCOMPLETE_MISSING_EXPOSURE" \
    1 0 \
    'Tailscale exposure inspection is incomplete' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    residual-single-route \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$RESIDUAL_ROUTE" \
    1 1 \
    'Tailscale Serve/Funnel configuration remains after the foreground session stopped' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    residual-custom-port-8443 \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$RESIDUAL_ROUTE" \
    1 1 \
    'Tailscale Serve/Funnel configuration remains after the foreground session stopped' \
    8443 0 "$ROOT/relay/tailscale.sh"

run_case \
    same-origin-replacement-route \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$REPLACEMENT_ROUTE" \
    1 1 \
    'Tailscale Serve/Funnel configuration remains after the foreground session stopped' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    multiple-foreign-routes \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$MULTIPLE_FOREIGN" \
    1 1 \
    'Tailscale Serve/Funnel configuration remains after the foreground session stopped' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    funnel-residual-state \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$FUNNEL_RESIDUAL" \
    1 1 \
    'Tailscale Serve/Funnel configuration remains after the foreground session stopped' \
    443 0 "$ROOT/relay/tailscale.sh"

run_case \
    cleanup-inspection-failure \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$EMPTY_STATE" \
    1 1 \
    'Could not verify Tailscale Serve/Funnel state during cleanup' \
    443 1 "$ROOT/relay/tailscale.sh"

run_case \
    positively-empty-post-stop \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$EMPTY_STATE" \
    1 0 \
    '' \
    443 0 "$ROOT/relay/tailscale.sh"

OLD_LAUNCHER="$(make_old_launcher)"
run_case \
    negative-control-old-unsafe-cleanup \
    "$EMPTY_STATE" \
    "$RUNNING_ROUTE" \
    "$RESIDUAL_ROUTE" \
    1 1 \
    'Could not remove the owned Tailscale Serve session' \
    443 0 "$OLD_LAUNCHER" 1

run_startup_identity_handshake_case() {
    local case_dir="$WORK_DIR/cases/startup-identity-handshake"
    local signal_file="$case_dir/foreign.signal"
    local identity_file="$case_dir/foreign.identity"
    local owner_file="$case_dir/foreign.owner"
    local failed_signal_file="$case_dir/failed.signal"
    local failed_identity_file="$case_dir/failed.identity"
    local failed_owner_file="$case_dir/failed.owner"
    local failed_start_log="$case_dir/failed-start.log"
    local pid

    mkdir -p "$case_dir"
    if ! FAKE_FOREIGN_PARENT_DELAY_TENTHS=3 start_foreign_process \
        "$signal_file" "$identity_file" "$owner_file" \
        startup-handshake-owner normal; then
        fail 'startup-identity-handshake rejected a delayed child-owned identity'
    fi
    pid="$ACTIVE_FOREIGN_PID"
    foreign_record_matches "$pid" "$ACTIVE_FOREIGN_START_TIME" \
        "$identity_file" "$owner_file" startup-handshake-owner ||
        fail 'startup-identity-handshake did not retain the child-published record'
    stop_foreign_process || fail 'startup-identity-handshake could not stop its delayed sentinel'
    assert_single_line "$signal_file" signaled

    if start_foreign_process "$failed_signal_file" "$failed_identity_file" \
        "$failed_owner_file" failed-start-owner fail 2>"$failed_start_log"; then
        fail 'failed-start-handshake was accepted without a child registration'
    fi
    assert_contains "$failed_start_log" 'child-owned startup handshake'
    [ -z "$ACTIVE_FOREIGN_PID" ] || fail 'failed-start-handshake left an active PID handle'
    [ ! -e "$failed_identity_file" ] || fail 'failed-start-handshake left an identity record'
    [ ! -e "${failed_identity_file}.ready" ] || fail 'failed-start-handshake left a readiness record'
    [ ! -e "$failed_owner_file" ] || fail 'failed-start-handshake left an owner record'
    [ ! -e "$failed_signal_file" ] || fail 'failed-start-handshake signaled a failed child'
    echo 'PASS startup-identity-handshake (delayed child publication accepted; failed startup rejected)'
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

run_absent_foreign_handle_case() {
    local case_dir="$WORK_DIR/cases/absent-foreign-handle"
    local signal_file="$case_dir/foreign.signal"
    local identity_file="$case_dir/foreign.identity"
    local owner_file="$case_dir/foreign.owner"
    local refusal_log="$case_dir/refusal.log"
    local owner_token='absent-foreign-owner'
    local pid
    local start_time
    local absent_pid=999999

    mkdir -p "$case_dir"
    while [ -e "/proc/$absent_pid" ]; do
        absent_pid=$((absent_pid + 1))
        [ "$absent_pid" -lt 1000050 ] || fail 'absent-foreign-handle could not find a private absent PID'
    done
    start_foreign_process "$signal_file" "$identity_file" "$owner_file" "$owner_token" ||
        fail 'absent-foreign-handle could not start its sentinel'
    pid="$ACTIVE_FOREIGN_PID"
    start_time="$ACTIVE_FOREIGN_START_TIME"
    ACTIVE_FOREIGN_PID="$absent_pid"
    if stop_foreign_process 2>"$refusal_log"; then
        fail 'absent-foreign-handle authorized a substituted absent PID'
    fi
    assert_contains "$refusal_log" 'refusing to signal foreign fixture'
    process_is_live "$pid" || fail 'absent-foreign-handle lost its original live sentinel'
    [ ! -e "$signal_file" ] || fail 'absent-foreign-handle signaled its test-owned sentinel'
    [ -e "$identity_file" ] || fail 'absent-foreign-handle erased its identity record'
    [ -e "$owner_file" ] || fail 'absent-foreign-handle erased its owner record'

    ACTIVE_FOREIGN_PID="$pid"
    ACTIVE_FOREIGN_START_TIME="$start_time"
    stop_foreign_process || fail 'absent-foreign-handle could not retire its restored identity safely'
    echo 'PASS absent-foreign-handle (dead-PID substitution refused before record retirement)'
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

run_absent_owned_handle_case() {
    local case_dir="$WORK_DIR/cases/absent-owned-handle"
    local signal_file="$case_dir/owned.signal"
    local identity_file="$case_dir/owned.identity"
    local owner_file="$case_dir/owned.owner"
    local pid_file="$case_dir/owned.pid"
    local refusal_log="$case_dir/refusal.log"
    local owner_token='absent-owned-owner'
    local pid
    local start_time
    local ready_file
    local saved_identity_file
    local saved_ready_file
    local saved_owner_file
    local saved_owner_token

    mkdir -p "$case_dir"
    start_foreign_process "$signal_file" "$identity_file" "$owner_file" "$owner_token" ||
        fail 'absent-owned-handle could not start its sentinel'
    pid="$ACTIVE_FOREIGN_PID"
    start_time="$ACTIVE_FOREIGN_START_TIME"
    ready_file="$ACTIVE_FOREIGN_READY_FILE"
    printf '%s\n' "$pid" > "$pid_file"
    register_owned_process "$pid_file" "$owner_file" "$owner_token" "$identity_file"
    [ "${#ACTIVE_OWNED_PROCESSES[@]}" -eq 1 ] || fail 'absent-owned-handle was not registered'
    saved_identity_file="$ACTIVE_FOREIGN_IDENTITY_FILE"
    saved_ready_file="$ACTIVE_FOREIGN_READY_FILE"
    saved_owner_file="$ACTIVE_FOREIGN_OWNER_FILE"
    saved_owner_token="$ACTIVE_FOREIGN_OWNER_TOKEN"
    clear_foreign_handle
    ACTIVE_OWNED_PROCESSES=("999999|$start_time|$pid_file|$owner_file|$owner_token|$identity_file")
    if cleanup_owned_processes 2>"$refusal_log"; then
        fail 'absent-owned-handle authorized a substituted dead PID'
    fi
    assert_contains "$refusal_log" 'refusing to retire owned fixture'
    process_is_live "$pid" || fail 'absent-owned-handle lost its original live sentinel'
    [ ! -e "$signal_file" ] || fail 'absent-owned-handle signaled its test-owned sentinel'
    [ -e "$identity_file" ] || fail 'absent-owned-handle erased its identity record'
    [ -e "$owner_file" ] || fail 'absent-owned-handle erased its owner record'

    ACTIVE_OWNED_PROCESSES=()
    ACTIVE_FOREIGN_PID="$pid"
    ACTIVE_FOREIGN_START_TIME="$start_time"
    ACTIVE_FOREIGN_IDENTITY_FILE="$saved_identity_file"
    ACTIVE_FOREIGN_READY_FILE="$saved_ready_file"
    ACTIVE_FOREIGN_OWNER_FILE="$saved_owner_file"
    ACTIVE_FOREIGN_OWNER_TOKEN="$saved_owner_token"
    stop_foreign_process || fail 'absent-owned-handle could not retire its restored identity safely'
    rm -f "$pid_file"
    echo 'PASS absent-owned-handle (dead-PID substitution refused before owned-record retirement)'
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

run_stale_foreign_handle_case() {
    local case_dir="$WORK_DIR/cases/stale-active-foreign"
    local signal_file="$case_dir/foreign.signal"
    local live_identity_file="$case_dir/foreign.live.identity"
    local stale_identity_file="$case_dir/foreign.stale.identity"
    local owner_file="$case_dir/foreign.owner"
    local refusal_log="$case_dir/refusal.log"
    local owner_token='stale-active-foreign-owner'
    local pid
    local live_start_time

    mkdir -p "$case_dir"
    start_foreign_process "$signal_file" "$live_identity_file" "$owner_file" "$owner_token" ||
        fail 'stale-active-foreign-handle could not start its sentinel'
    pid="$ACTIVE_FOREIGN_PID"
    live_start_time="$ACTIVE_FOREIGN_START_TIME"
    printf '%s|reused-before-registration\n' "$pid" > "$stale_identity_file"
    ACTIVE_FOREIGN_IDENTITY_FILE="$stale_identity_file"
    if stop_foreign_process 2>"$refusal_log"; then
        fail 'stale-active-foreign-handle authorized a signal'
    fi
    assert_contains "$refusal_log" 'refusing to signal foreign fixture'
    process_is_live "$pid" || fail 'stale-active-foreign-handle lost its live sentinel'
    [ ! -e "$signal_file" ] || fail 'stale-active-foreign-handle signaled its test-owned sentinel'

    ACTIVE_FOREIGN_START_TIME="$live_start_time"
    ACTIVE_FOREIGN_IDENTITY_FILE="$live_identity_file"
    stop_foreign_process || fail 'stale-active-foreign-handle could not retire the current identity safely'
    [ ! -e "$live_identity_file" ] || fail 'stale-active-foreign-handle left its identity record'
    [ ! -e "$owner_file" ] || fail 'stale-active-foreign-handle left its owner marker'
    echo 'PASS stale-active-foreign-handle (stale identity rejected; current identity retired with bounded cleanup)'
    CASES=$((CASES + 1))
    PASSED=$((PASSED + 1))
}

run_startup_identity_handshake_case
run_absent_foreign_handle_case
run_absent_owned_handle_case
run_stale_foreign_handle_case

printf 'supplemental Tailscale cleanup safety tests passed (%d cases; old-branch negative control attempted serve off)\n' "$PASSED"

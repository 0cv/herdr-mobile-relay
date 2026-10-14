#!/usr/bin/env bash

set +x

private_owned_file() {
    local file_identity
    [ -f "$1" ] && [ ! -L "$1" ] || return 1
    file_identity=$(stat -c '%u:%a:%h' "$1" 2>/dev/null) ||
        file_identity=$(stat -f '%u:%Lp:%l' "$1" 2>/dev/null) || return 1
    [ "$file_identity" = "$(id -u):600:1" ]
}

# Rollback calls these helpers from EXIT traps, where Bash gives a bare
# `return` the status from before the trap; return explicit statuses.
systemd_quoted() {
    local value="$1"
    case "$value" in *[!a-zA-Z0-9_./:=+-]*) ;; *) printf '%s' "$value"; return $? ;; esac
    value=${value//\\/\\\\}
    value=${value//\"/\\\"}
    value=${value//%/%%}
    if [ "${2:-}" = exec ]; then value=${value//\$/\$\$}; fi
    case "$value" in *$'\n'*|*$'\r'*) return 1 ;; esac
    printf '"%s"' "$value"
}

systemd_field() {
    local file="$1" key="$2" line value
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in "$key="*) ;; *) continue ;; esac
        value=${line#*=}
        case "$value" in
            \"*\") value=${value#\"}; value=${value%\"} ;;
            \"*) return 1 ;;
        esac
        value=${value//\\\"/\"}
        value=${value//\\\\/\\}
        value=${value//%%/%}
        if [ "$key" = ExecStart ]; then value=${value//\$\$/\$}; fi
        if [ "$key" = Environment ]; then
            case "$value" in HERDR_RELAY_ENV=*) value=${value#HERDR_RELAY_ENV=} ;; *) continue ;; esac
            # Accept the branch's older value-quoted assignment as well as
            # main's complete-assignment quoting; both must round-trip.
            case "$value" in
                \"*\") value=${value#\"}; value=${value%\"} ;;
                \"*) return 1 ;;
            esac
        fi
        printf '%s\n' "$value"
        return 0
    done < "$file"
    return 1
}

# Keep only terminal generations already validated by jobs -l and passed to
# wait; a later stop may accept their completion after Bash prunes its record.
CHILD_JOB_REAPED_SPECS=()
CHILD_JOB_REAPED_PIDS=()
CHILD_JOB_RECORD_PRESENT=false

# A numeric PID can be reused as soon as its process exits. Bash job specs are
# tied to this shell's job table, so managed foreground children keep both the
# jobspec generation and the original PID for identity checks and wait only.
# Capture jobs output through a file because command substitution runs the
# builtin in a subshell whose copied job table can omit terminal records.
_child_job_long_record() {
    local job_spec="$1"
    local output_file
    local line
    local line_count=0

    CHILD_JOB_OUTPUT=""
    CHILD_JOB_RECORD_PRESENT=false
    output_file="$(mktemp "${TMPDIR:-/tmp}/herdr-child-job.XXXXXX")" || return 1
    if ! LC_ALL=C jobs -l "$job_spec" > "$output_file" 2>/dev/null; then
        rm -f "$output_file"
        return 1
    fi
    while IFS= read -r line || [ -n "$line" ]; do
        line_count=$((line_count + 1))
        if [ "$line_count" -eq 1 ]; then
            CHILD_JOB_OUTPUT="$line"
        fi
    done < "$output_file"
    rm -f "$output_file" || return 1
    [ "$line_count" -eq 0 ] || CHILD_JOB_RECORD_PRESENT=true
    [ "$line_count" -eq 1 ] && [ -n "$CHILD_JOB_OUTPUT" ]
}

_child_job_record_state() {
    local expected_pid="$1"
    local job_number="$2"
    local job_output="$3"
    local job_token
    local listed_pid
    local state

    [ -n "$job_output" ] || return 1
    case "$job_output" in
        *$'\n'*) return 1 ;;
    esac
    IFS=$' \t' read -r job_token listed_pid state _ <<< "$job_output"
    case "$job_token" in
        "[$job_number]"|"[$job_number]+"|"[$job_number]-") ;;
        *) return 1 ;;
    esac
    [ "$listed_pid" = "$expected_pid" ] || return 1

    case "$state" in
        Running|Stopped)
            CHILD_JOB_STATE="$state"
            return 0
            ;;
        Done|Exit|Terminated|Killed|Aborted|Hangup|Segmentation|Floating)
            CHILD_JOB_STATE="$state"
            return 2
            ;;
        *)
            return 1
            ;;
    esac
}

# This cache is never a signal authority. A live or mismatched current record
# still fails identity checks; absence is accepted only for this exact pair
# after a validated terminal record was already waited in this shell.
_child_job_mark_reaped() {
    local job_spec="$1"
    local expected_pid="$2"
    local index

    for ((index = 0; index < ${#CHILD_JOB_REAPED_SPECS[@]}; index++)); do
        if [ "${CHILD_JOB_REAPED_SPECS[index]}" = "$job_spec" ]; then
            CHILD_JOB_REAPED_PIDS[index]="$expected_pid"
            return 0
        fi
    done
    CHILD_JOB_REAPED_SPECS+=("$job_spec")
    CHILD_JOB_REAPED_PIDS+=("$expected_pid")
}

_child_job_was_reaped() {
    local job_spec="$1"
    local expected_pid="$2"
    local index

    for ((index = 0; index < ${#CHILD_JOB_REAPED_SPECS[@]}; index++)); do
        if [ "${CHILD_JOB_REAPED_SPECS[index]}" = "$job_spec" ] &&
            [ "${CHILD_JOB_REAPED_PIDS[index]}" = "$expected_pid" ]; then
            return 0
        fi
    done
    return 1
}

_child_job_details() {
    local job_spec="$1"
    local expected_pid="$2"
    local job_number
    local mapped_pid
    local job_output
    local record_status
    local CHILD_JOB_OUTPUT=""

    case "$job_spec" in
        %[0-9]*) job_number="${job_spec#%}" ;;
        *) return 1 ;;
    esac
    case "$job_number" in
        ''|*[!0-9]*) return 1 ;;
    esac
    case "$expected_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac

    _child_job_long_record "$job_spec" || return 1
    job_output="$CHILD_JOB_OUTPUT"
    if _child_job_record_state "$expected_pid" "$job_number" "$job_output"; then
        record_status=0
    else
        record_status=$?
    fi
    [ "$record_status" -eq 2 ] && return 2
    [ "$record_status" -eq 0 ] || return 1

    case "$CHILD_JOB_STATE" in
        Running|Stopped) ;;
        *) return 1 ;;
    esac
    if mapped_pid="$(LC_ALL=C jobs -p "$job_spec" 2>/dev/null)"; then
        [ "$mapped_pid" = "$expected_pid" ] && return 0
        [ -z "$mapped_pid" ] || return 1
    else
        [ -z "$mapped_pid" ] || return 1
    fi

    # A child may finish after the long-form record was read but before Bash
    # resolves the live PID mapping. Reclassify once, from the fresh job record;
    # missing or still-active records remain an unknown-identity refusal.
    _child_job_long_record "$job_spec" || return 1
    job_output="$CHILD_JOB_OUTPUT"
    if _child_job_record_state "$expected_pid" "$job_number" "$job_output"; then
        return 1
    else
        record_status=$?
    fi
    [ "$record_status" -eq 2 ] && return 2
    return 1
}

capture_child_job() {
    local expected_pid="$1"
    local job_output
    local job_token
    local listed_pid
    local state
    local job_number
    local job_spec
    local mapped_pid

    case "$expected_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    job_output="$(LC_ALL=C jobs -l %+ 2>/dev/null)" || return 1
    IFS=' ' read -r job_token listed_pid state _ <<< "$job_output"
    case "$job_token" in
        \[*\]+|\[*\]) job_number="${job_token#\[}"; job_number="${job_number%%\]*}" ;;
        *) return 1 ;;
    esac
    case "$job_number" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$listed_pid" = "$expected_pid" ] || return 1
    case "$state" in
        Running|Stopped) ;;
        *) return 1 ;;
    esac

    job_spec="%$job_number"
    mapped_pid="$(LC_ALL=C jobs -p "$job_spec" 2>/dev/null)" || return 1
    [ "$mapped_pid" = "$expected_pid" ] || return 1
    CHILD_JOB_STATE=""
    _child_job_details "$job_spec" "$expected_pid" || return 1
    printf '%s\n' "$job_spec"
}

child_job_active() {
    local CHILD_JOB_STATE=""
    local details_status

    if _child_job_details "$1" "$2"; then
        return 0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        # Reap only after the exact jobspec/PID terminal record was validated.
        # Bash may prune that record before the launcher's EXIT cleanup runs.
        wait "$1" 2>/dev/null || true
        _child_job_mark_reaped "$1" "$2"
    fi
    return 1
}

_child_job_reap_forced_kill() {
    local job_spec="$1"
    local expected_pid="$2"
    local observation_limit=5
    local attempt
    local details_status
    local wait_status
    local CHILD_JOB_STATE=""

    # Five fresh observations with at most four 100ms sleeps bound this proof
    # to 400ms after KILL; no further signal is sent from this loop.
    for ((attempt = 1; attempt <= observation_limit; attempt++)); do
        if _child_job_details "$job_spec" "$expected_pid"; then
            details_status=0
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            [ "$CHILD_JOB_STATE" = Killed ] || return 1
            if wait "$expected_pid" 2>/dev/null; then
                wait_status=0
            else
                wait_status=$?
            fi
            [ "$wait_status" -eq 137 ] || return 1
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        [ "$details_status" -eq 0 ] || return 1
        case "$CHILD_JOB_STATE" in
            Running|Stopped) ;;
            *) return 1 ;;
        esac
        if [ "$attempt" -lt "$observation_limit" ]; then
            sleep 0.1
        fi
    done
    return 1
}

stop_child_job() {
    local job_spec="$1"
    local expected_pid="$2"
    local signal="$3"
    local attempts="${4:-5}"
    local attempt
    local details_status
    local CHILD_JOB_STATE=""

    case "$signal" in
        ''|*[!A-Za-z0-9]*) return 1 ;;
    esac
    case "$attempts" in
        ''|*[!0-9]*|0) return 1 ;;
    esac

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    if [ "$details_status" -ne 0 ]; then
        if [ "$CHILD_JOB_RECORD_PRESENT" != true ] &&
            _child_job_was_reaped "$job_spec" "$expected_pid"; then
            return 0
        fi
        return 1
    fi

    if [ "$CHILD_JOB_STATE" = Stopped ]; then
        if _child_job_details "$job_spec" "$expected_pid"; then
            details_status=0
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            wait "$job_spec" 2>/dev/null || true
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        [ "$details_status" -eq 0 ] || return 1
        [ "$CHILD_JOB_STATE" = Stopped ] || return 1
        kill -CONT "$job_spec" 2>/dev/null || return 1
    fi

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    [ "$details_status" -eq 0 ] || return 1
    [ "$CHILD_JOB_STATE" = Running ] || return 1
    kill "-$signal" "$job_spec" 2>/dev/null || return 1

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        sleep 1
        if _child_job_details "$job_spec" "$expected_pid"; then
            continue
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            wait "$job_spec" 2>/dev/null || true
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        return 1
    done

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    [ "$details_status" -eq 0 ] || return 1
    kill -KILL "$job_spec" 2>/dev/null || return 1
    _child_job_reap_forced_kill "$job_spec" "$expected_pid"
}

# Installed-service supervision has a separate, status-preserving contract.
# Call these functions DIRECTLY in the owning shell, never in $(...). The
# existing foreground activity/stop helpers above intentionally remain unchanged.
# Return 0 = Running/Stopped, 2 = validated/reaped Terminal, 1 = unknown.
# Results live in SUPERVISED_JOB_SPEC/STATE/STATUS, not stdout.
SUPERVISED_CAPTURED_COUNT=0
SUPERVISED_CAPTURED_SPECS=()
SUPERVISED_CAPTURED_PIDS=()
SUPERVISED_REAPED_COUNT=0
SUPERVISED_REAPED_SPECS=()
SUPERVISED_REAPED_PIDS=()
SUPERVISED_REAPED_STATUSES=()
SUPERVISED_JOB_SPEC=""
SUPERVISED_JOB_STATE=""
SUPERVISED_JOB_STATUS=""

_supervised_child_cached() {
    local job_spec="$1" expected_pid="$2" index
    for ((index = 0; index < SUPERVISED_REAPED_COUNT; index++)); do
        if [ "${SUPERVISED_REAPED_SPECS[index]}" = "$job_spec" ] &&
            [ "${SUPERVISED_REAPED_PIDS[index]}" = "$expected_pid" ]; then
            SUPERVISED_JOB_STATE=Terminal
            SUPERVISED_JOB_STATUS="${SUPERVISED_REAPED_STATUSES[index]}"
            return 0
        fi
    done
    return 1
}

_supervised_child_capture_records() {
    local output_file line
    SUPERVISED_JOB_RECORDS=""
    output_file="$(mktemp "${TMPDIR:-/tmp}/herdr-supervised-jobs.XXXXXX")" || return 1
    # %+ need not select a just-completed child when other jobs are Running.
    # Read the owning parent's table, not process discovery or a subshell copy.
    if ! LC_ALL=C jobs -l > "$output_file" 2>/dev/null; then
        rm -f "$output_file"
        return 1
    fi
    while IFS= read -r line || [ -n "$line" ]; do
        SUPERVISED_JOB_RECORDS="$SUPERVISED_JOB_RECORDS$line"$'\n'
    done < "$output_file"
    rm -f "$output_file" || return 1
    [ -n "$SUPERVISED_JOB_RECORDS" ]
}

_supervised_child_record_state() {
    local expected_pid="$1" job_number="$2" job_output="$3" record_status=0
    local job_token listed_pid description

    _child_job_record_state "$expected_pid" "$job_number" "$job_output" || record_status=$?
    [ "$record_status" -eq 1 ] || return "$record_status"

    # The foreground parser's unknown result can mean an identity mismatch,
    # not just an unfamiliar label. Revalidate the exact pair before extending
    # its terminal vocabulary; an unknown generation never permits a wait.
    [ -n "$job_output" ] || return 1
    case "$job_output" in *$'\n'*) return 1 ;; esac
    IFS=$' \t' read -r job_token listed_pid description <<< "$job_output"
    case "$job_token" in
        "[$job_number]"|"[$job_number]+"|"[$job_number]-") ;;
        *) return 1 ;;
    esac
    [ "$listed_pid" = "$expected_pid" ] || return 1

    # LC_ALL=C Bash signal-terminal descriptions on Linux/Darwin include
    # multiword labels and Darwin's colon suffix. A word/colon boundary avoids
    # accepting unrelated prefixes. These labels only authorize a terminal
    # wait; the status comes from wait, NEVER a guessed signal/description.
    # Keep this extension private to supervision: foreground contracts do not
    # change. Running/Stopped were classified by the original parser above.
    case "$description" in
        "Hangup"[[:space:]:]*|"Interrupt"[[:space:]:]*|"Quit"[[:space:]:]*|\
        "Killed"[[:space:]:]*|"Terminated"[[:space:]:]*|"Aborted"[[:space:]:]*|\
        "Illegal instruction"[[:space:]:]*|"Trace/BPT trap"[[:space:]:]*|\
        "Trace/breakpoint trap"[[:space:]:]*|"IOT trap"[[:space:]:]*|\
        "Abort trap"[[:space:]:]*|"EMT trap"[[:space:]:]*|"Bus error"[[:space:]:]*|\
        "Broken pipe"[[:space:]:]*|"Alarm clock"[[:space:]:]*|\
        "User defined signal 1"[[:space:]:]*|"User defined signal 2"[[:space:]:]*|\
        "Bad system call"[[:space:]:]*|"Stack fault"[[:space:]:]*|\
        "I/O possible"[[:space:]:]*|"IO possible"[[:space:]:]*|\
        "CPU time limit exceeded"[[:space:]:]*|"Cputime limit exceeded"[[:space:]:]*|\
        "File size limit exceeded"[[:space:]:]*|"Filesize limit exceeded"[[:space:]:]*|\
        "Virtual timer expired"[[:space:]:]*|"Profiling timer expired"[[:space:]:]*|\
        "Power failure"[[:space:]:]*|"Resource lost"[[:space:]:]*|\
        "Signal "[0-9]*|"Unknown signal "[0-9]*|"Real-time signal "[0-9]*)
            CHILD_JOB_STATE=Terminal
            return 2
            ;;
        *) return 1 ;;
    esac
}

_supervised_child_record() {
    local job_spec="$1" expected_pid="$2" job_output="$3"
    local record_status=0 index
    local CHILD_JOB_STATE=""

    if _supervised_child_cached "$job_spec" "$expected_pid"; then
        return 2
    fi
    _supervised_child_record_state "$expected_pid" "${job_spec#%}" "$job_output" || record_status=$?
    SUPERVISED_JOB_STATE="$CHILD_JOB_STATE"
    [ "$record_status" -ne 1 ] || return 1
    [ "$record_status" -eq 2 ] || return 0

    # Only an exact terminal jobspec/PID record permits this nonblocking wait.
    # jobs -l may prune it; wait by original PID, not a subsequently reused spec.
    # Cache completion/status only: this cache can NEVER authorize a signal.
    SUPERVISED_JOB_STATE=Terminal
    SUPERVISED_JOB_STATUS=0
    wait "$expected_pid" 2>/dev/null || SUPERVISED_JOB_STATUS=$?
    index=$SUPERVISED_REAPED_COUNT
    SUPERVISED_REAPED_SPECS[index]="$job_spec"
    SUPERVISED_REAPED_PIDS[index]="$expected_pid"
    SUPERVISED_REAPED_STATUSES[index]="$SUPERVISED_JOB_STATUS"
    SUPERVISED_REAPED_COUNT=$((SUPERVISED_REAPED_COUNT + 1))
    return 2
}

supervised_child_capture() {
    local expected_pid="$1" token listed_pid state number record_status=0
    local line candidate="" matches=0 index known_status
    SUPERVISED_JOB_SPEC=""
    SUPERVISED_JOB_STATE=""
    SUPERVISED_JOB_STATUS=""
    case "$expected_pid" in ''|*[!0-9]*) return 1 ;; esac
    _supervised_child_capture_records || return 1

    while IFS= read -r line; do
        IFS=$' \t' read -r token listed_pid state _ <<< "$line"
        if [ "$listed_pid" = "$expected_pid" ]; then
            candidate="$line"
            matches=$((matches + 1))
        fi
        # jobs -l can prune ANY reported terminal record. Retain/reap every
        # already-captured essential's exact terminal pair while it is visible.
        # Unrelated generations are neither waited nor given signal authority.
        for ((index = 0; index < SUPERVISED_CAPTURED_COUNT; index++)); do
            if [ "$listed_pid" = "${SUPERVISED_CAPTURED_PIDS[index]}" ]; then
                known_status=0
                _supervised_child_record "${SUPERVISED_CAPTURED_SPECS[index]}" \
                    "${SUPERVISED_CAPTURED_PIDS[index]}" "$line" || known_status=$?
                # Unknown records remain a refusal on the next fresh observation.
                [ "$known_status" -ne 1 ] || continue
            fi
        done
    done <<< "$SUPERVISED_JOB_RECORDS"
    SUPERVISED_JOB_STATE=""
    SUPERVISED_JOB_STATUS=""
    [ "$matches" -eq 1 ] || return 1
    IFS=$' \t' read -r token listed_pid state _ <<< "$candidate"
    case "$token" in
        \[*\]+|\[*\]-|\[*\]) number="${token#\[}"; number="${number%%\]*}" ;;
        *) return 1 ;;
    esac
    case "$number" in ''|*[!0-9]*) return 1 ;; esac
    _supervised_child_record "%$number" "$expected_pid" "$candidate" || record_status=$?
    [ "$record_status" -ne 1 ] || return 1
    SUPERVISED_JOB_SPEC="%$number"
    for ((index = 0; index < SUPERVISED_CAPTURED_COUNT; index++)); do
        if [ "${SUPERVISED_CAPTURED_SPECS[index]}" = "%$number" ] &&
            [ "${SUPERVISED_CAPTURED_PIDS[index]}" = "$expected_pid" ]; then
            return "$record_status"
        fi
    done
    SUPERVISED_CAPTURED_SPECS[SUPERVISED_CAPTURED_COUNT]="%$number"
    SUPERVISED_CAPTURED_PIDS[SUPERVISED_CAPTURED_COUNT]="$expected_pid"
    SUPERVISED_CAPTURED_COUNT=$((SUPERVISED_CAPTURED_COUNT + 1))
    return "$record_status"
}

supervised_child_observe() {
    local job_spec="$1" expected_pid="$2" number
    SUPERVISED_JOB_STATE=""
    SUPERVISED_JOB_STATUS=""
    case "$job_spec" in %[0-9]*) number="${job_spec#%}" ;; *) return 1 ;; esac
    case "$number" in ''|*[!0-9]*) return 1 ;; esac
    case "$expected_pid" in ''|*[!0-9]*) return 1 ;; esac

    if _supervised_child_cached "$job_spec" "$expected_pid"; then
        return 2
    fi
    _child_job_long_record "$job_spec" || return 1
    _supervised_child_record "$job_spec" "$expected_pid" "$CHILD_JOB_OUTPUT"
}

relay_release_root() {
    printf '%s\n' "${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay}"
}


tailscale_cli_registration_status() {
    local env_file="$1"
    local binary="${2:-}"
    local cli_binary
    local state_root
    local coordination_root
    local development_root
    local development_root_args=()
    local scope
    local installation_id
    local https_port
    local backend_port
    local state_home="${XDG_STATE_HOME:-$HOME/.local/state}"

    if [ -z "$binary" ]; then
        binary="$(relay_binary)" || return 1
    fi
    cli_binary="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_BIN)"
    state_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_STATE_ROOT)"
    coordination_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_COORDINATION_ROOT)"
    development_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT)"
    scope="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_SCOPE)"
    installation_id="$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)"
    https_port="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_HTTPS_PORT)"
    backend_port="$(env_file_value "$env_file" HERDR_RELAY_PORT)"
    state_root="${state_root:-$state_home/herdr-mobile-relay/tailscale-cli-registration}"
    coordination_root="${coordination_root:-$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination}"
    case "$https_port" in ''|*[!0-9]*) echo "✗ HERDR_TAILSCALE_CLI_HTTPS_PORT must be an explicit port." >&2; return 2 ;; esac
    case "$backend_port" in ''|*[!0-9]*) echo "✗ HERDR_RELAY_PORT must be an explicit backend port." >&2; return 2 ;; esac
    if [ "$scope" = development ]; then
        case "$development_root" in
            /*) development_root_args=(--development-root "$development_root") ;;
            *) echo "✗ Development-scope CLI status requires its launcher-bound private root." >&2; return 2 ;;
        esac
    fi
    "$binary" tailscale-cli assert-ready "${development_root_args[@]}" \
        --binary "$cli_binary" --state-root "$state_root" --coordination-root "$coordination_root" \
        --scope "$scope" --installation-id "$installation_id" \
        --https-port "$https_port" --backend-port "$backend_port"
}

relay_binary() {
    local binary
    local common_dir
    local packaged_binary

    common_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    packaged_binary="$(dirname "$common_dir")/herdr-mobile-relay"
    if [ -f "$(dirname "$common_dir")/release-manifest.json" ] &&
       [ -x "$packaged_binary" ]; then
        binary="$packaged_binary"
    else
        binary="${HERDR_RELAY_BIN:-$(relay_release_root)/current/herdr-mobile-relay}"
    fi
    if [ ! -x "$binary" ]; then
        echo "✗ Verified relay release is unavailable: $binary" >&2
        echo "  Reinstall the exact plugin version; production launchers do not build or fall back." >&2
        return 1
    fi
    printf '%s\n' "$binary"
}

# The plugin is installed from a git clone, so its verified release belongs to
# the repository that checkout points at: a fork or a private canary installs
# its own bundle instead of this project's. Anything that is not a plain GitHub
# owner/repo fails, leaving the installer's compiled default in place.
release_repository() {
    local checkout="$1"
    local url
    local owner_repo

    command -v git >/dev/null 2>&1 || return 1
    url="$(git -C "$checkout" remote get-url origin 2>/dev/null)" || return 1
    case "$url" in
        *github.com[:/]*) owner_repo="${url##*github.com}" ;;
        *) return 1 ;;
    esac
    owner_repo="${owner_repo#[:/]}"
    owner_repo="${owner_repo%.git}"
    case "$owner_repo" in
        */*/* | /* | */ | *[!A-Za-z0-9._/-]*) return 1 ;;
        */*) printf '%s\n' "$owner_repo" ;;
        *) return 1 ;;
    esac
}

# Resolves a key a person named at a prompt. Keys live in ~/.ssh, so a bare
# name means one of those, not a file in whatever directory the setup menu was
# started from; an explicit path, absolute or relative, is taken as typed.
ssh_key_path() {
    local entered="$1"
    local candidate

    [ -n "$entered" ] || return 1
    candidate="${entered/#\~/$HOME}"
    if [ -r "$candidate" ] && [ ! -d "$candidate" ]; then
        printf '%s\n' "$candidate"
        return 0
    fi
    case "$candidate" in
        */*) return 1 ;;
    esac
    candidate="$HOME/.ssh/$entered"
    [ -r "$candidate" ] && [ ! -d "$candidate" ] || return 1
    printf '%s\n' "$candidate"
}

# Resolve the intended path without creating, chmodding or migrating anything.
# Pre-consent readers must not use the mutating setup resolver below.
relay_env_file_read_only() {
    local script_dir="$1"

    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        printf '%s\n' "$HERDR_RELAY_ENV"
    elif [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        printf '%s/relay.env\n' "$HERDR_PLUGIN_CONFIG_DIR"
    else
        printf '%s/.env\n' "$script_dir"
    fi
}

# Removal selection considers BOTH affected definitions. A legacy survivor or
# a failed decoder is not absence. Distinct selected files require recovery.
service_environment_file() {
    local script_dir="$1" manager label definition configured selected="" line trimmed directive section count environment current_encoding legacy_encoding
    case "$(uname -s)" in Linux) manager=systemd ;; Darwin) manager=launchd ;; *) return 1 ;; esac
    for label in $(service_removal_labels "$manager"); do
        definition="$(service_removal_path "$manager" "$label")" || return 1
        service_removal_resource "$definition" || return 1
        if [ "$manager" = systemd ]; then
            # BOTH labels, including missing primary files, need effective
            # configuration agreement before any route/native authority.
            service_removal_systemd_configuration "$label" || return 1
        fi
        [ "$SERVICE_REMOVAL_RESOURCE_ID" != absent ] || continue
        literal_env_read "$definition" || return 1
        if [ "$manager" = systemd ]; then
            count=0; environment=""; section=""
            while IFS= read -r line; do
                line="${line#"${line%%[![:space:]]*}"}"
                case "$line" in ''|\#*|\;*) continue ;; esac
                trimmed="${line%"${line##*[![:space:]]}"}"
                # No logical-line continuation is supported: it could hide
                # an environment directive split across physical lines.
                case "$trimmed" in *\\) return 1 ;; '['*) section="$trimmed"; continue ;; esac
                case "$line" in *=*) ;; *) continue ;; esac
                directive="${line%%=*}"
                directive="${directive%"${directive##*[![:space:]]}"}"
                # systemd accepts whitespace around '='. Recognize the key
                # before admitting ONLY the producer's canonical spelling;
                # a later spaced override/reset must never be ignored.
                case "$directive" in
                    Environment)
                        [ "$section" = '[Service]' ] && [ "${line%%=*}" = Environment ] || return 1
                        count=$((count + 1)); environment="$line" ;;
                    EnvironmentFile|PassEnvironment|UnsetEnvironment) return 1 ;;
                esac
            done <<< "$LITERAL_TEXT"
            [ "$count" -eq 1 ] || return 1
            # Use the existing ONCE-only backslash/percent decoder, never
            # feed its decoded output through another decoder.
            configured="$(systemd_field "$definition" Environment && printf '.')" || return 1
        else
            service_removal_plist_label "$definition" "$label" || return 1
            configured="$(plutil -extract EnvironmentVariables.HERDR_RELAY_ENV raw -o - "$definition" 2>/dev/null && printf '.')" || return 1
        fi
        configured="${configured%.}"
        configured="${configured%$'\n'}"
        if [ "$manager" = systemd ]; then
            # Admit ONLY an exact current/legacy producer round trip. An
            # unescaped %u (or unsupported escape/quoting) is not a literal
            # path: systemd would interpret it differently. Decode once.
            current_encoding="$(systemd_quoted "HERDR_RELAY_ENV=$configured")" || return 1
            legacy_encoding="$(systemd_quote_value "$configured")" || return 1
            if [ "$environment" != "Environment=$current_encoding" ] &&
                [ "$environment" != "Environment=HERDR_RELAY_ENV=$legacy_encoding" ]; then
                # Whole-assignment quotes around an otherwise unquoted
                # current emission are equivalent, not a new decoder.
                case "$current_encoding" in \"*) return 1 ;; esac
                [ "$environment" = "Environment=\"$current_encoding\"" ] || return 1
            fi
        fi
        service_removal_env_safe "$configured" || return 1
        # An installed definition pointing at missing config is ambiguous,
        # unlike no definition and a genuinely absent fallback config.
        [ -f "$configured" ] && [ -r "$configured" ] || return 1
        [ -z "$selected" ] || [ "$selected" = "$configured" ] || return 1
        selected="$configured"
    done
    if [ -z "$selected" ]; then
        selected="$(relay_env_file_read_only "$script_dir")" || return 1
        service_removal_env_safe "$selected" || return 1
    fi
    printf '%s\n' "$selected"
}

relay_env_file() {
    local script_dir="$1"
    local config_dir
    local plugin_env

    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        printf '%s\n' "$HERDR_RELAY_ENV"
        return
    fi
    if [ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        printf '%s/.env\n' "$script_dir"
        return
    fi

    config_dir="$HERDR_PLUGIN_CONFIG_DIR"
    plugin_env="$config_dir/relay.env"
    mkdir -p "$config_dir"
    chmod 700 "$config_dir"
    if [ ! -f "$plugin_env" ] && [ -f "$script_dir/.env" ]; then
        umask 077
        cp "$script_dir/.env" "$plugin_env"
        chmod 600 "$plugin_env"
    fi
    if [ ! -d "$config_dir/push" ] && [ -d "$script_dir/push" ]; then
        umask 077
        cp -R "$script_dir/push" "$config_dir/push"
        chmod -R go-rwx "$config_dir/push"
    fi
    printf '%s\n' "$plugin_env"
}

canonical_file_path() {
    local path="$1"
    local directory
    local filename

    directory="$(dirname "$path")"
    filename="$(basename "$path")"
    if [ -d "$directory" ]; then
        directory="$(cd "$directory" && pwd -P)"
    fi
    printf '%s/%s\n' "${directory%/}" "$filename"
}
yaml_scalar() {
    local key="$1"
    local config="$2"
    local value

    value="$(sed -nE "s/^[[:space:]]*-?[[:space:]]*${key}:[[:space:]]*([^#]+).*/\\1/p" "$config" | head -1)"
    value="$(printf '%s' "$value" | sed 's/[[:space:]]*$//')"
    value="${value#\"}"
    value="${value%\"}"
    value="${value#\'}"
    value="${value%\'}"
    printf '%s\n' "$value"
}

expand_config_path() {
    local path="$1"
    local config="$2"

    case "$path" in
        \~/*) path="$HOME/${path#\~/}" ;;
        \$HOME/*) path="$HOME/${path#\$HOME/}" ;;
        /*) ;;
        *) path="$(dirname "$config")/$path" ;;
    esac
    canonical_file_path "$path"
}
valid_hostname() {
    local hostname="$1"
    local old_ifs
    local label

    [ "${#hostname}" -le 253 ] || return 1
    case "$hostname" in
        ""|.*|*.|*..*|*[!A-Za-z0-9.-]*) return 1 ;;
    esac
    old_ifs="$IFS"
    IFS=.
    # shellcheck disable=SC2086
    set -- $hostname
    IFS="$old_ifs"
    for label in "$@"; do
        [ "${#label}" -le 63 ] || return 1
        case "$label" in
            ""|-*|*-) return 1 ;;
        esac
    done
}

read_cloudflared_relay_config() {
    local config="$1"
    local expected_port="$2"
    local configured_tunnel
    local credentials_value
    local service_url
    local origin

    if [ ! -r "$config" ]; then
        echo "✗ Cloudflare tunnel config is not readable: $config" >&2
        return 1
    fi
    if ! cloudflared tunnel --config "$config" ingress validate; then
        echo "✗ cloudflared rejected the ingress syntax in $config." >&2
        return 1
    fi

    configured_tunnel="$(yaml_scalar tunnel "$config")"
    credentials_value="$(yaml_scalar credentials-file "$config")"
    CONFIG_HOST="$(yaml_scalar hostname "$config")"
    service_url="$(yaml_scalar service "$config")"
    if [ -z "$configured_tunnel" ]; then
        echo "✗ No tunnel identifier found in $config." >&2
        return 1
    fi
    if [ -z "$credentials_value" ]; then
        echo "✗ No credentials-file found in $config." >&2
        return 1
    fi
    if [ -z "$CONFIG_HOST" ] || ! valid_hostname "$CONFIG_HOST"; then
        echo "✗ No valid ingress hostname found in $config." >&2
        return 1
    fi
    if [[ "$service_url" != http://* ]]; then
        echo "✗ The first ingress origin in $config is not an HTTP loopback service." >&2
        return 1
    fi
    origin="${service_url#http://}"
    origin="${origin%%/*}"
    case "$origin" in
        "127.0.0.1:$expected_port"|"localhost:$expected_port") ;;
        *)
            echo "✗ Ingress origin $service_url does not match HERDR_RELAY_PORT=$expected_port." >&2
            return 1
            ;;
    esac

    CREDENTIALS_PATH="$(expand_config_path "$credentials_value" "$config")"
    if [ ! -r "$CREDENTIALS_PATH" ]; then
        echo "✗ Tunnel credentials are not readable: $CREDENTIALS_PATH" >&2
        return 1
    fi
    TUNNEL_UUID="$("$(relay_binary)" stable-state credential-id "$CREDENTIALS_PATH")"
    if [[ "$configured_tunnel" =~ ^[0-9a-fA-F-]{36}$ ]] &&
       [ "$(printf '%s' "$configured_tunnel" | tr '[:upper:]' '[:lower:]')" != "$TUNNEL_UUID" ]; then
        echo "✗ Config tunnel $configured_tunnel does not match credentials for $TUNNEL_UUID." >&2
        return 1
    fi
    # This value is the documented output of this sourced helper for callers.
    # shellcheck disable=SC2034
    TUNNEL_NAME="$configured_tunnel"
}

# The origin certificate cloudflared should manage tunnels with: an explicit
# TUNNEL_ORIGIN_CERT wins, then the config's own origincert, then cloudflared's
# default location. A path is always printed even when nothing is there to read,
# because the callers differ on what an unreadable certificate means — setup
# skips its existence check, teardown refuses.
cloudflared_origin_cert() {
    local config="$1"
    local certificate_value

    if [ -n "${TUNNEL_ORIGIN_CERT:-}" ]; then
        printf '%s\n' "$TUNNEL_ORIGIN_CERT"
        return
    fi
    certificate_value="$(yaml_scalar origincert "$config")"
    if [ -n "$certificate_value" ]; then
        expand_config_path "$certificate_value" "$config"
        return
    fi
    printf '%s\n' "$HOME/.cloudflared/cert.pem"
}

# The Cloudflare name of a tunnel UUID. A generated config records the UUID as
# its `tunnel:` scalar, so anything that reasons about the tunnel *name* — the
# Herdr namespace guard above all — has to ask Cloudflare which name the id was
# created with. Fails when the certificate cannot authorize the lookup, when
# Cloudflare refuses it, or when the id names no live tunnel.
cloudflared_tunnel_name_by_id() {
    local uuid="$1"
    local origin_cert="$2"
    local list_output
    local name

    [ -r "$origin_cert" ] || return 1
    list_output="$(mktemp "${TMPDIR:-/tmp}/herdr-tunnel-list.XXXXXX")" || return 1
    if ! cloudflared tunnel --origincert "$origin_cert" list --id "$uuid" --output json > "$list_output"; then
        rm -f "$list_output"
        return 1
    fi
    name="$("$(relay_binary)" stable-state tunnel-name-by-id "$list_output" "$uuid")" || name=""
    rm -f "$list_output"
    [ -n "$name" ] || return 1
    printf '%s\n' "$name"
}

cloudflare_cert_zone_name() {
    local origin_cert="$1"
    local payload
    local zone_id
    local api_token

    [ -r "$origin_cert" ] || return 1
    payload="$(
        sed -n '/BEGIN ARGO TUNNEL TOKEN/,/END ARGO TUNNEL TOKEN/p' "$origin_cert" |
            sed '1d;$d' | tr -d '\n' | base64 -d 2>/dev/null
    )" || return 1
    zone_id="$(printf '%s' "$payload" | sed -n 's/.*"zoneID":"\([^"]*\)".*/\1/p')"
    api_token="$(printf '%s' "$payload" | sed -n 's/.*"apiToken":"\([^"]*\)".*/\1/p')"
    [ -n "$zone_id" ] && [ -n "$api_token" ] || return 1
    curl --fail --silent --show-error --max-time 10 \
        -H "Authorization: Bearer $api_token" \
        "https://api.cloudflare.com/client/v4/zones/$zone_id" 2>/dev/null |
        sed -n 's/.*"result":{"id":"[^"]*","name":"\([^"]*\)".*/\1/p' | head -1
}

relogin_for_cloudflare_zone() {
    local origin_cert="$1"
    local zone="$2"
    local backup
    backup="$origin_cert.$(date +%Y%m%d%H%M%S)"

    echo "▸ Signing in to Cloudflare for $zone."
    echo "  The current certificate is kept as $backup."
    mv "$origin_cert" "$backup" || return 1
    if ! cloudflared tunnel login; then
        mv -f "$backup" "$origin_cert"
        echo "✗ Sign-in did not finish; the previous certificate is back." >&2
        return 1
    fi
    if [ ! -r "$origin_cert" ]; then
        mv -f "$backup" "$origin_cert"
        echo "✗ Sign-in produced no certificate; the previous one is back." >&2
        return 1
    fi
}

hostname_in_zone() {
    case "$1" in
        "$2" | *".$2") return 0 ;;
    esac
    return 1
}

cloudflare_routed_hostname() {
    printf '%s\n' "$1" |
        sed -n 's/.*Added CNAME \([^ ]*\) which will route.*/\1/p;s/.*INF \([^ ]*\) is already configured.*/\1/p' |
        head -1
}

installed_service_env_file() {
    local service_file

    case "$(uname -s)" in
        Linux)
            service_file="$HOME/.config/systemd/user/herdr-mobile-relay.service"
            if [ -r "$service_file" ]; then
                systemd_field "$service_file" Environment
            fi
            ;;
        Darwin)
            service_file="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
            if [ -r "$service_file" ]; then
                local plist_tool="${HERDR_PLUTIL:-/usr/bin/plutil}"
                [ -x "$plist_tool" ] || return 1
                "$plist_tool" -extract EnvironmentVariables.HERDR_RELAY_ENV raw -o - "$service_file"
            fi
            ;;
    esac
}

update_launchd_release_paths() {
    local plist="$1"
    local service_wrapper="$2"
    local work_dir="$3"
    local env_file="${4:-}"
    local plist_tool="${HERDR_PLUTIL:-/usr/bin/plutil}"
    local temp

    [ -x "$plist_tool" ] || {
        echo "plutil is unavailable: $plist_tool" >&2
        return 1
    }
    [ -f "$plist" ] && [ ! -L "$plist" ] || return 1
    temp="$(mktemp "${plist}.XXXXXX")" || return 1
    cp -p "$plist" "$temp" || { rm -f "$temp"; return 1; }
    if ! "$plist_tool" -replace ProgramArguments.0 -string "$service_wrapper" "$temp" ||
        ! "$plist_tool" -replace WorkingDirectory -string "$work_dir" "$temp"; then
        rm -f "$temp"
        return 1
    fi
    if [ -n "$env_file" ] && ! "$plist_tool" -replace EnvironmentVariables.HERDR_RELAY_ENV -string "$env_file" "$temp"; then
        rm -f "$temp"
        return 1
    fi
    if ! "$plist_tool" -lint "$temp"; then
        rm -f "$temp"
        return 1
    fi
    mv -f "$temp" "$plist"
}

require_user_service_context() {
    if [ "$(id -u)" -ne 0 ]; then
        return 0
    fi

    echo "Refusing to manage the Herdr Mobile Relay user service as root." >&2
    echo "Run the command again as the signed-in macOS or Linux user, without sudo." >&2
    return 1
}

launchd_service_loaded() {
    local service_target="$1"
    launchctl print "$service_target" >/dev/null 2>&1
}

reload_launchd_service_definition() {
    local plist="$1"
    local label="$2"
    local domain
    domain="gui/$(id -u)"
    local service_target="$domain/$label"
    local attempt
    local unloaded=false
    local bootstrapped=false

    require_user_service_context || return 1
    [ -f "$plist" ] && [ ! -L "$plist" ] || {
        echo "Cannot reload launchd service: plist is not a regular file: $plist" >&2
        return 1
    }
    if command -v plutil >/dev/null 2>&1; then
        plutil -lint "$plist" >/dev/null || {
            echo "Cannot reload launchd service: plist validation failed: $plist" >&2
            return 1
        }
    fi

    # A migration changes ProgramArguments, WorkingDirectory, and the relay
    # environment. Unload the plist using the form used by the legacy service
    # installer, then wait until launchd has actually removed its cached job.
    if launchd_service_loaded "$service_target"; then
        if ! launchctl bootout "$domain" "$plist"; then
            launchctl bootout "$service_target" || {
                echo "Could not unload launchd service $service_target" >&2
                return 1
            }
        fi
        for attempt in 1 2 3 4 5 6 7 8 9 10; do
            if ! launchd_service_loaded "$service_target"; then
                unloaded=true
                break
            fi
            sleep 1
        done
        if [ "$unloaded" != true ]; then
            echo "Timed out waiting for launchd to unload $service_target" >&2
            return 1
        fi
    fi

    # launchd can briefly reject bootstrap while completing a bootout. Retry
    # the registration, accepting success only when the exact job is loaded.
    for attempt in 1 2 3 4 5; do
        if [ "$attempt" -eq 5 ]; then
            if launchctl bootstrap "$domain" "$plist"; then
                bootstrapped=true
            fi
        elif launchctl bootstrap "$domain" "$plist" >/dev/null 2>&1; then
            bootstrapped=true
        fi
        if [ "$bootstrapped" = true ] ||
           launchd_service_loaded "$service_target"; then
            bootstrapped=true
            break
        fi
        sleep 1
    done
    if [ "$bootstrapped" != true ]; then
        echo "Could not bootstrap launchd service $service_target" >&2
        return 1
    fi

    launchctl enable "$service_target"
    launchctl kickstart -k "$service_target"
}

installed_relay_service_active() {
    case "$(uname -s)" in
        Darwin)
            launchd_service_loaded "gui/$(id -u)/com.herdr-mobile-relay.service"
            ;;
        Linux)
            command -v systemctl >/dev/null 2>&1 &&
                systemctl --user is-active --quiet herdr-mobile-relay.service
            ;;
        *)
            return 1
            ;;
    esac
}

restart_installed_relay_service() {
    case "$(uname -s)" in
        Darwin)
            launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
            ;;
        Linux)
            systemctl --user restart herdr-mobile-relay.service
            ;;
        *)
            return 1
            ;;
    esac
}

assert_service_env_matches() {
    local resolved_env
    local service_env

    resolved_env="$(canonical_file_path "$1")"
    service_env="$(installed_service_env_file)"
    if [ -z "$service_env" ]; then
        return
    fi
    service_env="$(canonical_file_path "$service_env")"
    if [ "$resolved_env" = "$service_env" ]; then
        return
    fi

    echo "✗ Refusing to use a different relay configuration than the installed service." >&2
    echo "  This command resolved: $resolved_env" >&2
    echo "  Installed service uses: $service_env" >&2
    echo "  Run the matching Herdr plugin action, or explicitly set:" >&2
    echo "  HERDR_RELAY_ENV=$service_env" >&2
    return 1
}

# A pane invoked straight from herdr has to hold the terminal open long enough
# to be read. Under the setup menu it must not: the menu pauses once on the way
# back, so a second prompt here would cost two keystrokes to return.
pause_before_close() {
    [ "${HERDR_SETUP_MENU:-}" != 1 ] || return 0
    if [ -t 0 ]; then
        echo ""
        read -r -p "Press Enter to close this pane." _answer
    fi
}

# Wrangler refuses to start on an older Node, and npx only says so after a long
# download, so every candidate below is version-checked before it is offered.
NODE_MIN_MAJOR=22

node_major_version() {
    local reported
    reported="$("$1" --version 2>/dev/null)" || return 1
    reported="${reported#v}"
    reported="${reported%%.*}"
    case "$reported" in
        '' | *[!0-9]*) return 1 ;;
    esac
    printf '%s\n' "$reported"
}

# Plugin panes inherit herdr's environment, not an interactive shell's, so a
# node installed by nvm, fnm, volta, or asdf is invisible here even though the
# person's own terminal finds it: the manager's PATH entry comes from a shell rc
# this process never sources. Look where those managers actually put it, prefer
# a "current" link over a pinned version so a node upgrade does not strand the
# recording, and honour what a previous run already recorded.
node_bin_dir() {
    local env_file="${1:-}"
    local candidate
    local major
    local recorded=""
    local on_path=""
    local node_version_bin

    if [ -n "$env_file" ] && [ -f "$env_file" ]; then
        recorded="$(env_file_value "$env_file" HERDR_APP_DEPLOY_NODE_DIR)"
    fi
    node_version_bin="$(find "${NVM_DIR:-$HOME/.nvm}/versions/node" -mindepth 2 -maxdepth 2 \
        -type d -name bin -print 2>/dev/null | sort -V | tail -1)"
    if on_path="$(command -v node 2>/dev/null)"; then
        on_path="$(dirname "$on_path")"
    fi
    for candidate in \
        "$recorded" \
        "$on_path" \
        "${NVM_DIR:-$HOME/.nvm}/current/bin" \
        "${FNM_DIR:-$HOME/.local/share/fnm}/aliases/default/bin" \
        "$HOME/.volta/bin" \
        "$HOME/.asdf/shims" \
        "$node_version_bin" \
        /opt/homebrew/bin \
        /usr/local/bin \
        "$HOME/.local/bin" \
        /usr/bin; do
        case "$candidate" in
            /*) ;;
            *) continue ;;
        esac
        [ -x "$candidate/node" ] && [ -x "$candidate/npx" ] || continue
        major="$(node_major_version "$candidate/node")" || continue
        [ "$major" -ge "$NODE_MIN_MAJOR" ] || continue
        printf '%s\n' "$candidate"
        return 0
    done
    return 1
}

# A menu reads as a wall of text when the choice and its explanation carry the
# same weight. Bold the choice, but only when a terminal will render it: piped
# output stays plain for logs and tests, and NO_COLOR is honoured.
menu_item() {
    local key="$1"
    local title="$2"

    if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
        printf '  \033[1m%s. %s\033[0m\n' "$key" "$title"
        return 0
    fi
    printf '  %s. %s\n' "$key" "$title"
}

generate_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 16
        return
    fi
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen | tr '[:upper:]' '[:lower:]' | tr -d '-'
        return
    fi
    echo "Cannot generate a relay token: install openssl or uuidgen." >&2
    return 1
}

generate_instance_id() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 16
        return
    fi
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen | tr '[:upper:]' '[:lower:]'
        return
    fi
    echo "Cannot generate a relay instance ID: install openssl or uuidgen." >&2
    return 1
}

env_file_value() {
    local env_file="$1"
    local key="$2"

    if [ ! -f "$env_file" ]; then
        return
    fi
    (
        unset "$key"
        set -a
        # shellcheck source=/dev/null
        . "$env_file"
        set +a
        printf '%s\n' "${!key-}"
    )
}

# Service-definition removal only. No transaction, process discovery, route,
# reservation or credential cleanup. Finite polls are NOT a hard OS deadline.
service_removal_labels() {
    case "$1" in
        systemd) printf '%s\n' herdr-mobile-relay.service herdr-remote.service ;;
        launchd) printf '%s\n' com.herdr-mobile-relay.service com.herdr-remote.service ;;
        *) return 1 ;;
    esac
}

service_removal_path() {
    case "$1:$2" in
        systemd:herdr-mobile-relay.service|systemd:herdr-remote.service) printf '%s/.config/systemd/user/%s\n' "$HOME" "$2" ;;
        launchd:com.herdr-mobile-relay.service|launchd:com.herdr-remote.service) printf '%s/Library/LaunchAgents/%s.plist\n' "$HOME" "$2" ;;
        *) return 1 ;;
    esac
}

service_removal_env_safe() {
    local file="$1" parent identity
    case "$file" in ''|*[$'\001'-$'\037'$'\177']*) return 1 ;; esac
    native_resource_parents_safe "$file" || return 1
    # Negative -e/-L below an unsearchable or non-directory ancestor do
    # NOT establish absence. Walk all ancestors, even after a negative
    # predicate, until each existing prefix (including /) is checked.
    parent="$(dirname "$file")" || return 1
    while :; do
        if [ "$parent" = / ] || [ -e "$parent" ] || [ -L "$parent" ]; then
            [ -d "$parent" ] && [ ! -L "$parent" ] && [ -x "$parent" ] || return 1
            identity="$(stat -c '%d:%i' "$parent" 2>/dev/null)" ||
                identity="$(stat -f '%d:%i' "$parent" 2>/dev/null)" || return 1
            [[ "$identity" =~ ^[0-9]+:[0-9]+$ ]] || return 1
        fi
        [ "$parent" != / ] || break
        parent="$(dirname "$parent")" || return 1
    done
    if [ -e "$file" ] || [ -L "$file" ]; then
        native_mutable_file_safe "$file" && [ -r "$file" ] || return 1
    fi
    return 0
}

service_removal_selected_disjoint() {
    local selected="$1" destination="$2" identity other
    [ -n "$selected" ] || return 0
    # Static alternate spellings can name the same single-link inode on
    # case-insensitive filesystems. Metadata is NOT authentication/a lock.
    service_removal_env_safe "$selected" && service_removal_env_safe "$destination" || return 1
    [ "$selected" != "$destination" ] || return 1
    [ -e "$selected" ] || return 0
    identity="$(stat -c '%d:%i' "$selected" 2>/dev/null)" ||
        identity="$(stat -f '%d:%i' "$selected" 2>/dev/null)" || return 1
    [[ "$identity" =~ ^[0-9]+:[0-9]+$ ]] || return 1
    if [ -e "$destination" ]; then
        other="$(stat -c '%d:%i' "$destination" 2>/dev/null)" ||
            other="$(stat -f '%d:%i' "$destination" 2>/dev/null)" || return 1
        [[ "$other" =~ ^[0-9]+:[0-9]+$ ]] || return 1
        [ "$identity" != "$other" ] || return 1
    fi
    return 0
}

service_removal_plist_label() {
    local file="$1" expected="$2" actual
    case "$expected" in com.herdr-mobile-relay.service|com.herdr-remote.service) ;; *) return 1 ;; esac
    literal_env_read "$file" || return 1
    actual="$(plutil -extract Label raw -o - "$file" 2>/dev/null && printf '.')" || return 1
    actual="${actual%.}"; actual="${actual%$'\n'}"
    # Do not follow a definition's arbitrary label into manager authority.
    [ "$actual" = "$expected" ]
}

service_removal_resource() {
    local file="$1" identity
    SERVICE_REMOVAL_RESOURCE_ID=unknown
    export -n SERVICE_REMOVAL_RESOURCE_ID
    service_removal_env_safe "$file" || return 1
    if [ ! -e "$file" ] && [ ! -L "$file" ]; then
        SERVICE_REMOVAL_RESOURCE_ID=absent; return 0
    fi
    identity="$(stat -c '%d:%i:%s:%Y' "$file" 2>/dev/null)" ||
        identity="$(stat -f '%d:%i:%z:%m' "$file" 2>/dev/null)" || return 1
    [[ "$identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]] || return 1
    SERVICE_REMOVAL_RESOURCE_ID="$identity"
    return 0
}

# Removal-specific effective systemd configuration admission, NOT quiescence.
# Only the established fragment with NO effective overlays or pending reload
# is supported. Never read a manager-reported arbitrary path or merge drop-ins.
service_removal_systemd_configuration() {
    local label="$1" file original output line count=0 code=0
    local id="" load="" active="" enabled="" fragment="" overlays="" reload=""
    local i=0 l=0 a=0 e=0 f=0 d=0 n=0 probe_status
    local LC_ALL=C
    file="$(service_removal_path systemd "$label")" || return 1
    service_removal_resource "$file" || return 1
    original="$SERVICE_REMOVAL_RESOURCE_ID"
    # Bash 3.2 read bounds acquisition to 4097 bytes and detects NUL. Only
    # EOF with <=4096 bytes is accepted. Both pipeline statuses are checked;
    # partial output/query failure cannot turn into empty metadata/absence.
    # These bounds do NOT impose a hard deadline on a nonresponsive tool.
    output="$(
        LC_ALL=C systemctl --user show "$label" --all --property=Id --property=LoadState --property=ActiveState --property=UnitFileState --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --no-pager 2>/dev/null | (
            code=0
            IFS= read -r -d '' -n 4097 output || code=$?
            [ "$code" -eq 1 ] && [ "${#output}" -le 4096 ] || exit 1
            printf '%s' "$output" || exit 1
        )
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
        printf '.'
    )" || return 1
    output="${output%.}"; output="${output%$'\n'}"
    while IFS= read -r line; do
        count=$((count + 1))
        case "$line" in
            Id=*) i=$((i + 1)); id="${line#*=}" ;;
            LoadState=*) l=$((l + 1)); load="${line#*=}" ;;
            ActiveState=*) a=$((a + 1)); active="${line#*=}" ;;
            UnitFileState=*) e=$((e + 1)); enabled="${line#*=}" ;;
            FragmentPath=*) f=$((f + 1)); fragment="${line#*=}" ;;
            DropInPaths=*) d=$((d + 1)); overlays="${line#*=}" ;;
            NeedDaemonReload=*) n=$((n + 1)); reload="${line#*=}" ;;
            *) return 1 ;;
        esac
    done <<< "$output"
    [ "$count:$i:$l:$a:$e:$f:$d:$n" = 7:1:1:1:1:1:1:1 ] || return 1
    [ "$id" = "$label" ] && [ -z "$overlays" ] && [ "$reload" = no ] || return 1
    if [ "$original" = absent ]; then
        [ "$load:$active" = not-found:inactive ] && [ -z "$fragment" ] || return 1
        case "$enabled" in ''|not-found) ;; *) return 1 ;; esac
    else
        [ "$load" = loaded ] && [ "$fragment" = "$file" ] || return 1
        case "$active" in active|inactive|failed|activating|deactivating) ;; *) return 1 ;; esac
        case "$enabled" in enabled|disabled) ;; *) return 1 ;; esac
    fi
    service_removal_resource "$file" || return 1
    [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ]
}

# Lifecycle acquisition must reject binary/over-limit input BEFORE Bash can
# discard NUL in command substitution. EOF alone (status 1) with bounded text
# is admitted; a NUL delimiter or limit+1 bytes makes read return 0 and refuses.
# This reader bounds bytes, not the completion time of the producer/filesystem.
service_removal_read_bounded() {
    local limit="$1" output="" code=0
    local LC_ALL=C
    case "$limit" in 4096|65536) ;; *) return 1 ;; esac
    IFS= read -r -d '' -n "$((limit + 1))" output || code=$?
    [ "$code" -eq 1 ] && [ "${#output}" -le "$limit" ] || return 1
    printf '%s' "$output"
}

# Supported systemd vocabulary is deliberately narrow: loaded with a known
# active state and enabled/disabled, or coherent not-found/inactive/no unit.
# Launchd requires exact domain/label headers and the documented not-found
# diagnostic. Loaded/waiting is never quiet. Missing disabled entry is enabled.
service_removal_observe() {
    local manager="$1" label="$2" output line code=0 domain uid count=0 load="" active="" enabled="" l=0 a=0 e=0 probe_status
    local LC_ALL=C
    SERVICE_REMOVAL_STATE=unknown SERVICE_REMOVAL_ENABLED=unknown
    export -n SERVICE_REMOVAL_STATE SERVICE_REMOVAL_ENABLED
    service_removal_path "$manager" "$label" >/dev/null || return 1
    if [ "$manager" = systemd ]; then
        output="$(
            LC_ALL=C systemctl --user show "$label" --property=LoadState --property=ActiveState --property=UnitFileState --no-pager 2>/dev/null | service_removal_read_bounded 4096
            probe_status=( "${PIPESTATUS[@]}" )
            [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
            printf '.'
        )" || return 1
        output="${output%.}"; output="${output%$'\n'}"
        while IFS= read -r line; do
            count=$((count + 1))
            case "$line" in
                LoadState=*) l=$((l + 1)); load="${line#*=}" ;;
                ActiveState=*) a=$((a + 1)); active="${line#*=}" ;;
                UnitFileState=*) e=$((e + 1)); enabled="${line#*=}" ;;
                *) return 1 ;;
            esac
        done <<< "$output"
        [ "$count:$l:$a:$e" = 3:1:1:1 ] || return 1
        if [ "$load" = not-found ]; then
            [ "$active" = inactive ] || return 1
            case "$enabled" in ''|not-found) ;; *) return 1 ;; esac
            SERVICE_REMOVAL_STATE=absent SERVICE_REMOVAL_ENABLED=false; return 0
        fi
        [ "$load" = loaded ] || return 1
        case "$active" in active|inactive|failed|activating|deactivating) ;; *) return 1 ;; esac
        case "$enabled" in enabled) SERVICE_REMOVAL_ENABLED=true ;; disabled) SERVICE_REMOVAL_ENABLED=false ;; *) return 1 ;; esac
        SERVICE_REMOVAL_STATE="$active"; return 0
    fi
    uid="$(id -u)" || return 1
    case "$uid" in ''|*[!0-9]*) return 1 ;; esac
    domain="gui/$uid"
    output="$(
        LC_ALL=C launchctl print "$domain" 2>/dev/null | service_removal_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
        printf '.'
    )" || return 1
    output="${output%.}"; output="${output%$'\n'}"
    case "$output" in "$domain = {"$'\n'*'}'|"$domain = {}") ;; *) return 1 ;; esac
    output="$(
        LC_ALL=C launchctl print "$domain/$label" 2>&1 | service_removal_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[1]}" -eq 0 ] || exit 1
        # Transport the genuine producer status OUTSIDE the bounded text;
        # only the existing exact not-found diagnostic admits nonzero.
        printf '\n%s.' "${probe_status[0]}"
    )" || return 1
    code="${output##*$'\n'}"; code="${code%.}"
    output="${output%$'\n'*}"; output="${output%$'\n'}"
    if [ "$code" -ne 0 ]; then
        [ "$output" = "Could not find service \"$label\" in domain for user gui: $uid" ] || return 1
        active=absent
    else
        case "$output" in "$domain/$label = {"$'\n'*'}'|"$domain/$label = {}") ;; *) return 1 ;; esac
        active=loaded
    fi
    output="$(
        LC_ALL=C launchctl print-disabled "$domain" 2>/dev/null | service_removal_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
        printf '.'
    )" || return 1
    output="${output%.}"; output="${output%$'\n'}"
    enabled=true
    count=0; l=0; a=0
    while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}"; line="${line%"${line##*[![:space:]]}"}"
        if [ "$l" -eq 0 ]; then
            [ "$line" = 'disabled services = {' ] || return 1
            l=1; continue
        fi
        [ "$a" -eq 0 ] || return 1
        case "$line" in
            '}') a=1 ;;
            "\"$label\" => true") enabled=false; count=$((count + 1)) ;;
            "\"$label\" => false") enabled=true; count=$((count + 1)) ;;
            \"*\"\ =\>\ true|\"*\"\ =\>\ false) case "$line" in *"\"$label\""*) return 1 ;; esac ;;
            *) return 1 ;;
        esac
    done <<< "$output"
    [ "$l:$a" = 1:1 ] && [ "$count" -le 1 ] || return 1
    SERVICE_REMOVAL_STATE="$active" SERVICE_REMOVAL_ENABLED="$enabled"
    return 0
}

service_removal_quiet() {
    service_removal_observe "$1" "$2" || return 1
    case "$SERVICE_REMOVAL_STATE:$SERVICE_REMOVAL_ENABLED" in absent:false|inactive:false) return 0 ;; *) return 1 ;; esac
}

service_removal_one() {
    local manager="$1" label="$2" file original attempt uid quiet=false
    file="$(service_removal_path "$manager" "$label")" || return 1
    service_removal_resource "$file" || return 1
    original="$SERVICE_REMOVAL_RESOURCE_ID"
    if [ "$manager" = launchd ] && [ "$original" != absent ]; then
        service_removal_plist_label "$file" "$label" || return 1
    elif [ "$manager" = systemd ]; then
        service_removal_systemd_configuration "$label" || return 1
        [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ] || return 1
    fi
    service_removal_observe "$manager" "$label" || return 1
    if [ "$manager" = systemd ]; then
        case "$SERVICE_REMOVAL_STATE" in
            absent|inactive) ;;
            *) systemctl --user stop "$label" >/dev/null 2>&1 || return 1 ;;
        esac
        if [ "$SERVICE_REMOVAL_ENABLED" != false ]; then
            systemctl --user disable "$label" >/dev/null 2>&1 || return 1
        fi
    else
        uid="$(id -u)" || return 1
        case "$uid" in ''|*[!0-9]*) return 1 ;; esac
        if [ "$SERVICE_REMOVAL_ENABLED" != false ]; then
            launchctl disable "gui/$uid/$label" >/dev/null 2>&1 || return 1
        fi
        if [ "$SERVICE_REMOVAL_STATE" != absent ]; then
            launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || return 1
        fi
    fi
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
        service_removal_observe "$manager" "$label" || return 1
        case "$SERVICE_REMOVAL_STATE:$SERVICE_REMOVAL_ENABLED" in absent:false|inactive:false) quiet=true; break ;; esac
        [ "$attempt" -eq 10 ] || sleep 0.1 || return 1
    done
    [ "$quiet" = true ] || return 1
    service_removal_resource "$file" || return 1
    [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ] || return 1
    if [ "$manager" = systemd ]; then
        service_removal_systemd_configuration "$label" || return 1
        [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ] || return 1
    fi
    service_removal_quiet "$manager" "$label" || return 1
    service_removal_resource "$file" || return 1
    [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ] || return 1
    if [ "$manager" = launchd ] && [ "$original" != absent ]; then
        service_removal_plist_label "$file" "$label" || return 1
    elif [ "$manager" = systemd ]; then
        service_removal_systemd_configuration "$label" || return 1
        [ "$SERVICE_REMOVAL_RESOURCE_ID" = "$original" ] || return 1
    fi
    # Last manager observation immediately precedes deletion. Cooperative
    # observations/metadata do not make pathname/controller races atomic.
    service_removal_quiet "$manager" "$label" || return 1
    if [ "$original" = absent ]; then
        printf 'Verified absent user service definition: %s.\n' "$label" || return 1
        return 0
    fi
    rm -f -- "$file" || { echo 'Service definition removal did not complete.' >&2; return 1; }
    if [ "$manager" = systemd ] && ! systemctl --user daemon-reload >/dev/null 2>&1; then
        printf 'Removed %s definition, but systemd reload failed; no restoration or activation attempted.\n' "$label" >&2
        return 1
    fi
    printf 'Removed quiescent user service definition: %s.\n' "$label"
}

service_removal_all() {
    local manager="$1" label failed=false
    case "$manager" in systemd|launchd) ;; *) return 1 ;; esac
    for label in $(service_removal_labels "$manager"); do
        if ! service_removal_one "$manager" "$label"; then
            printf 'Removal incomplete for %s; unsafe resources and existing recovery evidence were not cleared.\n' "$label" >&2
            failed=true
        fi
    done
    [ "$failed" = false ] || return 1
    echo 'Verified quiescence and service-definition removal for both Herdr labels; any explicit route disposition was separate from native definition removal.'
}

# Bounded literal admission precedes even a prior explicit route disposition.
# No selected assignment is evaluated or imported into manager/tool routing.
service_cli_route_disposition() {
    local env_file="$1" script_dir="$2" prior_disposition="${3:-}" choice mode ambient index key gateways pointer manager label destination
    service_removal_env_safe "$env_file" && literal_env_scan "$env_file" || {
        echo 'Cannot safely identify the installed relay transport; service removal refused.' >&2; return 1;
    }
    for ((index=0; index<${#LITERAL_KEYS[@]}; index++)); do
        key="${LITERAL_KEYS[index]}"
        case "$key" in
            # The existing exact-route adapter may consume these routes after
            # loading the file. Saved data cannot select its relay/helper,
            # interpreter, web root or internal service-cleanup permission.
            HERDR_BIN|HERDR_BIN_PATH|HERDR_PLUTIL|HERDR_RELEASE_ROOT|HERDR_WEB_ROOT|\
            HERDR_RELAY_BIN|HERDR_RELAY_BIN_DIR|HERDR_APP_DEPLOY_NPX|HERDR_APP_DEPLOY_NODE_DIR|\
            HERDR_DEV_*|HERDR_CLI_*|HERDR_STABLE_*|HERDR_SETUP_*) return 1 ;;
            HERDR_RELAY_ENV) [ "${LITERAL_VALUES[index]}" = "$env_file" ] || return 1 ;;
            HERDR_PLUGIN_CONFIG_DIR) [ "${LITERAL_VALUES[index]}" = "$(dirname "$env_file")" ] || return 1 ;;
            HERDR_*|CLOUDFLARED_*|TUNNEL_ORIGIN_CERT|GH_TOKEN|GITHUB_TOKEN|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|NO_PROXY|http_proxy|https_proxy|all_proxy|no_proxy) ;;
            *) return 1 ;;
        esac
    done
    # Selected updater credentials are read-only roles, never definitions to
    # delete. Reuse the existing role-disjointness guard without token reads,
    # migration, recovery creation or changing transaction machinery.
    literal_env_get HERDR_GITHUB_TOKEN_FILE; pointer="$LITERAL_VALUE"
    case "$(uname -s)" in Linux) manager=systemd ;; Darwin) manager=launchd ;; *) return 1 ;; esac
    for label in $(service_removal_labels "$manager"); do
        destination="$(service_removal_path "$manager" "$label")" || return 1
        [ "$env_file" != "$destination" ] || return 1
        native_selected_pointer_disjoint "$pointer" "$env_file" "$destination" || return 1
        for key in CLOUDFLARED_CONFIG TUNNEL_ORIGIN_CERT; do
            literal_env_get "$key"
            service_removal_selected_disjoint "$LITERAL_VALUE" "$destination" || return 1
        done
    done
    literal_env_get HERDR_GATEWAY_URL; gateways="$LITERAL_VALUE"
    literal_env_get HERDR_RELAY_TRANSPORT
    mode="$LITERAL_VALUE"
    mode="${mode#"${mode%%[![:space:]]*}"}"; mode="${mode%"${mode##*[![:space:]]}"}"
    mode="$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]')" || return 1
    ambient="${HERDR_RELAY_TRANSPORT:-}"
    ambient="${ambient#"${ambient%%[![:space:]]*}"}"; ambient="${ambient%"${ambient##*[![:space:]]}"}"
    ambient="$(printf '%s' "$ambient" | tr '[:upper:]' '[:lower:]')" || return 1
    case "$mode" in ''|cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) ;; *) return 1 ;; esac
    case "$ambient" in ''|cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) ;; *) return 1 ;; esac
    [ -z "$mode" ] || [ -z "$ambient" ] || [ "$mode" = "$ambient" ] || return 1
    mode="${mode:-$ambient}"
    if [ -n "$gateways" ]; then case "$mode" in ''|gateway) ;; *) return 1 ;; esac; fi
    case "$prior_disposition" in
        retained) echo 'Prior explicit route disposition: retained; no route operation attempted here.'; return 0 ;;
        removed) echo 'Prior explicit route disposition: removed; no route operation or readback attempted here.'; return 0 ;;
        '') ;;
        *) echo 'Invalid prior CLI route disposition.' >&2; return 1 ;;
    esac
    [ "$mode" = tailscale-cli ] || return 0
    echo 'The CLI-backed Tailscale Serve route is persistent and is not removed with this service.'
    echo 'Choose whether to remove that exact route, retain it, or cancel service removal.'
    [ -t 0 ] || { echo 'Interactive route disposition is required; the service was left installed.' >&2; return 1; }
    read -r -p 'Route disposition [r]emove / [k]eep route / [c]ancel: ' choice || return 1
    case "$choice" in
        r|R|remove|REMOVE)
            (unset HERDR_TAILSCALE_REQUEST; HERDR_RELAY_ENV="$env_file" "$script_dir/tailscale-cli.sh" unpublish) || {
                echo 'Exact route removal failed; the service was left installed. Choose keep explicitly or retry after recovery.' >&2; return 1;
            }
            ;;
        k|K|keep|KEEP) echo 'Persistent Tailscale Serve route will remain after this service is removed.' ;;
        *) echo 'Cancelled; service and Tailscale Serve route were left unchanged.'; return 1 ;;
    esac
    return 0
}

# Transport selection is intentionally centralized. An unset mode preserves the
# historical inference (gateway when HERDR_GATEWAY_URL is present, otherwise
# Cloudflare); an explicit mode is validated instead of silently combining two
# transports.
relay_transport_mode() {
    local env_file="${1:-${HERDR_RELAY_ENV:-}}"
    local mode=""
    local persisted_mode=""
    local persisted_gateway=""
    local gateways

    if [ "${HERDR_TAILSCALE_REQUEST:-}" = 1 ]; then
        mode=tailscale
    elif [ -n "$env_file" ]; then
        persisted_mode="$(env_file_value "$env_file" HERDR_RELAY_TRANSPORT)"
        persisted_gateway="$(env_file_value "$env_file" HERDR_GATEWAY_URL)"
        if [ -n "$persisted_mode" ]; then
            mode="$persisted_mode"
        elif [ -n "$persisted_gateway" ]; then
            mode=gateway
        fi
    fi
    if [ -z "$mode" ] && [ -n "${HERDR_RELAY_TRANSPORT+x}" ]; then
        mode="$HERDR_RELAY_TRANSPORT"
    fi
    mode="$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
    if [ -z "$mode" ]; then
        gateways="$(gateway_urls "$env_file")"
        if [ -n "$gateways" ]; then
            mode=gateway
        else
            mode=cloudflare
        fi
    fi
    case "$mode" in
        cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) printf '%s\n' "$mode" ;;
        *)
            echo "✗ Invalid HERDR_RELAY_TRANSPORT: $mode" >&2
            return 1
            ;;
    esac
}

# Installed entrypoints must not migrate/write configuration before their
# foreground/CLI/selection refusals. An unmigrated legacy plugin environment
# needs an explicit file choice, never silent Cloudflare inference.
relay_service_env_file() {
    local script_dir="$1" env_file
    env_file="$(relay_env_file_read_only "$script_dir")" || return 1
    if [ -z "${HERDR_RELAY_ENV:-}" ] && [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ] &&
        [ ! -f "$env_file" ] && [ -f "$script_dir/.env" ]; then
        echo "✗ Legacy relay configuration needs an explicit HERDR_RELAY_ENV selection before service installation/start; no files were migrated." >&2
        return 1
    fi
    printf '%s\n' "$env_file"
}

# Installed entrypoints call this only after loading the selected environment.
# Unlike chooser overrides, dispatch must agree with config.resolveTransport:
# trim edges (not embedded whitespace), infer legacy gateway, reject conflicts.
relay_service_transport_mode() {
    local mode gateways
    mode="$(printf '%s' "${HERDR_RELAY_TRANSPORT:-}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')"
    gateways="$(gateway_urls)"
    if [ -z "$mode" ]; then
        if [ -n "$gateways" ]; then mode=gateway; else mode=cloudflare; fi
    fi
    case "$mode" in
        gateway)
            [ -n "$gateways" ] || { echo "✗ Gateway transport requires HERDR_GATEWAY_URL." >&2; return 1; }
            ;;
        cloudflare|tailscale|tailscale-cli|tailscale-external)
            [ -z "$gateways" ] || { echo "✗ HERDR_GATEWAY_URL conflicts with the selected transport." >&2; return 1; }
            ;;
        *) echo "✗ Invalid HERDR_RELAY_TRANSPORT selection." >&2; return 1 ;;
    esac
    [ "${HERDR_TAILSCALE_REQUEST:-}" != 1 ] || mode=tailscale
    printf '%s\n' "$mode"
}

# Validate the saved list without choosing/publishing another transport or
# rewriting it. Runtime requires ws/wss; the stricter packaged origin parser
# additionally refuses credentials and non-base URLs. Empty entries match the
# legacy runtime list parser; embedded whitespace is never silently removed.
validate_service_gateway_urls() (
    local raw="${HERDR_GATEWAY_URL:-}" entry count=0
    set -f
    IFS=','
    # shellcheck disable=SC2086
    set -- $raw
    for entry in "$@"; do
        entry="$(printf '%s' "$entry" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
        [ -n "$entry" ] || continue
        case "$entry" in ws://*|wss://*) ;; *) return 1 ;; esac
        normalize_gateway_url "$entry" >/dev/null 2>&1 || return 1
        count=$((count + 1))
    done
    [ "$count" -gt 0 ]
)

# Noninteractive native install prerequisite: an already selected HTTPS app,
# never discovery, publication, prompts or recording a changed origin.
gateway_install_phone_app_origin() {
    local env_file="$1" origin
    validate_service_gateway_urls || {
        echo "✗ Configure a valid ws:// or wss:// base gateway list in HERDR_GATEWAY_URL before installing." >&2
        return 1
    }
    origin="$(phone_app_base_url "" "$env_file" 2>/dev/null)" || origin=""
    case "$origin" in
        https://*) printf '%s\n' "$origin" ;;
        *)
            echo "✗ Select an installed HTTPS Herdr phone app first (HERDR_PHONE_APP_URL or saved app origin)." >&2
            echo "  A gateway carries relay traffic only; the installer does not choose or publish an app." >&2
            return 1
            ;;
    esac
}

# /readyz stays transport-neutral. This distinct /healthz gate binds actual
# registration to the exact already verified local relay/release/bundle.
verify_gateway_install_readiness() {
    local port="$1" health="$2" origin="$3" binary attempt observed version revision web_hash instance
    binary="$(relay_binary)" || return 1
    instance="$(json_string_field "$health" instance "$binary")"
    version="$(json_string_field "$health" release_version "$binary")"
    revision="$(json_string_field "$health" revision "$binary")"
    web_hash="$(json_string_field "$health" bundle_hash "$binary")"
    [ -n "$instance" ] && [ -n "$version" ] && [ -n "$revision" ] && [ -n "$web_hash" ] || return 1
    # Pin the extracted packaged web root, not HERDR_WEB_ROOT. The optional
    # hash gate ties the full trusted app comparison to this exact /readyz
    # identity, including a different build of the same release/revision.
    if ! "$binary" verify-public --web-root "$(dirname "$binary")/web" --origin "$origin" \
        --version "$version" --revision "$revision" --web-hash "$web_hash" >/dev/null 2>&1; then
        echo "✗ The selected HTTPS phone app must serve the exact trusted packaged bundle; restoring the previous installation." >&2
        return 1
    fi
    for ((attempt = 1; attempt <= 15; attempt++)); do
        if observed="$(curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 --max-filesize 65536 \
            "http://127.0.0.1:$port/healthz" 2>/dev/null)" &&
            printf '%s\n' "$observed" | "$binary" verify-gateway-health \
                "$instance" "$version" "$revision" "$web_hash" >/dev/null 2>&1; then
            echo "Gateway registration and exact trusted phone-app bundle verified; phone reachability remains unproved."
            return 0
        fi
        if [ "$attempt" -lt 15 ]; then sleep 1; fi
    done
    echo "✗ The selected relay did not report verified gateway registration with usable inventory; restoring the previous installation." >&2
    return 1
}

clear_tailscale_selection() {
    local env_file="$1"

    remove_env_value_atomic "$env_file" HERDR_TAILSCALE_ORIGIN
    remove_env_value_atomic "$env_file" HERDR_RELAY_PAIRING_SOCKET
    remove_env_value_atomic "$env_file" HERDR_RELAY_RUN_ID
    remove_env_value_atomic "$env_file" HERDR_TAILSCALE_HTTPS_PORT
    remove_env_value_atomic "$env_file" HERDR_EXTERNAL_HTTPS_ORIGIN
    unset HERDR_TAILSCALE_ORIGIN HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_RUN_ID
    unset HERDR_TAILSCALE_HTTPS_PORT HERDR_EXTERNAL_HTTPS_ORIGIN
    unset HERDR_RELAY_CONTROL_RUN_ID
}

tailscale_cli_transport_switch_safe() {
    local env_file="$1"
    local binary state_root coordination_root installation_id backend_port state_home current_mode

    # HERDR_TAILSCALE_REQUEST intentionally overrides transport selection for
    # the native launcher. It must not hide a persisted CLI registration from
    # this pre-dispatch ownership check.
    current_mode="$( (unset HERDR_TAILSCALE_REQUEST; relay_transport_mode "$env_file") )" || return 1
    [ "$current_mode" = tailscale-cli ] || return 0
    binary="$(relay_binary)" || return 1
    state_home="${XDG_STATE_HOME:-$HOME/.local/state}"
    state_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_STATE_ROOT)"
    coordination_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_COORDINATION_ROOT)"
    installation_id="$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)"
    backend_port="$(env_file_value "$env_file" HERDR_RELAY_PORT)"
    state_root="${state_root:-$state_home/herdr-mobile-relay/tailscale-cli-registration}"
    coordination_root="${coordination_root:-$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination}"
    case "$backend_port" in ''|*[!0-9]*) echo "✗ Cannot change transport without a valid CLI backend port." >&2; return 1 ;; esac
    "$binary" tailscale-cli check-transport-switch --state-root "$state_root" \
        --coordination-root "$coordination_root" --installation-id "$installation_id" \
        --backend-port "$backend_port" || {
        echo "✗ The CLI Serve route or backend reservation is unresolved; explicitly unpublish/recover it before changing transport." >&2
        return 1
    }
}

tailscale_cli_transport_switch_with_commit() {
    local env_file="$1"
    local mode="$2"
    local binary state_root coordination_root installation_id backend_port state_home helper_dir

    binary="$(relay_binary)" || return 1
    state_home="${XDG_STATE_HOME:-$HOME/.local/state}"
    state_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_STATE_ROOT)"
    coordination_root="$(env_file_value "$env_file" HERDR_TAILSCALE_CLI_COORDINATION_ROOT)"
    installation_id="$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)"
    backend_port="$(env_file_value "$env_file" HERDR_RELAY_PORT)"
    state_root="${state_root:-$state_home/herdr-mobile-relay/tailscale-cli-registration}"
    coordination_root="${coordination_root:-$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination}"
    case "$backend_port" in ''|*[!0-9]*) echo "✗ Cannot change transport without a valid CLI backend port." >&2; return 1 ;; esac
    helper_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || return 1
    "$binary" tailscale-cli with-transport-switch-lock --state-root "$state_root" \
        --coordination-root "$coordination_root" --installation-id "$installation_id" \
        --backend-port "$backend_port" -- "${BASH:-bash}" "$helper_dir/transport-switch-locked.sh" "$env_file" "$mode"
}

set_relay_transport() {
    local env_file="$1"
    local mode="$2"
    local current_mode

    case "$mode" in
        cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) ;;
        *) echo "✗ Invalid relay transport: $mode" >&2; return 1 ;;
    esac
    current_mode="$( (unset HERDR_TAILSCALE_REQUEST; relay_transport_mode "$env_file") )"
    if [ "$current_mode" = tailscale-cli ] && [ "$mode" != tailscale-cli ] &&
        [ "${HERDR_CLI_TRANSPORT_SWITCH_LOCKED:-}" != 1 ]; then
        if ! tailscale_cli_transport_switch_with_commit "$env_file" "$mode"; then
            echo "✗ The CLI Serve route or backend reservation changed during transport selection; no transport switch was committed." >&2
            return 1
        fi
        if [ "$mode" != tailscale ]; then
            unset HERDR_TAILSCALE_ORIGIN HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_RUN_ID
            unset HERDR_TAILSCALE_HTTPS_PORT HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_CONTROL_RUN_ID
        fi
        if [ "$mode" = cloudflare ] || [ "$mode" = tailscale-external ]; then
            unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
        fi
        export HERDR_RELAY_TRANSPORT="$mode"
        return 0
    fi
    if [ "$mode" != tailscale ] && [ -e "$(tailscale_session_file "$env_file")" ]; then
        echo "✗ Cannot change transport while a foreground Tailscale session is recorded." >&2
        echo "  Stop that pane and verify its route before changing transport." >&2
        return 1
    fi
    if [ -e "$(tailscale_external_session_file "$env_file")" ]; then
        local external_session
        external_session="$(tailscale_external_session_file "$env_file")"
        local switch_session_run_id="${HERDR_CLI_TRANSPORT_SWITCH_SESSION_RUN_ID:-}"
        if [ "$mode" != tailscale-external ] || [ -z "$switch_session_run_id" ] ||
            [ ! -f "$external_session" ] || [ -L "$external_session" ] ||
            [ "$(env_file_value "$external_session" HERDR_RELAY_CONTROL_RUN_ID)" != "$switch_session_run_id" ]; then
            echo "✗ Cannot change transport while an operator-owned HTTPS Serve relay is running." >&2
            echo "  Stop that foreground pane before changing transport." >&2
            return 1
        fi
    fi
    if [ "$mode" != tailscale ]; then
        clear_tailscale_selection "$env_file"
    fi
    if [ "$mode" = cloudflare ] || [ "$mode" = tailscale-external ]; then
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_URL
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION
        unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
    fi
    set_env_value_atomic "$env_file" HERDR_RELAY_TRANSPORT "$mode"
    export HERDR_RELAY_TRANSPORT="$mode"
}

installed_relay_service_definition_present() {
    case "$(uname -s)" in
        Linux)
            local path="$HOME/.config/systemd/user/herdr-mobile-relay.service"
            [ -e "$path" ] || [ -L "$path" ]
            ;;
        Darwin)
            local path="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
            [ -e "$path" ] || [ -L "$path" ]
            ;;
        *) return 1 ;;
    esac
}

installed_legacy_relay_service_definition_present() {
    case "$(uname -s)" in
        Linux)
            local path="$HOME/.config/systemd/user/herdr-remote.service"
            [ -e "$path" ] || [ -L "$path" ]
            ;;
        Darwin)
            local path="$HOME/Library/LaunchAgents/com.herdr-remote.service.plist"
            [ -e "$path" ] || [ -L "$path" ]
            ;;
        *) return 1 ;;
    esac
}

cli_setup_service_definition_present() {
    installed_relay_service_definition_present || installed_legacy_relay_service_definition_present
}

# JSON scalar accessors use the packaged relay's bounded strict decoder. A
# text match is not authority: fields must be exact top-level values in a
# complete document with the requested JSON type. Optional string/number
# telemetry collapses invalid input to empty output; booleans retain failure.
relay_json_field() {
    local kind="$1"
    local json="$2"
    local key="$3"
    local binary="${4:-}"
    local status

    if [ -z "$binary" ]; then
        binary="$(relay_binary)" || return 1
    fi
    [ -x "$binary" ] || return 1
    if printf '%s' "$json" | "$binary" json-field "$kind" "$key" 2>/dev/null; then
        return 0
    else
        status=$?
    fi
    if [ "$status" -eq 2 ]; then
        echo "✗ The selected relay binary does not support strict JSON field extraction." >&2
        echo "  Reinstall a verified relay release matching these shell helpers." >&2
    fi
    return "$status"
}

json_bool_field() {
    relay_json_field bool "$1" "$2" "${3:-}"
}

json_number_field() {
    relay_json_field number "$1" "$2" "${3:-}" || return 0
}

# Session records are generated by this checkout and are read as constrained
# key/value data, never sourced. This keeps a stale or tampered marker from
# becoming shell code.
tailscale_session_file() {
    local env_file="$1"
    printf '%s/tailscale-session.env\n' "$(dirname "$env_file")"
}

tailscale_external_session_file() {
    local env_file="$1"
    printf '%s/tailscale-external-session.env\n' "$(dirname "$env_file")"
}

tailscale_session_value() {
    local session_file="$1"
    local key="$2"

    [ -r "$session_file" ] || return 1
    sed -n "s/^${key}=//p" "$session_file" | head -1
}

tailscale_control_request() {
    local socket="$1"
    local operation="$2"
    local run_id="$3"
    local instance="$4"

    [ -n "$socket" ] && [ -n "$run_id" ] && [ -n "$instance" ] || return 1
    "$(relay_binary)" pairing-control \
        --socket "$socket" --operation "$operation" \
        --run-id "$run_id" --instance "$instance"
}

arm_tailscale_setup_link() {
    local env_file="$1"
    local session_file
    local socket
    local run_id
    local instance
    local response

    session_file="$(tailscale_session_file "$env_file")"
    socket="$(tailscale_session_value "$session_file" HERDR_RELAY_PAIRING_SOCKET || true)"
    run_id="$(tailscale_session_value "$session_file" HERDR_RELAY_RUN_ID || true)"
    instance="$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)"
    [ -n "$socket" ] && [ -n "$run_id" ] && [ -n "$instance" ] || return 1
    response="$(tailscale_control_request "$socket" arm_bootstrap "$run_id" "$instance" 2>/dev/null)" || return 1
    [ "$(json_bool_field "$response" ok)" = true ] || return 1
    [ "$(json_string_field "$response" run_id)" = "$run_id" ] || return 1
    [ "$(json_string_field "$response" instance)" = "$instance" ] || return 1
    [ "$(json_bool_field "$response" invitation_armed)" = true ] || return 1
    [ -n "$(json_string_field "$response" invitation_expires_at)" ] || return 1
    printf '%s\n' "$response"
}

shell_quote_value() {
    local value="$1"
    local prefix
    printf "'" || return 1
    while [[ "$value" == *"'"* ]]; do
        prefix="${value%%"'"*}"
        printf "%s'\\\\''" "$prefix" || return 1
        value="${value#*"'"}"
    done
    printf "%s'\n" "$value" || return 1
    return 0
}

# Literal env grammar (Bash 3.2): <=256KiB, <=4096 lines, <=8192 bytes/line.
# Blank/comment lines or [indent][export SPACE]NAME=VALUE only. VALUE may
# concatenate unquoted/single/double-quoted fragments (including shell_quote_value
# output). Backslash quotes one unquoted byte; in double quotes only \\, ", $, `
# may be escaped. No expansion, substitution, operators, multiline or controls.
# Unquoted whitespace ends the value; only whitespace/comment may follow.
# Native admission rejects ALL unsupported lines and conflicting duplicates;
# identical duplicates are allowed. Filtering parses target assignments with
# this same grammar, preserves unrelated bytes, and refuses malformed targets.
literal_env_read() {
    local file="$1" size
    local LC_ALL=C
    LITERAL_TEXT=""
    export -n LITERAL_TEXT LITERAL_VALUE LITERAL_KEY LITERAL_FILTERED
    if [ ! -e "$file" ] && [ ! -L "$file" ]; then return 0; fi
    [ -f "$file" ] && [ ! -L "$file" ] || return 1
    size="$(wc -c < "$file")" || return 1
    size="${size//[[:space:]]/}"
    case "$size" in ''|*[!0-9]*) return 1 ;; esac
    [ "$size" -le 262144 ] || return 1
    # Sentinel preserves final newlines; partial/read failures are not absence.
    LITERAL_TEXT="$(head -c 262145 "$file" && printf '.')" || return 1
    LITERAL_TEXT="${LITERAL_TEXT%.}"
    [ "${#LITERAL_TEXT}" -eq "$size" ]  # Also refuses NUL loss in Bash strings.
}

literal_env_assignment() {
    local text="$1" c next mode=bare ended=false i
    local LC_ALL=C
    LITERAL_KEY="" LITERAL_VALUE=""
    [ "${#text}" -le 8192 ] || return 2
    case "$text" in *[$'\001'-$'\010'$'\013'-$'\037'$'\177']*) return 2 ;; esac
    while [[ "$text" == [' '$'\t']* ]]; do text="${text:1}"; done
    case "$text" in ''|\#*) return 1 ;; esac
    case "$text" in export[' '$'\t']*) text="${text:6}"; while [[ "$text" == [' '$'\t']* ]]; do text="${text:1}"; done ;; esac
    case "$text" in *=*) LITERAL_KEY="${text%%=*}"; text="${text#*=}" ;; *) return 2 ;; esac
    case "$LITERAL_KEY" in ''|[0-9]*|*[!a-zA-Z0-9_]*) return 2 ;; esac
    for ((i=0; i<${#text}; i++)); do
        c="${text:i:1}"
        if [ "$ended" = true ]; then
            case "$c" in ' '|$'\t') continue ;; \#) break ;; *) return 2 ;; esac
        fi
        case "$mode:$c" in
            "single:'") mode=bare ;;
            single:*) LITERAL_VALUE="$LITERAL_VALUE$c" ;;
            'double:"') mode=bare ;;
            'bare:"') mode=double ;;
            "bare:'") mode=single ;;
            'bare: '|bare:$'\t') ended=true ;;
            'bare:\'|'double:\')
                i=$((i + 1)); [ "$i" -lt "${#text}" ] || return 2
                next="${text:i:1}"
                if [ "$mode" = double ]; then
                    case "$next" in '\'|'"'|'$'|'`') ;; *) return 2 ;; esac
                fi
                LITERAL_VALUE="$LITERAL_VALUE$next"
                ;;
            'double:$'|'double:`') return 2 ;;
            bare:*)
                case "$c" in '$'|'`'|'('|')'|'{'|'}'|';'|'&'|'|'|'<'|'>'|'*'|'?'|'['|']'|'!'|'~'|'#') return 2 ;; esac
                LITERAL_VALUE="$LITERAL_VALUE$c"
                ;;
            double:*) LITERAL_VALUE="$LITERAL_VALUE$c" ;;
        esac
    done
    [ "$mode" = bare ] || return 2
    return 0
}

literal_env_scan() {
    local file="$1" remaining line count=0 status index found
    LITERAL_KEYS=() LITERAL_VALUES=()
    literal_env_read "$file" || return 1
    remaining="$LITERAL_TEXT"
    while [ -n "$remaining" ]; do
        line="${remaining%%$'\n'*}"
        if [[ "$remaining" == *$'\n'* ]]; then remaining="${remaining#*$'\n'}"; else remaining=""; fi
        count=$((count + 1)); [ "$count" -le 4096 ] || return 1
        status=0; literal_env_assignment "$line" || status=$?
        [ "$status" -ne 1 ] || continue
        [ "$status" -eq 0 ] || return 1
        found=false
        for ((index=0; index<${#LITERAL_KEYS[@]}; index++)); do
            if [ "${LITERAL_KEYS[index]}" = "$LITERAL_KEY" ]; then
                [ "${LITERAL_VALUES[index]}" = "$LITERAL_VALUE" ] || return 1
                found=true; break
            fi
        done
        if [ "$found" = false ]; then LITERAL_KEYS+=("$LITERAL_KEY"); LITERAL_VALUES+=("$LITERAL_VALUE"); fi
    done
    return 0
}

literal_env_get() {
    local key="$1" index
    LITERAL_FOUND=false LITERAL_VALUE=""
    for ((index=0; index<${#LITERAL_KEYS[@]}; index++)); do
        if [ "${LITERAL_KEYS[index]}" = "$key" ]; then
            LITERAL_FOUND=true LITERAL_VALUE="${LITERAL_VALUES[index]}"; return 0
        fi
    done
    return 0
}

literal_env_filter() {
    local key="$1" remaining="$LITERAL_TEXT" line newline status count=0
    LITERAL_FILTERED="" LITERAL_FOUND=false
    while [ -n "$remaining" ]; do
        line="${remaining%%$'\n'*}"; newline=""
        if [[ "$remaining" == *$'\n'* ]]; then remaining="${remaining#*$'\n'}"; newline=$'\n'; else remaining=""; fi
        count=$((count + 1)); [ "$count" -le 4096 ] || return 1
        status=0; literal_env_assignment "$line" || status=$?
        if [ "$status" -eq 0 ] && [ "$LITERAL_KEY" = "$key" ]; then
            LITERAL_FOUND=true; continue
        fi
        if [ "$status" -eq 2 ]; then case "$line" in *"$key"*) return 1 ;; esac; fi
        LITERAL_FILTERED="$LITERAL_FILTERED$line$newline"
    done
    return 0
}

literal_env_lookup() {
    local file="$1" key="$2" remaining line status found=false value="" count=0
    literal_env_read "$file" || return 1
    remaining="$LITERAL_TEXT"
    while [ -n "$remaining" ]; do
        line="${remaining%%$'\n'*}"
        if [[ "$remaining" == *$'\n'* ]]; then remaining="${remaining#*$'\n'}"; else remaining=""; fi
        count=$((count + 1)); [ "$count" -le 4096 ] || return 1
        status=0; literal_env_assignment "$line" || status=$?
        if [ "$status" -eq 2 ]; then case "$line" in *"$key"*) return 1 ;; esac; fi
        if [ "$status" -eq 0 ] && [ "$LITERAL_KEY" = "$key" ]; then found=true; value="$LITERAL_VALUE"; fi
    done
    LITERAL_FOUND="$found" LITERAL_VALUE="$value"
    return 0
}

literal_env_replace() {
    local file="$1" data="$2" directory temp
    directory="$(dirname "$file")" || return 1
    mkdir -p "$directory" || return 1
    temp="$(mktemp "$directory/.relay-env.XXXXXX")" || return 1
    if ! printf '%s' "$data" > "$temp" || ! chmod 600 "$temp" || ! mv -f "$temp" "$file"; then
        rm -f "$temp"; return 1
    fi
    return 0
}

set_env_value_atomic() {
    set +x
    local file="$1" key="$2" value="$3" quoted
    case "$key" in ''|[0-9]*|*[!A-Z0-9_]*) return 1 ;; esac
    case "$value" in *$'\n'*|*$'\r'*) return 1 ;; esac
    literal_env_read "$file" && literal_env_filter "$key" || return 1
    quoted="$(shell_quote_value "$value")" || return 1
    # Serialization must be complete and lossless before creating a temporary.
    literal_env_assignment "$key=$quoted" || return 1
    [ "$LITERAL_KEY" = "$key" ] && [ "$LITERAL_VALUE" = "$value" ] || return 1
    if [ -n "$LITERAL_FILTERED" ] && [[ "$LITERAL_FILTERED" != *$'\n' ]]; then LITERAL_FILTERED="$LITERAL_FILTERED"$'\n'; fi
    literal_env_replace "$file" "$LITERAL_FILTERED$key=$quoted"$'\n'
}

remove_env_value_if_equals_atomic() {
    set +x
    local file="$1" key="$2" expected="$3" current
    literal_env_lookup "$file" "$key" || return 1
    current="$LITERAL_VALUE"
    [ "$LITERAL_FOUND" = true ] && [ "$current" = "$expected" ] || return 0
    remove_env_value_atomic "$file" "$key"
}

remove_env_value_atomic() {
    set +x
    local file="$1" key="$2"
    case "$key" in ''|[0-9]*|*[!A-Z0-9_]*) return 1 ;; esac
    literal_env_read "$file" && literal_env_filter "$key" || return 1
    [ "$LITERAL_FOUND" = true ] || return 0
    literal_env_replace "$file" "$LITERAL_FILTERED"
}

persist_github_token() {
    set +x
    local file="$1" token="${2:-${GH_TOKEN:-}}" source="${3:-$1}" directory pointer recovery published=false failed=false target index
    export -n token
    [ -n "$token" ] || return 0
    directory="$(dirname "$file")" || return 1
    pointer="$directory/github-token"
    [ "$(canonical_file_path "$file")" != "$(canonical_file_path "$pointer")" ] || return 1
    recovery="$(mktemp -d "$directory/.relay-persist.XXXXXX")" || return 1
    chmod 700 "$recovery" || { rm -rf "$recovery"; return 1; }
    index=0
    for target in "$file" "$pointer"; do
        if [ -e "$target" ] || [ -L "$target" ]; then
            if ! native_mutable_file_safe "$target" || ! cp -p "$target" "$recovery/$index"; then rm -rf "$recovery"; return 1; fi
        fi
        index=$((index + 1))
    done
    if ! { [ ! -e "$source" ] || cp -p "$source" "$recovery/candidate"; } ||
        ! set_env_value_atomic "$recovery/candidate" HERDR_GITHUB_TOKEN_FILE "$pointer" ||
        ! printf '%s\n' "$token" > "$recovery/token" || ! chmod 600 "$recovery/token"; then
        rm -rf "$recovery"; return 1
    fi
    # A second-resource rename failure restores the first resource's exact
    # bytes/mode/absence. Failed restoration retains this owned private backup.
    if mv -f "$recovery/token" "$pointer"; then
        published=true
        if mv -f "$recovery/candidate" "$file"; then rm -rf "$recovery"; return $?; fi
    fi
    if [ "$published" = true ]; then
        if [ -f "$recovery/1" ]; then
            cp -p "$recovery/1" "$recovery/restore" && mv -f "$recovery/restore" "$pointer" || failed=true
        else rm -f "$pointer" || failed=true; fi
    fi
    if [ "$failed" = true ]; then echo 'Updater persistence restoration is incomplete; private recovery retained.' >&2;
    else rm -rf "$recovery"; fi
    return 1
}

append_env_default() {
    local file="$1" key="$2" value="$3"
    literal_env_lookup "$file" "$key" || return 1
    [ "$LITERAL_FOUND" = false ] || return 0
    set_env_value_atomic "$file" "$key" "$value"
}

ensure_relay_env() {
    set +x
    local file="$1" cloudflared_config="${2:-}" token="${3:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}" value directory candidate
    export -n token
    unset GH_TOKEN GITHUB_TOKEN
    literal_env_read "$file" || return 1
    [ -e "$file" ] || umask 077
    directory="$(dirname "$file")" || return 1
    mkdir -p "$directory" || return 1
    candidate="$(mktemp "$directory/.relay-defaults.XXXXXX")" || return 1
    if [ -e "$file" ] && ! cp -p "$file" "$candidate"; then rm -f "$candidate"; return 1; fi
    if ! literal_env_lookup "$candidate" HERDR_RELAY_TOKEN; then rm -f "$candidate"; return 1; fi
    if [ -z "$LITERAL_VALUE" ]; then
        value="$(generate_token)" || { rm -f "$candidate"; return 1; }
        [ -n "$value" ] && set_env_value_atomic "$candidate" HERDR_RELAY_TOKEN "$value" || { rm -f "$candidate"; return 1; }
    fi
    literal_env_lookup "$candidate" HERDR_RELAY_INSTANCE_ID || { rm -f "$candidate"; return 1; }
    if [ -z "$LITERAL_VALUE" ]; then
        value="$(generate_instance_id)" || { rm -f "$candidate"; return 1; }
        [ -n "$value" ] && set_env_value_atomic "$candidate" HERDR_RELAY_INSTANCE_ID "$value" || { rm -f "$candidate"; return 1; }
    fi
    if [ -n "$cloudflared_config" ] && ! append_env_default "$candidate" CLOUDFLARED_CONFIG "$cloudflared_config"; then rm -f "$candidate"; return 1; fi
    if ! remove_env_value_atomic "$candidate" GH_TOKEN || ! remove_env_value_atomic "$candidate" GITHUB_TOKEN || ! chmod 600 "$candidate"; then rm -f "$candidate"; return 1; fi
    if [ -n "$token" ]; then
        if ! persist_github_token "$file" "$token" "$candidate"; then rm -f "$candidate"; return 1; fi
        rm -f "$candidate" || return 1
    elif ! mv -f "$candidate" "$file"; then rm -f "$candidate"; return 1; fi
    return 0
}

# Native-only selected credential/policy admission. Never source selected text,
# never merge ambient updater credentials, never construct an app/store here.
native_installer_admit() {
    set +x
    local file="$1" runtime_file="${2:-$1}" key value index
    local LC_ALL=C
    export -n value
    unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
    NATIVE_SELECTED_TOKEN="" NATIVE_SELECTED_POINTER="" NATIVE_SIDECAR=""
    export -n NATIVE_SELECTED_TOKEN NATIVE_SELECTED_POINTER NATIVE_SIDECAR
    if [ -e "$file" ] || [ -L "$file" ]; then native_mutable_file_safe "$file" || return 1; fi
    literal_env_scan "$file" || { echo 'Invalid literal native configuration.' >&2; return 1; }
    case "$runtime_file" in /*) ;; *) echo 'Native runtime must be absolute.' >&2; return 1 ;; esac
    # Only configuration namespaces are loadable. Shell/tool-routing internals
    # (PATH, BASH_ENV, IFS, native_* etc.) cannot be selected-file assignments.
    for ((index=0; index<${#LITERAL_KEYS[@]}; index++)); do
        key="${LITERAL_KEYS[index]}" value="${LITERAL_VALUES[index]}"
        case "$key" in
            GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE) continue ;;
            HERDR_RELAY_ENV) [ "$value" = "$runtime_file" ] || return 1; continue ;;
            HERDR_PLUGIN_CONFIG_DIR) [ "$value" = "$(dirname "$runtime_file")" ] || return 1; continue ;;
            HERDR_*|CLOUDFLARED_*|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|NO_PROXY|http_proxy|https_proxy|all_proxy|no_proxy) ;;
            *) echo 'Unsupported native configuration assignment.' >&2; return 1 ;;
        esac
        printf -v "$key" '%s' "$value" || return 1
        export "$key"
    done
    [ -z "${HERDR_RELAY_RUN_ID:-}${HERDR_RELAY_CONTROL_RUN_ID:-}${HERDR_RELAY_PAIRING_SOCKET:-}" ] || { echo 'Installed policy refuses foreground identities.' >&2; return 1; }
    case "${HERDR_RELAY_REARM_BOOTSTRAP:-}" in ''|0|false|False|FALSE|f|F) ;; *) echo 'Installed policy refuses rearm.' >&2; return 1 ;; esac
    [ -z "${HERDR_RELAY_TOKEN:-}" ] || [ "${#HERDR_RELAY_TOKEN}" -eq 32 ] || { echo 'Installed policy requires a 32-byte key.' >&2; return 1; }
    case "${HERDR_RELAY_PORT:-8375}" in ''|*[!0-9]*) return 1 ;; esac
    value="${HERDR_RELAY_PORT:-8375}"
    [ "${#value}" -le 5 ] || return 1
    [ "$((10#$value))" -gt 0 ] && [ "$((10#$value))" -le 65535 ] || return 1
    # gateway_urls normally falls back to a sourced file. Here every selected
    # assignment is already literal data: disable that fallback for this call.
    TRANSPORT="$(HERDR_RELAY_ENV='' relay_service_transport_mode)" || return 1
    case "$TRANSPORT" in cloudflare|gateway) ;; *) echo 'Unavailable background transport.' >&2; return 1 ;; esac
    [ "$TRANSPORT" != gateway ] || validate_service_gateway_urls || return 1
    [ ! -e "$(tailscale_session_file "$runtime_file")" ] && [ ! -e "$(tailscale_external_session_file "$runtime_file")" ] || return 1
    literal_env_get HERDR_GITHUB_TOKEN_FILE; NATIVE_SELECTED_POINTER="$LITERAL_VALUE"
    if [ -n "$NATIVE_SELECTED_POINTER" ]; then
        case "$NATIVE_SELECTED_POINTER" in /*) ;; *) return 1 ;; esac
        private_owned_file "$NATIVE_SELECTED_POINTER" || { echo 'Selected updater resource requires recovery.' >&2; return 1; }
        native_selected_pointer_disjoint "$NATIVE_SELECTED_POINTER" "$runtime_file" "${3:-}" "${4:-}" || return 1
    else
        literal_env_get GH_TOKEN; NATIVE_SELECTED_TOKEN="$LITERAL_VALUE"
        literal_env_get GITHUB_TOKEN
        if [ -n "$NATIVE_SELECTED_TOKEN" ] && [ -n "$LITERAL_VALUE" ] && [ "$NATIVE_SELECTED_TOKEN" != "$LITERAL_VALUE" ]; then
            echo 'Ambiguous selected updater credentials require recovery.' >&2; return 1
        fi
        NATIVE_SELECTED_TOKEN="${NATIVE_SELECTED_TOKEN:-$LITERAL_VALUE}"
        if [ -n "$NATIVE_SELECTED_TOKEN" ]; then
            NATIVE_SIDECAR="$(dirname "$runtime_file")/github-token" || return 1
            [ "$(canonical_file_path "$NATIVE_SIDECAR")" != "$(canonical_file_path "$runtime_file")" ] || return 1
            if [ -e "$NATIVE_SIDECAR" ] || [ -L "$NATIVE_SIDECAR" ]; then
                native_mutable_file_safe "$NATIVE_SIDECAR" || return 1
            fi
        fi
    fi
    # Relative ancestors/symlinks are not hidden runtime/credential selectors.
    native_resource_parents_safe "$runtime_file" || return 1
    [ -z "$NATIVE_SELECTED_POINTER" ] || native_resource_parents_safe "$NATIVE_SELECTED_POINTER" || return 1
    [ -z "$NATIVE_SIDECAR" ] || native_resource_parents_safe "$NATIVE_SIDECAR" || return 1
    return 0
}

native_mutable_file_safe() {
    local identity owner mode links
    [ -f "$1" ] && [ ! -L "$1" ] || return 1
    identity="$(stat -c '%u:%a:%h' "$1" 2>/dev/null)" || identity="$(stat -f '%u:%Lp:%l' "$1" 2>/dev/null)" || return 1
    IFS=: read -r owner mode links <<< "$identity"
    [ "$owner" = "$(id -u)" ] && [ "$links" = 1 ] || return 1
    case "$mode" in ''|*[!0-7]*) return 1 ;; esac
    [ "$((8#$mode & 0022))" -eq 0 ]
}

native_resource_parents_safe() {
    local path="$1" parent
    # Missing ancestors must not hide future aliases through mkdir + '..'.
    # Require an absolute, unambiguous spelling before snapshot/publication.
    case "$path" in /*) ;; *) return 1 ;; esac
    case "/${path#/}/" in *'/../'*|*'/./'*|*'//'*) return 1 ;; esac
    parent="$(dirname "$path")" || return 1
    while [ "$parent" != / ] && [ "$parent" != . ]; do
        [ ! -L "$parent" ] || return 1
        parent="$(dirname "$parent")" || return 1
    done
    return 0
}

# A selected credential is read-only, not a mutable transaction destination.
# Refuse overlapping roles rather than snapshotting and later destroying it.
native_selected_pointer_disjoint() {
    local pointer="$1" destination identity other
    [ -n "$pointer" ] || return 0
    native_resource_parents_safe "$pointer" && private_owned_file "$pointer" || return 1
    identity="$(stat -c '%d:%i' "$pointer" 2>/dev/null)" ||
        identity="$(stat -f '%d:%i' "$pointer" 2>/dev/null)" || return 1
    [[ "$identity" =~ ^[0-9]+:[0-9]+$ ]] || return 1
    shift
    for destination in "$@"; do
        [ -n "$destination" ] || continue
        native_resource_parents_safe "$destination" || return 1
        if [ "$pointer" = "$destination" ]; then
            echo 'Selected updater resource overlaps native transaction destinations.' >&2
            return 1
        fi
        if [ -e "$destination" ] || [ -L "$destination" ]; then
            native_mutable_file_safe "$destination" || return 1
            other="$(stat -c '%d:%i' "$destination" 2>/dev/null)" ||
                other="$(stat -f '%d:%i' "$destination" 2>/dev/null)" || return 1
            [[ "$other" =~ ^[0-9]+:[0-9]+$ ]] || return 1
            # Also catch differently spelled names on case-insensitive filesystems.
            if [ "$identity" = "$other" ]; then
                echo 'Selected updater resource overlaps native transaction destinations.' >&2
                return 1
            fi
        fi
    done
    return 0
}

native_installer_candidate() {
    umask 077
    local file="$1" cloudflared_config="${2:-}" token="$NATIVE_SELECTED_TOKEN" pointer="$NATIVE_SELECTED_POINTER" sidecar="$NATIVE_SIDECAR"
    local candidate="$native_recovery/candidate.env"
    local LC_ALL=C
    export -n token
    [ "$TRANSPORT" = cloudflare ] || cloudflared_config=""
    if [ -e "$file" ]; then cp -p "$file" "$candidate" || return 1; else : > "$candidate" || return 1; fi
    if [ -n "$token" ]; then
        printf '%s\n' "$token" > "$native_recovery/candidate.token" && chmod 600 "$native_recovery/candidate.token" || return 1
        set_env_value_atomic "$candidate" HERDR_GITHUB_TOKEN_FILE "$sidecar" || return 1
    fi
    set_env_value_atomic "$candidate" HERDR_RELAY_TRANSPORT "$TRANSPORT" || return 1
    set_env_value_atomic "$candidate" HERDR_RELAY_PORT "${HERDR_RELAY_PORT:-8375}" || return 1
    if [ "$TRANSPORT" = gateway ]; then
        set_env_value_atomic "$candidate" HERDR_GATEWAY_URL "$HERDR_GATEWAY_URL" || return 1
    elif [ -n "${CLOUDFLARED_BIN:-}" ]; then
        set_env_value_atomic "$candidate" CLOUDFLARED_BIN "$CLOUDFLARED_BIN" || return 1
    fi
    ensure_relay_env "$candidate" "$cloudflared_config" '' || return 1
    # Validate the ACTUAL candidate grammar and staged token before constructing
    # the policy view. Never use the token candidate as executable shell data.
    literal_env_scan "$candidate" || return 1
    if [ -n "$token" ]; then
        literal_env_get HERDR_GITHUB_TOKEN_FILE
        [ "$LITERAL_VALUE" = "$sidecar" ] && private_owned_file "$native_recovery/candidate.token" || return 1
        literal_env_read "$native_recovery/candidate.token" || return 1
        [ "$LITERAL_TEXT" = "$token"$'\n' ] || return 1
    fi
    # A staged migration pointer is not live yet: policy validates all other
    # generated defaults with the real runtime path, then retains that pointer.
    cp -p "$candidate" "$native_recovery/admission.env" || return 1
    if [ -n "$token" ]; then remove_env_value_atomic "$native_recovery/admission.env" HERDR_GITHUB_TOKEN_FILE || return 1; fi
    native_installer_admit "$native_recovery/admission.env" "$file" "${native_definition:-}" "${native_legacy_definition:-}" || return 1
    [ -n "${HERDR_RELAY_TOKEN:-}" ] && [ "${#HERDR_RELAY_TOKEN}" -eq 32 ] || return 1
    NATIVE_SELECTED_POINTER="$pointer" NATIVE_SIDECAR="$sidecar"
    return 0
}

load_relay_env() {
    local env_file="$1"
    if [ ! -f "$env_file" ]; then
        return
    fi
    set -a
    # shellcheck source=/dev/null
    . "$env_file"
    set +a
}

wait_for_relay_identity_health() {
    local port="$1"
    local instance="$2"
    local origin="$3"
    local attempts="${4:-30}"
    local attempt
    local health

    command -v curl >/dev/null 2>&1 || return 1
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(curl --noproxy '*' --fail --silent --show-error --connect-timeout 1 --max-time 2 \
            "http://127.0.0.1:$port/healthz" 2>/dev/null)" &&
            [ "$(json_string_field "$health" status)" = ok ] &&
            [ "$(json_string_field "$health" readiness)" = ready ] &&
            [ "$(json_string_field "$health" transport)" = tailscale-cli ] &&
            [ "$(json_string_field "$health" instance)" = "$instance" ] &&
            [ "$(json_string_field "$health" tailscale_cli_origin)" = "$origin" ]; then
            printf '%s\n' "$health"
            return 0
        fi
        if [ "$attempt" -lt "$attempts" ]; then sleep 1; fi
    done
    return 1
}

systemd_quote_value() {
    local value="$1"
    case "$value" in *$'\n'*|*$'\r'*) return 1 ;; esac
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//%/%%}"
    printf '"%s"' "$value"
}

systemd_quote_exec() {
    local value="$1"
    value="${value//\$/\$\$}"
    systemd_quote_value "$value"
}

sed_escape_replacement() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//&/\\&}"
    value="${value//|/\\|}"
    printf '%s' "$value"
}

systemd_unquote_value() {
    local value="$1"
    local result=""
    local character
    local escaped=false
    local index
    if [[ "$value" == \"*\" ]]; then
        value="${value:1:${#value}-2}"
    fi
    for ((index = 0; index < ${#value}; index++)); do
        character="${value:index:1}"
        if [ "$escaped" = true ]; then
            result+="$character"
            escaped=false
        elif [ "$character" = \\ ]; then
            escaped=true
        else
            result+="$character"
        fi
    done
    [ "$escaped" = false ] || return 1
    result="${result//%%/%}"
    printf '%s' "$result"
}

xml_escape_text() {
    local value="$1"
    local result=""
    local character
    local index

    case "$value" in *$'\n'*|*$'\r'*) return 1 ;; esac
    for ((index = 0; index < ${#value}; index++)); do
        character="${value:index:1}"
        case "$character" in
            '&') result+='&amp;' ;;
            '<') result+='&lt;' ;;
            '>') result+='&gt;' ;;
            *) result+="$character" ;;
        esac
    done
    printf '%s' "$result"
}

wait_for_cli_admission() {
    local relay_bin="$1"
    local socket="$2"
    local run_id="$3"
    local instance="$4"
    local attempts="${5:-30}"
    local attempt
    local status

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if status="$("$relay_bin" pairing-control --socket "$socket" --operation status \
            --run-id "$run_id" --instance "$instance" 2>/dev/null)" &&
            [ "$(json_bool_field "$status" ready)" = true ] &&
            [ "$(json_bool_field "$status" persistent_route_ready)" = true ] &&
            [ "$(json_bool_field "$status" invitation_armed)" = true ]; then
            printf '%s\n' "$status"
            return 0
        fi
        if [ "$attempt" -lt "$attempts" ]; then sleep 1; fi
    done
    return 1
}

wait_for_relay_health() {
    local port="${1:-8375}"
    local attempts="${2:-15}"
    local delay="${3:-1}"
    local health
    local attempt
    local expected_instance="${4:-${HERDR_RELAY_INSTANCE_ID:-}}"
    local expected_version="${5:-}"
    local expected_revision="${6:-}"
    local expected_web_hash="${7:-}"
    local binary identity manifest manifest_json
    binary="$(relay_binary)" || return 1
    if [ -z "$expected_version" ] || [ -z "$expected_revision" ]; then
        identity="$("$binary" version --json)" || return 1
        expected_version="$(json_string_field "$identity" version)"
        expected_revision="$(json_string_field "$identity" revision)"
    fi
    if [ -z "$expected_web_hash" ]; then
        manifest="$(dirname "$binary")/release-manifest.json"
        if [ ! -f "$manifest" ] || [ ! -r "$manifest" ]; then
            echo "✗ Relay release identity manifest is missing or unreadable: $manifest" >&2
            echo "  Reinstall the verified relay release before checking readiness." >&2
            return 1
        fi
        if ! manifest_json="$(cat "$manifest" 2>/dev/null)"; then
            echo "✗ Relay release identity manifest is unreadable: $manifest" >&2
            return 1
        fi
        expected_web_hash="$(json_string_field "$manifest_json" web_hash)"
        if [ -z "$expected_web_hash" ]; then
            echo "✗ Relay release identity manifest has no non-empty web_hash: $manifest" >&2
            echo "  Reinstall the verified relay release before checking readiness." >&2
            return 1
        fi
    fi
    [ -n "$expected_instance" ] && [ -n "$expected_version" ] && [ -n "$expected_revision" ] && [ -n "$expected_web_hash" ] || return 1

    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required to verify relay health." >&2
        return 1
    fi

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Health-check attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/readyz" 2>/dev/null)" &&
           printf '%s\n' "$health" | "$binary" verify-readiness "$expected_instance" "$expected_version" "$expected_revision" "$expected_web_hash" 2>/dev/null; then
            printf '%s\n' "$health"
            return 0
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

json_string_field() {
    relay_json_field string "$1" "$2" "${3:-}" || return 0
}

# Proves the served release and packaged web bundle belong to the running
# binary before any invitation is armed or QR printed. The binary pins its own
# version and revision; the served health must report the same values and a
# usable bundle, and an installed release manifest (when present) must agree
# with both. A mutable `current` release or a stale bundle is refused rather
# than paired.
require_release_identity() {
    local health="$1"
    local binary="$2"
    local identity
    local binary_version
    local binary_revision
    local health_version
    local health_revision
    local bundle_hash
    local bundle_version
    local bundle_revision
    local manifest
    local manifest_version
    local manifest_revision
    local manifest_web_hash

    [ -n "$binary" ] && [ -x "$binary" ] || {
        echo "✗ Release identity could not be verified: no usable relay binary was found." >&2
        return 1
    }
    identity="$("$binary" version --json 2>/dev/null)" || {
        echo "✗ Release identity could not be verified: the relay binary did not report its version." >&2
        return 1
    }
    binary_version="$(json_string_field "$identity" version "$binary")"
    binary_revision="$(json_string_field "$identity" revision "$binary")"
    [ -n "$binary_version" ] && [ -n "$binary_revision" ] || {
        echo "✗ Release identity could not be verified: the relay binary reported no version or revision." >&2
        return 1
    }
    health_version="$(json_string_field "$health" version "$binary")"
    health_revision="$(json_string_field "$health" revision "$binary")"
    [ "$health_version" = "$binary_version" ] || {
        echo "✗ Served release version '$health_version' does not match the running binary version '$binary_version'." >&2
        return 1
    }
    [ "$health_revision" = "$binary_revision" ] || {
        echo "✗ Served release revision '$health_revision' does not match the running binary revision '$binary_revision'." >&2
        return 1
    }
    bundle_hash="$(json_string_field "$health" bundle_hash "$binary")"
    [ -n "$bundle_hash" ] || {
        echo "✗ Served release has no managed web bundle; refusing to pair a stale release." >&2
        return 1
    }
    bundle_version="$(json_string_field "$health" bundle_version "$binary")"
    bundle_revision="$(json_string_field "$health" bundle_revision "$binary")"
    [ "$bundle_version" = "$binary_version" ] || {
        echo "✗ Served web bundle version '$bundle_version' does not match the running binary version '$binary_version'." >&2
        return 1
    }
    [ "$bundle_revision" = "$binary_revision" ] || {
        echo "✗ Served web bundle revision '$bundle_revision' does not match the running binary revision '$binary_revision'." >&2
        return 1
    }
    manifest="$(dirname "$binary")/release-manifest.json"
    if [ -f "$manifest" ]; then
        manifest_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        manifest_revision="$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        manifest_web_hash="$(sed -n 's/^[[:space:]]*"web_hash":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        [ "$manifest_version" = "$binary_version" ] || {
            echo "✗ Release manifest version '$manifest_version' does not match the running binary version '$binary_version'." >&2
            return 1
        }
        [ "$manifest_revision" = "$binary_revision" ] || {
            echo "✗ Release manifest revision '$manifest_revision' does not match the running binary revision '$binary_revision'." >&2
            return 1
        }
        [ "$manifest_web_hash" = "$bundle_hash" ] || {
            echo "✗ Release manifest web hash does not match the served bundle web hash." >&2
            return 1
        }
    fi
    return 0
}

# Verify the selected phone-app origin against the release's extracted web
# bundle using system TLS and hostname validation before any relay invitation
# is armed. Suppress verifier diagnostics so operator URLs stay out of logs.
verify_phone_app_bundle() {
    local origin="$1"
    local binary="${2:-}"
    local identity version revision web_root

    [ -n "$binary" ] && [ -x "$binary" ] || binary="$(relay_binary)" || return 1
    identity="$("$binary" version --json 2>/dev/null)" || return 1
    version="$(json_string_field "$identity" version "$binary")"
    revision="$(json_string_field "$identity" revision "$binary")"
    [ -n "$version" ] && [ -n "$revision" ] || return 1
    web_root="${HERDR_WEB_ROOT:-$(dirname "$binary")/web}"
    if ! "$binary" verify-public --web-root "$web_root" --origin "$origin" \
        --version "$version" --revision "$revision" >/dev/null 2>&1; then
        echo "✗ The selected phone-app origin does not serve this release's verified Herdr bundle." >&2
        return 1
    fi
    return 0
}

# Reads the relay's own /healthz gateway object, which reports
# {"enabled":bool,"registered":bool,"relay_id":"...","clients":int}. Only the
# registered flag is echoed; the relay id stays out of terminal output.
gateway_registration_state() {
    local health="$1"
    printf '%s\n' "$health" |
        tr -d ' \t\n' |
        sed -n 's/.*"gateway":{\([^}]*\)}.*/\1/p' |
        sed -n 's/.*"registered":\([a-z]*\).*/\1/p' |
        head -1
}

# Blocks until the relay reports a live gateway registration, so the QR is only
# printed for a relay the phone can actually reach.
wait_for_gateway_registration() {
    local port="${1:-8375}"
    local attempts="${2:-30}"
    local delay="${3:-1}"
    local attempt
    local health

    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required to verify the gateway registration." >&2
        return 1
    fi

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Gateway registration attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null)"; then
            if [ "$(gateway_registration_state "$health")" = "true" ]; then
                return 0
            fi
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

verify_relay_release_health() {
    local health="$1"
    local expected_version="$2"
    local expected_revision="$3"
    local expected_web_hash="$4"

    local expected_instance="${5:-${HERDR_RELAY_INSTANCE_ID:-}}"
    local binary
    binary="$(relay_binary)" || return 1
    printf '%s\n' "$health" | "$binary" verify-readiness "$expected_instance" "$expected_version" "$expected_revision" "$expected_web_hash"
}

wait_for_relay_release_health() {
    local port="$1"
    local attempts="$2"
    local delay="$3"
    local expected_version="$4"
    local expected_revision="$5"
    local expected_web_hash="$6"
    local expected_instance="${7:-${HERDR_RELAY_INSTANCE_ID:-}}"
    local health
    local attempt

    [ -n "$expected_version" ] &&
        [ -n "$expected_revision" ] &&
        [ -n "$expected_web_hash" ] || {
            echo "Exact release health verification requires version, revision, and web hash." >&2
            return 1
        }

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Health-check attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(wait_for_relay_health "$port" 1 0 "$expected_instance" "$expected_version" "$expected_revision" "$expected_web_hash")"; then
            printf '%s\n' "$health"
            return 0
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

report_inventory_failure() {
    local health
    echo "Herdr agent inventory is unavailable or the installed relay identity does not match." >&2
    health="$(curl -fsS --max-time 2 "http://127.0.0.1:$1/healthz" 2>/dev/null)" || return 0
    # The relay reports inventory.error_code nested, which the strict top-level
    # accessor cannot read. This advisory hint changes nothing, so after the
    # strict decoder confirms a relay health document it matches the exact
    # nested code token instead.
    [ "$(json_string_field "$health" status)" = ok ] || return 0
    case "$health" in
        *'"error_code":"protocol_mismatch"'*|*'"error_code": "protocol_mismatch"'*)
            echo "Run: herdr server live-handoff" >&2
            ;;
    esac
    return 0
}

verify_public_readiness() {
    local env_file="$1" health="$2"
    local config hostname public attempt binary transport
    # Only the Cloudflare transport publishes through CLOUDFLARED_CONFIG. A
    # retained tunnel config must not gate another transport; CLI-backed Serve
    # callers verify their exact journaled route separately.
    transport="$(relay_transport_mode "$env_file")" || return 1
    [ "$transport" = cloudflare ] || return 0
    [ -z "$(gateway_urls "$env_file")" ] || return 0
    config="$(env_file_value "$env_file" CLOUDFLARED_CONFIG)"
    [ -n "$config" ] || return 0
    [ -f "$config" ] || return 1
    hostname="$(yaml_scalar hostname "$config")"
    valid_hostname "$hostname" || return 1
    binary="$(relay_binary)" || return 1
    for ((attempt = 1; attempt <= 15; attempt++)); do
        if public="$(curl -fsS --proto '=https' --max-time 5 "https://$hostname/readyz" 2>/dev/null)" &&
           printf '%s\n' "$public" | "$binary" verify-readiness \
               "$(json_string_field "$health" instance)" "$(json_string_field "$health" release_version)" \
               "$(json_string_field "$health" revision)" "$(json_string_field "$health" bundle_hash)"; then
            return 0
        fi
        [ "$attempt" -eq 15 ] || sleep 1
    done
    echo "Public health identity did not match the intended relay, or the public endpoint is unavailable; local service readiness is separate." >&2
    return 1
}

host_label() {
    hostname -s 2>/dev/null || hostname 2>/dev/null || echo relay
}

tailscale_https_port() {
    local port="${HERDR_TAILSCALE_HTTPS_PORT:-443}"
    case "$port" in
        ''|*[!0-9]*)
            echo "✗ HERDR_TAILSCALE_HTTPS_PORT must be a positive TCP port." >&2
            return 1
            ;;
    esac
    [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || {
        echo "✗ HERDR_TAILSCALE_HTTPS_PORT must be between 1 and 65535." >&2
        return 1
    }
    printf '%s\n' "$port"
}

# The token passes through argv only for the short-lived compiled helper.
build_setup_fragment() {
    "$(relay_binary)" setup-fragment "$1" "$2" "${3:-}"
}

# The configured blind gateway base URLs as a comma-separated candidate list —
# the same shape HERDR_GATEWAY_URL takes. The relay measures healthy candidates
# concurrently; configured order breaks close ties and is the fallback when no
# probe succeeds. Empty means the relay keeps using a Cloudflare tunnel.
gateway_urls() {
    local env_file="${1:-${HERDR_RELAY_ENV:-}}"
    local raw="${HERDR_GATEWAY_URL:-}"
    local old_ifs
    local list=""
    local entry

    if [ -z "$raw" ] && [ -n "$env_file" ]; then
        raw="$(env_file_value "$env_file" HERDR_GATEWAY_URL)"
    fi
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $raw
    IFS="$old_ifs"
    for entry in "$@"; do
        # A URL never contains whitespace, so dropping all of it is the same as
        # trimming what a hand-edited env file leaves around a comma.
        entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
        entry="${entry%/}"
        if [ -n "$entry" ]; then
            list="${list:+$list,}$entry"
        fi
    done
    printf '%s\n' "$list"
}

# The configured tie-break leader used by setup and service scripts. The live
# relay advertises its selected entry separately through status.
gateway_url() {
    local list

    list="$(gateway_urls "${1:-}")"
    printf '%s\n' "${list%%,*}"
}

# The gateway candidates this project operates for the community. They are
# compiled in so the free shared path costs a user no hostname, no account, and
# no typing: picking it in the chooser is the whole setup. The value has the
# same comma-separated shape as HERDR_GATEWAY_URL. An operator points elsewhere
# with HERDR_COMMUNITY_GATEWAY_URL; an explicitly empty value means "no
# community gateway", which is how a test or fork switches the option off.
HERDR_COMMUNITY_GATEWAY_DEFAULT="wss://gw1.herdr-mobile.dev,wss://gw2.herdr-mobile.dev"

community_gateway_url() {
    if [ "${HERDR_COMMUNITY_GATEWAY_URL+set}" = "set" ]; then
        printf '%s\n' "$HERDR_COMMUNITY_GATEWAY_URL"
        return
    fi
    printf '%s\n' "$HERDR_COMMUNITY_GATEWAY_DEFAULT"
}

# Canonicalizes anything a person might reasonably type — gw.example.com,
# https://gw.example.com, wss://gw.example.com — into the wss:// base URL the
# relay and the QR fragment use. It reuses the compiled origin normalizer, so a
# gateway URL is held to the same rules as the phone app origin: no
# credentials, no path, no query, no fragment.
normalize_gateway_url() {
    local input="$1"
    local origin

    case "$input" in
        wss://*) input="https://${input#wss://}" ;;
        ws://*) input="http://${input#ws://}" ;;
    esac
    if ! origin="$("$(relay_binary)" normalize-origin --allow-loopback-http "$input")"; then
        return 1
    fi
    case "$origin" in
        https://*) printf 'wss://%s\n' "${origin#https://}" ;;
        http://*) printf 'ws://%s\n' "${origin#http://}" ;;
        *) return 1 ;;
    esac
}

# Canonicalizes and deduplicates a comma-separated candidate list. One invalid
# entry rejects the choice: silently dropping a typo would make the advertised
# disaster-recovery path look configured when it is not.
normalize_gateway_urls() {
    local raw="$1"
    local old_ifs
    local entry
    local normalized
    local list=""

    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $raw
    IFS="$old_ifs"
    for entry in "$@"; do
        entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
        if ! normalized="$(normalize_gateway_url "$entry")"; then
            return 1
        fi
        case ",$list," in
            *,"$normalized",*) ;;
            *) list="${list:+$list,}$normalized" ;;
        esac
    done
    [ -n "$list" ] || return 1
    printf '%s\n' "$list"
}

# Builds the editable default shown after an operator supplies or deploys their
# own gateway. Their entries stay first; the project gateways are cold
# fallbacks, and normalization removes a community entry that is already theirs.
gateway_subscription_defaults() {
    local own="$1"
    local community

    community="$(community_gateway_url)"
    normalize_gateway_urls "$own${community:+,$community}"
}

# Every own-gateway path ends here. Showing the complete candidate list makes
# the fallback policy an explicit operator choice instead of hiding it behind a
# yes/no prompt or requiring prior knowledge of the community hostnames.
prompt_gateway_subscriptions() {
    local defaults
    local entered
    local normalized

    if ! defaults="$(normalize_gateway_urls "$1")"; then
        return 1
    fi
    while true; do
        if [ "${HERDR_GATEWAY_SUBSCRIPTIONS+set}" = "set" ]; then
            entered="$HERDR_GATEWAY_SUBSCRIPTIONS"
        else
            echo "" >&2
            echo "Gateways this relay and phone may use, in priority order:" >&2
            printf '%s\n' "$defaults" | tr ',' '\n' | sed 's/^/  /' >&2
            read -r -p "Gateways to subscribe to, comma-separated [keep this list]: " entered ||
                entered=""
        fi
        entered="${entered:-$defaults}"
        if normalized="$(normalize_gateway_urls "$entered")"; then
            printf '%s\n' "$normalized"
            return 0
        fi
        echo "✗ Enter one or more gateway hostnames or ws:// / wss:// origins." >&2
        [ "${HERDR_GATEWAY_SUBSCRIPTIONS+set}" != "set" ] || return 1
    done
}

# The HTTPS base for the gateway's own endpoints: /healthz, /probe, /whoami.
gateway_http_base() {
    case "$1" in
        wss://*) printf 'https://%s\n' "${1#wss://}" ;;
        ws://*) printf 'http://%s\n' "${1#ws://}" ;;
        *) printf '%s\n' "$1" ;;
    esac
}

# Confirms a gateway is actually answering before its URL is written into the
# relay environment, so a typo or a dead host fails during setup rather than at
# the first phone connection.
gateway_answers_healthz() {
    local body

    if ! body="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            "$(gateway_http_base "$1")/healthz" 2>/dev/null
    )"; then
        return 1
    fi
    printf '%s\n' "$body" | grep -Eq '"ok"[[:space:]]*:[[:space:]]*true'
}

# Round-trip milliseconds for a gateway's /healthz, or failure when it does not
# answer. Setup already opens this connection to prove the gateway is alive;
# reporting what it measured is what lets a person order the list by distance
# instead of guessing from hostnames. The body goes to a file so stdout carries
# only the timing: a curl that reports no timing still proves health, and the
# caller simply omits the number.
gateway_healthz_ms() {
    local body_file
    local seconds

    body_file="$(mktemp "${TMPDIR:-/tmp}/herdr-healthz.XXXXXX")" || return 1
    if ! seconds="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            --output "$body_file" \
            --write-out '%{time_total}' \
            "$(gateway_http_base "$1")/healthz" 2>/dev/null
    )"; then
        rm -f "$body_file"
        return 1
    fi
    if ! grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' "$body_file"; then
        rm -f "$body_file"
        return 1
    fi
    rm -f "$body_file"
    case "$seconds" in
        '' | *[!0-9.]*) return 0 ;;
    esac
    awk -v seconds="$seconds" 'BEGIN { printf "%d\n", (seconds * 1000) + 0.5 }'
}

# How a saved list reads in one phrase, for the setup menu's status line. The
# policy decides which gateway carries traffic, so it belongs next to the
# gateway it chose rather than only in the environment file.
gateway_selection_label() {
    case "$1" in
        latency) printf 'closest wins\n' ;;
        *) printf 'first listed wins\n' ;;
    esac
}

# The policy a prompt answer selects. Split from the prompt so the rule is
# testable without a terminal: "1" or empty keeps the default the list was
# built with, "2" takes the other one, and anything else keeps the default
# rather than silently changing which gateway carries traffic.
gateway_selection_choice() {
    local default_selection="$1"
    local answer="$2"

    if [ "$answer" = 2 ]; then
        if [ "$default_selection" = latency ]; then
            printf 'ordered\n'
        else
            printf 'latency\n'
        fi
        return 0
    fi
    printf '%s\n' "$default_selection"
}

# Asks how the relay should read the list it just saved. The default is the one
# the chosen action implies — an operator's own gateway is a priority, a pool of
# interchangeable public ones is ranked by distance — so Enter is always the
# right answer for someone who does not care.
#
# The terminal is the only guard: HERDR_GATEWAY_SELECTION cannot serve as an
# "automation is driving" signal, because the setup menu loads relay.env into
# the environment before running an action. Reading it here would let a saved
# policy silently answer the question and, worse, carry an old own-gateway
# `ordered` into a freshly chosen community list.
prompt_gateway_selection() {
    local default_selection="$1"
    local answer

    if [ ! -t 0 ]; then
        printf '%s\n' "$default_selection"
        return 0
    fi
    echo "" >&2
    echo "How should the relay pick from this list?" >&2
    if [ "$default_selection" = latency ]; then
        echo "  1. Closest by measured latency [default]" >&2
        echo "  2. First one that answers, in the order above" >&2
    else
        echo "  1. First one that answers, in the order above [default]" >&2
        echo "  2. Closest by measured latency" >&2
    fi
    read -r -p "Choice [1]: " answer || answer=""
    gateway_selection_choice "$default_selection" "$answer"
}

# Persists the transport choice. An empty URL removes the key, which returns
# the relay to the Cloudflare tunnel path — and takes the selection policy with
# it, so a relay that leaves the gateway path keeps no stale policy to revive
# behind a later, differently chosen list.
set_gateway_url() {
    local env_file="$1"
    local url="$2"

    if [ -e "$(tailscale_session_file "$env_file")" ] || [ -e "$(tailscale_external_session_file "$env_file")" ]; then
        echo "✗ Cannot change gateway selection while a foreground Tailscale Serve relay is recorded." >&2
        echo "  Stop that pane before changing transport." >&2
        return 1
    fi
    if [ -z "$url" ]; then
        set_relay_transport "$env_file" cloudflare || return
        return 0
    fi
    set_relay_transport "$env_file" gateway
    set_env_value_atomic "$env_file" HERDR_GATEWAY_URL "$url"
}

# Records how the relay picks among the configured candidates. "ordered" keeps
# the configured order, so an operator who lists their own gateway first keeps
# it even when a community one answers faster; "latency" ranks by measured RTT,
# which is the point for the interchangeable public candidates. The relay
# defaults to "ordered" when the key is absent, so only the community path has
# to write anything. Any other value is a caller bug that would quietly change
# which gateway carries traffic, so nothing is written.
set_gateway_selection() {
    local env_file="$1"
    local selection="$2"

    case "$selection" in
        ordered|latency) ;;
        *) return 1 ;;
    esac
    set_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION "$selection"
}

# Percent-encodes one fragment value with the compiled encoder, so an entry of
# the gateway list is escaped exactly like the keys the phone already parses.
# Values escape '=' and '&', so the only literal "relay=" in the helper's output
# is the key whose value is being extracted.
encode_fragment_value() {
    build_setup_fragment "" "" "$1" | sed -e 's/.*relay=//' -e 's/&.*//'
}

# Builds a setup fragment for either transport. A gateway-configured relay has
# no direct relay URL; its complete ordered candidate list travels only in
# `gateways=`. Each entry is encoded separately so the commas remain separators,
# while the relay token stays inside the URL fragment.
build_transport_setup_fragment() {
    local token="$1"
    local label="$2"
    local relay_url="${3:-}"
    local gateways
    local fragment
    local encoded=""
    local old_ifs
    local entry

    gateways="$(gateway_urls)"
    if [ -z "$gateways" ]; then
        build_setup_fragment "$token" "$label" "$relay_url"
        return
    fi
    fragment="$(build_setup_fragment "$token" "$label")"
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $gateways
    IFS="$old_ifs"
    for entry in "$@"; do
        encoded="${encoded:+$encoded,}$(encode_fragment_value "$entry")"
    done
    printf '%s&gateways=%s\n' "$fragment" "$encoded"
}

phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="${2:-${HERDR_RELAY_ENV:-}}"
    local app_url="${HERDR_PHONE_APP_URL:-${HERDR_APP_DEPLOY_ORIGIN:-}}"
    local normalized
    local configured_origin
    local observed_origin

    if [ -z "$app_url" ] && [ -n "$env_file" ]; then
        configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
        observed_origin="$(dirname "$env_file")/phone-app-origin"
        if [ -r "$configured_origin" ]; then
            app_url="$(head -1 "$configured_origin")"
        elif [ -r "$observed_origin" ]; then
            app_url="$(head -1 "$observed_origin")"
        fi
    fi
    if [ -z "$app_url" ] || [ "$app_url" = "relay" ]; then
        app_url="$relay_fallback"
    fi
    if ! normalized="$("$(relay_binary)" normalize-origin --allow-loopback-http "$app_url")"; then
        echo "✗ Enter a domain or HTTPS URL without a path, such as app.example.com." >&2
        return 1
    fi
    printf '%s\n' "$normalized"
}

phone_app_origin_serves_herdr() {
    local origin="$1"
    local manifest

    if ! manifest="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            "$origin/manifest.webmanifest" 2>/dev/null
    )"; then
        return 1
    fi
    printf '%s\n' "$manifest" \
        | grep -Eq '"name"[[:space:]]*:[[:space:]]*"Herdr Mobile Relay"'
}

# A separately hosted app uses herdr.<authorized-zone> by convention. Probe it
# before defaulting a new computer to its relay-served copy: browser storage and
# PWA identity are origin-scoped, so every relay must open the same app origin.
discover_cloudflare_phone_app_origin() {
    local origin_cert="${TUNNEL_ORIGIN_CERT:-$HOME/.cloudflared/cert.pem}"
    local zone
    local candidate

    [ -r "$origin_cert" ] || return 1
    zone="$(cloudflare_cert_zone_name "$origin_cert" 2>/dev/null)" || return 1
    [ -n "$zone" ] || return 1
    candidate="https://herdr.$zone"
    phone_app_origin_serves_herdr "$candidate" || return 1
    printf '%s\n' "$candidate"
}

# Asks for the origin of an already-installed Herdr app and validates it with
# the same normalizer the setup link uses. Shared by the tunnel chooser below
# and the gateway path, which has no relay-served origin of its own.
prompt_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local confirmation
    local entered_url
    local normalized

    while true; do
        if ! read -r -p "Installed app domain or URL, or q to cancel: " entered_url; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        case "$entered_url" in
            q | Q)
                echo "Setup cancelled." >&2
                return 1
                ;;
        esac
        if [ -z "$entered_url" ]; then
            echo "✗ Enter the domain shown in the installed app's Site settings," >&2
            echo "  for example app.example.com, or q to cancel." >&2
            continue
        fi
        if ! normalized="$(
            HERDR_PHONE_APP_URL="$entered_url" \
                phone_app_base_url "$relay_fallback" "$env_file"
        )"; then
            continue
        fi
        if ! phone_app_origin_serves_herdr "$normalized"; then
            echo "✗ No Herdr app was found at $normalized." >&2
            echo "  Enter the exact domain shown in the installed app's Site settings." >&2
            if ! read -r -p "Use this address anyway? [y/N]: " confirmation; then
                echo "" >&2
                echo "Setup cancelled." >&2
                return 1
            fi
            case "$confirmation" in
                y|Y|yes|YES|Yes)
                    ;;
                *)
                    continue
                    ;;
            esac
        fi
        printf '%s\n' "$normalized"
        return 0
    done
}

# The phone app origin for a gateway-configured relay. The gateway only copies
# encrypted frames, so it serves no app and there is no relay hostname to fall
# back to: the origin has to be recorded, configured, or entered.
gateway_phone_app_base_url() {
    local env_file="$1"
    local base

    if base="$(phone_app_base_url "" "$env_file" 2>/dev/null)" && [ -n "$base" ]; then
        printf '%s\n' "$base"
        return 0
    fi
    if [ -n "${HERDR_PHONE_APP_URL:-}" ] || [ ! -t 0 ]; then
        echo "✗ Set HERDR_PHONE_APP_URL to the HTTPS origin that serves the Herdr phone app." >&2
        echo "  The gateway carries relay traffic only; it does not host the app." >&2
        return 1
    fi
    echo "The gateway carries relay traffic only, so the phone app needs its own" >&2
    echo "HTTPS origin. Enter an installed Herdr app, or host one with make web-deploy." >&2
    echo "" >&2
    prompt_phone_app_base_url "" "$env_file"
}

phone_app_choice_action() {
    local current_origin="$1"
    local choice="${2:-1}"

    if [ -n "$current_origin" ]; then
        case "$choice" in
            1) printf 'keep\n' ;;
            2) printf 'relay\n' ;;
            3) printf 'existing\n' ;;
            q | Q) printf 'cancel\n' ;;
            *) printf 'invalid\n' ;;
        esac
        return
    fi
    case "$choice" in
        1) printf 'relay\n' ;;
        2) printf 'existing\n' ;;
        q | Q) printf 'cancel\n' ;;
        *) printf 'invalid\n' ;;
    esac
}

choose_tailscale_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local action
    local choice
    local current_origin=""
    local configured_origin
    local observed_origin

    configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
    observed_origin="$(dirname "$env_file")/phone-app-origin"

    # An explicit environment value is an operator request, so it wins over a
    # saved origin and is still normalized by the compiled URL/TLS contract.
    if [ -n "${HERDR_PHONE_APP_URL:-}" ]; then
        phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi
    if [ -n "${HERDR_APP_DEPLOY_ORIGIN:-}" ]; then
        HERDR_PHONE_APP_URL="$HERDR_APP_DEPLOY_ORIGIN" \
            phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi

    # A prior choice is shared app state, not a reason to discover a new
    # Cloudflare origin. Keep it unless the operator explicitly switches it.
    if [ -s "$configured_origin" ] || [ -s "$observed_origin" ]; then
        if ! current_origin="$(phone_app_base_url "$relay_fallback" "$env_file")"; then
            if ! stdin_is_terminal; then
                echo "✗ The saved phone app address is invalid." >&2
                return 1
            fi
            echo "  The saved phone app address is invalid; choose a replacement." >&2
            echo "" >&2
        fi
    fi

    # The foreground Tailscale caller has already selected an HTTPS relay
    # origin. Its packaged frontend is the safe default, but every app origin
    # is independently checked against this release before an invite is armed.
    if ! stdin_is_terminal; then
        if [ -n "$current_origin" ]; then
            printf '%s\n' "$current_origin"
        else
            phone_app_base_url "$relay_fallback" "$env_file"
        fi
        return
    fi

    echo "Where should the phone setup link open?" >&2
    echo "" >&2
    if [ -n "$current_origin" ]; then
        echo "  Current phone app: $current_origin" >&2
        echo "" >&2
        menu_item 1 "Keep current phone app (recommended)" >&2
        echo "     Reuse this shared app origin for the Tailscale relay." >&2
        echo "" >&2
        menu_item 2 "Use this Serve relay instead" >&2
        echo "     Use its verified HTTPS origin and packaged Herdr app." >&2
        echo "" >&2
        menu_item 3 "Use another installed Herdr app" >&2
    else
        menu_item 1 "This Serve relay (recommended)" >&2
        echo "     Use its verified HTTPS origin and packaged Herdr app." >&2
        echo "" >&2
        menu_item 2 "An existing installed Herdr app" >&2
    fi
    echo "     The shared app origin can be changed explicitly later." >&2
    echo "" >&2
    menu_item q "Cancel, change nothing" >&2
    echo "" >&2
    while true; do
        read -r -p "Choice [1]: " choice || choice="cancel"
        if [ "$choice" = "cancel" ]; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        action="$(phone_app_choice_action "$current_origin" "$choice")"
        case "$action" in
            keep)
                printf '%s\n' "$current_origin"
                return
                ;;
            relay)
                HERDR_PHONE_APP_URL=relay phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            existing)
                prompt_phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            cancel)
                echo "Setup cancelled." >&2
                return 1
                ;;
            invalid)
                if [ -n "$current_origin" ]; then
                    echo "✗ Choose 1, 2, 3, or q." >&2
                else
                    echo "✗ Choose 1, 2, or q." >&2
                fi
                ;;
        esac
    done
}

choose_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local setup_kind="${3:-stable}"
    local action
    local choice
    local current_origin=""
    local discovered_origin=""
    local configured_origin
    local observed_origin

    configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
    observed_origin="$(dirname "$env_file")/phone-app-origin"
    if [ "$setup_kind" = tailscale ] || [ "$setup_kind" = tailscale-external ]; then
        choose_tailscale_phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi
    if [ -z "${HERDR_PHONE_APP_URL:-}" ] && [ ! -s "$configured_origin" ]; then
        if [ -n "${HERDR_APP_DEPLOY_ORIGIN:-}" ]; then
            discovered_origin="$(
                HERDR_PHONE_APP_URL="$HERDR_APP_DEPLOY_ORIGIN" \
                    phone_app_base_url "$relay_fallback" "$env_file"
            )"
        else
            discovered_origin="$(discover_cloudflare_phone_app_origin || true)"
        fi
    fi
    if [ -n "${HERDR_PHONE_APP_URL:-}" ] || [ ! -t 0 ]; then
        if [ -n "$discovered_origin" ]; then
            printf '%s\n' "$discovered_origin"
        else
            phone_app_base_url "$relay_fallback" "$env_file"
        fi
        return
    fi

    echo "Where should the phone setup link open?" >&2
    echo "" >&2
    if [ -s "$configured_origin" ]; then
        if ! current_origin="$(phone_app_base_url "$relay_fallback" "$env_file")"; then
            echo "  The saved phone app address is invalid; choose a replacement." >&2
            echo "" >&2
        fi
    elif [ -n "$discovered_origin" ]; then
        current_origin="$discovered_origin"
        echo "  Found an existing Herdr app in this Cloudflare zone." >&2
    elif [ -s "$observed_origin" ]; then
        current_origin="$(phone_app_base_url "$relay_fallback" "$env_file" || true)"
        [ -z "$current_origin" ] ||
            echo "  Last connected phone app: $current_origin" >&2
    fi
    if [ -n "$current_origin" ]; then
        echo "  Current phone app: $current_origin" >&2
        echo "" >&2
        menu_item 1 "Keep current phone app (recommended)" >&2
        echo "     $current_origin" >&2
        echo "" >&2
        if [ "$setup_kind" = "temporary" ]; then
            menu_item 2 "Use this temporary relay instead" >&2
            echo "     Opens the TryCloudflare app. Its address changes after restart." >&2
        else
            menu_item 2 "Use this relay instead" >&2
            echo "     Switches the app origin to this computer's relay hostname." >&2
        fi
        echo "" >&2
        menu_item 3 "Use another installed Herdr app" >&2
    else
        if [ "$setup_kind" = "temporary" ]; then
            menu_item 1 "This temporary relay (recommended for trying one relay)" >&2
            echo "     Opens the TryCloudflare app. Its address changes after restart." >&2
        else
            menu_item 1 "This relay (recommended for one relay)" >&2
            echo "     Uses this computer's verified hostname as the installed app." >&2
        fi
        echo "" >&2
        menu_item 2 "An existing installed Herdr app" >&2
    fi
    echo "     Adds this computer to the same app as your other relays." >&2
    echo "" >&2
    menu_item q "Cancel, change nothing" >&2
    echo "" >&2
    while true; do
        read -r -p "Choice [1]: " choice || choice="cancel"
        if [ "$choice" = "cancel" ]; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        action="$(phone_app_choice_action "$current_origin" "$choice")"
        case "$action" in
            keep)
                printf '%s\n' "$current_origin"
                return
                ;;
            relay)
                HERDR_PHONE_APP_URL=relay phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            existing)
                prompt_phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            cancel)
                echo "Setup cancelled." >&2
                return 1
                ;;
            invalid)
                if [ -n "$current_origin" ]; then
                    echo "✗ Choose 1, 2, 3, or q." >&2
                else
                    echo "✗ Choose 1, 2, or q." >&2
                fi
                ;;
        esac
    done
}

record_phone_app_origin() {
    local origin="$1"
    local env_file="$2"
    local target
    local temporary

    if [ -z "$env_file" ]; then
        echo "✗ Cannot record the phone app origin without a relay environment path." >&2
        return 1
    fi
    target="$(dirname "$env_file")/phone-app-origin-configured"
    temporary="$target.tmp.$$"
    (
        umask 077
        trap 'rm -f "$temporary"' EXIT
        printf '%s\n' "$origin" > "$temporary"
        chmod 600 "$temporary"
        mv "$temporary" "$target"
        trap - EXIT
    )
}

# OSC 8 is a terminal protocol, not a vendor feature. Use it for any interactive
# terminal unless the user or terminal explicitly asks for plain output; modern
# terminals understand it and older ones safely ignore it.
stdout_is_terminal() {
    [ -t 1 ]
}

stdin_is_terminal() {
    [ -t 0 ]
}

terminal_hyperlinks_enabled() {
    stdout_is_terminal &&
        [ -z "${NO_COLOR+x}" ] &&
        [ "${TERM:-}" != dumb ]
}

# Setup URLs enter both the QR encoder and an OSC 8 sequence. Raw whitespace is
# not valid in a URL, and control bytes could terminate the terminal sequence.
# The compiled origin parser enforces HTTPS, with canonical loopback HTTP only.
phone_setup_url_is_safe() {
    local url="$1"
    local origin
    local normalized

    case "$url" in
        *[[:cntrl:]]* | *[[:space:]]*) return 1 ;;
    esac
    if [[ ! "$url" =~ ^(https?://[^/?#]+) ]]; then
        return 1
    fi
    origin="${BASH_REMATCH[1]}"
    if ! normalized="$("$(relay_binary)" normalize-origin \
        --allow-loopback-http "$origin" 2>/dev/null)"; then
        return 1
    fi
    case "$origin" in
        https://*) return 0 ;;
        *) [ "$normalized" = "$origin" ] ;;
    esac
}

# Prints an indented terminal QR code for the URL, or nothing when it cannot
# be drawn because the terminal is too narrow. A wrapped QR is worse than the
# plain link.
# Callers must keep working with empty output. Kept separate from
# build_setup_fragment on purpose: this call is allowed to fail, that one
# is not.
render_setup_qr() {
    local url="$1"
    local cols
    cols="$(tput cols 2>/dev/null || true)"
    "$(relay_binary)" qr --columns "${cols:-80}" "$url" 2>/dev/null || true
}

# The URL has already been validated before this internal emitter is called.
emit_phone_setup_url() {
    local phone_url="$1"

    if terminal_hyperlinks_enabled; then
        printf '  \033]8;;%s\033\\%s\033]8;;\033\\\n' "$phone_url" "$phone_url"
    else
        printf '  %s\n' "$phone_url"
    fi
}

print_phone_setup_url() {
    phone_setup_url_is_safe "$1" || return 1
    emit_phone_setup_url "$1"
}

# Shared tail of quick-start and setup-link output: QR code when possible,
# always the link. Invalid values fail before either output sink sees them.
print_phone_setup() {
    local phone_url="$1"
    local qr_code

    phone_setup_url_is_safe "$phone_url" || return 1
    qr_code="$(render_setup_qr "$phone_url")"
    if [ -n "$qr_code" ]; then
        echo "  Scan this QR code with your phone camera:"
        echo ""
        printf '%s\n' "$qr_code"
        echo ""
        echo "  This code contains your relay token; do not share screenshots of it."
        echo ""
        echo "  Or open this private setup link on your phone:"
    else
        echo "  Open this private setup link on your phone:"
    fi
    emit_phone_setup_url "$phone_url"
}

# The bootstrap invitation in a setup link is one-use: the first phone that
# pairs consumes it. Printing the link is the operator asking for one more
# pairing, so the running relay is told to arm a fresh invitation before the
# link is shown. The relay records its pid beside relay.env and re-arms on
# SIGUSR1. Returns 1 when no relay is running here.
arm_setup_link() {
    local env_file="$1"
    local pid

    pid="$(head -1 "$(dirname "$env_file")/relay.pid" 2>/dev/null || true)"
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    kill -0 "$pid" 2>/dev/null || return 1
    kill -USR1 "$pid" 2>/dev/null || return 1
    sleep 0.3
}

print_setup_link_arming() {
    if [ "$1" -eq 0 ]; then
        echo "  Keep the setup link private. Expired bootstrap links renew on retry before first pairing;" \
            "additional-device invitations expire 10 minutes after issue. Print it again for another."
    else
        echo "  The relay is not running here; start it, then print the link again."
    fi
}

require_supported_platform() {
    case "$(uname -s)" in
        Darwin|Linux)
            return
            ;;
        *)
            echo "Unsupported platform: Herdr Mobile Relay currently supports only Linux and macOS."
            exit 1
            ;;
    esac
}

# Acquire the canonical root's owner (O) for this script's lifetime. On success
# HOLDER_PID holds the managed-state holder and the owner lock is held. On any
# refusal the function returns non-zero with a message on stderr and leaves the
# filesystem untouched.
managed_owner_acquire() {
    local dir="$1"
    local attempt
    HOLDER_LOG="$(mktemp "${TMPDIR:-/tmp}/herdr-managed-owner.XXXXXX")"
    "$(relay_binary)" managed-state hold --dir "$dir" --operation owner >"$HOLDER_LOG" 2>&1 &
    HOLDER_PID=$!
    for ((attempt = 1; attempt <= 50; attempt++)); do
        if grep -q '"ok":true' "$HOLDER_LOG" 2>/dev/null; then
            return 0
        fi
        if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
            wait "$HOLDER_PID" 2>/dev/null || true
            echo "✗ The relay configuration is in use by another operation: $dir" >&2
            sed -n '1,10p' "$HOLDER_LOG" >&2
            HOLDER_PID=""
            rm -f "$HOLDER_LOG"
            HOLDER_LOG=""
            return 1
        fi
        sleep 0.1
    done
    kill -TERM "$HOLDER_PID" 2>/dev/null || true
    wait "$HOLDER_PID" 2>/dev/null || true
    echo "✗ Timed out acquiring the relay configuration lock: $dir" >&2
    HOLDER_PID=""
    rm -f "$HOLDER_LOG"
    HOLDER_LOG=""
    return 1
}

# Release the holder cleanly. Safe to call when no holder is active and from an
# EXIT trap; the holder retires the owner lock itself.
managed_owner_release() {
    [ -n "${HOLDER_PID:-}" ] || return 0
    kill -TERM "$HOLDER_PID" 2>/dev/null || true
    wait "$HOLDER_PID" 2>/dev/null || true
    HOLDER_PID=""
    if [ -n "${HOLDER_LOG:-}" ]; then
        rm -f "$HOLDER_LOG"
        HOLDER_LOG=""
    fi
    return 0
}

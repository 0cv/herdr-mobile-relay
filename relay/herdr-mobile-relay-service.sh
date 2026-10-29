#!/usr/bin/env bash
set +x
set -euo pipefail
# Only the generated launchd argv selects this adapter. Never infer it from
# platform, environment, configuration, or a completed supervisor's exit code.
SERVICE_LAUNCHD=false
case "$#" in
    0) ;;
    1) [ "$1" = --launchd ] || { echo 'Invalid installed-service arguments.' >&2; exit 2; }; SERVICE_LAUNCHD=true ;;
    *) echo 'Invalid installed-service arguments.' >&2; exit 2 ;;
esac
readonly SERVICE_LAUNCHD

# Called ONLY at explicit pre-child admission/refusal sites, before T001 traps.
# Adapter zero means terminal/non-ready, never a ready relay or installation.
service_refuse() {
    case "$1" in
        69) echo 'Installed-service CLI startup is disabled: unavailable/non-ready; no service was started.' >&2 ;;
        78) echo 'Installed service configuration/policy refusal: unavailable/non-ready; no service was started.' >&2 ;;
        *) exit "$1" ;;
    esac
    if [ "$SERVICE_LAUNCHD" = true ]; then
        echo 'launchd pre-child refusal: unavailable/non-ready; automatic retry suppressed.' >&2
        exit 0
    fi
    exit "$1"
}
service_trim() {
    SERVICE_VALUE="$1"
    while [[ "$SERVICE_VALUE" == [[:space:]]* ]]; do SERVICE_VALUE="${SERVICE_VALUE:1}"; done
    while [[ "$SERVICE_VALUE" == *[[:space:]] ]]; do SERVICE_VALUE="${SERVICE_VALUE%?}"; done
}
service_mode() {
    service_trim "$1"
    SERVICE_VALUE="$(printf '%s' "$SERVICE_VALUE" | LC_ALL=C tr '[:upper:]' '[:lower:]')" || return 1
    case "$SERVICE_VALUE" in ''|cloudflare|gateway|tailscale-cli|tailscale|tailscale-external) return 0 ;; *) return 1 ;; esac
}
service_policy() {
    case "$1" in
        HERDR_RELAY_RUN_ID|HERDR_RELAY_CONTROL_RUN_ID|HERDR_RELAY_PAIRING_SOCKET) [ -z "$2" ] ;;
        HERDR_RELAY_REARM_BOOTSTRAP|HERDR_TAILSCALE_REQUEST)
            case "$2" in ''|0|false|False|FALSE|f|F) return 0 ;; *) return 1 ;; esac ;;
        HERDR_GATEWAY_SELECTION)
            service_trim "$2"
            SERVICE_VALUE="$(printf '%s' "$SERVICE_VALUE" | LC_ALL=C tr '[:upper:]' '[:lower:]')" || return 1
            case "$SERVICE_VALUE" in ''|ordered|latency) return 0 ;; *) return 1 ;; esac ;;
    esac
}

RELAY_UPDATER_TOKEN_FILE=${HERDR_GITHUB_TOKEN_FILE:-}
export -n RELAY_UPDATER_TOKEN_FILE SERVICE_VALUE SERVICE_GATEWAYS
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR" 2>/dev/null)" || service_refuse 78
case "$ENV_FILE" in *[$'\001'-$'\037'$'\177']*) service_refuse 78 ;; esac
literal_env_scan "$ENV_FILE" 2>/dev/null || service_refuse 78
# The native literal configuration namespaces, not shell/internal routing or
# positional-argument syntax. No source/eval, including after admission.
for ((SERVICE_INDEX=0; SERVICE_INDEX<${#LITERAL_KEYS[@]}; SERVICE_INDEX++)); do
    SERVICE_KEY="${LITERAL_KEYS[SERVICE_INDEX]}"
    case "$SERVICE_KEY" in
        HERDR_RELAY_ENV) [ "${LITERAL_VALUES[SERVICE_INDEX]}" = "$ENV_FILE" ] || service_refuse 78 ;;
        HERDR_PLUGIN_CONFIG_DIR) [ "${LITERAL_VALUES[SERVICE_INDEX]}" = "$(dirname "$ENV_FILE")" ] || service_refuse 78 ;;
        HERDR_*|CLOUDFLARED_*|TUNNEL_ORIGIN_CERT|GH_TOKEN|GITHUB_TOKEN|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|NO_PROXY|http_proxy|https_proxy|all_proxy|no_proxy) ;;
        *) service_refuse 78 ;;
    esac
done
# Saved empties must not erase an applicable ambient ownership/policy refusal.
for SERVICE_KEY in HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_REARM_BOOTSTRAP HERDR_TAILSCALE_REQUEST HERDR_GATEWAY_SELECTION; do
    literal_env_get "$SERVICE_KEY" && service_policy "$SERVICE_KEY" "$LITERAL_VALUE" || service_refuse 78
    service_policy "$SERVICE_KEY" "${!SERVICE_KEY:-}" || service_refuse 78
done
literal_env_get HERDR_RELAY_TRANSPORT || service_refuse 78
service_mode "$LITERAL_VALUE" || service_refuse 78
SERVICE_SELECTED_MODE="$SERVICE_VALUE"
service_mode "${HERDR_RELAY_TRANSPORT:-}" || service_refuse 78
SERVICE_AMBIENT_MODE="$SERVICE_VALUE"
if [ -n "$SERVICE_SELECTED_MODE" ] && [ -n "$SERVICE_AMBIENT_MODE" ] &&
    [ "$SERVICE_SELECTED_MODE" != "$SERVICE_AMBIENT_MODE" ]; then service_refuse 78; fi
TRANSPORT="${SERVICE_SELECTED_MODE:-$SERVICE_AMBIENT_MODE}"
literal_env_get HERDR_GATEWAY_URL || service_refuse 78
SERVICE_GATEWAYS="${LITERAL_VALUE:-${HERDR_GATEWAY_URL:-}}"
# Presence agrees with legacy comma/whitespace inference without invoking the
# sourcing gateway_urls fallback or a relay/tool to decide CLI admission.
SERVICE_VALUE="$LITERAL_VALUE${HERDR_GATEWAY_URL:-}"
SERVICE_VALUE="${SERVICE_VALUE//[[:space:],]/}"
if [ -z "$TRANSPORT" ]; then
    if [ -n "$SERVICE_VALUE" ]; then TRANSPORT=gateway; else TRANSPORT=cloudflare; fi
fi
case "$TRANSPORT" in
    gateway) [ -n "$SERVICE_VALUE" ] || service_refuse 78 ;;
    *) [ -z "$SERVICE_VALUE" ] || service_refuse 78 ;;
esac
for SERVICE_SESSION in "$(tailscale_session_file "$ENV_FILE")" "$(tailscale_external_session_file "$ENV_FILE")"; do
    [ ! -e "$SERVICE_SESSION" ] && [ ! -L "$SERVICE_SESSION" ] || service_refuse 78
done
case "$TRANSPORT" in tailscale|tailscale-external) service_refuse 78 ;; esac
# Assign admitted literal values as DATA, preserving native-generated fields.
# Raw updater credentials are never exported to helpers or essential children.
for ((SERVICE_INDEX=0; SERVICE_INDEX<${#LITERAL_KEYS[@]}; SERVICE_INDEX++)); do
    SERVICE_KEY="${LITERAL_KEYS[SERVICE_INDEX]}"
    case "$SERVICE_KEY" in GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE) continue ;; esac
    printf -v "$SERVICE_KEY" '%s' "${LITERAL_VALUES[SERVICE_INDEX]}" 2>/dev/null && export "$SERVICE_KEY" 2>/dev/null || service_refuse 78
done
literal_env_get HERDR_GITHUB_TOKEN_FILE || service_refuse 78
RELAY_UPDATER_TOKEN_FILE="${LITERAL_VALUE:-$RELAY_UPDATER_TOKEN_FILE}"
export -n RELAY_UPDATER_TOKEN_FILE
export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_TRANSPORT="$TRANSPORT" HERDR_GATEWAY_URL="$SERVICE_GATEWAYS"
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_RELAY_REARM_BOOTSTRAP

# Admission is a compiled, profile-specific production decision, before any
# backend or CLI work. A launchd pre-child refusal must not spin on KeepAlive.
if [ "$TRANSPORT" = tailscale-cli ]; then
    RELAY_BIN="$(relay_binary 2>/dev/null)" || service_refuse 78
    [ -f "$RELAY_BIN" ] && [ -x "$RELAY_BIN" ] || service_refuse 78
    "$RELAY_BIN" tailscale-cli activation-check >/dev/null 2>&1 || service_refuse 69
    exec "$SCRIPT_DIR/tailscale-cli-service.sh"
fi

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
# Agents often install their CLI into a per-tool bin directory that the service
# PATH does not include (e.g. ~/.opencode/bin). Append any that exist so the
# relay can detect them; base entries keep precedence.
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH
export HERDR_RELAY_HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"

case "$TRANSPORT" in
    cloudflare)
        CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
        CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
        if [ -z "$CLOUDFLARED_BIN" ] || [ ! -x "$CLOUDFLARED_BIN" ]; then
            service_refuse 78
        fi
        if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
            service_refuse 78
        fi
        ;;
    gateway)
        validate_service_gateway_urls 2>/dev/null || service_refuse 78
        ;;
    *) service_refuse 78 ;;
esac

RELAY_BIN="$(relay_binary 2>/dev/null)" || service_refuse 78
[ -f "$RELAY_BIN" ] && [ -x "$RELAY_BIN" ] || service_refuse 78

if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    HERDR_BIN="$(command -v herdr)"
    export HERDR_BIN
fi

RELAY_PID=""
TUNNEL_PID=""
RELAY_JOB=""
TUNNEL_JOB=""
REQUESTED_STATUS=0

# Only single-command essential jobs are launched, with immediate parent-shell
# capture. No async launch occurs between a fresh identity check and a signal.
# This is Bash job-table ownership, not OS process-tree isolation: no numeric
# PID signal fallback, discovery, or guarantee against same-user interference.
request_shutdown() {
    [ "$REQUESTED_STATUS" -ne 0 ] || REQUESTED_STATUS="$1"
    return 0
}

signal_child() {
    local job="$1" pid="$2" signal="$3" observed=0
    supervised_child_observe "$job" "$pid" || observed=$?
    [ "$observed" -ne 2 ] || return 0
    [ "$observed" -eq 0 ] || return 1
    if [ "$signal" = CONT ] && [ "$SUPERVISED_JOB_STATE" != Stopped ]; then
        return 0
    fi
    # The observation above freshly validated both jobspec and original PID.
    if kill "-$signal" "$job" 2>/dev/null; then
        return 0
    fi
    # Completion can race the signal. Accept only validated terminal ownership,
    # never wait on a stopped/active/missing/mismatched generation.
    observed=0
    supervised_child_observe "$job" "$pid" || observed=$?
    [ "$observed" -eq 2 ]
}

stop_child() {
    local job="$1" pid="$2" attempt observed deadline
    [ -n "$job" ] || return 1
    # Resume stopped children before TERM, including ones that stop again while
    # shutting down. The graceful observation phase ends after fifty 100ms rounds
    # or about five seconds, whichever comes first. A round also costs forks (jobs
    # file, ps), so on a slow host the round count alone let two sequential phases
    # outlast a supervisor's completion bound. SECONDS counts whole seconds (a
    # phase of four to five real seconds); the round cap bounds a clock that jumps.
    deadline=$((SECONDS + 5))
    for ((attempt = 0; attempt < 50 && SECONDS < deadline; attempt++)); do
        observed=0
        supervised_child_observe "$job" "$pid" || observed=$?
        [ "$observed" -ne 2 ] || return 0
        [ "$observed" -eq 0 ] || return 1
        if [ "$SUPERVISED_JOB_STATE" = Stopped ]; then
            signal_child "$job" "$pid" CONT || return 1
            signal_child "$job" "$pid" TERM || return 1
        elif [ "$attempt" -eq 0 ]; then
            signal_child "$job" "$pid" TERM || return 1
        fi
        sleep 0.1
    done
    signal_child "$job" "$pid" KILL || return 1
    # KILL is not proof of completion: require terminal observation/reaping.
    # Never block in wait when a child is still Running, Stopped or unknown.
    for ((attempt = 0; attempt < 10; attempt++)); do
        observed=0
        supervised_child_observe "$job" "$pid" || observed=$?
        [ "$observed" -ne 2 ] || return 0
        [ "$observed" -eq 0 ] || return 1
        [ "$attempt" -eq 9 ] || sleep 0.1
    done
    return 1
}

finish() {
    local status="$1" cleanup_failed=0
    trap - EXIT
    # Both captured children get an attempt, even if one identity is unknown.
    # Signals during capture/cleanup only record intent, never interrupt the
    # bookkeeping or recursively enter cleanup. The first request wins.
    if [ -n "$TUNNEL_PID" ] && ! stop_child "$TUNNEL_JOB" "$TUNNEL_PID"; then
        echo "cloudflared cleanup could not confirm owned-child completion (job=${TUNNEL_JOB:-none})" >&2
        cleanup_failed=1
    fi
    if [ -n "$RELAY_PID" ] && ! stop_child "$RELAY_JOB" "$RELAY_PID"; then
        echo "herdr relay cleanup could not confirm owned-child completion (job=${RELAY_JOB:-none})" >&2
        cleanup_failed=1
    fi
    if [ "$REQUESTED_STATUS" -ne 0 ]; then
        status="$REQUESTED_STATUS"
    elif [ "$cleanup_failed" -ne 0 ] && [ "$status" -eq 0 ]; then
        status=1
    fi
    supervised_child_release_prepared
    exit "$status"
}
trap 'finish "$?"' EXIT
trap 'request_shutdown 130' INT
trap 'request_shutdown 143' TERM

unexpected_exit() {
    local name="$1" status="$2"
    echo "$name exited unexpectedly with status $status" >&2
    # An unsolicited clean exit of EITHER essential child is failure 1 so
    # systemd Restart=on-failure remains applicable. Preserve nonzero exits.
    [ "$status" -ne 0 ] || status=1
    exit "$status"
}

check_child() {
    local name="$1" job="$2" pid="$3" observed=0
    supervised_child_observe "$job" "$pid" || observed=$?
    [ "$REQUESTED_STATUS" -eq 0 ] || exit "$REQUESTED_STATUS"
    case "$observed" in
        2) unexpected_exit "$name" "$SUPERVISED_JOB_STATUS" ;;
        0)
            [ "$SUPERVISED_JOB_STATE" = Running ] && return 0
            echo "$name stopped unexpectedly" >&2
            ;;
        *) echo "$name owned-job identity is unavailable" >&2 ;;
    esac
    exit 1
}

# Diagnostic only: say what a failed capture saw (counts and state words of the
# listing it retained, never the listing text). Builtins only, so it adds no fork
# and runs after the listing. It changes no exit decision, even if a write or read fails.
report_capture_failure() {
    local name="$1" pid="$2" status="$3" rest="${SUPERVISED_JOB_RECORDS:-}" nl=$'\n'
    local line listed state records=0 matches=0 states=""
    while [ -n "$rest" ]; do
        line=${rest%%"$nl"*}
        case "$rest" in *"$nl"*) rest=${rest#*"$nl"} ;; *) rest="" ;; esac
        records=$((records + 1))
        IFS=$' \t' read -r _ listed state _ <<< "$line" || continue
        [ "$listed" = "$pid" ] || continue
        matches=$((matches + 1))
        states="${states:+$states,}${state:-unknown}"
    done
    echo "$name owned-job capture failed (status=$status records=$records pid-matches=$matches state=${states:-none})" >&2 || true
}

echo "Starting herdr relay on $HERDR_RELAY_HOST:$HERDR_RELAY_PORT"
# The listing file is made BEFORE the launch: no forked command may run between
# launching a child and its first jobs -l (see supervised_child_prepare).
supervised_child_prepare || { echo "herdr relay owned-job listing file is unavailable" >&2; exit 1; }
HERDR_GITHUB_TOKEN_FILE="$RELAY_UPDATER_TOKEN_FILE" "$RELAY_BIN" serve-installed &
RELAY_PID=$!
capture_status=0
supervised_child_capture "$RELAY_PID" || capture_status=$?
RELAY_JOB="$SUPERVISED_JOB_SPEC"
[ "$capture_status" -eq 0 ] || [ "$capture_status" -eq 2 ] || report_capture_failure "herdr relay" "$RELAY_PID" "$capture_status"
[ "$REQUESTED_STATUS" -eq 0 ] || exit "$REQUESTED_STATUS"
if [ "$capture_status" -eq 2 ]; then
    unexpected_exit "herdr relay" "$SUPERVISED_JOB_STATUS"
fi
if [ "$capture_status" -ne 0 ]; then
    exit 1
fi
check_child "herdr relay" "$RELAY_JOB" "$RELAY_PID"

if [ "$TRANSPORT" = cloudflare ]; then
    echo "Starting cloudflared"
    supervised_child_prepare || { echo "cloudflared owned-job listing file is unavailable" >&2; exit 1; }
    (unset HERDR_GITHUB_TOKEN_FILE; exec "$CLOUDFLARED_BIN" tunnel --config "$CLOUDFLARED_CONFIG" run) &
    TUNNEL_PID=$!
    capture_status=0
    supervised_child_capture "$TUNNEL_PID" || capture_status=$?
    TUNNEL_JOB="$SUPERVISED_JOB_SPEC"
    capture_exit_status="$SUPERVISED_JOB_STATUS"
    [ "$capture_status" -eq 0 ] || [ "$capture_status" -eq 2 ] || report_capture_failure "cloudflared" "$TUNNEL_PID" "$capture_status"
    [ "$REQUESTED_STATUS" -eq 0 ] || exit "$REQUESTED_STATUS"
    # Capturing the tunnel may also observe/prune a completed relay record.
    # Its cached status is retained, including if tunnel capture is uncertain.
    check_child "herdr relay" "$RELAY_JOB" "$RELAY_PID"
    if [ "$capture_status" -eq 2 ]; then
        unexpected_exit "cloudflared" "$capture_exit_status"
    fi
    if [ "$capture_status" -ne 0 ]; then
        exit 1
    fi
fi

while true; do
    [ "$REQUESTED_STATUS" -eq 0 ] || exit "$REQUESTED_STATUS"
    check_child "herdr relay" "$RELAY_JOB" "$RELAY_PID"
    if [ -n "$TUNNEL_JOB" ]; then
        check_child "cloudflared" "$TUNNEL_JOB" "$TUNNEL_PID"
    fi
    sleep 1
done

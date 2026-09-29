#!/bin/bash
# Persistent CLI-backed Serve service entrypoint. Route setup/removal belongs to
# the explicit setup/recovery workflow; service restarts never mutate Serve.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
[ -f "$ENV_FILE" ] || {
    echo "Tailscale CLI relay configuration is missing: $ENV_FILE; install or repair the service, then restart it." >&2
    exit 0
}
load_relay_env "$ENV_FILE"
[ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
    echo "Tailscale CLI service requires HERDR_RELAY_TRANSPORT=tailscale-cli; repair the service environment, then restart it." >&2
    exit 0
}

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH
if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    HERDR_BIN="$(command -v herdr)"
    export HERDR_BIN
fi
export HERDR_RELAY_HOST=127.0.0.1
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"
if ! RELAY_BIN="$(relay_binary)"; then
    echo "Verified relay executable is unavailable; repair the installation and restart the service." >&2
    exit 0
fi
if ! "$RELAY_BIN" tailscale-cli activation-check >/dev/null; then
    echo "CLI-backed profiles are not enabled; the service remains stopped until an authorized configuration change and restart." >&2
    exit 0
fi
trap 'exit 130' INT
trap 'exit 143' TERM
HTTPS_PORT="${HERDR_TAILSCALE_CLI_HTTPS_PORT:-443}"
PREFLIGHT_ATTEMPTS=7
PREFLIGHT_DELAY=1
attempt=1
while [ "$attempt" -le "$PREFLIGHT_ATTEMPTS" ]; do
    if ! CLI_BIN="$("$RELAY_BIN" tailscale-cli resolve-binary --binary "${HERDR_TAILSCALE_CLI_BIN:-}")"; then
        echo "Tailscale CLI binary resolution failed; install/select the CLI and restart the service." >&2
        exit 0
    fi
    export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN"
    if PREFLIGHT="$("$RELAY_BIN" tailscale-cli preflight --binary "$CLI_BIN" --https-port "$HTTPS_PORT")"; then
        [ "$(json_string_field "$PREFLIGHT" node_id)" = "${HERDR_TAILSCALE_CLI_NODE_ID:-}" ] &&
            [ "$(json_string_field "$PREFLIGHT" origin)" = "${HERDR_TAILSCALE_CLI_ORIGIN:-}" ] || {
            echo "Permanent Tailscale node/origin drift: service stopped without changing Serve; reconcile and restart." >&2
            exit 0
        }
        break
    else
        preflight_status=$?
    fi
    if [ "$preflight_status" -ne 75 ]; then
        echo "Permanent Tailscale CLI preflight failure (status $preflight_status); repair authentication, permissions, profile, or Serve state, then restart." >&2
        exit 0
    fi
    if [ "$attempt" -eq "$PREFLIGHT_ATTEMPTS" ]; then
        echo "Tailscale CLI stayed temporarily unavailable after $PREFLIGHT_ATTEMPTS read-only attempts; service stopped without changing Serve. Restart it after recovery." >&2
        exit 0
    fi
    jitter=$((RANDOM % (PREFLIGHT_DELAY / 2 + 1)))
    retry_delay=$((PREFLIGHT_DELAY + jitter))
    [ "$retry_delay" -le 30 ] || retry_delay=30
    echo "Tailscale CLI is temporarily unavailable (attempt $attempt/$PREFLIGHT_ATTEMPTS); retrying read-only in ${retry_delay}s." >&2
    sleep "$retry_delay"
    PREFLIGHT_DELAY=$((PREFLIGHT_DELAY * 2))
    [ "$PREFLIGHT_DELAY" -le 30 ] || PREFLIGHT_DELAY=30
    attempt=$((attempt + 1))
done

RELAY_PID=""
# shellcheck disable=SC2329 # Registered as the service wrapper's EXIT trap.
cleanup() {
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
        wait "$RELAY_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$RELAY_BIN" serve &
RELAY_PID=$!
if ! wait_for_relay_identity_health "$HERDR_RELAY_PORT" "${HERDR_RELAY_INSTANCE_ID:-}" "${HERDR_TAILSCALE_CLI_ORIGIN:-}"; then
    echo "The exact relay instance failed local readiness; CLI Serve remains unchanged." >&2
    exit 1
fi
if ! kill -0 "$RELAY_PID" 2>/dev/null; then
    status=0
    wait "$RELAY_PID" || status=$?
    RELAY_PID=""
    exit "$status"
fi

PAIRING_SOCKET="${HERDR_RELAY_PAIRING_SOCKET:-}"
CONTROL_RUN_ID="${HERDR_RELAY_CONTROL_RUN_ID:-}"
INSTANCE_ID="${HERDR_RELAY_INSTANCE_ID:-}"
[ -S "$PAIRING_SOCKET" ] && [ -n "$CONTROL_RUN_ID" ] && [ -n "$INSTANCE_ID" ] || {
    echo "Private pairing-control identity is unavailable; relay admission remains closed." >&2
    exit 1
}

ADMITTED=false
while kill -0 "$RELAY_PID" 2>/dev/null; do
    status="$("$RELAY_BIN" pairing-control --socket "$PAIRING_SOCKET" --operation status \
        --run-id "$CONTROL_RUN_ID" --instance "$INSTANCE_ID" 2>/dev/null || true)"
    if [ "$(json_bool_field "$status" persistent_route_ready)" = true ]; then
        if [ "$ADMITTED" != true ] || [ "$(json_bool_field "$status" quarantined)" = true ]; then
            admitted="$("$RELAY_BIN" pairing-control --socket "$PAIRING_SOCKET" --operation admit \
                --run-id "$CONTROL_RUN_ID" --instance "$INSTANCE_ID" 2>/dev/null || true)"
            if [ "$(json_bool_field "$admitted" ready)" = true ] &&
                [ "$(json_bool_field "$admitted" persistent_route_ready)" = true ] &&
                [ "$(json_bool_field "$admitted" local_ready)" = true ] &&
                [ "$(json_bool_field "$admitted" serve_ready)" = true ]; then
                ADMITTED=true
            else
                ADMITTED=false
                echo "Route verification failed; relay admission remains suspended." >&2
            fi
        fi
    else
        ADMITTED=false
    fi
    sleep 5
done

status=0
wait "$RELAY_PID" || status=$?
RELAY_PID=""
exit "$status"

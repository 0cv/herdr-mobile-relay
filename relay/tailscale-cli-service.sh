#!/bin/bash
# Persistent CLI-backed Serve service entrypoint. Route setup/removal belongs to
# the explicit setup/recovery workflow; service restarts never mutate Serve.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
[ -f "$ENV_FILE" ] || {
    echo "Tailscale CLI relay configuration is missing: $ENV_FILE" >&2
    exit 78
}
load_relay_env "$ENV_FILE"
[ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
    echo "Tailscale CLI service requires HERDR_RELAY_TRANSPORT=tailscale-cli" >&2
    exit 78
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
RELAY_BIN="$(relay_binary)"
"$RELAY_BIN" tailscale-cli activation-check >/dev/null || exit $?
CLI_BIN="$("$RELAY_BIN" tailscale-cli resolve-binary --binary "${HERDR_TAILSCALE_CLI_BIN:-}")" || {
    echo "No unambiguous absolute Tailscale CLI executable is available to the service." >&2
    exit 78
}
export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN"
HTTPS_PORT="${HERDR_TAILSCALE_CLI_HTTPS_PORT:-443}"
PREFLIGHT="$("$RELAY_BIN" tailscale-cli preflight --binary "$CLI_BIN" --https-port "$HTTPS_PORT")" || {
    echo "Read-only Tailscale service preflight failed; no Serve route was changed." >&2
    exit 78
}
[ "$(json_string_field "$PREFLIGHT" node_id)" = "${HERDR_TAILSCALE_CLI_NODE_ID:-}" ] &&
    [ "$(json_string_field "$PREFLIGHT" origin)" = "${HERDR_TAILSCALE_CLI_ORIGIN:-}" ] || {
    echo "Live Tailscale node or HTTPS origin drifted from the registered service identity." >&2
    exit 78
}

RELAY_PID=""
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

while kill -0 "$RELAY_PID" 2>/dev/null; do
    status="$("$RELAY_BIN" pairing-control --socket "$PAIRING_SOCKET" --operation status \
        --run-id "$CONTROL_RUN_ID" --instance "$INSTANCE_ID" 2>/dev/null || true)"
    if [ "$(json_bool_field "$status" persistent_route_ready)" = true ] &&
        [ "$(json_bool_field "$status" invitation_armed)" != true ]; then
        armed="$("$RELAY_BIN" pairing-control --socket "$PAIRING_SOCKET" --operation arm_bootstrap \
            --run-id "$CONTROL_RUN_ID" --instance "$INSTANCE_ID" 2>/dev/null || true)"
        if [ "$(json_bool_field "$armed" ready)" != true ] ||
            [ "$(json_bool_field "$armed" persistent_route_ready)" != true ] ||
            [ "$(json_bool_field "$armed" invitation_armed)" != true ]; then
            echo "Route verification or fresh readiness arm is not complete; admission remains closed." >&2
        fi
    fi
    sleep 5
done

status=0
wait "$RELAY_PID" || status=$?
RELAY_PID=""
exit "$status"

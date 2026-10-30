#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi
if [ -z "${HERDR_RELAY_ENV:-}" ]; then
    SERVICE_ENV="$(installed_service_env_file)"
    [ -z "$SERVICE_ENV" ] || export HERDR_RELAY_ENV="$SERVICE_ENV"
fi
ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
require_supported_platform
assert_service_env_matches "$ENV_FILE"
RELAY_BIN="$(relay_binary)"
"$RELAY_BIN" tailscale check --port 8443 || exit $?
ADDRESS="$("$RELAY_BIN" tailscale address --port 8443)" || exit $?
PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
PORT="${PORT:-8375}"

echo "Always on Tailscale"
echo "  Phone address: $ADDRESS"
echo "  Add only Tailscale Serve HTTPS port 8443 (relay port $PORT)."
echo "  Install or reuse the background relay service."
if [ ! -t 0 ] || ! read -r -p "Continue? [y/N] " answer; then
    echo "Cancelled; nothing changed."
    exit 1
fi
case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "Cancelled; nothing changed."; exit 1 ;;
esac

"$RELAY_BIN" tailscale serve --bg --port 8443 --relay-port "$PORT" || exit $?
set_gateway_url "$ENV_FILE" ""
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT 8443
export HERDR_RELAY_ENV="$ENV_FILE"
if ! "$SCRIPT_DIR/setup.sh" --install-missing; then
    echo "✗ Prerequisite setup failed; Tailscale Serve remains configured. Rerun setup or turn it off in the menu." >&2
    exit 1
fi
if installed_relay_service_active; then
    if ! restart_installed_relay_service; then
        echo "✗ Could not restart the background relay; Tailscale Serve remains configured." >&2
        exit 1
    fi
elif ! "$SCRIPT_DIR/service.sh" install; then
    echo "✗ Background service failed; Tailscale Serve remains configured." >&2
    exit 1
fi
if ! wait_for_relay_health "$PORT" 15 1 "$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"; then
    echo "✗ The background relay did not become healthy. Check service logs; Tailscale Serve remains configured." >&2
    exit 1
fi
exec "$SCRIPT_DIR/setup-link.sh"

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

OLD_GATEWAY_URL="$(env_file_value "$ENV_FILE" HERDR_GATEWAY_URL)"
OLD_GATEWAY_SELECTION="$(env_file_value "$ENV_FILE" HERDR_GATEWAY_SELECTION)"
OLD_TAILSCALE_PORT="$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_PORT)"
ENV_WAS_PRESENT=false
[ ! -f "$ENV_FILE" ] || ENV_WAS_PRESENT=true
OLD_GATEWAY_URL_PRESENT=false
OLD_GATEWAY_SELECTION_PRESENT=false
OLD_TAILSCALE_PORT_PRESENT=false
[ ! -f "$ENV_FILE" ] || {
    grep -q '^HERDR_GATEWAY_URL=' "$ENV_FILE" && OLD_GATEWAY_URL_PRESENT=true
    grep -q '^HERDR_GATEWAY_SELECTION=' "$ENV_FILE" && OLD_GATEWAY_SELECTION_PRESENT=true
    grep -q '^HERDR_TAILSCALE_PORT=' "$ENV_FILE" && OLD_TAILSCALE_PORT_PRESENT=true
    true
}
OLD_SERVICE_INSTALLED=false
SERVICE_CHANGED=false
[ -z "$(installed_service_env_file)" ] || OLD_SERVICE_INSTALLED=true
restore_selection() {
    if [ "$OLD_GATEWAY_URL_PRESENT" = true ]; then
        set_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_URL "$OLD_GATEWAY_URL"
    else
        remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_URL
    fi
    if [ "$OLD_GATEWAY_SELECTION_PRESENT" = true ]; then
        set_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_SELECTION "$OLD_GATEWAY_SELECTION"
    else
        remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_SELECTION
    fi
    if [ "$OLD_TAILSCALE_PORT_PRESENT" = true ]; then
        set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT "$OLD_TAILSCALE_PORT"
    else
        remove_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT
    fi
    if [ "$ENV_WAS_PRESENT" = false ]; then
        rm -f "$ENV_FILE"
    fi
    if [ "$OLD_SERVICE_INSTALLED" = true ] && [ "$SERVICE_CHANGED" = true ]; then
        if ! "$SCRIPT_DIR/service.sh" install; then
            echo "✗ Previous service could not be restored; run relay/service.sh install after checking its logs." >&2
        fi
    fi
}
rollback() {
    local status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
        restore_selection
        echo "✗ Always on Tailscale was not published; the previous selection was restored." >&2
    fi
}
trap rollback EXIT
remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_URL
remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_SELECTION
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT 8443
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
export HERDR_RELAY_ENV="$ENV_FILE"
if ! "$SCRIPT_DIR/setup.sh" --install-missing; then
    echo "✗ Prerequisite setup failed." >&2
    exit 1
fi
SERVICE_CHANGED=true
if ! "$SCRIPT_DIR/service.sh" install; then
    echo "✗ Background service failed." >&2
    exit 1
fi
if ! wait_for_relay_health "$PORT" 15 1 "$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"; then
    echo "✗ The background relay did not become healthy. Check service logs." >&2
    exit 1
fi
if ! "$RELAY_BIN" tailscale serve --bg --port 8443 --relay-port "$PORT"; then
    echo "✗ Could not publish Tailscale Serve." >&2
    exit 1
fi
trap - EXIT
exec "$SCRIPT_DIR/setup-link.sh"

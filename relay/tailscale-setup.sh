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
if [ -n "$(gateway_urls "$ENV_FILE")" ]; then
    echo "A gateway is selected. Switch to a Cloudflare tunnel first, then choose Always on Tailscale." >&2
    exit 1
fi
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

ENV_BACKUP="$(mktemp "${TMPDIR:-/tmp}/herdr-tailscale-env.XXXXXX")"
ENV_WAS_PRESENT=false
if [ -f "$ENV_FILE" ]; then
    ENV_WAS_PRESENT=true
    cp -p "$ENV_FILE" "$ENV_BACKUP"
fi
OLD_SERVICE_INSTALLED=false
SERVICE_CHANGED=false
PUBLISHED=false
[ -z "$(installed_service_env_file)" ] || OLD_SERVICE_INSTALLED=true
rollback() {
    local status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
        if [ "$PUBLISHED" = true ]; then
            "$RELAY_BIN" tailscale off --port 8443 --relay-port "$PORT" ||
                echo "✗ Remove the published mapping manually: tailscale serve --https=8443 off" >&2
        fi
        if [ "$ENV_WAS_PRESENT" = true ]; then
            cp -p "$ENV_BACKUP" "$ENV_FILE"
        else
            rm -f "$ENV_FILE"
        fi
        if [ "$SERVICE_CHANGED" = true ]; then
            if [ "$OLD_SERVICE_INSTALLED" = true ]; then
                "$SCRIPT_DIR/service.sh" install ||
                    echo "✗ Previous service could not be restored; run relay/service.sh install." >&2
            else
                "$SCRIPT_DIR/service.sh" uninstall ||
                    echo "✗ New service could not be removed; run relay/service.sh uninstall." >&2
            fi
        fi
        echo "✗ Always on Tailscale was not published; the previous selection was restored." >&2
    fi
    rm -f "$ENV_BACKUP"
}
trap rollback EXIT
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT 8443
export HERDR_RELAY_ENV="$ENV_FILE"
if ! "$SCRIPT_DIR/setup.sh" --tailscale-prerequisites; then
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
PUBLISHED=true
"$SCRIPT_DIR/setup-link.sh"

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi
adopt_installed_service_env
ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
require_supported_platform
assert_service_env_matches "$ENV_FILE"
# Rerunning this setup is how the saved hostname changes, so only Tailscale is refused.
if [ -n "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_PORT)" ]; then
    refuse_if_always_on_selected "$ENV_FILE" || exit 1
fi
require_core_relay_release || exit 1
GATEWAYS="$(gateway_urls "$ENV_FILE")"
PROXY_HOST="$(env_file_value "$ENV_FILE" HERDR_REVERSE_PROXY_HOST)"
PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
PORT="${PORT:-$RELAY_DEFAULT_PORT}"

if [ ! -t 0 ]; then
    echo "Cancelled; nothing changed. Run this from a terminal, or set HERDR_REVERSE_PROXY_HOST in $ENV_FILE and run relay/service.sh install."
    exit 1
fi

echo "Your own reverse proxy"
echo "  The relay runs as a background service with no tunnel. Your proxy"
echo "  serves HTTPS on port 443 and forwards it, WebSocket upgrades included,"
echo "  to the relay on port $PORT."
echo ""
while true; do
    if [ -n "$PROXY_HOST" ]; then
        read -r -p "Hostname your phone opens [$PROXY_HOST]: " entered || { echo ""; echo "Cancelled; nothing changed."; exit 1; }
        entered="${entered:-$PROXY_HOST}"
    else
        read -r -p "Hostname your phone opens, such as relay.home.example: " entered || { echo ""; echo "Cancelled; nothing changed."; exit 1; }
    fi
    entered="$(normalize_hostname "$entered")"
    if valid_hostname "$entered"; then
        PROXY_HOST="$entered"
        break
    fi
    echo "✗ Enter a hostname such as relay.home.example, without a port or path."
done

LISTEN=127.0.0.1
[ "$(env_file_value "$ENV_FILE" HERDR_RELAY_HOST)" != 0.0.0.0 ] || LISTEN=0.0.0.0
default_answer=y
[ "$LISTEN" = 127.0.0.1 ] || default_answer=n
read -r -p "Does the proxy run on this computer? [$default_answer] " answer || answer=""
case "${answer:-$default_answer}" in
    y|Y|yes|YES) LISTEN=127.0.0.1 ;;
    *) LISTEN=0.0.0.0 ;;
esac

echo ""
echo "  Phone address: https://$PROXY_HOST"
if [ "$LISTEN" = 127.0.0.1 ]; then
    echo "  Proxy target:  http://127.0.0.1:$PORT"
else
    echo "  Proxy target:  http://<this computer's LAN address>:$PORT"
    echo "  The relay accepts connections from every network this computer is on."
fi
echo "  Your phone must trust the proxy's HTTPS certificate."
echo "  Install or reuse the background relay service."
if [ -n "$GATEWAYS" ]; then
    echo "  This replaces the selected WebRTC gateway list: $GATEWAYS."
fi
if ! read -r -p "Continue? [y/N] " answer; then
    echo ""
    echo "Cancelled; nothing changed."
    exit 1
fi
case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "Cancelled; nothing changed."; exit 1 ;;
esac

ENV_BACKUP="$(mktemp "${TMPDIR:-/tmp}/herdr-proxy-env.XXXXXX")"
ENV_WAS_PRESENT=false
if [ -f "$ENV_FILE" ]; then
    ENV_WAS_PRESENT=true
    cp -p "$ENV_FILE" "$ENV_BACKUP"
fi
OLD_SERVICE_INSTALLED=false
SERVICE_CHANGED=false
[ -z "$(installed_service_env_file)" ] || OLD_SERVICE_INSTALLED=true
rollback() {
    local status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
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
        echo "✗ The reverse proxy setup did not finish; the previous relay configuration was restored." >&2
    fi
    rm -f "$ENV_BACKUP"
}
trap rollback EXIT
ensure_relay_env "$ENV_FILE"
if [ -n "$GATEWAYS" ]; then
    set_gateway_url "$ENV_FILE" ""
    unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
fi
set_env_value_atomic "$ENV_FILE" HERDR_REVERSE_PROXY_HOST "$PROXY_HOST"
set_env_value_atomic "$ENV_FILE" HERDR_RELAY_HOST "$LISTEN"
export HERDR_RELAY_ENV="$ENV_FILE"
if ! "$SCRIPT_DIR/setup.sh" --always-on-prerequisites; then
    echo "✗ Prerequisite setup failed." >&2
    exit 1
fi
SERVICE_CHANGED=true
if ! "$SCRIPT_DIR/service.sh" install; then
    echo "✗ Background service failed." >&2
    exit 1
fi
if ! wait_for_relay_health "$PORT" 15 1 "$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)" >/dev/null; then
    echo "✗ The background relay did not become healthy. Check service logs." >&2
    exit 1
fi
trap - EXIT
rm -f "$ENV_BACKUP"
if ! HERDR_REQUIRE_SETUP_QR=1 "$SCRIPT_DIR/setup-link.sh"; then
    echo "Your own reverse proxy is set up, but the QR could not be shown: choose Reverse Proxy → Choose Phone App and Show QR" >&2
    exit 1
fi

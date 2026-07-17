#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

assert_service_env_matches "$ENV_FILE"
load_relay_env "$ENV_FILE"

if ! command -v uv >/dev/null 2>&1; then
    echo "✗ uv is required. Run make quick-start to install missing tools."
    exit 1
fi
if [ -z "${HERDR_RELAY_TOKEN:-}" ]; then
    echo "✗ No relay token in $ENV_FILE. Run make setup first."
    exit 1
fi

# The stable hostname: explicit argument wins, otherwise the first ingress
# hostname in the cloudflared config the background service uses.
TUNNEL_HOST="${1:-}"
TUNNEL_HOST="${TUNNEL_HOST#https://}"
TUNNEL_HOST="${TUNNEL_HOST#wss://}"
TUNNEL_HOST="${TUNNEL_HOST%%/*}"
if [ -z "$TUNNEL_HOST" ]; then
    CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
    if [ ! -r "$CONFIG" ]; then
        echo "✗ Cannot determine this relay's hostname: $CONFIG is missing."
        echo "  Follow the README's Stable Hostnames section first, or pass the"
        echo "  hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
    TUNNEL_HOST="$(sed -nE 's/^[[:space:]]*-?[[:space:]]*hostname:[[:space:]]*([^[:space:]#]+).*/\1/p' "$CONFIG" | head -1)"
    if [ -z "$TUNNEL_HOST" ]; then
        echo "✗ No ingress hostname found in $CONFIG."
        echo "  Pass the hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
fi

HOST_LABEL="$(host_label)"
RELAY_URL="wss://$TUNNEL_HOST"
SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
PHONE_APP_FALLBACK="https://$TUNNEL_HOST"
PHONE_APP_BASE="$(choose_phone_app_base_url "$PHONE_APP_FALLBACK" "$ENV_FILE" stable)"
record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
DIRECT_URL="$PHONE_APP_FALLBACK/#$SETUP_FRAGMENT"

echo "🐑 Herdr Mobile Relay phone setup"
echo ""
print_phone_setup "$PHONE_URL"
if [ "$PHONE_URL" != "$DIRECT_URL" ]; then
    echo ""
    echo "  Direct browser fallback:"
    echo "  $DIRECT_URL"
fi
echo ""
echo "  The relay and tunnel must be running for the link to work:"
echo "  make service-status"

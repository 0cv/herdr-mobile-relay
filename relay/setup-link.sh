#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

assert_service_env_matches "$ENV_FILE"
load_relay_env "$ENV_FILE"

relay_binary >/dev/null
if [ -z "${HERDR_RELAY_TOKEN:-}" ]; then
    echo "✗ No relay token in $ENV_FILE. Run make setup first."
    exit 1
fi

# A gateway-configured relay has no tunnel hostname and no cloudflared config:
# the phone finds it through the gateway, so the link only needs the app origin.
TRANSPORT="$(relay_transport "$ENV_FILE")"
if [ "$TRANSPORT" = tailscale ]; then
    ADDRESS="$("$(relay_binary)" tailscale address --port "$RELAY_TAILSCALE_PORT")" || exit $?
    print_pairing "$ENV_FILE" "wss://${ADDRESS#https://}" "$ADDRESS" stable "🐑 Herdr Mobile Relay phone setup"
    echo "  Tailscale Serve: $ADDRESS"
    exit 0
fi

if [ "$TRANSPORT" = gateway ]; then
    GATEWAY_URL="$(gateway_url "$ENV_FILE")"
    print_pairing "$ENV_FILE" "" "" gateway "🐑 Herdr Mobile Relay phone setup"
    echo "  Gateway: $GATEWAY_URL"
    echo "  The relay must be running for the link to work:"
    echo "  make service-status"
    exit 0
fi

# The stable hostname: explicit argument wins, otherwise the first ingress
# hostname in the cloudflared config the background service uses.
TUNNEL_HOST="$(normalize_hostname "${1:-}")"
if [ -z "$TUNNEL_HOST" ]; then
    CONFIG="${CLOUDFLARED_CONFIG:-$RELAY_CLOUDFLARED_CONFIG}"
    if [ ! -r "$CONFIG" ]; then
        echo "✗ Cannot determine this relay's hostname: $CONFIG is missing."
        echo "  Follow the README's Stable Hostnames section first, or pass the"
        echo "  hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
    TUNNEL_HOST="$(yaml_scalar hostname "$CONFIG")"
    if [ -z "$TUNNEL_HOST" ]; then
        echo "✗ No ingress hostname found in $CONFIG."
        echo "  Pass the hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
fi
if ! valid_hostname "$TUNNEL_HOST"; then
    echo "✗ Enter a valid hostname such as relay.example.com." >&2
    exit 1
fi

print_pairing "$ENV_FILE" "wss://$TUNNEL_HOST" "https://$TUNNEL_HOST" stable "🐑 Herdr Mobile Relay phone setup"
echo "  The relay and tunnel must be running for the link to work:"
echo "  make service-status"

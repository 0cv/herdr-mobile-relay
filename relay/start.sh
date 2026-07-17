#!/bin/bash
set -euo pipefail
echo "🐑 Herdr Mobile Relay quick start"
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RELAY_PID=""
TUNNEL_PID=""
LOG_FILE=""

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
export HERDR_RELAY_ENV="$ENV_FILE"

cleanup() {
    if [ -n "$TUNNEL_PID" ] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
        kill "$TUNNEL_PID" 2>/dev/null || true
    fi
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
    fi
    if [ -n "$LOG_FILE" ]; then
        rm -f "$LOG_FILE"
    fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

require_supported_platform
ensure_relay_env "$ENV_FILE"
load_relay_env "$ENV_FILE"

if ! command -v uv >/dev/null 2>&1; then
    if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        echo "✗ uv is required. Run Herdr Mobile Relay: Quick Start to install missing tools."
    else
        echo "✗ uv is required. Run make quick-start to install missing tools."
    fi
    exit 1
fi
if ! command -v herdr >/dev/null 2>&1 && [ -z "${HERDR_BIN:-}" ]; then
    echo "✗ herdr is required. Install Herdr, then run Quick Start again."
    exit 1
fi
PORT="${HERDR_RELAY_PORT:-8375}"
HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
TUNNEL_TARGET_HOST="$HOST"
if [ "$TUNNEL_TARGET_HOST" = "0.0.0.0" ]; then
    TUNNEL_TARGET_HOST="127.0.0.1"
fi

# 1. Start relay (uv auto-installs deps)
echo "▸ Starting relay on $HOST:$PORT..."
uv run "$SCRIPT_DIR/herdr_relay.py" &
RELAY_PID=$!
sleep 2

if ! kill -0 $RELAY_PID 2>/dev/null; then
    echo "✗ Relay failed to start. Check if port $PORT is in use."
    exit 1
fi

# 2. Start tunnel
if command -v cloudflared >/dev/null 2>&1; then
    echo "▸ Starting Cloudflare tunnel..."
    LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/herdr-cloudflared.XXXXXX")"
    cloudflared tunnel --config /dev/null --url "http://$TUNNEL_TARGET_HOST:$PORT" >"$LOG_FILE" 2>&1 &
    TUNNEL_PID=$!

    URL=""
    for _ in $(seq 1 30); do
        if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
            echo "✗ Cloudflare tunnel failed:"
            sed -n '1,120p' "$LOG_FILE"
            exit 1
        fi
        URL="$(sed -nE 's/.*(https:\/\/[^ ]*\.trycloudflare\.com).*/\1/p' "$LOG_FILE" | head -1)"
        if [ -n "$URL" ]; then
            break
        fi
        sleep 1
    done

    if [ -z "$URL" ]; then
        echo "✗ Timed out waiting for Cloudflare tunnel URL. Recent cloudflared output:"
        tail -40 "$LOG_FILE"
        exit 1
    fi

    # Quick-tunnel DNS takes a few seconds to go live after cloudflared prints
    # the URL. Opening the link too early makes some home routers cache the
    # miss for up to 30 minutes, so wait until the name resolves publicly
    # before showing it. DNS-over-HTTPS keeps the local resolver untouched
    # until then.
    TUNNEL_HOST="${URL#https://}"
    printf '▸ Waiting for the tunnel hostname to go live..'
    DNS_READY=""
    DNS_DEADLINE=$((SECONDS + 45))
    while [ "$SECONDS" -lt "$DNS_DEADLINE" ]; do
        if curl -fsS --max-time 5 -H 'accept: application/dns-json' \
                "https://cloudflare-dns.com/dns-query?name=$TUNNEL_HOST&type=A" 2>/dev/null \
                | grep -q '"Answer"'; then
            DNS_READY=1
            break
        fi
        printf '.'
        sleep 2
    done
    if [ -n "$DNS_READY" ]; then
        echo " ✓"
    else
        echo ""
        echo "  Warning: the tunnel hostname does not resolve yet. If the link"
        echo "  does not open on your phone, wait a minute and scan again."
    fi

    HOST_LABEL="$(host_label)"
    RELAY_URL="wss://${URL#https://}"
    SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
    PHONE_APP_BASE="$(choose_phone_app_base_url "$URL" "$ENV_FILE" temporary)"
    if [ "$PHONE_APP_BASE" != "$URL" ]; then
        record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
    fi
    PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    DIRECT_URL="$URL/#$SETUP_FRAGMENT"

    echo ""
    echo "✓ Relay ready!"
    echo ""
    print_phone_setup "$PHONE_URL"
    if [ "$PHONE_URL" != "$DIRECT_URL" ]; then
        echo ""
        echo "  Direct browser fallback:"
        echo "  $DIRECT_URL"
    fi
    echo ""
    echo "  The phone app and relay are both served by this tunnel."
    echo "  The link configures this relay automatically and removes the token from the address bar."
    echo "  Keep this terminal open; press Ctrl-C here to stop the quick start."
    echo ""
    echo "  Manual setup details:"
    echo "  Tunnel URL: $URL"
    echo "  WebSocket:  wss://${URL#https://}"
    echo "  Token:      $HERDR_RELAY_TOKEN"
    echo ""

    # Watch both processes; macOS ships bash 3.2, which lacks wait -n, so poll.
    # Without this, a dead relay would leave the tunnel serving 502s silently.
    while kill -0 "$RELAY_PID" 2>/dev/null && kill -0 "$TUNNEL_PID" 2>/dev/null; do
        sleep 2
    done
    if ! kill -0 "$RELAY_PID" 2>/dev/null; then
        echo "✗ The relay process stopped. The tunnel cannot serve the app without it."
        if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
            echo "  Run Herdr Mobile Relay: Quick Start again; check port $PORT if it fails again."
        else
            echo "  Rerun make quick-start; check port $PORT if it fails again."
        fi
        exit 1
    fi
    echo "✗ Cloudflare tunnel stopped. Recent cloudflared output:"
    if [ -f "$LOG_FILE" ]; then
        tail -40 "$LOG_FILE"
    fi
    exit 1
else
    echo ""
    echo "✓ Relay and phone app running on http://$HOST:$PORT"
    echo "  Token: $HERDR_RELAY_TOKEN"
    echo ""
    echo "  Install cloudflared for remote access:"
    if [ "$(uname -s)" = "Darwin" ]; then
        echo "    brew install cloudflared"
    else
        echo "    https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/"
    fi
    echo ""
    wait $RELAY_PID
fi

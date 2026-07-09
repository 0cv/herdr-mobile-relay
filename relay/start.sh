#!/bin/bash
set -euo pipefail
echo "🐑 Herdr Mobile Relay setup"
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
RELAY_PID=""
TUNNEL_PID=""
LOG_FILE=""

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

generate_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 16
    else
        uuidgen | tr '[:upper:]' '[:lower:]' | tr -d '-'
    fi
}

ensure_env() {
    if [ ! -f "$ENV_FILE" ]; then
        umask 077
        cat > "$ENV_FILE" <<EOF
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
HERDR_RELAY_PLUGIN_PORT=8376
HERDR_RELAY_POLL_INTERVAL=2
HERDR_ALLOWED_ORIGINS=
HERDR_RELAY_TOKEN=$(generate_token)
EOF
        echo "Created $ENV_FILE"
    else
        chmod 600 "$ENV_FILE"
        if ! grep -q '^HERDR_RELAY_HOST=' "$ENV_FILE"; then
            printf '\nHERDR_RELAY_HOST=127.0.0.1\n' >> "$ENV_FILE"
        fi
        if ! grep -q '^HERDR_RELAY_PORT=' "$ENV_FILE"; then
            printf '\nHERDR_RELAY_PORT=8375\n' >> "$ENV_FILE"
        fi
        if ! grep -q '^HERDR_RELAY_PLUGIN_PORT=' "$ENV_FILE"; then
            printf '\nHERDR_RELAY_PLUGIN_PORT=8376\n' >> "$ENV_FILE"
        fi
        if ! grep -q '^HERDR_RELAY_POLL_INTERVAL=' "$ENV_FILE"; then
            printf '\nHERDR_RELAY_POLL_INTERVAL=2\n' >> "$ENV_FILE"
        fi
        if ! grep -q '^HERDR_ALLOWED_ORIGINS=' "$ENV_FILE"; then
            printf '\nHERDR_ALLOWED_ORIGINS=\n' >> "$ENV_FILE"
        fi
    fi

    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a

    if [ -z "${HERDR_RELAY_TOKEN:-}" ]; then
        printf '\nHERDR_RELAY_TOKEN=%s\n' "$(generate_token)" >> "$ENV_FILE"
        set -a
        # shellcheck source=/dev/null
        . "$ENV_FILE"
        set +a
    fi
}

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

ensure_env
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
    echo "✗ Relay failed to start. Check if port 8375 is in use."
    exit 1
fi

# 2. Start tunnel
if command -v cloudflared >/dev/null 2>&1; then
    echo "▸ Starting Cloudflare tunnel..."
    LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/herdr-cloudflared.XXXXXX")"
    cloudflared tunnel --url "http://$TUNNEL_TARGET_HOST:$PORT" >"$LOG_FILE" 2>&1 &
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

    echo ""
    echo "✓ Relay ready!"
    echo ""
    echo "  Tunnel URL: $URL"
    echo "  WebSocket:  wss://$(echo $URL | sed 's|https://||')"
    echo "  Token:      $HERDR_RELAY_TOKEN"
    echo ""
    echo "  → Open your deployed web app on your phone"
    echo "  → Paste the WebSocket URL and Token in Settings"
    echo ""

    if ! wait "$TUNNEL_PID"; then
        echo "✗ Cloudflare tunnel stopped. Recent cloudflared output:"
        if [ -f "$LOG_FILE" ]; then
            tail -40 "$LOG_FILE"
        fi
        exit 1
    fi
else
    echo ""
    echo "✓ Relay running on ws://$HOST:$PORT"
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

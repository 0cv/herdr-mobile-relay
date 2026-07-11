#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a
fi

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HERDR_RELAY_HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"

UV_BIN="${UV_BIN:-$(command -v uv || true)}"
CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if [ -z "$UV_BIN" ]; then
    echo "uv not found in PATH"
    exit 78
fi

if [ -z "$CLOUDFLARED_BIN" ]; then
    echo "cloudflared not found in PATH"
    exit 78
fi

if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
    echo "Cloudflare tunnel config not readable: $CLOUDFLARED_CONFIG"
    exit 78
fi

if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    export HERDR_BIN="$(command -v herdr)"
fi

RELAY_PID=""
TUNNEL_PID=""

cleanup() {
    if [ -n "$TUNNEL_PID" ] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
        kill "$TUNNEL_PID" 2>/dev/null || true
    fi
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

echo "Starting herdr relay on $HERDR_RELAY_HOST:$HERDR_RELAY_PORT"
"$UV_BIN" run "$SCRIPT_DIR/herdr_relay.py" &
RELAY_PID=$!

echo "Starting cloudflared with $CLOUDFLARED_CONFIG"
"$CLOUDFLARED_BIN" tunnel --config "$CLOUDFLARED_CONFIG" run &
TUNNEL_PID=$!

while true; do
    if ! kill -0 "$RELAY_PID" 2>/dev/null; then
        wait "$RELAY_PID" || status=$?
        echo "herdr relay exited with status ${status:-0}"
        exit "${status:-1}"
    fi

    if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
        wait "$TUNNEL_PID" || status=$?
        echo "cloudflared exited with status ${status:-0}"
        exit "${status:-1}"
    fi

    sleep 5
done

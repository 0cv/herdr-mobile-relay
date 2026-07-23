#!/bin/sh
# Go relay service script. Runs the compiled Go binary with cloudflared.
# Drop-in replacement for herdr-mobile-relay-service.sh once the Go relay
# passes parity gates.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ENV_FILE="${HERDR_RELAY_ENV:-$HOME/.config/herdr-mobile-relay/relay.env}"
if [ -f "$ENV_FILE" ]; then
    set -a
    . "$ENV_FILE"
    set +a
fi

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH

export HERDR_RELAY_HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"

RELAY_BIN="${HERDR_RELAY_BIN:-$REPO_DIR/bin/herdr-mobile-relay}"
CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if [ ! -x "$RELAY_BIN" ]; then
    echo "relay binary not found: $RELAY_BIN" >&2
    echo "run: scripts/build.sh" >&2
    exit 78
fi

if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    HERDR_BIN="$(command -v herdr)"
    export HERDR_BIN
fi

RELAY_PID=""
TUNNEL_PID=""

cleanup() {
    [ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null || true
    [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "Starting herdr relay (Go) on $HERDR_RELAY_HOST:$HERDR_RELAY_PORT"
"$RELAY_BIN" &
RELAY_PID=$!

if [ -n "$CLOUDFLARED_BIN" ] && [ -r "$CLOUDFLARED_CONFIG" ]; then
    echo "Starting cloudflared with $CLOUDFLARED_CONFIG"
    "$CLOUDFLARED_BIN" tunnel --config "$CLOUDFLARED_CONFIG" run &
    TUNNEL_PID=$!
fi

while true; do
    if ! kill -0 "$RELAY_PID" 2>/dev/null; then
        wait "$RELAY_PID" || status=$?
        echo "herdr relay exited with status ${status:-0}"
        exit "${status:-1}"
    fi
    if [ -n "$TUNNEL_PID" ] && ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
        wait "$TUNNEL_PID" || status=$?
        echo "cloudflared exited with status ${status:-0}"
        exit "${status:-1}"
    fi
    sleep 5
done

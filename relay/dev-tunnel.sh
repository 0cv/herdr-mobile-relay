#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEV_DIR="${HERDR_DEV_CONFIG_DIR:-$SCRIPT_DIR/.dev}"
DEV_BIN_DIR="$DEV_DIR/bin"

mkdir -p "$DEV_DIR" "$DEV_BIN_DIR"
chmod 700 "$DEV_DIR"

unset HERDR_PLUGIN_CONFIG_DIR
export HERDR_DEV_TUNNEL=1
export HERDR_RELAY_ENV="${HERDR_DEV_RELAY_ENV:-$DEV_DIR/relay.env}"
export HERDR_RELAY_HOST="127.0.0.1"
export HERDR_RELAY_PORT="${HERDR_DEV_RELAY_PORT:-18375}"
export HERDR_RELAY_PLUGIN_PORT="${HERDR_DEV_PLUGIN_PORT:-18376}"
export HERDR_WEB_ROOT="$REPO_DIR/frontend/dist"
if [ -z "${HERDR_RELAY_BIN:-}" ]; then
    "$REPO_DIR/scripts/build.sh" "$DEV_BIN_DIR"
    export HERDR_RELAY_BIN="$DEV_BIN_DIR/herdr-mobile-relay"
fi

echo "🐑 Herdr Mobile Relay development tunnel"
echo ""
echo "  Config:      $HERDR_RELAY_ENV"
echo "  Relay:       http://127.0.0.1:$HERDR_RELAY_PORT"
echo "  Plugin UDP:  127.0.0.1:$HERDR_RELAY_PLUGIN_PORT"
echo "  Web root:    $HERDR_WEB_ROOT"
echo "  Binary:      $HERDR_RELAY_BIN"
echo "  Production relay port 8375 and its configuration are not used."
echo ""

if ! command -v bun >/dev/null 2>&1; then
    echo "✗ bun is required for make dev-tunnel. Install Bun 1.4 first." >&2
    exit 1
fi

bun run --cwd "$REPO_DIR/frontend" build
"$SCRIPT_DIR/setup.sh" --install-missing
exec "$SCRIPT_DIR/start.sh"

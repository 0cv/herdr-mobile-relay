#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
require_tui_backend

ENTRYPOINT="${1:-}"
PLUGIN_ID="${HERDR_PLUGIN_ID:-herdr-mobile-relay.events}"
HERDR_COMMAND="${HERDR_BIN_PATH:-herdr}"

if [ -z "$ENTRYPOINT" ]; then
    echo "Missing plugin pane entrypoint" >&2
    exit 2
fi

case "$ENTRYPOINT" in
    status) PLACEMENT="overlay" ;;
    *) PLACEMENT="zoomed" ;;
esac

args=(
    plugin pane open
    --plugin "$PLUGIN_ID"
    --entrypoint "$ENTRYPOINT"
    --placement "$PLACEMENT"
    --env "PATH=$PATH"
    --focus
)
if [ -n "${HERDR_PANE_ID:-}" ]; then
    args+=(--target-pane "$HERDR_PANE_ID")
fi

exec "$HERDR_COMMAND" "${args[@]}"

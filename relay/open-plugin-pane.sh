#!/bin/bash
set -euo pipefail

ENTRYPOINT="${1:-}"
PLUGIN_ID="${HERDR_PLUGIN_ID:-herdr-mobile-relay.events}"
HERDR_COMMAND="${HERDR_BIN_PATH:-herdr}"

if [ -z "$ENTRYPOINT" ]; then
    echo "Missing plugin pane entrypoint" >&2
    exit 2
fi

args=(
    plugin pane open
    --plugin "$PLUGIN_ID"
    --entrypoint "$ENTRYPOINT"
    --placement zoomed
    --focus
)
if [ -n "${HERDR_PANE_ID:-}" ]; then
    args+=(--target-pane "$HERDR_PANE_ID")
fi

exec "$HERDR_COMMAND" "${args[@]}"

#!/bin/bash
# Standalone setup entrypoint for a newly opened desktop terminal.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PLUGIN_ID="herdr-mobile-relay.events"
HERDR_COMMAND="${HERDR_BIN_PATH:-$(command -v herdr || true)}"

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

if [ -n "$HERDR_COMMAND" ]; then
    CONFIG_DIR="$("$HERDR_COMMAND" plugin config-dir "$PLUGIN_ID" 2>/dev/null || true)"
else
    CONFIG_DIR=""
fi
if [ -z "$CONFIG_DIR" ]; then
    CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/herdr/plugins/config/$PLUGIN_ID"
fi

export HERDR_PLUGIN_CONFIG_DIR="$CONFIG_DIR"
if [ -n "$HERDR_COMMAND" ]; then
    export HERDR_BIN_PATH="$HERDR_COMMAND"
fi

exec "$SCRIPT_DIR/plugin-setup-menu.sh"

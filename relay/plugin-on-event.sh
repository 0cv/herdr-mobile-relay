#!/bin/sh
# Herdr event-hook launcher. Prefer the system interpreter for the lowest
# latency, but keep plugin events working on uv-managed, Python-free installs.
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
SCRIPT_DIR=$(CDPATH='' cd "$SCRIPT_DIR" && pwd)

if command -v python3 >/dev/null 2>&1; then
    exec python3 "$SCRIPT_DIR/on_event.py"
fi

if command -v uv >/dev/null 2>&1; then
    exec uv run --quiet python "$SCRIPT_DIR/on_event.py"
fi

if [ -x "$HOME/.local/bin/uv" ]; then
    exec "$HOME/.local/bin/uv" run --quiet python "$SCRIPT_DIR/on_event.py"
fi

echo "herdr-mobile-relay: python3 and uv are unavailable; skipping the optional agent-status event." >&2
exit 0

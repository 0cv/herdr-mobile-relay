#!/bin/bash
# Launch herdi TUI in a herdr split pane (right side, 30% width)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Install deps if needed
pip3 install -q textual websockets 2>/dev/null

if command -v herdr &>/dev/null; then
    PANE=$(herdr pane current 2>/dev/null | jq -r '.result.pane_id' 2>/dev/null)
    if [ -n "$PANE" ] && [ "$PANE" != "null" ]; then
        herdr pane split "$PANE" --direction right --ratio 0.3 --focus
        sleep 0.3
        NEW_PANE=$(herdr pane current 2>/dev/null | jq -r '.result.pane_id' 2>/dev/null)
        herdr pane send-text "$NEW_PANE" "python3 $SCRIPT_DIR/herdi_tui.py"
        exit 0
    fi
fi

# Fallback: just run directly
python3 "$SCRIPT_DIR/herdi_tui.py"

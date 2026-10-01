#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ACTION="${1:-}"
PROFILE="${2:-${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}}"
case "$ACTION" in
    install|remove) ;;
    *) echo "Usage: $0 install|remove [Pi agent directory]" >&2; exit 2 ;;
esac
[[ "$PROFILE" = /* ]] || { echo "Pi agent directory must be absolute" >&2; exit 2; }
TARGET="$PROFILE/extensions/herdr-mobile-relay-commands"
MARKER="$TARGET/.relay-owned"
[[ ! -L "$TARGET" && ! -L "$PROFILE/extensions" ]] || { echo "Refusing a symlinked extension directory" >&2; exit 1; }
if [[ -e "$TARGET" ]]; then
    [[ -f "$MARKER" && ! -L "$MARKER" && "$(<"$MARKER")" = herdr-mobile-relay-pi-commands-v1 ]] || {
        echo "Refusing to change an unowned extension directory" >&2; exit 1;
    }
    for file in index.ts bridge.mjs; do
        [[ ! -L "$TARGET/$file" ]] || { echo "Refusing a symlinked integration file" >&2; exit 1; }
    done
elif [[ "$ACTION" = remove ]]; then
    exit 0
fi
if [[ "$ACTION" = install ]]; then
    mkdir -p "$TARGET"
    chmod 700 "$TARGET"
    install -m 600 "$SCRIPT_DIR/pi-command-bridge/bridge.mjs" "$TARGET/bridge.mjs"
    install -m 600 "$SCRIPT_DIR/pi-command-bridge/index.ts" "$TARGET/index.ts"
    printf '%s\n' herdr-mobile-relay-pi-commands-v1 > "$MARKER"
    chmod 600 "$MARKER"
    echo "Installed relay command metadata integration in $TARGET"
else
    rm -f "$TARGET/index.ts" "$TARGET/bridge.mjs" "$MARKER"
    rmdir "$TARGET" 2>/dev/null || true
    echo "Removed relay-owned command integration files from $TARGET"
fi
echo "Run /reload in each affected Pi pane, or restart Pi. Other integrations and settings were not changed."

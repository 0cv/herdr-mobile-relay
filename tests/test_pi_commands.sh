#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEMP="$(mktemp -d)"
trap 'rm -rf "$TEMP"' EXIT
export HOME="$TEMP/home"
export PI_CODING_AGENT_DIR="$TEMP/custom-agent"
mkdir -p "$PI_CODING_AGENT_DIR/extensions"
printf '%s\n' unrelated > "$PI_CODING_AGENT_DIR/extensions/herdr.ts"
bash "$ROOT/relay/pi-commands.sh" install
TARGET="$PI_CODING_AGENT_DIR/extensions/herdr-mobile-relay-commands"
[[ -s "$TARGET/index.ts" && -s "$TARGET/bridge.mjs" ]]
cmp "$ROOT/relay/pi-command-bridge/bridge.mjs" "$TARGET/bridge.mjs"
printf '%s\n' user-data > "$TARGET/unrelated"
bash "$ROOT/relay/pi-commands.sh" install
bash "$ROOT/relay/pi-commands.sh" remove
[[ -f "$TARGET/unrelated" && ! -e "$TARGET/index.ts" ]]
[[ "$(<"$PI_CODING_AGENT_DIR/extensions/herdr.ts")" = unrelated ]]
if bash "$ROOT/relay/pi-commands.sh" install; then
    echo 'Overwrote an unowned directory' >&2; exit 1
fi
OTHER="$TEMP/other"
bash "$ROOT/relay/setup.sh" --pi-install "$OTHER"
[[ -s "$OTHER/extensions/herdr-mobile-relay-commands/index.ts" ]]
bash "$ROOT/relay/setup.sh" --pi-remove "$OTHER"
bash "$ROOT/relay/pi-commands.sh" remove "$OTHER"
mkdir -p "$TEMP/symlink-profile/extensions"
ln -s "$TARGET" "$TEMP/symlink-profile/extensions/herdr-mobile-relay-commands"
if bash "$ROOT/relay/pi-commands.sh" install "$TEMP/symlink-profile"; then
    echo 'Followed a symlinked integration' >&2; exit 1
fi

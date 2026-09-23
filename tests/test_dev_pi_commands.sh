#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEMP="$(mktemp -d)"
trap 'rm -rf "$TEMP"' EXIT
export HOME="$TEMP/home"
export PI_CODING_AGENT_DIR="$TEMP/custom-agent"
mkdir -p "$PI_CODING_AGENT_DIR/extensions" "$HOME/.pi/agent"
TARGET="$PI_CODING_AGENT_DIR/extensions/herdr-mobile-relay-commands"

HERDR_DEV_PI_COMMANDS_INSTALL=0 "$ROOT/relay/dev-pi-commands.sh" > "$TEMP/disabled"
[ ! -e "$TARGET" ]
grep -q 'disabled' "$TEMP/disabled"

"$ROOT/relay/dev-pi-commands.sh" </dev/null > "$TEMP/installed"
cmp "$ROOT/relay/pi-command-bridge/bridge.mjs" "$TARGET/bridge.mjs"
cmp "$ROOT/relay/pi-command-bridge/index.ts" "$TARGET/index.ts"
[ ! -e "$HOME/.pi/agent/extensions/herdr-mobile-relay-commands" ]
grep -q 'Reload affected Pi panes' "$TEMP/installed"

"$ROOT/relay/dev-pi-commands.sh" </dev/null > "$TEMP/current"
grep -q 'integration is current' "$TEMP/current"

printf '%s\n' outdated > "$TARGET/bridge.mjs"
printf "export { default } from './bridge.mjs';\n" > "$TARGET/index.ts"
"$ROOT/relay/dev-pi-commands.sh" </dev/null > "$TEMP/updated"
cmp "$ROOT/relay/pi-command-bridge/bridge.mjs" "$TARGET/bridge.mjs"
cmp "$ROOT/relay/pi-command-bridge/index.ts" "$TARGET/index.ts"

printf '%s\n' user-data > "$TARGET/unrelated"
bash "$ROOT/relay/pi-commands.sh" remove "$PI_CODING_AGENT_DIR" > /dev/null
"$ROOT/relay/dev-pi-commands.sh" </dev/null > "$TEMP/refused" 2>&1
[ "$(<"$TARGET/unrelated")" = user-data ]
[ ! -e "$TARGET/index.ts" ]
grep -q 'not installed' "$TEMP/refused"

export PI_CODING_AGENT_DIR="$TEMP/missing-agent"
"$ROOT/relay/dev-pi-commands.sh" </dev/null > "$TEMP/missing"
[ ! -e "$PI_CODING_AGENT_DIR" ]
grep -q 'no agent directory' "$TEMP/missing"

echo 'dev Pi command integration checks passed'

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROFILE="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"

case "${HERDR_DEV_PI_COMMANDS_INSTALL:-1}" in
    0) echo "Pi command discovery: installation disabled for this dev tunnel."; exit 0 ;;
    1) ;;
    *) echo "HERDR_DEV_PI_COMMANDS_INSTALL must be 0 or 1" >&2; exit 2 ;;
esac

if [ ! -d "$PROFILE" ]; then
    echo "Pi command discovery: no agent directory at $PROFILE; skipping optional integration."
    exit 0
fi

echo "Pi command discovery: installing or updating the integration in $PROFILE."
echo "This shared Pi profile remains changed after the dev tunnel stops."
if bash "$SCRIPT_DIR/pi-commands.sh" install "$PROFILE"; then
    echo "Reload affected Pi panes with /reload (or restart Pi) before using phone commands."
else
    echo "Pi command integration was not installed; the dev tunnel can still run." >&2
fi

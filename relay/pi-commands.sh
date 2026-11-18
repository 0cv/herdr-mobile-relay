#!/usr/bin/env bash
# Install or remove the Pi command metadata integration. The ownership rules
# and the bundled files live in the relay binary, so this script only finds it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ACTION="${1:-}"
PROFILE="${2:-${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}}"
case "$ACTION" in
    install|remove) ;;
    *) echo "Usage: $0 install|remove [Pi agent directory]" >&2; exit 2 ;;
esac

BINARY="$(relay_binary)" || exit 1
exec "$BINARY" pi-commands "$ACTION" "$PROFILE"

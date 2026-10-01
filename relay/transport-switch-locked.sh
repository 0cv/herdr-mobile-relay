#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

[ "$#" -eq 2 ] || { echo "Usage: transport-switch-locked.sh ENV_FILE TRANSPORT" >&2; exit 2; }
export HERDR_CLI_TRANSPORT_SWITCH_LOCKED=1
set_relay_transport "$1" "$2"

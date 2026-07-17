#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

SERVICE_ENV="$(installed_service_env_file)"
if [ -n "$SERVICE_ENV" ]; then
    export HERDR_RELAY_ENV="$SERVICE_ENV"
fi

if ! "$SCRIPT_DIR/setup-link.sh"; then
    echo ""
    echo "No stable phone setup link could be generated. Run Stable Tunnel setup"
    echo "first, or rerun Quick Start for a new temporary link."
    pause_before_close
    exit 1
fi

pause_before_close

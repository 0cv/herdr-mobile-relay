#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

adopt_installed_service_env announce

if ! "$SCRIPT_DIR/configure-app-deploy.sh"; then
    echo ""
    echo "App deployment configuration did not complete."
    pause_before_close
    exit 1
fi

pause_before_close

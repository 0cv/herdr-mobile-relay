#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

if [ -z "${HERDR_RELAY_ENV:-}" ]; then
    SERVICE_ENV="$(installed_service_env_file)"
    if [ -n "$SERVICE_ENV" ]; then
        export HERDR_RELAY_ENV="$SERVICE_ENV"
        echo "Reusing the installed relay configuration:"
        echo "  $SERVICE_ENV"
        echo ""
    fi
fi

if ! "$SCRIPT_DIR/configure-app-deploy.sh"; then
    echo ""
    echo "App deployment configuration did not complete."
    pause_before_close
    exit 1
fi

pause_before_close

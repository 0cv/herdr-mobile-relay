#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

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

echo "🐑 Herdr Mobile Relay stable tunnel setup"
echo ""
echo "This wizard provisions or reuses a named Cloudflare tunnel, installs the"
echo "background service, and verifies the public relay before showing its QR."
echo "If you only want to try the relay, run Quick Start instead:"
echo "  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events"
echo ""

if ! "$SCRIPT_DIR/stable-setup.sh"; then
    echo ""
    echo "Stable setup did not complete. Its state is resumable; use the exact"
    echo "rerun command printed above after correcting the reported problem."
    pause_before_close
    exit 1
fi

pause_before_close

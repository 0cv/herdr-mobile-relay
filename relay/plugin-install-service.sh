#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

echo "🐑 Herdr Mobile Relay background service setup"
echo ""
echo "This requires a named Cloudflare tunnel configuration. If you only want"
echo "to try the relay, run the Quick Start action instead:"
echo "  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events"
echo ""

if ! "$SCRIPT_DIR/setup.sh" --install-missing; then
    pause_before_close
    exit 1
fi
if ! "$SCRIPT_DIR/service.sh" install; then
    echo ""
    echo "The background service was not installed. Follow the Stable Hostnames"
    echo "section in the README, then run this action again."
    pause_before_close
    exit 1
fi

echo ""
if ! "$SCRIPT_DIR/setup-link.sh"; then
    echo ""
    echo "The service is running, but its public hostname could not be detected."
    echo "Set CLOUDFLARED_CONFIG in the plugin relay.env and run this action again."
    pause_before_close
    exit 1
fi

pause_before_close

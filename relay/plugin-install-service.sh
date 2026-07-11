#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

pause_before_close() {
    if [ -t 0 ]; then
        echo ""
        read -r -p "Press Enter to close this pane." _answer
    fi
}

echo "🐑 Herdr Mobile Relay background service setup"
echo ""
echo "This requires a named Cloudflare tunnel configuration. If you only want"
echo "to try the relay, use Herdr Mobile Relay: Quick Start instead."
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

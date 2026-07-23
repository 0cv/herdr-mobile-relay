#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

echo "🐑 Herdr Mobile Relay stable tunnel teardown"
echo ""
echo "This removes only resources recorded as wizard-owned. Run it before"
echo "uninstalling the plugin when you also want to remove its stable tunnel."
echo ""

if ! "$SCRIPT_DIR/stable-teardown.sh"; then
    echo ""
    echo "Stable teardown did not complete. The plugin can remain installed while"
    echo "you correct the reported problem and invoke this action again."
    pause_before_close
    exit 1
fi

echo ""
echo "Stable resources are cleared. To unregister the plugin, run:"
echo "  herdr plugin uninstall herdr-mobile-relay.events"
pause_before_close

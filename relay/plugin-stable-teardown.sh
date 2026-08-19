#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

echo "🐑 Herdr Mobile Relay stable tunnel teardown"
echo ""
echo "This removes the service, tunnel, config, and credentials recorded for"
echo "this stable relay, regardless of how an earlier setup created them."
echo ""

if ! HERDR_STABLE_TEARDOWN_WRAPPED=1 "$SCRIPT_DIR/stable-teardown.sh"; then
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

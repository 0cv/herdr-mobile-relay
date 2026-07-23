#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

echo "🐑 Herdr Mobile Relay plugin setup"
echo ""
echo "This pane installs missing user-level tools, creates private plugin"
echo "configuration, and starts a temporary relay with a phone setup QR code."
echo ""

"$SCRIPT_DIR/setup.sh" --install-missing
exec "$SCRIPT_DIR/start.sh"

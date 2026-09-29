#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
ENV_FILE="$(service_environment_file "$SCRIPT_DIR")"
service_cli_route_disposition "$ENV_FILE" "$SCRIPT_DIR"

LABELS=("com.herdr-mobile-relay.service" "com.herdr-remote.service")

for label in "${LABELS[@]}"; do
    plist="$HOME/Library/LaunchAgents/$label.plist"
    launchctl bootout "gui/$UID" "$plist" >/dev/null 2>&1 || true
    rm -f "$plist"
done

echo "Stopped and removed Herdr Mobile Relay services"

#!/bin/bash
set -euo pipefail

LABELS=("com.herdr-mobile-relay.service" "com.herdr-remote.service")

for label in "${LABELS[@]}"; do
    plist="$HOME/Library/LaunchAgents/$label.plist"
    launchctl bootout "gui/$UID" "$plist" >/dev/null 2>&1 || true
    rm -f "$plist"
done

echo "Stopped and removed Herdr Mobile Relay services"

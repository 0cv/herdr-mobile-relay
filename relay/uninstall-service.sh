#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
if [ "${HERDR_TAILSCALE_FULL_UNINSTALL:-}" != 1 ]; then
    SERVICE_ENV="$(installed_service_env_file)"
    if [ -n "$SERVICE_ENV" ]; then
        refuse_if_tailscale_selected "$SERVICE_ENV" || exit 1
    fi
    refuse_if_tailscale_selected "${HERDR_RELAY_ENV:-${SERVICE_ENV:-$(relay_env_file "$SCRIPT_DIR")}}" || exit 1
fi

LABELS=("com.herdr-mobile-relay.service" "com.herdr-remote.service")

for label in "${LABELS[@]}"; do
    plist="$HOME/Library/LaunchAgents/$label.plist"
    launchctl bootout "gui/$UID" "$plist" >/dev/null 2>&1 || true
    rm -f "$plist"
done

echo "Stopped and removed Herdr Mobile Relay services"

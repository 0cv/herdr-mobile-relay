#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
ENV_FILE="$(service_environment_file "$SCRIPT_DIR")"
service_cli_route_disposition "$ENV_FILE" "$SCRIPT_DIR"

LABELS=("herdr-mobile-relay.service" "herdr-remote.service")

for label in "${LABELS[@]}"; do
    systemctl --user disable --now "$label" >/dev/null 2>&1 || true
    rm -f "$HOME/.config/systemd/user/$label"
done
systemctl --user daemon-reload

echo "Stopped and removed Herdr Mobile Relay services"

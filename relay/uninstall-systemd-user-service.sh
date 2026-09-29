#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
[ "$#" -le 1 ] || { echo "Usage: $0 [--cli-route-disposition=retained|removed]" >&2; exit 2; }
ROUTE_DISPOSITION="${1:-}"
case "$ROUTE_DISPOSITION" in
    "") ;;
    --cli-route-disposition=retained) ROUTE_DISPOSITION=retained ;;
    --cli-route-disposition=removed) ROUTE_DISPOSITION=removed ;;
    *) echo "Usage: $0 [--cli-route-disposition=retained|removed]" >&2; exit 2 ;;
esac
ENV_FILE="$(service_environment_file "$SCRIPT_DIR")"
service_cli_route_disposition "$ENV_FILE" "$SCRIPT_DIR" "$ROUTE_DISPOSITION"

LABELS=("herdr-mobile-relay.service" "herdr-remote.service")

for label in "${LABELS[@]}"; do
    systemctl --user disable --now "$label" >/dev/null 2>&1 || true
    rm -f "$HOME/.config/systemd/user/$label"
done
systemctl --user daemon-reload

echo "Stopped and removed Herdr Mobile Relay services"

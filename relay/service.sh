#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ACTION="${1:-}"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_supported_platform

case "$ACTION" in
    install|uninstall|status|logs)
        ;;
    *)
        echo "Usage: $0 {install|uninstall|status|logs}"
        exit 2
        ;;
esac

case "$(uname -s)" in
    Darwin)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-service.sh" ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-service.sh" ;;
            status) exec launchctl print "gui/$(id -u)/$RELAY_LAUNCHD_LABEL" ;;
            logs) exec tail -f "$RELAY_LOG_DIR/service.log" "$RELAY_LOG_DIR/service.err" ;;
        esac
        ;;
    Linux)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-systemd-user-service.sh" ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh" ;;
            status) exec systemctl --user status "$RELAY_SYSTEMD_LABEL" ;;
            logs) exec journalctl --user -u "$RELAY_SYSTEMD_LABEL" -f ;;
        esac
        ;;
esac

#!/bin/bash
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
            status) exec launchctl print "gui/$(id -u)/com.herdr-mobile-relay.service" ;;
            logs) exec tail -f "$HOME/Library/Logs/herdr-mobile-relay/service.log" "$HOME/Library/Logs/herdr-mobile-relay/service.err" ;;
        esac
        ;;
    Linux)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-systemd-user-service.sh" ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh" ;;
            status) exec systemctl --user status herdr-mobile-relay.service ;;
            logs) exec journalctl --user -u herdr-mobile-relay.service -f ;;
        esac
        ;;
esac

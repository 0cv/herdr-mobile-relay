#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ACTION="${1:-}"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_supported_platform

case "$ACTION" in
    install|uninstall|status|stop|logs|rollback-cli-setup)
        ;;
    *)
        echo "Usage: $0 {install|uninstall|status|stop|logs} (rollback-cli-setup is internal)"
        exit 2
        ;;
esac

case "$(uname -s)" in
    Darwin)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-service.sh" ;;
            rollback-cli-setup)
                [ "${HERDR_CLI_SETUP_ROLLBACK:-}" = 1 ] || {
                    echo "✗ CLI setup rollback is an internal service cleanup operation." >&2
                    exit 2
                }
                plist="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
                [ -e "$plist" ] || exit 0
                launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || {
                    echo "✗ Could not unload the newly installed CLI service definition." >&2
                    exit 1
                }
                rm -f "$plist"
                echo "Removed the newly installed relay service definition; persistent Serve state was not changed."
                ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-service.sh" ;;
            status) exec launchctl print "gui/$(id -u)/com.herdr-mobile-relay.service" ;;
            stop) exec launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ;;
            logs) exec tail -f "$HOME/Library/Logs/herdr-mobile-relay/service.log" "$HOME/Library/Logs/herdr-mobile-relay/service.err" ;;
        esac
        ;;
    Linux)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-systemd-user-service.sh" ;;
            rollback-cli-setup)
                [ "${HERDR_CLI_SETUP_ROLLBACK:-}" = 1 ] || {
                    echo "✗ CLI setup rollback is an internal service cleanup operation." >&2
                    exit 2
                }
                label="herdr-mobile-relay.service"
                unit="$HOME/.config/systemd/user/$label"
                [ -e "$unit" ] || exit 0
                systemctl --user disable --now "$label" >/dev/null 2>&1 || {
                    echo "✗ Could not disable the newly installed CLI service definition." >&2
                    exit 1
                }
                rm -f "$unit"
                systemctl --user daemon-reload || {
                    echo "✗ Removed the CLI service file, but systemd could not reload its user configuration." >&2
                    exit 1
                }
                echo "Removed the newly installed relay service definition; persistent Serve state was not changed."
                ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh" ;;
            status) exec systemctl --user status herdr-mobile-relay.service ;;
            stop) exec systemctl --user stop herdr-mobile-relay.service ;;
            logs) exec journalctl --user -u herdr-mobile-relay.service -f ;;
        esac
        ;;
esac

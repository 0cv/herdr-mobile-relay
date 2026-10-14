#!/usr/bin/env bash
set +x
set -euo pipefail

ACTION="${1:-}"
case "$ACTION" in
    uninstall)
        [ "$#" -ge 1 ] && [ "$#" -le 2 ] || { echo 'Invalid service arguments.' >&2; exit 2; }
        case "${2:-}" in ''|--cli-route-disposition=retained|--cli-route-disposition=removed) ;; *) echo 'Invalid service arguments.' >&2; exit 2 ;; esac
        # An explicit empty extra argument is not the no-argument contract.
        [ "$#" -eq 1 ] || [ -n "$2" ] || { echo 'Invalid service arguments.' >&2; exit 2; }
        ;;
    rollback-cli-setup|assert-stopped)
        [ "$#" -eq 1 ] || { echo 'Invalid service arguments.' >&2; exit 2; }
        ;;
    install|status|stop|logs) ;; # Preserve the ordinary delegates' argv behavior.
    *) echo 'Usage: service.sh {install|uninstall|status|stop|logs|assert-stopped} (rollback-cli-setup is internal)' >&2; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
require_supported_platform

case "$ACTION" in
    assert-stopped|rollback-cli-setup)
        if [ "$ACTION" = rollback-cli-setup ] && [ "${HERDR_CLI_SETUP_ROLLBACK:-}" != 1 ]; then
            echo 'CLI setup rollback is an internal service cleanup operation.' >&2; exit 2
        fi
        require_user_service_context || exit 1
        case "$(uname -s)" in
            Linux) manager=systemd; current_label=herdr-mobile-relay.service ;;
            Darwin) manager=launchd; current_label=com.herdr-mobile-relay.service ;;
            *) exit 1 ;;
        esac
        if [ "$ACTION" = assert-stopped ]; then
            for label in $(service_removal_labels "$manager"); do
                service_removal_quiet "$manager" "$label" || {
                    echo 'Could not prove both user service labels inactive/absent and disabled; no reservation authority is granted.' >&2
                    exit 1
                }
            done
            # No listener observation or route/reservation change is made.
            exit 0
        fi
        trap 'exit 130' INT
        trap 'exit 143' TERM
        ENV_FILE="$(service_environment_file "$SCRIPT_DIR")" || {
            echo 'Current service cleanup cannot safely select its environment; no native removal started.' >&2; exit 1;
        }
        # Internal cleanup retains route state; it cannot bypass selected
        # credential/configuration admission or imply unpublish/readback.
        service_cli_route_disposition "$ENV_FILE" "$SCRIPT_DIR" retained || exit 1
        if service_removal_one "$manager" "$current_label"; then
            echo 'Current relay service-definition cleanup verified; legacy and persistent Serve state were not changed.'
            exit 0
        else
            echo 'Current relay service cleanup is incomplete; no reservation or route was cleared.' >&2
            exit 1
        fi
        ;;
esac

case "$(uname -s)" in
    Darwin)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-service.sh" ;;
            uninstall)
                if [ "$#" -eq 2 ]; then exec "$SCRIPT_DIR/uninstall-service.sh" "$2"; fi
                exec "$SCRIPT_DIR/uninstall-service.sh"
                ;;
            status) exec launchctl print "gui/$(id -u)/com.herdr-mobile-relay.service" ;;
            stop) exec launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ;;
            logs) exec tail -f "$HOME/Library/Logs/herdr-mobile-relay/service.log" "$HOME/Library/Logs/herdr-mobile-relay/service.err" ;;
        esac
        ;;
    Linux)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-systemd-user-service.sh" ;;
            uninstall)
                if [ "$#" -eq 2 ]; then exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh" "$2"; fi
                exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh"
                ;;
            status) exec systemctl --user status herdr-mobile-relay.service ;;
            stop) exec systemctl --user stop herdr-mobile-relay.service ;;
            logs) exec journalctl --user -u herdr-mobile-relay.service -f ;;
        esac
        ;;
esac

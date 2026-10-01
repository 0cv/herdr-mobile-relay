#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ACTION="${1:-}"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_supported_platform

case "$ACTION" in
    install|uninstall|status|stop|logs|rollback-cli-setup|assert-stopped)
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
            assert-stopped)
                loaded_jobs="$(launchctl list 2>/dev/null)" || {
                    echo "✗ Could not inspect launchd user jobs." >&2
                    exit 1
                }
                for label in com.herdr-mobile-relay.service com.herdr-remote.service; do
                    if printf '%s\n' "$loaded_jobs" | awk -v label="$label" '$3 == label { found=1 } END { exit !found }'; then
                        echo "✗ LaunchAgent $label remains loaded; unload it before releasing a backend reservation." >&2
                        exit 1
                    fi
                done
                ;;
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
            assert-stopped)
                for label in herdr-mobile-relay.service herdr-remote.service; do
                    state="$(systemctl --user show "$label" --property=LoadState --property=ActiveState --property=UnitFileState --no-pager 2>/dev/null)" || {
                        echo "✗ Could not verify systemd user-unit state for $label." >&2
                        exit 1
                    }
                    load_state="$(printf '%s\n' "$state" | awk -F= '$1 == "LoadState" { print $2 }')"
                    active="$(printf '%s\n' "$state" | awk -F= '$1 == "ActiveState" { print $2 }')"
                    enabled="$(printf '%s\n' "$state" | awk -F= '$1 == "UnitFileState" { print $2 }')"
                    [ "$load_state" = not-found ] && continue
                    case "$active" in inactive|failed) ;; *) echo "✗ User service $label is not stopped (state: $active)." >&2; exit 1 ;; esac
                    case "$enabled" in enabled|enabled-runtime|linked|linked-runtime|alias)
                        echo "✗ User service $label is still enabled and may restart." >&2
                        exit 1
                        ;;
                        disabled|static|indirect|generated|masked|not-found) ;;
                        *) echo "✗ Could not prove user service $label is disabled (state: $enabled)." >&2; exit 1 ;;
                    esac
                done
                ;;
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

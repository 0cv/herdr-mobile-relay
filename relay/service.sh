#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ACTION="${1:-}"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_supported_platform

case "$ACTION" in
    install|uninstall|status|logs|reset)
        ;;
    *)
        echo "Usage: $0 {install|uninstall|status|logs|reset}"
        exit 2
        ;;
esac

case "$(uname -s)" in
    Darwin)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-service.sh" ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-service.sh" ;;
            status) exec launchctl print "gui/$(id -u)/com.herdr-mobile-relay.service" ;;
            reset)
                load_relay_env "$(relay_env_file "$SCRIPT_DIR")"
                STATE_ROOT="${HERDR_RELAY_SUPERVISOR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay}"
                "$(relay_binary)" supervisor-reset "$STATE_ROOT/supervisor.json"
                exec launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
                ;;
            logs)
                load_relay_env "$(relay_env_file "$SCRIPT_DIR")"
                STATE_ROOT="${HERDR_RELAY_SUPERVISOR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay}"
                exec tail -F "$STATE_ROOT/logs/relay.stdout.log" "$STATE_ROOT/logs/relay.stderr.log" "$STATE_ROOT/logs/cloudflared.stdout.log" "$STATE_ROOT/logs/cloudflared.stderr.log"
                ;;
        esac
        ;;
    Linux)
        case "$ACTION" in
            install) exec "$SCRIPT_DIR/install-systemd-user-service.sh" ;;
            uninstall) exec "$SCRIPT_DIR/uninstall-systemd-user-service.sh" ;;
            status) exec systemctl --user status herdr-mobile-relay.service ;;
            reset)
                load_relay_env "$(relay_env_file "$SCRIPT_DIR")"
                STATE_ROOT="${HERDR_RELAY_SUPERVISOR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay}"
                "$(relay_binary)" supervisor-reset "$STATE_ROOT/supervisor.json"
                systemctl --user reset-failed herdr-mobile-relay.service
                exec systemctl --user restart herdr-mobile-relay.service
                ;;
            logs) exec journalctl --user -u herdr-mobile-relay.service -f ;;
        esac
        ;;
esac

#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
load_relay_env "$ENV_FILE"

echo "🐑 Herdr Mobile Relay status"
echo ""
echo "  Config file:  $ENV_FILE"
if [ -f "$ENV_FILE" ] && grep -q '^HERDR_RELAY_TOKEN=..*' "$ENV_FILE"; then
    echo "  Relay token:  present"
else
    echo "  Relay token:  missing — run the Quick Start action once"
fi

case "$(uname -s)" in
    Darwin)
        SERVICE_FILE="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        if [ ! -f "$SERVICE_FILE" ]; then
            echo "  Service:      not installed"
        elif launchctl print "gui/$(id -u)/com.herdr-mobile-relay.service" >/dev/null 2>&1; then
            echo "  Service:      installed (active)"
        else
            echo "  Service:      installed (inactive)"
        fi
        ;;
    Linux)
        SERVICE_FILE="$HOME/.config/systemd/user/herdr-mobile-relay.service"
        if [ ! -f "$SERVICE_FILE" ]; then
            echo "  Service:      not installed"
        else
            SERVICE_STATE="$(systemctl --user is-active herdr-mobile-relay.service 2>/dev/null || true)"
            if [ -n "$SERVICE_STATE" ]; then
                echo "  Service:      installed ($SERVICE_STATE)"
            else
                echo "  Service:      installed (status unavailable)"
            fi
        fi
        ;;
esac
SERVICE_ENV="$(installed_service_env_file)"
if [ -n "$SERVICE_ENV" ]; then
    echo "  Service env:  $SERVICE_ENV"
fi

PORT="${HERDR_RELAY_PORT:-8375}"
if HEALTH="$(curl -fsS --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null)"; then
    echo "  Relay health: $HEALTH"
else
    echo "  Relay health: not reachable on 127.0.0.1:$PORT — is the relay running?"
fi

pause_before_close

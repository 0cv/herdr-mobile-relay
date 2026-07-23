#!/bin/sh
# Marketplace build hook: install the exact pre-built release named by the
# plugin manifest. End-user hosts never compile Go or install Python/uv.
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
SCRIPT_DIR=$(CDPATH='' cd "$SCRIPT_DIR" && pwd)
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)

VERSION=$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml")
[ -n "$VERSION" ] || {
    echo "herdr-mobile-relay: herdr-plugin.toml has no exact version" >&2
    exit 1
}

INSTALL_ROOT=${HERDR_RELEASE_ROOT:-"${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay"}
BIN_DIR=${HERDR_RELAY_BIN_DIR:-"$HOME/.local/bin"}
export INSTALL_ROOT BIN_DIR

echo "herdr-mobile-relay: installing verified release $VERSION..." >&2
sh "$REPO_DIR/install.sh" "$VERSION"
"$INSTALL_ROOT/current/herdr-mobile-relay" verify-release "$INSTALL_ROOT/current" >/dev/null

# Propagate GH_TOKEN into the service environment for self-update.
if [ -n "${GH_TOKEN:-}" ] && [ -f "$SCRIPT_DIR/common.sh" ]; then
    . "$SCRIPT_DIR/common.sh"
    ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
    ensure_relay_env "$ENV_FILE"
fi

# Cut over an existing service to the new release root.
SERVICE_WRAPPER="$INSTALL_ROOT/current/relay/herdr-mobile-relay-service.sh"
case "$(uname -s)" in
    Linux)
        UNIT_FILE="$HOME/.config/systemd/user/herdr-mobile-relay.service"
        if [ -f "$UNIT_FILE" ] && [ -x "$SERVICE_WRAPPER" ]; then
            echo "herdr-mobile-relay: updating service unit to new release..." >&2
            sed -i "s|^ExecStart=.*|ExecStart=$SERVICE_WRAPPER|" "$UNIT_FILE"
            sed -i "s|^WorkingDirectory=.*|WorkingDirectory=$INSTALL_ROOT/current|" "$UNIT_FILE"
            systemctl --user daemon-reload 2>/dev/null || true
            if systemctl --user is-active --quiet herdr-mobile-relay.service 2>/dev/null; then
                echo "herdr-mobile-relay: restarting existing service..." >&2
                systemctl --user restart herdr-mobile-relay.service
            fi
        elif systemctl --user is-active --quiet herdr-mobile-relay.service 2>/dev/null; then
            echo "herdr-mobile-relay: restarting existing service..." >&2
            systemctl --user restart herdr-mobile-relay.service
        fi
        ;;
    Darwin)
        PLIST="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        if [ -f "$PLIST" ] && [ -x "$SERVICE_WRAPPER" ]; then
            echo "herdr-mobile-relay: updating service plist to new release..." >&2
            sed -i '' "s|<string>.*herdr-mobile-relay-service\.sh</string>|<string>$SERVICE_WRAPPER</string>|" "$PLIST"
            sed -i '' "s|<string>.*herdr-mobile-relay</string>\(.*WorkingDirectory.*\)|<string>$INSTALL_ROOT/current</string>\1|" "$PLIST" 2>/dev/null || true
            if launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay"; then
                echo "herdr-mobile-relay: restarting existing service..." >&2
                launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
            fi
        elif launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay"; then
            echo "herdr-mobile-relay: restarting existing service..." >&2
            launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
        fi
        ;;
esac

echo "" >&2
echo "herdr-mobile-relay: release $VERSION is ready." >&2
echo "herdr-mobile-relay: start setup with:" >&2
echo "  herdr plugin action invoke setup --plugin herdr-mobile-relay.events" >&2

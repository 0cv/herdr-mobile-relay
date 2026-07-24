#!/bin/bash
# Marketplace build hook: install the exact pre-built release named by the
# plugin manifest. End-user hosts never compile Go or install Python/uv.
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
SCRIPT_DIR=$(CDPATH='' cd "$SCRIPT_DIR" && pwd)
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

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
MANIFEST="$INSTALL_ROOT/current/release-manifest.json"
REVISION=$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -1)
WEB_HASH=$(sed -n 's/^[[:space:]]*"web_hash":[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -1)
[ -n "$REVISION" ] && [ -n "$WEB_HASH" ] || {
    echo "herdr-mobile-relay: installed release manifest has no identity" >&2
    exit 1
}

# Store the repository credential separately; the service receives only its
# path, so the relay, cloudflared, and agent subprocesses never inherit it.
ENV_FILE="$(installed_service_env_file)"
if [ -z "$ENV_FILE" ]; then
    ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
fi
if [ -n "${GH_TOKEN:-}" ]; then
    ensure_relay_env "$ENV_FILE"
fi

# Cut over an existing service to the new release root.
SERVICE_WRAPPER="$INSTALL_ROOT/current/relay/herdr-mobile-relay-service.sh"
service_restarted=false
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
                service_restarted=true
            fi
        elif systemctl --user is-active --quiet herdr-mobile-relay.service 2>/dev/null; then
            echo "herdr-mobile-relay: restarting existing service..." >&2
            systemctl --user restart herdr-mobile-relay.service
            service_restarted=true
        fi
        ;;
    Darwin)
        PLIST="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        if [ -f "$PLIST" ] && [ -x "$SERVICE_WRAPPER" ]; then
            echo "herdr-mobile-relay: updating service plist to new release..." >&2
            update_launchd_release_paths "$PLIST" "$SERVICE_WRAPPER" "$INSTALL_ROOT/current"
            if launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay"; then
                echo "herdr-mobile-relay: restarting existing service..." >&2
                launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
                service_restarted=true
            fi
        elif launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay"; then
            echo "herdr-mobile-relay: restarting existing service..." >&2
            launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
            service_restarted=true
        fi
        ;;
esac

if [ "$service_restarted" = true ]; then
    PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
    PORT="${PORT:-8375}"
    echo "herdr-mobile-relay: verifying replacement service identity..." >&2
    if ! HEALTH="$(wait_for_relay_health "$PORT" 30 1)"; then
        echo "herdr-mobile-relay: replacement service did not become healthy" >&2
        exit 1
    fi
    if ! verify_relay_release_health "$HEALTH" "$VERSION" "$REVISION" "$WEB_HASH"; then
        echo "herdr-mobile-relay: replacement service reported the wrong release identity" >&2
        exit 1
    fi
    case "$(uname -s)" in
        Linux)
            systemctl --user is-active --quiet herdr-mobile-relay.service || {
                echo "herdr-mobile-relay: replacement service is not active" >&2
                exit 1
            }
            ;;
        Darwin)
            launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay" || {
                echo "herdr-mobile-relay: replacement service is not loaded" >&2
                exit 1
            }
            ;;
    esac
fi

echo "" >&2
echo "herdr-mobile-relay: release $VERSION is ready." >&2
echo "herdr-mobile-relay: start setup with:" >&2
echo "  herdr plugin action invoke setup --plugin herdr-mobile-relay.events" >&2

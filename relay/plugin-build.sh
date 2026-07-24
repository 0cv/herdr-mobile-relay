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
INSTALLER=${HERDR_PLUGIN_INSTALLER:-"$REPO_DIR/install.sh"}

ENV_FILE="$(installed_service_env_file)"
if [ -z "$ENV_FILE" ]; then
    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        ENV_FILE="$HERDR_RELAY_ENV"
    else
        CONFIG_ROOT=${HERDR_PLUGIN_CONFIG_DIR:-"${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay"}
        ENV_FILE="$CONFIG_ROOT/relay.env"
    fi
fi
HERDR_PLUGIN_CONFIG_DIR=$(dirname "$ENV_FILE")
HERDR_RELAY_ENV="$ENV_FILE"
export INSTALL_ROOT BIN_DIR HERDR_PLUGIN_CONFIG_DIR HERDR_RELAY_ENV

PLATFORM=$(uname -s)
SERVICE_FILE=
SERVICE_BACKUP=
service_was_active=false
case "$PLATFORM" in
    Linux)
        SERVICE_FILE="$HOME/.config/systemd/user/herdr-mobile-relay.service"
        if systemctl --user is-active --quiet herdr-mobile-relay.service 2>/dev/null; then
            service_was_active=true
        fi
        ;;
    Darwin)
        SERVICE_FILE="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        if launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay"; then
            service_was_active=true
        fi
        ;;
esac
if [ -n "$SERVICE_FILE" ] && [ -f "$SERVICE_FILE" ]; then
    SERVICE_BACKUP=$(mktemp "${TMPDIR:-/tmp}/herdr-service.XXXXXX")
    cp "$SERVICE_FILE" "$SERVICE_BACKUP"
fi

PREVIOUS_RELEASE=
PREVIOUS_VERSION=
PREVIOUS_REVISION=
PREVIOUS_WEB_HASH=
if [ -L "$INSTALL_ROOT/current" ]; then
    previous_link=$(readlink "$INSTALL_ROOT/current")
    case "$previous_link" in
        /*) previous_candidate=$previous_link ;;
        *) previous_candidate="$INSTALL_ROOT/$previous_link" ;;
    esac
    if [ -d "$previous_candidate" ]; then
        PREVIOUS_RELEASE=$(CDPATH='' cd "$previous_candidate" && pwd -P)
        previous_manifest="$PREVIOUS_RELEASE/release-manifest.json"
        if [ -f "$previous_manifest" ]; then
            PREVIOUS_VERSION=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
            PREVIOUS_REVISION=$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
            PREVIOUS_WEB_HASH=$(sed -n 's/^[[:space:]]*"web_hash":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
        fi
    fi
fi

rollback_armed=false
rollback_plugin_migration() {
    rollback_armed=false
    echo "herdr-mobile-relay: replacement failed; restoring previous service..." >&2

    if [ -n "$PREVIOUS_RELEASE" ] && [ -d "$PREVIOUS_RELEASE" ]; then
        "$INSTALL_ROOT/current/herdr-mobile-relay" \
            activate-release "$INSTALL_ROOT" "$PREVIOUS_RELEASE" || return 1
    fi
    if [ -n "$SERVICE_BACKUP" ] && [ -n "$SERVICE_FILE" ]; then
        restore_temp="${SERVICE_FILE}.rollback.$$"
        cp "$SERVICE_BACKUP" "$restore_temp" || return 1
        mv -f "$restore_temp" "$SERVICE_FILE" || return 1
    fi

    if [ "$service_was_active" != true ]; then
        echo "herdr-mobile-relay: previous inactive service definition restored." >&2
        return 0
    fi
    case "$PLATFORM" in
        Linux)
            systemctl --user daemon-reload || return 1
            systemctl --user restart herdr-mobile-relay.service || return 1
            ;;
        Darwin)
            label=com.herdr-mobile-relay.service
            launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
            launchctl bootstrap "gui/$(id -u)" "$SERVICE_FILE" || return 1
            launchctl enable "gui/$(id -u)/$label" || return 1
            launchctl kickstart -k "gui/$(id -u)/$label" || return 1
            ;;
    esac

    rollback_port="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
    rollback_port="${rollback_port:-8375}"
    rollback_health="$(wait_for_relay_health "$rollback_port" 30 1)" || return 1
    if [ -n "$PREVIOUS_VERSION" ] && [ -n "$PREVIOUS_REVISION" ] && [ -n "$PREVIOUS_WEB_HASH" ]; then
        verify_relay_release_health \
            "$rollback_health" "$PREVIOUS_VERSION" "$PREVIOUS_REVISION" "$PREVIOUS_WEB_HASH" ||
            return 1
    fi
    case "$PLATFORM" in
        Linux) systemctl --user is-active --quiet herdr-mobile-relay.service || return 1 ;;
        Darwin) launchctl list 2>/dev/null | grep -q "com.herdr-mobile-relay" || return 1 ;;
    esac
    echo "herdr-mobile-relay: previous service recovered successfully." >&2
}

cleanup_plugin_build() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ] && [ "$rollback_armed" = true ]; then
        if ! rollback_plugin_migration; then
            echo "herdr-mobile-relay: ERROR: automatic rollback also failed" >&2
        fi
    fi
    if [ -n "$SERVICE_BACKUP" ]; then
        rm -f "$SERVICE_BACKUP"
    fi
    exit "$status"
}
trap cleanup_plugin_build EXIT

echo "herdr-mobile-relay: installing verified release $VERSION..." >&2
INSTALL_TOKEN=${GH_TOKEN:-}
if [ -z "$INSTALL_TOKEN" ] && [ -f "$ENV_FILE" ]; then
    configured_token_file="$(env_file_value "$ENV_FILE" HERDR_GITHUB_TOKEN_FILE)"
    expected_token_file="$(dirname "$ENV_FILE")/github-token"
    if [ "$configured_token_file" = "$expected_token_file" ] &&
       [ -f "$configured_token_file" ] &&
       [ ! -L "$configured_token_file" ]; then
        case "$(ls -ld "$configured_token_file" | awk '{print $1}')" in
            -rw-------*) ;;
            *) configured_token_file= ;;
        esac
    fi
    if [ -n "$configured_token_file" ]; then
        IFS= read -r INSTALL_TOKEN < "$configured_token_file" || true
    fi
fi
if [ -n "$INSTALL_TOKEN" ]; then
    GH_TOKEN="$INSTALL_TOKEN" sh "$INSTALLER" "$VERSION"
else
    sh "$INSTALLER" "$VERSION"
fi
unset INSTALL_TOKEN
if [ -n "$PREVIOUS_RELEASE" ] || [ -n "$SERVICE_BACKUP" ]; then
    rollback_armed=true
fi
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
if [ -n "${GH_TOKEN:-}" ]; then
    ensure_relay_env "$ENV_FILE"
fi

# Cut over an existing service to the new release root.
SERVICE_WRAPPER="$INSTALL_ROOT/current/relay/herdr-mobile-relay-service.sh"
service_restarted=false
case "$PLATFORM" in
    Linux)
        UNIT_FILE="$SERVICE_FILE"
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
        PLIST="$SERVICE_FILE"
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
    case "$PLATFORM" in
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

rollback_armed=false
echo "" >&2
echo "herdr-mobile-relay: release $VERSION is ready." >&2
echo "herdr-mobile-relay: start setup with:" >&2
echo "  herdr plugin action invoke setup --plugin herdr-mobile-relay.events" >&2

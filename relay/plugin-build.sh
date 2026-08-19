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

require_user_service_context

VERSION=$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml")
[ -n "$VERSION" ] || {
    echo "herdr-mobile-relay: herdr-plugin.toml has no exact version" >&2
    exit 1
}

INSTALL_ROOT=${HERDR_RELEASE_ROOT:-"${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay"}
BIN_DIR=${HERDR_RELAY_BIN_DIR:-"$HOME/.local/bin"}
INSTALLER=${HERDR_PLUGIN_INSTALLER:-"$REPO_DIR/install.sh"}
RELEASE_REPOSITORY=${HERDR_RELEASE_REPOSITORY:-}
if [ -z "$RELEASE_REPOSITORY" ]; then
    RELEASE_REPOSITORY=$(release_repository "$REPO_DIR" || true)
fi
if [ -n "$RELEASE_REPOSITORY" ]; then
    export HERDR_RELEASE_REPOSITORY="$RELEASE_REPOSITORY"
fi
TARGET_CONFIG_ROOT=${HERDR_PLUGIN_CONFIG_DIR:-}
if [ -z "$TARGET_CONFIG_ROOT" ] && command -v herdr >/dev/null 2>&1; then
    TARGET_CONFIG_ROOT="$(herdr plugin config-dir herdr-mobile-relay.events 2>/dev/null || true)"
fi
TARGET_CONFIG_ROOT=${TARGET_CONFIG_ROOT:-"${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay"}
case "$TARGET_CONFIG_ROOT" in
    /*) ;;
    *)
        echo "herdr-mobile-relay: plugin config directory must be absolute: $TARGET_CONFIG_ROOT" >&2
        exit 1
        ;;
esac
TARGET_ENV="$TARGET_CONFIG_ROOT/relay.env"
SOURCE_ENV="$(installed_service_env_file)"
if [ -z "$SOURCE_ENV" ] && [ -n "${HERDR_RELAY_ENV:-}" ] && [ -f "$HERDR_RELAY_ENV" ]; then
    if [ "$(canonical_file_path "$HERDR_RELAY_ENV")" = "$(canonical_file_path "$TARGET_ENV")" ]; then
        SOURCE_ENV="$HERDR_RELAY_ENV"
    fi
fi
ENV_FILE="$TARGET_ENV"
HERDR_PLUGIN_CONFIG_DIR="$TARGET_CONFIG_ROOT"
HERDR_RELAY_ENV="$TARGET_ENV"
export INSTALL_ROOT BIN_DIR HERDR_PLUGIN_CONFIG_DIR HERDR_RELAY_ENV

PLATFORM=$(uname -s)
SERVICE_FILE=
SERVICE_BACKUP=
service_was_active=false
service_should_run=false
recover_broken_service=false
service_cutover_started=false
case "$PLATFORM" in
    Linux)
        SERVICE_FILE="$HOME/.config/systemd/user/herdr-mobile-relay.service"
        case "$(systemctl --user is-active herdr-mobile-relay.service 2>/dev/null || true)" in
            active|activating|reloading) service_was_active=true ;;
        esac
        ;;
    Darwin)
        SERVICE_FILE="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        if launchd_service_loaded \
            "gui/$(id -u)/com.herdr-mobile-relay.service"; then
            service_was_active=true
        fi
        ;;
esac
service_should_run=$service_was_active
if [ -n "$SERVICE_FILE" ] && [ -f "$SERVICE_FILE" ]; then
    SERVICE_BACKUP=$(mktemp "${TMPDIR:-/tmp}/herdr-service.XXXXXX")
    cp "$SERVICE_FILE" "$SERVICE_BACKUP"
fi

validate_migration_source() {
    local source_env="$1"
    local source_canonical
    local service_canonical

    [ -f "$source_env" ] && [ ! -L "$source_env" ] || {
        echo "herdr-mobile-relay: installed service environment is not a regular file: $source_env" >&2
        return 1
    }
    grep -q '^HERDR_RELAY_TOKEN=' "$source_env" &&
        [ -n "$(env_file_value "$source_env" HERDR_RELAY_TOKEN)" ] ||
        {
            echo "herdr-mobile-relay: installed service environment has no relay token" >&2
            return 1
        }
    source_canonical="$(canonical_file_path "$source_env")"
    service_canonical="$(canonical_file_path "$(installed_service_env_file)")"
    [ "$source_canonical" = "$service_canonical" ] || {
        echo "herdr-mobile-relay: installed service environment changed during migration" >&2
        return 1
    }

    case "$PLATFORM" in
        Linux)
            grep -F "Environment=HERDR_RELAY_ENV=$source_env" "$SERVICE_FILE" >/dev/null &&
                grep -E '^ExecStart=.*herdr-(mobile-relay|remote)-service\.sh([[:space:]]|$)' \
                    "$SERVICE_FILE" >/dev/null || {
                    echo "herdr-mobile-relay: refusing to migrate an unrecognized systemd service" >&2
                    return 1
                }
            ;;
        Darwin)
            grep -F '<string>com.herdr-mobile-relay.service</string>' "$SERVICE_FILE" >/dev/null &&
                grep -E '<string>.*herdr-(mobile-relay|remote)-service\.sh</string>' \
                    "$SERVICE_FILE" >/dev/null || {
                    echo "herdr-mobile-relay: refusing to migrate an unrecognized launchd service" >&2
                    return 1
                }
            ;;
    esac
}

recognized_service_definition() {
    case "$PLATFORM" in
        Linux)
            grep -E '^Environment=HERDR_RELAY_ENV=/.+' "$SERVICE_FILE" >/dev/null &&
                grep -E '^ExecStart=.*herdr-(mobile-relay|remote)-service\.sh([[:space:]]|$)' \
                    "$SERVICE_FILE" >/dev/null
            ;;
        Darwin)
            grep -F '<string>com.herdr-mobile-relay.service</string>' "$SERVICE_FILE" >/dev/null &&
                grep -F '<key>HERDR_RELAY_ENV</key>' "$SERVICE_FILE" >/dev/null &&
                grep -E '<string>.*herdr-(mobile-relay|remote)-service\.sh</string>' \
                    "$SERVICE_FILE" >/dev/null
            ;;
        *) return 1 ;;
    esac
}

validate_recovery_config() {
    [ -f "$TARGET_ENV" ] && [ ! -L "$TARGET_ENV" ] || {
        echo "herdr-mobile-relay: persistent relay environment is unavailable: $TARGET_ENV" >&2
        return 1
    }
    grep -q '^HERDR_RELAY_TOKEN=' "$TARGET_ENV" &&
        [ -n "$(env_file_value "$TARGET_ENV" HERDR_RELAY_TOKEN)" ] || {
        echo "herdr-mobile-relay: persistent relay environment has no relay token" >&2
        return 1
    }
}

rewrite_service_release_paths() {
    local service_file="$1"
    local service_wrapper="$2"
    local work_dir="$3"
    local env_file="$4"

    [ -f "$service_file" ] && [ -x "$service_wrapper" ] || return 1
    case "$PLATFORM" in
        Linux)
            sed -i "s|^ExecStart=.*|ExecStart=$service_wrapper|" "$service_file"
            sed -i "s|^WorkingDirectory=.*|WorkingDirectory=$work_dir|" "$service_file"
            sed -i "s|^Environment=HERDR_RELAY_ENV=.*|Environment=HERDR_RELAY_ENV=$env_file|" \
                "$service_file"
            grep -Fx "ExecStart=$service_wrapper" "$service_file" >/dev/null &&
                grep -Fx "WorkingDirectory=$work_dir" "$service_file" >/dev/null &&
                grep -Fx "Environment=HERDR_RELAY_ENV=$env_file" "$service_file" >/dev/null
            ;;
        Darwin)
            update_launchd_release_paths \
                "$service_file" "$service_wrapper" "$work_dir" "$env_file"
            ;;
        *) return 1 ;;
    esac
}

CONFIG_BACKUP=
target_config_existed=false
if [ -e "$TARGET_CONFIG_ROOT" ]; then
    [ -d "$TARGET_CONFIG_ROOT" ] && [ ! -L "$TARGET_CONFIG_ROOT" ] || {
        echo "herdr-mobile-relay: persistent plugin config is not a regular directory: $TARGET_CONFIG_ROOT" >&2
        exit 1
    }
    [ -z "$(find "$TARGET_CONFIG_ROOT" -type l -print -quit)" ] || {
        echo "herdr-mobile-relay: persistent plugin config contains a symlink: $TARGET_CONFIG_ROOT" >&2
        exit 1
    }
    target_config_existed=true
fi
CONFIG_BACKUP=$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-config.XXXXXX")
if [ "$target_config_existed" = true ]; then
    cp -pR "$TARGET_CONFIG_ROOT/." "$CONFIG_BACKUP/"
fi

restore_target_config() {
    if [ -L "$TARGET_CONFIG_ROOT" ]; then
        echo "herdr-mobile-relay: refusing to restore through a symlinked config root" >&2
        return 1
    fi
    rm -rf "$TARGET_CONFIG_ROOT"
    if [ "$target_config_existed" = true ]; then
        mkdir -p "$TARGET_CONFIG_ROOT"
        cp -pR "$CONFIG_BACKUP/." "$TARGET_CONFIG_ROOT/"
    fi
}

copy_migration_entry() {
    local source_path="$1"
    local target_name="$2"
    local target_path="$TARGET_CONFIG_ROOT/$target_name"

    [ -e "$source_path" ] || return 0
    [ ! -L "$source_path" ] || {
        echo "herdr-mobile-relay: refusing symlinked migration source: $source_path" >&2
        return 1
    }
    rm -rf "$target_path"
    cp -pR "$source_path" "$target_path"
}

rewrite_path_prefix() {
    local filename="$1"
    local old_prefix="$2"
    local new_prefix="$3"
    local escaped_old
    local escaped_new
    local temp

    [ -f "$filename" ] || return 0
    escaped_old="$(printf '%s' "$old_prefix" | sed 's/[][\\.^$*+?{}|()]/\\&/g')"
    escaped_new="$(printf '%s' "$new_prefix" | sed 's/[\\&|]/\\&/g')"
    temp="$(mktemp "$(dirname "$filename")/.migration.XXXXXX")"
    sed "s|$escaped_old|$escaped_new|g" "$filename" > "$temp"
    chmod --reference="$filename" "$temp" 2>/dev/null || chmod 600 "$temp"
    mv -f "$temp" "$filename"
}

migrate_source_config() {
    local source_env="$1"
    local source_root
    local cloudflared_config

    source_root="$(dirname "$source_env")"
    if [ "$(canonical_file_path "$source_env")" = "$(canonical_file_path "$TARGET_ENV")" ]; then
        return
    fi
    echo "herdr-mobile-relay: migrating service state into persistent plugin config..." >&2
    mkdir -p "$TARGET_CONFIG_ROOT"
    chmod 700 "$TARGET_CONFIG_ROOT"
    copy_migration_entry "$source_env" relay.env
    copy_migration_entry "$source_root/push" push
    copy_migration_entry "$source_root/phone-app-origin" phone-app-origin
    copy_migration_entry "$source_root/stable-setup.json" stable-setup.json
    copy_migration_entry "$source_root/cloudflared" cloudflared
    copy_migration_entry "$source_root/update-state.json" update-state.json
    copy_migration_entry "$source_root/app-deploy-state.json" app-deploy-state.json
    rewrite_path_prefix "$TARGET_CONFIG_ROOT/stable-setup.json" \
        "$source_env" "$TARGET_ENV"
    rewrite_path_prefix "$TARGET_CONFIG_ROOT/stable-setup.json" \
        "$source_root" "$TARGET_CONFIG_ROOT"
    rewrite_path_prefix "$TARGET_CONFIG_ROOT/cloudflared/config.yml" \
        "$source_root" "$TARGET_CONFIG_ROOT"

    cloudflared_config="$(env_file_value "$TARGET_ENV" CLOUDFLARED_CONFIG)"
    if [ "$cloudflared_config" = "$source_root/cloudflared/config.yml" ]; then
        set_env_value_atomic "$TARGET_ENV" CLOUDFLARED_CONFIG \
            "$TARGET_CONFIG_ROOT/cloudflared/config.yml"
    fi
    chmod 600 "$TARGET_ENV"
}

if [ -n "$SERVICE_BACKUP" ]; then
    source_env_missing=false
    if [ -z "$SOURCE_ENV" ] || [ ! -e "$SOURCE_ENV" ]; then
        source_env_missing=true
    fi
    if [ "$source_env_missing" = true ] && [ -n "$SOURCE_ENV" ] && [ -L "$SOURCE_ENV" ]; then
        source_env_missing=false
    fi

    if [ "$source_env_missing" = true ]; then
        if ! recognized_service_definition; then
            echo "herdr-mobile-relay: refusing to recover an unrecognized service definition" >&2
            rm -rf "$CONFIG_BACKUP"
            rm -f "$SERVICE_BACKUP"
            exit 1
        fi
        if ! validate_recovery_config; then
            rm -rf "$CONFIG_BACKUP"
            rm -f "$SERVICE_BACKUP"
            exit 1
        fi
        echo "herdr-mobile-relay: recovering broken service paths from persistent plugin config..." >&2
        SOURCE_ENV="$TARGET_ENV"
        recover_broken_service=true
        service_should_run=true
    elif ! validate_migration_source "$SOURCE_ENV"; then
        rm -rf "$CONFIG_BACKUP"
        rm -f "$SERVICE_BACKUP"
        exit 1
    fi
    if [ "${HERDR_MOBILE_RELAY_NO_AUTO_SETUP:-}" = 1 ]; then
        service_should_run=true
    fi
fi

PREVIOUS_RELEASE=
PREVIOUS_VERSION=
PREVIOUS_REVISION=
PREVIOUS_WEB_HASH=
current_was_present=false
if [ -e "$INSTALL_ROOT/current" ] || [ -L "$INSTALL_ROOT/current" ]; then
    current_was_present=true
fi
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
    elif [ "$current_was_present" = false ]; then
        rm -f "$INSTALL_ROOT/current"
    fi
    if [ -n "$SERVICE_BACKUP" ] && [ -n "$SERVICE_FILE" ]; then
        restore_temp="${SERVICE_FILE}.rollback.$$"
        cp "$SERVICE_BACKUP" "$restore_temp" || return 1
        mv -f "$restore_temp" "$SERVICE_FILE" || return 1
    fi
    restore_target_config || return 1

    if [ "$recover_broken_service" = true ] &&
       [ "$service_cutover_started" = true ]; then
        rollback_wrapper="$INSTALL_ROOT/current/relay/herdr-mobile-relay-service.sh"
        rewrite_service_release_paths \
            "$SERVICE_FILE" "$rollback_wrapper" "$INSTALL_ROOT/current" "$TARGET_ENV" ||
            return 1
    fi

    if [ "$service_cutover_started" != true ]; then
        echo "herdr-mobile-relay: previous running service was left untouched." >&2
        return 0
    fi
    if [ "$service_should_run" != true ]; then
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
            reload_launchd_service_definition "$SERVICE_FILE" "$label" || return 1
            ;;
    esac

    rollback_env="${SOURCE_ENV:-$ENV_FILE}"
    rollback_port="$(env_file_value "$rollback_env" HERDR_RELAY_PORT)"
    rollback_port="${rollback_port:-8375}"
    if [ -n "$PREVIOUS_VERSION" ] && [ -n "$PREVIOUS_REVISION" ] && [ -n "$PREVIOUS_WEB_HASH" ]; then
        wait_for_relay_release_health \
            "$rollback_port" 30 1 \
            "$PREVIOUS_VERSION" "$PREVIOUS_REVISION" "$PREVIOUS_WEB_HASH" \
            >/dev/null || return 1
    else
        wait_for_relay_health "$rollback_port" 30 1 >/dev/null || return 1
    fi
    case "$PLATFORM" in
        Linux) systemctl --user is-active --quiet herdr-mobile-relay.service || return 1 ;;
        Darwin)
            launchd_service_loaded \
                "gui/$(id -u)/com.herdr-mobile-relay.service" || return 1
            ;;
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
    rm -rf "$CONFIG_BACKUP"
    exit "$status"
}
trap cleanup_plugin_build EXIT

INSTALL_TOKEN=${GH_TOKEN:-}
if [ -z "$INSTALL_TOKEN" ]; then
    for TOKEN_ENV in "$TARGET_ENV" "${SOURCE_ENV:-}"; do
        [ -n "$TOKEN_ENV" ] && [ -f "$TOKEN_ENV" ] || continue
        configured_token_file="$(env_file_value "$TOKEN_ENV" HERDR_GITHUB_TOKEN_FILE)"
        expected_token_file="$(dirname "$TOKEN_ENV")/github-token"
        if [ "$configured_token_file" = "$expected_token_file" ] &&
           [ -f "$configured_token_file" ] &&
           [ ! -L "$configured_token_file" ]; then
            case "$(ls -ld "$configured_token_file" | awk '{print $1}')" in
                -rw-------*) ;;
                *) configured_token_file= ;;
            esac
        else
            configured_token_file=
        fi
        if [ -n "$configured_token_file" ]; then
            IFS= read -r INSTALL_TOKEN < "$configured_token_file" || true
            [ -z "$INSTALL_TOKEN" ] || break
        fi
    done
fi

rollback_armed=true
migrate_source_config "${SOURCE_ENV:-$TARGET_ENV}"

echo "herdr-mobile-relay: installing verified release $VERSION..." >&2
if [ -n "$INSTALL_TOKEN" ]; then
    GH_TOKEN="$INSTALL_TOKEN" sh "$INSTALLER" "$VERSION"
else
    sh "$INSTALLER" "$VERSION"
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
if [ -n "$INSTALL_TOKEN" ]; then
    GH_TOKEN="$INSTALL_TOKEN" ensure_relay_env "$TARGET_ENV"
fi
unset INSTALL_TOKEN

# Cut over an existing service to the new release root.
SERVICE_WRAPPER="$INSTALL_ROOT/current/relay/herdr-mobile-relay-service.sh"
service_restarted=false
case "$PLATFORM" in
    Linux)
        UNIT_FILE="$SERVICE_FILE"
        if [ -f "$UNIT_FILE" ] && [ -x "$SERVICE_WRAPPER" ]; then
            echo "herdr-mobile-relay: updating service unit to new release..." >&2
            service_cutover_started=true
            rewrite_service_release_paths \
                "$UNIT_FILE" "$SERVICE_WRAPPER" "$INSTALL_ROOT/current" "$TARGET_ENV" || {
                echo "herdr-mobile-relay: service unit could not be updated safely" >&2
                exit 1
            }
            systemctl --user daemon-reload 2>/dev/null || true
            if [ "$service_should_run" = true ]; then
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
            service_cutover_started=true
            rewrite_service_release_paths \
                "$PLIST" "$SERVICE_WRAPPER" "$INSTALL_ROOT/current" "$TARGET_ENV" || {
                echo "herdr-mobile-relay: service plist could not be updated safely" >&2
                exit 1
            }
            if [ "$service_should_run" = true ]; then
                echo "herdr-mobile-relay: reloading existing service..." >&2
                reload_launchd_service_definition \
                    "$PLIST" "com.herdr-mobile-relay.service"
                service_restarted=true
            fi
        elif launchd_service_loaded \
            "gui/$(id -u)/com.herdr-mobile-relay.service"; then
            echo "herdr-mobile-relay: restarting existing service..." >&2
            launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
            service_restarted=true
        fi
        ;;
esac

if [ "$service_restarted" = true ]; then
    PORT="$(env_file_value "$TARGET_ENV" HERDR_RELAY_PORT)"
    PORT="${PORT:-8375}"
    echo "herdr-mobile-relay: verifying replacement service identity..." >&2
    if ! wait_for_relay_release_health \
        "$PORT" 30 1 "$VERSION" "$REVISION" "$WEB_HASH" >/dev/null; then
        echo "herdr-mobile-relay: replacement service did not report the expected release identity" >&2
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
            launchd_service_loaded \
                "gui/$(id -u)/com.herdr-mobile-relay.service" || {
                echo "herdr-mobile-relay: replacement service is not loaded" >&2
                exit 1
            }
            ;;
    esac
fi

rollback_armed=false

# A first install has nothing to run yet: a transport still has to be chosen and
# a phone paired, and this script's own output is never shown to the person who
# typed the install command. So open the setup menu instead of describing it.
# An upgrade stays silent, because its relay is already configured. The action
# is invoked detached and after this build exits, since herdr will not open a
# pane for a plugin whose build is still running.
transport_configured() {
    [ -f "$TARGET_ENV" ] || return 1
    [ -z "$(env_file_value "$TARGET_ENV" HERDR_GATEWAY_URL)" ] || return 0
    [ -n "$(env_file_value "$TARGET_ENV" CLOUDFLARED_CONFIG)" ]
}

schedule_setup_menu() {
    [ "${HERDR_MOBILE_RELAY_NO_AUTO_SETUP:-}" != 1 ] || return 0
    [ "$current_was_present" != true ] || return 0
    command -v herdr >/dev/null 2>&1 || return 0
    ! transport_configured || return 0
    (
        sleep 2
        herdr plugin action invoke setup --plugin herdr-mobile-relay.events
    ) >/dev/null 2>&1 &
}

echo "" >&2
echo "herdr-mobile-relay: release $VERSION is ready." >&2
schedule_setup_menu
echo "herdr-mobile-relay: start setup with:" >&2
echo "  herdr plugin action invoke setup --plugin herdr-mobile-relay.events" >&2

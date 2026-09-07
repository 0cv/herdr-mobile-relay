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
    local service_temp

    [ -f "$service_file" ] && [ -x "$service_wrapper" ] || return 1
    case "$PLATFORM" in
        Linux)
            service_temp="$(mktemp "$(dirname "$service_file")/.herdr-service.XXXXXX")" || return 1
            if ! awk -v wrapper="$service_wrapper" -v work="$work_dir" -v env_file="$env_file" '
                /^ExecStart=/ { print "ExecStart=" wrapper; next }
                /^WorkingDirectory=/ { print "WorkingDirectory=" work; next }
                /^Environment=HERDR_RELAY_ENV=/ { print "Environment=HERDR_RELAY_ENV=" env_file; next }
                { print }
            ' "$service_file" > "$service_temp" ||
               ! grep -Fx "ExecStart=$service_wrapper" "$service_temp" >/dev/null ||
               ! grep -Fx "WorkingDirectory=$work_dir" "$service_temp" >/dev/null ||
               ! grep -Fx "Environment=HERDR_RELAY_ENV=$env_file" "$service_temp" >/dev/null; then
                rm -f "$service_temp"
                return 1
            fi
            chmod --reference="$service_file" "$service_temp" 2>/dev/null || chmod 600 "$service_temp"
            mv -f "$service_temp" "$service_file"
            ;;
        Darwin)
            update_launchd_release_paths \
                "$service_file" "$service_wrapper" "$work_dir" "$env_file"
            ;;
        *) return 1 ;;
    esac
}

TARGET_CONFIG_PARENT=$(dirname "$TARGET_CONFIG_ROOT")
mkdir -p "$TARGET_CONFIG_PARENT"
CONFIG_BACKUP=
CONFIG_QUARANTINE=
CONFIG_RESTORE_STAGE=
MIGRATION_STAGE=
RELEASE_DOWNLOAD_DIR=
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
CONFIG_BACKUP=$(mktemp -d "$TARGET_CONFIG_PARENT/.herdr-plugin-config-backup.XXXXXX")
chmod 700 "$CONFIG_BACKUP"
if [ "$target_config_existed" = true ]; then
    cp -pR "$TARGET_CONFIG_ROOT/." "$CONFIG_BACKUP/"
fi

symlink_free_config_tree() {
    local root="$1"
    [ -d "$root" ] && [ ! -L "$root" ] &&
        [ -z "$(find "$root" -type l -print -quit)" ]
}

restore_target_config() {
    local target_moved=false

    if [ -L "$TARGET_CONFIG_ROOT" ]; then
        echo "herdr-mobile-relay: refusing to restore through a symlinked config root" >&2
        return 1
    fi
    CONFIG_RESTORE_STAGE=$(mktemp -d "$TARGET_CONFIG_PARENT/.herdr-plugin-config-restore.XXXXXX") || return 1
    chmod 700 "$CONFIG_RESTORE_STAGE" || return 1
    if [ "$target_config_existed" = true ]; then
        cp -pR "$CONFIG_BACKUP/." "$CONFIG_RESTORE_STAGE/" || return 1
        symlink_free_config_tree "$CONFIG_RESTORE_STAGE" || return 1
    fi

    CONFIG_QUARANTINE=$(mktemp -d "$TARGET_CONFIG_PARENT/.herdr-plugin-config-quarantine.XXXXXX") || return 1
    rmdir "$CONFIG_QUARANTINE" || return 1
    if [ -e "$TARGET_CONFIG_ROOT" ] || [ -L "$TARGET_CONFIG_ROOT" ]; then
        mv "$TARGET_CONFIG_ROOT" "$CONFIG_QUARANTINE" || return 1
        target_moved=true
    fi
    if [ "$target_config_existed" = true ]; then
        if ! mv "$CONFIG_RESTORE_STAGE" "$TARGET_CONFIG_ROOT"; then
            [ "$target_moved" = false ] || mv "$CONFIG_QUARANTINE" "$TARGET_CONFIG_ROOT"
            return 1
        fi
        CONFIG_RESTORE_STAGE=
    else
        rmdir "$CONFIG_RESTORE_STAGE" || return 1
        CONFIG_RESTORE_STAGE=
    fi
    [ "$target_moved" = false ] || rm -rf "$CONFIG_QUARANTINE"
    CONFIG_QUARANTINE=
}

copy_migration_entry() {
    local source_path="$1"
    local target_name="$2"
    local target_path="$MIGRATION_STAGE/$target_name"

    [ -e "$source_path" ] || return 0
    [ ! -L "$source_path" ] &&
        { [ -f "$source_path" ] || [ -d "$source_path" ]; } &&
        { [ ! -d "$source_path" ] || [ -z "$(find "$source_path" -type l -print -quit)" ]; } || {
        echo "herdr-mobile-relay: refusing symlinked migration source: $source_path" >&2
        return 1
    }
    rm -rf "$target_path"
    cp -pR "$source_path" "$target_path" || return 1
    if [ -d "$target_path" ]; then
        [ -z "$(find "$target_path" -type l -print -quit)" ] || return 1
    else
        [ -f "$target_path" ] && [ ! -L "$target_path" ]
    fi
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
    local migration_quarantine

    source_root="$(dirname "$source_env")"
    if [ "$(canonical_file_path "$source_env")" = "$(canonical_file_path "$TARGET_ENV")" ]; then
        return
    fi
    echo "herdr-mobile-relay: migrating service state into persistent plugin config..." >&2
    MIGRATION_STAGE=$(mktemp -d "$TARGET_CONFIG_PARENT/.herdr-plugin-config-migration.XXXXXX")
    chmod 700 "$MIGRATION_STAGE"
    if [ "$target_config_existed" = true ]; then
        cp -pR "$TARGET_CONFIG_ROOT/." "$MIGRATION_STAGE/"
    fi
    copy_migration_entry "$source_env" relay.env
    copy_migration_entry "$source_root/device-auth" device-auth
    copy_migration_entry "$source_root/push" push
    copy_migration_entry "$source_root/phone-app-origin" phone-app-origin
    copy_migration_entry "$source_root/phone-app-origin-configured" phone-app-origin-configured
    copy_migration_entry "$source_root/stable-setup.json" stable-setup.json
    copy_migration_entry "$source_root/cloudflared" cloudflared
    copy_migration_entry "$source_root/update-state.json" update-state.json
    copy_migration_entry "$source_root/app-deploy-state.json" app-deploy-state.json
    copy_migration_entry "$source_root/pane-profile-associations.json" pane-profile-associations.json
    symlink_free_config_tree "$MIGRATION_STAGE" || {
        echo "herdr-mobile-relay: staged migration contains a symlink" >&2
        return 1
    }
    rewrite_path_prefix "$MIGRATION_STAGE/stable-setup.json" \
        "$source_env" "$TARGET_ENV"
    rewrite_path_prefix "$MIGRATION_STAGE/stable-setup.json" \
        "$source_root" "$TARGET_CONFIG_ROOT"
    rewrite_path_prefix "$MIGRATION_STAGE/cloudflared/config.yml" \
        "$source_root" "$TARGET_CONFIG_ROOT"

    cloudflared_config="$(env_file_value "$MIGRATION_STAGE/relay.env" CLOUDFLARED_CONFIG)"
    if [ "$cloudflared_config" = "$source_root/cloudflared/config.yml" ]; then
        set_env_value_atomic "$MIGRATION_STAGE/relay.env" CLOUDFLARED_CONFIG \
            "$TARGET_CONFIG_ROOT/cloudflared/config.yml"
    fi
    sanitize_relay_runtime_env "$MIGRATION_STAGE/relay.env"
    chmod 600 "$MIGRATION_STAGE/relay.env"
    symlink_free_config_tree "$MIGRATION_STAGE" || return 1

    migration_quarantine=$(mktemp -d "$TARGET_CONFIG_PARENT/.herdr-plugin-config-migration-old.XXXXXX")
    rmdir "$migration_quarantine"
    if [ -e "$TARGET_CONFIG_ROOT" ] || [ -L "$TARGET_CONFIG_ROOT" ]; then
        mv "$TARGET_CONFIG_ROOT" "$migration_quarantine"
    else
        migration_quarantine=
    fi
    if ! mv "$MIGRATION_STAGE" "$TARGET_CONFIG_ROOT"; then
        [ -z "$migration_quarantine" ] || mv "$migration_quarantine" "$TARGET_CONFIG_ROOT"
        return 1
    fi
    MIGRATION_STAGE=
    [ -z "$migration_quarantine" ] || rm -rf "$migration_quarantine"
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
        releases_root=$(CDPATH='' cd "$INSTALL_ROOT/releases" 2>/dev/null && pwd -P || true)
        if [ -n "$releases_root" ]; then
            case "$PREVIOUS_RELEASE" in
                "$releases_root"/*) ;;
                *) PREVIOUS_RELEASE= ;;
            esac
        else
            PREVIOUS_RELEASE=
        fi
        previous_manifest="$PREVIOUS_RELEASE/release-manifest.json"
        if [ -n "$PREVIOUS_RELEASE" ] && [ -f "$previous_manifest" ] && [ ! -L "$previous_manifest" ] &&
           [ -x "$PREVIOUS_RELEASE/herdr-mobile-relay" ] && [ ! -L "$PREVIOUS_RELEASE/herdr-mobile-relay" ] &&
           "$PREVIOUS_RELEASE/herdr-mobile-relay" verify-release "$PREVIOUS_RELEASE" >/dev/null; then
            PREVIOUS_VERSION=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
            PREVIOUS_REVISION=$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
            PREVIOUS_WEB_HASH=$(sed -n 's/^[[:space:]]*"web_hash":[[:space:]]*"\([^"]*\)".*/\1/p' "$previous_manifest" | head -1)
        else
            PREVIOUS_RELEASE=
        fi
    fi
fi

SUPERVISOR_STATE_PATH=
SUPERVISOR_STATE_BACKUP=
supervisor_state_existed=false
snapshot_supervisor_state() {
    local state_env="${SOURCE_ENV:-$TARGET_ENV}"
    local state_root

    state_root="$(env_file_value "$state_env" HERDR_RELAY_SUPERVISOR_STATE_DIR)"
    state_root="${state_root:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay}"
    case "$state_root" in
        /*) ;;
        *) echo "herdr-mobile-relay: supervisor state directory must be absolute" >&2; return 1 ;;
    esac
    SUPERVISOR_STATE_PATH="$state_root/supervisor.json"
    if [ -e "$SUPERVISOR_STATE_PATH" ] || [ -L "$SUPERVISOR_STATE_PATH" ]; then
        [ -f "$SUPERVISOR_STATE_PATH" ] && [ ! -L "$SUPERVISOR_STATE_PATH" ] || {
            echo "herdr-mobile-relay: supervisor state is not a safe regular file" >&2
            return 1
        }
        SUPERVISOR_STATE_BACKUP=$(mktemp "${TMPDIR:-/tmp}/herdr-supervisor-state.XXXXXX")
        cp -p "$SUPERVISOR_STATE_PATH" "$SUPERVISOR_STATE_BACKUP"
        supervisor_state_existed=true
    fi
}

restore_or_reset_supervisor_state() {
    local state_directory
    local state_temp

    [ -n "$SUPERVISOR_STATE_PATH" ] || return 0
    if [ -n "$PREVIOUS_RELEASE" ] &&
       "$PREVIOUS_RELEASE/herdr-mobile-relay" supervisor-reset "$SUPERVISOR_STATE_PATH" >/dev/null 2>&1; then
        return 0
    fi
    if [ "$supervisor_state_existed" = true ]; then
        state_directory=$(dirname "$SUPERVISOR_STATE_PATH")
        mkdir -p "$state_directory" || return 1
        state_temp=$(mktemp "$state_directory/.supervisor-rollback.XXXXXX") || return 1
        cp -p "$SUPERVISOR_STATE_BACKUP" "$state_temp" || return 1
        mv -f "$state_temp" "$SUPERVISOR_STATE_PATH" || return 1
    else
        rm -f "$SUPERVISOR_STATE_PATH" || return 1
    fi
}

rollback_armed=false
rollback_plugin_migration() {
    rollback_armed=false
    echo "herdr-mobile-relay: replacement failed; restoring previous service..." >&2

    if [ -n "$PREVIOUS_RELEASE" ] && [ -d "$PREVIOUS_RELEASE" ]; then
        "$PREVIOUS_RELEASE/herdr-mobile-relay" \
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
    restore_or_reset_supervisor_state || return 1

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
    [ -n "$PREVIOUS_VERSION" ] && [ -n "$PREVIOUS_REVISION" ] && [ -n "$PREVIOUS_WEB_HASH" ] || return 1
    (
        load_relay_env "$rollback_env"
        unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN
        HERDR_RELEASE_ROOT="$INSTALL_ROOT"
        rollback_readiness_config="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
        wait_for_installed_relay_ready "$rollback_readiness_config" 30 1
    ) >/dev/null || return 1
    wait_for_relay_release_health \
        "$rollback_port" 30 1 \
        "$PREVIOUS_VERSION" "$PREVIOUS_REVISION" "$PREVIOUS_WEB_HASH" \
        >/dev/null || return 1
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
    rollback_failed=false
    if [ "$status" -ne 0 ] && [ "$rollback_armed" = true ]; then
        if ! rollback_plugin_migration; then
            rollback_failed=true
            echo "herdr-mobile-relay: ERROR: automatic rollback also failed" >&2
        fi
    fi
    if [ "$rollback_failed" = false ]; then
        [ -z "$SERVICE_BACKUP" ] || rm -f "$SERVICE_BACKUP"
        [ -z "$SUPERVISOR_STATE_BACKUP" ] || rm -f "$SUPERVISOR_STATE_BACKUP"
        [ -z "$CONFIG_RESTORE_STAGE" ] || rm -rf "$CONFIG_RESTORE_STAGE"
        [ -z "$CONFIG_QUARANTINE" ] || rm -rf "$CONFIG_QUARANTINE"
        [ -z "$MIGRATION_STAGE" ] || rm -rf "$MIGRATION_STAGE"
        rm -rf "$CONFIG_BACKUP"
    else
        echo "herdr-mobile-relay: rollback recovery data retained at $CONFIG_BACKUP" >&2
        [ -z "$SERVICE_BACKUP" ] || echo "herdr-mobile-relay: service backup retained at $SERVICE_BACKUP" >&2
        [ -z "$CONFIG_QUARANTINE" ] || echo "herdr-mobile-relay: config quarantine retained at $CONFIG_QUARANTINE" >&2
    fi
    [ -z "$RELEASE_DOWNLOAD_DIR" ] || rm -rf "$RELEASE_DOWNLOAD_DIR"
    exit "$status"
}
trap cleanup_plugin_build EXIT

# A private plugin may clone through SSH while its release API still requires an
# HTTPS token. Reuse an existing gh login when no explicit or plugin-configured
# token exists. An SSH key cannot be converted into an API credential.
gh_release_token() {
    command -v gh >/dev/null 2>&1 || return 1
    gh auth token --hostname github.com 2>/dev/null
}

release_asset_url() {
    local release_json="$1"
    local asset_name="$2"

    printf '%s' "$release_json" |
        tr -d '\n\r\t ' |
        sed 's/"url":"/\
"url":"/g' |
        awk -v name="\"name\":\"$asset_name\"" '
            index($0, name) == 0 { next }
            {
                line = $0
                sub(/^"url":"/, "", line)
                sub(/".*$/, "", line)
                print line
                exit
            }
        '
}

release_tag_revision() {
    printf '%s' "$1" |
        tr -d '\n\r\t ' |
        sed 's/"sha":"/\
"sha":"/' |
        sed -n 's/^"sha":"\([0-9a-fA-F][0-9a-fA-F]*\)".*/\1/p' |
        head -1
}

release_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

authenticated_release_curl() {
    local accept="$1"
    shift

    case "$INSTALL_TOKEN" in
        ''|*[!A-Za-z0-9_.-]*)
            echo "herdr-mobile-relay: GitHub release token contains unsupported characters" >&2
            return 1
            ;;
    esac
    env -u GH_TOKEN -u GITHUB_TOKEN -u HERDR_GITHUB_TOKEN_FILE \
        curl --config /dev/fd/3 "$@" 3<<EOF
header = "Authorization: token ${INSTALL_TOKEN}"
header = "Accept: ${accept}"
EOF
}

prepare_authenticated_offline_release() {
    local repository="${RELEASE_REPOSITORY:-0cv/herdr-mobile-relay}"
    local release_os
    local release_arch
    local archive_name
    local commit_json_path
    local release_json_path
    local commit_json
    local release_json
    local archive_url
    local checksums_url

    command -v curl >/dev/null 2>&1 || {
        echo "herdr-mobile-relay: curl is required for authenticated release downloads" >&2
        return 1
    }
    case "$PLATFORM" in
        Darwin) release_os=darwin ;;
        Linux) release_os=linux ;;
        *) echo "herdr-mobile-relay: unsupported release platform: $PLATFORM" >&2; return 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) release_arch=amd64 ;;
        arm64|aarch64) release_arch=arm64 ;;
        *) echo "herdr-mobile-relay: unsupported release architecture: $(uname -m)" >&2; return 1 ;;
    esac
    archive_name="herdr-mobile-relay_${VERSION}_${release_os}_${release_arch}.tar.gz"
    RELEASE_DOWNLOAD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-release.XXXXXX")" || return 1
    chmod 700 "$RELEASE_DOWNLOAD_DIR"
    commit_json_path="$RELEASE_DOWNLOAD_DIR/commit.json"
    release_json_path="$RELEASE_DOWNLOAD_DIR/release.json"
    OFFLINE_RELEASE_ARCHIVE="$RELEASE_DOWNLOAD_DIR/$archive_name"
    OFFLINE_RELEASE_CHECKSUMS="$RELEASE_DOWNLOAD_DIR/checksums.txt"

    authenticated_release_curl application/vnd.github+json \
        --fail --show-error --silent --location --connect-timeout 10 --max-time 120 \
        --output "$commit_json_path" --url "https://api.github.com/repos/$repository/commits/v$VERSION" || return 1
    authenticated_release_curl application/vnd.github+json \
        --fail --show-error --silent --location --connect-timeout 10 --max-time 120 \
        --output "$release_json_path" --url "https://api.github.com/repos/$repository/releases/tags/v$VERSION" || return 1
    commit_json="$(awk '{ printf "%s", $0 }' "$commit_json_path")"
    release_json="$(awk '{ printf "%s", $0 }' "$release_json_path")"
    OFFLINE_EXPECTED_REVISION="$(release_tag_revision "$commit_json")"
    archive_url="$(release_asset_url "$release_json" "$archive_name")"
    checksums_url="$(release_asset_url "$release_json" checksums.txt)"
    [ -n "$OFFLINE_EXPECTED_REVISION" ] && [ -n "$archive_url" ] && [ -n "$checksums_url" ] || {
        echo "herdr-mobile-relay: authenticated release metadata is incomplete" >&2
        return 1
    }
    authenticated_release_curl application/octet-stream \
        --fail --show-error --silent --location --connect-timeout 10 --max-time 120 \
        --output "$OFFLINE_RELEASE_CHECKSUMS" --url "$checksums_url" || return 1
    authenticated_release_curl application/octet-stream \
        --fail --show-error --silent --location --connect-timeout 10 --max-time 120 \
        --output "$OFFLINE_RELEASE_ARCHIVE" --url "$archive_url" || return 1
    OFFLINE_EXPECTED_ARCHIVE_SHA256="$(release_sha256 "$OFFLINE_RELEASE_ARCHIVE")"
}

release_api_available_without_token() {
    command -v curl >/dev/null 2>&1 || return 0
    curl --fail --silent --show-error --location \
        --connect-timeout 5 --max-time 10 \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/$RELEASE_REPOSITORY" >/dev/null 2>&1
}

explain_missing_release_auth() {
    echo "herdr-mobile-relay: cannot access release repository $RELEASE_REPOSITORY through GitHub's HTTPS API." >&2
    echo "herdr-mobile-relay: SSH access cloned the plugin source, but SSH keys do not authorize private release downloads." >&2
    echo "" >&2
    if ! command -v gh >/dev/null 2>&1; then
        case "$(uname -s)" in
            Darwin)
                if command -v brew >/dev/null 2>&1; then
                    echo "Install GitHub CLI:" >&2
                    echo "  brew install gh" >&2
                else
                    echo "Install GitHub CLI from https://cli.github.com/" >&2
                fi
                ;;
            *) echo "Install GitHub CLI from https://cli.github.com/" >&2 ;;
        esac
    fi
    echo "Authorize release access while keeping Git over SSH:" >&2
    echo "  gh auth login --hostname github.com --git-protocol ssh" >&2
    echo "Then rerun the same 'herdr plugin install' command." >&2
    echo "Alternatively, set GH_TOKEN to a token with Contents read access." >&2
}

INSTALL_TOKEN=${GH_TOKEN:-${GITHUB_TOKEN:-}}
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN
if [ -z "$INSTALL_TOKEN" ] &&
   [ -n "$RELEASE_REPOSITORY" ] &&
   [ "$RELEASE_REPOSITORY" != "0cv/herdr-mobile-relay" ]; then
    INSTALL_TOKEN="$(gh_release_token || true)"
fi
if [ -z "$INSTALL_TOKEN" ] &&
   [ -n "$RELEASE_REPOSITORY" ] &&
   [ "$RELEASE_REPOSITORY" != "0cv/herdr-mobile-relay" ] &&
   ! release_api_available_without_token; then
    explain_missing_release_auth
    exit 1
fi
OFFLINE_RELEASE_ARCHIVE=
OFFLINE_RELEASE_CHECKSUMS=
OFFLINE_EXPECTED_REVISION=
OFFLINE_EXPECTED_ARCHIVE_SHA256=
if [ -n "$INSTALL_TOKEN" ]; then
    prepare_authenticated_offline_release || {
        echo "herdr-mobile-relay: could not stage the authenticated release for credential-free installation" >&2
        exit 1
    }
fi
unset INSTALL_TOKEN

snapshot_supervisor_state
rollback_armed=true
migrate_source_config "${SOURCE_ENV:-$TARGET_ENV}"
sanitize_relay_runtime_env "$TARGET_ENV"

echo "herdr-mobile-relay: installing verified release $VERSION..." >&2
if [ -n "$OFFLINE_RELEASE_ARCHIVE" ]; then
    env -u GH_TOKEN -u GITHUB_TOKEN -u HERDR_GITHUB_TOKEN_FILE \
        HERDR_RELEASE_ARCHIVE="$OFFLINE_RELEASE_ARCHIVE" \
        HERDR_RELEASE_CHECKSUMS="$OFFLINE_RELEASE_CHECKSUMS" \
        HERDR_EXPECTED_REVISION="$OFFLINE_EXPECTED_REVISION" \
        HERDR_EXPECTED_ARCHIVE_SHA256="$OFFLINE_EXPECTED_ARCHIVE_SHA256" \
        sh "$INSTALLER" "$VERSION"
else
    env -u GH_TOKEN -u GITHUB_TOKEN -u HERDR_GITHUB_TOKEN_FILE \
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

ensure_relay_env "$TARGET_ENV"

verify_current_service_ready() {
    (
        load_relay_env "$TARGET_ENV"
        HERDR_RELEASE_ROOT="$INSTALL_ROOT"
        readiness_config="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
        wait_for_installed_relay_ready "$readiness_config" 30 1
    )
}

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
    echo "herdr-mobile-relay: verifying exact replacement service readiness..." >&2
    if ! verify_current_service_ready >/dev/null; then
        echo "herdr-mobile-relay: replacement service did not prove its live supervisor, public route, release identity, and exact inventory" >&2
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

# Nobody sees this script's output, so an install that only prints "release is
# ready" leaves a person with no idea what exists or what is still missing. The
# menu answers both and costs one keystroke to leave, so every install opens it,
# upgrades included. The action is invoked detached and after this build exits,
# since herdr will not open a pane for a plugin whose build is still running.
schedule_setup_menu() {
    [ "${HERDR_MOBILE_RELAY_NO_AUTO_SETUP:-}" != 1 ] || return 0
    command -v herdr >/dev/null 2>&1 || return 0
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

#!/bin/bash
set -euo pipefail

LABEL="com.herdr-mobile-relay.service"
LEGACY_LABEL="com.herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LEGACY_PLIST="$HOME/Library/LaunchAgents/$LEGACY_LABEL.plist"
SERVICE_TARGET="gui/$(id -u)/$LABEL"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_user_service_context

if [ -n "${HERDR_RELAY_ENV:-}" ]; then
    ENV_FILE="$HERDR_RELAY_ENV"
else
    ENV_FILE="${HERDR_PLUGIN_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay}/relay.env"
fi
ENV_DIR="$(dirname "$ENV_FILE")"
TOKEN_FILE="$ENV_DIR/github-token"
if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
    [ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] || {
        echo "Refusing unsafe Relay environment file: $ENV_FILE" >&2
        exit 1
    }
fi

CONFIGURED_CLOUDFLARED_CONFIG="$(env_file_value "$ENV_FILE" CLOUDFLARED_CONFIG)"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-${CONFIGURED_CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}}"
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN

if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
    echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
    echo "Create it first, or set CLOUDFLARED_CONFIG before running this installer."
    exit 1
fi

RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
WORK_DIR="$RELEASE_ROOT/current"

ENV_EXISTED=false
ENV_BACKUP=
TOKEN_EXISTED=false
TOKEN_BACKUP=
PLIST_EXISTED=false
PLIST_BACKUP=
PLIST_TEMP=
ENV_RESTORE_TEMP=
TOKEN_RESTORE_TEMP=
SERVICE_WAS_LOADED=false
service_transaction_changed=false
definition_replaced=false
rollback_failed=false
PREVIOUS_CLOUDFLARED_CONFIG=
PREVIOUS_LOCAL_HEALTH=
PREVIOUS_PUBLIC_HEALTH=
PREVIOUS_IDENTITY=

restore_relay_config() {
    if [ "$TOKEN_EXISTED" = true ]; then
        TOKEN_RESTORE_TEMP="$(mktemp "$ENV_DIR/.github-token.rollback.XXXXXX")" || return 1
        cp -p "$TOKEN_BACKUP" "$TOKEN_RESTORE_TEMP" || return 1
        chmod 600 "$TOKEN_RESTORE_TEMP" || return 1
        mv -f "$TOKEN_RESTORE_TEMP" "$TOKEN_FILE" || return 1
        TOKEN_RESTORE_TEMP=
    else
        rm -f "$TOKEN_FILE" || return 1
    fi
    if [ "$ENV_EXISTED" = true ]; then
        ENV_RESTORE_TEMP="$(mktemp "$ENV_DIR/.relay-env.rollback.XXXXXX")" || return 1
        cp -p "$ENV_BACKUP" "$ENV_RESTORE_TEMP" || return 1
        chmod 600 "$ENV_RESTORE_TEMP" || return 1
        mv -f "$ENV_RESTORE_TEMP" "$ENV_FILE" || return 1
        ENV_RESTORE_TEMP=
    else
        rm -f "$ENV_FILE" || return 1
    fi
}

capture_previous_service_readiness() {
    local host
    local port
    local public_ready
    local public_host
    local identity

    [ "$ENV_EXISTED" = true ] || return 1
    (
        load_relay_env "$ENV_FILE"
        unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN
        host="${HERDR_RELAY_HOST:-127.0.0.1}"
        port="${HERDR_RELAY_PORT:-8375}"
        case "$host" in
            127.0.0.1|localhost) local_health="http://$host:$port/healthz" ;;
            ::1) local_health="http://[::1]:$port/healthz" ;;
            *) return 1 ;;
        esac
        previous_config="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
        public_ready="${HERDR_RELAY_PUBLIC_HEALTH_URL:-}"
        if [ -z "$public_ready" ]; then
            public_host="$(yaml_scalar hostname "$previous_config")"
            valid_hostname "$public_host" || return 1
            public_ready="https://$public_host/readyz"
        fi
        case "$public_ready" in
            https://*/readyz) public_health="${public_ready%/readyz}/healthz" ;;
            https://*/healthz) public_health="$public_ready" ;;
            *) return 1 ;;
        esac
        identity="$(relay_release_identity_at_endpoints "$local_health" "$public_health")" || return 1
        printf '%s\n%s\n%s\n%s\n' "$previous_config" "$local_health" "$public_health" "$identity"
    )
}

prove_previous_service_readiness() {
    local attempt
    local identity

    [ "$SERVICE_WAS_LOADED" = true ] || return 0
    (
        load_relay_env "$ENV_FILE"
        unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" wait_for_installed_relay_ready "$PREVIOUS_CLOUDFLARED_CONFIG" 30 1
    ) >/dev/null || return 1
    for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
        identity="$(relay_release_identity_at_endpoints "$PREVIOUS_LOCAL_HEALTH" "$PREVIOUS_PUBLIC_HEALTH")" || true
        if [ "$identity" = "$PREVIOUS_IDENTITY" ]; then
            return 0
        fi
        [ "$attempt" -eq 30 ] || sleep 1
    done
    echo "Could not prove the restored launchd service's exact local and public release identity." >&2
    return 1
}

rollback_launchd_install() {
    local restore_temp

    service_transaction_changed=false
    restore_relay_config || return 1
    if [ "$definition_replaced" = true ]; then
        if [ "$SERVICE_WAS_LOADED" = true ] && [ "$PLIST_EXISTED" = true ]; then
            restore_temp="$(mktemp "$(dirname "$PLIST")/.${LABEL}.restore.XXXXXX")" || return 1
            if ! cp -p "$PLIST_BACKUP" "$restore_temp"; then
                rm -f "$restore_temp"
                return 1
            fi
            if ! mv -f "$restore_temp" "$PLIST"; then
                rm -f "$restore_temp"
                return 1
            fi
            reload_launchd_service_definition "$PLIST" "$LABEL" || return 1
            launchd_service_loaded "$SERVICE_TARGET" || {
                echo "Could not prove the restored launchd service is loaded: $SERVICE_TARGET" >&2
                return 1
            }
        else
            launchctl bootout "$SERVICE_TARGET" >/dev/null 2>&1 || true
            if launchd_service_loaded "$SERVICE_TARGET"; then
                echo "Could not prove the replacement launchd service is absent: $SERVICE_TARGET" >&2
                return 1
            fi
            if [ "$PLIST_EXISTED" = true ]; then
                restore_temp="$(mktemp "$(dirname "$PLIST")/.${LABEL}.restore.XXXXXX")" || return 1
                if ! cp -p "$PLIST_BACKUP" "$restore_temp"; then
                    rm -f "$restore_temp"
                    return 1
                fi
                if ! mv -f "$restore_temp" "$PLIST"; then
                    rm -f "$restore_temp"
                    return 1
                fi
            else
                rm -f "$PLIST" || return 1
            fi
        fi
    fi
    prove_previous_service_readiness || return 1
    echo "Restored the previous launchd service environment, definition, state, and exact live identity." >&2
}

cleanup_launchd_install() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ] && [ "$service_transaction_changed" = true ]; then
        if ! rollback_launchd_install; then
            rollback_failed=true
            echo "ERROR: launchd service rollback failed; recovery data retained." >&2
            [ -z "$ENV_BACKUP" ] || echo "Relay environment backup: $ENV_BACKUP" >&2
            if [ -n "$PLIST_BACKUP" ]; then
                echo "Launchd definition backup: $PLIST_BACKUP" >&2
            elif [ "$definition_replaced" = true ]; then
                echo "Launchd replacement definition: $PLIST" >&2
            fi
        fi
    fi
    [ -z "$PLIST_TEMP" ] || rm -f "$PLIST_TEMP"
    [ -z "$ENV_RESTORE_TEMP" ] || rm -f "$ENV_RESTORE_TEMP"
    [ -z "$TOKEN_RESTORE_TEMP" ] || rm -f "$TOKEN_RESTORE_TEMP"
    if [ "$rollback_failed" = false ]; then
        [ -z "$ENV_BACKUP" ] || rm -f "$ENV_BACKUP"
        [ -z "$TOKEN_BACKUP" ] || rm -f "$TOKEN_BACKUP"
        [ -z "$PLIST_BACKUP" ] || rm -f "$PLIST_BACKUP"
    fi
    exit "$status"
}
trap cleanup_launchd_install EXIT

if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
    [ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] || {
        echo "Refusing unsafe Relay environment file: $ENV_FILE" >&2
        exit 1
    }
    ENV_EXISTED=true
    ENV_BACKUP="$(mktemp "$ENV_DIR/.relay-env.backup.XXXXXX")"
    cp -p "$ENV_FILE" "$ENV_BACKUP"
    chmod 600 "$ENV_BACKUP"
fi
if [ -e "$TOKEN_FILE" ] || [ -L "$TOKEN_FILE" ]; then
    [ -f "$TOKEN_FILE" ] && [ ! -L "$TOKEN_FILE" ] || {
        echo "Refusing unsafe legacy GitHub token file: $TOKEN_FILE" >&2
        exit 1
    }
    TOKEN_EXISTED=true
    TOKEN_BACKUP="$(mktemp "$ENV_DIR/.github-token.backup.XXXXXX")"
    cp -p "$TOKEN_FILE" "$TOKEN_BACKUP"
    chmod 600 "$TOKEN_BACKUP"
fi

if [ -e "$PLIST" ] || [ -L "$PLIST" ]; then
    [ -f "$PLIST" ] && [ ! -L "$PLIST" ] || {
        echo "Refusing to replace unsafe launchd definition: $PLIST" >&2
        exit 1
    }
    PLIST_EXISTED=true
    PLIST_BACKUP="$(mktemp "$(dirname "$PLIST")/.${LABEL}.backup.XXXXXX")"
    cp -p "$PLIST" "$PLIST_BACKUP"
fi
if launchd_service_loaded "$SERVICE_TARGET"; then
    SERVICE_WAS_LOADED=true
    [ "$PLIST_EXISTED" = true ] || {
        echo "Refusing to replace a loaded launchd service without its definition: $SERVICE_TARGET" >&2
        exit 1
    }
    previous_readiness="$(capture_previous_service_readiness)" || {
        echo "Could not snapshot the existing launchd service's exact local and public release identity." >&2
        exit 1
    }
    PREVIOUS_CLOUDFLARED_CONFIG="$(printf '%s\n' "$previous_readiness" | sed -n '1p')"
    PREVIOUS_LOCAL_HEALTH="$(printf '%s\n' "$previous_readiness" | sed -n '2p')"
    PREVIOUS_PUBLIC_HEALTH="$(printf '%s\n' "$previous_readiness" | sed -n '3p')"
    PREVIOUS_IDENTITY="$(printf '%s\n' "$previous_readiness" | sed -n '4,6p')"
fi

service_transaction_changed=true
mkdir -p "$ENV_DIR"
ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
load_relay_env "$ENV_FILE"
verified_installed_release >/dev/null
[ -x "$SERVICE_WRAPPER" ] && [ -d "$WORK_DIR" ] || {
    echo "Verified installed relay service wrapper is unavailable: $SERVICE_WRAPPER" >&2
    echo "Install the complete release before installing its native service." >&2
    exit 1
}
mkdir -p "$HOME/Library/LaunchAgents"

PLIST_TEMP="$(mktemp "$(dirname "$PLIST")/.${LABEL}.candidate.XXXXXX")"
cat > "$PLIST_TEMP" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$SERVICE_WRAPPER</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>WorkingDirectory</key>
    <string>$WORK_DIR</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HERDR_RELAY_ENV</key>
        <string>$ENV_FILE</string>
    </dict>
</dict>
</plist>
EOF

chmod 600 "$PLIST_TEMP"
plutil -lint "$PLIST_TEMP" >/dev/null || {
    echo "Generated launchd definition is invalid" >&2
    exit 1
}

mv -f "$PLIST_TEMP" "$PLIST"
PLIST_TEMP=
definition_replaced=true
reload_launchd_service_definition "$PLIST" "$LABEL"

echo "Waiting for exact local and public Relay readiness..."
if ! HEALTH="$(wait_for_installed_relay_ready "$CLOUDFLARED_CONFIG")"; then
    echo "Replacement Relay service did not become ready; rolling back." >&2
    exit 1
fi
service_transaction_changed=false
launchctl bootout "gui/$(id -u)" "$LEGACY_PLIST" >/dev/null 2>&1 || true
rm -f "$LEGACY_PLIST"

echo "Installed and started $LABEL"
echo "Plist: $PLIST"
echo "Env:   $ENV_FILE"
echo "Relay readiness: $HEALTH"

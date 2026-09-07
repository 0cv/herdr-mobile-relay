#!/bin/bash
set -euo pipefail

LABEL="herdr-mobile-relay.service"
LEGACY_LABEL="herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_FILE="$UNIT_DIR/$LABEL"
LEGACY_UNIT_FILE="$UNIT_DIR/$LEGACY_LABEL"

export PATH="$HOME/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

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

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found"
    exit 1
fi

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "cloudflared not found in PATH"
    echo "Install cloudflared before installing the service."
    exit 1
fi

if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
    echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
    echo "Create it first, or set CLOUDFLARED_CONFIG in $ENV_FILE."
    exit 1
fi

RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
WORK_DIR="$RELEASE_ROOT/current"

ENV_EXISTED=false
ENV_BACKUP=
TOKEN_EXISTED=false
TOKEN_BACKUP=
UNIT_EXISTED=false
UNIT_BACKUP=
UNIT_TEMP=
ENV_RESTORE_TEMP=
TOKEN_RESTORE_TEMP=
SERVICE_WAS_ACTIVE=false
SERVICE_WAS_ENABLED=false
LEGACY_WAS_ACTIVE=false
LEGACY_WAS_ENABLED=false
LEGACY_STOPPED=false
service_transaction_changed=false
definition_replaced=false
rollback_failed=false
PREVIOUS_CLOUDFLARED_CONFIG=
PREVIOUS_LOCAL_HEALTH=
PREVIOUS_PUBLIC_HEALTH=
PREVIOUS_IDENTITY=
PREVIOUS_READINESS_CAPTURED=false

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

    [ "$PREVIOUS_READINESS_CAPTURED" = true ] || return 0
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
    echo "Could not prove the restored systemd service's exact local and public release identity." >&2
    return 1
}

rollback_systemd_install() {
    local restore_temp

    service_transaction_changed=false
    restore_relay_config || return 1
    if [ "$definition_replaced" = true ]; then
        if [ "$SERVICE_WAS_ACTIVE" != true ]; then
            systemctl --user stop "$LABEL" >/dev/null 2>&1 || true
            if systemctl --user is-active --quiet "$LABEL" >/dev/null 2>&1; then
                echo "Could not prove the replacement systemd service is inactive: $LABEL" >&2
                return 1
            fi
        fi
        if [ "$UNIT_EXISTED" = true ]; then
            restore_temp="$(mktemp "$UNIT_DIR/.${LABEL}.restore.XXXXXX")" || return 1
            if ! cp -p "$UNIT_BACKUP" "$restore_temp"; then
                rm -f "$restore_temp"
                return 1
            fi
            if ! mv -f "$restore_temp" "$UNIT_FILE"; then
                rm -f "$restore_temp"
                return 1
            fi
        else
            rm -f "$UNIT_FILE" || return 1
        fi
        systemctl --user daemon-reload || return 1
        if [ "$SERVICE_WAS_ENABLED" = true ]; then
            systemctl --user enable "$LABEL" >/dev/null || return 1
            systemctl --user is-enabled --quiet "$LABEL" >/dev/null 2>&1 || {
                echo "Could not prove the restored systemd service is enabled: $LABEL" >&2
                return 1
            }
        else
            systemctl --user disable "$LABEL" >/dev/null 2>&1 || true
            if systemctl --user is-enabled --quiet "$LABEL" >/dev/null 2>&1; then
                echo "Could not prove the restored systemd service is disabled: $LABEL" >&2
                return 1
            fi
        fi
        if [ "$SERVICE_WAS_ACTIVE" = true ]; then
            systemctl --user restart "$LABEL" || return 1
            systemctl --user is-active --quiet "$LABEL" >/dev/null 2>&1 || {
                echo "Could not prove the restored systemd service is active: $LABEL" >&2
                return 1
            }
        fi
    fi
    if [ "$LEGACY_STOPPED" = true ]; then
        if [ "$LEGACY_WAS_ENABLED" = true ]; then
            systemctl --user enable "$LEGACY_LABEL" >/dev/null || return 1
        fi
        if [ "$LEGACY_WAS_ACTIVE" = true ]; then
            systemctl --user restart "$LEGACY_LABEL" || return 1
            systemctl --user is-active --quiet "$LEGACY_LABEL" >/dev/null 2>&1 || {
                echo "Could not prove the restored legacy systemd service is active: $LEGACY_LABEL" >&2
                return 1
            }
        fi
    fi
    prove_previous_service_readiness || return 1
    if [ "$PREVIOUS_READINESS_CAPTURED" = true ]; then echo "Restored the previous systemd service environment, definition, state, and exact live identity." >&2; else echo "Restored the previous systemd service environment, definition, state, and activation; prior live identity was unavailable." >&2; fi
}

cleanup_systemd_install() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ] && [ "$service_transaction_changed" = true ]; then
        if ! rollback_systemd_install; then
            rollback_failed=true
            echo "ERROR: systemd service rollback failed; recovery data retained." >&2
            [ -z "$ENV_BACKUP" ] || echo "Relay environment backup: $ENV_BACKUP" >&2
            if [ -n "$UNIT_BACKUP" ]; then
                echo "Systemd definition backup: $UNIT_BACKUP" >&2
            elif [ "$definition_replaced" = true ]; then
                echo "Systemd replacement definition: $UNIT_FILE" >&2
            fi
        fi
    fi
    [ -z "$UNIT_TEMP" ] || rm -f "$UNIT_TEMP"
    [ -z "$ENV_RESTORE_TEMP" ] || rm -f "$ENV_RESTORE_TEMP"
    [ -z "$TOKEN_RESTORE_TEMP" ] || rm -f "$TOKEN_RESTORE_TEMP"
    if [ "$rollback_failed" = false ]; then
        [ -z "$ENV_BACKUP" ] || rm -f "$ENV_BACKUP"
        [ -z "$TOKEN_BACKUP" ] || rm -f "$TOKEN_BACKUP"
        [ -z "$UNIT_BACKUP" ] || rm -f "$UNIT_BACKUP"
    fi
    exit "$status"
}
trap cleanup_systemd_install EXIT

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

if [ -e "$UNIT_FILE" ] || [ -L "$UNIT_FILE" ]; then
    [ -f "$UNIT_FILE" ] && [ ! -L "$UNIT_FILE" ] || {
        echo "Refusing to replace unsafe systemd definition: $UNIT_FILE" >&2
        exit 1
    }
    UNIT_EXISTED=true
    UNIT_BACKUP="$(mktemp "$UNIT_DIR/.${LABEL}.backup.XXXXXX")"
    cp -p "$UNIT_FILE" "$UNIT_BACKUP"
fi
systemctl --user is-active --quiet "$LABEL" >/dev/null 2>&1 && SERVICE_WAS_ACTIVE=true
systemctl --user is-enabled --quiet "$LABEL" >/dev/null 2>&1 && SERVICE_WAS_ENABLED=true
if [ -e "$LEGACY_UNIT_FILE" ] || [ -L "$LEGACY_UNIT_FILE" ]; then
    [ -f "$LEGACY_UNIT_FILE" ] && [ ! -L "$LEGACY_UNIT_FILE" ] || {
        echo "Refusing unsafe legacy systemd definition: $LEGACY_UNIT_FILE" >&2
        exit 1
    }
    if systemctl --user is-enabled --quiet "$LEGACY_LABEL" >/dev/null 2>&1; then
        LEGACY_WAS_ENABLED=true
    fi
fi
if systemctl --user is-active --quiet "$LEGACY_LABEL" >/dev/null 2>&1; then
    [ -f "$LEGACY_UNIT_FILE" ] && [ ! -L "$LEGACY_UNIT_FILE" ] || {
        echo "Refusing to replace an active legacy systemd service without its definition: $LEGACY_LABEL" >&2
        exit 1
    }
    LEGACY_WAS_ACTIVE=true
fi
if [ "$SERVICE_WAS_ACTIVE" = true ]; then
    [ "$UNIT_EXISTED" = true ] || {
        echo "Refusing to replace an active systemd service without its definition: $LABEL" >&2
        exit 1
    }
fi
if [ "$SERVICE_WAS_ACTIVE" = true ] || [ "$LEGACY_WAS_ACTIVE" = true ]; then
    if previous_readiness="$(capture_previous_service_readiness)"; then
        PREVIOUS_CLOUDFLARED_CONFIG="$(printf '%s\n' "$previous_readiness" | sed -n '1p')"
        PREVIOUS_LOCAL_HEALTH="$(printf '%s\n' "$previous_readiness" | sed -n '2p')"
        PREVIOUS_PUBLIC_HEALTH="$(printf '%s\n' "$previous_readiness" | sed -n '3p')"
        PREVIOUS_IDENTITY="$(printf '%s\n' "$previous_readiness" | sed -n '4,6p')"
        PREVIOUS_READINESS_CAPTURED=true
    elif [ "$SERVICE_WAS_ACTIVE" = true ]; then
        echo "Could not snapshot the existing systemd service's exact local and public release identity." >&2
        exit 1
    else
        echo "Legacy systemd service readiness is unavailable; rollback can restore activation but cannot prove its prior live identity." >&2
    fi
fi

service_transaction_changed=true
if [ "$LEGACY_WAS_ACTIVE" = true ]; then
    LEGACY_STOPPED=true
    systemctl --user stop "$LEGACY_LABEL" || true
    if systemctl --user is-active --quiet "$LEGACY_LABEL" >/dev/null 2>&1; then
        echo "Could not stop the legacy systemd service before replacement: $LEGACY_LABEL" >&2
        exit 1
    fi
fi
mkdir -p "$ENV_DIR"
ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
load_relay_env "$ENV_FILE"
verified_installed_release >/dev/null
[ -x "$SERVICE_WRAPPER" ] && [ -d "$WORK_DIR" ] || {
    echo "Verified installed relay service wrapper is unavailable: $SERVICE_WRAPPER" >&2
    echo "Install the complete release before installing its native service." >&2
    exit 1
}
mkdir -p "$UNIT_DIR"

UNIT_TEMP="$(mktemp "$UNIT_DIR/.${LABEL}.candidate.XXXXXX")"
cat > "$UNIT_TEMP" <<EOF
[Unit]
Description=Herdr Mobile Relay and Cloudflare tunnel
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=120
StartLimitBurst=5

[Service]
Type=simple
WorkingDirectory=$WORK_DIR
Environment=HERDR_RELAY_ENV=$ENV_FILE
ExecStart=$SERVICE_WRAPPER
Restart=on-failure
RestartSec=10
KillMode=control-group

[Install]
WantedBy=default.target
EOF

chmod 600 "$UNIT_TEMP"
grep -Fx "WorkingDirectory=$WORK_DIR" "$UNIT_TEMP" >/dev/null &&
    grep -Fx "Environment=HERDR_RELAY_ENV=$ENV_FILE" "$UNIT_TEMP" >/dev/null &&
    grep -Fx "ExecStart=$SERVICE_WRAPPER" "$UNIT_TEMP" >/dev/null || {
        echo "Generated systemd definition is invalid" >&2
        exit 1
    }

mv -f "$UNIT_TEMP" "$UNIT_FILE"
UNIT_TEMP=
definition_replaced=true

systemctl --user daemon-reload
systemctl --user enable "$LABEL"
systemctl --user restart "$LABEL"

echo "Waiting for exact local and public Relay readiness..."
if ! HEALTH="$(wait_for_installed_relay_ready "$CLOUDFLARED_CONFIG")"; then
    echo "Replacement Relay service did not become ready; rolling back." >&2
    exit 1
fi
service_transaction_changed=false
systemctl --user disable --now "$LEGACY_LABEL" >/dev/null 2>&1 || true
rm -f "$LEGACY_UNIT_FILE"
systemctl --user daemon-reload

echo "Installed and started $LABEL"
echo "Unit: $UNIT_FILE"
echo "Env:  $ENV_FILE"
echo "Logs: journalctl --user -u $LABEL -f"
echo "Relay readiness: $HEALTH"

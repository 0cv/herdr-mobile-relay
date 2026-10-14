#!/usr/bin/env bash
set +x
set -euo pipefail
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE

LABEL="herdr-mobile-relay.service"
LEGACY_LABEL="herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_FILE="$UNIT_DIR/$LABEL"
LEGACY_UNIT_FILE="$UNIT_DIR/$LEGACY_LABEL"

export PATH="$HOME/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
. "$SCRIPT_DIR/native-install-transaction.sh"
require_user_service_context

ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR")" || exit 1

native_installer_admit "$ENV_FILE" "$ENV_FILE" "$UNIT_FILE" "$LEGACY_UNIT_FILE" || exit 1
if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found"
    exit 1
fi

RELAY_BIN="$(relay_binary)"

if [ "$TRANSPORT" = gateway ]; then
    GATEWAY_APP_ORIGIN="$(gateway_install_phone_app_origin "$ENV_FILE")" || exit 1
elif [ "$TRANSPORT" = cloudflare ]; then
    CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
    CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
    if [ -z "$CLOUDFLARED_BIN" ] || [ ! -x "$CLOUDFLARED_BIN" ]; then
        echo "cloudflared not found in PATH"
        echo "Install cloudflared before installing the service."
        exit 1
    fi
    if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
        echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
        echo "Create it first, or set CLOUDFLARED_CONFIG in $ENV_FILE."
        exit 1
    fi
fi
native_install_begin systemd "$UNIT_FILE" "$LEGACY_UNIT_FILE" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL" "$NATIVE_SIDECAR" || exit 1
native_installer_candidate "$ENV_FILE" "${CLOUDFLARED_CONFIG:-}" || exit 1
RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
if [ ! -x "$SERVICE_WRAPPER" ]; then
    SERVICE_WRAPPER="$SCRIPT_DIR/herdr-mobile-relay-service.sh"
fi
WORK_DIR="$RELEASE_ROOT/current"
if [ ! -d "$WORK_DIR" ]; then
    WORK_DIR="$(cd "$SCRIPT_DIR/.." && pwd -P)"
fi
[ -x "$SERVICE_WRAPPER" ] || exit 1
WORK_DIR_ESCAPED="$(systemd_quoted "$WORK_DIR")" || { echo "✗ Service work path contains unsupported control characters." >&2; exit 1; }
ENVIRONMENT_ESCAPED="$(systemd_quoted "HERDR_RELAY_ENV=$ENV_FILE")" || { echo "✗ Service environment path contains unsupported control characters." >&2; exit 1; }
SERVICE_WRAPPER_ESCAPED="$(systemd_quoted "$SERVICE_WRAPPER" exec)" || { echo "✗ Service executable path contains unsupported control characters." >&2; exit 1; }

STAGED_UNIT="$native_recovery/new.service"
cat > "$STAGED_UNIT" <<EOF
[Unit]
Description=Herdr Mobile Relay $TRANSPORT
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$WORK_DIR_ESCAPED
Environment=$ENVIRONMENT_ESCAPED
ExecStart=$SERVICE_WRAPPER_ESCAPED
Restart=on-failure
RestartSec=10
# Unavailable/config refusal remains FAILED, not ready. Genuine child 69/78
# also suppress restart while retaining the child's failure status.
RestartPreventExitStatus=69 78

[Install]
WantedBy=default.target
EOF

chmod 600 "$STAGED_UNIT" || exit 1
if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze --user verify "$STAGED_UNIT" >/dev/null || exit 1
fi
native_install_publish "$STAGED_UNIT" || exit 1
native_install_activate || exit 1

echo 'Replacement activation attempted; verifying readiness.'

# ensure_relay_env generates the token and instance identity into the file
# only, so read both back before the readiness gate compares identities.
PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
PORT="${PORT:-${HERDR_RELAY_PORT:-8375}}"
INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"
echo "Waiting for relay health on 127.0.0.1:$PORT..."
if ! HEALTH="$(wait_for_relay_health "$PORT" 15 1 "$INSTANCE")"; then
    report_inventory_failure "$PORT"
    echo "Replacement service did not become ready; attempting quiescence-gated rollback."
    echo "Inspect it with:"
    echo "  systemctl --user status $LABEL --no-pager"
    echo "  journalctl --user -u $LABEL -n 80 --no-pager"
    exit 1
fi
if [ "$TRANSPORT" = gateway ]; then
    verify_gateway_install_readiness "$PORT" "$HEALTH" "$GATEWAY_APP_ORIGIN"
else
    verify_public_readiness "$ENV_FILE" "$HEALTH"
fi
native_install_commit || exit 1
echo "Installed and verified $LABEL"

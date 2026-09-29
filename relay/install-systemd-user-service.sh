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

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

load_relay_env "$ENV_FILE"
TRANSPORT="$(relay_transport_mode "$ENV_FILE")"
if [ -e "$(tailscale_session_file "$ENV_FILE")" ] ||
    [ -e "$(tailscale_external_session_file "$ENV_FILE")" ] ||
    [ "$(relay_transport_mode "$ENV_FILE")" = tailscale ] ||
    [ "$(relay_transport_mode "$ENV_FILE")" = tailscale-external ]; then
    echo "✗ Background service installation is unsupported for foreground Tailscale Serve transports." >&2
    echo "  Stop the pane and choose a background-compatible transport before installing a service." >&2
    exit 1
fi
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found"
    exit 1
fi

RELAY_BIN="$(relay_binary)"

if [ "$TRANSPORT" = tailscale-cli ]; then
    if [ "${HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START:-}" = 1 ]; then
        "$RELAY_BIN" tailscale-cli activation-check >/dev/null || exit $?
    else
        tailscale_cli_registration_status "$ENV_FILE" >/dev/null || {
            echo "✗ A verified CLI-backed registration is required before installing its user service." >&2
            exit 1
        }
    fi
    ensure_relay_env "$ENV_FILE"
else
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
    ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
fi
RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
if [ ! -x "$SERVICE_WRAPPER" ]; then
    SERVICE_WRAPPER="$SCRIPT_DIR/herdr-mobile-relay-service.sh"
fi
WORK_DIR="$RELEASE_ROOT/current"
if [ ! -d "$WORK_DIR" ]; then
    WORK_DIR="$(cd "$SCRIPT_DIR/.." && pwd -P)"
fi
chmod +x "$SERVICE_WRAPPER"
mkdir -p "$UNIT_DIR"
WORK_DIR_ESCAPED="$(systemd_quote_value "$WORK_DIR")" || { echo "✗ Service work path contains unsupported control characters." >&2; exit 1; }
ENVIRONMENT_ESCAPED="$(systemd_quote_value "$ENV_FILE")" || { echo "✗ Service environment path contains unsupported control characters." >&2; exit 1; }
SERVICE_WRAPPER_ESCAPED="$(systemd_quote_exec "$SERVICE_WRAPPER")" || { echo "✗ Service executable path contains unsupported control characters." >&2; exit 1; }

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Herdr Mobile Relay $TRANSPORT
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$WORK_DIR_ESCAPED
Environment=HERDR_RELAY_ENV=$ENVIRONMENT_ESCAPED
ExecStart=$SERVICE_WRAPPER_ESCAPED
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user disable --now "$LEGACY_LABEL" >/dev/null 2>&1 || true
rm -f "$LEGACY_UNIT_FILE"
systemctl --user daemon-reload
systemctl --user enable "$LABEL"
systemctl --user restart "$LABEL"

echo "Installed and started $LABEL"
echo "Unit: $UNIT_FILE"
echo "Env:  $ENV_FILE"
echo "Logs: journalctl --user -u $LABEL -f"

PORT="${HERDR_RELAY_PORT:-8375}"
echo "Waiting for relay health on 127.0.0.1:$PORT..."
if ! HEALTH="$(wait_for_relay_health "$PORT")"; then
    echo "Relay service was installed, but it did not become healthy."
    echo "Inspect it with:"
    echo "  systemctl --user status $LABEL --no-pager"
    echo "  journalctl --user -u $LABEL -n 80 --no-pager"
    exit 1
fi
echo "Relay health: $HEALTH"

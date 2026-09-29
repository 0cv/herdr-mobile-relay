#!/bin/bash
set -euo pipefail

LABEL="com.herdr-mobile-relay.service"
LEGACY_LABEL="com.herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LEGACY_PLIST="$HOME/Library/LaunchAgents/$LEGACY_LABEL.plist"
LOG_DIR="$HOME/Library/Logs/herdr-mobile-relay"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

require_user_service_context

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

if [ "$TRANSPORT" = tailscale-cli ]; then
    RELAY_BIN="$(relay_binary)"
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
    if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
        echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
        echo "Create it first, or set CLOUDFLARED_CONFIG before running this installer."
        exit 1
    fi
    ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
fi
chmod +x "$SCRIPT_DIR/herdr-mobile-relay-service.sh"
mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR"

RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
WORK_DIR="$RELEASE_ROOT/current"
if [ ! -x "$SERVICE_WRAPPER" ]; then
    SERVICE_WRAPPER="$SCRIPT_DIR/herdr-mobile-relay-service.sh"
fi
if [ ! -d "$WORK_DIR" ]; then
    WORK_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
fi
SERVICE_WRAPPER_XML="$(xml_escape_text "$SERVICE_WRAPPER")" || { echo "✗ Service executable path contains unsupported XML characters." >&2; exit 1; }
WORK_DIR_XML="$(xml_escape_text "$WORK_DIR")" || { echo "✗ Service work path contains unsupported XML characters." >&2; exit 1; }
ENV_FILE_XML="$(xml_escape_text "$ENV_FILE")" || { echo "✗ Service environment path contains unsupported XML characters." >&2; exit 1; }
LOG_DIR_XML="$(xml_escape_text "$LOG_DIR")" || { echo "✗ Service log path contains unsupported XML characters." >&2; exit 1; }

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$SERVICE_WRAPPER_XML</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
        <key>NetworkState</key>
        <true/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>WorkingDirectory</key>
    <string>$WORK_DIR_XML</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HERDR_RELAY_ENV</key>
        <string>$ENV_FILE_XML</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$LOG_DIR_XML/service.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR_XML/service.err</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$UID" "$LEGACY_PLIST" >/dev/null 2>&1 || true
rm -f "$LEGACY_PLIST"
reload_launchd_service_definition "$PLIST" "$LABEL"

echo "Installed and started $LABEL"
echo "Plist: $PLIST"
echo "Env:   $ENV_FILE"
echo "Logs:  $LOG_DIR/service.log and $LOG_DIR/service.err"

PORT="${HERDR_RELAY_PORT:-8375}"
echo "Waiting for relay health on 127.0.0.1:$PORT..."
if ! HEALTH="$(wait_for_relay_health "$PORT")"; then
    echo "Relay service was installed, but it did not become healthy."
    echo "Inspect it with:"
    echo "  launchctl print gui/$(id -u)/$LABEL"
    echo "  tail -n 80 '$LOG_DIR/service.log' '$LOG_DIR/service.err'"
    exit 1
fi
echo "Relay health: $HEALTH"

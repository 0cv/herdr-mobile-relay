#!/usr/bin/env bash
set -euo pipefail

LABEL="com.herdr-mobile-relay.service"
LEGACY_LABEL="com.herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LEGACY_PLIST="$HOME/Library/LaunchAgents/$LEGACY_LABEL.plist"
LOG_DIR="$HOME/Library/Logs/herdr-mobile-relay"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
. "$SCRIPT_DIR/native-install-transaction.sh"

require_user_service_context

ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR")" || exit 1

load_relay_env "$ENV_FILE"
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
TRANSPORT="$(relay_service_transport_mode)" || exit 1
if [ "$TRANSPORT" = tailscale-cli ] && installed_legacy_relay_service_definition_present; then
    echo "✗ CLI setup refuses a legacy relay service definition; it will not stop or remove it. Preserve it and resolve its route before migrating." >&2
    exit 4
fi
if [ "$TRANSPORT" = tailscale-cli ] && [ "${HERDR_CLI_SETUP_NEW_SERVICE:-}" = 1 ] &&
    installed_relay_service_definition_present; then
    echo "✗ CLI setup refuses to replace a relay service definition that appeared during setup." >&2
    exit 4
fi
if [ -e "$(tailscale_session_file "$ENV_FILE")" ] ||
    [ -e "$(tailscale_external_session_file "$ENV_FILE")" ] ||
    [ "$TRANSPORT" = tailscale ] ||
    [ "$TRANSPORT" = tailscale-external ]; then
    echo "✗ Background service installation is unsupported for foreground Tailscale Serve transports." >&2
    echo "  Stop the pane and choose a background-compatible transport before installing a service." >&2
    exit 1
fi
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
    native_install_begin launchd "$PLIST" "$LEGACY_PLIST" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL"
    mkdir -p "$(dirname "$ENV_FILE")"
    ensure_relay_env "$ENV_FILE"
elif [ "$TRANSPORT" = gateway ]; then
    GATEWAY_APP_ORIGIN="$(gateway_install_phone_app_origin "$ENV_FILE")" || exit 1
    native_install_begin launchd "$PLIST" "$LEGACY_PLIST" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL"
    mkdir -p "$(dirname "$ENV_FILE")"
    ensure_relay_env "$ENV_FILE"
elif [ "$TRANSPORT" = cloudflare ]; then
    CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
    CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
    if [ -z "$CLOUDFLARED_BIN" ] || [ ! -x "$CLOUDFLARED_BIN" ]; then
        echo "cloudflared not found or not executable; install it before installing the Cloudflare service." >&2
        exit 1
    fi
    if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
        echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
        echo "Create it first, or set CLOUDFLARED_CONFIG before running this installer."
        exit 1
    fi
    native_install_begin launchd "$PLIST" "$LEGACY_PLIST" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL"
    mkdir -p "$(dirname "$ENV_FILE")"
    ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
else
    echo "Unsupported background transport." >&2
    exit 1
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

STAGED_PLIST="$native_recovery/new.plist"
cat > "$STAGED_PLIST" <<EOF
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

chmod 600 "$STAGED_PLIST"
plutil -lint "$STAGED_PLIST"
native_changed=true
native_stage="$(mktemp "$(dirname "$PLIST")/.herdr-service.XXXXXX")"
cp "$STAGED_PLIST" "$native_stage"
chmod 600 "$native_stage"
mv -f "$native_stage" "$PLIST"
if [ "$TRANSPORT" != tailscale-cli ] && [ "$native_legacy_active" = true ]; then
    launchctl bootout "gui/$UID" "$LEGACY_PLIST"
fi
reload_launchd_service_definition "$PLIST" "$LABEL"

echo "Installed and started $LABEL"
echo "Plist: $PLIST"
echo "Env:   $ENV_FILE"
echo "Logs:  $LOG_DIR/service.log and $LOG_DIR/service.err"

# ensure_relay_env generates the token and instance identity into the file
# only, so read both back before the readiness gate compares identities.
PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
PORT="${PORT:-${HERDR_RELAY_PORT:-8375}}"
INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"
echo "Waiting for relay health on 127.0.0.1:$PORT..."
if ! HEALTH="$(wait_for_relay_health "$PORT" 15 1 "$INSTANCE")"; then
    report_inventory_failure "$PORT"
    echo "Replacement service did not become ready; restoring the previous installation."
    echo "Inspect it with:"
    echo "  launchctl print gui/$(id -u)/$LABEL"
    echo "  tail -n 80 '$LOG_DIR/service.log' '$LOG_DIR/service.err'"
    exit 1
fi
if [ "$TRANSPORT" = gateway ]; then
    verify_gateway_install_readiness "$PORT" "$HEALTH" "$GATEWAY_APP_ORIGIN"
else
    verify_public_readiness "$ENV_FILE" "$HEALTH"
fi
native_install_commit
echo "Relay readiness: $HEALTH"

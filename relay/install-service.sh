#!/usr/bin/env bash
set +x
set -euo pipefail
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE

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

native_installer_admit "$ENV_FILE" "$ENV_FILE" "$PLIST" "$LEGACY_PLIST" || exit 1
if [ "$TRANSPORT" = gateway ]; then
    GATEWAY_APP_ORIGIN="$(gateway_install_phone_app_origin "$ENV_FILE")" || exit 1
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
fi
native_install_begin launchd "$PLIST" "$LEGACY_PLIST" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL" "$NATIVE_SIDECAR" || exit 1
native_installer_candidate "$ENV_FILE" "${CLOUDFLARED_CONFIG:-}" || exit 1
# Only create required absent directories; never chmod an existing executable.
(umask 077; mkdir -p "$LOG_DIR") || exit 1

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
        <string>--launchd</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <!-- Explicit pre-child launchd refusal exits zero/non-ready. -->
        <key>SuccessfulExit</key>
        <false/>
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

chmod 600 "$STAGED_PLIST" && plutil -lint "$STAGED_PLIST" >/dev/null || exit 1
native_install_publish "$STAGED_PLIST" || exit 1
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
    echo "  launchctl print gui/$(id -u)/$LABEL"
    echo "  tail -n 80 '$LOG_DIR/service.log' '$LOG_DIR/service.err'"
    exit 1
fi
if [ "$TRANSPORT" = gateway ]; then
    verify_gateway_install_readiness "$PORT" "$HEALTH" "$GATEWAY_APP_ORIGIN"
else
    verify_public_readiness "$ENV_FILE" "$HEALTH"
fi
native_install_commit || exit 1
echo "Installed and verified $LABEL"

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

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

load_relay_env "$ENV_FILE"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
    echo "Missing Cloudflare tunnel config: $CLOUDFLARED_CONFIG"
    echo "Create it first, or set CLOUDFLARED_CONFIG before running this installer."
    exit 1
fi

ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
chmod +x "$SCRIPT_DIR/herdr-mobile-relay-service.sh"
mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$SCRIPT_DIR/herdr-mobile-relay-service.sh</string>
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
    <string>$(cd "$SCRIPT_DIR/.." && pwd)</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HERDR_RELAY_ENV</key>
        <string>$ENV_FILE</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$LOG_DIR/service.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/service.err</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$UID" "$LEGACY_PLIST" >/dev/null 2>&1 || true
rm -f "$LEGACY_PLIST"
launchctl bootout "gui/$UID" "$PLIST" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$UID" "$PLIST"
launchctl enable "gui/$UID/$LABEL"
launchctl kickstart -k "gui/$UID/$LABEL"

echo "Installed and started $LABEL"
echo "Plist: $PLIST"
echo "Env:   $ENV_FILE"
echo "Logs:  $LOG_DIR/service.log and $LOG_DIR/service.err"

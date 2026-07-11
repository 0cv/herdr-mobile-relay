#!/bin/bash
set -euo pipefail

LABEL="herdr-mobile-relay.service"
LEGACY_LABEL="herdr-remote.service"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_FILE="$UNIT_DIR/$LABEL"
LEGACY_UNIT_FILE="$UNIT_DIR/$LEGACY_LABEL"

export PATH="$HOME/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

load_relay_env "$ENV_FILE"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found"
    exit 1
fi

if ! command -v uv >/dev/null 2>&1; then
    echo "uv not found in PATH"
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

ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
chmod +x "$SCRIPT_DIR/herdr-mobile-relay-service.sh"
mkdir -p "$UNIT_DIR"

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Herdr Mobile Relay and Cloudflare tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$REPO_DIR
Environment=HERDR_RELAY_ENV=$ENV_FILE
ExecStart=$SCRIPT_DIR/herdr-mobile-relay-service.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user disable --now "$LEGACY_LABEL" >/dev/null 2>&1 || true
rm -f "$LEGACY_UNIT_FILE"
systemctl --user daemon-reload
systemctl --user enable --now "$LABEL"

echo "Installed and started $LABEL"
echo "Unit: $UNIT_FILE"
echo "Env:  $ENV_FILE"
echo "Logs: journalctl --user -u $LABEL -f"

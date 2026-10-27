#!/usr/bin/env bash
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
. "$SCRIPT_DIR/native-install-transaction.sh"
require_user_service_context

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

load_relay_env "$ENV_FILE"
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl not found"
    exit 1
fi

relay_binary >/dev/null

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

native_install_begin systemd "$UNIT_FILE" "$LEGACY_UNIT_FILE" "$ENV_FILE" "$LABEL" "$LEGACY_LABEL"
ensure_relay_env "$ENV_FILE" "$CLOUDFLARED_CONFIG"
RELEASE_ROOT="$(relay_release_root)"
SERVICE_WRAPPER="$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh"
if [ ! -x "$SERVICE_WRAPPER" ]; then
    SERVICE_WRAPPER="$SCRIPT_DIR/herdr-mobile-relay-service.sh"
fi
WORK_DIR="$RELEASE_ROOT/current"
if [ ! -d "$WORK_DIR" ]; then
    WORK_DIR="$SCRIPT_DIR/.."
fi
chmod +x "$SERVICE_WRAPPER"
mkdir -p "$UNIT_DIR"

STAGED_UNIT="$native_recovery/new.service"
cat > "$STAGED_UNIT" <<EOF
[Unit]
Description=Herdr Mobile Relay and Cloudflare tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$(systemd_quoted "$WORK_DIR")
Environment=$(systemd_quoted "HERDR_RELAY_ENV=$ENV_FILE")
ExecStart=$(systemd_quoted "$SERVICE_WRAPPER" exec)
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

chmod 600 "$STAGED_UNIT"
if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze --user verify "$STAGED_UNIT"
fi
native_changed=true
native_stage="$(mktemp "$UNIT_DIR/.herdr-service.XXXXXX")"
cp "$STAGED_UNIT" "$native_stage"
chmod 600 "$native_stage"
mv -f "$native_stage" "$UNIT_FILE"
systemctl --user daemon-reload
if [ "$native_legacy_active" = true ]; then
    systemctl --user stop "$LEGACY_LABEL"
fi
systemctl --user enable "$LABEL"
systemctl --user restart "$LABEL"

echo "Installed and started $LABEL"
echo "Unit: $UNIT_FILE"
echo "Env:  $ENV_FILE"
echo "Logs: journalctl --user -u $LABEL -f"

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
    echo "  systemctl --user status $LABEL --no-pager"
    echo "  journalctl --user -u $LABEL -n 80 --no-pager"
    exit 1
fi
verify_public_readiness "$ENV_FILE" "$HEALTH"
native_install_commit
echo "Relay readiness: $HEALTH"

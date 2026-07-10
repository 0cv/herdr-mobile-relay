#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

echo "🐑 Herdr Mobile Relay token rotation"
echo ""

if [ ! -f "$ENV_FILE" ]; then
    echo "✗ $ENV_FILE does not exist. Run make setup first."
    exit 1
fi

NEW_TOKEN="$(generate_token)"

# Portable in-place edit (BSD and GNU sed disagree on -i): rewrite via a temp
# file, keeping every non-token line.
TMP_FILE="$(mktemp "${TMPDIR:-/tmp}/herdr-env.XXXXXX")"
grep -v '^HERDR_RELAY_TOKEN=' "$ENV_FILE" > "$TMP_FILE" || true
printf 'HERDR_RELAY_TOKEN=%s\n' "$NEW_TOKEN" >> "$TMP_FILE"
chmod 600 "$TMP_FILE"
mv "$TMP_FILE" "$ENV_FILE"

echo "✓ Wrote a new relay token to $ENV_FILE"
echo "  Phones configured with the old token stop working once the relay restarts."
echo ""

# Restart the background service when one is installed so the new token takes
# effect immediately; otherwise the next relay start picks it up.
RESTARTED=""
case "$(uname -s)" in
    Darwin)
        SERVICE="gui/$(id -u)/com.herdr-mobile-relay.service"
        if launchctl print "$SERVICE" >/dev/null 2>&1; then
            launchctl kickstart -k "$SERVICE"
            RESTARTED=1
        fi
        ;;
    Linux)
        if systemctl --user cat herdr-mobile-relay.service >/dev/null 2>&1; then
            systemctl --user restart herdr-mobile-relay.service
            RESTARTED=1
        fi
        ;;
esac
if [ -n "$RESTARTED" ]; then
    echo "✓ Restarted the background service with the new token."
else
    echo "  No background service found. Restart the relay (or rerun make quick-start)"
    echo "  to apply the new token."
fi
echo ""

# Re-add the relay on each phone with the new token. setup-link fails cleanly
# when no stable hostname is configured (quick-tunnel-only installs).
if ! "$SCRIPT_DIR/setup-link.sh"; then
    echo ""
    echo "  For quick tunnels, rerun make quick-start and scan the new QR code."
fi

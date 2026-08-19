#!/bin/bash
# Moves this relay to a different tunnel hostname. The named tunnel, its
# credentials, the relay token, and every paired phone survive: only the route
# and the ingress change, so a domain move costs one route and one restart
# instead of a teardown, a fresh setup, and a re-pair.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="${HERDR_RELAY_ENV:-}"
if [ -z "$ENV_FILE" ]; then
    ENV_FILE="$(installed_service_env_file)"
fi
if [ -z "$ENV_FILE" ]; then
    ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
fi
if [ ! -f "$ENV_FILE" ]; then
    echo "✗ No relay configuration found. Run Quick Start first." >&2
    exit 1
fi
ENV_FILE="$(canonical_file_path "$ENV_FILE")"
assert_service_env_matches "$ENV_FILE"
load_relay_env "$ENV_FILE"

CONFIG="${CLOUDFLARED_CONFIG:-}"
if [ -z "$CONFIG" ] || [ ! -f "$CONFIG" ]; then
    echo "✗ This relay does not run a Cloudflare tunnel." >&2
    echo "  Hostnames only apply to the tunnel path; a gateway relay is reached" >&2
    echo "  through its gateway. Use Choose Connection Method instead." >&2
    exit 1
fi

# The ingress carries the hostname the tunnel answers on; the tunnel line names
# the tunnel that has to learn the new route.
current_hostname() {
    sed -n 's/^[[:space:]]*-\{0,1\}[[:space:]]*hostname:[[:space:]]*\([^[:space:]]*\).*/\1/p' \
        "$CONFIG" | head -1
}

tunnel_reference() {
    sed -n 's/^[[:space:]]*tunnel:[[:space:]]*\([^[:space:]]*\).*/\1/p' "$CONFIG" | head -1
}

CURRENT_HOSTNAME="$(current_hostname)"
TUNNEL="$(tunnel_reference)"
if [ -z "$TUNNEL" ]; then
    echo "✗ $CONFIG names no tunnel, so there is nothing to route." >&2
    exit 1
fi

echo "🐑 Change the tunnel hostname"
echo ""
echo "  Tunnel:   $TUNNEL"
if [ -n "$CURRENT_HOSTNAME" ]; then
    echo "  Current:  $CURRENT_HOSTNAME"
fi
echo ""
echo "The new name must be in a domain on the same Cloudflare account. The old"
echo "record keeps working until you delete it, so phones can move over at their"
echo "own pace."
echo ""

NEW_HOSTNAME="${HERDR_STABLE_HOSTNAME:-}"
if [ -z "$NEW_HOSTNAME" ]; then
    while true; do
        if ! read -r -p "New hostname, or q to cancel: " NEW_HOSTNAME; then
            echo "" >&2
            exit 1
        fi
        case "$NEW_HOSTNAME" in
            q | Q)
                echo "Left unchanged."
                exit 0
                ;;
            '') continue ;;
        esac
        if printf '%s' "$NEW_HOSTNAME" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$' &&
            printf '%s' "$NEW_HOSTNAME" | grep -Fq .; then
            break
        fi
        echo "✗ Enter a bare hostname such as relay.example.com."
    done
fi
if [ "$NEW_HOSTNAME" = "$CURRENT_HOSTNAME" ]; then
    echo "✓ $NEW_HOSTNAME is already the hostname. Nothing to do."
    exit 0
fi

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "✗ cloudflared is not installed, so the route cannot be created." >&2
    exit 1
fi

echo ""
printf '▸ Routing %s to %s..' "$NEW_HOSTNAME" "$TUNNEL"
if ! ROUTE_OUTPUT="$(cloudflared tunnel route dns "$TUNNEL" "$NEW_HOSTNAME" 2>&1)"; then
    echo ""
    echo "✗ Cloudflare refused the route:" >&2
    printf '%s\n' "$ROUTE_OUTPUT" >&2
    echo "  A name already pointing somewhere else has to be freed first, and the" >&2
    echo "  domain has to be on the same account as the tunnel." >&2
    exit 1
fi
echo " ✓"

# The config is rewritten in place so an operator's own edits elsewhere in it
# survive; only the first ingress hostname moves.
CONFIG_BACKUP="$CONFIG.herdr-previous"
cp "$CONFIG" "$CONFIG_BACKUP"
TEMP_CONFIG="$CONFIG.herdr-new.$$"
if [ -n "$CURRENT_HOSTNAME" ]; then
    sed "s|^\([[:space:]]*-\{0,1\}[[:space:]]*hostname:[[:space:]]*\)$CURRENT_HOSTNAME|\1$NEW_HOSTNAME|" \
        "$CONFIG" > "$TEMP_CONFIG"
else
    sed "s|^\([[:space:]]*-\{0,1\}[[:space:]]*hostname:[[:space:]]*\).*|\1$NEW_HOSTNAME|" \
        "$CONFIG" > "$TEMP_CONFIG"
fi
if ! grep -Fq "$NEW_HOSTNAME" "$TEMP_CONFIG"; then
    rm -f "$TEMP_CONFIG"
    echo "✗ Could not rewrite the ingress hostname in $CONFIG." >&2
    exit 1
fi
mv -f "$TEMP_CONFIG" "$CONFIG"
echo "✓ $CONFIG now serves $NEW_HOSTNAME (previous copy: $CONFIG_BACKUP)"

# Whatever the wizard recorded has to agree, or a later teardown would chase the
# old name and refuse to finish.
STATE_FILE="${HERDR_STABLE_STATE_FILE:-$(dirname "$ENV_FILE")/stable-setup.json}"
if [ -f "$STATE_FILE" ]; then
    if "$(relay_binary)" stable-state update "$STATE_FILE" hostname "$NEW_HOSTNAME" >/dev/null 2>&1; then
        echo "✓ Recorded the new hostname in $STATE_FILE"
    else
        echo "▸ Could not update $STATE_FILE; teardown may still name the old host."
    fi
fi

echo ""
echo "▸ Restarting the relay service.."
"$SCRIPT_DIR/service.sh" install >/dev/null
echo "✓ Restarted."

PORT="${HERDR_RELAY_PORT:-8375}"
DEADLINE=$((SECONDS + ${HERDR_STABLE_HTTP_TIMEOUT:-90}))
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-hostname.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT
printf '▸ Waiting for https://%s/healthz' "$NEW_HOSTNAME"
while true; do
    if curl -fsS --max-time 5 "http://127.0.0.1:$PORT/healthz" > "$WORK_DIR/local.json" 2>/dev/null &&
        curl -fsS --max-time 5 "https://$NEW_HOSTNAME/healthz" > "$WORK_DIR/public.json" 2>/dev/null &&
        "$(relay_binary)" stable-state health-match "$WORK_DIR/local.json" "$WORK_DIR/public.json" \
            2>/dev/null; then
        echo " ✓"
        break
    fi
    if [ "$SECONDS" -ge "$DEADLINE" ]; then
        echo ""
        echo "✗ $NEW_HOSTNAME did not answer as this relay yet." >&2
        echo "  DNS and the certificate can take a minute; the old hostname still" >&2
        echo "  works, so nothing is broken. Reprint the QR once it answers." >&2
        exit 1
    fi
    printf '.'
    sleep 2
done

echo ""
echo "Phones already paired keep using $CURRENT_HOSTNAME until they import the"
echo "new link, and both names reach this relay until you delete the old record"
echo "in Cloudflare."
echo ""
exec "$SCRIPT_DIR/setup-link.sh" "$NEW_HOSTNAME"

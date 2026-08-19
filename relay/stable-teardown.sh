#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
export PATH="$HOME/.local/bin:$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
ENV_FILE="$(canonical_file_path "$ENV_FILE")"
STATE_FILE="${HERDR_STABLE_STATE_FILE:-$(dirname "$ENV_FILE")/stable-setup.json}"

state_command() {
    "$(relay_binary)" stable-state "$@"
}

state_get() {
    state_command get "$STATE_FILE" "$1"
}

state_update() {
    state_command update "$STATE_FILE" "$@"
}

dns_has_record() {
    local hostname="$1"
    local record_type
    local response

    for record_type in A AAAA CNAME; do
        if ! response="$(curl -fsS --max-time 5 -H 'accept: application/dns-json' \
            "https://cloudflare-dns.com/dns-query?name=$hostname&type=$record_type" 2>/dev/null)"; then
            return 2
        fi
        if printf '%s' "$response" | grep -q '"Answer"'; then
            return 0
        fi
    done
    return 1
}


if [ ! -f "$STATE_FILE" ]; then
    echo "✗ No Herdr stable-setup state exists at $STATE_FILE." >&2
    echo "  Teardown will not guess which Cloudflare resources belong to Herdr." >&2
    exit 1
fi

require_supported_platform

# The first read validates the ownership marker before any local or Cloudflare
# mutation. A malformed or foreign JSON file is never adopted.
TUNNEL_NAME="$(state_get tunnel_name)"
case "$TUNNEL_NAME" in
    herdr-mobile-relay-*) ;;
    *)
        echo "✗ Refusing teardown: the recorded tunnel is outside the Herdr stable-tunnel namespace: ${TUNNEL_NAME:-<empty>}" >&2
        exit 1
        ;;
esac

assert_service_env_matches "$ENV_FILE"

TUNNEL_UUID="$(state_get tunnel_uuid)"
HOSTNAME="$(state_get hostname)"
CONFIG="$(state_get config_path)"
CREDENTIALS="$(state_get credentials_path)"
ENV_CREATED="$(state_get env_created_by_wizard)"
TUNNEL_DELETED="$(state_get tunnel_deleted)"


case "$(uname -s)" in
    Darwin)
        SERVICE="com.herdr-mobile-relay.service"
        SERVICE_FILE="$HOME/Library/LaunchAgents/$SERVICE.plist"
        ;;
    Linux)
        SERVICE="herdr-mobile-relay.service"
        SERVICE_FILE="$HOME/.config/systemd/user/$SERVICE"
        ;;
esac

if [ "${HERDR_STABLE_TEARDOWN_WRAPPED:-}" != 1 ]; then
    echo "🐑 Herdr Mobile Relay stable tunnel teardown"
    echo ""
fi
echo "The recorded stable relay and its local Cloudflare files will be removed:"
echo "  Service:     $SERVICE ($SERVICE_FILE)"
echo "  Tunnel:      $TUNNEL_NAME (${TUNNEL_UUID:-unknown UUID})"
echo "  Hostname:    ${HOSTNAME:-unknown}"
echo "  Config:      ${CONFIG:-none}"
echo "  Credentials: ${CREDENTIALS:-none}"
echo ""

if [ "${HERDR_STABLE_TEARDOWN_YES:-}" != "1" ]; then
    if [ ! -t 0 ]; then
        echo "✗ Confirmation required. Run interactively, or set HERDR_STABLE_TEARDOWN_YES=1." >&2
        exit 1
    fi
    read -r -p "Type teardown to continue: " confirmation
    if [ "$confirmation" != "teardown" ]; then
        echo "Teardown cancelled."
        exit 0
    fi
fi

if [ -e "$SERVICE_FILE" ]; then
    echo "▸ Stopping the recorded relay service..."
    "$SCRIPT_DIR/service.sh" uninstall
    state_update "service_installed_by_wizard=false" "stage=service_removed"
else
    echo "▸ The recorded relay service is already absent."
fi

if [ "$TUNNEL_DELETED" != true ]; then
    if [ -z "$TUNNEL_UUID" ]; then
        echo "✗ The configured stable tunnel has no recorded UUID; state was preserved." >&2
        exit 1
    fi
    echo "▸ Deleting configured stable tunnel $TUNNEL_NAME ($TUNNEL_UUID)..."
    if ! cloudflared tunnel delete --force "$TUNNEL_UUID"; then
        echo "✗ Cloudflare tunnel deletion failed; local files and state were preserved." >&2
        echo "  If cert.pem is missing, run cloudflared tunnel login, then rerun this teardown." >&2
        exit 1
    fi
    state_update "tunnel_deleted=true" "stage=tunnel_deleted"
else
    echo "▸ The configured stable tunnel was already deleted on an earlier teardown run."
fi

if [ -n "$CONFIG" ]; then
    rm -f "$CONFIG"
    echo "✓ Removed stable relay config: $CONFIG"
fi
if [ -n "$CREDENTIALS" ]; then
    rm -f "$CREDENTIALS"
    echo "✓ Removed stable relay credentials: $CREDENTIALS"
fi

if [ "$ENV_CREATED" = true ]; then
    rm -f "$ENV_FILE"
    echo "✓ Removed relay environment created by the wizard: $ENV_FILE"
elif [ -n "$CONFIG" ]; then
    remove_env_value_if_equals_atomic "$ENV_FILE" CLOUDFLARED_CONFIG "$CONFIG"
    echo "✓ Cleared the recorded CLOUDFLARED_CONFIG entry when it matched $CONFIG"
fi

DNS_REMAINS=false
if [ -n "$HOSTNAME" ]; then
    set +e
    dns_has_record "$HOSTNAME"
    dns_status=$?
    set -e
    if [ "$dns_status" -ne 1 ]; then
        DNS_REMAINS=true
    fi
fi

if [ "$DNS_REMAINS" = true ]; then
    state_update "dns_cleanup_required=true" "stage=teardown_dns_remaining"
    echo "" >&2
    echo "⚠ The DNS record for $HOSTNAME still exists or could not be verified as removed." >&2
    echo "  cloudflared has no dependable DNS-route deletion command." >&2
    echo "  Open the Cloudflare dashboard for this zone, go to DNS > Records, and delete:" >&2
    echo "  $HOSTNAME" >&2
    echo "  Diagnostic state remains at: $STATE_FILE" >&2
    exit 1
fi

rm -f "$STATE_FILE"
echo ""
echo "✓ Stable teardown complete. No configured relay DNS record remains."

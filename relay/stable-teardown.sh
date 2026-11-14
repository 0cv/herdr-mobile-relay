#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
export PATH="$HOME/.local/bin:$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
ENV_FILE="$(canonical_file_path "$ENV_FILE")"
refuse_if_tailscale_selected "$ENV_FILE" || exit 1
STATE_FILE="${HERDR_STABLE_STATE_FILE:-$(dirname "$ENV_FILE")/stable-setup.json}"

require_supported_platform

if [ ! -f "$STATE_FILE" ]; then
    echo "✗ No stable setup state was found at $STATE_FILE." >&2
    exit 1
fi

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
MISROUTED_HOSTNAME="$(state_get misrouted_hostname)"
ORIGIN_CERT="${TUNNEL_ORIGIN_CERT:-$HOME/.cloudflared/cert.pem}"
if [ -z "$MISROUTED_HOSTNAME" ] && [ -n "$HOSTNAME" ]; then
    CERT_ZONE="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
    if [ -n "$CERT_ZONE" ] && ! hostname_in_zone "$HOSTNAME" "$CERT_ZONE"; then
        MISROUTED_HOSTNAME="$HOSTNAME.$CERT_ZONE"
        state_update "misrouted_hostname=$MISROUTED_HOSTNAME"
    fi
fi


SERVICE_FILE="$(relay_service_file || true)"
case "$(uname -s)" in
    Darwin) SERVICE="$RELAY_LAUNCHD_LABEL" ;;
    Linux) SERVICE="$RELAY_SYSTEMD_LABEL" ;;
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
REMAINING_DNS=()
for dns_name in "$HOSTNAME" "$MISROUTED_HOSTNAME"; do
    if [ -z "$dns_name" ] || [[ " ${REMAINING_DNS[*]-} " == *" $dns_name "* ]]; then
        continue
    fi
    set +e
    dns_has_record "$dns_name"
    dns_status=$?
    set -e
    if [ "$dns_status" -ne 1 ]; then
        DNS_REMAINS=true
        REMAINING_DNS+=("$dns_name")
    fi
done

if [ "$DNS_REMAINS" = true ]; then
    state_update "dns_cleanup_required=true" "stage=teardown_dns_remaining"
    echo "" >&2
    echo "⚠ Cloudflare DNS records still exist or could not be verified as removed." >&2
    echo "  cloudflared has no dependable DNS-route deletion command." >&2
    echo "  Open the Cloudflare dashboard, go to DNS > Records, and delete:" >&2
    printf '  %s\n' "${REMAINING_DNS[@]}" >&2
    echo "  Diagnostic state remains at: $STATE_FILE" >&2
    exit 1
fi

rm -f "$STATE_FILE"
echo ""
echo "✓ Stable teardown complete. No configured relay DNS record remains."

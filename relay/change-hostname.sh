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

# A local resolver may retain the NXDOMAIN from before the route was created.
# Prefer a fresh public DNS answer for both edge checks; older curl builds that
# lack DNS-over-HTTPS keep the system-resolver behavior.
PUBLIC_CURL_ARGS=()
if curl --help all 2>/dev/null | grep -q -- '--doh-url'; then
    PUBLIC_CURL_ARGS=(--doh-url "${HERDR_DOH_URL:-https://cloudflare-dns.com/dns-query}")
fi

public_curl() {
    curl "${PUBLIC_CURL_ARGS[@]}" "$@"
}


# cloudflared's origin certificate is issued for exactly one zone, and it will
# happily turn a name outside that zone into a subdomain of it. The certificate
# says which zone it covers, so the mismatch can be refused before any record
# exists rather than discovered afterwards from a stray CNAME.
ORIGIN_CERT="${TUNNEL_ORIGIN_CERT:-$HOME/.cloudflared/cert.pem}"

cert_zone_name() {
    local payload
    local zone_id
    local api_token

    [ -r "$ORIGIN_CERT" ] || return 1
    payload="$(
        sed -n '/BEGIN ARGO TUNNEL TOKEN/,/END ARGO TUNNEL TOKEN/p' "$ORIGIN_CERT" |
            sed '1d;$d' | tr -d '\n' | base64 -d 2>/dev/null
    )" || return 1
    zone_id="$(printf '%s' "$payload" | sed -n 's/.*"zoneID":"\([^"]*\)".*/\1/p')"
    api_token="$(printf '%s' "$payload" | sed -n 's/.*"apiToken":"\([^"]*\)".*/\1/p')"
    [ -n "$zone_id" ] && [ -n "$api_token" ] || return 1
    curl --fail --silent --show-error --max-time 10 \
        -H "Authorization: Bearer $api_token" \
        "https://api.cloudflare.com/client/v4/zones/$zone_id" 2>/dev/null |
        sed -n 's/.*"result":{"id":"[^"]*","name":"\([^"]*\)".*/\1/p' | head -1
}

# Signing in for another zone replaces the certificate, so the old one is kept:
# routes in the previous zone still need it, and a cancelled sign-in must leave
# this machine exactly as it was.
relogin_for_zone() {
    local zone="$1"
    local backup="$ORIGIN_CERT.$(date +%Y%m%d%H%M%S)"

    echo "▸ Signing in to Cloudflare for $zone."
    echo "  The current certificate is kept as $backup."
    mv "$ORIGIN_CERT" "$backup" || return 1
    if ! cloudflared tunnel login; then
        mv -f "$backup" "$ORIGIN_CERT"
        echo "✗ Sign-in did not finish; the previous certificate is back." >&2
        return 1
    fi
    [ -r "$ORIGIN_CERT" ] || {
        mv -f "$backup" "$ORIGIN_CERT"
        echo "✗ Sign-in produced no certificate; the previous one is back." >&2
        return 1
    }
    return 0
}

hostname_in_zone() {
    case "$1" in
        "$2" | *".$2") return 0 ;;
    esac
    return 1
}

CERT_ZONE="$(cert_zone_name || true)"
if [ -n "$CERT_ZONE" ] && ! hostname_in_zone "$NEW_HOSTNAME" "$CERT_ZONE"; then
    echo ""
    echo "▸ This tunnel's Cloudflare certificate covers $CERT_ZONE, not"
    echo "  $NEW_HOSTNAME. Routing it now would create"
    echo "  $NEW_HOSTNAME.$CERT_ZONE instead, so it has to be signed in first."
    if [ "${HERDR_CHANGE_HOSTNAME_RELOGIN:-}" = "false" ] || [ ! -t 0 ]; then
        echo "✗ Run 'cloudflared tunnel login' and pick the zone that owns" >&2
        echo "  $NEW_HOSTNAME, then run this again. Nothing was changed." >&2
        exit 1
    fi
    if ! read -r -p "Sign in to Cloudflare for ${NEW_HOSTNAME#*.} now? [Y/n]: " RELOGIN_ANSWER; then
        RELOGIN_ANSWER=n
    fi
    case "${RELOGIN_ANSWER:-y}" in
        y | Y | yes | YES) ;;
        *)
            echo "Left unchanged." >&2
            exit 1
            ;;
    esac
    if ! relogin_for_zone "${NEW_HOSTNAME#*.}"; then
        exit 1
    fi
    CERT_ZONE="$(cert_zone_name || true)"
    if [ -n "$CERT_ZONE" ] && ! hostname_in_zone "$NEW_HOSTNAME" "$CERT_ZONE"; then
        echo "✗ The new certificate covers $CERT_ZONE, still not $NEW_HOSTNAME." >&2
        echo "  Nothing was changed." >&2
        exit 1
    fi
fi

echo ""
printf '▸ Routing %s to %s..' "$NEW_HOSTNAME" "$TUNNEL"
if ! ROUTE_OUTPUT="$(cloudflared tunnel route dns "$TUNNEL" "$NEW_HOSTNAME" 2>&1)"; then
    echo ""
    echo "✗ Cloudflare refused the route:" >&2
    printf '%s\n' "$ROUTE_OUTPUT" >&2
    echo "  A name already pointing somewhere else has to be freed first." >&2
    exit 1
fi
echo " ✓"

# cloudflared exits 0 even when it did something else than asked: an origin
# certificate is scoped to one zone, and a name outside it becomes a subdomain
# of the zone the certificate covers - ask for relay.new.example and get
# relay.new.example.old.example, silently. It names what it created, so compare
# before anything local changes; nothing has been touched yet at this point.
ROUTED_NAME="$(
    printf '%s\n' "$ROUTE_OUTPUT" |
        sed -n 's/.*Added CNAME \([^ ]*\) which will route.*/\1/p;s/.*INF \([^ ]*\) is already configured.*/\1/p' |
        head -1
)"
if [ -n "$ROUTED_NAME" ] && [ "$ROUTED_NAME" != "$NEW_HOSTNAME" ]; then
    echo "✗ Cloudflare created $ROUTED_NAME, not $NEW_HOSTNAME." >&2
    echo "  This tunnel's origin certificate only covers one zone, so a name" >&2
    echo "  outside it is treated as a subdomain of that zone. Nothing here was" >&2
    echo "  changed; the relay still serves $CURRENT_HOSTNAME." >&2
    echo "  Run 'cloudflared tunnel login' and pick the zone that owns" >&2
    echo "  $NEW_HOSTNAME, then run this again. Delete the stray $ROUTED_NAME" >&2
    echo "  record in Cloudflare." >&2
    exit 1
fi

# The edge has to answer for the new name before the ingress stops serving the
# old one. Any HTTP status proves DNS and the tunnel are wired: until the
# ingress moves the tunnel replies 404, which is exactly the proof needed.
printf '▸ Waiting for the edge to answer %s' "$NEW_HOSTNAME"
DNS_DEADLINE=$((SECONDS + ${HERDR_STABLE_DNS_TIMEOUT:-60}))
while ! public_curl -sS --max-time 5 -o /dev/null "https://$NEW_HOSTNAME/healthz" 2>/dev/null; do
    if [ "$SECONDS" -ge "$DNS_DEADLINE" ]; then
        echo ""
        echo "✗ $NEW_HOSTNAME does not resolve to Cloudflare yet." >&2
        echo "  Nothing here was changed; the relay still serves $CURRENT_HOSTNAME." >&2
        exit 1
    fi
    printf '.'
    sleep 2
done
echo " ✓"

# Only now does anything local move. Every failure below restores this copy, so
# the relay is never left serving a name that answers nowhere.
CONFIG_BACKUP="$CONFIG.herdr-previous"
cp "$CONFIG" "$CONFIG_BACKUP"
restore_config() {
    cp "$CONFIG_BACKUP" "$CONFIG"
    "$SCRIPT_DIR/service.sh" install >/dev/null 2>&1 || true
    echo "▸ Restored $CURRENT_HOSTNAME; the relay is reachable again." >&2
}

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
# old name and refuse to finish. Assignments are key=value.
STATE_FILE="${HERDR_STABLE_STATE_FILE:-$(dirname "$ENV_FILE")/stable-setup.json}"
if [ -f "$STATE_FILE" ]; then
    if "$(relay_binary)" stable-state update "$STATE_FILE" "hostname=$NEW_HOSTNAME" >/dev/null 2>&1; then
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
        public_curl -fsS --max-time 5 "https://$NEW_HOSTNAME/healthz" > "$WORK_DIR/public.json" 2>/dev/null &&
        "$(relay_binary)" stable-state health-match "$WORK_DIR/local.json" "$WORK_DIR/public.json" \
            2>/dev/null; then
        echo " ✓"
        break
    fi
    if [ "$SECONDS" -ge "$DEADLINE" ]; then
        echo ""
        echo "✗ $NEW_HOSTNAME did not answer as this relay." >&2
        restore_config
        echo "  The route to $NEW_HOSTNAME stays; try again once it answers." >&2
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

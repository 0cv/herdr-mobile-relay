#!/usr/bin/env bash
set +x
set -euo pipefail
QUICKSTART_RELEASE_TOKEN=${GH_TOKEN:-${GITHUB_TOKEN:-}}
RELAY_UPDATER_TOKEN_FILE=${HERDR_GITHUB_TOKEN_FILE:-}
export -n QUICKSTART_RELEASE_TOKEN RELAY_UPDATER_TOKEN_FILE
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
echo "🐑 Herdr Mobile Relay quick start"
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RELAY_PID=""
TUNNEL_PID=""
TAILSCALE_ACTIVE=""
LOG_FILE=""

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
export HERDR_RELAY_ENV="$ENV_FILE"

cleanup() {
    if [ -n "$TUNNEL_PID" ] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
        kill "$TUNNEL_PID" 2>/dev/null || true
        wait_for_exit "$TUNNEL_PID" 5
    fi
    if [ -n "$TAILSCALE_ACTIVE" ]; then
        "$RELAY_BIN" tailscale off --port 9443 --relay-port "$PORT" || true
        TAILSCALE_ACTIVE=""
    fi
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
        wait_for_exit "$RELAY_PID" 5
    fi
    if [ -n "$LOG_FILE" ]; then
        rm -f "$LOG_FILE"
    fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

require_supported_platform
ensure_relay_env "$ENV_FILE" "" "$QUICKSTART_RELEASE_TOKEN"
unset QUICKSTART_RELEASE_TOKEN
load_relay_env "$ENV_FILE"
RELAY_UPDATER_TOKEN_FILE=${HERDR_GITHUB_TOKEN_FILE:-$RELAY_UPDATER_TOKEN_FILE}
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE

wait_for_exit() {
    local pid="$1"
    local attempts="$2"
    local attempt
    for ((attempt = 0; attempt < attempts; attempt++)); do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" 2>/dev/null || true
            return
        fi
        sleep 1
    done
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}

RELAY_BIN="$(relay_binary)"
if ! command -v herdr >/dev/null 2>&1 && [ -z "${HERDR_BIN:-}" ]; then
    echo "✗ herdr is required. Install Herdr, then run Quick Start again."
    exit 1
fi
PORT="${HERDR_RELAY_PORT:-8375}"
HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
GATEWAY_URL="$(gateway_url "$ENV_FILE")"
TUNNEL_TARGET_HOST="$HOST"
if [ "$TUNNEL_TARGET_HOST" = "0.0.0.0" ]; then
    TUNNEL_TARGET_HOST="127.0.0.1"
fi

# The quick tunnel serves the phone app from a hostname that changes on every
# launch, so every device enrolled under the previous hostname is stranded.
# Each tunnel launch starts with an empty device list and re-arms the one-use
# bootstrap invitation so the printed setup link pairs the phone again. A
# gateway and Tailscale keep stable identities, so their pairings survive a restart.
if [ -z "$GATEWAY_URL" ] && [ "${HERDR_DEV_TRANSPORT:-}" != tailscale ]; then
    export HERDR_RELAY_REARM_BOOTSTRAP=1
fi

if [ "${HERDR_DEV_TRANSPORT:-}" = tailscale ]; then
    if [ "${HERDR_DEV_TUNNEL:-}" != 1 ] || [ -n "$GATEWAY_URL" ]; then
        echo "✗ Tailscale dev tunnel requires isolated dev state without a configured gateway." >&2
        exit 1
    fi
    URL="$("$RELAY_BIN" tailscale address --port 9443)" || exit $?
fi

if [ "${HERDR_DEV_TUNNEL:-}" != 1 ] && installed_relay_service_active; then
    # The checkout's dev tunnel intentionally uses isolated ports and state,
    # so it may coexist with the installed production service.
    echo "▸ Restarting the installed background relay instead of starting a duplicate..."
    restart_installed_relay_service
    if ! HEALTH="$(wait_for_relay_health "$PORT")"; then
        echo "✗ The installed relay did not become healthy after restart."
        echo "  Run the Status action for service logs and diagnostics."
        exit 1
    fi
    echo "✓ Background relay ready: $HEALTH"
    echo ""
    exec "$SCRIPT_DIR/setup-link.sh"
fi

# 1. Start the verified packaged relay.
echo "▸ Starting relay on $HOST:$PORT..."
HERDR_GITHUB_TOKEN_FILE="$RELAY_UPDATER_TOKEN_FILE" "$RELAY_BIN" serve &
RELAY_PID=$!
sleep 2

if ! kill -0 $RELAY_PID 2>/dev/null; then
    echo "✗ Relay failed to start. Check if port $PORT is in use."
    exit 1
fi

# 2. Reach the phone. A configured gateway replaces the tunnel entirely: the
# relay dials the gateway itself, so nothing here has to be installed or
# supervised, and the QR is printed once the relay confirms registration.
if [ -n "$GATEWAY_URL" ]; then
    printf '▸ Registering with the gateway at %s..' "$GATEWAY_URL"
    if ! wait_for_gateway_registration "$PORT" 30 1; then
        echo ""
        echo "✗ The relay did not register with the gateway at $GATEWAY_URL."
        echo "  Check the URL and this computer's outbound HTTPS access, then rerun."
        exit 1
    fi
    echo " ✓"

    HOST_LABEL="$(host_label)"
    SETUP_FRAGMENT="$(build_transport_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL")"
    PHONE_APP_BASE="$(gateway_phone_app_base_url "$ENV_FILE")"
    record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
    PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"

    ARMED=0
    arm_setup_link "$ENV_FILE" || ARMED=$?
    echo ""
    echo "✓ Relay ready!"
    echo ""
    print_phone_setup "$PHONE_URL"
    echo ""
    print_setup_link_arming "$ARMED"
    echo "  No Cloudflare account, domain, or cloudflared install is involved."
    echo "  The gateway only copies encrypted frames it cannot read, and the phone"
    echo "  upgrades to a direct connection whenever the network allows it."
    echo "  The link configures this relay automatically and removes the token from the address bar."
    echo "  Keep this terminal open; press Ctrl-C here to stop the quick start."
    echo ""
    echo "  Manual setup details:"
    echo "  Gateway:    $GATEWAY_URL"
    echo "  Token:      $HERDR_RELAY_TOKEN"
    echo ""

    wait "$RELAY_PID"
elif [ "${HERDR_DEV_TRANSPORT:-}" = tailscale ]; then
    echo "▸ Publishing $URL via Tailscale Serve: only HTTPS port 9443 is added and removed on exit."
    "$RELAY_BIN" tailscale serve --port 9443 --relay-port "$PORT" &
    TUNNEL_PID=$!
    sleep 2
    if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
        wait "$TUNNEL_PID" || exit $?
        echo "✗ Tailscale Serve stopped before the relay was ready." >&2
        exit 1
    fi
    TAILSCALE_ACTIVE=1

    HOST_LABEL="$(host_label)"
    RELAY_URL="wss://${URL#https://}"
    SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
    PHONE_APP_BASE="$(choose_phone_app_base_url "$URL" "$ENV_FILE")"
    if [ "$PHONE_APP_BASE" != "$URL" ]; then
        record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
    fi
    PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    DIRECT_URL="$URL/#$SETUP_FRAGMENT"

    ARMED=0
    arm_setup_link "$ENV_FILE" || ARMED=$?
    echo ""
    echo "✓ Relay ready!"
    echo ""
    print_phone_setup "$PHONE_URL"
    if [ "$PHONE_URL" != "$DIRECT_URL" ]; then
        echo ""
        echo "  Direct browser fallback:"
        print_phone_setup_url "$DIRECT_URL"
    fi
    echo ""
    print_setup_link_arming "$ARMED"
    echo "  The phone app and relay are both served through Tailscale."
    echo "  Keep this terminal open; press Ctrl-C to stop the dev tunnel."
    echo ""
    echo "  Tailscale URL: $URL"
    echo "  WebSocket:     $RELAY_URL"
    echo ""

    while kill -0 "$RELAY_PID" 2>/dev/null && kill -0 "$TUNNEL_PID" 2>/dev/null; do
        sleep 2
    done
    if ! kill -0 "$RELAY_PID" 2>/dev/null; then
        echo "✗ The relay process stopped." >&2
    else
        echo "✗ Tailscale Serve stopped." >&2
    fi
    exit 1
elif command -v cloudflared >/dev/null 2>&1; then
    echo "▸ Starting Cloudflare tunnel..."
    LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/herdr-cloudflared.XXXXXX")"
    (unset HERDR_GITHUB_TOKEN_FILE; exec cloudflared tunnel --config /dev/null --url "http://$TUNNEL_TARGET_HOST:$PORT") >"$LOG_FILE" 2>&1 &
    TUNNEL_PID=$!

    URL=""
    for _ in $(seq 1 30); do
        if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
            echo "✗ Cloudflare tunnel failed:"
            sed -n '1,120p' "$LOG_FILE"
            exit 1
        fi
        URL="$(sed -nE 's/.*(https:\/\/[^ ]*\.trycloudflare\.com).*/\1/p' "$LOG_FILE" | head -1)"
        if [ -n "$URL" ]; then
            break
        fi
        sleep 1
    done

    if [ -z "$URL" ]; then
        echo "✗ Timed out waiting for Cloudflare tunnel URL. Recent cloudflared output:"
        tail -40 "$LOG_FILE"
        exit 1
    fi

    # Quick-tunnel DNS takes a few seconds to go live after cloudflared prints
    # the URL. Opening the link too early makes some home routers cache the
    # miss for up to 30 minutes, so wait until the name resolves publicly
    # before showing it. DNS-over-HTTPS keeps the local resolver untouched
    # until then.
    TUNNEL_HOST="${URL#https://}"
    printf '▸ Waiting for the tunnel hostname to go live..'
    DNS_READY=""
    DNS_DEADLINE=$((SECONDS + 45))
    while [ "$SECONDS" -lt "$DNS_DEADLINE" ]; do
        if curl -fsS --max-time 5 -H 'accept: application/dns-json' \
                "https://cloudflare-dns.com/dns-query?name=$TUNNEL_HOST&type=A" 2>/dev/null \
                | grep -q '"Answer"'; then
            DNS_READY=1
            break
        fi
        printf '.'
        sleep 2
    done
    if [ -n "$DNS_READY" ]; then
        echo " ✓"
    else
        echo ""
        echo "  Warning: the tunnel hostname does not resolve yet. If the link"
        echo "  does not open on your phone, wait a minute and scan again."
    fi

    HOST_LABEL="$(host_label)"
    RELAY_URL="wss://${URL#https://}"
    SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
    PHONE_APP_BASE="$(choose_phone_app_base_url "$URL" "$ENV_FILE" temporary)"
    if [ "$PHONE_APP_BASE" != "$URL" ]; then
        record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
    fi
    PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    DIRECT_URL="$URL/#$SETUP_FRAGMENT"

    ARMED=0
    arm_setup_link "$ENV_FILE" || ARMED=$?
    echo ""
    echo "✓ Relay ready!"
    echo ""
    print_phone_setup "$PHONE_URL"
    if [ "$PHONE_URL" != "$DIRECT_URL" ]; then
        echo ""
        echo "  Direct browser fallback:"
        print_phone_setup_url "$DIRECT_URL"
    fi
    echo ""
    print_setup_link_arming "$ARMED"
    echo "  The phone app and relay are both served by this tunnel."
    echo "  The link configures this relay automatically and removes the token from the address bar."
    echo "  Keep this terminal open; press Ctrl-C here to stop the quick start."
    echo ""
    echo "  Manual setup details:"
    echo "  Tunnel URL: $URL"
    echo "  WebSocket:  wss://${URL#https://}"
    echo "  Token:      $HERDR_RELAY_TOKEN"
    echo ""

    # Watch both processes; macOS ships bash 3.2, which lacks wait -n, so poll.
    # Without this, a dead relay would leave the tunnel serving 502s silently.
    while kill -0 "$RELAY_PID" 2>/dev/null && kill -0 "$TUNNEL_PID" 2>/dev/null; do
        sleep 2
    done
    if ! kill -0 "$RELAY_PID" 2>/dev/null; then
        echo "✗ The relay process stopped. The tunnel cannot serve the app without it."
        if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
            echo "  Run Herdr Mobile Relay: Quick Start again; check port $PORT if it fails again."
        else
            echo "  Rerun make quick-start; check port $PORT if it fails again."
        fi
        exit 1
    fi
    echo "✗ Cloudflare tunnel stopped. Recent cloudflared output:"
    if [ -f "$LOG_FILE" ]; then
        tail -40 "$LOG_FILE"
    fi
    exit 1
else
    echo ""
    echo "✓ Relay and phone app running on http://$HOST:$PORT"
    echo "  Token: $HERDR_RELAY_TOKEN"
    echo ""
    echo "  Install cloudflared for remote access:"
    if [ "$(uname -s)" = "Darwin" ]; then
        echo "    brew install cloudflared"
    else
        echo "    https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/"
    fi
    echo ""
    wait $RELAY_PID
fi

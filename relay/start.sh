#!/bin/bash
set -euo pipefail
echo "🐑 Herdr Mobile Relay quick start"
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
RELAY_PID=""
TUNNEL_PID=""
LOG_FILE=""

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

cleanup() {
    if [ -n "$TUNNEL_PID" ] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
        kill "$TUNNEL_PID" 2>/dev/null || true
    fi
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
    fi
    if [ -n "$LOG_FILE" ]; then
        rm -f "$LOG_FILE"
    fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

require_supported_platform
ensure_relay_env "$ENV_FILE"
load_relay_env "$ENV_FILE"

if ! command -v uv >/dev/null 2>&1; then
    echo "✗ uv is required. Run make quick-start to install missing tools."
    exit 1
fi
if ! command -v herdr >/dev/null 2>&1 && [ -z "${HERDR_BIN:-}" ]; then
    echo "✗ herdr is required. Run make quick-start to install missing tools."
    exit 1
fi
PORT="${HERDR_RELAY_PORT:-8375}"
HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
TUNNEL_TARGET_HOST="$HOST"
if [ "$TUNNEL_TARGET_HOST" = "0.0.0.0" ]; then
    TUNNEL_TARGET_HOST="127.0.0.1"
fi

# 1. Start relay (uv auto-installs deps)
echo "▸ Starting relay on $HOST:$PORT..."
uv run "$SCRIPT_DIR/herdr_relay.py" &
RELAY_PID=$!
sleep 2

if ! kill -0 $RELAY_PID 2>/dev/null; then
    echo "✗ Relay failed to start. Check if port 8375 is in use."
    exit 1
fi

# 2. Start tunnel
if command -v cloudflared >/dev/null 2>&1; then
    echo "▸ Starting Cloudflare tunnel..."
    LOG_FILE="$(mktemp "${TMPDIR:-/tmp}/herdr-cloudflared.XXXXXX")"
    cloudflared tunnel --config /dev/null --url "http://$TUNNEL_TARGET_HOST:$PORT" >"$LOG_FILE" 2>&1 &
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
    echo "▸ Waiting for the tunnel hostname to go live..."
    DNS_READY=""
    for _ in $(seq 1 30); do
        if curl -fsS --max-time 5 -H 'accept: application/dns-json' \
                "https://cloudflare-dns.com/dns-query?name=$TUNNEL_HOST&type=A" 2>/dev/null \
                | grep -q '"Answer"'; then
            DNS_READY=1
            break
        fi
        sleep 2
    done
    if [ -z "$DNS_READY" ]; then
        echo "  Warning: the tunnel hostname does not resolve yet. If the link"
        echo "  does not open on your phone, wait a minute and scan again."
    fi

    HOST_LABEL="$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo relay)"
    SETUP_FRAGMENT="$(uv run python -c 'import sys, urllib.parse; print(urllib.parse.urlencode({"setup": sys.argv[1], "label": sys.argv[2]}))' "$HERDR_RELAY_TOKEN" "$HOST_LABEL")"
    PHONE_URL="$URL/#$SETUP_FRAGMENT"

    # QR code of the setup link, so phones can scan instead of typing. Empty on
    # failure (e.g. offline before segno is cached); the link below still works.
    QR_CODE="$(uv run --quiet --with segno python -c '
import io, sys
import segno
buf = io.StringIO()
segno.make(sys.argv[1]).terminal(out=buf, compact=True, border=2)
sys.stdout.write("\n".join("  " + line for line in buf.getvalue().splitlines()))
' "$PHONE_URL" 2>/dev/null || true)"

    echo ""
    echo "✓ Relay ready!"
    echo ""
    if [ -n "$QR_CODE" ]; then
        echo "  Scan this QR code with your phone camera:"
        echo ""
        printf '%s\n' "$QR_CODE"
        echo ""
        echo "  Or open this private setup link on your phone:"
    else
        echo "  Open this private setup link on your phone:"
    fi
    echo "  $PHONE_URL"
    echo ""
    echo "  The phone app and relay are both served by this tunnel."
    echo "  The link configures this relay automatically and removes the token from the address bar."
    echo "  Keep this terminal open; press Ctrl-C here to stop the quick start."
    echo ""
    echo "  Manual setup details:"
    echo "  Tunnel URL: $URL"
    echo "  WebSocket:  wss://${URL#https://}"
    echo "  Token:      $HERDR_RELAY_TOKEN"
    echo ""

    if ! wait "$TUNNEL_PID"; then
        echo "✗ Cloudflare tunnel stopped. Recent cloudflared output:"
        if [ -f "$LOG_FILE" ]; then
            tail -40 "$LOG_FILE"
        fi
        exit 1
    fi
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

#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

if [ -z "${HERDR_RELAY_ENV:-}" ]; then
    SERVICE_ENV="$(installed_service_env_file)"
    if [ -n "$SERVICE_ENV" ]; then
        export HERDR_RELAY_ENV="$SERVICE_ENV"
        echo "Reusing the installed relay configuration:"
        echo "  $SERVICE_ENV"
        echo ""
    fi
fi

# This action is itself the explicit Cloudflare choice. It can be reached
# directly from the setup menu as well as through the transport chooser, so the
# wrapper—not only the chooser—must remove a previously selected gateway before
# stable-setup and setup-link decide which transport to configure and encode.
ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
if [ -e "$(tailscale_session_file "$ENV_FILE")" ]; then
    echo "✗ Cloudflare service installation is unavailable while a foreground Tailscale Serve session is active." >&2
    echo "  Stop the pane before changing transports." >&2
    exit 1
fi
CURRENT_TRANSPORT="$(relay_transport_mode "$ENV_FILE")"
if [ "$CURRENT_TRANSPORT" != cloudflare ]; then
    set_gateway_url "$ENV_FILE" ""
    unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
    unset HERDR_TAILSCALE_ORIGIN HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_RUN_ID
    if [ "$CURRENT_TRANSPORT" = gateway ]; then
        echo "Switching this relay from the WebRTC gateway to Cloudflare."
    else
        echo "Switching this relay from $CURRENT_TRANSPORT to Cloudflare."
    fi
    echo ""
fi

echo "🐑 Herdr Mobile Relay stable tunnel setup"
echo ""
echo "This wizard provisions or reuses a named Cloudflare tunnel, installs the"
echo "background service, and verifies the public relay before showing its QR."
echo "If you only want to try the relay, run Quick Start instead:"
echo "  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events"
echo ""

if ! HERDR_STABLE_SETUP_WRAPPED=1 "$SCRIPT_DIR/stable-setup.sh"; then
    echo ""
    echo "Stable setup did not complete. Its state is resumable; use the exact"
    echo "rerun command printed above after correcting the reported problem."
    pause_before_close
    exit 1
fi

pause_before_close

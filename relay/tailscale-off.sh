#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

if [ -z "${HERDR_RELAY_ENV:-}" ]; then
    SERVICE_ENV="$(installed_service_env_file)"
    [ -z "$SERVICE_ENV" ] || export HERDR_RELAY_ENV="$SERVICE_ENV"
fi
ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
assert_service_env_matches "$ENV_FILE"
if [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_PORT)" != 8443 ]; then
    echo "Always on Tailscale is not selected; nothing changed."
    exit 0
fi
PORT="$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)"
status=0
"$(relay_binary)" tailscale off --port 8443 --relay-port "${PORT:-8375}" || status=$?
if [ "$status" -ne 0 ]; then
    if [ "${HERDR_TAILSCALE_ABANDON:-}" != 1 ]; then
        echo "✗ Tailscale Serve could not be removed (exit $status); mapping may remain, selection kept. Run: tailscale serve --https=8443 off" >&2
        exit 1
    fi
    echo "⚠ Clearing the selection (exit $status); Tailscale Serve mapping may remain. Run: tailscale serve --https=8443 off" >&2
fi
remove_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT
unset HERDR_TAILSCALE_PORT
if [ -n "$(installed_service_env_file)" ]; then
    if [ -n "$(env_file_value "$ENV_FILE" CLOUDFLARED_CONFIG)" ] ||
        [ -n "$(gateway_urls "$ENV_FILE")" ]; then
        "$SCRIPT_DIR/service.sh" install
    else
        "$SCRIPT_DIR/service.sh" uninstall
    fi
fi
if [ "$status" -eq 0 ]; then
    echo "Always on Tailscale is off. Other Serve ports were not changed."
fi

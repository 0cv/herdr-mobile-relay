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
"$(relay_binary)" tailscale off --port 8443 --relay-port "${PORT:-8375}" || exit $?
if [ -n "$(installed_service_env_file)" ]; then
    "$SCRIPT_DIR/service.sh" uninstall
fi
remove_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_PORT
echo "Always on Tailscale is off. Other Serve ports were not changed."

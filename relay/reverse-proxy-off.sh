#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

adopt_installed_service_env
ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
assert_service_env_matches "$ENV_FILE"
if [ -z "$(env_file_value "$ENV_FILE" HERDR_REVERSE_PROXY_HOST)" ]; then
    echo "Your own reverse proxy is not selected; nothing changed."
    exit 0
fi
remove_env_value_atomic "$ENV_FILE" HERDR_REVERSE_PROXY_HOST
remove_env_value_atomic "$ENV_FILE" HERDR_RELAY_HOST
unset HERDR_REVERSE_PROXY_HOST HERDR_RELAY_HOST
if [ -n "$(installed_service_env_file)" ]; then
    if [ -n "$(env_file_value "$ENV_FILE" CLOUDFLARED_CONFIG)" ]; then
        "$SCRIPT_DIR/service.sh" install
    else
        "$SCRIPT_DIR/service.sh" uninstall
    fi
fi
echo "Your own reverse proxy is off. The relay listens on 127.0.0.1 again; your proxy configuration was not changed."

#!/usr/bin/env bash
# Strict installed CLI service: a single process owns the backend, device store,
# route observation and authenticated operator socket. Never run setup on restart.
set +x
set -euo pipefail
[ "$#" -eq 0 ] || { echo 'Invalid installed CLI service arguments.' >&2; exit 78; }
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR" 2>/dev/null)" || exit 78
SERVICE_ENV="$(installed_service_env_file 2>/dev/null)" || exit 78
[ -n "$SERVICE_ENV" ] || exit 78
assert_service_env_matches "$ENV_FILE" >/dev/null 2>&1 || exit 78
literal_env_scan "$ENV_FILE" 2>/dev/null || exit 78
literal_env_get HERDR_RELAY_TRANSPORT
[ "$LITERAL_FOUND" = true ] && [ "$LITERAL_VALUE" = tailscale-cli ] || exit 78
# No ambient-only selectors: the installed definition's literal file, not a
# caller's shell, selects the service identity, roots, route and credentials.
for KEY in HERDR_RELAY_TOKEN HERDR_RELAY_INSTANCE_ID HERDR_RELAY_PORT HERDR_RELAY_PLUGIN_PORT \
    HERDR_TAILSCALE_CLI_BIN HERDR_TAILSCALE_CLI_STATE_ROOT HERDR_TAILSCALE_CLI_COORDINATION_ROOT \
    HERDR_TAILSCALE_CLI_SCOPE HERDR_TAILSCALE_CLI_ORIGIN HERDR_PHONE_APP_URL; do
    literal_env_get "$KEY" || exit 78
    [ "$LITERAL_FOUND" = true ] && [ -n "$LITERAL_VALUE" ] && [ "${!KEY:-}" = "$LITERAL_VALUE" ] || exit 78
done
[ "${HERDR_TAILSCALE_CLI_SCOPE:-}" = production ] || exit 78
RELAY_BIN="$(relay_binary 2>/dev/null)" || exit 78
[ -f "$RELAY_BIN" ] && [ -x "$RELAY_BIN" ] || exit 78
"$RELAY_BIN" tailscale-cli activation-check >/dev/null 2>&1 || {
    echo 'Installed-service CLI startup is disabled: unavailable/non-ready; no service was started.' >&2
    exit 69
}
# Historical wrapper definitions have no durable selection reference or pinned
# physical executable. Only a fresh coordinator-published native definition may
# invoke serve-installed --selection; never upgrade this wrapper by observation.
echo 'Unpinned installed CLI wrapper cannot resume; fresh promotion is required.' >&2
exit 69

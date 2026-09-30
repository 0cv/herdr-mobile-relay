#!/bin/bash
# CLI-backed installed-service startup stays disabled pending separate runtime
# and physical-phone qualification. Real CLI access is foreground-only.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
[ -f "$ENV_FILE" ] || {
    echo "Tailscale CLI relay configuration is missing: $ENV_FILE; service startup remains disabled." >&2
    exit 0
}
load_relay_env "$ENV_FILE"
[ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
    echo "Tailscale CLI service requires HERDR_RELAY_TRANSPORT=tailscale-cli; service startup remains disabled." >&2
    exit 0
}

if ! RELAY_BIN="$(relay_binary)"; then
    echo "Verified relay executable is unavailable; service startup remains disabled." >&2
    exit 0
fi
if ! "$RELAY_BIN" tailscale-cli activation-check >/dev/null; then
    echo "CLI-backed profiles are disabled; service startup remains disabled." >&2
    exit 0
fi

echo "Installed-service CLI startup is disabled pending separate runtime and phone qualification; no Tailscale CLI or relay server was started." >&2
exit 0

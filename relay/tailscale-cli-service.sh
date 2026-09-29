#!/bin/bash
# Persistent CLI-backed Serve service entrypoint. Route setup/removal belongs to
# the explicit setup/recovery workflow; service restarts never mutate Serve.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
[ -f "$ENV_FILE" ] || {
    echo "Tailscale CLI relay configuration is missing: $ENV_FILE" >&2
    exit 78
}
load_relay_env "$ENV_FILE"
[ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
    echo "Tailscale CLI service requires HERDR_RELAY_TRANSPORT=tailscale-cli" >&2
    exit 78
}

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH
if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    HERDR_BIN="$(command -v herdr)"
    export HERDR_BIN
fi
export HERDR_RELAY_HOST=127.0.0.1
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"
RELAY_BIN="$(relay_binary)"

# The relay owns its own cancellation, route-drift admission monitor, and
# device-store lock. exec preserves launchd/systemd signals and exit status.
exec "$RELAY_BIN" serve

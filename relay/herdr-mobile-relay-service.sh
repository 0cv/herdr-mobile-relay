#!/usr/bin/env bash
set +x
set -euo pipefail
RELAY_UPDATER_TOKEN_FILE=${HERDR_GITHUB_TOKEN_FILE:-}
export -n RELAY_UPDATER_TOKEN_FILE
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR")" || exit 78

if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a
fi
RELAY_UPDATER_TOKEN_FILE=${HERDR_GITHUB_TOKEN_FILE:-$RELAY_UPDATER_TOKEN_FILE}
export -n RELAY_UPDATER_TOKEN_FILE
unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
TRANSPORT="$(relay_service_transport_mode)" || exit 78
if [ -e "$(tailscale_session_file "$ENV_FILE")" ] ||
    [ -e "$(tailscale_external_session_file "$ENV_FILE")" ] ||
    [ "$TRANSPORT" = tailscale ] ||
    [ "$TRANSPORT" = tailscale-external ]; then
    echo "foreground Tailscale Serve transports cannot run under the background service wrapper" >&2
    exit 78
fi
if [ "$TRANSPORT" = tailscale-cli ]; then
    exec "$SCRIPT_DIR/tailscale-cli-service.sh"
fi
# Installed policy is explicit in Go, not an environment-only bootstrap bypass.
# Refuse foreground identities/re-enrollment before spawning either child.
if [ -n "${HERDR_RELAY_RUN_ID:-}" ] || [ -n "${HERDR_RELAY_CONTROL_RUN_ID:-}" ] ||
    [ -n "${HERDR_RELAY_PAIRING_SOCKET:-}" ]; then
    echo "installed service refuses foreground ownership/control identities" >&2
    exit 78
fi
case "${HERDR_RELAY_REARM_BOOTSTRAP:-}" in
    ''|0|false|False|FALSE|f|F) ;;
    *) echo "installed service refuses bootstrap re-enrollment policy" >&2; exit 78 ;;
esac
unset HERDR_RELAY_REARM_BOOTSTRAP

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
# Agents often install their CLI into a per-tool bin directory that the service
# PATH does not include (e.g. ~/.opencode/bin). Append any that exist so the
# relay can detect them; base entries keep precedence.
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH
export HERDR_RELAY_HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"

case "$TRANSPORT" in
    cloudflare)
        CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
        CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
        if [ -z "$CLOUDFLARED_BIN" ] || [ ! -x "$CLOUDFLARED_BIN" ]; then
            echo "cloudflared not found or not executable" >&2
            exit 78
        fi
        if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
            echo "Cloudflare tunnel config not readable" >&2
            exit 78
        fi
        ;;
    gateway)
        validate_service_gateway_urls || { echo "Invalid configured gateway list." >&2; exit 78; }
        ;;
    *) exit 78 ;;
esac

RELAY_BIN="$(relay_binary)"

if [ -z "${HERDR_BIN:-}" ] && command -v herdr >/dev/null 2>&1; then
    HERDR_BIN="$(command -v herdr)"
    export HERDR_BIN
fi

RELAY_PID=""
TUNNEL_PID=""
# No other background jobs are created by this supervisor. Jobspecs refer to
# our Bash job table, never to a possibly reused OS PID when sending signals.
RELAY_JOB=""
TUNNEL_JOB=""

latest_job() {
    local token rest
    # Terminal children still have a job record here; unlike a fixed %2 this
    # also works when Bash has pruned a quickly exited relay before the tunnel.
    read -r token rest <<< "$(LC_ALL=C jobs -l %+ 2>/dev/null)"
    token="${token#\[}"
    printf '%%%s\n' "${token%%\]*}"
}

stop_child() {
    local job="$1" pid="$2"
    if [ "$(jobs -p "$job" 2>/dev/null)" != "$pid" ]; then
        wait "$pid" 2>/dev/null || true
        return
    fi
    kill -CONT "$job" 2>/dev/null || true
    kill -TERM "$job" 2>/dev/null || true
    for _ in 1 2 3 4 5; do
        if [ "$(jobs -pr "$job" 2>/dev/null)" != "$pid" ]; then
            wait "$pid" 2>/dev/null || true
            return
        fi
        sleep 1
    done
    kill -KILL "$job" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}

cleanup() {
    [ -z "$TUNNEL_JOB" ] || stop_child "$TUNNEL_JOB" "$TUNNEL_PID"
    [ -z "$RELAY_JOB" ] || stop_child "$RELAY_JOB" "$RELAY_PID"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Starting herdr relay on $HERDR_RELAY_HOST:$HERDR_RELAY_PORT"
HERDR_GITHUB_TOKEN_FILE="$RELAY_UPDATER_TOKEN_FILE" "$RELAY_BIN" serve-installed &
RELAY_PID=$!
RELAY_JOB="$(latest_job)"

if [ "$TRANSPORT" = cloudflare ]; then
    echo "Starting cloudflared"
    (unset HERDR_GITHUB_TOKEN_FILE; exec "$CLOUDFLARED_BIN" tunnel --config "$CLOUDFLARED_CONFIG" run) &
    TUNNEL_PID=$!
    TUNNEL_JOB="$(latest_job)"
fi

while true; do
    if [ "$(jobs -pr "$RELAY_JOB" 2>/dev/null)" != "$RELAY_PID" ]; then
        status=0
        wait "$RELAY_PID" || status=$?
        echo "herdr relay exited with status $status"
        exit "$status"
    fi

    if [ -n "$TUNNEL_JOB" ] && [ "$(jobs -pr "$TUNNEL_JOB" 2>/dev/null)" != "$TUNNEL_PID" ]; then
        status=0
        wait "$TUNNEL_PID" || status=$?
        echo "cloudflared exited with status $status"
        exit "$status"
    fi

    sleep 5
done

#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a
fi

PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
# Agents often install their CLI into a per-tool bin directory that the service
# PATH does not include (e.g. ~/.opencode/bin). Append any that exist so the
# relay can detect them; base entries keep precedence.
for agent_bin in "$HOME"/.[!.]*/bin; do
    [ -d "$agent_bin" ] && PATH="$PATH:$agent_bin"
done
export PATH
export HERDR_RELAY_HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
export HERDR_RELAY_PORT="${HERDR_RELAY_PORT:-8375}"

ACTIVE_RUNTIME="${HERDR_RELAY_ACTIVE_RUNTIME:-}"
if ! RELEASE_ROOT="$(verified_installed_release)"; then
    echo "Relay service will remain stopped until the verified release is repaired." >&2
    exit 0
fi
RELAY_BIN="$RELEASE_ROOT/herdr-mobile-relay"
TRUSTED_STATE_ROOT="$HOME/.local/state/herdr-mobile-relay"
case "$TRUSTED_STATE_ROOT" in
    /*) ;;
    *)
        echo "Relay service has no trusted absolute supervisor ledger and will remain stopped." >&2
        exit 0
        ;;
esac
STATE_ROOT="$TRUSTED_STATE_ROOT"
STATE_PATH="$STATE_ROOT/supervisor.json"
LOG_DIR="$STATE_ROOT/logs"

record_bootstrap_failure() {
    local reason="$1"
    local state

    echo "$reason" >&2
    if ! state="$("$RELAY_BIN" supervisor-record-failure "$STATE_PATH" 5 "$reason")"; then
        echo "Relay service could not persist a bounded bootstrap failure and will remain stopped." >&2
        exit 0
    fi
    case "$state" in
        *'"status":"retrying"'*) exit 75 ;;
        *'"status":"tripped"'*)
            echo "Relay service supervisor is tripped; run relay/service.sh reset after repairing the cause." >&2
            exit 0
            ;;
        *)
            echo "Relay service received an invalid bootstrap-failure result and will remain stopped." >&2
            exit 0
            ;;
    esac
}

record_trusted_bootstrap_failure() {
    local reason="$1"

    STATE_ROOT="$TRUSTED_STATE_ROOT"
    STATE_PATH="$STATE_ROOT/supervisor.json"
    LOG_DIR="$STATE_ROOT/logs"
    if ! mkdir -p "$STATE_ROOT" "$LOG_DIR" || ! chmod 700 "$STATE_ROOT" "$LOG_DIR"; then
        echo "$reason" >&2
        echo "Relay service cannot secure its trusted supervisor ledger and will remain stopped: $STATE_ROOT" >&2
        exit 0
    fi
    record_bootstrap_failure "$reason"
}

CONFIGURED_STATE_ROOT="${HERDR_RELAY_SUPERVISOR_STATE_DIR:-$TRUSTED_STATE_ROOT}"
case "$CONFIGURED_STATE_ROOT" in
    /*) ;;
    *) record_trusted_bootstrap_failure "HERDR_RELAY_SUPERVISOR_STATE_DIR must be an absolute path" ;;
esac
if ! mkdir -p "$CONFIGURED_STATE_ROOT" "$CONFIGURED_STATE_ROOT/logs" ||
   ! chmod 700 "$CONFIGURED_STATE_ROOT" "$CONFIGURED_STATE_ROOT/logs"; then
    record_trusted_bootstrap_failure "Relay service cannot secure its configured supervisor state directory: $CONFIGURED_STATE_ROOT"
fi
if ! STATE_ROOT="$(cd "$CONFIGURED_STATE_ROOT" && pwd -P)"; then
    record_trusted_bootstrap_failure "Relay service cannot canonicalize its configured supervisor state directory: $CONFIGURED_STATE_ROOT"
fi
STATE_PATH="$STATE_ROOT/supervisor.json"
LOG_DIR="$STATE_ROOT/logs"

if [ -e "$STATE_PATH" ]; then
    if ! PRIOR_STATE="$("$RELAY_BIN" supervisor-status "$STATE_PATH" 2>/dev/null)"; then
        record_bootstrap_failure "invalid prior supervisor state"
    fi
    case "$(json_string_field "$PRIOR_STATE" status)" in
        starting|running)
            record_bootstrap_failure "previous supervisor exited without a clean stop"
            ;;
    esac
fi

MANAGED_DEPLOYMENT="${HERDR_RELAY_MANAGED_DEPLOYMENT-false}"
case "$MANAGED_DEPLOYMENT" in
    true|false) ;;
    *)
        record_bootstrap_failure "HERDR_RELAY_MANAGED_DEPLOYMENT must be exactly true or false"
        ;;
esac
export HERDR_RELAY_MANAGED_DEPLOYMENT="$MANAGED_DEPLOYMENT"

CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-$(command -v cloudflared || true)}"
CLOUDFLARED_CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"

if [ -z "$CLOUDFLARED_BIN" ]; then
    record_bootstrap_failure "cloudflared not found in PATH"
fi

if [ ! -r "$CLOUDFLARED_CONFIG" ]; then
    record_bootstrap_failure "Cloudflare tunnel config not readable: $CLOUDFLARED_CONFIG"
fi

case "$HERDR_RELAY_HOST" in
    127.0.0.1|localhost) LOCAL_HEALTH_URL="http://$HERDR_RELAY_HOST:$HERDR_RELAY_PORT/readyz" ;;
    ::1) LOCAL_HEALTH_URL="http://[::1]:$HERDR_RELAY_PORT/readyz" ;;
    *)
        record_bootstrap_failure "Managed Relay supervision requires a loopback HERDR_RELAY_HOST"
        ;;
esac

PUBLIC_HEALTH_URL="${HERDR_RELAY_PUBLIC_HEALTH_URL:-}"
if [ -z "$PUBLIC_HEALTH_URL" ]; then
    PUBLIC_HOST="$(yaml_scalar hostname "$CLOUDFLARED_CONFIG")"
    if ! valid_hostname "$PUBLIC_HOST"; then
        record_bootstrap_failure "Cloudflare config has no valid public Relay hostname"
    fi
    PUBLIC_HEALTH_URL="https://$PUBLIC_HOST/readyz"
fi

INSTANCE="${HERDR_RELAY_INSTANCE_ID:-}"
if [ -z "$INSTANCE" ]; then
    record_bootstrap_failure "HERDR_RELAY_INSTANCE_ID is required for exact supervised release identity"
fi

set -- supervise \
    --relay "$RELAY_BIN" \
    --release-root "$RELEASE_ROOT" \
    --instance "$INSTANCE" \
    --cloudflared "$CLOUDFLARED_BIN" \
    --cloudflared-config "$CLOUDFLARED_CONFIG" \
    --state "$STATE_PATH" \
    --log-dir "$LOG_DIR" \
    --local-health "$LOCAL_HEALTH_URL" \
    --public-health "$PUBLIC_HEALTH_URL"

case "$MANAGED_DEPLOYMENT" in
    true)
        case "$ACTIVE_RUNTIME" in
            /*) ;;
            *)
                record_bootstrap_failure "HERDR_RELAY_ACTIVE_RUNTIME must name active-runtime.json by absolute path"
                ;;
        esac
        set -- "$@" --managed --active-runtime "$ACTIVE_RUNTIME"
        ;;
    false) ;;
esac

exec "$RELAY_BIN" "$@"

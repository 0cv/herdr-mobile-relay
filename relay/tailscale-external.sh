#!/bin/bash
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
[ -f "$ENV_FILE" ] || {
    echo "✗ Relay configuration is missing. Run setup before choosing operator-owned HTTPS Serve." >&2
    exit 1
}
assert_service_env_matches "$ENV_FILE"
load_relay_env "$ENV_FILE"
require_supported_platform

RELAY_BIN="$(relay_binary)"
ORIGIN="${HERDR_EXTERNAL_HTTPS_ORIGIN:-}"
case "${1:-}" in
    --origin)
        [ "$#" -eq 2 ] || { echo "Usage: $0 [--origin https://HOST[:PORT]]" >&2; exit 2; }
        ORIGIN="$2"
        ;;
    "") ;;
    *)
        echo "Usage: $0 [--origin https://HOST[:PORT]]" >&2
        exit 2
        ;;
esac
if [ -z "$ORIGIN" ]; then
    if ! stdin_is_terminal; then
        echo "✗ An exact HTTPS Serve origin is required (for example https://relay.tailnet.ts.net)." >&2
        exit 1
    fi
    echo "Use only an HTTPS origin that you already route to this computer's loopback relay."
    echo "Herdr will not inspect, create, adopt, or remove any Tailscale Serve/Funnel state."
    read -r -p "Existing HTTPS origin (q to cancel): " ORIGIN || ORIGIN=""
fi
case "$ORIGIN" in
    q|Q|"") echo "Setup cancelled; no Tailscale state was observed or changed."; exit 1 ;;
esac
CANONICAL_ORIGIN="$("$RELAY_BIN" normalize-external-origin "$ORIGIN" 2>/dev/null)" || {
    echo "✗ Origin must be a canonical HTTPS origin without credentials, path, query, fragment, or non-canonical port." >&2
    exit 1
}
[ "$CANONICAL_ORIGIN" = "$ORIGIN" ] || {
    echo "✗ Enter the canonical HTTPS origin exactly as shown; no URL rewriting was performed." >&2
    exit 1
}

CONFIG_DIR="$(dirname "$ENV_FILE")"
SESSION_FILE="$(tailscale_external_session_file "$ENV_FILE")"
CONTROL_SOCKET="$CONFIG_DIR/tailscale-external-control.sock"
if [ -e "$SESSION_FILE" ] || [ -e "$CONTROL_SOCKET" ]; then
    echo "✗ An operator-owned HTTPS Serve relay session or control socket already exists; inspect it before retrying." >&2
    exit 1
fi
if [ -e "$(tailscale_session_file "$ENV_FILE")" ]; then
    echo "✗ A managed Tailscale Serve session is recorded; no operator-owned ingress settings were changed." >&2
    exit 1
fi
if installed_relay_service_definition_present || installed_relay_service_active; then
    echo "✗ Operator-owned HTTPS Serve is foreground-only and cannot share a background relay service." >&2
    echo "  Stop and remove the Herdr Mobile Relay service before retrying." >&2
    exit 1
fi
TOKEN="$(env_file_value "$ENV_FILE" HERDR_RELAY_TOKEN)"
INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"
[ "${#TOKEN}" -eq 32 ] && [ -n "$INSTANCE" ] || {
    echo "✗ Existing relay key and instance identity are required; setup will not create or reset them." >&2
    exit 1
}
HOST="${HERDR_RELAY_HOST:-127.0.0.1}"
[ "$HOST" = 127.0.0.1 ] || {
    echo "✗ Operator-owned HTTPS Serve requires HERDR_RELAY_HOST=127.0.0.1; no public listener was started." >&2
    exit 1
}
PORT="${HERDR_RELAY_PORT:-8375}"
PLUGIN_PORT="${HERDR_RELAY_PLUGIN_PORT:-8376}"
case "$PORT:$PLUGIN_PORT" in
    *[!0-9:]*|:*) echo "✗ Relay ports are invalid." >&2; exit 1 ;;
esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] && [ "$PLUGIN_PORT" -ge 1 ] && [ "$PLUGIN_PORT" -le 65535 ] &&
    [ "$PORT" != "$PLUGIN_PORT" ] || { echo "✗ Relay and plugin ports must be distinct ports from 1 to 65535." >&2; exit 1; }

PHONE_APP_BASE="$(choose_phone_app_base_url "$ORIGIN" "$ENV_FILE" tailscale-external)" || {
    echo "Setup cancelled; no relay or Tailscale state was changed." >&2
    exit 1
}
HERDR_PHONE_APP_URL="$PHONE_APP_BASE"
export HERDR_PHONE_APP_URL

# A private transient record serializes cooperating setup/selection actions
# across the entire foreground lifetime. The exclusive create makes concurrent
# BYO starts fail closed before either can rewrite the saved selection.
RUN_ID="$(generate_instance_id)"
RELAY_PID=""
RELAY_JOB=""
RELAY_LOG="$(mktemp "$CONFIG_DIR/.tailscale-external-relay-log.XXXXXX")"
chmod 600 "$RELAY_LOG"
SESSION_CREATED=0
UNCLEAN_SHUTDOWN=0

write_external_session() {
    local stage="$1"
    local temporary
    local saved_run_id=""
    if [ "$SESSION_CREATED" -eq 0 ]; then
        if (set -o noclobber; {
            printf 'HERDR_EXTERNAL_HTTPS_ORIGIN=%s\n' "$ORIGIN"
            printf 'HERDR_RELAY_PAIRING_SOCKET=%s\n' "$CONTROL_SOCKET"
            printf 'HERDR_RELAY_CONTROL_RUN_ID=%s\n' "$RUN_ID"
            printf 'HERDR_RELAY_STAGE=%s\n' "$stage"
        } > "$SESSION_FILE") 2>/dev/null; then
            chmod 600 "$SESSION_FILE"
            SESSION_CREATED=1
            return 0
        fi
        return 1
    fi
    [ -f "$SESSION_FILE" ] && [ ! -L "$SESSION_FILE" ] || return 1
    saved_run_id="$(sed -n 's/^HERDR_RELAY_CONTROL_RUN_ID=//p' "$SESSION_FILE" | head -1)"
    [ "$saved_run_id" = "$RUN_ID" ] || return 1
    temporary="$(mktemp "$CONFIG_DIR/.tailscale-external-session.XXXXXX")"
    umask 077
    {
        printf 'HERDR_EXTERNAL_HTTPS_ORIGIN=%s\n' "$ORIGIN"
        printf 'HERDR_RELAY_PAIRING_SOCKET=%s\n' "$CONTROL_SOCKET"
        printf 'HERDR_RELAY_CONTROL_RUN_ID=%s\n' "$RUN_ID"
        printf 'HERDR_RELAY_STAGE=%s\n' "$stage"
        if [ -n "$RELAY_PID" ]; then
            printf 'HERDR_RELAY_PID=%s\n' "$RELAY_PID"
        fi
        if [ "$stage" = forced-shutdown ] || [ "$stage" = unclean-shutdown ]; then
            printf 'HERDR_RELAY_LOG=%s\n' "$RELAY_LOG"
        fi
    } > "$temporary"
    chmod 600 "$temporary"
    mv "$temporary" "$SESSION_FILE"
}

remove_external_session() {
    local saved_run_id=""
    if [ "$SESSION_CREATED" -eq 1 ] && [ -f "$SESSION_FILE" ] && [ ! -L "$SESSION_FILE" ]; then
        saved_run_id="$(sed -n 's/^HERDR_RELAY_CONTROL_RUN_ID=//p' "$SESSION_FILE" | head -1)"
        if [ "$saved_run_id" = "$RUN_ID" ]; then
            rm -f "$SESSION_FILE"
        fi
    fi
}

# A child that dies without Go's deferred socket cleanup can leave the private
# control pathname behind. Preserve its run identity on any abnormal exit,
# unexpected socket, or unknown Bash job generation; PID is inspection only.
preserve_unclean_shutdown() {
    local stage="$1"
    UNCLEAN_SHUTDOWN=1
    if ! write_external_session "$stage"; then
        echo "✗ Could not update the operator-owned Serve recovery record after unclean shutdown." >&2
    fi
}

stop_relay() {
    local attempt stop_status=0 details_status=0
    if [ -n "$RELAY_PID" ]; then
        # A numeric PID is not authority to signal: the child may have been
        # reaped and that number reused. Signal only the captured Bash job
        # generation, and refuse to signal when its identity is unknown.
        if [ -z "$RELAY_JOB" ]; then
            preserve_unclean_shutdown unclean-shutdown
            RELAY_PID=""
            return
        fi
        if _child_job_details "$RELAY_JOB" "$RELAY_PID"; then
            kill -TERM "$RELAY_JOB" 2>/dev/null || true
            for ((attempt = 0; attempt < 10; attempt++)); do
                if _child_job_details "$RELAY_JOB" "$RELAY_PID"; then
                    sleep 1
                else
                    details_status=$?
                    break
                fi
            done
            if [ "$details_status" -ne 0 ] && [ "$details_status" -ne 2 ]; then
                preserve_unclean_shutdown unclean-shutdown
                RELAY_PID=""
                return
            fi
            if _child_job_details "$RELAY_JOB" "$RELAY_PID"; then
                kill -KILL "$RELAY_JOB" 2>/dev/null || true
            fi
        else
            details_status=$?
            if [ "$details_status" -ne 2 ]; then
                preserve_unclean_shutdown unclean-shutdown
                RELAY_PID=""
                return
            fi
        fi
        wait "$RELAY_PID" 2>/dev/null || stop_status=$?
        if [ "$stop_status" -eq 137 ]; then
            preserve_unclean_shutdown forced-shutdown
        elif [ "$stop_status" -ne 0 ]; then
            preserve_unclean_shutdown unclean-shutdown
        fi
    fi
    RELAY_PID=""
}

cleanup() {
    stop_relay
    if [ -e "$CONTROL_SOCKET" ] || [ -L "$CONTROL_SOCKET" ]; then
        [ "$UNCLEAN_SHUTDOWN" -eq 1 ] || preserve_unclean_shutdown unclean-shutdown
    fi
    if [ "$UNCLEAN_SHUTDOWN" -eq 1 ]; then
        echo "✗ The relay did not retire cleanly; recovery evidence was retained and any control socket was left untouched." >&2
        echo "  Session: $SESSION_FILE" >&2
        echo "  Socket:  $CONTROL_SOCKET" >&2
        echo "  Log:     $RELAY_LOG" >&2
        echo "  Inspect these and confirm no relay process is live before retrying; Herdr did not remove the socket." >&2
    else
        remove_external_session
        if [ -n "$RELAY_LOG" ]; then
            rm -f "$RELAY_LOG"
        fi
    fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

write_external_session starting || {
    echo "✗ Another operator-owned HTTPS Serve action acquired the foreground session; no settings were changed." >&2
    exit 1
}
# Persist only the explicit transport/origin choice. Pairing credentials and
# phone-app origin are untouched. Invitation creation remains deferred in the
# relay until trusted public HTTPS and the exact release bundle have passed.
clear_tailscale_selection "$ENV_FILE"
remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_URL
remove_env_value_atomic "$ENV_FILE" HERDR_GATEWAY_SELECTION
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
set_env_value_atomic "$ENV_FILE" HERDR_RELAY_TRANSPORT tailscale-external
set_env_value_atomic "$ENV_FILE" HERDR_EXTERNAL_HTTPS_ORIGIN "$ORIGIN"
export HERDR_RELAY_ENV="$ENV_FILE"
export HERDR_RELAY_TRANSPORT=tailscale-external
export HERDR_RELAY_HOST=127.0.0.1
export HERDR_EXTERNAL_HTTPS_ORIGIN="$ORIGIN"
export HERDR_RELAY_PAIRING_SOCKET="$CONTROL_SOCKET"
export HERDR_RELAY_CONTROL_RUN_ID="$RUN_ID"
export HERDR_RELAY_REARM_BOOTSTRAP=0
# BYO HTTPS already supplies public relay ingress; do not request PCP/UPnP router mappings.
export HERDR_REACHABILITY_PORT_MAPPING=0

if ! "$RELAY_BIN" check-port --host 127.0.0.1 --port "$PORT" >/dev/null 2>&1; then
    echo "✗ Relay loopback port $PORT is occupied; no existing process was stopped." >&2
    exit 1
fi

echo "▸ Starting local Herdr relay backend on 127.0.0.1:$PORT..."
"$RELAY_BIN" serve >"$RELAY_LOG" 2>&1 &
RELAY_PID=$!
RELAY_JOB="$(capture_child_job "$RELAY_PID")" || {
    echo "✗ Could not capture the relay's Bash job generation; retaining the private session and log for recovery." >&2
    exit 1
}

HEALTH=""
for attempt in $(seq 1 90); do
    if ! _child_job_details "$RELAY_JOB" "$RELAY_PID"; then
        echo "✗ External Serve relay failed to start. No Tailscale state was observed or changed." >&2
        exit 1
    fi
    if HEALTH="$(curl --noproxy '*' --fail --silent --show-error --connect-timeout 1 --max-time 2 \
        "http://127.0.0.1:$PORT/healthz" 2>/dev/null)" &&
        [ "$(json_string_field "$HEALTH" instance)" = "$INSTANCE" ] &&
        [ "$(json_string_field "$HEALTH" transport)" = tailscale-external ] &&
        [ "$(json_string_field "$HEALTH" external_control_run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$HEALTH" external_https_origin)" = "$ORIGIN" ] &&
        [ "$(json_string_field "$HEALTH" readiness)" = ready ]; then
        break
    fi
    sleep 1
done
[ -n "$HEALTH" ] &&
    [ "$(json_string_field "$HEALTH" instance)" = "$INSTANCE" ] &&
    [ "$(json_string_field "$HEALTH" transport)" = tailscale-external ] &&
    [ "$(json_string_field "$HEALTH" external_control_run_id)" = "$RUN_ID" ] &&
    [ "$(json_string_field "$HEALTH" external_https_origin)" = "$ORIGIN" ] &&
    [ "$(json_string_field "$HEALTH" readiness)" = ready ] || {
    echo "✗ Local relay health, instance, or startup identity did not match; no setup link was printed." >&2
    exit 1
}
require_release_identity "$HEALTH" "$RELAY_BIN" || exit 1

# The selected relay origin must prove its public TLS identity. The selected
# phone-app origin is verified independently below. curl and Go use normal
# system roots/hostname verification; no bypass, trust-store edit, Tailscale
# CLI, or LocalAPI call exists.
PUBLIC_HEALTH=""
for attempt in $(seq 1 60); do
    if ! _child_job_details "$RELAY_JOB" "$RELAY_PID"; then
        echo "✗ External Serve relay exited before HTTPS verification; no setup link was printed." >&2
        exit 1
    fi
    if PUBLIC_HEALTH="$(curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 5 --max-redirs 0 \
        "$ORIGIN/healthz" 2>/dev/null)" &&
        [ "$(json_string_field "$PUBLIC_HEALTH" instance)" = "$INSTANCE" ] &&
        [ "$(json_string_field "$PUBLIC_HEALTH" external_control_run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$PUBLIC_HEALTH" transport)" = tailscale-external ] &&
        [ "$(json_string_field "$PUBLIC_HEALTH" external_https_origin)" = "$ORIGIN" ] &&
        [ "$(json_string_field "$PUBLIC_HEALTH" readiness)" = ready ]; then
        break
    fi
    sleep 1
done
[ -n "$PUBLIC_HEALTH" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" instance)" = "$INSTANCE" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" external_control_run_id)" = "$RUN_ID" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" transport)" = tailscale-external ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" external_https_origin)" = "$ORIGIN" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" readiness)" = ready ] || {
    echo "✗ Trusted HTTPS did not prove this relay instance, control run, and readiness; no invitation was armed." >&2
    exit 1
}
require_release_identity "$PUBLIC_HEALTH" "$RELAY_BIN" || exit 1
verify_phone_app_bundle "$PHONE_APP_BASE" "$RELAY_BIN" || {
    echo "✗ The selected phone-app origin does not serve this exact release's frontend bundle." >&2
    echo "  Keep the operator-owned HTTPS Serve origin for relay health/WSS and select an independently hosted verified Herdr app." >&2
    exit 1
}

write_external_session ready || {
    echo "✗ The external relay session record changed unexpectedly; no link was printed." >&2
    exit 1
}
echo "▸ Verifying the selected Herdr phone app and arming the one-use invitation..."
if ! "$SCRIPT_DIR/setup-link.sh"; then
    echo "✗ Setup link was not printed. The relay is being stopped locally; operator-owned Tailscale ingress remains unchanged." >&2
    exit 1
fi
echo ""
echo "The relay and selected Herdr instance are local to this pane. Ctrl-C stops only Herdr; HTTPS Serve ingress remains operator-owned."
while [ -n "$RELAY_PID" ]; do
    wait_status=0
    wait "$RELAY_PID" || wait_status=$?
    if [ "$wait_status" -eq 137 ]; then
        preserve_forced_shutdown
    fi
    RELAY_PID=""
    [ "$wait_status" -eq 0 ] || exit "$wait_status"
done

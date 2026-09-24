#!/bin/bash
set -euo pipefail

usage() {
    echo "Usage: $0 [--confirm-serve | --help]"
    echo "  --confirm-serve  Consent to tailnet HTTPS exposure of the loopback relay"
    echo "                   for this foreground invocation only (no saved consent)."
    echo "  Without the option, a fresh affirmative answer at a TTY prompt is required."
    echo "  Tailscale must already be installed/authenticated and relay setup complete."
    echo "  All service, identity and route preflight checks still apply."
}

# Keep consent in the original positional argv, not a sourced/exported variable.
# load_relay_env is a function with its own positional parameters; sourcing the
# configuration there cannot replace this invocation's arguments.
case "$#:${1:-}" in
    0:|1:--confirm-serve) ;;
    1:--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
export HERDR_RELAY_ENV="$ENV_FILE"
require_supported_platform

# Read persisted values before loading inherited environment. A persisted
# bootstrap-reset request is unsafe for a stable Tailscale origin and is never
# silently accepted; an inherited value is explicitly overridden below.
PERSISTED_REARM="$(env_file_value "$ENV_FILE" HERDR_RELAY_REARM_BOOTSTRAP || true)"
PERSISTED_TOKEN="$(env_file_value "$ENV_FILE" HERDR_RELAY_TOKEN || true)"
PERSISTED_INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID || true)"
load_relay_env "$ENV_FILE"
[ -n "$PERSISTED_TOKEN" ] && [ -n "${HERDR_RELAY_TOKEN:-}" ] &&
    [ "$PERSISTED_TOKEN" = "$HERDR_RELAY_TOKEN" ] &&
    [ -n "$PERSISTED_INSTANCE" ] && [ "$PERSISTED_INSTANCE" = "${HERDR_RELAY_INSTANCE_ID:-}" ] || {
    echo "✗ Relay credentials are not configured; run setup before Tailscale Serve." >&2
    exit 1
}
MODE="$(relay_transport_mode "$ENV_FILE")"
if [ "$MODE" != tailscale ] && [ "${HERDR_TAILSCALE_REQUEST:-}" != 1 ]; then
    echo "✗ Tailscale startup was requested without tailscale transport selection." >&2
    exit 1
fi
PERSISTED_REARM_NORMALIZED="$(printf '%s' "$PERSISTED_REARM" | tr '[:upper:]' '[:lower:]')"
if [ "$PERSISTED_REARM_NORMALIZED" = 1 ] ||
    [ "$PERSISTED_REARM_NORMALIZED" = true ] ||
    [ "$PERSISTED_REARM_NORMALIZED" = yes ] ||
    [ "$PERSISTED_REARM_NORMALIZED" = on ]; then
    echo "✗ Refusing Tailscale startup because HERDR_RELAY_REARM_BOOTSTRAP is enabled in $ENV_FILE." >&2
    echo "  Remove that setting explicitly; Tailscale startup never resets enrolled devices." >&2
    exit 1
fi
# This assignment is deliberately after every environment load. It prevents a
# stale Quick Start export from reaching the relay process.
export HERDR_RELAY_REARM_BOOTSTRAP=0
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
[ -d "$(dirname "$ENV_FILE")" ] || { echo "✗ Relay configuration directory is missing; run the Quick Start action to prepare it: $(dirname "$ENV_FILE")" >&2; exit 1; }
if installed_relay_service_definition_present || installed_relay_service_active; then
    echo "✗ A Herdr Mobile Relay service definition is installed." >&2
    echo "  Stop and remove the service before using foreground Tailscale Serve." >&2
    exit 1
fi

RELAY_BIN="$(relay_binary)"
TS_BIN="${HERDR_TAILSCALE_BIN:-$(command -v tailscale || true)}"
if [ -z "$TS_BIN" ] || [ ! -x "$TS_BIN" ]; then
    echo "✗ Tailscale CLI is unavailable. Install and authenticate Tailscale first." >&2
    exit 1
fi

PORT="${HERDR_RELAY_PORT:-8375}"
PLUGIN_PORT="${HERDR_RELAY_PLUGIN_PORT:-8376}"
case "$PORT:$PLUGIN_PORT" in
    *[!0-9:]*|*:0|0:*)
        echo "✗ Relay and plugin ports must be positive integers." >&2
        exit 1
        ;;
esac
[ "$PORT" -ge 1 ] && [ "$PLUGIN_PORT" -ge 1 ] &&
    [ "$PORT" -le 65535 ] && [ "$PLUGIN_PORT" -le 65535 ] || {
    echo "✗ Relay and plugin ports must be between 1 and 65535." >&2
    exit 1
}
[ "$PORT" != "$PLUGIN_PORT" ] || {
    echo "✗ Relay and plugin ports must be different." >&2
    exit 1
}
[ "${HERDR_RELAY_HOST:-127.0.0.1}" = 127.0.0.1 ] || {
    echo "✗ Tailscale Serve requires the relay backend to remain on 127.0.0.1." >&2
    exit 1
}
HTTPS_PORT="$(tailscale_https_port)"

SESSION_FILE="$(tailscale_session_file "$ENV_FILE")"
CONFIG_DIR="$(dirname "$ENV_FILE")"
CONTROL_SOCKET="$CONFIG_DIR/tailscale-control.sock"
if [ -e "$SESSION_FILE" ]; then
    echo "✗ A previous Tailscale foreground session record exists: $SESSION_FILE" >&2
    echo "  Inspect the recorded run and remove it only after its processes and Serve route are gone." >&2
    exit 1
fi
if [ -e "$CONTROL_SOCKET" ]; then
    echo "✗ Tailscale pairing control socket already exists: $CONTROL_SOCKET" >&2
    echo "  A stale or foreign owner must be investigated; it will not be unlinked automatically." >&2
    exit 1
fi

if ! "$RELAY_BIN" check-port --host 127.0.0.1 --port "$PORT" --protocol tcp >/dev/null 2>&1 ||
    ! "$RELAY_BIN" check-port --host 127.0.0.1 --port "$PLUGIN_PORT" --protocol udp >/dev/null 2>&1; then
    echo "✗ Relay or Herdr event port is occupied; no existing listener will be reused." >&2
    exit 1
fi

INSPECTION="$(
    "$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT"
)" || {
    echo "✗ Tailscale status/Serve inspection failed without changing configuration." >&2
    echo "  Check that the installed CLI supports structured status and Serve status JSON." >&2
    exit 1
}
if [ "$(json_bool_field "$INSPECTION" serve_inspected)" != true ] ||
    [ "$(json_bool_field "$INSPECTION" exposure_complete)" != true ]; then
    echo "✗ Tailscale exposure inspection is incomplete; nothing was changed." >&2
    exit 1
fi
if [ "$(json_string_field "$INSPECTION" backend_state)" != Running ] ||
    [ "$(json_bool_field "$INSPECTION" logged_in)" != true ]; then
    echo "✗ Tailscale is not running with an authenticated node." >&2
    echo "  Start tailscaled and authenticate it manually; this setup never logs in." >&2
    exit 1
fi
if [ "$(json_bool_field "$INSPECTION" funnel_configured)" != false ]; then
    echo "✗ Existing Tailscale Funnel configuration was found; it was left unchanged." >&2
    exit 1
fi
if [ "$(json_bool_field "$INSPECTION" serve_configured)" != false ]; then
    echo "✗ Existing Tailscale Serve configuration was found; it was left unchanged." >&2
    echo "  Remove or migrate that route manually before selecting this relay." >&2
    exit 1
fi
ORIGIN="$(json_string_field "$INSPECTION" origin)"
[ -n "$ORIGIN" ] || {
    echo "✗ The authenticated Tailscale node has no usable DNS identity." >&2
    exit 1
}
CONFIGURED_ORIGIN="$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_ORIGIN || true)"
if [ -n "$CONFIGURED_ORIGIN" ] && [ "$CONFIGURED_ORIGIN" != "$ORIGIN" ]; then
    echo "✗ Saved Tailscale origin does not match the authenticated node identity." >&2
    echo "  Saved: $CONFIGURED_ORIGIN" >&2
    echo "  Node:  $ORIGIN" >&2
    exit 1
fi

if [ "${1:-}" != --confirm-serve ]; then
    if [ ! -t 0 ]; then
        echo "✗ Explicit Tailscale Serve consent is required." >&2
        echo "  Re-run interactively, or pass --confirm-serve for this invocation only." >&2
        exit 1
    fi
    echo ""
    echo "Tailscale Serve will expose this loopback relay only inside the tailnet:"
    echo "  HTTPS origin: $ORIGIN"
    echo "  Backend:      http://127.0.0.1:$PORT"
    echo "  Lifetime:     this foreground pane; Ctrl-C removes this Serve session"
    echo "  Cleanup:      only this run's relay and Serve session will be stopped"
    read -r -p "Configure this Tailscale Serve session? [y/N] " CONSENT || CONSENT=""
    case "$CONSENT" in
        y|Y|yes|YES) ;;
        *) echo "Setup cancelled before changing Tailscale Serve."; exit 1 ;;
    esac
fi

# The second inspection closes the check/use race. A new route or Funnel setting
# is a conflict, not permission to reset another application.
INSPECTION="$(
    "$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT"
)" || { echo "✗ Tailscale changed before setup could start; retry." >&2; exit 1; }
if [ "$(json_bool_field "$INSPECTION" serve_inspected)" != true ] ||
    [ "$(json_bool_field "$INSPECTION" exposure_complete)" != true ]; then
    echo "✗ Tailscale exposure inspection is incomplete; nothing was changed." >&2
    exit 1
fi
if [ "$(json_bool_field "$INSPECTION" serve_configured)" != false ] ||
    [ "$(json_bool_field "$INSPECTION" funnel_configured)" != false ]; then
    echo "✗ Tailscale Serve/Funnel configuration appeared during confirmation; nothing was changed." >&2
    exit 1
fi

ORIGINAL_ENV_EXISTS=false
[ -f "$ENV_FILE" ] && ORIGINAL_ENV_EXISTS=true
ORIGINAL_ENV=""
[ "$ORIGINAL_ENV_EXISTS" = true ] && ORIGINAL_ENV="$(cat "$ENV_FILE")"
ORIGIN_FILE="$CONFIG_DIR/phone-app-origin-configured"
ORIGINAL_ORIGIN_EXISTS=false
[ -e "$ORIGIN_FILE" ] && ORIGINAL_ORIGIN_EXISTS=true
ORIGINAL_ORIGIN=""
[ "$ORIGINAL_ORIGIN_EXISTS" = true ] && ORIGINAL_ORIGIN="$(cat "$ORIGIN_FILE")"
MUTATED_ENV=false
MUTATED_ENV_CONTENT=""
MUTATED_ORIGIN=false
MUTATED_ORIGIN_CONTENT=""
RELAY_PID=""
SERVE_PID=""
RELAY_JOB=""
SERVE_JOB=""
RELAY_LOG=""
SERVE_LOG=""
RUN_ID=""
COMMITTED=false
CLEANUP_FAILED=false

restore_file_if_unchanged() {
    local path="$1"
    local existed="$2"
    local original="$3"
    local expected="$4"
    local current=""

    [ -e "$path" ] && current="$(cat "$path")"
    if [ "$current" != "$expected" ]; then
        echo "✗ Refusing rollback after concurrent modification of $path." >&2
        return 1
    fi
    if [ "$existed" = true ]; then
        local temp
        temp="$(mktemp "$(dirname "$path")/.tailscale-restore.XXXXXX")"
        printf '%s\n' "$original" > "$temp"
        chmod 600 "$temp"
        mv "$temp" "$path"
    else
        rm -f "$path"
    fi
}

rollback_selection() {
    if [ "$MUTATED_ENV" = true ]; then
        restore_file_if_unchanged "$ENV_FILE" "$ORIGINAL_ENV_EXISTS" "$ORIGINAL_ENV" "$MUTATED_ENV_CONTENT" || return 1
    fi
    if [ "$MUTATED_ORIGIN" = true ]; then
        restore_file_if_unchanged "$ORIGIN_FILE" "$ORIGINAL_ORIGIN_EXISTS" "$ORIGINAL_ORIGIN" "$MUTATED_ORIGIN_CONTENT" || return 1
    fi
}

write_session() {
    local temporary
    temporary="$(mktemp "$CONFIG_DIR/.tailscale-session.XXXXXX")"
    umask 077
    {
        printf 'HERDR_RELAY_RUN_ID=%s\n' "$RUN_ID"
        printf 'HERDR_RELAY_PAIRING_SOCKET=%s\n' "$CONTROL_SOCKET"
        printf 'HERDR_TAILSCALE_ORIGIN=%s\n' "$ORIGIN"
        printf 'HERDR_TAILSCALE_HTTPS_PORT=%s\n' "$HTTPS_PORT"
        printf 'HERDR_RELAY_SUPERVISOR_PID=%s\n' "$RELAY_PID"
        printf 'HERDR_TAILSCALE_SUPERVISOR_PID=%s\n' "$SERVE_PID"
    } > "$temporary"
    chmod 600 "$temporary"
    mv "$temporary" "$SESSION_FILE"
}

cleanup() {
    local status=0
    if [ -n "$SERVE_PID" ]; then
        stop_child_job "$SERVE_JOB" "$SERVE_PID" INT 12 || status=1
    fi
    if [ -n "$RELAY_PID" ]; then
        stop_child_job "$RELAY_JOB" "$RELAY_PID" INT 12 || status=1
    fi
    if [ -n "$SERVE_PID" ]; then
        if ! POST="$("$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null)"; then
            echo "✗ Could not verify Tailscale Serve/Funnel state during cleanup." >&2
            echo "  The foreground session record was retained; inspect it before retrying." >&2
            status=1
        elif [ "$(json_bool_field "$POST" serve_inspected)" != true ] ||
            [ "$(json_bool_field "$POST" exposure_complete)" != true ] ||
            [ "$(json_bool_field "$POST" serve_configured)" != false ] ||
            [ "$(json_bool_field "$POST" funnel_configured)" != false ]; then
            echo "✗ Tailscale Serve/Funnel configuration remains after the foreground session stopped; it was left unchanged." >&2
            echo "  Inspect the recorded session and remove residual configuration only after confirming its owner." >&2
            status=1
        fi
    fi
    if [ "$status" -eq 0 ]; then
        [ -z "$RELAY_LOG" ] || rm -f "$RELAY_LOG"
        [ -z "$SERVE_LOG" ] || rm -f "$SERVE_LOG"
        rm -f "$SESSION_FILE"
    else
        echo "✗ Child cleanup could not verify a managed generation; logs and session record were retained." >&2
    fi
    return "$status"
}
trap 'exit 130' INT TERM
trap 'status=$?; trap - EXIT INT TERM; cleanup || status=1; if [ "$COMMITTED" != true ] && [ "$status" -ne 0 ]; then rollback_selection || status=1; fi; exit "$status"' EXIT

# The serve child is the long-lived ownership holder (S6B2): it creates the
# pairing socket only after NewOwned has validated the published owner. Export
# exactly what that child needs and generate the run id before spawning it; no
# managed file is written until the control `status` acknowledgement proves
# admission.
export HERDR_RELAY_TRANSPORT=tailscale
export HERDR_TAILSCALE_ORIGIN="$ORIGIN"
export HERDR_RELAY_HOST=127.0.0.1
export HERDR_RELAY_PAIRING_SOCKET="$CONTROL_SOCKET"
RUN_ID="$(generate_instance_id)"
export HERDR_RELAY_RUN_ID="$RUN_ID"

RELAY_LOG="$(mktemp "$CONFIG_DIR/.tailscale-relay-log.XXXXXX")"
SERVE_LOG="$(mktemp "$CONFIG_DIR/.tailscale-serve-log.XXXXXX")"
chmod 600 "$RELAY_LOG" "$SERVE_LOG"
"$RELAY_BIN" supervise --grace 5s -- "$RELAY_BIN" serve >"$RELAY_LOG" 2>&1 &
RELAY_PID=$!
RELAY_JOB="$(capture_child_job "$RELAY_PID")" || {
    echo "✗ Could not capture the relay's active Bash job generation." >&2
    exit 1
}

HEALTH=""
for attempt in $(seq 1 30); do
    if ! child_job_active "$RELAY_JOB" "$RELAY_PID"; then
        echo "✗ Relay failed to bind; the configured port was not reused." >&2
        sed -n '1,80p' "$RELAY_LOG" >&2
        exit 1
    fi
    if HEALTH="$(wait_for_relay_health "$PORT" 1 0 2>/dev/null)"; then
        break
    fi
    sleep 1
done
[ -n "$HEALTH" ] || {
    echo "✗ Relay did not become healthy on 127.0.0.1:$PORT." >&2
    sed -n '1,80p' "$RELAY_LOG" >&2
    exit 1
}

PAIRING_STATUS="$(tailscale_control_request "$CONTROL_SOCKET" status "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)" || {
    echo "✗ Managed relay pairing control did not become ready." >&2
    exit 1
}
[ "$(json_bool_field "$PAIRING_STATUS" ready)" = true ] || {
    echo "✗ Managed relay did not acknowledge a bound HTTP endpoint." >&2
    exit 1
}

# Admission is proven: the serve child holds the published owner. Only now
# persist the normalized transport and the authenticated node origin, prepare
# the configuration root and record the foreground session. The origin is never
# accepted from an arbitrary hostname; it came from Self.DNSName. Mark the file
# before the first mutation so a later write failure still rolls back any
# earlier atomic write.
MUTATED_ENV=true
MUTATED_ENV_CONTENT="$ORIGINAL_ENV"
set_relay_transport "$ENV_FILE" tailscale
MUTATED_ENV_CONTENT="$(cat "$ENV_FILE")"
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_ORIGIN "$ORIGIN"
MUTATED_ENV_CONTENT="$(cat "$ENV_FILE")"
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_HTTPS_PORT "$HTTPS_PORT"
MUTATED_ENV_CONTENT="$(cat "$ENV_FILE")"
mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
write_session

# This is intentionally the foreground form of Serve. A background Serve route
# is not owned by this pane and is therefore outside this milestone.
"$RELAY_BIN" supervise --grace 5s -- "$TS_BIN" serve --https="$HTTPS_PORT" "http://127.0.0.1:$PORT" >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!
SERVE_JOB="$(capture_child_job "$SERVE_PID")" || {
    write_session || echo "✗ Could not refresh the diagnostic session record." >&2
    echo "✗ Could not capture Tailscale Serve's active Bash job generation." >&2
    exit 1
}
write_session

SERVE_READY=false
for attempt in $(seq 1 30); do
    if ! child_job_active "$SERVE_JOB" "$SERVE_PID"; then
        echo "✗ Tailscale Serve exited before its route became ready." >&2
        sed -n '1,80p' "$SERVE_LOG" >&2
        exit 1
    fi
    INSPECTION="$("$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null || true)"
    if [ "$(json_bool_field "$INSPECTION" serve_inspected)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" exposure_complete)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" serve_configured)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" funnel_configured)" = false ] &&
        [ "$(json_number_field "$INSPECTION" serve_route_count)" = 1 ] &&
        [ "$(json_bool_field "$INSPECTION" serve_route_owned)" = true ]; then
        SERVE_READY=true
        break
    fi
    sleep 1
done
[ "$SERVE_READY" = true ] || {
    echo "✗ Tailscale Serve did not report the owned route." >&2
    exit 1
}

curl_tailscale_health() {
    local origin="$1"
    shift
    local args=(--fail --silent --show-error --noproxy '*' --connect-timeout 3 --max-time 5 --max-redirs 0)
    if [ -n "${HERDR_TAILSCALE_CA_FILE:-}" ]; then
        [ -r "$HERDR_TAILSCALE_CA_FILE" ] || return 1
        args+=(--cacert "$HERDR_TAILSCALE_CA_FILE")
    fi
    curl "${args[@]}" "$origin/$1"
}

PUBLIC_HEALTH=""
for attempt in $(seq 1 30); do
    if PUBLIC_HEALTH="$(curl_tailscale_health "$ORIGIN" healthz 2>/dev/null)"; then
        break
    fi
    sleep 1
done
[ -n "$PUBLIC_HEALTH" ] || {
    echo "✗ Trusted HTTPS identity verification failed for $ORIGIN." >&2
    echo "  No certificate bypass or insecure fallback is permitted." >&2
    exit 1
}
[ "$(json_string_field "$PUBLIC_HEALTH" instance)" = "$HERDR_RELAY_INSTANCE_ID" ] || {
    echo "✗ HTTPS health belongs to a different relay instance." >&2
    exit 1
}
[ "$(json_string_field "$PUBLIC_HEALTH" managed_run_id)" = "$RUN_ID" ] || {
    echo "✗ HTTPS health belongs to a different foreground run." >&2
    exit 1
}
[ "$(json_string_field "$PUBLIC_HEALTH" transport)" = tailscale ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" tailscale_origin)" = "$ORIGIN" ] || {
    echo "✗ HTTPS health did not identify the verified Tailscale origin." >&2
    exit 1
}

require_release_identity "$PUBLIC_HEALTH" "$RELAY_BIN" || exit 1

PHONE_APP_BASE="$(choose_phone_app_base_url "$ORIGIN" "$ENV_FILE" tailscale)" || exit 1
MUTATED_ORIGIN=true
MUTATED_ORIGIN_CONTENT="$ORIGINAL_ORIGIN"
record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
MUTATED_ORIGIN_CONTENT="$(cat "$ORIGIN_FILE")"

ARMED_RESPONSE="$(arm_tailscale_setup_link "$ENV_FILE")" || {
    echo "✗ The relay did not acknowledge persistent invitation arming; no QR was printed." >&2
    exit 1
}
RUN_ID="$(json_string_field "$ARMED_RESPONSE" run_id)"
RELAY_URL="wss://${ORIGIN#https://}"
HOST_LABEL="$(host_label)"
SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"

# The URL is a credential. Recheck both owned jobs, the current route, and
# managed pairing readiness immediately before publishing it.
if ! child_job_active "$RELAY_JOB" "$RELAY_PID"; then
    echo "✗ The foreground relay stopped before its private setup link was ready." >&2
    exit 1
fi
if ! child_job_active "$SERVE_JOB" "$SERVE_PID"; then
    echo "✗ Tailscale Serve stopped before the private setup link was ready." >&2
    exit 1
fi
PAIRING_STATUS="$(tailscale_control_request "$CONTROL_SOCKET" status "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)" || {
    echo "✗ Managed relay pairing control stopped before the private setup link was ready." >&2
    exit 1
}
if [ "$(json_bool_field "$PAIRING_STATUS" ready)" != true ]; then
    echo "✗ Managed relay pairing was no longer ready; no private setup link was printed." >&2
    exit 1
fi
CURRENT_INSPECTION="$(
    "$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null
)" || {
    echo "✗ Tailscale Serve inspection failed before the private setup link was ready." >&2
    exit 1
}
if [ "$(json_bool_field "$CURRENT_INSPECTION" serve_inspected)" != true ] ||
    [ "$(json_bool_field "$CURRENT_INSPECTION" exposure_complete)" != true ] ||
    [ "$(json_bool_field "$CURRENT_INSPECTION" serve_configured)" != true ] ||
    [ "$(json_bool_field "$CURRENT_INSPECTION" funnel_configured)" != false ] ||
    [ "$(json_number_field "$CURRENT_INSPECTION" serve_route_count)" != 1 ] ||
    [ "$(json_bool_field "$CURRENT_INSPECTION" serve_route_owned)" != true ]; then
    echo "✗ The owned Tailscale Serve route changed before the private setup link was ready." >&2
    exit 1
fi
# All persistent state and live ownership are now verified; cleanup from this
# point must preserve the committed transport rather than roll it back.
COMMITTED=true
print_phone_setup "$PHONE_URL"

echo ""
echo "✓ Tailscale relay ready at $ORIGIN"
echo "  Backend:  http://127.0.0.1:$PORT"
echo "  This pane owns the relay and Serve session; press Ctrl-C to stop both."
echo "  The setup link is private and pairs one device within 10 minutes."

while child_job_active "$RELAY_JOB" "$RELAY_PID" && child_job_active "$SERVE_JOB" "$SERVE_PID"; do
    CURRENT_INSPECTION="$("$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT" 2>/dev/null || true)"
    if [ "$(json_bool_field "$CURRENT_INSPECTION" serve_inspected)" != true ] ||
        [ "$(json_bool_field "$CURRENT_INSPECTION" exposure_complete)" != true ] ||
        [ "$(json_bool_field "$CURRENT_INSPECTION" serve_configured)" != true ] ||
        [ "$(json_bool_field "$CURRENT_INSPECTION" funnel_configured)" != false ] ||
        [ "$(json_number_field "$CURRENT_INSPECTION" serve_route_count)" != 1 ] ||
        [ "$(json_bool_field "$CURRENT_INSPECTION" serve_route_owned)" != true ]; then
        echo "✗ The owned Tailscale Serve route is no longer active; cleaning up." >&2
        exit 1
    fi
    sleep 2
done
if ! child_job_active "$RELAY_JOB" "$RELAY_PID"; then
    echo "✗ The foreground relay stopped; its Serve peer will be removed." >&2
else
    echo "✗ Tailscale Serve stopped; the relay will be removed." >&2
fi
exit 1

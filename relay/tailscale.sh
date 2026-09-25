#!/bin/bash
set -euo pipefail

usage() {
    echo "Usage: $0 [--confirm-serve | --help]"
    echo "  --confirm-serve  Consent to tailnet HTTPS exposure for this invocation only."
    echo "  Without the option, a fresh affirmative answer at a TTY prompt is required."
    echo "  Tailscale must already be installed/authenticated and relay setup complete."
}

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
PERSISTED_REARM="$(env_file_value "$ENV_FILE" HERDR_RELAY_REARM_BOOTSTRAP || true)"
PERSISTED_TOKEN="$(env_file_value "$ENV_FILE" HERDR_RELAY_TOKEN || true)"
PERSISTED_INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID || true)"
load_relay_env "$ENV_FILE"
[ -n "$PERSISTED_TOKEN" ] && [ "$PERSISTED_TOKEN" = "${HERDR_RELAY_TOKEN:-}" ] &&
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
case "$PERSISTED_REARM_NORMALIZED" in
    1|true|yes|on)
        echo "✗ Tailscale startup refuses bootstrap reset; remove HERDR_RELAY_REARM_BOOTSTRAP explicitly." >&2
        exit 1
        ;;
esac
export HERDR_RELAY_REARM_BOOTSTRAP=0
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
[ -d "$(dirname "$ENV_FILE")" ] || {
    echo "✗ Relay configuration directory is missing; run setup first." >&2
    exit 1
}
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
    *[!0-9:]*|*:0|0:*) echo "✗ Relay and plugin ports must be positive integers." >&2; exit 1 ;;
esac
[ "$PORT" -ge 1 ] && [ "$PLUGIN_PORT" -ge 1 ] && [ "$PORT" -le 65535 ] && [ "$PLUGIN_PORT" -le 65535 ] || {
    echo "✗ Relay and plugin ports must be between 1 and 65535." >&2
    exit 1
}
[ "$PORT" != "$PLUGIN_PORT" ] || { echo "✗ Relay and plugin ports must be different." >&2; exit 1; }
[ "${HERDR_RELAY_HOST:-127.0.0.1}" = 127.0.0.1 ] || {
    echo "✗ Tailscale Serve requires the relay backend to remain on 127.0.0.1." >&2
    exit 1
}
HTTPS_PORT="$(tailscale_https_port)"
SESSION_FILE="$(tailscale_session_file "$ENV_FILE")"
CONFIG_DIR="$(dirname "$ENV_FILE")"
CONTROL_SOCKET="$CONFIG_DIR/tailscale-control.sock"
if [ -e "$SESSION_FILE" ] || [ -e "$CONTROL_SOCKET" ]; then
    echo "✗ A prior Tailscale session or control socket exists; inspect it before retrying." >&2
    exit 1
fi
if ! "$RELAY_BIN" check-port --host 127.0.0.1 --port "$PORT" --protocol tcp >/dev/null 2>&1 ||
    ! "$RELAY_BIN" check-port --host 127.0.0.1 --port "$PLUGIN_PORT" --protocol udp >/dev/null 2>&1; then
    echo "✗ Relay or Herdr event port is occupied; no existing listener will be reused." >&2
    exit 1
fi

inspect_tailscale() {
    "$RELAY_BIN" tailscale inspect --binary "$TS_BIN" --https-port "$HTTPS_PORT"
}
require_empty_supported_inspection() {
    local inspection="$1"
    [ "$(json_bool_field "$inspection" serve_inspected)" = true ] &&
        [ "$(json_bool_field "$inspection" exposure_complete)" = true ] || {
        echo "✗ Tailscale exposure inspection is incomplete; nothing was changed." >&2
        return 1
    }
    [ "$(json_string_field "$inspection" backend_state)" = Running ] &&
        [ "$(json_bool_field "$inspection" logged_in)" = true ] || {
        echo "✗ Tailscale is not running with an authenticated node." >&2
        return 1
    }
    [ "$(json_bool_field "$inspection" serve_configured)" = false ] &&
        [ "$(json_bool_field "$inspection" funnel_configured)" = false ] &&
        [ "$(json_number_field "$inspection" serve_route_count)" = 0 ] || {
        echo "✗ Existing Tailscale Serve/Funnel configuration was found; it was left unchanged." >&2
        return 1
    }
}
INSPECTION="$(inspect_tailscale)" || {
    echo "✗ Tailscale status/Serve inspection failed without changing configuration." >&2
    echo "  Check that the installed CLI supports the exact structured profile." >&2
    exit 1
}
require_empty_supported_inspection "$INSPECTION" || exit 1
ORIGIN="$(json_string_field "$INSPECTION" origin)"
NODE_ID="$(json_string_field "$INSPECTION" node_id)"
STATUS_VERSION="$(json_string_field "$INSPECTION" status_version)"
[ -n "$ORIGIN" ] && [ -n "$NODE_ID" ] && [ -n "$STATUS_VERSION" ] || {
    echo "✗ Tailscale identity or exact daemon version is unavailable." >&2
    exit 1
}
CONFIGURED_ORIGIN="$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_ORIGIN || true)"
if [ -n "$CONFIGURED_ORIGIN" ] && [ "$CONFIGURED_ORIGIN" != "$ORIGIN" ]; then
    echo "✗ Saved Tailscale origin does not match the authenticated node identity." >&2
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
    echo "  Lifetime:     this foreground pane; Ctrl-C requests authenticated retirement"
    read -r -p "Configure this Tailscale Serve session? [y/N] " CONSENT || CONSENT=""
    case "$CONSENT" in y|Y|yes|YES) ;; *) echo "Setup cancelled before changing Tailscale Serve."; exit 1 ;; esac
fi

# Close the check/use window after the operator's consent. A changed identity,
# version, Serve route, or Funnel state is a refusal, never adoption authority.
SECOND_INSPECTION="$(inspect_tailscale)" || { echo "✗ Tailscale changed before setup could start; retry." >&2; exit 1; }
require_empty_supported_inspection "$SECOND_INSPECTION" || exit 1
[ "$(json_string_field "$SECOND_INSPECTION" origin)" = "$ORIGIN" ] &&
    [ "$(json_string_field "$SECOND_INSPECTION" node_id)" = "$NODE_ID" ] &&
    [ "$(json_string_field "$SECOND_INSPECTION" status_version)" = "$STATUS_VERSION" ] || {
    echo "✗ Tailscale identity or daemon version changed during consent; nothing was changed." >&2
    exit 1
}

ORIGIN_FILE="$CONFIG_DIR/phone-app-origin-configured"
ROLLBACK_DIR="$(mktemp -d "$CONFIG_DIR/.tailscale-rollback.XXXXXX")"
trap 'rm -rf "$ROLLBACK_DIR"' EXIT
chmod 700 "$ROLLBACK_DIR"
file_mode() {
    case "$(uname -s)" in
        Darwin) stat -f '%Lp' "$1" ;;
        Linux) stat -c '%a' "$1" ;;
        *) return 1 ;;
    esac
}
snapshot_file() {
    local path="$1" snapshot="$2"
    if [ -e "$path" ] || [ -L "$path" ]; then
        [ -f "$path" ] && [ ! -L "$path" ] || {
            echo "✗ Refusing to snapshot non-regular managed selection state." >&2
            return 1
        }
        cp -p "$path" "$snapshot"
        printf 'true\n'
    else
        : > "$snapshot"
        printf 'false\n'
    fi
}
ORIGINAL_ENV_SNAPSHOT="$ROLLBACK_DIR/original-env"
EXPECTED_ENV_SNAPSHOT="$ROLLBACK_DIR/expected-env"
ORIGINAL_ORIGIN_SNAPSHOT="$ROLLBACK_DIR/original-origin"
EXPECTED_ORIGIN_SNAPSHOT="$ROLLBACK_DIR/expected-origin"
ORIGINAL_ENV_EXISTS="$(snapshot_file "$ENV_FILE" "$ORIGINAL_ENV_SNAPSHOT")"
ORIGINAL_ENV_MODE=""
[ "$ORIGINAL_ENV_EXISTS" != true ] || ORIGINAL_ENV_MODE="$(file_mode "$ENV_FILE")"
EXPECTED_ENV_EXISTS="$ORIGINAL_ENV_EXISTS"
EXPECTED_ENV_MODE="$ORIGINAL_ENV_MODE"
cp -p "$ORIGINAL_ENV_SNAPSHOT" "$EXPECTED_ENV_SNAPSHOT"
ORIGINAL_ORIGIN_EXISTS="$(snapshot_file "$ORIGIN_FILE" "$ORIGINAL_ORIGIN_SNAPSHOT")"
ORIGINAL_ORIGIN_MODE=""
[ "$ORIGINAL_ORIGIN_EXISTS" != true ] || ORIGINAL_ORIGIN_MODE="$(file_mode "$ORIGIN_FILE")"
EXPECTED_ORIGIN_EXISTS="$ORIGINAL_ORIGIN_EXISTS"
EXPECTED_ORIGIN_MODE="$ORIGINAL_ORIGIN_MODE"
cp -p "$ORIGINAL_ORIGIN_SNAPSHOT" "$EXPECTED_ORIGIN_SNAPSHOT"
MUTATED_ENV=false
MUTATED_ORIGIN=false
ROLLBACK_RETAIN=false
RELAY_PID=""
RELAY_JOB=""
RELAY_LOG=""
RUN_ID=""
SESSION_STAGE="local-ready"
RELAY_STARTED=false
CLEANUP_SAFE=true
COMMITTED=false
PRESERVE_RECOVERY=false
JOURNAL_WRITTEN=false

snapshot_matches() {
    local path="$1" expected_exists="$2" expected_mode="$3" snapshot="$4"
    if [ "$expected_exists" = true ]; then
        [ -f "$path" ] && [ ! -L "$path" ] &&
            [ "$(file_mode "$path")" = "$expected_mode" ] && cmp -s "$snapshot" "$path"
    else
        [ ! -e "$path" ] && [ ! -L "$path" ]
    fi
}
restore_file_if_unchanged() {
    local path="$1" original_exists="$2" original="$3" expected_exists="$4" expected_mode="$5" expected="$6"
    if ! snapshot_matches "$path" "$expected_exists" "$expected_mode" "$expected"; then
        echo "✗ Refusing rollback after concurrent modification of a managed selection file." >&2
        return 1
    fi
    if [ "$original_exists" = true ]; then
        local temporary
        temporary="$(mktemp "$(dirname "$path")/.tailscale-restore.XXXXXX")"
        cp -p "$original" "$temporary"
        mv "$temporary" "$path"
    else
        rm -f "$path"
    fi
}
rollback_selection() {
    if [ "$MUTATED_ENV" = true ]; then
        restore_file_if_unchanged "$ENV_FILE" "$ORIGINAL_ENV_EXISTS" "$ORIGINAL_ENV_SNAPSHOT" "$EXPECTED_ENV_EXISTS" "$EXPECTED_ENV_MODE" "$EXPECTED_ENV_SNAPSHOT" || return 1
    fi
    if [ "$MUTATED_ORIGIN" = true ]; then
        restore_file_if_unchanged "$ORIGIN_FILE" "$ORIGINAL_ORIGIN_EXISTS" "$ORIGINAL_ORIGIN_SNAPSHOT" "$EXPECTED_ORIGIN_EXISTS" "$EXPECTED_ORIGIN_MODE" "$EXPECTED_ORIGIN_SNAPSHOT" || return 1
    fi
}
write_session() {
    local temporary
    temporary="$(mktemp "$CONFIG_DIR/.tailscale-session.XXXXXX")"
    umask 077
    {
        printf 'HERDR_RELAY_STAGE=%s\n' "$SESSION_STAGE"
        printf 'HERDR_RELAY_RUN_ID=%s\n' "$RUN_ID"
        printf 'HERDR_RELAY_PAIRING_SOCKET=%s\n' "$CONTROL_SOCKET"
        printf 'HERDR_TAILSCALE_ORIGIN=%s\n' "$ORIGIN"
        printf 'HERDR_TAILSCALE_HTTPS_PORT=%s\n' "$HTTPS_PORT"
        printf 'HERDR_RELAY_SUPERVISOR_PID=%s\n' "$RELAY_PID"
    } > "$temporary"
    chmod 600 "$temporary"
    mv "$temporary" "$SESSION_FILE"
}
retire_and_verify() {
    local response
    response="$(tailscale_control_request "$CONTROL_SOCKET" retire "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)" || return 1
    [ "$(json_bool_field "$response" ok)" = true ] &&
        [ "$(json_string_field "$response" run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$response" instance)" = "$HERDR_RELAY_INSTANCE_ID" ] &&
        [ "$(json_bool_field "$response" route_cleared)" = true ] &&
        [ "$(json_bool_field "$response" local_watch_closed)" = true ]
}
wait_for_relay_exit() {
    local attempt
    for attempt in $(seq 1 60); do
        if ! child_job_active "$RELAY_JOB" "$RELAY_PID"; then
            return 0
        fi
        sleep 0.2
    done
    return 1
}
cleanup() {
    local status=0 attempt retired=false
    if [ "$RELAY_STARTED" = true ] && [ "$JOURNAL_WRITTEN" != true ] && [ -S "$CONTROL_SOCKET" ]; then
        if write_session; then JOURNAL_WRITTEN=true; fi
    fi
    if [ "$PRESERVE_RECOVERY" = true ]; then
        echo "✗ Managed activation/arm is ambiguous; retaining the live owner, control socket, recovery record, and logs." >&2
        return 1
    fi
    if [ "$RELAY_STARTED" = true ]; then
        CLEANUP_SAFE=false
        if [ -z "$RELAY_PID" ] || [ -z "$RELAY_JOB" ]; then
            echo "✗ Relay process identity is unavailable; retaining recovery evidence." >&2
            return 1
        fi
        if [ ! -e "$CONTROL_SOCKET" ]; then
            echo "✗ Private retirement control is unavailable; retaining the owner record and logs." >&2
            return 1
        fi
        for attempt in 1 2 3; do
            if retire_and_verify; then
                retired=true
                break
            fi
            [ "$attempt" -eq 3 ] || sleep 1
        done
        if [ "$retired" != true ]; then
            echo "✗ Authenticated retirement did not separately acknowledge route clear and local watch closure; retaining recovery evidence." >&2
            return 1
        fi
        if ! wait_for_relay_exit; then
            echo "✗ Relay did not exit after retirement acknowledgement; retaining its owner and artifacts." >&2
            return 1
        fi
        CLEANUP_SAFE=true
    fi
    if [ "$CLEANUP_SAFE" = true ]; then
        [ -z "$RELAY_LOG" ] || rm -f "$RELAY_LOG"
        rm -f "$SESSION_FILE"
    fi
    return "$status"
}
retain_recovery() {
    local reason="$1"
    PRESERVE_RECOVERY=true
    echo "✗ $reason; no setup link was printed." >&2
    echo "  The foreground owner, private control, and recovery record are retained." >&2
    echo "  Use the recorded run/socket with authenticated pairing-control retire after review." >&2
    return 1
}
trap 'status=$?; trap - EXIT INT TERM HUP; cleanup || status=1; if [ "$COMMITTED" != true ] && [ "$CLEANUP_SAFE" = true ] && [ "$status" -ne 0 ]; then if ! rollback_selection; then status=1; ROLLBACK_RETAIN=true; echo "✗ Selection rollback was refused; private snapshot retained at $ROLLBACK_DIR." >&2; fi; fi; if [ "$CLEANUP_SAFE" != true ] && { [ "$MUTATED_ENV" = true ] || [ "$MUTATED_ORIGIN" = true ]; }; then ROLLBACK_RETAIN=true; echo "✗ Managed selection remains paired with unresolved retirement; private rollback snapshots retained at $ROLLBACK_DIR." >&2; fi; if [ "$ROLLBACK_RETAIN" != true ]; then rm -rf "$ROLLBACK_DIR"; fi; exit "$status"' EXIT
trap 'exit 130' INT TERM

export HERDR_RELAY_TRANSPORT=tailscale
export HERDR_TAILSCALE_ORIGIN="$ORIGIN"
export HERDR_RELAY_HOST=127.0.0.1
export HERDR_RELAY_PAIRING_SOCKET="$CONTROL_SOCKET"
RUN_ID="$(generate_instance_id)"
export HERDR_RELAY_RUN_ID="$RUN_ID"
RELAY_LOG="$(mktemp "$CONFIG_DIR/.tailscale-relay-log.XXXXXX")"
chmod 600 "$RELAY_LOG"
"$RELAY_BIN" supervise --grace 5s -- "$RELAY_BIN" serve >"$RELAY_LOG" 2>&1 &
RELAY_PID=$!
RELAY_STARTED=true
CLEANUP_SAFE=false
RELAY_JOB="$(capture_child_job "$RELAY_PID")" || {
    echo "✗ Could not capture the relay's active Bash job generation." >&2
    exit 1
}
LOCAL_STATUS=""
for attempt in $(seq 1 60); do
    if ! child_job_active "$RELAY_JOB" "$RELAY_PID"; then
        echo "✗ Relay exited before publishing its managed control socket." >&2
        echo "  Private startup diagnostics retained at $RELAY_LOG." >&2
        exit 1
    fi
    if [ -S "$CONTROL_SOCKET" ] && [ "$JOURNAL_WRITTEN" != true ]; then
        write_session
        JOURNAL_WRITTEN=true
    fi
    if LOCAL_STATUS="$(tailscale_control_request "$CONTROL_SOCKET" status "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)" &&
        [ "$(json_bool_field "$LOCAL_STATUS" local_ready)" = true ] &&
        [ "$(json_bool_field "$LOCAL_STATUS" owner_held)" = true ] &&
        [ "$(json_bool_field "$LOCAL_STATUS" quarantined)" = false ]; then
        break
    fi
    sleep 1
done
[ "$(json_bool_field "$LOCAL_STATUS" local_ready)" = true ] &&
    [ "$(json_bool_field "$LOCAL_STATUS" owner_held)" = true ] || {
    echo "✗ Managed relay did not acknowledge local_ready with its owner held." >&2
    exit 1
}
# Persist the recovery identity only after Go has acquired O and published its
# authenticated control. Activation and invitation writes remain separate ACKs.
write_session
JOURNAL_WRITTEN=true

SESSION_STAGE="activation-pending"
write_session
ACTIVATE_RESPONSE=""
if ! ACTIVATE_RESPONSE="$(tailscale_control_request "$CONTROL_SOCKET" activate "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)"; then
    retain_recovery "Tailscale activation acknowledgement was lost" || exit 1
fi
if [ "$(json_bool_field "$ACTIVATE_RESPONSE" ok)" != true ]; then
    if [ "$(json_string_field "$ACTIVATE_RESPONSE" run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$ACTIVATE_RESPONSE" instance)" = "$HERDR_RELAY_INSTANCE_ID" ]; then
        REGISTRATION_OUTCOME="$(json_string_field "$ACTIVATE_RESPONSE" registration_outcome)"
        case "$REGISTRATION_OUTCOME" in
            not-dispatched|settled-no-write|settled-success)
                echo "✗ Tailscale activation was refused with a decoded $REGISTRATION_OUTCOME result; authenticated retirement will verify cleanup." >&2
                exit 1
                ;;
        esac
    fi
    retain_recovery "Tailscale activation outcome is unresolved" || exit 1
fi
if [ "$(json_string_field "$ACTIVATE_RESPONSE" run_id)" != "$RUN_ID" ] ||
    [ "$(json_string_field "$ACTIVATE_RESPONSE" instance)" != "$HERDR_RELAY_INSTANCE_ID" ] ||
    [ "$(json_bool_field "$ACTIVATE_RESPONSE" local_ready)" != true ] ||
    [ "$(json_bool_field "$ACTIVATE_RESPONSE" owner_held)" != true ] ||
    [ "$(json_bool_field "$ACTIVATE_RESPONSE" serve_ready)" != true ] ||
    [ "$(json_bool_field "$ACTIVATE_RESPONSE" quarantined)" != false ]; then
    retain_recovery "Tailscale activation response did not prove local readiness, live ownership, and Serve readiness" || exit 1
fi
SESSION_STAGE="activated"
write_session

"$RELAY_BIN" tailscale-route-check --binary "$TS_BIN" --origin "$ORIGIN" \
    --https-port "$HTTPS_PORT" --backend-port "$PORT" || {
    echo "✗ Fresh Tailscale route observation did not match the exact managed backend." >&2
    exit 1
}

curl_tailscale_health() {
    local origin="$1" endpoint="$2"
    local args=(--fail --silent --show-error --noproxy '*' --connect-timeout 3 --max-time 5 --max-redirs 0)
    curl "${args[@]}" "$origin/$endpoint"
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
    exit 1
}
[ "$(json_string_field "$PUBLIC_HEALTH" instance)" = "$HERDR_RELAY_INSTANCE_ID" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" managed_run_id)" = "$RUN_ID" ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" transport)" = tailscale ] &&
    [ "$(json_string_field "$PUBLIC_HEALTH" tailscale_origin)" = "$ORIGIN" ] || {
    echo "✗ HTTPS health belongs to a different foreground relay session." >&2
    exit 1
}
require_release_identity "$PUBLIC_HEALTH" "$RELAY_BIN" || exit 1

PHONE_APP_BASE="$(choose_phone_app_base_url "$ORIGIN" "$ENV_FILE" tailscale)" || exit 1
verify_phone_app_bundle "$PHONE_APP_BASE" "$RELAY_BIN" || exit 1
if ! snapshot_matches "$ENV_FILE" "$ORIGINAL_ENV_EXISTS" "$ORIGINAL_ENV_MODE" "$ORIGINAL_ENV_SNAPSHOT" ||
    ! snapshot_matches "$ORIGIN_FILE" "$ORIGINAL_ORIGIN_EXISTS" "$ORIGINAL_ORIGIN_MODE" "$ORIGINAL_ORIGIN_SNAPSHOT"; then
    echo "✗ Managed selection changed while setup was being verified; nothing was overwritten." >&2
    exit 1
fi
MUTATED_ENV=true
set_env_value_atomic "$EXPECTED_ENV_SNAPSHOT" HERDR_RELAY_TRANSPORT tailscale
EXPECTED_ENV_EXISTS=true
EXPECTED_ENV_MODE="$(file_mode "$EXPECTED_ENV_SNAPSHOT")"
set_relay_transport "$ENV_FILE" tailscale
if ! snapshot_matches "$ENV_FILE" "$EXPECTED_ENV_EXISTS" "$EXPECTED_ENV_MODE" "$EXPECTED_ENV_SNAPSHOT"; then
    echo "✗ Relay selection changed during setup; refusing to overwrite it." >&2
    exit 1
fi
set_env_value_atomic "$EXPECTED_ENV_SNAPSHOT" HERDR_TAILSCALE_ORIGIN "$ORIGIN"
EXPECTED_ENV_MODE="$(file_mode "$EXPECTED_ENV_SNAPSHOT")"
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_ORIGIN "$ORIGIN"
if ! snapshot_matches "$ENV_FILE" "$EXPECTED_ENV_EXISTS" "$EXPECTED_ENV_MODE" "$EXPECTED_ENV_SNAPSHOT"; then
    echo "✗ Relay selection changed during setup; refusing to overwrite it." >&2
    exit 1
fi
set_env_value_atomic "$EXPECTED_ENV_SNAPSHOT" HERDR_TAILSCALE_HTTPS_PORT "$HTTPS_PORT"
EXPECTED_ENV_MODE="$(file_mode "$EXPECTED_ENV_SNAPSHOT")"
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_HTTPS_PORT "$HTTPS_PORT"
if ! snapshot_matches "$ENV_FILE" "$EXPECTED_ENV_EXISTS" "$EXPECTED_ENV_MODE" "$EXPECTED_ENV_SNAPSHOT"; then
    echo "✗ Relay selection changed during setup; refusing to overwrite it." >&2
    exit 1
fi
if ! snapshot_matches "$ORIGIN_FILE" "$ORIGINAL_ORIGIN_EXISTS" "$ORIGINAL_ORIGIN_MODE" "$ORIGINAL_ORIGIN_SNAPSHOT"; then
    echo "✗ Phone-app origin changed during setup; refusing to overwrite it." >&2
    exit 1
fi
printf '%s\n' "$PHONE_APP_BASE" > "$EXPECTED_ORIGIN_SNAPSHOT"
chmod 600 "$EXPECTED_ORIGIN_SNAPSHOT"
EXPECTED_ORIGIN_EXISTS=true
EXPECTED_ORIGIN_MODE="$(file_mode "$EXPECTED_ORIGIN_SNAPSHOT")"
record_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
MUTATED_ORIGIN=true
if ! snapshot_matches "$ORIGIN_FILE" "$EXPECTED_ORIGIN_EXISTS" "$EXPECTED_ORIGIN_MODE" "$EXPECTED_ORIGIN_SNAPSHOT"; then
    echo "✗ Phone-app origin changed during setup; refusing to overwrite it." >&2
    exit 1
fi
SESSION_STAGE="arm-pending"
write_session

ARMED_RESPONSE=""
if ! ARMED_RESPONSE="$(tailscale_control_request "$CONTROL_SOCKET" arm_bootstrap "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)"; then
    retain_recovery "Bootstrap invitation acknowledgement was lost" || exit 1
fi
if [ "$(json_bool_field "$ARMED_RESPONSE" ok)" != true ]; then
    if [ "$(json_string_field "$ARMED_RESPONSE" run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$ARMED_RESPONSE" instance)" = "$HERDR_RELAY_INSTANCE_ID" ]; then
        ARM_OUTCOME="$(json_string_field "$ARMED_RESPONSE" arm_outcome)"
        case "$ARM_OUTCOME" in
            not-committed|committed)
                echo "✗ Bootstrap arm was refused with a decoded $ARM_OUTCOME result; authenticated retirement will verify cleanup." >&2
                exit 1
                ;;
        esac
    fi
    retain_recovery "Bootstrap invitation outcome is unresolved" || exit 1
fi
if [ "$(json_string_field "$ARMED_RESPONSE" run_id)" != "$RUN_ID" ] ||
    [ "$(json_string_field "$ARMED_RESPONSE" instance)" != "$HERDR_RELAY_INSTANCE_ID" ] ||
    [ "$(json_bool_field "$ARMED_RESPONSE" invitation_armed)" != true ] ||
    [ -z "$(json_string_field "$ARMED_RESPONSE" invitation_expires_at)" ] ||
    [ "$(json_bool_field "$ARMED_RESPONSE" serve_ready)" != true ] ||
    [ "$(json_bool_field "$ARMED_RESPONSE" owner_held)" != true ] ||
    [ "$(json_bool_field "$ARMED_RESPONSE" quarantined)" != false ]; then
    retain_recovery "Bootstrap arm response did not prove durable invitation and managed-route readiness" || exit 1
fi
SESSION_STAGE="ready"
write_session

RELAY_URL="wss://${ORIGIN#https://}"
SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$(host_label)" "$RELAY_URL")"
PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
if ! child_job_active "$RELAY_JOB" "$RELAY_PID" ||
    ! FINAL_STATUS="$(tailscale_control_request "$CONTROL_SOCKET" status "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null)" ||
    [ "$(json_bool_field "$FINAL_STATUS" ready)" != true ] ||
    [ "$(json_bool_field "$FINAL_STATUS" local_ready)" != true ] ||
    [ "$(json_bool_field "$FINAL_STATUS" serve_ready)" != true ] ||
    [ "$(json_bool_field "$FINAL_STATUS" owner_held)" != true ] ||
    [ "$(json_bool_field "$FINAL_STATUS" quarantined)" != false ] ||
    ! "$RELAY_BIN" tailscale-route-check --binary "$TS_BIN" --origin "$ORIGIN" \
        --https-port "$HTTPS_PORT" --backend-port "$PORT" ||
    ! PUBLIC_HEALTH="$(curl_tailscale_health "$ORIGIN" healthz 2>/dev/null)" ||
    [ "$(json_string_field "$PUBLIC_HEALTH" instance)" != "$HERDR_RELAY_INSTANCE_ID" ] ||
    [ "$(json_string_field "$PUBLIC_HEALTH" managed_run_id)" != "$RUN_ID" ] ||
    [ "$(json_string_field "$PUBLIC_HEALTH" transport)" != tailscale ] ||
    [ "$(json_string_field "$PUBLIC_HEALTH" tailscale_origin)" != "$ORIGIN" ]; then
    echo "✗ Final route, owner, TLS identity, bundle, or pairing checks failed; no setup link was printed." >&2
    exit 1
fi
verify_phone_app_bundle "$PHONE_APP_BASE" "$RELAY_BIN" || {
    echo "✗ Final phone-app bundle verification failed; no setup link was printed." >&2
    exit 1
}
require_release_identity "$PUBLIC_HEALTH" "$RELAY_BIN" || exit 1
COMMITTED=true
print_phone_setup "$PHONE_URL"
echo ""
echo "✓ Tailscale relay ready at $ORIGIN"
echo "  Go owns the foreground Serve route, watcher, relay, and authenticated retirement."
echo "  This pane must remain open; ingress is removed only after retirement is acknowledged."
echo "  The setup link is private and pairs one device within 10 minutes."

while child_job_active "$RELAY_JOB" "$RELAY_PID"; do
    CURRENT_STATUS="$(tailscale_control_request "$CONTROL_SOCKET" status "$RUN_ID" "$HERDR_RELAY_INSTANCE_ID" 2>/dev/null || true)"
    if [ "$(json_bool_field "$CURRENT_STATUS" ready)" != true ] ||
        [ "$(json_bool_field "$CURRENT_STATUS" owner_held)" != true ] ||
        [ "$(json_bool_field "$CURRENT_STATUS" serve_ready)" != true ] ||
        [ "$(json_bool_field "$CURRENT_STATUS" quarantined)" != false ] ||
        ! "$RELAY_BIN" tailscale-route-check --binary "$TS_BIN" --origin "$ORIGIN" \
            --https-port "$HTTPS_PORT" --backend-port "$PORT"; then
        echo "✗ Managed Tailscale readiness or exact route changed; requesting retirement." >&2
        exit 1
    fi
    sleep 2
done
echo "✗ Foreground managed relay stopped; recovery state is retained for inspection." >&2
exit 1

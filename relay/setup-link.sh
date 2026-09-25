#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"

assert_service_env_matches "$ENV_FILE"
load_relay_env "$ENV_FILE"
MODE="$(relay_transport_mode "$ENV_FILE")"
SESSION_FILE="$(tailscale_session_file "$ENV_FILE")"
EXTERNAL_SESSION_FILE="$(tailscale_external_session_file "$ENV_FILE")"
if [ -e "$SESSION_FILE" ] && [ "$MODE" != tailscale ]; then
    echo "✗ A managed Tailscale session is recorded with a conflicting transport; no link was printed." >&2
    exit 1
fi
if [ -e "$EXTERNAL_SESSION_FILE" ] && [ "$MODE" != tailscale-external ]; then
    echo "✗ An operator-owned HTTPS Serve session is recorded with a conflicting transport; no link was printed." >&2
    exit 1
fi

RELAY_BIN="$(relay_binary)"
if [ -z "${HERDR_RELAY_TOKEN:-}" ]; then
    echo "✗ No relay token in $ENV_FILE. Run make setup first."
    exit 1
fi

# The gateway and stable branches keep the historical direct origin write. One
# local indirection keeps that pre-transaction writer named only outside the
# tailscale transaction block.
write_phone_app_origin() {
    record_phone_app_origin "$@"
}

if [ "$MODE" = tailscale ]; then
    SESSION_FILE="$(tailscale_session_file "$ENV_FILE")"
    CONFIG_DIR="$(dirname "$ENV_FILE")"
    SOCKET="$(tailscale_session_value "$SESSION_FILE" HERDR_RELAY_PAIRING_SOCKET || true)"
    RUN_ID="$(tailscale_session_value "$SESSION_FILE" HERDR_RELAY_RUN_ID || true)"
    ORIGIN="$(tailscale_session_value "$SESSION_FILE" HERDR_TAILSCALE_ORIGIN || true)"
    SESSION_STAGE="$(tailscale_session_value "$SESSION_FILE" HERDR_RELAY_STAGE || true)"
    INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"
    PERSISTED_TOKEN="$(env_file_value "$ENV_FILE" HERDR_RELAY_TOKEN)"
    [ -n "$PERSISTED_TOKEN" ] && [ "$PERSISTED_TOKEN" = "${HERDR_RELAY_TOKEN:-}" ] || {
        echo "✗ Relay token state changed; no setup link was printed." >&2
        exit 1
    }
    case "$SOCKET" in
        "$CONFIG_DIR"/*) ;;
        *) echo "✗ No owned foreground Tailscale session is recorded."; exit 1 ;;
    esac
    [ -n "$RUN_ID" ] && [ -n "$ORIGIN" ] && [ -n "$INSTANCE" ] || {
        echo "✗ Tailscale session state is incomplete; no setup link was printed." >&2
        exit 1
    }
    [ "$SESSION_STAGE" = ready ] || {
        echo "✗ Tailscale activation/arm recovery is pending; no setup link was printed." >&2
        exit 1
    }
    TS_BIN="${HERDR_TAILSCALE_BIN:-$(command -v tailscale || true)}"
    [ -x "$TS_BIN" ] || { echo "✗ Tailscale CLI is unavailable." >&2; exit 1; }
    INSPECTION="$("$(relay_binary)" tailscale inspect --binary "$TS_BIN" --https-port "$(tailscale_https_port)")" || {
        echo "✗ Tailscale status could not be verified; no link was printed." >&2
        exit 1
    }
    [ "$(json_string_field "$INSPECTION" origin)" = "$ORIGIN" ] || {
        echo "✗ Tailscale's authenticated node identity no longer matches this session." >&2
        exit 1
    }
    [ "$(json_bool_field "$INSPECTION" serve_inspected)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" exposure_complete)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" serve_configured)" = true ] &&
        [ "$(json_bool_field "$INSPECTION" funnel_configured)" = false ] &&
        "$(relay_binary)" tailscale-route-check --binary "$TS_BIN" --origin "$ORIGIN" \
            --https-port "$(tailscale_https_port)" --backend-port "${HERDR_RELAY_PORT:-8375}" || {
        echo "✗ The authenticated managed route is not active; no link was printed." >&2
        exit 1
    }
    STATUS_RESPONSE="$(tailscale_control_request "$SOCKET" status "$RUN_ID" "$INSTANCE" 2>/dev/null)" || {
        echo "✗ The foreground relay is not running; start Tailscale again." >&2
        exit 1
    }
    [ "$(json_bool_field "$STATUS_RESPONSE" ready)" = true ] || {
        echo "✗ The foreground relay is not ready; no link was printed." >&2
        exit 1
    }
    CURL_ARGS=(--fail --silent --show-error --noproxy '*' --connect-timeout 3 --max-time 5 --max-redirs 0)
    HEALTH="$(curl "${CURL_ARGS[@]}" "$ORIGIN/healthz" 2>/dev/null)" || {
        echo "✗ Trusted HTTPS health verification failed; no link was printed." >&2
        exit 1
    }
    [ "$(json_string_field "$HEALTH" instance)" = "$INSTANCE" ] &&
        [ "$(json_string_field "$HEALTH" managed_run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$HEALTH" transport)" = tailscale ] &&
        [ "$(json_string_field "$HEALTH" tailscale_origin)" = "$ORIGIN" ] || {
        echo "✗ HTTPS health belongs to a different foreground run; no link was printed." >&2
        exit 1
    }
    require_release_identity "$HEALTH" "$(relay_binary)" || exit 1
    PHONE_APP_BASE="$(choose_phone_app_base_url "$ORIGIN" "$ENV_FILE" tailscale)" || exit 1
    verify_phone_app_bundle "$PHONE_APP_BASE" "$(relay_binary)" || exit 1
    REPRINT_OUTPUT=""
    REPRINT_STATUS=0
    REPRINT_OUTPUT="$("$(relay_binary)" managed-state reprint \
        --dir "$CONFIG_DIR" --socket "$SOCKET" --run-id "$RUN_ID" --instance "$INSTANCE" \
        --origin-file phone-app-origin-configured --origin-value "$PHONE_APP_BASE" 2>&1)" || REPRINT_STATUS=$?
    if [ "$REPRINT_STATUS" -ne 0 ]; then
        case "$REPRINT_STATUS" in
            6)
                echo "✗ The invitation acknowledgement was lost; no link was printed." >&2
                echo "  The origin was not changed and later reprints are blocked until the" >&2
                echo "  documented recovery reconciles the retained journal." >&2
                ;;
            3)
                echo "✗ The relay configuration is busy or the invitation was refused; no link was printed." >&2
                ;;
            *)
                echo "✗ The invitation transaction was refused; no link was printed." >&2
                ;;
        esac
        printf '%s\n' "$REPRINT_OUTPUT" >&2
        exit 1
    fi
    RELAY_URL="wss://${ORIGIN#https://}"
    SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$(host_label)" "$RELAY_URL")"
    echo "🐑 Herdr Mobile Relay phone setup"
    echo ""
    print_phone_setup "$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    echo ""
    print_setup_link_arming 0
    echo "  Tailscale origin: $ORIGIN"
    echo "  The foreground pane must remain open for the relay and Serve session."
    exit 0
fi

if [ "$MODE" = tailscale-external ]; then
    CONFIG_DIR="$(dirname "$ENV_FILE")"
    ORIGIN="$(tailscale_session_value "$EXTERNAL_SESSION_FILE" HERDR_EXTERNAL_HTTPS_ORIGIN || true)"
    SOCKET="$(tailscale_session_value "$EXTERNAL_SESSION_FILE" HERDR_RELAY_PAIRING_SOCKET || true)"
    RUN_ID="$(tailscale_session_value "$EXTERNAL_SESSION_FILE" HERDR_RELAY_CONTROL_RUN_ID || true)"
    SESSION_STAGE="$(tailscale_session_value "$EXTERNAL_SESSION_FILE" HERDR_RELAY_STAGE || true)"
    INSTANCE="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)"
    PERSISTED_ORIGIN="$(env_file_value "$ENV_FILE" HERDR_EXTERNAL_HTTPS_ORIGIN)"
    TOKEN="$(env_file_value "$ENV_FILE" HERDR_RELAY_TOKEN)"
    PHONE_APP_BASE="$(choose_phone_app_base_url "$ORIGIN" "$ENV_FILE" tailscale-external)" || exit 1
    [ "$SESSION_STAGE" = ready ] && [ "$ORIGIN" = "$PERSISTED_ORIGIN" ] &&
        [ -n "$SOCKET" ] && [ -n "$RUN_ID" ] && [ -n "$INSTANCE" ] && [ "${#TOKEN}" -eq 32 ] || {
        echo "✗ No complete live operator-owned HTTPS Serve session is recorded; no setup link was printed." >&2
        echo "  Start the BYO HTTPS Serve foreground action and verify its configured origin." >&2
        exit 1
    }
    case "$SOCKET" in
        "$CONFIG_DIR"/*) ;;
        *) echo "✗ External pairing-control socket is outside the private relay configuration directory." >&2; exit 1 ;;
    esac
    [ "$("$(relay_binary)" normalize-external-origin "$ORIGIN" 2>/dev/null || true)" = "$ORIGIN" ] || {
        echo "✗ Saved operator-owned HTTPS origin is not canonical; no link was printed." >&2
        exit 1
    }
    CONTROL_STATUS="$("$(relay_binary)" pairing-control --socket "$SOCKET" --operation status \
        --run-id "$RUN_ID" --instance "$INSTANCE" 2>/dev/null)" || {
        echo "✗ The operator-owned relay control endpoint is unavailable; no link was printed." >&2
        exit 1
    }
    RELAY_IDENTITY="$("$RELAY_BIN" version --json)" || {
        echo "✗ Relay release identity could not be read; no setup link was printed." >&2
        exit 1
    }
    [ "$(json_bool_field "$CONTROL_STATUS" local_ready)" = true ] &&
        [ "$(json_string_field "$CONTROL_STATUS" run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$CONTROL_STATUS" instance)" = "$INSTANCE" ] &&
        [ "$(json_string_field "$CONTROL_STATUS" transport)" = tailscale-external ] &&
        [ "$(json_string_field "$CONTROL_STATUS" phone_app_origin)" = "$PHONE_APP_BASE" ] &&
        [ "$(json_string_field "$CONTROL_STATUS" version)" = "$(json_string_field "$RELAY_IDENTITY" version "$RELAY_BIN")" ] &&
        [ "$(json_string_field "$CONTROL_STATUS" revision)" = "$(json_string_field "$RELAY_IDENTITY" revision "$RELAY_BIN")" ] &&
        [ -n "$(json_string_field "$CONTROL_STATUS" bundle_hash)" ] || {
        echo "✗ Private control identity or local release did not match; no setup link was printed." >&2
        exit 1
    }
    HEALTH="$(curl --noproxy '*' --fail --silent --show-error --connect-timeout 3 --max-time 5 --max-redirs 0 \
        "$ORIGIN/healthz" 2>/dev/null)" || {
        echo "✗ Trusted HTTPS health verification failed; no link was printed." >&2
        exit 1
    }
    [ "$(json_string_field "$HEALTH" status)" = ok ] &&
        [ "$(json_string_field "$HEALTH" readiness)" = ready ] &&
        [ "$(json_string_field "$HEALTH" instance)" = "$INSTANCE" ] &&
        [ "$(json_string_field "$HEALTH" external_control_run_id)" = "$RUN_ID" ] &&
        [ "$(json_string_field "$HEALTH" transport)" = tailscale-external ] &&
        [ "$(json_string_field "$HEALTH" external_https_origin)" = "$ORIGIN" ] || {
        echo "✗ HTTPS health belongs to a different instance, run, transport, or origin; no link was printed." >&2
        exit 1
    }
    require_release_identity "$HEALTH" "$(relay_binary)" || exit 1
    verify_phone_app_bundle "$PHONE_APP_BASE" "$(relay_binary)" || exit 1
    REPRINT_OUTPUT=""
    REPRINT_STATUS=0
    REPRINT_OUTPUT="$("$(relay_binary)" managed-state reprint --external \
        --dir "$CONFIG_DIR" --socket "$SOCKET" --run-id "$RUN_ID" --instance "$INSTANCE" \
        --origin-file phone-app-origin-configured --origin-value "$PHONE_APP_BASE" 2>&1)" || REPRINT_STATUS=$?
    if [ "$REPRINT_STATUS" -ne 0 ]; then
        case "$REPRINT_STATUS" in
            6)
                echo "✗ The invitation acknowledgement was ambiguous; no link or QR was printed." >&2
                echo "  Stop this foreground relay, then inspect/reconcile its retained journal with:" >&2
                echo "  $(relay_binary) managed-state recover --dir '$CONFIG_DIR'" >&2
                ;;
            3)
                echo "✗ External invitation arming was busy or definitely refused; no link was printed." >&2
                ;;
            *)
                echo "✗ External invitation arming was refused; no link was printed." >&2
                ;;
        esac
        printf '%s\n' "$REPRINT_OUTPUT" >&2
        exit 1
    fi
    [ "$(json_bool_field "$REPRINT_OUTPUT" ok)" = true ] &&
        [ -n "$(json_string_field "$REPRINT_OUTPUT" invitation_expires_at)" ] || {
        echo "✗ Durable invitation acknowledgement was incomplete; no link was printed." >&2
        exit 1
    }
    RELAY_URL="wss://${ORIGIN#https://}"
    SETUP_FRAGMENT="$(build_setup_fragment "$TOKEN" "$(host_label)" "$RELAY_URL")"
    echo "🐑 Herdr Mobile Relay phone setup — operator-owned HTTPS Serve"
    echo ""
    print_phone_setup "$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    echo ""
    print_setup_link_arming 0
    echo "  Relay HTTPS origin: $ORIGIN"
    echo "  Tailscale Serve/Funnel ingress is operator-owned and was not observed or changed."
    echo "  Stopping Herdr stops only this local backend; ingress remains configured by you."
    exit 0
fi

# A gateway-configured relay has no tunnel hostname and no cloudflared config:
# the phone finds it through the gateway, so the link only needs the app origin.
GATEWAY_URL="$(gateway_url "$ENV_FILE")"
if [ -n "$GATEWAY_URL" ]; then
    HOST_LABEL="$(host_label)"
    SETUP_FRAGMENT="$(build_transport_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL")"
    PHONE_APP_BASE="$(gateway_phone_app_base_url "$ENV_FILE")"
    write_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"

    ARMED=0
    arm_setup_link "$ENV_FILE" || ARMED=$?
    echo "🐑 Herdr Mobile Relay phone setup"
    echo ""
    print_phone_setup "$PHONE_APP_BASE/#$SETUP_FRAGMENT"
    echo ""
    print_setup_link_arming "$ARMED"
    echo "  Gateway: $GATEWAY_URL"
    echo "  The relay must be running for the link to work:"
    echo "  make service-status"
    exit 0
fi

# The stable hostname: explicit argument wins, otherwise the first ingress
# hostname in the cloudflared config the background service uses.
TUNNEL_HOST="${1:-}"
TUNNEL_HOST="${TUNNEL_HOST#https://}"
TUNNEL_HOST="${TUNNEL_HOST#wss://}"
TUNNEL_HOST="${TUNNEL_HOST%%/*}"
if [ -z "$TUNNEL_HOST" ]; then
    CONFIG="${CLOUDFLARED_CONFIG:-$HOME/.cloudflared/config-herdr-mobile-relay.yml}"
    if [ ! -r "$CONFIG" ]; then
        echo "✗ Cannot determine this relay's hostname: $CONFIG is missing."
        echo "  Follow the README's Stable Hostnames section first, or pass the"
        echo "  hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
    TUNNEL_HOST="$(sed -nE 's/^[[:space:]]*-?[[:space:]]*hostname:[[:space:]]*([^[:space:]#]+).*/\1/p' "$CONFIG" | head -1)"
    if [ -z "$TUNNEL_HOST" ]; then
        echo "✗ No ingress hostname found in $CONFIG."
        echo "  Pass the hostname directly: make setup-link HOST=relay-mac.yourdomain.com"
        exit 1
    fi
fi

HOST_LABEL="$(host_label)"
RELAY_URL="wss://$TUNNEL_HOST"
SETUP_FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$HOST_LABEL" "$RELAY_URL")"
PHONE_APP_FALLBACK="https://$TUNNEL_HOST"
PHONE_APP_BASE="$(choose_phone_app_base_url "$PHONE_APP_FALLBACK" "$ENV_FILE" stable)"
write_phone_app_origin "$PHONE_APP_BASE" "$ENV_FILE"
PHONE_URL="$PHONE_APP_BASE/#$SETUP_FRAGMENT"
DIRECT_URL="$PHONE_APP_FALLBACK/#$SETUP_FRAGMENT"
ARMED=0
arm_setup_link "$ENV_FILE" || ARMED=$?
echo "🐑 Herdr Mobile Relay phone setup"
echo ""
print_phone_setup "$PHONE_URL"
if [ "$PHONE_URL" != "$DIRECT_URL" ]; then
    echo ""
    echo "  Direct browser fallback:"
    print_phone_setup_url "$DIRECT_URL"
fi
echo ""
print_setup_link_arming "$ARMED"
echo "  The relay and tunnel must be running for the link to work:"
echo "  make service-status"

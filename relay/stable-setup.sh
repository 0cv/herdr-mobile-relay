#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
export PATH="$HOME/.local/bin:$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
ENV_FILE="$(canonical_file_path "$ENV_FILE")"
STATE_FILE="${HERDR_STABLE_STATE_FILE:-$(dirname "$ENV_FILE")/stable-setup.json}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-stable-setup.XXXXXX")"
ENV_WAS_PRESENT=false
ENV_CONFIG_WAS_PRESENT=false
STATE_WAS_PRESENT=false
LIST_READY=false
DNS_ROUTE_NEEDS_IDENTITY_PROOF=false
EXISTING_CONFIG_DNS_STATUS=2
SERVICE_WAS_INSTALLED=false

[ ! -f "$ENV_FILE" ] || ENV_WAS_PRESENT=true
[ ! -f "$STATE_FILE" ] || STATE_WAS_PRESENT=true
if [ -f "$ENV_FILE" ] && grep -q '^CLOUDFLARED_CONFIG=' "$ENV_FILE"; then
    ENV_CONFIG_WAS_PRESENT=true
fi

cleanup() {
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

state_command() {
    "$(relay_binary)" stable-state "$@"
}

state_get() {
    state_command get "$STATE_FILE" "$1"
}

state_update() {
    state_command update "$STATE_FILE" "$@"
}

rerun_command() {
    if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        echo "herdr plugin action invoke install-service --plugin herdr-mobile-relay.events"
        return
    fi
    if [ "$ENV_FILE" != "$(canonical_file_path "$SCRIPT_DIR/.env")" ]; then
        printf 'HERDR_RELAY_ENV=%q make stable-setup\n' "$ENV_FILE"
        return
    fi
    echo "make stable-setup"
}

fail_resumable() {
    echo "" >&2
    echo "Setup state was preserved in: $STATE_FILE" >&2
    echo "Rerun exactly:" >&2
    echo "  $(rerun_command)" >&2
    return 1
}

confirm_cloudflare_creation() {
    local confirmation

    echo ""
    echo "About to create Cloudflare resources in your account:"
    echo "  Tunnel:    $TUNNEL_NAME"
    echo "  DNS route: $RELAY_HOSTNAME"
    if [ "${HERDR_STABLE_YES:-}" = "1" ]; then
        return
    fi
    if [ ! -t 0 ]; then
        echo "✗ Confirmation required. Run interactively, or set HERDR_STABLE_YES=1." >&2
        return 1
    fi
    read -r -p "Create this tunnel and DNS route? [y/N] " confirmation || confirmation=""
    case "$confirmation" in
        y|Y|yes|YES) return ;;
        *)
            echo "Setup cancelled before creating Cloudflare resources."
            return 1
            ;;
    esac
}

confirm_existing_config_reuse() {
    local confirmation

    echo ""
    echo "An existing Cloudflare relay config was found:"
    echo "  Config:   $CONFIG"
    echo "  Tunnel:   $TUNNEL_NAME ($TUNNEL_UUID)"
    echo "  Hostname: $CONFIG_HOST"
    set +e
    dns_has_record "$CONFIG_HOST"
    EXISTING_CONFIG_DNS_STATUS=$?
    set -e
    case "$EXISTING_CONFIG_DNS_STATUS" in
        1) echo "  DNS:      no public record currently exists" ;;
        2) echo "  DNS:      could not be verified" ;;
        *) echo "  DNS:      public record exists" ;;
    esac
    echo ""
    echo "This config was not created by the current stable-setup state. Reusing it"
    echo "keeps the same tunnel and hostname; it does not create a new relay."

    case "${HERDR_STABLE_REUSE_CONFIG:-}" in
        1|true) return 0 ;;
        0|false) return 1 ;;
    esac
    if [ ! -t 0 ]; then
        echo "✗ Explicit confirmation is required before reusing this config." >&2
        echo "  Run interactively, or set HERDR_STABLE_REUSE_CONFIG=1." >&2
        return 1
    fi
    read -r -p "Reuse this existing tunnel and hostname? [y/N] " confirmation ||
        confirmation=""
    case "$confirmation" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

service_file_present() {
    case "$(uname -s)" in
        Darwin) [ -f "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ] ;;
        Linux) [ -f "$HOME/.config/systemd/user/herdr-mobile-relay.service" ] ;;
        *) return 1 ;;
    esac
}



dns_has_record() {
    local hostname="$1"
    local record_type
    local response

    for record_type in A AAAA CNAME; do
        if ! response="$(curl -fsS --max-time 5 -H 'accept: application/dns-json' \
            "https://cloudflare-dns.com/dns-query?name=$hostname&type=$record_type" 2>/dev/null)"; then
            return 2
        fi
        if printf '%s' "$response" | grep -q '"Answer"'; then
            return 0
        fi
    done
    return 1
}

select_cloudflare_domain() {
    local cert_zone
    local choice

    SELECTED_DOMAIN=""
    cert_zone="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
    if [ -z "$cert_zone" ]; then
        read -r -p "Cloudflare domain (for example, example.com): " SELECTED_DOMAIN
        return
    fi

    echo ""
    echo "Cloudflare domain authorized by the current login:"
    echo "  1. $cert_zone"
    echo "  2. Sign in to Cloudflare for another domain"
    while true; do
        read -r -p "Choice [1]: " choice || choice=""
        case "${choice:-1}" in
            1)
                SELECTED_DOMAIN="$cert_zone"
                return
                ;;
            2)
                if [ ! -t 0 ]; then
                    echo "✗ Cloudflare sign-in requires an interactive run." >&2
                    return 1
                fi
                if ! relogin_for_cloudflare_zone "$ORIGIN_CERT" "another domain"; then
                    return 1
                fi
                cert_zone="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
                if [ -z "$cert_zone" ]; then
                    echo "✗ Cloudflare sign-in completed, but its authorized domain could not be read." >&2
                    return 1
                fi
                echo "✓ Cloudflare authorized $cert_zone."
                SELECTED_DOMAIN="$cert_zone"
                return
                ;;
            *) echo "Enter 1 or 2." ;;
        esac
    done
}

ensure_hostname_certificate_zone() {
    local hostname="$1"
    local cert_zone
    local confirmation

    cert_zone="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
    if [ -z "$cert_zone" ]; then
        echo "▸ The Cloudflare login domain could not be read; the created route will be verified exactly."
        return
    fi
    if hostname_in_zone "$hostname" "$cert_zone"; then
        return
    fi

    echo ""
    echo "✗ The current Cloudflare login authorizes $cert_zone, not $hostname." >&2
    echo "  Continuing would create $hostname.$cert_zone instead." >&2
    case "${HERDR_STABLE_RELOGIN:-}" in
        1|true) confirmation="y" ;;
        0|false) confirmation="n" ;;
        *)
            if [ ! -t 0 ]; then
                echo "  Run interactively to authorize the correct domain." >&2
                return 1
            fi
            read -r -p "Sign in to Cloudflare for this hostname now? [Y/n] " confirmation ||
                confirmation=""
            ;;
    esac
    case "$confirmation" in
        ""|y|Y|yes|YES) ;;
        *) return 1 ;;
    esac

    if ! relogin_for_cloudflare_zone "$ORIGIN_CERT" "${hostname#*.}"; then
        return 1
    fi
    cert_zone="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
    if [ -z "$cert_zone" ] || ! hostname_in_zone "$hostname" "$cert_zone"; then
        echo "✗ The new Cloudflare login still does not authorize $hostname." >&2
        return 1
    fi
    echo "✓ Cloudflare authorized $cert_zone."
}

reconcile_dns_route_state() {
    local cert_zone
    local intended_status
    local stray_status

    MISROUTED_HOSTNAME="$(state_get misrouted_hostname)"
    cert_zone="$(cloudflare_cert_zone_name "$ORIGIN_CERT" || true)"
    if [ -z "$MISROUTED_HOSTNAME" ] &&
       [ "$CREATED_DNS" = true ] &&
       [ -n "$RELAY_HOSTNAME" ] &&
       [ -n "$cert_zone" ] &&
       ! hostname_in_zone "$RELAY_HOSTNAME" "$cert_zone"; then
        set +e
        dns_has_record "$RELAY_HOSTNAME"
        intended_status=$?
        set -e
        if [ "$intended_status" -eq 2 ]; then
            echo "✗ Could not verify the recorded hostname $RELAY_HOSTNAME." >&2
            return 1
        fi
        if [ "$intended_status" -eq 1 ]; then
            MISROUTED_HOSTNAME="$RELAY_HOSTNAME.$cert_zone"
        fi
    fi

    if [ -z "$MISROUTED_HOSTNAME" ]; then
        return
    fi
    set +e
    dns_has_record "$MISROUTED_HOSTNAME"
    stray_status=$?
    set -e
    if [ "$stray_status" -ne 1 ]; then
        state_update \
            "stage=route_mismatch" \
            "misrouted_hostname=$MISROUTED_HOSTNAME" \
            "created_dns=false"
        echo "✗ An earlier Cloudflare route created the wrong hostname:" >&2
        echo "  $MISROUTED_HOSTNAME" >&2
        echo "  Delete that DNS record in the Cloudflare dashboard, then rerun setup." >&2
        echo "  The tunnel and local config were preserved." >&2
        return 1
    fi

    echo "✓ The previous misrouted DNS record is gone; retrying $RELAY_HOSTNAME."
    state_update \
        "stage=hostname_selected" \
        "misrouted_hostname=" \
        "created_dns=false" \
        "dns_route_attempted=false"
    CREATED_DNS=false
    PREVIOUS_STAGE=hostname_selected
}

choose_hostname() {
    local recorded_hostname="$1"
    local previous_stage="$2"
    local domain
    local proposed
    local candidate
    local dns_status

    while true; do
        if [ -n "${HERDR_STABLE_HOSTNAME:-}" ]; then
            candidate="$HERDR_STABLE_HOSTNAME"
        elif [ -n "$recorded_hostname" ]; then
            read -r -p "Public hostname [$recorded_hostname]: " candidate || candidate=""
            candidate="${candidate:-$recorded_hostname}"
        else
            if [ -n "${HERDR_STABLE_DOMAIN:-}" ]; then
                domain="$HERDR_STABLE_DOMAIN"
            else
                if ! select_cloudflare_domain; then
                    return 1
                fi
                domain="$SELECTED_DOMAIN"
            fi
            domain="$(printf '%s' "$domain" | tr '[:upper:]' '[:lower:]')"
            domain="${domain#https://}"
            domain="${domain%%/*}"
            domain="${domain%.}"
            if ! valid_hostname "$domain" || [[ "$domain" != *.* ]]; then
                echo "✗ Enter a valid Cloudflare domain such as example.com." >&2
                [ -z "${HERDR_STABLE_DOMAIN:-}" ] || return 1
                continue
            fi
            proposed="relay-$(host_label | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-').$domain"
            read -r -p "Public hostname [$proposed]: " candidate || candidate=""
            candidate="${candidate:-$proposed}"
        fi

        candidate="$(printf '%s' "$candidate" | tr '[:upper:]' '[:lower:]')"
        candidate="${candidate#https://}"
        candidate="${candidate#wss://}"
        candidate="${candidate%%/*}"
        candidate="${candidate%.}"
        if ! valid_hostname "$candidate" || [[ "$candidate" != *.* ]]; then
            echo "✗ Enter a valid full hostname such as relay-workstation.example.com." >&2
            [ -z "${HERDR_STABLE_HOSTNAME:-}" ] || return 1
            recorded_hostname=""
            continue
        fi

        if ! ensure_hostname_certificate_zone "$candidate"; then
            [ -z "${HERDR_STABLE_HOSTNAME:-}" ] || return 1
            recorded_hostname=""
            continue
        fi

        set +e
        dns_has_record "$candidate"
        dns_status=$?
        set -e
        if [ "$dns_status" -eq 2 ]; then
            echo "✗ Could not query Cloudflare DNS-over-HTTPS for $candidate." >&2
            return 1
        fi
        if [ "$dns_status" -eq 1 ]; then
            RELAY_HOSTNAME="$candidate"
            return
        fi
        if [ "$previous_stage" = "routing_dns" ] && [ "$candidate" = "$recorded_hostname" ] && [ -n "${TUNNEL_UUID:-}" ]; then
            echo "▸ The recorded hostname now resolves. Verifying it against this relay before adopting the route."
            RELAY_HOSTNAME="$candidate"
            DNS_ROUTE_NEEDS_IDENTITY_PROOF=true
            return
        fi

        echo "✗ $candidate already has a public DNS record." >&2
        echo "  The wizard will not overwrite it. Choose another hostname." >&2
        [ -z "${HERDR_STABLE_HOSTNAME:-}" ] || return 1
        recorded_hostname=""
    done
}

print_login_guidance() {
    if [ "$(uname -s)" = "Darwin" ] || [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
        echo "Cloudflare authorization is required. cloudflared will print a URL and may open it in your desktop browser."
    else
        echo "Cloudflare authorization is required in this headless or remote session."
        echo "cloudflared will print a URL; open that exact URL manually in a browser, authorize the zone, then return here."
    fi
}

list_tunnels() {
    local output="$WORK_DIR/tunnels.json"
    local error="$WORK_DIR/tunnels.err"

    if ! cloudflared tunnel list --output json > "$output" 2> "$error"; then
        cat "$error" >&2
        print_login_guidance
        if ! cloudflared tunnel login; then
            echo "✗ Cloudflare login did not complete." >&2
            return 1
        fi
        if ! cloudflared tunnel list --output json > "$output" 2> "$error"; then
            cat "$error" >&2
            echo "✗ Cloudflare authorization is still unavailable after login." >&2
            return 1
        fi
    fi
    if ! state_command tunnel-id-by-name "$output" "__herdr_validation_only__" >/dev/null; then
        return 1
    fi
    LIST_FILE="$output"
    LIST_READY=true
}

ensure_tunnel_management() {
    if [ "$LIST_READY" = true ]; then
        return
    fi
    list_tunnels
}

validate_tunnel_config() {
    local config="$1"
    local expected_port="$2"
    local certificate_value
    local certificate_path=""
    local list_output="$WORK_DIR/config-tunnel-list.json"
    local list_error="$WORK_DIR/config-tunnel-list.err"

    if ! read_cloudflared_relay_config "$config" "$expected_port"; then
        return 1
    fi

    certificate_value="$(yaml_scalar origincert "$config")"
    if [ -n "${TUNNEL_ORIGIN_CERT:-}" ]; then
        certificate_path="$TUNNEL_ORIGIN_CERT"
    elif [ -n "$certificate_value" ]; then
        certificate_path="$(expand_config_path "$certificate_value" "$config")"
    elif [ -r "$HOME/.cloudflared/cert.pem" ]; then
        certificate_path="$HOME/.cloudflared/cert.pem"
    fi

    if [ -z "$certificate_path" ] || [ ! -r "$certificate_path" ]; then
        echo "  Tunnel credentials are usable; cert.pem is unavailable, so the management existence check is skipped."
        return
    fi
    if ! cloudflared tunnel --origincert "$certificate_path" list --id "$TUNNEL_UUID" --output json > "$list_output" 2> "$list_error"; then
        cat "$list_error" >&2
        echo "✗ Cloudflare could not confirm that tunnel $TUNNEL_UUID still exists." >&2
        return 1
    fi
    state_command tunnel-list-has "$list_output" "$TUNNEL_UUID"
    TUNNEL_NAME="$(state_command tunnel-name-by-id "$list_output" "$TUNNEL_UUID")"
}

write_generated_config() {
    local config="$1"
    local temp_file

    mkdir -p "$(dirname "$config")"
    chmod 700 "$(dirname "$config")"
    temp_file="$(mktemp "$(dirname "$config")/.cloudflared-config.XXXXXX")"
    cat > "$temp_file" <<EOF
tunnel: $TUNNEL_UUID
credentials-file: $CREDENTIALS_PATH

ingress:
  - hostname: $RELAY_HOSTNAME
    service: http://127.0.0.1:$PORT
  - service: http_status:404
EOF
    chmod 600 "$temp_file"
    mv "$temp_file" "$config"
}

wait_for_public_dns() {
    local timeout="${HERDR_STABLE_DNS_TIMEOUT:-90}"
    local delay="${HERDR_STABLE_POLL_DELAY:-2}"
    local deadline=$((SECONDS + timeout))
    local status

    printf '▸ Waiting up to %s seconds for public DNS' "$timeout"
    while true; do
        set +e
        dns_has_record "$RELAY_HOSTNAME"
        status=$?
        set -e
        if [ "$status" -eq 0 ]; then
            echo " ✓"
            return
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo ""
            echo "✗ Timed out after $timeout seconds waiting for public DNS for $RELAY_HOSTNAME." >&2
            return 1
        fi
        printf '.'
        sleep "$delay"
    done
}

wait_for_agent_readiness() {
    local timeout="${HERDR_STABLE_READY_TIMEOUT:-30}"
    local delay="${HERDR_STABLE_POLL_DELAY:-2}"
    local deadline=$((SECONDS + timeout))
    local ready_file="$WORK_DIR/local-ready.json"

    printf '▸ Waiting up to %s seconds for Herdr agent inventory' "$timeout"
    while true; do
        if curl -fsS --max-time 5 "http://127.0.0.1:$PORT/readyz" > "$ready_file" 2>/dev/null; then
            echo " ✓"
            return
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo ""
            curl -fsS --max-time 5 "http://127.0.0.1:$PORT/healthz" > "$LOCAL_HEALTH_FILE" 2>/dev/null || true
            echo "✗ The relay is running, but Herdr agent inventory is unavailable." >&2
            case "$(cat "$LOCAL_HEALTH_FILE" 2>/dev/null || true)" in
                *'"error_code": "protocol_mismatch"'*)
                    echo "  Run: herdr server live-handoff" >&2
                    ;;
                *)
                    echo "  Check the relay service logs, then rerun this setup." >&2
                    ;;
            esac
            return 1
        fi
        printf '.'
        sleep "$delay"
    done
}

wait_for_public_health() {
    local timeout="${HERDR_STABLE_HTTP_TIMEOUT:-60}"
    local delay="${HERDR_STABLE_POLL_DELAY:-2}"
    local deadline=$((SECONDS + timeout))
    local public_file="$WORK_DIR/public-health.json"
    local mismatch_file="$WORK_DIR/public-health.err"
    local received=false

    printf '▸ Waiting up to %s seconds for HTTPS relay health' "$timeout"
    while true; do
        if curl -fsS --max-time 5 "https://$RELAY_HOSTNAME/healthz" > "$public_file" 2>/dev/null; then
            received=true
            if state_command health-match "$LOCAL_HEALTH_FILE" "$public_file" 2> "$mismatch_file"; then
                echo " ✓"
                return
            fi
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo ""
            if [ "$received" = true ]; then
                echo "✗ Public health identity did not match the local relay:" >&2
                cat "$mismatch_file" >&2
            else
                echo "✗ Timed out after $timeout seconds waiting for https://$RELAY_HOSTNAME/healthz." >&2
            fi
            return 1
        fi
        printf '.'
        sleep "$delay"
    done
}

install_service() {
    local was_wizard_owned

    was_wizard_owned="$(state_get service_installed_by_wizard)"
    state_update "stage=installing_service"
    if ! "$SCRIPT_DIR/service.sh" install; then
        if [ "$SERVICE_WAS_INSTALLED" = false ] && service_file_present; then
            state_update "service_installed_by_wizard=true"
        fi
        echo "✗ The background service could not be installed or did not become healthy." >&2
        return 1
    fi
    if [ "$SERVICE_WAS_INSTALLED" = false ] || [ "$was_wizard_owned" = true ]; then
        state_update "service_installed_by_wizard=true"
    fi
}

if [ "${HERDR_STABLE_SETUP_WRAPPED:-}" != 1 ]; then
    echo "🐑 Herdr Mobile Relay stable tunnel setup"
    echo ""
fi

require_supported_platform
assert_service_env_matches "$ENV_FILE"
export HERDR_RELAY_ENV="$ENV_FILE"

if ! "$SCRIPT_DIR/setup.sh" --install-missing; then
    echo "✗ Prerequisite setup did not complete." >&2
    exit 1
fi

state_command init "$STATE_FILE" "$ENV_FILE"
if [ "$ENV_WAS_PRESENT" = false ]; then
    state_update "env_created_by_wizard=true"
fi
service_file_present && SERVICE_WAS_INSTALLED=true
if [ -z "$(state_get service_preexisting)" ]; then
    state_update "service_preexisting=$SERVICE_WAS_INSTALLED"
fi

load_relay_env "$ENV_FILE"
ORIGIN_CERT="${TUNNEL_ORIGIN_CERT:-$HOME/.cloudflared/cert.pem}"
PORT="${HERDR_RELAY_PORT:-8375}"
case "$PORT" in
    ""|*[!0-9]*|0)
        echo "✗ HERDR_RELAY_PORT must be a positive integer." >&2
        fail_resumable
        exit 1
        ;;
esac

PREVIOUS_STAGE="$(state_get stage)"
STATE_CREATED_CONFIG="$(state_get created_config)"
CONFIG="${CLOUDFLARED_CONFIG:-}"
if [ -z "$CONFIG" ] && [ -r "$HOME/.cloudflared/config-herdr-mobile-relay.yml" ]; then
    CONFIG="$HOME/.cloudflared/config-herdr-mobile-relay.yml"
fi

if [ "$STATE_CREATED_CONFIG" != true ] && [ -n "$CONFIG" ] && [ -f "$CONFIG" ]; then
    CONFIG="$(canonical_file_path "$CONFIG")"
    if ! validate_tunnel_config "$CONFIG" "$PORT"; then
        fail_resumable
        exit 1
    fi
    if [ "$(state_get existing_config_reuse_confirmed)" != true ]; then
        if ! confirm_existing_config_reuse; then
            [ "$STATE_WAS_PRESENT" = true ] || rm -f "$STATE_FILE"
            echo "" >&2
            echo "Left unchanged; the existing tunnel and config were not adopted." >&2
            echo "To keep this tunnel but move it, choose Change Tunnel Hostname." >&2
            echo "To create a different tunnel, first remove the preserved custom" >&2
            echo "Cloudflare tunnel and config, then run Stable Tunnel again." >&2
            exit 1
        fi
        state_update "existing_config_reuse_confirmed=true"
    else
        set +e
        dns_has_record "$CONFIG_HOST"
        EXISTING_CONFIG_DNS_STATUS=$?
        set -e
    fi
    if [ "$EXISTING_CONFIG_DNS_STATUS" -eq 1 ]; then
        echo "✗ The existing config points to $CONFIG_HOST, but that hostname has no public DNS record." >&2
        echo "  Stable setup will not silently recreate an old route." >&2
        echo "  Choose Change Tunnel Hostname, or restore its DNS record explicitly." >&2
        fail_resumable
        exit 1
    fi
    echo "▸ Reusing confirmed Cloudflare tunnel config without modifying it: $CONFIG"
    RELAY_HOSTNAME="$CONFIG_HOST"
    state_update \
        "stage=config_validated" \
        "tunnel_uuid=$TUNNEL_UUID" \
        "tunnel_name=$TUNNEL_NAME" \
        "hostname=$RELAY_HOSTNAME" \
        "credentials_path=$CREDENTIALS_PATH" \
        "config_path=$CONFIG" \
        "created_tunnel=false" \
        "created_dns=false" \
        "dns_route_attempted=false" \
        "created_credentials=false" \
        "created_config=false"
    if ! grep -q '^CLOUDFLARED_CONFIG=' "$ENV_FILE"; then
        state_update "env_config_added_by_wizard=true"
        set_env_value_atomic "$ENV_FILE" CLOUDFLARED_CONFIG "$CONFIG"
        CLOUDFLARED_CONFIG="$CONFIG"
        export CLOUDFLARED_CONFIG
    fi
else
    TUNNEL_UUID="$(state_get tunnel_uuid)"
    TUNNEL_NAME="$(state_get tunnel_name)"
    RELAY_HOSTNAME="$(state_get hostname)"
    CREDENTIALS_PATH="$(state_get credentials_path)"
    CONFIG="$(state_get config_path)"
    CREATED_DNS="$(state_get created_dns)"

    if [ -z "$TUNNEL_NAME" ]; then
        COMPUTER_LABEL="$(host_label | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
        TUNNEL_NAME="herdr-mobile-relay-${COMPUTER_LABEL:-relay}"
    fi
    if [ -z "$CONFIG" ]; then
        CONFIG="$(dirname "$ENV_FILE")/cloudflared/config.yml"
    fi
    if [ -z "$CREDENTIALS_PATH" ]; then
        CREDENTIALS_PATH="$(dirname "$ENV_FILE")/cloudflared/tunnel-credentials.json"
    fi
    CONFIG="$(canonical_file_path "$CONFIG")"
    CREDENTIALS_PATH="$(canonical_file_path "$CREDENTIALS_PATH")"

    if ! reconcile_dns_route_state; then
        fail_resumable
        exit 1
    fi

    if [ "$CREATED_DNS" != true ] && [ "$PREVIOUS_STAGE" != routing_dns ]; then
        if ! ensure_tunnel_management; then
            fail_resumable
            exit 1
        fi
    fi
    if [ "$CREATED_DNS" != true ]; then
        if ! choose_hostname "$RELAY_HOSTNAME" "$PREVIOUS_STAGE"; then
            fail_resumable
            exit 1
        fi
        if [ "$DNS_ROUTE_NEEDS_IDENTITY_PROOF" = true ]; then
            DNS_ROUTE_ATTEMPTED=true
        else
            DNS_ROUTE_ATTEMPTED=false
        fi
        state_update \
            "stage=hostname_selected" \
            "tunnel_name=$TUNNEL_NAME" \
            "hostname=$RELAY_HOSTNAME" \
            "credentials_path=$CREDENTIALS_PATH" \
            "config_path=$CONFIG" \
            "dns_route_attempted=$DNS_ROUTE_ATTEMPTED"
    elif [ -z "$RELAY_HOSTNAME" ]; then
        echo "✗ Recorded DNS stage has no hostname." >&2
        fail_resumable
        exit 1
    fi

    if [ -z "$TUNNEL_UUID" ]; then
        if ! ensure_tunnel_management; then
            fail_resumable
            exit 1
        fi
        if [ -f "$CREDENTIALS_PATH" ]; then
            case "$PREVIOUS_STAGE" in
                creating_tunnel|tunnel_created|config_written|routing_dns|dns_routed|installing_service|waiting_for_dns|waiting_for_https|complete)
                    ;;
                *)
                    echo "✗ Refusing to adopt an unowned credentials file: $CREDENTIALS_PATH" >&2
                    fail_resumable
                    exit 1
                    ;;
            esac
            TUNNEL_UUID="$(state_command credential-id "$CREDENTIALS_PATH")"
            chmod 600 "$CREDENTIALS_PATH"
            state_update \
                "stage=tunnel_created" \
                "tunnel_uuid=$TUNNEL_UUID" \
                "created_tunnel=true" \
                "created_credentials=true"
            echo "▸ Recovered the recorded tunnel from its credentials file: $TUNNEL_UUID"
        else
            EXISTING_TUNNEL_ID="$(state_command tunnel-id-by-name "$LIST_FILE" "$TUNNEL_NAME")"
            if [ -n "$EXISTING_TUNNEL_ID" ]; then
                echo "✗ An unrecorded Cloudflare tunnel already uses the name $TUNNEL_NAME." >&2
                echo "  The wizard will not adopt or replace it." >&2
                fail_resumable
                exit 1
            fi
            if ! confirm_cloudflare_creation; then
                fail_resumable
                exit 1
            fi
            mkdir -p "$(dirname "$CREDENTIALS_PATH")"
            chmod 700 "$(dirname "$CREDENTIALS_PATH")"
            state_update "stage=creating_tunnel"
            CREATE_OUTPUT="$WORK_DIR/tunnel-create.json"
            echo "▸ Creating dedicated Cloudflare tunnel $TUNNEL_NAME..."
            if ! cloudflared tunnel create --output json --credentials-file "$CREDENTIALS_PATH" "$TUNNEL_NAME" > "$CREATE_OUTPUT"; then
                echo "✗ Cloudflare tunnel creation failed." >&2
                fail_resumable
                exit 1
            fi
            if [ ! -f "$CREDENTIALS_PATH" ]; then
                echo "✗ cloudflared did not write the requested credentials file: $CREDENTIALS_PATH" >&2
                fail_resumable
                exit 1
            fi
            chmod 600 "$CREDENTIALS_PATH"
            TUNNEL_UUID="$(state_command create-id "$CREATE_OUTPUT")"
            CREDENTIAL_TUNNEL_UUID="$(state_command credential-id "$CREDENTIALS_PATH")"
            if [ "$TUNNEL_UUID" != "$CREDENTIAL_TUNNEL_UUID" ]; then
                echo "✗ Created tunnel UUID does not match its credentials file." >&2
                fail_resumable
                exit 1
            fi
            state_update \
                "stage=tunnel_created" \
                "tunnel_uuid=$TUNNEL_UUID" \
                "created_tunnel=true" \
                "created_credentials=true"
        fi
    fi

    if [ ! -r "$CREDENTIALS_PATH" ]; then
        echo "✗ Recorded tunnel credentials are not readable: $CREDENTIALS_PATH" >&2
        fail_resumable
        exit 1
    fi
    CREDENTIAL_TUNNEL_UUID="$(state_command credential-id "$CREDENTIALS_PATH")"
    if [ "$TUNNEL_UUID" != "$CREDENTIAL_TUNNEL_UUID" ]; then
        echo "✗ Recorded tunnel UUID does not match $CREDENTIALS_PATH." >&2
        fail_resumable
        exit 1
    fi

    if [ -e "$CONFIG" ] && [ "$STATE_CREATED_CONFIG" != true ] && [ "$PREVIOUS_STAGE" != writing_config ]; then
        echo "✗ Refusing to overwrite an unowned Cloudflare config: $CONFIG" >&2
        fail_resumable
        exit 1
    fi
    state_update \
        "stage=writing_config" \
        "tunnel_uuid=$TUNNEL_UUID" \
        "tunnel_name=$TUNNEL_NAME" \
        "hostname=$RELAY_HOSTNAME" \
        "credentials_path=$CREDENTIALS_PATH" \
        "config_path=$CONFIG"
    write_generated_config "$CONFIG"
    state_update \
        "stage=config_written" \
        "tunnel_uuid=$TUNNEL_UUID" \
        "tunnel_name=$TUNNEL_NAME" \
        "hostname=$RELAY_HOSTNAME" \
        "credentials_path=$CREDENTIALS_PATH" \
        "config_path=$CONFIG" \
        "created_config=true"
    STATE_CREATED_CONFIG=true

    if [ "${CLOUDFLARED_CONFIG:-}" != "$CONFIG" ]; then
        state_update "env_config_added_by_wizard=true"
        set_env_value_atomic "$ENV_FILE" CLOUDFLARED_CONFIG "$CONFIG"
        CLOUDFLARED_CONFIG="$CONFIG"
        export CLOUDFLARED_CONFIG
    elif [ "$ENV_CONFIG_WAS_PRESENT" = false ]; then
        state_update "env_config_added_by_wizard=true"
    fi

    if ! validate_tunnel_config "$CONFIG" "$PORT"; then
        fail_resumable
        exit 1
    fi
    # Validation may resolve a name through cert.pem; generated resources keep
    # the deterministic wizard-owned name recorded before the list call.
    TUNNEL_NAME="$(state_get tunnel_name)"

    if [ "$CREATED_DNS" != true ] && [ "$DNS_ROUTE_NEEDS_IDENTITY_PROOF" != true ]; then
        if ! ensure_tunnel_management; then
            fail_resumable
            exit 1
        fi
        state_update "stage=routing_dns" "dns_route_attempted=true"
        echo "▸ Routing $RELAY_HOSTNAME to tunnel $TUNNEL_UUID..."
        if ! ROUTE_OUTPUT="$(cloudflared tunnel route dns "$TUNNEL_UUID" "$RELAY_HOSTNAME" 2>&1)"; then
            printf '%s\n' "$ROUTE_OUTPUT" >&2
            echo "✗ Cloudflare could not create the DNS route." >&2
            echo "  The domain must belong to the zone selected during cloudflared tunnel login." >&2
            echo "  A conflicting DNS record must be removed or another hostname selected." >&2
            fail_resumable
            exit 1
        fi
        printf '%s\n' "$ROUTE_OUTPUT"
        ROUTED_NAME="$(cloudflare_routed_hostname "$ROUTE_OUTPUT")"
        if [ -z "$ROUTED_NAME" ]; then
            state_update "stage=route_unverified" "created_dns=false"
            echo "✗ Cloudflare returned success without naming the routed hostname." >&2
            echo "  Setup will not assume that the requested DNS record was created." >&2
            fail_resumable
            exit 1
        fi
        if [ "$ROUTED_NAME" != "$RELAY_HOSTNAME" ]; then
            state_update \
                "stage=route_mismatch" \
                "misrouted_hostname=$ROUTED_NAME" \
                "created_dns=false"
            echo "✗ Cloudflare created $ROUTED_NAME, not $RELAY_HOSTNAME." >&2
            echo "  Delete $ROUTED_NAME in Cloudflare DNS, then rerun setup." >&2
            echo "  The tunnel and local config were preserved." >&2
            fail_resumable
            exit 1
        fi
        state_update \
            "stage=dns_routed" \
            "misrouted_hostname=" \
            "created_dns=true"
    fi
fi

if ! install_service; then
    fail_resumable
    exit 1
fi

LOCAL_HEALTH_FILE="$WORK_DIR/local-health.json"
if ! curl -fsS --max-time 5 "http://127.0.0.1:$PORT/healthz" > "$LOCAL_HEALTH_FILE" 2>/dev/null; then
    echo "✗ The installed service is not reachable on 127.0.0.1:$PORT." >&2
    fail_resumable
    exit 1
fi
if ! state_command health-valid "$LOCAL_HEALTH_FILE"; then
    echo "✗ The local relay health response is incomplete." >&2
    fail_resumable
    exit 1
fi
if ! wait_for_agent_readiness; then
    fail_resumable
    exit 1
fi

state_update "stage=waiting_for_dns"
if ! wait_for_public_dns; then
    fail_resumable
    exit 1
fi
state_update "stage=waiting_for_https"
if ! wait_for_public_health; then
    fail_resumable
    exit 1
fi

if [ "$DNS_ROUTE_NEEDS_IDENTITY_PROOF" = true ]; then
    state_update "created_dns=true"
fi
state_update "stage=complete" "dns_cleanup_required=false"

echo ""
echo "✓ Stable relay verified: https://$RELAY_HOSTNAME"
echo "  Public status, instance, version, and protocol match the local relay."
echo ""
exec "$SCRIPT_DIR/setup-link.sh" "$RELAY_HOSTNAME"

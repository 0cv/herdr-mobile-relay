#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
load_relay_env "$ENV_FILE"

# The menu opens after every install, so it has to answer "what do I have" before
# it asks "what next". Every probe is bounded and optional: a status line that
# cannot be determined is omitted, never fatal.
installed_release_version() {
    local manifest="$(relay_release_root)/current/release-manifest.json"

    [ -f "$manifest" ] || return 1
    sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1
}

running_health() {
    local port="${HERDR_RELAY_PORT:-8375}"

    curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null
}

own_gateway_summary() {
    local installed="$1"
    local available="$2"
    local state_file
    local host
    local health
    local deployed
    local action="redeploy with 3"

    state_file="$(dirname "$ENV_FILE")/gateway-deploy"
    host="$(env_file_value "$state_file" HERDR_GATEWAY_DEPLOY_HOST)"
    [ -n "$host" ] || return 0
    if [ -n "$available" ] && [ -n "$installed" ] && [ "$available" != "$installed" ]; then
        action="run herdr plugin install 0cv/herdr-mobile-relay, then redeploy with 3"
    fi
    if ! health="$(curl -fsS --max-time 3 "https://$host/healthz" 2>/dev/null)"; then
        printf '%s is unreachable, version unknown' "$host"
    else
        deployed="$(json_string_field "$health" version)"
        if [ -n "$deployed" ]; then
            if [ -n "$available" ] && [ "$deployed" = "$available" ]; then
                printf '%s runs %s (latest available)\n' "$host" "$deployed"
                return 0
            fi
            printf '%s runs %s' "$host" "$deployed"
        else
            printf '%s is healthy, version unknown' "$host"
        fi
    fi
    if [ -n "$available" ]; then
        printf '; plugin offers %s - %s\n' "$available" "$action"
    else
        printf ' - %s\n' "$action"
    fi
}

service_state() {
    case "$(uname -s)" in
        Darwin)
            [ -f "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ] || return 1
            launchd_service_loaded "gui/$(id -u)/com.herdr-mobile-relay.service" &&
                printf 'installed (loaded)\n' || printf 'installed (not loaded)\n'
            ;;
        Linux)
            [ -f "$HOME/.config/systemd/user/herdr-mobile-relay.service" ] || return 1
            printf 'installed (%s)\n' \
                "$(systemctl --user is-active herdr-mobile-relay.service 2>/dev/null || echo unknown)"
            ;;
        *) return 1 ;;
    esac
}

transport_summary() {
    local health="$1"
    local available="$2"
    local gateways
    local active
    local current
    local count

    gateways="$(gateway_urls "$ENV_FILE")"
    if [ -n "$gateways" ]; then
        active="$(json_string_field "$health" gateway_url)"
        [ -n "$active" ] || active="$(json_string_field "$health" url)"
        [ -n "$active" ] || active="${gateways%%,*}"
        current="$(json_string_field "$health" gateway_version)"
        printf 'gateway %s' "$active"
        if [ -n "$current" ]; then
            printf ' runs %s' "$current"
            if [ -n "$available" ] && [ "$current" = "$available" ]; then
                printf ' (latest available)'
            elif [ -n "$available" ]; then
                printf '; latest available %s' "$available"
            fi
        elif [ -n "$available" ]; then
            printf ' (version unknown; latest available %s)' "$available"
        else
            printf ' (version unknown)'
        fi
        # Commas, not lines: the list has no trailing newline for wc to count.
        count=$(($(printf '%s' "$gateways" | tr -cd ',' | wc -c) + 1))
        if [ "$count" -gt 1 ]; then
            printf ' (+%s fallback)' "$((count - 1))"
        fi
        printf '\n'
        return 0
    fi
    if [ -n "${CLOUDFLARED_CONFIG:-}" ] && [ -f "$CLOUDFLARED_CONFIG" ]; then
        printf 'Cloudflare tunnel %s\n' \
            "$(sed -n 's/^[[:space:]]*-\{0,1\}[[:space:]]*hostname:[[:space:]]*\(.*\)/\1/p' \
                "$CLOUDFLARED_CONFIG" | head -1)"
        return 0
    fi
    printf 'not chosen yet - start with 1\n'
}

print_status() {
    local installed running health available service app_origin deployed own_gateway

    installed="$(installed_release_version || true)"
    health="$(running_health || true)"
    running="$(json_string_field "$health" release_version)"
    available="$(json_string_field "$health" gateway_available_version)"
    [ -n "$available" ] || available="$installed"
    if [ -n "$installed" ]; then
        if [ -z "$running" ]; then
            printf '  Relay:      %s installed, not running\n' "$installed"
        elif [ "$running" = "$installed" ]; then
            printf '  Relay:      %s running\n' "$running"
        else
            printf '  Relay:      %s installed, %s still running - restart pending\n' \
                "$installed" "$running"
        fi
    fi
    service="$(service_state || true)"
    [ -z "$service" ] || printf '  Service:    %s\n' "$service"
    own_gateway="$(own_gateway_summary "$installed" "$available" || true)"
    [ -z "$own_gateway" ] || printf '  Own gateway: %s\n' "$own_gateway"
    printf '  Phone path: %s\n' "$(transport_summary "$health" "$available")"
    app_origin="$(phone_app_base_url "" "$ENV_FILE" 2>/dev/null || true)"
    if [ -n "$app_origin" ]; then
        deployed="$(curl -fsS --max-time 3 "$app_origin/version.json" 2>/dev/null |
            sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -1 || true)"
        if [ -z "$deployed" ]; then
            printf '  Phone app:  %s (version unknown)\n' "$app_origin"
        elif [ -n "$installed" ] && [ "$deployed" != "$installed" ]; then
            printf '  Phone app:  %s serves %s, this relay ships %s - deploy with 8\n' \
                "$app_origin" "$deployed" "$installed"
        else
            printf '  Phone app:  %s serves %s\n' "$app_origin" "$deployed"
        fi
    fi
}

render_menu() {
    echo "🐑 Herdr Mobile Relay Setup"
    echo ""
    print_status
    echo ""
    echo "Choose a complete setup action:"
    echo ""
    echo "Connection"
    echo ""
    menu_item 1 "Temporary Cloudflare Tunnel"
    echo "     Start a foreground relay and temporary URL, then print its QR."
    echo "     An installed background relay is restarted instead of duplicated."
    echo ""
    menu_item 2 "Community WebRTC Gateway"
    echo "     Check the project's shared gateways, save the healthy candidates,"
    echo "     then start or restart the relay and print its QR."
    echo ""
    menu_item 3 "Deploy or Upgrade Your Own WebRTC Gateway"
    echo "     Copy the gateway shipped by this plugin to your server over SSH,"
    echo "     then start or restart the relay and print its QR."
    echo ""
    echo "Stable Cloudflare tunnel"
    echo ""
    menu_item 4 "Stable Tunnel"
    echo "     Guided permanent hostname, dedicated tunnel, and background service."
    echo ""
    menu_item 5 "Change Tunnel Hostname"
    echo "     Move this relay to a different tunnel hostname and reprint the QR."
    echo ""
    menu_item 6 "Remove Stable Tunnel"
    echo "     Remove this relay's recorded service, tunnel, config, and credentials."
    echo ""
    echo "Phone app"
    echo ""
    menu_item 7 "Choose Phone App and Show QR"
    echo "     Keep or change the shared app origin, then reprint the private setup QR."
    echo ""
    menu_item 8 "Configure App Deployment"
    echo "     Designate this computer as the deployment owner and pin its shared app origin."
    echo ""
    echo "Diagnostics"
    echo ""
    menu_item 9 "Show Full Status"
    echo "     Service, health, and a sanitized support snapshot."
    echo ""
    menu_item q "Exit, change nothing"
    echo ""
}

# Every action runs as a child, so finishing one comes back here with the status
# recomputed instead of ending the pane. Ctrl-C belongs to the action: the menu
# must survive it without swallowing it. A handler, never `trap '' INT` - an
# ignored signal is inherited by children as SIG_IGN, which would leave a
# prompt loop with no way out at all. Actions pause here rather than inside each
# script, which is why pause_before_close stands down under HERDR_SETUP_MENU.
run_action() {
    local action="$1"
    shift

    echo ""
    trap 'printf "\n"' INT
    (
        cd "$SCRIPT_DIR"
        HERDR_SETUP_MENU=1 "$action" "$@"
    ) || true
    unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
    load_relay_env "$ENV_FILE"
    trap - INT
    if [ -t 0 ]; then
        echo ""
        read -r -p "Press Enter to return to the menu." _answer || return 0
    fi
}

while true; do
    render_menu
    while true; do
        if ! read -r -p "Choice [1]: " choice; then
            echo ""
            exit 0
        fi
        case "${choice:-1}" in
            1) run_action "$SCRIPT_DIR/plugin-choose-transport.sh" temporary; break ;;
            2) run_action "$SCRIPT_DIR/plugin-choose-transport.sh" community; break ;;
            3) run_action "$SCRIPT_DIR/plugin-choose-transport.sh" own; break ;;
            4) run_action "$SCRIPT_DIR/plugin-install-service.sh"; break ;;
            5) run_action "$SCRIPT_DIR/plugin-change-hostname.sh"; break ;;
            6) run_action "$SCRIPT_DIR/plugin-stable-teardown.sh"; break ;;
            7) run_action "$SCRIPT_DIR/plugin-setup-link.sh"; break ;;
            8) run_action "$SCRIPT_DIR/plugin-configure-app-deploy.sh"; break ;;
            9) run_action "$SCRIPT_DIR/plugin-status.sh"; break ;;
            q | Q) exit 0 ;;
            *) echo "Enter 1, 2, 3, 4, 5, 6, 7, 8, 9, or q." ;;
        esac
    done
done

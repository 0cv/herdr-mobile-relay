#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
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

running_release_version() {
    local port="${HERDR_RELAY_PORT:-8375}"
    local health

    health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null)" || return 1
    printf '%s' "$health" |
        sed -n 's/.*"release_version":"\([^"]*\)".*/\1/p' | head -1
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
    local gateways
    local count

    gateways="$(gateway_urls "$ENV_FILE")"
    if [ -n "$gateways" ]; then
        # Commas, not lines: the list has no trailing newline for wc to count.
        count=$(($(printf '%s' "$gateways" | tr -cd ',' | wc -c) + 1))
        if [ "$count" -gt 1 ]; then
            printf 'gateway %s (+%s fallback)\n' "${gateways%%,*}" "$((count - 1))"
        else
            printf 'gateway %s\n' "$gateways"
        fi
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
    local installed running service app_origin deployed

    installed="$(installed_release_version || true)"
    running="$(running_release_version || true)"
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
    printf '  Phone path: %s\n' "$(transport_summary)"
    app_origin="$(phone_app_base_url "" "$ENV_FILE" 2>/dev/null || true)"
    if [ -n "$app_origin" ]; then
        deployed="$(curl -fsS --max-time 3 "$app_origin/version.json" 2>/dev/null |
            sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -1 || true)"
        if [ -z "$deployed" ]; then
            printf '  Phone app:  %s (version unknown)\n' "$app_origin"
        elif [ -n "$installed" ] && [ "$deployed" != "$installed" ]; then
            printf '  Phone app:  %s serves %s, this relay ships %s - deploy with 6\n' \
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
    echo "Choose how you want to start:"
    echo ""
    # Grouped by which world the entry belongs to: the tunnel entries only make
    # sense together, and a reader scanning for one of them should not have to
    # read the gateway and phone-app entries to find it.
    echo "Connection"
    echo ""
    menu_item 1 "Choose Connection Method"
    echo "     Decide how your phone reaches this computer: the community gateway"
    echo "     (free, shared, run by the project), your own gateway (best"
    echo "     performance, you own the bandwidth and the logs), or a Cloudflare"
    echo "     tunnel."
    echo ""
    menu_item 2 "Quick Start (recommended)"
    echo "     Installs missing tools, starts the relay, and prints the phone setup"
    echo "     QR code using whichever connection method is configured."
    echo ""
    echo "Cloudflare tunnel"
    echo ""
    menu_item 3 "Stable Tunnel"
    echo "     Guided permanent Cloudflare hostname, dedicated tunnel, and background service."
    echo ""
    menu_item 4 "Change Tunnel Hostname"
    echo "     Move this relay to a different tunnel hostname and reprint the QR."
    echo ""
    menu_item 5 "Remove Stable Tunnel"
    echo "     Remove this relay's recorded service, tunnel, config, and credentials."
    echo ""
    echo "Phone app"
    echo ""
    menu_item 6 "Show Phone Setup QR"
    echo "     Reprint the private link and QR for the installed relay, tunnel or gateway."
    echo ""
    menu_item 7 "Configure App Deployment"
    echo "     Let this computer deploy one separately hosted Cloudflare Pages app."
    echo ""
    echo "Diagnostics"
    echo ""
    menu_item 8 "Show Full Status"
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

    echo ""
    trap 'printf "\n"' INT
    HERDR_SETUP_MENU=1 "$action" || true
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
            1) run_action "$SCRIPT_DIR/plugin-choose-transport.sh"; break ;;
            2) run_action "$SCRIPT_DIR/plugin-quick-start.sh"; break ;;
            3) run_action "$SCRIPT_DIR/plugin-install-service.sh"; break ;;
            4) run_action "$SCRIPT_DIR/plugin-change-hostname.sh"; break ;;
            5) run_action "$SCRIPT_DIR/plugin-stable-teardown.sh"; break ;;
            6) run_action "$SCRIPT_DIR/plugin-setup-link.sh"; break ;;
            7) run_action "$SCRIPT_DIR/plugin-configure-app-deploy.sh"; break ;;
            8) run_action "$SCRIPT_DIR/plugin-status.sh"; break ;;
            q | Q) exit 0 ;;
            *) echo "Enter 1, 2, 3, 4, 5, 6, 7, 8, or q." ;;
        esac
    done
done

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# The menu belongs to no relay: it picks the instance for each probe and action.
unset HERDR_RELAY_BACKEND
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

# Runs only inside a subshell (see in_instance): it rewrites the environment so
# the chosen relay resolves its own env file, service and ports. The core relay
# never inherits the Herdr TUI pane's configuration.
select_instance() {
    local backend="$1"

    if [ "$backend" = hc ]; then
        unset HERDR_RELAY_ENV HERDR_PLUGIN_CONFIG_DIR HERDR_RELAY_PORT HERDR_RELAY_PLUGIN_PORT \
            HERDR_TAILSCALE_PORT HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION CLOUDFLARED_CONFIG \
            HERDR_BIN HERDR_BIN_PATH HERDR_SOCKET_PATH
    fi
    export HERDR_RELAY_BACKEND="$backend"
    resolve_relay_instance
    adopt_installed_service_env
    ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
    load_relay_env "$ENV_FILE"
}

in_instance() {
    local backend="$1"
    shift
    (
        select_instance "$backend" || exit 1
        "$@"
    )
}

instance_value() {
    (
        select_instance "$1" >/dev/null 2>&1 || exit 1
        printf '%s' "${!2}"
    )
}

instance_configured() {
    [ -f "$ENV_FILE" ]
}

# The menu opens after every install, so it has to answer "what do I have" before
# it asks "what next". Every probe is bounded and optional: a status line that
# cannot be determined is omitted, never fatal.
installed_release_version() {
    local manifest
    manifest="$(relay_release_root)/current/release-manifest.json"

    [ -f "$manifest" ] || return 1
    sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1
}

running_health() {
    local port="${HERDR_RELAY_PORT:-$RELAY_DEFAULT_PORT}"

    curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null
}

own_gateway_summary() {
    local installed="$1"
    local available="$2"
    local state_file
    local host
    local health
    local deployed
    local action="redeploy with WebRTC → Deploy or Upgrade Your Own WebRTC Gateway"

    state_file="$(dirname "$ENV_FILE")/gateway-deploy"
    host="$(env_file_value "$state_file" HERDR_GATEWAY_DEPLOY_HOST)"
    [ -n "$host" ] || return 0
    if [ -n "$available" ] && [ -n "$installed" ] && [ "$available" != "$installed" ]; then
        action="run herdr plugin install 0cv/herdr-mobile-relay, then redeploy with WebRTC → Deploy or Upgrade Your Own WebRTC Gateway"
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
    local state

    case "$(uname -s)" in
        Darwin)
            [ -f "$HOME/Library/LaunchAgents/$RELAY_LAUNCHD_LABEL.plist" ] || return 1
            launchd_service_loaded "gui/$(id -u)/$RELAY_LAUNCHD_LABEL" &&
                printf 'installed (loaded)\n' || printf 'installed (not loaded)\n'
            ;;
        Linux)
            [ -f "$HOME/.config/systemd/user/$RELAY_SYSTEMD_LABEL" ] || return 1
            state="$(systemctl --user is-active "$RELAY_SYSTEMD_LABEL" 2>/dev/null || true)"
            printf 'installed (%s)\n' "${state:-unknown}"
            ;;
        *) return 1 ;;
    esac
}

active_path() {
    if [ -n "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_PORT)" ]; then
        printf 'tailscale\n'
    elif [ -n "$(gateway_urls "$ENV_FILE")" ]; then
        printf 'webrtc\n'
    elif [ -n "${CLOUDFLARED_CONFIG:-}" ] && [ -f "$CLOUDFLARED_CONFIG" ]; then
        printf 'cloudflare\n'
    fi
}

transport_summary() {
    local health="$1"
    local available="$2"
    local gateways
    local active
    local current
    local count

    if [ -n "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_PORT)" ]; then
        printf 'Tailscale Serve (HTTPS port %s)\n' "$RELAY_TAILSCALE_PORT"
        return 0
    fi
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
            # Which rule picked this gateway is the answer to "why that one",
            # so it belongs beside the gateway rather than only in relay.env.
            printf ' (+%s fallback, %s)' "$((count - 1))" \
                "$(gateway_selection_label "$(env_file_value "$ENV_FILE" HERDR_GATEWAY_SELECTION)")"
        fi
        printf '\n'
        return 0
    fi
    if [ -n "${CLOUDFLARED_CONFIG:-}" ] && [ -f "$CLOUDFLARED_CONFIG" ]; then
        printf 'Cloudflare tunnel %s\n' \
            "$(yaml_scalar hostname "$CLOUDFLARED_CONFIG")"
        return 0
    fi
    printf 'not chosen yet - choose a path\n'
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
            printf '  Phone app:  %s serves %s, this relay ships %s' \
                "$app_origin" "$deployed" "$installed"
            if [ "$HERDR_RELAY_BACKEND" = hc ]; then
                printf '\n'
            else
                printf ' - Cloudflare → Configure App Deployment\n'
            fi
        else
            printf '  Phone app:  %s serves %s\n' "$app_origin" "$deployed"
        fi
    fi
}

core_server_running() {
    local server watchdog status=0

    hc server status >/dev/null 2>&1 &
    server=$!
    (sleep 3; kill "$server" 2>/dev/null) >/dev/null 2>&1 &
    watchdog=$!
    { wait "$server"; } 2>/dev/null || status=$?
    kill "$watchdog" 2>/dev/null || true
    return "$status"
}

print_instance_status() {
    if [ "$HERDR_RELAY_BACKEND" = hc ]; then
        echo "Herdr app relay (phone shows $(relay_host_label))"
        if core_server_running; then
            echo "  Core:       running"
        else
            echo "  Core:       not running - Start herdr-app or run \`hc server start\`"
        fi
    else
        echo "Herdr TUI relay (phone shows $(relay_host_label))"
    fi
    print_status
}

print_all_status() {
    local backend

    for backend in $detected; do
        in_instance "$backend" print_instance_status || echo "  Cannot read this relay's configuration."
        echo ""
    done
}

# Screens are lists of tokens: targets, then paths, then the actions of a path.
# Rendering and dispatch both read the same list, so numbers cannot drift.
target_backends() {
    case "$1" in
        tui) echo herdr ;;
        core) echo hc ;;
        both) echo "herdr hc" ;;
    esac
}

target_name() {
    case "$1" in
        tui) echo "Herdr TUI relay" ;;
        core) echo "Herdr app relay" ;;
    esac
}

target_paths() {
    case "$1" in
        tui) echo "cloudflare webrtc tailscale" ;;
        core) echo "cloudflare tailscale" ;;
        both) echo "stable on qr status" ;;
    esac
}

path_title() {
    case "$1" in
        cloudflare) echo "Cloudflare" ;;
        webrtc) echo "WebRTC" ;;
        tailscale) echo "Tailscale" ;;
    esac
}

path_items() {
    case "$2" in
        cloudflare)
            if [ "$1" = core ]; then
                echo "temporary stable hostname remove qr status"
            else
                echo "temporary stable hostname remove deploy qr status"
            fi
            ;;
        webrtc) echo "community own qr status" ;;
        tailscale) echo "on off qr status" ;;
    esac
}

screen_tokens() {
    if [ -z "$target" ]; then
        echo "tui core both"
    elif [ -n "$path" ]; then
        path_items "$target" "$path"
    else
        target_paths "$target"
    fi
}

tailscale_ports() {
    local backend

    for backend in $(target_backends "$target"); do
        instance_value "$backend" RELAY_TAILSCALE_PORT
        printf ' '
    done
}

render_item() {
    local number="$1"
    local token="$2"
    local ports

    case "$token" in
        temporary)
            menu_item "$number" "Temporary Cloudflare Tunnel"
            echo "     Start a foreground relay and temporary URL, then print its QR."
            echo "     An installed background relay is restarted instead of duplicated."
            ;;
        stable)
            if [ "$target" = both ]; then
                menu_item "$number" "Cloudflare Stable Tunnel"
                echo "     Guided permanent hostname and background service for the TUI relay,"
                echo "     then for the core relay, each followed by its own QR."
            else
                menu_item "$number" "Stable Tunnel"
                echo "     Guided permanent hostname, dedicated tunnel, and background service."
            fi
            ;;
        hostname)
            menu_item "$number" "Change Tunnel Hostname"
            echo "     Move this relay to a different tunnel hostname and reprint the QR."
            ;;
        remove)
            menu_item "$number" "Remove Stable Tunnel"
            echo "     Remove this relay's recorded service, tunnel, config, and credentials."
            ;;
        deploy)
            menu_item "$number" "Configure App Deployment"
            echo "     Make this computer the Cloudflare Pages deployment owner and pin its shared app origin."
            ;;
        community)
            menu_item "$number" "Community WebRTC Gateway"
            echo "     Check the project's shared gateways, save the healthy candidates,"
            echo "     then start or restart the relay and print its QR."
            ;;
        own)
            menu_item "$number" "Deploy or Upgrade Your Own WebRTC Gateway"
            echo "     Copy the gateway shipped by this plugin to your server over SSH,"
            echo "     then start or restart the relay and print its QR."
            ;;
        on)
            menu_item "$number" "Always on Tailscale"
            read -ra ports <<< "$(tailscale_ports)"
            if [ "$target" = both ]; then
                echo "     Publish HTTPS port ${ports[0]} for the TUI relay and ${ports[1]} for the core relay,"
                echo "     each with its own background service and QR."
            else
                echo "     Publish HTTPS port ${ports[0]} and install a background relay service."
            fi
            ;;
        off)
            menu_item "$number" "Turn Always on Tailscale Off"
            echo "     Remove only this relay's port $(tailscale_ports) and stop its service."
            ;;
        qr)
            menu_item "$number" "Choose Phone App and Show QR"
            echo "     Keep or change the shared app origin, then reprint the private setup QR."
            ;;
        status)
            menu_item "$number" "Show Full Status"
            echo "     Service, health, and a sanitized support snapshot."
            ;;
    esac
    echo ""
}

item_command() {
    case "$1" in
        temporary) echo "plugin-choose-transport.sh temporary" ;;
        stable) echo "plugin-install-service.sh" ;;
        hostname) echo "plugin-change-hostname.sh" ;;
        remove) echo "plugin-stable-teardown.sh" ;;
        deploy) echo "plugin-configure-app-deploy.sh" ;;
        community) echo "plugin-choose-transport.sh community" ;;
        own) echo "plugin-choose-transport.sh own" ;;
        on) echo "tailscale-setup.sh" ;;
        off) echo "tailscale-off.sh" ;;
        qr) echo "plugin-setup-link.sh" ;;
        status) echo "plugin-status.sh" ;;
    esac
}

render_menu() {
    local number=0 token heading

    [ ! -t 1 ] || printf '\033[H\033[2J'
    echo "🐑 Herdr Mobile Relay Setup"
    echo ""
    if [ -z "$path" ]; then
        print_all_status
        if [ -z "$target" ]; then
            echo "What should your phone control?"
            echo ""
            menu_item 1 "Herdr TUI"
            echo "     The relay for the Herdr terminal app. The phone shows $(host_label)."
            echo ""
            menu_item 2 "Herdr app (core)"
            echo "     A second relay for herdr-app. The phone shows $(host_label) (core)."
            echo ""
            menu_item 3 "Both"
            echo "     Set up both relays with one connection type, each paired separately."
            echo ""
        elif [ "$target" = both ]; then
            echo "Choose a path for both relays:"
            echo ""
            for token in "${tokens[@]}"; do
                number=$((number + 1))
                render_item "$number" "$token"
            done
        else
            echo "Choose a path:"
            echo ""
            for token in "${tokens[@]}"; do
                number=$((number + 1))
                menu_item "$number" "$(path_title "$token")$([ "$active" != "$token" ] || printf ' (active)')"
            done
            echo ""
        fi
    else
        heading="$(path_title "$path")"
        [ "$asked" = 0 ] || heading="$heading - $(target_name "$target")"
        echo "$heading"
        echo ""
        for token in "${tokens[@]}"; do
            number=$((number + 1))
            render_item "$number" "$token"
        done
    fi
    [ "$can_back" = 0 ] || menu_item b "Back"
    menu_item q "Exit, change nothing"
    echo ""
}

# Every action runs as a child, so finishing one comes back here with the status
# recomputed instead of ending the pane. Each relay's action runs in its own
# instance, so a core action never sees the TUI's environment and vice versa.
# Ctrl-C belongs to the action: the menu must survive it without swallowing it.
# A handler, never `trap '' INT` - an ignored signal is inherited by children as
# SIG_IGN, which would leave a prompt loop with no way out at all. Actions pause
# here rather than inside each script, which is why pause_before_close stands
# down under HERDR_SETUP_MENU.
run_item() {
    local command="$1"
    local backend
    local status

    echo ""
    trap 'printf "\n"' INT
    for backend in $(target_backends "$target"); do
        if [ "$target" = both ]; then
            case "$backend" in
                herdr) echo "── Herdr TUI relay ──" ;;
                hc) echo "── Herdr app relay ──" ;;
            esac
        fi
        status=0
        (
            select_instance "$backend" || exit 1
            cd "$SCRIPT_DIR"
            # shellcheck disable=SC2086
            set -- $command
            HERDR_SETUP_MENU=1 "$SCRIPT_DIR/$1" "${@:2}"
        ) || status=$?
        [ "$status" -ne 130 ] || break
    done
    trap - INT
    if [ -t 0 ]; then
        echo ""
        if ! read -r -p "Press Enter to return to the menu." _answer; then
            echo ""
            exit 0
        fi
    fi
}

detected=""
if command -v "${HERDR_BIN_PATH:-herdr}" >/dev/null 2>&1 || in_instance herdr instance_configured; then
    detected="herdr"
fi
if command -v hc >/dev/null 2>&1 || in_instance hc instance_configured; then
    detected="$detected hc"
fi
detected="${detected:-herdr}"
detected="${detected# }"

target=""
asked=0
case "$detected" in
    herdr) target=tui ;;
    hc) target=core ;;
    *) asked=1 ;;
esac
path=""
while true; do
    can_back=0
    if [ -n "$path" ] || { [ -n "$target" ] && [ "$asked" = 1 ]; }; then
        can_back=1
    fi
    read -ra tokens <<< "$(screen_tokens)"
    active=""
    default=1
    if [ -n "$target" ] && [ -z "$path" ] && [ "$target" != both ]; then
        active="$(in_instance "$(target_backends "$target")" active_path || true)"
        for index in "${!tokens[@]}"; do
            [ "${tokens[$index]}" != "$active" ] || default=$((index + 1))
        done
    fi
    render_menu
    while true; do
        if ! read -r -p "Choice [$default]: " choice; then
            echo ""
            exit 0
        fi
        choice="${choice:-$default}"
        case "$choice" in
            q | Q) exit 0 ;;
            b | B)
                if [ "$can_back" = 1 ]; then
                    if [ -n "$path" ]; then path=""; else target=""; fi
                    break
                fi
                ;;
            *[!0-9]* | 0*) ;;
            *)
                if [ "$choice" -le "${#tokens[@]}" ]; then
                    token="${tokens[$((choice - 1))]}"
                    if [ -z "$target" ]; then
                        target="$token"
                    elif [ -n "$path" ] || [ "$target" = both ]; then
                        run_item "$(item_command "$token")"
                    else
                        path="$token"
                    fi
                    break
                fi
                ;;
        esac
        if [ "$can_back" = 1 ]; then
            echo "Enter 1–${#tokens[@]}, b or q."
        else
            echo "Enter 1–${#tokens[@]} or q."
        fi
    done
done

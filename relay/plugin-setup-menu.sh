#!/usr/bin/env bash
set -euo pipefail

# Navigation owns no configuration or lifecycle authority. No common.sh/env
# loader, process/service probes, network tools or parent-side config parser.
SCRIPT_PARENT=${BASH_SOURCE[0]%/*}
[ "$SCRIPT_PARENT" != "${BASH_SOURCE[0]}" ] || SCRIPT_PARENT=.
if ! SCRIPT_DIR="$(CDPATH='' cd -- "$SCRIPT_PARENT" 2>/dev/null && pwd -P)"; then
    printf '%s\n' 'Setup menu unavailable.' >&2
    exit 1
fi
if ! cd -- "$SCRIPT_DIR" 2>/dev/null; then
    printf '%s\n' 'Setup menu unavailable.' >&2
    exit 1
fi

render_main() {
    printf '\n%s\n\n' 'Herdr Mobile Relay Setup'
    printf '\n%s\n' \
        'Connection' \
        '  1. Cloudflare' \
        '  2. WebRTC Gateway' \
        '  3. Tailscale' \
        '' \
        'Phone app' \
        '  4. Show setup QR — reprint the pairing QR and choose which phone app it opens' \
        '  5. Publish the phone app to your Cloudflare Pages (advanced)' \
        '     One stable app address for all computers, kept updated by this computer;' \
        '     needed for gateway unless an app address already exists.' \
        '' \
        'Diagnostics' \
        '  6. Show Full Status' \
        '  q. Exit, change nothing' \
        ''
}
render_submenu() {
    case "$PAGE" in
        cloudflare)
            printf '\n%s\n' 'Cloudflare' \
                '  1. Temporary tunnel' \
                '  2. Stable tunnel / background service' \
                '  3. Change hostname' \
                '  4. Remove stable tunnel'
            ;;
        gateway)
            printf '\n%s\n' 'WebRTC Gateway' \
                '  1. Community gateways' \
                '  2. Deploy or upgrade your own gateway (existing chooser)'
            ;;
        tailscale)
            printf '\n%s\n' 'Tailscale' \
                '  1. Always on — installed CLI Serve setup + pairing (requires a qualified profile and production enablement)' \
                '  2. Your own HTTPS route (advanced BYO)' \
                "  3. Remove Herdr's own route — selected owned route only, with explicit consent"
            ;;
    esac
    printf '%s\n\n' '  b. Back' '  q. Exit, change nothing'
}

MENU_EOF=0
run_action() {
    local action="$1"
    local status
    shift
    printf '\n'
    # A handler, NOT SIG_IGN: children retain their own INT cancellation policy.
    trap 'printf "\n"' INT
    if (
        cd -- "$SCRIPT_DIR" 2>/dev/null || exit 1
        HERDR_SETUP_MENU=1 "$SCRIPT_DIR/$action" "$@"
    ); then status=0; else status=$?; fi
    trap - INT
    if [ "$status" -ne 0 ]; then
        printf 'Action did not complete (exit %d); no follow-up action was requested.\n' "$status"
    fi
    # Child wrappers stand down under HERDR_SETUP_MENU. One parent pause only,
    # never consume the next piped navigation choice. EOF exits, not success.
    if [ -t 0 ] && [ -t 1 ]; then
        if printf '\nPress Enter to return to the menu.'; then
            if ! read -r _answer; then MENU_EOF=1; fi
        fi
    fi
}

PAGE=main
while true; do
    if [ "$PAGE" = main ]; then render_main; else render_submenu; fi
    while true; do
        if [ "$PAGE" = main ]; then printf 'Choice [1 opens Cloudflare]: '; else printf 'Choice: '; fi
        if ! IFS= read -r choice; then printf '\n'; exit 0; fi
        case "$choice" in q|Q) exit 0 ;; esac
        case "$PAGE:$choice" in
            main:|main:1) PAGE=cloudflare; break ;;
            main:2) PAGE=gateway; break ;;
            main:3) PAGE=tailscale; break ;;
            main:4) run_action plugin-setup-link.sh; break ;;
            main:5) run_action plugin-configure-app-deploy.sh; break ;;
            main:6) run_action plugin-status.sh; break ;;
            cloudflare:b|cloudflare:B|gateway:b|gateway:B|tailscale:b|tailscale:B) PAGE=main; break ;;
            cloudflare:1) run_action plugin-choose-transport.sh temporary; break ;;
            cloudflare:2) run_action plugin-install-service.sh; break ;;
            cloudflare:3) run_action plugin-change-hostname.sh; break ;;
            cloudflare:4) run_action plugin-stable-teardown.sh; break ;;
            gateway:1) run_action plugin-choose-transport.sh community; break ;;
            gateway:2) run_action plugin-choose-transport.sh own; break ;;
            tailscale:1) run_action plugin-choose-transport.sh tailscale-cli; break ;;
            tailscale:2) run_action plugin-choose-transport.sh tailscale-external; break ;;
            tailscale:3) run_action tailscale-cli.sh unpublish; break ;;
            *) printf '%s\n' 'Invalid choice; select a listed option, Back in a submenu, or q.' ;;
        esac
    done
    [ "$MENU_EOF" -eq 0 ] || exit 0
    # Action returns stay in their submenu; only Back enters the main menu.
done

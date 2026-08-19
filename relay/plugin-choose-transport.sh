#!/bin/bash
# Guided chooser for how the phone reaches this computer. Every option ends by
# writing (or clearing) the HERDR_GATEWAY_URL candidate list in the relay
# environment, which is the single switch the rest of the tooling reads, plus
# the HERDR_GATEWAY_SELECTION policy that decides how that list is read.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
CURRENT="$(gateway_urls "$ENV_FILE")"
COMMUNITY="$(community_gateway_url)"

echo "🐑 Choose how your phone reaches this computer"
echo ""
if [ -n "$CURRENT" ]; then
    echo "Currently using gateway candidates: $CURRENT"
else
    echo "Currently using a Cloudflare tunnel."
fi
echo ""
echo "Whichever you pick, traffic stays end-to-end encrypted and the phone"
echo "upgrades to a direct connection whenever the network allows it. The"
echo "choice only decides who carries the fallback when it cannot."
echo ""
menu_item 1 "Cloudflare tunnel (recommended)"
echo "     The original transport, and the default. Quick Start opens a"
echo "     temporary URL with no Cloudflare account and no domain."
echo ""
menu_item 2 "Community gateway"
echo "     Run by the project, free, shared, no account and no domain needed."
echo "     Best-effort capacity: fine for normal use, not for heavy transfers."
if [ -z "$COMMUNITY" ]; then
    echo "     (No community gateway is published yet — choose 1, 3, or 4.)"
fi
echo ""
menu_item 3 "Stable Cloudflare tunnel"
echo "     A permanent hostname, a dedicated tunnel, and a background service."
echo "     Needs a Cloudflare account with a domain; the wizard is resumable."
echo ""
menu_item 4 "Your own gateway"
echo "     Best performance and privacy: you own the box, the bandwidth, and"
echo "     the logs. Deploy one to a small VPS from here over SSH, or point at"
echo "     one you already run."
echo ""
menu_item q "Leave the current setting unchanged"
echo ""

# Every path into here normalizes every candidate first: the relay only accepts
# ws:// or wss:// entries, so community constants or typed addresses written in
# https:// form must be canonicalized before they reach the env file. Setup
# checks each health endpoint but requires only one to answer; an unavailable
# entry remains useful as a cold fallback. The selection policy is a parameter
# because both the community list and an operator's own list arrive here, and
# only the community one wants its candidates ranked by latency.
use_gateways() {
    local selection="$2"
    local urls
    local old_ifs
    local url
    local healthy=0
    local count

    if ! urls="$(normalize_gateway_urls "$1")"; then
        echo "✗ $1 is not a usable gateway candidate list."
        return 1
    fi
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $urls
    IFS="$old_ifs"
    count=$#
    for url in "$@"; do
        printf '▸ Checking %s..' "$url"
        if gateway_answers_healthz "$url"; then
            echo " ✓"
            healthy=$((healthy + 1))
        else
            echo " unavailable"
        fi
    done
    if [ "$healthy" -eq 0 ]; then
        echo ""
        echo "✗ No gateway health endpoint answered."
        echo "  Check the addresses, TLS termination, and that at least one"
        echo "  gateway is running. Plain HTTP is accepted only on loopback."
        return 1
    fi
    set_gateway_url "$ENV_FILE" "$urls"
    set_gateway_selection "$ENV_FILE" "$selection"
    echo ""
    if [ "$count" -eq 1 ]; then
        echo "✓ This relay will use $urls."
    elif [ "$selection" = "latency" ]; then
        echo "✓ Saved $count gateway candidates."
        echo "  The relay will register with the lowest-latency healthy one."
    else
        echo "✓ Saved $count gateway candidates."
        echo "  The relay will register with the first healthy one in that order."
    fi
    echo "  Run Quick Start to register and print the phone QR."
    return 0
}

choose_own_gateway() {
    local entered

    echo ""
    echo "  a. Deploy one on my own server over SSH"
    echo "  b. I already run a gateway — enter its address"
    echo "  c. Back"
    echo ""
    while true; do
        read -r -p "Choice [a]: " sub
        case "${sub:-a}" in
            a|A)
                exec "$SCRIPT_DIR/gateway-deploy.sh"
                ;;
            b|B)
                while true; do
                    if ! read -r -p "Gateway address(es), comma-separated, or q to cancel: " entered; then
                        echo ""
                        return 1
                    fi
                    case "$entered" in
                        q | Q)
                            echo "Left unchanged."
                            return 1
                            ;;
                    esac
                    [ -n "$entered" ] || continue
                    # An operator's own list is a priority order, not a pool of
                    # equivalents: entry one is theirs and must win.
                    if use_gateways "$entered" ordered; then
                        return 0
                    fi
                done
                ;;
            c|C)
                return 1
                ;;
            *)
                echo "Enter a, b, or c."
                ;;
        esac
    done
}

while true; do
    if ! read -r -p "Choice [1]: " choice; then
        echo ""
        exit 0
    fi
    case "${choice:-1}" in
        1)
            set_gateway_url "$ENV_FILE" ""
            echo ""
            echo "✓ This relay will use a Cloudflare tunnel."
            echo "  Run Quick Start for a temporary URL."
            exit 0
            ;;
        2)
            if [ -z "$COMMUNITY" ]; then
                echo "✗ No community gateway is published yet."
                echo "  Choose 4 to run your own, or 1 to keep using Cloudflare."
                continue
            fi
            # The published candidates are interchangeable, so the closest one
            # is the right one — this is the only path that ranks by latency.
            if use_gateways "$COMMUNITY" latency; then
                exit 0
            fi
            ;;
        3)
            # The stable wizard owns the Cloudflare path end to end, so the
            # gateway switch has to be cleared before it runs: a leftover
            # HERDR_GATEWAY_URL would send start.sh and the QR down the gateway
            # path and ignore the tunnel it just provisioned.
            set_gateway_url "$ENV_FILE" ""
            exec "$SCRIPT_DIR/plugin-install-service.sh"
            ;;
        4)
            if choose_own_gateway; then
                exit 0
            fi
            ;;
        q|Q)
            exit 0
            ;;
        *)
            echo "Enter 1, 2, 3, 4, or q."
            ;;
    esac
done

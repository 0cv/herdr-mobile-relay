#!/bin/bash
# Guided chooser for how the phone reaches this computer. Every option ends by
# writing (or clearing) the HERDR_GATEWAY_URL candidate list in the relay
# environment, which is the single switch the rest of the tooling reads, plus
# the HERDR_GATEWAY_SELECTION policy that decides how that list is read.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"
# shellcheck source=/dev/null
. "$SCRIPT_DIR/common.sh"

MODE="${1:-}"
case "$MODE" in
    temporary | stable | community | own) ;;
    *)
        echo "Usage: $0 {temporary|stable|community|own}" >&2
        exit 2
        ;;
esac

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
CURRENT="$(gateway_urls "$ENV_FILE")"
COMMUNITY="$(community_gateway_url)"


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
                if "$SCRIPT_DIR/gateway-deploy.sh" &&
                    [ -n "$(gateway_urls "$ENV_FILE")" ]; then
                    return 0
                fi
                return 1
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

case "$MODE" in
    temporary)
        set_gateway_url "$ENV_FILE" ""
        echo "✓ Temporary Cloudflare tunnel selected."
        if installed_relay_service_active; then
            echo "  A background relay is already installed, so Quick Start will"
            echo "  restart it and show its current QR instead of binding a second relay."
        fi
        exec "$SCRIPT_DIR/plugin-quick-start.sh"
        ;;
    stable)
        exec "$SCRIPT_DIR/plugin-install-service.sh"
        ;;
    community)
        if [ -z "$COMMUNITY" ]; then
            echo "✗ No community gateway is published yet."
            echo "  Choose Your Own Gateway or a Cloudflare tunnel instead."
            exit 1
        fi
        if use_gateways "$COMMUNITY" latency; then
            exec "$SCRIPT_DIR/plugin-quick-start.sh"
        fi
        exit 1
        ;;
    own)
        if choose_own_gateway; then
            exec "$SCRIPT_DIR/plugin-quick-start.sh"
        fi
        exit 1
        ;;
esac

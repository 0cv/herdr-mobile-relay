#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

tailscale_app_store_installed() {
    local home="${HOME:-}" bundle candidate link directory attempt configured_candidate
    local -a candidates=()
    bundle="/Applications/Tailscale.app"
    [ -f "$bundle/Contents/_MASReceipt/receipt" ] && return 0
    if [ -n "$home" ]; then
        bundle="$home/Applications/Tailscale.app"
        [ -f "$bundle/Contents/_MASReceipt/receipt" ] && return 0
    fi

    configured_candidate="${HERDR_DEV_TAILSCALE_CLI_BIN:-${HERDR_TAILSCALE_CLI_BIN:-}}"
    [ -n "$configured_candidate" ] && candidates+=("$configured_candidate")
    candidate="$(command -v tailscale 2>/dev/null || true)"
    [ -n "$candidate" ] && candidates+=("$candidate")
    candidates+=(/usr/local/bin/tailscale /opt/homebrew/bin/tailscale
        /Applications/Tailscale.app/Contents/MacOS/Tailscale)
    if [ -n "$home" ]; then
        candidates+=("$home/Applications/Tailscale.app/Contents/MacOS/Tailscale")
    fi

    for candidate in "${candidates[@]}"; do
        attempt=0
        while [ "$attempt" -lt 8 ]; do
            attempt=$((attempt + 1))
            case "$candidate" in
                */Tailscale.app/*)
                    bundle="${candidate%%/Contents/*}"
                    [ -f "$bundle/Contents/_MASReceipt/receipt" ] && return 0
                    ;;
            esac
            [ -L "$candidate" ] || break
            link="$(readlink "$candidate" 2>/dev/null || true)"
            [ -n "$link" ] || break
            case "$link" in
                /*) candidate="$link" ;;
                *) directory="${candidate%/*}"; candidate="$directory/$link" ;;
            esac
        done
    done
    return 1
}

# Choose before building or opening the tunnel: managed Serve has its own
# private root and consent gate and must never inherit this path's .dev state.
case "${HERDR_DEV_TRANSPORT:-}" in
    tailscale) exec "$SCRIPT_DIR/dev-tailscale.sh" "$@" ;;
    tailscale-cli) exec "$SCRIPT_DIR/dev-tailscale-cli.sh" "$@" ;;
    ''|tunnel) ;;
    *) echo "✗ HERDR_DEV_TRANSPORT must be tunnel, tailscale, or tailscale-cli." >&2; exit 2 ;;
esac
if [ -z "${HERDR_DEV_TRANSPORT:-}" ] && [ -t 0 ]; then
    option3_unavailable_reason=""
    if [ "$(uname -s)" = Darwin ] && tailscale_app_store_installed; then
        option3_unavailable_reason="App Store Tailscale was detected; legacy mode requires a standalone tailscaled."
    fi
    echo "Development transport:"
    echo "  1. Temporary Cloudflare tunnel — quick public URL + QR; no Tailscale needed (or use a saved gateway)."
    echo "  2. CLI-backed Tailscale Serve — for App Store Tailscale on macOS/arm64; needs the signed-in app's supported CLI and explicit route consent. The HTTPS route persists after stop; runtime/phone qualification is pending."
    if [ -n "$option3_unavailable_reason" ]; then
        echo "  3. Legacy Tailscale Serve — for advanced users with a supported standalone tailscaled; needs an authenticated Unix daemon. Not for the App Store app. [UNAVAILABLE: $option3_unavailable_reason]"
    else
        echo "  3. Legacy Tailscale Serve — for advanced users with a supported standalone tailscaled; needs an authenticated Unix daemon. Not for the App Store app; owns a temporary foreground route."
    fi
    read -r -p "Choice [1]: " choice || { echo "Cancelled; nothing was started." >&2; exit 2; }
    case "$choice" in
        ''|1) ;;
        2) exec "$SCRIPT_DIR/dev-tailscale-cli.sh" "$@" ;;
        3)
            if [ -n "$option3_unavailable_reason" ]; then
                echo "✗ Option 3 unavailable: $option3_unavailable_reason" >&2
                exit 2
            fi
            HERDR_DEV_TAILSCALE_ENABLE=1 exec "$SCRIPT_DIR/dev-tailscale.sh" "$@"
            ;;
        *) echo "✗ Choose 1, 2, or 3." >&2; exit 2 ;;
    esac
fi
DEV_DIR="${HERDR_DEV_CONFIG_DIR:-$SCRIPT_DIR/.dev}"

# Canonicalize a directory without requiring it to exist. Existing parents are
# resolved physically; missing components are kept textually (portable on
# macOS/Linux, no realpath -m).
canonical_dir() {
    local path="$1"
    local directory
    local name

    directory="$(dirname "$path")"
    name="$(basename "$path")"
    if [ -d "$directory" ]; then
        directory="$(cd "$directory" && pwd -P)"
    fi
    printf '%s/%s\n' "${directory%/}" "$name"
}

# The development directory must never be the production relay configuration
# root or a directory inside it: the production root holds relay.env, push and
# device-auth state, and this script mkdirs/chmods its target before setup.
production_config_roots=()
if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
    production_config_roots+=("$HERDR_PLUGIN_CONFIG_DIR")
else
    production_config_roots+=("${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay")
fi
if [ -n "${HERDR_RELAY_ENV:-}" ]; then
    production_config_roots+=("$(dirname "$HERDR_RELAY_ENV")")
fi

canonical_dev_dir="$(canonical_dir "$DEV_DIR")"
for production_root in "${production_config_roots[@]}"; do
    canonical_production_root="$(canonical_dir "$production_root")"
    if [ "$canonical_dev_dir" = "$canonical_production_root" ]; then
        refuse=1
    else
        case "$canonical_dev_dir" in
            "$canonical_production_root"/*) refuse=1 ;;
            *) refuse=0 ;;
        esac
    fi
    if [ "$refuse" = 1 ]; then
        printf '✗ Refusing HERDR_DEV_CONFIG_DIR=%s: it is the production relay configuration root or inside it.\n' "$DEV_DIR" >&2
        printf '  Choose a private development directory such as relay/.dev.\n' >&2
        exit 1
    fi
done
unset canonical_dev_dir canonical_production_root refuse

DEV_BIN_DIR="$DEV_DIR/bin"

mkdir -p "$DEV_DIR" "$DEV_BIN_DIR"
chmod 700 "$DEV_DIR"

unset HERDR_PLUGIN_CONFIG_DIR
export HERDR_DEV_TUNNEL=1
export HERDR_RELAY_ENV="${HERDR_DEV_RELAY_ENV:-$DEV_DIR/relay.env}"
export HERDR_RELAY_HOST="127.0.0.1"
export HERDR_RELAY_PORT="${HERDR_DEV_RELAY_PORT:-18375}"
export HERDR_RELAY_PLUGIN_PORT="${HERDR_DEV_PLUGIN_PORT:-18376}"
export HERDR_WEB_ROOT="$REPO_DIR/frontend/dist"
if [ -z "${HERDR_RELAY_BIN:-}" ]; then
    "$REPO_DIR/scripts/build.sh" "$DEV_BIN_DIR"
    export HERDR_RELAY_BIN="$DEV_BIN_DIR/herdr-mobile-relay"
fi

echo "🐑 Herdr Mobile Relay development tunnel"
echo ""
echo "  Config:      $HERDR_RELAY_ENV"
echo "  Relay:       http://127.0.0.1:$HERDR_RELAY_PORT"
echo "  Plugin UDP:  127.0.0.1:$HERDR_RELAY_PLUGIN_PORT"
echo "  Web root:    $HERDR_WEB_ROOT"
echo "  Binary:      $HERDR_RELAY_BIN"
echo "  Production relay port 8375 and its configuration are not used."
echo ""

if ! command -v bun >/dev/null 2>&1; then
    echo "✗ bun is required for make dev-tunnel. Install Bun 1.4 first." >&2
    exit 1
fi

bun run --cwd "$REPO_DIR/frontend" build
"$SCRIPT_DIR/setup.sh" --install-missing
exec "$SCRIPT_DIR/start.sh"

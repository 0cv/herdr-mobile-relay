#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

TAILSCALE_APP_STORE_BUNDLE=""
tailscale_app_store_installed() {
    local home="${HOME:-}" bundle candidate link directory attempt configured_candidate
    local -a candidates=()
    TAILSCALE_APP_STORE_BUNDLE=""
    bundle="/Applications/Tailscale.app"
    if [ -f "$bundle/Contents/_MASReceipt/receipt" ]; then
        TAILSCALE_APP_STORE_BUNDLE="$bundle"
        return 0
    fi
    if [ -n "$home" ]; then
        bundle="$home/Applications/Tailscale.app"
        if [ -f "$bundle/Contents/_MASReceipt/receipt" ]; then
            TAILSCALE_APP_STORE_BUNDLE="$bundle"
            return 0
        fi
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
                    if [ -f "$bundle/Contents/_MASReceipt/receipt" ]; then
                        TAILSCALE_APP_STORE_BUNDLE="$bundle"
                        return 0
                    fi
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

tailscale_app_store_version() {
    local bundle="$1" info_plist version
    info_plist="$bundle/Contents/Info.plist"
    [ -r "$info_plist" ] || return 1
    command -v plutil >/dev/null 2>&1 || return 1
    version="$(plutil -extract CFBundleShortVersionString raw -o - "$info_plist" 2>/dev/null)" || return 1
    [ -n "$version" ] || return 1
    printf '%s\n' "$version"
}

# Keep menu availability aligned with ResolveBinary without executing any
# candidate. Canonical paths, not wrapper contents or command behavior, define
# identity just as filepath.EvalSymlinks does in the Go resolver.
MENU_CLI_CANONICAL=""
MENU_CLI_CANDIDATES=()
MENU_CLI_CANDIDATE_COUNT=0
MENU_CLI_UNAVAILABLE_REASON=""
MENU_APP_STORE_CLI=/Applications/Tailscale.app/Contents/MacOS/Tailscale
# The App Store app's "Install CLI" action installs this exact two-line
# launcher, which only forwards to the bundle executable. Like ResolveBinary,
# treat exactly these bytes as an alias of the bundle candidate; any other
# script stays a distinct candidate. The launcher is compared, never run.
menu_cli_is_app_store_wrapper() {
    # shellcheck disable=SC2016 # The literal "$@" is part of the compared launcher bytes.
    [ -f "$1" ] && command -v cmp >/dev/null 2>&1 &&
        cmp -s "$1" <(printf '#!/bin/sh\n%s "$@"\n' "$MENU_APP_STORE_CLI")
}
canonical_menu_cli_candidate() {
    local candidate="$1" link directory base attempt
    case "$candidate" in /*) ;; *) return 1 ;; esac
    attempt=0
    while [ -L "$candidate" ]; do
        attempt=$((attempt + 1))
        [ "$attempt" -le 64 ] || return 1
        link="$(readlink "$candidate" 2>/dev/null || true)"
        [ -n "$link" ] || return 1
        case "$link" in
            /*) candidate="$link" ;;
            *) directory="${candidate%/*}"; candidate="$directory/$link" ;;
        esac
    done
    [ -f "$candidate" ] && [ -x "$candidate" ] || return 1
    directory="$(cd -P "$(dirname "$candidate")" 2>/dev/null && pwd -P)" || return 1
    base="$(basename "$candidate")"
    if [ "$directory" = / ]; then
        MENU_CLI_CANONICAL="/$base"
    else
        MENU_CLI_CANONICAL="$directory/$base"
    fi
    [ -f "$MENU_CLI_CANONICAL" ] && [ -x "$MENU_CLI_CANONICAL" ]
}

append_menu_cli_candidate() {
    local candidate="$1" goos="${2:-}" existing duplicate=0 resolved
    if canonical_menu_cli_candidate "$candidate"; then
        resolved="$MENU_CLI_CANONICAL"
        if [ "$goos" = Darwin ] && menu_cli_is_app_store_wrapper "$resolved" &&
            canonical_menu_cli_candidate "$MENU_APP_STORE_CLI"; then
            resolved="$MENU_CLI_CANONICAL"
        fi
        if [ "$MENU_CLI_CANDIDATE_COUNT" -gt 0 ]; then
            for existing in "${MENU_CLI_CANDIDATES[@]}"; do
                if [ "$existing" = "$resolved" ]; then
                    duplicate=1
                    break
                fi
            done
        fi
        if [ "$duplicate" = 0 ]; then
            MENU_CLI_CANDIDATES+=("$resolved")
            MENU_CLI_CANDIDATE_COUNT=$((MENU_CLI_CANDIDATE_COUNT + 1))
        fi
    fi
    return 0
}

resolve_menu_cli_candidate() {
    local goos="$1" selected_override remaining component more
    MENU_CLI_CANONICAL=""
    MENU_CLI_CANDIDATES=()
    MENU_CLI_CANDIDATE_COUNT=0
    MENU_CLI_UNAVAILABLE_REASON=""

    # Match dev-tailscale-cli.sh's override precedence. A nonempty invalid
    # override fails closed instead of falling through to PATH or the other var.
    selected_override="${HERDR_DEV_TAILSCALE_CLI_BIN:-${HERDR_TAILSCALE_CLI_BIN:-}}"
    if [ -n "$selected_override" ]; then
        case "$selected_override" in
            /*) ;;
            *)
                MENU_CLI_UNAVAILABLE_REASON="The selected Tailscale CLI override must be an absolute executable regular file."
                return 1
                ;;
        esac
        if ! canonical_menu_cli_candidate "$selected_override"; then
            MENU_CLI_UNAVAILABLE_REASON="The selected Tailscale CLI override must be an absolute executable regular file."
            return 1
        fi
        MENU_CLI_CANDIDATES=("$MENU_CLI_CANONICAL")
        MENU_CLI_CANDIDATE_COUNT=1
        return 0
    fi

    remaining="${PATH:-}"
    while :; do
        case "$remaining" in
            *:*) component="${remaining%%:*}"; remaining="${remaining#*:}"; more=true ;;
            *) component="$remaining"; more=false ;;
        esac
        # filepath.SplitList candidates are accepted only from absolute PATH
        # directories; empty and relative entries do not resolve a binary.
        case "$component" in /*) append_menu_cli_candidate "$component/tailscale" "$goos" ;; esac
        [ "$more" = true ] || break
    done
    if [ "$goos" = Darwin ]; then
        append_menu_cli_candidate "$MENU_APP_STORE_CLI" "$goos"
    fi
    case "$MENU_CLI_CANDIDATE_COUNT" in
        0)
            MENU_CLI_UNAVAILABLE_REASON="No executable Tailscale CLI candidate was found in absolute PATH entries or the Darwin App Store fallback."
            return 1
            ;;
        1)
            MENU_CLI_CANONICAL="${MENU_CLI_CANDIDATES[0]}"
            return 0
            ;;
        *)
            MENU_CLI_UNAVAILABLE_REASON="Multiple distinct executable CLI candidates were found; set an absolute HERDR_DEV_TAILSCALE_CLI_BIN or HERDR_TAILSCALE_CLI_BIN override."
            return 1
            ;;
    esac
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
    option2_unavailable_reason=""
    option3_unavailable_reason=""
    menu_os="$(uname -s 2>/dev/null || true)"
    menu_arch="$(uname -m 2>/dev/null || true)"
    case "$menu_os" in
        Darwin|Linux) ;;
        *)
            option2_unavailable_reason="Tailscale development is supported only on Linux and macOS."
            option3_unavailable_reason="$option2_unavailable_reason"
            ;;
    esac
    if [ -z "$option2_unavailable_reason" ]; then
        if [ "$menu_os" = Darwin ] && tailscale_app_store_installed; then
            option3_unavailable_reason="App Store Tailscale was detected; legacy mode requires a standalone tailscaled."
        fi
        if [ "$menu_os" != Darwin ] || [ "$menu_arch" != arm64 ]; then
            option2_unavailable_reason="CLI-backed development requires macOS/arm64 and the supported App Store Tailscale 1.102.4 profile."
        elif [ -z "$TAILSCALE_APP_STORE_BUNDLE" ]; then
            option2_unavailable_reason="No App Store Tailscale bundle was detected; CLI-backed development requires the supported App Store Tailscale 1.102.4 profile."
        else
            app_store_version="$(tailscale_app_store_version "$TAILSCALE_APP_STORE_BUNDLE" || true)"
            if [ "$app_store_version" != "1.102.4" ]; then
                if [ -n "$app_store_version" ]; then
                    option2_unavailable_reason="Only the App Store Tailscale 1.102.4 profile is enabled for CLI-backed development; detected version $app_store_version."
                else
                    option2_unavailable_reason="Could not read the App Store bundle version; CLI-backed development requires the supported 1.102.4 profile."
                fi
            fi
        fi
        if [ -z "$option2_unavailable_reason" ] && ! resolve_menu_cli_candidate "$menu_os"; then
            option2_unavailable_reason="$MENU_CLI_UNAVAILABLE_REASON"
        fi
    fi
    # One Tailscale entry: use the App Store CLI mode when its profile checks
    # out; otherwise the standalone-tailscaled mode, unless the App Store app
    # is present (which that mode cannot use). HERDR_DEV_TRANSPORT still
    # selects either mode explicitly.
    tailscale_mode=""
    tailscale_unavailable_reason=""
    if [ -z "$option2_unavailable_reason" ]; then
        tailscale_mode=cli
        tailscale_description="Tailscale Serve — private tailnet HTTPS + QR via the Tailscale app's CLI; route stays published after stop."
    elif [ -z "$option3_unavailable_reason" ]; then
        tailscale_mode=legacy
        tailscale_description="Tailscale Serve — private tailnet HTTPS + QR via standalone Tailscale; route ends when the relay stops."
    else
        tailscale_unavailable_reason="$option2_unavailable_reason"
        tailscale_description="Tailscale Serve — private tailnet HTTPS + QR."
    fi
    echo "Development transport:"
    echo "  1. Cloudflare tunnel — temporary public URL + QR; no Tailscale needed (or use a saved gateway)."
    if [ -n "$tailscale_unavailable_reason" ]; then
        echo "  2. $tailscale_description [UNAVAILABLE: $tailscale_unavailable_reason]"
    else
        echo "  2. $tailscale_description"
    fi
    read -r -p "Choice [1]: " choice || { echo "Cancelled; nothing was started." >&2; exit 2; }
    case "$choice" in
        ''|1) ;;
        2)
            case "$tailscale_mode" in
                cli) exec "$SCRIPT_DIR/dev-tailscale-cli.sh" "$@" ;;
                legacy) HERDR_DEV_TAILSCALE_ENABLE=1 exec "$SCRIPT_DIR/dev-tailscale.sh" "$@" ;;
            esac
            echo "✗ Tailscale Serve unavailable: $tailscale_unavailable_reason" >&2
            exit 2
            ;;
        *) echo "✗ Choose 1 or 2." >&2; exit 2 ;;
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

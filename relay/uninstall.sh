#!/bin/bash
# Full uninstall: remove the relay binary, releases, config, cache, and service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELEASE_ROOT="$(relay_release_root)"
BIN_LINK="${HERDR_RELAY_BIN_DIR:-$HOME/.local/bin}/herdr-mobile-relay"

# Resolve config/state directory from the environment the service actually uses.
resolve_config_dir() {
    local env_file=""
    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        env_file="$HERDR_RELAY_ENV"
    elif [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        printf '%s\n' "$HERDR_PLUGIN_CONFIG_DIR"
        return
    else
        env_file="$(installed_service_env_file)"
    fi
    if [ -n "$env_file" ] && [ -f "$env_file" ]; then
        printf '%s\n' "$(dirname "$env_file")"
        return
    fi
    printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay"
}

resolve_cache_dir() {
    printf '%s\n' "${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay"
}

CONFIG_DIR="$(resolve_config_dir)"
CACHE_DIR="$(resolve_cache_dir)"

# Canonicalize a path, resolving symlinks. Returns empty if path does not exist.
canonicalize() {
    local target="$1"
    if [ -L "$target" ]; then
        readlink -f "$target" 2>/dev/null || realpath "$target" 2>/dev/null || printf '%s\n' "$target"
    elif [ -d "$target" ]; then
        (cd "$target" && pwd -P)
    elif [ -e "$target" ]; then
        local dir base
        dir="$(dirname "$target")"
        base="$(basename "$target")"
        printf '%s/%s\n' "$(cd "$dir" && pwd -P)" "$base"
    else
        printf '%s\n' "$target"
    fi
}

# Verify that the exact resolved target stays under HOME and contains relay
# markers. This supports explicit HERDR_RELEASE_ROOT and XDG paths without
# allowing an arbitrary directory to be removed.
verify_removal_target() {
    local canonical="$1" label="$2"
    local canonical_home expected=""

    canonical_home="$(canonicalize "$HOME")"
    case "$label" in
        releases) expected="$(canonicalize "$RELEASE_ROOT")" ;;
        config/state) expected="$(canonicalize "$CONFIG_DIR")" ;;
        cache) expected="$(canonicalize "$CACHE_DIR")" ;;
    esac
    case "$canonical" in
        "$canonical_home"/*) ;;
        *)
            echo "  REFUSING to remove $label outside HOME: $canonical" >&2
            return 1
            ;;
    esac
    if [ -z "$expected" ] || [ "$canonical" != "$expected" ]; then
        echo "  REFUSING to remove $label: $canonical" >&2
        echo "  Path does not match the resolved herdr-mobile-relay $label root." >&2
        return 1
    fi

    # Require a dedicated sentinel whose recorded canonical root matches the
    # deletion target. Generic relay-looking filenames never authorize removal.
    if [ -d "$canonical" ]; then
        sentinel="$canonical/.herdr-mobile-relay-installation"
        if [ ! -f "$sentinel" ] ||
           ! grep -Fx 'product=herdr-mobile-relay' "$sentinel" >/dev/null ||
           ! grep -Fx "root=$canonical" "$sentinel" >/dev/null; then
            echo "  REFUSING to remove $label: $canonical" >&2
            echo "  Directory has no matching herdr-mobile-relay installation sentinel." >&2
            return 1
        fi
    fi

    return 0
}

preflight_removal_target() {
    local target="$1" label="$2"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return
    fi
    # A symlink is removed as a link only; its destination is never traversed.
    if [ -L "$target" ]; then
        return
    fi
    verify_removal_target "$(canonicalize "$target")" "$label"
}

safe_remove_dir() {
    local target="$1" label="$2"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return 0
    fi
    # If target is a symlink, remove only the link, never the resolved target.
    if [ -L "$target" ]; then
        rm -f "$target"
        echo "  Removed $label symlink: $target"
        return 0
    fi
    local canonical
    canonical="$(canonicalize "$target")"
    if ! verify_removal_target "$canonical" "$label"; then
        return 1
    fi
    rm -rf "$canonical"
    echo "  Removed $label: $canonical"
}

safe_remove_bin_link() {
    local target="$1"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return 0
    fi
    # Only remove if it is a symlink pointing into our release root, or a
    # regular file that is our binary (verified by name match).
    if [ -L "$target" ]; then
        local link_target
        link_target="$(readlink -f "$target" 2>/dev/null || readlink "$target")"
        local canonical_release
        canonical_release="$(canonicalize "$RELEASE_ROOT")"
        case "$link_target" in
            "$canonical_release/"*)
                rm -f "$target"
                echo "  Removed binary symlink: $target -> $link_target"
                return 0
                ;;
        esac
        echo "  Skipping binary link (symlink target not in release root): $target -> $link_target" >&2
        return 0
    fi
    # A regular shim must be byte-identical to the active verified release.
    # Its filename and executable bit alone are not ownership proof.
    local active_binary="$RELEASE_ROOT/current/herdr-mobile-relay"
    if [ -f "$active_binary" ] && [ ! -L "$active_binary" ] &&
       command -v cmp >/dev/null 2>&1 && cmp -s "$target" "$active_binary"; then
        rm -f "$target"
        echo "  Removed binary: $target"
    else
        echo "  Skipping binary (not identical to the active owned release): $target" >&2
    fi
}

# Validate every directory before stopping the service. A bad custom/XDG path
# must fail without disrupting a healthy installation.
preflight_removal_target "$RELEASE_ROOT" "releases"
preflight_removal_target "$CONFIG_DIR" "config/state"
preflight_removal_target "$CACHE_DIR" "cache"

echo "Herdr Mobile Relay — full uninstall"
echo ""
echo "This will remove:"
echo "  Service:      herdr-mobile-relay.service (systemd/launchd)"
echo "  Releases:     $RELEASE_ROOT"
echo "  Binary link:  $BIN_LINK"
echo "  Config/state: $CONFIG_DIR"
echo "  Cache:        $CACHE_DIR"
echo ""
read -r -p "Continue? [y/N] " choice
case "$choice" in
    y|Y|yes|YES) ;;
    *) echo "Cancelled."; exit 0 ;;
esac

echo ""

# Stop and remove the service — must succeed before deleting files.
service_stopped=false
case "$(uname -s)" in
    Darwin)
        if [ -f "$SCRIPT_DIR/uninstall-service.sh" ]; then
            if sh "$SCRIPT_DIR/uninstall-service.sh"; then
                service_stopped=true
            fi
        else
            service_stopped=true
        fi
        ;;
    Linux)
        if [ -f "$SCRIPT_DIR/uninstall-systemd-user-service.sh" ]; then
            if bash "$SCRIPT_DIR/uninstall-systemd-user-service.sh"; then
                service_stopped=true
            fi
        else
            service_stopped=true
        fi
        ;;
    *)
        service_stopped=true
        ;;
esac

if [ "$service_stopped" != "true" ]; then
    echo "" >&2
    echo "ERROR: Service uninstall failed. The relay may still be running." >&2
    echo "Stop it manually before removing files:" >&2
    echo "  systemctl --user stop herdr-mobile-relay.service" >&2
    echo "  (or launchctl unload ~/Library/LaunchAgents/com.herdr-mobile-relay.service.plist)" >&2
    echo "" >&2
    read -r -p "Continue with file removal anyway? [y/N] " force_choice
    case "$force_choice" in
        y|Y|yes|YES) ;;
        *) echo "Aborted. No files were removed."; exit 1 ;;
    esac
fi

# Verify the service is actually stopped before removing its files.
case "$(uname -s)" in
    Linux)
        if command -v systemctl >/dev/null 2>&1; then
            for svc in herdr-mobile-relay.service herdr-remote.service; do
                if systemctl --user is-active --quiet "$svc" 2>/dev/null; then
                    echo "" >&2
                    echo "ERROR: $svc is still running." >&2
                    echo "Stop it first: systemctl --user stop $svc" >&2
                    exit 1
                fi
            done
        fi
        ;;
    Darwin)
        for label in com.herdr-mobile-relay com.herdr-remote; do
            if launchctl list 2>/dev/null | grep -q "$label"; then
                echo "" >&2
                echo "ERROR: $label is still loaded." >&2
                echo "Unload it first: launchctl bootout gui/$(id -u)/$label" >&2
                exit 1
            fi
        done
        ;;
esac

# Remove the binary symlink (only if it points into our release root)
safe_remove_bin_link "$BIN_LINK"

# Remove all releases
safe_remove_dir "$RELEASE_ROOT" "releases"

# Remove config/state (push subscriptions, VAPID keys, tokens, update state)
safe_remove_dir "$CONFIG_DIR" "config/state"

# Remove cache and history
safe_remove_dir "$CACHE_DIR" "cache"

echo ""
echo "Herdr Mobile Relay has been uninstalled."
if command -v herdr >/dev/null 2>&1; then
    echo "Removing Herdr plugin registration..."
    if ! herdr plugin uninstall herdr-mobile-relay.events; then
        echo "Plugin registration remains; remove it with:" >&2
        echo "  herdr plugin uninstall herdr-mobile-relay.events" >&2
        exit 1
    fi
else
    echo "Herdr is unavailable; remove plugin registration later with:"
    echo "  herdr plugin uninstall herdr-mobile-relay.events"
fi

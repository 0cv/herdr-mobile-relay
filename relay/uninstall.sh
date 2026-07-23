#!/bin/bash
# Full uninstall: remove the relay binary, releases, config, cache, and service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELEASE_ROOT="$(relay_release_root)"
BIN_LINK="$HOME/.local/bin/herdr-mobile-relay"

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

# Verify a path is an expected root before removal. Refuses anything that
# does not exactly match one of the known installation directories.
verify_removal_target() {
    local canonical="$1" label="$2"

    # Expected paths are derived from well-known defaults, NOT from the
    # environment-controlled values being validated.
    local default_release="$HOME/.local/share/herdr-mobile-relay"
    local default_config="${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay"
    local default_cache="${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay"
    local herdr_plugin_config="${XDG_CONFIG_HOME:-$HOME/.config}/herdr/plugins/config/herdr-mobile-relay.events"

    local match=false
    case "$canonical" in
        "$default_release"|"$default_config"|"$default_cache"|"$herdr_plugin_config")
            match=true
            ;;
    esac

    # Also accept XDG_DATA_HOME-based release root if it is under $HOME.
    if [ "$match" != "true" ] && [ -n "${XDG_DATA_HOME:-}" ]; then
        case "$XDG_DATA_HOME" in
            "$HOME"/*)
                local xdg_release="$XDG_DATA_HOME/herdr-mobile-relay"
                local canonical_xdg
                canonical_xdg="$(canonicalize "$xdg_release")"
                if [ "$canonical" = "$canonical_xdg" ]; then
                    match=true
                fi
                ;;
        esac
    fi

    if [ "$match" != "true" ]; then
        echo "  REFUSING to remove $label: $canonical" >&2
        echo "  Path does not match any known herdr-mobile-relay installation root." >&2
        return 1
    fi

    # Require an installation marker to confirm this is actually a relay directory.
    if [ -d "$canonical" ]; then
        if [ ! -f "$canonical/release-manifest.json" ] && \
           [ ! -d "$canonical/releases" ] && \
           [ ! -f "$canonical/subscriptions.json" ] && \
           [ ! -d "$canonical/push" ] && \
           [ ! -f "$canonical/relay.env" ] && \
           [ ! -f "$canonical/herdr-mobile-relay.env" ]; then
            echo "  REFUSING to remove $label: $canonical" >&2
            echo "  Directory exists but contains no herdr-mobile-relay installation marker." >&2
            return 1
        fi
    fi

    return 0
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
    # Regular file: only remove if it matches the expected binary name and is executable
    local base
    base="$(basename "$target")"
    if [ "$base" = "herdr-mobile-relay" ] && [ -x "$target" ]; then
        rm -f "$target"
        echo "  Removed binary: $target"
    else
        echo "  Skipping binary (not a herdr-mobile-relay executable): $target" >&2
    fi
}

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
echo "Note: plugin registration in herdr-plugin.toml must be removed manually"
echo "      (herdr plugin uninstall herdr-mobile-relay.events)."

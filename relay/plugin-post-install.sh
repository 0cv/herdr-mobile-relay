#!/bin/sh
# Detached waiter started by plugin-build.sh. It opens setup only after Herdr
# has rewritten its registry with this manifest version and setup action.
set -eu

PLUGIN_ID="herdr-mobile-relay.events"
EXPECTED_VERSION="${1:-}"
BUILD_PID="${2:-0}"
REGISTRY_FILE="${HERDR_PLUGIN_REGISTRY:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/plugins.json}"
ATTEMPTS="${HERDR_POST_INSTALL_ATTEMPTS:-30}"
DELAY="${HERDR_POST_INSTALL_DELAY:-1}"
HERDR_COMMAND="${HERDR_BIN_PATH:-$(command -v herdr || true)}"
SOCKET_PATH="${HERDR_SOCKET_PATH:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/herdr.sock}"

if [ -z "$EXPECTED_VERSION" ]; then
    exit 0
fi

if [ -n "${HERDR_POST_INSTALL_LOCK_DIR:-}" ]; then
    LOCK_DIR="$HERDR_POST_INSTALL_LOCK_DIR"
else
    USER_ID="$(id -u 2>/dev/null || echo user)"
    LOCK_DIR="${TMPDIR:-/tmp}/herdr-mobile-relay-post-install-$USER_ID"
fi

acquire_lock() {
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        return 0
    fi
    if [ -r "$LOCK_DIR/pid" ]; then
        OLD_PID="$(sed -n '1p' "$LOCK_DIR/pid" 2>/dev/null || true)"
        if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
            return 1
        fi
    else
        # Another waiter may be between mkdir and writing its pid. Treat an
        # uninitialised lock as live rather than deleting it and opening twice.
        return 1
    fi
    rm -rf "$LOCK_DIR"
    mkdir "$LOCK_DIR" 2>/dev/null
}

acquire_lock || exit 0
printf '%s\n' "$$" > "$LOCK_DIR/pid"
trap 'rm -rf "$LOCK_DIR"' EXIT HUP INT TERM

registered_plugin_root() {
    PYTHON=""
    if command -v python3 >/dev/null 2>&1; then
        PYTHON="$(command -v python3)"
    elif command -v uv >/dev/null 2>&1; then
        PYTHON="$(uv python find 2>/dev/null || true)"
    elif [ -x "$HOME/.local/bin/uv" ]; then
        PYTHON="$("$HOME/.local/bin/uv" python find 2>/dev/null || true)"
    fi
    if [ -z "$PYTHON" ] || [ ! -x "$PYTHON" ]; then
        return 1
    fi

    "$PYTHON" - "$REGISTRY_FILE" "$PLUGIN_ID" "$EXPECTED_VERSION" <<'PY'
import json
import os
import sys

registry, plugin_id, expected_version = sys.argv[1:]
try:
    with open(registry, encoding="utf-8") as handle:
        plugins = json.load(handle)
except (OSError, ValueError):
    raise SystemExit(1)

for plugin in plugins if isinstance(plugins, list) else []:
    actions = plugin.get("actions") if isinstance(plugin, dict) else []
    if (
        plugin.get("plugin_id") == plugin_id
        and plugin.get("version") == expected_version
        and plugin.get("enabled") is not False
        and any(action.get("id") == "setup" for action in actions or [] if isinstance(action, dict))
        and plugin.get("plugin_root")
        and os.path.isdir(plugin["plugin_root"])
    ):
        print(plugin["plugin_root"])
        raise SystemExit(0)
raise SystemExit(1)
PY
}

# Build must exit before Herdr can write the new registry entry.
COUNT=0
while [ "$BUILD_PID" -gt 0 ] 2>/dev/null && kill -0 "$BUILD_PID" 2>/dev/null && [ "$COUNT" -lt 10 ]; do
    sleep "$DELAY"
    COUNT=$((COUNT + 1))
done

COUNT=0
PLUGIN_ROOT=""
while [ "$COUNT" -lt "$ATTEMPTS" ]; do
    PLUGIN_ROOT="$(registered_plugin_root 2>/dev/null || true)"
    if [ -n "$PLUGIN_ROOT" ]; then
        break
    fi
    sleep "$DELAY"
    COUNT=$((COUNT + 1))
done

if [ -z "$PLUGIN_ROOT" ]; then
    exit 0
fi

if [ -S "$SOCKET_PATH" ] && [ -n "$HERDR_COMMAND" ]; then
    export HERDR_SOCKET_PATH="$SOCKET_PATH"
    COUNT=0
    while [ "$COUNT" -lt 10 ]; do
        if [ -n "${HERDR_PANE_ID:-}" ]; then
            if "$HERDR_COMMAND" plugin pane open \
                --plugin "$PLUGIN_ID" \
                --entrypoint setup \
                --placement zoomed \
                --focus \
                --target-pane "$HERDR_PANE_ID"; then
                exit 0
            fi
        elif "$HERDR_COMMAND" plugin pane open \
            --plugin "$PLUGIN_ID" \
            --entrypoint setup \
            --placement overlay \
            --focus; then
            exit 0
        fi
        sleep "$DELAY"
        COUNT=$((COUNT + 1))
    done
fi

"$PLUGIN_ROOT/relay/plugin-open-terminal.sh" "$PLUGIN_ROOT" || true

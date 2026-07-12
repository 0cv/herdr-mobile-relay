#!/bin/sh
# Build step for `herdr plugin install`. herdr previews this command and asks
# for confirmation before running it — once, in the plugin root, with no
# runtime context and no guaranteed toolchain.
#
# Mirrors the herdr-plus pattern: prefer what is present, fetch what is
# missing, never block plugin registration. The relay itself needs no
# compilation — uv resolves its Python dependencies at first run from the
# inline metadata in relay/herdr_relay.py. This step makes sure uv exists
# (official standalone installer, user-level, same invocation setup.sh uses)
# and syncs that exact environment so the first Quick Start does not pause on
# downloads. cloudflared stays a Quick Start decision: it is optional (the
# relay runs locally without it) and it opens a public tunnel, so it deserves
# its own interactive yes.
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
SCRIPT_DIR=$(CDPATH='' cd "$SCRIPT_DIR" && pwd)
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
PATH="$HOME/.local/bin:$PATH"
export PATH

schedule_auto_setup() {
    if [ "${HERDR_MOBILE_RELAY_NO_AUTO_SETUP:-0}" = "1" ]; then
        return
    fi
    SOCKET_PATH="${HERDR_SOCKET_PATH:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/herdr.sock}"
    if [ ! -S "$SOCKET_PATH" ]; then
        if ! "$SCRIPT_DIR/plugin-open-terminal.sh" "$REPO_DIR" --can-launch; then
            return
        fi
    fi
    if ! command -v nohup >/dev/null 2>&1; then
        return
    fi

    EXPECTED_VERSION="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml")"
    if [ -z "$EXPECTED_VERSION" ]; then
        return
    fi
    LOG_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay"
    mkdir -p "$LOG_DIR"
    chmod 700 "$LOG_DIR"
    HERDR_SOCKET_PATH="$SOCKET_PATH" nohup sh "$SCRIPT_DIR/plugin-post-install.sh" "$EXPECTED_VERSION" "$$" \
        </dev/null >"$LOG_DIR/post-install.log" 2>&1 &
    echo "herdr-mobile-relay: setup will open automatically after registration." >&2
}

PREWARM_READY=1

if ! command -v uv >/dev/null 2>&1; then
    echo "herdr-mobile-relay: installing uv (user-level, official standalone installer)..." >&2
    if ! curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="$HOME/.local/bin" UV_NO_MODIFY_PATH=1 sh; then
        echo "herdr-mobile-relay: uv installation failed - continuing; Quick Start offers interactive installation." >&2
        PREWARM_READY=0
    fi
fi

if ! command -v uv >/dev/null 2>&1; then
    echo "herdr-mobile-relay: uv unavailable - skipping the pre-warm; Quick Start offers interactive installation." >&2
    PREWARM_READY=0
elif [ "$PREWARM_READY" -eq 1 ]; then
    echo "herdr-mobile-relay: pre-warming the relay's Python environment..." >&2
    if ! uv sync --quiet --script "$SCRIPT_DIR/herdr_relay.py"; then
        echo "herdr-mobile-relay: dependency pre-warm failed - continuing; Quick Start will retry." >&2
        PREWARM_READY=0
    fi
fi

echo "" >&2
if [ "$PREWARM_READY" -eq 1 ]; then
    echo "herdr-mobile-relay: dependencies are ready." >&2
fi
schedule_auto_setup
echo "herdr-mobile-relay: if setup does not open automatically, run:" >&2
echo "  herdr plugin action invoke setup --plugin herdr-mobile-relay.events" >&2

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
PATH="$HOME/.local/bin:$PATH"
export PATH

if ! command -v uv >/dev/null 2>&1; then
    echo "herdr-mobile-relay: installing uv (user-level, official standalone installer)..." >&2
    if ! curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="$HOME/.local/bin" UV_NO_MODIFY_PATH=1 sh; then
        echo "herdr-mobile-relay: uv installation failed - continuing; Quick Start offers interactive installation." >&2
        exit 0
    fi
fi

if ! command -v uv >/dev/null 2>&1; then
    echo "herdr-mobile-relay: uv unavailable - skipping the pre-warm; Quick Start offers interactive installation." >&2
    exit 0
fi

echo "herdr-mobile-relay: pre-warming the relay's Python environment..." >&2
if ! uv sync --quiet --script "$SCRIPT_DIR/herdr_relay.py"; then
    echo "herdr-mobile-relay: dependency pre-warm failed - continuing; Quick Start will retry." >&2
    exit 0
fi

# Installing must stay inert (no tunnel, no server without an explicit ask),
# so point at the next step instead of taking it.
echo "" >&2
echo "herdr-mobile-relay: ready. Start it with:" >&2
echo "  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events" >&2

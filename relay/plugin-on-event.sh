#!/bin/sh
# Herdr event hook: a bounded local UDP send from the packaged Go helper.
set -eu

RELEASE_ROOT=${HERDR_RELEASE_ROOT:-"${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay"}
RELAY_BIN=${HERDR_RELAY_BIN:-"$RELEASE_ROOT/current/herdr-mobile-relay"}
if [ ! -x "$RELAY_BIN" ]; then
    echo "herdr-mobile-relay: verified relay release is unavailable: $RELAY_BIN" >&2
    exit 1
fi
exec "$RELAY_BIN" event-hook

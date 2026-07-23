#!/bin/sh
# Compatibility entrypoint retained for checkout users.
set -eu
SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
exec sh "$REPO_DIR/relay/plugin-build.sh"

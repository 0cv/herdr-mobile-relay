#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-uninstall-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

TEST_HOME="$WORK_DIR/home"
RELEASE_ROOT="$TEST_HOME/custom/releases-root"
CONFIG_HOME="$TEST_HOME/custom/config"
CACHE_HOME="$TEST_HOME/custom/cache"
mkdir -p "$RELEASE_ROOT/releases" \
    "$CONFIG_HOME/herdr-mobile-relay" \
    "$CACHE_HOME/herdr-mobile-relay/claude-history"
touch "$CONFIG_HOME/herdr-mobile-relay/relay.env"

output="$(
    printf 'n\n' |
        HOME="$TEST_HOME" \
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
        XDG_CONFIG_HOME="$CONFIG_HOME" \
        XDG_CACHE_HOME="$CACHE_HOME" \
        bash "$REPO_DIR/relay/uninstall.sh"
)"
grep -F "Cancelled." <<<"$output" >/dev/null
test -d "$RELEASE_ROOT"

outside="$WORK_DIR/outside"
mkdir -p "$outside/releases"
if printf 'n\n' |
    HOME="$TEST_HOME" \
    HERDR_RELEASE_ROOT="$outside" \
    XDG_CONFIG_HOME="$CONFIG_HOME" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    bash "$REPO_DIR/relay/uninstall.sh" >/dev/null 2>&1; then
    echo "uninstall accepted a release root outside HOME" >&2
    exit 1
fi

echo "uninstall shell tests passed"

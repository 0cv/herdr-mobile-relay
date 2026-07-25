#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-uninstall-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT
SCRIPT_DIR="$WORK_DIR/relay"
mkdir -p "$SCRIPT_DIR"
cp "$REPO_DIR/relay/uninstall.sh" "$REPO_DIR/relay/common.sh" "$SCRIPT_DIR/"
FAKE_BIN="$WORK_DIR/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
exit 1
EOF
cat > "$FAKE_BIN/herdr" <<'EOF'
#!/bin/sh
test "$*" = "plugin uninstall herdr-mobile-relay.events"
EOF
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/herdr"
export PATH="$FAKE_BIN:$PATH"

TEST_HOME="$WORK_DIR/home"
RELEASE_ROOT="$TEST_HOME/custom/releases-root"
CONFIG_HOME="$TEST_HOME/custom/config"
CACHE_HOME="$TEST_HOME/custom/cache"
mkdir -p "$RELEASE_ROOT/releases" \
    "$CONFIG_HOME/herdr-mobile-relay" \
    "$CACHE_HOME/herdr-mobile-relay/claude-history"
touch "$CONFIG_HOME/herdr-mobile-relay/relay.env"
for target in "$RELEASE_ROOT" "$CONFIG_HOME/herdr-mobile-relay" "$CACHE_HOME/herdr-mobile-relay"; do
    canonical="$(cd "$target" && pwd -P)"
    printf 'product=herdr-mobile-relay\nroot=%s\n' "$canonical" > "$target/.herdr-mobile-relay-installation"
done
mkdir -p "$RELEASE_ROOT/releases/sealed/web"
printf 'sealed release\n' > "$RELEASE_ROOT/releases/sealed/web/index.html"
chmod a-w "$RELEASE_ROOT/releases/sealed" "$RELEASE_ROOT/releases/sealed/web"

output="$(
    printf 'n\n' |
        HOME="$TEST_HOME" \
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
        XDG_CONFIG_HOME="$CONFIG_HOME" \
        XDG_CACHE_HOME="$CACHE_HOME" \
        bash "$SCRIPT_DIR/uninstall.sh"
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
    bash "$SCRIPT_DIR/uninstall.sh" >/dev/null 2>&1; then
    echo "uninstall accepted a release root outside HOME" >&2
    exit 1
fi

wrong="$TEST_HOME/unrelated"
mkdir -p "$wrong/releases"
touch "$wrong/relay.env"
if printf 'n\n' |
    HOME="$TEST_HOME" \
    HERDR_RELEASE_ROOT="$wrong" \
    XDG_CONFIG_HOME="$CONFIG_HOME" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    bash "$SCRIPT_DIR/uninstall.sh" >/dev/null 2>&1; then
    echo "uninstall accepted generic markers in an unrelated in-home directory" >&2
    exit 1
fi

SOURCE_CHECKOUT="$TEST_HOME/source-checkout"
mkdir -p "$SOURCE_CHECKOUT/relay"
printf "HERDR_RELAY_TOKEN='source-token'\n" > "$SOURCE_CHECKOUT/relay/.env"
printf 'source file\n' > "$SOURCE_CHECKOUT/relay/keep.txt"

printf 'y\n' |
    HOME="$TEST_HOME" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$CONFIG_HOME/herdr-mobile-relay" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    bash "$SCRIPT_DIR/uninstall.sh" >/dev/null
test ! -e "$RELEASE_ROOT"
test ! -e "$CONFIG_HOME/herdr-mobile-relay"
test ! -e "$CACHE_HOME/herdr-mobile-relay"
test -f "$SOURCE_CHECKOUT/relay/.env"
test -f "$SOURCE_CHECKOUT/relay/keep.txt"

echo "uninstall shell tests passed"

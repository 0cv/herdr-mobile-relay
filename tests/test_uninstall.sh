#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-uninstall-test.XXXXXX")"
WORK_DIR="$(cd "$WORK_DIR" && pwd -P)"
cleanup() {
    chmod -R u+w "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT
SCRIPT_DIR="$WORK_DIR/relay"
mkdir -p "$SCRIPT_DIR"
cp "$REPO_DIR/relay/uninstall.sh" "$REPO_DIR/relay/common.sh" "$SCRIPT_DIR/"
FAKE_BIN="$WORK_DIR/bin"
mkdir -p "$FAKE_BIN"
REAL_STAT=$(command -v stat)
REAL_CHMOD=$(command -v chmod)
export REAL_STAT REAL_CHMOD
cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
exit 1
EOF
cat > "$FAKE_BIN/herdr" <<'EOF'
#!/bin/sh
test "$*" = "plugin uninstall herdr-mobile-relay.events"
EOF
cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
printf 'Linux\n'
EOF
cat > "$FAKE_BIN/stat" <<'EOF'
#!/bin/sh
case "${HERDR_TEST_STAT_MODE:-}" in
    fallback)
        [ "$1" != -f ] || exit 1
        printf 'regular file:600:1:%s\n' "$(id -u)"
        ;;
    failure) exit 1 ;;
    *) exec "$REAL_STAT" "$@" ;;
esac
EOF
cat > "$FAKE_BIN/chmod" <<'EOF'
#!/bin/sh
if [ -n "${HERDR_TEST_CHMOD_RACE_ROOT:-}" ] && [ "${1:-}" = -R ] && [ "${3:-}" = "$HERDR_TEST_CHMOD_RACE_ROOT" ]; then
    "$REAL_CHMOD" "$@"
    sentinel="$HERDR_TEST_CHMOD_RACE_ROOT/.herdr-mobile-relay-installation"
    mv "$sentinel" "$HERDR_TEST_CHMOD_RACE_ROOT/sentinel-target"
    ln -s sentinel-target "$sentinel"
    exit 0
fi
exec "$REAL_CHMOD" "$@"
EOF
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/herdr" "$FAKE_BIN/uname" "$FAKE_BIN/stat" "$FAKE_BIN/chmod"
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
    chmod 600 "$target/.herdr-mobile-relay-installation"
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

output="$(
    printf 'n\n' |
        HOME="$TEST_HOME" \
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
        XDG_CONFIG_HOME="$CONFIG_HOME" \
        XDG_CACHE_HOME="$CACHE_HOME" \
        HERDR_TEST_STAT_MODE=fallback \
        bash "$SCRIPT_DIR/uninstall.sh"
)"
grep -F "Cancelled." <<<"$output" >/dev/null
if printf 'n\n' |
    HOME="$TEST_HOME" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    XDG_CONFIG_HOME="$CONFIG_HOME" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    HERDR_TEST_STAT_MODE=failure \
    bash "$SCRIPT_DIR/uninstall.sh" >/dev/null 2>&1; then
    echo "uninstall accepted an ownership sentinel whose metadata could not be read" >&2
    exit 1
fi

for unsafe_kind in symlink hardlink public extra; do
    case "$unsafe_kind" in
        symlink)
            unsafe_root="$CONFIG_HOME/herdr-mobile-relay"
            mv "$unsafe_root/.herdr-mobile-relay-installation" "$unsafe_root/sentinel-target"
            ln -s sentinel-target "$unsafe_root/.herdr-mobile-relay-installation"
            ;;
        hardlink)
            unsafe_root="$CACHE_HOME/herdr-mobile-relay"
            ln "$unsafe_root/.herdr-mobile-relay-installation" "$unsafe_root/sentinel-hardlink"
            ;;
        public)
            unsafe_root="$RELEASE_ROOT"
            chmod 644 "$unsafe_root/.herdr-mobile-relay-installation"
            ;;
        extra)
            unsafe_root="$RELEASE_ROOT"
            printf 'unexpected=true\n' >> "$unsafe_root/.herdr-mobile-relay-installation"
            ;;
    esac
    if printf 'n\n' |
        HOME="$TEST_HOME" \
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
        XDG_CONFIG_HOME="$CONFIG_HOME" \
        XDG_CACHE_HOME="$CACHE_HOME" \
        bash "$SCRIPT_DIR/uninstall.sh" >/dev/null 2>&1; then
        echo "uninstall accepted an unsafe $unsafe_kind ownership sentinel" >&2
        exit 1
    fi
    case "$unsafe_kind" in
        symlink)
            rm "$unsafe_root/.herdr-mobile-relay-installation"
            mv "$unsafe_root/sentinel-target" "$unsafe_root/.herdr-mobile-relay-installation"
            ;;
        hardlink) rm "$unsafe_root/sentinel-hardlink" ;;
        public) chmod 600 "$unsafe_root/.herdr-mobile-relay-installation" ;;
        extra)
            canonical="$(cd "$unsafe_root" && pwd -P)"
            printf 'product=herdr-mobile-relay\nroot=%s\n' "$canonical" > "$unsafe_root/.herdr-mobile-relay-installation"
            chmod 600 "$unsafe_root/.herdr-mobile-relay-installation"
            ;;
    esac
done

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

RACE_HOME="$WORK_DIR/race-home"
RACE_RELEASE_ROOT="$RACE_HOME/releases-root"
RACE_CONFIG="$RACE_HOME/config/herdr-mobile-relay"
RACE_CACHE_HOME="$RACE_HOME/cache"
RACE_CACHE="$RACE_CACHE_HOME/herdr-mobile-relay"
mkdir -p "$RACE_RELEASE_ROOT" "$RACE_CONFIG" "$RACE_CACHE"
for target in "$RACE_RELEASE_ROOT" "$RACE_CONFIG" "$RACE_CACHE"; do
    canonical="$(cd "$target" && pwd -P)"
    printf 'product=herdr-mobile-relay\nroot=%s\n' "$canonical" > "$target/.herdr-mobile-relay-installation"
    chmod 600 "$target/.herdr-mobile-relay-installation"
done
printf 'must survive\n' > "$RACE_RELEASE_ROOT/keep.txt"
cat > "$SCRIPT_DIR/uninstall-systemd-user-service.sh" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 700 "$SCRIPT_DIR/uninstall-systemd-user-service.sh"
if printf 'y\n' |
    HOME="$RACE_HOME" \
    HERDR_RELEASE_ROOT="$RACE_RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$RACE_CONFIG" \
    XDG_CACHE_HOME="$RACE_CACHE_HOME" \
    HERDR_TEST_CHMOD_RACE_ROOT="$RACE_RELEASE_ROOT" \
    bash "$SCRIPT_DIR/uninstall.sh" >/dev/null 2>&1; then
    echo "uninstall did not revalidate a removal sentinel after stopping the service" >&2
    exit 1
fi
test -f "$RACE_RELEASE_ROOT/keep.txt"
rm "$SCRIPT_DIR/uninstall-systemd-user-service.sh"

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

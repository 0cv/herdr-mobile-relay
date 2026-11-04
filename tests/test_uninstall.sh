#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-uninstall-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT
SCRIPT_DIR="$WORK_DIR/relay"
mkdir -p "$SCRIPT_DIR"
cp "$REPO_DIR/relay/uninstall.sh" "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/uninstall-systemd-user-service.sh" "$SCRIPT_DIR/"
FAKE_BIN="$WORK_DIR/bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
case "$*" in *is-active*) exit 1 ;; esac
exit 0
EOF
cat > "$FAKE_BIN/herdr" <<'EOF'
#!/bin/sh
test "$*" = "plugin uninstall herdr-mobile-relay.events"
EOF
cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
printf 'Linux\n'
EOF
cat > "$FAKE_BIN/relay" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
[ "${FAIL_OFF:-}" != 1 ]
EOF
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/herdr" "$FAKE_BIN/uname" "$FAKE_BIN/relay"
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

printf "HERDR_TAILSCALE_PORT='8443'\n" > "$CONFIG_HOME/herdr-mobile-relay/relay.env"
export FAKE_LOG="$WORK_DIR/off.log"
if ! output="$(printf 'y\n' |
    HOME="$TEST_HOME" HERDR_RELAY_BIN="$FAKE_BIN/relay" FAIL_OFF=1 \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$CONFIG_HOME/herdr-mobile-relay" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    bash "$SCRIPT_DIR/uninstall.sh" 2>&1)"; then
    echo "full uninstall stopped on failed off: $output" >&2; exit 1
fi
grep -q 'tailscale serve --https=8443 off' <<< "$output"
test "$(grep -c 'tailscale off --port 8443' "$FAKE_LOG")" -eq 1
test ! -e "$RELEASE_ROOT"
test ! -e "$CONFIG_HOME/herdr-mobile-relay"
test ! -e "$CACHE_HOME/herdr-mobile-relay"
test -f "$SOURCE_CHECKOUT/relay/.env"
test -f "$SOURCE_CHECKOUT/relay/keep.txt"

mkdir -p "$RELEASE_ROOT" "$CONFIG_HOME/herdr-mobile-relay" "$CACHE_HOME/herdr-mobile-relay"
for target in "$RELEASE_ROOT" "$CONFIG_HOME/herdr-mobile-relay" "$CACHE_HOME/herdr-mobile-relay"; do
    canonical="$(cd "$target" && pwd -P)"
    printf 'product=herdr-mobile-relay\nroot=%s\n' "$canonical" > "$target/.herdr-mobile-relay-installation"
    chmod 600 "$target/.herdr-mobile-relay-installation"
done
printf "HERDR_TAILSCALE_PORT='8443'\n" > "$CONFIG_HOME/herdr-mobile-relay/relay.env"
: > "$FAKE_LOG"
printf 'y\n' |
    HOME="$TEST_HOME" HERDR_RELAY_BIN="$FAKE_BIN/relay" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$CONFIG_HOME/herdr-mobile-relay" \
    XDG_CACHE_HOME="$CACHE_HOME" \
    bash "$SCRIPT_DIR/uninstall.sh" > /dev/null
test "$(grep -c 'tailscale off --port 8443' "$FAKE_LOG")" -eq 1
test ! -e "$CONFIG_HOME/herdr-mobile-relay"

for order in core-first tui-first; do
    export HOME="$WORK_DIR/$order" HERDR_RELEASE_ROOT="$WORK_DIR/$order/releases"
    export XDG_CONFIG_HOME="$HOME/.config" XDG_CACHE_HOME="$HOME/.cache"
    mkdir -p "$HERDR_RELEASE_ROOT" "$HOME/.config/systemd/user"
    for target in "$HERDR_RELEASE_ROOT" "$XDG_CONFIG_HOME/herdr-mobile-relay" "$XDG_CACHE_HOME/herdr-mobile-relay"; do
        mkdir -p "$target"
        printf 'product=herdr-mobile-relay\nroot=%s\n' "$(cd "$target" && pwd -P)" > "$target/.herdr-mobile-relay-installation"
        chmod 600 "$target/.herdr-mobile-relay-installation"
    done
    printf 'TUI env sentinel\n' > "$XDG_CONFIG_HOME/herdr-mobile-relay/keep"
    printf 'TUI unit sentinel\n' > "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    printf 'core unit\n' > "$HOME/.config/systemd/user/herdr-mobile-relay-hc.service"
    HERDR_RELAY_BACKEND=hc bash -c '. "$1"; ensure_relay_env "$HERDR_RELAY_ENV"' _ "$SCRIPT_DIR/common.sh"
    if [ "$order" = tui-first ]; then
        if printf 'y\n' | bash "$SCRIPT_DIR/uninstall.sh" > "$WORK_DIR/refused" 2>&1; then
            echo "TUI uninstall removed a shared release" >&2; exit 1
        fi
        grep -q 'shared release' "$WORK_DIR/refused"
        rm "$XDG_CONFIG_HOME/herdr-mobile-relay-hc/relay.env"
    fi
    printf 'y\n' | HERDR_RELAY_BACKEND=hc bash "$SCRIPT_DIR/uninstall.sh" > "$WORK_DIR/core-uninstall"
    test -d "$HERDR_RELEASE_ROOT"
    test ! -e "$XDG_CONFIG_HOME/herdr-mobile-relay-hc"
    test ! -e "$XDG_CACHE_HOME/herdr-mobile-relay-hc"
    grep -Fx 'TUI env sentinel' "$XDG_CONFIG_HOME/herdr-mobile-relay/keep"
    grep -Fx 'TUI unit sentinel' "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    if grep -q 'Removing Herdr plugin' "$WORK_DIR/core-uninstall"; then exit 1; fi
    printf 'y\n' | bash "$SCRIPT_DIR/uninstall.sh" > /dev/null
    test ! -e "$HERDR_RELEASE_ROOT"
done
echo "uninstall shell tests passed"

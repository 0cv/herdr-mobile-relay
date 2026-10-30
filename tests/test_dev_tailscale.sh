#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-dev-tailscale.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT
export HOME="$WORK_DIR/home"
mkdir -p "$WORK_DIR/bin" "$WORK_DIR/state" "$HOME/.local/bin"
export FAKE_LOG="$WORK_DIR/actions" FAKE_ENV="$WORK_DIR/state/relay.env"
export HERDR_RELAY_BIN="$WORK_DIR/bin/herdr-mobile-relay"
export FAKE_TAILSCALE_BIN="$WORK_DIR/bin/tailscale"
export HERDR_RELAY_ENV="$FAKE_ENV" HERDR_DEV_TUNNEL=1 HERDR_DEV_TRANSPORT=tailscale
export HERDR_RELAY_PORT=$((30000 + $$ % 20000)) HERDR_RELAY_HOST=127.0.0.1
export HERDR_BIN="$WORK_DIR/bin/herdr" HERDR_PHONE_APP_URL=relay
printf 'HERDR_RELAY_TOKEN=test-token\nHERDR_RELAY_INSTANCE_ID=test-instance\n' > "$FAKE_ENV"
printf '#!/bin/sh\nexit 0\n' > "$HERDR_BIN"
cat > "$FAKE_TAILSCALE_BIN" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'tailscale %s\n' "$*" >> "$FAKE_LOG"
case "$1" in
    check)
        if [ "${FAKE_CHECK_FAIL:-}" = 1 ]; then
            echo 'enable HTTPS certificates in your Tailscale tailnet DNS settings' >&2
            exit 7
        fi ;;
    address) echo 'https://sheep.example.ts.net:9443' ;;
    serve)
        if [ "${FAKE_SERVE_FAIL:-}" = 1 ]; then
            echo 'Tailscale HTTPS port 9443 is already in use' >&2
            exit 9
        fi
        trap 'exit 0' TERM INT
        while :; do sleep 1 & wait $! || :; done ;;
    off) ;;
esac
EOF
cat > "$HERDR_RELAY_BIN" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 ${2:-}" in
    'tailscale '*) "$FAKE_TAILSCALE_BIN" "${@:2}" ;;
    'setup-fragment '*) printf 'token=%s&relay=%s\n' "$2" "$4" ;;
    'normalize-origin '*)
        if [ "${FAKE_ORIGIN_FAIL:-}" = 1 ]; then exit 1; fi
        printf '%s\n' "${@: -1}" ;;
    'qr '*) echo '[fake QR]' ;;
    'serve '*)
        printf 'rearm=%s\n' "${HERDR_RELAY_REARM_BOOTSTRAP:-}" >> "$FAKE_LOG"
        printf '%s\n' "$$" > "${FAKE_ENV%/*}/relay.pid"
        trap 'exit 0' TERM INT
        trap ':' USR1
        while :; do sleep 1 & wait $! || :; done ;;
esac
EOF
chmod +x "$HERDR_RELAY_BIN" "$HERDR_BIN" "$FAKE_TAILSCALE_BIN"

for tool in launchctl systemctl cloudflared; do
    cat > "$HOME/.local/bin/$tool" <<'EOF'
#!/bin/sh
printf 'unexpected %s %s\n' "${0##*/}" "$*" >> "$FAKE_LOG"
exit 1
EOF
    chmod +x "$HOME/.local/bin/$tool"
done

cat > "$WORK_DIR/bin/bun" <<'EOF'
#!/bin/sh
printf 'bun %s\n' "$HERDR_DEV_TRANSPORT" >> "$FAKE_LOG"
exit 32
EOF
chmod +x "$WORK_DIR/bin/bun"
export PATH="$WORK_DIR/bin:$PATH"

run_dev() {
    HERDR_DEV_CONFIG_DIR="$WORK_DIR/dev" "$REPO_DIR/relay/dev-tunnel.sh" > "$WORK_DIR/dev-output" 2>&1
}
if (unset HERDR_DEV_TRANSPORT; run_dev); then
    echo 'unattended dev tunnel published without transport selection' >&2
    exit 1
fi
grep -q 'Select a transport' "$WORK_DIR/dev-output"
! grep -q 'tailscale serve' "$FAKE_LOG" 2>/dev/null
if (FAKE_CHECK_FAIL=1 run_dev); then
    echo 'unusable Tailscale was accepted' >&2
    exit 1
fi
grep -q 'enable HTTPS certificates' "$WORK_DIR/dev-output"
if (HERDR_DEV_TRANSPORT=invalid run_dev); then
    echo 'unknown transport was accepted' >&2
    exit 1
fi
if (HERDR_DEV_TRANSPORT=cloudflare run_dev); then
    echo 'fake Bun unexpectedly succeeded' >&2
    exit 1
fi
test "$(grep -c 'tailscale check' "$FAKE_LOG")" -eq 1

tty_dev() {
    if [ "$(uname -s)" = Darwin ]; then
        script -q /dev/null env HERDR_DEV_CONFIG_DIR="$WORK_DIR/dev" "$REPO_DIR/relay/dev-tunnel.sh"
    else
        script -q -e -c "env HERDR_DEV_CONFIG_DIR='$WORK_DIR/dev' '$REPO_DIR/relay/dev-tunnel.sh'" /dev/null
    fi
}
if command -v script >/dev/null 2>&1; then
    : > "$FAKE_LOG"
    (unset HERDR_DEV_TRANSPORT; { printf '1\n'; sleep 1; } | tty_dev) > "$WORK_DIR/dev-output" 2>&1 || true
    grep -q 'bun tailscale' "$FAKE_LOG" || { echo 'TTY selection failed:' >&2; tail -30 "$WORK_DIR/dev-output" >&2; exit 1; }
    (unset HERDR_DEV_TRANSPORT; { printf '2\n'; sleep 1; } | tty_dev) > "$WORK_DIR/dev-output" 2>&1 || true
    grep -q 'bun cloudflare' "$FAKE_LOG"
    : > "$FAKE_LOG"
    (unset HERDR_DEV_TRANSPORT; { printf 'q\n'; sleep 1; } | tty_dev) > "$WORK_DIR/dev-output" 2>&1 || true
    grep -q 'nothing published' "$WORK_DIR/dev-output"
    ! grep -q 'bun ' "$FAKE_LOG"
    (unset HERDR_DEV_TRANSPORT; printf '' | tty_dev) > "$WORK_DIR/dev-output" 2>&1 || true
    grep -q 'nothing published' "$WORK_DIR/dev-output"
    (unset HERDR_DEV_TRANSPORT; FAKE_CHECK_FAIL=1; export FAKE_CHECK_FAIL; { printf '1\n'; sleep 1; } | tty_dev) > "$WORK_DIR/dev-output" 2>&1 || true
    grep -q 'enable HTTPS certificates' "$WORK_DIR/dev-output"
    grep -q 'bun cloudflare' "$FAKE_LOG"
fi

run_start() {
    "$REPO_DIR/relay/start.sh" > "$WORK_DIR/start-output" 2>&1 &
    START_PID=$!
}
wait_for_log() {
    local expected="$1"
    local i
    for ((i = 0; i < 100; i++)); do
        if grep -q "$expected" "$FAKE_LOG"; then return 0; fi
        sleep 0.1
    done
    echo "Timed out waiting for $expected" >&2
    exit 1
}
: > "$FAKE_LOG"
run_start
wait_for_log 'tailscale serve --port 9443'
for ((i = 0; i < 100; i++)); do
    if grep -q 'sheep.example.ts.net:9443/#token=' "$WORK_DIR/start-output"; then break; fi
    sleep 0.1
done
grep -q 'sheep.example.ts.net:9443/#token=' "$WORK_DIR/start-output"
grep -q 'relay=wss://sheep.example.ts.net:9443' "$WORK_DIR/start-output"
grep -q 'only HTTPS port 9443 is added and removed on exit' "$WORK_DIR/start-output"
grep -q '^rearm=$' "$FAKE_LOG"
grep -q '\[fake QR\]' "$WORK_DIR/start-output"
kill -TERM "$START_PID"
wait "$START_PID" || :
grep -q "tailscale off --port 9443 --relay-port $HERDR_RELAY_PORT" "$FAKE_LOG"

: > "$FAKE_LOG"
if (FAKE_ORIGIN_FAIL=1 run_start; wait "$START_PID"); then
    echo 'failed phone origin unexpectedly succeeded' >&2
    exit 1
fi
grep -q 'tailscale off --port 9443' "$FAKE_LOG"

: > "$FAKE_LOG"
if (FAKE_SERVE_FAIL=1 run_start; wait "$START_PID"); then
    echo 'port conflict unexpectedly succeeded' >&2
    exit 1
fi
grep -q 'Tailscale HTTPS port 9443 is already in use' "$WORK_DIR/start-output"
! grep -q 'tailscale off' "$FAKE_LOG"
if grep -q '^unexpected ' "$FAKE_LOG"; then
    echo 'A host service tool was invoked' >&2
    exit 1
fi

echo 'dev Tailscale tests passed'

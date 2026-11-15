#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-start-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

unset GH_TOKEN GITHUB_TOKEN
mkdir -p "$WORK_DIR/config"
ensure_relay_test_binary "$WORK_DIR"

# Quick Start must never race an installed background service for the relay
# port. It restarts that service, waits for health, and prints the existing
# endpoint's QR without launching the packaged relay binary.
START_SCRIPT_DIR="$WORK_DIR/start-scripts"
START_HOME="$WORK_DIR/start-home"
START_BIN_DIR="$START_HOME/.local/bin"
START_ENV="$WORK_DIR/config/start.env"
START_SERVICE_LOG="$WORK_DIR/start-service.log"
START_RELAY_LOG="$WORK_DIR/start-relay.log"
mkdir -p "$START_SCRIPT_DIR" "$START_BIN_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/start.sh" "$START_SCRIPT_DIR/"
printf "HERDR_RELAY_TOKEN='start-token'\nHERDR_RELAY_INSTANCE_ID='start-instance'\n" \
    > "$START_ENV"
cat > "$START_SCRIPT_DIR/setup-link.sh" <<'EOF'
#!/bin/sh
printf '%s\n' 'setup QR printed'
EOF
cat > "$START_BIN_DIR/systemctl" <<'EOF'
#!/bin/sh
case "$*" in
    "--user is-active --quiet herdr-mobile-relay.service")
        exit 0
        ;;
    "--user restart herdr-mobile-relay.service")
        printf 'restarted\n' > "$START_SERVICE_LOG"
        ;;
    *)
        exit 1
        ;;
esac
EOF
cat > "$START_BIN_DIR/launchctl" <<'EOF'
#!/bin/sh
case "$1" in
    print) exit 0 ;;
    kickstart) printf 'restarted\n' > "$START_SERVICE_LOG" ;;
    *) exit 1 ;;
esac
EOF
cat > "$START_BIN_DIR/curl" <<'EOF'
#!/bin/sh
printf '%s\n' '{"status":"ready","inventory":{"state":"ready"},"instance":"start-instance","version":"9.9.9","release_version":"9.9.9","revision":"test-revision","bundle_hash":"test-web","protocol":3}'
EOF
cat > "$START_BIN_DIR/herdr" <<'EOF'
#!/bin/sh
exit 0
EOF
mkdir -p "$START_BIN_DIR/web"
cp "$REPO_DIR/web/release.json" "$START_BIN_DIR/web/release.json"
printf '%s\n' '{"version":"9.9.9","revision":"test-revision","web_hash":"test-web"}' \
    > "$START_BIN_DIR/release-manifest.json"
cat > "$START_BIN_DIR/relay-bin" <<'EOF'
#!/bin/sh
case "$1" in
    version) printf '%s\n' '{"version":"9.9.9","revision":"test-revision"}'; exit 0 ;;
    verify-readiness) exec "$HERDR_TEST_RELAY_BIN" "$@" ;;
esac
printf '%s\n' "$*" >> "$START_RELAY_LOG"
exit 1
EOF
chmod 700 "$START_SCRIPT_DIR/setup-link.sh" "$START_BIN_DIR/systemctl" \
    "$START_BIN_DIR/launchctl" "$START_BIN_DIR/curl" "$START_BIN_DIR/herdr" "$START_BIN_DIR/relay-bin"
export START_SERVICE_LOG START_RELAY_LOG
START_OUTPUT="$(
    HOME="$START_HOME" \
        PATH="$START_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$START_BIN_DIR/relay-bin" \
        HERDR_RELAY_ENV="$START_ENV" \
        bash "$START_SCRIPT_DIR/start.sh"
)"
test -f "$START_SERVICE_LOG"
test ! -e "$START_RELAY_LOG"
case "$START_OUTPUT" in
    *"Restarting the installed background relay"*"Background relay ready"*"setup QR printed"*) ;;
    *) echo "Quick Start did not reuse the installed background service" >&2; exit 1 ;;
esac

echo "start shell tests passed"

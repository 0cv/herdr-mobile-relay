#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TEST_BASH="${HERDR_SHELL_COVERAGE_BASH:-bash}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-supervisor-service.XXXXXX")"
WORK_DIR="$(cd "$WORK_DIR" && pwd -P)"
cleanup() {
    if [ "${KEEP_HERDR_SUPERVISOR_FIXTURE:-}" = 1 ]; then
        printf 'retained supervisor fixture: %s\n' "$WORK_DIR" >&2
    else
        rm -rf "$WORK_DIR"
    fi
}
trap cleanup EXIT

TEST_HOME="$WORK_DIR/home"
BIN_DIR="$WORK_DIR/bin"
CONFIG_DIR="$WORK_DIR/config"
SUPERVISOR_RELEASE_ROOT="$WORK_DIR/supervisor-release-root"
SUPERVISOR_RELEASE="$SUPERVISOR_RELEASE_ROOT/releases/test-release"
mkdir -p "$TEST_HOME" "$BIN_DIR" "$CONFIG_DIR" "$SUPERVISOR_RELEASE"

cat > "$BIN_DIR/herdr-mobile-relay" <<'SH'
#!/bin/sh
printf '%s\n' "$@" > "$SUPERVISOR_ARGS"
case "${1:-}" in
    verify-release)
        exit 0
        ;;
    supervisor-record-failure)
        printf '{"status":"%s"}\n' "${BOOTSTRAP_STATUS:-retrying}"
        exit "${BOOTSTRAP_RECORD_EXIT:-0}"
        ;;
    supervisor-status)
        if [ -n "${SUPERVISOR_STATUS_BODY:-}" ]; then
            printf '%s\n' "$SUPERVISOR_STATUS_BODY"
        else
            printf '%s\n' '{"status":"stopped"}'
        fi
        exit "${SUPERVISOR_STATUS_EXIT:-0}"
        ;;
esac
exit "${SUPERVISOR_EXIT:-0}"
SH
cat > "$BIN_DIR/cloudflared" <<'SH'
#!/bin/sh
exit 99
SH
chmod 700 "$BIN_DIR/herdr-mobile-relay" "$BIN_DIR/cloudflared"
cp "$BIN_DIR/herdr-mobile-relay" "$SUPERVISOR_RELEASE/herdr-mobile-relay"
printf '%s\n' '{"version":"test","revision":"test-revision","web_hash":"test-web"}' > "$SUPERVISOR_RELEASE/release-manifest.json"
ln -s "releases/test-release" "$SUPERVISOR_RELEASE_ROOT/current"
export HERDR_RELEASE_ROOT="$SUPERVISOR_RELEASE_ROOT"

CLOUDFLARED_CONFIG="$CONFIG_DIR/cloudflared.yml"
cat > "$CLOUDFLARED_CONFIG" <<EOF
tunnel: fixture
credentials-file: $CONFIG_DIR/tunnel.json
ingress:
  - hostname: remote.example.test
    service: http://127.0.0.1:8375
EOF
printf '{}\n' > "$CONFIG_DIR/tunnel.json"

ACTIVE_RUNTIME="$CONFIG_DIR/active-runtime.json"
printf '{}\n' > "$ACTIVE_RUNTIME"
RELAY_ENV="$CONFIG_DIR/relay.env"
cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_ACTIVE_RUNTIME=$ACTIVE_RUNTIME
HERDR_RELAY_MANAGED_DEPLOYMENT=true
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF

ARGS_FILE="$WORK_DIR/supervisor.args"
HOME="$TEST_HOME" \
PATH="$BIN_DIR:/usr/bin:/bin" \
HERDR_RELAY_ENV="$RELAY_ENV" \
SUPERVISOR_ARGS="$ARGS_FILE" \
"$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"

assert_arg_pair() {
    local key="$1"
    local value="$2"
    awk -v key="$key" -v value="$value" 'previous == key && $0 == value { found = 1 } { previous = $0 } END { exit !found }' "$ARGS_FILE"
}

test "$(sed -n '1p' "$ARGS_FILE")" = "supervise"
assert_arg_pair --relay "$SUPERVISOR_RELEASE/herdr-mobile-relay"
assert_arg_pair --release-root "$SUPERVISOR_RELEASE"
assert_arg_pair --instance fixture-instance
assert_arg_pair --cloudflared "$BIN_DIR/cloudflared"
assert_arg_pair --cloudflared-config "$CLOUDFLARED_CONFIG"
assert_arg_pair --active-runtime "$ACTIVE_RUNTIME"
grep -Fx -- '--managed' "$ARGS_FILE" >/dev/null
assert_arg_pair --local-health "http://127.0.0.1:8375/readyz"
assert_arg_pair --public-health "https://remote.example.test/readyz"
assert_arg_pair --state "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"
assert_arg_pair --log-dir "$TEST_HOME/.local/state/herdr-mobile-relay/logs"

XDG_STATE_ROOT="$WORK_DIR/xdg-state"
rm -f "$ARGS_FILE"
HOME="$TEST_HOME" \
XDG_STATE_HOME="$XDG_STATE_ROOT" \
PATH="$BIN_DIR:/usr/bin:/bin" \
HERDR_RELAY_ENV="$RELAY_ENV" \
SUPERVISOR_ARGS="$ARGS_FILE" \
"$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"
assert_arg_pair --state "$XDG_STATE_ROOT/herdr-mobile-relay/supervisor.json"
assert_arg_pair --log-dir "$XDG_STATE_ROOT/herdr-mobile-relay/logs"

RELATIVE_HOME_LOG="$WORK_DIR/relative-home.log"
if ! (
    cd "$WORK_DIR"
    HOME=relative-home PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"
) >"$RELATIVE_HOME_LOG" 2>&1; then
    echo "service wrapper treated an untrusted relative supervisor ledger as a crash" >&2
    exit 1
fi
grep -F 'no trusted absolute supervisor ledger' "$RELATIVE_HOME_LOG" >/dev/null

UNUSABLE_TRUSTED_LOG="$WORK_DIR/unusable-trusted-state.log"
if ! HOME=/dev/null PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >"$UNUSABLE_TRUSTED_LOG" 2>&1; then
    echo "service wrapper treated an unsecurable trusted ledger as a crash" >&2
    exit 1
fi
grep -F 'cannot secure its trusted supervisor ledger' "$UNUSABLE_TRUSTED_LOG" >/dev/null

CANONICAL_STATE="$WORK_DIR/canonical-state"
CANONICAL_ENV="$CONFIG_DIR/canonical-state.env"
CANONICAL_BASH_ENV="$WORK_DIR/canonicalization-failure.bash"
PARENT_BASH_ENV="${BASH_ENV:-}"
cp "$RELAY_ENV" "$CANONICAL_ENV"
printf 'HERDR_RELAY_SUPERVISOR_STATE_DIR=%s\n' "$CANONICAL_STATE" >> "$CANONICAL_ENV"
cat > "$CANONICAL_BASH_ENV" <<'SH'
if [ -n "${HERDR_PARENT_BASH_ENV:-}" ]; then
    . "$HERDR_PARENT_BASH_ENV"
fi
pwd() {
    if [ "$PWD" = "$CANONICAL_STATE" ]; then
        return 1
    fi
    builtin pwd "$@"
}
SH
rm -f "$ARGS_FILE"
set +e
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_ENV="$CANONICAL_ENV" CANONICAL_STATE="$CANONICAL_STATE" HERDR_PARENT_BASH_ENV="$PARENT_BASH_ENV" BASH_ENV="$CANONICAL_BASH_ENV" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
service_status=$?
set -e
test "$service_status" -eq 75
grep -F 'cannot canonicalize its configured supervisor state directory' "$ARGS_FILE" >/dev/null

MISSING_INSTANCE_ENV="$CONFIG_DIR/missing-instance.env"
sed '/^HERDR_RELAY_INSTANCE_ID=/d' "$RELAY_ENV" > "$MISSING_INSTANCE_ENV"
rm -f "$ARGS_FILE"
set +e
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_ENV="$MISSING_INSTANCE_ENV" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
service_status=$?
set -e
test "$service_status" -eq 75
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
grep -F 'HERDR_RELAY_INSTANCE_ID is required' "$ARGS_FILE" >/dev/null

if ! HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELEASE_ROOT="$WORK_DIR/missing-release-root" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper treated a missing verified release as a crash" >&2
    exit 1
fi

rm -f "$ARGS_FILE"
set +e
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" HERDR_RELAY_SUPERVISOR_STATE_DIR=/dev/null/herdr-state SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
service_status=$?
set -e
if [ "$service_status" -ne 75 ]; then
    echo "service wrapper did not record a bounded failure for an unusable state directory" >&2
    exit 1
fi
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
test "$(sed -n '2p' "$ARGS_FILE")" = "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"
grep -F '/dev/null/herdr-state' "$ARGS_FILE" >/dev/null

rm -f "$ARGS_FILE"
set +e
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" HERDR_RELAY_SUPERVISOR_STATE_DIR=relative-supervisor-state SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
service_status=$?
set -e
if [ "$service_status" -ne 75 ]; then
    echo "service wrapper did not record a bounded failure for a relative state directory" >&2
    exit 1
fi
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
test "$(sed -n '2p' "$ARGS_FILE")" = "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"
grep -F 'must be an absolute path' "$ARGS_FILE" >/dev/null
test ! -e "$ROOT_DIR/relative-supervisor-state"

CHMOD_BIN="$TEST_HOME/.local/bin"
mkdir -p "$CHMOD_BIN"
cat > "$CHMOD_BIN/chmod" <<'SH'
#!/bin/sh
case " $* " in
    *"/chmod-state "*) exit 1 ;;
    *) exec /bin/chmod "$@" ;;
esac
SH
chmod 700 "$CHMOD_BIN/chmod"
rm -f "$ARGS_FILE"
if ! HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" HERDR_RELAY_SUPERVISOR_STATE_DIR="$WORK_DIR/chmod-state" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=tripped "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper treated an unsecurable configured state directory as a crash" >&2
    exit 1
fi
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
test "$(sed -n '2p' "$ARGS_FILE")" = "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"

SUPERVISOR_STATE="$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"
mkdir -p "$(dirname "$SUPERVISOR_STATE")"
printf 'stale\n' > "$SUPERVISOR_STATE"
rm -f "$ARGS_FILE"
if ! HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SUPERVISOR_STATUS_EXIT=1 BOOTSTRAP_RECORD_EXIT=1 "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper treated an unrecordable bootstrap failure as a crash" >&2
    exit 1
fi
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure

rm -f "$ARGS_FILE"
if ! HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SUPERVISOR_STATUS_EXIT=1 BOOTSTRAP_STATUS=invalid "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper treated an invalid failure record as a crash" >&2
    exit 1
fi
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure

HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SUPERVISOR_STATUS_BODY='{"status":"stopped"}' "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"

rm -f "$ARGS_FILE"
sed '/HERDR_RELAY_ACTIVE_RUNTIME=/d' "$RELAY_ENV" > "$RELAY_ENV.tmp"
mv "$RELAY_ENV.tmp" "$RELAY_ENV"
for bootstrap_status in retrying tripped; do
    rm -f "$ARGS_FILE"
    set +e
    HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS="$bootstrap_status" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
    service_status=$?
    set -e
    if [ "$bootstrap_status" = retrying ]; then
        test "$service_status" -eq 75
    else
        test "$service_status" -eq 0
    fi
    test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
    test "$(sed -n '2p' "$ARGS_FILE")" = "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"
    test "$(sed -n '3p' "$ARGS_FILE")" = 5
    grep -F 'HERDR_RELAY_ACTIVE_RUNTIME' "$ARGS_FILE" >/dev/null
done

printf 'stale\n' > "$SUPERVISOR_STATE"
rm -f "$ARGS_FILE"
set +e
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SUPERVISOR_STATUS_BODY='{"status":"running"}' BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1
service_status=$?
set -e
test "$service_status" -eq 75
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
grep -F 'without a clean stop' "$ARGS_FILE" >/dev/null
rm -f "$SUPERVISOR_STATE"

cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
HOME="$TEST_HOME" \
PATH="$BIN_DIR:/usr/bin:/bin" \
HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" \
HERDR_RELAY_ENV="$RELAY_ENV" \
SUPERVISOR_ARGS="$ARGS_FILE" \
"$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"
if grep -Fx -- '--managed' "$ARGS_FILE" >/dev/null || grep -Fx -- '--active-runtime' "$ARGS_FILE" >/dev/null; then
    echo "ordinary relay service was forced into managed runtime ownership" >&2
    exit 1
fi

for managed_value in '' 1 TRUE yes tru; do
    cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_MANAGED_DEPLOYMENT=$managed_value
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
    rm -f "$ARGS_FILE"
    if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
        echo "service wrapper accepted non-canonical HERDR_RELAY_MANAGED_DEPLOYMENT=$managed_value" >&2
        exit 1
    fi
    test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-record-failure
done

cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
for service_failure in 64 74; do
    if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SUPERVISOR_EXIT="$service_failure" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
        echo "service wrapper hid terminal supervisor failure $service_failure" >&2
        exit 1
    else
        service_status=$?
    fi
    test "$service_status" -eq "$service_failure"
done

NO_CLOUDFLARED_ENV="$CONFIG_DIR/no-cloudflared.env"
cat > "$NO_CLOUDFLARED_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
command() {
    if [ "${1:-}" = -v ] && [ "${2:-}" = cloudflared ]; then
        return 1
    fi
    builtin command "$@"
}
export -f command
if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$NO_CLOUDFLARED_ENV" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper started without cloudflared" >&2
    exit 1
fi
unset -f command
grep -F 'cloudflared not found in PATH' "$ARGS_FILE" >/dev/null

cat > "$NO_CLOUDFLARED_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CONFIG_DIR/missing-cloudflared.yml
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$NO_CLOUDFLARED_ENV" SUPERVISOR_ARGS="$ARGS_FILE" BOOTSTRAP_STATUS=retrying "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper started without readable cloudflared config" >&2
    exit 1
fi
grep -F 'Cloudflare tunnel config not readable' "$ARGS_FILE" >/dev/null

cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=localhost
HERDR_RELAY_PORT=8375
HERDR_RELAY_PUBLIC_HEALTH_URL=https://override.example.test/readyz
EOF
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"
assert_arg_pair --local-health "http://localhost:8375/readyz"
assert_arg_pair --public-health "https://override.example.test/readyz"

sed 's/HERDR_RELAY_HOST=localhost/HERDR_RELAY_HOST=::1/' "$RELAY_ENV" > "$RELAY_ENV.tmp"
mv "$RELAY_ENV.tmp" "$RELAY_ENV"
HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh"
assert_arg_pair --local-health "http://[::1]:8375/readyz"

sed 's/HERDR_RELAY_HOST=::1/HERDR_RELAY_HOST=0.0.0.0/' "$RELAY_ENV" > "$RELAY_ENV.tmp"
mv "$RELAY_ENV.tmp" "$RELAY_ENV"
if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper accepted a non-loopback relay host" >&2
    exit 1
fi

cat > "$RELAY_ENV" <<EOF
HERDR_RELAY_INSTANCE_ID=fixture-instance
CLOUDFLARED_CONFIG=$CLOUDFLARED_CONFIG
CLOUDFLARED_BIN=$BIN_DIR/cloudflared
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
EOF
sed 's/remote.example.test/not a hostname!/' "$CLOUDFLARED_CONFIG" > "$CLOUDFLARED_CONFIG.tmp"
mv "$CLOUDFLARED_CONFIG.tmp" "$CLOUDFLARED_CONFIG"
if HOME="$TEST_HOME" PATH="$BIN_DIR:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" "$TEST_BASH" "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" >/dev/null 2>&1; then
    echo "service wrapper accepted an invalid public hostname" >&2
    exit 1
fi

unset HERDR_RELEASE_ROOT
SERVICE_HOME="$WORK_DIR/service-home"
SERVICE_BIN="$WORK_DIR/service-bin"
SERVICE_CONFIG="$WORK_DIR/service-cloudflared.yml"
SERVICE_PERSISTENT_CONFIG="$SERVICE_HOME/.config/herdr-mobile-relay"
SERVICE_ENV="$SERVICE_PERSISTENT_CONFIG/relay.env"
SERVICE_STATE_ROOT="$WORK_DIR/custom-supervisor-state"
LAUNCHCTL_STATE="$WORK_DIR/launchctl-loaded"
SERVICE_RELEASE_ROOT="$SERVICE_HOME/.local/share/herdr-mobile-relay"
SERVICE_RELEASE="$SERVICE_RELEASE_ROOT/releases/test-release"
mkdir -p "$SERVICE_HOME" "$SERVICE_BIN" "$SERVICE_PERSISTENT_CONFIG"
printf 'tunnel: fixture\n' > "$SERVICE_CONFIG"
cat > "$SERVICE_ENV" <<EOF
HERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef
HERDR_RELAY_INSTANCE_ID=fixture-instance
HERDR_RELAY_SUPERVISOR_STATE_DIR=$SERVICE_STATE_ROOT
HERDR_RELAY_PUBLIC_HEALTH_URL=https://remote.example.test/readyz
CLOUDFLARED_CONFIG=$SERVICE_CONFIG
EOF
cat > "$SERVICE_BIN/launchctl" <<'SH'
#!/bin/sh
case "$1" in
    print)
        case "${2:-}" in
            *com.herdr-remote.service) [ -n "${LEGACY_LAUNCHCTL_STATE:-}" ] && test -e "$LEGACY_LAUNCHCTL_STATE" ;;
            *) test -e "$LAUNCHCTL_STATE" ;;
        esac
        ;;
    bootout)
        if [ "${REFUSE_LAUNCHD_BOOTOUT:-false}" = true ]; then
            exit 1
        fi
        if [ "${REQUIRE_EXACT_LAUNCHD_BOOTOUT:-false}" = true ] &&
           [ "${2:-}" != "gui/$(id -u)/com.herdr-mobile-relay.service" ]; then
            exit 1
        fi
        case "${2:-} ${3:-}" in
            *com.herdr-remote.service*) [ -z "${LEGACY_LAUNCHCTL_STATE:-}" ] || rm -f "$LEGACY_LAUNCHCTL_STATE" ;;
            *) rm -f "$LAUNCHCTL_STATE" ;;
        esac
        ;;
    bootstrap) : > "$LAUNCHCTL_STATE" ;;
    enable|kickstart) ;;
    *) exit 1 ;;
esac
SH
cat > "$SERVICE_BIN/plutil" <<'SH'
#!/bin/sh
exit 0
SH
cat > "$SERVICE_BIN/curl" <<'SH'
#!/bin/sh
previous_definition=false
case "${TEST_UNAME:-Darwin}" in
    Linux) grep -F '/previous/systemd/service' "$HOME/.config/systemd/user/herdr-mobile-relay.service" 2>/dev/null >/dev/null && previous_definition=true ;;
    Darwin) grep -F 'previous.launchd.definition' "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" 2>/dev/null >/dev/null && previous_definition=true ;;
esac
if [ "${HEALTH_MODE:-healthy}" != healthy ] && [ "$previous_definition" != true ]; then
    exit 1
fi
printf '%s\n' '{"status":"ok","instance":"fixture-instance","version":"1.2.3","protocol":"1","release_version":"1.2.3","revision":"fixture-revision","bundle_hash":"fixture-web"}'
SH
cat > "$SERVICE_BIN/sleep" <<'SH'
#!/bin/sh
exit 0
SH
cat > "$SERVICE_BIN/uname" <<'SH'
#!/bin/sh
case "${1:-}" in
    -m) printf 'arm64\n' ;;
    *) printf '%s\n' "${TEST_UNAME:-Darwin}" ;;
esac
SH
cat > "$SERVICE_BIN/tail" <<'SH'
#!/bin/sh
printf '%s\n' "$@" > "$SERVICE_LOG_ARGS"
SH
cat > "$SERVICE_BIN/systemctl" <<'SH'
#!/bin/sh
printf '%s\n' "$@" >> "$SYSTEMCTL_ARGS"
if [ "${STATEFUL_SYSTEMCTL:-false}" != true ]; then
    case " $* " in
        *" is-active "*" herdr-mobile-relay.service "|*" is-enabled "*" herdr-mobile-relay.service ")
            test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
            exit $?
            ;;
        *" is-active "*" herdr-remote.service "|*" is-enabled "*" herdr-remote.service ")
            test -f "$HOME/.config/systemd/user/herdr-remote.service"
            exit $?
            ;;
        *) exit 0 ;;
    esac
fi
test "${1:-}" = --user && shift
case "${1:-}" in
    is-active) test -e "$SYSTEMCTL_ACTIVE_STATE" ;;
    is-enabled) test -e "$SYSTEMCTL_ENABLED_STATE" ;;
    restart) : > "$SYSTEMCTL_ACTIVE_STATE" ;;
    stop)
        [ "${REFUSE_SYSTEMD_STOP:-false}" != true ] || exit 1
        if [ "${REQUIRE_SYSTEMD_UNIT_FOR_STOP:-false}" = true ] && [ ! -f "$SYSTEMD_UNIT_PATH" ]; then
            exit 1
        fi
        rm -f "$SYSTEMCTL_ACTIVE_STATE"
        ;;
    enable) : > "$SYSTEMCTL_ENABLED_STATE" ;;
    disable)
        rm -f "$SYSTEMCTL_ENABLED_STATE"
        if [ "${2:-}" = --now ]; then
            rm -f "$SYSTEMCTL_ACTIVE_STATE"
        fi
        ;;
    daemon-reload|reset-failed) ;;
    *) exit 1 ;;
esac
SH
cat > "$SERVICE_BIN/journalctl" <<'SH'
#!/bin/sh
printf '%s\n' "$@" > "$SERVICE_JOURNAL_ARGS"
SH
cat > "$SERVICE_BIN/cloudflared" <<'SH'
#!/bin/sh
exit 0
SH
chmod 700 "$SERVICE_BIN/launchctl" "$SERVICE_BIN/plutil" "$SERVICE_BIN/curl" "$SERVICE_BIN/sleep" "$SERVICE_BIN/uname" "$SERVICE_BIN/tail" "$SERVICE_BIN/systemctl" "$SERVICE_BIN/journalctl" "$SERVICE_BIN/cloudflared"

SOURCE_ONLY_HOME="$WORK_DIR/source-only-home"
SOURCE_ONLY_CONFIG="$SOURCE_ONLY_HOME/.config/herdr-mobile-relay"
mkdir -p "$SOURCE_ONLY_CONFIG"
cp "$SERVICE_ENV" "$SOURCE_ONLY_CONFIG/relay.env"
if HOME="$SOURCE_ONLY_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_PLUGIN_CONFIG_DIR="$SOURCE_ONLY_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" >"$WORK_DIR/source-only-install.log" 2>&1; then
    echo "service installer accepted a source checkout without an installed release" >&2
    exit 1
fi
test ! -e "$SOURCE_ONLY_HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
grep -F 'Verified installed relay release is unavailable' "$WORK_DIR/source-only-install.log" >/dev/null
if HOME="$SOURCE_ONLY_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELEASE_ROOT="$SOURCE_ONLY_HOME/releases" HERDR_PLUGIN_CONFIG_DIR="$SOURCE_ONLY_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" SYSTEMCTL_ARGS="$WORK_DIR/source-only-systemctl.args" "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" >"$WORK_DIR/source-only-systemd-install.log" 2>&1; then
    echo "systemd service installer accepted a source checkout without an installed release" >&2
    exit 1
fi
grep -F 'Verified installed relay release is unavailable' "$WORK_DIR/source-only-systemd-install.log" >/dev/null

EXTERNAL_RELEASE="$WORK_DIR/external-release"
EXTERNAL_RELEASE_ROOT="$SOURCE_ONLY_HOME/releases"
mkdir -p "$EXTERNAL_RELEASE/relay" "$EXTERNAL_RELEASE_ROOT/releases"
printf '#!/bin/sh\nexit 0\n' > "$EXTERNAL_RELEASE/herdr-mobile-relay"
printf '#!/bin/sh\nexit 0\n' > "$EXTERNAL_RELEASE/relay/herdr-mobile-relay-service.sh"
printf '%s\n' '{"version":"1.2.3","revision":"external","target":"darwin/arm64"}' > "$EXTERNAL_RELEASE/release-manifest.json"
chmod 700 "$EXTERNAL_RELEASE/herdr-mobile-relay" "$EXTERNAL_RELEASE/relay/herdr-mobile-relay-service.sh"
ln -s "$EXTERNAL_RELEASE" "$EXTERNAL_RELEASE_ROOT/current"
if HOME="$SOURCE_ONLY_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELEASE_ROOT="$EXTERNAL_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SOURCE_ONLY_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" >"$WORK_DIR/external-install.log" 2>&1; then
    echo "launchd service installer accepted current outside the release root" >&2
    exit 1
fi
test ! -e "$SOURCE_ONLY_HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
grep -F 'outside the verified releases directory' "$WORK_DIR/external-install.log" >/dev/null || {
    cat "$WORK_DIR/external-install.log" >&2
    exit 1
}
if HOME="$SOURCE_ONLY_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux HERDR_RELEASE_ROOT="$EXTERNAL_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SOURCE_ONLY_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" SYSTEMCTL_ARGS="$WORK_DIR/external-systemctl.args" "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" >"$WORK_DIR/external-systemd-install.log" 2>&1; then
    echo "systemd service installer accepted current outside the release root" >&2
    exit 1
fi
test ! -e "$SOURCE_ONLY_HOME/.config/systemd/user/herdr-mobile-relay.service"
grep -F 'outside the verified releases directory' "$WORK_DIR/external-systemd-install.log" >/dev/null || {
    cat "$WORK_DIR/external-systemd-install.log" >&2
    exit 1
}

mkdir -p "$SERVICE_RELEASE/relay"
cp "$ROOT_DIR/relay/herdr-mobile-relay-service.sh" "$SERVICE_RELEASE/relay/herdr-mobile-relay-service.sh"
cat > "$SERVICE_RELEASE/herdr-mobile-relay" <<'SH'
#!/bin/sh
case "${1:-}" in
    verify-release)
        printf '%s\n' "$@" > "$HOME/verify-release.args"
        if env | grep -Eq 'SENTINEL_|^HERDR_(WEB_ROOT|RELAY_BIN)='; then
            exit 91
        fi
        if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE|HERDR_WEB_ROOT|HERDR_RELAY_BIN)=' "$HERDR_PLUGIN_CONFIG_DIR/relay.env"; then
            exit 92
        fi
        [ ! -e "$HERDR_PLUGIN_CONFIG_DIR/github-token" ] || exit 93
        ;;
    supervisor-ready)
        printf '%s\n' "$@" > "$HOME/supervisor-ready.args"
        previous_definition=false
        case "${TEST_UNAME:-Darwin}" in
            Linux) grep -F '/previous/systemd/service' "$HOME/.config/systemd/user/herdr-mobile-relay.service" 2>/dev/null >/dev/null && previous_definition=true ;;
            Darwin) grep -F 'previous.launchd.definition' "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" 2>/dev/null >/dev/null && previous_definition=true ;;
        esac
        if [ "${HEALTH_MODE:-healthy}" != healthy ] && [ "$previous_definition" != true ]; then
            exit 1
        fi
        printf '%s\n' '{"status":"ready","instance":"fixture-instance"}'
        ;;
    *) exit 1 ;;
esac
SH
chmod 700 "$SERVICE_RELEASE/herdr-mobile-relay" "$SERVICE_RELEASE/relay/herdr-mobile-relay-service.sh"
printf '%s\n' '{"version":"1.2.3","revision":"fixture-revision","web_hash":"fixture-web","target":"darwin/arm64"}' > "$SERVICE_RELEASE/release-manifest.json"
ln -s "releases/test-release" "$SERVICE_RELEASE_ROOT/current"

cat >> "$SERVICE_ENV" <<EOF
GH_TOKEN=SENTINEL_LEGACY_GH_TOKEN
GITHUB_TOKEN=SENTINEL_LEGACY_GITHUB_TOKEN
HERDR_GITHUB_TOKEN_FILE=$SERVICE_PERSISTENT_CONFIG/github-token
HERDR_WEB_ROOT=$WORK_DIR/source-checkout/web
HERDR_RELAY_BIN=$WORK_DIR/source-checkout/herdr-mobile-relay
EOF
printf 'SENTINEL_LEGACY_TOKEN_FILE\n' > "$SERVICE_PERSISTENT_CONFIG/github-token"
if ! HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" >"$WORK_DIR/credential-launchd-install.log" 2>&1; then
    cat "$WORK_DIR/credential-launchd-install.log" >&2
    echo "launchd service installer could not sanitize legacy credentials and source paths" >&2
    exit 1
fi
SERVICE_PLIST="$SERVICE_HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
test "$(sed -n '1p' "$SERVICE_HOME/verify-release.args")" = verify-release
if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE|HERDR_WEB_ROOT|HERDR_RELAY_BIN)=' "$SERVICE_ENV"; then
    echo "launchd service installer retained a credential or source-only runtime path" >&2
    exit 1
fi
test ! -e "$SERVICE_PERSISTENT_CONFIG/github-token"
test "$(sed -n '1p' "$SERVICE_HOME/supervisor-ready.args")" = supervisor-ready
grep -Fx -- --state "$SERVICE_HOME/supervisor-ready.args" >/dev/null
grep -Fx -- --release-root "$SERVICE_HOME/supervisor-ready.args" >/dev/null
grep -Fx -- --instance "$SERVICE_HOME/supervisor-ready.args" >/dev/null
grep -Fx -- --local-health "$SERVICE_HOME/supervisor-ready.args" >/dev/null
grep -Fx -- --public-health "$SERVICE_HOME/supervisor-ready.args" >/dev/null
grep -F "<string>$SERVICE_RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh</string>" "$SERVICE_PLIST" >/dev/null
grep -F "<string>$SERVICE_RELEASE_ROOT/current</string>" "$SERVICE_PLIST" >/dev/null
grep -F "<string>$SERVICE_ENV</string>" "$SERVICE_PLIST" >/dev/null
grep -F '<key>KeepAlive</key>' "$SERVICE_PLIST" >/dev/null
grep -F '<key>SuccessfulExit</key>' "$SERVICE_PLIST" >/dev/null
grep -F '<key>ThrottleInterval</key>' "$SERVICE_PLIST" >/dev/null
grep -F '<integer>10</integer>' "$SERVICE_PLIST" >/dev/null
for forbidden_key in StandardOutPath StandardErrorPath; do
    if grep -F "<key>$forbidden_key</key>" "$SERVICE_PLIST" >/dev/null; then
        echo "launchd service retains respawn or unbounded-log key: $forbidden_key" >&2
        exit 1
    fi
done
SYSTEMCTL_ARGS="$WORK_DIR/systemctl-transaction.args"
SERVICE_UNIT="$SERVICE_HOME/.config/systemd/user/herdr-mobile-relay.service"
mkdir -p "$(dirname "$SERVICE_UNIT")"
mkdir -p "$SERVICE_HOME/.local/bin"
cp "$SERVICE_BIN/sleep" "$SERVICE_BIN/curl" "$SERVICE_HOME/.local/bin/"
printf '%s\n' '[Service]' 'ExecStart=/previous/systemd/service' > "$SERVICE_UNIT"
cp "$SERVICE_UNIT" "$WORK_DIR/systemd-definition-before"
cat >> "$SERVICE_ENV" <<EOF
GH_TOKEN=SENTINEL_TRANSACTION_GH_TOKEN
GITHUB_TOKEN=SENTINEL_TRANSACTION_GITHUB_TOKEN
HERDR_GITHUB_TOKEN_FILE=$SERVICE_PERSISTENT_CONFIG/github-token
EOF
printf 'SENTINEL_TRANSACTION_TOKEN_FILE\n' > "$SERVICE_PERSISTENT_CONFIG/github-token"
cp -p "$SERVICE_ENV" "$WORK_DIR/systemd-env-before"
cp -p "$SERVICE_PERSISTENT_CONFIG/github-token" "$WORK_DIR/systemd-token-before"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" HEALTH_MODE=unhealthy "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" > "$WORK_DIR/transactional-systemd-install.log" 2>&1; then
    echo "systemd service install hid a terminal unhealthy launch" >&2
    exit 1
fi
if ! cmp -s "$WORK_DIR/systemd-definition-before" "$SERVICE_UNIT"; then
    echo "systemd service installer did not restore the prior definition after readiness failure" >&2
    exit 1
fi
cmp -s "$WORK_DIR/systemd-env-before" "$SERVICE_ENV"
cmp -s "$WORK_DIR/systemd-token-before" "$SERVICE_PERSISTENT_CONFIG/github-token"
grep -Fx -- 'is-active' "$SYSTEMCTL_ARGS" >/dev/null
grep -Fx -- 'is-enabled' "$SYSTEMCTL_ARGS" >/dev/null
grep -Fx -- 'enable' "$SYSTEMCTL_ARGS" >/dev/null
grep -Fx -- 'restart' "$SYSTEMCTL_ARGS" >/dev/null
cat > "$SERVICE_PLIST" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>previous.launchd.definition</string></dict></plist>
EOF
cp "$SERVICE_PLIST" "$WORK_DIR/launchd-definition-before"
cp -p "$SERVICE_ENV" "$WORK_DIR/launchd-env-before"
cp -p "$SERVICE_PERSISTENT_CONFIG/github-token" "$WORK_DIR/launchd-token-before"
: > "$LAUNCHCTL_STATE"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" HEALTH_MODE=unhealthy "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" > "$WORK_DIR/unhealthy-install.log" 2>&1; then
    echo "service install hid a terminal unhealthy launch" >&2
    exit 1
fi
if ! cmp -s "$WORK_DIR/launchd-definition-before" "$SERVICE_PLIST" || [ ! -e "$LAUNCHCTL_STATE" ]; then
    echo "launchd service installer did not restore the prior loaded definition after readiness failure" >&2
    exit 1
fi
cmp -s "$WORK_DIR/launchd-env-before" "$SERVICE_ENV"
cmp -s "$WORK_DIR/launchd-token-before" "$SERVICE_PERSISTENT_CONFIG/github-token"
grep -F 'Replacement Relay service did not become ready; rolling back.' "$WORK_DIR/unhealthy-install.log" >/dev/null
grep -F 'Restored the previous launchd service environment, definition, state, and exact live identity.' "$WORK_DIR/unhealthy-install.log" >/dev/null

rm -f "$SERVICE_PLIST" "$LAUNCHCTL_STATE"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" HEALTH_MODE=unhealthy REQUIRE_EXACT_LAUNCHD_BOOTOUT=true "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" > "$WORK_DIR/first-launchd-install.log" 2>&1; then
    echo "first launchd service install hid a terminal unhealthy launch" >&2
    exit 1
fi
if [ -e "$SERVICE_PLIST" ] || [ -e "$LAUNCHCTL_STATE" ]; then
    echo "first launchd install rollback deleted its definition without proving the exact service absent" >&2
    exit 1
fi

rm -f "$SERVICE_PLIST" "$LAUNCHCTL_STATE"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" LAUNCHCTL_STATE="$LAUNCHCTL_STATE" HEALTH_MODE=unhealthy REFUSE_LAUNCHD_BOOTOUT=true "$TEST_BASH" "$ROOT_DIR/relay/install-service.sh" > "$WORK_DIR/refused-launchd-rollback.log" 2>&1; then
    echo "first launchd service install hid an unproved rollback" >&2
    exit 1
fi
if [ ! -e "$SERVICE_PLIST" ] || [ ! -e "$LAUNCHCTL_STATE" ]; then
    echo "launchd rollback discarded diagnostics while the replacement service could still be loaded" >&2
    exit 1
fi
grep -F "Launchd replacement definition: $SERVICE_PLIST" "$WORK_DIR/refused-launchd-rollback.log" >/dev/null
rm -f "$SERVICE_PLIST" "$LAUNCHCTL_STATE"

SYSTEMCTL_ARGS="$WORK_DIR/systemctl.args"
mkdir -p "$SERVICE_HOME/.local/bin"
cp "$SERVICE_BIN/systemctl" "$SERVICE_BIN/cloudflared" "$SERVICE_BIN/curl" "$SERVICE_BIN/sleep" "$SERVICE_HOME/.local/bin/"
cat >> "$SERVICE_ENV" <<EOF
GH_TOKEN=SENTINEL_SYSTEMD_GH_TOKEN
GITHUB_TOKEN=SENTINEL_SYSTEMD_GITHUB_TOKEN
HERDR_GITHUB_TOKEN_FILE=$SERVICE_PERSISTENT_CONFIG/github-token
HERDR_WEB_ROOT=$WORK_DIR/old-checkout/web
HERDR_RELAY_BIN=$WORK_DIR/old-checkout/herdr-mobile-relay
EOF
printf 'SENTINEL_SYSTEMD_TOKEN_FILE\n' > "$SERVICE_PERSISTENT_CONFIG/github-token"
if ! HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" >"$WORK_DIR/credential-systemd-install.log" 2>&1; then
    cat "$WORK_DIR/credential-systemd-install.log" >&2
    echo "systemd service installer could not sanitize legacy credentials and source paths" >&2
    exit 1
fi
if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE|HERDR_WEB_ROOT|HERDR_RELAY_BIN)=' "$SERVICE_ENV"; then
    echo "systemd service installer retained a credential or source-only runtime path" >&2
    exit 1
fi
test ! -e "$SERVICE_PERSISTENT_CONFIG/github-token"
SERVICE_UNIT="$SERVICE_HOME/.config/systemd/user/herdr-mobile-relay.service"
grep -F "WorkingDirectory=$SERVICE_RELEASE_ROOT/current" "$SERVICE_UNIT" >/dev/null
grep -F "Environment=HERDR_RELAY_ENV=$SERVICE_ENV" "$SERVICE_UNIT" >/dev/null
grep -F "ExecStart=$SERVICE_RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh" "$SERVICE_UNIT" >/dev/null
grep -F 'StartLimitIntervalSec=120' "$SERVICE_UNIT" >/dev/null
grep -F 'StartLimitBurst=5' "$SERVICE_UNIT" >/dev/null
grep -F 'Restart=on-failure' "$SERVICE_UNIT" >/dev/null
grep -F 'RestartSec=10' "$SERVICE_UNIT" >/dev/null
grep -F 'KillMode=control-group' "$SERVICE_UNIT" >/dev/null
printf '%s\n' '[Service]' 'ExecStart=/previous/systemd/service' > "$SERVICE_UNIT"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" HEALTH_MODE=unhealthy "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" > "$WORK_DIR/unhealthy-systemd-install.log" 2>&1; then
    echo "systemd service install hid a terminal unhealthy launch" >&2
    exit 1
fi
grep -F 'Replacement Relay service did not become ready; rolling back.' "$WORK_DIR/unhealthy-systemd-install.log" >/dev/null
grep -F 'Restored the previous systemd service environment, definition, state, and exact live identity.' "$WORK_DIR/unhealthy-systemd-install.log" >/dev/null

SYSTEMCTL_ACTIVE_STATE="$WORK_DIR/systemctl-active"
SYSTEMCTL_ENABLED_STATE="$WORK_DIR/systemctl-enabled"
rm -f "$SERVICE_UNIT" "$SYSTEMCTL_ACTIVE_STATE" "$SYSTEMCTL_ENABLED_STATE"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" STATEFUL_SYSTEMCTL=true SYSTEMCTL_ACTIVE_STATE="$SYSTEMCTL_ACTIVE_STATE" SYSTEMCTL_ENABLED_STATE="$SYSTEMCTL_ENABLED_STATE" SYSTEMD_UNIT_PATH="$SERVICE_UNIT" REQUIRE_SYSTEMD_UNIT_FOR_STOP=true HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" HEALTH_MODE=unhealthy "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" > "$WORK_DIR/first-systemd-install.log" 2>&1; then
    echo "first systemd service install hid a terminal unhealthy launch" >&2
    exit 1
fi
if [ -e "$SERVICE_UNIT" ] || [ -e "$SYSTEMCTL_ACTIVE_STATE" ] || [ -e "$SYSTEMCTL_ENABLED_STATE" ]; then
    echo "first systemd install rollback removed its definition without proving the service inactive and disabled" >&2
    exit 1
fi

rm -f "$SERVICE_UNIT" "$SYSTEMCTL_ACTIVE_STATE" "$SYSTEMCTL_ENABLED_STATE"
if HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" TEST_UNAME=Linux SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" STATEFUL_SYSTEMCTL=true SYSTEMCTL_ACTIVE_STATE="$SYSTEMCTL_ACTIVE_STATE" SYSTEMCTL_ENABLED_STATE="$SYSTEMCTL_ENABLED_STATE" SYSTEMD_UNIT_PATH="$SERVICE_UNIT" REFUSE_SYSTEMD_STOP=true HERDR_RELEASE_ROOT="$SERVICE_RELEASE_ROOT" HERDR_PLUGIN_CONFIG_DIR="$SERVICE_PERSISTENT_CONFIG" CLOUDFLARED_CONFIG="$SERVICE_CONFIG" HEALTH_MODE=unhealthy "$TEST_BASH" "$ROOT_DIR/relay/install-systemd-user-service.sh" > "$WORK_DIR/refused-systemd-rollback.log" 2>&1; then
    echo "first systemd service install hid an unproved rollback" >&2
    exit 1
fi
if [ ! -e "$SERVICE_UNIT" ] || [ ! -e "$SYSTEMCTL_ACTIVE_STATE" ]; then
    echo "systemd rollback discarded diagnostics while the replacement service could still be active" >&2
    exit 1
fi
grep -F "Systemd replacement definition: $SERVICE_UNIT" "$WORK_DIR/refused-systemd-rollback.log" >/dev/null
rm -f "$SERVICE_UNIT" "$SYSTEMCTL_ACTIVE_STATE" "$SYSTEMCTL_ENABLED_STATE"

rm -f "$ARGS_FILE"
HOME="$TEST_HOME" PATH="$BIN_DIR:$SERVICE_BIN:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" TEST_UNAME=Darwin "$TEST_BASH" "$ROOT_DIR/relay/service.sh" reset
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-reset
test "$(sed -n '2p' "$ARGS_FILE")" = "$TEST_HOME/.local/state/herdr-mobile-relay/supervisor.json"

rm -f "$ARGS_FILE" "$SYSTEMCTL_ARGS"
HOME="$TEST_HOME" PATH="$BIN_DIR:$SERVICE_BIN:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" SUPERVISOR_ARGS="$ARGS_FILE" SYSTEMCTL_ARGS="$SYSTEMCTL_ARGS" TEST_UNAME=Linux "$TEST_BASH" "$ROOT_DIR/relay/service.sh" reset
test "$(sed -n '1p' "$ARGS_FILE")" = supervisor-reset
grep -Fx -- reset-failed "$SYSTEMCTL_ARGS" >/dev/null
grep -Fx -- restart "$SYSTEMCTL_ARGS" >/dev/null

if HOME="$TEST_HOME" PATH="$BIN_DIR:$SERVICE_BIN:/usr/bin:/bin" HERDR_RELAY_BIN="$BIN_DIR/herdr-mobile-relay" HERDR_RELAY_ENV="$RELAY_ENV" TEST_UNAME=Darwin "$TEST_BASH" "$ROOT_DIR/relay/service.sh" invalid > "$WORK_DIR/invalid-action.log" 2>&1; then
    echo "service command accepted an invalid action" >&2
    exit 1
else
    test "$?" -eq 2
fi
grep -F '{install|uninstall|status|logs|reset}' "$WORK_DIR/invalid-action.log" >/dev/null

SERVICE_LOG_ARGS="$WORK_DIR/service-log.args" HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELAY_ENV="$SERVICE_ENV" "$TEST_BASH" "$ROOT_DIR/relay/service.sh" logs
test "$(sed -n '1p' "$WORK_DIR/service-log.args")" = -F
for child_log in relay.stdout.log relay.stderr.log cloudflared.stdout.log cloudflared.stderr.log; do
    grep -Fx "$SERVICE_STATE_ROOT/logs/$child_log" "$WORK_DIR/service-log.args" >/dev/null
done
if grep -F "$SERVICE_HOME/Library/Logs" "$WORK_DIR/service-log.args" >/dev/null; then
    echo "service log command still follows removed unbounded launchd logs" >&2
    exit 1
fi

SERVICE_JOURNAL_ARGS="$WORK_DIR/service-journal.args" HOME="$SERVICE_HOME" PATH="$SERVICE_BIN:/usr/bin:/bin" HERDR_RELAY_ENV="$SERVICE_ENV" TEST_UNAME=Linux "$TEST_BASH" "$ROOT_DIR/relay/service.sh" logs
grep -Fx -- herdr-mobile-relay.service "$WORK_DIR/service-journal.args" >/dev/null

awk '/^restore_relay_config\(\)/ { capture = 1 } !capture { print ""; next } /^trap cleanup_launchd_install EXIT/ { exit } { print }' "$ROOT_DIR/relay/install-service.sh" > "$WORK_DIR/install-service-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/install-service-functions.sh"
LEGACY_WAS_LOADED=false
LEGACY_STOPPED=false
LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
LEGACY_PLIST="$WORK_DIR/legacy-launchd.plist"
PREVIOUS_READINESS_CAPTURED=false

LAUNCHD_FUNCTION_ROOT="$WORK_DIR/launchd-function-tests"
mkdir -p "$LAUNCHD_FUNCTION_ROOT"
ENV_DIR="$LAUNCHD_FUNCTION_ROOT/config"
ENV_FILE="$ENV_DIR/relay.env"
TOKEN_FILE="$ENV_DIR/github-token"
mkdir -p "$ENV_DIR"
printf 'temporary env\n' > "$ENV_FILE"
printf 'temporary token\n' > "$TOKEN_FILE"
TOKEN_EXISTED=false
ENV_EXISTED=false
TOKEN_RESTORE_TEMP=
ENV_RESTORE_TEMP=
restore_relay_config
test ! -e "$ENV_FILE"
test ! -e "$TOKEN_FILE"

ENV_EXISTED=true
ENV_FILE="$LAUNCHD_FUNCTION_ROOT/capture.env"
printf 'fixture\n' > "$ENV_FILE"
CAPTURE_HOST=127.0.0.1
CAPTURE_PORT=8375
CAPTURE_PUBLIC=https://explicit.example.test/readyz
CAPTURE_CONFIG="$LAUNCHD_FUNCTION_ROOT/cloudflared.yml"
CAPTURE_PUBLIC_HOST=derived.example.test
load_relay_env() {
    HERDR_RELAY_HOST="$CAPTURE_HOST"
    HERDR_RELAY_PORT="$CAPTURE_PORT"
    HERDR_RELAY_PUBLIC_HEALTH_URL="$CAPTURE_PUBLIC"
    CLOUDFLARED_CONFIG="$CAPTURE_CONFIG"
}
yaml_scalar() { printf '%s\n' "$CAPTURE_PUBLIC_HOST"; }
valid_hostname() { [ "$1" != invalid ]; }
relay_release_identity_at_endpoints() { printf 'version\nrevision\nhash\n'; }

CAPTURE_HOST=::1
CAPTURE_PUBLIC=https://explicit.example.test/healthz
capture_output="$(capture_previous_service_readiness)"
grep -F 'http://[::1]:8375/healthz' <<<"$capture_output" >/dev/null
grep -F 'https://explicit.example.test/healthz' <<<"$capture_output" >/dev/null
CAPTURE_HOST=outside
if capture_previous_service_readiness >/dev/null; then
    echo "launchd readiness capture accepted a non-loopback host" >&2
    exit 1
fi
CAPTURE_HOST=127.0.0.1
CAPTURE_PUBLIC=
CAPTURE_PUBLIC_HOST=derived.example.test
capture_output="$(capture_previous_service_readiness)"
grep -F 'https://derived.example.test/healthz' <<<"$capture_output" >/dev/null
CAPTURE_PUBLIC=https://invalid.example.test/not-ready
if capture_previous_service_readiness >/dev/null; then
    echo "launchd readiness capture accepted an invalid public health URL" >&2
    exit 1
fi

SERVICE_WAS_LOADED=true
PREVIOUS_READINESS_CAPTURED=true
RELEASE_ROOT="$LAUNCHD_FUNCTION_ROOT/release-root"
PREVIOUS_CLOUDFLARED_CONFIG="$CAPTURE_CONFIG"
PREVIOUS_LOCAL_HEALTH=http://127.0.0.1:8375/healthz
PREVIOUS_PUBLIC_HEALTH=https://previous.example.test/healthz
PREVIOUS_IDENTITY='version
revision
hash'
wait_for_installed_relay_ready() { return 1; }
if prove_previous_service_readiness; then
    echo "launchd readiness proof accepted a failed supervisor proof" >&2
    exit 1
fi
wait_for_installed_relay_ready() { return 0; }
relay_release_identity_at_endpoints() { printf 'wrong\nidentity\nvalue\n'; }
sleep() { return 0; }
if prove_previous_service_readiness; then
    echo "launchd readiness proof accepted the wrong restored identity" >&2
    exit 1
fi

expect_launchd_rollback_failure() (
    case_label=$1
    LAUNCHD_ROLLBACK_MODE=$2
    SERVICE_WAS_LOADED=$3
    PLIST_EXISTED=true
    definition_replaced=true
    LABEL=com.herdr-mobile-relay.service
    SERVICE_TARGET=gui/501/com.herdr-mobile-relay.service
    case_root="$LAUNCHD_FUNCTION_ROOT/$case_label"
    mkdir -p "$case_root"
    PLIST="$case_root/current.plist"
    PLIST_BACKUP="$case_root/backup.plist"
    printf 'candidate\n' > "$PLIST"
    printf 'previous\n' > "$PLIST_BACKUP"
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    reload_launchd_service_definition() { return 0; }
    launchctl() { return 0; }
    launchd_service_loaded() {
        if [ "$SERVICE_WAS_LOADED" = true ]; then
            [ "$LAUNCHD_ROLLBACK_MODE" != loaded-proof ]
        else
            return 1
        fi
    }
    cp() {
        case "$LAUNCHD_ROLLBACK_MODE" in cp-*) return 1 ;; esac
        command cp "$@"
    }
    mv() {
        case "$LAUNCHD_ROLLBACK_MODE" in mv-*) return 1 ;; esac
        command mv "$@"
    }
    if rollback_launchd_install; then
        echo "launchd rollback accepted $case_label" >&2
        exit 1
    fi
)

expect_launchd_rollback_failure loaded-copy cp-loaded true
expect_launchd_rollback_failure loaded-move mv-loaded true
expect_launchd_rollback_failure loaded-proof loaded-proof true
expect_launchd_rollback_failure unloaded-copy cp-unloaded false
expect_launchd_rollback_failure unloaded-move mv-unloaded false

(
    LEGACY_STOPPED=true
    LEGACY_WAS_LOADED=true
    LEGACY_PLIST="$LAUNCHD_FUNCTION_ROOT/legacy.plist"
    LEGACY_LABEL=com.herdr-remote.service
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    definition_replaced=false
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    reload_launchd_service_definition() { printf '%s\n' "$1:$2" > "$LAUNCHD_FUNCTION_ROOT/legacy-reload"; }
    launchd_service_loaded() { return 0; }
    rollback_launchd_install
    grep -Fx "$LEGACY_PLIST:$LEGACY_LABEL" "$LAUNCHD_FUNCTION_ROOT/legacy-reload" >/dev/null
)
if (
    LEGACY_STOPPED=true
    LEGACY_WAS_LOADED=true
    LEGACY_PLIST="$LAUNCHD_FUNCTION_ROOT/legacy.plist"
    LEGACY_LABEL=com.herdr-remote.service
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    definition_replaced=false
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    reload_launchd_service_definition() { return 0; }
    launchd_service_loaded() { return 1; }
    rollback_launchd_install
); then
    echo "launchd rollback accepted an unproven legacy service restore" >&2
    exit 1
fi
if (
    LEGACY_STOPPED=true
    LEGACY_WAS_LOADED=true
    LEGACY_PLIST="$LAUNCHD_FUNCTION_ROOT/legacy.plist"
    LEGACY_LABEL=com.herdr-remote.service
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    definition_replaced=false
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    reload_launchd_service_definition() { return 1; }
    launchd_service_loaded() { return 0; }
    rollback_launchd_install
); then
    echo "launchd rollback ignored a legacy service reload failure" >&2
    exit 1
fi

LAUNCHD_CLEANUP_LOG="$LAUNCHD_FUNCTION_ROOT/cleanup.log"
if (
    set +e
    rollback_launchd_install() { return 1; }
    service_transaction_changed=true
    rollback_failed=false
    ENV_BACKUP=
    TOKEN_BACKUP=
    PLIST_BACKUP="$LAUNCHD_FUNCTION_ROOT/retained.plist"
    PLIST_TEMP=
    ENV_RESTORE_TEMP=
    TOKEN_RESTORE_TEMP=
    definition_replaced=true
    false
    cleanup_launchd_install
) >"$LAUNCHD_CLEANUP_LOG" 2>&1; then
    echo "launchd cleanup hid a rollback failure" >&2
    exit 1
fi
grep -F 'Launchd definition backup:' "$LAUNCHD_CLEANUP_LOG" >/dev/null

awk '/^restore_relay_config\(\)/ { capture = 1 } !capture { print ""; next } /^trap cleanup_systemd_install EXIT/ { exit } { print }' "$ROOT_DIR/relay/install-systemd-user-service.sh" > "$WORK_DIR/install-systemd-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/install-systemd-functions.sh"
LEGACY_WAS_ACTIVE=false
LEGACY_WAS_ENABLED=false
LEGACY_STOPPED=false
LEGACY_LABEL=herdr-remote.service
PREVIOUS_READINESS_CAPTURED=false

SYSTEMD_FUNCTION_ROOT="$WORK_DIR/systemd-function-tests"
mkdir -p "$SYSTEMD_FUNCTION_ROOT"
ENV_DIR="$SYSTEMD_FUNCTION_ROOT/config"
ENV_FILE="$ENV_DIR/relay.env"
TOKEN_FILE="$ENV_DIR/github-token"
mkdir -p "$ENV_DIR"
printf 'temporary env\n' > "$ENV_FILE"
printf 'temporary token\n' > "$TOKEN_FILE"
TOKEN_EXISTED=false
ENV_EXISTED=false
TOKEN_RESTORE_TEMP=
ENV_RESTORE_TEMP=
restore_relay_config
test ! -e "$ENV_FILE"
test ! -e "$TOKEN_FILE"

ENV_EXISTED=true
ENV_FILE="$SYSTEMD_FUNCTION_ROOT/capture.env"
printf 'fixture\n' > "$ENV_FILE"
CAPTURE_HOST=::1
CAPTURE_PORT=8375
CAPTURE_PUBLIC=https://explicit.example.test/healthz
CAPTURE_CONFIG="$SYSTEMD_FUNCTION_ROOT/cloudflared.yml"
CAPTURE_PUBLIC_HOST=derived.example.test
relay_release_identity_at_endpoints() { printf 'version\nrevision\nhash\n'; }
capture_output="$(capture_previous_service_readiness)"
grep -F 'http://[::1]:8375/healthz' <<<"$capture_output" >/dev/null
CAPTURE_HOST=outside
if capture_previous_service_readiness >/dev/null; then
    echo "systemd readiness capture accepted a non-loopback host" >&2
    exit 1
fi
CAPTURE_HOST=127.0.0.1
CAPTURE_PUBLIC=
CAPTURE_PUBLIC_HOST=derived.example.test
capture_output="$(capture_previous_service_readiness)"
grep -F 'https://derived.example.test/healthz' <<<"$capture_output" >/dev/null
CAPTURE_PUBLIC=https://invalid.example.test/not-ready
if capture_previous_service_readiness >/dev/null; then
    echo "systemd readiness capture accepted an invalid public health URL" >&2
    exit 1
fi

SERVICE_WAS_ACTIVE=true
PREVIOUS_READINESS_CAPTURED=true
RELEASE_ROOT="$SYSTEMD_FUNCTION_ROOT/release-root"
PREVIOUS_CLOUDFLARED_CONFIG="$CAPTURE_CONFIG"
PREVIOUS_LOCAL_HEALTH=http://127.0.0.1:8375/healthz
PREVIOUS_PUBLIC_HEALTH=https://previous.example.test/healthz
PREVIOUS_IDENTITY='version
revision
hash'
wait_for_installed_relay_ready() { return 1; }
if prove_previous_service_readiness; then
    echo "systemd readiness proof accepted a failed supervisor proof" >&2
    exit 1
fi
wait_for_installed_relay_ready() { return 0; }
relay_release_identity_at_endpoints() { printf 'wrong\nidentity\nvalue\n'; }
if prove_previous_service_readiness; then
    echo "systemd readiness proof accepted the wrong restored identity" >&2
    exit 1
fi

expect_systemd_rollback_failure() (
    case_label=$1
    SYSTEMD_ROLLBACK_MODE=$2
    SERVICE_WAS_ACTIVE=$3
    SERVICE_WAS_ENABLED=$4
    UNIT_EXISTED=true
    definition_replaced=true
    LABEL=herdr-mobile-relay.service
    case_root="$SYSTEMD_FUNCTION_ROOT/$case_label"
    UNIT_DIR="$case_root"
    UNIT_FILE="$case_root/current.service"
    UNIT_BACKUP="$case_root/backup.service"
    mkdir -p "$case_root"
    printf 'candidate\n' > "$UNIT_FILE"
    printf 'previous\n' > "$UNIT_BACKUP"
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    cp() {
        [ "$SYSTEMD_ROLLBACK_MODE" != copy ] || return 1
        command cp "$@"
    }
    mv() {
        [ "$SYSTEMD_ROLLBACK_MODE" != move ] || return 1
        command mv "$@"
    }
    systemctl() {
        [ "${1:-}" != --user ] || shift
        case "$SYSTEMD_ROLLBACK_MODE:${1:-}" in
            enabled-proof:is-enabled) return 1 ;;
            disabled-proof:is-active) return 1 ;;
            disabled-proof:is-enabled) return 0 ;;
            active-proof:is-enabled) return 1 ;;
            active-proof:is-active) return 1 ;;
            *) return 0 ;;
        esac
    }
    if rollback_systemd_install; then
        echo "systemd rollback accepted $case_label" >&2
        exit 1
    fi
)

expect_systemd_rollback_failure restore-copy copy true true
expect_systemd_rollback_failure restore-move move true true
expect_systemd_rollback_failure enabled-proof enabled-proof true true
expect_systemd_rollback_failure disabled-proof disabled-proof false false
expect_systemd_rollback_failure active-proof active-proof true false

(
    LEGACY_STOPPED=true
    LEGACY_WAS_ACTIVE=true
    LEGACY_WAS_ENABLED=true
    LEGACY_LABEL=herdr-remote.service
    definition_replaced=false
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    systemctl() {
        [ "${1:-}" != --user ] || shift
        printf '%s\n' "$*" >> "$SYSTEMD_FUNCTION_ROOT/legacy-restore"
        return 0
    }
    : > "$SYSTEMD_FUNCTION_ROOT/legacy-restore"
    rollback_systemd_install
    grep -Fx 'enable herdr-remote.service' "$SYSTEMD_FUNCTION_ROOT/legacy-restore" >/dev/null
    grep -Fx 'restart herdr-remote.service' "$SYSTEMD_FUNCTION_ROOT/legacy-restore" >/dev/null
)
if (
    LEGACY_STOPPED=true
    LEGACY_WAS_ACTIVE=true
    LEGACY_WAS_ENABLED=false
    LEGACY_LABEL=herdr-remote.service
    definition_replaced=false
    restore_relay_config() { return 0; }
    prove_previous_service_readiness() { return 0; }
    systemctl() {
        [ "${1:-}" != --user ] || shift
        [ "${1:-}" != is-active ]
    }
    rollback_systemd_install
); then
    echo "systemd rollback accepted an unproven legacy service restore" >&2
    exit 1
fi
for legacy_restore_failure in enable restart; do
    if (
        LEGACY_STOPPED=true
        LEGACY_WAS_ACTIVE=true
        LEGACY_WAS_ENABLED=true
        LEGACY_LABEL=herdr-remote.service
        definition_replaced=false
        restore_relay_config() { return 0; }
        prove_previous_service_readiness() { return 0; }
        systemctl() {
            [ "${1:-}" != --user ] || shift
            [ "${1:-}" != "$legacy_restore_failure" ]
        }
        rollback_systemd_install
    ); then
        echo "systemd rollback ignored a legacy service $legacy_restore_failure failure" >&2
        exit 1
    fi
done

SYSTEMD_CLEANUP_LOG="$SYSTEMD_FUNCTION_ROOT/cleanup.log"
if (
    set +e
    rollback_systemd_install() { return 1; }
    service_transaction_changed=true
    rollback_failed=false
    ENV_BACKUP=
    TOKEN_BACKUP=
    UNIT_BACKUP="$SYSTEMD_FUNCTION_ROOT/retained.service"
    UNIT_TEMP=
    ENV_RESTORE_TEMP=
    TOKEN_RESTORE_TEMP=
    definition_replaced=true
    false
    cleanup_systemd_install
) >"$SYSTEMD_CLEANUP_LOG" 2>&1; then
    echo "systemd cleanup hid a rollback failure" >&2
    exit 1
fi
grep -F 'Systemd definition backup:' "$SYSTEMD_CLEANUP_LOG" >/dev/null

write_service_fragment() {
    fragment_source=$1
    fragment_start=$2
    fragment_end=$3
    fragment_output=$4
    awk -v first="$fragment_start" -v last="$fragment_end" 'NR < first { print ""; next } NR <= last { print; next } { exit }' "$fragment_source" > "$fragment_output"
}

LAUNCHD_FRAGMENT="$WORK_DIR/install-service-functions.sh"
write_service_fragment "$ROOT_DIR/relay/install-service.sh" 17 29 "$LAUNCHD_FRAGMENT"
(
    safe_env="$WORK_DIR/launchd-explicit.env"
    printf 'safe\n' > "$safe_env"
    HERDR_RELAY_ENV="$safe_env"
    # shellcheck source=/dev/null
    . "$LAUNCHD_FRAGMENT"
    test "$ENV_FILE" = "$safe_env"
)
launchd_unsafe_env="$WORK_DIR/launchd-unsafe.env"
ln -s "$WORK_DIR/missing-launchd-env" "$launchd_unsafe_env"
if (HERDR_RELAY_ENV="$launchd_unsafe_env"; . "$LAUNCHD_FRAGMENT") >/dev/null 2>&1; then
    echo "launchd installer accepted an unsafe explicit environment" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 223 232 "$LAUNCHD_FRAGMENT"
if (
    ENV_FILE="$launchd_unsafe_env"
    ENV_DIR="$WORK_DIR"
    ENV_EXISTED=false
    ENV_BACKUP=
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an environment swapped before backup" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 233 242 "$LAUNCHD_FRAGMENT"
launchd_unsafe_token="$WORK_DIR/launchd-unsafe-token"
ln -s "$WORK_DIR/missing-launchd-token" "$launchd_unsafe_token"
if (
    TOKEN_FILE="$launchd_unsafe_token"
    ENV_DIR="$WORK_DIR"
    TOKEN_EXISTED=false
    TOKEN_BACKUP=
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an unsafe legacy token" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 244 286 "$LAUNCHD_FRAGMENT"
launchd_unsafe_plist="$WORK_DIR/launchd-unsafe.plist"
ln -s "$WORK_DIR/missing-launchd-plist" "$launchd_unsafe_plist"
if (
    PLIST="$launchd_unsafe_plist"
    LABEL=com.herdr-mobile-relay.service
    PLIST_EXISTED=false
    PLIST_BACKUP=
    SERVICE_WAS_LOADED=false
    SERVICE_TARGET=gui/501/com.herdr-mobile-relay.service
    LEGACY_PLIST="$WORK_DIR/missing-legacy-launchd.plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 1; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an unsafe service definition" >&2
    exit 1
fi
if (
    PLIST="$WORK_DIR/missing-loaded-launchd.plist"
    LABEL=com.herdr-mobile-relay.service
    PLIST_EXISTED=false
    PLIST_BACKUP=
    SERVICE_WAS_LOADED=false
    SERVICE_TARGET=gui/501/com.herdr-mobile-relay.service
    LEGACY_PLIST="$WORK_DIR/missing-legacy-launchd.plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 0; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted a loaded service without its definition" >&2
    exit 1
fi
if (
    PLIST="$WORK_DIR/snapshot-launchd.plist"
    LABEL=com.herdr-mobile-relay.service
    printf 'previous\n' > "$PLIST"
    PLIST_EXISTED=false
    PLIST_BACKUP=
    SERVICE_WAS_LOADED=false
    SERVICE_TARGET=gui/501/com.herdr-mobile-relay.service
    LEGACY_PLIST="$WORK_DIR/missing-legacy-launchd.plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { [ "$1" = "$SERVICE_TARGET" ]; }
    capture_previous_service_readiness() { return 1; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an existing service without an exact readiness snapshot" >&2
    exit 1
fi
(
    PLIST="$WORK_DIR/missing-primary-launchd.plist"
    LABEL=com.herdr-mobile-relay.service
    PLIST_EXISTED=false
    PLIST_BACKUP=
    SERVICE_WAS_LOADED=false
    SERVICE_TARGET=gui/501/com.herdr-mobile-relay.service
    LEGACY_PLIST="$WORK_DIR/legacy-only-launchd.plist"
    printf 'legacy\n' > "$LEGACY_PLIST"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    PREVIOUS_READINESS_CAPTURED=false
    launchd_service_loaded() { [ "$1" = "$LEGACY_SERVICE_TARGET" ]; }
    capture_previous_service_readiness() { return 1; }
    . "$LAUNCHD_FRAGMENT"
    test "$LEGACY_WAS_LOADED" = true
    test "$PREVIOUS_READINESS_CAPTURED" = false
)

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 260 272 "$LAUNCHD_FRAGMENT"
launchd_legacy_plist="$WORK_DIR/legacy-launchd.plist"
printf 'legacy\n' > "$launchd_legacy_plist"
(
    LEGACY_PLIST="$launchd_legacy_plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 0; }
    . "$LAUNCHD_FRAGMENT"
    test "$LEGACY_WAS_LOADED" = true
)
(
    LEGACY_PLIST="$launchd_legacy_plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 1; }
    set +e
    . "$LAUNCHD_FRAGMENT"
    set -e
    test "$LEGACY_WAS_LOADED" = false
)
launchd_unsafe_legacy_plist="$WORK_DIR/unsafe-legacy-launchd.plist"
ln -s "$WORK_DIR/missing-legacy-launchd" "$launchd_unsafe_legacy_plist"
if (
    LEGACY_PLIST="$launchd_unsafe_legacy_plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 1; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an unsafe legacy service definition" >&2
    exit 1
fi
if (
    LEGACY_PLIST="$WORK_DIR/missing-live-legacy-launchd.plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    LEGACY_WAS_LOADED=false
    launchd_service_loaded() { return 0; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted a loaded legacy service without its definition" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 288 306 "$LAUNCHD_FRAGMENT"
(
    LEGACY_WAS_LOADED=true
    LEGACY_STOPPED=false
    LEGACY_PLIST="$launchd_legacy_plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    launchctl() { return 1; }
    launchd_service_loaded() { return 1; }
    . "$LAUNCHD_FRAGMENT"
    test "$LEGACY_STOPPED" = true
)
if (
    LEGACY_WAS_LOADED=true
    LEGACY_STOPPED=false
    LEGACY_PLIST="$launchd_legacy_plist"
    LEGACY_SERVICE_TARGET=gui/501/com.herdr-remote.service
    launchctl() { return 0; }
    launchd_service_loaded() { return 0; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted a legacy service that remained loaded" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 288 315 "$LAUNCHD_FRAGMENT"
if (
    service_transaction_changed=false
    ENV_DIR="$WORK_DIR/launchd-missing-wrapper-config"
    ENV_FILE="$ENV_DIR/relay.env"
    CLOUDFLARED_CONFIG="$WORK_DIR/cloudflared.yml"
    SERVICE_WRAPPER="$WORK_DIR/missing-launchd-wrapper"
    WORK_DIR="$WORK_DIR/launchd-release"
    LEGACY_WAS_LOADED=false
    ensure_relay_env() { return 0; }
    load_relay_env() { return 0; }
    verified_installed_release() { return 0; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted a verified release without its service wrapper" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-service.sh" 350 354 "$LAUNCHD_FRAGMENT"
if (
    PLIST_TEMP="$WORK_DIR/invalid-candidate.plist"
    printf 'invalid\n' > "$PLIST_TEMP"
    plutil() { return 1; }
    . "$LAUNCHD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "launchd installer accepted an invalid generated definition" >&2
    exit 1
fi

SYSTEMD_FRAGMENT="$WORK_DIR/install-systemd-functions.sh"
write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 16 28 "$SYSTEMD_FRAGMENT"
(
    safe_env="$WORK_DIR/systemd-explicit.env"
    printf 'safe\n' > "$safe_env"
    HERDR_RELAY_ENV="$safe_env"
    # shellcheck source=/dev/null
    . "$SYSTEMD_FRAGMENT"
    test "$ENV_FILE" = "$safe_env"
)
systemd_unsafe_env="$WORK_DIR/systemd-unsafe.env"
ln -s "$WORK_DIR/missing-systemd-env" "$systemd_unsafe_env"
if (HERDR_RELAY_ENV="$systemd_unsafe_env"; . "$SYSTEMD_FRAGMENT") >/dev/null 2>&1; then
    echo "systemd installer accepted an unsafe explicit environment" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 246 255 "$SYSTEMD_FRAGMENT"
if (
    ENV_FILE="$systemd_unsafe_env"
    ENV_DIR="$WORK_DIR"
    ENV_EXISTED=false
    ENV_BACKUP=
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an environment swapped before backup" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 256 265 "$SYSTEMD_FRAGMENT"
systemd_unsafe_token="$WORK_DIR/systemd-unsafe-token"
ln -s "$WORK_DIR/missing-systemd-token" "$systemd_unsafe_token"
if (
    TOKEN_FILE="$systemd_unsafe_token"
    ENV_DIR="$WORK_DIR"
    TOKEN_EXISTED=false
    TOKEN_BACKUP=
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an unsafe legacy token" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 267 313 "$SYSTEMD_FRAGMENT"
systemd_unsafe_unit="$WORK_DIR/systemd-unsafe.service"
ln -s "$WORK_DIR/missing-systemd-unit" "$systemd_unsafe_unit"
if (
    UNIT_FILE="$systemd_unsafe_unit"
    UNIT_DIR="$WORK_DIR"
    LABEL=herdr-mobile-relay.service
    UNIT_EXISTED=false
    UNIT_BACKUP=
    SERVICE_WAS_ACTIVE=false
    SERVICE_WAS_ENABLED=false
    LEGACY_UNIT_FILE="$WORK_DIR/missing-legacy-systemd.service"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() { return 1; }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an unsafe service definition" >&2
    exit 1
fi
if (
    UNIT_FILE="$WORK_DIR/missing-active-systemd.service"
    UNIT_DIR="$WORK_DIR"
    LABEL=herdr-mobile-relay.service
    UNIT_EXISTED=false
    UNIT_BACKUP=
    SERVICE_WAS_ACTIVE=false
    SERVICE_WAS_ENABLED=false
    LEGACY_UNIT_FILE="$WORK_DIR/missing-legacy-systemd.service"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() {
        [ "${1:-}" != --user ] || shift
        [ "${*: -1}" = "$LABEL" ]
    }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an active service without its definition" >&2
    exit 1
fi
if (
    UNIT_FILE="$WORK_DIR/snapshot-systemd.service"
    UNIT_DIR="$WORK_DIR"
    LABEL=herdr-mobile-relay.service
    printf 'previous\n' > "$UNIT_FILE"
    UNIT_EXISTED=false
    UNIT_BACKUP=
    SERVICE_WAS_ACTIVE=false
    SERVICE_WAS_ENABLED=false
    LEGACY_UNIT_FILE="$WORK_DIR/missing-legacy-systemd.service"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() {
        [ "${1:-}" != --user ] || shift
        [ "${*: -1}" = "$LABEL" ]
    }
    capture_previous_service_readiness() { return 1; }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an existing service without an exact readiness snapshot" >&2
    exit 1
fi
(
    UNIT_FILE="$WORK_DIR/missing-primary-systemd.service"
    UNIT_DIR="$WORK_DIR"
    LABEL=herdr-mobile-relay.service
    UNIT_EXISTED=false
    UNIT_BACKUP=
    SERVICE_WAS_ACTIVE=false
    SERVICE_WAS_ENABLED=false
    LEGACY_UNIT_FILE="$WORK_DIR/legacy-only-systemd.service"
    printf 'legacy\n' > "$LEGACY_UNIT_FILE"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    PREVIOUS_READINESS_CAPTURED=false
    systemctl() {
        [ "${1:-}" != --user ] || shift
        [ "${1:-}" = is-active ] && [ "${*: -1}" = "$LEGACY_LABEL" ]
    }
    capture_previous_service_readiness() { return 1; }
    . "$SYSTEMD_FRAGMENT"
    test "$LEGACY_WAS_ACTIVE" = true
    test "$PREVIOUS_READINESS_CAPTURED" = false
)

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 278 293 "$SYSTEMD_FRAGMENT"
systemd_legacy_unit="$WORK_DIR/legacy-systemd.service"
printf 'legacy\n' > "$systemd_legacy_unit"
(
    LEGACY_UNIT_FILE="$systemd_legacy_unit"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() {
        [ "${1:-}" != --user ] || shift
        case "${1:-}" in
            is-active|is-enabled) return 0 ;;
        esac
        return 1
    }
    . "$SYSTEMD_FRAGMENT"
    test "$LEGACY_WAS_ACTIVE" = true
    test "$LEGACY_WAS_ENABLED" = true
)
(
    LEGACY_UNIT_FILE="$systemd_legacy_unit"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() { return 1; }
    set +e
    . "$SYSTEMD_FRAGMENT"
    set -e
    test "$LEGACY_WAS_ACTIVE" = false
    test "$LEGACY_WAS_ENABLED" = false
)
systemd_unsafe_legacy_unit="$WORK_DIR/unsafe-legacy-systemd.service"
ln -s "$WORK_DIR/missing-legacy-systemd" "$systemd_unsafe_legacy_unit"
if (
    LEGACY_UNIT_FILE="$systemd_unsafe_legacy_unit"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() { return 1; }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an unsafe legacy service definition" >&2
    exit 1
fi
if (
    LEGACY_UNIT_FILE="$WORK_DIR/missing-live-legacy-systemd.service"
    LEGACY_LABEL=herdr-remote.service
    LEGACY_WAS_ACTIVE=false
    LEGACY_WAS_ENABLED=false
    systemctl() {
        [ "${1:-}" != --user ] || shift
        [ "${1:-}" = is-active ]
    }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an active legacy service without its definition" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 315 323 "$SYSTEMD_FRAGMENT"
(
    LEGACY_WAS_ACTIVE=true
    LEGACY_STOPPED=false
    LEGACY_LABEL=herdr-remote.service
    systemctl() {
        [ "${1:-}" != --user ] || shift
        case "${1:-}" in
            stop|is-active) return 1 ;;
        esac
        return 0
    }
    . "$SYSTEMD_FRAGMENT"
    test "$LEGACY_STOPPED" = true
)
if (
    LEGACY_WAS_ACTIVE=true
    LEGACY_STOPPED=false
    LEGACY_LABEL=herdr-remote.service
    systemctl() {
        [ "${1:-}" != --user ] || shift
        return 0
    }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted a legacy service that remained active" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 315 332 "$SYSTEMD_FRAGMENT"
if (
    service_transaction_changed=false
    ENV_DIR="$WORK_DIR/systemd-missing-wrapper-config"
    ENV_FILE="$ENV_DIR/relay.env"
    CLOUDFLARED_CONFIG="$WORK_DIR/cloudflared.yml"
    SERVICE_WRAPPER="$WORK_DIR/missing-systemd-wrapper"
    WORK_DIR="$WORK_DIR/systemd-release"
    LEGACY_WAS_ACTIVE=false
    ensure_relay_env() { return 0; }
    load_relay_env() { return 0; }
    verified_installed_release() { return 0; }
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted a verified release without its service wrapper" >&2
    exit 1
fi

write_service_fragment "$ROOT_DIR/relay/install-systemd-user-service.sh" 357 363 "$SYSTEMD_FRAGMENT"
if (
    UNIT_TEMP="$WORK_DIR/invalid-candidate.service"
    WORK_DIR="$WORK_DIR/systemd-work"
    ENV_FILE="$WORK_DIR/systemd-env"
    SERVICE_WRAPPER="$WORK_DIR/systemd-wrapper"
    printf 'invalid\n' > "$UNIT_TEMP"
    . "$SYSTEMD_FRAGMENT"
) >/dev/null 2>&1; then
    echo "systemd installer accepted an invalid generated definition" >&2
    exit 1
fi

printf 'supervisor service wrapper tests passed\n'

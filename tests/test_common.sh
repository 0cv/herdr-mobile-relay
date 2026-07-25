#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-common-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"

id() {
    if [ "${1:-}" = "-u" ]; then
        printf '0\n'
        return
    fi
    command id "$@"
}
if require_user_service_context >/dev/null 2>&1; then
    echo "user service management unexpectedly accepted root" >&2
    exit 1
fi
unset -f id

DEV_RELAY_BIN="$WORK_DIR/dev/herdr-mobile-relay"
mkdir -p "$(dirname "$DEV_RELAY_BIN")"
printf '#!/bin/sh\nexit 0\n' > "$DEV_RELAY_BIN"
chmod 700 "$DEV_RELAY_BIN"
test "$(HERDR_RELAY_BIN="$DEV_RELAY_BIN" relay_binary)" = "$DEV_RELAY_BIN"

PACKAGED_RELEASE="$WORK_DIR/releases/0.0.0-test"
mkdir -p "$PACKAGED_RELEASE/relay"
cp "$REPO_DIR/relay/common.sh" "$PACKAGED_RELEASE/relay/common.sh"
printf '{}\n' > "$PACKAGED_RELEASE/release-manifest.json"
printf '#!/bin/sh\nexit 0\n' > "$PACKAGED_RELEASE/herdr-mobile-relay"
chmod 700 "$PACKAGED_RELEASE/herdr-mobile-relay"
PACKAGED_BINARY="$(
    HERDR_RELAY_BIN="$DEV_RELAY_BIN" \
        bash -c '. "$1"; relay_binary' _ "$PACKAGED_RELEASE/relay/common.sh"
)"
test "$PACKAGED_BINARY" = "$PACKAGED_RELEASE/herdr-mobile-relay"

ENV_FILE="$WORK_DIR/config/relay.env"
mkdir -p "$(dirname "$ENV_FILE")"
GH_TOKEN="test-private-token"
export GH_TOKEN
ensure_relay_env "$ENV_FILE"

if grep -q '^GH_TOKEN=' "$ENV_FILE"; then
    echo "relay.env exposed GH_TOKEN" >&2
    exit 1
fi
TOKEN_FILE="$(env_file_value "$ENV_FILE" HERDR_GITHUB_TOKEN_FILE)"
test "$TOKEN_FILE" = "$WORK_DIR/config/github-token"
test "$(cat "$TOKEN_FILE")" = "$GH_TOKEN"
if stat -c '%a' "$TOKEN_FILE" >/dev/null 2>&1; then
    mode="$(stat -c '%a' "$TOKEN_FILE")"
else
    mode="$(stat -f '%Lp' "$TOKEN_FILE")"
fi
test "$mode" = "600"

FAKE_PLIST_BUDDY="$WORK_DIR/PlistBuddy"
PLIST_LOG="$WORK_DIR/plist.log"
cat > "$FAKE_PLIST_BUDDY" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$PLIST_LOG"
EOF
chmod 700 "$FAKE_PLIST_BUDDY"
export PLIST_LOG
HERDR_PLIST_BUDDY="$FAKE_PLIST_BUDDY"
export HERDR_PLIST_BUDDY
touch "$WORK_DIR/service.plist"
update_launchd_release_paths "$WORK_DIR/service.plist" \
    "$WORK_DIR/releases/current/relay/herdr-mobile-relay-service.sh" \
    "$WORK_DIR/releases/current" \
    "$WORK_DIR/config/relay.env"
grep -F "Set :ProgramArguments:0 $WORK_DIR/releases/current/relay/herdr-mobile-relay-service.sh" "$PLIST_LOG" >/dev/null
grep -F "Set :WorkingDirectory $WORK_DIR/releases/current" "$PLIST_LOG" >/dev/null
grep -F "Set :EnvironmentVariables:HERDR_RELAY_ENV $WORK_DIR/config/relay.env" "$PLIST_LOG" >/dev/null

FAKE_LAUNCHCTL_DIR="$WORK_DIR/launchctl-bin"
LAUNCHCTL_LOG="$WORK_DIR/launchctl.log"
LAUNCHCTL_STATE="$WORK_DIR/launchctl.state"
LAUNCHCTL_UNLOAD_PENDING="$WORK_DIR/launchctl-unload-pending"
mkdir -p "$FAKE_LAUNCHCTL_DIR"
touch "$LAUNCHCTL_STATE"
cat > "$FAKE_LAUNCHCTL_DIR/launchctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$LAUNCHCTL_LOG"
case "$1" in
    print)
        if [ -f "$LAUNCHCTL_UNLOAD_PENDING" ]; then
            rm -f "$LAUNCHCTL_UNLOAD_PENDING" "$LAUNCHCTL_STATE"
            exit 0
        fi
        [ -f "$LAUNCHCTL_STATE" ]
        ;;
    bootout)
        touch "$LAUNCHCTL_UNLOAD_PENDING"
        ;;
    bootstrap)
        touch "$LAUNCHCTL_STATE"
        ;;
esac
EOF
cat > "$FAKE_LAUNCHCTL_DIR/sleep" <<'EOF'
#!/bin/sh
printf 'sleep %s\n' "$*" >> "$LAUNCHCTL_LOG"
EOF
chmod 700 "$FAKE_LAUNCHCTL_DIR/launchctl" "$FAKE_LAUNCHCTL_DIR/sleep"
export LAUNCHCTL_LOG LAUNCHCTL_STATE LAUNCHCTL_UNLOAD_PENDING
PATH="$FAKE_LAUNCHCTL_DIR:$PATH" reload_launchd_service_definition \
    "$WORK_DIR/service.plist" "com.herdr-mobile-relay.service"
LAUNCHD_DOMAIN="gui/$(id -u)"
sed -n '1p' "$LAUNCHCTL_LOG" |
    grep -Fx "print $LAUNCHD_DOMAIN/com.herdr-mobile-relay.service" >/dev/null
sed -n '2p' "$LAUNCHCTL_LOG" |
    grep -Fx "bootout $LAUNCHD_DOMAIN $WORK_DIR/service.plist" >/dev/null
sed -n '3p' "$LAUNCHCTL_LOG" |
    grep -Fx "print $LAUNCHD_DOMAIN/com.herdr-mobile-relay.service" >/dev/null
sed -n '4p' "$LAUNCHCTL_LOG" |
    grep -Fx "sleep 1" >/dev/null
sed -n '5p' "$LAUNCHCTL_LOG" |
    grep -Fx "print $LAUNCHD_DOMAIN/com.herdr-mobile-relay.service" >/dev/null
sed -n '6p' "$LAUNCHCTL_LOG" |
    grep -Fx "bootstrap $LAUNCHD_DOMAIN $WORK_DIR/service.plist" >/dev/null
sed -n '7p' "$LAUNCHCTL_LOG" |
    grep -Fx "enable $LAUNCHD_DOMAIN/com.herdr-mobile-relay.service" >/dev/null
sed -n '8p' "$LAUNCHCTL_LOG" |
    grep -Fx "kickstart -k $LAUNCHD_DOMAIN/com.herdr-mobile-relay.service" >/dev/null
test "$(wc -l < "$LAUNCHCTL_LOG" | tr -d ' ')" = "8"

HEALTH='{"status":"ok","release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
verify_relay_release_health "$HEALTH" "0.9.0" "abc123" "web456"
if verify_relay_release_health "$HEALTH" "0.9.0" "wrong" "web456"; then
    echo "release health accepted the wrong revision" >&2
    exit 1
fi

HEALTH_ATTEMPTS="$WORK_DIR/health-attempts"
cat > "$FAKE_LAUNCHCTL_DIR/curl" <<'EOF'
#!/bin/sh
attempt=0
if [ -f "$HEALTH_ATTEMPTS" ]; then
    attempt="$(cat "$HEALTH_ATTEMPTS")"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$HEALTH_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    printf '%s\n' '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"old","bundle_hash":"old-web"}'
else
    printf '%s\n' '{"status":"ok","instance":"test","version":"0.9.0","protocol":2,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
fi
EOF
chmod 700 "$FAKE_LAUNCHCTL_DIR/curl"
export HEALTH_ATTEMPTS
EXACT_HEALTH="$(
    PATH="$FAKE_LAUNCHCTL_DIR:$PATH" \
        wait_for_relay_release_health 8375 3 1 "0.9.0" "abc123" "web456"
)"
test "$(json_string_field "$EXACT_HEALTH" release_version)" = "0.9.0"
test "$(cat "$HEALTH_ATTEMPTS")" = "2"

echo "common shell tests passed"

#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TUNNEL_UUID="11111111-2222-4333-8444-555555555555"
CASES=""
TEST_COUNT=0

cleanup() {
    # shellcheck disable=SC2086
    rm -rf $CASES
}
trap cleanup EXIT

fail() {
    echo "not ok - $1" >&2
    exit 1
}

pass() {
    TEST_COUNT=$((TEST_COUNT + 1))
    echo "ok $TEST_COUNT - $1"
}

assert_contains() {
    local file="$1"
    local expected="$2"
    grep -Fq -- "$expected" "$file" || {
        sed -n '1,240p' "$file" >&2
        fail "expected '$expected' in $file"
    }
}

assert_not_contains() {
    local file="$1"
    local unexpected="$2"
    if grep -Fq -- "$unexpected" "$file"; then
        sed -n '1,240p' "$file" >&2
        fail "did not expect '$unexpected' in $file"
    fi
}

write_stubs() {
    cat > "$BIN/uname" <<'EOF'
#!/bin/sh
echo Linux
EOF
    cat > "$BIN/hostname" <<'EOF'
#!/bin/sh
echo workstation
EOF
    cat > "$BIN/herdr" <<'EOF'
#!/bin/sh
exit 0
EOF
    cat > "$BIN/uv" <<'EOF'
#!/bin/bash
printf 'uv %s\n' "$*" >> "$STUB_LOG"
case "$*" in
    *urllib.parse.urlencode*) echo 'setup=fake-token&label=workstation' ;;
    *urllib.parse.urlsplit*)
        value="${!#}"
        echo "${value%/}"
        ;;
esac
exit 0
EOF
    cat > "$BIN/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$STUB_LOG"
exit 0
EOF
    cat > "$BIN/cloudflared" <<'EOF'
#!/bin/bash
set -e
printf 'cloudflared %s\n' "$*" >> "$STUB_LOG"
args=" $* "
case "$args" in
    *" ingress validate "*)
        [ "${STUB_INGRESS_FAIL:-0}" != 1 ] || exit 1
        exit 0
        ;;
    *" list "*)
        if [ "${STUB_LOGIN_REQUIRED:-0}" = 1 ] && [ ! -f "$STUB_LOGIN_MARKER" ]; then
            echo 'ERR Missing origin certificate' >&2
            exit 1
        fi
        if [ -n "${STUB_LIST_JSON:-}" ] && [ -f "$STUB_LIST_JSON" ]; then
            cat "$STUB_LIST_JSON"
        else
            echo '[]'
        fi
        exit 0
        ;;
    *" login "*)
        echo 'Please open https://dash.cloudflare.com/argotunnel?callback=test'
        touch "$STUB_LOGIN_MARKER"
        exit 0
        ;;
    *" create "*)
        [ "${STUB_CREATE_FAIL:-0}" != 1 ] || exit 1
        credentials=''
        name=''
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --credentials-file)
                    credentials="$2"
                    shift 2
                    ;;
                --output)
                    shift 2
                    ;;
                tunnel|create)
                    shift
                    ;;
                *)
                    name="$1"
                    shift
                    ;;
            esac
        done
        mkdir -p "$(dirname "$credentials")"
        printf '{"AccountTag":"account","TunnelID":"%s","TunnelSecret":"secret"}\n' "$STUB_TUNNEL_UUID" > "$credentials"
        printf '{"id":"%s","name":"%s"}\n' "$STUB_TUNNEL_UUID" "$name"
        exit 0
        ;;
    *" route dns "*)
        if [ "${STUB_ROUTE_FAIL:-0}" = 1 ]; then
            echo 'API error: zone authorization failed for test zone' >&2
            exit 1
        fi
        touch "$STUB_ROUTE_MARKER"
        exit 0
        ;;
    *" delete "*)
        if [ "${STUB_DELETE_FAIL:-0}" = 1 ]; then
            echo 'ERR Cannot determine default origin certificate path' >&2
            exit 1
        fi
        rm -f "$STUB_ROUTE_MARKER"
        exit 0
        ;;
esac
echo "unexpected cloudflared invocation: $*" >&2
exit 2
EOF
    cat > "$BIN/curl" <<'EOF'
#!/bin/bash
url="${!#}"
printf 'curl %s\n' "$url" >> "$STUB_LOG"
case "$url" in
    http://127.0.0.1:*/healthz)
        echo '{"status": "ok", "instance": "instance-a", "version": "abc1234", "protocol": 1}'
        ;;
    https://cloudflare-dns.com/*)
        case "${STUB_DNS_MODE:-route}" in
            always|occupied|persists) echo '{"Status":0,"Answer":[{"type":1,"data":"192.0.2.1"}]}' ;;
            never) echo '{"Status":0}' ;;
            route)
                if [ -f "$STUB_ROUTE_MARKER" ]; then
                    echo '{"Status":0,"Answer":[{"type":1,"data":"192.0.2.1"}]}'
                else
                    echo '{"Status":0}'
                fi
                ;;
        esac
        ;;
    https://*/healthz)
        case "${STUB_HTTP_MODE:-success}" in
            success) echo '{"status": "ok", "instance": "instance-a", "version": "abc1234", "protocol": 1}' ;;
            mismatch) echo '{"status": "ok", "instance": "other-instance", "version": "abc1234", "protocol": 1}' ;;
            fail) exit 22 ;;
        esac
        ;;
    *)
        echo "unexpected curl URL: $url" >&2
        exit 22
        ;;
esac
EOF
    chmod 700 "$BIN"/*
}

new_case() {
    CASE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-stable-test.XXXXXX")"
    CASE_DIR="$(cd "$CASE_DIR" && pwd -P)"
    CASES="$CASES $CASE_DIR"
    HOME="$CASE_DIR/home"
    BIN="$HOME/.local/bin"
    OUTPUT="$CASE_DIR/output"
    STUB_LOG="$CASE_DIR/commands.log"
    mkdir -p "$HOME" "$BIN"
    : > "$STUB_LOG"
    write_stubs

    export HOME BIN STUB_LOG
    export PATH="$BIN:/usr/bin:/bin"
    export HERDR_RELAY_ENV="$HOME/relay.env"
    export HERDR_STABLE_STATE_FILE="$HOME/stable-setup.json"
    export HERDR_STABLE_PYTHON="/usr/bin/python3"
    export HERDR_STABLE_DOMAIN="example.test"
    export HERDR_STABLE_HOSTNAME="relay-workstation.example.test"
    export HERDR_STABLE_DNS_TIMEOUT=0
    export HERDR_STABLE_HTTP_TIMEOUT=0
    export HERDR_STABLE_POLL_DELAY=0
    export HERDR_STABLE_YES=1
    export HERDR_SETUP_YES=1
    export STUB_TUNNEL_UUID="$TUNNEL_UUID"
    export STUB_LOGIN_MARKER="$CASE_DIR/login-complete"
    export STUB_ROUTE_MARKER="$CASE_DIR/dns-routed"
    unset CLOUDFLARED_CONFIG DISPLAY WAYLAND_DISPLAY HERDR_PHONE_APP_URL
    unset STUB_CREATE_FAIL STUB_DELETE_FAIL STUB_DNS_MODE STUB_HTTP_MODE STUB_INGRESS_FAIL
    unset STUB_LIST_JSON STUB_LOGIN_REQUIRED STUB_ROUTE_FAIL
}

run_setup() {
    set +e
    # Keep confirmation cases deterministic when the parent test runner has a
    # TTY (for example, `make check` inside a Herdr pane).
    "$ROOT/relay/stable-setup.sh" < /dev/null > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
}

write_existing_config() {
    local port="$1"
    local config="$HOME/custom-config.yml"
    local credentials="$HOME/custom-credentials.json"
    printf '{"AccountTag":"account","TunnelID":"%s","TunnelSecret":"secret"}\n' "$TUNNEL_UUID" > "$credentials"
    cat > "$config" <<EOF
tunnel: herdr-mobile-relay-existing
credentials-file: $credentials

ingress:
  - hostname: existing.example.test
    service: http://127.0.0.1:$port
  - service: http_status:404
EOF
    cat > "$HERDR_RELAY_ENV" <<EOF
HERDR_RELAY_TOKEN=test-token
HERDR_RELAY_INSTANCE_ID=instance-a
HERDR_RELAY_PORT=$port
CLOUDFLARED_CONFIG=$config
EOF
    chmod 600 "$HERDR_RELAY_ENV" "$config" "$credentials"
}

test_success_and_alternate_port() {
    new_case
    printf 'HERDR_RELAY_PORT=8399\n' > "$HERDR_RELAY_ENV"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup success"; }
    assert_contains "$HOME/cloudflared/config.yml" 'service: http://127.0.0.1:8399'
    assert_contains "$HERDR_RELAY_ENV" 'HERDR_RELAY_INSTANCE_ID='
    assert_contains "$OUTPUT" 'Stable relay verified'
    assert_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    assert_contains "$OUTPUT" 'https://relay-workstation.example.test/#setup=fake-token'
    assert_contains "$HOME/phone-app-origin" 'https://relay-workstation.example.test'
    [ "$(/usr/bin/python3 -c 'import os, sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777))' "$HOME/phone-app-origin")" = 0o600 ] \
        || fail "phone app origin is not private"
    assert_contains "$STUB_LOG" 'cloudflared tunnel create --output json --credentials-file'
    assert_not_contains "$STUB_LOG" '--overwrite-dns'
    pass "successful creation uses the alternate relay port and prints QR only after verification"
}

test_existing_phone_app_origin() {
    new_case
    export HERDR_PHONE_APP_URL="https://app.example.test"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup with existing phone app"; }
    assert_contains "$OUTPUT" 'https://app.example.test/#setup=fake-token'
    assert_contains "$OUTPUT" 'Direct browser fallback:'
    assert_contains "$HOME/phone-app-origin" 'https://app.example.test'
    pass "guided setup records an existing installed app origin without baking it into the project"
}

test_creation_confirmation() {
    new_case
    unset HERDR_STABLE_YES
    run_setup
    [ "$STATUS" -ne 0 ] || fail "non-interactive creation without confirmation should fail"
    assert_contains "$OUTPUT" 'About to create Cloudflare resources in your account'
    assert_contains "$OUTPUT" 'herdr-mobile-relay-workstation'
    assert_contains "$OUTPUT" 'relay-workstation.example.test'
    assert_contains "$OUTPUT" 'Confirmation required. Run interactively, or set HERDR_STABLE_YES=1.'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '

    export HERDR_STABLE_YES=1
    : > "$STUB_LOG"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "confirmed stable setup"; }
    assert_contains "$STUB_LOG" ' tunnel create '
    assert_contains "$STUB_LOG" ' route dns '
    pass "new Cloudflare resources require confirmation or the explicit unattended opt-in"
}

test_existing_config_reuse() {
    new_case
    write_existing_config 8401
    checksum_before="$(cksum "$HOME/custom-config.yml")"
    export STUB_DNS_MODE=always
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "existing config reuse"; }
    [ "$checksum_before" = "$(cksum "$HOME/custom-config.yml")" ] || fail "custom config changed"
    assert_contains "$OUTPUT" 'Reusing existing Cloudflare tunnel config without modifying it'
    assert_contains "$OUTPUT" 'cert.pem is unavailable'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '
    pass "existing credentials-based config is validated and left untouched without cert.pem"
}

test_login_guidance() {
    new_case
    export STUB_LOGIN_REQUIRED=1 DISPLAY=:1
    run_setup
    [ "$STATUS" -eq 0 ] || fail "desktop login setup"
    assert_contains "$OUTPUT" 'may open it in your desktop browser'

    new_case
    export STUB_LOGIN_REQUIRED=1
    unset DISPLAY WAYLAND_DISPLAY
    run_setup
    [ "$STATUS" -eq 0 ] || fail "headless login setup"
    assert_contains "$OUTPUT" 'headless or remote session'
    assert_contains "$OUTPUT" 'open that exact URL manually'
    pass "desktop and headless Cloudflare login guidance remain explicit"
}

test_zone_failure_preserves_state() {
    new_case
    export STUB_ROUTE_FAIL=1
    run_setup
    [ "$STATUS" -ne 0 ] || fail "zone failure should fail"
    assert_contains "$OUTPUT" 'API error: zone authorization failed for test zone'
    assert_contains "$OUTPUT" 'zone selected during cloudflared tunnel login'
    assert_contains "$OUTPUT" 'Setup state was preserved'
    assert_contains "$OUTPUT" "HERDR_RELAY_ENV=$HERDR_RELAY_ENV make stable-setup"
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    [ "$(/usr/bin/python3 "$ROOT/relay/stable_state.py" get "$HERDR_STABLE_STATE_FILE" stage)" = routing_dns ] || fail "route stage not preserved"
    pass "zone authorization failures retain the original error and resumable state"
}

test_occupied_hostname() {
    new_case
    export STUB_DNS_MODE=occupied
    run_setup
    [ "$STATUS" -ne 0 ] || fail "occupied hostname should fail"
    assert_contains "$OUTPUT" 'already has a public DNS record'
    assert_contains "$OUTPUT" 'will not overwrite it'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    pass "occupied DNS is rejected before Cloudflare resources are created"
}

test_interrupted_route_resume() {
    new_case
    export STUB_ROUTE_FAIL=1
    run_setup
    [ "$STATUS" -ne 0 ] || fail "first interrupted route run"
    unset STUB_ROUTE_FAIL
    export STUB_DNS_MODE=occupied
    : > "$STUB_LOG"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "interrupted route resume"; }
    assert_contains "$OUTPUT" 'recorded hostname now resolves'
    assert_contains "$OUTPUT" 'Stable relay verified'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '
    pass "an interrupted DNS route resumes the recorded tunnel through relay identity verification"
}

test_health_mismatch_suppresses_qr() {
    new_case
    export STUB_HTTP_MODE=mismatch
    run_setup
    [ "$STATUS" -ne 0 ] || fail "health mismatch should fail"
    assert_contains "$OUTPUT" 'Public health identity did not match'
    assert_contains "$OUTPUT" 'instance does not match'
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    pass "public relay identity mismatch suppresses the phone QR"
}

test_separate_readiness_timeouts() {
    new_case
    export STUB_DNS_MODE=never
    run_setup
    [ "$STATUS" -ne 0 ] || fail "DNS timeout should fail"
    assert_contains "$OUTPUT" 'Timed out after 0 seconds waiting for public DNS'
    assert_not_contains "$OUTPUT" 'Waiting up to 0 seconds for HTTPS relay health'
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'

    new_case
    export STUB_HTTP_MODE=fail
    run_setup
    [ "$STATUS" -ne 0 ] || fail "HTTP timeout should fail"
    assert_contains "$OUTPUT" 'Waiting up to 0 seconds for public DNS'
    assert_contains "$OUTPUT" 'Timed out after 0 seconds waiting for https://'
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    pass "DNS and HTTPS readiness use independent waits and both suppress QR on timeout"
}

test_teardown_ownership_and_dns_retention() {
    new_case
    /usr/bin/python3 "$ROOT/relay/stable_state.py" init "$HERDR_STABLE_STATE_FILE" "$HERDR_RELAY_ENV"
    /usr/bin/python3 "$ROOT/relay/stable_state.py" update "$HERDR_STABLE_STATE_FILE" 'tunnel_name=someone-elses-tunnel'
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -ne 0 ] || fail "foreign tunnel teardown should fail"
    assert_contains "$OUTPUT" 'recorded tunnel name is not Herdr-owned'
    assert_not_contains "$STUB_LOG" ' tunnel delete '

    new_case
    run_setup
    [ "$STATUS" -eq 0 ] || fail "setup before failed tunnel deletion"
    export STUB_DELETE_FAIL=1
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -ne 0 ] || fail "failed tunnel deletion should fail teardown"
    assert_contains "$OUTPUT" 'Cannot determine default origin certificate path'
    assert_contains "$OUTPUT" 'If cert.pem is missing, run cloudflared tunnel login'
    [ -f "$HERDR_STABLE_STATE_FILE" ] || fail "state removed after tunnel deletion failure"

    new_case
    run_setup
    [ "$STATUS" -eq 0 ] || fail "setup before teardown"
    export STUB_DNS_MODE=persists
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -ne 0 ] || fail "remaining DNS should be reported"
    assert_contains "$OUTPUT" 'DNS record for relay-workstation.example.test still exists'
    assert_contains "$OUTPUT" 'Cloudflare dashboard'
    [ -f "$HERDR_STABLE_STATE_FILE" ] || fail "diagnostic state was removed"
    pass "teardown refuses foreign ownership and retains diagnosis when DNS remains"
}

echo "1..11"
test_success_and_alternate_port
test_existing_phone_app_origin
test_creation_confirmation
test_existing_config_reuse
test_login_guidance
test_zone_failure_preserves_state
test_occupied_hostname
test_interrupted_route_resume
test_health_mismatch_suppresses_qr
test_separate_readiness_timeouts
test_teardown_ownership_and_dns_retention

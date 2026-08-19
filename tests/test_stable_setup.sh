#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TUNNEL_UUID="11111111-2222-4333-8444-555555555555"
CASES=""
TEST_COUNT=0
TEST_BIN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-stable-bin.XXXXXX")"
TEST_RELAY_BIN="$TEST_BIN_DIR/herdr-mobile-relay"
GOMODCACHE="${GOMODCACHE:-${TMPDIR:-/tmp}/herdr-mobile-relay-go-mod}" \
    GOCACHE="${GOCACHE:-${TMPDIR:-/tmp}/herdr-mobile-relay-go-cache}" \
    go build -o "$TEST_RELAY_BIN" "$ROOT/cmd/herdr-mobile-relay"

cleanup() {
    # shellcheck disable=SC2086
    rm -rf $CASES
    rm -rf "$TEST_BIN_DIR"
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
        elif [ -f "$STUB_CREATED_TUNNEL_MARKER" ]; then
            cat "$STUB_CREATED_TUNNEL_MARKER"
        else
            echo 'null'
        fi
        exit 0
        ;;
    *" login "*)
        echo 'Please open https://dash.cloudflare.com/argotunnel?callback=test'
        touch "$STUB_LOGIN_MARKER"
        if [ -n "${STUB_LOGIN_ZONE_ID:-}" ]; then
            mkdir -p "$HOME/.cloudflared"
            {
                printf '%s\n' '-----BEGIN ARGO TUNNEL TOKEN-----'
                printf '{"zoneID":"%s","apiToken":"test-token","accountID":"test-account"}' "$STUB_LOGIN_ZONE_ID" | base64 | tr -d '\n'
                printf '\n%s\n' '-----END ARGO TUNNEL TOKEN-----'
            } > "$HOME/.cloudflared/cert.pem"
        fi
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
        printf '[{"id":"%s","name":"%s"}]\n' "$STUB_TUNNEL_UUID" "$name" > "$STUB_CREATED_TUNNEL_MARKER"
        printf '{"id":"%s","name":"%s"}\n' "$STUB_TUNNEL_UUID" "$name"
        exit 0
        ;;
    *" route dns "*)
        if [ "${STUB_ROUTE_FAIL:-0}" = 1 ]; then
            echo 'API error: zone authorization failed for test zone' >&2
            exit 1
        fi
        route_name="${STUB_ROUTE_NAME:-${!#}}"
        printf '%s\n' "$route_name" > "$STUB_ROUTE_MARKER"
        echo "INF Added CNAME $route_name which will route to this tunnel"
        exit 0
        ;;
    *" delete "*)
        if [ "${STUB_DELETE_FAIL:-0}" = 1 ]; then
            echo 'ERR Cannot determine default origin certificate path' >&2
            exit 1
        fi
        rm -f "$STUB_ROUTE_MARKER" "$STUB_CREATED_TUNNEL_MARKER"
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
        if [ "${STUB_READY_MODE:-success}" = protocol_mismatch ]; then
            echo '{"status": "ok", "readiness": "degraded", "inventory": {"state": "error", "error_code": "protocol_mismatch"}, "instance": "instance-a", "version": "abc1234", "protocol": 1}'
        else
            echo '{"status": "ok", "readiness": "ready", "inventory": {"state": "ready", "error_code": ""}, "instance": "instance-a", "version": "abc1234", "protocol": 1}'
        fi
        ;;
    http://127.0.0.1:*/readyz)
        if [ "${STUB_READY_MODE:-success}" = success ]; then
            echo '{"status": "ready", "inventory": {"state": "ready"}}'
        else
            exit 22
        fi
        ;;
    https://api.cloudflare.com/client/v4/zones/*)
        zone_id="${url##*/}"
        case "$zone_id" in
            zone-new) zone_name="${STUB_LOGIN_ZONE_NAME:-herdr-mobile.dev}" ;;
            *) zone_name="${STUB_CERT_ZONE_NAME:-example.test}" ;;
        esac
        printf '{"success":true,"result":{"id":"%s","name":"%s"}}\n' "$zone_id" "$zone_name"
        ;;
    https://cloudflare-dns.com/*)
        case "${STUB_DNS_MODE:-route}" in
            always|occupied|persists) echo '{"Status":0,"Answer":[{"type":1,"data":"192.0.2.1"}]}' ;;
            never) echo '{"Status":0}' ;;
            route)
                query_name="${url#*name=}"
                query_name="${query_name%%&*}"
                if [ -f "$STUB_ROUTE_MARKER" ] &&
                   [ "$(cat "$STUB_ROUTE_MARKER")" = "$query_name" ]; then
                    echo '{"Status":0,"Answer":[{"type":1,"data":"192.0.2.1"}]}'
                else
                    echo '{"Status":0}'
                fi
                ;;
        esac
        ;;
    https://*/manifest.webmanifest)
        if [ "${STUB_APP_ORIGIN:-}" = "${url%/manifest.webmanifest}" ]; then
            echo '{"name":"Herdr Mobile Relay"}'
        else
            exit 22
        fi
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

write_origin_cert() {
    local zone_id="$1"

    mkdir -p "$HOME/.cloudflared"
    {
        printf '%s\n' '-----BEGIN ARGO TUNNEL TOKEN-----'
        printf '{"zoneID":"%s","apiToken":"test-token","accountID":"test-account"}' "$zone_id" | base64 | tr -d '\n'
        printf '\n%s\n' '-----END ARGO TUNNEL TOKEN-----'
    } > "$HOME/.cloudflared/cert.pem"
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
    export HERDR_RELAY_BIN="$TEST_RELAY_BIN"
    export HERDR_STABLE_STATE_FILE="$HOME/stable-setup.json"
    export HERDR_STABLE_DOMAIN="example.test"
    export HERDR_STABLE_HOSTNAME="relay-workstation.example.test"
    export HERDR_STABLE_DNS_TIMEOUT=0
    export HERDR_STABLE_HTTP_TIMEOUT=0
    export HERDR_STABLE_READY_TIMEOUT=0
    export HERDR_STABLE_POLL_DELAY=0
    export HERDR_STABLE_YES=1
    export HERDR_SETUP_YES=1
    export STUB_TUNNEL_UUID="$TUNNEL_UUID"
    export STUB_LOGIN_MARKER="$CASE_DIR/login-complete"
    export STUB_ROUTE_MARKER="$CASE_DIR/dns-routed"
    export STUB_CREATED_TUNNEL_MARKER="$CASE_DIR/created-tunnel.json"
    unset CLOUDFLARED_CONFIG DISPLAY WAYLAND_DISPLAY HERDR_PHONE_APP_URL
    unset HERDR_APP_DEPLOY_ORIGIN HERDR_STABLE_REUSE_CONFIG HERDR_STABLE_RELOGIN
    unset STUB_APP_ORIGIN STUB_CREATE_FAIL STUB_DELETE_FAIL STUB_DNS_MODE
    unset STUB_HTTP_MODE STUB_READY_MODE STUB_INGRESS_FAIL
    unset STUB_LIST_JSON STUB_LOGIN_REQUIRED STUB_ROUTE_FAIL STUB_ROUTE_NAME
    unset STUB_CERT_ZONE_NAME STUB_LOGIN_ZONE_ID STUB_LOGIN_ZONE_NAME
}

run_setup() {
    set +e
    # Keep confirmation cases deterministic when the parent test runner has a
    # TTY (for example, `make check` inside a Herdr pane).
    "$ROOT/relay/stable-setup.sh" < /dev/null > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
}

run_setup_with_input() {
    local input="$1"

    set +e
    printf '%b' "$input" | "$ROOT/relay/stable-setup.sh" > "$OUTPUT" 2>&1
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
    printf 'HERDR_RELAY_PORT=8399\nHERDR_RELAY_TOKEN=fake-token\n' > "$HERDR_RELAY_ENV"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup success"; }
    assert_contains "$HOME/cloudflared/config.yml" 'service: http://127.0.0.1:8399'
    assert_contains "$HERDR_RELAY_ENV" 'HERDR_RELAY_INSTANCE_ID='
    assert_contains "$OUTPUT" 'Stable relay verified'
    assert_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    assert_contains "$OUTPUT" 'https://relay-workstation.example.test/#label=workstation&relay=wss%3A%2F%2Frelay-workstation.example.test&setup=fake-token'
    assert_contains "$HOME/phone-app-origin" 'https://relay-workstation.example.test'
    MODE="$(stat -c '%a' "$HOME/phone-app-origin" 2>/dev/null || stat -f '%Lp' "$HOME/phone-app-origin")"
    [ "$MODE" = 600 ] \
        || fail "phone app origin is not private"
    assert_contains "$STUB_LOG" 'cloudflared tunnel create --output json --credentials-file'
    assert_not_contains "$STUB_LOG" '--overwrite-dns'
    pass "successful creation uses the alternate relay port and prints QR only after verification"
}

test_existing_phone_app_origin() {
    new_case
    printf 'HERDR_RELAY_TOKEN=fake-token\n' > "$HERDR_RELAY_ENV"
    export HERDR_PHONE_APP_URL="https://app.example.test"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup with existing phone app"; }
    assert_contains "$OUTPUT" 'https://app.example.test/#label=workstation&relay=wss%3A%2F%2Frelay-workstation.example.test&setup=fake-token'
    assert_contains "$OUTPUT" 'Direct browser fallback:'
    assert_contains "$HOME/phone-app-origin" 'https://app.example.test'
    pass "guided setup records an existing installed app origin without baking it into the project"
}

test_deployed_phone_app_origin() {
    new_case
    cat > "$HERDR_RELAY_ENV" <<'EOF'
HERDR_RELAY_TOKEN=fake-token
HERDR_APP_DEPLOY_ORIGIN=https://app.example.test
EOF
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup with deployed phone app"; }
    assert_contains "$OUTPUT" 'https://app.example.test/#label=workstation&relay=wss%3A%2F%2Frelay-workstation.example.test&setup=fake-token'
    assert_contains "$HOME/phone-app-origin" 'https://app.example.test'
    pass "stable setup reuses the configured deployment origin"
}

test_discovered_phone_app_origin() {
    new_case
    printf 'HERDR_RELAY_TOKEN=fake-token\n' > "$HERDR_RELAY_ENV"
    write_origin_cert zone-old
    export STUB_APP_ORIGIN="https://herdr.example.test"
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "stable setup with discovered phone app"; }
    assert_contains "$OUTPUT" 'https://herdr.example.test/#label=workstation&relay=wss%3A%2F%2Frelay-workstation.example.test&setup=fake-token'
    assert_contains "$OUTPUT" 'Direct browser fallback:'
    assert_contains "$OUTPUT" 'https://relay-workstation.example.test/#label=workstation&relay=wss%3A%2F%2Frelay-workstation.example.test&setup=fake-token'
    assert_contains "$HOME/phone-app-origin" 'https://herdr.example.test'
    pass "stable setup discovers the shared phone app without changing the relay endpoint"
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
    [ "$STATUS" -ne 0 ] || fail "unconfirmed existing config reuse should fail"
    assert_contains "$OUTPUT" 'An existing Cloudflare relay config was found'
    assert_contains "$OUTPUT" 'Explicit confirmation is required before reusing this config'
    assert_contains "$OUTPUT" 'existing.example.test'
    [ ! -f "$HERDR_STABLE_STATE_FILE" ] || fail "declined config reuse left adoption state"
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '

    export HERDR_STABLE_REUSE_CONFIG=1
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "confirmed existing config reuse"; }
    [ "$checksum_before" = "$(cksum "$HOME/custom-config.yml")" ] || fail "custom config changed"
    assert_contains "$OUTPUT" 'Reusing confirmed Cloudflare tunnel config without modifying it'
    assert_contains "$OUTPUT" 'cert.pem is unavailable'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '
    pass "existing config requires explicit reuse and remains untouched"
}

test_cloudflare_zone_selection_and_route_verification() {
    new_case
    unset HERDR_STABLE_DOMAIN HERDR_STABLE_HOSTNAME
    write_origin_cert zone-old
    export STUB_CERT_ZONE_NAME=example.test
    run_setup_with_input '\n\n'
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "preloaded Cloudflare domain"; }
    assert_contains "$OUTPUT" 'Cloudflare domain authorized by the current login'
    assert_contains "$OUTPUT" '1. example.test'
    assert_contains "$HOME/cloudflared/config.yml" 'hostname: relay-workstation.example.test'

    new_case
    write_origin_cert zone-old
    export STUB_CERT_ZONE_NAME=150283.xyz
    export HERDR_STABLE_DOMAIN=herdr-mobile.dev
    export HERDR_STABLE_HOSTNAME=herdr-mac.herdr-mobile.dev
    export HERDR_STABLE_RELOGIN=false
    run_setup
    [ "$STATUS" -ne 0 ] || fail "wrong-zone certificate should fail before creation"
    assert_contains "$OUTPUT" 'current Cloudflare login authorizes 150283.xyz'
    assert_contains "$OUTPUT" 'Continuing would create herdr-mac.herdr-mobile.dev.150283.xyz instead'
    assert_not_contains "$STUB_LOG" ' tunnel create '

    new_case
    write_origin_cert zone-old
    export STUB_CERT_ZONE_NAME=example.test
    export STUB_ROUTE_NAME=relay-workstation.example.test.150283.xyz
    run_setup
    [ "$STATUS" -ne 0 ] || fail "misrouted hostname should be rejected"
    assert_contains "$OUTPUT" 'Cloudflare created relay-workstation.example.test.150283.xyz, not relay-workstation.example.test'
    assert_contains "$OUTPUT" 'Delete relay-workstation.example.test.150283.xyz in Cloudflare DNS'

    "$TEST_RELAY_BIN" stable-state update "$HERDR_STABLE_STATE_FILE" \
        'stage=waiting_for_dns' \
        'hostname=herdr-mac.herdr-mobile.dev' \
        'created_dns=true' \
        'misrouted_hostname='
    printf '%s\n' 'herdr-mac.herdr-mobile.dev.150283.xyz' > "$STUB_ROUTE_MARKER"
    write_origin_cert zone-old
    export STUB_CERT_ZONE_NAME=150283.xyz
    export HERDR_STABLE_DOMAIN=herdr-mobile.dev
    export HERDR_STABLE_HOSTNAME=herdr-mac.herdr-mobile.dev
    unset STUB_ROUTE_NAME
    run_setup
    [ "$STATUS" -ne 0 ] || fail "legacy misrouted DNS should block recovery"
    assert_contains "$OUTPUT" 'An earlier Cloudflare route created the wrong hostname'
    assert_contains "$OUTPUT" 'herdr-mac.herdr-mobile.dev.150283.xyz'

    rm -f "$STUB_ROUTE_MARKER"
    export HERDR_STABLE_RELOGIN=1
    export STUB_LOGIN_ZONE_ID=zone-new
    export STUB_LOGIN_ZONE_NAME=herdr-mobile.dev
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "wrong-zone recovery"; }
    assert_contains "$OUTPUT" 'previous misrouted DNS record is gone'
    assert_contains "$OUTPUT" 'Cloudflare authorized herdr-mobile.dev'
    assert_contains "$HOME/cloudflared/config.yml" 'hostname: herdr-mac.herdr-mobile.dev'
    assert_contains "$STUB_LOG" ' tunnel login'
    pass "Cloudflare domain selection prevents and repairs wrong-zone DNS routes"
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

test_malformed_tunnel_list_stops_before_prompt() {
    new_case
    unset HERDR_STABLE_HOSTNAME
    STUB_LIST_JSON="$CASE_DIR/tunnels.json"
    printf '{}\n' > "$STUB_LIST_JSON"
    export STUB_LIST_JSON
    run_setup
    [ "$STATUS" -ne 0 ] || fail "malformed tunnel list should fail"
    assert_contains "$OUTPUT" 'Cloudflare tunnel list output was not a JSON list'
    [ "$(grep -Fc -- 'Cloudflare tunnel list output was not a JSON list' "$OUTPUT")" -eq 1 ] \
        || fail "malformed tunnel list error should be reported once"
    assert_not_contains "$OUTPUT" 'Public hostname'
    assert_not_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$STUB_LOG" ' route dns '
    pass "malformed tunnel lists stop setup before prompting or creating resources"
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
    [ "$("$TEST_RELAY_BIN" stable-state get "$HERDR_STABLE_STATE_FILE" stage)" = routing_dns ] || fail "route stage not preserved"
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

test_inventory_failure_suppresses_qr() {
    new_case
    export STUB_READY_MODE=protocol_mismatch
    run_setup
    [ "$STATUS" -ne 0 ] || fail "inventory mismatch should fail"
    assert_contains "$OUTPUT" 'Herdr agent inventory is unavailable'
    assert_contains "$OUTPUT" 'herdr server live-handoff'
    assert_not_contains "$OUTPUT" 'Herdr Mobile Relay phone setup'
    pass "agent inventory failure is actionable and suppresses the phone QR"
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
    "$TEST_RELAY_BIN" stable-state init "$HERDR_STABLE_STATE_FILE" "$HERDR_RELAY_ENV"
    "$TEST_RELAY_BIN" stable-state update "$HERDR_STABLE_STATE_FILE" 'tunnel_name=someone-elses-tunnel'
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -ne 0 ] || fail "foreign tunnel teardown should fail"
    assert_contains "$OUTPUT" 'recorded tunnel is outside the Herdr stable-tunnel namespace'
    assert_not_contains "$STUB_LOG" ' tunnel delete '

    new_case
    write_existing_config 8401
    sed 's/herdr-mobile-relay-existing/someone-elses-tunnel/' "$HOME/custom-config.yml" > "$HOME/foreign-config.yml"
    mv "$HOME/foreign-config.yml" "$HOME/custom-config.yml"
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -ne 0 ] || fail "foreign config recovery should fail"
    assert_contains "$OUTPUT" 'config tunnel is outside the Herdr stable-tunnel namespace'
    [ -f "$HOME/custom-config.yml" ] || fail "foreign config recovery removed config"
    assert_not_contains "$STUB_LOG" ' tunnel delete '

    new_case
    write_existing_config 8401
    assert_not_contains "$STUB_LOG" ' tunnel delete '
    export STUB_DNS_MODE=never
    set +e
    HERDR_STABLE_TEARDOWN_YES=1 "$ROOT/relay/stable-teardown.sh" > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "recorded relay teardown"; }
    assert_contains "$OUTPUT" 'Recovered teardown identity from the retained Herdr Cloudflare config'
    assert_contains "$OUTPUT" 'Deleting configured stable tunnel herdr-mobile-relay-existing'
    assert_contains "$OUTPUT" 'Removed stable relay config'
    assert_contains "$OUTPUT" 'Removed stable relay credentials'
    assert_contains "$STUB_LOG" "tunnel delete --force $TUNNEL_UUID"
    [ ! -f "$HERDR_STABLE_STATE_FILE" ] || fail "recorded relay teardown retained state"
    [ ! -f "$HOME/custom-config.yml" ] || fail "recorded relay teardown retained config"
    [ ! -f "$HOME/custom-credentials.json" ] || fail "recorded relay teardown retained credentials"
    assert_not_contains "$HERDR_RELAY_ENV" 'CLOUDFLARED_CONFIG='

    export STUB_DNS_MODE=route
    run_setup
    [ "$STATUS" -eq 0 ] || { sed -n '1,240p' "$OUTPUT" >&2; fail "clean setup after recorded teardown"; }
    assert_contains "$OUTPUT" 'About to create Cloudflare resources'
    assert_contains "$OUTPUT" 'DNS route: relay-workstation.example.test'
    assert_contains "$STUB_LOG" ' tunnel create '
    assert_not_contains "$OUTPUT" 'Reusing confirmed Cloudflare tunnel config'

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
    assert_contains "$OUTPUT" 'Cloudflare DNS records still exist'
    assert_contains "$OUTPUT" 'Cloudflare dashboard'
    [ -f "$HERDR_STABLE_STATE_FILE" ] || fail "diagnostic state was removed"
    pass "teardown protects foreign state, removes recorded relays, and retains DNS diagnosis"
}

echo "1..16"
test_success_and_alternate_port
test_existing_phone_app_origin
test_deployed_phone_app_origin
test_discovered_phone_app_origin
test_creation_confirmation
test_existing_config_reuse
test_cloudflare_zone_selection_and_route_verification
test_login_guidance
test_malformed_tunnel_list_stops_before_prompt
test_zone_failure_preserves_state
test_occupied_hostname
test_interrupted_route_resume
test_health_mismatch_suppresses_qr
test_inventory_failure_suppresses_qr
test_separate_readiness_timeouts
test_teardown_ownership_and_dns_retention

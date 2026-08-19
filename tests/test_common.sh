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

# A plugin checkout installs the release of the repository it was cloned from,
# in whichever URL form git recorded, and nothing else may pass for one.
CHECKOUT="$WORK_DIR/checkout"
git init -q "$CHECKOUT"
for REMOTE_URL in \
    "git@github.com:0cv/herdr-mobile-relay-dev.git" \
    "https://github.com/0cv/herdr-mobile-relay-dev.git" \
    "https://github.com/0cv/herdr-mobile-relay-dev" \
    "ssh://git@github.com/0cv/herdr-mobile-relay-dev.git"; do
    git -C "$CHECKOUT" remote remove origin 2>/dev/null || true
    git -C "$CHECKOUT" remote add origin "$REMOTE_URL"
    test "$(release_repository "$CHECKOUT")" = "0cv/herdr-mobile-relay-dev"
done
for REJECTED_URL in \
    "https://gitlab.com/0cv/herdr-mobile-relay-dev.git" \
    "https://github.com/0cv/herdr-mobile-relay-dev/extra" \
    "https://github.com/0cv"; do
    git -C "$CHECKOUT" remote remove origin
    git -C "$CHECKOUT" remote add origin "$REJECTED_URL"
    if release_repository "$CHECKOUT" >/dev/null 2>&1; then
        echo "release repository accepted '$REJECTED_URL'" >&2
        exit 1
    fi
done
git -C "$CHECKOUT" remote remove origin
if release_repository "$CHECKOUT" >/dev/null 2>&1; then
    echo "release repository resolved a checkout without an origin" >&2
    exit 1
fi

# A key named at a prompt is a name, not a path: "ovh" means ~/.ssh/ovh, which
# is the only place it plausibly is.
FAKE_HOME="$WORK_DIR/keyhome"
mkdir -p "$FAKE_HOME/.ssh"
printf 'key\n' > "$FAKE_HOME/.ssh/ovh"
printf 'key\n' > "$WORK_DIR/local-key"
test "$(HOME="$FAKE_HOME" ssh_key_path ovh)" = "$FAKE_HOME/.ssh/ovh"
test "$(HOME="$FAKE_HOME" ssh_key_path '~/.ssh/ovh')" = "$FAKE_HOME/.ssh/ovh"
test "$(HOME="$FAKE_HOME" ssh_key_path "$WORK_DIR/local-key")" = "$WORK_DIR/local-key"
for MISSING in absent "$WORK_DIR/absent" "~/.ssh/absent" "" ".ssh"; do
    if HOME="$FAKE_HOME" ssh_key_path "$MISSING" >/dev/null 2>&1; then
        echo "ssh key resolver accepted '$MISSING'" >&2
        exit 1
    fi
done

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

GATEWAY_HEALTH='{"status":"ok","gateway":{"enabled":true,"registered":true,"relay_id":"AAAA","clients":1}}'
test "$(gateway_registration_state "$GATEWAY_HEALTH")" = "true"
test "$(gateway_registration_state '{"gateway": {"enabled": true, "registered": false}}')" = "false"
test -z "$(gateway_registration_state '{"status":"ok"}')"

# The setup fragment carries gateway=<url> for a gateway-configured relay and
# relay=<wss url> otherwise. Both keep the token inside the fragment.
FRAGMENT_BIN="$WORK_DIR/fragment/herdr-mobile-relay"
mkdir -p "$(dirname "$FRAGMENT_BIN")"
cat > "$FRAGMENT_BIN" <<'EOF'
#!/bin/sh
# Stands in for `herdr-mobile-relay setup-fragment TOKEN LABEL [RELAY]`:
# alphabetically sorted keys with percent-encoded values.
test "$1" = "setup-fragment" || exit 2
encoded="$(printf '%s' "$4" | sed -e 's|:|%3A|g' -e 's|/|%2F|g')"
if [ -z "$4" ]; then
    printf 'label=%s&setup=%s\n' "$3" "$2"
else
    printf 'label=%s&relay=%s&setup=%s\n' "$3" "$encoded" "$2"
fi
EOF
chmod 700 "$FRAGMENT_BIN"

GATEWAY_ENV="$WORK_DIR/config/gateway.env"
printf "HERDR_GATEWAY_URL='wss://gw.example.test/'\n" > "$GATEWAY_ENV"
GATEWAY_FRAGMENT="$(
    unset HERDR_GATEWAY_URL
    HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation "wss://relay.example.test"
)"
case "$GATEWAY_FRAGMENT" in
    *"gateway=wss%3A%2F%2Fgw.example.test"*) ;;
    *)
        echo "gateway fragment lost the gateway URL: $GATEWAY_FRAGMENT" >&2
        exit 1
        ;;
esac
case "$GATEWAY_FRAGMENT" in
    *relay=*)
        echo "gateway fragment still advertises a relay URL: $GATEWAY_FRAGMENT" >&2
        exit 1
        ;;
esac
case "$GATEWAY_FRAGMENT" in
    *"setup=relay-secret-token"*) ;;
    *)
        echo "gateway fragment dropped the relay token" >&2
        exit 1
        ;;
esac

TUNNEL_FRAGMENT="$(
    unset HERDR_GATEWAY_URL
    HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$WORK_DIR/config/relay.env" \
        build_transport_setup_fragment relay-secret-token workstation "wss://relay.example.test"
)"
test "$TUNNEL_FRAGMENT" = "label=workstation&relay=wss%3A%2F%2Frelay.example.test&setup=relay-secret-token"

# The environment wins over the env file, so a one-off gateway override works.
# gateways= carries even a single entry: the phone then needs no re-scan when a
# second gateway is added later.
ENV_GATEWAY_FRAGMENT="$(
    HERDR_GATEWAY_URL="wss://other.example.test" \
        HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation ""
)"
test "$ENV_GATEWAY_FRAGMENT" = "label=workstation&gateway=wss%3A%2F%2Fother.example.test&setup=relay-secret-token&gateways=wss%3A%2F%2Fother.example.test"

# An ordered list pairs gateway=<first entry> with the complete gateways= list,
# in order, so a phone fails over to the second gateway on its own.
LIST_GATEWAY_FRAGMENT="$(
    HERDR_GATEWAY_URL="wss://a.example.test, wss://b.example.test/" \
        HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation ""
)"
test "$LIST_GATEWAY_FRAGMENT" = "label=workstation&gateway=wss%3A%2F%2Fa.example.test&setup=relay-secret-token&gateways=wss%3A%2F%2Fa.example.test,wss%3A%2F%2Fb.example.test"

test "$(HERDR_GATEWAY_URL="wss://gw.example.test/" gateway_url "$WORK_DIR/config/relay.env")" = "wss://gw.example.test"
test -z "$(unset HERDR_GATEWAY_URL; gateway_url "$WORK_DIR/config/relay.env")"

# A list is parsed in order, with blank entries and trailing slashes dropped, and
# gateway_url stays the first entry that display and gateway= use.
test "$(HERDR_GATEWAY_URL=" wss://a.example.test ,, wss://b.example.test/ ," gateway_urls "$GATEWAY_ENV")" = "wss://a.example.test,wss://b.example.test"
test "$(HERDR_GATEWAY_URL="wss://a.example.test,wss://b.example.test" gateway_url "$GATEWAY_ENV")" = "wss://a.example.test"
test -z "$(unset HERDR_GATEWAY_URL; gateway_urls "$WORK_DIR/config/relay.env")"

# The gateway URL normalizer delegates to the compiled origin normalizer, so it
# is stubbed the same way the fragment helper is above.
NORMALIZE_BIN="$WORK_DIR/normalize/herdr-mobile-relay"
mkdir -p "$(dirname "$NORMALIZE_BIN")"
cat > "$NORMALIZE_BIN" <<'EOF'
#!/bin/sh
# Stands in for `herdr-mobile-relay normalize-origin --allow-loopback-http URL`:
# a bare host defaults to HTTPS, plain HTTP is loopback-only, and credentials,
# paths, queries, and fragments are rejected.
test "$1" = "normalize-origin" || exit 2
test "$2" = "--allow-loopback-http" || exit 2
value="$3"
case "$value" in
    *://*) ;;
    *) value="https://$value" ;;
esac
scheme="${value%%://*}"
host="${value#*://}"
host="${host%/}"
case "$host" in
    ''|*/*|*\?*|*'#'*|*@*) exit 1 ;;
esac
case "$scheme" in
    https) ;;
    http)
        case "${host%%:*}" in
            localhost|127.0.0.1|::1) ;;
            *) exit 1 ;;
        esac
        ;;
    *) exit 1 ;;
esac
printf '%s://%s\n' "$scheme" "$host"
EOF
chmod 700 "$NORMALIZE_BIN"
export HERDR_RELAY_BIN="$NORMALIZE_BIN"

test "$(normalize_gateway_url gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url https://gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url wss://gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url http://127.0.0.1:8443)" = "ws://127.0.0.1:8443"
test "$(normalize_gateway_urls "gw.example.com, https://backup.example.com, wss://gw.example.com")" = \
    "wss://gw.example.com,wss://backup.example.com"
for REJECTED_LIST in "gw.example.com,gw.example.com/x" ","; do
    if normalize_gateway_urls "$REJECTED_LIST" >/dev/null 2>&1; then
        echo "gateway list normalizer accepted '$REJECTED_LIST'" >&2
        exit 1
    fi
done
for REJECTED in "gw.example.com/x" "user:pw@gw.example.com" ""; do
    if normalize_gateway_url "$REJECTED" >/dev/null 2>&1; then
        echo "gateway URL normalizer accepted '$REJECTED'" >&2
        exit 1
    fi
done
unset HERDR_RELAY_BIN

test "$(gateway_http_base wss://gw.example.test)" = "https://gw.example.test"
test "$(gateway_http_base ws://127.0.0.1:8443)" = "http://127.0.0.1:8443"

# Writing the transport choice is what the chooser does; clearing it returns the
# relay to the Cloudflare tunnel path.
CHOICE_ENV="$WORK_DIR/config/choice.env"
set_gateway_url "$CHOICE_ENV" "wss://gw.example.test"
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_URL)" = "wss://gw.example.test"
test "$(unset HERDR_GATEWAY_URL; gateway_url "$CHOICE_ENV")" = "wss://gw.example.test"

# The selection policy is a second, independent switch with exactly two legal
# values. Anything else is refused without touching the file, because writing a
# policy nobody understands would silently change which gateway carries traffic.
set_gateway_selection "$CHOICE_ENV" ordered
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_SELECTION)" = "ordered"
set_gateway_selection "$CHOICE_ENV" latency
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_SELECTION)" = "latency"
SELECTION_ENV_BEFORE="$(cat "$CHOICE_ENV")"
for REJECTED_SELECTION in "fastest" "Ordered" "ordered latency" ""; do
    if set_gateway_selection "$CHOICE_ENV" "$REJECTED_SELECTION"; then
        echo "gateway selection accepted '$REJECTED_SELECTION'" >&2
        exit 1
    fi
    test "$(cat "$CHOICE_ENV")" = "$SELECTION_ENV_BEFORE"
done

# Leaving the gateway path drops the policy along with the list, so a later
# choice cannot inherit a stale one.
set_gateway_url "$CHOICE_ENV" ""
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$CHOICE_ENV"; then
    echo "clearing the gateway URL left a gateway key behind" >&2
    exit 1
fi
test -z "$(unset HERDR_GATEWAY_URL; gateway_url "$CHOICE_ENV")"

# The community gateway is published, so an install that configures nothing gets
# the shared one; an operator overrides it, and an explicitly empty value is the
# documented way to say "this build runs no community gateway".
test "$(unset HERDR_COMMUNITY_GATEWAY_URL; community_gateway_url)" = \
    "wss://gw1.herdr-mobile.dev,wss://gw2.herdr-mobile.dev"
test "$(HERDR_COMMUNITY_GATEWAY_URL="wss://community.example.test" community_gateway_url)" = "wss://community.example.test"
test "$(HERDR_COMMUNITY_GATEWAY_URL="wss://a.example.test,wss://b.example.test" community_gateway_url)" = \
    "wss://a.example.test,wss://b.example.test"
test -z "$(HERDR_COMMUNITY_GATEWAY_URL="" community_gateway_url)"

# The installed chooser accepts the managed candidate list without asking for
# addresses, retains an unavailable cold fallback, and requires one healthy
# gateway before saving anything.
CHOOSER_BIN_DIR="$WORK_DIR/chooser-bin"
CHOOSER_ENV="$WORK_DIR/config/chooser.env"
mkdir -p "$CHOOSER_BIN_DIR"
cat > "$CHOOSER_BIN_DIR/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *gw-a.example.test/healthz*)
        printf '%s\n' '{"ok":true}'
        ;;
    *)
        exit 22
        ;;
esac
EOF
chmod 700 "$CHOOSER_BIN_DIR/curl"
CHOOSER_OUTPUT="$(
    printf '2\n' |
        PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_COMMUNITY_GATEWAY_URL="gw-a.example.test,https://gw-b.example.test" \
        bash "$REPO_DIR/relay/plugin-choose-transport.sh"
)"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_URL)" = \
    "wss://gw-a.example.test,wss://gw-b.example.test"
# The published candidates are interchangeable, so this is the one option that
# asks for latency ranking.
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_SELECTION)" = "latency"
case "$CHOOSER_OUTPUT" in
    *"gw-b.example.test.. unavailable"*"Saved 2 gateway candidates."*) ;;
    *)
        echo "community chooser did not report the saved list and unavailable fallback" >&2
        exit 1
        ;;
esac

# The setup menu opens after every install, including upgrades, so it has to
# report what exists before offering to change it: a stale phone app is exactly
# the thing a person cannot otherwise see.
MENU_BIN_DIR="$WORK_DIR/menu-bin"
MENU_ENV="$WORK_DIR/config/menu.env"
MENU_ROOT="$WORK_DIR/menu-release"
mkdir -p "$MENU_BIN_DIR" "$MENU_ROOT/current"
printf '{\n  "version": "9.9.9"\n}\n' > "$MENU_ROOT/current/release-manifest.json"
printf "HERDR_GATEWAY_URL='wss://gw-a.example.test,wss://gw-b.example.test'\n" > "$MENU_ENV"
printf 'https://app.example.test\n' > "$WORK_DIR/config/phone-app-origin"
cat > "$MENU_BIN_DIR/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *127.0.0.1*healthz*) printf '{"status":"ok","release_version":"9.9.9"}\n' ;;
    *app.example.test/version.json*) printf '{"version":"9.9.8","assets":1}\n' ;;
    *) exit 22 ;;
esac
EOF
chmod 700 "$MENU_BIN_DIR/curl"
MENU_OUTPUT="$(
    printf 'q\n' |
        PATH="$MENU_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELEASE_ROOT="$MENU_ROOT" \
        HERDR_RELAY_ENV="$MENU_ENV" \
        bash "$REPO_DIR/relay/plugin-setup-menu.sh"
)"
case "$MENU_OUTPUT" in
    *"Relay:      9.9.9 running"*) ;;
    *) echo "setup menu did not report the running release" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"gateway wss://gw-a.example.test (+1 fallback)"*) ;;
    *) echo "setup menu did not report the configured gateway list" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"serves 9.9.8, this relay ships 9.9.9"*) ;;
    *) echo "setup menu did not report the stale phone app" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Exit, change nothing"*) ;;
    *) echo "setup menu did not offer a way out" >&2; exit 1 ;;
esac

echo "common shell tests passed"

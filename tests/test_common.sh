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

# Plugin panes never source a shell profile, so node has to be found where the
# version managers put it, and a run that already recorded a directory keeps it.
NODE_HOME="$WORK_DIR/nodehome"
fake_node_dir() {
    mkdir -p "$1"
    printf '#!/bin/sh\nexit 0\n' > "$1/node"
    printf '#!/bin/sh\nexit 0\n' > "$1/npx"
    chmod 700 "$1/node" "$1/npx"
}
fake_node_dir "$NODE_HOME/.nvm/versions/node/v20.0.0/bin"
fake_node_dir "$NODE_HOME/.nvm/versions/node/v26.1.0/bin"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH=/usr/bin:/bin node_bin_dir)" = \
    "$NODE_HOME/.nvm/versions/node/v26.1.0/bin"
fake_node_dir "$NODE_HOME/.nvm/current/bin"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH=/usr/bin:/bin node_bin_dir)" = \
    "$NODE_HOME/.nvm/current/bin"

NODE_ENV_FILE="$WORK_DIR/config/node.env"
mkdir -p "$(dirname "$NODE_ENV_FILE")"
fake_node_dir "$NODE_HOME/recorded/bin"
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/recorded/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH=/usr/bin:/bin node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/recorded/bin"

# A recording that no longer resolves must not win over a working install.
rm -rf "$NODE_HOME/recorded"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH=/usr/bin:/bin node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/.nvm/current/bin"

# Both binaries or nothing: wrangler is run through npx, so a directory that
# only carries node is not an installation. This cannot assert "found nothing",
# because the system paths it also searches genuinely hold node on some hosts.
mkdir -p "$NODE_HOME/half/bin"
printf '#!/bin/sh\nexit 0\n' > "$NODE_HOME/half/bin/node"
chmod 700 "$NODE_HOME/half/bin/node"
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/half/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH=/usr/bin:/bin node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/.nvm/current/bin"

# Titles are bold on a terminal only. A pipe is not one, so logs, tests, and
# non-terminal panes keep the plain text they parse, and NO_COLOR is honoured
# even when a terminal is present.
test "$(NO_COLOR= menu_item 3 "Stable Tunnel")" = "  3. Stable Tunnel"
test "$(NO_COLOR=1 menu_item q "Exit, change nothing")" = "  q. Exit, change nothing"

# Use the same origin contract as the packaged binary without depending on an
# installed release.
PHONE_SETUP_NORMALIZER="$WORK_DIR/phone-setup-normalizer"
cat > "$PHONE_SETUP_NORMALIZER" <<'EOF'
#!/bin/sh
test "$1 $2" = "normalize-origin --allow-loopback-http" || exit 2
case "$3" in
    'https://app.example.test ') printf '%s\n' 'https://app.example.test' ;;
    https://app.example.test | http://127.0.0.1:8375) printf '%s\n' "$3" ;;
    *) exit 1 ;;
esac
EOF
chmod 700 "$PHONE_SETUP_NORMALIZER"

PHONE_SETUP_URL='https://app.example.test/#setup=relay%3A%2F%2Fmachine.example.test%3A443%3Ftoken%3Dfixture-private-token-0123456789abcdef'
run_phone_setup_helper() (
    relay_binary() { printf '%s\n' "$PHONE_SETUP_NORMALIZER"; }
    render_setup_qr() {
        [ "${PHONE_SETUP_RENDER_BAD_QR:-}" != 1 ] ||
            printf 'attacker-controlled QR\n'
    }
    stdout_is_terminal() { [ "${PHONE_SETUP_TTY:-}" = 1 ]; }
    unset NO_COLOR
    [ "${PHONE_SETUP_NO_COLOR:-}" != 1 ] || NO_COLOR=1
    "$@"
)

PHONE_SETUP_PLAIN_EXPECTED="$(
    printf '  Open this private setup link on your phone:\n  %s\n' "$PHONE_SETUP_URL"
)"
PHONE_SETUP_LINK_EXPECTED="$(
    printf '  Open this private setup link on your phone:\n'
    printf '  \033]8;;%s\033\\%s\033]8;;\033\\\n' "$PHONE_SETUP_URL" "$PHONE_SETUP_URL"
)"

# Redirects stay plain. Interactive terminals receive the vendor-neutral OSC 8
# protocol; explicit plain-output requests still win.
test "$(run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL")" = \
    "$PHONE_SETUP_PLAIN_EXPECTED"
test "$(
    PHONE_SETUP_TTY=1 TERM=xterm-256color \
        run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL"
)" = "$PHONE_SETUP_LINK_EXPECTED"
test "$(
    PHONE_SETUP_TTY=1 PHONE_SETUP_NO_COLOR=1 TERM=xterm-256color \
        run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL"
)" = "$PHONE_SETUP_PLAIN_EXPECTED"
test "$(
    PHONE_SETUP_TTY=1 TERM=dumb \
        run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL"
)" = "$PHONE_SETUP_PLAIN_EXPECTED"

run_phone_setup_helper phone_setup_url_is_safe \
    'http://127.0.0.1:8375/#setup=fixture'
for REJECTED_PHONE_SETUP_URL in \
    "${PHONE_SETUP_URL}"$'\a''bell' \
    "${PHONE_SETUP_URL}"$'\033\\''escape' \
    "${PHONE_SETUP_URL}"$'\n''line' \
    'https://app.example.test /#setup=fixture' \
    'javascript:alert(1)' \
    'http://example.test/#setup=fixture'; do
    if run_phone_setup_helper phone_setup_url_is_safe \
        "$REJECTED_PHONE_SETUP_URL"; then
        echo "setup URL validator accepted an unsafe value" >&2
        exit 1
    fi
done
unset REJECTED_PHONE_SETUP_URL

# Rejection happens before either the QR or terminal sink writes.
PHONE_SETUP_ACTUAL="$WORK_DIR/phone-setup-rejected"
PHONE_SETUP_UNSAFE="${PHONE_SETUP_URL}"$'\033\\''escape'
if PHONE_SETUP_TTY=1 PHONE_SETUP_RENDER_BAD_QR=1 run_phone_setup_helper \
    print_phone_setup "$PHONE_SETUP_UNSAFE" > "$PHONE_SETUP_ACTUAL"; then
    echo "phone setup accepted an unsafe value" >&2
    exit 1
fi
test ! -s "$PHONE_SETUP_ACTUAL"

# Once a shared app is configured, numeric choice 1 must keep it. The previous
# implicit Enter-only default made the surrounding menu's usual "1" habit
# silently switch the QR back to this relay.
test "$(phone_app_choice_action https://app.example.test 1)" = keep
test "$(phone_app_choice_action https://app.example.test '')" = keep
test "$(phone_app_choice_action https://app.example.test 2)" = relay
test "$(phone_app_choice_action https://app.example.test 3)" = existing
test "$(phone_app_choice_action '' 1)" = relay
test "$(phone_app_choice_action '' 2)" = existing

# A phone-app origin no Pages project serves used to demand a project that
# could not exist, with no way out but killing the pane.
DEPLOY_HOME="$WORK_DIR/deploy-home"
DEPLOY_BIN="$WORK_DIR/deploy-bin"
DEPLOY_ENV="$DEPLOY_HOME/relay.env"
mkdir -p "$DEPLOY_HOME" "$DEPLOY_BIN"
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
cat > "$DEPLOY_BIN/relay-stub" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "normalize-origin --allow-loopback-http")
        host="${3#https://}"
        printf 'https://%s\n' "${host#http://}"
        ;;
    "pages-projects list") printf '  herdr-0cv (herdr-0cv.pages.dev, app.example.test)\n' ;;
    "pages-projects names") printf 'herdr-0cv\n' ;;
    "pages-projects matching")
        if [ "$3" = "https://app.example.test" ] ||
            { [ -f "$DEPLOY_ATTACHED" ] && [ "$3" = "https://$(cat "$DEPLOY_ATTACHED")" ]; }; then
            printf 'herdr-0cv\n'
        fi
        exit 0
        ;;
    "pages-projects validate")
        test "$3" = herdr-0cv || exit 1
        test "$4" = "https://app.example.test" ||
            { [ -f "$DEPLOY_ATTACHED" ] && [ "$4" = "https://$(cat "$DEPLOY_ATTACHED")" ]; }
        ;;
    *) exit 2 ;;
esac
EOF
chmod 700 "$DEPLOY_BIN/relay-stub"
printf '#!/bin/sh\nprintf "{}\\n"\n' > "$DEPLOY_BIN/npx"
printf '#!/bin/sh\nexit 0\n' > "$DEPLOY_BIN/node"
# Stands in for the Cloudflare API: records the attach request and reports the
# domain as served from then on.
cat > "$DEPLOY_BIN/curl" <<'EOF'
#!/bin/sh
for argument in "$@"; do
    case "$argument" in
        *"/pages/projects/"*"/domains")
            printf '%s\n' "$argument" > "$DEPLOY_ATTACH_LOG"
            ;;
        '{"name":"'*)
            printf '%s' "$argument" | sed 's/.*"name":"//;s/".*//' > "$DEPLOY_ATTACHED"
            ;;
    esac
done
printf '{"success":true}\n'
EOF
chmod 700 "$DEPLOY_BIN/npx" "$DEPLOY_BIN/node" "$DEPLOY_BIN/curl"
DEPLOY_ATTACHED="$WORK_DIR/attached-domain"
DEPLOY_ATTACH_LOG="$WORK_DIR/attach-url"
export DEPLOY_ATTACHED DEPLOY_ATTACH_LOG

run_configure_app_deploy() {
    printf '%b' "$1" | HOME="$DEPLOY_HOME" \
        PATH="$DEPLOY_BIN:$PATH" \
        HERDR_RELAY_BIN="$DEPLOY_BIN/relay-stub" \
        HERDR_APP_DEPLOY_NODE_DIR="$DEPLOY_BIN" \
        HERDR_RELAY_ENV="$DEPLOY_ENV" \
        bash "$REPO_DIR/relay/configure-app-deploy.sh" 2>&1
}

DEPLOY_OUTPUT="$(run_configure_app_deploy 'unserved.example.test\nq\n' || true)"
case "$DEPLOY_OUTPUT" in
    *"No Pages project above serves https://unserved.example.test"*"Configuration cancelled."*) ;;
    *)
        echo "app deploy did not explain or escape an unservable origin" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
if grep -q 'HERDR_CLOUDFLARE_PAGES_PROJECT' "$DEPLOY_ENV"; then
    echo "a cancelled app deploy configuration still wrote to relay.env" >&2
    exit 1
fi

# Correcting the origin at that second chance has to get past the origin problem
# and reach the project question, which is itself escapable. Prompts are
# invisible here because bash prints them only to a terminal, so the evidence is
# what the run did: it stopped at the project step without complaining about the
# origin again, and wrote nothing.
DEPLOY_OUTPUT="$(run_configure_app_deploy 'unserved.example.test\napp.example.test\nq\n' || true)"
case "$DEPLOY_OUTPUT" in
    *"No Pages project serves https://app.example.test either"*)
        echo "a corrected origin was rejected" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
case "$DEPLOY_OUTPUT" in
    *"Configuration cancelled."*) ;;
    *)
        echo "the project question could not be escaped" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
if grep -q 'HERDR_CLOUDFLARE_PAGES_PROJECT' "$DEPLOY_ENV"; then
    echo "escaping the project question still wrote to relay.env" >&2
    exit 1
fi

# The whole point of the flow: a matching origin and project are recorded.
printf '#!/bin/sh\nexit 0\n' > "$DEPLOY_BIN/systemctl"
chmod 700 "$DEPLOY_BIN/systemctl"
run_configure_app_deploy 'app.example.test\nherdr-0cv\n' >/dev/null 2>&1 || true
test "$(env_file_value "$DEPLOY_ENV" HERDR_APP_DEPLOY_ORIGIN)" = "https://app.example.test"
test "$(env_file_value "$DEPLOY_ENV" HERDR_CLOUDFLARE_PAGES_PROJECT)" = "herdr-0cv"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin-configured")" = "https://app.example.test"
printf 'https://relay.example.test\n' > "$(dirname "$DEPLOY_ENV")/phone-app-origin"
DEPLOY_REOPEN_OUTPUT="$(
    run_configure_app_deploy '\nherdr-0cv\nn\n' 2>&1 || true
)"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin-configured")" = "https://app.example.test"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin")" = "https://relay.example.test"

# With a token, the action attaches the domain itself instead of sending the
# person to the dashboard, and then records the configuration it just made
# possible.
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
rm -f "$DEPLOY_ATTACHED" "$DEPLOY_ATTACH_LOG"
CLOUDFLARE_API_TOKEN=test-token \
    CLOUDFLARE_ACCOUNT_ID=0123456789abcdef0123456789abcdef \
    HERDR_APP_DEPLOY_ATTACH_DOMAIN=true \
    run_configure_app_deploy 'new.example.test\nherdr-0cv\n' >/dev/null 2>&1 || true
test "$(cat "$DEPLOY_ATTACHED" 2>/dev/null)" = "new.example.test" ||
    { echo "the domain was never sent to Cloudflare" >&2; exit 1; }
case "$(cat "$DEPLOY_ATTACH_LOG" 2>/dev/null)" in
    *"/accounts/0123456789abcdef0123456789abcdef/pages/projects/herdr-0cv/domains") ;;
    *)
        echo "the attach request went to the wrong endpoint" >&2
        cat "$DEPLOY_ATTACH_LOG" >&2 || true
        exit 1
        ;;
esac
test "$(env_file_value "$DEPLOY_ENV" HERDR_APP_DEPLOY_ORIGIN)" = "https://new.example.test"

# Without a token it must not pretend: it says what to set, and changes nothing.
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
rm -f "$DEPLOY_ATTACHED" "$DEPLOY_ATTACH_LOG"
DEPLOY_OUTPUT="$(
    HERDR_APP_DEPLOY_ATTACH_DOMAIN=true \
        run_configure_app_deploy 'other.example.test\nq\n' || true
)"
case "$DEPLOY_OUTPUT" in
    *"set CLOUDFLARE_API_TOKEN in"*) ;;
    *)
        echo "app deploy did not name the credential it needs to attach a domain" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
[ ! -e "$DEPLOY_ATTACHED" ] ||
    { echo "a domain was attached without a token" >&2; exit 1; }

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

# The setup fragment carries only gateways=<ordered list> for a
# gateway-configured relay and relay=<wss url> otherwise. Both keep the token
# inside the fragment.
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
    *"gateways=wss%3A%2F%2Fgw.example.test"*) ;;
    *)
        echo "gateway fragment lost the candidate list: $GATEWAY_FRAGMENT" >&2
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
test "$ENV_GATEWAY_FRAGMENT" = "label=workstation&setup=relay-secret-token&gateways=wss%3A%2F%2Fother.example.test"

# An ordered candidate list is encoded in full, in order, so a phone fails over
# to the second gateway on its own.
LIST_GATEWAY_FRAGMENT="$(
    HERDR_GATEWAY_URL="wss://a.example.test, wss://b.example.test/" \
        HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation ""
)"
test "$LIST_GATEWAY_FRAGMENT" = "label=workstation&setup=relay-secret-token&gateways=wss%3A%2F%2Fa.example.test,wss%3A%2F%2Fb.example.test"

test "$(HERDR_GATEWAY_URL="wss://gw.example.test/" gateway_url "$WORK_DIR/config/relay.env")" = "wss://gw.example.test"
test -z "$(unset HERDR_GATEWAY_URL; gateway_url "$WORK_DIR/config/relay.env")"

# A list is parsed in order, with blank entries and trailing slashes dropped, and
# gateway_url stays the first entry used by setup and service scripts.
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

# An own gateway always reaches an explicit final candidate-list choice. The
# default keeps the operator entry first, appends every community fallback, and
# removes duplicates without reordering.
test "$(
    HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_COMMUNITY_GATEWAY_URL="wss://gw-a.example.test,wss://gw-b.example.test" \
        gateway_subscription_defaults "wss://own.example.test,wss://gw-a.example.test"
)" = "wss://own.example.test,wss://gw-a.example.test,wss://gw-b.example.test"
test "$(
    HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_GATEWAY_SUBSCRIPTIONS="wss://own.example.test,wss://backup.example.test" \
        prompt_gateway_subscriptions "wss://unused.example.test"
)" = "wss://own.example.test,wss://backup.example.test"
test -z "$(HERDR_COMMUNITY_GATEWAY_URL="" community_gateway_url)"

# The selection rule is a decision about which gateway carries traffic, so an
# unrecognized or empty answer keeps the default the list was built with rather
# than switching policies behind the operator's back.
test "$(gateway_selection_choice ordered '')" = "ordered"
test "$(gateway_selection_choice ordered 1)" = "ordered"
test "$(gateway_selection_choice ordered 2)" = "latency"
test "$(gateway_selection_choice latency '')" = "latency"
test "$(gateway_selection_choice latency 2)" = "ordered"
test "$(gateway_selection_choice ordered nonsense)" = "ordered"
# Without a terminal the action's own default stands. A saved policy in the
# environment must not answer for the operator: the setup menu loads relay.env
# before running an action, so honouring it here would let an old own-gateway
# `ordered` silently follow them into a freshly chosen community list.
test "$(prompt_gateway_selection ordered < /dev/null)" = "ordered"
test "$(HERDR_GATEWAY_SELECTION=ordered prompt_gateway_selection latency < /dev/null)" = "latency"
test "$(HERDR_GATEWAY_SELECTION=latency prompt_gateway_selection ordered < /dev/null)" = "ordered"
# The status line phrases the rule instead of printing the variable.
test "$(gateway_selection_label latency)" = "closest wins"
test "$(gateway_selection_label ordered)" = "first listed wins"
test "$(gateway_selection_label '')" = "first listed wins"

# Each flattened transport entry is complete: Community accepts the managed
# candidate list without asking for addresses, retains an unavailable cold
# fallback, saves the latency policy, and immediately enters Quick Start.
CHOOSER_BIN_DIR="$WORK_DIR/chooser-bin"
CHOOSER_SCRIPT_DIR="$WORK_DIR/chooser-scripts"
CHOOSER_ENV="$WORK_DIR/config/chooser.env"
CHOOSER_START_MARKER="$WORK_DIR/chooser-started"
mkdir -p "$CHOOSER_BIN_DIR" "$CHOOSER_SCRIPT_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-choose-transport.sh" \
    "$CHOOSER_SCRIPT_DIR/"
cat > "$CHOOSER_BIN_DIR/curl" <<'EOF'
#!/bin/sh
# Faithful enough for the probe: the health check reads the body from stdout,
# and the latency probe asks for the body in --output with the timing on
# stdout. A double that answered only the first shape would report every
# gateway unavailable.
OUT=""
PREV=""
for ARG in "$@"; do
    [ "$PREV" != "--output" ] || OUT="$ARG"
    PREV="$ARG"
done
case "$*" in
    *own.example.test/healthz* | *gw-a.example.test/healthz*)
        if [ -n "$OUT" ]; then
            printf '%s\n' '{"ok":true}' > "$OUT"
            printf '0.042'
        else
            printf '%s\n' '{"ok":true}'
        fi
        ;;
    *)
        exit 22
        ;;
esac
EOF
chmod 700 "$CHOOSER_BIN_DIR/curl"
CHOOSER_TRANSPORT_MARKER="$WORK_DIR/chooser-transport"
export CHOOSER_TRANSPORT_MARKER
cat > "$CHOOSER_SCRIPT_DIR/plugin-quick-start.sh" <<'EOF'
#!/bin/sh
if [ "${HERDR_GATEWAY_URL+x}" = x ]; then
    printf 'gateway=%s\n' "$HERDR_GATEWAY_URL" > "$CHOOSER_TRANSPORT_MARKER"
else
    printf 'unset\n' > "$CHOOSER_TRANSPORT_MARKER"
fi
printf 'started\n' > "$CHOOSER_START_MARKER"
EOF
chmod 700 "$CHOOSER_SCRIPT_DIR/plugin-quick-start.sh"
export CHOOSER_START_MARKER
# The setup menu loads relay.env before running an action, so a policy left by
# an earlier own-gateway setup is already exported here. Choosing Community has
# to reset it: these candidates are interchangeable, and inheriting `ordered`
# would silently pin the pool to whichever entry happens to be listed first.
CHOOSER_OUTPUT="$(
    PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_GATEWAY_SELECTION="ordered" \
        HERDR_COMMUNITY_GATEWAY_URL="gw-a.example.test,https://gw-b.example.test" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" community
)"
test -f "$CHOOSER_START_MARKER"
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

# The existing-gateway path ends with the complete subscription list too. An
# empty final answer accepts own-first plus all published community fallbacks.
rm -f "$CHOOSER_START_MARKER"
: > "$CHOOSER_ENV"
OWN_OUTPUT="$(
    printf 'b\nown.example.test\n\n' |
        PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_COMMUNITY_GATEWAY_URL="gw-a.example.test,https://gw-b.example.test" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" own
)"
test -f "$CHOOSER_START_MARKER"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_URL)" = \
    "wss://own.example.test,wss://gw-a.example.test,wss://gw-b.example.test"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_SELECTION)" = "ordered"
case "$OWN_OUTPUT" in
    *"Saved 3 gateway candidates."*"first healthy one in that order."*) ;;
    *) echo "own gateway chooser did not save the explicit fallback list" >&2; exit 1 ;;
esac

# Clearing the gateway file must also clear an inherited gateway variable
# before Quick Start builds the setup fragment. Otherwise the background relay
# switches to Cloudflare while the QR still sends the phone to the old gateway.
TEMPORARY_OUTPUT="$(
    PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_GATEWAY_URL="wss://stale.example.test" \
        HERDR_GATEWAY_SELECTION="latency" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" temporary
)"
test "$(cat "$CHOOSER_TRANSPORT_MARKER")" = "unset"
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$CHOOSER_ENV"; then
    echo "temporary tunnel selection left the gateway configured" >&2
    exit 1
fi
case "$TEMPORARY_OUTPUT" in
    *"Temporary Cloudflare tunnel selected."*) ;;
    *) echo "temporary tunnel selection was not reported" >&2; exit 1 ;;
esac

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
cat > "$START_BIN_DIR/curl" <<'EOF'
#!/bin/sh
printf '%s\n' '{"status":"ok","instance":"start-instance","version":"9.9.9","protocol":2}'
EOF
cat > "$START_BIN_DIR/herdr" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$START_BIN_DIR/relay-bin" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$START_RELAY_LOG"
exit 1
EOF
chmod 700 "$START_SCRIPT_DIR/setup-link.sh" "$START_BIN_DIR/systemctl" \
    "$START_BIN_DIR/curl" "$START_BIN_DIR/herdr" "$START_BIN_DIR/relay-bin"
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

# The top-level Stable Tunnel action clears a gateway before stable setup, so
# the service and QR cannot accidentally retain the previous transport.
STABLE_SWITCH_DIR="$WORK_DIR/stable-switch"
STABLE_SWITCH_ENV="$WORK_DIR/config/stable-switch.env"
STABLE_SWITCH_MARKER="$WORK_DIR/stable-switch-called"
mkdir -p "$STABLE_SWITCH_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-install-service.sh" \
    "$STABLE_SWITCH_DIR/"
cat > "$STABLE_SWITCH_DIR/stable-setup.sh" <<'EOF'
#!/bin/bash
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$HERDR_RELAY_ENV" ||
    [ -n "${HERDR_GATEWAY_URL:-}" ]; then
    echo "stable setup still inherited the gateway" >&2
    exit 1
fi
if [ "${HERDR_STABLE_SETUP_WRAPPED:-}" != 1 ]; then
    echo "🐑 Herdr Mobile Relay stable tunnel setup"
fi
printf 'called\n' > "$STABLE_SWITCH_MARKER"
EOF
chmod 700 "$STABLE_SWITCH_DIR/stable-setup.sh"
printf "HERDR_GATEWAY_URL='wss://gw.example.test'\nHERDR_GATEWAY_SELECTION='latency'\n" \
    > "$STABLE_SWITCH_ENV"
export STABLE_SWITCH_MARKER
STABLE_SWITCH_OUTPUT="$(
    HERDR_RELAY_ENV="$STABLE_SWITCH_ENV" HERDR_GATEWAY_URL="wss://inherited.example.test" \
        bash "$STABLE_SWITCH_DIR/plugin-install-service.sh" 2>&1
)"
[ -f "$STABLE_SWITCH_MARKER" ] ||
    { echo "the stable tunnel action did not run stable setup" >&2; exit 1; }
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$STABLE_SWITCH_ENV"; then
    echo "the direct stable tunnel action left the gateway configured" >&2
    exit 1
fi
case "$STABLE_SWITCH_OUTPUT" in
    *"Switching this relay from the WebRTC gateway to Cloudflare."*) ;;
    *) echo "the stable tunnel action did not explain the transport switch" >&2; exit 1 ;;
esac
test "$(printf '%s\n' "$STABLE_SWITCH_OUTPUT" |
    grep -c 'Herdr Mobile Relay stable tunnel setup')" -eq 1 ||
    { echo "stable tunnel action printed its heading more than once" >&2; exit 1; }

# The setup menu opens after every install, including upgrades, so it has to
# report what exists before offering to change it: a stale phone app is exactly
# the thing a person cannot otherwise see.
MENU_BIN_DIR="$WORK_DIR/menu-bin"
MENU_ENV="$WORK_DIR/config/menu.env"
MENU_ROOT="$WORK_DIR/menu-release"
mkdir -p "$MENU_BIN_DIR" "$MENU_ROOT/current"
printf '{\n  "version": "9.9.9"\n}\n' > "$MENU_ROOT/current/release-manifest.json"
printf "HERDR_GATEWAY_URL='wss://gw-a.example.test,wss://gw-b.example.test'\nHERDR_APP_DEPLOY_ORIGIN='https://app.example.test'\n" > "$MENU_ENV"
printf "HERDR_GATEWAY_DEPLOY_HOST='gw-owned.example.test'\n" \
    > "$WORK_DIR/config/gateway-deploy"
printf 'https://relay.example.test\n' > "$WORK_DIR/config/phone-app-origin"
cat > "$MENU_BIN_DIR/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *127.0.0.1*healthz*) printf '{"status":"ok","release_version":"9.9.9","gateway_url":"wss://gw-b.example.test","gateway_version":"9.9.8","gateway_available_version":"9.9.10"}\n' ;;
    *app.example.test/version.json*) printf '{"version":"9.9.8","assets":1}\n' ;;
    *gw-owned.example.test/healthz*) printf '{"ok":true,"version":"9.9.8","revision":"abc123"}\n' ;;
    *) exit 22 ;;
esac
EOF
chmod 700 "$MENU_BIN_DIR/curl"
# Entering an action and finishing it has to come back here, not end the pane:
# 9 shows the status, then the menu is redrawn and q leaves. Without a terminal
# the return prompt is skipped, so the input carries no extra newline.
MENU_OUTPUT="$(
    printf '9\nq\n' |
        PATH="$MENU_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELEASE_ROOT="$MENU_ROOT" \
        HERDR_RELAY_ENV="$MENU_ENV" \
        bash "$REPO_DIR/relay/plugin-setup-menu.sh"
)"
case "$MENU_OUTPUT" in
    *"Herdr Mobile Relay status"*) ;;
    *) echo "setup menu did not open the status action" >&2; exit 1 ;;
esac
test "$(printf '%s\n' "$MENU_OUTPUT" | grep -c 'Herdr Mobile Relay Setup')" -ge 2 ||
    { echo "setup menu did not come back after an action" >&2; exit 1; }
case "$MENU_OUTPUT" in
    *"Relay:      9.9.9 running"*) ;;
    *) echo "setup menu did not report the running release" >&2; exit 1 ;;
esac
# The status line names the rule that picked this gateway, so "why that one"
# is answerable without opening relay.env.
case "$MENU_OUTPUT" in
    *"gateway wss://gw-b.example.test runs 9.9.8; latest available 9.9.10 (+1 fallback, first listed wins)"*) ;;
    *) echo "setup menu did not report the active gateway, version, and selection rule" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"serves 9.9.8, this relay ships 9.9.9"*) ;;
    *) echo "setup menu did not report the stale phone app" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Own gateway: gw-owned.example.test runs 9.9.8; plugin offers 9.9.10 - run herdr plugin install 0cv/herdr-mobile-relay, then redeploy with 3"*) ;;
    *) echo "setup menu did not report how to update the stale self-hosted gateway" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Exit, change nothing"*) ;;
    *) echo "setup menu did not offer a way out" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Choose Phone App and Show QR"*) ;;
    *) echo "setup menu did not expose the phone app origin chooser" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Temporary Cloudflare Tunnel"*"Community WebRTC Gateway"*"Your Own WebRTC Gateway"*"Stable Tunnel"*) ;;
    *) echo "setup menu did not flatten every connection action" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Choose Connection Method"*)
        echo "setup menu retained the redundant connection submenu" >&2
        exit 1
        ;;
esac

# The menu process itself is long-lived. After a transport action removes a
# file-backed gateway, its inherited copy must be cleared before the next
# action; re-sourcing a file cannot unset a variable that is no longer present.
REFRESH_MENU_DIR="$WORK_DIR/refresh-menu-scripts"
REFRESH_MENU_ENV="$WORK_DIR/config/refresh-menu.env"
REFRESH_MENU_MARKER="$WORK_DIR/refresh-menu-state"
mkdir -p "$REFRESH_MENU_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" \
    "$REFRESH_MENU_DIR/"
printf "HERDR_GATEWAY_URL='wss://stale.example.test'\nHERDR_GATEWAY_SELECTION='latency'\n" \
    > "$REFRESH_MENU_ENV"
cat > "$REFRESH_MENU_DIR/plugin-choose-transport.sh" <<'EOF'
#!/bin/sh
: > "$HERDR_RELAY_ENV"
EOF
cat > "$REFRESH_MENU_DIR/plugin-status.sh" <<'EOF'
#!/bin/sh
if [ "${HERDR_GATEWAY_URL+x}" = x ]; then
    printf 'stale=%s\n' "$HERDR_GATEWAY_URL" > "$REFRESH_MENU_MARKER"
else
    printf 'unset\n' > "$REFRESH_MENU_MARKER"
fi
EOF
chmod 700 "$REFRESH_MENU_DIR/plugin-choose-transport.sh" \
    "$REFRESH_MENU_DIR/plugin-status.sh"
export REFRESH_MENU_MARKER
printf '1\n9\nq\n' |
    HERDR_RELAY_BIN="$NORMALIZE_BIN" HERDR_RELAY_ENV="$REFRESH_MENU_ENV" \
    bash "$REFRESH_MENU_DIR/plugin-setup-menu.sh" >/dev/null
test "$(cat "$REFRESH_MENU_MARKER")" = "unset"

# A setup pane can outlive an in-place plugin update. Replacing its script
# directory leaves the shell in a deleted cwd; the next action must first enter
# the newly installed directory rather than leaking getcwd/chdir failures.
STALE_MENU_DIR="$WORK_DIR/stale-menu-scripts"
STALE_MENU_OLD="$WORK_DIR/stale-menu-old"
STALE_MENU_FIFO="$WORK_DIR/stale-menu-input"
STALE_MENU_OUTPUT="$WORK_DIR/stale-menu-output"
STALE_MENU_MARKER="$WORK_DIR/stale-menu-action"
export STALE_MENU_MARKER
mkdir -p "$STALE_MENU_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" \
    "$STALE_MENU_DIR/"
mkfifo "$STALE_MENU_FIFO"
exec 7<> "$STALE_MENU_FIFO"
(
    HERDR_RELAY_BIN="$NORMALIZE_BIN" HERDR_RELAY_ENV="$MENU_ENV" \
        bash "$STALE_MENU_DIR/plugin-setup-menu.sh" <&7 \
        > "$STALE_MENU_OUTPUT" 2>&1
) &
STALE_MENU_PID=$!
STALE_MENU_RENDERED=false
for _attempt in 1 2 3 4 5 6 7 8 9 10; do
    if grep -q "Exit, change nothing" "$STALE_MENU_OUTPUT" 2>/dev/null; then
        STALE_MENU_RENDERED=true
        break
    fi
    sleep 0.1
done
if [ "$STALE_MENU_RENDERED" != true ]; then
    printf 'q\n' >&7
    wait "$STALE_MENU_PID" || true
    echo "setup menu did not render before cwd replacement" >&2
    exit 1
fi
mv "$STALE_MENU_DIR" "$STALE_MENU_OLD"
rm -rf "$STALE_MENU_OLD"
mkdir -p "$STALE_MENU_DIR"
cat > "$STALE_MENU_DIR/plugin-status.sh" <<'EOF'
#!/bin/sh
printf 'action ran\n' > "$STALE_MENU_MARKER"
EOF
chmod 700 "$STALE_MENU_DIR/plugin-status.sh"
printf '9\nq\n' >&7
wait "$STALE_MENU_PID"
exec 7>&-
test -f "$STALE_MENU_MARKER"
if grep -qE 'getcwd|cannot access parent directories|chdir:' "$STALE_MENU_OUTPUT"; then
    echo "setup action inherited the replaced plugin working directory" >&2
    exit 1
fi

# Moving a relay to a new domain must not mean tearing down the tunnel and
# re-pairing every phone: the route is added to the existing tunnel, the ingress
# follows, and the recorded hostname keeps up so a later teardown still matches.
MOVE_HOME="$WORK_DIR/move-home"
MOVE_BIN="$WORK_DIR/move-bin"
MOVE_ENV="$MOVE_HOME/relay.env"
MOVE_CONFIG="$MOVE_HOME/cloudflared/config.yml"
MOVE_STATE="$MOVE_HOME/stable-setup.json"
MOVE_ROUTE_LOG="$WORK_DIR/route-log"
mkdir -p "$MOVE_HOME/cloudflared" "$MOVE_BIN"
export MOVE_ROUTE_LOG
printf "HERDR_RELAY_TOKEN='move-token'\nCLOUDFLARED_CONFIG='%s'\n" "$MOVE_CONFIG" > "$MOVE_ENV"
cat > "$MOVE_CONFIG" <<'EOF'
tunnel: herdr-mobile-relay-fedora
credentials-file: /somewhere/creds.json
ingress:
  - hostname: relay-fedora.old.test
    service: http://localhost:8375
  - service: http_status:404
EOF
printf '{"owner":"herdr-mobile-relay-stable-setup-v1","schema":1,"hostname":"relay-fedora.old.test"}\n' \
    > "$MOVE_STATE"
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$MOVE_ROUTE_LOG"
exit 0
EOF
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
case "$*" in
    "--help all")
        printf '%s\n' '     --doh-url <URL>  Resolve host names over DNS-over-HTTPS'
        exit 0
        ;;
    *"relay-fedora.new.test"*)
        case "$*" in
            *"--doh-url "*) ;;
            *) exit 6 ;;
        esac
        ;;
esac
printf '{"status":"ok","instance":"same","version":"9.9.9","protocol":2}\n'
EOF
cat > "$MOVE_BIN/relay-stub" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "stable-state health-match") exit 0 ;;
    "stable-state update")
        printf '%s\n' "${4#hostname=}" > "$MOVE_STATE_RECORD"
        exit 0
        ;;
    *) exit 0 ;;
esac
EOF
printf '#!/bin/sh\nexit 0\n' > "$MOVE_BIN/service.sh"
chmod 700 "$MOVE_BIN/cloudflared" "$MOVE_BIN/curl" "$MOVE_BIN/relay-stub" "$MOVE_BIN/service.sh"
MOVE_STATE_RECORD="$WORK_DIR/state-hostname"
export MOVE_STATE_RECORD
reset_move_config() {
    cat > "$MOVE_CONFIG" <<'YAML'
tunnel: herdr-mobile-relay-fedora
credentials-file: /somewhere/creds.json
ingress:
  - hostname: relay-fedora.old.test
    service: http://localhost:8375
  - service: http_status:404
YAML
    printf "HERDR_RELAY_TOKEN='move-token'\nCLOUDFLARED_CONFIG='%s'\n" "$MOVE_CONFIG" > "$MOVE_ENV"
}
# setup-link and service.sh are the two things this action hands off to; both
# are stubbed so the test observes the move itself.
cp "$MOVE_BIN/service.sh" "$WORK_DIR/move-service.sh"

MOVE_OUTPUT="$(
    printf 'relay-fedora.new.test\n' |
        HOME="$MOVE_HOME" \
        PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" \
        HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_STABLE_STATE_FILE="$MOVE_STATE" \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
grep -Fq 'tunnel route dns herdr-mobile-relay-fedora relay-fedora.new.test' "$MOVE_ROUTE_LOG" ||
    { echo "the new hostname was never routed to the existing tunnel" >&2
      printf '%s\n' "$MOVE_OUTPUT" >&2; exit 1; }
grep -Fq 'hostname: relay-fedora.new.test' "$MOVE_CONFIG" ||
    { echo "the ingress still serves the old hostname" >&2; exit 1; }
grep -Fq 'hostname: relay-fedora.old.test' "$MOVE_CONFIG" ||
    { echo "the old hostname stopped serving before its record was deleted" >&2; exit 1; }
grep -Fq 'tunnel: herdr-mobile-relay-fedora' "$MOVE_CONFIG" ||
    { echo "the move rewrote more than the hostname" >&2; exit 1; }
test "$(cat "$MOVE_STATE_RECORD" 2>/dev/null)" = "relay-fedora.new.test" ||
    { echo "the recorded hostname was not updated" >&2; exit 1; }
[ -f "$MOVE_CONFIG.herdr-previous" ] ||
    { echo "the previous config was not kept" >&2; exit 1; }

# A gateway relay has no hostname to move, and saying so beats editing a config
# that is not in use.
printf "HERDR_RELAY_TOKEN='move-token'\n" > "$MOVE_ENV"
MOVE_OUTPUT="$(
    HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" \
        HERDR_RELAY_ENV="$MOVE_ENV" bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"does not run a Cloudflare tunnel"*) ;;
    *)
        echo "a gateway relay was not told hostnames do not apply" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac

# cloudflared exits 0 while creating a different name when its certificate does
# not cover the zone. Believing it once left a live relay serving a hostname
# that answered nowhere, so a mismatch must change nothing at all.
reset_move_config
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf 'INF Added CNAME relay-fedora.new.test.old.test which will route to your tunnel\n'
exit 0
EOF
chmod 700 "$MOVE_BIN/cloudflared"
MOVE_OUTPUT="$(
    printf 'relay-fedora.new.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"created relay-fedora.new.test.old.test, not relay-fedora.new.test"*) ;;
    *)
        echo "a mis-zoned route was accepted" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
grep -Fq 'hostname: relay-fedora.new.test' "$MOVE_CONFIG" &&
    { echo "a mis-zoned route still rewrote the ingress" >&2; exit 1; }

# A new name the edge never answers must not take the old one out of service.
reset_move_config
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf 'INF Added CNAME relay-fedora.unreachable.test which will route to your tunnel\n'
exit 0
EOF
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
exit 6
EOF
chmod 700 "$MOVE_BIN/cloudflared" "$MOVE_BIN/curl"
MOVE_OUTPUT="$(
    printf 'relay-fedora.unreachable.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_STABLE_DNS_TIMEOUT=0 \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"does not resolve to Cloudflare yet"*) ;;
    *)
        echo "an unresolvable new hostname was accepted" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
grep -Fq 'hostname: relay-fedora.old.test' "$MOVE_CONFIG" ||
    { echo "the old hostname was lost while the new one was unreachable" >&2; exit 1; }

# The certificate names the only zone cloudflared can route into. When the new
# hostname is outside it, the refusal has to come before any record exists.
reset_move_config
: > "$MOVE_ROUTE_LOG"
MOVE_CERT="$MOVE_HOME/cert.pem"
{
    printf -- '-----BEGIN ARGO TUNNEL TOKEN-----\n'
    printf '{"zoneID":"zone1","apiToken":"token1","accountID":"acct1"}' | base64
    printf -- '-----END ARGO TUNNEL TOKEN-----\n'
} > "$MOVE_CERT"
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *"/zones/zone1"*)
        printf '{"result":{"id":"zone1","name":"old.test"},"success":true}\n'
        ;;
    *) printf '{"status":"ok","instance":"same","version":"9.9.9","protocol":2}\n' ;;
esac
EOF
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$MOVE_ROUTE_LOG"
exit 0
EOF
chmod 700 "$MOVE_BIN/curl" "$MOVE_BIN/cloudflared"
MOVE_OUTPUT="$(
    printf 'relay-fedora.elsewhere.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        TUNNEL_ORIGIN_CERT="$MOVE_CERT" HERDR_CHANGE_HOSTNAME_RELOGIN=false \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"certificate covers old.test"*"cloudflared tunnel login"*) ;;
    *)
        echo "a hostname outside the certificate's zone was not refused" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
[ ! -s "$MOVE_ROUTE_LOG" ] ||
    { echo "a record was created before the zone was checked" >&2; exit 1; }
grep -Fq 'hostname: relay-fedora.old.test' "$MOVE_CONFIG" ||
    { echo "the refused move still touched the ingress" >&2; exit 1; }

echo "common shell tests passed"

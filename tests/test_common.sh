#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-common-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

(
    for hostname in relay.example.test RELAY.EXAMPLE.TEST. \
        HTTPS://RELAY.EXAMPLE.TEST./setup WSS://RELAY.EXAMPLE.TEST/path; do
        normalized="$(normalize_hostname "$hostname")"
        test "$normalized" = relay.example.test
        valid_hostname "$normalized"
    done
    test -z "$(normalize_hostname '')"
    for hostname in relay..example.test relay.-bad.test relay.bad-.test \
        relay_example.test http://relay.example.test; do
        if valid_hostname "$(normalize_hostname "$hostname")"; then
            echo "invalid hostname was accepted: $hostname" >&2
            exit 1
        fi
    done

    config="$WORK_DIR/scalars.yml"
    printf '%s\n' 'tunnel: "relay tunnel" # tunnel name' \
        "  - hostname: 'relay.example.test' # public hostname" > "$config"
    test "$(yaml_scalar tunnel "$config")" = 'relay tunnel'
    test "$(yaml_scalar hostname "$config")" = relay.example.test
)

(
    require_positive_integer 3 Attempts
    for invalid in '' 0 -1 1.5 text; do
        if require_positive_integer "$invalid" Attempts >/dev/null 2>&1; then
            echo "invalid attempt count was accepted: $invalid" >&2
            exit 1
        fi
    done

    env_file="$WORK_DIR/atomic/relay.env"
    set_env_value_atomic "$env_file" FIRST original
    set_env_value_atomic "$env_file" SECOND retained
    set_env_value_atomic "$env_file" FIRST ''
    test "$(env_file_value "$env_file" FIRST)" = ''
    test "$(env_file_value "$env_file" SECOND)" = retained
    test "$(env_file_value "$env_file" ABSENT)" = ''
    test "$(ABSENT=exported env_file_value "$env_file" ABSENT)" = ''
    test "$(SECOND=exported env_file_value "$env_file" SECOND)" = retained
    remove_env_value_if_equals_atomic "$env_file" SECOND different
    test "$(env_file_value "$env_file" SECOND)" = retained
    remove_env_value_if_equals_atomic "$env_file" FIRST ''
    ! grep -q '^FIRST=' "$env_file"
    remove_env_value_atomic "$env_file" SECOND
    test ! -s "$env_file"
    private_owned_file "$env_file"
    remove_env_value_atomic "$WORK_DIR/missing.env" FIRST
    test ! -e "$WORK_DIR/missing.env"
    if set_env_value_atomic "$env_file" FIRST "can't" >/dev/null 2>&1; then
        echo "an unquotable environment value was accepted" >&2
        exit 1
    fi
    test ! -s "$env_file"
)

(
    installed_service_env_file() { printf '%s\n' "$WORK_DIR/installed.env"; }
    export HERDR_RELAY_ENV="$WORK_DIR/explicit.env"
    adopt_installed_service_env announce
    test "$HERDR_RELAY_ENV" = "$WORK_DIR/explicit.env"
    unset HERDR_RELAY_ENV
    adopt_installed_service_env
    test "$HERDR_RELAY_ENV" = "$WORK_DIR/installed.env"
    unset HERDR_RELAY_ENV
    adopt_installed_service_env announce > "$WORK_DIR/adopt-output"
    grep -Fq 'Reusing the installed relay configuration:' "$WORK_DIR/adopt-output"
    test "$HERDR_RELAY_ENV" = "$WORK_DIR/installed.env"
    unset HERDR_RELAY_ENV
    installed_service_env_file() { return 0; }
    adopt_installed_service_env
    test -z "${HERDR_RELAY_ENV:-}"
)

(
    command() {
        if [ "$1" = -v ] && [ "$2" = openssl ]; then
            return 1
        fi
        builtin command "$@"
    }
    uuidgen() { printf 'ABCDEF01-2345-6789-ABCD-EF0123456789\n'; }
    test "$(generate_token)" = abcdef0123456789abcdef0123456789
    test "$(generate_instance_id)" = abcdef01-2345-6789-abcd-ef0123456789
)

(
    curl() {
        printf '%s\n' "${@: -1}" >> "$WORK_DIR/dns-requests"
        [ "$dns_mode" != unavailable ] || return 7
        if [ "$dns_mode" = answer ] && [[ "${@: -1}" = *type=CNAME ]]; then
            printf '{"Answer":[]}\n'
        else
            printf '{}\n'
        fi
    }
    dns_mode=answer
    dns_has_record relay.example.test
    test "$(wc -l < "$WORK_DIR/dns-requests" | tr -d ' ')" = 3
    if dns_has_record relay.example.test A; then
        echo "an A-only probe accepted a CNAME response" >&2
        exit 1
    else
        test "$?" -eq 1
    fi
    dns_mode=unavailable
    if dns_has_record relay.example.test; then
        echo "a failed DNS request was accepted" >&2
        exit 1
    else
        test "$?" -eq 2
    fi
)

(
    curl() {
        while [ "$#" -gt 0 ]; do
            if [ "$1" = --output ]; then
                shift
                printf '%s\n' "$gateway_body" > "$1"
            fi
            shift
        done
        printf '%s' "$gateway_seconds"
        return "$gateway_status"
    }
    gateway_body='{"ok":true}'
    gateway_seconds=0.012
    gateway_status=0
    test "$(gateway_healthz_ms wss://gateway.example.test)" = 12
    test -z "$(gateway_answers_healthz wss://gateway.example.test)"
    gateway_seconds=''
    gateway_answers_healthz wss://gateway.example.test
    gateway_body='{"ok":false}'
    ! gateway_answers_healthz wss://gateway.example.test
    gateway_status=7
    ! gateway_answers_healthz wss://gateway.example.test
)

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

(
    export HOME="$WORK_DIR/core-home" XDG_CONFIG_HOME="$WORK_DIR/core-home/config"
    export XDG_STATE_HOME="$WORK_DIR/core-home/state" XDG_CACHE_HOME="$WORK_DIR/core-home/cache"
    export HERDR_RELAY_BACKEND=hc HERDR_PLUGIN_CONFIG_DIR="$WORK_DIR/tui-plugin"
    export HERDR_SOCKET_PATH="$WORK_DIR/tui.sock" HC_SESSION=testing
    unset HERDR_RELAY_ENV HERDR_RELAY_PORT HERDR_RELAY_PLUGIN_PORT HC_SOCKET_PATH HC_SOCKET
    resolve_relay_instance
    test "$(relay_env_file "$REPO_DIR/relay")" = "$XDG_CONFIG_HOME/herdr-mobile-relay-hc/relay.env"
    test "$RELAY_SYSTEMD_LABEL" = herdr-mobile-relay-hc.service
    test "$RELAY_LAUNCHD_LABEL" = com.herdr-mobile-relay-hc.service
    test "$RELAY_TAILSCALE_PORT" = 8444
    test -z "${HERDR_PLUGIN_CONFIG_DIR:-}"
    test -z "${HERDR_SOCKET_PATH:-}"
    ensure_relay_env "$HERDR_RELAY_ENV"
    test "$(env_file_value "$HERDR_RELAY_ENV" HERDR_RELAY_PORT)" = 8377
    test "$(env_file_value "$HERDR_RELAY_ENV" HERDR_RELAY_PLUGIN_PORT)" = 8378
    ! grep -q '^HC_SOCKET' "$HERDR_RELAY_ENV"
    test "${HC_SOCKET_PATH+x}" != x
    test "$(env_file_value "$HERDR_RELAY_ENV" HC_SESSION)" = testing
    export HC_SOCKET_PATH=relative/relay.sock HC_SOCKET=relative/core.sock
    resolve_relay_instance
    ensure_relay_env "$HERDR_RELAY_ENV"
    test "$(env_file_value "$HERDR_RELAY_ENV" HC_SOCKET_PATH)" = relative/relay.sock
    test "$(env_file_value "$HERDR_RELAY_ENV" HC_SOCKET)" = relative/core.sock
    unset HC_SESSION HC_SOCKET HC_SOCKET_PATH
    ensure_relay_env "$WORK_DIR/core-default/relay.env"
    ! grep -q '^HC_' "$WORK_DIR/core-default/relay.env"
    test -n "$(env_file_value "$HERDR_RELAY_ENV" HERDR_RELAY_TOKEN)"
    test -n "$(env_file_value "$HERDR_RELAY_ENV" HERDR_RELAY_INSTANCE_ID)"
    test -f "$(dirname "$HERDR_RELAY_ENV")/.herdr-mobile-relay-installation"
    test -f "$XDG_CACHE_HOME/herdr-mobile-relay-hc/.herdr-mobile-relay-installation"
    case "$(relay_host_slug)" in *-hc) ;; *) exit 1 ;; esac
    if HERDR_RELAY_BACKEND=herdr resolve_relay_instance; then
        echo "backend mismatch accepted" >&2; exit 1
    fi
    if HERDR_GATEWAY_URL=wss://gateway.invalid resolve_relay_instance; then
        echo "core gateway accepted" >&2; exit 1
    fi
)

DEV_RELAY_BIN="$WORK_DIR/dev/herdr-mobile-relay"
mkdir -p "$(dirname "$DEV_RELAY_BIN")"
printf '#!/bin/sh\nexit 0\n' > "$DEV_RELAY_BIN"
chmod 700 "$DEV_RELAY_BIN"
test "$(HERDR_RELAY_BIN="$DEV_RELAY_BIN" relay_binary)" = "$DEV_RELAY_BIN"

(
    CORE_RELEASE_LOG="$WORK_DIR/core-release.log"
    core_release_binary() {
        printf '#!/bin/sh\necho ran >> "%s"\necho '"'"'%s'"'"'\n' "$CORE_RELEASE_LOG" "$1" > "$DEV_RELAY_BIN"
    }
    export HERDR_RELAY_BIN="$DEV_RELAY_BIN"

    core_release_binary '{"version":"0.25.0","revision":"r","target":"t","backends":["herdr","hc"]}'
    HERDR_RELAY_BACKEND=hc require_core_relay_release

    for old_identity in \
        '{"version":"0.24.0","revision":"r","target":"t"}' \
        '{"version":"0.25.0","revision":"r","target":"t","backends":["herdr"]}'; do
        core_release_binary "$old_identity"
        if HERDR_RELAY_BACKEND=hc require_core_relay_release 2>"$WORK_DIR/core-release.err"; then
            echo "core accepted a relay release without the hc backend: $old_identity" >&2
            exit 1
        fi
        grep -Fq 'cannot serve herdr-core.' "$WORK_DIR/core-release.err"
        grep -Fq 'Install 0.25.0 or newer first' "$WORK_DIR/core-release.err"
    done

    : > "$CORE_RELEASE_LOG"
    HERDR_RELAY_BACKEND=herdr require_core_relay_release
    test ! -s "$CORE_RELEASE_LOG"

    HERDR_RELAY_BIN="$WORK_DIR/missing/herdr-mobile-relay" HERDR_RELAY_BACKEND=hc require_core_relay_release
)

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
PACKAGED_RELEASE_REAL="$(cd "$PACKAGED_RELEASE" && pwd -P)"
test "$PACKAGED_BINARY" = "$PACKAGED_RELEASE_REAL/herdr-mobile-relay"

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
NODE_TOOL_PATH="$WORK_DIR/node-tools"
mkdir -p "$NODE_TOOL_PATH"
for tool in ls sort tail printenv; do
    ln -s "$(command -v "$tool")" "$NODE_TOOL_PATH/$tool"
done
fake_node_dir() {
    local dir="$1"
    local version="${2:-26.1.0}"
    mkdir -p "$dir"
    cat > "$dir/node" <<EOF
#!/bin/sh
[ "\$1" = --version ] && { echo "v$version"; exit 0; }
exit 0
EOF
    printf '#!/bin/sh\nexit 0\n' > "$dir/npx"
    chmod 700 "$dir/node" "$dir/npx"
}
fake_node_dir "$NODE_HOME/.nvm/versions/node/v20.0.0/bin" 20.0.0
fake_node_dir "$NODE_HOME/.nvm/versions/node/v26.1.0/bin" 26.1.0
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir)" = \
    "$NODE_HOME/.nvm/versions/node/v26.1.0/bin"
fake_node_dir "$NODE_HOME/.nvm/current/bin"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir)" = \
    "$NODE_HOME/.nvm/current/bin"

NODE_ENV_FILE="$WORK_DIR/config/node.env"
mkdir -p "$(dirname "$NODE_ENV_FILE")"
fake_node_dir "$NODE_HOME/recorded/bin"
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/recorded/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/recorded/bin"

# A recording that no longer resolves must not win over a working install.
rm -rf "$NODE_HOME/recorded"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/.nvm/current/bin"

# Both binaries or nothing: wrangler is run through npx, so a directory that
# only carries node is not an installation. This cannot assert "found nothing",
# because the system paths it also searches genuinely hold node on some hosts.
mkdir -p "$NODE_HOME/half/bin"
printf '#!/bin/sh\nexit 0\n' > "$NODE_HOME/half/bin/node"
chmod 700 "$NODE_HOME/half/bin/node"
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/half/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/.nvm/current/bin"

# Wrangler's floor is enforced here rather than discovered halfway through an
# npx download: a recorded node that is too old loses to a usable one.
fake_node_dir "$NODE_HOME/legacy/bin" 20.19.0
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/legacy/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir "$NODE_ENV_FILE")" = \
    "$NODE_HOME/.nvm/current/bin"

# A node that cannot report a version is not a node this action will drive.
fake_node_dir "$NODE_HOME/silent/bin"
printf '#!/bin/sh\nexit 0\n' > "$NODE_HOME/silent/bin/node"
chmod 700 "$NODE_HOME/silent/bin/node"
printf "HERDR_APP_DEPLOY_NODE_DIR='%s'\n" "$NODE_HOME/silent/bin" > "$NODE_ENV_FILE"
test "$(HOME="$NODE_HOME" NVM_DIR="$NODE_HOME/.nvm" PATH="$NODE_TOOL_PATH" node_bin_dir "$NODE_ENV_FILE")" = \
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
        [ "${PHONE_SETUP_RENDER_QR:-}" != 1 ] || printf 'fixture QR\n'
    }
    stdout_is_terminal() { [ "${PHONE_SETUP_TTY:-}" = 1 ]; }
    unset NO_COLOR
    [ "${PHONE_SETUP_NO_COLOR:-}" != 1 ] || NO_COLOR=1
    "$@"
)

PHONE_SETUP_LABEL="$(host_label)"
PHONE_SETUP_PLAIN_EXPECTED="$(
    printf '  Open this private setup link on your phone to add %s:\n  %s\n' \
        "$PHONE_SETUP_LABEL" "$PHONE_SETUP_URL"
)"
PHONE_SETUP_LINK_EXPECTED="$(
    printf '  Open this private setup link on your phone to add %s:\n' "$PHONE_SETUP_LABEL"
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

test "$(HERDR_RELAY_BACKEND=herdr relay_host_label)" = "$PHONE_SETUP_LABEL"
test "$(HERDR_RELAY_BACKEND=hc relay_host_label)" = "$PHONE_SETUP_LABEL (core)"
test "$(
    PHONE_SETUP_RENDER_QR=1 HERDR_RELAY_BACKEND=hc \
        run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL" | sed -n 1p
)" = "  Scan with your phone to add $PHONE_SETUP_LABEL (core)"
test "$(
    PHONE_SETUP_RENDER_QR=1 HERDR_RELAY_BACKEND=herdr \
        run_phone_setup_helper print_phone_setup "$PHONE_SETUP_URL" | sed -n 1p
)" = "  Scan with your phone to add $PHONE_SETUP_LABEL"

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

ENV_FILE="$WORK_DIR/config/relay.env"
mkdir -p "$(dirname "$ENV_FILE")"
EXPECTED_TOKEN="test-private-token"
GH_TOKEN="$EXPECTED_TOKEN"
GITHUB_TOKEN="$EXPECTED_TOKEN"
export GH_TOKEN GITHUB_TOKEN
ensure_relay_env "$ENV_FILE"

if [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]; then
    echo "ensure_relay_env left the raw release token in the caller's environment" >&2
    exit 1
fi
if grep -q '^GH_TOKEN=' "$ENV_FILE"; then
    echo "relay.env exposed GH_TOKEN" >&2
    exit 1
fi
TOKEN_FILE="$(env_file_value "$ENV_FILE" HERDR_GITHUB_TOKEN_FILE)"
test "$TOKEN_FILE" = "$WORK_DIR/config/github-token"
test "$(cat "$TOKEN_FILE")" = "$EXPECTED_TOKEN"
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
cat > "$WORK_DIR/service.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.herdr-mobile-relay.service</string>
    <key>ProgramArguments</key>
    <array>
        <string>$WORK_DIR/releases/current/relay/herdr-mobile-relay-service.sh</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$WORK_DIR/releases/current</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HERDR_RELAY_ENV</key>
        <string>$WORK_DIR/config/relay.env</string>
    </dict>
</dict>
</plist>
EOF
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
if command -v plutil >/dev/null 2>&1; then
    INVALID_PLIST="$WORK_DIR/invalid.plist"
    printf '%s\n' '<plist><dict><key>broken</dict></plist>' > "$INVALID_PLIST"
    if PATH="$FAKE_LAUNCHCTL_DIR:$PATH" reload_launchd_service_definition \
        "$INVALID_PLIST" "com.herdr-mobile-relay.service"; then
        echo "invalid plist was accepted" >&2
        exit 1
    fi
    test ! -s "$LAUNCHCTL_LOG"
fi
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

ensure_relay_test_binary "$WORK_DIR"
export HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN"
export HERDR_RELAY_INSTANCE_ID=test

# The inferred-identity path reads the selected binary's sibling manifest,
# never a fabricated field in web/release.json. Use the committed descriptor
# directly so this regression cannot accidentally reproduce the old fixture.
READY_RELEASE_ROOT="$WORK_DIR/readiness-releases"
READY_RELEASE="$READY_RELEASE_ROOT/releases/manifest-release"
mkdir -p "$READY_RELEASE/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_RELEASE/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_RELEASE/web/release.json"
printf '{"version":"dev","revision":"unknown","web_hash":"manifest-web"}\n' \
    > "$READY_RELEASE/release-manifest.json"
if grep -q '"bundle_hash"' "$READY_RELEASE/web/release.json"; then
    echo "committed frontend descriptor unexpectedly contains bundle_hash" >&2
    exit 1
fi
READY_CURL_LOG="$WORK_DIR/readiness-curl.log"
READY_ATTEMPTS="$WORK_DIR/readiness-attempts"
READY_CURL_MODE=static
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
: > "$READY_CURL_LOG"
curl() {
    printf '%s\n' "$*" >> "$READY_CURL_LOG"
    if [ "$READY_CURL_MODE" = retry ]; then
        local attempt=0
        [ ! -f "$READY_ATTEMPTS" ] || attempt="$(<"$READY_ATTEMPTS")"
        attempt=$((attempt + 1))
        printf '%s\n' "$attempt" > "$READY_ATTEMPTS"
        if [ "$attempt" -eq 1 ]; then
            printf '%s\n' '{"status":"ok","inventory":{"state":"error"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
            return
        fi
    fi
    printf '%s\n' "$READY_RESPONSE"
}

HERDR_RELAY_BIN="$READY_RELEASE/herdr-mobile-relay"
unset HERDR_RELEASE_ROOT HERDR_WEB_ROOT
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
test "$(cat "$READY_CURL_LOG")" = \
    '-fsS --max-time 2 http://127.0.0.1:8375/readyz'

# HERDR_WEB_ROOT and a descriptor's tempting bundle_hash must not replace the
# release manifest's identity.
READY_DECOY_WEB="$WORK_DIR/readiness-decoy-web"
mkdir -p "$READY_DECOY_WEB"
printf '{"bundle_hash":"descriptor-decoy"}\n' > "$READY_DECOY_WEB/release.json"
HERDR_WEB_ROOT="$READY_DECOY_WEB"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
: > "$READY_CURL_LOG"
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
READY_RESPONSE=${READY_RESPONSE/manifest-web/descriptor-decoy}
if wait_for_relay_health 8375 1 0 test >/dev/null 2>&1; then
    echo "readiness accepted the descriptor's misleading bundle_hash" >&2
    exit 1
fi
test -s "$READY_CURL_LOG"
unset HERDR_WEB_ROOT
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'

for invalid_health in \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"wrong-instance","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"wrong-version","revision":"unknown","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"wrong-revision","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"wrong-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":2}' \
    '{"status":"ready","inventory":{"state":"error"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'; do
    READY_RESPONSE="$invalid_health"
    : > "$READY_CURL_LOG"
    if wait_for_relay_health 8375 1 0 test >/dev/null 2>&1; then
        echo "readiness accepted invalid identity or inventory: $invalid_health" >&2
        exit 1
    fi
    test "$(wc -l < "$READY_CURL_LOG" | tr -d ' ')" = 1
done

if HERDR_RELAY_BACKEND=hc wait_for_relay_health 8377 1 0 test >"$WORK_DIR/core-ready.out" 2>"$WORK_DIR/core-ready.err"; then
    echo "core readiness accepted failed inventory" >&2
    exit 1
fi
grep -q 'Start herdr-app or run hc server start' "$WORK_DIR/core-ready.err"
! grep -Eq 'Herdr|TUI' "$WORK_DIR/core-ready.err"

# Preserve the bounded retry loop: an unready first reply is retried and a
# matching reply on the second poll is returned unchanged.
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
READY_CURL_MODE=retry
rm -f "$READY_ATTEMPTS"
: > "$READY_CURL_LOG"
READY_OUTPUT="$(wait_for_relay_health 8375 3 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
test "$(cat "$READY_ATTEMPTS")" = 2
test "$(wc -l < "$READY_CURL_LOG" | tr -d ' ')" = 2
READY_CURL_MODE=static

# Missing, unreadable/non-file, and empty manifests fail before any HTTP poll
# and identify the offending metadata path.
READY_MANIFEST="$READY_RELEASE/release-manifest.json"
READY_MANIFEST_BACKUP="$WORK_DIR/readiness-release-manifest.json"
mv "$READY_MANIFEST" "$READY_MANIFEST_BACKUP"
: > "$READY_CURL_LOG"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/missing-manifest.out" 2>"$WORK_DIR/missing-manifest.err"; then
    echo "readiness accepted a missing release manifest" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/missing-manifest.err" >/dev/null
test ! -s "$READY_CURL_LOG"
mkdir "$READY_MANIFEST"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/unreadable-manifest.out" 2>"$WORK_DIR/unreadable-manifest.err"; then
    echo "readiness accepted a non-file release manifest" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/unreadable-manifest.err" >/dev/null
test ! -s "$READY_CURL_LOG"
rmdir "$READY_MANIFEST"
if [ "$(id -u)" -ne 0 ]; then
    printf '{"web_hash":"unreadable-web"}\n' > "$READY_MANIFEST"
    chmod 000 "$READY_MANIFEST"
    if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/denied-manifest.out" 2>"$WORK_DIR/denied-manifest.err"; then
        chmod 600 "$READY_MANIFEST"
        echo "readiness accepted an unreadable release manifest" >&2
        exit 1
    fi
    grep -F "$READY_MANIFEST" "$WORK_DIR/denied-manifest.err" >/dev/null
    test ! -s "$READY_CURL_LOG"
    chmod 600 "$READY_MANIFEST"
fi
printf '{"web_hash":""}\n' > "$READY_MANIFEST"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/empty-hash.out" 2>"$WORK_DIR/empty-hash.err"; then
    echo "readiness accepted an empty manifest web_hash" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/empty-hash.err" >/dev/null
test ! -s "$READY_CURL_LOG"

# An explicit expected hash wins even when the manifest is absent or conflicts.
rm "$READY_MANIFEST"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"explicit-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test dev unknown explicit-web)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
printf '{"web_hash":"conflicting-manifest-web"}\n' > "$READY_MANIFEST"
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test dev unknown explicit-web)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
mv "$READY_MANIFEST_BACKUP" "$READY_MANIFEST"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'

# An explicit binary override and the current symlink each select the sibling
# manifest belonging to the chosen release, not another configured release.
READY_OVERRIDE_ROOT="$WORK_DIR/readiness-override-root"
READY_OVERRIDE="$READY_OVERRIDE_ROOT/releases/override"
mkdir -p "$READY_OVERRIDE/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_OVERRIDE/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_OVERRIDE/web/release.json"
printf '{"web_hash":"override-web"}\n' > "$READY_OVERRIDE/release-manifest.json"
HERDR_RELEASE_ROOT="$READY_RELEASE_ROOT"
HERDR_RELAY_BIN="$READY_OVERRIDE/herdr-mobile-relay"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"override-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
READY_CURRENT_ROOT="$WORK_DIR/readiness-current-root"
READY_CURRENT="$READY_CURRENT_ROOT/releases/current-target"
mkdir -p "$READY_CURRENT/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_CURRENT/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_CURRENT/web/release.json"
printf '{"web_hash":"current-web"}\n' > "$READY_CURRENT/release-manifest.json"
ln -s releases/current-target "$READY_CURRENT_ROOT/current"
unset HERDR_RELAY_BIN
HERDR_RELEASE_ROOT="$READY_CURRENT_ROOT"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"current-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"

# A packaged script's own release takes precedence over a foreign override and
# release root, and its manifest is resolved next to its selected binary.
READY_PACKAGED="$WORK_DIR/readiness-packaged"
mkdir -p "$READY_PACKAGED/relay" "$READY_PACKAGED/web"
cp "$REPO_DIR/relay/common.sh" "$READY_PACKAGED/relay/common.sh"
cp "$HERDR_TEST_RELAY_BIN" "$READY_PACKAGED/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_PACKAGED/web/release.json"
printf '{"web_hash":"packaged-web"}\n' > "$READY_PACKAGED/release-manifest.json"
chmod 700 "$READY_PACKAGED/herdr-mobile-relay"
HERDR_RELAY_BIN="$READY_OVERRIDE/herdr-mobile-relay"
HERDR_RELEASE_ROOT="$READY_CURRENT_ROOT"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"packaged-web","protocol":3}'
READY_OUTPUT="$(
    . "$READY_PACKAGED/relay/common.sh"
    wait_for_relay_health 8375 1 0 test
)"
test "$READY_OUTPUT" = "$READY_RESPONSE"

unset -f curl
HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN"
unset HERDR_RELEASE_ROOT HERDR_WEB_ROOT
HEALTH='{"status":"ready","inventory":{"state":"ready"},"instance":"test","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
verify_relay_release_health "$HEALTH" "0.9.0" "abc123" "web456"
if verify_relay_release_health "$HEALTH" "0.9.0" "wrong" "web456"; then
    echo "release health accepted the wrong revision" >&2
    exit 1
fi

for invalid_health in \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"wrong-instance","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}' \
    '{"status":"ready","inventory":{"state":"error"},"instance":"test","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'; do
    (
        curl() { printf '%s\n' "$invalid_health"; }
        if wait_for_relay_health 8375 1 0 test 0.9.0 abc123 web456; then
            echo 'generic readiness accepted unusable or foreign inventory' >&2
            exit 1
        fi
        if wait_for_relay_release_health 8375 1 0 0.9.0 abc123 web456 test; then
            echo 'release readiness accepted unusable or foreign inventory' >&2
            exit 1
        fi
    )
done

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
    printf '%s\n' '{"status":"ready","inventory":{"state":"ready"},"instance":"test","version":"0.9.0","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
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
unset HERDR_RELAY_BIN HERDR_RELAY_INSTANCE_ID

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

# Printing a setup link re-arms the running relay through relay.pid and SIGUSR1,
# and says plainly when no relay is running here.
ARM_DIR="$WORK_DIR/arm"
mkdir -p "$ARM_DIR"
ARM_ENV="$ARM_DIR/relay.env"
: > "$ARM_ENV"
ARM_LOG="$ARM_DIR/signals.log"
: > "$ARM_LOG"
bash -c 'trap "echo usr1 >> \"$1\"" USR1; while :; do sleep 0.1; done' _ "$ARM_LOG" &
ARM_PID=$!
printf '%s\n' "$ARM_PID" > "$ARM_DIR/relay.pid"
sleep 0.2
arm_setup_link "$ARM_ENV" ||
    { echo "arm_setup_link failed against a live pid" >&2; kill "$ARM_PID"; exit 1; }
sleep 0.3
kill "$ARM_PID" 2>/dev/null || true
wait "$ARM_PID" 2>/dev/null || true
grep -Fxq usr1 "$ARM_LOG" ||
    { echo "the relay was not signalled to re-arm the bootstrap" >&2; exit 1; }
printf '%s\n' "$ARM_PID" > "$ARM_DIR/relay.pid"
if arm_setup_link "$ARM_ENV"; then
    echo "a stale relay.pid was treated as a running relay" >&2
    exit 1
fi
rm -f "$ARM_DIR/relay.pid"
if arm_setup_link "$ARM_ENV"; then
    echo "a missing relay.pid was treated as a running relay" >&2
    exit 1
fi
case "$(print_setup_link_arming 0)" in
    *"pairs one phone within 10 minutes"*) ;;
    *) echo "the armed link hint is missing" >&2; exit 1 ;;
esac
case "$(print_setup_link_arming 1)" in
    *"relay is not running here"*) ;;
    *) echo "the not-running hint is missing" >&2; exit 1 ;;
esac

echo "common shell tests passed"

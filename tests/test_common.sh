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

GATEWAY_HEALTH='{"status":"ok","gateway":{"enabled":true,"registered":true,"relay_id":"AAAA","clients":1}}'
test "$(gateway_registration_state "$GATEWAY_HEALTH")" = "true"
test "$(gateway_registration_state '{"gateway": {"enabled": true, "registered": false}}')" = "false"
test -z "$(gateway_registration_state '{"status":"ok"}')"

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
expect "$(print_setup_link_arming 0)" "pairs one phone within 10 minutes"
expect "$(print_setup_link_arming 1)" "relay is not running here"

echo "common shell tests passed"

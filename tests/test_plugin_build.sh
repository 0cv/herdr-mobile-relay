#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-build-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

TEST_HOME="$WORK_DIR/home"
RELEASE_ROOT="$TEST_HOME/releases"
OLD_RELEASE="$RELEASE_ROOT/releases/0.8.6-old"
TEST_VERSION="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml")"
NEW_RELEASE="$RELEASE_ROOT/releases/$TEST_VERSION-new"
SOURCE_CONFIG="$TEST_HOME/source-checkout/relay"
SOURCE_ENV="$SOURCE_CONFIG/.env"
TARGET_CONFIG="$TEST_HOME/.config/herdr/plugins/config/herdr-mobile-relay.events"
UNIT_FILE="$TEST_HOME/.config/systemd/user/herdr-mobile-relay.service"
FAKE_BIN="$WORK_DIR/bin"
HEALTH_FILE="$WORK_DIR/health.json"
CONFIG_RECORD="$WORK_DIR/installer-config-root"
TOKEN_RECORD="$WORK_DIR/installer-token"
INSTALL_INPUT_RECORD="$WORK_DIR/installer-inputs"
REPO_RECORD="$WORK_DIR/installer-repository"
FRESH_TOKEN_RECORD="$WORK_DIR/fresh-installer-token"
FRESH_INSTALL_INPUT_RECORD="$WORK_DIR/fresh-installer-inputs"
FRESH_REPO_RECORD="$WORK_DIR/fresh-installer-repository"
RESTART_LOG="$WORK_DIR/restarts"
READY_LOG="$WORK_DIR/supervisor-ready"
SETUP_RECORD="$WORK_DIR/setup-invocations"
CANDIDATE_CREDENTIAL_RECORD="$WORK_DIR/candidate-credential-environment"
SUPERVISOR_STATE_ROOT="$WORK_DIR/supervisor-state"
SUPERVISOR_STATE_FILE="$SUPERVISOR_STATE_ROOT/supervisor.json"
SUPERVISOR_STATE_BEFORE="$WORK_DIR/supervisor-state-before.json"
mkdir -p "$OLD_RELEASE/relay" "$NEW_RELEASE/relay" "$SOURCE_CONFIG/device-auth" \
    "$SOURCE_CONFIG/push" "$SOURCE_CONFIG/cloudflared" \
    "$TARGET_CONFIG/push" "$(dirname "$UNIT_FILE")" "$FAKE_BIN" "$SUPERVISOR_STATE_ROOT"
OLD_RELEASE="$(CDPATH='' cd "$OLD_RELEASE" && pwd -P)"
NEW_RELEASE="$(CDPATH='' cd "$NEW_RELEASE" && pwd -P)"

printf "HERDR_RELAY_TOKEN='source-token'\nHERDR_RELAY_INSTANCE_ID='source-instance'\nHERDR_RELAY_PORT='18375'\nHERDR_RELAY_SUPERVISOR_STATE_DIR='%s'\nHERDR_RELAY_PUBLIC_HEALTH_URL='https://remote.example.test/readyz'\nCLOUDFLARED_CONFIG='%s/cloudflared/config.yml'\n" \
    "$SUPERVISOR_STATE_ROOT" "$SOURCE_CONFIG" > "$SOURCE_ENV"
printf '{"schema_version":1,"credentials":[{"credential_id":"source-credential"}]}\n' \
    > "$SOURCE_CONFIG/device-auth/devices.json"
printf 'source-subscriptions\n' > "$SOURCE_CONFIG/push/subscriptions.json"
printf 'source-origin\n' > "$SOURCE_CONFIG/phone-app-origin"
printf 'source-configured-origin\n' > "$SOURCE_CONFIG/phone-app-origin-configured"
printf 'source-update\n' > "$SOURCE_CONFIG/update-state.json"
printf 'source-app-deploy\n' > "$SOURCE_CONFIG/app-deploy-state.json"
printf '{"pane":"source-profile"}\n' > "$SOURCE_CONFIG/pane-profile-associations.json"
printf '{"owner":"herdr-mobile-relay-stable-setup-v1","env_file":"%s/.env","config_path":"%s/cloudflared/config.yml"}\n' \
    "$SOURCE_CONFIG" "$SOURCE_CONFIG" > "$SOURCE_CONFIG/stable-setup.json"
printf 'credentials-file: %s/cloudflared/tunnel-credentials.json\n' \
    "$SOURCE_CONFIG" > "$SOURCE_CONFIG/cloudflared/config.yml"
printf "HERDR_RELAY_TOKEN='target-token'\nHERDR_GITHUB_TOKEN_FILE='%s/github-token'\n" \
    "$TARGET_CONFIG" > "$TARGET_CONFIG/relay.env"
printf 'target-subscriptions\n' > "$TARGET_CONFIG/push/subscriptions.json"
printf 'target-update\n' > "$TARGET_CONFIG/update-state.json"
printf 'persisted-private-token\n' > "$TARGET_CONFIG/github-token"
chmod 600 "$TARGET_CONFIG/github-token"
cp -pR "$TARGET_CONFIG" "$WORK_DIR/target-before"

printf '{\n  "version": "0.8.6",\n  "revision": "old-revision",\n  "web_hash": "old-web"\n}\n' \
    > "$OLD_RELEASE/release-manifest.json"
printf '{\n  "version": "%s",\n  "revision": "new-revision",\n  "web_hash": "new-web"\n}\n' \
    "$TEST_VERSION" > "$NEW_RELEASE/release-manifest.json"

cat > "$NEW_RELEASE/herdr-mobile-relay" <<'EOF'
#!/bin/sh
self_dir="$(CDPATH='' cd "$(dirname "$0")" && pwd -P)"
case "$1" in
    verify-release)
        if [ "$self_dir" = "$NEW_RELEASE" ] && [ "${CHECK_CANDIDATE_CREDENTIALS:-}" = 1 ]; then
            leaked=
            env | grep -Eq 'SENTINEL_|^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' && leaked="${leaked}environment "
            grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' "$HERDR_RELAY_ENV" && leaked="${leaked}relay.env "
            [ ! -e "$(dirname "$HERDR_RELAY_ENV")/github-token" ] || leaked="${leaked}github-token "
            printf '%s\n' "${leaked:-clean}" > "$CANDIDATE_CREDENTIAL_RECORD"
            [ -z "$leaked" ] || exit 91
        fi
        exit 0
        ;;
    activate-release)
        if [ "$self_dir" = "$NEW_RELEASE" ] && [ "${CANDIDATE_ACTIVATE_FAIL:-}" = 1 ]; then
            exit 92
        fi
        root=$2
        release=$3
        temp="$root/.current-test"
        rm -f "$temp"
        ln -s "$release" "$temp"
        rm -f "$root/current"
        mv -f "$temp" "$root/current"
        ;;
    supervisor-ready)
        current="$(CDPATH='' cd "$RELEASE_ROOT/current" && pwd -P)"
        printf '%s\n' "$current" >> "$READY_LOG"
        if [ "$current" != "$OLD_RELEASE" ] && [ "${REPLACEMENT_REVISION:-wrong-revision}" != new-revision ]; then
            printf '%s\n' '{"status":"tripped","failure_count":5,"generation":99}' > "$SUPERVISOR_STATE_FILE"
            exit 1
        fi
        if [ "$current" = "$OLD_RELEASE" ] && [ "${ROLLBACK_SUPERVISOR_READY_FAIL:-}" = 1 ]; then
            exit 1
        fi
        printf '%s\n' '{"status":"ready","instance":"source-instance"}'
        ;;
    *) exit 1 ;;
esac
EOF
chmod 700 "$NEW_RELEASE/herdr-mobile-relay"
cp "$NEW_RELEASE/herdr-mobile-relay" "$OLD_RELEASE/herdr-mobile-relay"
printf '#!/bin/sh\nexit 0\n' > "$OLD_RELEASE/relay/herdr-mobile-relay-service.sh"
printf '#!/bin/sh\nexit 0\n' > "$NEW_RELEASE/relay/herdr-mobile-relay-service.sh"
chmod 700 "$OLD_RELEASE/relay/herdr-mobile-relay-service.sh" \
    "$NEW_RELEASE/relay/herdr-mobile-relay-service.sh"
ln -s "releases/0.8.6-old" "$RELEASE_ROOT/current"

cat > "$UNIT_FILE" <<EOF
[Service]
Environment=HERDR_RELAY_ENV=$SOURCE_ENV
ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh
WorkingDirectory=$TEST_HOME/source-checkout
EOF
printf '#!/bin/sh\nexit 0\n' > "$SOURCE_CONFIG/herdr-mobile-relay-service.sh"
chmod 700 "$SOURCE_CONFIG/herdr-mobile-relay-service.sh"

FAKE_INSTALLER="$WORK_DIR/install.sh"
cat > "$FAKE_INSTALLER" <<EOF
#!/bin/sh
set -eu
printf '%s\n' "\$HERDR_PLUGIN_CONFIG_DIR" > "$CONFIG_RECORD"
if env | grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)='; then
    env | grep -E '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' > "$TOKEN_RECORD"
else
    printf '%s\n' clean > "$TOKEN_RECORD"
fi
printf '%s\n%s\n%s\n%s\n' "\${HERDR_RELEASE_ARCHIVE:-}" "\${HERDR_RELEASE_CHECKSUMS:-}" "\${HERDR_EXPECTED_REVISION:-}" "\${HERDR_EXPECTED_ARCHIVE_SHA256:-}" > "$INSTALL_INPUT_RECORD"
printf '%s\n' "\${HERDR_RELEASE_REPOSITORY:-}" > "$REPO_RECORD"
[ "\${FAIL_INSTALLER:-}" != 1 ] || exit 1
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp"
ln -s "$NEW_RELEASE" "\$temp"
rm -f "\$INSTALL_ROOT/current"
mv -f "\$temp" "\$INSTALL_ROOT/current"
EOF
chmod 700 "$FAKE_INSTALLER"

cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
case " $* " in
    *" is-active "*)
        if [ "${FORCE_INACTIVE:-}" = 1 ] && [ ! -s "$RESTART_LOG" ]; then
            printf 'inactive\n'
            exit 3
        fi
        printf 'active\n'
        exit 0
        ;;
    *" restart "*)
        printf 'restart\n' >> "$RESTART_LOG"
        if grep -Fx "ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh" "$UNIT_FILE" 2>/dev/null >/dev/null ||
           [ "$(readlink -f "$RELEASE_ROOT/current" 2>/dev/null || true)" = "$OLD_RELEASE" ]; then
            if [ "${ROLLBACK_HEALTH_FAIL:-}" = 1 ]; then
                printf '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"wrong-rollback-revision","bundle_hash":"old-web"}\n' > "$HEALTH_FILE"
            else
                printf '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"old-revision","bundle_hash":"old-web"}\n' > "$HEALTH_FILE"
            fi
        else
            printf '{"status":"ok","instance":"test","version":"%s","protocol":2,"release_version":"%s","revision":"%s","bundle_hash":"new-web"}\n' \
                "$TEST_VERSION" "$TEST_VERSION" "${REPLACEMENT_REVISION:-wrong-revision}" > "$HEALTH_FILE"
        fi
        exit 0
        ;;
    *) exit 0 ;;
esac
EOF
cat > "$FAKE_BIN/curl" <<'EOF'
#!/bin/sh
output=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --url) url=$2; shift 2 ;;
        http://*|https://*|mock://*) url=$1; shift ;;
        *) shift ;;
    esac
done
case "$url" in
    */commits/v*) body='{"sha":"0123456789abcdef0123456789abcdef01234567"}' ;;
    */releases/tags/v*) body="{\"assets\":[{\"url\":\"mock://archive\",\"name\":\"herdr-mobile-relay_${TEST_VERSION}_linux_amd64.tar.gz\"},{\"url\":\"mock://checksums\",\"name\":\"checksums.txt\"}]}" ;;
    mock://archive) body='fake release archive' ;;
    mock://checksums) body='fake release checksums' ;;
    *"api.github.com/repos/"*)
        [ "${GH_API_PUBLIC:-}" = 1 ] || exit 22
        body='{}'
        ;;
    *) body="$(cat "$HEALTH_FILE")" ;;
esac
if [ -n "$output" ]; then
    printf '%s\n' "$body" > "$output"
else
    printf '%s\n' "$body"
fi
EOF
cat > "$FAKE_BIN/herdr" <<'EOF'
#!/bin/sh
if [ "$*" = "plugin config-dir herdr-mobile-relay.events" ]; then
    printf '%s\n' "$TARGET_CONFIG"
    exit 0
fi
case "$*" in
    'plugin action invoke setup --plugin herdr-mobile-relay.events')
        printf '%s\n' "$*" >> "$SETUP_RECORD"
        exit 0
        ;;
esac
exit 1
EOF
cat > "$FAKE_BIN/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$FAKE_BIN/gh" <<'EOF'
#!/bin/sh
[ "${GH_AUTH_FAIL:-}" != 1 ] || exit 1
if [ "$*" = "auth token --hostname github.com" ]; then
    printf '%s\n' 'private-clone-api-token'
    exit 0
fi
exit 1
EOF
cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
    -m) printf 'x86_64\n' ;;
    *) printf 'Linux\n' ;;
esac
EOF
chmod 700 "$FAKE_BIN/gh"
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/curl" "$FAKE_BIN/herdr" "$FAKE_BIN/sleep"
chmod 700 "$FAKE_BIN/uname"

export SOURCE_CONFIG TARGET_CONFIG UNIT_FILE HEALTH_FILE TEST_VERSION RESTART_LOG READY_LOG
export SETUP_RECORD
export RELEASE_ROOT OLD_RELEASE NEW_RELEASE CANDIDATE_CREDENTIAL_RECORD INSTALL_INPUT_RECORD
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    HERDR_RELEASE_REPOSITORY=0cv/herdr-mobile-relay-dev \
    GH_TOKEN='unsafe token' \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/unsafe-token-output" 2>&1; then
    echo "plugin build accepted an unsafe release token" >&2
    exit 1
fi
grep -F 'could not stage the authenticated release for credential-free installation' "$WORK_DIR/unsafe-token-output" >/dev/null

printf 'outside-source-data\n' > "$WORK_DIR/nested-symlink-target"
ln -s "$WORK_DIR/nested-symlink-target" "$SOURCE_CONFIG/device-auth/nested-link"
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 \
    REPLACEMENT_REVISION=new-revision \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/nested-symlink-output" 2>&1; then
    echo "plugin migration accepted a nested symlink from service state" >&2
    exit 1
fi
test ! -e "$CONFIG_RECORD"
rm -f "$SOURCE_CONFIG/device-auth/nested-link"

OUTSIDE_INSTALL_ROOT="$WORK_DIR/outside-install-root"
OUTSIDE_PREVIOUS_RELEASE="$WORK_DIR/outside-previous-release"
OUTSIDE_PREVIOUS_EXECUTION="$WORK_DIR/outside-previous-execution"
mkdir -p "$OUTSIDE_INSTALL_ROOT" "$OUTSIDE_PREVIOUS_RELEASE"
cat > "$OUTSIDE_PREVIOUS_RELEASE/herdr-mobile-relay" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$OUTSIDE_PREVIOUS_EXECUTION"
exit 0
EOF
chmod 700 "$OUTSIDE_PREVIOUS_RELEASE/herdr-mobile-relay"
printf '{"version":"outside","revision":"outside","web_hash":"outside"}\n' > "$OUTSIDE_PREVIOUS_RELEASE/release-manifest.json"
ln -s "$OUTSIDE_PREVIOUS_RELEASE" "$OUTSIDE_INSTALL_ROOT/current"
export OUTSIDE_PREVIOUS_EXECUTION
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$OUTSIDE_INSTALL_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    FAIL_INSTALLER=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/outside-previous-output" 2>&1; then
    echo "plugin migration unexpectedly accepted an installer failure with an external prior release" >&2
    exit 1
fi
if [ -e "$OUTSIDE_PREVIOUS_EXECUTION" ]; then
    echo "plugin rollback trusted or executed a previous release outside its releases directory" >&2
    exit 1
fi

if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    FAIL_INSTALLER=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/pre-cutover-output" 2>&1; then
    echo "plugin migration unexpectedly accepted an installer failure" >&2
    exit 1
fi
test ! -e "$RESTART_LOG"
diff -qr "$WORK_DIR/target-before" "$TARGET_CONFIG" >/dev/null
grep -F "previous running service was left untouched" "$WORK_DIR/pre-cutover-output" >/dev/null || {
    cat "$WORK_DIR/pre-cutover-output" >&2
    echo "pre-cutover failure did not confirm the previous service stayed untouched" >&2
    exit 1
}

printf '%s\n' '{"status":"stopped","failure_count":1,"generation":7}' > "$SUPERVISOR_STATE_FILE"
cp "$SUPERVISOR_STATE_FILE" "$SUPERVISOR_STATE_BEFORE"
export SUPERVISOR_STATE_FILE
cat >> "$SOURCE_ENV" <<EOF
GH_TOKEN=SENTINEL_MIGRATED_GH_TOKEN
GITHUB_TOKEN=SENTINEL_MIGRATED_GITHUB_TOKEN
HERDR_GITHUB_TOKEN_FILE=$SOURCE_CONFIG/github-token
EOF
printf 'SENTINEL_MIGRATED_TOKEN_FILE\n' > "$SOURCE_CONFIG/github-token"
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    GH_TOKEN=ambient-private-token \
    CHECK_CANDIDATE_CREDENTIALS=1 \
    CANDIDATE_ACTIVATE_FAIL=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
    cat "$WORK_DIR/output" >&2
    exit 1
fi

if [ "$(readlink -f "$RELEASE_ROOT/current")" != "$OLD_RELEASE" ]; then
    echo "plugin rollback delegated reactivation to the failed candidate" >&2
    exit 1
fi
test "$(cat "$CANDIDATE_CREDENTIAL_RECORD")" = clean || {
    echo "downloaded candidate observed GitHub credentials or pointers: $(cat "$CANDIDATE_CREDENTIAL_RECORD")" >&2
    exit 1
}
grep -Fx "ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$TEST_HOME/source-checkout" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$SOURCE_ENV" "$UNIT_FILE" >/dev/null
test "$(cat "$CONFIG_RECORD")" = "$TARGET_CONFIG"
test "$(cat "$TOKEN_RECORD")" = clean
installer_archive="$(sed -n '1p' "$INSTALL_INPUT_RECORD")"
installer_checksums="$(sed -n '2p' "$INSTALL_INPUT_RECORD")"
test "${installer_archive##*/}" = "herdr-mobile-relay_${TEST_VERSION}_linux_amd64.tar.gz"
test "${installer_checksums##*/}" = checksums.txt
test "${installer_archive%/*}" = "${installer_checksums%/*}"
test "$(sed -n '3p' "$INSTALL_INPUT_RECORD")" = 0123456789abcdef0123456789abcdef01234567
case "$(sed -n '4p' "$INSTALL_INPUT_RECORD")" in
    ????????????????????????????????????????????????????????????????) ;;
    *) echo "credential-free installer did not receive an exact staged archive digest" >&2; exit 1 ;;
esac
diff -qr "$WORK_DIR/target-before" "$TARGET_CONFIG" >/dev/null
grep -F "previous service recovered successfully" "$WORK_DIR/output" >/dev/null
grep -Fx "$NEW_RELEASE" "$READY_LOG" >/dev/null || {
    echo "replacement rollback gate never invoked exact supervisor readiness" >&2
    exit 1
}
if ! cmp -s "$SUPERVISOR_STATE_BEFORE" "$SUPERVISOR_STATE_FILE"; then
    echo "plugin rollback left the restored service supervisor state tripped" >&2
    exit 1
fi

if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    ROLLBACK_SUPERVISOR_READY_FAIL=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/unproved-rollback-output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
    exit 1
fi
grep -F 'ERROR: automatic rollback also failed' "$WORK_DIR/unproved-rollback-output" >/dev/null || {
    echo "plugin rollback declared success without exact restored supervisor readiness" >&2
    exit 1
}
if grep -F 'previous service recovered successfully' "$WORK_DIR/unproved-rollback-output" >/dev/null; then
    echo "plugin rollback declared endpoint-only recovery successful" >&2
    exit 1
fi
retained_config_backup=$(sed -n 's/^herdr-mobile-relay: rollback recovery data retained at //p' "$WORK_DIR/unproved-rollback-output" | tail -1)
[ -d "$retained_config_backup" ] || {
    echo "plugin rollback discarded recovery data after readiness proof failed" >&2
    exit 1
}

rm -f "$RESTART_LOG"
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    ROLLBACK_HEALTH_FAIL=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/rollback-health-output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
    exit 1
fi
grep -F 'ERROR: automatic rollback also failed' "$WORK_DIR/rollback-health-output" >/dev/null || {
    echo "plugin rollback declared success without exact restored endpoint identity" >&2
    exit 1
}

rm -f "$RESTART_LOG"
if ! HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 \
    FORCE_INACTIVE=1 \
    REPLACEMENT_REVISION=new-revision \
    HERDR_RELEASE_REPOSITORY=0cv/herdr-mobile-relay-dev \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/success-output" 2>&1; then
    cat "$WORK_DIR/success-output" >&2
    exit 1
fi

test "$(readlink -f "$RELEASE_ROOT/current")" = "$NEW_RELEASE"
# The release comes from the repository the plugin was installed from, so a
# private canary or a fork never downloads this project's bundle.
test "$(cat "$REPO_RECORD")" = 0cv/herdr-mobile-relay-dev
grep -Fx "ExecStart=$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$RELEASE_ROOT/current" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$TARGET_CONFIG/relay.env" "$UNIT_FILE" >/dev/null
grep -F source-token "$TARGET_CONFIG/relay.env" >/dev/null
grep -F source-instance "$TARGET_CONFIG/relay.env" >/dev/null
if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' "$TARGET_CONFIG/relay.env"; then
    echo "successful plugin migration persisted a GitHub credential or pointer" >&2
    exit 1
fi
test ! -e "$TARGET_CONFIG/github-token"
grep -F source-credential "$TARGET_CONFIG/device-auth/devices.json" >/dev/null
test "$(cat "$TARGET_CONFIG/push/subscriptions.json")" = source-subscriptions
test "$(cat "$TARGET_CONFIG/update-state.json")" = source-update
test "$(cat "$TARGET_CONFIG/app-deploy-state.json")" = source-app-deploy
cmp -s "$SOURCE_CONFIG/pane-profile-associations.json" "$TARGET_CONFIG/pane-profile-associations.json"
cp -p "$TARGET_CONFIG/pane-profile-associations.json" "$WORK_DIR/pane-profile-associations-after-migration.json"
test "$(cat "$TARGET_CONFIG/phone-app-origin")" = source-origin
test "$(cat "$TARGET_CONFIG/phone-app-origin-configured")" = source-configured-origin
grep -F "$TARGET_CONFIG/relay.env" "$TARGET_CONFIG/stable-setup.json" >/dev/null
grep -F "$TARGET_CONFIG/cloudflared/config.yml" "$TARGET_CONFIG/stable-setup.json" >/dev/null
grep -F "$TARGET_CONFIG/cloudflared/tunnel-credentials.json" \
    "$TARGET_CONFIG/cloudflared/config.yml" >/dev/null
test ! -e "$SOURCE_CONFIG/.herdr-mobile-relay-installation"
test ! -e "$REPO_DIR/relay/.herdr-mobile-relay-installation"
test "$(cat "$RESTART_LOG")" = "restart"

BROKEN_ROOT="$WORK_DIR/deleted-release"
cat > "$UNIT_FILE" <<EOF
[Service]
Environment=HERDR_RELAY_ENV=$BROKEN_ROOT/relay.env
ExecStart=$BROKEN_ROOT/relay/herdr-mobile-relay-service.sh
WorkingDirectory=$BROKEN_ROOT
EOF
rm -f "$RELEASE_ROOT/current" "$RESTART_LOG"
mv "$TARGET_CONFIG/relay.env" "$WORK_DIR/persistent-relay.env"
cp "$UNIT_FILE" "$WORK_DIR/broken-unit-before"

if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    FORCE_INACTIVE=1 \
    REPLACEMENT_REVISION=new-revision \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/missing-config-output" 2>&1; then
    echo "broken service recovery unexpectedly ran without persistent config" >&2
    exit 1
fi
diff -q "$WORK_DIR/broken-unit-before" "$UNIT_FILE" >/dev/null
test ! -e "$RESTART_LOG"
grep -F "persistent relay environment is unavailable" \
    "$WORK_DIR/missing-config-output" >/dev/null

mv "$WORK_DIR/persistent-relay.env" "$TARGET_CONFIG/relay.env"
if ! HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    FORCE_INACTIVE=1 \
    REPLACEMENT_REVISION=new-revision \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/recovery-output" 2>&1; then
    cat "$WORK_DIR/recovery-output" >&2
    exit 1
fi

grep -F "recovering broken service paths from persistent plugin config" \
    "$WORK_DIR/recovery-output" >/dev/null
test "$(readlink -f "$RELEASE_ROOT/current")" = "$NEW_RELEASE"
grep -Fx "ExecStart=$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh" \
    "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$RELEASE_ROOT/current" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$TARGET_CONFIG/relay.env" "$UNIT_FILE" >/dev/null
test "$(cat "$RESTART_LOG")" = "restart"
grep -F source-token "$TARGET_CONFIG/relay.env" >/dev/null
cmp -s "$WORK_DIR/pane-profile-associations-after-migration.json" "$TARGET_CONFIG/pane-profile-associations.json" || {
    echo "plugin reinstall changed the migrated pane-profile associations" >&2
    exit 1
}

rm -f "$RELEASE_ROOT/current" "$RESTART_LOG"
ln -s "releases/0.8.6-old" "$RELEASE_ROOT/current"
cat > "$UNIT_FILE" <<EOF
[Service]
Environment=HERDR_RELAY_ENV=$BROKEN_ROOT/relay.env
ExecStart=$BROKEN_ROOT/relay/herdr-mobile-relay-service.sh
WorkingDirectory=$BROKEN_ROOT
EOF
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    FORCE_INACTIVE=1 \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/recovery-rollback-output" 2>&1; then
    echo "broken service recovery unexpectedly accepted the wrong replacement identity" >&2
    exit 1
fi

test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
grep -Fx "ExecStart=$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh" \
    "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$RELEASE_ROOT/current" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$TARGET_CONFIG/relay.env" "$UNIT_FILE" >/dev/null
test "$(wc -l < "$RESTART_LOG")" -eq 2
grep -F "previous service recovered successfully" \
    "$WORK_DIR/recovery-rollback-output" >/dev/null
cmp -s "$WORK_DIR/pane-profile-associations-after-migration.json" "$TARGET_CONFIG/pane-profile-associations.json" || {
    echo "plugin rollback did not restore pane-profile associations byte-for-byte" >&2
    exit 1
}

# --- Every install opens the setup menu --------------------------------------
# Nobody sees this script's output, so an install that stops after "release is
# ready" leaves a person with no idea what exists or what is still missing. The
# menu answers both and costs one keystroke to leave, so an upgrade opens it too.
sleep 1
grep -Fq 'plugin action invoke setup --plugin herdr-mobile-relay.events' "$SETUP_RECORD" ||
    { echo "an upgrade did not open the setup menu" >&2; exit 1; }
rm -f "$SETUP_RECORD"

FRESH_HOME="$WORK_DIR/fresh-home"
FRESH_ROOT="$FRESH_HOME/releases"
FRESH_CONFIG="$FRESH_HOME/config"
FRESH_RELEASE="$FRESH_ROOT/releases/$TEST_VERSION-new"
mkdir -p "$FRESH_RELEASE/relay" "$FRESH_CONFIG"
cp "$NEW_RELEASE/release-manifest.json" "$FRESH_RELEASE/"
cp "$NEW_RELEASE/herdr-mobile-relay" "$FRESH_RELEASE/"
cp "$NEW_RELEASE/relay/herdr-mobile-relay-service.sh" "$FRESH_RELEASE/relay/"
FRESH_INSTALLER="$WORK_DIR/fresh-install.sh"
cat > "$FRESH_INSTALLER" <<EOF
#!/bin/sh
set -eu
if env | grep -Eq '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)='; then
    env | grep -E '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' > "$FRESH_TOKEN_RECORD"
else
    printf '%s\n' clean > "$FRESH_TOKEN_RECORD"
fi
printf '%s\n%s\n%s\n%s\n' "\${HERDR_RELEASE_ARCHIVE:-}" "\${HERDR_RELEASE_CHECKSUMS:-}" "\${HERDR_EXPECTED_REVISION:-}" "\${HERDR_EXPECTED_ARCHIVE_SHA256:-}" > "$FRESH_INSTALL_INPUT_RECORD"
printf '%s\n' "\${HERDR_RELEASE_REPOSITORY:-}" > "$FRESH_REPO_RECORD"
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp"
ln -s "$FRESH_RELEASE" "\$temp"
rm -f "\$INSTALL_ROOT/current"
mv -f "\$temp" "\$INSTALL_ROOT/current"
EOF
chmod 700 "$FRESH_INSTALLER"
rm -f "$RESTART_LOG"

# A machine that never ran this relay: no unit file, nothing active, no release
# under the root.
run_fresh_build() {
    HOME="$FRESH_HOME" \
        PATH="$FAKE_BIN:$PATH" \
        HERDR_RELEASE_ROOT="$FRESH_ROOT" \
        HERDR_PLUGIN_CONFIG_DIR="$FRESH_CONFIG" \
        HERDR_PLUGIN_INSTALLER="$FRESH_INSTALLER" \
        UNIT_FILE="$FRESH_HOME/.config/systemd/user/herdr-mobile-relay.service" \
        REPLACEMENT_REVISION=new-revision \
        FORCE_INACTIVE=1 \
        GH_TOKEN= \
        GITHUB_TOKEN= \
        "$@" \
        bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/fresh-output" 2>&1
}

# An SSH-only private checkout has no credential for GitHub's HTTPS release API.
# Diagnose that before migration instead of surfacing GitHub's deliberate 404.
if run_fresh_build env GH_AUTH_FAIL=1 \
    HERDR_RELEASE_REPOSITORY=0cv/herdr-mobile-relay-dev; then
    echo "an SSH-only private install reached the release installer" >&2
    exit 1
fi
grep -Fq 'SSH access cloned the plugin source' "$WORK_DIR/fresh-output" ||
    { echo "private release failure did not explain the SSH/API boundary" >&2; exit 1; }
grep -Fq 'gh auth login --hostname github.com --git-protocol ssh' \
    "$WORK_DIR/fresh-output" ||
    { echo "private release failure gave no authentication command" >&2; exit 1; }
[ ! -e "$FRESH_ROOT/current" ] ||
    { echo "private release authentication failure changed the release" >&2; exit 1; }

if ! run_fresh_build env HERDR_RELEASE_REPOSITORY=0cv/herdr-mobile-relay-dev; then
    cat "$WORK_DIR/fresh-output" >&2
    exit 1
fi
test "$(cat "$FRESH_TOKEN_RECORD")" = clean ||
    { echo "a private checkout exposed its gh API credential to the release installer" >&2; exit 1; }
test -n "$(sed -n '1p' "$FRESH_INSTALL_INPUT_RECORD")" ||
    { echo "a private checkout did not stage a credential-free offline release" >&2; exit 1; }
test "$(cat "$FRESH_REPO_RECORD")" = "0cv/herdr-mobile-relay-dev" ||
    { echo "a private checkout downloaded from the wrong release repository" >&2; exit 1; }
# The action is scheduled detached, so give it the moment it waits out.
sleep 1
grep -Fq 'plugin action invoke setup --plugin herdr-mobile-relay.events' "$SETUP_RECORD" ||
    { echo "a first install did not open the setup menu" >&2; exit 1; }

# Only the documented opt-out suppresses it; a configured relay still gets the
# menu, because seeing the current state is the point.
rm -f "$SETUP_RECORD" "$FRESH_ROOT/current" "$RESTART_LOG"
if ! run_fresh_build env HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1; then
    cat "$WORK_DIR/fresh-output" >&2
    exit 1
fi
sleep 1
if [ -e "$SETUP_RECORD" ]; then
    echo "HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 still opened the setup menu" >&2
    exit 1
fi

rm -f "$FRESH_ROOT/current" "$RESTART_LOG"
printf "HERDR_GATEWAY_URL='wss://gw.example.test'\n" >> "$FRESH_CONFIG/relay.env"
if ! run_fresh_build env; then
    cat "$WORK_DIR/fresh-output" >&2
    exit 1
fi
sleep 1
grep -Fq 'plugin action invoke setup --plugin herdr-mobile-relay.events' "$SETUP_RECORD" ||
    { echo "a configured relay did not open the setup menu" >&2; exit 1; }

REAL_CP=$(command -v cp)
REAL_MV=$(command -v mv)
RESTORE_RENAME_FAILURE_MARKER="$WORK_DIR/restore-rename-failed"
export REAL_CP REAL_MV RESTORE_RENAME_FAILURE_MARKER
cat > "$FAKE_BIN/cp" <<'EOF'
#!/bin/sh
source_path=
for argument in "$@"; do
    case "$argument" in
        -*) ;;
        *) [ -n "$source_path" ] || source_path=$argument ;;
    esac
done
case "$source_path" in
    */.herdr-plugin-config-backup.*/*)
        [ "${FAIL_CONFIG_RESTORE_COPY:-}" != 1 ] || exit 97
        ;;
esac
exec "$REAL_CP" "$@"
EOF
cat > "$FAKE_BIN/mv" <<'EOF'
#!/bin/sh
case " $* " in
    *'/.herdr-plugin-config-restore.'*)
        if [ "${FAIL_CONFIG_RESTORE_RENAME:-}" = 1 ] && [ ! -e "$RESTORE_RENAME_FAILURE_MARKER" ]; then
            : > "$RESTORE_RENAME_FAILURE_MARKER"
            exit 98
        fi
        ;;
esac
exec "$REAL_MV" "$@"
EOF
chmod 700 "$FAKE_BIN/cp" "$FAKE_BIN/mv"

if HOME="$TEST_HOME" PATH="$FAKE_BIN:$PATH" HERDR_RELEASE_ROOT="$RELEASE_ROOT" HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" FAIL_CONFIG_RESTORE_COPY=1 bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/restore-copy-failure-output" 2>&1; then
    echo "config restore copy failure unexpectedly completed the plugin migration" >&2
    exit 1
fi
copy_failure_backup=$(sed -n 's/^herdr-mobile-relay: rollback recovery data retained at //p' "$WORK_DIR/restore-copy-failure-output" | tail -1)
[ -d "$copy_failure_backup" ] && [ -f "$copy_failure_backup/relay.env" ] && [ -d "$TARGET_CONFIG" ] || {
    echo "config restore copy failure discarded the live tree or its recovery backup" >&2
    exit 1
}

cp -pR "$copy_failure_backup/." "$TARGET_CONFIG/"
rm -f "$RESTORE_RENAME_FAILURE_MARKER"
if HOME="$TEST_HOME" PATH="$FAKE_BIN:$PATH" HERDR_RELEASE_ROOT="$RELEASE_ROOT" HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" FAIL_CONFIG_RESTORE_RENAME=1 bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/restore-rename-failure-output" 2>&1; then
    echo "config restore rename failure unexpectedly completed the plugin migration" >&2
    exit 1
fi
rename_failure_backup=$(sed -n 's/^herdr-mobile-relay: rollback recovery data retained at //p' "$WORK_DIR/restore-rename-failure-output" | tail -1)
[ -e "$RESTORE_RENAME_FAILURE_MARKER" ] && [ -d "$rename_failure_backup" ] && [ -d "$TARGET_CONFIG" ] || {
    echo "config restore rename failure did not restore or retain recoverable config state" >&2
    exit 1
}

echo "plugin build migration, rollback, and recovery tests passed"

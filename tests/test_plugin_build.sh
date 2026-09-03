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
REPO_RECORD="$WORK_DIR/installer-repository"
FRESH_TOKEN_RECORD="$WORK_DIR/fresh-installer-token"
FRESH_REPO_RECORD="$WORK_DIR/fresh-installer-repository"
RESTART_LOG="$WORK_DIR/restarts"
SETUP_RECORD="$WORK_DIR/setup-invocations"
mkdir -p "$OLD_RELEASE/relay" "$NEW_RELEASE/relay" "$SOURCE_CONFIG/device-auth" \
    "$SOURCE_CONFIG/push" "$SOURCE_CONFIG/cloudflared" \
    "$TARGET_CONFIG/push" "$(dirname "$UNIT_FILE")" "$FAKE_BIN"

printf "HERDR_RELAY_TOKEN='source-token'\nHERDR_RELAY_INSTANCE_ID='source-instance'\nHERDR_RELAY_PORT='18375'\nCLOUDFLARED_CONFIG='%s/cloudflared/config.yml'\n" \
    "$SOURCE_CONFIG" > "$SOURCE_ENV"
printf '{"schema_version":1,"credentials":[{"credential_id":"source-credential"}]}\n' \
    > "$SOURCE_CONFIG/device-auth/devices.json"
printf 'source-subscriptions\n' > "$SOURCE_CONFIG/push/subscriptions.json"
printf 'source-origin\n' > "$SOURCE_CONFIG/phone-app-origin"
printf 'source-configured-origin\n' > "$SOURCE_CONFIG/phone-app-origin-configured"
printf 'source-update\n' > "$SOURCE_CONFIG/update-state.json"
printf 'source-app-deploy\n' > "$SOURCE_CONFIG/app-deploy-state.json"
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
case "$1" in
    verify-release) exit 0 ;;
    activate-release)
        root=$2
        release=$3
        temp="$root/.current-test"
        rm -f "$temp"
        ln -s "$release" "$temp"
        mv -Tf "$temp" "$root/current"
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
printf '%s\n' "\${GH_TOKEN:-}" > "$TOKEN_RECORD"
printf '%s\n' "\${HERDR_RELEASE_REPOSITORY:-}" > "$REPO_RECORD"
[ "\${FAIL_INSTALLER:-}" != 1 ] || exit 1
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp"
ln -s "$NEW_RELEASE" "\$temp"
mv -Tf "\$temp" "\$INSTALL_ROOT/current"
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
            printf '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"old-revision","bundle_hash":"old-web"}\n' > "$HEALTH_FILE"
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
case "$*" in
    *"api.github.com/repos/"*)
        [ "${GH_API_PUBLIC:-}" = 1 ] || exit 22
        printf '{}\n'
        ;;
    *) cat "$HEALTH_FILE" ;;
esac
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
chmod 700 "$FAKE_BIN/gh"
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/curl" "$FAKE_BIN/herdr" "$FAKE_BIN/sleep"

export SOURCE_CONFIG TARGET_CONFIG UNIT_FILE HEALTH_FILE TEST_VERSION RESTART_LOG
export SETUP_RECORD
export RELEASE_ROOT OLD_RELEASE
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
grep -F "previous running service was left untouched" "$WORK_DIR/pre-cutover-output" >/dev/null

if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
    cat "$WORK_DIR/output" >&2
    exit 1
fi

test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
grep -Fx "ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$TEST_HOME/source-checkout" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$SOURCE_ENV" "$UNIT_FILE" >/dev/null
test "$(cat "$CONFIG_RECORD")" = "$TARGET_CONFIG"
test "$(cat "$TOKEN_RECORD")" = "persisted-private-token"
diff -qr "$WORK_DIR/target-before" "$TARGET_CONFIG" >/dev/null
grep -F "previous service recovered successfully" "$WORK_DIR/output" >/dev/null

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
grep -F "HERDR_GITHUB_TOKEN_FILE='$TARGET_CONFIG/github-token'" "$TARGET_CONFIG/relay.env" >/dev/null
grep -F source-credential "$TARGET_CONFIG/device-auth/devices.json" >/dev/null
test "$(cat "$TARGET_CONFIG/push/subscriptions.json")" = source-subscriptions
test "$(cat "$TARGET_CONFIG/update-state.json")" = source-update
test "$(cat "$TARGET_CONFIG/app-deploy-state.json")" = source-app-deploy
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
printf '%s\n' "\${GH_TOKEN:-}" > "$FRESH_TOKEN_RECORD"
printf '%s\n' "\${HERDR_RELEASE_REPOSITORY:-}" > "$FRESH_REPO_RECORD"
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp"
ln -s "$FRESH_RELEASE" "\$temp"
mv -Tf "\$temp" "\$INSTALL_ROOT/current"
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
test "$(cat "$FRESH_TOKEN_RECORD")" = "private-clone-api-token" ||
    { echo "a private checkout did not reuse the gh API credential" >&2; exit 1; }
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

echo "plugin build migration, rollback, and recovery tests passed"

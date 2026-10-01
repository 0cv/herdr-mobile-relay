#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
unit_exec_line() { printf 'ExecStart=%s' "$(systemd_quoted "$1" exec)"; }
unit_work_line() { printf 'WorkingDirectory=%s' "$(systemd_quoted "$1")"; }
unit_env_line() { printf 'Environment=%s' "$(systemd_quoted "HERDR_RELAY_ENV=$1")"; }
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-build-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT
export HERDR_TEST_READINESS_BIN="$WORK_DIR/readiness-helper"
go build -o "$HERDR_TEST_READINESS_BIN" "$REPO_DIR/cmd/herdr-mobile-relay"

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
canonical_dir() {
    CDPATH='' cd "$1" && pwd -P
}
OLD_RELEASE=$(canonical_dir "$OLD_RELEASE")
NEW_RELEASE=$(canonical_dir "$NEW_RELEASE")

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
printf 'hostname: relay.example.test\ncredentials-file: %s/cloudflared/tunnel-credentials.json\n' \
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
# Fixture adapter for the exact flat health fields used by this test.
case "$1" in
    json-field|verify-readiness) exec "$HERDR_TEST_READINESS_BIN" "$@" ;;
    verify-release) exit 0 ;;
    activate-release)
        root=$2
        release=$3
        temp="$root/.current-test"
        rm -f "$temp" "$root/current"
        ln -s "$release" "$temp"
        mv -f "$temp" "$root/current"
        ;;
    tailscale-cli)
        [ "${2:-}" = assert-ready ] || exit 1
        printf '%s|%s\n' "$0" "$*" >> "$CLI_ROUTE_RECORD"
        if [ "${CLI_POST_ROUTE_FAIL:-}" = 1 ] &&
           [ "$(readlink -f "$0" 2>/dev/null || true)" = "$NEW_RELEASE/herdr-mobile-relay" ]; then
            exit 42
        fi
        printf '%s\n' '{"route":{"journal_state":"registered","readiness":"ready","runtime_qualified":true}}'
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
cp "$SOURCE_ENV" "$WORK_DIR/source-env-original"
cp "$UNIT_FILE" "$WORK_DIR/initial-unit"

FAKE_INSTALLER="$WORK_DIR/install.sh"
cat > "$FAKE_INSTALLER" <<EOF
#!/bin/sh
set -eu
printf '%s\n' "\$HERDR_PLUGIN_CONFIG_DIR" > "$CONFIG_RECORD"
printf '%s\n' "\${GH_TOKEN:-}" > "$TOKEN_RECORD"
printf '%s\n' "\${HERDR_RELEASE_REPOSITORY:-}" > "$REPO_RECORD"
[ "\${FAIL_INSTALLER:-}" != 1 ] || exit 1
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp" "\$INSTALL_ROOT/current"
ln -s "$NEW_RELEASE" "\$temp"
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
            printf '{"status":"ready","inventory":{"state":"ready"},"instance":"source-instance","version":"0.8.6","protocol":3,"release_version":"0.8.6","revision":"old-revision","bundle_hash":"old-web"}\n' > "$HEALTH_FILE"
        else
            printf '{"status":"ready","inventory":{"state":"ready"},"instance":"source-instance","version":"%s","protocol":3,"release_version":"%s","revision":"%s","bundle_hash":"new-web"}\n' \
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
    *)
        if [ -n "${CLI_HEALTH_RECORD:-}" ]; then cat "$HEALTH_FILE" >> "$CLI_HEALTH_RECORD"; fi
        cat "$HEALTH_FILE"
        ;;
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
cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
printf 'Linux\n'
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
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/curl" "$FAKE_BIN/herdr" \
    "$FAKE_BIN/sleep" "$FAKE_BIN/uname"

export REPO_DIR SOURCE_CONFIG TARGET_CONFIG UNIT_FILE HEALTH_FILE TEST_VERSION RESTART_LOG
export SETUP_RECORD
export RELEASE_ROOT OLD_RELEASE NEW_RELEASE
# CLI-backed Serve has no qualified update path; refuse before the installer,
# service manager, release pointer, or persisted state is touched.
printf "HERDR_RELAY_TRANSPORT='tailscale-cli'\n" >> "$TARGET_CONFIG/relay.env"
cp -pR "$TARGET_CONFIG" "$WORK_DIR/cli-target-before"
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/cli-output" 2>&1; then
    echo "CLI-backed Serve unexpectedly admitted a phone-managed update" >&2
    exit 1
fi
grep -F "CLI-backed Tailscale Serve updates are not qualified or available" "$WORK_DIR/cli-output" >/dev/null
test ! -e "$RESTART_LOG"
test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
diff -qr "$WORK_DIR/cli-target-before" "$TARGET_CONFIG" >/dev/null
cp "$WORK_DIR/target-before/relay.env" "$TARGET_CONFIG/relay.env"
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
    bash -x "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/output" 2>&1; then
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
if grep -F 'persisted-private-token' "$WORK_DIR/output" >/dev/null; then
    echo "plugin tracing exposed the persisted credential" >&2
    exit 1
fi
diff -qr "$WORK_DIR/target-before" "$TARGET_CONFIG" >/dev/null
grep -F "previous service recovered successfully" "$WORK_DIR/output" >/dev/null || {
    echo "previous service did not recover after replacement identity refusal" >&2
    cat "$WORK_DIR/output" >&2
    echo "fake service restarts:" >&2
    if [ -f "$RESTART_LOG" ]; then cat "$RESTART_LOG" >&2; else echo "<none>" >&2; fi
    echo "fake health response:" >&2
    if [ -f "$HEALTH_FILE" ]; then cat "$HEALTH_FILE" >&2; else echo "<none>" >&2; fi
    echo "restored unit:" >&2
    if [ -f "$UNIT_FILE" ]; then cat "$UNIT_FILE" >&2; else echo "<none>" >&2; fi
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
grep -Fx "$(unit_exec_line "$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh")" "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_work_line "$RELEASE_ROOT/current")" "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_env_line "$TARGET_CONFIG/relay.env")" "$UNIT_FILE" >/dev/null
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
grep -Fx "$(unit_exec_line "$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh")" \
    "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_work_line "$RELEASE_ROOT/current")" "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_env_line "$TARGET_CONFIG/relay.env")" "$UNIT_FILE" >/dev/null
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
grep -Fx "$(unit_exec_line "$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh")" \
    "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_work_line "$RELEASE_ROOT/current")" "$UNIT_FILE" >/dev/null
grep -Fx "$(unit_env_line "$TARGET_CONFIG/relay.env")" "$UNIT_FILE" >/dev/null
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
rm -f "\$temp" "\$INSTALL_ROOT/current"
ln -s "$FRESH_RELEASE" "\$temp"
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
        GH_TOKEN='' \
        GITHUB_TOKEN='' \
        "$@" \
        bash -x "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/fresh-output" 2>&1
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
if grep -F 'private-clone-api-token' "$WORK_DIR/fresh-output" >/dev/null; then
    echo "plugin tracing exposed the CLI credential" >&2
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

# --- Explicit operator-managed CLI package update ---------------------------
# Exercise the complete update entrypoint with only a fake relay manager,
# release installer, health endpoint and systemctl. The selected CLI is a
# sentinel and the persistent route journal is checked byte-for-byte.
CLI_UPDATE_DEVELOPMENT_ROOT="$TEST_HOME/.local/state/herdr-mobile-relay/tailscale-cli-development"
CLI_UPDATE_STATE="$CLI_UPDATE_DEVELOPMENT_ROOT/registration"
CLI_UPDATE_COORDINATION="$TEST_HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination"
CLI_UPDATE_MANAGER="$WORK_DIR/cli-update-manager"
CLI_UPDATE_MANAGER_RECORD="$WORK_DIR/cli-update-manager-record"
CLI_ROUTE_RECORD="$WORK_DIR/cli-route-record"
CLI_HEALTH_RECORD="$WORK_DIR/cli-health-record"
CLI_TAILSCALE="$WORK_DIR/cli-update-tailscale"
CLI_TAILSCALE_SENTINEL="$WORK_DIR/cli-update-tailscale-touched"
mkdir -p "$CLI_UPDATE_STATE" "$CLI_UPDATE_COORDINATION"
printf '%s\n' 'HERDR_DEV_TAILSCALE_CLI_ROOT=1' \
    "HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=$CLI_UPDATE_STATE" \
    "HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=$CLI_UPDATE_COORDINATION" \
    > "$CLI_UPDATE_DEVELOPMENT_ROOT/.herdr-dev-tailscale-cli"
chmod 600 "$CLI_UPDATE_DEVELOPMENT_ROOT/.herdr-dev-tailscale-cli"
printf '{"state":"registered","fixture":true}\n' > "$CLI_UPDATE_STATE/registration.json"
cp "$CLI_UPDATE_STATE/registration.json" "$WORK_DIR/cli-registration-before"
cat > "$CLI_TAILSCALE" <<EOF
#!/bin/sh
printf 'invoked\\n' >> "$CLI_TAILSCALE_SENTINEL"
exit 97
EOF
cat > "$CLI_UPDATE_MANAGER" <<'EOF'
#!/bin/sh
if [ "${1:-}" = json-field ]; then
    [ "${2:-}" = string ] || exit 1
    case "${3:-}" in status|release_version|revision|bundle_hash) ;; *) exit 1 ;; esac
    sed -n "s/.*\"${3}\":\"\\([^\"]*\\)\".*/\\1/p"
    exit 0
fi
printf '%s\n' "$*" >> "$CLI_UPDATE_MANAGER_RECORD"
case "$*" in
    'tailscale-cli activation-check') exit 0 ;;
    'tailscale-cli assert-ready '*)
        printf '%s\n' '{"route":{"journal_state":"registered","readiness":"ready","runtime_qualified":true}}'
        exit 0
        ;;
esac
exit 97
EOF
chmod 700 "$CLI_TAILSCALE" "$CLI_UPDATE_MANAGER"
write_cli_update_config() {
    local env_file="$1"
    cat >> "$env_file" <<EOF
HERDR_RELAY_TRANSPORT=tailscale-cli
HERDR_TAILSCALE_CLI_SCOPE=development
HERDR_TAILSCALE_CLI_BIN=$CLI_TAILSCALE
HERDR_TAILSCALE_CLI_STATE_ROOT=$CLI_UPDATE_STATE
HERDR_TAILSCALE_CLI_COORDINATION_ROOT=$CLI_UPDATE_COORDINATION
HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT=$CLI_UPDATE_DEVELOPMENT_ROOT
HERDR_TAILSCALE_CLI_HTTPS_PORT=9443
HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:9443
HERDR_TAILSCALE_CLI_NODE_ID=node-update-fixture
EOF
}
reset_cli_update_fixture() {
    rm -rf "$TARGET_CONFIG"
    cp -pR "$WORK_DIR/target-before/." "$TARGET_CONFIG/"
    cp "$WORK_DIR/source-env-original" "$SOURCE_ENV"
    cp "$WORK_DIR/initial-unit" "$UNIT_FILE"
    rm -f "$RELEASE_ROOT/current"
    ln -s "releases/0.8.6-old" "$RELEASE_ROOT/current"
    rm -f "$RESTART_LOG" "$CLI_UPDATE_MANAGER_RECORD" "$CLI_ROUTE_RECORD" "$CLI_HEALTH_RECORD"
}
reset_cli_update_fixture
write_cli_update_config "$SOURCE_ENV"
write_cli_update_config "$TARGET_CONFIG/relay.env"
cp -pR "$TARGET_CONFIG" "$WORK_DIR/cli-update-target-before"
export CLI_UPDATE_MANAGER_RECORD CLI_ROUTE_RECORD CLI_HEALTH_RECORD NEW_RELEASE

run_cli_update() {
    local answer="$1" output="$2"
    if CLI_UPDATE_ANSWER="$answer" \
        HOME="$TEST_HOME" \
        PATH="$FAKE_BIN:$PATH" \
        HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
        HERDR_PLUGIN_CONFIG_DIR="$TARGET_CONFIG" \
        HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
        HERDR_RELAY_ENV="$SOURCE_ENV" \
        HERDR_RELAY_BIN="$CLI_UPDATE_MANAGER" \
        HERDR_RELEASE_REPOSITORY=0cv/herdr-mobile-relay \
        REPLACEMENT_REVISION=new-revision \
        HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 \
        CLI_TAILSCALE_SENTINEL="$CLI_TAILSCALE_SENTINEL" \
        CLI_ROUTE_RECORD="$CLI_ROUTE_RECORD" \
        CLI_HEALTH_RECORD="$CLI_HEALTH_RECORD" \
        bash -c 'python3 - "$REPO_DIR" <<'"'"'PY'"'"'
import os
import pty
import subprocess
import sys

root = sys.argv[1]
master, slave = pty.openpty()
try:
    process = subprocess.Popen(
        [os.path.join(root, "relay", "tailscale-cli.sh"), "update"],
        cwd=root, env=os.environ.copy(), stdin=slave,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    os.close(slave)
    os.write(master, os.environ["CLI_UPDATE_ANSWER"].encode())
    stdout, stderr = process.communicate(timeout=90)
    sys.stdout.buffer.write(stdout)
    sys.stderr.buffer.write(stderr)
    raise SystemExit(process.returncode)
finally:
    os.close(master)
PY
' > "$output" 2>&1; then
        return 0
    else
        return $?
    fi
}

if run_cli_update $'n\n' "$WORK_DIR/cli-update-declined"; then
    echo "operator-managed CLI update unexpectedly ignored a declined confirmation" >&2
    exit 1
fi
grep -F "Cancelled; release, service, route, and journal were left unchanged." \
    "$WORK_DIR/cli-update-declined" >/dev/null || {
    echo "declined CLI update fixture produced unexpected output:" >&2
    while IFS= read -r line; do echo "$line" >&2; done < "$WORK_DIR/cli-update-declined"
    exit 1
}
test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
test ! -e "$RESTART_LOG"
diff -qr "$WORK_DIR/cli-update-target-before" "$TARGET_CONFIG" >/dev/null
test "$(cat "$CLI_UPDATE_STATE/registration.json")" = "$(cat "$WORK_DIR/cli-registration-before")"
test ! -e "$CLI_TAILSCALE_SENTINEL"

if ! run_cli_update $'y\n' "$WORK_DIR/cli-update-success"; then
    while IFS= read -r line; do echo "$line" >&2; done < "$WORK_DIR/cli-update-success"
    if [ -f "$HEALTH_FILE" ]; then while IFS= read -r line; do echo "health: $line" >&2; done < "$HEALTH_FILE"; fi
    if [ -f "$CLI_HEALTH_RECORD" ]; then while IFS= read -r line; do echo "health history: $line" >&2; done < "$CLI_HEALTH_RECORD"; fi
    if [ -f "$CLI_ROUTE_RECORD" ]; then while IFS= read -r line; do echo "route: $line" >&2; done < "$CLI_ROUTE_RECORD"; fi
    exit 1
fi
test "$(readlink -f "$RELEASE_ROOT/current")" = "$NEW_RELEASE"
test "$(cat "$RESTART_LOG")" = restart
grep -F "persistent Serve route and registration journal are retained" \
    "$WORK_DIR/cli-update-success" >/dev/null
[ "$(wc -l < "$CLI_ROUTE_RECORD")" -eq 1 ]
grep -F "tailscale-cli assert-ready --development-root $CLI_UPDATE_DEVELOPMENT_ROOT --binary $CLI_TAILSCALE" "$CLI_ROUTE_RECORD" >/dev/null
if grep -E 'tailscale-cli (publish|unpublish)' "$CLI_UPDATE_MANAGER_RECORD" "$CLI_ROUTE_RECORD" >/dev/null; then
    echo "operator-managed package update mutated the persistent Serve route" >&2
    exit 1
fi
test "$(cat "$CLI_UPDATE_STATE/registration.json")" = "$(cat "$WORK_DIR/cli-registration-before")"
test ! -e "$CLI_TAILSCALE_SENTINEL"

# If the installed process restarts successfully but exact route recovery fails,
# the previous release/service/config are restored and its route is rechecked.
reset_cli_update_fixture
write_cli_update_config "$SOURCE_ENV"
write_cli_update_config "$TARGET_CONFIG/relay.env"
if CLI_POST_ROUTE_FAIL=1 run_cli_update $'y\n' "$WORK_DIR/cli-update-route-drift"; then
    echo "operator-managed CLI update unexpectedly accepted post-restart route drift" >&2
    exit 1
fi
grep -F "replacement release did not recover the exact registered CLI Serve route" \
    "$WORK_DIR/cli-update-route-drift" >/dev/null
grep -F "previous service recovered successfully" "$WORK_DIR/cli-update-route-drift" >/dev/null
test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
grep -Fx "ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$SOURCE_ENV" "$UNIT_FILE" >/dev/null
test "$(wc -l < "$RESTART_LOG")" -eq 2
[ "$(wc -l < "$CLI_ROUTE_RECORD")" -eq 2 ]
test "$(cat "$CLI_UPDATE_STATE/registration.json")" = "$(cat "$WORK_DIR/cli-registration-before")"
test ! -e "$CLI_TAILSCALE_SENTINEL"

echo "plugin build migration, rollback, recovery, and operator-managed CLI update tests passed"

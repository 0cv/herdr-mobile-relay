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
mkdir -p "$OLD_RELEASE/relay" "$NEW_RELEASE/relay" "$SOURCE_CONFIG/push" \
    "$SOURCE_CONFIG/cloudflared" \
    "$TARGET_CONFIG/push" "$(dirname "$UNIT_FILE")" "$FAKE_BIN"

printf "HERDR_RELAY_TOKEN='source-token'\nHERDR_RELAY_INSTANCE_ID='source-instance'\nHERDR_RELAY_PORT='18375'\nCLOUDFLARED_CONFIG='%s/cloudflared/config.yml'\n" \
    "$SOURCE_CONFIG" > "$SOURCE_ENV"
printf 'source-subscriptions\n' > "$SOURCE_CONFIG/push/subscriptions.json"
printf 'source-origin\n' > "$SOURCE_CONFIG/phone-app-origin"
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
temp="\$INSTALL_ROOT/.current-install"
rm -f "\$temp"
ln -s "$NEW_RELEASE" "\$temp"
mv -Tf "\$temp" "\$INSTALL_ROOT/current"
EOF
chmod 700 "$FAKE_INSTALLER"

cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
case " $* " in
    *" is-active "*) exit 0 ;;
    *" restart "*)
        if grep -Fx "ExecStart=$SOURCE_CONFIG/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null; then
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
cat "$HEALTH_FILE"
EOF
chmod 700 "$FAKE_BIN/systemctl" "$FAKE_BIN/curl"

export SOURCE_CONFIG UNIT_FILE HEALTH_FILE TEST_VERSION
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$TARGET_CONFIG" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
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

if ! HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_CONFIG_DIR="$TARGET_CONFIG" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    REPLACEMENT_REVISION=new-revision \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/success-output" 2>&1; then
    cat "$WORK_DIR/success-output" >&2
    exit 1
fi

test "$(readlink -f "$RELEASE_ROOT/current")" = "$NEW_RELEASE"
grep -Fx "ExecStart=$RELEASE_ROOT/current/relay/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$RELEASE_ROOT/current" "$UNIT_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$TARGET_CONFIG/relay.env" "$UNIT_FILE" >/dev/null
grep -F source-token "$TARGET_CONFIG/relay.env" >/dev/null
grep -F source-instance "$TARGET_CONFIG/relay.env" >/dev/null
grep -F "HERDR_GITHUB_TOKEN_FILE='$TARGET_CONFIG/github-token'" "$TARGET_CONFIG/relay.env" >/dev/null
test "$(cat "$TARGET_CONFIG/push/subscriptions.json")" = source-subscriptions
test "$(cat "$TARGET_CONFIG/update-state.json")" = source-update
test "$(cat "$TARGET_CONFIG/app-deploy-state.json")" = source-app-deploy
test "$(cat "$TARGET_CONFIG/phone-app-origin")" = source-origin
grep -F "$TARGET_CONFIG/relay.env" "$TARGET_CONFIG/stable-setup.json" >/dev/null
grep -F "$TARGET_CONFIG/cloudflared/config.yml" "$TARGET_CONFIG/stable-setup.json" >/dev/null
grep -F "$TARGET_CONFIG/cloudflared/tunnel-credentials.json" \
    "$TARGET_CONFIG/cloudflared/config.yml" >/dev/null
test ! -e "$SOURCE_CONFIG/.herdr-mobile-relay-installation"
test ! -e "$REPO_DIR/relay/.herdr-mobile-relay-installation"

echo "plugin build migration and rollback tests passed"

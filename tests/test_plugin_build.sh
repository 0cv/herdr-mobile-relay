#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-build-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

TEST_HOME="$WORK_DIR/home"
RELEASE_ROOT="$TEST_HOME/releases"
OLD_RELEASE="$RELEASE_ROOT/releases/0.8.6-old"
NEW_RELEASE="$RELEASE_ROOT/releases/0.9.0-new"
CUSTOM_CONFIG="$TEST_HOME/custom-service-config"
ENV_FILE="$CUSTOM_CONFIG/custom.env"
UNIT_FILE="$TEST_HOME/.config/systemd/user/herdr-mobile-relay.service"
FAKE_BIN="$WORK_DIR/bin"
HEALTH_FILE="$WORK_DIR/health.json"
CONFIG_RECORD="$WORK_DIR/installer-config-root"
TOKEN_RECORD="$WORK_DIR/installer-token"
mkdir -p "$OLD_RELEASE/relay" "$NEW_RELEASE/relay" "$CUSTOM_CONFIG" \
    "$(dirname "$UNIT_FILE")" "$FAKE_BIN"

printf "HERDR_RELAY_PORT='18375'\nHERDR_GITHUB_TOKEN_FILE='%s/github-token'\n" \
    "$CUSTOM_CONFIG" > "$ENV_FILE"
printf 'persisted-private-token\n' > "$CUSTOM_CONFIG/github-token"
chmod 600 "$CUSTOM_CONFIG/github-token"
printf '{"version":"0.8.6","revision":"old-revision","web_hash":"old-web"}\n' \
    > "$OLD_RELEASE/release-manifest.json"
printf '{"version":"0.9.0","revision":"new-revision","web_hash":"new-web"}\n' \
    > "$NEW_RELEASE/release-manifest.json"

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
Environment=HERDR_RELAY_ENV=$ENV_FILE
ExecStart=$OLD_RELEASE/relay/herdr-mobile-relay-service.sh
WorkingDirectory=$OLD_RELEASE
EOF

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
        if grep -Fx "ExecStart=$OLD_RELEASE/relay/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null; then
            printf '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"old-revision","bundle_hash":"old-web"}\n' > "$HEALTH_FILE"
        else
            printf '{"status":"ok","instance":"test","version":"0.9.0","protocol":2,"release_version":"0.9.0","revision":"wrong-revision","bundle_hash":"new-web"}\n' > "$HEALTH_FILE"
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

export OLD_RELEASE UNIT_FILE HEALTH_FILE TOKEN_RECORD
if HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    HERDR_RELEASE_ROOT="$RELEASE_ROOT" \
    HERDR_PLUGIN_INSTALLER="$FAKE_INSTALLER" \
    bash "$REPO_DIR/relay/plugin-build.sh" >"$WORK_DIR/output" 2>&1; then
    echo "plugin migration unexpectedly accepted the wrong replacement identity" >&2
    exit 1
fi

test "$(readlink -f "$RELEASE_ROOT/current")" = "$OLD_RELEASE"
grep -Fx "ExecStart=$OLD_RELEASE/relay/herdr-mobile-relay-service.sh" "$UNIT_FILE" >/dev/null
grep -Fx "WorkingDirectory=$OLD_RELEASE" "$UNIT_FILE" >/dev/null
test "$(cat "$CONFIG_RECORD")" = "$CUSTOM_CONFIG"
test "$(cat "$TOKEN_RECORD")" = "persisted-private-token"
grep -F "previous service recovered successfully" "$WORK_DIR/output" >/dev/null

echo "plugin build rollback tests passed"

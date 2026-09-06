#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-build-coverage.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

awk 'NR < 156 { print ""; next } NR <= 188 { print; next } { exit }' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-functions.sh"

SERVICE_FILE="$WORK_DIR/herdr-mobile-relay.service"
SERVICE_WRAPPER="$WORK_DIR/herdr-mobile-relay-service.sh"
RELEASE_ROOT="$WORK_DIR/release"
ENV_FILE="$WORK_DIR/relay.env"
# shellcheck disable=SC2034
PLATFORM=Linux
printf '#!/bin/sh\n' > "$SERVICE_WRAPPER"
chmod 700 "$SERVICE_WRAPPER"

write_service() {
    printf '%s\n' \
        'ExecStart=/old/service-wrapper' \
        'WorkingDirectory=/old/release' \
        'Environment=HERDR_RELAY_ENV=/old/relay.env' > "$SERVICE_FILE"
    chmod 640 "$SERVICE_FILE"
}

expect_failure() {
    if rewrite_service_release_paths "$SERVICE_FILE" "$SERVICE_WRAPPER" "$RELEASE_ROOT" "$ENV_FILE"; then
        echo "rewrite_service_release_paths accepted $1" >&2
        exit 1
    fi
}

write_service
rewrite_service_release_paths "$SERVICE_FILE" "$SERVICE_WRAPPER" "$RELEASE_ROOT" "$ENV_FILE"
grep -Fx "ExecStart=$SERVICE_WRAPPER" "$SERVICE_FILE" >/dev/null
grep -Fx "WorkingDirectory=$RELEASE_ROOT" "$SERVICE_FILE" >/dev/null
grep -Fx "Environment=HERDR_RELAY_ENV=$ENV_FILE" "$SERVICE_FILE" >/dev/null

write_service
(
    # shellcheck disable=SC2329
    mktemp() { return 1; }
    expect_failure "mktemp failure"
)

write_service
(
    # shellcheck disable=SC2329
    awk() { return 1; }
    expect_failure "awk failure"
)

printf '%s\n' 'WorkingDirectory=/old/release' 'Environment=HERDR_RELAY_ENV=/old/relay.env' > "$SERVICE_FILE"
expect_failure "missing ExecStart"

printf '%s\n' 'ExecStart=/old/service-wrapper' 'Environment=HERDR_RELAY_ENV=/old/relay.env' > "$SERVICE_FILE"
expect_failure "missing WorkingDirectory"

printf '%s\n' 'ExecStart=/old/service-wrapper' 'WorkingDirectory=/old/release' > "$SERVICE_FILE"
expect_failure "missing relay environment"

write_service
(
    # shellcheck disable=SC2329
    chmod() {
        case "$1" in
            --reference=*) return 1 ;;
            *) command chmod "$@" ;;
        esac
    }
    rewrite_service_release_paths "$SERVICE_FILE" "$SERVICE_WRAPPER" "$RELEASE_ROOT" "$ENV_FILE"
)
test "$(stat -f '%Lp' "$SERVICE_FILE" 2>/dev/null || stat -c '%a' "$SERVICE_FILE")" = 600

write_service
(
    # shellcheck disable=SC2329
    mv() { return 1; }
    expect_failure "rename failure"
)

awk '
    /^unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE HERDR_WEB_ROOT HERDR_RELAY_BIN$/ { print; found_unset = 1; next }
    found_unset && /^ensure_relay_env "\$TARGET_ENV"$/ { print; exit }
' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-tail.sh"
ENSURE_RECORD="$WORK_DIR/ensure-record"
ensure_relay_env() {
    test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${HERDR_GITHUB_TOKEN_FILE+x}"
    printf '%s\n' "$1" > "$ENSURE_RECORD"
}
TARGET_ENV="$WORK_DIR/persistent/relay.env"
GH_TOKEN=one
GITHUB_TOKEN=two
HERDR_GITHUB_TOKEN_FILE=three
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-tail.sh"
test "$(cat "$ENSURE_RECORD")" = "$TARGET_ENV"

awk 'NR < 215 { print ""; next } NR <= 352 { print; next } { exit }' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-functions.sh"

RESTORE_PARENT="$WORK_DIR/restore-absent"
TARGET_CONFIG_PARENT="$RESTORE_PARENT"
TARGET_CONFIG_ROOT="$RESTORE_PARENT/config"
CONFIG_BACKUP="$RESTORE_PARENT/backup"
CONFIG_QUARANTINE=
CONFIG_RESTORE_STAGE=
target_config_existed=false
mkdir -p "$TARGET_CONFIG_PARENT" "$CONFIG_BACKUP"
restore_target_config
test ! -e "$TARGET_CONFIG_ROOT"

canonical_file_path() { printf '%s\n' "$1"; }
copy_migration_entry() {
    [ "$2" != relay.env ] || printf 'HERDR_RELAY_TOKEN=test\n' > "$MIGRATION_STAGE/relay.env"
}
rewrite_path_prefix() { return 0; }
env_file_value() { return 0; }
sanitize_relay_runtime_env() { return 0; }

if (
    TARGET_CONFIG_PARENT="$WORK_DIR/migration-symlink"
    TARGET_CONFIG_ROOT="$TARGET_CONFIG_PARENT/config"
    TARGET_ENV="$TARGET_CONFIG_ROOT/relay.env"
    target_config_existed=false
    mkdir -p "$TARGET_CONFIG_PARENT"
    symlink_free_config_tree() { return 1; }
    migrate_source_config "$WORK_DIR/source/relay.env"
); then
    echo "migrate_source_config accepted a symlinked staging tree" >&2
    exit 1
fi

(
    TARGET_CONFIG_PARENT="$WORK_DIR/migration-absent"
    TARGET_CONFIG_ROOT="$TARGET_CONFIG_PARENT/config"
    TARGET_ENV="$TARGET_CONFIG_ROOT/relay.env"
    target_config_existed=false
    mkdir -p "$TARGET_CONFIG_PARENT"
    symlink_free_config_tree() { return 0; }
    migrate_source_config "$WORK_DIR/source/relay.env"
    test -f "$TARGET_ENV"
)

if (
    TARGET_CONFIG_PARENT="$WORK_DIR/migration-rename"
    TARGET_CONFIG_ROOT="$TARGET_CONFIG_PARENT/config"
    TARGET_ENV="$TARGET_CONFIG_ROOT/relay.env"
    target_config_existed=true
    mkdir -p "$TARGET_CONFIG_ROOT"
    printf 'HERDR_RELAY_TOKEN=before\n' > "$TARGET_ENV"
    symlink_free_config_tree() { return 0; }
    mv() {
        [ "$1" != "$MIGRATION_STAGE" ] || return 1
        command mv "$@"
    }
    migrate_source_config "$WORK_DIR/source/relay.env"
); then
    echo "migrate_source_config accepted a failed atomic staging rename" >&2
    exit 1
fi
grep -F 'HERDR_RELAY_TOKEN=before' "$WORK_DIR/migration-rename/config/relay.env" >/dev/null

awk 'NR < 389 { print ""; next } NR <= 425 { print; next } { exit }' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-tail.sh"
PREVIOUS_TEST_ROOT="$WORK_DIR/previous-root"
PREVIOUS_EXTERNAL="$WORK_DIR/previous-external"
INSTALL_ROOT="$PREVIOUS_TEST_ROOT"
mkdir -p "$INSTALL_ROOT/releases" "$PREVIOUS_EXTERNAL"
ln -s "$PREVIOUS_EXTERNAL" "$INSTALL_ROOT/current"
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-tail.sh"
test -z "$PREVIOUS_RELEASE"

awk 'NR < 427 { print ""; next } NR <= 470 { print; next } { exit }' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-functions.sh"
SOURCE_ENV="$WORK_DIR/supervisor-source.env"
TARGET_ENV="$WORK_DIR/supervisor-target.env"
SUPERVISOR_STATE_PATH=
SUPERVISOR_STATE_BACKUP=
supervisor_state_existed=false
env_file_value() { printf '%s\n' relative-state; }
if snapshot_supervisor_state; then
    echo "snapshot_supervisor_state accepted a relative state directory" >&2
    exit 1
fi

UNSAFE_STATE_ROOT="$WORK_DIR/unsafe-supervisor-state"
mkdir -p "$UNSAFE_STATE_ROOT"
printf '{}\n' > "$UNSAFE_STATE_ROOT/state-target"
ln -s state-target "$UNSAFE_STATE_ROOT/supervisor.json"
env_file_value() { printf '%s\n' "$UNSAFE_STATE_ROOT"; }
if snapshot_supervisor_state; then
    echo "snapshot_supervisor_state accepted a symlinked state file" >&2
    exit 1
fi

PREVIOUS_RELEASE="$WORK_DIR/reset-release"
SUPERVISOR_STATE_PATH="$WORK_DIR/reset-supervisor.json"
supervisor_state_existed=false
mkdir -p "$PREVIOUS_RELEASE"
cat > "$PREVIOUS_RELEASE/herdr-mobile-relay" <<'EOF'
#!/bin/sh
[ "$1" = supervisor-reset ]
EOF
chmod 700 "$PREVIOUS_RELEASE/herdr-mobile-relay"
restore_or_reset_supervisor_state

awk 'NR < 570 { print ""; next } NR <= 719 { print; next } { exit }' "$ROOT_DIR/relay/plugin-build.sh" > "$WORK_DIR/plugin-build-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/plugin-build-functions.sh"

SHA_FALLBACK_BIN="$WORK_DIR/sha-fallback-bin"
mkdir -p "$SHA_FALLBACK_BIN"
ln -s "$(command -v awk)" "$SHA_FALLBACK_BIN/awk"
cat > "$SHA_FALLBACK_BIN/shasum" <<'EOF'
#!/bin/sh
printf '%064d  %s\n' 0 "$3"
EOF
chmod 700 "$SHA_FALLBACK_BIN/shasum"
test "$(PATH="$SHA_FALLBACK_BIN" release_sha256 "$WORK_DIR/digest-input")" = 0000000000000000000000000000000000000000000000000000000000000000

if (INSTALL_TOKEN='unsafe token'; authenticated_release_curl application/json --url https://api.example.test) 2>/dev/null; then
    echo "authenticated_release_curl accepted an unsafe token" >&2
    exit 1
fi

AUTH_BIN="$WORK_DIR/auth-bin"
NO_CURL_BIN="$WORK_DIR/no-curl-bin"
mkdir -p "$AUTH_BIN" "$NO_CURL_BIN"
cat > "$AUTH_BIN/uname" <<'EOF'
#!/bin/sh
[ "${1:-}" != -m ] || printf '%s\n' "${PLUGIN_TEST_ARCH:-x86_64}"
EOF
cat > "$AUTH_BIN/curl" <<'EOF'
#!/bin/sh
output=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --url) url=$2; shift 2 ;;
        *) shift ;;
    esac
done
case "${PLUGIN_CURL_FAILURE:-}:$url" in
    commit:*commits/*|release:*releases/tags/*|checksums:mock://checksums|archive:mock://archive) exit 22 ;;
esac
case "$url" in
    *commits/*) body='{"sha":"0123456789abcdef0123456789abcdef01234567"}' ;;
    *releases/tags/*)
        if [ "${PLUGIN_CURL_FAILURE:-}" = incomplete ]; then
            body='{"assets":[]}'
        else
            body="{\"assets\":[{\"url\":\"mock://archive\",\"name\":\"herdr-mobile-relay_${PLUGIN_TEST_VERSION}_linux_amd64.tar.gz\"},{\"url\":\"mock://checksums\",\"name\":\"checksums.txt\"}]}"
        fi
        ;;
    mock://checksums) body='checksums' ;;
    mock://archive) body='archive' ;;
    *) exit 23 ;;
esac
if [ -n "$output" ]; then
    printf '%s\n' "$body" > "$output"
else
    printf '%s\n' "$body"
fi
EOF
chmod 700 "$AUTH_BIN/uname" "$AUTH_BIN/curl"

if (PATH="$NO_CURL_BIN" INSTALL_TOKEN=valid PLATFORM=Linux VERSION=1.2.3 prepare_authenticated_offline_release); then
    echo "prepare_authenticated_offline_release accepted a missing curl" >&2
    exit 1
fi
if (PATH="$AUTH_BIN:/usr/bin:/bin" INSTALL_TOKEN=valid PLATFORM=Plan9 VERSION=1.2.3 prepare_authenticated_offline_release); then
    echo "prepare_authenticated_offline_release accepted an unsupported platform" >&2
    exit 1
fi
if (
    PATH="$AUTH_BIN:/usr/bin:/bin"
    INSTALL_TOKEN=valid
    PLATFORM=Darwin
    VERSION=1.2.3
    export PLUGIN_TEST_ARCH=arm64
    mktemp() { return 1; }
    prepare_authenticated_offline_release
); then
    echo "prepare_authenticated_offline_release accepted a failed arm64 staging directory" >&2
    exit 1
fi
if (PATH="$AUTH_BIN:/usr/bin:/bin" INSTALL_TOKEN=valid PLATFORM=Linux VERSION=1.2.3 PLUGIN_TEST_ARCH=mips prepare_authenticated_offline_release); then
    echo "prepare_authenticated_offline_release accepted an unsupported architecture" >&2
    exit 1
fi

expect_authenticated_prepare_failure() {
    if (
        export PLUGIN_CURL_FAILURE="$2" PLUGIN_TEST_VERSION=1.2.3
        PATH="$AUTH_BIN:/usr/bin:/bin"
        INSTALL_TOKEN=valid
        PLATFORM=Linux
        VERSION=1.2.3
        prepare_authenticated_offline_release
    ); then
        echo "prepare_authenticated_offline_release accepted $1" >&2
        exit 1
    fi
}

expect_authenticated_prepare_failure commit-download commit
expect_authenticated_prepare_failure release-download release
expect_authenticated_prepare_failure incomplete-metadata incomplete
expect_authenticated_prepare_failure checksums-download checksums
expect_authenticated_prepare_failure archive-download archive

printf 'changed shell plugin tests passed\n'

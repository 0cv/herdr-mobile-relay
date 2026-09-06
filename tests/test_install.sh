#!/bin/sh
set -eu

REPO_DIR=$(CDPATH='' cd "${0%/*}/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-install-test.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

# Load the installer functions without executing main.
sed '$d' "$REPO_DIR/install.sh" > "$WORK_DIR/install-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/install-functions.sh"

release_json='{
  "assets": [{
    "url": "https://api.github.com/repos/0cv/herdr-mobile-relay/releases/assets/123",
    "name": "herdr-mobile-relay_0.9.0_linux_amd64.tar.gz",
    "uploader": {
      "url": "https://api.github.com/users/0cv"
    }
  }, {
    "url": "https://api.github.com/repos/0cv/herdr-mobile-relay/releases/assets/124",
    "name": "checksums.txt",
    "uploader": {
      "url": "https://api.github.com/users/0cv"
    }
  }]
}'

archive_url=$(resolve_asset_url "$release_json" "herdr-mobile-relay_0.9.0_linux_amd64.tar.gz")
checksum_url=$(resolve_asset_url "$release_json" "checksums.txt")
test "$archive_url" = "https://api.github.com/repos/0cv/herdr-mobile-relay/releases/assets/123"
test "$checksum_url" = "https://api.github.com/repos/0cv/herdr-mobile-relay/releases/assets/124"

commit_json='{"sha":"0123456789abcdef0123456789abcdef01234567","commit":{"tree":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}'
test "$(resolve_tag_revision "$commit_json")" = "0123456789abcdef0123456789abcdef01234567"

if offline_release_requested; then
    echo "offline mode activated without explicit inputs" >&2
    exit 1
fi
(HERDR_RELEASE_ARCHIVE=archive offline_release_requested)
(HERDR_RELEASE_CHECKSUMS=checksums offline_release_requested)
(HERDR_EXPECTED_REVISION=revision offline_release_requested)
(HERDR_EXPECTED_ARCHIVE_SHA256=digest offline_release_requested)

sentinel_root="$WORK_DIR/custom-root"
write_install_sentinel "$sentinel_root"
canonical_root=$(CDPATH='' cd "$sentinel_root" && pwd -P)
grep -Fx 'product=herdr-mobile-relay' "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null
grep -Fx "root=$canonical_root" "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null
install_sentinel_is_valid "$sentinel_root" "$canonical_root"
install_sentinel_is_valid "$sentinel_root"

(
    stat() {
        case "$1" in
            -f) return 1 ;;
            -c) printf 'regular file:600:1:%s\n' "$(id -u)" ;;
        esac
    }
    install_sentinel_is_valid "$sentinel_root" "$canonical_root"
)
if (
    stat() { return 1; }
    install_sentinel_is_valid "$sentinel_root" "$canonical_root"
); then
    echo "install_sentinel_is_valid accepted a sentinel whose metadata could not be read" >&2
    exit 1
fi

exit_cleanup_sentinel="$WORK_DIR/exit-cleanup-sentinel"
: > "$exit_cleanup_sentinel"
(
    install_sentinel_temp="$exit_cleanup_sentinel"
    work_dir=
    herdr_command_pid=
    on_install_exit
)
test ! -e "$exit_cleanup_sentinel"

if (
    mktemp() { return 1; }
    write_install_sentinel "$WORK_DIR/mktemp-failure-root"
) 2>/dev/null; then
    echo "write_install_sentinel accepted a failed secure temporary-file creation" >&2
    exit 1
fi

if (
    printf() {
        [ "$1" != 'root=%s\n' ] || return 1
        command printf "$@"
    }
    write_install_sentinel "$WORK_DIR/write-failure-root"
) 2>/dev/null; then
    echo "write_install_sentinel accepted a failed sentinel write" >&2
    exit 1
fi

if (
    chmod() {
        [ "$1" != 600 ] || return 1
        command chmod "$@"
    }
    write_install_sentinel "$WORK_DIR/chmod-failure-root"
) 2>/dev/null; then
    echo "write_install_sentinel accepted a failed sentinel chmod" >&2
    exit 1
fi

if (
    mv() { return 1; }
    write_install_sentinel "$WORK_DIR/move-failure-root"
) 2>/dev/null; then
    echo "write_install_sentinel accepted a failed sentinel move" >&2
    exit 1
fi

if (
    install_sentinel_is_valid() { return 1; }
    write_install_sentinel "$WORK_DIR/post-validation-failure-root"
) 2>/dev/null; then
    echo "write_install_sentinel accepted a sentinel that failed post-install validation" >&2
    exit 1
fi

mktemp_log="$WORK_DIR/sentinel-mktemp.log"
mktemp_root="$WORK_DIR/mktemp-root"
(
    mktemp() {
        printf '%s\n' "$1" > "$mktemp_log"
        command mktemp "$@"
    }
    write_install_sentinel "$mktemp_root"
)
grep -F '.herdr-mobile-relay-installation.tmp.XXXXXX' "$mktemp_log" >/dev/null
install_sentinel_is_valid "$mktemp_root" "$(CDPATH='' cd "$mktemp_root" && pwd -P)"

for unsafe_kind in symlink hardlink public extra; do
    unsafe_root="$WORK_DIR/unsafe-$unsafe_kind"
    mkdir -p "$unsafe_root"
    unsafe_canonical=$(CDPATH='' cd "$unsafe_root" && pwd -P)
    unsafe_sentinel="$unsafe_root/.herdr-mobile-relay-installation"
    printf 'product=herdr-mobile-relay\nroot=%s\n' "$unsafe_canonical" > "$unsafe_sentinel"
    chmod 600 "$unsafe_sentinel"
    case "$unsafe_kind" in
        symlink)
            mv "$unsafe_sentinel" "$unsafe_root/sentinel-target"
            ln -s sentinel-target "$unsafe_sentinel"
            ;;
        hardlink) ln "$unsafe_sentinel" "$unsafe_root/sentinel-hardlink" ;;
        public) chmod 644 "$unsafe_sentinel" ;;
        extra) printf 'unexpected=true\n' >> "$unsafe_sentinel" ;;
    esac
    if (write_install_sentinel "$unsafe_root") 2>/dev/null; then
        echo "write_install_sentinel accepted an unsafe $unsafe_kind sentinel" >&2
        exit 1
    fi
done

unowned_root="$WORK_DIR/unowned"
mkdir -p "$unowned_root"
printf 'personal data\n' > "$unowned_root/keep.txt"
if (write_install_sentinel "$unowned_root") 2>/dev/null; then
    echo "write_install_sentinel claimed a nonempty unowned directory" >&2
    exit 1
fi
[ -f "$unowned_root/keep.txt" ]
[ ! -e "$unowned_root/.herdr-mobile-relay-installation" ]

legacy_home="$WORK_DIR/legacy-home"
legacy_release="$legacy_home/.local/share/herdr-mobile-relay"
legacy_config="$legacy_home/.config/herdr-mobile-relay"
legacy_cache="$legacy_home/.cache/herdr-mobile-relay"
mkdir -p "$legacy_config/device-auth" "$legacy_config/push" "$legacy_cache/claude-history" "$legacy_cache/uploads"
printf "HERDR_RELAY_TOKEN='legacy-token'\nHERDR_RELAY_INSTANCE_ID='legacy-instance'\n" > "$legacy_config/relay.env"
printf 'https://app.example.test\n' > "$legacy_config/phone-app-origin-configured"
printf '{"schema_version":1,"credentials":[]}\n' > "$legacy_config/device-auth/devices.json"
printf '[]\n' > "$legacy_config/push/subscriptions.json"
printf '{"state":"failed"}\n' > "$legacy_config/update-state.json"
printf '{"state":"idle"}\n' > "$legacy_config/app-deploy-state.json"
printf 'private-token\n' > "$legacy_config/github-token"
chmod 600 "$legacy_config/github-token"
printf '{"id":"legacy"}\n' > "$legacy_cache/activity.jsonl"
printf '[]\n' > "$legacy_cache/activity.tombstones"
mkdir -p "$legacy_cache/push"
printf 'private-key\n' > "$legacy_cache/push/vapid_private.pem"
printf 'verified\n' > "$legacy_cache/approval-verification-1"
printf '{"history":["preserved"]}\n' > "$legacy_cache/claude-history/pane.json"
HOME="$legacy_home"
prepare_install_roots "$legacy_release" "$legacy_config" "$legacy_cache"
test -f "$legacy_config/.herdr-mobile-relay-installation"
test -f "$legacy_cache/.herdr-mobile-relay-installation"
grep -F legacy-token "$legacy_config/relay.env" >/dev/null
test "$(cat "$legacy_config/phone-app-origin-configured")" = "https://app.example.test"
test "$(cat "$legacy_config/device-auth/devices.json")" = '{"schema_version":1,"credentials":[]}'
grep -F failed "$legacy_config/update-state.json" >/dev/null
grep -F idle "$legacy_config/app-deploy-state.json" >/dev/null
grep -F preserved "$legacy_cache/claude-history/pane.json" >/dev/null

symlink_config="$WORK_DIR/symlink-config"
mkdir -p "$symlink_config"
printf "HERDR_RELAY_TOKEN='legacy-token'\n" > "$symlink_config/relay.env"
ln -s "$legacy_config/relay.env" "$symlink_config/linked.env"
if (write_install_sentinel "$symlink_config" config) 2>/dev/null; then
    echo "write_install_sentinel accepted a symlinked legacy config" >&2
    exit 1
fi
test ! -e "$symlink_config/.herdr-mobile-relay-installation"

xdg_home="$WORK_DIR/xdg-home"
xdg_release="$xdg_home/.local/share/herdr-mobile-relay"
xdg_config="$xdg_home/.config/herdr-mobile-relay"
old_cache="$xdg_home/.cache/herdr-mobile-relay"
new_cache="$xdg_home/custom-cache/herdr-mobile-relay"
mkdir -p "$xdg_config/push" "$old_cache/claude-history"
printf "HERDR_RELAY_TOKEN='xdg-token'\n" > "$xdg_config/relay.env"
printf '[]\n' > "$xdg_config/push/subscriptions.json"
printf '{"history":["migrated"]}\n' > "$old_cache/claude-history/pane.json"
mkdir -p "$new_cache"
printf '#!/bin/sh\n' > "$new_cache/post-install.sh"
printf 'legacy waiter\n' > "$new_cache/post-install.log"
HOME="$xdg_home"
prepare_install_roots "$xdg_release" "$xdg_config" "$new_cache"
test ! -e "$old_cache"
grep -F migrated "$new_cache/claude-history/pane.json" >/dev/null
grep -F 'legacy waiter' "$new_cache/post-install.log" >/dev/null
test -f "$new_cache/.herdr-mobile-relay-installation"

timeout_started=$(date +%s)
if run_with_timeout 1 sleep 3; then
    echo "run_with_timeout did not time out" >&2
    exit 1
fi
timeout_finished=$(date +%s)
test "$((timeout_finished - timeout_started))" -lt 3

auth_bin="$WORK_DIR/auth-bin"
auth_args="$WORK_DIR/auth-argv"
auth_env="$WORK_DIR/auth-env"
auth_stdin="$WORK_DIR/auth-stdin"
auth_output="$WORK_DIR/auth-output"
mkdir -p "$auth_bin"
cat > "$auth_bin/curl" <<'EOF'
#!/bin/sh
env > "$AUTH_ENV"
printf '%s\n' "$@" > "$AUTH_ARGS"
cat <&3 > "$AUTH_STDIN"
output=
while [ "$#" -gt 0 ]; do
    if [ "$1" = --output ]; then
        output=$2
        break
    fi
    shift
done
if [ -n "$output" ]; then
    printf 'asset\n' > "$output"
else
    printf '{}\n'
fi
EOF
chmod 700 "$auth_bin/curl"
export AUTH_ARGS="$auth_args" AUTH_ENV="$auth_env" AUTH_STDIN="$auth_stdin"
export GH_TOKEN=SENTINEL_GH_TOKEN_NOT_IN_FETCH_PROCESS
export GITHUB_TOKEN=SENTINEL_GITHUB_TOKEN_NOT_IN_FETCH_PROCESS
export HERDR_GITHUB_TOKEN_FILE=/SENTINEL_GITHUB_TOKEN_POINTER_NOT_IN_FETCH_PROCESS
github_fetch_token=$GH_TOKEN
PATH="$auth_bin:/usr/bin:/bin" fetch_json https://api.github.test/release > "$auth_output"
test "$(cat "$auth_output")" = '{}'
grep -F 'Authorization: token SENTINEL_GH_TOKEN_NOT_IN_FETCH_PROCESS' "$auth_stdin" >/dev/null
if grep -F 'SENTINEL_' "$auth_args" "$auth_env" >/dev/null; then
    echo "authenticated GitHub fetch exposed its token through argv or child environment" >&2
    exit 1
fi
PATH="$auth_bin:/usr/bin:/bin" fetch https://api.github.test/asset "$auth_output"
test "$(cat "$auth_output")" = asset
grep -F 'Accept: application/octet-stream' "$auth_stdin" >/dev/null
if grep -F 'SENTINEL_' "$auth_args" "$auth_env" >/dev/null; then
    echo "authenticated GitHub asset fetch exposed its token through argv or child environment" >&2
    exit 1
fi
unset github_fetch_token GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE

if (github_fetch_token='unsafe token'; validate_github_fetch_token) 2>/dev/null; then
    echo "validate_github_fetch_token accepted unsupported characters" >&2
    exit 1
fi

wget_bin="$WORK_DIR/wget-bin"
wget_output="$WORK_DIR/wget-output"
mkdir -p "$wget_bin"
cat > "$wget_bin/wget" <<'EOF'
#!/bin/sh
output=
for argument do
    case "$argument" in
        --output-document=*) output=${argument#--output-document=} ;;
    esac
done
if [ "$output" = - ]; then
    printf '{"transport":"wget"}\n'
elif [ -n "$output" ]; then
    printf 'wget asset\n' > "$output"
fi
EOF
chmod 700 "$wget_bin/wget"
github_fetch_token=
(PATH="$wget_bin:/bin" fetch https://api.github.test/wget-asset "$wget_output")
test "$(cat "$wget_output")" = 'wget asset'
test "$(PATH="$wget_bin:/bin" fetch_json https://api.github.test/wget-json)" = '{"transport":"wget"}'
if (PATH="$wget_bin:/bin" github_fetch_token=authenticated fetch https://api.github.test/wget-auth "$wget_output") 2>/dev/null; then
    echo "fetch accepted authenticated wget transport" >&2
    exit 1
fi
if (PATH="$wget_bin:/bin" github_fetch_token=authenticated fetch_json https://api.github.test/wget-auth) 2>/dev/null; then
    echo "fetch_json accepted authenticated wget transport" >&2
    exit 1
fi
unset github_fetch_token

signal_downloader="$WORK_DIR/signal-downloader"
for signal_kind in wget curl; do
    signal_bin="$WORK_DIR/signal-bin-$signal_kind"
    mkdir -p "$signal_bin"
    for signal_command in awk dirname find head mktemp rm sed sleep tar tr uname; do
        ln -s "$(command -v "$signal_command")" "$signal_bin/$signal_command"
    done
    ln -s "$signal_downloader" "$signal_bin/$signal_kind"
done
cat > "$signal_downloader" <<'EOF'
#!/bin/sh
output=-
url=
case "${0##*/}" in
    wget)
        for arg do
            case "$arg" in
                --output-document=*) output=${arg#--output-document=} ;;
                *) url=$arg ;;
            esac
        done
        ;;
    curl)
        expect_output=0
        for arg do
            if [ "$expect_output" -eq 1 ]; then
                output=$arg
                expect_output=0
                continue
            fi
            case "$arg" in
                --output) expect_output=1 ;;
                --output=*) output=${arg#--output=} ;;
                *) url=$arg ;;
            esac
        done
        ;;
    *)
        exit 1
        ;;
esac
hang() {
    printf '%s\n' "$$" > "$DOWNLOAD_PID_FILE"
    trap 'exit 143' INT TERM
    while :; do
        sleep 1
    done
}
case "$url" in
    *commits/*)
        if [ "$DOWNLOAD_HANG_STAGE" = commits ]; then
            hang
        fi
        data='{"sha":"0123456789abcdef0123456789abcdef01234567"}'
        ;;
    *checksums.txt)
        data='placeholder'
        ;;
    *tar.gz)
        if [ "$DOWNLOAD_HANG_STAGE" = archive ]; then
            hang
        fi
        data=placeholder
        ;;
    *)
        exit 1
        ;;
esac
if [ "$output" = "-" ]; then
    printf '%s' "$data"
else
    printf '%s\n' "$data" > "$output"
fi
EOF
chmod 700 "$signal_downloader"

run_signal_install() {
    signal_kind=$1
    signal_stage=$2
    signal_root="$WORK_DIR/signal-$signal_kind-$signal_stage"
    signal_bin="$WORK_DIR/signal-bin-$signal_kind"
    signal_tmp="$signal_root/tmp"
    signal_home="$signal_root/home"
    signal_pid_file="$signal_root/downloader.pid"
    signal_log="$signal_root/install.log"
    mkdir -p "$signal_tmp" "$signal_home"
    (
        PATH="$signal_bin" \
        HOME="$signal_home" \
        TMPDIR="$signal_tmp" \
        VERSION=1.2.3 \
        DOWNLOAD_HANG_STAGE="$signal_stage" \
        DOWNLOAD_PID_FILE="$signal_pid_file" \
        /bin/sh "$REPO_DIR/install.sh" >"$signal_log" 2>&1
    ) &
    signal_installer_pid=$!
    signal_wait=0
    while [ ! -s "$signal_pid_file" ] && [ "$signal_wait" -lt 10 ]; do
        sleep 1
        signal_wait=$((signal_wait + 1))
    done
    if [ ! -s "$signal_pid_file" ]; then
        cat "$signal_log" >&2 || true
        kill "$signal_installer_pid" 2>/dev/null || true
        wait "$signal_installer_pid" 2>/dev/null || true
        echo "installer signal test did not reach $signal_kind $signal_stage download" >&2
        return 1
    fi
    signal_downloader_pid=$(cat "$signal_pid_file")
    kill -TERM "$signal_installer_pid"
    if wait "$signal_installer_pid"; then
        signal_status=0
    else
        signal_status=$?
    fi
    if [ "$signal_status" -ne 143 ]; then
        cat "$signal_log" >&2 || true
        echo "installer did not exit with SIGTERM status: $signal_status" >&2
        return 1
    fi
    if kill -0 "$signal_downloader_pid" 2>/dev/null; then
        kill -KILL "$signal_downloader_pid" 2>/dev/null || true
        echo "installer left the $signal_kind downloader running" >&2
        return 1
    fi
    test -z "$(find "$signal_tmp" -mindepth 1 -maxdepth 1 -print -quit)"
}

run_signal_install wget commits
run_signal_install wget archive
run_signal_install curl commits
run_signal_install curl archive

echo "install shell tests passed"

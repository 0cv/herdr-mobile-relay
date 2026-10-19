#!/bin/sh
set -eu

REPO_DIR=$(CDPATH='' cd "${0%/*}/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-install-test.XXXXXX")
trap 'status=$?; trap - EXIT INT TERM; chmod -R u+rwx "$WORK_DIR" && rm -rf "$WORK_DIR" || exit 1; exit "$status"' EXIT INT TERM

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

sentinel_root="$WORK_DIR/custom-root"
write_install_sentinel "$sentinel_root"
canonical_root=$(CDPATH='' cd "$sentinel_root" && pwd -P)
grep -Fx 'product=herdr-mobile-relay' "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null
grep -Fx "root=$canonical_root" "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null

sentinel_target="$WORK_DIR/sentinel-target"
mv "$sentinel_root/.herdr-mobile-relay-installation" "$sentinel_target"
ln -s "$sentinel_target" "$sentinel_root/.herdr-mobile-relay-installation"
if (write_install_sentinel "$sentinel_root") 2>/dev/null; then
    echo "write_install_sentinel accepted a symlinked ownership sentinel" >&2
    exit 1
fi
rm "$sentinel_root/.herdr-mobile-relay-installation"
mv "$sentinel_target" "$sentinel_root/.herdr-mobile-relay-installation"

chmod 644 "$sentinel_root/.herdr-mobile-relay-installation"
if (write_install_sentinel "$sentinel_root") 2>/dev/null; then
    echo "write_install_sentinel accepted a public ownership sentinel" >&2
    exit 1
fi
chmod 600 "$sentinel_root/.herdr-mobile-relay-installation"
ln "$sentinel_root/.herdr-mobile-relay-installation" "$WORK_DIR/hard-linked-sentinel"
if (write_install_sentinel "$sentinel_root") 2>/dev/null; then
    echo "write_install_sentinel accepted a hard-linked ownership sentinel" >&2
    exit 1
fi
rm "$WORK_DIR/hard-linked-sentinel"
write_install_sentinel "$sentinel_root"

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

# Exercise the public main entrypoint with a synthetic, privately staged
# package. The real checksum, tag/manifest and offline verification call path
# remains in install.sh; only the fixture's download and package executable are
# replaced. No ambient HOME, token, installed service or network is used.
package_fixture="$WORK_DIR/package-fixture"
mkdir -p "$package_fixture/bin" "$package_fixture/release" "$package_fixture/tmp"
fixture_revision=0123456789abcdef0123456789abcdef01234567
fixture_archive="herdr-mobile-relay_1.2.3_$(detect_os)_$(detect_arch).tar.gz"
printf '{\n  "schema": 1,\n  "version": "1.2.3",\n  "revision": "%s",\n  "target": "%s"\n}\n' \
    "$fixture_revision" "$(detect_os)/$(detect_arch)" > "$package_fixture/release/release-manifest.json"
printf 'fixture package\n' > "$package_fixture/release/fixture-marker"
cat > "$package_fixture/release/herdr-mobile-relay" <<'EOF'
#!/bin/sh
set -eu
case "$1" in
    verify-release)
        [ "$2" = --target ] && [ "$3" = "$TEST_TARGET" ] &&
            [ -f "$4/fixture-marker" ] &&
            grep -Fx '  "version": "1.2.3",' "$4/release-manifest.json" >/dev/null &&
            grep -Fx '  "revision": "0123456789abcdef0123456789abcdef01234567",' "$4/release-manifest.json" >/dev/null || exit 1
        case "$4" in */release) printf 'stage-verified\n' >> "$TEST_GATE_LOG" ;; esac
        ;;
    seal-release|prune-releases) printf '%s\n' "$1" >> "$TEST_GATE_LOG" ;;
    activate-release)
        printf 'activate-release\n' >> "$TEST_GATE_LOG"
        ln -s "releases/${3##*/}" "$2/current"
        ;;
    *) exit 1 ;;
esac
EOF
chmod 700 "$package_fixture/release/herdr-mobile-relay"
tar -czf "$package_fixture/$fixture_archive" -C "$package_fixture/release" .
sha256_file "$package_fixture/$fixture_archive" |
    awk -v name="$fixture_archive" '{print $1 "  " name}' > "$package_fixture/checksums.txt"
cat > "$package_fixture/bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=-
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output=$2; shift 2; continue ;;
        *) url=$1 ;;
    esac
    shift
done
case "$url" in
    */commits/v1.2.3)
        [ "$output" = - ] || exit 1
        printf '{"sha":"0123456789abcdef0123456789abcdef01234567"}\n'
        ;;
    */checksums.txt) cp "$TEST_PACKAGE/checksums.txt" "$output" ;;
    */*.tar.gz) cp "$TEST_PACKAGE/${url##*/}" "$output" ;;
    *) exit 1 ;;
esac
EOF
chmod 700 "$package_fixture/bin/curl"
# Only the unreadable-cache case denies enumeration, including when run with
# elevated test privileges that would otherwise bypass mode 0300. All other
# fixture find calls delegate to the host binary; production PATH is untouched.
cat > "$package_fixture/bin/find" <<'EOF'
#!/bin/sh
if [ -n "${TEST_REFUSE_FIND_ROOT:-}" ] && [ "$1" = "$TEST_REFUSE_FIND_ROOT" ]; then
    printf 'fixture: cache enumeration unavailable\n' >&2
    exit 1
fi
exec /usr/bin/find "$@"
EOF
chmod 700 "$package_fixture/bin/find"

# Capture complete fixture trees, including links (even dangling ones), file
# hashes and all modes. The package, downloader output and gate log live outside
# each case tree so a before/after mismatch identifies a persistent mutation.
fixture_snapshot() (
    locked_root=${2:-}
    if [ -n "$locked_root" ]; then
        # Record the actual mode before temporary read access. Restore that
        # same observed mode even if enumeration/hashing fails; an installer
        # chmod between snapshots remains visible rather than being reset.
        locked_identity=$(stat -c '%a:%u' "$locked_root" 2>/dev/null) ||
            locked_identity=$(stat -f '%Lp:%u' "$locked_root") || return 1
        trap 'snapshot_status=$?; chmod "${locked_identity%%:*}" "$locked_root" || exit 1; exit "$snapshot_status"' EXIT
        chmod u+rx "$locked_root" || return 1
    fi
    snapshot_paths=$(find "$1" -print) || return 1
    printf '%s\n' "$snapshot_paths" | LC_ALL=C sort | while IFS= read -r path; do
        if [ -L "$path" ]; then
            kind="link:$(readlink "$path")"
        elif [ -f "$path" ]; then
            kind="file:$(sha256_file "$path")"
        elif [ -d "$path" ]; then
            kind=directory
        else
            kind=other
        fi
        if [ "$path" = "$locked_root" ]; then
            mode=$locked_identity
        else
            mode=$(stat -c '%a:%u' "$path" 2>/dev/null) ||
                mode=$(stat -f '%Lp:%u' "$path")
        fi
        printf '%s %s %s\n' "$path" "$mode" "$kind"
    done
)

run_fixture_install() {
    case_root=$1
    gate_log=$2
    output_log=$3
    : > "$gate_log"
    /usr/bin/env -i PATH="$package_fixture/bin:/usr/bin:/bin" \
        HOME="$case_root/home" TMPDIR="$package_fixture/tmp" VERSION=1.2.3 \
        XDG_CACHE_HOME="$case_root/xdg-cache" \
        INSTALL_ROOT="$case_root/release-root" \
        HERDR_PLUGIN_CONFIG_DIR="$case_root/config-root" \
        BIN_DIR="$case_root/shim" \
        TEST_PACKAGE="$package_fixture" TEST_GATE_LOG="$gate_log" \
        TEST_REFUSE_FIND_ROOT=${4:-} \
        TEST_TARGET="$(detect_os)/$(detect_arch)" \
        /bin/sh "$REPO_DIR/install.sh" > "$output_log" 2>&1
}

# A source with a forbidden top-level entry, an unowned destination layout,
# and a late collision (after the earlier migratable claude-history entry).
# All three must reach package verification before migration admission refuses.
for scenario in invalid-source invalid-source-absent foreign-target late-collision unreadable-marked-cache; do
    case_root="$WORK_DIR/main-$scenario"
    source_cache="$case_root/home/.cache/herdr-mobile-relay"
    target_cache="$case_root/xdg-cache/herdr-mobile-relay"
    mkdir -p "$source_cache/claude-history" "$case_root/config-root/push"
    printf 'source history\n' > "$source_cache/claude-history/pane.json"
    printf 'HERDR_RELAY_TOKEN=fixture\n' > "$case_root/config-root/relay.env"
    printf 'push state\n' > "$case_root/config-root/push/subscriptions.json"
    printf 'app setting\n' > "$case_root/config-root/phone-app-origin"
    case "$scenario" in
        invalid-source|invalid-source-absent)
            printf 'unrelated source\n' > "$source_cache/not-a-cache-entry"
            ;;
        foreign-target)
            mkdir -p "$target_cache"
            printf 'foreign data\n' > "$target_cache/unrelated"
            chmod 751 "$target_cache"
            ;;
        late-collision)
            mkdir -p "$source_cache/uploads" "$target_cache"
            printf 'later source\n' > "$source_cache/uploads/photo"
            # Mark the otherwise admissible target BEFORE adding the link;
            # an unmarked linked target would fail layout validation earlier.
            write_install_sentinel "$target_cache" cache
            # A dangling link is occupied, even though -e reports it absent.
            ln -s missing-upload "$target_cache/uploads"
            chmod 710 "$target_cache"
            ;;
        unreadable-marked-cache)
            mkdir -p "$target_cache"
            write_install_sentinel "$target_cache" cache
            printf 'cache bytes\n' > "$target_cache/keep"
            chmod 300 "$target_cache"
            ;;
    esac
    # Preserve an existing owned release sentinel/current/shim, or prove an
    # absent release and shim stay absent on the second invalid-source case.
    if [ "$scenario" != invalid-source-absent ] && [ "$scenario" != unreadable-marked-cache ]; then
        mkdir -p "$case_root/release-root" "$case_root/shim"
        write_install_sentinel "$case_root/release-root"
        ln -s absent-release "$case_root/release-root/current"
        ln -s "$case_root/release-root/current/herdr-mobile-relay" "$case_root/shim/herdr-mobile-relay"
    elif [ "$scenario" = unreadable-marked-cache ]; then
        mkdir -p "$case_root/shim"
        ln -s missing-binary "$case_root/shim/herdr-mobile-relay"
    fi
    refuse_find=
    [ "$scenario" != unreadable-marked-cache ] || refuse_find=$target_cache
    before=$(fixture_snapshot "$case_root" "$refuse_find")
    gate_log="$WORK_DIR/gate-$scenario.log"
    output_log="$WORK_DIR/output-$scenario.log"
    if run_fixture_install "$case_root" "$gate_log" "$output_log" "$refuse_find"; then
        echo "main accepted $scenario" >&2
        exit 1
    fi
    grep -F 'stage-verified' "$gate_log" >/dev/null || {
        echo "main did not reach offline package verification for $scenario" >&2
        exit 1
    }
    grep -F 'migration admission refused' "$output_log" >/dev/null || {
        echo "main failed before migration admission for $scenario" >&2
        exit 1
    }
    test "$(cat "$gate_log")" = stage-verified
    if [ "$scenario" = late-collision ]; then
        grep -F 'migration entry collision: uploads' "$output_log" >/dev/null || {
            echo 'late collision did not reach the source-entry collision check' >&2
            exit 1
        }
    fi
    if [ "$scenario" = unreadable-marked-cache ]; then
        grep -F 'migration root enumeration refused: cache' "$output_log" >/dev/null || {
            echo 'marked unreadable cache did not fail during admission' >&2
            exit 1
        }
    fi
    after=$(fixture_snapshot "$case_root" "$refuse_find")
    test "$before" = "$after" || {
        echo "main changed persistent fixture state on $scenario refusal" >&2
        exit 1
    }
done

# Preserve the existing valid merge: an already admissible legacy destination
# retains its own entries while the source migrates and receives its sentinel.
positive_root="$WORK_DIR/main-valid"
positive_source="$positive_root/home/.cache/herdr-mobile-relay"
positive_target="$positive_root/xdg-cache/herdr-mobile-relay"
mkdir -p "$positive_source/claude-history" "$positive_target" "$positive_root/config-root/push"
printf 'history\n' > "$positive_source/claude-history/pane.json"
printf 'waiter\n' > "$positive_target/post-install.log"
printf '#!/bin/sh\n' > "$positive_target/post-install.sh"
printf 'HERDR_RELAY_TOKEN=fixture\n' > "$positive_root/config-root/relay.env"
printf 'push\n' > "$positive_root/config-root/push/subscriptions.json"
run_fixture_install "$positive_root" "$WORK_DIR/gate-valid.log" "$WORK_DIR/output-valid.log" || {
    echo 'valid main-entrypoint migration failed' >&2
    exit 1
}
test ! -e "$positive_source"
test "$(cat "$positive_target/claude-history/pane.json")" = history
test "$(cat "$positive_target/post-install.log")" = waiter
test -f "$positive_target/.herdr-mobile-relay-installation"
test -L "$positive_root/release-root/current"
test -L "$positive_root/shim/herdr-mobile-relay"

echo "install shell tests passed"

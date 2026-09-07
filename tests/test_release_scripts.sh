#!/bin/sh
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-release-script-test.XXXXXX")
cleanup() {
    chmod -R u+w "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT INT TERM

case $(uname -s) in
    Linux) HOST_OS=linux; WRONG_OS=darwin ;;
    Darwin) HOST_OS=darwin; WRONG_OS=linux ;;
    *)
        echo "unsupported test operating system: $(uname -s)" >&2
        exit 1
        ;;
esac
case $(uname -m) in
    x86_64|amd64) HOST_ARCH=amd64 ;;
    arm64|aarch64) HOST_ARCH=arm64 ;;
    *)
        echo "unsupported test architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

CHECKSUMS="$WORK_DIR/checksums.txt"
ARCHIVE="$WORK_DIR/herdr-mobile-relay_0.0.0_${WRONG_OS}_${HOST_ARCH}.tar.gz"
: > "$ARCHIVE"
printf '%064d  %s\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    "$REPO_DIR/scripts/check-installed-release.sh" \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$WRONG_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a non-native archive" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match native target"

ARCHIVE="$WORK_DIR/herdr-mobile-relay_0.0.0_${HOST_OS}_${HOST_ARCH}.tar.gz"
: > "$ARCHIVE"
printf '%064d  %s\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    "$REPO_DIR/scripts/check-installed-release.sh" \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$HOST_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a checksum mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "checksum mismatch"

printf '%064d  %s  unexpected-field\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    "$REPO_DIR/scripts/check-installed-release.sh" \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$HOST_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a malformed checksum entry" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "must contain exactly one entry"

BINARY_VERSION=0.0.0
MANIFEST_VERSION=9.9.9
REVISION=0123456789abcdef0123456789abcdef01234567
HOST_TARGET="$HOST_OS/$HOST_ARCH"
WRONG_TARGET="$WRONG_OS/$HOST_ARCH"
RELEASE_DIR="$WORK_DIR/release"
mkdir -p "$RELEASE_DIR/web" "$RELEASE_DIR/relay"
CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$BINARY_VERSION -X main.revision=$REVISION" \
    -o "$RELEASE_DIR/herdr-mobile-relay" \
    "$REPO_DIR/cmd/herdr-mobile-relay"
printf '%s\n' '<html></html>' > "$RELEASE_DIR/web/index.html"
printf '%s\n' license > "$RELEASE_DIR/LICENSE"
printf '%s\n' readme > "$RELEASE_DIR/README.md"
for WRAPPER in \
    common.sh \
    herdr-mobile-relay-service.sh \
    plugin-on-event.sh \
    setup-link.sh \
    stable-setup.sh \
    stable-teardown.sh \
    start.sh; do
    printf '%s\n' '#!/bin/sh' > "$RELEASE_DIR/relay/$WRAPPER"
done

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$MANIFEST_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
ARCHIVE="$WORK_DIR/herdr-mobile-relay_${MANIFEST_VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    "$REPO_DIR/scripts/check-installed-release.sh" \
        "$ARCHIVE" "$CHECKSUMS" "$MANIFEST_VERSION" "$REVISION" "$HOST_TARGET" 2>&1
); then
    echo "installed-release check accepted a manifest/binary identity mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match binary version"

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$WRONG_TARGET" >/dev/null
if OUTPUT=$(
    "$RELEASE_DIR/herdr-mobile-relay" verify-release \
        --target "$WRONG_TARGET" "$RELEASE_DIR" 2>&1
); then
    echo "verify-release accepted a manifest/binary target mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match binary target"
"$RELEASE_DIR/herdr-mobile-relay" verify-release \
    --allow-cross-target --target "$WRONG_TARGET" "$RELEASE_DIR" >/dev/null

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
ARCHIVE="$WORK_DIR/herdr-mobile-relay_${BINARY_VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$CHECKSUMS"

mkdir "$WORK_DIR/physical-tmp"
ln -s "$WORK_DIR/physical-tmp" "$WORK_DIR/logical-tmp"
TMPDIR="$WORK_DIR/logical-tmp" \
    "$REPO_DIR/scripts/check-installed-release.sh" \
    "$ARCHIVE" "$CHECKSUMS" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET"

INSTALL_HOME="$WORK_DIR/install-home"
INSTALL_ROOT="$WORK_DIR/install-root"
INSTALL_BIN="$WORK_DIR/install-bin"
INSTALL_CONFIG="$WORK_DIR/install-config"
INSTALL_CACHE="$WORK_DIR/install-cache"
mkdir -p "$INSTALL_HOME"
if HOME="$INSTALL_HOME" XDG_CONFIG_HOME="$INSTALL_CONFIG" XDG_CACHE_HOME="$INSTALL_CACHE" INSTALL_ROOT="$INSTALL_ROOT" BIN_DIR="$INSTALL_BIN" GH_TOKEN='' GITHUB_TOKEN='' HERDR_GITHUB_TOKEN_FILE='' HERDR_RELEASE_ARCHIVE="$ARCHIVE" HERDR_RELEASE_CHECKSUMS="$CHECKSUMS" HERDR_EXPECTED_REVISION="$REVISION" HERDR_EXPECTED_ARCHIVE_SHA256=0000000000000000000000000000000000000000000000000000000000000000 /bin/sh "$REPO_DIR/install.sh" "$BINARY_VERSION" > "$WORK_DIR/install-digest-mismatch.log" 2>&1; then
    echo "offline installer accepted a wrong independent digest despite a valid companion checksum" >&2
    exit 1
fi
grep -F 'offline release archive does not match independent expected SHA-256' "$WORK_DIR/install-digest-mismatch.log" >/dev/null
test ! -e "$INSTALL_ROOT/current"

if ! HOME="$INSTALL_HOME" XDG_CONFIG_HOME="$INSTALL_CONFIG" XDG_CACHE_HOME="$INSTALL_CACHE" INSTALL_ROOT="$INSTALL_ROOT" BIN_DIR="$INSTALL_BIN" GH_TOKEN='SENTINEL_OFFLINE_GH_TOKEN' GITHUB_TOKEN='SENTINEL_OFFLINE_GITHUB_TOKEN' HERDR_GITHUB_TOKEN_FILE='/SENTINEL_OFFLINE_TOKEN_POINTER' HERDR_RELEASE_ARCHIVE="$ARCHIVE" HERDR_RELEASE_CHECKSUMS="$CHECKSUMS" HERDR_EXPECTED_REVISION="$REVISION" HERDR_EXPECTED_ARCHIVE_SHA256="$HASH" /bin/sh "$REPO_DIR/install.sh" "$BINARY_VERSION" > "$WORK_DIR/install-offline.log" 2>&1; then
    cat "$WORK_DIR/install-offline.log" >&2
    echo "offline installer rejected an exact reviewed release" >&2
    exit 1
fi
grep -F 'Installing reviewed offline herdr-mobile-relay' "$WORK_DIR/install-offline.log" >/dev/null
OFFLINE_RELEASE="$(cd "$INSTALL_ROOT/current" && pwd -P)"
INSTALL_ROOT_PHYSICAL="$(cd "$INSTALL_ROOT" && pwd -P)"
case "$OFFLINE_RELEASE" in
    "$INSTALL_ROOT_PHYSICAL/releases/$BINARY_VERSION-$REVISION-$HOST_OS-$HOST_ARCH") ;;
    *) echo "offline installer activated an unexpected release path: $OFFLINE_RELEASE" >&2; exit 1 ;;
esac
"$OFFLINE_RELEASE/herdr-mobile-relay" verify-release --target "$HOST_TARGET" "$OFFLINE_RELEASE" >/dev/null

PACKAGE_REPO="$WORK_DIR/package-repo"
PACKAGE_BIN="$WORK_DIR/package-bin"
PACKAGE_BUILD_MARKER="$WORK_DIR/package-build-started"
PACKAGE_FAKE_BINARY="$WORK_DIR/package-fake-binary"
PACKAGE_SHELL=${HERDR_SHELL_COVERAGE_BASH:-/bin/sh}
PACKAGE_REAL_GIT=$(command -v git)
export PACKAGE_REAL_GIT
mkdir -p "$PACKAGE_REPO/scripts" "$PACKAGE_BIN"
cp "$REPO_DIR/scripts/package-release.sh" "$PACKAGE_REPO/scripts/package-release.sh"
cp "$REPO_DIR/scripts/stamp-web-version.mjs" "$PACKAGE_REPO/scripts/stamp-web-version.mjs"
cp -R "$REPO_DIR/web" "$PACKAGE_REPO/web"
cp -R "$REPO_DIR/relay" "$PACKAGE_REPO/relay"
cp "$REPO_DIR/LICENSE" "$PACKAGE_REPO/LICENSE"
cp "$REPO_DIR/README.md" "$PACKAGE_REPO/README.md"
cat > "$PACKAGE_FAKE_BINARY" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$PACKAGE_BIN/go" <<'EOF'
#!/bin/sh
: > "$PACKAGE_BUILD_MARKER"
output=
while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
        output=$2
        break
    fi
    shift
done
[ -n "$output" ] || exit 1
cp "$PACKAGE_FAKE_BINARY" "$output"
chmod 700 "$output"
EOF
cat > "$PACKAGE_BIN/bun" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 700 "$PACKAGE_FAKE_BINARY" "$PACKAGE_BIN/go" "$PACKAGE_BIN/bun"
git -C "$PACKAGE_REPO" init -q
git -C "$PACKAGE_REPO" config user.email release-test@example.invalid
git -C "$PACKAGE_REPO" config user.name 'Release Test'
git -C "$PACKAGE_REPO" add .
git -C "$PACKAGE_REPO" -c commit.gpgsign=false commit -qm fixture
PACKAGE_HEAD=$(git -C "$PACKAGE_REPO" rev-parse HEAD)

assert_package_source_rejected() {
    package_label=$1
    package_revision=$2
    package_output="$WORK_DIR/package-output-$package_label"
    rm -f "$PACKAGE_BUILD_MARKER"
    if PATH="$PACKAGE_BIN:/usr/bin:/bin" PACKAGE_BUILD_MARKER="$PACKAGE_BUILD_MARKER" PACKAGE_FAKE_BINARY="$PACKAGE_FAKE_BINARY" "$PACKAGE_SHELL" "$PACKAGE_REPO/scripts/package-release.sh" 1.2.3 "$package_revision" "$package_output" >/dev/null 2>&1; then
        echo "package release accepted $package_label source identity" >&2
        exit 1
    fi
    if [ -e "$PACKAGE_BUILD_MARKER" ] || [ -e "$package_output" ]; then
        echo "package release began producing $package_label output before source validation" >&2
        exit 1
    fi
}

assert_package_source_rejected wrong-head 0000000000000000000000000000000000000000
assert_package_source_rejected malformed-revision abc
assert_package_source_rejected uppercase-revision 0123456789ABCDEF0123456789ABCDEF01234567
mv "$PACKAGE_REPO/.git" "$PACKAGE_REPO/.git-hidden"
assert_package_source_rejected missing-git "$PACKAGE_HEAD"
mv "$PACKAGE_REPO/.git-hidden" "$PACKAGE_REPO/.git"
printf 'untracked\n' > "$PACKAGE_REPO/dirty-source"
assert_package_source_rejected dirty-tree "$PACKAGE_HEAD"
rm -f "$PACKAGE_REPO/dirty-source"
git -C "$PACKAGE_REPO" update-index --assume-unchanged README.md
printf '\nnon-HEAD tracked content\n' >> "$PACKAGE_REPO/README.md"
assert_package_source_rejected hidden-tracked-dirt "$PACKAGE_HEAD"
git -C "$PACKAGE_REPO" update-index --no-assume-unchanged README.md
git -C "$PACKAGE_REPO" checkout -- README.md
cat > "$PACKAGE_BIN/git" <<'EOF'
#!/bin/sh
case " $* " in
    *' status '*) exit 1 ;;
    *) exec "$PACKAGE_REAL_GIT" "$@" ;;
esac
EOF
chmod 700 "$PACKAGE_BIN/git"
assert_package_source_rejected unreadable-status "$PACKAGE_HEAD"
rm -f "$PACKAGE_BIN/git"

PACKAGE_REAL_TAR=$(command -v tar)
export PACKAGE_REAL_TAR
cat > "$PACKAGE_BIN/git" <<'EOF'
#!/bin/sh
if [ "${PACKAGE_FAIL_MODE:-}" = archive ]; then
    for argument do
        [ "$argument" != archive ] || exit 1
    done
fi
exec "$PACKAGE_REAL_GIT" "$@"
EOF
cat > "$PACKAGE_BIN/tar" <<'EOF'
#!/bin/sh
if [ "${PACKAGE_FAIL_MODE:-}" = extract ] && [ "${1:-}" = -xf ]; then
    exit 1
fi
exec "$PACKAGE_REAL_TAR" "$@"
EOF
chmod 700 "$PACKAGE_BIN/git" "$PACKAGE_BIN/tar"

assert_package_stage_rejected() {
    package_label=$1
    package_mode=$2
    expected_message=$3
    package_output="$WORK_DIR/package-stage-output-$package_label"
    package_log="$WORK_DIR/package-stage-$package_label.log"
    rm -f "$PACKAGE_BUILD_MARKER"
    if PATH="$PACKAGE_BIN:/usr/bin:/bin" PACKAGE_BUILD_MARKER="$PACKAGE_BUILD_MARKER" PACKAGE_FAKE_BINARY="$PACKAGE_FAKE_BINARY" PACKAGE_FAIL_MODE="$package_mode" "$PACKAGE_SHELL" "$PACKAGE_REPO/scripts/package-release.sh" 1.2.3 "$PACKAGE_HEAD" "$package_output" >"$package_log" 2>&1; then
        echo "package release accepted a failed $package_label operation" >&2
        exit 1
    fi
    grep -F "$expected_message" "$package_log" >/dev/null
    if [ -e "$PACKAGE_BUILD_MARKER" ]; then
        echo "package release began building after a failed $package_label operation" >&2
        exit 1
    fi
}

assert_package_stage_rejected source-archive archive "could not archive exact release source HEAD"
assert_package_stage_rejected source-extraction extract "could not stage exact release source HEAD"

PACKAGE_OUTPUT="$WORK_DIR/package-output-valid"
printf '/web/ignored-release-secret\n' >> "$PACKAGE_REPO/.git/info/exclude"
printf 'must never enter a release archive\n' > "$PACKAGE_REPO/web/ignored-release-secret"
rm -f "$PACKAGE_BUILD_MARKER"
PATH="$PACKAGE_BIN:/usr/bin:/bin" PACKAGE_BUILD_MARKER="$PACKAGE_BUILD_MARKER" PACKAGE_FAKE_BINARY="$PACKAGE_FAKE_BINARY" "$PACKAGE_SHELL" "$PACKAGE_REPO/scripts/package-release.sh" 1.2.3 "$PACKAGE_HEAD" "$PACKAGE_OUTPUT" >/dev/null
test -e "$PACKAGE_BUILD_MARKER"
test -s "$PACKAGE_OUTPUT/checksums.txt"
test "$(find "$PACKAGE_OUTPUT" -name 'herdr-mobile-relay_1.2.3_*.tar.gz' | wc -l | awk '{print $1}')" -eq 4
for package_archive in "$PACKAGE_OUTPUT"/herdr-mobile-relay_1.2.3_*.tar.gz; do
    if tar -tzf "$package_archive" | grep -F './web/ignored-release-secret' >/dev/null; then
        echo "package release copied an ignored worktree file into $(basename "$package_archive")" >&2
        exit 1
    fi
done

echo "release shell tests passed"

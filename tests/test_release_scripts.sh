#!/bin/sh
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-release-script-test.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

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
REVISION=test-revision
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

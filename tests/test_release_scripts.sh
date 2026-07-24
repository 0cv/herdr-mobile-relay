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

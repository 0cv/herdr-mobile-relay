#!/bin/sh
# Build complete, manifest-verified release archives for all supported targets.
set -eu

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
VERSION=${1:-}
REVISION=${2:-}
OUTPUT_DIR=${3:-"$REPO_DIR/dist/release"}

[ -n "$VERSION" ] || {
    echo "usage: scripts/package-release.sh VERSION REVISION [OUTPUT_DIR]" >&2
    exit 2
}
[ -n "$REVISION" ] || {
    echo "usage: scripts/package-release.sh VERSION REVISION [OUTPUT_DIR]" >&2
    exit 2
}
case "$VERSION" in
    v*) VERSION=${VERSION#v} ;;
esac
[ "${#REVISION}" -eq 40 ] || {
    echo "REVISION must be the exact lowercase 40-character Git HEAD" >&2
    exit 1
}
case "$REVISION" in
    *[!0-9a-f]*)
        echo "REVISION must be the exact lowercase 40-character Git HEAD" >&2
        exit 1
        ;;
esac
HEAD_REVISION=$(git -C "$REPO_DIR" rev-parse --verify HEAD 2>/dev/null) || {
    echo "release source must be a Git worktree with a readable HEAD" >&2
    exit 1
}
[ "$REVISION" = "$HEAD_REVISION" ] || {
    echo "REVISION does not match release source HEAD $HEAD_REVISION" >&2
    exit 1
}
git -C "$REPO_DIR" update-index --really-refresh >/dev/null 2>&1 || {
    echo "release source must exactly match its clean Git HEAD" >&2
    exit 1
}
SOURCE_STATUS=$(git -C "$REPO_DIR" status --porcelain --untracked-files=normal) || {
    echo "could not inspect release source status" >&2
    exit 1
}
[ -z "$SOURCE_STATUS" ] || {
    echo "release source must exactly match its clean Git HEAD" >&2
    exit 1
}

command -v go >/dev/null 2>&1 || {
    echo "go is required on the release builder" >&2
    exit 1
}
command -v bun >/dev/null 2>&1 || {
    echo "Bun is required on the release builder to stamp the web bundle" >&2
    exit 1
}
command -v tar >/dev/null 2>&1 || {
    echo "tar is required on the release builder" >&2
    exit 1
}

mkdir -p "$OUTPUT_DIR"
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-release.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM
SOURCE_DIR="$WORK_DIR/source"
SOURCE_ARCHIVE="$WORK_DIR/source.tar"
mkdir -p "$SOURCE_DIR"
git -C "$REPO_DIR" archive --format=tar --output="$SOURCE_ARCHIVE" "$REVISION" || {
    echo "could not archive exact release source HEAD" >&2
    exit 1
}
tar -xf "$SOURCE_ARCHIVE" -C "$SOURCE_DIR" || {
    echo "could not stage exact release source HEAD" >&2
    exit 1
}

(
    cd "$SOURCE_DIR"
    CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
        -o "$WORK_DIR/release-tool" \
        ./cmd/herdr-mobile-relay
)

for TARGET in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    GOOS=${TARGET%/*}
    GOARCH=${TARGET#*/}
    ARCHIVE="herdr-mobile-relay_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
    STAGE="$WORK_DIR/${GOOS}-${GOARCH}"
    mkdir -p "$STAGE/relay"

    (
        cd "$SOURCE_DIR"
        CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
            -trimpath \
            -ldflags "-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
            -o "$STAGE/herdr-mobile-relay" \
            ./cmd/herdr-mobile-relay
    )
    cp -R "$SOURCE_DIR/web" "$STAGE/web"
    bun "$SOURCE_DIR/scripts/stamp-web-version.mjs" "$STAGE/web/version.json" "$VERSION" "$REVISION"
    cp "$SOURCE_DIR/LICENSE" "$STAGE/LICENSE"
    cp "$SOURCE_DIR/README.md" "$STAGE/README.md"
    for WRAPPER in \
        common.sh \
        herdr-mobile-relay-service.sh \
        install-service.sh \
        install-systemd-user-service.sh \
        plugin-on-event.sh \
        service.sh \
        setup-link.sh \
        setup.sh \
        stable-setup.sh \
        stable-teardown.sh \
        start.sh \
        uninstall.sh \
        uninstall-service.sh \
        uninstall-systemd-user-service.sh; do
        cp "$SOURCE_DIR/relay/$WRAPPER" "$STAGE/relay/$WRAPPER"
    done
    "$WORK_DIR/release-tool" release-manifest "$STAGE" "$VERSION" "$REVISION" "$TARGET" >/dev/null
    # This host tool cannot execute cross-built binaries; native CI verifies each extracted executable.
    "$WORK_DIR/release-tool" verify-release --allow-cross-target --target "$TARGET" "$STAGE" >/dev/null
    tar -C "$STAGE" -czf "$OUTPUT_DIR/$ARCHIVE" .
done

CHECKSUMS="$OUTPUT_DIR/checksums.txt"
: > "$CHECKSUMS"
for ARCHIVE in "$OUTPUT_DIR"/herdr-mobile-relay_"$VERSION"_*.tar.gz; do
    NAME=${ARCHIVE##*/}
    if command -v sha256sum >/dev/null 2>&1; then
        HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
    else
        HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
    fi
    printf '%s  %s\n' "$HASH" "$NAME" >> "$CHECKSUMS"
done

echo "Release bundles written to $OUTPUT_DIR"

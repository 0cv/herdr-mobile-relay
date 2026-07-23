#!/bin/sh
# Build the Go relay binary for the current platform.
# Usage: scripts/build.sh [output-dir]
# Produces: herdr-mobile-relay (or herdr-mobile-relay.exe on windows)
set -eu

SCRIPT_DIR=${0%/*}
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
OUT_DIR="${1:-$REPO_DIR/bin}"

VERSION=$(git -C "$REPO_DIR" describe --tags --always 2>/dev/null || echo "dev")
REVISION=$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo "unknown")

mkdir -p "$OUT_DIR"

echo "Building herdr-mobile-relay $VERSION ($REVISION)..."
CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
    -o "$OUT_DIR/herdr-mobile-relay" \
    "$REPO_DIR/cmd/herdr-mobile-relay"

echo "Built: $OUT_DIR/herdr-mobile-relay"

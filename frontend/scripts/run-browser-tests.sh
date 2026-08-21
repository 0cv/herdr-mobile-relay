#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FRONTEND_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$FRONTEND_DIR/.." && pwd)"
WEB_ROOT="${1:-dist}"

cd "$FRONTEND_DIR"

CONTAINER_RUNTIME=""
CONTAINER_ARGS=()
if [ "${HERDR_WEBKIT_CONTAINER:-}" = 1 ]; then
    CONTAINER_RUNTIME="docker"
elif grep -Eq '^ID=("?fedora"?)$' /etc/os-release 2>/dev/null; then
    CONTAINER_RUNTIME="podman"
    CONTAINER_ARGS+=(--security-opt label=disable)
else
    HERDR_WEB_ROOT="$WEB_ROOT" bun x playwright test
    exit
fi

if ! command -v "$CONTAINER_RUNTIME" >/dev/null 2>&1; then
    echo "WebKit testing requires $CONTAINER_RUNTIME." >&2
    exit 1
fi

PLAYWRIGHT_VERSION="$(bun -e "console.log(JSON.parse(require('fs').readFileSync('package.json','utf8')).devDependencies['@playwright/test'])")"
if [ -z "$PLAYWRIGHT_VERSION" ]; then
    echo "Could not determine the pinned Playwright version from frontend/package.json" >&2
    exit 1
fi
WEBKIT_WORKERS="${HERDR_WEBKIT_WORKERS:-2}"
if ! [[ "$WEBKIT_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
    echo "HERDR_WEBKIT_WORKERS must be a positive integer." >&2
    exit 1
fi

echo "Installing Chromium browser files without apt dependencies."
bun x playwright install chromium
HERDR_WEB_ROOT="$WEB_ROOT" bun x playwright test --project=chromium-mobile

# WebKit stays on the container image's own Node: it ships node and npx, and
# installing Bun inside the image per run would cost more than Playwright's
# few seconds of process startup saves.
echo "Running WebKit in Playwright's version-matched official container."
"$CONTAINER_RUNTIME" run --rm \
    "${CONTAINER_ARGS[@]}" \
    -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    -e HERDR_WEB_ROOT="$WEB_ROOT" \
    -v "$FRONTEND_DIR:/work/frontend:ro" \
    -v "$REPO_DIR/web:/work/web:ro" \
    -w /work/frontend \
    "mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble" \
    npx playwright test --project=webkit-mobile \
        --workers="$WEBKIT_WORKERS" \
        --output=/tmp/playwright-results

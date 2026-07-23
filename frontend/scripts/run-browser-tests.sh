#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FRONTEND_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$FRONTEND_DIR/.." && pwd)"
WEB_ROOT="${1:-dist}"

cd "$FRONTEND_DIR"

if [ ! -r /etc/os-release ] || ! grep -Eq '^ID=("?fedora"?)$' /etc/os-release; then
    HERDR_WEB_ROOT="$WEB_ROOT" npm exec -- playwright test
    exit
fi

if ! command -v podman >/dev/null 2>&1; then
    echo "Fedora WebKit testing requires Podman: sudo dnf install podman" >&2
    exit 1
fi

PLAYWRIGHT_VERSION="$(node -p "JSON.parse(require('fs').readFileSync('package.json', 'utf8')).devDependencies['@playwright/test']")"
if [ -z "$PLAYWRIGHT_VERSION" ]; then
    echo "Could not determine the pinned Playwright version from frontend/package.json" >&2
    exit 1
fi
WEBKIT_WORKERS="${HERDR_WEBKIT_WORKERS:-2}"
if ! [[ "$WEBKIT_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
    echo "HERDR_WEBKIT_WORKERS must be a positive integer." >&2
    exit 1
fi

echo "Fedora detected: installing Chromium browser files without apt dependencies."
npm exec -- playwright install chromium
HERDR_WEB_ROOT="$WEB_ROOT" npm exec -- playwright test --project=chromium-mobile

echo "Running WebKit in Playwright's version-matched official container."
podman run --rm \
    --security-opt label=disable \
    -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    -e HERDR_WEB_ROOT="$WEB_ROOT" \
    -v "$FRONTEND_DIR:/work/frontend:ro" \
    -v "$REPO_DIR/web:/work/web:ro" \
    -w /work/frontend \
    "mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble" \
    npx playwright test --project=webkit-mobile \
        --workers="$WEBKIT_WORKERS" \
        --output=/tmp/playwright-results

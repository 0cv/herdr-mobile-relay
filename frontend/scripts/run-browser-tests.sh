#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FRONTEND_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$FRONTEND_DIR/.." && pwd)"
cd "$FRONTEND_DIR"

CONFIG="$1"
shift

# CI runs both engines natively: Ubuntu is a supported `playwright install
# --with-deps` target, and the pinned browser builds are identical to the
# container's. The container paths below remain for hosts where native WebKit
# cannot work: Fedora (install-deps has no dnf mapping and WebKit crashes on
# ABI mismatch) and any host that opts in with HERDR_WEBKIT_CONTAINER=1.
CONTAINER_RUNTIME=""
CONTAINER_RUN=()
if [ "${HERDR_WEBKIT_CONTAINER:-}" = 1 ]; then
    CONTAINER_RUNTIME="docker"
    CONTAINER_RUN=("$CONTAINER_RUNTIME" run --rm)
elif grep -Eq '^ID=("?fedora"?)$' /etc/os-release 2>/dev/null; then
    CONTAINER_RUNTIME="podman"
    CONTAINER_RUN=("$CONTAINER_RUNTIME" run --rm --security-opt label=disable)
else
    exec bun x playwright test --config "$CONFIG" "$@"
fi

if ! command -v "$CONTAINER_RUNTIME" >/dev/null 2>&1; then
    echo "WebKit testing requires $CONTAINER_RUNTIME (on Fedora: sudo dnf install podman)." >&2
    exit 1
fi

PLAYWRIGHT_VERSION="$(bun -e "console.log(JSON.parse(require('fs').readFileSync('package.json','utf8')).devDependencies['@playwright/test'])")"
if ! [[ "$PLAYWRIGHT_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Expected an exact Playwright version in frontend/package.json." >&2
    exit 1
fi

IMAGE="mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble"

echo "Installing Chromium browser files without apt dependencies."
bun x playwright install chromium

# Some main-suite tests launch WebKit directly instead of through the connected
# browser fixture, so that suite runs its whole WebKit project in the container.
if [ "$CONFIG" != playwright.attention.config.ts ]; then
    WEB_ROOT="${HERDR_WEB_ROOT:-dist}"
    WEBKIT_WORKERS="${HERDR_WEBKIT_WORKERS:-2}"
    if ! [[ "$WEBKIT_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
        echo "HERDR_WEBKIT_WORKERS must be a positive integer." >&2
        exit 1
    fi

    HERDR_WEB_ROOT="$WEB_ROOT" bun x playwright test --config "$CONFIG" --project=chromium-mobile "$@"

    # WebKit stays on the container image's own Node: it ships node and npx, and
    # installing Bun inside the image per run would cost more than Playwright's
    # few seconds of process startup saves.
    echo "Running WebKit in Playwright's version-matched official container."
    exec "${CONTAINER_RUN[@]}" \
        -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
        -e HERDR_WEB_ROOT="$WEB_ROOT" \
        -v "$FRONTEND_DIR:/work/frontend:ro" \
        -v "$REPO_DIR/web:/work/web:ro" \
        -w /work/frontend \
        "$IMAGE" \
        npx playwright test --config "$CONFIG" --project=webkit-mobile \
            --workers="$WEBKIT_WORKERS" \
            --output=/tmp/playwright-results "$@"
fi

# Keep the test runner and isolated Go relay fixture on the host. Only WebKit
# needs the Ubuntu browser ABI; Playwright forwards its loopback connections
# back to the host, including the fixture's dynamically allocated relay port.
CONTAINER_ID=""
cleanup() {
    if [ -n "$CONTAINER_ID" ]; then
        "$CONTAINER_RUNTIME" rm -f "$CONTAINER_ID" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The version-matched image is retained in the runtime's image cache across
# runs/reboots. Do not install Ubuntu packages into the Fedora host or fetch
# an unpinned Playwright CLI inside the container.
echo "Running browser tests with containerized WebKit and the host relay fixture."
CONTAINER_ID="$("${CONTAINER_RUN[@]}" -d \
    -p 127.0.0.1::3000 \
    -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    -v "$FRONTEND_DIR:/work/frontend:ro" \
    -w /work/frontend \
    "$IMAGE" \
    node node_modules/playwright/cli.js run-server --host 0.0.0.0 --port 3000)"
ADDRESS="$("$CONTAINER_RUNTIME" port "$CONTAINER_ID" 3000/tcp)"
if ! [[ "$ADDRESS" =~ ^127\.0\.0\.1:[0-9]+$ ]]; then
    echo "Could not determine the container's loopback-only Playwright endpoint: $ADDRESS" >&2
    exit 1
fi
ENDPOINT="ws://$ADDRESS/"

if ! bun -e '
const url = new URL(process.argv[1]);
url.protocol = "http:";
const deadline = Date.now() + 30_000;
while (Date.now() < deadline) {
  try {
    const response = await fetch(url, { signal: AbortSignal.timeout(1_000) });
    if (response.ok) process.exit(0);
  } catch { /* The container server may still be starting. */ }
  await Bun.sleep(100);
}
console.error("Container Playwright server did not become ready within 30 seconds.");
process.exit(1);
' "$ENDPOINT"; then
    "$CONTAINER_RUNTIME" logs "$CONTAINER_ID" >&2 || true
    exit 1
fi

# Scoped to this invocation. Chromium remains native; the config applies the
# endpoint only to its WebKit project. Test failures are preserved,
# and EXIT removes only the container this invocation created.
HERDR_WEBKIT_WS_ENDPOINT="$ENDPOINT" \
    bun x playwright test --config "$CONFIG" "$@"

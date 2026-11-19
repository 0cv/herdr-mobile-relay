#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
version="$(bun -e "console.log(require('./package.json').devDependencies['@playwright/test'])")"
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Expected an exact Playwright version in frontend/package.json." >&2
    exit 1
fi

work="$(mktemp -d /tmp/herdr-demo.XXXXXX)"
trap 'rm -rf "$work"' EXIT
node scripts/demo-site.mjs dist "$work/site"

docker run --rm --init --platform linux/amd64 --network none \
    -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    -v "$PWD:/work/frontend:ro" \
    -v "$work/site:/work/demo-site:ro" \
    -w /work/frontend \
    "mcr.microsoft.com/playwright:v${version}-noble" \
    bash -c 'PORT=4174 node scripts/browser-server.mjs /work/demo-site >&2 &
        exec node scripts/capture-demo-home.mjs' > "$work/home.jpeg"

mkdir -p test-results/demo
mv "$work/home.jpeg" test-results/demo/home.jpeg
printf 'Captured frontend/test-results/demo/home.jpeg\n'

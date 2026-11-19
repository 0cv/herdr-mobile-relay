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
mkdir "$work/captures"

docker run --rm --init --pull never --platform linux/amd64 --network none \
    -e PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    -v "$PWD:/work/frontend:ro" \
    -v "$work/site:/work/demo-site:ro" \
    -v "$work/captures:/work/captures" \
    -w /work/frontend \
    "mcr.microsoft.com/playwright:v${version}-noble" \
    bash -c 'PORT=4174 node scripts/browser-server.mjs /work/demo-site >&2 &
        exec node scripts/capture-demo-home.mjs /work/captures'

files=(home.jpeg terminal.jpeg agent_plan.jpeg conversations.jpeg git-history.jpeg
    devices.jpeg devices-invite.jpeg devices-qr.jpeg speech.jpeg notifications.jpg)
for file in "${files[@]}"; do
    if [[ ! -s "$work/captures/$file" ]]; then
        printf 'Missing demo capture: %s\n' "$file" >&2
        exit 1
    fi
done

mkdir -p test-results/demo
mv "${files[@]/#/$work/captures/}" test-results/demo/
cp "${files[@]/#/test-results/demo/}" ../images/
printf 'Captured %s demo screenshots in frontend/test-results/demo/ and images/.\n' "${#files[@]}"

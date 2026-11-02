#!/usr/bin/env bash
set -euo pipefail
: "${APPIUM_HOME:?Fresh canonical iOS Appium home required}"
: "${RUNNER_TEMP:?Runner evidence directory required}"
root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
node_binary="$(node -p 'require("node:fs").realpathSync(process.execPath)')"
case "${1:-}" in
  install)
    "$node_binary" --experimental-import-meta-resolve "$root/gate.ts" configure "$APPIUM_HOME"
    npm install --prefix "$APPIUM_HOME" --install-strategy=nested --no-audit --no-fund --engine-strict
    # Everything below is after the last npm mutation, including WDA/logger installation.
    "$node_binary" --experimental-import-meta-resolve "$root/gate.ts" pristine "$APPIUM_HOME"
    bun "$root/test.ts" original "$APPIUM_HOME" "$node_binary"
    "$node_binary" --experimental-import-meta-resolve "$root/gate.ts" patch "$APPIUM_HOME"
    bun "$root/test.ts" patched "$APPIUM_HOME" "$node_binary"
    "$node_binary" "$APPIUM_HOME/node_modules/appium/build/lib/main.js" driver list --installed >"$RUNNER_TEMP/appium-drivers.txt"
    ;;
  verify)
    "$node_binary" --experimental-import-meta-resolve "$root/gate.ts" verify "$APPIUM_HOME"
    ;;
  start)
    shift
    "$node_binary" --experimental-import-meta-resolve "$root/gate.ts" verify "$APPIUM_HOME"
    exec "$node_binary" "$APPIUM_HOME/node_modules/appium/build/lib/main.js" "$@"
    ;;
  *) exit 2 ;;
esac

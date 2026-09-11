#!/usr/bin/env bash
set -euo pipefail
: "${APPIUM_HOME:?Fresh Android Appium home required}"
: "${RUNNER_TEMP:?Runner evidence directory required}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "${1:-}" in
  install)
    node --experimental-import-meta-resolve "$root/gate.ts" configure "$APPIUM_HOME"
    APPIUM_SKIP_CHROMEDRIVER_INSTALL=1 npm ci --prefix "$APPIUM_HOME" --ignore-scripts --no-audit --no-fund --engine-strict
    node --experimental-import-meta-resolve "$root/gate.ts" patch "$APPIUM_HOME"
    node "$APPIUM_HOME/node_modules/appium/build/lib/main.js" driver list --installed >"$RUNNER_TEMP/appium-drivers.txt"
    ;;
  verify)
    node --experimental-import-meta-resolve "$root/gate.ts" verify "$APPIUM_HOME"
    ;;
  evidence)
    output="${2:?Evidence output directory required}"
    if [ -f "$APPIUM_HOME/retained-owner-integrity.json" ]; then
      test "$(wc -c < "$APPIUM_HOME/retained-owner-integrity.json")" -le 1048576
      mkdir -p "$output"
      cp "$APPIUM_HOME/retained-owner-integrity.json" "$output/android-appium-integrity.json"
      chmod 600 "$output/android-appium-integrity.json"
    fi
    ;;
  start)
    shift
    node --experimental-import-meta-resolve "$root/gate.ts" verify "$APPIUM_HOME"
    exec node "$APPIUM_HOME/node_modules/appium/build/lib/main.js" "$@"
    ;;
  *) exit 2 ;;
esac

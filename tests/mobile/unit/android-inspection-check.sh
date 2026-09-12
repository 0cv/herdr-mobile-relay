#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
fixture="$(mktemp -d /tmp/herdr-inspection-check.XXXXXXXX)"
trap 'rm -rf "$fixture"' EXIT
printf '%s\n' 'herdr-owned-inspection-fixture' > "$fixture/.owned"
mkdir -p "$fixture/source/tests/mobile" "$fixture/source/.github"
cp -R "$root/tests/mobile/android-appium" "$root/tests/mobile/unit" "$root/tests/mobile/support" "$fixture/source/tests/mobile/"
cp -R "$root/.github/workflows" "$root/.github/actions" "$fixture/source/.github/"
ln -s "$root/tests/mobile/node_modules" "$fixture/source/tests/mobile/node_modules"
cd "$fixture/source"
export APPIUM_HOME="$fixture/install" RUNNER_TEMP="$fixture/output"
export HERDR_OWNED_INSTALLER_FIXTURE=1 MOBILE_INSPECTION_TEST_OUTPUT="$fixture/output"
export MOBILE_PRISTINE_ANDROID_PACKAGE="$fixture/pristine"
unset NODE_PATH
mkdir -p "$RUNNER_TEMP"
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts configure "$APPIUM_HOME"
(cd "$APPIUM_HOME" && APPIUM_SKIP_CHROMEDRIVER_INSTALL=1 npm ci --ignore-scripts --no-audit --no-fund --engine-strict)
cp -R "$APPIUM_HOME/node_modules/appium-android-driver" "$MOBILE_PRISTINE_ANDROID_PACKAGE"
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts patch "$APPIUM_HOME"
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts verify "$APPIUM_HOME"
node --test tests/mobile/unit/android-adb-inspection.cjs
node --experimental-import-meta-resolve --test tests/mobile/unit/android-installed-inspection.mjs
node --test tests/mobile/unit/android-target-inspection.cjs
node --experimental-import-meta-resolve tests/mobile/unit/android-installed-owner.mjs
node --experimental-import-meta-resolve tests/mobile/unit/android-inspection-integrity.mjs
diff -r "$root/tests/mobile/android-appium" tests/mobile/android-appium
diff -r "$root/tests/mobile/support" tests/mobile/support
diff -r "$root/.github/workflows" .github/workflows
diff -r "$root/.github/actions" .github/actions
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts verify "$APPIUM_HOME"

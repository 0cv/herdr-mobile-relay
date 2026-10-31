#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/herdr-inspection-check.XXXXXXXX")"
trap 'rm -rf "$fixture"' EXIT
printf '%s\n' 'herdr-owned-inspection-fixture' > "$fixture/.owned"
mkdir -p "$fixture/source/tests/mobile" "$fixture/source/.github"
cp -R "$root/tests/mobile/android-appium" "$root/tests/mobile/unit" "$root/tests/mobile/support" "$fixture/source/tests/mobile/"
cp -R "$root/.github/workflows" "$root/.github/actions" "$fixture/source/.github/"
ln -s "$root/tests/mobile/node_modules" "$fixture/source/tests/mobile/node_modules"
cd "$fixture/source"
export APPIUM_HOME="$fixture/install" RUNNER_TEMP="$fixture/output"
export HERDR_OWNED_INSTALLER_FIXTURE=1 MOBILE_INSPECTION_TEST_OUTPUT="$fixture/output"
export MOBILE_PRISTINE_ANDROID_PACKAGE="$fixture/pristine" MOBILE_PRISTINE_UIA_PACKAGE="$fixture/pristine-uia"
unset NODE_PATH
mkdir -p "$RUNNER_TEMP"
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts configure "$APPIUM_HOME"
(cd "$APPIUM_HOME" && APPIUM_SKIP_CHROMEDRIVER_INSTALL=1 npm ci --ignore-scripts --no-audit --no-fund --engine-strict)
cp -R "$APPIUM_HOME/node_modules/appium-uiautomator2-driver/node_modules/appium-android-driver" "$MOBILE_PRISTINE_ANDROID_PACKAGE"
mkdir -p "$MOBILE_PRISTINE_UIA_PACKAGE/lib" "$MOBILE_PRISTINE_UIA_PACKAGE/build/lib"
cp "$APPIUM_HOME/node_modules/appium-uiautomator2-driver/lib/driver.ts" "$MOBILE_PRISTINE_UIA_PACKAGE/lib/"
cp "$APPIUM_HOME/node_modules/appium-uiautomator2-driver/build/lib/driver."{js,js.map,d.ts,d.ts.map} "$MOBILE_PRISTINE_UIA_PACKAGE/build/lib/"
node --experimental-import-meta-resolve tests/mobile/unit/android-installed-create-errors.mjs behavior pristine >"$RUNNER_TEMP/create-pristine-behavior.log" 2>&1 || { printf '%s\n' 'FAIL installed pristine create behavior'; exit 1; }
if node --experimental-import-meta-resolve tests/mobile/unit/android-installed-create-errors.mjs retention pristine >"$RUNNER_TEMP/create-pristine-retention.log" 2>&1; then
  printf '%s\n' 'FAIL pristine unexpectedly retains original create error'
  exit 1
else
  test "$?" -eq 41
fi
printf '%s\n' 'PASS installed pristine behavior and original-error retention negative'
node --experimental-import-meta-resolve tests/mobile/android-appium/gate.ts patch "$APPIUM_HOME"
node --experimental-import-meta-resolve tests/mobile/unit/android-installed-create-errors.mjs behavior patched >"$RUNNER_TEMP/create-patched-behavior.log" 2>&1 || { printf '%s\n' 'FAIL installed patched create behavior'; exit 1; }
node --experimental-import-meta-resolve tests/mobile/unit/android-installed-create-errors.mjs retention patched >"$RUNNER_TEMP/create-patched-retention.log" 2>&1 || { printf '%s\n' 'FAIL installed patched create retention'; exit 1; }
printf '%s\n' 'PASS installed patched create behavior and original-error retention'
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

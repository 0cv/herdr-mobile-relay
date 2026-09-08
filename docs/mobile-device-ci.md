# Installed-PWA device CI

`tests/mobile` exercises the release web tree on a real Android emulator or iOS simulator. It installs from the Home Screen flow, starts two local TLS/WSS relay peers, completes encrypted invitation pairing, switches the same origin to a verified candidate bundle, and checks the executing app build, credentials, preferences, lifecycle, keyboard, and bounded reload evidence.

The workflow has a manual/development mode and a reusable release mode. The release workflow passes its exact `release-bundles` artifact to `.github/workflows/mobile-ci.yml`; publication waits for every selected Android and iOS matrix entry. Do not treat a manual run from another commit as release evidence.

## Host-only checks

```sh
make mobile-ci-check
```

This runs the pinned Bun lint/type check/unit suite and the Go fixture tests. It does not pretend to validate Home Screen installation. A device run requires the platform tools and Appium listed in `tests/mobile/toolchains.json`.

## Preparing bundles locally

The candidate must be a release archive with its checksum and release manifest. The preparation command rejects a candidate directory unless the explicit local-only escape hatch is used.

```sh
bun install --frozen-lockfile --cwd tests/mobile
bun run --cwd tests/mobile prepare:bundle -- \
  --candidate "$PWD/dist/release/herdr-mobile-relay_0.21.0_linux_amd64.tar.gz" \
  --candidate-version 0.21.0 \
  --candidate-assets 367 \
  --candidate-revision "$(git rev-parse HEAD)" \
  --candidate-sha256 "$(awk '$2 == "herdr-mobile-relay_0.21.0_linux_amd64.tar.gz" { print $1 }' dist/release/checksums.txt)" \
  --output "$PWD/run-artifacts/mobile" \
  --baseline 0.20.8 --baseline 0.20.9 --baseline 0.20.10
```

For harness development only, a checked-out `web` directory can be used with `--allow-candidate-directory true`. That mode cannot prove archive provenance and is not used by CI.

The resulting `bundle-set.json` uses paths relative to its own directory, so it can be moved as an artifact without silently pointing at another checkout.

## Local device run

Build the fixture and install the harness dependencies first:

```sh
mkdir -p run-artifacts
go build -trimpath -o "$PWD/run-artifacts/herdr-mobile-fixture" ./tests/mobile/fixture
bun install --frozen-lockfile --cwd tests/mobile
```

For Android, create exactly one disposable emulator and record an ownership marker before running. The adapter refuses a physical or unmarked target, because it clears browser data and removes installed web providers:

```sh
avdmanager create avd --force --name herdr-mobile-ci --package "system-images;android-35;google_apis_playstore;x86_64" --device pixel_7
emulator -avd herdr-mobile-ci -no-window -no-audio -no-boot-anim -no-snapshot &
export MOBILE_PLATFORM=android
export ANDROID_SERIAL=emulator-5554
export MOBILE_DEVICE_OWNERSHIP_FILE="$PWD/run-artifacts/android-owned"
printf 'android:%s\n' "$ANDROID_SERIAL" > "$MOBILE_DEVICE_OWNERSHIP_FILE"
npm install --global appium@3.1.1
appium driver install uiautomator2@8.2.2
appium --address 127.0.0.1 --port 4723 &
export MOBILE_FIXTURE_BINARY="$PWD/run-artifacts/herdr-mobile-fixture"
```

For iOS, select the Xcode/runtime declared in `toolchains.json`, create one disposable iPhone 16 simulator, and record its ownership marker. The adapter does not erase an already booted simulator; the owner creates a fresh simulator instead:

```sh
sudo xcode-select -s /Applications/Xcode_16.4.app
runtime="$(xcrun simctl list runtimes | grep -E '^iOS 18\\.5 ' | grep -v unavailable | grep -oE 'com\\.apple\\.CoreSimulator\\.SimRuntime\\.[^ ]+' | head -n1)"
device_type="$(xcrun simctl list devicetypes | awk -F'[()]' '/iPhone 16 \\(/ { print $2; exit }')"
export IOS_SIMULATOR_UDID="$(xcrun simctl create herdr-mobile-ci "$device_type" "$runtime")"
export MOBILE_DEVICE_OWNERSHIP_FILE="$PWD/run-artifacts/ios-owned"
printf 'ios:%s\n' "$IOS_SIMULATOR_UDID" > "$MOBILE_DEVICE_OWNERSHIP_FILE"
xcrun simctl boot "$IOS_SIMULATOR_UDID"
xcrun simctl bootstatus "$IOS_SIMULATOR_UDID" -b
npm install --global appium@3.1.1
appium driver install xcuitest@12.10.0
appium --address 127.0.0.1 --port 4723 &
export MOBILE_PLATFORM=ios
export MOBILE_FIXTURE_BINARY="$PWD/run-artifacts/herdr-mobile-fixture"
```

Run one baseline at a time:

```sh
bun run --cwd tests/mobile run -- \
  --bundle-set "$PWD/run-artifacts/mobile/bundle-set.json" \
  --baseline 0.20.10 \
  --suite release \
  --output "$PWD/run-artifacts/device" \
  --private-output "$PWD/run-artifacts/private-device"
```

The runner keeps fixture info, TLS keys, and relay stores under `--private-output`, separate from the evidence directory. It removes private state in `finally`; workflow cleanup also removes it after failures. Only screenshots, `mobile-result.json`, and bounded redacted diagnostics belong in uploaded evidence. Do not copy `fixture-info.json` or relay state outside the private run directory.

## CI inputs and evidence

A manual workflow dispatch requires `artifact_run_id`, the successful `check` run that contains `release-bundles`. Automatic runs are limited to successful `main` checks. The release workflow invokes the same file as a reusable workflow with the exact artifact produced by its `build` job, and `publish` depends on the mobile result. Preparation validates the Linux candidate for Android and the Darwin arm64 candidate for iOS against the archive checksum, release manifest, web descriptor, hashed assets, and web-tree hash. It also runs a real Chromium/Go-fixture cache-recovery check with normal browser caching: the baseline loads, the target activates, a target-only missing stylesheet response is consumed, the failed phone plan is observed, and the candidate is reached through the shipped recovery button after the fault is repaired.

Each device matrix entry selects one of 0.20.8, 0.20.9, or 0.20.10 as the installed baseline. The release suite injects one bounded corrupt candidate-script response for historical upgrades and a missing-stylesheet response for the synthetic current-code pair, proves each request was consumed and phone completion was not acknowledged, then uses the shipped Try again control without reinstalling or re-pairing. It also builds two coherent temporary current-code bundles through the real frontend pipeline and runs the same fault/completion assertions against that pair. The historical 0.20.8/0.20.9 progress-format gap is reported as `HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:<version>` rather than treated as a successful acknowledgement or a failed upgrade. After recording completion evidence, the runner closes the non-dismissible update dialog, returns from Settings to the fixture's alpha agent, and only then runs the native keyboard check. Evidence artifacts contain the mobile result, screenshots, bounded fixture/Appium logs, driver versions, and platform version records. Secrets, setup fragments, relay credentials, and control headers are redacted or deleted before upload.

Physical-phone signoff remains separate: verify the same old-to-candidate flow on an approved Android handset and iPhone, with production access disabled and no uploaded device state. Record OS/browser/PWA provider, standalone launch, credential reconnect, preference preservation, cold relaunch, and keyboard results alongside the emulator artifacts; never treat simulator success as physical-device coverage.

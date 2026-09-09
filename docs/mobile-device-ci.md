# Installed-PWA device CI

`tests/mobile` exercises the release web tree on a real Android emulator or iOS simulator. It installs from the Home Screen flow, starts two local TLS/WSS relay peers, completes encrypted invitation pairing, switches the same origin to a verified candidate bundle, and checks the executing app build, credentials, preferences, lifecycle, keyboard, and bounded reload evidence.

The check workflow passes its exact `release-bundles` artifact to `.github/workflows/mobile-ci.yml` after the bundle job completes. Every device qualification first runs one controlled Android and iOS smoke against the latest historical baseline (`0.20.10`); a release request then runs one complete release scenario per platform before the requested full matrix. A push to `dev` stops after the controlled latest-baseline smoke, while `main` and pull requests targeting `main` continue through the full Android and iOS release suite across all historical baselines. The release device matrix adds the current-code synthetic pair once per platform, instead of repeating it for every historical baseline. Smoke matrices remain historical-only. Each matrix entry provisions its own disposable device and Appium session. The release workflow invokes the same reusable workflow for release tags and waits for the full suite against the exact release artifact before publication. These CI paths use GitHub-hosted runners with read-only permissions and no secrets.

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

Preparation requires a fresh output destination and refuses repository-root or input-overlapping paths rather than deleting caller files. The resulting `bundle-set.json` uses paths relative to its own directory, so it can be moved as an artifact without silently pointing at another checkout. After the archive checksum, manifest, and web-tree checks, preparation retains only each exact web tree, its release manifest, and bundle metadata; downloaded archives and native release files are not uploaded as mobile transport. Archive preparation requires GNU tar because safe extraction uses the GNU ownership and directory controls; on macOS install `gtar` and set `MOBILE_GNU_TAR` rather than weakening extraction.

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

Hosted Android installs the pinned Chrome/Trichrome pair declared in
`tests/mobile/toolchains.json` before Appium starts. The current reproducible
candidate is Chrome `131.0.6778.200` / version code `677820038` for x86+x86_64,
with the matching `com.google.android.trichromelibrary` library. CI verifies
both SHA-256 archives and the Google signing certificates, unpacks the Chrome
APKM, installs the library first, and then installs all Chrome splits. The
binary URLs are an APK.now mirror fallback because APKMirror is Cloudflare
blocked; replace them only with another source carrying the same hashes and
signing certificates.

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
open -Fn /Applications/Xcode_16.4.app/Contents/Developer/Applications/Simulator.app
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

A manual workflow dispatch requires `artifact_run_id`, the completed successful same-repository `check` push on `main` that contains `release-bundles`; provide `source_commit` when qualifying a specific revision. The dispatch path always uses external provenance. The reusable workflow validates repository, workflow, push event, `main` branch, conclusion, source SHA, exact artifact name, archive checksums, release-manifest revision, and expiration before provisioning devices. The check and release reusable calls bind to the caller's exact artifact and its manifest revision without comparing a pull-request merge build to the pull-request head SHA; evidence records that run head separately from the build SHA. The gate downloads only evidence artifacts from the current workflow attempt and validates each expected platform/baseline/suite/source/hash/native-provider result, including the exact recorded synthetic current-code web hash once per platform. Every Actions upload declares literal `retention-days: 1`, including release bundles, derived mobile bundles, controlled-smoke evidence, full-matrix evidence, and tag-release bundles. All exact source/build/mobile inputs must be consumed within that one-day window; an expired input requires a new recorded source run rather than an unrecorded substitute build. The release workflow invokes the same file with the exact artifact produced by its `build` job, and `publish` depends on the mobile result. Preparation validates the Linux candidate for Android and the Darwin arm64 candidate for iOS against the archive checksum, release manifest, web descriptor, hashed assets, and web-tree hash. It also runs a real Chromium/Go-fixture cache-recovery check with normal browser caching: the baseline loads, the target activates, a target-only missing stylesheet response is consumed, the failed phone plan is observed, and the candidate is reached through the shipped recovery button after the fault is repaired.

Each device matrix entry selects one of 0.20.8, 0.20.9, or 0.20.10 as the installed baseline. `baseline_set: latest` prepares only 0.20.10; smoke runs omit the unused synthetic bundle artifact. The controlled smoke and release suites inject one bounded corrupt candidate-script response for historical upgrades and a missing-stylesheet response for the synthetic current-code pair, prove each request was consumed and phone completion was not acknowledged, then use the shipped Try again control without reinstalling or re-pairing. It also builds two coherent temporary current-code bundles through the real frontend pipeline and runs the same fault/completion assertions against that pair. The historical 0.20.8/0.20.9 progress-format gap is reported as `HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:<version>` rather than treated as a successful acknowledgement or a failed upgrade. After recording completion evidence, the runner closes the non-dismissible update dialog, returns from Settings to the fixture's alpha agent, and only then runs the native keyboard check. Evidence artifacts contain the mobile result, screenshots, bounded fixture/Appium logs, driver versions, and platform version records. Secrets, setup fragments, relay credentials, and control headers are redacted or deleted before upload.

On hosted iOS, CI prebuilds the simulator WDA app, installs and launches it
through `simctl`/Appium's preinstalled-WDA path, and waits for
`http://127.0.0.1:8100/status` before creating the Safari session. This
separates a successful Xcode build from proof that WDA is actually listening;
bounded WDA and simulator logs are uploaded on failure.

Physical-phone signoff remains separate: verify the same old-to-candidate flow on an approved Android handset and iPhone, with production access disabled and no uploaded device state. Record OS/browser/PWA provider, standalone launch, credential reconnect, preference preservation, cold relaunch, and keyboard results alongside the emulator artifacts; never treat simulator success as physical-device coverage.

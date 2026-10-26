# Installed-PWA device CI

`tests/mobile` exercises the release web tree on a real Android emulator or iOS simulator. It installs from the Home Screen flow, starts two local TLS/WSS relay peers, completes encrypted invitation pairing, switches the same origin to a verified candidate bundle, and checks the executing app build, credentials, preferences, lifecycle, keyboard, and bounded reload evidence.

The check workflow passes its exact `release-bundles` artifact to `.github/workflows/mobile-ci.yml` after the bundle job completes. A release request first qualifies Android with one complete controlled release scenario against the latest historical baseline (`0.20.10`) before any iOS job starts. Every device qualification then runs one controlled Android and iOS smoke against that baseline; a release request then runs one complete release scenario per platform before the requested full matrix. A push to `dev` stops after the controlled latest-baseline smoke, while `main` and pull requests targeting `main` continue through the full Android and iOS release suite across all historical baselines. The release device matrix adds the current-code synthetic pair once per platform, instead of repeating it for every historical baseline. Smoke matrices remain historical-only. Each matrix entry provisions its own disposable device and Appium session. The release workflow invokes the same reusable workflow for release tags and waits for the full suite against the exact release artifact before publication. These CI paths use GitHub-hosted runners with read-only permissions and no secrets.

## Product result and environment qualification

The schema-2 mobile result separates `product.status` (PASS, FAIL or
INDETERMINATE), `environment_qualification`, and `finalization.status`. Android
qualification is PASS, FAIL or UNKNOWN; iOS explicitly reports NOT_APPLICABLE.
The runner exits successfully only for product PASS with successful required
collection, cleanup and evidence emission. Cleanup failures cannot erase the
original scenario/driver failure. Measurement END and collection finish before
native teardown; the result retains a by-value pre-teardown observation receipt
and a separate teardown snapshot.

Ordinary smoke can pass with environment FAIL only for completely validated GMS
user-0 enabled/disabled-component drift with stable remaining package, dependency,
image and provenance fields. Component comparisons use complete arrays and
recomputed canonical hashes, not public projections or counts. Chrome/Trichrome
drift, changed dependency identity, firstFatal, ownership/session/target loss,
invalid inspection or collection still blocks product smoke. Unknown child
relation is INDETERMINATE, also blocking: different browser and child PIDs, later
HTTP success, `isolated not needed`, or an unknown initiator do not prove a death
unrelated. Only the complete normal-retirement and exact planned-cold policies
below exempt a death; there is no general signal or exit-zero exemption.

The recorder is activated by the runner, not by ordinary driver admission. It
retains actual owner-object ordinals, native birth/kernel capability, bounded
inspection brackets, read/mutation receipts and the exact measured cold operation.
Each phase requires the reads and transitions performed by that scenario, with
consecutive operation counters and fresh inspection references. Initial install,
pairing navigation, asynchronous upgrade activation and warm/cold launch are
explicit transitions; document changes inside a read remain failures. The final
release checkpoint may reuse the immediately preceding lifecycle checkpoint
because no driver operation runs between them. Both driver snapshots require
explicit state fields, including the absence or presence of a first fatal error.
Owned-loss relation uses the retained owner segment plus measured log boundaries,
process inventories and, for a new cold owner, an unambiguous process-start record.
Clock-domain ambiguity, missing bounds or PID reuse cannot establish ownership.
These are bounded relations, not exact kernel-birth timestamps or child causality.
It is limited to 1,000 combined records and 8 MiB; overflow fails rather than
truncating. Coverage is bounded operations, not continuous renderer attribution.
The historical child5820 case remains product INDETERMINATE / environment FAIL;
its target relation is unproved and no historical result is rewritten.

`android-environment.ts check` requires `--contract report|qualification` and
explicit current-run, attempt, suite, baseline, scenario, prepared bundle and head
arguments. It recomputes and compares the immutable schema-3 runner assessment;
neither outer postcheck overwrites it. Input hashes bind sanitized persisted
bytes; invalid UTF-8 is rejected before decoding can change their identity.
Collector `bytes` counts persisted sanitized log bytes; `acquiredBytes` records
the separate raw acquisition count. Report mode permits a complete negative
assessment, never missing or invalid collection. Qualification mode additionally requires PASS.
`validate-evidence.ts --contract product|qualified` validates the complete expected
matrix and exact source/build/candidate/run/attempt identities. Both contracts
retain the installed-provider, runtime, fault, credential and preference checks.
The offline Android validator checks public installed-producer manifest/hash and
resolver parity; it does not open private APPIUM_HOME state or run the installer.

The reusable workflow exposes **Product smoke evidence**, **Environment report
publication (not qualification)** and release-only **Strict environment
qualification**. Report publication is independent of product success, validates
every expected row (including iOS), and visibly reports negative qualification.
FAIL dominates UNKNOWN while incomplete subchecks remain reported. A successful
report job means report delivery, not qualification. Missing completion,
observation or driver proof is INCOMPLETE with a bounded EVIDENCE_INVALID reason
and a nonzero report exit; it is not reclassified as an observed product failure.
A recorded product FAIL with complete evidence can still be reported. Missing
rows/artifacts or outputs never default to PASS. Console, step-summary and summary artifacts use
bounded allowlisted projections; omitted component names retain total and
withheld counts. Restricted diagnostics are not public summaries.

Release smoke, full-device evidence and the final release gate require qualified
results. Only that complete gate emits `qualification_status=PASS`; ordinary
smoke emits NOT_REQUESTED. The first release publication step asserts PASS before
any publication action. `product_status` and `environment_status` are derived
from validated current-attempt rows, not matrix last-writer outputs. Native
product errors are not suppressed with continue-on-error. Existing check.yml
scheduling, including main/ci-mobile release/all, is unchanged. GitHub protection
rule/check-name mapping is a separate operator decision, not changed here.

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
android_system_image="$(jq -r '.android.systemImage' tests/mobile/toolchains.json)"
android_device_profile="$(jq -r '.android.deviceProfile' tests/mobile/toolchains.json)"
avdmanager create avd --force --name herdr-mobile-ci --package "$android_system_image" --device "$android_device_profile"
emulator -avd herdr-mobile-ci -no-window -no-audio -no-boot-anim -no-snapshot &
export MOBILE_PLATFORM=android
export ANDROID_SERIAL=emulator-5554
export MOBILE_DEVICE_OWNERSHIP_FILE="$PWD/run-artifacts/android-owned"
printf 'android:%s\n' "$ANDROID_SERIAL" > "$MOBILE_DEVICE_OWNERSHIP_FILE"
npm install --global appium@3.7.0
appium driver install uiautomator2@8.7.0
appium --address 127.0.0.1 --port 4723 &
export MOBILE_FIXTURE_BINARY="$PWD/run-artifacts/herdr-mobile-fixture"
```

Hosted Android installs the pinned Chrome/Trichrome pair declared in
`tests/mobile/toolchains.json` before Appium starts. The current reproducible
CI-only pin is Chrome `131.0.6778.200` / version code `677820038` for
x86+x86_64, with the matching `com.google.android.trichromelibrary` library.
It is not a local ARM64-aligned browser release; keep `verified` false until a
matching signed ARM64 and x86_64 Chrome/Trichrome pair has binary manifests,
hashes, certificates, and hosted qualification. CI verifies
both SHA-256 archives and the Google signing certificates, unpacks the Chrome
APKM, installs the library first, and then installs all Chrome splits. The
hosted emulator uses the declared Google APIs image without the Play Store and
records GMS, module, Chrome, and Trichrome identities before and after each
Android scenario; package replacement or a forced restart fails qualification
and is independently assessed for product impact. A cold-booted emulator's GMS
sends its module init intents once at boot but on hosted runners takes up to
about three minutes to finish them, enabling and disabling its own components
until it logs `NewModuleHelper: Completed all new module init intents for all
processes.` Each Android job therefore runs one step before its scenario step
that samples every 5 seconds until that line is in the main log buffer and the
normalized GMS component state has then been unchanged for 20 seconds. If that
does not happen by device uptime 600 seconds, or three consecutive samples fail,
the step only warns and the unchanged strict before/after comparison decides. A
device uptime that does not increase between successful samples fails the step;
a restart long enough to fail three samples in a row ends as a warning instead.
Once sampling ends,
`android-gms-settle.json` records the decision, sampling failures and every
sample, and the step only warns if that file cannot be written. The
normal-retirement exception admits only
isolated Chrome sandbox helpers with a complete in-interval fork/start identity,
absent from the START inventory and absent from the END inventory unless listed
there under the same process name (that inventory is taken just before the END
marker), whose Chrome zygote is in the START inventory, or is absent from it and was forked
exactly once, by `zygote64`, inside the measurement, remains in the END inventory
and never exits. The helper must then end with that Zygote's report of exit
status zero, optionally with one system_server `has died` record whose label is
only the process's importance at death (such as `vis BTOP`, `fg  SVC` or
`cch+75 CEM`) or one `isolated not needed` kill with the matching isolated UID;
or with one such `isolated not needed` kill logged before that Zygote's SIGKILL
report by timestamp or capture order. A kill logged after a clean exit is
accepted because the helper can exit on its own while the system reaps it. If
there is no Zygote exit record, retirement instead requires exactly one helper
`ChildProcessService: Exiting child process.` record, followed by exactly one
system_server `isolated not needed` kill with the matching isolated UID and at
least one later `Child process died (type=N) pid=...` record from the Chrome
browser PID in the START process inventory. The helper's records must end by
the kill, its death event set must be exactly that kill, and the numbered
proof retains the helper self-exit, kill, and browser observation. Any Zygote
exit, including a crash signal, continues to use only the existing exit rules.
Except for this no-exit case, the Chromium child exit message, the helper's
lifetime and the order in which the fork, start, kill and exit records arrive
are not required, because independent producers stamp and submit them. For
force-stop and package checks
the lifetime starts at the earliest fork, start or helper record by timestamp
or by capture order. A planned Chrome force-stop
also covers helpers from its observed process set that the system kills as
`isolated not needed` after system_server logs `Force stopping com.android.chrome`
for user 0 inside the exact operation interval; that exemption admits only the
kill and one following START-inventory Chrome zygote report of exit status zero or
SIGKILL, so any other signal, crash or second exit stays fatal. The schema-3
check retains raw observations and numbered proof lines, reporting normal helper
lifetimes separately from fatal events and distinguishing event counts from PID
counts. The helper's own records may be stamped up to 20 ms before the Zygote
fork and no later than its exit, or no later than the matching kill when there
is no Zygote exit, in any capture order, and must fit both START/END marker
bounds, including audit subject timestamps. The fork and start record must always be inside both bounds; only when
the helper's own child exit, Zygote exit, matching ActivityManager kill or
helper-attributed Android death record is at or after END's timestamp and after
END in capture order may its remaining records, and records that mention it,
also come from the capture after END, still under every other check above. The
required Chrome browser observation does not by itself extend this post-END
allowance. Capture is monitored, receive-bounded `main`/`system` logcat, not an
exhaustive or lossless all-producer generation-time trace. Producers sample time
before submission; another producer's earlier/later timestamp is diagnostic
`boundaryDiscordances`, not proof of capture loss. Original bytes and line numbers
are retained without sorting. All captured records, including pre-START and
post-END spill, remain available for adverse and ambiguous attribution checks;
planned termination markers and exempted events must also fit both receive and
timestamp bounds. A planned termination waits up to 5 seconds after `am force-stop`
for every observed process of the package to exit before it logs `OP_END`; if any
observed PID or any process of that package is still listed after 5 seconds (or
25 seconds after `OP_BEGIN`, whichever comes first), the operation fails without
`OP_END`. Missing/duplicate markers, malformed records, reported loss and
collector failures still reject. A paused producer can submit only after collection
stops, and absence of loss text in these buffers cannot prove universal losslessness.
No quiet wait or marker provides an all-producer submission barrier.
Strictly parsed Android logd AVC subject records retain separate
`auditSubjects` attribution and raw numbered lines: logd reconstructs the subject
PID/TID and original timestamp, so publication after exit is not helper execution.
Only the supported nonpermissive isolated-app file-denial format, corroborated
numeric UID and unique audit serial qualify; original timestamps remain bounded,
and audit subjects stamped more than 20 ms before the fork or carrying adverse
content still reject. This textual reference
contract is not authenticated socket provenance or runtime-image attestation.
A helper PID appearing only as digits in another process's `Killing` UID field
is not a reference to that helper. PID references elsewhere in the record and
the killed helper's own isolated-UID consistency checks remain enforced.
Missing, ambiguous, reused or contradictory identities and incomplete log
coverage fail closed; force-stop, other signals, nonzero exits, crashes and
replacement evidence override clean-exit evidence. Within normal retirements and
the planned force-stop exemption, the only accepted helper signal is the SIGKILL
completing a matching system `isolated not needed` kill, and that reason is never
sufficient without the complete helper identity or planned operation. Main/privileged Chrome processes and GMS are not covered, and
native/document ownership and required teardown guarantees remain blocking.
Only the component-only product policy described above differs from strict
configuration qualification. Android 15's package manager stores a static-library record
as `<library-name>_<version-code>`; the snapshot reads Chrome's declared dependency,
queries that exact record, and records its resolved package path instead of passing a
version selector to `pm path`. The binary URLs are an APK.now mirror fallback because
APKMirror is Cloudflare blocked; replace them only with another source carrying
the same hashes and signing certificates.

For iOS, select the Xcode/runtime declared in `toolchains.json`, create one disposable iPhone 17 simulator, and record its ownership marker. Hosted iOS runs on the `xcode-27` label with Xcode 27.0 (27A266a) at `/Applications/Xcode_27.app` and the iOS 27.0 simulator runtime. iOS 27 is the only supported iOS target, so WDA is also built with `IPHONEOS_DEPLOYMENT_TARGET=27.0`. The adapter does not erase an already booted simulator; the owner creates a fresh simulator instead. Before any simulator exists, the hosted entrypoints check the selected developer directory and the Xcode version/build and fail without installing or falling back. They retain `ios-hosted-target.txt` (expected and observed Xcode identity plus the runner's `ImageOS`, `ImageVersion` and macOS version/build), the complete runtime inventory, and the selected inventory line, identifier, version, and build as `ios-runtime-inventory.txt` and `ios-runtime-selection.txt` in uploaded output, even when later provisioning or WDA steps fail. GitHub does not let a workflow pin a hosted image revision, so the image identity is recorded rather than enforced:

Locally, pass the developer directory explicitly instead of changing the global Xcode selector; hosted CI also runs `sudo xcode-select -s /Applications/Xcode_27.app` and requires `xcode-select -p` to report the same developer directory:

```sh
export DEVELOPER_DIR=/Applications/Xcode_27.app/Contents/Developer
xcodebuild -version  # must report Xcode 27.0 and Build version 27A266a
export IOS_PLATFORM_VERSION=27.0
runtime_inventory="$PWD/run-artifacts/ios-runtime-inventory.txt"
runtime_selection="$PWD/run-artifacts/ios-runtime-selection.txt"
if ! xcrun simctl list runtimes | tee "$runtime_inventory" >/dev/null; then
  printf 'expected_platform_version=%s\ninventory_command=failed\n' "$IOS_PLATFORM_VERSION" > "$runtime_selection"
  exit 1
fi
runtime_line="$(awk -v prefix="iOS $IOS_PLATFORM_VERSION (" 'index($0, prefix) == 1 && $0 !~ /unavailable/ { print; exit }' "$runtime_inventory")"
runtime="$(printf '%s\n' "$runtime_line" | sed -nE 's/.* - (com\.apple\.CoreSimulator\.SimRuntime\.[^ ]+).*/\1/p')"
runtime_version="$(printf '%s\n' "$runtime_line" | sed -nE 's/^iOS ([0-9]+\.[0-9]+) \(.*/\1/p')"
runtime_build="$(printf '%s\n' "$runtime_line" | sed -nE 's/^iOS [0-9]+\.[0-9]+ \([^)]* - ([^)]+)\) - .*/\1/p')"
printf 'expected_platform_version=%s\ninventory_line=%s\nselected_identifier=%s\nselected_version=%s\nselected_build=%s\n' \
  "$IOS_PLATFORM_VERSION" "$runtime_line" "$runtime" "$runtime_version" "$runtime_build" > "$runtime_selection"
test "$runtime_version" = "$IOS_PLATFORM_VERSION"
test -n "$runtime"
test -n "$runtime_build"
device_type="$(xcrun simctl list devicetypes | awk -F'[()]' '/iPhone 17 \(/ { print $2; exit }')"
export IOS_SIMULATOR_UDID="$(xcrun simctl create herdr-mobile-ci "$device_type" "$runtime")"
export MOBILE_DEVICE_OWNERSHIP_FILE="$PWD/run-artifacts/ios-owned"
printf 'ios:%s\n' "$IOS_SIMULATOR_UDID" > "$MOBILE_DEVICE_OWNERSHIP_FILE"
xcrun simctl boot "$IOS_SIMULATOR_UDID"
xcrun simctl bootstatus "$IOS_SIMULATOR_UDID" -b
npm install --global appium@3.7.0
appium driver install xcuitest@12.12.4
appium --address 127.0.0.1 --port 4723 &
export MOBILE_PLATFORM=ios
export MOBILE_FIXTURE_BINARY="$PWD/run-artifacts/herdr-mobile-fixture"
```

### Local iOS 27 operator prerequisite

Hosted CI targets the `xcode-27` image described above. A local host still needs
its own preparation: stable Xcode 27 requires macOS Tahoe 26.6 or newer, so a
local qualification is not possible on an older host.

Before a local iOS 27 qualification, an operator must use the Apple release
information at [Apple's iOS 27 support page](https://support.apple.com/en-us/100100),
the [Xcode support matrix](https://developer.apple.com/support/xcode/), and the
[Apple developer downloads](https://developer.apple.com/download/all/) to:

1. confirm supported Mac hardware and record measured free disk space with
   `df -h /` (reserve enough for macOS, Xcode, the iOS 27 simulator runtime, and
   WDA derived data; this project does not invent a fixed disk-size threshold);
2. upgrade macOS to at least 26.6 during an approved maintenance window;
3. install the stable Xcode 27 release and the exact iOS 27 simulator runtime,
   accept the license, and complete first-launch components with operator
   credentials; and
4. use an isolated developer directory and owned simulator while recording
   `DEVELOPER_DIR=/Applications/Xcode_27.app/Contents/Developer xcodebuild -version`
   and `DEVELOPER_DIR=/Applications/Xcode_27.app/Contents/Developer xcrun simctl list runtimes`.

After those identities are recorded, rebuild WDA from the pinned XCUITest
12.12.4/WDA 16.12.8 tree, run the managed XCTest/Appium qualification on an
owned iOS 27 simulator, and retain the runtime/build provenance. Do not use a
beta Xcode, a `macos-latest` label, or an unmeasured runtime. Do not change the
local global Xcode selector for this qualification; pass `DEVELOPER_DIR`
explicitly to each `xcodebuild`/`xcrun` command.

Run one baseline at a time. Supply explicit positive `GITHUB_RUN_ID` and
`GITHUB_RUN_ATTEMPT` identifiers plus the exact `MOBILE_SOURCE_RUN_HEAD_SHA`; local
receipts do not substitute for CI provenance or grant native execution authority:

```sh
bun run --cwd tests/mobile run -- \
  --bundle-set "$PWD/run-artifacts/mobile/bundle-set.json" \
  --baseline 0.20.10 \
  --suite release \
  --output "$PWD/run-artifacts/device" \
  --private-output "$PWD/run-artifacts/private-device"
```

The runner keeps fixture info, TLS keys, and relay stores under `--private-output`, separate from the evidence directory. It attempts every acquired-resource cleanup in `finally`, retaining individual failures; workflow cleanup also attempts removal after failures. Persistence or cleanup failures produce nonzero execution, not a guarantee that an artifact can be recovered. Only screenshots, `mobile-result.json`, and bounded redacted diagnostics belong in uploaded evidence. Do not copy `fixture-info.json` or relay state outside the private run directory.

## CI inputs and evidence

A manual workflow dispatch requires `artifact_run_id`, the completed successful same-repository `check` push on `main` that contains `release-bundles`; provide `source_commit` when qualifying a specific revision. The dispatch path always uses external provenance. The reusable workflow validates repository, workflow, push event, `main` branch, conclusion, source SHA, exact artifact name, archive checksums, release-manifest revision, and expiration before provisioning devices. The check and release reusable calls bind to the caller's exact artifact and its manifest revision without comparing a pull-request merge build to the pull-request head SHA; evidence records that run head separately from the build SHA. The gate downloads only evidence artifacts from the current workflow attempt and validates each expected platform/baseline/suite/source/hash/native-provider result, including the exact recorded synthetic current-code web hash once per platform. Every Actions upload declares literal `retention-days: 1`, including release bundles, derived mobile bundles, controlled-smoke evidence, full-matrix evidence, and tag-release bundles. All exact source/build/mobile inputs must be consumed within that one-day window; an expired input requires a new recorded source run rather than an unrecorded substitute build. The release workflow invokes the same file with the exact artifact produced by its `build` job, and `publish` depends on the mobile result. Preparation validates the Linux candidate for Android and the Darwin arm64 candidate for iOS against the archive checksum, release manifest, web descriptor, hashed assets, and web-tree hash. It also runs a real Chromium/Go-fixture cache-recovery check with normal browser caching: the baseline loads, the target activates, a target-only missing stylesheet response is consumed, the failed phone plan is observed, and the candidate is reached through the shipped recovery button after the fault is repaired.

A release request qualifies Android before any iOS work starts. The `android_qualification` job runs one complete controlled release scenario on Android with the 0.20.10 baseline and uploads it under its own `mobile-evidence-android-qualification` prefix, separate from smoke, release-smoke and full-device evidence. `android_qualification_evidence` then validates that current-attempt result with the qualified contract and requires strict environment qualification (`validate-environment.ts --require-pass`). Controlled smoke, which starts the first iOS simulator, waits for both jobs to succeed; a failed, cancelled or skipped Android qualification therefore prevents every iOS job and fails the gate. Ordinary smoke requests skip both jobs and keep reporting `qualification_status=NOT_REQUESTED`. This barrier is additional evidence: the complete Android and iOS matrices below still run.

Each device matrix entry selects one of 0.20.8, 0.20.9, or 0.20.10 as the installed baseline. `baseline_set: latest` prepares only 0.20.10; smoke runs omit the unused synthetic bundle artifact. The controlled smoke and release suites inject one bounded corrupt candidate-script response for historical upgrades and a missing-stylesheet response for the synthetic current-code pair, prove each request was consumed and phone completion was not acknowledged, then use the shipped Try again control without reinstalling or re-pairing. It also builds two coherent temporary current-code bundles through the real frontend pipeline and runs the same fault/completion assertions against that pair. The historical 0.20.8/0.20.9 progress-format gap is reported as `HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:<version>` rather than treated as a successful acknowledgement or a failed upgrade. After recording completion evidence, the runner closes the non-dismissible update dialog, returns from Settings to the fixture's alpha agent, and only then runs the native keyboard check. Evidence artifacts contain the mobile result, screenshots, bounded fixture/Appium logs, driver versions, and platform version records. Secrets, setup fragments, relay credentials, and control headers are redacted or deleted before upload.

On hosted iOS, both CI entrypoints prebuild pinned WDA 16.12.8 and use
`tests/mobile/support/ios-xctest.ts` to supervise one managed
`xcodebuild test-without-building` invocation. The helper selects exactly one
expected xctestrun and runner for the owned simulator. It records the child PID,
registration receipts, status identity and exit status under `IOS_XCTEST_STATE_DIR`.
Appium requires that live owner and uses its loopback `webDriverAgentUrl`; it does
not launch another WDA. The session also sets `appium:isHeadless`, and no
simulator UI (DeviceHub since Xcode 27) is opened: without it, Appium reboots a
booted simulator that has no UI window, which kills the managed WDA. The
existing 300-second startup deadline is shared with HTTP checks. Always-run
finalization stops the owner before diagnostic upload. Cleanup still validates
owned listeners, terminates only an owned runner, and stops and waits for the
owned xcodebuild process, but no longer queries the simulator's native system
log. It writes the payload-free `ios-wda-system-log-disposition.json` with
`not-requested-product-first`; this records a deliberate no-query decision, not
collection status or an explanation for a native timeout. Evidence-copy steps
include this disposition and do not copy an old system-log file. Local harness
invocations also require this managed owner; the simulator/Appium setup above
alone is not sufficient. Supply the already-built WDA product,
bootstrap and project paths, runtime and fresh state directory before running
`bun tests/mobile/support/ios-xctest.ts start`; finish with the matching `stop`
command and shutdown of only the marked simulator.

Hosted iOS jobs set `MOBILE_TIMING_SCALE=3`, which triples every phase and
WebDriver request allowance below the unchanged scenario deadline; the values
below are the unscaled ones used locally and by unit tests.

Initial Safari navigation issues one `simctl openurl` with a 90-second cap.
The following 110-second readiness phase allows two complete 45-second WebKit
discovery calls, a 15-second attachment, URL verification and return to native
context, plus 3 seconds for polling and overhead. The first discovery also
locates and connects to the simulator's Web Inspector socket, which hosted
runners have taken more than 20 seconds to do. All phases remain bounded by
the enclosing run deadline. A native-only discovery is not readiness: the
adapter must attach and read the expected origin, and a timed-out driver command
still quarantines the session rather than permitting another query.

When Safari's compact layout exposes Page Menu instead of Share, installation
opens the visible, enabled native `MoreMenuButton` once, verifies Safari remains
foreground, and observes the menu before looking for Share again. A web-content
lookalike cannot select Page Menu. Missing or unusable Share controls still fail
closed; this path does not skip the Share sheet or final installation confirmation.
A collapsed Share sheet may expose `View More` instead of its installation row.
The adapter selects only one visible, enabled action inside Safari's native Share
sheet, verifies it is hittable, and clicks it at most once. Lookup, attribute
checks and click are capped at five seconds each, with a complete hierarchy
observation reserved. A fresh bounded publication wait follows expansion under
the original search deadline. The expanded list must still pass the existing
visibility, row identity and progress checks; collection visibility is not waived.
The final confirmation is the one button whose accessibility name or visible
label is `Add` inside the visible navigation bar titled `Add to Home Screen`
(by the bar's name, label or a title text inside it). iOS 27 labels this button
`Add` without guaranteeing the accessibility name. The same selector proves the
button is unique before the single final tap. Failure evidence outside an
installed-app launch failure also saves one bounded native hierarchy as
`failure-hierarchy.xml`, unless a fatal driver failure has quarantined native
queries. The Share-sheet installation-row click records its start, acknowledgement
or failure separately from the final Add lookup; acknowledgement alone does not
prove the confirmation appeared.

The normal real-app journey no longer requests automatic confirmation GET
`/source` after that acknowledgement. It proceeds directly to the existing
foreground and final-Add checks. `confirmationCapture` records the explicit
`not-requested-product-first` disposition and copied row-click acknowledgement
timing, with no source receipt or projection. Other readiness and failure-evidence
source requests are unchanged. Client timing is correlation, not proof of native
cause or confirmation readiness.

The unique visible Add, complete enabled/visible/hittable checks, stable identity
and exactly one final tap are unchanged, as are settings and all deadlines:
5-second row click, 8-second lookup, 12-second final click, 75-second confirmation,
64-second round, 4-second restoration reserve and 1.5-second parent tail. A fatal
or unusable driver still blocks native diagnostics and further product commands;
settings restoration is not attempted on that session.

This retires r37's automatic pre-lookup capture recipe only for the normal journey.
Old r37/run22 evidence and failures remain unchanged. Hosted run40004048167's
diagnostic request prevented product lookup; removing it is not an underlying
iOS/native fix or a passing real-device test. Run39971886984 failed final-Add
lookup without this diagnostic, and its cause remains unknown. The standalone
projection utility and hypothetical fixtures remain local regression tools,
not observed native recovery or hosted qualification.

Both mobile CI upload paths withhold `RUNNER_TEMP/appium.log` on iOS, remove
any stale copy from the artifact directory, and emit the small static
`appium-log-disposition.json` instead. Use client timing and bounded diagnostics.
Android's existing log-copy behavior is unchanged. This privacy decision covers
the uploaded Appium log only: private ephemeral local Appium logging remains
enabled, and the startup-only console dump remains unchanged. It does
not claim removal of other existing restricted diagnostic artifacts.

The XCTest owner retains at most 128 recognized activity enums with host receipt
timestamps, plus unknown, oversized, incomplete and dropped-event counts.
Raw output, element labels, application names and source timestamps stay withheld.
Each stream has a 4 KiB line bound under the unchanged 100 MiB combined input cap.
These narrowly matched XCTest activity messages are diagnostic hints, not trusted
command identities, execution ordering, tap completion or qualification evidence.
The parser tests use synthetic activity lines; a hosted recovery is still required.

Android retains a separate all-buffer threadtime startup diagnostic before the
first certificate-preparation session. It connects only to the existing local
ADB server, uses one shell-v2 stream, and records bounded stdout/stderr, framing,
exit and truncation state. A closed stream without an exit frame has unknown
remote status. This preceding-window diagnostic does not replace the mandatory
qualified environment collector or establish exhaustive coverage or cleanliness.

Physical-phone signoff remains separate: verify the same old-to-candidate flow on an approved Android handset and iPhone, with production access disabled and no uploaded device state. Record OS/browser/PWA provider, standalone launch, credential reconnect, preference preservation, cold relaunch, and keyboard results alongside the emulator artifacts; never treat simulator success as physical-device coverage.

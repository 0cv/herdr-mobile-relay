# R0 CI/parser review

## Scope and verdict

This is the scoped R0 review of the CI/parser repair, not approval or implementation of the full Tailscale feature. The input candidate was `a034f1eebd90ddde9e38ad8644a0338b2a448f4f`, reviewed against base `b74a1c16c9c4a1b1207cad88fb00b33556e04025`. The final scoped worker repair is commit `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5`, on `ci/tailscale-native-preflight-ef9f843`.

The final source snapshot has Git tree `7213ce234d5335c2a1c03e5d5b13206c7543e04d`, 833 tracked entries, and source digest `sha256:3068043029a381e2fab1ff88cdc3d7b11140c652c9a4e7fe7b842093ea4f6d01`. GitHub check run `36032804212` independently reported the same commit, tree, count, and digest in the exact-checkout provenance step. The first check on `b9f493b` and its one predeclared failed-job rerun exposed a teardown error in an unrelated app test. A single narrowly justified test-fixture repair was added at `internal/app/server_test.go`; the final candidate's check and native runs both pass. No retry-until-green was performed.

```text
automatedChecksPassed: true   # final exact-commit check and native runs passed
scopedSourceReviewClean: false # independent reviewers have not returned verdicts
featureReady: false            # remains false for this R0-only task
```

## Baseline, dirty paths, and protected files

At entry, the assigned linked worktree was on `ci/tailscale-native-preflight-ef9f843` at candidate `a034f1e`; tracked files and index were clean. Existing user work appeared as `?? docs/plans/`. The expanded initial untracked inventory was:

- `docs/plans/tailscale-macos/NEXT-RUN.md`
- `docs/plans/tailscale-macos/PLAN.md`
- `docs/plans/tailscale-macos/REMAINING-IMPLEMENTATION-PLAN.md`
- `docs/plans/tailscale-macos/REVIEW.md`
- `docs/plans/tailscale-macos/execution/STATUS.json`
- `docs/plans/tailscale-macos/execution/WORKER-LOG.md`
- `docs/plans/tailscale-macos/imported/ACCEPTANCE-MATRIX.md`
- `docs/plans/tailscale-macos/imported/HANDOFF.md`
- `docs/plans/tailscale-macos/imported/MACOS-RUNBOOK.md`
- `docs/plans/tailscale-macos/imported/PLAN-VALIDATION.md`
- `docs/plans/tailscale-macos/imported/PLAN.md`
- `docs/plans/tailscale-macos/imported/SHA256SUMS`
- `docs/plans/tailscale-macos/imported/SLICE-TEMPLATE.md`
- `docs/plans/tailscale-macos/imported/START-HERE.md`
- `docs/plans/tailscale-macos/imported/VERIFICATION.md`
- `docs/plans/tailscale-macos/imported/handoff/UNAPPROVED-portable-bool.patch`
- `docs/plans/tailscale-macos/imported/handoff/plan-checks.json`
- `docs/plans/tailscale-macos/imported/handoff/provenance.json`
- `docs/plans/tailscale-macos/imported/handoff/source-inventory.json`
- `docs/plans/tailscale-macos/imported/status.json`
- `docs/plans/tailscale-macos/imported/verify-source.py`

After the worker commits, the index and tracked worktree are clean; `origin/ci/tailscale-native-preflight-ef9f843` points to `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5`. The final expanded untracked set is those same 21 user-owned paths plus this new `docs/plans/tailscale-macos/execution/CI-REVIEW.md`. No pre-existing plan/imported file was edited, staged, or committed. The imported source verifier was not run, and no standalone-source-verifier pass is claimed. The 14-file imported checksum verification passed.

The task-only source delta from candidate `a034f1e` to final commit `d2e5d01` contains six paths. The first five were committed as `b9f493b`; the last was the predeclared allowlist amendment committed as `d2e5d01`:

- `internal/tailscale/contract.go`
- `internal/tailscale/json_field_test.go`
- `relay/common.sh`
- `tests/test_checkout_provenance.py`
- `tests/test_common.sh`
- `internal/app/server_test.go`

### Narrow allowlist amendment (recorded before editing)

The one declared hosted rerun did not clear the full Go gate: run `36029030893`, attempt 1, failed the same app fixture in normal and race jobs; the single bounded rerun (attempt 2) passed normal Go quality but failed the race job again in that test. The two logs identify `internal/app/server_test.go:1635`, where `serverDone` propagates an error from the test's fake Unix-socket Herdr server after the test cancels the poller and closes the listener. Attempt 1 reported `EOF`; attempt 2 reported `write unix ... broken pipe`. The fixture currently treats decode/write errors from its peer as fatal unconditionally. The exact test passed once in each focused local mode, so this is not claimed as a local reproduction; the repeated full-suite CI failures plus the shutdown sequence are the evidence.

Before editing, the repair allowlist was amended by one path, `internal/app/server_test.go`, solely to classify fake-peer disconnects as expected after explicit fixture shutdown while continuing to fail on transport errors before shutdown and on unexpected methods. The fake now records decode/write errors until the test explicitly enters shutdown; the method-validation failure remains unconditional. This is a bounded test-fixture correction, not a product behavior change or expansion into the Tailscale feature. Its focused normal/race tests and the final required hosted gates passed; no additional CI retry was performed.

## Frozen 34-path candidate review inventory

The full reviewed candidate delta from base `b74a1c1` to candidate `a034f1e` contains exactly these 34 paths:

```text
.github/actions/mobile-device-run/action.yml
.github/workflows/check.yml
.github/workflows/mobile-ci.yml
.github/workflows/release.yml
.github/workflows/tailscale-native-preflight.yml
Dockerfile.gateway
cmd/herdr-mobile-relay/json_field.go
cmd/herdr-mobile-relay/json_field_test.go
cmd/herdr-mobile-relay/main.go
docs/development.md
go.mod
internal/deviceauth/store_test.go
internal/deviceauth/tailscale_preservation_test.go
internal/processsupervisor/supervisor_test.go
internal/tailscale/DR-1-session-authority.md
internal/tailscale/contract.go
internal/tailscale/json_field_test.go
internal/webrtclink/manager_test.go
relay/common.sh
relay/tailscale.sh
scripts/verify-checkout-provenance.py
tests/mobile/fixture/main_test.go
tests/test_checkout_provenance.py
tests/test_common.sh
tests/test_plugin_build.sh
tests/test_tailscale.sh
tests/test_tailscale_child_jobs.py
tests/test_tailscale_consent.py
tests/test_tailscale_launcher_order.py
tests/test_tailscale_process_groups.py
tests/test_tailscale_process_groups_darwin.py
tests/test_tailscale_release_identity.py
tests/test_tailscale_reprint.py
tests/test_tailscale_space_paths.py
```

Review boundaries included the parser/packaged CLI (`internal/tailscale/contract.go`, `internal/tailscale/json_field_test.go`, `cmd/herdr-mobile-relay/{json_field.go,json_field_test.go,main.go}`), shell dispatch and status consumers (`relay/common.sh`, `relay/tailscale.sh`, unchanged `relay/setup-link.sh`, `relay/plugin-setup-menu.sh`, `relay/start.sh`), packaged release inclusion (`scripts/package-release.sh`), workflow/provenance inputs, the original acceptance tests and fakes, the amended app-test teardown fixture (`internal/app/server_test.go`), and the draft authority record. The caller audit confirmed that the helper is selected by `relay_binary` without recursively invoking JSON parsing; setup-link passes its resolved binary for release identity checks; optional display telemetry stays optional, while permission-bearing booleans fail closed. `ServeRouteOwned` remains false. The draft DR-1 record is not an owner decision and does not authorize mutation.

## Worker findings and dispositions (not independent closure)

These are worker-detected issues with fixes/evidence for reviewer assessment; they are not marked independently closed.

| ID | Finding | Worker change and evidence | Disposition |
|---|---|---|---|
| `R0-SUB-01` | JSON string extraction rejected ASCII controls but accepted Unicode C1 controls such as U+009B. | Use `unicode.IsControl`; add `C1 string control character` regression case in `internal/tailscale/json_field_test.go`. The focused parser/race tests passed locally. | Worker repair; independent review pending. |
| `R0-SUB-02` | Shell JSON helper handling obscured useful errors when the relay binary was missing or too old to implement `json-field`. | Preserve `relay_binary` selection diagnostics, distinguish unsupported-helper exit status, and add missing/old-helper checks in `tests/test_common.sh`. Targeted shell-accessor/diagnostic checks passed. | Worker repair; independent review pending. |
| `R0-SUB-03` | Provenance negative controls did not demonstrate that byte/mode tampering is still found when Git `assume-unchanged` hides it from ordinary status. | Add temporary-Git byte and mode tests asserting status is clean while direct provenance verification refuses the changed checkout. | Test strengthening; independent review pending. |

## Review identities and verdicts

The worker inspected source and callers but does not count that as independent review. Independent reviewers were not assigned or available in this worker session, so no independent verdict is claimed.

| Reviewer | Identity | Focus | Verdict |
|---|---|---|---|
| Correctness/security | Not available in this worker session | JSON bounds, decoded duplicate/case handling, exact types/control characters, relay/CLI bootstrap, fail-closed consumers, and authority boundaries | Pending; no report received |
| CI/test validity | Not available in this worker session | Workflow triggers/pins, provenance, job gates/skips, artifacts/logging, namespace capability checks, and lifecycle fixture assertions | Pending; no report received |
| Worker source audit | This implementation session | Candidate source, callers, tests, workflow evidence, and bounded CI repair | Not independent; no approval verdict |

`scopedSourceReviewClean` therefore remains false; findings above remain subject to reviewer disposition.

## Verification evidence

### CI for final commit `d2e5d01`

- Native run **36032804006**, exact head `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5`: passed both Darwin arm64 and Linux amd64 preflight jobs, including native Go tests/vet/build, gated synthetic lifecycle checks, Linux namespace proof and child-job fixtures, and required-check gates. This is disposable hosted synthetic qualification only, not real-tailnet qualification.
- Check run **36032804212**, exact head `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5`: exact checkout provenance passed with the final digest above; pinned actionlint, format, `go vet ./...`, normal/race `go test ./...`, shell/production paths, frontend/mobile browser job, Linux journal acceptance, release bundles, and all four native release smokes passed. Configured mobile harness/device jobs were skipped and are excluded from coverage claims.
- Earlier failure history is retained: check **36029030893**, attempt 1, failed both normal/race at `internal/app.TestProductionEventInventoryRecoveryDrainsRefreshAcrossReconnect` (`server_test.go:1635: EOF`); the single predeclared `--failed` rerun passed normal Go quality and failed race at the same server-done check with `write unix ... broken pipe`. No data race was reported. The amended fake-server teardown now ignores only transport errors after explicit shutdown; unexpected methods and earlier I/O failures remain failures. The corrected exact candidate then passed both full Go gates in `36032804212`.
- Local post-amendment commands `go test -count=1 ./internal/app -run '^TestProductionEventInventoryRecoveryDrainsRefreshAcrossReconnect$'` and the corresponding `go test -race -count=1 ...` both passed with private temp/cache state, followed by `go vet ./...`. The bounded repro before the amendment also passed once in normal and race mode; those isolated passes did not substitute for hosted suite evidence.

### Earlier exact candidate evidence and local checks

- For input candidate `a034f1e`, check run **36006375810** and native run **36006375121** both passed their configured gates. Those results bind to `a034f1e`; final `d2e5d01` verification is supplied by runs **36032804212** and **36032804006**.
- Local Go version: `go1.27.1 darwin/arm64`. Focused Go commands use `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off GOFLAGS=-mod=readonly`, with private mode-0700 `GOCACHE` and `TMPDIR`:

```text
go test -race -count=1 ./internal/tailscale ./cmd/herdr-mobile-relay -run '^(TestExtractJSONField|TestRunJSONField)'
  PASS (focused parser/packaged CLI tests)
go test -count=1 ./internal/app -run '^TestProductionEventInventoryRecoveryDrainsRefreshAcrossReconnect$'
  PASS (bounded local reproduction; temporary local Unix/HTTP fixtures only)
go test -race -count=1 ./internal/app -run '^TestProductionEventInventoryRecoveryDrainsRefreshAcrossReconnect$'
  PASS (same bounded focused test under race detector)
go vet ./...
  PASS (all packages)
go build -trimpath -o <private-temp>/herdr-mobile-relay ./cmd/herdr-mobile-relay
  PASS
python3 tests/test_checkout_provenance.py -v
  PASS (temporary Git fixtures; 9 tests, including assume-unchanged byte/mode refusal)
bash -n relay/common.sh tests/test_common.sh tests/test_tailscale.sh
  PASS
```

The project shell suite and lifecycle-containing broad Make targets were not run locally. Targeted accessor dispatch and missing/old-helper diagnostic checks used a private adapter and passed. No personal-host Tailscale, tailnet, production service, lifecycle fixture, or production state was used. `actionlint` was unavailable locally; the pinned hosted actionlint step passed.

### Protected-file check and exclusions

`cd docs/plans/tailscale-macos/imported && shasum -a 256 -c SHA256SUMS` passed all 14 listed files. Do not infer full roadmap completion from R0 CI. Explicitly deferred: full 112-path feature review; F04 authority decision and any implementation; remaining writer/journal, readiness, browser, and native-support gates; full feature slices; and real-tailnet qualification. No personal-host lifecycle fixtures, real tailnet activity, deployment, release, or infrastructure operation was performed.

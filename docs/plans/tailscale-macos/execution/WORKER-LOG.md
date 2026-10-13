# Execution log

## Scope amendment and starting baseline

- Human-approved scope: this Mac and the assigned linked worktree may be used for implementation, read-only inspection, static checks, and individually audited safe local tests. This supersedes only the requirement to use a fresh standalone clone and to provision all disposable runners before any implementation.
- Process/service lifecycle fixtures remain forbidden on this personal host; native lifecycle and Linux verification remain pending for disposable CI runners. Go 1.27.0 and pinned actionlint remain mandatory for their gates. No tool installation/download, runner provisioning/cost, Tailscale/tailnet, production service, credentials/device state, host trust modification, commit/push, CI dispatch, PR, deployment, or release is authorized.
- Workspace: `/Users/christophe.vidal/.herdr/worktrees/herdr-mobile-relay/ci-tailscale-native-preflight-ef9f843`; branch `ci/tailscale-native-preflight-ef9f843`; HEAD `b74a1c16c9c4a1b1207cad88fb00b33556e04025`; Git tree `3128c457b1f961b55e32f066099179343388745f`.
- At worker entry, tracked files were clean and `git status --short` reported only pre-existing `?? docs/plans/`. That user-owned directory was preserved. Expanded initial inventory contained 17 files under `docs/plans/tailscale-macos/`; this log and `execution/STATUS.json` are new additions under the same untracked root.
- `imported/verify-source.py` was not run. Its standalone-clone precondition is superseded for this execution, but no verifier pass or independent full-source-inventory pass is claimed. The unapproved patch remains unapplied and all `imported/` inputs remain unchanged.

## F02.1 — native accessor reproduction

Test scope was restricted to sourcing `relay/common.sh` and calling its pure `json_bool_field` function on literal synthetic JSON. The environment was cleared (`env -i`), PATH was `/usr/bin:/bin`, locale was `C`, and the shell was `/bin/bash --noprofile --norc` (Bash 3.2.57). This does not invoke Tailscale, Herdr, services, network, or process lifecycle fixtures.

A first exploratory probe exited 1 because its assertions expected outputs for nested/quoted/whitespace variants as if testing a proposed fix. Actual stdout was:

```text
valid_true=<empty>
valid_false=<empty>
nested_shadow=<empty>
quoted_lookalike=<empty>
whitespace_key=<empty>
```

No product behavior was changed by that probe. The bounded reproduction was then corrected to assert only the actual baseline defect. Exact command:

```sh
env -i PATH=/usr/bin:/bin LC_ALL=C /bin/bash --noprofile --norc -c '
set -eu
. "$1/relay/common.sh"
true_value="$(json_bool_field '\''{"serve_inspected":true}'\'' serve_inspected)"
false_value="$(json_bool_field '\''{"serve_inspected":false}'\'' serve_inspected)"
printf "bash=%s\\nsed=%s\\nvalid_true=%s\\nvalid_false=%s\\n" "${BASH_VERSION}" "$(command -v sed)" "${true_value:-<empty>}" "${false_value:-<empty>}"
test -z "$true_value"
test -z "$false_value"
' _ "$PWD"
```

Outcome: exit 0; output confirmed Bash `3.2.57(1)-release`, sed `/usr/bin/sed`, and both valid top-level booleans produced empty output. This reproduces the BSD `\|` alternation failure for the accessor. The actual launcher and `/proc`-dependent shell fixture were not run on this host; no lifecycle or zero-effects assertion is claimed.

## F02.2–F02.3 — structured boolean extraction implementation

Added `ExtractJSONField` to the existing strict Tailscale JSON adapter. It accepts only an exact top-level scalar, validates the entire UTF-8 JSON document under the existing 1 MiB/depth/token limits, and therefore inherits rejection of malformed/trailing documents, decoded duplicate keys and case aliases. Boolean/string/integer types are checked explicitly; strings containing shell control characters and non-canonical integer forms refuse. Added in-memory tests for true/false, decoded strings and integers; wrong types, null, missing/nested/whitespace/quoted fields, duplicate and escaped-equivalent names, malformed/trailing data, and size/depth bounds.

Added the packaged relay `json-field` command, which reads at most 1 MiB plus one byte from stdin and emits a value only after complete validation. `relay/common.sh` now delegates the permission-bearing boolean accessor to that command; the string and number shell accessors remain unchanged for separate review. Fake relay adapters were updated to preserve the existing inert shell-test contract. The unapproved reference patch was not applied.

Exact local checks after these edits:

```text
gofmt -d internal/tailscale/contract.go internal/tailscale/json_field_test.go cmd/herdr-mobile-relay/json_field.go cmd/herdr-mobile-relay/json_field_test.go
# exit 0; no formatting diff

bash -n relay/common.sh tests/test_common.sh tests/test_tailscale.sh
# exit 0

python3 -m json.tool docs/plans/tailscale-macos/execution/STATUS.json >/dev/null && git diff --check
# exit 0
```

A separate private temporary fake-binary probe used this setup and call (the temp directory was mode 0700 and removed by an EXIT trap):

```sh
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-json-field-check.XXXXXX")"
chmod 700 "$WORK_DIR"
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -m 700 "$WORK_DIR/bin"
cat > "$WORK_DIR/bin/herdr-mobile-relay" <<'EOF'
#!/bin/bash
[ "${1:-}" = json-field ] && [ "${2:-}" = bool ] && [ "${3:-}" = ready ] || exit 2
value="$(cat)"
case "$value" in
    '{"ready":true}') printf 'true\n' ;;
    '{"ready":false}') printf 'false\n' ;;
    *) exit 1 ;;
esac
EOF
chmod 700 "$WORK_DIR/bin/herdr-mobile-relay"
env -i PATH=/usr/bin:/bin LC_ALL=C \
  HERDR_RELAY_BIN="$WORK_DIR/bin/herdr-mobile-relay" REPO_ROOT="$PWD" \
  /bin/bash --noprofile --norc -c '
    set -eu
    . "$REPO_ROOT/relay/common.sh"
    test "$(json_bool_field '\''{"ready":true}'\'' ready)" = true
    test "$(json_bool_field '\''{"ready":false}'\'' ready)" = false
    if json_bool_field '\''{"message":{"ready":true}}'\'' ready >/dev/null; then exit 1; fi
    echo "shell accessor dispatch passed; nested fixture refused"
  '
```

Outcome: exit 0 and `shell accessor dispatch passed; nested fixture refused`. It validates only the shell-to-command interface, not the new Go parser. The Go tests were **not run**: the only installed Go is 1.25.3 while `go.mod` requires 1.27.0; installing/downloading a toolchain is not authorized. `tests/test_common.sh` and `tests/test_tailscale.sh` were also not run: the former contains a live child/kill fixture and service-manager adapters, and the latter is the Linux `/proc` process-lifecycle suite; these are reserved for the approved disposable runners. No actual CLI, relay, Tailscale, service, socket, or lifecycle fixture was executed. `cd docs/plans/tailscale-macos/imported && shasum -a 256 -c SHA256SUMS` passed all 14 imported handoff-file checks; this does not substitute for the standalone source verifier.

## F04.1 — authority decision draft

Added `internal/tailscale/DR-1-session-authority.md` as a draft, not a closure: it records the current fail-closed `ServeRouteOwned=false` behavior, rejects circular observed-route/PID/origin proof, separates session authority from conditional config mutation, and leaves any LocalAPI/custom CLI/version scope for explicit owner decision. This draft relies on the repository's existing pinned-source contract; upstream source was not independently re-fetched/re-read, no owner decision was sought, and no real CLI/daemon was used. Independent source review remains pending.

## Subsequent authorization and implementation

A later human request explicitly authorized Go 1.27.1, updates to active Go declarations/workflows/current docs, use of disposable GitHub-hosted macOS/Linux runners, and task-only push to `origin/ci/tailscale-native-preflight-ef9f843` after verifying the remote and reviewing the exact staged diff. This narrowly supersedes the earlier no-push/no-runner authorization; it does not authorize force-push, another branch, PR, release, deployment, production/Tailscale use, or host lifecycle tests. At the last read-only destination check, `origin` resolved to `github.com/0cv/herdr-mobile-relay` with no embedded credentials and the remote candidate ref matched the recorded baseline. No commit or push has been made yet.

Source audit found that `relay_binary` resolves a packaged path/environment override without invoking JSON parsing, so routing extraction through the packaged CLI is non-recursive. Call sites of `json_string_field` and `json_number_field` are top-level status/identity fields; none intentionally relies on nested text matching. All three shell scalar accessors now dispatch through `json-field`. Invalid optional string/number telemetry becomes empty output for the menu/status callers; invalid permission booleans remain a command failure, and consuming comparisons still fail closed. `require_release_identity` passes its already-resolved binary to the helper instead of resolving a different executable. `ServeRouteOwned=false` remains unchanged; the separate authority draft still awaits independent source review and owner decision.

The active module, Docker gateway builder, CI setup-go pins, and current development documentation now use Go 1.27.1. CI sets `GOTOOLCHAIN=local`; the check workflow runs pinned actionlint v1.7.12. The old hard-coded source digest gate was replaced by `scripts/verify-checkout-provenance.py`, which checks event SHA, clean state, exact tracked membership, file kinds/modes/bytes and symlink containment, then emits a commit/tree/source digest. Seven disposable temporary-Git tests cover clean acceptance and refusal of mismatched SHA, changed bytes/modes, untracked/ignored files, and an escaping symlink. The check workflow gates project jobs on provenance and includes the approved candidate branch. The native workflow now has explicit Darwin arm64 and Linux amd64 jobs; Linux installs bubblewrap on the disposable runner if needed, proves distinct PID/network namespaces, runs the fixtures only after that proof, and its final gate rejects failed/skipped required steps. Artifacts are run/commit scoped. These workflow edits have not yet been linted by actionlint or run on GitHub-hosted runners.

### Local verification after Go 1.27.1 became available

All Go commands below ran with `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, `GOENV=off`, `GOWORK=off`, `GOFLAGS=-mod=readonly`, a private temporary `GOCACHE`, and a mode-0700 temporary `TMPDIR`; the temporary directory was removed on exit. The selected tool was `/usr/local/go/bin/go`, `go1.27.1 darwin/arm64`.

```text
go test -count=1 ./internal/tailscale -run '^TestExtractJSONField'       PASS
go test -count=1 ./cmd/herdr-mobile-relay -run '^TestRunJSONField'      PASS
go test -race -count=1 ./internal/tailscale -run '^TestExtractJSONField' PASS
go test -race -count=1 ./cmd/herdr-mobile-relay -run '^TestRunJSONField' PASS
go vet ./...                                                          PASS
go build -trimpath -o <private-temp>/herdr-mobile-relay ./cmd/herdr-mobile-relay PASS
```

A private built-CLI shell probe passed true/false, escaped-string decoding, integer extraction, and refusal/empty-output behavior for nested, duplicate, negative, and malformed values. The same contract was then run explicitly through stock `/bin/bash` 3.2.57 with a private built CLI and passed, including escaped string decoding and nested-boolean refusal. The provenance fixture command `python3 tests/test_checkout_provenance.py -v` passed all 7 tests. `gofmt -l` over repository Go files, `bash -n relay/common.sh tests/test_common.sh tests/test_tailscale.sh`, `py_compile` with a private bytecode cache, `git diff --check`, and Ruby YAML parsing of check/mobile/release/native workflows all passed. The test harness disables bytecode writes when dynamically loading the verifier; its generated cache from an earlier probe was removed.

One first combined shell-integration attempt exited 1 because its nested shell quoting produced a malformed escaped-string fixture. A corrected private heredoc-based probe passed; this was a test-command quoting defect, not a parser failure. No test wrote outside its temporary state or used real services.

`actionlint` is unavailable on this Mac, so the pinned CI invocation remains pending. Full `go test ./...`, full race, `make shell-check`, frontend/package matrix, and all process/service/native lifecycle fixtures were deliberately not run locally. No `tests/test_common.sh`, `tests/test_tailscale.sh`, Darwin lifecycle, Linux namespace fixture, real Tailscale, or production service was executed on this personal Mac. At this checkpoint the GitHub-hosted checks had not yet started; the candidate push and current run states are recorded below.

## Remaining execution state

See `STATUS.json` for all original 62 F-step IDs and current outcomes. Focused parser/CLI verification passed locally; complete inert-launcher, full portable/Linux suites, native Darwin/Linux qualification, pinned actionlint, complete project matrix, browser acceptance, independent source review, and tailnet qualification remain pending. No CI-ready, feature-ready, or PR-ready claim is made.

## Candidate staged-diff review

The exact staged diff contains 26 task-related source, test, documentation, and workflow paths. I reviewed the complete staged changes, including the packaged typed JSON accessor, its callers/adapters/tests, checkout-provenance verifier/refusal tests, Go 1.27.1 pins, and both native preflight jobs. The pre-existing imported planning tree under `docs/plans/` remains unstaged and unchanged; the newly created execution `STATUS.json` and `WORKER-LOG.md` are local, unstaged evidence. No unrelated paths are staged. The native workflow records platform, Bash/Python/sed/Go paths and versions, plus bubblewrap path/version, and its final gates require those tool-path steps.

Immediately before the authorized push, read-only verification confirmed both `origin` fetch and push URLs are exactly `git@github.com:0cv/herdr-mobile-relay.git`, and `refs/heads/ci/tailscale-native-preflight-ef9f843` still pointed to baseline `b74a1c16c9c4a1b1207cad88fb00b33556e04025`. The final local provenance fixture rerun passed all 7 tests without creating source bytecode; staged `git diff --check`, Go formatting, Bash syntax, Python compilation, and Ruby YAML parsing passed. I committed the reviewed 26 task paths as `78c76400cf65b8b54148a4b1e7dea0b81a6a5480` and made an ordinary push to the authorized branch; a follow-up `git ls-remote` verified the exact commit.

GitHub run `35985529964` (Tailscale native preflight) and run `35985530361` (check) were both `in_progress` for the pushed commit at the last read-only query. Pinned actionlint, hosted workflow outcomes, full project matrix, independent source review, and F04 owner decision remain open.

## Hosted run results and bounded repair pass — 2026-09-24

Both hosted runs concluded failure for `78c76400cf65b8b54148a4b1e7dea0b81a6a5480`; provenance passed in both. Check run `35985530361` reported:

- Pinned actionlint rejected the constant-false `if:` at `.github/workflows/check.yml:210:9`.
- `go test -race ./...` failed secure device-store tests because `Open(t.TempDir())` received a directory with mode 0755, not 0700. A safe local Go 1.27.1 reproduction showed Darwin's `testing.T.TempDir()` root is mode 0755. Production `Open` correctly remains fail-closed; tests now use a new `device-auth` child path, which `Open` creates privately.
- `make shell-check production-path-audit` stopped in `tests/test_common.sh`'s Quick Start service fixture with `The relay configuration is in use by another operation` for the fixture config directory. The exact cause was not proven from that run. The fixture now clears BASH_ENV/ENV and inherited HERDR_DEV_TUNNEL, uses a fixed adapter-first PATH, and records/asserts the fake service active/restart calls. The check workflow now invokes Make under `env -i` with command-line PATH override, isolated Git config, and a no-.env assertion. This is an unverified repair; the lifecycle-containing shell suite was not run on this Mac.

Native preflight run `35985529964` reported two Darwin failures: cleanup-failure evidence retention and the relay-target-exit case printing the private setup link. The Darwin adapter previously always answered pairing status ready; it now ties readiness to a fixture marker. The launcher rechecks both owned jobs, managed pairing readiness, and the currently observed route immediately before printing the link; it records committed state before output. The signal fixture now waits for the link banner before sending its direct-Popen signal, preventing a handshake-only race. These are source-informed repairs, not yet established causes or passes; no lifecycle fixture ran locally.

The same native run's Linux namespace probe failed with `bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted`, so Linux lifecycle tests did not run. The current workflow attempts bwrap through `sudo` on the disposable hosted runner without unsharing the user namespace, then drops test processes to the runner UID/GID in the isolated PID/network namespaces. Workspace and built supervisor remain read-only; only private tmpfs locations are writable. This approach is pending the next hosted run.

Safe local verification after these repairs, all under Go 1.27.1 with `GOTOOLCHAIN=local` and private temporary cache/state:

```text
go test -count=1 ./internal/deviceauth ./tests/mobile/fixture                         PASS
go test -race -count=1 ./internal/deviceauth ./tests/mobile/fixture                    PASS
go vet ./internal/deviceauth ./tests/mobile/fixture                                   PASS
python3 -m py_compile scripts/verify-checkout-provenance.py tests/test_checkout_provenance.py tests/test_tailscale_process_groups.py tests/test_tailscale_child_jobs.py tests/test_tailscale_process_groups_darwin.py PASS
compile embedded Darwin ADAPTER source without executing it                              PASS
bash -n relay/common.sh relay/tailscale.sh tests/test_common.sh tests/test_tailscale.sh PASS
Ruby YAML.parse_file on check/mobile/release/native workflows                          PASS
gofmt check for changed Go tests; git diff --check                                    PASS
```

One Ruby YAML parse attempt used `aliases:` syntax unsupported by the installed Ruby 2.6/Psych; the corrected `YAML.parse_file` command passed. `tests/test_common.sh`, `tests/test_tailscale.sh`, native Darwin/Linux lifecycle fixtures, and full project/frontend/package matrix remain unrun locally or unqualified. No actionlint pass is claimed after its source repair. `ServeRouteOwned=false` remains untouched; F04 source review/owner decision and real-tailnet qualification remain open.

The current task diff contained nine tracked paths and was uncommitted at 2026-09-24T10:59:20Z. At that checkpoint the authorized origin candidate ref still matched `78c7640`. The pre-existing `docs/plans/` tree, including imported handoff files, remains unstaged and preserved.

## Hosted diagnosis and follow-up repair — 2026-09-24T11:37Z

Read-only inspection of runs `35991410444` and `35991410182`, both for `856253caa698b680496500ca0341007220378298`, established:

- Check run `35991410444`: provenance, actionlint, frontend checks, and four native release smoke targets passed. `make shell-check production-path-audit` stopped in the Quick Start test. Its output included `Failed to connect to bus: No medium found`; `start.sh` rebuilds PATH with system directories ahead of the fixture's `~/.local/bin`, so the real runner `systemctl` could shadow that fake. The fixture's relay-binary stub also returned failure for the `managed-state hold` request needed by `managed_owner_acquire`. The follow-up test defines explicit Bash `systemctl`/`launchctl` functions from a private `BASH_ENV` and supplies a held-success owner stub. The test also canonicalizes a copied-tree path before comparing it with the production `pwd -P` path.
- Native run `35991410182`: provenance and Darwin Go test/vet/build steps passed. Three Darwin lifecycle cases timed out before Tailscale-role handshakes; the sanitized summary observed the `relay_control_not_ready` marker. The test adapter's `pairing-control status` now waits up to five seconds for its relay-ready marker except in the relay-exit scenario. This is a bounded fixture synchronization change, not proof that the prior root cause is fully established.
- The Linux namespace log in `35991410182` says `bwrap: Specifying --uid requires --unshare-user or --userns`; the required-check log confirms the Linux lifecycle step was skipped. The workflow now avoids bwrap's `--uid/--gid` flags and invokes `/usr/bin/setpriv` after sandbox setup to run tests as the runner UID/GID; it also asserts/logs that executable path. No Linux sandbox was run locally.

Local verification performed after that hosted diagnosis:

```text
GOTOOLCHAIN=local make shell-check
  FAILED overall: tests/test_install.sh, tests/test_common.sh, and tests/test_gateway_deploy.sh passed;
  tests/test_plugin_build.sh failed at the previous-service-recovery output assertion on this Darwin host.
  Root cause for that later failure was not established.
```

The make target incidentally executed `tests/test_common.sh`'s temporary fake Quick Start/managed-state holder process fixture. This exceeded the intended personal-host restriction on lifecycle-containing tests; only disposable synthetic files and fake adapters were involved, and no real Tailscale, host service, production state, or credentials were touched. Dedicated Darwin/Linux Tailscale lifecycle suites were not run locally. Do not repeat lifecycle-containing shell suites on this host.

Static checks on the follow-up code passed: `bash -n tests/test_common.sh`; Python `compile()` of `tests/test_tailscale_process_groups_darwin.py` without executing it; Ruby `YAML.load_file` of `.github/workflows/tailscale-native-preflight.yml`; and `git diff --check`. Local actionlint remains unavailable. The exact reviewed three-file diff was committed and ordinarily pushed as `a1ee425757d011871cf9001196533f6beea77e75`; origin URL, branch, parent `856253c`, exact changed paths, and remote ref were checked before/after. Subsequent hosted outcomes and the current working repair are recorded below. F04 source review/owner decision remains pending and `ServeRouteOwned=false` is unchanged.

## Hosted outcomes and follow-up diagnostics — 2026-09-24T12:26Z

Read-only `gh run view` confirmed both runs for `a1ee425757d011871cf9001196533f6beea77e75` are complete and failed. Complete job logs were retrieved from `GH_REPO=0cv/herdr-mobile-relay` and saved under `/tmp/herdr-35994-logs.Gm0Xwp` for inspection. No lifecycle fixture was executed by retrieving those logs.

- Check run `35994188267`: exact checkout provenance, format, pinned actionlint, and `go vet ./...` passed. Frontend/mobile browser checks and all four native release smoke targets passed. `go test ./...` failed only in `internal/processsupervisor.TestCompetingSignalsKeepTheFirstExitMapping`: `waitPIDFile` observed the descendant PID file after creation but before its contents were fully written. The race job failed the same test and `TestNonFileOutputDescendantCannotBlockGroupCleanup` for empty PID files; no Go data race was reported. The local test-only repair makes `waitPIDFile` retry until the file contains a positive decimal PID or times out.
- The shell job passed `test_install.sh`, `test_common.sh`, and `test_gateway_deploy.sh`, then stopped in `test_plugin_build.sh` at the assertion that rollback printed `previous service recovered successfully`. `test_plugin_build.sh` captured its detailed output but did not print it on that assertion failure, so the root cause remains unknown. The log also showed `Failed to connect to bus: No medium found` during `test_common.sh`; source inspection identified the temporary chooser test calling `installed_relay_service_active` without a fake `systemctl`/`launchctl`. The working diff adds private manager adapters and asserts that the expected probe was contained. The plugin-build test now prints its captured output if recovery still fails.
- Native run `35994187900`: Darwin arm64 passed Go tests, vet, build, all gated inert-fixture lifecycle tests, and its required-check gate. Linux provenance, Go tests/vet/build, bubblewrap setup, private PID/network namespace probe, and the four process-group integration fixtures passed. The Linux child-job suite failed five tests: active-to-terminal reclassification, owned-child signal/reap, pruned-terminal refusal, terminal-record repeated transition, and terminal-record classification. The final required Linux gate correctly failed. Source review found several fixture polling expressions run `jobs -l` in command substitutions even though production deliberately captures that builtin in the owning shell via a file. The working diff changes those fixture probes to redirection plus `read`, models the active-to-terminal reclassification with a marker, and adds failure diagnostics. This is a source-based repair hypothesis pending another hosted run; in particular, the owned-child failure cause is not yet established.

Local verification of this follow-up is deliberately static/compile-only:

```text
GOTOOLCHAIN=local go test -count=1 -run '^$' ./internal/processsupervisor  PASS (test package compiled; no tests executed)
bash -n tests/test_common.sh && bash -n tests/test_plugin_build.sh && bash -n relay/common.sh PASS
python3 -c 'from pathlib import Path; compile(Path("tests/test_tailscale_child_jobs.py").read_text(), "tests/test_tailscale_child_jobs.py", "exec")' PASS
gofmt -d internal/processsupervisor/supervisor_test.go; git diff --check PASS
```

No `make shell-check`, individual service/lifecycle suite, Tailscale/host service action, or native fixture was repeated locally. The standalone source verifier remains unrun. The two fixture repairs were committed and ordinarily pushed as `1908e58ab9d642ed6e42bff8444afc1d63e6daa1` after exact diff/path review and origin/ref verification. Check run `36003722011` and native run `36003721320` were then completed for that exact commit; details and the remaining uncommitted race-fixture repair follow. `ServeRouteOwned=false` remains unchanged; F04 source review/owner decision and full project qualification remain open.

## Hosted reruns and race-fixture repair — 2026-09-24T13:18Z

Read-only inspection of hosted runs confirmed the two fixture diagnoses and the latest outcome:

- Check run `36003722011` passed exact checkout provenance, pinned actionlint/format/vet, normal `go test ./...`, `make shell-check production-path-audit`, frontend/mobile browser checks, complete release bundles, and all four native release smokes. The only failed job was `go test -race ./...`, where `internal/webrtclink.TestOutcomeCountersClassifyEachSessionOnce` observed `{Direct:1, Relayed:0}` instead of `{Direct:0, Relayed:1}`.
- Native run `36003721320` passed all Darwin arm64 and Linux amd64 gates: native Go tests/vet/build, Darwin inert lifecycle fixtures, Linux PID/network namespace proof, setpriv fixture identity, process-group and child-job fixtures, and final required-check gates. These are disposable hosted synthetic-fixture results, not a real-tailnet qualification.
- Earlier run `36002192345` confirmed the plugin-build rollback issue remained while its normal Go quality and full race passed; run `36002191275` passed Darwin and failed Linux at `test_pruned_terminal_jobspec_remains_a_refusal`. The fixture had exposed a synthetic `Done` `jobs -l` row to `child_job_active` before attempting stop; that helper consumed/reaped the record, so the test no longer modeled a pruned Bash job. The latest fixture first emits the row, then lets the synthetic jobs builtin prune it; Linux qualification passed in `36003721320`.
- Rollback diagnosis: `relay/common.sh` uses the verified installed `relay_binary` for release-health JSON parsing. The test's fake current/new relay implemented `verify-release` and `activate-release`, but not the new `json-field` subcommand. Thus both wrong replacement health and valid restored-old health parsed as empty, making an otherwise successful restore appear to fail. The fake now supports only the four controlled flat string fields used by this health fixture. The hosted shell/production-path suite passed in `36003722011`; no production rollback behavior was changed.

The full race failure is isolated to a nondeterministic test setup: `h.negotiate` completes loopback WebRTC signaling while ICE pair-selection callbacks can race with `CloseSession`; under hosted race load the first session had already been nominated. `TestOutcomeCountersClassifyEachSessionOnce` now constructs synthetic sessions directly, giving the counter test deterministic nominated/unnominated states without opening loopback ICE or sockets. Focused verification passed:

```text
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off GOFLAGS=-mod=readonly \
  go test -race -count=1 ./internal/webrtclink -run '^TestOutcomeCountersClassifyEachSessionOnce$'
# PASS; synthetic session-counter test only

gofmt -d internal/webrtclink/manager_test.go
bash -n tests/test_plugin_build.sh
# PASS; gofmt reported no diff, shell syntax passed
```

`git diff --check`, the fixture's generated-shell `bash -n` probes, the fixture sed extraction probe, and `python3 -m json.tool docs/plans/tailscale-macos/execution/STATUS.json` also pass. No dedicated lifecycle test or lifecycle-containing shell suite was run locally. The standalone source verifier remains unrun and is not claimed as passed. At the 13:18 checkpoint only `internal/webrtclink/manager_test.go` was uncommitted; the pre-existing untracked `docs/plans/` tree remains preserved. `ServeRouteOwned=false` remains fail-closed; independent JSON accessor/source review and the F04 owner decision remain pending.

## Final hosted qualification update — 2026-09-24T13:45Z

The deterministic outcome-counter test was committed as `a034f1eebd90ddde9e38ad8644a0338b2a448f4f` and ordinarily pushed after rechecking branch, exact changed path, staged diff, origin URLs, and the previous remote ref. `git ls-remote` confirmed the candidate branch now points to that commit.

- Check run `36006375810` passed: exact checkout provenance; pinned actionlint, formatting, vet; `go test ./...`; `go test -race ./...`; shell/production paths; frontend/mobile browser checks; release bundles; Linux journal acceptance; and four native release smoke targets. Conditional mobile-device jobs skipped as configured and are not claimed as exercised.
- Native run `36006375121` passed Darwin arm64 and Linux amd64 preflight, including the gated Darwin inert lifecycle fixtures and Linux namespace/process-group/child-job checks with the required-check gates.
- Local verification of the repair was the focused `internal/webrtclink.TestOutcomeCountersClassifyEachSessionOnce` race test plus gofmt/static/JSON/whitespace checks. No dedicated lifecycle fixture or lifecycle-containing shell suite ran locally.

The execution is not marked fully ready: many later feature slices remain `not_started`; `imported/verify-source.py` was not run; the JSON accessor and DR-1 decision record still need independent review; F04 owner decision remains unmade; and real-tailnet qualification is out of scope. Production ownership remains fail-closed.

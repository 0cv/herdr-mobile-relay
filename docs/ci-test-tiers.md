# CI test tiers

The native-install suite (`tests/test_native_install.sh`) is a matrix of 2331 semantic cells: 1864 installer
invocations, 8 cancellation cells and 459 parser-probe, control and parity cells. Run in one process it does not fit
its 900 s bound, so CI splits it by cell id into shards and runs a subset on every push. The uninstall CLI suite
(`tests/test_service_uninstall_cli.py`: 1032 cells holding 1657 `Fixture.run` invocations, plus 3 controls) is sliced the same
way, with unchanged 900 s suite and 40 s invocation bounds. **A fast pass is never full qualification.**

## Workflows and events

| Workflow (required check) | Events | Sharded suites' cells | Shards per OS (macos-15, ubuntu-24.04) |
| --- | --- | --- | --- |
| `service-qualification.yml` (**Service qualification gate (fast)**) | push to `main`, `dev`, the CI branch | native: the explicit fast subset, 592 cells (353 installer runs + 8 cancellations); uninstall CLI: the explicit fast subset, 345 cells (550 invocations + 3 controls) | native 5, uninstall CLI 1 |
| `service-full-qualification.yml` (**Full service qualification gate**) | `pull_request` (before merge), `workflow_dispatch` for an exact candidate (optional `candidate_sha` must equal the run's commit), nightly cron on the default branch, push to the CI branch **only with `[full-qualification]` in the head commit message** (below) | native: all 2331 cells; uninstall CLI: all 1032 cells (1657 invocations + 3 controls) | native 28, uninstall CLI 3 |
| `check.yml` (Shell job) | push to `main`, `dev`, the CI branch; `pull_request` | none: runs `make shell-check-ci-complement` | - |
| `release.yml` `verify` | tag `v*` | requires `check.yml` **and** a successful *Full* run of the tagged commit (`workflow_dispatch` or nightly; a pull-request run tested a merge commit, not these bytes, and a push run is never accepted); none found fails closed; the existing 1800 s window | - |
| `resume-benchmark.yml` | `workflow_dispatch` only (was a per-push `check.yml` job) | - | - |

Both service workflows also run the ten unsharded suites on both runners: common, credentials, **install** (the
hosted macOS run of `install.sh`'s Darwin-specific `stat -f` branch), supervision, dispatch,
**doctor**, **setup-menu**, full uninstall, Tailscale CLI setup failure and release scripts. Dispatch,
doctor and setup-menu used to be nested inside the native suite, so one doctor failure hid every native cell; they are now
required runs with their own exit status and bounds (900 s, 120 s, 180 s). The uninstall CLI used to be the slowest of the
sequential suites job (its macOS run hit the 900 s bound); it is now its own `uninstall_cli` shard job in both workflows.
`make shell-check` is unchanged and complete (no environment variable selects everything: it still nests them and runs the
whole uninstall CLI suite, unsharded); `make shell-check-ci-complement` is it minus the native, full-uninstall and
uninstall-CLI suites, which the service workflows cover on every event `check.yml` runs on.
`common`, `credentials`, `install` and `release` are intentionally still run by both `check.yml` and the service workflows.

Branch rules must name the two gate jobs above (and `check`); that hosting configuration is a separate administrative
prerequisite that no YAML here establishes.

### Running the full tier on the CI branch

Pull requests, `workflow_dispatch` and the nightly cron always run the full tier and ignore the marker. A push to the CI branch
(`ci/tailscale-native-preflight-ef9f843`, the only branch the `push` trigger names) runs it **only when the head commit message of
the push contains the exact, case-sensitive text `[full-qualification]`**: put it in that commit's message, push, and the run
qualifies exactly that commit (`github.sha`), with the same SHA/run/attempt-bound evidence and exact-once gate as a dispatch run.
Earlier commits of the same push are not read. The `decide` job (Ubuntu, seconds, no checkout) computes this from the event and
gates every other job, the gate included. Without the marker `decide` passes, every other job is **skipped** and the run
shows no failure, yet no cell ran: **A skipped push run is not qualification**, and hosting rules may display its skipped gate
as passing. Only an explicit "no marker" decision skips the gate: a failed or cancelled `decide` still runs it, and it refuses.
`release.yml` accepts only `workflow_dispatch` and `schedule` runs, so even a marked push run is not release qualification: after
the merge, dispatch the Full run (or take the nightly one) for the tagged commit.

## Cells, the fast subset and shards

A cell id is the suite's fixture name (for example `capture-exit-launchd-gateway-legacy-label-True-ready-1-oversized-if`).
`tests/service_qualification_matrix.py` enumerates them by statically interpreting the suite's loops (it never runs the
suite); the suite gates each cell with `cell(id)` and fails if the ids it reaches differ from that enumeration. The
enumeration is frozen (`FROZEN`: totals, the id digest and a digest of every cell's own run/probe count) against the
committed pre-change source; a changed inventory, or a run moved between cells, fails until the constant is deliberately
reconciled. `tests/native_install_fast_cases.json` lists the fast ids explicitly with the seam each group
keeps. Kept in full: every parser seam (systemd; launchd domain, label, disabled) x label x malformed shape x caller
context, every shape at begin and in both rollback phases on every seam, every phase and caller context per seam and
label, both transports and both prior states in every phase, every origin, policy, fault-point x prior-state, success and
cancellation cell. Given up (full only): the second transport of probes, positive controls, policy and fault-matrix cells
(transport is not read there), and cross-combinations of shape x phase x caller context x prior state x transport. Fast has
361 installer executions against the audit's ~190 hypothesis: the coverage above needs more, and nothing was cut to hit 190.

Shards are assigned by `assign()`: longest-processing-time balancing over the modeled hosted-macOS cost of each cell (below),
the same assignment on both OSes, so every shard mixes systemd and launchd cells and each OS covers both inventories across
its aggregate. The shard counts are fixed (`SHARD_COUNTS`): native 5 fast and 28 full; changing one is a deliberate edit of the
helper, both workflows and this page.

### Uninstall CLI cells

An uninstall CLI cell is a fixture group of one or more `Fixture.run` invocations (1 to 7), named by its parameters, never by
a counter: `<manager>/<family>/<parameters>` with the manager the fixture model (`systemd` or `launchd`), for example
`launchd/capture/1/label-absent/oversize/observe-or`, or `control/<name>`. The suite opens every group with `cell(id)`;
`main()` refuses an invalid `HERDR_UNINSTALL_CLI_TIER`, `_SHARD` or `_CELL` before any control or fixture (a variable that is set
but empty is invalid too: unset it), and with none of them set it selects everything. `tests/service_qualification_matrix.py` enumerates the ids from the suite's text and freezes them
(`CLI_FROZEN`: 1032 cells, 1657 invocations, the digest of the ids, of every cell's id and run count and, in order, of every
invocation's call, Fixture and loop variables). At run time the ids
the suite reaches and each cell's invocation count must equal that enumeration. The three controls (timeout diagnostics,
fast-stub/model-stub equivalence, metadata batch) are cells pinned to **shard 1 of the tier, on each runner, and run nowhere
else**: the gate recomputes each shard's expected cells, so a control in another shard, twice, or in none fails it.

`tests/uninstall_cli_fast_cases.json` lists the fast cells with the seam each group keeps: all 39 lifecycle fault kinds
(both labels where the assertion is direction-specific), every capture seam x payload shape x caller context x label at least
once (40 cells), every observation-time branch, every positive state and every refusal class on both manager models, every
hidden unit directive, overlay location, effective-metadata property and query failure, every identity outcome, route choice and
signal. Fast is 345 cells and 550 invocations of 1657 (the audit's ~200 counted cases loosely: each atomic cell here runs 1 to 7
entrypoints, and every refusal class is kept on both models). Given up (full only): the cross-product of payload shape x caller
x label, fault kind x label (non-progress kinds), caller x observation count, serializer x label x directive, location x label x
survivor, property x label x position, and the second value of each protected environment key per model.

The uninstall CLI shards: 1 per OS in the fast tier, 3 in the full tier (modeled 304 s each, 34% of the 900 s bound).

## Exact-once evidence and the gate

The suite appends a cell id to its record only after that cell's assertions passed. The driver turns the record into
`native-shard-evidence.json` bound to the candidate SHA, run, tier, runner OS, shard and inventory digest; it is `pass` only
for exit 0 and exactly the shard's cells. The `gate` job runs with `always()` (unless `decide` explicitly skipped the run), requires every dependency job, `decide` included, to be
`success`, downloads the artifacts and recomputes the expected inventory itself. It fails on a missing, empty, duplicate,
unexpected, stale (SHA, run, or any attempt but the current one), cross-tier, cross-OS, cross-shard, malformed, partial, failed, timed-out or extra
artifact, and on any OS whose shards do not cover the tier exactly once with both managers. Evidence and summaries must carry
the gate's own run attempt, so after a partial re-run use "Re-run all jobs": the gate refuses artifacts kept from an earlier
attempt. `tests/test_service_qualification_matrix.py` demonstrates each case offline.

## Timing: measured versus modeled

Uninstall CLI: measured locally (Task G, complete run of the unchanged suite): 602.6 s of process-tree CPU over 1657
invocations; **modeled** hosted macOS cost = 1.38 hosted seconds per local CPU second (Task G calibration over 1062 hosted
invocations): about 913 s for the whole suite, 304 s per full shard (3 shards) and 359 s for the one fast shard including the
controls (45 s). No hosted run of any uninstall CLI shard exists; a shard over its unchanged 900 s bound fails closed.

### Native install: measured, derived and modeled

Source: the six hosted fast-tier jobs of each runner OS (three runs, commits `1bb8723f`, `0598331f`, `6304e405`, two shards
each; suite and fast-case file unchanged across them), whose `native installation fixture invocation N (platform) took Xs`
lines time every one of the 353 installer runs (81 to 87% of the suite seconds). Each line was mapped to its cell (the shard's cell order and run
counts; platform sequence and per-invocation stub-spawn tally agree in all twelve jobs) and the statistic is deliberately
conservative. The full calibration is `task-l-evidence/calibration.json` (tool `task-l-measure/calibrate.py`).

| What | Basis | Value used |
| --- | --- | --- |
| Seconds per timed invocation, per class and manager model (`CLASS_SECONDS`, macos-15) | **measured**: the largest per-job class mean of the six jobs, rounded up | e.g. capture-ready 6.0 / 7.89 s, capture-public 7.87 / 8.27 s, ready 8.99 / 11.12 s (systemd / launchd) |
| Per-shard overhead (`SHARD_OVERHEAD`) | **measured**: the largest suite seconds minus the invocation sum (66 to 127 s over the six macOS jobs) | 127 s on every shard |
| Probe, validation, cancellation and parity cells (`MODELED_SECONDS`) | **modeled**: the older local Ubuntu-equivalent estimates times `MACOS_FACTOR` (measured macOS seconds over those estimates for the timed cells) | factor 2.11 |
| Projected shard seconds, every count | **modeled**: balanced cell cost plus overhead; **never a measured hosted time** | below |

The overhead already contains the real cost of the modeled classes, which are priced again per cell: the projection is deliberately
generous (counting them once would make the fast tier 4 shards, not 5), because the six jobs cannot split the overhead into a fixed
and a per-cell part. The model back-tests above every hosted observation of the layout it was measured on (projected 978 / 972 s
against 843 / 723 s observed on macOS for two fast shards). Hosted run-to-run variance was up to 1.44x on the same shard (macOS
501 to 723 s).

The counts are the smallest for which every modeled macOS shard stays within **60% of the unchanged 900 s suite bound (540 s)**;
the suite bound, the 40 s invocation bound and the 180 s helper-build bound are not raised, no cell is dropped or retried:

| Tier (cells) | Shards per OS | Modeled macOS shard (max) | Margin to 540 s / 900 s | Modeled Ubuntu shard (max) |
| --- | ---: | ---: | ---: | ---: |
| fast (592) | 5 | 466 s | 74 s / 434 s | 270 s |
| full (2331) | 28 | 539 s | 1 s / 361 s | 315 s |

`python3 -I -B tests/service_qualification_matrix.py list --tier full` prints the modeled macOS seconds of every shard (`modeled_shard_seconds`). The
full tier's capture-ready and capture-public cells (1152 of 2331, 78% of its modeled macOS work) are priced from the 24 of each in the
fast subset, whose transport mix (two cloudflare cells per gateway cell) is slower than the full tier's even mix: that errs high
(pricing each transport at its own maximum would give 27 shards). **No full-tier native shard has run on a hosted runner**, and the
projections are not measurements: a shard over its unchanged 900 s bound fails closed (no retry, no pass-on-timeout) and the
remedy is a larger fixed count. The suite seconds exclude the job's checkout and toolchain setup.

### Cost and queueing

Jobs per event (each also runs on Ubuntu; the gate job is Ubuntu only): **a push runs 7 macOS jobs** (suites, 5 native shards, 1 uninstall
CLI shard; it ran 4 with 2 native shards) and **a full run 32** (suites, 28 native shards, 3 uninstall CLI shards; it ran 12 with 8).
GitHub caps how many macOS jobs a plan runs at once on standard hosted runners (its documentation lists 5 for the Free, Pro and Team
plans and 50 for Enterprise Cloud; this repository's plan was not inspected, so verify it): with 5, a full run needs at least seven
successive groups of macOS jobs and a push at least two, and pushes, pull requests and the nightly run queue behind each other. That
is a constraint, not a prediction: no wall-clock, queue or 15-minute figure is promised. `release.yml` (unchanged) waits at most 1800 s
for a still-running Full run, so dispatch it and let it finish before tagging.

## Hosted macOS Go job: real descriptor ACL evidence

`check.yml` job `go-darwin` (**Go Darwin ACL, doctor and support (macos-15)**: after `provenance`, read-only token, no secrets, `setup-go` caching off so that no cache is saved to or restored from the repository's Actions cache, no artifact; only the job log outlives the runner) runs
`go test -json -count=1 ./internal/darwinacl ./internal/doctor ./internal/support` on a GitHub-hosted macos-15 runner, then a **separate filtered**
`go test -json -count=1 ./cmd/herdr-mobile-relay -run '^TestSupportCommandProjectsLoadedPrivateState$'` appending to the same JSON log.
This is not a full cmd-suite execution. The Ubuntu `go` and `race` jobs never execute the real macOS ACL cells (they skip or return early off Darwin or off a hosted runner). The job
passes only if **both commands exit 0**, all four packages end `pass`, and each of six required tests ends `pass` **in its own package**
with no skipped or failed subtest: `internal/darwinacl` `TestRealPlainObjectsHaveNoACLOnAnAllowedVolume`; `internal/doctor`
`TestDarwinHostedRealACLGrant`, `TestDarwinHostedRealDirectoryACL` and `TestInstalledBundleDarwinACLWriterSafetyRemainsUnknownBeforeReads`;
`internal/support` `TestLoadDarwinHostedRealACL`; `cmd/herdr-mobile-relay` `TestSupportCommandProjectsLoadedPrivateState`.
For any of the six, **a skip, a missing test or a pass under another package is a failure**, never a pass (a skip of any other test in the three full packages is not examined: their `go test` still exits 0).
A failure prints package and test names (and the last five stderr lines of `go` itself when it fails), never the log. Only a passing
hosted run of this job qualifies the real-ACL cells and filtered CLI support cell on a real macOS host: until one has passed, they are unqualified.

## Uninstall mount confinement

On Linux, `relay/uninstall.sh` runs the relay's own mount guard (`internal/uninstallmount`, command
`__private-uninstall-mount-roots`) before any route, native or shared effect, and again for each directory root immediately
before its `chmod` (releases) and `rm -rf`. A root with a mount at, beneath or aliasing it, or any inventory the guard cannot
prove, refuses with no effects. All three selected roots are preflighted (a directory is proved, an absent root or a final symlink is not traversed). The guard
binary is a private, byte-compared copy of the installed relay, supplied only by a real (non-symlink) releases root, staged outside
every selected root and removed when the script exits (a copy that cannot be removed fails the run) (the releases root is deleted before the config and cache rechecks). Nothing is written before the installed relay, run in place, has proved all three roots, so a `TMPDIR` that is a bind-mount alias of a root is refused before the copy is staged. Darwin has no confinement proof: its path is
unchanged, never calls the guard, and stays an open obligation.

| Proof | Where it runs | What it shows |
| --- | --- | --- |
| Guard unit tests (synthetic inventories; absent, symlink and single-root kinds on the real filesystem) | `go test ./...` and the race job of `check.yml` (Linux only) | the autofs scope, parser strictness, per-root recheck |
| `tests/test_uninstall.sh` (both Service suites jobs, now given `NATIVE_HELPER`) | Linux host: the real compiled relay is the guard and refusals use a real FIFO; macOS host: a recording **model** guard checks call order and arguments | wiring and ordering, no effects on refusal; Darwin never calls the guard |
| `uninstall-mount-controls.yml` (ubuntu-24.04: push to the CI branch, `pull_request`, `workflow_dispatch`) | `scripts/ci/run-uninstall-mount-controls.sh` (sudo only for `mount --bind` and `umount` inside a private work directory, always unmounted) running `tests/test_uninstall_mounts.py` | real bind mounts at, beneath and aliasing each root (and as the temp directory) refuse before any effect or write; a mount after the native step is refused by the per-step recheck; absent and link-only roots; a sanitized census of mount filesystem types |

The full-uninstall suite gains about ten invocations and one binary copy per Linux-model invocation; its 900 s bound is unchanged. The
mount controls are a separate workflow, so the service gates, shard counts and their matrix are unchanged.

## Local use

Only the pure helper runs locally: `python3 -I -B tests/service_qualification_matrix.py list --tier fast`
(`--suite uninstall-cli` for the uninstall CLI) and `python3 -I -B tests/test_service_qualification_matrix.py`. The suites
themselves refuse outside disposable hosted CI.

## Native manager lifecycle: partial Q1, not M3

`.github/workflows/native-manager-lifecycle.yml` runs the installed release under the real launchd (macos-15) and systemd-user
(ubuntu-24.04) managers on disposable hosted runners only. `scripts/ci/run-native-lifecycle.sh` stops casual and accidental runs elsewhere: besides the Actions variables it requires a
process named `Runner.Worker` among its ancestors, read with the absolute `/bin/ps` so a `PATH` or function shim cannot answer for it. That is **not a security boundary**: a same-user caller
that forges such a process (or runs code before the script starts) passes it, and a caller with passwordless `sudo` can already change the host directly, so the driver must only be started by this
workflow on a hosted runner. It is **partial Q1,
not M3**: the probe, A install/start/stop/login-style relaunch/full uninstall and the refusals. Update, rollback and the signal matrix (Q2)
are not run, so nothing here claims the M3 lifecycle. A push to the CI branch runs it only with `[native-lifecycle]` in the head commit
message (exact, case-sensitive); a push without it runs `decide` alone and is not qualification. `workflow_dispatch` qualifies its commit
(`candidate_sha`, when given, must equal it). Jobs and caps: decide 5 min, probe 10 (both OSes), producer 30, lifecycle 45 (both OSes), gate
10; no retries, no fail-fast, one-day artifacts, a read-only token, and two macOS jobs per run. No hosted run exists yet: the first one measures. Branch rules and release admission stay separate prerequisites that no YAML here establishes.

| Part | What it does |
| --- | --- |
| probe | first real-manager contact, per OS; publishes `probe.json` (`native-capability-v1`, `result=pass\|fail`) for every outcome once its private staging exists (the hosted, path and checkout guards run first: a guard refusal writes nothing, and the upload step's `if-no-files-found: error` plus the gate's missing-artifact refusal make that red), and a failed probe stays red. It records the manager, the dedicated test user and its `default.target` (Linux), linger changes, curl's TLS backend and the CA-less refusal then trusted acceptance |
| producer | `make cross-build` then the unchanged `scripts/package-release.sh` at the checked-out commit; `producer.json` (`native-producer-v1`) binds the archive and manifest digests (B is null in Q1) |
| lifecycle | `a-install-start`, `a-stop-start-restart`, `login-style-relaunch`, `bad-token`, `bad-policy`, `public-wizard-premutation`, `gateway-missing-app`, `manager-missing-injected` (simulated), `a-full-uninstall`, `bystander-nonmutation`; plus `foreign-launchd-path` (macOS) or `foreign-systemd-fragment` and `foreign-systemd-dropin` (Linux) |
| HTTPS | a per-run `.test` name in one marked `/etc/hosts` line, a fresh CA and leaf trusted on the disposable runner, a TLS front on 127.0.0.1:443 that drops privileges before serving, and the fake cloudflared child of the real service wrapper forwarding the installed relay's real `/readyz`; a clean environment and a strict `curl` PATH shim; hits are counted on the front AND the child |
| Herdr | a nonroot Unix-socket API fixture through `HERDR_SOCKET_PATH` in `relay.env` (never `HERDR_BIN`), and an adapter that accepts only `plugin uninstall herdr-mobile-relay.events`; it proves no real Herdr registration |

The Linux product runs as a newly created disposable test UID, never the Actions user. `login-style-relaunch` stops only that user's manager
(`loginctl terminate-user`, else `systemctl stop user@UID.service`), starts it again with `systemctl start user@UID.service` and passes only if
the installed unit becomes active through `default.target` with no manual start on a new manager generation: a fresh user manager, not an
interactive login. macOS uses an owned-label `bootout` and `bootstrap` in `gui/$UID`. Cleanup is registered before each mutation; the stage is created with an atomic `mkdir` and removed only when that call proved this run created it. On an unknown
stop or identity the native definition is retained and the cleanup fails (the VM is destroyed), never force-deleted. A removal is accepted as proven only when its
absence check had a definite answer (an unreadable `/etc/hosts` or keychain listing, a failed `pgrep`, an unprobeable port 443 or a failed process listing is unknown, not absent).
The baseline refuses a real `herdr` on the service wrapper's search path and a manager other than the system one; a refusal case counts only with a known snapshot and a
product exit that is neither 0 nor a timeout (124, 125, 137).

On macOS the per-run CA is removed from the System keychain by its SHA-1 (`security delete-certificate -Z`) and its absence from the keychain listing is proven; `security remove-trusted-cert`
is not used, because it hangs as root on current runner images (actions/runner-images#12116). The admin trust-settings record of that destroyed per-run CA stays on the disposable VM, which is
harmless: the CA private key is deleted at creation, the certificate and the `/etc/hosts` line are gone, and the VM is destroyed.

The probe also records **diagnostic facts** so a failed run names its cause: for each real macOS manager query its status, size and a sanitized shape hint (`gui_print_status`, `gui_nul`, `gui_bytes`,
`gui_print_err` for `launchctl print gui/UID`; `disabled_status`, `disabled_bytes`, `disabled_admission`, `disabled_head`, `disabled_tail`, `disabled_err` for `launchctl print-disabled`, whose admission
reason is `pass`, `over-limit`, `nul`, `header`, `trailer` or `empty`, the product's own grammar: blank lines ignored, a `disabled services = {` section of `"label" => true|false|enabled|disabled` rows, further sections such as macOS 13+'s `login item associations` allowed; the helper and the product read it identically, and the product reads the domain listing, 89,044 bytes on macos-15, by status alone, so `gui_nul` and `gui_bytes` are evidence only), and the first failing baseline sub-check (`baseline_failed`: a `path-*` name, `manager-path-unread`,
`command-provenance`, `label-active`, `legacy-active`, `hosts` or `none`) with the observed `baseline_label_state` and `baseline_legacy_state`. On macOS the probe also asks ONE more read-only `launchctl print gui/UID/LABEL` just before the baseline and records the exact shape of its answer, never a byte of it, a label or a uid: `baseline_label_status` (its exit status, an integer) and, for each stream, `baseline_label_stdout` and `baseline_label_stderr`, one fixed word: `empty`, `bad-request` (exactly `Bad request.`), `not-found` (exactly the diagnostic for this label and uid), `bad-request+not-found` or `not-found+bad-request` (those two lines in that order), `framed` (a loaded answer: the header, a body and the closing `}`) or `other`; all three are `not-run` together where the helper kept no answer (the query could not run, timed out or was over the bound), and never otherwise: a launchctl that itself exits 126 or 127, or is ended by a signal (recorded as 128 plus the signal number, as the product's shell reads it), is recorded like any other status, with both streams named (`empty` for none), and the gate refuses a status beside `not-run` streams or `not-run` beside captured ones. Modern macOS answers a label that is not loaded with two lines, `Bad request.` then the diagnostic, status 113; the product (`native_install_observe`, `service_removal_observe`) reads a nonzero label query as absence only when its merged `2>&1` text, after one terminating LF, is exactly the diagnostic, `Bad request.` + LF + the diagnostic, or the diagnostic + LF + `Bad request.`, and the helper reads the same three texts when ONE stream carries them whole (it cannot know the order of two pipes, so a split answer is unknown there). A stage that was not reached is `not-run`; a hint keeps only
`[A-Za-z0-9 ={}"<>()._:/-]`, and of that only known launchd words, numbers of at most six digits and punctuation (every other token, a name, host, label or encoded text, becomes `<x>`, and so does a token that is or contains a name the producing host knows, its user, home directory and host names, even when it is a known word), with everything from the first path-like
token (a private home or temp path, which may hold blanks) to the end of the line replaced by one `<path>`, at most 64 characters cut between tokens (`none` for nothing); each query's stderr is captured through a cap of 4096 bytes
(the rest is drained, never written) and every read of a captured answer is bounded. These facts are **evidence, not admission**: the gate validates each strictly (an enum,
an integer range or the sanitized pattern, a hint holding a token that is no known word, number or punctuation, a path-like token or text after a `<path>` included; a missing, extra or malformed key is refused), but the pass rule and the failure phase never read them, so they cannot turn a failed probe into a pass. The gate also refuses a *passing* receipt whose diagnostics record a failed stage, because the driver fails closed on each: a baseline sub-check other than `none` or a baseline state other than `absent`; on macOS a `gui_print_status` or `disabled_status` other than 0, an admission other than `pass` or a `disabled_bytes` over 65536 bytes (a NUL in the domain listing fails nothing: the product discards that listing), and on Linux any recorded gui stage or label shape (a Linux probe asks no label query; the label shape is evidence on macOS and a passing macOS probe may record any coherent one).

Every case records before and after definition hash, mode, origin (`path =` or `FragmentPath` plus `DropInPaths`), state, release revision,
owned process digest and manager generation, the readiness identity, the Herdr socket counts and matched HTTPS hits of THAT case (deltas against the counters at its start, never cumulative values), `default.target` observed active after the Linux login-style restart, and a bystander digest. The
`gate` job (`tests/native_manager_lifecycle_matrix.py`, partial Q1) requires every dependency to succeed and exactly one probe per OS, one
producer and one `native-cases-v1` per OS, bound to the commit, run and attempt; it downloads the release bundle itself and re-hashes it against the producer receipt; it refuses a missing, empty, malformed, duplicate, extra,
failed, stale or cross-OS receipt, a failed probe receipt and any cleanup failure. It does not prove physical login/logout, external
Cloudflare, phone reachability, gateway success or real Herdr registration. Offline: `python3 -I -B tests/test_native_manager_lifecycle_matrix.py`.

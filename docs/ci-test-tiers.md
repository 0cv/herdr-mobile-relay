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

Both service workflows also run the nine unsharded suites on both runners: common, credentials, supervision, dispatch,
**doctor**, **setup-menu**, full uninstall, Tailscale CLI setup failure and release scripts. Dispatch,
doctor and setup-menu used to be nested inside the native suite, so one doctor failure hid every native cell; they are now
required runs with their own exit status and bounds (900 s, 120 s, 180 s). The uninstall CLI used to be the slowest of the
sequential suites job (its macOS run hit the 900 s bound); it is now its own `uninstall_cli` shard job in both workflows.
`make shell-check` is unchanged and complete (no environment variable selects everything: it still nests them and runs the
whole uninstall CLI suite, unsharded); `make shell-check-ci-complement` is it minus the native, full-uninstall and
uninstall-CLI suites, which the service workflows cover on every event `check.yml` runs on.
`common`, `credentials` and `release` are intentionally still run by both `check.yml` and the service workflows.

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

# CI test tiers

The native-install suite (`tests/test_native_install.sh`) is a matrix of 2331 semantic cells: 1864 installer
invocations, 8 cancellation cells and 459 parser-probe, control and parity cells. Run in one process it does not fit
its 900 s bound, so CI splits it by cell id into shards and runs a subset on every push. **A fast pass is never full
qualification.**

## Workflows and events

| Workflow (required check) | Events | Native cells | Shards per OS (macos-15, ubuntu-24.04) |
| --- | --- | --- | --- |
| `service-qualification.yml` (**Service qualification gate (fast)**) | push to `main`, `dev`, the CI branch | the explicit fast subset, 592 cells (353 installer runs + 8 cancellations) | 2 |
| `service-full-qualification.yml` (**Full service qualification gate**) | `pull_request` (before merge), `workflow_dispatch` for an exact candidate (optional `candidate_sha` must equal the run's commit), nightly cron on the default branch | all 2331 cells | 8 |
| `check.yml` (Shell job) | push to `main`, `dev`, the CI branch; `pull_request` | none: runs `make shell-check-ci-complement` | - |
| `release.yml` `verify` | tag `v*` | requires `check.yml` **and** a successful *Full* run of the tagged commit (`workflow_dispatch` or nightly; a pull-request run tested a merge commit, not these bytes); none found fails closed; the existing 1800 s window | - |
| `resume-benchmark.yml` | `workflow_dispatch` only (was a per-push `check.yml` job) | - | - |

Both service workflows also run the ten non-native suites on both runners: common, credentials, supervision, dispatch,
**doctor**, **setup-menu**, uninstall CLI, full uninstall, Tailscale CLI setup failure and release scripts. Dispatch,
doctor and setup-menu used to be nested inside the native suite, so one doctor failure hid every native cell; they are now
required runs with their own exit status and bounds (900 s, 120 s, 180 s). The uninstall CLI stays full in both tiers.
`make shell-check` is unchanged and complete (it still nests them); `make shell-check-ci-complement` is it minus the
native, full-uninstall and uninstall-CLI suites, which the service workflows cover on every event `check.yml` runs on.
`common`, `credentials` and `release` are intentionally still run by both `check.yml` and the service workflows.

Branch rules must name the two gate jobs above (and `check`); that hosting configuration is a separate administrative
prerequisite that no YAML here establishes.

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

Shards are assigned by `assign()`: longest-processing-time balancing over modeled per-class cost (hosted class medians,
launchd multipliers), identical on both OSes, so every shard mixes systemd and launchd cells and each OS covers both
inventories across its aggregate.

## Exact-once evidence and the gate

The suite appends a cell id to its record only after that cell's assertions passed. The driver turns the record into
`native-shard-evidence.json` bound to the candidate SHA, run, tier, runner OS, shard and inventory digest; it is `pass` only
for exit 0 and exactly the shard's cells. The `gate` job runs with `always()`, requires every dependency job to be
`success`, downloads the artifacts and recomputes the expected inventory itself. It fails on a missing, empty, duplicate,
unexpected, stale (SHA, run, or any attempt but the current one), cross-tier, cross-OS, cross-shard, malformed, partial, failed, timed-out or extra
artifact, and on any OS whose shards do not cover the tier exactly once with both managers. Evidence and summaries must carry
the gate's own run attempt, so after a partial re-run use "Re-run all jobs": the gate refuses artifacts kept from an earlier
attempt. `tests/test_service_qualification_matrix.py` demonstrates each case offline.

## Timing: measured versus modeled

Measured (hosted Ubuntu, before the shim change): 251 of 1864 invocations in 900 s; per-class medians in the audit.
No macOS native cell has ever completed on a hosted run. Everything else is **modeled**: the class costs and launchd
multipliers in `CLASS_SECONDS`, about 4949 s of Ubuntu-equivalent work in total, so about 619 s per full shard and 402 s
per fast shard plus build and import overhead. macOS may be slower; a full shard that exceeds the unchanged 900 s
bound fails closed (no retry, no pass-on-timeout) and the remedy is a larger fixed shard count, a deliberate edit. No
total wall-clock, queue or 15-minute figure is promised. Hosted measurements are required before trusting these numbers.

## Local use

Only the pure helper runs locally: `python3 -I -B tests/service_qualification_matrix.py list --tier fast` and
`python3 -I -B tests/test_service_qualification_matrix.py`. The suites themselves refuse outside disposable hosted CI.

# Review of the supplied Tailscale macOS plan

## Verdict

**The baseline diagnosis and security direction are substantially accurate. The plan should not be executed verbatim as a CI-only repair.** It needs the workflow/provenance, workspace, toolchain, scope and dependency corrections incorporated into [PLAN.md](PLAN.md).

This is a repository/source-informed plan review, not approval of the accumulated implementation. No orchestration was launched and no product files were changed. These documents and the preserved handoff are the only repository additions.

## Inputs and integrity

| Input | Verified identity |
|---|---|
| Received archive | `/Users/christophe.vidal/Downloads/tailscale-macos-plan.ylYprIYO.tar.gz` |
| Archive SHA-256 | `adcadb515cf46683955d412be984179061c50be0824a785a46464fe4a231545a` |
| Product HEAD at review | `b74a1c16c9c4a1b1207cad88fb00b33556e04025` |
| Current branch | `ci/tailscale-native-preflight-ef9f843` |
| Source digest, excluding diagnostic workflow | `ff8ff3deb6722cc29d95fce660e66d4af92491509a1cf483e84ab7231fcf072b` |
| Diagnostic workflow SHA-256 | `2547b80f197432eed5156203e73e94d40609ae30485fb3d6be56a91028523884` |
| Inventory | 826 source paths plus the workflow; source bytes, file kinds and modes matched before adding planning files |
| Accumulated product diff | 95 paths from `ef9f84351b5f257f1e8e0156057198fe8e644dd3` to the reviewed HEAD |
| Handoff structure | 14 feature areas / 62 named F-substeps |

Every entry in the received `SHA256SUMS` passed, including the copy preserved under `imported/`. Hashes establish consistency, not a signed provenance chain or implementation correctness.

The supplied verifier requires a standalone `.git` directory and rejects linked worktrees. This checkout has a `.git` file. For this review, source identity was independently checked read-only against the inventory without pretending the standalone-import requirement had been satisfied. No source import or execution environment was provisioned.

## Confirmed claims

1. **The red native preflight is real.** GitHub reports both runs [35950305948](https://github.com/0cv/herdr-mobile-relay/actions/runs/35950305948) and [35952772727](https://github.com/0cv/herdr-mobile-relay/actions/runs/35952772727) completed with failure at the stated commits. The diagnostic run's job data confirms successful provenance, Go setup, scoped Go tests/vet/build and a failing final gate. Its logs show four Darwin handshake failures, `Ran 4 tests in 180.273s`, and `FAILED (failures=4)`.
2. **The boolean accessor is not portable.** `relay/common.sh:1078` uses GNU BRE `\|`. An isolated extraction of that function, executed with `/bin/bash` and PATH restricted to `/usr/bin:/bin`, returned empty output for valid true fields on this native Mac. No launcher or service was involved.
3. **The unapproved patch is not a complete parser fix.** It recognizes the valid boolean but incorrectly returns true for nested-only fields, escaped duplicate keys and a different key containing whitespace. The exact controls are below.
4. **Real ownership is intentionally unavailable.** `internal/tailscale/tailscale.go:146` always assigns `ServeRouteOwned=false`; `relay/tailscale.sh` requires true before positive startup/monitoring. The imported F04 blocker is valid on both platforms, not just macOS.
5. **Upstream supports the need for an authority decision.** At the pinned commit, [`serve_v2.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_v2.go) creates a private watcher, obtains `n.SessionID`, inserts the foreground config, and waits on that watcher. The inspected flag/output path does not expose a parent-bound session contract. This does not prove every alternative design impossible; it does invalidate treating route observation as that contract.
6. **There are genuine portability gaps in existing test entrypoints.** `tests/test_tailscale.sh` uses `/proc`, `mapfile` and GNU `timeout`; the Darwin driver explicitly requires arm64; the proposed `make tailscale-*` targets are absent. The original plan correctly calls these future work.
7. **The broad feature backlog is justified by current structure.** The launcher still snapshots/restores env/origin through shell strings rather than an exact-byte journal; pre/post-consent checks do not compare the full authenticated identity; managed ownership exists but not every writer uses it. F05–F09 are not already proven by the existence of their helper packages.
8. **Tool versions in the plan match repository declarations.** Go 1.27.0, Bun 1.4.0 and Playwright 1.62.1 are declared in `go.mod`, workflows and `frontend/package.json`. The receiving host's currently selected Go is older, however.

## Required corrections

| ID | Priority | Finding | Disposition in reviewed plan |
|---|---|---|---|
| R1 | Blocking CI execution | The diagnostic provenance gate hard-codes the old source digest. Every product/test/planning edit changes it. The original runbook mentions triggers but does not explicitly migrate this gate. | CI-4 specifies revision-bound provenance and negative tests while retaining the immutable baseline. |
| R2 | Blocking execution setup | Current checkout is a linked worktree; the verifier explicitly requires a standalone clone. The plan's current host context and a blindly launched worker would conflict. | Require the original fresh-clone import before implementation; do not weaken verifier or silently change execution root. Account for clone modes and adding docs after import. |
| R3 | Blocking local builds | Selected local Go is 1.25.3, not required 1.27.0; no actionlint was found on PATH. | Explicit runner provisioning prerequisite, no install/download/downgrade performed during review. |
| R4 | High | The handoff is a full product-completion plan, not just a path to green CI. F13.2's dependency on package/browser work can postpone useful native CI unnecessarily. | Separate CI-ready and feature/PR-ready outcomes; early CI slices preserve the full later backlog. |
| R5 | High | Make prepends Homebrew/user-bin directories. Setting a clean parent PATH does not guarantee stock tools or fail-on-use adapters win in recipes. | Enforce/assert resolved tools and adapters inside Make/subprocesses, including recursion and environment hooks. |
| R6 | High | The unapproved regex patch fixes GNU/BSD syntax but accepts semantically wrong fields. Standard Go unmarshal alone would still leave duplicate/case pitfalls. | Whole-document bounded typed parsing and explicit decoded-key tests; no new end-user runtime dependency. |
| R7 | High | Reinspection is not atomic compare-and-mutate. Session authority alone does not establish safe concurrent config updates. | F04 ADR must preserve/qualify conditional mutation and inject concurrent foreign writes. |
| R8 | Medium | The normal workflow already contains a four-target native release-smoke matrix. Those jobs are not native Tailscale lifecycle qualification. | Reuse existing package jobs and artifacts; add missing lifecycle coverage without claiming smoke is full feature acceptance. |
| R9 | Medium | Some graph edges do not express the prose's actual acceptance gates; F11.1 omits F09.2, and the provisional F10 harness needs reconciliation with approved F04. | Explicit dependency corrections and all-required-step final validation; independent source work remains possible. |
| R10 | Medium | Literal “zero effects” conflicts with necessary admission helper/log activity, and historical orchestration aliases/scripts are not repository tools. | Define zero product/foreign effects and account for bounded helpers; leave orchestration configuration to the user's chosen runner. |

Additional clarifications:

- The existing diagnostic workflow **does** aggregate failures correctly using step `outcome`; do not “fix” it by removing the final gate. GitHub API step `conclusion` can say success for the failed lifecycle step because of `continue-on-error`. The failed test logs/final outcome are decisive.
- The normal `check.yml` push trigger covers main/dev, not this feature branch. Changing only the diagnostic trigger will not automatically run the full normal checks on a new work branch.
- `make mobile-ci-check` runs actionlint against three named workflows and omits `tailscale-native-preflight.yml`. Include the changed workflow explicitly in verification.
- Current stock `/usr/bin/sort -V` sorted `v8.0.0`, `v9.0.0`, `v10.0.0` correctly. The plan correctly calls the NVM issue a suspicion; retain that status rather than treating all BSD utilities as lacking GNU-like options.
- The pinned upstream [`client/local/serve.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/client/local/serve.go) copies ETag from GET and sends it as `If-Match` on POST. Qualifying actual daemon enforcement and failure behavior belongs to F04/F12; this review did not execute LocalAPI calls.
- The original `status.json` is historical initial state, not proof that its graph is a complete scheduler. Preserve it and initialize a new execution checkpoint rather than editing the checksum-covered import.

## Minimal native parser evidence

Only the extracted function body was executed, with a clean environment (`PATH=/usr/bin:/bin`, `LC_ALL=C`) and `/bin/bash --noprofile --norc`. The patch function was reconstructed in memory from its diff; **the patch was never applied to product source**.

For key `serve_inspected`:

| Function | JSON input | Observed stdout | Meaning |
|---|---|---|---|
| Baseline | `{"serve_inspected":true,"exposure_complete":true}` | empty | Reproduces the native BSD defect; status was still zero. |
| Reference patch | Same valid input | `true` | Narrow portability fix works. |
| Reference patch | `{"message":{"serve_inspected":true}}` | `true` | Wrong: no top-level field exists. |
| Reference patch | `{"serve_inspected":true,"\u0073erve_inspected":false}` | `true` | Wrong: decoded duplicate keys not rejected. |
| Reference patch | `{"serve_ inspected":true}` | `true` | Wrong: whitespace removal changes key identity. |
| Reference patch | A JSON string value containing escaped `"serve_inspected":true` | empty | This simple quoted-string control was rejected; do not claim it demonstrated the historical escaped-string exploit. |

These tests establish specific parser behavior, not that the full launcher passes after fixing it. The exact historical 70-byte startup logs/category remain handoff-reported; this review inspected CI job/test logs but did not download and independently verify that artifact's sanitized summary.

## What was not established

- No full review of every line of the accumulated 95-file implementation; relevant control paths, tests, build/workflow declarations and source contracts were inspected to validate the plan.
- No Go build/test/vet/race, broad shell suite, native lifecycle fixture, frontend build/browser run, release smoke, real Tailscale command or tailnet mutation was executed during this review. Go provisioning and disposable-runner prerequisites are still pending.
- Historical Linux passes, the restored 778-path comparison tree, old review approvals and the missing C2F2D dirty-tree history were not independently reconstructed. Preserve their historical/unverified labels and require the original explicit disposition.
- No production environment/device state was read or copied. No tools were installed; no services, trust stores, repository remotes or workflows were changed. GitHub queries were read-only.
- No commit, push, PR, release, workflow dispatch or orchestration launch occurred.

## Recheck the delivered planning files

From the repository root:

```bash
(cd docs/plans/tailscale-macos/imported && shasum -a 256 -c SHA256SUMS)
git diff --check
git status --short
```

Markdown local-link targets, the 62 F-step/table/status correspondence and the imported dependency graph were also checked structurally. Those checks are not product tests or semantic proof of dependencies. The reviewed plan's explicit corrections remain authoritative.

**Next action:** the user chooses the orchestration run using [PLAN.md](PLAN.md), starting with CI-0/F00 and runner provisioning—not by running `make check` on the personal Mac or applying the unapproved patch.

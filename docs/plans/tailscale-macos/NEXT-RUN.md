# Completed R0 assignment: review and close the CI/parser repair

**R0 was approved** at `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5` by workflow `3b823169-0686-44cd-97d5-6d842a8fe95e`, with two clean independent reviews. This assignment is retained as history; do not rerun it merely because of its old filename.

**Next bounded run: [R1-OWNERSHIP-DESIGN.md](R1-OWNERSHIP-DESIGN.md).** The original R0-only scope from [the roadmap](REMAINING-IMPLEMENTATION-PLAN.md) follows below.

## Objective

Independently review the already-green CI/parser changes, repair demonstrated in-scope defects if necessary, and return a precise scoped acceptance result. Do not implement the complete Tailscale feature in this run. In particular, do not stop this review merely because F04 ownership design or a real-tailnet environment remains pending.

## Inputs

- Repository: `0cv/herdr-mobile-relay`.
- Workspace: the existing linked worktree at `/Users/christophe.vidal/.herdr/worktrees/herdr-mobile-relay/ci-tailscale-native-preflight-ef9f843`.
- Authorized local/remote branch: `ci/tailscale-native-preflight-ef9f843` / `origin/ci/tailscale-native-preflight-ef9f843`.
- Review base: `b74a1c16c9c4a1b1207cad88fb00b33556e04025`.
- Reviewed candidate to start from: `a034f1eebd90ddde9e38ad8644a0338b2a448f4f`.
- Frozen candidate diff: `git diff b74a1c16c9c4a1b1207cad88fb00b33556e04025 a034f1eebd90ddde9e38ad8644a0338b2a448f4f` — 34 paths.
- Existing CI evidence: [check 36006375810](https://github.com/0cv/herdr-mobile-relay/actions/runs/36006375810) and [native 36006375121](https://github.com/0cv/herdr-mobile-relay/actions/runs/36006375121), both for that exact candidate.
- Detailed attempts and limitations: [execution/STATUS.json](execution/STATUS.json), [execution/WORKER-LOG.md](execution/WORKER-LOG.md), and the roadmap's settled operating rules.

Before editing, confirm canonical cwd, branch, HEAD, index and expanded dirty-file inventory. If HEAD changed, inspect and name the new delta; never reset or discard it to recreate these inputs. The existing untracked `docs/plans/` files are preserved user work. The old workflow `a3b35643-0f7a-45dd-87d0-3bd219a4c6b0` is terminal blocked and must not be retried automatically.

## Allowlist and exclusions

- Default repair allowlist: the **exact 34 paths** obtained from `git diff --name-only b74a1c16c9c4a1b1207cad88fb00b33556e04025 a034f1eebd90ddde9e38ad8644a0338b2a448f4f`, plus a new `docs/plans/tailscale-macos/execution/CI-REVIEW.md` report. Record the expanded list in the handoff before editing.
- Read relevant unchanged callers, helpers and original tests to evaluate interactions. If a fix needs another product/test file, explicitly revise the narrow allowlist with justification before editing; do not turn this into all-feature implementation.
- Keep imported plan/checksum/source inventory files unchanged. Do not rewrite historical execution evidence or baseline manifests to make them pass.
- No LocalAPI mutation implementation, ownership-flag bypass, real Tailscale commands, tailnet enrollment/administration, production-state access, host trust changes or service manipulation.
- No system-wide tool install, force-push, other-branch push, PR, release, deployment, or blanket staging of user files.

## Work sequence

### 1. Establish an honest review baseline

Inventory all 34 paths and classify them: parser/CLI/shell callers; workflow/provenance/toolchain; fixture changes; draft ADR. Bind the source snapshot to the CI evidence. The previous worker's successful commands and diff inspections are useful evidence, **not** independent source reviews.

Keep the prior local broad-shell execution scope deviation visible. Confirm future local verification cannot re-enter lifecycle/service fixtures through broad Make targets.

### 2. Review actual behavior, not just the report

**Correctness/security focus:**

- Whole-document JSON bounds, decoded duplicate/case conflicts, exact top-level types, string controls and optional telemetry behavior.
- `relay_binary`/`json-field` bootstrap recursion and physical helper selection in install/start/reprint/health call paths; old or missing helpers must fail safely and diagnostically.
- Permission-bearing consumers cannot accept partial/invalid input; unrelated legacy gateway/Cloudflare behavior is not accidentally broken by global accessor changes.
- `relay/tailscale.sh` changes preserve consent/readiness/cleanup invariants. Real session ownership remaining false is an explicit non-goal, not an authorization shortcut.
- Draft ADR distinguishes evidence, recommendation and pending owner approval; no scope decision is silently made by reviewing it.

**CI/test-validity focus:**

- Candidate branch triggers, Go 1.27.1 pins, pinned actionlint and all relevant workflow files.
- Exact checkout provenance: committed objects, paths/types/modes, dirty/untracked inputs, submodule/symlink handling and non-tautological negative controls.
- `continue-on-error`, matrix skip/cancellation, final outcome gates, artifact identities and bounded sanitized logs cannot produce false green.
- Linux namespace capability/privilege setup and macOS host assumptions are tested rather than faked; no unnecessary broad privilege change.
- Child-job pruning, competing signals, PID-file handshake, plugin rollback and WebRTC fixture repairs retain their original assertion obligations. Synthetic unit tests do not replace separate real lifecycle/integration obligations without explicit accounting.
- Disabled mobile-device jobs remain clearly excluded from claimed coverage; package smoke does not stand in for Tailscale lifecycle support.

### 3. Repair only substantiated in-scope findings

Reproduce with a focused safe test or disposable CI fixture, make the smallest correction, retain useful failure diagnostics, and rerun affected gates. Fixed-count stress is allowed when declared in advance; retry-until-green is not a repair. Keep unrelated pending feature work in the roadmap rather than expanding the run invisibly.

The worker should hand the scoped artifact to independent reviewers once it is ready to review. Do not keep implementing later roadmap milestones while waiting to make a globally complete feature.

### 4. Verify in the correct environment

Safe focused local baseline command (after effect audit; modules must already be available):

```bash
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off GOFLAGS=-mod=readonly \
  go test -race -count=1 ./internal/tailscale ./cmd/herdr-mobile-relay \
  -run '^(TestExtractJSONField|TestRunJSONField)'
```

Use explicit private test/build state when running tests, and record actual resolved Go 1.27.1 and stock shell/tool paths. No broad `make check`/`make shell-check` or lifecycle suite on the personal host as a shortcut. Audit other package tests individually before local execution; run the rest on disposable CI.

If product/workflow/test inputs change, run affected normal/race/static/frontend/package/native gates on the new candidate through ordinary task-only pushes to the authorized branch. Verify the destination and exact staged paths first. If inputs are unchanged, reviewers may use existing exact-revision CI results while clearly saying they did not personally rerun them. Missing old log detail is not proof of a test assertion; retrieve the needed evidence or run a bounded replacement on authorized CI.

### 5. Complete independent review before ending the slice

Use independent reviewers with the two focuses above. Reviewers inspect the actual final source and assertions—not only the worker's narrative. Findings must be dispositioned with code/test evidence. A clean review of a report containing unresolved code defects is **not** code acceptance.

Record any review cap or unresolved finding honestly. Neither zero outstanding findings from a never-started review nor the word “reviewed” in a worker's commit note substitutes for an independent verdict.

## Required output

Create the scoped `execution/CI-REVIEW.md` report with:

- Input/final commit and source digest, branch, initial/final dirty paths and protected user-file preservation.
- All reviewed paths and relevant interaction boundaries.
- Independent review identities/focuses/verdicts, findings and their dispositions.
- Exact test commands, CI run IDs, platform/architecture, skipped/excluded cases, failed attempts and later successful fixes.
- Separate `automatedChecksPassed`, `scopedSourceReviewClean` and `featureReady` values. **`featureReady` remains false** in this R0-only run, regardless of scoped approval.
- Explicit deferred items: full 112-path feature review, F04 authority decision/implementation, remaining writer/journal/readiness/browser/native-support gates, and real-tailnet qualification.

**Successful endpoint:** the CI/parser repair is independently reviewed and accepted for its stated scope, with required verification current. Stop and return that result. The next separately scoped task is R1's design qualification/decision—not an automatic continuation through the full roadmap.

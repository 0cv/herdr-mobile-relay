# Reviewed Tailscale macOS / CI execution plan

**Historical CI plan: initial execution reached `a034f1e`; R0 subsequently approved the scoped CI/parser repair at `d2e5d01`. The full Tailscale feature remains incomplete.**

## Read this first

**Current remaining-work roadmap: [REMAINING-IMPLEMENTATION-PLAN.md](REMAINING-IMPLEMENTATION-PLAN.md). Next bounded run: [R1-OWNERSHIP-DESIGN.md](R1-OWNERSHIP-DESIGN.md).** Those documents carry forward the user's approved current-worktree execution, Go 1.27.1 and authorized CI branch. Do not reopen the obsolete standalone-clone/Go 1.27.0 prerequisites below or run this entire historical backlog as a single review slice.

The original execution plan below is retained as history. It incorporates the supplied [62-substep plan](imported/PLAN.md), [acceptance matrix](imported/ACCEPTANCE-MATRIX.md), [verification contract](imported/VERIFICATION.md), and [slice template](imported/SLICE-TEMPLATE.md). Its unchanged feature obligations still apply, subject to the explicit scope amendments in the current roadmap; this is not permission to discard the larger backlog.

The exact received handoff is preserved under `imported/`, including its checksums, source inventory and **unapproved** patch. Do not edit that directory or apply the patch as an approved fix. Use [REVIEW.md](REVIEW.md) for findings and the limits of this review.

Reviewed product baseline:

- Repository: `0cv/herdr-mobile-relay`.
- Commit: `b74a1c16c9c4a1b1207cad88fb00b33556e04025`.
- Original pre-feature base: `ef9f84351b5f257f1e8e0156057198fe8e644dd3`.
- 826 source paths plus `.github/workflows/tailscale-native-preflight.yml`; all matched the handoff before these planning documents were added.
- Current receiving checkout is a **linked worktree**, not the standalone clone required by the import runbook.

## 1. Separate two outcomes

### A. Working CI — first delivery

An exact-commit, repeatable, honestly failing-or-passing CI path that covers portable contracts, native macOS/Linux lifecycle, and the project's existing quality/package checks. It must detect failures and omitted required cases, not simply turn the old diagnostic job green.

**The CI infrastructure and native fixture repair do not require a real tailnet or a new session-authority implementation.** Native lifecycle fixtures deliberately use synthetic inspection/health adapters. Passing them does not prove real pairing. Keep production ownership fail-closed while F04 is unresolved. If an existing required gate actually depends on positive real ownership, report that dependency as blocked and complete its feature prerequisite; do not weaken the gate or replace its real behavior with synthetic success merely to obtain `ciReady`.

### B. Working Tailscale feature — later completion

The original F04–F13 obligations: independently bound session authority, cooperating writers, transactions/recovery, exact release/readiness, credentials, real packaged-browser acceptance and separately authorized two-node tailnet qualification.

`ciReady` and `prReady` are separate. CI may be green while the feature remains unavailable; documentation must say so. `prReady` stays false until the original full acceptance matrix is satisfied or the owner explicitly approves a different scope. Do not hide a mandatory gate behind `continue-on-error`, an unreported skip, or a default-success readiness field.

## 2. Preconditions and authorization

1. **F00 import:** use the fresh standalone clone described in [HANDOFF.md](imported/HANDOFF.md). Do not run its verifier against this linked worktree and weaken it when it refuses. Verify baseline *before* adding planning documents/build outputs. Keep handoff/evidence outside the execution checkout during import. Use a private 0700 parent and an explicit clone umask of 022 so source modes match; fixture mode policy is separate.
2. Before every implementation/review slice, assert the canonical writable checkout, branch, HEAD, expected dirty paths and full input inventory. Never edit an installed relay, reference checkout or a second worktree accidentally. After import, inventory each new revision; the import verifier is intentionally only for the pinned baseline or scratch patch.
3. Explicitly provision tools in the disposable runner. This receiving host reports macOS 27.0 arm64, stock Bash 3.2.57, **Go 1.25.3**, Bun 1.4.0 and Python 3.14.7. The repository requires **Go 1.27.0**. Do not run builds with an accidental downloaded toolchain or downgrade `go.mod`; arrange Go 1.27.0 first. `actionlint` was not found on PATH. Recheck all tools in the actual runner rather than treating this host inventory as runner evidence.
4. Use a disposable native Mac runner/guest without production Herdr/Tailscale, developer credentials or mounted home for process/service-adjacent tests. HOME/XDG changes and `HERDR_S9B3_DISPOSABLE_DARWIN=1` do not create isolation. Use a real Linux VM/runner with the required namespace capabilities for Linux fixtures.
5. The user will select and launch orchestration. The imported `review.js`/Luna/Sol instructions are historical external tooling preferences, **not validated repository prerequisites**. Verify the user's available orchestration tooling separately; do not invent paths, launch an old run or repair external tooling as part of a product slice.
6. No real Tailscale, host trust changes, tool installation, commit/push, PR, deployment or release is authorized by this plan-review task. Future CI pushes need the user's execution authorization. F04 scope changes and F12 tailnet use retain their separate decision gates.

## 3. Ordered CI delivery slices

Use one narrow implementation/review handoff at a time. The CI labels below organize the original F-steps; they are not replacement implementations or a second test framework.

| Slice | Work and original coverage | Exit criteria |
|---|---|---|
| CI-0 — Baseline and runner | F00.1–F00.3; F01.1–F01.2; prerequisite inventory above. Build safe runner entrypoints only as needed. | Verified standalone baseline; exact writable root; tools provisioned; disposable Mac/Linux capabilities documented; effects inventory and fail-on-use adapters proven. |
| CI-1 — Reproduce and fix inspection parsing | F02.1–F02.4; relevant safe `test_common` fixtures. | Native BSD and Linux parser cases agree; complete inert inspection advances; incomplete/malformed inspection refuses before effects; no regex authorization shortcut. |
| CI-2 — Native lifecycle | F03.1–F03.3 after CI-1. Improve diagnostics before attempting repeated full runs. | All four Darwin cases pass on stock `/bin/bash`, with owned EOF/foreign survival and correct evidence retention. Scoped Go tests/vet/build and race tests pass under their declared safe environment. |
| CI-3 — Portable/Linux coverage | F01.3–F01.4 and F03.4. Split portable versus Linux-only suites without dropping assertions. | Linux child-job 19+, ordering 7+, process-group 4+ and supplemental assertions mapped and run; portable suite also runs on Mac; umasks 022/077 tested; fixed-count stress outcomes all retained. |
| CI-4 — Evolving-source CI | Implement the trigger/provenance/failure-semantics changes in section 5. Draft read-only in parallel, implement in a separate allowlist. | Legitimate reviewed source changes can run; tampered or dirty inputs fail provenance; correct branch trigger; missing/failed native cases force red; workflow syntax/lint passes. |
| CI-5 — Full project checkpoint | Run the existing quality/frontend/package matrix plus CI-1–CI-3 on the same frozen commit. Reuse existing native package-smoke jobs. | Required jobs actually run and pass; filtered suites cannot satisfy acceptance; declared source/archive/target identities agree; original disabled mobile jobs are not counted as coverage. |

**Suggested order:** CI-0 → CI-1 → CI-2 → CI-3 → CI-4 → CI-5. F03.1 diagnostics may be prepared after runner safety, before the parser repair. F04.1 should start early as read-only design work but does not block CI-1–CI-5.

Local focused passes precede CI pushes. CI supplies independent reproduction, not a remote edit/debug loop. If CI-5 exposes unrelated or integration failures, record them and use a narrow relevant F-step to fix them; do not claim every failure is the BSD regex defect.

## 4. Corrections to the original feature contracts

### F01 — Make cannot silently replace the test PATH

The Makefile currently prepends Homebrew and user-bin directories to PATH. A clean environment alone does not ensure that recipes execute stock BSD tools or fixture adapters first.

Have the reviewed runner enforce the final effective PATH **inside Make recipes and subprocesses**, including recursive Make. Use explicit tool paths or a tested command-line PATH override as appropriate. Assert `/bin/bash`, `/usr/bin/sed` and intended adapters are actually used. Check `BASH_ENV`, `ENV`, Git config, proxy variables, inherited `HERDR_*`/`TS_*`/deployment credentials and absolute sibling invocations. No test should source an untrusted production `.env` via Make's include.

The stock `/usr/bin/sort -V` smoke check passed on this Mac. Do not rewrite NVM sorting on the assumption that every BSD sort rejects `-V`; verify the actual supported runner versions and the relevant code path first.

### F02 — Structured extraction, not just portable alternation

The original accessor returns empty output for valid `true` on stock BSD sed. This is now directly reproduced, not merely a Linux POSIX-mode hypothesis. The reference patch fixes that narrow case but still accepts nested/shadowed keys and escaped duplicate names; it also removes whitespace *inside* strings/keys.

Prefer a narrow command in the existing **packaged relay executable**, with bounded typed JSON validation, rather than a new Python/jq/Go-toolchain runtime dependency. Standard `encoding/json.Unmarshal` alone does not reject duplicate keys or case-insensitive struct matching: preserve explicit decoded-key, type, depth, size and whole-document checks. Choose the interface only after checking bootstrap/binary-resolution recursion and all existing callers; retain intentional nested legacy lookups through explicit contracts, not an indiscriminate global behavior change.

Acceptance must cover at least valid true/false; missing/wrong-type fields; malformed/trailing JSON; nested shadows; duplicate and escaped-equivalent keys; whitespace in field names; quoted lookalikes; depth/output limits; string/number identity consumers. A missing/invalid value never authorizes cleanup or pairing. Reject permission-bearing malformed responses as a whole, not by selecting a convenient substring.

The two-file patch also changes a managed-holder test fixture. Treat fixture compatibility as its own reviewed test correction, not evidence that replacing sed alone fixes every test.

### F03 — Native arm64 is only one coverage cell

The existing Darwin driver explicitly rejects Intel. Keep that guard until the driver is deliberately generalized and validated; do not forge `uname` or silently skip Intel. Reuse Linux namespace tests only in actual Linux. Ensure bounded nonce/role reads and early launcher-exit diagnostics; the existing handshake loop has no total byte cap and can keep waiting after launcher death.

Passing synthetic lifecycle tests closes neither F04 nor the real-browser/tailnet gates. Conversely, a deliberately unavailable real ownership path is not a reason to suppress the fixture matrix.

### F04 — Parent authority and mutation concurrency are distinct

The pinned upstream CLI obtains a session ID from its private `WatchIPNBus` watcher and inserts a foreground config under it. The inspected command path exposes no parent-bound session output contract; matching a later observed route does not provide that binding. Local `Inspect` always sets `ServeRouteOwned=false`. F04 is a genuine cross-platform feature blocker.

In the ADR, separately specify **atomic concurrent-config protection**. A second read immediately before mutation narrows a race; it does not eliminate it. Pinned upstream `GetServeConfig` carries an ETag and `SetServeConfig` sends `If-Match`. Preserve and test the chosen mechanism's actual daemon-side conflict behavior and foreign-route preservation. Inject a foreign config change between read and mutation. Do not invent unsupported ownership APIs, silently switch to LocalAPI mutation, or use unconditional config replacement. A scope change still requires owner approval.

Keep explicit HTTPS eligibility checks: the pinned CLI can enter an interactive feature-enablement path. Launching it with `--yes` or without a TTY is not proof that prerequisites were safely verified.

### F05 — Define “zero effects” precisely

“No effects on contention” means no managed/product config, credentials, services, relay/Serve launches, downloads, deployment, install activation or foreign-resource mutation. Admission may necessarily start a bounded lock helper and create private test/log resources; list and account for these rather than promising literally no syscall/file/process activity. The existing shell holder creates a temporary log even on refusal.

Keep the cooperating-writer versus arbitrary same-UID adversary distinction. F05/F06 must not promise portable atomic compare-and-unlink against arbitrary same-UID replacement without an approved implementable design. Do not alter lock ordering or add a parallel lock regime casually.

### F07 / F13 — Extend existing coverage instead of recreating it

`.github/workflows/check.yml` already builds four release targets and has native smoke cells for Linux amd64/arm64 and Darwin amd64/arm64. Those smoke checks are useful but do not exercise the full Tailscale launcher. Extend them or share their artifact producer; do not replace them with arm64-only coverage or claim cross-compilation is runtime proof.

CI native/package jobs belong in the early CI track. Do not wait for F07's entire release-readiness redesign, F10 browser harness or F12 credentials to fix branch triggers and run ordinary native fixtures. F13.2's later full-feature matrix still includes Layer 1 and all original acceptance obligations.

## 5. Required CI workflow changes (CI-4)

1. **Fix triggers intentionally.** The diagnostic workflow only runs on pushes to `ci/tailscale-native-preflight-ef9f843`; `check.yml` runs on main/dev pushes or PRs. A new work branch triggers neither by default. Add the narrowly authorized execution branch or a reviewed manual entrypoint, and arrange the existing check jobs on that candidate without requiring an unauthorized PR/main push. Avoid arbitrary-ref checkout and secret-bearing PR execution.
2. **Replace the snapshot-only digest gate for ongoing development.** The diagnostic workflow hard-codes `ff8ff3de…` over all files except itself. Any implementation edit, new test, or these planning files invalidates it. Preserve the old manifest as historical evidence; do not keep regenerating it and call that historical preservation.
3. **Use revision-bound provenance.** For each new CI run assert checkout HEAD against the event's intended SHA, clean index/worktree and declared source membership. Verify checkout content/modes against committed Git objects, inventory current inputs, and bind artifacts/reports to commit and source digest. Reject undeclared untracked source/ignored runtime state before tool setup/build; explicitly account for later generated outputs and provisioned caches. Include workflow/test inputs in the new evidence identity without creating a self-hashing manifest. Comparing two freshly computed hashes from the same possibly modified tree is not independent verification.
4. **Test provenance refusal.** In a disposable copy, modify a tracked byte or mode, add undeclared source, and introduce an escaping symlink; the gate must fail. A legitimate separately reviewed commit must pass without rewriting the old baseline inventory. Document the trusted event/checkout boundary: this is reproducibility, not independent approval of attacker-controlled workflow code.
5. **Make failure unavoidable.** The current diagnostic `continue-on-error` steps already feed an `always()` final gate using `steps.<id>.outcome`; preserve that protection or use normal failing steps. Do not use the GitHub step `conclusion` alone: it can read success after a continued failure. A permanent aggregate must explicitly require every expected matrix outcome, including skipped/cancelled/absent cases, and must still execute after upstream failures. Diagnostics uploads must not turn test failures into success.
6. **Reuse and extend jobs.** Required initial native lifecycle: `macos-15` arm64 plus a Linux runner with proven namespace capability. Preserve the existing four native release-smoke targets. Add native Intel lifecycle qualification before declaring the full advertised macOS support qualified; unimplemented support remains pending, not implicitly covered by package smoke. Record architecture and resolved tool paths inside every cell.
7. **Lint every changed workflow.** Use an explicitly provisioned pinned actionlint; include `tailscale-native-preflight.yml`, which the existing `make mobile-ci-check` actionlint command omits. Pin/record tooling; do not silently install during offline verification. Broad pre-existing lint findings need an exact baseline and separate disposition, not blanket suppression.
8. **Keep artifacts private and identifiable.** Upload sanitized diagnostics and non-secret provenance only, with bounded size/retention. No setup fragments, env dumps, device stores, browser profiles or real tailnet credentials. Avoid duplicate artifact names across matrix cells. Package consumers verify the archive for their native target from the same run and candidate commit; do not substitute an unrelated historical green artifact.

## 6. Remaining feature backlog and dependency corrections

Retain all F00–F13 requirements from the imported plan. In particular:

- F04: approved real authority design and implementation, not `expected = observed`.
- F05–F06: every writer, journal/reprint/IPC/recovery, exact state preservation.
- F07–F09: coherent physical release, readiness, resolver/consent, credentials and update policy.
- F10–F11: real packaged relay/frontend under trusted isolated HTTPS/WSS; actual enrollment and commands, not credential/health mocks.
- F12: separately authorized distinct-node real tailnet runs and cleanup, including claimed platform/distribution compatibility.
- F13: truthful docs, complete final matrix and review of the accumulated 95-file product diff; explicit disposition of historical missing evidence.

The imported `status.json` is an **immutable initial template**, not a validated live scheduler or an approval record. Initialize a new execution status file outside `imported/`, preserving all 62 F-step IDs. Record separate source/native/package/Layer-1/Layer-2 outcomes and the CI milestones above. Do not mark implementation steps complete because this review verified their descriptions.

Apply these dependency rules when initializing execution state:

- Keep F04.1 eligible after F00.1; real authority must not block parser/fixture work.
- Allow early CI wiring after safe runner/parser/lifecycle evidence; the imported F13.2 dependencies apply to the *full-feature* matrix, not this early CI subset.
- F10.1–F10.4 may construct a harness and run negative cases after an F04.1 interface proposal. Record any provisional interface; reconcile it with the **approved** F04.2/F04.3 implementation and rerun affected harness cases before F11.1 positive acceptance.
- F11.1 must also require F09.2's backend enrollment/persistence checks, not merely F09.3 update-policy checks. F11.5 continues to require F09.4's manual-update preservation.
- F12.1's static credential-boundary/runbook design can start before F04.2 approval; real execution still requires F04.4, F11.5 and separately approved F12.2.
- Correct visibly misleading user docs about fail-closed availability early; the final F13.1 verified operating instructions still depend on completed feature behavior.
- A complete final gate checks **all mandatory substeps/outcomes**, not just terminal nodes in a dependency graph. Do not treat an acyclic graph as proof of semantic completeness.

## 7. Verification and delivery

Follow [VERIFICATION.md](imported/VERIFICATION.md) in the reviewed disposable environment. Its proposed `make tailscale-*` interfaces **do not exist yet**. Add only the interface needed for the current slice and test its prerequisite/refusal behavior.

Before a CI-ready checkpoint, run formatting, vet, normal/race Go tests; shell syntax, the portable/native split and production-path audit; frontend lint/type/unit/build/size; same-build shipped-web/browser checks; cross-build and exact native package smoke. Use the existing Makefile commands once safety is established. Preserve intentional unrelated opt-in test dispositions and existing disabled mobile-device jobs explicitly; they are neither passes nor replacements for Tailscale Layer 1/2.

Per slice: reproduce → minimal allowlisted change → focused positive/negative gates → affected regressions → independent source review → immutable evidence/checkpoint. Bound repair attempts; do not retry until lucky, overwrite failure logs, relax identity checks or delete required assertions.

CI-ready output must name the exact source commit, workflow/run IDs, all required jobs/cases, native architectures, archive digests, tool versions, source review, and remaining feature blockers. Full feature readiness additionally requires every original matrix gate. Propose a PR only after separate authorization; publish nothing as part of execution-plan preparation.

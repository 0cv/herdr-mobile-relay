# Tailscale foreground mode — macOS-led completion plan

## Goal and execution model

Finish optional, foreground, tailnet-only Tailscale Serve without weakening authentication, resetting devices, changing the selected app origin without consent, or touching foreign routes/processes/files. Develop and debug on **the receiving Mac**, not by pushing every experiment to GitHub. Keep Linux regression and real-tailnet qualification as separate required gates.

This document is an executable backlog, **not a claim of implementation approval or permission to use a production tailnet**. Read [HANDOFF.md](HANDOFF.md), [MACOS-RUNBOOK.md](MACOS-RUNBOOK.md), and [VERIFICATION.md](VERIFICATION.md) before the first edit. The plan and supporting files can be transferred together; no `/home/cv` path or old workflow database is required on the Mac.

### Non-negotiable scope

- Preinstalled, manually authenticated Tailscale only. No automatic installation/login, Funnel, global `serve off/reset`, ACL/DNS administration, or background-service feature.
- Loopback relay; trusted tailnet HTTPS/WSS; explicit invocation-local consent.
- Preserve relay key, instance, enrolled devices, app selection, foreign resources, and legacy non-Tailscale behavior.
- Phone-managed updates refuse before effects; stopped, verified manual updates are the supported path.
- An observed Serve route/session ID is **not proof of ownership**. No positive pairing until independently bound authority is implemented and qualified.
- No production Herdr/services, developer tailnet, host trust-store edits, or broad process-name/PID cleanup in fixtures.
- Commit/push for CI may use a separate feature/CI branch. Never push `main`/`dev`, open a PR, publish a release, or change branch protection as part of this plan without the applicable additional authorization.

## Starting point: useful implementation, not a finished feature

Published baseline: `b74a1c16c9c4a1b1207cad88fb00b33556e04025` on `ci/tailscale-native-preflight-ef9f843` in `0cv/herdr-mobile-relay`. It contains the accumulated candidate plus a branch-only diagnostic workflow. It is not `dev`, and its CI is red.

- Focused Linux fixtures have passed: 19 child-job, 7 launcher-order, 4 process-group, and 16 supplemental shell cases at recorded revisions. Repeat against every relevant new revision; these are not full Linux acceptance.
- Native macOS arm64 scoped Go tests/vet/build passed. Both CI preflights failed all four Darwin lifecycle cases before relay/Serve handshakes. Sanitized logs reported `exposure_inspection_incomplete`.
- A subsequent **unapproved**, local-only two-file boolean-parser patch is included for reference. It reproduced GNU/BSD sed portability trouble but has an unresolved JSON string/field confusion finding. Do not automatically apply it as a finished fix.
- Positive Serve authority is still deliberately unavailable (`ServeRouteOwned=false` in the inspector). Browser/tailnet/package acceptance is unfinished on both OSes.
- Old review C2F2D lacked an authenticated complete historical dirty-tree snapshot. New work gets a fresh committed baseline and complete inventories; do not fabricate the missing history or call that old finding fixed. See F00.3 and F13.3.
- A previous worker accidentally edited the comparison worktree. Exact original bytes were restored and all 778 recorded source paths checked. Do not transfer that contaminated worker revision or reuse its claimed approvals.

## How small iterations work

Execute **one numbered substep** below per implementation/review handoff. Each substep gets its own narrow file allowlist, input identity, focused test, negative control where applicable, and outcome. Prefer a handful of related files; if a step needs two different contracts or sprawling changes, split it before editing.

1. Start from the last verified revision in the one writable Mac checkout.
2. Add/reproduce the failing test; implement the smallest change in its allowlist.
3. Run the feature's local gate, then related regressions. Save failed attempts too.
4. Independent reviewer checks the actual diff, relevant callers, and evidence; the controller executes acceptance commands separately if reviewers are source-only.
5. A clean review is source approval for **that substep only**. Record native/Linux/CLI/browser evidence separately.
6. Commit only reviewed feature files on the separate branch; checkpoint state in [status.json](status.json). Do not regenerate a historical evidence manifest to make it pass.

An unavailable later gate must not prevent an independent unit/test feature being reviewed. Conversely, a unit pass never waives a later gate. Stop only the dependent branch of work; no routine human question for a test failure. Seek a decision for real authority changes, unavailable infrastructure, or a changed security guarantee.

Use [SLICE-TEMPLATE.md](SLICE-TEMPLATE.md) for each handoff. If reviewed orchestration is used, use the unchanged standard `review.js`, Luna Max implementer/reviewers and Sol XHigh unblocker. Launch it **from the Mac checkout it may edit**, with verified cwd/root; do not reuse old controller `workspaceMode: current` metadata. Reviewers must not edit; workers must not try to dispatch their own reviewers. Bound repair passes (e.g. three); stop and split an overgrown slice instead of re-running an unchanged blocked loop.

## Dependencies and first milestones

```text
F00 baseline
  └─ F01 safe local harness ─ F02 portable inspection ─ F03 native lifecycle
  └─ F04.1 authority decision [start early, read-only; don't postpone it]
       └─ approved authority scope ─ F04.2–4.4
F01 ─ F05 shared ownership ─ F06 transactions/reprint/recovery
F01 ─ F07 release/readiness
F01 ─ F08 resolver/menu/consent
F05 + F07 + F08 ─ F09 credentials/update preservation
F03 + F04 + F06 + F07 + F08 + F09 ─ F10 TLS/artifact harness ─ F11 browser journeys
F04 + F11 + separately authorized two-node environment ─ F12 real tailnet
All required native/Linux/source/package/browser/tailnet gates ─ F13 final readiness
```

F04's missing external authority does not block F02, F03's inert fixtures, F05–F09, or F10's harness construction/negative cases. It **does** block a claim that the real launcher can pair successfully and therefore full F11/F12 acceptance. No fake ownership flag in production to jump that dependency.

Suggested first milestone: F00 → F01.1–1.3 → F02.1–2.3 → F03.1–3.3, with F04.1 running as read-only design work. Deliver a native local reproducer and resolved startup/parser issue before scheduling another CI run. First **usable real feature** milestone additionally requires F04, F06–F09 and positive browser/tailnet evidence; do not promise that F03 alone makes Serve usable.

---

## F00 — Reproducible Mac handoff and safe workspace

**Outcome:** a single unambiguous writable checkout and a verifiable starting revision. No product changes.

| Step | Work | Independent check |
|---|---|---|
| F00.1 | Import the pinned published commit into a fresh standalone clone, new non-main/dev branch. Verify all baseline source paths using the transferred inventory. | Commit, 826 source files, modes, no missing/extra source, and diagnostic workflow hash match. No ignored runtime files or credentials transferred. |
| F00.2 | Inventory toolchain, macOS version/architecture, stock Bash/BSD tools; establish private work/evidence/cache directories and disposable native runner. | Reviewer can reproduce environment identity and demonstrate writes cannot reach the old/installed checkout. |
| F00.3 | Establish a **new** all-source baseline; map the accumulated diff against original base `ef9f843…` into the feature reviews below (not one giant up-front review). Preserve historical limitations; quarantine the two-file pending patch. | Every accumulated path has a review owner/feature; full current review completes by F13. No historical all-path preservation claim. Independent disposition for old C2F2D scope gap is still required before final readiness. |

**Files:** new local handoff/runner metadata only. Keep developer config, `.env`, device stores, issue documents and historical evidence outside source. **Fail control:** alter one inventory byte in a disposable copy: import verification must fail. See HANDOFF for exact commands.

## F01 — Fast, safe Mac test loop and genuine platform coverage

**Outcome:** tests run locally in minutes and failures are diagnostic; neither skipped tests nor GNU tools mask macOS defects.

| Step | Work | Independent check |
|---|---|---|
| F01.1 | Inventory each existing suite's effects. Prove fail-on-use service/Herdr/Tailscale/deployment adapters are actually reached, including absolute sibling script calls. | An intentionally wrong sibling/service adapter fails before any real service call. Preserve TS-TEST-001 safety repair. |
| F01.2 | Add narrow test-runner entrypoints for pure/file checks, stock-Mac shell fixtures, native process fixtures and Linux-only process fixtures. No product hooks. | Wrong OS/architecture/prerequisite exits non-success with a classified reason; a required case cannot disappear as a passing skip. |
| F01.3 | Split `make shell-check` into a portable contract suite plus explicit native/Linux-specific jobs while preserving all old assertions. Replace or isolate `/proc`, `mapfile`, GNU `timeout`; test BSD utilities and stock `/bin/bash`. | Mac portable + Mac lifecycle and Linux portable + Linux lifecycle all accounted for. Do not merely skip `tests/test_tailscale.sh` on Darwin. |
| F01.4 | Correct test-fixture mode and networking assumptions: explicitly chmod expected fixtures; use available private loopback, not public services. | Relevant suites pass under umask 022 and 077; deliberate mode/loopback errors fail clearly. Full suite is no longer "green" by separately choosing favorable umasks. |

**Files:** Makefile, narrowly scoped test scripts/runner, relevant fixture setup only. No general product refactor. Audit NVM version sorting on actual BSD `sort`; repair in its own tiny portability substep only if reproduced. Do not assume an old reviewer portability claim is a runtime fact.

**Local usage:** the proposed `make tailscale-unit`, `make tailscale-macos`, and `make tailscale-linux` interfaces are specified in VERIFICATION; they **do not exist yet**. Implement the runner safety contract before invoking broad targets on the Mac.

## F02 — Correct, portable structured inspection consumption

**Outcome:** stock macOS reads true/false and identity fields correctly, without accepting text inside JSON strings as authority.

| Step | Work | Independent check |
|---|---|---|
| F02.1 | Capture the baseline boolean-accessor failure using stock `/usr/bin/sed` on Mac and GNU POSIX mode as Linux negative control. Inspect emitted fixture JSON locally. | Explain whether the 70-byte incomplete-inspection refusal is reproduced; keep native logs separate from hypothesis. |
| F02.2 | Specify a bounded typed/top-level response contract. Prefer Go standard-library parsing through an existing/narrow helper or a rigorously tested portable parser; do not build a larger regex parser. Evaluate bootstrap/helper recursion before choosing a Go subcommand. | Design covers duplicates including escaped keys, wrong types, malformed/trailing/deep/oversized JSON, missing fields, string lookalikes and nested shadows. No new user runtime dependency. |
| F02.3 | Implement boolean consumption and a focused table test; audit string/number fields used for origin/run/instance to prevent the same problem. Change other accessors only in separate needed substeps. | Stock BSD/Mac tests and GNU/Linux tests agree. Exact valid true/false succeed; escaped lookalike, incomplete or malformed data never authorizes mutation/cleanup/pairing. Pre-fix and permissive mutants fail. |
| F02.4 | Run the actual inert launcher through preflight with complete/partial/unknown fixtures. | Complete fixture advances to the next genuine gate; incomplete fixtures make zero child/config/Serve effects. This is not real session ownership. |

**Files:** `relay/common.sh`, focused tests (including safe test_common cases); only if selected, a narrow `cmd/herdr-mobile-relay` helper and tests. `relay/tailscale.sh`/`setup-link.sh` caller changes get their own review. **Reference only:** `handoff/UNAPPROVED-portable-bool.patch`; do not carry its escaped-string bug forward.

## F03 — Foreground process lifecycle works on native macOS and Linux

**Outcome:** owned children/descendants retire within bounds; foreign processes survive; ambiguous cleanup retains evidence.

| Step | Work | Independent check |
|---|---|---|
| F03.1 | Improve the Darwin test harness first: bounded nonce/role handshake length/time, early launcher-exit detection, sanitized startup diagnostics, exact role/EOF accounting. | Fixture that exits before handshake fails promptly with useful error category, not four blind 45-second waits. Wrong nonce/duplicate role/partial handshake fails. |
| F03.2 | Run native Go supervisor normal/race + inspector inherited-pipe tests on Mac. Fix one concrete lifecycle defect per substep. | Direct-child exit, held output pipes, cancellation, ignored signals and group-anchor retirement complete within defined deadlines. Mutants removing WaitDelay/group ownership fail. |
| F03.3 | Run each Darwin launcher scenario individually, then the whole matrix: normal termination, relay leader exit, Serve leader exit, cleanup-failure retention; foreign survival. Use stock Bash. | All expected owned role channels reach EOF; no unrelated signal; session/log deletion only with proof. Preserve deliberate post-KILL proof refusal and never cache an unproved identity. |
| F03.4 | Run Linux child-job 19+, launcher-order 7+, group 4+ and native Go/race counterparts on the same source. Repeat race-sensitive scenarios a fixed count (e.g. 20), preserving every outcome. | No retry-until-green; injected missing/mismatched records and unsafe-cache mutants fail as designed. Distinguish direct target retirement from separately owned group cleanup. |

**Files:** driver tests first, then `internal/processsupervisor`, inspector supervision, `relay/common.sh` job helpers, launcher in separate substeps. **Safety:** lifecycle tests only in a disposable macOS guest/runner; direct Popen handles are not blanket PID/PGID reuse proof. Audit check-signal races and anchor lifetime. If cleanup proof is unavailable, fail and destroy the guest rather than scan/kill host processes. Linux containers on Mac do not count as Darwin evidence.

## F04 — Real Serve session authority (early design gate)

**Outcome:** a defensible, supported binding from this owned foreground process/channel to this Serve session and exact route. This is a **cross-platform functional blocker**, not a Mac test issue.

| Step | Work | Independent check |
|---|---|---|
| F04.1 | Reconcile `internal/tailscale/CONTRACT.md` with upstream pinned source: CLI/daemon 1.102.4, commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`. Write an ADR for independently bound session authority. | Reject circular `expected=observed`, PID-only, new-route-diff, origin/port matching or a healthy relay as session proof. State feasibility explicitly. |
| F04.2 | If a supported parent-bound CLI mechanism exists, pin and specify it. Otherwise present a concrete choice: approved newer upstream contract, explicit relay-owned LocalAPI session scope, custom CLI distribution scope, or continued unavailable feature. | **Owner authorization required** for new LocalAPI mutation/CLI distribution/tracing/version scope. Do not implement an unapproved alternative or mark DR-1 closed by assumption. |
| F04.3 | Implement the chosen adapter behind a narrow authority interface, with pure/fake contract tests; bind canonical HTTPS host/port, Proxy `/`, loopback backend, session and owned lifetime. | Foreign same-origin replacement, extra route, background/Funnel state, stale channel and arbitrary observed session all refuse. Test real caller, not only a permissive stub. |
| F04.4 | Wire pre-consent, post-consent/immediately-pre-mutation and runtime/reprint identity checks; require HTTPS eligibility without interactive automatic enablement. | Changed node/account/tailnet/DNS/version, logout, partial status, or missing HTTPS prerequisites: no start/arm/QR and no foreign cleanup. Real compatibility deferred to F12. |

**Files:** contract/ADR then narrow adapter/CLI/caller tests. No unrelated UI. **Independent exit:** F04.1 may finish with a well-founded decision request; F04 feature and positive E2E remain BLOCKED until authority is approved and proven. Other source features continue.

## F05 — One managed owner across every writer

**Outcome:** another launcher, menu action, service wrapper or updater cannot mutate a live managed root.

Use existing `internal/managedstate` and holder commands, not a second lock system. O means lifetime owner; T means bounded transaction. Re-inventory all callers at the actual Mac revision; old line numbers are not authority.

| Step | Work | Independent check |
|---|---|---|
| F05.1 | Audit writers and install-root/O/T ordering; decide/document DR-2 retirement threat model. | No age/PID stale takeover. Explicit distinction between cooperating-writer checks and impossible portable atomic compare-and-unlink against arbitrary same-UID replacement. Owner decision needed to narrow any stronger promise. |
| F05.2 | Setup/start/menu/chooser group; bootstrap missing roots explicitly, use read-only resolution before admission. | Held O causes zero mkdir/chmod/env/key/origin writes, zero launches and preserved winner state; aliases converge. |
| F05.3 | `configure-app-deploy` and read-only status/dev entrypoints. | Refusal precedes config/credential reads that have effects, job creation, downloads or service calls. Status never arms; dev roots cannot alias production. |
| F05.4 | Service install/uninstall/restart wrappers (one family per handoff). | No service stop/start/unit/plist mutation on contention or selected foreground mode; inert managers only. |
| F05.5 | Rotation, hostname, teardown/uninstall (one action per handoff). | No credential loss, foreign-file deletion, endpoint change, or route effect on refusal. Existing allowed legacy action remains functional. |
| F05.6 | Plugin/build/install/activation/pruning and shared release roots. | One lock ordering for both shell install and Go update; two config roots sharing an install root cannot race activation/pruning. |
| F05.7 | Go update/appdeploy schedule/worker/recovery constructors. | Reconcile existing flock regimes; validate at worker boundary, including constructor/recovery writes, not only shell admission. Network/job/deploy sentinels remain unused on refusal. |
| F05.8 | Cross-writer race matrix, crash retention and canonical-path aliases. | Two contenders: one winner, loser zero effects; no deadlock; inconsistent records retain evidence. |

**Files:** one named caller family plus its tests per step; existing helpers only as narrowly needed. This is intentionally many small conversions, not one repository-wide lock rewrite. Pure refusal tests do not require a real tailnet.

## F06 — Durable start, reprint and recovery without data loss

**Outcome:** partial setup never silently destroys origin/config/device state, and reprinting pairs another device safely.

| Step | Work | Independent check |
|---|---|---|
| F06.1 | Audit startup env/origin/session publication against existing journal primitives; close remaining nontransactional writes. | Fault injection at every write/fsync/rename/commit boundary preserves exact old bytes, modes, absence and trailing newlines or retains a recoverable journal. |
| F06.2 | Qualify live reprint using T only while Serve holds O, with release/run/socket/route revalidation. | Concurrent reprints serialize; definite arm rejection rolls back selected origin; ambiguous/persistence failure retains evidence and prints no link. |
| F06.3 | Qualify local-control sockets and bounded messages on native Mac/Linux. | Replacement file/symlink/socket survives all constructor/close paths; malformed/wrong-run/oversized/hanging requests refuse. Maintain DR-2's honest guarantee. |
| F06.4 | Explicit stopped-owner recovery with verify-before-mutate, no invitation-consumption guesses. | Live owner refusal; edited target/symlink/root mismatch refusal; each journal phase reconciled only when proven. No automatic owner takeover or blind marker deletion. |

**Files:** `internal/managedstate`, localcontrol, managed recover/reprint commands, launcher/reprint wiring, in separate substeps. Keep the device-store mutex/O/T ordering explicit. **Negative control:** removal of fingerprint checks or arm acknowledgement must make tests fail.

## F07 — Exact package identity and usable managed readiness

**Outcome:** the printed invitation reaches one coherent, usable physical release, not just an HTTP listener.

| Step | Work | Independent check |
|---|---|---|
| F07.1 | Write release identity schema/ADR: archive digest, manifest, binary version/revision/protocol, web hash, run/instance/origin. Avoid self-referential archive/manifest hashes. | Fixture archives with wrong/missing/malformed identity reject; exact trusted checksum provenance defined. |
| F07.2 | Pin one verified physical release directory for the foreground lifetime and helpers. | Swap `current` between operations: never mix versions or invoke substituted helper. Space-path execution works. |
| F07.3 | Make managed web/bundle, real HTTP+UDP bind, inventory and `/readyz` requirements explicit; preserve legacy degraded modes only where intended. | Occupy UDP after preflight; remove bundle; wrong inventory/run/protocol: no control-ready/arm/QR. `/healthz` compatibility tested separately. |
| F07.4 | Wire bounded trusted-HTTPS readiness and binary/control/public/browser identity comparisons before initial pairing and reprint. | Wrong version/revision/hash/SAN/run/instance/origin and oversized/timeout health responses fail; no `curl -k`. |
| F07.5 | Closed package inventory, safe extraction, wrapper dependencies and modes (`speech-voices.sh`, `.env.example`, web assets included). | Extract exact archive away from checkout; missing/extra/replaced/symlink/traversal entries rejected per schema. No build/source fallback. |
| F07.6 | Native extracted-package smoke on macOS and Linux targets. | Verified source→archive→extracted bytes; no checkout scripts/assets hide missing files; actual native runtime, not cross-compile only. |

**Files:** release helpers/packaging first, readiness server/callers separately. `scripts/package-release.sh`, `scripts/check-installed-release.sh`, `internal/app`, `internal/release`, `relay/common.sh` are starting points, not one allowlist. **No publication** during packaging checks.

## F08 — Setup menu, consent and transport resolution

**Outcome:** selecting foreground Tailscale is explicit and coherent; other transports still work.

| Step | Work | Independent check |
|---|---|---|
| F08.1 | Define one Go/shell resolution table for explicit/saved/inherited/unset values, gateway requirements, service restrictions and app selection. | Every contradictory case refuses before writes/downloads/registration/restart. Preserve unset-mode legacy inference. |
| F08.2 | Convert dispatchers one at a time to resolved mode, not gateway-presence heuristics. | Valid legacy gateway/Cloudflare/direct paths match prior behavior with inert adapters. Switching cannot leave stale Tailscale settings active. |
| F08.3 | Actual menu/chooser → launcher contract: one-shot `--confirm-serve` or fresh affirmative TTY; no saved/exported YES authorization. | EOF, empty/default, non-TTY, generic setup approval and inherited consent refuse. Explicit consent still runs all preflights. |
| F08.4 | Preserve existing shared app origin until explicit selection, make status read-only and errors/recovery guidance truthful. | Byte-compare app selection and devices across choices/cancellation; status prints no usable invitation. |

**Files:** resolver config/common then one menu/start/service entrypoint family at a time. **Local Mac:** PTY test for the real prompt with stock Bash and synthetic CLI; no real tool installation.

## F09 — Credential continuity and safe update behavior

**Outcome:** existing phones stay enrolled across start/stop/reprint/manual update; online updater cannot bypass policy.

| Step | Work | Independent check |
|---|---|---|
| F09.1 | Requalify exact-32-byte key and parsed reset prohibition at direct Go admission; tokenless legacy loopback separately. | All `strconv.ParseBool` true spellings rejected before store/reset/bind; origin-less clients still require keyed encrypted auth. |
| F09.2 | Exercise first/second enrollment, arm persistence failures, expiry/consumption/revocation in temporary real stores. | Exact identity/device preservation; second device distinct; failed persistence cannot print an accepted link. |
| F09.3 | Requalify resolved policy at state projection, schedule and worker boundaries, including cached available jobs. | Zero metadata fetch/job/deploy/install/service effects in foreground mode. A disabled button alone is not sufficient. |
| F09.4 | Stopped manual update from two verified fixture packages, then restart with same state. | Token, instance, selected app origin, enrolled devices preserved; new release identity coherent; active owner blocks update. No real install/plugin/service operation. |

**Files:** focused config/auth/deviceauth/update tests and smallest necessary corrections; manual-update harness and docs later. **Local independence:** may prove with actual backend/file/socket fixtures before browser evidence, but does not claim phone UI acceptance.

## F10 — Artifact-bound Layer 1 TLS/browser harness

**Outcome:** a real browser can exercise exact packaged relay/web through trusted non-localhost HTTPS/WSS without a real tailnet.

| Step | Work | Independent check |
|---|---|---|
| F10.1 | Audit existing frontend/mobile fixture interfaces; reuse provenance/fake-Herdr/browser tooling, not mocked credentials/sockets. | Source/artifact identity and private-output boundaries tested; no wholesale mobile CI merge or new dependency without reason. |
| F10.2 | Start an exact packaged relay + fake Herdr; inert Tailscale control adapter drives a real HTTPS/WSS proxy. Design the adapter around F04's authority interface. | Backend/health/arm/WebSocket responses come from real relay, not canned success. Unexpected control commands fail. |
| F10.3 | Fixture hostname + CA trusted only by isolated browser and health client; real service-worker-capable persistent profile. | Valid SAN succeeds, untrusted/wrong SAN fails before pairing; secure context and actual shipped worker activate/control page. No TLS-ignore/insecure-origin flags. |
| F10.4 | Same-run archive/evidence validator and private-log redaction. | Deliberate wrong archive/source/web hash, omitted case, skip, fabricated health, or secret-bearing artifact is rejected. |

**Mac leverage:** author/debug the harness locally; default Linux guest on the Mac may provide isolated browser trust/networking. Native Mac relay/package behavior is separately tested in F03/F07. If a native Mac browser cannot use a genuinely isolated trust store, do not import a CA into the developer's login/System keychain—run browser Layer 1 in a disposable guest and label its platform honestly.

**Exclusions:** no preloaded credentials/localStorage, handwritten setup URL, mocked relay WS, fake service worker, dev-server assets, host trust edits, real tailnet or phone-push certification. F10 harness/negative cases can proceed while F04 is blocked; complete positive launcher acceptance cannot.

## F11 — Real packaged-browser acceptance journeys

**Outcome:** user-visible functionality verified independently of the synthetic shell success fixtures.

| Step | Work | Independent check |
|---|---|---|
| F11.1 | Actual menu/consent/launcher-generated private link → fresh browser profile → enrollment. | Real encrypted handshake/device record, fragment removal, consumed invitation rejected; shipped assets/worker identity verified. |
| F11.2 | Reprint actual local-control invitation → second empty profile; harmless command through shipped UI. | Two distinct usable credentials; correlate command ID in fake-Herdr log and returned UI, not UI-only success. |
| F11.3 | Stop/restart same origin; both profiles reconnect without repairing; try inherited reset flag. | Device IDs/store/instance/origin preserved; no credential injection or reset. |
| F11.4 | Separate data-path interruption/reconnect from owned Serve-process death and explicit relaunch; cancellation and refusal matrix. | Bounded disconnect, owned cleanup or retained evidence, foreign survival; no automatic fallback, QR on error, or orphan. |
| F11.5 | Packaged manual update journey plus exact-identity/evidence validator negatives. | Preserved devices reconnect to intended new release; incomplete or mixed-artifact evidence fails acceptance. |

**Cases include:** existing Serve/Funnel, logout/permissions/unsupported CLI, changed identity, occupied ports, missing web/inventory, wrong HTTPS identity, arm persistence failure, active service, transport conflict, consent cancellation. Run cases separately during development, then mandatory whole matrix on one final archive. No skipped required case is a pass.

## F12 — Separately authorized real two-node tailnet qualification

**Outcome:** prove the pinned real CLI/daemon and actual remote browser path, not only adapters.

| Step | Work | Independent check |
|---|---|---|
| F12.1 | Static runbook/trusted-ref workflow/credential boundaries and cleanup account; reuse F11 scenario interface. | No PR-secret path, arbitrary ref/artifact injection, `pull_request_target`, broad grants, or production tailnet. No infrastructure provisioned by this step. |
| F12.2 | Obtain named disposable tailnet, two isolated authenticated nodes, restricted rights, pinned binaries, explicit operator/cleanup authorization. | Missing access = BLOCKED; Mac development or CI push permission is not this authorization. |
| F12.3 | Execute real Serve from relay node and browser from distinct remote tailnet node using exact packaged artifact. | Distinct node IDs/addresses, actual FQDN routing/trusted HTTPS, loopback backend, no Funnel, independently proven session authority. |
| F12.4 | Run enrollment/command/restart/interruption/refusal/update/foreign-preservation cases; validate owned resource teardown and redact credentials. | CLI schema/version matches contract, both devices survive restart, no unrelated route touched. Failure/cleanup uncertainty stays red. |

The Mac can be the development/control machine and, only if separately designated disposable, the relay node. Do not enroll the personal Mac or use an existing developer tailnet for convenience. Qualify each supported CLI/daemon distribution whose semantics differ; at minimum both claimed Linux and macOS foreground implementations need real compatibility evidence or a narrower explicitly approved support statement.

## F13 — Documentation, CI and final exact-revision release gate

**Outcome:** truthful operation instructions and one final evidence record, not a collection of unrelated green runs.

| Step | Work | Independent check |
|---|---|---|
| F13.1 | Update README/QUICKSTART/transports/security/development for verified prerequisites, scope, manual update and retained-evidence recovery. | No "fully works"/ownership/automatic cleanup claims beyond evidence; unsupported/fail-closed state visible while F04 is unresolved. |
| F13.2 | Wire required portable/native/package/Layer-1 jobs with actual Linux/Mac cells; keep Linux-only tests explicit; reuse mobile interfaces cautiously. | Missing native runners, artifact mismatch, omitted tests or failure cannot become green through skip/continue-on-error. macOS Intel support requires native Intel evidence, not arm64/cross-build. |
| F13.3 | Re-run acceptance matrix and review accumulated diff on frozen commit/archive; explicitly disposition every legacy finding, DR-1, DR-2, parser finding and historical scope limitation. | All mandatory current gates green and reviews current; original red history retained. Unknown historical delta is not fabricated or silently waived; independent reviewer/owner must accept any prospective replacement of that assurance. |
| F13.4 | Produce final source/archive/checksum/evidence bundle and proposed PR description. | `prReady` true only when the matrix is complete. Ask separately before PR/release/main/dev publication. |

**Build gates:** Go format/vet/test/race, relevant shell syntax/static/portable/native suites, frontend lint/type/unit/build, same-run web/release verification, supported native extracted packages, Layer 1 and separately authorized Layer 2. See VERIFICATION for exact known targets and environment. If any substantive known failure remains, do not call the feature complete on either OS.

## Completion checklist

- [ ] All feature substeps have scoped source review or explicit evidence-backed disposition.
- [ ] Stock Mac native launcher and Linux native launcher pass on final bytes.
- [ ] Independently bound real Serve authority implemented, no circular adoption.
- [ ] Every competing writer participates; transactions/recovery/devices preserved.
- [ ] Exact package/readiness/physical-release identity established.
- [ ] Real browser and real backend, then distinct-node real tailnet journeys pass.
- [ ] Native target coverage and broad regressions complete; no concealed flaky/skip results.
- [ ] Documentation matches behavior; historical provenance gap explicitly handled.
- [ ] Final clean independent review; separate publication authorization obtained.

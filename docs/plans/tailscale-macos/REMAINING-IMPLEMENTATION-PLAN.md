# Finish the real Tailscale connection

**Status: source-informed follow-up plan; not implemented or independently approved by writing this document.**

This is the current roadmap after the CI repair and its approved R0 review. For the **next bounded orchestration run**, use [R1-OWNERSHIP-DESIGN.md](R1-OWNERSHIP-DESIGN.md), not this entire roadmap as one implementation assignment. [NEXT-RUN.md](NEXT-RUN.md) is retained as the completed R0 assignment. No new run is launched by preparing these files.

## 1. Goal

On an explicitly designated test Mac, the user can select **Tailscale Serve (foreground)**, consent to exposure, scan the launcher-generated QR on a real phone, enroll, issue a harmless Herdr command, stop/restart, and reconnect without pairing again. Ctrl-C retires only this invocation's resources; other routes, services, credentials and enrolled devices remain intact.

Provide an explicit development entrypoint, proposed as **`make dev-tailscale`**, and an extracted-package acceptance path. The target does **not** exist today. `make dev-tunnel` is not a transport chooser and must not silently change behavior to mean Tailscale.

Success means a working, reviewed relay integration—not only a reachable test page, parser unit tests, or synthetic lifecycle CI.

## 2. Starting point: keep completed work

| Item | Actual status |
|---|---|
| Current post-R0 baseline | `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5` on `ci/tailscale-native-preflight-ef9f843`; original roadmap baseline was `a034f1e…` |
| Go | Installed and active declarations updated to 1.27.1; do not reopen the old 1.27.0 provisioning blocker |
| Parser | Packaged typed `json-field` command and shell consumers implemented; focused normal/race tests passed, including the user's local run; R0 added reviewed repairs |
| General CI | [36032804212](https://github.com/0cv/herdr-mobile-relay/actions/runs/36032804212) passed on the post-R0 baseline |
| Native fixtures | [36032804006](https://github.com/0cv/herdr-mobile-relay/actions/runs/36032804006) passed on Darwin arm64 and Linux amd64; these use synthetic Tailscale control adapters |
| Packages | Four native release smoke cells passed; they do not prove four-platform Tailscale lifecycle compatibility |
| Independent review | R0 workflow `3b823169-0686-44cd-97d5-6d842a8fe95e` completed **approved**, with two clean independent reviews of final revision `d2e5d01…`; approval is CI/parser scope only |
| Real session authority | `internal/tailscale/tailscale.go` still sets `ServeRouteOwned=false`; positive pairing is intentionally blocked |
| Local networking smoke | User reports the separate Tailscale test appears to work. No versioned artifact/node/browser evidence was collected; this is not relay acceptance |
| Existing safety foundations | `managedstate` ownership/journals/recovery, `localcontrol`, managed UDP handling, release manifests, device auth and updater guards already exist; review/reuse them rather than replacing them wholesale |

The original CI repair spanned **34 paths** from `b74a1c1…` to `a034f1e…`; R0 reviewed that candidate plus a six-path repair delta. At final `d2e5d01…`, the CI repair spans **35 paths** from `b74a1c1…`, and the whole feature spans **113 paths** from original base `ef9f843…`. These are different review scopes. Recompute inventories if the revision changes. The earlier workflow's blocked/zero-review outcome remains history; it was not the R0 outcome. [CI-REVIEW.md](execution/CI-REVIEW.md) is the worker's pre-review snapshot; final clean verdicts are recorded in the R0 controller artifacts.

History is in [execution/STATUS.json](execution/STATUS.json) and [execution/WORKER-LOG.md](execution/WORKER-LOG.md). They contain intermediate failures, superseded tool restrictions and some stale per-step summaries; preserve that history and append clearly identified current results, rather than treating every old status string as a current prerequisite.

## 3. Settled operating rules

- **Use the current linked worktree on this Mac.** The user approved this. No fresh clone, clean-tree reset, stash or automatic commit of planning files is required to begin. Record HEAD, initial dirty paths and input hashes; preserve the pre-existing untracked `docs/plans/` tree.
- Keep `imported/` immutable. Its standalone source verifier is **not applicable to this approved execution mode**, not a test that must be forced to pass. Preserve the fact it was not run. The new exact-checkout provenance checks and a fresh current-source review are prospective evidence, not reconstructed historical dirty-tree proof.
- Use Go **1.27.1**, Bun **1.4.0**, the existing frozen frontend lockfile, and pinned CI actionlint **1.7.12**. Do not install system-wide tools or upgrade unrelated dependencies. Explicit dependency/cache setup is separate from offline test execution.
- Implementation, static checks and **audited safe local tests** may use this Mac. Broad shell suites can launch children/service adapters: they are not automatically safe because HOME is temporary. Lifecycle/service-adjacent and Linux tests remain on disposable runners. Preserve the previous worker's disclosed local broad-shell scope deviation and prevent recurrence.
- Existing authorization covers task-only commits and ordinary pushes to **`origin/ci/tailscale-native-preflight-ef9f843`** for CI. Verify destination and staged paths each time. No force-push, other branch, PR, release or deployment. Do not stage unrelated user changes.
- Real Serve mutations/phone qualification require an explicit named environment and operator authorization. The user's manually run harmless HTTP smoke is not blanket permission for the agent to alter their tailnet, production relay or enrolled-device state.
- No automatic Tailscale install/login, Funnel, global Serve reset/off, host trust-store modifications, production service removal, or background-service feature.
- Do not mark a gate passed from historical evidence after changing its relevant inputs. A prerequisite unavailable for a later integration test does not block independent source work or its review.

## 4. One architectural decision to settle early

### Recommendation to evaluate: relay-owned foreground LocalAPI session

The current external CLI owns a private watcher/session, while the relay only observes resulting daemon state. The relay cannot turn that observation into independent authority by matching a hostname, PID, port, new route or health response.

**Recommended feasibility candidate:** a small adapter in the existing Go executable that owns the daemon's foreground notification/watch connection, obtains its own session identity through that connection, conditionally registers this invocation's route, and keeps the connection alive for the foreground lifetime. A fresh standalone `inspect` process must not infer authority from a session ID copied out of observed status.

This is a proposal, **not permission to implement LocalAPI mutation** and not a claim that all macOS distributions support the same access mechanism. The LocalAPI is version-sensitive; the existing contract pins Tailscale **1.102.4**, upstream commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`. Do not describe it as a stable API or silently accept arbitrary versions.

Before asking the owner to choose, independently verify from pinned upstream source:

1. The authenticated watcher-to-session relationship, session lifetime, daemon restart/logout behavior, and whether a parent-controlled channel can actually provide the required authority.
2. How the supported Linux daemon and macOS distributions expose LocalAPI and authorize a non-root client. Do not assume a Linux Unix-socket path works for the macOS app. Report incompatible distributions explicitly.
3. Conditional Serve mutation: the client carries ETag/`If-Match`; verify daemon enforcement, missing/stale ETag rejection and foreground cleanup isolation. A second inspection is not an atomic update.
4. Exact HTTPS eligibility without interactive enablement, full node/account/tailnet/version identity checks, and no effect on other sessions/settings.
5. Dependency footprint: compare a pinned upstream client dependency with a narrowly scoped standard-library transport. Justify any new module; no end-user Python/jq/Go-toolchain dependency.

**Decision record:** amend `internal/tailscale/DR-1-session-authority.md` with immutable source links, feasibility, supported versions/distributions, permissions, lifecycle/API boundary, dependency choice, reviewer outcome, and the user's explicit choice. The user is the owner; no unidentified additional approver needs to be found.

If that candidate is infeasible, present the evidence and a specific alternative (qualified upstream CLI contract, separately approved custom distribution, or keeping the feature unavailable). Do not silently fall back to observation-based ownership. A plan approval alone is not approval of a new mutation interface.

While the decision is pending, CI source review, writer/transaction audits, readiness tests and browser harness construction may continue. Positive real ownership/pairing may not.

## 5. Delivery milestones

Each numbered substep is a bounded handoff with its own file allowlist, test command/environment, negative control where applicable, source revision and independent review. Start by testing existing behavior; change only demonstrated gaps. Do not rewrite a whole subsystem merely because a milestone names it.

### R0 — Independently review the completed CI repair

**Completed and approved at `d2e5d01…`.** [NEXT-RUN.md](NEXT-RUN.md) preserves the R0 assignment. Its scoped approval does not approve a LocalAPI design or real-tailnet use.

- Review the 34-path CI/parser delta and relevant unchanged callers. In particular, check bootstrap/binary resolution, strict JSON semantics, optional string/number failure behavior, provenance coverage, workflow failure aggregation and test isolation.
- Check each repaired race/job/service fixture still asserts the original behavior. Replacing a timing-sensitive fixture with synthetic state must not erase a separate integration obligation; keep the pre-fix failure and post-fix evidence.
- Separate intentional unfinished feature behavior from defects in this slice. `ServeRouteOwned=false` is a declared limitation, not justification to skip the review or invent ownership.
- Fix substantiated in-scope issues minimally, rerun affected local/hosted gates, then have independent reviewers inspect the exact final revision. Worker diff inspection is not independent approval.

**Exit:** CI repair source review is clean for a named revision, required tests are green for relevant final bytes, and unreviewed wider-feature scope is explicit. It is not full feature approval.

### R1 — Qualify and approve the ownership design

**Current run plan: [R1-OWNERSHIP-DESIGN.md](R1-OWNERSHIP-DESIGN.md).** R1-A delivers independent review of a source-backed recommendation; R1-B is the user's subsequent explicit scope choice. Do not block design review waiting for that choice or treat a clean design review as mutation authorization.

1. Complete the source/distribution/API feasibility work in section 4. Deliver a reviewable ADR even if a later live environment is unavailable.
2. Obtain independent design review and the user's explicit selection of mutation/version/distribution/dependency scope.
3. Define a narrow session-lifetime interface and test seam. State who owns the watcher, how launch/reprint prove its liveness, how failures revoke readiness, and how shutdown preserves foreign state. Avoid passing authority as an arbitrary environment boolean or observed session string.

**Exit:** a concrete approved design, or an evidence-backed decision request. Only dependent ownership implementation is blocked by a pending decision; do not consume repeated controller interventions asking the same question.

### R2 — Implement genuine foreground session authority

1. Implement the approved adapter in a narrow Go boundary, keeping the existing read-only inspection contract separate. Add strict bounded fake-daemon tests before connecting a real daemon.
2. Bind the live session to run ID, node/daemon identity, canonical HTTPS host/port, `/` proxy handler and exact loopback backend. Do not persist a supposedly reusable ownership capability across process/daemon restarts.
3. Use the approved conditional mutation mechanism; test a foreign config change between read and write, wrong/missing preconditions, background/Funnel state and an extra route. Refuse rather than overwrite.
4. Qualify cancellation, dropped watch stream, inherited pipes, relay/helper death and bounded shutdown. Closing the owned lifetime must not issue a global reset. If daemon retirement cannot be confirmed within the deadline, retain evidence and fail instead of claiming cleanup.

**Exit:** the actual adapter/caller can prove one owned route against a fake daemon, all named negative cases refuse, and native helper/lifecycle tests pass. This is still not real CLI/daemon compatibility evidence.

### R3 — Integrate startup, all writers, reprint and recovery

1. **Startup/ownership:** define one owner handoff/order from launcher to relay; acquire before product-state mutation, bind actual HTTP/UDP listeners, and revalidate identity immediately before mutation and before arming. Do not introduce a second lock scheme.
2. **Transactions:** replace remaining shell string snapshots/rollback on the managed path with the existing journal primitives where needed. Fault-inject write/fsync/rename/commit and prove exact bytes, modes, absence and trailing newlines survive, or leave an explicit recoverable journal.
3. **Competing writers:** audit and convert one family at a time: setup/start/menu/chooser; config/app-deploy; service wrappers; token/hostname/teardown; installer/plugin activation/pruning; Go update/app-deploy schedule/worker/recovery. Reconcile shared install-root locks with existing flock/O/T ordering. On contention, no product-state, service, download, deployment or foreign-resource effects; bounded admission helpers/private diagnostics must be separately accounted for.
4. **Reprint/IPC/recovery:** reprint takes T while the live relay holds O, proves the live authority/physical release/socket/run again, and prints only after durable arm acknowledgement. Preserve replacement sockets/files. Distinguish definite rejection from lost acknowledgement. Recovery is explicit and stopped-owner-only; no stale-PID/age takeover or invitation-consumption guesses.
5. **Threat model:** independently disposition DR-2. Cooperation between managed writers does not imply portable atomic compare-and-unlink against an arbitrary malicious same-UID process. Record any approved guarantee adjustment rather than promising the impossible.

**Exit:** start/stop/reprint/crash/competition cases preserve credentials, selected origin and foreign resources; changed/unknown authority produces no QR and no destructive cleanup. Existing managedstate/localcontrol tests are retained and extended, not bypassed.

### R4 — Prove readiness, package identity and credential continuity

1. Extend existing release manifests/closed inventories rather than inventing a second format. Pin one physical release for the lifetime and every helper invocation; swapping `current` must not mix versions. Test space paths, missing/replaced assets and extraction traversal/symlinks.
2. Require actual HTTP and UDP bind, usable Herdr inventory, coherent web assets and managed readiness before pairing. A healthy `/healthz` listener alone is not readiness; preserve intended legacy degraded behavior separately.
3. Compare binary, control response, public trusted-HTTPS identity and web descriptors: version/revision/protocol/web hash plus run/instance/origin. Bound network/output handling. No TLS bypass.
4. Requalify exact-key and parsed reset guards, first/second enrollment, arm persistence failures, invitation expiry/reuse/revocation, and origin-less keyed clients. Restart must preserve both devices and the relay identity.
5. Test phone-managed update refusal at projection/schedule/worker boundaries, including cached jobs. Test stopped manual update between two verified packages, preserving key/instance/devices/origin; active ownership blocks activation/pruning.

**Exit:** one exact packaged artifact can legitimately become ready, while malformed/mixed identity, occupied UDP, missing web/inventory or persistence failures cannot arm or print pairing.

### R5 — Give the user a real development path and automated browser test

1. Add proposed **`make dev-tailscale`** with a narrow wrapper. Build/use one coherent binary+web pair, a private development root and separate loopback ports. Clearly label it a development instance; never migrate production credentials or alias a production config/release root.
2. Keep `make dev-tunnel` behavior intact. Production setup-menu `t` and the dev wrapper use the same reviewed core launcher. Selection is not exposure consent: prompt per invocation or accept an explicit one-shot argument; inherited/saved YES is not consent.
3. Do not stop/uninstall an existing production service to make the test work. If safe coexistence requires relaxing the current global service refusal, first review and test an explicit dev/production separation contract; otherwise use a designated test node and report the refusal honestly.
4. Preserve the selected shared app origin unless explicitly changed. Print actionable errors for unsupported Tailscale versions/distributions, unavailable HTTPS, wrong account/node, existing routes, busy ports and retained evidence. Until positive implementation is qualified, docs/menu must describe the unavailable/experimental state truthfully.
5. Reuse existing fake-Herdr/browser tooling to test the **exact extracted package** through a real HTTPS/WSS proxy, with a fake Tailscale control adapter at the R2 boundary. Trust a fixture CA only in isolated clients/profiles/guests. Do not inject credentials, manufacture the setup URL, mock relay WebSockets/health or substitute a service worker.
6. Drive actual consent/launcher-generated pairing, two fresh profiles, a harmless command acknowledged by fake Herdr, fragment removal, shipped worker activation, invitation reuse rejection, restart/reconnect, data-path interruption, owner death and stopped update. Keep those failure modes distinct.

**Exit:** the proposed development command is documented and tested; real packaged browser/backend journeys pass under synthetic control. The actual real-tailnet recipe is ready, but not claimed to have run.

### R6 — Qualify the actual Mac-to-phone Tailscale path

1. Prepare a bounded runbook naming source/archive hashes, CLI+daemon version/distribution, relay node and distinct client node, pre-existing Serve/Funnel state, HTTPS prerequisites, exact test config root/ports, operator, authorized mutation and cleanup scope. Obtain approval **before** live commands.
2. Default automated/destructive qualification uses a disposable tailnet and designated isolated nodes. An optional user-operated smoke on their chosen Mac/phone requires explicit consent for those exact resources and is not a substitute for the adverse-case suite. Never silently enroll a device, grant ACLs, enable HTTPS/Funnel or edit production state.
3. First happy path: installed authenticated node → actual launcher/consent → trusted HTTPS origin → launcher QR → real phone/browser enrollment → harmless command → Ctrl-C → verify owned session removal and foreign preservation.
4. Restart at the same origin; the device reconnects without re-pairing. Reprint for a second device. Test data-path reconnect separately from process death. On disposable nodes, test logout/identity drift, unsupported version, conflicting route, bad certificate, bind/readiness failure, interrupted arm, crash recovery and stopped manual update.
5. On iOS follow the real installed-PWA enrollment behavior rather than treating a disposable Safari tab as installed-app acceptance. Do not claim native push certification from a desktop browser or synthetic profile.
6. Collect bounded, sanitized evidence. Full QR URLs/fragments, keys, device stores and profiles stay private. Missing access means this gate is pending, not that all prior source work must be undone.

**Exit:** positive real route authority, remote HTTPS/WSS, actual enrollment/command/restart/update and owned cleanup are evidenced for the declared distribution/platform. User-visible milestone: **real Mac/phone integration works**; broader supported-platform acceptance may still be pending.

### R7 — Complete remaining platform and regression coverage

- Preserve current Darwin arm64 and Linux amd64 native suites and four native release-smoke cells. Add/qualify missing Intel macOS and Linux arm64 lifecycle/runtime coverage before claiming full existing platform support, or obtain explicit approval of a narrower Tailscale support statement.
- Complete the portable-shell versus Linux-only split, assertion mapping, fail-on-use adapters, effective PATH checks and umask 022/077 fixtures. Audit production sibling calls and GNU/BSD behavior instead of simply skipping tests on Mac.
- Use fixed-count bounded stress and negative controls for lifecycle/provenance/ownership assertions. Do not retry until green or loosen the proof after Bash prunes job records.
- Wire Layer 1/package evidence validation into ordinary candidate CI without secrets. Keep real-tailnet qualification a separate trusted-ref, explicitly authorized gate; no secret-bearing arbitrary-PR path.
- Re-run normal/race/vet/format, shell/static/production-path, frontend lint/type/unit/build/size/browser, cross-build, extracted-package and native jobs on final relevant bytes. The previously disabled mobile-device jobs are not silently counted as coverage.

**Exit:** an explicit supported-target and required-case matrix with real outcomes; no platform inferred from cross-build or unrelated smoke.

### R8 — Final review, documentation and handoff

- Assign every path of the full accumulated feature diff to an independent review slice, including relevant interactions and later changes. Close all substantive findings and record the prospective disposition of missing historical C2F2D evidence; never manufacture the old dirty-tree baseline.
- Update README, QUICKSTART, transports, security and development docs to exactly match qualified commands, versions/distributions, dev versus installed state, foreground lifetime, reprint, manual update and retained-evidence recovery.
- Freeze commit/artifact identities and publish a private/sanitized evidence index: source review, exact package/native results, Layer 1, real two-node/phone results, outstanding support limitations and cleanup outcome. Re-run changed dependency cones; old CI run IDs only cover their old inputs.
- Keep separate flags for automated CI, scoped source review, live Mac smoke, supported-feature acceptance and PR readiness. Full feature readiness needs every applicable required gate, not an empty findings list from a workflow that ran no reviewers.
- Ask separately before a PR/release. Do not use this plan to authorize publication.

**Exit:** complete independent approval and acceptance for the explicitly declared feature/support scope, plus an honest user testing recipe.

## 6. Ordering and independent progress

```text
R0 CI repair review -------------------------------+
R1 source/ADR feasibility --> owner scope decision  |
                             --> R2 adapter         |
R3 writer/journal audit and R4 package/auth audit <--+
R2 + R3 integration + R4 --> R5 complete browser path
R5 + named live-environment authorization --> R6 live phone/tailnet
R7 portability/CI coverage runs alongside the above
R0 + R1..R7 required exits --> R8 final acceptance
```

R3/R4 audits and their independent fixes may be reviewed before R2 is available. R5 harness construction/negative tests may start from a reviewed interface proposal; rerun against the approved final adapter before positive acceptance. R6 environment preparation/runbook design need not wait for implementation, but live mutations must.

**Do not give one persistent worker the whole roadmap and require it to finish before any reviewer runs.** That caused the previous run to reach an owner decision with zero reviews. Launch one resolved, bounded slice at a time; review its actual source before moving on. An unavailable later gate is documented, not a recurring generic intervention. Never automatically retry/relaunch a terminal workflow.

## 7. Crosswalk: no original obligation silently dropped

The original [acceptance matrix](imported/ACCEPTANCE-MATRIX.md) remains the detailed obligation list. Existing implementation may satisfy a requirement after review/tests; a `not_started` historical label is not an instruction to rewrite working code.

| Original work | Current disposition / completion path |
|---|---|
| F00 baseline/provenance | Approved current-worktree mode; fresh inventories and R0/R8 full scope review, not forced standalone verification |
| F01 safe/portable harness | Partial implementation; R0 checks CI changes, R7 closes the remaining portable/isolation/mode assertions |
| F02 parser | Implemented/tested; R0 independent review, retain caller regression coverage through R3–R5 |
| F03 lifecycle | Native arm64/amd64 synthetic CI green; R0 assertion review, R2/R3 ownership integration, R7 remaining cells/stress |
| F04 authority | R1/R2 plus real qualification R6; stays unavailable until genuinely implemented |
| F05 writers / DR-2 | R3, with final source/support review R8 |
| F06 journal/reprint/IPC/recovery | R3 and R5/R6 end-to-end evidence |
| F07 exact release/readiness | R4 and R5/R7 native/extracted/browser evidence |
| F08 resolver/menu/consent | R3 writer admission and R5 user entrypoints/legacy compatibility |
| F09 credentials/update | R4 and R5/R6 preservation journeys |
| F10/F11 Layer 1/browser | R5, exact-artifact validator/negative tests in R7 |
| F12 authorized real tailnet | R6, supported distribution/architecture accounting in R7 |
| F13 docs/final readiness | R8, not inferred from the already-green CI jobs |

## 8. Verification for this planning change

Only repository/status/source inspection, tool-version inventory and planning-file validation are performed while writing this roadmap. No implementation, live Tailscale commands, tests that launch services, commits, pushes or workflow launches are part of this task.

Validate local Markdown links, the R0–R8/F00–F13 crosswalk, unchanged imported checksums and changed-file scope. These checks do not approve the architecture or product. The next execution is the bounded source/design qualification in [R1-OWNERSHIP-DESIGN.md](R1-OWNERSHIP-DESIGN.md); R0 is already approved.

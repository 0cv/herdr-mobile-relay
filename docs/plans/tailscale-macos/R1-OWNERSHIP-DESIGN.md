# R1 — Qualify the foreground Serve ownership design

**Next bounded run: design/source research and independent review only. No product implementation or live Tailscale activity.**

R1 is outlined in [the roadmap](REMAINING-IMPLEMENTATION-PLAN.md); this file is its executable assignment. It follows completed R0 and stops with a reviewed, evidence-backed recommendation for the user. **Do not expand it into R2–R8.**

## 1. Deliverable and stopping rule

Answer: **How can this relay invocation independently prove that it owns exactly one foreground Tailscale Serve session, and retire only that session safely on Linux and macOS?**

Deliver an updated ADR, a source-evidence inventory, and a short owner decision brief. The recommendation may be feasible, feasible only for specific distributions, or unsupported by the evidence. Do not predetermine LocalAPI success.

R1 has two distinct gates:

- **R1-A — This run:** source research, concrete design/rejected alternatives, test specification, and independent design review. Hand the draft to reviewers even though the owner's choice is still pending.
- **R1-B — After this run:** the user explicitly selects the proposed API/version/distribution/dependency scope. Record that choice before a separately launched R2 implementation.

A clean controller result approves the **quality and scope of the design recommendation**, not API mutation, production readiness, or runtime compatibility. Do not exhaust human interventions asking for an owner decision before allowing source review. The user is the owner; no unidentified additional decision maker must be located.

## 2. Current baseline and settled prerequisites

- Workspace: `/Users/christophe.vidal/.herdr/worktrees/herdr-mobile-relay/ci-tailscale-native-preflight-ef9f843` — the existing **user-approved linked worktree**.
- Branch: `ci/tailscale-native-preflight-ef9f843`.
- Starting source: `d2e5d01075a87fc43a6c9c5b78cb06ac4a17b7b5`.
- R0 workflow `3b823169-0686-44cd-97d5-6d842a8fe95e` completed **approved**, with two clean independent reviews and `finalRevisionReviewed=true`.
- Exact-revision [check run 36032804212](https://github.com/0cv/herdr-mobile-relay/actions/runs/36032804212) and [native preflight 36032804006](https://github.com/0cv/herdr-mobile-relay/actions/runs/36032804006) passed. These do not establish real Tailscale compatibility.
- [R0 worker evidence](execution/CI-REVIEW.md) is a **pre-review snapshot** whose pending-review language was superseded by the controller's final verdicts. Do not reopen R0 just because that snapshot says pending.
- Go 1.27.1 is installed; no Go build, tool provisioning, new clone or native runner is required to write/review this design.
- Tailscale source contract: **1.102.4**, upstream commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`. This is an exact existing source target, not a claim that the user's installed distribution/version has been qualified.
- `Inspect` still returns `ServeRouteOwned=false`. That deliberate production refusal stays unchanged throughout R1.

Before edits, record canonical cwd, HEAD/branch, index and expanded dirty-file inventory. Preserve the existing untracked `docs/plans/` tree. If HEAD changed, identify the delta; never reset, stash or discard it to recreate this baseline. The old standalone-import verifier is not applicable to the approved current-worktree execution mode.

## 3. Editable outputs and prohibited effects

Only these documentation outputs may be created/edited in this run:

1. `internal/tailscale/DR-1-session-authority.md` — extend the existing draft rather than create a competing ADR.
2. `docs/plans/tailscale-macos/execution/R1-DESIGN-REVIEW.md` — decision brief, feasibility/support matrix, review/evidence boundaries and R2 handoff.
3. `docs/plans/tailscale-macos/execution/R1-SOURCE-EVIDENCE.json` — reproducible upstream/local source citations and open evidence gaps.

Read relevant repository files freely, but do not modify Go/shell/frontend source, tests, dependencies, workflows, manifests, the imported handoff or earlier execution reports. Documentation pseudocode is allowed; executable prototypes and new production hooks are not.

Public-source research/downloads to a private temporary directory **outside the checkout** are allowed. Fetch immutable source references and inspect them as data; do not execute downloaded code, build Tailscale, install packages, or access personal credential/config directories. Store hashes/citations in the evidence file, not unnecessary upstream source dumps in the repo.

No Tailscale CLI commands or LocalAPI connections, including exploratory reads against the personal daemon; no login, Serve/Funnel mutations, tailnet/ACL/DNS/HTTPS administration, services, sockets/process lifecycle experiments or trust-store changes. No broad Make/test targets. No commits, pushes, CI dispatch, PR or release is needed for this documentation-only slice; return the scoped artifacts for review. Broader prior CI authorization is not an instruction to publish this design prematurely.

Do not edit/rewrite the orchestration controller or relaunch an old workflow. Only explicitly missing **source evidence essential to the design** can block R1-A; unavailable live credentials/runners are later runtime prerequisites.

## 4. Read the real integration boundaries first

Start with:

- `internal/tailscale/{CONTRACT.md,DR-1-session-authority.md,tailscale.go,contract.go}` and their tests.
- `relay/tailscale.sh`, `relay/setup-link.sh` and relevant helpers in `relay/common.sh`.
- `cmd/herdr-mobile-relay/{main.go,managed_reprint.go,managed_recover.go,supervise.go}`.
- `internal/app/{managed_owner.go,managed_udp.go,server.go}`, `internal/localcontrol`, `internal/managedstate` and `internal/processsupervisor` at the relevant authority/lifetime boundaries.

Draw the **current** path: shell launcher → supervised external `tailscale serve` → private CLI watcher/session → read-only inspector → startup/reprint/monitor/cleanup consumers. Show where independently supplied authority is absent. Distinguish existing local managed-root ownership from ownership of a daemon-side Serve session; one does not prove the other.

Retain the R0 parser/CLI contract. Do not claim every lifecycle, transaction or helper is already qualified simply because CI is green.

## 5. Source questions that must be answered

Use `CONTRACT.md`'s immutable upstream links as starting points, not as independent proof. Follow definitions and callers into client transport, LocalAPI authorization, daemon mutation and session cleanup. Record exact paths, symbols and passages at pinned commits.

| Question | Required evidence / output |
|---|---|
| Session authority | Trace watcher creation, server-side session allocation, notification delivery and foreground registration. Explain precisely what binds a newly received session to this client's held connection. Is the session ID merely visible metadata, a capability, or neither? |
| Mutation permissions | Trace which local user/process may watch, read Serve config and mutate it. Identify authentication/authorization checks, not just successful HTTP method signatures. Do not imply resistance to arbitrary same-UID compromise. |
| Linux and macOS access | Identify daemon/socket/transport discovery and non-root access for Linux `tailscaled`, macOS standalone app, Mac App Store variant, and any separately claimed CLI/daemon distribution. Mark unsupported or uninspectable variants; do not assume Linux socket paths or GUI-private credentials are portable. |
| Conditional mutation | Follow `GetServeConfig` ETag and `SetServeConfig` `If-Match` through actual daemon enforcement. Specify absent/stale ETag behavior, concurrent writers and atomicity. What happens if a watcher disappears between read and mutation? |
| Foreground retirement | Follow watcher close, child/parent exit, daemon restart/logout and session-map cleanup/filtering. Distinguish guaranteed behavior, eventual behavior with no deadline, and runtime hypotheses. Never convert a finite local timeout into proof that the daemon has cleaned up. |
| HTTPS and identity | Determine source-backed HTTPS prerequisites, interactive-enablement behavior to avoid, DNS/certificate identity and node/account/tailnet/version checks. List available fields and their limits; do not invent a stable daemon epoch or account UUID absent from the source. |
| Output and cancellation | Specify bounds on messages, nesting, pending I/O, cancellation and long-lived watch handling, reusing current constraints where appropriate. Account for partial output and a daemon that stops responding. |
| Version and artifacts | Identify exact qualified source/ABI targets, CLI versus daemon version checks, supported distribution assumptions and artifact verification requirements. A newer upstream alternative must name its own immutable commit and be a separate proposed scope change. |

The public macOS implementation may not expose every distribution-specific mechanism. If essential code/evidence is unavailable, explicitly mark that cell **unproven** and exclude it from unconditional feasibility claims. Missing installed-version details can be listed as an operator prerequisite; they are not a reason to ask for personal credentials or perform live probing in R1.

## 6. Compare concrete options

Evaluate these alternatives against the same requirements:

| Option | Required disposition |
|---|---|
| Existing unmodified external CLI | Determine whether any documented, qualified parent-bound output/control contract exists. A printed URL, PID, new-route diff or an observed session ID alone is insufficient. |
| **Relay-owned version-pinned LocalAPI watch/session** | Preferred candidate to investigate, not an already approved solution. Specify the exact supported transport, non-root permissions, owned lifetime and conditional mutation/cleanup behavior per distribution. |
| A newer upstream CLI/API contract | Only if a concrete source-backed mechanism exists; state the version/support change explicitly, with no automatic upgrade. |
| Custom CLI distribution | State maintenance, packaging, signing/update and compatibility burden; do not select this silently because a prototype would be convenient. |
| Keep the feature unavailable | Honest fallback where authority cannot be proved or the owner rejects the needed scope. Does not invalidate completed CI work. |

Compare a pinned upstream client dependency with a narrow standard-library transport. Report required modules/transitive footprint, licensing, Go 1.27.1/build compatibility, macOS transport reuse and long-term API maintenance. No speculative dependency size claim without evidence, and no `go get`/`go mod tidy` or dependency change in this run.

Recommend **one** option with supported cells, risks and rejected alternatives. If no usable Mac path is substantiated, say that plainly; a Linux-only design is not an answer to the user's Mac goal without a separately approved support change.

## 7. Specify the proposed interface and state machine

Write a concrete design, not just “use LocalAPI.” All names below are conceptual requirements, not claims of existing APIs:

- An owned session/lifetime handle originates from a newly held, authenticated connection. Read-only observed status remains separate from independently supplied expected authority.
- Identify whether the relay process or a supervised helper holds that handle, its lifetime relationship to the shell/relay, and how startup, reprint and monitoring query it over a private bounded interface. Justify the choice against existing O/T and local-control ownership.
- A fresh `inspect` invocation cannot authorize pairing by receiving an arbitrary session string or environment `owned=true` flag. Stale journal/session metadata is evidence to inspect, not a reconnectable capability.
- Describe prepare → consent → revalidate → acquire authority → conditional route registration → actual readiness → arm/acknowledge → monitor → close/verify. Reconcile ordering with existing owner acquisition/journaling; do not create circular readiness or lock dependencies.
- Specify numeric local deadlines/output bounds and error classifications. For each transition, list permitted mutations, rollback versus retained evidence, and the next actor. No blind retry following an ambiguous mutation or lost acknowledgement.
- On watch loss, daemon/node identity change or unknown status, invalidate readiness/arming immediately. Explain how the relay stops or retains evidence without adopting a replacement session.
- Preserve foreign routes/state during cleanup. Observing a foreign replacement must never authorize deleting it; an unrelated route appearing must not require a global empty-config reset to call owned retirement successful.

Include a short trust/threat-model section: cooperating local writers versus malicious same-UID replacement, local transport authentication, what is and is not secret, and assumptions shared with pending DR-2. Any stronger guarantee or narrower support promise is an explicit owner decision, not an unstated design shortcut.

## 8. Specify the R2 tests; do not run them here

Map each test to a production interface/caller and required assertion. Distinguish source-derived fake contracts from later native/real-daemon qualification.

Minimum cases:

1. A session from the newly owned watch can register only the exact HTTPS host/port, `/` proxy and loopback backend for this run.
2. A matching observed URL/backend, borrowed session ID, PID or healthy relay cannot substitute for owned authority.
3. Wrong/missing/stale ETag and a concurrent foreign write reject mutation without overwriting foreign configuration.
4. Watch closure before or during mutation, dropped mutation acknowledgement, malformed/oversized/partial responses and timeout refuse readiness and preserve necessary evidence.
5. Same-origin replacement, changed route tuple, extra routes, background/Funnel state, logout and node/account/tailnet/version drift never authorize pairing or foreign cleanup.
6. Parent/helper/relay death, cancellation, held output pipes and daemon restart retire only proven owned resources, or remain explicitly unverified. No unconditional PID signalling or global Serve reset.
7. Reprint proves the live session/run/release again, survives serialization with T under O, and prints nothing on lost arm acknowledgement or stale local-control responses.
8. Separate distribution/permission incompatibility fails before mutation with actionable guidance.
9. Positive/negative tests exercise the actual planned production adapter, not only a fake that emits `serve_route_owned=true`.

Include at least one negative-control mutation for the authority binding, conditional update and cleanup-isolation assertions. Explain what the fake daemon can establish, what native OS tests add and what only a real pinned daemon/remote browser can prove. Live tests remain R6 work with separate authorization.

## 9. Artifact content and evidence discipline

### Updated ADR

Keep `internal/tailscale/DR-1-session-authority.md` clearly marked **proposed / awaiting owner selection** until the user decides. Include:

- Problem and explicit non-goals.
- Source-qualified facts, platform/distribution matrix and evidence gaps.
- Options, recommendation, permissions, dependency/version scope and security tradeoffs.
- Owned-lifetime interface/state machine and failure semantics.
- R2 test/implementation boundaries and R6 runtime qualification requirements.
- An exact owner decision question. Do not prefill consent or label runtime behavior verified from source alone.

### Source evidence JSON

Use a simple documented schema containing repository/commit, path/symbol, immutable URL where applicable, content SHA-256 for downloaded files, relevant passage/line range, supported claim, and qualification level (`source_observed`, `inferred`, `unverified_runtime`, or `unavailable`). Include local source HEAD and input hashes. A checksum is consistency evidence, not a signature or runtime proof. Do not use mutable branch links as the sole source authority.

### Decision brief

The `execution/R1-DESIGN-REVIEW.md` brief must let the user decide without reconstructing a source dump:

1. Recommended option in plain language, especially whether it works with the intended Mac distribution or needs a support change.
2. Exact new mutation/dependency/version/privilege scope requested and what remains prohibited.
3. Source-backed feasibility matrix, residual risks and blockers that must be resolved before R2.
4. Proposed R2 allowlist and ordered substeps; no implementation begins automatically.
5. A short approval statement the user can accept or amend, naming the actual option/versions/distributions/dependency—not generic “approve F04.”

Keep outcomes separate: design review, owner authorization, runtime qualification and feature readiness. Worker-authored documents must not claim independent verdicts that have not occurred. Reference the actual controller artifacts for final reviewer results; do not mistake a pre-review snapshot for a post-review acceptance record.

## 10. Independent review and completion

Use independent reviewers with complementary focuses:

- **Authority/security:** trace original upstream source, binding, authorization, conditional mutation, failure/cleanup isolation, and rejected circular proofs.
- **Platform/integration:** macOS distribution access, dependency/packaging feasibility, existing launcher/O/T/reprint integration, bounds, and realistic tests/support claims.

Reviewers inspect the cited source and actual ADR changes, not only the summary. They must distinguish a well-founded limitation from a missing essential proof. A recommendation may be clean as a decision document while its unsupported cells remain excluded; it cannot claim those cells are implementation-ready.

Validate links/source hashes, evidence JSON, documentation diff/whitespace, unchanged protected imported checksums and the exact editable file set. Runtime tests are **not applicable to this documentation-only slice**; the existing green CI is baseline context, not evidence for the proposed adapter.

Return the reviewed recommendation and artifact paths. Keep `ownerApproved=false`, `runtimeQualified=false`, `featureReady=false`, and R2 unstarted until the explicit later decision and implementation. If a concrete source-access problem prevents essential conclusions, record the missing evidence and bounded next action; do not request unrelated tool installs, a new checkout or a live tailnet merely to finish this phase.

**Stop after R1-A.** The user reviews the concrete recommendation and chooses its scope in R1-B. Do not automatically start R2, retry a terminal workflow, or treat design-document approval as permission to mutate Tailscale.

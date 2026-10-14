# Plan: CLI-backed Tailscale Serve for background relays and isolated development

## Status and decision

**Status: revised implementation handoff; not implemented or independently reviewed.**

Written against Herdr Mobile Relay commit
`85ed16497ec1e9fed2dbfb60b89b7fc8400f2acb` on
`ci/tailscale-native-preflight-ef9f843`.

The selected direction is a **new CLI-backed, persistent Tailscale Serve
transport**, using the installed Tailscale CLI to communicate with its daemon.
The installed relay must run as a normal background user service; no open
terminal pane is required. Development uses the same transport implementation
with independent state, ports, credentials, and route registration.

The primary missing platform is **macOS with the approved App Store Tailscale
application**, without installing another extension or replacing that app.
Company policy, existing authentication, and tailnet HTTPS/access permissions
remain prerequisites; this plan is not a way around device management.

This is a deliberate lifecycle change, not a version-check bypass. Preserve the
current direct-LocalAPI temporary-session mode and pure operator-owned/BYO mode
as separate transports. Do not silently convert either one's state or promises.
No claim of App Store runtime support is made until the qualification below passes.

## Workflow execution scope (read before starting)

The next explicitly launched implementation workflow owns **P0–P5**: draft the
contract, implement against it, test in disposable hosted fixtures, and obtain
independent cumulative review. **P0 is work for the worker, not a pre-existing
owner-supplied contract or human-approval prerequisite.** Missing design artifacts
are deliverables to create from public sources and repository contracts, not a
reason to ask the user to write them. Reviewers must review those artifacts along
with the implementation through the supplied controller; do not require an extra
controller, pre-cleared contract, or preliminary human checkpoint to begin P1.

Separate three decisions:

1. **Implementation scope:** develop the chosen persistent CLI approach, including
   its documented limitations, consent UI, recovery, and fail-closed gates. This
   can proceed without contacting any real daemon or accepting deployment risks
   on the user's behalf.
2. **Automated approval:** P0–P5 artifacts, hosted evidence, and independent reviews
   can establish implementation completion only. Do not hide missing evidence or
   upgrade a blocked/capped controller result to approval.
3. **Live enablement and release:** P6 is a separately authorized follow-up. Real
   mutations require explicit operator risk acceptance and scoped live permission;
   this plan revision supplies neither. Until then, new CLI profiles remain
   runtime-unqualified and unavailable for ordinary production activation. Build
   and test the full implementation behind that gate; fixture adapters are
   test-only, not shipped endpoint/profile overrides or a bypass switch.

The worker should draft a proposed risk decision/runbook during P0 and mark
owner acceptance **pending live enablement**, not accepted or a prerequisite for
writing code. The existing direct-session and BYO paths remain unchanged.
Unavailable App Store hardware, tailnet credentials, phone access, or live consent
is a **deferred P6 cell**, not a P0–P5 blocker. A real contradiction in the safety
requirements, unavailable essential source evidence, or a necessary scope change
still warrants a precise blocker; do not fabricate a contract to avoid escalation.

If a human decision is genuinely needed, present the smallest concrete question
and leave it awaiting the user's guidance; the supervising assistant must not
invent approval or reject it merely because authorization has not yet arrived.
This does not change the controller or override an actual user rejection.

The previous run `ead9163c-57df-472f-8284-c8dcbc1aea5d` remains **blocked, not
approved**. This revision fixes its plan-handoff prerequisite, not its historical
result, and is not an instruction to retry, resume, or relaunch that run.

## 1. Product outcomes

### Installed relay

1. Choose **Tailscale Serve (background service)** in setup.
2. Setup locates a usable installed CLI, performs bounded read-only checks, and
   shows the selected node, HTTPS listener, loopback backend, and persistence
   policy before asking for permission to create the route.
3. Setup starts/configures the relay's user service, publishes only the approved
   route, verifies HTTPS and the exact phone bundle, and offers pairing.
4. Close the setup pane: the relay continues running. At later user logins it
   starts without a terminal, shell profile, administrator prompt, or new QR.
5. Restart/update preserves enrolled devices and the selected HTTPS origin.
   Status, logs, setup-link reprint, stop, and explicit unpublish are available.

On macOS this means a **per-user LaunchAgent after login**, not an unsupported
promise of operation before login or while the App Store VPN is unavailable.
On Linux use the existing systemd user-service model; do not enable linger or
change system services without a separate operator decision.

### Development

- `make dev-tunnel` option 2 becomes an explicitly labelled **CLI-backed
  Tailscale Serve development** choice after this path is qualified.
- It uses the same implementation and compatibility checks as the release,
  not a mock, unsupported fallback, or relaxed security mode.
- The installed production relay remains running. Default development backend,
  plugin, and HTTPS ports are `18377`, `18378`, and `8443`, respectively; conflicts
  are refused, not overwritten or silently reassigned.
- Development stays foreground for useful build/log feedback. That is a UX
  choice, not a requirement of Tailscale or the installed transport.
- A phone can pair to the development instance without replacing production
  credentials. Different HTTPS ports form distinct browser origins; expose that
  distinction clearly in the setup UI and verify it with physical phones.

## 2. Reference behavior and limits

Collie was inspected at
[`08325fad245e4006b709948d7d3dbe301b6791fa`](https://github.com/AltanS/collie/tree/08325fad245e4006b709948d7d3dbe301b6791fa):

- [`cli/serve.ts`](https://github.com/AltanS/collie/blob/08325fad245e4006b709948d7d3dbe301b6791fa/cli/serve.ts)
  invokes the installed CLI, normally equivalent to
  `tailscale serve --bg --set-path=/ 8787`, records its mapping, and can adopt an
  existing background mapping whose target matches its own backend.
- [`bridge/front-door.ts`](https://github.com/AltanS/collie/blob/08325fad245e4006b709948d7d3dbe301b6791fa/bridge/front-door.ts)
  checks the observed mapping against its record before issuing a scoped `off`.
- [`docs/install.md`](https://github.com/AltanS/collie/blob/08325fad245e4006b709948d7d3dbe301b6791fa/docs/install.md)
  describes a background bridge managed by launchd/systemd.

Borrow **CLI delegation and background-service usability**, not all of Collie's
ownership, adoption, authorization, or recovery assumptions. Its default is
CLI-managed ingress, not pure BYO ingress. This source inspection does not prove
Collie works on this particular managed Mac.

[Tailscale's Serve documentation](https://tailscale.com/docs/reference/examples/serve)
explicitly permits App Store/macOS GUI builds to share local ports; their sandbox
restriction concerns serving files/directories directly. Herdr serves its own
frontend over loopback HTTP, so port proxying is the relevant capability.
The [CLI documentation](https://tailscale.com/docs/reference/tailscale-cli?tab=macos)
describes the CLI bundled in the App Store application.

Upstream's
[Darwin connection code](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/safesocket/safesocket_darwin.go)
explains platform-specific authentication. **The new Herdr transport delegates
that work to the installed CLI**; it does not scrape same-user-proof tokens,
scan processes with `lsof`, use XPC itself, or call LocalAPI directly.

CLI versus LocalAPI and persistent versus temporary routes are separate choices.
The CLI also supports foreground Serve. This plan deliberately chooses background
persistent Serve and does not imply LocalAPI inherently requires a terminal.

## 3. Scope and compatibility boundaries

### Transport identity

Add a distinct configuration value, provisionally `tailscale-cli`:

| Value | Authority and lifecycle | Service behavior |
| --- | --- | --- |
| `tailscale-cli` (new) | CLI-managed persistent route with a private registration journal | Background service or isolated foreground development |
| `tailscale` (existing) | Direct LocalAPI, in-process temporary session, conditional retirement | Existing foreground-only implementation unchanged |
| `tailscale-external` (existing) | Operator owns all ingress; Herdr never changes it | Existing BYO contract unchanged in this delivery |

Do not fake `SessionAuthority`, set `ServeRouteOwned=true` from observations, or
relax the existing adapter's version/ownership tests to implement the new mode.
Pure BYO must continue to make **zero** Tailscale CLI/LocalAPI calls.

### Platform qualification

- First required target: the user's reported App Store **1.102.4** profile.
  Reported metadata includes `osVariant=appstore`, matching CLI/daemon long
  strings, `gitCommit=3caf7d9e7dcaba589cfc58beda596929733e4fea`, supplemental
  commit `084ee3b64537a1276e56fc38cdf0a711da9f4936`, and capability version `142`.
  This is supplied metadata, not an independently verified artifact or live test.
- Initial Linux implementation candidate: upstream Tailscale v1.102.4 on
  Linux/amd64 with its Unix daemon. P0 must document the exact public source and
  candidate distribution/artifact metadata it can substantiate. Use a clearly
  labelled synthetic profile for fixture coverage where binary evidence is
  unavailable; do not invent an official build string. Additional architectures
  and real Linux qualification remain separate P6 cells.
- MacSys GUI, other versions, architectures, and packaging channels require their
  own recorded evidence; do not inherit runtime approval from a source fixture.
- Create a separate CLI compatibility policy based on documented commands,
  structured schemas, and tested builds. Do not reuse the direct adapter's
  upstream-commit-equality rule: a distribution's build stamp can differ.
- Track each profile separately as source-described, fixture-tested, and
  real-runtime-qualified. App Store 1.102.4 and the Linux candidate are initially
  implementation/test targets, not production-approved profiles. MacSys and all
  other versions/channels remain unsupported in this scope. Review public source
  schemas and construct synthetic fixtures without requiring live captures.
- Production activation requires separately recorded live qualification and the
  enablement decision. Until then, implement and test profile recognition and the
  refusal path as well as the full test-injected lifecycle; do not make a working
  personal daemon a prerequisite for P0–P5. P6's activation change must receive its
  own exact-revision regression checks/review, not bypass this gate via an env flag.
- Add reviewed compatible version ranges only with evidence; neither accept every
  future build nor promise every Tailscale update will be transparent. Unsupported
  upgrades produce actionable diagnostics and no configuration writes, not a
  request to downgrade a managed computer without authorization.

### Out of scope

Installing/logging into Tailscale; changing MDM, VPN, permissions, operator UID,
HTTPS/tailnet policy, DNS, or Funnel; public/plain-HTTP fallback; remote gateway
federation; replacing device authorization; new cryptography; and rewriting the
old LocalAPI adapter. No new runtime dependency is expected: use Go's existing
process/JSON support and the operator's installed CLI.

## 4. Architecture

### 4.1 Bounded CLI adapter

Implement a testable Go adapter (suggested package `internal/tailscalecli`),
separate from `internal/tailscale`'s session authority. It owns subprocess
execution, structured parsing, and result classification; shell scripts provide
UX and dispatch, not ad hoc JSON parsing or security decisions.

- Resolve an explicit absolute override first, then verified executable candidates
  from absolute PATH entries and the approved App Store application bundle.
  An alias/function in an interactive shell is not sufficient for launchd.
  Refuse ambiguous candidates rather than guessing which daemon was selected.
- Persist the selected executable/profile and revalidate before use. Handle normal
  app updates with compatibility checks, not an immutable binary hash that breaks
  every update. Reject unexpected path/profile changes without new approval.
- Use fixed argument arrays, no shell evaluation, `sudo`, or downloaded helper.
  Give commands deadlines, bounded stdout/stderr, deterministic exit handling,
  cancellation, and bounded child/pipe cleanup. Never log raw private status or
  credential-bearing output.
- Check App Store CLI invocation from a noninteractive LaunchAgent environment:
  it must enter CLI mode rather than open a GUI or wait on an invisible prompt.
  Preserve only required user/session context. Do not indiscriminately replace
  HOME for this CLI, or inherit arbitrary shell/environment overrides.
- Read-only preflight uses structured status, version, and Serve status; require
  authenticated node identity, compatible CLI/daemon metadata, canonical HTTPS
  naming, and supported complete configuration. CLI `osVariant` alone is not
  proof of the daemon's identity.
- Reject duplicate/case-conflicting keys, malformed/truncated/oversized JSON,
  unknown exposure semantics, and incomplete inspection. Sanitize errors into
  profile unavailable, logged out, permission denied, conflict, unsupported, or
  uncertain outcome rather than reporting all failures as a version mismatch.
- Only explicitly approved publication/removal methods may mutate Serve. A typical
  publication is `serve --bg --https=<port> --set-path=/ http://127.0.0.1:<backend>`;
  qualification must establish the exact argv/schema for each supported profile.
  Never invoke `serve reset`, `set-raw`, `funnel`, `up`, `login`, or privilege changes.

### 4.2 Registration manager and private state

Use a schema-versioned, atomically persisted journal with restrictive modes,
writer locking, and explicit recovery states. Record installation identity,
production/development scope, selected node/profile, host/listener/mount/backend,
consent scope, operation ID, and acknowledged publication/removal results.
Keep account/node details private; never store Tailscale authentication tokens.

A registration record is **operational attribution**, not an unforgeable ownership
capability. Route, URL, port, PID, or matching response alone never authorizes
adopting an unrecorded mapping. A restored/copied/corrupt record requires explicit
reconciliation, not automatic takeover.

Serialize mutations across cooperating Herdr production/development processes
for the same user/node, not just within each state root. A small private common
coordination lock/port reservation registry may be shared; device stores, secrets,
web builds, service state, and registration journals remain separate. Host-wide
coordination does not lock out arbitrary external Tailscale writers or other UIDs.

### 4.3 Application admission and frontend integrity

Add a distinct CLI readiness/admission path in the app and private local control
API. Reuse reviewed device-store and HTTPS/bundle verification primitives without
weakening the existing managed watch-bound gate.

- Start with the backend bound to loopback and sensitive app/WS admission closed;
  expose only the bounded health/static endpoints needed for verification.
- Verify the effective HTTPS origin with normal OS certificate/hostname trust,
  current instance/run/transport/release identity, and the complete selected
  frontend bundle before arming an invitation or admitting application traffic.
- E2EE, per-device controller/reader roles, revocation, durable one-use invitations,
  and credential reuse remain mandatory. Never infer authorization from tailnet
  membership or identity headers. Never reset devices during restart/rebuild.
- Keep invitation generation/reprint as authenticated operator actions. Background
  service logs must never contain tokens, QR fragments, session credentials, or
  a newly armed invitation on every restart.
- On detected route/identity drift, close admission and active relay sessions as
  appropriate, retain devices, and report degraded status; do not auto-repair.
  Specify bounded monitoring intervals and test detection latency. Polling is not
  instantaneous revocation and must not be described as a live LocalAPI watch.

## 5. Route lifecycle and honest safety contract

### Operations

| Event | Required behavior |
| --- | --- |
| First setup | Read-only preflight; bind/gate backend; show exact route and persistence; get consent; journal intent; issue at most one publish; durably record its result; inspect/read back and commit the registration; verify HTTPS/bundle; only then offer pairing. |
| Existing exact acknowledged registration | Validate same installation/node/route and live backend; resume without another Serve write. This is reuse of its record, not adoption based on target matching. |
| Matching route without a valid record | Refuse takeover. Explain BYO/manual reconciliation; do not fabricate a receipt. |
| Unrelated routes on other ports | Preserve them. Allow only if complete qualified schema proves no listener/mount/protocol conflict. Production and dev must pass together. |
| Conflict on selected listener | Refuse before mutation; initially reserve a dedicated HTTPS listener per instance instead of sharing mounts on one port. |
| Routine stop/restart/update | Keep the persistent route and registration. Stop/restart only the relay; do not issue `off` then republish on every restart. State clearly that ingress remains configured. |
| Route disappeared after successful setup | Mark degraded; no automatic recreation. Explicit repair can republish after fresh preflight/consent. An operator's manual removal must not be silently undone. |
| CLI timeout, killed child, or ambiguous failure after dispatch | Preserve `publish-uncertain`/`remove-uncertain`, close admission, and do not retry automatically. A subsequent matching or absent observation does not retrospectively establish acknowledgement. |
| Tailscale temporarily unavailable / login session not ready | Service waits/retries bounded read-only readiness checks with backoff. No login/start/permission changes or mutation loop. |
| Daemon/node identity or app profile changed | Suspend admission; fresh inspection and explicit reconciliation before changing routes or origin. Preserve device state. |
| Explicit unpublish | Show scope; require consent and valid acknowledged registration; re-inspect; remove only exact unchanged listener/mount; verify readback; retain evidence on mismatch/uncertainty. |
| Uninstall | Offer remove-route or explicitly leave-route choices. Do not erase the only registration/recovery record while removal is unconfirmed. Unattended uninstall without an authorized choice fails with guidance. |
| Transport switch | Explicitly decide what happens to the prior persistent route. Do not let it keep pointing at a backend repurposed for another transport; unresolved removal blocks backend repurposing. |

Proposed persisted states: `unconfigured`, `publish-pending`, `registered`,
`publish-uncertain`, `remove-pending`, `remove-uncertain`, and `removed`.
Runtime `ready`, `waiting`, and `conflicted` status is computed separately;
a persisted `registered` state is never sufficient proof of current readiness.
Write/fsync failures before dispatch prevent mutation; failures after dispatch
are unresolved. No exit code after dispatch alone proves that no mutation occurred.
Persist the acknowledged mutation result before waiting on remote HTTPS checks;
a frontend/TLS verification failure must not lose the publication receipt or
cause the next start to republish. Registration and application admission are
separate commits; failed readiness leaves the registered route visible in status
but application admission closed.

### Design limits; operator acceptance required before real mutations

1. **CLI check-to-write race:** a CLI invocation does not expose Herdr's current
   in-process ownership capability or an externally supplied atomic comparison
   against our earlier snapshot. Even if Tailscale internally uses conditional
   writes, another writer can change the selected mapping between our inspection
   and the CLI's own read. Scoped commands, fresh inspection, and Herdr locks
   reduce risk but do not prove preservation against noncooperating writers.
   Implement and test against this explicit design limit. Require an operator
   risk decision before live enablement, not before coding the mutation path or
   exercising it with a fake CLI. Never promise atomic ownership or guaranteed
   preservation of a concurrently replaced same route.
2. **Persistent route and port reuse:** after the relay stops/crashes, Serve can
   still forward to that loopback port. Another local process binding it could
   receive traffic or serve a different frontend. Reserve ports among Herdr
   instances, verify instance/bundle before pairing, and document this residual
   risk. E2EE does not authenticate replacement frontend JavaScript. Do not claim
   the old session-owned anti-port-reuse lifecycle is retained.
3. **No automatic global rollback:** failures retain an explicit degraded/recovery
   state. Never reset the daemon or restore an old whole Serve snapshot over
   someone else's changes. A route can remain configured after failed setup.
4. **No remote-drain guarantee:** successful scoped removal/readback is not proof
   that existing connections have drained or all remote traffic ceased.

Document all four limits in the proposed P0 contract, runtime consent, and
recovery guidance. Human acceptance remains pending until live enablement; the
worker may implement the bounded behavior and tests without recording that
acceptance. If the operator later rejects these limits, do not enable this mode;
offer BYO or separately qualified direct-session integration instead. Do not
hide a new guarantee inside an old mode name. Explicit unpublish should advise
against concurrent Serve changes.

## 6. Background service, updates, and development integration

### Background service

The current service wrapper and installers assume Cloudflare and refuse both
Tailscale transports. This is a real integration task, not just deleting a guard.

- Dispatch by explicit transport before Cloudflare config/tool checks. The new
  path must require neither `cloudflared` nor a Cloudflare YAML/account.
- Keep the existing production service label; use the packaged Go supervisor or
  direct `exec` so signals and exit status reach the owned relay correctly.
- Persist CLI path, relay state, Herdr executable/socket selection, and stable
  origin without relying on interactive shell startup files. Quote/escape plist,
  systemd, and environment values; handle paths containing spaces.
- Setup and service coordinate via one instance lock/private control API. Only
  one process owns the backend and device-store writer; setup attaches to it
  rather than launching a duplicate server.
- A completed service setup remains live after its pane closes. If setup fails
  after route/service changes, report each remaining resource and safe recovery;
  do not report success or destroy recovery evidence.
- Separate process liveness, local readiness, Tailscale availability, route
  registration, trusted HTTPS reachability, and pairing readiness in diagnostics.
- Distinguish transient unavailability from conflict/uncertainty. Avoid launchd
  or systemd restart storms and repeated Serve writes in either case.
- Integrate the verified updater/rollback path with persistent registrations and
  coherent binary/web bundles. Restarts do not rotate credentials, re-pair phones,
  or republish routes. Keep unsupported update actions explicitly disabled until
  their tests pass rather than removing the existing guard globally.

### Development

- Add an explicit `make dev-tailscale-cli` target and script. Retain
  `make dev-tailscale` / explicit `HERDR_DEV_TRANSPORT=tailscale` for the legacy
  session mode; introduce `HERDR_DEV_TRANSPORT=tailscale-cli` for automation.
  Update menu option 2's label/help so its new meaning is visible, not a silent
  fallback after the old mode refuses a profile.
- Use a new private default `relay/.dev-tailscale-cli/` (0700), rather than
  reinterpreting existing `.dev-tailscale/` owner/recovery files. Preserve old
  generated files; explicit migration is a later operator action if needed.
- Resolve executables and select the conventional Herdr socket without contacting
  services before opt-in. Build a coherent relay/frontend privately; retain
  explicit root, executable, socket, and port overrides and fail-closed checks.
- Keep relay state isolation separate from the CLI's real-user App Store session
  context. Do not use a synthetic HOME that breaks the approved app, and do not
  import production secrets into development to compensate.
- Verify nonoverlapping roots, separate credentials/control sockets/store locks,
  and distinct backend/plugin/HTTPS ports. Accommodate production's existing
  route on another port, but never touch it or its service definition.
- Default dev Ctrl-C stops only the dev relay and retains the persistent dev route,
  matching the installed lifecycle. Explain this in the initial consent and exit
  message. Provide an explicit dev unpublish action; do not imply temporary
  automatic cleanup or delete `.dev-tailscale-cli/` while a route is unresolved.
- A second checkout must use different ports/state or refuse. Cross-check the
  common reservation/registration metadata; do not reuse another checkout's
  matching route. Normal same-checkout reruns retain phone credentials and URL.

## 7. Migration and compatibility

- No automatic reinterpretation of `tailscale` as `tailscale-cli`, no automatic
  conversion from BYO to managed CLI ingress, and no deletion of old recovery
  records. Existing configurations continue to select their original contracts.
- New interactive setup advertises the background CLI mode as the normal
  Tailscale option only after its acceptance gates pass; keep the old mode
  explicitly labelled as advanced temporary/session-owned operation.
- A switch from an installed Cloudflare/gateway relay uses staged config and an
  explicit maintenance/restart decision. Preserve the old config and credentials;
  on failure restore it only when doing so cannot reuse a still-published new
  route's backend. Otherwise explain the recovery block.
- Stable instance/device identity survives intended transport changes; tell the
  operator when a new frontend origin requires a distinct PWA enrollment. Do not
  promise cross-origin browser credential migration that has not been implemented.
- Schema-version registration records and ensure older binaries cannot silently
  accept them during rollback. Keep old transport regression suites unchanged.

## 8. Implementation sequence and review gates

Each phase has a bounded deliverable and an evidence gate. For a subsequent
implementation invocation, P0–P5 are worker tasks; their contract and code are
reviewed cumulatively by the supplied implementation controller. No separate
pre-implementation human approval of the contract is required for this bounded
source/fixture scope. P6 is excluded from that invocation's completion criteria.
File names below are implementation proposals, not edits for this planning turn.

### P0 — Worker-authored contract and source/fixture profile design

**Deliver:** create `docs/tailscale-cli-contract.md` and
`docs/tailscale-cli-qualification.md` as new task-owned artifacts. They must contain:

- The lifecycle/state-transition table and section 5's four limits, with owner
  acceptance explicitly pending live enablement.
- Exact allowed read/publish/remove argv, parser schemas, and result semantics
  derived from pinned public source/docs; distinguish those sources from the
  supplied App Store metadata and from synthetic fixture data.
- Candidate App Store 1.102.4 and upstream Linux/amd64 v1.102.4 profiles, provenance
  and evidence gaps, and separate source/fixture/live qualification columns.
- Service identity/environment, admission/monitoring rules, default stop versus
  unpublish policy, journal recovery, and production activation refusal design.
- A proposed live-test runbook and precise future approval questions. The runbook
  is not authorized merely because it has been written.

**Gate to P1:** the worker has materialized a source-backed, internally consistent
contract with explicit unknowns and test expectations. Proceed to implement and
submit both documents and code to the controller's independent reviewers; do not
claim the contract is already reviewed. A source gap that prevents safe design
is a legitimate blocker, but absent live captures or owner acceptance alone is
not. Unproven noninteractive App Store behavior gets a hosted fixture and a P6
qualification case, not invented runtime evidence or an automatic stop. If source
research establishes that the approach cannot work within the stated constraints,
report that specific contradiction rather than changing the daemon or requiring
a permanent pane.

### P1 — CLI adapter and durable registration manager

**Likely files:** new `internal/tailscalecli/`, CLI subcommands under
`cmd/herdr-mobile-relay/`, private journal helpers/tests where appropriate.

**Deliver:** bounded execution, strict inspection, scoped publication/removal,
write-ahead receipts, shared mutation coordination, explicit recovery commands,
redacted diagnostics, and fake-CLI contracts. No service default changes yet.

**Gate:** refusal/uncertainty/concurrency tests pass in hosted environments; no
call reaches real tools through fixture PATH or inherited environment. No global
reset, guessed adoption, background repair loop, or unbounded subprocess.

### P2 — App admission and transport wiring

**Likely files:** `internal/config/config.go`, `internal/app/server.go`, new
CLI-specific app lifecycle code, `internal/localcontrol/`, shared verification
helpers, and focused `internal/deviceauth/` tests.

**Deliver:** distinct `tailscale-cli` config/status, gated startup, trusted
HTTPS/WSS/exact-bundle readiness, durable pairing, role enforcement, restart
credential reuse, and invalidation handling. Use explicit transport-specific
admission rather than injecting booleans into `SessionAuthority`.

**Gate:** positive and negative E2EE/browser/control flows pass; existing managed
watch-bound and BYO no-mutation tests still pass without relaxed expectations.

### P3 — Installed background-service UX

**Likely files:** `relay/common.sh`, `relay/start.sh`, a new CLI setup entrypoint,
`relay/service.sh`, `relay/herdr-mobile-relay-service.sh`, both service installers
and uninstallers, `relay/plugin-install-service.sh`, transport/setup menus,
`relay/setup-link.sh`, plugin actions, and update dispatch/guards.

**Deliver:** transport-specific installation with no Cloudflare prerequisite,
normal launchd/systemd operation, safe stop/unpublish/uninstall, status/reprint,
verified update/rollback, and explicit migration/repair UX.

**Gate:** hosted service lifecycle proves the relay survives setup-pane exit,
starts from a minimal service environment, tolerates Tailscale startup delays,
preserves credentials, and never repeatedly mutates Serve on restart.

### P4 — Isolated development using the same backend

**Likely files:** `Makefile`, `relay/dev-tunnel.sh`, new
`relay/dev-tailscale-cli.sh`, `.gitignore`, development/preflight/lifecycle tests.

**Deliver:** menu option 2 wiring, private defaults/overrides, candidate App Store
CLI selection with the qualification gate, production/dev coexistence fixtures,
explicit dev unpublish, and no legacy state conversion. Before P6, ordinary use
must explain that activation is unqualified and refuse rather than suggest it is
ready on this Mac. Do not stop the installed service to make the test pass.

**Gate:** two simultaneous instances plus foreign-route fixtures prove independent
credentials, ports, builds, registration records, and lifecycle effects.

### P5 — Packaging, documentation, and exact-revision automated evidence

**Likely files:** release packaging/check scripts, hosted ordinary/native workflow
jobs, extracted-package browser fixtures, `README.md`, `QUICKSTART.md`,
`docs/development.md`, `docs/transports.md`, and `docs/updates.md`.

**Deliver:** packaged code runs without Go/Bun/Python on the end-user machine;
new required CLI/service/browser acceptance cases; documented mode differences,
residual risks, recovery, and qualified profiles. Preserve existing gates.

**Gate:** exact-final-SHA ordinary and native CI succeeds; two independent read-only
reviews clear the accumulated implementation and its evidence. Earlier green
commits or clean reviews do not approve a later revision.

### P6 — Deferred: separately authorized real qualification and enablement

**Deliver:** bounded, operator-approved real tests with exact release/profile/OS
identities and sanitized outcomes. Start with a selected unused development
listener; record unrelated service/Serve state before and after. Only proceed to
installed-service migration tests with separate permission.

**Gate:** obtain explicit acceptance of section 5's limits plus scoped permission
before real access/mutation. Then pass the relevant physical acceptance cells,
review the profile-enablement change, and rerun its exact-SHA regression gates.
P6 is not executed or treated as an implementation blocker during P0–P5; record
it as deferred/not authorized. Hosted fixtures can establish automated completion,
never App Store/phone support or release approval.

## 9. Acceptance and verification matrix

| Area | Required cases |
| --- | --- |
| CLI selection | App Store bundle path, PATH override, minimal launchd environment, spaces, missing/ambiguous executable, incompatible CLI/daemon, legitimate distribution metadata, upgrade outside policy. |
| Parser/inspection | malformed/duplicate/unknown/truncated JSON; bounded output; logged out; missing HTTPS; permission denial; mixed foreground/background/Funnel/Services scopes; incomplete observations never mean absence. |
| First publication | explicit consent, empty selected listener, private journal before mutation, exact scoped argv, successful readback and HTTPS identity; cancellation before dispatch changes nothing. |
| Preservation | unrelated production listener and foreign routes unchanged; selected-listener conflict refused; matching unrecorded route refused; no global reset; concurrent cooperative starts serialized. |
| Failure/recovery | command hangs, child/pipe leaks, output loss, crash before/after journal fsync or dispatch, post-write nonzero/timeout, readback mismatch, journal corruption, disk-full, identity drift, stale receipt; no blind replay/adoption. |
| Cleanup | routine stop retains route; explicit scoped removal; replaced/missing route; lost removal acknowledgement; uninstall keeps recovery evidence; no reuse of unresolved old backend. Include a modeled external-writer race to demonstrate, not conceal, the limitation. |
| Background service | pane closure, login/startup ordering, sleep/wake, network outage, daemon restart, app update, relay crash, bounded backoff, no terminal/admin prompt, no Cloudflare requirement, update/rollback. |
| Admission | trusted HTTPS only, wrong hostname/cert/instance/run/transport/release/bundle refused, no early QR, lost arm acknowledgement, no secret in logs, reader/controller separation, revocation, device reuse after restart. |
| Development coexistence | installed service active; production route present; dev start/stop/unpublish leaves both untouched; same root/symlink/port conflicts refused; second checkout isolated; prior generated state preserved. |
| Extracted release | exact archive and production launcher/service code in network-disabled disposable fixtures with internal fixture networking only; HTTPS/WSS/E2EE two-device journeys; trust only the fixture CA; no certificate-ignore flags or replacement shipping implementation. |
| Physical qualification | App Store build under approved account, phone on authorized tailnet, dev+production origin behavior, installed iOS/Android PWA enrollment/reconnect, production service without pane, restart/sleep/wake/update and scoped cleanup. Record each tested platform; do not infer the other. |

A GUI macOS VM without an authenticated App Store app is not an App Store test.
A fake service controller is not proof of LaunchAgent behavior on the managed
Mac. Record both fixture coverage and real qualification as separate cells.

### Commands and execution boundary

Keep **Go 1.27.1, Bun 1.4.0, and actionlint 1.7.12** pinned.

- Local source/static/build checks only: diff/format checks, shell syntax and
  ShellCheck, Python AST parsing for fixtures, `go vet ./...`, `go build ./...`,
  and frontend lint/type-check/build. Do not invoke runtime scripts to validate
  documentation or executable selection.
- Disposable hosted CI: `make check` and relevant targeted Go/race, CLI-process,
  service, socket, browser, packaging, and extracted-release suites. Existing
  `make shell-check`/`make backend-check` include lifecycle execution and are not
  safe substitutes for local static checks on this Mac.
- If local `actionlint` is missing, record it as not run locally and provision
  **1.7.12** in disposable hosted CI; it is not a prerequisite for drafting P0 or
  editing code. Failure of the required hosted lint gate remains a real failure.
- Native hosted Darwin and Linux gates remain required. Isolate filesystem,
  credentials, environment, and network appropriately; fixtures must never fall
  through to an installed real CLI, daemon, service manager, or Herdr socket.
- Include named required cases and fail if they are skipped/missing. Bind logs,
  archive hashes, test results, and reviews to the exact candidate SHA.
- Live qualification needs fresh explicit authorization specifying node, account,
  route/ports, expected mutations, cleanup choice, and permission to contact the
  relevant services. Asking for this plan does **not** grant that authorization.

## 10. Workspace and delivery constraints

This plan is a new standalone file so existing plans/review ledgers remain
historical. Do not edit or relabel prior blocked, cancelled, capped, or approved
runs. Do not modify the orchestration controller to obtain an approval.

Preserve the pre-existing modified `internal/tailscale/DR-1-session-authority.md`
and untracked `docs/plans/`, `docs/collie-tailscale-comparison.md`, and
`docs/collie-tailscale-delivery-plan.md`. (This list originally also named
`frontend/package-lock.json` and `package-lock.json`. Both were accidental npm
artifacts and were removed from the candidate; they must not be preserved or
regenerated, and the frontend installs from `frontend/bun.lock`.) Preserve the
operator's local pi-orchestration `workflows/review.js`, which lives outside
this repository. Do not stash, reset, discard, stage, or overwrite unrelated
changes or existing private development state.

The current plan-editing turn is documentation-only: no service/daemon/CLI/socket
access, lifecycle tests, implementation, commit, or push. This sentence does not
prohibit source implementation or hosted-fixture tests in a subsequently launched
P0–P5 implementation workflow. The ban on personal/live service access remains.
For subsequently authorized implementation, stage only task files. Any permitted
push remains restricted to `origin/ci/tailscale-native-preflight-ef9f843`, without
force-push, main/master push, PR, release, or deployment. Preserve historical
qualified/unqualified distinctions and obtain separate release authorization.

## 11. Definition of done

### A. Implementation workflow completion (P0–P5)

All requirements here are proved by source/static checks and disposable hosted
fixtures, not by accessing a real personal daemon. The controller may approve
this implementation scope only after its review/evidence requirements pass;
that approval must explicitly report **P6 deferred; new CLI profiles not enabled
or live-qualified**. Runtime refusal is not a substitute for implementing/testing
the complete adapter, service, admission, and development lifecycle behind it.

- [ ] CLI-backed transport has a worker-authored, independently reviewed contract
      and qualification matrix; no human acceptance or live result is fabricated.
- [ ] App Store CLI selection/environment and lifecycle behavior have source-backed
      fixture coverage; actual App Store invocation remains a P6 test.
- [ ] Hosted service tests prove pane-independent lifecycle and login/start wiring;
      real managed-Mac login/App Store interaction remains a P6 test.
- [ ] `make dev-tunnel` option 2 uses the same implementation beside production in
      hosted coexistence fixtures, with an honest pre-P6 activation refusal.
- [ ] Scoped registration/removal, durable uncertainty, and no guessed adoption
      are implemented with the documented concurrency/port-reuse limits.
- [ ] E2EE, device roles, trusted HTTPS/bundle checks, and credential reuse pass.
- [ ] Existing direct-session and pure BYO contracts/tests remain intact.
- [ ] Service update/rollback, status/reprint, repair, and uninstall are tested.
- [ ] Exact-final-SHA ordinary/native/extracted-package CI and two independent
      cumulative implementation reviews pass.
- [ ] Unknown/unqualified profiles remain unavailable for ordinary activation;
      docs/status distinguish implementation approval from runtime support.

### B. Live enablement / release readiness (P6, separate authorization)

- [ ] Operator explicitly accepts the persistent-route limitations and authorizes
      the named node/account, commands, listeners/ports, and cleanup policy.
- [ ] Real App Store CLI operation works without another extension or privilege
      change, including the approved LaunchAgent environment and login lifecycle.
- [ ] Authorized App Store/Linux, background-service, coexistence, and physical-phone
      cells have explicit results; untested cells remain labelled unqualified.
- [ ] Profile-enablement changes have exact-SHA tests/reviews; release/deployment
      permission is separately obtained.

**Next implementation action after an explicit workflow launch:** write P0's two
contract/qualification artifacts from source evidence, then implement P1–P5 and
submit the cumulative work to review. Do not ask the user to supply that contract,
re-request live permission merely to write code, remove the old version check,
or call `tailscale serve` on the developer's machine. Do not relaunch a prior run
as part of this plan edit.

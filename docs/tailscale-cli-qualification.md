# CLI-backed Tailscale qualification matrix

**Status: development-only enablement for the exact supplied App Store macOS/arm64
1.102.4 profile. No real-runtime or physical-phone qualification is recorded.**
Source-backed fixtures establish parser/adapter behavior only; a recognized
profile is not proof of runtime compatibility. Production and installed-service
activation remain disabled until physical-phone qualification is recorded and a
separate production enablement is authorized. The owner accepted the four
persistent-route limitations for this isolated development route only; that
acceptance is not runtime qualification or production approval.

## Provenance and candidate profiles

| Candidate | Provenance available to this implementation | Source-described | Fixture-tested | Real-runtime-qualified | Activation |
| --- | --- | --- | --- | --- | --- |
| App Store macOS/arm64 1.102.4 | Handoff-supplied metadata: `osVariant=appstore`; client/daemon long strings reportedly match; reported `gitCommit=3caf7d9e7dcaba589cfc58beda596929733e4fea`, supplemental commit `084ee3b64537a1276e56fc38cdf0a711da9f4936`, capability `142`. This metadata is not an independently verified binary/capture. | CLI command and upstream schema behavior are described by pinned v1.102.4 source; App Store noninteractive execution, daemon/profile match, sandbox permission and login-service behavior remain unknown. | Synthetic version/status/Serve fixtures may cover the supplied metadata shape. Not a live capture. | No | Enabled only on Darwin/arm64 for isolated foreground development after exact route-bound consent; never production or installed-service activation. |
| Upstream Linux/amd64 v1.102.4 candidate | Public source tag v1.102.4 / upstream commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`; module checksum is in `go.sum`. No official distro artifact metadata or actual binary was inspected as part of this work. | CLI commands/schemas and source version metadata are described. The exact distributed binary/build string and socket permissions are not inferred from the source alone. | Clearly synthetic Linux candidate fixtures; no official build string is invented. | No | Disabled pending exact artifact and P6 Linux qualification. |
| MacSys GUI, Darwin/amd64, other macOS/Linux versions, architectures or channels | No selected exact profile evidence in this task. | Partial source similarities do not qualify a profile. | No profile fixture is an approval. | No | Unsupported/refuse. |

A profile parser recognizes source/supplied candidate metadata only to provide
bounded diagnostics and exercise refusal/fixture code. It does not set a
runtime-qualified bit. Version compatibility is separate from the existing
LocalAPI adapter's exact upstream-commit contract: candidate packaging may have
legitimate differing build stamps, but any accepted range requires reviewed
metadata evidence. Do not accept every future version or advise downgrading a
managed computer.

## Command and schema evidence

Pinned source references and argv are in the
[implementation contract](tailscale-cli-contract.md#source-and-evidence-boundary).
The read-only sequence is `status --json`, `version --json --daemon`, and
`serve status --json`. All three must finish within fixed deadlines and output
bounds. The `status` schema must establish a running authenticated identity;
the version schema must establish matching client/daemon release identity; and
Serve JSON must pass a complete supported-schema parse before it can describe
absence or a conflict. `serve status --json` reflects the pinned
`ipn.ServeConfig` JSON and exposes no independent schema version. Unknown fields
therefore refuse, rather than receiving best-effort interpretation.

Proposed mutation argv are limited to the exact background HTTPS publish and
scoped `off` forms in the contract. Any future change in CLI argument parsing,
`ServeConfig` shape, listener semantics, or removal behavior requires source
review and updated fixtures before a profile can be considered. `services`,
Funnel, HTTP/TCP listeners, unknown fields and any unparsed exposure fail closed
in the current narrow contract.

## Required implementation/fixture cases

Fixture adapters must be injected directly into package tests and cannot resolve
to a real CLI, service manager, daemon, Herdr socket or personal state root.
Output and errors are redacted; test logs assert no raw status/account/JSON,
credential, invitation or QR data appears. Required automated cases:

- **Executable selection:** absolute override precedence; missing/nonabsolute or
  nonexecutable override refusal; absolute PATH filtering; approved Darwin bundle
  candidate; duplicate-path deduplication; ambiguity refusal; paths with spaces;
  revalidation of selected path; no command invoked during lookup.
- **Launch environment:** fixed argv (no shell); curated user/session environment
  retains actual `HOME`; rejects inherited endpoint/profile overrides; no GUI,
  terminal, login, admin or prompt assumption is claimed by a fixture.
- **Inspection:** successful authenticated identity; logged-out and permission
  failures; client/daemon mismatch; unsupported metadata; malformed, invalid UTF-8,
  duplicate/case-alias, unknown, oversized, truncated and trailing JSON; status
  identity errors; Serve Funnel/services/unknown exposure; complete empty config;
  mixed unrelated routes; selected-listener conflict; no inference from skipped
  or partial calls.
- **Durable registration:** private state modes, schema validation, atomic
  persistence and fsync; journal intent before dispatch; explicit states; no
  guessed adoption; exact registered-route reuse does not publish again; node,
  route, backend or profile drift refuses; corrupt/copied journal retains evidence.
  `TestRecoverNeverResolvesAmbiguousPublicationFromObservedRoute` and
  `TestRecoverRefusesUnregisteredObservedRoute` exercise read-only recovery and
  prove matching/absent observations do not clear uncertainty or authorize adoption.
- **Mutation outcomes:** exact publish/unpublish argv only; explicit consent;
  pre-dispatch failure changes nothing; successful acknowledgement is durably
  recorded before HTTPS verification; timeout, killed child, lost output, nonzero
  result, readback mismatch or post-dispatch journal failure retains uncertainty;
  no retry, global reset, Funnel, `up`, login, service mutation or rollback.
- **Concurrency/recovery:** same-node production/development writers serialize;
  foreign/unrecorded mappings refuse; unrelated supported routes remain unchanged;
  an external-writer race is modeled to demonstrate the check-to-write limit;
  uncertain/corrupt state cannot be silently repaired, adopted, erased or used to
  repurpose its backend.
- **Admission/service:** the exact App Store profile may activate only in
  isolated foreground development after development qualification and route consent.
  Production and installed-service activation remain disabled pending physical-
  phone qualification and separate enablement. Status distinguishes
  `development_qualification_enabled` from `runtime_qualified`; pairing remains
  closed until trusted HTTPS, exact bundle, current identity and app readiness.
  The ordinary app/config constructors now refuse CLI-backed startup; only the
  process-local Go development workflow may provide the registration verifier.
  The previous tagged admission/service fixture results describe an earlier
  separate-process architecture and are not evidence for this revision. The
  current candidate adds positive/negative tests at the command, config,
  app-constructor and workflow-operation boundaries; compile-only checks are not
  passes, so the tests require exact-SHA hosted execution. No physical phone,
  service, or live Tailscale qualification is implied.
- **Development:** `.dev-tailscale-cli/` is a separate foreground workspace with
  fixed HTTPS 8443 -> loopback relay backend 18377 and plugin listener 18378;
  port overrides are refused before mutation. It has separate registration and
  coordination roots, read-only status/recovery, a single-process app/manager
  lifecycle, explicit scoped cleanup, and route-preserving Ctrl-C. Only the exact
  supplied App Store 1.102.4 metadata on Darwin/arm64 enables the real workflow;
  Linux, Darwin/amd64, MacSys, other versions and unrecognized profiles refuse
  before Serve mutation. An in-process `DevelopmentWorkflow` retains the
  manager after Go validation; it is not an OS privilege boundary. The Go
  constructor validates explicit opt-in, the exact profile,
  private isolated config/cache/data/release/runtime/registration roots, relay
  environment/marker bindings, production and installed-service separation,
  local Herdr socket identity, and the fixed tuple before CLI access; the
  manager repeats those checks before operations. The shell uses only
  filesystem-only binary selection and the Go `dev-tailscale-cli preflight`
  entrypoint; standalone `tailscale-cli preflight` refuses before executable
  selection. This is a safety boundary for ordinary bypasses, not launcher
  provenance or a privilege boundary against deliberate fabrication by the
  same user. `serve`, ordinary config loading and standalone `tailscale-cli`
  inspection/mutation commands refuse CLI-backed startup/operations. Setup
  displays the selected node/origin/ports and requires exact route-bound stdin
  consent in the Go-owned foreground process.
  No environment variable can supply consent. The designated owner's phone
  enrollment/E2EE smoke and exact-route cleanup are documented in the
  [supervised runbook](tailscale-cli-contract.md#supervised-owner-development-phone-smoke-and-cleanup-runbook);
  this remains development-only and does not qualify other phones, runtime
  support, services, or production.
- **Packaging and unchanged paths:** exact release archive uses no end-user Go,
  Bun or Python; existing direct-session and BYO regression suites stay intact.
  Hosted fixtures must exercise the single-process `dev-tailscale-cli` dispatch
  with inert synthetic binaries; the native workflow also runs
  `go test -tags=herdr_tailscale_test ./internal/tailscalecli` and must show
  `TestFixtureBuildRejectsOrdinaryExecutableBeforeExecution` as PASS, not SKIP.
  No test may override a hosted-only guard on the worker machine. The
  extracted-package job must verify the exact release archive. Require ordinary,
  native Darwin/Linux and extracted-package results for the exact final SHA;
  earlier green revisions do not apply to later code. Acceptance evidence must
  identify the exact SHA for every hosted result.

## Owner-phone development smoke and broader qualification

The supervised development runbook includes one owner-authorized enrollment and
E2EE smoke on the designated owner phone, followed by exact-route cleanup and a
complete before/after Serve comparison. The source/CI worker does not execute
that live runbook; the supervising assistant does so only after exact-final-SHA
hosted checks and independent review. Treat the phone journey as a narrow
owner-development smoke, not a compatibility matrix, runtime qualification,
release qualification, or production authorization. Do not infer qualification
from a profile preflight, server health, browser fixture, successful route, or one
phone. Production and installed-service enablement remain disabled pending the
separate qualification record and production enablement change.

## Service identity, recovery and monitoring

macOS service identity is per-user LaunchAgent after login; no pre-login guarantee
is claimed. Linux is the existing systemd user service; do not enable linger or
change system units. Preserve real user/session context required by the selected
CLI but do not rely on shell initialization. Service setup coordinates with the
relay through a single instance lock/private control API. Only one process may
own the backend/device-store writer. Do not require Cloudflare tools/config for a
future CLI transport. Status distinguishes process liveness, local readiness,
Tailscale availability, route registration, trusted HTTPS and pairing readiness.
Transient readiness checks use bounded backoff and read-only operations. Route
or identity drift closes admission; it never starts a Serve mutation loop.

Persist `publish-pending` or `remove-pending` before mutation; a crash or ambiguous
post-dispatch outcome becomes the corresponding `*-uncertain` record. Recovery
commands inspect and report evidence but do not auto-adopt or replay a mutation.
Only an explicit operator action with fresh consent can resolve a supported,
acknowledged registration. A normal stop or Ctrl-C is **stop relay, leave the
persistent route**. `unpublish` is a separate consented operation. Keep state if
unpublish is unconfirmed; do not delete the root to clear uncertainty.

Monitor on a bounded interval (initial design target: 5 seconds) with bounded
per-call timeouts and jittered backoff when the daemon is unavailable. This
polling has detection latency and is not a live LocalAPI watch or instantaneous
remote revocation.

## Development risk acceptance and live runbook

The owner accepted these four limits only for this isolated development route;
they must be shown before consent and in status/recovery guidance. Acceptance is
not runtime qualification or production authorization:

1. The CLI check-to-write race is not atomic with respect to external Serve writers.
2. The persistent route and local backend-port reuse can survive relay stop and
   expose a later process bound to the same port.
3. There is no automatic global rollback; unrelated Serve state must never be
   reset or restored from a snapshot.
4. There is no guarantee that remote connections drain after route removal.

### Authority boundary and exact authorized scope

The implementation worker may perform source, static, build and compile-only
work and hosted fixtures; the worker must never perform live Tailscale access,
service access, route changes or physical-phone testing. After exact-revision
hosted checks and independent approval, the supervising assistant may perform
the already owner-authorized development-only sequence on this Mac's current
node/account using the exact App Store Tailscale 1.102.4 candidate on
Darwin/arm64. This is not a request for another owner grant for the actions
listed below. Do not substitute another node, account, profile, device, action
or scope.

The only authorized route tuple is HTTPS 8443 -> `127.0.0.1:18377`, with plugin
listener 18378, in isolated foreground development. Runtime publication and
cleanup each still require the Go workflow's displayed, exact, typed
route-bound confirmation; no environment variable supplies consent. This
sequence does not authorize installed-service activation/migration, production
access or mutation, a production route, deployment, release, or broader
qualification. These source/documentation changes establish none of those
outcomes and do not themselves establish live qualification.

### Supervised sequence

0. **Exact-revision and host.** Confirm the reviewed checkout and current
   revision have ordinary, native Darwin/Linux and extracted-release hosted
   results, and independent review is complete. Confirm Darwin/arm64 and the
   selected executable/profile is the exact App Store 1.102.4 candidate. Confirm
   the current node/account is the owner-authorized one. Do not run a direct
   mutation, select another profile/node, or override the fixed ports.
1. **Read-only identity and coexistence baseline.** Verify the configured Herdr
   executable and `HERDR_SOCKET_PATH` identify the owner's active local Herdr
   Unix socket (`test -x "$HERDR_BIN"` and `test -S "$HERDR_SOCKET_PATH"`); do
   not replace `HOME` with development state. Read installed-service status and
   record only the metadata/hash/liveness needed to prove the service is
   unchanged; do not start, stop, edit or migrate it. Run the selected absolute
   Tailscale executable's read-only `status --json`, `version --json --daemon`
   and `serve status --json` sequence. Keep raw account/status output private;
   capture a complete private Serve baseline and a sanitized summary of every
   existing route. Stop for a logged-out daemon, wrong node/account, permission
   error, profile/version mismatch, malformed or unknown schema, incomplete
   observation, pending/uncertain journal or any ambiguity. Do not retry around
   a refusal, free a port, adopt a route, or remove another mapping.
2. **Conflict refusal and scoped publication.** Start only the isolated
   foreground development launcher. Its Go-owned preflight must validate
   isolation and the exact profile before real CLI access. Publication must
   refuse if HTTPS 8443, backend 18377 or plugin 18378 is occupied or conflicts,
   or the selected listener has an unrecorded/unknown mapping. Review the complete
   Serve baseline and all four accepted limits. Enter only the displayed exact
   node/origin/8443/`127.0.0.1:18377` typed runtime confirmation. EOF or any
   mismatch cancels before mutation; never clear a conflicting resource.
3. **Readback, identity, bundle and readiness.** After setup, compare the
   complete supported Serve configuration: only the intended HTTPS 8443 path
   `/` -> `http://127.0.0.1:18377` may have been added; all unrelated mappings
   must match the baseline. Confirm the plugin listener is 18378 and the local
   backend binds only to `127.0.0.1:18377`. Check local `/readyz` and `/healthz`
   for readiness and expected transport, instance, control-run, relay
   version/revision, and bundle version/revision/hash. Request
   `https://<canonical-node>:8443/healthz` without `-k`, redirects or trust
   bypass; require trusted system TLS/hostname verification, matching instance
   identity and the same release/bundle identity. Verify the configured phone-app
   origin's `version.json`, descriptor and assets match the exact release. Pairing
   remains closed until identity, trusted HTTPS, exact bundle and readiness pass.
   Preserve the pre-run service metadata/hash/liveness for comparison; do not
   activate or change that service.
4. **Owner's physical-phone assessment.** Leave the development route available
   for the already owner-authorized physical-phone assessment. The foreground
   Go process displays the one-use setup link/QR only after the checks above;
   show it privately to the designated owner and do not place it in notes,
   evidence, chat or logs. Use the designated owner phone on the intended
   tailnet. Complete the original E2EE enrollment and authenticated WebSocket
   assessment; record the assigned device role and assess controller and reader
   role behavior as separate cells under the original assessment requirements.
   Keep reader access read-only and controller-only permissions distinct. Do not
   send prompts, control agents, upload data, reset credentials or add a device
   outside the already authorized assessment. Disconnect/reconnect once and
   verify the enrolled credential resumes without re-pairing. If any role cell
   cannot be safely assessed within that scope, record it as pending rather than
   infer it from source or hosted fixtures. Stop on trust, identity, E2EE,
   readiness or role uncertainty; do not print a second invitation or clear
   device state.
5. **Exact scoped cleanup.** After the owner's phone assessment, end the phone
   session, stop the foreground relay with Ctrl-C, and prove the pairing socket
   is removed and backend TCP 18377 is free. Capture a complete private Serve
   snapshot and confirm the only eligible removal is the journaled node's exact
   HTTPS 8443 path `/` -> `127.0.0.1:18377`; unrelated routes still match the
   pre-publication baseline. Run the Go-owned development `unpublish` action and
   enter its displayed exact node/origin/port/backend runtime confirmation. The
   owner-authorized scope already includes removing this exact development
   route; fresh exact-route runtime confirmation remains mandatory and is not
   replaced by blanket consent. Go must repeat identity/schema/route/isolation checks and
   issue only scoped `serve ... off`. Read back the complete Serve state: the
   exact development route is absent, unrelated routes are unchanged, and
   installed-service configuration/hash/liveness still match the baseline. Keep
   the removed journal and private development state; never run `serve reset`,
   stop or modify a production service, or remove a different route.
6. **Uncertainty and cell recording.** If a CLI write times out, acknowledgement
   or readback is lost, route/node/identity differs, unknown fields appear,
   service/Serve state changes unexpectedly, a socket/port is occupied, or any
   result is ambiguous, stop and retain the journal, reservation and private
   evidence. Use only read-only status/recovery to inspect. Never retry an
   ambiguous write, erase recovery state, or touch production. Record each
   assessed cell's exact candidate, host/device/role, checks, outcome and
   redacted evidence; mark every untested qualification cell **pending**. The
   following cells are currently pending, not passed:

   | Assessment cell | Status |
   | --- | --- |
   | App Store 1.102.4 runtime on this Darwin/arm64 Mac and current node/account | Pending |
   | Complete pre/post Serve and installed-service noninterference | Pending |
   | Scoped HTTPS 8443 -> `127.0.0.1:18377` publication and exact `off` readback | Pending |
   | Trusted HTTPS, exact bundle, identity, local socket and readiness | Pending |
   | Owner physical-phone E2EE enrollment and controller role | Pending |
   | Reader role and read-only permission boundary | Pending |
   | Disconnect/reconnect with existing credential and no re-pairing | Pending |
   | Other phones/platforms, PWA lifecycle, sleep/wake, service, production and release cells | Pending |

This is a development-only supervised runbook, not a deployment instruction.
No live qualification, production enablement, deployment or release is
established by these edits.

# CLI-backed Tailscale qualification matrix

**Status: qualification plan; no live tests or approval recorded.** Source-backed
fixtures establish parser/adapter behavior only. All real profile qualification
and activation remain deferred to separately authorized P6 work. `ownerAcceptance`
for the persistent-route limitations is **pending live enablement**; implementation
or review of this matrix is not acceptance.

## Provenance and candidate profiles

| Candidate | Provenance available to this implementation | Source-described | Fixture-tested | Real-runtime-qualified | Activation |
| --- | --- | --- | --- | --- | --- |
| App Store macOS 1.102.4 | Handoff-supplied metadata: `osVariant=appstore`; client/daemon long strings reportedly match; reported `gitCommit=3caf7d9e7dcaba589cfc58beda596929733e4fea`, supplemental commit `084ee3b64537a1276e56fc38cdf0a711da9f4936`, capability `142`. This metadata is not an independently verified binary/capture. | CLI command and upstream schema behavior are described by pinned v1.102.4 source; App Store noninteractive execution, daemon/profile match, sandbox permission and login-service behavior remain unknown. | Synthetic version/status/Serve fixtures may cover the supplied metadata shape. Not a live capture. | No | Disabled pending P6 App Store invocation and service/phone qualification. |
| Upstream Linux/amd64 v1.102.4 candidate | Public source tag v1.102.4 / upstream commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`; module checksum is in `go.sum`. No official distro artifact metadata or actual binary was inspected as part of this work. | CLI commands/schemas and source version metadata are described. The exact distributed binary/build string and socket permissions are not inferred from the source alone. | Clearly synthetic Linux candidate fixtures; no official build string is invented. | No | Disabled pending exact artifact and P6 Linux qualification. |
| MacSys GUI, other macOS/Linux versions, architectures or channels | No selected exact profile evidence in this task. | Partial source similarities do not qualify a profile. | No profile fixture is an approval. | No | Unsupported/refuse. |

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
- **Admission/service:** no CLI profile can activate before P6; pairing remains
  closed until trusted HTTPS, exact bundle, current identity and app readiness;
  service stop preserves route; service restart performs no route write; bounded
  waiting does not mutate or restart-storm. `TestCLITailscaleControlStatusFailsClosedOnUnqualifiedRoute`
  covers unqualified-route refusal. Tagged `TestTailscaleCLIReadinessArmAndDrift`
  now exercises exact local/public HTTPS, the real public-bundle verifier over
  fixture TLS, pre-arm HTTP 503, complete E2EE enrollment, durable invitation
  state, and route-drift quarantine that preserves the enrolled credential.
  Normal and race runs of the required tagged app/control matrix passed. Named
  fake-only Linux systemd and macOS launchd fixtures passed install/health/stop;
  repeated service-wrapper starts did not publish, and uninstall preserved the
  persistent route plus out-of-tree registration journal after explicit consent.
  These are fixture results only, not real-service qualification. A reconstructed
  app process reopens durable device credentials, requires a fresh exact-route
  admission arm, and closes admission on drift; a reopened registration manager
  performs read-only recovery. Explicit shell unpublish tests decline and exact
  route-removal consent. An interactive operator-managed package update now
  checks the route before/after service restart and rolls back on drift using
  fake-only manager, service, installer, and health fixtures. Phone-managed
  updates remain refused; real profile coexistence and live qualification remain
  outstanding.
- **Development:** source now has a separate `.dev-tailscale-cli/` foreground
  workspace, default ports 18577/18578/9443 (distinct from production, tunnel,
  and managed-Tailscale fixtures), separate registration/coordination roots,
  read-only status/recovery, explicit unpublish, and route-preserving Ctrl-C.
  The fake-only hosted fixture passed positive start/update, exact-route recheck,
  development-scope isolation, and production-state preservation. Activation-gate
  refusal, Linux/macOS service fixtures, route-preserving stop/uninstall, and
  exact shell-unpublish consent were exercised without a real Tailscale CLI or
  service manager. Operator-managed package update is interactive and fixture-
  tested but remains unavailable until separate P6 profile qualification and
  activation; no profile is runtime-qualified.
- **Packaging and unchanged paths:** exact release archive uses no end-user Go,
  Bun or Python; existing direct-session and BYO regression suites stay intact.
  `test_dev_tailscale.py` and the extracted-package checker use an inert relay
  binary and no service-manager/CLI fallback for CLI wrapper dispatch. Require
  exact-final-revision ordinary, native Darwin/Linux and extracted-package
  fixtures and two independent cumulative reviews. Earlier green revisions do
  not apply to a later SHA.

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

## Risk acceptance and P6 runbook questions

All four limits in the contract must be disclosed in setup consent, status and
recovery instructions: non-atomic CLI check-to-write race, persistent route and
local port reuse, no automatic global rollback, and no remote-drain guarantee.
Operator acceptance remains pending. Prior to real tests, ask the operator to
confirm, in one scoped decision:

1. Which exact user account, tailnet/node, Tailscale app/package, OS version and
   release/profile may be contacted?
2. Which unused development HTTPS listener/backend and resulting origin may be
   published, and which unrelated Serve state must be preserved?
3. Is permission granted to run the listed read-only status/version/Serve checks
   and the exact publish/readback/removal commands against that node?
4. Should cleanup remove the exact acknowledged route or deliberately leave it?
   What should happen if acknowledgement/readback is uncertain?
5. After development qualification, is a separately scoped installed-service
   migration and physical-phone test authorized?

No question is answered by this document. The run starts with sanitized pre-state
capture and a selected unused development listener, stops on any mismatch, and
retains evidence on uncertainty. Installed-service migration, PWA enrollment,
sleep/wake/update and release approval are separate cells and permissions.

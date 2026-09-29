# CLI-backed Tailscale Serve contract

**Status: development-only enablement for the exact supplied App Store macOS
1.102.4 profile; runtime qualification remains pending.** The new transport is
named `tailscale-cli`. It is a distinct, persistent CLI-owned background Serve
route and is not an alias for the existing foreground LocalAPI transport
(`tailscale`) or operator-owned transport (`tailscale-external`). Only isolated
foreground development is enabled, with exact route-bound stdin consent.
Production and installed-service activation remain disabled until physical-phone
qualification is recorded and separate production enablement is authorized.

## Source and evidence boundary

The Go module pins `tailscale.com v1.102.4`; the repository's existing source
contract identifies upstream commit
[`bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`](https://github.com/tailscale/tailscale/tree/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8).
The pinned source confirms the read and Serve command shapes below. It describes
source behavior only: it is not a binary signature, proof of an installed
client/daemon pair, App Store invocation evidence, or runtime qualification.
The App Store metadata in the handoff is operator-supplied and has not been
independently captured. No live daemon is contacted by this implementation.

Pinned source references:

- [`status --json`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/status.go#L31-L103)
  marshals the daemon `ipnstate.Status`; the source itself warns that this JSON
  format can change between releases.
- [`version --json --daemon`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/version.go#L21-L81)
  combines client `version.Meta` with daemon `Status.Version` in `daemonLong`.
- [`serve status --json`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_legacy.go#L607-L628)
  JSON-marshals the current Serve config. Its representation is the pinned
  `ipn.ServeConfig`, not a separately versioned CLI envelope.
- [`serve` v2 argument parsing](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_v2.go#L239-L262)
  declares `--bg`, `--https`, `--set-path`, and the broader command surface.
  [`serve_v2_test.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_v2_test.go#L137-L188)
  exercises the background HTTPS proxy configuration and scoped `off` command.
- [`ServeConfig`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/serve.go#L46-L186)
  defines the persisted config shape and related listener/handler fields.
- The project [read-only LocalAPI contract](../internal/tailscale/CONTRACT.md)
  documents identity and strict JSON parsing used by the existing mode. It is
  not authority for persistent CLI route ownership.

The JSON parsers are deliberately stricter than `encoding/json` defaults:
reject invalid/truncated/oversized input, invalid UTF-8, trailing JSON, duplicate
or case-alias keys, excessive nesting and unknown Serve exposure semantics. A
partial or unknown inspection is never interpreted as an empty listener. The
source-derived fixture values are synthetic; they are not captured output.

## Allowed subprocess contract

The executable must resolve to one absolute regular executable path. An explicit
absolute override takes precedence and is rejected if invalid. Otherwise inspect
only absolute executable candidates from `PATH` and, on Darwin, the approved
`/Applications/Tailscale.app/Contents/MacOS/Tailscale` bundle path. Deduplicate
canonical paths and refuse ambiguity; aliases and shell functions are not
executable candidates. Keep the selected path/profile in private state and
revalidate the path and profile before every operation. Do not pin an immutable
binary hash as the compatibility policy.

Commands use fixed argv arrays, no shell, `sudo`, downloaded helper or mutable
environment override. Preserve only the user/session environment required by the
CLI (including the real `HOME`); do not replace `HOME` with the relay's isolated
state root. The subprocess receives only `/usr/bin:/bin:/usr/sbin:/sbin` as
`PATH`; caller-controlled search paths are never inherited. Use bounded deadlines,
stdout and stderr, child/pipe cleanup and redacted errors. Never log raw
status/config/version JSON.

Read-only inspection is exactly:

```text
<absolute-cli> status --json
<absolute-cli> version --json --daemon
<absolute-cli> serve status --json
```

The complete result must identify a logged-in node, consistent CLI/daemon
release metadata, canonical MagicDNS HTTPS name, and a complete supported Serve
schema. `osVariant` is one metadata signal, not daemon-identity proof. CLI exit
status, missing fields, unknown schema, command timeout, permission error and
incomplete Serve state remain distinct diagnostic outcomes.

The only proposed route mutation argv are:

```text
<absolute-cli> serve --bg --https=<https-port> --set-path=/ http://127.0.0.1:<backend-port>
<absolute-cli> serve --bg --https=<https-port> --set-path=/ off
```

The second form is allowed only after the journal has a durable acknowledged
registration, fresh complete preflight identifies the same node and exact
listener/mount/backend, and separate unpublish consent is obtained. Command
qualification must verify both argv forms for each future profile. Never call
`serve reset`, `set-config`, `set-raw`, `funnel`, `up`, `login`, or a privilege
change. The command is a CLI-mediated check-to-write sequence, not an atomic
Herdr ownership capability.

## Private registration and admission contract

The schema-versioned registration journal is operational attribution, not a
capability. It contains an installation identifier, scope (`production` or
`development`), node/profile identity, listener and mount, loopback backend,
consent scope, operation ID, state and acknowledged CLI result. It contains no
Tailscale authentication token. Create/validate private roots as real, non-symlink
mode-0700 directories and journal files as mode 0600. Serialize Herdr writers
for the same user/node with a shared private lock; this does not lock external
CLI writers or other UIDs. Reserve each loopback backend port durably in that
same shared root before starting a new relay listener; parse backend URLs so
127/8, `localhost` names and IPv6 loopback aliases conflict by effective port,
not just exact URL spelling. A `publish-pending` reservation is exclusive and
carries a unique setup-attempt ID: a second setup conflicts rather than taking
over the first attempt's claim, and cleanup/release must present that exact ID.
Recovery reports the pending claim and attempt ID. Release requires explicit
interactive confirmation, a stopped/unloaded service, a locally free backend
listener, and read-only proof that no Serve route targets the port. If setup is
interrupted or a service appears during setup, retain the claim until those
checks and the exact attempt-bound release are established.

CLI setup refuses to replace either current or legacy relay service definitions
on Linux or macOS. It does not stop or delete `herdr-remote.service` or
`com.herdr-remote.service.plist`; resolve/migrate an existing service explicitly
before retrying setup. The service installers also recheck this boundary before
creating a new CLI-backed service.
Persist intent atomically with file and parent-directory sync before
dispatch. Never overwrite corrupt, copied or mismatched recovery state.

Persisted states are `unconfigured`, `publish-pending`, `registered`,
`publish-uncertain`, `remove-pending`, `remove-uncertain`, `removed`,
`reconciled-present`, and `reconciled-absent`. Reconciled states record only an
operator-confirmed observation; they never claim a CLI mutation acknowledgement.
`ready`, `waiting`, `conflicted` and `degraded` are runtime observations, never
proof inferred from the journal alone. A pre-dispatch failure leaves the
operation unmutated. Any failure after dispatch without a complete successful
acknowledgement is uncertain; do not retry automatically. A successful CLI
acknowledgement is durably recorded before HTTPS/frontend verification. If
recording fails after dispatch, retain uncertainty and close admission. Readback
must verify the selected route without discarding unrelated recognized config.
Unknown exposure fields, services, Funnel, incomplete inspection, or any route
sharing the selected listener cause refusal.

A mapping without a valid acknowledged registration is never adopted, even if
host, port, URL, backend response or PID matches. An exact acknowledged mapping
can be reused without another Serve write after fresh node/config validation.
The read-only `Recover` operation reports only redacted journal/readback state;
it never clears uncertainty or treats an observed matching/absent route as
proof that a dispatched mutation was acknowledged. Reconciled-present remains
unready and unacknowledged until an explicit exact-route unpublish; reconciled-
absent may be followed only by a fresh consented publication. A disappeared
registered route is degraded and is not automatically recreated. Explicit repair
and explicit unpublish require fresh preflight and consent. Transport-selection
helpers run a local-only journal/reservation guard before changing away from
`tailscale-cli`; active, uncertain, or pending state blocks the change until the
exact route is explicitly unpublished and any owned pending reservation is
safely released. Routine service
stop/restart retains the persistent route; only the relay process stops. On
restart the service rechecks the route and resumes existing-device admission
without minting a new bootstrap invitation; invitation generation is an
operator-initiated setup or arm-bootstrap action. Service preflight retries only
classified transient read failures, for at most seven attempts with jittered
backoff capped at 30 seconds; permanent binary, authentication, permission,
identity, and schema failures stop the service without changing Serve. A publish
rejected before CLI dispatch stops the backend first, then releases its shared
backend-port reservation only after read-only inspection proves no Serve route
uses that listener; uncertain post-dispatch state retains both journal and
reservation. Phone-
managed package updates remain refused for CLI-backed installations. After
separate profile qualification and activation, `relay/tailscale-cli.sh update`
provides an interactive operator-managed package procedure: it checks the exact
journaled route before download and after restart, retains the route and journal,
and restores the previous release/service if recovery fails. It does not repair
or remove Serve routes or guarantee remote connection drain. Service-only uninstall interactively offers exact-route removal, retaining the
persistent Serve route, or cancellation; noninteractive uninstall refuses to
silently choose. Route removal delegates to the existing explicit consent and
exact-route procedure, and failed removal leaves the service installed. The
installed unit/plist environment path is inspected read-only so the prompt binds
to the correct relay configuration. A retained route and its journal remain in
place. An unresolved previous route blocks repurposing its backend.

Start the app backend loopback-only and keep application/WebSocket admission and
pairing closed until the verified HTTPS origin, current instance/run/transport/
release, exact complete web bundle, trusted OS TLS/hostname validation and
application readiness all pass. Preserve the existing E2EE, device roles,
revocation, invitation durability and credential reuse. Never reset devices on
restart or infer authorization from tailnet membership. Pairing links remain
operator-only actions and are never written to background-service logs. Monitor
identity and route drift with bounded read-only polling; close admission on
mismatch and do not auto-repair. Transient inspection failures suspend
admission reversibly; the service may resume existing-device admission only
after a fresh exact-route and readiness check, without minting a new invitation.
Polling is not an instantaneous revocation watch.

Production activation is currently hard-gated off: a recognized profile is
not runtime-qualified. The exact supplied App Store 1.102.4 profile has a
separate development-qualification bit, reported distinctly from
`runtime_qualified`; it is usable only in isolated foreground development.
There is no environment-variable, config-file or fixture switch that makes a
real profile runtime-qualified or production-enabled. Existing `tailscale` and
`tailscale-external` configuration contracts remain unchanged.

## Lifecycle and residual risks

| Event | Contract |
| --- | --- |
| First setup | Inspect read-only; bind/gate backend; show route and persistence; obtain consent; durably write `publish-pending`; issue at most one publish; persist the acknowledgement; inspect/read back; commit `registered`; validate HTTPS/bundle; then permit pairing. |
| Existing acknowledged route | Validate the same installation/node/route and live backend; reuse without a Serve write. |
| Matching but unrecorded route | Refuse; no takeover or synthetic receipt. |
| Unrelated listener | Preserve only when the entire supported schema proves no selected listener/mount/protocol conflict. |
| Routine stop/restart/update | Stop/restart only the relay; keep route and journal. The separately gated operator update performs exact-route checks before download and after restart, with package/service rollback on failed recovery. |
| Route disappears or identity drifts | Degrade, close admission and retain devices; no automatic repair. |
| Timeout/cancel/ambiguous post-dispatch result | Persist `publish-uncertain` or `remove-uncertain`; no automatic retry or cleanup. |
| Explicit unpublish | Warn against concurrent Serve edits; consent; require valid acknowledged registration; fresh exact-route check; scoped `off`; persist acknowledgement and verify readback. |
| Service-only uninstall | Interactively require exact-route removal, explicit route retention, or cancellation; noninteractive mode refuses; never erase unresolved recovery evidence. |
| Transport switch | Decide old persistent-route disposition; unresolved removal blocks backend reuse. |

Four limits must appear in setup consent and recovery guidance. The owner
accepted these limits only for this isolated development route; this is not
runtime qualification or production authorization:

1. **CLI check-to-write race.** A fresh CLI inspection and Herdr's cooperative
   lock cannot atomically compare-and-swap against external writers. Tailscale's
   internal behavior does not expose Herdr's in-process ownership capability.
   Another writer can replace the selected mapping between inspection and the
   CLI's own read. Scoped commands reduce but do not remove this risk.
2. **Persistent route / local port reuse.** Serve may keep forwarding after the
   relay crashes or stops. Another local process binding the port could receive
   traffic or serve a different frontend. Herdr reservations, readiness checks
   and bundle verification do not eliminate this residual risk; E2EE does not
   authenticate replacement frontend JavaScript.
3. **No global rollback.** Never reset Tailscale or restore an old whole Serve
   snapshot. A route may remain after failed setup; keep explicit recovery state.
4. **No remote-drain guarantee.** A successful scoped removal and readback does
   not prove that existing connections have drained or all remote traffic ceased.

### Development runbook

The supervising assistant executes this runbook only after independent review.
The only live candidate is the current Mac's App Store Tailscale 1.102.4 profile
on the owner's current node/account, in isolated foreground development, with
HTTPS Serve port 8443 and loopback backend 127.0.0.1:18377 (plugin backend
18378). Production, installed-service activation/migration, physical-phone
enrollment and release qualification are outside this runbook.

1. Confirm the executable, client/daemon identity and version exactly match the
   supplied App Store profile; stop for any unrecognized or inconsistent profile,
   wrong account/node, permission error, logged-out daemon or ambiguity.
2. Capture a sanitized pre-change summary of the complete Serve configuration,
   including unrelated routes. Preserve all unrelated state. The manager refuses
   if listener 8443 or backend 18377 is occupied, conflicting, unrecorded, or
   cannot be parsed completely. Do not free a port or remove another mapping.
3. Review the canonical node origin and four limitations shown by the launcher.
   Type the exact node/origin/8443/127.0.0.1:18377 confirmation displayed by the
   tool on stdin. No environment variable can consent; mismatch or EOF cancels
   before state or route mutation.
4. Run only foreground development setup. Verify readback is exactly the
   selected HTTPS route to 127.0.0.1:18377 and record a sanitized post-change
   Serve summary. Confirm unrelated routes are unchanged. Do not install/start a
   service or enroll a phone.
5. Stop and retain route/journal by default. If cleanup is explicitly chosen,
   use separate exact-route unpublish consent. On timeout, mismatch, unknown
   fields, CLI error after dispatch or uncertainty, stop and retain evidence; do
   not retry, reset Serve or remove another route.
6. Record the result as development-only. Physical-phone qualification remains
   separate; production stays disabled until it is recorded and separately
   enabled.

This runbook is not a deployment instruction. PWA enrollment, sleep/wake,
installed-service migration and release approval are distinct future phases.

## Source entrypoints and current gate

The Go `tailscale-cli` subcommand exposes read-only `status`/`recover`, strict
`assert-ready`, and separately consented `publish`/`unpublish`; its
`activation-check --scope development` reports development enablement separately
from runtime qualification, while the default production scope remains hard-gated. The relay shell lifecycle
routes those actions through the journal manager, installs only a per-user
service after `assert-ready`, and keeps stop separate from unpublish. The
foreground development entrypoint uses `.dev-tailscale-cli/` with independent
configuration, release, cache, runtime, and registration roots. It stages each
complete binary/web pair in a versioned release directory and atomically swaps
a single `current` symlink, retaining the prior coherent release if staging or
cutover fails.

`TailscaleCLIProfilesEnabled` remains false in ordinary and fixture builds.
The exact App Store candidate has a separate development-qualification bit; it is
not runtime-qualified. Production and installed-service activation remain
refused. The `herdr_tailscale_test` build hook only admits a CLI executable
carrying the explicit synthetic-fixture marker; ordinary installed Tailscale
executables are rejected by `NewClient` before execution. The fixture hook is
not runtime qualification or production activation. Current-revision ordinary,
native and extracted-package CI must establish the fixture results; no live
Tailscale CLI, daemon, tailnet, service, phone or Herdr socket was contacted by
this implementation phase.

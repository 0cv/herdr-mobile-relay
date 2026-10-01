# CLI-backed Tailscale Serve contract

**Status: development-only enablement for the exact supplied App Store macOS/arm64
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
state root. On Darwin, the curated child environment includes exactly one
`TAILSCALE_BE_CLI=1`, replacing any inherited value; Linux does not receive this
variable. The subprocess receives only `/usr/bin:/bin:/usr/sbin:/sbin` as `PATH`;
caller-controlled search paths are never inherited. Use bounded deadlines,
stdout and stderr, child/pipe cleanup and redacted errors. Never log raw
status/config/version JSON.

The selector addresses a confirmed App Store GUI fallback: during the supervised
read-only rehearsal on the supplied App Store 1.102.4 Darwin/arm64 build, the
three read-only commands returned `The Tailscale GUI failed to start` on stdout
and exited zero under the curated environment without a terminal variable.
Bisection showed `TAILSCALE_BE_CLI=1` selects CLI mode on that exact build. This is
empirical evidence for that closed-source build only, not a source guarantee for
other packaging/version profiles. Tailscale documents this escape hatch in
[upstream issue #7140](https://github.com/tailscale/tailscale/issues/7140) and
historical `ActLikeCLI` behavior, but the selector is absent from the open-source
v1.102.4 module tree. Do not infer support for another App Store build or qualify
a profile from the fixture. If the known GUI-startup text is returned, the
adapter reports a distinct redacted GUI-mode diagnostic rather than weakening
strict JSON parsing or misclassifying it as generic invalid JSON.

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

The second form is allowed only from the process-local development workflow,
after the journal has a durable acknowledged registration and fresh complete
preflight identifies the same node and exact listener/mount/backend. The action
must be within the applicable owner-authorized scope (the supervised runbook
below is already authorized); each execution still requires fresh exact-route
runtime confirmation. This is not blanket consent or a demand for a further
owner grant for that scoped cleanup. Command qualification must verify both
argv forms for each future profile.
Never call `serve reset`, `set-config`, `set-raw`, `funnel`, `up`, `login`, or a
privilege change. The standalone command surface cannot request a manager from
flags or journal data; the supported development entrypoint creates its
process-local handle only after Go-owned profile/isolation validation. This is
not protection against a deliberate same-user caller fabricating that complete
layout. The handle also does not make the CLI check-to-write interval atomic
against external writers.

## Private registration and admission contract

The schema-versioned registration journal is operational attribution, not a
capability. It contains an installation identifier, scope (`production` or
`development`), node/profile identity, listener and mount, loopback backend,
consent scope, operation ID, state and acknowledged CLI result. It contains no
Tailscale authentication token. A `DevelopmentWorkflow` holds the manager only
inside the current Go process, after the Go constructor rechecks explicit opt-in,
the exact Darwin/arm64 candidate, fixed ports, private roots, the complete
isolated XDG/release/web/runtime layout, the private relay environment and
marker, the current user service's environment file, production-root separation,
and the local Herdr socket binding. The manager repeats the layout checks before
real operations. These checks do not prove that a shell launcher was used, and
are not a privilege boundary against a deliberately fabricating process running
as the same user: that user can construct a matching private layout and
configuration. Paths, marker contents, and journal data alone do not grant
operations. Ordinary config loading, `serve`, `NewOwned`, and standalone
`tailscale-cli` mutation commands still refuse CLI-backed startup/operations.
Create/validate private roots as real, non-symlink mode-0700 directories and
journal files as mode 0600. Serialize Herdr writers for the same user/node with
a shared private lock; this does not lock external CLI writers or other UIDs. Reserve each loopback backend port durably in that
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
registered route is degraded and is not automatically recreated. The public
`dev-tailscale-cli reconcile` action records only an operation-ID-bound,
unambiguous exact-present or listener-absent observation for a pending/uncertain
journal; it never dispatches a Serve command. `repair-missing` is a separate
foreground action for an acknowledged or previously reconciled-absent
registration, requires a complete fresh absence check and an exact typed
confirmation bound to both the route tuple and prior operation, then performs
one fresh publication and readback. `abandon-missing` requires the foreground
relay stopped, the backend listener stopped, and fresh proof that neither the
selected listener nor any route to the backend exists; it records
`reconciled-absent` and releases only that registration's reservation without a
Serve mutation. Conflicting or incomplete observations refuse all three paths;
none adopts an observed route. Explicit repair and explicit unpublish require
fresh preflight and consent. Transport-selection helpers run a local-only
journal/reservation guard before changing away from
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

### Supervised owner development, phone smoke, and cleanup runbook

This runbook is for the already-authorized owner-operated development exercise
on one Darwin/arm64 Mac, the exact supplied App Store Tailscale 1.102.4 profile,
and the owner's designated phone. It covers one scoped persistent route and the
original physical-phone E2EE assessment, including device-role checks and
credential reconnect. It does not enable production, install or activate a
service, migrate production, qualify other devices/platforms, or change any
other Serve route. The implementation worker does not run these live
steps; the supervising assistant executes them only after exact-final-SHA
ordinary/native/extracted hosted checks and configured independent reviews are
complete. Do not substitute a node, account, profile, device, route, or port.

Use a private evidence directory outside the checkout with `umask 077`. Do not
save raw Tailscale account/status output, pairing links, QR payloads, tokens, or
phone screenshots in Git, CI artifacts, chat, or logs. Record only a redacted
node/profile label, release/instance identity, route tuple, readiness results,
and before/after route comparison. Stop and retain the journal and recovery
state whenever identity, readback, readiness, or a command result is uncertain.

1. **Read-only preflight and coexistence snapshot.** Confirm the current
   checkout's exact SHA matches the hosted ordinary, native Darwin/Linux, and
   extracted-release results and independent review is complete. Verify the host
   is Darwin/arm64 and the selected binary is the exact App Store 1.102.4
   candidate. Check the Herdr executable and `HERDR_SOCKET_PATH` identify the
   owner's active Herdr Unix socket (`test -x "$HERDR_BIN"` and
   `test -S "$HERDR_SOCKET_PATH"`); do not replace HOME with development state.
   Read installed service status and hash/record only the metadata needed to
   prove its configuration is unchanged; do not start, stop, edit, or migrate it.
   Run the read-only CLI sequence `status --json`, `version --json --daemon`,
   and `serve status --json` with the selected absolute executable. Keep raw
   status private. Save a private, complete pre-publication Serve snapshot and a
   sanitized summary of every existing route. Stop for a logged-out daemon,
   permission failure, profile/version mismatch, wrong node/account, malformed
   or unknown schema, pending/uncertain journal, or any ambiguity. Do not retry
   around refusal or clear an existing listener.
2. **Development preflight and scoped publication.** Start only
   `make dev-tailscale-cli` interactively. Its Go validator must independently
   check the explicit opt-in, non-overlapping production/service roots, private
   isolated roots and config, release/web identity, local Herdr socket, and
   fixed tuple before CLI inspection or mutation. The only allowed route is
   HTTPS 8443 -> `http://127.0.0.1:18377`, with the plugin listener on
   `127.0.0.1:18378`. Review the complete Serve snapshot and the four displayed
   residual risks. Enter only the exact node/origin/HTTPS/backend phrase shown
   by Go on stdin. There is no environment-variable consent. The fixed plugin
   UDP listener must bind before the backend can become ready; an occupied
   `127.0.0.1:18378` aborts startup before any Serve publication. Go waits for
   the foreground server's own backend-bind signal, then obtains a process-local
   listener lease across readiness, durable publication intent, Serve dispatch,
   and readback. Shutdown cannot release that listener until the lease ends; if
   it was released first, publication refuses even if a foreign matching health
   responder has rebound 18377. A health response alone is not ownership proof.
   Any occupied port, conflicting/unrecorded mapping, identity drift, or
   incomplete parse cancels without freeing ports, adopting routes, or touching
   production.
3. **Readback, HTTPS, bundle, and local readiness.** After Go reports setup
   ready, read `serve status --json` again and compare the complete supported
   configuration: exactly the intended HTTPS 8443 `/` proxy to
   `http://127.0.0.1:18377` may have been added; every unrelated route must match
   the before snapshot. Confirm the plugin UDP listener is 18378 and the local
   backend is bound only on `127.0.0.1:18377`. Check
   `http://127.0.0.1:18377/readyz` and `/healthz` for ready status, the expected
   transport, instance, control-run ID, relay version/revision, and bundle
   version/revision/hash. Without `-k`, redirects, or a custom trust bypass,
   request `https://<canonical-node>:8443/healthz`; require the trusted system
   TLS/hostname check, matching instance header/body, ready status, and identical
   release/bundle identity. Verify the configured `HERDR_PHONE_APP_URL` serves
   the exact release's `version.json`, descriptor, and assets. The Go admission
   path also rechecks trusted HTTPS and the complete phone-app bundle before the
   one-use invitation is armed. If any value differs, do not scan the link or
   attempt a repair.
4. **Owner phone enrollment and E2EE smoke.** Use only the designated owner
   phone, signed into the intended tailnet with its normal Tailscale connectivity.
   Confirm the setup link's HTTPS app origin is the reviewed phone app and the
   relay origin is the verified node route. The Go foreground process prints the
   one-use QR/link only after route, HTTPS, bundle, and admission checks pass.
   Treat it as a secret: show it only to the owner and do not copy it into notes
   or evidence. Scan/open it in the installed Herdr app, complete the one-use
   bootstrap enrollment as the owner's intended controller device, and confirm
   that the phone reaches the relay and completes the normal E2EE enrollment /
   authenticated WebSocket handshake. Perform only a harmless read-only agent
   inventory check. Record the controller role and assess controller/reader
   permissions as separate cells using only device(s) already within the
   owner-authorized phone assessment; reader access remains read-only and
   controller-only permissions must remain distinct. Do not send prompts,
   control an agent, upload data, add a device outside that assessment, or reset
   credentials. If a role cell is not safely exercised, leave it pending rather
   than infer it from source/CI. Disconnect/reconnect once and confirm the
   enrolled device credential resumes without re-pairing. If the app reports a
   trust, identity, E2EE, readiness or role problem, stop; do not print a second
   invitation or clear existing device state.
5. **Scoped cleanup and before/after comparison.** End the phone session, stop
   the foreground relay with Ctrl-C, and prove pairing socket removal and that
   backend TCP 18377 is free (`lsof -nP -iTCP:18377 -sTCP:LISTEN` returns no
   listener). Capture a new complete private `serve status --json` snapshot and
   verify the only eligible removal is the journaled node's exact HTTPS 8443
   path `/` -> `127.0.0.1:18377`; unrelated mappings must still match the
   pre-publication snapshot. Run
   `HERDR_DEV_TAILSCALE_CLI_ENABLE=1 relay/dev-tailscale-cli.sh unpublish` and
   enter only its exact typed node/origin/port/backend confirmation. Go performs
   fresh identity/schema/route checks and issues only the scoped `serve ... off`
   operation. Read back the complete Serve configuration and compare: the exact
   development route is absent, every unrelated route is unchanged, and
   production service/configuration hashes and liveness match the pre-run
   snapshot. Keep the `removed` journal and private development state; do not
   delete roots to make status appear clean. Do not use `serve reset`, stop a
   production service, or remove a different route.
6. **Uncertainty and report.** If a CLI write times out, acknowledgement/readback
   is lost, the complete Serve state changes unexpectedly, the route or node
   mismatches, a socket/port is occupied, or any step is ambiguous, stop. Retain
   the exact journal, reservation, and private evidence; run only read-only
   status/recovery to inspect. Never retry a possibly dispatched mutation,
   manually edit/erase recovery state, or remove another route. Report the
   development-only phone-smoke outcome with secrets and raw identity data
   redacted. This does not qualify production, installed-service behavior, other
   phones, sleep/wake, migration, or release support.

This is not a production deployment or migration instruction. Production and
installed-service activation remain compile-time disabled.

## Source entrypoints and current gate

Real Tailscale CLI access and CLI-backed server startup are confined to the
Go-owned development workflow. The exported `Client` has no exported real-binary
constructor; its subprocess runner and real constructor are package-private.
`ResolveBinary` and the standalone `resolve-binary` command only verify/select a
path and never run it. `activation-check` is local policy reporting and never
contacts Tailscale. Standalone `tailscale-cli preflight`, status, recovery, and
mutation operations refuse with `ErrWorkflowRequired` before executable
selection or state access.

| Public entrypoint | Real CLI access | Required admission / outcome |
| --- | --- | --- |
| `tailscale-cli resolve-binary` | No | Filesystem-only executable selection; no process construction. |
| `tailscale-cli activation-check` | No | Production remains disabled; development output points to the profile-checked workflow but grants no server or CLI authority. |
| `tailscale-cli preflight` and other standalone inspection/manager operations | No | Refuse before executable or state access. |
| `dev-tailscale-cli preflight` | Read-only only | `PreflightDevelopmentWorkflow` validates the complete private layout, explicit opt-in, exact Darwin/arm64 profile path, and fixed tuple before status/version/Serve inspection; returns no manager and cannot start a server. |
| `dev-tailscale-cli setup` | Yes, after consent | Go validates the complete private layout and requires exact typed confirmation against the configured node/origin before CLI contact; read-only preflight then verifies that identity before any route mutation. |
| `dev-tailscale-cli` update/status/recover/reconcile/assert-ready/release/repair-missing/abandon-missing/unpublish | Yes | `NewDevelopmentWorkflow` validates isolation before CLI preflight and retains the manager only in-process; operations repeat isolation, identity, profile and fixed-tuple checks. Reconcile and missing-route recovery require exact operation-bound operator input and never infer ownership from observed Serve state. |
| `config.Load` / `serve`, `app.New`, `app.NewOwned` | No | Refuse CLI transport startup without a workflow. `LoadDevelopmentCLI` and `app.NewDevelopmentCLI` require the same bound workflow and exact tuple. |
| `relay/dev-tailscale-cli.sh` | No direct CLI | May use `resolve-binary` for filesystem selection, then calls only `dev-tailscale-cli preflight` for real-CLI reads; it never calls standalone preflight or a Tailscale executable itself. |
| Installed CLI service/setup wrappers | No in shipped builds | The production activation check is false; wrappers stop before Tailscale CLI access or relay startup. They are not an alternate development workflow. |

The development command validates the explicit opt-in, production/service
separation, complete private layout and fixed port tuple. Setup first requires
exact typed confirmation bound to the private configuration's node and origin,
then performs read-only CLI preflight and verifies that identity before any
route mutation. It retains the manager only in-process, starts the app server,
and uses the local control API for admission and the authorized one-use owner
setup link. Status/recovery and scoped cleanup use the same workflow boundary
and repeat the Go isolation checks. This is an accidental-bypass/safety
boundary, not proof of launcher provenance or a privilege boundary against
deliberate same-user fabrication. The shell launcher supplies isolated
build/runtime paths but does not create separate relay/manager processes or
invoke route mutations itself.

The hosted command-level inventory is `TestDevelopmentTailscaleCLICommandEntrypointInventory`
in `cmd/herdr-mobile-relay/dev_tailscale_cli_entrypoint_test.go`; it follows
this table and covers the shipped `main` dispatcher, refusal cases, and a
marked synthetic preflight.
Ordinary config loading and app constructors cannot start CLI-backed service
mode; `LoadDevelopmentCLI` and `NewDevelopmentCLI` require the in-memory
workflow handle.

The foreground workspace uses `.dev-tailscale-cli/` with separate configuration,
release, cache, runtime, and registration roots. The supplied profile fixes
HTTPS Serve 8443 -> loopback relay backend 18377 and plugin listener 18378;
overrides are refused by the Go environment validator, config, workflow, and
manager operation boundaries. The manager revalidates the private directories,
relay environment, marker binding, current release/web layout, installed-service
environment, production-root non-overlap, and shared coordination root before
real operations. A marker or binary path is not authorization. A determined
same-user process can fabricate this complete environment; that residual is
documented and is not presented as an OS privilege boundary. The launcher stages each complete binary/web pair in a versioned
release directory and atomically swaps a single `current` symlink, retaining the
prior coherent release if staging or cutover fails.

`TailscaleCLIProfilesEnabled` remains false in ordinary and fixture builds.
The exact App Store candidate has a separate development-qualification bit; it is
not runtime-qualified. Production and installed-service activation remain
refused. The `herdr_tailscale_test` build hook only admits a CLI executable
carrying the explicit synthetic-fixture marker; ordinary installed Tailscale
executables are rejected by the package-private real-client constructor before
execution. The fixture hook is
not runtime qualification or production activation. Current-revision ordinary,
native and extracted-package CI must establish the fixture results; no live
Tailscale CLI, daemon, tailnet, service, phone or Herdr socket was contacted by
this implementation phase.

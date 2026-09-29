# CLI-backed Tailscale Serve contract

**Status: implementation contract draft; not independently reviewed.** The new
transport is named `tailscale-cli`. It is a distinct, persistent CLI-owned
background Serve route and is not an alias for the existing foreground
LocalAPI transport (`tailscale`) or operator-owned transport
(`tailscale-external`). The code and fixture work do not establish runtime
support. P6 live enablement and operator risk acceptance are pending; no profile
is currently enabled for ordinary activation.

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
state root. Use bounded deadlines, stdout and stderr, child/pipe cleanup and
redacted errors. Never log raw status/config/version JSON.

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
Tailscale authentication token. Create/validate private roots as real mode-0700
directories and journal files as mode 0600. Serialize Herdr writers for the same
user/node with a shared private lock; this does not lock external CLI writers or
other UIDs. Persist intent atomically with file and parent-directory sync before
dispatch. Never overwrite corrupt, copied or mismatched recovery state.

Persisted states are `unconfigured`, `publish-pending`, `registered`,
`publish-uncertain`, `remove-pending`, `remove-uncertain`, and `removed`.
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
proof that a dispatched mutation was acknowledged. A disappeared registered
route is degraded and is not automatically recreated. Explicit repair and
explicit unpublish require fresh preflight and consent. Routine service
stop/restart retains the persistent route; only the relay process stops. Phone-
managed package updates remain refused for CLI-backed installations. After
separate profile qualification and activation, `relay/tailscale-cli.sh update`
provides an interactive operator-managed package procedure: it checks the exact
journaled route before download and after restart, retains the route and journal,
and restores the previous release/service if recovery fails. It does not repair
or remove Serve routes or guarantee remote connection drain. Uninstall must preserve its journal until route
removal is acknowledged or the operator explicitly chooses to leave the route.
An unresolved previous route blocks repurposing its backend.

Start the app backend loopback-only and keep application/WebSocket admission and
pairing closed until the verified HTTPS origin, current instance/run/transport/
release, exact complete web bundle, trusted OS TLS/hostname validation and
application readiness all pass. Preserve the existing E2EE, device roles,
revocation, invitation durability and credential reuse. Never reset devices on
restart or infer authorization from tailnet membership. Pairing links remain
operator-only actions and are never written to background-service logs. Monitor
identity and route drift with bounded read-only polling; close admission on
mismatch and do not auto-repair. Polling is not an instantaneous revocation
watch.

Production activation is currently hard-gated off: a recognized source/fixture
profile is not a runtime-qualified profile. There is no environment-variable,
config-file or test-fixture switch that makes a real profile live-qualified.
Existing `tailscale` and `tailscale-external` configuration contracts remain
unchanged.

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
| Uninstall | Require explicit remove or leave choice; do not erase the only unresolved recovery record. |
| Transport switch | Decide old persistent-route disposition; unresolved removal blocks backend reuse. |

Four limits must appear in consent and recovery guidance. Operator acceptance is
**pending live enablement**:

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

### Proposed live-test runbook and future decisions (not authorization)

Before any P6 access, record the exact release, OS, executable path, client and
daemon profile/version, node/account identity, target backend/listeners, unrelated
Serve state and selected HTTPS origin in a sanitized record. Begin with a
user-selected unused development HTTPS listener and isolated development state.
Show the exact read/publish/readback operations, expected persistent route,
residual risks and cleanup choice. Obtain explicit confirmation of the named
node/account, exact route/ports, expected mutations, cleanup policy and permission
to contact the CLI/daemon and HTTPS endpoint. Check and record unrelated state
before and after; stop on any mismatch or uncertainty and retain recovery data.
Installed-service migration needs separate permission. Physical App Store, phone,
login/sleep/wake/update and release tests remain separate cells. Writing this
runbook grants no permission to execute it.

## Source entrypoints and current gate

The Go `tailscale-cli` subcommand exposes read-only `status`/`recover`, strict
`assert-ready`, and separately consented `publish`/`unpublish`; its
`activation-check` reads only the compile-time P6 gate. The relay shell lifecycle
routes those actions through the journal manager, installs only a per-user
service after `assert-ready`, and keeps stop separate from unpublish. The
foreground development entrypoint uses `.dev-tailscale-cli/` with independent
configuration, release, cache, runtime, and registration roots.

These source paths do not enable the transport: `TailscaleCLIProfilesEnabled`
remains false. Consequently setup, service installation, route commands and the
development launcher refuse before any Tailscale CLI/daemon access. Fixture E2EE
enrollment, server reconstruction with durable credential reopen/re-arm,
read-only registration recovery, exact shell-unpublish consent, concurrent
production/development scope registrations on a synthetic node, and fake-only
operator-managed package-update/service fixtures have passed. Real profile
coexistence and live runtime qualification remain outstanding. Source support
and fake-dispatch fixtures are not qualification or permission to activate.

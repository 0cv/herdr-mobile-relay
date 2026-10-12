# Pane-size lease containment (issue 60)

## Release scope

This build delivers Stage A containment only. The public manager constructor
makes the direct-TTY backend unavailable; no operator flag, OS, Herdr version,
legacy capability, or saved app preference can activate it. Both initial
`push_config` and later `herdr_status` capability payloads use the same server
eligibility predicate as command dispatch. Acquire and renewal share
`lease_pane_size`; both are rejected before process lookup or TTY I/O. Release
remains an idempotent owner-scoped cleanup command. There is no persisted lease
state to restore on process startup. Disconnect, sweep, and shutdown of a new
production manager have no terminal mutations to perform.

The app saves **Resize Session**, default off, independently of height. This
build keeps the effective resize predicate false even against an old relay;
the saved switch expresses consent, not present backend support. Height remains
saved but disabled. Controller fallback preserves columns and horizontal
scrolling; reader rendering and normal controller actions remain unchanged.
No lease ownership registry or immediate-release protocol is introduced while
acquisition is impossible. Any future backend must qualify those active
lifecycle paths before being made available.

## Operator configuration

`HERDR_PANE_SIZE_LEASE_ENABLED` defaults to `false`. Parsing uses Go's boolean
syntax (`true`/`false`, `1`/`0`, and supported case variants); missing or invalid
values mean false. This setting is **restart-only** and necessary, but not
sufficient: backend availability is checked separately and is false in this
build. There is no live configuration subsystem.

The checkout root `.env` is not the relay configuration. For a background
service, put overrides in its existing `HERDR_RELAY_ENV` file (normally the
plugin configuration's `relay.env`). Service wrappers load and export that file
on each start; service installation/update retains the recorded environment
file. Do not rely on a terminal's exported variable reaching launchd/systemd.
The wrapper regression checks that this setting reaches a synthetic relay.
No service/infrastructure change is performed by the source change or tests.

Default new relays protect cached old apps. A future operator-enabled qualified
backend could still receive automatic requests from old apps: the gate is not
retroactive user consent. A new app cannot fix an old relay's backend for other
clients. Previously altered geometry is not tracked by this process and is not
automatically recovered. Stop further leasing, then use a normal local resize.
A cleanup acknowledgement is not evidence that geometry was restored.

## Remaining gates

**Stage B (identity-safe direct-TTY backend) is not pursued.** Any PTY resize
that Herdr does not perform leaves Herdr's grid at the layout size while the
application draws at the leased size — the garbling reported in issue 60. No
amount of TTY identity or lifecycle care fixes that, so the legacy backend is
kept unavailable rather than hardened.

**Stage C (coordinated resizing) needs Herdr.** The contract is an absolute
rows/columns operation owned by Herdr that updates terminal grid and PTY
together, is scoped to a terminal identity (`PaneInfo.terminal_id`), defines
multi-client constraints, release/expiry/restart semantics, and a resize
revision for conditional restore. Herdr 0.9.3 (socket protocol 22) has no such
method: `pane.resize` changes split ratios only. Relative split resizing is not
this contract, and there is no silent fallback to `stty`. Verify the running
server's protocol before implementing; the installed CLI version is not
capability evidence.

**Phone cursor** is not rendered: `pane.read` returns text without a cursor
position and the app draws no cursor. Key delivery works. This is separate from
containment.

Verification evidence, human phone observations and review history are in
[issue-60-stage-a-qualification.md](issue-60-stage-a-qualification.md).

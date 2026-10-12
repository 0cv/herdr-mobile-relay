# Pane-size lease removal (issue 60)

## Release scope

Resizing the shared terminal from the phone is not available, and the legacy
direct-TTY backend that once did it has been deleted rather than carried
behind a gate. There is no operator flag, no app preference, and no lease
state: nothing to enable, nothing to restore on startup, nothing to release on
disconnect or shutdown.

The relay never advertises `pane_size_lease` or `pane_size_lease_rows`. The
wire commands are still recognized so an older app gets a clean answer:
`lease_pane_size` (acquire and renew) fails with "Resize Session is unavailable
on this relay" before any process lookup or TTY I/O, and `release_pane_size`
completes as an idempotent no-op. Both remain controller-only mutations with
the same target validation as before.

The app has no Resize Session or height switch; a saved preference from an
older app is ignored. Controller terminals keep the desktop width and scroll
horizontally; reader panes wrap to the phone width. Pane frames are no longer
flagged as settling after a resize, because no resize can happen.

Geometry altered by an older relay is not tracked by this release and is not
recovered automatically: stop the old relay, then use a normal local resize.

## Remaining gates

**An identity-safe direct-TTY backend is not pursued.** Any PTY resize that
Herdr does not perform leaves Herdr's grid at the layout size while the
application draws at the leased size — the garbling reported in issue 60. No
amount of TTY identity or lifecycle care fixes that, which is why the legacy
backend was removed instead of hardened.

**Coordinated resizing needs Herdr.** The contract is an absolute rows/columns
operation owned by Herdr that updates terminal grid and PTY together, is
scoped to a terminal identity (`PaneInfo.terminal_id`), defines multi-client
constraints, release/expiry/restart semantics, and a resize revision for
conditional restore. Herdr 0.9.3 (socket protocol 22) has no such method:
`pane.resize` changes split ratios only. Relative split resizing is not this
contract, and there is no silent fallback to `stty`. Verify the running
server's protocol before implementing; the installed CLI version is not
capability evidence.

**Phone cursor** is not rendered: `pane.read` returns text without a cursor
position and the app draws no cursor. Key delivery works. This is separate from
resizing.

Current removal verification and historical, candidate-bound containment
observations are recorded separately in
[issue-60-stage-a-qualification.md](issue-60-stage-a-qualification.md).

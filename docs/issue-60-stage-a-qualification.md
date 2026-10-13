# Issue 60 — opt-in resizing, removal and historical containment evidence

Issue: https://github.com/0cv/herdr-mobile-relay/issues/60
("Resize session garbles the desktop view"). Behavior and operator notes are in
[pane-size-leasing.md](pane-size-leasing.md). This file records what was
verified, by whom, and the limits of that evidence. The complete working
history (plans, review rounds, failed attempts, fixture details) is archived
outside the repository under
`~/.pi/workflows/recovery/issue60-handoff-20261011T090823Z/`.

## Option 2: opt-in resizing (assets 405)

This supersedes the removal below. The owner chose to bring Resize Session
back as a per-device opt-in, default off, accepting that the desktop and SSH
views of a leased pane look garbled while the phone holds it (Herdr 0.9.3 /
protocol 22 has no grid+PTY resize API). Design and residual risks are in
[pane-size-leasing.md](pane-size-leasing.md).

Implemented:

- Relay advertises `pane_size_opt_in` / `pane_size_opt_in_rows` on darwin and
  linux only, never `pane_size_lease`; `lease_pane_size` without `opt_in: true`
  fails before any Herdr, process or TTY access; `release_pane_size` accepts
  `immediate`. Lease read geometry applies only to the lease holder;
  `resize_settling` is back.
- New `internal/panesize`: direct `TIOCGWINSZ`/`TIOCSWINSZ` on a descriptor
  opened before a fresh `pane.get` / `pane.process_info` / `ps` re-validation;
  confirmed-gone versus unknown outcomes; a restore-pending state with at most
  five attempts in thirty seconds and one warning; generation fencing;
  per-dimension restore; bounded shutdown restore.
- App: Resize Session and Lease Terminal Height switches with the disclosure,
  gated on controller role, the opt-in capability and the switch; immediate
  release of every owned lease (including panes in navigation grace) when the
  switch is turned off; late replies ignored.

Automated verification by the implementation agent, 2026-10-13, on the initial
assets-404 candidate (macOS arm64 host; all exited 0 unless stated):

- `gofmt` clean; `go build ./... && go vet ./...`; `go test ./...`;
  `go test -race -count=1 ./internal/panesize ./internal/app
  ./internal/coordinator ./internal/transport`.
- Real-PTY tests on macOS: ioctl get/set through the production
  implementation, a mismatched device number refused without writing either
  PTY, and a held slave descriptor pinning its `/dev/ttysNNN` name across
  eight new allocations (reused immediately after release). The Linux variants
  (`TIOCSPTLCK`/`TIOCGPTN`) were compiled and vetted with `GOOS=linux` but not
  executed.
- `GOOS=windows go build ./...` fails exactly as at the previous commit in
  packages that do not build on Windows (`internal/herdr`, `appdeploy`,
  `profiles`, `pibridge`, `cmd/fake-herdr`); the non-darwin/linux build of
  `internal/panesize` was type-checked with `GOOS=freebsd go vet`.
- `go test -count=1 ./tests/blackbox -run PaneSize` against the compiled relay.
- `make production-path-audit`; `make frontend-check` (567 unit tests / 33
  files); one `make web-release` (assets 404); `make web-release-check`
  (`frontend/dist` equals `web`, 222 Chromium/WebKit journeys, four real-relay
  fixture journeys).

Parent verification on that initial candidate: **`make check` exited 0**,
including shell fixtures, all Go tests/race tests, 567 frontend tests,
222 browser journeys plus four real-relay fixture journeys, four-platform
cross-builds and release bundles. A separate uncached macOS manager run passed
its real-PTY tests without skips. Read-only requests to the running Herdr
confirmed the `pane.get` and `pane.process_info` response shapes; no terminal
was resized.

Independent review `0902c9bb…` requested changes: a disconnect could scan before
an acquisition inserted its lease (P1); busy-pane waiting could delay the fixed
cleanup deadline (P2); and Settings needed an explicit scrollback-loss warning
(P2). Parent corrections check cancellation under the disconnect scan's mutex,
start cleanup when the last lease ends (including the original expiry time for
a delayed sweep), and disclose scrollback truncation/loss. Deterministic tests
reproduced both backend defects before the fixes, then the complete manager
suite passed uncached with `-race -count=3`. Assets **405** include the warning.
The first review attempt `8d50265f…` failed immediately on a provider rate limit;
it reviewed nothing. Follow-up `e4190083…` confirmed the insertion race and
warning fixes, but caught a second deadline race: a successful acquisition
finishing after release could erase release-created cleanup. Completion now
supersedes cleanup only while live leases remain. A write-boundary regression
covers first acquisition and renewal, both disconnect and immediate release;
it preserves the original deadline/generation and proves deadline exhaustion
makes no additional lookup or write. The pre-fix failure is retained.

Final independent reviewer `f83fe68a…` accepted that correction with no new
findings, without executing tests or accessing live panes. This is separate
source-review acceptance, not approval of the historical workflows below.
The complete manager suite again passed `-race -count=3`.

Parent final verification, 2026-10-13, after all code corrections:
**`make check` exited 0**, with `GOTELEMETRY=off` and existing Go caches exported.
This covered formatting/vet/all Go tests/all race tests, shell fixtures,
production-path audit, frontend lint/types/**567 tests / 33 files**,
build/budgets, generated/shipped parity, **222 browser journeys + four real-relay
fixture journeys**, four-platform cross-builds and release-bundle checks.
The directly captured exit status and full log are retained under the recovery
root's `option2-Hyc6r8/post-commit-race-make-check.{exit,log}`, alongside all
review receipts and failed/passing regression logs. `git diff --check` passed.
Assets remained **405**; no additional generation was needed.

**Not yet phone-verified.** No phone-driven resize or real desktop/SSH view has
been observed with this build. Linux real-PTY tests remain compiled, not run.
The dev tunnel and production relay have not been restarted or updated.

## Removal (Option B, assets 403, superseded by Option 2)

This supersedes the gated Stage A build below. The `internal/panesize` package,
`stty` backend, sweep/restore lifecycle, operator flag, both Settings switches,
and frontend acquisition/renewal machinery have been removed. The relay never
advertises resizing; legacy acquire/renew commands fail clearly, and release
remains an authorized, idempotent no-op. Controller horizontal scrolling and
reader wrapping remain. See [pane-size-leasing.md](pane-size-leasing.md).

Implementation agent `12eb1246-cc19-4b40-aab8-f1aaf1471479` reported:

- Go formatting/build/vet, all Go tests, uncached app/coordinator/transport race
  tests, production-path audit and service-credential fixture passed.
- Frontend lint/types, **552 unit tests / 32 files**, build and budgets passed.
- One `make web-release` generated assets **403**; `make web-release-check`
  passed generated/shipped equality, **206 Chromium/WebKit journeys** and
  **four real-relay fixture journeys**. Ten browser cases were removed with
  obsolete lease-preference behavior; layout regression coverage remains.
- The first ingress-test harness timed out because `Server.Run`, not the
  constructor, installs the production handler. The corrected unit harness
  mirrors that dispatch; the timeout remains a failed attempt.

Independent reviewer `de81fab4-6ad9-4830-9a16-3c241f91d903` accepted the complete
branch with notes, without running tests. It identified historical-document
ambiguity (corrected here) and a missing real-server dispatch regression.
`tests/blackbox/panesize_test.go` now sends both legacy wire envelopes through
the compiled relay, including obsolete geometry/client fields, renewal and
repeated cleanup. It complements, rather than replaces, the ingress and
role/target unit coverage. All eight blackbox subcases passed uncached.
Follow-up reviewer `aa2b1792-62e2-4e14-9e78-be55b07de180` independently verified
both corrections and accepted with no findings; it did not execute tests.

Parent final verification, 2026-10-12: **`make check` exited 0**, with
`GOTELEMETRY=off` and existing `GOMODCACHE`/`GOCACHE` paths exported. This ran
formatting/vet/all Go tests/all race tests, shell fixtures, production-path
audit, frontend lint/types/**552 tests**, build/budgets, shipped-asset parity,
**206 browser journeys + four real-relay fixture journeys**, four-platform
cross-builds and release-bundle checks. Full output and the directly captured
exit status (not a piped `tail` status) are retained in the recovery archive's
`final-removal-xg7jyx/make-check.log` and `make-check.exit`. The targeted
blackbox log and independent review receipt are retained beside them.
`git diff --check` also passed. No additional asset generation was needed.

No phone observation qualifies assets 403: the assets-401 report below remains
historical, not evidence that the removed switches exist in this build.
Coordinator viewport-only read metadata remains as a separate, non-mutating
cleanup opportunity; its client-supplied geometry is stripped at ingress.
No new resize backend, cursor overlay, deployment or phone retest is claimed.

## Historical Stage A delivery (superseded)

- Relay rejects `lease_pane_size` (acquire and renew) before any process lookup
  or TTY I/O; `release_pane_size` stays idempotent. The `pane_size_lease` and
  `pane_size_lease_rows` capabilities are not advertised unless the operator
  flag is set **and** a backend is available; no backend is available in this
  build, so the legacy direct-`stty` path cannot run.
- App: "Resize Session" is a per-device preference, default off, never inferred
  from the older height preference; height stays saved but disabled. Controller
  terminals keep the desktop width and scroll horizontally; reader panes wrap.
  Prompts, keys, approvals, uploads and conversation history are unaffected.
- Not delivered: coordinated resizing through Herdr (no absolute cols/rows API
  in Herdr 0.9.3 / protocol 22), and a phone cursor overlay (see below).

## Historical Stage A automated verification

Candidate: app `0.22.7`, assets `402`, relay source at `62115683` plus this
change set. All commands exited 0 unless stated.

Stage A tree before Option B, 2026-10-12:

- `git diff --check`; `go build ./... && go vet ./...`
- `go test ./...` including `tests/blackbox`
- `make frontend-check` (lint, svelte-check, 565 unit tests / 33 files, build,
  size budgets, service-worker builds)
- `diff -qr frontend/dist web` and `make web-bundle-check` (generated == shipped)

Worker turn 2 (2026-10-11), the Settings-copy repair:

- `make web-release` (regenerated assets 402)
- `make web-release-check`: 216 Chromium/WebKit browser journeys and four
  real-relay attention/conversation fixture journeys, zero retries

The earlier worker turn 1 ran
`go test -race -count=1 ./internal/panesize ./internal/app ./internal/coordinator`;
this was not a new execution in turn 2 or evidence for the final removal.

Earlier in the same change set (2026-10-09/10), a full
`GOMODCACHE="$(go env GOMODCACHE)" GOCACHE="$(go env GOCACHE)" make check`
passed (formatting/vet/tests/race, shell fixtures, production-path audit,
frontend checks, browser journeys, four-platform cross-builds, release bundle).
It predates the assets-402 copy change and is not the new Option B execution
recorded above.

## Historical real-session containment evidence (disposable, measured)

Herdr `0.9.3` in fresh named sessions with isolated HOME/config, phone-width
Chromium and WebKit views against the actual relay and shipped assets, with the
old height preference saved and no Resize Session consent:

| Session | Client | Program | PTY before/after |
| --- | --- | --- | --- |
| macOS local | Herdr client | Vim | 94×39 unchanged (Chromium and WebKit) |
| Linux local + real SSH client | Herdr client | Vim | 94×39 unchanged (both engines) |
| macOS local | Herdr client | idle OMP 18.4.12 | 93–94×39 unchanged |

No phone-induced PTY change was measured; macOS also confirmed an unchanged
shell PID. Screenshots show native Vim wrapping inside the wider scrollable
grid, not phone-width reflow. The first OMP attempts showed provider onboarding
instead of the idle UI and are recorded as failed observations; a later run
reached the idle UI via a configuration setting. These are browser screenshots
and PTY measurements, not interactive desktop pixel sign-off.

## Historical human phone evidence (assets 401 only)

Owner's Android/Chrome phone through the ordinary `make dev-tunnel`, against
app `0.22.7` / assets **401**, relay `v0.22.7-dirty` revision `62115683`,
using fresh disposable workspaces in the owner's Herdr: `w28:p1` (real Vim)
and `w29:p1` (idle OMP, isolated HOME, unusable credentials, no prompts).

Owner report, verbatim: *"all good. the cursor move on the laptop when pressing
the up down arrow on the smartphone, but no cursor is visible on the
smartphone."*

Checklist covered: both views and alignment, sideways scroll, Resize Session
off/on/off with no desktop resize, height switch disabled, Settings
persistence, rotation/background-return/reload, Vim arrow keys. Endpoint PTY
samples: Vim 47×154 and OMP 47×153, unchanged before/after.

Limits: human-reported passes, not instrumented measurements; Android/Chrome
versions not recorded; endpoint samples do not exclude transient resizes;
assets 402 differ from 401 only by the Settings hint text and were browser-
verified, not phone-verified. **Phone cursor visibility did not pass**: the
relay's `TerminalFrame` carries no cursor position and `TerminalView` draws no
cursor. Key delivery is confirmed. This is tracked as separate work.

## Review history and findings

- Original reviewed run `b429e184…` (plan: default-off leasing): 11 worker
  turns, 9/9 reviews consumed, not approved. Its findings: F001 (no real-phone
  result for the exact candidate), F002 (an exploratory installed-iOS
  `com.apple.webapp` absent-consent/height assertion failed; cause unresolved),
  F003 (disposable real-phone containment check incomplete). The owner later
  excluded installed-iOS/simulator/hosted-PWA qualification from scope; F002
  remains a recorded failure, not a pass. The phone report supplies later,
  candidate-bound evidence relevant to F001/F003; it does not close the old
  ledger or qualify assets 402/403.
- Handoff run `7f4af9a4…` (containment completion on the same tree): round-one
  reviewers 1–2 clean; reviewer 3 raised current F001 — the Settings hint said
  every terminal stays desktop-sized, which is false for readers. Fixed in
  `SettingsView.svelte` with unit and browser assertions (assets 402). All three
  round-two reviewers recorded F001 as resolved with no new findings; the run
  could not formally close because its evidence tooling rejected `.svelte`
  files. Its ledger remained `claimed_fixed`, not closed. Formal `approved`
  status was not reached; the dispositions above are reviewers' narratives,
  not accepted round completion. The separate Option B review above does not
  rewrite either workflow's history.

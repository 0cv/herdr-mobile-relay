# Issue 60 — Stage A containment: evidence record

Issue: https://github.com/0cv/herdr-mobile-relay/issues/60
("Resize session garbles the desktop view"). Behavior and operator notes are in
[pane-size-leasing.md](pane-size-leasing.md). This file records what was
verified, by whom, and the limits of that evidence. The complete working
history (plans, review rounds, failed attempts, fixture details) is archived
outside the repository under
`~/.pi/workflows/recovery/issue60-handoff-20261011T090823Z/`.

## Delivered

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

## Automated verification

Candidate: app `0.22.7`, assets `402`, relay source at `62115683` plus this
change set. All commands exited 0 unless stated.

Final tree, 2026-10-12:

- `git diff --check`; `go build ./... && go vet ./...`
- `go test ./...` including `tests/blackbox`
- `make frontend-check` (lint, svelte-check, 565 unit tests / 33 files, build,
  size budgets, service-worker builds)
- `diff -qr frontend/dist web` and `make web-bundle-check` (generated == shipped)

Same tree, worker turn 2 (2026-10-11):

- `make web-release` (regenerated assets 402)
- `make web-release-check`: 216 Chromium/WebKit browser journeys and four
  real-relay attention/conversation fixture journeys, zero retries
- `go test -race -count=1 ./internal/panesize ./internal/app ./internal/coordinator`

Earlier in the same change set (2026-10-09/10), a full
`GOMODCACHE="$(go env GOMODCACHE)" GOCACHE="$(go env GOCACHE)" make check`
passed (formatting/vet/tests/race, shell fixtures, production-path audit,
frontend checks, browser journeys, four-platform cross-builds, release bundle).
It predates the assets-402 copy change; no new aggregate `make check` is claimed
for the final tree.

## Real-session containment evidence (disposable, measured)

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

## Human phone evidence

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
  remains a recorded failure, not a pass. F001/F003 are addressed for this
  change set by the human phone evidence above.
- Handoff run `7f4af9a4…` (containment completion on the same tree): round-one
  reviewers 1–2 clean; reviewer 3 raised current F001 — the Settings hint said
  every terminal stays desktop-sized, which is false for readers. Fixed in
  `SettingsView.svelte` with unit and browser assertions (assets 402). All three
  round-two reviewers recorded F001 as resolved with no new findings; the run
  could not formally close because its evidence tooling rejected `.svelte`
  files. Formal `approved` status was therefore not reached; the dispositions
  above are the reviewers' own recorded assessments.

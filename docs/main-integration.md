# Pinned main integration checkpoint

## Provenance and scope

- Branch parent: `6707b3bde6f19e9b3e688d7659317adf88e94302`.
- Pinned main parent: `6e96b2c75115d829530e1c3a9c2ec50fd02e94de`.
- Recorded older main: `f0f41c2cc941a6f62d0459c519ed20eefe14e033`;
  merge base: `ef9f84351b5f257f1e8e0156057198fe8e644dd3`.
- Normal two-parent merge, `--no-commit --no-ff`, autostash explicitly disabled.
  No rebase, squash, reset, stash, force push, main push, release or deployment.
- Implementation provenance observed at 2026-10-01T19:06:08Z:
  `PI_PROVIDER=openai-codex`, `PI_MODEL=gpt-6.1-sol`, reasoning `xhigh`.
  Sol implementation authority ends at 2026-10-01T19:30:58Z; unfinished work
  must retain artifacts for the owner-authorized Opus 5.5 continuation.
- Unrelated working-tree documentation, plans, npm lockfiles and Python cache
  are excluded from staging and must remain untouched.

## Additional main commits inspected and included

The entire additional source/test/documentation diff from the older main to
this pinned target was inspected (generated parent bundles are superseded).
The reachable commits are:

- `6e96b2c7`: merge PR #53, Cursor conversation history.
- `43cc38eb`: merge current main and prepare 0.22.4.
- `ef9e7849`: Cursor timestamp/session-location review repairs.
- `78d832f1`: merge PR #56, idle-agent status.
- `5313d171`: remove redundant hollow class.
- `a53e6ded`: Cursor Go formatting.
- `ef39f894`: PR #53 review fixes.
- `9b5d961e`: Cursor location and full-history hardening.
- `1d8fb1ab`: Cursor agent-transcript history.
- `6128efd0`: ready/idle green-ring status.

These include nested/flat Cursor transcripts, configured roots, workspace-trust
and UUID lookup, full-history projection, UTC conversion, associated tests, and
ready-vs-unread-vs-unknown frontend indicators.

## Actual conflict inventory (34 paths)

```text
.github/workflows/check.yml
Makefile
QUICKSTART.md
cmd/herdr-mobile-relay/main.go
frontend/build-versions.json
frontend/src/lib/store.ts
go.mod
go.sum
internal/app/server_test.go
internal/herdr/events_test.go
internal/update/manager.go
relay/common.sh
relay/herdr-mobile-relay-service.sh
relay/install-service.sh
relay/install-systemd-user-service.sh
relay/plugin-build.sh
relay/start.sh
scripts/check-installed-release.sh
scripts/package-release.sh
tests/test_common.sh
tests/test_plugin_build.sh
web/_redirects
web/assets/ConversationHistory-382.js
web/assets/ConversationHistory-385.js
web/assets/ConversationHistory-395.js
web/builds/0.21.3-382-d94d3346c9244736/index.html
web/builds/0.21.3-385-49de75bc97e7ddc0/index.html
web/builds/0.22.4-395-5e441fcfc931a750/index.html
web/herdr-bootstrap.js
web/herdr-bootstrap.js.br
web/release.json
web/release.json.br
web/version.json
web/version.json.br
```

## Semantic resolution rationale

- Dispatch imports/commands and Makefile/CI checks are unions. Main readiness,
  Pi bridge, native installer/recovery checks coexist with managed, BYO and CLI
  lifecycle checks. The closed shell job includes the pinned Bun binary directory
  for main's Pi tests without reopening caller-controlled runtime adapters.
  Go 1.27.1, Bun 1.4.0 and actionlint 1.7.12 remain pinned.
- Dependencies retain both parents' feature dependencies (including native
  modernc SQLite and Tailscale), with branch bbolt 1.4.2 and Go 1.27.1.
  `go mod tidy` reconciles metadata; no new feature dependency was introduced.
- App inventory test combines main's deterministic failure trigger with branch
  teardown-write tolerance. Event-bootstrap tests use main's shared collector,
  preserving branch's requirement to accept an event delivered just after the
  bootstrap buffer closes. Update launch combines the transport argument/veto
  with main's credential-scrubbed child command; its new credential regression
  is adapted to the combined signature, not removed.
- Service/installer source keeps foreground/CLI refusals before transaction or
  service mutation, and main's snapshot/staged installation/readiness/rollback.
  Existing legacy CLI service definitions are still refused, not migrated.
  Main's systemd serializer/parser supports complete-assignment quoting; the
  parser also accepts branch's earlier value-quoted assignments. Rewrites
  validate escaped values before writing and round-trip via that parser.
  Darwin retains real plist extraction and validated XML escaping.
- Packaged release checks retain both CLI disabled-dispatch and Pi installation
  fixtures. Packaging includes the native transaction helper now required by
  both merged installers, as well as all branch wrappers and `.env.example`.
  Background CLI dispatch scrubs legacy credential variables after reloading
  the environment, then retains the unchanged production activation refusal.
- Frontend keeps R-03 present-invalid/duplicate-selector atomic rejection and
  expired-link handling, main's confirmed browser opt-in and session-bound
  bootstrap opt-in, default iOS Home Screen deferral, identity/history/network
  handling and connection preservation. Guidance keeps intentional unused
  bootstrap renewal distinct from ordinary expiring invitations.
  Added unit and hosted Chromium/WebKit regressions combine explicit pairing
  with damaged metadata, duplicate selectors, cancel/storage failure and saved
  credentials; no invitation/socket is created on failure.
- Source was resolved before regenerating a single 0.22.4 asset version 396,
  greater than parent asset versions 385 and 395. `build-versions.json` has no
  history list; service worker 8 and notification icons 4 agree on both parents.
  Existing release policy replaces the web directory from one complete build,
  so no stale parent hash or mismatched compressed output is retained.
- Clean auto-merges were inspected at the app/readiness, updater environment,
  transport dispatch, setup/service/update/uninstall callers and frontend
  settings/store boundaries. Branch manager/public-command recovery source and
  activation constant have no changes versus the branch parent; late repairs
  `e4f4ffd8`, `1510b99c`, `6707b3bd` survive through branch ancestry and bytes.

## Verification boundary

No local test suite was executed. Local work is limited to formatting, static
checks, compilation and frontend bundle generation. All behavioral tests,
normal/race/tagged Go, shell/service/release fixtures, browser tests, four-target
native package smoke, extracted HTTPS/WSS/E2EE acceptance and native lifecycle
qualification run in disposable hosted CI on the submitted SHA. Ordinary and
native workflows must both be green before review readiness is claimed.
Mobile harness/device jobs remain intentionally disabled; this checkpoint
makes no real-daemon, physical-phone or production qualification claim.

Production activation, services, real daemon/tailnet/phone exercises, release
and deployment remain outside this integration, deferred to separately
qualified P6/enablement phases.

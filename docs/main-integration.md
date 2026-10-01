# Pinned main integration checkpoint

## Provenance and scope

- Branch parent: `6707b3bde6f19e9b3e688d7659317adf88e94302`.
- Pinned main parent: `6e96b2c75115d829530e1c3a9c2ec50fd02e94de`.
- Recorded older main: `f0f41c2cc941a6f62d0459c519ed20eefe14e033`;
  merge base: `ef9f84351b5f257f1e8e0156057198fe8e644dd3`.
- Normal two-parent merge, `--no-commit --no-ff`, autostash explicitly disabled.
  No rebase, squash, reset, stash, force push, main push, release or deployment.
- Implementation provenance, per the owner's timed model instruction (Sol
  until 2026-10-01T19:30:58Z, then Opus 5.5):
  - Sol (`openai-codex` / `gpt-6.1-sol` / `xhigh`, observed 19:06:08Z) wrote
    the merge `d740cdd4` (19:22:28Z) and follow-ups `0cdd529b` (19:25:10Z)
    and `1f47d543` (19:27:43Z), all committed and pushed before the cutoff,
    in workflow run `d452ea12-4153-4724-83df-8ea3b091ed97`.
  - The supervising assistant stopped that run at 19:31:47Z because its first
    worker turn was still running after the cutoff. It returned no handoff,
    was cancelled and unreviewed, and is not an approval.
  - Every later commit, starting with `9e77faef`, is by Opus
    (`anthropic` / `claude-opus-5-5` / `xhigh`, verified 19:32:47Z), after an
    independent audit of the merge, both parents, the conflict resolutions,
    clean auto-merges and the Sol follow-ups.
- `origin/main` has since moved beyond the pinned target; this checkpoint does
  not chase it. Corrections are forward commits on the task branch only.
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
  and the hosted runner's Node binary directory for main's Pi subprocess tests
  without reopening caller-controlled runtime adapters.
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
- `internal/herdr/events_test.go` is byte-identical to main: main's
  `collectBootstrapTestEvents` subsumes the branch's tolerance for an event that
  arrives just after the bootstrap buffer closes and additionally requires a
  clean end of stream, so no branch assertion is lost.

## Opus audit corrections (forward commits after the Sol follow-ups)

The audit re-read the combined merge diff, every conflict resolution, the
clean auto-merged callers of changed shell helpers and every hosted shell
fixture that had not yet run on the merged tree. Corrections:

- Root cause of the merged plugin rollback failure: Bash gives a bare
  `return` inside a trap handler the status from before the trap. The merged
  unit rewrite checks `systemd_quoted`'s status, so rollback of a recovered
  broken service (run from the `EXIT` trap) failed and left the old unit
  unrepaired. `systemd_quoted` and `require_user_service_context` (also on
  the launchd rollback path) now return explicit statuses, with a shell
  regression that calls both from a failing `EXIT` trap.
- Main's installer hint for an inventory protocol mismatch read `error_code`
  with a lenient text accessor, but the relay nests it under `inventory`, so
  the branch's strict top-level accessor dropped the "Run: herdr server
  live-handoff" guidance. The advisory now confirms a strict relay health
  document and then matches the exact nested code token; it changes no state.
- The conversation browser publishes a failed preparation before its worker
  releases the job, so an explicit retry in that window still reports the
  failure. Main's quota-retry test, already adapted on the branch, flaked under
  `-race` in that window; it now reissues only the explicit retry until the
  released job accepts it, then still requires a successful ready page.
- Main's Cloudflare public `/readyz` gate ran for any transport that kept a
  `CLOUDFLARED_CONFIG`, including CLI-backed Serve, whose exact route is
  verified separately. It now applies only to the Cloudflare transport. Shell
  regressions cover this scope and the compact nested live-handoff hint, and
  the Tailscale source contract requires the foreground credential scrub.
- Foreground managed and operator-owned Serve launchers start the relay
  directly, so they now drop raw `GH_TOKEN`/`GITHUB_TOKEN` after loading
  `relay.env`. This matches main's quick-start and service boundaries; the
  relay keeps only the private token-file path.
- Fixture composition only, with no assertion removed: main's native-installer
  fixture and the operator-managed CLI update fixture delegate strict JSON
  fields and readiness to the compiled relay. The branch CLI installer,
  refusal and uninstall fixtures now include the packaged native transaction
  helper, an exact readiness identity, hermetic `systemd-analyze`/`plutil`
  stand-ins, a reachable launchd user domain, private removal sentinels and
  main's complete-assignment `Environment=` quoting. Failed plugin-build
  assertions now report the newest fixture output and unit.

## Verification boundary

No local test suite was executed. Local work is limited to formatting, static
checks, compilation and frontend bundle generation. All behavioral tests,
normal/race/tagged Go, shell/service/release fixtures, browser tests, four-target
native package smoke, extracted HTTPS/WSS/E2EE acceptance and native lifecycle
qualification run in disposable hosted CI on the submitted SHA. Ordinary and
native workflows must both be green before review readiness is claimed.
The first merged ordinary run `36913821785` exposed an early shell-job failure:
main's Pi bridge test starts Node subprocesses, but the branch's closed PATH
omitted the hosted Node directory. The follow-up keeps the closed environment
and adds that explicitly resolved tool directory. This historical run remains
failed; only fresh exact-fix-SHA hosted evidence can verify the repair.
Run `36914151602` verified the Pi/install/credential shell cases progressed,
then exposed a composed start fixture missing the branch's `json-field`
dispatch: main's readiness helper could not parse the fixture manifest through
that selected relay. Start/plugin relay fixtures now delegate both scalar
parsing and readiness verification to the compiled merged relay helper. No
readiness assertions or behavioral cases were removed. This run also remains
historically failed pending fresh exact-SHA verification.
Ordinary run `36914460251` on `1f47d543` (native `36914459781` green) still
failed only in the shell job, silently inside the plugin build fixture. The
Opus corrections above were driven by these later failing shell jobs, each
historically failed and superseded only by fresh exact-SHA evidence:
`36916456422` (an intermittent Pi bridge Node fixture timeout that passed in
every other run), `36918051269`, `36919194468`, `36920097604` and
`36921473292` (plugin rollback; the last replayed the unit rewrite and
identified the trap-status cause) and `36922541123` (plugin build passed; the
development menu fixture's controlled PATH lacked Bash for main's portable
`#!/usr/bin/env bash` entrypoints, so the fixture now links the resolved Bash
beside its tool stand-ins) and `36923446618` (every shell fixture through the
speech wrapper passed; stable setup lost the nested live-handoff hint).
Ordinary run `36924614243` and native run `36924613528` were then green on
`ffa128f9`. The documentation-only successor `a5f04183` failed only its race
job (`36924668070`) in the conversation retry window above. Review readiness
requires both workflows green on the exact submitted SHA.

Mobile harness/device jobs remain intentionally disabled; this checkpoint
makes no real-daemon, physical-phone or production qualification claim.

Production activation, services, real daemon/tailnet/phone exercises, release
and deployment remain outside this integration, deferred to separately
qualified P6/enablement phases.

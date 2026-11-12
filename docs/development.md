# Local development

How to build, run, and test this project from a checkout, and how to fix the
local runtime problems that come up while doing it. Read this if you are changing
the relay rather than using it.

## Running from a checkout

```bash
git clone https://github.com/0cv/herdr-mobile-relay.git
cd herdr-mobile-relay
make dev-tunnel
```

`make dev-tunnel` builds the current Go source, interactively chooses Tailscale
Serve (when available) or a temporary Cloudflare tunnel, then builds the
frontend. Set `HERDR_DEV_TRANSPORT=tailscale` or `cloudflare` to skip the prompt
(required without a terminal). Tailscale serves HTTPS port 9443, proxying to the
dev relay
on localhost port 18375; the plugin uses UDP port 18376 by default. A conflicting
Serve port is refused; Ctrl-C attempts to remove only this dev mapping. Set
`HERDR_DEV_CONFIG_DIR` to move isolated state from `relay/.dev/`. The dev run
never uses the installed production relay or its port 8375. For Tailscale
prerequisites, see [transports.md](transports.md#tailscale-serve).

If a Pi agent directory exists, it installs or updates the live-command
integration there, including in noninteractive runs.
This changes the shared Pi profile, not the isolated relay state; the integration
remains after the tunnel stops. Run `/reload` in affected Pi panes after the
tunnel starts. Set `HERDR_DEV_PI_COMMANDS_INSTALL=0` to skip profile changes.
Other Pi profiles require their own installation via `relay/setup.sh --pi-install`.

## Common targets

```bash
make check             # all backend, frontend, browser, and release checks
make backend-check     # format, vet, tests, race detector, shell checks
make web-release       # replace committed web/ with a verified frontend build
make web-release-check # compare and browser-test the shipped web/ bundle
make relay-plugin      # link this checkout as a Herdr plugin
make stable-setup      # run the stable tunnel wizard with the installed relay
```

## Launch deadlines

Agent startup has a 40-second backend budget, followed by at most 12 seconds
for initial prompt delivery. The phone keeps launch requests pending for 60
seconds, allowing eight seconds for the final response. Both success and
initial-prompt warnings finalize the original request; duplicate requests with
the same request ID replay that result.

## Profile ownership and installation recovery

Clear and Restart start a replacement with the agent's launch profile. A
verified association between the profile and the terminal, pane, tab and
workspace identities wins; otherwise the profile matching the agent's reported
name (including aliases) is used. The replacement runs that profile's command,
so options from the original command line are not kept. With no matching
profile, the request is refused without any change.

Native conversation discovery or rollover does not establish ownership.
Verified records are stored privately in `profile-ownership.json` under the
relay runtime directory, and are usable after restart only once an accepted
inventory confirms the same target. Pending launches, incomplete observations
and removed profiles are not verified. Confirmed disappearance or terminal
replacement invalidates the association. Read-only access and explicitly
requested stop operations do not need a profile.

A corrupt ownership store disables verified associations, so Clear and Restart
use the reported agent name. Preserve the store for inspection and restore a
trusted backup while the relay is stopped. If no backup exists, an operator can
explicitly archive the damaged file and restart with an empty store; only new,
verified launches establish ownership again. Do not copy records to a
replacement terminal or edit a pending launch into a verified one.

Native installers stage definitions and preserve previous files, activation and
available readiness identity before changing a service. They require both local
`/readyz` identity/inventory and, for named tunnels, public readiness before
retiring the previous definition. A temporary public failure fails this install
attempt; it does not create a permanent runtime network-health requirement.
Gateway mode does not require a Cloudflare URL.

If rollback cannot be confirmed, its private recovery directory remains under
`$XDG_STATE_HOME/herdr-mobile-relay/recovery` (default
`~/.local/state/herdr-mobile-relay/recovery`). The `state` file identifies the
original paths and activation settings; `0`, `1`, and `2` contain any previous
current definition, legacy definition and environment file. A
`previous-ready.json` file, when present, records the previous verified runtime
identity. Inspect the native manager and these files before attempting another
installation; a retained directory means restoration is uncertain, not healthy.

Release credentials are excluded from ordinary child processes, including
profile probes, conversation readers, Git, clipboard and speech tools. The
updater alone deliberately receives the private credential-file pointer. The
plugin hook passes raw download credentials only to its installer, with tracing
disabled before token handling. Preserve unrelated PATH, proxy and agent config
variables when changing these boundaries.

## Testing a release candidate

Candidates are published as prereleases, which ordinary relays never install:
their update check resolves the latest stable release only. To run one:

```bash
herdr plugin install 0cv/herdr-mobile-relay --ref dev
```

Rerun that command to move to a newer candidate.

## Contributing

Work lands on `dev`: branch from `dev`, open pull requests into `dev`, and make
sure `make check` passes first. `main` only receives merges of `dev` at release
time, and a pull request into `main` from any other branch fails its check. See
`AGENTS.md` for the full branch and release flow.

## Toolchains

Backend development uses Go 1.27.3; frontend development uses Node.js
26.13.0 and Bun 1.4.4 (`bun install --cwd frontend`, then the `make` targets
above). Bun installs the packages and runs the repository's own scripts; Vite,
ESLint, Vitest and Playwright run on Node, because `bun run` honours their
`#!/usr/bin/env node` line whenever Node is on `PATH` (without Node it falls
back to Bun). CI installs both browsers natively (`bun x playwright install
--with-deps chromium webkit`); on Fedora, `install-deps` is unsupported and
native WebKit crashes, so `make frontend-browser` runs WebKit through
Playwright's official container via podman (Chromium runs natively — its dnf
dependencies are nspr nss dbus-libs atk at-spi2-atk cups-libs at-spi2-core
libXcomposite libXdamage libXext libXfixes libXrandr mesa-libgbm cairo pango
alsa-lib, per passportxyz/passport's fedora-install-playwright-deps.sh).
The Pi bridge subprocess regression in `make backend-check` also requires
Node.js 22 or newer on PATH. It uses Node's strict unhandled-rejection behavior
to verify that malformed metadata requests cannot terminate the agent runtime.
No additional npm dependencies are needed for this check.

Publishing the hosted web app (`make web-deploy`,
`make web-preview`) shells out to `npx wrangler`, which requires Node.js 22 or
newer on the publishing computer; CI and the relay's deploy action are exercised on
Node.js 26. `make web-deploy` then runs the public bundle verifier against
`WEB_ORIGIN` (the Pages domain by default; override it for a custom domain).
Packaged users need no toolchain at all.

### WebKit tests on Fedora

Do not install the Ubuntu-specific `libicu74` / `libjpeg-turbo8` packages or
symlink Fedora libraries to their ABI names. The version-matched official
Playwright container supplies WebKit and its dependencies. Podman must be
installed once (`sudo dnf install podman`); the image is downloaded on first
use and remains cached across runs and reboots. A Playwright version upgrade
fetches the matching new image.

Both browser test commands select the container automatically on Fedora:

```bash
make frontend-browser                    # Chromium and WebKit UI journeys
make frontend-browser-attention-release  # Chromium and WebKit relay/attention tests
# Focus only on the previously blocked engine:
HERDR_WEB_ROOT=../web bun run --cwd frontend test:browser:attention --project=webkit-attention
```

The attention runner keeps Bun, Go, and the isolated relay fixture on the host.
Only the WebKit browser runs in the container, with Playwright forwarding its
loopback traffic to the host's test HTTP and relay WebSocket servers. The
browser-control port is published only on `127.0.0.1`, on an automatically
allocated port, and the runner removes its container on exit without removing
the cached image. Test output and failure traces stay on the host. Ubuntu CI
continues to use native browsers; `HERDR_WEBKIT_CONTAINER=1` selects Docker for
hosts that explicitly want containerized WebKit. Directly invoking
`playwright test --config playwright.attention.config.ts` bypasses the wrapper;
use the package script or Make target instead.

The test-only `cmd/fake-herdr` binary provides deterministic Herdr CLI behavior,
response barriers, and process-control traces for black-box tests.

Installed-PWA device CI is documented in `docs/mobile-device-ci.md`. Its host-only
check does not replace the real Android Home Screen or iOS Home Screen runs;
macOS/Xcode is required for iOS, and each destructive device action requires a
run-owned disposable emulator or simulator marker.

## Grok history fixtures

The Grok fixture builder and regression scenarios contain synthetic content.
`internal/conversation/testdata/grok/rewind_updates.jsonl` is retained unchanged
from PR #49 commit `414a5c5306cf2d0f1bd4285ac46cec96266f390e`. That commit
attributes it to a reduced CLI 1.0.41 capture (ONE/TWO/THREE, rewind, FOUR).
The original capture is unavailable here; retaining that attribution is not an
independent live-provenance verification or a current-CLI compatibility claim.

Read-only string inspection of the installed Grok executable during adaptation
found `SessionUpdate::ImageCompressed`, `rewind_marker`, `target_prompt_index`,
`SearchReplaceOutput`, `SearchReplaceEditsApplied` and `tool_output_for_prompt`.
These corroborate protocol names, not complete serialization or replay rules.
The retained fixture supplies ordinary `session/update` messages and extension
`_x.ai/session/update` completion/rewind records. Synthetic hook/compression
records use the extension family; compression precedes the user image/text
chunks rather than splitting them. Edit fixtures exercise
`SearchReplace.EditsApplied.tool_output_for_prompt`, not raw file content.

The deterministic suite does not launch Grok or access existing conversations.
Run `go test ./internal/conversation -run Grok -count=1 -v` for local replay,
containment, pagination and full-file parsing scenarios; the live test is opt-in.

`TestGrokCLIIntegration` passed with installed CLI 1.0.70 on 2026-11-03,
confirming the earlier disposable exchange/edit observation. It creates fresh
sessions, a temporary working directory and isolated mutable state, using the
owner-authorized native login (including normal token refresh) without copying
credentials or resuming existing conversations. It verifies production lookup
and parsing of generated `updates.jsonl`, a user/assistant exchange, an exact
edit to a disposable file and projection of the actual
`SearchReplace.EditsApplied.tool_output_for_prompt` result. Observed method
families were `session/update` for user/assistant chunks and tool calls/updates,
and `_x.ai/session/update` for completion. Image/compression, hooks, rewind and
interjection behavior were not live-qualified; their fixture/source coverage
must not be mistaken for current CLI experiments.

With authorization to use native authentication, reproduce with
`HERDR_GROK_INTEGRATION=1 go test ./internal/conversation -run '^TestGrokCLIIntegration$' -count=1 -v -timeout 5m`.
When enabled, missing CLI capabilities, failed exchanges or missing expected
records fail the test rather than skip it. This live check does not replace the
broad Go/frontend gates or independent cumulative review.

## Herdr compatibility checks

The relay's minimum supported Herdr client is 0.7.5; 0.9.0 is the recommended
client for the full JSON inventory and workspace-management surface. The
installed client version is only one input: startup and the refresh loop ping
the running server and record its server version, protocol, endpoint generation,
and individual feature evidence. A stable endpoint generation does not imply
that every optional operation is supported.

Ordinary agent, pane, workspace, and tab inventory uses JSON operations. The
mobile terminal reads pane snapshots through `pane.read`, with a CLI fallback;
it does not attach through Herdr's separate binary direct-terminal transport.
Unprobed or unadvertised optional features are not compatibility failures.
Settings warns only for unsupported features and unsuccessful checks, not
`not_checked` or `not_advertised` evidence. Terminal-read support is checked at
startup and after reconnects using an empty explicit pane ID: Herdr's
`pane_not_found` refusal confirms the method without reading, scrolling, or
resizing a live pane. Pending reconnect checks are labeled as rechecks, not
failures. Event clients subscribe before taking a snapshot; reconnects refresh
the snapshot and do not replay all notifications missed while disconnected.

Workspace group close is a single explicit close operation over the current
workspace membership. It closes panes but never removes Git checkouts or
branches. Worktree removal remains a separate destructive operation with its
own dirty-checkout confirmation.

Use the fake Herdr binary or a temporary Unix socket fixture for tests. Do not
run production Herdr commands or mutate production state while checking these
paths.

## Core backend

`HERDR_RELAY_BACKEND=hc` selects herdr-core instead of the default `herdr`
backend; `HC_SOCKET_PATH` overrides its session socket. See
[herdr-core](herdr-core.md) for defaults and manual second-instance operation.

## Isolated core integration test

Run `HERDR_RELAY_HC_INTEGRATION=1 go test -v ./internal/hc ./internal/app -run '^TestRealCore'`
with `hc` on PATH. The tests start and stop their own core in a temporary directory,
with a separate session and configuration. They never touch the default core.
With `pi` on PATH, the run also checks Pi discovery inside a shell and as the
terminal program, using an isolated Pi configuration. Prompts and activity still
need a manual check before release; these tests do not start a model turn.
Without the environment variable or `hc`, these tests skip; Pi discovery also
skips without `pi`.

## Phone-side crash diagnostics

The production frontend installs raw DOM handlers before Svelte mounts. An
uncaught exception or rejected promise appears in a bottom **App error** banner;
tap it to dismiss it and allow a later error to be shown. Phones usually have
no accessible console, so include that text in a bug report.

For local `make dev-tunnel` diagnosis, set `HERDR_DEV_RUNTIME=1` before the
build. This enables Svelte's development runtime so invariant failures include
their data and indexes in the on-device banner. Release builds leave it off.

## Troubleshooting local runs

- **Port is busy:** `make dev-tunnel` uses 18375, Quick Start and the installed
  service use 8375; stop whatever already holds the one you need.
- **Herdr is not running:** start it with `herdr`, then retry the operation.
- **Agents are unavailable:** inspect `/healthz`; after a Herdr protocol update,
  run `herdr server live-handoff` and wait for the next relay poll.

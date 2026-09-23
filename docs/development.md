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

`make dev-tunnel` builds the current Go source and frontend, uses isolated ports
and state under `relay/.dev/`, and opens a temporary tunnel. It never uses the
installed production relay. If a Pi agent directory exists, it installs or
updates the live-command integration there, including in noninteractive runs.
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

Relaunching an agent requires a verified association between its profile and
terminal, pane, tab and workspace identities. Native conversation discovery or
rollover does not establish ownership. Verified records are stored privately in
`profile-ownership.json` under the relay runtime directory, and are usable after
restart only once an accepted inventory confirms the same target. Pending
launches, incomplete observations and removed profiles do not authorize relaunch.
Confirmed disappearance or terminal replacement invalidates the association.
Read-only access and explicitly requested stop operations remain available when
ownership is unknown; create a new agent with an explicit profile instead of
assuming a default executable for an existing pane.

A corrupt ownership store disables profile-derived relaunch. Preserve it for
inspection and restore a trusted backup while the relay is stopped. If no backup
exists, an operator can explicitly archive the damaged file and restart with an
empty store; only new, verified launches establish ownership again. Do not copy
records to a replacement terminal or edit a pending launch into a verified one.

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

Work lands on `dev`; open pull requests against it and make sure `make check`
passes first.

## Toolchains

Backend development uses Go 1.27.0; frontend development uses Bun 1.4 (`bun
install --cwd frontend`, then the `make` targets above). Playwright runs on
Bun. CI installs both browsers natively (`bun x playwright install
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
failure injection, and process-control traces for black-box tests.

Installed-PWA device CI is documented in `docs/mobile-device-ci.md`. Its host-only
check does not replace the real Android Home Screen or iOS Home Screen runs;
macOS/Xcode is required for iOS, and each destructive device action requires a
run-owned disposable emulator or simulator marker.

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

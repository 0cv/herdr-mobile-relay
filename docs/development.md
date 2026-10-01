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

`make dev-tunnel` asks on a terminal which development connection to use:

1. **Cloudflare tunnel** — a temporary public URL and QR, or a saved gateway;
   no Tailscale is needed. State stays under `relay/.dev/`, separate from the
   installed relay. Enter still selects this option.
2. **Tailscale Serve** — the menu offers one Tailscale choice and selects the
   usable mode from local filesystem/platform checks:
   - On macOS/arm64, the CLI-backed mode is selected only when an App Store
     receipt and readable `Info.plist` version `1.102.4` are found and filesystem-
     only executable resolution yields one unambiguous CLI. Its HTTPS route stays
     published after the foreground relay stops. Only this supplied profile is
     development-enabled; runtime and phone qualification remain pending.
   - If no App Store app is detected, the menu selects the legacy mode for an
     authenticated standalone `tailscaled` daemon. Its temporary session-owned
     route ends when the relay stops.
   - If an App Store app is detected but its profile or CLI candidate is
     unavailable, the single Tailscale option is marked unavailable. It never
     falls back to the incompatible standalone-daemon mode.

   The menu description identifies the selected mode and whether its route
   persists. Availability checks read files and platform metadata only; they
   never run Tailscale. The exact App Store Install CLI forwarding wrapper is
   treated as an alias of the bundle executable; other distinct candidates,
   including non-exact wrappers, remain ambiguous. Duplicate symlinks are
   deduplicated. Absolute `HERDR_DEV_TAILSCALE_CLI_BIN` or
   `HERDR_TAILSCALE_CLI_BIN` overrides take precedence and invalid overrides
   fail closed. CLI-backed mode uses separate `.dev-tailscale-cli/` state and
   requires its existing opt-in and exact route-consent gates; neither mode
   installs a service.

Automation behavior is unchanged: `make dev-tunnel` defaults to the tunnel,
`HERDR_DEV_TRANSPORT=tailscale-cli` selects the CLI-backed mode, and
`HERDR_DEV_TRANSPORT=tailscale` selects legacy direct-LocalAPI mode. Both explicit
Tailscale transports retain their noninteractive consent requirements.
“Tunneling” here means the Cloudflare/gateway path; Tailscale Serve is tailnet
HTTPS. Development roots remain separate.

For an **explicitly selected, already running and authenticated standalone**
supported Tailscale v1.102.4 Unix daemon, `make dev-tailscale` starts legacy
managed development without asking for paths or ports. The menu also selects
this mode as option 2 when no App Store app is detected. It does not support the
macOS App Store Tailscale app. The checkout-local state root
`relay/.dev-tailscale/` is created with mode 0700 only after input checks.
The development relay, plugin and HTTPS Serve ports default to 18377, 18378
and 8443; conflicts are refused rather than adopting a listener or changing
an enrolled port. CLI-backed development uses the same separate port contract, with state under
`relay/.dev-tailscale-cli/` and an independently consented persistent route. Its
pairing-control socket is placed outside the deep checkout in a short private
runtime directory (`/private/tmp/herdr-cli-<uid>/<sha256-of-checkout>/p.sock` on
macOS, `/tmp/...` on Linux); Go validates directory ownership/modes and the
platform AF_UNIX path-length limit before using the socket. The route journal,
relay config, credentials, and release remain in the private checkout state.
On setup/update, a marked root migrates the exact previous in-root socket value
only when that old path is absent (neither a socket nor a symlink). The migration
atomically rewrites only `HERDR_RELAY_PAIRING_SOCKET` and runs once; an existing
old-path object, unmarked root, or any other mismatch is refused unchanged. Go
then validates the new short socket path against the persisted relay environment.
The supported candidate is only the exact App Store macOS/arm64 1.102.4
profile; profile-specific development eligibility is separate from runtime
qualification. The launcher uses filesystem-only executable selection, then
calls `dev-tailscale-cli preflight`; the standalone `tailscale-cli preflight`
path is refused. Before any real CLI read or mutation, the Go entrypoint checks
explicit opt-in, private root ownership/modes, exact registration and
coordination bindings, isolated XDG/release/web/runtime paths, the relay
configuration, installed-service environment and production-root separation,
and the fixed HTTPS/backend/plugin tuple. Operation-level checks bind the
selected executable and local Herdr socket. This is not proof of launcher
provenance or a privilege boundary against a deliberately fabricating
same-user process. The persistent route survives stop and Ctrl-C;
scoped unpublish has its own exact typed confirmation. After opt-in, executable `tailscale` and `herdr` binaries
are located on `PATH` without running them. The Herdr socket is selected from
`HERDR_SOCKET_PATH` or the same `${XDG_CONFIG_HOME:-$HOME/.config}/herdr/herdr.sock`
default used by the relay and event hook, before development HOME/XDG isolation
is applied. The script checks that it is a Unix socket but does not connect to
it during selection. The binaries, socket, root and ports can all be overridden
explicitly when using a different profile; a custom root must already exist
with mode 0700. An installed relay service can keep running only when the
managed launcher independently verifies the dev marker, separate private
binary/web/state paths and nonproduction ports. The ordinary managed launcher
still refuses coexistence; a pre-existing Serve route is never adopted or
cleared. Direct interactive `make dev-tailscale` first asks for
development opt-in; noninteractive runs require `HERDR_DEV_TAILSCALE_ENABLE=1`.
The script builds a matching frontend and relay into private state and invokes
the real foreground managed launcher. It shows the exact HTTPS Serve route and
requires separate per-run consent before configuring it. It never installs, logs in, or starts Tailscale.
Locating the CLI does not discover, authenticate, or select a daemon for you;
the managed launcher checks the selected daemon's compatibility later. The
macOS GUI (MacSys) and App Store variants remain unsupported by this adapter.
Declining opt-in or running without a terminal and without the explicit opt-in
fails before contacting any daemon. For a noninteractive disposable CI run
with supported binaries and an existing Herdr socket, set
`HERDR_DEV_TAILSCALE_ENABLE=1` and explicitly consent to Serve with
`DEV_TAILSCALE_ARGS=--confirm-serve`. For custom state or reproducible binary
paths, explicitly override the defaults:

```bash
mkdir -m 700 /path/to/private/dev-state
HERDR_DEV_TAILSCALE_ENABLE=1 \
HERDR_DEV_TAILSCALE_DIR=/path/to/private/dev-state \
HERDR_DEV_TAILSCALE_BIN=/path/to/supported/tailscale \
HERDR_DEV_HERDR_BIN=/path/to/herdr \
HERDR_DEV_HERDR_SOCKET=/path/to/herdr.sock \
HERDR_DEV_TAILSCALE_PORT=18377 HERDR_DEV_TAILSCALE_PLUGIN_PORT=18378 \
HERDR_DEV_TAILSCALE_HTTPS_PORT=8443 make dev-tailscale
```

This is **not** a command to run on an unqualified personal daemon. Use it
only after deliberately preparing a supported profile and trusted HTTPS
frontend route. The installed relay service is not stopped or modified.
The managed launcher still asks for fresh Serve consent;
`DEV_TAILSCALE_ARGS=--confirm-serve` is an explicit per-invocation alternative
for unattended disposable CI. A missing private root/marker, unsupported
platform/profile, occupied port, existing foreground owner or uncertain cleanup
is refused. A pre-existing marked dev root retains its token, device store and
port choice; no rearm or automatic recovery is performed. Build replacements
preserve the previous generated version for inspection. The phone setup URL
requires a trusted certificate, live owner and exact matching web bundle.
MacSys is unsupported. The App Store profile is development-enabled only; no
real-runtime or physical-phone qualification is implied by this entrypoint.
A bootstrap setup link remains one-use, but its ten-minute window refreshes
when it is presented while no phone has enrolled yet. The window is not a hard
deadline measured from when the QR/link was printed. If the first phone reports
an expired or refused bootstrap link, retry the same link while that same
foreground relay is running; no manual arm command is needed. After a phone has
enrolled, the bootstrap invitation is consumed. Invitations created for
additional devices are separate one-use invitations with a fixed ten-minute
lifetime; a paired owner must create a fresh invitation if one expires or is
used. Re-arming does not extend ordinary device invitations, reset devices, or
change the Tailscale route. If the original relay is stopped or the refusal
remains unclear, retain state and follow the qualification runbook rather than
guessing or clearing credentials.

For CLI Serve state, `status` and `recover` are read-only. A pending or uncertain
publish/removal may be reconciled only by `reconcile`, after the read-only report
shows one unambiguous exact-present or listener-absent result and the operator
types the displayed operation-bound confirmation; reconciliation records that
observation and never replays a Serve command. If an acknowledged registration's
route has disappeared, `repair-missing` starts the isolated foreground relay,
rechecks the complete route absence and journal identity, and requires an exact
confirmation bound to the prior operation and route before one fresh publish and
readback. Do not use repair for a pending/uncertain mutation; reconcile it first.
A lost unpublish acknowledgement is reconciled the same way, including after a
reconciled-present publication: unpublish keeps the shared reservation's
pre-removal state, so the fresh present or absent observation of the new
removal operation can be recorded without replaying `off`.
`abandon-missing` is the stopped-service alternative when the route should not
be recreated: it requires a free backend listener and complete absence checks,
then records `reconciled-absent` for a registered or reconciled-present journal
and releases only the matching local reservation without changing Serve. It also
releases an exact reservation left beside a reconciled-absent or removed journal
after an interrupted final write, without rewriting that journal.
Conflicting/incomplete state, an occupied listener, or a mismatched operation
ID stops these commands without clearing or adopting route state. Invoke these actions through
`HERDR_DEV_TAILSCALE_CLI_ENABLE=1 relay/dev-tailscale-cli.sh <action>` only in
the isolated development profile; they do not qualify production or permit
live-system recovery outside owner authorization.

## Common targets

```bash
make check             # all backend, frontend, browser, and release checks
make backend-check     # format, vet, tests, race detector, shell checks
make dev-tailscale     # guided legacy foreground Serve; explicit private root/daemon/ports
make dev-tailscale-cli # isolated App Store 1.102.4 development; runtime qualification pending
make web-release       # replace committed web/ with a verified frontend build
make web-release-check # compare and browser-test the shipped web/ bundle
make relay-plugin      # link this checkout as a Herdr plugin
make stable-setup      # run the stable tunnel wizard with the installed relay
```

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

Backend development uses Go 1.27.1; frontend development uses Bun 1.4 (`bun
install --cwd frontend`, then the `make` targets above). Playwright runs on
Bun. CI installs both browsers natively (`bun x playwright install
--with-deps chromium webkit`); on Fedora, `install-deps` is unsupported and
native WebKit crashes, so `make frontend-browser` runs WebKit through
Playwright's official container via podman (Chromium runs natively — its dnf
dependencies are nspr nss dbus-libs atk at-spi2-atk cups-libs at-spi2-core
libXcomposite libXdamage libXext libXfixes libXrandr mesa-libgbm cairo pango
alsa-lib, per passportxyz/passport's fedora-install-playwright-deps.sh).
Publishing the hosted web app (`make web-deploy`,
`make web-preview`) shells out to `npx wrangler`, which requires Node.js 22 or
newer on that computer only; CI and the relay's deploy action are exercised on
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

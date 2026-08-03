# Herdr Mobile Relay

[![check](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml/badge.svg)](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml)

Control [Herdr](https://herdr.dev) agents from your phone. Each Linux or macOS
computer runs its own relay; the phone connects directly and merges all agents
into one installable web app.

**Current version:** [`0.13.8`](https://github.com/0cv/herdr-mobile-relay/releases/tag/v0.13.8) · [Changelog](CHANGELOG.md)

> [!IMPORTANT]
> Native Windows is not supported. WSL2 may work but is not tested.

## Install

Requirements: Herdr 0.7.5 or newer, Git, and `curl`.

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

Choose **Quick Start** from the setup menu. If the menu does not open:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Quick Start installs missing user-level tools with confirmation, starts the
relay and bundled app, and opens a temporary TryCloudflare tunnel. Scan its QR
code on your phone. Keep the pane open; Ctrl-C stops the relay and tunnel.

No Cloudflare account, domain, Python, Node.js, Go toolchain, separate web
deployment, or `sudo` is required for this trial path. See
[QUICKSTART.md](QUICKSTART.md) for the short walkthrough.

## Mobile Onboarding

https://github.com/user-attachments/assets/e52c4fd0-ef77-4852-bb43-078a7154eae8

The walkthrough follows setup from scanning the relay QR through the agent list,
terminal controls, and notification settings. The QR imports the relay URL,
label, and relay key, so treat the QR and setup link as secrets. Enable
notifications in the app's Settings; blocked-agent notifications are included,
while completion notifications are optional.

## Stable Setup

For a permanent hostname and background service, add a domain to Cloudflare and
run:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard creates or resumes a dedicated tunnel, checks the DNS route, installs
a user service, verifies the public relay identity, and then prints the private
phone QR. Run it once per computer with a distinct hostname, then add every QR
to the same phone app.

Useful actions:

```bash
herdr plugin action invoke setup-link --plugin herdr-mobile-relay.events
herdr plugin action invoke status --plugin herdr-mobile-relay.events
herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
herdr plugin action invoke uninstall --plugin herdr-mobile-relay.events
```

Run `stable-teardown` before uninstall if Cloudflare resources should also be
removed. Full uninstall removes the service, releases, relay state, push
credentials, cache, and plugin registration.

## What It Does

- Monitor and control agents across several computers.
- Start, rename, clear, restart, and stop agents from relay-provided launch
  profiles.
- Send prompts, terminal keys, slash commands, screenshots, and photos.
- Answer verified Codex, Claude Code, and Qoder approvals, plus structured
  questions from those agents and OpenCode.
- Search local activity and receive blocked or completion notifications.
- Require device verification before reconnecting relays.
- Detect Codex, Claude Code, OpenCode, Qoder CLI, Pi, Oh My Pi, and Kimi.

| Agents | Native Resize |
| --- | --- |
| <img src="images/home.jpeg" alt="Mobile list of Herdr agents" width="392"> | <img src="images/native_mobile_resolution.jpeg" alt="OMP terminal rendered at native mobile width" width="392"> |

| Plan Questions | Notifications |
| --- | --- |
| <img src="images/agent_plan.jpeg" alt="Structured plan question navigation" width="392"> | <img src="images/notifications.jpg" alt="Blocked-agent notification" width="392"> |


## Mobile Terminal

The terminal-width control in the header and Settings offers three modes:

- **Fit to Phone** reflows terminal output into the phone viewport.
- **Original Columns** preserves the captured terminal grid and allows
  horizontal scrolling.
- **Resize Session** temporarily leases the live PTY at the measured mobile
  width, so full-screen agents redraw for the phone. The relay restores the
  previous width on mode exit, disconnect, lease expiry, or shutdown.

Terminal History requests 100, 1,000, 5,000, or 10,000 lines per pane; 1,000 is
the default. The relay enforces the selected limit after any Claude or Qoder
history merge. Larger histories increase network transfer and rendering work.

Terminal Refresh controls how often the relay checks a visible pane: 100 ms,
250 ms, 500 ms, or 1 second. The 250 ms default balances responsiveness with
computer and phone CPU use while output is changing.

Returning to an unchanged Resize Session paints its cached rendered frame
immediately, then reacquires the lease and reconciles current content in the
background. Long tokens wrap in responsive output, while fixed-grid table rows
remain aligned.

The terminal controls send **Esc**, **Tab**, **Shift+Tab**, arrow keys, and
`Ctrl` plus the next keyboard letter. **Copy** copies the latest completed
agent response without ANSI control sequences when available; otherwise it
copies the visible terminal output.

## Updates

The plugin installs a pre-built, checksum- and manifest-verified bundle for the
exact version in `herdr-plugin.toml`. Users never compile the relay. Updates
atomically activate the executable, web app, and runtime wrappers, verify their
exact version, revision, and web hash after restart, and roll back the complete
release if verification fails.

Phone-driven upgrades run `herdr plugin install` in a transient worker pinned
to the release commit. The same plugin build hook can restore stale service
paths from the persistent plugin config, including when no usable local release
remains.

The relay-hosted app updates with its relay. For a separately hosted Cloudflare
Pages app, configure exactly one stable relay as deployment owner with the
`configure-app-deploy` action. From relays running this release onward, the
worker downloads and verifies the target release without activating it, checks
that the current and target apps and relays share a transport, deploys the
target web bundle, and verifies the public Pages version. Only then does it
install and restart the relay. A failed download, compatibility check,
deployment, or public-origin check leaves the current relay running.
Transport-breaking changes require a bridge release that supports both
transports. This release retains the existing E2EE v1 transport, so the upgrade
into it remains compatible with the previous phone app. The optional
deployment-owner role requires Node.js 24 and Cloudflare credentials on that
computer only.

## Local Development

```bash
git clone https://github.com/0cv/herdr-mobile-relay.git
cd herdr-mobile-relay
make dev-tunnel
```

`make dev-tunnel` builds the current Go source and frontend, uses isolated ports
and state under `relay/.dev/`, and opens a temporary tunnel. It never uses the
installed production relay.

Common targets:

```bash
make check             # all backend, frontend, browser, and release checks
make backend-check     # format, vet, tests, race detector, shell checks
make web-release       # replace committed web/ with a verified frontend build
make web-release-check # compare and browser-test the shipped web/ bundle
make relay-plugin      # link this checkout as a Herdr plugin
make stable-setup      # install a checkout-managed stable relay
```

Backend development uses Go 1.26.5; frontend development uses Node.js 24.
Packaged users need neither toolchain.

The test-only `cmd/fake-herdr` binary provides deterministic Herdr CLI behavior,
failure injection, and process-control traces for black-box tests. It is not
included in release archives.

## Runtime and Security

The relay binds to `127.0.0.1:8375`; its event hook uses loopback UDP port 8376.
Cloudflare Tunnel supplies HTTPS/WSS without opening an inbound port. Browser
origins are checked, tokens use constant-time comparison, uploads are limited,
and launch requests cannot provide arbitrary executables or shell commands.

Runtime data stays in the relay's private config and cache roots. The phone
stores its relay list locally. There is no central broker and relays do not
connect to one another.

When a relay key is configured, the phone and relay authenticate an ephemeral
P-256 ECDH handshake with that key, derive per-connection keys with
HKDF-SHA-256, and encrypt every subsequent WebSocket message with AES-256-GCM.
The phone sends encrypted key confirmation before the relay registers the
connection. The relay key stays in the QR/setup URL fragment and phone storage;
it is never placed in the WebSocket URL or an HTTP header. Cloudflare can still
observe connection metadata such as endpoints, timing, and encrypted frame
sizes, but not relay commands, terminal output, uploads, or push-subscription
details. Tokenless loopback development connections do not add
application-layer encryption.

As with any browser E2EE app, this assumes the phone is running trusted app
code. A provider that actively replaces the JavaScript before it reaches the
phone could capture the relay key; use an already installed app or an
independently controlled app origin when that threat is in scope.

Relay keys shorter than 16 bytes are rejected, but length alone is not entropy.
The visible handshake proof permits offline guesses, so use the random,
relay-unique key generated by setup rather than a human-chosen value.

Health endpoints:

- `GET /health` — process liveness.
- `GET /healthz` — version, revision, web bundle, instance, and inventory state.
- `GET /readyz` — HTTP 200 only after a successful Herdr inventory.

## Troubleshooting

- **No setup menu:** invoke the `setup` action shown above.
- **Port 8375 is busy:** stop the earlier Quick Start or installed service.
- **Temporary URL fails:** keep the pane open and rerun Quick Start for a new
  hostname if `cloudflared` stopped.
- **App opens but stays disconnected:** reopen the complete setup link,
  including its `#setup=...` fragment.
- **Agents are unavailable:** inspect `/healthz`; after a Herdr protocol update,
  run `herdr server live-handoff` and wait for the next relay poll.
- **Stable setup stops:** keep its state and rerun the exact command printed.
- **Need the stable QR:** invoke the `setup-link` action.

## License

Herdr Mobile Relay is licensed under the
[GNU Affero General Public License v3.0 or later](LICENSE).

# Herdr Mobile Relay

Approve [Herdr](https://herdr.dev) agents from your phone across multiple computers.

Herdr Mobile Relay runs a small local relay on each computer, exposes each relay through its own Cloudflare Tunnel hostname, and lets one static web app connect to all of them. The phone UI merges agents from every configured relay, so you can approve or inspect agents running on a Mac, a Fedora workstation, or any other supported machine without making those computers connect to each other.

## Screenshots

| Agents                                                                                                             | Terminal                                                                                                                 | Settings                                                                                                        |
| ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| <img src="images/home.jpeg" alt="Mobile home page showing Mac and Fedora agents merged into one list" width="240"> | <img src="images/terminal.jpeg" alt="Mobile terminal view for a Fedora agent with multiline input controls" width="240"> | <img src="images/settings.jpeg" alt="Mobile settings page with Mac and Fedora relay configuration" width="240"> |

## Attribution

This project is forked from and inspired by [dcolinmorgan/herdr-remote](https://github.com/dcolinmorgan/herdr-remote). Herdr Mobile Relay keeps the original idea of approving Herdr agents remotely, but has been substantially reworked around a static phone web app, per-computer local relays, Cloudflare Tunnel hostnames, and no SSH or Telegram fan-out.

## Marketplace Listing

**Name:** Herdr Mobile Relay

**Repository slug:** `herdr-mobile-relay`

**Plugin ID:** `herdr-mobile-relay.events`

**Short description:** Approve Herdr agents from your phone across multiple computers.

**Long description:** Run one local relay per computer, expose each relay through Cloudflare Tunnel, and manage all agents from one static mobile web app. Herdr Mobile Relay keeps machines independent: there is no SSH fan-out, central broker, Telegram bot, or native mobile app to install.

**Tags:** mobile, relay, cloudflare, multi-machine, approvals

## What It Does

- Runs one local relay per computer.
- Polls only the local `herdr` CLI on that computer.
- Exposes the relay through a `wss://` URL, usually via Cloudflare Tunnel.
- Lets the static web app connect to multiple relays and merge their agent lists client-side.
- Uses relay labels from the web app, such as `Mac` or `Fedora`, as the visible host badges.
- Supports an optional local Herdr plugin hook for faster blocked-agent updates.

## What It Does Not Do

- It does not connect your computers to each other.
- It does not use SSH remotes or SSH fan-out.
- It does not run a central hosted broker.
- It does not require Telegram, a native iOS app, or a native macOS menu bar app.

## Components

- **Relay:** Python WebSocket/HTTP service on port `8375`.
- **Web app:** static mobile UI in `web/`; stores relay configs in browser local storage.
- **Cloudflare Tunnel:** optional but recommended public access layer for each relay.
- **Background services:** launchd on macOS and user systemd on Linux/Fedora start the relay and `cloudflared`.
- **Herdr plugin hook:** optional local event push from `herdr` into the local relay over UDP.

## Quick Start

Start a temporary relay and Cloudflare quick tunnel:

```bash
./relay/start.sh
```

The script prints:

```text
Tunnel URL: https://example.trycloudflare.com
WebSocket:  wss://example.trycloudflare.com
Token:      0123456789abcdef0123456789abcdef
```

Open your deployed web app on your phone and add both the `wss://...trycloudflare.com` URL and token in Settings. Quick tunnels are temporary; the hostname changes when the tunnel restarts. The token is generated in `relay/.env` and reused on later quick-start runs.

## Web App

The web app is static and lives in `web/`.

Deploy it anywhere that can host static files over HTTPS. With Cloudflare Pages direct upload:

```bash
cp .env.example .env
# edit WEB_PROJECT in .env
make web-deploy
```

### Install on Your Phone

Install the deployed web app URL, not a `wss://` relay URL. The installed app keeps your relay settings in browser local storage.

On iPhone or iPad:

1. Open the deployed Herdr Mobile Relay web app in Safari.
2. Tap Share.
3. Tap Add to Home Screen.
4. Tap Add.

On Android:

1. Open the deployed Herdr Mobile Relay web app in Chrome.
2. Tap the install prompt, or open the three-dot menu.
3. Tap Install app or Add to Home screen.
4. Confirm the install.

The app includes a web manifest and Apple touch icon, so it installs with the Herdr Relay icon.

In the app Settings, add one relay entry per computer:

- **Relay Name:** display label such as `Mac` or `Fedora`
- **Relay URL:** `wss://...`
- **Token:** value of `HERDR_RELAY_TOKEN` if relay auth is enabled

## Stable Hostnames

For day-to-day use, create one named Cloudflare Tunnel and one DNS hostname per computer:

```bash
# On the Mac
cloudflared tunnel login
cloudflared tunnel create herdr-mobile-relay-mac
cloudflared tunnel route dns herdr-mobile-relay-mac relay-mac.yourdomain.com

# On Fedora
cloudflared tunnel create herdr-mobile-relay-fedora
cloudflared tunnel route dns herdr-mobile-relay-fedora relay-fedora.yourdomain.com
```

Create `~/.cloudflared/config-herdr-mobile-relay.yml` on each computer. Example for the Mac:

```yaml
tunnel: herdr-mobile-relay-mac
credentials-file: /Users/you/.cloudflared/<TUNNEL_ID>.json

ingress:
  - hostname: relay-mac.yourdomain.com
    service: http://localhost:8375
  - service: http_status:404
```

For Fedora:

```yaml
tunnel: herdr-mobile-relay-fedora
credentials-file: /home/you/.cloudflared/<TUNNEL_ID>.json

ingress:
  - hostname: relay-fedora.yourdomain.com
    service: http://localhost:8375
  - service: http_status:404
```

Use both relay URLs in the web app:

```text
wss://relay-mac.yourdomain.com
wss://relay-fedora.yourdomain.com
```

Do not run multiple computers as replicas of the same Cloudflare Tunnel if they serve different local relays. Cloudflare may send a WebSocket to any connector for that tunnel.

## Background Services

The background service starts both the relay and `cloudflared`.

On macOS:

```bash
make macos-service-install
make macos-service-status
make macos-service-logs
make macos-service-uninstall
```

On Fedora/Linux:

```bash
make linux-service-install
make linux-service-status
make linux-service-logs
make linux-service-uninstall
```

The service uses `relay/.env` for:

```env
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8375
HERDR_RELAY_PLUGIN_PORT=8376
HERDR_RELAY_POLL_INTERVAL=2
HERDR_ALLOWED_ORIGINS=
HERDR_RELAY_TOKEN=<shared-secret>
CLOUDFLARED_CONFIG=$HOME/.cloudflared/config-herdr-mobile-relay.yml
```

Read the token for the web app:

```bash
sed -n 's/^HERDR_RELAY_TOKEN=//p' relay/.env
```

New installs use `com.herdr-mobile-relay.service` on macOS and `herdr-mobile-relay.service` on Linux/Fedora. The installers remove the earlier `herdr-remote` service labels when they run. Existing ignored `relay/.env` files may still point at `config-herdr-remote.yml`; that is fine as long as the file exists, or you can update `CLOUDFLARED_CONFIG` to the new `config-herdr-mobile-relay.yml` path.

## Token Auth

Enable relay auth with:

```bash
export HERDR_RELAY_TOKEN="$(openssl rand -hex 16)"
make relay-run
```

Use the same `HERDR_RELAY_TOKEN` on multiple relays if you want one shared phone-side secret.

## Herdr Plugin Hook

The relay polls local `herdr` every few seconds. For faster blocked-agent updates, link the local plugin hook on each computer:

```bash
make relay-plugin
```

The plugin sends local agent-status events to the local relay over UDP on `127.0.0.1:8376`. It does not expose another network service and does not connect to other computers. If you change `HERDR_RELAY_PLUGIN_PORT`, the plugin hook reads the same `relay/.env` file so the relay and hook stay aligned.

## Security Model

- Relay access is protected with `HERDR_RELAY_TOKEN` for quick-start and service installs.
- Cloudflare Tunnel provides the public TLS endpoint; the relay itself listens locally on `127.0.0.1:8375` by default.
- Set `HERDR_RELAY_HOST` only if you intentionally need a non-loopback bind address. The relay refuses a non-loopback bind without `HERDR_RELAY_TOKEN`.
- Tokenless browser connections are rejected unless their `Origin` is listed in `HERDR_ALLOWED_ORIGINS` as a comma-separated list, such as `https://your-pages-site.pages.dev`.
- The web app stores relay URLs and tokens in browser local storage on the device where you configure it.
- A connected web client can send text and key input to Herdr panes exposed by that relay. Treat relay URLs and tokens as sensitive.
- The relay executes only local `herdr pane ...` commands; it does not shell into other machines.

## Requirements

- Python 3.10+
- [uv](https://docs.astral.sh/uv/)
- `cloudflared` for remote access
- Herdr 0.7+

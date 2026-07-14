# Herdr Mobile Relay

[![check](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml/badge.svg)](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml)

Approve [Herdr](https://herdr.dev) agents from your phone across multiple computers.

Herdr Mobile Relay runs a small local relay on each computer, exposes each relay through its own Cloudflare Tunnel hostname, and lets one static web app connect to all of them. The phone UI merges agents from every configured relay, so you can approve or inspect agents running on a Mac, a Fedora workstation, or any other supported machine without making those computers connect to each other.

> [!IMPORTANT]
> Herdr Mobile Relay currently supports Linux and macOS. Native Windows is not supported. It may work inside WSL2 because that provides a Linux environment, but WSL2 has not been tested and is not currently an officially supported setup.

## [Quick Start: One Command](QUICKSTART.md)

With Herdr already installed, install the relay as a Herdr plugin:

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

The install previews the plugin's commands and asks for confirmation. After Herdr registers the plugin, an interactive setup menu opens automatically in a managed Herdr pane, an Apple Terminal window/tab, Konsole, or GNOME Terminal—only when that destination can be identified reliably.

Choose **Quick Start** for a temporary tunnel with no account or domain, or **Stable Tunnel** for the advanced background-service path. Quick Start creates private persistent plugin configuration, offers to install `cloudflared` (and retries `uv` if preparation was unavailable), starts the relay, serves the phone app, and opens a free temporary Cloudflare tunnel. **A Cloudflare account, domain, Make, Node.js, Python installation, and separate web deployment are not required for this first trial.**

If no safe terminal destination is detected, installation still succeeds and prints the fallback command:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

When it is ready, the terminal prints a **QR code** and the matching private **Phone setup** link. Scan the QR code with your phone camera, or open the exact link on your phone; either way the app loads and adds the relay automatically. Keep the terminal open while using the quick tunnel, and press Ctrl-C when finished.

> [!CAUTION]
> The setup link — and therefore the QR code — contains the relay token in its URL fragment. The fragment is not sent to the web server and the app removes it after importing, but you should still avoid sharing the original link or a screenshot of the QR code.

See the **[beginner-friendly QUICKSTART](QUICKSTART.md)** for screenshots-level steps, the local-checkout fallback, and troubleshooting. Once the trial works, move to a [stable hostname and background service](#stable-hostnames) for everyday use.

## Screenshots

| Agents                                                                                                             | Terminal                                                                                                                 |
| ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| <img src="images/home.jpeg" alt="Mobile home page showing Mac and Fedora agents merged into one list" width="392"> | <img src="images/terminal.jpeg" alt="Mobile terminal view for a Fedora agent with multiline input controls" width="392"> |

| Start Agent                                                                                                                                                      | Activity                                                                                                                    |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| <img src="images/new%20agent.jpeg" alt="Mobile Start Agent form with computer, agent, working directory, generated name, and optional initial task" width="392"> | <img src="images/activities.jpeg" alt="Searchable mobile activity history merged across Mac and Fedora relays" width="392"> |

| Settings                                                                                                        | Notifications                                                                                                 |
| --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| <img src="images/settings.jpeg" alt="Mobile settings page with Mac and Fedora relay configuration" width="392"> | <img src="images/notifications.jpg" alt="Mobile notification for a blocked Herdr agent approval" width="392"> |

## What This Fork Adds

Forked from [dcolinmorgan/herdr-remote](https://github.com/dcolinmorgan/herdr-remote), which established the core idea: control Herdr approval prompts from another device. This fork turns that idea into an installable mobile web app for multiple machines, with no central broker and no machine-to-machine trust.

- **Multi-computer phone UI:** one static web app connects to multiple independent relays and merges Mac, Fedora, or other hosts into one agent list.
- **Per-machine isolation:** each computer runs only its own local relay and `herdr` CLI calls; relays do not SSH into each other or share state.
- **PWA notifications:** installed phones receive Web Push notifications when an agent blocks, including a safe Approve once action; denial remains available after opening the app, and optional completion alerts can open the finished agent directly.
- **Mobile terminal composer:** terminal view has a compact phone-first composer, quick terminal keys, a phone-local Copy response button, inline approval actions, themes, font sizing, and a jump-to-bottom affordance.
- **Real approval handling:** blocked cards parse prompt text and approval options, then map visible choices back to the correct Herdr key actions for Codex and Claude Code.
- **Confirmed controls and activity:** command acknowledgements report failures explicitly, approvals wait for an observed state change, and a bounded per-relay activity history is merged on the phone.
- **Activity-aware home ordering:** agents most recently changed on their computer appear first within each status section, including activity initiated outside the phone app.
- **Remote agent management:** start automatically detected installed agents, then rename, clear, or stop them from the phone.
- **Screenshot/photo upload:** attach an image from the phone, store it on the target computer, and insert the local path into the agent prompt.
- **Optional device unlock:** require the phone's platform authenticator before reconnecting relays after open, reload, or resume.
- **Service installers:** macOS launchd and Linux/Fedora user systemd installers set up the relay, tunnel, token, and cleanup of older service names.
- **Plugin-first trial:** one Herdr plugin install opens a conservative interactive setup menu, installs missing user-level prerequisites with confirmation, and provides an auto-configuring TryCloudflare link.
- **Security hardening:** token generation, loopback binding by default, Origin checks for browser clients, constant-time token comparison, and safer behavior for public tunnels.

## Marketplace Listing

**Name:** Herdr Mobile Relay

**Repository slug:** `herdr-mobile-relay`

**Plugin ID:** `herdr-mobile-relay.events`

**Current version:** `0.5.1`

**Short description:** Approve and monitor Herdr agents remotely from your phone — a mobile web app for Android/iOS smartphones with push notifications, QR setup, and multi-computer relays.

**Long description:** Remote-control Herdr from any smartphone. Run one local relay per computer, expose each relay through Cloudflare Tunnel, and approve, monitor, and manage all agents from one installable mobile web app (PWA) on Android or iOS. Herdr Mobile Relay keeps machines independent: there is no SSH fan-out, central broker, Telegram bot, or native mobile app to install. It adds multi-relay agent merging, actionable Web Push notifications, confirmed terminal and approval controls, remote agent lifecycle management, merged activity history, screenshot/photo upload, optional device unlock, and service installers for macOS and Linux.

**Tags:** mobile, smartphone, phone, remote, approvals, monitoring, notifications, push, android, ios, pwa, qr, cloudflare, relay, multi-machine

## What It Does

- Runs one local relay per computer.
- Polls only the local `herdr` CLI on that computer.
- Exposes the relay through a `wss://` URL, usually via Cloudflare Tunnel.
- Lets the static web app connect to multiple relays and merge their agent lists client-side.
- Uses relay labels from the web app, such as `Mac` or `Fedora`, as the visible host badges.
- Shows blocked prompts with inline approval buttons on the agent list and in the terminal view.
- Sends Web Push notifications for blocked agents to installed phones, even when the app is closed or suspended, with optional notifications when agents finish.
- Routes notification taps and the Approve once action back to the matching relay and pane when the app can resolve it.
- Confirms remote commands and records blocked, resolved, approval, prompt, upload, and lifecycle activity locally on each relay.
- Starts automatically detected installed agents and supports rename, clear, and stop controls.
- Uploads screenshots and photos from the phone to the connected relay's local filesystem.
- Provides a compact mobile terminal UI with send, attach, terminal keys, themes, and font-size controls.
- Can require fingerprint, face unlock, or passcode verification before reconnecting relays.
- Supports an optional local Herdr plugin hook for faster blocked-agent updates.

## What It Does Not Do

- It does not connect your computers to each other.
- It does not use SSH remotes or SSH fan-out.
- It does not run a central hosted broker.
- It does not require Telegram, a native iOS app, or a native macOS menu bar app.
- It does not currently run natively on Windows. WSL2 may work but remains untested and unsupported.

## Components

- **Relay:** Python WebSocket service that also serves the phone app and an HTTP health response on port `8375`.
- **Web app:** static mobile UI in `web/`; stores relay configs in browser local storage.
- **Cloudflare Tunnel:** optional but recommended public access layer for each relay.
- **Background services:** launchd on macOS and user systemd on Linux/Fedora start the relay and `cloudflared`.
- **Herdr plugin hook:** optional local event push from `herdr` into the local relay over UDP.

## Requirements

The plugin-first quick start needs only:

- Linux or macOS; native Windows is not supported, and WSL2 remains untested
- Herdr 0.7.0 or newer, Git, and `curl`

Herdr uses Git to install the plugin. With your confirmation, the plugin build installs missing [uv](https://docs.astral.sh/uv/getting-started/installation/) and prepares the relay environment; the setup menu opens after registration, and Quick Start retries `uv` if needed and offers to install [`cloudflared`](https://developers.cloudflare.com/tunnel/downloads/) interactively. Both tools are installed for your user account, and `uv` supplies Python when necessary. Set `HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1` when an automated install must not open a pane or terminal.

Optional later requirements:

- A Cloudflare account and domain for a permanent named tunnel
- Node.js/npm only for deploying a separate Cloudflare Pages copy or running the frontend checks

## Web App

The web app is static and lives in `web/`. The one-command quick start serves it directly from the relay, so beginners do not need to deploy it separately.

For an independent, always-available app origin—especially useful with multiple computers—you can still deploy `web/` anywhere that hosts static files over HTTPS. With Cloudflare Pages direct upload:

```bash
# edit WEB_PROJECT in .env first if needed
make web-deploy
```

### Install on Your Phone

For the first trial, simply scan the printed QR code or open the printed Phone setup link. Quick-tunnel hostnames change, so wait until you have a stable hostname or independently hosted app before treating the installation as permanent. Install the HTTPS app URL, not a `wss://` relay URL; the installed app keeps relay settings in browser local storage.

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

The Phone setup link adds the first relay automatically. To add another relay manually, use Settings:

- **Relay Name:** display label such as `Mac` or `Fedora`
- **Relay URL:** `wss://...`
- **Token:** value of `HERDR_RELAY_TOKEN`

Then tap **Enable Notifications** in Settings. The relay generates Web Push VAPID keys in `relay/push/` and stores this device's push subscription there, so installed PWAs can be notified even when the app is closed or suspended. Blocked-agent alerts are always included while push is active; enable **Notify When Agents Finish** if you also want an alert that opens a newly completed agent. Completion alerts are off by default and saved separately for each phone browser.

For multiple relays, the app creates one scoped service-worker push subscription per relay. Each relay keeps its standard VAPID keypair and subscriptions privately under `relay/push/`; no push configuration is required.

In the terminal view, tap the image icon to attach a screenshot or photo. The web app uploads images up to 10 MB to `~/.cache/herdr-mobile-relay/uploads` on that computer and inserts the local file path into the prompt.

Claude Code's alternate-screen interface exposes only its current visible rows through Herdr. The relay therefore builds a stable, bounded 500-line history per Claude pane from advancing snapshots. Known older viewports are ignored, so scrolling the laptop while Claude is idle does not rewrite the phone history. The accumulated history — up to 500 lines of terminal content per pane — is persisted in `~/.cache/herdr-mobile-relay/claude-history/` (one file per pane, `0600` permissions) so it survives relay restarts. A pane's file is deleted when the pane closes, and files for panes that no longer exist are removed after 7 days. The relay still cannot recover text discarded before it first saw the pane.

Use **＋** in the app header to start an agent on a connected computer. The relay exposes detected `codex`, `claude`, and `opencode` executables as safe launch profiles. Select a working directory reported by that relay; the suggested name updates to `<directory>-<agent>`. The optional initial task is sent as the agent's first literal prompt after Herdr creates it and is never interpreted by a shell. Each launched agent is moved into its own named tab so it cannot inherit another agent's tab label. Use **•••** from a terminal to rename, clear, or stop that agent. Clear starts a fresh replacement with the same detected profile and working directory, moves it to a dedicated tab, and then closes the old pane.

Use **◷** to view and search the merged activity history from all connected relays. Each relay keeps the latest 500 entries in `~/.cache/herdr-mobile-relay/activity.jsonl`. Prompt activity stores only a short preview, not full terminal output.

Within each home-page status section, agents are ordered by the most recent change observed by their relay. Herdr does not currently expose a historical activity timestamp, so the relay infers this from status changes, terminal-output growth, metadata changes, phone actions, and agent events while it is running. Ordering falls back to the existing host/name order after a relay restart until new activity is observed.

The relay automatically exposes installed Codex, Claude Code, and OpenCode executables in the Start Agent form. Its working-directory browser starts at the relay user's home directory and retrieves only the current folder's immediate non-hidden subdirectories as you navigate. It does not recursively scan or preload the filesystem. There is no profile configuration, and the browser cannot submit an executable or arbitrary shell command. Navigation and agent launches stay inside the current user's home directory.

## Stable Hostnames

For day-to-day use, run the stable setup wizard once on each computer. From the plugin setup menu, choose **Stable Tunnel**. From a local checkout, run:

```bash
make stable-setup
```

The wizard:

1. Resolves the same persistent relay environment used by the service and refuses to modify a different checkout or plugin configuration.
2. Reuses a valid `CLOUDFLARED_CONFIG` without changing it, or creates a dedicated `herdr-mobile-relay-<computer>` tunnel, credentials file, ingress config, and DNS route.
3. Installs the macOS launchd or Linux user-systemd service.
4. Waits up to 90 seconds for public DNS, then separately up to 60 seconds for `https://<hostname>/healthz`.
5. Prints the phone setup link and QR code only when the public response has the same relay instance ID, version, and protocol as the local service.

Cloudflare authorization happens in the foreground so its URL remains visible. On a desktop, `cloudflared` may open that URL in the browser. In a headless or remote shell, copy the exact URL it prints into a browser, authorize the intended zone, and return to the terminal. The domain entered in the wizard must belong to that selected zone.

The default hostname is editable and follows `relay-<computer>.<domain>`. The wizard never passes `--overwrite-dns`: an unrelated record is a conflict, and you must choose a different hostname. A route that was created immediately before an interruption is recovered only after starting the recorded tunnel and proving that its public health identity matches the local relay.

Immediately before creating a new tunnel, the wizard shows the exact tunnel name and DNS hostname and asks for confirmation. Unattended automation must opt in explicitly with `HERDR_STABLE_YES=1`. Existing-config reuse does not prompt, and once the tunnel exists an interrupted-setup resume does not prompt again or create a duplicate.

Progress is stored atomically in a private `stable-setup.json` beside the resolved `relay.env` or `relay/.env`. Tunnel credentials and generated configuration are also private. If setup stops or times out, keep the state file and run the exact command printed by the wizard; it resumes the recorded tunnel instead of creating a duplicate. You can edit the hostname on a resumed run until DNS routing succeeds.

Common failures are explicit:

- **The domain must belong to the zone selected during login:** rerun the wizard after authorizing the correct Cloudflare zone. The original `cloudflared` error remains visible.
- **The hostname already has a public DNS record:** choose another hostname or remove the unrelated record yourself; the wizard will not overwrite it.
- **Public DNS timed out after 90 seconds:** DNS did not become visible through Cloudflare DNS-over-HTTPS. Cloudflare's resolver may still have the earlier negative result from the hostname conflict check cached, so the first 90-second wait can be tight. This is independent of the HTTPS wait; rerun the exact command and setup resumes safely.
- **HTTPS health timed out after 60 seconds:** DNS is live, but the public tunnel did not serve the local relay in time. Identity mismatches name the differing health field, and no QR is printed.

Quick Start remains a disposable TryCloudflare path. It is not automatically promoted to the stable tunnel; enter Stable Tunnel through the setup menu or run `make stable-setup` when you are ready.

### Teardown

For wizard-created resources, run:

```bash
make stable-teardown
```

Marketplace users can run the same confirmed teardown without locating the managed checkout:

```bash
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
```

Teardown requires the Herdr ownership marker and a `herdr-mobile-relay-` tunnel name. It shows the exact service, tunnel, hostname, config, and credentials before confirmation. It stops only a service installed by the wizard, deletes only its recorded tunnel and generated files, and never changes a custom Cloudflare config.

`cloudflared` has no dependable DNS-route deletion command. After deleting the tunnel, teardown checks the hostname. If the record still resolves—or removal cannot be verified—it retains diagnostic state and prints the exact hostname plus Cloudflare dashboard DNS cleanup instructions instead of claiming success.

Herdr's `plugin uninstall` unregisters the plugin and removes its managed checkout, but deliberately keeps the separate plugin configuration directory and does not run a manifest teardown hook. If you want both the wizard-owned infrastructure and the plugin removed, invoke `stable-teardown` first and uninstall only after teardown completes:

```bash
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
herdr plugin uninstall herdr-mobile-relay.events
```

If the plugin was already uninstalled, reinstall it to recover the teardown action; the retained config and ownership state let teardown resume safely.

### Existing or Custom Cloudflare Configuration

If `CLOUDFLARED_CONFIG` already points to a readable config, `make stable-setup` leaves the file untouched. It validates ingress syntax, a readable credentials file, the tunnel identifier, the ingress hostname, and an origin matching `HERDR_RELAY_PORT`. When `cert.pem` or another origin certificate is available, it also confirms the tunnel still exists. A credentials-based config remains valid for running the tunnel when `cert.pem` is unavailable, so that management-only check is skipped rather than failing setup.

Users who intentionally manage Cloudflare themselves can keep using the low-level path. Create the named tunnel, DNS record, credentials, and config manually, then install the service:

```yaml
tunnel: herdr-mobile-relay-my-computer
credentials-file: /absolute/path/to/<TUNNEL_ID>.json

ingress:
  - hostname: relay-my-computer.example.com
    service: http://127.0.0.1:8375
  - service: http_status:404
```

```bash
make service-install
make setup-link HOST=relay-my-computer.example.com
```

`make service-install` remains intentionally low level: it requires an existing Cloudflare config, installs the platform service, and verifies only local health. Public DNS/HTTPS readiness and QR gating belong to `make stable-setup`.

The setup link configures the app served at the relay's own hostname. If you installed the app from a separately hosted origin instead, add the relay in that app's Settings using the relay URLs:

```text
wss://relay-mac.yourdomain.com
wss://relay-fedora.yourdomain.com
```

Do not run multiple computers as replicas of the same Cloudflare Tunnel if they serve different local relays. Cloudflare may send a WebSocket to any connector for that tunnel.

## Background Services

The background service starts both the relay and `cloudflared`. The generic commands detect macOS or Linux automatically:

```bash
make service-install
make service-status
make service-logs
make service-uninstall
```

The older `make macos-service-*` and `make linux-service-*` targets remain available for explicit platform-specific use.

Marketplace users do not need to find the managed checkout or run Make. The existing Stable Tunnel action now opens the guided wizard:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

Checkout `make setup-link` and `make rotate-token` commands manage checkout configuration only. They fail before reading or changing a token if the installed service is pinned to a different plugin-managed `relay.env`; use the matching Herdr plugin action or set `HERDR_RELAY_ENV` explicitly to that service configuration.

Local checkouts store runtime values in `relay/.env`; marketplace installs use `relay.env` under `herdr plugin config-dir herdr-mobile-relay.events`. Setup generates the token and a private non-secret instance ID; the stable wizard also records the tunnel path. The instance ID is internal and intentionally omitted from `.env.example`.

```env
HERDR_RELAY_TOKEN=<shared-secret>
CLOUDFLARED_CONFIG=/absolute/path/to/config.yml
```

Read the token for the web app:

```bash
sed -n 's/^HERDR_RELAY_TOKEN=//p' relay/.env
```

New installs use `com.herdr-mobile-relay.service` on macOS and `herdr-mobile-relay.service` on Linux/Fedora. The installers remove the earlier `herdr-remote` service labels when they run. Existing ignored `relay/.env` files may still point at `config-herdr-remote.yml`; the wizard reuses it when it passes current validation.

## Token Auth

Enable relay auth with:

```bash
export HERDR_RELAY_TOKEN="$(openssl rand -hex 16)"
make relay-run
```

Use the same `HERDR_RELAY_TOKEN` on multiple relays if you want one shared phone-side secret.

If a token leaks — for example a shared screenshot of a setup QR code — rotate it:

```bash
make rotate-token
```

This writes a new token to the active relay configuration (`relay/.env` for a checkout or the stable Herdr plugin config directory), restarts the background service when one is installed, and prints a fresh setup link and QR code to re-add the relay on each phone. Configurations using the old token stop working as soon as the relay restarts.

## Herdr Plugin Hook

The marketplace plugin provides both the Quick Start action and faster local agent-status events:

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

For local development, link the checkout instead:

```bash
make relay-plugin
```

The plugin sends local agent-status events to the local relay over UDP on `127.0.0.1:8376`. It does not expose another network service and does not connect to other computers.

For a support-friendly summary of the resolved configuration, background-service state, and local relay health, invoke the Status action:

```bash
herdr plugin action invoke status --plugin herdr-mobile-relay.events
```

The repository-root `herdr-plugin.toml` declares Quick Start, background-service, and Status actions, their managed setup panes, and the event hook. Marketplace configuration lives outside Herdr's replaceable managed checkout; `make relay-plugin` remains the convenient local-development command.

## Security Model

- The relay-served phone app is public static content; status and control WebSockets still require the relay token.
- Quick-start setup links carry the token only in the URL fragment, which is not included in HTTP requests, and the app removes it from the address bar after import.
- Relay access is protected with `HERDR_RELAY_TOKEN` for quick-start and service installs.
- Cloudflare Tunnel provides the public TLS endpoint; the relay itself listens locally on `127.0.0.1:8375` by default.
- Set `HERDR_RELAY_HOST` only if you intentionally need a non-loopback bind address. The relay refuses a non-loopback bind without `HERDR_RELAY_TOKEN`.
- Tokenless browser connections are rejected unless their `Origin` is listed in `HERDR_ALLOWED_ORIGINS` as a comma-separated list, such as `https://your-pages-site.pages.dev`.
- The web app stores relay URLs and tokens in browser local storage on the device where you configure it.
- Web Push VAPID private keys and push subscriptions are private local runtime state. Checkouts keep them in ignored `relay/push/`; marketplace installs keep them beside `relay.env` in Herdr's stable plugin config directory so reinstalling the managed checkout does not remove phone subscriptions.
- A connected web client can send text and key input to Herdr panes exposed by that relay. Treat relay URLs and tokens as sensitive.
- Notification action payloads contain only relay/pane routing metadata. The PWA reconnects with its locally stored relay credential and performs device verification when enabled before sending the action.
- Agent launch requests select only from supported executables automatically found by the relay; the browser cannot submit an executable or shell command.
- The relay executes only fixed local `herdr pane ...` and supported `herdr agent ...` operations; it does not shell into other machines.

## Health and Versions

`GET /health` keeps the original plain-text `ok` body for existing uptime checks and adds the relay instance in the `X-Herdr-Relay-Instance` header. `GET /healthz` returns `{"status": "ok", "instance": "<non-secret-id>", "version": "<git-hash>[-dirty]", "protocol": <n>}` without authentication for detailed monitoring. The stable wizard compares all four fields with the local service before printing a QR. The relay also prints its version at startup and reports it to the app, which shows it per relay in Settings. When a relay and the app speak different protocol versions, Settings shows an update warning and blocks incompatible control commands.

Uploaded images in `~/.cache/herdr-mobile-relay/uploads` are pruned automatically after 7 days.

## License

Herdr Mobile Relay is licensed under the [GNU Affero General Public License v3.0 or later](LICENSE).

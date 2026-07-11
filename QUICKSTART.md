# Herdr Mobile Relay Quick Start

This is the beginner path. It gets one Linux or macOS computer connected to one phone before asking you to configure permanent domains or background services.

> [!IMPORTANT]
> Windows is not currently supported.

## Before You Start

You need Herdr 0.7.0 or newer, Git, and `curl`. Check with:

```bash
herdr --version && git --version && curl --version
```

You do **not** need a Cloudflare account, domain, existing Python installation, Node.js, or separately hosted web app for this trial. Cloudflare describes [TryCloudflare quick tunnels](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/) as free testing tunnels that create temporary random hostnames without moving a domain to Cloudflare.

## 1. Install and Start the Plugin

```bash
herdr plugin install 0cv/herdr-mobile-relay &&
  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events
```

This is one paste with two consent points. Herdr previews the plugin commands before you confirm the installation, including a one-time build step that installs `uv` (user-level) if missing and prepares the relay's Python environment. A failed download does not block plugin registration; Quick Start retries missing setup interactively. After installation succeeds, the chained action opens the Quick Start pane.

A managed setup pane opens inside Herdr and:

1. Creates a private relay token and minimal local configuration.
2. Detects `uv` and `cloudflared`.
3. Offers to install anything missing using the tools' official installers or release binaries.
4. Starts the relay and serves the phone app from it.
5. Starts a temporary Cloudflare quick tunnel.

If it asks to install missing tools, type `y` and press Enter. Installation is for your user account; the script does not need `sudo`.

Prefer a keystroke over the command? Bind the action in Herdr's `config.toml`, then run `herdr server reload-config`:

```toml
[[keys.command]]
key = "prefix+m"
type = "plugin_action"
command = "herdr-mobile-relay.events.quick-start"
description = "Herdr Mobile Relay: Quick Start"
```

Plugin configuration is stored under Herdr's persistent config directory, not its replaceable managed checkout. Print the directory at any time with:

```bash
herdr plugin config-dir herdr-mobile-relay.events
```

### Local Checkout Fallback

To develop the relay or run it without installing the marketplace plugin:

```bash
git clone https://github.com/0cv/herdr-mobile-relay.git && cd herdr-mobile-relay && make quick-start
```

The checkout path stores its configuration in `relay/.env`; the rest of the experience is the same.

Checkout `make setup-link` and `make rotate-token` commands refuse to run when an installed service points at a different plugin-managed configuration. This prevents printing a QR token that the running relay never loaded.

## 2. Scan the QR Code (or Open the Phone Setup Link)

After startup, the Quick Start pane shows a QR code and the matching Phone setup link:

<img src="images/quickstart.png" alt="Quick Start output showing a QR code, the private phone setup link, and manual setup details" width="640">

Point your phone camera at the QR code and open the link it offers—that is the whole setup. The app opens, saves the relay URL and token, and connects automatically; there is nothing to type or paste into Settings. If the QR code does not appear or does not scan, open the printed HTTPS link on your phone instead—it is the same link.

The token is carried in the URL fragment, which browsers do not send to the server, and the app removes the fragment from the address bar immediately after importing it.

Do not share the setup link or a photo of the QR code: anyone who has both the tunnel URL and token can control agents exposed by that relay.

## 3. Use It

Keep the Quick Start pane open. Run your normal coding agent in another Herdr pane, or tap **＋** in the phone app to start an installed Codex, Claude Code, or OpenCode agent in a selected project directory.

The quick tunnel stops when you press Ctrl-C or close its pane. The next **Herdr Mobile Relay: Quick Start** action keeps the same relay token but creates a new random tunnel URL, so scan the newly printed QR code (or open the new link).

## If It Does Not Work

For a local checkout, run the non-installing prerequisite check:

```bash
make setup
```

Common issues:

- **`herdr`, `git`, or `curl` is missing:** install it, then rerun the plugin installation. Make is needed only for the local-checkout fallback.
- **Port 8375 is already in use:** stop the existing relay or service, then run the Quick Start action again.
- **The phone link times out:** keep the Quick Start pane open and check whether `cloudflared` printed a connectivity error.
- **The link or QR code never loads (site cannot be reached):** some home routers cache a failed DNS lookup for up to 30 minutes if the tunnel hostname is opened before its DNS record goes live. The quick start waits for the hostname to resolve before printing the QR code, so this should be rare. If it still happens, press Ctrl-C and run the Quick Start action again; each run gets a fresh hostname. Switching the phone to mobile data for the first open also bypasses the router's cache.
- **The app opens but does not connect:** reopen the complete newly printed link, including the `#setup=...` fragment.
- **macOS blocks a project folder:** choose a non-protected project folder or grant the Herdr relay process the appropriate Files and Folders permission.

## Make It Permanent

TryCloudflare quick tunnels are intended for testing, have no uptime guarantee, and change hostname when restarted. For everyday use:

1. Create a Cloudflare account and put a domain on Cloudflare.

   > [!TIP]
   > An available all-numeric `.xyz` domain with 6–9 digits typically costs about $1 per year through Cloudflare Registrar. The `.xyz` registry lists this numeric class at $0.99/year, and Cloudflare sells domains at registry cost without markup. Verify the current price at checkout.

2. Follow the README's [Stable Hostnames](README.md#stable-hostnames) section once per computer.
3. Invoke the service installer action; its managed pane installs the platform user service and prints the stable QR code:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

For a local checkout, the equivalent commands are:

```bash
make service-install
make setup-link
```

This prints a QR code and setup link for the stable hostname; scanning it adds the relay to the app automatically.

Repeat the relay setup on each Linux or macOS computer. You can add every stable relay to the same phone app; agents are merged client-side.

## Optional: Host the App Separately

The relay-served app is sufficient for the quick start and stable single-relay setup. For an app origin that remains available independently of any relay—especially with multiple computers—host `web/` on any HTTPS static host. Cloudflare Pages deployment requires a Cloudflare account plus Node.js/npm:

```bash
# Edit WEB_PROJECT in .env first if needed.
make web-deploy
```

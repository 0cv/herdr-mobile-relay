# Herdr Mobile Relay Quick Start

This connects one Linux or macOS computer to your phone through a temporary TryCloudflare tunnel. It does not require a Cloudflare account or domain.

> [!IMPORTANT]
> Native Windows is not supported. You need Herdr 0.7.5 or newer, Git, and `curl`.

## 1. Install the Plugin

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

When the setup menu opens, choose **Quick Start**. If no menu opens, run:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Approve installation of missing user-level tools if prompted. Quick Start creates private relay configuration, starts the relay and bundled phone app, and opens a temporary Cloudflare tunnel.

You do not need Python, Node.js, npm, a separately hosted web app, or `sudo` for this path.

## 2. Open the Phone Link

Wait for the temporary tunnel, then choose **This temporary relay** for a simple one-relay trial or **An existing installed Herdr app** to add this computer to an app that already contains other relays. Scan the printed QR code or open the complete HTTPS setup link on your phone.

The app imports the relay URL, label, and token automatically. Do not share the link or QR code: either can grant control of agents on this relay.

Keep the Quick Start pane open while testing. Ctrl-C stops the relay and tunnel. The next run creates a different hostname, so scan its new QR code.

## 3. Try It

Run an agent in Herdr, or tap **＋** in the phone app to start a detected Codex, Claude Code, OpenCode, or any agent you have configured (see [Agent Profiles Configuration](README.md#agent-profiles-configuration)). You can inspect output, send prompts, answer approvals and structured questions, upload an image, and manage the agent lifecycle.

## Make It Permanent

For everyday use, add a domain to Cloudflare and run the guided stable setup:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard confirms the tunnel and hostname, creates or resumes the DNS route, installs the background service, and prints a QR code only after the public relay identity is verified.

On the first stable setup, choose **This relay** to let it host the phone app. If Herdr is already installed on the phone from another HTTPS address, choose **An existing installed Herdr app** and enter the domain or URL shown in that app's Site settings. Entering `app.example.com` is enough; the wizard adds `https://` automatically and checks for the Herdr app manifest. The saved selection is shown on later interactive runs and stored only in the relay's private configuration.

If it stops, rerun the exact command it prints. Setup resumes its recorded state rather than creating a duplicate tunnel.

Repeat stable setup on each computer with a different hostname. Add each QR link to the same phone app.

After the stable service is running with Herdr 0.7.5 or newer, Settings can check and install later versioned relay updates one computer at a time. An older relay shows **Update Help** with the one-time command to update and restart it. That Marketplace command preserves the configuration used by an existing checkout-installed service; the checkout command remains available for users who prefer to stay checkout-managed. A separately hosted phone app remains a separate deployment.

The app also checks the committed upstream app release. Relay-hosted apps update with their relay. If you deliberately host one app separately on Cloudflare Pages, configure exactly one stable relay as its deployment owner:

```bash
herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events
```

The action offers to deploy the current release immediately, which updates an older installed PWA that does not have the Deploy button yet. After that one-time bootstrap, **Settings → Deploy App** can publish later verified committed bundles and reload the phone only after the public origin reports the new version.

Useful actions:

```bash
herdr plugin action invoke setup-link --plugin herdr-mobile-relay.events
herdr plugin action invoke status --plugin herdr-mobile-relay.events
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
```

Use `setup-link` whenever you need to reprint the private link and QR for an existing stable relay.

An installed PWA privately tells connected relays which app origin to use for later QR codes; no public app hostname is built in. If a relay was removed before that registration happened, a source checkout can bootstrap it once with `make setup-link APP_URL=app.example.com`, using the domain from the installed app's site settings.

Teardown removes only stable resources recorded as wizard-owned. Run it before uninstalling the plugin if those Cloudflare resources should also be removed.

## Local Checkout

For development or a non-marketplace installation:

```bash
git clone https://github.com/0cv/herdr-mobile-relay.git
cd herdr-mobile-relay
make quick-start
```

Use `make stable-setup` for the checkout's permanent setup. Checkout commands use `relay/.env`; plugin commands use the plugin's persistent configuration. Do not use a checkout's `make setup-link` for a running marketplace installation—the mismatch is intentionally rejected.

## Troubleshooting

- **No setup menu:** run the `setup` plugin action above.
- **Port 8375 is already in use:** stop the previous Quick Start or installed relay service.
- **The temporary URL times out:** keep the setup pane open and check its `cloudflared` error. Rerun Quick Start for a fresh hostname.
- **The app opens but remains disconnected:** reopen the complete link, including `#setup=...`.
- **Stable setup fails or times out:** keep its state file and rerun the exact command shown.
- **Need the stable QR again:** choose **Show Phone Setup QR** or invoke the `setup-link` plugin action.
- **Stable DNS name already exists:** choose another hostname; the wizard never overwrites an existing record.
- **macOS cannot browse a project folder:** grant the relay process access under Files and Folders, or choose an unprotected directory.

For more detail, see the [README](README.md).

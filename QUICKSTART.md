# Herdr Mobile Relay Quick Start

Connect one Linux or macOS computer to your phone through a temporary
TryCloudflare tunnel. You need Herdr 0.7.5 or newer, Git, and `curl`.

## 1. Install

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

Choose **Quick Start** when the setup menu opens. If it does not:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Approve missing user-level tools if prompted. The plugin downloads the exact
verified relay bundle; it does not require Python, Node.js, a Go toolchain, or
`sudo`.

## 2. Pair the Phone

Wait for the temporary tunnel, then choose:

- **This temporary relay** for a simple one-computer trial.
- **An existing installed Herdr app** to add this computer to an existing app.

Scan the QR or open the complete HTTPS setup link. Keep it private: it contains
the relay encryption key in the URL fragment. The fragment is not sent in the
HTTP request, and the app removes it after import.

Keep the Quick Start pane open. Ctrl-C stops both processes, and the next run
creates a new hostname and setup link.

## 3. Try It

Run an agent in Herdr or tap **＋** in the phone app. You can inspect output,
send prompts, answer approvals and plan questions, upload images, and manage the
agent lifecycle.

## Make It Permanent

Add a domain to Cloudflare, then run:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard creates or resumes a dedicated tunnel, installs a background user
service, verifies the public endpoint, and prints the stable QR. Repeat this on
each computer with a different hostname and add every QR to the same phone app.

Useful actions:

```bash
herdr plugin action invoke setup-link --plugin herdr-mobile-relay.events
herdr plugin action invoke status --plugin herdr-mobile-relay.events
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
herdr plugin action invoke uninstall --plugin herdr-mobile-relay.events
```

Run `stable-teardown` before uninstall if the wizard-owned Cloudflare tunnel and
DNS route should also be removed.

## Troubleshooting

- **Port 8375 is busy:** stop the previous Quick Start or installed service.
- **Temporary URL fails:** rerun Quick Start for a fresh hostname.
- **App stays disconnected:** reopen the full link including `#setup=...`.
- **Need the stable QR again:** invoke `setup-link`.
- **Stable setup stops:** rerun the exact command it prints; setup is resumable.

See [README.md](README.md) for stable deployment, updates, development, and
security details.

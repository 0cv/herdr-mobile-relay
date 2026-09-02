# Herdr Mobile Relay Quick Start

Connect one Linux or macOS computer to your phone through a temporary Cloudflare
tunnel, or through a gateway that needs no Cloudflare account (see **Skip
Cloudflare**). You need Herdr 0.7.5 or newer, Git, and `curl`.

## 1. Install

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

Choose **Temporary Cloudflare Tunnel** when the setup menu opens. If it does
not:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Approve missing user-level tools if prompted. The plugin downloads the exact
verified relay bundle; it does not require Python, Node.js, a Go toolchain, or
`sudo`.

## 2. Pair the Phone

Both paths print the QR once they know which origin serves the phone app.

On the tunnel path, wait for the temporary tunnel, then choose:

- **This temporary relay** for a simple one-computer trial.
- **An existing installed Herdr app** to add this computer to an app you already
  use.

On the gateway path the QR follows registration, and the app origin has to be an
installed Herdr app — a gateway carries relay traffic only. It reuses a recorded
or `HERDR_PHONE_APP_URL` origin, and asks for one when neither exists.

Scan the QR or open the complete HTTPS setup link. Keep it private: it contains
the one-use bootstrap invitation in the URL fragment, which is never sent in
the HTTP request. The installed app removes it after enrollment. iOS browser
tabs retain it without redeeming it and direct you to the installed app, which
prevents a disposable Safari tab from consuming the invitation.

Keep the Quick Start pane open. Ctrl-C stops the relay, and on the tunnel path
the next run creates a new hostname and setup link.

## 3. Try It

Run an agent in Herdr or tap **＋** in the phone app. You can inspect output,
send prompts, answer approvals and plan questions, upload images, and manage the
agent lifecycle.

On hosts with a published Piper runtime, setup downloads the engine and the
English voice that reads responses aloud, cached outside the release so updates
never fetch them again. Reading aloud turns itself on the first time. French,
German, Spanish, and Chinese are downloaded on demand from the phone's Settings
or with `relay/speech-voices.sh --languages fr`. Stock Apple Silicon uses
macOS `say`; Settings does not offer neural voice downloads unless Piper is
already installed.

## Skip Cloudflare

Choose **Community WebRTC Gateway** in the setup menu. It checks the project's
published gateways, saves the ordered list, then starts the relay and prints its
QR. No account, no domain, no `cloudflared`. The gateways are run by the
project: free, shared, best-effort.

A gateway carries relay traffic only, so the phone app lives elsewhere: point
`HERDR_PHONE_APP_URL` at an installed Herdr app, or host one with
`make web-deploy`.

A gateway cannot read your traffic — it copies frames that are already encrypted
between the phone and the relay — and right after connecting both sides try to
cut it out of the path with a direct WebRTC connection.

[docs/transports.md](docs/transports.md) explains every choice and its settings;
[docs/gateway-self-hosting.md](docs/gateway-self-hosting.md) covers running your
own gateway.

## Make It Permanent

Add a domain to Cloudflare, then run:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard creates or resumes a dedicated tunnel, installs a background user
service, verifies the public endpoint, and prints the stable QR. Repeat it on
each computer with a different hostname and add every QR to the same phone app.

[docs/cloudflare-tunnel.md](docs/cloudflare-tunnel.md) has the rest: hostname
changes, the full action list, teardown, and uninstall.

## Troubleshooting

- **Port 8375 is busy:** stop the previous Quick Start or installed service.
- **Temporary URL fails:** rerun Quick Start for a fresh hostname.
- **Gateway registration times out:** check `HERDR_GATEWAY_URL` and outbound
  HTTPS access; `curl -s localhost:8375/healthz` reports `gateway.registered`.
- **App stays disconnected:** reopen the full link including `#setup=...`.
- **Need the stable QR again:** invoke `setup-link`.
- **Stable setup stops:** rerun the exact command it prints; setup is resumable.

[README.md](README.md) indexes the rest of the documentation.

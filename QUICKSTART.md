# Herdr Mobile Relay Quick Start

Connect one Linux or macOS computer to your phone, either through a blind
gateway with no Cloudflare account (see **Skip Cloudflare** below) or through a
temporary TryCloudflare tunnel. You need Herdr 0.7.5 or newer, Git, and `curl`.

## 1. Install

```bash
herdr plugin install 0cv/herdr-mobile-relay-dev
```

Choose **Quick Start** when the setup menu opens. If it does not:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Approve missing user-level tools if prompted. The plugin downloads the exact
verified relay bundle; it does not require Python, Node.js, a Go toolchain, or
`sudo`.

## 2. Pair the Phone

On the gateway path the QR appears once the registration is confirmed. On the
tunnel path, wait for the temporary tunnel, then choose:

- **This temporary relay** for a simple one-computer trial.
- **An existing installed Herdr app** to add this computer to an existing app.

Scan the QR or open the complete HTTPS setup link. Keep it private: it contains
the relay encryption key in the URL fragment. The fragment is not sent in the
HTTP request, and the app removes it after import.

Keep the Quick Start pane open. Ctrl-C stops the relay, and on the tunnel path
the next run creates a new hostname and setup link.

## 3. Try It

Run an agent in Herdr or tap **＋** in the phone app. You can inspect output,
send prompts, answer approvals and plan questions, upload images, and manage the
agent lifecycle.

## Skip Cloudflare

Choose **Choose Connection Method → Community gateway** in the setup menu and
Quick Start never installs, launches, or needs `cloudflared`. The gateway is run
by the project, free, shared, and best-effort: no account, no domain, and nothing
to type. Point at a different one by setting a gateway URL yourself:

```bash
HERDR_GATEWAY_URL=wss://gw.example.com make quick-start
```

The relay registers with that gateway itself and the QR appears as soon as the
registration is confirmed. The gateway holds no secrets and cannot read your
traffic: it only copies frames that are already encrypted between the phone and
the relay, under an id derived from the relay key. It is a single static binary
you can self-host, and the menu can deploy one to your own server over SSH — see
[docs/gateway-self-hosting.md](docs/gateway-self-hosting.md) and
[docs/transports.md](docs/transports.md).

Right after connecting, the phone and computer try to cut the gateway out: they
exchange a WebRTC offer, answer, and ICE candidates inside the encrypted channel
and move the same frames onto a direct DataChannel. If the direct path cannot
form, or breaks later, the session stays on (or returns to) the gateway without
reconnecting.

That direct path opens a UDP socket on the computer, where the tunnel was
strictly outbound. Reaching it is not enough to talk to the relay: ICE requires
the session credentials that travel only inside the encrypted channel, the DTLS
certificate is pinned by the fingerprint in the exchanged SDP, and the Herdr
end-to-end handshake is still the only authorization for control. Set
`HERDR_REACHABILITY_PORT_MAPPING=0` to stop the relay from asking your router
for a port mapping, `HERDR_WEBRTC_UDP_PORT` to fix the port, or
`HERDR_TRANSPORT_FORCE_RELAY=1` to keep everything on the gateway.

The phone app itself is not served by the gateway. Point `HERDR_PHONE_APP_URL`
at an installed Herdr app, or host one yourself with `make web-deploy`.

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
- **Gateway registration times out:** check `HERDR_GATEWAY_URL` and outbound
  HTTPS access; `curl -s localhost:8375/healthz` reports `gateway.registered`.
- **App stays disconnected:** reopen the full link including `#setup=...`.
- **Need the stable QR again:** invoke `setup-link`.
- **Stable setup stops:** rerun the exact command it prints; setup is resumable.

See [README.md](README.md) for stable deployment, updates, development, and
security details.

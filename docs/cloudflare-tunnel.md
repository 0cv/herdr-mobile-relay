# Stable Cloudflare tunnel setup

How to give one computer a permanent hostname and a background relay service
through a dedicated Cloudflare Tunnel. Read this only if you want the relay to
stay reachable without an open pane; the temporary TryCloudflare tunnel that
Quick Start opens needs none of it — no Cloudflare account, no domain, and no
service installation.

## When you need this page

Quick Start opens a temporary TryCloudflare tunnel and prints its QR code. The
pane has to stay open, and the hostname changes every time. The stable path
instead creates a dedicated tunnel on a domain you control, installs a user
service so the relay survives a closed pane, and keeps the same hostname across
restarts. It needs a Cloudflare account with a domain added to it.

## Run the wizard

For a permanent hostname and background service, add a domain to Cloudflare and
run:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard creates or resumes a dedicated tunnel, checks the DNS route, installs
a user service, verifies the public relay identity, and then prints the private
phone QR. Run it once per computer with a distinct hostname, then add every QR
to the same phone app.

Because the wizard creates *or resumes*, it is safe to rerun: if it stops part
way, keep its state and rerun the exact command it printed.

## Useful actions

```bash
herdr plugin action invoke setup-link --plugin herdr-mobile-relay.events
herdr plugin action invoke status --plugin herdr-mobile-relay.events
herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
herdr plugin action invoke uninstall --plugin herdr-mobile-relay.events
```

`setup-link` reprints the private phone QR and setup link. `status` reports the
current state. `configure-app-deploy` designates this stable relay as the
deployment owner for a separately hosted Cloudflare Pages app — see
[docs/updates.md](updates.md).

## Teardown

Run `stable-teardown` before uninstall if Cloudflare resources should also be
removed. Full uninstall removes the service, releases, relay state, push
credentials, cache, and plugin registration.

## Troubleshooting

- **No setup menu:** invoke the `setup` action:
  `herdr plugin action invoke setup --plugin herdr-mobile-relay.events`.
- **Temporary URL fails:** keep the pane open and rerun Quick Start for a new
  hostname if `cloudflared` stopped.
- **Stable setup stops:** keep its state and rerun the exact command printed.
- **Need the stable QR:** invoke the `setup-link` action.
- **App opens but stays disconnected:** reopen the complete setup link,
  including its `#setup=...` fragment.

The QR imports the relay URL, label, and relay key, so treat the QR and setup
link as secrets.

# Stable Cloudflare tunnel setup

How to give one computer a permanent hostname and a background relay service
through a dedicated Cloudflare Tunnel. Read this only if you want the relay to
stay reachable without an open pane.

## When you need this page

Quick Start's temporary tunnel needs an open pane and gets a new hostname every
time. The stable path creates a dedicated tunnel on a domain you control,
installs a user service, and keeps the same hostname across restarts. It needs
a Cloudflare account with a domain added to it.

## Run the wizard

Add a domain to Cloudflare, then run:

```bash
herdr plugin action invoke install-service --plugin herdr-mobile-relay.events
```

The wizard ends by printing the private phone QR. Run it once per computer with
a distinct hostname, then add every QR to the same phone app.

If the default `cloudflared` config already exists, the wizard displays its
tunnel, hostname, and public DNS status before asking whether to reuse it. It
does not adopt the config unattended; `HERDR_STABLE_REUSE_CONFIG=1` is the
explicit opt-in for automation.

## Useful actions

```bash
herdr plugin action invoke setup-link --plugin herdr-mobile-relay.events
herdr plugin action invoke change-hostname --plugin herdr-mobile-relay.events
herdr plugin action invoke status --plugin herdr-mobile-relay.events
herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events
herdr plugin action invoke stable-teardown --plugin herdr-mobile-relay.events
herdr plugin action invoke uninstall --plugin herdr-mobile-relay.events
```

`setup-link` reprints the private phone QR and setup link. `status` reports the
current state. `configure-app-deploy` designates this stable relay as the
deployment owner for a separately hosted Cloudflare Pages app — see
[docs/updates.md](updates.md).

`change-hostname` moves the relay to another name — a new domain, say — by
routing it to the same tunnel and rewriting the ingress. The tunnel, its
credentials, and the relay token stay, so phones only need the new link, and
the old record keeps answering until you delete it in Cloudflare.

A tunnel's origin certificate covers one zone, and `cloudflared` turns a name
outside it into a subdomain of it: ask for `relay.new.example` and get
`relay.new.example.old.example`. The action reads the zone out of
`~/.cloudflared/cert.pem`, refuses before anything is created, and offers to
sign in for the right zone — keeping the old certificate, which routes in the
previous zone still need. Nothing local changes until the new name answers at
the edge, and a failed move restores the previous hostname.

## Teardown

Run `stable-teardown` before uninstall if its Cloudflare resources should also
be removed. After the explicit `teardown` confirmation, it removes the service,
tunnel, config, credentials, and matching local config pointer recorded in the
validated Herdr stable state. Historical `created_by_wizard` flags do not
authorize the operation: a relay previously adopted from an existing config is
still the configured relay and is removed. The state ownership marker, service
environment match, and Herdr tunnel-name namespace protect unrelated resources.

`cloudflared` cannot dependably delete a DNS route. If the record remains,
teardown preserves its diagnostic state and names the exact record to remove
in the Cloudflare dashboard. Rerun teardown afterward to finish. Use
`change-hostname` instead when the tunnel should be retained under a new name.

Full uninstall removes the service, releases, relay state, push credentials,
cache, and plugin registration.

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

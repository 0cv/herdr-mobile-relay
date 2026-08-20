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

The URL before `#setup=...` is the phone-app origin; it must stay identical on
every computer because installed-app identity and relay storage are
origin-scoped. The relay's own `wss://` hostname remains inside the private
fragment. On a new computer the wizard checks `https://herdr.<authorized-zone>`
for an existing Herdr app and uses it when found. In the setup menu, choose
**Choose Phone App and Show QR** to keep or change this origin. Use **Configure
App Deployment** only on the one computer that should publish app updates; its
configured deployment origin is authoritative and cannot be replaced by an
older relay-hosted app reconnecting in the background.
When a current origin is shown, option **1** keeps it. Option **2** deliberately
switches to this relay's hostname, and option **3** selects another app.

`cloudflared` login authorizes one zone at a time. The wizard reads and
preselects that domain from `~/.cloudflared/cert.pem`; choose **Sign in to
Cloudflare for another domain** to use Cloudflare's account-zone picker and
replace the active authorization. Manual domain entry remains available when
the certificate's zone cannot be resolved.

When it can read the authorized zone, the wizard refuses a hostname outside it
before creating a tunnel. Either way it compares the exact CNAME reported by
`cloudflared` with the requested hostname afterwards: the CLI can otherwise exit
successfully after silently appending its old zone. A prior affected run names
the stray record to delete, then resumes with the same tunnel once the correct
zone is authorized.

If `CLOUDFLARED_CONFIG` or `~/.cloudflared/config-herdr-mobile-relay.yml`
already exists, the wizard displays its tunnel, hostname, and public DNS status
before asking whether to reuse it. It does not adopt the config unattended;
`HERDR_STABLE_REUSE_CONFIG=1` is the explicit opt-in for automation.

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
outside it into a subdomain of that zone: ask for `relay.new.example` and get
`relay.new.example.old.example`. Both stable setup and `change-hostname` read
the authorized zone when it is resolvable, refuse before creating the wrong
route, and offer to sign in for the right zone. The old certificate is retained
as a backup because routes in the previous zone may still need it. If the moved
hostname never answers its public health check, the previous local config is
restored.

## Teardown

Run `stable-teardown` before uninstall if its Cloudflare resources should also
be removed. After the explicit `teardown` confirmation, it removes the service,
tunnel, config, credentials, and matching local config pointer recorded in the
validated Herdr stable state. Historical `created_by_wizard` flags do not
authorize the operation: a relay previously adopted from an existing config is
still the configured relay and is removed. The state ownership marker, service
environment match, and Herdr tunnel-name namespace protect unrelated resources.
If an older teardown cleared state after preserving every resource, the action
can recover the teardown identity from the retained config. Recovery needs a
config whose `tunnel:` entry is a `herdr-mobile-relay-*` name, a loopback relay
origin on the configured port, a hostname, and credentials matching that tunnel;
otherwise it refuses without deleting anything.

`cloudflared` cannot dependably delete a DNS route. If the record remains,
teardown preserves its diagnostic state and names the exact record to remove
in the Cloudflare dashboard. Rerun teardown afterward to finish. Use
`change-hostname` instead when the tunnel should be retained under a new name.

Full uninstall removes the service, releases, relay state, push credentials, and
cache. It also removes the plugin registration when Herdr is reachable, and
prints the manual command when it is not.

## Troubleshooting

- **No setup menu:** invoke the `setup` action:
  `herdr plugin action invoke setup --plugin herdr-mobile-relay.events`.
- **Stable setup stops:** keep its state and rerun the exact command printed.
- **Need the stable QR:** invoke the `setup-link` action.
- **Wrong zone appended to the hostname:** delete the stray record the wizard
  names, authorize the right zone, then rerun.

[QUICKSTART.md](../QUICKSTART.md) covers the failures shared with first-time
setup.

The QR imports the relay URL, label, and relay key, so treat the QR and setup
link as secrets.

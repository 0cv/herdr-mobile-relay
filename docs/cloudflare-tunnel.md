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
for an existing Herdr app and uses it when found. If the app has another
hostname, choose **An existing installed Herdr app** and enter that exact
origin. `configure-app-deploy` records its selected origin for later QRs.

`cloudflared` login authorizes one zone at a time. The wizard reads and
preselects that domain from `~/.cloudflared/cert.pem`; choose **Sign in to
Cloudflare for another domain** to use Cloudflare's account-zone picker and
replace the active authorization. Manual domain entry remains available when
the certificate's zone cannot be resolved.

The wizard refuses a hostname outside the authorized zone before creating a
tunnel. It also compares the exact CNAME reported by `cloudflared` with the
requested hostname: the CLI can otherwise exit successfully after silently
appending its old zone. A prior affected run names the stray record to delete,
then resumes with the same tunnel after the correct zone is authorized.

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
outside it into a subdomain of that zone: ask for `relay.new.example` and get
`relay.new.example.old.example`. Both stable setup and `change-hostname` read
the authorized zone, refuse before creating the wrong route, and offer to sign
in for the right zone. The old certificate is retained as a backup because
routes in the previous zone may still need it. A failed hostname move restores
the previous local config.

## Teardown

Run `stable-teardown` before uninstall if its Cloudflare resources should also
be removed. After the explicit `teardown` confirmation, it removes the service,
tunnel, config, credentials, and matching local config pointer recorded in the
validated Herdr stable state. Historical `created_by_wizard` flags do not
authorize the operation: a relay previously adopted from an existing config is
still the configured relay and is removed. The state ownership marker, service
environment match, and Herdr tunnel-name namespace protect unrelated resources.
If an older teardown cleared state after preserving every resource, the action
recovers the teardown identity from the retained config. Recovery requires a
valid Herdr tunnel name, loopback relay origin on the configured port, hostname,
and matching tunnel credential UUID; otherwise it refuses without deleting
anything.

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

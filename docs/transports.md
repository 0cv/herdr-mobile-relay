# How your phone reaches your computer

Four transports can carry traffic between your phone and a relay: a Cloudflare
tunnel, Tailscale Serve, the community gateway, or a gateway you run yourself.
All four use end-to-end relay encryption. Only the two gateway choices then try
to leave the transport behind: phone and computer negotiate a direct peer-to-peer
connection and the gateway is left carrying the fallback. Cloudflare tunnel
traffic always goes through Cloudflare; Tailscale Serve is tailnet-only.

## The four choices

| Choice | What it needs from you | Who carries the traffic | When to pick it |
| --- | --- | --- | --- |
| **Cloudflare tunnel** | Nothing for Quick Start's temporary URL; a Cloudflare account with a domain for a permanent hostname and background service. | Cloudflare's edge | The default. See [cloudflare-tunnel.md](cloudflare-tunnel.md) for the permanent hostname. |
| **Tailscale Serve** | Tailscale on phone and computer in the same tailnet; MagicDNS and HTTPS certificates enabled. | Your Tailscale tailnet | Private HTTPS without a Cloudflare account, domain, or gateway. |
| **Community gateway** | No account and no domain, but the phone app must already be hosted somewhere — a gateway serves no app. | A gateway operated by the project, until the direct path forms | Free, shared, best-effort; not for heavy transfers. Pick it to avoid Cloudflare setup entirely. |
| **Your own gateway** | A small VPS with Docker and a public hostname. | Your own gateway, until the direct path forms | Dedicated bandwidth, and the transport logs stay on your machine. See [gateway-self-hosting.md](gateway-self-hosting.md). |

Pick **Temporary Cloudflare Tunnel**, **Community WebRTC Gateway**, **Deploy or
Upgrade Your Own WebRTC Gateway**, **Stable Tunnel**, or **Always on Tailscale**
from the setup menu. A completed choice starts or restarts the relay and prints
the phone QR; there is no second Quick Start step.

## Tailscale Serve

Install Tailscale on your phone; the computer's Tailscale CLI must be 1.52 or
newer. Sign in to the same tailnet and enable MagicDNS and HTTPS certificates in
the tailnet DNS settings. Supported computers are Linux and macOS with
standalone or App Store Tailscale; the App
Store app must be running. The relay uses your installed Tailscale CLI (not an
embedded dependency), searching `HERDR_TAILSCALE_BIN` (absolute executable),
then `PATH`, then `~/Applications/Tailscale.app/Contents/MacOS/Tailscale` or
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`. It sets
`TAILSCALE_BE_CLI=1` automatically. If Linux denies Serve permission, **you**
can run `sudo tailscale set --operator=$USER` once; setup never runs it for you.

**Always on Tailscale** asks for confirmation, installs or reuses the existing
user launchd/systemd relay service on localhost port 8375 by default, then
publishes it at HTTPS port 8443 via `tailscale serve --bg`. Serve persists
without the setup pane. After a reboot, the user service resumes according to
its normal login/start policy: launchd runs in the macOS user session; a Linux
systemd user service may require login or separately configured lingering to
run before login. Tailscale must also be running. Setup does not enable
lingering or make other automatic admin changes. Reinstalling, restarting, or
updating the existing service remains permitted while Tailscale is selected.

If a gateway is selected, choose a Cloudflare option first to deselect it.
Before selecting another transport, choose **Turn Always on Tailscale Off**;
transport switching is not automatic. If already configured, choose **Choose
Phone App and Show QR** rather than repeating setup. Setup refuses a conflicting
Serve port and reuses a matching background route; removal touches only this
relay's mapped HTTPS port, never other Serve ports or Funnel. If Serve removal
fails (including when the CLI is missing), the selection stays and the mapping
may remain: restore Tailscale and retry. Successful removal clears the selection
even if the subsequent service install or uninstall fails. If removal is
impossible, set `HERDR_TAILSCALE_ABANDON=1` for the turn-off action to clear the
selection without removing the mapping; clean up port 8443 manually. Full
uninstall attempts the same scoped removal and warns if manual cleanup is needed.

## The gateway path

Choosing a gateway skips `cloudflared` entirely — no Cloudflare account, domain,
or tunnel. Already running one? Choose **Deploy or Upgrade Your Own WebRTC
Gateway**, then **I already run a gateway** and type its address. Unattended
setups set the same list in the relay environment:

```bash
HERDR_GATEWAY_URL=wss://gw.example.com
```

The QR is printed once the gateway confirms the registration and the phone-app
origin is settled — the gateway carries relay traffic, never the app itself, so
the first run asks which installed Herdr app to pair with, or takes
`HERDR_PHONE_APP_URL`.

A gateway holds no secrets and never learns the relay key: the relay registers
under an id derived from that key, the phone answers a challenge that the *relay*
verifies, and the gateway only copies frames that are already encrypted. It is a
single static binary you can self-host, and the setup menu can deploy one to your
own server over SSH.

## The direct upgrade

Once a phone is connected through the gateway, both sides negotiate a direct
WebRTC DataChannel (`herdr-dc-v1`) inside the relayed session. The direct path
runs its own end-to-end handshake and takes over only after it has carried a real
message, so a half-working peer connection cannot strand the app. The gateway
session is dropped ten seconds later. If the direct path never forms, or later
breaks, the phone opens a relayed session again — automatically, with no
re-pairing.

Phone **Settings** names what each relay is using right now: `gateway <host>`
while relayed, `direct, via <host>` once the upgrade takes over, or
`relay URL <host>` on a Cloudflare tunnel, Tailscale Serve, or LAN address.

The gateway also answers address discovery on UDP 3478. That is what lets a phone
on a cellular network reach a home computer with no port forwarding and no router
configuration; the gateway only reflects a source address it already observes, so
no third-party service is involved. On a self-hosted gateway, inbound UDP 3478
must be open, because a TLS reverse proxy cannot carry raw UDP.

The direct path opens a UDP socket on the computer, where the tunnel was strictly
outbound. Reaching that socket is not enough to talk to the relay: ICE requires
session credentials that travel only inside the authenticated encrypted channel,
the DTLS certificate is pinned by the fingerprint in the exchanged SDP,
unsolicited packets are dropped by the ICE agent, and the end-to-end handshake
remains the only authorization for control on every path.

## Relay settings

- `HERDR_GATEWAY_URL` — one or more gateway base URLs, separated by commas
  (`wss://gw.example.com,wss://backup.example.com`). Empty, the default, keeps the
  Cloudflare tunnel path. The relay probes every candidate's health endpoint at
  startup, keeps exactly one registration, and after a failure excludes that
  entry for the pass and takes the next healthy one. The pairing QR carries the
  whole list, so either side can fail over without a re-scan. The phone lists
  every saved candidate, in priority order, under the relay in **Settings**.
- `HERDR_GATEWAY_REGISTRATION_TOKEN` — optional self-hosted gateway registration
  secret. Set the same value on the gateway and relay; it is never sent on the
  wire or included in pairing links. Empty preserves open registration.
- `HERDR_GATEWAY_SELECTION` — `ordered`, the default, takes the first healthy
  entry in configured order, which makes a list you write yourself a priority
  list. `latency` keeps the lowest-latency healthy entry, with configured order
  breaking ties within 20 ms. You do not have to set this by hand: the setup
  menu asks whenever a list has more than one entry, defaulting to `ordered`
  for a list built around your own gateway and `latency` for the community
  list, whose gateways are interchangeable. The menu's status line names the
  rule in force, and setup prints each candidate's measured round trip so the
  order is an informed choice.
- `HERDR_WEBRTC_UDP_PORT` — fixed UDP port for the direct path; `0` (default) uses
  an ephemeral port.
- `HERDR_REACHABILITY_PORT_MAPPING` — ask the router for a PCP, NAT-PMP, or UPnP
  mapping to raise direct-path success; `1` by default, `0` never talks to the
  router.
- `HERDR_TRANSPORT_FORCE_RELAY` — `1` disables the direct upgrade and keeps every
  frame on the gateway path.

## Troubleshooting

- **Gateway never registers:** check `HERDR_GATEWAY_URL` and outbound HTTPS
  access; `GET /healthz` reports `gateway.registered`.
- **Registration rejected as `unauthorized`:** set the gateway's matching
  `HERDR_GATEWAY_REGISTRATION_TOKEN` on the relay and check that the relay's full
  id is in the gateway's `HERDR_GATEWAY_ALLOWED_RELAY_IDS`, if configured. The
  relay retries with exponential backoff; restart it after changing its env.

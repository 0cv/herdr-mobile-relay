# How your phone reaches your computer

Three transports can carry traffic between your phone and a relay: a Cloudflare
tunnel, the community gateway, or a gateway you run yourself. All three are
end-to-end encrypted. Only the two gateway choices then try to leave the
transport behind: phone and computer negotiate a direct peer-to-peer connection
and the gateway is left carrying the fallback. Cloudflare tunnel traffic always
goes through Cloudflare.

## The three choices

| Choice | What it needs from you | Who carries the traffic | When to pick it |
| --- | --- | --- | --- |
| **Cloudflare tunnel** | Nothing for Quick Start's temporary URL; a Cloudflare account with a domain for a permanent hostname and background service. | Cloudflare's edge | The default. See [cloudflare-tunnel.md](cloudflare-tunnel.md) for the permanent hostname. |
| **Community gateway** | No account and no domain, but the phone app must already be hosted somewhere — a gateway serves no app. | A gateway operated by the project, until the direct path forms | Free, shared, best-effort; not for heavy transfers. Pick it to avoid Cloudflare setup entirely. |
| **Your own gateway** | A small VPS with Docker and a public hostname. | Your own gateway, until the direct path forms | Dedicated bandwidth, and the transport logs stay on your machine. See [gateway-self-hosting.md](gateway-self-hosting.md). |

Pick **Temporary Cloudflare Tunnel**, **Community WebRTC Gateway**, **Deploy or
Upgrade Your Own WebRTC Gateway**, or **Stable Tunnel** directly from the setup
menu. A completed choice is recorded, starts or restarts the relay, and prints
the phone QR; there is no second Quick Start step.

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
`relay URL <host>` on a Cloudflare tunnel or LAN address.

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

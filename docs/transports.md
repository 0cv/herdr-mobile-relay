# How your phone reaches your computer

Three transports can carry traffic between your phone and a relay: a Cloudflare
tunnel, the community gateway, or a gateway you run yourself. Whichever you pick,
traffic stays end-to-end encrypted, and the phone upgrades to a direct
peer-to-peer connection when the network allows — so the transport you choose only
decides who carries the fallback.

## The three choices

| Choice | What it needs from you | Who carries the traffic | When to pick it |
| --- | --- | --- | --- |
| **Cloudflare tunnel** | Nothing for Quick Start's temporary URL; a Cloudflare account with a domain for a permanent hostname and background service. | Cloudflare's edge | The default. See [cloudflare-tunnel.md](cloudflare-tunnel.md) for the permanent hostname. |
| **Community gateway** | Nothing — no account, no domain. | A gateway operated by the project | Free, shared, best-effort; not for heavy transfers. Pick it to avoid Cloudflare setup entirely. |
| **Your own gateway** | A small VPS with Docker and a public hostname. | Your own gateway | Dedicated bandwidth, and the transport logs stay on your machine. See [gateway-self-hosting.md](gateway-self-hosting.md). |

Pick **Temporary Cloudflare Tunnel**, **Community WebRTC Gateway**, **Your Own
WebRTC Gateway**, or **Stable Tunnel** directly from the setup menu. Each action
records its choice, starts or restarts the relay when appropriate, and prints
the phone QR; there is no second Quick Start step.

## The gateway path

With `HERDR_GATEWAY_URL` set, Quick Start skips `cloudflared` entirely — no
Cloudflare account, domain, or tunnel:

```bash
HERDR_GATEWAY_URL=wss://gw.example.com make quick-start
```

The QR is printed as soon as the gateway confirms the registration.

A gateway holds no secrets and never learns the relay key: the relay registers
under an id derived from that key, the phone answers a challenge that the *relay*
verifies, and the gateway only copies frames that are already encrypted. It is a
single static binary you can self-host, and the setup menu can deploy one to your
own server over SSH.

## The direct upgrade

Once a phone is connected through the gateway, both sides negotiate a direct
WebRTC DataChannel (`herdr-dc-v1`) inside the existing encrypted channel and move
the same frames onto it. If the direct path never forms, or later breaks, traffic
stays on — or returns to — the gateway without reconnecting the session.

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
  Cloudflare tunnel path. The relay probes every candidate over HTTPS at startup,
  keeps exactly one registration, and after a failure excludes that entry for the
  pass and takes the next healthy one. The pairing QR carries the whole list, so
  either side can fail over without a re-scan.
  The phone lists every saved candidate, in priority order, under the relay in
  **Settings**.
- `HERDR_GATEWAY_SELECTION` — `ordered`, the default, takes the first healthy
  entry in configured order, which makes a list you write yourself a priority
  list. `latency` keeps the lowest-latency healthy entry, with configured order
  breaking ties within 20 ms; the setup menu writes it only for the community
  list, whose gateways are interchangeable.
- `HERDR_WEBRTC_UDP_PORT` — fixed UDP port for the direct path; `0` (default) uses
  an ephemeral port.
- `HERDR_REACHABILITY_PORT_MAPPING` — ask the router for a UPnP/NAT-PMP mapping to
  raise direct-path success; `1` by default, `0` never talks to the router.
- `HERDR_TRANSPORT_FORCE_RELAY` — `1` disables the direct upgrade and keeps every
  frame on the gateway path.

## Troubleshooting

- **Gateway never registers:** check `HERDR_GATEWAY_URL` and outbound HTTPS
  access; `GET /healthz` reports `gateway.registered`.

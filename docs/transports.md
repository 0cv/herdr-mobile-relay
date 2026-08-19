# How your phone reaches your computer

The transport choices behind the relay: a Cloudflare tunnel, the community
gateway, or a gateway you run yourself. Read this to pick one, and to understand
the direct WebRTC upgrade that takes all three out of the path when the network
allows it.

Whichever you pick, traffic stays end-to-end encrypted and the phone upgrades to
a direct connection whenever the network allows it. The choice only decides who
carries the fallback when it cannot.

## The three choices

| Choice | What it needs from you | Who carries the traffic | When to pick it |
| --- | --- | --- | --- |
| **Cloudflare tunnel** | Nothing for the temporary Quick Start URL. A Cloudflare account with a domain for a permanent hostname and background service. | Cloudflare's edge | The default, and the recommended start. Quick Start opens a temporary URL with no Cloudflare account and no domain; see [docs/cloudflare-tunnel.md](cloudflare-tunnel.md) for the permanent hostname. |
| **Community gateway** | Nothing — no account and no domain. | A gateway operated by the project | Free, shared, best-effort capacity: fine for normal use, not for heavy transfers. Pick it when you want no Cloudflare setup at all. |
| **Your own gateway** | One small VPS with Docker and a public hostname that resolves to it. | Your own gateway | Best performance and privacy: you own the box, the bandwidth, and the logs. See [docs/gateway-self-hosting.md](gateway-self-hosting.md). |

Pick from the setup menu under **Choose Connection Method**, which writes or
clears `HERDR_GATEWAY_URL` for you. The community gateway appears there only
when the build has one published; otherwise choose a Cloudflare tunnel or your
own gateway.

## The gateway path

Cloudflare is no longer the only way to reach a relay. Set `HERDR_GATEWAY_URL`
in the relay environment and Quick Start skips `cloudflared` entirely — no
Cloudflare account, domain, or tunnel:

```bash
HERDR_GATEWAY_URL=wss://gw.example.com make quick-start
```

The relay dials that gateway itself, and the QR is printed as soon as the
gateway confirms the registration.

The gateway is blind by construction. It holds no secrets and never learns the
relay key: the relay registers under an id derived from the key with
HKDF-SHA-256, the phone answers a challenge with an HMAC the *relay* verifies,
and the gateway only copies already-encrypted frames between the two. It cannot
read commands, terminal output, uploads, or push details, and it is one static
binary you can self-host — see
[docs/gateway-self-hosting.md](gateway-self-hosting.md) and `make gateway`.

The plugin can deploy that gateway for you. **Choose Connection Method → Your
own gateway → Deploy one on my own server over SSH** asks for the public
hostname and the server's SSH address, copies a compose bundle carrying the
gateway source, builds and starts it there, waits for the certificate, and
records the verified `wss://` URL only after `/healthz` answers. The server needs
nothing but Docker, and the relay key never leaves your computer.

## The direct WebRTC upgrade

Once a phone is connected through the gateway, both sides try to remove it from
the path. Over the existing encrypted channel they exchange a WebRTC offer,
answer, and ICE candidates, and on success the reliable ordered DataChannel
`herdr-dc-v1` carries the same encrypted frames directly between phone and
computer. If the direct path never forms, or later breaks, traffic stays on (or
returns to) the gateway without reconnecting the session.

The gateway also answers address discovery on UDP 3478, so both the phone and
the computer learn the address the internet sees them at and can offer it as an
ICE candidate. That is what lets a phone on a cellular network reach a home
computer directly, with no port forwarding and no router configuration; no
third-party service is involved, since the gateway only reflects a source
address it already observes. On a self-hosted gateway, inbound UDP 3478 must be
open — it is published on the host directly, because a TLS reverse proxy cannot
carry raw UDP.

Address discovery is the piece that lets two ordinary NATs meet. Neither side
can see its own public address from behind a NAT, so neither could name an
endpoint the other is able to dial; each asks the gateway what address it
arrives from, offers that as an ICE candidate, and the two agents then probe the
candidate pairs directly. Nothing is forwarded on either router — the mapping
each NAT already created for the outbound probe is what the peer uses.

## Relay environment settings

- `HERDR_GATEWAY_URL` — one or more gateway base URLs, separated by commas
  (`wss://gw.example.com,wss://backup.example.com`). Empty (the default) keeps
  the Cloudflare tunnel path. At startup the relay probes every candidate
  concurrently over HTTPS and keeps exactly one registration with the
  lowest-latency healthy gateway. After a failure it excludes that gateway and
  repeats the choice among the remaining fallbacks; configured order breaks
  ties within 20 ms and remains the fallback if every probe fails. The
  relay advertises the selected gateway first, so the phone saves the same
  active-first list. The pairing QR also carries the whole configured list, so
  either side can fail over without a re-scan.
  The setup chooser validates every managed candidate and requires at least one
  to answer, while retaining unavailable entries as cold fallbacks.
- `HERDR_WEBRTC_UDP_PORT` — fixed UDP port for the direct path; `0` (default)
  uses an ephemeral port.
- `HERDR_REACHABILITY_PORT_MAPPING` — ask the router for a UPnP/NAT-PMP mapping
  to raise direct-path success; `1` by default, `0` never talks to the router.
- `HERDR_TRANSPORT_FORCE_RELAY` — test flag; `1` disables the direct upgrade and
  keeps every frame on the gateway path.

## Security posture of the direct path

This changes the security posture of the computer: the direct path opens a UDP
socket, where the tunnel was strictly outbound. Reaching that socket is not
enough to talk to the relay — ICE requires the session `ufrag`/`pwd`, which are
delivered only inside the already-authenticated E2EE channel, and the DTLS
certificate is pinned by the fingerprint in the exchanged SDP. Unsolicited
packets are dropped by the ICE agent, and the Herdr E2EE handshake remains the
only authorization for control on every path. Set
`HERDR_REACHABILITY_PORT_MAPPING=0` to keep the relay from requesting a router
mapping, or `HERDR_TRANSPORT_FORCE_RELAY=1` to stay on the gateway.

## Troubleshooting

- **Gateway never registers:** check `HERDR_GATEWAY_URL` and outbound HTTPS
  access; `GET /healthz` reports `gateway.registered`.

# How your phone reaches your computer

Five transport choices can carry traffic between your phone and relay: a
Cloudflare tunnel, the community gateway, a gateway you run yourself, managed
Tailscale Serve, or operator-owned Tailscale HTTPS Serve. All are end-to-end encrypted.
Only the two gateway choices
then try to leave the transport behind: phone and computer negotiate a direct
peer-to-peer connection and the gateway is left carrying the fallback. Cloudflare
and Tailscale traffic use their respective trusted HTTPS paths.

## The five choices

| Choice | What it needs from you | Who carries the traffic | When to pick it |
| --- | --- | --- | --- |
| **Cloudflare tunnel** | Nothing for Quick Start's temporary URL; a Cloudflare account with a domain for a permanent hostname and background service. | Cloudflare's edge | The default. See [cloudflare-tunnel.md](cloudflare-tunnel.md) for the permanent hostname. |
| **Community gateway** | No account and no domain, but the phone app must already be hosted somewhere — a gateway serves no app. | A gateway operated by the project, until the direct path forms | Free, shared, best-effort; not for heavy transfers. Pick it to avoid Cloudflare setup entirely. |
| **Your own gateway** | A small VPS with Docker and a public hostname. | Your own gateway, until the direct path forms | Dedicated bandwidth, and the transport logs stay on your machine. See [gateway-self-hosting.md](gateway-self-hosting.md). |
| **Managed Tailscale Serve (foreground)** | A supported preinstalled, running, authenticated Tailscale node. | Your tailnet's Tailscale HTTPS path; Herdr manages its own temporary Serve route. | Private tailnet access without a public gateway; the setup pane must stay open. |
| **Operator-owned Tailscale HTTPS Serve (BYO, foreground)** | An existing canonical HTTPS origin that you have routed to this relay's loopback listener. | Your independently configured ingress; Herdr does not inspect or change it. | Keep an existing operator-owned HTTPS route, and retain it after Herdr stops. |

Pick **Temporary Cloudflare Tunnel**, **Community WebRTC Gateway**, **Deploy or
Upgrade Your Own WebRTC Gateway**, **Managed Tailscale Serve (foreground)**,
**Operator-owned Tailscale HTTPS Serve (BYO, foreground)**, or **Stable Tunnel**
directly from the setup menu (`t` for managed, `b` for BYO). A completed choice
starts or restarts the relay and verifies the phone app before printing a QR.

## Managed Tailscale Serve

Choose **Managed Tailscale Serve (foreground)**, or press `t` in the setup menu, when this
computer already has a supported Tailscale Unix daemon installed, running, and
authenticated. The relay binds only to `127.0.0.1`; the launcher performs a
read-only identity/version/Serve/Funnel preflight, asks for per-run consent, then
the relay process owns its LocalAPI watcher and conditionally manages the exact
HTTPS route. It never runs `tailscale login`, invokes `tailscale serve`, enables
Funnel, resets devices, adopts an existing route, or overwrites existing Serve
configuration. Existing Serve or Funnel configuration is a refusal and is left
untouched.

This is a source-supported, not real-daemon-qualified, profile pinned to
Tailscale v1.102.4: Linux and the open-source Darwin Unix-daemon socket. MacSys
GUI, App Store, and other GUI/build variants are unsupported or unqualified; the
adapter refuses unrecognized version metadata rather than guessing which daemon
the CLI addresses. A running daemon and tailnet HTTPS reachability are operator
prerequisites. No physical phone or real-daemon qualification is implied.

This mode is deliberately foreground-only. The setup pane owns the relay,
LocalAPI watch/session, conditional route, and private pairing-control socket;
keep it open while using the phone. Ctrl-C requests authenticated retirement;
the route, owner, and backend are not released unless route clearing and local
watch closure are separately acknowledged. If cleanup is unresolved, the
foreground owner/control and recovery evidence are retained for inspection.
Background service installation and phone-managed relay updates are refused
while Tailscale Serve is selected, so stop the pane and remove the foreground
selection before using the Cloudflare service/update path.

The printed link uses the verified Tailscale HTTPS origin and its packaged
Herdr phone frontend by default. A reused or explicitly selected app origin is
checked with normal TLS hostname verification and must serve this release's
complete web bundle before an invitation is armed. If this relay already has a
shared app origin, the setup flow keeps it; choose the Tailscale origin or
another installed Herdr app to switch explicitly. `HERDR_TAILSCALE_HTTPS_PORT`
can select a free HTTPS Serve port (the default is `443`);
`HERDR_TAILSCALE_BIN` can point at a non-default CLI; the launcher and Go-side
read-only preflight use that same selected executable. Public HTTPS checks use
the operating system's normal
certificate and hostname verification; the launcher has no certificate-bypass
or custom-CA option.

Managed Tailscale Serve therefore needs no separately hosted app for a new
configuration. Tailscale must be installed and authenticated manually; the relay
does not mutate Tailscale account state.

## Operator-owned HTTPS Serve (BYO)

Choose this distinct foreground transport (menu key `b`) only after you have
independently configured an HTTPS Serve origin to reach Herdr's local listener
on `127.0.0.1:8375` by default (or your configured loopback port). Enter a canonical
`https://host[:port]` origin with no path,
query, fragment, or credentials. Herdr stores this as the distinct
`HERDR_RELAY_TRANSPORT=tailscale-external` selection and
`HERDR_EXTERNAL_HTTPS_ORIGIN`; the `tailscale` value remains the separate
managed-Serve mode. Herdr listens only on loopback, checks the origin using
normal system TLS trust and hostname verification, and requires
that `/healthz` identify this exact relay instance, control run, transport, and
release. The operator-owned Serve origin carries relay health and WSS; it need
not host the phone frontend. Herdr keeps your saved phone-app origin or lets
you choose this Serve origin or another installed Herdr app. The selected app
origin must pass trusted TLS and serve this exact release's complete bundle
(set `HERDR_PHONE_APP_URL` for an unattended explicit choice). Do not use a
relay origin that reaches some other instance or an untrusted certificate.

This BYO path never runs `tailscale`, opens Tailscale LocalAPI, or reads Serve,
Funnel, account, or device state. It neither creates nor checks nor clears an
ingress route. You own HTTPS Serve configuration, access policy, route health,
and cleanup. Herdr prints the route as operator-owned and on Ctrl-C stops only
its own loopback backend and private pairing control. The transport also
disables automatic PCP/UPnP router mapping. Your ingress remains as configured,
but is no longer backed by this stopped relay. If the backend does not exit after
ten seconds, Herdr sends SIGKILL. If the backend is SIGKILLed by Herdr or
otherwise, Herdr retains the private session record, child PID (for inspection
only), and relay log, and leaves any control socket pathname untouched; a later
start refuses while that evidence remains. PIDs may be reused: inspect
the process identity and confirm no relay process is live before manually
removing stale recovery files, and never remove a socket that may belong to a
live process. A foreground pane is required; background service installation
and phone-managed relay updates are refused. MacSys GUI, App Store, and other
unqualified daemon profiles are not
enabled by the managed adapter; BYO does not probe a daemon profile at all.

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

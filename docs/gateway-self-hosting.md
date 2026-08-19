# Self-hosting the Herdr gateway

The gateway (`cmd/herdr-gateway`) is the fallback transport between a phone and
a computer relay. It replaces the per-user Cloudflare tunnel: the relay dials
**out** to the gateway over WSS, the phone dials **in**, and the gateway copies
opaque frames between them. When the phone and the computer manage a direct
WebRTC DataChannel, the gateway is used only for rendezvous and signaling.

You do not have to trust the gateway operator, and you do not have to use the
project's gateway. One small VPS with flat egress runs your own.

## Fast path: deploy from the plugin menu

`herdr plugin install 0cv/herdr-mobile-relay` already ships the deployer, so
nothing is downloaded and no installer is piped into a shell. Open the setup
menu and choose **Choose Connection Method → Your own gateway → Deploy one on my
own server over SSH**. It asks for two things:

- the **public hostname** phones dial, which must resolve to the server;
- the **server's SSH address**, such as `root@203.0.113.10`.

It then writes the bundle (`docker-compose.yml`, Caddy TLS configuration,
`.env`, and the gateway source), copies it to the server over one authenticated
SSH connection, installs Docker there when you allow it, runs `docker compose up
-d --build`, waits for `127.0.0.1:8443/healthz` on the server and for the first
certificate on the public name, and only then records the verified `wss://` URL
as `HERDR_GATEWAY_URL`. Leave the SSH address empty to write the bundle without
deploying it, or run the wizard directly:

```sh
bash relay/gateway-deploy.sh
```

The server needs nothing but Docker: the bundle carries `gateway-source/` — the
gateway command and the two packages it imports — and compose builds the image
there, so no Go toolchain, registry credential, or GitHub fetch is involved. The
relay key never leaves your computer.

Non-interactive equivalents: `HERDR_GATEWAY_DEPLOY_HOST`,
`HERDR_GATEWAY_DEPLOY_SERVER`, `HERDR_GATEWAY_DEPLOY_REMOTE_DIR` (default
`/opt/herdr-gateway`), `HERDR_GATEWAY_DEPLOY_EMAIL`,
`HERDR_GATEWAY_DEPLOY_DIR`, and `HERDR_GATEWAY_DEPLOY_INSTALL_DOCKER=true`.
The SSH account must be root or have passwordless sudo.

The project-operated community gateway is the free, shared, best-effort
choice when it is available. Self-hosting is the choice when you want
dedicated bandwidth and control of the gateway's transport logs; the gateway
still cannot read application plaintext because E2EE terminates at the phone
and relay.

## The hostname phones dial

The gateway needs one public name that resolves to the server, and it can live at
any registrar or DNS provider — the domain does not have to be hosted where the
server is. In Cloudflare's DNS tab, an `A` record `gw` pointing at the server's
IPv4 (plus `AAAA` for IPv6) is all it takes. Leave the nameservers alone.

Set that record to **DNS only** (grey cloud), not proxied. A proxied record puts
Cloudflare's edge back in the path the gateway exists to shorten: it terminates
TLS itself, so phones would trust Cloudflare's certificate instead of the one
Caddy owns, TLS-ALPN validation cannot reach the server, and every relayed frame
would traverse a third network with its own idle timeouts.

Confirm the record resolves to the server, not to a proxy address, before
deploying:

```sh
dig +short gw.example.com
```

A hostname your provider generated for the server also works, as long as it
forward-resolves to it — reverse DNS alone does not. Prefer a name in a domain
you own: Let's Encrypt's limit of 50 certificates per registered domain per week
is shared by everyone using a provider's zone, and its 5-per-identical-name limit
is easy to exhaust while testing.

## What the gateway can and cannot see

The gateway holds **no secrets at all**. It never learns the relay key, the
pairing token, or the E2EE session keys.

It can see:

- the two derived rendezvous identifiers on the wire: `relay_id` (22 characters,
  derived one-way from the relay key) and, per connection, the 32-byte challenge
  it issued plus the phone's HMAC answer;
- source IP addresses, connection times, and byte counts, plus the source port
  of an address-discovery request, which is the address it reflects back;
- frame sizes and timing.

It cannot see:

- anything inside a frame. Every frame is AES-256-GCM ciphertext from the
  existing Herdr end-to-end session, which is established *through* the gateway
  and terminates on the phone and the relay;
- WebRTC SDP or ICE candidates: signaling travels inside the encrypted channel,
  so the gateway never learns either side's local addresses, and it is never
  told which candidate pair the two of them chose;
- terminal output, prompts, uploads, push subscriptions, or the relay key.

It never derives, stores, or verifies a secret:

- `relay_id = base64url(HKDF-SHA256(relay_key, "herdr-gateway-v1", "herdr-gw-id", 16))`
- `rendezvous_key = HKDF-SHA256(relay_key, "herdr-gateway-v1", "herdr-gw-auth", 32)`

Only the relay and the paired phone can compute `rendezvous_key`, so **the relay
authenticates the phone, not the gateway**. The gateway issues a random 32-byte
challenge, forwards the phone's answer to the relay inside the `OPEN` frame, and
the relay verifies the HMAC before the E2EE handshake starts. A gateway that
lies about a challenge, or an attacker who steals a `relay_id`, still cannot get
past the relay.

What is logged: transport events only, at `INFO`. Relay ids appear truncated to
their first six characters. Frame bytes, nonces, proofs, and close payload
contents are never logged.

The only thing persisted, and only when you ask for it, is a per-relay
relayed-byte counter (see `HERDR_GATEWAY_STATE`).

## Deploying

One small flat-egress VPS is the design target. Expect roughly 0.5–1 GB of
egress per month per *relayed* active user; users on a direct WebRTC path cost
almost nothing beyond rendezvous. At 1,000 active users with a fifth of them
relayed that is only 75–150 GB/month, so on any host with a flat or generous
egress allowance **bandwidth is not the binding constraint** — concurrent
connections are. Each paired phone costs a goroutine pair and a bounded 4 MiB
outbound queue, so size for peak simultaneous phones and file descriptors, not
for transfer.

What to actually shop for, in priority order: at least 1 TB of included egress
(150 GB/month leaves generous headroom), 1 vCPU and 1–2 GB RAM, IPv6, and a
provider that tolerates outbound UDP. That specification is met by the entry
plan of essentially every reputable VPS host, so pick on network quality and
region rather than on price.

Two reference deployments, both fine, priced August 2026 — verify before you
buy, because both providers moved prices in 2026:

- **OVHcloud d2-2** (1 vCore / 2 GB / 25 GB NVMe), €5.71/month ex-VAT. Instance
  traffic is not metered, but public bandwidth is capped at 100 Mbit/s and is
  explicitly not guaranteed, so it is shared best-effort.
- **Hetzner CPX12** (1 vCPU AMD / 2 GB / 40 GB), €11.99/month in Falkenstein,
  Nuremberg, or Helsinki, with 20 TB of included egress billed on outgoing
  traffic only. US is €17.99 and Singapore €15.99; the older Cost-Optimized
  CX/CAX line is currently unavailable.

Where traffic is unmetered, read the throughput cap instead of the allowance —
and remember a relay spends it twice, once receiving and once sending. At the
expected 150 GB/month the average rate is under 0.5 Mbit/s, so a 100 Mbit/s cap
is ample; it becomes the limit only when many phones upload images at once,
which is exactly the traffic the direct WebRTC path removes from the gateway.

On a metered host, check two things the headline allowance hides. First, some
providers count inbound as well as outbound against the allowance — AWS
Lightsail does — and a gateway is a pure relay, so every byte is counted twice
and the effective allowance is half the advertised figure. Second, compare the
*overage* rate, not the included amount: Lightsail bills $0.09/GB beyond the
bundle (about $92/TB) against roughly €1/TB on a flat-egress host. Both are
fine at the expected 150 GB/month, but they behave very differently under abuse
or a traffic bug, and AWS has no hard spend cap. If you deploy somewhere
metered, set a provider budget alarm and lower
`HERDR_GATEWAY_MONTHLY_BYTES` so this gateway's own quota refuses new relayed
connections before the bill grows.

Latency, not capacity, is the reason to add a second instance. The relayed path
targets a 250–400 ms resume, so a phone and computer both far from the gateway
notice. Scaling out needs no shared state, anycast, or session affinity: the
gateway URL is per-relay configuration and the phone learns it from the QR or
the hybrid descriptor, so `gw-eu`, `gw-us`, and `gw-ap` can be entirely
independent single-instance deployments.

Two operational notes. `/probe` sends a UDP datagram to the requesting client's
own address, which some providers' abuse tooling treats as scanning; it is rate
limited to one request per 10 s per IP, never targets an address the requester
did not connect from, and returns fewer bytes than it receives, so it cannot be
used for reflection or amplification. And the gateway needs outbound UDP for
that endpoint, inbound TCP 443 for everything else, and inbound UDP 3478 for
address discovery.

### Docker

```sh
docker build -f Dockerfile.gateway -t herdr-gateway .
docker run -d --name herdr-gateway \
  -p 127.0.0.1:8443:8443 \
  -p 3478:3478/udp \
  -e HERDR_GATEWAY_MONTHLY_BYTES=0 \
  -e HERDR_GATEWAY_LOG_FORMAT=json \
  herdr-gateway
```

The image is `gcr.io/distroless/static:nonroot` with a single static binary and
no shell. To enforce quotas that survive restarts, drop
`HERDR_GATEWAY_MONTHLY_BYTES=0`, add `-v /var/lib/herdr-gateway:/state -e
HERDR_GATEWAY_STATE=/state/counters.json`, and make that directory writable by
uid 65532 (`nonroot`).

`3478/udp` is published on every interface on purpose: address discovery is raw
UDP and no TLS reverse proxy can carry it, so unlike 8443 it has to be reachable
from the internet directly. Set `HERDR_GATEWAY_STUN_ADDR=` (empty) to turn the
listener off and drop the port.

### Plain binary

```sh
go build -trimpath -o /usr/local/bin/herdr-gateway ./cmd/herdr-gateway
```

```ini
# /etc/systemd/system/herdr-gateway.service
[Unit]
Description=Herdr blind WSS gateway
After=network-online.target

[Service]
ExecStart=/usr/local/bin/herdr-gateway
Environment=HERDR_GATEWAY_ADDR=127.0.0.1:8443
Environment=HERDR_GATEWAY_LOG_FORMAT=json
Environment=HERDR_GATEWAY_STATE=/var/lib/herdr-gateway/counters.json
Environment=HERDR_GATEWAY_TRUSTED_PROXY=true
DynamicUser=yes
StateDirectory=herdr-gateway
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

`SIGINT` and `SIGTERM` drop the relay links, flush the counters, and drain
in-flight HTTP requests for up to 10 seconds.

## TLS termination

The gateway speaks plain HTTP and does **not** terminate TLS. Phones require
`wss://`, so put a reverse proxy in front of it and let the proxy own the
certificate. Bind the gateway to loopback so it is never reachable directly.

Caddy needs no WebSocket-specific configuration:

```caddyfile
gw.example.com {
	reverse_proxy 127.0.0.1:8443
}
```

nginx does:

```nginx
server {
    listen 443 ssl http2;
    server_name gw.example.com;

    ssl_certificate     /etc/letsencrypt/live/gw.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/gw.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_buffering off;
        # The gateway pings relays every 30 s; anything above 120 s is safe.
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

Set `HERDR_GATEWAY_TRUSTED_PROXY=true` whenever a proxy is in front, so the
per-IP connect limit and `/probe` see the real client address instead of the
proxy's. Leave it `false` when the gateway is exposed directly: otherwise any
client can forge `X-Forwarded-For` and bypass the rate limit.

Do not enable HTTP compression on the proxy for these routes. Payloads are
ciphertext, so compression buys nothing and leaks length information.

## Configuration

Everything is read from the environment at startup; there are no flags.

| Variable | Default | Meaning |
| --- | --- | --- |
| `HERDR_GATEWAY_ADDR` | `:8443` | Listen address. Use `127.0.0.1:8443` behind a proxy. |
| `HERDR_GATEWAY_STUN_ADDR` | `:3478` | UDP address-discovery listener. Must be reachable from the internet directly; a proxy cannot carry it. Empty disables it. |
| `HERDR_GATEWAY_MAX_CLIENTS_PER_RELAY` | `8` | Concurrent phone connections per relay. Negative removes the cap. Refusals return `too_many_clients`. |
| `HERDR_GATEWAY_MAX_RELAYS` | `1024` | Registered relays this gateway will hold. Negative removes the cap. Refusals return `at_capacity`. |
| `HERDR_GATEWAY_MAX_CLIENTS` | `512` | Phone connections across every relay. Negative removes the cap. Refusals return `at_capacity`. |
| `HERDR_GATEWAY_CONNECT_RATE_PER_MINUTE` | `30` | Phone connection attempts per client IP per minute; relay registrations are counted separately against the same number. Negative removes the limit. Refusals return `rate_limited`. |
| `HERDR_GATEWAY_MONTHLY_BYTES` | `5368709120` (5 GiB) | Bytes copied in both directions, per relay, per UTC calendar month. `0` means unlimited. |
| `HERDR_GATEWAY_QUOTA_WARN_PERCENT` | `80` | Percentage of the quota at which the relay receives one advisory warning. Negative disables the warning. |
| `HERDR_GATEWAY_IDLE_TIMEOUT` | `300` | Seconds a phone connection may carry no traffic before it is closed. Negative disables idle reaping. Relay links use ping/pong instead. |
| `HERDR_GATEWAY_STATE` | unset | Path to a JSON file holding relayed-byte counters. Unset keeps counters in memory only. |
| `HERDR_GATEWAY_TRUSTED_PROXY` | `false` | Believe the leftmost `X-Forwarded-For` entry. Enable only behind a proxy you control. |
| `HERDR_GATEWAY_LOG_FORMAT` | `text` | `text` or `json` structured logs on stderr. |

Fixed, not configurable: protocol version 1, a 10-second hello deadline, the
per-frame ciphertext cap of the wire protocol, a 30-second relay ping interval
(a relay is dropped after two consecutive missed pongs), and one `/probe` per
10 seconds per IP.

### Ceilings on a shared instance

Every other limit above is per-relay or per-IP, which is enough for a gateway
that serves you alone and not enough for one anybody may register with. The two
whole-gateway ceilings, `HERDR_GATEWAY_MAX_RELAYS` and
`HERDR_GATEWAY_MAX_CLIENTS`, bound the rest: without them a stranger holding many
addresses and many self-generated relay ids grows goroutines and memory until the
process dies, no matter how generous the bandwidth is.

The client ceiling is a memory bound rather than a bandwidth one. Each connection
may queue up to 4 MiB before it is dropped as too slow, so the default 512 caps
the worst case near 2 GiB while a real population sits close to empty. Raise it
with the RAM you actually have, and remember that a session which upgrades to the
direct path stops consuming a slot within seconds.

There is deliberately **no per-IP concurrency cap**. Carrier NAT puts thousands
of unrelated phones behind a single address, so a per-IP concurrency limit refuses
legitimate users while barely inconveniencing an attacker who has more addresses.
The per-IP *rate* limit stays, because a connection storm from one address is
still worth slowing down.

Address discovery has its own ceilings: 20 datagrams per 5 seconds per source
address over a fixed table, plus a global 2000 per second, and a response can
never exceed roughly twice its request. A forged source address can therefore
waste a little of the gateway's CPU and nothing of a victim's bandwidth.

If `HERDR_GATEWAY_STATE` is set, the file is loaded at startup, rewritten
atomically every 30 seconds and on shutdown, and created with mode `0600`. It
contains counters and nothing else. Counters from previous months are dropped on
load, so a gateway that was offline across a month boundary starts the month
clean.

### Address discovery

The gateway answers address discovery on UDP `HERDR_GATEWAY_STUN_ADDR` so both
the phone and the relay learn the address the internet sees them at. Neither
peer can know that address on its own: NAT rewrites the source port on the way
out, so a candidate list gathered locally names addresses that are useless to
the other side. With the reflected address in hand both sides offer a reflexive
ICE candidate, and the direct DataChannel forms across two ordinary NATs without
anyone touching a router.

Three properties keep this from widening the gateway's blast radius:

- **Only the port is advertised.** The gateway's hello carries `stun_port` and
  nothing else. Each peer builds the address from the gateway host it already
  dialed — the relay from `HERDR_GATEWAY_URL`, the phone from its stored gateway
  URL — and refuses a host from the gateway. A compromised or hostile gateway
  therefore cannot redirect address discovery at a third party; the worst it can
  do is point at one of its own ports, which is where the traffic already goes.
- **Reflection reveals nothing new.** The answer is the source address of the
  packet the gateway just received. It already observes that address on the WSS
  connection, so a peer that asks tells the gateway nothing it did not have.
  Requests are unauthenticated, stateless, and answered with fewer bytes than
  they carry, so the listener cannot be used for amplification.
- **No third-party service is involved.** Address discovery runs on the gateway
  you already trust with rendezvous. There is no external STUN provider to add
  to the trust boundary, and no traffic is ever relayed on the peers' behalf by
  anything other than this gateway's own WSS path.

UPnP/NAT-PMP port mapping on the relay (`HERDR_REACHABILITY_PORT_MAPPING`) still
helps — a mapped port gives a stable, router-blessed candidate — but it is no
longer required for a direct path off the LAN, and it never worked for the phone
at all.

What still forces a relayed connection, honestly:

- **UDP blocked outright** on either side. A network that drops UDP leaves
  nothing for ICE to use, and every frame stays on the gateway.
- **Hard NAT on both ends.** Symmetric NAT picks a fresh external port per
  destination, so the address the gateway reflects is not the address the peer
  will see. One hard NAT plus one ordinary NAT usually still connects; two hard
  NATs do not, and the session runs relayed.

Disabling the listener (`HERDR_GATEWAY_STUN_ADDR=`) is a supported choice: the
relay then falls back to host candidates plus whatever UPnP/NAT-PMP obtained,
which is what a LAN-only deployment needs, and everything else relays.

## Quota tuning

The quota exists to bound a public gateway's bandwidth bill, not to police
users. It behaves like this:

1. At `HERDR_GATEWAY_QUOTA_WARN_PERCENT` of the monthly limit the relay receives
   exactly one advisory notice, which the relay surfaces in its UI.
2. Once the limit is reached the relay receives one `quota_exceeded` notice and
   **new** phone connections are refused with the `quota_exceeded` code.
   Connections that are already established are never severed mid-session.
3. Counters reset on the UTC month boundary.

Guidance:

- Self-hosting for yourself or a household: set `HERDR_GATEWAY_MONTHLY_BYTES=0`.
  You are paying for your own traffic and the limit only gets in your way.
- Hosting for a group: divide your VPS's monthly egress allowance by the number
  of relays you expect and halve it, since every relayed byte is counted once on
  the way in and once on the way out. On a 1 TB plan with 20 relays, 25 GiB
  (`26843545600`) per relay is comfortable.
- A relay that keeps hitting the quota is a relay whose direct WebRTC path is
  failing. Fixing reachability on that computer (UPnP/NAT-PMP, IPv6, or a
  forwarded UDP port) removes the traffic from the gateway entirely; the quota
  warning says as much.

## Pointing a relay at your gateway

On the computer relay:

```sh
HERDR_GATEWAY_URL=wss://gw.example.com
```

A base URL with no trailing slash and no path. The relay appends the routes
itself. The paired phone learns the same base from the QR payload, so a relay
and its phones must use the same gateway.

The relay key is unchanged by the move, and so is the QR payload: both
identifiers are derived from the key you already have.

## Routes and ports

Everything below is served on `HERDR_GATEWAY_ADDR` (TCP, HTTP, normally behind a
TLS proxy) except address discovery, which listens on
`HERDR_GATEWAY_STUN_ADDR` (UDP 3478 by default) and must be published directly
on the host.

| Route | Purpose |
| --- | --- |
| `GET /relay` | WebSocket. One multiplexed registration per computer relay. |
| `GET /connect` | WebSocket. One phone connection, bare binary frames after the hello. |
| `GET /healthz` | Liveness and coarse load. |
| `GET /whoami` | `{"ip":"<your public ip>"}`, used by relay reachability checks. |
| `POST /probe` | Sends one UDP datagram back to the caller's own address. |

| Listener | Purpose |
| --- | --- |
| UDP 3478 | Address discovery. Answers with the source address of the request so the phone and the relay can offer reflexive ICE candidates. Stateless, unauthenticated, and never larger than the request. |

Both WebSocket routes start with a JSON hello exchange as text frames — the
gateway's `gateway_hello`, the client's `register` or `connect`, then the
gateway's `ready` — after which every frame is binary. A refusal arrives as a
`{"type":"error","code":...}` text frame followed by a close with the same code
as its reason. One exception, because it happens after framing has begun: when a
relay re-registers, the older link for that `relay_id` is closed with WebSocket
status 1008 and the reason `relay_busy`, and its phone connections are detached.

`gateway_hello` carries `stun_port`, the UDP port above, and omits it when the
listener is disabled. Only the port travels: a peer combines it with the gateway
host it already dialed, so the hello cannot send address discovery elsewhere.

`POST /probe` takes `{"port":<1024-65535>,"token":"<32 bytes, unpadded
base64url>"}` and answers `{"sent":true,"observed_ip":"..."}`. The datagram
always goes to the source address the gateway observed; a destination address in
the request body is ignored, so the endpoint cannot be used for reflection or
amplification.

## The `/healthz` contract

```sh
curl -sS https://gw.example.com/healthz
```

```json
{"ok":true,"relays":3,"clients":4,"uptime_seconds":86412}
```

- Always `200` with `ok: true` while the process is serving; a load balancer can
  treat any other status, or a connection failure, as down.
- `relays` is the number of live registrations, `clients` the number of phone
  connections currently paired across all of them.
- `uptime_seconds` is whole seconds since startup.
- No authentication, and deliberately no relay ids, addresses, or byte counts —
  the endpoint is safe to expose publicly.
- `stun_port` is the UDP port address discovery answers on, or absent when the
  listener is disabled.

## Running one for other people

A gateway that only you dial needs no operations. One that other people's
phones depend on needs four habits and no more; the posture is explicitly
best-effort, and that is what the app tells users.

**Watch one number.** `clients` in `/healthz` is the only signal that matters:
it is the count of phones currently on the relayed path. It is normal for it to
sit near zero even with many active users, because every session leaves the
gateway within ten seconds of the direct path forming. A `clients` count that
climbs and stays up means hole punching is failing for a cohort, not that the
gateway is busy.

```sh
watch -n30 'curl -sS https://gw.example.com/healthz'
```

**Give the state file a disk that persists.** `HERDR_GATEWAY_STATE` holds the
relayed-byte counters that enforce the monthly quota. Losing it resets the
month's accounting; it does not break any session. It is rewritten every 30
seconds and on shutdown, so a snapshot is never far behind.

**Certificates renew themselves.** Caddy owns ACME; there is nothing to rotate
by hand. The one failure worth alerting on is Caddy refusing to renew because
port 80 stopped being reachable — the same inbound rule the deploy opened.

**Upgrades are a redeploy.** `bash relay/gateway-deploy.sh` against the same
host reuses the remembered answers and rebuilds from the bundle it copies. Live
relays reconnect on their own; phones fall back to the gateway path while a
restart is in flight, so an upgrade costs a few seconds of relayed latency and
no session.

**What you are not promising.** No uptime target, no capacity guarantee, no
data retention: the gateway holds no keys and stores nothing but byte counters.
If it goes down, phones with a direct path keep working, phones without one
reconnect when it returns, and anyone who needs more can self-host with this
document or point at their own with `HERDR_GATEWAY_URL`.

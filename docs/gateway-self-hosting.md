# Self-hosting the Herdr gateway

The gateway (`cmd/herdr-gateway`) is the fallback transport between a phone and
a computer relay. It replaces the per-user Cloudflare tunnel: the relay dials
**out** to the gateway over WSS, the phone dials **in**, and the gateway copies
opaque frames between them. When the phone and the computer manage a direct
WebRTC DataChannel, the gateway is used only for rendezvous and signaling.

You do not have to trust the gateway operator, and you do not have to use the
project's gateway.

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
Password authentication is fine: the deployment authenticates once and reuses
that session. Only sudo must not prompt, so use root or an account with
passwordless sudo.

The project-operated community gateway is shared and best-effort. Self-hosting
gives you dedicated bandwidth and control of the gateway's transport logs.
Neither can read application plaintext: E2EE terminates at the phone and relay.

Running your own gateway does not mean giving up the community ones. The
deployment offers to keep them, and writes `HERDR_GATEWAY_URL` with your own
entries first and the community gateways after;
`HERDR_GATEWAY_DEPLOY_FALLBACK=false` declines them non-interactively. Because
a configured list is ordered by priority (`HERDR_GATEWAY_SELECTION=ordered`,
the default), a community gateway is reached only when every one of your own is
unhealthy: it is a cold fallback, not a faster alternative that can pull
traffic off your box.

## The hostname phones dial

The gateway needs one public name pointing at the server: an `A` record, say
`gw`, at the server's IPv4. On Cloudflare set it to **DNS only** (grey cloud); a
proxied record terminates TLS at the edge, so phones trust Cloudflare's
certificate, not Caddy's. Add `AAAA` only if the server answers on IPv6: Let's
Encrypt prefers IPv6 when a name publishes one, so a dead `AAAA` blocks every
certificate attempt while IPv4 checks look healthy. Verify the record resolves
to the server before deploying:

```sh
dig +short gw.example.com
```

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
- terminal output, prompts, uploads, or push subscriptions.

Both rendezvous values are derived from the relay key, which the gateway never
has, and it never derives, stores, or verifies either:

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
contents are never logged. The only thing persisted, and only when you configure
it, is a per-relay relayed-byte counter (`HERDR_GATEWAY_STATE`).

## Deploying

One small VPS is the design target: 1 vCPU, 1–2 GB RAM, IPv6, and a provider
that tolerates outbound UDP. Concurrent connections are the binding constraint,
not bandwidth — each paired phone costs a goroutine pair and a bounded 4 MiB
outbound queue — so size for peak simultaneous phones and file descriptors.

Expect 0.5–1 GB of egress per month per *relayed* active user; a direct WebRTC
path costs almost nothing beyond rendezvous. A thousand active users with a
fifth relayed is 75–150 GB/month, under 0.5 Mbit/s on average.

On a metered host, check whether inbound counts too — a relay pays for every
byte twice — and set a budget alarm plus a lower `HERDR_GATEWAY_MONTHLY_BYTES`,
so the gateway's own quota refuses new relayed connections before the bill
grows.

Latency, not capacity, is the reason to add a second instance, and scaling out
needs no shared state or session affinity: `gw-eu`, `gw-us`, and `gw-ap` are
independent deployments, and each relay carries its own ordered gateway list.

The gateway needs inbound TCP 80 and 443, inbound UDP 3478, and outbound UDP.
`/probe` replies to the address the client connected from, one request per 10 s
per IP, returning fewer bytes than it receives: it cannot reflect or amplify,
but some providers' abuse tooling still flags UDP replies.

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

`3478/udp` is published on every interface because address discovery is raw UDP
and no TLS reverse proxy can carry it. Set `HERDR_GATEWAY_STUN_ADDR=` (empty) to
turn the listener off and drop the port.

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
process dies.

`HERDR_GATEWAY_MAX_CLIENTS` is a memory bound, not a bandwidth one. Each
connection may queue up to 4 MiB before it is dropped as too slow, so the default
512 caps the worst case near 2 GiB. Raise it with the RAM you have; a session
that upgrades to the direct path stops consuming a slot within seconds.

There is deliberately **no per-IP concurrency cap**: carrier NAT puts thousands
of unrelated phones behind a single address, so the cap would refuse legitimate
users while barely inconveniencing an attacker who has more addresses. The
per-IP *rate* limit stays.

Address discovery has its own ceilings: 20 datagrams per 5 seconds per source
address over a fixed table, plus a global 2000 per second, and a response can
never exceed roughly twice its request, so a forged source address wastes a
little of the gateway's CPU and none of a victim's bandwidth.

If `HERDR_GATEWAY_STATE` is set, the file is loaded at startup, rewritten
atomically every 30 seconds and on shutdown, and created with mode `0600`. It
contains counters and nothing else. Counters from previous months are dropped on
load.

### Address discovery

The gateway answers address discovery on UDP `HERDR_GATEWAY_STUN_ADDR` so the
phone and the relay learn the address the internet sees them at, which each side
then offers as a reflexive ICE candidate. Neither peer can learn that address on
its own, because NAT rewrites the source port on the way out.

Three properties keep this from widening the gateway's blast radius:

- **Only the port is advertised.** The gateway's hello carries `stun_port` and
  nothing else. Each peer builds the address from the gateway host it already
  dialed — the relay from `HERDR_GATEWAY_URL`, the phone from its stored gateway
  URL — and refuses a host from the gateway. A compromised or hostile gateway
  therefore cannot redirect address discovery at a third party; the worst it can
  do is point at one of its own ports.
- **Reflection reveals nothing new.** The answer is the source address of the
  packet the gateway just received, which it already observes on the WSS
  connection. Requests are unauthenticated, stateless, and answered with fewer
  bytes than they carry, so the listener cannot be used for amplification.
- **No third-party service is involved.** Address discovery runs on the gateway
  you already trust with rendezvous, so no external STUN provider joins the trust
  boundary, and nothing but this gateway's own WSS path ever relays traffic on
  the peers' behalf.

UPnP/NAT-PMP port mapping on the relay (`HERDR_REACHABILITY_PORT_MAPPING`) gives
a stable, router-blessed candidate, but it is no longer required for a direct
path off the LAN, and it never worked for the phone at all.

A session still runs relayed when either side blocks UDP outright, or when both
ends sit behind symmetric NAT. Disabling the listener
(`HERDR_GATEWAY_STUN_ADDR=`) leaves the relay with host candidates plus whatever
UPnP/NAT-PMP obtained, which is what a LAN-only deployment needs; everything
else relays.

## Quota tuning

The quota bounds a public gateway's bandwidth bill. It behaves like this:

1. At `HERDR_GATEWAY_QUOTA_WARN_PERCENT` of the monthly limit the relay receives
   exactly one advisory notice, which the relay surfaces in its UI.
2. Once the limit is reached the relay receives one `quota_exceeded` notice and
   **new** phone connections are refused with the `quota_exceeded` code.
   Connections that are already established are never severed mid-session.
3. Counters reset on the UTC month boundary.

Guidance:

- Self-hosting for yourself or a household: set `HERDR_GATEWAY_MONTHLY_BYTES=0`.
- Hosting for a group: divide your monthly egress allowance by the number of
  relays you expect and halve it, since every relayed byte is counted once on the
  way in and once on the way out. On a 1 TB plan with 20 relays, 25 GiB
  (`26843545600`) per relay is comfortable.
- A relay that keeps hitting the quota has a failing direct WebRTC path. Fixing
  reachability on that computer (UPnP/NAT-PMP, IPv6, or a forwarded UDP port)
  removes the traffic from the gateway entirely.

## Pointing a relay at your gateway

On the computer relay:

```sh
HERDR_GATEWAY_URL=wss://gw.example.com
```

A base URL with no trailing slash and no path; the relay appends the routes. The
paired phone learns the same base from the QR payload, so a relay and its phones
must use the same gateway. Moving gateways changes neither the relay key nor the
QR payload.

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
listener is disabled.

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

A gateway that only you dial needs no operations. One that other people's phones
depend on needs four habits, and the posture stays best-effort — which is what
the app tells users.

**Watch one number.** `clients` in `/healthz` is the count of phones currently on
the relayed path. It sits near zero even with many active users, because every
session leaves the gateway within ten seconds of the direct path forming. A
`clients` count that climbs and stays up means hole punching is failing for a
cohort, not that the gateway is busy.

```sh
watch -n30 'curl -sS https://gw.example.com/healthz'
```

**Give the state file a disk that persists.** `HERDR_GATEWAY_STATE` holds the
relayed-byte counters that enforce the monthly quota. Losing it resets the
month's accounting and breaks no session.

**Certificates renew themselves.** Caddy owns ACME. The one failure worth
alerting on is Caddy refusing to renew because port 80 stopped being reachable —
the same inbound rule the deploy opened.

**Upgrades are a redeploy.** `bash relay/gateway-deploy.sh` against the same host
reuses the remembered answers and rebuilds from the bundle it copies. Live relays
reconnect on their own, so an upgrade costs a few seconds of relayed latency and
no session.

**What you are not promising.** No uptime target, no capacity guarantee, no data
retention: the gateway holds no keys and stores nothing but byte counters. If it
goes down, phones with a direct path keep working, and the rest reconnect when it
returns.

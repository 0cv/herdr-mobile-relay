# Run your own gateway

The gateway is the fallback path between your phone and a computer relay: the
relay dials **out** to it over WSS, the phone dials **in**, and the gateway copies
encrypted frames between them. Once the two sides negotiate a direct WebRTC
connection, the gateway only handles rendezvous and signaling.

Running your own gives you dedicated bandwidth and keeps the transport logs on a
machine you control. It changes nothing about encryption: no gateway can read
your traffic.

You need a small VPS with Docker and a public hostname that resolves to it.
1 vCPU and 1–2 GB RAM is plenty.

## Deploy it from the setup menu

Open the setup menu and choose **Choose Connection Method → Your own gateway →
Deploy one on my own server over SSH**. It asks for the public hostname phones
will dial and the server's SSH address (`root@203.0.113.10`), then:

1. writes a compose bundle — `docker-compose.yml`, Caddy TLS configuration,
   `.env`, and the gateway source;
2. copies it to the server over one authenticated SSH connection, installing
   Docker there if you allow it;
3. runs `docker compose up -d --build` and waits for `/healthz` on the server and
   for the first certificate on the public name;
4. records the verified `wss://` URL as `HERDR_GATEWAY_URL` in the relay
   environment.

The image is built on the server from the bundled source, so the server needs
nothing but Docker — no Go toolchain, registry credentials, or GitHub access. The
relay key never leaves your computer.

Password authentication works; the deployment authenticates once and reuses that
session. Sudo must not prompt, so use root or an account with passwordless sudo.

Leave the SSH address empty to write the bundle without deploying it, or run the
wizard directly:

```sh
bash relay/gateway-deploy.sh
```

Answers are remembered, so a later run against the same host offers them as
defaults. To skip the prompts entirely, set `HERDR_GATEWAY_DEPLOY_HOST`,
`HERDR_GATEWAY_DEPLOY_SERVER`, `HERDR_GATEWAY_DEPLOY_REMOTE_DIR` (default
`/opt/herdr-gateway`), `HERDR_GATEWAY_DEPLOY_EMAIL` (ACME contact),
`HERDR_GATEWAY_DEPLOY_DIR` (local bundle directory), and
`HERDR_GATEWAY_DEPLOY_INSTALL_DOCKER=true`.

The deployment offers to keep the community gateways behind your own, and writes
your entries first; `HERDR_GATEWAY_DEPLOY_FALLBACK=false` declines them. The relay
tries a configured list in order, so a community gateway is used only when every
gateway of yours is unhealthy.

## DNS and firewall

Point one public name at the server with an `A` record. On Cloudflare set it to
**DNS only** (grey cloud): a proxied record terminates TLS at the edge, so phones
would trust Cloudflare's certificate instead of your gateway's. Add `AAAA` only if
the server really answers on IPv6 — Let's Encrypt prefers IPv6 when a name
publishes one, so a dead `AAAA` blocks every certificate attempt while IPv4 looks
healthy. Check the record before deploying:

```sh
dig +short gw.example.com
```

Open inbound TCP 80 and 443, inbound UDP 3478, and allow outbound UDP.

## What the gateway can and cannot see

It holds no secrets — not the relay key, not the pairing token, not the session
keys.

It sees source addresses, connection times, byte counts, frame sizes and timing,
and two rendezvous values derived one-way from the relay key — the 22-character
`relay_id` a relay registers under, and the challenge/answer pair of each phone
connection.

It cannot see frame contents (AES-256-GCM ciphertext from a session that
terminates on the phone and the relay), the WebRTC SDP and ICE candidates that
travel inside that session, or anything derived from them: prompts, terminal
output, uploads, push subscriptions.

The gateway does not authenticate anyone. It issues a random 32-byte challenge,
forwards the phone's HMAC answer to the relay, and **the relay** verifies it
before the encrypted handshake starts, using a key only it and the paired phone
can derive. A hostile gateway, or an attacker who steals a `relay_id`, still
cannot get past the relay.

Logs hold transport events at `INFO`, with relay ids truncated to six characters;
frame bytes, nonces, and proofs are never logged. The only thing persisted, and
only if you set `HERDR_GATEWAY_STATE`, is a per-relay byte counter.

## Capacity and bandwidth

Concurrent connections, not bandwidth, are the binding constraint: each paired
phone costs a goroutine pair and a 4 MiB outbound queue, so size for peak
simultaneous phones and file descriptors.

Expect 0.5–1 GB of egress per month per *relayed* active user; sessions that
reach the direct path cost almost nothing. On a metered host, check whether
inbound traffic is billed too — a relayed byte is paid for twice — and set a
budget alarm plus a lower `HERDR_GATEWAY_MONTHLY_BYTES` so the quota refuses new
relayed connections before the bill grows.

Latency, not capacity, is the reason to add a second instance. Instances need no
shared state or session affinity, and each relay carries its own ordered list.

## Deploying by hand

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

The image is `gcr.io/distroless/static:nonroot`: one static binary, no shell. For
quotas that survive restarts, drop `HERDR_GATEWAY_MONTHLY_BYTES=0`, add `-v
/var/lib/herdr-gateway:/state -e HERDR_GATEWAY_STATE=/state/counters.json`, and
make that directory writable by uid 65532 (`nonroot`).

`3478/udp` is published on every interface because address discovery is raw UDP
and no TLS proxy can carry it. `HERDR_GATEWAY_STUN_ADDR=` (empty) turns the
listener off so the port can stay closed.

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

On `SIGINT` or `SIGTERM` the gateway drops the relay links, flushes the counters,
and drains in-flight requests for up to 10 seconds.

## TLS termination

The gateway speaks plain HTTP and does **not** terminate TLS. Phones require
`wss://`, so bind the gateway to loopback and let a reverse proxy own the
certificate.

```caddyfile
gw.example.com {
	reverse_proxy 127.0.0.1:8443
}
```

nginx needs the WebSocket headers and a generous read timeout:

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

Set `HERDR_GATEWAY_TRUSTED_PROXY=true` behind a proxy, so the per-IP connect
limit and `/probe` see the real client address. Leave it `false` when the gateway
is exposed directly, or any client can forge `X-Forwarded-For` and bypass the
rate limit. Do not compress these routes: the payloads are ciphertext, so
compression buys nothing and leaks length information.

## Configuration

Everything is read from the environment at startup; there are no flags.

| Variable | Default | Meaning |
| --- | --- | --- |
| `HERDR_GATEWAY_ADDR` | `:8443` | Listen address. Use `127.0.0.1:8443` behind a proxy. |
| `HERDR_GATEWAY_STUN_ADDR` | `:3478` | UDP address-discovery listener. Must be reachable directly; a proxy cannot carry it. Empty disables it. |
| `HERDR_GATEWAY_MAX_CLIENTS_PER_RELAY` | `8` | Concurrent phone connections per relay. Negative removes the cap. Refusals return `too_many_clients`. |
| `HERDR_GATEWAY_MAX_RELAYS` | `1024` | Registered relays this gateway will hold. Negative removes the cap. Refusals return `at_capacity`. |
| `HERDR_GATEWAY_MAX_CLIENTS` | `512` | Phone connections across every relay. Negative removes the cap. Refusals return `at_capacity`. |
| `HERDR_GATEWAY_CONNECT_RATE_PER_MINUTE` | `30` | Phone connection attempts per client IP per minute; relay registrations are counted separately against the same number. Negative removes the limit. Refusals return `rate_limited`. |
| `HERDR_GATEWAY_MONTHLY_BYTES` | `5368709120` (5 GiB) | Bytes copied in both directions, per relay, per UTC calendar month. `0` means unlimited. |
| `HERDR_GATEWAY_QUOTA_WARN_PERCENT` | `80` | Percentage of the quota at which the relay receives one advisory warning. Negative disables it. |
| `HERDR_GATEWAY_IDLE_TIMEOUT` | `300` | Seconds a phone connection may carry no traffic before it is closed. Negative disables idle reaping. Relay links use ping/pong instead. |
| `HERDR_GATEWAY_STATE` | unset | JSON file holding the byte counters. Unset keeps them in memory only. |
| `HERDR_GATEWAY_TRUSTED_PROXY` | `false` | Believe the leftmost `X-Forwarded-For` entry. Enable only behind a proxy you control. |
| `HERDR_GATEWAY_LOG_FORMAT` | `text` | `text` or `json` structured logs on stderr. |

Fixed: protocol version 1, a 10-second hello deadline, the wire protocol's
per-frame ciphertext cap, a 30-second relay ping interval (a relay is dropped
after two missed pongs), and one `/probe` per 10 seconds per IP.

The state file, when configured, is loaded at startup, rewritten atomically every
30 seconds and on shutdown, and created with mode `0600`. It holds byte counters
and nothing else; previous months are dropped on load.

### Opening the gateway to other people

Most limits above are per-relay or per-IP, which is enough for a gateway that
serves only you. `HERDR_GATEWAY_MAX_RELAYS` and `HERDR_GATEWAY_MAX_CLIENTS` are
what bound a stranger who registers many self-generated relay ids from many
addresses. `HERDR_GATEWAY_MAX_CLIENTS` bounds memory rather than bandwidth: each
connection may queue up to 4 MiB before being dropped as too slow, so the default
512 caps the worst case near 2 GiB. Raise it with the RAM you have.

There is deliberately no per-IP *concurrency* cap — carrier NAT puts thousands of
unrelated phones behind one address, so it would refuse legitimate users while
barely inconveniencing an attacker with more addresses. The per-IP rate limit
stays.

## Address discovery

The gateway answers address discovery on UDP `HERDR_GATEWAY_STUN_ADDR`, so the
phone and the relay learn the address the internet sees them at and can offer it
as an ICE candidate. Neither peer can learn it alone, because NAT rewrites the
source port on the way out, and no third-party STUN service is involved.

The gateway's hello advertises only a port; each peer builds the address from the
gateway host it already dialed, so a hostile gateway cannot redirect discovery at
a third party. Replies go to the source address of the request, never exceed
roughly twice its size, and are limited to 20 datagrams per 5 seconds per source
address and 2000 per second overall — the listener cannot be used for reflection
or amplification.

A session stays relayed when either side blocks UDP, or when both ends sit behind
symmetric NAT. Disabling the listener leaves the relay with LAN candidates plus
any UPnP/NAT-PMP mapping it obtained, which is what a LAN-only deployment needs.

## Quota tuning

1. At `HERDR_GATEWAY_QUOTA_WARN_PERCENT` of the monthly limit, the relay receives
   one advisory notice and shows it in its UI.
2. At the limit, the relay receives one `quota_exceeded` notice and **new** phone
   connections are refused with that code. Established connections are never cut
   mid-session.
3. Counters reset on the UTC month boundary.

For yourself or a household, set `HERDR_GATEWAY_MONTHLY_BYTES=0`. For a group,
divide your monthly egress allowance by the number of relays you expect and halve
it, since every relayed byte is counted on the way in and on the way out — on a
1 TB plan with 20 relays, 25 GiB (`26843545600`) per relay is comfortable.

A relay that keeps hitting the quota has a failing direct path; fixing
reachability on that computer (UPnP/NAT-PMP, IPv6, or a forwarded UDP port) takes
its traffic off the gateway entirely.

## Point a relay at your gateway

```sh
HERDR_GATEWAY_URL=wss://gw.example.com
```

A base URL, no trailing slash and no path; the relay appends the routes. The
paired phone learns the same base from the QR payload, so a relay and its phones
use the same gateway. Moving gateways changes neither the relay key nor the QR.

## Routes

Served on `HERDR_GATEWAY_ADDR`, normally behind the TLS proxy:

| Route | Purpose |
| --- | --- |
| `GET /relay` | WebSocket. One multiplexed registration per computer relay. |
| `GET /connect` | WebSocket. One phone connection. |
| `GET /healthz` | Liveness and coarse load. |
| `GET /whoami` | `{"ip":"<your public ip>"}`, used by relay reachability checks. |
| `POST /probe` | Sends one UDP datagram back to the caller's own address, so the relay can test inbound UDP. |

Address discovery listens separately on UDP `HERDR_GATEWAY_STUN_ADDR` and must be
published directly on the host.

Both WebSocket routes begin with a JSON hello exchange and carry binary frames
afterwards. A refusal arrives as `{"type":"error","code":...}` followed by a close
with the same code as its reason.

`/healthz` needs no authentication and exposes no relay ids, addresses, or byte
counts, so it is safe to expose publicly:

```json
{"ok":true,"relays":3,"clients":4,"uptime_seconds":86412,"stun_port":3478}
```

`relays` counts live registrations, `clients` counts phone connections paired
across all of them, and `stun_port` is `0` when address discovery is disabled.
Anything other than `200` with `ok: true`, or a failed connection, means down.

## Operating one for other people

**Watch `clients`.** It counts phones on the relayed path and sits near zero even
with many active users, because sessions leave the gateway within seconds of the
direct path forming. A count that climbs and stays up means hole punching is
failing for a group of users, not that the gateway is busy.

```sh
watch -n30 'curl -sS https://gw.example.com/healthz'
```

**Give `HERDR_GATEWAY_STATE` a disk that persists.** Losing the file resets the
month's quota accounting and breaks no session.

**Keep port 80 reachable.** Caddy renews certificates itself; the failure worth
alerting on is a renewal blocked because the ACME port was closed.

**Upgrade by redeploying.** `bash relay/gateway-deploy.sh` against the same host
reuses the remembered answers and rebuilds from the bundle it copies. Relays
reconnect on their own, so an upgrade costs a few seconds of relayed latency and
no session.

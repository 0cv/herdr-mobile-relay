# Plan: Hybrid Transport — WebRTC Direct + Blind WSS Gateway Fallback

Status: implemented; Phase 8 (Cloudflare removal) deferred to the bridge-window release
Replaces: per-user Cloudflare Tunnel (quick and stable setup) as the transport
between the phone PWA and the computer relay.

## 0. Implementation notes

Deviations decided while implementing this plan; the sections below are
otherwise authoritative.

- **Phase 1 (connectivity spike) was skipped.** Its deliverables — a tiny
  gateway, a Pion responder, and a browser page implementing §5/§6 — are
  superseded by the production Phase 4 and Phase 5 components, which are built
  against the same matrix and are not disposable. Nothing was cut: the go/no-go
  evidence is collected against the shipped implementation instead of a throwaway
  one.
- **Phase 8 (Cloudflare removal) is deferred to a post-migration release.**
  Tunnel provisioning, `cloudflared` install/supervision, the Pages deploy
  manager, and teardown state all remain, and the release manifests keep
  declaring `herdr-e2ee-v1` alongside `herdr-hybrid-v1` for the whole bridge
  window (§10).
- **Phone authentication is verified by the relay, not the gateway** (deviation
  from §4.1). A gateway that checked the connect HMAC itself would have to hold
  `rendezvous_key` per relay, contradicting the "blind by construction" and
  stateless goals. Instead the gateway holds *zero* secrets: it issues a 32-byte
  nonce, forwards the phone's `{nonce, proof}` to the relay inside the multiplex
  `OpOpen` frame, and the relay verifies the HMAC against its own derived
  `rendezvous_key` before the E2EE handshake starts, closing that `conn_id`
  immediately on failure. Relay registration is first-come per derived
  `relay_id`, and a later registration replaces the earlier one because a
  reconnecting relay is the common case. The gateway keeps only anti-abuse
  duties: per-IP connect rate limits, per-relay client caps and byte quotas,
  frame-size caps, and idle timeouts.
- **The §6 chunk framing applies to both binary transports, not just the
  DataChannel.** A 10 MiB image upload is one ~13.5 MB encrypted frame, which
  the gateway's 1 MiB per-frame cap cannot carry; raising that cap would let a
  handful of phones pin hundreds of megabytes on one shared VPS, exactly the
  cost profile §8 avoids. The framing therefore lives in `internal/framing`
  (mirrored by `frontend/src/lib/transports/chunking.ts`) and is applied
  underneath the whole connection — the E2EE handshake included — at 16 KiB
  chunks on the DataChannel and 256 KiB chunks on the gateway path.
  `gatewaywire.MaxFramePayload` stays at 1 MiB.
- **Signaling is accepted on any authenticated path except an existing direct
  one** (narrowing of §4.2's "only valid on gateway-path connections"). The
  E2EE handshake is the authorization boundary and is identical on every path,
  so a path-based gate adds no security while blocking the bridge-window users
  on a legacy WSS URL who benefit most from the upgrade. Recursive upgrades are
  refused, and negotiation is rate-limited per client (8 per minute, 2 s
  minimum spacing) on top of the bounded session and candidate counts.
- **The frontend bootstrap budget moved from 108 KiB to 112 KiB gzip.** The
  release contract ships a single `assets/app.js` whose hash is verified, so
  the hybrid transport cannot be code-split out of the initial payload; the
  guard in `frontend/scripts/check-size.mjs` was raised by the measured amount
  with the reason recorded next to it.

## 1. Summary

Replace Cloudflare with a two-path transport:

1. **Blind WSS gateway** (project-operated, self-hostable): carries rendezvous,
   WebRTC signaling, and encrypted application traffic as a fallback. Usable in
   ~250–400 ms on any network that permits HTTPS.
2. **WebRTC DataChannel** (browser-native, Pion on the relay): the preferred
   data path. Established in the background after the fallback connects; the
   connection upgrades transparently when ICE succeeds.

The existing application E2EE (P-256 ECDH + HKDF + AES-256-GCM,
`internal/transport/e2ee.go`, `frontend/src/lib/e2ee.ts`) runs unchanged over
both paths. The gateway never sees plaintext, SDP contents, relay keys, or push
subscriptions.

```
                        ┌────────── direct WebRTC DataChannel ─────────┐
                        │            (preferred data path)             │
  Phone PWA ── WSS ── Gateway ── WSS (outbound) ── Computer relay ── Herdr
  (static origin)       │
                        └── rendezvous + signaling + encrypted fallback
```

## 2. Goals / non-goals

Goals:

- No per-user Cloudflare account, domain, DNS record, or `cloudflared` process.
- Zero-install phone: the PWA remains the only client.
- Works on hostile networks (CGNAT cellular, office Wi-Fi, blocked UDP) via the
  WSS fallback — never a hard failure where HTTPS works.
- Direct phone↔computer path for the majority of pairs; project bandwidth is
  paid only for rendezvous plus the fallback minority (~15–25 %).
- Preserve the existing E2EE trust model and QR onboarding.
- Pure Go backend (Pion), TypeScript frontend (native `RTCPeerConnection`).
  No Rust, WASM, TURN, or STUN/TURN credential infrastructure.
- Gateway URL is relay configuration, not a hardcoded constant: project-hosted
  default, self-hosted alternative.

Non-goals:

- Seamless in-flight message migration between paths. The protocol is already
  snapshot-on-connect plus idempotent commands with request timeouts; a path
  switch behaves like a fast reconnect.
- TURN. The WSS gateway is the relay of last resort and is more reachable
  (works through HTTP proxies) than TURN/TLS.
- Native phone apps, iroh, libp2p (see §14 for the re-evaluation triggers).

## 3. Decision record

| Decision | Rationale |
| --- | --- |
| Hybrid, not pure WebRTC | Cold WebRTC setup (signaling+ICE+DTLS+SCTP+DCEP+E2EE) is 0.5–2 s and fails on ~10–20 % of pairs. The fallback provides Cloudflare-parity resume (~250–400 ms) and a 100 % reachability floor. |
| WSS fallback, not TURN | One service instead of two; rides through HTTP proxies TURN cannot; reuses the mandatory signaling connection; blind by construction. |
| Signaling inside the E2EE channel | Offer/answer/ICE flow as new message types over the already-authenticated encrypted fallback session. The gateway never sees SDP (which contains IPs); auth and replay protection are inherited. |
| Keep application E2EE on both paths | DTLS protects the wire, not authorization. The relay-key handshake proves the phone may control Herdr and keeps every intermediary (gateway operator included) blind. |
| One reliable ordered DataChannel | The E2EE session requires strict sequence order (`e2ee.go` `open()`); multiple channels would need reorder buffers or separate sessions. Revisit only if bulk transfers measurably block controls. |
| Close fallback after direct is stable | Avoids duplicate-broadcast suppression complexity in the hub. Reconnecting the gateway on direct failure costs ~250 ms; ICE restarts imply path failure anyway. |
| Gateway credentials derived from relay key | `relay_id = HKDF(relay_key, "herdr-gw-id")`, `rendezvous_key = HKDF(relay_key, "herdr-gw-auth")`. QR payload unchanged; derivation is one-way so the gateway learns nothing about the relay key. Rotation couples to the existing `rotate-token` flow — acceptable. |
| Gateway-relayed mode ships before WebRTC | Phase 4 alone already delivers "Cloudflare-free". De-risks the schedule; WebRTC becomes a latency/cost upgrade, not a launch blocker. |

Alternatives rejected (evaluated 2026-08): Iroh (browser = relay-forever, Rust
at both ends), libp2p (WebRTC underneath + large surface), Tailscale/Headscale
(VPN install on phone), hosted tunnels (same dependency, new vendor), direct
exposure (CGNAT), Tor (client requirement), brokers (semantic mismatch).

## 4. Components

### 4.1 Gateway (new)

New top-level component, e.g. `cmd/herdr-gateway` + `internal/gateway`.
Single static Go binary; Docker image for self-hosting; deploy target is one
small flat-egress VPS.

Responsibilities — and nothing else:

- Accept one outbound WSS registration per computer relay.
- Accept phone connections addressed by `relay_id`.
- Verify HMAC challenges against `rendezvous_key`; pair connections.
- Copy opaque frames bidirectionally.
- Enforce limits: per-IP connection rate, per-relay concurrent clients,
  per-relay monthly relayed-byte quota (soft: warn; hard: refuse relay path,
  never refuse signaling), frame-size cap, idle timeouts.
- Serve `/healthz`.

Explicit non-responsibilities: no Herdr protocol awareness, no persistence
beyond counters, no TLS-terminated inspection of inner frames, no logging of
frame contents or SDP.

Wire protocol (binary WS frames after a JSON hello):

```
frame = [u8 version=1][u8 opcode][u32 conn_id][payload]
opcodes: DATA=0  OPEN=1  CLOSE=2  PING=3  PONG=4
```

- Relay hello: `{"type":"register","relay_id","proof","proto":1}` where
  `proof` answers a server nonce with HMAC(rendezvous_key).
- Phone hello: `{"type":"connect","relay_id","proof","proto":1}`.
- After pairing, `DATA` payloads are verbatim Herdr E2EE frames
  (raw ciphertext bytes — see §8 framing changes).

### 4.2 Computer relay (Go)

- **Transport-neutral hub** (`internal/transport`): extract a logical-frame
  connection interface (read frame / write frame / close / context / name)
  from `ws.go`. `ClientConn`, admission ordering, send buffers, slow-client
  eviction, metrics, and shutdown stay in common code; `coder/websocket`
  becomes one adapter. `performServerE2EEHandshake` consumes the interface.
- **Gateway adapter**: maintains the outbound WSS registration (reconnect with
  backoff, re-register), demuxes `conn_id`s into per-phone logical connections
  fed to the hub exactly like local WebSocket clients.
- **WebRTC adapter** (Pion v4): one shared UDP mux (single local port, one
  listener regardless of peer count); one `PeerConnection` per requesting
  client; accepts only the expected channel label + transport version; adapts
  DataChannel messages to the logical-frame interface via §6 framing; bounds
  pending negotiations and reassembly state.
- **Signaling handlers**: new message types on the existing encrypted channel:
  `webrtc_offer`, `webrtc_answer`, `webrtc_ice`, `webrtc_close` (all with
  `request_id`, rate-limited, only valid on gateway-path connections).
- **Reachability helpers** (§7): UPnP/NAT-PMP/PCP mapping attempt, IPv6
  candidate support, self-test via gateway probe.

### 4.3 Frontend (TypeScript/Svelte)

- **Transport abstraction**: extract the WebSocket wiring out of
  `frontend/src/lib/store.ts` (`connectRelay`, `sendRaw`, handlers) into
  `lib/transports/{types,websocket,gateway,webrtc}.ts`. `RelayStore` consumes
  a `RelayTransport` interface: `connect()`, `send(frame)`, `onFrame`,
  `onStateChange`, `close()`. Command handling, E2EE, timeouts, agent merging,
  and UI state are untouched.
- **Gateway transport**: WSS to the gateway, hello/proof, binary frames.
- **WebRTC transport**: offer creation, trickle ICE (signaling messages sent
  through the store's encrypted send path), DataChannel framing (§6),
  `bufferedAmount`/`bufferedamountlow` backpressure, ICE restart.
- **Path manager**: fallback-first connect; background direct attempt; switch
  active transport when the DataChannel's E2EE session completes and the
  snapshot arrives; close fallback after 10 s of direct stability; on direct
  failure reconnect gateway immediately and retry direct with capped backoff.
- **Resume-latency fixes** (Phase 0, transport-independent):
  - `visibilitychange`/`pageshow`/`online` → reset reconnect backoff
    (`RECONNECT_BASE_DELAY_MS` currently 3 s) and revalidate immediately.
  - Replace the 10 s `CONNECTION_HEALTH_TIMEOUT_MS` half-dead detection with
    an app-level ping message and ~2 s deadline on foreground.

### 4.4 Setup, packaging, services

- `relay/setup.sh` / `start.sh`: stop installing/launching `cloudflared` on the
  new path; relay registers with the configured gateway; QR printed once the
  gateway confirms registration.
- `relay/stable-setup.sh`: no tunnel/DNS provisioning; installs the relay-only
  user service; records gateway URL + verification in state.
- `herdr-plugin.toml`, service wrappers, uninstall/teardown scripts, and shell
  tests updated accordingly (Cloudflare paths remain during the bridge window).
- Env: `HERDR_GATEWAY_URL` (default: project gateway), plus existing vars.

### 4.5 QR / saved-relay schema

Transport-tagged, backward compatible (missing `transport` = legacy WSS URL):

```ts
type RelayConfig =
  | { transport?: 'websocket'; id; label; url; token }
  | { transport: 'hybrid'; id; label; gatewayUrl; token }  // relay_id derived from token
```

All secrets stay in the URL fragment; the app strips the fragment after import
(existing behavior in `lib/config.ts` `importQuickSetup`).

### 4.6 PWA origin

A stable canonical HTTPS origin is required (new phones cannot bootstrap the
app through a DataChannel). Options: GitHub Pages or gateway-hosted static
files — decide in Phase 4 (gateway-hosted keeps one dependency and lets the
existing release `web_hash` verification carry over).

Origin migration for installed PWAs (localStorage, service worker, and push
subscriptions are origin-scoped): bridge app gains an explicit "Move to the
new app" action — serialize relay configs into a fragment, open the canonical
origin, import, clear, prompt reinstall + push re-registration.

## 5. Connection lifecycle

Cold connect / resume:

1. Phone opens WSS to gateway, proves rendezvous, pairs → logical connection.
2. Existing E2EE handshake over it → usable (~250–400 ms; Cloudflare parity).
3. Background: `webrtc_offer` over the encrypted channel; trickle ICE; DTLS;
   SCTP; DataChannel opens; second (independent) E2EE handshake over the
   channel; relay hub treats it as a new client and sends the snapshot.
4. Phone switches the active transport to direct; after 10 s stable, closes
   the gateway connection.
5. Direct failure at any time → reconnect gateway (fast), resume relayed,
   retry direct.

Network change (Wi-Fi↔cellular): DataChannel survives via ICE restart when the
`PeerConnection` is alive (~100–300 ms; signaling requires reopening the
gateway connection first if it was closed). Otherwise full cold connect.

Ordering/consistency: no cross-path continuity. Every new hub client receives
the full snapshot (existing `SetOnConnect` behavior in
`internal/app/server.go`); pending `request_id`s fail/timeout and are retried
per existing store semantics.

## 6. DataChannel framing (mandatory)

DataChannel messages have practical size limits (~64 KiB default per SDP
`max-message-size`; head-of-line risks with large messages). Current logical
messages reach ~14 MiB+ (10 MiB upload as base64 data URL inside JSON).

- Chunk every encrypted logical frame into ≤16 KiB binary chunks:
  `[u8 ver=1][u8 flags: START|END][payload]`; `START` chunk prepends
  `[u32 logical_len]`.
- Reliable ordered channel ⇒ no message ids or reorder buffers; exactly one
  in-flight logical message per direction.
- Enforce the existing 21 MiB logical cap (`wsMaxReadBytes`); reject oversized,
  malformed, or stalled (>30 s) assemblies by closing the channel.
- Sender backpressure: pause at `bufferedAmount` ≥ 4 MiB, resume via
  `bufferedamountlow` at 1 MiB; retain the 5 s send-timeout/eviction semantics
  from `writePump`.

## 7. Desktop reachability helpers

Raises direct-path success for the dominant use case (phone on cellular →
computer at home) and enables the future certhash path (§14):

- Attempt UPnP/NAT-PMP/PCP UDP port mapping for the shared ICE mux port.
- Advertise IPv6 host candidates; attempt PCP/IGD pinhole where available.
- Self-test: ask the gateway to probe the mapped address; expose the result in
  `status` output and `/healthz`.

## 8. Bandwidth and cost controls

Expected load: signaling ≈ negligible for 100 % of users; full traffic only
for the fallback share. Estimate ~0.5–1 GB/month per *relayed* active user;
at 1,000 active users ≈ 75–150 GB/month — one flat-egress VPS. Levers:

- Binary WS frames on the gateway path: raw ciphertext, dropping the base64
  (+33 %) and JSON envelope of the current E2EE frame encoding.
- Compress plaintext before encryption (the encrypted path currently disables
  WS compression entirely — `ws.go` `CompressionDisabled`).
- Relayed-path defaults: terminal refresh 500 ms (direct: 250 ms), history
  1,000 lines. Direct connections keep full fidelity.
- Per-relay quotas with in-app warnings pointing at self-hosting or router
  fixes; signaling is never capped.
- Optional CDN in front of the project gateway if DDoS requires it.

## 9. Security requirements

- Relay key never leaves phone/relay; gateway sees only HKDF-derived ids.
- Both hello proofs are challenge-response (server nonce), not bearer strings.
- Gateway pairing grants only frame copying; the Herdr E2EE handshake remains
  the sole authorization for control.
- Rate limits on signaling message types; bounded pending `PeerConnection`s
  and ICE candidate counts on the relay.
- WebRTC exposes a UDP socket on the computer: ICE requires the remote to know
  the session ufrag/pwd (delivered only inside the E2EE channel), and DTLS
  certs are pinned via SDP fingerprints. Document the changed posture (was:
  strictly outbound tunnel).
- Existing invariants preserved: constant-time token comparisons, no secrets
  in URLs/headers, fragment-only QR secrets, loopback tokenless dev mode.

## 10. Compatibility and rollout

- New transport capability id: `herdr-hybrid-v1` in release manifests.
  Bridge release declares `app_transports`/`relay_transports` =
  `[herdr-e2ee-v1, herdr-hybrid-v1]`; `ValidateUpgradeCompatibility`
  (`internal/release/compatibility.go`) already enforces the bridge window.
- Bridge behavior: existing configs keep WSS/Cloudflare; the updated relay
  advertises its hybrid descriptor over the already-authenticated connection;
  the app stores it and prefers the hybrid path. No mandatory re-scan.
- Cloudflare removal (tunnel provisioning, `cloudflared` install/supervision,
  Pages deploy manager, teardown state) only after the migration window, in a
  release that drops `herdr-e2ee-v1`-only support.

## 11. Phases

Phase 0 — Resume-latency fixes (independent, ship immediately)
- Foreground/online listeners reset backoff; ping-based health (~2 s).
- Acceptance: suspend→resume reconnect < 1.5 s on live network, on both
  current transports; existing frontend tests pass.

Phase 1 — Connectivity spike (disposable, go/no-go)
- Tiny gateway + Pion responder + browser page implementing §5/§6 end to end.
- Matrix: LAN, dual-NAT residential, CGNAT cellular→home, UDP-blocked Wi-Fi,
  HTTP-proxy egress, iOS Safari + installed PWA, Android Chrome + PWA,
  Wi-Fi↔cellular flip, 10 MiB transfer, forced-fallback mode.
- Go/no-go: fallback works everywhere HTTPS works; direct succeeds on LAN and
  ≥ one dual-NAT residential pair; framing sustains 10 MiB with stable memory.

Phase 2 — Transport-neutral hub (no behavior change)
- Logical-frame interface; `ws.go` becomes an adapter; E2EE handshake off
  `*websocket.Conn`. Existing `internal/transport` tests pass unchanged.

Phase 3 — Frontend transport abstraction (no behavior change)
- `RelayTransport` interface + WebSocket adapter; store consumes it.
- Acceptance: existing vitest + Playwright suites pass.

Phase 4 — Gateway service + relayed mode end to end  ← "Cloudflare-free" milestone
- `cmd/herdr-gateway`, relay gateway adapter, frontend gateway transport,
  derived credentials, quotas, Docker image, self-host docs.
- Acceptance: phone controls a relay through the gateway with no `cloudflared`
  running; gateway logs contain no plaintext/SDP; quota enforcement tested;
  black-box test with `cmd/fake-herdr` passes over the gateway path.

Phase 5 — WebRTC direct upgrade
- Pion adapter, signaling messages, framing, path manager, upgrade/failover.
- Acceptance: LAN pair upgrades to direct < 2 s after fallback connect;
  pulling the direct path (firewall rule) resumes relayed < 1 s; forced-relay
  env flag for testing; soak: 24 h connection with hourly network flips.

Phase 6 — Reachability helpers
- Port mapping, IPv6 candidates, gateway probe self-test, status surfacing.
- Acceptance: on a UPnP-enabled router, cellular phone→home connects direct.

Phase 7 — Setup/packaging/rollout
- Scripts, plugin actions, service installers, QR schema, bridge release with
  dual transports, PWA origin decision + migration action, docs (README,
  QUICKSTART, security section), shell tests.
- Acceptance: fresh install on a clean machine reaches "scan QR → control
  agent" with no Cloudflare dependency; upgrade from the previous release
  keeps existing phones connected throughout.

Phase 8 — Cloudflare removal
- Delete tunnel provisioning/supervision/teardown and Pages deploy manager;
  manifests drop the legacy transport; changelog + docs.

## 12. Test matrix (recurring, phases 4–7)

| Axis | Values |
| --- | --- |
| Phone network | LAN, residential Wi-Fi, CGNAT cellular, UDP-blocked office, HTTP-proxy-only |
| Home network | UPnP on, UPnP off, CGNAT ISP, IPv6 dual-stack, IPv6-off |
| Browser | iOS Safari, iOS installed PWA, Android Chrome, Android installed PWA, desktop |
| Events | cold start, resume after >30 s suspend, Wi-Fi↔cellular flip, gateway restart, relay restart, forced fallback |
| Payloads | keystrokes, 250 ms terminal stream, 10k-line history, 10 MiB upload |

## 13. Risks

| Risk | Mitigation |
| --- | --- |
| Direct success rate lower than expected on target networks | Fallback is a first-class path; Phase 1 measures before commitment; Phase 6 raises the rate. |
| Gateway becomes availability/abuse liability | Self-hostable + configurable URL; quotas; stateless design allows N instances; optional CDN front. |
| iOS PWA background/WebRTC quirks | Phase 1 exercises iOS explicitly; fallback path never depends on WebRTC. |
| Two E2EE sessions + path switching introduces state bugs | No cross-path continuity by design; snapshot-on-connect already reconciles; soak tests. |
| Bridge-window regressions for existing users | Transports manifest gate (`compatibility.go`) forces a dual-transport bridge release; upgrade test in Phase 7. |
| Scope creep (multi-channel, TURN, migration) | Explicit non-goals; revisit only with measurements. |

## 14. Future work / watch items

- **WebTransport + `serverCertificateHashes`**: simpler direct path (no ICE)
  when the desktop self-test (§7) proves reachability; certs rotate ≤14 days
  via gateway re-advertisement. Add as a third transport adapter when Safari
  support matures.
- **iroh**: adopt as an adapter if browser-direct connections ship
  (track n0-computer/iroh#2671 and release notes) or if a native phone app
  ever happens (native iroh then becomes the preferred stack).
- **Multiple DataChannels / WebTransport streams** for bulk transfers: only if
  measurements show uploads blocking terminal controls.
- **Community/self-hosted gateway federation**: E2EE already makes untrusted
  gateways safe (Syncthing relay-pool model).
- **Cloudflare Workers + Durable Objects as a gateway host**: one Durable
  Object per `relay_id` is exactly this design's pairing model, and the
  WebSocket Hibernation API is built for connections that idle between bursts.
  Rejected for now on three counts: Workers still has no UDP socket API, so the
  §7 `/probe` self-test cannot run there; it would fork a security-critical
  component into a second TypeScript implementation alongside the self-hostable
  Go binary; and Durable Object duration billing charges wall-clock time while
  a session is active, which is precisely when a terminal stream is *not*
  hibernating. Revisit only if Workers ships UDP sockets.

## 15. Open questions

1. **Gateway hosting — decided.** One entry-level VPS in the EU. The §8
   estimate of 75–150 GB/month at 1,000 active users fits inside the smallest
   included-traffic allowance on the market, so concurrency, not bandwidth, is
   the first limit to hit, and the shopping list is short: ≥1 TB egress,
   1 vCPU / 1–2 GB, IPv6, outbound UDP permitted. Two references as of August
   2026: OVHcloud d2-2 (€5.71/month ex-VAT, traffic unmetered but public
   bandwidth capped at 100 Mbit/s best-effort, Discovery range with a 99.95 %
   SLA rather than 99.99 %) and Hetzner CPX12 (€11.99/month, 20 TB included,
   outgoing-only accounting). d2-2 is the better value and the lower SLA is
   tolerable because a gateway outage does not drop established direct
   connections, only new rendezvous. Regional instances (`gw-eu`, `gw-us`, …)
   need no shared state or anycast: the gateway URL is per-relay configuration
   and the phone learns it from the QR or the hybrid descriptor. Default URL
   and funding still open.
2. Quota numbers (proposal: soft 5 GB/month relayed per relay, warn at 80 %).
   At 20 TB that ceiling supports roughly 4,000 relays at full quota.
3. **PWA canonical origin — decided: Cloudflare Pages.** It is already
   implemented and tested (`make web-deploy`, `WEB_PROJECT`, the `appdeploy`
   manager, and `web_hash` release verification), it introduces no per-user
   Cloudflare account, domain, DNS record, or `cloudflared` process, and it
   keeps the gateway to one job. Gateway-hosted static files would add an
   availability dependency between the app shell and the relay transport for no
   benefit. The relay learns the origin from `HERDR_PHONE_APP_URL`.
4. Whether Quick Start keeps a `cloudflared` escape hatch during the bridge
   window for users who cannot reach the project gateway.

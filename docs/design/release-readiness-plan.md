# Release readiness and transport parity plan

Working plan for shipping the hybrid transport (gateway + direct WebRTC +
address discovery) at a polish level comparable to iroh and Tailscale where it
matters, and deliberately not where it does not. Nothing here is released;
`v0.16.4` in production contains none of this work, so wire contracts and
defaults may still change freely.

## 1. Where we stand

Proven, with evidence from live runs and the test suites:

| Capability | Status | Evidence |
| --- | --- | --- |
| Relay-first fallback, blind gateway | done | black-box hybrid suite; live VPS |
| Address discovery via own gateway (UDP 3478) | done | `stun_port` in hello; pion interop test |
| Direct upgrade off-LAN over cellular | done | live 5G session, pair `host/srflx`, gateway `clients` 1 → 0 |
| Reflexive publication (port-preserving and port-rewriting NATs) | done | `NAT1To1IPs` + synthesised trickled srflx, unit-tested |
| NAT mapping keepalive | done | 10 s re-discovery; test pins it under a 30 s idle timeout |
| UPnP / NAT-PMP / PCP | done | `internal/portmap`, 30 min self-test |
| Session observability | done | `webrtc_sessions` in `/healthz`: candidate types + nominated pair, never addresses |
| Global abuse ceilings | done | `MaxRelays` 1024 / `MaxClients` 512 / STUN 2000/s + 20 per 5 s per source |
| Community gateway default | done | compiled into `relay/common.sh`; chooser order: Cloudflare, community, stable, own |
| SSH deploy wizard with remembered answers | done | `tests/test_gateway_deploy.sh` |
| Docs restructure | done | README 108 lines; six auxiliary docs, links verified |

All gates pass: `make go-check` (78 packages, race), `make frontend-check`
(svelte-check clean, 217 unit tests), `make shell-check`, and 66 browser tests
each in Chromium and WebKit.

## 2. Gap review

Each gap carries evidence, a proposed fix, effort (S/M/L), and whether it
blocks the release.

### Traversal

**G1 — IPv6 reflexive discovery: investigated, closed as not-applicable.**
The original premise was that `net.ResolveUDPAddr("udp", …)` yields one family,
so a dual-stack relay never advertises an IPv6 reflexive candidate. Implementing
per-family discovery disproved the premise in two ways, both measured here:
- **IPv6 does not need reflexive discovery.** There is no NAT to reflect
  through, so a global IPv6 address is already a reachable host candidate, and
  pion gathers it directly: the shared socket binds `[::]` (verified dual-stack,
  it writes to `::1` successfully) and `UDP6` is in the configured network types.
- **Pion cannot serve it anyway.** An IPv6-only discovery through
  `UniversalUDPMuxDefault.GetXORMappedAddrContext` times out against
  `pion/ice v4.4.1`, even with a working responder on `[::1]` and a socket that
  can reach it. Attempting it per cycle would burn the full discovery timeout
  (5 s) on every 10 s keepalive.
*Outcome:* discovery stays single-family and now says so in a comment naming the
measurement. The publish path was generalised to a set of mappings anyway
(`DiscoverMappedAddresses` / `PublishMappedAddresses`), which costs nothing and
is where a future v6 mapping would slot in. **No longer blocks release.**
The real IPv6 lesson is operational: this workstation has no global IPv6 at all,
which is why every direct path here had to traverse IPv4 NAT.

**G2 — No symmetric-NAT port prediction.** When a NAT allocates a new external
port per destination, the exchanged reflexive address is wrong for the peer and
no pair forms; we relay. Tailscale attempts port prediction and parallel
probing; iroh similarly. This is the one hard-NAT cohort where they beat us.
*Fix:* none yet — prediction is probabilistic, complex, and only worth it if
the cohort is real. Decision-gated by G9 telemetry: sessions whose
`remote_types` contain `srflx` but whose selected pair stays empty are exactly
this population. Effort **L**. **Blocks release: no** (relaying is the designed
fallback, same as DERP).

**G3 — No native relay link-change notification.** Tailscale re-probes the
instant an interface changes. The relay now notices on the next 10 s mapping
keepalive or on a failed attempt; mapped addresses are trickled into sessions
already negotiating. On the phone, Chromium's Network Information event runs
the existing 2 s application health probe, covering Wi-Fi/cellular handoffs
that remain nominally online.
*Fix (optional):* netlink subscription on Linux and route-change notification
elsewhere, triggering discovery immediately. Effort **M**. **Blocks release:
no** — the portable 10 s relay bound plus the phone-side event is appropriate
for a computer that mostly sits on one network.

### Availability

**G4 — Multi-gateway cold failover and selection (done).** `HERDR_GATEWAY_URL`
accepts an ordered list, and both the QR and live hybrid descriptor carry all
entries. The relay concurrently measures each gateway's DNS/TCP/TLS/HTTP health
round trip at startup, retains exactly one registration with the lowest-latency
healthy entry, and re-runs selection only after that entry fails. Configured
order breaks measurements within 20 ms and remains the fallback when every
probe fails. The live descriptor puts the selected gateway first, so an
already-paired phone learns a backup added later and follows the relay's choice
without interrupting the current session. This is latency-aware cold failover,
not an active-active fleet: no periodic probes, duplicate registrations, or
automatic migration of a healthy session.

**G5 — Registration displacement is unconditional.** `registerRelay` replaces a
live link whenever the same `relay_id` registers again (`relay.go`: "never
refused"). The id is a 128-bit HKDF output, so strangers cannot guess it — but
anyone who has ever seen it (operator logs show a 6-char prefix only, but a
shared QR or a compromised phone reveals the key from which it derives) can
evict the real relay in a loop.
*Fix:* prefer the live link — before replacing, ping the existing link with a
~2 s deadline; if it answers, refuse the newcomer with `relay_busy`. A genuine
restart wins because the dead link misses the ping; an attacker cannot evict a
healthy relay. Effort **S**. **Blocks release: yes** — it is cheap and it
protects the availability of every community-gateway user.

**G6 — Community gateway operations.** Decided: the permanent names are
`gw1.herdr-mobile.dev` (the original VPS) and `gw2.herdr-mobile.dev` (Canada),
compiled in as the ordered default so the relay probes both and keeps the
lowest-latency healthy one. The throwaway `gw.66556644.xyz` domain is gone and
no longer resolves, so every relay shipped before this change falls back to
nothing until it updates.
*Remaining:* (a) redeploy `gw1` under its new hostname so Caddy issues a
certificate for it — the old name's certificate is why it currently answers
`tlsv1 alert internal error`; (b) deploy `gw2`; (c) a minimal ops runbook:
`/healthz` watch, disk for `gateway-state`, certificate renewal is Caddy's, and
the documented posture is best-effort with no uptime promise.
**Blocks release: yes.**

### Product and UX

**G7 — `at_capacity` is invisible on the phone.** The gateway now refuses with
`at_capacity` when full, but `frontend/src/lib/transports/gateway.ts` maps only
`unknown_relay`, `quota_exceeded`, `rate_limited`, `too_many_clients`,
`relay_busy`, `bad_hello`, `internal`. Users on a full community gateway would
see a generic failure with no hint that trying later helps.
*Fix:* add the code with an honest message ("The shared gateway is at
capacity; try again later or switch transports") and decide fatality:
non-fatal, since capacity recovers. Effort **S**. **Blocks release: yes.**

**G8 — Resume relapse window.** Measured: ~12 s relayed after a phone
sleep/resume (3 s renegotiation + 10 s `DIRECT_STABILITY_MS` standby before the
gateway socket closes; traffic moves to direct at second ~3). Working as
designed; the standby exists so a flapping direct path cannot drop the session.
*Fix (optional):* attempt an ICE restart on the surviving session before a full
renegotiation, which would shave the 3 s half.
Effort **M**. **Blocks release: no.**

**G9 — No upgrade-success telemetry.** The per-session instrument exists;
nothing aggregates it. One counter pair in the relay's `/healthz`
(`sessions_direct_total`, `sessions_relayed_total` since start) answers "how
often does the direct path form for me" and produces the evidence the G2
decision needs. Local only, no addresses, nothing leaves the machine.
Effort **S**. **Blocks release: no, but do it** — it is the instrument the
roadmap depends on.

### Verification

**G10 — NAT-behaviour matrix: built and green.** `tests/blackbox/natmatrix_test.go`
plus `natlab_test.go` run the real relay, gateway and phone in five Linux
network namespaces per cell, with nftables standing in for the NAT dimensions:
`masquerade` for endpoint-independent mapping, `masquerade random` for
symmetric, conntrack alone for address-and-port-dependent filtering, and a
dynamic set plus port-preserving DNAT for address-dependent. The phone half is
the same test binary re-exec'd through `ip netns exec`, so it reuses the
existing `hybrid_test.go` handshake rather than a parallel client. Classified
from `/healthz` (`webrtc_sessions` nominated pair, then the direct/relayed
counters). Guarded by `HERDR_NAT_MATRIX=1` and skipped with a precise reason
without root, `ip`, or `nft`; `make nat-matrix` is the entry point.
Measured here (`unshare -rmn`, one run, 63 s):

| mapping | filtering | outcome |
|---|---|---|
| endpoint-independent | address-dependent | DIRECT, pair `host/prflx` |
| endpoint-independent | address-and-port-dependent | DIRECT, pair `srflx/srflx` |
| symmetric | address-dependent | RELAYED, gateway still serves commands |
| symmetric | address-and-port-dependent | RELAYED, gateway still serves commands |

Both symmetric cells relay, which is the expected shape: a per-destination
external port makes the address each side advertises wrong for the other, and
no filtering behaviour can rescue that. This is now the harness G2 port
prediction would have to be developed against.

**G11 — WebKit suite: verified on this machine.** `run-browser-tests.sh` takes
the Fedora path here: Chromium natively, WebKit in the pinned
`mcr.microsoft.com/playwright:v1.62.1-noble` container under podman. Both
projects ran green locally (66 passed each), so the suite no longer depends on
CI to be exercised before a tag.

**G15 — `make shell-check` was flaky (fixed).** `tests/test_gateway_deploy.sh`
asserted the uploaded bundle with `tar -tzf … | grep -Fq …` under
`set -o pipefail`: `grep -q` exits at the first match, `tar` then dies of
SIGPIPE, and the pipeline reports failure even though the entry was present.
It failed roughly one run in four, always as "uploaded archive carries no
gateway source", which reads like a packaging bug and is not one. The listing
now goes to a file first; 25 consecutive runs are green. Kept here because the
same shape (`long-producer | grep -q` under pipefail) is easy to reintroduce.

### Release engineering

**G12 — Version and bridge.** Ship as **0.17.0** (features, no breaking wire
change for existing users: releases declare both `herdr-e2ee-v1` and
`herdr-hybrid-v1`, and the legacy WSS fallback in the phone covers migrated
configs). Move CHANGELOG `[Unreleased]` under `0.17.0` at tag time.

**G13 — Docs hygiene.** `docs/webrtc-hybrid-transport-plan.md`,
`docs/terminal-history-journal-plan.md`, and this file are design documents,
not user docs: move to `docs/design/` (or delete the first two, which are
implemented). The README doc index stays as is.

**G14 — Packaging: verified, no work.** All plugin actions run from the
checkout (`relay/open-plugin-pane.sh`), which carries the wizard and the
gateway source `copy_gateway_source` needs; the release bundle whitelist is
unaffected. Recorded here so nobody re-audits it.

## 3. Deliberate non-goals

- **TURN.** The gateway *is* the relay of last resort; a second relay protocol
  buys nothing.
- **QUIC/WireGuard data plane.** SCTP DataChannel throughput is sufficient for
  terminal traffic and uploads; revisit only if a real workload says otherwise.
- **Peer relays** (Tailscale's newest tier): meaningless at one-relay-per-home
  topology.
- **Per-IP concurrency caps**: carrier CGNAT shares one address across
  thousands of phones; documented in `docs/gateway-self-hosting.md`.
- **G2 port prediction** until G9/G10 produce evidence of a real cohort.

## 4. Phases

### Phase 1 — correctness and cheap hardening (done)

1. **G1** — *closed, not implemented.* The premise did not survive measurement:
   pion's universal mux cannot answer an IPv6 XOR-mapped request through the
   shared socket, and IPv6 needs no reflexive discovery anyway. See G1 above.
2. **G5** — done. `registerRelay` probes the incumbent (2 s) and refuses the
   newcomer with `relay_busy` when it answers; a dead link is still replaced.
   Two gateway tests pin both halves.
3. **G7** — done. `at_capacity` maps to a non-fatal message; covered by the
   existing `it.each` fatality table.
4. **G9** — done. `sessions_direct_total` / `sessions_relayed_total` in
   `/healthz`, one bucket per finished session, asserted by a counter test.

### Phase 2 — resilience (done)

5. **G4** — done, both halves plus the seam between them. Relay:
   comma-separated `HERDR_GATEWAY_URL`, concurrent bounded health RTT selection,
   exactly one registration, and active-first live advertisement. Phone:
   `gatewayUrls` persistence and rotation before the legacy fallback, one pass
   per usable session as the ceiling.
6. **G10** — done; see the matrix above. `make nat-matrix`, root-gated.
7. **G3 / G8** — not done, still optional polish.

### Phase 3 — ship (partly done)

8. **G6** — runbook written; hostname decision and VPS redeploy still open.
9. **G13** done (docs under `docs/design/`); **G12** done in the working tree:
   `herdr-plugin.toml` and the README say 0.17.0, CHANGELOG cut, `web/` bundle
   restamped (`{"version":"0.17.0","assets":256}`).
10. **G11** done here: Chromium and WebKit both ran locally under podman.
11. Gates green (`make check`, `make shell-check`, blackbox `-race`). The public
    tag, release workflow, and public-gateway verification remain.

### Decision gate — after two weeks of field telemetry

Read `sessions_relayed_total` vs `sessions_direct_total` from real users (ask,
never collect). If the relayed cohort with both-side srflx is material, scope
G2 port prediction on top of the G10 harness; otherwise close it as
won't-fix-by-design.

## 5. Release acceptance checklist

- [x] Phase 1 items merged with tests — G1 closed as not-applicable with a
      measurement, G5, G7, G9 implemented
- [x] G4 wire format decided and shipped: `gateway=` plus `gateways=`, list in
      env and QR, relay and phone both fail over, seam pinned by a test that
      parses the verbatim shell-produced fragment
- [x] G10 netns NAT matrix built; 4/4 cells behave as designed
- [ ] Community gateway hostname final, VPS on ceilings build — runbook written
      (`docs/gateway-self-hosting.md`, "Running one for other people"); the
      hostname decision and the redeploy are still open and are yours
- [x] `make check` green including browser suites (Chromium + WebKit, 66 each)
- [x] Black-box hybrid suite green with `-race`
- [x] CHANGELOG cut to 0.17.0; design docs moved to `docs/design/`
- [ ] Fresh install from the tag on a clean machine pairs a phone through:
      Quick Start tunnel, community gateway, and an SSH-deployed private
      gateway
- [ ] 5G upgrade verified once against the released build (`clients` 1 → 0,
      pair `host/srflx`)

The public tag, release workflow, and public-gateway redeploy are the remaining
release steps.

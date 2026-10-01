# Resume timing and the resume benchmark

This page describes deliverable B1 of the always-on/fast-resume plan:
**measure before optimizing**. It adds local, privacy-bounded resume timing to
the phone app and a hosted, synthetic benchmark harness. It changes no
reconnect behaviour, timeout, dial order, E2EE handshake or authorization
rule, and it makes **no performance claim**. The last-known view (B2),
reconnect changes (B3) and Push/physical qualification (B4) are separate
deliverables.

## What the app measures

A **wake epoch** begins at the first observable wake signal:

| Trigger | Signal |
| --- | --- |
| `cold-start` | the app starts in a visible page (or the first visible event after a hidden start) |
| `visible` | `visibilitychange` to visible |
| `pageshow` | `pageshow` with `persisted` (back/forward cache restore) |
| `resume` | the page lifecycle `resume` event after a freeze |
| `network` | `online` or a `navigator.connection` change while visible and no epoch is collecting |

Signals within two seconds of an epoch's start, while it still has an
unfinished relay sample, or while device verification is pending, are
**coalesced** into it and recorded with their first offset (for example
`{ visible: 0, focus: 3, online: 40 }`). A focus event never opens an epoch on
its own; a network change after an epoch has finished opens a new one, so a
real network change while the app is in use is measured rather than
suppressed. `offline` is recorded as a hint but never opens an epoch. Hiding,
freezing or `pagehide` closes the epoch; an unfinished sample is then
`hidden` (abandoned), not a failure or a success.

Each relay contributes one **sample** per epoch. The epoch owns every dial,
retry, gateway fallback and replacement that follows, so a failed or
superseded attempt stays inside the same elapsed time instead of becoming a
separate success. Every connection attempt gets a generation number; the path
manager additionally scopes each gateway, legacy and direct attempt, and any
callback from an abandoned attempt is ignored. Counters record connection
attempts, raw path dials, supersessions, timeouts (handshake timers or the
2-second foreground probe) and failures.

A sample **succeeds** only when an `agents` snapshot arrives on the current
connection generation while the relay reports inventory `ready` and not
`stale`, that snapshot was requested after the wake (by a dial inside the
epoch, or by the post-wake probe on a reused connection), and its animation
frame renders, all within **60 seconds** of the epoch start. A fresh snapshot
after the deadline is recorded as `lateFreshAt` and the sample stays a
`deadline` non-completion. Other outcomes are `auth-rejected`,
`unlock-cancelled`, `hidden` and `removed`.

**Lifecycle category** per sample: `cold-launch` or `discarded`
(`document.wasDiscarded`) for a cold start, `bfcache` for a persisted
`pageshow`, otherwise `reconnect` when a dial happened in the epoch and `warm`
when the existing connection answered the probe.

### Phases

Offsets are milliseconds from the epoch start on the monotonic clock
(`performance.now()`), so wall-clock changes cannot distort them. Hidden
duration is the only wall-clock figure (a suspended page's monotonic clock may
not advance).

| Phase | Meaning |
| --- | --- |
| `probe`, `probe-answer` | post-wake refresh request on a reused connection and the first message after it |
| `dial` | the socket (or `RTCPeerConnection`) is created for this attempt |
| `open` | raw WebSocket open, or DataChannel open on the direct path |
| `gateway-hello`, `gateway-proof`, `gateway-ready` | gateway challenge received, answered, and the gateway ready |
| `e2ee-hello`, `e2ee-server-hello`, `e2ee-confirm`, `authenticated` | client hello sent, server hello received, client confirmation sent, server finish verified and the path ready |
| `first-frame` | first authenticated application message on the attempt |
| `inventory`, `rendered` | the fresh agents snapshot was published, and the frame that paints it ran |

Device unlock is recorded separately (`requestedAt`, `unlockedAt`,
`failedAt`), so the export reports unlock-inclusive time and
transport-eligible time (from the unlock) apart. Network events are hints
(`navigator.onLine` at the wake, first `online`/`offline`/`change` offsets);
none of them gates anything.

The **direct WebRTC upgrade** has its own timeline per sample (`dial`,
`offer`, `answer`, `ice-connected`, `open`, the E2EE phases, `promotedAt`,
attempts and failures). Inventory that first arrives over the relayed gateway
is labelled `gateway/relayed` even if the path is promoted later; a later wake
served by the promoted path is labelled `gateway/direct`.

### What is not observable

A web page cannot observe the operating system's wake before its first script
runs, or DNS, TCP and TLS inside a browser WebSocket. These are always
reported as `unavailable` (`os-wake-to-js`, `dns`, `tcp`, `tls`) and never as
zero or an estimate; `dial` to `open` is the composite. A cold start records
`navigationToAppMs`, navigation start to app start, which is observable. Phases
that do not apply are reported as `not_applicable`: gateway phases on WSS,
handshake phases for a reused warm connection, probes on a cold launch. The
direct path has no DNS/TLS/WebSocket phase; ICE replaces them. A snapshot that
was already in flight when the probe was sent can be counted as the probe's
answer; the probe-request rule bounds, but cannot remove, that ambiguity.

### Ingress labels come from the authenticated session

| Label | Source |
| --- | --- |
| `wss/ingress-unknown` | any WSS sample before an authenticated descriptor, from a relay without one, or on a plaintext loopback development session |
| `wss/cloudflare`, `wss/tailscale-managed`, `wss/tailscale-byo`, `wss/tailscale-cli` | the relay's `ingress` descriptor inside the authenticated `push_config` |
| `wss/other` | an authenticated descriptor for a non-WSS mode (for example a gateway relay reached on its legacy URL) |
| `gateway/relayed`, `gateway/direct` | the authenticated path the hybrid transport reports |

Hostnames are never used. The descriptor is additive
(`internal/protocol/protocol.go` `PushConfig.Ingress`, set by
`internal/app/server.go`); old apps ignore it and old relays omit it. It names
the configured mode, not proof of the route, and grants nothing.

## Privacy limits

- Memory only: at most 100 epochs and 24-hour logical retention (by both
  monotonic and wall clock; a wall clock that jumps backwards prunes rather
  than extends), at most 16 relay samples per epoch.
- No uploads, analytics, service-worker work, background timers or extra
  health requests. Measurement is passive.
- Recorded: path class, lifecycle, phase offsets, counters and outcome codes.
  Never recorded: hostnames, URLs, IP addresses, SDP or candidates, relay,
  device or pane identifiers, prompts, output, credentials, invitation
  fragments or raw error text. Relay identifiers exist only as keys of an
  in-memory tracking map used to scope callbacks, and are never copied into a
  sample or the export.
- Cleared on app teardown, on **Clear Resume Timings**, and when the user
  turns measurement off. The opt-out (`herdr_resume_metrics = off` in
  localStorage) is the only persisted value.

## Exporting a summary

Open **Settings → Resume Timing** and choose **Export Redacted Summary**. The
JSON appears in a read-only field and as a download link. Schema
`herdr-resume-summary/1` contains epoch and sample counts, trigger counts,
coalesced signal and network-hint counts, hidden-duration buckets, unlock
counts, and per path/lifecycle group: valid attempts, on-time completions,
non-completions by reason, abandoned and late counts, all-attempt
time-to-fresh p50/p95 (non-completions censored at 60 s; `insufficient` below
5 or 20 samples; `beyond-deadline` when the quantile is censored),
success-only and transport-eligible distributions, retry counters, phase
medians, `unavailable` and `not_applicable` phase lists, and direct-upgrade
counts. The card is a labelled region with a heading, a labelled switch, a
captioned table with row headers, and a polite live status line.

## Implementation map and deviations from the plan

| Plan file | Change |
| --- | --- |
| `frontend/src/lib/resume-metrics.ts` | new: wake epochs, attempt generations, ring, opt-out |
| `frontend/src/lib/resume-summary.ts` | new (not in the plan): redacted aggregates and labels, loaded only with the Settings card |
| `frontend/src/components/ResumeTimingSettings.svelte` | new (not in the plan): the Settings card, a lazy chunk |
| `frontend/src/components/SettingsView.svelte` | lazily loads the card |
| `frontend/src/lib/security.ts`, `store.ts` | passive hooks only; control flow and every existing call are unchanged |
| `frontend/src/lib/transports/types.ts`, `encrypted.ts`, `websocket.ts`, `gateway.ts`, `webrtc.ts`, `path-manager.ts`, `index.ts` | an optional `observe` callback in the existing `TransportAuthentication` options; observer exceptions are swallowed |
| `frontend/src/lib/types.ts` | unchanged: no shared view type needed a new field |
| `internal/protocol/protocol.go`, `internal/app/server.go` | additive `ingress` descriptor |
| `internal/protocol/protocol_test.go`, `internal/app/ingress_descriptor_test.go` | descriptor tests (a new server test file rather than growing `server_test.go`) |
| `frontend/tests/unit/resume-metrics.test.ts` | fake-clock unit tests and store wiring |
| `frontend/tests/unit/resume-benchmark-analysis.test.ts`, `resume-benchmark-runner.test.ts` | analyzer known cases; runner preregistration and matching (the runner test file is an addition) |
| `frontend/tests/browser/resume.spec.ts`, `resume-fixture.mjs` | Chromium and WebKit fixtures; the shared synthetic relay is an addition used by both the spec and the runner |
| `frontend/scripts/run-resume-benchmarks.mjs`, `analyze-resume-benchmarks.mjs` | hosted harness |

The Settings card and summary code load as one lazy chunk so the startup
payload stays inside the existing 165 KiB gzip budget, which was not raised.
No runtime or development dependency was added; the harness uses the pinned
`@playwright/test`, Node built-ins and WebCrypto.

## Hosted benchmark harness

`make resume-benchmark-pilot RESUME_BENCHMARK_SHA=<full sha>` runs
`frontend/scripts/run-resume-benchmarks.mjs` and then
`frontend/scripts/analyze-resume-benchmarks.mjs`. It is the separately named
`Resume benchmark pilot` job in `.github/workflows/check.yml`, outside
`make check`, and it uploads only sanitized JSON: the preregistration, every
attempted epoch, the analysis and a Markdown summary. Run it on disposable
hosted runners only.

The runner serves the shipped `web/` bundle with the repository's static
server and drives it in Chromium (Pixel 7 profile) and WebKit (iPhone 15
profile). `tests/browser/resume-fixture.mjs` replaces `WebSocket` and, when
needed, `RTCPeerConnection` in the page with a synthetic relay that answers
the real E2EE handshake with WebCrypto and the stored device credential, speaks
the gateway rendezvous and chunk framing, and changes its inventory text for
every wake. Scripted conditions:

| Scenario | Conditions |
| --- | --- |
| `warm-short` | hidden 200–800 ms, connection kept |
| `hidden-5m` | frozen page for 5 minutes (wall clock only), socket silently half-open |
| `blackhole-restore` | frozen 30 s, half-open socket, new dials stall until the network returns 1–4 s after the wake, then connect on a 1/3/7/15/31 s SYN retransmission schedule; `online` fires at restoration |
| `discard` | frozen 2 minutes, reload with `document.wasDiscarded`; measured from navigation start |

Fixture latencies are synthetic (8–30 ms per hop), visibility is emulated, and
there is no real radio, VPN, carrier, Cloudflare or Tailscale network. Results
describe the app's own scheduling and protocol phases under these scripts,
nothing more.

**Endpoint.** Success is the first animation frame that paints the active
epoch's authenticated, ready, non-stale inventory within 60 s of the first
visible event (navigation start for a discard). The page records the paint
itself, so a render that beats the harness call still counts from when it
happened. Everything else is a non-completion with its reason (`deadline`,
`late`, `harness-error`, ...), right-censored at 60 s.

**State reset and matching.** Every epoch, and in a paired design each
variant of a pair, runs in a fresh browser context with fresh storage and a
fresh synthetic relay. Both variants of a pair use the same seed, and so the
same hidden time, restoration delay and fixture latency stream; the order is
an independent seeded coin per pair.

**Preregistration.** Before the first epoch the runner writes
`preregistration.json` (its SHA-256 is bound into the evidence) with the
candidate and baseline SHAs, browser/transport/scenario strata and roles, seed
derivation, hidden, network-restoration and unlock schedules, sample size and
unit, endpoints, analysis method, bootstrap settings, family size, bounds,
harness-invalid criteria, the replacement limit and the negative controls.

**Harness-invalid exclusions** are objective and preregistered only:
`browser-crash`, `bundle-load-failed` (the bundle did not load with HTTP 200)
and `warmup-timeout` (the cold connection before the measured wake did not
render within 30 s). They exclude both variants of a pair, are counted with
reasons, and earn at most two same-seed replacements. Any other reason is a
non-completion. A candidate failure is never an exclusion.

**Negative controls** are safety assertions, never latency successes:
revoked credential (refusal, no fresh inventory, no redial loop), cancelled
unlock (locked, no dial), permanent outage over a 15 s window (no fresh
inventory, no crash), and iOS deferred pairing (deferral, no dial). A failed
control fails the job.

## Analysis method

All valid attempted epochs are in the denominators. Quantiles are nearest-rank
over all attempts with non-completions censored beyond the deadline; when a
quantile lands among censored values it is `beyond-deadline` and not
identifiable. Success-only percentiles are supplementary diagnostics only.

- **Pilot** (one variant): Wilson 95% interval for on-time completion,
  percentile-bootstrap 95% intervals for all-attempt p50/p95, success-only
  p50/p95, log-latency SD, dials/handshakes/bytes per epoch and hidden
  activity, and planning numbers for a confirmatory design.
- **Paired** (confirmatory): reliability uses Newcombe's hybrid score
  interval for the paired difference in non-completion rates (method 10);
  latency uses a paired percentile bootstrap of the all-attempt quantile ratio
  candidate/baseline, resampling independent pairs or declared blocks; a
  censored candidate replicate is +∞ and a censored baseline replicate is
  treated as +∞, so censoring only widens bounds. Every bound is one-sided at
  95% with Bonferroni family-wise control over every preregistered acceptance
  comparison (2 per targeted stratum, 3 per regression control), and the
  adjusted level, method and counts are retained in the output.

B3 decision rules, applied per stratum: the upper bound for
`candidate − baseline` non-completion must be at most +1 percentage point;
the targeted stratum's all-attempt p95 ratio upper bound at most 0.80;
regression controls' p50 and p95 ratio upper bounds at most 1.10. Fewer pairs
than planned (and never fewer than 400), a non-identifiable quantile or bound,
or wide intervals are `inconclusive`; an identifiable bound beyond its
threshold is `not-accepted`; an unsafe negative control rejects everything.
The analyzer tests include a candidate that is twice as fast when it succeeds
but fails 5% more often: it is `not-accepted`.

## Preregistration template and power assumptions for B3

Run `frontend/scripts/run-resume-benchmarks.mjs --design paired
--baseline-root <web of baseline> --candidate-root <web of candidate>
--baseline-sha <sha> --candidate-sha <sha> --samples <pairs>
--targets <stratum id>[,...]` with a new preregistration for each candidate.
Record before running:

- candidate and baseline SHAs, and web build hashes;
- strata (browser × transport × scenario), which are targeted and which are
  regression controls, so the family size is fixed (2 per target, 3 per
  control);
- base seed, hidden, restoration and unlock schedules;
- a fixed number of pairs per stratum chosen from the planning numbers below,
  never fewer than 400 and never extended after looking;
- endpoints, bounds, harness-invalid criteria and the replacement limit.

Planning assumptions, computed by `reliabilityPlanning` from a pilot's failure
rate `p`: with independent failures in both arms the discordance rate is
`2p(1−p)`, and a paired non-inferiority test at margin 1 point, one-sided
level `α/m` and 80% power needs about `(z₁₋α/m + z₀.₈)² · 2p(1−p) / 0.01²`
pairs. Even with zero failures, the Wilson bound only falls inside the margin
at `n ≥ z²(1−0.01)/0.01` pairs: 381 for a family of two, 498 for four.
Examples: with zero discordance, 400 pairs give an upper bound of 0.95
points at a family of two but 1.24 points at a family of four; a 10% pilot
failure rate implies about 14,128 pairs for one target. 400 is a floor, not a
guarantee of power. Latency precision has no closed form here; the pilot's
bootstrap interval widths and log-time SD indicate whether p95 will be
identifiable, and a stratum whose p95 is censored in the pilot cannot pass a
p95 criterion without first reducing its non-completions.

## Pilot baseline results

Pending the first hosted pilot run on this branch. The results will be
recorded here with the run ID, SHA and measured web build, and labelled as a
pilot: variance and workload estimates, not p95 acceptance.

## Limits of this evidence

Hosted headless browsers with an emulated page lifecycle and a synthetic relay
do not represent physical phones, real suspension, radios, VPNs or ingress
providers. Real Android, iOS, Cloudflare and Tailscale measurements are later,
separately authorized evidence (B4); small physical samples can describe
observed behaviour but cannot carry a hosted statistical bound. No mobile,
physical-device or live qualification is claimed here.

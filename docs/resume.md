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

A new signal joins (is **coalesced** into) the current epoch while that epoch
is still collecting: a relay sample is unfinished or device verification is
pending. A finished epoch absorbs only lifecycle duplicates (visible,
pageshow, resume, focus) within two seconds of its start. A network `online`
or `change` after every sample finished opens a new `network` epoch, even
half a second later, so the revalidation it causes is measured rather than
swallowed. Coalesced signals keep their first offset (for example
`{ visible: 0, focus: 3, online: 40 }`). A focus event never opens an epoch on
its own, and `offline` is recorded as a hint but never opens one. Hiding,
freezing or `pagehide` closes the epoch; an unfinished sample is then
`hidden` (abandoned), not a failure or a success.

When an epoch opens, every relay that can resume is **enrolled** with one
sample: configured, not waiting for Home Screen pairing, not refused, and
holding a credential or usable key. (The cold start enrols again once saved
relays are loaded.) A wake whose dial never starts because verification is
pending or was cancelled, or whose carried dial never reports a phase, still
ends with an outcome. The epoch owns every dial, retry, gateway fallback and
replacement that follows, so a failed or superseded attempt stays inside the
same elapsed time instead of becoming a separate success. Every connection
attempt gets a generation number; the path manager additionally scopes each
gateway, legacy and direct attempt, and any callback from an abandoned attempt
is ignored. Counters record connection attempts, raw path dials, path dials
that failed or timed out before becoming usable, supersessions, timeouts
(handshake timers, the 2-second foreground probe or an unanswered keepalive)
and connection failures. The first eight path dials of a sample are kept as
records (`websocket` or `gateway`, dial offset, last milestone reached, when it
became usable, and how it ended: `failed`, `timeout`, `superseded` or
`auth-rejected`), so a failed first gateway is not erased when a second
gateway or the legacy URL succeeds.

A sample **succeeds** only when an `agents` snapshot arrives on the current
connection generation and live route while the relay reports inventory
`ready` and not `stale`, that snapshot was requested after the wake (by a dial
inside the epoch, or by the post-wake probe on a reused connection), and its
animation frame paints while the app is unlocked, all within **60 seconds** of
the epoch start. If the connection closes, the transport falls back to
connecting, or a new path is dialled before that frame, the snapshot cannot
complete the sample and a later fresh snapshot is needed. Inventory painted
behind the device lock completes at the first frame after unlocking. A fresh
snapshot after the deadline is recorded as `lateFreshAt` and the sample stays
a `deadline` non-completion. Other outcomes are `auth-rejected`,
`unlock-cancelled`, `hidden` and `removed` (the relay was removed or stopped
being able to connect).

**Lifecycle category** per sample: `cold-launch` or `discarded`
(`document.wasDiscarded`) for a cold start, `bfcache` for a persisted
`pageshow`, otherwise `reconnect` when a dial happened (or was needed) in the
epoch and `warm` when the existing connection answered the probe.

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
none of them gates anything. Time to observed reachability is the `open` of
the attempt that served the sample (phases restart with each path dial, so
failed dials cannot contribute one), or `probe-answer` on a reused
connection.

The **direct WebRTC upgrade** has its own timeline per sample (`dial`,
`offer`, `answer`, `ice-connected`, `open`, the E2EE phases, `promotedAt`,
attempts and failures). Inventory that first arrives over the relayed gateway
is labelled `gateway/relayed` even if the path is promoted later; a later wake
served by the promoted path is labelled `gateway/direct`.

### What is not observable

A web page cannot observe the operating system's wake before its first script
runs, so `os-wake-to-js` is always `unavailable`. DNS, TCP and TLS happen
inside a browser WebSocket dial and expose no split: they are `unavailable`
(never zero or an estimate) for groups that dialled a relay URL or gateway,
with `dial` to `open` as the composite, and `not_applicable` for groups that
only reused a live connection, including the direct WebRTC path, where ICE
over the existing session replaces them (`direct.not_applicable` lists `dns`,
`tcp`, `tls`, `websocket-open` and the gateway phases). A cold start records
`navigationToAppMs`, navigation start to app start, which is observable. Other
phases that did not happen are `not_applicable`: gateway phases on WSS,
handshake phases for a reused warm connection, probes on a cold launch. A
snapshot that was already in flight when the probe was sent can be counted as
the probe's answer; the probe-request rule bounds, but cannot remove, that
ambiguity.

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
  than extends), at most 16 relay samples per epoch. When an epoch leaves the
  ring, every internal reference to it and its samples is dropped too.
- Per relay, the app keeps only live connection identity outside the ring
  (connection generation, path class, whether it authenticated, and its
  ingress descriptor), so the next wake on a healthy connection is labelled
  and sampled correctly after **Clear** or after measurement is switched back
  on. It holds no timing, is kept while measurement is off for that reason,
  and is dropped on app teardown and when a relay is removed.
- No uploads, analytics, service-worker work, background timers or extra
  health requests. Measurement is passive.
- Recorded: path class, lifecycle, phase offsets, counters and outcome codes.
  Never recorded: hostnames, URLs, IP addresses, SDP or candidates, relay,
  device or pane identifiers, prompts, output, credentials, invitation
  fragments or raw error text. Relay identifiers exist only as keys of an
  in-memory tracking map used to scope callbacks, and are never copied into a
  sample or the export.
- Timings are cleared on app teardown, on **Clear Resume Timings**, and when
  the user turns measurement off. The opt-out (`herdr_resume_metrics = off` in
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
success-only and transport-eligible distributions, retry counters (including
path dials and path failures), path-attempt outcome counts such as
`gateway:failed` or `websocket:served`, phase medians, `unavailable` and
`not_applicable` phase lists, and direct-upgrade counts. The card is a labelled region with a heading, a labelled switch, a
captioned table with row headers, and a polite live status line.

## Implementation map and deviations from the plan

| Plan file | Change |
| --- | --- |
| `frontend/src/lib/resume-metrics.ts` | new: wake epochs, attempt generations, ring, opt-out |
| `frontend/src/lib/resume-summary.ts` | new (not in the plan): redacted aggregates and labels, loaded only with the Settings card |
| `frontend/src/components/ResumeTimingSettings.svelte` | new (not in the plan): the Settings card, a lazy chunk |
| `frontend/src/components/SettingsView.svelte` | lazily loads the card |
| `frontend/src/lib/security.ts`, `store.ts` | passive hooks only (wake, lock state, enrolment, attempt, phase, connecting, retirement and inventory observations); control flow and every existing call are unchanged |
| `frontend/src/lib/transports/types.ts`, `encrypted.ts`, `websocket.ts`, `gateway.ts`, `webrtc.ts`, `path-manager.ts`, `index.ts` | an optional `observe` callback in the existing `TransportAuthentication` options, including a `failed` observation when a gateway or legacy path closes; observer exceptions are swallowed |
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

**Endpoint.** Success is the first animation frame that paints an agent card
(its accessible name, never a workspace label) naming the active epoch's
agents from a snapshot that the synthetic relay sent with `ready`, non-stale
inventory, on a session that is still the live authenticated path, with no
unlock dialog covering it, within 60 s of the first visible event (navigation
start for a discard). Every snapshot carries its own sequence number, and the
relay records each one's epoch, freshness and session, so the page can check
the rendered card against the relay's truth. A card from a stale snapshot, a
workspace-only refresh, or a session the relay is abandoning does not count;
if the session is retired before the frame paints, the render does not count
either. The page records the paint itself, so a render that beats the harness
call still counts from when it happened. Hosted browser tests exercise each of
these traps (`workspaceOnly`, `stale` and `abandonFirst` fixture faults).
Everything else is a non-completion with its reason (`deadline`, `late`,
`load-failed`, `warmup-failed`, `page-crash`, `browser-disconnected`,
`harness-error`), right-censored at 60 s.

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

**Harness-invalid exclusions** are objective, preregistered and established
outside the page under test: `server-unavailable` (after a failed page load,
an independent request from the runner to the local static server fails with
a network error rather than any HTTP status) and `browser-unavailable` (a
fresh browser context or page could not be created before the page was
opened). They exclude both variants of a pair, are counted with reasons, and
earn at most two same-seed replacements; a pair whose three rounds are all
excluded is reported as exhausted and leaves the sample short. A page crash, a
lost browser, a bundle that fails to load from a running server, and an app
that never renders its first inventory are attempted epochs that did not
complete (`page-crash`, `browser-disconnected`, `load-failed`,
`warmup-failed`), never exclusions. A lost browser is relaunched for the next
epoch.

**Negative controls** are safety assertions, never latency successes:
revoked credential (refusal, no fresh inventory, no redial loop), cancelled
unlock (locked, no dial), permanent outage over a 15 s window (no fresh
inventory, no crash), and iOS deferred pairing (deferral, no dial). Every
control runs for every browser and trial against **each variant's** bundle,
and each result names its variant. A failed, missing, duplicated or
unexpected control result fails the job; safe baseline controls never vouch
for a candidate.

## Analysis method

All valid attempted epochs are in the denominators. A `fresh` outcome counts
only with a finite, non-negative numeric time within the deadline; an absent,
null, string, negative or non-finite time is a `missing-measurement`
non-completion, never a guessed zero. Quantiles are nearest-rank over all
attempts with non-completions censored beyond the deadline; when a quantile
lands among censored values it is `beyond-deadline` and not identifiable.
Success-only percentiles are supplementary diagnostics only.

Before any statistic, evidence is checked against its preregistration. Only
preregistered strata and variants, pair indices `0 … n−1` for the fixed
sample size `n`, the preregistered seed for each pair, and replacement rounds
up to the limit count; a replacement round is admissible only after an
excluded round, and duplicate records are refused. Any violation, including
evidence collected beyond the preregistered sample, makes the stratum and the
experiment `invalid`: extra pairs cannot buy precision. Each pair uses its
first non-excluded round; a variant missing from that round is a
`missing-outcome` non-completion. A pilot meets its sample only when every
stratum has `n` measured, non-excluded epochs.

- **Pilot** (one variant): Wilson 95% interval for on-time completion,
  percentile-bootstrap 95% intervals for all-attempt p50/p95, success-only
  p50/p95, log-latency SD, dials/handshakes/bytes per epoch and hidden
  activity, and planning numbers for a confirmatory design.
- **Paired** (confirmatory): reliability uses Newcombe's hybrid score
  interval for the paired difference in non-completion rates (method 10);
  latency uses a paired percentile bootstrap of the all-attempt quantile ratio
  candidate/baseline, resampling independent pairs or declared blocks; a
  censored candidate quantile against a finite baseline is +∞, and a
  censored baseline quantile is unknown (+∞ for the upper bound, 0 for the
  lower bound), so censoring only widens bounds. Every bound is one-sided at
  95% with Bonferroni family-wise control over every preregistered acceptance
  comparison (2 per targeted stratum, 3 per regression control), and the
  adjusted level, method and counts are retained in the output.

B3 decision rules, applied per stratum: the upper bound for
`candidate − baseline` non-completion must be at most +1 percentage point;
the targeted stratum's all-attempt p95 ratio upper bound at most 0.80;
regression controls' p50 and p95 ratio upper bounds at most 1.10. Each
comparison is `pass` when its upper bound is within the threshold,
`not-accepted` when even its one-sided lower bound (at the same adjusted
level) is beyond it, a demonstrated violation, and `inconclusive` otherwise.
A precision-limited interval, fewer valid pairs than preregistered, a
preregistered sample below the 400-pair floor, or a non-identifiable
quantile is `inconclusive`, with the reason, never a rejection or a pass. For
example, a family of five with 400 pairs and no discordance has a
reliability upper bound of 1.33 points: inconclusive. A censored baseline
quantile is unknown and can demonstrate neither result. Every outcome other
than `pass` on every comparison keeps the baseline. An unsafe negative
control makes the experiment `not-accepted`; incomplete controls or
evidence outside the preregistration make it `invalid`. The analyzer tests
include a candidate that is twice as fast when it succeeds but fails 5% more
often: its loss is demonstrated and it is `not-accepted`.

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

**Pilot only: variance and workload estimates under scripted synthetic
conditions. This is not p95 acceptance and supports no performance claim.**

The figures below came from the first version of the harness, before review
tightened the endpoint (agent cards only, relay-verified freshness and live
path), the exclusion rules and the evidence validation. They are retained as
history and are superseded by the rerun on the revised harness, which will
replace this table.

Source: `check` run 36940357170, job *Resume benchmark pilot*, on commit
`005065acaafcebfd86be7c8e2024625cdb6cc9b3`, measuring the shipped `web/`
build `bc5f856f0c96e0c9007ec2f701cfefe00d5d53eb998a09b58dfdf0467a95afb5`
(later commits that change only documentation or tests ship the same build,
and each hosted run repeats the pilot on its own commit). Preregistration
SHA-256 `9d9b1dc4be9920869ca9ef946ccb2cb2cf8cb4009216986fa5d3495f3154cbe0`;
Chromium 151.0.7922.34 and WebKit 26.5 under Node v22.23.3 on a hosted Linux
runner; 10.7 minutes. 480 attempted epochs (16 strata × 30), all valid, no
harness exclusions or replacements; no hidden-time dials or bytes in any
stratum. All 24 negative-control trials (4 controls × 3 trials × 2 browsers)
were safe.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | Dials / handshakes per epoch | Mean bytes | SD of ln(ms) |
| --- | --- | --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 20 (18–22) | 29 (26–30) | 0 / 0 | 1,170 | 0.27 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 67 (54–79) | 98 (87–103) | 1 / 1 | 3,398 | 0.32 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,042 (2,074–3,072) | 5,104 (5,034–5,105) | 1 / 1 | 3,398 | 0.35 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 139 (125–141) | 191 (161–227) | 1 / 1 | 3,539 | 0.20 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 21 (16–26) | 32 (30–36) | 0 / 0 | 771 | 0.35 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 103 (84–117) | 149 (135–153) | 1 / 1 | 2,533 | 0.36 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,056 (2,097–3,148) | 5,125 (5,097–5,145) | 1 / 1 | 2,676 | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 143 (134–161) | 198 (181–229) | 1 / 1 | 2,795 | 0.22 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 34 (31–37) | 50 (43–210) | 0 / 0 | 1,170 | 0.37 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 100 (86–103) | 131 (116–131) | 1 / 1 | 3,503 | 0.21 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,069 (2,099–3,110) | 5,117 (5,076–5,130) | 1 / 1 | 3,782 | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 185 (168–201) | 249 (216–266) | 1 / 1 | 3,388 | 0.17 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 36 (33–39) | 49 (46–52) | 0 / 0 | 771 | 0.19 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 116 (101–131) | 148 (147–164) | 1 / 1 | 2,561 | 0.23 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,084 (2,147–3,112) | 5,120 (5,084–5,129) | 1 / 1 | 2,860 | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 198 (183–201) | 247 (231–248) | 1 / 1 | 2,742 | 0.14 |

Reading it as a pilot:

- Under these scripts every attempted epoch completed in time, but 30
  attempts without a failure only bound the failure rate below about 11.4%
  (two-sided Wilson). Planning a reliability comparison at that pessimistic
  rate needs about 15,800 pairs for one target; at an assumed 0% it is
  precision-limited to at least 381 pairs (family of two) or 498 (four). A
  confirmatory design must state which assumption it uses, or first run a
  larger pilot.
- A nearest-rank p95 of 30 values is essentially the second-largest value, so
  the p95 intervals are coarse; the log-time SDs (0.14–0.40) are the variance
  inputs for planning latency precision.
- `blackhole-restore` is dominated by the scripted conditions: the 2-second
  foreground probe timeout, then a dial that waits for restoration and the
  1/3 s SYN retransmission steps. `hidden-5m` redials at once because five
  minutes of silence exceed the keepalive freshness bound, so it skips the
  probe. `warm-short` reuses the connection with no dial or handshake.
- Synthetic fixture latency, emulated suspension and headless browsers make
  these figures unsuitable for comparison with phones or real networks.

An earlier run (36938672837 on `da19f2591c0f90a1c102833004acdc1ef0cf139d`)
exposed two harness defects, both fixed before the run above and recorded here
so its numbers are not reused: every `discard` epoch was a harness error
because the reload landed on the bootstrap redirect, and killing sockets after
advancing the frozen clock let an in-flight reply mark a dead path as recently
active, which put a 2-second probe timeout into some `hidden-5m` epochs.

## Limits of this evidence

Hosted headless browsers with an emulated page lifecycle and a synthetic relay
do not represent physical phones, real suspension, radios, VPNs or ingress
providers. Real Android, iOS, Cloudflare and Tailscale measurements are later,
separately authorized evidence (B4); small physical samples can describe
observed behaviour but cannot carry a hosted statistical bound. No mobile,
physical-device or live qualification is claimed here.

# Resume timing and the resume benchmark

This page describes deliverable B1 of the always-on/fast-resume plan:
**measure before optimizing**. It adds local, privacy-bounded resume timing to
the phone app and a hosted, synthetic benchmark harness. It changes no
reconnect behaviour, timeout, dial order, E2EE handshake or authorization
rule, and it makes **no performance claim**. The last-known view (B2),
reconnect changes (B3) and Push/physical qualification (B4) are separate
deliverables.

## B2 candidate: last-known is not fresh completion

The source candidate adds an optional encrypted, tab-scoped, dated read-only summary
and an independent action-freshness guard. Persistence defaults off and does not
control action authority. Summary rows never receive a live-agent benchmark selector
and cannot satisfy the fresh-inventory endpoint. First-known render is descriptive
only, never an on-time fresh completion. The 60-second deadline, all-attempt
accounting and conservative occlusion rules remain unchanged.

Action freshness uses an authenticated random request nonce and one atomic response
from a poll started after admission. An already-in-flight poll, handshake, ping,
activity, workspace-only frame or uncorrelated legacy inventory cannot establish it.
Connection/path replacement, wake, lock, refusal or unready/stale inventory invalidate
it; workspace freshness is separate. Metrics observe this boundary, not grant it.
Unsupported relays fail closed rather than falling back to B1's timing heuristic.

The summary retains only bounded labels, coarse type/status and local last-fresh time;
labels may be sensitive. It expires within 60 minutes, uses encrypted sessionStorage
only, is removed from DOM/current references on lock and is invalidated by Forget,
opt-out, removal, credential change or observed refusal. Tab restoration may retain
it; fresh Home Screen sessions, tab closure or eviction may miss. Read the consent,
bounds and threat-model limits in [security.md](security.md#opt-in-last-known-summaries).
There is no real-device acceleration or B4 qualification claim.

The pre-review B1 status/evidence passages below are historical; the roadmap records
B1 approved at `1d545fc56dd8f0570f0e1157da3f3cd21c1ff00a` (closeout run `1a77291c`).
This candidate's B2 hosted acceptance and independent approval remain outstanding;
that B1 closeout does not approve B2 or statistical/physical confirmation.

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
connection generation and live path while the relay reports inventory
`ready` and not `stale`, that snapshot was requested after the wake (by a dial
inside the epoch, or by the post-wake probe on a reused connection), and its
animation frame shows a **visible** inventory view (the agent list, or the
agent rail beside a terminal) while the app is unlocked, all within
**60 seconds** of the epoch start.

Each fresh snapshot is its own pending object, which its frame callback (and
any later retry) holds. A callback acts only if its object is still the
relay's current pending snapshot; otherwise it returns without changing
anything, so an obsolete frame can never complete, alter or erase a newer
snapshot or a finished sample. The pending snapshot is withdrawn at once,
together with its `inventory` and `rendered` offsets (never other phases,
and never from a finished sample), when the connection is replaced or ends,
a new path is dialled, the transport falls back to connecting, it reports a
different path (a direct WebRTC promotion, or a fallback to the gateway), or
the relay reports its inventory not ready or stale. A later fresh snapshot
on the current path is then accepted at once, even while the obsolete frame
is still queued. A newer fresh snapshot that arrives before the frame
replaces the pending one, because that frame shows the newer one; the
`inventory` offset is the arrival of the snapshot actually shown. Pending
snapshots are detached when a new wake replaces the epoch, when retention
prunes it, and on Clear, opt-out and teardown; the replaced wake keeps what
it had recorded.

A view counts only if it is shown: the agent list and the rail each
register a check that their root element is connected, laid out with a
non-zero size, CSS-visible and on a visible page, evaluated at the frame
itself, after Svelte has committed the DOM. An empty authoritative inventory
still counts, since the list is shown. The rail is hidden by CSS below 900 px
(a phone terminal), so it registers only while `(min-width: 900px)` matches,
follows that media query's change events, and is still checked at the frame,
so a resize just before the frame cannot pass. Fresh inventory published
while no visible view exists (Settings open, or a phone terminal) waits, and
the same snapshot is retried at the first frame after a visible view
registers, if it is still current; inventory painted behind the device lock
completes at the first frame after unlocking. No polling or timer is
involved. Callbacks that arrive after their connection ended,
such as a handshake promise that settles after the socket closed, are
ignored; the encrypted transport also neither sends nor reports a client
hello once its attempt has closed. A fresh snapshot after the deadline is
recorded as `lateFreshAt` and the sample stays a `deadline` non-completion.
Other outcomes are `auth-rejected`, `unlock-cancelled`, `hidden` and
`removed` (the relay was removed or stopped being able to connect).

**Lifecycle category** per sample: `cold-launch` or `discarded`
(`document.wasDiscarded`) for a cold start, `bfcache` for a persisted
`pageshow`, otherwise `reconnect` when a dial happened (or was needed) in the
epoch and `warm` when the existing connection answered the probe. A
persisted `pageshow` that joins an epoch opened by `resume` or `visible`
(restores can report them in either order) turns that epoch and its samples
into `bfcache`, keeping the original start and deadline.

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
ambiguity in the in-app metric. The hosted benchmark does not depend on it:
its endpoint uses the synthetic relay's record of which snapshot belongs to
which wake, and the runner and the browser tests that assert in-app outcomes
idle for 500 ms after the cold connection's own refresh before hiding.

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
`not_applicable` phase lists, and direct-upgrade counts. The card is a
labelled region with a heading, a labelled switch, a captioned table with row
headers, and a polite live status line.

Deadlines and retention are applied whenever the ring is read, and time
passing changes them without any event. The card therefore re-reads the ring
on every recorded change, at the moment of export (so an export never carries
a stale in-progress sample or a wake outside the 24-hour window), and once
more when the open wake's deadline or the oldest wake's retention expiry is
due while the card is open. That single timer exists only while Settings
shows the card; nothing runs in the background otherwise.

## Implementation map and deviations from the plan

| Plan file | Change |
| --- | --- |
| `frontend/src/lib/resume-metrics.ts` | new: wake epochs, attempt generations, ring, opt-out |
| `frontend/src/lib/resume-summary.ts` | new (not in the plan): redacted aggregates and labels, loaded only with the Settings card |
| `frontend/src/components/ResumeTimingSettings.svelte` | new (not in the plan): the Settings card, a lazy chunk |
| `frontend/src/components/SettingsView.svelte` | lazily loads the card |
| `frontend/src/components/AgentList.svelte`, `AgentRail.svelte` | not in the plan: register a visibility check of the inventory view's root (the rail only while `(min-width: 900px)` matches), so a sample completes only in a frame that shows inventory |
| `frontend/tests/unit/resume-timing-settings.test.ts` | addition: the card's export and deadline/retention refresh with an advanced clock |
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
| `hidden-30s` | frozen page for 30 s (wall clock only), connection still healthy, so the app's probe keeps it |
| `hidden-5m` | frozen page for 5 minutes, socket silently half-open |
| `hidden-long` | frozen page for 2 hours, past the one-hour hidden keepalive bound, socket silently half-open |
| `blackhole-restore` | frozen 30 s, half-open socket, new dials stall until the network returns 1–4 s after the wake, then connect on a 1/3/7/15/31 s SYN retransmission schedule; `online` fires at restoration |
| `network-change` | page stays visible; the path goes silently half-open and 200–400 ms later `navigator.connection` reports a change, which starts the measured epoch (`online` stays true; WebKit, which has no `navigator.connection`, gets a fixture event target) |
| `discard` | frozen 2 minutes, reload with `document.wasDiscarded`; measured from navigation start |

Paths (`--transports`): `wss-cloudflare` and `wss-tailscale` (direct WSS
whose authenticated descriptor says Cloudflare or managed Tailscale Serve),
`gateway-relayed` (direct upgrade disabled) and `gateway-direct` (the measured
wake starts once the direct WebRTC path has been promoted; a run that never
promotes is a `warmup-failed` attempt). For `gateway-direct` epochs the runner
also keeps the **direct upgrade** as a separate, bounded observation. The
fixture opens a 30-second window at the wake itself and closes it on its own
timer; the observation covers the wake up to the first promotion or the
window's end, whichever is first, however early or late the primary outcome
is recorded. Once the primary outcome is final the runner waits for that
window (returning at once if it is already over) and stores
`direct_upgrade`: the outcome (`promoted` inside the window; `not-promoted`
when direct attempts started inside it and none was promoted; `stayed-direct`
when nothing was attempted and the direct session in use at the wake was
still the live path when the window closed, decided at that moment, so a
later loss does not change it; `not-attempted`; or `unobserved` if the window
never closed or the page could no longer be read), the number of direct
attempts and refused offers inside the window, and the relay-side offsets
from the wake of the first offer, answer, DataChannel open, direct E2EE
authentication and promotion inside it. Attempts, refusals and milestones
after the window are ignored: an upgrade that only starts after it, even
with a primary outcome recorded much later, is `not-attempted`. The
primary 60-second deadline and outcome are unaffected, and the analyzer
reports these as supplementary descriptive counts and promotion times only.
Other paths record `not-applicable`. The synthetic DataChannel exercises the
app's signalling, E2EE and promotion, not real ICE, NAT or radio behaviour.
Every workload and path can be
selected for a preregistered design with `--scenarios` and `--transports`.
The default pilot stays bounded to `warm-short`, `hidden-5m`,
`blackhole-restore` and `discard` on `wss-cloudflare` and `gateway-relayed`
(16 strata), and the hosted browser suite runs one seeded epoch of every
workload × path through the runner's own epoch code in both browsers, so each
selectable stratum is shown to work without enlarging the pilot.

Fixture latencies are synthetic (8–30 ms per hop), visibility is emulated, and
there is no real radio, VPN, carrier, Cloudflare or Tailscale network. Results
describe the app's own scheduling and protocol phases under these scripts,
nothing more.

**Endpoint.** Success is the first animation frame that paints an agent card's
actual marker-bearing inventory text (not merely its accessible name,
background or logo, and never a workspace label) naming the active epoch's
agents from a snapshot that the synthetic relay sent with `ready`, non-stale
inventory, on a session that is still the live authenticated path and the one
the app is currently using, with no unlock dialog covering it, within 60 s of
the first visible event (navigation start for a discard, the change event
for a network handoff). The relay knows which session is current from the
order in which it handed each session's first application message to the
app: the hybrid transport promotes a direct path on exactly that message and
falls back to the gateway when direct dies, so the latest still-live session
is the path in use. A gateway draining during the ten-second stability window
after a promotion is open but not current, and its cards do not count. Every snapshot carries its own sequence number, and the
relay records each one's epoch, freshness and session, so the page can check
the rendered card against the relay's truth. A card from a stale snapshot, a
workspace-only refresh, or a session the relay is abandoning does not count;
if the session is retired before the frame paints, the render does not count
either. The frame re-reads what it actually paints, so a newer fresh snapshot
that replaced the first one before the paint (a reconnect's initial snapshot
followed by the answer to the app's own refresh) counts at that frame. The
page records the paint itself, so a render that beats the harness call still
counts from when it happened. Hosted browser tests exercise each of these
traps (`workspaceOnly`, `stale` and `abandonFirst` fixture faults), the
replacement case (`burst`), and a direct promotion landing between render and
paint (`holdPaintUntilDirect`, a test-only widening of that window that holds
the paint check until a direct session is selected): the gateway's card is
rejected as `not-current-path` and only the direct path's snapshot counts.
Presentation is checked both before scheduling and at the completion frame.
The accessible name binds the relay record, but the same marker must also
occur in actual text inside `.agent-open`. The synthetic snapshot puts it
first in `cwd`, which compact phone cards visibly show, retaining the private
path canary. A text range scopes layout, viewport/clipping checks and exposed
hit-tested area to the complete marker, not the article rectangle. It needs
non-zero font/layout, a visible page and painted glyph color/text fill.
Effective styles from the text parent through the card to the root must not
hide it with `display:none`, hidden visibility, zero opacity or
`content-visibility:hidden`. Filtered/blended inventory and masks are
conservatively ineligible, including zero-opacity filters in percentage or
combined forms. DOM presence and an accessible name alone are not a paint.

**Conservative composition eligibility.** Pointer hit-testing is only an
additional geometry check, not proof of visual exposure: it ignores
pointer-transparent paint and shadows. Independently, the harness refuses
unsupported generated `::before`/`::after` boxes, pointer-transparent layers,
shadows/outlines, compositing effects, active top layers and shadow/custom or
replaced-content boundaries. The shipped in-flow disclosure arrow and small
status-dot ring have narrowly checked, disjoint bounds. Empty non-replaced
block/inline `span`/`div` leaves with only bounded background/border paint can
be pointer-transparent only when disjoint from the complete marker (not
list-item or CSS-content replacements); the shipped
nav-update badge therefore does not block inventory. Full-cover pseudo-boxes
are refused regardless of pointer targeting, as are repositioned disclosure
boxes and unbounded shadows. This is a deliberately limited supported
composition, not a general pixel/compositor oracle. Unknown compositions stay
in the attempted denominator as non-completions; there is no exclusion,
deadline restart or alteration of the measured page's pointer styles.
A separate hosted two-engine baseline preflight checks compatibility before
the full suite and pilot; those functional checks are not pilot epochs.
The marker-bearing Text node's parent must also be a **leaf element**.
Arbitrary descendants are unsupported even if empty, disjoint or
pointer-targetable: their ink cannot be inferred from their layout rectangles.
In particular, an opaque absolute child can cover the complete marker while
leaving its Text/range intact. A descendant hit is never evidence of exposed
text; the additional hit test requires the leaf parent itself. The declared
baseline marker spans remain eligible. This conservative refusal changes only
the harness, not shipped frontend bytes or measured pointer styles.
Fixed diagnostic codes contain no DOM labels, styles, URLs or identifiers.
Mutation, visibility, resize, scroll and completed CSS-transition/animation
changes retry eligibility; a permanently hidden card creates no frame retry
loop. A reveal counts only its later qualifying frame, retaining the original
wake and fixed 60-second deadline. A queued frame from an abandoned epoch
cannot count for a later one. Chromium and WebKit regressions cover these
presentation traps, hiding between scheduling and paint, and a reveal after
the deadline. Test styles are served from a synthetic same-origin URL,
without relaxing the shipped Content Security Policy.
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
- **Paired** (confirmatory): reliability uses Tango's asymptotic score
  interval (1998) for the paired difference in non-completion rates, one of
  the paired-proportion intervals Fagerland, Lydersen and Laake recommend.
  It depends only on the discordant pairs and the number of pairs: how the
  concordant pairs split between both-failed and both-completed carries no
  information about the difference and cannot narrow the bound. With no
  discordant pair the bound is ±z²/(n + z²) however many pairs failed in
  both arms (an earlier Newcombe hybrid-score interval estimated the
  correlation from the observed table and collapsed to zero width for, say,
  200 both-failed and 200 both-completed pairs), and with no baseline-only
  failure its upper bound is the Wilson upper bound of the candidate-only
  rate. The analyzer tests compute its exact miss probability over every
  outcome at rare discordance with mixed concordant outcomes (for example a
  true 0.5-point loss with half the pairs failing in both arms, where it is
  never missed). Latency uses a paired percentile bootstrap of the all-attempt quantile ratio
  candidate/baseline, resampling matched pairs. The matched pair is the only
  independent unit (each pair runs in fresh browser contexts with its own
  preregistered seed); evidence that labels records with a `block` or
  `cluster`, or a registration that declares blocks or clusters, is
  `invalid`, because neither interval would be valid for clustered data. A
  censored candidate quantile is only known to be at least the 60-second
  deadline: +∞ for the upper bound, and the deadline itself for the lower
  bound. A censored baseline quantile is unknown (+∞ for the upper bound, 0
  for the lower bound). Censoring therefore only widens bounds: a censored
  comparison is never identifiable and never passes, and it is rejected only
  when even the censoring-aware lower bound exceeds the threshold (for
  example a candidate p95 of at least 60 s against a 1 s baseline, but not
  against a 59 s baseline, where a true 61 s would still be a ratio of 1.03).
  Every bound is one-sided at 95% with Bonferroni family-wise control over
  every preregistered acceptance comparison (2 per targeted stratum, 3 per
  regression control), and the adjusted level, method and counts are
  retained in the output.

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
evidence outside the preregistration make it `invalid`.

The registration itself must stay inside the approved contract, or the
analyzer reports `invalid` (and a pilot does not meet its sample) rather than
analyse it under relaxed rules: `deadline_ms` exactly 60,000 (the analyzer
always classifies at 60 s), bounds no laxer than the defaults (a margin above
1 point, a target ratio above 0.80, a regression ratio above 1.10, fewer than
400 minimum pairs, alpha above 0.05 or power below 0.8 are refused; stricter
values are allowed and used), harness exclusions limited to
`server-unavailable` and `browser-unavailable`, all four negative controls
declared with at least one trial each, and at least 30 epochs per pilot
stratum. The violations are listed in `contract_violations`. The analyzer tests
include a candidate that is twice as fast when it succeeds but fails 5% more
often: its loss is demonstrated and it is `not-accepted`.

## Preregistration template and power assumptions for B3

Run `frontend/scripts/run-resume-benchmarks.mjs --design paired
--baseline-root <web of baseline> --candidate-root <web of candidate>
--baseline-sha <sha> --candidate-sha <sha> --samples <pairs>
--targets <stratum id>[,...]` with a new preregistration for each candidate.
Record before running:

- candidate and baseline SHAs, and web build hashes;
- strata (browser × transport × scenario, chosen from the seven workloads
  and four paths above with `--browsers`, `--transports` and `--scenarios`),
  which are targeted and which are regression controls, so the family size
  is fixed (2 per target, 3 per control); regression controls should cover
  every transport;
- base seed, hidden, restoration and unlock schedules;
- a fixed number of pairs per stratum chosen from the planning numbers below,
  never fewer than 400 and never extended after looking;
- endpoints, bounds, harness-invalid criteria and the replacement limit.

Planning assumptions, computed by `reliabilityPlanning` from a pilot's failure
rate `p`: with independent failures in both arms the discordance rate is
`2p(1−p)`, and a paired non-inferiority test at margin 1 point, one-sided
level `α/m` and 80% power needs about `(z₁₋α/m + z₀.₈)² · 2p(1−p) / 0.01²`
pairs. Even with zero discordant pairs (however many fail in both arms), the
score bound z²/(n + z²) only falls inside the margin at `n ≥ z²(1−0.01)/0.01`
pairs: 381 for a family of two, 498 for four.
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

### Descendant-composition repair pilot — descriptive, not acceptance

Source `4e4a38f2e79a11d079b4b2bb1f6d21e7b8c82bef`: `check`
**37029148501**, attempt 1, pilot job **110911354476**; native preflight
**37029147952**, attempt 1, both completed successfully. The harness now
refuses non-leaf marker-text parents rather than inferring glyph exposure
from a covering descendant. Shipping `web/` remains
`a562bac854196a7a231a2ff999ccd9d9436727ec432bdbdfca4809e8e1ae6e05`;
no shipped bundle or runtime behaviour changed.
Preregistration binding (compact JSON):
`fb75b0b7dc43e199fa30c07ad193c1e94d896045dac730544b5c16b1d1512e5a`.
Chromium 151.0.7922.34, WebKit 26.5, Node v22.23.3, hosted Linux;
643,125 ms (10.7 minutes). All **480 attempted epochs** (16×30) are retained,
all on time, with no exclusions, replacements, missing/exhausted outcomes,
contract/evidence violations or hidden activity; all **24/24** baseline
negative controls are safe. This one-variant pilot is not a comparison;
paired controls against both variants remain required by the runner/analyzer.
No numbers are pooled with historical pilots.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | SD of ln(ms) |
| --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 24 (22–28) | 32 (30–34) | 0.21 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 71 (60–81) | 101 (89–105) | 0.29 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3053 (2080–3078) | 5100 (5040–5112) | 0.34 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 138 (124–151) | 203 (167–211) | 0.21 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 26 (21–31) | 39 (36–39) | 0.31 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 109 (91–129) | 158 (140–162) | 0.36 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3048 (2100–3140) | 5131 (5100–5147) | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 156 (130–171) | 208 (191–209) | 0.21 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 37 (34–39) | 51 (45–54) | 0.17 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 101 (85–116) | 135 (119–137) | 0.22 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3072 (2102–3115) | 5121 (5081–5131) | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 192 (175–217) | 234 (220–243) | 0.13 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 38 (37–41) | 52 (47–55) | 0.17 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 118 (103–134) | 151 (149–167) | 0.23 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3086 (2150–3115) | 5123 (5086–5131) | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 202 (184–217) | 239 (233–248) | 0.13 |

Thirty on-time outcomes still only bound failure below about 11.4% (two-sided
Wilson). These coarse p95/log-time figures are workload/variance inputs,
**never p95 acceptance or superiority**. The same source run passed 649 unit,
478 browser (156 presentation, 78/engine) and four attention tests, with no
browser retries/flaky cases; the separate two-engine baseline preflight passed.
The 12 added cases cover ordinary opaque targetable children before arrival
and while a frame is held, prove the old descendant hit despite covering
marker ranges, require later reveal, retain original-deadline censoring and
isolate an abandoned frame; baseline leaf text remains eligible.
The default pilot does not sample direct upgrades; all other selectable
workloads/paths remain covered separately by the browser suite. Mobile jobs
were skipped, not qualified. Final docs-only SHA CI/evidence is retained
privately rather than recursively changing this source-pilot citation.
**B1 remains unapproved pending the one authorized independent review.**
Adequately powered preregistered ≥400-pair B3 confirmation and separately
authorized B4/physical/live qualification remain future.

### Historical composition pilot — void for endpoint acceptance

Independent review `df8f3015` on `baf7681d` left original
`807dcb5c-6293-4de7-a330-12c899ac3380:F022` and successor
`4be2b3b7-7383-40dd-85d4-1e060f314bb6:F001` OPEN: opaque ordinary child
spans cover marker text but `parent.contains(elementFromPoint(...))`
falsely accepts them. That counterexample was source-derived, not executed by
the reviewer. Pilots **37013311295** on `c7879756` and **37017426942** on
`baf7681d` are therefore **void for B1 endpoint acceptance**. Their 480 on-time
attempts each, 24 safe controls and passing suites remain historical facts,
not current eligibility proof; none are pooled with the repaired pilot.
The same review independently resolved original
`807dcb5c-6293-4de7-a330-12c899ac3380:F023` on `baf7681d`; that disposition
requires fresh exact-new-SHA evidence, not automatic inheritance.
Eight prior reviews remain consumed. The owner authorized one scoped repair,
hosted verification and one further independent review; no post-negative
repair or extra review is authorized. B1 remains unapproved pending that review.

The historical cited repair pilot is `check` run **37013311295**, attempt 1,
job *Resume benchmark pilot*, on
`c787975676a2167ca80e99be31ef13d86c6689e5`, measuring unchanged shipped `web/`
`a562bac854196a7a231a2ff999ccd9d9436727ec432bdbdfca4809e8e1ae6e05`.
Only harness/tests/preregistration and hosted preflight wiring changed.
Preregistration binding (compact JSON):
`d711ca4dee5f77aea824e5f824e09a656e807a69e37e698824b2d40fcd7a540c`.
Chromium 151.0.7922.34, WebKit 26.5, Node v22.23.3, hosted Linux;
10.7 minutes. All **480 attempted epochs** (16×30) are retained, all on time;
no non-completions, exclusions, replacements, missing/exhausted outcomes,
contract/evidence violations or hidden activity. All 24/24 negative controls
were safe. No direct-upgrade stratum is sampled in this bounded pilot; other
selectable workloads/paths remain separately exercised by the browser suite.
Numbers are not pooled or compared with older harness pilots.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | Dials / handshakes per epoch | Mean bytes | SD of ln(ms) |
| --- | --- | --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 24 (22–27) | 34 (30–45) | 0 / 0 | 1,198 | 0.24 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 72 (61–82) | 102 (90–106) | 1 / 1 | 3,658 | 0.29 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,058 (2,084–3,080) | 5,101 (5,041–5,115) | 1 / 1 | 3,694 | 0.34 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 141 (122–145) | 185 (165–189) | 1 / 1 | 3,694 | 0.21 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 24 (22–31) | 37 (36–42) | 0 / 0 | 792 | 0.32 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 103 (91–128) | 160 (139–165) | 1 / 1 | 2,782 | 0.34 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,049 (2,100–3,149) | 5,137 (5,102–5,150) | 1 / 1 | 2,953 | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 148 (137–172) | 245 (191–273) | 1 / 1 | 2,978 | 0.23 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 35 (34–39) | 50 (44–51) | 0 / 0 | 1,198 | 0.17 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 102 (87–117) | 135 (119–139) | 1 / 1 | 3,582 | 0.22 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,071 (2,103–3,115) | 5,119 (5,077–5,131) | 1 / 1 | 3,881 | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 190 (175–204) | 253 (231–260) | 1 / 1 | 3,345 | 0.15 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 39 (34–42) | 52 (47–56) | 0 / 0 | 792 | 0.18 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 118 (103–133) | 151 (149–166) | 1 / 1 | 2,646 | 0.23 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,087 (2,149–3,114) | 5,122 (5,086–5,131) | 1 / 1 | 2,922 | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 200 (183–204) | 235 (231–252) | 1 / 1 | 2,757 | 0.14 |

Thirty successes only bound failure below about 11.4% (two-sided Wilson).
These coarse p95/log-time estimates are planning inputs, never acceptance.
The same run passed 649 unit tests, 460 browser tests (including 138
presentation cases, 40 added this remediation) and four attention tests,
without browser retries; the separate two-engine preflights also passed.
Native preflight 37013310435 attempt 1 passed on the same SHA. Mobile
harness/device jobs were skipped, not qualified. Final documentation SHA
requires its own retained hosted evidence, not a recursively changed citation.

**Historical status before the eighth review:** B1 remained unapproved.
Five historical plus both authorized successor review rounds were consumed. The last independent review left original
`807dcb5c:F022`/successor F001 open on `f7f71cf6`; this subsequent repair is
unreviewed under the exhausted allowance. Green tests/pilot cannot resolve
that finding or constitute approval. Original `807dcb5c:F023` was independently
resolved on reviewed `f7f71cf6`, not automatically on later source. B3 adequately
powered ≥400-pair confirmation and separately authorized physical/mobile/live
qualification remain future requirements.

Final-remediation failures are retained, not relabelled passing: ordinary
37009193750 on `5ec530fa` was cancelled after frontend/pilot timeouts while
the first conservative predicate refused default pointer-transparent paint;
37011871186 on `289556c4` failed with `No tests found` from an over-anchored
preflight selector; 37012362996 on `98c1d8ad` ran both engines and failed
preflight with `composition:pointer-transparent`. The bounded empty,
non-overlapping leaf rule in `c7879756` admits the default nav-update badge
without permitting full-cover layers. A one-error local type check also failed
and was corrected before that commit. Earlier failure history remains below.

### Historical marker-text pilot — void for endpoint acceptance

Successor review 2 on `f7f71cf6` found that pointer hit-testing admitted
fully painted, pointer-transparent overlays. Pilots 37002392165 and its
final-documentation repeat 37003999674 are **void for B1 endpoint acceptance**.
Their passing 98 presentation cases and 480 on-time epochs are retained facts,
not general presentation proof; the former table below is historical only.

The former marker-text-checked pilot was `check` run **37002392165**, attempt
1, job *Resume benchmark pilot*, on
`5d6bd88a258564681d4961ce74f72764e1d55b26`. It measures the unchanged shipped
`web/` build
`a562bac854196a7a231a2ff999ccd9d9436727ec432bdbdfca4809e8e1ae6e05`;
only the harness/tests/preregistration changed, not shipping frontend bytes.
The fixture now includes its marker in the visible inventory path; these
workload estimates are not pooled or compared with older harness pilots.
Preregistration SHA-256 (compact JSON, as bound into evidence):
`6c754671f72781d58ff1350be60603bea1249f91e1e2c395eef863dc7a0bd744`.
Chromium 151.0.7922.34 and WebKit 26.5, Node v22.23.3, hosted Linux;
10.6 minutes. All **480 attempted epochs** (16 selected strata × 30) are
retained: 480 on time, no non-completions, exclusions, replacements, missing
outcomes or contract/evidence violations. No hidden-time dials or bytes.
All 24/24 negative-control trials were safe, with none missing or duplicated.
The default pilot has no direct-upgrade strata; those and the other selectable
workloads remain separately exercised by the browser suite, not pooled here.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | Dials / handshakes per epoch | Mean bytes | SD of ln(ms) |
| --- | --- | --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 22 (20–24) | 32 (27–33) | 0 / 0 | 1,198 | 0.25 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 72 (56–80) | 99 (90–100) | 1 / 1 | 3,477 | 0.29 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,050 (2,080–3,083) | 5,105 (5,039–5,106) | 1 / 1 | 3,513 | 0.34 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 135 (120–139) | 169 (152–205) | 1 / 1 | 3,694 | 0.20 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 23 (16–27) | 34 (31–34) | 0 / 0 | 792 | 0.36 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 109 (88–122) | 148 (142–158) | 1 / 1 | 2,561 | 0.37 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,052 (2,097–3,145) | 5,130 (5,096–5,140) | 1 / 1 | 2,659 | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 150 (124–158) | 197 (179–197) | 1 / 1 | 2,880 | 0.21 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 36 (32–38) | 48 (42–49) | 0 / 0 | 1,198 | 0.17 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 100 (84–108) | 133 (117–138) | 1 / 1 | 3,418 | 0.23 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,081 (2,102–3,112) | 5,117 (5,079–5,128) | 1 / 1 | 3,871 | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 189 (182–199) | 217 (211–217) | 1 / 1 | 3,339 | 0.13 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 36 (33–40) | 48 (45–50) | 0 / 0 | 792 | 0.18 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 117 (101–132) | 149 (148–165) | 1 / 1 | 2,632 | 0.24 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,085 (2,148–3,111) | 5,121 (5,084–5,129) | 1 / 1 | 2,928 | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 186 (183–199) | 248 (231–264) | 1 / 1 | 2,732 | 0.14 |

Thirty successes still bound failure only below about 11.4% (two-sided
Wilson); the coarse p95 and log-time SD are planning inputs, not acceptance.
The preregistered ≥400-pair, adequately powered B3 experiment remains future.
The same run passed 649 unit tests, 418 Chromium/WebKit browser tests
(including 98 presentation regressions, 66 added for successor F001), and
4 attention tests, without browser retries. Native preflight run 37002391842
attempt 1 also passed on that SHA. Mobile harness/device jobs were skipped;
no physical qualification. Final documentation revisions require their own
exact-SHA hosted evidence, retained privately rather than recursively
changing this pilot citation.

### Historical successor pilots — void for endpoint acceptance

Successor review 1 on `794bf1ab` left original `807dcb5c:F022` open (F001):
a visible article could qualify with hidden `.agent-open` content or a fully
transparent filter. Pilots 36996036157, 36997055130 and the documentation-only
repeat 36998546535 are **void for B1 endpoint acceptance**, despite their
480 on-time epochs and the later green 32-case presentation regressions.
Their artifacts remain retained; neither green workflows nor a pilot prove
presentation outside the tested contract. The following former baseline
numbers are retained only as history, never reused as current evidence.

The former article-checked pilot was `check` run **36997055130**, attempt
1, job *Resume benchmark pilot*, on
`6579a4ba774c37d62f6a8481f4b6c238762baf31`. It measures the unchanged shipped
`web/` build
`a562bac854196a7a231a2ff999ccd9d9436727ec432bdbdfca4809e8e1ae6e05`;
only the harness/tests changed, so no production bundle was regenerated.
Preregistration SHA-256:
`b5b7e0e794d516c0ba46c1acb3582b51ba3cae79e7c4210ed74bf0ef585f418d`.
Chromium 151.0.7922.34 and WebKit 26.5, Node v22.23.3, hosted Linux;
10.6 minutes. All **480 attempted epochs** (16 selected strata × 30) are
retained: 480 on time, no non-completions, exclusions, replacements, missing
outcomes or contract/evidence violations. No hidden-time dials or bytes.
All 24/24 negative-control trials were safe, with none missing or duplicated.
The default pilot has no direct-upgrade strata; those and the other selectable
workloads remain separately exercised by the browser suite, not pooled here.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | Dials / handshakes per epoch | Mean bytes | SD of ln(ms) |
| --- | --- | --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 21 (19–23) | 29 (27–31) | 0 / 0 | 1,176 | 0.24 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 70 (56–80) | 99 (90–104) | 1 / 1 | 3,478 | 0.30 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,057 (2,079–3,088) | 5,098 (5,047–5,105) | 1 / 1 | 3,620 | 0.34 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 136 (118–143) | 162 (157–167) | 1 / 1 | 3,655 | 0.19 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 23 (16–27) | 36 (32–36) | 0 / 0 | 776 | 0.36 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 102 (83–129) | 146 (136–154) | 1 / 1 | 2,636 | 0.36 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,047 (2,100–3,150) | 5,129 (5,102–5,137) | 1 / 1 | 2,707 | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 141 (133–163) | 197 (187–241) | 1 / 1 | 2,803 | 0.22 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 34 (32–38) | 48 (42–49) | 0 / 0 | 1,176 | 0.17 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 99 (83–103) | 132 (117–133) | 1 / 1 | 3,478 | 0.22 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,078 (2,099–3,111) | 5,117 (5,084–5,129) | 1 / 1 | 3,822 | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 182 (168–187) | 217 (215–227) | 1 / 1 | 3,442 | 0.14 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 36 (32–41) | 48 (46–51) | 0 / 0 | 776 | 0.18 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 117 (100–132) | 149 (148–164) | 1 / 1 | 2,636 | 0.24 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,084 (2,148–3,112) | 5,120 (5,087–5,130) | 1 / 1 | 2,879 | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 198 (183–201) | 246 (230–268) | 1 / 1 | 2,742 | 0.14 |

Thirty successes still bound failure only below about 11.4% (two-sided
Wilson); the coarse p95 and log-time SD are planning inputs, not acceptance.
The preregistered ≥400-pair, adequately powered B3 experiment remains future.
The same run passed 649 unit tests, 352 Chromium/WebKit browser tests
(including 32 presentation regressions), and 4 attention tests, without
browser retries. Native preflight run 36997054896 attempt 1 also passed on
that SHA. Mobile harness/device jobs were skipped; no physical qualification.
Final documentation revisions require their own exact-SHA hosted evidence,
retained privately rather than recursively changing this pilot citation.

### Historical pilot results — void, retained only as history

The table below formerly described the current baseline. It and its
`fe6a696a` documentation-only repeat (36987388461) are **void for B1 endpoint
acceptance**: original `807dcb5c:F022` found that their harness accepted
CSS-hidden cards. No earlier pilot is reused to establish presentation,
latency or reliability; the original artifacts and numbers are preserved.
The repair's first run 36996036157 on `4010dad9` had a passing 480-epoch
presentation-checked pilot but failed 28 new browser cases because inline
injected test CSS violated the shipped CSP (each retried twice). That failure
is retained, not recast as green; same-origin fixture CSS fixed the test
setup in `6579a4ba`, without weakening CSP. For `807dcb5c:F023`, native
36987388129 attempt 1 remains a failure (SIGTERM exit 2 versus 130 in
`test_isolated_dev_coexists_with_active_service`). Attempt 2 passed, rerunning
the failed Linux job and retaining Darwin's attempt-1 success. Neither its
cause nor physical qualification is inferred from the rerun.

Historical source (not current evidence): `check` run 36985761267, job *Resume benchmark pilot*, on commit
`8a721261c68f2e71220d0b49755c71e7d5ecacae`, measuring the shipped `web/`
build `a562bac854196a7a231a2ff999ccd9d9436727ec432bdbdfca4809e8e1ae6e05`
with the current-path endpoint, exclusion rules, contract checks and evidence
validation described above (later commits that change only documentation ship
the same build, and each hosted run repeats the pilot on its own commit).
Preregistration SHA-256 (as bound into the evidence)
`5723fc06f3ccb2b86c5d866e0681f030ce9ebb13b8b99e570a415af24c3be68c`, no
contract violations; Chromium 151.0.7922.34 and WebKit 26.5 under Node
v22.23.3 on a hosted Linux runner; 10.7 minutes. The default pilot samples
no `gateway-direct` stratum, so it has no direct-upgrade summary. 480 attempted epochs (the
bounded default of 16 strata × 30), all valid, no harness exclusions or
replacements; no hidden-time dials or bytes in any stratum. All 24
negative-control trials (4 controls × 3 trials × 2 browsers) were safe. The
other workloads and paths (`hidden-30s`, `hidden-long`, `network-change`,
`wss-tailscale`, `gateway-direct`) were exercised once each per browser by
the hosted browser suite in the same run (for `gateway-direct` also checking
the direct-upgrade record, plus one refused and one delayed upgrade, an
upgrade that only happens after its window while the primary outcome comes
later still, and a direct session that dies just after its window), not
sampled by this pilot.

| Stratum | On time (Wilson 95%) | p50 ms (bootstrap 95%) | p95 ms (bootstrap 95%) | Dials / handshakes per epoch | Mean bytes | SD of ln(ms) |
| --- | --- | --- | --- | --- | --- | --- |
| chromium/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 21 (19–23) | 29 (27–31) | 0 / 0 | 1,176 | 0.25 |
| chromium/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 64 (57–77) | 101 (89–101) | 1 / 1 | 3,372 | 0.28 |
| chromium/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,050 (2,080–3,075) | 5,102 (5,034–5,105) | 1 / 1 | 3,478 | 0.34 |
| chromium/wss-cloudflare/discard | 30/30 (88.6–100%) | 136 (127–143) | 194 (165–221) | 1 / 1 | 3,549 | 0.22 |
| chromium/gateway-relayed/warm-short | 30/30 (88.6–100%) | 21 (17–27) | 33 (32–35) | 0 / 0 | 776 | 0.35 |
| chromium/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 98 (82–125) | 151 (143–157) | 1 / 1 | 2,636 | 0.35 |
| chromium/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,050 (2,096–3,142) | 5,127 (5,094–5,142) | 1 / 1 | 2,683 | 0.40 |
| chromium/gateway-relayed/discard | 30/30 (88.6–100%) | 150 (135–169) | 215 (182–320) | 1 / 1 | 2,779 | 0.24 |
| webkit/wss-cloudflare/warm-short | 30/30 (88.6–100%) | 35 (34–38) | 48 (42–49) | 0 / 0 | 1,176 | 0.16 |
| webkit/wss-cloudflare/hidden-5m | 30/30 (88.6–100%) | 100 (86–101) | 133 (116–134) | 1 / 1 | 3,574 | 0.23 |
| webkit/wss-cloudflare/blackhole-restore | 30/30 (88.6–100%) | 3,070 (2,100–3,111) | 5,117 (5,076–5,128) | 1 / 1 | 3,748 | 0.34 |
| webkit/wss-cloudflare/discard | 30/30 (88.6–100%) | 195 (171–216) | 239 (232–255) | 1 / 1 | 3,291 | 0.15 |
| webkit/gateway-relayed/warm-short | 30/30 (88.6–100%) | 36 (32–39) | 48 (45–50) | 0 / 0 | 776 | 0.17 |
| webkit/gateway-relayed/hidden-5m | 30/30 (88.6–100%) | 116 (100–132) | 148 (148–166) | 1 / 1 | 2,581 | 0.24 |
| webkit/gateway-relayed/blackhole-restore | 30/30 (88.6–100%) | 3,085 (2,148–3,112) | 5,122 (5,085–5,131) | 1 / 1 | 2,868 | 0.34 |
| webkit/gateway-relayed/discard | 30/30 (88.6–100%) | 185 (183–202) | 265 (232–425) | 1 / 1 | 2,633 | 0.19 |

Reading it as a pilot:

- Under these scripts every attempted epoch completed in time, but 30
  attempts without a failure only bound the failure rate below about 11.4%
  (two-sided Wilson). Planning a reliability comparison at that pessimistic
  rate needs about 15,800 pairs for one target; at an assumed 0% it is
  precision-limited to at least 381 pairs (family of two) or 498 (four). A
  confirmatory design must state which assumption it uses, or first run a
  larger pilot.
- A nearest-rank p95 of 30 values is essentially the second-largest value, so
  the p95 intervals are coarse; the log-time SDs (0.15–0.40) are the variance
  inputs for planning latency precision.
- `blackhole-restore` is dominated by the scripted conditions: the 2-second
  foreground probe timeout, then a dial that waits for restoration and the
  1/3 s SYN retransmission steps. `hidden-5m` redials at once because five
  minutes of silence exceed the keepalive freshness bound, so it skips the
  probe. `warm-short` reuses the connection with no dial or handshake.
- Synthetic fixture latency, emulated suspension and headless browsers make
  these figures unsuitable for comparison with phones or real networks.

Earlier runs are void or superseded, and recorded here so their numbers are
not reused:

- 36938672837 on `da19f2591c0f90a1c102833004acdc1ef0cf139d`: every `discard`
  epoch was a harness error because the reload landed on the bootstrap
  redirect, and killing sockets after advancing the frozen clock let an
  in-flight reply mark a dead path as recently active, which put a 2-second
  probe timeout into some `hidden-5m` epochs.
- 36940357170 on `005065acaafcebfd86be7c8e2024625cdb6cc9b3`: 480/480 on time,
  but under the first endpoint, which accepted the epoch marker anywhere in
  the page (including a workspace label) without relay-verified freshness or
  a live-path check, and which treated crashes and failed warm-ups as
  exclusions. Review replaced that endpoint.
- 36946693294 on `a8dca37270e2bb2adf66accecf4958a60e2825a5`: the first run of
  the revised endpoint recorded 42 non-completions (warm-up failures and
  deadlines, most in WebKit `blackhole-restore`). Every deadline epoch had
  `dials=1, handshakes=1, refreshes=1`: the app reconnected and received
  fresh inventory, but the reconnect's initial snapshot was replaced by the
  answer to the app's own refresh before its frame painted, and the harness
  skipped the newer snapshot while the first was pending. That harness race
  was fixed in `b4ebbfbb` (the frame re-reads what it paints) with a `burst`
  regression fixture; the app was unchanged.
- 36964563904 on `b4ebbfbbb13d12c4a1979e3410f7bec19d9131c9` (build
  `51ddf36c30aac7c7545fc683e0d3f51e40a4022ec73116794d4094066213807b`,
  previously documented here): 480/480 on time, but its endpoint accepted any
  open authenticated session rather than the one the app was using, and its
  analyzer accepted registrations outside the contract. Superseded by the run
  above on the current endpoint and build, and not reused.
- 36972958606 on `b754d77f9829bf1b88e5275394dea71008f2abdf` (build
  `87c8ebcd2b8ba1fc52d1ebc836125c2f494ef64aefae37661c344e1795ccb44e`,
  previously documented here) and its docs-only repeat 36974191330 on
  `f36372995f619cb727b8fc7f33e53e55955d4ad5`: 480/480 on time with no
  violations, but measured with the app's earlier in-app completion rule (a
  queued frame could act on a superseded snapshot, and a mounted but hidden
  rail counted as shown) and before the clustered-evidence refusal and the
  direct-upgrade record. The pilot's endpoint is the harness's own painted
  agent card, so these numbers are close, but they belong to an older build
  and are not reused.
- 36979888303 on `e19c79f95e2bf8f1b01420edd12003e1a9516ff3` (previously
  documented here) and its docs-only repeat 36981336600 on
  `36b6e1e702d7ef68cdbfda810ecdfb78b5f78d20`: the same build and 480/480 on
  time with no violations, but registered before the direct-upgrade record
  was bounded to its own window and before the analyzer's reliability bound
  moved to Tango's score interval, so their preregistrations differ from the
  current one. Superseded by the run above and not reused.

## Limits of this evidence

Hosted headless browsers with an emulated page lifecycle and a synthetic relay
do not represent physical phones, real suspension, radios, VPNs or ingress
providers. Real Android, iOS, Cloudflare and Tailscale measurements are later,
separately authorized evidence (B4); small physical samples can describe
observed behaviour but cannot carry a hosted statistical bound. No mobile,
physical-device or live qualification is claimed here.

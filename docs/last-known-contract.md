# B2 last-known implementation contract (awaiting independent approval)

This is an implementation boundary, not an enablement announcement. Worker
hosted qualification is recorded below; independent B2 approval remains
outstanding. No physical resume or performance claim is made.

## Complete eager shell accounting

The gzip ceiling remains **169216 bytes**. The size guard charges the stable
fallback `index.html` and `herdr-bootstrap.js` even when the host redirects,
the build-specific entry, its parser-blocking `manifest-loader.js`, the single
application JS/CSS pair, the immediate startup update probe's `version.json`,
and the larger gzip representation of `manifest.webmanifest` / `setup.webmanifest`.
The loader selects exactly one manifest; both variants are measured, not added
together. New shell scripts, stylesheets or preloads fail the guard until their
scope is explicitly accounted for. Vite still rejects shared eager JS imports.

The shell has no eager image or remote-font dependency. The apple-touch/PWA icons
are install metadata, not page-rendered images; Nerd Symbols is a unicode-gated,
`font-display: swap` face used by terminal/history content, not the initial
home/summary surface. The hash worker and Terminal/Conversation/Settings chunks
are content/action-triggered, not shared eager imports. Service-worker/push art
requires notification state and authenticated relay configuration; `release.json`
is release-verifier metadata, not a startup fetch. These are shipped and validated,
but are outside the eager home/summary shell budget; the budget is not all traffic
from later deep-link navigation, authenticated services, installation or updates.

Release finalization compares a bounded set of original/Oxc/Bun representations
using the existing release toolchain and the guard's unchanged gzip settings,
retaining the smallest before hashing/compression. It never replaces an already
smaller original with a larger transformation. Imports remain external and no
chunk, feature, readiness/error path or deadline is removed or deferred. The unchanged
release validator, complete packaged browser suites and exact-SHA byte equality
remain required. Exact current receipts belong in the additive handoff index.

Successor finding F008 identified an omission in earlier accounting: the loader
was missing from the five-file totals. Those receipts and their reported numbers
remain historical evidence, **not proof of complete eager-payload compliance**.
The startup manifest/version metadata are now also charged conservatively. No
old receipt is retroactively relabelled as passing the expanded guard.

## Hosted worker qualification

### F005/F006 behavioral source receipt

Source `d7bf41cf8190526fe7bc1591a710c051d00eb9c8` passed hosted
[37091060905](https://github.com/0cv/herdr-mobile-relay/actions/runs/37091060905):
focused Go race checks, lint/types, 759 unit tests, all 268 focused and 492 complete
packaged Chromium-mobile/WebKit-mobile cases, without skipped browser cases.
New coverage includes already-open peer invalidation after denied writes/rejected
locks, recovered sync fencing, cancellation retry after error and 60-second timeout,
exact grant ownership, retained controller identity, restart fencing and visible
cancellation retry. Native preflight passed
[37091060885](https://github.com/0cv/herdr-mobile-relay/actions/runs/37091060885).
The former five-file guard reported **167168 B / 169216 B** gzip;
see the incomplete-accounting correction above.
Its source-bound generated assets were imported verbatim after all 43 SHA-256
checks and complete coverage verification. Manifest SHA-256:
`974bc82be191fe3dfa4cccdaf59d0e2c7e58d9744efd86ecf1c8cc73fbb6cc49`.
This is behavioral evidence only, not independent approval or provider-model
attestation. Required local runtime provenance inspection and its limitations are
explained below. Post-import final-SHA receipts
are identified by the handoff and source-bound CI artifacts, not by substituting a
pre-import source SHA for the final candidate.

### Final candidate submitted to review round 2

Candidate `88a34571533d6f5a8ee2c556ca251f2d65a8874e` passed final-SHA hosted
[37087913459](https://github.com/0cv/herdr-mobile-relay/actions/runs/37087913459):
752 unit tests, all 268 focused and 492 complete packaged browser cases, release
parity (`diff -qr frontend/dist web`), and the then-current bundle/graph/size
validation (not complete eager accounting; see the correction above).
Ordinary release qualification, including release equality, Go race/vet checks,
four-target packaging and native release smoke, passed
[37087913699](https://github.com/0cv/herdr-mobile-relay/actions/runs/37087913699).
Native preflight passed
[37087913405](https://github.com/0cv/herdr-mobile-relay/actions/runs/37087913405).
There were no skipped required packaged browser cases; conditional mobile harness
and physical-device jobs were skipped and do not establish physical qualification.
The final-SHA artifact was retrieved: exact source binding, all 43 hashes,
complete coverage and byte-identical checked-in outputs passed verification.
Its manifest SHA-256 was
`d91ff4dfba49f01ef8587f43bb516aa20a37032afd105c5750b0164024bd9535`.

These are immutable receipts for the named candidate, not a declaration that a
historical import source is the latest revision. Subsequent submissions identify
their exact candidate and final-SHA receipts in the worker handoff and source-bound
CI artifacts; match the head SHA in the
[branch workflow runs](https://github.com/0cv/herdr-mobile-relay/actions?query=branch%3Aci-tailscale-native-preflight-ef9f843).
A later source/asset change is not qualified by an older green receipt.

### Historical source of the round-2 asset import

Source `ca0267612b281dac17352559e12d244846964b4d` passed hosted run
[37086787547](https://github.com/0cv/herdr-mobile-relay/actions/runs/37086787547):
focused Go race checks, lint/type checks, **752 unit tests**, all **268 focused**
and **492 complete** packaged Chromium-mobile/WebKit-mobile cases, without skipped
browser cases. This includes post-freshness saved notification-policy fetch/display,
failed opt-out sync/recovery fencing, and 513 completed uploads beyond the shared
grant budget while preserving an unrelated lease. Native preflight also passed
[37086787600](https://github.com/0cv/herdr-mobile-relay/actions/runs/37086787600).
The then-current release validator passed and the former five-file guard
reported **166939 B / 169216 B** gzip (not complete eager accounting).
These receipts address historical worker qualification, not reviewer dispositions.

The source-bound `b2-generated-assets-ca0267612b281dac17352559e12d244846964b4d`
artifact supplied the updated `web/` and `frontend/build-versions.json` verbatim.
Its exact source binding, all 43 generated-file SHA-256 checks and complete manifest
coverage passed before import and were verified again against the imported bytes.
Manifest SHA-256: `d91ff4dfba49f01ef8587f43bb516aa20a37032afd105c5750b0164024bd9535`.
Later source or asset revisions repeat the same hosted worker gates before handoff;
independent B2 approval and physical/statistical qualification remain outstanding.

### Pre-review baseline

Source `a1510708672ce98fbcdc283e013d3b5f5ce80498` passed hosted run
[37082388198](https://github.com/0cv/herdr-mobile-relay/actions/runs/37082388198):
focused Go race tests, lint/type checks, 746 unit tests, 268 focused packaged
browser cases and the complete 492-case Chromium-mobile/WebKit-mobile suite.
There were no skipped browser cases. Existing release-graph validation passed;
the former five-file guard reported **166837 B / 169216 B** gzip (not complete
eager accounting; see the correction above).

The source-bound `b2-generated-assets-a1510708672ce98fbcdc283e013d3b5f5ce80498`
artifact supplied the imported `web/` and `frontend/build-versions.json` verbatim.
Its source binding and all 43 generated-file SHA-256 checks passed, with complete
manifest coverage including lazy chunks and compressed assets. The manifest's
SHA-256 is `8e51d51dd64531f9ebd288d5565a2ba10fdccc38ce505cf816c50d2c30ae5496`.
The imported revision `15d5edd5056c661a99e5173c30f45b405ed85686` subsequently
passed the complete hosted suite in
[37083443366](https://github.com/0cv/herdr-mobile-relay/actions/runs/37083443366),
ordinary release equality and bundle qualification in
[37083443624](https://github.com/0cv/herdr-mobile-relay/actions/runs/37083443624),
and native preflight in
[37083443380](https://github.com/0cv/herdr-mobile-relay/actions/runs/37083443380)
(attempt 2; attempt 1 failed only on artifact-upload ETIMEDOUT). Its final-SHA
artifact was also retrieved and all 43 checksums, complete manifest coverage,
and byte-identical checked-in outputs were verified. These are named historical
qualification receipts, not independent approval or a waiver for later revisions.
Review remediations repeat the same hosted gates before worker handoff. No project
code was executed locally.

## Correlated freshness and compatibility

The additive `inventory_snapshot_v1` capability uses the existing authenticated
`refresh_agents` command with a `snapshot_request_id`: exactly 22 canonical
base64url characters representing a random 128-bit nonce. It does not change
protocol version 3 or the legacy inventory messages.

A capable relay queues at most one nonce per connected client (latest wins),
with a 15-second deadline and a global cap of 1,024 pending clients. Only a
poll **started after the request was admitted** can answer it. A poll already
in flight, a registration snapshot, an immediate committed-state refresh,
an event publication, activity, push configuration or a ping cannot answer.
The response is one atomic `inventory_snapshot` message containing the nonce,
status, agents and workspaces selected from coherent coordinator state after
that successful poll, not the sticky presentation merge. This preserves real
target generations and authoritative events that land during/after the query.
Readiness is checked again inside the send barrier. Failure, cancellation or a
rejected topology commit cannot return ready data.
Expired responses are dropped. No command/action is queued for later replay.

Upload begins reserve one of the shared 512 cleanup slots before transmission;
competing watch/lease/view acquisition cannot strand an admitted begin. Failed
writes, errors, aborts, disconnection and the unchanged 60-second deadline release
pending reservations. Successful begins replace them with exact-target upload grants.
Only matched successful finish/cancel responses release established upload grants.
After reconnect, only cancellation authority is retained, bound to the same enrolled
credential (including secret, device, role and version) and relay connection configuration.
It never restores chunk/finish, watch, lease or live-target authority. Removal,
authentication rejection or a changed credential/configuration prevents recovery.
Terminal teardown keeps a pending controller in the in-memory store so remounting
that pane, even with a new live generation, exposes explicit cancellation retry for
the original target/upload ID without starting another upload. Successful cleanup
removes the retained controller. Scope invalidation also discards retention so a
fresh pane can start a new upload, without restoring revoked cleanup authority.
The binding-time scope token fences late teardown callbacks from re-inserting an
invalidated controller, including credential changes while disconnected.
This is tab-lifetime recovery, not durable storage:
reload/process loss still relies on the relay's bounded staged-upload expiration.

The frontend freshness primitive binds its single outstanding nonce to an
opaque connection/session identity, active path identity and wake generation.
Application integration invalidates it on replacement, wake, lock, unready
inventory, descriptor/capability replacement, credential changes, hidden/offline/
freeze/pagehide suspension and disconnection. Hidden pages cannot acquire fresh
action authority; existing keepalive timing is unchanged. Scoped cleanup includes
clearing previously-owned viewing suppression, never acquiring a new viewed
target. Installed-controller app-origin registration waits for correlated fresh
inventory and is sent once per connection.
Only a matching ready/non-stale response grants freshness, including a valid
empty inventory. Workspace validation is independent of agent validation.
Re-resolution against current authoritative targets is still required at
actual dispatch; the freshness primitive is not an authorization substitute.

Compatibility is intentionally asymmetric:

- Old client + new relay: unchanged legacy request/status/agents/workspaces
  behavior; no correlated message unless the new field is supplied.
- New client + old relay: uncorrelated replies never enter operational live
  target surfaces, satisfy this stronger freshness assertion or enable actions
  protected by it. The UI explains that a relay upgrade is needed. There is no timing-based fallback.
- A new client must learn the capability through the authenticated channel,
  not from an unauthenticated gateway descriptor.

The integration wires correlated freshness into the store, filters live target
surfaces, gates raw and command dispatch with an explicit policy, and adds a
separate summary component and Settings consent. Hosted integration/caller
qualification passed at the named revisions above; independent review/approval
remains outstanding. Unsupported relays never satisfy action freshness.

## Summary schema and cryptographic boundary

Summary version 1 contains only local relay association, last-fresh time,
explicit truncation, synthetic display-only group/row IDs, bounded agent and
workspace labels and allowlisted coarse type/status. No live object is spread
or persisted. Labels may themselves be sensitive. Generic labels replace
missing names; no cwd/project/terminal/conversation fallback is allowed.

Labels are capped at 120 Unicode code points and 480 UTF-8 bytes. Per relay:
200 agents, 50 groups, 64 KiB UTF-8 plaintext, 60-minute maximum age. Stored
ciphertext, envelope and encoding overhead count toward 512 KiB and 10 relays;
eviction is oldest first. Invalid/future times and detected rollback miss.

Only validated enrolled per-device credentials qualify. HKDF-SHA-256 uses the
decoded credential secret, a fresh random 256-bit salt and a distinct last-known
domain. AES-256-GCM uses a fresh 96-bit IV on every write. Canonical AAD binds
origin, opaque local relay association, credential ID/version, schema, last-fresh
time and expiry. Legacy configuration IDs can contain names/hosts: they are never
stored in the summary envelope. A separate non-extractable HKDF-SHA-256/HMAC key
derives the opaque association from the runtime configuration identity and
credential/device/version/role context. Changing the full configuration identity
is a miss even if its legacy shortened ID collides. Origin, opaque credential
identity/version, timestamps and ciphertext size remain observable metadata.
Keys are non-extractable and never persisted. Envelopes are bounded and checked
before decoding or decrypting; plaintext is validated entirely before publish.
Failures never fall back to plaintext or affect pairing/reconnect.

SessionStorage is the only summary backend in B2. It may survive tab restoration,
but can be absent on a new Home Screen launch, tab closure or browser eviction.
Encryption protects a standalone blob, not XSS or extraction of all origin
storage: credentials already live in localStorage. WebAuthn is an interface
gate, not a hardware-backed cache key. JavaScript heap erasure is not promised.

The cache class starts disabled and locked. Lock, Forget and explicit invalidation
fence pending asynchronous work before clearing current memory and summary DOM.
Credential/removal/refusal lifecycle callers invalidate all scopes conservatively.
`LastKnownControl` supplies mandatory persisted opaque epochs, Web Locks serialization
and content-free storage/BroadcastChannel/resume notifications. A usable
BroadcastChannel is required before caching can become eligible, including from
persisted opt-in: absent, rejected or initially unwritable channels fail closed.
Storage events alone cannot carry a failed durable invalidation. Later sync or
recovery of browser APIs does not silently undo this failure latch. No tab notification
contains a relay ID, label, credential or ciphertext. A restored tab must validate
its session envelope against the persisted epoch before displaying it. Snapshot
requests capture control generation so a pre-Forget response cannot repopulate it.
SessionStorage storage events alone do not clear other tabs. A failed control
change stays fail-closed in the current tab across later notifications and resume
syncs, even if the old persisted record remains enabled. A content-free failed-
invalidation notification also withdraws summaries and latches off listening peers;
it is deny-only, never authority to enable or restore. Unreachable/suspended peers
may miss that signal: if durable invalidation fails, cross-tab/reload erasure remains
uncertain and the operation reports failure rather than promising global deletion. A successful Forget retry
conservatively persists off after such failure; enabling again requires an explicit
successful opt-in. Sync cannot restore consent while a control change is pending.
Storage-denied deletion must report uncertainty rather than promise durable erasure. Offline remote
revocation is unknowable until observed; expiry bounds remaining visibility.

## Approval and qualification boundary

The integration is not an independently accepted B2 feature. The named hosted
qualification covered fail-closed dispatch, alternate caller/lifecycle paths,
cross-tab restoration, component/browser journeys and B1 regressions; its generated
assets were retrieved and source-bound before import and checked again afterward.
Independent approval and physical/statistical qualification remain outstanding.
The plan requires preserving the model-authentication extension policy and
inspecting actual model provenance; configured labels alone are insufficient.
Inspected local runtime records preserve that policy and bind worker identities,
turns and requests to `openai-codex/gpt-6.1-sol` through `prepared.model` and
`before_provider_request` `ctx.model` observations. These are actual local
runtime/request-selection metadata, not authenticated provider attestation:
`providerBoundConfirmed:false`. They do not prove final wire bytes or which model
the provider ultimately processed or returned; response IDs alone do not prove it.
The literal plan does not prescribe provider receipts, final-wire binding or
returned `response.model` as a mandatory evidence format.

Historical finding `f4725027-0c33-4b3e-b695-bc66c554fcb9:F007` and that run's blocked
outcome remain recorded, not rewritten. Additive independent adjudication
`d2a143c2` and both first-round linked-successor reviewers judged it invalid as a
blocker under the literal plan, while retaining the provider-attestation limitation.
This is not B2 approval; complete final-revision independent review, required local
provenance inspection and exact-SHA hosted qualification still apply.
Any subsequent source or asset revision repeats hosted qualification before worker
handoff; historical receipts are not substituted for current checks. Local project
build/test scripts remain prohibited by the supplied plan.

### Current source and verification status

- Relay: `internal/app/inventory_snapshot.go`, `server.go`,
  `internal/coordinator/poller.go`, `internal/protocol/protocol.go`.
- Frontend foundations: `frontend/src/lib/inventory-freshness.ts`,
  `last-known-types.ts`, `last-known-crypto.ts`, `last-known.ts`.
- Tests include Go poll/barrier cases and frontend freshness, summary/crypto,
  coordination, read-only component, store dispatch and browser journeys.
- Hosted foundations at `4bbe93098ffe9d38c6db43c8a8d3a5cf453b7013` passed runs
  `37044147190` (source/release pipeline) and `37044147180` (native preflight).
  Those are not integration acceptance.
- Historical integration `e9ed8c3…` failed type checking. `ed07b0e…` passed
  lint/type checking and focused Go race tests, but failed 61/709 frontend cases
  (648 passed), including legacy fixture assumptions and one wake-guard regression.
  Release generation/browser acceptance were skipped in those failed runs. These
  failures were repaired before the complete qualification recorded above; they
  are retained as historical failures, not current outstanding acceptance work.
- Source formatting only: `gofmt -w internal/coordinator/poller.go
  internal/app/server.go internal/app/inventory_snapshot.go
  internal/app/inventory_snapshot_test.go internal/protocol/protocol.go`
  completed with exit 0. No local project lint/type/build/test command ran.
- B2-only revisions have been committed and non-force pushed under the explicit
  hosted-verification authorization. No local executable project check, deployment,
  application installation or production mutation has been performed. Qualified
  generated assets were retrieved, fully checksum-verified and imported verbatim;
  the imported final-SHA artifact was separately retrieved and verified as above.

The earlier missing hosted-path authorization is resolved by explicit human
guidance permitting B2-only commits/non-force pushes to the existing CI branch
for disposable hosted tests/builds and artifact retrieval, with no deployment
or installation. `.github/workflows/b2-source.yml` uses the existing release
pipeline and hosted Go/frontend/browser checks, and returns SHA-bound generated
assets. The named execution outcomes above record that qualification; source
inspection alone is not a pass.

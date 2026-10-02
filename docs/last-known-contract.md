# B2 last-known implementation contract (in progress)

This is an implementation boundary, not acceptance evidence or an enablement
announcement. B2 remains incomplete until integration, hosted tests, generated
bundle verification and independent approval are complete. No physical resume
or performance claim is made.

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

The current integration candidate wires correlated freshness into the store,
filters live target surfaces, gates raw and command dispatch with an explicit
policy, and adds a separate summary component and Settings consent. This is an
unqualified candidate: hosted integration/caller regressions and review are
still mandatory. Unsupported relays never satisfy action freshness.

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
and content-free storage/BroadcastChannel/resume notifications. No tab notification
contains a relay ID, label, credential or ciphertext. A restored tab must validate
its session envelope against the persisted epoch before displaying it. Snapshot
requests capture control generation so a pre-Forget response cannot repopulate it.
SessionStorage storage events alone do not clear other tabs. Storage-denied deletion must
report uncertainty rather than promise durable erasure. Offline remote
revocation is unknowable until observed; expiry bounds remaining visibility.

## Outstanding delivery work

The integration is not an accepted B2 feature. Fail-closed dispatch and alternate
caller/lifecycle coverage, cross-tab restoration, component/browser journeys and B1
regressions must pass on the final revision. Hosted-generated assets must then be
retrieved and bound to the release source. Security/mobile/resume documentation
now describes the candidate, not qualification. Hosted execution is mandatory; local
build/test scripts are prohibited by the supplied plan.

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
- Integration `e9ed8c3…` failed type checking. `ed07b0e…` passed lint/type checking
  and focused Go race tests, but failed 61/709 frontend cases (648 passed),
  including legacy fixture assumptions and one wake-guard regression. Release
  generation/browser acceptance were consequently skipped. Fixes and stronger
  correlation fixtures are being developed; final-revision passes remain required.
- Source formatting only: `gofmt -w internal/coordinator/poller.go
  internal/app/server.go internal/app/inventory_snapshot.go
  internal/app/inventory_snapshot_test.go internal/protocol/protocol.go`
  completed with exit 0. No local project lint/type/build/test command ran.
- B2-only revisions have been committed and non-force pushed under the explicit
  hosted-verification authorization. No local executable project check, deployment,
  application installation or production mutation has been performed. Generated
  assets for the integrated final candidate have not yet been retrieved.

The earlier missing hosted-path authorization is resolved by explicit human
guidance permitting B2-only commits/non-force pushes to the existing CI branch
for disposable hosted tests/builds and artifact retrieval, with no deployment
or installation. `.github/workflows/b2-source.yml` uses the existing release
pipeline and hosted Go/frontend/browser checks, and returns SHA-bound generated
assets. Its execution outcomes must be recorded; source inspection is not a pass.

# In-process foreground session authority (R2B)

## Scope and caller obligations

`SessionAuthority` is the same-process, pinned LocalAPI owner for one ephemeral
foreground Serve session. It is implemented in `session.go` and
`session_config.go`; it does not enable an app, shell, bootstrap, listener, or
pairing caller. `Inspect` remains read-only and permanently reports
`ServeRouteOwned=false`.

The managed caller must acquire its existing owner lock **before** constructing
this authority, keep the backend listener bound and fail-closed through route
registration, uncertain outcomes, and selective deletion, and keep pairing
revoked / the listener bound-but-inert whenever `Invalidation()` closes or
`Status` reports invalidation/quarantine. The caller must not pass in a session
ID, ownership boolean, or readiness flag. `Prepare` is read-only; after explicit
consent, `Activate` creates the one watcher and submits at most one conditional
registration. `Validate` is a fresh owner/config check only; local health,
listener/TLS checks, invitation durability, gate arming, and pairing readiness
remain later caller obligations. There is no `Close` destructor. `Retire` is the
only intentional watcher cancellation path, and it reaches it only after
`RouteCleared`.

The activation/request context is not the watcher lifetime context. Watcher
lifetime is derived from an owner-held background context and is stopped only
after config-level route clearing. A timeout/cancel during registration is
therefore not a deferred watcher close and cannot become evidence that the POST
did not apply. Callers must retain O/control and the inert listener when a
result is unresolved.

`WithValidatedRoute` is the serialized commit seam for owner-dependent durable
arming: it holds the authority operation lock through a fresh pre-check, the
caller's bounded health/admission callback, a second live route/watch check,
and the caller's one-way commit callback. Retirement and watcher invalidation
are ordered before that final check or after the commit; the method does not
supply local readiness or change the obligation to treat a post-commit lost ACK
as committed/ambiguous. Lifecycle-operation lock acquisition is context-aware,
so cancellation while queued returns without scheduling later work. The raw
watch reader closes its done channel at EOF; the monitor publishes invalidation
before waiting for that lock. `AdmissionChannels` exposes only the ended-watch
and invalidation channels to the managed BootstrapGate; it exposes no route/owner
status and acquires no
authority mutex during the gate's final admission check. That lets the gate
reject Resolve and Complete on raw watch EOF while asynchronous status
revocation is still queued.

## Admission and registration

Production construction is `NewSessionAuthority(expectedVersion,
versionMetadata, httpsPort, backendPort)`. It uses `newLocalAPI` to select one
transport cell from admitted metadata. The standalone cell keeps the fixed R2A
Unix-socket policy. App Store metadata selects the separate R2D cell, which is
unconditionally disabled (`appStoreLocalAPIProfileEnabled = false`) and has an
empty production profile table; it never falls back to the standalone socket.
There is no endpoint, socket, CLI, token, or version injection in the public
constructor. Package-private synthetic profile/discovery/dial seams are used
only by deterministic tests.

`Prepare` requires a running authenticated status, a complete stable identity
(node ID, numeric user ID and available user profile, DNS name, tailnet name,
MagicDNS suffix/setting, certificate-domain set, and exact daemon version),
the already-present pinned `Self.CapMap["https"]` capability, a complete
bounded Serve response and a source-valid empty config (`null` or `{}`). The
capability is the pinned `tailcfg.CapabilityHTTPS == "https"` used by
`ipnstate.PeerStatus.HasCap`; the deprecated `Capabilities` slice and
`CertDomains` alone are not HTTPS readiness. This owner never enables HTTPS,
logs in, or changes preferences, privilege, tailnet, DNS, or Funnel state.

After consent, `Activate` rechecks identity/config/ETag, opens one live watch,
uses only the nonempty session ID from that watch's initial notification, then
re-reads identity/config immediately before exactly one conditional POST. The
posted entry is only the canonical HTTPS `host:port` listener, `Proxy /`, and
exact `http://127.0.0.1:PORT` backend. Missing/invalid ETag or changed identity
or config means no POST. No route adoption, background mapping, CLI child,
registration replay, or reconnect is supported. The ID stays in private memory;
it is absent from status, diagnostics, files, and QR-facing APIs.

The LocalAPI write disposition is preserved as four distinct outcomes:

- `not-dispatched`: local request validation refused before dispatch;
- `settled-success`: a complete, source-valid `200` with empty body;
- `settled-no-write`: a complete, source-valid `412 etag mismatch` response;
- `unresolved`: timeout, cancellation/reset after dispatch, truncation, malformed
  response, wrong version, or any unrecognized response.

An unresolved registration plus an absent GET is still unresolved. `Validate`
requires a complete fresh status and full config, unchanged identity/capability,
a live owned watcher and exactly the one expected foreground entry with no
additional config/exposure. It never authorizes pairing by itself.

## Full Serve preservation and selective retirement

The cleanup parser validates against the complete pinned `ipn.ServeConfig`
shape (including `TCP`, `Web`, `Services`, `AllowFunnel`, `Foreground`, and all
recognized nested fields), after strict bounded JSON validation. It rejects
unknown members, duplicate/case aliases, invalid scalar/null types, excessive
size/depth, and unsupported version before mutation. The raw recognized members
are preserved while removing only `Foreground[ownedSessionID]`; cleanup does
not pass through the deliberately narrow `ParseServe` observational projection,
clear all Serve state, require global emptiness, or normalize foreign entries.
Pairing admission is narrower than preservation: only the canonical empty
initial config can be registered, and validation requires just this run's exact
single foreground route.

For settled-success registration, retirement re-reads full identity and config
while the watch is live. An absent own key is `routeCleared`; the owner then
cancels the local stream and joins the reader. If the full value at the own key
is exactly the submitted entry, cleanup removes only that key using a fresh
nonempty ETag, requires a complete acknowledgement and a fresh full readback
without that key. A `412` rebases from a fresh read; retries are limited to three
conditional cleanup writes and one five-second cleanup budget across repeated
calls. Ambiguous deletion is never blindly replayed: only a fresh read can
permit a new exact-key deletion. An unresolved registration stays quarantined
when absent. If the exact submitted value later appears, an acknowledged
selective delete plus full absence readback can reconcile it. Changed/expanded
same-key values and unknown schema refuse mutation and intentional watcher
closure. A settled no-write / not-dispatched result can clear only after a fresh
read proves the key absent.

Status separates `RouteCleared`, `LocalWatchClosed`, and
`RemoteWatchRetirementUnknown`. The last remains true: pinned source supplies no
server watcher-GC acknowledgement or finite deadline. Normal local release
requires config-level `RouteCleared` and a joined local reader; it is not proof
of raw StateStore erasure, global hostname unreachability, drained ingress, or
remote watcher reclamation. On spontaneous watcher loss, an unchanged admitted
identity plus a fresh absent config can establish config-level clearing only
for a settled registration/no-write; an exact remaining key or unresolved write
stays quarantined. The pinned watcher-map removal and `DeleteForegroundSession`
cleanup take the backend mutex in separate steps. A `SetServeConfig` can
interleave after the no-op session delete but before watcher-map removal and
install the key; the LocalAPI has no atomic active-watch admission guard. If
that occurs, the owner invalidates and quarantines on watch loss, does not retry
or adopt, and leaves the backend inert obligation with the caller; it cannot
claim the residual route was cleared. `TestSessionAuthorityWatcherDeleteMapRemovalGapNeverValidatesOrRetries`
models this interleaving. There is no automatic watch replacement or
stale-process recovery.

## Threat boundary and limits

Cooperating local writers reserve and modify their own ephemeral foreground
keys. The owner preserves recognized unrelated routes, Services, Funnel values,
and independent foreground keys. The daemon's sourced watcher cleanup deletes
only the exiting watch's key. A same-UID/root/operator writer that repurposes
this exact key violates that noninterference assumption. While live, a detected
changed key causes no cleanup write and no intentional watcher close. This does
**not** protect the replacement if the watch exits involuntarily, nor does it
protect a same-key replacement in the final GET-to-close gap: the daemon may
unconditionally delete that key before the client observes watcher exit. The
owner makes no claim of guaranteed same-key preservation across those races.

The watcher/session authority is in-process only and does not constrain other
processes with LocalAPI authority. `routeCleared` is config-level, not proof of
daemon bookkeeping/ingress drain. R2B did not implement app gate revocation,
keep-the-process-alive quarantine, shell O persistence, actual daemon
qualification, the held-output-pipe supervisor case, or hosted CI. R2D adds
only the disabled fixture-level App Store transport below; it does not alter
owner behavior. The separate R2C app/control integration connects
`SessionAuthority` to a revocable device bootstrap gate and private lifecycle
control without changing the owner, selective-retirement, or
remote-watch-unknown contract; see
[`../app/TAILSCALE-INTEGRATION.md`](../app/TAILSCALE-INTEGRATION.md). Native
daemon/runtime qualification and final hosted CI remain independent gates.

`SessionAuthority.Origin()` exposes only the canonical HTTPS origin derived
from the immutable prepared node identity and listener port. It contains no
watcher/session identifier, performs no independent ownership admission, and
is used only for configured-origin admission and app-health consistency
tests; every mutation and lifecycle fact still comes from the production
authority's live LocalAPI checks.

## Disabled App Store transport cell (R2D; fixture-only)

The App Store macOS LocalAPI transport is a separate selector for the
in-process `SessionAuthority`, not a change to the owner or to `Inspect`.
`appStoreLocalAPIProfileEnabled` is `false` and the production exact-version
profile table is empty. The exact tuple is `osVariant`, `gitCommit`,
`extraGitCommit`, `long`, `daemonLong`, and `cap`; until an approved profile and
switch exist, construction refuses before discovery. `Inspect` still has its
independent App Store `gitCommit` version-admission mismatch, a documented
follow-up outside this slice. `ServeRouteOwned` remains false, and
`RemoteWatchRetirementUnknown` remains true.

The fixture reuses pinned Tailscale v1.102.4 `client/local/local.go` (`Client`,
`defaultDialer`, `DoLocalRequest`) and
`safesocket/safesocket.go` / `safesocket/safesocket_darwin.go`
(`LocalTCPPortAndToken`, `localTCPPortAndTokenDarwin`,
`portAndTokenFromSameUserProof`), referenced through the unexported
`appStoreDiscover` seam; `dialLoopbackTCP` defaults to
`(&net.Dialer{}).DialContext`. All clients set `OmitAuth=true`; a bounded
RoundTripper supplies `SetBasicAuth("", token)` exactly once. Discovery is
one-shot, deadline-bounded at two seconds, and validates port 1..65535 plus a
1..256-byte printable ASCII token excluding colon/whitespace. Upstream same-user
proof discovery cannot be cancelled once started, so a timeout refuses while
its goroutine may finish in the background. The dial target is exactly
`local-tailscaled.sock:80`, redirected only to `127.0.0.1:<discovered-port>`;
401/403 never rediscover or retry. Credentials, port, and auth headers are not
reported.

Tests inject only synthetic metadata, credentials, profiles and in-memory
connections. The package `TestMain` replaces the production discovery and dial
seams with fail-fast guards; tests do not call `lsof`, read proof files, open
sockets, or contact a daemon/tailnet. This is fixture-only and not
runtime-qualified: it does not enable pairing, change settings, or establish
live authority.

## Source and test evidence map

Pinned source: Tailscale v1.102.4, commit
`bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`.

- `ipn/localapi/serve.go:25-78`: complete synchronous Serve config GET/POST;
  HTTP 412 ETag refusal.
- `ipn/ipnlocal/serve.go:306-438`: nonempty ETag comparison and config mutation
  under the backend mutex; `:425-433` removes only the foreground session key.
- `ipn/ipnlocal/serve.go:1571-1650`: effective Serve config filters inactive
  foreground entries.
- `ipn/ipnlocal/local.go:3717-3880`: watch cleanup registration and session-key
  deletion; no daemon acknowledgement is exposed.
- `ipn/ipnstate/ipnstate.go:295-360` and `tailcfg/tailcfg.go:2508`:
  `CapMap` and the `"https"` capability used by `HasCap`.
- `ipn/serve.go:46-186`: pinned `ServeConfig` and nested handler/service schema.

Deterministic tests instantiate the production LocalAPI adapter and
`SessionAuthority` with an in-memory byte-level HTTP/stream fake; no socket,
process, daemon, user path, or service is used:

- `TestSessionAuthorityRegistersAndValidatesOnlyItsLiveWatcher`,
  `TestSessionAuthorityRejectsIdentityAndCapabilityDriftBeforeWrite`,
  `TestSessionAuthorityRequiresEmptyConfigAndETagBeforeWatchOrWrite`.
- `TestSessionAuthorityNoWrite412PreservesForeignConfigAndCanClear`,
  `TestSessionAuthoritySettledNoWriteWithSameKeyConflictRefusesClose`,
  `TestSessionAuthorityStalledRegistrationTimeoutAndLateCommitStayQuarantined`,
  `TestSessionAuthorityAppliedRegistrationWithLostAckIsSelectivelyRemoved`.
- `TestSessionAuthorityCleanupRebasesAndPreservesRecognizedForeignFields`,
  `TestSessionAuthorityCleanupConditionalWritesAreCappedAcrossRetries`,
  `TestSessionAuthorityValidationRejectsForeignExposureButRetirementPreservesIt`,
  `TestSessionConfigPreservesFullPinnedServeSchemaWhenRemovingOneSession`.
- `TestSessionAuthorityChangedSameKeyQuarantinesWithoutMutationOrIntentionalClose`,
  `TestSessionAuthorityFinalReadCloseGapSameKeyReplacementRemainsAnExplicitLimit`,
  `TestSessionAuthoritySpontaneousWatchLossUsesFreshAbsenceAndInvalidates`,
  `TestSessionAuthorityRemoteWatcherGCIsUnknownAfterAcknowledgedConfigClear`.
- `TestSessionAuthorityConcurrentValidateRetireSerializesAndNeverMutatesAfterClear`,
  `TestSessionAuthorityBackgroundMonitorSignalsRouteDrift`, and the
  `TestLocalAPI*` protocol tests cover serialized transitions, monitoring,
  bounded transports and request classification.
- `TestAppStoreLocalAPIDisabledRefusesBeforeDiscovery`,
  `TestAppStoreLocalAPIDiscoveryBoundsAndValidation`,
  `TestAppStoreLocalAPIDialsOnlyDiscoveredLoopbackPort`,
  `TestAppStoreLocalAPIRejectsInvalidIdentityAndVersion`,
  `TestAppStoreLocalAPIMissingOrDeniedAuthenticationFailsClosed`,
  `TestAppStoreLocalAPIRedactsDiscoveredSecret`,
  `TestAppStoreLocalAPINeverUsesUpstreamDefaultAuthOrDiscovery`,
  `TestSessionAuthorityAppStoreFakeDaemonLifecycle`, and
  `TestLocalAPIStandaloneSocketPolicyUnchanged` cover the disabled App Store
  cell using synthetic metadata/credentials and in-memory fakes only.

# Managed Tailscale app/control integration

This slice connects the R2B in-process `tailscale.SessionAuthority` to the
managed app process. It is not a complete user-facing Tailscale launch flow:
the launcher migration, native daemon qualification and live phone/browser
qualification remain separate work.

## Owner and startup ordering

`cmd/herdr-mobile-relay serve` acquires managed owner O before calling
`app.NewOwned`. For Tailscale only, `NewOwned` performs bounded read-only
`tailscale inspect` work (status, version metadata and Serve status), derives
the configured HTTPS port, requires the exact configured node origin and
empty/non-Funnel Serve observation, then constructs and calls `Prepare` on the
fixed production `SessionAuthority`. CLI route observation is only an admission
check; it never establishes ownership. The in-process LocalAPI owner still
requires its own version, identity, HTTPS capability, config and ETag checks.

The managed constructor installs a stable, closed `deviceauth.BootstrapGate`
in the Hub and does not call `deviceauth.Open`, protect the device-auth
folder, or arm an invitation. The Hub refuses new sessions until durable owner
arming; resolver calls before store attachment are transiently refused. Once a
store is attached, a closed gate rejects invitation resolution/completion
before reaching Store (so an expired bootstrap invite cannot be refreshed or
consumed). Existing credential records are preserved. Gate revocation
synchronizes with in-flight resolver operations and cannot be undone.

The backend and UDP listeners must be bound, the local inventory must be ready,
and the private localcontrol socket must be live before `local_ready` becomes
true. Only localcontrol `activate`, bound to the private socket's run ID and
instance, may submit the first Serve write. Activation and arming do fresh O/L,
route, local `/readyz`, UDP, bundle-identity and trusted HTTPS checks. Activation
also runs `appdeploy.VerifyPublic`; arming repeats it before store initialization
and repeats the live owner/health check before persisting a new bootstrap
invitation and opening the gate. No QR is printed by this Go slice.

## Private control status and bounds

The private protocol exposes `status`, `activate`, `arm_bootstrap` and `retire`.
Responses contain redacted `owner_held`, `local_ready`, `serve_ready`, `ready`,
`quarantined`, `route_cleared`, `local_watch_closed` and
`remote_watch_retirement_unknown` facts, plus the existing release and
invitation status. No watcher ID, LocalAPI token or session handle crosses the
socket. The localcontrol server checks the exact run/instance identity, bounds
request and response JSON, rejects duplicate/case-conflicting members, and
cancels an operation context when the client disconnects.

Read/status, activation, arming and retirement have explicit 30s, 60s, 60s and
40s operation deadlines respectively. The 60-second budgets cover multiple
bounded five-second LocalAPI calls plus release/TLS health checks; callers may
use shorter contexts, in which case the result is not guessed. A lost arm
acknowledgement remains ambiguous to the caller, so the existing reprint journal
stays staged and no link is printed.

## Quarantine and owner release

A live owner/route/identity/health failure revokes the gate, closes Hub session
admission and current clients, and makes all HTTP endpoints inert while leaving
the backend listener and private control available. Retirement does this before
calling selective `SessionAuthority.Retire`. The process returns and releases O
only after exact config-level route clearing and local watch-reader closure are
reported. `remote_watch_retirement_unknown` remains true: the pinned daemon
API has no watcher-GC acknowledgement. A signal is handled as a bounded retire
request, not as permission to cancel the watcher or release O. On unresolved
cleanup, the process stays alive, retains O/control and accepts an explicit
later status/retire attempt; a second signal does not certify cleanup.

An unsolicited watch/identity/route failure follows the same quarantine path.
No automatic takeover or global Serve reset is performed. The upstream same-key
replacement limitation and other LocalAPI/runtime limits remain as described in
[`SESSION-CONTRACT.md`](../tailscale/SESSION-CONTRACT.md).

## Verification map and limits

The new bootstrap-gate unit tests exercise transient refusal for an expired
invitation without store mutation and close serialization with an in-flight
resolver. A deterministic LocalAPI `SessionAuthority` test checks the canonical
prepared origin and safe pre-activation unwind without opening a watch or
writing a route. App-constructor tests use a package-private, prepared/not-
dispatched authority fixture only for refusal paths: they verify absent and
pre-existing device-auth bytes/modes are untouched, construction/status/rejected
activation/arm do not write or initialize the store, and unresolved retirement
keeps pairing revoked and O unsafe to release. They do not synthesize a live
route or bypass the production TLS health check. The private protocol has
bounded callbacks and an operation cancellation path; its real-socket tests are
hosted-CI tests, not run on the restricted local Mac. The positive app
activation-to-public-TLS-to-persisted-arm integration remains subject to
independent review and hosted caller coverage.

This slice does not change `relay/tailscale.sh`, does not select or install a
Tailscale distribution, and does not execute Tailscale commands/LocalAPI,
inspect personal runtime paths, or qualify daemon behavior. It does not prove a
real node's TLS health, filesystem permissions, Herdr inventory, browser or
phone behavior. Static build and deterministic fake-LocalAPI tests are not
runtime qualification. The ordinary `check` and `Tailscale native preflight`
workflow results on the final source SHA are required evidence before review.

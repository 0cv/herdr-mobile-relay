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
also runs `appdeploy.VerifyPublic`. Arming first performs the complete trusted
readiness/bundle preflight, then opens the existing device store read-only and
deferred (without creating or rewriting it), and performs the same live checks
again inside the synchronized route, gate/revocation and owner transaction
before durable arming. The store syncs the invitation and runs a final
context/managed-owner check before the transaction succeeds; refusal restores
exact prior bytes/modes or retains explicit recovery evidence and quarantine.
The successful durable write under those locks is the commit boundary; only
then does the gate open and Hub admission resume. After it, disconnect/lost ACK is committed
or ambiguous and never triggers rollback of exposed/consumed credentials. No
QR is printed by this Go slice.

## Private control status and bounds

The private protocol exposes `status`, `activate`, `arm_bootstrap` and `retire`.
Responses contain redacted `owner_held`, `local_ready`, `serve_ready`, `ready`,
`quarantined`, `route_cleared`, `local_watch_closed` and
`remote_watch_retirement_unknown` facts, plus the existing release and
invitation status. No watcher ID, LocalAPI token or session handle crosses the
socket. The localcontrol server checks the exact run/instance identity, bounds
request and response JSON, rejects duplicate/case-conflicting members, and
cancels an operation context when the client disconnects.

Read/status, activation, arming and retirement have explicit 30s, 4m, 7m and
40s operation deadlines respectively. The public bundle verifier retains its
existing two-minute bound and production TLS verification; this slice does not
shorten it or alter non-Tailscale defaults. Activation has one full bundle pass
plus the surrounding bounded owner/health checks. Arming allows two full bundle
passes (before deferred store attachment and again under the synchronized
commit gate), multiple bounded LocalAPI calls, and surrounding five-second
health checks. A caller may still provide a shorter context; incomplete results
are not guessed. Deadline relationship tests pin these margins. A lost arm ACK
after durable commit remains committed/ambiguous to the caller, so the existing
reprint journal stays staged and no link is printed.

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
A primary `http.Server.Serve` error also revokes pairing and starts retirement;
while cleanup is unresolved, a retained-listener view keeps the actual bound
port reserved and hands the backend to an inert HTTP 503 server. Listener close
is allowed only after both route clearing and local watch closure are proven.
This prevents ordinary `Serve` return/`Shutdown` from releasing the socket, but
cannot prevent an external process from forcibly closing the FD, kernel loss, or
process termination. No atomic port reacquisition is claimed. No automatic
takeover or global Serve reset is performed. The upstream same-key replacement
limitation and other LocalAPI/runtime limits remain as described in
[`SESSION-CONTRACT.md`](../tailscale/SESSION-CONTRACT.md).

## Verification map and limits

The strictly tagged (`herdr_tailscale_test`) hosted tests in
`tailscale_integration_test.go` construct the real `SessionAuthority` over a raw
pinned-protocol LocalAPI fake, use the actual backend and private localcontrol
socket, check public health and the complete release bundle over TLS with a
generated fixture CA and normal hostname verification, durably arm,
authenticate and enroll through the real encrypted WebSocket/BootstrapGate
path, and retire while closing the admitted client through the fixture's public
TLS reverse proxy. They prove activation alone leaves WebSocket admission at
HTTP 503, and late-failure cases cover the second local health, public health,
bundle identity and owner checks. Delayed verifier fixtures exercise successful
bounded activation/arm, cancellation during activation and final arm admission,
and a lost-control-ACK case asserts that a committed invitation is not rolled
back. `check.yml` runs all five named root cases with `go test -json`, fails on
missing, skipped, failed or zero test cases, and uploads only sanitized evidence
containing the exact candidate SHA, observed test count and required-case
results. The tagged job uses no credentials; its evidence excludes test output,
private keys, QR data, credentials and profiles. Tagged integration coverage is
not exact-release acceptance; the later packaged-browser gate remains
required. These tests and amended sources have NOT been formatted, built, or
executed by this worker; no hosted result or independent approval is claimed.

Authorization history must remain explicit: a prior R2C worker ran broad Go,
race and socket tests on this Mac despite the explicit local-execution ban. The
former statement that real-socket tests had not run locally was false. No proof
that those unauthorized runs had no host side effects exists. This worker used
only source read/edit/write tools and ran no commands/tests. Future tests must
run on disposable hosted CI; hosted success, when obtained, is evidence of test
behavior, not retroactive authorization of the earlier local runs.

The original R2C scope was also exceeded by edits to
`internal/transport/ws.go`, `internal/transport/managed_admission_test.go`, and
`internal/tailscale/session_test.go`. Those paths are prospectively allowed in
this correction round for necessary gate/admission behavior and tests; that
later allowance does not make the original edits retroactively authorized.

This slice does not change `relay/tailscale.sh`, does not select or install a
Tailscale distribution, and does not execute Tailscale commands/LocalAPI,
inspect personal runtime paths, or qualify daemon behavior. It does not prove a
real node's TLS health, filesystem permissions, Herdr inventory, browser or
phone behavior. Static build and deterministic fake-LocalAPI tests are not
runtime qualification. The ordinary `check` and `Tailscale native preflight`
workflow results on the final source SHA are required evidence before review.

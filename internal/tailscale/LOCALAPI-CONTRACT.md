# Bounded LocalAPI transport contract (R2A)

## Scope and authority boundary

This package-internal adapter is a transport slice for a later same-package
session owner. It does **not** enable callers, create `SessionAuthority`, clean up
Serve state, or change `Inspect`; the existing CLI adapter remains read-only and
`ServeRouteOwned=false`. A route, status response, PID, health check, copied
session ID, or this transport alone does not establish relay ownership.

The only production constructor is unexported `newLocalAPI(expectedVersion,
versionMetadata)`. It requires independently obtained, source-valid version
metadata and fixes the LocalAPI host, paths, transport, and platform socket
policy. The LocalAPI-specific admission additionally requires `extraGitCommit`
and `osVariant` to be absent or empty; supplemental GUI builds need a separately
source-qualified exact policy and are refused here. Matching source metadata is
not cryptographic artifact provenance. The legacy `Inspect` version contract is
unchanged. Test injection exists only in package `_test.go` code and uses an
in-memory `RoundTripper`; there is no environment, CLI, arbitrary-URL, or fake
owned-session escape hatch.

## Pinned source and request protocol

The source candidate is `tailscale.com v1.102.4`, Git origin
`https://github.com/tailscale/tailscale`, tag `refs/tags/v1.102.4`, commit
`bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`. The module checksum is
`h1:FcAkb7MgfFFUIUASg18Mv5cAiraxb+eMgOQaOKqKyPo=` and its go.mod checksum is
`h1:47bv91Xbg4K1p5wti7F1dmKvUVWV5BXF78d9EWJ+d6c=`. The upstream go.mod declares
Go 1.26.6; this project remains on Go 1.27.1.

The adapter permits only these requests to `http://local-tailscaled.sock`:

| Method | Exact path | Contract |
| --- | --- | --- |
| GET | `/localapi/v0/status` | Bounded complete JSON; existing strict status parser; response status version and `Tailscale-Version` must equal the independently admitted pinned version. |
| GET | `/localapi/v0/serve-config` | Bounded complete JSON object, or source-valid `null` for absent config; exactly one nonempty source SHA-256 ETag (64 lowercase hex characters); response version checked. The raw value is preserved. |
| POST | `/localapi/v0/serve-config` | One conditional attempt only; a nonempty source-shaped ETag is mandatory; config must be one bounded strict JSON object before dispatch. No idempotency header or body replay handle. |
| GET | `/localapi/v0/watch-ipn-bus?mask=NotifyInitialState` | Long-lived newline-delimited JSON stream; bounded first event and locally retained initial nonempty session ID. |

No other methods, query strings, path aliases, hosts, schemes, proxy authorization,
redirect targets, custom sockets, or network endpoints are accepted. The configured
`http.Transport` has no `ProxyFromEnvironment` and dials only the pinned local
Unix socket; Darwin loopback and token discovery are refused in this slice. It
caps response headers at 64 KiB and disables transparent compression/keep-alive.
One-shot requests (including response reads)
use five-second contexts; each response body and each watch JSON event is capped
at 1 MiB. The existing `strictJSON` enforces the byte, UTF-8, single-document,
depth, value,
duplicate-name, and case-conflict rules. Watch lines require a terminating LF;
unterminated, malformed, oversized, invalid UTF-8, wrong-version, or unexpected
post-initial-session events fail closed. Cancellation closes the body and joins
the local reader. The five-second initial-watch deadline includes connection,
headers, and the first complete event.

Errors are fixed, sanitized diagnostics (well below 64 KiB). The adapter never
returns or logs response bodies, headers, authorization, LocalAPI proof tokens,
token paths, or session IDs. The response-header cap is independent of these
short diagnostics.

## Conditional write outcome

Local validation failures (including missing/empty/invalid `If-Match`) are
`not-dispatched` and send zero requests. After dispatch, only a complete response
with the exact admitted `Tailscale-Version` can settle an outcome:

- `200` with a complete empty body is `settled-success`, matching the pinned
  `serve-config` POST handler's success response.
- `412` with the exact complete body `etag mismatch\n` and source
  `text/plain; charset=utf-8` content type is `settled-no-write`: the pinned
  handler emits that fixed `http.Error` response only for an ETag mismatch
  before updating config. Any other 412 body is unresolved.
- Every timeout, reset, truncation, oversize, malformed or unexpected response,
  wrong version, or other status is `unresolved`.

There is no readback, replay, reconciliation, or cleanup here. A `412` body is
consumed but never surfaced. POST construction clears `Request.GetBody`, sends no
idempotency key, and the production transport disables keep-alives. Upstream
`DoLocalRequest` invokes one `http.Client.Do`. Go 1.27.1's `net/http.Client` follows
redirects when a redirect response has `Location` (`src/net/http/client.go`,
`redirectBehavior`/`Client.do`, around lines 502–511 and 651–661); the wrapper
removes `Location` from every 3xx before the client sees it, so neither a new
method nor the sensitive authorization header can be forwarded. Go's
`net/http.Transport` retries only selected reused-connection failures
(`src/net/http/transport.go`, `shouldRetryRequest`, around lines 841–875);
keep-alives are disabled and the POST has no body replay function or idempotency
key, so config registration is not retried. These are source/version-specific
claims, not a daemon runtime qualification.

## Upstream seam and platform limits

The adapter uses `client/local.Client.DoLocalRequest` for Tailscale capability
and request handling. In the pinned source, that method creates an `http.Client`
with the configured `Transport`, but exposes no `CheckRedirect`, timeout, or
response-header-size option (`client/local/local.go`, `Client`, `defaultDialer`,
and `DoLocalRequest`). The small documented duplication is its local
`DialContext` selection through `safesocket.ConnectContext` and
`paths.DefaultTailscaledSocket`, exported source-backed primitives. The request
still goes through `DoLocalRequest`, which adds Tailscale capability metadata.

On Darwin, `DoLocalRequest` also calls `safesocket.LocalTCPPortAndToken`
synchronously. The pinned App Store discovery path invokes `lsof` with
`exec.Command(...).Output()` and has no context/deadline seam. To preserve the
five-second request bound without adding unbounded process/token discovery, the
Darwin constructor sets upstream `OmitAuth=true` and accepts only the
peer-credential standalone Unix socket. Sandboxed GUI/App Store/MacSys loopback
and proof-token discovery is deliberately unsupported/refused in R2A; it needs a
separately reviewed bounded source/auth seam. No credential discovery, external
process, or daemon connection was run for this slice.

- Linux accepts only the generic upstream default
  `/var/run/tailscale/tailscaled.sock`; Unix peer credentials remain the
  source-backed authorization boundary. Distro-specific paths, custom sockets,
  and unknown OS targets refuse.
- Darwin accepts only the pinned standalone socket
  `/var/run/tailscaled.socket` with OS peer credentials. Sandboxed GUI,
  App Store, and MacSys distributions are a distinct source cell and are
  refused rather than entering the unbounded upstream `lsof` token path.
- Only Linux and macOS runtimes are admitted. Android (despite its Linux build
  tags), iOS (despite its Darwin build tags), Windows, and other targets refuse
  at construction; platform dialers also check the exact runtime OS. There is
  no CLI fallback, login, sudo, feature/prefs mutation, token extraction, or
  installation.

The transport source seams were inspected in immutable Tailscale commit
`bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`:

- [`client/local/local.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/client/local/local.go),
  `Client`, `defaultDialer`, `DoLocalRequest`, and `WatchIPNBus`.
- [`safesocket/safesocket.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/safesocket/safesocket.go),
  `LocalTCPPortAndToken` and `ConnectContext`.
- [`safesocket/safesocket_darwin.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/safesocket/safesocket_darwin.go),
  Darwin same-user proof discovery/fallback.
- [`paths/paths.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/paths/paths.go),
  default socket selection.
- [`ipn/localapi/serve.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/localapi/serve.go),
  Serve config GET/POST, ETag, and 412 behavior.
- [`ipn/localapi/localapi.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/localapi/localapi.go),
  version headers and newline-encoded watch notifications.
- [`ipn/backend.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/backend.go),
  initial-state mask and `Notify.SessionID` contract.
- [`ipn/ipn_view.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/ipn_view.go)
  plus [`client/local/serve.go`](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/client/local/serve.go): an absent
  `ServeConfigView` marshals as JSON `null`; upstream's typed getter normalizes
  that value to an empty struct. This raw adapter accepts strict whole-document
  `null` and preserves it for the later schema-aware owner.

SHA-256 of the locally inspected source files (all from the pinned commit):

| Source | SHA-256 |
| --- | --- |
| `client/local/local.go` | `c3fd3412fa51656d4daccdb2f586b9e238c642cd3e5a059f3ebd7be00acb8f72` |
| `client/local/serve.go` | `5f6953d80408cf9f4805cd7a24773e06b8b36873d5f36c5587bb01578991d7c0` |
| `safesocket/safesocket.go` | `aa01cd26a62f0218f481f339eb32812c176ab9361c73ce5764dd7a1f4cd3ab73` |
| `safesocket/safesocket_darwin.go` | `11ff8c307c44b3fb3f2fd6da20a2acf0e1d18c88d5949cfd75e0051222995e7d` |
| `paths/paths.go` | `e534ebb1ed064b761473b1bbb883403212a76749bc7d2227ed2b6cb88bb6c157` |
| `ipn/localapi/serve.go` | `b72a474e83d6c5bf9278f210470796e0790c832610e538df5b67d37311025aac` |
| `ipn/localapi/localapi.go` | `b13da90ef452b4e9ab01bc123a55cb11650e28d38207ef72359b63a225a1dc47` |
| `ipn/backend.go` | `e71a93c81358c8916de1b6cc1272985268699fc66096944cca8924e4c5725e13` |
| `ipn/ipn_view.go` | `421bbb585309f5c0fbc73ad5b8da596a34b24598e76764c54c988563fae3acd4` |

The Go 1.27.1 sources used to verify redirect/retry behavior were
`src/net/http/client.go` (`ced3428a85206de8de79c10de38d34951e0b9823c0ccb68ff51329d048a1f7b9`)
and `src/net/http/transport.go` (`b3c0ef6ea21d8bfa3d8ee4d6f53e3f5e5982c5d6bfcd14536437adeb214a6933`).

The source-versioned transport does not prove installed artifact provenance,
LocalAPI permissions, daemon compatibility, or runtime watcher lifecycle.

## Dependency measurement and limits

The approved throwaway import-closure measurement used Go 1.27.1 and
`CGO_ENABLED=0`: Darwin amd64/arm64 each had 317–318 packages and 15 external
modules; Linux amd64/arm64 each had 324–325 packages and 18 external modules.
After this adapter import, static `go list -deps ./internal/tailscale`
measurements under the same four targets were 319/318/326/325 packages and
15/15/18/18 third-party modules (excluding the relay root module). The
Linux-only modules were `github.com/jsimonetti/rtnetlink`,
`github.com/mdlayher/netlink`, and `github.com/mdlayher/socket`. Existing higher
project versions win MVS for `github.com/coder/websocket` (1.8.15),
`golang.org/x/crypto` (0.55.0), and `golang.org/x/net` (0.58.0); selected closure
versions are recorded by `go.mod`/`go.sum`.

Tailscale's module graph requires `go.etcd.io/bbolt v1.4.2`, so MVS upgraded the
relay's existing direct bbolt from v1.3.11; it also selected `golang.org/x/sync
v0.22.0` (previous full module checksum was v0.10.0). The bbolt package is used
by existing `internal/conversation` code, not this LocalAPI package closure.
Cross-builds compile against the selected version, but no conversation/database
runtime tests were run in this transport-only slice; compatibility deserves
review/coverage in the later integration regression slice. These are required
module-graph selections, not optional feature dependencies.

The pinned root license is BSD-3-Clause; 17 transitive root-license
classifications were recorded in the private measurement, but this was not a
per-file SPDX audit. The sampled unstripped Darwin/arm64 throwaway binary grew
by 9,909,632 bytes versus an import-free throwaway baseline. That is **not** the
relay binary size or a final release-size/license audit.

## Required next owner contract

A later same-package owner may receive the initial session ID only from this
live `localAPIWatch`. No public caller can construct ownership from an observed
ID. The owner must retain the watch and reconcile the complete
`Foreground[sessionID]` while the watch remains live before intentional local
cancellation. `localAPIWatch.Close` closes the local response and joins its
reader only; it is not evidence of daemon-side watcher retirement, route
absence, a cleanup deadline, or permission to release the managed owner. No
`SessionAuthority`, callers, cleanup, readiness, or feature enablement are
implemented in this transport slice.

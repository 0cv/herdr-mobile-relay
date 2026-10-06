# Bounded LocalAPI transport contract (R2A / R2D fixture transport)

## Scope and authority boundary

This bounded adapter is used by the same-package in-process owner described in
[`SESSION-CONTRACT.md`](SESSION-CONTRACT.md). It still does **not** enable app,
shell, bootstrap, or pairing callers, and it does not change `Inspect`; the CLI
adapter remains read-only and `ServeRouteOwned=false`. A route, status response,
PID, health check, copied session ID, or this transport alone does not establish
relay ownership.

The only production constructor is unexported `newLocalAPI(expectedVersion,
versionMetadata)`. It requires independently obtained, source-valid version
metadata and fixes the LocalAPI host, paths, transport, and platform socket
policy. The standalone cell additionally requires `extraGitCommit` and
`osVariant` to be absent or empty. App Store metadata selects a separate
profile-gated cell described below; no metadata falls back between cells.
Matching source metadata is not cryptographic artifact provenance. The legacy
`Inspect` version contract is unchanged. Ordinary unit tests use in-memory `RoundTripper`s in `_test.go` files.
The positive hosted app test uses `session_testbridge.go`, which is compiled only
with the explicit `herdr_tailscale_test` build tag and accepts only a raw
protocol `RoundTripper`; fixed LocalAPI host/path allowlists and production
response validation still execute. It exposes no owned/readiness injection and
is absent from normal production builds. There is no environment, CLI, or
arbitrary-URL escape hatch.

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
| GET | `/localapi/v0/status` | Bounded complete JSON; existing strict status parser; response status version and `Tailscale-Version` must equal the independently admitted pinned version. The session owner additionally validates pinned `Self.CapMap` and requires the exact `https` capability key (`tailcfg.CapabilityHTTPS`); legacy `Capabilities` and `CertDomains` alone do not qualify. |
| GET | `/localapi/v0/serve-config` | Bounded complete JSON object, or source-valid `null` for absent config; exactly one nonempty source SHA-256 ETag (64 lowercase hex characters); response version checked. The raw value is preserved. |
| POST | `/localapi/v0/serve-config` | One conditional attempt only; a nonempty source-shaped ETag is mandatory; config must be one bounded strict JSON object before dispatch. No idempotency header or body replay handle. |
| GET | `/localapi/v0/watch-ipn-bus?mask=2` | The pinned `ipn.NotifyWatchOpt` text codec encodes `NotifyInitialState` as decimal `2`; the daemon parses `mask` with `strconv.ParseUint`. Long-lived newline-delimited JSON stream; bounded first event and locally retained initial nonempty session ID. |

No other methods, query strings, path aliases, hosts, schemes, proxy authorization,
redirect targets, custom sockets, or network endpoints are accepted. Each admitted
cell uses an `http.Transport` without `ProxyFromEnvironment`; the standalone
cells dial only their pinned local Unix sockets, while the disabled App Store
cell's separate loopback restriction is specified below. Transports cap response
headers at 64 KiB and disable transparent compression/keep-alive.
One-shot requests (including response reads)
use five-second contexts; each response body and each watch JSON event is capped
at 1 MiB. The existing `strictJSON` enforces the byte, UTF-8, single-document,
depth, value,
duplicate-name, and case-conflict rules. The watch allowlist requires the pinned numeric query `mask=2`; symbolic Go constant names are not accepted by the daemon's decimal parser. Watch lines require a terminating LF;
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

On Darwin, `DoLocalRequest` can call `safesocket.LocalTCPPortAndToken`
synchronously unless every client sets `OmitAuth=true`. The pinned App Store
fallback uses loopback TCP and HTTP Basic auth, while its same-user-proof lookup
invokes `lsof` with `exec.Command(...).Output()` and has no context/deadline
seam. Both LocalAPI cells therefore keep upstream auth discovery disabled. The
standalone Darwin cell continues to use only its peer-credential Unix socket.
The separate App Store cell below supplies a bounded wrapper around the exact
upstream discovery function and adds the corresponding Basic header in the
existing bounded transport. The wrapper times out but cannot cancel an already
running upstream lookup.

- Linux accepts only the generic upstream default
  `/var/run/tailscale/tailscaled.sock`; Unix peer credentials remain the
  source-backed authorization boundary. Distro-specific paths, custom sockets,
  and unknown OS targets refuse.
- Darwin standalone accepts only the pinned socket `/var/run/tailscaled.socket`
  with OS peer credentials. App Store GUI discovery is a distinct, disabled
  LocalAPI source cell; MacSys remains unsupported.
- Only Linux and macOS runtimes are admitted. Android (despite its Linux build
  tags), iOS (despite its Darwin build tags), Windows, and other targets refuse
  at construction; platform dialers also check the exact runtime OS. There is
  no CLI fallback, login, sudo, feature/prefs mutation, token extraction, or
  installation.

## App Store LocalAPI cell (R2D; fixture-only, not runtime-qualified)

The in-process `SessionAuthority` has a second, Darwin-only transport cell for
the App Store GUI LocalAPI. It remains unconditionally disabled by
`appStoreLocalAPIProfileEnabled = false`; the production exact-profile table
`appStoreLocalAPIProfiles` is empty. The standalone Unix-socket cell is not a
fallback: admitted metadata selects one cell, and a refused App Store identity
never tries the standalone socket. The separate `Inspect`/CLI version admission
still rejects the App Store `gitCommit` variant; that remains a documented
follow-up and is not changed by this transport slice.

The cell reuses the pinned v1.102.4 upstream implementation, not a new protocol:
`client/local/local.go` (`Client`, `defaultDialer`, `DoLocalRequest`) supplies the
fixed `local-tailscaled.sock:80` HTTP endpoint, and
`safesocket/safesocket.go` / `safesocket/safesocket_darwin.go`
(`LocalTCPPortAndToken`, `localTCPPortAndTokenDarwin`,
`portAndTokenFromSameUserProof`) supplies the only discovery source through the
unexported `appStoreDiscover` seam; `dialLoopbackTCP` defaults to
`(&net.Dialer{}).DialContext`. Every `local.Client` has `OmitAuth=true` and
explicit `Dial`/`Transport`; the bounded wrapper calls `SetBasicAuth("", token)`
once. Discovery runs in a goroutine with
a two-second deadline, but the upstream lookup has no cancellation seam and may
continue after timeout. Discovery is one-shot per client; 401/403 do not
rediscover or retry. Only ports 1..65535 and 1..256 printable ASCII token bytes
excluding colon and whitespace are admitted. The HTTP dial accepts only
`local-tailscaled.sock:80` and redirects only to the discovered
`127.0.0.1:<port>`; alternate networks, hosts, ports, localhost, IPv6 and remote
addresses are refused before dialing. Token and port are absent from fixed
errors, string renderings, status and logs.

Tests use only synthetic metadata/credentials and in-memory RoundTrippers or
`net.Pipe`; the package `TestMain` replaces both production discovery and dial
seams with fail-fast guards. No real same-user-proof lookup, socket, daemon, or
LocalAPI was accessed. This is fixture-only; it is not runtime-qualified and
does not enable pairing or live authority.

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

## Owner binding (R2B)

`SessionAuthority` now receives the initial session ID only from its live
`localAPIWatch`; no public caller can construct ownership from an observed ID.
It retains the watch and reconciles the complete `Foreground[sessionID]` while
the watch is live before intentional local cancellation. `localAPIWatch.Close`
closes the local response and joins its reader only; it is not evidence of
daemon-side watcher retirement, route absence, a cleanup deadline, or
permission to release the managed owner. See `SESSION-CONTRACT.md` for the
implemented owner behavior and the remaining caller/runtime qualification
limits.

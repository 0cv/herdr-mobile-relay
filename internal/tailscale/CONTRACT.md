# Read-only Tailscale source contract (S5)

## Authority, not runtime qualification

This adapter targets **exactly 1.102.4**, upstream commit
`bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8` (tag v1.102.4, annotated tag object
`3caf7d9e7dcaba589cfc58beda596929733e4fea`). It does not mean “at least” that
version. Intended native Linux/macOS distributions, architectures, actual long
build strings, CLI/daemon artifacts and lifecycle behavior remain unqualified.
No actual Tailscale binary was downloaded, installed, compiled or executed by
this slice. Public source copies and their hashes are recorded by the external
S5 reference manifest; GitHub signature verification was false/unknown_key,
**not verified signing**. Version metadata is not cryptographic artifact proof.

All following links are immutable upstream source at that commit:

- [ServeConfig, HostPort and handlers, ipn/serve.go:46–186](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/serve.go#L46-L186).
- [SetWebHandler / SetFunnel, ipn/serve.go:419–533](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/serve.go#L419-L533).
- [Actual JSON Serve status, serve_legacy.go:616–628](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_legacy.go#L616-L628) marshals the complete config, including Foreground; a nearby stale TODO does not override that code.
- [Nil configuration to empty struct, client/local/serve.go:22–37](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/client/local/serve.go#L22-L37). The local API is explicitly not stable across releases.
- [Status identity types, ipnstate.go:31–95,175–192,234–260](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/ipnstate/ipnstate.go#L31-L95).
- [Version command, version.go:21–81](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/version.go#L21-L81), [metadata, version/prop.go:257–324](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/version/prop.go#L257-L324), [long/short formats, version/version.go:60–104](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/version/version.go#L60-L104).
- [Foreground flags, serve_v2.go:239–262](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_v2.go#L239-L262), [watcher/config lifecycle:490–558](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/cmd/tailscale/cli/serve_v2.go#L490-L558), [SessionID, backend.go:304–308](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/backend.go#L304-L308).
- [Session removal, ipnlocal/serve.go:425–433](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/ipnlocal/serve.go#L425-L433), [inactive-session filtering:1646–1650](https://github.com/tailscale/tailscale/blob/bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8/ipn/ipnlocal/serve.go#L1646-L1650). These establish no finite cleanup deadline or process ownership attestation.

## Decoding and supported exposure subset

Every input is bounded to 1 MiB including whitespace, valid UTF-8, one JSON
value, maximum nesting depth 32 and at most 100001 visited values. A token pass
rejects duplicate decoded member names (including escaped spellings) and
case-conflicting members in **every** object before decoding. Explicit exact-key
allowlists and typed scalar decoding prevent encoding/json's case-insensitive
struct-field matching and null-to-zero behavior from granting authority.
Rejection returns an error, never a complete partial configuration. Unknown
exposure members are refused even alongside valid known fields. Work is bounded
by the byte/value/depth limits; no recursive Foreground is supported.

Only root `{}` (optionally whitespace) means canonical absence. Known empty
maps, null, false, arrays and unrelated documents are errors. Nonempty
AllowFunnel maps with false entries remain **configured retained state**, not
absence; they also disqualify exact matching. Boolean true at root or inside
Foreground is detected as Funnel. Invalid Funnel types refuse the whole result.
A complete result never means a configuration is safe to adopt.

Supported configuration fields are exact `TCP`, `Web`, `AllowFunnel`, and
`Foreground`. `Services` (including Tun), TCP forwarding, HTTP listeners,
TerminateTLS, ProxyProtocol, filesystem/text/redirect/app-capability handlers,
and every unknown extension are deliberately unsupported and refused (even if
empty or false). This is a conservative subset, not implementation of all Serve
modes. A TCP listener is exactly HTTPS=true. Web keys are lowercase canonical
`host:explicit-port`, never URL strings; ports are canonical decimal 1–65535.
Each Web listener must have its TCP counterpart, and every TCP listener must
have Web handlers. A TCP plus Web association is **one route per handler**, not
two routes. Handlers are nonempty exact Proxy objects at absolute mount paths;
proxy targets are full HTTP(S) URLs with valid host/port, no credentials,
query, fragment or whitespace (shorthand targets are conservatively refused).
Foreground sessions are nonempty IDs with nonempty configs and cannot nest.

Observed `Route` values retain session, listener type, host, port, handler type,
mount path and backend, sorted lexicographically by their fixed-field JSON
encoding. Background routes have an empty session. No observed nested key
shadows shell-consumed top-level metadata names.

## Authenticated identity and version admission

Running is exact-case. A usable identity requires a nonempty **string** Self.ID,
a positive numeric Self.UserID, a usable dotted Self.DNSName with its source
trailing dot, and CurrentTailnet Name/MagicDNSSuffix/MagicDNSEnabled of the
correct types. DNS must belong to that suffix. A present nonempty legacy
MagicDNSSuffix must agree. No numeric ID coercion or Running+empty Self is
accepted. CertDomains and available User profile data are validated and retained;
inspection exposes account strings and tailnet/DNS/certificate data separately.
No account/tailnet UUID or unsupported profile semantics are invented. Nullable
CertDomains/User are allowed as the source slice/map can be nil; this is **not**
an HTTPS prerequisite guarantee. Absent account profiles are not fabricated.

Unrelated status telemetry (Peer, IPs, traffic counters, health, capabilities,
other Self telemetry, AuthURL, ClientVersion, etc.) is ignored after global JSON
integrity checking. Relevant identity casing and underscore/hyphen aliases are
refused. Non-running known states remain logged out diagnostics; Inspect returns
an error and never queries Serve or marks them complete. Relevant authenticated
fields are validated before LoggedIn is set. Status.Version is the daemon long
version and participates in version policy, not alone in authentication.

Inspect's read-only order is `status --json`, then `version --json --daemon`,
then `serve status --json`. No `--upstream` is used. Version fields are exact
lowercase metadata names, with typed optional flags and supplemental strings.
majorMinorPatch and short must both be 1.102.4; gitCommit is mandatory and must
be the full pinned commit. Long, daemonLong and Status.Version must agree.
Release long strings accept a 7–40 character prefix of the pinned commit,
optionally prefixed `t`, with an optional supplemental hash (optionally `g`)
matching an available full 40-hex extraGitCommit. Development, dirty, unstable,
untagged changecount, missing identity and inconsistent values refuse. This
accepts a family of documented release formats, not an invented unique official
binary string; actual distribution qualification is still required. Validated
version metadata is retained in inspection output.

`serve_inspected` and `exposure_complete` are set together only after supported
version, identity and **all** Serve scopes validate. They remain false for
skipped queries, timeout/command errors, malformed/truncated output and unsupported
versions. Consumers must not infer absence from default false configuration
booleans. All six consumers (preflight, post-confirmation, cleanup retirement,
startup readiness, monitoring, reprint) now require both flags explicitly true;
Funnel must be explicitly false.

## Matching is not ownership

`ExactRouteMatch` requires complete configured single-route state without any
Funnel state and compares against an **independently supplied** foreground
session, canonical HTTPS host/port, Proxy `/` and exact
`http://127.0.0.1:PORT` backend. Wrong session, same-origin replacement, changed
backend/port/path/handler, background and extra routes refuse. It proves tuple
equality, not process ownership. Inspect has no independently owned session or
backend input and always leaves `serve_route_owned=false`. Startup therefore
cannot arm/print pairing based on this adapter alone. This temporary conservative
refusal is intentional pending S6/S9 ownership binding, not product readiness.

## Fixture corrections and evidence boundaries

All positive fixtures are **source-derived synthetic inputs**, not CLI captures.
The prior pure status fixture's numeric Self.ID became a stable string plus
required source identity/version fields. URL-form Web keys became host:port
keys with matching HTTPS TCP entries. The old sparse empty-map “absence” became
canonical `{}`. Old sparse false-Funnel and object-valued Funnel cases now assert
errors, not successful absence or invented object-as-true compatibility. Valid
false-Funnel retained-state behavior is tested separately. The existing
subprocess test received only fixture/expectation adaptation; S5 never selects
or executes it.

The two old-API prepatch tests genuinely failed at behavioral assertions before
production changes; exact bytes and baseline hashes were retained externally.
The selected 13 Go tests use only in-memory inputs and a strict runner callback
(no exec, sockets, real Inspect, product filesystem or fuzzing). The separate
19-case consent regression uses exact copied scripts with inert adapters in the
approved namespace sandbox; original positive cases still intentionally stop at
the second-inspection error. Eight new first/second missing/false completeness
scenarios refuse before mutation. Reaching consent is not successful exposure.

PR-TS-003 parsing and PR-TS-008/009 model foundations are scoped coverage, not
runtime closure. PR-TS-007 second-identity comparison, HTTPS prerequisites and
ownership (S6), output-pipe supervision and lifecycle (S9), old shell fixture argv
adaptation (S12), product docs (S13), native/browser/tailnet/L2 acceptance remain
open. Guard additions alone do not qualify full marker retention or cleanup.

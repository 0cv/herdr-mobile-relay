# DR-1: Independently bound Tailscale Serve session authority

**Status:** Draft decision record; source review and owner decision pending. This is not an implementation approval or a claim of runtime qualification.

## Decision requested

Keep the foreground Tailscale feature fail-closed unless a supported interface independently binds this relay invocation to the exact Tailscale Serve session it may observe and clean up. The currently pinned adapter does not provide that binding. Do not set `ServeRouteOwned=true` by observing a matching route.

## Evidence and current limit

The repository adapter is pinned to Tailscale 1.102.4 / `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8` in [`CONTRACT.md`](CONTRACT.md). It obtains read-only status, version metadata, and Serve status; Serve status can report Foreground configuration and the observed session label. `Inspect` has no independently supplied session authority and deliberately returns `ServeRouteOwned=false` ([`tailscale.go`](tailscale.go), `contract.go`). The launcher requires positive ownership before it arms pairing, prints a link, monitors the route, or removes a session.

The existing source contract records that the pinned CLI creates a private IPN watcher, obtains its `SessionID`, inserts foreground config under that session, and waits on the watcher. The inspected command/output contract does not provide the parent relay an independently bound session ID. A route observed after launching the CLI is therefore evidence about daemon state, not proof that this child invocation created or owns it.

This draft uses the repository's existing pinned-source contract and review notes. Upstream source was not re-fetched or independently re-reviewed for this draft, and no real CLI, daemon, or tailnet was run. Those limitations remain open evidence requirements.

## Required invariants

1. Authority must originate from a documented supported contract tied to this invocation and daemon-side session, not from the state the relay is trying to adopt.
2. Exact route equality (HTTPS listener, canonical host/port, `Proxy` `/`, loopback backend, session) is necessary for route qualification but is not ownership proof by itself.
3. Never infer authority from PID/job identity alone, origin/port matching, a newly observed route or route-set diff, relay health, a later observed session ID, or `expected = observed`.
4. Missing, malformed, stale, changed, or contradictory authority refuses startup/reprint/cleanup before pairing or foreign-resource mutation. Preserve the route and evidence when ownership cannot be proven.
5. HTTPS eligibility and certificate prerequisites are checked explicitly before invoking Serve. A noninteractive invocation or `--yes` is not itself proof that interactive feature enablement is safe.
6. Session authority and concurrent-config protection are separate obligations. A second read immediately before mutation narrows but does not close a race. Any chosen mutation contract must prove daemon-side conditional conflict behavior, preserve unrelated config, and test a foreign config change inserted between read and mutation. The pinned-source review notes ETag/`If-Match`; its actual daemon enforcement still requires source qualification and a fault-injection test.

## Options requiring an explicit owner decision

- Keep the feature unavailable and continue to refuse positive ownership.
- Adopt a supported upstream CLI contract that exposes an independently bound parent/child session authority, if one is documented and qualified.
- Scope a relay-owned, version-pinned LocalAPI/CLI adapter with explicit session lifecycle and conditional Serve-config mutation.
- Scope and maintain a custom CLI distribution that supplies the missing authority contract.

No option that expands LocalAPI mutation, supported CLI distribution, tracing, or version scope is selected here. Those are owner decision gates, not implementation details to infer from this draft.

## Current behavior and exit conditions

Until an option is explicitly approved and implemented, preserve `ServeRouteOwned=false`, retain caller fail-closed checks, and do not claim positive pairing, owned cleanup, or full feature readiness. The ADR can be source-reviewed as a design request; it does not close F04.2–F04.4 or the real native/tailnet acceptance gates.

A future approved implementation must test the real caller contract and its refused alternatives, including a foreign config inserted between read and conditional mutation, before any real two-node qualification. Synthetic adapters alone cannot close the runtime authority gate.

# Implementation plan: issues #37 and #38

Status: proposed; this task changes only this plan, not production code.

## 1. Scope, evidence, and implementation order

Reviewed the open issues and their bodies (neither had comments):

- [#37: Conversation history stops at a forked session boundary](https://github.com/0cv/herdr-mobile-relay/issues/37).
- [#38: Inventory status can stay stuck at "changing too quickly"](https://github.com/0cv/herdr-mobile-relay/issues/38).

Code baseline: `7f05acbc984e5c37dc2cfe3db29a3f8bc986a76c` on this checkout's
`dev` branch. The reports concern older releases; the findings below refer to
this checkout. Recheck the named functions if implementing after other changes
have landed. Function names are more reliable than issue-body line numbers.

Implement **#38 first**, then **#37**, in separate commits or PRs. They are
independent. #38 is a focused synchronization/publication repair; #37 touches
the history cursor contract and needs more extensive tests. Do not change Herdr,
rewrite transcript files, add dependencies, restart production services, or
change the relay's minimum supported Herdr version.

There is existing issue-#36 work in progress: untracked
`docs/issue-36-inventory-backoff-plan.md` and `internal/coordinator/poller_backoff_test.go`,
plus concurrent edits to `internal/coordinator/poller.go` and
`docs/cloudflare-tunnel.md`. Leave that work intact. Polling backoff is not a
prerequisite for either fix, but its changes must be preserved when editing the
same functions. Keep new retry-timing/failure-counter changes out of #38.

### Terms used below

- **Anchor session**: the session ID reported by Herdr and requested by the
  phone. In #37 it can remain A even after Claude starts writing session B.
- **Segment**: one Claude JSONL transcript file. A → B → C is three segments.
- **Recent page**: the bounded, immediate history read near a file's end.
- **Snapshot**: the existing background-prepared bbolt index for older history.
- **Live inventory status**: `coordinator.State.InventoryStatus()`.
- **Committed inventory status**: the cached status in `app.Server.inventoryView`
  that has passed through the outbound publication barrier.

## 2. Issue #38: prevent lost or reordered inventory recovery

### 2.1 What the current code establishes

Read these files before editing:

| File / function | Role |
| --- | --- |
| `internal/coordinator/state.go`: `MarkInventoryFailure`, `MarkTopologyDegraded`, `InventoryStatus`, `CommitPoll`, `CommitTopology` | Own live readiness and error fields under `State.mu`. Successful inventory commits clear the error. |
| `internal/coordinator/poller.go`: `poll`, `commitEventTopology`, `handleTopologyStale`, `notifyStatusChange` | Polling and the events loop can independently change status and invoke callbacks. |
| Same file: `notifyAgentsChanged`, `notifyWorkspacesChanged`, `broadcastMu` | Existing example of serializing a current-state read, deduplication, and publication. |
| `internal/app/server.go`: `SetOnInventoryStatus` callback and `broadcastCommitted` | Turn status into an `inventory_status` frame and update `inventoryView`. |
| Same file: `SetOnConnect`, refresh handler, `sendRequestedAgentRefreshes`, `publicInventoryStatus`, `committedInventoryStatus` | Handshake/refresh use the committed cache; `/healthz` uses live state. |
| `internal/transport/ws.go`: `BroadcastPrepared` and client registration | Serialize committed-cache updates with connection handshakes. |
| `frontend/src/lib/store.ts`: `inventory_status` handling | Replaces connection inventory status when a frame arrives; it does not independently consult `/healthz`. |

`notifyStatusChange(previous)` compares the live status after an operation with
that operation's earlier observation. It does **not** compare with the last
status published to phones. It also does not serialize status callbacks.
Only `state`, `error_code`, `message`, and `stale` participate in the comparison;
timestamps deliberately do not cause a push every poll.

A code-permitted lost-recovery sequence is:

1. A poll and an event-topology operation both capture `previous = ready`.
2. The poll degrades live state and publishes `error/topology_churn`.
3. The event operation commits successfully, restoring live state to `ready`.
4. The event operation compares its old `ready` with current `ready`, so it
   suppresses the recovery callback.
5. Subsequent healthy polls also compare `ready` with `ready`. The server cache
   and all phones can remain degraded indefinitely.

There is a second hazard: an error callback can capture its status, stall, and
publish after a newer ready callback. The transport registration lock orders
frames as they arrive; it cannot determine which captured status is newer.

These are explainable races in current code, **not proof of the exact schedule
on the reporter's machine**. Write deterministic reproductions before claiming
that the observed incident has been reproduced.

### 2.2 Chosen fix: one current-state inventory publisher

Use the **actual committed server view** as the publication baseline. Select
current state, compare, update the committed view, and queue the corresponding
inventory frames under the transport registration barrier. Preserve the wire
protocol and render-change deduplication.

A smaller change—remembering last-published status under `broadcastMu`—would
fix the two primary poll/event races, but leaves two related holes:

- Immediate and deferred refresh replies read status before `Hub.Send`, which
  does not hold the registration barrier. They can capture error, stall while a
  ready broadcast happens, then deliver the old error afterward.
- Separate status/agent publications permit a handshake to obtain new readiness
  with an old or empty agent cache. Deferred refresh replies also run only from
  the deduplicated agent-change callback; an unchanged poll need not complete a
  queued refresh.

Therefore implement the coherent publication path below, rather than adding a
second status cache in the poller or serving live status from selected getters.

#### A. `internal/coordinator/state.go`: capture one consistent value

Add an `InventorySnapshot` value with `Status map[string]any`,
`Agents []*AgentState`, and `Workspaces []herdr.Workspace`. Add
`State.InventorySnapshot()` that copies all three under **one** `State.mu.RLock`.

Refactor `InventoryStatus()` and `Snapshot()` through private lock-held helpers;
do not call public lock-taking getters recursively while holding the RWMutex.
Preserve pane sorting, displayed status/ack behavior, generations and revisions.
Copy nested agent options/interaction data and workspace worktree pointers;
reuse existing clone helpers where correct. Keep returned snapshots independent
of subsequent State mutations.

Do not change `CommitPoll` token checks, topology generations, or failure/stale
semantics. Timestamps and topology generation are not status-ordering sequence
numbers.

#### B. `internal/coordinator/poller.go`: notify without stale payloads

Replace the three publication callbacks with `SetOnInventoryChange(func() error)`
and one `notifyInventoryChange()` helper. The callback is a signal to read
**current** state; it carries no previously captured status/agents/workspaces.
Hold the existing `broadcastMu` through the synchronous callback so publication
and post-publication side effects remain serialized. Never hold `State.mu` while
calling it; register the callback before starting polling/events.

Invoke the helper once after each outcome:

- either required fetch marks inventory failure;
- `CommitPoll` succeeds;
- event bootstrap/update `CommitTopology` succeeds;
- a topology-stale sample is rejected, after its handler schedules a retry or
  marks degradation.

Remove `previousStatus` reads/arguments and the three old notification methods.
Remove `lastAgentsJSON`/`lastWorkspacesJSON`; their deduplication moves to the
server publisher. Preserve retry/backoff/cancellation behavior, including the
concurrent #36 work. A rejected sample without degradation simply publishes
whatever current state actually says. Log publication errors; a later attempt
must retry without needing another semantic transition.

#### C. `internal/transport/ws.go`: add ordered dynamic batch helpers

Keep `BroadcastPrepared` for unrelated single-message updates. Add:

```go
BroadcastBatchPrepared(build func() (messages []any, commit func(), err error)) error
SendBatchPrepared(client *ClientConn, build func() []any) bool
SendBatchPreparedByID(clientID string, build func() []any) bool
```

The broadcast helper's required order is:

1. Acquire `Hub.register`.
2. Invoke `build` **inside the barrier**; this is where fresh state is selected.
3. Encode every message before updating the view or queueing any part of the
   batch. On build/encoding error, return without committing or sending.
4. Invoke `commit` once, including for an empty batch or zero clients.
5. Snapshot recipients and enqueue the messages for each recipient in the
   supplied order; preserve existing slow-client eviction behavior.
6. Release the barrier.

The send helpers also acquire the barrier **before** invoking their builder,
encode before pushing, and handle absent/disconnected clients normally. Share
an internal already-locked implementation for `ByID`; do not lock recursively.
A build/commit callback must not call a hub operation that reacquires the barrier.

These batches order inventory messages against each other and registration;
they are not a new atomic frontend protocol. Unrelated command responses may
still interleave. No send-buffer redesign is needed.

#### D. `internal/app/server.go`: publish the complete committed view

Add `workspaceView` protected by `stateViewMu` alongside `agentView` and
`inventoryView`. Initialize all three from one State inventory snapshot. Add
`committedInventorySnapshot()` that clones the trio under one view read lock.
Existing narrower accessors may remain for unrelated callers/tests.

Add `publishCurrentInventory(ctx) error` and wire it to the poller's new callback.
Inside `BroadcastBatchPrepared`'s builder:

1. Read one fresh State snapshot and release `State.mu`.
2. Read the committed view and release `stateViewMu`.
3. Preserve `projectAgentResources` and `mergeAgentSnapshot` revision fencing.
   Construct the actual agents payload from the merged result so the committed
   cache and emitted payload agree.
4. Compare status's four semantic fields against committed status. Compare
   agents excluding `StateRevision`, as the poller does today; compare full
   workspace render data as today.
5. Build messages in order: changed `inventory_status`, required `agents`, then
   required `workspaces`. **Every transition to ready forces authoritative
   agents and workspaces frames, including identical or empty inventories.**
   The frontend ignores non-stale startup/error placeholders; status must come
   first so it accepts the authoritative recovery snapshot.
6. Return a closure that updates the trio together after encoding succeeds.
   On silent timestamp-only changes, refresh cached status timestamps without
   unsolicited frames. For suppressed revision-only agent changes, retain the
   committed agent payload/revision baseline rather than pretending an unsent
   pane revision was published.

After the barrier is released, preserve the work in today's `SetOnChange` body:
recovered push reconciliation, history synchronization/capture, and dispatcher
slot pruning on appropriate ready agent changes/recovery. This stays under the
poller callback's serialization but outside State/view/hub locks. Do not put
push persistence, pane reads, or other expensive I/O in the registration barrier.

Drain deferred refresh requests after every publication attempt, including an
unchanged success or failure. A failed encode leaves the old committed view
intact and must not advance dedupe state.

Migrate the poller's authoritative full-inventory/status publication sites to
this publisher; do not retain two competing writers. Preserve
`broadcastCommitted`'s per-pane `agent_update`/`blocked` revision fencing and
activity handling. Move semantic comparison helpers to app code as needed and
update old callback-based tests, rather than retaining unused compatibility APIs.

#### E. Connection and refresh paths

- `SetOnConnect` already runs inside `Hub.register`. Read one committed snapshot
  and use it for `push_config.inventory`, agents, workspaces and the trailing
  status. Remove its live `state.Workspaces()` read. Do not call a batch helper
  that locks the barrier again.
- Immediate `refresh_agents` uses `SendBatchPrepared`; its builder reads one
  committed snapshot, then returns `status → agents → workspaces`.
- Change `sendRequestedAgentRefreshes` to take no captured agents argument.
  Move/clear pending IDs under `refreshMu`, release it, then use
  `SendBatchPreparedByID` for each client. Snapshot selection happens inside
  each builder, never before taking the barrier.
- Keep `Wake()` for explicit refresh. A pending refresh must complete on an
  unchanged poll or failure; it must not depend on an agent rendering change.

### 2.3 Locking and compatibility invariants

- Order: `broadcastMu` → `Hub.register`; inside the barrier take/release
  `State.mu` for capture, then separately take/release `stateViewMu`. Never wait
  for the hub while holding either State/view mutex.
- Release `stateViewMu` before enqueue/eviction; disconnect callbacks can run.
  Release `refreshMu` before taking the hub barrier.
- Handshake callbacks must not acquire `broadcastMu`. Poller callbacks must not
  re-enter themselves. Audit these lock directions before implementation.
- Zero clients and zero changed messages still allow a committed-view refresh.
- No getter mutates the committed cache. A connection sees the old complete
  inventory followed by an update, or the new complete inventory—not new ready
  status with an old startup placeholder.
- After operations/notifications settle, committed semantic status converges
  to live status. A brief difference while a new operation is in flight is not
  this bug. A later genuine failure may legitimately degrade readiness again.
- Preserve per-pane generation/revision checks and stale agent retention.
  Successful empty inventory is ready, not an outage.

No frontend production change is expected for #38. Do not make an `agents`
frame implicitly clear an error; stale agent snapshots are intentionally usable
for display. Existing message kinds support the fix on old PWAs.

### 2.4 Required regression tests

Use channels/barriers and the existing fake Herdr/socket fixtures, not sleeps
that hope to hit a race. New tests may live in
`internal/coordinator/inventory_status_test.go` and
`internal/app/inventory_status_test.go`.

1. **Lost recovery, unchanged agent data.** Start ready and publish it. Arrange
   an event operation to begin before degradation, then pause it with a test
   enrichment barrier before its commit. Degrade and publish error. Release
   the event commit using the same agents/workspaces. Assert a ready publication
   occurs and the last status is ready. The previous implementation must miss
   recovery under this schedule.
2. **Publishers cannot overtake.** Pause a batch builder after it captures error
   while it owns the barrier. Restore live state and notify on another goroutine.
   Release the first builder; assert publication order error → ready. Also delay
   an old notification before it acquires the barrier: if recovery committed
   first, the delayed builder must read current ready, not replay captured error.
   Add bounded test deadlines for deadlocks.
3. **Repair is not dependent on an agent diff.** After a degraded publication,
   an identical successful inventory must publish ready despite unchanged
   topology; the new publisher must force authoritative recovery snapshots.
4. **Both success paths.** Cover recovery via `CommitPoll` and via
   `CommitTopology`; cover `topology_churn`, `command_failed`, and
   `server_not_running` failure transitions.
5. **Deduplication and real-cache repair.** Repeated equal render/semantic state
   produces no unsolicited frames; timestamp-only status updates refresh the
   committed metadata silently. Every transition to ready forces authoritative
   agents/workspaces, including empty lists. Deliberately leave the cache
   degraded while State is ready; one unchanged publication must repair it.
   Test snapshot ownership, nil callback, and failed encoding without advancing
   committed/dedupe state.
6. **Zero listeners.** Through production wiring, publish degraded then ready
   with no clients. Assert the committed inventory snapshot is ready and has
   matching agents/workspaces. Attach a client; both `push_config.inventory`
   and `inventory_status` must be ready with the matching topology.
7. **Connected client and refresh.** Receive degraded then ready on a connected
   client. After recovery, exercise refresh and reconnect; assert no stale
   degraded frame is served afterward. Compare live and committed semantic
   fields; do not demand timestamp equality after suppressed healthy polls.
8. **Registration/refresh barriers.** Gate the batch builder and race connection
   registration or immediate/deferred refresh with recovery. Assert an old
   complete snapshot then update, or the new complete snapshot, never a mixed
   tuple or an old error queued after ready. Include zero-recipient/empty-batch
   preparation, encoding errors, shutdown, and slow-client eviction. Preserve
   `TestOversizedHandshakeEvictsWithoutRegistrationDeadlock`.
9. **Unchanged refresh completion.** Queue refresh clients, run identical success
   and repeated failure attempts, and assert IDs drain/replies arrive. Include
   disconnected clients; no agent diff should be necessary.
10. **State snapshot coherence.** Commit topology with matching agent/workspace
    markers concurrently with reads. Each snapshot must match internally; edits
    to returned maps/options/worktree data must not mutate State.
11. **Frontend regression.** In `frontend/tests/unit/store.test.ts`, feed degraded
    then ready while retaining identical agent data. Assert warning/command-gate
    recovery without reconnect. Agents alone must not clear the error. Ready
    followed by `agents:[]` must clear retained idle agents after restart. Extend
    the existing browser inventory-recovery journey to stale topology-churn
    recovery and verify warning/control recovery without a new socket.

Acceptance: a stable successful inventory after a transient failure clears the
warning on connected, refreshed, and newly connected phones without restarting
the relay. Race-detector tests pass. Healthy traffic and polling cadence stay
unchanged.

## 3. Issue #37: browse Claude continuation chains

### 3.1 What must actually change

`Reader.read()` does resolve just one file, as the report says. However, the
current mobile `get_conversation_history` handler calls
`Browser.ReadPage()`, not `Reader.ReadFor()`. The Browser has its own single-file
resolution in `sourceFor()`, signed cursors, captured file ranges, and background
indexes. **Changing only `reader.go`, or only following the newest pointer,
does not finish this issue.**

The legacy Reader remains important: latest-response extraction and finished
pane capture in `internal/app/server.go` call `ReadFor()`.

Read these files:

| File | Relevant pieces |
| --- | --- |
| `internal/conversation/reader.go` | `Reader.read`, `Locate`, `safeSessionID`, `containedRegularFile`, `parseTranscript`, `stableRowID`. |
| `internal/conversation/browser.go` | `fileSource`, `sourceFor`, recent/prepare/snapshot handlers, cursor factories, job retry/expiry/close, response budgeting. |
| `internal/conversation/browser_cursor.go` | Signed, scope-bound `browseCursor`; maximum encoded length is 2048 bytes. |
| `internal/conversation/jsonl_records.go` | Bounded record reading, exact offsets, complete versus trailing partial records. |
| `internal/conversation/projection.go`, `browser_index.go` | Per-file offset IDs, tool-result association, disk indexing. |
| `internal/conversation/source_open_*.go`, `roots_test.go` | Safe regular-file opening and profile/project containment rules. |
| `frontend/src/components/ConversationHistory.svelte` | `historyPageMismatch`, paging, snapshot identity, diagnostics, both display modes. |
| `frontend/src/lib/store.ts`, `types.ts` | Strict page normalization and TypeScript wire types. |

### 3.2 Required behavior and deliberate limits

For files A → B → C, while Herdr still reports A:

- A latest read shows the latest visible C messages, not the end of A.
- Paging older reaches C's beginning, then B, then A. Entries within each page
  remain oldest-to-newest. Chain order is authoritative; do not sort all records
  by timestamp.
- Full history traverses large parent files through the existing background
  index. The 16 MiB recent-read window is not the end of full history.
- An empty new continuation does not hide the last visible parent messages.
- A broken continuation preserves readable history and displays a warning;
  it must not silently claim the complete conversation was read.
- Newly appended messages and newly created continuation files become visible
  on an ordinary latest refresh, without waiting for the 60-second location
  cache TTL or restarting the relay.
- In-progress older browsing uses a fixed chain/range. Appending C → D must not
  retarget an existing cursor to D or invalidate A/B entry IDs.

Scope this change to normalized `claude` and `claudecode`. Qoder shares Claude
message parsing but there is no evidence here that its continuation semantics
are identical. Leave other providers unchanged.

Follow only explicit forward `continued-in` links. Do not discover ancestors
of a directly requested B by scanning every transcript, infer links from file
mtime, or rewrite the session ID reported by Herdr. A request starting at B can
reach B and its descendants; recovering an unreferenced A is outside this fix.

Keep tool association scoped to each segment. Do not match reused tool-call IDs
across unrelated files. Do not deduplicate messages by text: repeated prompts
are legitimate. A sanitized real continuation sample should be checked for
copied native-message UUIDs before implementation. If Claude copies ancestor
rows into the child, add an identity-based replay rule and fixture before
shipping; never guess that identical text means a duplicate.

### 3.3 Step A — add one shared, bounded chain resolver

Create `internal/conversation/claude_chain.go` and corresponding tests. Keep
`Reader.Locate()` unchanged: session-title lookup deliberately shares its exact
root choice with conversation lookup. Chain following belongs after location
selection, not inside that cache.

Suggested private model (names may vary, responsibilities may not):

```text
claudeSegment:
    sessionID, Location, capturedEnd, fileRevision
    footerStart, footerEnd, footerDigest
claudeChain:
    ordered segments [anchor ... newest readable descendant]
    optional bounded incomplete-reason code
```

The resolver accepts context, anchor `Location`, and anchor session ID. Use it
from both Reader and Browser. It returns descriptors, not concatenated text.

Algorithm:

1. Open the anchor with the checks used by `captureFileSource`: contained real
   path, regular file, safely opened handle, and identity recheck. Close handles
   after capturing/scanning unless explicitly transferring ownership.
2. Inspect at most the last **64 KiB per segment** using the complete-record
   semantics of `NewJSONLRecordReader`. The issue describes a final, small
   pointer record; do not scan megabytes on every latest request just to find
   that footer. Retain the observed footer range/digest for cursor validation.
3. Consider only parsed objects whose `type` is exactly `continued-in` and
   `isSidechain` is not true. Require a nonempty, `safeSessionID`-valid
   `continuedInSessionId`. When `sessionId` is present, require it to match the
   segment being inspected. Do not traverse malformed or mismatched pointers.
4. A valid final JSON object without a newline is complete. A partially written
   final record is not a corrupt permanent link: do not follow it until complete;
   another latest request must retry. If multiple conflicting completed links
   exist in the footer, stop with an ambiguity warning rather than choosing an
   arbitrary branch. An uninspectable/oversized footer must be reported as a
   bounded-resolution limitation, not certified as the end of the chain.
5. Resolve the child as `<selected project directory>/<child ID>.jsonl` under
   the **same initially selected profile root and real project directory**.
   Never call the global multi-root `Locate` afresh for each child. Duplicate
   IDs in another profile/project must not redirect the conversation.
6. Apply containment and regular-file checks to every child. Reject path
   traversal, symlink escapes, FIFOs/devices, and a file retargeted during open.
   A safe symlinked project inside the configured root must still work.
7. Track both session IDs and resolved file identities/paths. Stop on a cycle,
   including different IDs aliasing the same file. Limit a chain to **64
   segments**: discovery reads at most 4 MiB of footers, plus bounded identity
   anchors. Check cancellation between opens/records.
8. If a referenced child is missing/unreadable/unsafe or the hop limit is hit,
   return the readable prefix with an incomplete reason. Do not fail the whole
   conversation when its parent can be read. Never put filesystem paths or raw
   transcript contents into public diagnostics.
9. Do not negative-cache continuation results across latest requests. The anchor
   location cache may remain; missing B must be found on the next latest read
   after B appears.

This same-project policy is intentional. If a real fixture demonstrates that
Claude legitimately relocates continuations, stop and specify a contained,
unambiguous lookup extension; do not silently fall back to all configured roots.

### 3.4 Step B — update the legacy Reader without making it unbounded

Add a Claude-specific branch after validating the provider/session and locating
the anchor. Leave other providers' parsing and cursor behavior unchanged.

- Resolve the chain, then read newest segments first with a **shared 16 MiB
  transcript budget**, not 16 MiB times 64 files. Parse each file independently,
  reverse the collected segment groups into chronological order, and apply the
  existing page limit/default/max logic to the combined bounded entries.
- Preserve existing anchor-file row IDs. Namespace continuation-file legacy IDs
  with a bounded hash of that segment's identity so identical raw lines in two
  files cannot collide. IDs must not depend on the chain's current length or on
  later file growth. Do not expose paths in IDs.
- Preserve `before` paging inside the loaded bounded window, including across
  a small parent/child boundary. If a supplied `before` ID cannot be found, do
  not silently jump back to the newest page for the new Claude chain path;
  return an explicit reload/source-changed result.
- Keep `Total` as the count of entries actually loaded by this bounded Reader.
  Set `FileTruncated` if any relevant file prefix or older segment was omitted.
  Do not make `HasMore` promise arbitrary full-history access through this
  legacy API. Full unbounded-history traversal belongs to Browser snapshots.
- Surface an incomplete-chain reason separately from ordinary tail clipping.
  Keep readable entries `Available=true`. The Browser diagnostic described below
  can share the same internal reason enum.

Add a server regression for latest-response/finished-pane extraction with stale
session A and a later answer in B. No session-title or agent identity mutation
is needed to make these callers see B.

### 3.5 Step C — make the Browser chain-aware, reusing per-file indexes

Use a **Claude chain adapter around the existing single-file browsing engine**.
Do not build a huge concatenated temporary JSONL file, multiply the recent byte
budget by chain length, or rewrite bbolt offsets into one virtual address space.
Each per-file snapshot keeps its current revision, offsets, digest checks, tool
association, quotas, and worker lifecycle.

#### Chain context and cursor representation

Add a private immutable chain manifest retained by `Browser` for cursor-backed
browsing. It contains:

- a random chain-context ID and the hash of the original `BrowseScope`;
- the resolver's ordered segment descriptors and fixed captured ends;
- incomplete-chain diagnostics;
- a logical snapshot ID for the whole browsing context;
- expiry/last-access bookkeeping, protected by `Browser.mu`.

Use `CursorTTL` for retention, cap the manifest registry at **256 contexts**, and
expire/evict idle contexts explicitly. No file handles are owned by a manifest;
individual reads/jobs own them. Protect active reads from eviction with retained
references, following the existing job acquire/release pattern. If capacity is
occupied by active contexts, return a bounded retryable capacity error rather
than evicting an active reader or growing without limit. Reuse an identical
manifest for the same scope and captured descriptors when possible.

Extend the signed `browseCursor` with a chain ID and segment index, plus a
`segment` mode meaning "start this older file at its captured end." Keep its
existing fields for recent/prepare/snapshot continuation. Use an optional index
or another representation that distinguishes missing index from valid index 0;
reject a chain ID without its index and an index without a chain ID. **Do not
put a nested encoded cursor, file paths, or the entire 64-file manifest into
the token.**
Validate the ID/index, original scope, expiry, and 2048-byte limit before using
any manifest. Chain expiry returns `cursor_expired`; wrong scope/index/tampering
returns `invalid_cursor`.

#### Integration points

1. On a Claude latest request, resolve current links and capture a new/reusable
   context. Read the newest segment's recent page. For an ordinary one-file
   history, preserve the existing path and public behavior.
2. On a chain cursor request, load the **saved** context. Select its segment,
   safely reopen its saved `Location`, compare file revision/footer evidence,
   reject shrink/replacement/changed captured ranges, and cap reading at its
   saved end. Do not rediscover the latest leaf for that cursor.
3. Factor the recent reader so it can take an already selected `fileSource`.
   Thread a private source binding through recent/prepare reads and jobs; all
   source reacquisition, especially `retryJob`, must reopen the saved segment
   rather than `Locate(original Scope.SessionID)`. Explicitly set the source's
   effective end to the manifest's captured end before job creation **and on
   retry**. Today those paths use the newly opened file's larger end; leaving
   that behavior unchanged would let later messages/tool results leak into a
   supposedly frozen chain snapshot.
4. Keep the phone-visible `BrowseScope` unchanged. The internal selected source
   is separate from the agent/session/terminal tuple that authenticates cursors.
   Keep the server's `sameConversationTuple` post-read check unchanged.
5. Carry the chain binding into `recentCursor`, `prepareCursor`, `snapshotCursor`,
   and the job's preparation cursor. Store it on the job/source as needed so
   retries and preparation polling cannot lose the selected segment. Include
   chain context ID and segment index in job dedup keys and `jobMatches`: two
   different contexts must not reuse a job whose cached cursor/binding belongs
   to the first context. Reuse only within the same context. Existing aggregate
   disk/queue limits still apply; do not introduce unbounded duplicate jobs.
6. In `memoryPage` and `readSnapshotPageAt`, use same-file older cursors while
   entries remain there. When the file is exhausted and a parent exists, emit
   a `segment` cursor for the previous manifest entry. Include this cursor in
   page-size fitting **before** calculating the response budget. Never append
   an unbudgeted wrapper after a page has already been fitted.
7. A page may contain fewer than `limit` rows at a segment boundary. That is
   valid. If a fully inspected segment has no visible rows, advance through
   empty segments within the same bounded request where possible. If its recent
   window is empty but an unscanned prefix exists, offer the existing prepare
   cursor; do not synchronously index the prefix just to fill the page.
8. `HasMore=true` always requires a nonempty usable cursor. The last row in A
   terminates traversal. A missing descendant is a diagnostic, not a fabricated
   endless older cursor. A source that disappears during saved-cursor traversal
   must produce an explicit source-change/unavailable result, not skip silently.
9. For multi-file browsing, return `Total=null` unless a correct whole-context
   count is actually known. Do not label C's per-file total as the chain total.
10. Keep existing per-file snapshot cleanup/quotas/leases. Extend Browser expiry
    and `Close()` to discard contexts, including failure and cancellation paths.
    Releasing a context must not close a file handle still owned by a job.

#### Public identity is not per-file identity

`ConversationHistory.svelte:historyPageMismatch` currently rejects a changed
`source_revision` or snapshot ID. Simply returning B's revision followed by A's
revision would cause a reload at every boundary.

- Keep internal entry IDs and index validation keyed by the selected file's
  existing `fileRevision`/`browserEntryID`. Never re-key old rows when C is added.
- Initially expose the anchor source revision as logical `source_revision`.
  Preserve it when single-file A becomes A → B or later descendants are appended.
  Keep per-scope lineage identity beside the latest manifest and compare its
  previously known segment identities/link prefix on fresh discovery too. A
  replacement/retargeted existing segment must trigger source-changed/reload and
  a new lineage identity, not silently merge a different B under A's old public
  revision. Bound this bookkeeping with the context registry. A routine new
  context after append is not a new lineage. Do not use its random context ID
  as `source_revision`.
- Once older chain browsing is active, ready pages use `mode=snapshot` and the
  context's one logical snapshot ID, even if a particular small file was read
  from its recent window. This represents a frozen browsing context, not a
  promise that every segment has a bbolt file. Preparing/failed responses keep
  the existing state/progress/retry contract.
- All later ready older pages in that context use the same logical snapshot ID.
  Per-file bbolt snapshot IDs remain private to job/cursor validation.
- Latest requests remain `mode=recent`; returning to latest discards the old
  context on the phone and discovers appended messages/links. Old cursors remain
  usable until expiry and retain their old captured bounds.

Add comments explaining these two layers of identity. Do not solve boundary
mismatches by removing `historyPageMismatch` safeguards globally.

### 3.6 Step D — fix repeated preparation and show diagnostics

#### Continue preparing older segments after a snapshot

This is a required frontend behavior change, not just a backend feature:
`ConversationHistory.svelte` currently blocks preparation timers, `loadLatest`,
Continue, and preparation controls whenever `snapshotActive` is true. After
B's snapshot, preparation of a large A would otherwise strand the history UI.

1. Separate **cursor-status polling** from **cursorless latest refresh**. It is
   acceptable to extract a dedicated preparation-poll helper to make the
   distinction explicit. While a snapshot is active, forbid cursorless refresh
   but allow polling/retrying the current prepare cursor.
2. Audit every `snapshotActive` guard: the visibility/timer paths, preparation
   effect, `loadLatest`, `continuePreparation`, and preparation controls. Do not
   globally remove snapshot protection; relax it only for status-cursor work.
3. Keep cancellation, visibility/security-lock checks, bounded poll counts,
   Stop/Continue/Retry controls, entries and scroll anchors working during the
   second and subsequent preparations. Never merge a fresh latest page into a
   frozen browsing context.
4. On successful ready completion, use exactly `page.nextCursor` and
   `page.hasMore`. Today `loadLatest` retains `statusCursor` when a terminal
   snapshot returns no cursor; remove that success-path fallback or final EOF
   will appear to have more history forever. Retain a cursor only while
   preparing or when failure is genuinely retryable; terminal source-change/
   expiry follows the existing reload path.
5. Use the logical snapshot ID from the backend throughout. Do not clear loaded
   B entries or change the public identity while waiting for A's index.

#### Incomplete-chain warning

Add optional fields such as `continuation_incomplete: boolean` and
`continuation_reason: string` to `BrowseDiagnostics`, with a small fixed enum of
reason codes shared with the resolver: missing/unreadable source, invalid link,
ambiguous link, cycle, and resolution limit.

Update these locations together:

1. `internal/conversation/browser.go`: Go wire diagnostics and partial pages.
2. `frontend/src/lib/types.ts`: optional diagnostics fields.
3. `frontend/src/lib/store.ts:normalizeConversationPage`: validate the boolean
   and bounded reason code; absent fields default to the old behavior.
4. `frontend/src/components/ConversationHistory.svelte`: merge the new fields,
   show a static user-facing warning with `role="status"`, and offer the existing
   reload/latest action. For a known missing child: "This conversation continues
   in another session, but part of that history is unavailable. Reload to try
   again." For a scan limit that cannot establish whether a child exists, say
   that history could not be fully checked rather than asserting a continuation.
5. Update diagnostics on a new latest result instead of OR-ing a stale warning
   forever. A child file appearing and a successful reload must clear the
   warning. Preserve a fixed context's warning during older traversal.

Do not misuse `source_truncated` for a broken link: that field already means
older bytes can be loaded. Both Conversation and Full history share this
component, so one accessible warning covers both modes. Optional fields preserve
compatibility with an older relay or frontend. No protocol-version bump should
be necessary.

### 3.7 Required tests and exact expected results

Create reusable sanitized fixtures in temporary configured Claude roots. Use
small files except when testing byte-window behavior. Do not read a developer's
real Claude history.

| Test | Setup and assertion |
| --- | --- |
| Latest from stale ID | A has `a1,a2` then a valid A → B marker; B has `b1,b2`. Request A with limit 1. Latest entry is `b2` in both Reader and Browser. |
| Backward boundary | Same fixture, Browser limit 1. Following cursors yields pages `b2`, `b1`, `a2`, `a1`, then stops. Every row ID occurs once; each page's rows are chronological. |
| Multiple hops | A → B → C, request A. Traverse newest C through oldest A. Conflicting timestamps must not reorder segments. |
| Empty/metadata-only child | B has no visible rows. Latest can still show A's last visible message; traversal neither loops nor hides the parent. |
| Large parent and child | Set Browser `RecentBytes` small enough to force preparation in both A and B. Traverse both snapshots; no gap/overlap at recent → snapshot or B → A boundaries. |
| IDs and refresh | Append B records, then B → C. Existing A/B row IDs stay stable, latest sees C, old cursors stay on saved bounds. |
| Single-file becomes chain | Obtain latest A page first, then append A → B and create B. Next latest response keeps compatible public revision and shows B; no forced reload error. |
| Snapshot identity | Traverse B's prepared snapshot into A's recent and prepared pages. Public revision/logical snapshot ID stay the same; internal file identities stay distinct. |
| Missing child recovers | Read A → missing B: readable A plus warning. Create B using the same Reader/Browser. Next latest/reload reads B without waiting for location TTL; warning clears. |
| Invalid links | Empty ID, wrong type, traversal strings, mismatched session ID, sidechain marker, conflicting links, A → A, A → B → A, aliased-file cycle. Assert bounded work, readable prefix, appropriate diagnostics, and no unrelated file access. |
| Root selection | Duplicate child IDs in different projects/profiles; symlinked contained project; child symlink escape; FIFO/nonregular child. Follow only the chosen contained project and never block opening a FIFO. |
| Trailing record | Partial marker, completed marker without newline, CRLF records, oversized footer. Retry sees completion; incomplete data does not become a permanent bogus link. |
| Quotas and lifecycle | Hop limit, total discovery budget, recent budget, cursor length, response bytes, registry capacity/expiry, cancellation, failed job retry, concurrent read/eviction/Close. No leaked handles/jobs or unbounded scans. |
| Cursor security | Tampered chain/index/mode, wrong pane/generation/terminal/provider/scope, expired context, removed/replaced/truncated/rewritten segment. Reject explicitly; never switch to latest behind the cursor. |
| Legacy bounds | Parent/child IDs are unique, `before` crosses a loaded boundary, unknown `before` errors explicitly, clipping is reported, latest-response extraction sees the child. |
| Other providers | Existing Qoder, Codex, Pi, OMP, OMO, OpenCode, Hermes, root-selection, tool-association, and cursor tests remain unchanged and pass. |

Frontend tests:

- `frontend/tests/unit/store.test.ts`: normalize optional fields; reject malformed
  fields/cursors; retain compatibility when fields are absent.
- `frontend/tests/unit/components.test.ts`: warning shown with readable entries,
  warning cleared after recovery, history not reset at a normal segment boundary;
  second preparation polls while snapshot-active, Stop/Continue still work,
  cursorless latest does not run, and successful EOF clears the old prepare
  cursor/Load older control.
- `frontend/tests/browser/mobile-journeys.spec.ts`: stale session A, newer B
  messages in Conversation and Full history, load older into A, preserve scroll
  anchor, prepare both files, show/recover from a missing continuation, and return
  to latest after C is appended. Extend existing fixtures; do not require Claude
  or production Herdr in browser CI.
- `internal/app` test: exercise the actual `get_conversation_history` command
  using a stale anchor, not only the lower-level Reader. Retain generation/tuple
  rejection when the pane really changes during the request.

Acceptance: the mobile history for anchor A includes every visible row from the
readable A → B → C chain, subject only to existing explicitly reported record
limits. Large older files remain pageable. Broken links are visible, current
latest reads recover when files appear, and security/resource limits hold.

## 4. Suggested commit-sized implementation checklist

- [ ] **38.1** Add deterministic lost-recovery and callback-order tests; confirm
  they expose the old behavior.
- [ ] **38.2** Add atomic State snapshot copying and transport prepared-batch
  helpers with ownership/ordering/error tests.
- [ ] **38.3** Wire the single server publisher and signal-only poller callback;
  remove operation-local comparisons and move deduplication. Fence handshake
  and both refresh paths; preserve post-publication side effects and #36 work.
- [ ] **38.4** Add server/transport/frontend recovery regression coverage;
  verify unchanged-refresh completion and no healthy push spam.
- [ ] **37.1** Add shared safe chain discovery, limits, diagnostics, and fixtures.
- [ ] **37.2** Update the legacy Reader and latest-response caller tests.
- [ ] **37.3** Add bounded Browser contexts and chain cursor fields; integrate
  source binding through recent, prepare, snapshot, retry, and cleanup paths.
- [ ] **37.4** Add parent traversal and logical public identity; prove complete
  large-file traversal and all resource/security invariants.
- [ ] **37.5** Fix frontend preparation during snapshot browsing and successful
  EOF handling; add diagnostics and end-to-end paging/recovery tests. Document
  continuation behavior and limitations in `docs/mobile-app.md`.
- [ ] **Final** Run verification below; regenerate the shipped frontend for #37;
  inspect the diff; stage only files belonging to these issues.

Do not mark #37 complete after 37.2: that fixes helper readers, not the mobile
Browser. Do not ship only the newest-file redirect and leave parent history
unreachable.

## 5. Verification instructions

All commands are run from the repository root. Required toolchains are Go
1.27.0 and Bun 1.4, as documented in `docs/development.md` and project manifests.
Use disposable fake upstreams; never stop the user's live Herdr to simulate an
outage. The Makefile loads `.env`; do not print it or run deployment targets.

During implementation:

```sh
# Backend lint/static analysis and focused tests.
go vet ./internal/conversation ./internal/coordinator ./internal/app ./internal/transport
go test ./internal/conversation ./internal/coordinator ./internal/app ./internal/transport
go test -race -p 1 ./internal/conversation ./internal/coordinator ./internal/app ./internal/transport

# Frontend lint, types, unit tests, and build/size validation.
make frontend-check

# Iterate on browser tests against the built dist bundle.
make frontend-browser

# Backend build without writing binaries into the checkout.
go build ./cmd/...
```

Run `gofmt` on changed Go files and check `git diff --check`. Before committing
or opening a PR, run the repository's full verification:

```sh
# Required when frontend production source changes for #37:
# generates verified web/ assets; never edit web/ files by hand.
make web-release

# Includes backend formatting/vet/tests/race/shell audit, frontend lint/types/
# tests/build, shipped-bundle comparison/browser tests, cross-builds and bundles.
make check
```

`make backend-check` and `make cross-build` are useful separate gates while
working on #38. See `docs/development.md` for Chromium/WebKit prerequisites and
the Fedora container path. Report unavailable prerequisites and unrelated
baseline failures explicitly; do not describe a partial check as full success.

### Planning-time verification already performed

During planning, against the local checkout without implementing either fix
(and with independent issue-#36 work appearing concurrently):

- `go test ./internal/conversation ./internal/coordinator ./internal/app ./internal/transport` — passed.
- `go test -race -p 1 ./internal/conversation ./internal/coordinator ./internal/app ./internal/transport` — passed.

These are baseline checks, not proof that either issue is fixed. Full lint,
frontend, build, and release gates must be run for the implementation. This
planning change alone does not require regenerating `web/`.

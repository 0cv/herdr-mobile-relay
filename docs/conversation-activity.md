# Conversation activity

Conversation keeps prompts, agent questions (Claude Code AskUserQuestion) with
their answers, and final answers. **Full history**
shows intermediate assistant messages and tool cards. Both use browser-local
scrolling; neither requests a pane size. Terminal
remains available for menus, approvals and other native interactions. Composer
and attachment permissions are unchanged.

## Sources and prerequisites

| Source boundary | Reading source | Active branch | In-progress text | Tool progress |
| --- | --- | --- | --- | --- |
| Stock Pi 1.3.0 with the updated optional integration | Public session-manager traversal and normalized extension events | Runtime leaf/parent traversal | Provisional text | Normalized start/progress/end |
| Pi without the integration | Existing session JSONL reader | Unverified | Unavailable | Recorded results only |
| Codex response_item/message JSONL | Existing recorded-history reader | Unverified | No attach-to-running-TUI interface qualified | Recorded results only |
| Claude user/assistant JSONL and continued-session records | Existing recorded-history reader, excluding sidechains | Unverified | No attach-to-running-TUI interface qualified | Recorded results only |
| Cursor Agent JSONL (`cursor`, `cursoragent`) | Existing recorded-history reader | Unverified | No provisional text | Recorded results only |
| Qoder JSONL (`qoder`, `qodercli`) | Existing recorded-history reader | Unverified | No provisional text | Recorded results only |
| Oh My Pi JSONL (`omp`, `ohmypi`) | Existing recorded-history reader | Unverified | No provisional text | Recorded results only |
| OpenCode 1.x `session/message/part`, 2.x `session_v2/session_message`, and upgraded mixed SQLite stores | Per-session native database reader; bounded recorded updates | Unverified | Persisted edits only | Recorded state changes |
| Hermes (`hermes`, `hermesagent`) | Existing native database reader | Unverified | Persisted edits only | Recorded calls/results |
| Antigravity CLI (`agy`, `antigravity`) SQLite `steps` per conversation | Native database reader; user messages, replies and tool calls | Unverified | No provisional text | Tool calls marked completed when their result step exists; result bodies are not read |
| Oh My OpenCode (`omo`, `ohmyopencode`) | Existing session JSONL and validated `senpi.todo-state` reader | Unverified | No provisional text | Recorded results and plan snapshots |
| Kimi Code native 1.5 / legacy-engine 1.4 journals (0.36.x), plus validated 2.2.0 undo branches | Explicit main-agent event fold | Validated journal branch | Completed recorded responses, not tokens | Bounded recorded calls/results |

The recorded feed checks for changes every 250 ms and delivers changed
records through the encrypted transport. Provider names accept case, spaces,
hyphens and underscores, including the aliases above. OpenCode uses the existing
reader's per-session choice of 1.x `session/message/part` or 2.x
`session_v2/session_message` tables, including mixed stores. Loose JSON-file
OpenCode stores are not supported by this reader. Hermes retains its workspace
check. Oh My OpenCode retains its JSONL identity and plan validation; plan changes
request a fresh snapshot rather than sending a new plan protocol.

## Native history opening cost

OpenCode 2.x selects recent visible turns with one lookahead turn and no eager
session-wide count. Browse responses use `has_more` and an opaque cursor;
`total` is null, not an estimate. Indexed session/sequence access avoids work
proportional to all visible history just to compute a total. Filtering hidden
turns can still visit intervening rows; the provider's indexes and row sizes
also affect query cost.

Hermes and unindexed OpenCode 1.x retain their full-session validation, logical
ordering and duplicate handling before selecting a recent page. Like Kimi's
cold journal validation, these cold opens may grow with the entire session;
limited response size does not make that database work bounded. Hermes may
repeat this work on later opens, while unchanged OpenCode queries can use the
reader's cache. No provider schema or trust checks are changed to avoid it.
Synthetic measurement tests record this growth without imposing a false
bounded-work budget on those layouts. These are logical cold-query measurements,
not flushed operating-system caches or real-phone latency measurements.

Installed source boundaries inspected during development were Pi 1.3.0, Codex
0.160.0 package metadata, and a Claude executable symlink targeting 2.1.287.
The Codex and Claude versions are not live-stream or flush-cadence qualifications.
Transcript availability does not imply partial-response streaming. No RPC/SDK
attachment to those running TUIs is assumed. No agent fork or private renderer
API is used.

The source notice distinguishes live Pi events, recorded-history updates,
synchronizing, paused, disconnected and incomplete output. Recorded sources do
not claim that their linear log is the runtime's active branch, except for Kimi's validated journal branch selection. Partial lines,
omissions and reader diagnostics retain their existing meaning.

## Kimi Code recorded history

This reader targets the TypeScript MoonshotAI/kimi-code journal language derived
from pinned 0.36.1 and 2.2.0 source research, not the archived Python kimi-cli.
Native protocol 1.5 and optional legacy-engine 1.4 share the supported
`context.append_message` / step/content/tool event language. Protocol 1.0 and
`ses_` imports are unavailable because import can lose origin/privacy metadata.
Human-engine-only journals are unavailable; duplicate human-engine records beside
supported context records are suppressed. Unknown record types, malformed records,
invalid branch bases and incomplete switch/undo/undone triples fail closed with
an explicit reason. This is synthetic source-derived qualification, not an
installed Kimi runtime or all-engine/version parity claim.

Only human-origin user text, assistant text and bounded tool name/input/result
text are exported. Think/encrypted content, media, attachments, origins, extras,
usage, headers, hook expansions, system injections and compaction summaries are
not exported. Allowed text and tool input/output can still contain pasted secrets;
schema filtering is not semantic secret redaction. Subagents, blobs, configuration,
credentials and user input recall are never read for history.

A current Herdr-reported ID (kind `id`, or no kind) is preferred and never silently
replaced when missing. Otherwise the sole live Kimi pane at the foreground cwd
selects the non-archived session with highest persisted `updatedAt`, then ID.
An inferred selection retains its selected root and journal path separately from
a reported ID; inferred browser, activity backfill and completion reads use that
validated location, not an ID lookup. Activity and completion validate the captured
pane/terminal/server/generation and selection before and after reading; failed or
obsolete reads do not fall back to an archived duplicate ID. Historical Kimi
activity without a current pane keeps its captured extract rather than re-resolving
an ID of unknown origin. Each feed
freshness check (including the 250 ms tick and update delivery) revalidates it;
snapshot and pagination reads check before and after reading too. A changed
inferred ID or source location, archive, deletion or index tombstone closes the old subscription,
rejects its pages/cursors and wakes inventory reconciliation immediately, rather
than waiting for its normal interval. The phone then resets to the newly inferred
session or unavailable history. History-only route following requires the same
relay-issued inferred-owner token (relay start, pane, terminal, tab, workspace,
lexical cwd and verified process incarnation), the same relay/server/pane/terminal and the next generation.
Stock Herdr `pane.process_info` supplies PID/CWD/argv, not process start time.
On macOS the relay reads the local kernel's microsecond process start time through
`kern.proc.pid`, fenced by two fresh Herdr observations and two matching kernel
observations. Failure to obtain matching evidence leaves the inferred owner empty.
Other platforms currently leave it empty: Linux `/proc` start-time ticks are not
used as a high-resolution incarnation claim. PID/CWD/argv alone remains a weaker
read fence, never verified owner continuity. There is no new remote API field or
Kimi agent patch. Kernel lookup coverage is tested with owned synthetic processes;
Kimi itself is absent, so this is not native Kimi runtime qualification.
Reported sessions, Terminal routes, stale bookmarks, skipped generations and
process/pane replacements and ownerless Kimi snapshots are not followed. No command
is replayed. Initial selection and every freshness check consider all Kimi peers
in coordinator topology, regardless of cached foreground or workspace cwd, and
fail closed when a possible conflict cannot be ruled out. Empty identity, failed
lookup or replacement evidence is not proof of departure. A fresh foreground cwd
elsewhere or removal from topology rules out that peer. A peer freshly changed
into the target cwd invalidates old inferred pages and subscriptions even if its
cached cwd differed. Stable unavailable selections do not repeatedly reset. Reported-ID selection
policy is unchanged, including no recency fallback or archive exclusion.

Idle inference stats a cached, bounded set of root/index/session-directory/state
paths and journal identities; it does not enumerate directories or read metadata
contents again unless a dependency changes. Discovery retains the 2,048-entry /
4 MiB metadata bounds, with at most 64 configured roots, 8 cached selections and
8,192 watched paths per selection. Journal growth alone does not repeat discovery.
Initial enrichment uses at most 32 process checks under one two-second deadline;
unverified remaining peers prevent inference. Freshness checks the selected
process and at most 32 topology peers under one shared two-second deadline;
exceeding the peer cap fails closed as ambiguous. Each completed process check
uses two fresh socket requests, not cached process inventory.

Argv is a process fence, not proof of the currently selected session. Recency is
only a persisted-selection heuristic: an unpersisted TUI change cannot be detected
until its directory, index or state evidence changes. Sessionless startup is
normal. Native workspace hashing preserves lexical cwd spelling; bounded
directory/metadata/index evidence admits registered workspace buckets.
Contradictory cwd/ID/index evidence is rejected. Configured roots are ordered;
the first matching root supplies both title and transcript. Foreground PID/argv,
pane generation, cwd and session changes fence subscriptions and cursors.

The 250 ms feed observes disk records. Kimi persists complete model responses,
not token deltas. Appends deliver changed entries; undo, branch switches, clear,
compaction, truncation and rewrites reset loaded history. Full history retains
pre-clear and pre-compaction display entries while establishing undo floors.
Only the selected branch is folded; paired branch undo is not applied twice.
Partial trailing records wait for a newline and report incompleteness.

Replay requires the entire main journal: at most 16 MiB, 100,000 parsed candidates,
1 MiB per record, JSON depth 32, 128 branches/open steps/pending tools and 128 tools
per entry. Discovery admits at most 2,048 directory entries and 4 MiB of metadata
per lookup (256 KiB per metadata/index file). Histories exceeding these bounds
are unavailable rather than returning an unverifiable tail. Recent root-level
`state.json` is required; older `session-meta`-only layouts are not qualified.
Eight in-memory source digest states fence rewrites without retaining raw journals.
The existing 200-entry, 2 MiB response and opaque 15-minute cursor bounds apply.
No frontend-specific Kimi path is needed: advertised history opens Conversation /
Full history by default, with the existing initial-unavailable Terminal fallback
and saved preferences unchanged.

## Pi privacy policy

With the integration loaded, provisional assistant text and normalized tool
start/progress/end activity always stream to authorized paired devices, including
read-only readers, while **Full history** is open. This mirrors the streaming text
and tool activity the laptop TUI already displays; Terminal mirroring also sends
on-screen streaming text to those paired devices. Exact custom terminal rendering
is unsupported.

Pi dispatches `message_update` before completion transformations, and
`message_end` handlers can still replace content before persistence. Completion-time
redactions by other extensions cannot retract text or tool arguments/progress
already delivered to a phone, just as with Terminal mirroring. Finalized history
reflects persisted transformations, not a guarantee against earlier disclosure.

The integration does not export raw provider events, hidden reasoning, signatures,
images, system instructions, custom-message execution, arbitrary `details`, or
invented persisted nested-tool messages. Nested executions carry an ephemeral
parent-call association and disappear at the persisted turn boundary.

Completed runtime history uses Pi entry IDs. Provisional IDs are scoped to the
integration incarnation and explicitly removed at the persisted `turn_end`
boundary; identical text is never used for matching. Page cursors retain a
captured ancestor chain. Branch navigation invalidates those cursors; ordinary
appends do not. Compaction preserves earlier raw branch history. A late load
cannot reconstruct an already-started partial response and stays incomplete
during tool progress until a validated persisted boundary or a fresh assistant
start permits recovery. Non-text user turns retain an explicit omission marker;
image payloads are never exported. Aborted/provider-error responses
remove provisional content and report incomplete output without exporting raw
provider error details. A subsequent successful turn restores ready state.

## Transport and bounds

The integration adds a sibling private Unix socket, not a public port. Existing
catalog deadlines remain unchanged. The relay checks socket
ownership/permissions, peer PID, inode, challenge echo, source incarnation and
current exact target/process before accepting data. Challenge echo alone is not
authentication. The existing authenticated encrypted phone transport is reused.
At startup, a refused UID-owned socket can be reclaimed only after inode and
private-directory checks. Live listeners, symlinks and unrelated files are left
alone. Normal Node shutdown removes both socket paths.

- Pi owns the approximately 250 ms partial-update batching cadence. Capture
  handlers never await browser/network I/O. The socket independently admits up to
  eight subscriptions and eight page requests, plus at most 16 pending request
  handshakes with an absolute one-second deadline that incoming bytes cannot
  extend. Admission or close cancels the handshake timer. A full subscription
  pool cannot consume page capacity. There are at most 32 accepted sockets, a 1 MiB frame limit and a
  bounded writable queue; slow clients disconnect.
- Provisional tools occupy one bounded turn-level card group, at most 64 calls
  and 504 KiB of tool data; updates coalesce at the same source cadence. Only
  permitted text/arguments and explicit execution state are exported. Truncation
  warnings indicate actual byte/structural omissions, not ordinary normalization.
  Image-only and mixed non-text tool results retain safe omission notices in
  progress and finalized cards without exporting image bytes or metadata.
- Runtime pages contain at most 200 visible entries and traverse at most 2,048
  raw ancestors per request. Text is capped at 64 KiB, tool input/output at
  16 KiB each, and tool count at 64. Message projection inspects at most 128
  content blocks and reports structural omissions, including in partial updates.
  Tool arguments also have structural bounds.
  At most 64 opaque pagination cursors survive for 15 minutes per incarnation.
- The relay coalesces viewers into at most eight sources, eight viewers per
  source. Its Pi bridge independently bounds streams and page requests to eight
  each, so fully occupied streams do not consume history-pagination capacity. Each source retains at most 128 batches / 2 MiB for replay. Recorded
  JSONL sources retain one bounded recent page in addition and check for changes
  every 250 ms. Claude checks all resolved continuation segments using the
  existing same-project, 64-segment / 4 MiB footer-discovery bounds; a child-only
  append can wake the feed without an anchor-file change. Only changed records
  are delivered. Database idle checks stat contained main/WAL files, including
  identity, size, modification and change time, without SQL or a persistent read
  transaction. Changed databases use rowid-watermarked append queries (512 rows
  plus an overflow sentinel) and recheck IDs returned to viewers, up to a shared
  20,000-ID budget. OpenCode uses existing primary-key indexes for those checks
  and the existing message-to-part index for 1.x parts; query plans are checked
  before executing lookups. Part reads share a 32,768-row budget, with at most
  256 parts per message. OpenCode 2.x parts live inside each message's JSON.
  Hermes has no message-update index: an EXPLAIN-verified session/rowid range
  of at most 32,768 target-session rows plus a sentinel bounds edit checks,
  logical-ID deduplication and tool-result association, so unrelated sessions do
  not consume the range. Without a usable session/rowid index, it uses a bounded
  global rowid range and resets on overflow rather than scanning the store. Live ticks never count the store or run full-store joins/dedup.
  Missing indexes, exceeded budgets, non-increasing Hermes append timestamps
  (including copied/compacted generations), or incomplete tool associations
  request a reset/snapshot rather than an unbounded scan. Explicit snapshot/paging reads
  retain the native readers and totals; they are not part of the live-tick
  budget. Loaded older-page edits are observed; edits outside the tracked loaded
  window may wait for normal paging. Viewers share the ID budget; exhausting it
  resets the feed. Queries retain the existing deadlines and 8 MiB output cap;
  the native page and replay bounds still apply. SQLite opens remain read-only/query-only; observation never
  changes journal mode or checkpoints. Source generations include schema version
  and WAL incarnation, so vacuum, schema changes, WAL rotation and replacement
  invalidate old pages. Deletion within the observed window, plan, diagnostics
  or availability changes require a fresh snapshot, as do gaps, overflows or
  incompatible revisions. Unchanged unavailable sources retry at five seconds,
  but a file change wakes them at the normal cadence.
- Each connection has one active conversation and at most one pending update
  request. Observed pages return an opaque per-client `subscription_id`; update
  and unsubscribe requests must echo it. An old unsubscribe cannot cancel a
  replacement subscription. Update requests wait at most ten seconds. Views unsubscribe on close,
  lock or backgrounding; disconnected or abandoned interest expires within
  thirty seconds. No prompt is replayed on reconnect.
- Phone history and provisional content share the existing 32 MiB content
  budget: 31 MiB history plus at most 1 MiB / 16 provisional entries. History
  retains the existing 20,000-entry ceiling. Caches remain bounded and
  memory-only. At most 24 message articles mount at once; scrolling near the
  top reveals earlier loaded windows, and reaching a window's bottom reveals
  newer loaded messages. Search covers all loaded messages. Reading an older
  window keeps its entry anchor rather than dropping content underneath an
  unrelated tail update. Following resumes only at the actual loaded tail's
  bottom, without a jump button, across updates and render-window shifts.
  Tables/code retain their own horizontal scrolling. Expanded tool
  cards and up to 16 table offsets per message survive loaded-window unmounts.
  This bounded UI state belongs to the view's target/source scope, is pruned with
  loaded entries, and is cleared when that scope or view is discarded.

These are resource limits and scheduling targets, not physical-phone latency or
frame-rate measurements. Network, ownership checks, parsing and rendering add
latency. Owner phone usability and fixed-device performance acceptance remain
separate from synthetic/runtime and browser automation evidence.

## Optional activation and rollback

Do not install or reload the integration in shared/owner profiles as part of
ordinary tests. In an explicitly selected private or owner-authorized profile,
use the existing `relay/pi-commands.sh install <agent-dir>` procedure, then load
or reload the integration in the intended stock Pi process. The update includes
`activity.mjs` beside `bridge.mjs` and `index.ts`. Keep the normal Pi binary stock.
No dev tunnel or owner-service operation is needed by the observer.

To stop observing, close Full history/Conversation, lock the app, or leave it in
the background. To remove the optional integration, use the existing
`relay/pi-commands.sh remove <agent-dir>` procedure and reload only the selected
Pi process. The removal script preserves unrelated files. Removing the bridge
returns Pi to visibly recorded-only history. No terminal mode, model, saved
settings or owner session is changed by reading.

## Qualification

`tests/pi-activity.test.mjs` covers bounded projection, provisional/native
reconciliation, branch cursors, late-load visibility, cadence/coalescing and the
private socket. `tests/pi-live-contract.mjs` is an explicitly invoked, pinned
stock-runtime synthetic public-SDK fixture with private environment paths and a
network guard. It is not an owner-account, interactive-TUI or physical-phone
qualification. `tests/pi-live-tui.mjs` additionally exercises the stock CLI in
private Bun PTYs in regular and fullscreen modes, with a synthetic provider,
explicit extensions, private profiles and network guards. With
`PI_TEST_APP_FIXTURE` pointing to a privately compiled `internal/app` test binary,
it additionally enrolls a private Chromium phone-sized context through the
actual encrypted transport and history/update handlers. That fixture supplies
synthetic inventory/process-info responses, uses actual Pi socket peers, and
rejects phone-originated agent mutations. A private PTY keyboard prompt verifies
that desktop input still reaches stock Pi while the browser is observing, and
that the persisted user prompt arrives before assistant completion. Run only
from a clean environment with private paths
and existing local dependencies; it is opt-in, not part of default checks.
Keep failed-run evidence and the exact tested source identity;
passing an SDK fixture does not establish custom-TUI or all-provider parity.

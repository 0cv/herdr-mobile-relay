# Terminal History Journal — Design & Implementation Plan

Status: **implemented, then retired (2026-08-16).** This document is kept as
the decision record for why terminal history is now rendered as Herdr's raw
pane window. See §0 below before reading anything else.
Scope: relay (Go) + phone app (Svelte frontend).
Origin: 2026-08-15 debugging session — resize seam fragments ("…going sil"),
unboxed stale-width command blocks, and the structural limits of client-side
history stitching.

---

## 0. Outcome — the journal was retired for the raw window

The journal was fully implemented (capture, seams with pre-resize screen
snapshots, paged API, disclosure UI) and worked as specified. It was retired
the next day, live testing having established three facts:

1. **Herdr reflows everything.** Measured live: `recent-unwrapped` output is
   re-split and re-padded to the current pty width on every read — a window
   read at 151 columns shares zero raw-equal lines with the same pane at 58
   columns. Row identity across a width change does not exist, so any
   retained history (client merge or relay journal alike) inevitably mixes
   width generations, marked by seams.
2. **Reflow also makes retention unnecessary for the served window.** A fresh
   read while the phone holds the lease is the entire ≤1,000-row window
   already rendered at phone width — strictly better than anything
   reconstructed, at zero complexity.
3. **Seams accumulate and read as damage.** Real phone use produced stacked
   "N rows from before a resize" disclosures interrupting a single reply.
   Honest, but suboptimal versus simply showing Herdr's window.

Decision: render the raw window, cap the Terminal History setting at Herdr's
per-read limit (options 100/500/1,000), delete the journal (relay package,
protocol capability, app machinery, seam UI). The lease flow, settle-window
display suppression, and stale-width degradation (unboxing, border collapse)
remain.

**Herdr's cap, verified in source** (`src/app/api_helpers.rs`,
`read_terminal_snapshot`): `lines.min(1000)` — a one-line API clamp; the
snapshot functions honor any requested count, and retention is separately
configurable (`advanced.scrollback_limit_bytes`, panes observed holding 7k+
rows). Raising the cap is a trivial patch but means maintaining a Herdr fork
per host, and each read renders/serializes the window while the relay polls
at 250 ms — deep windows want on-demand reads, not poll-rate reads. The
sustainable path is an upstream request to make the per-read cap
configurable; the relay already passes `lines` through, so a raised cap
scales the raw-window design with a one-line options change.

Everything below is the historical design and its findings.

How to use this document: read §1–§5 to understand the system and the
problem; §6–§9 are the design; §10 is the work, phase by phase, with
acceptance criteria; §11 is the code you get to delete; §12–§14 are testing,
rollout, and risks. Line numbers drift — always locate code by the symbol
names given here (grep for them), not by line.

---

## 1. Glossary

| Term | Meaning |
|---|---|
| **pane** | One terminal session managed by Herdr, addressed as `w8:p35` (workspace:pane). Backed by exactly one pty. |
| **pty** | The pseudo-terminal device. Has exactly **one** kernel winsize (columns × rows) at any moment. |
| **Herdr** | The external terminal multiplexer (embeds a Ghostty-based emulator). The relay talks to it over a Unix socket via `internal/herdr/client.go`. |
| **scrollback** | Rows that scrolled off the top of the live screen. Committed at the width they had when they scrolled out; never rewritten. |
| **lease** | A temporary claim on a pane's width. When the phone opens a terminal, it leases the pane to its measured columns; on release the baseline width is restored. Managed by `internal/panesize/manager.go` (`Manager.Acquire`, `Manager.Release`, `Manager.ActiveColumns`, `Manager.reconcile` — multiple clients resolve to the minimum columns). |
| **SIGWINCH** | Signal a process receives when its pty is resized. Full-screen agents (omp, claude, codex…) react by repainting their visible screen at the new width. |
| **redraw junk** | The side effect of that repaint: the old screen's rows get pushed up into scrollback, so scrollback accumulates duplicated transcript blocks and frozen chrome (status bars, input boxes) around every width change. |
| **settle window** | `paneResizeSettleWindow = 3s` in `internal/app/pane_watch.go`. Frames read within 3 s of an actual leased-width change are stamped `resize_settling: true` (see `preparePaneResponse` in `internal/app/server.go`, which calls `Manager.ResizedWithin`). |
| **pane watch** | Server push loop: `internal/app/pane_watch.go`, polls a watched pane every `defaultPaneWatchInterval = 250ms`, sends `pane_content` / `pane_delta` frames with content fingerprints. |
| **`viewport_only` frame** | A pane read made while a lease is active (request carries `terminal_columns`). Shape: newest ≤1,000 rows; the last `viewport_rows` rows are the live screen. Assembled in `internal/coordinator/dispatch.go` (`HandleReadPane`) and `internal/app/server.go` (`preparePaneResponse`). |
| **the merge** | `mergeResizedTerminalHistory` in `frontend/src/lib/terminal.ts` — the client-side stitching this plan deletes. It accumulates "committed history" across frames using text matching (overlap + anchor) and drops rows inside the settle window. |
| **regime** | This plan's term for a span of time during which the pane width did not change (e.g. "baseline 168 cols", "leased 52 cols"). Regime boundaries come from the panesize manager, never from text. |
| **journal** | This plan's new relay-side store: an append-only ledger of logical lines per pane, tagged by regime, paged to the phone on demand. |

---

## 2. How the system works today

### 2.1 File map (relay)

| Path | Role |
|---|---|
| `internal/app/server.go` | WebSocket message dispatch (`lease_pane_size`, `read_pane`, `upload_image`, …), `preparePaneResponse` (adds `truncated`, `viewport_only`, `viewport_rows`, `resize_settling`), capability advertisement in the `push_config` hello (search `protocol.Capabilities`). |
| `internal/app/pane_watch.go` | Per-client pane watch loop; fingerprints; `pane_delta` frames via `internal/panedelta`. |
| `internal/coordinator/dispatch.go` | `HandleReadPane` → `readPaneForDisplay`: **every display read uses `recent`** (physical rows) except unresized Claude panes, which use `recent-unwrapped`. Also caps content lines (`capPaneContentLines`). |
| `internal/herdr/client.go` | `ReadPane` (= `recent-unwrapped`), `ReadPaneRecent` (= `recent`), `ReadPaneVisible` (= `visible`). |
| `internal/panesize/manager.go` | Lease lifecycle. Key methods: `Acquire`, `Release`, `ReleaseClient`, `SweepExpired`, `ActiveColumns`, `ActiveRows`, `ResizedWithin`, `reconcile` (min columns across leases), `restore` (baseline on release). Width applied via `stty` on the pane's tty (`setColumns`). |
| `internal/protocol/protocol.go` | `Capabilities` list, writable-action allowlist, decode-failure responses. |
| `cmd/fake-herdr/` | Scripted Herdr stand-in used by Go tests (validates `--source recent|recent-unwrapped|visible`). |

### 2.2 File map (frontend)

| Path | Role |
|---|---|
| `frontend/src/lib/store.ts` | Relay connections, E2EE, message routing (`handleMessage`), pane frame plumbing (`terminalFrames`, fingerprints, `viewportOnly`/`viewportRows`/`resizeSettling` passthrough — search `message.resize_settling`). |
| `frontend/src/components/TerminalView.svelte` | The terminal screen. Lease flow (`leasePaneSize`, `flushPaneSizeLease`), frame application (`applyFrame`), and the **merge driving state**: `resizeHistoryBaseline`, `resizeHistoryState`, `resizeHistorySkipCommit`, `resizedTerminalHistoryFrame`, `beginResizeSettling`, `scheduleSettledPaneRead`. |
| `frontend/src/lib/terminal.ts` | ANSI → HTML rendering (`renderTerminalContent`, `terminalHtmlRows`), responsive degradation of stale-width grids (`responsiveTerminalGridLine`, `trimTerminalChrome`), separators (`TERMINAL_SEPARATOR_TOKEN`), seam rule (`TERMINAL_RESIZE_SEAM_TOKEN`), and the **merge machinery** (§11). |
| `frontend/src/lib/config.ts`, `preferences.ts` | `TERMINAL_HISTORY_KEY`, options `[100, 1000, 5000, 10000]`. |
| `frontend/src/components/SettingsView.svelte` | Terminal History fieldset + hint copy. |
| `frontend/tests/unit/helpers.test.ts` | Unit tests incl. the merge suite. |
| `frontend/tests/browser/mobile-journeys.spec.ts` | 65 journeys against an in-page fake relay (`boot`, `handshake`, `server(page, …)` helpers defined near the top of the file). |

### 2.3 Data flow when the phone opens a terminal (today)

1. App opens pane → first `read_pane` (no lease yet) → full frame at the
   current (usually desktop) width. `TerminalView` stores it as
   `resizeHistoryBaseline`.
2. App measures its columns → `lease_pane_size` → `panesize.Manager.Acquire`
   runs `stty` on the pane tty → SIGWINCH → agent repaints at phone width.
3. All subsequent reads carry `terminal_columns` → relay answers with
   `viewport_only` frames (`readPaneForDisplay` uses the `visible` source).
   Frames read within 3 s of the width change also carry
   `resize_settling: true`.
4. `TerminalView.resizedTerminalHistoryFrame` merges each `viewport_only`
   frame into client-side "committed history"
   (`mergeResizedTerminalHistory`): the last `viewport_rows` rows are the
   live screen; rows above are appended past a text **anchor**; flagged
   frames commit nothing (their scrolled rows are dropped as junk).
5. On close, the lease releases; `Manager.restore` puts the baseline width
   back; the agent repaints for the desktop again. The merged state is
   discarded.

### 2.4 Why that breaks (the bug story)

Committed history is built by *text matching across polls*. During a width
change the agent re-renders, and rows displaced inside the settle window are
dropped wholesale because junk and genuine rows are textually
indistinguishable. Observed result: history containing a 50-column wrap of a
sentence spliced directly onto a 53-column wrap of the same sentence with
the joining fragment ("ent ") lost — a mid-word seam. Additional structural
problems:

- Herdr hard-caps every read at 1,000 rows (`lines.min(1000)` in Herdr's
  `api_helpers.rs`; **not configurable**). History beyond that exists only
  in one app session's memory.
- The phone sleeps/backgrounds; when polling resumes past the anchor, rows
  are unrecoverable ("window advanced past the anchor").
- Every seam decision is a client-side guess about server-side events the
  relay knows exactly (lease timestamps).

An interim mitigation already exists (uncommitted at the time of writing):
`TERMINAL_RESIZE_SEAM_TOKEN` renders a dashed rule where the settle window
dropped rows. The journal makes the *drop* rare and exact; the rule remains
the honest marker for it.

---

## 3. Verified constraints (do not re-litigate without new evidence)

1. **One width at a time.** One pty ⇒ one winsize ⇒ one rendering. A TUI's
   byte stream is width-specific (absolute cursor addressing), so replaying
   it into a second emulator at another width produces garbage. You cannot
   render the same session at two widths.
2. **Periodic phone-width snapshots don't work.** A SIGWINCH repaint covers
   only the visible screenful (~40–90 rows), not the interval since the
   last snapshot (coverage ~5%), and every forced repaint deposits redraw
   junk into the single scrollback the desktop user also sees.
3. **Herdr read sources** (`internal/herdr/client.go`):
   - `visible` — physical grid rows, exact screen geometry;
   - `recent` — physical wrapped rows at commit-time width;
   - `recent-unwrapped` — logical lines with emulator soft-wraps rejoined,
     **but only up to the current pty width**. Measured live (2026-08-16):
     Herdr re-splits and re-pads its unwrapped output whenever the width
     changes — a 400-line window read at 151 columns shares *zero* raw-equal
     lines with the same pane read at 58 columns, and no per-line
     normalization recovers the correspondence (max line length tracks the
     width: 151 → 58). Unwrapped lines are therefore width-stable **within
     one regime only**; nothing per-line survives a regime boundary.
   Limitation: rows the agent's own renderer composed (tables, box chrome,
   hanging indents) are hard rows either way. The journal stores every line
   at the width that was active when it scrolled off; the phone re-wraps
   wider lines with CSS.
4. **The relay is the width authority.** `panesize.Manager` owns every
   lease, baseline, applied-column change and `ResizedWithin` timestamps.
   Regime boundaries must come from it — never from text.

---

## 4. Target architecture

Two concerns, two mechanisms:

| Concern | Source | Width behavior |
|---|---|---|
| Live viewport + interaction | `visible` + phone lease (unchanged) | phone-exact grid, agent-composed |
| History (scrollback) | **relay journal** of `recent-unwrapped` logical lines | prose re-wraps at any width; composed layout width-tagged |

```
herdr pty ──visible (physical rows)──────────▶ live screen (grid-exact, leased)
         └─recent-unwrapped (logical lines)──▶ relay journal ──paged──▶ phone history
                                               (append-only,            (re-wrapped at
                                                regime-tagged)           phone width)
```

Consequences:

- The lease's job shrinks to the live surface. Content streamed while the
  phone watches is agent-composed at phone width — natively readable in the
  journal forever.
- History rendering never reconstructs anything: the journal is the single
  source of truth, captured once, server-side, with exact lease knowledge.
- Deep history pages past 1,000 rows (the journal has no Herdr cap).
- The entire client-side merge apparatus is deleted (§11).

---

## 5. Journal semantics (normative)

### 5.1 Entry model

```go
// internal/journal/journal.go
type EntryKind uint8

const (
    EntryLines EntryKind = iota // ANSI logical lines captured in one regime
    EntrySeam                   // rows were dropped at a lease transition
)

type Entry struct {
    Seq        uint64    // monotonically increasing per pane, never reused
    Kind       EntryKind
    Columns    int       // applied pty columns when captured
    Leased     bool      // true if a phone lease was active
    CapturedAt time.Time
    Lines      []string  // EntryLines: ANSI logical lines (no trailing \n)
    Dropped    int       // EntrySeam: number of physical rows dropped
}
```

### 5.2 Capture rules

- One goroutine per journaled pane (the "capturer"), started on first phone
  interest (first `read_pane` or watch from a phone client), stopped after
  `journalIdleTTL` (default 30 min) without watchers.
- Every tick (reuse `defaultPaneWatchInterval = 250ms`), read
  `recent-unwrapped` (ANSI format, 1,000 lines) and append **new** logical
  lines to the ring:
  - **Within a regime**, append detection is *exact-row matching* of the
    previous read's tail against the new read (same primitive family as
    `internal/panedelta`, but on logical lines). Same width + 250 ms cadence
    ⇒ overlap is essentially always found; if not (e.g. >1,000 lines burst
    between ticks), append everything and record `EntrySeam{Dropped: -1}`
    (unknown count) — never guess.
  - **At a regime boundary** (the panesize manager reports a lease change
    since the last tick — expose a `ChangeSeq(paneID)` counter or reuse
    `ResizedWithin`), capture stops until the settle window passes, then
    re-baselines: the first post-window read becomes the new tail; rows
    that appeared during the window are **not** journaled; if any non-empty
    rows were skipped, append `EntrySeam` with the observed drop count.
    Cross-regime text matching is **prohibited by design** — that is the
    brittleness being deleted.
- The live screen's rows are never journaled while still on screen: only
  lines that have scrolled past the screen top (i.e. present in
  `recent-unwrapped` output *above* the final `viewport_rows` region) are
  eligible. Use `Manager.ActiveRows` / the read's viewport metadata to cut.
- Ring bound: `journalMaxLines = 10_000` logical lines per pane (~1–2 MB
  worst case). Evicting from the front increments a `truncated` flag the
  API reports.

### 5.3 What the journal does NOT do

- It does not persist across relay restarts (v1 is in-memory; disk spill is
  future work).
- It does not de-duplicate agent redraws outside settle windows (they don't
  occur outside them — that's what the settle window *is*).
- It does not transform content: entries store raw ANSI logical lines; all
  rendering decisions stay in the app.

---

## 6. Protocol & API

### 6.1 Capability

Append `"pane_history"` to `protocol.Capabilities`
(`internal/protocol/protocol.go`). The app must treat absence as "relay too
old" and fall back (§8.3).

### 6.2 New message pair (as shipped)

Request (app → relay; routed in `internal/app/server.go` — it is read-only, so
it does **not** join the audited-write allowlist):

```json
{
  "type": "read_pane_history",
  "request_id": "hist-1723750000-abc",
  "pane_id": "w8:p35",
  "before_seq": 4200,        // backwards: entries with seq < before_seq
  "follow": false,           // forwards: entries with seq > after_seq
  "after_seq": 0,            // with follow, 0 means "from the ring start"
  "limit": 500               // max lines per page; server clamps to 1000
}
```

Both directions are required in practice: the app opens a pane, pages
**backwards** to fill the Terminal History window, then follows **forwards**
on every frame poll (throttled), because rows that scroll off the live screen
become journal entries the app would otherwise never see — omitting the
forward direction leaves a growing hole between the journal tail and the live
screen. `follow` is an explicit direction flag rather than "`after_seq > 0`":
a pane whose journal was empty when it opened follows from sequence zero, and
an absent field is indistinguishable from zero on the wire.

Response (relay → app):

```json
{
  "type": "pane_history",
  "request_id": "hist-1723750000-abc",
  "pane_id": "w8:p35",
  "entries": [
    {"seq": 4180, "kind": "lines", "columns": 168, "leased": false,
     "lines": ["\u001b[1mBoth questions answered…\u001b[0m", "…"]},
    {"seq": 4181, "kind": "seam", "columns": 52, "leased": true, "dropped": 14},
    {"seq": 4182, "kind": "lines", "columns": 52, "leased": true, "lines": ["…"]}
  ],
  "next_seq": 4180,          // oldest returned seq; pass back as before_seq
  "complete": false,         // backwards: reached the ring start
                             // forwards: nothing newer is retained
  "truncated": true          // the ring evicted lines older than this page
}
```

`before_seq` is exclusive and `next_seq` is the oldest sequence the page
returned, so paging never repeats or skips an entry even after eviction.
`next_seq` is 0 on a complete page and on every forward page.

Decode-failure shape: extend `DecodeFailureResponse` in
`internal/protocol/protocol.go` with a `read_pane_history` →
`pane_history {ok:false-style error}` mapping, mirroring `upload_image`.

### 6.3 Unchanged surfaces

`pane_content` / `pane_delta` frames, the lease actions, and the settle flag
stay exactly as they are — the live viewport pipeline is not touched.

---

## 7. Frontend rendering design

### 7.1 Model in `TerminalView.svelte` (as shipped)

- State: `historyEntries` (oldest→newest, `$state.raw` — deep proxies would
  cost on every row read and break frame-identity comparisons),
  `historyNextSeq` (backward cursor), `historyNewestSeq` (forward cursor),
  `historyComplete`, `historyLoading`, `historyRingTruncated`.
- Composition, not per-entry rendering: `journalHistoryFrame` builds
  `[...journalRows, ...liveScreenRows]` into one content string and hands it
  to the existing `renderTerminalContent` pipeline. Per-entry `columns` is
  **not** needed for rendering — `responsiveTerminalGridLine` already degrades
  any line wider than the phone grid, whatever width composed it — so the
  virtualizer, find, copy and anchor machinery stay untouched.
  `kind: "seam"` contributes one `TERMINAL_RESIZE_SEAM_TOKEN` row.
- Paging: on open, page backwards until the Terminal History setting is
  covered or the ring start is reached. On every viewport frame, follow
  forwards (throttled to 750 ms) so rows that just scrolled off the screen
  land in history before the screen drops them. Retained entries are trimmed
  to twice the configured window.
- Anchoring: a fetched page prepends rows, so `journalHistoryFrame` reports
  exactly how many rows appeared above the screen and `applyFrame` subtracts
  them from `renderedRowShift`. Without that the reader jumps a page.
- Optional (Phase 4): entries with `leased: false` and box/table content
  collapse behind a "N desktop-width rows" disclosure.

### 7.2 Store plumbing (`store.ts`)

- `readPaneHistory(agent, { beforeSeq, afterSeq, limit })` → sends
  `read_pane_history`, resolves on matching `request_id` (same pending-map
  pattern as `uploadImage`).
- Route `pane_history` in `handleMessage`; reject pending pages on disconnect.

### 7.3 Fallback (relay without `pane_history`)

Render exactly what `read_pane` returns (the raw ≤1,000-row window),
scrolled-back rows included, **without any stitching**. This shows redraw
junk near resize points — accepted; it is what a real terminal shows, and it
is strictly less wrong than the merge. The merge is never kept as a
fallback.

---

## 8. Phases, tasks, acceptance

Work top-to-bottom; each phase lands independently green. Do not run
formatters or project-wide suites mid-phase; validate at each phase end with
the commands listed.

### Phase 0 — probes (done 2026-08-15, live Herdr 0.8.0, scratch pane `wE:p1`)

1. **`recent-unwrapped` ANSI fidelity: confirmed.** A 484-column red/green
   line on a 151×46 pane occupies 4 physical rows in `visible`/`recent`,
   each row separately SGR-terminated (`ESC[0m ESC[38;5;2m … ESC[0m`). The
   same read at `--source recent-unwrapped` returns it as **one** logical
   line with the SGR prefix intact and the mid-word split (`filler80-1` +
   `2 filler…`) rejoined verbatim. 174 physical rows collapsed to 90
   logical lines.
   Note: `--lines N` counts **physical** rows even for `recent-unwrapped`
   (20 rows → 11 logical lines), so the journal always requests the 1,000
   row maximum.
   Caveat found while smoke-testing capture: one long-lived pane returned
   `recent-unwrapped` lines that **fused two independent short rows** (row
   padded to the full 151 columns, then the next row appended — 38 of 85
   lines). It reproduces from the CLI on that pane, so it is Herdr-side, not
   relay-side, and it also affects today's Claude display path, which already
   reads `recent-unwrapped`. It did **not** reproduce on clean panes after an
   `stty` resize, after an alternate-screen app exited, or after a block of
   genuinely soft-wrapped lines. Capture tolerates it: fusion is settled
   before a row becomes eligible, so the live run journaled 75 streamed lines
   with no loss, no duplication and no spurious seam.
2. **Alternate screen: no special case needed.** While `less` was in the
   alternate screen, all three sources returned exactly the 46 visible rows
   — `recent`/`recent-unwrapped` do **not** traverse the app's own history
   and do **not** expose the primary scrollback. On quit the primary
   scrollback returned intact and the alt-screen rows were never committed
   to it. Because the capturer only journals lines above the live screen
   (§5.2), an alt-screen pane yields zero eligible lines: the rule already
   covers it. No alt-screen detection is implemented.
3. **Per-agent variance: journal captures every pane as `recent-unwrapped`.**
   The display path's source choice is a live-grid concern (physical rows
   keep agent chrome cuttable by exact row count); the journal's purpose is
   width-independent logical lines. Qoder needs no exception: its box chrome
   is agent-composed, so it is a hard row in `recent-unwrapped` too and is
   width-tagged through the entry's `columns`, which drives the existing
   `responsiveTerminalGridLine` degradation on the phone.

**Wrap model (used by the screen cut, §5.2).** A logical line of display
width `L` occupies `max(1, ceil(L/columns))` physical rows; verified against
both probe lines (249 cols → 2 rows, 484 cols → 4 rows at 151 columns). The
capturer withholds the minimal set of trailing logical lines covering the
pane's `rows` physical rows, so the journal never contains a row that is
still on the live screen and never gaps once the pane scrolls.

### Phase 1 — relay journal core (done)

1. New package `internal/journal`:
   - `journal.go`: types from §5.1, `Journal` (per-pane ring + mutex),
     `Append`, `Seam`, `Page(beforeSeq, limit)`, eviction.
   - `capture.go`: the capturer goroutine (§5.2). Dependencies injected as
     narrow interfaces (`paneReader`, `widthAuthority`) so tests use fakes —
     follow the style of `internal/panesize/manager_test.go`
     (`fakeProcessInfoProvider`, `fakeCommandRunner`).
2. Wire lifecycle in `internal/app/server.go`: start/stop capturers from
   pane interest (piggyback on `startPaneWatch` / watch teardown), global
   shutdown alongside `paneSizeM`.
3. Unit tests (`internal/journal/journal_test.go`, `capture_test.go`):
   - same-regime append across ticks, exact overlap;
   - burst larger than the read window → append-all + unknown-count seam;
   - lease transition → settle skip → re-baseline + seam with count;
   - no cross-regime matching (construct a case where text *would* match
     across widths and assert it is not used);
   - ring eviction sets `truncated`;
   - lifecycle: idle TTL stops the capturer; renewed interest restarts it.
   Acceptance: `gofmt -l internal && go vet ./... && go test ./...` clean.

**Delivered as described, with three deviations found while building it.**
   - Interfaces are exported (`PaneReader`, `WidthAuthority`) because the app
     package constructs the capturer.
   - `panesize.Manager.Geometry` was added as the width authority's read side:
     leased panes answer from lease state, unleased panes resolve through the
     pane's tty **outside** the manager mutex so a journal poll can never queue
     a phone's lease behind three subprocesses. The capturer caches geometry
     for `GeometryTTL = 2s` and refreshes immediately whenever the manager
     reports a resize.
   - **Journals are never reconciled against the poller's inventory.** That
     list only carries panes that report an agent, so reconciling wiped the
     journal of every plain shell pane every poll — caught by a live socket
     probe, not by a unit test. Instead a capturer drops its own journal when
     the pane stops answering for ~2 minutes (`captureFailureLimit`) or when
     interest goes stale, which also prevents a restarted capturer from
     splicing a fresh baseline onto pre-gap history.

### Phase 2 — API + capability (done)

1. `internal/protocol/protocol.go`: add `pane_history` capability +
   decode-failure mapping. Update `protocol_test.go`.
2. `internal/app/server.go`: route `read_pane_history` → journal `Page`,
   clamp `limit`, answer `pane_history`. Update `server_test.go` with the
   request/response contract (§6.2), including the not-journaled-pane case
   (`complete: true`, empty entries).
3. `cmd/fake-herdr`: no change needed (journal reads go through
   `herdr.Client`), but extend the **browser-harness fake relay**
   (`frontend/tests/browser/mobile-journeys.spec.ts` `server(...)` helpers)
   to speak `pane_history` for Phase 3's journeys.
   Acceptance: `go test ./...` clean; a hand-driven socket session
   (existing E2EE probe pattern) pages history off a live dev relay.

### Phase 3 — phone cutover + deletion (done)

1. Implement §7 (store plumbing, paged rendering, fallback).
2. Delete §11's inventory. The `resize_settling` flag keeps exactly one
   client-side use: suppressing *display* of transient frames mid-resize.
3. Rewrite tests:
   - `helpers.test.ts`: drop the merge suite; add entry-rendering tests
     (prose re-wrap, seam rule, grid degradation per `columns`).
   - `mobile-journeys.spec.ts`: rewrite "preserves deep history…" and the
     junk-window journey against fake-relay `pane_history`; add: paging
     three pages up keeps scroll anchor; resize storm (three lease changes
     mid-stream) yields zero lost prose lines and exactly the seams the
     fake relay emitted; old-relay fallback journey (no capability →
     raw window, no `read_pane_history` sent).
4. Update Settings hint copy, `README.md` (Terminal History section),
   `CHANGELOG.md` (Unreleased), and delete stale comments referencing the
   merge.
   Acceptance: `make check` green (both browser engines, size budget —
   expect a net *reduction*; lower the `limitKiB` guard in
   `frontend/scripts/check-size.mjs` back down if the bundle shrinks below
   the current ceiling).

**Delivered.** Two notes for whoever reads the diff:
   - `resizeFrameBaseline` survives as the *only* client-side use of the
     settle flag: while it is set and the current frame is either the
     pre-resize one or flagged `resize_settling`, the view keeps showing
     "Resizing terminal…" instead of a half-repainted screen. Everything else
     in §9's settled-read row is gone, including the timers.
   - Both `historyEntries` and `resizeFrameBaseline` are `$state.raw`. Svelte 5
     deep-proxies objects assigned to `$state`, which silently broke the
     `next === resizeFrameBaseline` identity check — the placeholder never
     appeared. Anything compared by identity in this component must stay raw.

### Phase 4 — polish + release

1. Optional: collapsed desktop-width segments (disclosure UI).
2. Relay load measurement: 10 journaled panes on the dev relay; record CPU
   deltas in this file; tune tick/TTL if needed.
3. Release via the validated flow: version bump → `make web-release` →
   `make check` → commit → tag → push → watch `check`/`release` workflows →
   verify GitHub Release assets. (See CHANGELOG 0.15.x releases for the
   exact command sequence precedent.)

---

## 9. Deletion inventory (Phase 3 — applied)

Every symbol below is gone from the tree; the table is kept as the record of
what the journal replaced.

`frontend/src/lib/terminal.ts` — deleted:

| Symbol | Note |
|---|---|
| `mergeResizedTerminalHistory` | the merge |
| `ResizedTerminalHistory`, `MergedResizedTerminalHistory` | its state types |
| `terminalViewportOverlap` | viewport-only overlap matcher |
| `resizedStableAnchor`, `resizedAnchorMatchEnd` | anchor matcher |
| `MAX_RESIZED_TERMINAL_HISTORY_LINES` | merge ring bound |
| helpers left unused after the above (check `normalizedTerminalLine` consumers) | |

`frontend/src/components/TerminalView.svelte` — deleted:

| Symbol | Note |
|---|---|
| `resizeHistoryBaseline`, `resizeHistoryState`, `resizeHistoryTruncated`, `resizeHistorySkipCommit` | merge state |
| `resizedTerminalHistoryFrame` | merge driver, replaced by `journalHistoryFrame` |
| `resizeBaselineRequestPane`, `resizeBaselineRequestFrame` | pre-lease baseline read; the relay captures the baseline now |
| `resizeExpectedLines`, `resizeSettleDeadline`, `resizeReadPending`, `scheduleSettledPaneRead`, the timers in `beginResizeSettling` | settled-read heuristics |
| `rememberResizedTerminalFrame` / `validResizedTerminalFrame` cache, `applyCachedResizeFrame`, the whole `<script module>` block | existed to preserve merged state across reopens |

Kept on purpose: lease flow (`leasePaneSize`, `flushPaneSizeLease`),
`TERMINAL_RESIZE_SEAM_TOKEN` + `.term-resize-seam`, `renderedRowShift`
(live-view virtualization, now also compensated for prepended history pages),
`responsiveTerminalGridLine` (stale-grid degradation), and `resizeFrameBaseline`
as the single transient-display suppressor.

Tests deleted/rewritten: the merge suite in `helpers.test.ts`
("drops the redrawn scrollback block…", overlap/anchor cases, the seam
tests added 2026-08-15); the deep-history and junk-window journeys.

---

## 10. Testing strategy summary

| Layer | Tool | What proves it |
|---|---|---|
| Journal core | `go test ./internal/journal/...` with fakes | capture rules §5.2, exhaustively |
| API contract | `internal/app/server_test.go` | request/response, clamps, failure shapes |
| Rendering | `frontend/tests/unit` (vitest) | entry → HTML (re-wrap, seams, grids) |
| End to end | `mobile-journeys.spec.ts`, both engines via `make frontend-browser` | paging, resize storm, fallback |
| Live smoke | dev relay + real phone (`make dev-tunnel`) | scroll an actual long omp session through several rotations; verify no seams and correct collapse |

Never validate with piecemeal commands before shipping: the release gate is
`make check` (it also catches `frontend/dist` vs `web/` divergence).

---

## 11. Rollout & compatibility

- New app + old relay: capability absent → raw-window fallback (§7.3).
- Old app + new relay: nothing changes (`read_pane` untouched); old bundles
  keep their merge until they update.
- The relay self-updates; the app is served from Cloudflare Pages —
  publish the Pages bundle **before** updating relays (established ordering
  from the 0.15.x releases).

---

## 12. Risks & open questions

| Risk | Status |
|---|---|
| `recent-unwrapped` ANSI fidelity across rejoined wraps | **Closed** — Phase 0 probe 1 confirmed it |
| Alternate-screen apps' unwrapped output semantics | **Closed** — probe 2: the source exposes only the alt screen, and the screen cut already yields zero eligible lines |
| Qoder needs physical rows | **Closed** — probe 3: the journal captures every pane as `recent-unwrapped`; composed chrome stays a hard row and degrades by width |
| `recent-unwrapped` fuses two independent short rows on some long-lived panes | **Open, upstream.** Reproducible from the CLI, not relay-side; affects today's Claude display path too. Capture tolerates it (fusion settles before a row becomes eligible); a fused pair renders as one padded line |
| Herdr reflows unwrapped output at the current width | **Closed by design change** — anchors normalize styling/padding but cannot survive a regime boundary; on any anchor loss the capturer records a `dropped=-1` seam and re-baselines **without appending the window** (an append-everything here duplicated ~1,000 rows on every lease transition and pushed genuine text out of the display slice — the 2026-08-16 missing-table bug). A >1,000-line burst between ticks now loses its window to the seam instead of being replayed; that trade is deliberate |
| The pre-resize screen was structurally lost | **Closed** — the capturer retains each stable read's screen region; a regime-boundary seam carries that snapshot (`Entry.Lines` on seams, tagged with the regime that ended). The phone renders the seam as a tappable rule ("N rows from before a resize") and reveals the snapshot on demand, collapsed by default so idle opens show no duplication against the live re-render |
| Relay CPU/memory at scale | interest-gated capturers + 30 min idle TTL + 10k-line ring; Phase 4 measurement still open |
| >1,000-line bursts between ticks | seam(-1) + re-baseline; the window is not replayed (see reflow row) |
| `displayWidth` counts `\t` as one cell | **Open, minor.** A tab-bearing screen line makes `screenCut` cut one line too deep and can journal a row the screen still shows (transient duplicate). Agents render status chrome with spaces; treat `\t` as an 8-column stop if it ever surfaces |
| Herdr changes read semantics upstream | the journal isolates the app from it |
| Losing a journal to an unrelated event | **Closed** — journals are never reconciled against the agent inventory; a capturer drops its own journal only after ~2 min of unreadable ticks or stale interest |

Optional upstream ask (not required): soft-wrap continuation flags on
`visible`/`recent` reads would let the live screen share the width-free
pipeline someday.

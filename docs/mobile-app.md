# The Herdr Mobile Relay app

What the phone app does once a relay is connected: the agent list, read-only
workspace inspection, and the mobile terminal. Read this if you have finished
setup and want to know what every screen and control is for.

## What it does

- Monitor and control agents across several computers, with new, closed, and
  renamed agents, workspaces, and tabs reflected within seconds through a
  live Herdr event stream.
- Start, rename, clear, and stop agents from relay-provided launch profiles.
- Send prompts, atomic terminal text-and-key input, slash commands, and
  cancellable attachment batches; drafts survive locally for 48 hours. Search
  loaded terminal output and open explicit HTTP(S) links.
- Answer approvals bound to the current prompt, command, and ordered choices
  from Codex, Claude Code, Qoder, Oh My Pi, and Pi, plus structured questions
  from those agents and OpenCode.
- Inspect the current agent's workspace files, images, Git status, upstream
  ahead/behind counts, and unified diffs without exposing a write action.
- Read searchable native conversations for Claude Code, Codex, OpenCode,
  Qoder, Pi, Oh My Pi, Oh My OpenCode, Hermes, and Cursor Agent in focused
  conversation or full-history form; validated Oh My OpenCode plans appear with
  their current task states.
- Configure durable notification categories, settle delay, cooldown, snooze,
  and a neutral delivery test separately for each paired relay and device.
- Pair named controller or reader devices. Reader devices can inspect agents,
  conversations, and workspaces; manage their own notifications; and revoke
  themselves, but cannot control agents or administer other devices.
- Optionally require device verification before the app connects at open, and
  again before the interface unlocks on resume. A resume keeps the existing
  encrypted session, so verifying is instant rather than a fresh connection;
  the unlock screen covers the page and the terminal stops streaming until it
  succeeds.
- Optionally request a screen wake lock only while a terminal is visible, or
  read responses aloud in English, French, German, Spanish, or Chinese. The
  relay synthesizes the audio and streams it to the phone encrypted - real
  media playback that keeps reading with the screen off, and response text never
  reaches a third-party speech server. On hosts with a published Piper runtime,
  setup downloads the neural engine and English voice into
  `$XDG_CACHE_HOME/herdr-mobile-relay/speech` when `XDG_CACHE_HOME` is set, or
  `~/.cache/herdr-mobile-relay/speech` otherwise. Relay updates never touch the
  cache. Reading aloud switches itself on the first time a relay reports a
  voice; after that the setting decides. Every other
  language is downloaded on demand, from Settings on the phone or with
  `relay/speech-voices.sh --languages fr`, and Settings lists what a relay has
  cached and removes voices one language at a time. If the runtime was cached
  before a failed extraction fix, run
  `relay/speech-voices.sh --reinstall-runtime`; this replaces only the engine
  and keeps all downloaded voices. Stock Apple Silicon uses macOS `say` and does
  not offer neural downloads unless Piper is already installed. Without a
  neural voice the relay falls back to espeak-ng, espeak, flite, or macOS `say`;
  a language it cannot speak is reported in Settings instead of failing at the
  Speak button.
- Detect Codex, Claude Code, OpenCode, Qoder CLI, Pi, Oh My Pi, and Kimi.

| Agents | Native Resize |
| --- | --- |
| <img src="../images/home.jpeg" alt="Mobile list of Herdr agents" width="392"> | <img src="../images/native_mobile_resolution.jpeg" alt="OMP terminal rendered at native mobile width" width="392"> |

| Plan Questions | Notifications |
| --- | --- |
| <img src="../images/agent_plan.jpeg" alt="Structured plan question navigation" width="392"> | <img src="../images/notifications.jpg" alt="Blocked-agent notification" width="392"> |

| Git Inspection | Native Conversations |
| --- | --- |
| <img src="../images/git-history.jpeg" alt="Read-only mobile Git diff with diff-aware colors and zoom controls" width="392"> | <img src="../images/conversations.jpeg" alt="Mobile native conversation history rendered from the agent transcript" width="392"> |

| Terminal | Read Aloud |
| --- | --- |
| <img src="../images/terminal.jpeg" alt="Mobile terminal with Copy, Speak, attachments, and terminal keys" width="392"> | <img src="../images/speech.jpeg" alt="Speech settings with the language choice and the relay's cached voices" width="392"> |

### Cursor picker filters

**Send filter text** appends to an open Cursor model picker without pressing
Enter or selecting a model. It supports up to 32 ASCII letters, digits, spaces,
and `.-_/+:()[]`. Trailing spaces, multiline text, and other characters are
rejected without trimming. Other picker layouts may be refused.

The relay sends separate characters at least 150 ms apart and checks the live
filter after visible characters. Interior spaces are verified with the next
visible character. Terminal readback hides trailing spaces already in the
picker: if the first visible character reveals an unexpected prefix, delivery
stops and is reported as uncertain. Inspect the picker before sending more;
the full draft is not restored after uncertain or partial delivery.

Terminal controls are locked while sending. Select separately afterward, or
press Escape to close the model picker. Relay commands are serialized per pane,
but desktop typing is not locked: avoid typing there during delivery. Fresh
reads detect observed interference, not an atomic guarantee against it.

## Paired devices

A controller opens **Settings → Devices → Invite Device** to create a ten-minute,
one-use invitation. The relay draws both a complete setup link and its QR code;
share either privately. The first browser that completes the encrypted
handshake consumes the invitation. Create a separate invitation for every
additional controller or reader, or print the setup link again on the
computer: every print arms the relay's bootstrap for one more phone.

| Invite a device | Paired devices |
| --- | --- |
| <img src="../images/devices-invite.jpeg" alt="Invite Device dialog choosing a name and the reader role" width="392"> | <img src="../images/devices.jpeg" alt="Paired devices with rename, revoke, forget, and reset controls" width="392"> |

An uncaught phone-side error appears in a bottom **App error** banner because
mobile browsers often provide no useful console. Copy or photograph its text
for a bug report, then tap the banner to dismiss it; a later independent error
will display a new banner.

## Opening links from other apps on iOS

External links open in Safari, not the installed Home Screen app. To use those
links, pair Safari as its own device: in the Home Screen app, choose
**Settings → Devices → Invite Device**, open that invitation in Safari, then
choose **Pair this browser instead** and confirm. Safari is listed and revocable
separately; its one-use invitation cannot also pair the Home Screen app.
Safari may clear this site's storage after a week without use; invite it again
if that happens.

Link to an agent with `https://<app>/#notify=<url-encoded {"pane_id","host"}>`,
where the fragment value is `encodeURIComponent(JSON.stringify({ pane_id, host }))`.
Use the agent's pane ID and computer host name. Once Safari is paired, opening
this link from another app takes you to that agent.

## Workspace navigation and inspection

The home screen keeps agents that need input visible at the top. By default,
each workspace below them appears once — mixed — with a dot for its most
notable session: done, then working, then idle. The **Home Workspaces**
setting can separate them into Done, Working, and Idle sections instead; both
layouts retain the workspace and tab hierarchy. On a phone, tap the
magnifying-glass button to search projects, workspaces, paths, tabs, sessions,
agents, hosts, and relays.
At 900 CSS pixels and wider, an agent rail keeps those workspace groups beside
the open terminal.

A workspace card names its computer on the first line, beside the workspace
label, and gives the second line to the workspace directory. Agent rows inside
the card, including those nested in a linked worktree, show their own working
directory rather than repeating that computer name. A directory inside the
computer's home directory is written `~/code/app`, matching how the relay's
directory browser labels it; every other directory is written in full. An
agent that reports no directory keeps its name on that line.

When the relay advertises tab ordering, press and hold an agent card until its
tab lifts, then drag to reorder the tab in Herdr; a plain tap still opens the
agent, and Alt+arrow keys on a focused card provide the same control. The
change is applied to the desktop immediately.

Opened workspace cards remain expanded after visiting an agent and returning to
the home screen.

The workspace cards come from Herdr's complete workspace inventory rather than
being inferred from active agents. A workspace therefore remains visible when
it contains only a shell or no running agent; the phone does not expose that
shell as a generic terminal.

Open **Workspaces** from the folder button in the header to:

- open **Create Workspace**, choose a directory and label in its dialog, then
  confirm a non-focusing workspace with the native initial tab;
- press and hold a workspace card until it lifts, then drag it into place;
  Alt+arrow keys on its reorder handle provide the same accessible control;
- rename or close a workspace, or start an agent as a second tab in a selected
  workspace;
- open **Worktrees** in a dialog, create a branch-backed worktree, or open an
  existing checkout as a non-focusing Herdr workspace; Git worktrees are flat,
  so the dialog is offered on repository workspaces only, never inside a
  linked worktree;
- close a linked-worktree workspace without deleting its checkout, or remove
  the checkout while retaining its Git branch.
- closing a repository workspace with linked worktrees first offers **Close
  Workspace Group**. The second dialog lists the current group and must be
  confirmed separately; Herdr closes the group that is open when the operation
  runs, so membership can change after confirmation. It never removes Git
  checkouts or branches.

Linked worktrees are nested below their repository workspace on both the home
screen and the Workspaces page, drawn as a tree with connector rails that
match Herdr's parent/child presentation. Rows repeat neither the repository
name nor a checkout path that merely restates the worktree's label; a path
appears only when it differs. Dragging a repository moves that complete group
atomically when Herdr exposes `workspace.move_block`; older compatible Herdr
versions can still move a standalone workspace and ask for an update before
moving a linked group.

Normal worktree removal refuses a dirty checkout. **Force Remove** is offered
only after that refusal and requires a second confirmation because it discards
uncommitted checkout changes. Creating a worktree uses Herdr's configured
worktree directory; the phone cannot provide an arbitrary checkout path.

The separate **Remove Worktree** action is the destructive checkout operation.
It retains the Git branch and is subject to the dirty-checkout and force-remove
rules above. Closing a workspace and removing its worktree are never the same
operation.

## Herdr compatibility

Settings reports the installed Herdr client version separately from the running
server version and protocol. It also reports the generation of the relay's
stable JSON endpoint contract and the reason each feature is supported,
unsupported, or still unknown. An endpoint generation is not a claim that all
optional features are available.

Agent, pane, workspace, and tab inventory uses Herdr's JSON operations. The
mobile terminal remains a separate binary-stream compatibility path, so a
server can support inventory and workspace controls while its terminal
transport is unavailable. The event stream is subscribed before each snapshot;
after a reconnect the relay takes a fresh snapshot rather than replaying every
notification missed while disconnected.

**Inspect Workspace** is read-only and is available only when the connected
relay advertises workspace inspection and the agent reports a working
directory. The relay confines reads to that directory, skips symlinks and
common generated directories, and returns at most 4,000 tree entries. Text
previews are limited to 1 MiB and image previews to 5 MiB.

Git inspection disables hooks, pagers, text conversion, external diffs, lazy
fetches, and user/system Git configuration. Status is limited to 2,000 changed
files, individual unified diffs to 1 MiB, and Git commands to eight seconds.
On narrow screens, swipe the file or changed-file sidebar left to collapse it;
the adjacent sidebar button restores it. Pinch the diff or use its zoom
controls to resize it without changing the rest of the app.

## Mobile terminal

**Resize Session** is now a persistent, per-browser/app preference, **off by
default**, including after an upgrade. The old height preference is not consent:
in older apps, turning **Lease Terminal Height** off still leased width.
Height's saved value is retained, but its switch is subordinate to Resize
Session and backend availability.

**Resizing is unavailable in this build, even if either saved preference or
`HERDR_PANE_SIZE_LEASE_ENABLED` is true.** The experimental direct-TTY backend
reopened cached terminal paths without authoritative incarnation validation and
could desynchronize Herdr's grid from the PTY. It has not passed the lifecycle
safety gate. No new size lease is created or renewed, even on an older relay
advertising legacy capabilities. Saving Resize Session consent does not enable
that backend.

Controller terminals preserve desktop-width column alignment and scroll
horizontally. Reader rendering is unchanged (desktop-width text wraps to the
phone viewport). Prompts, keys, approvals, uploads, terminal reading, and
Conversation remain available; reader pairing is not a resizing workaround.
Herdr 0.7.5 remains the minimum. Browser wrapping cannot reconstruct an
independently sized full-screen TUI for one shared PTY.

Old/cached apps are protected by the new relay's default-off gate and unavailable
backend, including direct lease requests. Updating only the app cannot impose
this relay gate or lifecycle fixes on an older relay. If a future qualified
backend is enabled by its operator, old apps may still request automatic leases;
operator consent does not supply per-device consent to legacy apps.

This is containment, **not** safe optional or coordinated resizing. It does not
restore geometry left by older code or an interrupted process. Stop further
leasing (update the relay and app, or close old clients), then perform a normal
local terminal/window resize if necessary. Do not use ad hoc `stty` as a durable
fix. Restarting an old relay is not proof of restored geometry.

For older relays only: ordinary navigation release shortens that client's lease
to at most ten seconds; disconnect attempts cleanup immediately; a lost client's
lease expires after 120 seconds without renewal. Older apps can keep renewing
for five minutes while hidden. These are lease/cleanup timings, **not restoration
guarantees**: old restores can fail and retry indefinitely. This containment build
creates no leases, so there is no online immediate-revocation feature or
restoration acknowledgement to promise. Offline delivery cannot be guaranteed.
Target-bound per-client revocation and generation ordering must be qualified
before any backend is re-enabled. A coordinated Herdr-owned resize contract
remains a separate integration; native-client views alone do not give different
render widths to one shared PTY.

See [resize containment and follow-up evidence](pane-size-leasing.md) for operator
configuration and the remaining safety boundary.

Terminal History keeps 100, 500, 1,000, or 10,000 lines in the terminal view.
1,000 is the default and the ceiling on the gateway-relayed path; direct
connections can use 10,000. The "older history" notice reports when rows beyond
the served window exist. Use **Copy** for the latest response or
**Conversation History** for clean, searchable earlier turns.

In **Settings → Agents → Default View**, choose **Terminal** or
**Conversation** for new agent openings. Terminal is the initial default. In
**Manage agent → Default View**, choose **Use default** or set a preference for
that pane; the menu is available from both Terminal and Conversation. These
preferences are stored only in this browser/app on this device and apply across
its connected computers. A pane preference belongs to the pane, not its harness,
and changing its session or name does not reset it. A new or replaced terminal
inherits the global default.

Preference changes affect the next opening, not the screen currently shown, and
manual Terminal/Conversation switching does not change the saved preference.
Unsupported agents, agents without a session, or unavailable transcripts open
in Terminal instead. Terminal remains one tap away for approvals, questions,
and other terminal-only controls.

For supported agents, the terminal header opens **Conversation History** after
the agent reports a session. It opens on the newest turn and stays there as
turns arrive; scrolling up to read holds your position until you return to the
end. **Conversation** keeps each user prompt and the latest agent answer from
that exchange. **Full history** shows every recorded message with collapsible
tool activity. Both use an escaped Markdown subset, search filters the
currently displayed view, each fenced code block has its own copy control, and
each message can copy its original Markdown.
The composer below the history sends ordinary multiline prompts and accepts
one or more validated attachments from the file picker or clipboard. Supported
files are images, UTF-8 text, Markdown, PDF, JSON, CSV, DOCX, XLSX, PPTX, ODT,
ODS, and ODP. Uploads show per-file progress, can be cancelled, and interrupted
files restart from the beginning. Live uploads are bound to the exact target
generation; after restart, opaque references remain bound to the server
session, pane, terminal, and agent session that created them. The relay
revalidates the current exact target before resolving a reference. The history
does not
render attachments inline. Draft persistence, slash-command suggestions,
hidden-value entry, approvals, structured questions, and terminal menus remain
in the terminal view. The composer locks when one of those interactions needs
attention, and the **Terminal** header button switches to the same agent without
adding repeated view toggles to browser history.

Hidden reasoning, injected system records, and sidechain turns remain excluded.
Claude continuation links are followed within the selected project, so a stale
anchor session can show newer continuation sessions in order. Conversation
History requests up to 200 records at a time but loads only enough logical
exchanges for the current view: tool-only pages and exchanges split across page
boundaries are followed automatically. The newest usable exchange is shown on
open, and scrolling near the top loads earlier exchanges without a normal-path
**Load older turns** click. A small in-memory preview can appear immediately on
a warm reopen while the current latest page is checked; it is bounded, never
persisted by the browser, and never authorizes a cursor or proves that history
is empty. Loading, preparation, context search, errors, and the true beginning
of the available source have separate status messages. Search filters loaded
content only. Older browsing keeps a fixed chain snapshot and continues across
large parent files through the local index; a missing, invalid, ambiguous,
cyclic, or bounded-out continuation remains readable and displays an
incomplete-history warning. Reads are confined to known session directories and
the newest 16 MiB of very large logs. When that bound omits older turns, they
remain in the harness log on the computer. Preparation reports progress and
never blocks the relay's live terminal paths. Snapshot pages remain stable while
new turns are written, and storage, source-change, corruption, oversized-record,
and expired-cursor states are reported without exposing transcript paths or raw
records. Pagination uses only short-lived opaque cursors; the app never
constructs a cursor from a displayed entry ID. OpenCode and Hermes retain their
native database pagination semantics while binding cursors to the selected
source identity.

### Conversation history cache and limits

The relay never edits a provider transcript or native database to serve this
view. SQLite reads use read-only mode; JSONL reads use captured regular-file
handles and reject symlink retargeting. The optional derived cache lives under
`<cache>/conversation-history`, which is created as mode `0700`; each bbolt
snapshot is a regular mode `0600` file. Startup removes abandoned snapshot
files, and shutdown and expiry remove the derived index, not the source
transcript.

Default bounds are one active preparation, four queued preparations, a
30-second client-interest lease, 15-minute snapshot expiry, and 15-minute
cursor expiry. A snapshot is limited to 512 MiB and all snapshots together to
1 GiB. A JSONL record is limited to 16 MiB, a recent window to 16 MiB, a page
to 200 entries, and a response to 2 MiB. Exceeding a cache limit fails the
preparation with a retryable capacity result; it never truncates or deletes a
provider source. A cancelled or abandoned preparation stops at scan and index
checkpoints.

Recent pages report no total when only a bounded tail was read. Prepared and
native pages report their visible-entry total when the source supplies one.
`source_truncated` identifies a bounded JSONL tail, while diagnostics count
oversized or corrupt records and tool/payload omissions. Valid entries remain
available when a later record or an OMO plan update is invalid. A failed
preparation retains its continuation cursor so **Retry** does not lose the
already displayed recent turns. The relay also retains a bounded, in-memory
recent-range projection cache (four projections, 16 MiB per item, 64 MiB total,
and a 60-second idle expiry by default). It rechecks containment, identity, and
the exact range digest before reusing it; the cache contains no source handles,
cursors, or transcript files and is cleared on relay shutdown.

Cursors are signed, opaque, scoped to the provider, workspace, session, pane,
server session, terminal, and target generation, and contain an expiry and
source revision. They bind a file range digest, snapshot identity, or native
source identity; appends are accepted only when the captured range remains
stable. Tampering, scope changes, expiry, truncation, replacement, rewrite,
and symlink retargeting are rejected. OpenCode and Hermes keep their native
pagination anchors while applying the same scope and source checks.

Terminal Refresh controls how often the relay checks a visible pane: 100 ms,
250 ms, 500 ms, or 1 second; 250 ms is the default.

**Find** searches the rows loaded into the current terminal view, highlights
visible matches, and moves between the first 1,000 of them.

Explicit HTTP(S) URLs in terminal output become external links with opener and
referrer isolation. When the last terminal lines name supported key hints such
as arrows, Enter, Esc, Tab, Y/N, or a modifier chord, the app offers matching
one-tap actions through the same ordered key path. Detected actions can be
dismissed and never replace verified approval or structured-question controls.

The terminal controls send **Esc**, **Tab** (`⇥`), **Enter**, arrow keys, and
**F1**–**F12** behind the **F keys** row. **Shift** (`⇧`), **Ctrl** (`^`), and
**Alt** can be combined, remain armed for repeated input, and apply to typed
characters or any available terminal key — arm `^` and type `c` to interrupt.
A live status confirms the exact chord. Toggle the modifiers off or move focus
to the composer to disarm them.

When the pane's last line is a prompt that echoes nothing — `[sudo] password
for …`, an SSH passphrase, `gpg`, a PIN — the app names it and offers a masked
field, even though the ordinary composer stays locked on a screen it cannot
classify. The value is typed into the pane as individual keystrokes followed by
Enter. Nothing is sent until you press Send, and the value is never stored on
the phone, never inserted as pasted text, and never recorded in activity or the
audit log. Relays older than this feature say so and leave the prompt to the
computer.

**Copy** captures the agent's latest completed response without ANSI control
sequences. When the relay advertises response copy and the agent is one of
Claude Code, Codex, Kimi, Oh My Pi, Pi, or Qoder, it runs that agent's own copy
command; every other case takes the visible terminal output. The agent command
refuses a busy composer, so it cannot interrupt an in-flight turn.

# The Herdr Mobile Relay app

What the phone app does once a relay is connected: the agent list, read-only
workspace inspection, and the mobile terminal. Read this if you have finished
setup and want to know what every screen and control is for.

## What it does

- Monitor and control agents across several computers, with new, closed, and
  renamed agents, workspaces, and tabs reflected within seconds through a
  live Herdr event stream (15-second reconciliation backstop).
- Group agents by status and relay workspace: done, working, and idle
  workspaces sit in their own sections, and agents that need input remain
  individually actionable on top. A Mixed setting shows each workspace once
  with a dot for its most notable session state.
- Start, rename, clear, restart, and stop agents from relay-provided launch
  profiles.
- Send durable prompt drafts, terminal keys, slash commands, screenshots, and
  photos; search loaded terminal output and open explicit HTTP(S) links.
- Answer verified Codex, Claude Code, and Qoder approvals, plus structured
  questions from those agents, OpenCode, OMP, and Pi.
- Inspect the current agent's workspace files, images, Git status, and unified
  diffs without exposing a write action.
- Read searchable native conversations for Claude Code, Codex, Qoder, Pi, and
  Oh My Pi in focused conversation or full-history form; review a retained
  24-hour activity summary and receive blocked or completion notifications.
- Require device verification before reconnecting relays.
- Detect Codex, Claude Code, OpenCode, Qoder CLI, Pi, Oh My Pi, and Kimi.

| Agents | Native Resize |
| --- | --- |
| <img src="../images/home.jpeg" alt="Mobile list of Herdr agents" width="392"> | <img src="../images/native_mobile_resolution.jpeg" alt="OMP terminal rendered at native mobile width" width="392"> |

| Plan Questions | Notifications |
| --- | --- |
| <img src="../images/agent_plan.jpeg" alt="Structured plan question navigation" width="392"> | <img src="../images/notifications.jpg" alt="Blocked-agent notification" width="392"> |

| Git Inspection | Native Conversations |
| --- | --- |
| <img src="../images/git-history.jpeg" alt="Read-only mobile Git diff with syntax-aware colors and zoom controls" width="392"> | <img src="../images/conversations.jpeg" alt="Mobile native conversation history rendered from the agent transcript" width="392"> |

## Workspace navigation and inspection

The home screen keeps agents that need input visible at the top. By default,
workspaces below them are separated into Done, Working, and Idle sections that
retain the workspace and tab hierarchy. The **Home Workspaces** setting can mix
them instead: each workspace appears once with a dot for its most notable
session — done, then working, then idle. On a phone, tap the magnifying-glass
button to search projects, workspaces, paths, tabs, sessions, agents, hosts,
and relays.
At 900 CSS pixels and wider, an agent rail keeps those workspace groups beside
the open terminal.

When the relay advertises tab ordering, press and hold an agent card until its
tab lifts, then drag to reorder the tab in Herdr; a plain tap still opens the
agent, and Alt+arrow keys on a focused card provide the same control. The
change is applied to the desktop immediately. Tab moves made on the desktop
arrive through the Herdr event stream and update the mobile order.

Opened workspace cards remain expanded after visiting an agent and returning to
the home screen.

**Inspect Workspace** is read-only and is available only when the connected
relay advertises workspace inspection and the agent reports a working
directory. The relay confines reads to that directory, skips symlinks and
common generated directories, and returns at most 4,000 tree entries. Text
previews are limited to 1 MiB and image previews to 5 MiB.

Git inspection disables hooks, pagers, text conversion, external diffs, lazy
fetches, and user/system Git configuration. Status is limited to 2,000 changed
files, individual unified diffs to 1 MiB, and Git commands to eight seconds.
The inspector has no save, stage, commit, or shell control.
On narrow screens, swipe the file or changed-file sidebar left to collapse it;
the adjacent sidebar button restores it. Unified diffs use theme-aware colors
for headers, hunks, additions, and deletions. Pinch the diff or use its zoom
controls to resize it without changing the rest of the app.

## Mobile terminal

The mobile terminal always uses **Resize Session**. While a terminal is open,
the relay leases the live PTY at the measured phone width so full-screen agents
redraw for the phone. The relay restores the previous width when the terminal
closes, the phone disconnects, the lease expires, or the relay shuts down.

Terminal History keeps 100, 500, or 1,000 lines in the terminal view; 1,000 is
the default and matches the most Herdr serves per pane read. The view renders
Herdr's window exactly as served: Herdr re-wraps its entire scrollback to the
current pty width on every read, so while the phone holds the lease the whole
visible history is already phone-shaped — nothing is stitched, reconciled, or
retained on the phone across width changes. The "older history" notice reports
when rows beyond the served window exist. Use **Copy** for the latest response
or **Conversation History** for clean, searchable earlier turns.

For supported agents, the terminal header opens **Conversation History** after
the agent reports a session. The relay reads that harness's local transcript,
associates bounded tool calls with their recorded results, and pages the newest
80 user or assistant messages at a time. **Conversation** keeps each user prompt
and the latest agent answer from that exchange. **Full history** shows every
recorded message with collapsible tool activity. Both use an escaped Markdown
subset, search filters the currently displayed view, and each message can copy
its original Markdown. Hidden reasoning, injected system records, and sidechain
turns remain excluded. Reads are confined to known session directories and the
newest 16 MiB of very large logs. When that bound omits older turns, they remain
in the harness log on the computer; restarting the relay neither caused the
bound nor removed those turns.

Terminal Refresh controls how often the relay checks a visible pane: 100 ms,
250 ms, 500 ms, or 1 second. The 250 ms default balances responsiveness with
computer and phone CPU use while output is changing.
Unchanged checks do not retransmit terminal history to the phone; the relay
sends terminal bytes only when the pane frame changes.

Returning to an unchanged Resize Session paints its cached rendered frame
immediately, then reacquires the lease and refreshes the preserved history and
clean current screen in the background.

**Find** searches every row loaded into the current terminal view, highlights
visible matches, and moves between matches even when the terminal has
virtualized them off-screen.

Explicit HTTP(S) URLs in terminal output become external links with opener and
referrer isolation. When the last terminal lines name supported key hints such
as arrows, Enter, Esc, Tab, Y/N, or a modifier chord, the app offers matching
one-tap actions through the same ordered key path. Detected actions can be
dismissed and never replace verified approval or structured-question controls.

The terminal controls send **Esc**, **Tab**, **Enter**, and arrow keys.
**Shift**, **Ctrl**, and **Alt** can be combined, remain armed for repeated
input, and apply to typed characters or any available terminal key. Sends are
ordered, and a live status confirms the exact chord. Toggle the modifiers off
or move focus to the composer to disarm them.
When an unclassified blocked pane needs inspection, the composer inserts
literal terminal text and sends **Enter** as one ordered action instead of
starting a new agent prompt.

**Copy** runs the agent's own copy command (Claude Code, Codex, Kimi, OMP, Pi,
and Qoder) to capture its latest
completed response without ANSI control sequences, falling back to the
visible terminal output for other agents such as OpenCode. Copy is disabled
while the agent is still working, so it can no longer interrupt an in-flight
turn.

# Changelog

Notable user-facing changes to Herdr Mobile Relay are documented here. The
project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.15.9] - 2026-08-15

### Added

- Separate **Done** home-screen section for workspaces with finished sessions,
  between the input queue and Working.
- **Home Workspaces** setting: **By State** (default) keeps Done, Working, and
  Idle workspace sections; **Mixed** shows each workspace once with a dot for
  its most notable session — done, then working, then idle. Agents needing
  input stay on top in both layouts.

## [0.15.8] - 2026-08-15

### Fixed

- Make the deployed-app reload journey wait for the reloaded document instead
  of a URL that already matches before the navigation commits, so WebKit
  release validation no longer fails intermittently.

## [0.15.7] - 2026-08-15

### Fixed

- Stop losing terminal rows in **Resize Session** when output streams faster
  than the refresh interval: resized reads now return the recent scrollback
  window instead of only the live screen, and rows are committed to history
  once they scroll out of the viewport.
- Keep the history above the live viewport intact when opening a resized
  terminal, instead of cutting a screenful of pre-resize lines that the
  phone-width screen does not cover.
- Keep agent status bars, input boxes, and duplicated redraws out of resized
  terminal history: display reads now return physical rows, the pre-resize
  screen is cut from the baseline by exact row count, and the redrawn
  transcript block an agent pushes into scrollback on a width change is
  skipped instead of committed.
- Close the timing hole that let a late resize re-render reach history: the
  relay now marks pane frames read within three seconds of an actual leased
  width change, and the app skips committing rows from marked frames instead
  of relying on a one-shot skip that the agent's redraw could outlive.
- Document that Herdr serves at most about 1,000 rows per pane read: the
  5,000 and 10,000 Terminal History limits preserve rows beyond that while
  the terminal stays open and output streams, not when a pane is opened.
- Honour scrolling up while output is streaming: content growth no longer
  re-pins the terminal to the bottom, only a viewport or controls height
  change does. Reaching the bottom re-engages the pinned mode.
- Keep the reading position fixed while new output arrives: when the loaded
  window drops its oldest rows, the scroll anchor follows the same row instead
  of its stale index.

## [0.15.6] - 2026-08-14

### Fixed

- Preserve the requested terminal history through **Resize Session** while
  replacing only the stale desktop viewport with the clean phone-width screen.

## [0.15.5] - 2026-08-14

### Fixed

- Show the clean current pane after resizing a loaded agent instead of mixing
  terminal redraws captured at incompatible widths.

## [0.15.4] - 2026-08-14

### Fixed

- Restore iOS push notifications by using a routable HTTPS VAPID contact
  subject accepted by Apple's push service.

## [0.15.3] - 2026-08-13

### Changed

- Keep working agents visible in a workspace-grouped home-screen section and
  rename the remaining home-screen group to **Idle**.
- Reorder Herdr tabs from the phone by pressing and holding an agent card,
  then dragging: tabs slide around the lifted card as it follows the finger,
  and the new arrangement stays put while Herdr confirms. A plain tap still
  opens the agent, and Alt+arrow keys offer the same control. Desktop tab
  moves mirror back to the mobile order, which follows Herdr's visual
  positions instead of stable tab numbers.
- Make home-screen text unselectable so a long press starts a tab drag
  instead of the platform text-selection and search sheets.

## [0.15.2] - 2026-08-13

### Fixed

- Preserve Codex free-text answers by opening its notes editor before typing and
  pacing terminal input so **Enter** cannot overtake the pasted text.
- Detect Codex follow-up questions immediately, including partially drawn
  question frames and the current separate `esc to interrupt` footer.
- Switch an active chat to structured question controls as soon as a question
  frame arrives, and restore the terminal position when the question clears.
- Refresh structured question controls even when the terminal bytes have stopped
  changing, so an open plan no longer requires leaving and reopening the agent.

## [0.15.1] - 2026-08-13

### Fixed

- Keep terminal output pinned to its latest row when the mobile viewport or
  terminal controls change height, including when a scroll event arrives before
  resize observation.
- Submit free-text input and **Enter** as one ordered action in unclassified
  blocked panes.
- Render OMP and Pi **Ask** dialogs, including final answer reviews, with the
  same structured mobile controls as other supported agents; keep final
  submissions safe across relay state updates.
- Keep structured question controls mounted while an answer advances to the next
  question, including across terminal redraws and pane-refresh acknowledgements.
- Classify the OMP plan-review action menu as a structured approval so the
  phone offers its actions as buttons instead of the raw terminal.
- Parse OMP **Ask** questions whose frame is partially scrolled on narrow
  panes: a hidden custom-answer row no longer forces the raw terminal, wrapped
  question tabs still yield the right progress, and the inner scrollbar column
  no longer leaks into option labels.
- Keep the confirmed option selected when revisiting a Claude question whose
  free-text row still holds an earlier typed note; the stale note no longer
  re-selects **Other** or blocks resubmission.
- Load the existing OMP custom-answer note when revisiting a question, and
  replace the note instead of appending when a new answer is typed.
- Anchor the jump-to-latest button to the terminal output area so it no longer
  overlaps the composer send button.
- Use the full window width for the terminal and question layout on large
  screens instead of centering it with side margins.
- Number each answered question in the final review summary, render it on its
  own line with the question in bold, keep wrapped Claude review prompts
  complete, and show the actual typed free-text answer in the review instead
  of a placeholder once the relay has seen it (submitted from the phone or
  observed on a revisited question).
- Constrain the terminal to the window on wide agent-rail layouts so its
  history scrolls again instead of growing past the screen.
- Enlarge the header and agent-rail icons for easier tapping on tablets.
- Use a single comfortable column for home cards and workspaces on wide
  screens instead of a cramped two-column grid.

## [0.15.0] - 2026-08-12

### Added

- Add workspace-first home cards, global agent search through the phone's
  magnifying-glass button, and a workspace agent rail on wider displays.
- Add read-only, symlink-confined workspace file, image, Git status, and unified
  diff inspection with bounded output, hardened Git execution, theme-aware diff
  colors, and per-diff pinch/button zoom.
- Add a focused user/final-answer conversation view plus full history with safe
  structured Markdown, per-message Markdown copy, and collapsible tool calls
  and results.
- Add isolated HTTP(S) links and conservative key-hint actions to terminal
  output.
- Add a retained 24-hour activity summary with observed working time,
  attention, completion, action, relay, and per-agent totals.
- Add private rotating JSONL attempt/result attribution for remote agent writes
  without storing prompt, response, or upload content.

### Changed

- Raise the guarded initial page payload ceiling from 96 KiB to 104 KiB for the
  expanded navigation and inspection UI, while moving push-only notification
  artwork out of the page bootstrap.

### Fixed

- Keep opened workspace cards expanded across agent navigation.
- Prevent non-agent pane metadata from briefly appearing as empty agent tabs.
- Let the mobile workspace sidebar collapse by swipe or button, and vertically
  center the Git branch label.
- Avoid repeating tab labels inside workspace agent tiles, and show relative
  activity ages only after the relay observes actual agent activity.
- Explain that the 16 MiB conversation read bound leaves older turns in the
  harness log and is unrelated to relay restarts.

## [0.14.11] - 2026-08-12

### Added

- Add durable per-pane prompt drafts with bounded browser storage and explicit
  fallback when persistence is unavailable.
- Add native, searchable user/assistant conversation history for Claude Code,
  Codex, Qoder, Pi, and Oh My Pi.
- Add literal find across loaded terminal history with match highlighting and
  next/previous navigation through virtualized rows.
- Add **Alt** and multi-modifier terminal chords, ordered key delivery, and
  visible chord confirmation.

### Changed

- Preserve completed-agent triage and recent ordering across relay restarts.

### Fixed

- Keep the mobile terminal navigation pad limited to arrow keys supported by
  Herdr 0.8.0 instead of offering Home, End, Page Up, and Page Down actions
  that fail.
- Keep uncertain prompt deliveries out of the composer and persistent draft
  store so reconnecting cannot invite an accidental duplicate send.

## [0.14.10] - 2026-08-12

### Added

- Add **Rename Session** to the agent menu for every harness except OpenCode. It
  sends `/rename new_session_name`.

### Changed

- Make **Resize Session** the only terminal-width behavior. Remove the
  **Fit to Phone** and **Original Columns** choices from the header and
  Settings.
- Label the existing agent rename action **Rename Tab**. Open either rename
  action in a dedicated form prefilled with the current name; unnamed sessions
  start blank.

### Fixed

- Keep error and status messages above modal backdrops so they remain fully
  visible while a dialog is open.
- Preserve the selected terminal position when **Resize Session** reacts to a
  phone-width change.
- Make **Rename Tab** call Herdr's tab-label operation directly instead of the
  restricted agent-name operation, allowing labels such as `123`.
- Prefill **Rename Session** from the current title on both current and older
  relays without exposing raw session paths or UUIDs.
- Let **Rename Session** submit natural titles with spaces and uppercase
  characters instead of being blocked by tab-name validation.
- Match Activity excerpts to the selected terminal text size and terminal font,
  including Nerd Font status symbols.

## [0.14.9] - 2026-08-10

### Fixed

- Keep the selected terminal history in **Resize Session** instead of showing
  only the roughly 46-row live viewport, and wrap stale desktop-width grids so
  their text remains readable on the phone.

## [0.14.8] - 2026-08-10

### Fixed

- Keep **Resize Session** output live and ordered during long responses by
  replacing only its current viewport while retaining pre-resize scrollback.

## [0.14.7] - 2026-08-10

### Fixed

- Stop checking the app origin every second after its deployment target is
  already loaded, keeping the **About** update status stable.

## [0.14.6] - 2026-08-10

### Fixed

- Keep **Resize Session** text stable across live refreshes by retaining phone
  scrollback and replacing only the clean, current terminal viewport.
- Wait for a hosted app deployment to converge before loading it, so an old
  cached app cannot consume and suppress the automatic update.

## [0.14.5] - 2026-08-10

### Fixed

- Preserve configured terminal scrollback depth in **Resize Session** while
  continuing to discard stale desktop-width rows.

## [0.14.4] - 2026-08-10

### Fixed

- Make **Load Update** replace a stale installed phone app reliably instead of
  retaining its previous document.

## [0.14.3] - 2026-08-10

### Fixed

- Continue pending relay updates automatically after loading a newly deployed
  phone app.

## [0.14.2] - 2026-08-10

### Fixed

- Keep **Resize Session** terminal text stable across refreshes instead of
  mixing stale desktop-width scrollback into the phone-width view.

## [0.14.1] - 2026-08-09

### Fixed

- Let stable setup create the first Cloudflare tunnel when `cloudflared`
  reports `null` for an account with no tunnels.

## [0.14.0] - 2026-08-07

### Changed

- Replace the **Shift+Tab** button with combinable **Shift** and **Ctrl**
  modifier keys: tap either (or both) to arm it, then type a letter or tap
  **Tab** to send the combined chord.
- Keep the terminal's modifier keyboard open across repeated key sends
  instead of closing it after every press; it now closes only when focus
  moves to the composer, terminal, or **Enter**/**Send**.

## [0.13.12] - 2026-08-04

### Added

- Stream agent topology updates from Herdr with a 15-second reconciliation
  backstop.
- Report when older terminal history is left out of a pane view, whether Herdr
  clipped the scrollback or the selected line limit did.

### Fixed

- Classify `server_not_running` command failures as safe-to-retry
  `not_started` results and show the actionable Herdr startup message.
- Keep a multi-step prompt or question answer whose earlier input already
  reached the agent marked as unsafe to retry, so a later `server_not_running`
  failure can no longer invite a duplicate send.
- Preserve unsafe prompt handling for older relays that report
  `dispatched_unknown` without an error payload.
- Keep honouring `HERDR_RELAY_POLL_INTERVAL` while the Herdr event stream is
  unavailable, instead of always falling back to the 15-second reconcile.
- Keep response copying from interrupting an agent's in-flight turn.

## [0.13.11] - 2026-08-03

### Fixed

- Keep the **Check for Updates** controls stable while app and relay checks are in flight.
- Make the cross-browser regression coverage independent of viewport scroll adjustments.

## [0.13.10] - 2026-08-03

### Fixed

- Keep the **Check for Updates** controls stable while app and relay checks are in flight.

## [0.13.9] - 2026-08-03

### Fixed

- Retry rate-limited GitHub release checks through public Atom feeds.
- Reload deployed phone-app bundles with a cache-busted navigation after sleep.
- Reduce the Markdown response preview font size on mobile.

## [0.13.8] - 2026-08-03

### Fixed

- Allow response copying when terminal-only updates keep the pane revision stable.
- Preserve pending agent prompts while running response-copy commands.

## [0.13.7] - 2026-08-03

### Fixed

- Prevent response-copy actions from interrupting active agent turns.
- Handle native copy menus, repeated uncounted confirmations, and empty
  clipboards without accepting stale responses.
- Keep relay response copying independent of slash-command catalog loading and
  correct OMP/Pi session-title resolution.

## [0.13.6] - 2026-08-01

### Fixed

- Copy the latest completed agent response when available, with visible
  terminal output preserved as a fallback.

## [0.13.5] - 2026-08-01

### Fixed

- Preserve app-deployment credentials and settings across detached workers,
  bound their process trees, and keep failures actionable.
- Recover stale relay update state after interrupted workers and terminate
  update subprocess trees without leaving inherited output pipes behind.
- Stop and reap installer `curl`/`wget` downloads on cancellation, including
  metadata downloads, before cleaning temporary files.
- Resolve renamed Qoder, Pi, and Oh My Pi sessions using their stored names.

## [0.13.4] - 2026-07-31

### Fixed

- Route app deployment owners running releases older than 0.13.3 through the
  one-time Terminal bootstrap before scheduling an update. Restored failures
  show the copyable recovery command instead of an update retry loop.


## [0.13.3] - 2026-07-31

### Fixed

- Preserve separately hosted app deployment settings when the managed updater
  hands off to its detached worker, so app-first relay updates no longer fail
  after their release bundle is verified.

## [0.13.2] - 2026-07-31

### Changed

- Replace competing app and per-relay update controls with one source-specific
  safe update action, and distinguish current phone-app status from available
  relay updates. The persistent progress screen publishes the phone app first
  when required, updates relays one at a time, survives reloads, and shows
  terminal errors with an explicit close action.

### Fixed

- Keep Cloudflare Pages deployments in progress while the public app origin
  converges on the verified bundle, instead of reporting a stale edge response
  as a failed deployment that succeeds when retried.

## [0.13.1] - 2026-07-31

### Changed

- From relays running this release onward, deploy and publicly verify a
  separately hosted Cloudflare Pages phone app before installing its
  deployment-owner relay update. Download, compatibility, or deployment
  failures leave the current relay running.

### Security

- Declare phone-app and relay transport capabilities in verified release
  manifests, and refuse upgrades that cannot preserve connectivity in both
  app-first and relay-first rollout windows without a bridge release. This
  release retains E2EE v1 for compatibility with the previous app during the
  upgrade into it.

## [0.13.0] - 2026-07-31

### Added

- Encrypt token-authenticated phone-to-relay WebSockets end to end with a
  key-authenticated ephemeral P-256 handshake, HKDF-SHA-256 session keys, and
  AES-256-GCM frames. Relay keys no longer travel in WebSocket URLs or HTTP
  headers, so Cloudflare Tunnel terminates TLS without receiving relay content.
- Show compact agent logos on the session list for Codex, Claude Code,
  OpenCode, Pi, Oh My Pi, Kimi, and Qoder, with an accessible fallback for
  custom agents.

### Changed

- Reduce the phone app's initial payload by removing unused Tailwind-generated
  CSS and its build integration.

### Security

- Require encrypted client key confirmation before relay registration, so a
  captured client hello cannot be replayed into a live relay connection.
- Reject configured relay keys shorter than 16 bytes and document the
  handshake's offline-guessing boundary.
- Specify the E2EE wire format byte for byte, validate shared deterministic
  Go/browser vectors, and fuzz malformed client hellos and encrypted envelopes.

## [0.12.0] - 2026-07-29

### Changed

- Move active terminal watching to the relay, offer 100 ms, 250 ms, 500 ms, and
  1-second refresh intervals with 250 ms as the default, and send
  fingerprint-acknowledged deltas without repeated full snapshots or terminal
  polling from the phone.
- Negotiate no-context WebSocket compression for messages over 512 bytes,
  reducing full terminal frames and other large relay updates.
- Keep complete terminal-history reads on the persistent Herdr socket instead of
  falling back to a new CLI process for every visible change.

### Fixed

- Keep the live terminal feed refreshing while the prompt input is focused.
- Keep acknowledged completions in the Idle section even while Herdr continues
  reporting an explicit completion status.

## [0.11.2] - 2026-07-28

### Fixed

- Keep the app's displayed upstream version from being downgraded by stale
  release metadata from an older relay.
- Check every connected self-updating relay automatically so installable relay
  updates appear without requiring a manual **Check App** action.

## [0.11.1] - 2026-07-28

### Changed

- Virtualize long terminal histories with measured row heights, bounded DOM
  windows, stable scroll anchors, and a complete copy/accessibility transcript
  so opening the phone keyboard no longer scales with the configured history.

## [0.11.0] - 2026-07-28

### Added

- Three terminal-width modes: **Fit to Phone**, **Original Columns**, and
  **Resize Session**.
- Safe pane-size leases that temporarily resize a live PTY to the measured
  mobile width and restore it on mode exit, disconnect, expiry, or shutdown.
- Bundled Nerd Font symbols for consistent terminal glyphs without a system
  font dependency.
- Default launch profiles for Pi, Oh My Pi, and Kimi.
- Slash-command suggestions for Pi, Oh My Pi, Kimi Code, and OpenCode built-in
  TUI commands.
- A mobile **Ctrl** modifier that opens the phone keyboard and submits the next
  letter as a terminal chord such as `Ctrl+C` or `Ctrl+O`.
- Mobile **Shift+Tab** and **Copy** controls for cycling agent modes and copying
  the visible terminal output.

### Changed

- Move in-app notifications below the header instead of covering terminal
  controls.
- Replaced agent-specific mobile terminal branches with one shared ANSI and
  fixed-grid rendering pipeline.
- Made 1,000 lines the default terminal history, with explicit 100, 5,000, and
  10,000-line choices in Settings.
- Made **Resize Session** the default terminal-width mode when no preference is
  saved.
- Reuse valid in-memory resized frames and rendered HTML when reopening a pane;
  lease renewal and fresh content reconciliation now run in the background.
- Defer offscreen terminal rows and skip repeated ANSI/HTML work for unchanged
  frames to reduce large-history rendering cost.

### Fixed

- Send mobile `Ctrl` letter chords without an unintended Shift modifier.
- Enforce the requested line count after Claude and Qoder history merging, not
  only before it.
- Refresh style-only ANSI rows and read Qoder's current physical screen so
  `/permissions` tab highlights follow arrow-key navigation.
- Submit Qoder prompts and slash commands from **Send** without requiring a
  separate **Enter** action.
- Suppress transient viewport-only snapshots while a resized terminal is still
  reflowing its scrollback.
- Keep long URLs, hashes, and other unbroken strings within responsive terminal
  layouts.
- Preserve box-drawing tables and fixed-grid rows instead of wrapping and
  distorting their cells.
- Release pane-size leases when their WebSocket owner disappears, preventing a
  laptop terminal from remaining narrowed.

[Unreleased]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.9...HEAD
[0.15.9]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.8...v0.15.9
[0.15.8]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.7...v0.15.8
[0.15.7]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.6...v0.15.7
[0.15.6]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.5...v0.15.6
[0.15.5]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.4...v0.15.5
[0.15.4]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.3...v0.15.4
[0.15.3]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.2...v0.15.3
[0.15.2]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.1...v0.15.2
[0.15.1]: https://github.com/0cv/herdr-mobile-relay/compare/v0.15.0...v0.15.1
[0.15.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.11...v0.15.0
[0.14.11]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.10...v0.14.11
[0.14.10]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.9...v0.14.10
[0.14.9]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.8...v0.14.9
[0.14.8]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.7...v0.14.8
[0.14.7]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.6...v0.14.7
[0.14.6]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.5...v0.14.6
[0.14.5]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.4...v0.14.5
[0.14.4]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.3...v0.14.4
[0.14.3]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.2...v0.14.3
[0.14.2]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.1...v0.14.2
[0.14.1]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.12...v0.14.0
[0.13.12]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.11...v0.13.12
[0.13.11]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.10...v0.13.11
[0.13.10]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.9...v0.13.10
[0.13.8]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.7...v0.13.8
[0.13.7]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.6...v0.13.7
[0.13.6]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.5...v0.13.6
[0.13.5]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.4...v0.13.5
[0.13.4]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.3...v0.13.4
[0.13.3]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.2...v0.13.3
[0.13.2]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.1...v0.13.2
[0.13.1]: https://github.com/0cv/herdr-mobile-relay/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.2...v0.12.0
[0.11.2]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.1...v0.11.2
[0.11.1]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.10.7...v0.11.0

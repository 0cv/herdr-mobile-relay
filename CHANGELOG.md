# Changelog

Notable user-facing changes to Herdr Mobile Relay are documented here. The
project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/0cv/herdr-mobile-relay/compare/v0.14.0...HEAD
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

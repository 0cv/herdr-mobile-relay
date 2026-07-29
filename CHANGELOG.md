# Changelog

Notable user-facing changes to Herdr Mobile Relay are documented here. The
project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.2...HEAD
[0.11.2]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.1...v0.11.2
[0.11.1]: https://github.com/0cv/herdr-mobile-relay/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/0cv/herdr-mobile-relay/compare/v0.10.7...v0.11.0

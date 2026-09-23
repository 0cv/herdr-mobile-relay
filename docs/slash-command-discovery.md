# Slash-command suggestions

The phone's slash-command palette is a bounded, best-effort catalog. Most
providers share a **2,000 custom command or skill-file budget** for each
provider discovery pass and return at most **4,096 entries**, including
builtins. The phone validates and retains the same 4,096-entry maximum. These
limits are shared in
[`contracts/fixtures/slash_command_limits.json`](../contracts/fixtures/slash_command_limits.json)
and are checked against the Go and TypeScript implementations.

Hermes intentionally has two independent 2,000-file passes: one for its
native project/profile skills and one for configured compatibility skill
folders. It can therefore inspect more than 2,000 custom files in one request,
while its final catalog and wire-size limits still apply. This preserves Hermes'
provider precedence and configured-folder behavior; it is not a promise of an
unbounded catalog.

## Cursor

Cursor needs its own provider rather than the generic skill path, for two
reasons. It names a skill's slash command after the **directory** holding
`SKILL.md`, not the frontmatter `name` field — a skill in
`~/.cursor/skills/pdf-forms` is `/pdf-forms` even when its frontmatter says
`name: pdf form toolkit`. And it has a built-in command table the generic path
cannot supply: without a provider, a cursor pane falls through to the generic
path and gets an empty palette, not even builtins.

The provider publishes the commands Cursor registers unconditionally and scans
personal skills from `~/.cursor/skills` and the shared `~/.agents/skills`. It
deliberately excludes `~/.cursor/skills-cursor` (reserved for Cursor's own
built-in skills and managed automatically — Cursor's own documentation tells
users never to create skills there), `~/.cursor/cloud-skills`,
`~/.cursor/plugins`, and the `~/.claude`, `~/.codex` and `~/.grok` trees, since
those either duplicate another agent's catalog in a cursor pane or expose
internal state that is not a user-authored skill.

Cursor gates several commands behind its debug flag; `/open-in-prompt-quality`
and the `/dev:*` entries are development-only. These are omitted, as are
`/detach`, `/goal`, `/max-mode`, `/usage`, and `/zen-mode`: their availability
depends on the pane's persistent session, feature flags, model catalog, or
runtime capabilities, which discovery cannot determine. Their names and aliases
are not reserved, so user-authored commands with those names remain discoverable.
Configured `agent-profiles.ini` skill folders remain available as the escape
hatch for pointing the palette somewhere outside the roots above.

Project commands and skills are scanned at the pane's working directory, not
inferred from its ancestors. Builtin names and aliases (such as `/new` for
`/clear`) take precedence over Markdown commands, which take precedence over
skills, with command names compared without regard to case. Markdown command
files are not filtered by `hidden` or `user-invocable` frontmatter;
`user-invocable` filtering applies only to skills: Cursor excludes boolean
`false` and trimmed, case-insensitive `"false"`, not `no`, `off`, or `0`.
Skill frontmatter accepts a leading UTF-8 BOM and explicit YAML language markers
such as `---yaml` and `--- yml`. Empty command and skill files are skipped.
Native command and skill files larger than Cursor's 1 MiB limit are skipped
before reading; reads are also bounded in case a file grows during discovery.

Native skill discovery reads `SKILL.md` at the root and through ten directory
levels, skipping `node_modules`, `__pycache__`, `dist`, and `build`, while
deduplicating linked skill files across roots. Project skill links must remain
inside their skill root. Personal directory links may point outside the root,
but only that directory's own skill is read, not its descendants. Individual
`SKILL.md` links must remain inside the skill root in both scopes. Skills whose
nonempty `metadata.surfaces` excludes `cli` are omitted. Command files may be
symlinks to regular files; pipes and sockets are never read. If a file or
directory budget interrupts a skill root's discovery, that root's skills are
omitted and the catalog is marked incomplete: assigning their command IDs
requires finding all duplicate folder names within the root.

A catalog can be incomplete when a discovery pass, the final entry cap, or
the serialized response-size guard is reached. One outbound relay message is
limited to 4 MiB; the relay clips a large serialized `command_result` before
sending it and sets `truncated: true`. The `truncated` flag remains
conservative: it is not an exact count of omitted eligible commands. The phone
also marks a catalog incomplete if an older or newer relay sends more than its
local cap.

Typing filters only the suggestions already loaded on the phone. Filtering
runs over the retained catalog before the UI applies a separate 200-row display
cap. If more matches remain, the palette says that more matching commands are
hidden and asks the user to keep typing; keyboard selection and ARIA indices
cover only the rendered rows. If a command is absent, send it manually; the
palette does not search the computer on every keystroke. Opening the palette
revalidates the catalog. **Refresh commands** requests another catalog without
changing or submitting the composer text. An open palette also refreshes after
reconnecting or changing the selected session. Same-session suggestions may
remain visible with a refreshing indicator; a session change clears them.

Cache identity includes the relay, server session, pane, terminal generation,
agent session, agent name, and working directory. This is not a filesystem watch:
reopen the palette or use its refresh button after changing command resources.

## Pi runtime commands

For Pi 0.87.0, install the relay-owned metadata integration into the selected
**agent directory** (not the directory containing it):

```bash
bash relay/setup.sh --pi-install "$HOME/.pi/agent"
# A custom profile:
bash relay/setup.sh --pi-install /absolute/path/to/profile/agent
```

With no directory argument, setup uses `PI_CODING_AGENT_DIR`, then
`~/.pi/agent`. Normal plugin installation does not install the optional Pi
integration. For an installed release that includes this feature, use the
installed release's `relay/setup.sh` rather than an unrelated checkout; the
published 0.21.3 relay cannot discover live Pi extension commands. For a
newer installed release, the default installation path is:

```bash
bash "${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay}/current/relay/setup.sh" \
  --pi-install "$HOME/.pi/agent"
```

`make dev-tunnel` installs or updates the integration automatically when the
selected Pi profile exists; set `HERDR_DEV_PI_COMMANDS_INSTALL=0` to skip it.
Run `/reload` in each affected Pi pane after installation or restart Pi.
Install the integration into every profile whose commands you want to discover.
After updating the relay bundle, rerun the install command to copy its matching
bridge version into each selected profile, then reload Pi. It lives
alongside Herdr's integration and does not edit settings, trust decisions, or
other extension files. To remove only its owned files:

```bash
bash relay/setup.sh --pi-remove /absolute/path/to/profile/agent
```

The relay queries `pi.getCommands()` in the selected running session, preserving
invocation suffixes, extension-before-skill-before-template invocation precedence,
descriptions, and canonical `sourceInfo` provenance. Credential-bearing Git
authorities (including protocol-less shorthand), query parameters, and fragments
are removed before bridge serialization. This also covers authentication copied
into package cache paths and base directories; the relay independently sanitizes
all three fields before returning them to devices. Sanitized provenance paths
are display metadata, not filesystem navigation targets. In each decoded path
component, any nonempty prefix through the last `@` is conservatively treated as
sensitive and removed, regardless of hostname shape. Leading `@scope` components
are preserved. This path policy does not strip Git refs or npm versions from the
separately sanitized source field. The relay merges these with its builtin TUI catalog.
Skills are included only when Pi's current autocomplete provider advertises
them: `getCommands()` alone includes skills even when skill suggestions are
disabled. A template using an active skill's invocation name cannot override
that skill's metadata. When the skill is hidden, its shadowed template is not
advertised as callable either. Prompt bodies and command handlers are never exported or executed.
Explicit command-format overrides and native-discovery suppression still take
precedence over runtime discovery.

The bridge starts only in an interactive TUI session inside Herdr, with
`HERDR_PANE_ID` and `HERDR_SOCKET_PATH` present. Endpoint identity includes the
Herdr socket's canonical path and inode, pane, native Pi session file path,
PID, and process incarnation. The relay independently checks Herdr's foreground process-group leader and
Unix peer credentials, and rechecks the process and session after discovery. Nested sessions cannot replace the root registration. Missing or
ambiguous identity is not guessed from a working directory.

Sockets are mode 0600 inside user-owned mode 0700 directories under
`/tmp/herdr-pi-<uid>-<instance hash>/`. Requests have bounded timeouts and
concurrency; bridge responses are capped at 2 MiB before the relay's 4 MiB
outbound guard. Autocomplete receives a request AbortSignal, cancelled on the
800 ms deadline, disconnect, or shutdown. Only one unsettled autocomplete call
is retained across requests and reloads. If an extension ignores cancellation,
subsequent queries still return current extension/prompt metadata as partial,
without launching more autocomplete work. Skill discovery resumes when that
call settles; restart Pi if it never does. Shutdown, reload, and replacement close owned sockets. A stale
socket is rejected rather than taken over. The integration requires no Node
packages beyond Pi's own runtime; it ships in the relay release archive.

### Runtime states and troubleshooting

- **Loading:** resource discovery has not finished rebuilding Pi autocomplete.
  Wait for Pi startup/reload, then refresh the palette.
- **Partial:** the bridge is missing, unavailable, mismatched, or some metadata
  could not be retained. Filesystem/builtin suggestions remain usable but may
  omit extensions and prompts. Check installation in the active profile, reload,
  and confirm the pane is running interactive Pi directly as its foreground job.
- **Unavailable:** the runtime reports that discovery is not usable. Manual
  command entry still works.
- **Truncated:** a size/file budget was reached; this is independent of runtime
  availability and is shown separately.

A catalog revision hashes the retained metadata and status. Reopening or
refreshing the palette obtains the current catalog without reconnecting.
If `/orches` has no suggestions, you may still type `/orchestrate` and explicitly
send it when that extension is loaded. Suggestions are not an execution
whitelist. Live physical-phone discovery and orchestration wizard
navigation/cancellation are separate acceptance checks and have not been
performed for this change.

For a repeatable runtime contract check against a local Pi installation, run:

```bash
PI_TEST_PACKAGE=/absolute/path/to/pi-coding-agent \
PI_TEST_ORCHESTRATION=/absolute/path/to/orchestration/src/index.ts \
bun tests/pi-runtime-contract.mjs
```

This uses temporary HOME/profile/project roots and does not send prompts or
invoke orchestration handlers. The optional orchestration path verifies its
registrations. To exercise authenticated shorthand with synthetic markers using
Pi's actual parser, resource loader, and user/project/temporary package paths:

```bash
PI_TEST_PACKAGE=/absolute/path/to/pi-coding-agent \
bun tests/pi-provenance-contract.mjs
```

This offline check makes no package downloads or installations. It verifies
redaction in bridge socket responses and runs the Go relay-boundary regression
against the same Pi-derived metadata. Go dependencies must already be cached. `make check` includes fixture-backed bridge/socket, profile
installation/removal, protocol, browser, and release-packaging checks without
requiring a globally installed Pi package.

The relay and phone bundle should be updated together so the larger catalog and
its guidance are available on the device. Release the bundled phone assets with
`make web-release`; do not edit `web/` by hand.

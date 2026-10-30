# Slash-command suggestions

The phone's slash-command palette is a bounded, best-effort catalog. Most
providers share a **2,000 custom command or skill-file budget** for each
provider discovery pass and return at most **4,096 entries**, including
builtins. The phone validates and retains the same 4,096-entry maximum. These
limits are shared in
[`contracts/fixtures/slash_command_limits.json`](../contracts/fixtures/slash_command_limits.json)
and are checked against the Go and TypeScript implementations.

Command and skill files larger than 1 MiB are skipped without marking the
catalog truncated; reads stop at that size. Cursor applies its own 1 MiB limit,
described below.

Hermes intentionally has two independent 2,000-file passes: one for its
native project/profile skills and one for configured compatibility skill
folders. It can therefore inspect more than 2,000 custom files in one request,
while its final catalog and wire-size limits still apply. This preserves Hermes'
provider precedence and configured-folder behavior; it is not a promise of an
unbounded catalog.

| Provider | Catalog source |
| --- | --- |
| Claude Code, Qoder | Builtins plus native command and skill files |
| Cursor Agent | Builtins plus project commands and native/configured skills |
| Codex | Builtins plus discovered skills |
| Pi | Builtins and filesystem fallback; optional exact-session runtime catalog |
| Oh My Pi | Builtins plus selected profile/project resources |
| OpenCode | 17 builtin suggestions plus bounded native Markdown commands; skills use native `@` mentions |
| Hermes, Kimi | Builtins plus native/configured skills |
| Antigravity CLI | 1.4.7 builtins and aliases plus `~/.gemini/skills` and configured skill dirs |

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

## OpenCode

The filesystem catalog follows the installed OpenCode **2.0.29** Markdown
command loader: personal `${XDG_CONFIG_HOME:-~/.config}/opencode/{command,commands}`
and project `.opencode/{command,commands}`. Project discovery walks bounded
ancestors through the Git root (or filesystem root outside a repository), with
nearer projects overriding personal and outer project commands. Within a scope,
`commands` overrides `command`. Builtin suggestions retain precedence.
Nested filenames keep their native slash separator: `commands/git/review.md`
is `/git/review`, not `/git:review`. Command frontmatter `hidden` and
`user-invocable` are not native OpenCode command filters.

Only command names and descriptions are exported; prompt bodies, shell
interpolation and agent/model directives are never executed. The shared
2,000-file budget and 1 MiB file limit apply, with at most 4,096 visited
filesystem entries and 32 directory levels per tree. Symlink entries and special
files are skipped; a linked root itself may be selected. Budget/depth loss sets
`truncated`; oversized files are skipped as for other providers. Names are
restricted to the palette's 120-character safe path format.

OpenCode 2.0.29 skills are **`@id` mentions**, selected by its native `/skills`
menu, not `/id` commands. They are deliberately not invented as slash-command
suggestions here. Use Terminal for that native menu. This bounded filesystem
catalog does not evaluate `opencode.json`/JSONC command definitions, remote or
system-managed configuration, runtime plugins, policies, or configured skill
sources; it is not a complete native-runtime catalog.

## Pi runtime commands

Filesystem discovery follows Pi’s trust-store lookup: canonicalize the working
directory, then look up its exact path and each canonical ancestor in
`trust.json`. Stored keys are not rewritten through symlinks. A noncanonical
alias cannot override a canonical decision or independently grant project
trust; null entries defer to ancestors and then `defaultProjectTrust`.

For Pi, turn on **Settings → Pi Integration** for its computer. This
controller-only setting installs the relay-owned integration into all Pi agent
directories known to that relay and reports each profile's result. Readers see
it read-only. It starts off and never edits Pi settings, trust decisions,
Herdr's integration or other extensions. It supports live command discovery,
text/tool progress, extension notices and the exact-session unconfirmed reply
path for blocked Pi panes.

The CLI remains an alternative for a selected **agent directory** (not the
directory containing it):

```bash
bash relay/setup.sh --pi-install "$HOME/.pi/agent"
# A custom profile:
bash relay/setup.sh --pi-install /absolute/path/to/profile/agent
```

With no directory argument, setup uses `PI_CODING_AGENT_DIR`, then
`~/.pi/agent`. App and CLI state agree because ownership is read from disk.
At relay startup, including verified-update activation, installed marker-owned
copies are refreshed from the relay bundle. Unowned directories and symlinked
extension directories/files are refused. Files are mode 0600 in a mode 0700
integration directory.

Run `/reload` in each affected Pi pane or restart Pi to activate changes. The
relay never reloads Pi or types into a pane automatically. The setting shows a
generic reload hint; it does not claim that a running Pi has loaded new files.
Turning it off, or this CLI alternative, removes only marker-verified owned files:

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
Herdr socket's canonical path and inode, pane, exact Pi session, PID, and process
incarnation. A native session reference may name Pi's current session ID or its
current session file; the bridge matches the actual session manager value and
rejects unrelated IDs or paths. The relay independently checks Herdr's whole foreground
process group and Unix peer credentials, and rechecks the process and session
after discovery. A shell foreground, missing shell identity, or any additional
foreground process (including an inherited-stdio editor) makes discovery
unavailable; merely finding the group leader is insufficient. Nested sessions cannot replace the root registration. Missing or
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

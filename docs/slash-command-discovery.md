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
and the `/dev:*` entries are development-only. Both groups are omitted:
publishing them would offer the phone commands the pane does not have. Configured
`agent-profiles.ini` skill folders remain available as the escape hatch for
pointing the palette somewhere outside the roots above.

Project commands and skills are scanned at the pane's working directory, not
inferred from its ancestors. Builtin names and aliases (such as `/new` for
`/clear`) take precedence over Markdown commands, which take precedence over
skills, with command names compared without regard to case. Markdown command
files are not filtered by `hidden` or `user-invocable` frontmatter;
`user-invocable` filtering applies only to skills. Empty command and skill files
are skipped. Native command and skill files larger than Cursor's 1 MiB limit
are skipped before reading; reads are also bounded in case a file grows during
discovery.

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
palette does not search the computer again. Suggestions are cached for the pane
and agent/worktree identity and are invalidated when the relay reconnects.
Reopening a terminal is not a filesystem watch or an explicit refresh.

The relay and phone bundle should be updated together so the larger catalog and
its guidance are available on the device. Release the bundled phone assets with
`make web-release`; do not edit `web/` by hand.

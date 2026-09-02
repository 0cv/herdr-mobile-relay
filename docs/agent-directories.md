# Agents with a non-default config directory

How the relay finds an agent's sessions, transcripts, slash commands, and
skills when that agent does not use its default configuration root. Read this
if the conversation view says "No conversation log is available for this
session" or a profile's commands are missing from the palette.

The relay runs as a background launchd/systemd user service, so it never
inherits the shell environment a Herdr pane runs in. If a pane sets
`CLAUDE_CONFIG_DIR`, `PI_CODING_AGENT_DIR`, or similar to use a non-default
profile — one per herdr setup — the pane keeps whatever title herdr itself
reports, but the relay-resolved session name and the transcript both come up
empty, and the conversation view shows "No conversation log is available for
this session." For Pi and Oh My Pi the same lists below also decide where that
profile's slash commands and skills are discovered, so a profile whose commands
are missing from the palette has the same root cause. Claude Code and Qoder
resolve their personal commands and skills from `~/.claude` and `~/.qoder`
directly, so those lists do not move command discovery for them.
Native palette discovery follows the verified loader behavior of specific agent
versions on a best-effort basis; newer agent releases can change edge-case
discovery semantics before the relay catches up.

Standalone Kimi Code palettes follow its native roots: project
`.kimi-code/skills` and `.agents/skills`, user
`$KIMI_CODE_HOME/skills` (default `~/.kimi-code/skills`) and
`~/.agents/skills`, then `extra_skill_dirs` from
`$KIMI_CODE_HOME/config.toml`. `KIMI_CODE_HOME` relocates only Kimi's own
configuration and skills; the shared `~/.agents/skills` root stays under the
user home.

Pi and Oh My Pi need no configuration for their named profiles as long as the
config root stays at its default, `~/.pi` or `~/.omp`: both keep a profile's
sessions at `<config root>/profiles/<name>/agent`, so the relay discovers
`~/.omp/profiles/*/agent` and `~/.pi/profiles/*/agent` during lookups rather
than only at startup. A profile created while the relay is running is picked up
after the discovery cache refreshes, with no restart. Transcript-location hits
and session titles can remain cached for up to 60 seconds; location misses are
retried after 5 seconds.
Transcript content is read fresh from the selected location. Auto-discovery
only ever expands the home config
root, never a configured one — so a relocated Pi or Oh My Pi config root does
not get its profiles discovered, even after its `<root>/agent` is added to the
matching `HERDR_*_CONFIG_DIRS` list. Each profile under a relocated root must
then be listed individually, as `<root>/profiles/<name>/agent`. For example,
with the whole root moved to `/data/omp` and one profile named `work`:

```bash
HERDR_OMP_CONFIG_DIRS=/data/omp/agent:/data/omp/profiles/work/agent
```

Every other case needs to be named explicitly:

| Variable | Home default it adds to | Agent's own directory variable |
| --- | --- | --- |
| `HERDR_CLAUDE_CONFIG_DIRS` | `~/.claude` | `CLAUDE_CONFIG_DIR` |
| `HERDR_QODER_CONFIG_DIRS` | `~/.qoder` | none |
| `HERDR_CODEX_CONFIG_DIRS` | `~/.codex` | `CODEX_HOME` |
| `HERDR_PI_CONFIG_DIRS` | `~/.pi/agent` | `PI_CODING_AGENT_DIR` |
| `HERDR_OMP_CONFIG_DIRS` | `~/.omp/agent` | `PI_CODING_AGENT_DIR` (Oh My Pi is a Pi fork and shares Pi's default) |
| `HERDR_OPENCODE_DATA_DIRS` | `${XDG_DATA_HOME:-~/.local/share}/opencode` | none |
| `HERDR_OMO_CONFIG_DIRS` | `~/.senpi/agent` plus supported `~/.omo` layouts | `OMO_CODING_AGENT_DIR`, `SENPI_CODING_AGENT_DIR`, `PI_CODING_AGENT_DIR` |

Three things to get right:

- The home default is always searched. Setting a list *adds* profiles; it
  never replaces the default.
- Entries are directories, not pre-joined `projects`/`sessions` paths — the
  relay appends the right leaf itself. Use the same value you'd put in that
  row's own "Agent's own directory variable": a full config directory for
  Claude, Qoder, and Codex, but already the *agent* directory for Pi and Oh My
  Pi, since that is what `PI_CODING_AGENT_DIR` takes.
- What you configure is searched before what the relay discovered, and the home
  default is searched last.

Separate multiple entries with a colon — the platform path-list separator, as
in `PATH` — so a directory whose name contains a literal colon can't be
listed this way.

Set these in the file named by `HERDR_RELAY_ENV`:
`$HERDR_PLUGIN_CONFIG_DIR/relay.env` for an installation, `relay/.env` for a
checkout. For example, with two herdr setups using `~/agents/claude-work` and
`~/agents/claude-personal` as their Claude profiles, add:

```bash
HERDR_CLAUDE_CONFIG_DIRS="~/agents/claude-work:~/agents/claude-personal"
```

The service wrapper sources that file with `set -a`, so every key in it
becomes part of the relay's environment — quote a path that contains spaces,
since the file is parsed by bash. A leading `~` or `~/` in an entry is
expanded against your home directory by the relay itself, not by bash, so it
survives quoting; whatever remains must still be an absolute path, or the
entry is silently dropped. After editing, restart the relay: reopen the setup
menu with the `herdr plugin pane open` command in [README.md](../README.md),
then choose your connection option again. An already-installed background
service is restarted rather than started twice.

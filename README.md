# Herdr Mobile Relay

[![check](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml/badge.svg)](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml)

Control [Herdr](https://herdr.dev) agents from your phone. Each Linux or macOS
computer runs its own relay; the phone connects to them and merges every agent
into one installable web app.

**Current version:** [`0.17.5`](https://github.com/0cv/herdr-mobile-relay/releases/tag/v0.17.5) · [Changelog](CHANGELOG.md)

> [!IMPORTANT]
> Native Windows is not supported. WSL2 may work but is not tested.

## Get started in two minutes

Requirements: Herdr 0.7.5 or newer, Git, and `curl`.

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

The setup menu opens automatically after installation.

To reopen the main setup menu later:

```bash
herdr plugin pane open \
  --plugin herdr-mobile-relay.events \
  --entrypoint setup \
  --placement zoomed \
  --focus
```

Start with **Community WebRTC Gateway**. It is the recommended path: as fast
to set up as the temporary tunnel, with stable relay connectivity and no
Cloudflare account, domain, `cloudflared`, or tunnel configuration. If prompted,
choose the installed Herdr app that should host the phone UI. The relay starts
and prints a QR code.

**Temporary Cloudflare Tunnel** is the fastest getting-started option for a
one-computer trial. It installs any missing user-level tools with confirmation,
starts the relay and bundled app, and prints a QR code.

Scan the QR with your phone. Keep the pane open; Ctrl-C stops the relay.

Neither quick-start path needs `sudo` or a Python, Node.js, or Go toolchain.
Treat the QR and its setup link as secrets: they carry the relay key.

[QUICKSTART.md](QUICKSTART.md) has pairing detail and troubleshooting for both
paths.

## What you get

| | |
| --- | --- |
| <img src="images/home.jpeg" alt="Mobile list of Herdr agents" width="392"> | <img src="images/agent_plan.jpeg" alt="Structured plan question navigation" width="392"> |

- Monitor and control agents across several computers, grouped by status and
  workspace, with agents that need input pinned on top.
- Start, rename, clear, and stop agents; send prompts, terminal keys, slash
  commands, screenshots, and photos.
- Answer verified approvals and structured plan questions from Codex, Claude
  Code, Qoder, OpenCode, Oh My Pi, and Pi.
- Read searchable native conversation history, and inspect workspace files,
  images, and Git diffs read-only.
- Receive blocked-agent notifications, with completion notifications optional.

**[Full feature tour →](docs/mobile-app.md)**

## Mobile onboarding

https://github.com/user-attachments/assets/e52c4fd0-ef77-4852-bb43-078a7154eae8

The walkthrough follows setup from scanning the QR through the agent list,
terminal controls, and notification settings.

## Choosing how your phone connects

The setup menu exposes each complete connection path directly:

| Choice | Needs | Best for |
| --- | --- | --- |
| Community gateway | no account, domain, or tunnel configuration; an installed app origin | the recommended stable, no-configuration relay path |
| Cloudflare tunnel | nothing for a temporary URL; a Cloudflare account and domain for a permanent hostname | the fastest one-computer trial or a permanent background service |
| Your own gateway | a small VPS | dedicated bandwidth and control of the transport logs |

All three are end-to-end encrypted. On either gateway the phone and the computer
then negotiate a direct peer-to-peer connection, leaving the gateway with the
fallback; Cloudflare tunnel traffic stays on Cloudflare.

- **[Transports explained →](docs/transports.md)**
- **[Permanent Cloudflare tunnel →](docs/cloudflare-tunnel.md)**
- **[Run your own gateway →](docs/gateway-self-hosting.md)**

## Agents with a non-default config directory

The relay runs as a background launchd/systemd user service, so it never
inherits the shell environment a Herdr pane runs in. If a pane sets
`CLAUDE_CONFIG_DIR`, `PI_CODING_AGENT_DIR`, or similar to use a non-default
profile — one per herdr setup — the pane keeps whatever title herdr itself
reports, but the relay-resolved session name and the transcript both come up
empty, and the conversation view shows "No conversation log is available for
this session."

Pi and Oh My Pi need no configuration for their named profiles as long as the
config root stays at its default, `~/.pi` or `~/.omp`: both keep a profile's
sessions at `<config root>/profiles/<name>/agent`, so the relay discovers
`~/.omp/profiles/*/agent` and `~/.pi/profiles/*/agent` on startup. Restart the
relay after adding a profile. Auto-discovery only ever expands the home config
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
menu with the `herdr plugin pane open` command under **Get started in two
minutes** above, then choose your connection option again. An
already-installed background service is restarted rather than started twice.

## Documentation

| Page | What is in it |
| --- | --- |
| [QUICKSTART.md](QUICKSTART.md) | The fast path, start to paired phone |
| [docs/mobile-app.md](docs/mobile-app.md) | Every feature: agent list, workspace inspection, mobile terminal |
| [docs/transports.md](docs/transports.md) | Cloudflare, community gateway, own gateway, direct WebRTC |
| [docs/cloudflare-tunnel.md](docs/cloudflare-tunnel.md) | The stable tunnel wizard, DNS, and teardown |
| [docs/gateway-self-hosting.md](docs/gateway-self-hosting.md) | Deploying and operating a gateway |
| [docs/updates.md](docs/updates.md) | Verified releases, phone-driven upgrades, Herdr compatibility |
| [docs/security.md](docs/security.md) | What is encrypted, what an intermediary sees, the audit log |
| [docs/development.md](docs/development.md) | Building, testing, and contributing |

## Security in one paragraph

Prompts, terminal output, uploads, and push details are encrypted end to end
between the phone and the relay. Whatever carries the traffic — a Cloudflare
tunnel or a gateway — observes connection metadata only, never plaintext; on the
direct path no application data reaches it at all, though a gateway still
answers address discovery. The relay exposes no write action to the workspace
inspector, and the app can require device verification before it reconnects.
[Details →](docs/security.md)

## License

[GNU Affero General Public License v3.0 or later](LICENSE).

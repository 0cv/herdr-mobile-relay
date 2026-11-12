# A second relay for herdr-core

Set `HERDR_RELAY_BACKEND=hc` to connect to herdr-core, the backend used by
herdr-app, instead of the Herdr TUI. Each relay process has one backend. Run two
relays to use both: the phone shows `<hostname>` and `<hostname> (core)` as two
computers. Install and pair each instance separately, with the setup menu or the
commands below.

## Available on core

- Agent list and status, and terminal screen reading.
- Prompts, including while an agent is waiting for an answer.
- Key presses, text without Enter, and typed question answers. Typed text must
  be single-line and contain no control characters.
- Secret input to a verified no-echo prompt, without input-side persistence.
  Programs that echo or log a secret can still expose it in terminal output.
- Start agents with **+**, and stop, clear or restart agents. Clear and Restart
  run the agent's recorded launch profile, or else the profile matching its
  name, so options from the original command line are not kept.
  Clear and Restart start a replacement in the same tab, then kill the old terminal
  after waiting up to five seconds for herdr.app to adopt the replacement. If adoption
  is not confirmed in time, the old terminal is still killed and the tab may come back unnamed.
  New tabs receive the requested name if herdr.app adopts them within a minute;
  otherwise the agent still starts, with an unnamed tab.
  Initial prompts wait for reported idle status; otherwise launch completes with
  a warning and leaves the prompt unsent.
- Rename and reorder tabs and workspaces in herdr-app. These actions can be
  refused when the desktop changed the layout at the same moment; try again.
- Pane size leasing (Resize Session). The phone's size applies to the shared
  core terminal, so herdr-app shows the phone's layout while a device leases,
  as with Resize Session on the TUI. The desktop can take the size back by
  resizing its window. Releasing the lease restores the previous viewer's size.
- Pi slash commands, activity, and the Conversation view (history and live)
  when Pi runs in a core terminal with the Pi bridge installed. Pi's session is
  learned from its bridge, so it needs the bridge from this release. The TUI
  relay owns the shared Pi extension.

The phone hides actions that core does not support: agent identity renaming;
tab creation and close; creating and closing workspaces, and worktrees; the Pi
integration switch; phone-driven relay updates and app deploys. Core upgrades
are manual.

## Conversation on core

| Agent | How its session is found | What the user does |
| --- | --- | --- |
| Pi | Pi bridge; used when no label session applies. | Install this release's bridge through the TUI relay, which owns the shared extension; reload Pi. |
| Claude Code | `SessionStart` hook in `~/.claude/settings.json`. | Install hooks and restart. |
| Codex | `SessionStart` hook in `~/.codex/hooks.json`, after the first prompt of each start or resume. | Install hooks, enable `[features] hooks = true` (also enabled by Herdr), and trust the hook in `/hooks`. Phone-started Codex needs nothing extra: the core relay adds `--no-daemon`. For desktop starts, use `codex --no-daemon`. |
| Cursor | Flat `sessionStart` hook in `~/.cursor/hooks.json`. | Install hooks and restart cursor-agent. Appears only once Herdr recognises it; it sends no OSC 7501 status. |
| Omp | Extension in `~/.omp/agent/extensions/herdr-mobile-relay-session.ts` reports starts and switches. | Install hooks and restart. |
| OpenCode | Plugin in `~/.config/opencode/herdr-mobile-relay-session/tui.js`, registered alongside Herdr's plugin in `cli.json`, follows the displayed session to its root parent. | Convert `cli.json` to strict JSON (no comments or trailing commas) before install or uninstall; install hooks and restart. |
| Qoder | `SessionStart` hook in `~/.qoder/settings.json`; subagent events ignored. | Install hooks and restart qodercli. |
| agy (Antigravity CLI) | Named `herdr-mobile-relay` hook in `~/.gemini/config/hooks.json` reads `conversationId`. | Install hooks, restart agy and start a new conversation; `SessionStart` fires for new conversations only. |
| Hermes | `runtime/active_sessions.json` under configured Hermes roots; exactly one CLI entry matching foreground PID and process creation time. | No hook. Appears only once Herdr recognises it; it sends no OSC 7501 status. |
| Grok | `~/.grok/active_sessions.json`; unique foreground PID and process start-time match (within 1 ms). | No hook for grok-build. |
| Kimi | Native journals, working directory and foreground process, as on the TUI. | No hook for kimi-code; subscription errors cannot produce a conversation. |

Run `herdr-mobile-relay agent-hooks install`, `status` or `uninstall` to manage
the relay's hooks. Other hooks are preserved; missing config directories are
skipped. Hooks do nothing outside core terminals: they use `HC_TERMINAL_ID`
and `HC_TERMINAL_SOCKET`, verify foreground PID ancestry, and write
`herdr-mobile-relay.session` and `herdr-mobile-relay.pid` together. Labels
apply only while that PID stays foreground; missing, stale or ambiguous
session evidence leaves Conversation unavailable.

Codex shows "1 hook needs review before it can run" until approved in `/hooks`.
Approval lives under `[hooks.state]` in `~/.codex/config.toml`; the installer
leaves that file and trust unchanged. Use `codex --no-daemon` in herdr.app:
the default shared background server runs hooks outside the terminal.
To apply this only inside Herdr (core terminals and TUI panes), add to `~/.zshrc`:

```zsh
if [[ -n "$HC_TERMINAL_ID" || -n "$HERDR_PANE_ID" ]]; then
  codex() { command codex --no-daemon "$@"; }
fi
```

## Configuration and isolation

Core passes the environment of whoever started it to every terminal it runs.
If started from a Herdr TUI pane, every core terminal inherits that pane's
`HERDR_PANE_ID`, `HERDR_SOCKET_PATH`, `HERDR_TAB_ID` and `HERDR_WORKSPACE_ID`,
which are stale and meaningless there.
Inside a core terminal, the identity is `HC_TERMINAL_ID` and
`HC_TERMINAL_SOCKET`; whenever either is present, the Pi bridge uses those and
never falls back to the `HERDR_*` values, so Pi registers only with the core relay.

`HERDR_RELAY_BACKEND` defaults to `herdr`; its other value is `hc`. On core:

| Setting | Default |
| --- | --- |
| Socket | `HC_SOCKET_PATH`, then `HC_SOCKET`, then `$XDG_RUNTIME_DIR/herdr-core/<session>.sock`, then `/run/user/<uid>/herdr-core/<session>.sock` if `/run/user/<uid>` is a directory, otherwise `<state home>/herdr-core/<session>/<session>.sock` |
| Session | `HC_SESSION`, otherwise `default` |
| State home | `XDG_STATE_HOME`, otherwise `~/.local/state` |
| HTTP port | `HERDR_RELAY_PORT=8377` |
| Plugin UDP port | `HERDR_RELAY_PLUGIN_PORT=8378` |
| macOS service name | `com.herdr-mobile-relay-hc.service` |
| Linux service name | `herdr-mobile-relay-hc.service` |
| Runtime directory | `$XDG_CONFIG_HOME/herdr-mobile-relay-hc` (normally `~/.config/herdr-mobile-relay-hc`) |
| Cache directory | `$XDG_CACHE_HOME/herdr-mobile-relay-hc` (normally `~/.cache/herdr-mobile-relay-hc`) |

The relay does not read the core's `socket` setting in `config.toml`.

`HERDR_SOCKET_PATH` is ignored on core. Explicit port values win.
If `HERDR_RELAY_ENV` is set, its containing
directory becomes the runtime directory, regardless of `HERDR_PLUGIN_CONFIG_DIR`.

Do **not** share these with the TUI instance:

- Runtime directory or env file, including `relay.pid` and `device-auth/`.
- Relay key (`HERDR_RELAY_TOKEN`), instance ID or paired-device credentials.
- Listening ports (including any explicitly fixed WebRTC UDP port).
- Service name/label and service definition.

The installed release and phone-app origin can be shared. Pair each relay
separately in the same phone app.

## Install and pair the second relay

Use the same installed release as the TUI relay. Start herdr-app or run
`hc server start` yourself first: setup requires a reachable core and never
starts or stops it; relay inventory readiness checks connectivity.
For a non-default core, set `HC_SESSION`, `HC_SOCKET` or `HC_SOCKET_PATH`
on the initial setup command; default paths are resolved in the service environment.

### With the setup menu

Open the menu from the Herdr plugin (**Herdr Mobile Relay: Setup**), or from the
installed release, which also works on a computer with herdr-app and no `herdr`
command (`install.sh` prints this command when it finishes):

```bash
bash "${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay}/current/relay/plugin-setup-menu.sh"
```

The menu shows the status of each relay it finds: **Herdr TUI relay** (the phone
shows `<host>`) and **Herdr app relay** (the phone shows `<host> (core)`). When
both exist it first asks what the phone should control:

- **Herdr TUI**: the usual menu.
- **Herdr app (core)**: Cloudflare (temporary tunnel, stable tunnel, change
  hostname, remove) and Tailscale on port 8444. No WebRTC and no app deployment.
- **Both**: Cloudflare stable tunnel or Always on Tailscale, run for the TUI
  relay and then for the core relay with one transport for both, each followed
  by its own QR. Temporary tunnels run in the foreground, so they are not offered.

Each QR says which computer it adds. Scan both in the same phone app. **b** goes
back to the first question.

### With commands

Every command below selects core with `HERDR_RELAY_BACKEND=hc`. Its private env
is `${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay-hc/relay.env`, not the
checkout or plugin env. Setup creates a new key and instance ID and persists
the backend, ports and explicitly set `HC_SOCKET_PATH`, `HC_SOCKET` and `HC_SESSION` for the background service. Never copy the TUI
env, tunnel config or device state.

### From a checkout

For stable Cloudflare, choose an unused hostname in your Cloudflare zone:

```bash
HERDR_RELAY_BACKEND=hc make stable-setup
HERDR_RELAY_BACKEND=hc make service-status
HERDR_RELAY_BACKEND=hc make setup-link APP_URL=https://YOUR-EXISTING-PHONE-APP
```

The wizard proposes a tunnel and DNS slug ending in `-hc`, writes its own
Cloudflare config beside the core env and installs the background service.
It never falls back to the TUI Cloudflare config.

Alternatively, use Tailscale on the computer and phone:

```bash
HERDR_RELAY_BACKEND=hc bash relay/tailscale-setup.sh
HERDR_RELAY_BACKEND=hc make setup-link APP_URL=https://YOUR-EXISTING-PHONE-APP
```

Tailscale setup uses HTTPS port **8444**, leaving the TUI's 8443 alone.
`HERDR_TAILSCALE_PORT` overrides that default. Turn it off before switching
transports:

```bash
HERDR_RELAY_BACKEND=hc bash relay/tailscale-off.sh
```

For a temporary Cloudflare tunnel instead of a background service:

```bash
HERDR_RELAY_BACKEND=hc make quick-start
```

### From an installed release

No checkout or Makefile is needed. Adjust the shared root if you installed it
elsewhere:

```bash
RELEASE="${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay}/current"

# Choose one transport:
HERDR_RELAY_BACKEND=hc bash "$RELEASE/relay/stable-setup.sh"
# Or:
HERDR_RELAY_BACKEND=hc bash "$RELEASE/relay/tailscale-setup.sh"

HERDR_RELAY_BACKEND=hc bash "$RELEASE/relay/service.sh" status
HERDR_RELAY_BACKEND=hc HERDR_PHONE_APP_URL=https://YOUR-EXISTING-PHONE-APP \
  bash "$RELEASE/relay/setup-link.sh"
```

For a temporary tunnel, run `setup.sh --install-missing` then `start.sh` under
`$RELEASE/relay/`, prefixing both with `HERDR_RELAY_BACKEND=hc`.

Scan the QR in the same phone app as the TUI relay. Each invitation pairs one
device within ten minutes; rerun `setup-link.sh` or `make setup-link` for another
phone. The core computer's label gets " (core)" automatically.

### Manage or remove core

| Action | Checkout | Installed release (`$RELEASE/relay/`) |
| --- | --- | --- |
| Install/reinstall and restart service | `make service-install` | `bash "$RELEASE/relay/service.sh" install` |
| Status | `make service-status` | `bash "$RELEASE/relay/service.sh" status` |
| Logs | `make service-logs` | `bash "$RELEASE/relay/service.sh" logs` |
| Rotate key and pair again | `make rotate-token` | `bash "$RELEASE/relay/rotate-token.sh"` |
| Change Cloudflare hostname | `bash relay/change-hostname.sh` | `bash "$RELEASE/relay/change-hostname.sh"` |
| Tear down stable Cloudflare | `make stable-teardown` | `bash "$RELEASE/relay/stable-teardown.sh"` |
| Turn off Tailscale | `bash relay/tailscale-off.sh` | `bash "$RELEASE/relay/tailscale-off.sh"` |
| Remove service only | `make service-uninstall` | `bash "$RELEASE/relay/service.sh" uninstall` |
| Remove core service and local state | `bash relay/uninstall.sh` | `bash "$RELEASE/relay/uninstall.sh"` |

**Prefix every table command with `HERDR_RELAY_BACKEND=hc`.** Turn off Tailscale
or tear down the stable Cloudflare tunnel before removing the service. Full
core uninstall preserves the shared release, CLI, TUI plugin, Pi extension and
agent hooks. Full TUI uninstall refuses while the core service remains installed;
remove core first.

WebRTC gateway transport is refused on core, and the Herdr plugin's build and
pane launchers are TUI-only. Phone-driven updates, app deployment and Pi
integration management remain TUI-only. Updating the shared release changes what both services use on their
next restart; reinstall/restart the core service manually after updating.

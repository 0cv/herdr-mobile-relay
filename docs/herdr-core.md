# A second relay for herdr-core

Set `HERDR_RELAY_BACKEND=hc` to connect to herdr-core, the backend used by
herdr-app, instead of the Herdr TUI. Each relay process has one backend. Run two
relays to use both: the phone shows `<hostname>` and `<hostname> (core)` as two
computers. Install and pair each instance separately with the commands below.

## Available on core

- Agent list and status, and terminal screen reading.
- Prompts, including while an agent is waiting for an answer.
- Key presses, text without Enter, and typed question answers. Typed text must
  be single-line and contain no control characters.
- Secret input to a verified no-echo prompt, without input-side persistence.
  Programs that echo or log a secret can still expose it in terminal output.
- Start agents with **+**, and stop, clear or restart relay-launched agents.
  Clear and Restart start a replacement in a new terminal, then kill the old one.
  herdr.app names the adopted new tab itself; the terminal carries the agent name.
  Initial prompts wait for reported idle status; otherwise launch completes with
  a warning and leaves the prompt unsent.
- Rename and reorder tabs in herdr-app. Either action can be refused when the
  desktop changed the layout at the same moment; try again.
- Pane size leasing (Resize Session). The phone's size applies to the shared
  core terminal, so herdr-app shows the phone's layout while a device leases,
  as with Resize Session on the TUI. The desktop can take the size back by
  resizing its window. Releasing the lease restores the previous viewer's size.
- Pi slash commands, activity, and the Conversation view (history and live)
  when Pi runs in a core terminal with the Pi bridge installed. Pi's session is
  learned from its bridge, so it needs the bridge from this release. The TUI
  relay owns the shared Pi extension.

The phone hides actions that core does not support: agent identity renaming;
tab creation and close; workspace and worktree management; the Pi
integration switch; phone-driven relay updates and app deploys. Core upgrades
are manual.

## How the core relay learns agent sessions

The `herdr-mobile-relay agent-session claude|codex|cursor|omp` hook command reads a
SessionStart payload and reports its `session_id` through terminal labels:
`herdr-mobile-relay.session` and `herdr-mobile-relay.pid`, written together.
It uses only `HC_TERMINAL_ID` and `HC_TERMINAL_SOCKET`, and accepts the
foreground pid only when it is an ancestor of the hook. The relay uses the
session only while that pid remains the terminal's foreground pid, ignoring
stale labels. Pi still discovers sessions through its bridge when no label
session applies.

## Conversation for Claude Code, Codex, Cursor, Omp and Hermes

Run `herdr-mobile-relay agent-hooks install` once from a terminal to enable
Conversation for Claude Code, Codex, Cursor (cursor-agent) and Omp. Restart
running agents to load the hooks. The hook does nothing outside a core terminal.
Cursor adds a flat `sessionStart` command in `~/.cursor/hooks.json`. Omp uses
`~/.omp/agent/extensions/herdr-mobile-relay-session.ts`, reporting session start
and switch events without changing Herdr's extension.

Hermes needs no hook. On core, the relay reads `runtime/active_sessions.json`
under its configured Hermes roots. It accepts exactly one CLI entry matching the
terminal foreground PID and its process creation time; missing, stale or
ambiguous evidence leaves Conversation unavailable.

Use `herdr-mobile-relay agent-hooks status` to check installation and
`herdr-mobile-relay agent-hooks uninstall` to remove only the relay's hooks.
Existing Herdr hooks are preserved; missing agent config directories are skipped.
Codex also needs `[features] hooks = true` in its `config.toml`, as enabled by
Herdr's Codex integration. After installing, open `/hooks` in Codex once and
trust the new SessionStart hook: Codex shows "1 hook needs review before it can
run" until approved. Codex stores approval under `[hooks.state]` in
`~/.codex/config.toml`; the installer never writes that trust and leaves this
file unchanged.

In herdr.app terminals, start Codex with `codex --no-daemon`. By default, Codex
runs hooks in a shared background server, outside the terminal, so its sessions
cannot be matched to their terminal. Codex runs its session-start hook only when
the first prompt is sent, so Conversation appears after the first prompt of each
start or resume.

To do this only inside Herdr (herdr.app terminals and Herdr TUI panes), add to
`~/.zshrc`:

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

`HERDR_SOCKET_PATH` is ignored on core. Explicit port and
`HERDR_RELAY_SERVICE_NAME` values win. If `HERDR_RELAY_ENV` is set, its containing
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

WebRTC gateway transport and plugin menu entry points are refused on core.
Phone-driven updates, app deployment and Pi integration management remain
TUI-only. Updating the shared release changes what both services use on their
next restart; reinstall/restart the core service manually after updating.

# A second relay for herdr-core

Set `HERDR_RELAY_BACKEND=hc` to connect to herdr-core, the backend used by
herdr-app, instead of the Herdr TUI. Each relay process has one backend. Run two
relays to use both: the phone shows `<hostname>` and `<hostname> (core)` as two
computers. There is no automatic discovery or relay supervisor; configure and
manage each instance yourself.

## Available on core

- Agent list and status, and terminal screen reading.
- Prompts, including while an agent is waiting for an answer.
- Key presses and structured question answers that need no typed text.
- Stop an agent.
- Pane size leasing (Resize Session). The phone's size applies to the shared
  core terminal, so herdr-app shows the phone's layout while a device leases,
  as with Resize Session on the TUI. The desktop can take the size back by
  resizing its window. Releasing the lease restores the previous viewer's size.
- Pi slash commands, activity, and the Conversation view (history and live)
  when Pi runs in a core terminal with the Pi bridge installed. Pi's session is
  learned from its bridge, so it needs the bridge from this release. The TUI
  relay owns the shared Pi extension.

The phone hides actions that core does not support: starting, clearing or
restarting agents; renaming; tab, workspace and worktree management; manual and
secret text input; answers requiring typed text; the Pi
integration switch; phone-driven relay updates and app deploys. Core upgrades
are manual.

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
| Socket | `HC_SOCKET_PATH`, otherwise `$XDG_STATE_HOME/herdr-core/<session>/<session>.sock` |
| Session | `HC_SESSION`, otherwise `default` |
| State home | `XDG_STATE_HOME`, otherwise `~/.local/state` |
| HTTP port | `HERDR_RELAY_PORT=8377` |
| Plugin UDP port | `HERDR_RELAY_PLUGIN_PORT=8378` |
| macOS service name | `com.herdr-mobile-relay-hc.service` |
| Linux service name | `herdr-mobile-relay-hc.service` |
| Runtime directory | `$XDG_CONFIG_HOME/herdr-mobile-relay-hc` (normally `~/.config/herdr-mobile-relay-hc`) |
| Cache directory | `$XDG_CACHE_HOME/herdr-mobile-relay-hc` (normally `~/.cache/herdr-mobile-relay-hc`) |

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

## Manual second-instance setup

These steps assume an installed TUI user service, a shared release containing
core support, and an already-running core accessible to the same OS user. They
use a separate named Cloudflare tunnel so the existing TUI transport is left
alone. You need `cloudflared`, an authorized Cloudflare zone and an unused
hostname in that zone. Do not run the setup wizard for this second instance.

### 1. Create private configuration

In Bash, set the shared install path (adjust it if the TUI service uses a custom
release root):

```bash
RELEASE="${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay/current"
CORE_DIR="$HOME/.config/herdr-mobile-relay-hc"
umask 077
mkdir -p "$CORE_DIR"
chmod 700 "$CORE_DIR"
```

Create a **new** `$CORE_DIR/relay.env`, not a copy of the TUI env. Use absolute
paths for `HC_SOCKET_PATH` and `CLOUDFLARED_CONFIG` below:

```sh
HERDR_RELAY_BACKEND=hc
HERDR_RELAY_HOST=127.0.0.1
HERDR_RELAY_PORT=8377
HERDR_RELAY_PLUGIN_PORT=8378
HC_SOCKET_PATH='/absolute/path/to/herdr-core/default/default.sock'
CLOUDFLARED_CONFIG='/absolute/path/to/.config/herdr-mobile-relay-hc/cloudflared.yml'
```

The service wrapper still supplies port 8375 when it is absent, so explicitly
set 8377 even though the Go core backend defaults to it. Omit `HC_SOCKET_PATH`
only if the default session socket is correct; otherwise use its actual path,
or set `HC_SESSION` and `XDG_STATE_HOME` in this file.

Generate a new key and instance ID using the same generators as the installers
(run once on the new file):

```bash
. "$RELEASE/relay/common.sh"
printf 'HERDR_RELAY_TOKEN=%s\nHERDR_RELAY_INSTANCE_ID=%s\n' \
  "$(generate_token)" "$(generate_instance_id)" >> "$CORE_DIR/relay.env"
chmod 600 "$CORE_DIR/relay.env"
```

### 2. Give core its own tunnel

Choose a unique tunnel name and hostname; replace the examples below. The
hostname must belong to the zone already authorized by `cloudflared`.

```bash
cloudflared tunnel create --output json \
  --credentials-file "$CORE_DIR/tunnel.json" herdr-mobile-relay-hc-mycomputer
```

Use the returned tunnel UUID in `$CORE_DIR/cloudflared.yml`:

```yaml
tunnel: REPLACE_WITH_TUNNEL_UUID
credentials-file: /absolute/path/to/.config/herdr-mobile-relay-hc/tunnel.json
ingress:
  - hostname: core-relay.example.com
    service: http://127.0.0.1:8377
  - service: http_status:404
```

```bash
chmod 600 "$CORE_DIR/cloudflared.yml" "$CORE_DIR/tunnel.json"
cloudflared tunnel route dns REPLACE_WITH_TUNNEL_UUID core-relay.example.com
```

Do not reuse the TUI tunnel or change its ingress. The copied service wrapper
will run this tunnel alongside the core relay.

### 3a. macOS: copy the launchd definition

```bash
cp "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" \
  "$HOME/Library/LaunchAgents/com.herdr-mobile-relay-hc.service.plist"
mkdir -p "$HOME/Library/Logs/herdr-mobile-relay-hc"
```

Edit **only the copy**:

- Set `Label` to `com.herdr-mobile-relay-hc.service`.
- In `EnvironmentVariables`, set `HERDR_RELAY_ENV` to the absolute path of
  `$CORE_DIR/relay.env` and `HERDR_RELAY_SERVICE_NAME` to
  `com.herdr-mobile-relay-hc.service`.
- Set `StandardOutPath` and `StandardErrorPath` to `service.log` and
  `service.err` under the new `~/Library/Logs/herdr-mobile-relay-hc` directory,
  using absolute paths.
- Keep `ProgramArguments` pointing at the shared
  `current/relay/herdr-mobile-relay-service.sh` and `WorkingDirectory` at
  `current`. Keep the existing restart settings.

Plists do not expand `$HOME` or `~`. Check and start the copy:

```bash
chmod 600 "$HOME/Library/LaunchAgents/com.herdr-mobile-relay-hc.service.plist"
plutil -lint "$HOME/Library/LaunchAgents/com.herdr-mobile-relay-hc.service.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.herdr-mobile-relay-hc.service.plist"
launchctl print "gui/$(id -u)/com.herdr-mobile-relay-hc.service"
```

Stop with `launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.herdr-mobile-relay-hc.service.plist"`;
start again with `bootstrap` above. Restart a loaded service with
`launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay-hc.service"`.

### 3b. Linux: copy the systemd user unit

```bash
cp "$HOME/.config/systemd/user/herdr-mobile-relay.service" \
  "$HOME/.config/systemd/user/herdr-mobile-relay-hc.service"
```

Edit **only the copy**. Change the description to identify core and replace its
`Environment=HERDR_RELAY_ENV=...` line with these lines, using your absolute
path rather than the example:

```ini
Environment="HERDR_RELAY_ENV=/home/you/.config/herdr-mobile-relay-hc/relay.env"
Environment="HERDR_RELAY_SERVICE_NAME=herdr-mobile-relay-hc.service"
```

Keep `ExecStart` pointing at the shared
`current/relay/herdr-mobile-relay-service.sh`, `WorkingDirectory` at `current`,
and the existing restart and install settings. Then:

```bash
chmod 600 "$HOME/.config/systemd/user/herdr-mobile-relay-hc.service"
systemctl --user daemon-reload
systemctl --user enable herdr-mobile-relay-hc.service
systemctl --user start herdr-mobile-relay-hc.service
systemctl --user status herdr-mobile-relay-hc.service
journalctl --user -u herdr-mobile-relay-hc.service -f
```

Use `systemctl --user stop herdr-mobile-relay-hc.service` to stop it and
`systemctl --user restart herdr-mobile-relay-hc.service` to restart it.

### 4. Check and pair the core instance

Check `http://127.0.0.1:8377/readyz` and
`https://core-relay.example.com/readyz`; both should report the core instance ID
from its env file and ready inventory. Do not pair if the public endpoint
reports the TUI identity.

`relay/setup-link.sh` checks the installed **TUI** service's env path, so it is
not the second-instance entry point. Instead, use its underlying helpers in a
fresh Bash shell. Set `RELEASE` and `CORE_DIR` as above, then:

```bash
export HERDR_RELAY_ENV="$CORE_DIR/relay.env"
. "$RELEASE/relay/common.sh"
load_relay_env "$HERDR_RELAY_ENV"
FRAGMENT="$(build_setup_fragment "$HERDR_RELAY_TOKEN" "$(host_label) (core)" \
  'wss://core-relay.example.com')"
if arm_setup_link "$HERDR_RELAY_ENV"; then
  print_phone_setup "https://YOUR-EXISTING-PHONE-APP/#$FRAGMENT"
else
  printf 'Core relay is not running; start it and try again.\n'
fi
```

Replace the relay hostname and phone-app origin before running. The origin
must be the same HTTPS app already installed on your phone, not a second app.
`arm_setup_link` reads `relay.pid` beside the core env file and signals that
relay to arm a fresh invitation. The fragment encoder uses the core token
loaded from its env file, not the TUI token. Scan the QR or open the full
link in your phone app. It pairs one device within ten minutes; repeat for
another device. Keep the link private.

## Helpers and upgrades

The shipped service helpers still address the TUI service. Do not use these to
manage core, even with `HERDR_RELAY_BACKEND=hc`:

- `relay/service.sh`, `relay/install-service.sh`,
  `relay/install-systemd-user-service.sh`, `relay/uninstall-service.sh` and
  `relay/uninstall-systemd-user-service.sh`; the `make service-install`,
  `service-uninstall`, `service-status` and `service-logs` targets.
- `relay/rotate-token.sh` / `make rotate-token`: its restart targets the TUI.
- `relay/plugin-setup-menu.sh`, the plugin setup actions,
  `relay/setup.sh` / `make setup`, `relay/start.sh` / `make quick-start`,
  `relay/stable-setup.sh` / `make stable-setup`, and
  `relay/stable-teardown.sh` / `make stable-teardown`.
- `relay/setup-link.sh` / `make setup-link`: use the core-specific helper calls
  above instead.
- Phone update and deploy actions, and `relay/configure-app-deploy.sh` /
  `make app-deploy-setup`: keep these on the TUI owner.

The plugin install/upgrade path in `relay/plugin-build.sh` manages the TUI
service, not the copied core service. Upgrading the TUI relay updates the shared
install. Restart the core service by
hand afterwards using the command for your OS above. The core relay neither
updates itself from the phone nor installs or refreshes the shared Pi extension;
enable and maintain that integration through the TUI relay.

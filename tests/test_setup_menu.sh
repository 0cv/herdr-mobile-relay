#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/herdr-menu-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
unset HERDR_BIN_PATH
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/config" XDG_DATA_HOME="$WORK/data"
export HERDR_PLUGIN_CONFIG_DIR="$WORK/plugin" HERDR_RELAY_ENV="$WORK/config/relay.env"
export HERDR_RELEASE_ROOT="$WORK/releases" HERDR_RELAY_BIN="$WORK/bin/herdr-mobile-relay"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$HERDR_PLUGIN_CONFIG_DIR" "$WORK/bin" "$WORK/relay"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" "$WORK/relay/"
for tool in curl launchctl systemctl tailscale cloudflared herdr herdr-mobile-relay; do
    printf '#!/bin/sh\nexit 1\n' > "$WORK/bin/$tool"
done
cat > "$WORK/bin/uname" <<'FAKE'
#!/bin/sh
if [ "$1" = -s ]; then echo Linux; else /usr/bin/uname "$@"; fi
FAKE
cat > "$WORK/bin/hc" <<'FAKE'
#!/bin/sh
[ "$*" = "server status" ] || { echo "unexpected hc $*" >&2; exit 2; }
[ ! -f "$HC_HANG" ] || exec sleep 30
[ ! -f "$HC_DOWN" ]
FAKE
cat > "$WORK/bin/curl" <<'FAKE'
#!/bin/sh
for url; do :; done
case "$url" in
    *:8375/healthz) echo '{"release_version":"1.0.0"}' ;;
    *:8377/healthz) echo '{"release_version":"2.0.0"}' ;;
    *) exit 1 ;;
esac
FAKE
cat > "$WORK/bin/systemctl" <<'FAKE'
#!/bin/sh
case "$*" in
    *herdr-mobile-relay-hc.service*) echo inactive; exit 3 ;;
    *herdr-mobile-relay.service*) echo active ;;
    *) exit 1 ;;
esac
FAKE
for action in plugin-choose-transport plugin-install-service plugin-change-hostname plugin-stable-teardown plugin-configure-app-deploy plugin-setup-link plugin-status tailscale-setup tailscale-off; do
    cat > "$WORK/relay/$action.sh" <<'FAKE'
#!/bin/sh
printf 'ACTION: %s %s\n' "${0##*/}" "$*"
printf 'BACKEND: %s env=%s plugin=%s\n' "$HERDR_RELAY_BACKEND" "$HERDR_RELAY_ENV" "${HERDR_PLUGIN_CONFIG_DIR:-}"
FAKE
done
chmod +x "$WORK/bin/"* "$WORK/relay/"*.sh
ln -s "$BASH" "$WORK/bin/bash"
# The menu offers the core relay when `hc` exists, so tests never see the developer's PATH.
PYTHON3="$(command -v python3)"
export PATH="$WORK/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HC_DOWN="$WORK/hc-down" HC_HANG="$WORK/hc-hang"
CORE_ENV="$XDG_CONFIG_HOME/herdr-mobile-relay-hc/relay.env"
HOST="$(hostname -s)"
without() { mv "$WORK/bin/$1" "$WORK/$1.hidden"; }
restore() { mv "$WORK/$1.hidden" "$WORK/bin/$1"; }

menu() { printf '%b' "$1" | bash "$WORK/relay/plugin-setup-menu.sh"; }
items() { printf '%s\n' "$1" | grep -E '^  [123bq]\. ' || true; }
expect() { [[ "$1" == *"$2"* ]] || { printf 'Missing: %s\n' "$2" >&2; exit 1; }; }
reject() { [[ "$1" != *"$2"* ]] || { printf 'Unexpected: %s\n' "$2" >&2; exit 1; }; }

> "$HERDR_RELAY_ENV"
without hc
for entry in plugin-build open-plugin-pane; do
    if HERDR_RELAY_BACKEND=hc bash "$REPO_DIR/relay/$entry.sh" setup > "$WORK/core-output" 2>&1; then
        echo "core accepted TUI entry point $entry" >&2; exit 1
    fi
    grep -q 'TUI-only' "$WORK/core-output"
done
output="$(menu 'q\n')"
[[ "$(items "$output")" == $'  1. Cloudflare\n  2. WebRTC\n  3. Tailscale\n  q. Exit, change nothing' ]]
reject "$output" '  b. Back'
reject "$output" 'Temporary Cloudflare Tunnel'
reject "$output" 'ACTION:'

output="$(menu '1\nq\n')"
expect "$output" '  5. Configure App Deployment'
expect "$output" '  7. Show Full Status'
reject "$output" 'Community WebRTC Gateway'
reject "$output" 'Always on Tailscale'
reject "$output" 'ACTION:'

output="$(menu '2\nq\n')"
expect "$output" '  1. Community WebRTC Gateway'
expect "$output" '  2. Deploy or Upgrade Your Own WebRTC Gateway'
expect "$output" '  4. Show Full Status'
reject "$output" 'Temporary Cloudflare Tunnel'
reject "$output" 'Always on Tailscale'

output="$(menu '3\nq\n')"
expect "$output" '  1. Always on Tailscale'
expect "$output" '  2. Turn Always on Tailscale Off'
reject "$output" 'Temporary Cloudflare Tunnel'
reject "$output" 'Community WebRTC Gateway'

while read -r path choice action; do
    output="$(menu "$path\\n$choice\\nq\\n")"
    expect "$output" "ACTION: $action"
done <<'EOF'
1 1 plugin-choose-transport.sh temporary
1 2 plugin-install-service.sh
1 3 plugin-change-hostname.sh
1 4 plugin-stable-teardown.sh
1 5 plugin-configure-app-deploy.sh
1 6 plugin-setup-link.sh
1 7 plugin-status.sh
2 1 plugin-choose-transport.sh community
2 2 plugin-choose-transport.sh own
2 3 plugin-setup-link.sh
2 4 plugin-status.sh
3 1 tailscale-setup.sh
3 2 tailscale-off.sh
3 3 plugin-setup-link.sh
3 4 plugin-status.sh
EOF

output="$(menu '\nq\n')"
expect "$output" '  1. Temporary Cloudflare Tunnel'
output="$(menu '1\nb\nq\n')"
[[ "$(printf '%s\n' "$output" | grep -c 'Choose a path:')" == 2 ]]
output="$(menu '2\n4\nq\n')"
expect "$output" 'ACTION: plugin-status.sh '
[[ "$(printf '%s\n' "$output" | grep -c '^WebRTC$')" == 2 ]]
[[ "$(printf '%s\n' "$output" | grep -c 'Choose a path:')" == 1 ]]
output="$(menu '8\n2\n9\nb\nq\n')"
expect "$output" 'Enter 1–3 or q.'
expect "$output" 'Enter 1–4, b or q.'
reject "$output" 'ACTION:'
reject "$output" $'\033'

"$PYTHON3" - "$WORK/relay/plugin-setup-menu.sh" <<'PY'
import errno
import os
import pty
import select
import subprocess
import sys
import time

master, slave = pty.openpty()
process = subprocess.Popen(
    ["bash", sys.argv[1]], stdin=slave, stdout=slave, stderr=slave
)
os.close(slave)
output = bytearray()
deadline = time.monotonic() + 10

def read_chunk():
    remaining = deadline - time.monotonic()
    if remaining <= 0 or not select.select([master], [], [], remaining)[0]:
        raise TimeoutError("setup menu PTY timed out")
    try:
        return os.read(master, 4096)
    except OSError as error:
        if error.errno == errno.EIO:
            return b""
        raise

def wait_for(text, start=0):
    while True:
        index = output.find(text, start)
        if index >= 0:
            return index + len(text)
        chunk = read_chunk()
        assert chunk, f"setup menu closed before {text!r}"
        output.extend(chunk)

try:
    cursor = wait_for(b"Choice [1]: ")
    os.write(master, b"2\n")
    cursor = wait_for(b"Choice [1]: ", cursor)
    os.write(master, b"9\n")
    cursor = wait_for("Enter 1–4, b or q.".encode(), cursor)
    cursor = wait_for(b"Choice [1]: ", cursor)
    os.write(master, b"4\n")
    wait_for(b"Press Enter to return to the menu.", cursor)
    os.write(master, b"\x04")
    while chunk := read_chunk():
        output.extend(chunk)
    assert process.wait(timeout=max(0.01, deadline - time.monotonic())) == 0
finally:
    if process.poll() is None:
        process.kill()
        process.wait()
    os.close(master)

clear = b"\x1b[H\x1b[2J"
header = "🐑 Herdr Mobile Relay Setup".encode()
assert output.count(header) == 2
assert output.count(clear + header) == 2, "every menu screen must start with a clear"
assert output.count(clear) == 2, "invalid choices and EOF must not clear the screen"
assert output.count("Enter 1–4, b or q.".encode()) == 1
assert b"ACTION: plugin-status.sh" in output
assert clear not in output.split(b"ACTION: plugin-status.sh", 1)[1]
PY

menu '1\n' >/dev/null
menu '' >/dev/null

printf "HERDR_GATEWAY_URL='wss://gw.example.test'\n" > "$HERDR_RELAY_ENV"
output="$(menu 'q\n')"
expect "$output" '  2. WebRTC (active)'
output="$(menu '\nq\n')"
expect "$output" '  1. Community WebRTC Gateway'
reject "$output" '  1. Temporary Cloudflare Tunnel'

printf "HERDR_TAILSCALE_PORT='8443'\nHERDR_GATEWAY_URL='wss://gw.example.test'\n" > "$HERDR_RELAY_ENV"
output="$(menu 'q\n')"
expect "$output" '  3. Tailscale (active)'
output="$(menu '\n2\nq\n')"
expect "$output" 'ACTION: tailscale-off.sh '
[[ "$(printf '%s\n' "$output" | grep -c '^Tailscale$')" == 2 ]]

printf "CLOUDFLARED_CONFIG='%s/cloudflared.yml'\n" "$WORK" > "$HERDR_RELAY_ENV"
printf 'hostname: relay.example.test\n' > "$WORK/cloudflared.yml"
output="$(menu 'q\n')"
expect "$output" '  1. Cloudflare (active)'
output="$(HERDR_RELAY_BACKEND=hc menu 'q\n')"
expect "$output" "Herdr TUI relay (phone shows $HOST)"
reject "$output" 'Herdr app relay'

# Core only: no herdr command, no TUI relay env, a core env and hc.
rm -f "$HERDR_RELAY_ENV"
without herdr
mkdir -p "${CORE_ENV%/*}"
printf "HERDR_RELAY_BACKEND='hc'\n" > "$CORE_ENV"
restore hc
output="$(menu 'q\n')"
reject "$output" 'What should your phone control?'
reject "$output" 'Herdr TUI'
expect "$output" "Herdr app relay (phone shows $HOST (core))"
expect "$output" '  Core:       running'
expect "$output" '  Phone path: not chosen yet - choose a path'
[[ "$(items "$output")" == $'  1. Cloudflare\n  2. Tailscale\n  q. Exit, change nothing' ]]
reject "$output" 'WebRTC'
touch "$HC_DOWN"
output="$(menu 'q\n')"
expect "$output" '  Core:       not running - Start herdr-app or run `hc server start`'
touch "$HC_HANG"
SECONDS=0
output="$(menu 'q\n')"
expect "$output" '  Core:       not running - Start herdr-app or run `hc server start`'
((SECONDS < 10)) || { echo 'core status probe was not bounded' >&2; exit 1; }
rm -f "$HC_DOWN" "$HC_HANG"
output="$(menu '1\nq\n')"
expect "$output" '  1. Temporary Cloudflare Tunnel'
expect "$output" '  4. Remove Stable Tunnel'
expect "$output" '  5. Choose Phone App and Show QR'
expect "$output" '  6. Show Full Status'
reject "$output" 'Configure App Deployment'
reject "$output" '  7.'
reject "$output" 'Install Herdr'
output="$(menu '2\nq\n')"
expect "$output" '  1. Always on Tailscale'
expect "$output" 'Publish HTTPS port 8444 and install a background relay service.'
expect "$output" "Remove only this relay's port 8444"
reject "$output" '8443'
while read -r path choice action; do
    output="$(menu "$path\\n$choice\\nq\\n")"
    expect "$output" "ACTION: $action"
    expect "$output" "BACKEND: hc env=$CORE_ENV plugin="$'\n'
done <<'EOF'
1 1 plugin-choose-transport.sh temporary
1 2 plugin-install-service.sh
1 3 plugin-change-hostname.sh
1 4 plugin-stable-teardown.sh
1 5 plugin-setup-link.sh
1 6 plugin-status.sh
2 1 tailscale-setup.sh
2 2 tailscale-off.sh
2 3 plugin-setup-link.sh
2 4 plugin-status.sh
EOF
output="$(menu '1\nb\nq\n')"
[[ "$(printf '%s\n' "$output" | grep -c 'Choose a path:')" == 2 ]]
output="$(menu '3\n2\nq\n')"
expect "$output" 'Enter 1–2 or q.'
output="$(menu '1\n7\nq\n')"
expect "$output" 'Enter 1–6, b or q.'
reject "$output" 'ACTION:'

printf "HERDR_TAILSCALE_PORT='8444'\n" >> "$CORE_ENV"
output="$(menu '\n1\nq\n')"
expect "$output" '  2. Tailscale (active)'
output="$(menu 'q\n')"
expect "$output" '  Phone path: Tailscale Serve (HTTPS port 8444)'
output="$(menu '\nq\n')"
expect "$output" '  1. Always on Tailscale'

# The TUI's plugin pane variables never reach a core action.
export HERDR_RELAY_PORT=9999 HERDR_GATEWAY_URL=wss://tui.example.test HERDR_TAILSCALE_PORT=9443
output="$(menu '1\n2\nq\n')"
expect "$output" "BACKEND: hc env=$CORE_ENV plugin="$'\n'
unset HERDR_RELAY_PORT HERDR_GATEWAY_URL HERDR_TAILSCALE_PORT

# TUI and core detected: the target question comes first.
: > "$HERDR_RELAY_ENV"
printf "HERDR_TAILSCALE_PORT='8443'\n" > "$HERDR_RELAY_ENV"
restore herdr
mkdir -p "$HOME/.config/systemd/user"
: > "$HOME/.config/systemd/user/herdr-mobile-relay.service"
: > "$HOME/.config/systemd/user/herdr-mobile-relay-hc.service"
mkdir -p "$HERDR_RELEASE_ROOT/current"
printf '{\n  "version": "1.0.0"\n}\n' > "$HERDR_RELEASE_ROOT/current/release-manifest.json"
output="$(menu 'q\n')"
expect "$output" "Herdr TUI relay (phone shows $HOST)"
expect "$output" "Herdr app relay (phone shows $HOST (core))"
expect "$output" 'What should your phone control?'
reject "$output" 'Choose a path'
expect "$output" '  1. Herdr TUI'
expect "$output" '  2. Herdr app (core)'
expect "$output" '  3. Both'
reject "$output" '  b. Back'
expect "$output" '  Relay:      1.0.0 running'
expect "$output" '  Relay:      1.0.0 installed, 2.0.0 still running - restart pending'
expect "$output" '  Service:    installed (active)'
expect "$output" '  Service:    installed (inactive)'
expect "$output" '  Phone path: Tailscale Serve (HTTPS port 8443)'
expect "$output" '  Phone path: Tailscale Serve (HTTPS port 8444)'
expect "$output" '  Core:       running'
touch "$HC_DOWN"
output="$(menu 'q\n')"
expect "$output" '  Core:       not running - Start herdr-app or run `hc server start`'
rm -f "$HC_DOWN"
output="$(menu '4\n0\nb\nq\n')"
[[ "$(printf '%s\n' "$output" | grep -c 'Enter 1–3 or q.')" == 3 ]]

output="$(menu '1\nq\n')"
expect "$output" 'Choose a path:'
expect "$output" '  3. Tailscale (active)'
reject "$output" 'What should your phone control?'$'\n''  1.'$'\n'
[[ "$(printf '%s\n' "$output" | grep -c 'What should your phone control?')" == 1 ]]
output="$(menu '1\n1\nq\n')"
expect "$output" 'Cloudflare - Herdr TUI relay'
expect "$output" '  7. Show Full Status'
output="$(menu '1\n3\n1\nq\n')"
expect "$output" 'Tailscale - Herdr TUI relay'
expect "$output" 'Publish HTTPS port 8443 and install'
expect "$output" "BACKEND: herdr env=$HERDR_RELAY_ENV plugin=$HERDR_PLUGIN_CONFIG_DIR"
output="$(menu '1\nb\n2\nq\n')"
[[ "$(printf '%s\n' "$output" | grep -c 'What should your phone control?')" == 2 ]]
expect "$output" 'Choose a path:'
output="$(menu '2\n2\n2\nq\n')"
expect "$output" 'Tailscale - Herdr app relay'
expect "$output" 'Publish HTTPS port 8444 and install'
reject "$output" 'WebRTC'
expect "$output" "BACKEND: hc env=$CORE_ENV plugin="$'\n'
output="$(menu '2\n1\nq\n')"
expect "$output" 'Cloudflare - Herdr app relay'
reject "$output" 'Configure App Deployment'
reject "$output" 'WebRTC'
output="$(menu '2\nq\n')"
reject "$output" 'WebRTC'
reject "$output" 'Community'

# Both: only the service paths that work on both, run for the TUI, then the core.
output="$(menu '3\nq\n')"
expect "$output" 'Choose a path for both relays:'
expect "$output" '  1. Cloudflare Stable Tunnel'
expect "$output" '  2. Always on Tailscale'
expect "$output" 'Publish HTTPS port 8443 for the TUI relay and 8444 for the core relay'
expect "$output" '  3. Choose Phone App and Show QR'
expect "$output" '  4. Show Full Status'
expect "$output" '  b. Back'
reject "$output" 'Temporary Cloudflare Tunnel'
reject "$output" 'WebRTC'
reject "$output" 'Configure App Deployment'
reject "$output" 'Turn Always on Tailscale Off'
while read -r choice action; do
    output="$(menu "3\\n$choice\\nq\\n")"
    [[ "$(printf '%s\n' "$output" | grep -E '^(ACTION|BACKEND|──)' )" == "── Herdr TUI relay ──
ACTION: $action 
BACKEND: herdr env=$HERDR_RELAY_ENV plugin=$HERDR_PLUGIN_CONFIG_DIR
── Herdr app relay ──
ACTION: $action 
BACKEND: hc env=$CORE_ENV plugin=" ]] || { printf 'Both did not run %s for each relay:\n%s\n' "$action" "$output" >&2; exit 1; }
done <<'EOF'
1 plugin-install-service.sh
2 tailscale-setup.sh
3 plugin-setup-link.sh
4 plugin-status.sh
EOF
output="$(menu '3\n5\nb\nq\n')"
expect "$output" 'Enter 1–4, b or q.'
[[ "$(printf '%s\n' "$output" | grep -c 'What should your phone control?')" == 2 ]]

# Every script the menu can run, directly or through another, ships in the release.
seen=" "
queue="plugin-setup-menu"
while [ -n "$queue" ]; do
    set -- $queue
    name="$1"
    shift
    queue="$*"
    case "$seen" in *" $name "*) continue ;; esac
    seen="$seen$name "
    grep -qE "^ +$name\.sh( |;)" "$REPO_DIR/scripts/package-release.sh" ||
        [ "$name" = plugin-setup-menu ] ||
        { echo "release bundle is missing relay/$name.sh" >&2; exit 1; }
    for next in $(grep -oE '[a-z][a-z-]+\.sh' "$REPO_DIR/relay/$name.sh" | sed 's/\.sh$//'); do
        [ ! -f "$REPO_DIR/relay/$next.sh" ] || queue="$queue $next"
    done
done

echo 'setup menu tests passed'

#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/herdr-menu-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/config" XDG_DATA_HOME="$WORK/data"
export HERDR_PLUGIN_CONFIG_DIR="$WORK/plugin" HERDR_RELAY_ENV="$WORK/config/relay.env"
export HERDR_RELEASE_ROOT="$WORK/releases" HERDR_RELAY_BIN="$WORK/bin/herdr-mobile-relay"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$HERDR_PLUGIN_CONFIG_DIR" "$WORK/bin" "$WORK/relay"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" "$WORK/relay/"
for tool in curl launchctl systemctl tailscale cloudflared herdr herdr-mobile-relay; do
    printf '#!/bin/sh\nexit 1\n' > "$WORK/bin/$tool"
done
for action in plugin-choose-transport plugin-install-service plugin-change-hostname plugin-stable-teardown plugin-configure-app-deploy plugin-setup-link plugin-status tailscale-setup tailscale-off; do
    printf '#!/bin/sh\nprintf "ACTION: %%s %%s\\n" "${0##*/}" "$*"\n' > "$WORK/relay/$action.sh"
done
chmod +x "$WORK/bin/"* "$WORK/relay/"*.sh
export PATH="$WORK/bin:$PATH"

menu() { printf '%b' "$1" | bash "$WORK/relay/plugin-setup-menu.sh"; }
items() { printf '%s\n' "$1" | grep -E '^  [123bq]\. ' || true; }
expect() { [[ "$1" == *"$2"* ]] || { printf 'Missing: %s\n' "$2" >&2; exit 1; }; }
reject() { [[ "$1" != *"$2"* ]] || { printf 'Unexpected: %s\n' "$2" >&2; exit 1; }; }

> "$HERDR_RELAY_ENV"
for entry in plugin-setup-menu plugin-build open-plugin-pane; do
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

python3 - "$WORK/relay/plugin-setup-menu.sh" <<'PY'
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
echo 'setup menu tests passed'

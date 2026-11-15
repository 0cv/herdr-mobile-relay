#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-setup-menu-flow-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

unset GH_TOKEN GITHUB_TOKEN
mkdir -p "$WORK_DIR/config"
NORMALIZE_BIN="$WORK_DIR/normalize/herdr-mobile-relay"
mkdir -p "$(dirname "$NORMALIZE_BIN")"
write_origin_stub "$NORMALIZE_BIN"

# The setup menu opens after every install, including upgrades, so it has to
# report what exists before offering to change it: a stale phone app is exactly
# the thing a person cannot otherwise see.
MENU_BIN_DIR="$WORK_DIR/menu-bin"
MENU_ENV="$WORK_DIR/config/menu.env"
MENU_ROOT="$WORK_DIR/menu-release"
mkdir -p "$MENU_BIN_DIR" "$MENU_ROOT/current" "$WORK_DIR/menu-home"
# The menu offers the core relay when `hc` or a core env exists, so these runs
# see neither the developer's tools nor their home directory.
MENU_SYSTEM_PATH="/usr/bin:/bin:/usr/sbin:/sbin"
menu_isolation=(env HERDR_BIN_PATH= HOME="$WORK_DIR/menu-home" XDG_CONFIG_HOME="$WORK_DIR/menu-home/.config" PATH="$MENU_BIN_DIR:$MENU_SYSTEM_PATH")
printf '{\n  "version": "9.9.9"\n}\n' > "$MENU_ROOT/current/release-manifest.json"
printf "HERDR_GATEWAY_URL='wss://gw-a.example.test,wss://gw-b.example.test'\nHERDR_APP_DEPLOY_ORIGIN='https://app.example.test'\n" > "$MENU_ENV"
printf "HERDR_GATEWAY_DEPLOY_HOST='gw-owned.example.test'\n" \
    > "$WORK_DIR/config/gateway-deploy"
printf 'https://relay.example.test\n' > "$WORK_DIR/config/phone-app-origin"
write_stub "$MENU_BIN_DIR/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *127.0.0.1*healthz*) printf '{"status":"ok","release_version":"9.9.9","gateway_url":"wss://gw-b.example.test","gateway_version":"9.9.8","gateway_available_version":"9.9.10"}\n' ;;
    *app.example.test/version.json*) printf '{"version":"9.9.8","assets":1}\n' ;;
    *gw-owned.example.test/healthz*) printf '{"ok":true,"version":"9.9.8","revision":"abc123"}\n' ;;
    *) exit 22 ;;
esac
EOF
# Entering an action and finishing it has to come back here, not end the pane:
# WebRTC → Show Full Status shows the status, then the submenu is redrawn and q leaves. Without a terminal
# the return prompt is skipped, so the input carries no extra newline.
MENU_OUTPUT="$(
    printf '2\n4\nq\n' |
        "${menu_isolation[@]}" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELEASE_ROOT="$MENU_ROOT" \
        HERDR_RELAY_ENV="$MENU_ENV" \
        "$BASH" "$REPO_DIR/relay/plugin-setup-menu.sh"
)"
case "$MENU_OUTPUT" in
    *"Herdr Mobile Relay status"*) ;;
    *) echo "setup menu did not open the status action" >&2; exit 1 ;;
esac
test "$(printf '%s\n' "$MENU_OUTPUT" | grep -c 'Herdr Mobile Relay Setup')" -ge 2 ||
    { echo "setup menu did not come back after an action" >&2; exit 1; }
case "$MENU_OUTPUT" in
    *"Relay:      9.9.9 running"*) ;;
    *) echo "setup menu did not report the running release" >&2; exit 1 ;;
esac
# The status line names the rule that picked this gateway, so "why that one"
# is answerable without opening relay.env.
case "$MENU_OUTPUT" in
    *"gateway wss://gw-b.example.test runs 9.9.8; latest available 9.9.10 (+1 fallback, first listed wins)"*) ;;
    *) echo "setup menu did not report the active gateway, version, and selection rule" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"serves 9.9.8, this relay ships 9.9.9 - Cloudflare → Configure App Deployment"*) ;;
    *) echo "setup menu did not report the stale phone app" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Own gateway: gw-owned.example.test runs 9.9.8; plugin offers 9.9.10 - run herdr plugin install 0cv/herdr-mobile-relay, then redeploy with WebRTC → Deploy or Upgrade Your Own WebRTC Gateway"*) ;;
    *) echo "setup menu did not report how to update the stale self-hosted gateway" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Exit, change nothing"*) ;;
    *) echo "setup menu did not offer a way out" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"Choose Phone App and Show QR"*) ;;
    *) echo "setup menu did not expose the phone app origin chooser" >&2; exit 1 ;;
esac
case "$MENU_OUTPUT" in
    *"  1. Cloudflare"*"  2. WebRTC (active)"*"  3. Tailscale"*"  1. Community WebRTC Gateway"*"  2. Deploy or Upgrade Your Own WebRTC Gateway"*) ;;
    *) echo "setup menu did not group WebRTC actions or mark it active" >&2; exit 1 ;;
esac

# The menu process itself is long-lived. After a transport action removes a
# file-backed gateway, its inherited copy must be cleared before the next
# action; re-sourcing a file cannot unset a variable that is no longer present.
REFRESH_MENU_DIR="$WORK_DIR/refresh-menu-scripts"
REFRESH_MENU_ENV="$WORK_DIR/config/refresh-menu.env"
REFRESH_MENU_MARKER="$WORK_DIR/refresh-menu-state"
mkdir -p "$REFRESH_MENU_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" \
    "$REFRESH_MENU_DIR/"
printf "HERDR_GATEWAY_URL='wss://stale.example.test'\nHERDR_GATEWAY_SELECTION='latency'\n" \
    > "$REFRESH_MENU_ENV"
write_stub "$REFRESH_MENU_DIR/plugin-choose-transport.sh" <<'EOF'
#!/bin/sh
: > "$HERDR_RELAY_ENV"
EOF
write_stub "$REFRESH_MENU_DIR/plugin-status.sh" <<'EOF'
#!/bin/sh
if [ "${HERDR_GATEWAY_URL+x}" = x ]; then
    printf 'stale=%s\n' "$HERDR_GATEWAY_URL" > "$REFRESH_MENU_MARKER"
else
    printf 'unset\n' > "$REFRESH_MENU_MARKER"
fi
EOF
export REFRESH_MENU_MARKER
printf '2\n1\n4\nq\n' |
    "${menu_isolation[@]}" HERDR_RELAY_BIN="$NORMALIZE_BIN" HERDR_RELAY_ENV="$REFRESH_MENU_ENV" \
    "$BASH" "$REFRESH_MENU_DIR/plugin-setup-menu.sh" >/dev/null
test "$(cat "$REFRESH_MENU_MARKER")" = "unset"

# A setup pane can outlive an in-place plugin update. Replacing its script
# directory leaves the shell in a deleted cwd; the next action must first enter
# the newly installed directory rather than leaking getcwd/chdir failures.
STALE_MENU_DIR="$WORK_DIR/stale-menu-scripts"
STALE_MENU_OLD="$WORK_DIR/stale-menu-old"
STALE_MENU_FIFO="$WORK_DIR/stale-menu-input"
STALE_MENU_OUTPUT="$WORK_DIR/stale-menu-output"
STALE_MENU_MARKER="$WORK_DIR/stale-menu-action"
export STALE_MENU_MARKER
mkdir -p "$STALE_MENU_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-setup-menu.sh" \
    "$STALE_MENU_DIR/"
mkfifo "$STALE_MENU_FIFO"
exec 7<> "$STALE_MENU_FIFO"
(
    "${menu_isolation[@]}" HERDR_RELAY_BIN="$NORMALIZE_BIN" HERDR_RELAY_ENV="$MENU_ENV" \
        "$BASH" "$STALE_MENU_DIR/plugin-setup-menu.sh" <&7 \
        > "$STALE_MENU_OUTPUT" 2>&1
) &
STALE_MENU_PID=$!
STALE_MENU_RENDERED=false
for _attempt in 1 2 3 4 5 6 7 8 9 10; do
    if grep -q "Exit, change nothing" "$STALE_MENU_OUTPUT" 2>/dev/null; then
        STALE_MENU_RENDERED=true
        break
    fi
    sleep 0.1
done
if [ "$STALE_MENU_RENDERED" != true ]; then
    printf 'q\n' >&7
    wait "$STALE_MENU_PID" || true
    echo "setup menu did not render before cwd replacement" >&2
    exit 1
fi
mv "$STALE_MENU_DIR" "$STALE_MENU_OLD"
rm -rf "$STALE_MENU_OLD"
mkdir -p "$STALE_MENU_DIR"
write_stub "$STALE_MENU_DIR/plugin-status.sh" <<'EOF'
#!/bin/sh
printf 'action ran\n' > "$STALE_MENU_MARKER"
EOF
printf '2\n4\nq\n' >&7
wait "$STALE_MENU_PID"
exec 7>&-
test -f "$STALE_MENU_MARKER"
if grep -qE 'getcwd|cannot access parent directories|chdir:' "$STALE_MENU_OUTPUT"; then
    echo "setup action inherited the replaced plugin working directory" >&2
    exit 1
fi

echo "plugin-setup-menu shell tests passed"

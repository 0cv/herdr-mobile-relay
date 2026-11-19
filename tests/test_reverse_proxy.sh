#!/usr/bin/env bash
set -euo pipefail
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/herdr-reverse-proxy.XXXXXX")"
trap 'status=$?; if [ "$status" -ne 0 ]; then tail -80 "$WORK/out" >&2; fi; rm -rf "$WORK"; exit "$status"' EXIT
mkdir -p "$WORK/tmp"
export TMPDIR="$WORK/tmp" HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/xdg/config" XDG_DATA_HOME="$WORK/xdg/data" XDG_CACHE_HOME="$WORK/xdg/cache"
export HERDR_PLUGIN_CONFIG_DIR="$WORK/plugin" HERDR_RELEASE_ROOT="$WORK/release"
export HERDR_RELAY_ENV="$HOME/config/relay.env" HERDR_RELAY_BIN="$WORK/bin/herdr-mobile-relay"
export HERDR_BIN="$WORK/bin/herdr" HERDR_PHONE_APP_URL=relay FAKE_LOG="$WORK/log"
mkdir -p "$WORK/bin" "$HOME/config" "$WORK/release/current" "$HOME/.config/systemd/user" "$WORK/plugin"
printf '{"version":"v","revision":"r","web_hash":"h"}\n' > "$WORK/bin/release-manifest.json"
printf '#!/bin/sh\nexit 0\n' > "$HERDR_BIN"
cat > "$HERDR_RELAY_BIN" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$1" in
    version) echo '{"version":"v","revision":"r","backends":["herdr","hc"]}' ;;
    setup-fragment) printf 'token=%s&relay=%s\n' "$2" "$4" ;;
    normalize-origin) printf '%s\n' "${@: -1}" ;;
    qr) echo '[fake QR]' ;;
    serve) printf 'serve host=%s\n' "$HERDR_RELAY_HOST" >> "$FAKE_LOG" ;;
esac
EOF
cat > "$WORK/bin/curl" <<'EOF'
#!/bin/sh
printf 'curl %s\n' "$*" >> "$FAKE_LOG"
[ "${FAIL_HEALTH:-}" != 1 ] || exit 1
instance=$(awk -F "'" '/^HERDR_RELAY_INSTANCE_ID=/ {print $2}' "$HERDR_RELAY_ENV")
printf '{"status":"ready","inventory":{"state":"ready"},"instance":"%s","release_version":"v","revision":"r","bundle_hash":"h","protocol":3}\n' "$instance"
EOF
cat > "$WORK/bin/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$FAKE_LOG"
case "$*" in *is-active*) exit 3 ;; esac
EOF
cat > "$WORK/bin/cloudflared" <<'EOF'
#!/bin/sh
printf 'cloudflared %s\n' "$*" >> "$FAKE_LOG"
exit 1
EOF
printf '#!/bin/sh\nexit 0\n' > "$WORK/bin/systemd-analyze"
printf '#!/bin/sh\nexit 0\n' > "$WORK/bin/sleep"
cat > "$WORK/bin/id" <<'EOF'
#!/bin/sh
[ "$1" = -u ] && { echo 501; exit; }
/usr/bin/id "$@"
EOF
cat > "$WORK/bin/uname" <<'EOF'
#!/bin/sh
if [ "$1" = -s ]; then echo Linux; else /usr/bin/uname "$@"; fi
EOF
chmod +x "$WORK/bin/"*
mkdir -p "$HOME/.local/bin"
for fake in curl systemctl systemd-analyze cloudflared herdr herdr-mobile-relay uname id sleep; do
    ln -s "$WORK/bin/$fake" "$HOME/.local/bin/$fake"
done
export PATH="$WORK/bin:$PATH"
UNIT="$HOME/.config/systemd/user/herdr-mobile-relay.service"

run() { "$REPO_DIR/relay/$1" "${@:2}" > "$WORK/out" 2>&1; }
assert_absent() {
    if grep -Eq "$1" "$2"; then
        echo "Unexpected $1 in $2" >&2
        exit 1
    fi
}
tty_setup() {
    if [ "$(/usr/bin/uname -s)" = Darwin ]; then
        script -q /dev/null "$REPO_DIR/relay/reverse-proxy-setup.sh"
    else
        script -q -e -c "'$REPO_DIR/relay/reverse-proxy-setup.sh'" /dev/null
    fi
}
answer() { { printf '%s\n' "$@"; /bin/sleep 1; } | tty_setup > "$WORK/out" 2>&1; }

# The service runs with no tunnel when a reverse proxy is selected, even with
# no cloudflared config at all.
printf "HERDR_RELAY_TOKEN='saved'\nHERDR_REVERSE_PROXY_HOST='relay.home.test'\n" > "$HERDR_RELAY_ENV"
(CLOUDFLARED_BIN="$WORK/bin/cloudflared" run herdr-mobile-relay-service.sh) || :
grep -q 'Starting herdr relay' "$WORK/out"
assert_absent 'cloudflared|Cloudflare tunnel config' "$WORK/out"
assert_absent '^cloudflared ' "$FAKE_LOG"
run service.sh install
test -f "$UNIT"
assert_absent '^CLOUDFLARED_CONFIG=' "$HERDR_RELAY_ENV"
run setup-link.sh
grep -q 'relay=wss://relay.home.test' "$WORK/out"
grep -q 'Reverse proxy: https://relay.home.test' "$WORK/out"

refusal="Your own reverse proxy is selected. Choose Reverse Proxy → Stop Using Your Own Reverse Proxy first."
for entry in 'plugin-choose-transport.sh community' 'plugin-install-service.sh' 'start.sh' 'tailscale-setup.sh' 'service.sh uninstall'; do
    : > "$FAKE_LOG"
    cp "$HERDR_RELAY_ENV" "$WORK/before"
    read -r -a args <<< "$entry"
    if run "${args[@]}"; then echo "$entry accepted the reverse proxy selection" >&2; exit 1; fi
    grep -Fq "$refusal" "$WORK/out" || { echo "$entry missing refusal" >&2; exit 1; }
    cmp "$HERDR_RELAY_ENV" "$WORK/before"
done

run reverse-proxy-off.sh
assert_absent '^HERDR_REVERSE_PROXY_HOST=|^HERDR_RELAY_HOST=' "$HERDR_RELAY_ENV"
test ! -e "$UNIT"
run reverse-proxy-off.sh
grep -q 'not selected; nothing changed' "$WORK/out"

printf "HERDR_TAILSCALE_PORT='8443'\n" > "$HERDR_RELAY_ENV"
if run reverse-proxy-setup.sh; then echo 'setup accepted a Tailscale selection' >&2; exit 1; fi
grep -q 'Always on Tailscale is selected' "$WORK/out"
rm -f "$HERDR_RELAY_ENV"
if run reverse-proxy-setup.sh; then echo 'unattended setup accepted' >&2; exit 1; fi
test ! -e "$HERDR_RELAY_ENV"

if command -v script >/dev/null 2>&1; then
    : > "$FAKE_LOG"
    answer relay.home.test y n || :
    test ! -e "$HERDR_RELAY_ENV"
    test ! -e "$UNIT"

    : > "$FAKE_LOG"
    (export FAIL_HEALTH=1; answer relay.home.test y y) || :
    grep -q 'previous relay configuration was restored' "$WORK/out"
    test ! -e "$HERDR_RELAY_ENV"
    test ! -e "$UNIT"

    printf "HERDR_GATEWAY_URL='wss://gw.example.test'\nHERDR_GATEWAY_SELECTION='latency'\n" > "$HERDR_RELAY_ENV"
    : > "$FAKE_LOG"
    answer 'relay.home.test:8443' 'https://Relay.Home.Test/' n y
    grep -q 'without a port or path' "$WORK/out"
    grep -q 'replaces the selected WebRTC gateway list: wss://gw.example.test' "$WORK/out"
    grep -q "^HERDR_REVERSE_PROXY_HOST='relay.home.test'" "$HERDR_RELAY_ENV"
    grep -q "^HERDR_RELAY_HOST='0.0.0.0'" "$HERDR_RELAY_ENV"
    assert_absent '^HERDR_GATEWAY_URL=|^HERDR_GATEWAY_SELECTION=|^CLOUDFLARED_CONFIG=' "$HERDR_RELAY_ENV"
    grep -q 'relay=wss://relay.home.test' "$WORK/out"
    test -f "$UNIT"
    assert_absent '^cloudflared ' "$FAKE_LOG"

    : > "$FAKE_LOG"
    answer '' y y
    grep -q "^HERDR_REVERSE_PROXY_HOST='relay.home.test'" "$HERDR_RELAY_ENV"
    grep -q "^HERDR_RELAY_HOST='127.0.0.1'" "$HERDR_RELAY_ENV"

    printf "CLOUDFLARED_CONFIG='%s'\n" "$HOME/cloudflare.yml" >> "$HERDR_RELAY_ENV"
    printf 'hostname: relay.cloudflare.test\n' > "$HOME/cloudflare.yml"
    : > "$FAKE_LOG"
    run reverse-proxy-off.sh
    test -f "$UNIT"
    grep -q 'systemctl --user restart' "$FAKE_LOG"
    grep -q "^CLOUDFLARED_CONFIG=" "$HERDR_RELAY_ENV"
fi
echo 'reverse proxy tests passed'

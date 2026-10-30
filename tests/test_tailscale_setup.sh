#!/usr/bin/env bash
set -euo pipefail
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/herdr-always-ts.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
export HOME="$WORK/home" HERDR_PLUGIN_CONFIG_DIR="$WORK/plugin" HERDR_RELEASE_ROOT="$WORK/release"
export HERDR_RELAY_ENV="$HOME/config/relay.env" HERDR_RELAY_BIN="$WORK/bin/herdr-mobile-relay"
export HERDR_BIN="$WORK/bin/herdr" HERDR_PHONE_APP_URL=relay FAKE_LOG="$WORK/log"
mkdir -p "$WORK/bin" "$HOME/config" "$WORK/release/current" "$HOME/.config/systemd/user" "$HOME/Library/LaunchAgents" "$WORK/plugin"
printf '{"version":"v","revision":"r","web_hash":"h"}\n' > "$WORK/bin/release-manifest.json"
printf '#!/bin/sh\nexit 0\n' > "$HERDR_BIN"
cat > "$HERDR_RELAY_BIN" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$1" in
    tailscale)
        case "$2" in
            check) [ "${FAIL_CHECK:-}" != 1 ] || { echo 'Tailscale needs HTTPS certificates' >&2; exit 7; } ;;
            address) [ "${FAIL_ADDRESS:-}" != 1 ] || { echo 'Tailscale is stopped' >&2; exit 5; }
                echo 'https://sheep.tail.ts.net:8443' ;;
            serve) [ "${FAIL_CONFLICT:-}" != 1 ] || { echo 'HTTPS port 8443 already in use' >&2; exit 9; } ;;
            off) [ "${FAIL_OFF:-}" != 1 ] || { echo 'Tailscale is down' >&2; exit 5; } ;;
        esac ;;
    version) echo '{"version":"v","revision":"r"}' ;;
    setup-fragment) printf 'token=%s&relay=%s\n' "$2" "$4" ;;
    normalize-origin) printf '%s\n' "${@: -1}" ;;
    qr) echo '[fake QR]' ;;
    speech-voices) ;;
    serve) printf 'rearm=%s\n' "${HERDR_RELAY_REARM_BOOTSTRAP:-}" >> "$FAKE_LOG" ;;
esac
EOF
cat > "$WORK/bin/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *gw.example.test/healthz*)
        while [ "$#" -gt 0 ]; do
            if [ "$1" = --output ]; then printf '{"ok":true}\n' > "$2"; echo 0.01; exit 0; fi
            shift
        done
        echo '{"ok":true}'
        exit 0 ;;
esac
[ "${FAIL_HEALTH:-}" != 1 ] || exit 1
instance=$(awk -F "'" '/^HERDR_RELAY_INSTANCE_ID=/ {print $2}' "$HERDR_RELAY_ENV")
printf '{"status":"ready","inventory":{"state":"ready"},"instance":"%s","release_version":"v","revision":"r","bundle_hash":"h","protocol":3}\n' "$instance"
EOF
cat > "$WORK/bin/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$FAKE_LOG"
case "$*" in *is-active*) exit 3 ;; esac
EOF
cat > "$WORK/bin/launchctl" <<'EOF'
#!/bin/sh
printf 'launchctl %s\n' "$*" >> "$FAKE_LOG"
case "$1" in print) case "$2" in gui/*/*) exit 1 ;; esac ;; esac
EOF
cat > "$WORK/bin/cloudflared" <<'EOF'
#!/bin/sh
exit 1
EOF
cat > "$WORK/bin/plutil" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$WORK/bin/id" <<'EOF'
#!/bin/sh
[ "$1" = -u ] && { echo 501; exit; }
/usr/bin/id "$@"
EOF
cat > "$WORK/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$WORK/bin/uname" <<'EOF'
#!/bin/sh
if [ "$1" = -s ]; then echo "${FAKE_PLATFORM:-Linux}"; else /usr/bin/uname "$@"; fi
EOF
chmod +x "$WORK/bin/"*
mkdir -p "$HOME/.local/bin"
for fake in curl systemctl launchctl cloudflared uname id sleep; do
    ln -s "$WORK/bin/$fake" "$HOME/.local/bin/$fake"
done
export PATH="$WORK/bin:$PATH"

run() { "$REPO_DIR/relay/$1" "${@:2}" > "$WORK/out" 2>&1; }
tty_setup() {
    if [ "$(/usr/bin/uname -s)" = Darwin ]; then
        script -q /dev/null "$REPO_DIR/relay/tailscale-setup.sh"
    else
        script -q -e -c "'$REPO_DIR/relay/tailscale-setup.sh'" /dev/null
    fi
}
if run tailscale-setup.sh; then echo 'unattended setup accepted' >&2; exit 1; fi
test ! -e "$HERDR_RELAY_ENV"
! grep -q 'tailscale serve' "$FAKE_LOG"
if (FAIL_CHECK=1 run tailscale-setup.sh); then echo 'failed check accepted' >&2; exit 1; fi
grep -q 'needs HTTPS certificates' "$WORK/out"
printf '10\nq\n' | run plugin-setup-menu.sh
grep -q 'Always on Tailscale' "$WORK/out"
grep -q 'Cancelled; nothing changed' "$WORK/out"
test ! -e "$HERDR_RELAY_ENV"
if command -v script >/dev/null 2>&1; then
    : > "$FAKE_LOG"
    ({ printf 'n\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
    test ! -e "$HERDR_RELAY_ENV"
    ! grep -q 'tailscale serve' "$FAKE_LOG"
    (printf '' | tty_setup) > "$WORK/out" 2>&1 || :
    test ! -e "$HERDR_RELAY_ENV"
    (export FAIL_CONFLICT=1; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
    test ! -e "$HERDR_RELAY_ENV"
    grep -q 'already in use' "$WORK/out"
    ({ printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1
    grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
    grep -q 'serve --bg --port 8443 --relay-port 8375' "$FAKE_LOG"
    grep -q 'relay=wss://sheep.tail.ts.net:8443' "$WORK/out"
    grep -q '\[fake QR\]' "$WORK/out"
    test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    ! grep -q cloudflared "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    printf "CLOUDFLARED_CONFIG='%s'\n" "$HOME/missing-cloudflare.yml" >> "$HERDR_RELAY_ENV"
    ( . "$REPO_DIR/relay/common.sh"; verify_public_readiness "$HERDR_RELAY_ENV" '{}' )
    run plugin-setup-link.sh
    grep -q 'relay=wss://sheep.tail.ts.net:8443' "$WORK/out"
    if (export FAIL_ADDRESS=1; run plugin-setup-link.sh); then echo 'QR printed while Tailscale was down' >&2; exit 1; fi
    grep -q 'Tailscale is stopped' "$WORK/out"
    (export FAIL_HEALTH=1; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
    grep -q 'Tailscale Serve remains configured' "$WORK/out"
    test "$(grep -c '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV")" -eq 1
fi
printf '11\nq\n' | run plugin-setup-menu.sh
grep -q 'off --port 8443 --relay-port 8375' "$FAKE_LOG"
! grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
: > "$FAKE_LOG"
run tailscale-off.sh
! grep -q 'tailscale off' "$FAKE_LOG"
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
(export FAIL_OFF=1; bash -c '. "$1/relay/common.sh"; switch_off_tailscale "$HERDR_RELAY_ENV"' _ "$REPO_DIR") > "$WORK/out" 2>&1
grep -q 'tailscale serve --https=8443 off' "$WORK/out"
! grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
grep -q 'off --port 8443' "$FAKE_LOG"
! grep -q 'off --port 9443' "$FAKE_LOG"
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
(export FAIL_OFF=1; bash -c '. "$1/relay/common.sh"; switch_off_tailscale "$HERDR_RELAY_ENV"; set_gateway_url "$HERDR_RELAY_ENV" wss://gw.example.test' _ "$REPO_DIR") > "$WORK/out" 2>&1
grep -q 'tailscale serve --https=8443 off' "$WORK/out"
! grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
grep -q "HERDR_GATEWAY_URL='wss://gw.example.test'" "$HERDR_RELAY_ENV"
. "$REPO_DIR/relay/common.sh"
set_gateway_url "$HERDR_RELAY_ENV" ""
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
FAKE_PLATFORM=Darwin run install-service.sh || { tail -40 "$WORK/out" >&2; exit 1; }
test -f "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
(run herdr-mobile-relay-service.sh) || :
grep -q 'Starting herdr relay' "$WORK/out"
! grep -q 'Starting cloudflared' "$WORK/out"
grep -q '^rearm=$' "$FAKE_LOG"
run plugin-setup-link.sh
grep -q 'relay=wss://sheep.tail.ts.net:8443' "$WORK/out"
echo 'always-on Tailscale tests passed'

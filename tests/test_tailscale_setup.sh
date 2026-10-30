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
            off) [ "${FAIL_OFF_CONFLICT:-}" != 1 ] || { echo 'HTTPS port 8443 has another mapping' >&2; exit 9; }
                [ "${FAIL_OFF:-}" != 1 ] || { echo 'Tailscale is down' >&2; exit 5; }
                [ "${MISSING_CLI:-}" != 1 ] || { echo 'tailscale CLI not installed' >&2; exit 3; }
                [ -z "${HERDR_TAILSCALE_BIN:-}" ] || { echo 'HERDR_TAILSCALE_BIN must be an absolute executable' >&2; exit 3; } ;;
        esac ;;
    version) echo '{"version":"v","revision":"r"}' ;;
    setup-fragment) [ "${FAIL_LINK:-}" != 1 ] || exit 8
        printf 'token=%s&relay=%s\n' "$2" "$4" ;;
    normalize-origin) printf '%s\n' "${@: -1}" ;;
    qr) [ "${FAIL_QR:-}" != 1 ] || exit 8
        echo '[fake QR]' ;;
    speech-voices) [ "${FAIL_PREREQ:-}" != 1 ] || exit 8 ;;
    serve) printf 'rearm=%s\n' "${HERDR_RELAY_REARM_BOOTSTRAP:-}" >> "$FAKE_LOG" ;;
esac
EOF
cat > "$WORK/bin/curl" <<'EOF'
#!/bin/sh
printf 'curl %s\n' "$*" >> "$FAKE_LOG"
case "$*" in
    *gw.example.test/healthz*)
        [ "${FAIL_GATEWAY:-}" != 1 ] || exit 1
        while [ "$#" -gt 0 ]; do
            if [ "$1" = --output ]; then printf '{"ok":true}\n' > "$2"; echo 0.01; exit 0; fi
            shift
        done
        echo '{"ok":true}'
        exit 0 ;;
esac
[ "${FAIL_HEALTH:-}" != 1 ] || exit 1
instance=$(awk -F "'" '/^HERDR_RELAY_INSTANCE_ID=/ {print $2}' "$HERDR_RELAY_ENV")
printf '{"status":"ready","inventory":{"state":"ready"},"gateway":{"registered":true},"instance":"%s","release_version":"v","revision":"r","bundle_hash":"h","protocol":3}\n' "$instance"
EOF
cat > "$WORK/bin/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$FAKE_LOG"
case "$*" in *is-active*) exit 3 ;; *restart*) [ "${FAIL_SERVICE:-}" != 1 ] || exit 8 ;; esac
EOF
cat > "$WORK/bin/launchctl" <<'EOF'
#!/bin/sh
printf 'launchctl %s\n' "$*" >> "$FAKE_LOG"
case "$1" in print) case "$2" in gui/*/*) exit 1 ;; esac ;; esac
EOF
cat > "$WORK/bin/cloudflared" <<'EOF'
#!/bin/sh
printf 'cloudflared %s\n' "$*" >> "$FAKE_LOG"
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
assert_absent() {
    if grep -Eq "$1" "$2"; then
        echo "Unexpected $1 in $2" >&2
        exit 1
    fi
}
tty_setup() {
    if [ "$(/usr/bin/uname -s)" = Darwin ]; then
        script -q /dev/null "$REPO_DIR/relay/tailscale-setup.sh"
    else
        script -q -e -c "'$REPO_DIR/relay/tailscale-setup.sh'" /dev/null
    fi
}
if run tailscale-setup.sh; then echo 'unattended setup accepted' >&2; exit 1; fi
test ! -e "$HERDR_RELAY_ENV"
assert_absent 'tailscale serve' "$FAKE_LOG"
if (FAIL_CHECK=1 run tailscale-setup.sh); then echo 'failed check accepted' >&2; exit 1; fi
grep -q 'needs HTTPS certificates' "$WORK/out"
printf "HERDR_GATEWAY_URL='wss://gw.example.test'\n" > "$HERDR_RELAY_ENV"
: > "$FAKE_LOG"
if run tailscale-setup.sh; then echo 'gateway selected setup accepted' >&2; exit 1; fi
grep -q 'Switch to a Cloudflare tunnel first' "$WORK/out"
assert_absent . "$FAKE_LOG"
grep -q '^HERDR_GATEWAY_URL=' "$HERDR_RELAY_ENV"
mkdir -p "$WORK/stable-wrapper"
cp "$REPO_DIR/relay/plugin-install-service.sh" "$REPO_DIR/relay/common.sh" "$WORK/stable-wrapper/"
printf '#!/bin/sh\nexit 1\n' > "$WORK/stable-wrapper/stable-setup.sh"
chmod +x "$WORK/stable-wrapper/stable-setup.sh"
if "$WORK/stable-wrapper/plugin-install-service.sh" > "$WORK/out" 2>&1; then
    echo 'fake stable setup unexpectedly succeeded' >&2; exit 1
fi
grep -q 'Switching this relay from the WebRTC gateway to Cloudflare' "$WORK/out"
assert_absent '^HERDR_GATEWAY_URL=' "$HERDR_RELAY_ENV"
printf "CLOUDFLARED_CONFIG='%s'\n" "$HOME/cloudflare.yml" >> "$HERDR_RELAY_ENV"
printf 'hostname: relay.cloudflare.test\n' > "$HOME/cloudflare.yml"
run service.sh install
grep -q "CLOUDFLARED_CONFIG='" "$HERDR_RELAY_ENV"
assert_absent '^HERDR_GATEWAY_URL=' "$HERDR_RELAY_ENV"
run service.sh uninstall
rm -f "$HERDR_RELAY_ENV"

if command -v script >/dev/null 2>&1; then
    : > "$FAKE_LOG"
    ({ printf 'n\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
    test ! -e "$HERDR_RELAY_ENV"
    assert_absent 'tailscale serve' "$FAKE_LOG"
    for failure in FAIL_PREREQ FAIL_SERVICE FAIL_HEALTH FAIL_CONFLICT; do
        : > "$FAKE_LOG"
        (export "$failure=1"; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
        test ! -e "$HERDR_RELAY_ENV"
        test ! -e "$HOME/.config/systemd/user/herdr-mobile-relay.service"
        if [ "$failure" = FAIL_CONFLICT ]; then
            grep -q 'tailscale off --port 8443' "$FAKE_LOG"
        else
            assert_absent 'tailscale off' "$FAKE_LOG"
        fi
        if [ "$failure" = FAIL_PREREQ ]; then
            assert_absent 'systemctl .* (stop|disable)' "$FAKE_LOG"
        else
            grep -q 'systemctl --user disable --now' "$FAKE_LOG" || grep -q 'systemctl --user stop' "$FAKE_LOG"
        fi
        if [ "$failure" != FAIL_CONFLICT ]; then assert_absent 'tailscale serve --bg' "$FAKE_LOG"; fi
    done

    printf "CLOUDFLARED_CONFIG='%s'\nHERDR_RELAY_TOKEN='saved'\n" "$HOME/cloudflare.yml" > "$HERDR_RELAY_ENV"
    printf 'hostname: relay.cloudflare.test\n' > "$HOME/cloudflare.yml"
    run service.sh install
    cp "$HERDR_RELAY_ENV" "$WORK/prior"
    test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    : > "$FAKE_LOG"
    (export FAIL_HEALTH=1; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1 || :
    cmp "$HERDR_RELAY_ENV" "$WORK/prior"
    [ "$(grep -c 'systemctl --user restart' "$FAKE_LOG")" -ge 2 ]
    test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    for failure in FAIL_LINK FAIL_QR; do
        : > "$FAKE_LOG"
        if (export "$failure=1"; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1; then
            echo 'failed QR accepted' >&2; exit 1
        fi
        grep -q 'Always on Tailscale is set up, but the QR could not be shown' "$WORK/out"
        grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
        test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
        assert_absent 'tailscale off' "$FAKE_LOG"
        : > "$FAKE_LOG"
        if run tailscale-setup.sh; then echo 'selected setup accepted' >&2; exit 1; fi
        grep -q "Always on Tailscale is already set up. Use 'Show setup QR', or 'Turn off Tailscale' first." "$WORK/out"
        assert_absent . "$FAKE_LOG"
        run tailscale-off.sh
        cmp "$HERDR_RELAY_ENV" "$WORK/prior" || { echo 'turn-off did not restore service form' >&2; exit 1; }
    done
    : > "$FAKE_LOG"
    if (export FAIL_CONFLICT=1 FAIL_OFF=1; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1; then
        echo 'failed publish cleanup accepted' >&2; exit 1
    fi
    grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
    grep -q 'tailscale serve --https=8443 off' "$WORK/out"
    grep -q 'HERDR_TAILSCALE_ABANDON=1' "$WORK/out"
    test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    assert_absent 'systemctl --user disable --now' "$FAKE_LOG"
    run tailscale-off.sh
    for failure in MISSING_CLI HERDR_TAILSCALE_BIN; do
        : > "$FAKE_LOG"
        if (export FAIL_CONFLICT=1; export "$failure=1"; { printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1; then
            echo "failed publish cleanup accepted: $failure" >&2; exit 1
        fi
        grep -q 'mapping may remain.*selection kept' "$WORK/out"
        grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
        test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
        run tailscale-off.sh
    done
    : > "$FAKE_LOG"
    ({ printf 'y\n'; /bin/sleep 1; } | tty_setup) > "$WORK/out" 2>&1
    grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
    grep -q 'serve --bg --port 8443 --relay-port 8375' "$FAKE_LOG"
    publish_line="$(grep -n 'serve --bg --port 8443 --relay-port 8375' "$FAKE_LOG" | tail -1 | cut -d: -f1)"
    health_line="$(grep -n 'curl .*127.0.0.1:8375/readyz' "$FAKE_LOG" | tail -1 | cut -d: -f1)"
    [ "$publish_line" -gt "$health_line" ] || { echo 'Tailscale published before health' >&2; exit 1; }
    grep -q 'relay=wss://sheep.tail.ts.net:8443' "$WORK/out"
    test -f "$HOME/.config/systemd/user/herdr-mobile-relay.service"
    assert_absent cloudflared "$HOME/.config/systemd/user/herdr-mobile-relay.service"
fi

cp "$HERDR_RELAY_ENV" "$WORK/selected"
run service.sh install
cmp "$HERDR_RELAY_ENV" "$WORK/selected"
grep -q 'systemctl --user restart' "$FAKE_LOG"

refusal="Always on Tailscale is selected. Choose 'Turn off Tailscale' in the setup menu first."
for entry in 'stable-setup.sh' 'gateway-deploy.sh' 'plugin-choose-transport.sh temporary' 'plugin-choose-transport.sh community' 'plugin-choose-transport.sh own' 'plugin-choose-transport.sh stable' 'plugin-install-service.sh' 'plugin-quick-start.sh' 'setup.sh' 'setup.sh --install-missing' 'start.sh' 'stable-teardown.sh' 'change-hostname.sh' 'service.sh uninstall' 'uninstall-systemd-user-service.sh' 'uninstall-service.sh'; do
    : > "$FAKE_LOG"
    cp "$HERDR_RELAY_ENV" "$WORK/before"
    read -r -a args <<< "$entry"
    if run "${args[@]}"; then echo "$entry accepted Tailscale selection" >&2; exit 1; fi
    grep -Fq "$refusal" "$WORK/out" || { echo "$entry missing refusal" >&2; exit 1; }
    cmp "$HERDR_RELAY_ENV" "$WORK/before"
    assert_absent . "$FAKE_LOG"
done

: > "$FAKE_LOG"
(FAIL_OFF=1 run tailscale-off.sh) && { echo 'failed off accepted' >&2; exit 1; }
grep -q 'tailscale serve --https=8443 off' "$WORK/out"
grep -q 'exit 5' "$WORK/out"
grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
assert_absent 'systemctl' "$FAKE_LOG"
(FAIL_OFF_CONFLICT=1 run tailscale-off.sh) && { echo 'conflicting mapping cleared selection' >&2; exit 1; }
grep -q 'mapping may remain, selection kept' "$WORK/out"
grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
run tailscale-off.sh
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
for failure in MISSING_CLI HERDR_TAILSCALE_BIN; do
    : > "$FAKE_LOG"
    if (export "$failure=1"; run tailscale-off.sh); then
        echo "$failure cleared selection without removing Serve" >&2; exit 1
    fi
    grep -q 'mapping may remain.*selection kept' "$WORK/out"
    grep -q '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
    assert_absent 'systemctl' "$FAKE_LOG"
done
run tailscale-off.sh
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
: > "$FAKE_LOG"
run service.sh uninstall
grep -q 'systemctl --user disable --now' "$FAKE_LOG"
run service.sh install
(CLOUDFLARED_BIN="$WORK/bin/cloudflared" run herdr-mobile-relay-service.sh) || :
grep -q 'Starting cloudflared' "$WORK/out"
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
(FAIL_OFF=1 HERDR_TAILSCALE_ABANDON=1 run tailscale-off.sh)
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
grep -q 'mapping may remain' "$WORK/out"

printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
printf '11\nq\n' | run plugin-setup-menu.sh
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
grep -q 'systemctl .* restart' "$FAKE_LOG" || grep -q 'systemctl .* start' "$FAKE_LOG"
. "$REPO_DIR/relay/common.sh"
remove_env_value_atomic "$HERDR_RELAY_ENV" CLOUDFLARED_CONFIG
set_env_value_atomic "$HERDR_RELAY_ENV" HERDR_GATEWAY_URL wss://gw.example.test
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
printf "CLOUDFLARED_CONFIG='%s'\n" "$HOME/cloudflare.yml" >> "$HERDR_RELAY_ENV"
run tailscale-off.sh
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
grep -q 'systemctl .* restart' "$FAKE_LOG"
: > "$FAKE_LOG"
(CLOUDFLARED_BIN="$WORK/bin/cloudflared" run herdr-mobile-relay-service.sh) || :
grep -q 'Starting cloudflared' "$WORK/out"
remove_env_value_atomic "$HERDR_RELAY_ENV" HERDR_GATEWAY_URL
remove_env_value_atomic "$HERDR_RELAY_ENV" CLOUDFLARED_CONFIG
printf "HERDR_TAILSCALE_PORT='8443'\n" >> "$HERDR_RELAY_ENV"
: > "$FAKE_LOG"
run tailscale-off.sh
grep -q 'systemctl --user disable --now' "$FAKE_LOG" || grep -q 'systemctl --user stop' "$FAKE_LOG"
assert_absent '^HERDR_TAILSCALE_PORT=' "$HERDR_RELAY_ENV"
echo 'always-on Tailscale tests passed'

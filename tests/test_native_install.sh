#!/usr/bin/env bash
set -euo pipefail
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-native-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT
export NATIVE_HELPER="$WORK_DIR/helper"
go build -o "$NATIVE_HELPER" "$REPO_DIR/cmd/herdr-mobile-relay"
# plutil ships only with macOS. Elsewhere the launchd cases get a stand-in
# that still rejects a missing or empty staged plist.
HOST_PLUTIL="$(command -v plutil || true)"

for platform in systemd launchd; do
  for initial in active inactive fresh; do
    for failure in none write reload start readiness public rollback; do
     for envstate in preset absent no-instance; do
        # A host that has never been configured reaches the installer with no
        # relay.env, or one without an instance identity, so the readiness
        # gate must read the identity the installer itself just generated.
        case "$envstate:$failure" in
            preset:*) ;;
            *:none|*:rollback) ;;
            *) continue ;;
        esac
        CASE="$WORK_DIR/$platform-$initial-$failure-$envstate home & space"
        export HOME="$CASE" CASE FAILURE="$failure" INITIAL="$initial"
        export XDG_STATE_HOME="$HOME/state"
        BIN="$HOME/.local/bin"
        mkdir -p "$BIN" "$HOME/config" "$HOME/releases/current/relay" "$HOME/releases/current/web" "$HOME/.config/systemd/user" "$HOME/Library/LaunchAgents" "$HOME/tmp"
        export TMPDIR="$HOME/tmp/"
        export HERDR_RELEASE_ROOT="$HOME/releases"
        export HERDR_RELAY_ENV="$HOME/config/relay.env"
        case "$envstate" in
            preset)
                printf 'HERDR_RELAY_TOKEN=test\nHERDR_RELAY_INSTANCE_ID=instance\nHERDR_RELAY_PORT=18375\nCLOUDFLARED_CONFIG=%q\n' "$HOME/config/tunnel.yml" > "$HERDR_RELAY_ENV"
                ;;
            no-instance)
                printf 'HERDR_RELAY_TOKEN=test\nHERDR_RELAY_PORT=18375\nCLOUDFLARED_CONFIG=%q\n' "$HOME/config/tunnel.yml" > "$HERDR_RELAY_ENV"
                ;;
            absent)
                rm -f "$HERDR_RELAY_ENV"
                export CLOUDFLARED_CONFIG="$HOME/config/tunnel.yml"
                ;;
        esac
        printf 'hostname: relay.example.test\n' > "$HOME/config/tunnel.yml"
        printf '#!/bin/sh\nexit 0\n' > "$HOME/releases/current/relay/herdr-mobile-relay-service.sh"
        chmod 700 "$HOME/releases/current/relay/herdr-mobile-relay-service.sh"
        cp "$REPO_DIR/web/release.json" "$HOME/releases/current/web/release.json"
        printf '{"version":"version","revision":"revision","web_hash":"web"}\n' \
            > "$HOME/releases/current/release-manifest.json"
        cat > "$HOME/releases/current/herdr-mobile-relay" <<'EOF'
#!/bin/sh
case "$1" in
version) printf '{"version":"version","revision":"revision"}\n' ;;
verify-readiness) exec "$NATIVE_HELPER" "$@" ;;
*) exit 1 ;;
esac
EOF
        cat > "$BIN/systemctl" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "$CASE/manager.log"
case "$*" in
*is-active*) [ "$INITIAL" = active ] && exit 0; exit 3 ;;
*is-enabled*) [ "$INITIAL" = active ] && exit 0; exit 1 ;;
*stop*) [ "$FAILURE" != rollback ] || exit 1; exit 0 ;;
*daemon-reload*) step=reload ;;
*restart*) step=start ;;
*) exit 0 ;;
esac
if [ "$FAILURE" = "$step" ] || { [ "$FAILURE" = rollback ] && [ "$step" = start ]; }; then
    if [ ! -f "$CASE/failed" ] || [ "$FAILURE" = rollback ]; then touch "$CASE/failed"; exit 1; fi
fi
EOF
        cat > "$BIN/launchctl" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "$CASE/manager.log"
if [ "$1" = print ]; then
    case "$2" in gui/*/*) ;; gui/*) exit 0 ;; esac
fi
loaded="$CASE/loaded"
case "$*" in *herdr-remote*) loaded="$CASE/legacy-loaded" ;; esac
case "$1" in
print) [ -f "$loaded" ]; exit $? ;;
bootout) if [ "$FAILURE" = rollback ] && [ -f "$CASE/failed" ]; then exit 1; fi; rm -f "$loaded" ;;
bootstrap)
    if [ "$FAILURE" = reload ] && ! grep -q 'previous definition' "$3"; then touch "$CASE/failed"; exit 1; fi
    touch "$loaded"
    ;;
kickstart)
    if [ "$FAILURE" = start ] || [ "$FAILURE" = rollback ]; then touch "$CASE/failed"; exit 1; fi
    ;;
*) exit 0 ;;
esac
if [ "$FAILURE" = rollback ] && [ "$1" = bootstrap ] && [ -f "$CASE/failed" ]; then exit 1; fi
EOF
        # The fake relay answers with whatever instance identity the env file
        # currently records, so a generated identity is matched like a
        # preconfigured one.
        cat > "$BIN/curl" <<'EOF'
#!/bin/sh
status=ready
revision=revision
instance=$(sed -n "s/^HERDR_RELAY_INSTANCE_ID=['\"]*\([^'\"]*\)['\"]*$/\1/p" "$HERDR_RELAY_ENV" 2>/dev/null | tail -1)
[ "$FAILURE" != readiness ] || status=degraded
case "$*" in *https://*) [ "$FAILURE" != public ] || revision=foreign ;; esac
printf '{"status":"ready","inventory":{"state":"%s"},"instance":"%s","release_version":"version","revision":"%s","bundle_hash":"web","protocol":3}\n' "$status" "$instance" "$revision"
EOF
        cat > "$BIN/cp" <<'EOF'
#!/bin/sh
if [ "$FAILURE" = write ]; then
    case "$*" in *.herdr-service.*) exit 1 ;; esac
fi
exec /bin/cp "$@"
EOF
        for command in cloudflared sleep systemd-analyze; do printf '#!/bin/sh\nexit 0\n' > "$BIN/$command"; done
        if [ -z "$HOST_PLUTIL" ]; then
            printf '#!/bin/sh\n[ "$1" = -lint ] && [ -s "$2" ]\n' > "$BIN/plutil"
        fi
        cat > "$BIN/id" <<'EOF'
#!/bin/sh
if [ "$1" = -u ]; then printf '501\n'; else /usr/bin/id "$@"; fi
EOF
        chmod 700 "$BIN/"* "$HOME/releases/current/herdr-mobile-relay"
        export PATH="$BIN:$PATH"
        if [ "$initial" = active ]; then touch "$CASE/loaded" "$CASE/legacy-loaded"; fi
        if [ "$platform" = systemd ]; then
            definition="$HOME/.config/systemd/user/herdr-mobile-relay.service"
            legacy="$HOME/.config/systemd/user/herdr-remote.service"
            script=install-systemd-user-service.sh
        else
            definition="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
            legacy="$HOME/Library/LaunchAgents/com.herdr-remote.service.plist"
            script=install-service.sh
        fi
        if [ "$initial" != fresh ]; then
            printf 'previous definition\n' > "$definition"
            printf 'legacy definition\n' > "$legacy"
        fi
        if [ -f "$HERDR_RELAY_ENV" ]; then cp "$HERDR_RELAY_ENV" "$CASE/env-before"; fi
        if bash "$REPO_DIR/relay/$script" > "$CASE/output" 2>&1; then
            if [ "$failure" != none ]; then echo "$platform accepted $failure failure" >&2; exit 1; fi
            test ! -e "$legacy"
            test -n "$(sed -n 's/^HERDR_RELAY_INSTANCE_ID=//p' "$HERDR_RELAY_ENV")"
        else
            if [ "$failure" = none ]; then cat "$CASE/output" >&2; exit 1; fi
            if [ "$initial" = fresh ]; then
                test ! -e "$definition"
                test ! -e "$legacy"
            else
                grep -Fx 'previous definition' "$definition" >/dev/null
                grep -Fx 'legacy definition' "$legacy" >/dev/null
            fi
            if [ -f "$CASE/env-before" ]; then
                cmp "$HERDR_RELAY_ENV" "$CASE/env-before"
            else
                test ! -e "$HERDR_RELAY_ENV"
            fi
            if [ "$failure" = rollback ]; then
                grep -F 'rollback is incomplete' "$CASE/output" >/dev/null
                test -n "$(find "$HOME/state" -name herdr-native-recovery.\* -type d -print -quit)"
            fi
        fi
     done
    done
  done
done
printf 'native installation transaction fixtures passed\n'

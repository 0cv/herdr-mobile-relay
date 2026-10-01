#!/bin/sh
# Verify and exercise version, serve, and support from a native release.
set -eu

ARCHIVE=${1:-}
CHECKSUMS=${2:-}
EXPECTED_VERSION=${3:-}
EXPECTED_REVISION=${4:-}
EXPECTED_TARGET=${5:-}
[ -n "$ARCHIVE" ] && [ -f "$ARCHIVE" ] &&
    [ -n "$CHECKSUMS" ] && [ -f "$CHECKSUMS" ] &&
    [ -n "$EXPECTED_VERSION" ] && [ -n "$EXPECTED_REVISION" ] &&
    [ -n "$EXPECTED_TARGET" ] || {
    echo "usage: scripts/check-installed-release.sh ARCHIVE CHECKSUMS VERSION REVISION os/arch" >&2
    exit 2
}

case $(uname -s) in
    Linux) HOST_OS=linux ;;
    Darwin) HOST_OS=darwin ;;
    *)
        echo "unsupported release-check operating system: $(uname -s)" >&2
        exit 1
        ;;
esac
case $(uname -m) in
    x86_64|amd64) HOST_ARCH=amd64 ;;
    arm64|aarch64) HOST_ARCH=arm64 ;;
    *)
        echo "unsupported release-check architecture: $(uname -m)" >&2
        exit 1
        ;;
esac
HOST_TARGET="$HOST_OS/$HOST_ARCH"
[ "$EXPECTED_TARGET" = "$HOST_TARGET" ] || {
    echo "expected release target $EXPECTED_TARGET does not match native target $HOST_TARGET" >&2
    exit 1
}
ARCHIVE_NAME=${ARCHIVE##*/}
EXPECTED_NAME="herdr-mobile-relay_${EXPECTED_VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
[ "$ARCHIVE_NAME" = "$EXPECTED_NAME" ] || {
    echo "release archive $ARCHIVE_NAME does not match candidate $EXPECTED_NAME" >&2
    exit 1
}

EXPECTED_HASH=$(
    awk -v name="$ARCHIVE_NAME" '
        NF == 2 && $2 == name { count++; hash = $1 }
        END {
            if (count != 1) {
                exit 1
            }
            print hash
        }
    ' "$CHECKSUMS"
) || {
    echo "checksums.txt must contain exactly one entry for $ARCHIVE_NAME" >&2
    exit 1
}
[ "${#EXPECTED_HASH}" -eq 64 ] || {
    echo "invalid SHA-256 for $ARCHIVE_NAME" >&2
    exit 1
}
case "$EXPECTED_HASH" in
    *[!0-9a-f]*)
        echo "invalid SHA-256 for $ARCHIVE_NAME" >&2
        exit 1
        ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    ACTUAL_HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
[ "$ACTUAL_HASH" = "$EXPECTED_HASH" ] || {
    echo "checksum mismatch for $ARCHIVE_NAME" >&2
    exit 1
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-installed-check.XXXXXX")
RELAY_PID=
cleanup() {
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill -INT "$RELAY_PID" 2>/dev/null || true
        wait "$RELAY_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT INT TERM

RELEASE_DIR="$WORK_DIR/release"
CONFIG_HOME="$WORK_DIR/config"
CACHE_HOME="$WORK_DIR/cache"
DATA_HOME="$WORK_DIR/data"
mkdir -p "$RELEASE_DIR" "$CONFIG_HOME" "$CACHE_HOME" "$DATA_HOME"
# macOS exposes /var through /private/var, and os.Executable reports the
# physical path. Use the same canonical directory for extraction, execution,
# and support-state verification.
RELEASE_DIR=$(CDPATH='' cd "$RELEASE_DIR" && pwd -P)
tar -xzf "$ARCHIVE" -C "$RELEASE_DIR"

RELAY="$RELEASE_DIR/herdr-mobile-relay"
[ -x "$RELAY" ] || {
    echo "release does not contain an executable relay" >&2
    exit 1
}
for WRAPPER in tailscale.sh tailscale-external.sh tailscale-cli.sh tailscale-cli-service.sh; do
    [ -x "$RELEASE_DIR/relay/$WRAPPER" ] || {
        echo "release does not contain executable $WRAPPER" >&2
        exit 1
    }
done
"$RELAY" verify-release \
    --target "$EXPECTED_TARGET" \
    --version "$EXPECTED_VERSION" \
    --revision "$EXPECTED_REVISION" \
    "$RELEASE_DIR" >/dev/null

# Exercise the exact extracted CLI service dispatch with an inert relay fixture.
# Copying the packaged wrappers under a manifest-free temporary root guarantees
# relay_binary cannot prefer or execute the extracted production binary.
CLI_FIXTURE_ROOT="$WORK_DIR/cli-service-fixture"
CLI_FIXTURE_HOME="$WORK_DIR/cli-service-home"
CLI_FIXTURE_ENV="$WORK_DIR/cli-service.env"
CLI_FIXTURE_RECORD="$WORK_DIR/cli-service.record"
CLI_FIXTURE_RELAY="$WORK_DIR/cli-fake-relay"
CLI_FIXTURE_SENTINEL="$WORK_DIR/cli-invoked"
mkdir -p "$CLI_FIXTURE_ROOT/relay" "$CLI_FIXTURE_HOME"
for WRAPPER in common.sh herdr-mobile-relay-service.sh tailscale-cli-service.sh; do
    cp "$RELEASE_DIR/relay/$WRAPPER" "$CLI_FIXTURE_ROOT/relay/$WRAPPER"
done
cat > "$CLI_FIXTURE_ENV" <<'EOF'
HERDR_RELAY_TRANSPORT='tailscale-cli'
HERDR_RELAY_INSTANCE_ID='release-fixture-instance'
HERDR_RELAY_PORT='18377'
HERDR_TAILSCALE_CLI_SCOPE='development'
EOF
CLI_FIXTURE_CLI="$WORK_DIR/fake-tailscale"
printf '#!/bin/sh\n: > "$HERDR_CLI_SENTINEL"\nexit 97\n' > "$CLI_FIXTURE_CLI"
chmod 700 "$CLI_FIXTURE_CLI"
cat > "$CLI_FIXTURE_RELAY" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$HERDR_CLI_SERVICE_RECORD"
case "$*" in 'tailscale-cli activation-check') exit 0 ;; esac
echo unexpected service relay command >&2
exit 97
EOF
chmod 700 "$CLI_FIXTURE_RELAY"
HOME="$CLI_FIXTURE_HOME" \
PATH="/usr/bin:/bin" \
HERDR_RELAY_ENV="$CLI_FIXTURE_ENV" \
HERDR_RELAY_BIN="$CLI_FIXTURE_RELAY" \
HERDR_CLI_SERVICE_RECORD="$CLI_FIXTURE_RECORD" \
HERDR_CLI_SENTINEL="$CLI_FIXTURE_SENTINEL" \
HERDR_TAILSCALE_CLI_BIN="$CLI_FIXTURE_CLI" \
"$CLI_FIXTURE_ROOT/relay/herdr-mobile-relay-service.sh" >"$WORK_DIR/cli-service.log" 2>&1 || {
    echo "extracted CLI service dispatch failed" >&2
    sed -n '1,80p' "$WORK_DIR/cli-service.log" >&2
    exit 1
}
[ "$(sed -n '1p' "$CLI_FIXTURE_RECORD")" = 'tailscale-cli activation-check' ] &&
    [ "$(wc -l < "$CLI_FIXTURE_RECORD" | tr -d ' ')" = 1 ] &&
    [ ! -e "$CLI_FIXTURE_SENTINEL" ] &&
    grep -Fq 'Installed-service CLI startup is disabled' "$WORK_DIR/cli-service.log" || {
    echo "extracted CLI service did not remain disabled before CLI execution" >&2
    exit 1
}

bash "$RELEASE_DIR/relay/pi-commands.sh" install "$WORK_DIR/pi-agent" >/dev/null
cmp "$RELEASE_DIR/relay/pi-command-bridge/bridge.mjs" \
    "$WORK_DIR/pi-agent/extensions/herdr-mobile-relay-commands/bridge.mjs"
bash "$RELEASE_DIR/relay/pi-commands.sh" remove "$WORK_DIR/pi-agent" >/dev/null
[ ! -e "$WORK_DIR/pi-agent/extensions/herdr-mobile-relay-commands" ]

PORT=$((40000 + ($$ % 20000)))
PLUGIN_PORT=$((PORT + 1))
XDG_CONFIG_HOME="$CONFIG_HOME" \
XDG_CACHE_HOME="$CACHE_HOME" \
XDG_DATA_HOME="$DATA_HOME" \
HERDR_RELAY_PORT="$PORT" \
HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT" \
HERDR_RELAY_HOST=127.0.0.1 \
HERDR_RELAY_TOKEN='' \
HERDR_BIN=/bin/false \
HERDR_WEB_ROOT="$RELEASE_DIR/web" \
"$RELAY" serve >"$WORK_DIR/relay.log" 2>&1 &
RELAY_PID=$!

SUPPORT_STATE="$CONFIG_HOME/herdr-mobile-relay/support-state.json"
attempt=0
while [ ! -s "$SUPPORT_STATE" ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -gt 100 ] || ! kill -0 "$RELAY_PID" 2>/dev/null; then
        echo "installed relay did not produce a support snapshot" >&2
        sed -n '1,120p' "$WORK_DIR/relay.log" >&2
        exit 1
    fi
    sleep 0.05
done

XDG_CONFIG_HOME="$CONFIG_HOME" \
XDG_CACHE_HOME="$CACHE_HOME" \
XDG_DATA_HOME="$DATA_HOME" \
"$RELAY" support >"$WORK_DIR/support.json"
grep -q '"protocol": 3' "$WORK_DIR/support.json" || {
    echo "installed relay support output does not report protocol 3" >&2
    sed -n '1,120p' "$WORK_DIR/support.json" >&2
    exit 1
}
grep -qF "\"release_directory\": \"$RELEASE_DIR\"" "$WORK_DIR/support.json" || {
    echo "installed relay support output does not report the canonical release directory" >&2
    sed -n '1,120p' "$WORK_DIR/support.json" >&2
    exit 1
}

kill -INT "$RELAY_PID"
wait "$RELAY_PID"
RELAY_PID=

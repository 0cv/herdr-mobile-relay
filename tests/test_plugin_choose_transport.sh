#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-choose-transport-test.XXXXXX")"
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

# Each flattened transport entry is complete: Community accepts the managed
# candidate list without asking for addresses, retains an unavailable cold
# fallback, saves the latency policy, and immediately enters Quick Start.
CHOOSER_BIN_DIR="$WORK_DIR/chooser-bin"
CHOOSER_SCRIPT_DIR="$WORK_DIR/chooser-scripts"
CHOOSER_ENV="$WORK_DIR/config/chooser.env"
CHOOSER_START_MARKER="$WORK_DIR/chooser-started"
mkdir -p "$CHOOSER_BIN_DIR" "$CHOOSER_SCRIPT_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-choose-transport.sh" \
    "$CHOOSER_SCRIPT_DIR/"
write_stub "$CHOOSER_BIN_DIR/curl" <<'EOF'
#!/bin/sh
# Faithful enough for the probe: the health check reads the body from stdout,
# and the latency probe asks for the body in --output with the timing on
# stdout. A double that answered only the first shape would report every
# gateway unavailable.
OUT=""
PREV=""
for ARG in "$@"; do
    [ "$PREV" != "--output" ] || OUT="$ARG"
    PREV="$ARG"
done
case "$*" in
    *own.example.test/healthz* | *gw-a.example.test/healthz*)
        if [ -n "$OUT" ]; then
            printf '%s\n' '{"ok":true}' > "$OUT"
            printf '0.042'
        else
            printf '%s\n' '{"ok":true}'
        fi
        ;;
    *)
        exit 22
        ;;
esac
EOF
CHOOSER_TRANSPORT_MARKER="$WORK_DIR/chooser-transport"
export CHOOSER_TRANSPORT_MARKER
write_stub "$CHOOSER_SCRIPT_DIR/plugin-quick-start.sh" <<'EOF'
#!/bin/sh
if [ "${HERDR_GATEWAY_URL+x}" = x ]; then
    printf 'gateway=%s\n' "$HERDR_GATEWAY_URL" > "$CHOOSER_TRANSPORT_MARKER"
else
    printf 'unset\n' > "$CHOOSER_TRANSPORT_MARKER"
fi
printf 'started\n' > "$CHOOSER_START_MARKER"
EOF
export CHOOSER_START_MARKER
# The setup menu loads relay.env before running an action, so a policy left by
# an earlier own-gateway setup is already exported here. Choosing Community has
# to reset it: these candidates are interchangeable, and inheriting `ordered`
# would silently pin the pool to whichever entry happens to be listed first.
CHOOSER_OUTPUT="$(
    PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_GATEWAY_SELECTION="ordered" \
        HERDR_COMMUNITY_GATEWAY_URL="gw-a.example.test,https://gw-b.example.test" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" community
)"
test -f "$CHOOSER_START_MARKER"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_URL)" = \
    "wss://gw-a.example.test,wss://gw-b.example.test"
# The published candidates are interchangeable, so this is the one option that
# asks for latency ranking.
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_SELECTION)" = "latency"
case "$CHOOSER_OUTPUT" in
    *"gw-b.example.test.. unavailable"*"Saved 2 gateway candidates."*) ;;
    *)
        echo "community chooser did not report the saved list and unavailable fallback" >&2
        exit 1
        ;;
esac

# The existing-gateway path ends with the complete subscription list too. An
# empty final answer accepts own-first plus all published community fallbacks.
rm -f "$CHOOSER_START_MARKER"
: > "$CHOOSER_ENV"
OWN_OUTPUT="$(
    printf 'b\nown.example.test\n\n' |
        PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_COMMUNITY_GATEWAY_URL="gw-a.example.test,https://gw-b.example.test" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" own
)"
test -f "$CHOOSER_START_MARKER"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_URL)" = \
    "wss://own.example.test,wss://gw-a.example.test,wss://gw-b.example.test"
test "$(env_file_value "$CHOOSER_ENV" HERDR_GATEWAY_SELECTION)" = "ordered"
case "$OWN_OUTPUT" in
    *"Saved 3 gateway candidates."*"first healthy one in that order."*) ;;
    *) echo "own gateway chooser did not save the explicit fallback list" >&2; exit 1 ;;
esac

# Clearing the gateway file must also clear an inherited gateway variable
# before Quick Start builds the setup fragment. Otherwise the background relay
# switches to Cloudflare while the QR still sends the phone to the old gateway.
TEMPORARY_OUTPUT="$(
    PATH="$CHOOSER_BIN_DIR:$PATH" \
        HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_RELAY_ENV="$CHOOSER_ENV" \
        HERDR_GATEWAY_URL="wss://stale.example.test" \
        HERDR_GATEWAY_SELECTION="latency" \
        bash "$CHOOSER_SCRIPT_DIR/plugin-choose-transport.sh" temporary
)"
test "$(cat "$CHOOSER_TRANSPORT_MARKER")" = "unset"
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$CHOOSER_ENV"; then
    echo "temporary tunnel selection left the gateway configured" >&2
    exit 1
fi
expect "$TEMPORARY_OUTPUT" "Temporary Cloudflare tunnel selected."

echo "plugin-choose-transport shell tests passed"

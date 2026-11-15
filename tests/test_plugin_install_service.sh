#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-install-service-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

unset GH_TOKEN GITHUB_TOKEN
mkdir -p "$WORK_DIR/config"

# The top-level Stable Tunnel action clears a gateway before stable setup, so
# the service and QR cannot accidentally retain the previous transport.
STABLE_SWITCH_DIR="$WORK_DIR/stable-switch"
STABLE_SWITCH_ENV="$WORK_DIR/config/stable-switch.env"
STABLE_SWITCH_MARKER="$WORK_DIR/stable-switch-called"
mkdir -p "$STABLE_SWITCH_DIR"
cp "$REPO_DIR/relay/common.sh" "$REPO_DIR/relay/plugin-install-service.sh" \
    "$STABLE_SWITCH_DIR/"
write_stub "$STABLE_SWITCH_DIR/stable-setup.sh" <<'EOF'
#!/usr/bin/env bash
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$HERDR_RELAY_ENV" ||
    [ -n "${HERDR_GATEWAY_URL:-}" ]; then
    echo "stable setup still inherited the gateway" >&2
    exit 1
fi
if [ "${HERDR_STABLE_SETUP_WRAPPED:-}" != 1 ]; then
    echo "🐑 Herdr Mobile Relay stable tunnel setup"
fi
printf 'called\n' > "$STABLE_SWITCH_MARKER"
EOF
printf "HERDR_GATEWAY_URL='wss://gw.example.test'\nHERDR_GATEWAY_SELECTION='latency'\n" \
    > "$STABLE_SWITCH_ENV"
export STABLE_SWITCH_MARKER
STABLE_SWITCH_OUTPUT="$(
    HERDR_RELAY_ENV="$STABLE_SWITCH_ENV" HERDR_GATEWAY_URL="wss://inherited.example.test" \
        bash "$STABLE_SWITCH_DIR/plugin-install-service.sh" 2>&1
)"
[ -f "$STABLE_SWITCH_MARKER" ] ||
    { echo "the stable tunnel action did not run stable setup" >&2; exit 1; }
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$STABLE_SWITCH_ENV"; then
    echo "the direct stable tunnel action left the gateway configured" >&2
    exit 1
fi
expect "$STABLE_SWITCH_OUTPUT" "Switching this relay from the WebRTC gateway to Cloudflare."
test "$(printf '%s\n' "$STABLE_SWITCH_OUTPUT" |
    grep -c 'Herdr Mobile Relay stable tunnel setup')" -eq 1 ||
    { echo "stable tunnel action printed its heading more than once" >&2; exit 1; }

echo "plugin-install-service shell tests passed"

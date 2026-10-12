#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/herdr-service-credentials.XXXXXX")"
trap 'status=$?; rm -rf "$WORK"; exit $status' EXIT
mkdir -p "$WORK/home"
export HOME="$WORK/home" SERVICE_TEST_ROOT="$WORK"
printf 'hostname: fixture.invalid\n' > "$WORK/tunnel.yml"
printf 'GH_TOKEN=synthetic-service-token\nGITHUB_TOKEN=synthetic-service-token\nHERDR_GITHUB_TOKEN_FILE=%q\n' "$WORK/private-token" > "$WORK/relay.env"
printf 'synthetic-file-token\n' > "$WORK/private-token"
chmod 600 "$WORK/private-token"
cat > "$WORK/relay" <<'EOF'
#!/bin/sh
printf '%s|%s|%s|%s' "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${HERDR_GITHUB_TOKEN_FILE:-}" "${HTTPS_PROXY:-}" > "$SERVICE_TEST_ROOT/relay-environment"
for attempt in 1 2 3 4 5; do
    [ ! -f "$SERVICE_TEST_ROOT/tunnel-environment" ] || exit 0
    sleep 0.1
done
exit 1
EOF
cat > "$WORK/cloudflared" <<'EOF'
#!/bin/sh
printf '%s|%s|%s|%s' "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${HERDR_GITHUB_TOKEN_FILE:-}" "${HTTPS_PROXY:-}" > "$SERVICE_TEST_ROOT/tunnel-environment"
sleep 1
EOF
chmod 700 "$WORK/relay" "$WORK/cloudflared"
GH_TOKEN=synthetic-ambient-token GITHUB_TOKEN=synthetic-ambient-token \
HERDR_GITHUB_TOKEN_FILE="$WORK/ambient-pointer" HTTPS_PROXY=http://proxy.invalid \
HERDR_RELAY_ENV="$WORK/relay.env" HERDR_RELAY_BIN="$WORK/relay" \
CLOUDFLARED_BIN="$WORK/cloudflared" CLOUDFLARED_CONFIG="$WORK/tunnel.yml" \
bash -x "$ROOT/relay/herdr-mobile-relay-service.sh" > "$WORK/output" 2>&1 || true
test "$(<"$WORK/relay-environment")" = "||$WORK/private-token|http://proxy.invalid"
test "$(<"$WORK/tunnel-environment")" = '|||http://proxy.invalid'
if grep -E 'synthetic-(service|ambient|file)-token' "$WORK/output" >/dev/null; then
    echo 'service tracing exposed a credential' >&2
    exit 1
fi
printf 'service credential boundaries passed\n'

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
case "$1" in
normalize-origin) printf 'https://gateway.example.test\n'; exit 0 ;;
serve-installed) ;;
*) exit 2 ;;
esac
printf '%s|%s|%s|%s' "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${HERDR_GITHUB_TOKEN_FILE:-}" "${HTTPS_PROXY:-}" > "$SERVICE_TEST_ROOT/relay-environment"
[ "${SERVICE_TEST_TRANSPORT:-}" != gateway ] || exit 23
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
# Either essential child's unsolicited clean exit maps to supervisor failure 1.
status=0
GH_TOKEN=synthetic-ambient-token GITHUB_TOKEN=synthetic-ambient-token \
HERDR_GITHUB_TOKEN_FILE="$WORK/ambient-pointer" HTTPS_PROXY=http://proxy.invalid \
HERDR_RELAY_ENV="$WORK/relay.env" HERDR_RELAY_BIN="$WORK/relay" \
CLOUDFLARED_BIN="$WORK/cloudflared" CLOUDFLARED_CONFIG="$WORK/tunnel.yml" \
bash -x "$ROOT/relay/herdr-mobile-relay-service.sh" > "$WORK/output" 2>&1 || status=$?
test "$status" = 1
test "$(<"$WORK/relay-environment")" = "||$WORK/private-token|http://proxy.invalid"
test "$(<"$WORK/tunnel-environment")" = '|||http://proxy.invalid'
if grep -E 'synthetic-(service|ambient|file)-token' "$WORK/output" >/dev/null; then
    echo 'service tracing exposed a credential' >&2
    exit 1
fi
# Gateway retains the updater pointer for the relay but starts no tunnel,
# even when unrelated Cloudflare configuration is still saved.
rm -f "$WORK/tunnel-environment" "$WORK/relay-environment"
printf 'HERDR_RELAY_TRANSPORT=gateway\nHERDR_GATEWAY_URL=wss://gateway.example.test\nCLOUDFLARED_CONFIG=%q\n' "$WORK/tunnel.yml" >> "$WORK/relay.env"
status=0
SERVICE_TEST_TRANSPORT=gateway GH_TOKEN=synthetic-ambient-token GITHUB_TOKEN=synthetic-ambient-token \
HERDR_GITHUB_TOKEN_FILE="$WORK/ambient-pointer" HTTPS_PROXY=http://proxy.invalid \
HERDR_RELAY_ENV="$WORK/relay.env" HERDR_RELAY_BIN="$WORK/relay" \
CLOUDFLARED_BIN="$WORK/cloudflared" \
bash -x "$ROOT/relay/herdr-mobile-relay-service.sh" > "$WORK/gateway-output" 2>&1 || status=$?
test "$status" = 23
test "$(<"$WORK/relay-environment")" = "||$WORK/private-token|http://proxy.invalid"
test ! -e "$WORK/tunnel-environment"
if grep -E 'synthetic-(service|ambient|file)-token' "$WORK/gateway-output" >/dev/null; then
    echo 'gateway service tracing exposed a credential' >&2
    exit 1
fi
# Native installers scrub raw in-memory keys before normalization. Even with
# no ambient release token, ensure_relay_env must remove both saved legacy
# keys while retaining the already selected private updater pointer.
(
    # shellcheck source=../relay/common.sh
    . "$ROOT/relay/common.sh"
    unset GH_TOKEN GITHUB_TOKEN
    printf 'HERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef\nHERDR_RELAY_INSTANCE_ID=fixture\n' >> "$WORK/relay.env"
    ensure_relay_env "$WORK/relay.env"
    if grep -Eq '^(GH_TOKEN|GITHUB_TOKEN)=' "$WORK/relay.env"; then
        echo 'normalized service environment retained a legacy raw token key' >&2
        exit 1
    fi
    load_relay_env "$WORK/relay.env"
    test "$(/bin/sh -c 'printf "%s|%s|%s" "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${HERDR_GITHUB_TOKEN_FILE:-}"')" = "||$WORK/private-token"
)
# Native selected-only migration stages bytes/pointer before scrubbing and
# never uses an ambient installer credential. No manager or live publication.
for key in GH_TOKEN GITHUB_TOKEN; do
    (
        . "$ROOT/relay/common.sh"
        directory="$WORK/native-$key"
        mkdir -m 700 "$directory"
        directory="$(cd "$directory" && pwd -P)"
        native_recovery="$directory/recovery"; mkdir -m 700 "$native_recovery"
        file="$directory/relay.env"
        printf 'HERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef\nHERDR_RELAY_TRANSPORT=cloudflare\n  export %s=\x27synthetic-native-selected-token\x27\n\t%s="synthetic-native-selected-token"\n' "$key" "$key" > "$file"
        cp -p "$file" "$directory/before"
        export GH_TOKEN=synthetic-ambient-native-token GITHUB_TOKEN=synthetic-distinct-native-token
        export HERDR_GITHUB_TOKEN_FILE="$directory/ambient-pointer"
        if native_installer_admit "$file" relative-runtime; then exit 1; fi
        cmp "$file" "$directory/before"
        set -x
        native_installer_admit "$file"
        test "$NATIVE_SELECTED_TOKEN" = synthetic-native-selected-token
        native_installer_candidate "$file"
        cmp "$file" "$directory/before"
        test ! -e "$directory/github-token"
        test "$(cat "$native_recovery/candidate.token")" = synthetic-native-selected-token
        load_relay_env "$native_recovery/candidate.env"
        test "$(/bin/sh -c 'printf "%s|%s|%s" "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${HERDR_GITHUB_TOKEN_FILE:-}"')" = "||$directory/github-token"
    ) > "$WORK/native-$key-output" 2>&1
    if grep -E 'synthetic-(native-selected|ambient-native|distinct-native)-token' "$WORK/native-$key-output" >/dev/null; then
        echo 'native selected credential tracing exposed a credential' >&2; exit 1
    fi
done
# General callers retain explicit credential precedence; failed second-resource
# publication restores an existing UNSELECTED destination's exact bytes/mode.
(
    . "$ROOT/relay/common.sh"
    file="$WORK/persist-failure.env"; pointer="$WORK/github-token"
    printf 'KEEP=untouched\n' > "$file"
    printf 'prior-unselected-content\n' > "$pointer"; chmod 640 "$pointer"
    cp -p "$file" "$file.before"; cp -p "$pointer" "$pointer.before"
    mv() { [ "${!#}" != "$file" ] || return 2; command mv "$@"; }
    if persist_github_token "$file" synthetic-explicit-contract-token; then exit 1; fi
    cmp "$file" "$file.before"; cmp "$pointer" "$pointer.before"
    mode="$(stat -c '%a' "$pointer" 2>/dev/null)" || mode="$(stat -f '%Lp' "$pointer")"
    test "$mode" = 640
)
printf 'service credential boundaries passed\n'

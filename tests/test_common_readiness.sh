#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-common-readiness-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

unset GH_TOKEN GITHUB_TOKEN
FAKE_LAUNCHCTL_DIR="$WORK_DIR/launchctl-bin"
LAUNCHCTL_LOG="$WORK_DIR/launchctl.log"
mkdir -p "$FAKE_LAUNCHCTL_DIR"
cat > "$FAKE_LAUNCHCTL_DIR/sleep" <<'EOF'
#!/bin/sh
printf 'sleep %s\n' "$*" >> "$LAUNCHCTL_LOG"
EOF
chmod 700 "$FAKE_LAUNCHCTL_DIR/sleep"
export LAUNCHCTL_LOG
ensure_relay_test_binary "$WORK_DIR"
export HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN"
export HERDR_RELAY_INSTANCE_ID=test

# The inferred-identity path reads the selected binary's sibling manifest,
# never a fabricated field in web/release.json. Use the committed descriptor
# directly so this regression cannot accidentally reproduce the old fixture.
READY_RELEASE_ROOT="$WORK_DIR/readiness-releases"
READY_RELEASE="$READY_RELEASE_ROOT/releases/manifest-release"
mkdir -p "$READY_RELEASE/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_RELEASE/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_RELEASE/web/release.json"
printf '{"version":"dev","revision":"unknown","web_hash":"manifest-web"}\n' \
    > "$READY_RELEASE/release-manifest.json"
if grep -q '"bundle_hash"' "$READY_RELEASE/web/release.json"; then
    echo "committed frontend descriptor unexpectedly contains bundle_hash" >&2
    exit 1
fi
READY_CURL_LOG="$WORK_DIR/readiness-curl.log"
READY_ATTEMPTS="$WORK_DIR/readiness-attempts"
READY_CURL_MODE=static
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
: > "$READY_CURL_LOG"
curl() {
    printf '%s\n' "$*" >> "$READY_CURL_LOG"
    if [ "$READY_CURL_MODE" = retry ]; then
        local attempt=0
        [ ! -f "$READY_ATTEMPTS" ] || attempt="$(<"$READY_ATTEMPTS")"
        attempt=$((attempt + 1))
        printf '%s\n' "$attempt" > "$READY_ATTEMPTS"
        if [ "$attempt" -eq 1 ]; then
            printf '%s\n' '{"status":"ok","inventory":{"state":"error"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
            return
        fi
    fi
    printf '%s\n' "$READY_RESPONSE"
}

HERDR_RELAY_BIN="$READY_RELEASE/herdr-mobile-relay"
unset HERDR_RELEASE_ROOT HERDR_WEB_ROOT
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
test "$(cat "$READY_CURL_LOG")" = \
    '-fsS --max-time 2 http://127.0.0.1:8375/readyz'

# HERDR_WEB_ROOT and a descriptor's tempting bundle_hash must not replace the
# release manifest's identity.
READY_DECOY_WEB="$WORK_DIR/readiness-decoy-web"
mkdir -p "$READY_DECOY_WEB"
printf '{"bundle_hash":"descriptor-decoy"}\n' > "$READY_DECOY_WEB/release.json"
HERDR_WEB_ROOT="$READY_DECOY_WEB"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
: > "$READY_CURL_LOG"
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
READY_RESPONSE=${READY_RESPONSE/manifest-web/descriptor-decoy}
if wait_for_relay_health 8375 1 0 test >/dev/null 2>&1; then
    echo "readiness accepted the descriptor's misleading bundle_hash" >&2
    exit 1
fi
test -s "$READY_CURL_LOG"
unset HERDR_WEB_ROOT
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'

for invalid_health in \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"wrong-instance","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"wrong-version","revision":"unknown","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"wrong-revision","bundle_hash":"manifest-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"wrong-web","protocol":3}' \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":2}' \
    '{"status":"ready","inventory":{"state":"error"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'; do
    READY_RESPONSE="$invalid_health"
    : > "$READY_CURL_LOG"
    if wait_for_relay_health 8375 1 0 test >/dev/null 2>&1; then
        echo "readiness accepted invalid identity or inventory: $invalid_health" >&2
        exit 1
    fi
    test "$(wc -l < "$READY_CURL_LOG" | tr -d ' ')" = 1
done

if HERDR_RELAY_BACKEND=hc wait_for_relay_health 8377 1 0 test >"$WORK_DIR/core-ready.out" 2>"$WORK_DIR/core-ready.err"; then
    echo "core readiness accepted failed inventory" >&2
    exit 1
fi
grep -q 'Start herdr-app or run hc server start' "$WORK_DIR/core-ready.err"
! grep -Eq 'Herdr|TUI' "$WORK_DIR/core-ready.err"

# Preserve the bounded retry loop: an unready first reply is retried and a
# matching reply on the second poll is returned unchanged.
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'
READY_CURL_MODE=retry
rm -f "$READY_ATTEMPTS"
: > "$READY_CURL_LOG"
READY_OUTPUT="$(wait_for_relay_health 8375 3 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
test "$(cat "$READY_ATTEMPTS")" = 2
test "$(wc -l < "$READY_CURL_LOG" | tr -d ' ')" = 2
READY_CURL_MODE=static

# Missing, unreadable/non-file, and empty manifests fail before any HTTP poll
# and identify the offending metadata path.
READY_MANIFEST="$READY_RELEASE/release-manifest.json"
READY_MANIFEST_BACKUP="$WORK_DIR/readiness-release-manifest.json"
mv "$READY_MANIFEST" "$READY_MANIFEST_BACKUP"
: > "$READY_CURL_LOG"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/missing-manifest.out" 2>"$WORK_DIR/missing-manifest.err"; then
    echo "readiness accepted a missing release manifest" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/missing-manifest.err" >/dev/null
test ! -s "$READY_CURL_LOG"
mkdir "$READY_MANIFEST"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/unreadable-manifest.out" 2>"$WORK_DIR/unreadable-manifest.err"; then
    echo "readiness accepted a non-file release manifest" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/unreadable-manifest.err" >/dev/null
test ! -s "$READY_CURL_LOG"
rmdir "$READY_MANIFEST"
if [ "$(id -u)" -ne 0 ]; then
    printf '{"web_hash":"unreadable-web"}\n' > "$READY_MANIFEST"
    chmod 000 "$READY_MANIFEST"
    if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/denied-manifest.out" 2>"$WORK_DIR/denied-manifest.err"; then
        chmod 600 "$READY_MANIFEST"
        echo "readiness accepted an unreadable release manifest" >&2
        exit 1
    fi
    grep -F "$READY_MANIFEST" "$WORK_DIR/denied-manifest.err" >/dev/null
    test ! -s "$READY_CURL_LOG"
    chmod 600 "$READY_MANIFEST"
fi
printf '{"web_hash":""}\n' > "$READY_MANIFEST"
if wait_for_relay_health 8375 1 0 test >"$WORK_DIR/empty-hash.out" 2>"$WORK_DIR/empty-hash.err"; then
    echo "readiness accepted an empty manifest web_hash" >&2
    exit 1
fi
grep -F "$READY_MANIFEST" "$WORK_DIR/empty-hash.err" >/dev/null
test ! -s "$READY_CURL_LOG"

# An explicit expected hash wins even when the manifest is absent or conflicts.
rm "$READY_MANIFEST"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"explicit-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test dev unknown explicit-web)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
printf '{"web_hash":"conflicting-manifest-web"}\n' > "$READY_MANIFEST"
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test dev unknown explicit-web)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
mv "$READY_MANIFEST_BACKUP" "$READY_MANIFEST"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"manifest-web","protocol":3}'

# An explicit binary override and the current symlink each select the sibling
# manifest belonging to the chosen release, not another configured release.
READY_OVERRIDE_ROOT="$WORK_DIR/readiness-override-root"
READY_OVERRIDE="$READY_OVERRIDE_ROOT/releases/override"
mkdir -p "$READY_OVERRIDE/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_OVERRIDE/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_OVERRIDE/web/release.json"
printf '{"web_hash":"override-web"}\n' > "$READY_OVERRIDE/release-manifest.json"
HERDR_RELEASE_ROOT="$READY_RELEASE_ROOT"
HERDR_RELAY_BIN="$READY_OVERRIDE/herdr-mobile-relay"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"override-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"
READY_CURRENT_ROOT="$WORK_DIR/readiness-current-root"
READY_CURRENT="$READY_CURRENT_ROOT/releases/current-target"
mkdir -p "$READY_CURRENT/web"
cp "$HERDR_TEST_RELAY_BIN" "$READY_CURRENT/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_CURRENT/web/release.json"
printf '{"web_hash":"current-web"}\n' > "$READY_CURRENT/release-manifest.json"
ln -s releases/current-target "$READY_CURRENT_ROOT/current"
unset HERDR_RELAY_BIN
HERDR_RELEASE_ROOT="$READY_CURRENT_ROOT"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"current-web","protocol":3}'
READY_OUTPUT="$(wait_for_relay_health 8375 1 0 test)"
test "$READY_OUTPUT" = "$READY_RESPONSE"

# A packaged script's own release takes precedence over a foreign override and
# release root, and its manifest is resolved next to its selected binary.
READY_PACKAGED="$WORK_DIR/readiness-packaged"
mkdir -p "$READY_PACKAGED/relay" "$READY_PACKAGED/web"
cp "$REPO_DIR/relay/common.sh" "$READY_PACKAGED/relay/common.sh"
cp "$HERDR_TEST_RELAY_BIN" "$READY_PACKAGED/herdr-mobile-relay"
cp "$REPO_DIR/web/release.json" "$READY_PACKAGED/web/release.json"
printf '{"web_hash":"packaged-web"}\n' > "$READY_PACKAGED/release-manifest.json"
chmod 700 "$READY_PACKAGED/herdr-mobile-relay"
HERDR_RELAY_BIN="$READY_OVERRIDE/herdr-mobile-relay"
HERDR_RELEASE_ROOT="$READY_CURRENT_ROOT"
READY_RESPONSE='{"status":"ready","inventory":{"state":"ready"},"instance":"test","release_version":"dev","revision":"unknown","bundle_hash":"packaged-web","protocol":3}'
READY_OUTPUT="$(
    . "$READY_PACKAGED/relay/common.sh"
    wait_for_relay_health 8375 1 0 test
)"
test "$READY_OUTPUT" = "$READY_RESPONSE"

unset -f curl
HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN"
unset HERDR_RELEASE_ROOT HERDR_WEB_ROOT
HEALTH='{"status":"ready","inventory":{"state":"ready"},"instance":"test","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
verify_relay_release_health "$HEALTH" "0.9.0" "abc123" "web456"
if verify_relay_release_health "$HEALTH" "0.9.0" "wrong" "web456"; then
    echo "release health accepted the wrong revision" >&2
    exit 1
fi

for invalid_health in \
    '{"status":"ready","inventory":{"state":"ready"},"instance":"wrong-instance","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}' \
    '{"status":"ready","inventory":{"state":"error"},"instance":"test","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'; do
    (
        curl() { printf '%s\n' "$invalid_health"; }
        if wait_for_relay_health 8375 1 0 test 0.9.0 abc123 web456; then
            echo 'generic readiness accepted unusable or foreign inventory' >&2
            exit 1
        fi
        if wait_for_relay_release_health 8375 1 0 0.9.0 abc123 web456 test; then
            echo 'release readiness accepted unusable or foreign inventory' >&2
            exit 1
        fi
    )
done

HEALTH_ATTEMPTS="$WORK_DIR/health-attempts"
cat > "$FAKE_LAUNCHCTL_DIR/curl" <<'EOF'
#!/bin/sh
attempt=0
if [ -f "$HEALTH_ATTEMPTS" ]; then
    attempt="$(cat "$HEALTH_ATTEMPTS")"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$HEALTH_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    printf '%s\n' '{"status":"ok","instance":"test","version":"0.8.6","protocol":2,"release_version":"0.8.6","revision":"old","bundle_hash":"old-web"}'
else
    printf '%s\n' '{"status":"ready","inventory":{"state":"ready"},"instance":"test","version":"0.9.0","protocol":3,"release_version":"0.9.0","revision":"abc123","bundle_hash":"web456"}'
fi
EOF
chmod 700 "$FAKE_LAUNCHCTL_DIR/curl"
export HEALTH_ATTEMPTS
EXACT_HEALTH="$(
    PATH="$FAKE_LAUNCHCTL_DIR:$PATH" \
        wait_for_relay_release_health 8375 3 1 "0.9.0" "abc123" "web456"
)"
test "$(json_string_field "$EXACT_HEALTH" release_version)" = "0.9.0"
test "$(cat "$HEALTH_ATTEMPTS")" = "2"
unset HERDR_RELAY_BIN HERDR_RELAY_INSTANCE_ID

echo "common readiness shell tests passed"

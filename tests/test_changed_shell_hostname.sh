#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-hostname-coverage.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT
BIN_DIR="$WORK_DIR/bin"
mkdir -p "$BIN_DIR"

awk 'NR >= 100 && NR <= 111 { print; next } NR >= 184 && NR <= 198 { print; next } NR <= 198 { print ""; next } { exit }' "$ROOT_DIR/relay/change-hostname.sh" > "$WORK_DIR/change-hostname-fragment.sh"

cat > "$BIN_DIR/curl" <<'SH'
#!/bin/sh
if [ "$*" = "--help all" ]; then
    [ "${NO_DOH:-}" = 1 ] || printf '%s\n' '  --doh-url <URL>'
    exit 0
fi
case "${NO_DOH:-}:$*" in
    1:*--doh-url*) exit 2 ;;
    :*--doh-url*) ;;
    1:*) exit 0 ;;
    *) exit 2 ;;
esac
count=0
[ ! -f "$CURL_COUNT" ] || count=$(cat "$CURL_COUNT")
count=$((count + 1))
printf '%s\n' "$count" > "$CURL_COUNT"
[ "$count" -gt 1 ]
SH
cat > "$BIN_DIR/sleep" <<'SH'
#!/bin/sh
exit 0
SH
chmod 700 "$BIN_DIR/curl" "$BIN_DIR/sleep"

CURL_COUNT="$WORK_DIR/curl-count"
export CURL_COUNT
# shellcheck source=/dev/null
NEW_HOSTNAME=relay.example.test PATH="$BIN_DIR:$PATH" HERDR_STABLE_DNS_TIMEOUT=10 HERDR_DOH_URL=https://resolver.example.test/dns-query . "$WORK_DIR/change-hostname-fragment.sh"
test "$(cat "$CURL_COUNT")" = 2

find "$CURL_COUNT" -type f -delete
# shellcheck source=/dev/null
NO_DOH=1 NEW_HOSTNAME=relay.example.test PATH="$BIN_DIR:$PATH" HERDR_STABLE_DNS_TIMEOUT=10 . "$WORK_DIR/change-hostname-fragment.sh"
test ! -e "$CURL_COUNT"

printf 'changed shell hostname tests passed\n'

#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd "${0%/*}/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/herdr-download-test.XXXXXX")
trap 'status=$?; rm -rf "$WORK"; exit $status' EXIT INT TERM
mkdir -p "$WORK/bin" "$WORK/home"
sed '$d' "$ROOT/install.sh" > "$WORK/functions.sh"
cat > "$WORK/bin/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$TEST_RECORD/argv"
env > "$TEST_RECORD/environment"
cat <&3 > "$TEST_RECORD/config"
EOF
cat > "$WORK/bin/sleep" <<'EOF'
#!/bin/sh
if ( : <&3 ) 2>/dev/null; then
    : > "$TEST_RECORD/descriptor-leak"
fi
env > "$TEST_RECORD/waiter-environment"
exec /bin/sleep "$@"
EOF
chmod +x "$WORK/bin/curl" "$WORK/bin/sleep"
cat > "$WORK/driver.sh" <<'EOF'
. "$TEST_RECORD/functions.sh"
fetch https://api.github.com/repos/example/release/assets/1 "$TEST_RECORD/output"
env > "$TEST_RECORD/unrelated-environment"
EOF
secret=synthetic_download_token_123
HOME="$WORK/home" TEST_RECORD="$WORK" PATH="$WORK/bin:$PATH" GH_TOKEN="$secret" GITHUB_TOKEN="$secret" \
    sh -x "$WORK/driver.sh" > "$WORK/output.log" 2>&1
[ ! -e "$WORK/descriptor-leak" ]
for file in argv environment unrelated-environment output.log; do
    if grep -F "$secret" "$WORK/$file" >/dev/null; then
        echo "download credential leaked into $file" >&2
        exit 1
    fi
done
grep -Fx 'header = "Authorization: token synthetic_download_token_123"' "$WORK/config" >/dev/null
grep -Fx /dev/fd/3 "$WORK/argv" >/dev/null
rm "$WORK/argv"
if HOME="$WORK/home" TEST_RECORD="$WORK" PATH="$WORK/bin:$PATH" GH_TOKEN='invalid"token' \
    sh "$WORK/driver.sh" > "$WORK/invalid.log" 2>&1; then
    echo 'invalid token accepted' >&2
    exit 1
fi
[ ! -e "$WORK/argv" ]
if grep -F 'invalid"token' "$WORK/invalid.log" >/dev/null; then
    echo 'invalid token leaked' >&2
    exit 1
fi
printf '%s\n' 'download credential isolation tests passed'

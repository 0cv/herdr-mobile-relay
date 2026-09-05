#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-speech-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

SCRIPT_DIR="$WORK_DIR/relay"
mkdir -p "$SCRIPT_DIR"
cp "$REPO_DIR/relay/speech-voices.sh" "$REPO_DIR/relay/common.sh" "$SCRIPT_DIR/"
SCRIPT="$SCRIPT_DIR/speech-voices.sh"

unset HERDR_RELAY_BIN
export HOME="$WORK_DIR/home"
export XDG_CACHE_HOME="$HOME/cache"
export HERDR_RELEASE_ROOT="$HOME/releases-root"
export ARGV_FILE="$WORK_DIR/argv.txt"
export STUB_EXIT=0

# The catalog, the digests, and the downloads live in the relay binary, so a
# stub that only records its argv proves the wrapper without any network.
mkdir -p "$HERDR_RELEASE_ROOT/current"
STUB="$HERDR_RELEASE_ROOT/current/herdr-mobile-relay"
cat > "$STUB" <<'STUB'
#!/bin/bash
printf '%s\n' "$@" > "$ARGV_FILE"
exit "${STUB_EXIT:-0}"
STUB
chmod 700 "$STUB"

# Wrapper flags before --, the argv the relay binary must receive after it.
forwards() {
    local wrapper=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
        wrapper+=("$1")
        shift
    done
    shift
    rm -f "$ARGV_FILE"
    "$SCRIPT" "${wrapper[@]}" >/dev/null
    if [ "$(cat "$ARGV_FILE")" != "$(printf '%s\n' "$@")" ]; then
        echo "wrapper turned '${wrapper[*]}' into '$(tr '\n' ' ' < "$ARGV_FILE")'" >&2
        exit 1
    fi
}

# Setup calls the wrapper bare, and the binary owns the default language list.
rm -f "$ARGV_FILE"
"$SCRIPT" >/dev/null
test "$(cat "$ARGV_FILE")" = "$(printf '%s\n' speech-voices install)"

forwards --missing -- speech-voices missing
forwards --missing --languages en,fr -- speech-voices missing --languages en,fr
forwards --languages en,fr -- speech-voices install --languages en,fr
forwards --remove --languages de -- speech-voices remove --languages de
forwards --reinstall-runtime -- speech-voices reinstall-runtime

# The binary decides the outcome, so its exit status has to survive the wrapper.
rm -f "$ARGV_FILE"
if STUB_EXIT=3 "$SCRIPT" --languages en >/dev/null 2>&1; then
    echo "wrapper reported success for a failing relay binary" >&2
    exit 1
else
    test "$?" -eq 3
fi

# --help is answered by the wrapper itself, before any binary is resolved.
rm -f "$ARGV_FILE"
help_output="$("$SCRIPT" --help)"
grep -q -- "--remove" <<<"$help_output"
grep -q -- "--languages en,fr,de,es,zh" <<<"$help_output"
test ! -e "$ARGV_FILE"

if "$SCRIPT" --nonsense >/dev/null 2>&1; then
    echo "wrapper accepted an unknown flag" >&2
    exit 1
else
    test "$?" -eq 2
fi

# Without an installed release there is nothing to download the voices with.
if HERDR_RELEASE_ROOT="$HOME/absent" "$SCRIPT" --missing \
    >/dev/null 2>"$WORK_DIR/absent.log"; then
    echo "wrapper ran without an installed relay release" >&2
    exit 1
fi
grep -q "install it first" "$WORK_DIR/absent.log"

echo "speech voice wrapper tests passed"

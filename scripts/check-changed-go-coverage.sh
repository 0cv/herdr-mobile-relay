#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH='' cd "$(dirname "$0")/.." && pwd -P)
BASE=${HERDR_CHANGED_COVERAGE_BASE:-c6e24cc3f627ecc791db39d77b177a754c46501e}
TEMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/herdr-changed-go-coverage.XXXXXX")
trap 'rm -rf "$TEMP_ROOT"' EXIT HUP INT TERM

cd "$ROOT_DIR"
python3 tests/test_changed_go_coverage.py >/dev/null
python3 scripts/check-changed-go-coverage.py --self-test >/dev/null
go test ./tests/changed-go-coverage-ast >/dev/null

CHANGES="$TEMP_ROOT/changes.json"
OVERLAY="$TEMP_ROOT/overlay.json"
INVENTORY="$TEMP_ROOT/inventory.json"
INSTRUMENTED="$TEMP_ROOT/instrumented"
OUTPUT="$TEMP_ROOT/test-output.log"

python3 scripts/check-changed-go-coverage.py --base "$BASE" --write-changes "$CHANGES" >/dev/null
go run ./tests/changed-go-coverage-ast --changes "$CHANGES" --output-root "$INSTRUMENTED" --overlay "$OVERLAY" --inventory "$INVENTORY"
PACKAGES=$(python3 scripts/check-changed-go-coverage.py --inventory "$INVENTORY" --print-packages)
[ -n "$PACKAGES" ] || { echo "changed Go coverage passed: no production Go changes"; exit 0; }

: >"$OUTPUT"
INDEX=0
for PACKAGE in $PACKAGES; do
    INDEX=$((INDEX + 1))
    PACKAGE_OUTPUT="$TEMP_ROOT/$INDEX.log"
    if [ "$(go env GOOS)" = darwin ] && [ "$PACKAGE" = ./internal/coordinator ]; then
        TEST_PATTERN=$(go test "$PACKAGE" -list '^Test' | awk '/^Test/ && $0 != "TestTabReorderUsesHerdrSocketAPI" && $0 != "TestResolveCwdReturnsCanonicalSymlinkTarget" { names = names separator $0; separator = "|" } END { print "^(" names ")$" }')
        if ! go test -count=1 -overlay "$OVERLAY" -v -run "$TEST_PATTERN" "$PACKAGE" >"$PACKAGE_OUTPUT" 2>&1; then
            cat "$PACKAGE_OUTPUT"
            exit 1
        fi
    elif ! go test -count=1 -overlay "$OVERLAY" -v "$PACKAGE" >"$PACKAGE_OUTPUT" 2>&1; then
        cat "$PACKAGE_OUTPUT"
        exit 1
    fi
    cat "$PACKAGE_OUTPUT" >>"$OUTPUT"
    echo "instrumented changed Go package: $PACKAGE"
done

python3 scripts/check-changed-go-coverage.py --inventory "$INVENTORY" --test-output "$OUTPUT"

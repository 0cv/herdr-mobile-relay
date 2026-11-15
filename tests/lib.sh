# Helpers shared by the bash test suites. Source this file; do not run it.

ensure_relay_test_binary() {
    local build_dir="$1" repo_dir
    if [ -f "${HERDR_TEST_RELAY_BIN:-}" ] && [ -x "$HERDR_TEST_RELAY_BIN" ]; then
        return
    fi
    repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    (cd "$repo_dir" && go build -o "$build_dir/herdr-mobile-relay" ./cmd/herdr-mobile-relay)
    export HERDR_TEST_RELAY_BIN="$build_dir/herdr-mobile-relay"
}

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

write_stub() {
    cat > "$1"
    chmod 700 "$1"
}

write_origin_stub() {
    write_stub "$1" <<'EOF'
#!/bin/sh
# Stands in for `herdr-mobile-relay normalize-origin --allow-loopback-http URL`:
# a bare host defaults to HTTPS, plain HTTP is loopback-only, and credentials,
# paths, queries, and fragments are rejected.
test "$1" = "normalize-origin" || exit 2
test "$2" = "--allow-loopback-http" || exit 2
value="$3"
case "$value" in
    *://*) ;;
    *) value="https://$value" ;;
esac
scheme="${value%%://*}"
host="${value#*://}"
host="${host%/}"
case "$host" in
    ''|*/*|*\?*|*'#'*|*@*) exit 1 ;;
esac
case "$scheme" in
    https) ;;
    http)
        case "${host%%:*}" in
            localhost|127.0.0.1|::1) ;;
            *) exit 1 ;;
        esac
        ;;
    *) exit 1 ;;
esac
printf '%s://%s\n' "$scheme" "$host"
EOF
}

fail() {
    printf '%s\n' "$1" >&2
    exit 1
}

expect() {
    [[ "$1" == *"$2"* ]] || fail "$(printf 'Missing: %s\nIn:\n%s' "$2" "$1")"
}

reject() {
    [[ "$1" != *"$2"* ]] || fail "$(printf 'Unexpected: %s\nIn:\n%s' "$2" "$1")"
}

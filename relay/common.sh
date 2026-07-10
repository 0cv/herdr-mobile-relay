#!/bin/bash

generate_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 16
        return
    fi
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen | tr '[:upper:]' '[:lower:]' | tr -d '-'
        return
    fi
    echo "Cannot generate a relay token: install openssl or uuidgen." >&2
    return 1
}

append_env_default() {
    local env_file="$1"
    local key="$2"
    local value="$3"

    if grep -q "^${key}=" "$env_file"; then
        return
    fi
    printf '%s=%s\n' "$key" "$value" >> "$env_file"
}

ensure_relay_env() {
    local env_file="$1"
    local cloudflared_config="${2:-}"

    if [ ! -f "$env_file" ]; then
        umask 077
        touch "$env_file"
        echo "Created $env_file"
    fi

    chmod 600 "$env_file"
    if ! grep -q '^HERDR_RELAY_TOKEN=' "$env_file" || [ -z "$(sed -n 's/^HERDR_RELAY_TOKEN=//p' "$env_file" | tail -1)" ]; then
        printf 'HERDR_RELAY_TOKEN=%s\n' "$(generate_token)" >> "$env_file"
    fi
    if [ -n "$cloudflared_config" ]; then
        append_env_default "$env_file" CLOUDFLARED_CONFIG "$cloudflared_config"
    fi
}

load_relay_env() {
    local env_file="$1"
    if [ ! -f "$env_file" ]; then
        return
    fi
    set -a
    # shellcheck source=/dev/null
    . "$env_file"
    set +a
}

host_label() {
    hostname -s 2>/dev/null || hostname 2>/dev/null || echo relay
}

# Must never fail once uv is present: callers embed the result in the setup
# link. The token passes through argv, briefly visible in ps; this matches the
# pre-existing pattern and lasts only for the interpreter startup.
build_setup_fragment() {
    uv run python -c 'import sys, urllib.parse; print(urllib.parse.urlencode({"setup": sys.argv[1], "label": sys.argv[2]}))' "$1" "$2"
}

# Prints an indented terminal QR code for the URL, or nothing when it cannot
# be drawn: segno unavailable (e.g. offline before it is cached), or the
# terminal is too narrow — a wrapped QR is worse than the plain link.
# Callers must keep working with empty output. Kept separate from
# build_setup_fragment on purpose: this call is allowed to fail, that one
# is not.
render_setup_qr() {
    local url="$1"
    local cols
    cols="$(tput cols 2>/dev/null || true)"
    uv run --quiet --with segno python -c '
import io, sys
import segno
buf = io.StringIO()
segno.make(sys.argv[1]).terminal(out=buf, compact=True, border=2)
lines = ["  " + line for line in buf.getvalue().splitlines()]
if max(map(len, lines)) > int(sys.argv[2]):
    sys.exit(1)
sys.stdout.write("\n".join(lines))
' "$url" "${cols:-80}" 2>/dev/null || true
}

# Shared tail of quick-start and setup-link output: QR code when possible,
# always the link.
print_phone_setup() {
    local phone_url="$1"
    local qr_code
    qr_code="$(render_setup_qr "$phone_url")"
    if [ -n "$qr_code" ]; then
        echo "  Scan this QR code with your phone camera:"
        echo ""
        printf '%s\n' "$qr_code"
        echo ""
        echo "  This code contains your relay token; do not share screenshots of it."
        echo ""
        echo "  Or open this private setup link on your phone:"
    else
        echo "  Open this private setup link on your phone:"
    fi
    echo "  $phone_url"
}

require_supported_platform() {
    case "$(uname -s)" in
        Darwin|Linux)
            return
            ;;
        *)
            echo "Unsupported platform: Herdr Mobile Relay currently supports only Linux and macOS."
            exit 1
            ;;
    esac
}

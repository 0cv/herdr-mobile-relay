#!/bin/bash

relay_env_file() {
    local script_dir="$1"
    local config_dir
    local plugin_env

    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        printf '%s\n' "$HERDR_RELAY_ENV"
        return
    fi
    if [ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        printf '%s/.env\n' "$script_dir"
        return
    fi

    config_dir="$HERDR_PLUGIN_CONFIG_DIR"
    plugin_env="$config_dir/relay.env"
    mkdir -p "$config_dir"
    chmod 700 "$config_dir"
    if [ ! -f "$plugin_env" ] && [ -f "$script_dir/.env" ]; then
        umask 077
        cp "$script_dir/.env" "$plugin_env"
        chmod 600 "$plugin_env"
    fi
    if [ ! -d "$config_dir/push" ] && [ -d "$script_dir/push" ]; then
        umask 077
        cp -R "$script_dir/push" "$config_dir/push"
        chmod -R go-rwx "$config_dir/push"
    fi
    printf '%s\n' "$plugin_env"
}

canonical_file_path() {
    local path="$1"
    local directory
    local filename

    directory="$(dirname "$path")"
    filename="$(basename "$path")"
    if [ -d "$directory" ]; then
        directory="$(cd "$directory" && pwd -P)"
    fi
    printf '%s/%s\n' "${directory%/}" "$filename"
}

installed_service_env_file() {
    local service_file

    case "$(uname -s)" in
        Linux)
            service_file="$HOME/.config/systemd/user/herdr-mobile-relay.service"
            if [ -r "$service_file" ]; then
                sed -n 's/^Environment=HERDR_RELAY_ENV=//p' "$service_file" | tail -1
            fi
            ;;
        Darwin)
            service_file="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
            if [ -r "$service_file" ]; then
                awk '
                    /<key>HERDR_RELAY_ENV<\/key>/ { found = 1; next }
                    found && /<string>/ {
                        sub(/^.*<string>/, "")
                        sub(/<\/string>.*$/, "")
                        print
                        exit
                    }
                ' "$service_file"
            fi
            ;;
    esac
}

assert_service_env_matches() {
    local resolved_env
    local service_env

    resolved_env="$(canonical_file_path "$1")"
    service_env="$(installed_service_env_file)"
    if [ -z "$service_env" ]; then
        return
    fi
    service_env="$(canonical_file_path "$service_env")"
    if [ "$resolved_env" = "$service_env" ]; then
        return
    fi

    echo "✗ Refusing to use a different relay configuration than the installed service." >&2
    echo "  This command resolved: $resolved_env" >&2
    echo "  Installed service uses: $service_env" >&2
    echo "  Run the matching Herdr plugin action, or explicitly set:" >&2
    echo "  HERDR_RELAY_ENV=$service_env" >&2
    return 1
}

pause_before_close() {
    if [ -t 0 ]; then
        echo ""
        read -r -p "Press Enter to close this pane." _answer
    fi
}

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

generate_instance_id() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 16
        return
    fi
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen | tr '[:upper:]' '[:lower:]'
        return
    fi
    echo "Cannot generate a relay instance ID: install openssl or uuidgen." >&2
    return 1
}

env_file_value() {
    local env_file="$1"
    local key="$2"

    if [ ! -f "$env_file" ]; then
        return
    fi
    (
        set -a
        # shellcheck source=/dev/null
        . "$env_file"
        set +a
        printenv "$key" 2>/dev/null || true
    )
}

set_env_value_atomic() {
    local env_file="$1"
    local key="$2"
    local value="$3"
    local directory
    local temp_file

    case "$value" in
        *"'"*)
            echo "Cannot write $key: single quotes are not supported in relay environment values." >&2
            return 1
            ;;
    esac

    directory="$(dirname "$env_file")"
    mkdir -p "$directory"
    temp_file="$(mktemp "$directory/.relay-env.XXXXXX")"
    if [ -f "$env_file" ]; then
        grep -v "^${key}=" "$env_file" > "$temp_file" || true
    fi
    printf "%s='%s'\n" "$key" "$value" >> "$temp_file"
    chmod 600 "$temp_file"
    mv "$temp_file" "$env_file"
}

remove_env_value_if_equals_atomic() {
    local env_file="$1"
    local key="$2"
    local expected="$3"
    local current
    local directory
    local temp_file

    if [ ! -f "$env_file" ]; then
        return
    fi
    current="$(
        set -a
        # shellcheck source=/dev/null
        . "$env_file"
        set +a
        printenv "$key" 2>/dev/null || true
    )"
    if [ "$current" != "$expected" ]; then
        return
    fi

    directory="$(dirname "$env_file")"
    temp_file="$(mktemp "$directory/.relay-env.XXXXXX")"
    grep -v "^${key}=" "$env_file" > "$temp_file" || true
    chmod 600 "$temp_file"
    mv "$temp_file" "$env_file"
}

append_env_default() {
    local env_file="$1"
    local key="$2"
    local value="$3"

    if grep -q "^${key}=" "$env_file"; then
        return
    fi
    set_env_value_atomic "$env_file" "$key" "$value"
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
    if ! grep -q '^HERDR_RELAY_TOKEN=' "$env_file" || [ -z "$(env_file_value "$env_file" HERDR_RELAY_TOKEN)" ]; then
        set_env_value_atomic "$env_file" HERDR_RELAY_TOKEN "$(generate_token)"
    fi
    if ! grep -q '^HERDR_RELAY_INSTANCE_ID=' "$env_file" || [ -z "$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)" ]; then
        set_env_value_atomic "$env_file" HERDR_RELAY_INSTANCE_ID "$(generate_instance_id)"
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

wait_for_relay_health() {
    local port="${1:-8375}"
    local attempts="${2:-15}"
    local delay="${3:-1}"
    local health
    local attempt

    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required to verify relay health." >&2
        return 1
    fi

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Health-check attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null)"; then
            case "$health" in
                *'"status": "ok"'*'"instance":'*'"version":'*'"protocol":'*)
                    printf '%s\n' "$health"
                    return 0
                    ;;
            esac
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

host_label() {
    hostname -s 2>/dev/null || hostname 2>/dev/null || echo relay
}

# Must never fail once uv is present: callers embed the result in the setup
# link. The token passes through argv, briefly visible in ps; this matches the
# pre-existing pattern and lasts only for the interpreter startup.
build_setup_fragment() {
    uv run python -c 'import sys, urllib.parse; values = {"setup": sys.argv[1], "label": sys.argv[2]}; relay = sys.argv[3] if len(sys.argv) > 3 else ""; values.update({"relay": relay} if relay else {}); print(urllib.parse.urlencode(values))' "$1" "$2" "${3:-}"
}

phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="${2:-${HERDR_RELAY_ENV:-}}"
    local app_url="${HERDR_PHONE_APP_URL:-}"
    local normalized
    local recorded_origin

    if [ -z "$app_url" ] && [ -n "$env_file" ]; then
        recorded_origin="$(dirname "$env_file")/phone-app-origin"
        if [ -r "$recorded_origin" ]; then
            app_url="$(head -1 "$recorded_origin")"
        fi
    fi
    if [ -z "$app_url" ] || [ "$app_url" = "relay" ]; then
        app_url="$relay_fallback"
    fi
    if ! normalized="$(uv run python -c '
import sys
import urllib.parse

try:
    value = sys.argv[1].strip()
    if "://" not in value:
        value = f"https://{value}"
    parsed = urllib.parse.urlsplit(value)
    port = parsed.port
except ValueError:
    raise SystemExit(1)
loopback_http = parsed.scheme == "http" and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
if (
    (parsed.scheme != "https" and not loopback_http)
    or not parsed.hostname
    or parsed.username
    or parsed.password
    or parsed.path not in {"", "/"}
    or parsed.query
    or parsed.fragment
    or any(ord(character) < 33 for character in parsed.netloc)
):
    raise SystemExit(1)
print(f"{parsed.scheme}://{parsed.netloc}")
' "$app_url")"; then
        echo "✗ Enter a domain or HTTPS URL without a path, such as app.example.com." >&2
        return 1
    fi
    printf '%s\n' "$normalized"
}

phone_app_origin_serves_herdr() {
    local origin="$1"
    local manifest

    if ! manifest="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            "$origin/manifest.webmanifest" 2>/dev/null
    )"; then
        return 1
    fi
    printf '%s\n' "$manifest" \
        | grep -Eq '"name"[[:space:]]*:[[:space:]]*"Herdr Mobile Relay"'
}

choose_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local setup_kind="${3:-stable}"
    local choice
    local confirmation
    local current_origin=""
    local entered_url
    local normalized
    local recorded_origin

    recorded_origin="$(dirname "$env_file")/phone-app-origin"
    if [ -n "${HERDR_PHONE_APP_URL:-}" ] || [ ! -t 0 ]; then
        phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi

    echo "Where should the phone setup link open?" >&2
    echo "" >&2
    if [ -s "$recorded_origin" ]; then
        if current_origin="$(phone_app_base_url "$relay_fallback" "$env_file")"; then
            echo "  Current phone app: $current_origin" >&2
            echo "  Press Enter to keep it, or choose another option below." >&2
            echo "" >&2
        else
            echo "  The saved phone app address is invalid; choose a replacement." >&2
            echo "" >&2
        fi
    fi
    if [ "$setup_kind" = "temporary" ]; then
        echo "  1. This temporary relay (recommended for trying one relay)" >&2
        echo "     Opens the TryCloudflare app. Its address changes after restart." >&2
    else
        echo "  1. This relay (recommended for one relay)" >&2
        echo "     Uses this computer's verified hostname as the installed app." >&2
    fi
    echo "" >&2
    echo "  2. An existing installed Herdr app" >&2
    echo "     Adds this computer to the same app as your other relays." >&2
    echo "" >&2
    while true; do
        if [ -n "$current_origin" ]; then
            read -r -p "Choice [keep current]: " choice || choice="cancel"
        else
            read -r -p "Choice [1]: " choice || choice="cancel"
        fi
        if [ "$choice" = "cancel" ]; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        if [ -z "$choice" ] && [ -n "$current_origin" ]; then
            printf '%s\n' "$current_origin"
            return
        fi
        case "${choice:-1}" in
            1)
                HERDR_PHONE_APP_URL=relay phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            2)
                while true; do
                    if ! read -r -p "Installed app domain or URL (for example, app.example.com): " entered_url; then
                        echo "" >&2
                        echo "Setup cancelled." >&2
                        return 1
                    fi
                    if [ -z "$entered_url" ]; then
                        echo "✗ Enter the domain shown in the installed app's Site settings." >&2
                        continue
                    fi
                    if ! normalized="$(
                        HERDR_PHONE_APP_URL="$entered_url" \
                            phone_app_base_url "$relay_fallback" "$env_file"
                    )"; then
                        continue
                    fi
                    if ! phone_app_origin_serves_herdr "$normalized"; then
                        echo "✗ No Herdr app was found at $normalized." >&2
                        echo "  Enter the exact domain shown in the installed app's Site settings." >&2
                        if ! read -r -p "Use this address anyway? [y/N]: " confirmation; then
                            echo "" >&2
                            echo "Setup cancelled." >&2
                            return 1
                        fi
                        case "$confirmation" in
                            y|Y|yes|YES|Yes)
                                ;;
                            *)
                                continue
                                ;;
                        esac
                    fi
                    printf '%s\n' "$normalized"
                    return
                done
                ;;
            *)
                echo "✗ Choose 1 or 2." >&2
                ;;
        esac
    done
}

record_phone_app_origin() {
    local origin="$1"
    local env_file="$2"
    local target
    local temporary

    if [ -z "$env_file" ]; then
        echo "✗ Cannot record the phone app origin without a relay environment path." >&2
        return 1
    fi
    target="$(dirname "$env_file")/phone-app-origin"
    temporary="$target.tmp.$$"
    (
        umask 077
        trap 'rm -f "$temporary"' EXIT
        printf '%s\n' "$origin" > "$temporary"
        chmod 600 "$temporary"
        mv "$temporary" "$target"
        trap - EXIT
    )
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

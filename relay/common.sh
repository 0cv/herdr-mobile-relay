#!/bin/bash

relay_release_root() {
    printf '%s\n' "${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay}"
}

relay_binary() {
    local binary
    local common_dir
    local packaged_binary

    common_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    packaged_binary="$(dirname "$common_dir")/herdr-mobile-relay"
    if [ -f "$(dirname "$common_dir")/release-manifest.json" ] &&
       [ -x "$packaged_binary" ]; then
        binary="$packaged_binary"
    else
        binary="${HERDR_RELAY_BIN:-$(relay_release_root)/current/herdr-mobile-relay}"
    fi
    if [ ! -x "$binary" ]; then
        echo "✗ Verified relay release is unavailable: $binary" >&2
        echo "  Reinstall the exact plugin version; production launchers do not build or fall back." >&2
        return 1
    fi
    printf '%s\n' "$binary"
}

# The plugin is installed from a git clone, so its verified release belongs to
# the repository that checkout points at: a fork or a private canary installs
# its own bundle instead of this project's. Anything that is not a plain GitHub
# owner/repo fails, leaving the installer's compiled default in place.
release_repository() {
    local checkout="$1"
    local url
    local owner_repo

    command -v git >/dev/null 2>&1 || return 1
    url="$(git -C "$checkout" remote get-url origin 2>/dev/null)" || return 1
    case "$url" in
        *github.com[:/]*) owner_repo="${url##*github.com}" ;;
        *) return 1 ;;
    esac
    owner_repo="${owner_repo#[:/]}"
    owner_repo="${owner_repo%.git}"
    case "$owner_repo" in
        */*/* | /* | */ | *[!A-Za-z0-9._/-]*) return 1 ;;
        */*) printf '%s\n' "$owner_repo" ;;
        *) return 1 ;;
    esac
}

# Resolves a key a person named at a prompt. Keys live in ~/.ssh, so a bare
# name means one of those, not a file in whatever directory the setup menu was
# started from; an explicit path, absolute or relative, is taken as typed.
ssh_key_path() {
    local entered="$1"
    local candidate

    [ -n "$entered" ] || return 1
    candidate="${entered/#\~/$HOME}"
    if [ -r "$candidate" ] && [ ! -d "$candidate" ]; then
        printf '%s\n' "$candidate"
        return 0
    fi
    case "$candidate" in
        */*) return 1 ;;
    esac
    candidate="$HOME/.ssh/$entered"
    [ -r "$candidate" ] && [ ! -d "$candidate" ] || return 1
    printf '%s\n' "$candidate"
}

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

update_launchd_release_paths() {
    local plist="$1"
    local service_wrapper="$2"
    local work_dir="$3"
    local env_file="${4:-}"
    local plist_buddy="${HERDR_PLIST_BUDDY:-/usr/libexec/PlistBuddy}"

    [ -x "$plist_buddy" ] || {
        echo "PlistBuddy is unavailable: $plist_buddy" >&2
        return 1
    }
    "$plist_buddy" -c "Set :ProgramArguments:0 $service_wrapper" "$plist"
    "$plist_buddy" -c "Set :WorkingDirectory $work_dir" "$plist"
    if [ -n "$env_file" ]; then
        "$plist_buddy" -c "Set :EnvironmentVariables:HERDR_RELAY_ENV $env_file" "$plist"
    fi
}

require_user_service_context() {
    if [ "$(id -u)" -ne 0 ]; then
        return
    fi

    echo "Refusing to manage the Herdr Mobile Relay user service as root." >&2
    echo "Run the command again as the signed-in macOS or Linux user, without sudo." >&2
    return 1
}

launchd_service_loaded() {
    local service_target="$1"
    launchctl print "$service_target" >/dev/null 2>&1
}

reload_launchd_service_definition() {
    local plist="$1"
    local label="$2"
    local domain="gui/$(id -u)"
    local service_target="$domain/$label"
    local attempt
    local unloaded=false
    local bootstrapped=false

    require_user_service_context || return 1
    [ -f "$plist" ] && [ ! -L "$plist" ] || {
        echo "Cannot reload launchd service: plist is not a regular file: $plist" >&2
        return 1
    }
    if command -v plutil >/dev/null 2>&1; then
        plutil -lint "$plist" >/dev/null || {
            echo "Cannot reload launchd service: plist validation failed: $plist" >&2
            return 1
        }
    fi

    # A migration changes ProgramArguments, WorkingDirectory, and the relay
    # environment. Unload the plist using the form used by the legacy service
    # installer, then wait until launchd has actually removed its cached job.
    if launchd_service_loaded "$service_target"; then
        if ! launchctl bootout "$domain" "$plist"; then
            launchctl bootout "$service_target" || {
                echo "Could not unload launchd service $service_target" >&2
                return 1
            }
        fi
        for attempt in 1 2 3 4 5 6 7 8 9 10; do
            if ! launchd_service_loaded "$service_target"; then
                unloaded=true
                break
            fi
            sleep 1
        done
        if [ "$unloaded" != true ]; then
            echo "Timed out waiting for launchd to unload $service_target" >&2
            return 1
        fi
    fi

    # launchd can briefly reject bootstrap while completing a bootout. Retry
    # the registration, accepting success only when the exact job is loaded.
    for attempt in 1 2 3 4 5; do
        if [ "$attempt" -eq 5 ]; then
            if launchctl bootstrap "$domain" "$plist"; then
                bootstrapped=true
            fi
        elif launchctl bootstrap "$domain" "$plist" >/dev/null 2>&1; then
            bootstrapped=true
        fi
        if [ "$bootstrapped" = true ] ||
           launchd_service_loaded "$service_target"; then
            bootstrapped=true
            break
        fi
        sleep 1
    done
    if [ "$bootstrapped" != true ]; then
        echo "Could not bootstrap launchd service $service_target" >&2
        return 1
    fi

    launchctl enable "$service_target"
    launchctl kickstart -k "$service_target"
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

remove_env_value_atomic() {
    local env_file="$1"
    local key="$2"
    local directory
    local temp_file

    if [ ! -f "$env_file" ] || ! grep -q "^${key}=" "$env_file"; then
        return
    fi
    directory="$(dirname "$env_file")"
    temp_file="$(mktemp "$directory/.relay-env.XXXXXX")"
    grep -v "^${key}=" "$env_file" > "$temp_file" || true
    chmod 600 "$temp_file"
    mv "$temp_file" "$env_file"
}

persist_github_token() {
    local env_file="$1"
    local token_file
    local temp_file

    if [ -z "${GH_TOKEN:-}" ]; then
        return 0
    fi
    token_file="$(dirname "$env_file")/github-token"
    temp_file="$(mktemp "$(dirname "$env_file")/.github-token.XXXXXX")"
    printf '%s\n' "$GH_TOKEN" > "$temp_file"
    chmod 600 "$temp_file"
    mv "$temp_file" "$token_file"
    set_env_value_atomic "$env_file" HERDR_GITHUB_TOKEN_FILE "$token_file"
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
    persist_github_token "$env_file"
    # Migrate older installs that exposed the token to the complete service
    # process tree. Only the credential-file path remains in relay.env.
    remove_env_value_atomic "$env_file" GH_TOKEN
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
                *'"status": "ok"'*|*'"status":"ok"'*)
                    if [[ "$health" == *'"instance":'* && "$health" == *'"version":'* && "$health" == *'"protocol":'* ]]; then
                        printf '%s\n' "$health"
                        return 0
                    fi
                    ;;
            esac
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

json_string_field() {
    local json="$1"
    local key="$2"
    printf '%s\n' "$json" |
        sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" |
        head -1
}

# Reads the relay's own /healthz gateway object, which reports
# {"enabled":bool,"registered":bool,"relay_id":"...","clients":int}. Only the
# registered flag is echoed; the relay id stays out of terminal output.
gateway_registration_state() {
    local health="$1"
    printf '%s\n' "$health" |
        tr -d ' \t\n' |
        sed -n 's/.*"gateway":{\([^}]*\)}.*/\1/p' |
        sed -n 's/.*"registered":\([a-z]*\).*/\1/p' |
        head -1
}

# Blocks until the relay reports a live gateway registration, so the QR is only
# printed for a relay the phone can actually reach.
wait_for_gateway_registration() {
    local port="${1:-8375}"
    local attempts="${2:-30}"
    local delay="${3:-1}"
    local attempt
    local health

    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required to verify the gateway registration." >&2
        return 1
    fi

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Gateway registration attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null)"; then
            if [ "$(gateway_registration_state "$health")" = "true" ]; then
                return 0
            fi
        fi
        if [ "$attempt" -lt "$attempts" ]; then
            sleep "$delay"
        fi
    done

    return 1
}

verify_relay_release_health() {
    local health="$1"
    local expected_version="$2"
    local expected_revision="$3"
    local expected_web_hash="$4"

    [ "$(json_string_field "$health" status)" = "ok" ] &&
        [ "$(json_string_field "$health" release_version)" = "$expected_version" ] &&
        [ "$(json_string_field "$health" revision)" = "$expected_revision" ] &&
        [ "$(json_string_field "$health" bundle_hash)" = "$expected_web_hash" ]
}

wait_for_relay_release_health() {
    local port="$1"
    local attempts="$2"
    local delay="$3"
    local expected_version="$4"
    local expected_revision="$5"
    local expected_web_hash="$6"
    local health
    local attempt

    [ -n "$expected_version" ] &&
        [ -n "$expected_revision" ] &&
        [ -n "$expected_web_hash" ] || {
            echo "Exact release health verification requires version, revision, and web hash." >&2
            return 1
        }

    case "$attempts" in
        ""|*[!0-9]*|0)
            echo "Health-check attempts must be a positive integer." >&2
            return 1
            ;;
    esac

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if health="$(wait_for_relay_health "$port" 1 0)" &&
           verify_relay_release_health \
               "$health" "$expected_version" "$expected_revision" "$expected_web_hash"; then
            printf '%s\n' "$health"
            return 0
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

# The token passes through argv only for the short-lived compiled helper.
build_setup_fragment() {
    "$(relay_binary)" setup-fragment "$1" "$2" "${3:-}"
}

# The configured blind gateway base URLs as a comma-separated candidate list —
# the same shape HERDR_GATEWAY_URL takes. The relay measures healthy candidates
# concurrently; configured order breaks close ties and is the fallback when no
# probe succeeds. Empty means the relay keeps using a Cloudflare tunnel.
gateway_urls() {
    local env_file="${1:-${HERDR_RELAY_ENV:-}}"
    local raw="${HERDR_GATEWAY_URL:-}"
    local old_ifs
    local list=""
    local entry

    if [ -z "$raw" ] && [ -n "$env_file" ]; then
        raw="$(env_file_value "$env_file" HERDR_GATEWAY_URL)"
    fi
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $raw
    IFS="$old_ifs"
    for entry in "$@"; do
        # A URL never contains whitespace, so dropping all of it is the same as
        # trimming what a hand-edited env file leaves around a comma.
        entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
        entry="${entry%/}"
        if [ -n "$entry" ]; then
            list="${list:+$list,}$entry"
        fi
    done
    printf '%s\n' "$list"
}

# The configured tie-break leader. Terminal output and the gateway= fragment key
# each name exactly one gateway; the live relay advertises its selected entry.
gateway_url() {
    local list

    list="$(gateway_urls "${1:-}")"
    printf '%s\n' "${list%%,*}"
}

# The gateway candidates this project operates for the community. They are
# compiled in so the free shared path costs a user no hostname, no account, and
# no typing: picking it in the chooser is the whole setup. The value has the
# same comma-separated shape as HERDR_GATEWAY_URL. An operator points elsewhere
# with HERDR_COMMUNITY_GATEWAY_URL; an explicitly empty value means "no
# community gateway", which is how a test or fork switches the option off.
HERDR_COMMUNITY_GATEWAY_DEFAULT="wss://gw1.herdr-mobile.dev,wss://gw2.herdr-mobile.dev"

community_gateway_url() {
    if [ "${HERDR_COMMUNITY_GATEWAY_URL+set}" = "set" ]; then
        printf '%s\n' "$HERDR_COMMUNITY_GATEWAY_URL"
        return
    fi
    printf '%s\n' "$HERDR_COMMUNITY_GATEWAY_DEFAULT"
}

# Canonicalizes anything a person might reasonably type — gw.example.com,
# https://gw.example.com, wss://gw.example.com — into the wss:// base URL the
# relay and the QR fragment use. It reuses the compiled origin normalizer, so a
# gateway URL is held to the same rules as the phone app origin: no
# credentials, no path, no query, no fragment.
normalize_gateway_url() {
    local input="$1"
    local origin

    case "$input" in
        wss://*) input="https://${input#wss://}" ;;
        ws://*) input="http://${input#ws://}" ;;
    esac
    if ! origin="$("$(relay_binary)" normalize-origin --allow-loopback-http "$input")"; then
        return 1
    fi
    case "$origin" in
        https://*) printf 'wss://%s\n' "${origin#https://}" ;;
        http://*) printf 'ws://%s\n' "${origin#http://}" ;;
        *) return 1 ;;
    esac
}

# Canonicalizes and deduplicates a comma-separated candidate list. One invalid
# entry rejects the choice: silently dropping a typo would make the advertised
# disaster-recovery path look configured when it is not.
normalize_gateway_urls() {
    local raw="$1"
    local old_ifs
    local entry
    local normalized
    local list=""

    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $raw
    IFS="$old_ifs"
    for entry in "$@"; do
        entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
        if ! normalized="$(normalize_gateway_url "$entry")"; then
            return 1
        fi
        case ",$list," in
            *,"$normalized",*) ;;
            *) list="${list:+$list,}$normalized" ;;
        esac
    done
    [ -n "$list" ] || return 1
    printf '%s\n' "$list"
}

# The HTTPS base for the gateway's own endpoints: /healthz, /probe, /whoami.
gateway_http_base() {
    case "$1" in
        wss://*) printf 'https://%s\n' "${1#wss://}" ;;
        ws://*) printf 'http://%s\n' "${1#ws://}" ;;
        *) printf '%s\n' "$1" ;;
    esac
}

# Confirms a gateway is actually answering before its URL is written into the
# relay environment, so a typo or a dead host fails during setup rather than at
# the first phone connection.
gateway_answers_healthz() {
    local body

    if ! body="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            "$(gateway_http_base "$1")/healthz" 2>/dev/null
    )"; then
        return 1
    fi
    printf '%s\n' "$body" | grep -Eq '"ok"[[:space:]]*:[[:space:]]*true'
}

# Persists the transport choice. An empty URL removes the key, which returns
# the relay to the Cloudflare tunnel path — and takes the selection policy with
# it, so a relay that leaves the gateway path keeps no stale policy to revive
# behind a later, differently chosen list.
set_gateway_url() {
    local env_file="$1"
    local url="$2"

    if [ -z "$url" ]; then
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_URL
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION
        return 0
    fi
    set_env_value_atomic "$env_file" HERDR_GATEWAY_URL "$url"
}

# Records how the relay picks among the configured candidates. "ordered" keeps
# the configured order, so an operator who lists their own gateway first keeps
# it even when a community one answers faster; "latency" ranks by measured RTT,
# which is the point for the interchangeable public candidates. The relay
# defaults to "ordered" when the key is absent, so only the community path has
# to write anything. Any other value is a caller bug that would quietly change
# which gateway carries traffic, so nothing is written.
set_gateway_selection() {
    local env_file="$1"
    local selection="$2"

    case "$selection" in
        ordered|latency) ;;
        *) return 1 ;;
    esac
    set_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION "$selection"
}

# Percent-encodes one fragment value with the compiled encoder, so an entry of
# the gateway list is escaped exactly like the keys the phone already parses.
# Values escape '=' and '&', so the only literal "relay=" in the helper's output
# is the key whose value is being extracted.
encode_fragment_value() {
    build_setup_fragment "" "" "$1" | sed -e 's/.*relay=//' -e 's/&.*//'
}

# build_setup_fragment for either transport: a gateway-configured relay is
# reached through the gateway, so the fragment carries gateway=<url> instead of
# relay=<wss url>, plus gateways=<the whole ordered list>. The compiled helper
# still does the percent-encoding, and because it escapes '=' inside values, the
# only "relay=" in its output is the key itself. All secrets stay inside the
# fragment either way.
build_transport_setup_fragment() {
    local token="$1"
    local label="$2"
    local relay_url="${3:-}"
    local gateways
    local fragment
    local encoded=""
    local old_ifs
    local entry

    gateways="$(gateway_urls)"
    if [ -z "$gateways" ]; then
        build_setup_fragment "$token" "$label" "$relay_url"
        return
    fi
    fragment="$(build_setup_fragment "$token" "$label" "${gateways%%,*}" |
        sed -e 's/^relay=/gateway=/' -e 's/&relay=/\&gateway=/')"
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $gateways
    IFS="$old_ifs"
    for entry in "$@"; do
        encoded="${encoded:+$encoded,}$(encode_fragment_value "$entry")"
    done
    # The complete list travels even when it holds a single entry, so a relay
    # that gains a second gateway later costs no paired phone a re-scan.
    printf '%s&gateways=%s\n' "$fragment" "$encoded"
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
    if ! normalized="$("$(relay_binary)" normalize-origin --allow-loopback-http "$app_url")"; then
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

# Asks for the origin of an already-installed Herdr app and validates it with
# the same normalizer the setup link uses. Shared by the tunnel chooser below
# and the gateway path, which has no relay-served origin of its own.
prompt_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local confirmation
    local entered_url
    local normalized

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
        return 0
    done
}

# The phone app origin for a gateway-configured relay. The gateway only copies
# encrypted frames, so it serves no app and there is no relay hostname to fall
# back to: the origin has to be recorded, configured, or entered.
gateway_phone_app_base_url() {
    local env_file="$1"
    local base

    if base="$(phone_app_base_url "" "$env_file" 2>/dev/null)" && [ -n "$base" ]; then
        printf '%s\n' "$base"
        return 0
    fi
    if [ -n "${HERDR_PHONE_APP_URL:-}" ] || [ ! -t 0 ]; then
        echo "✗ Set HERDR_PHONE_APP_URL to the HTTPS origin that serves the Herdr phone app." >&2
        echo "  The gateway carries relay traffic only; it does not host the app." >&2
        return 1
    fi
    echo "The gateway carries relay traffic only, so the phone app needs its own" >&2
    echo "HTTPS origin. Enter an installed Herdr app, or host one with make web-deploy." >&2
    echo "" >&2
    prompt_phone_app_base_url "" "$env_file"
}

choose_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local setup_kind="${3:-stable}"
    local choice
    local current_origin=""
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
                prompt_phone_app_base_url "$relay_fallback" "$env_file"
                return
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
# be drawn because the terminal is too narrow. A wrapped QR is worse than the
# plain link.
# Callers must keep working with empty output. Kept separate from
# build_setup_fragment on purpose: this call is allowed to fail, that one
# is not.
render_setup_qr() {
    local url="$1"
    local cols
    cols="$(tput cols 2>/dev/null || true)"
    "$(relay_binary)" qr --columns "${cols:-80}" "$url" 2>/dev/null || true
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

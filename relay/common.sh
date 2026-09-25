#!/bin/bash

# Keep only terminal generations already validated by jobs -l and passed to
# wait; a later stop may accept their completion after Bash prunes its record.
CHILD_JOB_REAPED_SPECS=()
CHILD_JOB_REAPED_PIDS=()
CHILD_JOB_RECORD_PRESENT=false

# A numeric PID can be reused as soon as its process exits. Bash job specs are
# tied to this shell's job table, so managed foreground children keep both the
# jobspec generation and the original PID for identity checks and wait only.
# Capture jobs output through a file because command substitution runs the
# builtin in a subshell whose copied job table can omit terminal records.
_child_job_long_record() {
    local job_spec="$1"
    local output_file
    local line
    local line_count=0

    CHILD_JOB_OUTPUT=""
    CHILD_JOB_RECORD_PRESENT=false
    output_file="$(mktemp "${TMPDIR:-/tmp}/herdr-child-job.XXXXXX")" || return 1
    if ! LC_ALL=C jobs -l "$job_spec" > "$output_file" 2>/dev/null; then
        rm -f "$output_file"
        return 1
    fi
    while IFS= read -r line || [ -n "$line" ]; do
        line_count=$((line_count + 1))
        if [ "$line_count" -eq 1 ]; then
            CHILD_JOB_OUTPUT="$line"
        fi
    done < "$output_file"
    rm -f "$output_file" || return 1
    [ "$line_count" -eq 0 ] || CHILD_JOB_RECORD_PRESENT=true
    [ "$line_count" -eq 1 ] && [ -n "$CHILD_JOB_OUTPUT" ]
}

_child_job_record_state() {
    local expected_pid="$1"
    local job_number="$2"
    local job_output="$3"
    local job_token
    local listed_pid
    local state

    [ -n "$job_output" ] || return 1
    case "$job_output" in
        *$'\n'*) return 1 ;;
    esac
    IFS=$' \t' read -r job_token listed_pid state _ <<< "$job_output"
    case "$job_token" in
        "[$job_number]"|"[$job_number]+"|"[$job_number]-") ;;
        *) return 1 ;;
    esac
    [ "$listed_pid" = "$expected_pid" ] || return 1

    case "$state" in
        Running|Stopped)
            CHILD_JOB_STATE="$state"
            return 0
            ;;
        Done|Exit|Terminated|Killed|Aborted|Hangup|Segmentation|Floating)
            CHILD_JOB_STATE="$state"
            return 2
            ;;
        *)
            return 1
            ;;
    esac
}

# This cache is never a signal authority. A live or mismatched current record
# still fails identity checks; absence is accepted only for this exact pair
# after a validated terminal record was already waited in this shell.
_child_job_mark_reaped() {
    local job_spec="$1"
    local expected_pid="$2"
    local index

    for ((index = 0; index < ${#CHILD_JOB_REAPED_SPECS[@]}; index++)); do
        if [ "${CHILD_JOB_REAPED_SPECS[index]}" = "$job_spec" ]; then
            CHILD_JOB_REAPED_PIDS[index]="$expected_pid"
            return 0
        fi
    done
    CHILD_JOB_REAPED_SPECS+=("$job_spec")
    CHILD_JOB_REAPED_PIDS+=("$expected_pid")
}

_child_job_was_reaped() {
    local job_spec="$1"
    local expected_pid="$2"
    local index

    for ((index = 0; index < ${#CHILD_JOB_REAPED_SPECS[@]}; index++)); do
        if [ "${CHILD_JOB_REAPED_SPECS[index]}" = "$job_spec" ] &&
            [ "${CHILD_JOB_REAPED_PIDS[index]}" = "$expected_pid" ]; then
            return 0
        fi
    done
    return 1
}

_child_job_details() {
    local job_spec="$1"
    local expected_pid="$2"
    local job_number
    local mapped_pid
    local job_output
    local record_status
    local CHILD_JOB_OUTPUT=""

    case "$job_spec" in
        %[0-9]*) job_number="${job_spec#%}" ;;
        *) return 1 ;;
    esac
    case "$job_number" in
        ''|*[!0-9]*) return 1 ;;
    esac
    case "$expected_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac

    _child_job_long_record "$job_spec" || return 1
    job_output="$CHILD_JOB_OUTPUT"
    if _child_job_record_state "$expected_pid" "$job_number" "$job_output"; then
        record_status=0
    else
        record_status=$?
    fi
    [ "$record_status" -eq 2 ] && return 2
    [ "$record_status" -eq 0 ] || return 1

    case "$CHILD_JOB_STATE" in
        Running|Stopped) ;;
        *) return 1 ;;
    esac
    if mapped_pid="$(LC_ALL=C jobs -p "$job_spec" 2>/dev/null)"; then
        [ "$mapped_pid" = "$expected_pid" ] && return 0
        [ -z "$mapped_pid" ] || return 1
    else
        [ -z "$mapped_pid" ] || return 1
    fi

    # A child may finish after the long-form record was read but before Bash
    # resolves the live PID mapping. Reclassify once, from the fresh job record;
    # missing or still-active records remain an unknown-identity refusal.
    _child_job_long_record "$job_spec" || return 1
    job_output="$CHILD_JOB_OUTPUT"
    if _child_job_record_state "$expected_pid" "$job_number" "$job_output"; then
        return 1
    else
        record_status=$?
    fi
    [ "$record_status" -eq 2 ] && return 2
    return 1
}

capture_child_job() {
    local expected_pid="$1"
    local job_output
    local job_token
    local listed_pid
    local state
    local job_number
    local job_spec
    local mapped_pid

    case "$expected_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    job_output="$(LC_ALL=C jobs -l %+ 2>/dev/null)" || return 1
    IFS=' ' read -r job_token listed_pid state _ <<< "$job_output"
    case "$job_token" in
        \[*\]+|\[*\]) job_number="${job_token#\[}"; job_number="${job_number%%\]*}" ;;
        *) return 1 ;;
    esac
    case "$job_number" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ "$listed_pid" = "$expected_pid" ] || return 1
    case "$state" in
        Running|Stopped) ;;
        *) return 1 ;;
    esac

    job_spec="%$job_number"
    mapped_pid="$(LC_ALL=C jobs -p "$job_spec" 2>/dev/null)" || return 1
    [ "$mapped_pid" = "$expected_pid" ] || return 1
    CHILD_JOB_STATE=""
    _child_job_details "$job_spec" "$expected_pid" || return 1
    printf '%s\n' "$job_spec"
}

child_job_active() {
    local CHILD_JOB_STATE=""
    local details_status

    if _child_job_details "$1" "$2"; then
        return 0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        # Reap only after the exact jobspec/PID terminal record was validated.
        # Bash may prune that record before the launcher's EXIT cleanup runs.
        wait "$1" 2>/dev/null || true
        _child_job_mark_reaped "$1" "$2"
    fi
    return 1
}

_child_job_reap_forced_kill() {
    local job_spec="$1"
    local expected_pid="$2"
    local observation_limit=5
    local attempt
    local details_status
    local wait_status
    local CHILD_JOB_STATE=""

    # Five fresh observations with at most four 100ms sleeps bound this proof
    # to 400ms after KILL; no further signal is sent from this loop.
    for ((attempt = 1; attempt <= observation_limit; attempt++)); do
        if _child_job_details "$job_spec" "$expected_pid"; then
            details_status=0
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            [ "$CHILD_JOB_STATE" = Killed ] || return 1
            if wait "$expected_pid" 2>/dev/null; then
                wait_status=0
            else
                wait_status=$?
            fi
            [ "$wait_status" -eq 137 ] || return 1
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        [ "$details_status" -eq 0 ] || return 1
        case "$CHILD_JOB_STATE" in
            Running|Stopped) ;;
            *) return 1 ;;
        esac
        if [ "$attempt" -lt "$observation_limit" ]; then
            sleep 0.1
        fi
    done
    return 1
}

stop_child_job() {
    local job_spec="$1"
    local expected_pid="$2"
    local signal="$3"
    local attempts="${4:-5}"
    local attempt
    local details_status
    local CHILD_JOB_STATE=""

    case "$signal" in
        ''|*[!A-Za-z0-9]*) return 1 ;;
    esac
    case "$attempts" in
        ''|*[!0-9]*|0) return 1 ;;
    esac

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    if [ "$details_status" -ne 0 ]; then
        if [ "$CHILD_JOB_RECORD_PRESENT" != true ] &&
            _child_job_was_reaped "$job_spec" "$expected_pid"; then
            return 0
        fi
        return 1
    fi

    if [ "$CHILD_JOB_STATE" = Stopped ]; then
        if _child_job_details "$job_spec" "$expected_pid"; then
            details_status=0
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            wait "$job_spec" 2>/dev/null || true
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        [ "$details_status" -eq 0 ] || return 1
        [ "$CHILD_JOB_STATE" = Stopped ] || return 1
        kill -CONT "$job_spec" 2>/dev/null || return 1
    fi

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    [ "$details_status" -eq 0 ] || return 1
    [ "$CHILD_JOB_STATE" = Running ] || return 1
    kill "-$signal" "$job_spec" 2>/dev/null || return 1

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        sleep 1
        if _child_job_details "$job_spec" "$expected_pid"; then
            continue
        else
            details_status=$?
        fi
        if [ "$details_status" -eq 2 ]; then
            wait "$job_spec" 2>/dev/null || true
            _child_job_mark_reaped "$job_spec" "$expected_pid"
            return 0
        fi
        return 1
    done

    if _child_job_details "$job_spec" "$expected_pid"; then
        details_status=0
    else
        details_status=$?
    fi
    if [ "$details_status" -eq 2 ]; then
        wait "$job_spec" 2>/dev/null || true
        _child_job_mark_reaped "$job_spec" "$expected_pid"
        return 0
    fi
    [ "$details_status" -eq 0 ] || return 1
    kill -KILL "$job_spec" 2>/dev/null || return 1
    _child_job_reap_forced_kill "$job_spec" "$expected_pid"
}

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

# Resolve the intended path without creating, chmodding or migrating anything.
# Pre-consent readers must not use the mutating setup resolver below.
relay_env_file_read_only() {
    local script_dir="$1"

    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        printf '%s\n' "$HERDR_RELAY_ENV"
    elif [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        printf '%s/relay.env\n' "$HERDR_PLUGIN_CONFIG_DIR"
    else
        printf '%s/.env\n' "$script_dir"
    fi
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
yaml_scalar() {
    local key="$1"
    local config="$2"
    local value

    value="$(sed -nE "s/^[[:space:]]*-?[[:space:]]*${key}:[[:space:]]*([^#]+).*/\\1/p" "$config" | head -1)"
    value="$(printf '%s' "$value" | sed 's/[[:space:]]*$//')"
    value="${value#\"}"
    value="${value%\"}"
    value="${value#\'}"
    value="${value%\'}"
    printf '%s\n' "$value"
}

expand_config_path() {
    local path="$1"
    local config="$2"

    case "$path" in
        \~/*) path="$HOME/${path#\~/}" ;;
        \$HOME/*) path="$HOME/${path#\$HOME/}" ;;
        /*) ;;
        *) path="$(dirname "$config")/$path" ;;
    esac
    canonical_file_path "$path"
}
valid_hostname() {
    local hostname="$1"
    local old_ifs
    local label

    [ "${#hostname}" -le 253 ] || return 1
    case "$hostname" in
        ""|.*|*.|*..*|*[!A-Za-z0-9.-]*) return 1 ;;
    esac
    old_ifs="$IFS"
    IFS=.
    # shellcheck disable=SC2086
    set -- $hostname
    IFS="$old_ifs"
    for label in "$@"; do
        [ "${#label}" -le 63 ] || return 1
        case "$label" in
            ""|-*|*-) return 1 ;;
        esac
    done
}

read_cloudflared_relay_config() {
    local config="$1"
    local expected_port="$2"
    local configured_tunnel
    local credentials_value
    local service_url
    local origin

    if [ ! -r "$config" ]; then
        echo "✗ Cloudflare tunnel config is not readable: $config" >&2
        return 1
    fi
    if ! cloudflared tunnel --config "$config" ingress validate; then
        echo "✗ cloudflared rejected the ingress syntax in $config." >&2
        return 1
    fi

    configured_tunnel="$(yaml_scalar tunnel "$config")"
    credentials_value="$(yaml_scalar credentials-file "$config")"
    CONFIG_HOST="$(yaml_scalar hostname "$config")"
    service_url="$(yaml_scalar service "$config")"
    if [ -z "$configured_tunnel" ]; then
        echo "✗ No tunnel identifier found in $config." >&2
        return 1
    fi
    if [ -z "$credentials_value" ]; then
        echo "✗ No credentials-file found in $config." >&2
        return 1
    fi
    if [ -z "$CONFIG_HOST" ] || ! valid_hostname "$CONFIG_HOST"; then
        echo "✗ No valid ingress hostname found in $config." >&2
        return 1
    fi
    if [[ "$service_url" != http://* ]]; then
        echo "✗ The first ingress origin in $config is not an HTTP loopback service." >&2
        return 1
    fi
    origin="${service_url#http://}"
    origin="${origin%%/*}"
    case "$origin" in
        "127.0.0.1:$expected_port"|"localhost:$expected_port") ;;
        *)
            echo "✗ Ingress origin $service_url does not match HERDR_RELAY_PORT=$expected_port." >&2
            return 1
            ;;
    esac

    CREDENTIALS_PATH="$(expand_config_path "$credentials_value" "$config")"
    if [ ! -r "$CREDENTIALS_PATH" ]; then
        echo "✗ Tunnel credentials are not readable: $CREDENTIALS_PATH" >&2
        return 1
    fi
    TUNNEL_UUID="$("$(relay_binary)" stable-state credential-id "$CREDENTIALS_PATH")"
    if [[ "$configured_tunnel" =~ ^[0-9a-fA-F-]{36}$ ]] &&
       [ "$(printf '%s' "$configured_tunnel" | tr '[:upper:]' '[:lower:]')" != "$TUNNEL_UUID" ]; then
        echo "✗ Config tunnel $configured_tunnel does not match credentials for $TUNNEL_UUID." >&2
        return 1
    fi
    TUNNEL_NAME="$configured_tunnel"
}

# The origin certificate cloudflared should manage tunnels with: an explicit
# TUNNEL_ORIGIN_CERT wins, then the config's own origincert, then cloudflared's
# default location. A path is always printed even when nothing is there to read,
# because the callers differ on what an unreadable certificate means — setup
# skips its existence check, teardown refuses.
cloudflared_origin_cert() {
    local config="$1"
    local certificate_value

    if [ -n "${TUNNEL_ORIGIN_CERT:-}" ]; then
        printf '%s\n' "$TUNNEL_ORIGIN_CERT"
        return
    fi
    certificate_value="$(yaml_scalar origincert "$config")"
    if [ -n "$certificate_value" ]; then
        expand_config_path "$certificate_value" "$config"
        return
    fi
    printf '%s\n' "$HOME/.cloudflared/cert.pem"
}

# The Cloudflare name of a tunnel UUID. A generated config records the UUID as
# its `tunnel:` scalar, so anything that reasons about the tunnel *name* — the
# Herdr namespace guard above all — has to ask Cloudflare which name the id was
# created with. Fails when the certificate cannot authorize the lookup, when
# Cloudflare refuses it, or when the id names no live tunnel.
cloudflared_tunnel_name_by_id() {
    local uuid="$1"
    local origin_cert="$2"
    local list_output
    local name

    [ -r "$origin_cert" ] || return 1
    list_output="$(mktemp "${TMPDIR:-/tmp}/herdr-tunnel-list.XXXXXX")" || return 1
    if ! cloudflared tunnel --origincert "$origin_cert" list --id "$uuid" --output json > "$list_output"; then
        rm -f "$list_output"
        return 1
    fi
    name="$("$(relay_binary)" stable-state tunnel-name-by-id "$list_output" "$uuid")" || name=""
    rm -f "$list_output"
    [ -n "$name" ] || return 1
    printf '%s\n' "$name"
}

cloudflare_cert_zone_name() {
    local origin_cert="$1"
    local payload
    local zone_id
    local api_token

    [ -r "$origin_cert" ] || return 1
    payload="$(
        sed -n '/BEGIN ARGO TUNNEL TOKEN/,/END ARGO TUNNEL TOKEN/p' "$origin_cert" |
            sed '1d;$d' | tr -d '\n' | base64 -d 2>/dev/null
    )" || return 1
    zone_id="$(printf '%s' "$payload" | sed -n 's/.*"zoneID":"\([^"]*\)".*/\1/p')"
    api_token="$(printf '%s' "$payload" | sed -n 's/.*"apiToken":"\([^"]*\)".*/\1/p')"
    [ -n "$zone_id" ] && [ -n "$api_token" ] || return 1
    curl --fail --silent --show-error --max-time 10 \
        -H "Authorization: Bearer $api_token" \
        "https://api.cloudflare.com/client/v4/zones/$zone_id" 2>/dev/null |
        sed -n 's/.*"result":{"id":"[^"]*","name":"\([^"]*\)".*/\1/p' | head -1
}

relogin_for_cloudflare_zone() {
    local origin_cert="$1"
    local zone="$2"
    local backup="$origin_cert.$(date +%Y%m%d%H%M%S)"

    echo "▸ Signing in to Cloudflare for $zone."
    echo "  The current certificate is kept as $backup."
    mv "$origin_cert" "$backup" || return 1
    if ! cloudflared tunnel login; then
        mv -f "$backup" "$origin_cert"
        echo "✗ Sign-in did not finish; the previous certificate is back." >&2
        return 1
    fi
    if [ ! -r "$origin_cert" ]; then
        mv -f "$backup" "$origin_cert"
        echo "✗ Sign-in produced no certificate; the previous one is back." >&2
        return 1
    fi
}

hostname_in_zone() {
    case "$1" in
        "$2" | *".$2") return 0 ;;
    esac
    return 1
}

cloudflare_routed_hostname() {
    printf '%s\n' "$1" |
        sed -n 's/.*Added CNAME \([^ ]*\) which will route.*/\1/p;s/.*INF \([^ ]*\) is already configured.*/\1/p' |
        head -1
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

installed_relay_service_active() {
    case "$(uname -s)" in
        Darwin)
            launchd_service_loaded "gui/$(id -u)/com.herdr-mobile-relay.service"
            ;;
        Linux)
            command -v systemctl >/dev/null 2>&1 &&
                systemctl --user is-active --quiet herdr-mobile-relay.service
            ;;
        *)
            return 1
            ;;
    esac
}

restart_installed_relay_service() {
    case "$(uname -s)" in
        Darwin)
            launchctl kickstart -k "gui/$(id -u)/com.herdr-mobile-relay.service"
            ;;
        Linux)
            systemctl --user restart herdr-mobile-relay.service
            ;;
        *)
            return 1
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

# A pane invoked straight from herdr has to hold the terminal open long enough
# to be read. Under the setup menu it must not: the menu pauses once on the way
# back, so a second prompt here would cost two keystrokes to return.
pause_before_close() {
    [ "${HERDR_SETUP_MENU:-}" != 1 ] || return 0
    if [ -t 0 ]; then
        echo ""
        read -r -p "Press Enter to close this pane." _answer
    fi
}

# Wrangler refuses to start on an older Node, and npx only says so after a long
# download, so every candidate below is version-checked before it is offered.
NODE_MIN_MAJOR=22

node_major_version() {
    local reported
    reported="$("$1" --version 2>/dev/null)" || return 1
    reported="${reported#v}"
    reported="${reported%%.*}"
    case "$reported" in
        '' | *[!0-9]*) return 1 ;;
    esac
    printf '%s\n' "$reported"
}

# Plugin panes inherit herdr's environment, not an interactive shell's, so a
# node installed by nvm, fnm, volta, or asdf is invisible here even though the
# person's own terminal finds it: the manager's PATH entry comes from a shell rc
# this process never sources. Look where those managers actually put it, prefer
# a "current" link over a pinned version so a node upgrade does not strand the
# recording, and honour what a previous run already recorded.
node_bin_dir() {
    local env_file="${1:-}"
    local candidate
    local major
    local recorded=""
    local on_path=""

    if [ -n "$env_file" ] && [ -f "$env_file" ]; then
        recorded="$(env_file_value "$env_file" HERDR_APP_DEPLOY_NODE_DIR)"
    fi
    if on_path="$(command -v node 2>/dev/null)"; then
        on_path="$(dirname "$on_path")"
    fi
    for candidate in \
        "$recorded" \
        "$on_path" \
        "${NVM_DIR:-$HOME/.nvm}/current/bin" \
        "${FNM_DIR:-$HOME/.local/share/fnm}/aliases/default/bin" \
        "$HOME/.volta/bin" \
        "$HOME/.asdf/shims" \
        "$(ls -d "${NVM_DIR:-$HOME/.nvm}"/versions/node/*/bin 2>/dev/null | sort -V | tail -1)" \
        /opt/homebrew/bin \
        /usr/local/bin \
        "$HOME/.local/bin" \
        /usr/bin; do
        case "$candidate" in
            /*) ;;
            *) continue ;;
        esac
        [ -x "$candidate/node" ] && [ -x "$candidate/npx" ] || continue
        major="$(node_major_version "$candidate/node")" || continue
        [ "$major" -ge "$NODE_MIN_MAJOR" ] || continue
        printf '%s\n' "$candidate"
        return 0
    done
    return 1
}

# A menu reads as a wall of text when the choice and its explanation carry the
# same weight. Bold the choice, but only when a terminal will render it: piped
# output stays plain for logs and tests, and NO_COLOR is honoured.
menu_item() {
    local key="$1"
    local title="$2"

    if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
        printf '  \033[1m%s. %s\033[0m\n' "$key" "$title"
        return 0
    fi
    printf '  %s. %s\n' "$key" "$title"
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
        unset "$key"
        set -a
        # shellcheck source=/dev/null
        . "$env_file"
        set +a
        printenv "$key" 2>/dev/null || true
    )
}

# Transport selection is intentionally centralized. An unset mode preserves the
# historical inference (gateway when HERDR_GATEWAY_URL is present, otherwise
# Cloudflare); an explicit mode is validated instead of silently combining two
# transports.
relay_transport_mode() {
    local env_file="${1:-${HERDR_RELAY_ENV:-}}"
    local mode=""
    local persisted_mode=""
    local persisted_gateway=""
    local gateways

    if [ "${HERDR_TAILSCALE_REQUEST:-}" = 1 ]; then
        mode=tailscale
    elif [ -n "$env_file" ]; then
        persisted_mode="$(env_file_value "$env_file" HERDR_RELAY_TRANSPORT)"
        persisted_gateway="$(env_file_value "$env_file" HERDR_GATEWAY_URL)"
        if [ -n "$persisted_mode" ]; then
            mode="$persisted_mode"
        elif [ -n "$persisted_gateway" ]; then
            mode=gateway
        fi
    fi
    if [ -z "$mode" ] && [ -n "${HERDR_RELAY_TRANSPORT+x}" ]; then
        mode="$HERDR_RELAY_TRANSPORT"
    fi
    mode="$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
    if [ -z "$mode" ]; then
        gateways="$(gateway_urls "$env_file")"
        if [ -n "$gateways" ]; then
            mode=gateway
        else
            mode=cloudflare
        fi
    fi
    case "$mode" in
        cloudflare|gateway|tailscale|tailscale-external) printf '%s\n' "$mode" ;;
        *)
            echo "✗ Invalid HERDR_RELAY_TRANSPORT: $mode" >&2
            return 1
            ;;
    esac
}

clear_tailscale_selection() {
    local env_file="$1"

    remove_env_value_atomic "$env_file" HERDR_TAILSCALE_ORIGIN
    remove_env_value_atomic "$env_file" HERDR_RELAY_PAIRING_SOCKET
    remove_env_value_atomic "$env_file" HERDR_RELAY_RUN_ID
    remove_env_value_atomic "$env_file" HERDR_TAILSCALE_HTTPS_PORT
    remove_env_value_atomic "$env_file" HERDR_EXTERNAL_HTTPS_ORIGIN
    unset HERDR_TAILSCALE_ORIGIN HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_RUN_ID
    unset HERDR_TAILSCALE_HTTPS_PORT HERDR_EXTERNAL_HTTPS_ORIGIN
    unset HERDR_RELAY_CONTROL_RUN_ID
}

set_relay_transport() {
    local env_file="$1"
    local mode="$2"

    case "$mode" in
        cloudflare|gateway|tailscale|tailscale-external) ;;
        *) echo "✗ Invalid relay transport: $mode" >&2; return 1 ;;
    esac
    if [ "$mode" != tailscale ] && [ -e "$(tailscale_session_file "$env_file")" ]; then
        echo "✗ Cannot change transport while a foreground Tailscale session is recorded." >&2
        echo "  Stop that pane and verify its route before changing transport." >&2
        return 1
    fi
    if [ -e "$(tailscale_external_session_file "$env_file")" ]; then
        echo "✗ Cannot change transport while an operator-owned HTTPS Serve relay is running." >&2
        echo "  Stop that foreground pane before changing transport." >&2
        return 1
    fi
    if [ "$mode" != tailscale ]; then
        clear_tailscale_selection "$env_file"
    fi
    if [ "$mode" = cloudflare ] || [ "$mode" = tailscale-external ]; then
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_URL
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION
        unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
    fi
    set_env_value_atomic "$env_file" HERDR_RELAY_TRANSPORT "$mode"
    export HERDR_RELAY_TRANSPORT="$mode"
}

installed_relay_service_definition_present() {
    case "$(uname -s)" in
        Linux) [ -f "$HOME/.config/systemd/user/herdr-mobile-relay.service" ] ;;
        Darwin) [ -f "$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ] ;;
        *) return 1 ;;
    esac
}

# JSON scalar accessors use the packaged relay's bounded strict decoder. A
# text match is not authority: fields must be exact top-level values in a
# complete document with the requested JSON type. Optional string/number
# telemetry collapses invalid input to empty output; booleans retain failure.
relay_json_field() {
    local kind="$1"
    local json="$2"
    local key="$3"
    local binary="${4:-}"
    local status

    if [ -z "$binary" ]; then
        binary="$(relay_binary)" || return 1
    fi
    [ -x "$binary" ] || return 1
    if printf '%s' "$json" | "$binary" json-field "$kind" "$key" 2>/dev/null; then
        return 0
    else
        status=$?
    fi
    if [ "$status" -eq 2 ]; then
        echo "✗ The selected relay binary does not support strict JSON field extraction." >&2
        echo "  Reinstall a verified relay release matching these shell helpers." >&2
    fi
    return "$status"
}

json_bool_field() {
    relay_json_field bool "$1" "$2" "${3:-}"
}

json_number_field() {
    relay_json_field number "$1" "$2" "${3:-}" || return 0
}

# Session records are generated by this checkout and are read as constrained
# key/value data, never sourced. This keeps a stale or tampered marker from
# becoming shell code.
tailscale_session_file() {
    local env_file="$1"
    printf '%s/tailscale-session.env\n' "$(dirname "$env_file")"
}

tailscale_external_session_file() {
    local env_file="$1"
    printf '%s/tailscale-external-session.env\n' "$(dirname "$env_file")"
}

tailscale_session_value() {
    local session_file="$1"
    local key="$2"

    [ -r "$session_file" ] || return 1
    sed -n "s/^${key}=//p" "$session_file" | head -1
}

tailscale_control_request() {
    local socket="$1"
    local operation="$2"
    local run_id="$3"
    local instance="$4"

    [ -n "$socket" ] && [ -n "$run_id" ] && [ -n "$instance" ] || return 1
    "$(relay_binary)" pairing-control \
        --socket "$socket" --operation "$operation" \
        --run-id "$run_id" --instance "$instance"
}

arm_tailscale_setup_link() {
    local env_file="$1"
    local session_file
    local socket
    local run_id
    local instance
    local response

    session_file="$(tailscale_session_file "$env_file")"
    socket="$(tailscale_session_value "$session_file" HERDR_RELAY_PAIRING_SOCKET || true)"
    run_id="$(tailscale_session_value "$session_file" HERDR_RELAY_RUN_ID || true)"
    instance="$(env_file_value "$env_file" HERDR_RELAY_INSTANCE_ID)"
    [ -n "$socket" ] && [ -n "$run_id" ] && [ -n "$instance" ] || return 1
    response="$(tailscale_control_request "$socket" arm_bootstrap "$run_id" "$instance" 2>/dev/null)" || return 1
    [ "$(json_bool_field "$response" ok)" = true ] || return 1
    [ "$(json_string_field "$response" run_id)" = "$run_id" ] || return 1
    [ "$(json_string_field "$response" instance)" = "$instance" ] || return 1
    [ "$(json_bool_field "$response" invitation_armed)" = true ] || return 1
    [ -n "$(json_string_field "$response" invitation_expires_at)" ] || return 1
    printf '%s\n' "$response"
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
    relay_json_field string "$1" "$2" "${3:-}" || return 0
}

# Proves the served release and packaged web bundle belong to the running
# binary before any invitation is armed or QR printed. The binary pins its own
# version and revision; the served health must report the same values and a
# usable bundle, and an installed release manifest (when present) must agree
# with both. A mutable `current` release or a stale bundle is refused rather
# than paired.
require_release_identity() {
    local health="$1"
    local binary="$2"
    local identity
    local binary_version
    local binary_revision
    local health_version
    local health_revision
    local bundle_hash
    local bundle_version
    local bundle_revision
    local manifest
    local manifest_version
    local manifest_revision
    local manifest_web_hash

    [ -n "$binary" ] && [ -x "$binary" ] || {
        echo "✗ Release identity could not be verified: no usable relay binary was found." >&2
        return 1
    }
    identity="$("$binary" version --json 2>/dev/null)" || {
        echo "✗ Release identity could not be verified: the relay binary did not report its version." >&2
        return 1
    }
    binary_version="$(json_string_field "$identity" version "$binary")"
    binary_revision="$(json_string_field "$identity" revision "$binary")"
    [ -n "$binary_version" ] && [ -n "$binary_revision" ] || {
        echo "✗ Release identity could not be verified: the relay binary reported no version or revision." >&2
        return 1
    }
    health_version="$(json_string_field "$health" version "$binary")"
    health_revision="$(json_string_field "$health" revision "$binary")"
    [ "$health_version" = "$binary_version" ] || {
        echo "✗ Served release version '$health_version' does not match the running binary version '$binary_version'." >&2
        return 1
    }
    [ "$health_revision" = "$binary_revision" ] || {
        echo "✗ Served release revision '$health_revision' does not match the running binary revision '$binary_revision'." >&2
        return 1
    }
    bundle_hash="$(json_string_field "$health" bundle_hash "$binary")"
    [ -n "$bundle_hash" ] || {
        echo "✗ Served release has no managed web bundle; refusing to pair a stale release." >&2
        return 1
    }
    bundle_version="$(json_string_field "$health" bundle_version "$binary")"
    bundle_revision="$(json_string_field "$health" bundle_revision "$binary")"
    [ "$bundle_version" = "$binary_version" ] || {
        echo "✗ Served web bundle version '$bundle_version' does not match the running binary version '$binary_version'." >&2
        return 1
    }
    [ "$bundle_revision" = "$binary_revision" ] || {
        echo "✗ Served web bundle revision '$bundle_revision' does not match the running binary revision '$binary_revision'." >&2
        return 1
    }
    manifest="$(dirname "$binary")/release-manifest.json"
    if [ -f "$manifest" ]; then
        manifest_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        manifest_revision="$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        manifest_web_hash="$(sed -n 's/^[[:space:]]*"web_hash":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)"
        [ "$manifest_version" = "$binary_version" ] || {
            echo "✗ Release manifest version '$manifest_version' does not match the running binary version '$binary_version'." >&2
            return 1
        }
        [ "$manifest_revision" = "$binary_revision" ] || {
            echo "✗ Release manifest revision '$manifest_revision' does not match the running binary revision '$binary_revision'." >&2
            return 1
        }
        [ "$manifest_web_hash" = "$bundle_hash" ] || {
            echo "✗ Release manifest web hash does not match the served bundle web hash." >&2
            return 1
        }
    fi
    return 0
}

# Verify the selected phone-app origin against the release's extracted web
# bundle using system TLS and hostname validation before any relay invitation
# is armed. Suppress verifier diagnostics so operator URLs stay out of logs.
verify_phone_app_bundle() {
    local origin="$1"
    local binary="${2:-}"
    local identity version revision web_root

    [ -n "$binary" ] && [ -x "$binary" ] || binary="$(relay_binary)" || return 1
    identity="$("$binary" version --json 2>/dev/null)" || return 1
    version="$(json_string_field "$identity" version "$binary")"
    revision="$(json_string_field "$identity" revision "$binary")"
    [ -n "$version" ] && [ -n "$revision" ] || return 1
    web_root="${HERDR_WEB_ROOT:-$(dirname "$binary")/web}"
    if ! "$binary" verify-public --web-root "$web_root" --origin "$origin" \
        --version "$version" --revision "$revision" >/dev/null 2>&1; then
        echo "✗ The selected phone-app origin does not serve this release's verified Herdr bundle." >&2
        return 1
    fi
    return 0
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

tailscale_https_port() {
    local port="${HERDR_TAILSCALE_HTTPS_PORT:-443}"
    case "$port" in
        ''|*[!0-9]*)
            echo "✗ HERDR_TAILSCALE_HTTPS_PORT must be a positive TCP port." >&2
            return 1
            ;;
    esac
    [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || {
        echo "✗ HERDR_TAILSCALE_HTTPS_PORT must be between 1 and 65535." >&2
        return 1
    }
    printf '%s\n' "$port"
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

# The configured tie-break leader used by setup and service scripts. The live
# relay advertises its selected entry separately through status.
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

# Builds the editable default shown after an operator supplies or deploys their
# own gateway. Their entries stay first; the project gateways are cold
# fallbacks, and normalization removes a community entry that is already theirs.
gateway_subscription_defaults() {
    local own="$1"
    local community

    community="$(community_gateway_url)"
    normalize_gateway_urls "$own${community:+,$community}"
}

# Every own-gateway path ends here. Showing the complete candidate list makes
# the fallback policy an explicit operator choice instead of hiding it behind a
# yes/no prompt or requiring prior knowledge of the community hostnames.
prompt_gateway_subscriptions() {
    local defaults
    local entered
    local normalized

    if ! defaults="$(normalize_gateway_urls "$1")"; then
        return 1
    fi
    while true; do
        if [ "${HERDR_GATEWAY_SUBSCRIPTIONS+set}" = "set" ]; then
            entered="$HERDR_GATEWAY_SUBSCRIPTIONS"
        else
            echo "" >&2
            echo "Gateways this relay and phone may use, in priority order:" >&2
            printf '%s\n' "$defaults" | tr ',' '\n' | sed 's/^/  /' >&2
            read -r -p "Gateways to subscribe to, comma-separated [keep this list]: " entered ||
                entered=""
        fi
        entered="${entered:-$defaults}"
        if normalized="$(normalize_gateway_urls "$entered")"; then
            printf '%s\n' "$normalized"
            return 0
        fi
        echo "✗ Enter one or more gateway hostnames or ws:// / wss:// origins." >&2
        [ "${HERDR_GATEWAY_SUBSCRIPTIONS+set}" != "set" ] || return 1
    done
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

# Round-trip milliseconds for a gateway's /healthz, or failure when it does not
# answer. Setup already opens this connection to prove the gateway is alive;
# reporting what it measured is what lets a person order the list by distance
# instead of guessing from hostnames. The body goes to a file so stdout carries
# only the timing: a curl that reports no timing still proves health, and the
# caller simply omits the number.
gateway_healthz_ms() {
    local body_file
    local seconds

    body_file="$(mktemp "${TMPDIR:-/tmp}/herdr-healthz.XXXXXX")" || return 1
    if ! seconds="$(
        curl --fail --silent --show-error \
            --connect-timeout 3 \
            --max-time 8 \
            --output "$body_file" \
            --write-out '%{time_total}' \
            "$(gateway_http_base "$1")/healthz" 2>/dev/null
    )"; then
        rm -f "$body_file"
        return 1
    fi
    if ! grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' "$body_file"; then
        rm -f "$body_file"
        return 1
    fi
    rm -f "$body_file"
    case "$seconds" in
        '' | *[!0-9.]*) return 0 ;;
    esac
    awk -v seconds="$seconds" 'BEGIN { printf "%d\n", (seconds * 1000) + 0.5 }'
}

# How a saved list reads in one phrase, for the setup menu's status line. The
# policy decides which gateway carries traffic, so it belongs next to the
# gateway it chose rather than only in the environment file.
gateway_selection_label() {
    case "$1" in
        latency) printf 'closest wins\n' ;;
        *) printf 'first listed wins\n' ;;
    esac
}

# The policy a prompt answer selects. Split from the prompt so the rule is
# testable without a terminal: "1" or empty keeps the default the list was
# built with, "2" takes the other one, and anything else keeps the default
# rather than silently changing which gateway carries traffic.
gateway_selection_choice() {
    local default_selection="$1"
    local answer="$2"

    if [ "$answer" = 2 ]; then
        if [ "$default_selection" = latency ]; then
            printf 'ordered\n'
        else
            printf 'latency\n'
        fi
        return 0
    fi
    printf '%s\n' "$default_selection"
}

# Asks how the relay should read the list it just saved. The default is the one
# the chosen action implies — an operator's own gateway is a priority, a pool of
# interchangeable public ones is ranked by distance — so Enter is always the
# right answer for someone who does not care.
#
# The terminal is the only guard: HERDR_GATEWAY_SELECTION cannot serve as an
# "automation is driving" signal, because the setup menu loads relay.env into
# the environment before running an action. Reading it here would let a saved
# policy silently answer the question and, worse, carry an old own-gateway
# `ordered` into a freshly chosen community list.
prompt_gateway_selection() {
    local default_selection="$1"
    local answer

    if [ ! -t 0 ]; then
        printf '%s\n' "$default_selection"
        return 0
    fi
    echo "" >&2
    echo "How should the relay pick from this list?" >&2
    if [ "$default_selection" = latency ]; then
        echo "  1. Closest by measured latency [default]" >&2
        echo "  2. First one that answers, in the order above" >&2
    else
        echo "  1. First one that answers, in the order above [default]" >&2
        echo "  2. Closest by measured latency" >&2
    fi
    read -r -p "Choice [1]: " answer || answer=""
    gateway_selection_choice "$default_selection" "$answer"
}

# Persists the transport choice. An empty URL removes the key, which returns
# the relay to the Cloudflare tunnel path — and takes the selection policy with
# it, so a relay that leaves the gateway path keeps no stale policy to revive
# behind a later, differently chosen list.
set_gateway_url() {
    local env_file="$1"
    local url="$2"

    if [ -e "$(tailscale_session_file "$env_file")" ] || [ -e "$(tailscale_external_session_file "$env_file")" ]; then
        echo "✗ Cannot change gateway selection while a foreground Tailscale Serve relay is recorded." >&2
        echo "  Stop that pane before changing transport." >&2
        return 1
    fi
    if [ -z "$url" ]; then
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_URL
        remove_env_value_atomic "$env_file" HERDR_GATEWAY_SELECTION
        unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION
        set_relay_transport "$env_file" cloudflare
        return 0
    fi
    set_relay_transport "$env_file" gateway
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

# Builds a setup fragment for either transport. A gateway-configured relay has
# no direct relay URL; its complete ordered candidate list travels only in
# `gateways=`. Each entry is encoded separately so the commas remain separators,
# while the relay token stays inside the URL fragment.
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
    fragment="$(build_setup_fragment "$token" "$label")"
    old_ifs="$IFS"
    IFS=','
    # shellcheck disable=SC2086
    set -- $gateways
    IFS="$old_ifs"
    for entry in "$@"; do
        encoded="${encoded:+$encoded,}$(encode_fragment_value "$entry")"
    done
    printf '%s&gateways=%s\n' "$fragment" "$encoded"
}

phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="${2:-${HERDR_RELAY_ENV:-}}"
    local app_url="${HERDR_PHONE_APP_URL:-${HERDR_APP_DEPLOY_ORIGIN:-}}"
    local normalized
    local configured_origin
    local observed_origin

    if [ -z "$app_url" ] && [ -n "$env_file" ]; then
        configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
        observed_origin="$(dirname "$env_file")/phone-app-origin"
        if [ -r "$configured_origin" ]; then
            app_url="$(head -1 "$configured_origin")"
        elif [ -r "$observed_origin" ]; then
            app_url="$(head -1 "$observed_origin")"
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

# A separately hosted app uses herdr.<authorized-zone> by convention. Probe it
# before defaulting a new computer to its relay-served copy: browser storage and
# PWA identity are origin-scoped, so every relay must open the same app origin.
discover_cloudflare_phone_app_origin() {
    local origin_cert="${TUNNEL_ORIGIN_CERT:-$HOME/.cloudflared/cert.pem}"
    local zone
    local candidate

    [ -r "$origin_cert" ] || return 1
    zone="$(cloudflare_cert_zone_name "$origin_cert" 2>/dev/null)" || return 1
    [ -n "$zone" ] || return 1
    candidate="https://herdr.$zone"
    phone_app_origin_serves_herdr "$candidate" || return 1
    printf '%s\n' "$candidate"
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
        if ! read -r -p "Installed app domain or URL, or q to cancel: " entered_url; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        case "$entered_url" in
            q | Q)
                echo "Setup cancelled." >&2
                return 1
                ;;
        esac
        if [ -z "$entered_url" ]; then
            echo "✗ Enter the domain shown in the installed app's Site settings," >&2
            echo "  for example app.example.com, or q to cancel." >&2
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

phone_app_choice_action() {
    local current_origin="$1"
    local choice="${2:-1}"

    if [ -n "$current_origin" ]; then
        case "$choice" in
            1) printf 'keep\n' ;;
            2) printf 'relay\n' ;;
            3) printf 'existing\n' ;;
            q | Q) printf 'cancel\n' ;;
            *) printf 'invalid\n' ;;
        esac
        return
    fi
    case "$choice" in
        1) printf 'relay\n' ;;
        2) printf 'existing\n' ;;
        q | Q) printf 'cancel\n' ;;
        *) printf 'invalid\n' ;;
    esac
}

choose_tailscale_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local action
    local choice
    local current_origin=""
    local configured_origin
    local observed_origin

    configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
    observed_origin="$(dirname "$env_file")/phone-app-origin"

    # An explicit environment value is an operator request, so it wins over a
    # saved origin and is still normalized by the compiled URL/TLS contract.
    if [ -n "${HERDR_PHONE_APP_URL:-}" ]; then
        phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi
    if [ -n "${HERDR_APP_DEPLOY_ORIGIN:-}" ]; then
        HERDR_PHONE_APP_URL="$HERDR_APP_DEPLOY_ORIGIN" \
            phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi

    # A prior choice is shared app state, not a reason to discover a new
    # Cloudflare origin. Keep it unless the operator explicitly switches it.
    if [ -s "$configured_origin" ] || [ -s "$observed_origin" ]; then
        if ! current_origin="$(phone_app_base_url "$relay_fallback" "$env_file")"; then
            if ! stdin_is_terminal; then
                echo "✗ The saved phone app address is invalid." >&2
                return 1
            fi
            echo "  The saved phone app address is invalid; choose a replacement." >&2
            echo "" >&2
        fi
    fi

    # The foreground Tailscale caller has already selected an HTTPS relay
    # origin. Its packaged frontend is the safe default, but every app origin
    # is independently checked against this release before an invite is armed.
    if ! stdin_is_terminal; then
        if [ -n "$current_origin" ]; then
            printf '%s\n' "$current_origin"
        else
            phone_app_base_url "$relay_fallback" "$env_file"
        fi
        return
    fi

    echo "Where should the phone setup link open?" >&2
    echo "" >&2
    if [ -n "$current_origin" ]; then
        echo "  Current phone app: $current_origin" >&2
        echo "" >&2
        menu_item 1 "Keep current phone app (recommended)" >&2
        echo "     Reuse this shared app origin for the Tailscale relay." >&2
        echo "" >&2
        menu_item 2 "Use this Serve relay instead" >&2
        echo "     Use its verified HTTPS origin and packaged Herdr app." >&2
        echo "" >&2
        menu_item 3 "Use another installed Herdr app" >&2
    else
        menu_item 1 "This Serve relay (recommended)" >&2
        echo "     Use its verified HTTPS origin and packaged Herdr app." >&2
        echo "" >&2
        menu_item 2 "An existing installed Herdr app" >&2
    fi
    echo "     The shared app origin can be changed explicitly later." >&2
    echo "" >&2
    menu_item q "Cancel, change nothing" >&2
    echo "" >&2
    while true; do
        read -r -p "Choice [1]: " choice || choice="cancel"
        if [ "$choice" = "cancel" ]; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        action="$(phone_app_choice_action "$current_origin" "$choice")"
        case "$action" in
            keep)
                printf '%s\n' "$current_origin"
                return
                ;;
            relay)
                HERDR_PHONE_APP_URL=relay phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            existing)
                prompt_phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            cancel)
                echo "Setup cancelled." >&2
                return 1
                ;;
            invalid)
                if [ -n "$current_origin" ]; then
                    echo "✗ Choose 1, 2, 3, or q." >&2
                else
                    echo "✗ Choose 1, 2, or q." >&2
                fi
                ;;
        esac
    done
}

choose_phone_app_base_url() {
    local relay_fallback="$1"
    local env_file="$2"
    local setup_kind="${3:-stable}"
    local action
    local choice
    local current_origin=""
    local discovered_origin=""
    local configured_origin
    local observed_origin

    configured_origin="$(dirname "$env_file")/phone-app-origin-configured"
    observed_origin="$(dirname "$env_file")/phone-app-origin"
    if [ "$setup_kind" = tailscale ] || [ "$setup_kind" = tailscale-external ]; then
        choose_tailscale_phone_app_base_url "$relay_fallback" "$env_file"
        return
    fi
    if [ -z "${HERDR_PHONE_APP_URL:-}" ] && [ ! -s "$configured_origin" ]; then
        if [ -n "${HERDR_APP_DEPLOY_ORIGIN:-}" ]; then
            discovered_origin="$(
                HERDR_PHONE_APP_URL="$HERDR_APP_DEPLOY_ORIGIN" \
                    phone_app_base_url "$relay_fallback" "$env_file"
            )"
        else
            discovered_origin="$(discover_cloudflare_phone_app_origin || true)"
        fi
    fi
    if [ -n "${HERDR_PHONE_APP_URL:-}" ] || [ ! -t 0 ]; then
        if [ -n "$discovered_origin" ]; then
            printf '%s\n' "$discovered_origin"
        else
            phone_app_base_url "$relay_fallback" "$env_file"
        fi
        return
    fi

    echo "Where should the phone setup link open?" >&2
    echo "" >&2
    if [ -s "$configured_origin" ]; then
        if ! current_origin="$(phone_app_base_url "$relay_fallback" "$env_file")"; then
            echo "  The saved phone app address is invalid; choose a replacement." >&2
            echo "" >&2
        fi
    elif [ -n "$discovered_origin" ]; then
        current_origin="$discovered_origin"
        echo "  Found an existing Herdr app in this Cloudflare zone." >&2
    elif [ -s "$observed_origin" ]; then
        current_origin="$(phone_app_base_url "$relay_fallback" "$env_file" || true)"
        [ -z "$current_origin" ] ||
            echo "  Last connected phone app: $current_origin" >&2
    fi
    if [ -n "$current_origin" ]; then
        echo "  Current phone app: $current_origin" >&2
        echo "" >&2
        menu_item 1 "Keep current phone app (recommended)" >&2
        echo "     $current_origin" >&2
        echo "" >&2
        if [ "$setup_kind" = "temporary" ]; then
            menu_item 2 "Use this temporary relay instead" >&2
            echo "     Opens the TryCloudflare app. Its address changes after restart." >&2
        else
            menu_item 2 "Use this relay instead" >&2
            echo "     Switches the app origin to this computer's relay hostname." >&2
        fi
        echo "" >&2
        menu_item 3 "Use another installed Herdr app" >&2
    else
        if [ "$setup_kind" = "temporary" ]; then
            menu_item 1 "This temporary relay (recommended for trying one relay)" >&2
            echo "     Opens the TryCloudflare app. Its address changes after restart." >&2
        else
            menu_item 1 "This relay (recommended for one relay)" >&2
            echo "     Uses this computer's verified hostname as the installed app." >&2
        fi
        echo "" >&2
        menu_item 2 "An existing installed Herdr app" >&2
    fi
    echo "     Adds this computer to the same app as your other relays." >&2
    echo "" >&2
    menu_item q "Cancel, change nothing" >&2
    echo "" >&2
    while true; do
        read -r -p "Choice [1]: " choice || choice="cancel"
        if [ "$choice" = "cancel" ]; then
            echo "" >&2
            echo "Setup cancelled." >&2
            return 1
        fi
        action="$(phone_app_choice_action "$current_origin" "$choice")"
        case "$action" in
            keep)
                printf '%s\n' "$current_origin"
                return
                ;;
            relay)
                HERDR_PHONE_APP_URL=relay phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            existing)
                prompt_phone_app_base_url "$relay_fallback" "$env_file"
                return
                ;;
            cancel)
                echo "Setup cancelled." >&2
                return 1
                ;;
            invalid)
                if [ -n "$current_origin" ]; then
                    echo "✗ Choose 1, 2, 3, or q." >&2
                else
                    echo "✗ Choose 1, 2, or q." >&2
                fi
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
    target="$(dirname "$env_file")/phone-app-origin-configured"
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

# OSC 8 is a terminal protocol, not a vendor feature. Use it for any interactive
# terminal unless the user or terminal explicitly asks for plain output; modern
# terminals understand it and older ones safely ignore it.
stdout_is_terminal() {
    [ -t 1 ]
}

stdin_is_terminal() {
    [ -t 0 ]
}

terminal_hyperlinks_enabled() {
    stdout_is_terminal &&
        [ -z "${NO_COLOR+x}" ] &&
        [ "${TERM:-}" != dumb ]
}

# Setup URLs enter both the QR encoder and an OSC 8 sequence. Raw whitespace is
# not valid in a URL, and control bytes could terminate the terminal sequence.
# The compiled origin parser enforces HTTPS, with canonical loopback HTTP only.
phone_setup_url_is_safe() {
    local url="$1"
    local origin
    local normalized

    case "$url" in
        *[[:cntrl:]]* | *[[:space:]]*) return 1 ;;
    esac
    if [[ ! "$url" =~ ^(https?://[^/?#]+) ]]; then
        return 1
    fi
    origin="${BASH_REMATCH[1]}"
    if ! normalized="$("$(relay_binary)" normalize-origin \
        --allow-loopback-http "$origin" 2>/dev/null)"; then
        return 1
    fi
    case "$origin" in
        https://*) return 0 ;;
        *) [ "$normalized" = "$origin" ] ;;
    esac
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

# The URL has already been validated before this internal emitter is called.
emit_phone_setup_url() {
    local phone_url="$1"

    if terminal_hyperlinks_enabled; then
        printf '  \033]8;;%s\033\\%s\033]8;;\033\\\n' "$phone_url" "$phone_url"
    else
        printf '  %s\n' "$phone_url"
    fi
}

print_phone_setup_url() {
    phone_setup_url_is_safe "$1" || return 1
    emit_phone_setup_url "$1"
}

# Shared tail of quick-start and setup-link output: QR code when possible,
# always the link. Invalid values fail before either output sink sees them.
print_phone_setup() {
    local phone_url="$1"
    local qr_code

    phone_setup_url_is_safe "$phone_url" || return 1
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
    emit_phone_setup_url "$phone_url"
}

# The bootstrap invitation in a setup link is one-use: the first phone that
# pairs consumes it. Printing the link is the operator asking for one more
# pairing, so the running relay is told to arm a fresh invitation before the
# link is shown. The relay records its pid beside relay.env and re-arms on
# SIGUSR1. Returns 1 when no relay is running here.
arm_setup_link() {
    local env_file="$1"
    local pid

    pid="$(head -1 "$(dirname "$env_file")/relay.pid" 2>/dev/null || true)"
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    kill -0 "$pid" 2>/dev/null || return 1
    kill -USR1 "$pid" 2>/dev/null || return 1
    sleep 0.3
}

print_setup_link_arming() {
    if [ "$1" -eq 0 ]; then
        echo "  This link pairs one phone within 10 minutes. Print it again for another."
    else
        echo "  The relay is not running here; start it, then print the link again."
    fi
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

# Acquire the canonical root's owner (O) for this script's lifetime. On success
# HOLDER_PID holds the managed-state holder and the owner lock is held. On any
# refusal the function returns non-zero with a message on stderr and leaves the
# filesystem untouched.
managed_owner_acquire() {
    local dir="$1"
    local attempt
    HOLDER_LOG="$(mktemp "${TMPDIR:-/tmp}/herdr-managed-owner.XXXXXX")"
    "$(relay_binary)" managed-state hold --dir "$dir" --operation owner >"$HOLDER_LOG" 2>&1 &
    HOLDER_PID=$!
    for ((attempt = 1; attempt <= 50; attempt++)); do
        if grep -q '"ok":true' "$HOLDER_LOG" 2>/dev/null; then
            return 0
        fi
        if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
            wait "$HOLDER_PID" 2>/dev/null || true
            echo "✗ The relay configuration is in use by another operation: $dir" >&2
            sed -n '1,10p' "$HOLDER_LOG" >&2
            HOLDER_PID=""
            rm -f "$HOLDER_LOG"
            HOLDER_LOG=""
            return 1
        fi
        sleep 0.1
    done
    kill -TERM "$HOLDER_PID" 2>/dev/null || true
    wait "$HOLDER_PID" 2>/dev/null || true
    echo "✗ Timed out acquiring the relay configuration lock: $dir" >&2
    HOLDER_PID=""
    rm -f "$HOLDER_LOG"
    HOLDER_LOG=""
    return 1
}

# Release the holder cleanly. Safe to call when no holder is active and from an
# EXIT trap; the holder retires the owner lock itself.
managed_owner_release() {
    [ -n "${HOLDER_PID:-}" ] || return 0
    kill -TERM "$HOLDER_PID" 2>/dev/null || true
    wait "$HOLDER_PID" 2>/dev/null || true
    HOLDER_PID=""
    if [ -n "${HOLDER_LOG:-}" ]; then
        rm -f "$HOLDER_LOG"
        HOLDER_LOG=""
    fi
    return 0
}

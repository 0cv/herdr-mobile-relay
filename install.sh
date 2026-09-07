#!/bin/sh
# Install one exact, complete Herdr Mobile Relay release. No toolchain needed.
set -eu

REPO=${HERDR_RELEASE_REPOSITORY:-0cv/herdr-mobile-relay}
BINARY=herdr-mobile-relay

info() { printf '==> %s\n' "$1" >&2; }
fatal() { printf 'error: %s\n' "$1" >&2; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Linux) printf '%s\n' linux ;;
        Darwin) printf '%s\n' darwin ;;
        *) fatal "unsupported OS: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) printf '%s\n' amd64 ;;
        aarch64|arm64) printf '%s\n' arm64 ;;
        *) fatal "unsupported architecture: $(uname -m)" ;;
    esac
}

run_with_timeout() {
    herdr_timeout_seconds=$1
    shift
    "$@" &
    herdr_command_pid=$!
    herdr_elapsed=0
    while kill -0 "$herdr_command_pid" 2>/dev/null; do
        if [ "$herdr_elapsed" -ge "$herdr_timeout_seconds" ]; then
            kill -9 "$herdr_command_pid" 2>/dev/null || true
            wait "$herdr_command_pid" 2>/dev/null || true
            herdr_command_pid=
            return 124
        fi
        sleep 1
        herdr_elapsed=$((herdr_elapsed + 1))
    done
    if wait "$herdr_command_pid"; then
        herdr_status=0
    else
        herdr_status=$?
    fi
    herdr_command_pid=
    return "$herdr_status"
}

terminate_active_command() {
    herdr_signal=$1
    if [ -z "${herdr_command_pid:-}" ]; then
        return 0
    fi
    kill "$herdr_signal" "$herdr_command_pid" 2>/dev/null || true
    herdr_signal_elapsed=0
    while kill -0 "$herdr_command_pid" 2>/dev/null; do
        if [ "$herdr_signal_elapsed" -ge 2 ]; then
            kill -KILL "$herdr_command_pid" 2>/dev/null || true
            break
        fi
        sleep 1
        herdr_signal_elapsed=$((herdr_signal_elapsed + 1))
    done
    wait "$herdr_command_pid" 2>/dev/null || true
    herdr_command_pid=
}

on_install_exit() {
    terminate_active_command -TERM
    if [ -n "${install_sentinel_temp:-}" ]; then
        rm -f "$install_sentinel_temp"
    fi
    if [ -n "${work_dir:-}" ]; then
        rm -rf "$work_dir"
    fi
}

on_install_signal() {
    herdr_signal=$1
    herdr_exit_status=$2
    terminate_active_command "$herdr_signal"
    exit "$herdr_exit_status"
}

validate_github_fetch_token() {
    case "${github_fetch_token:-}" in
        ""|*[!A-Za-z0-9_.-]*) fatal "GitHub release token contains unsupported characters" ;;
    esac
}

authenticated_curl() {
    accept=$1
    shift
    validate_github_fetch_token
    run_with_timeout 120 env -u GH_TOKEN -u GITHUB_TOKEN -u HERDR_GITHUB_TOKEN_FILE \
        curl --config /dev/fd/3 "$@" 3<<EOF
header = "Authorization: token ${github_fetch_token}"
header = "Accept: ${accept}"
EOF
}

fetch() {
    if command -v curl >/dev/null 2>&1; then
        if [ -n "${github_fetch_token:-}" ]; then
            authenticated_curl application/octet-stream \
                --fail --show-error --silent --location \
                --connect-timeout 10 --max-time 120 --output "$2" --url "$1"
        else
            run_with_timeout 120 curl --fail --show-error --silent --location \
                --connect-timeout 10 --max-time 120 --output "$2" --url "$1"
        fi
    elif command -v wget >/dev/null 2>&1; then
        [ -z "${github_fetch_token:-}" ] || fatal "curl is required for authenticated release downloads"
        run_with_timeout 120 wget --quiet --timeout=120 --tries=1 --output-document="$2" "$1"
    else
        fatal "curl or wget is required"
    fi
}

fetch_json() {
    if command -v curl >/dev/null 2>&1; then
        if [ -n "${github_fetch_token:-}" ]; then
            authenticated_curl application/vnd.github+json \
                --fail --show-error --silent --location \
                --connect-timeout 10 --max-time 120 --url "$1"
        else
            run_with_timeout 120 curl --fail --show-error --silent --location \
                --connect-timeout 10 --max-time 120 \
                -H "Accept: application/vnd.github+json" --url "$1"
        fi
    else
        [ -z "${github_fetch_token:-}" ] || fatal "curl is required for authenticated release downloads"
        run_with_timeout 120 wget --quiet --timeout=120 --tries=1 --output-document=- \
            --header="Accept: application/vnd.github+json" "$1"
    fi
}

resolve_asset_url() {
    release_json=$1
    asset_name=$2
    # GitHub places the asset API URL before the asset name and nested uploader
    # URLs after it. Split at every URL key, then select the record whose
    # following fields contain the exact asset name.
    printf '%s' "$release_json" |
        tr -d '\n\r\t ' |
        sed 's/"url":"/\
"url":"/g' |
        awk -v name="\"name\":\"$asset_name\"" '
            index($0, name) == 0 { next }
            {
                line = $0
                sub(/^"url":"/, "", line)
                sub(/".*$/, "", line)
                print line
                exit
            }
        '
}

resolve_tag_revision() {
    commit_json=$1
    printf '%s' "$commit_json" |
        tr -d '\n\r\t ' |
        sed 's/"sha":"/\
"sha":"/' |
        sed -n 's/^"sha":"\([0-9a-fA-F][0-9a-fA-F]*\)".*/\1/p' |
        head -1
}

offline_release_requested() {
    [ -n "${HERDR_RELEASE_ARCHIVE:-}" ] ||
        [ -n "${HERDR_RELEASE_CHECKSUMS:-}" ] ||
        [ -n "${HERDR_EXPECTED_REVISION:-}" ] ||
        [ -n "${HERDR_EXPECTED_ARCHIVE_SHA256:-}" ]
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

validate_archive_paths() {
    tar -tzf "$1" | awk '
        {
            name = $0
            sub(/^\.\//, "", name)
            if (name ~ /^\// || name ~ /(^|\/)\.\.(\/|$)/ || name ~ /\\/) {
                bad = 1
            }
        }
        END { exit bad ? 1 : 0 }
    ' || return 1
    tar -tvzf "$1" | awk '
        {
            kind = substr($1, 1, 1)
            if (kind != "-" && kind != "d") {
                bad = 1
            }
        }
        END { exit bad ? 1 : 0 }
    ' || return 1
}

legacy_root_entry_allowed() {
    root_kind=$1
    entry_name=$2
    entry_path=$3
    case "$root_kind:$entry_name" in
        config:relay.env|config:.env|config:phone-app-origin|config:phone-app-origin-configured|config:stable-setup.json|\
        config:update-state.json|config:app-deploy-state.json|config:support-state.json|config:github-token|\
        config:update.lock|config:app-deploy.lock)
            [ -f "$entry_path" ] && [ ! -L "$entry_path" ]
            ;;
        config:update-job-*.json|config:app-deploy-job-*.json)
            [ -f "$entry_path" ] && [ ! -L "$entry_path" ]
            ;;
        config:device-auth|config:push|config:cloudflared)
            [ -d "$entry_path" ] && [ ! -L "$entry_path" ]
            ;;
        cache:activity.jsonl|cache:activity.tombstones|cache:post-install.sh|cache:post-install.log|\
        cache:approval-verification*|cache:notification-approval-fix-test-*)
            [ -f "$entry_path" ] && [ ! -L "$entry_path" ]
            ;;
        cache:claude-history|cache:uploads|cache:push)
            [ -d "$entry_path" ] && [ ! -L "$entry_path" ]
            ;;
        *)
            return 1
            ;;
    esac
}

validate_legacy_root() {
    legacy_root=$1
    root_kind=$2
    [ -d "$legacy_root" ] && [ ! -L "$legacy_root" ] || return 1
    [ -z "$(find "$legacy_root" -type l -print -quit)" ] || return 1
    found=false
    cache_state=false
    cache_waiter_script=false
    cache_waiter_log=false
    for legacy_entry in "$legacy_root"/* "$legacy_root"/.[!.]* "$legacy_root"/..?*; do
        [ -e "$legacy_entry" ] || continue
        legacy_name=${legacy_entry##*/}
        [ "$legacy_name" != ".herdr-mobile-relay-installation" ] || continue
        legacy_root_entry_allowed "$root_kind" "$legacy_name" "$legacy_entry" || return 1
        found=true
        case "$legacy_name" in
            activity.jsonl|claude-history|uploads) cache_state=true ;;
            post-install.sh) cache_waiter_script=true ;;
            post-install.log) cache_waiter_log=true ;;
        esac
    done
    [ "$found" = true ] || return 1
    if [ "$root_kind" = config ]; then
        legacy_env=
        [ ! -f "$legacy_root/relay.env" ] || legacy_env="$legacy_root/relay.env"
        [ -n "$legacy_env" ] || [ ! -f "$legacy_root/.env" ] || legacy_env="$legacy_root/.env"
        [ -n "$legacy_env" ] || return 1
        grep -q '^HERDR_RELAY_TOKEN=' "$legacy_env" || return 1
    elif [ "$root_kind" = cache ]; then
        [ "$cache_state" = true ] ||
            { [ "$cache_waiter_script" = true ] && [ "$cache_waiter_log" = true ]; } ||
            return 1
    fi
}

install_sentinel_is_valid() {
    sentinel_check_root=$1
    sentinel_check_canonical=${2:-}
    sentinel_check_path="$sentinel_check_root/.herdr-mobile-relay-installation"
    [ -d "$sentinel_check_root" ] && [ ! -L "$sentinel_check_root" ] || return 1
    [ -f "$sentinel_check_path" ] && [ ! -L "$sentinel_check_path" ] || return 1
    if sentinel_check_metadata=$(LC_ALL=C stat -f '%HT:%Lp:%l:%u' "$sentinel_check_path" 2>/dev/null); then
        :
    elif sentinel_check_metadata=$(LC_ALL=C stat -c '%F:%a:%h:%u' "$sentinel_check_path" 2>/dev/null); then
        :
    else
        return 1
    fi
    sentinel_check_uid=$(id -u)
    case "$sentinel_check_metadata" in
        "Regular File:600:1:$sentinel_check_uid"|"regular file:600:1:$sentinel_check_uid") ;;
        *) return 1 ;;
    esac
    if [ -z "$sentinel_check_canonical" ]; then
        sentinel_check_canonical=$(CDPATH='' cd "$sentinel_check_root" && pwd -P) || return 1
    fi
    sentinel_check_lines=$(wc -l < "$sentinel_check_path" | tr -d '[:space:]') || return 1
    [ "$sentinel_check_lines" = 2 ] || return 1
    [ "$(sed -n '1p' "$sentinel_check_path")" = 'product=herdr-mobile-relay' ] || return 1
    [ "$(sed -n '2p' "$sentinel_check_path")" = "root=$sentinel_check_canonical" ] || return 1
    [ -z "$(sed -n '3p' "$sentinel_check_path")" ] || return 1
}

write_install_sentinel() {
    sentinel_root=$1
    root_kind=${2:-new}
    sentinel="$sentinel_root/.herdr-mobile-relay-installation"
    if [ -e "$sentinel" ] || [ -L "$sentinel" ]; then
        canonical_root=$(CDPATH='' cd "$sentinel_root" && pwd -P)
        install_sentinel_is_valid "$sentinel_root" "$canonical_root" ||
            fatal "installation root has a mismatched ownership sentinel: $sentinel_root"
        return
    fi
    if [ -e "$sentinel_root" ]; then
        [ -d "$sentinel_root" ] ||
            fatal "installation root is not a directory: $sentinel_root"
        if [ -n "$(find "$sentinel_root" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
            case "$root_kind" in
                config|cache)
                    validate_legacy_root "$sentinel_root" "$root_kind" ||
                        fatal "refusing to claim nonempty directory without a validated Python 0.8.6 layout: $sentinel_root"
                    ;;
                *)
                    fatal "refusing to claim nonempty directory without an ownership sentinel: $sentinel_root"
                    ;;
            esac
        fi
    else
        mkdir -p "$sentinel_root"
    fi
    chmod 700 "$sentinel_root"
    canonical_root=$(CDPATH='' cd "$sentinel_root" && pwd -P)
    install_sentinel_temp=$(mktemp "$sentinel_root/.herdr-mobile-relay-installation.tmp.XXXXXX") ||
        fatal "could not create ownership sentinel safely: $sentinel_root"
    if ! {
        printf 'product=herdr-mobile-relay\n'
        printf 'root=%s\n' "$canonical_root"
    } > "$install_sentinel_temp"; then
        rm -f "$install_sentinel_temp"
        install_sentinel_temp=
        fatal "could not write ownership sentinel: $sentinel_root"
    fi
    if ! chmod 600 "$install_sentinel_temp" || ! mv -f "$install_sentinel_temp" "$sentinel"; then
        rm -f "$install_sentinel_temp"
        install_sentinel_temp=
        fatal "could not install ownership sentinel: $sentinel_root"
    fi
    install_sentinel_temp=
    install_sentinel_is_valid "$sentinel_root" "$canonical_root" ||
        fatal "installed ownership sentinel failed validation: $sentinel_root"
}

prepare_install_roots() {
    release_root=$1
    config_root=$2
    cache_root=$3
    legacy_cache="$HOME/.cache/herdr-mobile-relay"

    write_install_sentinel "$release_root" new
    write_install_sentinel "$config_root" config

    if [ "$cache_root" != "$legacy_cache" ] &&
       [ -d "$legacy_cache" ] &&
       [ ! -e "$legacy_cache/.herdr-mobile-relay-installation" ]; then
        validate_legacy_root "$legacy_cache" cache ||
            fatal "legacy Python cache has unexpected contents: $legacy_cache"
        if [ -e "$cache_root" ]; then
            [ -d "$cache_root" ] || fatal "cache destination is not a directory: $cache_root"
            if [ -n "$(find "$cache_root" -mindepth 1 -maxdepth 1 -print -quit)" ] &&
               [ ! -f "$cache_root/.herdr-mobile-relay-installation" ]; then
                validate_legacy_root "$cache_root" cache ||
                    fatal "cannot migrate the Python cache into unowned destination: $cache_root"
            fi
        else
            mkdir -p "$cache_root"
        fi
        for legacy_entry in "$legacy_cache"/* "$legacy_cache"/.[!.]* "$legacy_cache"/..?*; do
            [ -e "$legacy_entry" ] || continue
            legacy_name=${legacy_entry##*/}
            [ ! -e "$cache_root/$legacy_name" ] ||
                fatal "legacy Python cache entry already exists at destination: $legacy_name"
            mv "$legacy_entry" "$cache_root/$legacy_name" ||
                fatal "could not migrate legacy Python cache entry: $legacy_name"
        done
        rmdir "$legacy_cache" ||
            fatal "could not remove migrated legacy Python cache root"
        info "Migrated Python cache from $legacy_cache to $cache_root"
    fi
    write_install_sentinel "$cache_root" cache
}

main() {
    command -v tar >/dev/null 2>&1 || fatal "tar is required"
    command -v awk >/dev/null 2>&1 || fatal "awk is required"
    command -v find >/dev/null 2>&1 || fatal "find is required"

    version=${VERSION:-${1:-}}
    [ -n "$version" ] || fatal "an exact VERSION is required; unpinned latest installs are refused"
    version=${version#v}
    case "$version" in
        *[!0-9.]*|.*|*..*|*.) fatal "VERSION must use MAJOR.MINOR.PATCH" ;;
    esac
    [ "$(printf '%s' "$version" | awk -F. '{print NF}')" -eq 3 ] ||
        fatal "VERSION must use MAJOR.MINOR.PATCH"

    os=$(detect_os)
    arch=$(detect_arch)
    target="$os/$arch"
    archive="${BINARY}_${version}_${os}_${arch}.tar.gz"
    tag="v$version"
    release_root=${INSTALL_ROOT:-"${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay"}
    shim_dir=${BIN_DIR:-"$HOME/.local/bin"}
    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        config_root=$(dirname "$HERDR_RELAY_ENV")
    else
        config_root=${HERDR_PLUGIN_CONFIG_DIR:-"${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay"}
    fi
    cache_root="${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay"

    work_dir=$(mktemp -d "${TMPDIR:-/tmp}/herdr-install.XXXXXX")
    trap 'on_install_exit' EXIT
    trap 'on_install_signal -INT 130' INT
    trap 'on_install_signal -TERM 143' TERM
    archive_path="$work_dir/$archive"
    checksums_path="$work_dir/checksums.txt"
    stage="$work_dir/release"
    commit_json_path="$work_dir/commit.json"

    [ -z "${HERDR_RELEASE_BASE_URL:-}" ] || fatal "HERDR_RELEASE_BASE_URL is not accepted; release downloads are pinned to GitHub"
    github_fetch_token=${GH_TOKEN:-${GITHUB_TOKEN:-}}
    unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
    if offline_release_requested; then
        [ -n "${HERDR_RELEASE_ARCHIVE:-}" ] &&
            [ -n "${HERDR_RELEASE_CHECKSUMS:-}" ] &&
            [ -n "${HERDR_EXPECTED_REVISION:-}" ] &&
            [ -n "${HERDR_EXPECTED_ARCHIVE_SHA256:-}" ] ||
            fatal "offline install requires archive, checksums, exact revision, and independent archive SHA-256"
        case "$HERDR_RELEASE_ARCHIVE:$HERDR_RELEASE_CHECKSUMS" in
            /*:/*) ;;
            *) fatal "offline release archive and checksums paths must be absolute" ;;
        esac
        [ -f "$HERDR_RELEASE_ARCHIVE" ] && [ ! -L "$HERDR_RELEASE_ARCHIVE" ] ||
            fatal "offline release archive must be a regular non-symlink file"
        [ -f "$HERDR_RELEASE_CHECKSUMS" ] && [ ! -L "$HERDR_RELEASE_CHECKSUMS" ] ||
            fatal "offline release checksums must be a regular non-symlink file"
        [ "${#HERDR_EXPECTED_REVISION}" -eq 40 ] ||
            fatal "offline expected revision must be an exact lowercase commit"
        case "$HERDR_EXPECTED_REVISION" in
            *[!0123456789abcdef]*) fatal "offline expected revision must be an exact lowercase commit" ;;
        esac
        [ "${#HERDR_EXPECTED_ARCHIVE_SHA256}" -eq 64 ] ||
            fatal "offline expected archive SHA-256 must be exact lowercase hex"
        case "$HERDR_EXPECTED_ARCHIVE_SHA256" in
            *[!0123456789abcdef]*) fatal "offline expected archive SHA-256 must be exact lowercase hex" ;;
        esac
        [ "${HERDR_RELEASE_ARCHIVE##*/}" = "$archive" ] ||
            fatal "offline release archive name does not match version and native target"
        input_hash=$(sha256_file "$HERDR_RELEASE_ARCHIVE")
        [ "$input_hash" = "$HERDR_EXPECTED_ARCHIVE_SHA256" ] ||
            fatal "offline release archive does not match independent expected SHA-256"
        cp "$HERDR_RELEASE_ARCHIVE" "$archive_path" || fatal "could not stage offline release archive"
        cp "$HERDR_RELEASE_CHECKSUMS" "$checksums_path" || fatal "could not stage offline release checksums"
        [ "$(sha256_file "$archive_path")" = "$HERDR_EXPECTED_ARCHIVE_SHA256" ] ||
            fatal "offline release archive changed while being staged"
        tag_revision="$HERDR_EXPECTED_REVISION"
        github_fetch_token=
        info "Installing reviewed offline ${BINARY} ${version} (${target})"
    else
        info "Resolving ${BINARY} ${version} (${target}) from ${REPO}"
        fetch_json "https://api.github.com/repos/${REPO}/commits/${tag}" > "$commit_json_path" ||
            fatal "could not resolve release tag from GitHub API (private repositories require GH_TOKEN; SSH access authenticates Git only)"
        commit_json=$(awk '{ printf "%s", $0 }' "$commit_json_path")
        tag_revision=$(resolve_tag_revision "$commit_json")
        case "$tag_revision" in
            ????????????????????????????????????????) ;;
            *) fatal "release tag did not resolve to an exact commit" ;;
        esac
        info "Downloading ${archive} from ${REPO}"
        if [ -n "$github_fetch_token" ]; then
            api_url="https://api.github.com/repos/${REPO}/releases/tags/${tag}"
            release_json_path="$work_dir/release.json"
            fetch_json "$api_url" > "$release_json_path" ||
                fatal "could not fetch release metadata from GitHub API"
            release_json=$(awk '{ printf "%s", $0 }' "$release_json_path")
            archive_url=$(resolve_asset_url "$release_json" "$archive")
            checksum_url=$(resolve_asset_url "$release_json" "checksums.txt")
            [ -n "$archive_url" ] || fatal "release has no asset named $archive"
            [ -n "$checksum_url" ] || fatal "release has no asset named checksums.txt"
            fetch "$checksum_url" "$checksums_path" ||
                fatal "required checksums.txt download failed"
            fetch "$archive_url" "$archive_path" ||
                fatal "release archive download failed"
        else
            base_url="https://github.com/${REPO}/releases/download/${tag}"
            fetch "$base_url/checksums.txt" "$checksums_path" ||
                fatal "required checksums.txt download failed"
            fetch "$base_url/$archive" "$archive_path" ||
                fatal "release archive download failed"
        fi
    fi
    unset github_fetch_token

    matches=$(awk -v name="$archive" '
        NF == 2 {
            file = $2
            sub(/^\*/, "", file)
            if (file == name) print tolower($1)
        }
    ' "$checksums_path")
    count=$(printf '%s\n' "$matches" | awk 'NF { count++ } END { print count + 0 }')
    [ "$count" -eq 1 ] || fatal "checksums.txt must contain one exact entry for $archive"
    expected=$(printf '%s\n' "$matches" | awk 'NF { print; exit }')
    actual=$(sha256_file "$archive_path")
    [ "$expected" = "$actual" ] || fatal "checksum mismatch for $archive"

    validate_archive_paths "$archive_path" || fatal "archive contains an unsafe path"
    mkdir -p "$stage"
    chmod 700 "$stage"
    tar -xzf "$archive_path" -C "$stage" || fatal "release extraction failed"
    [ -x "$stage/$BINARY" ] || fatal "archive is missing the relay executable"
    [ -f "$stage/release-manifest.json" ] || fatal "archive is missing release-manifest.json"

    "$stage/$BINARY" verify-release --target "$target" "$stage" >/dev/null ||
        fatal "release verification failed"
    manifest_version=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$stage/release-manifest.json" | head -1)
    revision=$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$stage/release-manifest.json" | head -1)
    [ "$manifest_version" = "$version" ] || fatal "release manifest version mismatch"
    [ "$revision" = "$tag_revision" ] ||
        fatal "release manifest revision does not match tag commit"

    releases_dir="$release_root/releases"
    final_dir="$releases_dir/${version}-${revision}-${os}-${arch}"
    previous_dir=
    if [ -L "$release_root/current" ]; then
        previous_link=$(readlink "$release_root/current")
        case "$previous_link" in
            /*) previous_dir=$previous_link ;;
            *) previous_dir="$release_root/$previous_link" ;;
        esac
    fi
    prepare_install_roots "$release_root" "$config_root" "$cache_root"
    mkdir -p "$releases_dir" "$shim_dir"
    chmod 700 "$release_root" "$releases_dir"
    if [ -e "$final_dir" ]; then
        "$stage/$BINARY" verify-release --target "$target" "$final_dir" >/dev/null ||
            fatal "existing target release directory is invalid"
    else
        mv "$stage" "$final_dir" || fatal "could not install release directory"
    fi
    "$final_dir/$BINARY" seal-release "$final_dir" ||
        fatal "could not seal installed release directory"
    if [ -n "$previous_dir" ]; then
        "$final_dir/$BINARY" prune-releases "$release_root" "$final_dir" "$previous_dir" ||
            fatal "could not prune obsolete releases"
    else
        "$final_dir/$BINARY" prune-releases "$release_root" "$final_dir" ||
            fatal "could not prune obsolete releases"
    fi
    shim_temp="$shim_dir/.${BINARY}.$$"
    rm -f "$shim_temp"
    ln -s "$release_root/current/$BINARY" "$shim_temp"
    mv -f "$shim_temp" "$shim_dir/$BINARY" ||
        fatal "could not install executable shim"

    "$final_dir/$BINARY" activate-release "$release_root" "$final_dir" ||
        fatal "could not atomically activate the complete release"

    info "Installed ${BINARY} ${version} to $final_dir"
    info "Active release: $release_root/current"
    case ":$PATH:" in
        *":$shim_dir:"*) ;;
        *) printf 'Add %s to PATH.\n' "$shim_dir" >&2 ;;
    esac
}

main "$@"

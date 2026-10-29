#!/bin/bash
# Isolated foreground development transport for the separate CLI Serve owner.
# Only the exact App Store 1.102.4 candidate is development-enabled; this is not
# runtime qualification. The real HOME is preserved for the selected Tailscale CLI; all relay roots,
# credentials, release data, sockets, caches, and journals live in the private
# .dev-tailscale-cli tree. Stopping the process never removes the persistent route.
set -euo pipefail
umask 077

# This qualification profile is fixed to HTTPS Serve 8443 -> loopback relay
# backend 18377, with plugin listener 18378. Reject every override before any
# executable, state root, or Tailscale CLI is touched.
if [ -n "${HERDR_DEV_TAILSCALE_CLI_PORT:-}" ]; then
    echo "✗ Development CLI relay port is fixed at 18377; overrides are refused." >&2
    exit 2
fi
if [ -n "${HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT:-}" ]; then
    echo "✗ Development CLI plugin port is fixed at 18378; overrides are refused." >&2
    exit 2
fi
if [ -n "${HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT:-}" ]; then
    echo "✗ Development CLI HTTPS Serve port is fixed at 8443; overrides are refused." >&2
    exit 2
fi
RELAY_PORT=18377
PLUGIN_PORT=18378
HTTPS_PORT=8443

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd -P)"
DEFAULT_DEV_ROOT="$SCRIPT_DIR/.dev-tailscale-cli"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELAY_BIN="${HERDR_DEV_TAILSCALE_CLI_RELAY_BIN:-}"
if [ -n "$RELAY_BIN" ]; then
    case "$RELAY_BIN" in /*) [ -x "$RELAY_BIN" ] && [ ! -d "$RELAY_BIN" ] || { echo "✗ Selected relay binary is not executable." >&2; exit 2; } ;; *) echo "✗ Relay binary path must be absolute." >&2; exit 2 ;; esac
    "$RELAY_BIN" tailscale-cli activation-check --scope development || exit $?
else
    command -v go >/dev/null 2>&1 || { echo "✗ Go is required to check development-qualification enablement." >&2; exit 2; }
    (cd "$REPO_DIR" && GOTOOLCHAIN=local GOFLAGS=-mod=readonly go run ./cmd/herdr-mobile-relay tailscale-cli activation-check --scope development) || exit $?
fi

GATE_RELAY_BIN="$RELAY_BIN"
cli_relay_call() {
    if [ -n "$GATE_RELAY_BIN" ]; then
        "$GATE_RELAY_BIN" "$@"
    else
        (cd "$REPO_DIR" && GOTOOLCHAIN=local GOFLAGS=-mod=readonly go run ./cmd/herdr-mobile-relay "$@")
    fi
}
# First build has no current binary. Decode only through the same strict Go
# json-field command used by the normal relay helper, never shell JSON parsing.
cli_discovery_string() {
    printf '%s' "$1" | cli_relay_call json-field string "$2" 2>/dev/null
}

usage() {
    echo "CLI-backed development uses a separate development-scope registration and private state." >&2
    echo "It requires an unambiguous absolute Tailscale CLI and explicit development opt-in." >&2
    echo "Fresh setup discovers the node ID and canonical HTTPS origin; optional configured identities must match." >&2
    echo "Interactive: make dev-tailscale-cli (development opt-in; then route publication consent)." >&2
    echo "Scripted transport opt-in: HERDR_DEV_TAILSCALE_CLI_ENABLE=1 make dev-tailscale-cli"
    echo "Route publication shows the address and asks for y/yes on stdin; no environment variable can consent." >&2
    echo "Optional: HERDR_DEV_TAILSCALE_CLI_DIR, HERDR_DEV_TAILSCALE_CLI_BIN," >&2
    echo "  HERDR_DEV_TAILSCALE_CLI_ORIGIN, HERDR_DEV_TAILSCALE_CLI_NODE_ID, HERDR_DEV_HERDR_BIN." >&2
    echo "Lifecycle: setup, repair-missing, status, recover, reconcile, abandon-missing, release-reservation, update, unpublish, and stop (Ctrl-C)." >&2
}

ACTION="${1:-setup}"
[ "$#" -eq 0 ] || shift
case "$ACTION" in setup|repair-missing|status|recover|reconcile|abandon-missing|release-reservation|unpublish|update|stop) ;; *) usage; exit 2 ;; esac

case "${HERDR_DEV_TAILSCALE_CLI_ENABLE:-}" in
    1) ;;
    '')
        [ -t 0 ] || { usage; exit 2; }
        read -r -p "Set up isolated CLI-backed Serve development? [y/N] " answer || exit 2
        case "$answer" in y|Y|yes|YES) export HERDR_DEV_TAILSCALE_CLI_ENABLE=1 ;; *) echo "Cancelled; nothing was started."; exit 2 ;; esac
        ;;
    *) echo "✗ HERDR_DEV_TAILSCALE_CLI_ENABLE must be 1 to opt in." >&2; exit 2 ;;
esac

DEV_ROOT="${HERDR_DEV_TAILSCALE_CLI_DIR:-$DEFAULT_DEV_ROOT}"
case "$DEV_ROOT" in /*) ;; *) echo "✗ Development state path must be absolute." >&2; exit 2 ;; esac
[ ! -L "$DEV_ROOT" ] || { echo "✗ Development root cannot be a symlink." >&2; exit 2; }
if [ -d "$DEV_ROOT" ]; then
    DEV_ROOT="$(cd "$DEV_ROOT" && pwd -P)"
elif [ "$DEV_ROOT" != "$DEFAULT_DEV_ROOT" ]; then
    echo "✗ Custom development roots must already exist with mode 0700." >&2
    exit 2
fi
[ "$DEV_ROOT" != / ] && [ "$DEV_ROOT" != "$HOME" ] || { echo "✗ Root or home cannot be a development state directory." >&2; exit 2; }

# Keep the AF_UNIX control socket independent of checkout depth. The complete
# root hash gives each checkout a stable, isolated directory under a short
# user-private temporary base; Go independently derives and validates it.
case "$(uname -s)" in
    Darwin) SOCKET_TMP_BASE=/private/tmp/herdr-cli-$(id -u) ;;
    Linux) SOCKET_TMP_BASE=/tmp/herdr-cli-$(id -u) ;;
    *) echo "✗ Only Linux and macOS development are supported." >&2; exit 2 ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
    DEV_ROOT_HASH="$(printf '%s' "$DEV_ROOT" | sha256sum)"
elif command -v shasum >/dev/null 2>&1; then
    DEV_ROOT_HASH="$(printf '%s' "$DEV_ROOT" | shasum -a 256)"
else
    echo "✗ sha256sum or shasum is required to isolate the short development control socket." >&2
    exit 2
fi
DEV_ROOT_HASH="${DEV_ROOT_HASH%% *}"
case "$DEV_ROOT_HASH" in *[!0-9a-f]*|'') echo "✗ Could not derive the private development socket directory." >&2; exit 2 ;; esac
SOCKET_DIR="$SOCKET_TMP_BASE/$DEV_ROOT_HASH"
PAIRING_SOCKET="$SOCKET_DIR/p.sock"

ensure_private_socket_directory() {
    local path mode owner
    for path in "$SOCKET_TMP_BASE" "$SOCKET_DIR"; do
        if [ -e "$path" ] || [ -L "$path" ]; then
            [ -d "$path" ] && [ ! -L "$path" ] || { echo "✗ Development control-socket directory is not a real directory: $path" >&2; return 1; }
            case "$(uname -s)" in
                Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
                Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
            esac
            [ "$mode" = 700 ] && [ "$owner" = "$(id -u)" ] || { echo "✗ Existing development control-socket directory has unsafe ownership or permissions: $path" >&2; return 1; }
        else
            mkdir -m 700 "$path" || return 1
        fi
    done
}

dev_current_release() {
    local target release_name resolved
    [ -L "$DEV_ROOT/current" ] || return 1
    target="$(readlink "$DEV_ROOT/current")" || return 1
    case "$target" in releases/*) release_name="${target#releases/}" ;; *) return 1 ;; esac
    case "$release_name" in ''|.|..|*/*|*[!A-Za-z0-9._-]*) return 1 ;; esac
    [ -d "$DEV_ROOT/releases/$release_name" ] && [ ! -L "$DEV_ROOT/releases/$release_name" ] || return 1
    resolved="$(cd "$DEV_ROOT/current" 2>/dev/null && pwd -P)" || return 1
    [ "$resolved" = "$DEV_ROOT/releases/$release_name" ] || return 1
    [ -d "$resolved/bin" ] && [ ! -L "$resolved/bin" ] &&
        [ -f "$resolved/bin/herdr-mobile-relay" ] && [ ! -L "$resolved/bin/herdr-mobile-relay" ] &&
        [ -x "$resolved/bin/herdr-mobile-relay" ] && [ -d "$resolved/web" ] && [ ! -L "$resolved/web" ] || return 1
    printf '%s\n' "$resolved"
}

DEFAULT_SHARED_COORDINATION_ROOT="$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination"
DEV_ENV_FILE="$DEV_ROOT/relay.env"
INSTALLED_ENV_FILE="$(installed_service_env_file 2>/dev/null || true)"
if [ -n "$INSTALLED_ENV_FILE" ]; then
    PRODUCTION_ENV_FILE="$INSTALLED_ENV_FILE"
    if [ -n "${HERDR_RELAY_ENV:-}" ] &&
        [ "$(canonical_file_path "$HERDR_RELAY_ENV")" != "$(canonical_file_path "$DEV_ENV_FILE")" ] &&
        [ "$(canonical_file_path "$HERDR_RELAY_ENV")" != "$(canonical_file_path "$INSTALLED_ENV_FILE")" ]; then
        echo "✗ Inherited HERDR_RELAY_ENV differs from the installed service's environment file." >&2
        exit 2
    fi
elif [ -n "${HERDR_RELAY_ENV:-}" ] && [ "$HERDR_RELAY_ENV" != "$DEV_ENV_FILE" ]; then
    PRODUCTION_ENV_FILE="$HERDR_RELAY_ENV"
elif [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
    PRODUCTION_ENV_FILE="$HERDR_PLUGIN_CONFIG_DIR/relay.env"
else
    PRODUCTION_ENV_FILE="$SCRIPT_DIR/.env"
fi
export HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE="$PRODUCTION_ENV_FILE"
# Do not source a production-path alias of the development relay.env before
# validating migration state. This also catches a symlink alias of the file.
if { [ -e "$PRODUCTION_ENV_FILE" ] || [ -L "$PRODUCTION_ENV_FILE" ]; } &&
    [ -e "$DEV_ENV_FILE" ] && [ "$PRODUCTION_ENV_FILE" -ef "$DEV_ENV_FILE" ]; then
    echo "✗ Production and development relay environments resolve to the same file; refusing before reading it." >&2
    exit 2
fi
PRODUCTION_COORDINATION_ROOT=""
if [ -e "$PRODUCTION_ENV_FILE" ] || [ -L "$PRODUCTION_ENV_FILE" ]; then
    [ -f "$PRODUCTION_ENV_FILE" ] && [ ! -L "$PRODUCTION_ENV_FILE" ] || {
        echo "✗ Installed production environment is not a regular file." >&2
        exit 2
    }
    PRODUCTION_COORDINATION_ROOT="$(env_file_value "$PRODUCTION_ENV_FILE" HERDR_TAILSCALE_CLI_COORDINATION_ROOT)"
fi
COORDINATION_ROOT="${PRODUCTION_COORDINATION_ROOT:-$DEFAULT_SHARED_COORDINATION_ROOT}"
if [ -n "${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-}" ] &&
    [ "$HERDR_TAILSCALE_CLI_COORDINATION_ROOT" != "$COORDINATION_ROOT" ]; then
    echo "✗ Inherited coordination root differs from the installed service's shared root." >&2
    exit 2
fi
case "$COORDINATION_ROOT" in /*) ;; *) echo "✗ Shared coordination root must be absolute." >&2; exit 2 ;; esac
case "$DEV_ROOT$COORDINATION_ROOT" in *$'\n'*|*$'\r'*) echo "✗ Development and coordination roots cannot contain line breaks." >&2; exit 2 ;; esac
if [ -e "$COORDINATION_ROOT" ] || [ -L "$COORDINATION_ROOT" ]; then
    [ -d "$COORDINATION_ROOT" ] && [ ! -L "$COORDINATION_ROOT" ] || {
        echo "✗ Shared coordination root must be a real directory." >&2
        exit 2
    }
    COORDINATION_ROOT="$(cd "$COORDINATION_ROOT" && pwd -P)"
fi
case "$DEV_ROOT/" in "$COORDINATION_ROOT/"*) echo "✗ Shared coordination root overlaps development state." >&2; exit 2 ;; esac
case "$COORDINATION_ROOT/" in "$DEV_ROOT/"*) echo "✗ Shared coordination root overlaps development state." >&2; exit 2 ;; esac

for protected in \
    "${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay" \
    "${HERDR_PLUGIN_CONFIG_DIR:-$HOME/.config/herdr-mobile-relay}" \
    "${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay" \
    "${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay" \
    "$SCRIPT_DIR/.dev-tailscale" "$SCRIPT_DIR/.dev"; do
    [ -d "$protected" ] || continue
    canonical_protected="$(cd "$protected" && pwd -P)"
    case "$DEV_ROOT/" in "$canonical_protected/"*) echo "✗ Development root overlaps installed or other development state." >&2; exit 2 ;; esac
    case "$canonical_protected/" in "$DEV_ROOT/"*) echo "✗ Installed or other development state overlaps this root." >&2; exit 2 ;; esac
done
case "$DEV_ROOT/" in "$REPO_DIR/relay/.dev-tailscale/"*|"$REPO_DIR/relay/.dev/"*) echo "✗ Do not reuse managed-Tailscale or tunnel development state." >&2; exit 2 ;; esac
[ -z "${HERDR_RELAY_ENV:-}" ] || [ "$HERDR_RELAY_ENV" = "$DEV_ROOT/relay.env" ] || {
    echo "✗ Inherited relay environment conflicts with the isolated development root." >&2
    exit 2
}

if [ "$ACTION" = stop ]; then
    echo "This development relay is foreground-only. Send Ctrl-C in its terminal; the CLI Serve route and journal remain configured."
    exit 0
fi

ENV_FILE="$DEV_ROOT/relay.env"
MARKER="$DEV_ROOT/.herdr-dev-tailscale-cli"
MIGRATE_LEGACY_SOCKET=0
PENDING="$DEV_ROOT/.setup-pending"
BUILD_DIR=""
NEXT_POINTER=""
SETUP_STAGE=""
COMPLETE_STAGE=""
# Save process choices before loading the persisted literal environment.
REQUESTED_PHONE_APP="${HERDR_DEV_PHONE_APP_URL:-${HERDR_PHONE_APP_URL:-}}"
REQUESTED_ORIGIN="${HERDR_DEV_TAILSCALE_CLI_ORIGIN:-${HERDR_TAILSCALE_CLI_ORIGIN:-}}"
REQUESTED_NODE_ID="${HERDR_DEV_TAILSCALE_CLI_NODE_ID:-${HERDR_TAILSCALE_CLI_NODE_ID:-}}"
# shellcheck disable=SC2329 # Invoked through the setup EXIT-trap cleanup function.
cleanup_dev_build_stage() {
    case "$BUILD_DIR" in "$DEV_ROOT"/.build.*) [ ! -d "$BUILD_DIR" ] || rm -rf "$BUILD_DIR" ;; esac
    case "$NEXT_POINTER" in "$DEV_ROOT"/.current.*) [ ! -L "$NEXT_POINTER" ] || rm -f "$NEXT_POINTER" ;; esac
}
# shellcheck disable=SC2329 # Registered indirectly as an EXIT trap.
cleanup_dev_setup() {
    if [ -d "$PENDING" ]; then
        recover_dev_setup || echo "✗ Interrupted setup retained uncertain state; inspect .setup-pending before retrying." >&2
    fi
    case "$SETUP_STAGE" in "$DEV_ROOT"/.setup-stage.*) [ ! -d "$SETUP_STAGE" ] || rm -rf "$SETUP_STAGE" ;; esac
    case "$COMPLETE_STAGE" in "$DEV_ROOT"/.setup-complete.*) cleanup_complete_setup || true ;; esac
    cleanup_dev_build_stage
}
dev_owned_private_directory() {
    local path="$1" mode owner
    [ -d "$path" ] && [ ! -L "$path" ] || return 1
    case "$(uname -s)" in
        Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
        Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
        *) return 1 ;;
    esac
    [ "$mode" = 700 ] && [ "$owner" = "$(id -u)" ]
}
dev_owned_private_file() {
    local path="$1" mode owner
    [ -f "$path" ] && [ ! -L "$path" ] || return 1
    case "$(uname -s)" in
        Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
        Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
        *) return 1 ;;
    esac
    [ "$mode" = 600 ] && [ "$owner" = "$(id -u)" ]
}
# All snapshots are private, regular, exact 0600 bytes. An absent snapshot
# means the path was absent, not that arbitrary content may be repaired.
cleanup_complete_setup() {
    [ -d "$COMPLETE_STAGE" ] && [ ! -L "$COMPLETE_STAGE" ] || return 1
    for name in candidate-pointer candidate-env candidate-marker old-pointer old-env old-marker old-existence; do
        [ ! -e "$COMPLETE_STAGE/$name" ] ||
            { dev_owned_private_file "$COMPLETE_STAGE/$name" && rm "$COMPLETE_STAGE/$name"; } || return 1
    done
    rmdir "$COMPLETE_STAGE"
}
setup_snapshot_matches() {
    local path="$1" snapshot="$2"
    if [ -f "$snapshot" ]; then
        dev_owned_private_file "$path" && cmp -s "$snapshot" "$path"
    else
        [ ! -e "$path" ] && [ ! -L "$path" ]
    fi
}
setup_old_or_candidate() {
    setup_snapshot_matches "$1" "$PENDING/old-$2" ||
        setup_snapshot_matches "$1" "$PENDING/candidate-$2"
}
setup_pointer_matches() {
    local pointer="$1" expected="$2"
    if [ -f "$expected" ]; then
        [ -L "$pointer" ] && [ "$(readlink "$pointer")" = "$(<"$expected")" ]
    else
        [ ! -e "$pointer" ] && [ ! -L "$pointer" ]
    fi
}
replace_dev_pointer() {
    local target="$1" staged="$DEV_ROOT/.current.$$"
    [ ! -e "$staged" ] && [ ! -L "$staged" ] || return 1
    ln -s "$target" "$staged" || return 1
    NEXT_POINTER="$staged"
    case "$(uname -s)" in
        Linux) mv -fT "$staged" "$DEV_ROOT/current" ;;
        Darwin) mv -f -h "$staged" "$DEV_ROOT/current" ;;
        *) return 1 ;;
    esac || return 1
    NEXT_POINTER=""
}
setup_pending_snapshots_valid() {
    local old_pointer_present=0 old_env_present=0 old_marker_present=0
    dev_owned_private_directory "$DEV_ROOT" && dev_owned_private_directory "$PENDING" || return 1
    for name in candidate-pointer candidate-env candidate-marker old-existence; do
        dev_owned_private_file "$PENDING/$name" || return 1
    done
    for name in old-pointer old-env old-marker restore-env; do
        [ ! -e "$PENDING/$name" ] && [ ! -L "$PENDING/$name" ] ||
            dev_owned_private_file "$PENDING/$name" || return 1
    done
    [ ! -f "$PENDING/old-pointer" ] || old_pointer_present=1
    [ ! -f "$PENDING/old-env" ] || old_env_present=1
    [ ! -f "$PENDING/old-marker" ] || old_marker_present=1
    printf 'pointer=%s\nenv=%s\nmarker=%s\n' "$old_pointer_present" "$old_env_present" "$old_marker_present" |
        cmp -s - "$PENDING/old-existence" || return 1
    [ "$old_env_present" = "$old_marker_present" ] &&
        { [ "$old_pointer_present" = 0 ] || [ "$old_env_present" = 1 ]; } || return 1
    if [ -f "$PENDING/restore-env" ]; then
        [ -f "$PENDING/old-env" ] && cmp -s "$PENDING/restore-env" "$PENDING/old-env" || return 1
    fi
    for name in env marker; do
        if [ -e "$PENDING/publish-$name" ] || [ -L "$PENDING/publish-$name" ]; then
            dev_owned_private_file "$PENDING/publish-$name" &&
                cmp -s "$PENDING/publish-$name" "$PENDING/candidate-$name" || return 1
        fi
    done
}
recover_dev_setup() {
    local old_target
    [ -e "$PENDING" ] || [ -L "$PENDING" ] || return 0
    setup_pending_snapshots_valid || return 1
    setup_old_or_candidate "$ENV_FILE" env &&
        setup_old_or_candidate "$MARKER" marker &&
        { setup_pointer_matches "$DEV_ROOT/current" "$PENDING/old-pointer" ||
          setup_pointer_matches "$DEV_ROOT/current" "$PENDING/candidate-pointer"; } || return 1
    # A rollback to an old pointer is proved only if its release still has
    # the required private, complete shape. Unknown prior bytes are retained.
    if [ -f "$PENDING/old-pointer" ]; then
        old_target="$(<"$PENDING/old-pointer")"
        case "${old_target#releases/}" in ''|.|..|*/*|*[!A-Za-z0-9._-]*) return 1 ;; esac
        case "$old_target" in releases/*) ;; *) return 1 ;; esac
        dev_owned_private_directory "$DEV_ROOT/$old_target" &&
            [ -d "$DEV_ROOT/$old_target/bin" ] && [ ! -L "$DEV_ROOT/$old_target/bin" ] &&
            [ -d "$DEV_ROOT/$old_target/web" ] && [ ! -L "$DEV_ROOT/$old_target/web" ] &&
            [ -f "$DEV_ROOT/$old_target/web/version.json" ] && [ ! -L "$DEV_ROOT/$old_target/web/version.json" ] &&
            [ -f "$DEV_ROOT/$old_target/bin/herdr-mobile-relay" ] &&
            [ ! -L "$DEV_ROOT/$old_target/bin/herdr-mobile-relay" ] &&
            [ -x "$DEV_ROOT/$old_target/bin/herdr-mobile-relay" ] || return 1
    fi
    # No route operation has run while this journal exists. Roll back only
    # exact candidate bytes; never overwrite a changed saved app or marker.
    if setup_pointer_matches "$DEV_ROOT/current" "$PENDING/candidate-pointer"; then
        if [ -f "$PENDING/old-pointer" ]; then
            replace_dev_pointer "$old_target" || return 1
        else
            [ -L "$DEV_ROOT/current" ] &&
                [ "$(readlink "$DEV_ROOT/current")" = "$(<"$PENDING/candidate-pointer")" ] || return 1
            rm "$DEV_ROOT/current" || return 1
        fi
    fi
    if [ -f "$PENDING/old-env" ] && ! setup_snapshot_matches "$ENV_FILE" "$PENDING/old-env"; then
        [ -f "$PENDING/restore-env" ] || cp -p "$PENDING/old-env" "$PENDING/restore-env" || return 1
        setup_snapshot_matches "$ENV_FILE" "$PENDING/candidate-env" &&
            mv -f "$PENDING/restore-env" "$ENV_FILE" || return 1
    elif [ ! -f "$PENDING/old-env" ] && setup_snapshot_matches "$ENV_FILE" "$PENDING/candidate-env"; then
        rm "$ENV_FILE" || return 1
    fi
    if [ ! -f "$PENDING/old-marker" ] && setup_snapshot_matches "$MARKER" "$PENDING/candidate-marker"; then
        rm "$MARKER" || return 1
    fi
    setup_snapshot_matches "$ENV_FILE" "$PENDING/old-env" &&
        setup_snapshot_matches "$MARKER" "$PENDING/old-marker" &&
        setup_pointer_matches "$DEV_ROOT/current" "$PENDING/old-pointer" || return 1
    for name in candidate-pointer candidate-env candidate-marker old-pointer old-env old-marker old-existence publish-env publish-marker restore-env; do
        [ ! -e "$PENDING/$name" ] || rm "$PENDING/$name" || return 1
    done
    rmdir "$PENDING"
}
dev_root_marker_matches() {
    dev_owned_private_file "$MARKER" || return 1
    printf '%s\n' 'HERDR_DEV_TAILSCALE_CLI_ROOT=1' \
        "HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=$DEV_ROOT/registration" \
        "HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=$COORDINATION_ROOT" | cmp -s - "$MARKER"
}
dev_legacy_socket_parent_is_safe() {
    [ ! -e "$DEV_ROOT/config" ] && [ ! -L "$DEV_ROOT/config" ] ||
        dev_owned_private_directory "$DEV_ROOT/config"
}
dev_socket_setting_matches() {
    local value="$1" assignment_count plain_count expected
    assignment_count="$(grep -Ec '^[[:space:]]*(export[[:space:]]+)?HERDR_RELAY_PAIRING_SOCKET=' "$ENV_FILE" || true)"
    plain_count="$(grep -c '^HERDR_RELAY_PAIRING_SOCKET=' "$ENV_FILE" || true)"
    [ "$assignment_count" = 1 ] && [ "$plain_count" = 1 ] || return 1
    expected="HERDR_RELAY_PAIRING_SOCKET=$(shell_quote_value "$value")"
    grep -Fqx "$expected" "$ENV_FILE"
}
if [ "$ACTION" = setup ] || [ "$ACTION" = update ]; then
    # Cancellation can arrive during read-only discovery, before a build or
    # pending journal exists. Keep its status distinct from a build failure.
    trap 'exit 130' INT
    trap 'exit 143' TERM
    # A previous interrupted cutover is reversible only while EVERY live
    # input is still exactly its recorded old or candidate byte/mode state.
    recover_dev_setup || exit 1
elif [ -e "$PENDING" ] || [ -L "$PENDING" ]; then
    echo "✗ Interrupted setup needs proved recovery before any route action." >&2
    exit 1
fi
if { [ "$ACTION" = setup ] || [ "$ACTION" = update ]; } &&
    { [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; }; then
    # Validate the root, environment file and binding marker before inspecting
    # relay.env. Never source legacy state to decide whether it may be migrated.
    if ! dev_owned_private_directory "$DEV_ROOT" || ! dev_owned_private_file "$ENV_FILE" ||
        ! dev_root_marker_matches; then
        echo "✗ Existing development state is not a marked private CLI root; refused before reading relay.env." >&2
        exit 1
    fi
    legacy_pairing_socket="$DEV_ROOT/config/pairing-control.sock"
    if dev_socket_setting_matches "$legacy_pairing_socket" &&
        dev_legacy_socket_parent_is_safe &&
        [ ! -e "$legacy_pairing_socket" ] && [ ! -L "$legacy_pairing_socket" ]; then
        MIGRATE_LEGACY_SOCKET=1
    fi
    if [ "$MIGRATE_LEGACY_SOCKET" != 1 ] && ! dev_socket_setting_matches "$PAIRING_SOCKET"; then
        echo "✗ Existing development state records a different pairing socket; it was retained without migration. Choose a separate private root or follow a reviewed migration before setup/update." >&2
        exit 2
    fi
fi
assert_fixed_development_ports() {
    if [ "${HERDR_RELAY_PORT:-}" != "$RELAY_PORT" ] ||
        [ "${HERDR_RELAY_PLUGIN_PORT:-}" != "$PLUGIN_PORT" ] ||
        [ "${HERDR_TAILSCALE_CLI_HTTPS_PORT:-}" != "$HTTPS_PORT" ]; then
        echo "✗ Existing development ports differ from fixed HTTPS 8443 -> backend 18377/plugin 18378; state was retained." >&2
        return 1
    fi
}
if [ "$ACTION" != setup ]; then
    if ! dev_owned_private_directory "$DEV_ROOT" || ! dev_owned_private_file "$ENV_FILE" ||
        ! dev_root_marker_matches; then
        echo "✗ No marked CLI development state exists; nothing was inspected or removed." >&2
        exit 1
    fi
    load_relay_env "$ENV_FILE"
    assert_fixed_development_ports
    if [ -n "${HERDR_DEV_TAILSCALE_CLI_RELAY_BIN:-}" ]; then
        RELAY_BIN="$HERDR_DEV_TAILSCALE_CLI_RELAY_BIN"
        if current_release="$(dev_current_release)"; then WEB_ROOT="$DEV_ROOT/current/web"; else WEB_ROOT="$DEV_ROOT/web"; fi
    elif current_release="$(dev_current_release)"; then
        RELAY_BIN="$DEV_ROOT/current/bin/herdr-mobile-relay"
        WEB_ROOT="$DEV_ROOT/current/web"
    elif [ -x "$DEV_ROOT/bin/herdr-mobile-relay" ]; then
        RELAY_BIN="$DEV_ROOT/bin/herdr-mobile-relay" # Legacy root from pre-transactional development builds.
        WEB_ROOT="$DEV_ROOT/web"
    else
        echo "✗ No coherent CLI development release is available; no state was changed." >&2
        exit 1
    fi
    CLI_BIN="${HERDR_TAILSCALE_CLI_BIN:-}"
    case "$ACTION" in
        status|recover|reconcile|release-reservation|unpublish|abandon-missing)
            if { [ "$ACTION" = unpublish ] || [ "$ACTION" = abandon-missing ] || [ "$ACTION" = reconcile ]; } && [ -S "$PAIRING_SOCKET" ]; then
                echo "✗ Stop the foreground development relay before scoped route cleanup; the route remains configured." >&2
                exit 1
            fi
            export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$RELAY_BIN"
            export HERDR_WEB_ROOT="$WEB_ROOT" HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
            export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
            export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN" HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT="$DEV_ROOT"
            export HERDR_TAILSCALE_CLI_COORDINATION_ROOT="$COORDINATION_ROOT"
            export HERDR_TAILSCALE_CLI_SCOPE=development HERDR_RELAY_PORT="$RELAY_PORT" HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT"
            exec "$RELAY_BIN" dev-tailscale-cli "$ACTION"
            ;;
        update)
            [ ! -S "$PAIRING_SOCKET" ] || {
                echo "✗ Stop the foreground development relay before replacing its build; registration was retained." >&2
                exit 1
            }
            export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$RELAY_BIN"
            export HERDR_WEB_ROOT="$WEB_ROOT" HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
            export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
            export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN" HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT="$DEV_ROOT"
            export HERDR_TAILSCALE_CLI_COORDINATION_ROOT="$COORDINATION_ROOT"
            export HERDR_TAILSCALE_CLI_SCOPE=development HERDR_RELAY_PORT="$RELAY_PORT" HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT"
            "$RELAY_BIN" dev-tailscale-cli assert-ready >/dev/null || {
                echo "✗ Exact development route is not ready; no build or route mutation was performed." >&2
                exit 1
            }
            ;;

    esac
fi

# A missing custom root is not created. The checkout-local root is created only
# after activation, path, CLI, origin, socket, port, and explicit route-consent checks.
if [ -d "$DEV_ROOT" ]; then
    [ "$(uname -s)" = Darwin ] && root_mode="$(stat -f '%Lp' "$DEV_ROOT")" || root_mode="$(stat -c '%a' "$DEV_ROOT")"
    [ "$root_mode" = 700 ] || { echo "✗ Development root must already have mode 0700; it is never repaired." >&2; exit 2; }
    if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
        if [ ! -f "$ENV_FILE" ] || [ -L "$ENV_FILE" ] || [ ! -f "$MARKER" ] ||
            [ -L "$MARKER" ] || ! grep -Fxq 'HERDR_DEV_TAILSCALE_CLI_ROOT=1' "$MARKER"; then
            echo "✗ Existing state is not a marked private CLI development relay; no bytes were changed." >&2
            exit 1
        fi
        load_relay_env "$ENV_FILE"
        assert_fixed_development_ports
    else
        [ ! -e "$MARKER" ] && [ ! -L "$MARKER" ] || { echo "✗ Stale development marker needs inspection." >&2; exit 1; }
    fi
else
    [ "$DEV_ROOT" = "$DEFAULT_DEV_ROOT" ] || { echo "✗ Custom development root must already exist." >&2; exit 2; }
fi

CLI_BIN="${HERDR_DEV_TAILSCALE_CLI_BIN:-${HERDR_TAILSCALE_CLI_BIN:-}}"
case "$CLI_BIN" in
    "") ;;
    /*) [ -x "$CLI_BIN" ] && [ ! -d "$CLI_BIN" ] || { echo "✗ Selected Tailscale CLI override is not executable." >&2; exit 2; } ;;
    *) echo "✗ An explicit Tailscale CLI override must be absolute." >&2; exit 2 ;;
esac
HERDR_DEV_HERDR_BIN="${HERDR_DEV_HERDR_BIN:-${HERDR_BIN:-}}"
if [ -z "$HERDR_DEV_HERDR_BIN" ]; then
    HERDR_DEV_HERDR_BIN="$(type -P herdr 2>/dev/null || true)"
fi
case "$HERDR_DEV_HERDR_BIN" in /*) [ -x "$HERDR_DEV_HERDR_BIN" ] && [ ! -d "$HERDR_DEV_HERDR_BIN" ] || { echo "✗ Select an absolute executable Herdr path." >&2; exit 2; } ;; *) echo "✗ Install Herdr on PATH or set HERDR_DEV_HERDR_BIN to its absolute executable." >&2; exit 2 ;; esac
HERDR_DEV_HERDR_SOCKET="${HERDR_DEV_HERDR_SOCKET:-${HERDR_SOCKET_PATH:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/herdr.sock}}"
case "$HERDR_DEV_HERDR_SOCKET" in /*) ;; *) echo "✗ Herdr socket path must be absolute." >&2; exit 2 ;; esac
[ -S "$HERDR_DEV_HERDR_SOCKET" ] || { echo "✗ Herdr socket does not exist; it is not contacted during selection." >&2; exit 2; }

DNS_NAME=""
PROFILE=""
CONFIGURED_ORIGIN="${REQUESTED_ORIGIN:-${HERDR_TAILSCALE_CLI_ORIGIN:-}}"
PHONE_APP="${REQUESTED_PHONE_APP:-${HERDR_PHONE_APP_URL:-}}"
CONFIGURED_NODE_ID="${REQUESTED_NODE_ID:-${HERDR_TAILSCALE_CLI_NODE_ID:-}}"
if [ -n "$PHONE_APP" ]; then
    NORMALIZED_APP="$(cli_relay_call normalize-external-origin "$PHONE_APP")" || {
        echo "✗ The selected phone-app origin is not canonical HTTPS; nothing was started." >&2; exit 2;
    }
    [ "$NORMALIZED_APP" = "$PHONE_APP" ] || { echo "✗ Noncanonical phone-app origin was refused." >&2; exit 2; }
fi
if [ "$ACTION" = setup ]; then
    CLI_BIN="$(cli_relay_call tailscale-cli resolve-binary --binary "$CLI_BIN")" || {
        echo "✗ No unambiguous absolute Tailscale CLI candidate was selected." >&2; exit 2;
    }
fi
if [ -f "$ENV_FILE" ]; then
    [ -n "$PHONE_APP" ] && [ -n "$CONFIGURED_ORIGIN" ] && [ -n "$CONFIGURED_NODE_ID" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_PHONE_APP_URL)" = "$PHONE_APP" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_ORIGIN)" = "$CONFIGURED_ORIGIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_NODE_ID)" = "$CONFIGURED_NODE_ID" ] || {
        echo "✗ Existing CLI profile/origin changed; registration requires explicit recovery." >&2; exit 1;
    }
    ORIGIN="$CONFIGURED_ORIGIN"
    NODE_ID="$CONFIGURED_NODE_ID"
else
    [ "$ACTION" = setup ] || { echo "✗ Update requires existing isolated CLI development state." >&2; exit 1; }
    # Read-only bootstrap uses the same Go CLI Preflight/Origin implementation
    # as full preflight. No placeholder is ever written to relay.env or runtime.
    export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN" HERDR_TAILSCALE_CLI_ORIGIN="$CONFIGURED_ORIGIN"
    export HERDR_TAILSCALE_CLI_NODE_ID="$CONFIGURED_NODE_ID" HERDR_PHONE_APP_URL="$PHONE_APP"
    export HERDR_TAILSCALE_CLI_SCOPE=development HERDR_RELAY_TRANSPORT=tailscale-cli
    export HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT="$DEV_ROOT" HERDR_TAILSCALE_CLI_STATE_ROOT="$DEV_ROOT/registration"
    export HERDR_TAILSCALE_CLI_COORDINATION_ROOT="$COORDINATION_ROOT" HERDR_TAILSCALE_CLI_HTTPS_PORT="$HTTPS_PORT"
    export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$DEV_ROOT/current/bin/herdr-mobile-relay"
    export HERDR_WEB_ROOT="$DEV_ROOT/current/web" HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
    export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
    export HERDR_RELAY_HOST=127.0.0.1 HERDR_RELAY_PORT="$RELAY_PORT" HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT"
    export HERDR_RELAY_PAIRING_SOCKET="$PAIRING_SOCKET" HERDR_REACHABILITY_PORT_MAPPING=0 HERDR_RELAY_REARM_BOOTSTRAP=0
    export HERDR_BIN="$HERDR_DEV_HERDR_BIN" HERDR_SOCKET_PATH="$HERDR_DEV_HERDR_SOCKET"
    DISCOVERY="$(cli_relay_call dev-tailscale-cli discover)" || {
        echo "✗ Read-only Go-owned development origin discovery failed; no route or state was created." >&2; exit 1;
    }
    ORIGIN="$(cli_discovery_string "$DISCOVERY" origin)" || exit 1
    NODE_ID="$(cli_discovery_string "$DISCOVERY" node_id)" || exit 1
    DNS_NAME="$(cli_discovery_string "$DISCOVERY" dns_name)" || exit 1
    PROFILE="$(cli_discovery_string "$DISCOVERY" profile)" || exit 1
    if [ -z "$NODE_ID" ] || [ -z "$DNS_NAME" ] || [ -z "$ORIGIN" ] ||
        [ "$PROFILE" != macos-appstore-1.102.4-supplied-metadata ] ||
        [ "$ORIGIN" = https://preflight.invalid:8443 ] || [ "$NODE_ID" = node-preflight-pending ] ||
        { [ -n "$CONFIGURED_ORIGIN" ] && [ "$ORIGIN" != "$CONFIGURED_ORIGIN" ]; } ||
        { [ -n "$CONFIGURED_NODE_ID" ] && [ "$NODE_ID" != "$CONFIGURED_NODE_ID" ]; }; then
        echo "✗ Discovery did not match the exact configured node and HTTPS origin." >&2
        exit 1
    fi
    PHONE_APP="${PHONE_APP:-$ORIGIN}"
    echo "Go-owned read-only discovery selected node $NODE_ID ($DNS_NAME), profile $PROFILE, HTTPS origin $ORIGIN."
fi

if [ "$ACTION" = setup ]; then
    echo "Development qualification is profile-gated; runtime and phone qualification remain pending."
    echo "The launcher will show the address and ask before publishing the route."
    [ -f "$ENV_FILE" ] || true
else
    [ -f "$ENV_FILE" ] || { echo "✗ Update requires existing isolated CLI development state." >&2; exit 1; }
fi

[ ! -S "$PAIRING_SOCKET" ] || {
    echo "✗ Stop the foreground development relay before replacing its build; registration was retained." >&2
    exit 1
}
if ! command -v go >/dev/null 2>&1 || ! command -v bun >/dev/null 2>&1; then
    echo "✗ Development build requires Go 1.27.1 and Bun 1.4.0." >&2
    exit 2
fi
[ "$(uname -s)" = Darwin ] || [ "$(uname -s)" = Linux ] || { echo "✗ Only Linux and macOS development are supported." >&2; exit 2; }
if [ ! -d "$DEV_ROOT" ]; then mkdir -m 700 "$DEV_ROOT"; fi
[ -d "$DEV_ROOT" ] && [ ! -L "$DEV_ROOT" ] || { echo "✗ Development root is not a real directory." >&2; exit 2; }
case "$(uname -s)" in Darwin) root_mode="$(stat -f '%Lp' "$DEV_ROOT")" ;; Linux) root_mode="$(stat -c '%a' "$DEV_ROOT")" ;; esac
[ "$root_mode" = 700 ] || { echo "✗ Development root must be mode 0700." >&2; exit 2; }
for leaf in config cache data web bin registration releases; do
    path="$DEV_ROOT/$leaf"
    [ ! -L "$path" ] || { echo "✗ Symlink inside development root is refused." >&2; exit 2; }
    if [ -e "$path" ]; then
        [ -d "$path" ] || { echo "✗ Development state path is not a directory: $path" >&2; exit 2; }
        case "$(uname -s)" in
            Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
            Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
            *) echo "✗ Only Linux and macOS development are supported." >&2; exit 2 ;;
        esac
        [ "$mode" = 700 ] && [ "$owner" = "$(id -u)" ] || { echo "✗ Existing private development root has unsafe ownership or permissions: $path" >&2; exit 2; }
    else
        mkdir -m 700 "$path"
    fi
done
if [ "$ACTION" = setup ] || [ "$ACTION" = update ]; then
    ensure_private_socket_directory
fi
if [ -e "$DEV_ROOT/current" ] || [ -L "$DEV_ROOT/current" ]; then
    [ -f "$ENV_FILE" ] && [ -f "$MARKER" ] || {
        echo "✗ A current release without its marked environment is not fresh development state." >&2
        exit 1
    }
    dev_current_release >/dev/null || {
        echo "✗ Current development release pointer is not a complete managed release." >&2
        exit 1
    }
fi
[ ! -L "$COORDINATION_ROOT" ] || { echo "✗ Shared node-coordination root cannot be a symlink." >&2; exit 2; }
if [ -e "$COORDINATION_ROOT" ]; then
    [ -d "$COORDINATION_ROOT" ] || { echo "✗ Shared node-coordination path is not a directory." >&2; exit 2; }
    case "$(uname -s)" in
        Darwin) coordination_mode="$(stat -f '%Lp' "$COORDINATION_ROOT")"; coordination_owner="$(stat -f '%u' "$COORDINATION_ROOT")" ;;
        Linux) coordination_mode="$(stat -c '%a' "$COORDINATION_ROOT")"; coordination_owner="$(stat -c '%u' "$COORDINATION_ROOT")" ;;
        *) echo "✗ Only Linux and macOS development are supported." >&2; exit 2 ;;
    esac
    [ "$coordination_mode" = 700 ] && [ "$coordination_owner" = "$(id -u)" ] || {
        echo "✗ Shared node-coordination root has unsafe ownership or permissions." >&2
        exit 2
    }
else
    mkdir -p "$COORDINATION_ROOT"
    chmod 700 "$COORDINATION_ROOT"
    COORDINATION_ROOT="$(cd "$COORDINATION_ROOT" && pwd -P)"
fi
if [ -e "$MARKER" ] || [ -L "$MARKER" ]; then
    dev_root_marker_matches || {
        echo "✗ Development root binding is missing or inconsistent; existing state was retained." >&2
        exit 1
    }
fi
trap cleanup_dev_setup EXIT
version="$(sed -n 's/^version = "\([0-9.]*\)"$/\1/p' "$REPO_DIR/herdr-plugin.toml")"
revision="$(git -C "$REPO_DIR" rev-parse HEAD)"
[ -n "$version" ] && [ "${#revision}" -eq 40 ] || { echo "✗ Cannot determine coherent development build identity." >&2; exit 1; }
BUILD_DIR="$(mktemp -d "$DEV_ROOT/.build.XXXXXX")"
echo "Preparing frontend dependencies from frontend/bun.lock..."
NODE_ENV=development bun install --cwd "$REPO_DIR/frontend" --frozen-lockfile || {
    dependency_status=$?
    echo "✗ Frontend dependency installation failed; no new development release was selected. Fix the install error above and retry." >&2
    exit "$dependency_status"
}
bun run --cwd "$REPO_DIR/frontend" build
cp -R "$REPO_DIR/frontend/dist" "$BUILD_DIR/web"
bun "$REPO_DIR/scripts/stamp-web-version.mjs" "$BUILD_DIR/web/version.json" "$version" "$revision"
bun "$REPO_DIR/frontend/scripts/validate-build.mjs" "$BUILD_DIR/web"
CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.revision=$revision" \
    -o "$BUILD_DIR/herdr-mobile-relay" "$REPO_DIR/cmd/herdr-mobile-relay"
[ -x "$BUILD_DIR/herdr-mobile-relay" ] && [ -f "$BUILD_DIR/web/version.json" ] &&
    [ -d "$BUILD_DIR/web" ] && [ ! -L "$BUILD_DIR/web" ] || {
    echo "✗ Staged development release is incomplete; prior release remains active." >&2; exit 1;
}
mkdir -m 700 "$BUILD_DIR/bin"
mv "$BUILD_DIR/herdr-mobile-relay" "$BUILD_DIR/bin/herdr-mobile-relay"
release_id="$(generate_instance_id)"
release_name="${version}-${revision}-${release_id}"
RELEASE_DIR="$DEV_ROOT/releases/$release_name"
[ ! -e "$RELEASE_DIR" ] && [ ! -L "$RELEASE_DIR" ] || { echo "✗ Staged release identifier already exists." >&2; exit 1; }
mv "$BUILD_DIR" "$RELEASE_DIR"
BUILD_DIR=""

SETUP_STAGE="$(mktemp -d "$DEV_ROOT/.setup-stage.XXXXXX")"
printf '%s\n' "releases/$release_name" > "$SETUP_STAGE/candidate-pointer"
if [ -L "$DEV_ROOT/current" ]; then readlink "$DEV_ROOT/current" > "$SETUP_STAGE/old-pointer"; fi
if [ -f "$ENV_FILE" ]; then cp -p "$ENV_FILE" "$SETUP_STAGE/old-env"; fi
if [ -f "$MARKER" ]; then cp -p "$MARKER" "$SETUP_STAGE/old-marker"; fi
old_pointer_present=0 old_env_present=0 old_marker_present=0
[ ! -f "$SETUP_STAGE/old-pointer" ] || old_pointer_present=1
[ ! -f "$SETUP_STAGE/old-env" ] || old_env_present=1
[ ! -f "$SETUP_STAGE/old-marker" ] || old_marker_present=1
printf 'pointer=%s\nenv=%s\nmarker=%s\n' "$old_pointer_present" "$old_env_present" "$old_marker_present" > "$SETUP_STAGE/old-existence"
if [ ! -f "$ENV_FILE" ]; then
    token="$(generate_token)"
    instance="$(generate_instance_id)"
    control_run="$(generate_instance_id)"
    [ "${#token}" -eq 32 ] && [ -n "$instance" ] && [ -n "$control_run" ] || { echo "✗ Could not prepare private development identity." >&2; exit 2; }
    printf '%s\n' 'HERDR_DEV_TAILSCALE_CLI_ROOT=1' \
        "HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=$DEV_ROOT/registration" \
        "HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=$COORDINATION_ROOT" > "$SETUP_STAGE/candidate-marker"
    initializing_env="$SETUP_STAGE/candidate-env"
    : > "$initializing_env"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_TRANSPORT tailscale-cli
    set_env_value_atomic "$initializing_env" HERDR_RELAY_TOKEN "$token"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_INSTANCE_ID "$instance"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_CONTROL_RUN_ID "$control_run"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_HOST 127.0.0.1
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PORT "$RELAY_PORT"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PLUGIN_PORT "$PLUGIN_PORT"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PAIRING_SOCKET "$PAIRING_SOCKET"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_ORIGIN "$ORIGIN"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_SCOPE development
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_BIN "$CLI_BIN"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_STATE_ROOT "$DEV_ROOT/registration"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_COORDINATION_ROOT "$COORDINATION_ROOT"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT "$DEV_ROOT"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_HTTPS_PORT "$HTTPS_PORT"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_NODE_ID "$NODE_ID"
    set_env_value_atomic "$initializing_env" HERDR_PHONE_APP_URL "$PHONE_APP"
    set_env_value_atomic "$initializing_env" HERDR_BIN "$HERDR_DEV_HERDR_BIN"
    set_env_value_atomic "$initializing_env" HERDR_SOCKET_PATH "$HERDR_DEV_HERDR_SOCKET"
    set_env_value_atomic "$initializing_env" HERDR_REACHABILITY_PORT_MAPPING 0
    set_env_value_atomic "$initializing_env" HERDR_RELAY_REARM_BOOTSTRAP 0
    set_env_value_atomic "$initializing_env" HERDR_RELAY_POLL_INTERVAL 2
else
    cp -p "$ENV_FILE" "$SETUP_STAGE/candidate-env"
    cp -p "$MARKER" "$SETUP_STAGE/candidate-marker"
    if [ "$MIGRATE_LEGACY_SOCKET" = 1 ]; then
        set_env_value_atomic "$SETUP_STAGE/candidate-env" HERDR_RELAY_PAIRING_SOCKET "$PAIRING_SOCKET"
    fi
    [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_TRANSPORT)" = tailscale-cli ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_SCOPE)" = development ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)" = "$RELAY_PORT" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_PLUGIN_PORT)" = "$PLUGIN_PORT" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_HTTPS_PORT)" = "$HTTPS_PORT" ] || {
        echo "✗ Existing CLI development identity, scope or ports changed; state was retained." >&2
        exit 1
    }
    [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_ORIGIN)" = "$ORIGIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT)" = "$DEV_ROOT" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_BIN)" = "$CLI_BIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_NODE_ID)" = "$NODE_ID" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_BIN)" = "$HERDR_DEV_HERDR_BIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_SOCKET_PATH)" = "$HERDR_DEV_HERDR_SOCKET" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_PHONE_APP_URL)" = "$PHONE_APP" ] || {
        echo "✗ Existing CLI profile/origin changed; registration requires explicit recovery." >&2
        exit 1
    }
fi

if [ ! -f "$SETUP_STAGE/old-env" ] ||
    ! cmp -s "$SETUP_STAGE/old-env" "$SETUP_STAGE/candidate-env"; then
    cp -p "$SETUP_STAGE/candidate-env" "$SETUP_STAGE/publish-env"
fi
if [ ! -f "$SETUP_STAGE/old-marker" ]; then
    cp -p "$SETUP_STAGE/candidate-marker" "$SETUP_STAGE/publish-marker"
fi
# The pending journal becomes visible atomically before any live env, marker
# or pointer mutation. Interrupted attempts recover only exact snapshots.
[ ! -e "$PENDING" ] && [ ! -L "$PENDING" ] || { echo "✗ Existing setup journal requires recovery." >&2; exit 1; }
mv "$SETUP_STAGE" "$PENDING"
SETUP_STAGE=""
if ! { setup_snapshot_matches "$ENV_FILE" "$PENDING/old-env" &&
    setup_snapshot_matches "$MARKER" "$PENDING/old-marker" &&
    setup_pointer_matches "$DEV_ROOT/current" "$PENDING/old-pointer"; }; then
    echo "✗ Saved development state changed before cutover; journal retained if recovery is uncertain." >&2
    exit 1
fi
if [ -f "$PENDING/publish-marker" ]; then mv "$PENDING/publish-marker" "$MARKER"; fi
if [ -f "$PENDING/publish-env" ]; then mv "$PENDING/publish-env" "$ENV_FILE"; fi
if ! { setup_snapshot_matches "$ENV_FILE" "$PENDING/candidate-env" &&
    setup_snapshot_matches "$MARKER" "$PENDING/candidate-marker" &&
    setup_pointer_matches "$DEV_ROOT/current" "$PENDING/old-pointer"; }; then
    echo "✗ Candidate or saved pointer changed before cutover; no release was selected." >&2
    exit 1
fi
replace_dev_pointer "releases/$release_name"
load_relay_env "$ENV_FILE"
current_release="$(dev_current_release)" || { echo "✗ Atomic cutover did not select a complete development release." >&2; exit 1; }
[ "$current_release" = "$RELEASE_DIR" ] || { echo "✗ Current release pointer did not select the staged build." >&2; exit 1; }
RELAY_BIN="$DEV_ROOT/current/bin/herdr-mobile-relay"
GATE_RELAY_BIN="$RELAY_BIN"
export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$RELAY_BIN" HERDR_WEB_ROOT="$DEV_ROOT/current/web"
export HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
export HERDR_RELAY_HOST=127.0.0.1 HERDR_RELAY_TRANSPORT=tailscale-cli HERDR_RELAY_REARM_BOOTSTRAP=0 HERDR_REACHABILITY_PORT_MAPPING=0
unset HERDR_PLUGIN_CONFIG_DIR HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_ORIGIN HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID
unset GH_TOKEN CURL_CA_BUNDLE SSL_CERT_FILE NODE_EXTRA_CA_CERTS
export HERDR_BIN="$HERDR_DEV_HERDR_BIN" HERDR_SOCKET_PATH="$HERDR_DEV_HERDR_SOCKET"
export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN" HERDR_TAILSCALE_CLI_ORIGIN="$ORIGIN" HERDR_TAILSCALE_CLI_NODE_ID="$NODE_ID" HERDR_TAILSCALE_CLI_SCOPE=development
export HERDR_TAILSCALE_CLI_STATE_ROOT="$DEV_ROOT/registration" HERDR_TAILSCALE_CLI_COORDINATION_ROOT="$COORDINATION_ROOT"
export HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT="$DEV_ROOT"
export HERDR_TAILSCALE_CLI_HTTPS_PORT="$HTTPS_PORT" HERDR_PHONE_APP_URL="$PHONE_APP"
export HERDR_RELAY_PAIRING_SOCKET="$PAIRING_SOCKET"
export HERDR_RELAY_PORT="$RELAY_PORT" HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT"

PREFLIGHT="$("$RELAY_BIN" dev-tailscale-cli preflight)" || {
    echo "✗ Full Go-owned isolated development preflight failed; prior state is retained on proved rollback." >&2; exit 1;
}
LIVE_NODE_ID="$(json_string_field "$PREFLIGHT" node_id "$RELAY_BIN")"
LIVE_ORIGIN="$(json_string_field "$PREFLIGHT" origin "$RELAY_BIN")"
LIVE_DNS_NAME="$(json_string_field "$PREFLIGHT" dns_name "$RELAY_BIN")"
LIVE_PROFILE="$(json_string_field "$PREFLIGHT" profile "$RELAY_BIN")"
if [ -z "$LIVE_NODE_ID" ] || [ -z "$LIVE_ORIGIN" ] || [ -z "$LIVE_DNS_NAME" ] || [ -z "$LIVE_PROFILE" ] ||
    [ "$LIVE_NODE_ID" != "$NODE_ID" ] || [ "$LIVE_ORIGIN" != "$ORIGIN" ] ||
    { [ -n "$DNS_NAME" ] && [ "$LIVE_DNS_NAME" != "$DNS_NAME" ]; } ||
    { [ -n "$PROFILE" ] && [ "$LIVE_PROFILE" != "$PROFILE" ]; } ||
    ! setup_snapshot_matches "$ENV_FILE" "$PENDING/candidate-env" ||
    ! setup_snapshot_matches "$MARKER" "$PENDING/candidate-marker" ||
    ! setup_pointer_matches "$DEV_ROOT/current" "$PENDING/candidate-pointer" ||
    ! setup_pending_snapshots_valid; then
    echo "✗ Full preflight changed identity or candidate state; no route was started." >&2
    exit 1
fi
# The full validator has accepted the actual current release and canonical
# saved app. Rename the journal atomically BEFORE deleting snapshots: an
# interruption during deletion cannot leave an unrecoverable pending record.
COMPLETE_STAGE="$DEV_ROOT/.setup-complete.$$"
[ ! -e "$COMPLETE_STAGE" ] && [ ! -L "$COMPLETE_STAGE" ] || exit 1
mv "$PENDING" "$COMPLETE_STAGE"
cleanup_complete_setup || { echo "✗ Complete private setup snapshots need inspection; route not started." >&2; exit 1; }
COMPLETE_STAGE=""
if [ "$MIGRATE_LEGACY_SOCKET" = 1 ]; then
    echo "▸ Moved this root's stopped pairing-control socket to the short private socket directory."
fi
trap cleanup_dev_build_stage EXIT
exec "$RELAY_BIN" dev-tailscale-cli foreground --action "$ACTION"

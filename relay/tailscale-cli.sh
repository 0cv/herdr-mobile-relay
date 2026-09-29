#!/bin/bash
# CLI-backed persistent Serve lifecycle. Every route command is delegated to the
# Go journal manager; this wrapper never parses Serve JSON or invokes Tailscale.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ACTION="${1:-setup}"
[ "$#" -eq 0 ] || shift
ENV_FILE="$(relay_env_file_read_only "$SCRIPT_DIR")"
load_relay_env "$ENV_FILE"
RELAY_BIN="$(relay_binary)"
SCOPE="${HERDR_TAILSCALE_CLI_SCOPE:-production}"
INSTALLATION_ID="${HERDR_RELAY_INSTANCE_ID:-}"
STATE_ROOT="${HERDR_TAILSCALE_CLI_STATE_ROOT:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay/tailscale-cli-registration}"
COORDINATION_ROOT="${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay/tailscale-cli-coordination}"
HTTPS_PORT="${HERDR_TAILSCALE_CLI_HTTPS_PORT:-443}"
BACKEND_PORT="${HERDR_RELAY_PORT:-8375}"
CLI_BIN="${HERDR_TAILSCALE_CLI_BIN:-}"
NODE_ID="${HERDR_TAILSCALE_CLI_NODE_ID:-}"

usage() {
    cat >&2 <<'EOF'
Usage: relay/tailscale-cli.sh {setup|status|recover|stop|unpublish|update|uninstall}

setup requires an explicit tailscale-cli relay.env selection, absolute selected
CLI path, canonical HTTPS origin, private registration roots, and operator-bound
node/port consent. Stop retains the persistent Serve route. Unpublish is a
separate consented operation. Unresolved records are never erased automatically.
EOF
}

check_action() {
    case "$SCOPE" in production|development) ;; *) echo "✗ Invalid CLI Serve scope." >&2; return 1 ;; esac
    [ -n "$INSTALLATION_ID" ] || { echo "✗ HERDR_RELAY_INSTANCE_ID is required." >&2; return 1; }
    case "$HTTPS_PORT" in ''|*[!0-9]*) echo "✗ HERDR_TAILSCALE_CLI_HTTPS_PORT must be numeric." >&2; return 1 ;; esac
    case "$BACKEND_PORT" in ''|*[!0-9]*) echo "✗ HERDR_RELAY_PORT must be numeric." >&2; return 1 ;; esac
    [ "$HTTPS_PORT" -ge 1 ] && [ "$HTTPS_PORT" -le 65535 ] &&
        [ "$BACKEND_PORT" -ge 1 ] && [ "$BACKEND_PORT" -le 65535 ] &&
        [ "$HTTPS_PORT" != "$BACKEND_PORT" ] || {
        echo "✗ CLI HTTPS and relay backend ports must be distinct valid ports." >&2
        return 1
    }
    case "$CLI_BIN" in
        /*) [ -x "$CLI_BIN" ] && [ ! -d "$CLI_BIN" ] || { echo "✗ Selected Tailscale CLI is not executable." >&2; return 1; } ;;
        *) echo "✗ HERDR_TAILSCALE_CLI_BIN must be the selected absolute executable path." >&2; return 1 ;;
    esac
}

ensure_private_root() {
    local path="$1"
    local mode owner
    case "$path" in /*) ;; *) echo "✗ Registration roots must be absolute." >&2; return 1 ;; esac
    [ ! -L "$path" ] || { echo "✗ Registration root cannot be a symlink: $path" >&2; return 1; }
    if [ ! -e "$path" ]; then
        mkdir -p "$path"
        chmod 700 "$path"
    fi
    [ -d "$path" ] && [ ! -L "$path" ] || { echo "✗ Registration root is not a real directory: $path" >&2; return 1; }
    case "$(uname -s)" in
        Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
        Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
        *) echo "✗ CLI-backed Serve supports only macOS and Linux user services." >&2; return 1 ;;
    esac
    [ "$mode" = 700 ] && [ "$owner" = "$(id -u)" ] || {
        echo "✗ Registration roots must be owned by this user with mode 0700: $path" >&2
        return 1
    }
}

activation_check() {
    "$RELAY_BIN" tailscale-cli activation-check
}

manager_call() {
    local operation="$1"
    shift
    "$RELAY_BIN" tailscale-cli "$operation" \
        --binary "$CLI_BIN" --state-root "$STATE_ROOT" --coordination-root "$COORDINATION_ROOT" \
        --scope "$SCOPE" --installation-id "$INSTALLATION_ID" \
        --https-port "$HTTPS_PORT" --backend-port "$BACKEND_PORT" "$@"
}

prompt_yes() {
    local message="$1" answer
    [ -t 0 ] || { echo "✗ $message requires an interactive terminal." >&2; return 1; }
    read -r -p "$message [y/N] " answer || return 1
    case "$answer" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

request_node_id() {
    if [ -n "$NODE_ID" ]; then return 0; fi
    [ -t 0 ] || { echo "✗ Set HERDR_TAILSCALE_CLI_NODE_ID to the explicitly approved node ID." >&2; return 1; }
    read -r -p "Exact node ID approved for this route: " NODE_ID || return 1
    [ -n "$NODE_ID" ] || { echo "✗ A node ID is required." >&2; return 1; }
}

explicit_unpublish() {
    request_node_id || return 1
    echo "This removes only the journaled HTTPS $HTTPS_PORT / route on node $NODE_ID."
    echo "It does not reset Serve, remove other routes, or guarantee remote connection drain."
    prompt_yes "Authorize this exact route removal and accept the no-remote-drain limit?" || {
        echo "Cancelled; registration and route were left unchanged."
        return 1
    }
    manager_call unpublish --accepted --node-id "$NODE_ID" \
        --accept-route-removal --accept-no-remote-drain
}

setup_cli() {
    [ -f "$ENV_FILE" ] || { echo "✗ Relay config is missing: $ENV_FILE" >&2; return 1; }
    [ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
        echo "✗ Select tailscale-cli explicitly in relay.env before setup; no transport is migrated implicitly." >&2
        return 1
    }
    check_action
    case "${HERDR_TAILSCALE_CLI_ORIGIN:-}" in https://*) ;; *) echo "✗ Set a canonical HERDR_TAILSCALE_CLI_ORIGIN first." >&2; return 1 ;; esac
    [ -n "${HERDR_PHONE_APP_URL:-}" ] || { echo "✗ Set the exact verified HERDR_PHONE_APP_URL first." >&2; return 1; }
    request_node_id
    echo "Persistent HTTPS route: $HERDR_TAILSCALE_CLI_ORIGIN -> 127.0.0.1:$BACKEND_PORT"
    echo "Scope: $SCOPE; node: $NODE_ID; HTTPS port: $HTTPS_PORT"
    echo "The route persists after relay stop; a local process may later reuse its backend port."
    echo "No global rollback or remote-drain guarantee is provided."
    prompt_yes "Publish this exact route and accept every documented risk?" || {
        echo "Cancelled; no route or service was changed."
        return 1
    }
    # The compile-time P6 gate is checked before roots, services or CLI access.
    activation_check
    ensure_private_root "$STATE_ROOT"
    ensure_private_root "$COORDINATION_ROOT"
    set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_CLI_NODE_ID "$NODE_ID"
    set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_CLI_STATE_ROOT "$STATE_ROOT"
    set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_CLI_COORDINATION_ROOT "$COORDINATION_ROOT"
    set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_CLI_HTTPS_PORT "$HTTPS_PORT"
    manager_call publish --node-id "$NODE_ID" --accepted \
        --accept-persistent-route --accept-check-to-write-race --accept-port-reuse \
        --accept-no-rollback --accept-no-remote-drain
    echo "▸ Installing the per-user relay service; route publication is not repeated on restart."
    "$SCRIPT_DIR/service.sh" install || {
        echo "✗ Service setup failed after route publication; registration was retained for recovery." >&2
        echo "  Run: relay/tailscale-cli.sh recover" >&2
        return 1
    }
    local socket="${HERDR_RELAY_PAIRING_SOCKET:-}"
    local run_id="${HERDR_RELAY_CONTROL_RUN_ID:-}"
    local armed
    [ -S "$socket" ] && [ -n "$run_id" ] || {
        echo "✗ Service is installed, but its private pairing-control endpoint is unavailable; route was retained." >&2
        return 1
    }
    armed="$("$RELAY_BIN" pairing-control --socket "$socket" --operation arm_bootstrap \
        --run-id "$run_id" --instance "$INSTALLATION_ID")" || {
        echo "✗ Invitation arming failed after route publication; registration was retained for recovery." >&2
        return 1
    }
    [ "$(json_bool_field "$armed" ready)" = true ] &&
        [ "$(json_bool_field "$armed" persistent_route_ready)" = true ] &&
        [ "$(json_bool_field "$armed" invitation_armed)" = true ] || {
        echo "✗ Service did not confirm qualified route and durable invitation admission; route was retained." >&2
        return 1
    }
    echo "✓ CLI-backed service and invitation are ready."
    "$SCRIPT_DIR/setup-link.sh"
}

case "$ACTION" in
    setup)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        setup_cli
        ;;
    status)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        "$SCRIPT_DIR/service.sh" status || true
        if activation_check; then
            check_action
            manager_call status
        else
            echo "CLI route status is unavailable while candidate profiles are disabled; no CLI was contacted." >&2
            exit 2
        fi
        ;;
    recover)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        manager_call recover
        ;;
    stop)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        "$SCRIPT_DIR/service.sh" stop
        echo "Relay service stopped; persistent Serve route and registration were retained."
        ;;
    unpublish)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        explicit_unpublish
        ;;
    update)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        [ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] || {
            echo "✗ Operator-managed CLI update requires an explicitly selected tailscale-cli transport." >&2
            exit 1
        }
        manager_call assert-ready >/dev/null
        exec "$SCRIPT_DIR/plugin-build.sh" --operator-managed-tailscale-cli
        ;;
    uninstall)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        echo "Choose whether to remove the exact route before uninstalling."
        if prompt_yes "Remove the journaled persistent Serve route first?"; then
            explicit_unpublish
        else
            prompt_yes "Leave the route configured and preserve its registration journal?" || exit 1
        fi
        exec "$SCRIPT_DIR/uninstall.sh"
        ;;
    *) usage; exit 2 ;;
esac

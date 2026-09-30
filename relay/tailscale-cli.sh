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
COORDINATION_ROOT="${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination}"
DEVELOPMENT_ROOT="${HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT:-}"
HTTPS_PORT="${HERDR_TAILSCALE_CLI_HTTPS_PORT:-443}"
BACKEND_PORT="${HERDR_RELAY_PORT:-8375}"
CLI_BIN="${HERDR_TAILSCALE_CLI_BIN:-}"
NODE_ID="${HERDR_TAILSCALE_CLI_NODE_ID:-}"
ORIGIN="${HERDR_TAILSCALE_CLI_ORIGIN:-}"

usage() {
    cat >&2 <<'EOF'
Usage: relay/tailscale-cli.sh {setup|status|recover|release-reservation|reconcile|arm-bootstrap|stop|unpublish|update|uninstall}

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
        "") ;;
        /*) [ -x "$CLI_BIN" ] && [ ! -d "$CLI_BIN" ] || { echo "✗ Selected Tailscale CLI is not executable." >&2; return 1; } ;;
        *) echo "✗ Explicit HERDR_TAILSCALE_CLI_BIN override must be absolute." >&2; return 1 ;;
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
    local development_root_args=()
    if [ "$SCOPE" = development ]; then
        case "$DEVELOPMENT_ROOT" in
            /*) development_root_args=(--development-root "$DEVELOPMENT_ROOT") ;;
            *) echo "✗ Development-scope CLI operations require the launcher-bound private root." >&2; return 1 ;;
        esac
    fi
    "$RELAY_BIN" tailscale-cli "$operation" "${development_root_args[@]}" \
        --binary "$CLI_BIN" --state-root "$STATE_ROOT" --coordination-root "$COORDINATION_ROOT" \
        --scope "$SCOPE" --installation-id "$INSTALLATION_ID" --origin "$ORIGIN" \
        --https-port "$HTTPS_PORT" --backend-port "$BACKEND_PORT" "$@"
}

CLI_RESERVATION_CLAIMED=false
CLI_RESERVATION_ID=""
CLI_PUBLISH_ATTEMPTED=false
CLI_SERVICE_INSTALL_ATTEMPTED=false
cleanup_cli_backend_reservation() {
    [ "$CLI_RESERVATION_CLAIMED" = true ] && [ "$CLI_PUBLISH_ATTEMPTED" != true ] || return 0
    if [ "$CLI_SERVICE_INSTALL_ATTEMPTED" = true ]; then
        if installed_relay_service_definition_present; then
            HERDR_CLI_SETUP_ROLLBACK=1 "$SCRIPT_DIR/service.sh" rollback-cli-setup >/dev/null 2>&1 || {
                "$SCRIPT_DIR/service.sh" stop >/dev/null 2>&1 || true
                echo "⚠ Newly installed service could not be rolled back; it may restart later, so its backend reservation is retained." >&2
                echo "  Inspect and disable the service before attempting explicit reservation recovery." >&2
                return 0
            }
            echo "Removed the newly installed service definition; persistent Serve state was not changed."
        else
            "$SCRIPT_DIR/service.sh" stop >/dev/null 2>&1 || {
                echo "⚠ Backend reservation retained because the service could not be stopped safely." >&2
                return 0
            }
        fi
        CLI_SERVICE_INSTALL_ATTEMPTED=false
    fi
    if manager_call release-backend-port --node-id "$NODE_ID" --reservation-id "$CLI_RESERVATION_ID" --service-stopped >/dev/null 2>&1; then
        CLI_RESERVATION_CLAIMED=false
    else
        echo "⚠ Backend reservation retained; explicit read-only inspection/release is required." >&2
    fi
}

arm_cli_bootstrap() {
    "$RELAY_BIN" pairing-control --socket "${HERDR_RELAY_PAIRING_SOCKET:-}" \
        --operation arm_bootstrap --run-id "${HERDR_RELAY_CONTROL_RUN_ID:-}" \
        --instance "$INSTALLATION_ID" >/dev/null || {
        echo "✗ Operator-requested bootstrap invitation could not be armed." >&2
        return 1
    }
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

release_pending_reservation() {
    activation_check
    check_action
    [ -t 0 ] || { echo "✗ Backend reservation recovery requires an interactive terminal." >&2; return 2; }
    local report reservation_id reservation_state releasable answer
    if report="$(manager_call recover)"; then
        :
    else
        [ -n "$report" ] || { echo "✗ Recovery report is unavailable; reservation was not changed." >&2; return 1; }
    fi
    reservation_id="$(json_string_field "$report" reservation_attempt_id)"
    reservation_state="$(json_string_field "$report" reservation_state)"
    releasable="$(json_bool_field "$report" reservation_releasable)"
    [ -n "$reservation_id" ] && [ "$reservation_state" = publish-pending ] && [ "$releasable" = true ] || {
        echo "✗ No exact pending setup reservation is available for release." >&2
        printf '%s\n' "$report"
        return 1
    }
    printf '%s\n' "$report"
    "$SCRIPT_DIR/service.sh" assert-stopped || {
        echo "✗ Stop the installed relay service and verify its backend listener is gone before releasing this claim." >&2
        return 1
    }
    read -r -p "Type RELEASE BACKEND RESERVATION $reservation_id to release only this stopped setup attempt: " answer || return 1
    [ "$answer" = "RELEASE BACKEND RESERVATION $reservation_id" ] || {
        echo "Cancelled; backend reservation was retained."
        return 1
    }
    manager_call release-backend-port --node-id "$NODE_ID" --reservation-id "$reservation_id" --service-stopped
}

reconcile_pending() {
    activation_check
    check_action
    [ -t 0 ] || { echo "✗ Journal reconciliation requires an interactive terminal." >&2; return 2; }
    local report operation_id observation answer
    if report="$(manager_call recover)"; then
        :
    else
        [ -n "$report" ] || { echo "✗ Recovery report is unavailable; journal was not changed." >&2; return 1; }
    fi
    operation_id="$(json_string_field "$report" recovery_operation_id)"
    observation="$(json_string_field "$report" observation)"
    [ -n "$operation_id" ] || { echo "✗ No exact pending operation is available for reconciliation." >&2; return 1; }
    case "$observation" in
        exact-registered-route-present) observation=present ;;
        selected-listener-absent) observation=absent ;;
        *) echo "✗ Observed route does not match a reconcilable exact pending operation." >&2; return 1 ;;
    esac
    printf '%s\n' "$report"
    echo "This changes only the local journal after a fresh exact-operation and route-state check."
    read -r -p "Type RECONCILE $operation_id $observation to authorize: " answer || return 1
    [ "$answer" = "RECONCILE $operation_id $observation" ] || {
        echo "Cancelled; journal and route are unchanged."
        return 1
    }
    manager_call reconcile --operation-id "$operation_id" --confirm-observed-route "$observation" \
        --accepted --accept-journal-reconciliation --node-id "$NODE_ID"
}

explicit_unpublish() {
    request_node_id || return 1
    echo "This removes only the journaled HTTPS $HTTPS_PORT / route on node $NODE_ID."
    echo "The CLI cannot atomically protect the check-to-write interval; avoid concurrent Serve edits."
    echo "It does not reset Serve, remove other routes, or guarantee remote connection drain."
    prompt_yes "Authorize this exact route removal and accept the check-to-write race and no-remote-drain limits?" || {
        echo "Cancelled; registration and route were left unchanged."
        return 1
    }
    manager_call unpublish --accepted --node-id "$NODE_ID" \
        --accept-route-removal --accept-check-to-write-race --accept-no-remote-drain
}

setup_cli() {
    activation_check
    echo "✗ Installed-service CLI setup is disabled; real CLI access is limited to the isolated foreground development workflow." >&2
    echo "  No Tailscale CLI, service manager, or relay server was contacted." >&2
    return 1
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
    release-reservation)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        release_pending_reservation
        ;;
    reconcile)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        reconcile_pending
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
    arm-bootstrap)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        prompt_yes "Create a new one-use bootstrap invitation for this exact registered route?" || exit 1
        manager_call assert-ready >/dev/null
        "$RELAY_BIN" pairing-control --socket "${HERDR_RELAY_PAIRING_SOCKET:-}" \
            --operation admit --run-id "${HERDR_RELAY_CONTROL_RUN_ID:-}" --instance "$INSTALLATION_ID" >/dev/null || {
            echo "✗ The exact route is not ready for admission." >&2
            exit 1
        }
        arm_cli_bootstrap
        wait_for_cli_admission "$RELAY_BIN" "${HERDR_RELAY_PAIRING_SOCKET:-}" \
            "${HERDR_RELAY_CONTROL_RUN_ID:-}" "$INSTALLATION_ID" || exit 1
        exec "$SCRIPT_DIR/setup-link.sh"
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
        exec "$SCRIPT_DIR/uninstall.sh"
        ;;
    *) usage; exit 2 ;;
esac

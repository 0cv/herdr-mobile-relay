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
Usage: relay/tailscale-cli.sh {setup|status|arm-status|reprint|recover|release-reservation|reconcile|arm-bootstrap|stop|unpublish|remove-missing|update|uninstall}

setup requires an explicit tailscale-cli relay.env selection, absolute selected
CLI path, canonical HTTPS origin, private registration roots, and operator-bound
node/port consent. Stop retains the persistent Serve route. Unpublish is a
separate consented operation. Remove-missing is the explicit, operation-bound
exit for a proven absent route; it never unpublishes a present route.
Unresolved records are never erased automatically.
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

selected_installed_cli() {
    [ "$(relay_transport_mode "$ENV_FILE")" = tailscale-cli ] &&
        [ "$SCOPE" = production ] &&
        assert_service_env_matches "$ENV_FILE" || {
        echo '✗ The installed CLI transport and service configuration must be selected explicitly.' >&2
        return 1
    }
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

installed_control_run() {
    local status
    status="$("$RELAY_BIN" installed-control status)" || return 1
    [ "$(json_string_field "$status" code)" = ok ] &&
        [ "$(json_string_field "$status" publication)" = disabled ] &&
        [ "$(json_string_field "$status" installation)" = "$INSTALLATION_ID" ] || return 1
    CONTROL_RUN="$(json_string_field "$status" run)"
    case "$CONTROL_RUN" in
        ????????????????????????????????) ;; *) return 1 ;;
    esac
}

wait_installed_control() {
    local deadline=$((SECONDS + 30))
    # Discovery and status are read-only; the selected installation, run and
    # disabled-publication status must be observed before the single arm send.
    while [ "$SECONDS" -lt "$deadline" ]; do
        if installed_control_run; then return 0; fi
        sleep 1
    done
    echo '✗ Installed control did not become ready for this selected service. The service and route were retained; inspect tailscale-cli.sh status and recover, then use reprint when ready. Do not select Always on again or guess a new operation ID.' >&2
    return 1
}

arm_cli_bootstrap() {
    local operation_id reply invitation
    wait_installed_control || return 1
    read -r -p 'Stable operation ID for this one-use invitation (save it for status recovery): ' operation_id || return 1
    [ -n "$operation_id" ] || return 1
    # This authenticated operation performs its own fresh local/route/HTTPS and
    # exact packaged-app admission; assert-ready alone cannot create a link.
    reply="$("$RELAY_BIN" installed-control arm --run "$CONTROL_RUN" --operation-id "$operation_id")" || {
        echo '✗ Invitation unavailable or outcome uncertain. Inspect status with the SAME operation ID; never retry with a new ID.' >&2
        return 1
    }
    [ "$(json_string_field "$reply" code)" = ok ] &&
        [ "$(json_string_field "$reply" arm_receipt)" = acknowledged ] || return 1
    invitation="$(json_string_field "$reply" invitation)"
    [ -n "$invitation" ] || return 1
    printf '%s\n' "$invitation"
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
    request_node_id || return 1
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
    local report operation_id observation reservation_id answer
    request_node_id || return 1
    if report="$(manager_call recover)"; then
        :
    else
        [ -n "$report" ] || { echo "✗ Recovery report is unavailable; journal was not changed." >&2; return 1; }
    fi
    operation_id="$(json_string_field "$report" recovery_operation_id)"
    reservation_id="$(json_string_field "$report" reservation_attempt_id)"
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
    "$SCRIPT_DIR/service.sh" assert-stopped || {
        echo '✗ Service quiescence is required before journal reconciliation.' >&2
        return 1
    }
    manager_call reconcile --operation-id "$operation_id" --confirm-observed-route "$observation" \
        --reservation-id "$reservation_id" --accepted --accept-journal-reconciliation --node-id "$NODE_ID"
}

explicit_remove_missing() {
    [ -t 0 ] || { echo '✗ Absence removal requires an interactive terminal.' >&2; return 2; }
    request_node_id || return 1
    local report operation_id reservation_id observation state answer
    report="$(manager_call recover)" || [ -n "$report" ] || return 1
    operation_id="$(json_string_field "$report" recovery_operation_id)"
    reservation_id="$(json_string_field "$report" reservation_attempt_id)"
    observation="$(json_string_field "$report" observation)"
    state="$(json_string_field "$report" journal_state)"
    [ -n "$operation_id" ] && { [ "$state" = registered ] || [ "$state" = reconciled-absent ]; } &&
        [ "$observation" = selected-listener-absent ] || {
        echo '✗ No exact acknowledged missing route is available for owned removal.' >&2
        return 1
    }
    printf '%s\n' "$report"
    echo 'This operation stops only the pinned service, proves exact route absence, releases its reservation and retires its owned definition/claim.'
    read -r -p "Type REMOVE MISSING $NODE_ID $ORIGIN $HTTPS_PORT $BACKEND_PORT $operation_id $reservation_id to authorize: " answer || return 1
    [ "$answer" = "REMOVE MISSING $NODE_ID $ORIGIN $HTTPS_PORT $BACKEND_PORT $operation_id $reservation_id" ] || return 1
    "$RELAY_BIN" remove-missing-installed "$NODE_ID" "$operation_id" "$reservation_id" I-ACCEPT-ABSENCE-REMOVAL
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
    # Only the pinned coordinator may quiesce the owned definition and remove
    # the journaled route. A missing/foreign claim is not fallback authority to
    # stop or disable a service by its name.
    [ -d "$HOME/.local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1" ] || {
        echo '✗ No owned promoted service claim; route and service were left unchanged.' >&2
        return 1
    }
    "$RELAY_BIN" remove-installed "$NODE_ID"
}

setup_cli() {
    activation_check
    check_action
    selected_installed_cli || return 1
    [ -t 0 ] && [ -t 1 ] || {
        echo '✗ Installed CLI setup requires an interactive operator.' >&2
        return 1
    }
    # This child is the sole temporary setup owner. The separate wizard is an
    # authenticated installed-control client, not an ambient pairing-control
    # run. Always cancel and join the child on refusal/EOF/interrupt.
    "$RELAY_BIN" promote-installed </dev/null &
    local promotion_pid=$! joined=false
    cleanup_setup_child() {
        if [ "$joined" != true ]; then
            kill -TERM "$promotion_pid" 2>/dev/null || true
            wait "$promotion_pid" 2>/dev/null || true
        fi
    }
    trap cleanup_setup_child EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    "$RELAY_BIN" installed-control-wizard || return 1
    local result=0
    wait "$promotion_pid" || result=$?
    joined=true
    trap - EXIT INT TERM
    [ "$result" -eq 0 ] || {
        echo '✗ Setup publication or fresh service activation was not verified; retain recovery evidence.' >&2
        return 1
    }
    echo 'Fresh installed service activation attempted. Installed control must become ready before arming an invitation.'
}

case "$ACTION" in
    setup)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        setup_cli
        ;;
    status)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        check_action
        # The existing diagnostic reports selection, packaged/local evidence and
        # unknown Herdr/phone checks; it does not grant CLI query authority.
        HERDR_SETUP_MENU=1 "$SCRIPT_DIR/plugin-status.sh" --concise --config "$ENV_FILE" || :
        activation_check || {
            echo 'Installed CLI profile/route/service readiness unavailable; no CLI or service manager was contacted.' >&2
            exit 3
        }
        route_status=0
        route="$(manager_call status)" || route_status=$?
        control_status=unavailable
        service_status=unavailable
        # A failed route/profile selection is not authority for a second
        # control or native-manager query. Recover can inspect retained state.
        if [ "$route_status" -eq 0 ]; then
            if installed_control_run; then control_status=observed; fi
            if [ "$control_status" = observed ] && "$SCRIPT_DIR/service.sh" status >/dev/null 2>&1; then
                service_status=manager_responded
            fi
        fi
        journal="$(json_string_field "$route" journal_state)"
        readiness="$(json_string_field "$route" readiness)"
        profile="$(json_string_field "$route" profile)"
        qualified="$(json_bool_field "$route" runtime_qualified 2>/dev/null || true)"
        case "$journal" in unconfigured|publish-pending|registered|publish-uncertain|remove-pending|remove-uncertain|removed|reconciled-present|reconciled-absent) ;; *) journal=unknown ;; esac
        case "$readiness" in ready|waiting|degraded|conflicted|uncertain|unqualified) ;; *) readiness=unknown ;; esac
        case "$profile" in macos-appstore-1.102.4-supplied-metadata|linux-amd64-1.102.4-source-candidate) ;; *) profile=unknown ;; esac
        case "$qualified" in true|false) ;; *) qualified=unknown ;; esac
        printf 'installed_service=%s installed_control=%s cli_profile=%s runtime_qualified=%s registration=%s route_readiness=%s phone=unknown\n' \
            "$service_status" "$control_status" "$profile" "$qualified" "$journal" "$readiness"
        # Manager response is not verified service liveness or Herdr/phone
        # reachability. Keep the composite decision incomplete even when the
        # selected registered route itself is ready.
        exit 3
        ;;
    arm-status)
        [ "$#" -eq 1 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check
        check_action
        installed_control_run || exit 1
        "$RELAY_BIN" installed-control status --run "$CONTROL_RUN" --operation-id "$1"
        ;;
    reprint)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check
        check_action
        installed_control_run || exit 1
        # The owned reprint itself repeats registered-route, trusted HTTPS,
        # exact app-bundle and selected-identity admission before releasing a link.
        reply="$("$RELAY_BIN" installed-control reprint --run "$CONTROL_RUN")" || exit 1
        [ "$(json_string_field "$reply" code)" = ok ] || exit 1
        link="$(json_string_field "$reply" invitation)"
        [ -n "$link" ] || exit 1
        printf '%s\n' "$link"
        ;;
    recover)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check
        check_action
        manager_call recover
        ;;
    release-reservation)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        release_pending_reservation
        ;;
    reconcile)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        reconcile_pending
        ;;
    stop)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check || exit 1
        "$RELAY_BIN" stop-installed
        echo "Owned relay service stopped; persistent Serve route and registration were retained."
        ;;
    unpublish)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        selected_installed_cli || exit 1
        explicit_unpublish
        ;;
    remove-missing)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        activation_check
        check_action
        selected_installed_cli || exit 1
        explicit_remove_missing
        ;;
    arm-bootstrap)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check
        check_action
        prompt_yes "Create a new one-use bootstrap invitation for this exact registered route?" || exit 1
        manager_call assert-ready >/dev/null
        arm_cli_bootstrap
        ;;
    update)
        [ "$#" -le 1 ] || { usage; exit 2; }
        activation_check
        check_action
        selected_installed_cli || exit 1
        manager_call assert-ready >/dev/null
        if [ -d "$HOME/.local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1" ] && [ "$#" -eq 0 ]; then
            [ -t 0 ] || { echo '✗ Owned update requires an explicit pinned candidate.' >&2; exit 1; }
            read -r -p 'Physical executable in the already installed verified candidate release: ' candidate || exit 1
            [ -n "$candidate" ] || exit 1
            set -- "$candidate"
        fi
        exec "$SCRIPT_DIR/plugin-build.sh" --operator-managed-tailscale-cli "$@"
        ;;
    uninstall)
        [ "$#" -eq 0 ] || { usage; exit 2; }
        selected_installed_cli || exit 1
        activation_check
        check_action
        # Full-plugin erase has separate native confinement obligations. The
        # shipped uninstaller retains its fail-closed guard; do not use this
        # action as a substitute for the owned route/service removal above.
        exec "$SCRIPT_DIR/uninstall.sh"
        ;;
    *) usage; exit 2 ;;
esac

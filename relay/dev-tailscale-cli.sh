#!/bin/bash
# Isolated foreground development transport for the separate CLI Serve owner.
# The real HOME is preserved for the selected Tailscale CLI; all relay roots,
# credentials, release data, sockets, caches, and journals live in the private
# .dev-tailscale-cli tree. Stopping the process never removes the persistent route.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd -P)"
DEFAULT_DEV_ROOT="$SCRIPT_DIR/.dev-tailscale-cli"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELAY_BIN="${HERDR_DEV_TAILSCALE_CLI_RELAY_BIN:-}"
if [ -n "$RELAY_BIN" ]; then
    case "$RELAY_BIN" in /*) [ -x "$RELAY_BIN" ] && [ ! -d "$RELAY_BIN" ] || { echo "✗ Selected relay binary is not executable." >&2; exit 2; } ;; *) echo "✗ Relay binary path must be absolute." >&2; exit 2 ;; esac
    "$RELAY_BIN" tailscale-cli activation-check || exit $?
else
    command -v go >/dev/null 2>&1 || { echo "✗ Go is required to check the compile-time activation gate." >&2; exit 2; }
    (cd "$REPO_DIR" && GOTOOLCHAIN=local GOFLAGS=-mod=readonly go run ./cmd/herdr-mobile-relay tailscale-cli activation-check) || exit $?
fi

GATE_RELAY_BIN="$RELAY_BIN"
cli_relay_call() {
    if [ -n "$GATE_RELAY_BIN" ]; then
        "$GATE_RELAY_BIN" "$@"
    else
        (cd "$REPO_DIR" && GOTOOLCHAIN=local GOFLAGS=-mod=readonly go run ./cmd/herdr-mobile-relay "$@")
    fi
}

usage() {
    echo "CLI-backed development uses a separate development-scope registration and private state." >&2
    echo "It requires a selected absolute Tailscale CLI, exact HTTPS origin, node ID, and explicit risk consent." >&2
    echo "Interactive: make dev-tailscale-cli (development opt-in; then route publication consent)." >&2
    echo "Scripted: HERDR_DEV_TAILSCALE_CLI_ENABLE=1 make dev-tailscale-cli" >&2
    echo "Optional: HERDR_DEV_TAILSCALE_CLI_DIR, HERDR_DEV_TAILSCALE_CLI_BIN," >&2
    echo "  HERDR_DEV_TAILSCALE_CLI_ORIGIN, HERDR_DEV_TAILSCALE_CLI_NODE_ID, HERDR_DEV_HERDR_BIN." >&2
    echo "Lifecycle: setup, status, setup-link, recover, update, unpublish, and stop (Ctrl-C)." >&2
}

ACTION="${1:-setup}"
[ "$#" -eq 0 ] || shift
case "$ACTION" in setup|status|recover|unpublish|setup-link|update|stop) ;; *) usage; exit 2 ;; esac

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

DEFAULT_SHARED_COORDINATION_ROOT="$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination"
DEV_ENV_FILE="$DEV_ROOT/relay.env"
INSTALLED_ENV_FILE="$(installed_service_env_file 2>/dev/null || true)"
if [ -n "$INSTALLED_ENV_FILE" ]; then
    PRODUCTION_ENV_FILE="$INSTALLED_ENV_FILE"
    if [ -n "${HERDR_RELAY_ENV:-}" ] && [ "$HERDR_RELAY_ENV" != "$DEV_ENV_FILE" ] &&
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
DEV_RESERVATION_CLAIMED=false
DEV_PUBLISH_STARTED=false
DEV_PUBLISH_NOT_DISPATCHED=false
# shellcheck disable=SC2329 # Registered as an EXIT trap after the reservation is claimed.
cleanup_dev_backend_reservation() {
    [ "$DEV_RESERVATION_CLAIMED" = true ] || return 0
    if [ "$DEV_PUBLISH_STARTED" = true ] && [ "$DEV_PUBLISH_NOT_DISPATCHED" != true ]; then
        return 0
    fi
    # cleanup_relay calls this only after stopping the backend. The manager
    # retains reservations whenever a journal or observed route is present.
    if cli_relay_call tailscale-cli release-backend-port --binary "$CLI_BIN" \
        --state-root "$DEV_ROOT/registration" --coordination-root "$COORDINATION_ROOT" \
        --scope development --installation-id "$HERDR_RELAY_INSTANCE_ID" --node-id "$NODE_ID" \
        --origin "$ORIGIN" --https-port "$HTTPS_PORT" --backend-port "$RELAY_PORT" >/dev/null 2>&1; then
        DEV_RESERVATION_CLAIMED=false
    else
        echo "⚠ Development backend reservation retained because read-only inspection did not prove safe release." >&2
    fi
}
if [ "$ACTION" != setup ]; then
    if [ ! -d "$DEV_ROOT" ] || [ ! -f "$ENV_FILE" ] || [ ! -f "$MARKER" ] ||
        ! grep -Fxq 'HERDR_DEV_TAILSCALE_CLI_ROOT=1' "$MARKER"; then
        echo "✗ No marked CLI development state exists; nothing was inspected or removed." >&2
        exit 1
    fi
    load_relay_env "$ENV_FILE"
    RELAY_BIN="${HERDR_DEV_TAILSCALE_CLI_RELAY_BIN:-$DEV_ROOT/bin/herdr-mobile-relay}"
    CLI_BIN="${HERDR_TAILSCALE_CLI_BIN:-}"
    manager_args=(--binary "$CLI_BIN" --state-root "$HERDR_TAILSCALE_CLI_STATE_ROOT" \
        --coordination-root "$COORDINATION_ROOT" --scope development \
        --installation-id "$HERDR_RELAY_INSTANCE_ID" --https-port "$HERDR_TAILSCALE_CLI_HTTPS_PORT" \
        --backend-port "$HERDR_RELAY_PORT")
    case "$ACTION" in
        status) exec "$RELAY_BIN" tailscale-cli status "${manager_args[@]}" ;;
        recover) exec "$RELAY_BIN" tailscale-cli recover "${manager_args[@]}" ;;
        setup-link)
            export HERDR_DEV_TAILSCALE_CLI_ROOT=1 HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$RELAY_BIN"
            export HERDR_WEB_ROOT="$DEV_ROOT/web" HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
            export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
            exec "$SCRIPT_DIR/setup-link.sh"
            ;;
        update)
            [ ! -S "$DEV_ROOT/config/pairing-control.sock" ] || {
                echo "✗ Stop the foreground development relay before replacing its build; registration was retained." >&2
                exit 1
            }
            "$RELAY_BIN" tailscale-cli assert-ready "${manager_args[@]}" >/dev/null || {
                echo "✗ Exact development route is not ready; no build or route mutation was performed." >&2
                exit 1
            }
            ;;
        unpublish)
            [ -t 0 ] || { echo "✗ Route removal requires an interactive terminal." >&2; exit 2; }
            echo "This removes only the exact development route in the private journal."
            echo "The CLI check-to-write interval is not atomic; do not edit Serve concurrently."
            echo "It cannot guarantee remote connection drain and will not reset other Serve routes."
            read -r -p "Authorize removal and accept the check-to-write race and no-remote-drain limits? [y/N] " answer || exit 2
            case "$answer" in y|Y|yes|YES) ;; *) echo "Cancelled; route and journal are unchanged."; exit 1 ;; esac
            exec "$RELAY_BIN" tailscale-cli unpublish "${manager_args[@]}" \
                --accepted --node-id "${HERDR_DEV_TAILSCALE_CLI_NODE_ID:-}" \
                --accept-route-removal --accept-check-to-write-race --accept-no-remote-drain
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
case "$HERDR_DEV_HERDR_BIN" in /*) [ -x "$HERDR_DEV_HERDR_BIN" ] && [ ! -d "$HERDR_DEV_HERDR_BIN" ] || { echo "✗ Select an absolute executable Herdr path." >&2; exit 2; } ;; *) echo "✗ Set HERDR_DEV_HERDR_BIN to the absolute Herdr executable." >&2; exit 2 ;; esac
HERDR_DEV_HERDR_SOCKET="${HERDR_DEV_HERDR_SOCKET:-${HERDR_SOCKET_PATH:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/herdr.sock}}"
case "$HERDR_DEV_HERDR_SOCKET" in /*) ;; *) echo "✗ Herdr socket path must be absolute." >&2; exit 2 ;; esac
[ -S "$HERDR_DEV_HERDR_SOCKET" ] || { echo "✗ Herdr socket does not exist; it is not contacted during selection." >&2; exit 2; }

RELAY_PORT="${HERDR_DEV_TAILSCALE_CLI_PORT:-18377}"
PLUGIN_PORT="${HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT:-18378}"
HTTPS_PORT="${HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT:-8443}"
for port in "$RELAY_PORT" "$PLUGIN_PORT" "$HTTPS_PORT"; do
    case "$port" in ''|*[!0-9]*) usage; exit 2 ;; esac
    [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || { echo "✗ Development ports must be unprivileged." >&2; exit 2; }
    case "$port" in 8375|8376|18375|18376) echo "✗ Production, tunnel, and managed-Tailscale ports are reserved." >&2; exit 2 ;; esac
done
[ "$RELAY_PORT" != "$PLUGIN_PORT" ] && [ "$RELAY_PORT" != "$HTTPS_PORT" ] && [ "$PLUGIN_PORT" != "$HTTPS_PORT" ] || { echo "✗ Choose three distinct development ports." >&2; exit 2; }

CONFIGURED_ORIGIN="${HERDR_DEV_TAILSCALE_CLI_ORIGIN:-${HERDR_TAILSCALE_CLI_ORIGIN:-}}"
PHONE_APP="${HERDR_DEV_PHONE_APP_URL:-${HERDR_PHONE_APP_URL:-}}"
CONFIGURED_NODE_ID="${HERDR_DEV_TAILSCALE_CLI_NODE_ID:-${HERDR_TAILSCALE_CLI_NODE_ID:-}}"
[ -n "$PHONE_APP" ] || { echo "✗ Set the exact verified phone-app origin." >&2; exit 2; }
if [ "$ACTION" = setup ]; then
    CLI_BIN="$(cli_relay_call tailscale-cli resolve-binary --binary "$CLI_BIN")" || {
        echo "✗ No unambiguous absolute Tailscale CLI candidate was selected." >&2
        exit 2
    }
    PREFLIGHT="$(cli_relay_call tailscale-cli preflight --binary "$CLI_BIN" --https-port "$HTTPS_PORT")" || {
        echo "✗ Read-only Tailscale node/origin preflight failed; no route or state was changed." >&2
        exit 1
    }
    NODE_ID="$(json_string_field "$PREFLIGHT" node_id "$RELAY_BIN")"
    ORIGIN="$(json_string_field "$PREFLIGHT" origin "$RELAY_BIN")"
    DNS_NAME="$(json_string_field "$PREFLIGHT" dns_name "$RELAY_BIN")"
    [ -n "$NODE_ID" ] && [ -n "$ORIGIN" ] && [ -n "$DNS_NAME" ] || {
        echo "✗ Read-only preflight did not establish a complete node identity and origin." >&2
        exit 1
    }
    [ -z "$CONFIGURED_NODE_ID" ] || [ "$CONFIGURED_NODE_ID" = "$NODE_ID" ] || {
        echo "✗ Configured node ID does not match the live read-only preflight." >&2
        exit 1
    }
    [ -z "$CONFIGURED_ORIGIN" ] || [ "$CONFIGURED_ORIGIN" = "$ORIGIN" ] || {
        echo "✗ Configured HTTPS origin does not match the live node's canonical origin." >&2
        exit 1
    }
    if [ -n "${HERDR_DEV_TAILSCALE_CLI_RELAY_BIN:-}" ]; then
        [ "$("$RELAY_BIN" normalize-external-origin "$ORIGIN" 2>/dev/null || true)" = "$ORIGIN" ] || {
            echo "✗ Development HTTPS origin is not canonical." >&2
            exit 2
        }
    fi
else
    ORIGIN="$CONFIGURED_ORIGIN"
    NODE_ID="$CONFIGURED_NODE_ID"
    [ -n "$ORIGIN" ] && [ -n "$NODE_ID" ] || { echo "✗ Existing CLI development identity is incomplete." >&2; exit 2; }
fi

if [ "$ACTION" = setup ]; then
    if [ -t 0 ]; then
        echo "Development-only persistent HTTPS route: $ORIGIN -> 127.0.0.1:$RELAY_PORT"
        echo "Scope: development; HTTPS port: $HTTPS_PORT; selected node: $NODE_ID"
        echo "The route survives Ctrl-C, stop, and checkout deletion unless explicitly unpublished."
        echo "Consent also accepts the CLI check/write race, backend port reuse, no global rollback, and no remote-drain guarantee."
        read -r -p "Type PUBLISH to authorize this exact route and all listed risks: " answer || exit 2
        [ "$answer" = PUBLISH ] || { echo "Cancelled; no state or route was changed."; exit 1; }
    else
        [ "${HERDR_DEV_TAILSCALE_CLI_PUBLISH:-}" = PUBLISH ] || { echo "✗ Set HERDR_DEV_TAILSCALE_CLI_PUBLISH=PUBLISH for the exact route consent." >&2; exit 2; }
    fi
else
    [ -f "$ENV_FILE" ] || { echo "✗ Update requires existing marked CLI development state." >&2; exit 1; }
fi

[ ! -S "$DEV_ROOT/config/pairing-control.sock" ] || {
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
for leaf in config cache data web bin registration; do
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
fi
if [ ! -f "$ENV_FILE" ]; then
    token="$(generate_token)"
    instance="$(generate_instance_id)"
    control_run="$(generate_instance_id)"
    [ "${#token}" -eq 32 ] && [ -n "$instance" ] && [ -n "$control_run" ] || { echo "✗ Could not prepare private development identity." >&2; exit 2; }
    printf 'HERDR_DEV_TAILSCALE_CLI_ROOT=1\n' > "$MARKER"
    chmod 600 "$MARKER"
    initializing_env="$DEV_ROOT/.relay-env.initializing.$$"
    : > "$initializing_env"
    chmod 600 "$initializing_env"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_TRANSPORT tailscale-cli
    set_env_value_atomic "$initializing_env" HERDR_RELAY_TOKEN "$token"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_INSTANCE_ID "$instance"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_CONTROL_RUN_ID "$control_run"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_HOST 127.0.0.1
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PORT "$RELAY_PORT"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PLUGIN_PORT "$PLUGIN_PORT"
    set_env_value_atomic "$initializing_env" HERDR_RELAY_PAIRING_SOCKET "$DEV_ROOT/config/pairing-control.sock"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_ORIGIN "$ORIGIN"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_SCOPE development
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_BIN "$CLI_BIN"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_STATE_ROOT "$DEV_ROOT/registration"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_COORDINATION_ROOT "$COORDINATION_ROOT"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_HTTPS_PORT "$HTTPS_PORT"
    set_env_value_atomic "$initializing_env" HERDR_TAILSCALE_CLI_NODE_ID "$NODE_ID"
    set_env_value_atomic "$initializing_env" HERDR_PHONE_APP_URL "$PHONE_APP"
    set_env_value_atomic "$initializing_env" HERDR_BIN "$HERDR_DEV_HERDR_BIN"
    set_env_value_atomic "$initializing_env" HERDR_SOCKET_PATH "$HERDR_DEV_HERDR_SOCKET"
    set_env_value_atomic "$initializing_env" HERDR_REACHABILITY_PORT_MAPPING 0
    set_env_value_atomic "$initializing_env" HERDR_RELAY_REARM_BOOTSTRAP 0
    set_env_value_atomic "$initializing_env" HERDR_RELAY_POLL_INTERVAL 2
    mv "$initializing_env" "$ENV_FILE"
else
    [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_TRANSPORT)" = tailscale-cli ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_SCOPE)" = development ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_PORT)" = "$RELAY_PORT" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_PLUGIN_PORT)" = "$PLUGIN_PORT" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_HTTPS_PORT)" = "$HTTPS_PORT" ] || {
        echo "✗ Existing CLI development identity, scope or ports changed; state was retained." >&2
        exit 1
    }
    [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_ORIGIN)" = "$ORIGIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_BIN)" = "$CLI_BIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_TAILSCALE_CLI_NODE_ID)" = "$NODE_ID" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_BIN)" = "$HERDR_DEV_HERDR_BIN" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_SOCKET_PATH)" = "$HERDR_DEV_HERDR_SOCKET" ] &&
        [ "$(env_file_value "$ENV_FILE" HERDR_PHONE_APP_URL)" = "$PHONE_APP" ] || {
        echo "✗ Existing CLI profile/origin changed; registration requires explicit recovery." >&2
        exit 1
    }
fi

load_relay_env "$ENV_FILE"
set_env_value_atomic "$ENV_FILE" HERDR_TAILSCALE_CLI_COORDINATION_ROOT "$COORDINATION_ROOT"
if [ "$ACTION" = setup ]; then
    cli_relay_call tailscale-cli reserve-backend-port --binary "$CLI_BIN" \
        --state-root "$DEV_ROOT/registration" --coordination-root "$COORDINATION_ROOT" \
        --scope development --installation-id "$HERDR_RELAY_INSTANCE_ID" --node-id "$NODE_ID" \
        --origin "$ORIGIN" --https-port "$HTTPS_PORT" --backend-port "$RELAY_PORT"
    DEV_RESERVATION_CLAIMED=true
    trap cleanup_dev_backend_reservation EXIT
fi
RELAY_BIN="$DEV_ROOT/bin/herdr-mobile-relay"
version="$(sed -n 's/^version = "\([0-9.]*\)"$/\1/p' "$REPO_DIR/herdr-plugin.toml")"
revision="$(git -C "$REPO_DIR" rev-parse HEAD)"
[ -n "$version" ] && [ "${#revision}" -eq 40 ] || { echo "✗ Cannot determine coherent development build identity." >&2; exit 1; }
BUILD_DIR="$(mktemp -d "$DEV_ROOT/.build.XXXXXX")"
bun run --cwd "$REPO_DIR/frontend" build --outDir "$BUILD_DIR/web"
bun "$REPO_DIR/scripts/stamp-web-version.mjs" "$BUILD_DIR/web/version.json" "$version" "$revision"
bun "$REPO_DIR/frontend/scripts/validate-build.mjs" "$BUILD_DIR/web"
CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.revision=$revision" \
    -o "$BUILD_DIR/herdr-mobile-relay" "$REPO_DIR/cmd/herdr-mobile-relay"
[ ! -L "$DEV_ROOT/bin/herdr-mobile-relay" ] && [ ! -L "$DEV_ROOT/web" ] || {
    echo "✗ Refusing to replace symlinked development build state." >&2
    exit 1
}
[ ! -e "$DEV_ROOT/bin/herdr-mobile-relay" ] || mv "$DEV_ROOT/bin/herdr-mobile-relay" "$BUILD_DIR/herdr-mobile-relay.previous"
[ ! -e "$DEV_ROOT/web" ] || mv "$DEV_ROOT/web" "$BUILD_DIR/web.previous"
mv "$BUILD_DIR/herdr-mobile-relay" "$DEV_ROOT/bin/herdr-mobile-relay"
mv "$BUILD_DIR/web" "$DEV_ROOT/web"
export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELAY_BIN="$RELAY_BIN" HERDR_WEB_ROOT="$DEV_ROOT/web"
export HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
export XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
export HERDR_RELAY_HOST=127.0.0.1 HERDR_RELAY_TRANSPORT=tailscale-cli HERDR_RELAY_REARM_BOOTSTRAP=0 HERDR_REACHABILITY_PORT_MAPPING=0
unset HERDR_PLUGIN_CONFIG_DIR HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_ORIGIN HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID
unset GH_TOKEN CURL_CA_BUNDLE SSL_CERT_FILE NODE_EXTRA_CA_CERTS
export HERDR_BIN="$HERDR_DEV_HERDR_BIN" HERDR_SOCKET_PATH="$HERDR_DEV_HERDR_SOCKET"
export HERDR_TAILSCALE_CLI_BIN="$CLI_BIN" HERDR_TAILSCALE_CLI_ORIGIN="$ORIGIN" HERDR_TAILSCALE_CLI_SCOPE=development
export HERDR_TAILSCALE_CLI_STATE_ROOT="$DEV_ROOT/registration" HERDR_TAILSCALE_CLI_COORDINATION_ROOT="$COORDINATION_ROOT"
export HERDR_TAILSCALE_CLI_HTTPS_PORT="$HTTPS_PORT" HERDR_PHONE_APP_URL="$PHONE_APP"
export HERDR_RELAY_PAIRING_SOCKET="$DEV_ROOT/config/pairing-control.sock"
export HERDR_RELAY_PORT="$RELAY_PORT" HERDR_RELAY_PLUGIN_PORT="$PLUGIN_PORT"

RELAY_PID=""
# shellcheck disable=SC2329 # Registered as EXIT trap to stop the foreground fixture relay.
cleanup_relay() {
    if [ -n "$RELAY_PID" ] && kill -0 "$RELAY_PID" 2>/dev/null; then
        kill "$RELAY_PID" 2>/dev/null || true
        wait "$RELAY_PID" 2>/dev/null || true
    fi
    cleanup_dev_backend_reservation
}
trap cleanup_relay EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"$RELAY_BIN" serve &
RELAY_PID=$!
if ! wait_for_relay_identity_health "$RELAY_PORT" "$HERDR_RELAY_INSTANCE_ID" "$ORIGIN"; then
    echo "✗ The exact development relay did not bind and pass local readiness; no route was published." >&2
    exit 1
fi

if [ "$ACTION" = setup ]; then
    DEV_PUBLISH_STARTED=true
    if "$RELAY_BIN" tailscale-cli publish --binary "$CLI_BIN" \
        --state-root "$DEV_ROOT/registration" --coordination-root "$COORDINATION_ROOT" \
        --scope development --installation-id "$HERDR_RELAY_INSTANCE_ID" --origin "$ORIGIN" \
        --node-id "$NODE_ID" --https-port "$HTTPS_PORT" --backend-port "$RELAY_PORT" \
        --accepted --accept-persistent-route --accept-check-to-write-race --accept-port-reuse \
        --accept-no-rollback --accept-no-remote-drain; then
        :
    else
        publish_status=$?
        [ "$publish_status" -eq 3 ] && DEV_PUBLISH_NOT_DISPATCHED=true
        exit "$publish_status"
    fi
else
    "$RELAY_BIN" tailscale-cli assert-ready --binary "$CLI_BIN" \
        --state-root "$DEV_ROOT/registration" --coordination-root "$COORDINATION_ROOT" \
        --scope development --installation-id "$HERDR_RELAY_INSTANCE_ID" --origin "$ORIGIN" \
        --https-port "$HTTPS_PORT" --backend-port "$RELAY_PORT" >/dev/null || {
        echo "✗ Development route drifted after update; no route repair was attempted." >&2
        exit 1
    }
fi

admitted="$("$RELAY_BIN" pairing-control --socket "$DEV_ROOT/config/pairing-control.sock" \
    --operation admit --run-id "$HERDR_RELAY_CONTROL_RUN_ID" --instance "$HERDR_RELAY_INSTANCE_ID")" || {
    echo "✗ Development admission could not resume the exact registered route." >&2
    exit 1
}
[ "$(json_bool_field "$admitted" ready)" = true ] &&
    [ "$(json_bool_field "$admitted" persistent_route_ready)" = true ] || {
    echo "✗ Development admission did not confirm the exact registered route." >&2
    exit 1
}
if [ "$ACTION" = setup ]; then
    armed="$("$RELAY_BIN" pairing-control --socket "$DEV_ROOT/config/pairing-control.sock" \
        --operation arm_bootstrap --run-id "$HERDR_RELAY_CONTROL_RUN_ID" --instance "$HERDR_RELAY_INSTANCE_ID")" || {
        echo "✗ Operator-initiated development invitation could not be armed; the route was retained." >&2
        exit 1
    }
    [ "$(json_bool_field "$armed" invitation_armed)" = true ] || {
        echo "✗ Development invitation was not durably armed." >&2
        exit 1
    }
fi

echo "CLI Serve route published to the private development journal. Ctrl-C stops only this relay process; the route remains configured."
echo "Run '$SCRIPT_DIR/dev-tailscale-cli.sh status|recover|unpublish' for later route lifecycle operations."
status=0
wait "$RELAY_PID" || status=$?
RELAY_PID=""
exit "$status"

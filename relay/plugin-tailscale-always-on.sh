#!/usr/bin/env bash
# Installed, explicitly selected persistent Serve. This chooser does not own
# publication: the existing promotion and installed-control wizard do.
set -euo pipefail
umask 077
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELAY_BIN="$(relay_binary)" || exit 1
# No configuration, native-manager or CLI inspection before production activation.
"$RELAY_BIN" tailscale-cli activation-check || exit 1
[ -t 0 ] && [ -t 1 ] || { echo '✗ Installed Serve selection requires an interactive terminal.' >&2; exit 1; }
require_user_service_context || exit 1
ENV_FILE="$(relay_service_env_file "$SCRIPT_DIR")" || exit 1
case "$ENV_FILE" in /*) ;; *) echo '✗ Installed relay configuration must be absolute.' >&2; exit 1 ;; esac
[ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] && [ -r "$ENV_FILE" ] &&
    native_resource_parents_safe "$ENV_FILE" && native_mutable_file_safe "$ENV_FILE" &&
    literal_env_scan "$ENV_FILE" || { echo '✗ A private, literal installed relay configuration is required; nothing was selected.' >&2; exit 1; }
original_text="$LITERAL_TEXT"
assert_service_env_matches "$ENV_FILE" || exit 1
# A service definition (including the legacy label) must never be replaced or
# restarted by a transport choice, even if it currently looks stopped.
if cli_setup_service_definition_present; then
    echo '✗ An installed service definition already exists. No automatic transport switch or native replacement is allowed.' >&2
    exit 1
fi
for session in "$(tailscale_session_file "$ENV_FILE")" "$(tailscale_external_session_file "$ENV_FILE")"; do
    [ ! -e "$session" ] && [ ! -L "$session" ] || { echo '✗ Stop and resolve the foreground Serve session first.' >&2; exit 1; }
done
[ ! -e "$HOME/.local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1" ] &&
    [ ! -L "$HOME/.local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1" ] || {
    echo '✗ An installed native claim already exists; inspect/recover it, never replace it from setup.' >&2; exit 1;
}
# Only literal values are considered. A live route is not a selection or an
# owned registration. Do not import saved helper, development or consent knobs.
for key in "${LITERAL_KEYS[@]}"; do
    case "$key" in
        HERDR_RELAY_BIN|HERDR_RELEASE_ROOT|HERDR_WEB_ROOT|HERDR_PLUTIL|HERDR_DEV_*|HERDR_CLI_*|HERDR_SETUP_*|HERDR_TAILSCALE_REQUEST|HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT)
            echo '✗ Unsafe installed selection data; nothing was changed.' >&2; exit 1 ;;
    esac
done
literal_env_get HERDR_RELAY_TRANSPORT
case "$LITERAL_VALUE" in ''|cloudflare|gateway|tailscale|tailscale-external|tailscale-cli) current="$LITERAL_VALUE" ;; *) echo '✗ Invalid saved transport.' >&2; exit 1 ;; esac
literal_env_get HERDR_GATEWAY_URL
[ "$current" != tailscale-cli ] || [ -z "$LITERAL_VALUE" ] || { echo '✗ Selected CLI configuration conflicts with a gateway.' >&2; exit 1; }
for key in HERDR_TAILSCALE_ORIGIN HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT; do
    literal_env_get "$key"
    [ -z "$LITERAL_VALUE" ] || { echo '✗ Another Serve/session selection is recorded; resolve it before installing.' >&2; exit 1; }
done
literal_env_get HERDR_RELAY_INSTANCE_ID; instance="$LITERAL_VALUE"
literal_env_get HERDR_RELAY_TOKEN; token="$LITERAL_VALUE"
[ -n "$instance" ] && [ "${#token}" -eq 32 ] || { echo '✗ Install the plugin and prepare its relay identity first.' >&2; exit 1; }
literal_env_get HERDR_RELAY_HOST
case "$LITERAL_VALUE" in ''|127.0.0.1) ;; *) echo '✗ Installed Serve needs a loopback relay.' >&2; exit 1 ;; esac
literal_env_get HERDR_RELAY_PORT; backend="${LITERAL_VALUE:-8375}"
literal_env_get HERDR_RELAY_PLUGIN_PORT; plugin="${LITERAL_VALUE:-8376}"
case "$backend:$plugin" in *[!0-9:]*|:*|*:) echo '✗ Invalid saved relay ports.' >&2; exit 1 ;; esac
literal_env_get HERDR_PHONE_APP_URL; app="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_BIN; saved_binary="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_HTTPS_PORT; saved_https="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_NODE_ID; saved_node="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_ORIGIN; saved_origin="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_SCOPE; saved_scope="$LITERAL_VALUE"
state_root="$HOME/.local/state/herdr-mobile-relay/tailscale-cli-registration"
coordination_root="$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination"
literal_env_get HERDR_TAILSCALE_CLI_STATE_ROOT; saved_state_root="$LITERAL_VALUE"; [ -z "$LITERAL_VALUE" ] || state_root="$LITERAL_VALUE"
literal_env_get HERDR_TAILSCALE_CLI_COORDINATION_ROOT; saved_coordination_root="$LITERAL_VALUE"; [ -z "$LITERAL_VALUE" ] || coordination_root="$LITERAL_VALUE"
# Never adopt a pre-existing unselected journal; it may belong to another run.
native_resource_parents_safe "$state_root" && native_resource_parents_safe "$coordination_root" || exit 1
if [ -e "$state_root" ] || [ -L "$state_root" ]; then
    echo '✗ A Serve registration root already exists; inspect/recover it, never start fresh setup over it.' >&2; exit 1
fi
case "$saved_https" in ''|*[!0-9]*) [ -z "$saved_https" ] || { echo '✗ Invalid saved HTTPS port.' >&2; exit 1; } ;; esac
https="${saved_https:-443}"
[ "$https" -ge 1 ] && [ "$https" -le 65535 ] && [ "$https" != "$backend" ] || { echo '✗ Invalid HTTPS listener.' >&2; exit 1; }
# The resolver checks ambiguity and absolute executable identity without running
# a CLI. The second command performs only read-only status/version/Serve queries.
cli="$("$RELAY_BIN" tailscale-cli resolve-binary --binary "${saved_binary:-${HERDR_TAILSCALE_CLI_BIN:-}}")" || exit 1
report="$("$RELAY_BIN" tailscale-cli installed-preflight --binary "$cli" --https-port "$https" --backend-port "$backend")" || {
    echo '✗ Installed CLI identity/profile is not qualified; selection is unchanged.' >&2; exit 1;
}
node="$(json_string_field "$report" node_id "$RELAY_BIN")"
origin="$(json_string_field "$report" origin "$RELAY_BIN")"
[ -n "$node" ] && [ -n "$origin" ] || { echo '✗ Incomplete read-only node identity.' >&2; exit 1; }
[ -z "$saved_node" ] || [ "$saved_node" = "$node" ] || { echo '✗ Pinned node differs; selection is unchanged.' >&2; exit 1; }
[ -z "$saved_origin" ] || [ "$saved_origin" = "$origin" ] || { echo '✗ Pinned origin differs; selection is unchanged.' >&2; exit 1; }
if [ "$current" = tailscale-cli ]; then
    [ "$saved_scope" = production ] && [ "$saved_binary" = "$cli" ] &&
        [ "$saved_https" = "$https" ] && [ "$saved_node" = "$node" ] &&
        [ "$saved_origin" = "$origin" ] || { echo '✗ Existing CLI selection is immutable; recover it explicitly.' >&2; exit 1; }
else
    [ -z "$saved_scope$saved_binary$saved_node$saved_origin$saved_https$saved_state_root$saved_coordination_root" ] || {
        echo '✗ Partial CLI selection exists; do not overwrite it from the menu.' >&2; exit 1;
    }
fi
[ -n "$app" ] || app="$origin"
printf 'Installed Serve: node %s, HTTPS %s -> 127.0.0.1:%s (plugin %s), phone app %s\n' "$node" "$origin" "$backend" "$plugin" "$app"
printf '%s\n' 'This creates an owned persistent route and a user service. Stopping the service does not unpublish Serve.'
read -r -p 'Select this exact installed route for the existing setup/consent wizard? [y/N] ' answer || exit 1
case "$answer" in y|Y|yes|YES) ;; *) echo 'Cancelled; selection was left unchanged.'; exit 1 ;; esac
stage="" created_state=false created_coordination=false
cleanup_selection() {
    [ -z "$stage" ] || rm -f -- "$stage"
    [ "$created_state" = false ] || rmdir -- "$state_root" 2>/dev/null || :
    [ "$created_coordination" = false ] || rmdir -- "$coordination_root" 2>/dev/null || :
}
trap cleanup_selection EXIT
if [ "$current" != tailscale-cli ]; then
    stage="$(mktemp "$(dirname "$ENV_FILE")/.tailscale-selection.XXXXXX")" || exit 1
    cp "$ENV_FILE" "$stage" && chmod 600 "$stage" || exit 1
    for key in HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_HTTPS_PORT HERDR_RELAY_REARM_BOOTSTRAP; do
        remove_env_value_atomic "$stage" "$key" || exit 1
    done
    for entry in \
        "HERDR_RELAY_TRANSPORT:tailscale-cli" "HERDR_RELAY_HOST:127.0.0.1" \
        "HERDR_RELAY_PORT:$backend" "HERDR_RELAY_PLUGIN_PORT:$plugin" \
        "HERDR_REACHABILITY_PORT_MAPPING:0" "HERDR_RELAY_REARM_BOOTSTRAP:0" \
        "HERDR_TAILSCALE_CLI_SCOPE:production" "HERDR_TAILSCALE_CLI_BIN:$cli" \
        "HERDR_TAILSCALE_CLI_ORIGIN:$origin" "HERDR_TAILSCALE_CLI_NODE_ID:$node" \
        "HERDR_TAILSCALE_CLI_HTTPS_PORT:$https" "HERDR_TAILSCALE_CLI_STATE_ROOT:$state_root" \
        "HERDR_TAILSCALE_CLI_COORDINATION_ROOT:$coordination_root" "HERDR_PHONE_APP_URL:$app"; do
        set_env_value_atomic "$stage" "${entry%%:*}" "${entry#*:}" || exit 1
    done
    ( unset HERDR_RELAY_TRANSPORT HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_REQUEST
      load_relay_env "$stage"
      "$RELAY_BIN" tailscale-cli installed-selection-check ) || {
        echo '✗ Candidate installed configuration was refused; previous selection retained.' >&2; exit 1;
    }
else
    ( unset HERDR_RELAY_TRANSPORT HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_REQUEST
      load_relay_env "$ENV_FILE"
      "$RELAY_BIN" tailscale-cli installed-selection-check ) || exit 1
fi
# The existing installed manager requires real private roots, but does not
# create them. Prepare them only after consent and validated selection.
prepare_root() {
        local path="$1" mode owner
        case "$path" in /*) ;; *) return 1 ;; esac
        [ ! -L "$path" ] || return 1
        if [ ! -e "$path" ]; then
            mkdir -p "$(dirname "$path")" || return 1
            mkdir -m 700 "$path" || return 1
            if [ "$path" = "$state_root" ]; then created_state=true; else created_coordination=true; fi
        fi
        [ -d "$path" ] && [ ! -L "$path" ] || return 1
        case "$(uname -s)" in
            Darwin) mode="$(stat -f '%Lp' "$path")"; owner="$(stat -f '%u' "$path")" ;;
            Linux) mode="$(stat -c '%a' "$path")"; owner="$(stat -c '%u' "$path")" ;;
            *) return 1 ;;
        esac
        [ "$mode" = 700 ] && [ "$owner" = "$(id -u)" ]
    }
# Recheck before the single atomic selection cutover. No route/native action
# occurs here; promotion separately revalidates its pinned configuration.
literal_env_read "$ENV_FILE" && [ "$LITERAL_TEXT" = "$original_text" ] &&
    native_mutable_file_safe "$ENV_FILE" &&
    [ ! -e "$state_root" ] && [ ! -L "$state_root" ] &&
    ! cli_setup_service_definition_present &&
    [ ! -e "$HOME/.local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1" ] || exit 1
prepare_root "$state_root" || exit 1
prepare_root "$coordination_root" || exit 1
literal_env_read "$ENV_FILE" && [ "$LITERAL_TEXT" = "$original_text" ] &&
    ! cli_setup_service_definition_present || exit 1
if [ -n "$stage" ]; then mv -f "$stage" "$ENV_FILE" || exit 1; fi
trap - EXIT
export HERDR_RELAY_ENV="$ENV_FILE"
# Publication, fresh-only native installation and seven exact consent phrases
# remain exclusively in the existing installed promotion/control workflow.
( unset HERDR_RELAY_TRANSPORT HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_REQUEST
  load_relay_env "$ENV_FILE"
  "$SCRIPT_DIR/tailscale-cli.sh" setup ) || exit 1
# A failed/uncertain setup retains the explicit selection and journal for
# recovery; never roll back a possibly published route by changing transport.
link="$( ( unset HERDR_RELAY_TRANSPORT HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_REQUEST
           load_relay_env "$ENV_FILE"
           "$SCRIPT_DIR/tailscale-cli.sh" arm-bootstrap ) )" || exit 1
print_phone_setup "$link"

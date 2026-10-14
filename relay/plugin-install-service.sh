#!/usr/bin/env bash
set +x
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
if [ -n "${HERDR_BIN_PATH:-}" ]; then
    export HERDR_BIN="$HERDR_BIN_PATH"
fi

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

wizard_refuse() {
    echo "✗ Cloudflare service setup requires an unambiguous, background-compatible literal configuration; no setup was started." >&2
    exit 1
}

# Selection is read-only. An explicit choice wins; an existing installed
# definition must decode successfully, never become apparent absence.
wizard_env_file() {
    local definition platform selected
    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        case "$HERDR_RELAY_ENV" in *[$'\001'-$'\037'$'\177']*) return 1 ;; esac
        relay_service_env_file "$SCRIPT_DIR"
        return $?
    fi
    platform="$(uname -s)" || return 1
    case "$platform" in
        Linux) definition="$HOME/.config/systemd/user/herdr-mobile-relay.service" ;;
        Darwin) definition="$HOME/Library/LaunchAgents/com.herdr-mobile-relay.service.plist" ;;
        *) return 1 ;;
    esac
    if [ -e "$definition" ] || [ -L "$definition" ]; then
        native_resource_parents_safe "$definition" && native_mutable_file_safe "$definition" &&
            [ -r "$definition" ] && literal_env_read "$definition" || return 1
        # Preserve decoder output until its one framing newline is removed.
        # Command substitution must not hide a trailing control in the path.
        selected="$(installed_service_env_file 2>/dev/null && printf '.')" || return 1
        selected="${selected%.}"; selected="${selected%$'\n'}"
        [ -n "$selected" ] || return 1
        case "$selected" in *[$'\001'-$'\037'$'\177']*) return 1 ;; esac
        [ -r "$selected" ] || return 1
        printf '%s\n' "$selected"
        return $?
    fi
    case "${HERDR_PLUGIN_CONFIG_DIR:-}" in *[$'\001'-$'\037'$'\177']*) return 1 ;; esac
    selected="$(relay_service_env_file "$SCRIPT_DIR" 2>/dev/null)" || return 1
    if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ] && [ ! -e "$selected" ] &&
        { [ -e "$SCRIPT_DIR/.env" ] || [ -L "$SCRIPT_DIR/.env" ]; }; then
        return 1
    fi
    printf '%s\n' "$selected"
}

wizard_trim() {
    WIZARD_VALUE="$1"
    while [[ "$WIZARD_VALUE" == [[:space:]]* ]]; do WIZARD_VALUE="${WIZARD_VALUE:1}"; done
    while [[ "$WIZARD_VALUE" == *[[:space:]] ]]; do WIZARD_VALUE="${WIZARD_VALUE%?}"; done
}

wizard_policy() {
    local key="$1" value="$2"
    case "$key" in
        HERDR_RELAY_RUN_ID|HERDR_RELAY_CONTROL_RUN_ID|HERDR_RELAY_PAIRING_SOCKET)
            [ -z "$value" ]; return $? ;;
        HERDR_RELAY_REARM_BOOTSTRAP|HERDR_TAILSCALE_REQUEST)
            case "$value" in ''|0|false|False|FALSE|f|F) return 0 ;; *) return 1 ;; esac ;;
    esac
    wizard_trim "$value"
    WIZARD_VALUE="$(printf '%s' "$WIZARD_VALUE" | LC_ALL=C tr '[:upper:]' '[:lower:]')" || return 1
    case "$key:$WIZARD_VALUE" in
        HERDR_RELAY_TRANSPORT:|HERDR_RELAY_TRANSPORT:cloudflare|HERDR_RELAY_TRANSPORT:gateway) return 0 ;;
        HERDR_GATEWAY_SELECTION:|HERDR_GATEWAY_SELECTION:ordered|HERDR_GATEWAY_SELECTION:latency) return 0 ;;
    esac
    return 1
}

# Only inspect the old gateway spelling, without a sourcing fallback or a
# binary/network probe. Edge whitespace may be trimmed; embedded whitespace,
# credentials, paths, queries and fragments must not turn into permission.
wizard_gateway() {
    local remaining="$1" entry authority
    WIZARD_GATEWAY_PRESENT=false
    while :; do
        entry="${remaining%%,*}"
        wizard_trim "$entry"; entry="$WIZARD_VALUE"
        if [ -n "$entry" ]; then
            case "$entry" in ws://*) authority="${entry#ws://}" ;; wss://*) authority="${entry#wss://}" ;; *) return 1 ;; esac
            authority="${authority%/}"
            case "$authority" in ''|*[[:space:]]*|*[/@?#]*) return 1 ;; esac
            # Brackets are literal IPv6 authority delimiters, not shell syntax.
            authority="${authority//\[/}"; authority="${authority//\]/}"
            case "$authority" in *[!a-zA-Z0-9.:-]*) return 1 ;; esac
            WIZARD_GATEWAY_PRESENT=true
        fi
        case "$remaining" in *,*) remaining="${remaining#*,}" ;; *) break ;; esac
    done
    return 0
}

# Native candidates remember CLOUDFLARED_BIN. A saved literal must agree with
# the caller's explicit choice, or an existing caller/stable/native PATH lookup.
# The fixed search paths mirror the read-only consumers; they do not let the
# selected file introduce arbitrary routing. Never execute/probe the program.
# Caller-selected package-manager symlinks remain valid.
wizard_cloudflared_bin() {
    local selected="$1" expected="${CLOUDFLARED_BIN:-}" search_path
    [ -n "$selected" ] || return 1
    case "$selected" in *[$'\001'-$'\037'$'\177']*) return 1 ;; esac
    case "$selected" in /*) ;; *) return 1 ;; esac
    case "/${selected#/}/" in *'/../'*|*'/./'*|*'//'*) return 1 ;; esac
    [ -f "$selected" ] && [ -x "$selected" ] || return 1
    if [ -n "$expected" ]; then
        [ "$selected" = "$expected" ]; return $?
    fi
    for search_path in "$PATH" \
        "$HOME/.local/bin:$PATH:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
        "$HOME/.local/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"; do
        expected="$(PATH="$search_path" command -v cloudflared 2>/dev/null)" || expected=""
        [ "$selected" != "$expected" ] || return 0
    done
    return 1
}

ENV_FILE="$(wizard_env_file 2>/dev/null)" || wizard_refuse
case "$ENV_FILE" in *[$'\001'-$'\037'$'\177']*) wizard_refuse ;; esac
native_resource_parents_safe "$ENV_FILE" || wizard_refuse
if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
    native_mutable_file_safe "$ENV_FILE" && [ -r "$ENV_FILE" ] || wizard_refuse
fi
literal_env_scan "$ENV_FILE" 2>/dev/null || wizard_refuse

# Saved data cannot select shell startup, release/helper executables, deployment
# interpreters or the verifier's web root. Nor can it supply setup permissions
# or helper controls. These belong to the caller, not the HERDR_* data wildcard.
# Only the native-produced cloudflared field has a caller-agreement exception.
# Nothing from this scan is sourced or exported.
for ((INDEX=0; INDEX<${#LITERAL_KEYS[@]}; INDEX++)); do
    KEY="${LITERAL_KEYS[INDEX]}"
    case "$KEY" in
        HERDR_BIN|HERDR_BIN_PATH|HERDR_PLUTIL|HERDR_RELEASE_ROOT|HERDR_WEB_ROOT|\
        HERDR_APP_DEPLOY_NPX|HERDR_APP_DEPLOY_NODE_DIR|\
        HERDR_*_BIN|HERDR_*_BIN_*|HERDR_DEV_*|HERDR_PLUGIN_*|HERDR_CLI_*|HERDR_STABLE_*|HERDR_SETUP_*) wizard_refuse ;;
        HERDR_RELAY_ENV)
            literal_env_get "$KEY" || wizard_refuse
            [ "$LITERAL_VALUE" = "$ENV_FILE" ] || wizard_refuse ;;
        CLOUDFLARED_BIN)
            literal_env_get "$KEY" && wizard_cloudflared_bin "$LITERAL_VALUE" || wizard_refuse ;;
        HERDR_*|CLOUDFLARED_CONFIG|TUNNEL_ORIGIN_CERT|GH_TOKEN|GITHUB_TOKEN) ;;
        *) wizard_refuse ;;
    esac
done
for KEY in HERDR_RELAY_TRANSPORT HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_REARM_BOOTSTRAP HERDR_TAILSCALE_REQUEST HERDR_GATEWAY_SELECTION; do
    literal_env_get "$KEY" && wizard_policy "$KEY" "$LITERAL_VALUE" || wizard_refuse
    # A saved empty assignment never suppresses an applicable ambient refusal.
    wizard_policy "$KEY" "${!KEY:-}" || wizard_refuse
done
literal_env_get HERDR_RELAY_TRANSPORT || wizard_refuse
wizard_policy HERDR_RELAY_TRANSPORT "$LITERAL_VALUE" || wizard_refuse
CURRENT_TRANSPORT="$WIZARD_VALUE"
wizard_policy HERDR_RELAY_TRANSPORT "${HERDR_RELAY_TRANSPORT:-}" || wizard_refuse
[ -n "$CURRENT_TRANSPORT" ] || CURRENT_TRANSPORT="$WIZARD_VALUE"
literal_env_get HERDR_GATEWAY_URL && wizard_gateway "$LITERAL_VALUE" || wizard_refuse
SAVED_GATEWAY="$WIZARD_GATEWAY_PRESENT"
wizard_gateway "${HERDR_GATEWAY_URL:-}" || wizard_refuse
if [ "$SAVED_GATEWAY" = true ] || [ "$WIZARD_GATEWAY_PRESENT" = true ]; then
    [ -n "$CURRENT_TRANSPORT" ] || CURRENT_TRANSPORT=gateway
fi
[ -n "$CURRENT_TRANSPORT" ] || CURRENT_TRANSPORT=cloudflare
for SESSION in "$(tailscale_session_file "$ENV_FILE")" "$(tailscale_external_session_file "$ENV_FILE")"; do
    [ ! -e "$SESSION" ] && [ ! -L "$SESSION" ] || wizard_refuse
done
literal_env_get HERDR_GITHUB_TOKEN_FILE || wizard_refuse
# Selected updater resources are read-only, never this wizard's env destination.
native_selected_pointer_disjoint "$LITERAL_VALUE" "$ENV_FILE" 2>/dev/null || wizard_refuse

# This action is the explicit Cloudflare choice, not a generic native installer.
# Use checked literal setters directly: the older compound transport setters
# source configuration and may mask an intermediate failure in a conditional.
export HERDR_RELAY_ENV="$ENV_FILE"
for KEY in HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_ORIGIN HERDR_TAILSCALE_HTTPS_PORT HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_REARM_BOOTSTRAP HERDR_TAILSCALE_REQUEST; do
    if ! remove_env_value_atomic "$ENV_FILE" "$KEY" 2>/dev/null; then
        echo "✗ Cloudflare selection could not be persisted; stable setup was not started." >&2
        exit 1
    fi
done
if ! set_env_value_atomic "$ENV_FILE" HERDR_RELAY_TRANSPORT cloudflare 2>/dev/null; then
    echo "✗ Cloudflare selection could not be persisted; stable setup was not started." >&2
    exit 1
fi
literal_env_scan "$ENV_FILE" 2>/dev/null || wizard_refuse
literal_env_get HERDR_RELAY_TRANSPORT || wizard_refuse
[ "$LITERAL_VALUE" = cloudflare ] || wizard_refuse
for KEY in HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_ORIGIN HERDR_TAILSCALE_HTTPS_PORT HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET HERDR_RELAY_REARM_BOOTSTRAP HERDR_TAILSCALE_REQUEST; do
    literal_env_get "$KEY" || wizard_refuse
    [ "$LITERAL_FOUND" = false ] || wizard_refuse
done
unset HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_TAILSCALE_ORIGIN HERDR_TAILSCALE_HTTPS_PORT
unset HERDR_EXTERNAL_HTTPS_ORIGIN HERDR_RELAY_RUN_ID HERDR_RELAY_CONTROL_RUN_ID HERDR_RELAY_PAIRING_SOCKET
unset HERDR_RELAY_REARM_BOOTSTRAP HERDR_TAILSCALE_REQUEST
export HERDR_RELAY_TRANSPORT=cloudflare
if [ "$CURRENT_TRANSPORT" = gateway ]; then
    echo "Switching this relay from the WebRTC gateway to Cloudflare."
    echo ""
fi

echo "🐑 Herdr Mobile Relay stable tunnel setup"
echo ""
echo "This wizard provisions or reuses a named Cloudflare tunnel, installs the"
echo "background service, and verifies the public relay before showing its QR."
echo "If you only want to try the relay, run Quick Start instead:"
echo "  herdr plugin action invoke quick-start --plugin herdr-mobile-relay.events"
echo ""

if HERDR_STABLE_SETUP_WRAPPED=1 "$SCRIPT_DIR/stable-setup.sh"; then
    STATUS=0
else
    STATUS=$?
    echo ""
    echo "Stable setup did not complete. Its state is resumable; use the exact"
    echo "rerun command printed above after correcting the reported problem."
fi
pause_before_close || :
exit "$STATUS"

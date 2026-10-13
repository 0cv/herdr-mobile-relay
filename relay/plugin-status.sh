#!/usr/bin/env bash
set -euo pipefail

# Status is a rendering of the packaged doctor, never shell-evaluated config or
# an independent service/network health implementation. No common.sh startup.
refuse() {
    if printf '%s\n' "$1" >&2; then :; fi
    exit "$2"
}
JSON_MODE=0
CONFIG_SET=0
SEEN_JSON=0
SEEN_INGRESS=0
SEEN_CONCISE=0
COMMAND=doctor
# Function positional arguments preserve the caller's argv without empty-array
# nounset behavior on the packaged macOS Bash 3.2.
validate_args() {
while [ "$#" -gt 0 ]; do
    case "$1" in
        --concise)
            [ "$SEEN_CONCISE" -eq 0 ] || refuse 'invalid diagnostic invocation' 2
            SEEN_CONCISE=1
            COMMAND=status
            ;;
        --json)
            [ "$SEEN_JSON" -eq 0 ] || refuse 'invalid diagnostic invocation' 2
            SEEN_JSON=1
            JSON_MODE=1
            ;;
        --probe-ingress)
            [ "$SEEN_INGRESS" -eq 0 ] || refuse 'invalid diagnostic invocation' 2
            SEEN_INGRESS=1
            ;;
        --config)
            [ "$CONFIG_SET" -eq 0 ] || refuse 'invalid diagnostic invocation' 2
            CONFIG_SET=1
            [ "$#" -ge 2 ] || refuse 'invalid diagnostic invocation' 2
            shift
            [ -n "$1" ] || refuse 'invalid diagnostic invocation' 2
            ;;
        *) refuse 'invalid diagnostic invocation' 2 ;;
    esac
    shift
done
}
validate_args "$@"
SCRIPT_PARENT=${BASH_SOURCE[0]%/*}
[ "$SCRIPT_PARENT" != "${BASH_SOURCE[0]}" ] || SCRIPT_PARENT=.
if ! SCRIPT_DIR="$(CDPATH= cd -- "$SCRIPT_PARENT" 2>/dev/null && pwd -P)"; then
    refuse 'diagnostic executable unavailable' 1
fi
PACKAGE_ROOT=${SCRIPT_DIR%/*}
if [ -f "$PACKAGE_ROOT/release-manifest.json" ] && [ -f "$PACKAGE_ROOT/herdr-mobile-relay" ] && [ -x "$PACKAGE_ROOT/herdr-mobile-relay" ]; then
    BINARY="$PACKAGE_ROOT/herdr-mobile-relay"
elif [ -n "${HERDR_RELAY_BIN:-}" ]; then
    BINARY="$HERDR_RELAY_BIN"
else
    [ -n "${HERDR_RELEASE_ROOT:-}${XDG_DATA_HOME:-}${HOME:-}" ] || refuse 'diagnostic executable unavailable' 1
    RELEASE_ROOT=${HERDR_RELEASE_ROOT:-${XDG_DATA_HOME:-${HOME:-}/.local/share}/herdr-mobile-relay}
    BINARY="$RELEASE_ROOT/current/herdr-mobile-relay"
fi
case "$BINARY" in /*) ;; *) refuse 'diagnostic executable unavailable' 1 ;; esac
[ -f "$BINARY" ] && [ -x "$BINARY" ] || refuse 'diagnostic executable unavailable' 1
if [ "$CONFIG_SET" -eq 0 ]; then
    if [ -n "${HERDR_RELAY_ENV:-}" ]; then
        ENV_FILE="$HERDR_RELAY_ENV"
    elif [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        ENV_FILE="$HERDR_PLUGIN_CONFIG_DIR/relay.env"
    else
        ENV_FILE="$SCRIPT_DIR/.env"
    fi
    set -- --config "$ENV_FILE" "$@"
fi
# Consume only the wrapper's rendering flag. Rotate remaining positional argv
# rather than using empty arrays (macOS Bash 3.2); config values remain data even
# if they resemble flags. Preserve the original ordering of forwarded arguments.
invoke_diagnostic() {
    local remaining="$#"
    local value
    while [ "$remaining" -gt 0 ]; do
        case "$1" in
            --concise) shift ;;
            --config)
                value="$2"
                shift 2
                set -- "$@" --config "$value"
                remaining=$((remaining - 1))
                ;;
            *)
                value="$1"
                shift
                set -- "$@" "$value"
                ;;
        esac
        remaining=$((remaining - 1))
    done
    "$BINARY" "$COMMAND" "$@"
}
# Suppress path-bearing shell/loader errors, including a raced-away executable.
# The trusted doctor's finalized output is a finite-catalog report. Retain it
# in memory (no temporary file) to distinguish a report decision of 2 from an
# invocation refusal, which emits NO stdout. Do not parse or re-decide health.
# Command substitution removes trailing newlines; restore the report terminator.
if REPORT="$(invoke_diagnostic "$@" 2>/dev/null)"; then
    STATUS=0
else
    STATUS=$?
fi
if [ -n "$REPORT" ]; then
    if ! printf '%s\n' "$REPORT" 2>/dev/null; then
        refuse 'diagnostic output unavailable' 1
    fi
elif [ "$STATUS" -eq 2 ]; then
    if printf '%s\n' 'diagnostic input refused' >&2; then :; fi
fi
case "$STATUS" in
    126|127) if printf '%s\n' 'diagnostic executable unavailable' >&2; then :; fi ;;
esac
# The setup menu pauses once itself. Never prompt or add text to JSON/pipelines;
# EOF at an interactive pause cannot change the diagnostic command's exit.
if [ "$JSON_MODE" -eq 0 ] && [ "${HERDR_SETUP_MENU:-}" != 1 ] && [ -t 0 ] && [ -t 1 ]; then
    if printf '\nPress Enter to close this pane.'; then
        if read -r _answer; then :; fi
    fi
fi
exit "$STATUS"

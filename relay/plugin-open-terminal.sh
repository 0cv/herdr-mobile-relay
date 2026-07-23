#!/bin/bash
# Open the standalone setup menu only in terminal environments whose launch
# semantics are explicit and testable. Unknown terminals deliberately fall
# back to the command printed by plugin-build.sh.
set -euo pipefail

PLUGIN_ROOT="${1:-}"
MODE="${2:-open}"
SETUP_COMMAND="$PLUGIN_ROOT/relay/plugin-setup-terminal.command"

if [ -z "$PLUGIN_ROOT" ] || [ ! -x "$SETUP_COMMAND" ]; then
    exit 1
fi

terminal_kind() {
    case "$(uname -s)" in
        Darwin)
            if [ "${TERM_PROGRAM:-}" = "Apple_Terminal" ] && command -v open >/dev/null 2>&1; then
                echo "apple-terminal"
                return
            fi
            ;;
        Linux)
            if command -v konsole >/dev/null 2>&1 && {
                [ -n "${KONSOLE_VERSION:-}" ] || [[ "${XDG_CURRENT_DESKTOP:-}" == *KDE* ]];
            }; then
                echo "konsole"
                return
            fi
            if command -v gnome-terminal >/dev/null 2>&1 && {
                [ -n "${GNOME_TERMINAL_SERVICE:-}" ] || [[ "${XDG_CURRENT_DESKTOP:-}" == *GNOME* ]];
            }; then
                echo "gnome-terminal"
                return
            fi
            ;;
    esac
    return 1
}

KIND="$(terminal_kind)" || exit 1
if [ "$MODE" = "--can-launch" ]; then
    exit 0
fi

case "$KIND" in
    apple-terminal)
        open -a Terminal "$SETUP_COMMAND" >/dev/null 2>&1
        ;;
    konsole)
        if [ -n "${KONSOLE_VERSION:-}" ]; then
            konsole --new-tab -e "$SETUP_COMMAND" >/dev/null 2>&1
        else
            konsole -e "$SETUP_COMMAND" >/dev/null 2>&1
        fi
        ;;
    gnome-terminal)
        gnome-terminal -- "$SETUP_COMMAND" >/dev/null 2>&1
        ;;
esac

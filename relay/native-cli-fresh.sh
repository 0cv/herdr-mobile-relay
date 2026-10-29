#!/usr/bin/env bash
# Fixed fresh-only native driver. Never source relay.env or enter the replacement
# transaction: that transaction can reactivate an earlier backend after publish.
set +x
set -euo pipefail
[ "$#" -eq 1 ] || exit 78
case "$1" in fresh|activate|quiesce|assert-stopped|start|remove|retire) ;; *) exit 78 ;; esac
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
. "$SCRIPT_DIR/common.sh"
. "$SCRIPT_DIR/native-install-transaction.sh"
require_user_service_context
case "$(uname -s)" in
    Darwin)
        native_manager=launchd
        label=com.herdr-mobile-relay.service
        legacy=com.herdr-remote.service
        definition="$HOME/Library/LaunchAgents/$label.plist"
        legacy_definition="$HOME/Library/LaunchAgents/$legacy.plist" ;;
    Linux)
        native_manager=systemd
        label=herdr-mobile-relay.service
        legacy=herdr-remote.service
        definition="$HOME/.config/systemd/user/$label"
        legacy_definition="$HOME/.config/systemd/user/$legacy" ;;
    *) exit 78 ;;
esac
native_resource_parents_safe "$definition" && native_resource_parents_safe "$legacy_definition" || exit 78
[ ! -e "$legacy_definition" ] && [ ! -L "$legacy_definition" ] || exit 78
if [ "$1" = fresh ] || [ "$1" = retire ]; then
    [ ! -e "$definition" ] && [ ! -L "$definition" ] || exit 78
else
    [ -f "$definition" ] && [ ! -L "$definition" ] && native_mutable_file_safe "$definition" || exit 78
fi
if [ "$1" = retire ]; then
    if [ "$native_manager" = systemd ]; then systemctl --user daemon-reload || exit 75; fi
    for name in "$label" "$legacy"; do
        native_install_observe "$name" || exit 75
        [ "$NATIVE_STATE:$NATIVE_ENABLED" = absent:false ] || exit 75
    done
    exit 0
fi
# Owned transitions may never stop a foreign loaded label or a legacy job.
if [ "$1" != fresh ] && [ "$1" != activate ]; then
    [ -f "$definition" ] && [ ! -L "$definition" ] && native_mutable_file_safe "$definition" || exit 78
    native_install_observe "$legacy" || exit 78
    if [ "$native_manager" = launchd ]; then
        [ "$NATIVE_STATE:$NATIVE_FLAG_PRESENT" = absent:0 ] || exit 78
    else
        [ "$NATIVE_STATE:$NATIVE_ENABLED" = absent:false ] || exit 78
    fi
    native_install_observe "$label" || exit 78
    native_install_origin_ok "$label" "$definition" || exit 78
    case "$1" in
        quiesce)
            if [ "$native_manager" = systemd ]; then
                systemctl --user disable "$label" && systemctl --user stop "$label" || exit 75
            else
                launchctl disable "gui/$UID/$label" || exit 75
                if [ "$NATIVE_STATE" = loaded ]; then launchctl bootout "gui/$UID/$label" || exit 75; fi
            fi ;;
        start)
            case "$NATIVE_STATE" in inactive|failed|absent) ;; *) exit 78 ;; esac
            [ "$NATIVE_ENABLED" = false ] || exit 78
            if [ "$native_manager" = systemd ]; then
                systemctl --user daemon-reload && systemctl --user enable "$label" && systemctl --user start "$label" || exit 75
            else
                launchctl enable "gui/$UID/$label" && launchctl bootstrap "gui/$UID" "$definition" || exit 75
            fi ;;
        assert-stopped|remove) ;; # reobserve below
    esac
    native_install_observe "$label" || exit 75
    if [ "$1" = start ]; then
        case "$NATIVE_STATE" in active|loaded) exit 0 ;; *) exit 75 ;; esac
    fi
    if [ "$native_manager" = launchd ]; then
        [ "$NATIVE_STATE:$NATIVE_ENABLED" = absent:false ] || exit 75
    else
        # Failed is parsed but never quiet, even when disabled.
        [ "$NATIVE_STATE:$NATIVE_ENABLED" = inactive:false ] || exit 75
    fi
    exit 0
fi
# Neither a disabled/failed service nor an old/foreign definition is fresh.
for name in "$label" "$legacy"; do
    native_install_observe "$name" || exit 78
    if [ "$native_manager" = launchd ]; then
        # An unlisted launchd label defaults to enabled even without any job
        # or persisted setting. A listed (including disabled) old label is NOT
        # fresh; never adopt its historical native state.
        [ "$NATIVE_STATE:$NATIVE_FLAG_PRESENT" = absent:0 ] || exit 78
    else
        [ "$NATIVE_STATE:$NATIVE_ENABLED" = absent:false ] || exit 78
    fi
done
[ "$1" = activate ] || exit 0
# One activation attempt. Never restart, roll back, clear Serve or touch legacy.
if [ "$native_manager" = systemd ]; then
    systemctl --user daemon-reload && systemctl --user enable "$label" && systemctl --user start "$label" || exit 75
else
    launchctl enable "gui/$UID/$label" && launchctl bootstrap "gui/$UID" "$definition" || exit 75
fi
native_install_observe "$label" || exit 75
case "$NATIVE_STATE" in active|loaded) exit 0 ;; *) exit 75 ;; esac

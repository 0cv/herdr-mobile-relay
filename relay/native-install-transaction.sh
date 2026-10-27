#!/usr/bin/env bash

native_systemd_state() {
    local code
    if systemctl --user "$1" --quiet "$2"; then
        printf true
        return 0
    else
        code=$?
    fi
    case "$1:$code" in
        is-active:3|is-active:4|is-enabled:1|is-enabled:4) printf false ;;
        *) echo "Cannot snapshot native service activation." >&2; return 1 ;;
    esac
}

native_install_begin() {
    native_manager="$1"
    native_definition="$2"
    native_legacy_definition="$3"
    native_environment="$4"
    native_label="$5"
    native_legacy_label="$6"
    native_active=false
    native_enabled=false
    native_legacy_active=false
    native_legacy_enabled=false
    native_changed=false
    native_stage=""
    native_committed=false
    local recovery_root="${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay/recovery"
    mkdir -p "$recovery_root" || return 1
    chmod 700 "$recovery_root" || return 1
    native_recovery="$(mktemp -d "$recovery_root/herdr-native-recovery.XXXXXX")" || return 1
    chmod 700 "$native_recovery" || return 1
    native_snapshot_complete=false
    trap native_install_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    local index=0 file
    for file in "$native_definition" "$native_legacy_definition" "$native_environment"; do
        if [ -e "$file" ] || [ -L "$file" ]; then
            [ -f "$file" ] && [ ! -L "$file" ] || return 1
            cp -p "$file" "$native_recovery/$index" || return 1
        fi
        index=$((index + 1))
    done
    if [ "$native_manager" = systemd ]; then
        systemctl --user show-environment >/dev/null || return 1
        native_active="$(native_systemd_state is-active "$native_label")" || return 1
        native_enabled="$(native_systemd_state is-enabled "$native_label")" || return 1
        native_legacy_active="$(native_systemd_state is-active "$native_legacy_label")" || return 1
        native_legacy_enabled="$(native_systemd_state is-enabled "$native_legacy_label")" || return 1
    else
        launchctl print "gui/$UID" >/dev/null 2>&1 || return 1
        launchctl print "gui/$UID/$native_label" >/dev/null 2>&1 && native_active=true
        launchctl print "gui/$UID/$native_legacy_label" >/dev/null 2>&1 && native_legacy_active=true
    fi
    printf 'definition=%s\nlegacy_definition=%s\nenvironment=%s\nactive=%s\nenabled=%s\nlegacy_active=%s\nlegacy_enabled=%s\n' \
        "$native_definition" "$native_legacy_definition" "$native_environment" \
        "$native_active" "$native_enabled" "$native_legacy_active" "$native_legacy_enabled" > "$native_recovery/state"
    chmod 600 "$native_recovery/state"
    native_snapshot_complete=true
    native_previous_ready=false
    native_previous_port="$(env_file_value "$native_environment" HERDR_RELAY_PORT)"
    native_previous_port="${native_previous_port:-8375}"
    if [ "$native_active" = true ] || [ "$native_legacy_active" = true ]; then
        local previous binary instance
        binary="$(relay_binary)" || return 1
        instance="$(env_file_value "$native_environment" HERDR_RELAY_INSTANCE_ID)"
        if previous="$(curl -fsS --max-time 2 "http://127.0.0.1:$native_previous_port/readyz" 2>/dev/null)" &&
           printf '%s\n' "$previous" | "$binary" verify-readiness "$instance" "" "" "" 2>/dev/null; then
            printf '%s\n' "$previous" > "$native_recovery/previous-ready.json"
            chmod 600 "$native_recovery/previous-ready.json"
            native_previous_ready=true
        fi
    fi
    return 0
}

native_install_restore_files() {
    local index=0 file failed=false
    for file in "$native_definition" "$native_legacy_definition" "$native_environment"; do
        if [ -f "$native_recovery/$index" ]; then
            cp -p "$native_recovery/$index" "$file" || failed=true
        else
            rm -f "$file" || failed=true
        fi
        index=$((index + 1))
    done
    [ "$failed" = false ]
}

native_install_restore_activation() {
    local failed=false label active enabled
    if [ "$native_manager" = systemd ]; then
        systemctl --user daemon-reload || failed=true
        for label in "$native_label" "$native_legacy_label"; do
            active="$native_active"
            enabled="$native_enabled"
            if [ "$label" = "$native_legacy_label" ]; then
                active="$native_legacy_active"
                enabled="$native_legacy_enabled"
            fi
            if [ "$enabled" = true ]; then
                systemctl --user enable "$label" || failed=true
            elif [ -f "$HOME/.config/systemd/user/$label" ]; then
                systemctl --user disable "$label" || failed=true
            fi
            if [ "$active" = true ]; then
                systemctl --user restart "$label" || failed=true
            fi
        done
    else
        if [ "$native_active" = true ] && ! launchctl print "gui/$UID/$native_label" >/dev/null 2>&1; then
            launchctl bootstrap "gui/$UID" "$native_definition" || failed=true
        fi
        if [ "$native_legacy_active" = true ] && ! launchctl print "gui/$UID/$native_legacy_label" >/dev/null 2>&1; then
            launchctl bootstrap "gui/$UID" "$native_legacy_definition" || failed=true
        fi
    fi
    [ "$failed" = false ]
}

native_install_exit() {
    local result=$? failed=false
    trap - EXIT INT TERM
    [ -z "$native_stage" ] || rm -f "$native_stage"
    if [ "$native_snapshot_complete" = false ]; then
        rm -rf "$native_recovery"
        echo "Native installation was not started because the previous state could not be captured." >&2
        exit 1
    fi
    if [ "$native_committed" = true ]; then
        rm -rf "$native_recovery"
        exit "$result"
    fi
    set +e
    if [ "$native_changed" = true ]; then
        if [ "$native_manager" = systemd ]; then
            systemctl --user stop "$native_label" || failed=true
            if [ "$native_enabled" = false ]; then systemctl --user disable "$native_label" || failed=true; fi
        else
            if launchctl print "gui/$UID/$native_label" >/dev/null 2>&1; then
                launchctl bootout "gui/$UID" "$native_definition" || failed=true
            fi
        fi
    fi
    native_install_restore_files || failed=true
    if [ "$native_changed" = true ]; then
        native_install_restore_activation || failed=true
        if [ "$native_active" = true ] || [ "$native_legacy_active" = true ]; then
            if [ "$native_previous_ready" = true ]; then
                local previous
                previous="$(cat "$native_recovery/previous-ready.json")"
                wait_for_relay_release_health "$native_previous_port" 15 1 \
                    "$(json_string_field "$previous" release_version)" "$(json_string_field "$previous" revision)" \
                    "$(json_string_field "$previous" bundle_hash)" "$(json_string_field "$previous" instance)" >/dev/null || failed=true
            else
                echo "Previous runtime readiness was unavailable; restored service identity cannot be confirmed." >&2
                failed=true
            fi
        fi
    fi
    if [ "$failed" = true ]; then
        echo "Native installation rollback is incomplete. Private recovery files remain at $native_recovery; service state requires inspection." >&2
    else
        rm -rf "$native_recovery"
        echo "Native installation failed; previous files and activation were restored." >&2
    fi
    [ "$result" -ne 0 ] || result=1
    exit "$result"
}

native_install_commit() {
    if [ "$native_manager" = systemd ]; then
        if [ -f "$native_legacy_definition" ]; then
            systemctl --user disable --now "$native_legacy_label" || return 1
            rm -f "$native_legacy_definition" || return 1
            systemctl --user daemon-reload || return 1
        fi
    else
        rm -f "$native_legacy_definition" || return 1
    fi
    native_committed=true
}

plist_text() {
    local value="$1"
    value=${value//&/\&amp;}
    value=${value//</\&lt;}
    value=${value//>/\&gt;}
    value=${value//\"/\&quot;}
    value=${value//\'/\&apos;}
    printf '%s' "$value"
}

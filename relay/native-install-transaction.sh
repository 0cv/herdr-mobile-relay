#!/usr/bin/env bash

# Finite label-specific observations. Controller failure is UNKNOWN, never
# absence. No native output is forwarded. This is a cooperative manager-state
# contract, not process discovery or adversarial same-user isolation.
# Reject NUL and over-limit bytes BEFORE command substitution can erase them.
# EOF (read status 1) within the byte bound is the only admitted termination.
# This bounds captured bytes, not producer/filesystem completion time.
native_install_read_bounded() {
    local limit="$1" output="" code=0
    local LC_ALL=C
    case "$limit" in 4096|65536) ;; *) return 1 ;; esac
    IFS= read -r -d '' -n "$((limit + 1))" output || code=$?
    [ "$code" -eq 1 ] && [ "${#output}" -le "$limit" ] || return 1
    printf '%s' "$output"
}

native_install_observe() {
    local label="$1" output line load="" active="" enabled="" count=0 code=0 domain="gui/$UID" disabled probe_status
    local tab=$'\t' job_path="" job_paths=0 job_ambiguous=false
    local LC_ALL=C
    # native_observed_origin is launchd-only evidence taken from this same observation; it is
    # empty (origin unknown) unless exactly one job-level path line is found below.
    NATIVE_STATE=unknown NATIVE_ENABLED=unknown native_observed_origin=""
    if [ "$native_manager" = systemd ]; then
        output="$(
            LC_ALL=C systemctl --user show "$label" --property=LoadState --property=ActiveState --property=UnitFileState 2>/dev/null | native_install_read_bounded 4096
            probe_status=( "${PIPESTATUS[@]}" )
            [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
        )" || return 1
        while IFS= read -r line; do
            count=$((count + 1))
            case "$line" in
                LoadState=*) [ -z "$load" ] || return 1; load="${line#*=}" ;;
                ActiveState=*) [ -z "$active" ] || return 1; active="${line#*=}" ;;
                UnitFileState=*) [ -z "$enabled" ] || return 1; enabled="${line#*=}" ;;
                *) return 1 ;;
            esac
        done <<< "$output"
        [ "$count" -eq 3 ] || return 1
        if [ "$load" = not-found ] && [ "$active" = inactive ] && [ -z "$enabled" ]; then
            NATIVE_STATE=absent NATIVE_ENABLED=false; return 0
        fi
        [ "$load" = loaded ] || return 1
        case "$active" in active|inactive) ;; *) return 1 ;; esac
        case "$enabled" in enabled) enabled=true ;; disabled) enabled=false ;; *) return 1 ;; esac
        NATIVE_STATE="$active" NATIVE_ENABLED="$enabled"
        return 0
    fi
    output="$(
        LC_ALL=C launchctl print "$domain" 2>/dev/null | native_install_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
    )" || return 1
    output="$(
        LC_ALL=C launchctl print "$domain/$label" 2>&1 | native_install_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[1]}" -eq 0 ] || exit 1
        # Frame genuine producer status separately, outside bounded text.
        printf '\n%s' "${probe_status[0]}"
    )" || return 1
    code="${output##*$'\n'}"; output="${output%$'\n'*}"
    # Preserve the original substitution's trailing-newline normalization,
    # now only AFTER binary admission and producer-status separation.
    while [ "${output%$'\n'}" != "$output" ]; do output="${output%$'\n'}"; done
    if [ "$code" -ne 0 ]; then
        # Only the exact label/domain not-found diagnostic is absence. A bad
        # domain, malformed output or any other nonzero result remains unknown.
        [ "$output" = "Could not find service \"$label\" in domain for user gui: $UID" ] || return 1
        active=absent
    else
        case "$output" in "$domain/$label = {"$'\n'*|"$domain/$label = {}") ;; *) return 1 ;; esac
        # A loaded waiting job can launch again: only bootout/observed absence
        # establishes launchd quiescence, regardless of current PID/state.
        active=loaded
        # Origin evidence comes from this SAME captured text: no extra launchctl query.
        # Grammar: exactly ONE job-level line, a single TAB then `path = <value>`.
        # Any other record naming `path` (keyword ends at EOF or a non-identifier
        # character) is ambiguous, including path=, whitespace variants and nested
        # records. Duplicates likewise leave native_observed_origin empty. Observe
        # stays as lenient as before (a loaded body without a path line is still loaded);
        # only native_install_origin_ok, called by native_install_begin, acts on it.
        while IFS= read -r line; do
            case "$line" in
                "${tab}path = "*) job_paths=$((job_paths + 1)); job_path="${line#"${tab}path = "}" ;;
                *)
                    line="${line#"${line%%[![:space:]]*}"}"
                    case "$line" in path|path[![:alnum:]_]*) job_ambiguous=true ;; esac ;;
            esac
        done <<< "$output"
        if [ "$job_paths" -eq 1 ] && [ "$job_ambiguous" = false ]; then native_observed_origin="$job_path"; fi
    fi
    disabled="$(
        LC_ALL=C launchctl print-disabled "$domain" 2>/dev/null | native_install_read_bounded 65536
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
    )" || return 1
    case "$disabled" in "disabled services = {"*'}') ;; *) return 1 ;; esac
    enabled=true
    count=0
    while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}"
        case "$line" in
            "\"$label\" => true") enabled=false; count=$((count + 1)) ;;
            "\"$label\" => false") enabled=true; count=$((count + 1)) ;;
            *"\"$label\""*) return 1 ;;
        esac
    done <<< "$disabled"
    [ "$count" -le 1 ] || return 1
    NATIVE_STATE="$active" NATIVE_ENABLED="$enabled"
    return 0
}

# Origin of a LOADED same-label service. Observation by state alone cannot tell
# Herdr's own definition from a vendor or other foreign one holding the same label,
# and quiesce stops, disables or boots out whatever holds it. native_install_begin
# therefore requires the loaded definition to be Herdr's own expected path BEFORE the
# snapshot is marked complete and before any manager mutation. Anything unestablished
# (unknown, empty, duplicate, malformed, NUL, over-limit, query failure) refuses.
# No manager-reported text is ever forwarded. The uninstall path applies the same
# FragmentPath/DropInPaths rule in relay/common.sh service_removal_systemd_configuration.
#
# systemd: one additive, bounded query, issued only for a label that is not absent.
# Exactly the two lines FragmentPath= and DropInPaths=, each once and nothing else.
# The fragment must equal the expected definition path and there must be no drop-ins
# (a drop-in is foreign configuration of the same label). NeedDaemonReload is not
# required: install runs daemon-reload itself and origin is the only concern here.
native_install_origin_systemd() {
    local label="$1" expected="$2" output line fragment="" overlays="" count=0 f=0 d=0 probe_status
    local LC_ALL=C
    output="$(
        LC_ALL=C systemctl --user show "$label" --property=FragmentPath --property=DropInPaths 2>/dev/null | native_install_read_bounded 4096
        probe_status=( "${PIPESTATUS[@]}" )
        [ "${probe_status[0]}:${probe_status[1]}" = 0:0 ] || exit 1
    )" || return 1
    while IFS= read -r line; do
        count=$((count + 1))
        case "$line" in
            FragmentPath=*) f=$((f + 1)); fragment="${line#*=}" ;;
            DropInPaths=*) d=$((d + 1)); overlays="${line#*=}" ;;
            *) return 1 ;;
        esac
    done <<< "$output"
    [ "$count:$f:$d" = 2:1:1 ] || return 1
    [ -n "$fragment" ] && [ "$fragment" = "$expected" ] && [ -z "$overlays" ]
}

# launchd: native_observed_origin, parsed by native_install_observe from the same label output
# it already captures (no second launchctl query), must equal the expected plist path.
# Call it immediately after native_install_observe for that label: it reads the
# NATIVE_STATE and native_observed_origin that observation just set.
native_install_origin_ok() {
    local label="$1" expected="$2" established=false
    [ "$NATIVE_STATE" != absent ] || return 0
    if [ "$native_manager" = systemd ]; then
        if native_install_origin_systemd "$label" "$expected"; then established=true; fi
    elif [ -n "$native_observed_origin" ] && [ "$native_observed_origin" = "$expected" ]; then
        established=true
    fi
    if [ "$established" = true ]; then return 0; fi
    echo "Native installation refused: the loaded service for $label was not established to be defined by Herdr's own definition path; no service or definition was changed." >&2
    return 1
}

native_install_begin() {
    native_manager="$1" native_definition="$2" native_legacy_definition="$3" native_environment="$4"
    native_label="$5" native_legacy_label="$6"
    native_resources=("$native_definition" "$native_legacy_definition" "$native_environment")
    [ -z "${7:-}" ] || native_resources+=("$7")
    native_stage="" native_snapshot_complete=false native_published=false native_quiesced=false
    native_activation_attempted=false native_committed=false native_previous_ready=false
    native_active=false native_enabled=false native_legacy_active=false native_legacy_enabled=false
    local root="${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay/recovery" file index=0 previous binary instance identifier
    local identities=() identity_count=0 identity_index
    native_selected_pointer_disjoint "${NATIVE_SELECTED_POINTER:-}" "${native_resources[@]}" || return 1
    for file in "${native_resources[@]}"; do
        identifier="$(canonical_file_path "$file")" || return 1
        # Stock Bash 3.2 nounset cannot expand an empty array. Only read
        # indices that the explicit count confirms have been assigned.
        for ((identity_index=0; identity_index<identity_count; identity_index++)); do
            [ "${identities[identity_index]}" != "$identifier" ] || return 1
        done
        identities[identity_count]="$identifier"
        identity_count=$((identity_count + 1))
    done
    # No selected-file mutation until every snapshot and both state queries
    # succeed. Existing directories/executables are not chmodded as a shortcut.
    (umask 077; mkdir -p "$root") || return 1
    native_recovery="$(mktemp -d "$root/herdr-native-recovery.XXXXXX")" || return 1
    chmod 700 "$native_recovery" || { rm -rf "$native_recovery"; return 1; }
    trap native_install_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    for file in "${native_resources[@]}"; do
        native_resource_parents_safe "$file" || return 1
        if [ -e "$file" ] || [ -L "$file" ]; then
            native_mutable_file_safe "$file" || return 1
            cp -p "$file" "$native_recovery/$index" || return 1
        fi
        index=$((index + 1))
    done
    native_install_observe "$native_label" || return 1
    native_install_origin_ok "$native_label" "$native_definition" || return 1
    native_enabled="$NATIVE_ENABLED"
    case "$NATIVE_STATE" in active|loaded) native_active=true ;; esac
    native_install_observe "$native_legacy_label" || return 1
    native_install_origin_ok "$native_legacy_label" "$native_legacy_definition" || return 1
    native_legacy_enabled="$NATIVE_ENABLED"
    case "$NATIVE_STATE" in active|loaded) native_legacy_active=true ;; esac
    index=0
    for file in "${native_resources[@]}"; do
        printf '%s=' "$index" && shell_quote_value "$file" || return 1
        index=$((index + 1))
    done > "$native_recovery/resources" || return 1
    chmod 600 "$native_recovery/resources" || return 1
    printf 'active=%s\nenabled=%s\nlegacy_active=%s\nlegacy_enabled=%s\n' \
        "$native_active" "$native_enabled" "$native_legacy_active" "$native_legacy_enabled" > "$native_recovery/state" || return 1
    chmod 600 "$native_recovery/state" || return 1
    native_snapshot_complete=true
    literal_env_scan "$native_environment" || return 1
    literal_env_get HERDR_RELAY_PORT; native_previous_port="${LITERAL_VALUE:-8375}"
    literal_env_get HERDR_RELAY_INSTANCE_ID; instance="$LITERAL_VALUE"
    if [ "$native_active" = true ] || [ "$native_legacy_active" = true ]; then
        binary="$(relay_binary)" || return 1
        if previous="$(curl -fsS --max-time 2 "http://127.0.0.1:$native_previous_port/readyz" 2>/dev/null)" &&
            printf '%s\n' "$previous" | "$binary" verify-readiness "$instance" '' '' '' >/dev/null 2>&1; then
            printf '%s\n' "$previous" > "$native_recovery/previous-ready.json" || return 1
            chmod 600 "$native_recovery/previous-ready.json" || return 1
            native_previous_ready=true
        fi
    fi
    return 0
}

native_install_label_quiet() {
    native_install_observe "$1" || return 1
    if [ "$native_manager" = launchd ]; then [ "$NATIVE_STATE" = absent ]; return $?; fi
    case "$NATIVE_STATE" in absent|inactive) [ "$NATIVE_ENABLED" = false ] ;; *) return 1 ;; esac
}

native_install_quiesce() {
    local label attempt
    for label in "$native_label" "$native_legacy_label"; do
        native_install_observe "$label" || return 1
        if [ "$native_manager" = systemd ]; then
            if [ "$NATIVE_STATE" != absent ]; then
                systemctl --user stop "$label" >/dev/null 2>&1 || true
                systemctl --user disable "$label" >/dev/null 2>&1 || true
            fi
        elif [ "$NATIVE_STATE" != absent ]; then
            launchctl bootout "gui/$UID/$label" >/dev/null 2>&1 || true
        fi
        for ((attempt=0; attempt<10; attempt++)); do
            native_install_observe "$label" || return 1
            if native_install_label_quiet "$label"; then break; fi
            [ "$attempt" -eq 9 ] || sleep 0.1 || return 1
        done
        [ "$attempt" -lt 10 ] || return 1
    done
    # Stop return codes are not authority. Re-observe BOTH affected labels.
    native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label"
}

native_install_replace() {
    local source="$1" destination="$2" directory
    directory="$(dirname "$destination")" || return 1
    native_stage="$(mktemp "$directory/.herdr-service.XXXXXX")" || return 1
    if ! cp -p "$source" "$native_stage" ||
        ! native_install_label_quiet "$native_label" || ! native_install_label_quiet "$native_legacy_label" ||
        ! mv -f "$native_stage" "$destination"; then
        rm -f "$native_stage"; native_stage=""; return 1
    fi
    native_stage=""
    return 0
}

native_install_publish() {
    local definition_source="$1"
    # Set before any stop attempt so even partial manager progress is handled.
    native_quiesced=true
    native_install_quiesce || return 1
    (umask 077; mkdir -p "$(dirname "$native_environment")" "$(dirname "$native_definition")") || return 1
    native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label" || return 1
    native_published=true
    if [ -n "$NATIVE_SIDECAR" ]; then
        native_install_replace "$native_recovery/candidate.token" "$NATIVE_SIDECAR" || return 1
    fi
    native_install_replace "$native_recovery/candidate.env" "$native_environment" || return 1
    native_install_replace "$definition_source" "$native_definition" || return 1
    return 0
}

native_install_activate() {
    native_activation_attempted=true
    native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label" || return 1
    if [ "$native_manager" = systemd ]; then
        systemctl --user daemon-reload && systemctl --user enable "$native_label" && systemctl --user restart "$native_label" || return 1
    else
        launchctl enable "gui/$UID/$native_label" && launchctl bootstrap "gui/$UID" "$native_definition" || return 1
    fi
    native_install_observe "$native_label" || return 1
    case "$NATIVE_STATE" in active|loaded) return 0 ;; *) return 1 ;; esac
}

native_install_restore_files() {
    local file index=0
    native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label" || return 1
    for file in "${native_resources[@]}"; do
        native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label" || return 1
        if [ -f "$native_recovery/$index" ]; then
            native_install_replace "$native_recovery/$index" "$file" || return 1
        else
            rm -f "$file" || return 1
        fi
        index=$((index + 1))
    done
    return 0
}

native_install_restore_activation() {
    local label enabled active previous
    # Any restoration failure is handled by the caller BEFORE this function.
    native_install_label_quiet "$native_label" && native_install_label_quiet "$native_legacy_label" || return 1
    # No prior activation without the readiness evidence saved at begin. Return
    # BEFORE any manager action (reload/enable/disable/restart/bootstrap) so an
    # unverified service is never started: the services stay quiesced and the
    # caller retains the recovery evidence.
    if [ "$native_active" = true ] || [ "$native_legacy_active" = true ]; then
        [ "$native_previous_ready" = true ] || return 1
    fi
    if [ "$native_manager" = systemd ]; then systemctl --user daemon-reload || return 1; fi
    for label in "$native_label" "$native_legacy_label"; do
        enabled="$native_enabled"; [ "$label" != "$native_legacy_label" ] || enabled="$native_legacy_enabled"
        if [ "$native_manager" = systemd ]; then
            if [ "$enabled" = true ]; then systemctl --user enable "$label" || return 1; fi
        else
            if [ "$enabled" = true ]; then launchctl enable "gui/$UID/$label" || return 1;
            else launchctl disable "gui/$UID/$label" || return 1; fi
        fi
    done
    for label in "$native_label" "$native_legacy_label"; do
        active="$native_active"; [ "$label" != "$native_legacy_label" ] || active="$native_legacy_active"
        [ "$active" = true ] || continue
        native_install_observe "$label" || return 1
        if [ "$native_manager" = systemd ]; then
            [ "$NATIVE_STATE" = inactive ] || return 1
            systemctl --user restart "$label" || return 1
        else
            [ "$NATIVE_STATE" = absent ] || return 1
            if [ "$label" = "$native_label" ]; then launchctl bootstrap "gui/$UID" "$native_definition" || return 1;
            else launchctl bootstrap "gui/$UID" "$native_legacy_definition" || return 1; fi
        fi
        native_install_observe "$label" || return 1
        case "$NATIVE_STATE" in active|loaded) ;; *) return 1 ;; esac
    done
    if [ "$native_active" = true ] || [ "$native_legacy_active" = true ]; then
        [ "$native_previous_ready" = true ] || return 1
        previous="$(cat "$native_recovery/previous-ready.json")" || return 1
        wait_for_relay_release_health "$native_previous_port" 15 1 \
            "$(json_string_field "$previous" release_version)" "$(json_string_field "$previous" revision)" \
            "$(json_string_field "$previous" bundle_hash)" "$(json_string_field "$previous" instance)" >/dev/null || return 1
    fi
    return 0
}

native_install_exit() {
    local result=$? failed=false
    trap - EXIT
    trap ':' INT TERM
    set +e
    [ -z "$native_stage" ] || rm -f "$native_stage"
    if [ "$native_committed" = true ]; then rm -rf "$native_recovery"; exit "$result"; fi
    if [ "$native_snapshot_complete" = true ] && [ "$native_quiesced" = true ]; then
        # NO restore/remove/reactivate after active/unknown quiescence. A
        # failed file restoration likewise forbids all prior reactivation.
        if ! native_install_quiesce; then failed=true;
        elif [ "$native_published" = true ] && ! native_install_restore_files; then failed=true;
        elif ! native_install_restore_activation; then failed=true; fi
    fi
    if [ "$failed" = true ]; then
        echo 'Native installation rollback is incomplete; current resources and private recovery evidence are retained.' >&2
    else
        rm -rf "$native_recovery" || failed=true
        echo 'Native installation failed; no replacement remains. Prior activation restoration requires the recorded readiness evidence.' >&2
    fi
    [ "$result" -ne 0 ] || result=1
    exit "$result"
}

native_install_commit() {
    # Commit removes only a freshly observed inactive/disabled or absent
    # legacy definition. It cannot infer safe removal from disable's status.
    native_install_label_quiet "$native_legacy_label" || return 1
    rm -f "$native_legacy_definition" || return 1
    if [ "$native_manager" = systemd ]; then systemctl --user daemon-reload || return 1; fi
    native_committed=true
    return 0
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

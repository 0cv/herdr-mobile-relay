#!/usr/bin/env bash
# Full uninstall: remove the relay binary, releases, config, cache, and service.
set +x
set -euo pipefail
[ "$#" -eq 0 ] || { echo 'Invalid full-uninstall arguments.' >&2; exit 2; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

RELEASE_ROOT="$(relay_release_root)"
BIN_LINK="${HERDR_RELAY_BIN_DIR:-$HOME/.local/bin}/herdr-mobile-relay"

# Controller routing and deletion bookkeeping are never loaded from config.
shared_progress=false
shared_attempted=false
# Linux only: the relay's own mount-confinement guard (full_uninstall_mount_*
# below). Darwin never sets MOUNT_GUARD, stages nothing and never calls it.
MOUNT_GUARD=false
GUARD_BIN=""
GUARD_DIR=""
GUARD_HOME=""
full_uninstall_fail() {
    echo 'Full uninstall incomplete; remaining shared roots and recovery were retained.' >&2
    if [ "$shared_progress" = true ]; then
        echo 'Earlier verified shared-file batches completed; no restoration or activation was attempted.' >&2
    fi
    if [ "$shared_attempted" = true ]; then
        echo 'Checked shared-file batch failed; partial shared effects are possible.' >&2
    elif [ "$shared_progress" = false ]; then
        echo 'Shared release/config/cache/link roots retained; native removal may be partial.' >&2
    fi
    exit "${1:-1}"
}
trap 'full_uninstall_fail 130' INT
trap 'full_uninstall_fail 143' TERM
require_user_service_context || full_uninstall_fail
case "$(uname -s)" in
    Linux) MANAGER=systemd; NATIVE_REMOVER="$SCRIPT_DIR/uninstall-systemd-user-service.sh"; MOUNT_GUARD=true ;;
    Darwin) MANAGER=launchd; NATIVE_REMOVER="$SCRIPT_DIR/uninstall-service.sh" ;;
    *) echo 'Unsupported full-uninstall platform.' >&2; full_uninstall_fail ;;
esac
# The native delegate is required even with absent definitions. This is a
# cooperative owned-source admission, not authentication against same-UID edits.
service_removal_env_safe "$NATIVE_REMOVER" && [ -f "$NATIVE_REMOVER" ] || {
    echo 'Native service remover is missing or unsafe.' >&2; full_uninstall_fail;
}
FALLBACK_ENV="${HERDR_RELAY_ENV:-${HERDR_PLUGIN_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay}/relay.env}"
RELAY_ENV_FILE="$(HERDR_RELAY_ENV="$FALLBACK_ENV" service_environment_file "$SCRIPT_DIR")" || {
    echo 'Full uninstall cannot safely select the service configuration.' >&2; full_uninstall_fail;
}
[ -z "${HERDR_RELAY_ENV:-}" ] || [ "$HERDR_RELAY_ENV" = "$RELAY_ENV_FILE" ] || full_uninstall_fail
CONFIG_DIR="$(dirname "$RELAY_ENV_FILE")" || full_uninstall_fail
[ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ] || [ "$HERDR_PLUGIN_CONFIG_DIR" = "$CONFIG_DIR" ] || full_uninstall_fail
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/herdr-mobile-relay"

# Small caller-local data admission: finite transport/registration fields only.
# The unchanged native delegate independently admits credential/resource roles.
full_uninstall_admit() {
    local index key mode ambient saved pointer label destination
    service_removal_env_safe "$RELAY_ENV_FILE" && literal_env_scan "$RELAY_ENV_FILE" || return 1
    for ((index=0; index<${#LITERAL_KEYS[@]}; index++)); do
        key="${LITERAL_KEYS[index]}"
        case "$key" in
            HERDR_BIN|HERDR_BIN_PATH|HERDR_PLUTIL|HERDR_RELEASE_ROOT|HERDR_WEB_ROOT|HERDR_RELAY_BIN|HERDR_RELAY_BIN_DIR|HERDR_APP_DEPLOY_NPX|HERDR_APP_DEPLOY_NODE_DIR|HERDR_DEV_*|HERDR_CLI_*|HERDR_STABLE_*|HERDR_SETUP_*) return 1 ;;
            HERDR_RELAY_ENV) [ "${LITERAL_VALUES[index]}" = "$RELAY_ENV_FILE" ] || return 1 ;;
            HERDR_PLUGIN_CONFIG_DIR) [ "${LITERAL_VALUES[index]}" = "$CONFIG_DIR" ] || return 1 ;;
            HERDR_*|CLOUDFLARED_*|TUNNEL_ORIGIN_CERT|GH_TOKEN|GITHUB_TOKEN|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|NO_PROXY|http_proxy|https_proxy|all_proxy|no_proxy) ;;
            *) return 1 ;;
        esac
    done
    # Admit selected credential/configuration roles BEFORE explicit unpublish,
    # without opening token content or granting a prior route disposition.
    literal_env_get HERDR_GITHUB_TOKEN_FILE; pointer="$LITERAL_VALUE"
    for label in $(service_removal_labels "$MANAGER"); do
        destination="$(service_removal_path "$MANAGER" "$label")" || return 1
        service_removal_selected_disjoint "$RELAY_ENV_FILE" "$destination" || return 1
        native_selected_pointer_disjoint "$pointer" "$RELAY_ENV_FILE" "$destination" || return 1
        for key in CLOUDFLARED_CONFIG TUNNEL_ORIGIN_CERT; do
            literal_env_get "$key"
            service_removal_selected_disjoint "$LITERAL_VALUE" "$destination" || return 1
        done
    done
    literal_env_get HERDR_RELAY_TRANSPORT; mode="$LITERAL_VALUE"
    mode="${mode#"${mode%%[![:space:]]*}"}"; mode="${mode%"${mode##*[![:space:]]}"}"
    mode="$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]')" || return 1
    ambient="${HERDR_RELAY_TRANSPORT:-}"
    ambient="${ambient#"${ambient%%[![:space:]]*}"}"; ambient="${ambient%"${ambient##*[![:space:]]}"}"
    ambient="$(printf '%s' "$ambient" | tr '[:upper:]' '[:lower:]')" || return 1
    case "$mode" in ''|cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) ;; *) return 1 ;; esac
    case "$ambient" in ''|cloudflare|gateway|tailscale|tailscale-cli|tailscale-external) ;; *) return 1 ;; esac
    [ -z "$mode" ] || [ -z "$ambient" ] || [ "$mode" = "$ambient" ] || return 1
    TRANSPORT="${mode:-$ambient}"
    literal_env_get HERDR_GATEWAY_URL
    if [ -n "$LITERAL_VALUE" ]; then
        case "$TRANSPORT" in ''|gateway) TRANSPORT=gateway ;; *) return 1 ;; esac
    fi
    CLI_STATE_ROOT="${HERDR_TAILSCALE_CLI_STATE_ROOT:-${XDG_STATE_HOME:-$HOME/.local/state}/herdr-mobile-relay/tailscale-cli-registration}"
    # Match tailscale-cli.sh: only the registration default follows XDG_STATE_HOME.
    CLI_COORDINATION_ROOT="${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-$HOME/.local/state/herdr-mobile-relay/tailscale-cli-coordination}"
    for key in HERDR_TAILSCALE_CLI_STATE_ROOT HERDR_TAILSCALE_CLI_COORDINATION_ROOT; do
        literal_env_get "$key"; saved="$LITERAL_VALUE"
        if [ "$LITERAL_FOUND" = true ]; then
            case "$saved" in /|''|*[$'\001'-$'\037'$'\177']*) return 1 ;; /*) ;; *) return 1 ;; esac
            service_removal_env_safe "$saved/registration-root-admission" || return 1
            if [ "$key" = HERDR_TAILSCALE_CLI_STATE_ROOT ]; then
                [ -z "${HERDR_TAILSCALE_CLI_STATE_ROOT:-}" ] || [ "$saved" = "$HERDR_TAILSCALE_CLI_STATE_ROOT" ] || return 1
                CLI_STATE_ROOT="$saved"
            else
                [ -z "${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-}" ] || [ "$saved" = "$HERDR_TAILSCALE_CLI_COORDINATION_ROOT" ] || return 1
                CLI_COORDINATION_ROOT="$saved"
            fi
        fi
    done
    for saved in "$CLI_STATE_ROOT" "$CLI_COORDINATION_ROOT"; do
        case "$saved" in /|''|*[$'\001'-$'\037'$'\177']*) return 1 ;; /*) ;; *) return 1 ;; esac
        service_removal_env_safe "$saved/registration-root-admission" || return 1
    done
    return 0
}
full_uninstall_admit || { echo 'Full uninstall configuration admission refused.' >&2; full_uninstall_fail; }
# Pin selection through the delegate. No selected assignment is exported.
export HERDR_RELAY_ENV="$RELAY_ENV_FILE"
full_uninstall_quiet() {
    local label
    for label in $(service_removal_labels "$MANAGER"); do
        service_removal_quiet "$MANAGER" "$label" || {
            echo 'Fresh both-label quiescence is unavailable; shared removal stopped.' >&2; return 1;
        }
    done
    return 0
}
TAILSCALE_SESSION_FILE="$CONFIG_DIR/tailscale-session.env"
EXTERNAL_SESSION_FILE="$CONFIG_DIR/tailscale-external-session.env"
if [ -e "$TAILSCALE_SESSION_FILE" ] || [ -L "$TAILSCALE_SESSION_FILE" ]; then
    echo "✗ A managed foreground Tailscale Serve session is recorded at $TAILSCALE_SESSION_FILE." >&2
    echo "  Stop that pane and verify managed cleanup before uninstalling." >&2
    exit 1
fi
if [ -e "$EXTERNAL_SESSION_FILE" ] || [ -L "$EXTERNAL_SESSION_FILE" ]; then
    echo "✗ An operator-owned HTTPS Serve relay is running at $EXTERNAL_SESSION_FILE." >&2
    echo "  Stop the Herdr pane before uninstalling; the operator-owned Tailscale ingress is not removed." >&2
    exit 1
fi

# Canonicalize a path, resolving symlinks. Returns empty if path does not exist.
canonicalize() {
    local target="$1"
    if [ -L "$target" ]; then
        readlink -f "$target" 2>/dev/null || realpath "$target" 2>/dev/null || printf '%s\n' "$target"
    elif [ -d "$target" ]; then
        (cd "$target" && pwd -P)
    elif [ -e "$target" ]; then
        local dir base
        dir="$(dirname "$target")" || return 1
        base="$(basename "$target")" || return 1
        dir="$(cd "$dir" && pwd -P)" || return 1
        printf '%s/%s\n' "$dir" "$base"
    else
        printf '%s\n' "$target"
    fi
}

# Verify that the exact resolved target stays under HOME and contains relay
# markers. This supports explicit HERDR_RELEASE_ROOT and XDG paths without
# allowing an arbitrary directory to be removed.
verify_removal_target() {
    local canonical="$1" label="$2"
    local canonical_home expected=""

    canonical_home="$(canonicalize "$HOME")" || return 1
    case "$label" in
        releases) expected="$(canonicalize "$RELEASE_ROOT")" || return 1 ;;
        config/state) expected="$(canonicalize "$CONFIG_DIR")" || return 1 ;;
        cache) expected="$(canonicalize "$CACHE_DIR")" || return 1 ;;
    esac
    case "$canonical" in
        "$canonical_home"/*) ;;
        *)
            echo "  REFUSING to remove $label outside HOME: $canonical" >&2
            return 1
            ;;
    esac
    if [ -z "$expected" ] || [ "$canonical" != "$expected" ]; then
        echo "  REFUSING to remove $label: $canonical" >&2
        echo "  Path does not match the resolved herdr-mobile-relay $label root." >&2
        return 1
    fi

    # Require a dedicated sentinel whose recorded canonical root matches the
    # deletion target. Generic relay-looking filenames never authorize removal.
    if [ -d "$canonical" ]; then
        sentinel="$canonical/.herdr-mobile-relay-installation"
        if ! private_owned_file "$sentinel" ||
           ! grep -Fx 'product=herdr-mobile-relay' "$sentinel" >/dev/null ||
           ! grep -Fx "root=$canonical" "$sentinel" >/dev/null; then
            echo "  REFUSING to remove $label: $canonical" >&2
            echo "  Directory has no matching herdr-mobile-relay installation sentinel." >&2
            return 1
        fi
    fi

    return 0
}

preflight_removal_target() {
    local target="$1" label="$2"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return
    fi
    # A symlink is removed as a link only; its destination is never traversed.
    if [ -L "$target" ]; then
        return
    fi
    local canonical
    canonical="$(canonicalize "$target")" || return 1
    verify_removal_target "$canonical" "$label"
}

# Refuse an unowned release inode before route, native, link or shared effects.
# find defaults to -P: a current/release symlink is never followed. A failed
# traversal or unsupported link-count predicate is uncertainty, not absence.
preflight_release_links() {
    local canonical="$1" linked
    [ -d "$canonical" ] && [ ! -L "$canonical" ] || return 1
    linked="$(find -P "$canonical" -type f -links +1 -print -quit)" || return 1
    if [ -n "$linked" ]; then
        echo '  REFUSING to remove releases with multiply-linked files.' >&2
        return 1
    fi
}

# Linux mount confinement. The installed binary lives in the releases root,
# which is deleted before the config and cache roots are rechecked, so a private
# 0700 copy, compared byte for byte with the installed binary, is staged once
# and removed when the script exits (normal exit, `exit N`, the INT/TERM handlers
# above, bash's own exit on a terminating signal; SIGKILL cannot be handled). The
# EXIT trap is installed BEFORE the directory exists, and a copy that cannot be
# removed is reported and turns a would-be success into a failure. It is never
# found through PATH or HERDR_RELAY_BIN, only a real (non-symlink) releases root
# can supply it, it is staged outside every selected root, and a copy that cannot
# be made, compared or executed refuses. NOTHING is written until the installed
# relay, run in place, has proved all three roots: a TMPDIR that is a bind-mount
# alias of a root (or of a subtree of one) is not visible to a path comparison, but
# the proof refuses every such alias, so the copy is never staged through one.
full_uninstall_guard_cleanup() {
    local status=$?
    [ -n "$GUARD_DIR" ] || return 0
    rm -rf -- "$GUARD_DIR" || true
    [ -e "$GUARD_DIR" ] || [ -L "$GUARD_DIR" ] || return 0
    echo "Private mount-guard copy could not be removed: $GUARD_DIR" >&2
    [ "$status" -ne 0 ] || status=1
    exit "$status"
}

# Physical location of root entry $1 itself (a final symlink is not followed): its
# nearest existing ancestor directory, resolved, plus the names below it. The
# tolerant guard then proves absence, or the link, without traversing it.
full_uninstall_root_entry() {
    local parent suffix resolved
    parent="$(dirname "$1")" || return 1
    suffix="/$(basename "$1")" || return 1
    while [ ! -d "$parent" ]; do
        suffix="/$(basename "$parent")$suffix"
        parent="$(dirname "$parent")" || return 1
    done
    # `cd` and `pwd -P` resolve every symlink (a parent may itself be one) with no lexical fallback.
    resolved="$(cd "$parent" && pwd -P)" || return 1
    printf '%s%s\n' "${resolved%/}" "$suffix"
}

# Succeeds when physical path $1 is neither one of the physical roots $2... nor beneath one.
full_uninstall_outside_roots() {
    local path="$1" root
    shift
    for root in "$@"; do
        case "$path" in "$root"|"$root"/*) return 1 ;; esac
    done
    return 0
}

# $@ are the physical selected roots. The staged copy must survive the deletion of any of them.
full_uninstall_guard_stage() {
    local active="$RELEASE_ROOT/current/herdr-mobile-relay" active_real release_real tmp_base staged
    [ -z "$GUARD_BIN" ] || return 0
    # A symlinked releases root is removed as a link only and never traversed, so it cannot supply an executable.
    # `current` may be a symlink inside a real root, but the binary must resolve to a file beneath that root.
    [ -d "$RELEASE_ROOT" ] && [ ! -L "$RELEASE_ROOT" ] || return 1
    release_real="$(canonicalize "$RELEASE_ROOT")" || return 1
    active_real="$(canonicalize "$active")" || return 1
    case "$active_real" in "$release_real"/*) ;; *) return 1 ;; esac
    [ -f "$active_real" ] && [ ! -L "$active" ] && [ ! -L "$active_real" ] && [ -x "$active_real" ] || return 1
    command -v mktemp >/dev/null 2>&1 && command -v install >/dev/null 2>&1 &&
        command -v cmp >/dev/null 2>&1 || return 1
    GUARD_HOME="$(cd "$HOME" && pwd -P)" || return 1
    # The physical temp base, from `cd` and `pwd -P` alone: a symlinked TMPDIR is followed, and a failed resolution refuses
    # (canonicalize would fall back to the lexical path). It is checked before anything is created, then the directory itself.
    tmp_base="${TMPDIR:-/tmp}"
    [ -d "$tmp_base" ] || return 1
    tmp_base="$(cd "$tmp_base" && pwd -P)" || return 1
    full_uninstall_outside_roots "$tmp_base" "$@" || return 1
    # The proof before any write: the installed relay itself, by its resolved absolute path, checks all three roots. This is
    # what refuses a TMPDIR that aliases a root through a bind mount (its physical name is outside every root), so no copy is
    # created, and no cleanup `rm -rf` can later act, through such an alias. The copy below is then proved again by running it.
    full_uninstall_guard_exec "$active_real" "$@" || return 1
    trap full_uninstall_guard_cleanup EXIT
    GUARD_DIR="$(mktemp -d "$tmp_base/herdr-uninstall-guard.XXXXXX")" || return 1
    staged="$(cd "$GUARD_DIR" && pwd -P)" || return 1
    full_uninstall_outside_roots "$staged" "$@" || return 1
    install -m 0700 "$active_real" "$staged/relay" || return 1
    cmp -s "$active_real" "$staged/relay" || return 1
    GUARD_BIN="$staged/relay"
}

# Run guard binary $1 (an absolute path) on the physical roots $2...; any nonzero status, output or crash refuses.
full_uninstall_guard_exec() {
    local binary="$1"
    shift
    HOME="$GUARD_HOME" "$binary" __private-uninstall-mount-roots "$@" >/dev/null 2>&1
}

# The staged private copy, on physical roots.
full_uninstall_guard_run() {
    full_uninstall_guard_exec "$GUARD_BIN" "$@"
}

# Before any route, native or shared effect: all three selected roots go to the
# guard. A directory root is proved in full; an absent root has nothing to prove
# and a symlink root is removed as a link only, never traversed, so neither can
# make another root unprovable. The guard binary, the staging and the guard must
# all work even when no root is a directory: an unavailable proof refuses. The
# proof runs twice: first the installed relay in place (before anything is
# written), then the staged copy (which must also execute).
full_uninstall_mount_preflight() {
    local target root
    set --
    for target in "$RELEASE_ROOT" "$CONFIG_DIR" "$CACHE_DIR"; do
        if [ -L "$target" ] || [ ! -e "$target" ]; then
            root="$(full_uninstall_root_entry "$target")" || return 1
        else
            root="$(canonicalize "$target")" || return 1
        fi
        set -- "$@" "$root"
    done
    full_uninstall_guard_stage "$@" && full_uninstall_guard_run "$@"
}

# Immediately before a destructive step on directory root $1: the same proof for
# that root alone, from a fresh mount inventory. A refusal precedes the step, so
# it is not a partially applied shared batch.
full_uninstall_mount_recheck() {
    [ "$MOUNT_GUARD" = true ] || return 0
    full_uninstall_guard_run "$1" && return 0
    echo "  REFUSING to change $1: its mount confinement can no longer be proved." >&2
    shared_attempted=false
    return 1
}

safe_remove_dir() {
    local target="$1" label="$2"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return 0
    fi
    # If target is a symlink, remove only the link, never the resolved target.
    if [ -L "$target" ]; then
        full_uninstall_quiet || return 1
        shared_attempted=true
        rm -f "$target" || return 1
        shared_attempted=false
        shared_progress=true
        echo "  Removed $label symlink: $target"
        return 0
    fi
    local canonical
    canonical="$(canonicalize "$target")" || return 1
    if ! verify_removal_target "$canonical" "$label"; then
        return 1
    fi
    full_uninstall_quiet || return 1
    shared_attempted=true
    if [ "$label" = "releases" ]; then
        preflight_release_links "$canonical" || return 1
        full_uninstall_mount_recheck "$canonical" || return 1
        # Only directory permissions are needed to unlink sealed release
        # contents. Never chmod a file: its inode may have an outside link.
        find -P "$canonical" -type d -exec chmod u+w {} + || return 1
        shared_attempted=false
        shared_progress=true
        full_uninstall_quiet || return 1
        shared_attempted=true
    fi
    full_uninstall_mount_recheck "$canonical" || return 1
    rm -rf "$canonical" || return 1
    shared_attempted=false
    shared_progress=true
    echo "  Removed $label: $canonical"
}

safe_remove_bin_link() {
    local target="$1"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then
        return 0
    fi
    # Only remove if it is a symlink pointing into our release root, or a
    # regular file that is our binary (verified by name match).
    if [ -L "$target" ]; then
        local link_target
        link_target="$(readlink -f "$target" 2>/dev/null || readlink "$target")" || return 1
        local canonical_release
        canonical_release="$(canonicalize "$RELEASE_ROOT")" || return 1
        case "$link_target" in
            "$canonical_release/"*)
                full_uninstall_quiet || return 1
                shared_attempted=true
                rm -f "$target" || return 1
                shared_attempted=false
                shared_progress=true
                echo "  Removed binary symlink: $target -> $link_target"
                return 0
                ;;
        esac
        echo "  Skipping binary link (symlink target not in release root): $target -> $link_target" >&2
        return 0
    fi
    # A regular shim must be byte-identical to the active verified release.
    # Its filename and executable bit alone are not ownership proof.
    local active_binary="$RELEASE_ROOT/current/herdr-mobile-relay"
    if [ -f "$active_binary" ] && [ ! -L "$active_binary" ] &&
       command -v cmp >/dev/null 2>&1 && cmp -s "$target" "$active_binary"; then
        full_uninstall_quiet || return 1
        shared_attempted=true
        rm -f "$target" || return 1
        shared_attempted=false
        shared_progress=true
        echo "  Removed binary: $target"
    else
        echo "  Skipping binary (not identical to the active owned release): $target" >&2
    fi
}

# Searchable nonsymlink ancestry is required before treating a target as absent.
# Final symlinks retain the existing link-only removal policy.
for target in "$RELEASE_ROOT" "$CONFIG_DIR" "$CACHE_DIR" "$BIN_LINK"; do
    parent="$(dirname "$target")" || full_uninstall_fail
    service_removal_env_safe "$parent/.herdr-full-uninstall-admission" || {
        echo 'Full-uninstall target ancestry is unavailable or unsafe.' >&2; full_uninstall_fail;
    }
done
# Validate every directory before stopping the service. A bad custom/XDG path
# must fail without disrupting a healthy installation.
preflight_removal_target "$RELEASE_ROOT" "releases" || full_uninstall_fail
preflight_removal_target "$CONFIG_DIR" "config/state" || full_uninstall_fail
preflight_removal_target "$CACHE_DIR" "cache" || full_uninstall_fail
# This must precede even the optional exact-route disposition and the native
# delegate. Refusal leaves the service, journal and all shared roots untouched.
if [ -d "$RELEASE_ROOT" ] && [ ! -L "$RELEASE_ROOT" ]; then
    release_canonical="$(canonicalize "$RELEASE_ROOT")" || full_uninstall_fail
    preflight_release_links "$release_canonical" || full_uninstall_fail
fi
# Linux only, after every read-only admission and still before any route,
# native, link or shared effect: refusal leaves the service, journal and all
# roots untouched. Darwin has no confinement proof and is unchanged.
if [ "$MOUNT_GUARD" = true ]; then
    full_uninstall_mount_preflight || {
        echo 'Full uninstall refused: mount confinement of the uninstall roots could not be proved; nothing was changed.' >&2
        full_uninstall_fail
    }
fi

CLI_ROUTE_DISPOSITION=""
if [ "$TRANSPORT" = tailscale-cli ]; then
    for registration_root in "$CLI_STATE_ROOT" "$CLI_COORDINATION_ROOT"; do
        registration_root="$(canonicalize "$registration_root")" || full_uninstall_fail
        for removable_root in "$CONFIG_DIR" "$CACHE_DIR" "$RELEASE_ROOT"; do
            removable_root="$(canonicalize "$removable_root")" || full_uninstall_fail
            case "$registration_root" in
                "$removable_root"|"$removable_root"/*)
                    echo "✗ CLI registration root overlaps uninstall target $removable_root: $registration_root" >&2
                    echo "  Move the registration journal outside removable relay directories before uninstalling." >&2
                    exit 1
                    ;;
            esac
            case "$removable_root" in
                "$registration_root"|"$registration_root"/*)
                    echo "✗ Uninstall target $removable_root contains CLI registration root $registration_root." >&2
                    exit 1
                    ;;
            esac
        done
    done
    echo "The CLI-backed HTTPS Serve route persists independently of this service and uninstall."
    if [ -t 0 ]; then
        read -r -p "Remove only the exact journaled route before uninstall? [y/N] " remove_route || full_uninstall_fail
    else
        remove_route="no"
    fi
    case "$remove_route" in
        y|Y|yes|YES)
            HERDR_RELAY_ENV="$RELAY_ENV_FILE" "$SCRIPT_DIR/tailscale-cli.sh" unpublish || {
                echo "✗ Exact route removal did not complete; uninstall stopped and the journal was preserved." >&2
                exit 1
            }
            CLI_ROUTE_DISPOSITION=removed
            ;;
        *)
            if [ -t 0 ]; then
                read -r -p "Explicitly leave the route configured and preserve its journal? [y/N] " leave_route || full_uninstall_fail
            else
                leave_route="no"
            fi
            case "$leave_route" in
                y|Y|yes|YES)
                    echo "Leaving persistent route and private registration journal intact."
                    CLI_ROUTE_DISPOSITION=retained
                    ;;
                *) echo "Uninstall cancelled; choose route removal or explicit route preservation."; exit 1 ;;
            esac
            ;;
    esac
fi

echo "Herdr Mobile Relay — full uninstall"
echo ""
echo "This will remove:"
echo "  Service:      herdr-mobile-relay.service (systemd/launchd)"
echo "  Releases:     $RELEASE_ROOT"
echo "  Binary link:  $BIN_LINK"
echo "  Config/state: $CONFIG_DIR"
echo "  Cache:        $CACHE_DIR"
if [ "$TRANSPORT" = tailscale-external ]; then
    echo "  Operator-owned HTTPS Serve/Funnel ingress is not inspected or removed by this uninstall."
fi
echo ""
read -r -p "Continue? [y/N] " choice || full_uninstall_fail
case "$choice" in
    y|Y|yes|YES) ;;
    *) echo "Cancelled."; exit 0 ;;
esac

echo ""

# Delegate failure can follow safe native partial progress, but cannot grant
# shared-file authority. There is deliberately no force/extra-yes escape.
service_removal_env_safe "$NATIVE_REMOVER" && [ -f "$NATIVE_REMOVER" ] || full_uninstall_fail
native_status=0
# Stock Bash 3.2 nounset cannot expand an empty array. Delegate with no
# arguments, or with exactly the one explicit route disposition, instead.
if [ -z "$CLI_ROUTE_DISPOSITION" ]; then
    bash "$NATIVE_REMOVER" || native_status=$?
else
    bash "$NATIVE_REMOVER" "--cli-route-disposition=$CLI_ROUTE_DISPOSITION" || native_status=$?
fi
if [ "$native_status" -ne 0 ]; then
    echo 'Native service removal did not complete.' >&2
    full_uninstall_fail "$native_status"
fi
full_uninstall_quiet || full_uninstall_fail

# Each existing destructive seam gates immediately before effects, even when
# invoked conditionally. Observations are cooperative, not locks/deadlines.
safe_remove_bin_link "$BIN_LINK" || full_uninstall_fail
safe_remove_dir "$RELEASE_ROOT" "releases" || full_uninstall_fail
safe_remove_dir "$CONFIG_DIR" "config/state" || full_uninstall_fail
safe_remove_dir "$CACHE_DIR" "cache" || full_uninstall_fail

if command -v herdr >/dev/null 2>&1; then
    full_uninstall_quiet || full_uninstall_fail
    echo "Removing Herdr plugin registration..."
    shared_attempted=true
    if ! herdr plugin uninstall herdr-mobile-relay.events; then
        echo "Plugin registration remains; remove it with:" >&2
        echo "  herdr plugin uninstall herdr-mobile-relay.events" >&2
        full_uninstall_fail
    fi
    shared_attempted=false
    shared_progress=true
else
    echo "Herdr is unavailable; remove plugin registration later with:"
    echo "  herdr plugin uninstall herdr-mobile-relay.events"
fi
full_uninstall_quiet || full_uninstall_fail
echo ""
echo "Herdr Mobile Relay has been uninstalled."

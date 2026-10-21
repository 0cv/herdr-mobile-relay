#!/bin/bash
# Privileged real-mount controls for the Linux full uninstall (tests/test_uninstall_mounts.py).
#
#   NATIVE_HELPER=/abs/path/to/the/built/relay /bin/bash scripts/ci/run-uninstall-mount-controls.sh
#
# This is the only place sudo is used, and only for `mount --bind` and `umount`, only on paths inside one private work
# directory (HERDR_MOUNT_WORK) that the driver itself creates under RUNNER_TEMP. The suite and its fixture's inert systemctl
# stub reach those two operations only through `__mountctl`, below. An EXIT/INT/TERM trap unmounts everything left under
# the work directory (deepest first) and deletes it ONLY once no mount remains beneath it, so removing the fixtures can
# never write through a bind mount. A mount the suite leaked fails the run. Every guard follows run-service-qualification.sh:
# a genuine disposable non-root GitHub-hosted Linux runner, or refusal before anything is created.
set -euo pipefail

refuse() {
    printf 'uninstall mount controls refused: %s\n' "$1" >&2
    exit 2
}

hosted_guards() {
    [ "${GITHUB_ACTIONS:-}" = true ] || refuse 'requires a GitHub Actions run'
    [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] || refuse 'requires a GitHub-hosted runner'
    [ "$(id -u)" -ne 0 ] || refuse 'must not run as root'
    [ "$(uname -s)" = Linux ] || refuse 'requires a Linux runner'
}

# ------------------------------------------------------------ mountctl ----
# __mountctl bind SOURCE TARGET | __mountctl unbind TARGET: every path must be an existing real directory strictly inside
# HERDR_MOUNT_WORK, spelled physically (no symlink, no dot component).
if [ "${1:-}" = __mountctl ]; then
    shift
    hosted_guards
    work="${HERDR_MOUNT_WORK:-}"
    case "$work" in /*) ;; *) refuse 'HERDR_MOUNT_WORK is not an absolute path' ;; esac
    [ -d "$work" ] && [ ! -L "$work" ] && [ "$(cd "$work" && pwd -P)" = "$work" ] || refuse 'HERDR_MOUNT_WORK is not a real directory'
    inside_work() {
        case "$1" in "$work"/*) ;; *) return 1 ;; esac
        case "$1" in */../* | */.. | */./* | */. | *//*) return 1 ;; esac
        [ -d "$1" ] && [ ! -L "$1" ] && [ "$(cd "$1" && pwd -P)" = "$1" ]
    }
    case "${1:-}:$#" in
        bind:3)
            inside_work "$2" && inside_work "$3" || refuse 'bind paths must be real directories inside the work directory'
            sudo -n /usr/bin/mount --bind "$2" "$3"
            ;;
        unbind:2)
            inside_work "$2" || refuse 'unbind path must be a real directory inside the work directory'
            sudo -n /usr/bin/umount "$2"
            ;;
        *) refuse 'usage: __mountctl bind SOURCE TARGET | __mountctl unbind TARGET' ;;
    esac
    exit 0
fi

# ---------------------------------------------------------------- guards ----
[ "$#" -eq 0 ] || refuse 'unexpected arguments'
hosted_guards
[ -n "${GITHUB_SHA:-}" ] || refuse 'GITHUB_SHA is not set'
[ -n "${HOME:-}" ] && [ -d "$HOME" ] || refuse 'HOME is not an existing directory'
case "${RUNNER_TEMP:-}" in /*) ;; *) refuse 'RUNNER_TEMP is not an absolute path' ;; esac
[ -d "$RUNNER_TEMP" ] && [ ! -L "$RUNNER_TEMP" ] || refuse 'RUNNER_TEMP is not a real directory'
git_toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || refuse 'not inside a Git checkout'
[ "$(pwd -P)" = "$git_toplevel" ] || refuse 'must run from the repository root'
[ "$(git rev-parse HEAD 2>/dev/null)" = "$GITHUB_SHA" ] || refuse 'checkout HEAD does not match GITHUB_SHA'
case "${NATIVE_HELPER:-}" in /*) ;; *) refuse 'NATIVE_HELPER is not an absolute path' ;; esac
[ -f "$NATIVE_HELPER" ] && [ -x "$NATIVE_HELPER" ] && [ ! -L "$NATIVE_HELPER" ] || refuse 'NATIVE_HELPER is not an executable file'
PYTHON3=$(command -v python3 2>/dev/null) || refuse 'python3 was not found on PATH'
"$PYTHON3" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)' || refuse 'python3 3.11 or newer is required'
# Only the two operations below are ever run through sudo; refuse before creating anything if they are not permitted.
sudo -n -l /usr/bin/mount >/dev/null 2>&1 && sudo -n -l /usr/bin/umount >/dev/null 2>&1 ||
    refuse 'passwordless sudo for mount and umount is unavailable'

driver="$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")"
evidence="$RUNNER_TEMP/uninstall-mount-controls"
[ ! -L "$evidence" ] || refuse 'evidence directory is a symlink'
(umask 077 && mkdir -p "$evidence") || refuse 'cannot create the evidence directory'
work_dir=$(mktemp -d "$RUNNER_TEMP/herdr-uninstall-mounts.XXXXXX") || refuse 'cannot create a private work directory'
work_dir=$(cd "$work_dir" && pwd -P)

mountinfo_file=/proc/self/mountinfo
# mounted_under_work: the mount points beneath the work directory, one per line. It FAILS when the mount table cannot be read
# (or is empty, which a live namespace never is): an unknown table is never the same as "no mounts".
mounted_under_work() {
    local table
    table=$(cat "$mountinfo_file") || return 1
    [ -n "$table" ] || return 1
    printf '%s\n' "$table" | awk -v w="$work_dir" '$5 == w || index($5, w "/") == 1 { print $5 }'
}

leaked=0
# unmount_all: unmount whatever remains beneath the work directory, deepest first. Succeeds only when a mount table that
# could be read shows nothing remaining; any read or sort failure is a failure (the directory is then NOT deleted).
unmount_all() {
    attempt=0
    while [ "$attempt" -lt 8 ]; do
        remaining=$(mounted_under_work) || return 1
        [ -n "$remaining" ] || return 0
        remaining=$(printf '%s\n' "$remaining" | sort -r) || return 1
        [ "$attempt" -gt 0 ] || leaked=1
        while IFS= read -r mount_point; do
            sudo -n /usr/bin/umount "$mount_point" >/dev/null 2>&1 || true
        done <<< "$remaining"
        attempt=$((attempt + 1))
    done
    remaining=$(mounted_under_work) || return 1
    [ -z "$remaining" ]
}

# shellcheck disable=SC2329  # invoked through the traps below
cleanup() {
    cleanup_status=$?
    trap - EXIT INT TERM
    if unmount_all; then
        rm -rf "$work_dir" || cleanup_status=1
    else
        printf 'uninstall mount controls: mounts remain beneath the work directory; it was NOT deleted\n' >&2
        cleanup_status=1
    fi
    if [ "$leaked" -ne 0 ]; then
        printf 'uninstall mount controls: a fixture mount was left behind by the suite\n' >&2
        cleanup_status=1
    fi
    exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Control for the cleanup rule above: with an unreadable mount table, unmount_all must fail (so the trap would keep the directory).
if ( mountinfo_file="$work_dir/.no-such-mountinfo"; unmount_all ) 2>/dev/null; then
    printf 'uninstall mount controls: an unreadable mount table was treated as having no mounts\n' >&2
    exit 1
fi
printf 'control: an unreadable mount table is not treated as clear\n'
printf 'uninstall mount controls: %s, candidate %s\n' "$(uname -sr)" "$GITHUB_SHA"
suite_status=0
# Time budget: the workflow step is capped at 15 minutes (900 s) and that clock includes this driver's setup and the teardown trap
# above. The suite gets 600 s plus a 30 s kill-after (630 s at most), which leaves at least 270 s for setup and for the unmount
# rounds and census of the trap, so the step deadline can never cancel the driver before its teardown has run.
timeout --kill-after=30 600 /usr/bin/env -i \
    HOME="$HOME" PATH="$PATH" LC_ALL=C \
    GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
    HERDR_MOUNT_WORK="$work_dir" HERDR_MOUNTCTL="$driver" HERDR_MOUNT_EVIDENCE="$evidence" \
    NATIVE_HELPER="$NATIVE_HELPER" \
    "$PYTHON3" tests/test_uninstall_mounts.py || suite_status=$?
if [ "$suite_status" -eq 0 ]; then
    printf 'RESULT uninstall mount controls: pass\n'
else
    printf 'RESULT uninstall mount controls: FAILED, exit %s\n' "$suite_status" >&2
fi
exit "$suite_status"

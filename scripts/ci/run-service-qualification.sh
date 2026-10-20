#!/bin/bash
# Service qualification driver for GitHub-hosted CI.
#
# Modes (anything else refuses with exit 2):
#   suites               the nine unsharded suites, each invoked the way the Makefile's
#                        shell-check invokes it. The dispatch, doctor and setup-menu suites that
#                        tests/test_native_install.sh nests are each their own required run here
#                        (own exit status, timing and bound), so one doctor failure can no
#                        longer hide the native cells
#   native TIER I/N      one shard of tests/test_native_install.sh (TIER fast|full; N is the
#                        tier's fixed shard count in tests/service_qualification_matrix.py) and
#                        the SHA/tier/OS/shard-bound evidence of the cells it COMPLETED
#   uninstall-cli TIER I/N  the same for tests/test_service_uninstall_cli.py (its controls run in
#                        shard 1 of the tier only)
#   all (the default)    suites, then the whole exhaustive native suite and the whole uninstall
#                        CLI suite, each in one process
# CI runs `suites`, `native` and `uninstall-cli` as separate jobs. The suite files list the cells;
# this driver only bounds, records and verifies them. tests/test_service_supervision.py is in no
# Makefile target and no other workflow; this driver is the only place that runs it.
# Invoked by the service workflows as:
#
#   /bin/bash scripts/ci/run-service-qualification.sh suites
#   /bin/bash scripts/ci/run-service-qualification.sh native fast 1/2
#   /bin/bash scripts/ci/run-service-qualification.sh uninstall-cli full 3/3
#
# so that macOS runs the stock /bin/bash 3.2 and Linux runs bash 5. This file
# therefore avoids associative arrays, mapfile/readarray, case-conversion
# expansions, &>>, and empty-array expansion under nounset.
#
# Fail-closed contract:
#   * Refuses (exit 2, fixed message) unless it is a genuine hosted run. It
#     never sets, defaults or forges GITHUB_ACTIONS, RUNNER_ENVIRONMENT or any
#     HERDR_* hosted marker; the suites refuse on their own when those are not
#     the real runner values, and that refusal is a failure like any other.
#   * Every suite is attempted, even after earlier failures or timeouts. A
#     suite passes only by exiting 0. Nonzero, death by signal, a hosted-guard
#     refusal, a wall-clock timeout and a suite that modifies the checkout all
#     fail it.
#   * The script exits nonzero unless every suite passed, and it writes a
#     sanitized summary at the end of the run.
#
# This script runs project tests. It is meant to be run by CI only.
set -eu

# Fixed wall-clock bounds, and nothing else. Each suite may run at most
# SUITE_TIMEOUT_SECONDS. The dispatch suite's helper build is bounded separately
# by HELPER_BUILD_TIMEOUT_SECONDS, and its tests then get their own full
# SUITE_TIMEOUT_SECONDS. There is no aggregate budget, no reservation and no
# job-clock arithmetic. Residual, stated plainly: ten suites at their bounds can
# need more than the workflow's 60-minute job cap (timeout-minutes). A
# pathologically slow or hung run is then cancelled by GitHub at that cap, which
# is a red job with a missing summary artifact, never a pass. Tune
# SUITE_TIMEOUT_SECONDS from the first hosted run.
SUITE_TIMEOUT_SECONDS=900
HELPER_BUILD_TIMEOUT_SECONDS=180

refuse() {
    printf 'service qualification refused: %s\n' "$1" >&2
    exit 2
}

# ---------------------------------------------------------------- mode ----

mode=${1:-all}
shard_tier=full
shard_spec=''
case "$mode" in
    all | suites) [ "$#" -le 1 ] || refuse 'unexpected arguments' ;;
    native | uninstall-cli)
        [ "$#" -eq 3 ] || refuse "usage: $mode TIER I/N"
        shard_tier=$2
        shard_spec=$3
        case "$shard_tier" in fast | full) ;; *) refuse 'unknown tier' ;; esac
        case "$shard_spec" in
            [1-9]/[1-9] | [1-9]/[1-9][0-9] | [1-9][0-9]/[1-9] | [1-9][0-9]/[1-9][0-9]) ;;
            *) refuse 'malformed shard' ;;
        esac
        ;;
    *) refuse 'unknown mode' ;;
esac

# ---------------------------------------------------------------- guards ----

[ "${GITHUB_ACTIONS:-}" = true ] || refuse 'requires a GitHub Actions run'
[ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] || refuse 'requires a GitHub-hosted runner'
[ "$(id -u)" -ne 0 ] || refuse 'must not run as root'
case "$(uname -s)" in
    Darwin | Linux) ;;
    *) refuse 'requires a Darwin or Linux runner' ;;
esac
[ -n "${GITHUB_SHA:-}" ] || refuse 'GITHUB_SHA is not set'
[ -n "${HOME:-}" ] && [ -d "$HOME" ] || refuse 'HOME is not an existing directory'
case "${RUNNER_TEMP:-}" in
    /*) ;;
    *) refuse 'RUNNER_TEMP is not an absolute path' ;;
esac
[ -d "$RUNNER_TEMP" ] && [ ! -L "$RUNNER_TEMP" ] || refuse 'RUNNER_TEMP is not a real directory'

git_toplevel=$(git rev-parse --show-toplevel 2>/dev/null) || refuse 'not inside a Git checkout'
[ "$(pwd -P)" = "$git_toplevel" ] || refuse 'must run from the repository root'
candidate_sha=$(git rev-parse HEAD 2>/dev/null) || refuse 'cannot read the checkout HEAD'
[ "$candidate_sha" = "$GITHUB_SHA" ] || refuse 'checkout HEAD does not match GITHUB_SHA'

PYTHON3=$(command -v python3 2>/dev/null) || refuse 'python3 was not found on PATH'
"$PYTHON3" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)' ||
    refuse 'python3 3.11 or newer is required (service dispatch fixtures need tomllib)'
GO_BIN=$(command -v go 2>/dev/null) || refuse 'go was not found on PATH'

# ----------------------------------------------------------- bookkeeping ----

summary_dir="$RUNNER_TEMP/service-qualification"
[ ! -L "$summary_dir" ] || refuse 'summary directory is a symlink'
(umask 077 && mkdir -p "$summary_dir") || refuse 'cannot create the summary directory'
chmod 0700 "$summary_dir" || refuse 'cannot restrict the summary directory'
work_dir=$(mktemp -d "$RUNNER_TEMP/herdr-service-qualification.XXXXXX") ||
    refuse 'cannot create a private work directory'

# shellcheck disable=SC2329  # invoked through the EXIT trap below
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

GOTOOLCHAIN=${GOTOOLCHAIN:-local}
overall=pass
checkout_unchanged=1
records=''
suite_count=0

checkout_state() {
    git status --porcelain --untracked-files=all 2>/dev/null
}

baseline_state=$(checkout_state) || refuse 'cannot read the checkout state'

# ------------------------------------------------------------ supervisor ----

# bounded SECONDS COMMAND [ARG...]
#
# Enforces a wall-clock bound without GNU timeout (absent on macOS) and owns a
# process group for the command. Ownership uses an ANCHOR: before the command
# starts, a small helper process becomes the leader of a fresh process group
# (same session, never holding the command's pipes) and the command joins that
# group. The anchor is not reaped until the group has been signalled, so the group
# id (the anchor's pid) cannot be recycled while the SIGKILL is sent, on every
# path: normal exit, timeout, SIGTERM/SIGINT and a spawn failure. A SIGTERM or
# SIGINT only RECORDS its number; the supervisor acts on it at safe points (the
# wait loop), never between creating a child and owning its handle and never during
# cleanup, so the anchor and the suite are always killed and reaped before the
# supervisor exits with 128+N, and no suite is started once a signal is recorded.
# On timeout the whole group is killed; a group left behind after a normal exit is
# killed too, so no suite can leak processes into the next one. A suite that makes
# its own session escapes the group kill exactly as it did before: the suites'
# fixtures do so with start_new_session and kill those children by their own pids.
# None of the suites relies on being its own session or group leader (they
# never call getpgrp or getsid and never signal group 0 or -pgid; checked by
# reading).
# Exit status: the command's own status, 128+N when it died of signal N, 124 on
# timeout, 143/130 when this supervisor itself is terminated, and nonzero when the
# command cannot be started. It is never 0 unless the command exited 0: after the
# cleanup the two signals are blocked while the default actions are restored. A
# wakeup pipe retains signals caught before the block, even if their Python
# callbacks never ran; signals held by the kernel terminate the supervisor when
# unblocked. Signals arriving later also take the default nonzero termination.
# process_group needs Python 3.11, which the guards above already require.
bounded() {
    bounded_limit=$1
    shift
    "$PYTHON3" - "$bounded_limit" "$@" <<'PY'
import os
import signal
import subprocess
import sys
import time

limit = int(sys.argv[1])
command = sys.argv[2:]

# A termination signal only records its number. It is acted on in the wait loop
# below, never while a child is being created or while cleaning up, so the anchor
# and the suite are always owned before they can be abandoned and the process group
# is always killed and reaped before this supervisor exits with 128+N.
stop = 0


def terminated(signum, _frame):
    global stop
    if not stop:
        stop = signum


# The C signal handler writes a byte before deferring the Python callback. Keep
# that independent record through the final handler handoff below. Both pipe
# descriptors are non-inheritable and Popen closes them in the children. Never
# drain the pipe before the handoff: if it fills, an earlier byte still records
# termination, so dropping further wakeups cannot turn a signal into success.
signal_read, signal_write = os.pipe()
os.set_blocking(signal_read, False)
os.set_blocking(signal_write, False)
signal.set_wakeup_fd(signal_write, warn_on_full_buffer=False)
signal.signal(signal.SIGTERM, terminated)
signal.signal(signal.SIGINT, terminated)

status = 1
anchor = None
proc = None
try:
    # The anchor only holds the group id. Its sleep is a leak backstop and exceeds
    # the bound; it is always killed below.
    anchor = subprocess.Popen(
        [sys.executable, "-I", "-B", "-c", "import time; time.sleep(%d)" % (limit + 60)],
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        close_fds=True, process_group=0)
    if not stop:
        proc = subprocess.Popen(command, stdin=subprocess.DEVNULL, process_group=anchor.pid)
        deadline = time.monotonic() + limit
        while True:
            code = proc.poll()
            if code is not None:
                status = code if code >= 0 else 128 - code
                break
            if stop:
                break
            if time.monotonic() >= deadline:
                print("service qualification: suite exceeded its %ds wall-clock bound; "
                      "killing its process group" % limit, file=sys.stderr, flush=True)
                status = 124
                break
            time.sleep(0.05)
finally:
    # Signal the group while the anchor is alive and unreaped, THEN reap the
    # command and the anchor.
    if anchor is not None:
        try:
            os.killpg(anchor.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
    if proc is not None:
        proc.wait()
    if anchor is not None:
        anchor.wait()
# The supervisor is single-threaded. Block both signals before changing either
# handler, then read the wakeup pipe while they remain blocked. Every signal
# already consumed by the C trampoline has left a byte, independently of whether
# its Python callback ran before the swap. One byte is sufficient; an empty pipe
# raises BlockingIOError, while any other read failure fails closed.
# Signals not consumed before the block stay pending in the kernel and terminate
# us under SIG_DFL when unblocked; later signals also terminate us, including
# during the final status check or interpreter shutdown. No signal can be lost
# between the pipe read and exit. The mask cannot reach children: both are already
# reaped. The pipe is kept open until process exit, which closes both descriptors.
terminating = {signal.SIGTERM, signal.SIGINT}
signal.pthread_sigmask(signal.SIG_BLOCK, terminating)
signal.signal(signal.SIGTERM, signal.SIG_DFL)
signal.signal(signal.SIGINT, signal.SIG_DFL)
try:
    caught = os.read(signal_read, 1)
except BlockingIOError:
    caught = b""
if not stop and caught:
    stop = caught[0]
signal.pthread_sigmask(signal.SIG_UNBLOCK, terminating)
if stop:
    sys.exit(128 + stop)
sys.exit(status)
PY
}

# run_closed SECONDS EXTRA COMMAND [ARG...]
#
# Runs COMMAND through bounded() under a closed environment: `env -i` plus
# exactly HOME TMPDIR PATH GOTOOLCHAIN GOFLAGS GITHUB_ACTIONS RUNNER_ENVIRONMENT
# RUNNER_TEMP and, when EXTRA is non-empty, that one NAME=value assignment.
# TMPDIR is the runner's real value and is omitted when the runner has none
# (suites then use their own /tmp fallback); it is not replaced by RUNNER_TEMP. The
# suites build fixtures that assert on ambient credentials and tool selection,
# so they must not inherit the rest of the runner environment. The runner PATH
# is kept so the setup-go/setup-bun/python3 toolchains resolve. GITHUB_ACTIONS
# and RUNNER_ENVIRONMENT are forwarded unchanged; the guards above already
# required their genuine hosted values.
run_closed() {
    closed_limit=$1
    closed_extra=$2
    shift 2
    bounded "$closed_limit" /usr/bin/env -i \
        HOME="$HOME" ${TMPDIR:+"TMPDIR=$TMPDIR"} PATH="$PATH" \
        GOTOOLCHAIN="$GOTOOLCHAIN" GOFLAGS="${GOFLAGS:-}" \
        GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
        RUNNER_TEMP="$RUNNER_TEMP" \
        ${closed_extra:+"$closed_extra"} "$@"
}

# ---------------------------------------------------------------- suites ----

banner() {
    printf '\n================================================================\n'
    printf 'SUITE %s\n' "$1"
    printf '================================================================\n'
}

# record_suite NAME EXIT SECONDS: appends to the table and the summary, and
# downgrades the overall result unless the suite exited 0.
record_suite() {
    suite_count=$((suite_count + 1))
    records="${records}$1 $2 $3
"
    if [ "$2" -eq 0 ]; then
        printf 'RESULT %s: exit %s in %ss\n' "$1" "$2" "$3"
        return 0
    fi
    overall=fail
    printf 'RESULT %s: FAILED, exit %s in %ss\n' "$1" "$2" "$3"
}

# check_checkout NAME: a suite that leaves the checkout different from the
# pre-run baseline (tracked edits, new files, deletions) fails the run.
check_checkout() {
    if after_state=$(checkout_state) && [ "$after_state" = "$baseline_state" ]; then
        return 0
    fi
    overall=fail
    checkout_unchanged=0
    printf 'RESULT %s: FAILED, the suite modified the checkout (git status now differs):\n' "$1" >&2
    checkout_state >&2 || true
    return 0
}

# run_suite NAME EXTRA COMMAND [ARG...]
run_suite() {
    suite_name=$1
    suite_extra=$2
    shift 2
    banner "$suite_name"
    suite_started=$SECONDS
    suite_status=0
    run_closed "$SUITE_TIMEOUT_SECONDS" "$suite_extra" "$@" || suite_status=$?
    record_suite "$suite_name" "$suite_status" "$((SECONDS - suite_started))"
    check_checkout "$suite_name"
}

run_service_suites() {
    run_suite tests/test_common.sh '' /bin/bash tests/test_common.sh
    run_suite tests/test_service_credentials.sh '' /bin/bash tests/test_service_credentials.sh
    run_suite tests/test_service_supervision.py '' "$PYTHON3" tests/test_service_supervision.py

    # The dispatch, doctor and setup-menu suites require NATIVE_HELPER at import time: the compiled
    # relay used for strict JSON/origin commands (the native suite builds its own the same way:
    # go build -o <helper> ./cmd/herdr-mobile-relay). The build only compiles; it does not run the
    # relay. It has its own bound (HELPER_BUILD_TIMEOUT_SECONDS) and each suite then gets its own
    # bound, the dispatch suite's time including the build; nothing is subtracted between them. A
    # failed or timed-out build fails all three without running them.
    dispatch_name=tests/test_service_transport_dispatch.py
    helper="$work_dir/native-helper"
    helper_status=0
    banner "$dispatch_name"
    dispatch_started=$SECONDS
    printf 'building the NATIVE_HELPER relay binary for the dispatch, doctor and setup-menu suites\n'
    run_closed "$HELPER_BUILD_TIMEOUT_SECONDS" '' "$GO_BIN" build -o "$helper" ./cmd/herdr-mobile-relay ||
        helper_status=$?
    dispatch_status=$helper_status
    if [ "$helper_status" -ne 0 ]; then
        printf 'NATIVE_HELPER build failed; the suite is not run\n' >&2
    else
        run_closed "$SUITE_TIMEOUT_SECONDS" "NATIVE_HELPER=$helper" "$PYTHON3" "$dispatch_name" ||
            dispatch_status=$?
    fi
    record_suite "$dispatch_name" "$dispatch_status" "$((SECONDS - dispatch_started))"
    check_checkout "$dispatch_name"

    # The doctor and setup-menu suites keep the bounds they had nested in the native suite (120s, 180s).
    run_helper_suite tests/test_plugin_doctor.py 120
    run_helper_suite tests/test_setup_menu.py 180

    run_suite tests/test_uninstall.sh '' /bin/bash tests/test_uninstall.sh
    # The launcher-admission variable is set for this one suite only, as in the Makefile.
    run_suite tests/test_tailscale_cli_setup_failure.py HERDR_TAILSCALE_LAUNCHER_CI=1 \
        "$PYTHON3" tests/test_tailscale_cli_setup_failure.py
    run_suite tests/test_release_scripts.sh '' /bin/sh tests/test_release_scripts.sh
}

# run_helper_suite NAME SECONDS: a Python suite that needs the NATIVE_HELPER built above.
run_helper_suite() {
    banner "$1"
    helper_suite_started=$SECONDS
    helper_suite_status=$helper_status
    if [ "$helper_status" -ne 0 ]; then
        printf 'NATIVE_HELPER build failed; the suite is not run\n' >&2
    else
        run_closed "$2" "NATIVE_HELPER=$helper" "$PYTHON3" "$1" || helper_suite_status=$?
    fi
    record_suite "$1" "$helper_suite_status" "$((SECONDS - helper_suite_started))"
    check_checkout "$1"
}

# A sharded suite runs the cells selected by TIER and SHARD (the whole exhaustive suite when there is no shard), the native one
# with its nested children skipped: they are the required suites above. The suite appends one line per COMPLETED cell to the cell
# record; the evidence below is written only from that record, bound to this candidate, run, tier, runner OS and shard, and is
# `pass` only if the suite exited 0 and its completed cells are exactly the shard's cells. A failed, partial, timed-out or empty
# shard still gets a (failing) record.
# run_cell_suite KIND FILE ENV_PREFIX EVIDENCE_FILE EXTRA COMMAND [ARG...]
run_cell_suite() {
    cell_kind=$1
    cell_file=$2
    cell_prefix=$3
    cell_evidence=$4
    cell_extra=$5
    shift 5
    cell_name=$cell_file
    cell_record="$summary_dir/$cell_kind-cells.txt"
    if [ -n "$shard_spec" ]; then
        cell_name="$cell_name#$shard_tier-${shard_spec%/*}of${shard_spec#*/}"
    fi
    run_suite "$cell_name" "$cell_extra" \
        "${cell_prefix}_TIER=$shard_tier" ${shard_spec:+"${cell_prefix}_SHARD=$shard_spec"} \
        "${cell_prefix}_EVIDENCE=$cell_record" "$@"
    [ -n "$shard_spec" ] || return 0
    cell_exit=$suite_status
    evidence_status=0
    "$PYTHON3" -I -B tests/service_qualification_matrix.py evidence --suite "$cell_kind" --tier "$shard_tier" --shard "$shard_spec" \
        --sha "$candidate_sha" --run-id "${GITHUB_RUN_ID:-}" --run-attempt "${GITHUB_RUN_ATTEMPT:-}" \
        --runner-os "$(uname -s)" --suite-exit "$cell_exit" --cells "$cell_record" \
        --output "$summary_dir/$cell_evidence" || evidence_status=$?
    if [ "$evidence_status" -ne 0 ]; then
        overall=fail
        printf 'service qualification: the shard did not produce passing evidence\n' >&2
    fi
}

run_native_suite() {
    run_cell_suite native tests/test_native_install.sh HERDR_NATIVE_INSTALL native-shard-evidence.json \
        HERDR_NATIVE_INSTALL_SKIP_NESTED_CHILDREN=1 /bin/bash tests/test_native_install.sh
}

run_uninstall_cli_suite() {
    run_cell_suite uninstall-cli tests/test_service_uninstall_cli.py HERDR_UNINSTALL_CLI uninstall-cli-shard-evidence.json \
        '' "$PYTHON3" tests/test_service_uninstall_cli.py
}

EXPECTED_SUITE_COUNT=0
case "$mode" in
    suites) EXPECTED_SUITE_COUNT=9; run_service_suites ;;
    native) EXPECTED_SUITE_COUNT=1; run_native_suite ;;
    uninstall-cli) EXPECTED_SUITE_COUNT=1; run_uninstall_cli_suite ;;
    *) EXPECTED_SUITE_COUNT=11; run_service_suites; run_native_suite; run_uninstall_cli_suite ;;
esac

# --------------------------------------------------------------- results ----

# The run must have executed every suite and must leave the checkout as found.
if [ "$suite_count" -ne "$EXPECTED_SUITE_COUNT" ]; then
    overall=fail
    printf 'service qualification: recorded %s suites, expected %s\n' "$suite_count" "$EXPECTED_SUITE_COUNT" >&2
fi
if final_state=$(checkout_state) && [ "$final_state" = "$baseline_state" ]; then
    :
else
    overall=fail
    checkout_unchanged=0
    printf 'service qualification: the checkout differs from its pre-run state\n' >&2
fi

printf '\n================================================================\n'
printf 'SERVICE QUALIFICATION SUMMARY\n'
printf '================================================================\n'
printf '%s' "$records" | while read -r table_name table_exit table_seconds; do
    if [ "$table_exit" -eq 0 ]; then
        table_result=pass
    else
        table_result=FAIL
    fi
    printf '%-4s %-48s exit %-4s %5ss\n' "$table_result" "$table_name" "$table_exit" "$table_seconds"
done
printf 'checkout unchanged: %s\n' "$([ "$checkout_unchanged" -eq 1 ] && echo yes || echo no)"
printf 'result: %s\n' "$overall"

# Sanitized summary: no environment values, tokens, absolute paths or logs.
python_version=$("$PYTHON3" -c 'import platform; print(platform.python_version())') ||
    python_version=unknown
summary_status=0
"$PYTHON3" - "$summary_dir/summary.json" "$candidate_sha" "$(uname -s)" "$(uname -m)" \
    "$BASH_VERSION" "$python_version" "$checkout_unchanged" "$overall" "$records" \
    "$mode" "${GITHUB_RUN_ID:-}" "${GITHUB_RUN_ATTEMPT:-}" <<'PY' || summary_status=$?
import json
import os
import sys

(path, candidate_sha, runner_os, runner_arch, bash_version, python_version,
 checkout_unchanged, result, records, mode, run_id, run_attempt) = sys.argv[1:13]
suites = []
for line in records.splitlines():
    name, code, seconds = line.rsplit(" ", 2)
    suites.append({"name": name, "exit": int(code), "seconds": int(seconds)})
document = {
    "schema_version": 2,
    "candidate_sha": candidate_sha,
    "mode": mode,
    "run_id": run_id,
    "run_attempt": run_attempt,
    "runner": {"os": runner_os, "arch": runner_arch},
    "bash_version": bash_version,
    "python_version": python_version,
    "suites": suites,
    "checkout_unchanged": checkout_unchanged == "1",
    "result": result,
}
temporary = path + ".tmp"
descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
    json.dump(document, stream, indent=2)
    stream.write("\n")
os.replace(temporary, path)
PY
if [ "$summary_status" -ne 0 ]; then
    printf 'service qualification: could not write the summary\n' >&2
    exit 1
fi

[ "$overall" = pass ] || exit 1
exit 0

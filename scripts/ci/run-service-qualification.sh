#!/bin/bash
# Service qualification driver for GitHub-hosted CI.
#
# Runs the nine service/installer suites that exercise the A1 repairs, in a
# fixed order, each invoked the way the Makefile's shell-check invokes it.
# Coverage elsewhere, stated precisely:
#   * Suite 4 (tests/test_service_supervision.py) is in no Makefile target and
#     no workflow; this job is the only place that runs it.
#   * Suite 5 (tests/test_service_transport_dispatch.py) is not run directly by
#     the Makefile or any workflow, but it runs transitively as a child of
#     tests/test_native_install.sh, which is suite 3 here and is part of
#     `make shell-check` (which check.yml runs). It therefore runs twice in
#     this job by design: once directly, so it has its own exit status and
#     timing, and once inside suite 3.
# Invoked by .github/workflows/service-qualification.yml as:
#
#   /bin/bash scripts/ci/run-service-qualification.sh
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
#   * The script exits nonzero unless every suite passed, and it always writes
#     its summary before the workflow's job cap can end the job (see the time
#     budget below).
#
# This script runs project tests. It is meant to be run by CI only.
set -eu

# One suite may run at most SUITE_TIMEOUT_SECONDS in total; for the dispatch
# suite that single bound covers the helper build AND the tests.
SUITE_TIMEOUT_SECONDS=900
HELPER_BUILD_TIMEOUT_SECONDS=180
# Nine suites at their own bound could need far more than the workflow's
# 60-minute job cap (timeout-minutes), and that cap also covers the steps
# before and after this script. The suites therefore share an aggregate budget,
# TOTAL_BUDGET_SECONDS, measured from the FIRST step of the job: the workflow's
# first step records the job start time in SERVICE_QUALIFICATION_JOB_STARTED,
# and script_started is derived from it below, so checkout, provenance and
# toolchain setup are inside the budget. The budget never skips a suite: every
# suite that has not run yet keeps a reserved MIN_SUITE_SECONDS out of it, and
# a suite may use at most what remains after those reservations (never more
# than SUITE_TIMEOUT_SECONDS). While the budget is ample, suites keep their
# full bound; after repeated timeouts, later suites still start and get at
# least MIN_SUITE_SECONDS. If every suite runs to its limit they end about
# TOTAL_BUDGET_SECONDS after the job start (the dispatch suite's build and
# test phases can add at most one more MIN_SUITE_SECONDS), which leaves the
# rest of the 60 minutes, at least about 7, for the summary, the upload and the
# post-job steps, provided setup left room for the reservations.
# Residual, stated plainly: this cannot prevent the cap from being reached if
# the steps before the script consume most of it, or if the runner or Actions
# hangs. The job is then cancelled by GitHub: a red job with a missing
# artifact, never a pass.
TOTAL_BUDGET_SECONDS=3000
MIN_SUITE_SECONDS=150
EXPECTED_SUITE_COUNT=9

refuse() {
    printf 'service qualification refused: %s\n' "$1" >&2
    exit 2
}

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

# The budget clock starts at the job's first step (see the budget comment
# above). The variable is validated, not defaulted: a missing, malformed or
# future start time refuses. A large elapsed time is NOT a refusal: the clamp
# at MIN_SUITE_SECONDS in remaining_seconds() keeps every suite attempted.
# run_closed forwards an explicit allowlist, so the suites never see it.
job_started=${SERVICE_QUALIFICATION_JOB_STARTED:-}
case "$job_started" in
    '' | *[!0-9]*) refuse 'the job start time (SERVICE_QUALIFICATION_JOB_STARTED) is missing or not a number' ;;
esac
[ "${#job_started}" -le 12 ] || refuse 'the job start time (SERVICE_QUALIFICATION_JOB_STARTED) is not a plausible epoch time'
job_now=$(date +%s) || refuse 'cannot read the clock'
case "$job_now" in
    '' | *[!0-9]*) refuse 'cannot read the clock' ;;
esac
job_elapsed=$((10#$job_now - 10#$job_started))
[ "$job_elapsed" -ge 0 ] || refuse 'the job start time is in the future'
script_started=$((SECONDS - job_elapsed))

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
# Runs COMMAND in its own session/process group and enforces a wall-clock
# bound without GNU timeout (absent on macOS). On timeout the whole process
# group is killed. A group left behind after a normal exit is killed too, so
# no suite can leak processes into the next one. Exit status: the command's
# own status, 128+N when it died of signal N, 124 on timeout, 143/130 when this
# supervisor itself is terminated. It is never 0 unless the command exited 0.
bounded() {
    bounded_limit=$1
    shift
    "$PYTHON3" - "$bounded_limit" "$@" <<'PY'
import os
import signal
import subprocess
import sys

limit = int(sys.argv[1])
command = sys.argv[2:]


def terminated(signum, _frame):
    raise SystemExit(128 + signum)


signal.signal(signal.SIGTERM, terminated)
signal.signal(signal.SIGINT, terminated)

status = 1
proc = subprocess.Popen(command, stdin=subprocess.DEVNULL, start_new_session=True)
try:
    try:
        code = proc.wait(timeout=limit)
        status = code if code >= 0 else 128 - code
    except subprocess.TimeoutExpired:
        print("service qualification: suite exceeded its %ds wall-clock bound; "
              "killing its process group" % limit, file=sys.stderr, flush=True)
        status = 124
finally:
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass
    proc.wait()
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

# remaining_seconds CAP STARTED: sets limit_left to how long a command may still
# run for the suite currently being run, which started at STARTED (a $SECONDS
# value): the lesser of what is left of that suite's own CAP and what is left of
# the aggregate budget after reserving MIN_SUITE_SECONDS for every suite that has
# not run yet. The budget share is never below MIN_SUITE_SECONDS, so every suite
# is always attempted (this clamp only matters against small per-suite overhead
# drift; the reservations make it unreachable otherwise).
remaining_seconds() {
    suite_left=$(($1 - (SECONDS - $2)))
    later_suites=$((EXPECTED_SUITE_COUNT - suite_count - 1))
    [ "$later_suites" -ge 0 ] || later_suites=0
    budget_left=$((TOTAL_BUDGET_SECONDS - (SECONDS - script_started) - later_suites * MIN_SUITE_SECONDS))
    [ "$budget_left" -ge "$MIN_SUITE_SECONDS" ] || budget_left=$MIN_SUITE_SECONDS
    limit_left=$suite_left
    [ "$budget_left" -ge "$suite_left" ] || limit_left=$budget_left
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
    remaining_seconds "$SUITE_TIMEOUT_SECONDS" "$suite_started"
    run_closed "$limit_left" "$suite_extra" "$@" || suite_status=$?
    record_suite "$suite_name" "$suite_status" "$((SECONDS - suite_started))"
    check_checkout "$suite_name"
}

run_suite tests/test_common.sh '' /bin/bash tests/test_common.sh
run_suite tests/test_service_credentials.sh '' /bin/bash tests/test_service_credentials.sh
run_suite tests/test_native_install.sh '' /bin/bash tests/test_native_install.sh
run_suite tests/test_service_supervision.py '' "$PYTHON3" tests/test_service_supervision.py

# test_service_transport_dispatch.py requires NATIVE_HELPER at import time: the
# compiled relay used for strict JSON/origin commands. Only test_native_install.sh
# builds it, and it runs this file as a child. To run it standalone, build the
# same helper the same way (go build -o <helper> ./cmd/herdr-mobile-relay).
# The build only compiles; it does not run the relay. A failed build fails the
# suite without running it. The build and the tests share ONE suite bound
# (SUITE_TIMEOUT_SECONDS, measured from dispatch_started): the build gets at
# most HELPER_BUILD_TIMEOUT_SECONDS of it and the tests get whatever remains.
dispatch_name=tests/test_service_transport_dispatch.py
helper="$work_dir/native-helper"
dispatch_status=0
banner "$dispatch_name"
dispatch_started=$SECONDS
remaining_seconds "$SUITE_TIMEOUT_SECONDS" "$dispatch_started"
build_limit=$limit_left
[ "$build_limit" -le "$HELPER_BUILD_TIMEOUT_SECONDS" ] || build_limit=$HELPER_BUILD_TIMEOUT_SECONDS
printf 'building the NATIVE_HELPER relay binary for this suite\n'
run_closed "$build_limit" '' "$GO_BIN" build -o "$helper" ./cmd/herdr-mobile-relay ||
    dispatch_status=$?
if [ "$dispatch_status" -ne 0 ]; then
    printf 'NATIVE_HELPER build failed; the suite is not run\n' >&2
else
    remaining_seconds "$SUITE_TIMEOUT_SECONDS" "$dispatch_started"
    if [ "$limit_left" -lt 1 ]; then
        printf 'the suite bound was spent by the helper build; the suite is not run\n' >&2
        dispatch_status=124
    else
        run_closed "$limit_left" "NATIVE_HELPER=$helper" "$PYTHON3" "$dispatch_name" ||
            dispatch_status=$?
    fi
fi
record_suite "$dispatch_name" "$dispatch_status" "$((SECONDS - dispatch_started))"
check_checkout "$dispatch_name"

run_suite tests/test_service_uninstall_cli.py '' "$PYTHON3" tests/test_service_uninstall_cli.py
run_suite tests/test_uninstall.sh '' /bin/bash tests/test_uninstall.sh
# The launcher-admission variable is set for this one suite only, as in the Makefile.
run_suite tests/test_tailscale_cli_setup_failure.py HERDR_TAILSCALE_LAUNCHER_CI=1 \
    "$PYTHON3" tests/test_tailscale_cli_setup_failure.py
run_suite tests/test_release_scripts.sh '' /bin/sh tests/test_release_scripts.sh

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
    "$BASH_VERSION" "$python_version" "$checkout_unchanged" "$overall" "$records" <<'PY' || summary_status=$?
import json
import os
import sys

(path, candidate_sha, runner_os, runner_arch, bash_version, python_version,
 checkout_unchanged, result, records) = sys.argv[1:10]
suites = []
for line in records.splitlines():
    name, code, seconds = line.rsplit(" ", 2)
    suites.append({"name": name, "exit": int(code), "seconds": int(seconds)})
document = {
    "schema_version": 1,
    "candidate_sha": candidate_sha,
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

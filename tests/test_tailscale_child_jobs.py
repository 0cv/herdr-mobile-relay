#!/usr/bin/env python3
"""Isolated behavioral tests for Bash-managed direct-child generations."""
import os
from pathlib import Path
import shlex
import signal
import subprocess
import tempfile
import time
import unittest


if os.environ.get("HERDR_S9B2_SANDBOX") != "1":
    raise SystemExit("Refusing child-process tests outside the S9B2 sandbox")

if os.environ.get("HERDR_S9B2_MUTANT") == "1":
    common_path = Path(os.environ["HERDR_S9B2_OVERLAY"]) / "common.sh"
else:
    common_path = Path(os.environ["HERDR_S9B2_SOURCE"]) / "relay/common.sh"

COMMON = shlex.quote(str(common_path))


def run_bash(body, timeout=12):
    """Run fixture-only Bash code and forcibly contain leftovers on timeout."""
    process = subprocess.Popen(
        ["/bin/bash", "--noprofile", "--norc", "-c", body],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        stdout, stderr = process.communicate()
        raise AssertionError(f"sandbox fixture timed out\nstdout:\n{stdout}\nstderr:\n{stderr}")
    return process.returncode, stdout, stderr


class ChildJobTests(unittest.TestCase):
    def run_fixture(self, body, timeout=12):
        status, stdout, stderr = run_bash(body, timeout)
        self.assertEqual(status, 0, f"fixture exit {status}\nstdout:\n{stdout}\nstderr:\n{stderr}")
        return stdout

    def test_capture_returns_generation_jobspec(self):
        body = f'''\
set -u
. {COMMON}
pid=""
cleanup() {{ [ -z "$pid" ] || {{ kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }}; }}
trap cleanup EXIT
python3 -c 'import time; time.sleep(30)' &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
case "$job" in %[0-9]*) ;; *) echo "not a jobspec: $job" >&2; exit 3 ;; esac
[ "$(jobs -p "$job" 2>/dev/null)" = "$pid" ] || exit 4
child_job_active "$job" "$pid" || exit 5
printf 'captured=%s pid=%s\\n' "$job" "$pid"
'''
        output = self.run_fixture(body)
        self.assertRegex(output, r"captured=%[0-9]+ pid=[0-9]+")

    def test_terminal_job_record_is_reaped_without_signalling_repeatedly(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-terminal-") as directory:
            go_dir = Path(directory) / "go"
            go_dir.mkdir()
            kill_called = shlex.quote(str(Path(directory) / "kill-called"))
            go_directory = shlex.quote(str(go_dir))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); done=pathlib.Path(sys.argv[2]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)'); "
                "done.write_text('done')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
kill_called={kill_called}
go_directory={go_directory}
record_file="$go_directory/record"
done_file=""
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
kill() {{ printf '%s\\n' "$*" >> "$kill_called"; return 97; }}
jobs() {{
    if [ "${{1:-}}" = -l ] && [ -f "$done_file" ]; then
        printf '[%s]+ %s Done fixture\\n' "${{job#%}}" "$pid"
        return 0
    fi
    if [ "${{1:-}}" = -p ] && [ -f "$done_file" ]; then return 1; fi
    builtin jobs "$@"
}}
child_code={shlex.quote(child_code)}
for ((repeat=1; repeat<=25; repeat++)); do
    go_file="$go_directory/$repeat"
    done_file="$go_file.done"
    python3 -c "$child_code" "$go_file" "$done_file" &
    pid=$!
    job="$(capture_child_job "$pid")" || exit 2
    : > "$go_file"
    observed=""
    record_status=0
    for ((probe=0; probe<500; probe++)); do
        LC_ALL=C jobs -l "$job" > "$record_file" 2>/dev/null || true
        observed=""
        IFS= read -r observed < "$record_file" || true
        record_status=0
        _child_job_record_state "$pid" "${{job#%}}" "$observed" || record_status=$?
        [ "$record_status" -eq 2 ] && break
        sleep 0.01
    done
    [ "$record_status" -eq 2 ] || {{ printf 'terminal record timeout: pid=%s job=%s observed=%s status=%s\\n' "$pid" "$job" "$observed" "$record_status" >&2; exit 3; }}
    [ "$CHILD_JOB_STATE" = Done ] || exit 4
    LC_ALL=C jobs -p "$job" > "$record_file" 2>/dev/null || true
    mapping=""
    IFS= read -r mapping < "$record_file" || true
    [ -z "$mapping" ] || exit 5
    stop_child_job "$job" "$pid" INT 1 || exit 6
    pid=""
    job=""
    [ ! -e "$kill_called" ] || exit 7
done
printf 'terminal jobs reaped without kill across 25 transitions\\n'
'''
            output = self.run_fixture(body, timeout=30)
            self.assertIn("25 transitions", output)

    def test_terminal_record_is_classified_before_pid_probe(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-terminal-order-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            calls = shlex.quote(str(Path(directory) / "calls"))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); done=pathlib.Path(sys.argv[2]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)'); "
                "done.write_text('done')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
go_file={go_file}
done_file="$go_file.done"
calls={calls}
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} "$go_file" "$done_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
job_number="${{job#%}}"
: > "$go_file"
for ((probe=0; probe<500; probe++)); do
    [ -f "$done_file" ] && break
    sleep 0.01
done
[ -f "$done_file" ] || {{ printf 'terminal-order timeout: pid=%s job=%s\\n' "$pid" "$job" >&2; exit 3; }}
jobs() {{
    printf '%s\\n' "${{1:-}}" >> "$calls"
    if [ "${{1:-}}" = -l ]; then
        printf '[%s]+ %s Done fixture\\n' "$job_number" "$pid"
        return 0
    fi
    return 1
}}
kill() {{ return 97; }}
stop_child_job "$job" "$pid" INT 1 || exit 4
[ "$(wc -l < "$calls" | tr -d ' ')" = 1 ] || exit 5
[ "$(cat "$calls")" = -l ] || exit 6
pid=""
printf 'terminal record classified before any PID mapping probe\\n'
'''
            output = self.run_fixture(body, timeout=15)
            self.assertIn("before any PID mapping probe", output)

    def test_active_to_terminal_transition_is_reclassified_once(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-transition-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            armed = shlex.quote(str(Path(directory) / "armed"))
            calls = shlex.quote(str(Path(directory) / "calls"))
            reclassified = shlex.quote(str(Path(directory) / "reclassified"))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
go_file={go_file}
reclassified={reclassified}
armed={armed}
calls={calls}
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} "$go_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
: > "$armed"
jobs() {{
    printf '%s %s\\n' "${{1:-}}" "${{2:-}}" >> "$calls"
    if [ "${{1:-}}" = -l ] && [ -e "$reclassified" ]; then
        printf '[%s]+ %s Done fixture\\n' "${{job#%}}" "$pid"
        return 0
    fi
    if [ "${{1:-}}" = -p ] && [ -e "$armed" ]; then
        rm -f "$armed"
        : > "$go_file"
        : > "$reclassified"
        return 1
    fi
    builtin jobs "$@"
}}
kill() {{ return 97; }}
if stop_child_job "$job" "$pid" INT 1; then
    :
else
    status=$?
    LC_ALL=C jobs -l "$job" >&2 || true
    printf 'transition fixture stop failed: status=%s pid=%s job=%s reclassified=%s calls=%s\\n' \
        "$status" "$pid" "$job" "$([ -e "$reclassified" ] && echo yes || echo no)" "$(cat "$calls")" >&2
    exit 3
fi
[ "$(wc -l < "$calls" | tr -d ' ')" = 3 ] || exit 4
[ "$(sed -n '1p' "$calls")" = "-l $job" ] || exit 5
[ "$(sed -n '2p' "$calls")" = "-p $job" ] || exit 6
[ "$(sed -n '3p' "$calls")" = "-l $job" ] || exit 7
pid=""
printf 'active-to-terminal reclassified with one fresh long probe\\n'
'''
            output = self.run_fixture(body, timeout=15)
            self.assertIn("one fresh long probe", output)

    def test_pruned_terminal_jobspec_remains_a_refusal(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-pruned-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            done_file = shlex.quote(str(Path(directory) / "done"))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); done=pathlib.Path(sys.argv[2]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)'); "
                "done.write_text('done')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
go_file={go_file}
done_file={done_file}
pruned=false
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; wait "$pid" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} "$go_file" "$done_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
: > "$go_file"
for ((probe=0; probe<500; probe++)); do
    [ -f "$done_file" ] && break
    sleep 0.01
done
[ -f "$done_file" ] || {{ printf 'pruned-record timeout: pid=%s job=%s\\n' "$pid" "$job" >&2; exit 3; }}
jobs() {{
    if [ "${{1:-}}" = -l ] && [ "$pruned" = false ]; then
        pruned=true
        printf '[%s]+ %s Done fixture\\n' "${{job#%}}" "$pid"
        return 0
    fi
    return 1
}}
if child_job_active "$job" "$pid"; then exit 5; fi
if stop_child_job "$job" "$pid" INT 1; then exit 6; fi
wait "$pid" 2>/dev/null || true
pid=""
printf 'pruned terminal jobspec refused\\n'
'''
            output = self.run_fixture(body, timeout=15)
            self.assertIn("pruned terminal jobspec refused", output)

    def test_verified_terminal_record_survives_subsequent_bash_pruning(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-verified-terminal-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            kill_called = shlex.quote(str(Path(directory) / "kill-called"))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
go_file={go_file}
kill_called={kill_called}
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} "$go_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
: > "$go_file"
wait "$job" 2>/dev/null || true
terminal_record=true
jobs() {{
    if [ "${{1:-}}" = -l ] && [ "$terminal_record" = true ]; then
        terminal_record=false
        printf '[%s]+ %s Done fixture\\n' "${{job#%}}" "$pid"
        return 0
    fi
    return 1
}}
kill() {{ : > "$kill_called"; return 97; }}
if child_job_active "$job" "$pid"; then exit 3; fi
stop_child_job "$job" "$pid" INT 1 || exit 4
[ ! -e "$kill_called" ] || exit 5
pid=""
job=""
printf 'validated terminal generation remains complete after Bash prunes its record\\n'
'''
            output = self.run_fixture(body, timeout=15)
            self.assertIn("after Bash prunes its record", output)

    def test_terminal_record_with_wrong_pid_refuses_and_foreign_survives(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-wrong-pid-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            child_code = (
                "import pathlib,signal,sys,time; "
                "signal.signal(signal.SIGINT,signal.SIG_IGN); "
                "gate=pathlib.Path(sys.argv[1]); deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)')"
            )
            foreign_code = (
                "import signal,time; signal.signal(signal.SIGINT,signal.SIG_IGN); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\\
set -u
. {COMMON}
foreign_pid=""
foreign_job=""
pid=""
job=""
go_file={go_file}
cleanup() {{
    [ -z "$pid" ] || {{ : > "$go_file"; builtin kill -KILL "$job" 2>/dev/null || true; wait "$job" 2>/dev/null || true; }}
    [ -z "$foreign_pid" ] || {{ builtin kill -KILL "$foreign_job" 2>/dev/null || true; wait "$foreign_job" 2>/dev/null || true; }}
}}
trap cleanup EXIT
python3 -c {shlex.quote(foreign_code)} &
foreign_pid=$!
foreign_job="$(capture_child_job "$foreign_pid")" || exit 2
python3 -c {shlex.quote(child_code)} "$go_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 3
job_number="${{job#%}}"
jobs() {{
    if [ "${{1:-}}" = -l ]; then
        : > "$go_file"
        printf '[%s]+ %s Done foreign fixture\\n' "$job_number" "$foreign_pid"
        return 0
    fi
    return 1
}}
if stop_child_job "$job" "$pid" INT 1; then exit 4; fi
builtin kill -0 "$foreign_pid" 2>/dev/null || exit 5
wait "$job" 2>/dev/null || true
pid=""
printf 'wrong terminal PID refused; foreign fixture survived\\n'
'''
            output = self.run_fixture(body, timeout=12)
            self.assertIn("foreign fixture survived", output)

    def test_terminal_record_with_wrong_job_number_refuses(self):
        self.assert_terminal_record_refused("wrong-number")

    def test_terminal_record_with_multiline_output_refuses(self):
        self.assert_terminal_record_refused("multiline")

    def assert_terminal_record_refused(self, variant):
        with tempfile.TemporaryDirectory(prefix="s9b3-malformed-") as directory:
            go_file = shlex.quote(str(Path(directory) / "go"))
            child_code = (
                "import pathlib,sys,time; gate=pathlib.Path(sys.argv[1]); "
                "deadline=time.monotonic()+5; "
                "exec('while not gate.exists() and time.monotonic() < deadline: time.sleep(0.001)')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
go_file={go_file}
variant={shlex.quote(variant)}
cleanup() {{ if [ -n "$pid" ]; then : > "$go_file"; builtin kill -KILL "$job" 2>/dev/null || true; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} "$go_file" &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
job_number="${{job#%}}"
if [ "$variant" = wrong-number ]; then
    listed_job=$((job_number + 1))
else
    listed_job="$job_number"
fi
jobs() {{
    if [ "${{1:-}}" = -l ]; then
        : > "$go_file"
        printf '[%s]+ %s Done fixture\\n' "$listed_job" "$pid"
        if [ "$variant" = multiline ]; then printf '  %s Running extra-process\\n' "$pid"; fi
        return 0
    fi
    return 1
}}
if stop_child_job "$job" "$pid" INT 1; then exit 3; fi
wait "$pid" 2>/dev/null || true
pid=""
printf '%s terminal jobspec refused\\n' "$variant"
'''
            output = self.run_fixture(body, timeout=12)
            self.assertIn(f"{variant} terminal jobspec refused", output)

    def test_stopped_job_is_continued_before_signal(self):
        with tempfile.TemporaryDirectory(prefix="s9b3-stopped-") as directory:
            ready = shlex.quote(str(Path(directory) / "ready"))
            marker = shlex.quote(str(Path(directory) / "int-received"))
            continued = shlex.quote(str(Path(directory) / "continued"))
            signals = shlex.quote(str(Path(directory) / "signals"))
            child_code = (
                "import pathlib,signal,sys,time; ready,marker,continued=sys.argv[1:4]; "
                "signal.signal(signal.SIGCONT,lambda *_: pathlib.Path(continued).write_text('CONT')); "
                "handler=lambda *_: (pathlib.Path(marker).write_text('INT'), sys.exit(0)); "
                "signal.signal(signal.SIGINT,handler); pathlib.Path(ready).write_text('ready'); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\\
set -u
. {COMMON}
pid=""
job=""
continued={continued}
marker={marker}
signals={signals}
cleanup() {{ if [ -n "$pid" ]; then builtin kill -CONT "$job" 2>/dev/null || true; builtin kill -KILL "$job" 2>/dev/null || true; wait "$job" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} {ready} {marker} {continued} &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
for ((i=0; i<200; i++)); do [ -f {ready} ] && break; sleep 0.01; done
[ -f {ready} ] || exit 3
builtin kill -STOP "$pid" 2>/dev/null || exit 4
jobs() {{
    if [ "${{1:-}}" = -l ]; then
        if [ -f "$marker" ]; then state=Done
        elif [ -f "$continued" ]; then state=Running
        else state=Stopped
        fi
        printf '[%s]+ %s %s fixture\\n' "${{job#%}}" "$pid" "$state"
        return 0
    fi
    builtin jobs "$@"
}}
kill() {{
    printf '%s %s\\n' "$1" "$2" >> "$signals"
    builtin kill "$@" || return $?
    [ "$1" != -CONT ] || : > "$continued"
}}
stop_child_job "$job" "$pid" INT 2 || exit 5
[ -f "$continued" ] || exit 6
[ -f "$marker" ] || exit 7
[ "$(sed -n '1p' "$signals")" = "-CONT $job" ] || exit 8
[ "$(sed -n '2p' "$signals")" = "-INT $job" ] || exit 9
pid=""
printf 'stopped job continued and received INT\\n'
'''
            output = self.run_fixture(body, timeout=12)
            self.assertIn("continued and received INT", output)

    def test_owned_child_receives_signal_and_is_reaped(self):
        with tempfile.TemporaryDirectory(prefix="s9b2-owned-") as directory:
            ready = shlex.quote(str(Path(directory) / "ready"))
            marker = shlex.quote(str(Path(directory) / "int-received"))
            record = shlex.quote(str(Path(directory) / "record"))
            child_code = (
                "import pathlib,signal,sys,time; "
                "ready,marker=sys.argv[1:3]; "
                "handler=lambda *_: (pathlib.Path(marker).write_text('INT'), sys.exit(0)); "
                "signal.signal(signal.SIGINT, handler); "
                "pathlib.Path(ready).write_text('ready'); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\
set -u
. {COMMON}
pid=""
job=""
record={record}
cleanup() {{ if [ -n "$pid" ]; then kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} {ready} {marker} &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
for ((i=0; i<200; i++)); do [ -f {ready} ] && break; sleep 0.01; done
[ -f {ready} ] || exit 3
jobs() {{
    if [ "${{1:-}}" = -l ] && [ -f {marker} ]; then
        printf '[%s]+ %s Done fixture\\n' "${{job#%}}" "$pid"
        return 0
    fi
    builtin jobs "$@"
}}
if stop_child_job "$job" "$pid" INT 1; then
    :
else
    status=$?
    LC_ALL=C jobs -l "$job" > "$record" 2>/dev/null || true
    observed=""
    IFS= read -r observed < "$record" || true
    printf 'owned-child stop failed: status=%s pid=%s job=%s marker=%s record=%s\\n' \
        "$status" "$pid" "$job" "$([ -f {marker} ] && echo yes || echo no)" "$observed" >&2
    ps -o pid,ppid,pgid,sid,stat -p "$pid" >&2 || true
    exit 4
fi
[ -f {marker} ] || exit 5
if child_job_active "$job" "$pid"; then exit 6; fi
pid=""
printf 'stopped=%s reaped=waited\\n' "$job"
'''
            output = self.run_fixture(body)
            self.assertIn("reaped=waited", output)

    def test_stale_jobspec_refuses_foreign_pid(self):
        with tempfile.TemporaryDirectory(prefix="s9b2-stale-") as directory:
            ready = shlex.quote(str(Path(directory) / "ready"))
            marker = shlex.quote(str(Path(directory) / "int-received"))
            child_code = (
                "import pathlib,signal,sys,time; "
                "ready,marker=sys.argv[1:3]; "
                "signal.signal(signal.SIGINT, signal.SIG_IGN); "
                "pathlib.Path(ready).write_text('ready'); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\
set -u
. {COMMON}
foreign_pid=""
foreign_job=""
cleanup() {{ if [ -n "$foreign_pid" ] && kill -0 "$foreign_pid" 2>/dev/null; then kill -KILL "$foreign_job" 2>/dev/null || kill -KILL "$foreign_pid" 2>/dev/null || true; fi; if [ -n "$foreign_pid" ]; then wait "$foreign_pid" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} {ready} {marker} &
foreign_pid=$!
foreign_job="$(capture_child_job "$foreign_pid")" || exit 2
for ((i=0; i<200; i++)); do [ -f {ready} ] && break; sleep 0.01; done
[ -f {ready} ] || exit 3
if stop_child_job '%999999999' "$foreign_pid" INT 1; then exit 4; fi
kill -0 "$foreign_pid" 2>/dev/null || exit 5
printf 'foreign=%s survived stale jobspec\\n' "$foreign_pid"
'''
            started = time.monotonic()
            output = self.run_fixture(body)
            self.assertLess(time.monotonic() - started, 0.5, "stale jobspec refusal was not prompt")
            self.assertIn("survived stale jobspec", output)

    def test_monitor_rejects_foreign_pid(self):
        with tempfile.TemporaryDirectory(prefix="s9b2-monitor-") as directory:
            ready = shlex.quote(str(Path(directory) / "ready"))
            child_code = (
                "import pathlib,sys,time; "
                "pathlib.Path(sys.argv[1]).write_text('ready'); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\
set -u
. {COMMON}
foreign_pid=""
foreign_job=""
cleanup() {{ if [ -n "$foreign_pid" ] && kill -0 "$foreign_pid" 2>/dev/null; then kill -KILL "$foreign_job" 2>/dev/null || kill -KILL "$foreign_pid" 2>/dev/null || true; fi; if [ -n "$foreign_pid" ]; then wait "$foreign_pid" 2>/dev/null || true; fi; }}
trap cleanup EXIT
python3 -c {shlex.quote(child_code)} {ready} &
foreign_pid=$!
foreign_job="$(capture_child_job "$foreign_pid")" || exit 2
for ((i=0; i<200; i++)); do [ -f {ready} ] && break; sleep 0.01; done
[ -f {ready} ] || exit 3
if child_job_active '%999999999' "$foreign_pid"; then exit 4; fi
kill -0 "$foreign_pid" 2>/dev/null || exit 5
printf 'foreign=%s rejected by monitor\\n' "$foreign_pid"
'''
            output = self.run_fixture(body)
            self.assertIn("rejected by monitor", output)

    def test_ignored_signal_escalates_to_kill(self):
        with tempfile.TemporaryDirectory(prefix="s9b2-escalate-") as directory:
            ready = shlex.quote(str(Path(directory) / "ready"))
            signals = shlex.quote(str(Path(directory) / "signals"))
            waits = shlex.quote(str(Path(directory) / "waits"))
            records = shlex.quote(str(Path(directory) / "jobs-records"))
            child_code = (
                "import pathlib,signal,sys,time; "
                "signal.signal(signal.SIGINT, signal.SIG_IGN); "
                "pathlib.Path(sys.argv[1]).write_text('ready'); "
                "exec('while True: time.sleep(0.05)')"
            )
            body = f'''\
set -u
. {COMMON}
pid=""
job=""
signals={signals}
waits={waits}
records={records}
: > "$signals"
: > "$waits"
: > "$records"
DIAG_AFTER_KILL=false
cleanup() {{
    if [ -n "$pid" ] && [ -n "$job" ] && child_job_active "$job" "$pid"; then
        builtin kill -KILL "$job" 2>/dev/null || true
        builtin wait "$pid" 2>/dev/null || true
    fi
}}
trap cleanup EXIT
kill() {{
    builtin kill "$@"
    local kill_status=$?
    printf 'signal=%s status=%s\\n' "$*" "$kill_status" >> "$signals"
    if [ "${{1:-}}" = -KILL ] && [ "$kill_status" -eq 0 ]; then
        DIAG_AFTER_KILL=true
    fi
    return "$kill_status"
}}
wait() {{
    builtin wait "$@"
    local wait_status=$?
    printf 'wait=%s status=%s\\n' "$*" "$wait_status" >> "$waits"
    return "$wait_status"
}}
jobs() {{
    local diag_jobs_status=0
    local diag_jobs_line=""
    local diag_phase=PREKILL
    if [ "${{1:-}}" = -l ] && [ -n "${{output_file:-}}" ]; then
        [ "$DIAG_AFTER_KILL" != true ] || diag_phase=POSTKILL
        builtin jobs "$@"
        diag_jobs_status=$?
        printf '%s args=%s status=%s\\n' "$diag_phase" "$*" "$diag_jobs_status" >> "$records"
        while IFS= read -r diag_jobs_line || [ -n "$diag_jobs_line" ]; do
            printf 'record=%s\\n' "$diag_jobs_line" >> "$records"
        done < "$output_file"
        return "$diag_jobs_status"
    fi
    builtin jobs "$@"
}}
process_state() {{
    local process_stat=""
    local process_rest=""
    if IFS= read -r process_stat < "/proc/$1/stat"; then
        process_rest="${{process_stat##*) }}"
        printf '%s\\n' "${{process_rest%% *}}"
    else
        printf 'gone\\n'
    fi
}}
python3 -c {shlex.quote(child_code)} {ready} &
pid=$!
job="$(capture_child_job "$pid")" || exit 2
for ((i=0; i<200; i++)); do [ -f {ready} ] && break; sleep 0.01; done
[ -f {ready} ] || exit 3
started=$SECONDS
if stop_child_job "$job" "$pid" INT 1; then stop_status=0; else stop_status=$?; fi
elapsed=$((SECONDS - started))
[ "$elapsed" -lt 3 ] || exit 4

mapfile -t observed_signals < "$signals"
[ "${{#observed_signals[@]}}" -eq 2 ] || exit 5
[ "${{observed_signals[0]}}" = "signal=-INT $job status=0" ] || exit 6
[ "${{observed_signals[1]}}" = "signal=-KILL $job status=0" ] || exit 7

job_number="${{job#%}}"
post_kill_query_count=0
last_post_kill_status=""
last_post_kill_record_count=0
post_kill_exact_killed=false
last_post_kill_exact_killed=false
in_post_kill_query=false
listed_job=""
listed_pid=""
listed_state=""
mapfile -t job_records < "$records"
for observation in "${{job_records[@]}}"; do
    case "$observation" in
        POSTKILL\\ args=*)
            in_post_kill_query=true
            post_kill_query_count=$((post_kill_query_count + 1))
            last_post_kill_status="${{observation##*status=}}"
            last_post_kill_record_count=0
            last_post_kill_exact_killed=false
            ;;
        PREKILL\\ args=*)
            in_post_kill_query=false
            ;;
        record=*)
            if [ "$in_post_kill_query" = true ]; then
                record_line="${{observation#record=}}"
                last_post_kill_record_count=$((last_post_kill_record_count + 1))
                listed_job=""
                listed_pid=""
                listed_state=""
                read -r listed_job listed_pid listed_state _ <<< "$record_line"
                case "$listed_job" in
                    "[$job_number]"|"[$job_number]+"|"[$job_number]-") ;;
                    *) continue ;;
                esac
                if [ "$listed_pid" = "$pid" ] && [ "$listed_state" = Killed ]; then
                    post_kill_exact_killed=true
                    last_post_kill_exact_killed=true
                fi
            fi
            ;;
    esac
done
[ "$post_kill_query_count" -gt 0 ] || exit 8

proc_state="$(process_state "$pid")"
case "$proc_state" in
    gone|Z|X) ;;
    *) printf 'direct child survived KILL: pid=%s state=%s\\n' "$pid" "$proc_state" >&2; exit 9 ;;
esac
cache_spec_count="${{#CHILD_JOB_REAPED_SPECS[@]}}"
cache_pid_count="${{#CHILD_JOB_REAPED_PIDS[@]}}"
mapfile -t product_waits < "$waits"

if [ "$stop_status" -eq 0 ]; then
    [ "$CHILD_JOB_RECORD_PRESENT" = true ] || exit 10
    [ "$post_kill_exact_killed" = true ] || exit 11
    [ "$cache_spec_count" -eq 1 ] && [ "$cache_pid_count" -eq 1 ] || exit 12
    [ "${{CHILD_JOB_REAPED_SPECS[0]}}" = "$job" ] || exit 13
    [ "${{CHILD_JOB_REAPED_PIDS[0]}}" = "$pid" ] || exit 14
    [ "${{#product_waits[@]}}" -eq 1 ] || exit 15
    [ "${{product_waits[0]}}" = "wait=$pid status=137" ] || exit 16
    [ "$proc_state" = gone ] || exit 17
    printf 'verified exact Killed record, PID wait status 137, and cached jobspec/PID\\n'
elif [ "$stop_status" -eq 1 ] && [ "$cache_spec_count" -eq 0 ] && [ "$cache_pid_count" -eq 0 ]; then
    if [ "$CHILD_JOB_RECORD_PRESENT" = false ] && [ "$last_post_kill_status" != 0 ] &&
        [ "$last_post_kill_record_count" -eq 0 ] && [ "$last_post_kill_exact_killed" = false ] &&
        [ "$post_kill_exact_killed" = false ] && [ "${{#product_waits[@]}}" -eq 0 ]; then
        refusal_reason=pruned-terminal-record
    elif [ "$CHILD_JOB_RECORD_PRESENT" = true ] && [ "$last_post_kill_exact_killed" = true ] &&
        [ "${{#product_waits[@]}}" -eq 1 ] &&
        [ "${{product_waits[0]}}" = "wait=$pid status=127" ]; then
        refusal_reason=wait-status-unavailable
    else
        printf 'unexpected fail-closed refusal state: record=%s last_status=%s last_records=%s wait_count=%s\\n' \\
            "$CHILD_JOB_RECORD_PRESENT" "$last_post_kill_status" "$last_post_kill_record_count" \\
            "${{#product_waits[@]}}" >&2
        exit 18
    fi
    if builtin wait "$pid" 2>/dev/null; then probe_wait_status=0; else probe_wait_status=$?; fi
    [ "$probe_wait_status" -eq 137 ] || [ "$probe_wait_status" -eq 127 ] || exit 19
    [ "$(process_state "$pid")" = gone ] || exit 20
    printf 'fail-closed refusal (%s): no reaped cache/repeat/foreign signal; direct child retired (wait %s)\\n' \\
        "$refusal_reason" "$probe_wait_status"
else
    printf 'unexpected stop status/cache: status=%s specs=%s pids=%s\\n' \\
        "$stop_status" "$cache_spec_count" "$cache_pid_count" >&2
    exit 21
fi
pid=""
'''
            output = self.run_fixture(body)
            self.assertTrue(
                "verified exact Killed record" in output or "fail-closed refusal (" in output,
                f"expected verified cleanup or fail-closed refusal, got: {output}",
            )


    def run_forced_kill_fixture(self, variant, scenario, wait_status=137, foreign=False):
        with tempfile.TemporaryDirectory(prefix="s9b3-forced-kill-") as directory:
            signals = shlex.quote(str(Path(directory) / "signals"))
            waits = shlex.quote(str(Path(directory) / "waits"))
            foreign_code = shlex.quote("import time; time.sleep(30)")
            body = f'''\\
set -u
. {COMMON}
job=%41
pid=1234
phase=active
variant={shlex.quote(variant)}
fixture_wait_status={wait_status}
signals={signals}
waits={waits}
exact_terminal_proof=false
foreign_pid=""
cleanup() {{
    if [ -n "$foreign_pid" ]; then
        builtin kill -KILL "$foreign_pid" 2>/dev/null || true
        wait "$foreign_pid" 2>/dev/null || true
    fi
}}
trap cleanup EXIT
jobs() {{
    if [ "${{1:-}}" = -l ]; then
        case "$phase" in
            active) printf '[41]+ 1234 Running fixture\\n'; return 0 ;;
            post-kill)
                case "$variant" in
                    missing) return 1 ;;
                    wrong-pid) printf '[41]+ %s Killed foreign\\n' "$foreign_pid" ;;
                    wrong-job) printf '[42]+ 1234 Killed foreign\\n' ;;
                    malformed) printf 'not a valid jobs record\\n' ;;
                    done) printf '[41]+ 1234 Done fixture\\n' ;;
                    exit) printf '[41]+ 1234 Exit 1 fixture\\n' ;;
                    unknown) printf '[41]+ 1234 Paused fixture\\n' ;;
                    killed)
                        printf '[41]+ 1234 Killed fixture\\n'
                        exact_terminal_proof=true
                        ;;
                    *) return 2 ;;
                esac
                return 0
                ;;
            pruned) return 1 ;;
            *) return 2 ;;
        esac
    fi
    if [ "${{1:-}}" = -p ] && [ "$phase" = active ]; then
        printf '1234\\n'
        return 0
    fi
    return 1
}}
kill() {{
    printf '%s %s\\n' "$1" "$2" >> "$signals"
    if [ "$1" = -KILL ]; then phase=post-kill; fi
}}
wait() {{
    printf '%s %s\\n' "$phase" "$*" >> "$waits"
    [ "$#" -eq 1 ] && [ "$1" = "$pid" ] &&
        [ "$exact_terminal_proof" = true ] || return 125
    return "$fixture_wait_status"
}}
sleep() {{ :; }}
if [ {1 if foreign else 0} -eq 1 ]; then
    python3 -c {foreign_code} &
    foreign_pid=$!
fi
{scenario}'''
            return self.run_fixture(body)

    def test_forced_kill_missing_record_refuses_without_caching(self):
        output = self.run_forced_kill_fixture("missing", '''\
if stop_child_job "$job" "$pid" INT 1; then exit 3; fi
if stop_child_job "$job" "$pid" INT 1; then exit 4; fi
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 0 ] || exit 5
[ ! -s "$waits" ] || exit 6
[ "$(wc -l < "$signals" | tr -d ' ')" = 2 ] || exit 7
printf 'missing post-KILL record refused on repeated cleanup without caching\\n'
''')
        self.assertIn("repeated cleanup without caching", output)

    def test_forced_kill_mismatched_terminal_record_refuses_and_foreign_survives(self):
        for variant, foreign in (("wrong-pid", True), ("wrong-job", False), ("malformed", False)):
            with self.subTest(variant=variant):
                output = self.run_forced_kill_fixture(variant, '''\
for repeat in 1 2; do
    if stop_child_job "$job" "$pid" INT 1; then exit 3; fi
done
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 0 ] || exit 4
[ ! -s "$waits" ] || exit 5
[ "$(wc -l < "$signals" | tr -d ' ')" = 2 ] || exit 6
if [ -n "$foreign_pid" ]; then
    builtin kill -0 "$foreign_pid" 2>/dev/null || exit 7
fi
printf '%s post-KILL record refused repeatedly without waiting or caching\\n' "$variant"
''', foreign=foreign)
                self.assertIn("refused repeatedly without waiting or caching", output)

    def test_forced_kill_invalid_wait_status_refuses_without_caching(self):
        for wait_status in (0, 1, 127):
            with self.subTest(wait_status=wait_status):
                output = self.run_forced_kill_fixture("killed", '''\
if stop_child_job "$job" "$pid" INT 1; then exit 3; fi
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 0 ] || exit 4
[ "$(cat "$waits")" = "post-kill $pid" ] || exit 5
printf 'wait status %s refused without caching after exact Killed proof\\n' "$fixture_wait_status"
''', wait_status=wait_status)
                self.assertIn(f"wait status {wait_status} refused without caching", output)

    def test_forced_kill_killed_record_and_wait_137_allow_pruned_cache(self):
        output = self.run_forced_kill_fixture("killed", '''\
stop_child_job "$job" "$pid" INT 1 || exit 3
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 1 ] || exit 4
[ "${CHILD_JOB_REAPED_SPECS[0]}" = "$job" ] || exit 5
[ "${CHILD_JOB_REAPED_PIDS[0]}" = "$pid" ] || exit 6
[ "$(cat "$waits")" = "post-kill $pid" ] || exit 7
phase=pruned
stop_child_job "$job" "$pid" INT 1 || exit 8
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 1 ] || exit 9
[ "$(wc -l < "$signals" | tr -d ' ')" = 2 ] || exit 10
[ "$(wc -l < "$waits" | tr -d ' ')" = 1 ] || exit 11
printf 'exact Killed record and wait 137 cached; pruned record accepted without another signal\\n'
''', wait_status=137)
        self.assertIn("without another signal", output)

    def test_forced_kill_unexpected_terminal_state_refuses_without_caching(self):
        for variant in ("done", "exit", "unknown"):
            with self.subTest(variant=variant):
                output = self.run_forced_kill_fixture(variant, '''\
if stop_child_job "$job" "$pid" INT 1; then exit 3; fi
[ "${#CHILD_JOB_REAPED_SPECS[@]}" -eq 0 ] || exit 4
[ ! -s "$waits" ] || exit 5
printf 'unexpected %s state after KILL refused without caching\\n' "$variant"
''')
                self.assertIn(f"unexpected {variant} state after KILL refused without caching", output)


if __name__ == "__main__":
    unittest.main(verbosity=2)

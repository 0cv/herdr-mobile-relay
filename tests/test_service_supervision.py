#!/usr/bin/env python3
"""Hosted-only A1-F001/F002 regression SOURCE; authoring is not execution.

Copies the actual service/common bytes. Children and forbidden tools are inert.
BASH_ENV instruments parent builtins, not a replacement supervisor: real jobs,
kill and wait run in the owning Bash. Explicit observation gates control races.
Run on separately admitted disposable Linux Bash and Darwin /bin/bash 3.2.
No manager, network, Tailscale, installed service or projection is executed.
"""
import os
import sys

# Truthful admission, before fixture creation or subprocesses; never set these.
if (os.environ.get("GITHUB_ACTIONS") != "true"
        or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"):
    raise SystemExit("service supervision fixtures require disposable GitHub-hosted CI")

import json
import pathlib
import plistlib
import shutil
import signal
import subprocess
import tempfile
import time

REPO = pathlib.Path(__file__).resolve().parents[1]
BASH = os.environ.get("SERVICE_TEST_BASH", "/bin/bash")
assert pathlib.Path(BASH).is_absolute(), "fixture Bash must be explicitly absolute"
DEADLINE = 20

CHILD = r'''
import json, os, pathlib, signal, sys, time
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
kind = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
def event(action, **values):
    with (root / (kind + ".events")).open("a") as out:
        out.write(json.dumps(dict(action=action, pid=os.getpid(), **values)) + "\n")
if kind == "relay" and args == ["normalize-origin", "--allow-loopback-http", "https://gateway.example.test"]:
    print("https://gateway.example.test")
    raise SystemExit(0)
if kind == "relay":
    assert args == ["serve-installed"], "wrong installed dispatch"
elif kind == "cloudflared":
    assert args == ["tunnel", "--config", str(root / "tunnel.yml"), "run"]
elif kind != "bystander":
    event("forbidden")
    raise SystemExit(99)
mode = os.environ.get("MODE_" + kind.upper(), "normal")
def term(signum, frame):
    event("term")
    if mode == "ignore":
        return
    if mode == "restop":
        event("restop")
        os.kill(os.getpid(), signal.SIGSTOP)
        return
    event("exit", code=0)
    raise SystemExit(0)
def cont(signum, frame):
    event("cont")
signal.signal(signal.SIGTERM, term)
signal.signal(signal.SIGINT, term)
signal.signal(signal.SIGCONT, cont)
def signal_exit(signum):
    # Python ignores SIGPIPE by default. Restore the genuine signal disposition
    # so Bash must observe/reap a signaled child, not a synthetic exit(141).
    event("signal-exit", signal=signum)
    signal.signal(signum, signal.SIG_DFL)
    os.kill(os.getpid(), signum)
    raise AssertionError("terminal signal did not terminate fixture child")
event("start")
if mode == "immediate-signal":
    signal_exit(int(os.environ["CHILD_SIGNAL"]))
if mode == "immediate":
    event("exit", code=int(os.environ["CHILD_CODE"]))
    raise SystemExit(int(os.environ["CHILD_CODE"]))
while True:
    signal_path = root / (kind + ".signal")
    if signal_path.exists():
        signal_exit(int(signal_path.read_text()))
    path = root / (kind + ".exit")
    if path.exists():
        code = int(path.read_text())
        event("exit", code=code)
        raise SystemExit(code)
    time.sleep(.01)
'''

# All records are synthetic fixture-local data. No process discovery or manager.
# Instrumentation performs no asynchronous launches except the owned bystander
# and a deliberately completed unrelated generation BEFORE service startup.
BUILTINS = r'''
fixture_capture_count=0
fixture_job_count=0
fixture_kill_count=0
fixture_gate_used=0
fixture_trace() { printf '%s\n' "$*" >> "$FIXTURE_ROOT/shell.events"; }
fixture_gate() {
    local name="$1" attempt deadline
    fixture_trace "gate $name"
    : > "$FIXTURE_ROOT/$name.held"
    if [ "$name" = capture ]; then
        # The capture gate holds the parent BEFORE its first job listing so a child
        # can terminate first. A forked /bin/sleep here would make Bash report and
        # delete a signal-terminated child's job entry while the gate waits, so the
        # listing could never show the terminal record this gate exists to present
        # (the product itself forks only a short mktemp before that listing). This
        # loop forks nothing (builtins only, bounded by SECONDS), so the gate leaves
        # the job table as an immediate capture would have found it.
        deadline=$((SECONDS + 10))
        while [ ! -f "$FIXTURE_ROOT/$name.release" ]; do
            [ "$SECONDS" -lt "$deadline" ] || { fixture_trace "gate-timeout $name"; return 1; }
        done
        return 0
    fi
    for ((attempt=0; attempt<1000; attempt++)); do
        [ ! -f "$FIXTURE_ROOT/$name.release" ] || return 0
        /bin/sleep 0.01
    done
    fixture_trace "gate-timeout $name"
    return 1
}
fixture_trace "bash $BASH_VERSION"
if [ "${WITH_GENERATIONS:-0}" = 1 ]; then
    (
        for ((prior_attempt=0; prior_attempt<1000; prior_attempt++)); do
            [ ! -f "$FIXTURE_ROOT/prior.release" ] || exit 0
            /bin/sleep 0.01
        done
        exit 99
    ) &
    fixture_prior_pid=$!
    builtin jobs -l %+ > "$FIXTURE_ROOT/prior.record"
    : > "$FIXTURE_ROOT/prior.release"
    builtin wait "$fixture_prior_pid"
    fixture_trace "prior-reaped $fixture_prior_pid"
fi
"$FIXTURE_ROOT/bin/bystander" >/dev/null 2>&1 &
fixture_bystander_pid=$!
builtin jobs -l %+ > "$FIXTURE_ROOT/bystander.record"
fixture_trace "bystander $fixture_bystander_pid"
jobs() {
    local rc=0 line token pid state rest attempt terminal_gate=0 corrupt_pid expected_pid
    fixture_job_count=$((fixture_job_count+1))
    fixture_trace "jobs $BASH_SUBSHELL $*"
    if [ "$*" = '-l' ]; then
        fixture_capture_count=$((fixture_capture_count+1))
        expected_pid="${RELAY_PID:-}"
        [ "$fixture_capture_count" != 2 ] || expected_pid="${TUNNEL_PID:-}"
        if [ "${CAPTURE_GATE:-0}" = "$fixture_capture_count" ]; then
            fixture_gate capture || return 1
        fi
    elif [ "$1" = -l ] && [ "$2" = "${RELAY_JOB:-}" ] &&
        [ "$fixture_gate_used" = 0 ] && [ -f "$FIXTURE_ROOT/arm-poll" ]; then
        fixture_gate_used=1
        fixture_gate poll || return 1
        [ "${POLL_TERMINAL:-0}" != 1 ] || terminal_gate=1
    fi
    # In completion gates, preserve and return the ACTUAL terminal builtin
    # record on its FIRST terminal observation. Do not prune it and reread.
    for ((attempt=0; attempt<1000; attempt++)); do
        rc=0
        builtin jobs "$@" > "$FIXTURE_ROOT/job.record" 2>/dev/null || rc=$?
        line=''
        IFS= read -r line < "$FIXTURE_ROOT/job.record" || true
        IFS=' ' read -r token pid state rest <<< "$line"
        [ "$terminal_gate" = 1 ] || break
        case "$state" in Done|Exit|Terminated|Killed) break ;; esac
        [ "$rc" = 0 ] && [ -n "$line" ] || return 1
        /bin/sleep 0.01
    done
    [ "$attempt" -lt 1000 ] || return 1
    if [ "$*" = '-l' ]; then
        while IFS= read -r line; do
            IFS=' ' read -r token pid state rest <<< "$line"
            fixture_trace "record $token $pid $state"
            if [ "$pid" = "$expected_pid" ]; then
                fixture_trace "capture $fixture_capture_count $token $pid $state"
                if [ "${CAPTURE_MISMATCH:-0}" = "$fixture_capture_count" ]; then
                    printf '%s %s Running unrelated-generation\n' "$token" "$fixture_bystander_pid"
                    continue
                fi
                [ "${CAPTURE_UNKNOWN:-0}" != "$fixture_capture_count" ] || continue
            fi
            printf '%s\n' "$line"
        done < "$FIXTURE_ROOT/job.record"
        return "$rc"
    fi
    fixture_trace "record $token $pid $state"
    # Narrow corruption seam, armed only AFTER both captures: a current spec
    # mapped to another actual fixture job, or a missing/pruned record. Neither
    # is signal authority for the original captured child.
    corrupt_pid="${RELAY_PID:-}"
    [ "${CORRUPT_CHILD:-relay}" != cloudflared ] || corrupt_pid="${TUNNEL_PID:-}"
    if [ -f "$FIXTURE_ROOT/mismatch" ] && [ "$pid" = "$corrupt_pid" ]; then
        printf '%s %s Running unrelated-generation\n' "$token" "$fixture_bystander_pid"
        return 0
    fi
    if [ -f "$FIXTURE_ROOT/unknown" ] && [ "$pid" = "$corrupt_pid" ]; then
        return 1
    fi
    [ -z "$line" ] || printf '%s\n' "$line"
    return "$rc"
}
kill() {
    local rc=0
    fixture_kill_count=$((fixture_kill_count+1))
    fixture_trace "signal $*"
    if [ "$1" = -TERM ] && [ -f "$FIXTURE_ROOT/arm-signal" ]; then
        /bin/rm -f "$FIXTURE_ROOT/arm-signal"
        if [ "${SIGNAL_FAIL_RACE:-0}" = 1 ]; then
            fixture_gate signal || return 1
            return 1
        fi
        builtin kill "$@" 2>/dev/null || rc=$?
        fixture_gate signal || return 1
        return "$rc"
    fi
    builtin kill "$@" 2>/dev/null || rc=$?
    return "$rc"
}
wait() {
    local rc=0
    fixture_trace "wait-begin $*"
    builtin wait "$@" 2>/dev/null || rc=$?
    fixture_trace "wait-end $* $rc"
    return "$rc"
}
'''


def write(path, text, executable=False):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o700 if executable else 0o600)


def until(predicate, message, proc=None, seconds=5):
    deadline = time.monotonic() + seconds
    while not predicate():
        assert time.monotonic() < deadline, message
        assert proc is None or proc.poll() is None, message + " (supervisor exited)"
        time.sleep(.01)


def alive(pid):
    try:
        os.kill(pid, 0)  # fixture PID only, observation, never product authority
        return True
    except ProcessLookupError:
        return False


class Fixture:
    def __init__(self, parent, name, transport="cloudflare", **extra):
        self.root = parent.resolve() / name
        self.launchd = extra.pop("LAUNCHD_START", False)
        self.root.mkdir(mode=0o700)
        self.scripts = self.root / "relay"
        self.scripts.mkdir(mode=0o700)
        for name in ("common.sh", "herdr-mobile-relay-service.sh", "tailscale-cli-service.sh", "install-service.sh"):
            shutil.copy2(REPO / "relay" / name, self.scripts / name)
            assert (self.scripts / name).read_bytes() == (REPO / "relay" / name).read_bytes()
        adapter = "#!" + sys.executable + "\n" + CHILD
        for name in ("relay", "cloudflared", "bystander", "tailscale", "tailscaled", "herdr",
                     "curl", "systemctl", "launchctl", "sudo", "ssh", "go", "git"):
            write(self.root / "bin" / name, adapter, True)
        write(self.root / "builtins.sh", BUILTINS)
        write(self.root / "tunnel.yml", "inert tunnel config\n")
        text = "HERDR_RELAY_TRANSPORT=" + transport + "\n"
        if transport == "gateway":
            text += "HERDR_GATEWAY_URL=wss://gateway.example.test\n"
        write(self.root / "relay.env", text)
        self.before = (self.root / "relay.env").read_bytes()
        self.env = dict(HOME=str(self.root), PATH=str(self.root / "bin") + ":/usr/bin:/bin",
                        TMPDIR=str(self.root), FIXTURE_ROOT=str(self.root),
                        BASH_ENV=str(self.root / "builtins.sh"),
                        HERDR_RELAY_ENV=str(self.root / "relay.env"),
                        HERDR_RELAY_BIN=str(self.root / "bin/relay"),
                        CLOUDFLARED_BIN=str(self.root / "bin/cloudflared"),
                        CLOUDFLARED_CONFIG=str(self.root / "tunnel.yml"), **extra)
        self.transport = transport
        self.proc = None
        self.output = self.root / "output"
        self.stream = None

    def start(self):
        # A file, not inherited stdout pipes: a leaked child cannot turn
        # communicate into an accidental hang or a false successful cleanup.
        self.stream = self.output.open("w")
        argv = [str(self.scripts / "herdr-mobile-relay-service.sh")]
        if self.launchd:
            source = (self.scripts / "install-service.sh").read_text()
            template = source.split('cat > "$STAGED_PLIST" <<EOF\n', 1)[1].split("\nEOF", 1)[0]
            definition = plistlib.loads(template.encode())
            assert definition["ProgramArguments"] == ["$SERVICE_WRAPPER_XML", "--launchd"]
            assert definition["KeepAlive"] == {"SuccessfulExit": False}
            argv += definition["ProgramArguments"][1:]
        self.proc = subprocess.Popen([BASH, *argv],
                                     env=self.env, stdin=subprocess.DEVNULL,
                                     stdout=self.stream, stderr=subprocess.STDOUT,
                                     start_new_session=True)

    def events(self, kind):
        path = self.root / (kind + ".events")
        return [json.loads(s) for s in path.read_text().split("\n")[:-1]] if path.exists() else []

    def shell(self):
        path = self.root / "shell.events"
        return path.read_text().splitlines() if path.exists() else []

    def ready(self, kinds=("relay", "cloudflared")):
        until(lambda: all(any(e["action"] == "start" for e in self.events(k)) for k in kinds),
              "children did not start", self.proc)

    def captured(self, count=2):
        until(lambda: sum(s.startswith("capture ") for s in self.shell()) >= count,
              "parent did not capture children", self.proc)

    def held(self, gate):
        until(lambda: (self.root / (gate + ".held")).exists(), "gate not reached: " + gate, self.proc)

    def release(self, gate):
        write(self.root / (gate + ".release"), "release\n")

    def exit_child(self, kind, code):
        pending = self.root / (kind + ".exit.pending")
        write(pending, str(code))
        pending.replace(self.root / (kind + ".exit"))

    def signal_child(self, kind, sig):
        pending = self.root / (kind + ".signal.pending")
        write(pending, str(int(sig)))
        pending.replace(self.root / (kind + ".signal"))

    def complete_signal(self, kind, sig, kinds=None):
        self.complete(128 + sig, kinds)
        assert "wait-end " + str(self.pid(kind)) + " " + str(128 + sig) in self.shell()
        assert any(e["action"] == "signal-exit" and e["signal"] == sig for e in self.events(kind))
        assert "exited unexpectedly with status " + str(128 + sig) in self.output.read_text()

    def pid(self, kind):
        starts = [e["pid"] for e in self.events(kind) if e["action"] == "start"]
        if starts:
            return starts[0]
        # TERM may retire a captured child before its adapter has initialized.
        count = "1" if kind == "relay" else "2"
        return int(next(s.split()[3] for s in self.shell() if s.startswith("capture " + count + " ")))

    def continued(self, kind):
        count = "1" if kind == "relay" else "2"
        token = next(s.split()[2] for s in self.shell() if s.startswith("capture " + count + " "))
        # TERM may run before Python's CONT handler; do not require handler
        # ordering. The actual owned signal plus terminal wait is the proof.
        return "signal -CONT %" + token.strip("[]+-") in self.shell()

    def complete(self, status, kinds=("relay", "cloudflared"), unknown=None):
        # All assertions precede fallback containment. wait-end plus PID absence
        # proves supervisor reaping, not just handler entry or harness cleanup.
        observed = self.proc.wait(timeout=DEADLINE)
        # Evidence only: the bare comparison gave a hosted log with no observed value. The
        # assertion itself is unchanged; its message now names the case, both statuses and
        # the bounded tails of the supervisor output and the parent-shell record (fixture data).
        assert observed == status, "supervisor exit status %r, expected %r (case %s); output tail %r; shell tail %r" % (
            observed, status, self.root.name,
            self.output.read_text()[-600:] if self.output.exists() else "", self.shell()[-12:])
        self.stream.flush()
        assert (self.root / "relay.env").read_bytes() == self.before
        assert (self.root / "tunnel.yml").read_text() == "inert tunnel config\n"
        assert not any(e["action"] == "forbidden" for p in (self.root / "bin").iterdir()
                       for e in self.events(p.name)), "effectful tool invoked"
        lines = self.shell()
        actual = {k for k in ("relay", "cloudflared") if any(e["action"] == "start" for e in self.events(k))}
        actual.update("relay" if s.split()[1] == "1" else "cloudflared"
                      for s in lines if s.startswith("capture "))
        if kinds is None:
            kinds = tuple(sorted(actual))
            assert "relay" in actual
        else:
            assert actual == set(kinds), "missing or unasserted essential sibling"
        assert not any(s.startswith("gate-timeout") for s in lines)
        assert all(s.split()[1] == "0" for s in lines if s.startswith("jobs ")), "subshell job observation"
        if sys.platform == "darwin" and BASH == "/bin/bash":
            assert any(s.startswith("bash 3.2.") for s in lines), "Darwin fixture did not use stock Bash 3.2"
        for kind in kinds:
            pid = self.pid(kind)
            waits = [s for s in lines if s.startswith("wait-end " + str(pid) + " ")]
            if kind == unknown:
                assert not waits and alive(pid), "unknown identity gained wait/signal authority"
            else:
                assert len(waits) == 1, "child not terminal-reaped exactly once"
                assert not alive(pid), "child survived supervisor cleanup"
        until(lambda: any(e["action"] == "start" for e in self.events("bystander")), "bystander did not start")
        bystander = self.pid("bystander")
        assert alive(bystander), "supervisor killed unrelated parent job"
        assert not any(e["action"] in ("term", "exit") for e in self.events("bystander"))
        # Every product signal must be to a captured essential jobspec, never PID.
        specs = {"%" + s.split()[2].strip("[]+-") for s in lines if s.startswith("capture ")}
        bystander_record = (self.root / "bystander.record").read_text().split()[0].strip("[]+-")
        if self.env.get("WITH_GENERATIONS") == "1":
            prior = (self.root / "prior.record").read_text().split()
            assert prior[0].strip("[]+-") == bystander_record, "fixture did not exercise spec reuse"
            assert int(prior[1]) != bystander, "fixture did not exercise a new PID generation"
        for s in lines:
            if s.startswith("signal "):
                target = s.split()[-1]
                assert target.startswith("%") and target in specs and target != "%" + bystander_record
        assert "cleanup could not confirm" not in self.output.read_text() or unknown is not None

    def contain(self):
        # Failure containment is intentionally separate and cannot satisfy any
        # complete() assertion. Only this owned fixture group is targeted.
        if self.proc is not None:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.proc.wait(timeout=5)
        if self.stream is not None:
            self.stream.close()


def run_case(parent, name, exercise, transport="cloudflare", **extra):
    fixture = Fixture(parent, name, transport, **extra)
    try:
        fixture.start()
        exercise(fixture)
    finally:
        fixture.contain()


def restart_source_contract():
    # Check the actual generated-definition SOURCE. No installer/controller
    # execution: launchd KeepAlive keys are OR conditions, not a readiness proof.
    unit = (REPO / "relay/install-systemd-user-service.sh").read_text()
    template = unit.split('cat > "$STAGED_UNIT" <<EOF\n', 1)[1].split("\nEOF", 1)[0]
    unit = dict(line.split("=", 1) for line in template.splitlines() if "=" in line and not line.startswith("#"))
    assert unit["Type"] == "simple" and unit["Restart"] == "on-failure" and unit["RestartSec"] == "10"
    assert unit["RestartPreventExitStatus"].split() == ["69", "78"]
    assert not {"SuccessExitStatus", "RemainAfterExit"} & unit.keys()
    assert unit["ExecStart"] == "$SERVICE_WRAPPER_ESCAPED"
    source = (REPO / "relay/install-service.sh").read_text()
    template = source.split('cat > "$STAGED_PLIST" <<EOF\n', 1)[1].split("\nEOF", 1)[0]
    plist = plistlib.loads(template.encode())
    assert plist["ProgramArguments"] == ["$SERVICE_WRAPPER_XML", "--launchd"]
    assert plist["RunAtLoad"] is True and plist["ThrottleInterval"] == 10
    assert plist["KeepAlive"] == dict(SuccessfulExit=False)


def main():
    restart_source_contract()
    with tempfile.TemporaryDirectory(prefix="herdr-service-supervision-") as tmp:
        parent = pathlib.Path(tmp)
        # Only the explicit producer argv adapts pre-child refusals. Child
        # 69/78, uncertainty and requested signals retain T001 failure/status.
        for transport in ("gateway", "cloudflare"):
            kinds = ("relay",) if transport == "gateway" else ("relay", "cloudflared")
            for kind in kinds:
                for code in (0, 23, 69, 78):
                    def launchd_exit(f, kind=kind, code=code, kinds=kinds):
                        f.ready(kinds)
                        f.captured(len(kinds))
                        f.exit_child(kind, code)
                        f.complete(code or 1, kinds)
                        assert "automatic retry suppressed" not in f.output.read_text()
                    run_case(parent, f"launchd-exit-{transport}-{kind}-{code}", launchd_exit, transport,
                             LAUNCHD_START=True, WITH_GENERATIONS="1")
            for sig in (signal.SIGINT, signal.SIGTERM):
                def launchd_signal(f, sig=sig, kinds=kinds):
                    f.ready(kinds)
                    f.captured(len(kinds))
                    f.proc.send_signal(sig)
                    f.complete(128 + sig, kinds)
                run_case(parent, f"launchd-request-{transport}-{sig}", launchd_signal, transport, LAUNCHD_START=True)
            def launchd_unknown(f, kinds=kinds):
                f.ready(kinds)
                f.captured(len(kinds))
                write(f.root / "arm-poll", "armed\n")
                f.held("poll")
                write(f.root / "unknown", "armed\n")
                f.release("poll")
                f.complete(1, kinds, unknown="relay")
                assert "automatic retry suppressed" not in f.output.read_text()
            run_case(parent, "launchd-unknown-" + transport, launchd_unknown, transport, LAUNCHD_START=True)
        for transport in ("gateway", "cloudflare"):
            kinds = ("relay",) if transport == "gateway" else ("relay", "cloudflared")
            for kind in kinds:
                for code in (0, 23, 69, 78):
                    def exits(f, kind=kind, code=code, kinds=kinds):
                        f.ready(kinds)
                        f.captured(len(kinds))
                        f.exit_child(kind, code)
                        f.complete(code or 1, kinds)
                    run_case(parent, f"exit-{transport}-{kind}-{code}", exits, transport, WITH_GENERATIONS="1")
                    # Genuine unsynchronized immediate exit as well as a
                    # deterministic terminal-before-capture observation gate.
                    def immediate(f, code=code):
                        # A relay can finish just after its capture, so a tunnel
                        # may or may not start. Assert EVERY actual sibling.
                        f.complete(code or 1, kinds=None)
                    run_case(parent, f"immediate-{transport}-{kind}-{code}", immediate, transport,
                             **{"MODE_" + kind.upper(): "immediate", "CHILD_CODE": str(code)})
                    def capture(f, kind=kind, code=code, kinds=kinds):
                        f.held("capture")
                        f.ready(kinds if kind == "cloudflared" else ("relay",))
                        f.exit_child(kind, code)
                        until(lambda: not alive(f.pid(kind)), "child did not finish before capture")
                        f.release("capture")
                        f.complete(code or 1, kinds if kind == "cloudflared" else ("relay",))
                    run_case(parent, f"capture-{transport}-{kind}-{code}", capture, transport,
                             CAPTURE_GATE="2" if kind == "cloudflared" else "1")
                # Non-core-dumping signals exercise genuine Bash C-locale
                # terminal labels: Broken pipe, Alarm clock, User defined...
                # Exact statuses vary for USR1 across Darwin/Linux; never infer
                # them from job text. Assert sibling reaping before containment.
                for child_signal in (signal.SIGPIPE, signal.SIGALRM, signal.SIGUSR1):
                    def signaled(f, kind=kind, sig=child_signal, kinds=kinds):
                        f.ready(kinds)
                        f.captured(len(kinds))
                        f.signal_child(kind, sig)
                        f.complete_signal(kind, sig, kinds)
                    run_case(parent, f"signal-{transport}-{kind}-{child_signal}", signaled, transport,
                             WITH_GENERATIONS="1")
                    def immediate_signal(f, kind=kind, sig=child_signal):
                        f.complete_signal(kind, sig)
                    run_case(parent, f"immediate-signal-{transport}-{kind}-{child_signal}",
                             immediate_signal, transport,
                             **{"MODE_" + kind.upper(): "immediate-signal", "CHILD_SIGNAL": str(int(child_signal))})
                    def captured_signal(f, kind=kind, sig=child_signal, kinds=kinds):
                        f.held("capture")
                        f.ready(kinds if kind == "cloudflared" else ("relay",))
                        f.signal_child(kind, sig)
                        until(lambda: not alive(f.pid(kind)), "signaled child did not finish before capture")
                        f.release("capture")
                        f.complete_signal(kind, sig, kinds if kind == "cloudflared" else ("relay",))
                    run_case(parent, f"capture-signal-{transport}-{kind}-{child_signal}",
                             captured_signal, transport, CAPTURE_GATE="2" if kind == "cloudflared" else "1")
            for sig in (signal.SIGINT, signal.SIGTERM):
                def requested(f, sig=sig, kinds=kinds):
                    f.ready(kinds)
                    f.captured(len(kinds))
                    f.proc.send_signal(sig)  # after BOTH Cloudflare starts/captures
                    f.complete(128 + sig, kinds)
                run_case(parent, f"requested-{transport}-{sig}", requested, transport)

        def peer_terminal_during_capture(f):
            f.held("capture")
            f.ready()
            f.exit_child("relay", 23)
            f.exit_child("cloudflared", 0)
            until(lambda: not alive(f.pid("relay")) and not alive(f.pid("cloudflared")),
                  "siblings did not finish during capture")
            f.release("capture")
            f.complete(23)
        run_case(parent, "both-terminal-during-tunnel-capture", peer_terminal_during_capture, CAPTURE_GATE="2")

        def peer_signal_during_capture(f):
            f.held("capture")
            f.ready()
            f.signal_child("relay", signal.SIGPIPE)
            f.exit_child("cloudflared", 0)
            until(lambda: not alive(f.pid("relay")) and not alive(f.pid("cloudflared")),
                  "siblings did not finish during signal capture")
            f.release("capture")
            f.complete_signal("relay", signal.SIGPIPE, ("relay", "cloudflared"))
            assert "wait-end " + str(f.pid("cloudflared")) + " 0" in f.shell()
        run_case(parent, "peer-signal-during-tunnel-capture", peer_signal_during_capture, CAPTURE_GATE="2")

        def peer_terminal_with_capture_refusal(f):
            f.held("capture")
            f.ready()
            f.exit_child("relay", 23)
            until(lambda: not alive(f.pid("relay")), "relay did not finish during uncertain capture")
            f.release("capture")
            f.complete(23, unknown="cloudflared")
        run_case(parent, "peer-terminal-with-capture-refusal", peer_terminal_with_capture_refusal,
                 CAPTURE_GATE="2", CAPTURE_UNKNOWN="2")

        for corrupt in ("MISMATCH", "UNKNOWN"):
            for kind, count in (("relay", 1), ("cloudflared", 2)):
                def capture_refusal(f, kind=kind, count=count):
                    f.complete(1, kinds=("relay",) if count == 1 else ("relay", "cloudflared"), unknown=kind)
                    assert not any(e["action"] == "term" for e in f.events(kind))
                    assert "owned-job capture failed" in f.output.read_text()
                run_case(parent, "capture-refusal-" + corrupt + "-" + kind, capture_refusal,
                         **{"CAPTURE_" + corrupt: str(count), "WITH_GENERATIONS": "1"})

        for corrupt in ("mismatch", "unknown"):
            def identity(f, corrupt=corrupt):
                f.ready()
                f.captured()
                write(f.root / "arm-poll", "armed\n")
                f.held("poll")
                write(f.root / corrupt, "armed\n")
                f.release("poll")
                f.complete(1, unknown="relay")
                # Despite relay uncertainty, the sibling was cleaned/reaped.
                assert any(e["action"] == "term" for e in f.events("cloudflared"))
                assert not any(e["action"] == "term" for e in f.events("relay"))
            run_case(parent, "identity-" + corrupt, identity, WITH_GENERATIONS="1")

        for primary in (23, 130, 143):
            def cleanup_uncertainty(f, primary=primary):
                f.ready()
                f.captured()
                write(f.root / "arm-poll", "armed\n")
                f.held("poll")
                write(f.root / "unknown", "armed\n")
                if primary == 23:
                    f.exit_child("relay", 23)
                else:
                    f.proc.send_signal(primary - 128)
                f.release("poll")
                f.complete(primary, unknown="cloudflared")
                assert "cloudflared cleanup could not confirm" in f.output.read_text()
            run_case(parent, "cleanup-uncertainty-" + str(primary), cleanup_uncertainty,
                     CORRUPT_CHILD="cloudflared", POLL_TERMINAL="1" if primary == 23 else "0")

        for transport in ("gateway", "cloudflare"):
            kinds = ("relay",) if transport == "gateway" else ("relay", "cloudflared")
            for kind in kinds:
                def stopped_poll(f, kind=kind, kinds=kinds):
                    f.ready(kinds)
                    f.captured(len(kinds))
                    write(f.root / "arm-poll", "armed\n")
                    f.held("poll")
                    os.kill(f.pid(kind), signal.SIGSTOP)
                    f.release("poll")
                    f.complete(1, kinds)
                    assert "stopped unexpectedly" in f.output.read_text()
                    assert f.continued(kind)
                run_case(parent, "stopped-before-poll-" + transport + "-" + kind, stopped_poll, transport)

        def stopped_shutdown(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            os.kill(f.pid("cloudflared"), signal.SIGSTOP)
            f.exit_child("relay", 23)
            f.release("poll")
            f.complete(23)
            assert f.continued("cloudflared")
        run_case(parent, "stopped-before-shutdown", stopped_shutdown, POLL_TERMINAL="1")

        def stopped_relay_shutdown(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            os.kill(f.pid("relay"), signal.SIGSTOP)
            f.proc.send_signal(signal.SIGTERM)
            f.release("poll")
            f.complete(143)
            assert f.continued("relay")
        run_case(parent, "relay-stopped-before-requested-shutdown", stopped_relay_shutdown)

        for kind in ("relay", "cloudflared"):
            for mode in ("ignore", "restop"):
                def resistant(f, kind=kind, mode=mode):
                    f.ready()
                    f.captured()
                    write(f.root / "arm-signal", "armed\n")
                    f.exit_child("cloudflared" if kind == "relay" else "relay", 23)
                    f.held("signal")
                    until(lambda: any(e["action"] == "term" for e in f.events(kind)), "TERM not received")
                    if mode == "ignore":
                        # Stop AFTER TERM delivery while cleanup is in progress.
                        os.kill(f.pid(kind), signal.SIGSTOP)
                    f.release("signal")
                    f.complete(23)
                    assert "wait-end " + str(f.pid(kind)) + " 137" in f.shell()
                    assert f.continued(kind)
                    if mode == "restop":
                        assert sum(e["action"] == "restop" for e in f.events(kind)) >= 2
                run_case(parent, "term-resistant-" + kind + "-" + mode, resistant,
                         **{"MODE_" + kind.upper(): mode})

        def finish_signal_race(f):
            f.ready()
            f.captured()
            write(f.root / "arm-signal", "armed\n")
            f.exit_child("relay", 23)
            f.held("signal")
            f.exit_child("cloudflared", 0)
            until(lambda: not alive(f.pid("cloudflared")), "child did not finish during signal race")
            f.release("signal")
            f.complete(23)
            assert not any(e["action"] == "term" for e in f.events("cloudflared"))
            assert "wait-end " + str(f.pid("cloudflared")) + " 0" in f.shell()
        run_case(parent, "completion-before-signal-failure", finish_signal_race, SIGNAL_FAIL_RACE="1")

        # Both children resist TERM: neither can prevent the other's bounded
        # cleanup, even if the second receives the original requested signal.
        for sig in (signal.SIGINT, signal.SIGTERM):
            def both_resist(f, sig=sig):
                f.ready()
                f.captured()
                f.proc.send_signal(sig)
                f.complete(128 + sig)
                for kind in ("relay", "cloudflared"):
                    assert "wait-end " + str(f.pid(kind)) + " 137" in f.shell()
            run_case(parent, "both-resist-" + str(sig), both_resist,
                     MODE_RELAY="ignore", MODE_CLOUDFLARED="restop")
    print("hosted service supervision fixtures passed")


if __name__ == "__main__":
    main()

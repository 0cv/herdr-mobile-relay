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
import re
import shutil
import signal
import subprocess
import tempfile
import time

REPO = pathlib.Path(__file__).resolve().parents[1]
BASH = os.environ.get("SERVICE_TEST_BASH", "/bin/bash")
assert pathlib.Path(BASH).is_absolute(), "fixture Bash must be explicitly absolute"
DEADLINE = 20
TAIL_LIMIT = 64 * 1024


def tail_bytes(path, limit=TAIL_LIMIT):
    # Evidence only: read at most the last `limit` bytes of a fixture log, never the
    # whole file. A runaway supervisor may still be growing it while a failure message
    # is built, before fixture containment runs. A missing file reads as empty.
    try:
        with open(path, "rb") as handle:
            handle.seek(0, os.SEEK_END)
            handle.seek(max(0, handle.tell() - limit))
            return handle.read(limit)
    except OSError:
        return b""


def tail_text(path, chars):
    return tail_bytes(path).decode("utf-8", errors="replace")[-chars:]


def tail_lines(path, count):
    return tail_bytes(path).decode("utf-8", errors="replace").splitlines()[-count:]


def process_state(pid):
    # Evidence only, for a fixture PID: the kernel's one-letter state (T means stopped).
    try:
        with open("/proc/%d/stat" % pid, "rb") as handle:
            return handle.read(512).decode("ascii", errors="replace").rsplit(")", 1)[1].split()[0]
    except (OSError, IndexError):
        pass
    try:
        result = subprocess.run(["/bin/ps", "-o", "state=", "-p", str(pid)], stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.SubprocessError):
        return "unknown"
    return result.stdout.strip()[:8] or "gone"


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
fixture_transient_used=0
fixture_transient_probe_used=0
fixture_reuse_used=0
fixture_trace() { printf '%s\n' "$*" >> "$FIXTURE_ROOT/shell.events"; }
# mktemp is a forked command, so Bash may report and delete a signal-terminated
# child's job entry after it. Record each use (template name only) in order with
# the job observations, then run the real tool unchanged.
mktemp() {
    local last=""
    for last; do :; done
    fixture_trace "mktemp ${last##*/}"
    command mktemp "$@"
}
fixture_gate() {
    local name="$1" attempt deadline post_signal_fork
    fixture_trace "gate $name"
    : > "$FIXTURE_ROOT/$name.held"
    if [ "$name" = capture ] || [ "$name" = transient ] || [ "$name" = reuse ]; then
        # Capture holds the parent before listing; transient holds it before the
        # first post-miss jobs -p probe; reuse holds it after the relay's first
        # post-capture observation. None may fork while the child exits.
        # The capture gate holds the parent BEFORE its first job listing so a child
        # can terminate first. A forked /bin/sleep here would make Bash report and
        # delete a signal-terminated child's job entry while the gate waits, so the
        # listing could never show the terminal record this gate exists to present
        # (the product prepares its listing file BEFORE launching a child, so it forks
        # nothing between a launch and that listing; listing_source_contract() pins
        # that order in the source). This loop forks nothing (builtins only, bounded
        # by SECONDS), so the gate leaves the job table as an immediate capture would
        # have found it.
        deadline=$((SECONDS + 10))
        while [ ! -f "$FIXTURE_ROOT/$name.release" ]; do
            [ "$SECONDS" -lt "$deadline" ] || { fixture_trace "gate-timeout $name"; return 1; }
        done
        return 0
    fi
    for ((attempt=0; attempt<1000; attempt++)); do
        [ ! -f "$FIXTURE_ROOT/$name.release" ] || return 0
        post_signal_fork=0
        if [ "$name" = poll ] && [ -f "$FIXTURE_ROOT/prune-after-signal" ]; then
            post_signal_fork=1
        fi
        /bin/sleep 0.01
        [ "$post_signal_fork" = 0 ] || fixture_trace "post-signal-fork"
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
    local rc=0 line token pid state rest attempt terminal_gate=0 corrupt_pid expected_pid capture_limit=2
    [ "${TRANSPORT:-}" != gateway ] || capture_limit=1
    fixture_job_count=$((fixture_job_count+1))
    fixture_trace "jobs $BASH_SUBSHELL $*"
    # The initial capture calls jobs -l with ONE argument. Under the service's
    # set -u, an unguarded $2 exits Bash before builtin jobs can run; its
    # redirected stderr hides the nounset diagnostic from fixture output.
    if [ "$*" = '-l' ]; then
        fixture_capture_count=$((fixture_capture_count+1))
        expected_pid="${RELAY_PID:-}"
        [ "$fixture_capture_count" != 2 ] || expected_pid="${TUNNEL_PID:-}"
        if [ "${CAPTURE_GATE:-0}" = "$fixture_capture_count" ]; then
            fixture_gate capture || return 1
        fi
    elif [ "${1:-}" = -l ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ "$fixture_gate_used" = 0 ] && [ -f "$FIXTURE_ROOT/arm-poll" ]; then
        fixture_gate_used=1
        fixture_gate poll || return 1
        [ "${POLL_TERMINAL:-0}" != 1 ] || terminal_gate=1
    fi
    # Hide one post-capture record without reading/pruning the real job table.
    # The next product observation must still validate the actual jobspec/PID;
    # this seam never supplies a forged terminal record or an exit status.
    if [ "${1:-}" = -l ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ "$fixture_transient_used" = 0 ] && [ -f "$FIXTURE_ROOT/transient-record" ]; then
        fixture_transient_used=1
        fixture_trace "transient-record-unavailable ${RELAY_PID:-}"
        return 1
    fi
    # The first hidden long record has already returned. Hold the first
    # captured-spec probe using builtins only: the child must receive genuine
    # SIGPIPE BEFORE this observation and before the next long-record retry.
    if [ "${1:-}" = -p ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ "$fixture_transient_used" = 1 ] && [ "$fixture_transient_probe_used" = 0 ] &&
        [ -f "$FIXTURE_ROOT/transient-record" ]; then
        fixture_transient_probe_used=1
        fixture_gate transient || return 1
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
            if [ "$fixture_capture_count" -gt "$capture_limit" ]; then
                # This is the absence proof, not an essential capture (gateway
                # still needs its first listing traced as capture 1).
                # Only the collision case projects a conflicting inventory:
                # no extra job is launched and the real builtin is still read.
                if [ -f "$FIXTURE_ROOT/collision" ]; then
                    if [ "$pid" = "${RELAY_PID:-}" ]; then
                        fixture_trace "inventory-hidden $token $pid"
                        continue
                    fi
                    if [ "$pid" = "$fixture_bystander_pid" ]; then
                        fixture_trace "inventory $token ${RELAY_PID:-} Running"
                        printf '%s %s Running conflicting-generation\n' "$token" "$RELAY_PID"
                        continue
                    fi
                fi
                fixture_trace "inventory $token $pid $state"
            else
                fixture_trace "record $token $pid $state"
                if [ "$pid" = "$expected_pid" ]; then
                    fixture_trace "capture $fixture_capture_count $token $pid $state"
                    if [ "${CAPTURE_MISMATCH:-0}" = "$fixture_capture_count" ]; then
                        printf '%s %s Running unrelated-generation\n' "$token" "$fixture_bystander_pid"
                        continue
                    fi
                    [ "${CAPTURE_UNKNOWN:-0}" != "$fixture_capture_count" ] || continue
                fi
            fi
            printf '%s\n' "$line"
        done < "$FIXTURE_ROOT/job.record"
        return "$rc"
    fi
    fixture_trace "record $token $pid $state"
    if [ "${1:-}" = -p ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ -f "$FIXTURE_ROOT/empty-live-pid" ]; then
        # Force a 0/empty result with a REAL live captured job still listed in
        # the complete inventory; an ambiguous observation must not wait.
        fixture_trace "empty-live-pid $rc $line"
        return 0
    fi
    if [ "${1:-}" = -p ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ -f "$FIXTURE_ROOT/empty-pruned-pid" ] && [ -z "$line" ]; then
        # On either stock Bash, emulate Darwin's observed successful EMPTY pid
        # probe only after the genuine builtin returns no PID. Never hide a PID.
        fixture_trace "empty-pruned-pid $rc"
        return 0
    fi
    if [ "${1:-}" = -p ] && [ "${2:-}" = "${RELAY_JOB:-}" ] &&
        [ "$fixture_transient_used" = 1 ] && [ -f "$FIXTURE_ROOT/transient-record" ]; then
        fixture_trace "transient-spec-check $rc $line"
    fi
    # REUSE_PARK=1: the first observation of the relay's jobspec after its capture, before any
    # tunnel exists, has just read its real Running record. Park (builtins only) while the relay
    # dies of a genuine SIGPIPE and is reaped, then fork foreground commands that START after its
    # death. Hosted runs showed a fork DURING the death prunes nothing (fe36b89d) while forks after
    # it do (the post-signal-fork cases): Bash reports and deletes the entry, and the tunnel
    # launched next is numbered into the relay's jobspec. Nothing here is forged.
    if [ "${REUSE_PARK:-0}" = 1 ] && [ "${1:-}" = -l ] && [ -n "${RELAY_JOB:-}" ] &&
        [ "${2:-}" = "$RELAY_JOB" ] && [ -z "${TUNNEL_PID:-}" ] && [ "$fixture_reuse_used" = 0 ]; then
        fixture_reuse_used=1
        fixture_gate reuse || return 1
        /bin/sleep 0.01
        /bin/sleep 0.01
        fixture_trace "reuse-prune-forks"
    fi
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
        try:
            until(lambda: all(any(e["action"] == "start" for e in self.events(k)) for k in kinds),
                  "children did not start", self.proc)
        except AssertionError as error:
            # Failure evidence only; preserve the readiness/exit and five-second
            # assertions. The hosted log must identify an early supervisor exit.
            raise AssertionError("%s (case %s); output tail %r; shell tail %r" % (
                error, self.root.name, tail_text(self.output, 600),
                tail_lines(self.root / "shell.events", 30))) from None

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

    def child_states(self, kinds):
        # Evidence only: the kernel state of each fixture child. Bash lists a child as Running
        # in the record; this tells a child that is really stopped (T) from one that is running.
        states = {}
        for kind in kinds or ("relay", "cloudflared"):
            try:
                states[kind] = process_state(self.pid(kind))
            except Exception:
                states[kind] = "unknown"
        return states

    def shutdown_evidence(self):
        # Evidence only, from the bounded tail of the parent-shell record. A supervisor that never
        # ran its request trap (a lost SIGINT) shows no signal to any child and no child "term";
        # one inside a slow shutdown shows TERM/CONT rounds and whether KILL was already sent.
        lines = tail_lines(self.root / "shell.events", 4000)
        sent = [s.split()[1] for s in lines if s.startswith("signal ")]
        return dict(supervisor=process_state(self.proc.pid), observations=sum(s.startswith("jobs ") for s in lines),
                    signals={name: sent.count(name) for name in sorted(set(sent))},
                    waits=sum(s.startswith("wait-end ") for s in lines),
                    term_events={k: sum(e["action"] == "term" for e in self.events(k)) for k in ("relay", "cloudflared")})

    def complete(self, status, kinds=("relay", "cloudflared"), unknown=None):
        # All assertions precede fallback containment. wait-end plus PID absence
        # proves supervisor reaping, not just handler entry or harness cleanup.
        try:
            observed = self.proc.wait(timeout=DEADLINE)
        except subprocess.TimeoutExpired:
            # Evidence only: a supervisor that outlives the deadline gave a bare TimeoutExpired
            # with no case name and no tail (hosted run 38825734282). It still fails the case;
            # the message adds the same bounded fixture data as the comparison below, with a
            # longer parent-shell tail to show where it is blocked (an open wait-begin).
            raise AssertionError("supervisor still running after %ds (case %s); output tail %r; shell tail %r; child states %r; shutdown evidence %r" % (
                DEADLINE, self.root.name,
                tail_text(self.output, 600), tail_lines(self.root / "shell.events", 30),
                self.child_states(kinds), self.shutdown_evidence())) from None
        # Evidence only: the bare comparison gave a hosted log with no observed value. The
        # assertion itself is unchanged; its message now names the case, both statuses and
        # the bounded tails of the supervisor output and the parent-shell record (fixture data).
        assert observed == status, "supervisor exit status %r, expected %r (case %s); output tail %r; shell tail %r" % (
            observed, status, self.root.name,
            tail_text(self.output, 600), tail_lines(self.root / "shell.events", 36))
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
        # The first listing of a launched child must follow its launch with no forked
        # command: the traced line before every capture listing (the exact call
        # `jobs -l`) is the prepared-file mktemp that preceded the launch, never a
        # mktemp of capture's own, which could let Bash prune a signal-killed child.
        capture_listings = 0
        for index, line in enumerate(lines):
            if line == "jobs 0 -l" and capture_listings < sum(s.startswith("capture ") for s in lines):
                capture_listings += 1
                assert index > 0 and lines[index - 1] == "mktemp herdr-supervised-prepared.XXXXXX", (
                    "a forked command ran between launching a child and its first job listing: %r"
                    % (lines[max(0, index - 2):index + 1],))
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


def listing_source_contract():
    # Source contract, no execution (like restart_source_contract). Bash reports and
    # deletes the job entry of a signal-terminated child after the next FORKED command,
    # so (1) the listing file must be prepared immediately BEFORE each essential child
    # is launched and (2) only the PID assignment and a status reset may run between the
    # launch and the capture, which itself may fork nothing before its first `jobs -l`.
    # The runtime trace assertion in Fixture.complete() cannot see the launch, so a
    # prepare moved after it, or a command inserted after it, is caught here.
    jobs_hook = BUILTINS.split("\njobs() {\n", 1)[1].split("\nkill() {\n", 1)[0]
    assert '"$2"' not in jobs_hook, "unary jobs -l must not expand unset $2 under service nounset"
    service = (REPO / "relay/herdr-mobile-relay-service.sh").read_text().splitlines()
    code = [line.strip() for line in service if line.strip() and not line.strip().startswith("#")]
    for fragment, pid_var in (('"$RELAY_BIN" serve-installed', "RELAY_PID"),
                              ('exec "$CLOUDFLARED_BIN" tunnel', "TUNNEL_PID")):
        launches = [i for i, line in enumerate(code) if fragment in line and line.endswith("&")]
        assert len(launches) == 1, "expected exactly one launch of " + pid_var
        launch = launches[0]
        assert launch > 0 and code[launch - 1].startswith("supervised_child_prepare || "), (
            "listing file is not prepared immediately before the launch of " + pid_var)
        captures = [i for i, line in enumerate(code)
                    if i > launch and line.startswith('supervised_child_capture "$' + pid_var + '"')]
        assert captures, "no capture follows the launch of " + pid_var
        assert code[launch + 1:captures[0]] == [pid_var + "=$!", "capture_status=0"], (
            "a command runs between the launch of " + pid_var + " and its first job listing")
    common = (REPO / "relay/common.sh").read_text().splitlines()

    def body(header, stop):
        first = next(i for i, line in enumerate(common) if line.startswith(header))
        last = next(i for i in range(first, len(common)) if stop in common[i])
        return [line.strip() for line in common[first + 1:last]
                if line.strip() and not line.strip().startswith("#")]
    external = re.compile(r"(?<![\w-])(mktemp|rm|sleep|cat|sed|awk|grep|tr|date|stat)(?![\w-])")
    forking = lambda line: "$(" in line or "`" in line or external.search(line) is not None
    before_listing = body("supervised_child_capture() {", "_supervised_child_capture_records ||")
    assert before_listing and not any(forking(line) for line in before_listing), (
        "supervised_child_capture runs a forking command before its listing")
    listing = body("_supervised_child_capture_records() {", "jobs -l >")
    assert 'if [ -n "$SUPERVISED_JOB_FILE" ]; then' in listing
    for index, line in enumerate(listing):
        if "mktemp" in line:
            # Only the fallback for callers that never prepared, in the else branch.
            assert listing[index - 1] == "else" and "herdr-supervised-jobs" in line, (
                "the capture's own mktemp is not confined to the unprepared fallback")
        else:
            assert not forking(line), "the capture runs a forking command before its first jobs -l: " + line
    # Positive pin, deliberately replacing the open-ended denylist above (which stays as an
    # extra): any command that is not a builtin (touch, mkdir, ...) forks, and a fork before
    # the first `jobs -l` lets Bash report and delete a signal-terminated child's job entry.
    # So every code line before the listing is pinned exactly. Comments and blank lines stay
    # free to change; any added, removed or reordered code line fails the suite. Brittle by
    # design: a later edit of these preambles must update the pin with it.
    pinned_capture = [
        'local expected_pid="$1" token listed_pid state number record_status=0',
        'local line candidate="" matches=0 index known_status',
        'SUPERVISED_JOB_SPEC=""',
        'SUPERVISED_JOB_STATE=""',
        'SUPERVISED_JOB_STATUS=""',
        """case "$expected_pid" in ''|*[!0-9]*) return 1 ;; esac""",
    ]
    pinned_listing = [
        'local output_file line',
        'SUPERVISED_JOB_RECORDS=""',
        'if [ -n "$SUPERVISED_JOB_FILE" ]; then',
        'output_file="$SUPERVISED_JOB_FILE"',
        'SUPERVISED_JOB_FILE=""',
        'else',
        'output_file="$(mktemp "${TMPDIR:-/tmp}/herdr-supervised-jobs.XXXXXX")" || return 1',
        'fi',
    ]
    assert before_listing == pinned_capture, (
        "capture preamble changed; every line before the first jobs -l must be a pinned builtin: %r, expected %r"
        % (before_listing, pinned_capture))
    assert listing == pinned_listing, (
        "listing preamble changed; every line before the first jobs -l must be a pinned builtin: %r, expected %r"
        % (listing, pinned_listing))

    def stop_line(header, stop):
        first = next(i for i, line in enumerate(common) if line.startswith(header))
        last = next(i for i in range(first, len(common)) if stop in common[i])
        return common[last].strip()

    def function_code(header):
        first = next(i for i, line in enumerate(common) if line.startswith(header))
        last = next(i for i in range(first + 1, len(common)) if common[i] == "}")
        return [line.strip() for line in common[first + 1:last]
                if line.strip() and not line.strip().startswith("#")]
    # Seam: nothing may sit between the pinned preamble and the listing call, or between the
    # listing function's preamble and the one line that carries the listing.
    assert stop_line("supervised_child_capture() {", "_supervised_child_capture_records ||") == (
        "_supervised_child_capture_records || return 1"), "the line after the capture preamble is not the listing call"
    assert stop_line("_supervised_child_capture_records() {", "jobs -l >") == (
        'if ! LC_ALL=C jobs -l > "$output_file" 2>/dev/null; then'), "the line carrying the first listing changed"
    assert sum("jobs -l >" in line for line in function_code("_supervised_child_capture_records() {")) == 1, (
        "the listing helper must contain exactly one jobs -l > line")
    # The jobspec-reuse helpers only classify and prove: no signal, no wait (the one wait
    # sits in _supervised_child_reap_absent, after the proof, by original PID).
    for helper in ("_supervised_child_other_generation() {", "_supervised_child_spec_reused() {"):
        assert not re.search(r"(?<![\w-])(kill|wait)(?![\w-])", "\n".join(function_code(helper))), (
            helper + " must neither signal nor wait")
    # Uniqueness guard. Bash runs the LAST definition of a function, but the checks above read
    # the FIRST header they find. A second definition anywhere in relay/*.sh (written `name() {`,
    # `name(){`, `name ()` with the brace on the next line, or `function name`) could shadow the
    # pinned copy and add a fork before the first `jobs -l` while every pin still passes. So each
    # helper must have exactly one definition across relay/*.sh. Comment lines are skipped; a
    # call never has `()` after the name, so `name()` and `function name` are definitions.
    # Brittle by design: an intentional move or redefinition of either helper must update this.
    for helper in ("supervised_child_capture", "_supervised_child_capture_records"):
        definition = re.compile(
            r"(?<![\w-])(?:function[ \t]+" + re.escape(helper) + r"(?![\w-])"
            r"|" + re.escape(helper) + r"[ \t]*\([ \t]*\))")
        definitions = []
        for path in sorted((REPO / "relay").glob("*.sh")):
            text = path.read_text(encoding="utf-8", errors="replace")
            for number, line in enumerate(text.splitlines(), 1):
                if not line.strip().startswith("#") and definition.search(line):
                    definitions.append("relay/%s:%d" % (path.name, number))
        assert len(definitions) == 1, (
            "%s must have exactly one definition across relay/*.sh, found %d: %s"
            % (helper, len(definitions), ", ".join(definitions) or "none"))


def shutdown_bound_source_contract():
    # Source contract, no execution. On a slow host every graceful round costs forks (jobs file,
    # ps), and two sequential fifty-round phases outlasted the 20s bound on hosted macOS. The
    # graceful phase must therefore also end by elapsed time; the round cap stays as the bound
    # for a clock that moves. Brittle by design: change this pin together with that loop.
    service = (REPO / "relay/herdr-mobile-relay-service.sh").read_text()
    body = service.split("\nstop_child() {\n", 1)[1].split("\n}\n", 1)[0]
    code = [line.strip() for line in body.splitlines() if line.strip() and not line.strip().startswith("#")]
    assert "deadline=$((SECONDS + 5))" in code, "the graceful phase has no five-second deadline"
    assert "for ((attempt = 0; attempt < 50 && SECONDS < deadline; attempt++)); do" in code, (
        "the graceful phase is not bounded by both fifty rounds and its elapsed-time deadline")


def main():
    restart_source_contract()
    listing_source_contract()
    shutdown_bound_source_contract()
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

        def transient_signal_record(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            write(f.root / "transient-record", "armed\n")
            f.release("poll")
            f.held("transient")
            lines = f.shell()
            pid = f.pid("relay")
            spec = "%" + next(s.split()[2] for s in lines if s.startswith("capture 1 ")).strip("[]+-")
            assert sum(s == "transient-record-unavailable " + str(pid) for s in lines) == 1
            assert "jobs 0 -p " + spec in lines, "first unreadable record did not probe captured spec"
            assert not any(s.startswith("transient-spec-check ") for s in lines), (
                "spec was observed before the signal gate")
            f.signal_child("relay", signal.SIGPIPE)
            until(lambda: not alive(pid), "SIGPIPE child did not finish before spec probe")
            f.release("transient")
            f.complete_signal("relay", signal.SIGPIPE)
            lines = f.shell()
            probes = [i for i, s in enumerate(lines) if s.startswith("transient-spec-check ")]
            assert probes and lines[probes[0]] == "transient-spec-check 0 " + str(pid), (
                "first probe proved absence rather than exercising the retry: %r" %
                [lines[i] for i in probes])
            wait_at = next(i for i, s in enumerate(lines) if s == "wait-begin " + str(pid))
            after_probe = lines[probes[0] + 1:wait_at]
            assert "jobs 0 -l " + spec in after_probe, (
                "captured spec was not re-observed after the terminated-child probe")
            terminal_record = any(len(parts := s.split()) >= 4 and parts[0] == "record" and
                                  parts[1].strip("[]+-") == spec[1:] and parts[2] == str(pid) and
                                  parts[3] not in ("Running", "Stopped") for s in after_probe)
            proven_absence = any(s.startswith("transient-spec-check ") and
                                 s.split()[1] != "0" for s in after_probe)
            assert terminal_record or proven_absence, (
                "retry neither validated a terminal record nor proved spec absence")
            assert sum(s == "wait-end " + str(pid) + " 141" for s in lines) == 1
        run_case(parent, "transient-record-signal-relay", transient_signal_record)

        def pruned_signal_record(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            f.signal_child("relay", signal.SIGPIPE)
            until(lambda: not alive(f.pid("relay")), "SIGPIPE child did not finish before poll")
            write(f.root / "prune-after-signal", "armed\n")
            until(lambda: "post-signal-fork" in f.shell(),
                  "poll did not fork after SIGPIPE", f.proc)
            # Bash may prune the terminal record on the gate's real fork.
            f.release("poll")
            f.complete_signal("relay", signal.SIGPIPE)
            spec = "%" + next(s.split()[2] for s in f.shell() if s.startswith("capture 1 ")).strip("[]+-")
            assert "jobs 0 -p " + spec in f.shell(), "pruned status did not use captured-spec absence"
        run_case(parent, "pruned-signal-relay", pruned_signal_record)

        def empty_pruned_pid(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            f.signal_child("relay", signal.SIGPIPE)
            until(lambda: not alive(f.pid("relay")), "SIGPIPE child did not finish before poll")
            write(f.root / "prune-after-signal", "armed\n")
            until(lambda: "post-signal-fork" in f.shell(),
                  "poll did not fork after SIGPIPE", f.proc)
            write(f.root / "empty-pruned-pid", "armed\n")
            f.release("poll")
            f.complete_signal("relay", signal.SIGPIPE)
            lines = f.shell()
            pid = f.pid("relay")
            spec = "%" + next(s.split()[2] for s in lines if s.startswith("capture 1 ")).strip("[]+-")
            assert "jobs 0 -l " + spec in lines
            assert "jobs 0 -p " + spec in lines
            assert any(s.startswith("empty-pruned-pid ") for s in lines), (
                "the real pid probe did not return empty after capture/prune")
            inventories = [i for i, s in enumerate(lines) if s == "jobs 0 -l"]
            assert len(inventories) >= 3, "no complete owning-shell inventory after the empty pid listing"
            wait_at = lines.index("wait-begin " + str(pid))
            assert inventories[2] < wait_at
            last_inventory = max(i for i in inventories if i < wait_at)
            assert not any(s.startswith("inventory [" + spec[1:] + "]")
                           for s in lines[last_inventory + 1:wait_at]), (
                "captured jobspec was still in the final job-table inventory")
        run_case(parent, "post-capture-pruned-empty-pid", empty_pruned_pid)

        def empty_live_pid(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            write(f.root / "unknown", "armed\n")
            write(f.root / "empty-live-pid", "armed\n")
            f.release("poll")
            f.complete(1, unknown="relay")
            lines = f.shell()
            pid = f.pid("relay")
            spec = "%" + next(s.split()[2] for s in lines if s.startswith("capture 1 ")).strip("[]+-")
            assert any(s.startswith("empty-live-pid 0 " + str(pid)) for s in lines)
            assert any(s.startswith("inventory [" + spec[1:] + "]") and
                       s.split()[2] == str(pid) for s in lines), (
                "the live captured generation was not visible in the full inventory")
            assert not any(s.startswith("wait-begin " + str(pid)) for s in lines)
            assert not any(s == "signal -TERM " + spec or s == "signal -KILL " + spec
                           for s in lines)
        run_case(parent, "post-capture-live-empty-pid-refusal", empty_live_pid)

        def different_spec_same_pid(f):
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            write(f.root / "unknown", "armed\n")
            write(f.root / "empty-live-pid", "armed\n")
            write(f.root / "collision", "armed\n")
            f.release("poll")
            # The parent must refuse the ambiguous PID, not block waiting for
            # the still-live relay. Fallback containment cannot satisfy this.
            f.complete(1, unknown="relay")
            lines = f.shell()
            pid = f.pid("relay")
            spec = "%" + next(s.split()[2] for s in lines if s.startswith("capture 1 ")).strip("[]+-")
            bystander_number = (f.root / "bystander.record").read_text().split()[0].strip("[]+-")
            assert any(s == "empty-live-pid 0 " + str(pid) for s in lines)
            assert any(s.startswith("inventory-hidden ") and
                       s.split()[1].strip("[]+-") == spec[1:] and s.split()[2] == str(pid)
                       for s in lines), "test did not hide the captured jobspec"
            assert any(s.startswith("inventory ") and s.split()[1].strip("[]+-") == bystander_number
                       and s.split()[2] == str(pid) and s.split()[3] == "Running"
                       for s in lines), "test did not map the PID under the other jobspec"
            assert not any(s.startswith("wait-begin " + str(pid)) for s in lines)
            assert not any(s == "signal -TERM " + spec or s == "signal -KILL " + spec
                           for s in lines)
        run_case(parent, "post-capture-other-spec-same-pid-refusal", different_spec_same_pid)

        def reused_jobspec(f):
            # Natural jobspec reuse, not simulated: REUSE_PARK parks the parent (builtins only) just
            # after the relay's first post-capture observation and before the tunnel exists. The
            # relay dies of a real SIGPIPE there and is reaped; the hook's forks then start AFTER
            # its death (a fork DURING it pruned nothing on hosted macOS and Ubuntu, fe36b89d), so
            # Bash deletes the entry and the tunnel is launched into the relay's jobspec. The
            # supervisor must report the relay's genuine wait status by its original PID.
            f.ready(("relay",))
            f.held("reuse")
            relay = f.pid("relay")
            f.signal_child("relay", signal.SIGPIPE)
            until(lambda: not alive(relay), "relay did not die while the parent was parked", f.proc)
            f.release("reuse")
            f.complete_signal("relay", signal.SIGPIPE)
            lines = f.shell()
            captures = [s.split() for s in lines if s.startswith("capture ")]
            first = next((i for i, s in enumerate(lines) if s.startswith("capture 1 ")), 0)
            assert len(captures) == 2 and captures[1][3] != str(relay) and (
                captures[0][2].strip("[]+-") == captures[1][2].strip("[]+-")), (
                "fixture did not exercise jobspec reuse: captures %r; shell events %r" % (captures, lines[first:first + 30]))
            tunnel = captures[1][3]
            wait_at = lines.index("wait-begin " + str(relay))
            observed = lines[lines.index(" ".join(captures[1])) + 1:wait_at]
            assert any(s.startswith("record ") and s.split()[2] == tunnel and
                       s.split()[1].strip("[]+-") == captures[0][2].strip("[]+-") for s in observed), (
                "the relay's jobspec did not list the tunnel generation; shell events %r" % (lines[first:wait_at + 1],))
            assert "mktemp herdr-supervised-reuse.XXXXXX" in observed, "reuse was not proven from the inventory"
            assert any(s.startswith("inventory ") and s.split()[2] == tunnel for s in observed)
            assert not any(s.startswith("inventory ") and s.split()[2] == str(relay) for s in observed)
            assert not any(s.startswith("signal ") for s in lines[:wait_at]), "signal before the relay's status was read"
            assert "identity is unavailable" not in f.output.read_text()
        run_case(parent, "reused-jobspec-after-immediate-signal-relay", reused_jobspec, REUSE_PARK="1")

        def reused_spec_other_spec_same_pid(f):
            # Another generation under the relay's jobspec proves nothing while the captured PID
            # is still listed under a different jobspec: refused, no wait and no signal.
            f.ready()
            f.captured()
            write(f.root / "arm-poll", "armed\n")
            f.held("poll")
            write(f.root / "mismatch", "armed\n")
            write(f.root / "collision", "armed\n")
            f.release("poll")
            f.complete(1, unknown="relay")
            lines = f.shell()
            pid = f.pid("relay")
            spec = "%" + next(s.split()[2] for s in lines if s.startswith("capture 1 ")).strip("[]+-")
            bystander_number = (f.root / "bystander.record").read_text().split()[0].strip("[]+-")
            assert "mktemp herdr-supervised-reuse.XXXXXX" in lines, "mismatch did not reach the reuse proof"
            assert any(s.startswith("inventory ") and s.split()[1].strip("[]+-") == bystander_number
                       and s.split()[2] == str(pid) for s in lines), "test did not map the PID under the other jobspec"
            assert not any(s.startswith("wait-begin " + str(pid)) for s in lines)
            assert not any(s == "signal -TERM " + spec or s == "signal -KILL " + spec for s in lines)
            assert not any(e["action"] == "term" for e in f.events("relay"))
        run_case(parent, "post-capture-reused-spec-other-spec-same-pid-refusal", reused_spec_other_spec_same_pid)

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
                if corrupt == "mismatch":
                    # The reuse proof was tried and refused: the live relay is still listed.
                    assert "mktemp herdr-supervised-reuse.XXXXXX" in f.shell()
                    assert any(s.startswith("inventory ") and s.split()[2] == str(f.pid("relay"))
                               for s in f.shell())
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
                sent = time.monotonic()
                f.proc.send_signal(sig)
                f.complete(128 + sig)
                # Evidence only: the hosted log records the margin under the completion bound.
                print("both-resist-%d: supervisor exited %.1fs after the signal (bound %ds)" % (
                    sig, time.monotonic() - sent, DEADLINE), flush=True)
                for kind in ("relay", "cloudflared"):
                    assert "wait-end " + str(f.pid(kind)) + " 137" in f.shell()
            run_case(parent, "both-resist-" + str(sig), both_resist,
                     MODE_RELAY="ignore", MODE_CLOUDFLARED="restop")
    print("hosted service supervision fixtures passed")


if __name__ == "__main__":
    main()

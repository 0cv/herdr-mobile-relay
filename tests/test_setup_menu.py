#!/usr/bin/env python3
"""Hosted disposable menu/wrapper fixtures; inert actions prove wiring only.
NATIVE_HELPER cases exercise actual partial status, not package/phone health.
"""
import os
import sys

# Refuse before imports/temp/subprocess activity. No required-case local skip.
if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
    print("setup menu fixtures require disposable GitHub-hosted CI", file=sys.stderr)
    raise SystemExit(2)

import json
from pathlib import Path
import pty
import re
import select
import shutil
import signal
import stat
import subprocess
import tempfile
import time

REPO = Path(__file__).resolve().parent.parent
HELPER = os.environ.get("NATIVE_HELPER", "")
if not HELPER or not Path(HELPER).is_file() or not os.access(HELPER, os.X_OK):
    raise SystemExit("setup menu fixtures require the native fixture's compiled helper")

ACTIONS = ["plugin-choose-transport.sh", "plugin-install-service.sh", "plugin-change-hostname.sh",
           "plugin-stable-teardown.sh", "plugin-setup-link.sh", "plugin-configure-app-deploy.sh"]
CANARIES = ["secret-menu-canary", "private-menu-node", "private-menu-path", "$(", "\x1b"]


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def executable(path, body):
    path.write_text(body)
    path.chmod(0o700)


ACTOR = '''import json, os, pathlib, signal, sys, time
entry = pathlib.Path(sys.argv[0]).name
record = dict(kind="action", entry=entry, argv=sys.argv[1:], pid=os.getpid(),
              menu=os.environ.get("HERDR_SETUP_MENU"), cwd=os.getcwd(),
              consent={k: os.environ.get(k) for k in ("HERDR_SETUP_YES", "HERDR_TAILSCALE_YES", "HERDR_TAILSCALE_CONFIRM_SERVE")},
              routing=os.environ.get("HERDR_GATEWAY_URL"), interrupt_ignored=signal.getsignal(signal.SIGINT) == signal.SIG_IGN)
with open(os.environ["MENU_TRACE"], "a") as f:
    f.write(json.dumps(record)+"\\n")
if os.environ.get("MUTATE_CONFIG") == "1":
    pathlib.Path(os.environ["HERDR_RELAY_ENV"]).write_text("HERDR_RELAY_TRANSPORT=tailscale-external\\nHERDR_EXTERNAL_HTTPS_ORIGIN=https://private-menu-node.invalid\\n")
if os.environ.get("WAIT_INT") == "1":
    def interrupted(signum, frame):
        print("Disposable action interrupted", flush=True)
        raise SystemExit(130)
    signal.signal(signal.SIGINT, interrupted)
    print("FIXTURE_CHILD_WAIT_INT", flush=True)
    time.sleep(30)
print("Disposable action returned", flush=True)
raise SystemExit(int(os.environ.get("ACTION_EXIT", "0")))
'''
DIAGNOSTIC = '''import json, os, sys
with open(os.environ["MENU_TRACE"], "a") as f:
    f.write(json.dumps(dict(kind="diagnostic", argv=sys.argv[1:], menu=os.environ.get("HERDR_SETUP_MENU")))+"\\n")
if os.environ.get("NOISY_DIAGNOSTIC"):
    print("private-menu-path secret-menu-canary", file=sys.stderr)
code = int(os.environ.get("DIAGNOSTIC_EXIT", "3"))
if os.environ.get("EMPTY_DIAGNOSTIC"):
    raise SystemExit(code)
frame = os.environ.get("BAD_FRAME")
if frame == "multiline":
    print("first\\nsecond")
elif frame == "control":
    print("first\\x1bsecond")
elif frame == "oversize":
    print("x"*2049)
elif sys.argv[1] == "status":
    print("selected_transport=unknown status=unknown exit="+str(code)+" local_relay=unknown")
else:
    print("Doctor fixture (inert), exit "+str(code))
raise SystemExit(code)
'''


class Fixture:
    def __init__(self, root, real=False):
        self.root = root
        self.scripts = root / "package with spaces/relay"
        self.home = root / "isolated home"
        self.config = root / "private config"
        self.traps = root / "traps"
        for directory in [self.scripts, self.home, self.config, self.traps]:
            directory.mkdir(parents=True)
        for name in ["plugin-setup-menu.sh", "plugin-status.sh"]:
            shutil.copy2(REPO / "relay" / name, self.scripts / name)
        self.trace = root / "calls.jsonl"
        self.env_file = self.config / "relay.env"
        self.source_marker = root / "source-called"
        self.tool_marker = root / "tool-called"
        self.env_file.write_text('HERDR_GATEWAY_URL=wss://private-menu-node\nGH_TOKEN=secret-menu-canary\nK=$(touch "'+str(self.source_marker)+'")\n')
        self.env_file.chmod(0o600)
        (self.config / "device-auth.json").write_text('synthetic secret-menu-canary; not read\n')
        (self.config / "device-auth.json").chmod(0o600)
        # Even sourcing definitions would trip this. Product menu must be independent.
        executable(self.scripts / "common.sh", '#!/bin/sh\nprintf called > "$MENU_SOURCE"\nexit 91\n')
        for name in ACTIONS:
            executable(self.scripts / name, "#!"+sys.executable+"\n"+ACTOR)
        self.binary = root / "diagnostic"
        executable(self.binary, "#!"+sys.executable+"\n"+DIAGNOSTIC)
        for name in ["curl", "tailscale", "tailscaled", "systemctl", "launchctl", "cloudflared", "herdr", "ssh", "scp",
                     "sudo", "go", "bun", "npm", "npx", "uname", "id", "security", "touch", "brew"]:
            executable(self.traps / name, '#!/bin/sh\nprintf called > "$MENU_TOOL"\nexit 91\n')
        self.env = dict(PATH=str(self.traps)+":/usr/bin:/bin", HOME=str(self.home),
                        HERDR_RELAY_ENV=str(self.env_file), HERDR_RELAY_BIN=HELPER if real else str(self.binary),
                        MENU_TRACE=str(self.trace), MENU_SOURCE=str(self.source_marker), MENU_TOOL=str(self.tool_marker))
        if real:
            self.env_file.write_text("HERDR_RELAY_TRANSPORT=cloudflare\n")

    def snapshot(self):
        result = {}
        for base in [self.scripts, self.home, self.config]:
            for path in [base, *sorted(base.rglob("*"))]:
                info = path.lstat()
                result[str(path.relative_to(self.root))] = (stat.S_IMODE(info.st_mode), path.read_bytes() if path.is_file() else None)
        return result

    def calls(self):
        return [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []

    def navigation(self, data, extra=None, preserve=True):
        before = self.snapshot()
        result = subprocess.run(["/bin/bash", str(self.scripts / "plugin-setup-menu.sh")],
                                input=data, capture_output=True, text=True, env=dict(self.env, **(extra or {})), timeout=25)
        require(result.returncode == 0, "diagnostic/child refusal terminated navigation")
        self.safe_output(result.stdout+result.stderr)
        require(not result.stderr, "navigation emitted unexpected stderr")
        require("Press Enter" not in result.stdout, "piped navigation consumed a return pause")
        if preserve:
            require(self.snapshot() == before, "navigation sourced/migrated/rewrote/chmodded state")
        return result.stdout

    def safe_output(self, text):
        require(not any(canary in text for canary in CANARIES), "private/config/control input escaped presentation")
        require(not self.source_marker.exists() and not self.tool_marker.exists(), "navigation used source/service/network/toolchain capability")

    def actions(self):
        return [call for call in self.calls() if call["kind"] == "action"]

    def headers(self):
        return [call for call in self.calls() if call["kind"] == "diagnostic" and call["argv"][0] == "status"]


def read_until(process, master, output, marker, deadline):
    while marker not in output:
        require(time.monotonic() < deadline, "PTY menu deadline exceeded")
        ready, _, _ = select.select([master], [], [], 0.1)
        if ready:
            try:
                data = os.read(master, 8192)
            except OSError:
                data = b""
            require(data, "PTY closed before required state")
            output.extend(data)
        require(process.poll() is None or marker in output, "menu ended before required state")


def transcript_tail(output, fixture, limit=1200):
    """Bounded PTY transcript tail for failure output: fixed labels only; no paths, canaries or token-like runs."""
    text = bytes(output).decode("utf-8", "replace").replace("\r\n", "\n")
    for value, label in [(str(fixture.scripts), "<scripts>"), (str(fixture.root), "<fixture>")]:
        text = text.replace(value, label)
    for canary in CANARIES:
        text = text.replace(canary, "<redacted>")
    text = re.sub(r"[A-Za-z0-9+/=_-]{32,}", "<redacted>", text)
    text = re.sub(r"(?<![\w<>.])/\S[^\n]*", "<path>", text)  # to end of line: a path may contain spaces
    return "".join(c if c == "\n" or " " <= c <= "~" else "\\u%04x" % ord(c) for c in text)[-limit:]


def report_failure(case, phase, process, output, fixture, started):
    """Best effort: a diagnostics fault must never replace the case's own failure."""
    try:
        state = "running" if process.poll() is None else "exited rc=%s" % process.returncode
        print("%s FAILED in phase '%s' after %.1fs; menu %s; %d transcript bytes; sanitized tail:\n%s\n--- end of tail ---"
              % (case, phase, time.monotonic()-started, state, len(output), transcript_tail(output, fixture)),
              file=sys.stderr, flush=True)
    except Exception:
        print("%s FAILED; transcript diagnostics unavailable" % case, file=sys.stderr, flush=True)


def wait_exit(process, master, output, deadline):
    """Wait for the menu to exit while still reading its PTY. On macOS a session leader that ran an exec'd child
    can stay in exit (unreapable, and killpg returns EPERM) while its PTY output is unread, until the master reads or
    closes it. Reading must not stop at the last marker. Same absolute deadline as every other step; the
    transcript is complete (post-exit drain included) when this returns."""
    while process.poll() is None:
        require(time.monotonic() < deadline, "PTY menu did not exit before the deadline")
        if not select.select([master], [], [], 0.1)[0]:
            continue
        try:
            data = os.read(master, 8192)
        except OSError:
            data = b""
        if not data:
            process.wait(timeout=max(0.1, deadline-time.monotonic()))
            break
        output.extend(data)
    # Drain finite fixture output left after exit: the exit can race the last read. Bounded to 2 s.
    drained_by = time.monotonic()+2
    while time.monotonic() < drained_by and select.select([master], [], [], 0.1)[0]:
        try:
            block = os.read(master, 8192)
        except OSError:
            break
        if not block:
            break
        output.extend(block)


def contain(process, master):
    """Bounded, non-raising teardown of one fixture-owned session; it cannot mask the case's own failure. SIGKILL
    goes only to this new session's group. macOS answers EPERM for a leader that is already exiting; closing the
    master (hangup) is what releases it, so the master is closed before the bounded wait."""
    if process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGKILL)  # solely this fixture-owned new session
        except OSError:
            pass  # ESRCH: already gone; EPERM: exiting macOS leader, released by the master close below
    try:
        os.close(master)
    except OSError:
        pass
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        print("containment incomplete: fixture session leader not reaped 5s after SIGKILL and master close",
              file=sys.stderr, flush=True)


def terminal_case(fixture, doctor=False, eof=False, interrupt=False):
    before = fixture.snapshot()
    master, slave = pty.openpty()
    env = dict(fixture.env, WAIT_INT="1") if interrupt else fixture.env
    # Exact new fixture-owned session; cleanup never scans/signals services.
    process = subprocess.Popen(["/bin/bash", str(fixture.scripts / "plugin-setup-menu.sh")],
                               stdin=slave, stdout=slave, stderr=slave, env=env, start_new_session=True)
    os.close(slave)
    output = bytearray()
    started = time.monotonic()
    deadline = time.monotonic()+25
    phase = "awaiting the main prompt"
    try:
        read_until(process, master, output, b"Choice [1 opens Cloudflare]: ", deadline)
        if eof:
            os.write(master, b"\x04")
        else:
            os.write(master, b"6\n" if doctor else b"4\n")
            if interrupt:
                phase = "awaiting the interruptible child"
                read_until(process, master, output, b"FIXTURE_CHILD_WAIT_INT", deadline)
                actions = fixture.actions()
                require(len(actions) == 1 and not actions[0]["interrupt_ignored"], "child inherited SIG_IGN")
                os.kill(process.pid, signal.SIGINT)  # parent's installed handler must survive
                os.kill(actions[0]["pid"], signal.SIGINT)  # only the just-recorded owned child
            phase = "awaiting the return pause"
            read_until(process, master, output, b"Press Enter to return to the menu.", deadline)
            os.write(master, b"\x04" if interrupt else b"\nq\n")
        phase = "awaiting menu exit (PTY still read)"
        wait_exit(process, master, output, deadline)
        phase = "checking the transcript"
        text = output.decode()
        require(process.returncode == 0, "menu did not survive child refusal/EOF")
        require(text.count("Press Enter to return to the menu.") == (0 if eof else 1), "wrong interactive return pause count")
        require("Press Enter to close this pane" not in text, "child wrapper double-paused")
        if interrupt:
            require("Action did not complete (exit 130)" in text, "interrupted child reported success/lost status")
        fixture.safe_output(text)
        require(before == fixture.snapshot(), "PTY navigation mutated state")
    except Exception:
        report_failure("terminal_case(doctor=%s, eof=%s, interrupt=%s)" % (doctor, eof, interrupt), phase, process, output, fixture, started)
        raise
    finally:
        contain(process, master)


def inplace_update_case(f):
    # In-place update/deleted cwd: only the named replacement directory is used.
    master, slave = pty.openpty()
    child = subprocess.Popen(["/bin/bash", str(f.scripts / "plugin-setup-menu.sh")], stdin=slave, stdout=slave,
                             stderr=slave, env=f.env, start_new_session=True)
    os.close(slave)
    output = bytearray()
    started = time.monotonic()
    deadline = time.monotonic()+25
    phase = "awaiting the main prompt"
    try:
        read_until(child, master, output, b"Choice [1 opens Cloudflare]: ", deadline)
        old = f.root / "old-owned-scripts"
        f.scripts.rename(old)
        f.scripts.mkdir()
        shutil.copy2(old / "plugin-status.sh", f.scripts / "plugin-status.sh")
        shutil.copy2(old / "plugin-setup-link.sh", f.scripts / "plugin-setup-link.sh")
        shutil.rmtree(old)  # exact disposable fixture directory only
        os.write(master, b"4\n")
        phase = "awaiting the return pause"
        read_until(child, master, output, b"Press Enter to return to the menu.", deadline)
        os.write(master, b"\nq\n")
        phase = "awaiting menu exit (PTY still read)"
        wait_exit(child, master, output, deadline)
        phase = "checking the transcript"
        require(child.returncode == 0 and len(f.actions()) == 1 and f.actions()[0]["cwd"] == str(f.scripts), "action used deleted cwd")
        require(not any(word in output.decode() for word in ["getcwd", "cannot access parent directories", "chdir:"]), "replaced directory leaked loader error")
        f.safe_output(output.decode())
    except Exception:
        report_failure("inplace_update_case", phase, child, output, f, started)
        raise
    finally:
        contain(child, master)


with tempfile.TemporaryDirectory(prefix="herdr-setup-menu-") as directory:
    base = Path(directory).resolve()
    number = 0

    def fresh(real=False):
        global number
        number += 1
        return Fixture(base / str(number), real)

    # Invalid/blank/default/Back/EOF/removed aliases cannot select a child.
    for data, headers in [("", 1), ("q\n", 1), ("t\nT\nb\nB\n7\n8\n9\ninvalid\nq\n", 1),
                          ("\n\ninvalid\nb\nq\n", 2), ("1\n", 1), ("2\nq\n", 1),
                          ("2\n\ninvalid\nb\nq\n", 2), ("3\n1\n3\n\ninvalid\nb\nq\n", 2)]:
        f = fresh()
        text = f.navigation(data)
        require(not f.actions() and len(f.headers()) == headers, "navigation/unavailable/default unexpectedly dispatched or reprobed")
        require(all(call["argv"] == ["status", "--config", str(f.env_file)] and call["menu"] == "1" for call in f.headers()),
                "header used ingress/arbitrary command or permitted wrapper pause")
        require("Managed Tailscale Serve (foreground)" not in text, "removed managed mode still advertised")
    for submenu in ["1", "2", "3"]:
        f = fresh()
        f.navigation(submenu+"\n")  # EOF at every submenu
        require(not f.actions() and len(f.headers()) == 1, "submenu EOF ran an action")

    maps = [("1\n1\nb\nq\n", "plugin-choose-transport.sh", ["temporary"]),
            ("1\n2\nb\nq\n", "plugin-install-service.sh", []),
            ("1\n3\nb\nq\n", "plugin-change-hostname.sh", []),
            ("1\n4\nb\nq\n", "plugin-stable-teardown.sh", []),
            ("2\n1\nb\nq\n", "plugin-choose-transport.sh", ["community"]),
            ("2\n2\nb\nq\n", "plugin-choose-transport.sh", ["own"]),
            ("3\n2\nb\nq\n", "plugin-choose-transport.sh", ["tailscale-external"]),
            ("4\nq\n", "plugin-setup-link.sh", []),
            ("5\nq\n", "plugin-configure-app-deploy.sh", [])]
    for data, entry, args in maps:
        f = fresh()
        f.navigation(data)
        calls = f.actions()
        require(len(calls) == 1 and calls[0]["entry"] == entry and calls[0]["argv"] == args, "explicit action map mismatch")
        require(calls[0]["menu"] == "1" and calls[0]["cwd"] == str(f.scripts), "action did not enter current directory/stand down child pause")
        require(all(value is None for value in calls[0]["consent"].values()) and calls[0]["routing"] is None,
                "menu loaded file-backed routing/automatic consent")
        require(len(f.headers()) == 2, "main return did not refresh header exactly once")
    f = fresh()
    text = f.navigation("6\nq\n")
    require([call["argv"][0] for call in f.calls()] == ["status", "doctor", "status"], "Doctor bypassed wrapper or started another action")
    require("Action did not complete (exit 3)" in text, "incomplete doctor decision hidden")

    for code in [0, 1, 2, 3]:
        f = fresh()
        text = f.navigation("q\n", dict(DIAGNOSTIC_EXIT=str(code), NOISY_DIAGNOSTIC="1"))
        require("exit="+str(code) in text and len(f.headers()) == 1, "header decision changed navigation or was reparsed")
    for extra in [dict(DIAGNOSTIC_EXIT="127", EMPTY_DIAGNOSTIC="1"), dict(DIAGNOSTIC_EXIT="127"),
                  dict(BAD_FRAME="multiline"), dict(BAD_FRAME="control"), dict(BAD_FRAME="oversize")]:
        f = fresh()
        require("diagnostic unavailable" in f.navigation("q\n", extra), "invalid/unavailable header not fixed unknown")
    f = fresh()
    f.env["HERDR_RELAY_BIN"] = str(f.root / "private-menu-path-missing")
    require("diagnostic unavailable" in f.navigation("q\n"), "missing binary triggered guessing/build/download")
    f = fresh()
    (f.scripts / "plugin-status.sh").unlink()  # disposable missing wrapper case
    require("diagnostic unavailable" in f.navigation("q\n"), "missing wrapper terminated navigation")
    f = fresh()
    text = f.navigation("4\nq\n", dict(ACTION_EXIT="73"))
    require(len(f.actions()) == 1 and "Action did not complete (exit 73)" in text, "failed action dispatched a follow-up or lost exit")

    # Real compiled partial diagnostic: private selected transport is finite,
    # metadata remains unavailable; no startup/config.Load/Tailscale or external capability.
    for mode in ["cloudflare", "gateway", "tailscale", "tailscale-external", "tailscale-cli"]:
        f = fresh(real=True)
        config = "HERDR_RELAY_TRANSPORT="+mode+"\n"
        if mode == "gateway":
            config += "HERDR_GATEWAY_URL=wss://private-menu-node.invalid\n"
        if mode == "tailscale-external":
            config += "HERDR_EXTERNAL_HTTPS_ORIGIN=https://private-menu-node.invalid\n"
        f.env_file.write_text(config)
        text = f.navigation("t\n3\n1\n3\nb\n6\nq\n")
        require("selected_transport="+mode in text and "status=unknown exit=3" in text, "actual partial model selection missing/claimed ready")
        require(not f.actions(), "actual read-only navigation started a lifecycle")
    f = fresh(real=True)
    f.env_file.write_text('K=$(touch "'+str(f.source_marker)+'")\n')
    require("selected_transport=unknown status=failed exit=2" in f.navigation("q\n"), "actual refused config guessed Cloudflare/terminated navigation")
    f = fresh(real=True)
    f.env_file.unlink()
    require("selected_transport=unknown status=unknown exit=3" in f.navigation("q\n"), "actual missing config invented selection")
    f = fresh(real=True)
    text = f.navigation("4\nq\n", dict(MUTATE_CONFIG="1"), preserve=False)
    require("selected_transport=cloudflare" in text and "selected_transport=tailscale-external" in text,
            "return header reused cached parent config instead of genuine doctor refresh")
    require(len(f.actions()) == 1 and f.actions()[0]["routing"] is None, "file-backed config exported to explicit child")

    terminal_case(fresh())
    terminal_case(fresh(), doctor=True)
    terminal_case(fresh(), eof=True)
    terminal_case(fresh(), interrupt=True)

    inplace_update_case(fresh())

print("hosted navigation/header fixtures passed (not downstream lifecycle/package/phone qualification)")

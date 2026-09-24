#!/usr/bin/env python3
"""Native Linux lifecycle qualification for the Tailscale process supervisor.

This driver is intentionally unusable outside the private bwrap PID/network
namespace prepared by the invoking verification command.
"""
import ctypes
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest


if os.environ.get("HERDR_S9B3_SANDBOX") != "1":
    raise SystemExit("Refusing to run process-group fixtures outside the private S9B3 bwrap sandbox")
try:
    pid_namespace = os.readlink("/proc/self/ns/pid")
    net_namespace = os.readlink("/proc/self/ns/net")
except OSError as exc:
    raise SystemExit(f"Refusing process-group fixtures without namespace evidence: {exc}")
if pid_namespace == os.environ.get("HERDR_S9B3_HOST_PID_NS"):
    raise SystemExit("Refusing process-group fixtures: PID namespace is not private")
if net_namespace == os.environ.get("HERDR_S9B3_HOST_NET_NS"):
    raise SystemExit("Refusing process-group fixtures: network namespace is not private")

SOURCE = Path(os.environ.get("HERDR_S9B3_SOURCE", ""))
SUPERVISOR = Path(os.environ.get("HERDR_S9B3_SUPERVISOR", ""))
if not (SOURCE / "relay/common.sh").is_file() or not (SOURCE / "relay/tailscale.sh").is_file():
    raise SystemExit("Candidate relay/common.sh and relay/tailscale.sh are required")
if not SUPERVISOR.is_file() or not os.access(SUPERVISOR, os.X_OK):
    raise SystemExit("The real built herdr-mobile-relay supervisor command is required")

# Make killed orphaned fixtures waitable so checks distinguish a dead process
# from a live one and can positively reap each test-owned descendant.
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(36, 1, 0, 0, 0) != 0:
    raise SystemExit(f"Could not enable fixture-only subreaping: errno {ctypes.get_errno()}")

ADAPTER = r'''#!/usr/bin/python3
import json, os, signal, subprocess, sys, time
from pathlib import Path

state = Path(os.environ["HERDR_S9B3_STATE"])
scenario = os.environ["HERDR_S9B3_SCENARIO"]


def json_out(value):
    print(json.dumps(value, separators=(",", ":")))


def status(configured):
    return {
        "backend_state": "Running", "logged_in": True,
        "origin": "https://node.example.invalid:8443",
        "serve_configured": configured, "funnel_configured": False,
        "serve_route_count": 1 if configured else 0,
        "serve_route_owned": configured, "serve_inspected": True,
        "exposure_complete": True,
    }


def tree(role, exit_first=False):
    leader = state / (role + "-leader.pid")
    descendant = state / (role + "-descendant.pid")
    ready = state / (role + "-descendant.ready")
    leader.write_text(str(os.getpid()))
    child = subprocess.Popen([sys.executable, __file__, "--descendant", str(ready)])
    descendant.write_text(str(child.pid))
    deadline = time.monotonic() + 5
    while not ready.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    if not ready.exists():
        raise SystemExit("descendant fixture did not become ready")
    if role == "tailscale" and not exit_first:
        (state / "serve-configured").write_text("owned fixture route")

    def stop(_signum, _frame):
        if role == "tailscale" and scenario != "cleanup-failure":
            try:
                (state / "serve-configured").unlink()
            except FileNotFoundError:
                pass
        raise SystemExit(0)

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    if exit_first:
        raise SystemExit(27)
    while True:
        time.sleep(1)


if len(sys.argv) > 1 and sys.argv[1] == "--descendant":
    ready = Path(sys.argv[2])
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    ready.write_text(str(os.getpid()))
    while True:
        time.sleep(1)

if len(sys.argv) > 1 and sys.argv[1] == "--curl":
    url = sys.argv[-1]
    if scenario == "relay-exit" and url.startswith("http://127.0.0.1:"):
        raise SystemExit(22)
    json_out({
        "status": "ok", "instance": os.environ["HERDR_RELAY_INSTANCE_ID"],
        "version": "fixture", "revision": "fixture-revision", "protocol": "1",
        "bundle_hash": "fixture-bundle-hash", "bundle_version": "fixture",
        "bundle_revision": "fixture-revision",
        "managed_run_id": os.environ.get("HERDR_RELAY_RUN_ID", ""),
        "transport": "tailscale",
        "tailscale_origin": os.environ.get("HERDR_TAILSCALE_ORIGIN", "https://node.example.invalid:8443"),
    })
    raise SystemExit(0)

if len(sys.argv) > 1 and sys.argv[1] == "--tailscale":
    args = sys.argv[2:]
    if args and args[0] == "serve":
        tree("tailscale", exit_first=(scenario == "serve-exit"))
    raise SystemExit(0)

args = sys.argv[1:]
if not args:
    raise SystemExit(2)
if args[0] == "serve":
    tree("relay", exit_first=(scenario == "relay-exit"))
elif args[0] == "check-port":
    raise SystemExit(0)
elif args[0] == "version":
    json_out({"version": "fixture", "revision": "fixture-revision"})
elif args[0] == "tailscale":
    configured = (state / "serve-configured").exists()
    json_out(status(configured))
elif args[0] == "pairing-control":
    operation = ""
    for index, item in enumerate(args[:-1]):
        if item == "--operation":
            operation = args[index + 1]
    run_id = os.environ.get("HERDR_RELAY_RUN_ID", "fixture-run")
    instance = os.environ["HERDR_RELAY_INSTANCE_ID"]
    if operation == "arm_bootstrap":
        json_out({"ok": True, "run_id": run_id, "instance": instance,
                  "invitation_armed": True, "invitation_expires_at": "2099-01-01T00:00:00Z"})
    else:
        json_out({"ready": True})
elif args[0] == "normalize-origin":
    print(args[-1])
elif args[0] == "setup-fragment":
    print("relay=fixture&host=fixture")
elif args[0] == "qr":
    pass
else:
    raise SystemExit(93)
'''

RELAY_ADAPTER = r'''#!/bin/bash
set -euo pipefail
if [ "${1:-}" = supervise ]; then
    shift
    grace=5s
    if [ "${1:-}" = --grace ]; then grace="$2"; shift 2; fi
    [ "${1:-}" = -- ] || exit 96
    shift
    if [ "${1:-}" = "$HERDR_TAILSCALE_BIN" ]; then
        printf '%s\n' "$$" > "$HERDR_S9B3_STATE/tailscale-supervisor.pid"
    else
        printf '%s\n' "$$" > "$HERDR_S9B3_STATE/relay-supervisor.pid"
    fi
    exec "$HERDR_S9B3_SUPERVISOR" supervise --grace "$grace" -- "$@"
fi
exec /usr/bin/python3 "$HERDR_S9B3_ADAPTER" "$@"
'''

TAILSCALE_ADAPTER = r'''#!/bin/bash
set -euo pipefail
exec /usr/bin/python3 "$HERDR_S9B3_ADAPTER" --tailscale "$@"
'''

CURL_ADAPTER = r'''#!/bin/bash
set -euo pipefail
exec /usr/bin/python3 "$HERDR_S9B3_ADAPTER" --curl "$@"
'''


class ProcessGroupIntegration(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="s9b3-")).resolve()
        self.addCleanup(shutil.rmtree, self.root, True)
        self.state = self.root / "state"
        self.state.mkdir()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.scripts = self.root / "relay"
        self.scripts.mkdir()
        shutil.copy2(SOURCE / "relay/common.sh", self.scripts / "common.sh")
        shutil.copy2(SOURCE / "relay/tailscale.sh", self.scripts / "tailscale.sh")
        self.adapter = self.root / "adapter.py"
        self.adapter.write_text(ADAPTER)
        self.relay = self.tools / "relay-adapter"
        self.relay.write_text(RELAY_ADAPTER)
        self.tailscale = self.tools / "tailscale-adapter"
        self.tailscale.write_text(TAILSCALE_ADAPTER)
        self.curl = self.tools / "curl"
        self.curl.write_text(CURL_ADAPTER)
        for path in (self.relay, self.tailscale, self.curl):
            path.chmod(0o755)
        self.env_file = self.root / "config" / "relay.env"
        self.env_file.parent.mkdir(mode=0o700)
        self.env_file.write_text(
            "HERDR_RELAY_TOKEN='fixture-token'\n"
            "HERDR_RELAY_INSTANCE_ID='fixture-instance'\n"
            "HERDR_RELAY_TRANSPORT='tailscale'\n"
        )
        self.session = self.env_file.parent / "tailscale-session.env"
        self.logs = self.env_file.parent
        self.scenario = "stable"
        self.launcher = None
        self.base_env = os.environ.copy()
        self.base_env.update({
            "HOME": str(self.root / "home"),
            "XDG_CONFIG_HOME": str(self.root / "xdg-config"),
            "XDG_DATA_HOME": str(self.root / "xdg-data"),
            "TMPDIR": str(self.root / "tmp"),
            "PATH": str(self.tools) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
            "HERDR_RELAY_ENV": str(self.env_file),
            "HERDR_RELAY_BIN": str(self.relay),
            "HERDR_TAILSCALE_BIN": str(self.tailscale),
            "HERDR_S9B3_SUPERVISOR": str(SUPERVISOR),
            "HERDR_S9B3_ADAPTER": str(self.adapter),
            "HERDR_S9B3_STATE": str(self.state),
            "HERDR_S9B3_SCENARIO": self.scenario,
            "HERDR_PHONE_APP_URL": "https://app.example.invalid",
            "HERDR_RELAY_HOST": "127.0.0.1",
            "HERDR_RELAY_PORT": "8375",
            "HERDR_RELAY_PLUGIN_PORT": "8376",
            "HERDR_TAILSCALE_HTTPS_PORT": "8443",
            "NO_COLOR": "1",
        })
        for directory in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "TMPDIR"):
            Path(self.base_env[directory]).mkdir(parents=True, exist_ok=True)

    def spawn_launcher(self, scenario):
        self.scenario = scenario
        env = self.base_env.copy()
        env["HERDR_S9B3_SCENARIO"] = scenario
        self.launcher = subprocess.Popen(
            ["/bin/bash", str(self.scripts / "tailscale.sh"), "--confirm-serve"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, env=env, start_new_session=True,
        )
        return self.launcher

    def wait_for(self, predicate, message, timeout=12):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            if self.launcher is not None and self.launcher.poll() is not None:
                break
            time.sleep(0.025)
        self.fail(message)

    def wait_launcher(self, timeout=45):
        try:
            return self.launcher.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.launcher.kill()
            stdout, stderr = self.launcher.communicate()
            self.fail(f"launcher timed out\nstdout:\n{stdout}\nstderr:\n{stderr}")

    def fixture_pid(self, name):
        return int((self.state / name).read_text().strip())

    def assert_pid_retired(self, pid, timeout=5):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                waited, _ = os.waitpid(pid, os.WNOHANG)
                if waited == pid:
                    return
            except ChildProcessError:
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    return
            time.sleep(0.02)
        self.fail(f"test-owned process {pid} survived group cleanup")

    def start_foreign(self):
        ready = self.root / "foreign.ready"
        code = (
            "import os,signal,sys,time; "
            "signal.signal(signal.SIGINT,signal.SIG_IGN); "
            "signal.signal(signal.SIGTERM,signal.SIG_IGN); "
            "open(sys.argv[1],'w').write(str(os.getpid())); "
            "time.sleep(3600)"
        )
        foreign = subprocess.Popen([sys.executable, "-c", code, str(ready)], start_new_session=True)
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(ready.exists(), "foreign fixture failed its readiness handshake")
        self.addCleanup(self.stop_foreign, foreign)
        return foreign

    def stop_foreign(self, foreign):
        if foreign.poll() is None:
            foreign.kill()
        foreign.wait(timeout=5)

    def assert_session_supervisor_pids(self):
        content = self.session.read_text()
        values = dict(line.split("=", 1) for line in content.splitlines() if "=" in line)
        self.assertIn("HERDR_RELAY_SUPERVISOR_PID", values)
        self.assertIn("HERDR_TAILSCALE_SUPERVISOR_PID", values)
        self.assertNotIn("HERDR_RELAY_PID", values)
        self.assertNotIn("HERDR_TAILSCALE_PID", values)
        self.assertEqual(values["HERDR_RELAY_SUPERVISOR_PID"], (self.state / "relay-supervisor.pid").read_text().strip())
        self.assertEqual(values["HERDR_TAILSCALE_SUPERVISOR_PID"], (self.state / "tailscale-supervisor.pid").read_text().strip())

    def test_relay_and_serve_descendants_retire_launcher_signal_and_foreign_survives(self):
        foreign = self.start_foreign()
        launcher = self.spawn_launcher("stable")
        self.wait_for(lambda: (self.state / "tailscale-descendant.ready").exists(), "Serve descendant did not start")
        self.wait_for(lambda: self.session.exists() and "HERDR_TAILSCALE_SUPERVISOR_PID=" in self.session.read_text(), "supervisor session record did not become ready")
        self.assert_session_supervisor_pids()
        self.assertTrue((self.state / "relay-descendant.ready").exists(), "relay descendant did not start")
        self.wait_for(lambda: (self.state / "serve-configured").exists(), "inert Serve fixture did not become ready")

        relay_leader = self.fixture_pid("relay-leader.pid")
        relay_descendant = self.fixture_pid("relay-descendant.pid")
        serve_leader = self.fixture_pid("tailscale-leader.pid")
        serve_descendant = self.fixture_pid("tailscale-descendant.pid")
        launcher.send_signal(signal.SIGTERM)
        stdout, stderr = self.wait_launcher()
        self.assertNotIn("Cleanup evidence was retained", stderr)
        self.assert_pid_retired(relay_leader)
        self.assert_pid_retired(relay_descendant)
        self.assert_pid_retired(serve_leader)
        self.assert_pid_retired(serve_descendant)
        self.assertFalse(self.session.exists(), "successful cleanup retained session record")
        self.assertFalse(list(self.logs.glob(".tailscale-*-log.*")), "successful cleanup retained logs")
        foreign.send_signal(signal.SIGCONT)
        self.assertIsNone(foreign.poll(), "foreign process/group was affected by owned cleanup")

    def assert_direct_exit_retires_descendant_without_link(self, scenario, role):
        launcher = self.spawn_launcher(scenario)
        ready = self.state / (role + "-descendant.ready")
        self.wait_for(lambda: ready.exists(), f"{role} descendant did not start")
        pid = self.fixture_pid(role + "-descendant.pid")
        stdout, _stderr = self.wait_launcher(timeout=40)
        self.assertNotIn("private setup link", stdout)
        self.assertNotIn("Open this private setup link", stdout)
        self.assertNotEqual(launcher.returncode, 0, f"{scenario} unexpectedly succeeded")
        self.assert_pid_retired(pid)

    def test_relay_target_exit_retires_descendant_without_printing_setup_link(self):
        self.assert_direct_exit_retires_descendant_without_link("relay-exit", "relay")

    def test_tailscale_target_exit_retires_descendant_without_printing_setup_link(self):
        self.assert_direct_exit_retires_descendant_without_link("serve-exit", "tailscale")

    def test_cleanup_failure_retains_session_record_and_logs(self):
        launcher = self.spawn_launcher("cleanup-failure")
        self.wait_for(lambda: (self.state / "tailscale-descendant.ready").exists(), "Serve descendant did not start")
        self.wait_for(lambda: self.session.exists() and "HERDR_TAILSCALE_SUPERVISOR_PID=" in self.session.read_text(), "session record did not include supervisor PIDs")
        pids = [self.fixture_pid("relay-descendant.pid"), self.fixture_pid("tailscale-descendant.pid")]
        launcher.send_signal(signal.SIGTERM)
        stdout, stderr = self.wait_launcher()
        self.assertNotEqual(launcher.returncode, 0, f"fixture failed to exercise cleanup refusal\n{stdout}\n{stderr}")
        self.assertTrue(self.session.is_file(), "cleanup failure discarded the diagnostic session record")
        self.assertTrue(list(self.logs.glob(".tailscale-*-log.*")), "cleanup failure discarded child logs")
        for pid in pids:
            self.assert_pid_retired(pid)


if __name__ == "__main__":
    unittest.main(verbosity=2)

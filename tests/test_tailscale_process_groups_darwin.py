#!/usr/bin/env python3
"""Native Darwin lifecycle qualification using per-process Unix-socket handles.

This is deliberately separate from the Linux bwrap driver: it uses no /proc,
PID namespace, prctl, process-table polling, or numeric PID signalling.
"""
import os
from pathlib import Path
import platform
import secrets
import select
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest


if sys.platform != "darwin":
    raise SystemExit("Refusing Darwin lifecycle fixtures outside macOS")
if os.environ.get("HERDR_S9B3_DISPOSABLE_DARWIN") != "1":
    raise SystemExit("Refusing Darwin lifecycle fixtures without the disposable-runner gate")
if platform.machine().lower() not in ("arm64", "aarch64"):
    raise SystemExit("Refusing native qualification: this driver requires Darwin/arm64")

SOURCE = Path(os.environ.get("HERDR_S9B3_SOURCE", ""))
SUPERVISOR = Path(os.environ.get("HERDR_S9B3_SUPERVISOR", ""))
if not (SOURCE / "relay/common.sh").is_file() or not (SOURCE / "relay/tailscale.sh").is_file():
    raise SystemExit("The exact candidate relay/common.sh and relay/tailscale.sh are required")
if not SUPERVISOR.is_file() or not os.access(SUPERVISOR, os.X_OK):
    raise SystemExit("The exact built herdr-mobile-relay supervisor is required")

ADAPTER = r'''import json, os, pathlib, signal, socket, subprocess, sys, time

state = pathlib.Path(os.environ["HERDR_S9B3_DARWIN_STATE"])
scenario = os.environ["HERDR_S9B3_DARWIN_SCENARIO"]
nonce = os.environ["HERDR_S9B3_DARWIN_NONCE"]
socket_path = os.environ["HERDR_S9B3_DARWIN_SOCKET"]


def connect(role):
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.connect(socket_path)
    connection.sendall((nonce + "|" + role + "\n").encode("utf-8"))
    return connection


def hold(role, ignore=False):
    connection = connect(role)
    if ignore:
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    return connection


def descendant(role):
    connection = hold(role, ignore=True)
    (state / (role + ".ready")).write_text(nonce)
    try:
        while True:
            time.sleep(1)
    finally:
        connection.close()


def tree(kind, exit_first=False):
    role = kind + "-leader"

    def retire(_signum, _frame):
        if kind == "relay":
            try:
                (state / "relay-ready").unlink()
            except FileNotFoundError:
                pass
        if kind == "tailscale" and scenario != "cleanup-failure":
            try:
                (state / "serve-configured").unlink()
            except FileNotFoundError:
                pass
        raise SystemExit(0)

    signal.signal(signal.SIGINT, retire)
    signal.signal(signal.SIGTERM, retire)
    connection = hold(role)
    child_role = kind + "-descendant"
    child = subprocess.Popen(
        [sys.executable, __file__, "--descendant", child_role],
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    ready = state / (child_role + ".ready")
    deadline = time.monotonic() + 8
    while not ready.exists() and time.monotonic() < deadline:
        if child.poll() is not None:
            raise SystemExit("descendant fixture exited before its socket handshake")
        time.sleep(0.01)
    if not ready.exists():
        raise SystemExit("descendant fixture did not complete its socket handshake")
    if kind == "tailscale" and not exit_first:
        (state / "serve-configured").write_text("fixture route")
    if kind == "relay" and not exit_first:
        (state / "relay-ready").write_text(nonce)
    try:
        if exit_first:
            raise SystemExit(27)
        while True:
            time.sleep(1)
    finally:
        connection.close()


def inspection():
    configured = (state / "serve-configured").exists()
    result = {
        "backend_state": "Running", "logged_in": True,
        "origin": "https://node.example.invalid:8443",
        "serve_configured": configured, "funnel_configured": False,
        "serve_route_count": 1 if configured else 0,
        "serve_route_owned": configured, "serve_inspected": True,
        "exposure_complete": True,
    }
    if configured:
        result["serve_route_origin"] = result["origin"]
    print(json.dumps(result, separators=(",", ":")))


def main(args):
    if args and args[0] == "--descendant":
        descendant(args[1])
        return
    if args and args[0] == "--foreign":
        connection = hold("foreign")
        try:
            sys.stdin.readline()
        finally:
            connection.close()
        return
    if args and args[0] == "--tailscale":
        command = args[1:]
        if command and command[0] == "serve":
            tree("tailscale", scenario == "serve-exit")
            return
        raise SystemExit(91)
    if args and args[0] == "--curl":
        url = args[-1]
        result = {
            "status": "ok", "instance": os.environ["HERDR_RELAY_INSTANCE_ID"],
            "version": "fixture", "revision": "fixture-revision",
            "bundle_hash": "fixture-bundle-hash", "bundle_version": "fixture",
            "bundle_revision": "fixture-revision", "protocol": "1",
        }
        if url.startswith("https://"):
            result.update({
                "managed_run_id": os.environ.get("HERDR_RELAY_RUN_ID", ""),
                "transport": "tailscale",
                "tailscale_origin": "https://node.example.invalid:8443",
            })
        print(json.dumps(result, separators=(",", ":")))
        return

    if not args:
        raise SystemExit(2)
    command = args[0]
    if command == "serve":
        tree("relay", scenario == "relay-exit")
    elif command == "tailscale" and args[1:2] == ["inspect"]:
        inspection()
    elif command == "check-port":
        return
    elif command == "version":
        print('{"version":"fixture","revision":"fixture-revision"}')
    elif command == "pairing-control":
        operation = ""
        for index, item in enumerate(args[:-1]):
            if item == "--operation":
                operation = args[index + 1]
        run_id = os.environ.get("HERDR_RELAY_RUN_ID", "fixture-run")
        instance = os.environ["HERDR_RELAY_INSTANCE_ID"]
        if operation == "status":
            ready = state / "relay-ready"
            deadline = time.monotonic() + 5
            while (
                not ready.is_file()
                and scenario != "relay-exit"
                and time.monotonic() < deadline
            ):
                time.sleep(0.01)
            if not ready.is_file():
                raise SystemExit(94)
            print('{"ready":true}')
        elif operation == "arm_bootstrap":
            print(json.dumps({
                "ok": True, "run_id": run_id, "instance": instance,
                "invitation_armed": True,
                "invitation_expires_at": "2099-01-01T00:00:00Z",
            }, separators=(",", ":")))
        else:
            raise SystemExit(92)
    elif command == "normalize-origin":
        print(args[-1])
    elif command == "setup-fragment":
        print("relay=fixture&host=fixture")
    elif command == "qr":
        return
    else:
        raise SystemExit(93)


main(sys.argv[1:])
'''

RELAY_ADAPTER = r'''#!/bin/bash
set -euo pipefail
if [ "${1:-}" = json-field ]; then
    exec "$HERDR_S9B3_SUPERVISOR" "$@"
fi
if [ "${1:-}" = supervise ]; then
    shift
    exec "$HERDR_S9B3_SUPERVISOR" supervise "$@"
fi
exec "$HERDR_S9B3_DARWIN_PYTHON" "$HERDR_S9B3_DARWIN_ADAPTER" "$@"
'''

TAILSCALE_ADAPTER = r'''#!/bin/bash
set -euo pipefail
exec "$HERDR_S9B3_DARWIN_PYTHON" "$HERDR_S9B3_DARWIN_ADAPTER" --tailscale "$@"
'''

CURL_ADAPTER = r'''#!/bin/bash
set -euo pipefail
exec "$HERDR_S9B3_DARWIN_PYTHON" "$HERDR_S9B3_DARWIN_ADAPTER" --curl "$@"
'''

LAUNCHCTL_ADAPTER = r'''#!/bin/bash
exit 1
'''

OWNED_ROLES = {
    "relay-leader", "relay-descendant",
    "tailscale-leader", "tailscale-descendant",
}


class DarwinLifecycle(unittest.TestCase):
    def setUp(self):
        os.umask(0o077)
        self.root = Path(tempfile.mkdtemp(prefix="s9d-", dir="/tmp"))
        os.chmod(self.root, 0o700)
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.tools = self.root / "tools"
        self.tools.mkdir(mode=0o700)
        self.config_dir = self.root / "config"
        self.config_dir.mkdir(mode=0o700)
        self.home = self.root / "home"
        self.home.mkdir(mode=0o700)
        self.tmp = self.root / "tmp"
        self.tmp.mkdir(mode=0o700)
        self.xdg = {}
        for name in ("config", "data", "cache", "state", "runtime"):
            path = self.root / ("xdg-" + name)
            path.mkdir(mode=0o700)
            self.xdg[name] = path
        self.socket_path = self.root / "s"
        self.nonce = secrets.token_hex(32)
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(str(self.socket_path))
        self.listener.listen(16)
        self.listener.settimeout(0.1)
        self.connections = {}
        self.launcher = None
        self.launcher_log = None
        self.foreign = None
        self.foreign_log = None
        self.scenario = "stable"

        self.adapter = self.root / "adapter.py"
        self.adapter.write_text(ADAPTER)
        self.relay = self.tools / "relay"
        self.tailscale = self.tools / "tailscale"
        self.curl = self.tools / "curl"
        self.launchctl = self.tools / "launchctl"
        for path, text in (
            (self.relay, RELAY_ADAPTER),
            (self.tailscale, TAILSCALE_ADAPTER),
            (self.curl, CURL_ADAPTER),
            (self.launchctl, LAUNCHCTL_ADAPTER),
        ):
            path.write_text(text)
            path.chmod(0o700)

        self.env_file = self.config_dir / "relay.env"
        self.env_file.write_text(
            "HERDR_RELAY_TOKEN='fixture-token'\n"
            "HERDR_RELAY_INSTANCE_ID='fixture-instance'\n"
            "HERDR_RELAY_TRANSPORT='tailscale'\n"
            "HERDR_TAILSCALE_ORIGIN='https://node.example.invalid:8443'\n"
        )
        self.base_env = os.environ.copy()
        self.base_env.update({
            "HOME": str(self.home),
            "TMPDIR": str(self.tmp),
            "XDG_CONFIG_HOME": str(self.xdg["config"]),
            "XDG_DATA_HOME": str(self.xdg["data"]),
            "XDG_CACHE_HOME": str(self.xdg["cache"]),
            "XDG_STATE_HOME": str(self.xdg["state"]),
            "XDG_RUNTIME_DIR": str(self.xdg["runtime"]),
            "PATH": str(self.tools) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
            "HERDR_RELAY_ENV": str(self.env_file),
            "HERDR_RELAY_BIN": str(self.relay),
            "HERDR_TAILSCALE_BIN": str(self.tailscale),
            "HERDR_S9B3_SUPERVISOR": str(SUPERVISOR),
            "HERDR_S9B3_DARWIN_ADAPTER": str(self.adapter),
            "HERDR_S9B3_DARWIN_PYTHON": sys.executable,
            "HERDR_S9B3_DARWIN_STATE": str(self.state),
            "HERDR_S9B3_DARWIN_SOCKET": str(self.socket_path),
            "HERDR_S9B3_DARWIN_NONCE": self.nonce,
            "HERDR_S9B3_DARWIN_SCENARIO": self.scenario,
            "HERDR_PHONE_APP_URL": "https://app.example.invalid",
            "HERDR_RELAY_HOST": "127.0.0.1",
            "HERDR_RELAY_PORT": "8375",
            "HERDR_RELAY_PLUGIN_PORT": "8376",
            "HERDR_TAILSCALE_HTTPS_PORT": "8443",
            "NO_COLOR": "1",
        })

    def accept_roles(self, expected):
        deadline = time.monotonic() + 45
        expected = set(expected)
        while not expected.issubset(set(self.connections)) and time.monotonic() < deadline:
            try:
                connection, _address = self.listener.accept()
            except socket.timeout:
                continue
            connection.settimeout(5)
            message = b""
            while b"\n" not in message:
                chunk = connection.recv(256)
                if not chunk:
                    self.fail("fixture disconnected before its nonce/role handshake")
                message += chunk
            line, extra = message.split(b"\n", 1)
            if extra:
                self.fail("fixture sent data after its one-line handshake")
            try:
                received_nonce, role = line.decode("utf-8").split("|", 1)
            except (UnicodeDecodeError, ValueError):
                self.fail("fixture sent a malformed socket handshake")
            if received_nonce != self.nonce:
                self.fail("fixture socket handshake carried the wrong run nonce")
            if role not in expected or role in self.connections:
                self.fail("fixture sent an unexpected or duplicate role handshake: " + role)
            connection.setblocking(False)
            self.connections[role] = connection
        missing = expected - set(self.connections)
        if missing:
            self.fail("timed out waiting for socket handshakes: " + ", ".join(sorted(missing)))

    def start_foreign(self):
        self.foreign_log = open(self.root / "foreign.log", "wb")
        self.foreign = subprocess.Popen(
            [sys.executable, str(self.adapter), "--foreign"],
            stdin=subprocess.PIPE, stdout=self.foreign_log, stderr=subprocess.STDOUT,
            env=self.base_env,
        )
        self.accept_roles({"foreign"})

    def start_launcher(self, scenario):
        self.scenario = scenario
        self.base_env["HERDR_S9B3_DARWIN_SCENARIO"] = scenario
        self.launcher_log = open(self.root / "launcher.log", "wb")
        self.launcher = subprocess.Popen(
            ["/bin/bash", str(SOURCE / "relay/tailscale.sh"), "--confirm-serve"],
            stdin=subprocess.DEVNULL, stdout=self.launcher_log, stderr=subprocess.STDOUT,
            env=self.base_env, start_new_session=True,
        )

    def wait_launcher(self):
        try:
            status = self.launcher.wait(timeout=45)
        except subprocess.TimeoutExpired:
            self.fail("launcher exceeded the bounded lifecycle timeout; no fallback signal was sent")
        self.launcher_log.flush()
        self.launcher_log.close()
        return status

    def assert_owned_eof(self, roles):
        for role in sorted(roles):
            connection = self.connections[role]
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                readable, _writable, _exceptional = select.select([connection], [], [], 0.1)
                if not readable:
                    continue
                try:
                    data = connection.recv(1)
                except OSError as exc:
                    self.fail("owned role did not retire by orderly EOF: " + role + ": " + str(exc))
                if data == b"":
                    break
                self.fail("owned role sent unexpected post-handshake data: " + role)
            else:
                self.fail("owned role socket stayed open after launcher cleanup: " + role)

    def assert_foreign_alive(self):
        self.assertIsNone(self.foreign.poll(), "foreign direct Popen handle exited during owned cleanup")
        connection = self.connections["foreign"]
        readable, _writable, _exceptional = select.select([connection], [], [], 0.1)
        if readable:
            if connection.recv(1) == b"":
                self.fail("foreign socket closed during owned cleanup")
            self.fail("foreign fixture sent unexpected post-handshake data")

    def finish_foreign(self):
        self.assert_foreign_alive()
        self.foreign.stdin.write(b"exit\n")
        self.foreign.stdin.flush()
        self.foreign.stdin.close()
        self.foreign.wait(timeout=5)
        self.assert_owned_eof({"foreign"})
        self.foreign_log.close()

    def assert_evidence_removed(self):
        self.assertFalse((self.config_dir / "tailscale-session.env").exists(), "successful cleanup retained session evidence")
        self.assertEqual(list(self.config_dir.glob(".tailscale-*-log.*")), [], "successful cleanup retained child logs")

    def run_case(self, scenario, roles, signal_launcher=False, foreign=False):
        if foreign:
            self.start_foreign()
        self.start_launcher(scenario)
        expected = set(roles)
        if foreign:
            expected.add("foreign")
        self.accept_roles(expected)

        if signal_launcher:
            deadline = time.monotonic() + 20
            link_banner = b"Open this private setup link"
            while time.monotonic() < deadline:
                self.assertIsNone(self.launcher.poll(), "launcher exited before publishing the private link")
                if link_banner in (self.root / "launcher.log").read_bytes():
                    break
                time.sleep(0.01)
            else:
                self.fail("launcher never committed and published its private setup link")
            # The only launcher signal uses the handle returned by this direct
            # Popen; no recorded or observed numeric PID is consulted.
            self.launcher.send_signal(signal.SIGTERM)

        status = self.wait_launcher()
        self.assertNotEqual(status, 0, "fixture scenario unexpectedly returned success")
        self.assert_owned_eof(roles)
        if scenario == "cleanup-failure":
            self.assertTrue((self.config_dir / "tailscale-session.env").is_file(), "cleanup failure discarded session evidence")
            self.assertTrue(list(self.config_dir.glob(".tailscale-*-log.*")), "cleanup failure discarded child logs")
        else:
            self.assert_evidence_removed()
        if scenario in ("relay-exit", "serve-exit"):
            log_text = (self.root / "launcher.log").read_bytes().decode("utf-8", "replace")
            self.assertNotIn("Open this private setup link", log_text)
            self.assertNotIn("Scan this QR code", log_text)
        if foreign:
            self.finish_foreign()

    def test_launcher_termination_retires_owned_roles_and_preserves_foreign(self):
        self.run_case("stable", OWNED_ROLES, signal_launcher=True, foreign=True)

    def test_relay_target_exit_retires_descendant(self):
        self.run_case("relay-exit", {"relay-leader", "relay-descendant"})

    def test_serve_target_exit_retires_descendant(self):
        self.run_case("serve-exit", OWNED_ROLES)

    def test_cleanup_failure_retains_evidence_after_owned_retirement(self):
        self.run_case("cleanup-failure", OWNED_ROLES, signal_launcher=True)

    def tearDown(self):
        result = getattr(getattr(self, "_outcome", None), "result", None)
        failed = result is not None and any(test is self for test, _traceback in result.failures + result.errors)
        if failed:
            print("Darwin fixture artifacts retained for disposable-runner destruction: " + str(self.root))
            return
        if self.launcher_log is not None and not self.launcher_log.closed:
            self.launcher_log.close()
        if self.foreign_log is not None and not self.foreign_log.closed:
            self.foreign_log.close()
        for connection in self.connections.values():
            connection.close()
        self.listener.close()
        print("Darwin fixture artifacts retained: " + str(self.root))


if __name__ == "__main__":
    unittest.main(verbosity=2)

#!/usr/bin/env python3
"""Hosted, disposable lifecycle fixture for the managed Tailscale launcher.

This test copies and runs the candidate shell launcher against a protocol-faithful
Unix-socket control fake, a read-only Tailscale inspection adapter, and inert TLS,
release, and bundle adapters. It must only be enabled by hosted CI; it never
contacts a Tailscale installation or changes host services/trust.
"""
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing launcher lifecycle execution outside the hosted CI gate")

SOURCE = Path(__file__).resolve().parents[1]
FIXTURE_TOKEN = "a1" * 16
FIXTURE_RUN_ID = "11" * 16
FIXTURE_FRAGMENT = "relay=" + FIXTURE_TOKEN + "&host=fixture"
FIXTURE_INSTANCE = "fixture-instance"
FIXTURE_APP = "https://app.example.invalid"

ADAPTER = r'''#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import socket
import sys
import time

MODE = os.environ.get("HERDR_FIXTURE_MODE", "success")
EVENTS = Path(os.environ["HERDR_FIXTURE_EVENTS"])
STATE = Path(os.environ["HERDR_FIXTURE_STATE"])
SOCKET_PATH = Path(os.environ.get("HERDR_RELAY_PAIRING_SOCKET", os.environ.get("HERDR_FIXTURE_SOCKET", "")))
ENV_FILE = Path(os.environ["HERDR_RELAY_ENV"])
SESSION_FILE = Path(os.environ["HERDR_FIXTURE_SESSION"])
RUN_ID = os.environ.get("HERDR_RELAY_RUN_ID", "fixture-run")
INSTANCE = os.environ["HERDR_RELAY_INSTANCE_ID"]
ORIGIN = os.environ.get("HERDR_TAILSCALE_ORIGIN", "https://relay.example.invalid:8443")


def event(name):
    entry = {"event": name}
    try:
        data = ENV_FILE.read_bytes()
        entry["env_sha256"] = hashlib.sha256(data).hexdigest()
        entry["env_mode"] = ENV_FILE.stat().st_mode & 0o777
    except FileNotFoundError:
        entry["env_sha256"] = None
    try:
        session = dict(line.split("=", 1) for line in SESSION_FILE.read_text().splitlines() if "=" in line)
        entry["session_stage"] = session.get("HERDR_RELAY_STAGE")
    except FileNotFoundError:
        entry["session_stage"] = None
    with EVENTS.open("a", encoding="utf-8") as output:
        output.write(json.dumps(entry, sort_keys=True) + "\n")


def write_state(**values):
    try:
        state = json.loads(STATE.read_text())
    except (OSError, ValueError):
        state = {}
    state.update(values)
    temp = STATE.with_suffix(".tmp")
    temp.write_text(json.dumps(state, sort_keys=True))
    temp.replace(STATE)


def send_control(operation, run_id=None, instance=None, socket_path=None):
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(3)
    connection.connect(socket_path or str(SOCKET_PATH))
    payload = {"protocol": 1, "op": operation, "run_id": run_id or RUN_ID, "instance": instance or INSTANCE}
    connection.sendall((json.dumps(payload, separators=(",", ":")) + "\n").encode())
    data = b""
    while b"\n" not in data:
        chunk = connection.recv(4096)
        if not chunk:
            break
        data += chunk
    connection.close()
    if not data:
        raise SystemExit(1)
    print(data.decode().strip())
    return 0


def serve():
    if SOCKET_PATH.exists() or SOCKET_PATH.is_symlink():
        raise SystemExit("fixture socket collision")
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(str(SOCKET_PATH))
    os.chmod(SOCKET_PATH, 0o600)
    listener.listen(8)
    listener.settimeout(0.2)
    write_state(active=False, armed=False)
    Path(os.environ["HERDR_FIXTURE_SERVER_READY"]).write_text("ready")
    event("server-ready")
    active = False
    armed = False
    stopping = False
    while not stopping:
        try:
            connection, _ = listener.accept()
        except socket.timeout:
            continue
        except OSError:
            break
        connection.settimeout(3)
        data = b""
        while b"\n" not in data:
            chunk = connection.recv(4096)
            if not chunk:
                break
            data += chunk
        try:
            request = json.loads(data.split(b"\n", 1)[0])
        except (ValueError, IndexError):
            connection.close()
            continue
        operation = request.get("op")
        if (request.get("protocol") != 1 or request.get("run_id") != RUN_ID or
                request.get("instance") != INSTANCE):
            response = {"ok": False, "error": "pairing control identity mismatch"}
        elif operation == "status":
            event("status-ready" if armed else "status-active" if active else "status-local-ready")
            response = {
                "ok": True, "run_id": RUN_ID, "instance": INSTANCE,
                "ready": active and armed, "local_ready": True, "owner_held": True,
                "serve_ready": active, "quarantined": False,
                "route_cleared": False, "local_watch_closed": False,
            }
        elif operation == "activate":
            event("activate")
            if MODE == "activation-negative":
                response = {
                    "ok": False, "error": "pairing control operation was refused",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "local_ready": True, "owner_held": True, "serve_ready": False,
                    "quarantined": True, "registration_outcome": "settled-no-write",
                }
            elif MODE == "activation-unresolved":
                response = {
                    "ok": False, "error": "pairing control operation was refused",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "local_ready": True, "owner_held": True, "serve_ready": False,
                    "quarantined": True, "registration_outcome": "unresolved",
                }
            elif MODE == "activation-lost-ack":
                active = True
                write_state(active=True, armed=armed)
                event("activation-committed")
                connection.close()
                continue
            else:
                active = True
                write_state(active=True, armed=armed)
                response = {
                    "ok": True, "run_id": RUN_ID, "instance": INSTANCE,
                    "local_ready": True, "owner_held": True, "serve_ready": True,
                    "quarantined": False, "registration_outcome": "settled-success",
                }
        elif operation == "arm_bootstrap":
            event("arm")
            if MODE == "arm-concurrent-edit":
                ENV_FILE.write_bytes(ENV_FILE.read_bytes().replace(b"preserve-this", b"concurrent-edit"))
                response = {
                    "ok": False, "error": "bootstrap invitation could not be persisted",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "owner_held": True, "serve_ready": True,
                    "arm_outcome": "not-committed",
                }
            elif MODE in ("arm-negative", "arm-committed-negative"):
                committed = MODE == "arm-committed-negative"
                armed = committed
                response = {
                    "ok": False, "error": "bootstrap invitation could not be persisted",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "owner_held": True, "serve_ready": True,
                    "invitation_armed": committed,
                    "arm_outcome": "committed" if committed else "not-committed",
                }
            elif MODE == "arm-unresolved":
                response = {
                    "ok": False, "error": "bootstrap invitation could not be persisted",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "owner_held": True, "serve_ready": True,
                    "arm_outcome": "unresolved",
                }
            elif MODE == "arm-lost-ack":
                armed = True
                write_state(active=active, armed=True)
                event("arm-committed")
                connection.close()
                continue
            else:
                armed = True
                write_state(active=active, armed=True)
                response = {
                    "ok": True, "run_id": RUN_ID, "instance": INSTANCE,
                    "owner_held": True, "serve_ready": True, "quarantined": False,
                    "invitation_armed": True,
                    "invitation_expires_at": "2099-01-01T00:00:00Z",
                    "arm_outcome": "committed",
                }
        elif operation == "retire":
            event("retire")
            allow = Path(os.environ["HERDR_FIXTURE_ALLOW_RETIRE"]).exists()
            if MODE == "retire-refusal" and not allow:
                response = {
                    "ok": False, "error": "pairing control operation was refused",
                    "run_id": RUN_ID, "instance": INSTANCE,
                    "route_cleared": False, "local_watch_closed": False,
                    "remote_watch_retirement_unknown": True,
                }
            else:
                response = {
                    "ok": True, "run_id": RUN_ID, "instance": INSTANCE,
                    "owner_held": True, "route_cleared": True,
                    "local_watch_closed": True,
                    "remote_watch_retirement_unknown": True,
                }
                stopping = True
        else:
            response = {"ok": False, "error": "unsupported pairing control operation"}
        connection.sendall((json.dumps(response, separators=(",", ":")) + "\n").encode())
        connection.close()
    listener.close()
    try:
        SOCKET_PATH.unlink()
    except FileNotFoundError:
        pass
    Path(os.environ["HERDR_FIXTURE_SERVER_RETIRED"]).write_text("route-cleared-and-watch-closed")
    event("server-exit")


def main():
    if len(sys.argv) < 2:
        raise SystemExit(2)
    mode = sys.argv[1]
    args = sys.argv[2:]
    if mode == "relay":
        if not args:
            raise SystemExit(2)
        command = args[0]
        tail = args[1:]
        if command == "json-field":
            kind, key = tail[:2]
            try:
                value = json.load(sys.stdin).get(key)
            except (ValueError, AttributeError):
                raise SystemExit(1)
            valid = ((kind == "bool" and type(value) is bool) or
                     (kind == "string" and isinstance(value, str)) or
                     (kind == "number" and type(value) is int and value >= 0))
            if not valid:
                raise SystemExit(1)
            print(str(value).lower() if type(value) is bool else value)
            return
        if command == "supervise":
            index = 0
            if tail[index:index + 1] == ["--grace"]:
                index += 2
            if tail[index:index + 1] != ["--"]:
                raise SystemExit(96)
            target = tail[index + 1:]
            if not target:
                raise SystemExit(2)
            os.execv(target[0], target)
        if command == "serve":
            serve()
            return
        if command == "check-port":
            return
        if command == "tailscale":
            event("inspect")
            inspection_count = Path(os.environ["HERDR_FIXTURE_INSPECTIONS"])
            count = int(inspection_count.read_text()) if inspection_count.exists() else 0
            count += 1
            inspection_count.write_text(str(count))
            configured = MODE in ("preexisting-route", "preexisting-multiple") or (MODE == "second-inspection-route" and count > 1)
            incomplete = MODE in ("incomplete-inspection", "missing-exposure")
            value = {
                "backend_state": "Running", "logged_in": True,
                "origin": ORIGIN, "node_id": "fixture-node",
                "status_version": "1.102.4-tbbcd7d1fc",
                "serve_configured": configured, "funnel_configured": MODE == "preexisting-funnel",
                "serve_route_count": 2 if MODE == "preexisting-multiple" else 1 if configured else 0,
                "serve_inspected": True,
            }
            if MODE != "missing-exposure":
                value["exposure_complete"] = not incomplete
            print(json.dumps(value, separators=(",", ":")))
            return
        if command == "tailscale-route-check":
            args_map = dict(zip(tail[::2], tail[1::2]))
            current = json.loads(STATE.read_text()) if STATE.exists() else {}
            event("route-check")
            if not current.get("active") or args_map.get("--origin") != ORIGIN or args_map.get("--backend-port") != os.environ.get("HERDR_RELAY_PORT", "8375"):
                raise SystemExit(1)
            return
        if command == "version":
            if tail == ["--json"]:
                event("binary-version")
                print(json.dumps({"version": "fixture-version", "revision": "fixture-revision"}))
                return
            if not tail:
                print("herdr-mobile-relay fixture-version (fixture-revision)")
                return
        if command == "verify-public":
            event("bundle-verify")
            count_file = Path(os.environ["HERDR_FIXTURE_BUNDLE_COUNT"])
            count = int(count_file.read_text()) if count_file.exists() else 0
            count += 1
            count_file.write_text(str(count))
            if MODE == "final-bundle-failure" and count >= 2:
                raise SystemExit(1)
            return
        if command == "pairing-control":
            options = {}
            index = 0
            while index < len(tail):
                if index + 1 >= len(tail):
                    raise SystemExit(2)
                options[tail[index]] = tail[index + 1]
                index += 2
            socket_path = options.get("--socket")
            if not socket_path:
                raise SystemExit(2)
            # The fixture emits a machine-readable negative response with a
            # successful process status, matching the candidate CLI contract.
            return send_control(options.get("--operation", "status"),
                                options.get("--run-id"), options.get("--instance"), options.get("--socket"))
        if command == "normalize-origin":
            print(tail[-1].rstrip("/"))
            return
        if command == "setup-fragment":
            print("relay=" + tail[0] + "&host=fixture")
            return
        if command == "qr":
            event("qr-render")
            print("FIXTURE-QR")
            return
        raise SystemExit(93)
    if mode == "control-server":
        serve()
        return
    if mode == "control-request":
        options = {}
        index = 0
        while index < len(args):
            if index + 1 >= len(args):
                raise SystemExit(2)
            options[args[index]] = args[index + 1]
            index += 2
        return send_control(options.get("--operation", "status"),
                            options.get("--run-id"), options.get("--instance"), options.get("--socket"))
    if mode == "tailscale-cli":
        if args in (["status", "--json"], ["version", "--json", "--daemon"], ["serve", "status", "--json"]):
            event("tailscale-read-only")
            return
        event("tailscale-mutation")
        raise SystemExit(91)
    if mode == "curl":
        url = args[-1] if args else ""
        event("health")
        if not url.endswith("/healthz"):
            raise SystemExit(22)
        print(json.dumps({
            "status": "ok", "readiness": "ready", "instance": INSTANCE,
            "managed_run_id": RUN_ID, "transport": "tailscale",
            "tailscale_origin": ORIGIN, "version": "fixture-version",
            "revision": "fixture-revision", "bundle_hash": "fixture-bundle-hash",
            "bundle_version": "fixture-version", "bundle_revision": "fixture-revision",
        }, separators=(",", ":")))
        return
    if mode == "openssl":
        if args == ["rand", "-hex", "16"]:
            print("11" * 16)
            return
        raise SystemExit(93)
    if mode == "hostname":
        print("fixture-host")
        return
    if mode == "sleep":
        time.sleep(min(float(args[0]) if args else 0.01, 0.02))
        return
    if mode in ("systemctl", "launchctl"):
        raise SystemExit(1)
    if mode == "tput":
        print("80")
        return
    raise SystemExit(93)


if __name__ == "__main__":
    main()
'''


class Fixture:
    def __init__(self, mode="success"):
        self.root = Path(tempfile.mkdtemp(prefix="herdr-tailscale-launcher-", dir="/tmp"))
        self.root.chmod(0o700)
        self.mode = mode
        self.config = self.root / "config"
        self.config.mkdir(mode=0o700)
        self.bin = self.root / "bin"
        self.bin.mkdir(mode=0o700)
        self.scripts = self.root / "relay"
        self.scripts.mkdir(mode=0o700)
        self.home = self.root / "home"
        self.home.mkdir(mode=0o700)
        self.tmp = self.root / "tmp"
        self.tmp.mkdir(mode=0o700)
        for name in ("common.sh", "tailscale.sh"):
            shutil.copy2(SOURCE / "relay" / name, self.scripts / name)
            if (self.scripts / name).read_bytes() != (SOURCE / "relay" / name).read_bytes():
                raise AssertionError("candidate relay script copy did not match")
        self.adapter = self.bin / "adapter.py"
        self.adapter.write_text(ADAPTER)
        self.adapter.chmod(0o700)
        self.env_file = self.config / "relay.env"
        self.session_file = self.config / "tailscale-session.env"
        self.origin_file = self.config / "phone-app-origin-configured"
        self.socket = self.config / "tailscale-control.sock"
        self.events = self.root / "events.jsonl"
        self.state = self.root / "state.json"
        self.inspections = self.root / "inspect-count"
        self.bundle_count = self.root / "bundle-count"
        self.server_ready = self.root / "server-ready"
        self.server_retired = self.root / "server-retired"
        self.allow_retire = self.root / "allow-retire"
        self.original_env = (
            "HERDR_RELAY_TOKEN='" + FIXTURE_TOKEN + "'\n"
            "HERDR_RELAY_INSTANCE_ID='" + FIXTURE_INSTANCE + "'\n"
            "HERDR_RELAY_TRANSPORT='cloudflare'\n"
            "HERDR_UNRELATED_KEY='preserve-this'\n"
        ).encode()
        self.env_file.write_bytes(self.original_env)
        self.env_file.chmod(0o640)
        self.original_origin = b"https://shared.example.invalid\n\n"
        self.origin_file.write_bytes(self.original_origin)
        self.origin_file.chmod(0o640)
        for name in ("herdr-mobile-relay", "tailscale", "curl", "openssl", "hostname", "systemctl", "launchctl", "sleep", "tput"):
            path = self.bin / name
            path.write_text("#!/bin/sh\nexec " + sys.executable + " " + str(self.adapter) + " " +
                            {"herdr-mobile-relay": "relay", "tailscale": "tailscale-cli"}.get(name, name) + " \"$@\"\n")
            path.chmod(0o700)
        self.env = {
            "HOME": str(self.home),
            "TMPDIR": str(self.tmp),
            "PATH": str(self.bin) + os.pathsep + "/usr/bin:/bin",
            "TERM": "dumb", "NO_COLOR": "1",
            "HERDR_RELAY_ENV": str(self.env_file),
            "HERDR_RELAY_BIN": str(self.bin / "herdr-mobile-relay"),
            "HERDR_TAILSCALE_BIN": str(self.bin / "tailscale"),
            "HERDR_TAILSCALE_REQUEST": "1",
            "HERDR_TAILSCALE_LAUNCHER_CI": "1",
            "HERDR_TAILSCALE_HTTPS_PORT": "8443",
            "HERDR_RELAY_PORT": "18375",
            "HERDR_RELAY_PLUGIN_PORT": "18376",
            "HERDR_PHONE_APP_URL": FIXTURE_APP,
            "HERDR_FIXTURE_MODE": mode,
            "HERDR_FIXTURE_EVENTS": str(self.events),
            "HERDR_FIXTURE_STATE": str(self.state),
            "HERDR_FIXTURE_INSPECTIONS": str(self.inspections),
            "HERDR_FIXTURE_BUNDLE_COUNT": str(self.bundle_count),
            "HERDR_FIXTURE_SERVER_READY": str(self.server_ready),
            "HERDR_FIXTURE_SERVER_RETIRED": str(self.server_retired),
            "HERDR_FIXTURE_ALLOW_RETIRE": str(self.allow_retire),
            "HERDR_FIXTURE_SOCKET": str(self.socket),
            "HERDR_FIXTURE_SESSION": str(self.session_file),
            "HERDR_RELAY_TOKEN": FIXTURE_TOKEN,
            "HERDR_RELAY_INSTANCE_ID": FIXTURE_INSTANCE,
        }
        self.process = None
        self.outputs = (b"", b"")

    def launch(self, capture_pipes=False):
        stdout = subprocess.PIPE if capture_pipes else (self.root / "launcher.stdout").open("wb")
        stderr = subprocess.PIPE if capture_pipes else (self.root / "launcher.stderr").open("wb")
        self.process = subprocess.Popen(
            ["/bin/bash", str(self.scripts / "tailscale.sh"), "--confirm-serve"],
            cwd=self.root, env=self.env, stdin=subprocess.DEVNULL,
            stdout=stdout, stderr=stderr, close_fds=True, start_new_session=True,
        )
        if not capture_pipes:
            stdout.close()
            stderr.close()
        return self.process

    def output_files(self):
        output = (self.root / "launcher.stdout").read_bytes() if (self.root / "launcher.stdout").exists() else b""
        error = (self.root / "launcher.stderr").read_bytes() if (self.root / "launcher.stderr").exists() else b""
        return output, error

    def communicate(self, timeout=15):
        try:
            out, err = self.process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            raise AssertionError("launcher did not close its stdout/stderr pipes within the bounded lifecycle deadline")
        if out is None and err is None:
            out, err = self.output_files()
        self.outputs = (out or b"", err or b"")
        return self.process.returncode, self.outputs[0], self.outputs[1]

    def wait_for(self, predicate, message, timeout=8):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            if self.process is not None and self.process.poll() is not None:
                break
            time.sleep(0.02)
        raise AssertionError(message)

    def wait_for_link(self, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            out, err = self.output_files() if self.process.stdout is None else self.read_pipe_output()
            has_link_banner = (b"Open this private setup link" in out or
                                b"Or open this private setup link" in out)
            if has_link_banner and FIXTURE_TOKEN.encode() in out:
                self.outputs = (out, err)
                return out, err
            if self.process.poll() is not None:
                self.outputs = (out, err)
                return out, err
            time.sleep(0.02)
        out, err = self.output_files() if self.process.stdout is None else self.read_pipe_output()
        link_origins = [line.split(b"/#", 1)[0].strip() for line in out.splitlines() if FIXTURE_TOKEN.encode() in line]
        raise AssertionError(
            "launcher did not reach the private setup-link boundary; "
            f"banner={(b'Open this private setup link' in out or b'Or open this private setup link' in out)}, "
            f"link_origins={link_origins!r}, "
            f"events={self.event_names()!r}, stderr_bytes={len(err)}"
        )

    def read_pipe_output(self):
        output = getattr(self, "_pipe_output", bytearray())
        error = getattr(self, "_pipe_error", bytearray())
        self._pipe_output = output
        self._pipe_error = error
        for stream, target in ((self.process.stdout, output), (self.process.stderr, error)):
            if stream is None:
                continue
            ready, _, _ = select.select([stream], [], [], 0)
            if ready:
                chunk = os.read(stream.fileno(), 65536)
                if chunk:
                    target.extend(chunk)
        return bytes(output), bytes(error)

    def events_list(self):
        if not self.events.exists():
            return []
        values = []
        for line in self.events.read_text().splitlines():
            try:
                values.append(json.loads(line))
            except ValueError:
                continue
        return values

    def event_names(self):
        return [entry["event"] for entry in self.events_list()]

    def recover_retire(self):
        if not self.socket.exists():
            return
        self.allow_retire.write_text("allow")
        values = dict(line.split("=", 1) for line in self.session_file.read_text().splitlines() if "=" in line)
        request_env = self.env.copy()
        request_env["HERDR_RELAY_RUN_ID"] = values["HERDR_RELAY_RUN_ID"]
        request_env["HERDR_RELAY_PAIRING_SOCKET"] = values["HERDR_RELAY_PAIRING_SOCKET"]
        try:
            result = subprocess.run(
                [str(self.bin / "herdr-mobile-relay"), "pairing-control",
                 "--socket", values["HERDR_RELAY_PAIRING_SOCKET"],
                 "--operation", "retire", "--run-id", values["HERDR_RELAY_RUN_ID"],
                 "--instance", FIXTURE_INSTANCE],
                cwd=self.root, env=request_env, stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            raise AssertionError("test-owned fake owner recovery request failed") from None
        if result.returncode != 0:
            raise AssertionError("test-owned fake owner recovery request failed")
        self.wait_for(lambda: self.server_retired.exists(), "test-owned fake owner did not acknowledge cleanup", timeout=5)

    def close(self):
        if self.process is not None and self.process.poll() is None:
            self.process.send_signal(signal.SIGTERM)
            try:
                self.process.communicate(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.communicate(timeout=2)
        if self.socket.exists():
            try:
                self.recover_retire()
            except Exception:
                pass
        shutil.rmtree(self.root, ignore_errors=True)


class ManagedLauncherLifecycle(unittest.TestCase):
    def setUp(self):
        self.fixtures = []

    def fixture(self, mode="success"):
        fixture = Fixture(mode)
        self.fixtures.append(fixture)
        return fixture

    def tearDown(self):
        for fixture in reversed(self.fixtures):
            fixture.close()

    def assert_contains(self, output, fragment, message):
        self.assertTrue(fragment in output, message)

    def assert_not_contains(self, output, fragment, message):
        self.assertTrue(fragment not in output, message)

    def assert_exact_file(self, path, expected, message):
        self.assertTrue(path.read_bytes() == expected, message)

    def assert_no_link(self, output):
        self.assert_not_contains(output, b"Scan this QR code", "QR output appeared before the launcher committed")
        self.assert_not_contains(output, b"Open this private setup link", "setup link appeared before the launcher committed")
        self.assert_not_contains(output, b"Or open this private setup link", "setup link appeared before the launcher committed")
        self.assert_not_contains(output, FIXTURE_TOKEN.encode(), "fixture credential leaked outside the setup-link boundary")
        self.assert_not_contains(output, FIXTURE_RUN_ID.encode(), "fixture session identifier leaked outside the private recovery record")

    def assert_private_rollback_snapshot(self, fixture):
        snapshots = list(fixture.config.glob(".tailscale-rollback.*"))
        self.assertEqual(len(snapshots), 1, "private rollback snapshot was not retained")
        self.assertEqual(snapshots[0].stat().st_mode & 0o777, 0o700)
        self.assert_exact_file(snapshots[0] / "original-env", fixture.original_env, "private rollback evidence lost the original relay.env")
        self.assertEqual((snapshots[0] / "original-env").stat().st_mode & 0o777, 0o640)
        self.assert_exact_file(snapshots[0] / "original-origin", fixture.original_origin, "private rollback evidence lost the original phone-app origin")
        self.assertEqual((snapshots[0] / "original-origin").stat().st_mode & 0o777, 0o640)

    def assert_private_recovery(self, fixture, stage):
        self.assertTrue(fixture.session_file.exists(), "recovery journal was not retained")
        self.assertEqual(fixture.session_file.stat().st_mode & 0o777, 0o600)
        self.assertTrue(("HERDR_RELAY_STAGE=" + stage).encode() in fixture.session_file.read_bytes(), "recovery journal stage did not match the dispatched operation")
        self.assertTrue(fixture.socket.exists(), "authenticated recovery control socket was not retained")
        self.assertEqual(fixture.socket.stat().st_mode & 0o777, 0o600)

    def assert_private_logs_redacted(self, fixture):
        for path in fixture.config.glob(".tailscale-relay-log.*"):
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            contents = path.read_bytes()
            self.assert_not_contains(contents, FIXTURE_TOKEN.encode(), "fixture credential leaked into private startup log")
            self.assert_not_contains(contents, FIXTURE_FRAGMENT.encode(), "setup fragment leaked into private startup log")
            self.assert_not_contains(contents, FIXTURE_RUN_ID.encode(), "session identifier leaked into private startup log")

    def assert_no_tailscale_writes(self, fixture):
        names = fixture.event_names()
        self.assertNotIn("tailscale-mutation", names)
        first_inspection_refusals = (
            "preexisting-route", "preexisting-funnel", "preexisting-multiple",
            "incomplete-inspection", "missing-exposure",
        )
        expected_inspections = 1 if fixture.mode in first_inspection_refusals else 2
        self.assertEqual(names.count("inspect"), expected_inspections)
        event_data = fixture.events.read_bytes() if fixture.events.exists() else b""
        self.assert_not_contains(event_data, FIXTURE_TOKEN.encode(), "fixture credential entered fake status/event evidence")
        self.assert_not_contains(event_data, FIXTURE_FRAGMENT.encode(), "setup fragment entered fake status/event evidence")
        self.assert_not_contains(event_data, FIXTURE_RUN_ID.encode(), "session identifier entered fake status/event evidence")

    def test_existing_or_incomplete_route_refuses_before_relay_start(self):
        for mode, expected in (("preexisting-route", b"Existing Tailscale Serve/Funnel configuration"),
                               ("preexisting-funnel", b"Existing Tailscale Serve/Funnel configuration"),
                               ("preexisting-multiple", b"Existing Tailscale Serve/Funnel configuration"),
                               ("incomplete-inspection", b"Tailscale exposure inspection is incomplete"),
                               ("missing-exposure", b"Tailscale exposure inspection is incomplete")):
            with self.subTest(mode=mode):
                fixture = self.fixture(mode)
                fixture.launch()
                code, output, error = fixture.communicate()
                self.assertNotEqual(code, 0)
                self.assert_contains(output + error, expected, "launcher refusal did not identify the preflight boundary")
                self.assert_no_link(output + error)
                self.assertFalse(fixture.server_ready.exists())
                self.assertFalse(fixture.session_file.exists())
                self.assert_exact_file(fixture.env_file, fixture.original_env, "relay.env changed before owner readiness")
                self.assert_exact_file(fixture.origin_file, fixture.original_origin, "phone-app origin changed before owner readiness")
                self.assert_no_tailscale_writes(fixture)

    def test_transport_mode_precedence_refuses_without_entering_tailscale(self):
        fixture = self.fixture()
        fixture.env["HERDR_TAILSCALE_REQUEST"] = "0"
        fixture.launch()
        code, output, error = fixture.communicate()
        self.assertNotEqual(code, 0)
        self.assert_no_link(output + error)
        self.assertFalse(fixture.server_ready.exists())
        self.assertEqual(fixture.event_names().count("inspect"), 0)
        self.assert_exact_file(fixture.env_file, fixture.original_env, "mode-selection refusal changed relay.env")

    def test_second_inspection_after_consent_refuses_changed_route(self):
        fixture = self.fixture("second-inspection-route")
        fixture.launch()
        code, output, error = fixture.communicate()
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"Existing Tailscale Serve/Funnel configuration", "second inspection did not refuse the changed route")
        self.assertFalse(fixture.server_ready.exists())
        self.assertFalse(fixture.session_file.exists())
        self.assert_exact_file(fixture.env_file, fixture.original_env, "second-inspection refusal changed relay.env")
        self.assert_no_link(output + error)
        self.assert_no_tailscale_writes(fixture)

    def test_positive_launch_orders_owner_route_tls_bundle_arm_and_cleanup(self):
        fixture = self.fixture()
        fixture.launch(capture_pipes=True)
        output, error = fixture.wait_for_link()
        events = fixture.events_list()
        names = [entry["event"] for entry in events]
        required = ["server-ready", "status-local-ready", "activate", "route-check",
                    "health", "binary-version", "bundle-verify", "arm", "status-ready",
                    "route-check", "health", "bundle-verify", "qr-render"]
        index = -1
        for name in required:
            index = names.index(name, index + 1)
        bundle_indices = [index for index, name in enumerate(names) if name == "bundle-verify"]
        binary_version_indices = [index for index, name in enumerate(names) if name == "binary-version"]
        self.assertGreaterEqual(len(bundle_indices), 2)
        self.assertLess(names.index("arm"), bundle_indices[-1])
        self.assertLess(bundle_indices[-1], binary_version_indices[-1])
        self.assertLess(binary_version_indices[-1], names.index("qr-render"))
        activation = next(entry for entry in events if entry["event"] == "activate")
        self.assertEqual(activation["env_sha256"], hashlib.sha256(fixture.original_env).hexdigest())
        self.assertEqual(activation["session_stage"], "activation-pending")
        arm = next(entry for entry in events if entry["event"] == "arm")
        self.assertNotEqual(arm["env_sha256"], activation["env_sha256"])
        self.assertEqual(arm["session_stage"], "arm-pending")
        self.assertTrue(
            b"Open this private setup link" in output or b"Or open this private setup link" in output,
            "committed launcher did not print its private-link banner",
        )
        link_line = next(line for line in output.splitlines() if FIXTURE_TOKEN.encode() in line)
        link_origin = link_line.split(b"/#", 1)[0].strip()
        self.assertTrue(link_origin == FIXTURE_APP.encode(), f"setup link did not use the explicit fixture app origin: {link_origin!r}")
        self.assertTrue(FIXTURE_FRAGMENT.encode() in link_line, "setup link omitted the expected fixture fragment")
        non_link_output = b"\n".join(line for line in output.splitlines() if FIXTURE_TOKEN.encode() not in line)
        self.assert_not_contains(error, FIXTURE_TOKEN.encode(), "fixture credential leaked into diagnostics")
        self.assert_not_contains(error, FIXTURE_FRAGMENT.encode(), "setup fragment leaked into diagnostics")
        self.assert_not_contains(non_link_output + error, FIXTURE_FRAGMENT.encode(), "setup fragment appeared outside the private link")
        self.assert_not_contains(output.lower() + error.lower(), b"session_id", "session identifier appeared in terminal output")
        self.assert_not_contains(non_link_output + error, FIXTURE_RUN_ID.encode(), "session identifier appeared outside the private recovery record")
        self.assert_no_tailscale_writes(fixture)
        fixture.process.send_signal(signal.SIGTERM)
        code, final_out, final_err = fixture.communicate()
        self.assertEqual(code, 130)
        self.assertTrue(fixture.server_retired.exists())
        self.assertFalse(fixture.session_file.exists())
        self.assertFalse(list(fixture.config.glob(".tailscale-*-log.*")))
        self.assertFalse(list(fixture.config.glob(".tailscale-rollback.*")))
        self.assertEqual(fixture.event_names().count("retire"), 1)
        self.assertIn("server-exit", fixture.event_names())
        self.assert_not_contains(final_err, FIXTURE_TOKEN.encode(), "fixture credential leaked during retirement")
        self.assert_not_contains(final_err, FIXTURE_FRAGMENT.encode(), "setup fragment leaked during retirement")
        self.assert_not_contains(final_out.lower() + final_err.lower(), b"session_id", "session identifier appeared during retirement")
        final_non_link_output = b"\n".join(line for line in final_out.splitlines() if FIXTURE_TOKEN.encode() not in line)
        self.assert_not_contains(final_non_link_output + final_err, FIXTURE_RUN_ID.encode(), "session identifier appeared during retirement")

    def test_decoded_activation_no_write_retires_and_restores_exact_prior_files(self):
        fixture = self.fixture("activation-negative")
        fixture.launch()
        code, output, error = fixture.communicate()
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"decoded settled-no-write result", "activation refusal was not classified as a decoded negative reply")
        self.assert_no_link(output + error)
        self.assert_exact_file(fixture.env_file, fixture.original_env, "activation no-write did not restore relay.env byte-for-byte")
        self.assertEqual(fixture.env_file.stat().st_mode & 0o777, 0o640)
        self.assert_exact_file(fixture.origin_file, fixture.original_origin, "activation no-write did not restore phone-app origin byte-for-byte")
        self.assertEqual(fixture.origin_file.stat().st_mode & 0o777, 0o640)
        self.assertFalse(fixture.session_file.exists())
        self.assertTrue(fixture.server_retired.exists())
        self.assertNotIn("arm", fixture.event_names())
        self.assert_no_tailscale_writes(fixture)

    def test_decoded_arm_refusals_are_not_lost_ack_and_restore_exact_prior_files(self):
        for mode, outcome in (("arm-negative", "not-committed"),
                              ("arm-committed-negative", "committed")):
            with self.subTest(mode=mode):
                fixture = self.fixture(mode)
                fixture.launch()
                code, output, error = fixture.communicate()
                self.assertNotEqual(code, 0)
                self.assert_contains(output + error, ("decoded " + outcome + " result").encode(), "arm refusal was not classified as a decoded negative reply")
                self.assert_no_link(output + error)
                self.assert_exact_file(fixture.env_file, fixture.original_env, "arm refusal did not restore relay.env byte-for-byte")
                self.assertEqual(fixture.env_file.stat().st_mode & 0o777, 0o640)
                self.assert_exact_file(fixture.origin_file, fixture.original_origin, "arm refusal did not restore phone-app origin byte-for-byte")
                self.assertEqual(fixture.origin_file.stat().st_mode & 0o777, 0o640)
                self.assertFalse(fixture.session_file.exists())
                self.assertTrue(fixture.server_retired.exists())
                self.assert_no_tailscale_writes(fixture)

    def test_final_phone_bundle_failure_prevents_link_and_restores_selection(self):
        fixture = self.fixture("final-bundle-failure")
        fixture.launch()
        code, output, error = fixture.communicate()
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"Final phone-app bundle verification failed", "final bundle verification failure was not reported")
        self.assert_no_link(output + error)
        names = fixture.event_names()
        first_bundle = names.index("bundle-verify")
        final_bundle = names.index("bundle-verify", first_bundle + 1)
        self.assertTrue(names.index("arm") < final_bundle, "final bundle verification did not follow the acknowledged invitation arm")
        self.assert_exact_file(fixture.env_file, fixture.original_env, "final bundle failure did not restore relay.env byte-for-byte")
        self.assert_exact_file(fixture.origin_file, fixture.original_origin, "final bundle failure did not restore phone-app origin byte-for-byte")
        self.assertFalse(fixture.session_file.exists())
        self.assertTrue(fixture.server_retired.exists())
        self.assertEqual(fixture.event_names().count("bundle-verify"), 2)
        self.assert_no_tailscale_writes(fixture)

    def test_lost_activation_ack_retains_live_owner_and_closes_launcher_pipes(self):
        fixture = self.fixture("activation-lost-ack")
        fixture.launch(capture_pipes=True)
        code, output, error = fixture.communicate(timeout=8)
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"Tailscale activation acknowledgement was lost", "lost activation acknowledgement was not retained")
        self.assert_private_recovery(fixture, "activation-pending")
        self.assertFalse(fixture.server_retired.exists())
        self.assert_exact_file(fixture.env_file, fixture.original_env, "lost activation acknowledgement mutated relay.env")
        self.assert_no_link(output + error)
        self.assert_private_logs_redacted(fixture)
        self.assertFalse(list(fixture.config.glob(".tailscale-rollback.*")), "activation ambiguity retained a selection snapshot before any selection write")
        self.assert_no_tailscale_writes(fixture)
        fixture.recover_retire()

    def test_unresolved_decoded_write_retains_recovery_without_exposing_link(self):
        fixture = self.fixture("activation-unresolved")
        fixture.launch(capture_pipes=True)
        code, output, error = fixture.communicate(timeout=8)
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"activation outcome is unresolved", "unresolved activation outcome was not retained")
        self.assert_private_recovery(fixture, "activation-pending")
        self.assertFalse(fixture.server_retired.exists())
        self.assert_no_link(output + error)
        self.assert_not_contains(error, FIXTURE_TOKEN.encode(), "fixture credential leaked into unresolved-outcome diagnostics")
        self.assert_not_contains(error, FIXTURE_FRAGMENT.encode(), "setup fragment leaked into unresolved-outcome diagnostics")
        self.assert_private_logs_redacted(fixture)
        self.assert_no_tailscale_writes(fixture)
        fixture.recover_retire()

    def test_lost_arm_ack_keeps_owner_and_selection_and_closes_launcher_pipes(self):
        fixture = self.fixture("arm-lost-ack")
        fixture.launch(capture_pipes=True)
        code, output, error = fixture.communicate(timeout=8)
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"Bootstrap invitation acknowledgement was lost", "lost arm acknowledgement was not retained")
        self.assert_private_recovery(fixture, "arm-pending")
        self.assertFalse(fixture.server_retired.exists())
        self.assertTrue(fixture.env_file.read_bytes() != fixture.original_env, "unresolved arm unexpectedly rolled back the selected transport")
        self.assertTrue(b"HERDR_RELAY_TRANSPORT='tailscale'" in fixture.env_file.read_bytes(), "unresolved arm did not preserve Tailscale selection")
        self.assert_no_link(output + error)
        self.assert_not_contains(error, FIXTURE_TOKEN.encode(), "fixture credential leaked into lost-ack diagnostics")
        self.assert_not_contains(error, FIXTURE_FRAGMENT.encode(), "setup fragment leaked into lost-ack diagnostics")
        self.assert_private_logs_redacted(fixture)
        self.assert_private_rollback_snapshot(fixture)
        self.assert_not_contains(output.lower() + error.lower(), b"session_id", "session identifier appeared in lost-ack diagnostics")
        self.assert_no_tailscale_writes(fixture)
        fixture.recover_retire()

    def test_concurrent_selection_edit_is_not_overwritten_by_rollback(self):
        fixture = self.fixture("arm-concurrent-edit")
        fixture.launch()
        code, output, error = fixture.communicate()
        self.assertNotEqual(code, 0)
        self.assert_contains(error, b"Selection rollback was refused", "concurrent selection edit was not detected")
        current = fixture.env_file.read_bytes()
        self.assertTrue(b"concurrent-edit" in current, "rollback overwrote an unrelated concurrent relay.env edit")
        self.assertTrue(current != fixture.original_env, "concurrent selection edit disappeared")
        self.assertTrue(fixture.server_retired.exists(), "known no-write arm refusal did not retire the fake owner")
        self.assertFalse(fixture.session_file.exists(), "successful retirement retained the foreground journal")
        snapshots = list(fixture.config.glob(".tailscale-rollback.*"))
        self.assertEqual(len(snapshots), 1)
        self.assert_exact_file(snapshots[0] / "original-env", fixture.original_env, "private rollback evidence lost the original relay.env")
        self.assert_no_link(output + error)
        self.assert_no_tailscale_writes(fixture)

    def test_unresolved_arm_refusal_keeps_selection_and_recovery(self):
        fixture = self.fixture("arm-unresolved")
        fixture.launch(capture_pipes=True)
        code, output, error = fixture.communicate(timeout=8)
        self.assertNotEqual(code, 0)
        self.assert_contains(output + error, b"Bootstrap invitation outcome is unresolved", "unresolved arm reply was not retained")
        self.assert_private_recovery(fixture, "arm-pending")
        self.assertFalse(fixture.server_retired.exists())
        self.assertTrue(fixture.env_file.read_bytes() != fixture.original_env, "unresolved arm unexpectedly rolled back selected transport")
        self.assert_no_link(output + error)
        self.assert_not_contains(error, FIXTURE_TOKEN.encode(), "fixture credential leaked into unresolved arm diagnostics")
        self.assert_private_logs_redacted(fixture)
        self.assert_private_rollback_snapshot(fixture)
        self.assert_no_tailscale_writes(fixture)
        fixture.recover_retire()

    def test_retirement_requires_both_route_and_local_watch_acknowledgements(self):
        fixture = self.fixture("retire-refusal")
        fixture.launch(capture_pipes=True)
        output, error = fixture.wait_for_link()
        fixture.process.send_signal(signal.SIGTERM)
        code, final_out, final_err = fixture.communicate(timeout=15)
        self.assertNotEqual(code, 0)
        self.assert_contains(final_out + final_err, b"did not separately acknowledge route clear and local watch closure", "cleanup did not require both retirement acknowledgements")
        self.assert_private_recovery(fixture, "ready")
        self.assertTrue(b"HERDR_RELAY_TRANSPORT='tailscale'" in fixture.env_file.read_bytes())
        self.assertTrue(list(fixture.config.glob(".tailscale-relay-log.*")))
        self.assert_private_logs_redacted(fixture)
        self.assert_private_rollback_snapshot(fixture)
        self.assertFalse(fixture.server_retired.exists())
        self.assertEqual(fixture.event_names().count("retire"), 3)
        self.assert_no_tailscale_writes(fixture)
        fixture.recover_retire()


if __name__ == "__main__":
    unittest.main(verbosity=2)

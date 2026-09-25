#!/usr/bin/env python3
"""Disposable extracted-release managed Tailscale/HTTPS browser acceptance.

This gate is intentionally Docker-only: the package launcher and binary run
inside a network-disabled Linux container, against the fixed production
LocalAPI Unix socket and a strict v1.102.4 protocol fixture. The only browser
is an ordinary Chromium browser with a CA trusted only in that disposable
container. It must never be enabled against a developer host.
"""
from __future__ import annotations

import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socket
import socketserver
import stat
import ssl
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.parse

HOST = "relay.tailnet.ts.net"
TS_VERSION = "1.102.4-tbbcd7d1fc"
TS_COMMIT = "bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8"
WATCH_ID = "package-watch-session"
TOKEN_BYTES = 16
STAGES = {
    "archive_verify", "archive_checksum", "archive_extract", "archive_manifest",
    "archive_binary_verify", "archive_payload", "fixture_startup", "managed_launch", "browser_enroll",
    "setup_reprint", "browser_reprint", "managed_retirement", "managed_restart",
    "browser_restart", "foreign_route_rejection", "held_pipe_cleanup",
    "ambiguous_localapi_ack", "complete",
}
FAILURE_CODES = {
    "fixture_assertion", "unexpected_exception", "archive_checksum_io",
    "archive_checksum_mismatch", "archive_extract_failure", "archive_unsafe_entry",
    "archive_binary_missing", "archive_manifest_missing",
    "archive_manifest_invalid", "archive_manifest_identity", "archive_binary_start",
    "archive_binary_rejected", "archive_managed_wrapper_missing",
    "archive_external_wrapper_missing", "archive_managed_wrapper_not_executable",
    "archive_external_wrapper_not_executable", "archive_binary_not_executable",
    "managed_launcher_spawn", "managed_launcher_output_limit",
    "managed_launcher_exit_before_link", "managed_launcher_link_timeout",
    "fixture_localapi_watch_missing", "fixture_localapi_registration_missing",
    "fixture_localapi_session_missing",
}
BROWSER_STAGES = {
    "browser_runner", "controller_enrollment", "controller_inventory",
    "controller_command", "reader_invitation", "reader_enrollment",
    "reader_read_only", "credential_preservation", "browser_complete",
}
EXPECTED_CASES = [
    "archive_checksum_and_exact_release_identity",
    "managed_and_byo_wrappers_are_extracted_unchanged",
    "fixed_localapi_mask_etag_registration_and_selective_delete",
    "launcher_generated_setup_link_enrolls_real_controller_profile",
    "controller_reads_fake_inventory_and_sends_harmless_command",
    "second_persistent_profile_enrolls_reader_and_read_only_is_enforced",
    "reprint_and_managed_restart_preserve_enrolled_device_credentials",
    "foreign_route_is_preserved_and_no_setup_link_is_emitted",
    "held_pipe_child_exits_with_bounded_launcher_cleanup",
    "ambiguous_committed_write_retains_private_recovery_without_replay_or_url",
]


class GateFailure(RuntimeError):
    def __init__(self, code: str):
        self.code = code if code in FAILURE_CODES else "fixture_assertion"
        super().__init__(self.code)


def die(message: str, code: str = "fixture_assertion") -> "NoReturn":
    del message  # diagnostics remain code-only in stdout/stderr and artifacts
    raise GateFailure(code)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, sort_keys=True) + "\n", encoding="utf-8")
    temporary.replace(path)


class APIState:
    def __init__(self, path: Path):
        self.path = path
        self.lock = threading.RLock()
        self.config: dict = {}
        self.etag = hashlib.sha256(b"{}").hexdigest()
        self.events: list[str] = []
        self.foreign_after_registration = False
        self.drop_registration_ack = False
        self.foreign_name = "independent-operator-session"
        self.watch_closed = threading.Event()

    def event(self, value: str) -> None:
        with self.lock:
            self.events.append(value)
            self.persist()

    def persist(self) -> None:
        write_json(self.path, {
            "config": self.config,
            "events": self.events,
            "etag": self.etag,
            "watch_closed": self.watch_closed.is_set(),
        })


class UnixHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    address_family = socket.AF_UNIX
    daemon_threads = True
    allow_reuse_address = False

    def server_bind(self) -> None:
        self.socket.bind(self.server_address)
        self.server_name = "local-tailscaled.sock"
        self.server_port = 80


class LocalAPIHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = ""
    sys_version = ""

    @property
    def fixture(self) -> APIState:
        return self.server.fixture  # type: ignore[attr-defined]

    def log_message(self, _format: str, *_args: object) -> None:
        return

    def _headers(self, status: int, body: bytes, content_type: str = "application/json") -> None:
        self.send_response(status)
        self.send_header("Tailscale-Version", TS_VERSION)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        if body:
            self.wfile.write(body)
        self.close_connection = True

    def do_GET(self) -> None:
        fixture = self.fixture
        if self.path == "/localapi/v0/watch-ipn-bus?mask=2":
            # Only watch registration uses the fixture lock; the response body
            # remains open independently while status/config operations proceed.
            fixture.event("localapi:watch:mask=2")
            self.send_response(200)
            self.send_header("Tailscale-Version", TS_VERSION)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            try:
                self.wfile.write((json.dumps({"Version": TS_VERSION, "SessionID": WATCH_ID}) + "\\n").encode())
                self.wfile.flush()
                while not fixture.watch_closed.wait(0.1):
                    self.wfile.write((json.dumps({"Version": TS_VERSION}) + "\\n").encode())
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass
            finally:
                fixture.event("localapi:watch:closed")
            return
        with fixture.lock:
            if self.path == "/localapi/v0/status":
                fixture.events.append("localapi:status")
                fixture.persist()
                body = json.dumps({
                    "BackendState": "Running",
                    "Version": TS_VERSION,
                    "Self": {
                        "ID": "package-fixture-node",
                        "UserID": 41,
                        "DNSName": HOST + ".",
                        "CapMap": {"https": None},
                    },
                    "CurrentTailnet": {
                        "Name": "Disposable package test",
                        "MagicDNSSuffix": "tailnet.ts.net",
                        "MagicDNSEnabled": True,
                    },
                    "CertDomains": [HOST],
                    "User": {"41": {
                        "ID": 41,
                        "LoginName": "fixture@example.invalid",
                        "DisplayName": "Disposable fixture",
                        "ProfilePicURL": "",
                    }},
                }, separators=(",", ":")).encode()
                self._headers(200, body)
                return
            if self.path == "/localapi/v0/serve-config":
                fixture.events.append("localapi:config:get")
                fixture.persist()
                body = json.dumps(fixture.config, separators=(",", ":")).encode()
                self.send_response(200)
                self.send_header("Tailscale-Version", TS_VERSION)
                self.send_header("Content-Type", "application/json")
                self.send_header("ETag", fixture.etag)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)
                self.close_connection = True
                return
        fixture.event("localapi:unexpected")
        self._headers(404, b"{}")

    def do_POST(self) -> None:
        fixture = self.fixture
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        drop_response = False
        with fixture.lock:
            fixture.events.append("localapi:config:post")
            if self.path != "/localapi/v0/serve-config" or self.headers.get("If-Match") != fixture.etag:
                fixture.persist()
                self._headers(412, b"etag mismatch\n", "text/plain; charset=utf-8")
                return
            fixture.events.append("localapi:conditional:if-match")
            if self.headers.get("Content-Type") != "application/json":
                fixture.persist()
                self._headers(400, b"invalid content type", "text/plain; charset=utf-8")
                return
            try:
                next_config = json.loads(body)
            except (ValueError, UnicodeDecodeError):
                fixture.persist()
                self._headers(400, b"invalid JSON", "text/plain; charset=utf-8")
                return
            if not isinstance(next_config, dict):
                fixture.persist()
                self._headers(400, b"invalid Serve config", "text/plain; charset=utf-8")
                return
            foreground = next_config.get("Foreground")
            if isinstance(foreground, dict) and WATCH_ID in foreground:
                entry = foreground[WATCH_ID]
                expected_host = f"{HOST}:{os.environ['HERDR_TAILSCALE_HTTPS_PORT']}"
                expected_proxy = f"http://127.0.0.1:{os.environ['HERDR_RELAY_PORT']}"
                try:
                    valid_registration = (
                        entry["TCP"][os.environ["HERDR_TAILSCALE_HTTPS_PORT"]]["HTTPS"] is True
                        and entry["Web"][expected_host]["Handlers"]["/"]["Proxy"] == expected_proxy
                        and len(foreground) == 1
                    )
                except (KeyError, TypeError):
                    valid_registration = False
                if not valid_registration:
                    fixture.persist()
                    self._headers(400, b"invalid pinned foreground route", "text/plain; charset=utf-8")
                    return
                fixture.events.append("localapi:registration:foreground-exact")
                if fixture.foreign_after_registration:
                    foreground[fixture.foreign_name] = {
                    "TCP": {"443": {"HTTPS": True}},
                    "Web": {"operator.tailnet.ts.net:443": {"Handlers": {"/": {"Text": "operator route"}}}},
                }
            if WATCH_ID in (fixture.config.get("Foreground") or {}) and WATCH_ID not in (next_config.get("Foreground") or {}):
                fixture.events.append("localapi:retirement:selective-delete")
                if fixture.foreign_name in (fixture.config.get("Foreground") or {}) and fixture.foreign_name not in (next_config.get("Foreground") or {}):
                    fixture.persist()
                    self._headers(409, b"foreign session removal refused", "text/plain; charset=utf-8")
                    return
            fixture.config = next_config
            fixture.etag = hashlib.sha256(body).hexdigest()
            fixture.persist()
            if WATCH_ID in (next_config.get("Foreground") or {}) and fixture.drop_registration_ack:
                fixture.drop_registration_ack = False
                drop_response = True
        if drop_response:
            fixture.event("localapi:ack:dropped-after-commit")
            self.close_connection = True
            return
        self._headers(200, b"")


def status_document(config: dict) -> dict:
    routes: dict = {}
    foreground = config.get("Foreground", {}) if isinstance(config, dict) else {}
    if isinstance(foreground, dict):
        routes = foreground
    return {"Foreground": routes} if routes else {}


def fake_cli_program() -> str:
    # This is the test-only CLI shim, not a LocalAPI endpoint override. The
    # package's Go owner still dials /var/run/tailscale/tailscaled.sock.
    return r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
state = json.loads(Path(os.environ["HERDR_FIXTURE_API_STATE"]).read_text())
config = state.get("config", {})
status = {
  "Version":"1.102.4-tbbcd7d1fc", "BackendState":"Running",
  "Self":{"ID":"package-fixture-node","UserID":41,"DNSName":"relay.tailnet.ts.net.","CapMap":{"https":None}},
  "CurrentTailnet":{"Name":"Disposable package test","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":True},
  "CertDomains":["relay.tailnet.ts.net"],
  "User":{"41":{"ID":41,"LoginName":"fixture@example.invalid","DisplayName":"Disposable fixture","ProfilePicURL":""}}
}
version = {
  "majorMinorPatch":"1.102.4", "short":"1.102.4", "long":"1.102.4-tbbcd7d1fc",
  "gitCommit":"bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8",
  "daemonLong":"1.102.4-tbbcd7d1fc", "extraGitCommit":"", "osVariant":"", "cap":141,
  "isDev":False, "gitDirty":False, "unstableBranch":False
}
if args == ["status", "--json"]:
  print(json.dumps(status,separators=(",",":"))); raise SystemExit(0)
if args == ["version", "--json", "--daemon"]:
  print(json.dumps(version,separators=(",",":"))); raise SystemExit(0)
if args == ["serve", "status", "--json"]:
  print(json.dumps({"TCP":config.get("TCP",{}),"Web":config.get("Web",{}),"Services":config.get("Services",{}),"AllowFunnel":config.get("AllowFunnel",{}),"Foreground":config.get("Foreground",{})},separators=(",",":")))
  raise SystemExit(0)
# No CLI write operation is part of managed ownership; fail any attempt.
raise SystemExit(97)
'''


class PublicHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = ""
    sys_version = ""

    def log_message(self, _format: str, *_args: object) -> None:
        return

    def _websocket(self) -> None:
        backend = socket.create_connection(("127.0.0.1", int(os.environ["HERDR_RELAY_PORT"])), timeout=10)
        headers = [(key, value) for key, value in self.headers.items() if key.lower() != "host"]
        request = f"{self.command} {self.path} HTTP/1.1\r\nHost: {self.headers.get('Host', HOST)}\r\n"
        request += "".join(f"{key}: {value}\r\n" for key, value in headers) + "\r\n"
        backend.sendall(request.encode("latin-1"))
        response = bytearray()
        while b"\r\n\r\n" not in response:
            chunk = backend.recv(4096)
            if not chunk:
                raise ConnectionError("backend WebSocket closed before upgrade")
            response.extend(chunk)
            if len(response) > 64 * 1024:
                raise ConnectionError("backend WebSocket headers exceeded fixture bound")
        self.connection.sendall(response)

        def copy(source: socket.socket, destination: socket.socket) -> None:
            try:
                while True:
                    chunk = source.recv(65536)
                    if not chunk:
                        break
                    destination.sendall(chunk)
            except (OSError, ssl.SSLError):
                pass
            try:
                destination.shutdown(socket.SHUT_WR)
            except OSError:
                pass

        left = threading.Thread(target=copy, args=(self.connection, backend), daemon=True)
        left.start()
        copy(backend, self.connection)
        left.join(timeout=2)
        backend.close()

    def _proxy(self) -> None:
        connection = http.client.HTTPConnection("127.0.0.1", int(os.environ["HERDR_RELAY_PORT"]), timeout=15)
        request_headers = {key: value for key, value in self.headers.items() if key.lower() not in {"host", "connection", "content-length"}}
        request_headers["Host"] = self.headers.get("Host", HOST)
        body = self.rfile.read(int(self.headers.get("Content-Length", "0"))) if self.headers.get("Content-Length") else None
        connection.request(self.command, self.path, body=body, headers=request_headers)
        response = connection.getresponse()
        data = response.read()
        self.send_response(response.status, response.reason)
        for key, value in response.getheaders():
            if key.lower() not in {"connection", "transfer-encoding", "content-length", "keep-alive"}:
                self.send_header(key, value)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        if data:
            self.wfile.write(data)
        self.close_connection = True
        connection.close()

    def do_GET(self) -> None:
        if self.headers.get("Upgrade", "").lower() == "websocket":
            try:
                self._websocket()
            except (OSError, ssl.SSLError, ConnectionError):
                self.close_connection = True
            return
        try:
            self._proxy()
        except (OSError, http.client.HTTPException):
            self.close_connection = True

    def do_POST(self) -> None:
        self._proxy()


# http.client is intentionally imported after the fixture-only class declaration
# to make static reviewers see the narrow proxy dependency.
import http.client


def start_public_server(certificate: Path, key: Path) -> http.server.ThreadingHTTPServer:
    server = http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["HERDR_TAILSCALE_HTTPS_PORT"])), PublicHandler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(str(certificate), str(key))
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, name="fixture-tailnet-https", daemon=True)
    thread.start()
    return server


def make_certificates(root: Path) -> tuple[Path, Path, Path]:
    ca_key, ca_cert = root / "fixture-ca.key", root / "fixture-ca.crt"
    leaf_key, leaf_csr, leaf_cert, extensions = root / "leaf.key", root / "leaf.csr", root / "leaf.crt", root / "leaf.ext"
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
        "-subj", "/CN=Herdr disposable package fixture CA", "-keyout", str(ca_key), "-out", str(ca_cert),
        "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign",
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run([
        "openssl", "req", "-newkey", "rsa:2048", "-nodes", "-subj", f"/CN={HOST}",
        "-keyout", str(leaf_key), "-out", str(leaf_csr),
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    extensions.write_text(
        "subjectAltName=DNS:" + HOST + "\n"
        "basicConstraints=critical,CA:FALSE\n"
        "keyUsage=critical,digitalSignature,keyEncipherment\n"
        "extendedKeyUsage=serverAuth\n", encoding="ascii",
    )
    subprocess.run([
        "openssl", "x509", "-req", "-in", str(leaf_csr), "-CA", str(ca_cert), "-CAkey", str(ca_key),
        "-CAcreateserial", "-days", "2", "-extfile", str(extensions), "-out", str(leaf_cert),
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.chmod(ca_key, 0o600)
    os.chmod(leaf_key, 0o600)
    trusted = Path("/usr/local/share/ca-certificates/herdr-disposable-package-fixture.crt")
    shutil.copyfile(ca_cert, trusted)
    subprocess.run(["update-ca-certificates"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return ca_cert, leaf_cert, leaf_key


def safe_link_from_output(output: bytes, origin: str) -> str | None:
    # This value remains in memory and is sent only through a private pipe to
    # Playwright. It is never printed, put in a log, or included in evidence.
    prefix = (origin + "/#").encode()
    for line in output.splitlines():
        start = line.find(prefix)
        if start < 0:
            continue
        try:
            raw = re.split(r"[\s\x1b]", line[start:].decode("ascii", "strict"), maxsplit=1)[0]
            parsed = urllib.parse.urlsplit(raw)
            query = urllib.parse.parse_qs(parsed.fragment, strict_parsing=True)
            token = query.get("setup", [""])[0]
        except (IndexError, UnicodeError, ValueError):
            continue
        if (parsed.scheme == "https" and parsed.netloc == origin.removeprefix("https://")
                and re.fullmatch(r"[0-9a-f]{32}", token)):
            return raw
    return None


def launch_managed(package: Path, env: dict[str, str], timeout: float = 90, expect_link: bool = True) -> tuple[subprocess.Popen[bytes], str, bytes, bytes]:
    try:
        process = subprocess.Popen(
            ["/bin/bash", str(package / "relay" / "tailscale.sh"), "--confirm-serve"],
            cwd=package, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            start_new_session=True, close_fds=True,
        )
    except OSError:
        die("packaged managed launcher could not be started", "managed_launcher_spawn")
    assert process.stdout is not None and process.stderr is not None
    output = bytearray()
    state: dict[str, object] = {"link": None, "stderr_bytes": 0, "output_limit": False}
    lock = threading.Lock()
    done = [threading.Event(), threading.Event()]

    def drain(stream, index: int) -> None:
        try:
            while True:
                chunk = os.read(stream.fileno(), 65536)
                if not chunk:
                    return
                with lock:
                    if index == 0:
                        if state["link"] is None and not state["output_limit"]:
                            output.extend(chunk)
                            if len(output) > 2 * 1024 * 1024:
                                state["output_limit"] = True
                                output.clear()
                            else:
                                state["link"] = safe_link_from_output(bytes(output), env["HERDR_FIXTURE_ORIGIN"])
                    else:
                        state["stderr_bytes"] = int(state["stderr_bytes"]) + len(chunk)
        except OSError:
            return
        finally:
            done[index].set()

    readers = [
        threading.Thread(target=drain, args=(process.stdout, 0), daemon=True),
        threading.Thread(target=drain, args=(process.stderr, 1), daemon=True),
    ]
    for reader in readers:
        reader.start()
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        with lock:
            link = state["link"]
            output_limit = bool(state["output_limit"])
        if output_limit:
            try:
                os.killpg(process.pid, 9)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                pass
            die("launcher output exceeded the package acceptance bound", "managed_launcher_output_limit")
        if link:
            return process, str(link), bytes(output), b""
        if process.poll() is not None and all(event.wait(0.05) for event in done):
            with lock:
                captured = bytes(output)
                stderr_bytes = int(state["stderr_bytes"])
            if not expect_link:
                return process, "", captured, b""
            die(f"packaged managed launcher exited before setup-link emission (exit={process.returncode}, stderr_bytes={stderr_bytes})", "managed_launcher_exit_before_link")
        time.sleep(0.05)
    try:
        os.killpg(process.pid, 9)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=3)
    except subprocess.TimeoutExpired:
        pass
    for reader in readers:
        reader.join(timeout=2)
    with lock:
        stderr_bytes = int(state["stderr_bytes"])
    die(f"packaged managed launcher timed out (stderr_bytes={stderr_bytes})", "managed_launcher_link_timeout")


def launch_managed_rejection(package: Path, env: dict[str, str], timeout: float = 90) -> tuple[subprocess.Popen[bytes], bytes]:
    process, link, stdout, _ = launch_managed(package, env, timeout, expect_link=False)
    if link or process.poll() is None or process.returncode == 0:
        stop_launcher(process)
        die("managed launcher rejection was not bounded, nonzero, and link-free")
    return process, stdout


def stop_launcher(process: subprocess.Popen[bytes]) -> bool:
    if process.poll() is not None:
        return False
    try:
        os.killpg(process.pid, 2)
    except ProcessLookupError:
        return False
    try:
        process.wait(timeout=20)
        return process.returncode == 130
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, 9)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            pass
        return False


browser_stage = "browser_runner"
browser_exception_type = ""


def run_playwright(package_root: Path, input_record: dict, mode: str, evidence_path: Path) -> dict:
    global browser_stage, browser_exception_type
    script = Path("/workspace/tests/tailscale-package-browser.mjs")
    if not script.is_file():
        browser_stage = "browser_runner"
        browser_exception_type = "BrowserScriptMissing"
        die("browser acceptance script is missing from exact candidate checkout")
    browser_input = {**input_record, "mode": mode, "evidence_path": str(evidence_path)}
    try:
        completed = subprocess.run(
            ["node", str(script)], input=json.dumps(browser_input).encode(), stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, timeout=240, check=False,
            env={**os.environ, "PLAYWRIGHT_BROWSERS_PATH": "/ms-playwright"},
        )
    except subprocess.TimeoutExpired as error:
        browser_stage = "browser_runner"
        browser_exception_type = type(error).__name__
        die("persistent-profile browser acceptance exceeded its bounded runtime")
    # stdout is a bounded, sanitized JSON protocol, not Playwright's console.
    try:
        value = json.loads(completed.stdout)
    except (ValueError, UnicodeDecodeError):
        browser_stage = "browser_runner"
        browser_exception_type = "BrowserProtocolError"
        die("browser acceptance did not return its bounded evidence record")
    if not isinstance(value, dict) or value.get("mode") != mode:
        browser_stage = "browser_runner"
        browser_exception_type = "BrowserProtocolError"
        die("browser acceptance evidence mode mismatch")
    stage_value = value.get("stage")
    browser_stage = stage_value if isinstance(stage_value, str) and stage_value in BROWSER_STAGES else "browser_runner"
    error_value = value.get("exception_type", "")
    browser_exception_type = error_value if isinstance(error_value, str) and re.fullmatch(r"[A-Za-z][A-Za-z0-9]{0,47}", error_value) else ""
    if completed.returncode != 0 and not browser_exception_type:
        browser_exception_type = "BrowserAssertionError"
    return value


def verify_archive(archive: Path, checksums: Path, version: str, revision: str, release: Path, mark_stage, record_digest, record_wrapper) -> tuple[str, Path]:
    name = archive.name
    mark_stage("archive_checksum")
    try:
        lines = [line.split() for line in checksums.read_text(encoding="ascii").splitlines() if line.strip()]
        archive_digest = sha256(archive)
        record_digest(archive_digest)
    except (OSError, UnicodeError):
        die("release checksum input could not be read", "archive_checksum_io")
    matches = [entry for entry in lines if len(entry) == 2 and entry[1] == name]
    if len(matches) != 1 or matches[0][0] != archive_digest:
        die("release archive checksum did not match the exact release checksum file", "archive_checksum_mismatch")

    mark_stage("archive_extract")
    release.mkdir(mode=0o700, parents=True)
    try:
        with tarfile.open(archive, "r:gz") as bundle:
            members = bundle.getmembers()
            for member in members:
                path = Path(member.name)
                if path.is_absolute() or ".." in path.parts or member.issym() or member.islnk() or not (member.isfile() or member.isdir()):
                    die("release archive contained an unsafe path or entry type", "archive_unsafe_entry")
            bundle.extractall(release, members=members)
    except (OSError, tarfile.TarError, ValueError):
        die("release archive extraction failed", "archive_extract_failure")

    mark_stage("archive_payload")
    binary = release / "herdr-mobile-relay"
    wrappers = (
        (release / "relay" / "tailscale.sh", "managed"),
        (release / "relay" / "tailscale-external.sh", "external"),
    )
    for wrapper, kind in wrappers:
        present = wrapper.is_file()
        mode = stat.S_IMODE(wrapper.stat().st_mode) if present else None
        executable = present and os.access(wrapper, os.X_OK)
        record_wrapper(kind, present, oct(mode) if mode is not None else None, executable)
        if not present:
            die("release archive omitted a Tailscale launcher", f"archive_{kind}_wrapper_missing")
        if not executable:
            die("extracted Tailscale launcher was not executable", f"archive_{kind}_wrapper_not_executable")
    if not binary.is_file():
        die("release archive omitted its executable relay binary", "archive_binary_missing")
    if not os.access(binary, os.X_OK):
        die("extracted relay binary was not executable", "archive_binary_not_executable")

    mark_stage("archive_manifest")
    manifest = release / "release-manifest.json"
    if not manifest.is_file():
        die("release archive omitted release manifest", "archive_manifest_missing")
    try:
        manifest_data = json.loads(manifest.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, ValueError):
        die("release manifest could not be read", "archive_manifest_invalid")
    if manifest_data.get("version") != version or manifest_data.get("revision") != revision or manifest_data.get("target") != "linux/amd64":
        die("release manifest did not bind the tested package to the exact candidate", "archive_manifest_identity")

    mark_stage("archive_binary_verify")
    try:
        verified = subprocess.run([
            str(binary), "verify-release", "--target", "linux/amd64", "--version", version,
            "--revision", revision, str(release),
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
    except OSError:
        die("packaged relay verification could not be started", "archive_binary_start")
    if verified.returncode:
        die("packaged relay refused exact candidate manifest verification", "archive_binary_rejected")
    return archive_digest, binary


def main() -> int:
    if os.environ.get("HERDR_EXTRACTED_PACKAGE_BROWSER_CI") != "1":
        die("refusing package/browser lifecycle outside explicit disposable CI container")
    source_sha = os.environ.get("GITHUB_SHA", "")
    workflow_url = os.environ.get("GITHUB_RUN_URL", "")
    version = os.environ.get("HERDR_PACKAGE_VERSION", "")
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha) or not version or not workflow_url.startswith("https://github.com/"):
        die("exact source SHA, release version, or hosted workflow URL is missing")
    archive = Path("/artifact/herdr-mobile-relay_" + version + "_linux_amd64.tar.gz")
    checksums = Path("/artifact/checksums.txt")
    state: APIState | None = None
    local_server: UnixHTTPServer | None = None
    public_server: http.server.ThreadingHTTPServer | None = None
    launcher: subprocess.Popen[bytes] | None = None
    evidence_path = Path("/evidence/tailscale-extracted-package-browser.json")
    case_results: dict[str, str] = {}
    transitions: list[str] = []
    archive_digest = ""
    archive_wrappers: dict[str, dict] = {}
    binary_digest = ""
    binary_path = ""
    browser_evidence: dict = {}
    current_stage = "archive_verify"
    completed_stages: list[str] = []
    failure_type = ""
    failure_code = ""
    ambiguous_owner_pid: int | None = None

    def set_stage(value: str) -> None:
        nonlocal current_stage
        if value not in STAGES:
            raise RuntimeError("unknown sanitized acceptance stage")
        current_stage = value
        if not completed_stages or completed_stages[-1] != value:
            completed_stages.append(value)

    def record_archive_digest(value: str) -> None:
        nonlocal archive_digest
        archive_digest = value

    def record_archive_wrapper(kind: str, present: bool, mode: str | None, executable: bool) -> None:
        archive_wrappers[kind] = {"present": present, "mode": mode, "executable": executable}

    temporary_root = Path(tempfile.mkdtemp(prefix="herdr-package-browser-", dir="/tmp"))
    os.chmod(temporary_root, 0o700)
    origin = f"https://{HOST}:{os.environ['HERDR_TAILSCALE_HTTPS_PORT']}"
    try:
        set_stage("archive_verify")
        archive_digest, binary = verify_archive(
            archive, checksums, version, source_sha, temporary_root / "release", set_stage,
            record_archive_digest, record_archive_wrapper,
        )
        binary_digest = sha256(binary)
        binary_path = str(binary.resolve())
        case_results[EXPECTED_CASES[0]] = "pass"
        if not all((temporary_root / "release" / "relay" / wrapper).is_file() for wrapper in ("tailscale.sh", "tailscale-external.sh")):
            die("managed/BYO launcher wrappers were not extracted")
        case_results[EXPECTED_CASES[1]] = "pass"

        set_stage("fixture_startup")
        config_dir = temporary_root / "config"
        runtime = temporary_root / "runtime"
        cache = temporary_root / "cache"
        data = temporary_root / "data"
        home = temporary_root / "home"
        fixture_bin = temporary_root / "bin"
        for path in (config_dir, runtime, cache, data, home, fixture_bin):
            path.mkdir(mode=0o700, parents=True)
        env_file = config_dir / "relay.env"
        token = os.urandom(TOKEN_BYTES).hex()
        instance = "package-acceptance-instance"
        env_file.write_text(
            f"HERDR_RELAY_TOKEN={token}\nHERDR_RELAY_INSTANCE_ID={instance}\n"
            "HERDR_RELAY_TRANSPORT=tailscale\nHERDR_RELAY_HOST=127.0.0.1\n"
            "HERDR_RELAY_REARM_BOOTSTRAP=0\nHERDR_RELAY_PORT=18377\nHERDR_RELAY_PLUGIN_PORT=18378\n",
            encoding="ascii",
        )
        os.chmod(env_file, 0o600)
        api_state_file = temporary_root / "api-state.json"
        state = APIState(api_state_file)
        state.persist()
        socket_dir = Path("/var/run/tailscale")
        socket_dir.mkdir(mode=0o755, parents=True, exist_ok=True)
        socket_path = socket_dir / "tailscaled.sock"
        if socket_path.exists() or socket_path.is_symlink():
            die("fixed production LocalAPI socket path already exists in disposable runtime")
        local_server = UnixHTTPServer(str(socket_path), LocalAPIHandler)
        local_server.fixture = state  # type: ignore[attr-defined]
        local_server.daemon_threads = True
        threading.Thread(target=local_server.serve_forever, name="fixture-localapi", daemon=True).start()
        os.chmod(socket_path, 0o600)

        cli_script = fixture_bin / "tailscale"
        cli_script.write_text(fake_cli_program(), encoding="utf-8")
        cli_script.chmod(0o700)
        ca, leaf, leaf_key = make_certificates(temporary_root)
        public_server = start_public_server(leaf, leaf_key)
        fake_herdr = Path("/fixture-bin/fake-herdr")
        if not fake_herdr.is_file() or not os.access(fake_herdr, os.X_OK):
            die("test-only fake Herdr inventory executable was not mounted")
        scenario = temporary_root / "scenario.json"
        scenario.write_text(json.dumps({
            "panes": [{
                "pane_id": "workspace:agent", "agent": "codex", "name": "Package fixture",
                "agent_status": "idle", "workspace_id": "workspace", "tab_id": "tab",
                "cwd": "/tmp/herdr-package-fixture", "revision": 1,
            }],
            "workspaces": [{"workspace_id": "workspace", "label": "Package fixture"}],
            "tabs": [{"tab_id": "tab", "workspace_id": "workspace", "label": "Package fixture", "cwd": "/tmp/herdr-package-fixture"}],
            "content": {"workspace:agent": "Harmless package fixture output"},
        }), encoding="utf-8")
        operations = temporary_root / "fake-herdr-operations.jsonl"
        relay_env = os.environ.copy()
        relay_env.update({
            "HOME": str(home), "TMPDIR": str(temporary_root),
            "XDG_CONFIG_HOME": str(config_dir), "XDG_CACHE_HOME": str(cache),
            "XDG_DATA_HOME": str(data), "HERDR_RELAY_ENV": str(env_file),
            "HERDR_TAILSCALE_REQUEST": "1", "HERDR_TAILSCALE_BIN": str(cli_script),
            "HERDR_TAILSCALE_HTTPS_PORT": os.environ["HERDR_TAILSCALE_HTTPS_PORT"],
            "HERDR_BIN": str(fake_herdr), "HERDR_SOCKET_PATH": str(runtime / "herdr.sock"),
            "HERDR_RELEASE_ROOT": str(data / "releases"),
            "HERDR_RELAY_POLL_INTERVAL": "1",
            "FAKE_HERDR_SCENARIO": str(scenario), "FAKE_HERDR_OPERATIONS": str(operations),
            "HERDR_FIXTURE_API_STATE": str(api_state_file), "HERDR_FIXTURE_ORIGIN": origin,
            "CURL_CA_BUNDLE": str(ca), "SSL_CERT_FILE": str(ca),
            "NO_PROXY": "*", "no_proxy": "*", "GITHUB_SHA": source_sha,
        })
        relay_env.pop("HERDR_PHONE_APP_URL", None)
        relay_env.pop("HERDR_RELAY_BIN", None)
        relay_env.pop("HERDR_WEB_ROOT", None)
        package = temporary_root / "release"
        set_stage("managed_launch")
        launcher, link, _private_stdout, _ = launch_managed(package, relay_env)
        transitions.extend(["preflight:read-only", "watch:mask=2", "route:conditional-register", "launcher:setup-link"])
        if state.events.count("localapi:watch:mask=2") != 1:
            die("fixed production LocalAPI did not record the required watch mask", "fixture_localapi_watch_missing")
        if state.events.count("localapi:config:post") < 1:
            die("fixed production LocalAPI did not record conditional registration", "fixture_localapi_registration_missing")
        if WATCH_ID not in state.config.get("Foreground", {}):
            die("fixed production LocalAPI did not retain the exact session", "fixture_localapi_session_missing")
        browser_record = {"setup_url": link, "origin": origin, "profiles": str(temporary_root / "profiles"), "fake_herdr_operations": str(operations)}
        set_stage("browser_enroll")
        browser_evidence = run_playwright(package, browser_record, "enroll", evidence_path)
        for name in browser_evidence.get("passed_cases", []):
            if name in EXPECTED_CASES:
                case_results[name] = "pass"
        if browser_evidence.get("result") != "pass":
            die("real packaged frontend E2EE two-profile browser flow did not pass")
        transitions.extend(["browser:controller-enrolled", "browser:reader-enrolled", "serve:reprint-committed"])

        set_stage("setup_reprint")
        setup_reprint = subprocess.run(
            ["/bin/bash", str(package / "relay" / "setup-link.sh")], cwd=package, env=relay_env,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=45, check=False,
        )
        if setup_reprint.returncode != 0:
            die("unmodified extracted setup-link reprint did not complete")
        reprint_link = safe_link_from_output(setup_reprint.stdout, origin)
        if not reprint_link:
            die("unmodified extracted setup-link omitted its reprinted link")
        # The new one-use setup invitation must not replace either enrolled
        # credential; the browser profiles compare their private storage before
        # and after this reprint and the subsequent owner restart.
        set_stage("browser_reprint")
        reprint_result = run_playwright(package, {**browser_record, "setup_url": reprint_link}, "reprint", evidence_path)
        if reprint_result.get("result") != "pass":
            die("browser credential-preservation check failed after setup-link reprint")
        set_stage("managed_retirement")
        if not stop_launcher(launcher):
            die("managed packaged launcher did not acknowledge bounded Ctrl-C retirement")
        launcher = None
        transitions.extend(["retirement:route-cleared", "retirement:local-watch-closed"])
        if state.config.get("Foreground"):
            die("conditional managed retirement left its foreground route configured")
        if "localapi:watch:closed" not in state.events:
            time.sleep(0.25)
        if "localapi:watch:closed" not in state.events:
            die("LocalAPI fixture did not observe a closed local watch after retirement")

        # Relaunch the exact extracted launcher with unchanged private state,
        # then reconnect both independent persistent profiles using credentials
        # already enrolled by the production frontend.
        set_stage("managed_restart")
        launcher, restart_link, _restart_stdout, _ = launch_managed(package, relay_env)
        if not restart_link:
            die("packaged managed restart did not create a fresh launcher-generated invitation")
        set_stage("browser_restart")
        restarted = run_playwright(package, browser_record, "restart", evidence_path)
        if restarted.get("result") != "pass" or restarted.get("credentials_preserved") is not True:
            die("controller/reader credentials did not survive actual relay restart")
        case_results[EXPECTED_CASES[6]] = "pass"
        if not stop_launcher(launcher):
            die("restarted packaged launcher did not acknowledge bounded Ctrl-C retirement")
        launcher = None
        if state.config.get("Foreground"):
            die("second selective retirement left its foreground route configured")

        set_stage("foreign_route_rejection")
        state.foreign_after_registration = True
        state.persist()
        launcher, negative_stdout = launch_managed_rejection(package, relay_env)
        # Registration adds an independent full Serve entry in the same atomic
        # fixture write. The launcher must reject this foreign route and print
        # no setup fragment; cleanup may clear only its exact session.
        if (origin + "/#").encode() in negative_stdout or token.encode() in negative_stdout:
            die("foreign-route conflict unexpectedly emitted a setup fragment or token")
        if b"Open this private setup link" in negative_stdout or b"Or open this private setup link" in negative_stdout:
            die("foreign-route refusal leaked a setup invitation")
        foreground = state.config.get("Foreground", {})
        if state.foreign_name not in foreground:
            die("selective managed cleanup overwrote the independent foreign route")
        foreign_before = json.dumps(foreground[state.foreign_name], sort_keys=True)
        launcher = None
        transitions.extend(["foreign-route:preserved", "managed-route:selective-delete"])
        event_sequence = state.events
        if (event_sequence.count("localapi:watch:mask=2") < 3 or event_sequence.count("localapi:config:post") < 5
                or event_sequence.count("localapi:conditional:if-match") < 5
                or event_sequence.count("localapi:registration:foreground-exact") < 3
                or event_sequence.count("localapi:retirement:selective-delete") < 3):
            die("LocalAPI protocol fixture did not exercise numeric watch, conditional registration/deletion, restart, and reprint transitions")
        if json.dumps(state.config.get("Foreground", {}).get(state.foreign_name), sort_keys=True) != foreign_before:
            die("selective cleanup changed the foreign route bytes")
        case_results[EXPECTED_CASES[7]] = "pass"
        case_results[EXPECTED_CASES[2]] = "pass"

        # Force a real executable descendant to keep CLI stdout/stderr open on
        # a read-only inspection. The production Go adapter must honor its
        # bounded WaitDelay instead of waiting for that inherited pipe.
        held = fixture_bin / "tailscale-held-pipe"
        child_file = temporary_root / "held-child.pid"
        held.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = status ]; then (sleep 20) & echo $! > \"" + str(child_file) + "\"; fi\n"
            "exec \"" + str(cli_script) + "\" \"$@\"\n", encoding="utf-8",
        )
        held.chmod(0o700)
        held_env = dict(relay_env, HERDR_TAILSCALE_BIN=str(held))
        set_stage("held_pipe_cleanup")
        started = time.monotonic()
        inspection = subprocess.run(
            [str(binary), "tailscale", "inspect", "--binary", str(held), "--https-port", os.environ["HERDR_TAILSCALE_HTTPS_PORT"]],
            env=held_env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=8, check=False,
        )
        elapsed = time.monotonic() - started
        child_file = temporary_root / "held-child.pid"
        try:
            child_pid = int(child_file.read_text(encoding="ascii").strip())
            os.kill(child_pid, 15)
        except (OSError, ValueError):
            pass
        if elapsed > 7.0 or inspection.returncode != 0:
            die("held inherited child pipe was not bounded by the production CLI inspection")
        case_results[EXPECTED_CASES[8]] = "pass"

        # Isolate the lost-ACK case from both the successful E2EE journey and
        # the foreign route. The fixture commits the exact conditional POST,
        # closes the response before ACK, and then forbids activation replay or
        # global cleanup. Production must preserve the private recovery owner
        # and must not print a device setup URL.
        set_stage("ambiguous_localapi_ack")
        ambiguous_root = temporary_root / "ambiguous-ack"
        ambiguous_config = ambiguous_root / "config"
        ambiguous_runtime = ambiguous_root / "runtime"
        ambiguous_cache = ambiguous_root / "cache"
        ambiguous_data = ambiguous_root / "data"
        ambiguous_home = ambiguous_root / "home"
        for path in (ambiguous_config, ambiguous_runtime, ambiguous_cache, ambiguous_data, ambiguous_home):
            path.mkdir(mode=0o700, parents=True)
        ambiguous_token = os.urandom(TOKEN_BYTES).hex()
        ambiguous_env_file = ambiguous_config / "relay.env"
        ambiguous_env_file.write_text(
            f"HERDR_RELAY_TOKEN={ambiguous_token}\nHERDR_RELAY_INSTANCE_ID=package-ambiguous-instance\n"
            "HERDR_RELAY_TRANSPORT=tailscale\nHERDR_RELAY_HOST=127.0.0.1\n"
            "HERDR_RELAY_REARM_BOOTSTRAP=0\nHERDR_RELAY_PORT=18377\nHERDR_RELAY_PLUGIN_PORT=18378\n",
            encoding="ascii",
        )
        os.chmod(ambiguous_env_file, 0o600)
        ambiguous_state_file = ambiguous_root / "api-state.json"
        state = APIState(ambiguous_state_file)
        state.drop_registration_ack = True
        state.persist()
        local_server.fixture = state  # type: ignore[attr-defined]
        ambiguous_operations = ambiguous_root / "fake-herdr-operations.jsonl"
        ambiguous_env = dict(relay_env)
        ambiguous_env.update({
            "HOME": str(ambiguous_home), "TMPDIR": str(ambiguous_root),
            "XDG_CONFIG_HOME": str(ambiguous_config), "XDG_CACHE_HOME": str(ambiguous_cache),
            "XDG_DATA_HOME": str(ambiguous_data), "HERDR_RELAY_ENV": str(ambiguous_env_file),
            "HERDR_SOCKET_PATH": str(ambiguous_runtime / "herdr.sock"),
            "HERDR_RELEASE_ROOT": str(ambiguous_data / "releases"),
            "FAKE_HERDR_OPERATIONS": str(ambiguous_operations),
            "HERDR_FIXTURE_API_STATE": str(ambiguous_state_file),
        })
        launcher, ambiguous_stdout = launch_managed_rejection(package, ambiguous_env)
        if (origin + "/#").encode() in ambiguous_stdout or ambiguous_token.encode() in ambiguous_stdout or b"setup link" in ambiguous_stdout.lower():
            die("ambiguous conditional-write refusal emitted a setup URL or token")
        if (state.events.count("localapi:ack:dropped-after-commit") != 1
                or state.events.count("localapi:config:post") != 1
                or state.events.count("localapi:conditional:if-match") != 1
                or "localapi:retirement:selective-delete" in state.events):
            die("ambiguous conditional write was replayed or globally cleared")
        foreground = state.config.get("Foreground", {})
        if len(state.config) != 1 or len(foreground) != 1:
            die("ambiguous write recovery did not preserve its exact committed foreground entry")
        recovery_file = ambiguous_config / "tailscale-session.env"
        if not recovery_file.is_file() or stat.S_IMODE(recovery_file.stat().st_mode) != 0o600:
            die("ambiguous write did not retain its private mode-0600 recovery record")
        recovery = dict(
            line.split("=", 1) for line in recovery_file.read_text(encoding="utf-8").splitlines() if "=" in line
        )
        if recovery.get("HERDR_RELAY_STAGE") != "activation-pending":
            die("ambiguous write recovery record lost its activation-pending stage")
        control_socket = ambiguous_config / "tailscale-control.sock"
        if not control_socket.is_socket() or recovery.get("HERDR_RELAY_PAIRING_SOCKET") != str(control_socket):
            die("ambiguous write did not retain its private authenticated control socket")
        try:
            ambiguous_owner_pid = int(recovery["HERDR_RELAY_SUPERVISOR_PID"])
            os.kill(ambiguous_owner_pid, 0)
        except (KeyError, OSError, ValueError):
            die("ambiguous write did not retain its live foreground owner")
        case_results[EXPECTED_CASES[9]] = "pass"
        transitions.extend(["lost-ack:conditional-post-committed", "lost-ack:no-replay", "lost-ack:private-recovery-retained", "lost-ack:no-setup-link"])
        launcher = None

        if len(case_results) != len(EXPECTED_CASES) or any(case_results.get(name) != "pass" for name in EXPECTED_CASES):
            die("one or more required package acceptance cases were missing")
        set_stage("complete")
        return 0
    except Exception as error:
        failure_type = type(error).__name__
        failure_code = error.code if isinstance(error, GateFailure) else "unexpected_exception"
        print(f"extracted package acceptance failed: stage={current_stage} code={failure_code}", file=sys.stderr)
        raise
    finally:
        if launcher is not None:
            stop_launcher(launcher)
        if public_server is not None:
            public_server.shutdown()
            public_server.server_close()
        if state is not None:
            transitions.extend(state.events[-12:])
        record = {
            "schema_version": 1,
            "candidate_sha": source_sha,
            "archive_sha256": archive_digest,
            "archive_wrappers": archive_wrappers,
            "tested_binary_sha256": binary_digest,
            "tested_binary_path": binary_path,
            "archive_path": str(archive),
            "workflow_url": workflow_url,
            "isolation": "docker-network-none-fixed-localapi-socket",
            "tls_verification": "system-trust-inside-disposable-container; browser-ignore-disabled",
            "current_stage": current_stage,
            "completed_stages": completed_stages,
            "exception_type": failure_type,
            "failure_code": failure_code,
            "browser_stage": browser_stage,
            "browser_exception_type": browser_exception_type,
            "expected_case_count": len(EXPECTED_CASES),
            "executed_case_count": len([name for name in EXPECTED_CASES if case_results.get(name) == "pass"]),
            "cases": [{"name": name, "result": case_results.get(name, "missing")} for name in EXPECTED_CASES],
            "fixture_transitions": list(dict.fromkeys(transitions))[:64],
            "browser": {key: browser_evidence.get(key) for key in ("result", "controller_enrolled", "reader_enrolled", "controller_read", "controller_command", "reader_read", "reader_mutation_denied", "credentials_preserved") if key in browser_evidence},
            "result": "pass" if len(case_results) == len(EXPECTED_CASES) and all(case_results.get(name) == "pass" for name in EXPECTED_CASES) else "fail",
        }
        evidence_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(evidence_path, record)
        if ambiguous_owner_pid is not None:
            try:
                os.kill(ambiguous_owner_pid, 15)
            except ProcessLookupError:
                pass
        if local_server is not None:
            local_server.fixture.watch_closed.set()  # type: ignore[attr-defined]
            local_server.shutdown()
            local_server.server_close()
        shutil.rmtree(temporary_root, ignore_errors=True)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        # Do not include external exception strings: they may carry private URLs
        # or process diagnostics. The sanitized artifact is the only evidence.
        print("extracted package acceptance failed: " + type(error).__name__, file=sys.stderr)
        sys.exit(1)

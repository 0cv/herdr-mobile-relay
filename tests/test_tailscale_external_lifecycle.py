#!/usr/bin/env python3
"""Hosted disposable HTTPS/WSS fixture for operator-owned Serve mode.

This test starts the real candidate relay behind a local TLS reverse proxy,
uses only temporary keys/state, and deliberately provides a Tailscale executable
that fails if called. No system trust store, Tailscale daemon, router, or external
service is changed.
"""

from __future__ import annotations

import functools
import http.client
import json
import os
import pathlib
import re
import select
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, SimpleHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


CASES: dict[str, str] = {}
PHASE = "setup"
DIAGNOSTIC_CODE = ""


def safe_launcher_diagnostic(output: bytes) -> str:
    markers = (
        (b"External Serve relay failed to start.", "relay_process_exited_before_health"),
        (b"Local relay health, instance, or startup identity did not match", "local_health_identity_mismatch"),
        (b"Trusted HTTPS did not prove this relay instance", "external_https_identity_mismatch"),
        (b"selected phone-app origin does not serve this exact release", "phone_app_bundle_mismatch"),
        (b"Setup link was not printed.", "setup_link_arm_failure"),
        (b"Relay loopback port", "relay_port_conflict"),
    )
    for marker, code in markers:
        if marker in output:
            return code
    return "no_safe_launcher_diagnostic"


def record(name: str) -> None:
    CASES[name] = "pass"


def fail(message: str) -> None:
    raise AssertionError(message)


def free_port(excluded: set[int] | None = None) -> int:
    excluded = excluded or set()
    while True:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            candidate = int(sock.getsockname()[1])
        if candidate not in excluded:
            return candidate


class RelayProxyHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format: str, *_args: Any) -> None:
        # Access logs could contain the setup fragment; never print them.
        return

    @property
    def server_state(self) -> "RelayProxy":
        return self.server  # type: ignore[return-value]

    def do_GET(self) -> None:
        state = self.server_state
        state.request_count += 1
        if self.path == "/healthz":
            state.health_count += 1
            if state.health_mode == "refuse":
                body = b"operator-owned fixture refuses health verification\n"
                self.send_response(503)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)
                return
        if self.headers.get("Upgrade", "").lower() == "websocket":
            self._websocket_tunnel()
            return
        if self.path != "/healthz":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.send_header("Connection", "close")
            self.end_headers()
            return
        self._http_proxy()

    def do_HEAD(self) -> None:
        self.server_state.request_count += 1
        self._http_proxy()

    def _http_proxy(self) -> None:
        state = self.server_state
        connection = http.client.HTTPConnection("127.0.0.1", state.backend_port, timeout=10)
        headers = {
            key: value
            for key, value in self.headers.items()
            if key.lower() not in {"connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade"}
        }
        headers["Connection"] = "close"
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else None
        try:
            try:
                connection.request(self.command, self.path, body=body, headers=headers)
                response = connection.getresponse()
                payload = response.read()
                if self.command == "GET" and self.path == "/healthz" and state.health_mode in {"wrong_instance", "wrong_bundle"}:
                    health = json.loads(payload)
                    if state.health_mode == "wrong_instance":
                        health["instance"] = "foreign-fixture-instance"
                    else:
                        health["bundle_hash"] = "0" * 64
                    payload = json.dumps(health, separators=(",", ":")).encode("utf-8")
            except OSError:
                self.send_response(502, "Operator-owned backend unavailable")
                self.send_header("Content-Length", "0")
                self.send_header("Connection", "close")
                self.end_headers()
                return
            self.send_response(response.status, response.reason)
            excluded = {
                "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
                "te", "trailer", "transfer-encoding", "upgrade", "content-length",
            }
            for key, value in response.getheaders():
                if key.lower() not in excluded:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(payload)))
            self.send_header("Connection", "close")
            self.end_headers()
            if self.command != "HEAD" and payload:
                self.wfile.write(payload)
        finally:
            connection.close()

    def _websocket_tunnel(self) -> None:
        upstream = socket.create_connection(("127.0.0.1", self.server_state.backend_port), timeout=5)
        request = [f"{self.command} {self.path} HTTP/1.1\r\n"]
        for key, value in self.headers.items():
            request.append(f"{key}: {value}\r\n")
        request.append("\r\n")
        upstream.sendall("".join(request).encode("iso-8859-1"))
        response = bytearray()
        try:
            while b"\r\n\r\n" not in response:
                chunk = upstream.recv(4096)
                if not chunk:
                    fail("relay closed before the WebSocket handshake")
                response.extend(chunk)
                if len(response) > 65536:
                    fail("relay WebSocket handshake exceeded the fixture limit")
            self.connection.sendall(response)
            if b" 101 " not in bytes(response).split(b"\r\n", 1)[0]:
                return
            peers = (self.connection, upstream)
            while True:
                readable, _, _ = select.select(peers, (), (), 1.0)
                for source in readable:
                    payload = source.recv(65536)
                    if not payload:
                        return
                    destination = upstream if source is self.connection else self.connection
                    destination.sendall(payload)
        except (BrokenPipeError, ConnectionResetError, OSError):
            return
        finally:
            upstream.close()


class StaticAppHandler(SimpleHTTPRequestHandler):
    def log_message(self, _format: str, *_args: Any) -> None:
        return


class StaticAppServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, web_root: pathlib.Path, certificate: pathlib.Path, private_key: pathlib.Path):
        handler = functools.partial(StaticAppHandler, directory=str(web_root))
        super().__init__(("127.0.0.1", 0), handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(str(certificate), str(private_key))
        self.socket = context.wrap_socket(self.socket, server_side=True)

    def handle_error(self, _request: Any, _client_address: Any) -> None:
        return


class RelayProxy(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, backend_port: int, certificate: pathlib.Path, private_key: pathlib.Path):
        self.backend_port = backend_port
        self.health_mode = "pass"
        self.health_count = 0
        self.request_count = 0
        super().__init__(("127.0.0.1", 0), RelayProxyHandler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(str(certificate), str(private_key))
        self.socket = context.wrap_socket(self.socket, server_side=True)

    def handle_error(self, _request: Any, _client_address: Any) -> None:
        # Avoid traceback/logging paths that might contain request data.
        return


def read_setup_state(config_dir: pathlib.Path) -> tuple[bytes, list[str]]:
    path = config_dir / "device-auth" / "devices.json"
    data = path.read_bytes()
    decoded = json.loads(data)
    credentials = decoded.get("credentials")
    if not isinstance(credentials, list):
        fail("device store did not contain the expected credential list")
    identifiers = []
    for item in credentials:
        if not isinstance(item, dict) or not item.get("credential_id") or not item.get("device_id"):
            fail("device store contained an incomplete credential record")
        identifiers.append(str(item["credential_id"]))
    return data, identifiers


def read_until_setup_link(process: subprocess.Popen[bytes], timeout: float = 90.0) -> bytes:
    global DIAGNOSTIC_CODE
    if process.stdout is None:
        fail("relay launcher output pipe is unavailable")
    output = bytearray()
    deadline = time.monotonic() + timeout
    descriptor = process.stdout.fileno()
    while time.monotonic() < deadline:
        if process.poll() is not None:
            while select.select([descriptor], [], [], 0)[0]:
                chunk = os.read(descriptor, 4096)
                if not chunk:
                    break
                output.extend(chunk)
            DIAGNOSTIC_CODE = safe_launcher_diagnostic(bytes(output))
            fail("external Serve launcher exited before printing its verified setup link")
        ready, _, _ = select.select([descriptor], [], [], 0.25)
        if ready:
            chunk = os.read(descriptor, 4096)
            if not chunk:
                DIAGNOSTIC_CODE = safe_launcher_diagnostic(bytes(output))
                fail("external Serve launcher closed output before the setup link")
            output.extend(chunk)
            if b"This link pairs one phone within 10 minutes" in output:
                return bytes(output)
    DIAGNOSTIC_CODE = safe_launcher_diagnostic(bytes(output))
    fail("external Serve launcher did not reach verified setup-link output in time")
    return b""


def start_launcher(root: pathlib.Path, env: dict[str, str], active: list[subprocess.Popen[bytes]]) -> tuple[subprocess.Popen[bytes], bytes]:
    process = subprocess.Popen(
        [str(root / "relay" / "tailscale-external.sh"), "--origin", env["HERDR_EXTERNAL_HTTPS_FIXTURE_ORIGIN"]],
        cwd=root,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    active.append(process)
    return process, read_until_setup_link(process)


def stop_launcher(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    try:
        process.communicate(timeout=18)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.communicate(timeout=5)
    if process.returncode is None:
        fail("external Serve launcher failed to exit")


def run_setup_link(root: pathlib.Path, env: dict[str, str]) -> tuple[int, bytes]:
    result = subprocess.run(
        [str(root / "relay" / "setup-link.sh")],
        cwd=root,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=90,
        check=False,
    )
    return result.returncode, result.stdout


def run_e2ee_client(root: pathlib.Path, env: dict[str, str]) -> None:
    client_env = dict(env)
    client_env["HERDR_EXTERNAL_FIXTURE_ORIGIN"] = env["HERDR_EXTERNAL_HTTPS_FIXTURE_ORIGIN"]
    client_env["HERDR_EXTERNAL_FIXTURE_TOKEN"] = env["HERDR_EXTERNAL_FIXTURE_TOKEN"]
    result = subprocess.run(
        ["go", "test", "-tags=herdr_tailscale_test", "./internal/app", "-run", "^TestTailscaleExternalBYOHostedE2EE$", "-count=1"],
        cwd=root,
        env=client_env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=180,
        check=False,
    )
    if result.returncode != 0:
        fail("hosted encrypted WSS enrollment client failed")


def build_fixture(root: pathlib.Path, directory: pathlib.Path, revision: str) -> tuple[pathlib.Path, pathlib.Path, pathlib.Path]:
    plugin = (root / "herdr-plugin.toml").read_text(encoding="utf-8")
    match = re.search(r'^version\s*=\s*"([^"]+)"', plugin, re.MULTILINE)
    if match is None:
        fail("candidate plugin version is unavailable")
    version = match.group(1)
    binary_dir = directory / "bin"
    binary_dir.mkdir(mode=0o700)
    binary = binary_dir / "herdr-mobile-relay"
    command = [
        "go", "build", "-trimpath",
        "-ldflags", f"-X main.version={version} -X main.revision={revision}",
        "-o", str(binary), "./cmd/herdr-mobile-relay",
    ]
    result = subprocess.run(command, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=240, check=False)
    if result.returncode != 0:
        fail("candidate relay binary build failed")
    binary.chmod(0o700)
    fake_herdr = binary_dir / "fake-herdr"
    fake_build = subprocess.run(["go", "build", "-trimpath", "-o", str(fake_herdr), "./cmd/fake-herdr"],
                                cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=180, check=False)
    if fake_build.returncode != 0:
        fail("hosted fake Herdr fixture build failed")
    fake_herdr.chmod(0o700)

    web_root = directory / "web"
    shutil.copytree(root / "web", web_root)
    version_path = web_root / "version.json"
    version_data = json.loads(version_path.read_text(encoding="utf-8"))
    version_data["revision"] = revision
    version_path.write_text(json.dumps(version_data, separators=(",", ":")) + "\n", encoding="utf-8")
    return binary, fake_herdr, web_root


def candidate_revision(root: pathlib.Path) -> str:
    result = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, check=True)
    revision = result.stdout.strip()
    expected = os.environ.get("GITHUB_SHA", revision)
    if revision != expected:
        fail("hosted fixture checkout does not match its event SHA")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        fail("hosted fixture candidate SHA is invalid")
    return revision


def evidence_path() -> pathlib.Path | None:
    raw = os.environ.get("HERDR_EXTERNAL_FIXTURE_EVIDENCE", "")
    return pathlib.Path(raw) if raw else None


def write_evidence(result: str, candidate: str, error_type: str = "") -> None:
    destination = evidence_path()
    if destination is None:
        return
    payload = {
        "schema_version": 1,
        "candidate_sha": candidate,
        "fixture": "operator-owned-tailscale-https-serve",
        "result": result,
        "cases": [{"name": name, "result": CASES[name]} for name in sorted(CASES)],
        "diagnostic_class": error_type,
        "diagnostic_code": DIAGNOSTIC_CODE,
        "contains_credentials": False,
        "system_trust_store_modified": False,
        "tailscale_cli_invoked": False,
        "production_tailscale_ingress_modified": False,
        "disposable_https_fixture_behavior_toggled": True,
    }
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    destination.chmod(0o600)


def run_fixture() -> str:
    global PHASE
    root = pathlib.Path(__file__).resolve().parent.parent
    revision = candidate_revision(root)
    go_environment = subprocess.run(["go", "env", "GOCACHE", "GOMODCACHE"], stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, text=True, check=True).stdout.splitlines()
    if len(go_environment) != 2:
        fail("hosted Go cache paths are unavailable")
    with tempfile.TemporaryDirectory(prefix="herdr-r3b-https-") as temporary:
        directory = pathlib.Path(temporary)
        (directory / "home").mkdir(mode=0o700)
        config_dir = directory / "config"
        config_dir.mkdir(mode=0o700)
        certificate = directory / "fixture-cert.pem"
        private_key = directory / "fixture-key.pem"
        openssl = subprocess.run(
            [
                "openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-nodes",
                "-days", "1", "-keyout", str(private_key), "-out", str(certificate),
                "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
                "-addext", "basicConstraints=critical,CA:TRUE",
                "-addext", "keyUsage=critical,keyCertSign,cRLSign,digitalSignature",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if openssl.returncode != 0:
            fail("disposable HTTPS fixture certificate creation failed")
        private_key.chmod(0o600)
        certificate.chmod(0o600)
        record("temporary_loopback_https_certificate")

        relay_port, plugin_port = free_port(), free_port()
        while plugin_port == relay_port:
            plugin_port = free_port()
        token = os.urandom(16).hex()
        instance = os.urandom(16).hex()
        env_file = config_dir / "relay.env"
        env_file.write_text(
            f"HERDR_RELAY_TOKEN={token}\n"
            f"HERDR_RELAY_INSTANCE_ID={instance}\n"
            f"HERDR_RELAY_HOST=127.0.0.1\n"
            f"HERDR_RELAY_PORT={relay_port}\n"
            f"HERDR_RELAY_PLUGIN_PORT={plugin_port}\n",
            encoding="utf-8",
        )
        env_file.chmod(0o600)

        binary, fake_herdr, web_root = build_fixture(root, directory, revision)
        scenario = directory / "fake-herdr-scenario.json"
        scenario.write_text(
            '{"panes":[{"pane_id":"fixture-pane","terminal_id":"fixture-terminal",'
            '"agent":"claude","name":"fixture","agent_status":"working",'
            '"tab_id":"fixture-tab","workspace_id":"fixture-workspace","cwd":"/tmp",'
            '"revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"fixture-tab",'
            '"workspace_id":"fixture-workspace","label":"fixture","number":1,"cwd":"/tmp"}]}',
            encoding="utf-8",
        )
        socket_path = directory / "herdr.sock"
        proxy = RelayProxy(relay_port, certificate, private_key)
        static_app = StaticAppServer(web_root, certificate, private_key)
        reserved_ports = {proxy.server_port, static_app.server_port}
        if relay_port in reserved_ports or plugin_port in reserved_ports:
            relay_port = free_port(reserved_ports)
            plugin_port = free_port(reserved_ports | {relay_port})
            env_file.write_text(
                f"HERDR_RELAY_TOKEN={token}\n"
                f"HERDR_RELAY_INSTANCE_ID={instance}\n"
                f"HERDR_RELAY_HOST=127.0.0.1\n"
                f"HERDR_RELAY_PORT={relay_port}\n"
                f"HERDR_RELAY_PLUGIN_PORT={plugin_port}\n",
                encoding="utf-8",
            )
        proxy.backend_port = relay_port
        proxy_thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        static_app_thread = threading.Thread(target=static_app.serve_forever, daemon=True)
        proxy_thread.start()
        static_app_thread.start()
        origin = f"https://127.0.0.1:{proxy.server_port}"
        phone_app_origin = f"https://127.0.0.1:{static_app.server_port}"

        fake_bin = directory / "home" / ".local" / "bin"
        fake_bin.mkdir(mode=0o700, parents=True)
        fake_tailscale = fake_bin / "tailscale"
        fake_tailscale.write_text(
            "#!/bin/sh\nprintf 'invoked\\n' >> \"$HERDR_TAILSCALE_SENTINEL\"\nexit 97\n",
            encoding="utf-8",
        )
        fake_tailscale.chmod(0o700)
        sentinel = directory / "tailscale-called"
        empty_ca = directory / "empty-ca.pem"
        empty_ca.write_bytes(b"")
        env = dict(os.environ)
        env.update(
            {
                "HOME": str(directory / "home"),
                "XDG_CONFIG_HOME": str(directory / "xdg-config"),
                "XDG_CACHE_HOME": str(directory / "xdg-cache"),
                "XDG_DATA_HOME": str(directory / "xdg-data"),
                "GOCACHE": go_environment[0],
                "GOMODCACHE": go_environment[1],
                "TMPDIR": str(directory),
                "PATH": f"{fake_bin}:{os.environ.get('PATH', '/usr/bin:/bin:/usr/sbin:/sbin')}",
                "HERDR_RELAY_ENV": str(env_file),
                "HERDR_RELAY_BIN": str(binary),
                "HERDR_WEB_ROOT": str(web_root),
                "HERDR_BIN": str(fake_herdr),
                "HERDR_SOCKET_PATH": str(socket_path),
                "HERDR_RELAY_POLL_INTERVAL": "0.25",
                "FAKE_HERDR_SCENARIO": str(scenario),
                "FAKE_HERDR_OPERATIONS": str(directory / "fake-herdr-operations.jsonl"),
                "HERDR_TAILSCALE_BIN": str(fake_tailscale),
                "HERDR_TAILSCALE_SENTINEL": str(sentinel),
                "HERDR_EXTERNAL_HTTPS_FIXTURE_ORIGIN": origin,
                "HERDR_EXTERNAL_FIXTURE_TOKEN": token,
                "HERDR_PHONE_APP_URL": phone_app_origin,
                "HERDR_RELAY_PORT": str(relay_port),
                "HERDR_RELAY_PLUGIN_PORT": str(plugin_port),
                "HERDR_REACHABILITY_PORT_MAPPING": "0",
                "HERDR_TRANSPORT_FORCE_RELAY": "1",
                "CURL_CA_BUNDLE": str(certificate),
                "SSL_CERT_FILE": str(certificate),
                "NO_PROXY": "127.0.0.1,localhost",
                "no_proxy": "127.0.0.1,localhost",
            }
        )
        active: list[subprocess.Popen[bytes]] = []
        try:
            PHASE = "first_foreground_start"
            first, first_output = start_launcher(root, env, active)
            PHASE = "first_setup_output_contract"
            if b"operator-owned HTTPS Serve" not in first_output or b"This link pairs one phone" not in first_output:
                fail("candidate did not print its operator-owned verified setup link")
            PHASE = "first_tailscale_cli_refusal"
            if sentinel.exists():
                fail("BYO launcher invoked the Tailscale CLI")
            relay_context = ssl.create_default_context(cafile=str(certificate))
            relay_only = http.client.HTTPSConnection("127.0.0.1", proxy.server_port, context=relay_context, timeout=3)
            relay_only.request("GET", "/version.json")
            relay_only_response = relay_only.getresponse()
            relay_only_response.read()
            relay_only.close()
            PHASE = "relay_https_origin_behavior"
            if relay_only_response.status != 404:
                fail("relay HTTPS fixture unexpectedly served the independent phone-app bundle")
            record("relay_https_origin_proved_health_wss_only")
            PHASE = "persisted_external_transport_origin"
            saved_config = dict(
                line.split("=", 1)
                for line in env_file.read_text(encoding="utf-8").splitlines()
                if "=" in line
            )
            if saved_config.get("HERDR_RELAY_TRANSPORT") != "tailscale-external" or \
                    saved_config.get("HERDR_EXTERNAL_HTTPS_ORIGIN") != origin:
                fail("operator-owned transport/origin selection was not persisted canonically")
            PHASE = "persisted_independent_phone_app_origin"
            saved_app_origin = (config_dir / "phone-app-origin-configured").read_text(encoding="utf-8").strip()
            if saved_app_origin != phone_app_origin:
                fail("independently hosted phone-app origin was not retained")
            record("independent_phone_app_bundle_trusted_and_retained")
            PHASE = "no_managed_or_transient_run_ownership"
            if "HERDR_RELAY_CONTROL_RUN_ID" in saved_config or "HERDR_RELAY_RUN_ID" in saved_config:
                fail("transient external or managed run ownership leaked into persistent configuration")
            record("foreground_start_trusted_https_release_identity_and_qr")

            PHASE = "first_e2ee_enrollment"
            run_e2ee_client(root, env)
            first_store, first_ids = read_setup_state(config_dir)
            if len(first_ids) != 1:
                fail("first WSS pairing did not persist exactly one device credential")
            record("trusted_wss_end_to_end_enrollment")

            PHASE = "untrusted_certificate_refusal"
            bad_tls_env = dict(env)
            bad_tls_env["CURL_CA_BUNDLE"] = str(empty_ca)
            bad_tls_env["SSL_CERT_FILE"] = str(empty_ca)
            request_count = proxy.request_count
            status, output = run_setup_link(root, bad_tls_env)
            if status == 0 or b"Open this private setup link" in output or b"Scan this QR code" in output:
                fail("untrusted HTTPS certificate did not fail closed without a setup link")
            if proxy.request_count != request_count:
                fail("untrusted certificate reached the HTTPS Serve fixture")
            if (config_dir / "device-auth" / "devices.json").read_bytes() != first_store:
                fail("TLS refusal changed the persistent invitation or device credentials")
            record("untrusted_tls_reprint_refused_without_state_change")

            PHASE = "adverse_https_health_refusal"
            proxy.health_mode = "refuse"
            health_count = proxy.health_count
            status, output = run_setup_link(root, env)
            if status == 0 or b"Open this private setup link" in output or b"Scan this QR code" in output:
                fail("adverse HTTPS health did not fail closed without a setup link")
            if proxy.health_count <= health_count:
                fail("adverse HTTPS health response was not exercised")
            if (config_dir / "device-auth" / "devices.json").read_bytes() != first_store:
                fail("adverse HTTPS health changed the persistent invitation or device credentials")
            proxy.health_mode = "pass"
            record("wrong_https_relay_health_refused_without_state_change")

            for mode, case in (
                ("wrong_instance", "foreign_https_instance_refused_without_state_change"),
                ("wrong_bundle", "incorrect_https_bundle_refused_without_state_change"),
            ):
                PHASE = case
                proxy.health_mode = mode
                health_count = proxy.health_count
                status, output = run_setup_link(root, env)
                if status == 0 or b"Open this private setup link" in output or b"Scan this QR code" in output:
                    fail(f"{mode} HTTPS health identity did not fail closed without a setup link")
                if proxy.health_count <= health_count:
                    fail(f"{mode} HTTPS health response was not exercised")
                if (config_dir / "device-auth" / "devices.json").read_bytes() != first_store:
                    fail(f"{mode} HTTPS refusal changed persistent invitation or device credentials")
                record(case)
            proxy.health_mode = "pass"

            PHASE = "wrong_private_control_identity_refusal"
            session_file = config_dir / "tailscale-external-session.env"
            session = dict(
                line.split("=", 1)
                for line in session_file.read_text(encoding="utf-8").splitlines()
                if "=" in line
            )
            wrong_run = "wrong-control-run"
            command = [
                str(binary), "managed-state", "reprint", "--external",
                "--dir", str(config_dir),
                "--socket", session["HERDR_RELAY_PAIRING_SOCKET"],
                "--run-id", wrong_run,
                "--instance", instance,
                "--origin-file", "phone-app-origin-configured",
                "--origin-value", phone_app_origin,
            ]
            wrong = subprocess.run(command, cwd=root, env=env, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15, check=False)
            if wrong.returncode == 0 or b"invitation_expires_at" in wrong.stdout:
                fail("wrong external control identity was accepted for invitation reprint")
            if (config_dir / "device-auth" / "devices.json").read_bytes() != first_store:
                fail("wrong control identity changed the persistent invitation or device credentials")
            if (config_dir / "owner.lock").exists():
                fail("operator-owned mode created a managed Tailscale owner lock")
            record("private_control_identity_refused_without_managed_owner")

            PHASE = "successful_reprint_after_adverse_cases"
            status, output = run_setup_link(root, env)
            if status != 0 or b"This link pairs one phone within 10 minutes" not in output:
                fail("valid HTTPS/control reprint did not recover after adverse-case refusals")
            rearmed, rearmed_ids = read_setup_state(config_dir)
            if rearmed_ids != first_ids or rearmed == first_store:
                fail("successful invitation reprint did not preserve exactly the enrolled credential")
            record("durable_reprint_preserved_first_device")

            PHASE = "second_e2ee_enrollment"
            run_e2ee_client(root, env)
            second_store, second_ids = read_setup_state(config_dir)
            if len(second_ids) != 2 or first_ids[0] not in second_ids:
                fail("second invitation did not enroll a distinct device while retaining the first")
            record("second_wss_enrollment_preserved_prior_device")

            PHASE = "foreground_stop_and_restart"
            stop_launcher(first)
            active.remove(first)
            if (config_dir / "tailscale-external-session.env").exists():
                fail("foreground shutdown retained a live external session record")
            if (config_dir / "tailscale-external-control.sock").exists():
                fail("foreground shutdown retained its private pairing-control socket")
            if sentinel.exists():
                fail("BYO lifecycle invoked the Tailscale CLI")
            if not proxy_thread.is_alive() or not proxy.socket:
                fail("operator-owned HTTPS ingress fixture was stopped with the Herdr backend")
            try:
                socket.create_connection(("127.0.0.1", relay_port), timeout=1).close()
                fail("foreground shutdown left the Herdr loopback backend running")
            except OSError:
                pass
            record("stop_retained_operator_ingress_and_stopped_only_backend")

            PHASE = "restart_credential_persistence"
            second, second_output = start_launcher(root, env, active)
            if b"This link pairs one phone within 10 minutes" not in second_output:
                fail("foreground restart did not verify and re-arm the existing instance")
            restarted_store, restarted_ids = read_setup_state(config_dir)
            if len(restarted_ids) != 2 or set(restarted_ids) != set(second_ids):
                fail("foreground restart reset or lost enrolled device credentials")
            if (config_dir / "owner.lock").exists():
                fail("external foreground restart created managed ownership state")
            if sentinel.exists():
                fail("BYO restart invoked the Tailscale CLI")
            record("restart_rearmed_without_resetting_credentials_or_ingress")

            PHASE = "final_stop"
            stop_launcher(second)
            active.remove(second)
            if sentinel.exists():
                fail("BYO shutdown invoked the Tailscale CLI")
            # TLS still terminates at the operator-owned proxy, while its now
            # stopped loopback backend correctly produces a gateway error.
            context = ssl.create_default_context(cafile=str(certificate))
            connection = http.client.HTTPSConnection("127.0.0.1", proxy.server_port, context=context, timeout=3)
            connection.request("GET", "/healthz")
            response = connection.getresponse()
            response.read()
            connection.close()
            if response.status != 502:
                fail("operator-owned ingress did not remain after its Herdr backend stopped")
            app_connection = http.client.HTTPSConnection(
                "127.0.0.1", static_app.server_port, context=context, timeout=3
            )
            app_connection.request("GET", "/version.json")
            app_response = app_connection.getresponse()
            app_version = app_response.read()
            app_connection.close()
            if app_response.status != 200 or revision.encode("ascii") not in app_version:
                fail("independent verified phone app did not remain after the relay backend stopped")
            record("operator_https_ingress_remained_after_final_stop")
            record("independent_phone_app_remained_after_backend_stop")
        finally:
            for process in list(active):
                stop_launcher(process)
            proxy.shutdown()
            proxy.server_close()
            proxy_thread.join(timeout=3)
            static_app.shutdown()
            static_app.server_close()
            static_app_thread.join(timeout=3)

    record("no_system_trust_or_production_tailscale_state_changed")
    return revision


def main() -> int:
    candidate = ""
    try:
        candidate = candidate_revision(pathlib.Path(__file__).resolve().parent.parent)
        revision = run_fixture()
        write_evidence("pass", revision)
        print("PASS hosted operator-owned HTTPS Serve lifecycle: HTTPS/WSS, E2EE, refusal, restart, and ingress ownership")
        return 0
    except Exception as error:  # noqa: BLE001 - report only a sanitized diagnostic class.
        CASES.setdefault(PHASE, "fail")
        write_evidence("fail", candidate, type(error).__name__)
        diagnostic = f"; {DIAGNOSTIC_CODE}" if DIAGNOSTIC_CODE else ""
        print(f"FAIL hosted operator-owned HTTPS Serve fixture at {PHASE} ({type(error).__name__}{diagnostic})", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

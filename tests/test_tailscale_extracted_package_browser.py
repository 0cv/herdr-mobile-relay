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
RELAY_PORT = 18377
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
    "fixture_browser_nss_setup_failed", "managed_launcher_spawn", "managed_launcher_output_limit",
    "managed_launcher_exit_before_link", "managed_launcher_link_timeout",
    "managed_launcher_cli_refusal", "managed_launcher_pre_serve_status_exit",
    "managed_launcher_pre_watch_exit", "managed_launcher_pre_registration_exit",
    "managed_launcher_registration_state_unconfirmed",
    "managed_launcher_post_registration_exit", "managed_launcher_owner_prepare_failed",
    "managed_launcher_runtime_directory_failed", "managed_launcher_inspection_failed",
    "managed_launcher_inventory_failed", "managed_launcher_herdr_inventory_poll_failed",
    "managed_launcher_herdr_workspace_poll_failed", "managed_launcher_local_readiness_failed",
    "managed_launcher_owner_changed_root", "managed_launcher_owner_foreign_state",
    "managed_launcher_owner_invalid_record", "managed_launcher_owner_unknown_authority",
    "managed_launcher_owner_retained_evidence", "managed_launcher_owner_busy",
    "managed_launcher_owner_timeout", "managed_launcher_owner_random_failure",
    "managed_launcher_owner_io_failure", "managed_launcher_owner_unavailable",
    "managed_launcher_bootstrap_failed", "managed_launcher_session_authority_failed",
    "managed_launcher_owner_validation_marker",
    "fixture_localapi_watch_missing", "fixture_localapi_registration_missing",
    "fixture_localapi_session_missing", "fixture_registration_health_identity",
}
BROWSER_STAGES = {
    "browser_runner", "controller_enrollment", "controller_inventory",
    "controller_command", "controller_settings", "controller_settings_devices",
    "reader_invitation", "reader_enrollment",
    "reader_read_only", "credential_preservation", "browser_complete",
}
BROWSER_PROFILE_NAMES = {"controller", "reader"}
BROWSER_STORAGE_TYPES = {
    "absent", "invalid_json", "object", "array", "string", "number", "boolean", "null", "unavailable",
}
BROWSER_STORAGE_CHECKPOINTS = {"after_navigation", "credential_wait_failed", "credentialed"}
BROWSER_UI_CHECKPOINTS = {
    "inventory_initial", "agent_button_timeout", "agent_button_disabled",
    "agent_button_ready", "agent_click_failed", "prompt_wait_failed", "prompt_visible",
    "settings_navigated",
    "command_initial", "command_fill_failed", "command_prompt_filled",
    "command_send_failed", "command_result",
}
BROWSER_UI_VIEWS = {
    "agents", "terminal", "history", "settings", "workspaces", "launch",
    "activity", "activity_detail", "push", "push_unavailable", "notification", "other",
}
BROWSER_CONNECTION_STATES = {"unknown", "disconnected", "connected", "partial", "active_agent"}
BROWSER_INVENTORY_STATES = {"ready", "loading", "unavailable", "not_reported"}
BROWSER_AGENT_STATES = {"idle", "needs inspection", "working", "done", "other", "not_applicable"}
BROWSER_SEND_ACTION_STATES = {"missing", "send_prompt", "submit_terminal_text", "submitting_input"}
BROWSER_STATUS_TONES = {"danger", "warning", "success", "muted", "unknown"}
BROWSER_DIAGNOSTIC_CATEGORIES = {
    "websocket", "network", "storage", "tls", "type_error", "reference_error",
    "syntax_error", "dom_exception", "console_error", "page_error", "navigation_error",
}
BROWSER_EXCEPTION_TYPES = {
    "BrowserScriptMissing", "BrowserProtocolError", "BrowserAssertionError", "BrowserBudgetTimeout",
    "BrowserError", "TimeoutExpired", "TimeoutError", "StrictLocatorError",
    "Error", "TypeError", "ReferenceError",
    "SyntaxError", "RangeError", "DOMException", "TargetClosedError", "ProtocolError", "PageClosedError",
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


PRIVATE_LOG_STATUS_CATEGORIES = {
    "private_log_not_observed", "private_log_not_configured", "private_log_absent",
    "private_log_unreadable", "private_log_unclassified",
}


LAUNCHER_STDERR_PHASES = {
    "stderr_absent", "stderr_unclassified", "local_ready_refused",
    "activation_refused_not_dispatched", "activation_refused_settled_no_write",
    "activation_refused_settled_success", "activation_outcome_unresolved",
    "activation_response_unproven", "route_check_mismatch", "https_health_request_failed",
    "https_health_identity_mismatch", "release_identity_failed", "phone_bundle_verification_failed",
    "bootstrap_arm_refused", "bootstrap_outcome_unresolved", "bootstrap_arm_response_unproven",
    "final_checks_failed", "selection_snapshot_mismatch", "prior_session_or_socket",
    "relay_port_occupied", "operator_ingress_running", "preflight_credentials_missing",
    "preflight_transport_refused", "preflight_exposure_incomplete", "preflight_unauthenticated",
    "preflight_existing_serve", "preflight_inspection_failed", "preflight_identity_missing",
    "preflight_origin_changed", "preflight_second_inspection_failed",
    "preflight_identity_changed", "preflight_selection_missing",
}
LAUNCHER_STDERR_PATTERNS = (
    ("preflight_credentials_missing", re.compile(r"Relay credentials are not configured", re.IGNORECASE)),
    ("preflight_transport_refused", re.compile(r"Tailscale startup was requested without tailscale transport selection|Tailscale startup refuses bootstrap reset", re.IGNORECASE)),
    ("preflight_exposure_incomplete", re.compile(r"Tailscale exposure inspection is incomplete", re.IGNORECASE)),
    ("preflight_unauthenticated", re.compile(r"Tailscale is not running with an authenticated node", re.IGNORECASE)),
    ("preflight_existing_serve", re.compile(r"Existing Tailscale Serve/Funnel configuration was found", re.IGNORECASE)),
    ("preflight_inspection_failed", re.compile(r"Tailscale status/Serve inspection failed", re.IGNORECASE)),
    ("preflight_identity_missing", re.compile(r"Tailscale identity or exact daemon version is unavailable", re.IGNORECASE)),
    ("preflight_origin_changed", re.compile(r"Saved Tailscale origin does not match", re.IGNORECASE)),
    ("preflight_second_inspection_failed", re.compile(r"Tailscale changed before setup could start", re.IGNORECASE)),
    ("preflight_identity_changed", re.compile(r"Tailscale identity or daemon version changed during consent", re.IGNORECASE)),
    ("preflight_selection_missing", re.compile(r"Relay configuration directory is missing|Tailscale CLI is unavailable", re.IGNORECASE)),
    ("prior_session_or_socket", re.compile(r"A prior Tailscale session or control socket exists", re.IGNORECASE)),
    ("relay_port_occupied", re.compile(r"Relay or Herdr event port is occupied", re.IGNORECASE)),
    ("operator_ingress_running", re.compile(r"Operator-owned HTTPS Serve is still running", re.IGNORECASE)),
    ("local_ready_refused", re.compile(r"Managed relay did not acknowledge local_ready with its owner held\.", re.IGNORECASE)),
    ("activation_refused_not_dispatched", re.compile(r"Tailscale activation was refused with a decoded not-dispatched result", re.IGNORECASE)),
    ("activation_refused_settled_no_write", re.compile(r"Tailscale activation was refused with a decoded settled-no-write result", re.IGNORECASE)),
    ("activation_refused_settled_success", re.compile(r"Tailscale activation was refused with a decoded settled-success result", re.IGNORECASE)),
    ("activation_outcome_unresolved", re.compile(r"Tailscale activation outcome is unresolved", re.IGNORECASE)),
    ("activation_response_unproven", re.compile(r"Tailscale activation response did not prove local readiness, live ownership, and Serve readiness", re.IGNORECASE)),
    ("route_check_mismatch", re.compile(r"Fresh Tailscale route observation did not match the exact managed backend\.", re.IGNORECASE)),
    ("https_health_request_failed", re.compile(r"Trusted HTTPS identity verification failed for ", re.IGNORECASE)),
    ("https_health_identity_mismatch", re.compile(r"HTTPS health belongs to a different foreground relay session\.", re.IGNORECASE)),
    ("release_identity_failed", re.compile(r"Release identity could not be verified|Served release .* does not match|Served web bundle .* does not match|Release manifest .* does not match|Served release has no managed web bundle", re.IGNORECASE)),
    ("phone_bundle_verification_failed", re.compile(r"The selected phone-app origin does not serve this release's verified Herdr bundle|Final phone-app bundle verification failed", re.IGNORECASE)),
    ("bootstrap_arm_refused", re.compile(r"Bootstrap arm was refused with a decoded (?:not-committed|committed) result", re.IGNORECASE)),
    ("bootstrap_outcome_unresolved", re.compile(r"Bootstrap invitation outcome is unresolved", re.IGNORECASE)),
    ("bootstrap_arm_response_unproven", re.compile(r"Bootstrap arm response did not prove durable invitation and managed-route readiness", re.IGNORECASE)),
    ("final_checks_failed", re.compile(r"Final route, owner, TLS identity, bundle, or pairing checks failed", re.IGNORECASE)),
    ("selection_snapshot_mismatch", re.compile(r"Managed selection changed while setup was being verified|Relay selection changed during setup|Phone-app origin changed during setup", re.IGNORECASE)),
)


class GateFailure(RuntimeError):
    def __init__(self, code: str, launcher_log_category: str = "", launcher_stderr_phase: str = ""):
        self.code = code if code in FAILURE_CODES else "fixture_assertion"
        self.launcher_log_category = launcher_log_category if (
            launcher_log_category in PRIVATE_LOG_STATUS_CATEGORIES
            or launcher_log_category in PRIVATE_LOG_CATEGORY_CODES
        ) else ""
        self.launcher_stderr_phase = launcher_stderr_phase if launcher_stderr_phase in LAUNCHER_STDERR_PHASES else ""
        super().__init__(self.code)


LOCALAPI_COUNT_EVENTS = (
    "localapi:status",
    "localapi:config:get",
    "localapi:config:post",
    "localapi:watch:mask=2",
    "localapi:watch:closed",
    "localapi:conditional:if-match",
    "localapi:registration:foreground-exact",
    "localapi:retirement:selective-delete",
    "localapi:unexpected",
)


def safe_localapi_event_counts(events: list[str]) -> dict[str, int]:
    return {event: events.count(event) for event in LOCALAPI_COUNT_EVENTS}


def safe_localapi_registration_window(events: list[str]) -> dict[str, object]:
    registration = "localapi:registration:foreground-exact"
    retirement = "localapi:retirement:selective-delete"
    try:
        start = events.index(registration) + 1
    except ValueError:
        return {"observed": False, "retirement_observed": False, "counts": safe_localapi_event_counts([])}
    end = next((index for index in range(start, len(events)) if events[index] == retirement), len(events))
    return {
        "observed": True,
        "retirement_observed": end < len(events),
        "counts": safe_localapi_event_counts(events[start:end]),
    }


def safe_fake_herdr_operations(path: str | None) -> list[dict[str, str | int]]:
    if not path:
        return []
    try:
        with Path(path).open("rb") as source:
            lines = source.read(65536).decode("utf-8", "ignore").splitlines()
    except OSError:
        return []
    commands = {"agent prompt", "agent list", "pane list", "workspace list", "tab list"}
    outcomes = {"started", "succeeded", "failed"}
    counts: dict[tuple[str, str], int] = {}
    for line in lines:
        try:
            operation = json.loads(line)
        except ValueError:
            continue
        args = operation.get("argv") if isinstance(operation, dict) else None
        if not isinstance(args, list) or len(args) < 2 or not all(isinstance(arg, str) for arg in args[:2]):
            continue
        command = " ".join(args[:2])
        outcome = operation.get("outcome")
        key = (command if command in commands else "other", outcome if outcome in outcomes else "other")
        counts[key] = counts.get(key, 0) + 1
    return [
        {"command": command, "outcome": outcome, "count": count}
        for (command, outcome), count in sorted(counts.items())
    ]


def safe_fixture_cli_operations(env: dict[str, str]) -> list[str]:
    path = env.get("HERDR_FIXTURE_CLI_EVENTS")
    if not path:
        return []
    try:
        with Path(path).open("rb") as source:
            lines = source.read(4096).decode("ascii", "ignore").splitlines()
    except OSError:
        return []
    allowed = {"status_json", "version_daemon_json", "serve_status_json", "unsupported"}
    return [line for line in lines if line in allowed][-16:]


PRIVATE_LOG_CATEGORY_CODES = {
    "owner_prepare_failed": "managed_launcher_owner_prepare_failed",
    "runtime_directory_failed": "managed_launcher_runtime_directory_failed",
    "inspection_failed": "managed_launcher_inspection_failed",
    "local_readiness_failed": "managed_launcher_local_readiness_failed",
    "herdr_inventory_poll_failed": "managed_launcher_herdr_inventory_poll_failed",
    "herdr_workspace_poll_failed": "managed_launcher_herdr_workspace_poll_failed",
    "managed_owner_changed_root": "managed_launcher_owner_changed_root",
    "managed_owner_foreign_state": "managed_launcher_owner_foreign_state",
    "managed_owner_invalid_record": "managed_launcher_owner_invalid_record",
    "managed_owner_unknown_authority": "managed_launcher_owner_unknown_authority",
    "managed_owner_retained_evidence": "managed_launcher_owner_retained_evidence",
    "managed_owner_busy": "managed_launcher_owner_busy",
    "managed_owner_timeout": "managed_launcher_owner_timeout",
    "managed_owner_random_failure": "managed_launcher_owner_random_failure",
    "managed_owner_io_failure": "managed_launcher_owner_io_failure",
    "managed_owner_unavailable": "managed_launcher_owner_unavailable",
    "inventory_failed": "managed_launcher_inventory_failed",
    "bootstrap_failed": "managed_launcher_bootstrap_failed",
    "session_authority_failed": "managed_launcher_session_authority_failed",
    "owner_validation_marker": "managed_launcher_owner_validation_marker",
}
PRIVATE_LOG_PATTERNS = (
    ("owner_prepare_failed", re.compile(r"read-only Tailscale owner preparation failed", re.IGNORECASE)),
    ("runtime_directory_failed", re.compile(r"resolve managed (?:runtime|control socket) directory", re.IGNORECASE)),
    ("inspection_failed", re.compile(r"read-only Tailscale inspection failed|Tailscale status/Serve inspection failed", re.IGNORECASE)),
    ("local_readiness_failed", re.compile(r"local relay (?:inventory(?:, UDP,)? or backend readiness|readiness) is incomplete", re.IGNORECASE)),
    ("herdr_workspace_poll_failed", re.compile(r"workspace inventory poll failed", re.IGNORECASE)),
    ("herdr_inventory_poll_failed", re.compile(r"inventory poll failed", re.IGNORECASE)),
    ("managed_owner_changed_root", re.compile(r"managedstate: canonical root identity changed", re.IGNORECASE)),
    ("managed_owner_foreign_state", re.compile(r"managedstate: foreign managed state", re.IGNORECASE)),
    ("managed_owner_invalid_record", re.compile(r"managedstate: invalid managed record", re.IGNORECASE)),
    ("managed_owner_unknown_authority", re.compile(r"managedstate: unknown managed authority", re.IGNORECASE)),
    ("managed_owner_retained_evidence", re.compile(r"managedstate: retained managed evidence", re.IGNORECASE)),
    ("managed_owner_busy", re.compile(r"managedstate: lock held by another owner or acquisition", re.IGNORECASE)),
    ("managed_owner_timeout", re.compile(r"managedstate: bounded transaction contention expired", re.IGNORECASE)),
    ("managed_owner_random_failure", re.compile(r"managedstate: random source failure", re.IGNORECASE)),
    ("managed_owner_io_failure", re.compile(r"managedstate: filesystem operation failed", re.IGNORECASE)),
    ("managed_owner_unavailable", re.compile(r"managed owner is unavailable|managed owner is not acquired", re.IGNORECASE)),
    ("inventory_failed", re.compile(r"(?:initial inventory|inventory (?:initialization|startup|refresh)) failed", re.IGNORECASE)),
    ("bootstrap_failed", re.compile(r"bootstrap", re.IGNORECASE)),
    ("session_authority_failed", re.compile(r"construct Tailscale session authority|Tailscale foreground route", re.IGNORECASE)),
    ("owner_validation_marker", re.compile(r"managed owner validation failed(?: before Tailscale retirement)?|managed owner is (?:unavailable|not acquired)", re.IGNORECASE)),
)


def managed_private_log_category(env: dict[str, str]) -> str:
    env_file = env.get("HERDR_RELAY_ENV")
    if not env_file:
        return "private_log_not_configured"
    candidates = sorted(Path(env_file).parent.glob(".tailscale-relay-log.*"))
    if not candidates:
        return "private_log_absent"
    try:
        latest = candidates[-1]
        with latest.open("rb") as source:
            size = latest.stat().st_size
            if size > 65536:
                source.seek(size - 65536)
            private_log = source.read(65536).decode("utf-8", "ignore")
    except OSError:
        return "private_log_unreadable"
    for category, pattern in PRIVATE_LOG_PATTERNS:
        if pattern.search(private_log):
            return category
    return "private_log_unclassified"


def managed_launcher_exit_code(env: dict[str, str], launcher_log_category: str = "") -> str:
    operations = safe_fixture_cli_operations(env)
    if "unsupported" in operations:
        return "managed_launcher_cli_refusal"
    if "serve_status_json" not in operations:
        return "managed_launcher_pre_serve_status_exit"
    try:
        state_path = Path(env["HERDR_FIXTURE_API_STATE"])
        with state_path.open("rb") as source:
            events = json.loads(source.read(16384)).get("events", [])
    except (KeyError, OSError, UnicodeError, ValueError, TypeError):
        events = []
    if not isinstance(events, list):
        events = []
    if "localapi:watch:mask=2" not in events:
        category = launcher_log_category or managed_private_log_category(env)
        return PRIVATE_LOG_CATEGORY_CODES.get(category, "managed_launcher_pre_watch_exit")
    if "localapi:config:post" not in events:
        return "managed_launcher_registration_state_unconfirmed"
    return "managed_launcher_post_registration_exit"


def die(
    message: str, code: str = "fixture_assertion", launcher_log_category: str = "",
    launcher_stderr_phase: str = "",
) -> "NoReturn":
    del message  # diagnostics remain code-only in stdout/stderr and artifacts
    raise GateFailure(code, launcher_log_category, launcher_stderr_phase)


def managed_launcher_stderr_phase(private_stderr: bytes) -> str:
    if not private_stderr:
        return "stderr_absent"
    text = private_stderr.decode("utf-8", "ignore")
    for phase, pattern in LAUNCHER_STDERR_PATTERNS:
        if pattern.search(text):
            return phase
    return "stderr_unclassified"


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
    def __init__(self, path: Path, relay_port: int, expected_version: str, expected_revision: str, expected_origin: str):
        self.path = path
        self.relay_port = relay_port
        self.expected_version = expected_version
        self.expected_revision = expected_revision
        self.expected_origin = expected_origin
        self.lock = threading.RLock()
        self.config: dict = {}
        self.registration_readyz_probes: list[dict[str, int | str]] = []
        self.registration_healthz_probes: list[dict[str, bool | int | str]] = []
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

    def probe_local_readyz_after_registration(self) -> None:
        probe: dict[str, int | str] = {"trigger": "registration_post", "result": "other"}
        connection = http.client.HTTPConnection("127.0.0.1", self.relay_port, timeout=0.5)
        try:
            connection.request("GET", "/readyz", headers={"Connection": "close"})
            response = connection.getresponse()
            status: int | str = response.status if 100 <= response.status <= 599 else "other"
            body = response.read(4097)
            readiness = "other"
            if len(body) <= 4096:
                try:
                    value = json.loads(body).get("status")
                except (ValueError, AttributeError, TypeError):
                    value = None
                if value in {"ready", "unavailable"}:
                    readiness = value
            probe = {"trigger": "registration_post", "http_status": status, "readiness": readiness}
        except (socket.timeout, TimeoutError):
            probe["result"] = "timeout"
        except ConnectionRefusedError:
            probe["result"] = "connection_refused"
        except ConnectionResetError:
            probe["result"] = "connection_reset"
        except (OSError, http.client.HTTPException):
            probe["result"] = "other"
        finally:
            connection.close()
        with self.lock:
            if len(self.registration_readyz_probes) < 8:
                self.registration_readyz_probes.append(probe)

    def probe_local_healthz_after_registration(self) -> None:
        probe: dict[str, bool | int | str] = {"trigger": "registration_post", "result": "other"}
        connection = http.client.HTTPConnection("127.0.0.1", self.relay_port, timeout=0.5)
        try:
            connection.request("GET", "/healthz", headers={"Connection": "close"})
            response = connection.getresponse()
            status: int | str = response.status if 100 <= response.status <= 599 else "other"
            body = response.read(65537)
            if status != 200:
                probe = {"trigger": "registration_post", "http_status": status, "result": "http_error"}
            elif len(body) > 65536:
                probe = {"trigger": "registration_post", "http_status": status, "result": "body_too_large"}
            else:
                try:
                    health = json.loads(body)
                except (ValueError, TypeError):
                    health = None
                if isinstance(health, dict):
                    probe = {
                        "trigger": "registration_post",
                        "http_status": status,
                        "result": "response",
                        "health_status_ok": health.get("status") == "ok",
                        "readiness_ready": health.get("readiness") == "ready",
                        "transport_tailscale": health.get("transport") == "tailscale",
                        "version_matches": health.get("version") == self.expected_version,
                        "revision_matches": health.get("revision") == self.expected_revision,
                        "bundle_version_matches": health.get("bundle_version") == self.expected_version,
                        "bundle_revision_matches": health.get("bundle_revision") == self.expected_revision,
                        "origin_matches": health.get("tailscale_origin") == self.expected_origin,
                    }
                else:
                    probe = {"trigger": "registration_post", "http_status": status, "result": "invalid_json"}
        except (socket.timeout, TimeoutError):
            probe["result"] = "timeout"
        except ConnectionRefusedError:
            probe["result"] = "connection_refused"
        except ConnectionResetError:
            probe["result"] = "connection_reset"
        except (OSError, http.client.HTTPException):
            probe["result"] = "other"
        finally:
            connection.close()
        with self.lock:
            if len(self.registration_healthz_probes) < 8:
                self.registration_healthz_probes.append(probe)


class HerdrSocketFixture:
    """Faithful disposable implementation of the production Herdr socket API."""

    METHODS = {
        "ping", "agent.list", "pane.list", "workspace.list", "tab.list",
        "session.snapshot", "events.subscribe", "pane.read",
    }

    def __init__(self, path: Path, scenario_path: Path, operations_path: Path):
        scenario = json.loads(scenario_path.read_text(encoding="utf-8"))
        self.panes = scenario.get("panes") or []
        self.tabs = scenario.get("tabs") or []
        self.workspaces = scenario.get("workspaces") or []
        self.content = scenario.get("content") or {}
        self.path = path
        self.operations_path = operations_path
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.path.exists() or self.path.is_symlink():
            raise FileExistsError("selected Herdr fixture socket path already exists")
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(str(self.path))
        os.chmod(self.path, 0o600)
        self.listener.listen(8)
        self.listener.settimeout(0.25)
        self.stopped = threading.Event()
        self.lock = threading.RLock()
        self.counts: dict[tuple[str, str], int] = {}
        self.connections: set[socket.socket] = set()
        self.workers: list[threading.Thread] = []
        self.thread = threading.Thread(target=self._serve, name="fixture-herdr-socket", daemon=True)
        self.thread.start()
        write_json(self.operations_path, self.operation_summary())

    def operation_summary(self) -> list[dict[str, str | int]]:
        with self.lock:
            return [
                {"method": method, "outcome": outcome, "count": count}
                for (method, outcome), count in sorted(self.counts.items())
            ]

    def _record(self, method: str, outcome: str) -> None:
        safe_method = method if method in self.METHODS else "other"
        safe_outcome = outcome if outcome in {"succeeded", "failed"} else "other"
        with self.lock:
            key = (safe_method, safe_outcome)
            self.counts[key] = self.counts.get(key, 0) + 1
            write_json(self.operations_path, self.operation_summary())

    def wait_ready(self, timeout: float = 5.0) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            probe: socket.socket | None = None
            try:
                if not stat.S_ISSOCK(self.path.stat().st_mode):
                    time.sleep(0.05)
                    continue
                probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                probe.settimeout(0.5)
                probe.connect(str(self.path))
                request = {"id": "package-fixture-readiness", "method": "ping", "params": {}}
                probe.sendall(json.dumps(request, separators=(",", ":")).encode() + b"\n")
                response = bytearray()
                while b"\n" not in response and len(response) <= 65536:
                    chunk = probe.recv(4096)
                    if not chunk:
                        break
                    response.extend(chunk)
                decoded = json.loads(bytes(response).split(b"\n", 1)[0])
                result = decoded.get("result", {})
                if decoded.get("id") == request["id"] and result.get("type") == "pong":
                    return
            except (OSError, ValueError, TypeError, AttributeError):
                time.sleep(0.05)
            finally:
                if probe is not None:
                    probe.close()
        raise TimeoutError("selected Herdr fixture socket did not become protocol-ready")

    def _serve(self) -> None:
        while not self.stopped.is_set():
            try:
                connection, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            if self.stopped.is_set():
                connection.close()
                return
            with self.lock:
                self.connections.add(connection)
                worker = threading.Thread(target=self._handle, args=(connection,), daemon=True)
                self.workers.append(worker)
            worker.start()

    def _result(self, method: str, request: dict) -> dict:
        if method == "ping":
            return {
                "type": "pong", "version": "0.9.0", "protocol": 1,
                "capabilities": {"endpoint_protocol_generation": 1},
            }
        if method == "agent.list":
            return {"type": "agent_list", "agents": self.panes}
        if method == "pane.list":
            return {"type": "pane_list", "panes": self.panes}
        if method == "workspace.list":
            return {"type": "workspace_list", "workspaces": self.workspaces}
        if method == "tab.list":
            return {"type": "tab_list", "tabs": self.tabs}
        if method == "session.snapshot":
            return {"type": "session_snapshot", "snapshot": {
                "version": "0.9.0", "protocol": 1, "workspaces": self.workspaces,
                "tabs": self.tabs, "panes": self.panes, "agents": self.panes,
            }}
        if method == "events.subscribe":
            return {"type": "subscription_started"}
        if method == "pane.read":
            params = request.get("params")
            pane_id = params.get("pane_id") if isinstance(params, dict) else None
            text = self.content.get(pane_id, "Harmless package fixture output")
            return {"type": "pane_read", "read": {"text": text, "truncated": False}}
        raise ValueError("unsupported fixture method")

    def _handle(self, connection: socket.socket) -> None:
        method = "other"
        responded = False
        try:
            with connection:
                connection.settimeout(5.0)
                request_line = bytearray()
                while b"\n" not in request_line and len(request_line) <= 1024 * 1024:
                    chunk = connection.recv(4096)
                    if not chunk:
                        return
                    request_line.extend(chunk)
                request = json.loads(bytes(request_line).split(b"\n", 1)[0])
                method_value = request.get("method") if isinstance(request, dict) else None
                method = method_value if isinstance(method_value, str) else "other"
                request_id = request.get("id", "") if isinstance(request, dict) else ""
                if method not in self.METHODS:
                    self._record(method, "failed")
                    response = {"id": request_id, "error": {
                        "code": "unknown_method", "message": "fixture method unavailable",
                    }}
                else:
                    result = self._result(method, request)
                    response = {"id": request_id, "result": result}
                connection.sendall(json.dumps(response, separators=(",", ":")).encode() + b"\n")
                responded = True
                if method in self.METHODS:
                    self._record(method, "succeeded")
                if method == "events.subscribe":
                    connection.settimeout(None)
                    while connection.recv(4096):
                        pass
        except (OSError, ValueError, TypeError, AttributeError):
            if method in self.METHODS and not responded:
                self._record(method, "failed")
        finally:
            with self.lock:
                self.connections.discard(connection)

    def close(self) -> None:
        self.stopped.set()
        try:
            self.listener.close()
        except OSError:
            pass
        with self.lock:
            connections = list(self.connections)
            workers = list(self.workers)
        for connection in connections:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                connection.close()
            except OSError:
                pass
        self.thread.join(timeout=3)
        for worker in workers:
            worker.join(timeout=3)
        try:
            self.path.unlink()
        except FileNotFoundError:
            pass


class UnixHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    address_family = socket.AF_UNIX
    daemon_threads = True
    allow_reuse_address = False

    def server_bind(self) -> None:
        self.socket.bind(self.server_address)
        self.server_name = "local-tailscaled.sock"
        self.server_port = 80

    def handle_error(self, _request: object, _client_address: object) -> None:
        return


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
                self.wfile.write((json.dumps({"Version": TS_VERSION, "SessionID": WATCH_ID}) + "\n").encode())
                self.wfile.flush()
                while not fixture.watch_closed.wait(0.1):
                    self.wfile.write((json.dumps({"Version": TS_VERSION}) + "\n").encode())
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
        registration_post_committed = False
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
                expected_proxy = f"http://127.0.0.1:{fixture.relay_port}"
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
            # The pinned daemon decodes into ipn.ServeConfig, whose
            # Foreground field is omitempty on subsequent LocalAPI GETs.
            # Preserve the submitted route for the removal assertion above,
            # then model the daemon's canonical empty configuration.
            if next_config.get("Foreground") == {}:
                del next_config["Foreground"]
            fixture.config = next_config
            fixture.etag = hashlib.sha256(body).hexdigest()
            registration_post_committed = WATCH_ID in (next_config.get("Foreground") or {})
            fixture.persist()
            if registration_post_committed and fixture.drop_registration_ack:
                fixture.drop_registration_ack = False
                drop_response = True
        if registration_post_committed:
            fixture.probe_local_readyz_after_registration()
            fixture.probe_local_healthz_after_registration()
        if drop_response:
            fixture.event("localapi:ack:dropped-after-commit")
            self.close_connection = True
            return
        self._headers(200, b"")


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
  operation = "status_json"
elif args == ["version", "--json", "--daemon"]:
  operation = "version_daemon_json"
elif args == ["serve", "status", "--json"]:
  operation = "serve_status_json"
else:
  operation = "unsupported"
try:
  with open(os.environ["HERDR_FIXTURE_CLI_EVENTS"], "a", encoding="ascii") as events:
    events.write(operation + "\n")
except OSError:
  pass
if operation == "status_json":
  print(json.dumps(status,separators=(",",":"))); raise SystemExit(0)
if operation == "version_daemon_json":
  print(json.dumps(version,separators=(",",":"))); raise SystemExit(0)
if operation == "serve_status_json":
  print(json.dumps(config,separators=(",",":")))
  raise SystemExit(0)
# No CLI write operation is part of managed ownership; fail any attempt.
raise SystemExit(97)
'''


class PublicRequestState:
    METHODS = {"GET", "POST", "HEAD"}
    PATHS = {"/healthz": "healthz", "/release.json": "release.json", "/version.json": "version.json", "/": "root", "/index.html": "index.html"}
    TLS_REASON_LABELS = {
        "CERTIFICATE_VERIFY_FAILED": "certificate_verify_failed",
        "TLSV1_ALERT_UNKNOWN_CA": "alert_unknown_ca",
        "SSLV3_ALERT_UNKNOWN_CA": "alert_unknown_ca",
        "TLSV1_ALERT_CERTIFICATE_UNKNOWN": "alert_certificate_unknown",
        "SSLV3_ALERT_CERTIFICATE_UNKNOWN": "alert_certificate_unknown",
        "TLSV1_ALERT_BAD_CERTIFICATE": "alert_bad_certificate",
        "SSLV3_ALERT_BAD_CERTIFICATE": "alert_bad_certificate",
        "TLSV1_ALERT_HANDSHAKE_FAILURE": "alert_handshake_failure",
        "SSLV3_ALERT_HANDSHAKE_FAILURE": "alert_handshake_failure",
        "TLSV1_ALERT_CERTIFICATE_REQUIRED": "alert_certificate_required",
        "TLSV1_ALERT_UNSUPPORTED_CERTIFICATE": "alert_unsupported_certificate",
        "SSLV3_ALERT_UNSUPPORTED_CERTIFICATE": "alert_unsupported_certificate",
        "TLSV1_ALERT_CERTIFICATE_EXPIRED": "alert_certificate_expired",
        "TLSV1_ALERT_CERTIFICATE_REVOKED": "alert_certificate_revoked",
        "TLSV1_ALERT_INTERNAL_ERROR": "alert_internal_error",
        "SSLV3_ALERT_INTERNAL_ERROR": "alert_internal_error",
        "TLSV1_ALERT_DECODE_ERROR": "alert_decode_error",
        "TLSV1_ALERT_DECRYPT_ERROR": "alert_decrypt_error",
        "TLSV1_ALERT_DECRYPTION_FAILED": "alert_decrypt_error",
        "TLSV1_ALERT_ILLEGAL_PARAMETER": "alert_illegal_parameter",
        "TLSV1_ALERT_NO_RENEGOTIATION": "alert_no_renegotiation",
        "TLSV1_ALERT_USER_CANCELLED": "alert_user_cancelled",
        "TLSV1_ALERT_EXPORT_RESTRICTION": "alert_export_restriction",
        "SSLV3_ALERT_NO_CERTIFICATE": "alert_no_certificate",
        "SSLV3_ALERT_UNEXPECTED_MESSAGE": "alert_unexpected_message",
        "SSLV3_ALERT_DECOMPRESSION_FAILURE": "alert_decompression_failure",
        "TLSV1_ALERT_PROTOCOL_VERSION": "alert_protocol_version",
        "TLSV1_ALERT_ACCESS_DENIED": "alert_access_denied",
        "TLSV1_ALERT_INSUFFICIENT_SECURITY": "alert_insufficient_security",
        "TLSV1_ALERT_NO_APPLICATION_PROTOCOL": "alert_no_application_protocol",
        "SSLV3_ALERT_BAD_RECORD_MAC": "alert_bad_record_mac",
        "TLSV1_UNRECOGNIZED_NAME": "alert_unrecognized_name",
        "WRONG_VERSION_NUMBER": "wrong_version",
        "UNKNOWN_PROTOCOL": "unknown_protocol",
        "NO_SHARED_CIPHER": "no_shared_cipher",
        "HANDSHAKE_FAILURE": "handshake_failure",
        "UNEXPECTED_EOF_WHILE_READING": "unexpected_eof",
        "HTTP_REQUEST": "plaintext_http",
        "HTTPS_PROXY_REQUEST": "https_proxy_request",
    }

    def __init__(self):
        self.lock = threading.Lock()
        self.requests: dict[tuple[str, str], int] = {}
        self.responses: dict[tuple[str, str, int | str], int] = {}
        self.tls_accept_attempts = 0
        self.tls_handshake_successes = 0
        self.tls_failure_counts: dict[str, int] = {}

    def _kinds(self, method: str, request_path: str) -> tuple[str, str]:
        path = urllib.parse.urlsplit(request_path).path
        path_kind = self.PATHS.get(path)
        if path_kind is None:
            path_kind = "asset" if path.startswith(("/assets/", "/static/")) else "other"
        return (method if method in self.METHODS else "other", path_kind)

    def record(self, method: str, request_path: str) -> None:
        key = self._kinds(method, request_path)
        with self.lock:
            self.requests[key] = self.requests.get(key, 0) + 1

    def record_response(self, method: str, request_path: str, status: int) -> None:
        method_kind, path_kind = self._kinds(method, request_path)
        status_kind: int | str = status if 100 <= status <= 599 else "other"
        with self.lock:
            key = (method_kind, path_kind, status_kind)
            self.responses[key] = self.responses.get(key, 0) + 1

    def record_tls_accept_attempt(self) -> None:
        with self.lock:
            self.tls_accept_attempts += 1

    def record_tls_handshake_success(self) -> None:
        with self.lock:
            self.tls_handshake_successes += 1

    def record_tls_handshake_failure(self, reason: object) -> None:
        reason_label = self.TLS_REASON_LABELS.get(reason) if isinstance(reason, str) else None
        if reason_label is None and isinstance(reason, str) and re.fullmatch(
            r"(?:SSLV3|TLSV1|TLSV1_2|TLSV1_3)_ALERT_[A-Z0-9_]{1,48}", reason,
        ):
            reason_label = "unlisted_tls_alert"
        if reason_label is None:
            reason_label = "other"
        with self.lock:
            self.tls_failure_counts[reason_label] = self.tls_failure_counts.get(reason_label, 0) + 1

    def tls_summary(self) -> dict[str, object]:
        with self.lock:
            failures = sum(self.tls_failure_counts.values())
            return {
                "accept_attempts": self.tls_accept_attempts,
                "handshake_successes": self.tls_handshake_successes,
                "ssl_errors": [
                    {"reason": reason, "count": count}
                    for reason, count in sorted(self.tls_failure_counts.items())
                ],
                "accepts_without_tls_result": max(
                    0, self.tls_accept_attempts - self.tls_handshake_successes - failures,
                ),
            }

    def operation_summary(self) -> list[dict[str, object]]:
        with self.lock:
            return [
                {
                    "method": method,
                    "path": path,
                    "request_count": count,
                    "responses": [
                        {"status": status, "count": response_count}
                        for (response_method, response_path, status), response_count in sorted(
                            self.responses.items(), key=lambda item: (item[0][0], item[0][1], str(item[0][2])),
                        )
                        if (response_method, response_path) == (method, path)
                    ],
                }
                for (method, path), count in sorted(self.requests.items())
            ]


class PublicHTTPServer(http.server.ThreadingHTTPServer):
    def get_request(self):
        self.fixture.record_tls_accept_attempt()  # type: ignore[attr-defined]
        request = None
        try:
            request, client_address = super().get_request()
            request.do_handshake()
        except ssl.SSLError as error:
            if request is not None:
                try:
                    request.close()
                except OSError:
                    pass
            self.fixture.record_tls_handshake_failure(error.reason)  # type: ignore[attr-defined]
            raise
        self.fixture.record_tls_handshake_success()  # type: ignore[attr-defined]
        return request, client_address

    def handle_error(self, _request: object, _client_address: object) -> None:
        return


class PublicHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = ""
    sys_version = ""

    def log_message(self, _format: str, *_args: object) -> None:
        return

    @property
    def request_state(self) -> PublicRequestState:
        return self.server.fixture  # type: ignore[attr-defined]

    def _websocket(self) -> None:
        backend = socket.create_connection(("127.0.0.1", RELAY_PORT), timeout=10)
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
        status_line = bytes(response).split(b"\r\n", 1)[0]
        status_match = re.match(rb"HTTP/1\.[01] ([1-5][0-9][0-9])(?:[ \t]|$)", status_line)
        if status_match:
            self.request_state.record_response(self.command, self.path, int(status_match.group(1)))
        self.connection.sendall(response)
        # The ten-second dial/upgrade bound must not become an idle timeout on
        # the long-lived E2EE WebSocket. A quiet controller is still connected.
        backend.settimeout(None)

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
        connection = http.client.HTTPConnection("127.0.0.1", RELAY_PORT, timeout=15)
        request_headers = {key: value for key, value in self.headers.items() if key.lower() not in {"host", "connection", "content-length"}}
        request_headers["Host"] = self.headers.get("Host", HOST)
        body = self.rfile.read(int(self.headers.get("Content-Length", "0"))) if self.headers.get("Content-Length") else None
        connection.request(self.command, self.path, body=body, headers=request_headers)
        response = connection.getresponse()
        self.request_state.record_response(self.command, self.path, response.status)
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
        self.request_state.record(self.command, self.path)
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
        self.request_state.record(self.command, self.path)
        try:
            self._proxy()
        except (OSError, http.client.HTTPException):
            self.close_connection = True


# http.client is intentionally imported after the fixture-only class declaration
# to make static reviewers see the narrow proxy dependency.
import http.client


def start_public_server(certificate: Path, key: Path) -> http.server.ThreadingHTTPServer:
    server = PublicHTTPServer(("127.0.0.1", int(os.environ["HERDR_TAILSCALE_HTTPS_PORT"])), PublicHandler)
    server.fixture = PublicRequestState()  # type: ignore[attr-defined]
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(str(certificate), str(key))
    server.socket = context.wrap_socket(server.socket, server_side=True, do_handshake_on_connect=False)
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


def prepare_chromium_nss_trust(ca_certificate: Path, browser_home: Path) -> None:
    nss_database = browser_home / ".pki" / "nssdb"
    try:
        nss_database.mkdir(mode=0o700, parents=True)
        database_arg = f"sql:{nss_database}"
        subprocess.run(
            ["certutil", "-N", "-d", database_arg, "--empty-password"],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            check=True, timeout=15,
        )
        subprocess.run(
            [
                "certutil", "-A", "-d", database_arg, "-n", "Herdr disposable package fixture CA",
                "-t", "C,,", "-i", str(ca_certificate),
            ],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            check=True, timeout=15,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        die("isolated Chromium NSS trust store setup failed", "fixture_browser_nss_setup_failed")


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


def launch_managed(package: Path, env: dict[str, str], timeout: float = 90, expect_link: bool = True) -> tuple[subprocess.Popen[bytes], str, bytes, bytes, str]:
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
    state: dict[str, object] = {
        "link": None, "stderr_bytes": 0, "stderr_private": bytearray(), "output_limit": False,
    }
    launcher_log_category = "private_log_not_observed"
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
                        private_stderr = state["stderr_private"]
                        if isinstance(private_stderr, bytearray):
                            private_stderr.extend(chunk)
                            if len(private_stderr) > 65536:
                                del private_stderr[:len(private_stderr) - 65536]
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
    def stop_phase() -> str:
        with lock:
            private = bytes(state["stderr_private"])
        text = private.decode("utf-8", "ignore")
        for phrase, category in (
            ("Private retirement control is unavailable", "control_unavailable"),
            ("Authenticated retirement did not separately acknowledge", "retirement_ack_missing"),
            ("Relay did not exit after retirement acknowledgement", "relay_exit_missing"),
            ("Managed Tailscale readiness or exact route changed", "readiness_changed"),
            ("Foreground managed relay stopped", "child_exited"),
        ):
            if phrase in text:
                return category
        return "no_matching_launcher_error"
    process.fixture_stop_phase = stop_phase  # type: ignore[attr-defined]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        observed_log_category = managed_private_log_category(env)
        if (observed_log_category in PRIVATE_LOG_CATEGORY_CODES
                or observed_log_category in PRIVATE_LOG_STATUS_CATEGORIES):
            launcher_log_category = observed_log_category
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
            die("launcher output exceeded the package acceptance bound", "managed_launcher_output_limit", launcher_log_category)
        if link:
            return process, str(link), bytes(output), b"", launcher_log_category
        if process.poll() is not None and all(event.wait(0.05) for event in done):
            observed_log_category = managed_private_log_category(env)
            if (observed_log_category in PRIVATE_LOG_CATEGORY_CODES
                    or observed_log_category in PRIVATE_LOG_STATUS_CATEGORIES):
                launcher_log_category = observed_log_category
            with lock:
                captured = bytes(output)
                stderr_bytes = int(state["stderr_bytes"])
                private_stderr = bytes(state["stderr_private"])
            if not expect_link:
                return process, "", captured, b"", launcher_log_category
            die(
                f"packaged managed launcher exited before setup-link emission (exit={process.returncode}, stderr_bytes={stderr_bytes})",
                managed_launcher_exit_code(env, launcher_log_category), launcher_log_category,
                managed_launcher_stderr_phase(private_stderr),
            )
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
        private_stderr = bytes(state["stderr_private"])
    die(
        f"packaged managed launcher timed out (stderr_bytes={stderr_bytes})",
        "managed_launcher_link_timeout", launcher_log_category,
        managed_launcher_stderr_phase(private_stderr),
    )


def launch_managed_rejection(package: Path, env: dict[str, str], timeout: float = 90) -> tuple[subprocess.Popen[bytes], bytes]:
    process, link, stdout, _, _ = launch_managed(package, env, timeout, expect_link=False)
    if link or process.poll() is None or process.returncode == 0:
        stop_launcher(process)
        die("managed launcher rejection was not bounded, nonzero, and link-free")
    return process, stdout


launcher_stop_outcomes: list[dict[str, str | int]] = []


def record_launcher_stop(outcome: str, process: subprocess.Popen[bytes]) -> None:
    returncode = process.poll()
    if len(launcher_stop_outcomes) >= 4:
        return
    exit_class = "unknown"
    if returncode == 130:
        exit_class = "interrupt_130"
    elif returncode == 0:
        exit_class = "zero"
    elif returncode is not None:
        exit_class = "signal" if returncode < 0 else "other_nonzero"
    phase = getattr(process, "fixture_stop_phase", lambda: "no_matching_launcher_error")()
    record: dict[str, str | int] = {"outcome": outcome, "exit_class": exit_class, "launcher_phase": phase}
    if returncode is not None and -255 <= returncode <= 255:
        record["exit_code"] = returncode
    launcher_stop_outcomes.append(record)


def stop_launcher(process: subprocess.Popen[bytes]) -> bool:
    if process.poll() is not None:
        record_launcher_stop("exited_before_interrupt", process)
        return False
    try:
        os.killpg(process.pid, 2)
    except ProcessLookupError:
        record_launcher_stop("process_group_missing", process)
        return False
    try:
        process.wait(timeout=20)
        record_launcher_stop("interrupted", process)
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
        record_launcher_stop("interrupt_timeout", process)
        return False


browser_stage = "browser_runner"
browser_exception_type = ""
browser_progress_record: dict[str, object] = {
    "mode": "other", "result": "unknown", "stage": "browser_runner",
    "passed_cases": [], "profiles": [], "exception_type": "",
}


def safe_browser_profile(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    profile_name = value.get("profile")
    if not isinstance(profile_name, str) or profile_name not in BROWSER_PROFILE_NAMES:
        return None
    storage_records = value.get("storage")
    storage = []
    if isinstance(storage_records, list):
        for snapshot in storage_records[:8]:
            if not isinstance(snapshot, dict):
                continue
            checkpoint = snapshot.get("checkpoint")
            if not isinstance(checkpoint, str) or checkpoint not in BROWSER_STORAGE_CHECKPOINTS:
                continue
            keys = {}
            for key in ("device_auth", "relays"):
                item = snapshot.get(key)
                if not isinstance(item, dict):
                    continue
                value_type = item.get("type")
                present = item.get("present")
                if isinstance(value_type, str) and value_type in BROWSER_STORAGE_TYPES and isinstance(present, bool):
                    keys[key] = {"present": present, "type": value_type}
            storage.append({
                "checkpoint": checkpoint,
                "available": snapshot.get("available") is True,
                "keys": keys,
            })

    ui_records = value.get("ui_snapshots")
    ui_snapshots = []
    if isinstance(ui_records, list):
        for snapshot in ui_records[:8]:
            if not isinstance(snapshot, dict):
                continue
            checkpoint = snapshot.get("checkpoint")
            if not isinstance(checkpoint, str) or checkpoint not in BROWSER_UI_CHECKPOINTS:
                continue
            tones = snapshot.get("status_tones")
            safe_tones = {
                tone: count for tone, count in tones.items()
                if tone in {"danger", "warning", "success", "muted"}
                and type(count) is int and 0 <= count <= 1000
            } if isinstance(tones, dict) else {}
            def enum_value(field: str, allowed: set[str], default: str) -> str:
                candidate = snapshot.get(field)
                return candidate if isinstance(candidate, str) and candidate in allowed else default

            ui_snapshot = {
                "checkpoint": checkpoint,
                "view": enum_value("view", BROWSER_UI_VIEWS, "other"),
                "header_tone": enum_value("header_tone", BROWSER_STATUS_TONES, "unknown"),
                "connection_state": enum_value("connection_state", BROWSER_CONNECTION_STATES, "unknown"),
                "inventory_state": enum_value("inventory_state", BROWSER_INVENTORY_STATES, "not_reported"),
                "active_agent_status": enum_value("active_agent_status", BROWSER_AGENT_STATES, "not_applicable"),
                "send_action_state": enum_value("send_action_state", BROWSER_SEND_ACTION_STATES, "missing"),
                "status_tones": safe_tones,
            }
            for field in (
                "prompt_inputs", "enabled_prompt_inputs", "disabled_prompt_inputs",
                "send_prompt_buttons", "submit_terminal_text_buttons", "submitting_input_buttons",
                "agent_cards", "open_buttons", "enabled_open_buttons", "disabled_open_buttons",
                "stale_agent_cards", "connected_relays", "configured_relays",
            ):
                count = snapshot.get(field)
                if type(count) is int and 0 <= count <= 1000:
                    ui_snapshot[field] = count
            ui_snapshots.append(ui_snapshot)

    def safe_counts(name: str) -> dict[str, int]:
        counts = value.get(name)
        if not isinstance(counts, dict):
            return {}
        return {
            category: count for category, count in counts.items()
            if category in BROWSER_DIAGNOSTIC_CATEGORIES and type(count) is int and 0 <= count <= 16
        }

    return {
        "profile": profile_name,
        "storage": storage,
        "ui_snapshots": ui_snapshots,
        "console_errors": safe_counts("console_errors"),
        "page_errors": safe_counts("page_errors"),
        "navigation_errors": safe_counts("navigation_errors"),
        "websockets": {
            field: count for field, count in value.get("websockets", {}).items()
            if field in {"attempts", "closed", "errors"} and type(count) is int and 0 <= count <= 16
        } if isinstance(value.get("websockets"), dict) else {},
    }


def safe_browser_progress(value: object, mode: str) -> dict[str, object]:
    record = value if isinstance(value, dict) else {}
    stage = record.get("stage")
    passed_cases = record.get("passed_cases")
    profiles = record.get("profiles")
    result = record.get("result")
    exception_type = record.get("exception_type")
    safe_profiles = []
    if isinstance(profiles, list):
        seen = set()
        for profile in profiles[:2]:
            safe_profile = safe_browser_profile(profile)
            if safe_profile and safe_profile["profile"] not in seen:
                seen.add(safe_profile["profile"])
                safe_profiles.append(safe_profile)
    return {
        "mode": mode if mode in {"enroll", "reprint", "restart"} else "other",
        "result": result if isinstance(result, str) and result in {"pass", "fail"} else "unknown",
        "stage": stage if isinstance(stage, str) and stage in BROWSER_STAGES else "browser_runner",
        "passed_cases": [
            name for name in passed_cases if isinstance(name, str) and name in EXPECTED_CASES
        ] if isinstance(passed_cases, list) else [],
        "profiles": safe_profiles,
        "exception_type": exception_type if isinstance(exception_type, str) and exception_type in BROWSER_EXCEPTION_TYPES else "",
    }


def read_browser_progress(path: Path, mode: str) -> dict[str, object]:
    try:
        with path.open("rb") as source:
            value = json.loads(source.read(4096))
    except (OSError, UnicodeError, ValueError):
        value = None
    return safe_browser_progress(value, mode)


def run_playwright(package_root: Path, input_record: dict, mode: str, evidence_path: Path) -> dict:
    global browser_stage, browser_exception_type, browser_progress_record
    script = Path("/workspace/tests/tailscale-package-browser.mjs")
    if not script.is_file():
        browser_stage = "browser_runner"
        browser_exception_type = "BrowserScriptMissing"
        die("browser acceptance script is missing from exact candidate checkout")
    progress_path = package_root.parent / f"browser-progress-{mode}.json"
    try:
        progress_path.unlink()
    except FileNotFoundError:
        pass
    browser_input = {
        **input_record, "mode": mode, "evidence_path": str(evidence_path),
        "progress_path": str(progress_path),
    }
    try:
        completed = subprocess.run(
            ["node", str(script)], input=json.dumps(browser_input).encode(), stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, timeout=180, check=False,
            env={
                **os.environ,
                "HOME": str(package_root.parent / "chromium-home"),
                "PLAYWRIGHT_BROWSERS_PATH": "/ms-playwright",
            },
        )
    except subprocess.TimeoutExpired as error:
        output_record = None
        raw_output = error.stdout
        if isinstance(raw_output, bytes) and len(raw_output) <= 65536:
            try:
                output_record = json.loads(raw_output.decode("utf-8"))
            except (UnicodeError, ValueError):
                output_record = None
        elif isinstance(raw_output, str) and len(raw_output) <= 65536:
            try:
                output_record = json.loads(raw_output)
            except ValueError:
                output_record = None
        if isinstance(output_record, dict) and output_record.get("mode") == mode:
            browser_progress_record = safe_browser_progress(output_record, mode)
        else:
            browser_progress_record = read_browser_progress(progress_path, mode)
        browser_stage = str(browser_progress_record["stage"])
        browser_exception_type = "TimeoutExpired"
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
    browser_progress_record = safe_browser_progress(value, mode)
    browser_stage = str(browser_progress_record["stage"])
    error_value = value.get("exception_type", "")
    browser_exception_type = error_value if isinstance(error_value, str) and error_value in BROWSER_EXCEPTION_TYPES else ""
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
    herdr_socket: HerdrSocketFixture | None = None
    ambiguous_herdr_socket: HerdrSocketFixture | None = None
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
    launcher_log_category = "private_log_not_observed"
    launcher_stderr_phase = ""
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
            f"HERDR_RELAY_REARM_BOOTSTRAP=0\nHERDR_RELAY_PORT={RELAY_PORT}\nHERDR_RELAY_PLUGIN_PORT=18378\n",
            encoding="ascii",
        )
        os.chmod(env_file, 0o600)
        api_state_file = temporary_root / "api-state.json"
        state = APIState(api_state_file, RELAY_PORT, version, source_sha, origin)
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

        cli_events_file = temporary_root / "tailscale-cli-events.log"
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
                "pane_id": "workspace:agent", "terminal_id": "package-terminal",
                "agent": "codex", "name": "Package fixture", "agent_status": "idle",
                "workspace_id": "workspace", "tab_id": "tab",
                "cwd": "/tmp/herdr-package-fixture", "revision": 1,
            }],
            "workspaces": [{"workspace_id": "workspace", "label": "Package fixture"}],
            "tabs": [{"tab_id": "tab", "workspace_id": "workspace", "label": "Package fixture", "cwd": "/tmp/herdr-package-fixture"}],
            "content": {"workspace:agent": "Harmless package fixture output"},
        }), encoding="utf-8")
        herdr_socket_operations = temporary_root / "herdr-socket-operations.json"
        herdr_socket = HerdrSocketFixture(runtime / "herdr.sock", scenario, herdr_socket_operations)
        herdr_socket.wait_ready()
        transitions.append("herdr-socket:ready")
        operations = temporary_root / "fake-herdr-operations.jsonl"
        relay_env = os.environ.copy()
        relay_env.update({
            "HOME": str(home), "TMPDIR": str(temporary_root),
            "XDG_CONFIG_HOME": str(config_dir), "XDG_CACHE_HOME": str(cache),
            "XDG_DATA_HOME": str(data), "HERDR_RELAY_ENV": str(env_file),
            "HERDR_TAILSCALE_REQUEST": "1", "HERDR_TAILSCALE_BIN": str(cli_script),
            "HERDR_TAILSCALE_HTTPS_PORT": os.environ["HERDR_TAILSCALE_HTTPS_PORT"],
            "HERDR_RELAY_PORT": str(state.relay_port),
            "HERDR_BIN": str(fake_herdr), "HERDR_SOCKET_PATH": str(herdr_socket.path),
            "HERDR_RELEASE_ROOT": str(data / "releases"),
            "HERDR_RELAY_POLL_INTERVAL": "1",
            "FAKE_HERDR_SCENARIO": str(scenario), "FAKE_HERDR_OPERATIONS": str(operations),
            "HERDR_FIXTURE_API_STATE": str(api_state_file), "HERDR_FIXTURE_CLI_EVENTS": str(cli_events_file),
            "HERDR_FIXTURE_ORIGIN": origin,
            "CURL_CA_BUNDLE": str(ca), "SSL_CERT_FILE": str(ca),
            "NO_PROXY": "*", "no_proxy": "*", "GITHUB_SHA": source_sha,
        })
        relay_env.pop("HERDR_PHONE_APP_URL", None)
        relay_env.pop("HERDR_RELAY_BIN", None)
        relay_env.pop("HERDR_WEB_ROOT", None)
        package = temporary_root / "release"
        set_stage("managed_launch")
        launcher, link, _private_stdout, _, launcher_log_category = launch_managed(package, relay_env)
        transitions.extend(["preflight:read-only", "watch:mask=2", "route:conditional-register", "launcher:setup-link"])
        if state.events.count("localapi:watch:mask=2") != 1:
            die("fixed production LocalAPI did not record the required watch mask", "fixture_localapi_watch_missing")
        if state.events.count("localapi:config:post") < 1:
            die("fixed production LocalAPI did not record conditional registration", "fixture_localapi_registration_missing")
        if WATCH_ID not in state.config.get("Foreground", {}):
            die("fixed production LocalAPI did not retain the exact session", "fixture_localapi_session_missing")
        if not state.registration_healthz_probes:
            die("registration fixture did not observe local health and bundle identity")
        health_probe = state.registration_healthz_probes[0]
        if (health_probe.get("http_status") != 200 or health_probe.get("result") != "response"
                or any(health_probe.get(key) is not True for key in (
                    "health_status_ok", "readiness_ready", "transport_tailscale", "version_matches",
                    "revision_matches", "bundle_version_matches", "bundle_revision_matches", "origin_matches",
                ))):
            die("exact packaged local health and bundle identity were not ready at route registration", "fixture_registration_health_identity")
        prepare_chromium_nss_trust(ca, temporary_root / "chromium-home")
        browser_record = {
            "setup_url": link, "origin": origin, "profiles": str(temporary_root / "profiles"),
            "fake_herdr_operations": str(operations),
            "herdr_socket_operations": str(herdr_socket_operations),
        }
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
        launcher, restart_link, _restart_stdout, _, restart_log_category = launch_managed(package, relay_env)
        if restart_log_category in PRIVATE_LOG_CATEGORY_CODES:
            launcher_log_category = restart_log_category
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
        # The previous case intentionally retains a foreign route whose
        # operator-owned Text handler the strict read-only parser refuses.
        # Exercise pipe bounding against an independent empty CLI snapshot;
        # never alter or conceal the preserved foreign LocalAPI state.
        held_state_file = temporary_root / "held-inspection-state.json"
        write_json(held_state_file, {"config": {}})
        held_env = dict(relay_env, HERDR_TAILSCALE_BIN=str(held), HERDR_FIXTURE_API_STATE=str(held_state_file))
        set_stage("held_pipe_cleanup")
        started = time.monotonic()
        inspection = subprocess.run(
            [str(binary), "tailscale", "inspect", "--binary", str(held), "--https-port", os.environ["HERDR_TAILSCALE_HTTPS_PORT"]],
            env=held_env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=8, check=False,
        )
        elapsed = time.monotonic() - started
        try:
            child_pid = int(child_file.read_text(encoding="ascii").strip())
        except (OSError, ValueError):
            die("held-pipe shim did not spawn the inherited-pipe descendant")
        try:
            os.kill(child_pid, 15)
        except ProcessLookupError:
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
            f"HERDR_RELAY_REARM_BOOTSTRAP=0\nHERDR_RELAY_PORT={RELAY_PORT}\nHERDR_RELAY_PLUGIN_PORT=18378\n",
            encoding="ascii",
        )
        os.chmod(ambiguous_env_file, 0o600)
        ambiguous_state_file = ambiguous_root / "api-state.json"
        state = APIState(ambiguous_state_file, RELAY_PORT, version, source_sha, origin)
        state.drop_registration_ack = True
        state.persist()
        local_server.fixture = state  # type: ignore[attr-defined]
        ambiguous_operations = ambiguous_root / "fake-herdr-operations.jsonl"
        ambiguous_socket_operations = ambiguous_root / "herdr-socket-operations.json"
        ambiguous_herdr_socket = HerdrSocketFixture(
            ambiguous_runtime / "herdr.sock", scenario, ambiguous_socket_operations,
        )
        ambiguous_herdr_socket.wait_ready()
        ambiguous_env = dict(relay_env)
        ambiguous_env.update({
            "HOME": str(ambiguous_home), "TMPDIR": str(ambiguous_root),
            "HERDR_RELAY_PORT": str(state.relay_port),
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
        if isinstance(error, GateFailure) and error.launcher_log_category:
            launcher_log_category = error.launcher_log_category
        if isinstance(error, GateFailure) and error.launcher_stderr_phase:
            launcher_stderr_phase = error.launcher_stderr_phase
        print(f"extracted package acceptance failed: stage={current_stage} code={failure_code}", file=sys.stderr)
        raise
    finally:
        if launcher is not None:
            stop_launcher(launcher)
        if public_server is not None:
            public_server.shutdown()
            public_server.server_close()
        if state is not None:
            with state.lock:
                localapi_events = list(state.events)
                registration_readyz_probes = list(state.registration_readyz_probes)
                registration_healthz_probes = list(state.registration_healthz_probes)
            transitions.extend(localapi_events[-12:])
        else:
            localapi_events = []
            registration_readyz_probes = []
            registration_healthz_probes = []
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
            "tls_verification": "system-and-isolated-chromium-nss; browser-ignore-disabled",
            "current_stage": current_stage,
            "completed_stages": completed_stages,
            "exception_type": failure_type,
            "failure_code": failure_code,
            "browser_stage": browser_stage,
            "browser_exception_type": browser_exception_type,
            "browser_progress": browser_progress_record,
            "expected_case_count": len(EXPECTED_CASES),
            "executed_case_count": len([name for name in EXPECTED_CASES if case_results.get(name) == "pass"]),
            "cases": [{"name": name, "result": case_results.get(name, "missing")} for name in EXPECTED_CASES],
            "fixture_transitions": list(dict.fromkeys(transitions))[:64],
            "fixture_cli_operations": safe_fixture_cli_operations(relay_env if "relay_env" in locals() else {}),
            "localapi_operation_counts": safe_localapi_event_counts(localapi_events),
            "localapi_first_registration_window_counts": safe_localapi_registration_window(localapi_events),
            "registration_readyz_probes": registration_readyz_probes,
            "registration_healthz_probes": registration_healthz_probes,
            "fake_herdr_operations": safe_fake_herdr_operations(
                (relay_env.get("FAKE_HERDR_OPERATIONS") if "relay_env" in locals() else None),
            ),
            "herdr_socket_operations": herdr_socket.operation_summary() if herdr_socket is not None else [],
            "ambiguous_herdr_socket_operations": (
                ambiguous_herdr_socket.operation_summary() if ambiguous_herdr_socket is not None else []
            ),
            "launcher_log_category": launcher_log_category,
            "launcher_stderr_phase": launcher_stderr_phase,
            "launcher_stop_outcomes": launcher_stop_outcomes,
            "public_http_requests": (
                public_server.fixture.operation_summary() if public_server is not None else []
            ),
            "public_tls_handshakes": (
                public_server.fixture.tls_summary() if public_server is not None else {
                    "accept_attempts": 0, "handshake_successes": 0, "ssl_errors": [],
                    "accepts_without_tls_result": 0,
                }
            ),
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
        if ambiguous_herdr_socket is not None:
            ambiguous_herdr_socket.close()
        if herdr_socket is not None:
            herdr_socket.close()
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

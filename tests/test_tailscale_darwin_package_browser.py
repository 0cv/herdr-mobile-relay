#!/usr/bin/env python3
"""One isolated Darwin/arm64 extracted-package BYO browser cell.

Imports are stdlib-only and inert. Offline checks run before admission or any
project import. This is not managed Serve, host trust, or a zero-egress sandbox.
A source review is not evidence that an actual browser/package qualifies.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
from dataclasses import dataclass
import datetime
import errno
import gzip
import hashlib
import http.client
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import platform
import re
import selectors
import shutil
import signal
import socket
import sqlite3
import ssl
import stat
import subprocess
import sys
import tarfile
import threading
import time
import traceback
from types import MappingProxyType, SimpleNamespace
import urllib.parse
import urllib.request
import zipfile

BASE = "cbacea6efe8916f2a040e6ca4927e221e9b6e15c"
REF = "refs/heads/ci/tailscale-native-preflight-ef9f843"
REPOSITORY = "0cv/herdr-mobile-relay"
PLATFORM = "darwin-isolated-byo-v1"
ORIGIN = "https://127.0.0.1:18443"
FILES = (
    ".github/workflows/tailscale-darwin-package-browser.yml",
    "tests/test_tailscale_darwin_package_browser.py",
    "tests/test_tailscale_extracted_package_browser.py",
    "tests/tailscale-package-browser.mjs",
)
INHERITED = (BASE, 37609261144, 11477250012,
             "cd2de67aaacfcd61d6a8292b34c8f10e2f62760844b8e2572b65eff12132c1ad")
LIMITS = MappingProxyType({
    "zip": 64 * 1024 * 1024, "tar": 64 * 1024 * 1024,
    "zip_count": 128, "tar_count": 16384, "individual": 64 * 1024 * 1024,
    "unpacked": 256 * 1024 * 1024, "dependencies": 1024 * 1024 * 1024,
    "stdin": 65536, "result": 65536, "raw": 2 * 1024 * 1024,
    "receipt": 262144, "total_seconds": 1260,
})
PHASES = (90, 300, 210, 630, 30)
GUARDS = frozenset({"proxy", "herdr_socket", "fake_herdr", "pipe_drain"})
# Admission projection of F3/LAUNCHCTL-FIRST-PUSH-GRANT.json. The expected
# parent is a frozen parent-grant literal, NEVER selected from CI/git observations.
# The earlier grant and authorized:false contract remain historical, unedited.
# Exactly the next reviewed fast-forward and attempt 1; not a durable nonce
# across prohibited branch reset/deletion/force-push/recreation or redispatch.
# Operational admission only, NOT cryptographic hosted-runner authentication:
# a local caller can forge environment/event inputs. The parent grant authorizes
# at most two label-scoped read-only queries ONLY on the isolated hosted runner
# in the single first-fast-forward attempt from the pinned e59e8022 parent.
# No local runtime or local launchctl is authorized, even with fabricated inputs.
LAUNCHCTL_GRANT_ID = "q2-parent-launchctl-print-first-push-e59e8022-20261008"
LAUNCHCTL_PINNED_PARENT = "e59e8022daaefed14317d760f17f904ad6cd55eb"
PUSH_EVENT_CAP = 65536
LAUNCHCTL_SCOPE = MappingProxyType({
    "command": "launchctl print gui/$(id -u)/com.herdr-mobile-relay.service",
    "mode": "read-only query; output discarded", "maxQueries": 2,
    "where": "isolated GitHub-hosted macos-15 Darwin runner only",
    "when": "attempt 1 of the first fast-forward push from " + LAUNCHCTL_PINNED_PARENT,
})
# These are benign only on the client side. Backend calls wrap even these
# exception types as fatal proxy errors before they reach the client boundary.
CLIENT_ABORTS = (ssl.SSLError, ConnectionResetError, BrokenPipeError,
                 ConnectionAbortedError, TimeoutError)
ENROLL = (
    "dev_setup_link_imports_bare_wss_origin_in_extracted_frontend",
    "launcher_generated_setup_link_enrolls_real_controller_profile",
    "controller_reads_fake_inventory_and_sends_harmless_command",
    "second_persistent_profile_enrolls_reader_and_read_only_is_enforced",
)
PRESERVE = ("reprint_and_byo_restart_preserve_enrolled_device_credentials",)
OFFLINE = MappingProxyType({
    "parser": ("valid_result", "malformed_json", "oversize_json", "wrong_mode",
               "wrong_platform", "missing_case", "duplicate_case", "extra_case",
               "exit_disagreement", "invalid_producer", "invalid_budget_or_paths",
               "sensitive_receipt_fields"),
    "archive": ("valid_archive", "traversal", "absolute", "duplicate", "ambiguous",
                "symlink", "hardlink", "special_member", "zip_digest", "tar_digest",
                "checksum", "manifest_target_or_version", "zip_count", "tar_count",
                "individual_bytes", "aggregate_bytes"),
    "phase-caps": ("within_limits", "per_phase_deadline", "global_deadline",
                   "stop_before_next_phase", "stdin_cap", "result_cap", "raw_output_cap",
                   "receipt_cap", "dependency_storage_cap", "hydration_stream_cap",
                   "immutable_production_limits", "injected_operations_refuse_real_effects"),
    "cleanup": ("ordered_success", "failure_cleanup", "handled_signal_cleanup",
                "bounded_escalation_and_reap", "unowned_ids_refused",
                "surviving_children_fail", "surviving_listener_fail", "closure_error_fails",
                "symlink_root_refused", "existing_root_refused"),
})


class Refusal(RuntimeError):
    """Only the internal fixed code, never a sensitive subprocess message, escapes."""


def require(condition, code):
    if not condition:
        raise Refusal(code)


def cap(kind, value):
    require(type(value) is int and 0 <= value <= LIMITS[kind], "cap_" + kind)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def failure_detail(error, failed_command=None):
    """Secret-free locator for a failed cell.

    Records only the exception class, errno, and the innermost function/line in
    this file, plus the failing tool label and exit code. Never messages,
    output, paths or URLs.
    """
    here = os.path.realpath(__file__)
    frames = [frame for frame in traceback.extract_tb(error.__traceback__)
              if os.path.realpath(frame.filename) == here]
    # Locate the caller of the generic refusal helpers, not the helper itself.
    frames = [frame for frame in frames if frame.name not in {"require", "cap"}] or frames
    last = frames[-1] if frames else None
    code = getattr(error, "errno", None)
    detail = {"type": type(error).__name__ if type(error).__name__ in SAFE_EXCEPTION_TYPES else "Unknown",
              "errno": code if type(code) is int else None,
              "function": (last.name if last.name in SAFE_FUNCTIONS else "unknown") if last else None,
              "line": last.lineno if last and type(last.lineno) is int else None,
              "command": None, "exit_code": None}
    if failed_command:
        label, exit_code = failed_command
        detail["command"] = label if label in SAFE_COMMANDS else "unknown"
        detail["exit_code"] = exit_code if type(exit_code) is int else None
    return detail


def command_label(argv):
    """Tool/subcommand label only: basenames of the first three arguments."""
    parts = [re.sub(r"[^A-Za-z0-9_.:-]", "", os.path.basename(str(arg)))[:32] for arg in argv[:3]]
    label = " ".join(part for part in parts if part)[:96]
    return label if label in SAFE_COMMANDS else "unknown"


def read_bounded(path, maximum):
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_size <= maximum,
            "file_identity")
    with path.open("rb") as stream:
        data = stream.read(maximum + 1)
    require(len(data) <= maximum, "file_cap")
    after = path.lstat()
    require((info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) ==
            (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), "file_drift")
    return data


def strict_json(data, kind="result"):
    cap(kind, len(data))

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate_json_key")
            result[key] = value
        return result

    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=pairs,
                          parse_constant=lambda _: (_ for _ in ()).throw(Refusal("json_constant")))
    except (ValueError, UnicodeError, RecursionError):
        raise Refusal("json_invalid") from None


def browser_result(data, mode, exit_code):
    value = strict_json(data)
    allowed = {"mode", "result", "passed_cases", "stage", "profiles", "fixture_platform",
               "owned_contexts_closed", "controller_enrolled", "controller_read",
               "controller_command", "reader_enrolled", "reader_read", "reader_mutation_denied",
               "credentials_preserved", "exception_type", "app_worker_verified", "notification_delivery_disabled", "launch_arguments_verified"}
    require(type(value) is dict and set(value) <= allowed, "browser_fields")
    require(mode in {"enroll", "reprint", "restart"} and value.get("mode") == mode,
            "browser_mode")
    require(value.get("fixture_platform") == PLATFORM, "browser_platform")
    expected = ENROLL if mode == "enroll" else PRESERVE
    require(value.get("passed_cases") == list(expected), "browser_cases")
    require(type(exit_code) is int and exit_code == 0 and value.get("result") == "pass",
            "browser_exit")
    require(value.get("owned_contexts_closed") is True, "browser_cleanup")
    require(value.get("stage") == "browser_complete" and not value.get("exception_type"), "browser_stage")
    required = (("controller_enrolled", "controller_read", "controller_command", "reader_enrolled",
                 "reader_read", "reader_mutation_denied", "credentials_preserved") if mode == "enroll" else (
                    "controller_enrolled", "controller_read", "reader_enrolled", "reader_read", "credentials_preserved")) + (
                    "app_worker_verified", "notification_delivery_disabled", "launch_arguments_verified")
    require(all(value.get(key) is True for key in required), "browser_assertions")
    # Diagnostic data is not forwarded into the uploaded receipt. Reject sensitive
    # fields at any depth rather than relying on dropping them during sanitization.
    def inspect(item, depth=0):
        require(depth <= 12, "browser_depth")
        if type(item) is dict:
            for key, nested in item.items():
                require(not re.search(r"token|secret|credential(?!s_preserved)|setup_url|der_cert|raw|trace|screenshot",
                                      key, re.I), "browser_sensitive")
                inspect(nested, depth + 1)
        elif type(item) is list:
            require(len(item) <= 128, "browser_list")
            for nested in item:
                inspect(nested, depth + 1)
        elif type(item) is str:
            require(len(item) <= 128 and not re.search(r"https?://|wss?://|[#\r\n]", item),
                    "browser_sensitive")
        else:
            require(item is None or type(item) in {bool, int}, "browser_type")
    inspect(value)
    return {"mode": mode, "cases": list(expected), "ok": True, "contexts_closed": True,
            "exit_code": exit_code, "result_bytes": len(data),
            "assertions": {key: value[key] for key in required}}


# Diagnostic strings are a finite protocol, never truncated external values.
SAFE_FAILURES = frozenset({"unexpected_exception", "fixture_refused", "command_failed", "phase_deadline",
    "owned_operation_allowlist", "cleanup_uncertain", "receipt_validation", "cap_raw", "cap_dependencies",
    "cap_stdin", "cap_result", "cap_receipt", "launchctl_authority", "service_guard_query_cap"})
SAFE_EXCEPTION_TYPES = frozenset({"Unknown", "Refusal", "CleanupFailure", "GateFailure", "SystemExit",
    "KeyboardInterrupt", "OSError", "FileNotFoundError", "PermissionError", "FileExistsError", "TimeoutError",
    "ConnectionError", "ConnectionRefusedError", "ConnectionResetError", "BrokenPipeError", "ConnectionAbortedError",
    "ValueError", "TypeError", "KeyError", "IndexError", "AttributeError", "RuntimeError", "AssertionError",
    "UnicodeError", "UnicodeDecodeError", "RecursionError", "SSLError", "SSLCertVerificationError",
    "TimeoutExpired", "HTTPError", "URLError", "BadZipFile", "ReadError", "OperationalError"})
SAFE_FUNCTIONS = frozenset({"unknown", "runtime", "command", "phase", "interrupted", "read_bounded", "strict_json",
    "pairs", "producer", "hydrate", "fetch", "streaming_body", "archive_preflight", "entry_policy", "zip_entry",
    "digest_policy", "checksum_policy", "manifest_policy", "paths_admission", "root_policy", "environment",
    "check", "enter", "remaining", "alarm", "expired", "spawn", "capture", "capture_launcher", "storage",
    "payload_storage_bytes", "check_guards", "effect", "require_launchctl", "profiles", "certificates",
    "tls_controls", "private_link", "fake_operation_summary", "browser_result", "receipt_policy",
    "receipt_values_safe", "verify", "remove_owned_tree", "stop", "close", "group_gone", "reap_bounded",
    "read", "__init__", "socket_allowed", "fake_allowed", "start_launcher", "require", "cap",
    "verify_producer_archive", "record_digest", "write_receipt", "publish_receipt", "publication"})
SAFE_COMMANDS = frozenset({"unknown", "git rev-parse HEAD", "git rev-parse HEADtree", "git status --porcelainv1",
    "git diff --name-only", "node --version", "bun --version", "go version", "curl --version", "openssl version",
    "bun install --frozen-lockfile", "node cli.js install", "go build -modcacherw", "go test -modcacherw",
    "go version -m", "bash setup-link.sh"})


def safe_failure_code(error):
    code = str(error) if isinstance(error, Refusal) else "unexpected_exception"
    return code if code in SAFE_FAILURES else "fixture_refused"


def receipt_values_safe(value):
    """Closed typed schema for EVERY present field, on both PASS and FAIL.

    Partial progress is allowed only at the top level (and source-hash map).
    Nested structures are closed. No generic string sink accepts a filesystem
    path or opaque token: strings are finite public literals, bounded versions,
    or explicitly named digest fields. Refusals never include a value/key/path.
    """
    def enum(*items):
        return lambda item: type(item) is str and item in items

    def pattern(expression):
        return lambda item: type(item) is str and re.fullmatch(expression, item) is not None

    def integer(maximum=1000000, minimum=0):
        return lambda item: type(item) is int and minimum <= item <= maximum

    def nullable(rule):
        return lambda item: item is None or rule(item)

    boolean = lambda item: type(item) is bool
    seconds = lambda item: type(item) in {int, float} and 0 <= item <= LIMITS["total_seconds"] + PHASES[4]
    digest = pattern(r"[0-9a-f]{64}")
    revision = pattern(r"[0-9a-f]{40}")
    version = pattern(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}[a-z]?")
    count = integer()
    exit_code = integer(2147483647, -2147483648)
    assertion_names = ("controller_enrolled", "controller_read", "controller_command", "reader_enrolled",
        "reader_read", "reader_mutation_denied", "credentials_preserved", "app_worker_verified",
        "notification_delivery_disabled", "launch_arguments_verified")
    assertions = {key: boolean for key in assertion_names}
    case = {"mode": enum("enroll", "reprint", "restart"), "cases": (enum(*ENROLL, *PRESERVE), 4),
        "ok": boolean, "contexts_closed": boolean, "exit_code": exit_code,
        "result_bytes": integer(LIMITS["result"]), "stdin_bytes": integer(LIMITS["stdin"]),
        "seconds": seconds, "assertions": assertions}
    tls_category = enum("positive", "unknown_ca", "wrong_host")
    tls_tool = {"category": tls_category, "exit_code": exit_code}
    methods = enum("ping", "agent.list", "pane.list", "workspace.list", "tab.list", "session.snapshot",
                   "events.subscribe", "pane.read", "other")
    guard_observation = {"ordinal": integer(2, 1), "private_plist_before_absent": boolean,
        "private_plist_after_absent": boolean, "setup_ready_seconds": seconds, "loaded_service": boolean,
        "proof_kind": enum("source_bound_wrapper_continued"), "query_exit_zero": boolean,
        "numeric_query_exit_observed": boolean}
    schema = {
        "schemaVersion": integer(1, 1), "candidate_sha": revision, "candidate_tree": revision,
        "source_hashes": {path: digest for path in FILES}, "result": enum("pass", "fail"),
        "scope": enum("darwin_arm64_byo_only"), "launchctl_query_grant_id": enum(LAUNCHCTL_GRANT_ID),
        "github_run_id": integer(10**15 - 1, 1), "github_run_attempt": integer(1, 1),
        "launchctl_query_pinned_parent": enum(LAUNCHCTL_PINNED_PARENT),
        "producer": {"sha": revision, "run": integer(10**15, 1), "artifact": integer(10**15, 1),
                     "zip_sha256": digest, "fresh": boolean},
        "version": version, "manifest_sha256": digest, "binary_sha256": digest,
        "compiler": {"version": enum("go1.27.1"), "target": enum("darwin/arm64"), "metadata_sha256": digest},
        "browser_identity": {"sha256": digest, "version": enum("151.0.7922.34"), "revision": enum("1234"),
            "playwright": enum("1.62.1"), "switches_sha256": digest, "target": enum("darwin/arm64")},
        "tools": {key: version for key in ("python", "node", "bun", "go", "curl", "openssl")},
        "source_proof": {"chromium_commit": enum("782af9cb30a53f54487e5d2e44738645a8ec457c"),
            "playwright_tag": enum("v1.62.1"), "go_tag": enum("go1.27.1"), "trust": enum("TRUST-PROOF.md")},
        "profile_inputs": {name: {"preferences_sha256": digest, **(
            {"database_absent": boolean} if name == "negative" else {"database_sha256": digest})}
            for name in ("controller", "reader", "negative")},
        "profile_policy": {"schema": integer(1, 1), "root_der_sha256": digest,
            "trust_blob": enum("0a020803"), "platform_integration": boolean, "sandbox": boolean, "mock_keychain": boolean},
        "service_guard": {"uid": integer(2147483647), "label": enum("com.herdr-mobile-relay.service"),
            "starts": integer(2), "command": enum("launchctl_print_gui_label"),
            "launchctl_path": enum("/bin/launchctl"), "launchctl_sha256": digest,
            "source_guard_sha256": digest, "source_launcher_sha256": digest, "observations": (guard_observation, 2)},
        "trust_controls": {"python_curl_packaged_go": ({"category": tls_category,
            "python": {"category": tls_category, "verify_code": nullable(integer(1000))},
            "curl": tls_tool, "packaged_go": tls_tool}, 3),
            "browser": {"ok": boolean, "argv_ok": boolean, "contexts_closed": boolean,
                "version": enum("151.0.7922.34"), "observations": (enum("empty_profile_refuses_ca",
                    "trusted_profile_accepts_ip", "trusted_profile_refuses_unknown_ca", "trusted_profile_refuses_wrong_host"), 4)}},
        "cases": (case, 3), "phases": ({"id": integer(4), "ok": boolean, "exit_code": integer(1),
            "cap_seconds": lambda item: type(item) is int and item in PHASES, "seconds": seconds}, 5),
        "cleanup": boolean,
        "owned_cleanup": {"children": count, "listeners": count,
            "surviving_children": nullable(count), "surviving_listeners": nullable(count),
            "observed_surviving_children": count, "observed_surviving_listeners": count,
            "unobserved_children": count, "unobserved_listeners": count,
            "closure_errors": (enum("child_cleanup", "child_observation_unavailable", "listener_cleanup",
                "listener_observation_unavailable", "cleanup_deadline", "cleanup_incomplete", "browser_contexts_unobserved"), 7),
            "uncertain": boolean, "root_deleted": boolean},
        "request_counts": ({key: count for key in ("tls_accepted", "tls_refused", "http_get", "websocket", "client_aborted")}, 3),
        "fixture_operations": {
            "fake": ({"command": enum("agent list", "pane list", "workspace list", "tab list", "pane read", "agent prompt"),
                "outcome": enum("started", "succeeded"), "count": count}, 12),
            "socket": ({"method": methods, "outcome": enum("succeeded", "failed", "other"), "count": count}, 27)},
        "captured_output_bytes": integer(LIMITS["raw"] + 131072),
        "dependency_payload_bytes_peak": integer(LIMITS["dependencies"]),
        "failure": enum(*SAFE_FAILURES), "failure_guard": enum(*GUARDS),
        "failure_detail": {"type": enum(*SAFE_EXCEPTION_TYPES), "errno": nullable(exit_code),
            "function": nullable(enum(*SAFE_FUNCTIONS)), "line": nullable(integer(1000000, 1)),
            "command": nullable(enum(*SAFE_COMMANDS)), "exit_code": nullable(exit_code)},
    }
    if "archive_hashes" in value:
        require(type(value.get("version")) is str and version(value["version"]), "receipt_schema")
        schema["archive_hashes"] = {f"herdr-mobile-relay_{value['version']}_darwin_arm64.tar.gz": digest,
                                    "checksums.txt": digest}

    def verify(item, rule, partial=False):
        if type(rule) is dict:
            require(type(item) is dict and (set(item) <= set(rule) if partial else set(item) == set(rule)),
                    "receipt_schema")
            for key, nested in item.items():
                # The source map is intentionally sparse on an early failure.
                verify(nested, rule[key], rule is schema and key == "source_hashes")
        elif type(rule) is tuple:
            child_rule, maximum = rule
            require(type(item) is list and len(item) <= maximum, "receipt_schema")
            for nested in item:
                # Request counter keys are finite, but zero counters are omitted.
                verify(nested, child_rule, child_rule is schema["request_counts"][0])
        else:
            require(rule(item) is True, "receipt_schema")

    # Browser cases are assigned only after full protocol validation. Their
    # assertion schema depends on mode, not on PASS versus FAIL of the cell.
    modes = value.get("cases", [])
    require(type(modes) is list and len(modes) <= 3, "receipt_schema")
    for result in modes:
        require(type(result) is dict and type(result.get("assertions")) is dict, "receipt_schema")
        keys = set(assertion_names) if result.get("mode") == "enroll" else set(assertion_names) - {
            "controller_command", "reader_mutation_denied"}
        require(set(result["assertions"]) == keys and all(boolean(v) for v in result["assertions"].values()), "receipt_schema")
    case["assertions"] = lambda item: type(item) is dict and set(item) <= set(assertions) and all(boolean(v) for v in item.values())
    verify(value, schema, partial=True)


def receipt_policy(data, candidate, source_hashes, producer_identity):
    """Validate the uploaded protocol itself, not just runtime progress flags."""
    value = strict_json(data, "receipt")
    fields = {"schemaVersion", "candidate_sha", "candidate_tree", "source_hashes", "result", "scope",
              "producer", "archive_hashes", "version", "manifest_sha256", "binary_sha256", "compiler",
              "browser_identity", "tools", "source_proof", "profile_inputs", "profile_policy", "service_guard",
              "trust_controls", "cases", "phases", "cleanup", "owned_cleanup", "request_counts",
              "fixture_operations", "captured_output_bytes", "dependency_payload_bytes_peak", "failure",
              "failure_detail", "failure_guard", "launchctl_query_grant_id",
              "github_run_id", "github_run_attempt", "launchctl_query_pinned_parent"}
    require(type(value) is dict and set(value) <= fields, "receipt_fields")

    def shape(item, keys):
        require(type(item) is dict and set(item) == set(keys), "receipt_shape")

    def digest(item):
        require(type(item) is str and re.fullmatch(r"[0-9a-f]{64}", item), "receipt_digest")

    def number(item, maximum, minimum=0):
        require(type(item) in {int, float} and minimum <= item < maximum, "receipt_number")

    def count(item):
        require(type(item) is int and 0 <= item <= 1000000, "receipt_count")

    # Fail receipts may be partial. All strings/keys still belong to the finite
    # sanitized protocol: no raw errors, paths, URL fragments or secret payloads.
    def sanitized(item, depth=0):
        require(depth <= 12, "receipt_depth")
        if type(item) is dict:
            for key, nested in item.items():
                require(type(key) is str and len(key) <= 128 and not re.search(
                    r"token|secret|password|setup_url|der_cert|raw_log|trace|screenshot", key, re.I), "receipt_sensitive")
                sanitized(nested, depth + 1)
        elif type(item) is list:
            require(len(item) <= 128, "receipt_list")
            for nested in item:
                sanitized(nested, depth + 1)
        elif type(item) is str:
            require(len(item) <= 256 and not re.search(r"https?://|wss?://|[#\r\n]|-----BEGIN", item), "receipt_sensitive")
        else:
            require(item is None or type(item) in {bool, int, float}, "receipt_type")
    sanitized(value)
    receipt_values_safe(value)
    require(type(value.get("schemaVersion")) is int and value["schemaVersion"] == 1
            and value.get("candidate_sha") == candidate and value.get("source_hashes") == source_hashes
            and value.get("producer") == producer_identity
            and value.get("scope") == "darwin_arm64_byo_only" and value.get("result") in {"pass", "fail"},
            "receipt_identity")
    for item in source_hashes.values():
        digest(item)
    cap("dependencies", value.get("dependency_payload_bytes_peak"))
    requests = value.get("request_counts")
    require(type(requests) is list and len(requests) <= 3, "receipt_requests")
    for request in requests:
        require(type(request) is dict and set(request) <= {
            "tls_accepted", "tls_refused", "http_get", "websocket", "client_aborted"}, "receipt_request_fields")
        for amount in request.values():
            count(amount)
    if value["result"] == "fail":
        require(type(value.get("failure")) is str and re.fullmatch(r"[a-z0-9_]{1,80}", value["failure"]), "receipt_failure")
        if "failure_guard" in value or value["failure"] == "owned_operation_allowlist":
            require(type(value.get("failure_guard")) is str and value["failure_guard"] in GUARDS,
                    "receipt_failure_guard")
        detail = value.get("failure_detail")
        if detail is not None:
            shape(detail, ("type", "errno", "function", "line", "command", "exit_code"))
            require(type(detail["type"]) is str and re.fullmatch(r"[A-Za-z0-9_]{1,64}", detail["type"])
                    and all(detail[key] is None or type(detail[key]) is int for key in ("errno", "line", "exit_code"))
                    and (detail["function"] is None or (type(detail["function"]) is str
                         and re.fullmatch(r"[A-Za-z0-9_]{1,64}", detail["function"])))
                    and (detail["command"] is None or (type(detail["command"]) is str
                         and re.fullmatch(r"[A-Za-z0-9_.: -]{1,96}", detail["command"]))), "receipt_failure_detail")
        # A cap failure records the actual count at detection, including bounded
        # chunks already read by concurrent drains. It is never a passing cap
        # observation, and must still produce a sanitized failure receipt.
        observed = value.get("captured_output_bytes")
        require(type(observed) is int and 0 <= observed <= LIMITS["raw"] + 131072,
                "receipt_failed_output_count")
        return False
    cap("raw", value.get("captured_output_bytes"))
    require(set(value) == fields - {"failure", "failure_detail", "failure_guard"} and set(source_hashes) == set(FILES)
            and value["cleanup"] is True and value["launchctl_query_grant_id"] == LAUNCHCTL_GRANT_ID, "receipt_complete")
    require(re.fullmatch(r"[0-9a-f]{40}", value["candidate_tree"]), "receipt_tree")
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value["version"]), "receipt_version")
    archive_name = f"herdr-mobile-relay_{value['version']}_darwin_arm64.tar.gz"
    shape(value["archive_hashes"], (archive_name, "checksums.txt"))
    for item in (*value["archive_hashes"].values(), value["manifest_sha256"], value["binary_sha256"]):
        digest(item)
    compiler = value["compiler"]
    shape(compiler, ("version", "target", "metadata_sha256"))
    require(compiler["version"] == "go1.27.1" and compiler["target"] == "darwin/arm64", "receipt_compiler")
    digest(compiler["metadata_sha256"])
    browser = value["browser_identity"]
    shape(browser, ("sha256", "version", "revision", "playwright", "switches_sha256", "target"))
    require(browser["version"] == "151.0.7922.34" and browser["revision"] == "1234"
            and browser["playwright"] == "1.62.1" and browser["target"] == "darwin/arm64", "receipt_browser")
    digest(browser["sha256"])
    digest(browser["switches_sha256"])
    shape(value["tools"], ("python", "node", "bun", "go", "curl", "openssl"))
    require(value["tools"]["node"] == "24.21.0" and value["tools"]["bun"] == "1.4.0"
            and value["tools"]["go"] == "1.27.1" and all(type(v) is str and re.fullmatch(
                r"[0-9]+\.[0-9]+\.[0-9]+[a-z]?", v) for v in value["tools"].values()), "receipt_tools")
    require(value["source_proof"] == {"chromium_commit": "782af9cb30a53f54487e5d2e44738645a8ec457c",
            "playwright_tag": "v1.62.1", "go_tag": "go1.27.1", "trust": "TRUST-PROOF.md"}, "receipt_source_proof")
    profile_policy = value["profile_policy"]
    shape(profile_policy, ("schema", "root_der_sha256", "trust_blob", "platform_integration", "sandbox", "mock_keychain"))
    require(type(profile_policy["schema"]) is int and profile_policy["schema"] == 1
            and profile_policy["trust_blob"] == "0a020803" and profile_policy["platform_integration"] is False
            and profile_policy["sandbox"] is True and profile_policy["mock_keychain"] is True, "receipt_profile_policy")
    digest(profile_policy["root_der_sha256"])
    shape(value["profile_inputs"], ("controller", "reader", "negative"))
    for name, profile in value["profile_inputs"].items():
        shape(profile, ("database_absent", "preferences_sha256") if name == "negative"
              else ("database_sha256", "preferences_sha256"))
        digest(profile["preferences_sha256"])
        if name == "negative":
            require(profile["database_absent"] is True, "receipt_negative_profile")
        else:
            digest(profile["database_sha256"])
    phases = value["phases"]
    require(type(phases) is list and len(phases) == 5, "receipt_phases")
    total = 0
    for index, phase in enumerate(phases):
        shape(phase, ("id", "ok", "exit_code", "cap_seconds", "seconds"))
        require(type(phase["id"]) is int and phase["id"] == index and phase["ok"] is True
                and type(phase["exit_code"]) is int and phase["exit_code"] == 0
                and phase["cap_seconds"] == PHASES[index], "receipt_phase")
        number(phase["seconds"], PHASES[index])
        total += phase["seconds"]
    require(total < LIMITS["total_seconds"], "receipt_total")
    modes = value["cases"]
    require(type(modes) is list and len(modes) == 3, "receipt_cases")
    for mode, result in zip(("enroll", "reprint", "restart"), modes):
        shape(result, ("mode", "cases", "ok", "contexts_closed", "exit_code", "result_bytes", "assertions", "seconds", "stdin_bytes"))
        expected = ENROLL if mode == "enroll" else PRESERVE
        require(result["mode"] == mode and result["cases"] == list(expected) and result["ok"] is True
                and result["contexts_closed"] is True and type(result["exit_code"]) is int
                and result["exit_code"] == 0, "receipt_case")
        assertions = (("controller_enrolled", "controller_read", "controller_command", "reader_enrolled",
                       "reader_read", "reader_mutation_denied", "credentials_preserved") if mode == "enroll"
                      else ("controller_enrolled", "controller_read", "reader_enrolled", "reader_read", "credentials_preserved")) + (
                      "app_worker_verified", "notification_delivery_disabled", "launch_arguments_verified")
        shape(result["assertions"], assertions)
        require(all(v is True for v in result["assertions"].values()), "receipt_assertions")
        cap("result", result["result_bytes"])
        cap("stdin", result["stdin_bytes"])
        require(result["result_bytes"] > 0 and result["stdin_bytes"] > 0, "receipt_protocol_bytes")
        number(result["seconds"], 150)
    trust = value["trust_controls"]
    shape(trust, ("python_curl_packaged_go", "browser"))
    require(trust["browser"] == {"ok": True, "observations": ["empty_profile_refuses_ca", "trusted_profile_accepts_ip",
        "trusted_profile_refuses_unknown_ca", "trusted_profile_refuses_wrong_host"], "argv_ok": True,
        "version": "151.0.7922.34", "contexts_closed": True}, "receipt_browser_trust")
    require(type(trust["python_curl_packaged_go"]) is list and len(trust["python_curl_packaged_go"]) == 3, "receipt_tls")
    for expected, control in zip(("positive", "unknown_ca", "wrong_host"), trust["python_curl_packaged_go"]):
        shape(control, ("category", "python", "curl", "packaged_go"))
        require(control["category"] == expected, "receipt_tls_category")
        shape(control["python"], ("category", "verify_code"))
        for tool in ("curl", "packaged_go"):
            shape(control[tool], ("category", "exit_code"))
            require(control[tool]["category"] == expected and type(control[tool]["exit_code"]) is int, "receipt_tls_tool")
        require(control["python"]["category"] == expected and (
            control["python"]["verify_code"] is None if expected == "positive" else
            control["python"]["verify_code"] in ({20, 21} if expected == "unknown_ca" else {64})), "receipt_python_tls")
        require((control["curl"]["exit_code"] == 0 and control["packaged_go"]["exit_code"] == 0) if expected == "positive"
                else (control["curl"]["exit_code"] == 60 and control["packaged_go"]["exit_code"] != 0), "receipt_tls_exit")
    guard = value["service_guard"]
    shape(guard, ("uid", "label", "starts", "command", "launchctl_path", "launchctl_sha256",
                  "source_guard_sha256", "source_launcher_sha256", "observations"))
    require(type(guard["uid"]) is int and guard["uid"] >= 0 and guard["label"] == "com.herdr-mobile-relay.service"
            and guard["starts"] == 2 and guard["command"] == "launchctl_print_gui_label"
            and guard["launchctl_path"] == "/bin/launchctl", "receipt_guard")
    for key in ("launchctl_sha256", "source_guard_sha256", "source_launcher_sha256"):
        digest(guard[key])
    require(type(guard["observations"]) is list and len(guard["observations"]) == 2, "receipt_guard_observations")
    for index, observation in enumerate(guard["observations"], 1):
        shape(observation, ("ordinal", "private_plist_before_absent", "private_plist_after_absent", "setup_ready_seconds",
                            "loaded_service", "proof_kind", "query_exit_zero", "numeric_query_exit_observed"))
        require(observation["ordinal"] == index and observation["private_plist_before_absent"] is True
                and observation["private_plist_after_absent"] is True and observation["loaded_service"] is False
                and observation["proof_kind"] == "source_bound_wrapper_continued" and observation["query_exit_zero"] is False
                and observation["numeric_query_exit_observed"] is False, "receipt_guard_observation")
        number(observation["setup_ready_seconds"], 70)
    owned = value["owned_cleanup"]
    shape(owned, ("children", "listeners", "surviving_children", "surviving_listeners", "observed_surviving_children",
                  "observed_surviving_listeners", "unobserved_children", "unobserved_listeners", "closure_errors", "uncertain", "root_deleted"))
    for key in ("children", "listeners"):
        count(owned[key])
        require(owned[key] > 0, "receipt_owned_resources")
    require(all(type(owned[key]) is int and owned[key] == 0 for key in (
        "surviving_children", "surviving_listeners", "observed_surviving_children", "observed_surviving_listeners",
        "unobserved_children", "unobserved_listeners")) and owned["closure_errors"] == []
        and owned["uncertain"] is False and owned["root_deleted"] is True, "receipt_cleanup")
    require(len(requests) == 3, "receipt_requests")
    require(requests[0].get("websocket", 0) >= 2 and requests[0].get("http_get", 0) > 0
            and requests[0].get("tls_refused", 0) > 0
            and all(request.get("tls_refused", 0) > 0 for request in requests[1:])
            and all(request.get("http_get", 0) == request.get("websocket", 0) == 0 for request in requests[1:]), "receipt_request_controls")
    operations = value["fixture_operations"]
    shape(operations, ("fake", "socket"))
    require(type(operations["fake"]) is list and type(operations["socket"]) is list, "receipt_operations")
    seen = set()
    for row in operations["fake"]:
        shape(row, ("command", "outcome", "count"))
        require(row["command"] in {"agent list", "pane list", "workspace list", "tab list", "pane read", "agent prompt"}
                and row["outcome"] == "succeeded" and row["command"] not in seen, "receipt_fake_operation")
        seen.add(row["command"])
        count(row["count"])
        require(row["count"] > 0 and (row["command"] != "agent prompt" or row["count"] == 1), "receipt_fake_count")
    require("agent prompt" in seen, "receipt_fake_mutation")
    counts = {}
    for row in operations["socket"]:
        shape(row, ("method", "outcome", "count"))
        key = (row["method"], row["outcome"])
        require(key not in counts, "receipt_socket_duplicate")
        count(row["count"])
        require(row["count"] > 0, "receipt_socket_count")
        counts[key] = row["count"]
    require(socket_operations_allowed(counts) and counts.get(("pane.read", "succeeded"), 0) >= 2
            and counts.get(("ping", "succeeded"), 0) > 0, "receipt_socket_operations")
    return True


def producer(sha_value, run, artifact, digest, fresh, candidate, candidate_run):
    require(type(sha_value) is str and re.fullmatch(r"[0-9a-f]{40}", sha_value)
            and type(digest) is str and re.fullmatch(r"[0-9a-f]{64}", digest)
            and type(run) is int and run > 0 and type(artifact) is int and artifact > 0,
            "producer_identity")
    require((fresh is False and (sha_value, run, artifact, digest) == INHERITED)
            or (fresh is True and sha_value == candidate and run == candidate_run),
            "producer_binding")


def paths_admission(root, evidence, runner_temp, budget):
    require(type(budget) is int and budget == LIMITS["total_seconds"], "budget")
    require(root.is_absolute() and evidence.is_absolute() and root != evidence,
            "resource_path")
    require(root.parent == runner_temp and evidence.parent.parent == runner_temp
            and evidence.parent != root and evidence.name == "darwin-package-browser.json",
            "resource_path")
    require(root.parent.resolve(strict=True) == root.parent and
            evidence.parent.parent.resolve(strict=True) == evidence.parent.parent, "resource_parent")
    root_policy(root.exists(), root.is_symlink())
    require(not evidence.is_symlink() and not evidence.exists() and
            not evidence.parent.is_symlink() and not evidence.parent.exists(), "receipt_exists")


def root_policy(exists, symlink):
    require(type(exists) is bool and type(symlink) is bool and not exists and not symlink,
            "root_exists")


@dataclass(frozen=True)
class Entry:
    name: str
    kind: str
    size: int


def entry_policy(entries, kind):
    maximum = LIMITS[kind + "_count"]
    seen = {}
    folded = set()
    ancestors = set()
    total = 0
    root_entry = False
    for count, entry in enumerate(entries, 1):
        require(count <= maximum, "archive_count")
        # The unchanged production producer uses `tar -C STAGE ... .`.
        # Admit exactly one inert root-directory header, never a root payload.
        if kind == "tar" and entry.name in {".", "./"}:
            require(entry.kind == "directory" and type(entry.size) is int and entry.size == 0
                    and not root_entry, "archive_root")
            root_entry = True
            continue
        name = entry.name.removeprefix("./").removesuffix("/")
        require(name and len(name.encode("utf-8")) <= 4096 and re.fullmatch(r"[A-Za-z0-9._/-]+", name)
                and not name.startswith("/") and "\\" not in name and not any(ord(c) < 32 for c in name)
                and all(part not in {"", ".", ".."} for part in name.split("/")), "archive_path")
        require(entry.kind in {"file", "directory"}, "archive_type")
        require(name not in seen and name.casefold() not in folded, "archive_duplicate")
        folded.add(name.casefold())
        require(type(entry.size) is int and 0 <= entry.size <= LIMITS["individual"], "archive_individual")
        require(entry.kind == "file" or entry.size == 0, "archive_directory")
        for parent in Path(name).parents:
            parent_name = parent.as_posix().casefold()
            require(seen.get(parent_name) != "file", "archive_parent")
            ancestors.add(parent_name)
        require(entry.kind != "file" or name.casefold() not in ancestors, "archive_parent")
        seen[name.casefold()] = entry.kind
        total += entry.size
        require(total <= LIMITS["unpacked"], "archive_aggregate")
    return seen


def zip_entry(item):
    unix_type = stat.S_IFMT(item.external_attr >> 16)
    permitted = {0, stat.S_IFDIR} if item.is_dir() else {0, stat.S_IFREG}
    require(unix_type in permitted and not (item.flag_bits & 1)
            and item.compress_type in {zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED}, "zip_type")
    return Entry(item.filename, "directory" if item.is_dir() else "file", item.file_size)


def digest_policy(actual, expected, code):
    require(type(expected) is str and re.fullmatch(r"[0-9a-f]{64}", expected)
            and actual == expected, code)


def checksum_policy(data, basename, digest):
    require(len(data) <= 65536, "checksum_cap")
    try:
        matches = [line.split() for line in data.decode("ascii").splitlines() if line.strip()]
    except UnicodeError:
        raise Refusal("checksum_format") from None
    matches = [line for line in matches if len(line) == 2 and line[1] == basename]
    require(matches == [[digest, basename]], "checksum")


def manifest_policy(manifest, version, revision, payload):
    require(type(manifest) is dict and type(manifest.get("schema")) is int and manifest.get("schema") == 1
            and manifest.get("target") == "darwin/arm64" and manifest.get("version") == version
            and manifest.get("revision") == revision and type(manifest.get("files")) is dict,
            "manifest_identity")
    require(manifest["files"] == payload and all(re.fullmatch(r"[0-9a-f]{64}", h)
                                               for h in payload.values()), "manifest_payload")
    for path in ("herdr-mobile-relay", "web/index.html", "web/sw.js", "relay/common.sh",
                 "relay/tailscale.sh", "relay/tailscale-external.sh", "relay/setup-link.sh"):
        require(path in payload, "manifest_required")


class CountReader:
    def __init__(self, stream, kind):
        self.stream, self.kind, self.count = stream, kind, 0

    def read(self, n=-1):
        require(type(n) is int and n >= 0, "unbounded_read")
        data = self.stream.read(min(n, LIMITS[self.kind] - self.count + 1))
        self.count += len(data)
        cap(self.kind, self.count)
        return data


class Budget:
    def __init__(self, clock=time.monotonic):
        self.clock, self.start, self.phase_start = clock, clock(), clock()
        self.phase, self.failed = -1, False

    def check(self):
        require(not self.failed, "phase_stopped")
        now = self.clock()
        if now - self.start >= LIMITS["total_seconds"] or (
                0 <= self.phase < 4 and now - self.start >= LIMITS["total_seconds"] - PHASES[4]) or (
                self.phase >= 0 and now - self.phase_start >= PHASES[self.phase]):
            self.failed = True
            raise Refusal("phase_deadline")

    def enter(self, phase):
        self.check()
        require(phase == self.phase + 1 and phase < len(PHASES), "phase_order")
        self.phase, self.phase_start = phase, self.clock()

    def remaining(self):
        self.check()
        reserve = PHASES[4] if self.phase < 4 else 0
        return min(LIMITS["total_seconds"] - reserve - (self.clock() - self.start),
                   PHASES[self.phase] - (self.clock() - self.phase_start))

    @contextmanager
    def alarm(self):
        # Runtime-only hard interruption also covers a blocked hydration read,
        # archive extraction or filesystem observation. Offline fakes use check.
        previous = signal.getsignal(signal.SIGALRM)
        require(signal.getitimer(signal.ITIMER_REAL) == (0.0, 0.0), "phase_timer_collision")
        def expired(*_):
            self.failed = True
            raise Refusal("phase_deadline")
        signal.signal(signal.SIGALRM, expired)
        try:
            signal.setitimer(signal.ITIMER_REAL, self.remaining())
            yield
            self.check()
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            signal.signal(signal.SIGALRM, previous)


class CleanupDeadline:
    """One monotonic cap shared by all closures, deletion and receipt writing."""
    def __init__(self, seconds=30, clock=time.monotonic, latest=None, reserve=0):
        self.clock, self.reserve = clock, reserve
        self.end = clock() + seconds
        if latest is not None:
            self.end = min(self.end, latest)

    def check(self):
        require(self.clock() < self.end - self.reserve, "cleanup_deadline")

    def remaining(self, maximum):
        self.check()
        return min(maximum, self.end - self.reserve - self.clock())

    @contextmanager
    def alarm(self):
        # Runtime only, on the owned Python main thread. Offline clock checks
        # never enter this context and never install timers or deliver signals.
        # Interrupt an unexpectedly blocking closer/filesystem/receipt operation,
        # not just measure it after returning. An interrupted cleanup is failure.
        previous = signal.getsignal(signal.SIGALRM)
        require(signal.getitimer(signal.ITIMER_REAL) == (0.0, 0.0), "cleanup_timer_collision")
        def expired(*_):
            raise Refusal("cleanup_deadline")
        signal.signal(signal.SIGALRM, expired)
        try:
            signal.setitimer(signal.ITIMER_REAL, self.remaining(30))
            yield
            self.check()
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            signal.signal(signal.SIGALRM, previous)


def remove_owned_tree(root, identity, deadline, remove=shutil.rmtree):
    deadline.check()
    info = root.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_ino == identity.st_ino
            and info.st_dev == identity.st_dev and root.resolve(strict=True) == root,
            "cleanup_root_identity")
    deadline.check()
    remove(root)  # Runtime alarm covers this entire operation, including C/IO waits.
    deadline.check()
    require(not root.exists() and not root.is_symlink(), "cleanup_root_survived")


class CleanupFailure(Refusal):
    def __init__(self, evidence):
        super().__init__("cleanup_uncertain")
        self.evidence = evidence


class Cleanup:
    """Objects confer authority, not caller-supplied PIDs. Operations are injected."""
    def __init__(self):
        self.children, self.listeners = [], []
        # Keep this same object available to runtime even if close is interrupted.
        # Null means not observed, never an inferred absence of survivors.
        self.evidence = {"children": 0, "listeners": 0, "surviving_children": None,
                         "surviving_listeners": None, "observed_surviving_children": 0,
                         "observed_surviving_listeners": 0, "unobserved_children": 0,
                         "unobserved_listeners": 0, "closure_errors": [], "uncertain": True,
                         "root_deleted": False}

    def error(self, code):
        if code not in self.evidence["closure_errors"]:
            self.evidence["closure_errors"].append(code)
        self.evidence["uncertain"] = True

    def child(self, child):
        self.children.append(child)
        self.evidence["children"] = len(self.children)
        self.evidence["unobserved_children"] = len(self.children)
        return child

    def listener(self, listener):
        self.listeners.append(listener)
        self.evidence["listeners"] = len(self.listeners)
        self.evidence["unobserved_listeners"] = len(self.listeners)
        return listener

    def stop(self, child, deadline=None):
        require(any(item is child for item in self.children), "unowned_child")
        deadline = deadline if deadline is not None else CleanupDeadline(10)
        for name, seconds in (("INT", 3), ("TERM", 3), ("KILL", 2)):
            deadline.check()
            if child.done():
                break
            child.send(name)
            child.wait_bounded(deadline.remaining(seconds))
        deadline.check()
        require(child.reap_bounded(deadline.remaining(2)) and child.done() and child.group_gone(), "child_survived")
        deadline.check()

    def close(self, deadline=None):
        deadline = deadline if deadline is not None else CleanupDeadline()
        self.evidence.update(children=len(self.children), listeners=len(self.listeners))
        children, listeners = [], []
        for child in reversed(self.children):
            try:
                self.stop(child, deadline)
                children.append(False)
            except Exception:
                self.error("child_cleanup")
                try:
                    deadline.check()
                    # Read-only observation does not renew signaling authority
                    # after reaping, including when descendants survived.
                    children.append(not (child.done() and child.group_gone()))
                except Exception:
                    children.append(None)
                    self.error("child_observation_unavailable")
        for listener in reversed(self.listeners):
            try:
                deadline.check()
                listener.close(deadline)
                deadline.check()
                require(listener.closed(), "listener_survived")
                listeners.append(False)
            except Exception:
                self.error("listener_cleanup")
                try:
                    deadline.check()
                    listeners.append(not listener.closed())
                except Exception:
                    listeners.append(None)
                    self.error("listener_observation_unavailable")
        self.evidence["unobserved_children"] = children.count(None)
        self.evidence["unobserved_listeners"] = listeners.count(None)
        self.evidence["observed_surviving_children"] = children.count(True)
        self.evidence["observed_surviving_listeners"] = listeners.count(True)
        self.evidence["surviving_children"] = None if None in children else sum(children)
        self.evidence["surviving_listeners"] = None if None in listeners else sum(listeners)
        try:
            deadline.check()
        except Refusal:
            self.error("cleanup_deadline")
        self.evidence["uncertain"] = bool(self.evidence["closure_errors"])
        if self.evidence["uncertain"]:
            raise CleanupFailure(self.evidence)
        return self.evidence


class Child:
    def __init__(self, process, effects):
        self.process, self.reaped, self.effects = process, False, effects

    def done(self):
        # poll reaps only this Popen's child. Once reaped, group signaling is
        # forbidden; a remaining group is uncertainty, not renewed PID authority.
        result = self.process.poll()
        self.reaped = result is not None
        return self.reaped

    def send(self, name):
        self.effects.effect("signal")
        require(not self.reaped and self.process.returncode is None, "reaped_signal_refused")
        try:
            os.killpg(self.process.pid, getattr(signal, "SIG" + name))
        except ProcessLookupError:
            pass

    def wait_bounded(self, seconds):
        try:
            self.process.wait(timeout=seconds)
            self.reaped = True
        except subprocess.TimeoutExpired:
            pass

    def reap_bounded(self, seconds):
        self.wait_bounded(seconds)
        return self.reaped

    def group_gone(self):
        try:
            os.killpg(self.process.pid, 0)
        except ProcessLookupError:
            return True
        return False


class EffectGate:
    """The runtime boundary can be injected closed before any real operation."""
    KINDS = frozenset({"network", "executable", "browser", "go", "launchctl", "trust", "signal"})

    def __init__(self, authorized, *, launchctl_grant=None, launchctl_context=None, launchctl_observe=None):
        require(type(authorized) is bool, "effect_gate_type")
        self.authorized = authorized
        self.launchctl_grant, self.launchctl_context = launchctl_grant, launchctl_context
        self.launchctl_observe = launchctl_observe
        self.launchctl_queries = 0

    def require_launchctl(self):
        require(self.authorized and launchctl_authorized(self.launchctl_grant, self.launchctl_context),
                "launchctl_authority")
        # Runtime re-reads the bounded event and read-only git identity before
        # launcher capture AND query reservation. Offline callers inject both.
        if self.launchctl_observe is not None:
            current = self.launchctl_observe()
            require(current == self.launchctl_context and launchctl_authorized(self.launchctl_grant, current),
                    "launchctl_authority")

    def effect(self, kind):
        require(self.authorized and kind in self.KINDS, "real_effect_refused")
        if kind == "launchctl":
            self.require_launchctl()
            require(self.launchctl_queries < LAUNCHCTL_SCOPE["maxQueries"], "service_guard_query_cap")
            self.launchctl_queries += 1


def launchctl_packet_allowed(packet):
    # Do not echo rejected input. The workflow projects this exact frozen grant
    # ID/parent; packet equality is operational admission, not proof of origin.
    if type(packet) is not str:
        return False
    try:
        value = strict_json(packet.encode("utf-8"))
    except (Refusal, UnicodeError):
        return False
    return (type(value) is dict and set(value) == {"grantId", "authorized", "pinnedParent", "scope"}
            and value.get("grantId") == LAUNCHCTL_GRANT_ID and value.get("authorized") is True
            and value.get("pinnedParent") == LAUNCHCTL_PINNED_PARENT
            and type(value.get("scope")) is dict and value["scope"] == dict(LAUNCHCTL_SCOPE)
            and type(value["scope"].get("maxQueries")) is int)


def launchctl_authorized(packet, observation):
    # Operational projection of the hosted-only parent grant, not proof of the
    # caller's host. Forging a local observation never grants local permission.
    return launchctl_packet_allowed(packet) and admission_policy(observation)


def ci_context_allowed(observation):
    # Environment/platform observations are operational admission checks, not
    # authentication or a security boundary against a deliberately local forger.
    return (type(observation) is dict and observation.get("system") == "Darwin"
            and observation.get("machine") == "arm64" and observation.get("enabled") == "1"
            and observation.get("repository") == REPOSITORY and observation.get("ref") == REF
            and observation.get("attempt") == "1" and observation.get("event_name") == "push"
            and observation.get("runner_environment") == "github-hosted" and observation.get("image_os") == "macos15"
            and type(observation.get("candidate_sha")) is str
            and re.fullmatch(r"[0-9a-f]{40}", observation["candidate_sha"]) is not None
            and observation["candidate_sha"] != "0" * 40
            and type(observation.get("run_id")) is str
            and re.fullmatch(r"[1-9][0-9]{0,14}", observation["run_id"]) is not None)


def admission_policy(observation):
    # Same pure operational predicate at CI admission and each launcher/query
    # boundary. Rechecking forgeable inputs does not authenticate a hosted runner.
    return (ci_context_allowed(observation)
            and observation.get("event_before") == LAUNCHCTL_PINNED_PARENT
            and observation.get("event_after") == observation["candidate_sha"]
            and observation.get("event_ref") == REF
            and all(observation.get(key) is False for key in ("event_forced", "event_created", "event_deleted"))
            and observation.get("checked_out_sha") == observation["candidate_sha"]
            and type(observation.get("candidate_parents")) is list
            and observation["candidate_parents"] == [LAUNCHCTL_PINNED_PARENT])


def git_candidate_identity(candidate):
    # Read only the checked-out object's SHA/parents, not its message or files.
    # No replacement objects, optional index locks or ambient git configuration.
    try:
        result = subprocess.run(["git", "--no-replace-objects", "--no-optional-locks",
            "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
            "log", "-1", "--format=%H%n%P", "HEAD", "--"], cwd=candidate,
            env={"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                 "GIT_OPTIONAL_LOCKS": "0"}, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10,
            check=False)
        require(result.returncode == 0 and len(result.stdout) <= 4096, "launchctl_authority")
        lines = result.stdout.decode("ascii").splitlines()
        require(len(lines) == 2, "launchctl_authority")
        return {"checked_out_sha": lines[0], "candidate_parents": lines[1].split()}
    except (OSError, UnicodeError, subprocess.TimeoutExpired):
        raise Refusal("launchctl_authority") from None


def first_push_observation(observation, event_path, candidate, *, read_event=read_bounded,
                           query_candidate=git_candidate_identity):
    # The caller-selected event and checkout bind the authorized hosted attempt
    # operationally; they do not make a forged local invocation authorized.
    require(ci_context_allowed(observation), "ci_admission")
    require(type(event_path) is str and event_path and Path(event_path).is_absolute(), "launchctl_authority")
    try:
        event = strict_json(read_event(Path(event_path), PUSH_EVENT_CAP))
    except (OSError, Refusal):
        raise Refusal("launchctl_authority") from None
    require(type(event) is dict, "launchctl_authority")
    # Check the event BEFORE git acquisition, let alone any privileged launcher.
    require(event.get("before") == LAUNCHCTL_PINNED_PARENT
            and event.get("after") == observation["candidate_sha"] and event.get("ref") == REF
            and all(event.get(key) is False for key in ("forced", "created", "deleted")), "launchctl_authority")
    identity = query_candidate(candidate)
    require(type(identity) is dict and set(identity) == {"checked_out_sha", "candidate_parents"}, "launchctl_authority")
    current = {**observation, **identity, "event_before": event["before"], "event_after": event["after"],
        "event_ref": event["ref"], **{"event_" + key: event[key] for key in ("forced", "created", "deleted")}}
    require(admission_policy(current), "launchctl_authority")
    return current


def child_environment_allowed(env):
    return not any(key in env for key in ("GH_TOKEN", "GITHUB_TOKEN", "BUN_AUTH_TOKEN", "NODE_OPTIONS", "PYTHONPATH"))


def service_inputs_allowed(plist_present, plist_symlink, command_path):
    # Metadata observations, not a launchctl probe or service-controller effect.
    return plist_present is False and plist_symlink is False and command_path == "/bin/launchctl"


def fixture_port_allowed(port):
    return type(port) is int and port in {18443, 18444, 18445}


def public_request_allowed(raw_path, host, upgrade, paths, port):
    try:
        parts = urllib.parse.urlsplit(raw_path)
    except ValueError:
        return False
    return (fixture_port_allowed(port) and not parts.scheme and not parts.netloc
            and parts.path in paths and not parts.fragment
            and (not parts.query or re.fullmatch(r"v=[A-Za-z0-9._-]{1,128}", parts.query) is not None)
            and host == "127.0.0.1:" + str(port)
            and (not upgrade or (upgrade == "websocket" and parts.path in {"/", "/ws"} and not parts.query)))


def socket_operations_allowed(counts):
    allowed = {"ping", "agent.list", "pane.list", "workspace.list", "tab.list", "session.snapshot",
               "events.subscribe", "pane.read"}
    return all(method in allowed and outcome == "succeeded" for method, outcome in counts)


def proxy_operations_allowed(servers):
    return all(server.failure is False for server in servers)


def owned_socket_closed(error, closing):
    # Closing our own listener/client sockets may race an in-flight read.
    # Do not suppress policy/internal exceptions merely because cleanup began.
    return (closing and isinstance(error, OSError)
            and error.errno in {errno.EBADF, errno.ENOTCONN})


def proxy_request_counts(servers):
    result = []
    for server in servers:
        with server.lock:
            result.append(dict(server.counts))
    return result


def fake_operation_summary(data, complete=False):
    counts, sequences = {}, set()
    for line in data.splitlines():
        value = strict_json(line)
        require(type(value) is dict and type(value.get("sequence")) is int
                and value["sequence"] not in sequences, "fake_operation_sequence")
        sequences.add(value["sequence"])
        argv = value.get("argv")
        require(type(argv) is list and len(argv) >= 2 and all(type(arg) is str for arg in argv), "fake_operation_argv")
        command = " ".join(argv[:2])
        require(command in {"agent list", "pane list", "workspace list", "tab list", "pane read", "agent prompt"},
                "fake_operation_allowlist")
        if command == "agent prompt":
            require(argv == ["agent", "prompt", "workspace:agent", "package acceptance harmless ping"],
                    "fake_mutation_allowlist")
        outcome = value.get("outcome")
        require(outcome in {"started", "succeeded"} and (not complete or outcome == "succeeded"), "fake_operation_outcome")
        key = (command, outcome)
        counts[key] = counts.get(key, 0) + 1
    require(sum(count for (command, _), count in counts.items() if command == "agent prompt") <= 1,
            "fake_mutation_count")
    return [{"command": command, "outcome": outcome, "count": count}
            for (command, outcome), count in sorted(counts.items())]


def payload_storage_bytes(directories, check, *, walk=os.walk, metadata=Path.lstat):
    """Observe live installer payload; disappearance is not an unreadable file."""
    total = 0

    def walk_error(error):
        if not isinstance(error, FileNotFoundError):
            raise error

    for directory in directories:
        check()
        for parent, _, files in walk(directory, followlinks=False, onerror=walk_error):
            check()
            for name in files:
                check()
                try:
                    info = metadata(Path(parent) / name)
                except FileNotFoundError:
                    # Bun/Playwright/Go remove staging files while we observe
                    # their payload. A deleted entry consumes no current bytes;
                    # permission and other IO errors still fail the observation.
                    continue
                if stat.S_ISREG(info.st_mode):
                    total += info.st_size
                    cap("dependencies", total)
    return total


class Operations:
    def __init__(self, root, env, budget, cleanup, effects=None):
        self.root, self.env, self.budget, self.cleanup = root, env, budget, cleanup
        self.effects = effects if effects is not None else EffectGate(True)
        self.raw, self.children, self.storage_peak = 0, [], 0
        self.output_lock = threading.Lock()
        self.guards, self.failure_guard = [], None

    def add_guard(self, name, guard):
        require(type(name) is str and name in GUARDS and callable(guard), "guard_identity")
        self.guards.append((name, guard))

    def check_guards(self, check=lambda: None):
        for name, guard in self.guards:
            check()
            try:
                allowed = guard() is True
            except Exception:
                allowed = False
            if not allowed:
                self.failure_guard = self.failure_guard or name
                raise Refusal("owned_operation_allowlist")

    def check(self):
        self.budget.check()
        self.check_guards()

    def storage(self):
        # Count downloaded payload (including installer staging), not generated
        # release/fixture binaries or Go's compilation cache. GOTMPDIR keeps Go
        # build scratch separate from the installers' streamed TMPDIR payload.
        total = payload_storage_bytes((
            self.root / "browsers", self.root / "go-mod-cache", self.root / "go-path",
            self.root / "cache", self.root / "home/.bun", self.root / "tmp",
            Path(__file__).resolve().parents[1] / "frontend/node_modules"), self.budget.check)
        self.storage_peak = max(self.storage_peak, total)
        return total

    def spawn(self, argv, cwd=None, env=None):
        self.check()
        self.effects.effect("executable")
        require(all(type(arg) is str for arg in argv) and argv, "command_arguments")
        child_env = self.env if env is None else env
        require(child_environment_allowed(child_env), "child_secret_environment")
        child = self.cleanup.child(Child(subprocess.Popen(
            argv, cwd=cwd, env=child_env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, start_new_session=True, close_fds=True), self.effects))
        self.children.append(child)
        return child

    def capture_launcher(self, argv, guard, *, cwd, link_origin):
        # This is the shared runtime/offline launcher boundary. Default
        # EffectGate(True) is NOT sufficient for a protected launchctl query.
        self.effects.require_launchctl()
        self.check()
        require(type(guard.get("starts")) is int and 0 <= guard["starts"] < 2, "service_guard_query_cap")
        self.effects.effect("launchctl")
        guard["starts"] += 1  # Reserve the attempt before capture can fail.
        return self.capture(argv, 70, cwd=cwd, link_origin=link_origin)

    def capture(self, argv, timeout, data=b"", cwd=None, link_origin=None, env=None, result_cap=None):
        cap("stdin", len(data))
        start = time.monotonic()
        end = start + min(timeout, self.budget.remaining())
        child = self.spawn(argv, cwd, env)
        process = child.process
        output, errors = bytearray(), bytearray()
        with selectors.DefaultSelector() as selector:
            for stream, label in ((process.stdout, "out"), (process.stderr, "err")):
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, label)
            if data:
                os.set_blocking(process.stdin.fileno(), False)
                selector.register(process.stdin, selectors.EVENT_WRITE, "in")
            else:
                process.stdin.close()
            pending = memoryview(data)
            while selector.get_map():
                self.check()
                require(time.monotonic() < end, "command_deadline")
                self.storage()
                for key, _ in selector.select(0.1):
                    if key.data == "in":
                        count = os.write(key.fd, pending)
                        pending = pending[count:]
                        if not pending:
                            selector.unregister(key.fileobj)
                            key.fileobj.close()
                        continue
                    chunk = os.read(key.fd, 16384)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        key.fileobj.close()
                        continue
                    with self.output_lock:
                        self.raw += len(chunk)
                        cap("raw", self.raw)
                    (output if key.data == "out" else errors).extend(chunk)
                    if result_cap is not None:
                        require(len(output) <= result_cap, "browser_output_cap")
                    if link_origin is not None:
                        link = private_link(bytes(output), link_origin)
                        if link is not None:
                            # The foreground wrapper remains owned. Drain its
                            # remaining pipes with tracked bounded threads.
                            drain = PipeDrain(process, selector, self)
                            self.add_guard("pipe_drain", lambda: not drain.error)
                            child.startup_seconds = time.monotonic() - start
                            require(child.startup_seconds < timeout, "launcher_deadline")
                            return child, link
        process.wait(timeout=max(0.01, end - time.monotonic()))
        child.reaped = True
        require(child.group_gone(), "command_descendants_survived")
        require(time.monotonic() < end, "command_deadline")
        return SimpleNamespace(returncode=process.returncode, stdout=bytes(output), stderr=bytes(errors),
                               seconds=time.monotonic() - start)


class PipeDrain:
    def __init__(self, process, selector, operations):
        self.stop_event, self.threads, self.streams = threading.Event(), [], []
        self.error = False
        operations.cleanup.listener(self)
        for key in list(selector.get_map().values()):
            selector.unregister(key.fileobj)
            if key.data == "in":
                key.fileobj.close()
                continue
            self.streams.append(key.fileobj)
            thread = threading.Thread(target=self.drain, args=(key.fileobj, operations), daemon=True)
            self.threads.append(thread)
            thread.start()

    def drain(self, stream, operations):
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(stream, selectors.EVENT_READ)
                while not self.stop_event.is_set():
                    for key, _ in selector.select(0.1):
                        chunk = os.read(key.fd, 16384)
                        if not chunk:
                            return
                        with operations.output_lock:
                            operations.raw += len(chunk)
                            cap("raw", operations.raw)
        except Exception:
            self.error = True

    def close(self, deadline):
        self.stop_event.set()
        for thread in self.threads:
            if thread.ident is not None:
                thread.join(deadline.remaining(2))
        for stream in self.streams:
            deadline.check()
            stream.close()

    def closed(self):
        return not self.error and all(not thread.is_alive() for thread in self.threads)


def private_link(data, origin):
    for line in data.splitlines():
        start = line.find((origin + "/#").encode())
        if start < 0:
            continue
        try:
            value = re.split(rb"[\s\x1b]", line[start:], maxsplit=1)[0].decode("ascii")
            parts = urllib.parse.urlsplit(value)
            fields = urllib.parse.parse_qs(parts.fragment, strict_parsing=True)
            if (parts.scheme == "https" and parts.netloc == origin[8:] and parts.path == "/"
                    and not parts.query and len(fields.get("setup", [])) == 1
                    and re.fullmatch(r"[0-9a-f]{32}", fields["setup"][0])):
                return value
        except (ValueError, UnicodeError):
            pass
    return None


def environment(root):
    env = {"PATH": os.environ["PATH"], "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
           "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly", "GOENV": "off", "GOWORK": "off",
           "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
           "BASH_ENV": "/dev/null", "ENV": "/dev/null", "NO_PROXY": "*", "no_proxy": "*",
           "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}
    for key, directory in {"HOME": "home", "TMPDIR": "tmp", "XDG_CONFIG_HOME": "config",
                           "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "GOCACHE": "go-cache",
                           "GOMODCACHE": "go-mod-cache", "GOPATH": "go-path", "GOTMPDIR": "go-build-temp",
                           "PLAYWRIGHT_BROWSERS_PATH": "browsers"}.items():
        path = root / directory
        path.mkdir(mode=0o700)
        env[key] = str(path)
    return env


def streaming_body(response, maximum, budget):
    chunks, size = [], 0
    while True:
        budget.check()
        chunk = response.read(min(65536, maximum - size + 1))
        if not chunk:
            break
        size += len(chunk)
        require(size <= maximum, "hydration_stream_cap")
        chunks.append(chunk)
    return b"".join(chunks)


def hydrate(args, root, budget, candidate, effects):
    effects.effect("network")
    token = os.environ.pop("GH_TOKEN", None)
    os.environ.pop("GITHUB_TOKEN", None)
    require(token is not None and len(token) <= 4096, "hydration_token_missing")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def fetch(path, maximum=65536, raw=False):
        budget.check()
        request = urllib.request.Request("https://api.github.com/repos/" + REPOSITORY + path,
            headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
                     "X-GitHub-Api-Version": "2022-11-28"})
        if raw:
            request.add_header("Accept", "application/vnd.github+json")
        try:
            response = opener.open(request, timeout=min(10, budget.remaining()))
        except urllib.error.HTTPError as error:
            require(raw and error.code == 302, "artifact_api")
            location = error.headers.get("Location", "")
            parsed = urllib.parse.urlsplit(location)
            require(parsed.scheme == "https" and parsed.hostname is not None
                    and parsed.username is None and parsed.password is None
                    and (parsed.hostname.endswith(".blob.core.windows.net")
                         or parsed.hostname.endswith(".actions.githubusercontent.com")), "artifact_redirect")
            # Never forward the hydration token to the signed blob URL.
            response = opener.open(urllib.request.Request(location), timeout=min(10, budget.remaining()))
        with response:
            data = streaming_body(response, maximum, budget)
        return data if raw else strict_json(data)

    try:
        run = fetch(f"/actions/runs/{args.producer_run}")
        artifact = fetch(f"/actions/artifacts/{args.artifact_id}")
        require(run.get("head_sha") == args.producer_sha and run.get("event") == "push"
                and run.get("run_attempt") == 1
                and run.get("head_repository", {}).get("full_name") == REPOSITORY,
                "artifact_run")
        if args.fresh_producer:
            require(run.get("path") == FILES[0] and run.get("head_branch") == REF.removeprefix("refs/heads/"),
                    "artifact_workflow")
            jobs = fetch(f"/actions/runs/{args.producer_run}/jobs?per_page=100")
            matches = [job for job in jobs.get("jobs", []) if job.get("name") == "Fresh candidate release producer"]
            require(len(matches) == 1 and matches[0].get("status") == "completed"
                    and matches[0].get("conclusion") == "success", "producer_job")
        else:
            require(run.get("conclusion") == "success", "producer_job")
        require(artifact.get("id") == args.artifact_id and artifact.get("name") == "release-bundles"
                and artifact.get("expired") is False and artifact.get("workflow_run", {}).get("id") == args.producer_run
                and artifact.get("workflow_run", {}).get("head_sha") == args.producer_sha
                and artifact.get("digest") == "sha256:" + args.artifact_zip_sha256,
                "artifact_metadata")
        require(datetime.datetime.fromisoformat(artifact["expires_at"].replace("Z", "+00:00")) >
                datetime.datetime.now(datetime.timezone.utc), "artifact_expired")
        data = fetch(f"/actions/artifacts/{args.artifact_id}/zip", LIMITS["zip"], True)
        digest_policy(sha(data), args.artifact_zip_sha256, "zip_digest")
        version = re.search(rb'^version = "([0-9]+\.[0-9]+\.[0-9]+)"$',
                            read_bounded(candidate / "herdr-plugin.toml", 65536), re.M)
        require(version is not None, "producer_version")
        version = version[1].decode()
        archive_name = f"herdr-mobile-relay_{version}_darwin_arm64.tar.gz"
        with zipfile.ZipFile(io.BytesIO(data)) as bundle:
            members = bundle.infolist()
            entries = [zip_entry(item) for item in members]
            entry_policy(entries, "zip")
            selected = {}
            for name in (archive_name, "checksums.txt"):
                matches = [item for item in members if item.filename == name]
                require(len(matches) == 1, "archive_ambiguous")
                with bundle.open(matches[0]) as stream:
                    value = CountReader(stream, "tar" if name == archive_name else "result").read(
                        LIMITS["tar"] + 1 if name == archive_name else 65537)
                path = root / name
                with path.open("xb") as output:
                    output.write(value)
                selected[name] = sha(value)
        checksum_policy(read_bounded(root / "checksums.txt", 65536), archive_name, selected[archive_name])
        archive_preflight(root / archive_name, version, args.producer_sha, budget)
        return version, root / archive_name, selected
    finally:
        token = None
        data = b""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        return None


def archive_preflight(archive, version, revision, budget):
    cap("tar", archive.stat().st_size)
    entries, payload, manifest = [], {}, None
    with archive.open("rb") as compressed:
        with gzip.GzipFile(fileobj=compressed) as inflated:
            with tarfile.open(fileobj=CountReader(inflated, "unpacked"), mode="r|") as bundle:
                for member in bundle:
                    budget.check()
                    require(len(entries) < LIMITS["tar_count"], "archive_count")
                    require(not member.pax_headers and not member.sparse, "archive_extensions")
                    kind = "file" if member.isfile() else "directory" if member.isdir() else "special"
                    entries.append(Entry(member.name, kind, member.size))
                    # Validate before opening this payload; final validation also
                    # detects parent/file conflicts independent of archive order.
                    entry_policy([entries[-1]], "tar")
                    name = member.name.removeprefix("./").removesuffix("/")
                    if kind == "file":
                        stream = bundle.extractfile(member)
                        require(stream is not None, "archive_payload")
                        digest, data, size = hashlib.sha256(), bytearray(), 0
                        while True:
                            budget.check()
                            chunk = stream.read(65536)
                            if not chunk:
                                break
                            size += len(chunk)
                            require(size <= member.size, "archive_payload")
                            digest.update(chunk)
                            if name == "release-manifest.json":
                                data.extend(chunk)
                                cap("receipt", len(data))
                        require(size == member.size, "archive_payload")
                        if name == "release-manifest.json":
                            manifest = strict_json(bytes(data), "receipt")
                        else:
                            payload[name] = digest.hexdigest()
    entry_policy(entries, "tar")
    manifest_policy(manifest, version, revision, payload)
    return manifest


def verify_producer_archive(archive, checksums, archive_hashes, version, revision,
                            release, budget, verify_archive):
    """Bind post-install verification inputs to the producer ZIP's two digests."""
    budget.check()
    require(type(archive_hashes) is dict and set(archive_hashes) == {archive.name, "checksums.txt"},
            "archive_binding")
    archive_bytes = read_bounded(archive, LIMITS["tar"])
    budget.check()
    digest_policy(sha(archive_bytes), archive_hashes[archive.name], "tar_digest")
    checksum_bytes = read_bounded(checksums, 65536)
    budget.check()
    digest_policy(sha(checksum_bytes), archive_hashes["checksums.txt"], "checksum_digest")
    checksum_policy(checksum_bytes, archive.name, archive_hashes[archive.name])
    # Created exclusively AFTER candidate-controlled installs. Use only these
    # bounded, producer-bound copies, not their previously writable input paths.
    # This is cooperative byte binding, NOT hostile-same-UID containment: a
    # concurrent same-UID process could still replace a file between our check
    # and the helper's own re-open. No atomic-path guarantee is claimed.
    captured = release.parent / "producer-verified-inputs"
    captured.mkdir(mode=0o700)
    copied_archive, copied_checksums = captured / archive.name, captured / "checksums.txt"
    for path, data in ((copied_archive, archive_bytes), (copied_checksums, checksum_bytes)):
        budget.check()
        with path.open("xb") as output:
            require(output.write(data) == len(data), "archive_copy_write")
        budget.check()

    def record_digest(actual):
        # The shared helper invokes this BEFORE archive_extract and before its
        # packaged verify-release subprocess. Never bind only its return value.
        budget.check()
        digest_policy(actual, archive_hashes[archive.name], "tar_digest")
        digest_policy(sha(read_bounded(copied_checksums, 65536)),
                      archive_hashes["checksums.txt"], "checksum_digest")
        budget.check()

    return verify_archive(copied_archive, copied_checksums, version, revision, release,
        lambda _: budget.check(), record_digest, lambda *_: None,
        target="darwin/arm64", verifier_timeout=10)


def write_receipt(path, encoded, deadline):
    """A complete write is not a successful command exit or a passing cell."""
    cap("receipt", len(encoded))
    deadline.check()
    with path.open("xb") as destination:
        require(destination.write(encoded) == len(encoded), "receipt_write")
    deadline.check()  # Includes slow write/flush/close; staging may still contain pass bytes.


def publish_receipt(staged, evidence, runner_exit, candidate, run_id, source_hashes,
                    producer_identity, deadline):
    """Offline publication AFTER the runner has exited, with no runtime effects.

    A cell passes only with a successful workflow step/job (runner exit 0) AND
    a published receipt validating as pass for the exact SHA/run/source/producer.
    A receipt alone never establishes pass. This does not atomically couple
    process exit and filesystem publication: a publisher error also fails the
    workflow step, even if its output write completed before that error.
    """
    require(type(runner_exit) is int and 0 <= runner_exit <= 255, "publication_exit")
    require(set(source_hashes) == set(FILES), "publication_sources")
    # Fixed, typed fallback: never forward stale pass bytes or invalid staged
    # fields. Zero counters here describe publication's lack of runtime evidence,
    # not an inferred absence of requests/output in the failed runner.
    value = {"schemaVersion": 1, "candidate_sha": candidate, "source_hashes": source_hashes,
        "producer": producer_identity, "scope": "darwin_arm64_byo_only", "result": "fail",
        "github_run_id": run_id, "github_run_attempt": 1,
        "launchctl_query_grant_id": LAUNCHCTL_GRANT_ID,
        "launchctl_query_pinned_parent": LAUNCHCTL_PINNED_PARENT,
        "failure": "command_failed" if runner_exit else "receipt_validation",
        "request_counts": [], "captured_output_bytes": 0, "dependency_payload_bytes_peak": 0}
    if runner_exit:
        value["failure_detail"] = {"type": "SystemExit", "errno": None, "function": "publish_receipt",
            "line": None, "command": "unknown", "exit_code": runner_exit}
    encode = lambda item: (json.dumps(item, separators=(",", ":")) + "\n").encode()
    require(receipt_policy(encode(value), candidate, source_hashes, producer_identity) is False,
            "publication_identity")
    passed = False
    deadline.check()
    try:
        data = read_bounded(staged, LIMITS["receipt"])
        observed = strict_json(data, "receipt")
        require(type(observed) is dict and type(observed.get("source_hashes")) is dict,
                "receipt_identity")
        observed_sources = observed["source_hashes"]
        require(all(path in source_hashes and digest == source_hashes[path]
                    for path, digest in observed_sources.items()), "receipt_identity")
        qualified = receipt_policy(data, candidate, observed_sources, producer_identity)
        # FAIL may be early/partial; every present run/grant field must still
        # agree. PASS's closed schema requires ALL of these identities.
        require(all(observed.get(key, expected) == expected for key, expected in (
            ("github_run_id", run_id), ("github_run_attempt", 1),
            ("launchctl_query_grant_id", LAUNCHCTL_GRANT_ID),
            ("launchctl_query_pinned_parent", LAUNCHCTL_PINNED_PARENT))), "receipt_identity")
        if qualified:
            require(observed_sources == source_hashes, "receipt_identity")
            if runner_exit == 0:
                value, passed = observed, True
        else:
            value = observed  # Preserve only validated, sanitized failure evidence.
    except (OSError, Refusal):
        pass  # The fixed failure value contains no rejected bytes or error text.
    deadline.check()
    encoded = encode(value)
    require(receipt_policy(encoded, candidate, value["source_hashes"], producer_identity) is passed,
            "publication_result")
    write_receipt(evidence, encoded, deadline)
    return passed


def publication(args):
    # Offline file publication only; this entrypoint cannot authorize any
    # launcher, launchctl, trust, browser, network or packaged-binary operation.
    os.environ.pop("GH_TOKEN", None)
    os.environ.pop("GITHUB_TOKEN", None)
    source, run = os.environ.get("GITHUB_SHA"), os.environ.get("GITHUB_RUN_ID")
    require(type(run) is str and re.fullmatch(r"[1-9][0-9]{0,14}", run)
            and os.environ.get("GITHUB_RUN_ATTEMPT") == "1", "publication_identity")
    run_id = int(run)
    producer(args.producer_sha, args.producer_run, args.artifact_id, args.artifact_zip_sha256,
             args.fresh_producer, source, run_id)
    staged, evidence = args.staged_receipt, args.evidence
    os.umask(0o077)
    deadline = CleanupDeadline(seconds=5)
    with deadline.alarm():
        runner_temp = Path(os.environ["RUNNER_TEMP"]).resolve(strict=True)
        require(staged.is_absolute() and evidence.is_absolute()
                and staged.parent != evidence.parent
                and staged.parent.parent == evidence.parent.parent == runner_temp
                and staged.name == evidence.name == "darwin-package-browser.json"
                and not staged.parent.is_symlink()
                and (not staged.parent.exists() or staged.parent.resolve(strict=True) == staged.parent),
                "resource_path")
        root_policy(evidence.parent.exists(), evidence.parent.is_symlink())
        evidence.parent.mkdir(mode=0o700)
        info = evidence.parent.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) == 0o700
                and evidence.parent.resolve(strict=True) == evidence.parent, "receipt_parent_identity")
        candidate = Path(__file__).resolve().parents[1]
        sources = {path: sha(read_bounded(candidate / path, 8 * 1024 * 1024)) for path in FILES}
        identity = {"sha": args.producer_sha, "run": args.producer_run, "artifact": args.artifact_id,
                    "zip_sha256": args.artifact_zip_sha256, "fresh": args.fresh_producer}
        passed = publish_receipt(staged, evidence, args.runner_exit_code, source, run_id, sources, identity, deadline)
    # A zero-exit runner with absent/invalid/failed staging cannot make the step
    # succeed. A failed runner gets sanitized evidence, then its status is kept
    # by the workflow. No cleanup or retry is performed here.
    return 0 if passed or args.runner_exit_code != 0 else 1


def profiles(root, der, effects):
    effects.effect("trust")
    result = {}
    for name in ("controller", "reader"):
        directory = root / "profiles" / name / "Default"
        directory.mkdir(mode=0o700, parents=True)
        database = directory / "ServerCertificate"
        with sqlite3.connect(database) as db:
            db.execute("CREATE TABLE meta(key LONGVARCHAR NOT NULL UNIQUE PRIMARY KEY,value LONGVARCHAR)")
            db.executemany("INSERT INTO meta VALUES(?,?)", (("version", 1), ("last_compatible_version", 1),
                                                          ("mmap_status", 0)))
            db.execute("CREATE TABLE certificates(sha256hash_hex TEXT PRIMARY KEY,der_cert BLOB NOT NULL,trust_settings BLOB NOT NULL)")
            db.execute("INSERT INTO certificates VALUES(?,?,?)", (sha(der), der, bytes.fromhex("0a020803")))
        database.chmod(0o600)
        prefs = directory / "Preferences"
        prefs.write_text(json.dumps({"certificates": {"ca_platform_integration_enabled": False}}))
        prefs.chmod(0o600)
        result[name] = {"database_sha256": sha(read_bounded(database, 65536)),
                        "preferences_sha256": sha(read_bounded(prefs, 65536))}
    negative = root / "profiles/negative/Default"
    negative.mkdir(mode=0o700, parents=True)
    (negative / "Preferences").write_text(json.dumps({"certificates": {"ca_platform_integration_enabled": False}}))
    (negative / "Preferences").chmod(0o600)
    require(not (negative / "ServerCertificate").exists(), "negative_profile_not_empty")
    result["negative"] = {"database_absent": True,
        "preferences_sha256": sha(read_bounded(negative / "Preferences", 65536))}
    return result


def certificates(root, operations):
    def command(*args):
        result = operations.capture(["openssl", *args], 10)
        require(result.returncode == 0, "certificate_command")
    for ca, subject in (("ca", "Disposable Herdr trusted fixture root"),
                        ("unknown-ca", "Disposable Herdr unknown fixture root")):
        config = root / (ca + ".cnf")
        # Native LibreSSL need not add identifiers implicitly. Python's strict
        # verifier needs them, and distinct issuer names keep the unknown-CA
        # control from selecting the trusted root and reporting signature error.
        config.write_text("[req]\ndistinguished_name=dn\nx509_extensions=ca\nprompt=no\n"
                          "[dn]\nCN=" + subject + "\n[ca]\nbasicConstraints=critical,CA:true\n"
                          "keyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n")
        command("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", str(config),
                "-keyout", str(root / (ca + ".key")), "-out", str(root / (ca + ".pem")))
    for name, ca, san in (("valid", "ca", "IP:127.0.0.1"), ("wrong-host", "ca", "DNS:wrong.invalid"),
                          ("unknown", "unknown-ca", "IP:127.0.0.1")):
        ext = root / (name + ".ext")
        ext.write_text("basicConstraints=critical,CA:false\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
                       "subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid:always\n"
                       "extendedKeyUsage=serverAuth\nsubjectAltName=" + san + "\n")
        command("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=fixture", "-keyout",
                str(root / (name + ".key")), "-out", str(root / (name + ".csr")))
        command("x509", "-req", "-days", "1", "-in", str(root / (name + ".csr")), "-CA", str(root / (ca + ".pem")),
                "-CAkey", str(root / (ca + ".key")), "-set_serial", "1", "-extfile", str(ext), "-out", str(root / (name + ".pem")))
    command("x509", "-in", str(root / "ca.pem"), "-outform", "DER", "-out", str(root / "ca.der"))


class Proxy(http.server.HTTPServer):
    allow_reuse_address = False

    def __init__(self, port, root, leaf, operations):
        require(fixture_port_allowed(port), "fixture_port")
        operations.effects.effect("network")
        self.operations, self.lock = operations, threading.RLock()
        self.workers, self.connections, self.counts = [], set(), {}
        self.failure = False
        self.paths = {"/", "/index.html", "/healthz", "/release.json", "/version.json", "/ws", "/favicon.ico"}
        self.paths.update("/" + p.relative_to(root / "release/web").as_posix()
                          for p in (root / "release/web").rglob("*") if p.is_file())
        self.thread, self.socket = None, None
        self.closing = False
        operations.cleanup.listener(self)
        super().__init__(("127.0.0.1", port), ProxyHandler, bind_and_activate=False)
        self.server_bind()
        self.server_activate()
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(root / (leaf + ".pem"), root / (leaf + ".key"))
        self.socket = context.wrap_socket(self.socket, server_side=True, do_handshake_on_connect=False)
        self.timeout = 0.1
        self.thread = threading.Thread(target=self.serve_owned, daemon=True)
        self.thread.start()

    def serve_owned(self):
        # No BaseServer.shutdown() wait: the owned accept loop has a short
        # timeout and a closing flag. Closure joins against the shared deadline.
        while not self.closing:
            try:
                self.handle_request()
            except Exception as error:
                if not owned_socket_closed(error, self.closing):
                    self.failure = True
                return

    def handle_error(self, _request, _address):
        # BaseServer catches dispatch errors before the accept loop sees them.
        # Latch them without its default raw traceback/address logging.
        self.failure = True

    def process_request(self, connection, address):
        with self.lock:
            if self.closing:
                connection.close()
                return
            self.connections.add(connection)
            worker = threading.Thread(target=self.worker, args=(connection, address), daemon=True)
            self.workers.append(worker)
            worker.start()

    def record(self, label):
        with self.lock:
            self.counts[label] = self.counts.get(label, 0) + 1

    def worker(self, connection, address):
        accepted = False
        try:
            connection.settimeout(5)
            connection.do_handshake()
            accepted = True
            self.record("tls_accepted")
            # BaseRequestHandler also finishes/flushes in its constructor. Its
            # client-side errors belong to the post-handshake abort category.
            self.finish_request(connection, address)
        except CLIENT_ABORTS:
            self.record("client_aborted" if accepted else "tls_refused")
        except Exception as error:
            if not owned_socket_closed(error, self.closing):
                self.failure = True
        finally:
            try:
                connection.close()
            except CLIENT_ABORTS:
                self.record("client_aborted" if accepted else "tls_refused")
            except Exception as error:
                if not owned_socket_closed(error, self.closing):
                    self.failure = True
            finally:
                with self.lock:
                    self.connections.discard(connection)

    def close(self, deadline):
        deadline.check()
        with self.lock:
            self.closing = True
        if self.socket is not None:
            self.server_close()
        with self.lock:
            for connection in list(self.connections):
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                connection.close()
        for thread in [self.thread, *self.workers]:
            if thread is not None and thread.ident is not None:
                thread.join(deadline.remaining(2))

    def closed(self):
        return (self.socket is None or self.socket.fileno() == -1) and (self.thread is None or not self.thread.is_alive()) and all(
            not thread.is_alive() for thread in self.workers)


class ProxyHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def backend_call(self, operation, *args, **kwargs):
        # Never misclassify a reset/timeout on the backend or proxy machinery
        # as a benign browser abort, even if handler.finish later masks it.
        try:
            return operation(*args, **kwargs)
        except Exception:
            self.server.failure = True
            raise Refusal("proxy_backend") from None

    def do_GET(self):
        server = self.server
        # Reject ambiguity before scalar allowlist evaluation, traffic counters,
        # backend allocation or forwarding, including WebSocket upgrade headers.
        hosts, upgrades = self.headers.get_all("Host", []), self.headers.get_all("Upgrade", [])
        require(len(hosts) == 1 and len(upgrades) <= 1, "request_header_multiplicity")
        upgrade = upgrades[0].lower() if upgrades else ""
        require(public_request_allowed(self.path, hosts[0], upgrade, server.paths,
                                       server.server_port), "request_allowlist")
        require("Content-Length" not in self.headers and "Transfer-Encoding" not in self.headers, "request_body")
        server.record("websocket" if upgrade == "websocket" else "http_get")
        # Shipped fragments contain a bare WSS origin, so Chromium requests
        # Upgrade at '/', not '/ws'. Preserve the real upgrade and subprotocol.
        if upgrade == "websocket":
            self.websocket()
            return
        backend = self.backend_call(http.client.HTTPConnection, "127.0.0.1", 18377, timeout=5)
        try:
            self.backend_call(backend.request, "GET", self.path, headers={"Host": self.headers["Host"]})
            response = self.backend_call(backend.getresponse)
            data = self.backend_call(response.read, LIMITS["individual"] + 1)
            cap("individual", len(data))
            headers = self.backend_call(response.getheaders)
            self.send_response(response.status)
            for key, value in headers:
                if key.lower() not in {"connection", "content-length", "transfer-encoding"}:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        finally:
            self.backend_call(backend.close)

    def websocket(self):
        require(self.headers.get("Upgrade", "").lower() == "websocket", "request_upgrade")
        backend = self.backend_call(socket.create_connection, ("127.0.0.1", 18377), timeout=5)
        with self.server.lock:
            self.server.connections.add(backend)
        try:
            headers = "".join(f"{k}: {v}\r\n" for k, v in self.headers.items())
            self.backend_call(backend.sendall, (f"GET {self.path} HTTP/1.1\r\n" + headers + "\r\n").encode("latin1"))
            response = bytearray()
            while b"\r\n\r\n" not in response:
                chunk = self.backend_call(backend.recv, 4096)
                require(bool(chunk), "websocket_upgrade")
                response.extend(chunk)
                cap("result", len(response))
            require(response.startswith(b"HTTP/1.1 101 "), "websocket_upgrade")
            self.connection.sendall(response)
            with selectors.DefaultSelector() as selector:
                self.backend_call(selector.register, backend, selectors.EVENT_READ, self.connection)
                self.backend_call(selector.register, self.connection, selectors.EVENT_READ, backend)
                while True:
                    self.server.operations.budget.check()
                    for key, _ in self.backend_call(selector.select, 0.1):
                        chunk = (self.backend_call(backend.recv, 16384) if key.fileobj is backend
                                 else self.connection.recv(16384))
                        if not chunk:
                            return
                        if key.data is backend:
                            self.backend_call(backend.sendall, chunk)
                        else:
                            self.connection.sendall(chunk)
        finally:
            try:
                self.backend_call(backend.close)
            finally:
                with self.server.lock:
                    self.server.connections.discard(backend)

    def handle_one_request(self):
        try:
            # Keep stdlib parsing and its request-line bound, but dispatch only
            # GET and do not swallow TimeoutError as the stdlib dispatcher does.
            self.raw_requestline = self.rfile.readline(65537)
            require(len(self.raw_requestline) <= 65536, "request_line")
            if not self.raw_requestline:
                self.close_connection = True
                return
            words = self.raw_requestline.split()
            require(words and words[0] == b"GET", "request_method")
            require(self.parse_request(), "request_parse")
            self.do_GET()
            self.wfile.flush()
        except CLIENT_ABORTS:
            self.server.record("client_aborted")
            self.close_connection = True
        except Exception as error:
            if not owned_socket_closed(error, self.server.closing):
                self.server.failure = True
            raise

    def finish(self):
        # The stdlib silently discards socket errors on this final flush.
        # Classify every operation and close both streams, so a later client
        # abort cannot mask an earlier internal error during handler finishing.
        operations = [] if self.wfile.closed else [self.wfile.flush]
        operations.extend((self.wfile.close, self.rfile.close))
        first_error = None
        for operation in operations:
            try:
                operation()
            except CLIENT_ABORTS:
                self.server.record("client_aborted")
            except Exception as error:
                if not owned_socket_closed(error, self.server.closing):
                    self.server.failure = True
                    first_error = first_error or error
        if first_error is not None:
            raise first_error

    def send_error(self, code, message=None, explain=None):
        # Parse errors remain fatal even if the client aborts during the error
        # response, masking the parser's original rejection.
        self.server.failure = True
        super().send_error(code, message, explain)


# No test-root, TLS-ignore, alternate browser, or NSS path in this probe.
TRUST_SCRIPT = r'''
const fs = require('node:fs');
const path = require('node:path');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const {chromium} = require(path.join(input.candidate, 'frontend/node_modules/@playwright/test'));
// Same declarative policy and predicate as the extracted-package driver.
// Normalize case/underscore switch aliases, reject every sandbox mention, and
// the finite zygote/single-process/in-process-GPU isolation-weakening family.
// Do not reject /unsafe/: pinned Playwright uses SwiftShader/self-XSS flags.
const LAUNCH_ARGUMENT_POLICY = {"switchPattern":"^--([A-Za-z][A-Za-z0-9_-]*)(?:=([^\\u0000-\\u001f\\u007f]*))?$","forbiddenPattern":"sandbox|ignore-certificate|ignore-ssl|insecure-localhost|test-root|unsafely-treat-insecure|disable-web-security|allow-running-insecure|host-resolver-rules|proxy-server|proxy-pac|proxy-bypass-list|remote-debugging-port|load-extension|disable-site-isolation","processIsolationNames":["no-zygote","single-process","in-process-gpu"]};
function launchArgumentsAllowed(argv, userDataDir) {
  if (!Array.isArray(argv) || argv.length < 2 || !argv.every((argument) => typeof argument === 'string')) return false;
  const args = argv.slice(1);
  const switches = [];
  const syntax = new RegExp(LAUNCH_ARGUMENT_POLICY.switchPattern);
  const forbidden = new RegExp(LAUNCH_ARGUMENT_POLICY.forbiddenPattern, 'i');
  for (const argument of args) {
    if (argument === 'about:blank') continue;
    const match = syntax.exec(argument);
    if (!match || match[0] !== argument) return false;
    const name = match[1].toLowerCase().replace(/_/g, '-');
    const value = match[2] ?? '';
    if (forbidden.test(name) || forbidden.test(value.replace(/_/g, '-'))
      || LAUNCH_ARGUMENT_POLICY.processIsolationNames.includes(name)) return false;
    switches.push({ name, value });
  }
  const profiles = switches.filter((entry) => entry.name === 'user-data-dir');
  return profiles.length === 1 && profiles[0].value === userDataDir
    && ['--use-mock-keychain', '--enable-automation', '--remote-debugging-pipe'].every((flag) => args.includes(flag))
    && args.some((argument) => argument === '--headless' || argument.startsWith('--headless='));
}
const policyProfile = '/owned-policy-profile';
const policyArgv = ['chromium', '--user-data-dir='+policyProfile, '--use-mock-keychain',
  '--enable-automation', '--remote-debugging-pipe', '--headless', 'about:blank'];
if (!launchArgumentsAllowed(policyArgv, policyProfile)
    || ['--proxy-server=http://invalid', '--proxy-pac-url=http://invalid', '--proxy-bypass-list=*']
      .some(flag => launchArgumentsAllowed([...policyArgv,flag], policyProfile))) {
  throw new Error('launch argument policy regression');
}
const owned = new Set();
const observations = [];
let stopping=false, launchUncertain=false, emitted=false;
function emit(ok, argvOK, version) {
  if (emitted) return;
  emitted=true;
  process.stdout.write(JSON.stringify({ok:ok && !stopping, observations, argv_ok:argvOK, version,
    contexts_closed:owned.size===0 && !launchUncertain})+'\n');
}
async function open(profile) {
  if (stopping) throw new Error('owned browser stopping');
  launchUncertain=true;
  const context = await chromium.launchPersistentContext(profile, {
    headless:true, channel:'chromium', chromiumSandbox:true, args:['--enable-automation'], timeout:15000
  });
  owned.add(context);
  launchUncertain=false;
  if (stopping) throw new Error('owned browser stopping');
  return context;
}
async function close(context) {
  let timer;
  try {
    await Promise.race([context.close(), new Promise((_,reject) => {
      timer=setTimeout(() => reject(new Error('closure deadline')), 5000);
    })]);
    owned.delete(context);
  } finally { clearTimeout(timer); }
}
async function interrupt() {
  if (stopping) return;
  stopping=true;
  const timer=setTimeout(() => process.exit(1), 2800);
  try {
    await Promise.allSettled([...owned].map(close));
    emit(false, false, '');
  } finally {
    clearTimeout(timer);
    process.exit(1);
  }
}
for (const name of ['SIGINT','SIGTERM']) process.on(name, () => { void interrupt(); });
async function check(context, port, refusal) {
  const page = await context.newPage();
  let error = null, response = null;
  try { response=await page.goto('https://127.0.0.1:'+port+'/healthz', {timeout:15000}); }
  catch(e) { error=String(e.message); }
  if (refusal) {
    if (!error || !error.includes(refusal)) throw new Error('unexpected TLS outcome');
  } else if (error || response?.status() !== 200 || !await page.evaluate(() => isSecureContext)) {
    throw new Error('positive TLS control failed');
  }
  await page.close();
}
(async () => {
  let ok=false, argvOK=false, version='';
  try {
    const empty=await open(path.join(input.profiles,'negative'));
    await check(empty,18443,'ERR_CERT_AUTHORITY_INVALID');
    observations.push('empty_profile_refuses_ca');
    await close(empty);
    const trusted=await open(path.join(input.profiles,'controller'));
    const page=trusted.pages()[0] || await trusted.newPage();
    const session=await trusted.newCDPSession(page);
    const info=await session.send('Browser.getVersion');
    version=info.product.replace(/^Chrome\//,'');
    if (version !== '151.0.7922.34') throw new Error('browser identity mismatch');
    const {arguments:argv}=await session.send('Browser.getBrowserCommandLine');
    argvOK=launchArgumentsAllowed(argv, path.join(input.profiles,'controller'));
    if (!argvOK) throw new Error('launch argument mismatch');
    await check(trusted,18443,null); observations.push('trusted_profile_accepts_ip');
    await check(trusted,18444,'ERR_CERT_AUTHORITY_INVALID'); observations.push('trusted_profile_refuses_unknown_ca');
    await check(trusted,18445,'ERR_CERT_COMMON_NAME_INVALID'); observations.push('trusted_profile_refuses_wrong_host');
    await close(trusted);
    ok=true;
  } finally {
    const closures=await Promise.allSettled([...owned].map(close));
    ok=ok && !stopping && !launchUncertain && owned.size===0 && closures.every(v=>v.status==='fulfilled');
    emit(ok, argvOK, version);
    process.exitCode=ok?0:1;
  }
})().catch(()=>{process.exitCode=1;});
'''


def tls_controls(root, operations, binary, version, revision, candidate):
    outcomes = []
    for port, expected in ((18443, "positive"), (18444, "unknown_ca"), (18445, "wrong_host")):
        origin = f"https://127.0.0.1:{port}"
        context = ssl.create_default_context(cafile=str(root / "ca.pem"))
        python_code = None
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=3) as plain:
                with context.wrap_socket(plain, server_hostname="127.0.0.1"):
                    require(expected == "positive", "python_tls_negative_accepted")
        except ssl.SSLCertVerificationError as error:
            python_code = error.verify_code
            require(expected != "positive" and ((expected == "wrong_host" and error.verify_code == 64)
                    or (expected == "unknown_ca" and error.verify_code in {20, 21})), "python_tls_refusal")
        curl = operations.capture(["curl", "--noproxy", "*", "--fail", "--silent", "--show-error",
                                   "--max-time", "5", "--max-redirs", "0", origin + "/healthz"], 6)
        go = operations.capture([str(binary), "verify-public", "--origin", origin, "--web-root",
                                 str(root / "release/web"), "--version", version, "--revision", revision], 10)
        if expected == "positive":
            require(curl.returncode == 0 and go.returncode == 0, "tls_positive")
        else:
            require(curl.returncode == 60 and go.returncode != 0, "tls_negative_exit")
            pattern = rb"unknown authority" if expected == "unknown_ca" else rb"certificate.*(?:IP|127\.0\.0\.1)|IP.*certificate"
            require(re.search(pattern, go.stderr, re.I) is not None
                    and (expected != "wrong_host" or b"unknown authority" not in go.stderr.lower()), "go_tls_refusal_category")
            curl_pattern = (rb"unable to get local issuer|self.signed certificate|unable to verify|invalid certificate chain|certificate chain is not trusted" if expected == "unknown_ca"
                            else rb"no alternative certificate subject name matches|does not match target|IP address mismatch")
            require(re.search(curl_pattern, curl.stderr, re.I) is not None, "curl_tls_refusal_category")
        outcomes.append({"category": expected,
            "python": {"category": expected, "verify_code": python_code},
            "curl": {"category": expected, "exit_code": curl.returncode},
            "packaged_go": {"category": expected, "exit_code": go.returncode}})
    script = root / "trust-controls.cjs"
    script.write_text(TRUST_SCRIPT)
    # Playwright owns detached browser groups, not the Node parent's group.
    # A killed/malformed driver cannot attest their retirement: preserve that
    # uncertainty even if the tracked Node process itself was reaped.
    value = None
    try:
        result = operations.capture(["node", str(script)], 90, json.dumps({
            "candidate": str(candidate), "profiles": str(root / "profiles")}).encode(), result_cap=LIMITS["result"])
        value = strict_json(result.stdout)
    finally:
        if type(value) is not dict or value.get("contexts_closed") is not True:
            operations.cleanup.error("browser_contexts_unobserved")
    require(result.returncode == 0 and type(value) is dict and value.get("ok") is True
            and value.get("contexts_closed") is True and value.get("argv_ok") is True
            and value.get("version") == "151.0.7922.34" and value.get("observations") == [
                "empty_profile_refuses_ca", "trusted_profile_accepts_ip", "trusted_profile_refuses_unknown_ca",
                "trusted_profile_refuses_wrong_host"], "browser_trust_controls")
    return {"python_curl_packaged_go": outcomes, "browser": value}


def runtime(args):
    budget = Budget()  # Admission acquisition consumes the unchanged phase/global caps.
    ci_context = {"system": platform.system(), "machine": platform.machine(),
        "enabled": os.environ.get("HERDR_DARWIN_PACKAGE_BROWSER_CI"),
        "repository": os.environ.get("GITHUB_REPOSITORY"), "ref": os.environ.get("GITHUB_REF"),
        "attempt": os.environ.get("GITHUB_RUN_ATTEMPT"), "event_name": os.environ.get("GITHUB_EVENT_NAME"),
        "candidate_sha": os.environ.get("GITHUB_SHA"), "run_id": os.environ.get("GITHUB_RUN_ID"),
        "runner_environment": os.environ.get("RUNNER_ENVIRONMENT"), "image_os": os.environ.get("ImageOS")}
    require(ci_context_allowed(ci_context), "ci_admission")
    require(launchctl_packet_allowed(args.launchctl_query_grant), "launchctl_authority")
    candidate = Path(__file__).resolve().parents[1]
    event_path = os.environ.get("GITHUB_EVENT_PATH")
    observe = lambda: first_push_observation(ci_context, event_path, candidate)
    launchctl_context = observe()
    require(launchctl_authorized(args.launchctl_query_grant, launchctl_context), "launchctl_authority")
    source, run_id = launchctl_context["candidate_sha"], int(launchctl_context["run_id"])
    producer(args.producer_sha, args.producer_run, args.artifact_id, args.artifact_zip_sha256,
             args.fresh_producer, source, run_id)
    # Runtime evidence is PRIVATE STAGING ONLY, never the workflow upload path.
    # Publication is a separate offline invocation after this command exits.
    root, evidence = args.private_root, args.evidence
    paths_admission(root, evidence, Path(os.environ["RUNNER_TEMP"]).resolve(strict=True), args.budget_seconds)
    os.umask(0o077)
    root_identity, evidence_identity = None, None
    cleanup = Cleanup()
    operations = Operations(root, {}, budget, cleanup, EffectGate(True,
        launchctl_grant=args.launchctl_query_grant, launchctl_context=launchctl_context, launchctl_observe=observe))
    receipt = {"schemaVersion": 1, "candidate_sha": source, "result": "fail", "phases": [],
               "launchctl_query_grant_id": LAUNCHCTL_GRANT_ID, "launchctl_query_pinned_parent": LAUNCHCTL_PINNED_PARENT,
               "github_run_id": run_id, "github_run_attempt": int(launchctl_context["attempt"]),
               "producer": {"sha": args.producer_sha, "run": args.producer_run, "artifact": args.artifact_id,
                            "zip_sha256": args.artifact_zip_sha256, "fresh": args.fresh_producer},
               "cases": [], "request_counts": [], "cleanup": False, "scope": "darwin_arm64_byo_only",
               "owned_cleanup": cleanup.evidence}
    initial_sources = {}
    failure, proxy_servers, socket_fixture = None, [], None
    previous_signals = {}

    def interrupted(signum, _):
        raise Refusal("handled_signal_" + str(signum))

    failed_command = []

    def command(argv, timeout=10, cwd=None):
        result = operations.capture(argv, timeout, cwd=cwd)
        if result.returncode != 0:
            failed_command[:] = [command_label(argv), result.returncode]
        require(result.returncode == 0, "command_failed")
        return result.stdout

    @contextmanager
    def phase(index):
        budget.enter(index)
        if index == 0:
            budget.phase_start = budget.start  # Admission/root setup belongs to the 90s phase.
        start, completed = budget.phase_start, False
        try:
            with budget.alarm():
                yield
                budget.check()
            completed = True
        finally:
            receipt["phases"].append({"id": index, "ok": completed, "exit_code": 0 if completed else 1,
                "cap_seconds": PHASES[index], "seconds": int((time.monotonic() - start) * 1000) / 1000})

    try:
        for signum in (signal.SIGINT, signal.SIGTERM):
            previous_signals[signum] = signal.signal(signum, interrupted)
        # Every allocation after admission is inside the failure cleanup scope
        # and the hydration deadline, including root/environment setup.
        with phase(0):
            root.mkdir(mode=0o700)
            root_identity = root.lstat()
            evidence.parent.mkdir(mode=0o700)
            evidence_identity = evidence.parent.lstat()
            require(stat.S_ISDIR(evidence_identity.st_mode) and evidence_identity.st_uid == os.getuid()
                    and stat.S_IMODE(evidence_identity.st_mode) == 0o700, "receipt_parent_identity")
            operations.env = environment(root)
            initial_sources = {path: sha(read_bounded(candidate / path, 8 * 1024 * 1024)) for path in FILES}
            receipt["source_hashes"] = initial_sources
            receipt["source_proof"] = {"chromium_commit": "782af9cb30a53f54487e5d2e44738645a8ec457c",
                "playwright_tag": "v1.62.1", "go_tag": "go1.27.1", "trust": "TRUST-PROOF.md"}
            require(command(["git", "rev-parse", "HEAD"]).decode().strip() == source, "candidate_head")
            require(not command(["git", "status", "--porcelain=v1", "--untracked-files=all"]), "candidate_dirty")
            changed = command(["git", "diff", "--name-only", BASE, source, "--"]).decode().splitlines()
            require(set(changed) == set(FILES), "candidate_scope")
            receipt["candidate_tree"] = command(["git", "rev-parse", "HEAD^{tree}"]).decode().strip()
            version, archive, hashes = hydrate(args, root, budget, candidate, operations.effects)
            receipt["archive_hashes"] = hashes
            receipt["version"] = version
        with phase(1):
            tools = {"python": platform.python_version()}
            for name, argv in (("node", ["node", "--version"]), ("bun", ["bun", "--version"]),
                               ("go", ["go", "version"]), ("curl", ["curl", "--version"]),
                               ("openssl", ["openssl", "version"])):
                output = command(argv)
                match = re.search(rb"[0-9]+\.[0-9]+\.[0-9]+[a-z]?", output)
                require(match is not None, "tool_version")
                tools[name] = match[0].decode("ascii")
            require(tools["node"] == "24.21.0" and tools["bun"] == "1.4.0"
                    and tools["go"] == "1.27.1", "tool_identity")
            receipt["tools"] = tools
            command(["bun", "install", "--frozen-lockfile"], 290, candidate / "frontend")
            command(["node", str(candidate / "frontend/node_modules/@playwright/test/cli.js"),
                     "install", "--no-shell", "chromium"], 290)
            browser_manifest = strict_json(read_bounded(candidate / "frontend/node_modules/playwright-core/browsers.json", 65536))
            browser = [b for b in browser_manifest["browsers"] if b["name"] == "chromium"]
            require(len(browser) == 1 and browser[0]["revision"] == "1234"
                    and browser[0]["browserVersion"] == "151.0.7922.34", "browser_manifest")
            require(strict_json(read_bounded(candidate / "frontend/node_modules/@playwright/test/package.json", 65536))["version"] == "1.62.1",
                    "playwright_version")
            executables = list((root / "browsers").glob("chromium-1234/**/Contents/MacOS/Google Chrome for Testing"))
            require(len(executables) == 1, "browser_executable")
            browser_bytes = read_bounded(executables[0], LIMITS["dependencies"])
            require(browser_bytes[:8] == bytes.fromhex("cffaedfe0c000001"), "browser_native_arm64")
            receipt["browser_identity"] = {"sha256": sha(browser_bytes), "target": "darwin/arm64",
                "version": "151.0.7922.34", "revision": "1234", "playwright": "1.62.1",
                # Identity of the shipped bundle that generates Chromium's default
                # switches (Playwright 1.62 no longer ships chromiumSwitches.js).
                "switches_sha256": sha(read_bounded(candidate / "frontend/node_modules/playwright-core/lib/coreBundle.js", 32 * 1024 * 1024))}
            operations.storage()
        with phase(2):
            operations.effects.effect("go")
            (root / "bin").mkdir(mode=0o700)
            # Default Go download directories are 0555 and cannot be retired by
            # rmtree as the unprivileged runner. Keep only this fresh private
            # module cache writable; -mod=readonly still protects the module.
            command(["go", "build", "-modcacherw", "-trimpath", "-o", str(root / "bin/fake-herdr"), "./cmd/fake-herdr"], 200, candidate)
            command(["go", "test", "-modcacherw", "-c", "-o", str(root / "bin/dev-printer.test"), "./cmd/herdr-mobile-relay"], 200, candidate)
        with phase(3):
            spec = importlib.util.spec_from_file_location("extracted_package_fixture", candidate / FILES[2])
            helper = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(helper)

            def helper_run(argv, **kwargs):
                return operations.capture(argv, kwargs.get("timeout") or 10,
                                          cwd=kwargs.get("cwd"), env=kwargs.get("env"))
            helper.subprocess = SimpleNamespace(run=helper_run, DEVNULL=subprocess.DEVNULL,
                PIPE=subprocess.PIPE, TimeoutExpired=subprocess.TimeoutExpired)
            _, binary = verify_producer_archive(archive, root / "checksums.txt", hashes, version,
                args.producer_sha, root / "release", budget, helper.verify_archive)
            receipt["manifest_sha256"] = sha(read_bounded(root / "release/release-manifest.json", LIMITS["receipt"]))
            receipt["binary_sha256"] = sha(read_bounded(binary, LIMITS["individual"]))
            compiler = command(["go", "version", "-m", str(binary)])
            require(b"go1.27.1" in compiler and b"GOOS=darwin" in compiler and b"GOARCH=arm64" in compiler,
                    "packaged_compiler")
            receipt["compiler"] = {"version": "go1.27.1", "target": "darwin/arm64", "metadata_sha256": sha(compiler)}
            certificates(root, operations)
            root_der = read_bounded(root / "ca.der", 65536)
            receipt["profile_inputs"] = profiles(root, root_der, operations.effects)
            receipt["profile_policy"] = {"schema": 1, "root_der_sha256": sha(root_der), "trust_blob": "0a020803",
                "platform_integration": False, "sandbox": True, "mock_keychain": True}
            (root / "empty-roots").mkdir(mode=0o700)
            env = operations.env
            env.update({"SSL_CERT_FILE": str(root / "ca.pem"), "SSL_CERT_DIR": str(root / "empty-roots"),
                        "GODEBUG": "x509sslcertoverrideplatform=1", "CURL_CA_BUNDLE": str(root / "ca.pem")})
            (root / "runtime").mkdir(mode=0o700)
            scenario = root / "scenario.json"
            scenario.write_text(json.dumps({"panes": [{"pane_id": "workspace:agent", "terminal_id": "fixture-terminal",
                "agent": "codex", "name": "Package fixture", "agent_status": "idle", "workspace_id": "workspace",
                "tab_id": "tab", "cwd": str(root), "revision": 1}],
                "workspaces": [{"workspace_id": "workspace", "label": "Package fixture"}],
                "tabs": [{"tab_id": "tab", "workspace_id": "workspace", "label": "Package fixture", "cwd": str(root)}],
                "content": {"workspace:agent": "Harmless fixture output"}}))
            socket_fixture = helper.HerdrSocketFixture(root / "runtime/herdr.sock", scenario,
                root / "socket-operations.json", on_owned=lambda fixture: cleanup.listener(SocketOwner(fixture)))
            socket_fixture.wait_ready()
            def socket_allowed():
                with socket_fixture.lock:
                    return (socket_operations_allowed(socket_fixture.counts)
                            and (socket_fixture.stopped.is_set() or socket_fixture.thread.is_alive()))
            operations.add_guard("herdr_socket", socket_allowed)
            for port, leaf in ((18443, "valid"), (18444, "unknown"), (18445, "wrong-host")):
                proxy_servers.append(Proxy(port, root, leaf, operations))
            operations.add_guard("proxy", lambda: proxy_operations_allowed(proxy_servers))
            relay_key, instance = os.urandom(16).hex(), os.urandom(16).hex()
            env_file = root / "runtime/relay.env"
            env_file.write_text(f"HERDR_RELAY_TOKEN={relay_key}\nHERDR_RELAY_INSTANCE_ID={instance}\n"
                "HERDR_RELAY_HOST=127.0.0.1\nHERDR_RELAY_PORT=18377\nHERDR_RELAY_PLUGIN_PORT=18378\n"
                "HERDR_RELAY_REARM_BOOTSTRAP=0\nHERDR_REACHABILITY_PORT_MAPPING=0\n")
            env_file.chmod(0o600)
            env.update({"HERDR_RELAY_ENV": str(env_file), "HERDR_RELAY_PORT": "18377", "HERDR_RELAY_PLUGIN_PORT": "18378",
                "HERDR_RELAY_HOST": "127.0.0.1", "HERDR_PHONE_APP_URL": ORIGIN, "HERDR_WEB_ROOT": str(root / "release/web"),
                "HERDR_RELEASE_ROOT": str(root / "data/herdr-mobile-relay"), "HERDR_BIN": str(root / "bin/fake-herdr"),
                "HERDR_SOCKET_PATH": str(root / "runtime/herdr.sock"), "HERDR_REACHABILITY_PORT_MAPPING": "0",
                "FAKE_HERDR_SCENARIO": str(scenario), "FAKE_HERDR_OPERATIONS": str(root / "fake-operations.jsonl")})
            require("HERDR_RELAY_TRANSPORT" not in env and "GH_TOKEN" not in env and "GITHUB_TOKEN" not in env, "child_environment")
            launcher_args = ["/bin/bash", str(root / "release/relay/tailscale-external.sh"), "--origin", ORIGIN]
            fake_path = root / "fake-operations.jsonl"
            def fake_allowed():
                if fake_path.exists():
                    fake_operation_summary(read_bounded(fake_path, 65536))
                return True
            operations.add_guard("fake_herdr", fake_allowed)
            wrapper_path = ("/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:"
                + env["HOME"] + "/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + env["PATH"])
            launchctl_path = shutil.which("launchctl", path=wrapper_path)
            require(service_inputs_allowed(False, False, launchctl_path), "service_guard_command_provenance")
            receipt["service_guard"] = {"uid": os.getuid(), "label": "com.herdr-mobile-relay.service",
                "starts": 0, "command": "launchctl_print_gui_label", "launchctl_path": launchctl_path,
                "launchctl_sha256": sha(read_bounded(Path(launchctl_path), LIMITS["individual"])),
                "source_guard_sha256": sha(read_bounded(root / "release/relay/common.sh", LIMITS["individual"])),
                "source_launcher_sha256": sha(read_bounded(root / "release/relay/tailscale-external.sh", LIMITS["individual"])),
                "observations": []}
            def start_launcher():
                guard = receipt["service_guard"]
                require(guard["starts"] < 2, "service_guard_query_cap")
                plist = Path(env["HOME"]) / "Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
                require(service_inputs_allowed(plist.exists(), plist.is_symlink(),
                                               shutil.which("launchctl", path=wrapper_path)), "private_service_definition")
                require(shutil.which("launchctl", path=wrapper_path) == launchctl_path
                        and sha(read_bounded(Path(launchctl_path), LIMITS["individual"])) == guard["launchctl_sha256"],
                        "service_guard_command_drift")
                child, setup = operations.capture_launcher(launcher_args, guard, cwd=root / "release", link_origin=ORIGIN)
                require(service_inputs_allowed(plist.exists(), plist.is_symlink(),
                                               shutil.which("launchctl", path=wrapper_path)), "private_service_definition")
                require(shutil.which("launchctl", path=wrapper_path) == launchctl_path
                        and sha(read_bounded(Path(launchctl_path), LIMITS["individual"])) == guard["launchctl_sha256"],
                        "service_guard_command_drift")
                guard["observations"].append({"ordinal": guard["starts"],
                    "private_plist_before_absent": True, "private_plist_after_absent": True,
                    "setup_ready_seconds": child.startup_seconds, "loaded_service": False,
                    # The unchanged guard redirects query output. These are
                    # source-bound branch observations, NOT a syscall trace or
                    # an observed numeric launchctl exit/duration. The enclosing
                    # wrapper deadline bounds the query. Never add a third probe.
                    "proof_kind": "source_bound_wrapper_continued", "query_exit_zero": False,
                    "numeric_query_exit_observed": False})
                return child, setup
            launcher, link = start_launcher()
            operations.effects.effect("browser")
            receipt["trust_controls"] = tls_controls(root, operations, binary, version, args.producer_sha, candidate)
            dev_url, dev_origin = helper.development_setup_link(root / "bin/dev-printer.test", link, ORIGIN,
                relay_https_origin=ORIGIN, temporary_parent=root / "tmp")
            browser_input = {"fixture_platform": PLATFORM, "setup_url": link, "dev_setup_url": dev_url,
                "dev_relay_origin": dev_origin, "origin": ORIGIN, "profiles": str(root / "profiles"),
                "fake_herdr_operations": str(root / "fake-operations.jsonl"), "herdr_socket_operations": str(root / "socket-operations.json")}
            for mode in ("enroll", "reprint", "restart"):
                if mode == "reprint":
                    output = command(["/bin/bash", str(root / "release/relay/setup-link.sh")], 20, root / "release")
                    require(private_link(output, ORIGIN) is not None, "reprint_link")
                elif mode == "restart":
                    cleanup.stop(launcher, CleanupDeadline(min(10, budget.remaining())))
                    launcher, restart_link = start_launcher()
                    require(restart_link is not None, "restart_link")
                browser_payload = json.dumps({**browser_input, "mode": mode}).encode()
                browser_closure = None
                try:
                    result = operations.capture(["node", str(candidate / FILES[3])], 150,
                        browser_payload, result_cap=LIMITS["result"])
                    browser_closure = strict_json(result.stdout)
                finally:
                    if (type(browser_closure) is not dict or browser_closure.get("mode") != mode
                            or browser_closure.get("fixture_platform") != PLATFORM
                            or browser_closure.get("owned_contexts_closed") is not True):
                        cleanup.error("browser_contexts_unobserved")
                observation = browser_result(result.stdout, mode, result.returncode)
                observation.update(seconds=result.seconds, stdin_bytes=len(browser_payload))
                receipt["cases"].append(observation)
                operations.check()
            operations.check()
            receipt["request_counts"] = proxy_request_counts(proxy_servers)
            receipt["fixture_operations"] = {
                "fake": fake_operation_summary(read_bounded(fake_path, 65536), complete=True),
                "socket": socket_fixture.operation_summary()}
            require(any(row["command"] == "agent prompt" and row["outcome"] == "succeeded" and row["count"] == 1
                        for row in receipt["fixture_operations"]["fake"]), "fake_controller_command_missing")
            operations.storage()
            require(initial_sources == {path: sha(read_bounded(candidate / path, 8 * 1024 * 1024)) for path in FILES}, "source_drift")
            require(not command(["git", "status", "--porcelain=v1", "--untracked-files=all"]), "candidate_dirty")
    except (Exception, SystemExit, KeyboardInterrupt) as error:
        failure = safe_failure_code(error)
        receipt["failure_detail"] = failure_detail(error, failed_command or None)
    finally:
        os.environ.pop("GH_TOKEN", None)
        os.environ.pop("GITHUB_TOKEN", None)
        start = time.monotonic()
        # Reserve receipt time WITHIN the same immutable end, not a fresh
        # deadline after cleanup. Work exhaustion can then retain failure
        # observations in a bounded receipt without any further cleanup effects.
        deadline = CleanupDeadline(latest=budget.start + LIMITS["total_seconds"], reserve=1)
        try:
            try:
                with deadline.alarm():
                    receipt["owned_cleanup"] = cleanup.close(deadline)
                    # Final observations follow every owned closure/reap: late
                    # failures cannot race the last pre-cleanup acceptance check.
                    try:
                        operations.check_guards(deadline.check)
                        if failure is None and socket_fixture is not None:
                            receipt["fixture_operations"] = {"socket": socket_fixture.operation_summary(),
                                "fake": fake_operation_summary(read_bounded(fake_path, 65536), complete=True)}
                    except Exception as error:
                        if failure is None:
                            failure = safe_failure_code(error)
                            receipt["failure_detail"] = failure_detail(error)
                    # Even a late guard failure still attempts owned-root cleanup.
                    if root_identity is not None:
                        remove_owned_tree(root, root_identity, deadline)
                    else:
                        require(not root.exists() and not root.is_symlink(), "cleanup_root_unowned")
                    deadline.check()
                    receipt["owned_cleanup"]["root_deleted"] = True
                    receipt["cleanup"] = True
            except Exception as error:
                receipt["cleanup"] = False
                cleanup.error("cleanup_deadline" if isinstance(error, Refusal)
                              and str(error) == "cleanup_deadline" else "cleanup_incomplete")
                failure = failure or "cleanup_uncertain"
            for signum, previous in previous_signals.items():
                signal.signal(signum, previous)
            # Only serialization may consume the reserve. No closure, deletion,
            # signal or retry is performed after entering this final phase.
            deadline.reserve = 0
            with deadline.alarm():
                seconds = time.monotonic() - start
                require(seconds < PHASES[4], "cleanup_deadline")
                receipt["phases"].append({"id": 4, "ok": receipt["cleanup"],
                    "exit_code": 0 if receipt["cleanup"] else 1, "cap_seconds": PHASES[4], "seconds": round(seconds, 3)})
                receipt["captured_output_bytes"] = operations.raw
                receipt["dependency_payload_bytes_peak"] = operations.storage_peak
                # Snapshot even if cleanup or a final guard failed. Failed cells
                # may have fewer than three allocated proxies, but never raw data.
                receipt["request_counts"] = proxy_request_counts(proxy_servers)
                if operations.failure_guard is not None:
                    receipt["failure_guard"] = operations.failure_guard
                receipt["result"] = "pass" if failure is None and receipt["cleanup"] and len(receipt["cases"]) == 3 else "fail"
                if failure:
                    receipt["failure"] = failure if failure in SAFE_FAILURES else "fixture_refused"
                receipt["source_hashes"] = initial_sources
                encoded = (json.dumps(receipt, separators=(",", ":")) + "\n").encode()
                try:
                    qualified = receipt_policy(encoded, source, initial_sources, receipt["producer"])
                    require(qualified == (receipt["result"] == "pass"), "receipt_result_agreement")
                except Refusal:
                    receipt["result"], receipt["failure"] = "fail", "receipt_validation"
                    encoded = (json.dumps(receipt, separators=(",", ":")) + "\n").encode()
                    require(receipt_policy(encoded, source, initial_sources, receipt["producer"]) is False,
                            "receipt_failure_validation")
                cap("receipt", len(encoded))
                deadline.check()
                require(evidence_identity is not None, "receipt_parent_unowned")
                current_evidence = evidence.parent.lstat()
                require(stat.S_ISDIR(current_evidence.st_mode) and evidence.parent.resolve(strict=True) == evidence.parent
                        and (current_evidence.st_dev, current_evidence.st_ino) == (
                            evidence_identity.st_dev, evidence_identity.st_ino), "receipt_parent_drift")
                # Even a complete staged PASS followed by a deadline/signal
                # failure cannot be published as PASS: publication requires the
                # actual completed command's exit status, not these private bytes.
                write_receipt(evidence, encoded, deadline)
        finally:
            for signum, previous in previous_signals.items():
                signal.signal(signum, previous)
    return 0 if receipt["result"] == "pass" else 1


class SocketOwner:
    def __init__(self, fixture):
        self.fixture = fixture

    def close(self, deadline):
        deadline.check()
        fixture = self.fixture
        early_death = (fixture.thread.ident is not None and not fixture.thread.is_alive()
                       and not fixture.stopped.is_set())
        fixture.stopped.set()
        if fixture.listener is not None:
            fixture.listener.close()
        with fixture.lock:
            for connection in list(fixture.connections):
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                connection.close()
        for thread in [fixture.thread, *fixture.workers]:
            if thread.ident is not None:
                thread.join(deadline.remaining(2))
        deadline.check()
        fixture.path.unlink(missing_ok=True)
        require(not early_death, "fixture_listener_early_death")

    def closed(self):
        return (self.fixture.listener is None or self.fixture.listener.fileno() == -1) and all(not thread.is_alive()
            for thread in [self.fixture.thread, *self.fixture.workers])


def offline_check(group):
    """Finite assertions against the same policies used above; no runtime import."""
    require(group in OFFLINE, "offline_group")
    cases = []

    def evaluated(case_id, operation):
        try:
            ok = operation() is True
        except Exception:
            ok = False
        cases.append({"id": case_id, "ok": ok})

    def rejects(operation):
        try:
            operation()
        except Refusal:
            return True
        return False

    valid = {"mode": "enroll", "fixture_platform": PLATFORM, "result": "pass", "passed_cases": list(ENROLL),
             "stage": "browser_complete", "owned_contexts_closed": True,
             **{key: True for key in ("controller_enrolled", "controller_read", "controller_command",
             "reader_enrolled", "reader_read", "reader_mutation_denied", "credentials_preserved",
             "app_worker_verified", "notification_delivery_disabled", "launch_arguments_verified")}}
    encode = lambda value: json.dumps(value).encode()
    if group == "parser":
        sources = {path: "1" * 64 for path in FILES}
        producer_id = {"sha": BASE, "run": 1, "artifact": 1, "zip_sha256": "2" * 64, "fresh": True}
        valid_receipt = {
            "schemaVersion": 1, "candidate_sha": BASE, "candidate_tree": "3" * 40,
            "source_hashes": sources, "producer": producer_id, "scope": "darwin_arm64_byo_only",
            "result": "pass", "version": "1.2.3", "cleanup": True,
            "launchctl_query_grant_id": LAUNCHCTL_GRANT_ID, "launchctl_query_pinned_parent": LAUNCHCTL_PINNED_PARENT,
            "github_run_id": 1, "github_run_attempt": 1,
            "archive_hashes": {"herdr-mobile-relay_1.2.3_darwin_arm64.tar.gz": "4" * 64, "checksums.txt": "5" * 64},
            "manifest_sha256": "6" * 64, "binary_sha256": "7" * 64,
            "compiler": {"version": "go1.27.1", "target": "darwin/arm64", "metadata_sha256": "8" * 64},
            "browser_identity": {"sha256": "9" * 64, "version": "151.0.7922.34", "revision": "1234",
                "playwright": "1.62.1", "target": "darwin/arm64", "switches_sha256": "a" * 64},
            "tools": {"python": "3.14.0", "node": "24.21.0", "bun": "1.4.0", "go": "1.27.1",
                      "curl": "8.7.1", "openssl": "3.3.6"},
            "source_proof": {"chromium_commit": "782af9cb30a53f54487e5d2e44738645a8ec457c",
                "playwright_tag": "v1.62.1", "go_tag": "go1.27.1", "trust": "TRUST-PROOF.md"},
            "profile_policy": {"schema": 1, "root_der_sha256": "b" * 64, "trust_blob": "0a020803",
                "platform_integration": False, "sandbox": True, "mock_keychain": True},
            "profile_inputs": {name: {"preferences_sha256": "c" * 64, **(
                {"database_absent": True} if name == "negative" else {"database_sha256": "d" * 64})}
                for name in ("controller", "reader", "negative")},
            "phases": [{"id": index, "ok": True, "exit_code": 0, "cap_seconds": seconds, "seconds": 1}
                       for index, seconds in enumerate(PHASES)],
            "cases": [{**browser_result(encode({**valid, "mode": mode, "passed_cases": list(
                ENROLL if mode == "enroll" else PRESERVE)}), mode, 0), "seconds": 1, "stdin_bytes": 1}
                      for mode in ("enroll", "reprint", "restart")],
            "trust_controls": {"browser": {"ok": True, "observations": ["empty_profile_refuses_ca", "trusted_profile_accepts_ip",
                "trusted_profile_refuses_unknown_ca", "trusted_profile_refuses_wrong_host"], "argv_ok": True,
                "version": "151.0.7922.34", "contexts_closed": True},
                "python_curl_packaged_go": [{"category": category,
                    "python": {"category": category, "verify_code": code},
                    "curl": {"category": category, "exit_code": 0 if category == "positive" else 60},
                    "packaged_go": {"category": category, "exit_code": 0 if category == "positive" else 1}}
                    for category, code in (("positive", None), ("unknown_ca", 20), ("wrong_host", 64))]},
            "service_guard": {"uid": 501, "label": "com.herdr-mobile-relay.service", "starts": 2,
                "command": "launchctl_print_gui_label", "launchctl_path": "/bin/launchctl",
                "launchctl_sha256": "e" * 64, "source_guard_sha256": "f" * 64, "source_launcher_sha256": "0" * 64,
                "observations": [{"ordinal": index, "private_plist_before_absent": True,
                    "private_plist_after_absent": True, "setup_ready_seconds": 1, "loaded_service": False,
                    "proof_kind": "source_bound_wrapper_continued", "query_exit_zero": False,
                    "numeric_query_exit_observed": False} for index in (1, 2)]},
            "owned_cleanup": {"children": 10, "listeners": 6, "surviving_children": 0, "surviving_listeners": 0,
                "observed_surviving_children": 0, "observed_surviving_listeners": 0,
                "unobserved_children": 0, "unobserved_listeners": 0, "closure_errors": [], "uncertain": False, "root_deleted": True},
            "request_counts": [{"http_get": 10, "websocket": 2, "tls_accepted": 12, "tls_refused": 1},
                               {"tls_refused": 3}, {"tls_refused": 3}],
            "fixture_operations": {"fake": [{"command": "agent prompt", "outcome": "succeeded", "count": 1}],
                "socket": [{"method": method, "outcome": "succeeded", "count": count}
                           for method, count in (("ping", 1), ("pane.read", 2))]},
            "captured_output_bytes": 100, "dependency_payload_bytes_peak": 1000,
        }
        def receipt_reject(path, replacement, failed=False):
            value = strict_json(encode(valid_receipt), "receipt")
            if failed:
                value.update(result="fail", failure="unexpected_exception")
            selected = value
            for part in path[:-1]:
                selected = selected[part]
            selected[path[-1]] = replacement
            return rejects(lambda: receipt_policy(encode(value), BASE, sources, producer_id))
        def request_receipts():
            with_aborts = {**valid_receipt, "request_counts": [
                {**request, "client_aborted": 2} for request in valid_receipt["request_counts"]]}
            require(receipt_policy(encode(with_aborts), BASE, sources, producer_id), "offline_abort_receipt")
            failed = {**with_aborts, "result": "fail", "failure": "owned_operation_allowlist", "failure_guard": "proxy"}
            require(receipt_policy(encode(failed), BASE, sources, producer_id) is False, "offline_failed_guard_receipt")
            for name in GUARDS:
                require(receipt_policy(encode({**failed, "failure_guard": name}), BASE, sources, producer_id) is False,
                        "offline_named_guard_receipt")
            partial = {**failed, "failure": "command_failed", "request_counts": []}
            require(receipt_policy(encode(partial), BASE, sources, producer_id) is False, "offline_partial_request_receipt")
            missing = dict(failed)
            del missing["request_counts"]
            require(rejects(lambda: receipt_policy(encode(missing), BASE, sources, producer_id)), "offline_missing_counts")
            missing = dict(failed)
            del missing["failure_guard"]
            require(rejects(lambda: receipt_policy(encode(missing), BASE, sources, producer_id)), "offline_missing_guard")
            for bad in (None, "unknown", "https://invalid", 1, []):
                require(rejects(lambda: receipt_policy(encode({**failed, "failure_guard": bad}), BASE, sources, producer_id)),
                        "offline_guard_sanitized")
            for result in (with_aborts, failed):
                for bad in (True, 1.5, "2", -1, 1000001):
                    require(rejects(lambda: receipt_policy(encode({**result, "request_counts": [
                        {"client_aborted": bad}]}), BASE, sources, producer_id)), "offline_abort_count_type")
                require(rejects(lambda: receipt_policy(encode({**result, "request_counts": [
                    {"unknown": 1}]}), BASE, sources, producer_id)), "offline_request_label")
                require(rejects(lambda: receipt_policy(encode({**result, "request_counts": [{}, {}, {}, {}]}),
                    BASE, sources, producer_id)), "offline_request_count_bound")
            # Abort counts cannot stand in for the positive traffic or any
            # negative TLS refusal; negative ports must still serve no HTTP/WSS.
            for index, label, amount in ((0, "http_get", 0), (0, "websocket", 0), (0, "tls_refused", 0),
                    (1, "tls_refused", 0), (2, "tls_refused", 0), (1, "http_get", 1), (2, "websocket", 1)):
                requests = [dict(request) for request in with_aborts["request_counts"]]
                requests[index][label] = amount
                require(rejects(lambda: receipt_policy(encode({**with_aborts, "request_counts": requests}),
                    BASE, sources, producer_id)), "offline_abort_not_a_control")
            return True
        def safe_failure_receipts():
            # Partial progress matching the three retained hosted failure shapes,
            # with the current request-counter/guard protocol additions. This
            # checks validity, not an attribution of the hosted guard's cause.
            early_fields = {"schemaVersion", "candidate_sha", "candidate_tree", "source_hashes", "producer",
                "scope", "result", "version", "cleanup", "archive_hashes", "tools", "source_proof",
                "phases", "cases", "owned_cleanup", "request_counts", "captured_output_bytes",
                "dependency_payload_bytes_peak", "launchctl_query_grant_id", "launchctl_query_pinned_parent",
                "github_run_id", "github_run_attempt"}
            early = {key: val for key, val in valid_receipt.items() if key in early_fields}
            early.update(result="fail", failure="unexpected_exception", cases=[], request_counts=[],
                phases=[dict(valid_receipt["phases"][i]) for i in (0, 1, 4)],
                owned_cleanup={**valid_receipt["owned_cleanup"], "children": 11, "listeners": 0})
            early["phases"][1].update(ok=False, exit_code=1)
            require(receipt_policy(encode(early), BASE, sources, producer_id) is False, "offline_early_failure")
            detail = {"type": "FileNotFoundError", "errno": 2, "function": "read_bounded", "line": 141,
                      "command": None, "exit_code": None}
            require(receipt_policy(encode({**early, "failure_detail": detail}), BASE, sources, producer_id) is False,
                    "offline_detailed_early_failure")
            later = {key: val for key, val in valid_receipt.items() if key not in {"trust_controls", "fixture_operations"}}
            later.update(result="fail", failure="owned_operation_allowlist", failure_guard="proxy", cases=[],
                service_guard={**valid_receipt["service_guard"], "starts": 0, "observations": []},
                phases=[dict(phase) for phase in valid_receipt["phases"]], request_counts=[{}, {}, {}])
            later["phases"][3].update(ok=False, exit_code=1)
            require(receipt_policy(encode(later), BASE, sources, producer_id) is False, "offline_late_guard_failure")
            uncertain = {**early, "failure": "cleanup_uncertain", "cleanup": False,
                "owned_cleanup": {**early["owned_cleanup"], "surviving_children": None, "surviving_listeners": None,
                    "unobserved_children": 11, "closure_errors": ["cleanup_deadline", "cleanup_incomplete"],
                    "uncertain": True, "root_deleted": False}}
            require(receipt_policy(encode(uncertain), BASE, sources, producer_id) is False, "offline_uncertain_failure")
            no_sources = {**early, "source_hashes": {}}
            require(receipt_policy(encode(no_sources), BASE, {}, producer_id) is False, "offline_pre_source_failure")
            return True

        def sensitive_failures():
            path_value = "/private/tmp/credential"
            token_value = "g8Qv3mZ6rT2pN9xL4cW7sK5uH0dF1jB8aE6yR3iM9oP2nS4tV7zX5"
            failed = {**valid_receipt, "result": "fail", "failure": "unexpected_exception",
                "failure_detail": {"type": "Refusal", "errno": None, "function": "check", "line": 1,
                                   "command": None, "exit_code": None}}
            def string_paths(item, prefix=()):
                if type(item) is dict:
                    for key, nested in item.items():
                        yield from string_paths(nested, (*prefix, key))
                elif type(item) is list:
                    for index, nested in enumerate(item):
                        yield from string_paths(nested, (*prefix, index))
                elif type(item) is str:
                    yield prefix
            # Mutate every string leaf, not just tools.curl or a known URL.
            for path in string_paths(failed):
                for replacement in (path_value, "C:\\private\\credential", "../private/credential", token_value, token_value.lower()):
                    value = strict_json(encode(failed), "receipt")
                    selected = value
                    for part in path[:-1]:
                        selected = selected[part]
                    selected[path[-1]] = replacement
                    try:
                        receipt_policy(encode(value), BASE, sources, producer_id)
                    except Refusal as error:
                        require(str(error) in {"receipt_schema", "receipt_sensitive", "receipt_identity"}
                                and replacement not in str(error), "offline_receipt_no_echo")
                    else:
                        return False
            for field in valid_receipt:
                require(receipt_reject((field,), "opaque_invalid_field", failed=True), "offline_failed_field_type")
            for field in ("command", "function", "type"):
                for bad in (path_value, token_value):
                    detail = {**failed["failure_detail"], field: bad}
                    require(rejects(lambda: receipt_policy(encode({**failed, "failure_detail": detail}),
                            BASE, sources, producer_id)), "offline_failed_diagnostic_field")
            for bad in (path_value, token_value):
                require(rejects(lambda: receipt_policy(encode({**failed, "failure_guard": bad}),
                        BASE, sources, producer_id)), "offline_failed_guard_field")
            require(safe_failure_code(Refusal(token_value)) == "fixture_refused"
                    and command_label([path_value, token_value]) == "unknown", "offline_safe_diagnostics")
            custom_error = type(token_value, (RuntimeError,), {})(path_value)
            require(failure_detail(custom_error)["type"] == "Unknown", "offline_safe_exception")
            return safe_failure_receipts()

        def publication_cases():
            # Same writer and publication function as runtime/workflow, with
            # memory-only files and clocks. No filesystem allocation or timer.
            now = [0.0]
            clock = lambda: now[0]
            class ReceiptFile:
                def __init__(self, data=None, delay_at=None):
                    self.data, self.delay_at = data, delay_at
                def lstat(self):
                    if self.data is None:
                        raise FileNotFoundError("inert absent staging")
                    return SimpleNamespace(st_mode=stat.S_IFREG, st_nlink=1, st_size=len(self.data),
                                           st_dev=1, st_ino=2, st_mtime_ns=3)
                def open(self, mode):
                    if mode == "rb":
                        return io.BytesIO(self.data)
                    require(mode == "xb" and self.data is None, "offline_receipt_exclusive")
                    owner = self
                    class Writer(io.BytesIO):
                        def write(self, data):
                            count = super().write(data)
                            if owner.delay_at == "write":
                                now[0] = 30
                            return count
                        def close(self):
                            owner.data = self.getvalue()
                            super().close()
                            if owner.delay_at == "close":
                                now[0] = 30
                    return Writer()
            def publish(staged, status):
                output = ReceiptFile()
                qualified = publish_receipt(staged, output, status, BASE, 1, sources, producer_id,
                                            CleanupDeadline(seconds=5, clock=clock))
                observed = strict_json(output.data, "receipt")
                require(receipt_policy(output.data, BASE, observed["source_hashes"], producer_id) is qualified,
                        "offline_publication_validated")
                require(len(output.data) <= LIMITS["receipt"], "offline_publication_bound")
                return qualified, observed, output.data
            # The original failure: all PASS bytes were written before the
            # post-write deadline check refused. They remain PRIVATE and are
            # never uploaded/published as PASS after the failed command exits.
            for delay in ("write", "close"):
                now[0] = 0
                staged = ReceiptFile(delay_at=delay)
                require(rejects(lambda: write_receipt(staged, encode(valid_receipt), CleanupDeadline(clock=clock))),
                        "offline_slow_staging_refused")
                require(strict_json(staged.data, "receipt")["result"] == "pass", "offline_stale_pass_reproduced")
                now[0] = 0  # Separate post-exit publisher cap, not a renewed runtime budget.
                qualified, observed, _ = publish(staged, 1)
                require(not qualified and observed["result"] == "fail" and observed["failure"] == "command_failed"
                        and "archive_hashes" not in observed, "offline_slow_write_no_published_pass")
            now[0] = 0
            staged = ReceiptFile()
            write_receipt(staged, encode(valid_receipt), CleanupDeadline(clock=clock))
            qualified, observed, _ = publish(staged, 0)
            require(qualified and observed == valid_receipt, "offline_publication_positive")
            qualified, observed, _ = publish(staged, 17)
            require(not qualified and observed["result"] == "fail" and observed["failure_detail"]["exit_code"] == 17
                    and "cases" not in observed and staged.data == encode(valid_receipt), "offline_failed_exit_stale_pass")
            failed = {**valid_receipt, "result": "fail", "failure": "owned_operation_allowlist", "failure_guard": "proxy"}
            qualified, observed, _ = publish(ReceiptFile(encode(failed)), 1)
            require(not qualified and observed == failed, "offline_sanitized_failure_preserved")
            qualified, observed, _ = publish(ReceiptFile(encode(failed)), 0)
            require(not qualified and observed == failed, "offline_zero_exit_failed_receipt_not_pass")
            early = {**failed, "source_hashes": {FILES[0]: sources[FILES[0]]}}
            qualified, observed, _ = publish(ReceiptFile(encode(early)), 1)
            require(not qualified and observed == early, "offline_partial_source_failure_preserved")
            for bad in ({**valid_receipt, "github_run_id": 2}, {**valid_receipt, "candidate_sha": "c" * 40},
                    {**valid_receipt, "source_hashes": {**sources, FILES[0]: "a" * 64}},
                    {**valid_receipt, "producer": {**producer_id, "artifact": 2}}):
                qualified, observed, _ = publish(ReceiptFile(encode(bad)), 0)
                require(not qualified and observed["result"] == "fail" and observed["failure"] == "receipt_validation",
                        "offline_publication_exact_binding")
            for bad in (None, b"{", b" " * (LIMITS["receipt"] + 1), encode({**failed, "secret": "opaque_marker"})):
                qualified, observed, data = publish(ReceiptFile(bad), 1)
                require(not qualified and observed["result"] == "fail" and b"opaque_marker" not in data,
                        "offline_invalid_staging_no_echo")
            for status in (True, -1, 256):
                output = ReceiptFile()
                require(rejects(lambda: publish_receipt(staged, output, status, BASE, 1, sources, producer_id,
                            CleanupDeadline(clock=clock))) and output.data is None, "offline_publication_exit_type")
            # Source-bind the exit/publication/upload wiring as well as testing
            # its functions. The runtime path is never the uploaded path.
            workflow = read_bounded(Path(__file__).resolve().parents[1] / FILES[0], 65536).decode()
            require('runner_status=0' in workflow and '|| runner_status=$?' in workflow
                    and '--runner-exit-code "$runner_status"' in workflow and 'exit "$runner_status"' in workflow
                    and '--publish-receipt' in workflow
                    and '--evidence "$RUNNER_TEMP/herdr-dpb-staging/darwin-package-browser.json"' in workflow
                    and 'path: ${{ runner.temp }}/herdr-dpb-evidence/darwin-package-browser.json' in workflow
                    and 'path: ${{ runner.temp }}/herdr-dpb-staging' not in workflow, "offline_publication_workflow")
            return True

        def receipt_cases():
            # New first-push identity fields are mandatory on PASS; every
            # present field is typed on FAIL as well (never an opaque sink).
            for field in ("launchctl_query_grant_id", "launchctl_query_pinned_parent", "github_run_id", "github_run_attempt"):
                missing = dict(valid_receipt)
                del missing[field]
                require(rejects(lambda: receipt_policy(encode(missing), BASE, sources, producer_id)), "offline_missing_grant_identity")
            for failed in (False, True):
                for field, invalid in (("launchctl_query_grant_id", ("q2-parent-launchctl-print-grant-20261008", None)),
                        ("launchctl_query_pinned_parent", ("0" * 40, LAUNCHCTL_PINNED_PARENT.upper(), "1" * 40)),
                        ("github_run_id", (True, 0, -1, 10**15, "1", 1.0)),
                        ("github_run_attempt", (True, 0, 2, "1", 1.0)),
                        ("candidate_sha", (BASE.upper(), BASE[:-1], "not_a_revision"))):
                    for bad in invalid:
                        require(receipt_reject((field,), bad, failed=failed), "offline_grant_identity_type")
            partial = {**valid_receipt, "result": "fail", "failure": "unexpected_exception"}
            for field in ("launchctl_query_grant_id", "launchctl_query_pinned_parent", "github_run_id", "github_run_attempt"):
                del partial[field]
            require(receipt_policy(encode(partial), BASE, sources, producer_id) is False, "offline_partial_grant_identity")
            return all(receipt_reject(path, replacement) for path, replacement in (
                (("launchctl_query_grant_id",), "unknown_grant"),
                (("cases",), valid_receipt["cases"][:-1]),
                (("cases", 0, "cases"), list(ENROLL[:-1])),
                (("cases", 0, "assertions", "reader_mutation_denied"), False),
                (("trust_controls", "browser", "observations"), []),
                (("trust_controls", "python_curl_packaged_go", 1, "packaged_go", "exit_code"), 0),
                (("trust_controls", "python_curl_packaged_go", 2, "python", "verify_code"), 20),
                (("service_guard", "observations", 0, "loaded_service"), True),
                (("service_guard", "observations", 1, "private_plist_after_absent"), False),
                (("owned_cleanup", "surviving_children"), 1),
                (("owned_cleanup", "surviving_listeners"), 1),
                (("owned_cleanup", "unobserved_children"), 1),
                (("owned_cleanup", "uncertain"), True),
                (("owned_cleanup", "closure_errors"), ["closure_failed"]),
                (("fixture_operations", "fake", 0, "count"), 2),
                (("fixture_operations", "socket", 1, "outcome"), "failed"),
                (("request_counts", 0, "websocket"), 0),
                (("captured_output_bytes",), LIMITS["raw"] + 1),
            ))
        operations = {
            "valid_result": lambda: browser_result(encode(valid), "enroll", 0)["ok"]
                and receipt_policy(encode(valid_receipt), BASE, sources, producer_id)
                and request_receipts()
                and receipt_policy(encode({**valid_receipt, "result": "fail", "failure": "cap_raw",
                    "captured_output_bytes": LIMITS["raw"] + 16384}), BASE, sources, producer_id) is False,
            "malformed_json": lambda: rejects(lambda: browser_result(b"{", "enroll", 0)),
            "oversize_json": lambda: rejects(lambda: strict_json(b" " * 65537)),
            "wrong_mode": lambda: rejects(lambda: browser_result(encode({**valid, "mode": "restart"}), "enroll", 0)),
            "wrong_platform": lambda: rejects(lambda: browser_result(encode({**valid, "fixture_platform": "linux"}), "enroll", 0)),
            "missing_case": lambda: rejects(lambda: browser_result(encode({**valid, "passed_cases": list(ENROLL[:-1])}), "enroll", 0))
                and receipt_cases(),
            "duplicate_case": lambda: rejects(lambda: browser_result(encode({**valid, "passed_cases": [*ENROLL, ENROLL[0]]}), "enroll", 0)),
            "extra_case": lambda: rejects(lambda: browser_result(encode({**valid, "passed_cases": [*ENROLL, "extra"]}), "enroll", 0)),
            "exit_disagreement": lambda: rejects(lambda: browser_result(encode(valid), "enroll", 1))
                and rejects(lambda: browser_result(encode({**valid, "stage": "controller_enrollment"}), "enroll", 0))
                and rejects(lambda: browser_result(encode({**valid, "app_worker_verified": False}), "enroll", 0))
                and rejects(lambda: browser_result(encode({**valid, "notification_delivery_disabled": False}), "enroll", 0))
                and receipt_reject(("phases", 3, "exit_code"), 1)
                and receipt_reject(("phases", 3, "seconds"), 630)
                and receipt_reject(("cases", 2, "exit_code"), 1)
                and publication_cases(),
            "invalid_producer": lambda: rejects(lambda: producer(BASE, 1, 1, "0" * 64, False, BASE, 1)),
            "invalid_budget_or_paths": lambda: rejects(lambda: paths_admission(Path("relative"), Path("receipt"), Path("/tmp"), 1260)),
            "sensitive_receipt_fields": lambda: rejects(lambda: browser_result(encode({**valid, "secret": "x"}), "enroll", 0))
                and receipt_reject(("tools", "secret"), "offline_marker")
                and receipt_reject(("tools", "curl"), "https://invalid/#setup=offline_marker")
                and sensitive_failures(),
        }
    elif group == "archive":
        valid_entries = [Entry(".", "directory", 0), Entry("web", "directory", 0),
                         Entry("web/index.html", "file", 3)]
        def entries(*items):
            return lambda: rejects(lambda: entry_policy(items, "tar"))
        payload = {path: "0" * 64 for path in ("herdr-mobile-relay", "web/index.html", "web/sw.js", "relay/common.sh",
                   "relay/tailscale.sh", "relay/tailscale-external.sh", "relay/setup-link.sh")}
        manifest = {"schema": 1, "version": "1.2.3", "revision": BASE, "target": "darwin/arm64", "files": payload}
        def valid_archive():
            require(entry_policy(valid_entries, "tar")["web/index.html"] == "file", "offline_archive")
            manifest_policy(manifest, "1.2.3", BASE, payload)
            checksum_policy(("0" * 64 + "  bundle.tar.gz\n").encode(), "bundle.tar.gz", "0" * 64)
            # Exercise the actual post-install capture/callback boundary using
            # in-memory paths and an injected verifier. Extraction and packaged
            # binary execution are separate recorded effects, never real ones.
            name = "herdr-mobile-relay_1.2.3_darwin_arm64.tar.gz"
            original = b"producer archive"
            original_checksums = (sha(original) + "  " + name + "\n").encode()
            hashes = {name: sha(original), "checksums.txt": sha(original_checksums)}
            def exercise(archive_bytes, checksum_bytes, tamper=None):
                files, directories, effects = {}, set(), []
                class MemoryPath:
                    def __init__(self, path):
                        self.path = Path(path)
                    @property
                    def parent(self):
                        return MemoryPath(self.path.parent)
                    @property
                    def name(self):
                        return self.path.name
                    def __truediv__(self, child):
                        return MemoryPath(self.path / child)
                    def mkdir(self, mode):
                        require(mode == 0o700 and self.path not in directories, "offline_capture_private_fresh")
                        directories.add(self.path)
                    def lstat(self):
                        data = files[self.path]
                        return SimpleNamespace(st_mode=stat.S_IFREG, st_nlink=1, st_size=len(data),
                                               st_dev=1, st_ino=2, st_mtime_ns=3)
                    def open(self, mode):
                        if mode == "rb":
                            return io.BytesIO(files[self.path])
                        require(mode == "xb" and self.path not in files, "offline_capture_exclusive")
                        owner = self
                        class Writer(io.BytesIO):
                            def close(self):
                                files[owner.path] = self.getvalue()
                                super().close()
                        return Writer()
                root = MemoryPath("/inert-package")
                archive, checksums = root / name, root / "checksums.txt"
                files[archive.path], files[checksums.path] = archive_bytes, checksum_bytes
                def verifier(captured_archive, captured_checksums, version, revision, release,
                             mark_stage, record_digest, record_wrapper, **options):
                    require(captured_archive.parent.path == captured_checksums.parent.path ==
                            Path("/inert-package/producer-verified-inputs")
                            and captured_archive.parent.path in directories
                            and captured_archive.name == name and version == "1.2.3" and revision == BASE
                            and options == {"target": "darwin/arm64", "verifier_timeout": 10}, "offline_captured_inputs")
                    effects.append("verifier")
                    if tamper == "archive":
                        files[captured_archive.path] = b"changed private copy"
                    elif tamper == "checksums":
                        files[captured_checksums.path] = b"changed private checksums"
                    mark_stage("archive_checksum")
                    actual = sha(read_bounded(captured_archive, LIMITS["tar"]))
                    record_digest(actual)  # Same pre-extraction interface as the shared helper.
                    checksum_policy(read_bounded(captured_checksums, 65536), name, actual)
                    mark_stage("archive_extract")
                    effects.append("extract")
                    effects.append("binary_execution")
                    return actual, release / "herdr-mobile-relay"
                refused = False
                try:
                    actual, _ = verify_producer_archive(archive, checksums, hashes, "1.2.3", BASE,
                        root / "release", Budget(lambda: 0.0), verifier)
                    require(actual == hashes[name], "offline_verified_digest")
                except Refusal:
                    refused = True
                return refused, effects, directories
            refused, effects, directories = exercise(original, original_checksums)
            require(not refused and effects == ["verifier", "extract", "binary_execution"]
                    and directories == {Path("/inert-package/producer-verified-inputs")}, "offline_binding_positive")
            changed = b"replacement archive"
            changed_checksums = (sha(changed) + "  " + name + "\n").encode()
            # Archive only, checksum only, and BOTH consistently replaced by an
            # install: all refuse before even calling the extraction verifier.
            for archive_bytes, checksum_bytes in ((changed, original_checksums),
                    (original, original_checksums + b"\n"), (changed, changed_checksums)):
                refused, effects, directories = exercise(archive_bytes, checksum_bytes)
                require(refused and not effects and not directories, "offline_replacement_before_extraction_or_binary")
            for tamper in ("archive", "checksums"):
                refused, effects, _ = exercise(original, original_checksums, tamper)
                require(refused and effects == ["verifier"], "offline_callback_before_extraction_or_binary")
            return True
        def zip_types():
            def item(name, kind):
                value = zipfile.ZipInfo(name)
                value.external_attr = kind << 16
                return value
            require(zip_entry(item("web/", stat.S_IFDIR)).kind == "directory", "offline_zip_directory")
            require(zip_entry(item("web/", 0)).kind == "directory", "offline_zip_unspecified")
            require(zip_entry(item("index.html", stat.S_IFREG)).kind == "file", "offline_zip_file")
            return all(rejects(lambda kind=kind: zip_entry(item("bad/", kind)))
                       for kind in (stat.S_IFCHR, stat.S_IFBLK, stat.S_IFIFO, stat.S_IFSOCK,
                                    stat.S_IFLNK, stat.S_IFREG)) and rejects(
                       lambda: zip_entry(item("bad", stat.S_IFDIR)))
        operations = {
            "valid_archive": valid_archive,
            "traversal": entries(Entry("../escape", "file", 1)),
            "absolute": entries(Entry("/escape", "file", 1)),
            "duplicate": entries(Entry("same", "file", 1), Entry("./same", "file", 1)),
            "ambiguous": entries(Entry("web", "file", 1), Entry("web/index.html", "file", 1)),
            "symlink": entries(Entry("link", "symlink", 0)),
            "hardlink": entries(Entry("link", "hardlink", 0)),
            "special_member": lambda: entries(Entry("fifo", "fifo", 0))() and zip_types(),
            "zip_digest": lambda: rejects(lambda: digest_policy(sha(b"zip"), "0" * 64, "zip_digest")),
            "tar_digest": lambda: rejects(lambda: digest_policy(sha(b"tar"), "0" * 64, "tar_digest")),
            "checksum": lambda: rejects(lambda: checksum_policy(b"bad  bundle.tar.gz", "bundle.tar.gz", "0" * 64)),
            "manifest_target_or_version": lambda: rejects(lambda: manifest_policy({**manifest, "target": "linux/amd64"}, "1.2.3", BASE, payload))
                and rejects(lambda: manifest_policy(manifest, "9.9.9", BASE, payload)),
            "zip_count": lambda: rejects(lambda: entry_policy((Entry(str(i), "file", 0) for i in range(129)), "zip")),
            "tar_count": lambda: rejects(lambda: entry_policy((Entry(str(i), "file", 0) for i in range(16385)), "tar")),
            "individual_bytes": entries(Entry("large", "file", LIMITS["individual"] + 1)),
            "aggregate_bytes": entries(*(Entry(str(i), "file", LIMITS["individual"]) for i in range(5))),
        }
    elif group == "phase-caps":
        now = [0.0]
        clock = lambda: now[0]
        def deadline(seconds, stopped=False):
            budget = Budget(clock)
            budget.enter(0)
            now[0] += seconds
            refused = rejects(budget.check)
            return refused and (not stopped or rejects(lambda: budget.enter(1)))
        def global_deadline():
            budget = Budget(clock)
            now[0] += 1260
            return rejects(budget.check)
        def within():
            budget = Budget(clock)
            for i in range(5):
                budget.enter(i)
                now[0] += 1
                budget.check()
            return budget.remaining() > 0
        def immutable():
            try:
                LIMITS["stdin"] = 999999
            except TypeError:
                return LIMITS["stdin"] == 65536 and PHASES == (90, 300, 210, 630, 30)
            return False
        def real_guards():
            class ClosedRecordingGate(EffectGate):
                def __init__(self):
                    super().__init__(False)
                    self.calls = []
                def effect(self, kind):
                    self.calls.append(kind)
                    super().effect(kind)
            valid_admission = {"system": "Darwin", "machine": "arm64", "enabled": "1",
                "repository": REPOSITORY, "ref": REF, "attempt": "1", "event_name": "push", "run_id": "1",
                "runner_environment": "github-hosted", "image_os": "macos15", "candidate_sha": "c" * 40,
                "event_before": LAUNCHCTL_PINNED_PARENT, "event_after": "c" * 40, "event_ref": REF,
                "event_forced": False, "event_created": False, "event_deleted": False,
                "checked_out_sha": "c" * 40, "candidate_parents": [LAUNCHCTL_PINNED_PARENT]}
            require(admission_policy(valid_admission), "offline_admission_positive")
            require(socket_operations_allowed({("ping", "succeeded"): 1})
                    and proxy_operations_allowed([SimpleNamespace(failure=False)])
                    and child_environment_allowed({"HOME": "/owned/home"})
                    and service_inputs_allowed(False, False, "/bin/launchctl")
                    and fixture_port_allowed(18443)
                    and public_request_allowed("/", "127.0.0.1:18443", "websocket", {"/"}, 18443)
                    and public_request_allowed("/healthz", "127.0.0.1:18443", "", {"/healthz"}, 18443),
                    "offline_owned_guard_positive")
            guards = [lambda field=field: admission_policy({**valid_admission, field: "unsafe"})
                      for field in valid_admission]
            guards.extend([lambda: socket_operations_allowed({("unsafe.mutation", "succeeded"): 1}),
                           lambda: socket_operations_allowed({("ping", "failed"): 1}),
                           lambda: proxy_operations_allowed([SimpleNamespace(failure=True)]),
                           lambda: child_environment_allowed({"GH_TOKEN": "inert-offline-marker"}),
                           lambda: child_environment_allowed({"NODE_OPTIONS": "inert-offline-marker"}),
                           lambda: service_inputs_allowed(True, False, "/bin/launchctl"),
                           lambda: service_inputs_allowed(False, True, "/bin/launchctl"),
                           lambda: service_inputs_allowed(False, False, "/unowned/launchctl"),
                           lambda: fixture_port_allowed(443),
                           lambda: public_request_allowed("/", "foreign.invalid", "websocket", {"/"}, 18443),
                           lambda: public_request_allowed("https://foreign.invalid/", "127.0.0.1:18443", "", {"/"}, 18443),
                           lambda: public_request_allowed("/private", "127.0.0.1:18443", "", {"/"}, 18443),
                           lambda: public_request_allowed("/?secret=value", "127.0.0.1:18443", "", {"/"}, 18443),
                           lambda: public_request_allowed("/healthz", "127.0.0.1:18443", "websocket", {"/healthz"}, 18443)])
            for guard in guards:
                gate = ClosedRecordingGate()
                mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), gate)
                mock.add_guard("proxy", guard)
                try:
                    mock.spawn(["must-not-execute"])
                except Refusal as error:
                    require(str(error) == "owned_operation_allowlist" and mock.failure_guard == "proxy",
                            "offline_real_guard_category")
                else:
                    return False
                require(not gate.calls and not mock.children, "offline_effect_began")
            for name in GUARDS:
                mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), ClosedRecordingGate())
                mock.add_guard(name, lambda: (_ for _ in ()).throw(ValueError("inert-private-error")))
                require(rejects(mock.check) and mock.failure_guard == name, "offline_raised_guard_identity")
                require(rejects(lambda: mock.check_guards(lambda: None)) and mock.failure_guard == name,
                        "offline_final_guard_identity")
            return True
        def launchctl_grant_cases():
            # Pure injected CI/event/git observations: no environment, event or
            # checkout probes, git subprocess, host query or launcher. Only public
            # policy source is read below. Exercise the actual bounded reader
            # using in-memory metadata/streams, not a permissive substitute.
            base = {"system": "Darwin", "machine": "arm64", "enabled": "1", "repository": REPOSITORY,
                "ref": REF, "attempt": "1", "event_name": "push", "candidate_sha": "c" * 40, "run_id": "1",
                "runner_environment": "github-hosted", "image_os": "macos15"}
            event = {"before": LAUNCHCTL_PINNED_PARENT, "after": base["candidate_sha"], "ref": REF,
                "forced": False, "created": False, "deleted": False}
            identity = {"checked_out_sha": base["candidate_sha"], "candidate_parents": [LAUNCHCTL_PINNED_PARENT]}
            reads, queries = [], []
            class EventFile:
                def __init__(self, data, *, mode=stat.S_IFREG, size=None, links=1):
                    self.data = data
                    self.info = SimpleNamespace(st_mode=mode, st_nlink=links,
                        st_size=len(data) if size is None else size, st_dev=1, st_ino=2, st_mtime_ns=3)
                def lstat(self):
                    return self.info
                def open(self, mode):
                    require(mode == "rb", "offline_event_read_mode")
                    return io.BytesIO(self.data)
            def observe(observation=base, data=None, commit=identity, path="/inert-push-event.json", **metadata):
                def read_event(selected, maximum):
                    require(str(selected) == "/inert-push-event.json" and maximum == PUSH_EVENT_CAP == 65536,
                            "offline_event_bound")
                    reads.append(True)
                    return read_bounded(EventFile(encode(event) if data is None else data, **metadata), maximum)
                def query_candidate(selected):
                    require(selected == Path("/inert-candidate"), "offline_candidate_query")
                    queries.append(True)
                    return commit
                return first_push_observation(observation, path, Path("/inert-candidate"),
                    read_event=read_event, query_candidate=query_candidate)
            context = observe()
            packet = {"grantId": LAUNCHCTL_GRANT_ID, "authorized": True,
                "pinnedParent": LAUNCHCTL_PINNED_PARENT, "scope": dict(LAUNCHCTL_SCOPE)}
            valid_packet = encode(packet).decode()
            require(launchctl_authorized(valid_packet, context) and len(reads) == len(queries) == 1,
                    "offline_launchctl_first_push_positive")
            # Bind the workflow's explicit packet to the same exact frozen values.
            workflow = read_bounded(Path(__file__).resolve().parents[1] / FILES[0], 65536).decode()
            workflow_packet = re.search(r"--launchctl-query-grant '([^']+)'", workflow)
            require(workflow_packet is not None and strict_json(workflow_packet[1].encode()) == packet,
                    "offline_workflow_grant_binding")
            for bad_base in ({**base, "attempt": "2"}, *({**base, "event_name": name}
                    for name in ("workflow_dispatch", "pull_request", "schedule")),
                    {**base, "candidate_sha": "0" * 40}, {**base, "candidate_sha": "C" * 40},
                    {**base, "candidate_sha": "c" * 39}, {**base, "run_id": "0"}, {**base, "run_id": "1" * 16}):
                before = (len(reads), len(queries))
                require(rejects(lambda: observe(observation=bad_base)) and before == (len(reads), len(queries)),
                        "offline_ci_refused_before_acquisition")
            missing_before = dict(event)
            del missing_before["before"]
            bad_events = [missing_before, *({**event, "before": before} for before in
                (None, 0, "0" * 40, LAUNCHCTL_PINNED_PARENT.upper(), LAUNCHCTL_PINNED_PARENT[:-1], "b" * 40)),
                {**event, "after": "d" * 40}, {**event, "after": "C" * 40}, {**event, "ref": "refs/heads/other"},
                *({**event, key: True} for key in ("forced", "created", "deleted")),
                *({**event, key: 0} for key in ("forced", "created", "deleted")), [], None]
            bad_data = [*(encode(item) for item in bad_events), b"{", b"not JSON", b"{\"before\": NaN}",
                encode(event).replace(b'"before":', b'"before": null, "before":')]
            for data in bad_data:
                before = len(queries)
                require(rejects(lambda: observe(data=data)) and len(queries) == before,
                        "offline_event_refused_before_git")
            for options in ({"path": None}, {"path": ""}, {"path": "relative"},
                    {"data": b" " * (PUSH_EVENT_CAP + 1)}, {"size": PUSH_EVENT_CAP + 1},
                    {"data": b" " * (PUSH_EVENT_CAP + 1), "size": 1},
                    {"mode": stat.S_IFLNK}, {"mode": stat.S_IFDIR}, {"links": 2}):
                require(rejects(lambda: observe(**options)), "offline_event_file_refused")
            # Ordinary second fast-forward: both before and sole parent are the
            # first candidate, not the frozen parent. No reusable grant.
            later_base = {**base, "candidate_sha": "d" * 40}
            later_event = {**event, "before": base["candidate_sha"], "after": later_base["candidate_sha"]}
            later_identity = {"checked_out_sha": later_base["candidate_sha"], "candidate_parents": [base["candidate_sha"]]}
            require(rejects(lambda: observe(observation=later_base, data=encode(later_event), commit=later_identity)),
                    "offline_second_push_refused")
            for commit in ({**identity, "candidate_parents": [LAUNCHCTL_PINNED_PARENT, "b" * 40]},
                    {**identity, "candidate_parents": []}, {**identity, "candidate_parents": ["b" * 40]},
                    {**identity, "candidate_parents": LAUNCHCTL_PINNED_PARENT},
                    {**identity, "checked_out_sha": "d" * 40}, {}):
                require(rejects(lambda: observe(commit=commit)), "offline_candidate_parent_refused")
            bad_packets = [None, False, "", "null", "{}", "{", encode({**packet, "authorized": False}).decode(),
                encode({**packet, "authorized": 1}).decode(), encode({**packet, "grantId": "unknown"}).decode(),
                encode({**packet, "grantId": "q2-parent-launchctl-print-grant-20261008"}).decode(),
                encode({**packet, "pinnedParent": "b" * 40}).decode(),
                encode({key: value for key, value in packet.items() if key != "pinnedParent"}).decode(),
                encode({**packet, "scope": False}).decode(), encode({**packet, "extra": True}).decode(),
                encode({**packet, "scope": {**packet["scope"], "maxQueries": 3}}).decode(),
                encode({**packet, "scope": {**packet["scope"], "maxQueries": 2.0}}).decode(),
                encode({**packet, "scope": {**packet["scope"], "command": "launchctl bootstrap"}}).decode(),
                valid_packet.replace('"authorized": true', '"authorized": true, "authorized": true')]
            contexts = [(valid_packet, {**context, field: "unsafe"}) for field in context]
            contexts.extend((valid_packet, {**context, "candidate_parents": parents})
                for parents in ([], ["b" * 40], [LAUNCHCTL_PINNED_PARENT, "b" * 40]))
            for raw, observation in [*((bad, context) for bad in bad_packets), *contexts]:
                gate = EffectGate(True, launchctl_grant=raw, launchctl_context=observation)
                mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), gate)
                launches, guard = [], {"starts": 0}
                mock.capture = lambda *args, **kwargs: launches.append(True)
                require(rejects(lambda: mock.capture_launcher(["must-not-execute"], guard,
                            cwd=None, link_origin=ORIGIN)) and not launches and not mock.children
                        and guard["starts"] == gate.launchctl_queries == 0, "offline_launchctl_refused_before_launcher")
                require(rejects(lambda: gate.effect("launchctl")) and gate.launchctl_queries == 0,
                        "offline_launchctl_effect_refused")
            require(rejects(lambda: EffectGate(True).effect("launchctl")), "offline_default_gate_not_launchctl_authority")
            # Stale admission cannot authorize capture OR a direct query effect.
            stale_gate = EffectGate(True, launchctl_grant=valid_packet, launchctl_context=context,
                launchctl_observe=lambda: observe(data=encode(later_event)))
            stale_mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), stale_gate)
            stale_launches, stale_guard = [], {"starts": 0}
            stale_mock.capture = lambda *args, **kwargs: stale_launches.append(True)
            require(rejects(lambda: stale_mock.capture_launcher(["must-not-execute"], stale_guard, cwd=None, link_origin=ORIGIN))
                and rejects(lambda: stale_gate.effect("launchctl")) and not stale_launches
                and stale_guard["starts"] == stale_gate.launchctl_queries == 0, "offline_revalidation_before_effect")
            gate = EffectGate(True, launchctl_grant=valid_packet, launchctl_context=context, launchctl_observe=observe)
            mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), gate)
            launches, guard = [], {"starts": 0}
            mock.capture = lambda *args, **kwargs: launches.append(True)
            for _ in range(2):
                mock.capture_launcher(["inert-owned-launcher"], guard, cwd=None, link_origin=ORIGIN)
            require(len(launches) == guard["starts"] == gate.launchctl_queries == 2
                    and rejects(lambda: mock.capture_launcher(["must-not-execute"], guard, cwd=None, link_origin=ORIGIN))
                    and rejects(lambda: gate.effect("launchctl"))
                    and len(launches) == gate.launchctl_queries == 2, "offline_launchctl_two_queries_only")
            return True

        def launch_argument_cases():
            # Source-bound interpretation of BOTH actual declarative JS policies.
            # Bind the complete predicate to the finite interpreter below; never
            # pretend Python ran JS or spawn Node/a browser in an offline case.
            # These are NEW assertions in the existing case, not historical coverage.
            expected_function = r'''function launchArgumentsAllowed(argv, userDataDir) {
  if (!Array.isArray(argv) || argv.length < 2 || !argv.every((argument) => typeof argument === 'string')) return false;
  const args = argv.slice(1);
  const switches = [];
  const syntax = new RegExp(LAUNCH_ARGUMENT_POLICY.switchPattern);
  const forbidden = new RegExp(LAUNCH_ARGUMENT_POLICY.forbiddenPattern, 'i');
  for (const argument of args) {
    if (argument === 'about:blank') continue;
    const match = syntax.exec(argument);
    if (!match || match[0] !== argument) return false;
    const name = match[1].toLowerCase().replace(/_/g, '-');
    const value = match[2] ?? '';
    if (forbidden.test(name) || forbidden.test(value.replace(/_/g, '-'))
      || LAUNCH_ARGUMENT_POLICY.processIsolationNames.includes(name)) return false;
    switches.push({ name, value });
  }
  const profiles = switches.filter((entry) => entry.name === 'user-data-dir');
  return profiles.length === 1 && profiles[0].value === userDataDir
    && ['--use-mock-keychain', '--enable-automation', '--remote-debugging-pipe'].every((flag) => args.includes(flag))
    && args.some((argument) => argument === '--headless' || argument.startsWith('--headless='));
}'''
            # Full expected Playwright 1.62.1 macOS headless persistent argv from
            # the frozen 9393fa79... bundle facts, with chromiumSandbox:true and
            # our sole extra --enable-automation. Not a copied runtime allowlist.
            profile = "/owned-policy-profile"
            expected_argv = ["chromium", "--disable-field-trial-config", "--disable-background-networking",
                "--disable-background-timer-throttling", "--disable-backgrounding-occluded-windows",
                "--disable-back-forward-cache", "--disable-breakpad", "--disable-client-side-phishing-detection",
                "--disable-component-extensions-with-background-pages", "--disable-component-update",
                "--no-default-browser-check", "--disable-default-apps", "--disable-dev-shm-usage",
                "--disable-edgeupdater", "--disable-extensions",
                "--disable-features=AvoidUnnecessaryBeforeUnloadCheckSync,BoundaryEventDispatchTracksNodeRemoval,DestroyProfileOnBrowserClose,DialMediaRouteProvider,GlobalMediaControls,HttpsUpgrades,LensOverlay,MediaRouter,PaintHolding,ThirdPartyStoragePartitioning,BlockOriginHeaderModificationOnRedirect,Translate,AutoDeElevate,OptimizationHints,msForceBrowserSignIn,msEdgeUpdateLaunchServicesPreferredVersion",
                "--enable-features=CDPScreenshotNewSurface", "--allow-pre-commit-input", "--disable-hang-monitor",
                "--disable-ipc-flooding-protection", "--disable-popup-blocking", "--disable-prompt-on-repost",
                "--disable-renderer-backgrounding", "--disable-updater-scheduler", "--force-color-profile=srgb",
                "--metrics-recording-only", "--no-first-run", "--password-store=basic", "--use-mock-keychain",
                "--no-service-autorun", "--export-tagged-pdf", "--disable-search-engine-choice-screen",
                "--unsafely-disable-devtools-self-xss-warnings", "--edge-skip-compat-layer-relaunch",
                "--disable-infobars", "--disable-search-engine-choice-screen", "--disable-sync",
                "--enable-unsafe-swiftshader", "--headless", "--hide-scrollbars", "--mute-audio",
                "--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4",
                "--enable-automation", "--user-data-dir=" + profile, "--remote-debugging-pipe", "about:blank"]
            negative_flags = ("--disable-gpu-sandbox", "--disable-gpu-sandbox=1", "--DISABLE-GPU-SANDBOX",
                "--disable_gpu_sandbox", "--no-sandbox", "--disable-setuid-sandbox", "--no-zygote-sandbox",
                "--disable-renderer-sandbox", "--disable-features=GpuSandbox,NetworkServiceSandbox",
                "--disable-features=gPuSaNdBoX", "-disable-gpu-sandbox", "https://foreign.invalid",
                "--no-zygote", "--no_zygote=0", "--single-process", "--IN_PROCESS_GPU",
                "--proxy-server=http://invalid", "--proxy-pac-url=http://invalid", "--proxy-bypass-list=*",
                "--PROXY_BYPASS_LIST=*", "--ignore-certificate-errors", "--ignore-ssl-errors",
                "--allow-insecure-localhost", "--test-root-certificate=1", "--unsafely-treat-insecure-origin-as-secure=1",
                "--disable-web-security", "--allow-running-insecure-content", "--disable-site-isolation-trials",
                "--host_resolver_rules=MAP * 127.0.0.1", "--remote-debugging-port=0", "--load-extension=/inert",
                "--user_data_dir=/foreign", "--user-data-dir=" + profile, "--", "--bad name", "--flag=bad\nvalue")
            shared = read_bounded(Path(__file__).resolve().parent / "tailscale-package-browser.mjs", 1024 * 1024).decode()
            policies = []
            for text in (shared, TRUST_SCRIPT):
                declaration = re.search(r"const LAUNCH_ARGUMENT_POLICY = (\{[^\n]+\});\n", text)
                function = re.search(r"function launchArgumentsAllowed\(argv, userDataDir\) \{.*?\n\}", text, re.S)
                require(declaration is not None and function is not None and function[0] == expected_function,
                        "offline_launch_argument_policy_source")
                policy = strict_json(declaration[1].encode())
                policies.append(policy)
                syntax = re.compile(policy["switchPattern"], re.ASCII)
                forbidden = re.compile(policy["forbiddenPattern"], re.I | re.ASCII)
                def allowed(argv, user_data_dir):
                    if type(argv) is not list or len(argv) < 2 or not all(type(v) is str for v in argv):
                        return False
                    args, switches = argv[1:], []
                    for argument in args:
                        if argument == "about:blank":
                            continue
                        match = syntax.fullmatch(argument)
                        if match is None:
                            return False
                        name, value = match[1].lower().replace("_", "-"), match[2] or ""
                        if forbidden.search(name) or forbidden.search(value.replace("_", "-")) or name in policy["processIsolationNames"]:
                            return False
                        switches.append((name, value))
                    profiles = [value for name, value in switches if name == "user-data-dir"]
                    return (profiles == [user_data_dir]
                        and all(flag in args for flag in ("--use-mock-keychain", "--enable-automation", "--remote-debugging-pipe"))
                        and any(v == "--headless" or v.startswith("--headless=") for v in args))
                require(allowed(expected_argv, profile), "offline_full_expected_argv_positive")
                for flag in negative_flags:
                    require(not allowed([*expected_argv, flag], profile), "offline_launch_argument_refused")
                for required in ("--use-mock-keychain", "--enable-automation", "--remote-debugging-pipe",
                                 "--headless", "--user-data-dir=" + profile):
                    require(not allowed([v for v in expected_argv if v != required], profile), "offline_required_switch")
                require(not allowed(expected_argv, "/foreign") and not allowed([*expected_argv, 1], profile),
                        "offline_owned_profile_and_types")
            require(policies[0] == policies[1], "offline_same_argument_name_policy")
            return True

        def proxy_transport_cases():
            # Exercise real worker -> BaseRequestHandler construction -> stdlib
            # parsing -> our GET/abort/backend paths, using memory-only peers.
            # No socket, listener, backend dial, signal or subprocess is created.
            class Input(io.BytesIO):
                def __init__(self, data, error, fail_at):
                    super().__init__(data)
                    self.error, self.fail_at, self.calls = error, fail_at, 0
                def readline(self, maximum=-1):
                    self.calls += 1
                    if self.calls == self.fail_at:
                        raise self.error("inert client read")
                    return super().readline(maximum)
            class Output(io.BytesIO):
                def __init__(self, error, write_error, flush_at, close_error):
                    super().__init__()
                    self.error, self.write_error, self.flush_at, self.flushes = error, write_error, flush_at, 0
                    self.close_error = close_error
                def write(self, data):
                    if self.write_error:
                        raise self.error("inert client write")
                    return super().write(data)
                def flush(self):
                    self.flushes += 1
                    if self.flushes == self.flush_at:
                        raise self.error("inert client flush")
                    super().flush()
                def close(self):
                    super().close()
                    if self.close_error is not None:
                        error, self.close_error = self.close_error, None
                        raise error("inert client close")
            class Connection:
                def __init__(self, data, error, read_at, write_error, flush_at, handshake, close_error):
                    self.input = Input(data, error, read_at)
                    self.output = Output(error, write_error, flush_at, close_error)
                    self.error, self.handshake, self.closed = error, handshake, False
                def settimeout(self, seconds):
                    require(seconds == 5, "offline_proxy_timeout")
                def do_handshake(self):
                    if self.handshake:
                        raise self.error("inert handshake")
                def makefile(self, mode, _buffering):
                    return self.input if mode == "rb" else self.output
                def sendall(self, data):
                    self.output.write(data)
                def close(self):
                    self.closed = True
            class Backend:
                status = 200
                def __init__(self, error, fail_at):
                    self.error, self.fail_at, self.closed = error, fail_at, False
                def check(self, stage):
                    if stage == self.fail_at:
                        raise self.error("inert backend")
                def request(self, *_args, **_kwargs):
                    self.check("request")
                def getresponse(self):
                    self.check("response")
                    return self
                def read(self, _maximum):
                    self.check("read")
                    return b"fixture"
                def getheaders(self):
                    self.check("headers")
                    return [("Content-Type", "text/plain")]
                def sendall(self, _data):
                    self.check("send")
                def recv(self, _maximum):
                    self.check("recv")
                    return b"HTTP/1.1 101 Switching Protocols\r\n\r\n"
                def close(self):
                    self.check("close")
                    self.closed = True
            def exercise(data=b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:18443\r\n\r\n", *,
                         error=TimeoutError, read_at=0, write_error=False, flush_at=0,
                         handshake=False, backend_error="", buffered=True, closing=False, close_error=None):
                backend = Backend(error, backend_error)
                calls = []
                class Handler(ProxyHandler):
                    wbufsize = 1 if buffered else 0
                    def backend_call(self, operation, *args, **kwargs):
                        if operation in {http.client.HTTPConnection, socket.create_connection}:
                            calls.append(operation)
                            def construct():
                                backend.check("connect")
                                return backend
                            return super().backend_call(construct)
                        return super().backend_call(operation, *args, **kwargs)
                server = object.__new__(Proxy)
                server.lock, server.counts, server.failure, server.closing = threading.RLock(), {}, False, closing
                server.server_port, server.server_name, server.paths = 18443, "inert", {"/healthz", "/", "/ws"}
                server.RequestHandlerClass = Handler
                connection = Connection(data, error, read_at, write_error, flush_at, handshake, close_error)
                server.connections = {connection}
                server.worker(connection, ("127.0.0.1", 1))
                require(connection.closed and not server.connections, "offline_proxy_peer_closed")
                require(proxy_request_counts([server]) == [server.counts], "offline_proxy_snapshot")
                return server, calls
            errors = (ssl.SSLEOFError, ssl.SSLZeroReturnError, ssl.SSLError, ConnectionResetError,
                      BrokenPipeError, ConnectionAbortedError, TimeoutError)
            for error in errors:
                for options in ({"read_at": 1}, {"read_at": 2}, {"write_error": True},
                                {"flush_at": 1}, {"flush_at": 2}):
                    server, _ = exercise(error=error, **options)
                    require(proxy_operations_allowed([server]) and server.counts.get("client_aborted") == 1
                            and server.counts.get("tls_accepted") == 1 and not server.counts.get("tls_refused"),
                            "offline_benign_client_abort")
                server, calls = exercise(error=error, handshake=True)
                require(proxy_operations_allowed([server]) and server.counts == {"tls_refused": 1} and not calls,
                        "offline_handshake_refusal")
                # Exactly the Python TLS-only positive control: no HTTP request.
                server, _ = exercise(b"", error=error, flush_at=1)
                require(proxy_operations_allowed([server]) and server.counts == {"tls_accepted": 1, "client_aborted": 1},
                        "offline_handler_finish_abort")
                for options in ({"read_at": 1}, {"write_error": True}):
                    server, _ = exercise(error=error, buffered=False, **options)
                    require(proxy_operations_allowed([server]) and server.counts.get("client_aborted") == 1,
                            "offline_unbuffered_client_abort")
                for stage in ("connect", "request", "response", "read", "headers", "close"):
                    for closing in (False, True):
                        server, _ = exercise(error=error, backend_error=stage, closing=closing)
                        require(not proxy_operations_allowed([server]), "offline_backend_error_fatal")
            server, _ = exercise(b"")
            require(proxy_operations_allowed([server]) and server.counts == {"tls_accepted": 1}, "offline_tls_only_eof")
            websocket = b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nUpgrade: websocket\r\n\r\n"
            server, _ = exercise(websocket, error=ssl.SSLEOFError, write_error=True)
            require(proxy_operations_allowed([server]) and server.counts.get("websocket") == 1
                    and server.counts.get("client_aborted") == 1, "offline_websocket_client_abort")
            for stage in ("send", "recv"):
                server, _ = exercise(websocket, error=ConnectionResetError, backend_error=stage)
                require(not proxy_operations_allowed([server]), "offline_websocket_backend_error_fatal")
            rejected = [f"{method} / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\n\r\n".encode()
                        for method in ("HEAD", "POST", "PUT", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PATCH")]
            rejected.extend((b"GET /private HTTP/1.1\r\nHost: 127.0.0.1:18443\r\n\r\n",
                b"GET /?secret=value HTTP/1.1\r\nHost: 127.0.0.1:18443\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: foreign.invalid\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nUpgrade: h2c\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nContent-Length: 1\r\n\r\nx",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nContent-Length:\r\n\r\nx",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
                b"GET / HTTP/invalid\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nHost: foreign.invalid\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nHost: 127.0.0.1:18443\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nHost: foreign.invalid\r\nUpgrade: websocket\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nUpgrade: websocket\r\nUpgrade: h2c\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nUpgrade: websocket\r\nUpgrade: websocket\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:18443\r\nUpgrade:\r\nUpgrade: h2c\r\n\r\n"))
            for request in rejected:
                for closing in (False, True):
                    server, calls = exercise(request, error=BrokenPipeError, flush_at=1, closing=closing)
                    require(not proxy_operations_allowed([server]) and not calls
                            and not server.counts.get("http_get") and not server.counts.get("websocket"),
                            "offline_policy_failure_latched")
                mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), EffectGate(False))
                mock.add_guard("proxy", lambda: proxy_operations_allowed([server]))
                require(rejects(lambda: mock.spawn(["must-not-execute"])) and mock.failure_guard == "proxy"
                        and not mock.children, "offline_policy_stops_effects")
            for error in (RuntimeError, ValueError, OSError):
                for closing in (False, True):
                    server, _ = exercise(error=error, read_at=1, closing=closing)
                    require(not proxy_operations_allowed([server]) and not server.counts.get("client_aborted"),
                            "offline_internal_error_fatal")
            for error in (RuntimeError, TimeoutError, ConnectionResetError):
                server = object.__new__(Proxy)
                server.failure = False
                connection = Connection(b"", error, 0, False, 0, False, None)
                server.get_request = lambda: (connection, ("127.0.0.1", 1))
                def dispatch_failure(*_args):
                    raise error("inert dispatch error")
                server.process_request = dispatch_failure
                server.shutdown_request = lambda request: request.close()
                server._handle_request_noblock()  # Real stdlib dispatch/error hook.
                require(server.failure is True and connection.closed, "offline_dispatch_error_fatal")
            server, _ = exercise(error=RuntimeError, flush_at=2, close_error=BrokenPipeError)
            require(not proxy_operations_allowed([server]) and server.counts.get("client_aborted") == 1,
                    "offline_finish_internal_error_not_masked")
            server, _ = exercise(error=lambda _: OSError(errno.EBADF, "inert owned closure"), read_at=1, closing=True)
            require(proxy_operations_allowed([server]) and not server.counts.get("client_aborted"), "offline_owned_close_race")
            require(owned_socket_closed(OSError(errno.EBADF, "inert owned closure"), True)
                    and not owned_socket_closed(OSError(errno.EBADF, "inert live error"), False)
                    and not owned_socket_closed(ValueError("inert internal error"), True), "offline_owned_closure_scope")
            return True
        def dependency_storage():
            directory = Path("/inert-installer-payload")
            checks = []
            def walk(root, *, followlinks, onerror):
                require(root == directory and followlinks is False, "offline_storage_walk")
                onerror(FileNotFoundError("inert disappeared directory"))
                yield str(root), [], ["retained", "deleted"]
            def metadata(path):
                if path.name == "deleted":
                    raise FileNotFoundError("inert disappeared staging file")
                return SimpleNamespace(st_mode=stat.S_IFREG, st_size=7)
            require(payload_storage_bytes([directory], lambda: checks.append(True),
                    walk=walk, metadata=metadata) == 7 and bool(checks), "offline_storage_live_payload")
            require(rejects(lambda: payload_storage_bytes([directory], lambda: None, walk=walk,
                    metadata=lambda _: SimpleNamespace(st_mode=stat.S_IFREG, st_size=LIMITS["dependencies"] + 1))),
                    "offline_storage_stream_cap")
            def unreadable(root, *, followlinks, onerror):
                onerror(PermissionError("inert unreadable payload directory"))
                return iter(())
            try:
                payload_storage_bytes([directory], lambda: None, walk=unreadable, metadata=metadata)
            except PermissionError:
                return rejects(lambda: cap("dependencies", LIMITS["dependencies"] + 1))
            return False
        def certificate_configuration():
            # Exercise the production generator without files or an OpenSSL
            # process, including the command-to-configuration bindings.
            written, commands = {}, []
            class InertPath:
                def __init__(self, name="/inert-certificates"):
                    self.name = name
                def __truediv__(self, child):
                    return InertPath(self.name + "/" + child)
                def __str__(self):
                    return self.name
                def write_text(self, text):
                    written[self.name.rsplit("/", 1)[-1]] = text
            class InertOperations:
                returncode = 0
                def capture(self, argv, timeout):
                    require(argv[0] == "openssl" and timeout == 10, "offline_certificate_command")
                    commands.append(argv)
                    return SimpleNamespace(returncode=self.returncode)
            root, adapter = InertPath(), InertOperations()
            certificates(root, adapter)
            require(set(written) == {"ca.cnf", "unknown-ca.cnf", "valid.ext", "unknown.ext", "wrong-host.ext"}
                    and len(commands) == 9, "offline_certificate_outputs")
            def option(argv, flag):
                return argv[argv.index(flag) + 1]
            subjects = []
            for index, name in enumerate(("ca", "unknown-ca")):
                text = written[name + ".cnf"]
                require("x509_extensions=ca\n" in text and "[ca]\n" in text
                        and "basicConstraints=critical,CA:true\n" in text
                        and "keyUsage=critical,keyCertSign,cRLSign\n" in text
                        and "subjectKeyIdentifier=hash\n" in text, "offline_ca_extensions")
                subjects.append(next(line for line in text.splitlines() if line.startswith("CN=")))
                require(option(commands[index], "-config") == str(root / (name + ".cnf"))
                        and option(commands[index], "-out") == str(root / (name + ".pem")), "offline_ca_binding")
            require(subjects[0] != subjects[1], "offline_distinct_ca_subjects")
            for index, (name, ca, san) in enumerate((("valid", "ca", "IP:127.0.0.1"),
                    ("wrong-host", "ca", "DNS:wrong.invalid"), ("unknown", "unknown-ca", "IP:127.0.0.1"))):
                text = written[name + ".ext"]
                require("basicConstraints=critical,CA:false\n" in text
                        and "keyUsage=critical,digitalSignature,keyEncipherment\n" in text
                        and "subjectKeyIdentifier=hash\n" in text
                        and "authorityKeyIdentifier=keyid:always\n" in text
                        and "extendedKeyUsage=serverAuth\n" in text
                        and "subjectAltName=" + san + "\n" in text, "offline_leaf_extensions")
                signing = commands[3 + index * 2]
                require(option(signing, "-extfile") == str(root / (name + ".ext"))
                        and option(signing, "-CA") == str(root / (ca + ".pem"))
                        and option(signing, "-CAkey") == str(root / (ca + ".key"))
                        and option(signing, "-out") == str(root / (name + ".pem")), "offline_leaf_binding")
            adapter.returncode = 1
            return rejects(lambda: certificates(root, adapter))
        def forbidden_effects():
            require(real_guards(), "offline_real_guards")
            require(launchctl_grant_cases(), "offline_launchctl_grants")
            require(launch_argument_cases(), "offline_launch_argument_policy")
            require(proxy_transport_cases(), "offline_proxy_transport")
            require(certificate_configuration(), "offline_certificate_configuration")
            gate = EffectGate(False)
            mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), gate)
            blocked = all(rejects(lambda name=name: gate.effect(name)) for name in EffectGate.KINDS)
            return (blocked and rejects(lambda: mock.spawn(["must-not-execute"]))
                    and rejects(lambda: hydrate(None, None, None, None, gate))
                    and rejects(lambda: profiles(None, b"", gate))
                    and rejects(lambda: Proxy(0, None, None, mock))
                    and rejects(lambda: Child(SimpleNamespace(returncode=None, pid=0), gate).send("INT"))
                    and not mock.children)
        operations = {"within_limits": within, "per_phase_deadline": lambda: deadline(90),
            "global_deadline": global_deadline, "stop_before_next_phase": lambda: deadline(90, True),
            "stdin_cap": lambda: rejects(lambda: cap("stdin", 65537)),
            "result_cap": lambda: rejects(lambda: cap("result", 65537)),
            "raw_output_cap": lambda: rejects(lambda: cap("raw", 2097153)),
            "receipt_cap": lambda: rejects(lambda: cap("receipt", 262145)),
            "dependency_storage_cap": dependency_storage,
            "hydration_stream_cap": lambda: rejects(lambda: CountReader(io.BytesIO(b"ab"), "zip").read(-1))
                and rejects(lambda: cap("zip", 67108865))
                and rejects(lambda: streaming_body(io.BytesIO(b"ab"), 1, Budget(clock)))
                and streaming_body(io.BytesIO(b"a"), 1, Budget(clock)) == b"a",
            "immutable_production_limits": immutable,
            "injected_operations_refuse_real_effects": forbidden_effects}
    else:
        def cleanup_case(survive=False, listener_survives=False, error=False, handled=False, escalation=False):
            owner, child, listener = Cleanup(), InertChild(survive, escalation), InertListener(listener_survives, error)
            owner.child(child)
            owner.listener(listener)
            if survive or listener_survives or error:
                try:
                    owner.close()
                except CleanupFailure as failure:
                    evidence = failure.evidence
                    return (evidence is owner.evidence and evidence["uncertain"] is True
                            and evidence["children"] == 1 and evidence["listeners"] == 1
                            and evidence["surviving_children"] == int(survive)
                            and evidence["surviving_listeners"] == int(listener_survives or error)
                            and evidence["observed_surviving_children"] == int(survive)
                            and evidence["observed_surviving_listeners"] == int(listener_survives or error)
                            and evidence["unobserved_children"] == evidence["unobserved_listeners"] == 0
                            and bool(evidence["closure_errors"]) and evidence["root_deleted"] is False)
                return False
            if handled:
                try:
                    raise Refusal("handled_signal")
                except Refusal:
                    evidence = owner.close()
            else:
                evidence = owner.close()
            require(evidence is owner.evidence and evidence["uncertain"] is False
                    and evidence["closure_errors"] == [] and evidence["surviving_children"] == 0
                    and evidence["surviving_listeners"] == 0
                    and evidence["observed_surviving_children"] == evidence["observed_surviving_listeners"] == 0
                    and evidence["unobserved_children"] == evidence["unobserved_listeners"] == 0,
                    "offline_cleanup_evidence")
            return listener.closed() and child.done() and child.events == (
                ["INT", "wait3", "TERM", "wait3", "KILL", "wait2", "reap2"] if escalation else ["INT", "wait3", "reap2"])
        def aggregate_and_deletion_deadlines():
            now = [0.0]
            clock = lambda: now[0]
            class SlowChild(InertChild):
                def wait_bounded(self, seconds):
                    now[0] += seconds
                def reap_bounded(self, seconds):
                    now[0] += seconds
                    return self.finished
            owner = Cleanup()
            for _ in range(7):
                owner.child(SlowChild(survives=True))
            require(rejects(lambda: owner.close(CleanupDeadline(clock=clock))), "offline_aggregate_cleanup")
            require(owner.evidence["uncertain"] is True and "cleanup_deadline" in owner.evidence["closure_errors"]
                    and owner.evidence["surviving_children"] is None
                    and owner.evidence["unobserved_children"] > 0
                    and owner.evidence["observed_surviving_children"] > 0
                    and owner.evidence["root_deleted"] is False, "offline_aggregate_evidence")
            # The same production root identity/deletion wrapper uses an inert
            # filesystem adapter. No directories, timer or signals are created.
            class Root:
                present = True
                def lstat(self):
                    return SimpleNamespace(st_mode=stat.S_IFDIR, st_dev=1, st_ino=1)
                def resolve(self, strict=True):
                    return self
                def exists(self):
                    return self.present
                def is_symlink(self):
                    return False
            root = Root()
            identity = root.lstat()
            def remove(value):
                value.present = False
                now[0] += 31
            now[0] = 0
            deadline = CleanupDeadline(clock=clock)
            empty_owner = Cleanup()
            evidence = empty_owner.close(deadline)
            require(rejects(lambda: remove_owned_tree(root, identity, deadline, remove)),
                    "offline_deletion_cleanup")
            empty_owner.error("cleanup_deadline")
            require(evidence["root_deleted"] is False and evidence["uncertain"] is True
                    and evidence["surviving_children"] == 0 and evidence["surviving_listeners"] == 0,
                    "offline_deletion_evidence")
            now[0] = 0
            reserved = CleanupDeadline(clock=clock, reserve=1)
            now[0] = 29
            require(rejects(reserved.check), "offline_receipt_reserve_work_refusal")
            reserved.reserve = 0
            reserved.check()  # Same end, only the reserved serialization window.
            now[0] = 30
            require(rejects(reserved.check), "offline_receipt_deadline_refusal")
            return True
        operations = {"ordered_success": cleanup_case, "failure_cleanup": lambda: cleanup_case(handled=True),
            "handled_signal_cleanup": lambda: cleanup_case(handled=True),
            "bounded_escalation_and_reap": lambda: cleanup_case(escalation=True) and aggregate_and_deletion_deadlines(),
            "unowned_ids_refused": lambda: rejects(lambda: Cleanup().stop(InertChild())),
            "surviving_children_fail": lambda: cleanup_case(survive=True),
            "surviving_listener_fail": lambda: cleanup_case(listener_survives=True),
            "closure_error_fails": lambda: cleanup_case(error=True),
            "symlink_root_refused": lambda: rejects(lambda: root_policy(False, True)),
            "existing_root_refused": lambda: rejects(lambda: root_policy(True, False))}
    require(set(operations) == set(OFFLINE[group]), "offline_definition")
    for case_id in OFFLINE[group]:
        evaluated(case_id, operations[case_id])
    ok = [case["id"] for case in cases] == list(OFFLINE[group]) and all(case["ok"] is True for case in cases)
    result = {"schemaVersion": 1, "group": group, "ok": ok, "cases": cases}
    data = (json.dumps(result, separators=(",", ":")) + "\n").encode()
    require(len(data) <= 16384, "offline_output")
    sys.stdout.buffer.write(data)
    return 0 if ok else 1


class InertChild:
    def __init__(self, survives=False, escalation=False):
        self.survives, self.escalation, self.finished, self.events = survives, escalation, False, []

    def done(self):
        return self.finished

    def send(self, name):
        self.events.append(name)
        if not self.survives and (not self.escalation or name == "KILL"):
            self.finished = True

    def wait_bounded(self, seconds):
        self.events.append("wait" + str(seconds))

    def reap_bounded(self, seconds):
        self.events.append("reap" + str(seconds))
        return self.finished

    def group_gone(self):
        return self.finished


class InertListener:
    def __init__(self, survives=False, error=False):
        self.survives, self.error, self.finished = survives, error, False

    def close(self, deadline):
        deadline.check()
        if self.error:
            raise Refusal("mock_close_error")
        self.finished = not self.survives

    def closed(self):
        return self.finished


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--offline-self-check", choices=tuple(OFFLINE))
    parser.add_argument("--producer-sha")
    parser.add_argument("--producer-run", type=int)
    parser.add_argument("--artifact-id", type=int)
    parser.add_argument("--artifact-zip-sha256")
    parser.add_argument("--fresh-producer", action="store_true")
    parser.add_argument("--launchctl-query-grant")
    parser.add_argument("--private-root", type=Path)
    parser.add_argument("--evidence", type=Path, help="private staging for runtime; published path for --publish-receipt")
    parser.add_argument("--publish-receipt", action="store_true")
    parser.add_argument("--staged-receipt", type=Path)
    parser.add_argument("--runner-exit-code", type=int)
    parser.add_argument("--budget-seconds", type=int, default=1260)
    args = parser.parse_args(argv)
    if args.offline_self_check:
        require(len(sys.argv[1:] if argv is None else argv) == 2, "offline_arguments")
        return offline_check(args.offline_self_check)
    if args.publish_receipt:
        require(args.staged_receipt is not None and args.evidence is not None
                and args.runner_exit_code is not None, "publication_paths_missing")
        return publication(args)
    require(args.staged_receipt is None and args.runner_exit_code is None, "runtime_arguments")
    require(args.private_root is not None and args.evidence is not None, "runtime_paths_missing")
    return runtime(args)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Refusal:
        # No exception text or argparse echo of secret inputs is uploaded.
        print('{"schemaVersion":1,"ok":false,"failure":"admission_refused"}', file=sys.stderr)
        raise SystemExit(1)

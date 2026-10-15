#!/usr/bin/env python3
"""One isolated Darwin/arm64 extracted-package BYO browser cell.

Imports are stdlib-only and inert. Offline checks never enter admission/runtime;
the authorized finalizer inspects the helper before its offline import.
This is not managed Serve, host trust, or a zero-egress sandbox.
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
import inspect
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
import tempfile
import threading
import time
import traceback
from types import MappingProxyType, SimpleNamespace
import urllib.parse
import urllib.request
import zipfile

BASE = "cbacea6efe8916f2a040e6ca4927e221e9b6e15c"
# BASE is history (the earlier single-commit candidate) and is also a sample SHA
# in many offline fixtures; it is no longer the scope or grant base. SCOPE_BASE is
# the CI-branch tip that the parked WIP, the service repairs and the CI wiring
# descend from. The candidate change set is measured from it, and the launchctl
# grant's push admission is anchored to it (see ALLOWED, REQUIRED and the grant).
SCOPE_BASE = "c77855cb4e779c00a85d194f5bffe72ac3016554"
REF = "refs/heads/ci/tailscale-native-preflight-ef9f843"
# The exact Python the cell and its admission steps run under, pinned in the workflow
# with actions/setup-python (the runner image's ambient python3 is mutable). The
# receipt and the qualify gate require the same exact value, like Node, Bun and Go.
PYTHON_VERSION = "3.14.0"
REPOSITORY = "0cv/herdr-mobile-relay"
PLATFORM = "darwin-isolated-byo-v1"
# The Darwin fixture serves the phone app AND the relay WSS dial at this one
# origin. The production development-link printer (printDevelopmentSetupLink)
# refuses any relay origin whose port is not tailscalecli.DevelopmentHTTPSPort
# (8443), and the frozen browser runner refuses any WebSocket destination other
# than this origin's WSS twin, so the single fixture origin must use 8443. Only
# the two negative TLS controls keep 18444/18445.
ORIGIN = "https://127.0.0.1:8443"
FILES = (
    ".github/workflows/tailscale-darwin-package-browser.yml",
    "tests/test_tailscale_darwin_package_browser.py",
    "tests/test_tailscale_extracted_package_browser.py",
    "tests/tailscale-package-browser.mjs",
)
# The four receipt-hashed FILES plus the standalone qualification gate. FILES
# (receipt source_hashes, helper/MJS indexes) deliberately stays the four runner
# files; the gate script is not receipt-hashed.
QUALIFY_GATE = "scripts/qualify-darwin-package-browser.py"
CANDIDATE_PATHS = (*FILES, QUALIFY_GATE)
# Candidate change-set scope, measured by `git diff --name-only SCOPE_BASE HEAD`.
# A candidate is in scope when it contains every REQUIRED gate file (so a candidate
# that lacks this gate is refused) and touches nothing outside ALLOWED. ALLOWED is
# a frozen literal of explicit repo-relative paths (no globs, no directory
# prefixes), exact at the reviewed snapshot: it equals the diff from SCOPE_BASE to
# the candidate that introduced it, so it carries no speculative entries. A later
# commit that touches a path outside ALLOWED must extend ALLOWED in the same
# commit; that friction is intended. The workflow's producer step calls
# candidate_scope_allowed from this module, so there is exactly one allowlist.
REQUIRED = (FILES[0], FILES[1], QUALIFY_GATE)
ALLOWED = (
    # Parked always-on/doctor/menu WIP (commit 88a6714) and its tests and docs.
    # These paths are in the candidate only because the WIP is parked on this branch;
    # they are NOT reviewed until M3 and listing them here is not a review.
    "README.md",
    "cmd/herdr-mobile-relay/doctor.go",
    "cmd/herdr-mobile-relay/doctor_test.go",
    "cmd/herdr-mobile-relay/main.go",
    "cmd/herdr-mobile-relay/main_test.go",
    "docs/collie-tailscale-comparison.md",
    "docs/collie-tailscale-delivery-plan.md",
    "docs/plans/tailscale-macos/NEXT-RUN.md",
    "docs/plans/tailscale-macos/PLAN.md",
    "docs/plans/tailscale-macos/R1-OWNERSHIP-DESIGN.md",
    "docs/plans/tailscale-macos/REMAINING-IMPLEMENTATION-PLAN.md",
    "docs/plans/tailscale-macos/REVIEW.md",
    "docs/plans/tailscale-macos/execution/CI-REVIEW.md",
    "docs/plans/tailscale-macos/execution/R1-DESIGN-REVIEW.md",
    "docs/plans/tailscale-macos/execution/R1-SOURCE-EVIDENCE.json",
    "docs/plans/tailscale-macos/execution/STATUS.json",
    "docs/plans/tailscale-macos/execution/WORKER-LOG.md",
    "docs/plans/tailscale-macos/imported/ACCEPTANCE-MATRIX.md",
    "docs/plans/tailscale-macos/imported/HANDOFF.md",
    "docs/plans/tailscale-macos/imported/MACOS-RUNBOOK.md",
    "docs/plans/tailscale-macos/imported/PLAN-VALIDATION.md",
    "docs/plans/tailscale-macos/imported/PLAN.md",
    "docs/plans/tailscale-macos/imported/SHA256SUMS",
    "docs/plans/tailscale-macos/imported/SLICE-TEMPLATE.md",
    "docs/plans/tailscale-macos/imported/START-HERE.md",
    "docs/plans/tailscale-macos/imported/VERIFICATION.md",
    "docs/plans/tailscale-macos/imported/handoff/UNAPPROVED-portable-bool.patch",
    "docs/plans/tailscale-macos/imported/handoff/plan-checks.json",
    "docs/plans/tailscale-macos/imported/handoff/provenance.json",
    "docs/plans/tailscale-macos/imported/handoff/source-inventory.json",
    "docs/plans/tailscale-macos/imported/status.json",
    "docs/plans/tailscale-macos/imported/verify-source.py",
    "docs/tailscale-cli-background-plan.md",
    "internal/app/server.go",
    "internal/app/server_test.go",
    "internal/doctor/bundle.go",
    "internal/doctor/bundle_test.go",
    "internal/doctor/config.go",
    "internal/doctor/config_test.go",
    "internal/doctor/doctor.go",
    "internal/doctor/doctor_test.go",
    "internal/doctor/herdr.go",
    "internal/doctor/herdr_peer_darwin.go",
    "internal/doctor/herdr_peer_linux.go",
    "internal/doctor/herdr_test.go",
    "internal/doctor/local.go",
    "internal/doctor/local_test.go",
    "internal/doctor/package_identity.go",
    "internal/doctor/package_identity_test.go",
    "internal/doctor/probes.go",
    "internal/doctor/probes_test.go",
    "internal/readiness/readiness.go",
    "internal/readiness/readiness_test.go",
    "internal/support/support.go",
    "internal/support/support_test.go",
    "internal/tailscale/DR-1-session-authority.md",
    "relay/plugin-setup-menu.sh",
    "relay/plugin-status.sh",
    "scripts/package-release.sh",
    "tests/test_plugin_doctor.py",
    "tests/test_setup_menu.py",
    "tests/test_tailscale_consent.py",
    # The 21 reviewed service/installer repairs (A1-F001..A1-F009, revision 23 plus the
    # reviewed follow-up fix to relay/native-install-transaction.sh and its regression).
    "relay/common.sh",
    "relay/herdr-mobile-relay-service.sh",
    "relay/install-service.sh",
    "relay/install-systemd-user-service.sh",
    "relay/native-install-transaction.sh",
    "relay/plugin-install-service.sh",
    "relay/service.sh",
    "relay/tailscale-cli-service.sh",
    "relay/uninstall-service.sh",
    "relay/uninstall-systemd-user-service.sh",
    "relay/uninstall.sh",
    "scripts/check-installed-release.sh",
    "tests/test_common.sh",
    "tests/test_native_install.sh",
    "tests/test_release_scripts.sh",
    "tests/test_service_credentials.sh",
    "tests/test_service_supervision.py",
    "tests/test_service_transport_dispatch.py",
    "tests/test_service_uninstall_cli.py",
    "tests/test_tailscale_cli_setup_failure.py",
    "tests/test_uninstall.sh",
    # T001 CI wiring: the service-qualification job and the check.yml edits.
    ".github/workflows/check.yml",
    ".github/workflows/service-qualification.yml",
    "scripts/ci/run-service-qualification.sh",
    # Hosted-job timeout caps (T004 review F002): the tag-triggered release workflow.
    # Added only for that finding; check.yml above already covers the branch jobs.
    ".github/workflows/release.yml",
    # This gate: the Darwin workflow, its runner and the qualification gate script.
    ".github/workflows/tailscale-darwin-package-browser.yml",
    "tests/test_tailscale_darwin_package_browser.py",
    "scripts/qualify-darwin-package-browser.py",
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
PROBE_CAP = 64  # Deliberate finite refusal bound, not an observed refresh count.
COUNT_MAX = 65535
SOCKET_ORDINARY = frozenset({"ping", "agent.list", "pane.list", "workspace.list", "tab.list",
    "session.snapshot", "events.subscribe", "pane.read"})
SOCKET_PROBES = frozenset({"workspace.move_block", "tab.move", "pane.read"})
SOCKET_PAIRS = frozenset(
    {(method, outcome) for method in SOCKET_ORDINARY - {"pane.read"} for outcome in ("succeeded", "failed")}
    | {("pane.read", outcome) for outcome in ("succeeded", "refused_expected", "rejected", "failed")}
    | {(method, outcome) for method in SOCKET_PROBES - {"pane.read"}
       for outcome in ("refused_expected", "rejected", "failed")}
    | {("other", "rejected"), ("other", "failed")})
SOCKET_REASONS = frozenset({"ordinary", "inert_probe", "probe_shape", "invalid_params", "unsupported_method",
    "malformed_request", "non_object_request", "invalid_envelope", "invalid_request_id", "oversize_request",
    "incomplete_request", "recv_failed", "send_failed", "internal_error"})
SOCKET_EXCEPTION_CATEGORIES = frozenset({"timeout", "connection_reset", "broken_pipe", "os_error", "internal"})
SOCKET_EXCEPTION_BUCKETS = ("handler_exceptions", "stream_exceptions", "listener_exceptions", "publication_exceptions")
SOCKET_GUARDS = frozenset({"allowed", "unexpected_pair", "probe_cap_exceeded", "counter_saturated",
    "recorded_exception", "listener_not_alive", "not_evaluated"})
# Exact enumerated READ-ONLY Herdr CLI queries the packaged relay can issue,
# derived from internal/herdr/client.go (+ call sites): (argv length, literal
# flag positions). Operand positions must be non-empty and not option-like.
# Every mutating command (workspace/tab create|rename, pane run|send-*|close,
# agent start|rename, worktree create|open|remove, ...) is deliberately absent.
# "--version" is a single argument: fake-herdr exits before recording it.
FAKE_READ_ONLY = MappingProxyType({
    "agent list": (2, {}), "pane list": (2, {}), "workspace list": (2, {}), "tab list": (2, {}),
    "integration status": (2, {}),                                   # internal/profiles/resolver.go
    "agent get": (3, {}),                                            # internal/coordinator/lifecycle.go
    "pane process-info": (4, {2: "--pane"}),                         # internal/panesize/manager.go, pi_commands.go
    "worktree list": (4, {2: "--workspace"}),                        # internal/coordinator/workspace.go
    "pane read": (9, {3: "--lines", 5: "--source", 7: "--format"}),  # socket-failure fallback only
})
# The single harmless controller command; the only mutation ever accepted.
FAKE_PROMPT = "agent prompt"
FAKE_PROMPT_ARGV = ("agent", "prompt", "workspace:agent", "package acceptance harmless ping")
FAKE_COMMANDS = frozenset(FAKE_READ_ONLY) | {FAKE_PROMPT}
FAKE_OUTCOMES = frozenset({"started", "succeeded", "failed"})
# A refusal diagnostic may additionally bucket anything else as "other".
FAKE_DIAGNOSTIC_COMMANDS = FAKE_COMMANDS | {"other"}
FAKE_DIAGNOSTIC_OUTCOMES = FAKE_OUTCOMES | {"other"}
FAKE_ROW_CAP = len(FAKE_DIAGNOSTIC_COMMANDS) * len(FAKE_DIAGNOSTIC_OUTCOMES)
# Each fake-herdr row is ~0.4 KiB (it records the socket path); the observed Linux run
# issued 14. This bounds the live evidence file with a wide margin, never the allowlist.
FAKE_OPERATIONS_CAP = 262144
# Admission projection of the roadmap's launchctl grant (q3, 20261014). The
# pinned scope base is a frozen literal (SCOPE_BASE), NEVER selected from CI/git
# observations. The spent q2 grants (2230f96f: q2-owner-launchctl-print-push-
# 2230f96f-20261013; dd608845, 8c1f22ff, 3c8f0c8d and e59e8022 before it) and the
# earlier authorized:false contract remain historical and are refused by this
# predicate. The packet key `pinnedParent` and the receipt key
# `launchctl_query_pinned_parent` keep their names so the receipt schema and the
# qualify gate's PASS_FIELDS are unchanged; their value is now the grant's pinned
# scope base, not a single parent. The grant covers any attempt-1, non-forced,
# linear fast-forward push to the CI branch whose `before` is SCOPE_BASE or a
# descendant of it (`before` is an ancestor of the candidate, the candidate has
# exactly one parent, and NO commit in the pushed range before..candidate is a
# merge), because the CI loop pushes more commits after the first one. A rerun,
# workflow_dispatch, a created or deleted ref, a force push, a merge commit
# anywhere in the pushed range, before == after, a before that is not an ancestor
# of the candidate and a push after a non-descendant reset all refuse. Operational
# admission only, NOT cryptographic hosted-runner authentication: a local caller
# can forge environment/event inputs. The grant authorizes at most two
# label-scoped read-only queries ONLY on the isolated hosted runner. No local
# runtime or local launchctl is authorized, even with fabricated inputs. This
# projection is NOT owner authority: no effect can occur before the owner
# approves G1, because a push is the only trigger.
LAUNCHCTL_GRANT_ID = "q3-roadmap-launchctl-print-ff-c77855cb-20261014"
LAUNCHCTL_PINNED_PARENT = SCOPE_BASE
PUSH_EVENT_CAP = 65536
LAUNCHCTL_SCOPE = MappingProxyType({
    "command": "launchctl print gui/$(id -u)/com.herdr-mobile-relay.service",
    "mode": "read-only query; output discarded", "maxQueries": 2,
    "where": "isolated GitHub-hosted macos-15 Darwin runner only",
    "when": "attempt 1 of a fast-forward push to " + REF + " whose before descends from " + LAUNCHCTL_PINNED_PARENT,
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


def candidate_scope_allowed(changed):
    # REQUIRED <= changed <= ALLOWED, no duplicates: a missing gate file, a path
    # outside the frozen allowlist, a repeat or a non-list container all refuse.
    return (type(changed) is list and all(type(path) is str for path in changed)
            and len(changed) == len(set(changed))
            and set(REQUIRED) <= set(changed) <= set(ALLOWED))


def cap(kind, value):
    require(type(value) is int and 0 <= value <= LIMITS[kind], "cap_" + kind)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def failure_detail(error, failed_command=None):
    """Secret-free locator for a failed cell.

    Records only the exception class, errno, and the innermost function/line in
    this file, the innermost function/line (and, for GateFailure, the fixed
    code) in the imported extracted helper, plus the failing tool label and
    exit code. Never messages, output, paths or URLs.
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
              "command": None, "exit_code": None, **helper_locator(error)}
    if failed_command:
        label, exit_code = failed_command
        detail["command"] = label if label in SAFE_COMMANDS else "unknown"
        detail["exit_code"] = exit_code if type(exit_code) is int else None
    return detail


def helper_locator(error):
    """Innermost frame in the imported extracted helper, excluding its die() raiser.

    The helper's GateFailure codes are coarse (most die() sites share
    "fixture_assertion"), so the helper line identifies the failing check. The
    helper is recognised by its repository-relative path (it is imported from
    the candidate checkout under another module name). Function names and codes
    come from fixed sets; anything else is "unknown"/None. Never a message.
    """
    suffix = "/" + FILES[2]
    frames = [frame for frame in traceback.extract_tb(error.__traceback__)
              if os.path.realpath(frame.filename).replace(os.sep, "/").endswith(suffix)
              and frame.name != "die"]
    last = frames[-1] if frames else None
    code = getattr(error, "code", None) if type(error).__name__ == "GateFailure" else None
    return {"helper_function": (last.name if last.name in SAFE_HELPER_FUNCTIONS else "unknown") if last else None,
            "helper_line": last.lineno if last and type(last.lineno) is int else None,
            "helper_code": code if type(code) is str and code in SAFE_HELPER_CODES else None}


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
    "tls_controls", "packaged_go_tls_control", "private_link", "fake_operation_summary", "browser_result", "receipt_policy",
    "receipt_values_safe", "verify", "remove_owned_tree", "stop", "close", "group_gone", "reap_bounded",
    "read", "__init__", "socket_allowed", "fake_allowed", "start_launcher", "require", "cap",
    "verify_producer_archive", "record_digest", "write_receipt", "publish_receipt", "publication"})
# Helper (tests/test_tailscale_extracted_package_browser.py) frames reachable from
# the Darwin runtime and its GateFailure codes. "die" is deliberately absent: the
# locator names the die() caller. The offline parser group pins that each name
# exists in the helper.
SAFE_HELPER_FUNCTIONS = frozenset({"unknown", "development_setup_link", "safe_link_from_output", "verify_archive",
    "write_json", "sha256", "wait_ready", "close", "operation_summary", "diagnostic_summary", "socket_scenario_view",
    "socket_ordinary_result", "socket_predispatch_refusal", "dispatch_socket_request", "socket_exception_category",
    "socket_diagnostic_snapshot", "handle_socket_connection", "__init__", "_serve", "_handle", "_result",
    "_publish_operations"})
SAFE_HELPER_CODES = frozenset({"fixture_assertion", "unexpected_exception", "archive_checksum_io",
    "archive_checksum_mismatch", "archive_extract_failure", "archive_unsafe_entry", "archive_binary_missing",
    "archive_manifest_missing", "archive_manifest_invalid", "archive_manifest_identity", "archive_binary_start",
    "archive_binary_timeout", "archive_binary_rejected", "archive_managed_wrapper_missing",
    "archive_external_wrapper_missing", "archive_managed_wrapper_not_executable",
    "archive_external_wrapper_not_executable", "archive_binary_not_executable"})
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
    methods = enum(*SOCKET_ORDINARY, "workspace.move_block", "tab.move", "other")
    socket_row = {"method": methods, "outcome": enum("succeeded", "refused_expected", "rejected", "failed"),
                  "count": integer(COUNT_MAX, 1)}
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
        "request_counts": ({key: count for key in ("tls_accepted", "tls_refused", "http_get", "websocket",
                                                   "websocket_refused", "client_aborted")}, 3),
        # FAIL receipts only: the secret-free first-cause locator of each latched proxy.
        "proxy_failure": ({"proxy": integer(2), "reason": enum(*PROXY_FAILURE_REASONS),
            "path": enum(*PROXY_PATH_CATEGORIES), "query": enum(*PROXY_QUERY_CATEGORIES),
            "method": enum(*PROXY_METHOD_CATEGORIES)}, 3),
        "fixture_operations": {
            "fake": ({"command": enum(*FAKE_DIAGNOSTIC_COMMANDS), "outcome": enum(*FAKE_DIAGNOSTIC_OUTCOMES),
                "count": count}, FAKE_ROW_CAP),
            "socket": (socket_row, 26)},
        "socket_diagnostics": {"schema": integer(1, 1), "rows": (socket_row, 26),
            "reasons": lambda item: socket_counter_map_safe(item, SOCKET_REASONS),
            **{key: lambda item: socket_counter_map_safe(item, SOCKET_EXCEPTION_CATEGORIES)
               for key in SOCKET_EXCEPTION_BUCKETS},
            "saturated": boolean, "liveness_before_cleanup": enum("serving", "stopped", "early_death", "not_started"),
            "closed_after_cleanup": enum("closed", "open", "not_observed"), "guard": enum(*SOCKET_GUARDS)},
        "captured_output_bytes": integer(LIMITS["raw"] + 131072),
        "dependency_payload_bytes_peak": integer(LIMITS["dependencies"]),
        "failure": enum(*SAFE_FAILURES), "failure_guard": enum(*GUARDS),
        "failure_detail": {"type": enum(*SAFE_EXCEPTION_TYPES), "errno": nullable(exit_code),
            "function": nullable(enum(*SAFE_FUNCTIONS)), "line": nullable(integer(1000000, 1)),
            "command": nullable(enum(*SAFE_COMMANDS)), "exit_code": nullable(exit_code),
            "helper_function": nullable(enum(*SAFE_HELPER_FUNCTIONS)),
            "helper_line": nullable(integer(1000000, 1)),
            "helper_code": nullable(enum(*SAFE_HELPER_CODES))},
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
    if "fixture_operations" in value:
        require("socket_diagnostics" in value, "receipt_schema")
        require(fake_rows_policy(value["fixture_operations"]["fake"], complete=False), "receipt_schema")
        socket_row_counts(value["fixture_operations"]["socket"])
    if "socket_diagnostics" in value:
        socket_diagnostics_policy(value["socket_diagnostics"])


def receipt_policy(data, candidate, source_hashes, producer_identity):
    """Validate the uploaded protocol itself, not just runtime progress flags."""
    value = strict_json(data, "receipt")
    fields = {"schemaVersion", "candidate_sha", "candidate_tree", "source_hashes", "result", "scope",
              "producer", "archive_hashes", "version", "manifest_sha256", "binary_sha256", "compiler",
              "browser_identity", "tools", "source_proof", "profile_inputs", "profile_policy", "service_guard",
              "trust_controls", "cases", "phases", "cleanup", "owned_cleanup", "request_counts",
              "fixture_operations", "captured_output_bytes", "dependency_payload_bytes_peak", "failure",
              "failure_detail", "failure_guard", "launchctl_query_grant_id",
              "github_run_id", "github_run_attempt", "launchctl_query_pinned_parent", "socket_diagnostics",
              "proxy_failure"}
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
            "tls_accepted", "tls_refused", "http_get", "websocket", "websocket_refused", "client_aborted"},
            "receipt_request_fields")
        for amount in request.values():
            count(amount)
    if value["result"] == "fail":
        require(type(value.get("failure")) is str and re.fullmatch(r"[a-z0-9_]{1,80}", value["failure"]), "receipt_failure")
        if "failure_guard" in value or value["failure"] == "owned_operation_allowlist":
            require(type(value.get("failure_guard")) is str and value["failure_guard"] in GUARDS,
                    "receipt_failure_guard")
        detail = value.get("failure_detail")
        if detail is not None:
            shape(detail, ("type", "errno", "function", "line", "command", "exit_code",
                           "helper_function", "helper_line", "helper_code"))
            require(type(detail["type"]) is str and re.fullmatch(r"[A-Za-z0-9_]{1,64}", detail["type"])
                    and all(detail[key] is None or type(detail[key]) is int
                            for key in ("errno", "line", "exit_code", "helper_line"))
                    and all(detail[key] is None or (type(detail[key]) is str
                            and re.fullmatch(r"[A-Za-z0-9_]{1,64}", detail[key]))
                            for key in ("helper_function", "helper_code"))
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
    require(set(value) == fields - {"failure", "failure_detail", "failure_guard", "proxy_failure"}
            and set(source_hashes) == set(FILES)
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
    require(value["tools"]["python"] == PYTHON_VERSION and value["tools"]["node"] == "24.21.0"
            and value["tools"]["bun"] == "1.4.0"
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
            and all(request.get("http_get", 0) == request.get("websocket", 0) == request.get("websocket_refused", 0) == 0
                    for request in requests[1:]), "receipt_request_controls")
    operations = value["fixture_operations"]
    shape(operations, ("fake", "socket"))
    require(type(operations["fake"]) is list and type(operations["socket"]) is list, "receipt_operations")
    require(fake_rows_policy(operations["fake"], complete=True), "receipt_fake_operation")
    require(("agent prompt", "succeeded") in {(row["command"], row["outcome"]) for row in operations["fake"]},
            "receipt_fake_mutation")
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
    socket_diagnostics_policy(value["socket_diagnostics"], passing=True, operation_rows=operations["socket"])
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
    """One monotonic cap shared by observations, closures, deletion and receipt writing."""
    def __init__(self, seconds=30, clock=time.monotonic, latest=None, reserve=0, started=None):
        self.clock, self.reserve = clock, reserve
        self.end = (clock() if started is None else started) + seconds
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


def remove_owned_tree(root, identity, deadline, remove=shutil.rmtree, sleep=time.sleep):
    deadline.check()
    info = root.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_ino == identity.st_ino
            and info.st_dev == identity.st_dev and root.resolve(strict=True) == root,
            "cleanup_root_identity")
    # A detached Chromium helper that outlives its aborted driver can still be
    # writing under a profile for a moment (its debugging pipe has just closed),
    # which makes one rmtree pass fail with ENOTEMPTY. Retry a few times inside
    # the same shared deadline; the last OSError is never swallowed.
    for attempt in range(REMOVE_ATTEMPTS):
        deadline.check()
        try:
            remove(root)  # Runtime alarm covers this entire operation, including C/IO waits.
            break
        except FileNotFoundError:
            break
        except OSError:
            if attempt == REMOVE_ATTEMPTS - 1:
                raise
            sleep(deadline.remaining(0.25))
    deadline.check()
    require(not root.exists() and not root.is_symlink(), "cleanup_root_survived")


REMOVE_ATTEMPTS = 4


def close_owned_resources(cleanup, deadline):
    """Close every owned child/listener. True when the only recorded uncertainty is the
    unattested browser-context closure and the owned root may still be deleted; any other
    closure uncertainty re-raises CleanupFailure exactly as before."""
    try:
        cleanup.close(deadline)
    except CleanupFailure as closure:
        if not owned_root_removable(closure.evidence):
            raise
        return True
    return False


def owned_root_removable(evidence):
    """True only when every owned child/listener is observed gone and the sole
    recorded closure uncertainty is the unattested browser-context closure."""
    return (type(evidence) is dict and evidence.get("closure_errors") == ["browser_contexts_unobserved"]
            and all(type(evidence.get(key)) is int and evidence[key] == 0 for key in (
                "surviving_children", "surviving_listeners", "observed_surviving_children",
                "observed_surviving_listeners", "unobserved_children", "unobserved_listeners")))


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


def full_sha(value):
    # A lowercase 40-hex object ID that is not all zeros. Never option-like text.
    return type(value) is str and re.fullmatch(r"[0-9a-f]{40}", value) is not None and value != "0" * 40


def admission_policy(observation):
    # Same pure operational predicate at CI admission and each launcher/query
    # boundary. Rechecking forgeable inputs does not authenticate a hosted runner.
    # Fast-forward ancestry: an attempt-1, non-forced push whose `before` is a real
    # tip different from the candidate, that descends from SCOPE_BASE (equal counts)
    # and is an ancestor of the candidate, whose candidate has exactly one parent and
    # whose whole pushed range before..candidate contains no merge commit (linear
    # history, not only a linear tip). The three git facts must each be exactly True.
    return (ci_context_allowed(observation)
            and full_sha(observation.get("event_before"))
            and observation["event_before"] != observation.get("event_after")
            and observation.get("event_after") == observation["candidate_sha"]
            and observation.get("event_ref") == REF
            and all(observation.get(key) is False for key in ("event_forced", "event_created", "event_deleted"))
            and observation.get("checked_out_sha") == observation["candidate_sha"]
            and type(observation.get("candidate_parents")) is list
            and len(observation["candidate_parents"]) == 1
            and full_sha(observation["candidate_parents"][0])
            and observation.get("scope_base_precedes_before") is True
            and observation.get("before_precedes_candidate") is True
            and observation.get("pushed_range_linear") is True)


IDENTITY_KEYS = frozenset({"checked_out_sha", "candidate_parents", "scope_base_precedes_before",
                           "before_precedes_candidate", "pushed_range_linear"})
GIT_OUTPUT_CAP = 4096


def run_git_bounded(argv, *, cwd, env, stdout, stderr, timeout, check):
    # A subprocess.run-shaped runner for git_candidate_identity whose stdout collection is
    # bounded WHILE the child runs: at most GIT_OUTPUT_CAP + 1 bytes are ever held (a
    # `git log` parent list or a `rev-list` answer cannot grow memory without bound), and
    # the child's process group is killed and reaped as soon as it exceeds that cap or the
    # deadline. A result longer than the cap is returned as GIT_OUTPUT_CAP + 1 bytes so the
    # caller refuses it by length; a deadline raises TimeoutExpired after the kill and reap.
    # Only the one call shape git_candidate_identity uses is accepted.
    #
    # Race-free group ownership: before git starts, an ANCHOR process becomes the leader of
    # a fresh process group (same session; it does not hold git's stdout pipe), and git joins
    # that group. The anchor is never reaped until after the group has been signalled, so the
    # group id (the anchor's pid) cannot be recycled by another process while the signal is
    # sent, on EVERY path: a normal return, the cap, the deadline, a leader that already
    # exited, and a spawn failure. The group is killed on every exit, so a descendant that
    # inherited the stdout pipe or kept running must not outlive the query. The anchor's
    # 30 s sleep is only a leak backstop. process_group needs Python 3.11 (CI pins 3.14.0).
    require(stdout == subprocess.PIPE and stderr == subprocess.DEVNULL and check is False
            and type(timeout) is int and timeout > 0, "launchctl_authority")
    deadline = time.monotonic() + timeout
    anchor = None
    process = None
    data = bytearray()
    try:
        anchor = subprocess.Popen([sys.executable, "-I", "-B", "-c", "import time; time.sleep(30)"],
                                  stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                  stderr=subprocess.DEVNULL, close_fds=True, process_group=0)
        process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.DEVNULL, close_fds=True, process_group=anchor.pid)
        descriptor = process.stdout.fileno()
        os.set_blocking(descriptor, False)
        with selectors.DefaultSelector() as selector:
            selector.register(descriptor, selectors.EVENT_READ)
            while len(data) <= GIT_OUTPUT_CAP:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(argv, timeout)
                if not selector.select(min(remaining, 0.25)):
                    continue
                try:
                    chunk = os.read(descriptor, GIT_OUTPUT_CAP + 1 - len(data))
                except BlockingIOError:
                    continue
                if not chunk:
                    break
                data += chunk
        if len(data) <= GIT_OUTPUT_CAP:
            process.wait(timeout=max(deadline - time.monotonic(), 0.001))
    finally:
        # Signal the whole group while the anchor is still alive and unreaped, THEN reap git
        # and the anchor. Reaping git first is harmless (it is not the group leader).
        if anchor is not None:
            try:
                os.killpg(anchor.pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
        if process is not None:
            process.wait()
            process.stdout.close()
        if anchor is not None:
            anchor.wait()
    return subprocess.CompletedProcess(argv, process.returncode, bytes(data))


def git_candidate_identity(candidate, before, runner=run_git_bounded):
    # Read only the checked-out object's SHA/parents and three history facts, not
    # messages or files. Four read-only git queries with one hardened invocation:
    # no replacement objects, optional index locks or ambient git configuration.
    # `before` is the push event's pre-push tip; it must already be a lowercase
    # 40-hex SHA (never option-like text) before any argv is built. The runner is
    # injectable so the argv and the strict exit-code mapping can be tested offline; the
    # default runner bounds the output while collecting it. The length check in query()
    # below still applies to any injected runner.
    require(full_sha(before), "launchctl_authority")
    prefix = ["git", "--no-replace-objects", "--no-optional-locks",
              "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"]
    environment = {"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                   "GIT_OPTIONAL_LOCKS": "0"}

    def query(*arguments):
        result = runner([*prefix, *arguments], cwd=candidate, env=environment, stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL, timeout=10, check=False)
        require(type(result.returncode) is int and type(result.stdout) is bytes
                and len(result.stdout) <= GIT_OUTPUT_CAP, "launchctl_authority")
        return result

    def ancestor(older, newer):
        # Exit 0 is an ancestor (an equal commit counts), exit 1 is not. Any other
        # status, including an unknown object (128) or a signal, is a refusal.
        status = query("merge-base", "--is-ancestor", older, newer).returncode
        require(status in (0, 1), "launchctl_authority")
        return status == 0

    def linear(older, newer):
        # `rev-list --merges --max-count=1 older..newer` prints the first merge commit
        # in the pushed range, if any: empty output is a merge-free range. Any status
        # but 0 (an unknown object, a signal) is a refusal, never "linear".
        result = query("rev-list", "--merges", "--max-count=1", older + ".." + newer, "--")
        require(result.returncode == 0, "launchctl_authority")
        return result.stdout == b""

    try:
        result = query("log", "-1", "--format=%H%n%P", "HEAD", "--")
        require(result.returncode == 0, "launchctl_authority")
        lines = result.stdout.decode("ascii").splitlines()
        require(len(lines) == 2, "launchctl_authority")
        return {"checked_out_sha": lines[0], "candidate_parents": lines[1].split(),
                "scope_base_precedes_before": ancestor(SCOPE_BASE, before),
                "before_precedes_candidate": ancestor(before, "HEAD"),
                "pushed_range_linear": linear(before, "HEAD")}
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
    # `before` must be a real lowercase 40-hex tip different from `after`.
    before = event.get("before")
    require(full_sha(before) and before != event.get("after")
            and event.get("after") == observation["candidate_sha"] and event.get("ref") == REF
            and all(event.get(key) is False for key in ("forced", "created", "deleted")), "launchctl_authority")
    identity = query_candidate(candidate, before)
    require(type(identity) is dict and set(identity) == IDENTITY_KEYS, "launchctl_authority")
    current = {**observation, **identity, "event_before": before, "event_after": event["after"],
        "event_ref": event["ref"], **{"event_" + key: event[key] for key in ("forced", "created", "deleted")}}
    require(admission_policy(current), "launchctl_authority")
    return current


def child_environment_allowed(env):
    return not any(key in env for key in ("GH_TOKEN", "GITHUB_TOKEN", "BUN_AUTH_TOKEN", "NODE_OPTIONS", "PYTHONPATH"))


def service_inputs_allowed(plist_present, plist_symlink, command_path):
    # Metadata observations, not a launchctl probe or service-controller effect.
    return plist_present is False and plist_symlink is False and command_path == "/bin/launchctl"


def fixture_port_allowed(port):
    return type(port) is int and port in {8443, 18444, 18445}


def deployment_query_allowed(path, query):
    # appdeploy.publicResourceURL/checkPublicBundle: QueryEscape leaves this
    # bounded, literal SHA/int64/nonnegative-int grammar unchanged. Do not
    # decode queries or admit arbitrary cache keys, duplicate keys or mixtures.
    match = re.fullmatch(r"herdr_deploy_check=[0-9a-f]{40}-(0|-?[1-9][0-9]{0,18})-"
                         r"(0|[1-9][0-9]{0,18})(?:-(entry|javascript|stylesheet|br-entry|"
                         r"br-javascript|br-stylesheet|version|version-br))?", query)
    if match is None:
        return False
    nonce, attempt, resource = match.groups()
    if not -(2**63) <= int(nonce) < 2**63 or not 0 <= int(attempt) < 2**63:
        return False
    if path == "/release.json":
        return resource is None
    if path == "/version.json":
        return resource in {"version", "version-br"}
    if re.fullmatch(r"/builds/[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-f0-9]{16,64}/index\.html", path):
        return resource in {"entry", "br-entry"}
    if re.fullmatch(r"/assets/app-[a-f0-9]{64}\.js", path):
        return resource in {"javascript", "br-javascript"}
    if re.fullmatch(r"/assets/app-[a-f0-9]{64}\.css", path):
        return resource in {"stylesheet", "br-stylesheet"}
    return False


BUILD_ENTRY_PATH = re.compile(r"/builds/[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-f0-9]{16,64}/index\.html")


def browser_query_allowed(path, query):
    """The two exact query forms the shipped phone app itself issues.

    frontend/src/lib/updates.ts checkAppUpdate/waitForDeployedApp read
    `/version.json?check=<Date.now()>` on EVERY app start (and periodically), and
    cacheBustedAppUrl / public/manifest-loader.js retry navigate to
    `/index.html?herdr_reload=<version|current|recovery>-<Date.now()>`, which the
    relay web handler 307-redirects to the digest-addressed build entry while
    KEEPING the query. Each form is bound to its own path, literal and bounded.
    Query decoding, extra keys, mixtures and other paths stay refused.
    """
    if path == "/version.json":
        return re.fullmatch(r"check=[0-9]{1,16}", query) is not None
    if path in {"/", "/index.html"} or BUILD_ENTRY_PATH.fullmatch(path) is not None:
        return re.fullmatch(r"herdr_reload=[A-Za-z0-9._-]{1,64}-[0-9]{1,16}", query) is not None
    return False


def public_request_allowed(raw_path, host, upgrade, paths, port):
    try:
        parts = urllib.parse.urlsplit(raw_path)
    except ValueError:
        return False
    return (fixture_port_allowed(port) and not parts.scheme and not parts.netloc
            and parts.path in paths and not parts.fragment
            and (not parts.query or re.fullmatch(r"v=[A-Za-z0-9._-]{1,128}", parts.query) is not None
                 or (raw_path == parts.path + "?" + parts.query
                     and (deployment_query_allowed(parts.path, parts.query)
                          or browser_query_allowed(parts.path, parts.query))))
            and host == "127.0.0.1:" + str(port)
            and (not upgrade or (upgrade == "websocket" and parts.path in {"/", "/ws"} and not parts.query)))


def socket_pair_guard(counts):
    for pair, count in counts.items():
        if (type(pair) is not tuple or len(pair) != 2 or pair not in SOCKET_PAIRS
                or type(count) is not int or not 1 <= count <= COUNT_MAX):
            return "unexpected_pair"
        method, outcome = pair
        if method in SOCKET_ORDINARY and outcome == "succeeded":
            continue
        if method in SOCKET_PROBES and outcome == "refused_expected":
            if count > PROBE_CAP:
                return "probe_cap_exceeded"
            continue
        return "unexpected_pair"
    return "allowed"


def socket_operations_allowed(counts):
    return socket_pair_guard(counts) == "allowed"


def socket_counter_map_safe(item, keys):
    return (type(item) is dict and set(item) <= keys
            and all(type(count) is int and 1 <= count <= COUNT_MAX for count in item.values()))


def socket_row_counts(rows):
    require(type(rows) is list and len(rows) <= 26, "receipt_schema")
    counts = {}
    order = []
    for row in rows:
        require(type(row) is dict and set(row) == {"method", "outcome", "count"}, "receipt_schema")
        require(type(row["method"]) is str and type(row["outcome"]) is str, "receipt_schema")
        pair = (row["method"], row["outcome"])
        require(pair in SOCKET_PAIRS and pair not in counts
                and type(row["count"]) is int and 1 <= row["count"] <= COUNT_MAX, "receipt_schema")
        order.append(pair)
        counts[pair] = row["count"]
    require(order == sorted(order), "receipt_schema")
    return counts


def socket_diagnostic_guard(diagnostics):
    if diagnostics["saturated"]:
        return "counter_saturated"
    if any(diagnostics[key] for key in SOCKET_EXCEPTION_BUCKETS):
        return "recorded_exception"
    if diagnostics["liveness_before_cleanup"] != "serving":
        return "listener_not_alive"
    return socket_pair_guard({(row["method"], row["outcome"]): row["count"] for row in diagnostics["rows"]})


def socket_diagnostics_policy(diagnostics, passing=False, operation_rows=None):
    fields = {"schema", "rows", "reasons", *SOCKET_EXCEPTION_BUCKETS, "saturated",
              "liveness_before_cleanup", "closed_after_cleanup", "guard"}
    require(type(diagnostics) is dict and set(diagnostics) == fields
            and type(diagnostics["schema"]) is int and diagnostics["schema"] == 1, "receipt_schema")
    counts = socket_row_counts(diagnostics["rows"])
    require(socket_counter_map_safe(diagnostics["reasons"], SOCKET_REASONS)
            and all(socket_counter_map_safe(diagnostics[key], SOCKET_EXCEPTION_CATEGORIES) for key in SOCKET_EXCEPTION_BUCKETS)
            and type(diagnostics["saturated"]) is bool
            and type(diagnostics["liveness_before_cleanup"]) is str
            and diagnostics["liveness_before_cleanup"] in {"serving", "stopped", "early_death", "not_started"}
            and type(diagnostics["closed_after_cleanup"]) is str
            and diagnostics["closed_after_cleanup"] in {"closed", "open", "not_observed"}
            and type(diagnostics["guard"]) is str and diagnostics["guard"] in SOCKET_GUARDS, "receipt_schema")
    if not diagnostics["saturated"]:
        require(sum(counts.values()) < COUNT_MAX
                and sum(counts.values()) == sum(diagnostics["reasons"].values())
                and sum(diagnostics["handler_exceptions"].values()) == sum(diagnostics["reasons"].get(key, 0)
                    for key in ("recv_failed", "send_failed", "internal_error")), "receipt_schema")
    if passing:
        require(diagnostics["guard"] == socket_diagnostic_guard(diagnostics) == "allowed"
                and diagnostics["closed_after_cleanup"] == "closed"
                and set(diagnostics["reasons"]) <= {"ordinary", "inert_probe"}
                and diagnostics["rows"] == operation_rows, "receipt_socket_diagnostics")
    return True


def socket_fixture_allowed(fixture, receipt):
    current = fixture.diagnostic_summary()
    if fixture.stopped.is_set() and "socket_diagnostics" in receipt:
        # Cleanup stops the listener, but never excuses a late row or fault.
        current["liveness_before_cleanup"] = receipt["socket_diagnostics"]["liveness_before_cleanup"]
    return socket_diagnostic_guard(current) == "allowed"


def capture_socket_diagnostics(receipt, fixture, check=lambda: None):
    # Exactly one before-cleanup snapshot, even on a construction/guard failure.
    # Commit only a complete observation obtained within the caller's deadline.
    check()
    diagnostics = fixture.diagnostic_summary()
    diagnostics.update(closed_after_cleanup="not_observed", guard=socket_diagnostic_guard(diagnostics))
    socket_diagnostics_policy(diagnostics)
    check()
    receipt["socket_diagnostics"] = diagnostics
    receipt["fixture_operations"] = {"fake": receipt.get("fixture_operations", {}).get("fake", []),
                                     "socket": diagnostics["rows"]}
    return diagnostics


def finish_socket_diagnostics(diagnostics, owner, check=lambda: None):
    check()
    try:
        diagnostics["closed_after_cleanup"] = "closed" if owner.closed() else "open"
    except Exception:
        diagnostics["closed_after_cleanup"] = "not_observed"
    # owner.closed may catch an alarm interruption as an unavailable observation.
    # Check before the NEXT lock so a consumed one-shot alarm cannot leave that
    # subsequent acquisition unbounded.
    check()
    publication_summary = getattr(owner, "publication_summary", None)
    if publication_summary is None:
        return
    # A worker may finish its sole callback during retirement, even if closure
    # fails. Refresh only sticky publication evidence, never the captured rows,
    # reasons or pre-cleanup liveness. No filesystem I/O occurs under this lock.
    publication = publication_summary()
    check()
    # Sticky late fault categories (publication, held-stream, listener) only
    # ever grow. A late handler fault or non-allowed row cannot enter the
    # immutable captured rows/reasons, so it is carried as the final guard
    # reason instead, at the SAME priority as the live guard (saturation, then
    # any exception, then liveness, then pairs) even when the captured guard
    # was already a refusal. A late allowed row changes nothing.
    for bucket in ("publication_exceptions", "stream_exceptions", "listener_exceptions"):
        for category, count in publication.get(bucket, {}).items():
            diagnostics[bucket][category] = max(diagnostics[bucket].get(category, 0), count)
    diagnostics["saturated"] = diagnostics["saturated"] or publication["saturated"]
    guard = socket_diagnostic_guard(diagnostics)
    if guard not in {"counter_saturated", "recorded_exception"}:
        if publication.get("handler_exceptions"):
            guard = "recorded_exception"
        elif guard == "allowed":
            late_pair = publication.get("pair_guard", "allowed")
            guard = late_pair if late_pair in SOCKET_GUARDS else "unexpected_pair"
    diagnostics["guard"] = guard


def cleanup_socket_observation(receipt, fixture, cleanup, deadline, *, initial=False):
    """Runtime alarm is already armed; never renew it or retry an unavailable snapshot."""
    if fixture is None:
        return True
    try:
        deadline.check()
        if initial:
            capture_socket_diagnostics(receipt, fixture, deadline.check)
        else:
            require("socket_diagnostics" in receipt, "receipt_failure_validation")
            finish_socket_diagnostics(receipt["socket_diagnostics"], SocketOwner(fixture), deadline.check)
            require(receipt["socket_diagnostics"]["closed_after_cleanup"] != "not_observed",
                    "cleanup_uncertain")
        deadline.check()
        return True
    except Exception as error:
        cleanup.error("cleanup_deadline" if isinstance(error, Refusal)
                      and str(error) == "cleanup_deadline" else "cleanup_incomplete")
        receipt["cleanup"], receipt["result"] = False, "fail"
        receipt.setdefault("failure", "cleanup_uncertain")
        return False


def require_socket_cleanup_evidence(receipt, fixture):
    # A partial failure may retain a valid initial snapshot with explicit late
    # uncertainty. Without it there is no bounded valid runner receipt: retain
    # any existing operations, refuse staging, and let the nonzero exit bind the
    # separate publisher's lack-of-evidence failure. Never fabricate empty rows.
    if fixture is not None:
        require("socket_diagnostics" in receipt and "fixture_operations" in receipt,
                "receipt_failure_validation")
        socket_diagnostics_policy(receipt["socket_diagnostics"])
        require(receipt["fixture_operations"]["socket"] == receipt["socket_diagnostics"]["rows"],
                "receipt_failure_validation")


def proxy_operations_allowed(servers):
    return all(server.failure is False for server in servers)


# A latched proxy failure is recorded as a FIXED secret-free locator, never a raw
# path, query, header, token, address or exception message. Every value below is
# a member of one of these finite sets; anything else collapses to a default.
PROXY_FAILURE_REASONS = frozenset({
    "request_line", "request_method", "request_parse", "request_header_multiplicity", "request_allowlist",
    "request_body", "request_upgrade", "websocket_upgrade", "proxy_backend", "cap_individual", "cap_result",
    "phase_stopped", "phase_deadline", "handler_exception", "handler_finish", "server_error", "accept_error",
    "worker_exception"})
PROXY_PATH_CATEGORIES = frozenset({
    "unknown", "invalid", "root", "index", "healthz", "release", "version", "build_entry", "asset", "icon", "font",
    "manifest", "service_worker", "script", "favicon", "websocket", "other"})
PROXY_QUERY_CATEGORIES = frozenset({"unknown", "none", "version", "deploy_check", "check", "reload", "other"})
PROXY_METHOD_CATEGORIES = frozenset({"unknown", "get", "head", "post", "put", "delete", "options", "patch",
                                     "connect", "trace", "other"})
PROXY_FIXED_PATHS = MappingProxyType({
    "/": "root", "/index.html": "index", "/healthz": "healthz", "/release.json": "release",
    "/version.json": "version", "/favicon.ico": "favicon", "/sw.js": "service_worker",
    "/manifest.webmanifest": "manifest", "/setup.webmanifest": "manifest", "/herdr-bootstrap.js": "script",
    "/manifest-loader.js": "script", "/notification-icons.js": "script", "/ws": "websocket"})


def proxy_request_categories(raw_path):
    """(path, query) coarse categories of a request target. Never echoes any part of it."""
    try:
        parts = urllib.parse.urlsplit(raw_path)
    except (ValueError, TypeError, AttributeError):
        return "invalid", "other"
    if parts.scheme or parts.netloc:
        return "invalid", "other"
    path = parts.path
    if path in PROXY_FIXED_PATHS:
        path_category = PROXY_FIXED_PATHS[path]
    elif BUILD_ENTRY_PATH.fullmatch(path) is not None:
        path_category = "build_entry"
    else:
        path_category = next((name for prefix, name in (("/assets/", "asset"), ("/icons/", "icon"),
                                                         ("/fonts/", "font")) if path.startswith(prefix)), "other")
    query = parts.query
    if not query:
        query_category = "none"
    elif re.fullmatch(r"v=[A-Za-z0-9._-]{1,128}", query) is not None:
        query_category = "version"
    else:
        query_category = next((name for prefix, name in (("herdr_deploy_check=", "deploy_check"),
            ("check=", "check"), ("herdr_reload=", "reload")) if query.startswith(prefix)), "other")
    return path_category, query_category


def proxy_method_category(raw):
    try:
        name = raw.decode("ascii").lower() if type(raw) is bytes else str(raw).lower()
    except UnicodeError:
        return "other"
    return name if name in PROXY_METHOD_CATEGORIES - {"unknown", "other"} else "other"


def proxy_failure_reason(error):
    code = str(error) if isinstance(error, Refusal) else "handler_exception"
    return code if code in PROXY_FAILURE_REASONS else "handler_exception"


def proxy_failure_rows(servers):
    """One secret-free row per proxy that latched a failure (at most one per proxy)."""
    rows = []
    for index, server in enumerate(servers):
        if getattr(server, "failure", False) is not True:
            continue
        recorded = getattr(server, "failure_reason", None)
        if (type(recorded) is not tuple or len(recorded) != 4 or recorded[0] not in PROXY_FAILURE_REASONS
                or recorded[1] not in PROXY_PATH_CATEGORIES or recorded[2] not in PROXY_QUERY_CATEGORIES
                or recorded[3] not in PROXY_METHOD_CATEGORIES):
            recorded = ("handler_exception", "unknown", "unknown", "unknown")
        rows.append({"proxy": index, "reason": recorded[0], "path": recorded[1], "query": recorded[2],
                     "method": recorded[3]})
    return rows


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


def fake_read_only_shape(argv):
    """True only for an exact enumerated read-only query argv (no wildcard)."""
    if not (type(argv) is list and len(argv) >= 2 and all(type(arg) is str for arg in argv)):
        return False
    rule = FAKE_READ_ONLY.get(" ".join(argv[:2]))
    if rule is None:
        return False
    length, literals = rule
    return len(argv) == length and all(
        argv[index] == literals[index] if index in literals else (argv[index] != "" and not argv[index].startswith("-"))
        for index in range(2, length))


def fake_operation_summary(data, complete=False):
    """Strict summary: refuse any command outside the enumerated allowlist.

    Read-only queries may be started/succeeded/failed (cmd/fake-herdr records
    "failed" for read queries it does not implement; a query may still be in
    flight while the relay runs). The one harmless prompt must have succeeded
    when complete, and is never accepted as failed."""
    counts, sequences = {}, set()
    for line in data.splitlines():
        value = strict_json(line)
        require(type(value) is dict and type(value.get("sequence")) is int
                and value["sequence"] not in sequences, "fake_operation_sequence")
        sequences.add(value["sequence"])
        argv = value.get("argv")
        require(type(argv) is list and len(argv) >= 2 and all(type(arg) is str for arg in argv), "fake_operation_argv")
        command = " ".join(argv[:2])
        require(command in FAKE_COMMANDS, "fake_operation_allowlist")
        outcome = value.get("outcome")
        if command == FAKE_PROMPT:
            require(tuple(argv) == FAKE_PROMPT_ARGV, "fake_mutation_allowlist")
            require(outcome in {"started", "succeeded"} and (not complete or outcome == "succeeded"),
                    "fake_operation_outcome")
        else:
            require(fake_read_only_shape(argv), "fake_operation_allowlist")
            require(outcome in FAKE_OUTCOMES, "fake_operation_outcome")
        key = (command, outcome)
        counts[key] = counts.get(key, 0) + 1
    require(sum(count for (command, _), count in counts.items() if command == FAKE_PROMPT) <= 1,
            "fake_mutation_count")
    return [{"command": command, "outcome": outcome, "count": count}
            for (command, outcome), count in sorted(counts.items())]


def fake_operation_diagnostics(data):
    """Sanitized, secret-free bucketed summary for FAIL receipts. Never raises.

    Only category labels and counts survive: argv, paths, environment and
    identifiers are dropped; an unknown or malformed command is "other" and an
    unknown outcome is "other". Used to name the category of a guard refusal."""
    counts = {}
    for line in data[:FAKE_OPERATIONS_CAP].splitlines():
        try:
            value = json.loads(line)
        except (ValueError, RecursionError):
            continue
        if type(value) is not dict:
            continue
        argv = value.get("argv")
        command = "other"
        if type(argv) is list and len(argv) >= 2 and all(type(arg) is str for arg in argv):
            name = " ".join(argv[:2])
            if (name == FAKE_PROMPT and tuple(argv) == FAKE_PROMPT_ARGV) or (
                    name in FAKE_READ_ONLY and fake_read_only_shape(argv)):
                command = name
        outcome = value.get("outcome")
        key = (command, outcome if type(outcome) is str and outcome in FAKE_OUTCOMES else "other")
        counts[key] = min(counts.get(key, 0) + 1, COUNT_MAX)
    return [{"command": command, "outcome": outcome, "count": count}
            for (command, outcome), count in sorted(counts.items())]


def read_fake_operations(path, opener=os.open):
    """Bounded, validated snapshot of the live fake-herdr evidence file.

    cmd/fake-herdr writes a complete temp file and renames it over the
    pathname on every started/finished record, so the pathname's inode
    legitimately changes while the relay runs. Read through ONE opened
    descriptor instead of requiring the pathname to stay unchanged: the opened
    inode is never modified after its rename (an unlinked one has st_nlink 0),
    so it is an immutable, complete snapshot and no retry is needed. Symlinks,
    non-regular files, hard-linked files and oversize files still refuse; the
    content/shape validation of fake_operation_summary is unchanged."""
    try:
        descriptor = opener(str(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    except OSError:  # symlink (ELOOP), absent or unreadable evidence is a fixed refusal
        raise Refusal("file_identity") from None
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_nlink <= 1, "file_identity")
        require(info.st_size <= FAKE_OPERATIONS_CAP, "file_cap")
        chunks, size = [], 0
        while size <= FAKE_OPERATIONS_CAP:
            chunk = os.read(descriptor, min(65536, FAKE_OPERATIONS_CAP + 1 - size))
            if not chunk:
                break
            chunks.append(chunk)
            size += len(chunk)
        require(size <= FAKE_OPERATIONS_CAP, "file_cap")
        return b"".join(chunks)
    finally:
        os.close(descriptor)


def fake_failure_rows(path):
    """Best-effort bounded read for FAIL diagnostics; never raises."""
    try:
        return fake_operation_diagnostics(read_fake_operations(path).decode("utf-8", "replace"))
    except Exception:
        return []


def fake_rows_policy(rows, complete):
    """Closed shape of fake rows. complete=True is a PASS receipt: no "other",
    unique (command, outcome), prompt exactly once and succeeded. complete=False
    (FAIL diagnostics) additionally admits the "other" buckets."""
    commands = FAKE_COMMANDS if complete else FAKE_DIAGNOSTIC_COMMANDS
    outcomes = FAKE_OUTCOMES if complete else FAKE_DIAGNOSTIC_OUTCOMES
    if not (type(rows) is list and len(rows) <= FAKE_ROW_CAP):
        return False
    seen = set()
    for row in rows:
        if not (type(row) is dict and set(row) == {"command", "outcome", "count"}
                and type(row["command"]) is str and row["command"] in commands
                and type(row["outcome"]) is str and row["outcome"] in outcomes
                and type(row["count"]) is int and 0 < row["count"] <= COUNT_MAX
                and (row["command"], row["outcome"]) not in seen):
            return False
        seen.add((row["command"], row["outcome"]))
        if complete and row["command"] == FAKE_PROMPT and (row["outcome"] != "succeeded" or row["count"] != 1):
            return False
    return True


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
            "line": None, "command": "unknown", "exit_code": runner_exit,
            "helper_function": None, "helper_line": None, "helper_code": None}
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
                # Keep the fully validated, source-bound socket snapshot as
                # failure evidence, never the staged PASS or its assertions.
                value["socket_diagnostics"] = observed["socket_diagnostics"]
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
        self.failure, self.failure_reason = False, None
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
                    self.latch("accept_error")
                return

    def latch(self, reason, path="unknown", query="unknown", method="unknown"):
        """Latch the fatal proxy failure with a fixed secret-free first-cause locator."""
        # First cause wins. A concurrent second latch may race this check; either
        # value is a true cause, and both are fixed-set, so no lock is required.
        # The locator is published before the flag the guard polls.
        if getattr(self, "failure_reason", None) is None:
            self.failure_reason = (
                reason if reason in PROXY_FAILURE_REASONS else "handler_exception",
                path if path in PROXY_PATH_CATEGORIES else "unknown",
                query if query in PROXY_QUERY_CATEGORIES else "unknown",
                method if method in PROXY_METHOD_CATEGORIES else "unknown")
        self.failure = True

    def handle_error(self, _request, _address):
        # BaseServer catches dispatch errors before the accept loop sees them.
        # Latch them without its default raw traceback/address logging.
        self.latch("server_error")

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
                self.latch("worker_exception")
        finally:
            try:
                connection.close()
            except CLIENT_ABORTS:
                self.record("client_aborted" if accepted else "tls_refused")
            except Exception as error:
                if not owned_socket_closed(error, self.closing):
                    self.latch("worker_exception")
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
    # Coarse fixed-set categories of the request being handled, for the
    # secret-free first-cause locator only. Never the raw target.
    diag_method = diag_path = diag_query = "unknown"

    def log_message(self, *_):
        pass

    def latch(self, reason):
        self.server.latch(reason, self.diag_path, self.diag_query, self.diag_method)

    def backend_call(self, operation, *args, **kwargs):
        # Never misclassify a reset/timeout on the backend or proxy machinery
        # as a benign browser abort, even if handler.finish later masks it.
        try:
            return operation(*args, **kwargs)
        except Exception:
            self.latch("proxy_backend")
            raise Refusal("proxy_backend") from None

    def do_GET(self):
        server = self.server
        self.diag_path, self.diag_query = proxy_request_categories(self.path)
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
            if not response.startswith(b"HTTP/1.1 101 "):
                self.relay_upgrade_refusal(backend, response)
                return
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

    def relay_upgrade_refusal(self, backend, response):
        """Relay a legitimate non-101 upgrade answer to the browser; never latch it.

        The relay hub answers a handshake it will not upgrade with a bounded
        net/http error (400 wrong subprotocol, 403 origin, 503 pairing not ready
        or shutting down). A restarting backend legitimately does this, and the
        shipped client retries. Anything that is not a well-formed 4xx/5xx status
        line stays a fatal websocket_upgrade failure. The answer is forwarded
        verbatim and bounded; the connection is then closed.
        """
        head, _, body = bytes(response).partition(b"\r\n\r\n")
        lines = head.split(b"\r\n")
        require(re.fullmatch(rb"HTTP/1\.1 [45][0-9]{2}(?: [\x20-\x7e]{0,64})?", lines[0]) is not None,
                "websocket_upgrade")
        lengths = [line.split(b":", 1)[1].strip() for line in lines[1:]
                   if line.split(b":", 1)[0].strip().lower() == b"content-length" and b":" in line]
        data = bytearray(response)
        # Fail closed on every present framing declaration: exactly one canonical Content-Length
        # (no leading zeros, bounded digits) and no Transfer-Encoding, before anything is forwarded.
        require(all(line.split(b":", 1)[0].strip().lower() != b"transfer-encoding" for line in lines[1:])
                and len(lengths) <= 1
                and all(re.fullmatch(rb"(?:0|[1-9][0-9]{0,6})", length) is not None for length in lengths),
                "websocket_upgrade")
        if lengths:
            expected = int(lengths[0])
            cap("result", len(head) + 4 + expected)
            while len(body) < expected:
                chunk = self.backend_call(backend.recv, 4096)
                # EOF before the declared body is complete is a backend fault, not a refusal.
                require(bool(chunk), "websocket_upgrade")
                body += chunk
                data.extend(chunk)
        self.connection.sendall(bytes(data))
        self.server.record("websocket_refused")
        self.close_connection = True

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
            self.diag_method = proxy_method_category(words[0]) if words else "unknown"
            require(words and words[0] == b"GET", "request_method")
            require(self.parse_request(), "request_parse")
            self.do_GET()
            self.wfile.flush()
        except CLIENT_ABORTS:
            self.server.record("client_aborted")
            self.close_connection = True
        except Exception as error:
            if not owned_socket_closed(error, self.server.closing):
                self.latch(proxy_failure_reason(error))
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
                    self.latch("handler_finish")
                    first_error = first_error or error
        if first_error is not None:
            raise first_error

    def send_error(self, code, message=None, explain=None):
        # Parse errors remain fatal even if the client aborts during the error
        # response, masking the parser's original rejection.
        self.latch("request_parse")
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
    await check(empty,8443,'ERR_CERT_AUTHORITY_INVALID');
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
    await check(trusted,8443,null); observations.push('trusted_profile_accepts_ip');
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


def packaged_go_tls_control(root, operations, binary, version, revision, origin, category):
    require(category in {"positive", "unknown_ca", "wrong_host"}, "tls_category")
    # Production appdeploy.verifyPublic retries TLS errors until its two-minute
    # context expires. A ten-second wrapper deadline cannot observe that exit
    # or its categorized certificate error. Allow ten seconds of startup/exit
    # margin for negative controls; Operations.capture still clamps this to the
    # unchanged phase/global remainder. Never count a wrapper timeout as refusal.
    timeout = 10 if category == "positive" else 130
    return operations.capture([str(binary), "verify-public", "--origin", origin, "--web-root",
                               str(root / "release/web"), "--version", version, "--revision", revision], timeout)


def tls_controls(root, operations, binary, version, revision, candidate):
    outcomes = []
    for port, expected in ((8443, "positive"), (18444, "unknown_ca"), (18445, "wrong_host")):
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
        go = packaged_go_tls_control(root, operations, binary, version, revision, origin, expected)
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
    failure, proxy_servers, socket_fixture, fake_path = None, [], None, None
    closure_uncertain = False
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
            changed = command(["git", "diff", "--name-only", SCOPE_BASE, source, "--"]).decode().splitlines()
            require(candidate_scope_allowed(changed), "candidate_scope")
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
            require(tools["python"] == PYTHON_VERSION and tools["node"] == "24.21.0" and tools["bun"] == "1.4.0"
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
            def own_socket(fixture):
                nonlocal socket_fixture
                socket_fixture = fixture  # Retain even if allocation/bind/start raises.
                cleanup.listener(SocketOwner(fixture))
            socket_fixture = helper.HerdrSocketFixture(root / "runtime/herdr.sock", scenario,
                root / "socket-operations.json", on_owned=own_socket)
            socket_fixture.wait_ready()
            def socket_allowed():
                return socket_fixture_allowed(socket_fixture, receipt)
            operations.add_guard("herdr_socket", socket_allowed)
            for port, leaf in ((8443, "valid"), (18444, "unknown"), (18445, "wrong-host")):
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
                    fake_operation_summary(read_fake_operations(fake_path))
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
                "fake": fake_operation_summary(read_fake_operations(fake_path), complete=True),
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
        # Anchor once at cleanup entry, BEFORE diagnostic locks or other work.
        # The alarm and serialization reserve share this end (also globally
        # clamped); a slow/interrupted initial observation cannot renew it.
        start = budget.clock()
        deadline = CleanupDeadline(clock=budget.clock, started=start,
            latest=budget.start + LIMITS["total_seconds"], reserve=1)
        os.environ.pop("GH_TOKEN", None)
        os.environ.pop("GITHUB_TOKEN", None)
        try:
            try:
                with deadline.alarm():
                    if not cleanup_socket_observation(receipt, socket_fixture, cleanup, deadline, initial=True):
                        failure = failure or "cleanup_uncertain"
                    try:
                        # Recorded closure uncertainty (browser contexts a guard-aborted
                        # runner could not attest) must not skip owned-root deletion when
                        # every owned child and listener is observed gone. It stays in the
                        # evidence, keeps cleanup False and never replaces the original
                        # failure. Any other uncertainty still aborts before deletion.
                        closure_uncertain = close_owned_resources(cleanup, deadline)
                        receipt["owned_cleanup"] = cleanup.evidence
                        if closure_uncertain:
                            failure = failure or "cleanup_uncertain"
                    finally:
                        if not cleanup_socket_observation(receipt, socket_fixture, cleanup, deadline):
                            failure = failure or "cleanup_uncertain"
                    # Final observations follow every owned closure/reap: late
                    # failures cannot race the last pre-cleanup acceptance check.
                    try:
                        operations.check_guards(deadline.check)
                        if failure is None and socket_fixture is not None:
                            receipt["fixture_operations"]["fake"] = fake_operation_summary(
                                read_fake_operations(fake_path), complete=True)
                    except Exception as error:
                        if failure is None:
                            failure = safe_failure_code(error)
                            receipt["failure_detail"] = failure_detail(error)
                    # A FAIL receipt otherwise carries no fake-operation rows, so a
                    # fake_herdr refusal cannot name its category. Record the
                    # sanitized bucketed summary (never argv/paths/ids); an
                    # unreadable file simply leaves the rows empty.
                    if failure is not None and fake_path is not None and "fixture_operations" in receipt:
                        receipt["fixture_operations"]["fake"] = fake_failure_rows(fake_path)
                    # Even a late guard failure still attempts owned-root cleanup.
                    if root_identity is not None:
                        remove_owned_tree(root, root_identity, deadline)
                    else:
                        require(not root.exists() and not root.is_symlink(), "cleanup_root_unowned")
                    deadline.check()
                    receipt["owned_cleanup"]["root_deleted"] = True
                    receipt["cleanup"] = not closure_uncertain
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
                # Name the first cause of every latched proxy failure (fixed codes
                # and coarse categories only). A latch that no guard observed yet
                # still revokes PASS.
                latched = proxy_failure_rows(proxy_servers)
                if latched:
                    receipt["proxy_failure"] = latched
                    failure = failure or "owned_operation_allowlist"
                    operations.failure_guard = operations.failure_guard or "proxy"
                if operations.failure_guard is not None:
                    receipt["failure_guard"] = operations.failure_guard
                receipt["result"] = "pass" if failure is None and receipt["cleanup"] and len(receipt["cases"]) == 3 else "fail"
                if failure:
                    receipt["failure"] = failure if failure in SAFE_FAILURES else "fixture_refused"
                receipt["source_hashes"] = initial_sources
                # Carry late faults into serialization under the SAME end,
                # without reacquiring an unavailable initial observation. A
                # refresh interruption may keep valid partial failure evidence,
                # but must revoke PASS before validation or staging.
                if not cleanup_socket_observation(receipt, socket_fixture, cleanup, deadline):
                    failure = failure or "cleanup_uncertain"
                require_socket_cleanup_evidence(receipt, socket_fixture)
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

    def publication_summary(self):
        counters = self.fixture.counters
        with counters.lock:
            return {"publication_exceptions": dict(counters.publication_exceptions),
                    "stream_exceptions": dict(counters.stream_exceptions),
                    "listener_exceptions": dict(counters.listener_exceptions),
                    "handler_exceptions": dict(counters.handler_exceptions),
                    "pair_guard": socket_pair_guard(dict(counters.counts)), "saturated": counters.saturated}


def offline_check(group):
    """Finite offline assertions; helper import only after the finalizer's AST inspection."""
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
    if group in {"parser", "phase-caps", "cleanup"}:
        spec = importlib.util.spec_from_file_location("extracted_package_fixture", Path(__file__).resolve().parents[1] / FILES[2])
        helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helper)
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
            "tools": {"python": PYTHON_VERSION, "node": "24.21.0", "bun": "1.4.0", "go": "1.27.1",
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
        socket_rows = valid_receipt["fixture_operations"]["socket"]
        socket_rows.extend({"method": method, "outcome": "refused_expected", "count": 1} for method in sorted(SOCKET_PROBES))
        socket_rows.sort(key=lambda row: (row["method"], row["outcome"]))
        valid_receipt["socket_diagnostics"] = {"schema": 1, "rows": socket_rows,
            "reasons": {"ordinary": 3, "inert_probe": 3},
            **{key: {} for key in SOCKET_EXCEPTION_BUCKETS}, "saturated": False,
            "liveness_before_cleanup": "serving", "closed_after_cleanup": "closed", "guard": "allowed"}
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
                    (1, "tls_refused", 0), (2, "tls_refused", 0), (1, "http_get", 1), (2, "websocket", 1),
                    (1, "websocket_refused", 1), (2, "websocket_refused", 1)):
                requests = [dict(request) for request in with_aborts["request_counts"]]
                requests[index][label] = amount
                require(rejects(lambda: receipt_policy(encode({**with_aborts, "request_counts": requests}),
                    BASE, sources, producer_id)), "offline_abort_not_a_control")
            return True
        def proxy_failure_receipts():
            # The secret-free first-cause locator is a FAIL-only field with a closed schema.
            failed = {**valid_receipt, "result": "fail", "failure": "owned_operation_allowlist",
                      "failure_guard": "proxy"}
            row = {"proxy": 0, "reason": "request_allowlist", "path": "version", "query": "check", "method": "get"}
            require(receipt_policy(encode({**failed, "proxy_failure": [row]}), BASE, sources, producer_id) is False,
                    "offline_proxy_failure_receipt")
            for field, values in (("reason", PROXY_FAILURE_REASONS), ("path", PROXY_PATH_CATEGORIES),
                                  ("query", PROXY_QUERY_CATEGORIES), ("method", PROXY_METHOD_CATEGORIES)):
                for value in values:
                    require(receipt_policy(encode({**failed, "proxy_failure": [{**row, field: value}]}),
                            BASE, sources, producer_id) is False, "offline_proxy_failure_values")
            require(receipt_policy(encode({**failed, "proxy_failure": [{**row, "proxy": index} for index in (0, 1, 2)]}),
                    BASE, sources, producer_id) is False and receipt_policy(encode({**failed, "proxy_failure": []}),
                    BASE, sources, producer_id) is False, "offline_proxy_failure_rows_bound")
            bad_rows = ([{**row, "proxy": 3}], [{**row, "proxy": -1}], [{**row, "proxy": True}], [{**row, "proxy": "0"}],
                [{**row, "reason": "/private/path"}], [{**row, "reason": "https://invalid"}], [{**row, "path": "/private"}],
                [{**row, "path": "builds"}], [{**row, "query": "check=1"}], [{**row, "query": "token"}],
                [{**row, "method": "GET"}], [{**row, "method": "secret"}], [{**row, "reason": None}],
                [{**row, "reason": ["request_allowlist"]}], [{**row, "url": "x"}], [{key: val for key, val in row.items()
                    if key != "query"}], [dict(row)] * 4, row, "request_allowlist", 1, None, [None], [[]])
            for bad in bad_rows:
                require(rejects(lambda: receipt_policy(encode({**failed, "proxy_failure": bad}), BASE, sources, producer_id)),
                        "offline_proxy_failure_refusal")
            # A PASS receipt can never carry a failure locator, and the counter for a relayed
            # non-101 upgrade answer is an ordinary non-negative count, not a free-form label.
            require(rejects(lambda: receipt_policy(encode({**valid_receipt, "proxy_failure": [row]}),
                    BASE, sources, producer_id)), "offline_proxy_failure_pass_refused")
            refused = {**valid_receipt, "request_counts": [{**request} for request in valid_receipt["request_counts"]]}
            refused["request_counts"][0]["websocket_refused"] = 2
            require(receipt_policy(encode(refused), BASE, sources, producer_id) is True, "offline_websocket_refused_count")
            for bad in (True, -1, "2", 1.5, 1000001):
                refused["request_counts"][0]["websocket_refused"] = bad
                require(rejects(lambda: receipt_policy(encode(refused), BASE, sources, producer_id)),
                        "offline_websocket_refused_count_type")
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
                      "command": None, "exit_code": None,
                      "helper_function": None, "helper_line": None, "helper_code": None}
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

        def dev_link_origin_contract():
            # iteration-2 hosted failure: the fixture origin used a port the production
            # development-link printer refuses. Bind the fixture origin to every
            # consumer of that contract by source, without running any of them.
            root = Path(__file__).resolve().parents[1]
            parsed = urllib.parse.urlsplit(ORIGIN)
            go_port = re.search(r"DevelopmentHTTPSPort\s*=\s*(\d+)", (root / "internal/tailscalecli/client.go").read_text())
            printer = (root / "cmd/herdr-mobile-relay/dev_tailscale_cli.go").read_text()
            browser = (root / "tests/tailscale-package-browser.mjs").read_text()
            require(parsed.scheme == "https" and parsed.hostname == "127.0.0.1" and parsed.port == 8443
                    and parsed.path == "" and parsed.query == "" and parsed.fragment == ""
                    and ORIGIN == "https://127.0.0.1:8443", "offline_origin_canonical")
            require(go_port is not None and int(go_port.group(1)) == parsed.port
                    and "relayURL.Port() != strconv.Itoa(tailscalecli.DevelopmentHTTPSPort)" in printer,
                    "offline_origin_printer_port")
            # The frozen runner permits exactly this origin's WSS twin as the one-use
            # import dial (refusing and recording any other destination), so the
            # development relay origin must be this same authority.
            require(browser.count("input.origin.replace(/^https:/, 'wss:')") == 2
                    and "allowed = new URL(route.request().url()).origin === input.origin" in browser,
                    "offline_origin_browser_destinations")
            require(fixture_port_allowed(8443) and fixture_port_allowed(18444) and fixture_port_allowed(18445)
                    and not fixture_port_allowed(18443) and not fixture_port_allowed(443), "offline_origin_ports")
            return True

        def helper_locator_cases():
            # A real helper GateFailure (the iteration-2 hosted shape): the locator names
            # the die() CALLER line inside the helper, plus the fixed code, while the
            # Darwin-file locator still reports its own innermost frame.
            try:
                helper.development_setup_link(Path("/nonexistent-printer"), "https://x.invalid/#setup=" + "a" * 32, ORIGIN)
            except helper.GateFailure as error:
                detail = failure_detail(error)
            else:
                return False
            helper_lines = Path(__file__).resolve().parents[1].joinpath(FILES[2]).read_text().splitlines()
            require(detail["type"] == "GateFailure" and detail["helper_function"] == "development_setup_link"
                    and detail["helper_code"] == "fixture_assertion" and type(detail["helper_line"]) is int
                    and "die(" in helper_lines[detail["helper_line"] - 1]
                    and "printer fixture is unavailable" in helper_lines[detail["helper_line"] - 1]
                    and detail["function"] == "unknown", "offline_helper_locator_real")
            require(set(detail) == {"type", "errno", "function", "line", "command", "exit_code",
                                    "helper_function", "helper_line", "helper_code"}, "offline_helper_locator_shape")
            # No helper frame: function/line are null (the code is a property of the
            # GateFailure itself); a non-GateFailure never reports a code.
            require(helper_locator(helper.GateFailure("fixture_assertion")) ==
                    {"helper_function": None, "helper_line": None, "helper_code": "fixture_assertion"}
                    and helper_locator(Refusal("x")) == {"helper_function": None, "helper_line": None,
                                                         "helper_code": None}, "offline_helper_locator_absent")
            def synthetic(function, code, filename=None, kind="GateFailure"):
                exception = type(kind, (RuntimeError,), {})("secret message")
                exception.code = code
                namespace = {"error": exception}
                source = "def " + function + "():\n    raise error\n"
                try:
                    exec(compile(source, filename or "/any/checkout/" + FILES[2], "exec"), namespace)
                    namespace[function]()
                except RuntimeError as raised:
                    return helper_locator(raised)
            require(synthetic("development_setup_link", "archive_binary_missing") ==
                    {"helper_function": "development_setup_link", "helper_line": 2, "helper_code": "archive_binary_missing"}
                    and synthetic("https___evil_example_token", "fixture_assertion")["helper_function"] == "unknown"
                    and synthetic("die", "fixture_assertion")["helper_function"] is None
                    and synthetic("development_setup_link", "/private/tmp/credential")["helper_code"] is None
                    and synthetic("development_setup_link", "fixture_refused")["helper_code"] is None
                    and synthetic("development_setup_link", "archive_binary_missing", kind="OSError")["helper_code"] is None
                    and synthetic("development_setup_link", "fixture_assertion", "/any/checkout/" + FILES[2] + ".bak")
                        == {"helper_function": None, "helper_line": None, "helper_code": "fixture_assertion"}
                    and synthetic("development_setup_link", "fixture_assertion", "/any/other.py")
                        == {"helper_function": None, "helper_line": None, "helper_code": "fixture_assertion"},
                    "offline_helper_locator_safe_sets")
            # The fixed sets cannot rot: every named function exists in the helper and
            # every code is one the helper itself defines.
            import ast
            names = {node.name for node in ast.walk(ast.parse("\n".join(helper_lines))) if isinstance(node, ast.FunctionDef)}
            require(SAFE_HELPER_FUNCTIONS - {"unknown"} <= names and "die" not in SAFE_HELPER_FUNCTIONS
                    and SAFE_HELPER_CODES <= helper.FAILURE_CODES, "offline_helper_locator_sets_exist")
            # Receipt policy accepts the helper locator and refuses unsafe or mistyped values.
            failed = {**valid_receipt, "result": "fail", "failure": "unexpected_exception", "failure_detail": detail}
            require(receipt_policy(encode(failed), BASE, sources, producer_id) is False, "offline_helper_locator_receipt")
            for key, bad in (("helper_line", "7"), ("helper_line", True), ("helper_line", 0), ("helper_line", 1000001),
                    ("helper_function", "evil_name"), ("helper_function", 7), ("helper_code", "evil_code"),
                    ("helper_code", 7)):
                require(rejects(lambda: receipt_policy(encode({**failed, "failure_detail": {**detail, key: bad}}),
                        BASE, sources, producer_id)), "offline_helper_locator_receipt_refused")
            for key in ("helper_function", "helper_line", "helper_code"):
                require(rejects(lambda: receipt_policy(encode({**failed, "failure_detail": {
                        name: item for name, item in detail.items() if name != key}}), BASE, sources, producer_id)),
                    "offline_helper_locator_receipt_incomplete")
            return True

        def sensitive_failures():
            path_value = "/private/tmp/credential"
            token_value = "g8Qv3mZ6rT2pN9xL4cW7sK5uH0dF1jB8aE6yR3iM9oP2nS4tV7zX5"
            failed = {**valid_receipt, "result": "fail", "failure": "unexpected_exception",
                "failure_detail": {"type": "Refusal", "errno": None, "function": "check", "line": 1,
                                   "command": None, "exit_code": None,
                                   "helper_function": None, "helper_line": None, "helper_code": None}}
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
            for field in ("command", "function", "type", "helper_function", "helper_code"):
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
            require(helper_locator_cases(), "offline_helper_locator")
            return safe_failure_receipts()

        def socket_pass_refusals():
            for pair in sorted(SOCKET_PAIRS):
                if socket_operations_allowed({pair: 1}):
                    continue
                value = strict_json(encode(valid_receipt), "receipt")
                rows = [{"method": pair[0], "outcome": pair[1], "count": 1}]
                value["fixture_operations"]["socket"] = rows
                value["socket_diagnostics"].update(rows=rows, reasons={"unsupported_method": 1}, guard="unexpected_pair")
                require(rejects(lambda: receipt_policy(encode(value), BASE, sources, producer_id)), "offline_refused_pair_pass")
            for bucket in SOCKET_EXCEPTION_BUCKETS:
                for category in SOCKET_EXCEPTION_CATEGORIES:
                    require(receipt_reject(("socket_diagnostics", bucket), {category: 1}), "offline_exception_pass")
            require(receipt_reject(("socket_diagnostics", "saturated"), True), "offline_saturation_pass")
            for guard in SOCKET_GUARDS - {"allowed"}:
                require(receipt_reject(("socket_diagnostics", "guard"), guard), "offline_socket_guard_pass")
            for liveness in ("stopped", "early_death", "not_started"):
                require(receipt_reject(("socket_diagnostics", "liveness_before_cleanup"), liveness), "offline_listener_pass")
            for closed in ("open", "not_observed"):
                require(receipt_reject(("socket_diagnostics", "closed_after_cleanup"), closed), "offline_socket_closure_pass")
            require(receipt_reject(("socket_diagnostics", "rows"), []), "offline_socket_row_agreement")
            return True

        def socket_privacy():
            diagnostics = valid_receipt["socket_diagnostics"]
            invalid = [(("unknown",), 1), (("publication_exceptions_unknown",), {"internal": 1}),
                (("schema",), True), (("schema",), 2), (("saturated",), 1),
                (("rows",), diagnostics["rows"] * 6), (("rows",), list(reversed(diagnostics["rows"]))),
                (("rows",), [diagnostics["rows"][0]] * 2), (("rows", 0, "unknown"), 1),
                (("rows", 0, "method"), "unsafe.mutation"), (("rows", 0, "outcome"), "other"),
                (("reasons",), {"ordinary": 1}), (("reasons",), {"unknown": 6}),
                (("liveness_before_cleanup",), "alive"), (("closed_after_cleanup",), "unknown"), (("guard",), "unknown")]
            for bucket in SOCKET_EXCEPTION_BUCKETS:
                invalid.append(((bucket,), {"unknown": 1}))
            for bad in (True, 0, 65536, -1, 1.0, "1"):
                invalid.append((("rows", 0, "count"), bad))
                invalid.append((("reasons",), {"ordinary": bad}))
                invalid.extend(((bucket,), {"internal": bad}) for bucket in SOCKET_EXCEPTION_BUCKETS)
            for bad in ("/private/tmp/credential", "../private/credential", "inert-opaque-token-123456789"):
                for field in ("schema", "rows", "reasons", *SOCKET_EXCEPTION_BUCKETS, "saturated",
                              "liveness_before_cleanup", "closed_after_cleanup", "guard"):
                    invalid.append(((field,), bad))
                for field in ("method", "outcome", "count"):
                    invalid.append((("rows", 0, field), bad))
                invalid.append((("reasons",), {bad: 1}))
                invalid.extend(((bucket,), {bad: 1}) for bucket in SOCKET_EXCEPTION_BUCKETS)
            for path, replacement in invalid:
                value = strict_json(encode(valid_receipt), "receipt")
                value.update(result="fail", failure="unexpected_exception")
                selected = value["socket_diagnostics"]
                for part in path[:-1]:
                    selected = selected[part]
                selected[path[-1]] = replacement
                try:
                    receipt_policy(encode(value), BASE, sources, producer_id)
                except Refusal as error:
                    require(str(error) in {"receipt_schema", "receipt_sensitive"}, "offline_socket_no_echo")
                    require(not any(marker in str(error) for marker in ("/private", "credential", "inert-opaque-token")),
                            "offline_publication_private_fields_no_echo")
                else:
                    return False
            for pair in (("ping", "refused_expected"), ("other", "succeeded"), ("tab.move", "succeeded")):
                require(receipt_reject(("socket_diagnostics", "rows"), [
                    {"method": pair[0], "outcome": pair[1], "count": 1}], failed=True), "offline_impossible_row")
            for result in ("pass", "fail"):
                missing = {key: value for key, value in valid_receipt.items() if key != "socket_diagnostics"}
                if result == "fail":
                    missing.update(result="fail", failure="unexpected_exception")
                require(rejects(lambda: receipt_policy(encode(missing), BASE, sources, producer_id)), "offline_missing_socket_diagnostics")
            return True

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
            def preserve_socket_failure(diagnostics):
                failed = {**valid_receipt, "result": "fail", "failure": "owned_operation_allowlist",
                    "failure_guard": "herdr_socket", "socket_diagnostics": diagnostics,
                    "fixture_operations": {**valid_receipt["fixture_operations"], "socket": diagnostics["rows"]}}
                data = (json.dumps(failed, separators=(",", ":")) + "\n").encode()
                before = encode(diagnostics)
                require(socket_diagnostics_policy(diagnostics)
                        and receipt_policy(data, BASE, sources, producer_id) is False,
                        "offline_saturation_failure_valid")
                for status in (0, 17):
                    staged = ReceiptFile(data)
                    qualified, observed, uploaded = publish(staged, status)
                    require(not qualified and observed == failed and uploaded == staged.data == data
                            and encode(diagnostics) == before, "offline_saturation_failure_bytes_preserved")
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
            require(observed["socket_diagnostics"] == valid_receipt["socket_diagnostics"], "offline_failed_exit_socket_snapshot")
            # Synthetic aggregate boundaries, not observed historical traffic.
            # Ordinary rows and bounded inert probes split EVERY bucket below
            # COUNT_MAX while the handled-connection total reaches/exceeds it.
            for total in (COUNT_MAX - 1, COUNT_MAX, COUNT_MAX + 1):
                rows = [{"method": "pane.read", "outcome": "succeeded", "count": total - 4},
                        {"method": "ping", "outcome": "succeeded", "count": 1}]
                rows.extend({"method": method, "outcome": "refused_expected", "count": 1}
                            for method in sorted(SOCKET_PROBES))
                rows.sort(key=lambda row: (row["method"], row["outcome"]))
                reasons = {"ordinary": total - 3, "inert_probe": 3}
                require(sum(row["count"] for row in rows) == sum(reasons.values()) == total
                        and all(0 < count < COUNT_MAX for count in (
                            *[row["count"] for row in rows], *reasons.values()))
                        and socket_operations_allowed(socket_row_counts(rows)), "offline_aggregate_subcap_buckets")
                for saturated in (False, True):
                    aggregate = strict_json(encode(valid_receipt), "receipt")
                    aggregate["fixture_operations"]["socket"] = rows
                    diagnostics = aggregate["socket_diagnostics"]
                    diagnostics.update(rows=rows, reasons=reasons, saturated=saturated,
                                       guard="counter_saturated" if saturated else "allowed")
                    before = encode(diagnostics)
                    valid_snapshot = saturated or total < COUNT_MAX
                    if valid_snapshot:
                        require(socket_diagnostics_policy(diagnostics), "offline_aggregate_diagnostic_valid")
                    else:
                        require(rejects(lambda: socket_diagnostics_policy(diagnostics)),
                                "offline_aggregate_unsaturated_diagnostic_refused")
                    # Also try a forged allowed guard with matching fixture
                    # rows and a successful runner exit: none excuses saturation.
                    for guard in (("counter_saturated", "allowed") if saturated else ("allowed",)):
                        claimed = {**aggregate, "socket_diagnostics": {**diagnostics, "guard": guard}}
                        passing = not saturated and total < COUNT_MAX
                        if passing:
                            require(socket_diagnostics_policy(claimed["socket_diagnostics"], passing=True,
                                        operation_rows=rows)
                                    and receipt_policy(encode(claimed), BASE, sources, producer_id),
                                    "offline_aggregate_below_boundary_pass")
                        else:
                            require(rejects(lambda: socket_diagnostics_policy(claimed["socket_diagnostics"],
                                        passing=True, operation_rows=rows))
                                    and rejects(lambda: receipt_policy(encode(claimed), BASE, sources, producer_id)),
                                    "offline_aggregate_direct_and_uploaded_pass_refused")
                        staged = ReceiptFile(encode(claimed))
                        qualified, observed, _ = publish(staged, 0)
                        require(qualified is passing and observed["result"] == ("pass" if passing else "fail")
                                and staged.data == encode(claimed), "offline_aggregate_publication_exit_zero")
                        require(observed == claimed if passing else observed["failure"] == "receipt_validation",
                                "offline_aggregate_publication_fail_closed")
                    # A truthful saturated failure below the terminal boundary
                    # can come from an independently clipped exception bucket.
                    failed_diagnostics = diagnostics
                    if total < COUNT_MAX:
                        failed_diagnostics = {**diagnostics,
                            "publication_exceptions": {"os_error": COUNT_MAX if saturated else 1},
                            "guard": "counter_saturated" if saturated else "recorded_exception"}
                    if valid_snapshot:
                        preserve_socket_failure(failed_diagnostics)
                    else:
                        failed = {**aggregate, "result": "fail", "failure": "owned_operation_allowlist",
                            "failure_guard": "herdr_socket", "socket_diagnostics": failed_diagnostics}
                        require(rejects(lambda: receipt_policy(encode(failed), BASE, sources, producer_id)),
                                "offline_aggregate_impossible_failure_refused")
                        for status in (0, 17):
                            staged = ReceiptFile(encode(failed))
                            qualified, observed, _ = publish(staged, status)
                            require(not qualified and observed["result"] == "fail"
                                    and "socket_diagnostics" not in observed and staged.data == encode(failed),
                                    "offline_aggregate_impossible_failure_not_forwarded")
                    require(encode(diagnostics) == before, "offline_aggregate_observations_unchanged")
            # Unsaturated row/reason and handler/reason disagreement still
            # refuses FAIL evidence, independently of the aggregate boundary.
            for changes in ({"reasons": {"ordinary": 2, "inert_probe": 3}},
                            {"handler_exceptions": {"timeout": 1}, "guard": "recorded_exception"}):
                diagnostics = {**strict_json(encode(valid_receipt["socket_diagnostics"])), **changes}
                failed = {**valid_receipt, "result": "fail", "failure": "owned_operation_allowlist",
                    "failure_guard": "herdr_socket", "socket_diagnostics": diagnostics}
                require(rejects(lambda: socket_diagnostics_policy(diagnostics))
                        and rejects(lambda: receipt_policy(encode(failed), BASE, sources, producer_id)),
                        "offline_unsaturated_counter_disagreement_refused")
                qualified, observed, _ = publish(ReceiptFile(encode(failed)), 0)
                require(not qualified and observed["failure"] == "receipt_validation"
                        and "socket_diagnostics" not in observed, "offline_counter_disagreement_not_forwarded")
            # Matching handler failures remain genuine unsaturated evidence.
            diagnostics = strict_json(encode(valid_receipt["socket_diagnostics"]))
            diagnostics.update(rows=[{"method": "other", "outcome": "failed", "count": 1}],
                reasons={"recv_failed": 1}, handler_exceptions={"timeout": 1}, guard="recorded_exception")
            preserve_socket_failure(diagnostics)
            # Acquisition-consistent separate bucket clipping: terminal rows
            # and reasons (or handler categories and reasons) can disagree.
            diagnostics = strict_json(encode(valid_receipt["socket_diagnostics"]))
            diagnostics.update(rows=[{"method": "pane.read", "outcome": "succeeded", "count": COUNT_MAX},
                                     {"method": "ping", "outcome": "succeeded", "count": 1}],
                reasons={"ordinary": COUNT_MAX}, saturated=True, guard="counter_saturated")
            require(sum(row["count"] for row in diagnostics["rows"]) != sum(diagnostics["reasons"].values()),
                    "offline_clipped_terminal_totals_disagree")
            preserve_socket_failure(diagnostics)
            diagnostics = strict_json(encode(valid_receipt["socket_diagnostics"]))
            diagnostics.update(rows=[{"method": "other", "outcome": "failed", "count": COUNT_MAX}],
                reasons={"recv_failed": COUNT_MAX}, handler_exceptions={"timeout": COUNT_MAX, "os_error": 1},
                saturated=True, guard="counter_saturated")
            require(sum(diagnostics["handler_exceptions"].values()) != sum(diagnostics["reasons"].values()),
                    "offline_clipped_exception_totals_disagree")
            preserve_socket_failure(diagnostics)
            # Exception buckets can saturate with only a few terminal rows;
            # the saturation flag must not be equated with the row total.
            diagnostics = strict_json(encode(valid_receipt["socket_diagnostics"]))
            diagnostics.update(publication_exceptions={"os_error": COUNT_MAX},
                               saturated=True, guard="counter_saturated")
            preserve_socket_failure(diagnostics)
            for category in SOCKET_EXCEPTION_CATEGORIES:
                fault = strict_json(encode(valid_receipt), "receipt")
                fault["socket_diagnostics"]["publication_exceptions"] = {category: 1}
                # A claimed allowed guard, successful exit, or later complete
                # write cannot turn the sticky callback fault into PASS.
                require(rejects(lambda: receipt_policy(encode(fault), BASE, sources, producer_id))
                        and socket_diagnostic_guard(fault["socket_diagnostics"]) == "recorded_exception",
                        "offline_publication_fault_direct_pass_refused")
                for _ in range(2):
                    fault_staged = ReceiptFile()
                    write_receipt(fault_staged, encode(fault), CleanupDeadline(clock=clock))
                    qualified, observed, data = publish(fault_staged, 0)
                    require(not qualified and observed["result"] == "fail" and observed["failure"] == "receipt_validation"
                            and strict_json(fault_staged.data, "receipt")["socket_diagnostics"]["publication_exceptions"] == {category: 1}
                            and b"/private" not in data and b"inert-token" not in data,
                            "offline_later_write_callback_fault_not_pass")
                late_fault = strict_json(encode(valid_receipt), "receipt")
                before = strict_json(encode(late_fault["socket_diagnostics"]), "receipt")
                late_owner = SimpleNamespace(closed=lambda: True, publication_summary=lambda: {
                    "publication_exceptions": {category: 1}, "saturated": False})
                finish_socket_diagnostics(late_fault["socket_diagnostics"], late_owner)
                require(late_fault["socket_diagnostics"] == {**before, "publication_exceptions": {category: 1},
                        "guard": "recorded_exception"}
                        and rejects(lambda: receipt_policy(encode(late_fault), BASE, sources, producer_id)),
                        "offline_late_receipt_refresh_refuses_direct_pass")
                qualified, observed, _ = publish(ReceiptFile(encode(late_fault)), 0)
                require(not qualified and observed["result"] == "fail" and observed["failure"] == "receipt_validation",
                        "offline_late_receipt_refresh_refuses_uploaded_pass")
                fault["socket_diagnostics"]["guard"] = "recorded_exception"
                fault.update(result="fail", failure="owned_operation_allowlist", failure_guard="herdr_socket")
                require(receipt_policy(encode(fault), BASE, sources, producer_id) is False,
                        "offline_publication_fault_valid_failure")
                for status in (0, 1):
                    qualified, observed, _ = publish(ReceiptFile(encode(fault)), status)
                    require(not qualified and observed == fault, "offline_publication_fault_failure_preserved")
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
                (("tools", "python"), "3.13.1"), (("tools", "python"), PYTHON_VERSION + "1"),
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
        def fake_operation_cases():
            canonical = {
                "agent list": ["agent", "list"], "pane list": ["pane", "list"],
                "workspace list": ["workspace", "list"], "tab list": ["tab", "list"],
                "integration status": ["integration", "status"], "agent get": ["agent", "get", "1-1"],
                "pane process-info": ["pane", "process-info", "--pane", "1-1"],
                "worktree list": ["worktree", "list", "--workspace", "w_1"],
                "pane read": ["pane", "read", "1-1", "--lines", "80", "--source", "recent", "--format", "text"]}
            require(set(canonical) == set(FAKE_READ_ONLY), "offline_fake_canonical_set")
            prompt = list(FAKE_PROMPT_ARGV)
            def log(*items):
                return b"".join(encode({"sequence": index, "argv": argv, "outcome": outcome,
                                        "environment": {"HERDR_SOCKET_PATH": "/private/x"}}) + b"\n"
                                for index, (argv, outcome) in enumerate(items, 1))
            def refused(data, complete=False):
                return rejects(lambda: fake_operation_summary(data, complete=complete))
            # Every enumerated read-only query is accepted for all three outcomes, live and complete.
            for argv in canonical.values():
                for outcome in ("started", "succeeded", "failed"):
                    for complete in (False, True):
                        rows = fake_operation_summary(log((argv, outcome), (prompt, "succeeded")), complete=complete)
                        require({"command": " ".join(argv[:2]), "outcome": outcome, "count": 1} in rows
                                and {"command": "agent prompt", "outcome": "succeeded", "count": 1} in rows,
                                "offline_fake_read_only_accepted")
            # The observed Linux-shaped run: repeated failed queries plus the one prompt, complete.
            observed = log(*([(canonical[name], "failed") for name in (
                "integration status", "pane process-info", "worktree list", "agent get")] * 3 + [(prompt, "succeeded")]))
            require(fake_operation_summary(observed, complete=True) == [
                {"command": "agent get", "outcome": "failed", "count": 3},
                {"command": "agent prompt", "outcome": "succeeded", "count": 1},
                {"command": "integration status", "outcome": "failed", "count": 3},
                {"command": "pane process-info", "outcome": "failed", "count": 3},
                {"command": "worktree list", "outcome": "failed", "count": 3}], "offline_fake_observed_shape")
            # The single harmless prompt: started live, never failed, once, exact argv, succeeded when complete.
            require(fake_operation_summary(log((prompt, "started"))) == [
                {"command": "agent prompt", "outcome": "started", "count": 1}], "offline_fake_prompt_live")
            require(refused(log((prompt, "started")), True) and refused(log((prompt, "failed")))
                    and refused(log((prompt, "succeeded"), (prompt, "succeeded")))
                    and refused(log((prompt[:3] + ["other text"], "succeeded")))
                    and refused(log((prompt[:2], "succeeded"))), "offline_fake_prompt_refused")
            # Every mutating Herdr CLI command (and anything unknown) is refused for every outcome.
            mutating = (["workspace", "create", "--cwd", "/x", "--label", "l", "--no-focus"],
                ["workspace", "rename", "w", "l"], ["tab", "create", "--workspace", "w", "--cwd", "/x", "--label", "l", "--no-focus"],
                ["tab", "rename", "t", "l"], ["pane", "run", "p", "cmd"], ["pane", "send-keys", "p", "Enter"],
                ["pane", "send-text", "p", "t"], ["pane", "close", "p"], ["agent", "rename", "p", "n"],
                ["agent", "start", "n", "--kind", "k", "--pane", "p", "--timeout", "1"],
                ["worktree", "create", "--workspace", "w", "--branch", "b", "--no-focus"],
                ["worktree", "open", "--workspace", "w", "--no-focus"], ["worktree", "remove", "--workspace", "w"],
                ["integration", "install"], ["server", "stop"], ["pane", "unknown"])
            for argv in mutating:
                for outcome in ("started", "succeeded", "failed", "other"):
                    require(refused(log((argv, outcome))) and refused(log((argv, outcome)), True)
                            and refused(log((canonical["pane list"], "succeeded"), (argv, outcome))),
                            "offline_fake_mutation_refused")
            # Exact argv shape: no wildcard, extra/missing/option-like operands refuse.
            shaped = [canonical["agent get"] + ["extra"], canonical["agent list"] + ["--all"],
                canonical["integration status"] + ["--json"], ["agent", "get"], ["agent", "get", ""], ["agent", "get", "--pane"],
                ["pane", "process-info", "1-1"], ["pane", "process-info", "--workspace", "1-1"],
                ["pane", "process-info", "--pane", ""], ["pane", "process-info", "--pane", "-x"],
                ["worktree", "list", "--pane", "w"], ["worktree", "list", "--workspace", "w", "--no-focus"],
                canonical["pane read"][:-2], ["pane", "read", "1-1", "--source", "recent", "--lines", "80", "--format", "text"]]
            require(all(refused(log((argv, "succeeded"))) for argv in shaped), "offline_fake_shape_refused")
            require(refused(log((["--version"], "succeeded"))) and refused(log((canonical["agent list"], "bogus")))
                    and refused(log((canonical["agent list"], None)))
                    and refused(log((canonical["agent list"], "succeeded")) + log((canonical["pane list"], "succeeded")))
                    and refused(b"{") and refused(log(([], "succeeded"))) and refused(log(([1, 2], "succeeded"))),
                    "offline_fake_malformed_refused")
            # Source-bound: every CLI command client.go can issue is classified; nothing is unaccounted for.
            client = (Path(__file__).resolve().parents[1] / "internal/herdr/client.go").read_text()
            issued = {" ".join(pair) for pair in re.findall(
                r'(?:runResult\(ctx, &result,|runCommand\(ctx,|args := \[\]string\{|args = append\(args,)\s*"([a-z-]+)",\s*"([a-z-]+)"',
                client)}
            known_mutating = {" ".join(argv[:2]) for argv in mutating[:13]}
            require(issued >= {"integration status", "pane process-info", "worktree list", "agent get", "pane read",
                                "agent prompt"} and issued <= FAKE_COMMANDS | known_mutating
                    and issued - FAKE_COMMANDS == known_mutating
                    and FAKE_COMMANDS - issued <= {"agent list", "pane list", "workspace list", "tab list"},
                    "offline_fake_source_classification")
            # FAIL diagnostics: bucketed, secret-free, never raising; unreadable evidence leaves no rows.
            marker = "offline_secret_marker"
            noisy = (log((["worktree", "create", "--workspace", marker], "failed"),
                         (canonical["agent get"], "failed"), (canonical["agent get"], "weird"), ([marker, marker], "succeeded"))
                     + b"{\n" + b"\n".join(encode(item) for item in ([1], {"argv": "x"}, {"argv": [marker]})) + b"\n")
            diagnostic = fake_operation_diagnostics(noisy.decode())
            require(diagnostic == [{"command": "agent get", "outcome": "failed", "count": 1},
                                   {"command": "agent get", "outcome": "other", "count": 1},
                                   {"command": "other", "outcome": "failed", "count": 1},
                                   {"command": "other", "outcome": "other", "count": 2},
                                   {"command": "other", "outcome": "succeeded", "count": 1}]
                    and marker not in json.dumps(diagnostic) and fake_operation_diagnostics("") == [],
                    "offline_fake_diagnostics")
            require(fake_rows_policy(diagnostic, complete=False) and not fake_rows_policy(diagnostic, complete=True),
                    "offline_fake_diagnostic_policy")
            with tempfile.TemporaryDirectory() as directory:
                evidence = Path(directory) / "fake-operations.jsonl"
                require(fake_failure_rows(evidence) == [], "offline_fake_failure_rows_absent")
                evidence.write_bytes(noisy)
                require(fake_failure_rows(evidence) == diagnostic, "offline_fake_failure_rows")
                link = Path(directory) / "link.jsonl"
                link.symlink_to(evidence)
                require(fake_failure_rows(link) == [], "offline_fake_failure_rows_symlink")
            # Receipts: PASS admits all enumerated read-only rows, FAIL admits diagnostics incl. "other";
            # neither admits duplicates, prompt anomalies, unknown labels or an oversize row set.
            every = sorted([{"command": name, "outcome": outcome, "count": 2} for name in canonical
                            for outcome in ("started", "succeeded", "failed")]
                           + [{"command": "agent prompt", "outcome": "succeeded", "count": 1}],
                           key=lambda row: (row["command"], row["outcome"]))
            def fake_receipt(rows, **changes):
                value = {**valid_receipt, **changes}
                value["fixture_operations"] = {**valid_receipt["fixture_operations"], "fake": rows}
                return encode(value)
            def fail(rows):
                return fake_receipt(rows, result="fail", failure="owned_operation_allowlist", failure_guard="fake_herdr")
            prompt_row = {"command": "agent prompt", "outcome": "succeeded", "count": 1}
            require(receipt_policy(fake_receipt(every), BASE, sources, producer_id)
                    and receipt_policy(fail(diagnostic), BASE, sources, producer_id) is False
                    and receipt_policy(fail([]), BASE, sources, producer_id) is False
                    and receipt_policy(fail(every), BASE, sources, producer_id) is False,
                    "offline_fake_receipts_accepted")
            bad_pass = (
                [], [{"command": "agent get", "outcome": "failed", "count": 1}],
                [prompt_row, {**prompt_row, "outcome": "started"}], [{**prompt_row, "outcome": "failed"}],
                [{**prompt_row, "count": 2}], [prompt_row, dict(prompt_row)],
                [prompt_row, {"command": "other", "outcome": "failed", "count": 1}],
                [prompt_row, {"command": "agent get", "outcome": "other", "count": 1}],
                [prompt_row, {"command": "pane run", "outcome": "succeeded", "count": 1}],
                [prompt_row, {"command": "agent get", "outcome": "failed", "count": 0}],
                [prompt_row, {"command": "agent get", "outcome": "failed", "count": True}],
                [prompt_row, {"command": "agent get", "outcome": "failed", "count": 1, "argv": []}],
                [prompt_row, {"command": ["agent", "get"], "outcome": "failed", "count": 1}],
                [prompt_row, {"command": "agent get", "outcome": ["failed"], "count": 1}],
                [prompt_row, {"command": "agent get", "outcome": "failed", "count": 1},
                 {"command": "agent get", "outcome": "failed", "count": 2}], {"fake": []}, None)
            require(all(rejects(lambda rows=rows: receipt_policy(fake_receipt(rows), BASE, sources, producer_id))
                        for rows in bad_pass), "offline_fake_pass_receipt_refused")
            bad_fail = (
                [{"command": "pane run", "outcome": "failed", "count": 1}],
                [{"command": "other", "outcome": "bogus", "count": 1}],
                [{"command": "other", "outcome": "failed", "count": 1}, {"command": "other", "outcome": "failed", "count": 1}],
                [{"command": "other", "outcome": "failed", "count": 0}],
                [{"command": "other", "outcome": "failed", "count": 1, "argv": ["x"]}],
                [{"command": "other", "outcome": "failed"}],
                [{"command": name, "outcome": str(index), "count": 1} for index in range(FAKE_ROW_CAP + 1)
                 for name in ("other",)])
            require(all(rejects(lambda rows=rows: receipt_policy(fail(rows), BASE, sources, producer_id))
                        for rows in bad_fail), "offline_fake_fail_receipt_refused")
            # The live evidence file is atomically replaced by fake-herdr. One descriptor is an
            # immutable snapshot: a replacement between open and read, or any number of
            # concurrent replacements, never refuses a legitimate file.
            line = log((canonical["agent get"], "failed"))
            newer = log((canonical["agent get"], "failed"), (canonical["pane list"], "succeeded"))
            with tempfile.TemporaryDirectory() as directory:
                evidence = Path(directory) / "fake-operations.jsonl"
                def publish(data, name="staged"):
                    staged = Path(directory) / name
                    staged.write_bytes(data)
                    os.replace(staged, evidence)
                publish(line)
                require(read_fake_operations(evidence) == line and fake_operation_summary(
                    read_fake_operations(evidence)) == [{"command": "agent get", "outcome": "failed", "count": 1}],
                    "offline_fake_read_real_file")
                def replacing_opener(target, flags):
                    descriptor = os.open(target, flags)
                    for count in range(8):  # more replacements than any bounded retry could absorb
                        publish(newer, "staged" + str(count))
                    return descriptor
                require(read_fake_operations(evidence, replacing_opener) == line
                        and read_fake_operations(evidence) == newer, "offline_fake_read_snapshot_under_replacement")
                stop = threading.Event()
                def writer():
                    while not stop.is_set():
                        publish(line, "w1")
                        publish(newer, "w2")
                thread = threading.Thread(target=writer, daemon=True)
                thread.start()
                try:
                    require(all(len(fake_operation_summary(read_fake_operations(evidence), complete=False)) in (1, 2)
                                for _ in range(300)), "offline_fake_read_concurrent_replacement")
                finally:
                    stop.set()
                    thread.join(5)
                require(not thread.is_alive(), "offline_fake_read_writer_stopped")
                link = Path(directory) / "link.jsonl"
                link.symlink_to(evidence)
                hard = Path(directory) / "hard.jsonl"
                os.link(evidence, hard)
                folder = Path(directory) / "folder"
                folder.mkdir()
                big = Path(directory) / "big.jsonl"
                big.write_bytes(b"x" * (FAKE_OPERATIONS_CAP + 1))
                require(all(rejects(lambda target=target: read_fake_operations(target))
                            for target in (link, hard, folder, big, Path(directory) / "absent.jsonl")),
                        "offline_fake_read_refusals")
            return True
        operations = {
            "valid_result": lambda: browser_result(encode(valid), "enroll", 0)["ok"]
                and receipt_policy(encode(valid_receipt), BASE, sources, producer_id)
                and valid_receipt["socket_diagnostics"]["publication_exceptions"] == {}
                and request_receipts() and proxy_failure_receipts() and fake_operation_cases()
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
                and publication_cases() and socket_pass_refusals(),
            "invalid_producer": lambda: rejects(lambda: producer(BASE, 1, 1, "0" * 64, False, BASE, 1)),
            "invalid_budget_or_paths": lambda: rejects(lambda: paths_admission(Path("relative"), Path("receipt"), Path("/tmp"), 1260))
                and dev_link_origin_contract(),
            "sensitive_receipt_fields": lambda: rejects(lambda: browser_result(encode({**valid, "secret": "x"}), "enroll", 0))
                and receipt_reject(("tools", "secret"), "offline_marker")
                and receipt_reject(("tools", "curl"), "https://invalid/#setup=offline_marker")
                and sensitive_failures() and socket_privacy(),
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
            require(budget.remaining() > 0, "offline_within_limits")
            scenario = helper.socket_scenario_view({"panes": [{"pane_id": "fixture:pane"}],
                "tabs": [{"tab_id": "fixture-tab"}], "workspaces": [{"workspace_id": "fixture-workspace"}],
                "content": {"fixture:pane": "Harmless fixture output"}})
            # Literal pre-change result shapes and insertion order, not a second
            # call to the new result implementation used as its own oracle.
            expected = {
                "ping": {"type": "pong", "version": "0.9.0", "protocol": 1,
                    "capabilities": {"endpoint_protocol_generation": 1}},
                "agent.list": {"type": "agent_list", "agents": scenario["panes"]},
                "pane.list": {"type": "pane_list", "panes": scenario["panes"]},
                "workspace.list": {"type": "workspace_list", "workspaces": scenario["workspaces"]},
                "tab.list": {"type": "tab_list", "tabs": scenario["tabs"]},
                "session.snapshot": {"type": "session_snapshot", "snapshot": {"version": "0.9.0", "protocol": 1,
                    "workspaces": scenario["workspaces"], "tabs": scenario["tabs"], "panes": scenario["panes"], "agents": scenario["panes"]}},
                "events.subscribe": {"type": "subscription_started"},
                "pane.read": {"type": "pane_read", "read": {"text": "Harmless fixture output", "truncated": False}}}
            compact = lambda value: json.dumps(value, separators=(",", ":")).encode()
            counters = helper.SocketCounters()
            for method, result in expected.items():
                request = {"id": "mobile-relay-api-1", "method": method, "params": {"pane_id": "fixture:pane"}}
                response, label, outcome, reason = helper.dispatch_socket_request(compact(request), scenario)
                require(compact(response) == compact({"id": request["id"], "result": result})
                        and (label, outcome, reason) == (method, "succeeded", "ordinary"), "offline_ordinary_bytes")
                require(compact(helper.HerdrSocketFixture._result(SimpleNamespace(scenario_view=scenario), method, request))
                        == compact(result), "offline_result_delegation")
                counters.record(label, outcome, reason)
            request = {"id": "package-fixture-readiness", "method": "pane.read", "params": {"pane_id": "nonempty-unknown"}}
            response, label, outcome, reason = helper.dispatch_socket_request(compact(request), scenario)
            require(compact(response) == compact({"id": request["id"], "result": {"type": "pane_read",
                "read": {"text": "Harmless package fixture output", "truncated": False}}})
                and (label, outcome, reason) == ("pane.read", "succeeded", "ordinary"), "offline_nonempty_pane_bytes")
            rows = counters.operation_summary()
            require(rows == [{"method": method, "outcome": "succeeded", "count": 1} for method in sorted(SOCKET_ORDINARY)]
                    and all(set(row) == {"method", "outcome", "count"} for row in rows), "offline_linux_rows")
            return True
        def immutable():
            try:
                LIMITS["stdin"] = 999999
            except TypeError:
                require(LIMITS["stdin"] == 65536 and PHASES == (90, 300, 210, 630, 30), "offline_limits_immutable")
            else:
                return False
            expected = (
                ("workspace.move_block", (("workspace_ids", "empty_list"),), "workspace_move_block_failed", "empty selection"),
                ("tab.move", (("insert_index", "int:0"), ("tab_id", "empty_str")), "tab_not_found", "empty tab"),
                ("pane.read", (("format", "str:ansi"), ("lines", "int:1"), ("pane_id", "empty_str"),
                    ("source", "str:visible"), ("strip_ansi", "false")), "pane_not_found", "empty pane"))
            require(type(helper.INERT_PROBES) is tuple and len(helper.INERT_PROBES) == 3
                    and helper.INERT_PROBES == expected and helper.InertProbe._fields == ("method", "params", "code", "message")
                    and helper.PROBE_MATCHERS == {"empty_list", "empty_str", "int:0", "int:1", "false", "str:ansi", "str:visible"}
                    and helper.PROBE_CAP == PROBE_CAP == 64 and helper.COUNT_MAX == COUNT_MAX == 65535
                    and helper.SOCKET_PAIRS == SOCKET_PAIRS and len(SOCKET_PAIRS) == 26
                    and helper.SOCKET_REASONS == SOCKET_REASONS and len(SOCKET_REASONS) == 14
                    and helper.SOCKET_EXCEPTION_CATEGORIES == SOCKET_EXCEPTION_CATEGORIES, "offline_probe_contract")
            require(helper.SOCKET_EXCEPTION_BUCKETS == SOCKET_EXCEPTION_BUCKETS == (
                    "handler_exceptions", "stream_exceptions", "listener_exceptions", "publication_exceptions")
                    and SOCKET_EXCEPTION_CATEGORIES == {"timeout", "connection_reset", "broken_pipe", "os_error", "internal"},
                    "offline_publication_closed_contract")
            for error_type, category in ((TimeoutError, "timeout"), (ConnectionResetError, "connection_reset"),
                    (BrokenPipeError, "broken_pipe"), (OSError, "os_error"), (RuntimeError, "internal")):
                counters = helper.SocketCounters()
                error = error_type("/private/inert-token-never-recorded")
                for _ in range(COUNT_MAX + 2):
                    counters.record_exception("publication_exceptions", error)
                state = SimpleNamespace(counters=counters, thread=SimpleNamespace(ident=1, is_alive=lambda: True),
                    stopped=SimpleNamespace(is_set=lambda: False))
                diagnostics = helper.socket_diagnostic_snapshot(state)
                diagnostics.update(closed_after_cleanup="closed", guard=socket_diagnostic_guard(diagnostics))
                require(counters.publication_exceptions == {category: COUNT_MAX} and counters.saturated is True
                        and counters.counts == counters.reasons == {} and counters.handled_connections == 0
                        and diagnostics["guard"] == "counter_saturated"
                        and socket_diagnostics_policy(diagnostics)
                        and rejects(lambda: socket_diagnostics_policy(diagnostics, passing=True, operation_rows=[])),
                        "offline_publication_acquisition_saturation")
                require(b"/private" not in encode(diagnostics) and b"inert-token" not in encode(diagnostics),
                        "offline_saturated_publication_privacy")
            for bucket in ("unknown", "handler_exceptions", "publication_exceptions_unknown"):
                try:
                    helper.SocketCounters().record_exception(bucket, RuntimeError("inert invalid bucket"))
                except ValueError:
                    pass
                else:
                    return False
            values = {"empty_list": [], "empty_str": "", "int:0": 0, "int:1": 1,
                      "false": False, "str:ansi": "ansi", "str:visible": "visible"}
            scenario = helper.socket_scenario_view({})
            for probe in helper.INERT_PROBES:
                require(type(probe) is helper.InertProbe and type(probe.params) is tuple
                        and all(type(pair) is tuple and len(pair) == 2 and pair[1] in helper.PROBE_MATCHERS for pair in probe.params),
                        "offline_nested_probe_tuple")
                attempts = (lambda: setattr(probe, "method", "unsafe"), lambda: setattr(probe, "params", ()),
                    lambda: probe.params[0].__setitem__(0, "unsafe"), lambda: helper.INERT_PROBES.__setitem__(0, probe))
                for attempt in attempts:
                    try:
                        attempt()
                    except (AttributeError, TypeError):
                        pass
                    else:
                        return False
                request = encode({"id": "mobile-relay-api-1", "method": probe.method,
                                  "params": {key: values[tag] for key, tag in probe.params}})
                first = helper.dispatch_socket_request(request, scenario)[0]
                first["id"], first["error"]["code"] = "changed", "changed"
                second = helper.dispatch_socket_request(request, scenario)[0]
                require(second == {"id": "mobile-relay-api-1", "error": {"code": probe.code, "message": probe.message}}
                        and second is not first and second["error"] is not first["error"], "offline_fresh_probe_response")
            for bad in ([], {"panes": {}}, {"tabs": None}, {"workspaces": "invalid"}, {"content": {"pane": 1}}, {"content": {1: "text"}}):
                try:
                    helper.socket_scenario_view(bad)
                except ValueError:
                    pass
                else:
                    return False
            return True
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
                "checked_out_sha": "c" * 40, "candidate_parents": [LAUNCHCTL_PINNED_PARENT],
                "scope_base_precedes_before": True, "before_precedes_candidate": True, "pushed_range_linear": True}
            require(admission_policy(valid_admission), "offline_admission_positive")
            # UNEXECUTED: exercise the production predicate with the accepted
            # Go generator's resource/suffix pairs, not a permissive proxy stub.
            entry = "/builds/1.2.3-1-" + "a" * 16 + "/index.html"
            javascript = "/assets/app-" + "b" * 64 + ".js"
            stylesheet = "/assets/app-" + "c" * 64 + ".css"
            verifier_paths = {"/release.json", "/version.json", entry, javascript, stylesheet}
            cache_bust = "herdr_deploy_check=" + BASE + "-1791800000000000000-0"
            verifier_requests = [("/release.json", ""), (entry, "-entry"), (entry, "-br-entry"),
                (javascript, "-javascript"), (javascript, "-br-javascript"),
                (stylesheet, "-stylesheet"), (stylesheet, "-br-stylesheet"),
                ("/version.json", "-version"), ("/version.json", "-version-br")]
            for path, suffix in verifier_requests:
                require(public_request_allowed(path + "?" + cache_bust + suffix, "127.0.0.1:8443", "",
                                               verifier_paths, 8443), "offline_verifier_query_positive")
            for nonce, attempt in (("0", "0"), (str(-(2**63)), str(2**63 - 1)), (str(2**63 - 1), "1")):
                require(public_request_allowed("/release.json?herdr_deploy_check=" + BASE + "-" + nonce + "-" + attempt,
                        "127.0.0.1:8443", "", verifier_paths, 8443), "offline_verifier_integer_bounds")
            for query in (cache_bust + "&" + cache_bust, cache_bust + "&v=1", "v=1&" + cache_bust,
                          cache_bust + "-other", cache_bust + "-entry", cache_bust + "&arbitrary=1",
                          cache_bust.replace("=", "=%61", 1), cache_bust.replace("-0", "-00"),
                          "herdr_deploy_check=" + BASE + "-0--1", "herdr_deploy_check=" + BASE.upper() + "-0-0",
                          "herdr_deploy_check=" + BASE + "-" + str(2**63) + "-0",
                          "herdr_deploy_check=" + BASE + "-" + str(-(2**63) - 1) + "-0",
                          "herdr_deploy_check=" + BASE + "-0-" + str(2**63), cache_bust + "0" * 129,
                          cache_bust + "\n", cache_bust + "%26v=1", cache_bust.replace("check", "%63heck")):
                require(not public_request_allowed("/release.json?" + query, "127.0.0.1:8443", "",
                            verifier_paths, 8443), "offline_verifier_query_refusal")
            raw = "/release.json?" + cache_bust
            for request, host, upgrade, paths, port in ((raw, "foreign.invalid", "", verifier_paths, 8443),
                    (raw, "127.0.0.1:18444", "", verifier_paths, 8443),
                    (raw, "127.0.0.1:443", "", verifier_paths, 443),
                    (raw, "127.0.0.1:8443", "websocket", verifier_paths, 8443),
                    (raw, "127.0.0.1:8443", "", set(), 8443),
                    (raw + "#fragment", "127.0.0.1:8443", "", verifier_paths, 8443),
                    ("https://127.0.0.1:8443" + raw, "127.0.0.1:8443", "", verifier_paths, 8443),
                    ("/healthz?" + cache_bust, "127.0.0.1:8443", "", {"/healthz"}, 8443),
                    (javascript + "?" + cache_bust + "-stylesheet", "127.0.0.1:8443", "", verifier_paths, 8443)):
                require(not public_request_allowed(request, host, upgrade, paths, port), "offline_verifier_authority_refusal")
            require(public_request_allowed("/healthz?v=legacy._-1", "127.0.0.1:8443", "", {"/healthz"}, 8443)
                    and not public_request_allowed("/healthz?v=" + "a" * 129, "127.0.0.1:8443", "", {"/healthz"}, 8443),
                    "offline_existing_version_query")
            # Iteration-4 blocker: the shipped app itself issues /version.json?check=<ms> on every
            # start (checkAppUpdate) and /index.html?herdr_reload=<key>-<ms> on reload/recovery, which
            # the relay 307-redirects to the build entry KEEPING the query. Admit exactly those.
            app_paths = {"/", "/index.html", "/healthz", "/release.json", "/version.json", entry, javascript,
                         stylesheet, "/sw.js", "/manifest.webmanifest", "/herdr-bootstrap.js"}
            admitted = ["/version.json?check=" + stamp for stamp in ("0", "1791800000000", "9" * 16)]
            admitted += [path + "?herdr_reload=" + key + "-" + stamp
                         for path in ("/", "/index.html", entry)
                         for key in ("current", "recovery", "0.22.4", "0.22.4-rc.1", "a" * 64)
                         for stamp in ("1", "1791800000000", "9" * 16)]
            for request in admitted:
                require(public_request_allowed(request, "127.0.0.1:8443", "", app_paths, 8443),
                        "offline_app_query_positive")
            require(not public_request_allowed("/version.json?check=1", "127.0.0.1:8443", "", {"/healthz"}, 8443)
                    and not public_request_allowed("/version.json?check=1", "127.0.0.1:18444", "", app_paths, 8443)
                    and not public_request_allowed("/version.json?check=1", "127.0.0.1:443", "", app_paths, 443),
                    "offline_app_query_needs_owned_path_and_port")
            for request in ("/version.json?check=", "/version.json?check=abc", "/version.json?check=-1",
                            "/version.json?check=1.5", "/version.json?check=" + "1" * 17,
                            "/version.json?check=1&check=2", "/version.json?check=1&v=1", "/version.json?v=1&check=1",
                            "/version.json?check=1&arbitrary=1", "/version.json?check=%31", "/version.json?check=1\n",
                            "/version.json?CHECK=1", "/version.json?check=1#fragment", "/version.json?check==1",
                            "//version.json?check=1", "/version.json?herdr_reload=current-1",
                            "/version.json?" + cache_bust.replace("-0", "-0-version") + "&check=1",
                            "/healthz?check=1", "/release.json?check=1", "/sw.js?check=1", "/?check=1",
                            "/index.html?check=1", entry + "?check=1", javascript + "?check=1",
                            "/index.html?herdr_reload=", "/index.html?herdr_reload=current",
                            "/index.html?herdr_reload=current-", "/index.html?herdr_reload=-1",
                            "/index.html?herdr_reload=current-abc", "/index.html?herdr_reload=current-" + "1" * 17,
                            "/index.html?herdr_reload=" + "a" * 65 + "-1", "/index.html?herdr_reload=a/b-1",
                            "/index.html?herdr_reload=a%2Fb-1", "/index.html?herdr_reload=current-1&v=1",
                            "/index.html?herdr_reload=current-1&herdr_reload=current-2",
                            "/index.html?herdr_reload=current-1&other=1", "/index.html?herdr_reload=current-1\n",
                            "/index.html?herdr_reload=current-1#fragment", "/index.html?herdr_reload=current-1&check=1",
                            "/healthz?herdr_reload=current-1", "/version.json?herdr_reload=current-1",
                            javascript + "?herdr_reload=current-1", "/builds/../index.html?herdr_reload=current-1",
                            "/private?herdr_reload=current-1", "/manifest.webmanifest?herdr_reload=current-1"):
                require(not public_request_allowed(request, "127.0.0.1:8443", "", app_paths, 8443),
                        "offline_app_query_refusal")
            # Neither form may ride a WebSocket upgrade (bare wss origin, no query) or a foreign host.
            for request in ("/?herdr_reload=current-1", "/version.json?check=1", "/index.html?herdr_reload=current-1"):
                require(not public_request_allowed(request, "127.0.0.1:8443", "websocket", app_paths, 8443)
                        and not public_request_allowed(request, "foreign.invalid", "", app_paths, 8443),
                        "offline_app_query_upgrade_host_refusal")
            require(public_request_allowed("/", "127.0.0.1:8443", "websocket", app_paths, 8443)
                    and not public_request_allowed("/index.html", "127.0.0.1:8443", "websocket", app_paths, 8443)
                    and not public_request_allowed("/anything", "127.0.0.1:8443", "", app_paths, 8443)
                    and not public_request_allowed("/spa-route", "127.0.0.1:8443", "", app_paths, 8443),
                    "offline_app_surface_still_exact")
            require(socket_operations_allowed({("ping", "succeeded"): 1})
                    and proxy_operations_allowed([SimpleNamespace(failure=False)])
                    and child_environment_allowed({"HOME": "/owned/home"})
                    and service_inputs_allowed(False, False, "/bin/launchctl")
                    and fixture_port_allowed(8443)
                    and public_request_allowed("/", "127.0.0.1:8443", "websocket", {"/"}, 8443)
                    and public_request_allowed("/healthz", "127.0.0.1:8443", "", {"/healthz"}, 8443),
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
                           lambda: public_request_allowed("/", "foreign.invalid", "websocket", {"/"}, 8443),
                           lambda: public_request_allowed("https://foreign.invalid/", "127.0.0.1:8443", "", {"/"}, 8443),
                           lambda: public_request_allowed("/private", "127.0.0.1:8443", "", {"/"}, 8443),
                           lambda: public_request_allowed("/?secret=value", "127.0.0.1:8443", "", {"/"}, 8443),
                           lambda: public_request_allowed("/healthz", "127.0.0.1:8443", "websocket", {"/healthz"}, 8443)])
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
            # The three history facts are what git would report for this observation.
            identity = {"checked_out_sha": base["candidate_sha"], "candidate_parents": [LAUNCHCTL_PINNED_PARENT],
                "scope_base_precedes_before": True, "before_precedes_candidate": True, "pushed_range_linear": True}
            reads, queries, query_befores = [], [], []
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
                def query_candidate(selected, before):
                    require(selected == Path("/inert-candidate"), "offline_candidate_query")
                    queries.append(True)
                    query_befores.append(before)
                    return commit
                return first_push_observation(observation, path, Path("/inert-candidate"),
                    read_event=read_event, query_candidate=query_candidate)
            context = observe()
            packet = {"grantId": LAUNCHCTL_GRANT_ID, "authorized": True,
                "pinnedParent": LAUNCHCTL_PINNED_PARENT, "scope": dict(LAUNCHCTL_SCOPE)}
            valid_packet = encode(packet).decode()
            require(launchctl_authorized(valid_packet, context) and len(reads) == len(queries) == 1
                    and query_befores == [event["before"]],
                    "offline_launchctl_first_push_positive")
            # Bind the workflow's explicit packet to the same exact frozen values.
            workflow = read_bounded(Path(__file__).resolve().parents[1] / FILES[0], 65536).decode()
            workflow_packets = re.findall(r"--launchctl-query-grant '([^']+)'", workflow)
            require(len(workflow_packets) == 1 and workflow.count("--launchctl-query-grant") == 1
                    and strict_json(workflow_packets[0].encode()) == packet,
                    "offline_workflow_grant_binding")
            # The literal actually projected by the YAML itself (not only the
            # reconstructed packet) passes the production parser and admission
            # predicate with the valid synthetic first-push context.
            require(launchctl_packet_allowed(workflow_packets[0])
                    and launchctl_authorized(workflow_packets[0], context),
                    "offline_workflow_grant_accepted")
            # Source-bind the shared scope predicate and the offline-control wiring.
            # The workflow must not carry the old single-commit pins (not even in
            # comments), must call this module's predicate from the producer step
            # (one allowlist, none duplicated in YAML), and must run every offline
            # group and the qualification gate's own self-test on the hosted runner.
            require("cbacea6e" not in workflow and "2230f96f" not in workflow
                    and "module.candidate_scope_allowed(" in workflow and "module.SCOPE_BASE" in workflow
                    and "ALLOWED" not in workflow and "expected = {" not in workflow
                    and all(("tests/test_tailscale_darwin_package_browser.py --offline-self-check " + group) in workflow
                            for group in OFFLINE)
                    and "scripts/qualify-darwin-package-browser.py --self-test" in workflow
                    and workflow.index("--offline-self-check parser") < workflow.index("--fresh-producer"),
                    "offline_workflow_scope_and_controls_binding")
            # The Python runtime is pinned in BOTH jobs, before the first python3 invocation of each
            # job, to the exact value the receipt and the qualify gate require.
            pins = [m.start() for m in re.finditer(r"uses: actions/setup-python@v6\n(?:        timeout-minutes: [0-9]+\n)?"
                                                   r"        with:\n          python-version: \"" + re.escape(PYTHON_VERSION) + r"\"\n",
                                                   workflow)]
            require(workflow.count("actions/setup-python@") == 2 and len(pins) == 2
                    and pins[0] < workflow.index("python3 -I -B -")
                    and pins[1] < workflow.index("--offline-self-check parser")
                    and re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", PYTHON_VERSION) is not None,
                    "offline_workflow_python_pin")
            # The complete spent 2230f96f, e59e8022, 3c8f0c8d, 8c1f22ff and dd608845 packets,
            # including their original scope.when, are refused at packet level. At event
            # level a spent parent as `before` is refused because git reports that
            # SCOPE_BASE does not precede it (every spent parent is older than
            # SCOPE_BASE), which these cases inject exactly as git would. A candidate
            # whose sole parent is a spent parent cannot descend from `before`.
            spent_grants = (
                ("q2-owner-launchctl-print-push-2230f96f-20261013", "2230f96fb61e1740c21154935a25c235bab71cf3"),
                ("q2-parent-launchctl-print-first-push-e59e8022-20261008", "e59e8022daaefed14317d760f17f904ad6cd55eb"),
                ("q2-owner-launchctl-print-push-3c8f0c8d-20261012", "3c8f0c8db54e9e147bd0034bab83051fe186324e"),
                ("q2-owner-launchctl-print-push-8c1f22ff-20261013", "8c1f22ff4980335ce0d1acc1731429a3457067a9"),
                ("q2-owner-launchctl-print-push-dd608845-20261013", "dd608845b73c0323f5cbcfa140ad52affb28ec7f"))
            for spent_grant, spent_parent in spent_grants:
                spent_packet = encode({"grantId": spent_grant, "authorized": True, "pinnedParent": spent_parent,
                    "scope": {**LAUNCHCTL_SCOPE, "when": "attempt 1 of the first fast-forward push from " + spent_parent}}).decode()
                spent_event = {**event, "before": spent_parent}
                spent_before_identity = {**identity, "scope_base_precedes_before": False}
                spent_parent_identity = {**identity, "candidate_parents": [spent_parent], "before_precedes_candidate": False}
                spent_before = {**context, "event_before": spent_parent, "scope_base_precedes_before": False}
                spent_parent_context = {**context, "candidate_parents": [spent_parent], "before_precedes_candidate": False}
                before = (len(reads), len(queries))
                require(LAUNCHCTL_GRANT_ID != spent_grant and LAUNCHCTL_PINNED_PARENT != spent_parent
                        and not launchctl_packet_allowed(spent_packet)
                        and not launchctl_authorized(spent_packet, context)
                        and rejects(lambda: observe(data=encode(spent_event), commit=spent_before_identity))
                        and rejects(lambda: observe(commit=spent_parent_identity))
                        and not admission_policy(spent_before)
                        and not admission_policy(spent_parent_context)
                        and not launchctl_authorized(workflow_packets[0], spent_before)
                        and not launchctl_authorized(workflow_packets[0], spent_parent_context)
                        and (len(reads), len(queries)) == (before[0] + 2, before[1] + 2),
                        "offline_spent_" + spent_parent[:3] + "_grant_refused")
            for bad_base in ({**base, "attempt": "2"}, *({**base, "event_name": name}
                    for name in ("workflow_dispatch", "pull_request", "schedule")),
                    {**base, "candidate_sha": "0" * 40}, {**base, "candidate_sha": "C" * 40},
                    {**base, "candidate_sha": "c" * 39}, {**base, "run_id": "0"}, {**base, "run_id": "1" * 16}):
                before = (len(reads), len(queries))
                require(rejects(lambda: observe(observation=bad_base)) and before == (len(reads), len(queries)),
                        "offline_ci_refused_before_acquisition")
            missing_before = dict(event)
            del missing_before["before"]
            # `before` malformed in any way, all-zero (a created ref), equal to `after`
            # (nothing pushed), or option-like text is refused BEFORE any git argv exists.
            bad_events = [missing_before, *({**event, "before": before} for before in
                (None, 0, "0" * 40, LAUNCHCTL_PINNED_PARENT.upper(), LAUNCHCTL_PINNED_PARENT[:-1],
                 LAUNCHCTL_PINNED_PARENT + "0", "g" * 40, "-" + "a" * 39, "--upload-pack=x" + "a" * 25,
                 " " + "a" * 39, "a" * 39 + "\n", event["after"], True, [LAUNCHCTL_PINNED_PARENT])),
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
            # INVERTED by design (the one control whose verdict changed): a second
            # fast-forward whose before is the first candidate and descends from the
            # scope base is now ADMITTED, because the CI loop pushes more commits. It is
            # paired with refusals for every way a later push can be unsafe.
            later_base = {**base, "candidate_sha": "d" * 40}
            later_event = {**event, "before": base["candidate_sha"], "after": later_base["candidate_sha"]}
            later_identity = {"checked_out_sha": later_base["candidate_sha"],
                "candidate_parents": [base["candidate_sha"]],
                "scope_base_precedes_before": True, "before_precedes_candidate": True, "pushed_range_linear": True}
            later_context = observe(observation=later_base, data=encode(later_event), commit=later_identity)
            require(admission_policy(later_context) and launchctl_authorized(valid_packet, later_context)
                    and launchctl_authorized(workflow_packets[0], later_context)
                    and later_context["event_before"] == base["candidate_sha"] != LAUNCHCTL_PINNED_PARENT
                    and query_befores[-1] == base["candidate_sha"], "offline_second_push_admitted")
            # A multi-commit push is also a fast-forward: the sole parent need not be `before`.
            require(admission_policy(observe(commit={**identity, "candidate_parents": ["b" * 40]})),
                    "offline_multi_commit_push_admitted")
            # Paired refusals: before does not descend from the scope base (reset/force onto
            # an unrelated or older history), before is not an ancestor of the candidate
            # (a non-fast-forward), before == candidate (nothing pushed), a merge candidate.
            require(rejects(lambda: observe(observation=later_base, data=encode(later_event),
                        commit={**later_identity, "scope_base_precedes_before": False}))
                    and rejects(lambda: observe(observation=later_base, data=encode(later_event),
                        commit={**later_identity, "before_precedes_candidate": False}))
                    and rejects(lambda: observe(observation=later_base, data=encode({**later_event, "before": later_base["candidate_sha"]}),
                        commit=later_identity))
                    and rejects(lambda: observe(observation=later_base, data=encode(later_event),
                        commit={**later_identity, "candidate_parents": [base["candidate_sha"], "b" * 40]})),
                    "offline_later_push_refusals")
            # A merge commit ANYWHERE in the pushed range refuses even though the candidate tip
            # has exactly one parent (before -> merge -> single-parent tip): git reports the range
            # as not linear. This holds for the first push and for a later push.
            require(rejects(lambda: observe(commit={**identity, "pushed_range_linear": False}))
                    and rejects(lambda: observe(observation=later_base, data=encode(later_event),
                        commit={**later_identity, "pushed_range_linear": False}))
                    and not admission_policy({**context, "pushed_range_linear": False})
                    and not launchctl_authorized(valid_packet, {**context, "pushed_range_linear": False})
                    and not launchctl_authorized(workflow_packets[0], {**later_context, "pushed_range_linear": False}),
                    "offline_merge_in_pushed_range_refused")
            for commit in ({**identity, "candidate_parents": [LAUNCHCTL_PINNED_PARENT, "b" * 40]},
                    {**identity, "candidate_parents": []}, {**identity, "candidate_parents": LAUNCHCTL_PINNED_PARENT},
                    {**identity, "candidate_parents": ["not-a-sha"]}, {**identity, "candidate_parents": [LAUNCHCTL_PINNED_PARENT.upper()]},
                    {**identity, "candidate_parents": ["0" * 40]},
                    {**identity, "scope_base_precedes_before": False}, {**identity, "before_precedes_candidate": False},
                    {**identity, "scope_base_precedes_before": 1}, {**identity, "before_precedes_candidate": "unsafe"},
                    {**identity, "scope_base_precedes_before": None},
                    {**identity, "pushed_range_linear": False}, {**identity, "pushed_range_linear": None},
                    {**identity, "pushed_range_linear": 1}, {**identity, "pushed_range_linear": "true"},
                    {**identity, "checked_out_sha": "d" * 40}, {}):
                require(rejects(lambda: observe(commit=commit)), "offline_candidate_parent_refused")
            # Ancestry facts missing, or extra identity keys, never pass.
            for key in ("scope_base_precedes_before", "before_precedes_candidate", "pushed_range_linear"):
                require(rejects(lambda key=key: observe(commit={k: v for k, v in identity.items() if k != key})),
                        "offline_ancestry_fact_required")
            require(rejects(lambda: observe(commit={**identity, "extra": True})), "offline_identity_key_set")
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
                for parents in ([], ["b" * 39], [LAUNCHCTL_PINNED_PARENT, "b" * 40]))
            contexts.extend((valid_packet, {**context, key: value})
                for key in ("scope_base_precedes_before", "before_precedes_candidate", "pushed_range_linear")
                for value in (False, None, 1, 0, "true"))
            contexts.extend((valid_packet, {**context, "event_before": before})
                for before in ("0" * 40, context["event_after"], "b" * 39, LAUNCHCTL_PINNED_PARENT.upper()))
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
            # A different but individually admissible observation (a later push) is not
            # the admitted one: revalidation compares the whole context, not only policy.
            moved_gate = EffectGate(True, launchctl_grant=valid_packet, launchctl_context=context,
                launchctl_observe=lambda: observe(observation=later_base, data=encode(later_event), commit=later_identity))
            moved_mock = Operations(Path("/must-not-be-accessed"), {}, Budget(clock), Cleanup(), moved_gate)
            moved_launches, moved_guard = [], {"starts": 0}
            moved_mock.capture = lambda *args, **kwargs: moved_launches.append(True)
            require(rejects(lambda: moved_mock.capture_launcher(["must-not-execute"], moved_guard, cwd=None, link_origin=ORIGIN))
                    and not moved_launches and moved_guard["starts"] == moved_gate.launchctl_queries == 0,
                    "offline_revalidation_detects_changed_observation")
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

        def candidate_scope_cases():
            # Production predicate on injected lists only: no git, process or file I/O.
            gate = "scripts/qualify-darwin-package-browser.py"
            require(FILES == tuple(CANDIDATE_PATHS[:4]) and len(FILES) == 4 and gate not in FILES
                    and CANDIDATE_PATHS == (*FILES, gate) and QUALIFY_GATE == gate, "offline_candidate_paths_constants")
            # Constants sanity: REQUIRED is exactly the three gate files, REQUIRED <= ALLOWED,
            # no duplicates, and every entry is an explicit, clean repo-relative file path
            # (no glob, directory prefix, absolute path, '..', backslash or empty segment).
            require(REQUIRED == (FILES[0], FILES[1], gate) and type(ALLOWED) is tuple and type(REQUIRED) is tuple
                    and len(set(REQUIRED)) == len(REQUIRED) and len(set(ALLOWED)) == len(ALLOWED)
                    and set(REQUIRED) <= set(ALLOWED) and gate in ALLOWED
                    and all(type(path) is str and path and not path.startswith("/") and not path.endswith("/")
                            and "\\" not in path and "\n" not in path and not any(ch in path for ch in "*?[]{}")
                            and all(part not in ("", ".", "..") for part in path.split("/"))
                            for path in ALLOWED),
                    "offline_candidate_scope_constants")
            require(re.fullmatch(r"[0-9a-f]{40}", SCOPE_BASE) is not None and SCOPE_BASE != BASE
                    and LAUNCHCTL_PINNED_PARENT == SCOPE_BASE, "offline_scope_base_constant")
            minimal, full = list(REQUIRED), list(ALLOWED)
            # `outside` is a path that is NOT allowed. (README.md, for one, IS allowed:
            # it is an ordinary parked-WIP entry, so it is a valid optional extra.)
            outside = "outside-the-allowlist/unlisted.txt"
            require(outside not in ALLOWED and "extra.txt" not in ALLOWED and "unrelated.txt" not in ALLOWED
                    and "relay\\common.sh" not in ALLOWED, "offline_scope_outside_paths_not_allowed")
            optional = [path for path in ALLOWED if path not in REQUIRED][0]
            require(candidate_scope_allowed(minimal) and candidate_scope_allowed(list(reversed(minimal)))
                    and candidate_scope_allowed(full) and candidate_scope_allowed(list(reversed(full)))
                    and candidate_scope_allowed([*minimal, optional]) and candidate_scope_allowed([optional, *minimal]),
                    "offline_candidate_scope_accepts")
            invalid = [[], list(FILES), full + [outside], minimal + [outside], [outside, *full], full + [full[0]],
                minimal + [minimal[0]], [*minimal, optional, optional], ["unrelated.txt", *minimal[1:]],
                full + ["extra.txt"], full + ["../" + full[0]], full + ["/" + full[0]], full + [full[0] + "/"],
                full + [full[0].upper()], full + ["relay\\common.sh"], full + ["relay"], full + ["relay/"],
                full + ["tests/*.py"],
                tuple(full), set(full), frozenset(minimal), None, "".join(minimal), {"a": 1},
                [*minimal, 1], [*minimal, None], [*minimal, b"x"], [*full, ("a",)], [*minimal, True]]
            # Each REQUIRED path missing, one at a time, from the otherwise complete set.
            invalid.extend([path for path in full if path != required] for required in REQUIRED)
            invalid.extend([path for path in minimal if path != required] for required in REQUIRED)
            # An extra path outside ALLOWED next to every REQUIRED combination.
            invalid.extend([*minimal[:index], *minimal[index + 1:], "extra.txt"] for index in range(len(minimal)))
            # The two receipt-hashed runner files this gate does not change are NOT
            # allowed: a later commit that edits them must extend ALLOWED in that commit.
            require(FILES[2] not in ALLOWED and FILES[3] not in ALLOWED, "offline_unchanged_runner_files_not_allowed")
            invalid.append([*minimal, FILES[2]])
            require(all(not candidate_scope_allowed(item) for item in invalid), "offline_candidate_scope_refusals")
            return True

        def git_identity_cases():
            # git_candidate_identity with an injected fake runner: the exact argv, the
            # strict exit-code mapping, and refusal of timeouts, spawn errors and
            # over-long output. No real git, no process and no file I/O.
            head, parent, before = "c" * 40, "a" * 40, "b" * 40
            log_out = (head + "\n" + parent + "\n").encode()
            merge_out = ("d" * 40 + "\n").encode()
            candidate = Path("/inert-candidate")
            def fake(merge_statuses=(0, 0), *, log=(0, log_out), error=None, junk=b"", range_result=(0, b"")):
                statuses, calls = list(merge_statuses), []
                def runner(argv, **kwargs):
                    calls.append((list(argv), kwargs))
                    if error is not None:
                        raise error
                    if "log" in argv:
                        return SimpleNamespace(returncode=log[0], stdout=log[1])
                    if "rev-list" in argv:
                        return SimpleNamespace(returncode=range_result[0], stdout=range_result[1])
                    return SimpleNamespace(returncode=statuses.pop(0), stdout=junk)
                return runner, calls
            hardened = ["git", "--no-replace-objects", "--no-optional-locks", "-c", "core.fsmonitor=false",
                        "-c", "core.untrackedCache=false"]
            environment = {"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                           "GIT_OPTIONAL_LOCKS": "0"}
            runner, calls = fake()
            require(git_candidate_identity(candidate, before, runner) == {"checked_out_sha": head,
                    "candidate_parents": [parent], "scope_base_precedes_before": True,
                    "before_precedes_candidate": True, "pushed_range_linear": True}
                    and [argv for argv, _ in calls] == [
                        hardened + ["log", "-1", "--format=%H%n%P", "HEAD", "--"],
                        hardened + ["merge-base", "--is-ancestor", SCOPE_BASE, before],
                        hardened + ["merge-base", "--is-ancestor", before, "HEAD"],
                        hardened + ["rev-list", "--merges", "--max-count=1", before + "..HEAD", "--"]]
                    and all(kwargs == {"cwd": candidate, "env": environment, "stdout": subprocess.PIPE,
                                       "stderr": subprocess.DEVNULL, "timeout": 10, "check": False}
                            for _, kwargs in calls), "offline_git_identity_argv")
            # Exit 0 -> True, exit 1 -> False, for each ancestry query independently.
            for statuses, expected in (((0, 0), (True, True)), ((1, 0), (False, True)),
                                       ((0, 1), (True, False)), ((1, 1), (False, False))):
                runner, _ = fake(statuses)
                value = git_candidate_identity(candidate, before, runner)
                require((value["scope_base_precedes_before"], value["before_precedes_candidate"]) == expected
                        and value["pushed_range_linear"] is True, "offline_git_identity_exit_mapping")
            # The range query: empty output is a merge-free range; a printed merge commit is not
            # linear (reported as False, never raised); any status but 0 refuses, as does a
            # malformed or over-long answer.
            runner, _ = fake(range_result=(0, merge_out))
            require(git_candidate_identity(candidate, before, runner)["pushed_range_linear"] is False,
                    "offline_git_identity_merge_in_range")
            for result in ((1, b""), (128, b""), (129, b""), (-9, b""), (255, b""), (True, b""), (0, "not bytes"),
                           (0, None), (0, b"x" * (GIT_OUTPUT_CAP + 1)), (2, merge_out)):
                runner, _ = fake(range_result=result)
                require(rejects(lambda runner=runner: git_candidate_identity(candidate, before, runner)),
                        "offline_git_identity_range_refused")
            # Any other status (unknown object 128, usage 129, a signal, bool/non-int) refuses,
            # in either query, and so does a failing or malformed `log`.
            for statuses in ((2, 0), (0, 2), (128, 0), (0, 128), (129, 0), (-9, 0), (0, -9), (True, 0), (0, False), (255, 0)):
                runner, _ = fake(statuses)
                require(rejects(lambda runner=runner: git_candidate_identity(candidate, before, runner)),
                        "offline_git_identity_other_status_refused")
            for log in ((1, log_out), (128, log_out), (0, (head + "\n").encode()), (0, b""),
                        (0, (head + "\n" + parent + "\nextra\n").encode()), (0, b"\xff\xfe\n\n"),
                        (0, b"x" * (GIT_OUTPUT_CAP + 1)), (True, log_out), (0, "not bytes"), (0, None)):
                runner, _ = fake(log=log)
                require(rejects(lambda runner=runner: git_candidate_identity(candidate, before, runner)),
                        "offline_git_identity_log_refused")
            # Timeout, spawn failure and over-long merge-base output map to the fixed refusal.
            for failure in (subprocess.TimeoutExpired(["git"], 10), FileNotFoundError(), PermissionError(), OSError()):
                runner, _ = fake(error=failure)
                require(rejects(lambda runner=runner: git_candidate_identity(candidate, before, runner)),
                        "offline_git_identity_error_refused")
            runner, _ = fake(junk=b"x" * (GIT_OUTPUT_CAP + 1))
            require(rejects(lambda: git_candidate_identity(candidate, before, runner)), "offline_git_identity_overlong_refused")
            runner, _ = fake(junk=b"x" * GIT_OUTPUT_CAP)
            require(git_candidate_identity(candidate, before, runner)["scope_base_precedes_before"] is True,
                    "offline_git_identity_output_bound_exact")
            # An unsafe `before` never reaches a runner: no argv is built for it.
            for bad in (None, "", "0" * 40, "B" * 40, "b" * 39, "b" * 41, "-" + "b" * 39, "--upload-pack=x", " " + "b" * 39,
                        before + "\n", 12, [before], True):
                runner, calls = fake()
                require(rejects(lambda runner=runner, bad=bad: git_candidate_identity(candidate, bad, runner)) and not calls,
                        "offline_git_identity_unsafe_before_refused")
            return True

        def bounded_git_runner_cases():
            # run_git_bounded with REAL, harmless children: the interpreter running this check
            # (never git, launchctl or any service tool). The stdout cap must be enforced WHILE
            # collecting, the child must be killed and reaped at the cap or the deadline, and the
            # deadline must hold for a silent child and for a slow dribble that stays under the
            # cap. Every child has its own short lifetime, and every case is time-limited.
            def child(code):
                return [sys.executable, "-I", "-B", "-c", code]
            options = dict(cwd="/", env={"PATH": "/usr/bin:/bin"}, stdout=subprocess.PIPE,
                           stderr=subprocess.DEVNULL, timeout=10, check=False)
            flood = "import os,time\nend=time.monotonic()+30\nwhile time.monotonic()<end:\n    os.write(1,b'x'*65536)\n"
            started = time.monotonic()
            result = run_git_bounded(child(flood), **options)
            require(type(result.stdout) is bytes and len(result.stdout) == GIT_OUTPUT_CAP + 1
                    and type(result.returncode) is int and time.monotonic() - started < 8,
                    "offline_bounded_git_flood_cut_at_cap")
            # Exactly the cap is accepted whole; one byte more is returned as cap + 1 bytes, which the
            # caller refuses by length (query() requires len <= GIT_OUTPUT_CAP).
            exact = run_git_bounded(child("import os\nos.write(1,b'y'*%d)" % GIT_OUTPUT_CAP), **options)
            over = run_git_bounded(child("import os\nos.write(1,b'y'*%d)" % (GIT_OUTPUT_CAP + 1)), **options)
            require(exact.returncode == 0 and len(exact.stdout) == GIT_OUTPUT_CAP
                    and len(over.stdout) == GIT_OUTPUT_CAP + 1, "offline_bounded_git_cap_boundary")
            small = run_git_bounded(child("import sys\nsys.stdout.write('hello')"), **options)
            failed = run_git_bounded(child("import sys\nsys.stdout.write('z')\nraise SystemExit(3)"), **options)
            require(small.returncode == 0 and small.stdout == b"hello"
                    and failed.returncode == 3 and failed.stdout == b"z", "offline_bounded_git_status_and_output")
            def times_out(code, seconds):
                begun = time.monotonic()
                try:
                    run_git_bounded(child(code), **{**options, "timeout": seconds})
                except subprocess.TimeoutExpired:
                    return time.monotonic() - begun < seconds + 5
                return False
            require(times_out("import time\ntime.sleep(30)", 1)
                    and times_out("import os,time\nfor _ in range(100):\n    os.write(1,b'a')\n    time.sleep(0.2)", 1),
                    "offline_bounded_git_deadline_silent_and_dribble")
            # A leader that EXITS while a descendant in its process group keeps running must not
            # leave that descendant alive: (a) the descendant inherited the stdout pipe, so the
            # runner waits for the deadline and then refuses; (b) the descendant detached its
            # stdout, so the runner returns normally. In both cases the group is killed.
            # The descendant writes a heartbeat counter (os.pwrite on one descriptor, so the
            # file is never truncated); a frozen counter after the call means it is gone. The
            # leader starts it, waits until it is demonstrably running, writes `ok` and exits.
            # The descendant also stops by itself after 20 s, so a failed case cannot leak it.
            descendant = ("import os,sys,time\n"
                          "fd=os.open(sys.argv[1],os.O_WRONLY|os.O_CREAT,0o600)\n"
                          "end=time.monotonic()+20\n"
                          "n=0\n"
                          "while time.monotonic()<end:\n"
                          "    n+=1\n"
                          "    os.pwrite(fd,b'%012d'%n,0)\n"
                          "    time.sleep(0.05)\n")
            def group_survivor_case(holds_pipe):
                with tempfile.TemporaryDirectory() as directory:
                    beat = os.path.join(directory, "beat")
                    leader = ("import os,subprocess,sys,time\n"
                              "subprocess.Popen([sys.executable,'-I','-B','-c',%r,%r]%s)\n"
                              "end=time.monotonic()+3\n"
                              "while time.monotonic()<end and (not os.path.exists(%r) or os.path.getsize(%r)==0):\n"
                              "    time.sleep(0.01)\n"
                              "sys.stdout.write('ok')\n"
                              % (descendant, beat, "" if holds_pipe else ",stdout=subprocess.DEVNULL", beat, beat))
                    try:
                        finished = run_git_bounded(child(leader), **{**options, "timeout": 5})
                    except subprocess.TimeoutExpired:
                        finished = None
                    time.sleep(0.5)
                    first = Path(beat).read_bytes()
                    time.sleep(0.5)
                    second = Path(beat).read_bytes()
                    return finished, first, second
            finished, first, second = group_survivor_case(True)
            require(finished is None and first != b"" and first == second,
                    "offline_bounded_git_descendant_holding_pipe_killed")
            finished, first, second = group_survivor_case(False)
            require(finished is not None and finished.returncode == 0 and finished.stdout == b"ok"
                    and first != b"" and first == second, "offline_bounded_git_descendant_after_exit_killed")
            # The anchor mechanism itself (race-free group ownership). Popen is wrapped to RECORD
            # the processes and keyword arguments run_git_bounded uses, and is restored in the
            # finally. Checked: the anchor leads its own group (process_group=0) without holding
            # git's stdout pipe (stdout=DEVNULL); git joins the anchor's group (process_group=
            # anchor pid) and no longer starts a new session; the git child's real group id equals
            # the anchor's pid and differs from this process's group; the anchor was killed and
            # REAPED (returncode -SIGKILL) after the run; and a spawn failure still kills and
            # reaps the anchor and propagates OSError.
            real_popen = subprocess.Popen
            launched = []
            def recording_popen(*args, **kwargs):
                spawned = real_popen(*args, **kwargs)
                launched.append((spawned, kwargs))
                return spawned
            subprocess.Popen = recording_popen
            try:
                grouped = run_git_bounded(child("import os\nos.write(1,b'%d' % os.getpgrp())"), **options)
                normal_run = list(launched)
                del launched[:]
                try:
                    run_git_bounded(["/nonexistent-offline-binary"], **options)
                except OSError:
                    spawn_failed = True
                else:
                    spawn_failed = False
                failed_run = list(launched)
            finally:
                subprocess.Popen = real_popen
            require(len(normal_run) == 2 and grouped.returncode == 0 and grouped.stdout.isdigit(),
                    "offline_bounded_git_anchor_launch")
            (anchor, anchor_options), (git_child, git_options) = normal_run
            require(anchor_options.get("process_group") == 0 and anchor_options.get("stdout") == subprocess.DEVNULL
                    and not anchor_options.get("start_new_session")
                    and git_options.get("process_group") == anchor.pid and not git_options.get("start_new_session")
                    and git_options.get("stdout") == subprocess.PIPE, "offline_bounded_git_anchor_options")
            require(int(grouped.stdout) == anchor.pid and anchor.pid != os.getpgrp()
                    and anchor.returncode == -signal.SIGKILL and git_child.returncode == 0,
                    "offline_bounded_git_anchor_group_killed_and_reaped")
            require(spawn_failed and len(failed_run) == 1 and failed_run[0][0].returncode == -signal.SIGKILL,
                    "offline_bounded_git_anchor_reaped_on_spawn_failure")
            try:
                run_git_bounded(["/nonexistent-offline-binary"], **options)
            except OSError:
                pass
            else:
                return False
            # Only the exact call shape git_candidate_identity uses is accepted.
            for bad in ({"stdout": subprocess.DEVNULL}, {"stderr": subprocess.PIPE}, {"check": True},
                        {"timeout": 0}, {"timeout": 1.5}, {"timeout": None}):
                require(rejects(lambda bad=bad: run_git_bounded(child("pass"), **{**options, **bad})),
                        "offline_bounded_git_call_shape")
            require(git_candidate_identity.__defaults__ == (run_git_bounded,), "offline_bounded_git_is_default")
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
                    self.close_error, self.written = close_error, bytearray()
                def write(self, data):
                    if self.write_error:
                        raise self.error("inert client write")
                    self.written.extend(data)
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
                def __init__(self, error, fail_at, chunks=None):
                    self.error, self.fail_at, self.closed, self.chunks = error, fail_at, False, chunks
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
                    if self.chunks is not None:
                        return self.chunks.pop(0) if self.chunks else b""
                    return b"HTTP/1.1 101 Switching Protocols\r\n\r\n"
                def close(self):
                    self.check("close")
                    self.closed = True
            def exercise(data=b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n", *,
                         error=TimeoutError, read_at=0, write_error=False, flush_at=0,
                         handshake=False, backend_error="", buffered=True, closing=False, close_error=None,
                         chunks=None, capture=None):
                backend = Backend(error, backend_error, None if chunks is None else list(chunks))
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
                server.failure_reason = None
                server.server_port, server.server_name, server.paths = 8443, "inert", {"/healthz", "/", "/ws"}
                server.RequestHandlerClass = Handler
                connection = Connection(data, error, read_at, write_error, flush_at, handshake, close_error)
                if capture is not None:
                    capture.append(connection)
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
            websocket = b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade: websocket\r\n\r\n"
            server, _ = exercise(websocket, error=ssl.SSLEOFError, write_error=True)
            require(proxy_operations_allowed([server]) and server.counts.get("websocket") == 1
                    and server.counts.get("client_aborted") == 1, "offline_websocket_client_abort")
            for stage in ("send", "recv"):
                server, _ = exercise(websocket, error=ConnectionResetError, backend_error=stage)
                require(not proxy_operations_allowed([server]), "offline_websocket_backend_error_fatal")
            # A legitimate non-101 upgrade answer (relay not ready / restarting / wrong subprotocol / origin)
            # is relayed verbatim to the browser and never latches a proxy failure.
            for answer in (b"HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nwait\n",
                           b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n",
                           b"HTTP/1.1 403 Forbidden\r\nContent-Length: 19\r\n\r\nOrigin not allowed\n",
                           b"HTTP/1.1 503 Service Unavailable\r\n\r\n"):
                sent = []
                server, _ = exercise(websocket, error=ConnectionResetError, chunks=[answer], capture=sent)
                require(proxy_operations_allowed([server]) and server.failure_reason is None
                        and server.counts == {"tls_accepted": 1, "websocket": 1, "websocket_refused": 1}
                        and proxy_failure_rows([server]) == []
                        and bytes(sent[0].output.written) == answer, "offline_websocket_refusal_relayed")
            # The head and the declared body may arrive in separate backend reads; a body cut short by EOF is fatal.
            sent = []
            server, _ = exercise(websocket, error=ConnectionResetError, capture=sent, chunks=[
                b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 5\r\n\r\n", b"wa", b"it\n"])
            require(proxy_operations_allowed([server]) and bytes(sent[0].output.written) == (
                b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 5\r\n\r\nwait\n"), "offline_websocket_refusal_split")
            sent = []
            server, _ = exercise(websocket, error=ConnectionResetError, capture=sent, chunks=[
                b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 9\r\n\r\nwait\n"])
            require(not proxy_operations_allowed([server]) and not server.counts.get("websocket_refused")
                    and server.failure_reason[0] == "websocket_upgrade" and not sent[0].output.written,
                    "offline_websocket_refusal_short_fatal")
            # Every present-but-unusable framing declaration is fatal and forwards nothing.
            for framing in (b"Content-Length: 000009\r\n", b"Content-Length: 100000\r\n", b"Content-Length: 70000\r\n",
                            b"Content-Length: 9999999999999999999999\r\n", b"Content-Length: +5\r\n",
                            b"Content-Length: 5 5\r\n", b"Content-Length: -1\r\n", b"Content-Length: \r\n",
                            b"Content-Length: 5\r\nContent-Length: 5\r\n", b"Content-Length: 5\r\nContent-Length: 6\r\n",
                            b"content-length : 000005\r\n", b"Transfer-Encoding: chunked\r\n",
                            b"Content-Length: 5\r\nTransfer-Encoding: chunked\r\n"):
                sent = []
                server, _ = exercise(websocket, error=ConnectionResetError, capture=sent, chunks=[
                    b"HTTP/1.1 503 Service Unavailable\r\n" + framing + b"\r\nwait\n"])
                require(not proxy_operations_allowed([server]) and not server.counts.get("websocket_refused")
                        and server.failure_reason[0] in {"websocket_upgrade", "cap_result"}
                        and not sent[0].output.written, "offline_websocket_refusal_framing_fatal")
            # A client abort while relaying stays benign; a backend fault mid-body is still fatal.
            server, _ = exercise(websocket, error=ssl.SSLEOFError, write_error=True,
                                 chunks=[b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"])
            require(proxy_operations_allowed([server]) and server.counts.get("client_aborted") == 1,
                    "offline_websocket_refusal_client_abort")
            server, _ = exercise(websocket, error=ConnectionResetError, backend_error="recv",
                                 chunks=[b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 5\r\n\r\n"])
            require(not proxy_operations_allowed([server]), "offline_websocket_refusal_backend_fault")
            # Not a refusal answer (success/redirect/1xx/other protocol/garbage/oversize/early EOF) stays fatal.
            for answer in (b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", b"HTTP/1.1 302 Found\r\n\r\n",
                           b"HTTP/1.1 100 Continue\r\n\r\n", b"HTTP/1.0 503 Service Unavailable\r\n\r\n",
                           b"HTTP/1.1 6xx Bad\r\n\r\n", b"HTTP/1.1 503\x00 Bad\r\n\r\n", b"garbage\r\n\r\n",
                           b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 70000\r\n\r\n", b""):
                sent = []
                server, _ = exercise(websocket, error=ConnectionResetError, chunks=[answer], capture=sent)
                require(not proxy_operations_allowed([server]) and not server.counts.get("websocket_refused")
                        and server.failure_reason[0] in {"websocket_upgrade", "cap_result"}
                        and not sent[0].output.written, "offline_websocket_malformed_refusal_fatal")
            # First-cause locators: fixed codes and coarse categories only, never a raw path/query/token.
            diagnostics = (
                (b"GET /version.json?check=abc HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_allowlist", "version", "check", "get")),
                (b"GET /private?secret=value HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_allowlist", "other", "other", "get")),
                (b"GET /index.html?herdr_reload=bad HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_allowlist", "index", "reload", "get")),
                (b"GET /healthz?herdr_deploy_check=zzz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_allowlist", "healthz", "deploy_check", "get")),
                (b"GET /assets/app-" + b"a" * 64 + b".js?x=1 HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_allowlist", "asset", "other", "get")),
                (b"GET / HTTP/1.1\r\nHost: foreign.invalid\r\nUpgrade: websocket\r\n\r\n",
                 ("request_allowlist", "root", "none", "get")),
                (b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade: h2c\r\n\r\n",
                 ("request_allowlist", "root", "none", "get")),
                (b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nContent-Length: 1\r\n\r\nx",
                 ("request_body", "root", "none", "get")),
                (b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_header_multiplicity", "healthz", "none", "get")),
                (b"HEAD /healthz?token=secret HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_method", "unknown", "unknown", "head")),
                (b"BREW /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                 ("request_method", "unknown", "unknown", "other")),
                (b"GET /healthz HTTP/invalid\r\n\r\n", ("request_parse", "unknown", "unknown", "get")))
            for request, expected in diagnostics:
                server, calls = exercise(request, error=BrokenPipeError, flush_at=1)
                encoded_rows = json.dumps(proxy_failure_rows([server]))
                require(not proxy_operations_allowed([server]) and server.failure_reason == expected
                        and proxy_failure_rows([server]) == [{"proxy": 0, "reason": expected[0], "path": expected[1],
                            "query": expected[2], "method": expected[3]}]
                        and not any(word in encoded_rows for word in ("secret", "private", "abc", "token", "bad", "zzz", "x=1")),
                        "offline_proxy_failure_locator")
            server, _ = exercise(error=ConnectionResetError, backend_error="request")
            require(server.failure_reason == ("proxy_backend", "healthz", "none", "get"), "offline_proxy_backend_locator")
            # The FIRST cause wins and an unknown/forged code collapses to a safe default.
            probe = object.__new__(Proxy)
            probe.failure = False
            probe.latch("worker_exception", "healthz", "none", "get")
            probe.latch("request_allowlist", "root", "none", "get")
            require(probe.failure is True and probe.failure_reason == ("worker_exception", "healthz", "none", "get"),
                    "offline_proxy_first_cause")
            probe = object.__new__(Proxy)
            probe.latch("/private/token", "/private", "token=1", "GET")
            require(probe.failure_reason == ("handler_exception", "unknown", "unknown", "unknown"), "offline_proxy_locator_defaults")
            require(proxy_failure_rows([SimpleNamespace(failure=False), SimpleNamespace(failure=True),
                    SimpleNamespace(failure=True, failure_reason=("request_allowlist", "/x", "check", "get")),
                    SimpleNamespace(failure=True, failure_reason=("request_allowlist", "version", "check", "get"))]) == [
                    {"proxy": 1, "reason": "handler_exception", "path": "unknown", "query": "unknown", "method": "unknown"},
                    {"proxy": 2, "reason": "handler_exception", "path": "unknown", "query": "unknown", "method": "unknown"},
                    {"proxy": 3, "reason": "request_allowlist", "path": "version", "query": "check", "method": "get"}]
                    and proxy_failure_rows([]) == [], "offline_proxy_failure_rows")
            for target in ("/", "/index.html", "/healthz?v=1", "/builds/1.2.3-1-" + "a" * 16 + "/index.html",
                           "/release.json?herdr_deploy_check=x", "/version.json?check=1", "/anything/else?secret=1",
                           "https://foreign.invalid/x", "//foreign.invalid/x", "/\x00?x", "http://[::1"):
                path_category, query_category = proxy_request_categories(target)
                require(path_category in PROXY_PATH_CATEGORIES and query_category in PROXY_QUERY_CATEGORIES
                        and not any(word in path_category + query_category for word in ("secret", "foreign", "anything", "builds")),
                        "offline_proxy_categories_fixed")
            require(proxy_request_categories("/version.json?check=1") == ("version", "check")
                    and proxy_request_categories("/private?secret=1") == ("other", "other")
                    and proxy_request_categories("https://foreign.invalid/") == ("invalid", "other")
                    and proxy_method_category(b"GET") == "get" and proxy_method_category(b"\xff") == "other"
                    and proxy_method_category(b"secret") == "other"
                    and proxy_failure_reason(Refusal("request_allowlist")) == "request_allowlist"
                    and proxy_failure_reason(Refusal("/private/path")) == "handler_exception"
                    and proxy_failure_reason(RuntimeError("request_allowlist")) == "handler_exception",
                    "offline_proxy_category_mapping")
            rejected = [f"{method} / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n".encode()
                        for method in ("HEAD", "POST", "PUT", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PATCH")]
            rejected.extend((b"GET /private HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                b"GET /?secret=value HTTP/1.1\r\nHost: 127.0.0.1:8443\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: foreign.invalid\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade: h2c\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nContent-Length: 1\r\n\r\nx",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nContent-Length:\r\n\r\nx",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
                b"GET / HTTP/invalid\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nHost: foreign.invalid\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nHost: 127.0.0.1:8443\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nHost: foreign.invalid\r\nUpgrade: websocket\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade: websocket\r\nUpgrade: h2c\r\n\r\n",
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade: websocket\r\nUpgrade: websocket\r\n\r\n",
                b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8443\r\nUpgrade:\r\nUpgrade: h2c\r\n\r\n"))
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
        def socket_contract_cases():
            scenario = helper.socket_scenario_view({})
            def state(counters, alive=True, started=True, stopped=False):
                return SimpleNamespace(counters=counters, thread=SimpleNamespace(ident=1 if started else None,
                    is_alive=lambda: alive), stopped=SimpleNamespace(is_set=lambda: stopped))
            class ObservedLock:
                def __init__(self, lock):
                    self.lock, self.depth = lock, 0
                def __enter__(self):
                    self.lock.__enter__()
                    self.depth += 1
                    return self
                def __exit__(self, *args):
                    self.depth -= 1
                    return self.lock.__exit__(*args)
            def exercise(raw, recv_error=None, send_error=None, stream_error=None, view=scenario, publication_error=None):
                counters, sent, timeouts = helper.SocketCounters(), [], []
                counters.lock = ObservedLock(counters.lock)
                source = io.BytesIO(raw)
                responded, flow, callbacks = [False], [], []
                def recv(maximum):
                    flow.append("recv")
                    if recv_error is not None:
                        raise recv_error("/private/inert-token-never-recorded")
                    if responded[0] and stream_error is not None:
                        raise stream_error("/private/inert-token-never-recorded")
                    return source.read(maximum)
                def send(data):
                    flow.append("send")
                    sent.append(data)
                    if send_error is not None:
                        raise send_error("/private/inert-token-never-recorded")
                    responded[0] = True
                def timeout(value):
                    flow.append("hold" if value is None else "timeout")
                    timeouts.append(value)
                def terminal():
                    callbacks.append((counters.lock.depth, counters.handled_connections,
                        sum(counters.counts.values()), sum(counters.reasons.values()), list(flow)))
                    flow.append("callback")
                    if publication_error is not None:
                        raise publication_error("/private/inert-token-never-recorded")
                helper.handle_socket_connection(recv, send, view, counters, set_timeout=timeout, on_terminal=terminal)
                require(len(callbacks) == flow.count("callback") == 1 and callbacks[0][:4] == (0, 1, 1, 1)
                        and (not sent or callbacks[0][4][-1] == "send") and counters.lock.depth == 0,
                        "offline_callback_once_after_terminal_outside_lock")
                if timeouts[-1] is None:
                    require(flow[flow.index("callback") + 1:] == ["hold", "recv"], "offline_callback_before_stream_hold")
                if publication_error is None:
                    require(counters.publication_exceptions == {}, "offline_successful_callback_empty_bucket")
                diagnostics = helper.socket_diagnostic_snapshot(state(counters))
                diagnostics.update(closed_after_cleanup="not_observed", guard=socket_diagnostic_guard(diagnostics))
                require(socket_diagnostics_policy(diagnostics) and counters.handled_connections == 1
                        and sum(row["count"] for row in diagnostics["rows"]) == sum(diagnostics["reasons"].values()) == 1,
                        "offline_one_terminal_row")
                require(b"/private" not in encode(diagnostics) and b"inert-token" not in encode(diagnostics), "offline_socket_counter_privacy")
                return counters, sent, timeouts, diagnostics
            def request(method, params, rid="mobile-relay-api-1"):
                return encode({"id": rid, "method": method, "params": params})
            # Inert replay of the packaged relay's own socket traffic (capabilities.go
            # refresh: ping + three capability probes; client.go/events.go reads and
            # subscription). HEAD recorded the two move probes as ("other", "failed"),
            # which its allowlist refused; the current fixture answers them with the
            # recognized validation refusals and counts them as refused_expected.
            relay_traffic = [
                ("ping", {}, "mobile-relay-api-1", None), ("workspace.move_block", {"workspace_ids": []},
                    "mobile-relay-api-2", "workspace_move_block_failed"),
                ("tab.move", {"tab_id": "", "insert_index": 0}, "mobile-relay-api-3", "tab_not_found"),
                ("pane.read", {"pane_id": "", "source": "visible", "lines": 1, "format": "ansi", "strip_ansi": False},
                    "mobile-relay-api-4", "pane_not_found"),
                ("agent.list", {}, "mobile-relay-api-5", None), ("pane.list", {}, "mobile-relay-api-6", None),
                ("workspace.list", {}, "mobile-relay-api-7", None), ("tab.list", {}, "mobile-relay-api-8", None),
                ("pane.read", {"pane_id": "workspace:agent", "source": "visible", "lines": 20, "format": "text",
                    "strip_ansi": True}, "mobile-relay-api-9", None),
                ("session.snapshot", {}, "mobile-relay-snapshot", None),
                ("events.subscribe", {"subscriptions": []}, "mobile-relay-events", None)]
            relay_counts = {}
            for method, params, rid, code in relay_traffic:
                response, label, outcome, _reason = helper.dispatch_socket_request(request(method, params, rid), scenario)
                require(response["id"] == rid and label == method and ("error" in response) == (code is not None)
                        and (code is None or response["error"]["code"] == code), "offline_relay_traffic_response")
                relay_counts[(label, outcome)] = relay_counts.get((label, outcome), 0) + 1
            require(socket_operations_allowed(relay_counts)
                    and relay_counts[("pane.read", "refused_expected")] == relay_counts[("pane.read", "succeeded")] == 1
                    and relay_counts[("workspace.move_block", "refused_expected")] == 1
                    and relay_counts[("tab.move", "refused_expected")] == 1
                    and not socket_operations_allowed({("other", "failed"): 2, ("ping", "succeeded"): 1}),
                    "offline_relay_traffic_allowed")
            values = {"empty_list": [], "empty_str": "", "int:0": 0, "int:1": 1,
                      "false": False, "str:ansi": "ansi", "str:visible": "visible"}
            for probe in helper.INERT_PROBES:
                params = {key: values[tag] for key, tag in probe.params}
                response, label, outcome, reason = helper.dispatch_socket_request(request(probe.method, params), scenario)
                require(response == {"id": "mobile-relay-api-1", "error": {"code": probe.code, "message": probe.message}}
                        and (label, outcome, reason) == (probe.method, "refused_expected", "inert_probe"), "offline_exact_probe")
                counters, sent, _, _ = exercise(request(probe.method, params) + b"\n")
                require(counters.counts == {(probe.method, "refused_expected"): 1}
                        and json.loads(sent[0]) == response, "offline_probe_sent_before_record")
                shape_reason = "invalid_params" if probe.method == "pane.read" else "probe_shape"
                require(helper.dispatch_socket_request(encode({"id": "mobile-relay-api-1", "method": probe.method}), scenario)[1:]
                        == (probe.method, "rejected", shape_reason), "offline_missing_probe_params")
                negatives = [(bad, shape_reason) for bad in (None, [], "wrong", False, 0, 0.0)]
                negatives.append(({**params, "extra": 1}, "probe_shape"))
                for key in params:
                    missing = {name: value for name, value in params.items() if name != key}
                    negatives.append((missing, "invalid_params" if probe.method == "pane.read" and key == "pane_id" else "probe_shape"))
                    replacements = {
                        "workspace_ids": [["nonempty"], None, {}, "", False, 0],
                        "tab_id": ["nonempty", None, [], False, 0],
                        "insert_index": [True, False, 0.0, "0", 1, None, []],
                        "pane_id": [None, True, 0, 0.0, []],
                        "lines": [True, False, 1.0, "1", 0, 2, None],
                        "format": ["text", None, [], True], "source": ["recent", None, [], True],
                        "strip_ansi": [True, 0, "false", None, []]}
                    for bad in replacements[key]:
                        negatives.append(({**params, key: bad}, "invalid_params" if key == "pane_id" else "probe_shape"))
                for negative, expected_reason in negatives:
                    response, label, outcome, reason = helper.dispatch_socket_request(request(probe.method, negative), scenario)
                    require((label, outcome, reason) == (probe.method, "rejected", expected_reason)
                            and response["id"] == "mobile-relay-api-1" and response["error"]["code"] == "fixture_refused",
                            "offline_probe_negative")
            for method in ("pane.send_input", "pane.send_keys", "workspace.move", "agent.stop", "unknown"):
                response, label, outcome, reason = helper.dispatch_socket_request(request(method, {}), scenario)
                require((label, outcome, reason) == ("other", "rejected", "unsupported_method")
                        and response["error"]["code"] == "unknown_method" and response["id"] == "mobile-relay-api-1",
                        "offline_no_mutation_authority")
            predispatch = [(b"{", "malformed_request"), (b"\xff", "malformed_request"),
                (b'{"id":"x","id":"y","method":"ping"}', "malformed_request"),
                (b'{"id":"x","method":"ping","params":{"duplicate":1,"duplicate":2}}', "malformed_request"),
                (b"[]", "non_object_request"), (b"null", "non_object_request"), (b"1", "non_object_request"),
                (encode({"id": "do-not-echo", "method": "ping", "extra": 1}), "invalid_envelope"),
                (encode({"method": "ping"}), "invalid_envelope"), (encode({"id": "do-not-echo"}), "invalid_envelope"),
                (encode({"id": "do-not-echo", "method": False}), "invalid_envelope")]
            predispatch.extend((b'{"id":"x","method":"ping","params":' + constant + b'}', "malformed_request")
                               for constant in (b"NaN", b"Infinity", b"-Infinity"))
            predispatch.extend((request("ping", {}, rid), "invalid_request_id")
                               for rid in (False, 1, None, [], {}, "", "x" * 129, "/private/path", "x\n"))
            for raw, expected_reason in predispatch:
                response, label, outcome, reason = helper.dispatch_socket_request(raw, scenario)
                require((label, outcome, reason) == ("other", "rejected", expected_reason)
                        and response == {"id": "", "error": {"code": "invalid_request", "message": "invalid request: fixture refused"}},
                        "offline_predispatch_no_echo")
                counters, sent, _, _ = exercise(raw + b"\n")
                require(counters.reasons == {expected_reason: 1} and json.loads(sent[0])["id"] == "", "offline_predispatch_handled")
            for rid in ("package-fixture-readiness", "mobile-relay-api-123", "fixture:pane", "x" * 128):
                require(helper.dispatch_socket_request(request("ping", {}, rid), scenario)[0]["id"] == rid, "offline_compatible_ids")
            for raw in (b"", request("ping", {})):
                counters, sent, _, _ = exercise(raw)
                require(counters.counts == {("other", "rejected"): 1} and counters.reasons == {"incomplete_request": 1}
                        and not sent, "offline_clean_incomplete")
            require(helper.dispatch_socket_request(b"x" * helper.SOCKET_REQUEST_MAX, scenario)[3] == "oversize_request", "offline_dispatch_size")
            for raw in (b"x" * helper.SOCKET_REQUEST_MAX, b"x" * helper.SOCKET_REQUEST_MAX + b"\n"):
                counters, sent, _, _ = exercise(raw)
                require(counters.reasons == {"oversize_request": 1} and json.loads(sent[0])["id"] == "", "offline_oversize_handled")
            raw = request("ping", {})
            counters, _, _, _ = exercise(raw + b" " * (helper.SOCKET_REQUEST_MAX - 1 - len(raw)) + b"\n")
            require(counters.counts == {("ping", "succeeded"): 1}, "offline_read_bound_positive")
            errors = ((TimeoutError, "timeout"), (ConnectionResetError, "connection_reset"),
                      (BrokenPipeError, "broken_pipe"), (OSError, "os_error"), (RuntimeError, "internal"))
            for error, category in errors:
                counters, sent, timeouts, diagnostics = exercise(request("ping", {}) + b"\n", publication_error=error)
                require(counters.counts == {("ping", "succeeded"): 1} and counters.reasons == {"ordinary": 1}
                        and counters.publication_exceptions == {category: 1}
                        and counters.handler_exceptions == counters.stream_exceptions == counters.listener_exceptions == {}
                        and json.loads(sent[0])["result"]["type"] == "pong" and timeouts == [5.0]
                        and diagnostics["guard"] == "recorded_exception", "offline_callback_fault_not_transport_failure")
                fixture_state = state(counters)
                fixture_state.diagnostic_summary = lambda: helper.socket_diagnostic_snapshot(fixture_state)
                captured = capture_socket_diagnostics({}, fixture_state)
                require(captured["publication_exceptions"] == {category: 1}
                        and captured["liveness_before_cleanup"] == "serving"
                        and captured["closed_after_cleanup"] == "not_observed"
                        and not socket_fixture_allowed(fixture_state, {}), "offline_callback_fault_before_cleanup_capture")
                later_source, later_sent, later_callbacks = io.BytesIO(request("ping", {}) + b"\n"), [], []
                helper.handle_socket_connection(later_source.read, later_sent.append, scenario, counters,
                    on_terminal=lambda: later_callbacks.append((counters.lock.depth, counters.handled_connections,
                        dict(counters.publication_exceptions))))
                later = helper.socket_diagnostic_snapshot(state(counters))
                later.update(closed_after_cleanup="closed", guard=socket_diagnostic_guard(later))
                require(later_callbacks == [(0, 2, {category: 1})] and len(later_sent) == 1
                        and counters.counts == {("ping", "succeeded"): 2} and counters.reasons == {"ordinary": 2}
                        and counters.handled_connections == 2 and later["publication_exceptions"] == {category: 1}
                        and diagnostics["publication_exceptions"] == {category: 1}
                        and later["guard"] == "recorded_exception" and socket_diagnostics_policy(later)
                        and rejects(lambda: socket_diagnostics_policy(later, passing=True, operation_rows=later["rows"])),
                        "offline_later_callback_success_keeps_fault")
                counters, _, timeouts, diagnostics = exercise(request("events.subscribe", {}) + b"\n", publication_error=error)
                require(counters.counts == {("events.subscribe", "succeeded"): 1} and counters.reasons == {"ordinary": 1}
                        and counters.publication_exceptions == {category: 1} and counters.stream_exceptions == {}
                        and timeouts == [5.0, None] and diagnostics["guard"] == "recorded_exception",
                        "offline_callback_fault_keeps_stream_eof_hold")
                counters, _, timeouts, _ = exercise(request("events.subscribe", {}) + b"\n",
                    publication_error=error, stream_error=ConnectionResetError)
                require(counters.publication_exceptions == {category: 1} and counters.stream_exceptions == {"connection_reset": 1}
                        and counters.handler_exceptions == {} and counters.reasons == {"ordinary": 1}
                        and counters.handled_connections == 1 and timeouts == [5.0, None],
                        "offline_callback_and_stream_faults_separate")
            counters, _, _, _ = exercise(request("ping", {}) + b"\n", send_error=BrokenPipeError, publication_error=OSError)
            require(counters.counts == {("ping", "failed"): 1} and counters.reasons == {"send_failed": 1}
                    and counters.handler_exceptions == {"broken_pipe": 1} and counters.publication_exceptions == {"os_error": 1}
                    and counters.handled_connections == 1, "offline_send_and_callback_faults_separate")
            # Exercise the actual Linux publisher ordering with an in-memory
            # writer, not a fixture, socket, filesystem operation or fault.
            counters, source, writes, sequence = helper.SocketCounters(), io.BytesIO(request("ping", {}) + b"\n"), [], []
            counters.lock = ObservedLock(counters.lock)
            publisher = SimpleNamespace(operations_lock=ObservedLock(threading.RLock()), operations_path=object(),
                operation_summary=counters.operation_summary)
            original_writer = helper.write_json
            def memory_write(path, rows):
                sequence.append("write")
                writes.append((path, rows, counters.lock.depth, publisher.operations_lock.depth))
            def memory_send(_data):
                sequence.append("send")
                require(counters.handled_connections == 0, "offline_publisher_send_before_record")
            helper.write_json = memory_write
            try:
                helper.handle_socket_connection(source.read, memory_send, scenario, counters,
                    on_terminal=lambda: helper.HerdrSocketFixture._publish_operations(publisher))
            finally:
                helper.write_json = original_writer
            require(sequence == ["send", "write"] and writes == [(publisher.operations_path,
                    [{"method": "ping", "outcome": "succeeded", "count": 1}], 0, 1)]
                    and counters.publication_exceptions == {} and counters.reasons == {"ordinary": 1}
                    and counters.handled_connections == 1 and counters.lock.depth == publisher.operations_lock.depth == 0,
                    "offline_publisher_io_outside_counter_lock")
            for error, category in errors:
                counters, sent, _, diagnostics = exercise(b"", recv_error=error)
                require(not sent and counters.counts == {("other", "failed"): 1}
                        and counters.reasons == {"recv_failed": 1} and counters.handler_exceptions == {category: 1}
                        and diagnostics["guard"] == "recorded_exception", "offline_recv_failure")
                send_requests = [(request("ping", {}), "ping"), (b"{", "other"),
                    (request("unknown", {}), "other"), (request("tab.move", {"tab_id": "", "insert_index": 0}), "tab.move"),
                    (request("tab.move", {"tab_id": "nonempty", "insert_index": 0}), "tab.move")]
                for raw, label in send_requests:
                    counters, sent, _, diagnostics = exercise(raw + b"\n", send_error=error)
                    require(len(sent) == 1 and counters.counts == {(label, "failed"): 1}
                            and counters.reasons == {"send_failed": 1} and counters.handler_exceptions == {category: 1}
                            and diagnostics["guard"] == "recorded_exception", "offline_failed_send_not_success_or_rejection")
                counters, _, timeouts, diagnostics = exercise(request("events.subscribe", {}) + b"\n", stream_error=error)
                require(counters.counts == {("events.subscribe", "succeeded"): 1} and counters.reasons == {"ordinary": 1}
                        and counters.handler_exceptions == {} and counters.stream_exceptions == {category: 1}
                        and timeouts == [5.0, None] and diagnostics["guard"] == "recorded_exception", "offline_stream_exception_separate")
                counters = helper.SocketCounters()
                fixture_state = state(counters, alive=False)
                class FailedListenerDispatch:
                    def __enter__(self):
                        raise error("/private/inert-token")
                    def __exit__(self, *_args):
                        return False
                # An accept polling timeout continues normally. Exercise loop
                # death after accept, before a handler is reached, for all five
                # categories (including timeout) without inventing a row.
                fixture_state.listener = SimpleNamespace(accept=lambda: (None, None))
                fixture_state.lock = FailedListenerDispatch()
                helper.HerdrSocketFixture._serve(fixture_state)  # Duck state only; no fixture or socket allocation.
                diagnostics = helper.socket_diagnostic_snapshot(fixture_state)
                require(counters.counts == counters.reasons == {} and counters.handled_connections == 0
                        and counters.listener_exceptions == {category: 1} and diagnostics["liveness_before_cleanup"] == "early_death"
                        and socket_diagnostic_guard(diagnostics) == "recorded_exception", "offline_listener_death_no_row")
            # Every ordinary and probe reply also produces a truthful failed
            # row if sending it fails; the successful/refused row is not kept.
            send_cases = [(method, {"pane_id": "nonempty"}) for method in SOCKET_ORDINARY]
            send_cases.extend((probe.method, {key: values[tag] for key, tag in probe.params}) for probe in helper.INERT_PROBES)
            for method, params in send_cases:
                counters, _, _, _ = exercise(request(method, params) + b"\n", send_error=BrokenPipeError)
                require(counters.counts == {(method, "failed"): 1} and counters.reasons == {"send_failed": 1},
                        "offline_all_reply_send_failures")
            counters = helper.SocketCounters()
            idle_state, stopping = state(counters), [False]
            idle_state.stopped = SimpleNamespace(is_set=lambda: stopping[0])
            def idle_timeout():
                stopping[0] = True
                raise TimeoutError("inert accept poll")
            idle_state.listener = SimpleNamespace(accept=idle_timeout)
            helper.HerdrSocketFixture._serve(idle_state)
            require(counters.counts == counters.reasons == counters.listener_exceptions == {}, "offline_accept_poll_not_death")
            # Retirement flips during accept: only an owner-closed descriptor
            # (EBADF) is benign; EIO on a live listener stays a recorded fault.
            for retiring_error, expected_bucket in ((OSError(errno.EBADF, "inert closed listener"), {}),
                    (OSError(errno.EIO, "/private/inert-token"), {"os_error": 1}),
                    (ConnectionResetError("/private/inert-token"), {"connection_reset": 1})):
                counters = helper.SocketCounters()
                race_state, flag = state(counters), [False]
                race_state.stopped = SimpleNamespace(is_set=lambda: flag[0])
                def accept_after_retirement(error=retiring_error):
                    flag[0] = True  # SocketOwner.close sets stopped before closing the listener.
                    raise error
                race_state.listener = SimpleNamespace(accept=accept_after_retirement)
                helper.HerdrSocketFixture._serve(race_state)
                require(counters.listener_exceptions == expected_bucket and counters.counts == counters.reasons == {}
                        and b"inert-token" not in encode(counters.listener_exceptions), "offline_listener_retirement_errno_scope")
            counters = helper.SocketCounters()
            for alive, started, stopped, expected in ((True, True, False, "serving"), (False, True, False, "early_death"),
                    (False, False, False, "not_started"), (False, True, True, "stopped")):
                require(helper.socket_diagnostic_snapshot(state(counters, alive, started, stopped))["liveness_before_cleanup"] == expected,
                        "offline_liveness_enum")
            counters, _, timeouts, diagnostics = exercise(request("events.subscribe", {}) + b"\n")
            require(counters.stream_exceptions == {} and counters.handler_exceptions == {}
                    and counters.counts == {("events.subscribe", "succeeded"): 1} and timeouts == [5.0, None]
                    and diagnostics["guard"] == "allowed", "offline_stream_eof_normal")
            counters, sent, _, diagnostics = exercise(request("agent.list", {}) + b"\n", view={})
            require(not sent and counters.counts == {("other", "failed"): 1}
                    and counters.reasons == {"internal_error": 1} and counters.handler_exceptions == {"internal": 1}
                    and diagnostics["guard"] == "recorded_exception", "offline_internal_dispatch_failure")
            # All 44 matrix cells: 26 producible and 18 impossible, never just a
            # positive sample. Only successful reads and bounded refusals admit.
            methods = SOCKET_ORDINARY | SOCKET_PROBES | {"other"}
            for method in methods:
                for outcome in ("succeeded", "refused_expected", "rejected", "failed"):
                    pair = (method, outcome)
                    admit = (method in SOCKET_ORDINARY and outcome == "succeeded") or (method in SOCKET_PROBES and outcome == "refused_expected")
                    require(socket_operations_allowed({pair: 1}) is admit, "offline_full_pair_matrix")
                    rows = [{"method": method, "outcome": outcome, "count": 1}]
                    if pair in SOCKET_PAIRS:
                        require(socket_row_counts(rows) == {pair: 1}, "offline_producible_pair")
                    else:
                        require(rejects(lambda: socket_row_counts(rows)), "offline_impossible_pair")
            for method in SOCKET_PROBES:
                require(socket_operations_allowed({(method, "refused_expected"): 64})
                        and socket_pair_guard({(method, "refused_expected"): 65}) == "probe_cap_exceeded", "offline_probe_cap_boundary")
            for count in (True, 0, 65536, 1.0, "1"):
                require(not socket_operations_allowed({("ping", "succeeded"): count}), "offline_guard_typed_count")
            counters = helper.SocketCounters()
            for _ in range(COUNT_MAX + 2):
                counters.record("ping", "succeeded", "ordinary")
            diagnostics = helper.socket_diagnostic_snapshot(state(counters))
            require(counters.counts == {("ping", "succeeded"): COUNT_MAX} and counters.reasons == {"ordinary": COUNT_MAX}
                    and counters.handled_connections == COUNT_MAX and counters.saturated is True
                    and socket_diagnostic_guard(diagnostics) == "counter_saturated", "offline_acquisition_saturation")
            counters = helper.SocketCounters()
            error = TimeoutError("/private/inert-token")
            for _ in range(COUNT_MAX + 2):
                counters.record("other", "failed", "recv_failed", error)
            require(counters.counts == {("other", "failed"): COUNT_MAX} and counters.reasons == {"recv_failed": COUNT_MAX}
                    and counters.handler_exceptions == {"timeout": COUNT_MAX} and counters.saturated is True,
                    "offline_handler_acquisition_saturation")
            for bucket in ("stream_exceptions", "listener_exceptions"):
                counters = helper.SocketCounters()
                for _ in range(COUNT_MAX + 2):
                    counters.record_exception(bucket, RuntimeError("/private/inert-token"))
                require(getattr(counters, bucket) == {"internal": COUNT_MAX} and counters.saturated is True,
                        "offline_exception_acquisition_saturation")
            return True

        def packaged_go_tls_control_cases():
            # Exercise the production command boundary with an inert capture,
            # not the binary, a subprocess, a clock sleep or a TLS connection.
            root, binary = Path("/must-not-be-accessed"), Path("/must-not-execute")
            calls = []
            response = SimpleNamespace(returncode=1, stdout=b"", stderr=b"inert certificate refusal")
            def capture(argv, timeout):
                calls.append((argv, timeout))
                return response
            mock = SimpleNamespace(capture=capture)
            for category, port, timeout in (("positive", 8443, 10), ("unknown_ca", 18444, 130),
                                             ("wrong_host", 18445, 130)):
                origin = f"https://127.0.0.1:{port}"
                result = packaged_go_tls_control(root, mock, binary, "fixture-version", "fixture-revision", origin, category)
                require(result is response and calls[-1] == ([str(binary), "verify-public", "--origin", origin,
                        "--web-root", str(root / "release/web"), "--version", "fixture-version",
                        "--revision", "fixture-revision"], timeout), "offline_packaged_tls_command_deadline")
            require(rejects(lambda: packaged_go_tls_control(root, mock, binary, "v", "r", ORIGIN, "other"))
                    and len(calls) == 3 and PHASES == (90, 300, 210, 630, 30)
                    and LIMITS["total_seconds"] == 1260, "offline_packaged_tls_caps_unchanged")
            def expired(*_args):
                raise Refusal("command_deadline")
            require(rejects(lambda: packaged_go_tls_control(root, SimpleNamespace(capture=expired), binary,
                    "v", "r", ORIGIN, "unknown_ca")), "offline_packaged_tls_timeout_not_refusal")
            return True

        def forbidden_effects():
            require(real_guards(), "offline_real_guards")
            require(launchctl_grant_cases(), "offline_launchctl_grants")
            require(candidate_scope_cases(), "offline_candidate_scope")
            require(git_identity_cases(), "offline_git_identity")
            require(bounded_git_runner_cases(), "offline_bounded_git_runner")
            require(launch_argument_cases(), "offline_launch_argument_policy")
            require(proxy_transport_cases(), "offline_proxy_transport")
            require(certificate_configuration(), "offline_certificate_configuration")
            require(packaged_go_tls_control_cases(), "offline_packaged_tls_control")
            require(socket_contract_cases(), "offline_socket_contract")
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
            # Mid-browser abort: a detached helper may still be writing under the owned root for a
            # moment, so a pass can fail with ENOTEMPTY. A few bounded retries inside the SAME shared
            # deadline complete deletion; the last error is never swallowed and the cap is never renewed.
            def flaky(failures, error=lambda: OSError(errno.ENOTEMPTY, "inert racing helper"), spend=0):
                calls = []
                def remove_pass(value):
                    calls.append(True)
                    now[0] += spend
                    if len(calls) <= failures:
                        raise error()
                    value.present = False
                return remove_pass, calls
            for failures in (0, 1, REMOVE_ATTEMPTS - 1):
                root.present, now[0], slept = True, 0, []
                remove_pass, calls = flaky(failures)
                remove_owned_tree(root, identity, CleanupDeadline(clock=clock), remove_pass, slept.append)
                require(len(calls) == failures + 1 and root.present is False and len(slept) == failures
                        and all(0 < pause <= 0.25 for pause in slept), "offline_deletion_retry_completes")
            root.present, now[0] = True, 0
            remove_pass, calls = flaky(REMOVE_ATTEMPTS)
            try:
                remove_owned_tree(root, identity, CleanupDeadline(clock=clock), remove_pass, lambda _: None)
            except OSError:
                require(len(calls) == REMOVE_ATTEMPTS and root.present is True, "offline_deletion_retry_exhausted")
            else:
                return False
            root.present, now[0] = True, 0
            remove_pass, calls = flaky(REMOVE_ATTEMPTS, spend=31)
            require(rejects(lambda: remove_owned_tree(root, identity, CleanupDeadline(clock=clock), remove_pass,
                    lambda _: None)) and len(calls) == 1 and root.present is True, "offline_deletion_retry_deadline")
            root.present, now[0] = True, 0
            remove_pass, calls = flaky(1, error=lambda: FileNotFoundError(errno.ENOENT, "inert vanished"))
            require(rejects(lambda: remove_owned_tree(root, identity, CleanupDeadline(clock=clock), remove_pass,
                    lambda _: None)) and len(calls) == 1 and root.present is True, "offline_deletion_vanished_survivor")
            root.present, now[0] = True, 0
            remove_pass, calls = flaky(0)
            remove_owned_tree(root, identity, CleanupDeadline(clock=clock), remove_pass, lambda _: None)
            require(len(calls) == 1 and root.present is False and REMOVE_ATTEMPTS == 4, "offline_deletion_single_pass")
            # Guard-aborted browser: only the unattested-contexts closure error is recorded while every
            # owned child/listener is observed gone. Closure stays uncertain (cleanup remains False,
            # the original failure is never replaced) but the root MUST still be deleted. Anything
            # else uncertain still aborts before deletion exactly as before.
            def aborted_owner(*errors, survive=False, listener=False, listener_error=False):
                owner, child = Cleanup(), InertChild(survive)
                owner.child(child)
                owner.listener(InertListener(listener, listener_error))
                for error in errors:
                    owner.error(error)
                return owner
            owner = aborted_owner("browser_contexts_unobserved")
            require(close_owned_resources(owner, CleanupDeadline()) is True
                    and owner.evidence["closure_errors"] == ["browser_contexts_unobserved"]
                    and owner.evidence["uncertain"] is True and owner.evidence["root_deleted"] is False
                    and owner.evidence["surviving_children"] == owner.evidence["surviving_listeners"] == 0
                    and owned_root_removable(owner.evidence), "offline_aborted_browser_closure_removable")
            require(close_owned_resources(aborted_owner(), CleanupDeadline()) is False, "offline_clean_closure")
            for refused in (aborted_owner("browser_contexts_unobserved", survive=True),
                            aborted_owner("browser_contexts_unobserved", listener=True),
                            aborted_owner("browser_contexts_unobserved", listener_error=True),
                            aborted_owner("browser_contexts_unobserved", "cleanup_deadline"),
                            aborted_owner("browser_contexts_unobserved", "cleanup_incomplete"),
                            aborted_owner("cleanup_incomplete"), aborted_owner("child_cleanup"),
                            aborted_owner(survive=True)):
                try:
                    close_owned_resources(refused, CleanupDeadline())
                except CleanupFailure as failure:
                    require(failure.evidence is refused.evidence and not owned_root_removable(failure.evidence),
                            "offline_unattested_closure_not_removable")
                else:
                    return False
            for evidence in (None, [], {}, {"closure_errors": []}, {"closure_errors": ["browser_contexts_unobserved"]},
                             {**owner.evidence, "unobserved_children": 1}, {**owner.evidence, "surviving_listeners": None},
                             {**owner.evidence, "observed_surviving_children": 1}, {**owner.evidence, "surviving_children": True},
                             {**owner.evidence, "closure_errors": ["browser_contexts_unobserved", "browser_contexts_unobserved"]}):
                require(not owned_root_removable(evidence), "offline_removable_predicate_strict")
            # The runtime must keep ordering: close (tolerating only the removable case), THEN delete,
            # and only a certain closure can make cleanup True. Bind the shipped source to that shape.
            runtime_source = inspect.getsource(runtime)
            require(runtime_source.index("close_owned_resources(cleanup, deadline)")
                    < runtime_source.index("remove_owned_tree(root, root_identity, deadline)")
                    < runtime_source.index('receipt["cleanup"] = not closure_uncertain')
                    and 'failure = failure or "cleanup_uncertain"' in runtime_source, "offline_runtime_cleanup_order")
            now[0] = 0
            reserved = CleanupDeadline(clock=clock, reserve=1)
            now[0] = 29
            require(rejects(reserved.check), "offline_receipt_reserve_work_refusal")
            reserved.reserve = 0
            reserved.check()  # Same end, only the reserved serialization window.
            now[0] = 30
            require(rejects(reserved.check), "offline_receipt_deadline_refusal")
            return True
        def cleanup_entry_observation_cases():
            # UNEXECUTED: injected clock/lock observations use the real extracted
            # snapshot and runtime cleanup seam; no timers, threads, files or
            # sockets. A lock may consume time, refuse, or be interrupted.
            for consumed, interruption, latest in ((7, None, 140), (4, RuntimeError, 140),
                    (29, Refusal, 140), (31, None, 140), (7, None, 106)):
                now, acquisitions = [100.0], []
                clock = lambda: now[0]
                cleanup = Cleanup()
                deadline = CleanupDeadline(clock=clock, started=now[0], latest=latest, reserve=1)
                end = min(130, latest)
                counters = helper.SocketCounters()
                counters.record("ping", "succeeded", "ordinary")
                class ObservationLock:
                    def __enter__(self):
                        acquisitions.append(True)
                        now[0] += consumed
                        if interruption is not None:
                            raise interruption("cleanup_deadline" if interruption is Refusal else "inert observation")
                    def __exit__(self, *_):
                        pass
                # operation_summary nests the RLock; keep the real counter
                # renderer with its original inert lock, and inject only the
                # outer snapshot's potentially blocking acquisition.
                state = SimpleNamespace(counters=SimpleNamespace(lock=ObservationLock(),
                    operation_summary=counters.operation_summary, reasons=counters.reasons, counts=counters.counts,
                    **{key: getattr(counters, key) for key in SOCKET_EXCEPTION_BUCKETS}, saturated=False),
                    thread=SimpleNamespace(ident=1, is_alive=lambda: True),
                    stopped=SimpleNamespace(is_set=lambda: False), workers=[], listener=None)
                state.diagnostic_summary = lambda: helper.socket_diagnostic_snapshot(state)
                receipt = {"result": "pass", "cleanup": True,
                    "fixture_operations": {"fake": [], "socket": counters.operation_summary()}}
                obtained = cleanup_socket_observation(receipt, state, cleanup, deadline, initial=True)
                require(deadline.end == end and acquisitions == [True], "offline_initial_observation_anchored")
                if consumed == 7 and latest == 140:
                    require(obtained and deadline.remaining(30) == 22, "offline_snapshot_consumes_cleanup_time")
                    cleanup.close(deadline)
                    require_socket_cleanup_evidence(receipt, state)
                    # A late publication lock consumes the same remaining time.
                    state.counters.lock = ObservationLock()
                    require(cleanup_socket_observation(receipt, state, cleanup, deadline)
                            and deadline.end == end and deadline.remaining(30) == 15,
                            "offline_late_observation_same_end")
                else:
                    require(not obtained and receipt["result"] == "fail" and receipt["cleanup"] is False
                            and receipt["failure"] == "cleanup_uncertain" and cleanup.evidence["uncertain"]
                            and "socket_diagnostics" not in receipt
                            and receipt["fixture_operations"]["socket"] == counters.operation_summary()
                            and rejects(lambda: require_socket_cleanup_evidence(receipt, state)),
                            "offline_unavailable_snapshot_no_invented_evidence")
                    child = InertChild()
                    cleanup.child(child)
                    require(rejects(lambda: cleanup.close(deadline)), "offline_partial_cleanup_uncertain")
                    require(child.events == (["INT", "wait3", "reap2"] if consumed == 4 else [])
                            and cleanup.evidence["root_deleted"] is False, "offline_remaining_time_only_cleanup")
                    require(not cleanup_socket_observation(receipt, state, cleanup, deadline)
                            and acquisitions == [True], "offline_no_initial_observation_retry")
            # Valid initial evidence + unavailable late publication observation
            # is a coherent FAIL, with sticky diagnostics and cleanup uncertainty.
            now = [0.0]
            cleanup, counters = Cleanup(), helper.SocketCounters()
            counters.record("ping", "succeeded", "ordinary")
            state = SimpleNamespace(counters=counters, thread=SimpleNamespace(ident=1, is_alive=lambda: False),
                stopped=SimpleNamespace(is_set=lambda: True), workers=[], listener=None)
            state.diagnostic_summary = lambda: helper.socket_diagnostic_snapshot(state)
            deadline = CleanupDeadline(clock=lambda: now[0], started=0, reserve=1)
            sources = {path: "1" * 64 for path in FILES}
            identity = {"sha": BASE, "run": 1, "artifact": 1, "zip_sha256": "2" * 64, "fresh": True}
            receipt = {"schemaVersion": 1, "candidate_sha": BASE, "source_hashes": sources,
                "producer": identity, "scope": "darwin_arm64_byo_only", "result": "pass", "cleanup": True,
                "owned_cleanup": cleanup.evidence, "request_counts": [], "captured_output_bytes": 0,
                "dependency_payload_bytes_peak": 0}
            require(cleanup_socket_observation(receipt, state, cleanup, deadline, initial=True), "offline_partial_snapshot")
            class UnavailableLock:
                def __enter__(self):
                    now[0] += 4
                    raise Refusal("cleanup_deadline")
                def __exit__(self, *_):
                    pass
            counters.lock = UnavailableLock()
            require(not cleanup_socket_observation(receipt, state, cleanup, deadline)
                    and receipt["result"] == "fail" and receipt["cleanup"] is False
                    and deadline.end == 30 and deadline.remaining(30) == 25,
                    "offline_partial_refresh_revokes_pass")
            require_socket_cleanup_evidence(receipt, state)
            require(receipt_policy(encode(receipt), BASE, sources, identity) is False, "offline_partial_failure_receipt")
            class Staged:
                def lstat(self):
                    return SimpleNamespace(st_mode=stat.S_IFREG, st_nlink=1, st_size=len(encode(receipt)),
                        st_dev=1, st_ino=2, st_mtime_ns=3)
                def open(self, mode):
                    require(mode == "rb", "offline_partial_staging_read")
                    return io.BytesIO(encode(receipt))
            class Published(io.BytesIO):
                def open(self, mode):
                    require(mode == "xb", "offline_partial_publication_write")
                    return self
                def close(self):
                    pass
            output = Published()
            deadline.reserve = 0
            require(not publish_receipt(Staged(), output, 1, BASE, 1, sources, identity, deadline)
                    and strict_json(output.getvalue())["result"] == "fail" and deadline.end == 30,
                    "offline_partial_publication_never_pass")
            return True
        def socket_snapshot_cleanup(early_death=False):
            # Same capture/finish and owner close as runtime, entirely duck-typed
            # memory state. No HerdrSocketFixture, socket, thread or file exists.
            stopped, alive, descriptor, observations = [False], [not early_death], [3], []
            counters = helper.SocketCounters()
            if not early_death:
                counters.record("other", "rejected", "unsupported_method")
            class Stop:
                def is_set(self):
                    return stopped[0]
                def set(self):
                    stopped[0] = True
            class Thread:
                ident = 1
                def is_alive(self):
                    return alive[0]
                def join(self, _timeout):
                    alive[0] = False
            class Listener:
                def close(self):
                    descriptor[0] = -1
                def fileno(self):
                    return descriptor[0]
            fixture = SimpleNamespace(counters=counters, lock=counters.lock, thread=Thread(), stopped=Stop(),
                listener=Listener(), workers=[], connections=set(), path=SimpleNamespace(unlink=lambda **_kwargs: None))
            def snapshot():
                observations.append(stopped[0])
                return helper.socket_diagnostic_snapshot(fixture)
            fixture.diagnostic_summary = snapshot
            receipt = {}
            diagnostics = capture_socket_diagnostics(receipt, fixture)
            before = strict_json(encode(diagnostics), "receipt")
            require(observations == [False] and diagnostics["closed_after_cleanup"] == "not_observed"
                    and diagnostics["liveness_before_cleanup"] == ("early_death" if early_death else "serving")
                    and diagnostics["guard"] == ("listener_not_alive" if early_death else "unexpected_pair"),
                    "offline_before_cleanup_snapshot")
            owner = SocketOwner(fixture)
            if early_death:
                require(rejects(lambda: owner.close(CleanupDeadline(clock=lambda: 0.0))), "offline_early_death_owner_refusal")
            else:
                owner.close(CleanupDeadline(clock=lambda: 0.0))
            # A late terminal row changes live counters, never the already
            # captured rows/reasons or their matching receipt socket rows.
            counters.record("ping", "succeeded", "ordinary")
            finish_socket_diagnostics(diagnostics, owner)
            require(observations == [False] and stopped[0] and diagnostics == {**before, "closed_after_cleanup": "closed"}
                    and diagnostics["rows"] == receipt["fixture_operations"]["socket"]
                    and helper.socket_diagnostic_snapshot(fixture)["liveness_before_cleanup"] == "stopped"
                    and helper.socket_diagnostic_snapshot(fixture)["rows"] != diagnostics["rows"],
                    "offline_after_cleanup_consistency")
            require(socket_diagnostics_policy(diagnostics)
                    and rejects(lambda: socket_diagnostics_policy(diagnostics, passing=True, operation_rows=diagnostics["rows"])),
                    "offline_failure_snapshot_not_pass")
            finish_socket_diagnostics(diagnostics, SimpleNamespace(closed=lambda: False))
            require(diagnostics["closed_after_cleanup"] == "open", "offline_open_after_cleanup")
            finish_socket_diagnostics(diagnostics, SimpleNamespace(closed=lambda: (_ for _ in ()).throw(RuntimeError("inert private error"))))
            require(diagnostics["closed_after_cleanup"] == "not_observed", "offline_closure_unobserved")
            return True
        def publication_cleanup_cases(closure_error=False):
            # Model a recorded terminal response whose sole publication callback
            # is still pending when the parent snapshots. Retirement acquires
            # its fault; no real thread, socket, fixture or filesystem is used.
            for saturate in (False, True):
                stopped, alive, descriptor, events = [False], [True, True], [3], []
                counters = helper.SocketCounters()
                counters.record("ping", "succeeded", "ordinary")
                class Stop:
                    def is_set(self):
                        return stopped[0]
                    def set(self):
                        stopped[0] = True
                def failed_callback():
                    events.append("callback")
                    raise OSError("/private/inert-token-never-recorded")
                class Thread:
                    def __init__(self, index):
                        self.index, self.ident = index, index + 1
                    def is_alive(self):
                        return alive[self.index]
                    def join(self, _timeout):
                        events.append("worker_join" if self.index else "listener_join")
                        if self.index:
                            if saturate:
                                # Pure boundary state, followed by actual typed
                                # acquisition at COUNT_MAX during retirement.
                                with counters.lock:
                                    counters.publication_exceptions["os_error"] = COUNT_MAX - 1
                            try:
                                failed_callback()
                            except Exception as error:
                                counters.record_exception("publication_exceptions", error)
                            events.append("later_callback")  # A successful later callback cannot clear the bucket.
                        alive[self.index] = False
                        if self.index and closure_error:
                            raise RuntimeError("/private/inert-token-never-recorded")
                class Listener:
                    def close(self):
                        events.append("listener_close")
                        descriptor[0] = -1
                    def fileno(self):
                        return descriptor[0]
                def unlink(**_kwargs):
                    events.append("unlink")
                fixture = SimpleNamespace(counters=counters, lock=counters.lock, thread=Thread(0), stopped=Stop(),
                    listener=Listener(), workers=[Thread(1)], connections=set(), path=SimpleNamespace(unlink=unlink))
                fixture.diagnostic_summary = lambda: helper.socket_diagnostic_snapshot(fixture)
                receipt = {}
                diagnostics = capture_socket_diagnostics(receipt, fixture)
                before = strict_json(encode(diagnostics), "receipt")
                require(before["publication_exceptions"] == {} and before["saturated"] is False
                        and before["liveness_before_cleanup"] == "serving" and before["guard"] == "allowed",
                        "offline_publication_before_retirement")
                cleanup, child, owner = Cleanup(), InertChild(), SocketOwner(fixture)
                other_listener = InertListener()
                cleanup.child(child)
                cleanup.listener(other_listener)
                cleanup.listener(owner)
                operations = Operations(Path("/must-not-be-accessed"), {}, Budget(lambda: 0.0), cleanup, EffectGate(False))
                operations.add_guard("herdr_socket", lambda: socket_fixture_allowed(fixture, receipt))
                operations.check_guards()
                try:
                    cleanup.close(CleanupDeadline(clock=lambda: 0.0))
                except CleanupFailure as error:
                    require(closure_error and error.evidence is cleanup.evidence, "offline_publication_cleanup_failure")
                else:
                    require(not closure_error, "offline_publication_cleanup_error_not_suppressed")
                finally:
                    finish_socket_diagnostics(diagnostics, owner)
                expected_count = COUNT_MAX if saturate else 1
                expected_guard = "counter_saturated" if saturate else "recorded_exception"
                expected_events = ["listener_close", "listener_join", "worker_join", "callback", "later_callback"]
                if not closure_error:
                    expected_events.append("unlink")
                require(events == expected_events
                        and child.events == ["INT", "wait3", "reap2"] and child.done() and owner.closed() and other_listener.closed()
                        and cleanup.evidence["uncertain"] is closure_error
                        and cleanup.evidence["closure_errors"] == (["listener_cleanup"] if closure_error else [])
                        and cleanup.evidence["surviving_children"] == cleanup.evidence["surviving_listeners"] == 0,
                        "offline_publication_fault_keeps_owned_cleanup")
                require(diagnostics == {**before, "publication_exceptions": {"os_error": expected_count},
                        "saturated": saturate, "closed_after_cleanup": "closed", "guard": expected_guard}
                        and diagnostics["rows"] == receipt["fixture_operations"]["socket"]
                        and counters.handled_connections == sum(counters.counts.values()) == sum(counters.reasons.values()) == 1
                        and counters.handler_exceptions == counters.stream_exceptions == counters.listener_exceptions == {},
                        "offline_retirement_fault_refresh_not_second_terminal")
                require(socket_diagnostics_policy(diagnostics)
                        and rejects(lambda: socket_diagnostics_policy(diagnostics, passing=True, operation_rows=diagnostics["rows"]))
                        and not socket_fixture_allowed(fixture, receipt) and rejects(operations.check_guards)
                        and operations.failure_guard == "herdr_socket", "offline_actual_late_callback_guard_refuses")
                finish_socket_diagnostics(diagnostics, owner)  # Final serialization refresh is monotonic too.
                require(diagnostics["publication_exceptions"] == {"os_error": expected_count}
                        and diagnostics["guard"] == expected_guard and counters.handled_connections == 1,
                        "offline_final_publication_refresh_sticky")
                unobserved = SimpleNamespace(closed=lambda: (_ for _ in ()).throw(RuntimeError("/private/inert-token")),
                    publication_summary=owner.publication_summary)
                finish_socket_diagnostics(diagnostics, unobserved)
                require(diagnostics["closed_after_cleanup"] == "not_observed"
                        and diagnostics["publication_exceptions"] == {"os_error": expected_count}
                        and diagnostics["saturated"] is saturate and diagnostics["liveness_before_cleanup"] == "serving"
                        and b"/private" not in encode(diagnostics) and b"inert-token" not in encode(diagnostics),
                        "offline_unobserved_closure_keeps_callback_fault")
            return True
        def late_socket_fault_cases():
            # A held-stream/listener/handler fault or non-allowed row arriving
            # after the before-cleanup snapshot is carried into the final guard
            # reason; captured rows/reasons/liveness stay immutable. Inert state.
            # Both a clean snapshot and one already refused (unexpected_pair):
            # the final reason follows the live guard's priority, so a late
            # handler/stream/listener exception still wins over a pair refusal.
            for refused in (False, True):
                for kind, late_guard in (("stream", "recorded_exception"), ("listener", "recorded_exception"),
                        ("handler", "recorded_exception"), ("row", "unexpected_pair")):
                    counters = helper.SocketCounters()
                    counters.record("events.subscribe", "succeeded", "ordinary")
                    if refused:
                        counters.record("other", "rejected", "unsupported_method")
                    fixture = SimpleNamespace(counters=counters, thread=SimpleNamespace(ident=1, is_alive=lambda: True),
                        stopped=SimpleNamespace(is_set=lambda: False))
                    diagnostics = helper.socket_diagnostic_snapshot(fixture)
                    diagnostics.update(closed_after_cleanup="not_observed", guard=socket_diagnostic_guard(diagnostics))
                    before = strict_json(encode(diagnostics), "receipt")
                    require(before["guard"] == ("unexpected_pair" if refused else "allowed")
                            and before["liveness_before_cleanup"] == "serving", "offline_late_fault_before_snapshot")
                    if kind == "stream":
                        counters.record_exception("stream_exceptions", ConnectionResetError("/private/inert-token"))
                    elif kind == "listener":
                        counters.record_exception("listener_exceptions", OSError("/private/inert-token"))
                    elif kind == "handler":
                        counters.record("other", "failed", "recv_failed", TimeoutError("/private/inert-token"))
                    else:
                        counters.record("other", "rejected", "incomplete_request")
                    owner = SimpleNamespace(closed=lambda: True,
                        publication_summary=SocketOwner(SimpleNamespace(counters=counters)).publication_summary)
                    for _ in range(2):  # Final serialization refresh is monotonic too.
                        finish_socket_diagnostics(diagnostics, owner)
                        expected = {**before, "closed_after_cleanup": "closed", "guard": late_guard}
                        if kind == "stream":
                            expected["stream_exceptions"] = {"connection_reset": 1}
                        elif kind == "listener":
                            expected["listener_exceptions"] = {"os_error": 1}
                        require(diagnostics == expected and socket_diagnostics_policy(diagnostics)
                                and rejects(lambda: socket_diagnostics_policy(diagnostics, passing=True,
                                    operation_rows=diagnostics["rows"]))
                                and b"/private" not in encode(diagnostics) and b"inert-token" not in encode(diagnostics),
                                "offline_late_socket_fault_guard_reason")
            return True
        def owner_close_stream_cases():
            # Production seam, inert memory state: a successful subscription's
            # publication callback is pending when the owner begins closing the
            # connection; the held-stream receive then runs on the closed
            # descriptor. That EBADF is the owner's own retirement, not a
            # fault. Genuine live errors (even after the retirement flag) and
            # non-OSError faults stay recorded.
            scenario = helper.socket_scenario_view({})
            raw = encode({"id": "mobile-relay-events", "method": "events.subscribe",
                          "params": {"subscriptions": []}}) + b"\n"
            def run(retiring_enabled, owner_closes, stream_error):
                counters, source, closed, stopped = helper.SocketCounters(), io.BytesIO(raw), [False], [False]
                def recv(maximum):
                    if closed[0]:
                        raise OSError(errno.EBADF, "inert closed descriptor")
                    if source.tell() >= len(raw) and stream_error is not None:
                        raise stream_error("/private/inert-token-never-recorded")
                    return source.read(maximum)
                def terminal():  # The owner's close happens while this callback is pending.
                    if owner_closes:  # "flag": retirement signalled, descriptor still live (SocketOwner.close order).
                        stopped[0], closed[0] = True, owner_closes == "close"
                helper.handle_socket_connection(recv, lambda data: None, scenario, counters,
                    set_timeout=lambda value: None, on_terminal=terminal,
                    retiring=(lambda: stopped[0]) if retiring_enabled else None)
                return counters
            quiet = run(True, "close", None)
            require(quiet.stream_exceptions == quiet.publication_exceptions == quiet.handler_exceptions
                    == quiet.listener_exceptions == {} and quiet.counts == {("events.subscribe", "succeeded"): 1}
                    and quiet.reasons == {"ordinary": 1} and quiet.handled_connections == 1,
                    "offline_owner_close_not_stream_fault")
            snapshot = helper.socket_diagnostic_snapshot(SimpleNamespace(counters=quiet,
                thread=SimpleNamespace(ident=1, is_alive=lambda: True), stopped=SimpleNamespace(is_set=lambda: False)))
            require(socket_diagnostic_guard(snapshot) == "allowed", "offline_owner_close_guard_allowed")
            control = run(False, "close", None)  # Without the retirement signal the same race is a recorded fault.
            require(control.stream_exceptions == {"os_error": 1}, "offline_owner_close_control_records")
            live = run(True, False, ConnectionResetError)  # Genuine error while NOT retiring stays fail-closed.
            require(live.stream_exceptions == {"connection_reset": 1}, "offline_live_stream_error_recorded")
            # Retirement signalled but the descriptor is still live: a genuine reset
            # (or any other OSError) is NOT owner-induced and must be recorded.
            flagged = run(True, "flag", ConnectionResetError)
            require(flagged.stream_exceptions == {"connection_reset": 1}, "offline_live_reset_after_retirement_recorded")
            flagged_io = run(True, "flag", lambda text: OSError(errno.EIO, text))
            require(flagged_io.stream_exceptions == {"os_error": 1}, "offline_live_oserror_after_retirement_recorded")
            snapshot = helper.socket_diagnostic_snapshot(SimpleNamespace(counters=flagged,
                thread=SimpleNamespace(ident=1, is_alive=lambda: True), stopped=SimpleNamespace(is_set=lambda: False)))
            require(socket_diagnostic_guard(snapshot) == "recorded_exception", "offline_live_reset_guard_refuses")
            def raises_non_os(retiring):
                counters = helper.SocketCounters()
                source = io.BytesIO(raw)
                def recv(maximum):
                    if source.tell() >= len(raw):
                        raise ValueError("/private/inert-token-never-recorded")
                    return source.read(maximum)
                helper.handle_socket_connection(recv, lambda data: None, scenario, counters,
                    set_timeout=lambda value: None, retiring=retiring)
                return counters
            require(sum(raises_non_os(lambda: True).stream_exceptions.values()) == 1,
                    "offline_non_os_stream_error_while_retiring_recorded")
            fixture_source = inspect.getsource(helper.HerdrSocketFixture._handle)
            require("retiring=self.stopped.is_set" in fixture_source, "offline_fixture_wires_retirement_signal")
            return True
        operations = {"ordered_success": cleanup_case, "failure_cleanup": lambda: cleanup_case(handled=True) and socket_snapshot_cleanup()
                and publication_cleanup_cases() and late_socket_fault_cases() and owner_close_stream_cases(),
            "handled_signal_cleanup": lambda: cleanup_case(handled=True),
            "bounded_escalation_and_reap": lambda: cleanup_case(escalation=True) and aggregate_and_deletion_deadlines()
                and cleanup_entry_observation_cases(),
            "unowned_ids_refused": lambda: rejects(lambda: Cleanup().stop(InertChild())),
            "surviving_children_fail": lambda: cleanup_case(survive=True),
            "surviving_listener_fail": lambda: cleanup_case(listener_survives=True),
            "closure_error_fails": lambda: cleanup_case(error=True) and socket_snapshot_cleanup(early_death=True)
                and publication_cleanup_cases(closure_error=True),
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

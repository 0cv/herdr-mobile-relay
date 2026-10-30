#!/usr/bin/env python3
"""Offline qualification gate for one exact Darwin extracted-package browser candidate.

Qualified ONLY when the supplied `gh run list` projection contains exactly one
successful first-attempt run for each of the three required workflows on
the exact SHA/branch AND the supplied Darwin receipt is a complete PASS under
the runner's own receipt contract (tests/test_tailscale_darwin_package_browser.py
receipt_values_safe/receipt_policy/publish_receipt), bound to that Darwin run,
to the exact SHA, to the applicable pinned launchctl rule, and to the SHA256
of the four runner source blobs at that SHA. The integration rule additionally
requires the candidate commit's immutable tree and exact ordered parents.

This tool never observes GitHub, downloads, commits, publishes or runs the
candidate. Normal mode only reads the two JSON files you name and (read-only)
four git blobs; --self-test uses embedded fixtures and no git/network/files.
Diagnostics are fixed codes: no input values, errors, paths or secrets.
Self-test output is synthetic offline evidence, never hosted qualification.

usage:
  qualify-darwin-package-browser.py --sha SHA --runs RUNS.json --receipt RECEIPT.json
                                    [--repo DIR] [--reviewed-hashes HASHES.json]
  qualify-darwin-package-browser.py --self-test
RUNS.json is: gh run list --branch ci/tailscale-native-preflight-ef9f843 --commit SHA \\
  --json databaseId,workflowName,headSha,headBranch,event,status,conclusion,attempt
The check run must have event pull_request; native preflight and Darwin must have event push.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import math
import os
import re
import select
import stat
import subprocess
import sys
import time
from types import MappingProxyType

BRANCH = "ci/tailscale-native-preflight-ef9f843"
WORKFLOWS = (
    ("check", "check"),
    ("native_preflight", "Tailscale native preflight"),
    ("darwin", "Darwin extracted-package browser subcell"),
)
RUN_FIELDS = frozenset({"databaseId", "workflowName", "headSha", "headBranch", "event", "status",
                        "conclusion", "attempt"})
# The runner's receipt-hashed files (FILES in the Darwin runner). The gate script
# itself is part of the candidate change set but is NOT a receipt-hashed file.
FILES = (
    ".github/workflows/tailscale-darwin-package-browser.yml",
    "tests/test_tailscale_darwin_package_browser.py",
    "tests/test_tailscale_extracted_package_browser.py",
    "tests/tailscale-package-browser.mjs",
)
# The q3 receipt identity remains the pinned scope base, not a single parent.
# The separate integration identity is bound to the exact ordered commit parents.
GRANT_ID = "q3-roadmap-launchctl-print-ff-c77855cb-20261014"
PINNED_PARENT = "c77855cb4e779c00a85d194f5bffe72ac3016554"
INTEGRATION_GRANT_ID = "q4-owner-hosted-integration-9aed9d04-27054011-20261030"
INTEGRATION_BEFORE = "9aed9d0464ed28e241608cb9365d26f064a31cf9"
INTEGRATION_MAIN = "27054011fd8137d5f9702cc9e718aff1bd73443d"
# The exact Python the workflow pins (actions/setup-python); the receipt must record it,
# like Node, Bun and Go, in lockstep with PYTHON_VERSION in the Darwin runner.
PYTHON_VERSION = "3.14.0"
PHASES = (90, 300, 210, 630, 30)
TOTAL_SECONDS = 1260
RAW_CAP = 2 * 1024 * 1024
DEPENDENCY_CAP = 1024 * 1024 * 1024
PROTOCOL_CAP = 65536
COUNT_MAX = 65535
PROBE_CAP = 64
RUNS_CAP, RECEIPT_CAP, HASHES_CAP, BLOB_CAP = 1024 * 1024, 262144, 65536, 8 * 1024 * 1024
ENROLL = (
    "dev_setup_link_imports_bare_wss_origin_in_extracted_frontend",
    "launcher_generated_setup_link_enrolls_real_controller_profile",
    "controller_reads_fake_inventory_and_sends_harmless_command",
    "second_persistent_profile_enrolls_reader_and_read_only_is_enforced",
)
PRESERVE = ("reprint_and_byo_restart_preserve_enrolled_device_credentials",)
ASSERT_COMMON = ("app_worker_verified", "notification_delivery_disabled", "launch_arguments_verified")
ASSERT_ENROLL = ("controller_enrolled", "controller_read", "controller_command", "reader_enrolled",
                 "reader_read", "reader_mutation_denied", "credentials_preserved") + ASSERT_COMMON
ASSERT_PRESERVE = ("controller_enrolled", "controller_read", "reader_enrolled", "reader_read",
                   "credentials_preserved") + ASSERT_COMMON
SOCKET_ORDINARY = frozenset({"ping", "agent.list", "pane.list", "workspace.list", "tab.list",
                             "session.snapshot", "events.subscribe", "pane.read", "pane.get", "pane.process_info"})
SOCKET_PROBES = frozenset({"workspace.move_block", "tab.move", "pane.read"})
SOCKET_PAIRS = frozenset(
    {(m, o) for m in SOCKET_ORDINARY - {"pane.read"} for o in ("succeeded", "failed")}
    | {("pane.read", o) for o in ("succeeded", "refused_expected", "rejected", "failed")}
    | {(m, o) for m in SOCKET_PROBES - {"pane.read"} for o in ("refused_expected", "rejected", "failed")}
    | {("other", "rejected"), ("other", "failed")})
SOCKET_METHODS = SOCKET_ORDINARY | {"workspace.move_block", "tab.move", "other"}
SOCKET_OUTCOMES = frozenset({"succeeded", "refused_expected", "rejected", "failed"})
SOCKET_BUCKETS = ("handler_exceptions", "stream_exceptions", "listener_exceptions", "publication_exceptions")
# Mirrors the runner's exact enumerated READ-ONLY Herdr CLI queries (FAKE_READ_ONLY in
# tests/test_tailscale_darwin_package_browser.py) plus the single harmless prompt. Read-only
# rows may be started/succeeded/failed; the prompt must be exactly one succeeded row.
FAKE_READ_ONLY_COMMANDS = frozenset({"agent list", "pane list", "workspace list", "tab list", "pane read",
    "integration status", "agent get", "pane process-info", "worktree list"})
FAKE_PROMPT = "agent prompt"
FAKE_COMMANDS = FAKE_READ_ONLY_COMMANDS | {FAKE_PROMPT}
FAKE_OUTCOMES = frozenset({"started", "succeeded", "failed"})
FAKE_ROW_CAP = len(FAKE_COMMANDS) * len(FAKE_OUTCOMES)
PASS_FIELDS = frozenset({
    "schemaVersion", "candidate_sha", "candidate_tree", "source_hashes", "result", "scope", "producer",
    "archive_hashes", "version", "manifest_sha256", "binary_sha256", "compiler", "browser_identity", "tools",
    "source_proof", "profile_inputs", "profile_policy", "service_guard", "trust_controls", "cases", "phases",
    "cleanup", "owned_cleanup", "request_counts", "fixture_operations", "captured_output_bytes",
    "dependency_payload_bytes_peak", "launchctl_query_grant_id", "github_run_id", "github_run_attempt",
    "launchctl_query_pinned_parent", "socket_diagnostics"})
INTEGRATION_FIELDS = frozenset({"integration_before", "integration_parents"})
DIGEST = re.compile(r"[0-9a-f]{64}")
REVISION = re.compile(r"[0-9a-f]{40}")
VERSION = re.compile(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}")
TOOL_VERSION = re.compile(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}[a-z]?")
RUN_ID_MAX = 10**15 - 1


class Refusal(RuntimeError):
    """Carries only a fixed code, never an input value."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code


def require(condition, code):
    if not condition:
        raise Refusal(code)


def is_int(value, low=None, high=None):
    return (type(value) is int and (low is None or value >= low) and (high is None or value <= high))


def is_number(value, high):
    return type(value) in (int, float) and 0 <= value < high


def is_digest(value):
    return type(value) is str and DIGEST.fullmatch(value) is not None


def is_exact(value, keys):
    return type(value) is dict and set(value) == set(keys)


def strict_json(data, maximum):
    require(type(data) is bytes and len(data) <= maximum, "input_cap")

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate_json_key")
            result[key] = value
        return result

    def constant(_):
        raise Refusal("json_constant")

    def real(text):
        value = float(text)
        require(math.isfinite(value), "json_nonfinite")
        return value

    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=pairs, parse_constant=constant, parse_float=real)
    except (ValueError, UnicodeError, RecursionError):
        raise Refusal("json_invalid") from None


def read_file(path, maximum):
    try:
        require(type(path) is str and path, "input_unavailable")
        info = os.lstat(path)
        require(stat.S_ISREG(info.st_mode) and info.st_size <= maximum, "input_file")
        with open(path, "rb") as stream:
            data = stream.read(maximum + 1)
    except OSError:
        raise Refusal("input_unavailable") from None
    require(len(data) <= maximum, "input_cap")
    return data


# ---------------------------------------------------------------- run evidence

def check_runs(sha, runs):
    """gh run list projection -> (failure codes, Darwin databaseId or None)."""
    if type(runs) is not list or len(runs) > 1000:
        return ["runs_shape"], None
    for row in runs:
        # databaseId/attempt are typed per required run below (a bool is never an ID).
        if not is_exact(row, RUN_FIELDS) or not all(
                type(row[key]) is str for key in ("workflowName", "headSha", "headBranch", "event", "status", "conclusion")):
            return ["runs_row_shape"], None
    failures, darwin_id = [], None
    for key, name in WORKFLOWS:
        # Never filter by SHA/branch/event first: a wrong-context or rerun row
        # for a required workflow is itself a duplicate/failed observation.
        matching = [row for row in runs if row["workflowName"] == name]
        if not matching:
            failures.append("run_missing_" + key)
            continue
        if len(matching) > 1:
            failures.append("run_duplicate_" + key)
            continue
        row = matching[0]
        valid_id = is_int(row["databaseId"], 1, RUN_ID_MAX)
        for ok, reason in ((valid_id, "id"), (row["headSha"] == sha, "sha"), (row["headBranch"] == BRANCH, "branch"),
                           (row["event"] == ("pull_request" if key == "check" else "push"), "event"),
                           (row["status"] == "completed", "status"),
                           (row["conclusion"] == "success", "conclusion"), (is_int(row["attempt"], 1, 1), "attempt")):
            if not ok:
                failures.append(f"run_{reason}_{key}")
        if key == "darwin" and valid_id:
            darwin_id = row["databaseId"]
    return failures, darwin_id


# ------------------------------------------------------------ receipt contract

def sanitized(item, depth=0):
    """Finite public protocol: no secret-looking keys, URLs, fragments or PEM."""
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


def socket_pair_guard(counts):
    for pair, count in counts.items():
        if pair not in SOCKET_PAIRS or not is_int(count, 1, COUNT_MAX):
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


def socket_rows(rows):
    require(type(rows) is list and len(rows) <= len(SOCKET_PAIRS), "receipt_socket_rows")
    counts, order = {}, []
    for row in rows:
        require(is_exact(row, ("method", "outcome", "count")) and type(row["method"]) is str
                and type(row["outcome"]) is str and row["method"] in SOCKET_METHODS
                and row["outcome"] in SOCKET_OUTCOMES, "receipt_socket_rows")
        pair = (row["method"], row["outcome"])
        require(pair in SOCKET_PAIRS and pair not in counts and is_int(row["count"], 1, COUNT_MAX), "receipt_socket_rows")
        order.append(pair)
        counts[pair] = row["count"]
    require(order == sorted(order), "receipt_socket_rows")
    return counts


def check_socket(operations, diagnostics):
    counts = socket_rows(operations["socket"])
    require(socket_pair_guard(counts) == "allowed" and counts.get(("pane.read", "succeeded"), 0) >= 2
            and counts.get(("ping", "succeeded"), 0) > 0, "receipt_socket_operations")
    require(is_exact(diagnostics, ("schema", "rows", "reasons", *SOCKET_BUCKETS, "saturated",
                                   "liveness_before_cleanup", "closed_after_cleanup", "guard")), "receipt_socket_diagnostics")
    require(is_int(diagnostics["schema"], 1, 1) and diagnostics["rows"] == operations["socket"]
            and socket_rows(diagnostics["rows"]) == counts, "receipt_socket_diagnostics")
    reasons = diagnostics["reasons"]
    require(type(reasons) is dict and set(reasons) <= {"ordinary", "inert_probe"}
            and all(is_int(count, 1, COUNT_MAX) for count in reasons.values()), "receipt_socket_diagnostics")
    # The fixture records ordinary reasons only for succeeded rows and inert
    # probe reasons only for refused_expected rows, exactly once per connection.
    succeeded = sum(count for (_, outcome), count in counts.items() if outcome == "succeeded")
    refused = sum(count for (_, outcome), count in counts.items() if outcome == "refused_expected")
    require(reasons.get("ordinary", 0) == succeeded and reasons.get("inert_probe", 0) == refused
            and succeeded + refused == sum(counts.values()) < COUNT_MAX, "receipt_socket_diagnostics")
    require(all(diagnostics[bucket] == {} for bucket in SOCKET_BUCKETS), "receipt_socket_exceptions")
    require(diagnostics["saturated"] is False, "receipt_socket_saturated")
    require(diagnostics["liveness_before_cleanup"] == "serving" and diagnostics["closed_after_cleanup"] == "closed",
            "receipt_socket_liveness")
    require(diagnostics["guard"] == "allowed" and socket_pair_guard(counts) == "allowed", "receipt_socket_guard")


def check_trust(trust):
    require(is_exact(trust, ("python_curl_packaged_go", "browser")), "receipt_trust")
    browser = trust["browser"]
    require(is_exact(browser, ("ok", "observations", "argv_ok", "version", "contexts_closed"))
            and all(browser[key] is True for key in ("ok", "argv_ok", "contexts_closed"))
            and browser["version"] == "153.0.8010.12" and browser["observations"] == [
        "empty_profile_refuses_ca", "trusted_profile_accepts_ip", "trusted_profile_refuses_unknown_ca",
        "trusted_profile_refuses_wrong_host"], "receipt_browser_trust")
    controls = trust["python_curl_packaged_go"]
    require(type(controls) is list and len(controls) == 3, "receipt_tls")
    for expected, control in zip(("positive", "unknown_ca", "wrong_host"), controls):
        require(is_exact(control, ("category", "python", "curl", "packaged_go")) and control["category"] == expected,
                "receipt_tls_category")
        require(is_exact(control["python"], ("category", "verify_code")) and control["python"]["category"] == expected,
                "receipt_python_tls")
        verify = control["python"]["verify_code"]
        require(verify is None if expected == "positive" else
                (is_int(verify, 0, 1000) and verify in ({20, 21} if expected == "unknown_ca" else {64})),
                "receipt_python_tls")
        for tool in ("curl", "packaged_go"):
            entry = control[tool]
            require(is_exact(entry, ("category", "exit_code") + (("proof",) if tool == "packaged_go" else ()))
                    and entry["category"] == expected
                    and is_int(entry["exit_code"], -2147483648, 2147483647), "receipt_tls_tool")
        # The packaged Go control proves the trusted origin accepted, or an untrusted one refused (main's exit 1); never the category.
        require(control["packaged_go"]["proof"] == ("accepted" if expected == "positive" else "refusal_only"), "receipt_tls_proof")
        curl, golang = control["curl"]["exit_code"], control["packaged_go"]["exit_code"]
        require((curl == 0 and golang == 0) if expected == "positive" else (curl == 60 and golang == 1),
                "receipt_tls_exit")


def check_phases_and_cases(value):
    phases = value["phases"]
    require(type(phases) is list and len(phases) == 5, "receipt_phases")
    total = 0
    for index, phase in enumerate(phases):
        require(is_exact(phase, ("id", "ok", "exit_code", "cap_seconds", "seconds")) and is_int(phase["id"], index, index)
                and phase["ok"] is True and is_int(phase["exit_code"], 0, 0) and is_int(phase["cap_seconds"], PHASES[index],
                PHASES[index]) and is_number(phase["seconds"], PHASES[index]), "receipt_phase")
        total += phase["seconds"]
    require(total < TOTAL_SECONDS, "receipt_total")
    cases = value["cases"]
    require(type(cases) is list and len(cases) == 3, "receipt_cases")
    for mode, result in zip(("enroll", "reprint", "restart"), cases):
        require(is_exact(result, ("mode", "cases", "ok", "contexts_closed", "exit_code", "result_bytes", "assertions",
                                  "seconds", "stdin_bytes")), "receipt_case")
        expected = ENROLL if mode == "enroll" else PRESERVE
        require(result["mode"] == mode and result["cases"] == list(expected) and result["ok"] is True
                and result["contexts_closed"] is True and is_int(result["exit_code"], 0, 0), "receipt_case")
        names = ASSERT_ENROLL if mode == "enroll" else ASSERT_PRESERVE
        require(is_exact(result["assertions"], names) and all(v is True for v in result["assertions"].values()),
                "receipt_assertions")
        require(is_int(result["result_bytes"], 1, PROTOCOL_CAP) and is_int(result["stdin_bytes"], 1, PROTOCOL_CAP),
                "receipt_protocol_bytes")
        require(is_number(result["seconds"], 150), "receipt_case_seconds")


def check_identities(value):
    require(value["source_proof"] == {"chromium_commit": "971a7443b0c9b0a9b2860529b33331b76077ec62",
            "playwright_tag": "v1.63.0", "go_tag": "go1.27.3", "trust": "TRUST-PROOF.md"}, "receipt_source_proof")
    compiler = value["compiler"]
    require(is_exact(compiler, ("version", "target", "metadata_sha256")) and compiler["version"] == "go1.27.3"
            and compiler["target"] == "darwin/arm64" and is_digest(compiler["metadata_sha256"]), "receipt_compiler")
    browser = value["browser_identity"]
    require(is_exact(browser, ("sha256", "version", "revision", "playwright", "switches_sha256", "target"))
            and browser["version"] == "153.0.8010.12" and browser["revision"] == "1243"
            and browser["playwright"] == "1.63.0" and browser["target"] == "darwin/arm64"
            and is_digest(browser["sha256"]) and is_digest(browser["switches_sha256"]), "receipt_browser")
    tools = value["tools"]
    require(is_exact(tools, ("python", "node", "bun", "go", "curl", "openssl")) and tools["python"] == PYTHON_VERSION
            and tools["node"] == "26.13.0" and tools["bun"] == "1.4.4" and tools["go"] == "1.27.3"
            and all(type(v) is str and TOOL_VERSION.fullmatch(v) for v in tools.values()), "receipt_tools")
    policy = value["profile_policy"]
    require(is_exact(policy, ("schema", "root_der_sha256", "trust_blob", "platform_integration", "sandbox", "mock_keychain"))
            and is_int(policy["schema"], 1, 1) and policy["trust_blob"] == "0a020803"
            and policy["platform_integration"] is False and policy["sandbox"] is True
            and policy["mock_keychain"] is True and is_digest(policy["root_der_sha256"]), "receipt_profile_policy")
    profiles = value["profile_inputs"]
    require(is_exact(profiles, ("controller", "reader", "negative")), "receipt_profiles")
    for name, profile in profiles.items():
        if name == "negative":
            require(is_exact(profile, ("database_absent", "preferences_sha256")) and profile["database_absent"] is True
                    and is_digest(profile["preferences_sha256"]), "receipt_negative_profile")
        else:
            require(is_exact(profile, ("database_sha256", "preferences_sha256")) and is_digest(profile["database_sha256"])
                    and is_digest(profile["preferences_sha256"]), "receipt_profiles")
    for key in ("candidate_tree",):
        require(type(value[key]) is str and REVISION.fullmatch(value[key]), "receipt_tree")
    require(type(value["version"]) is str and VERSION.fullmatch(value["version"]), "receipt_version")
    archive_name = f"herdr-mobile-relay_{value['version']}_darwin_arm64.tar.gz"
    require(is_exact(value["archive_hashes"], (archive_name, "checksums.txt"))
            and all(is_digest(v) for v in value["archive_hashes"].values())
            and is_digest(value["manifest_sha256"]) and is_digest(value["binary_sha256"]), "receipt_archive")


def check_guard_and_cleanup(value):
    guard = value["service_guard"]
    require(is_exact(guard, ("uid", "label", "starts", "command", "launchctl_path", "launchctl_sha256",
                             "source_guard_sha256", "source_launcher_sha256", "observations"))
            and is_int(guard["uid"], 0, 2147483647) and guard["label"] == "com.herdr-mobile-relay.service"
            and is_int(guard["starts"], 2, 2) and guard["command"] == "launchctl_print_gui_label"
            and guard["launchctl_path"] == "/bin/launchctl"
            and all(is_digest(guard[key]) for key in ("launchctl_sha256", "source_guard_sha256", "source_launcher_sha256")),
            "receipt_guard")
    observations = guard["observations"]
    require(type(observations) is list and len(observations) == 2, "receipt_guard_observations")
    for index, item in enumerate(observations, 1):
        require(is_exact(item, ("ordinal", "private_plist_before_absent", "private_plist_after_absent", "setup_ready_seconds",
                                "loaded_service", "proof_kind", "query_exit_zero", "numeric_query_exit_observed"))
                and is_int(item["ordinal"], index, index) and item["private_plist_before_absent"] is True
                and item["private_plist_after_absent"] is True and item["loaded_service"] is False
                and item["proof_kind"] == "source_bound_wrapper_continued" and item["query_exit_zero"] is False
                and item["numeric_query_exit_observed"] is False and is_number(item["setup_ready_seconds"], 70),
                "receipt_guard_observation")
    require(value["cleanup"] is True, "receipt_cleanup")
    owned = value["owned_cleanup"]
    require(is_exact(owned, ("children", "listeners", "surviving_children", "surviving_listeners",
                             "observed_surviving_children", "observed_surviving_listeners", "unobserved_children",
                             "unobserved_listeners", "closure_errors", "uncertain", "root_deleted")), "receipt_cleanup")
    require(is_int(owned["children"], 1, 1000000) and is_int(owned["listeners"], 1, 1000000)
            and all(is_int(owned[key], 0, 0) for key in ("surviving_children", "surviving_listeners",
            "observed_surviving_children", "observed_surviving_listeners", "unobserved_children", "unobserved_listeners"))
            and owned["closure_errors"] == [] and owned["uncertain"] is False and owned["root_deleted"] is True,
            "receipt_cleanup")


def check_requests_and_operations(value):
    requests = value["request_counts"]
    require(type(requests) is list and len(requests) == 3, "receipt_requests")
    for request in requests:
        require(type(request) is dict and set(request) <= {"tls_accepted", "tls_refused", "http_get", "websocket",
                "websocket_refused", "client_aborted"} and all(is_int(v, 0, 1000000) for v in request.values()),
                "receipt_request_fields")
    require(requests[0].get("websocket", 0) >= 2 and requests[0].get("http_get", 0) > 0
            and requests[0].get("tls_refused", 0) > 0
            and all(r.get("tls_refused", 0) > 0 and r.get("http_get", 0) == 0 and r.get("websocket", 0) == 0
                    and r.get("websocket_refused", 0) == 0 for r in requests[1:]), "receipt_request_controls")
    operations = value["fixture_operations"]
    require(is_exact(operations, ("fake", "socket")) and type(operations["fake"]) is list
            and len(operations["fake"]) <= FAKE_ROW_CAP, "receipt_operations")
    seen = {}
    for row in operations["fake"]:
        # Type-check labels before any hashed membership: [] / {} are valid JSON but unhashable.
        require(is_exact(row, ("command", "outcome", "count")) and type(row["command"]) is str
                and row["command"] in FAKE_COMMANDS and type(row["outcome"]) is str
                and row["outcome"] in FAKE_OUTCOMES and (row["command"], row["outcome"]) not in seen
                and is_int(row["count"], 1, COUNT_MAX)
                and (row["command"] != FAKE_PROMPT or (row["outcome"] == "succeeded" and row["count"] == 1)),
                "receipt_fake_operation")
        seen[(row["command"], row["outcome"])] = row["count"]
    require(seen.get((FAKE_PROMPT, "succeeded")) == 1, "receipt_fake_mutation")
    check_socket(operations, value["socket_diagnostics"])


def validate_receipt(value, sha, run_id):
    """Closed PASS contract. Returns fixed failure codes (empty list is PASS)."""
    try:
        require(type(value) is dict, "receipt_type")
        sanitized(value)
        early = []
        if value.get("result") != "pass":
            early.append("receipt_not_pass")
        if any(key in value for key in ("failure", "failure_detail", "failure_guard", "proxy_failure")):
            early.append("receipt_failure_present")
        if early:
            return early
        integration = value.get("launchctl_query_grant_id") == INTEGRATION_GRANT_ID
        require(set(value) == PASS_FIELDS | (INTEGRATION_FIELDS if integration else set()), "receipt_fields")
        require(is_int(value["schemaVersion"], 1, 1) and value["scope"] == "darwin_arm64_byo_only", "receipt_identity")
        require(value["candidate_sha"] == sha, "receipt_sha")
        require(run_id is not None and is_int(value["github_run_id"], 1, RUN_ID_MAX) and value["github_run_id"] == run_id,
                "receipt_run_binding")
        require(is_int(value["github_run_attempt"], 1, 1), "receipt_attempt")
        require((value["launchctl_query_grant_id"], value["launchctl_query_pinned_parent"]) ==
                ((INTEGRATION_GRANT_ID, INTEGRATION_BEFORE) if integration else (GRANT_ID, PINNED_PARENT)),
                "receipt_grant")
        if integration:
            require(value["integration_before"] == INTEGRATION_BEFORE
                    and value["integration_parents"] == [INTEGRATION_BEFORE, INTEGRATION_MAIN], "receipt_grant")
        producer = value["producer"]
        # The producer is a job in this same workflow run: fresh, same SHA, same run.
        require(is_exact(producer, ("sha", "run", "artifact", "zip_sha256", "fresh")) and producer["sha"] == sha
                and is_int(producer["run"], 1, 10**15) and producer["run"] == run_id
                and is_int(producer["artifact"], 1, 10**15) and is_digest(producer["zip_sha256"])
                and producer["fresh"] is True, "receipt_producer")
        hashes = value["source_hashes"]
        require(is_exact(hashes, FILES) and all(is_digest(v) for v in hashes.values()), "receipt_source_hashes")
        check_phases_and_cases(value)
        check_identities(value)
        check_trust(value["trust_controls"])
        check_guard_and_cleanup(value)
        check_requests_and_operations(value)
        require(is_int(value["captured_output_bytes"], 0, RAW_CAP)
                and is_int(value["dependency_payload_bytes_peak"], 0, DEPENDENCY_CAP), "receipt_budget")
        return []
    except Refusal as refusal:
        return [refusal.code]
    except (TypeError, ValueError, KeyError, AttributeError, IndexError, RecursionError):
        # Defense in depth: any unforeseen malformed shape is a fixed refusal, never a crash.
        return ["receipt_malformed"]


# ------------------------------------------------------------ source blob hashes

GIT_TIMEOUT = 20
REAP_TIMEOUT = 5  # Separate finite budget: total acquisition never exceeds GIT_TIMEOUT + REAP_TIMEOUT.
GIT_ENV = MappingProxyType({
    "PATH": "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0",
    # Never lazily fetch a missing (promisor) object: no network, no repository mutation.
    "GIT_NO_LAZY_FETCH": "1", "GIT_ALLOW_PROTOCOL": "none"})


def git_command(sha, path):
    """Fixed read-only argument vector; protocol.allow=never also blocks any transport (inherited by children)."""
    return ["git", "--no-replace-objects", "--no-optional-locks", "-c", "protocol.allow=never",
            "cat-file", "blob", f"{sha}:{path}"]


def collect_bounded(process, maximum, timeout, clock=time.monotonic, ready=None, read=None):
    """Read at most `maximum` bytes under one deadline; kill and reap the owned process otherwise."""
    ready = ready or (lambda wait: bool(select.select([process.stdout], [], [], wait)[0]))
    read = read or (lambda size: os.read(process.stdout.fileno(), size))
    end, chunks, total = clock() + timeout, [], 0
    try:
        while True:
            remaining = end - clock()
            require(remaining > 0, "source_blob_unavailable")
            if not ready(remaining):
                continue  # The deadline is re-checked on every iteration.
            chunk = read(65536)
            if not chunk:
                break
            total += len(chunk)
            require(total <= maximum, "source_blob_unavailable")  # Stop at the cap, never buffer past it.
            chunks.append(chunk)
        remaining = end - clock()
        require(remaining > 0, "source_blob_unavailable")
        returncode = process.wait(timeout=remaining)
    finally:
        if process.poll() is None:
            try:
                process.kill()
            except OSError:
                pass
            try:
                process.wait(timeout=REAP_TIMEOUT)
            except subprocess.TimeoutExpired:
                # Termination could not be observed within the finite budget: fail closed, never wait forever.
                raise Refusal("source_blob_unavailable") from None
    return returncode, b"".join(chunks)


def git_blob_reader(repo, sha, popen=subprocess.Popen, ready=None, read=None, clock=time.monotonic):
    def read_blob(path):
        try:
            require(type(repo) is str and os.path.isdir(repo), "source_blob_unavailable")
            command = (git_command(sha, path) if path is not None else
                       ["git", "--no-replace-objects", "--no-optional-locks", "-c", "protocol.allow=never",
                        "cat-file", "commit", sha])
            process = popen(command, cwd=repo, env=dict(GIT_ENV), stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, shell=False)
            code, data = collect_bounded(process, BLOB_CAP, GIT_TIMEOUT, clock, ready, read)
        except (OSError, subprocess.SubprocessError):
            raise Refusal("source_blob_unavailable") from None
        require(code == 0, "source_blob_unavailable")
        return data
    return read_blob


# -------------------------------------------------------------------- evaluation

def evaluate(sha, load_runs, load_receipt, read_blob, load_reviewed=None):
    """Pure orchestration over injected loaders: returns the verdict object."""
    if type(sha) is not str or REVISION.fullmatch(sha) is None:
        return {"qualified": False, "sha": None, "failures": ["sha_invalid"]}
    failures, runs_ok, receipt_ok = [], False, False
    darwin_id, receipt, computed = None, None, None
    try:
        run_failures, darwin_id = check_runs(sha, strict_json(load_runs(), RUNS_CAP))
        failures += run_failures
        runs_ok = not run_failures
    except Refusal as refusal:
        failures.append("runs_" + refusal.code)
    except Exception:  # noqa: BLE001 - fixed code only; never a crash or raw error
        failures.append("runs_internal")
    try:
        receipt = strict_json(load_receipt(), RECEIPT_CAP)
        receipt_failures = validate_receipt(receipt, sha, darwin_id)
        failures += receipt_failures
        receipt_ok = not receipt_failures
    except Refusal as refusal:
        failures.append("receipt_" + refusal.code)
    except Exception:  # noqa: BLE001
        failures.append("receipt_internal")
    try:
        computed = {}
        for path in FILES:
            blob = read_blob(path)
            require(type(blob) is bytes and len(blob) <= BLOB_CAP, "source_blob_unavailable")
            computed[path] = hashlib.sha256(blob).hexdigest()
    except Exception:  # noqa: BLE001 - any acquisition failure is the same fixed code
        computed = None
        failures.append("source_blob_unavailable")
    if type(receipt) is dict and receipt.get("launchctl_query_grant_id") == INTEGRATION_GRANT_ID:
        try:
            commit = read_blob(None)
            require(type(commit) is bytes and len(commit) <= BLOB_CAP, "integration_graph_unavailable")
            header = commit.split(b"\n\n", 1)[0].splitlines()
            tree = (b"tree " + receipt["candidate_tree"].encode("ascii"))
            parents = [b"parent " + parent.encode("ascii") for parent in (INTEGRATION_BEFORE, INTEGRATION_MAIN)]
            require(header[:3] == [tree, *parents] and
                    not any(line.startswith(b"parent ") for line in header[3:]), "integration_graph_mismatch")
        except Exception:  # noqa: BLE001 - never treat a missing commit as graph evidence
            failures.append("integration_graph_mismatch")
    receipt_hashes = receipt.get("source_hashes") if type(receipt) is dict else None
    if computed is not None and receipt_hashes != computed:
        failures.append("source_hash_mismatch")
    if load_reviewed is not None:
        try:
            reviewed = strict_json(load_reviewed(), HASHES_CAP)
            if not is_exact(reviewed, FILES) or not all(is_digest(v) for v in reviewed.values()):
                failures.append("reviewed_hashes_shape")
            elif reviewed != computed or reviewed != receipt_hashes:
                failures.append("reviewed_hash_mismatch")
        except Refusal as refusal:
            failures.append("reviewed_" + refusal.code)
        except Exception:  # noqa: BLE001
            failures.append("reviewed_internal")
    failures = list(dict.fromkeys(failures))
    qualified = not failures and runs_ok and receipt_ok and computed is not None
    return {"qualified": qualified, "sha": sha, "failures": failures if not qualified else []}


# --------------------------------------------------------------------- self-test

SELF_SHA = "c0ffee" + "0" * 34
SELF_RUNS = {"check": 1001, "native_preflight": 1002, "darwin": 1003}
SELF_BLOBS = {path: ("synthetic blob for " + path).encode() for path in FILES}
SELF_HASHES = {path: hashlib.sha256(blob).hexdigest() for path, blob in SELF_BLOBS.items()}


def fixture_runs(sha=SELF_SHA):
    return [{"databaseId": SELF_RUNS[key], "workflowName": name, "headSha": sha, "headBranch": BRANCH,
             "event": "pull_request" if key == "check" else "push", "status": "completed",
             "conclusion": "success", "attempt": 1}
            for key, name in WORKFLOWS]


def fixture_receipt(sha=SELF_SHA, run=SELF_RUNS["darwin"]):
    digest = lambda character: character * 64
    pairs = {("agent.list", "succeeded"): 2, ("events.subscribe", "succeeded"): 2, ("pane.list", "succeeded"): 2,
             ("pane.read", "refused_expected"): 1, ("pane.read", "succeeded"): 3, ("ping", "succeeded"): 5,
             ("session.snapshot", "succeeded"): 1, ("tab.list", "succeeded"): 2, ("tab.move", "refused_expected"): 1,
             ("workspace.list", "succeeded"): 2, ("workspace.move_block", "refused_expected"): 2}
    rows = [{"method": m, "outcome": o, "count": c} for (m, o), c in sorted(pairs.items())]
    succeeded = sum(c for (_, o), c in pairs.items() if o == "succeeded")
    refused = sum(c for (_, o), c in pairs.items() if o == "refused_expected")
    control = lambda category, python, curl, golang: {"category": category,
        "python": {"category": category, "verify_code": python},
        "curl": {"category": category, "exit_code": curl}, "packaged_go": {"category": category, "exit_code": golang,
                                                                                                     "proof": "accepted" if category == "positive" else "refusal_only"}}
    observation = lambda ordinal: {"ordinal": ordinal, "private_plist_before_absent": True,
        "private_plist_after_absent": True, "setup_ready_seconds": 4.25, "loaded_service": False,
        "proof_kind": "source_bound_wrapper_continued", "query_exit_zero": False, "numeric_query_exit_observed": False}
    case = lambda mode, names, assertions: {"mode": mode, "cases": list(names), "ok": True, "contexts_closed": True,
        "exit_code": 0, "result_bytes": 512, "stdin_bytes": 256, "seconds": 20.5,
        "assertions": {name: True for name in assertions}}
    return {
        "schemaVersion": 1, "candidate_sha": sha, "candidate_tree": "d" * 40, "source_hashes": dict(SELF_HASHES),
        "result": "pass", "scope": "darwin_arm64_byo_only",
        "producer": {"sha": sha, "run": run, "artifact": 777, "zip_sha256": digest("2"), "fresh": True},
        "archive_hashes": {"herdr-mobile-relay_1.2.3_darwin_arm64.tar.gz": digest("3"), "checksums.txt": digest("4")},
        "version": "1.2.3", "manifest_sha256": digest("5"), "binary_sha256": digest("6"),
        "compiler": {"version": "go1.27.3", "target": "darwin/arm64", "metadata_sha256": digest("7")},
        "browser_identity": {"sha256": digest("8"), "version": "153.0.8010.12", "revision": "1243",
            "playwright": "1.63.0", "switches_sha256": digest("9"), "target": "darwin/arm64"},
        "tools": {"python": PYTHON_VERSION, "node": "26.13.0", "bun": "1.4.4", "go": "1.27.3", "curl": "8.7.1",
                  "openssl": "1.1.1w"},
        "source_proof": {"chromium_commit": "971a7443b0c9b0a9b2860529b33331b76077ec62", "playwright_tag": "v1.63.0",
                         "go_tag": "go1.27.3", "trust": "TRUST-PROOF.md"},
        "profile_inputs": {"controller": {"database_sha256": digest("a"), "preferences_sha256": digest("b")},
            "reader": {"database_sha256": digest("c"), "preferences_sha256": digest("d")},
            "negative": {"database_absent": True, "preferences_sha256": digest("e")}},
        "profile_policy": {"schema": 1, "root_der_sha256": digest("f"), "trust_blob": "0a020803",
            "platform_integration": False, "sandbox": True, "mock_keychain": True},
        "service_guard": {"uid": 501, "label": "com.herdr-mobile-relay.service", "starts": 2,
            "command": "launchctl_print_gui_label", "launchctl_path": "/bin/launchctl",
            "launchctl_sha256": digest("1"), "source_guard_sha256": digest("2"), "source_launcher_sha256": digest("3"),
            "observations": [observation(1), observation(2)]},
        "trust_controls": {"python_curl_packaged_go": [control("positive", None, 0, 0), control("unknown_ca", 20, 60, 1),
                                                       control("wrong_host", 64, 60, 1)],
            "browser": {"ok": True, "argv_ok": True, "contexts_closed": True, "version": "153.0.8010.12",
                "observations": ["empty_profile_refuses_ca", "trusted_profile_accepts_ip",
                                 "trusted_profile_refuses_unknown_ca", "trusted_profile_refuses_wrong_host"]}},
        "cases": [case("enroll", ENROLL, ASSERT_ENROLL), case("reprint", PRESERVE, ASSERT_PRESERVE),
                  case("restart", PRESERVE, ASSERT_PRESERVE)],
        "phases": [{"id": i, "ok": True, "exit_code": 0, "cap_seconds": PHASES[i], "seconds": s}
                   for i, s in enumerate((3.5, 12.25, 40.0, 90.125, 2.5))],
        "cleanup": True,
        "owned_cleanup": {"children": 25, "listeners": 4, "surviving_children": 0, "surviving_listeners": 0,
            "observed_surviving_children": 0, "observed_surviving_listeners": 0, "unobserved_children": 0,
            "unobserved_listeners": 0, "closure_errors": [], "uncertain": False, "root_deleted": True},
        "request_counts": [{"tls_accepted": 10, "tls_refused": 3, "http_get": 20, "websocket": 4, "client_aborted": 1},
                           {"tls_refused": 2}, {"tls_refused": 2}],
        "fixture_operations": {"fake": [{"command": c, "outcome": o, "count": 1 if c == "agent prompt" else n}
            for c, o, n in (("agent get", "failed", 3), ("agent list", "succeeded", 3), ("agent prompt", "succeeded", 1),
                            ("integration status", "failed", 2), ("pane list", "succeeded", 3),
                            ("pane process-info", "failed", 4), ("pane read", "succeeded", 3),
                            ("tab list", "succeeded", 3), ("workspace list", "succeeded", 3),
                            ("worktree list", "failed", 3), ("worktree list", "started", 1))],
            "socket": rows},
        "captured_output_bytes": 123456, "dependency_payload_bytes_peak": 987654321,
        "launchctl_query_grant_id": GRANT_ID, "github_run_id": run, "github_run_attempt": 1,
        "launchctl_query_pinned_parent": PINNED_PARENT,
        "socket_diagnostics": {"schema": 1, "rows": copy.deepcopy(rows),
            "reasons": {"ordinary": succeeded, "inert_probe": refused},
            **{bucket: {} for bucket in SOCKET_BUCKETS}, "saturated": False, "liveness_before_cleanup": "serving",
            "closed_after_cleanup": "closed", "guard": "allowed"},
    }


def historical_fail_receipt():
    """Sanitized shape of preserved 3c8f0c8d run 37809619973: phase 3 failed, no cases, spent grant."""
    sha, run = "3c8f0c8db54e9e147bd0034bab83051fe186324e", 37809619973
    seconds = (3.817, 8.942, 41.729, 43.453, 2.812)
    return {"schemaVersion": 1, "candidate_sha": sha, "result": "fail",
        "phases": [{"id": i, "ok": i != 3, "exit_code": 1 if i == 3 else 0, "cap_seconds": PHASES[i], "seconds": s}
                   for i, s in enumerate(seconds)],
        "launchctl_query_grant_id": "q2-parent-launchctl-print-first-push-e59e8022-20261008",
        "launchctl_query_pinned_parent": "e59e8022daaefed14317d760f17f904ad6cd55eb",
        "github_run_id": run, "github_run_attempt": 1,
        "producer": {"sha": sha, "run": run, "artifact": 11563938119,
                     "zip_sha256": "9b59a34870803d827f62187427e1c2ce040d02072c3bb7eeda9c5fe9f914916c", "fresh": True},
        "cases": [], "request_counts": [{}, {}, {}], "cleanup": True, "scope": "darwin_arm64_byo_only",
        "owned_cleanup": {"children": 25, "listeners": 4, "surviving_children": 0, "surviving_listeners": 0,
            "observed_surviving_children": 0, "observed_surviving_listeners": 0, "unobserved_children": 0,
            "unobserved_listeners": 0, "closure_errors": [], "uncertain": False, "root_deleted": True},
        "source_hashes": {FILES[0]: "08c6ff2ed6e5addb6aaf23383c144687a4d89012e51b4fafca037e5d0abb0dbb",
            FILES[1]: "b0f4e01b33d88cc2c5014656dc53591bcc6ea446c0f493c7a03a3f727a7a74bd",
            FILES[2]: "635652ffbb35e704d66442054e6ac1d97f2d64bdb5e01d4b262e0ae24729e169",
            FILES[3]: "f906d36c002d79b643b3ed07f5401dc3275b7b7f98ca84e51adb8c13ca4f0426"},
        "failure": "owned_operation_allowlist", "failure_guard": "herdr_socket",
        "failure_detail": {"type": "Refusal", "errno": None, "function": "check_guards", "line": 1211,
                           "command": None, "exit_code": None}}


def self_test():
    results, encode = [], lambda item: json.dumps(item).encode()
    blobs = lambda path: SELF_BLOBS[path]

    def run(sha, runs, receipt, reader=blobs, reviewed=None):
        as_bytes = lambda item: item if type(item) is bytes else encode(item)
        return evaluate(sha, lambda: as_bytes(runs), lambda: as_bytes(receipt), reader,
                        None if reviewed is None else (lambda: as_bytes(reviewed)))

    def expect(name, verdict, code=None):
        ok = (verdict["qualified"] is True and verdict["failures"] == []) if code is None else (
            verdict["qualified"] is False and code in verdict["failures"])
        results.append((name, ok))

    def mutated(function, base=None):
        item = copy.deepcopy(fixture_receipt() if base is None else base)
        function(item)
        return item

    def bad_run(function):
        runs = fixture_runs()
        function(runs)
        return runs

    expect("accept_all_pass", run(SELF_SHA, fixture_runs(), fixture_receipt()))
    integrated = mutated(lambda r: r.update(launchctl_query_grant_id=INTEGRATION_GRANT_ID,
        launchctl_query_pinned_parent=INTEGRATION_BEFORE, integration_before=INTEGRATION_BEFORE,
        integration_parents=[INTEGRATION_BEFORE, INTEGRATION_MAIN]))
    commit = ("tree " + integrated["candidate_tree"] + "\nparent " + INTEGRATION_BEFORE +
              "\nparent " + INTEGRATION_MAIN + "\nauthor inert <inert@example.invalid> 1 +0000\n\nmessage\n").encode()
    def integrated_reader(path, content=commit):
        return content if path is None else blobs(path)
    expect("accept_exact_integration_graph", run(SELF_SHA, fixture_runs(), integrated, integrated_reader))
    for name, content in (("wrong_parent", commit.replace(INTEGRATION_MAIN.encode(), b"b" * 40)),
            ("reversed_parents", commit.replace(("parent " + INTEGRATION_BEFORE + "\nparent " + INTEGRATION_MAIN).encode(),
                ("parent " + INTEGRATION_MAIN + "\nparent " + INTEGRATION_BEFORE).encode())),
            ("missing_parent", commit.replace(("parent " + INTEGRATION_MAIN + "\n").encode(), b"")),
            ("extra_parent", commit.replace(b"\nauthor ", b"\nparent " + b"b" * 40 + b"\nauthor ")),
            ("wrong_tree", commit.replace(integrated["candidate_tree"].encode(), b"b" * 40))):
        expect("reject_integration_" + name, run(SELF_SHA, fixture_runs(), integrated,
            lambda path, content=content: integrated_reader(path, content)), "integration_graph_mismatch")
    expect("reject_integration_missing_commit", run(SELF_SHA, fixture_runs(), integrated), "integration_graph_mismatch")
    for name, change in (("before", {"integration_before": PINNED_PARENT}),
            ("main", {"integration_parents": [INTEGRATION_BEFORE, "b" * 40]}),
            ("reversed_metadata", {"integration_parents": [INTEGRATION_MAIN, INTEGRATION_BEFORE]}),
            ("spent_id", {"launchctl_query_grant_id": "q2-owner-launchctl-print-push-2230f96f-20261013"}),
            ("q3_id", {"launchctl_query_grant_id": GRANT_ID}),
            ("wrong_source", {"source_hashes": {**integrated["source_hashes"], FILES[0]: "b" * 64}})):
        changed = copy.deepcopy(integrated)
        changed.update(change)
        expect("reject_integration_" + name, run(SELF_SHA, fixture_runs(), changed, integrated_reader),
               "source_hash_mismatch" if name == "wrong_source" else
               "receipt_fields" if name in {"q3_id", "spent_id"} else "receipt_grant")
    expect("accept_reviewed_hashes", run(SELF_SHA, fixture_runs(), fixture_receipt(), reviewed=SELF_HASHES))
    expect("accept_unrelated_extra_workflow", run(SELF_SHA, fixture_runs() + [{**fixture_runs()[0],
        "workflowName": "unrelated", "databaseId": 9, "conclusion": "failure"}], fixture_receipt()))
    # Preserved 3c8 failure: its own SHA/run, otherwise-green runs; the receipt alone must refuse.
    historical = "3c8f0c8db54e9e147bd0034bab83051fe186324e"
    verdict = run(historical, [{**row, "headSha": historical, "databaseId": 37809619973 if row["workflowName"].startswith(
        "Darwin") else row["databaseId"]} for row in fixture_runs()], historical_fail_receipt())
    expect("reject_preserved_3c8_fail", verdict, "receipt_not_pass")
    expect("reject_preserved_3c8_fail_marker", verdict, "receipt_failure_present")
    # Run evidence.
    for key, name in WORKFLOWS:
        index = [n for _, n in WORKFLOWS].index(name)
        expect("reject_missing_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r.pop(i)), fixture_receipt()), "run_missing_" + key)
        expect("reject_duplicate_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r.append(dict(r[i], databaseId=99))),
               fixture_receipt()), "run_duplicate_" + key)
        expect("reject_rerun_attempt_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(attempt=2)),
               fixture_receipt()), "run_attempt_" + key)
        expect("reject_rerun_row_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r.append(dict(r[i], attempt=2, databaseId=98))),
               fixture_receipt()), "run_duplicate_" + key)
        expect("reject_failed_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(conclusion="failure")),
               fixture_receipt()), "run_conclusion_" + key)
        expect("reject_incomplete_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(status="in_progress",
               conclusion="")), fixture_receipt()), "run_status_" + key)
        expect("reject_wrong_sha_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(headSha="e" * 40)),
               fixture_receipt()), "run_sha_" + key)
        expect("reject_wrong_branch_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(headBranch="main")),
               fixture_receipt()), "run_branch_" + key)
        expect("reject_wrong_event_" + key, run(SELF_SHA, bad_run(
            lambda r, i=index, k=key: r[i].update(event="push" if k == "check" else "pull_request")),
            fixture_receipt()), "run_event_" + key)
        expect("reject_bool_id_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(databaseId=True)),
               fixture_receipt()), "run_id_" + key)
        expect("reject_bool_attempt_" + key, run(SELF_SHA, bad_run(lambda r, i=index: r[i].update(attempt=True)),
               fixture_receipt()), "run_attempt_" + key)
    expect("reject_runs_not_list", run(SELF_SHA, {"runs": []}, fixture_receipt()), "runs_shape")
    expect("reject_runs_extra_field", run(SELF_SHA, bad_run(lambda r: r[0].update(url="x")), fixture_receipt()), "runs_row_shape")
    expect("reject_runs_row_not_object", run(SELF_SHA, bad_run(lambda r: r.append(5)), fixture_receipt()), "runs_row_shape")
    expect("reject_runs_missing_field", run(SELF_SHA, bad_run(lambda r: r[0].pop("attempt")), fixture_receipt()), "runs_row_shape")
    expect("reject_runs_empty", run(SELF_SHA, [], fixture_receipt()), "run_missing_darwin")
    expect("reject_sha_invalid", run("ABC", fixture_runs(), fixture_receipt()), "sha_invalid")
    expect("reject_sha_short", run(SELF_SHA[:39], fixture_runs(), fixture_receipt()), "sha_invalid")
    expect("reject_runs_other_sha", run("a" * 40, fixture_runs(), fixture_receipt("a" * 40)), "run_sha_check")
    # Strict JSON / types.
    expect("reject_runs_malformed", run(SELF_SHA, b"[", fixture_receipt()), "runs_json_invalid")
    expect("reject_receipt_malformed", run(SELF_SHA, fixture_runs(), b"{"), "receipt_json_invalid")
    expect("reject_receipt_nan", run(SELF_SHA, fixture_runs(), b'{"schemaVersion": NaN}'), "receipt_json_constant")
    expect("reject_receipt_infinite", run(SELF_SHA, fixture_runs(), b'{"x": 1e999}'), "receipt_json_nonfinite")
    expect("reject_receipt_duplicate_key", run(SELF_SHA, fixture_runs(), b'{"result": "pass", "result": "fail"}'),
           "receipt_duplicate_json_key")
    expect("reject_receipt_oversize", run(SELF_SHA, fixture_runs(), b" " * (RECEIPT_CAP + 1)), "receipt_input_cap")
    expect("reject_receipt_not_object", run(SELF_SHA, fixture_runs(), []), "receipt_type")
    expect("reject_receipt_secret_key", run(SELF_SHA, fixture_runs(), mutated(lambda r: r.update(api_token="x"))),
           "receipt_sensitive")
    expect("reject_receipt_url_value", run(SELF_SHA, fixture_runs(), mutated(lambda r: r.update(version="https://x"))),
           "receipt_sensitive")
    # Receipt binding and identity.
    def receipt_case(name, function, code):
        expect(name, run(SELF_SHA, fixture_runs(), mutated(function)), code)
    receipt_case("reject_missing_field", lambda r: r.pop("service_guard"), "receipt_fields")
    receipt_case("reject_extra_field", lambda r: r.update(extra=1), "receipt_fields")
    receipt_case("reject_result_fail", lambda r: r.update(result="fail"), "receipt_not_pass")
    receipt_case("reject_failure_field", lambda r: r.update(failure="command_failed"), "receipt_failure_present")
    receipt_case("reject_failure_guard", lambda r: r.update(failure_guard="herdr_socket"), "receipt_failure_present")
    receipt_case("reject_failure_detail", lambda r: r.update(failure_detail={}), "receipt_failure_present")
    receipt_case("reject_wrong_candidate", lambda r: r.update(candidate_sha="d" * 40), "receipt_sha")
    receipt_case("reject_wrong_run", lambda r: r.update(github_run_id=1005), "receipt_run_binding")
    receipt_case("reject_bool_run", lambda r: r.update(github_run_id=True), "receipt_run_binding")
    receipt_case("reject_attempt_two", lambda r: r.update(github_run_attempt=2), "receipt_attempt")
    receipt_case("reject_bool_attempt", lambda r: r.update(github_run_attempt=True), "receipt_attempt")
    receipt_case("reject_spent_grant", lambda r: r.update(
        launchctl_query_grant_id="q2-parent-launchctl-print-first-push-e59e8022-20261008"), "receipt_grant")
    receipt_case("reject_spent_parent", lambda r: r.update(
        launchctl_query_pinned_parent="e59e8022daaefed14317d760f17f904ad6cd55eb"), "receipt_grant")
    receipt_case("reject_spent_3c8_grant", lambda r: r.update(
        launchctl_query_grant_id="q2-owner-launchctl-print-push-3c8f0c8d-20261012"), "receipt_grant")
    receipt_case("reject_spent_3c8_parent", lambda r: r.update(
        launchctl_query_pinned_parent="3c8f0c8db54e9e147bd0034bab83051fe186324e"), "receipt_grant")
    # Iteration-3 locator fields (innermost helper frame) never make a failed receipt acceptable.
    receipt_case("reject_helper_locator_failure", lambda r: r.update(result="fail", failure="unexpected_exception",
        failure_detail={"type": "GateFailure", "errno": None, "function": "runtime", "line": 1, "command": None,
                        "exit_code": None, "helper_function": "development_setup_link", "helper_line": 1,
                        "helper_code": "fixture_assertion"}), "receipt_failure_present")
    receipt_case("reject_spent_8c1_grant", lambda r: r.update(
        launchctl_query_grant_id="q2-owner-launchctl-print-push-8c1f22ff-20261013"), "receipt_grant")
    receipt_case("reject_spent_8c1_parent", lambda r: r.update(
        launchctl_query_pinned_parent="8c1f22ff4980335ce0d1acc1731429a3457067a9"), "receipt_grant")
    # Iteration-4 diagnostics: a relayed non-101 upgrade answer is an ordinary counter (never on a
    # negative-TLS control), while any proxy first-cause locator marks the receipt non-passing.
    expect("accept_websocket_refused_counter", run(SELF_SHA, fixture_runs(), mutated(
        lambda r: r["request_counts"][0].update(websocket_refused=2))))
    receipt_case("reject_websocket_refused_on_control", lambda r: r["request_counts"][1].update(websocket_refused=1),
                 "receipt_request_controls")
    receipt_case("reject_websocket_refused_type", lambda r: r["request_counts"][0].update(websocket_refused=True),
                 "receipt_request_fields")
    receipt_case("reject_proxy_failure_locator", lambda r: r.update(proxy_failure=[{
        "proxy": 0, "reason": "request_allowlist", "path": "version", "query": "check", "method": "get"}]),
        "receipt_failure_present")
    diagnostic_proxy = run(SELF_SHA, fixture_runs(), mutated(lambda r: r.update(
        result="fail", failure="owned_operation_allowlist", failure_guard="proxy",
        proxy_failure=[{"proxy": 0, "reason": "request_allowlist", "path": "version", "query": "check",
                        "method": "get"}])))
    expect("reject_proxy_failure_fail_receipt", diagnostic_proxy, "receipt_not_pass")
    results.append(("reject_proxy_failure_fixed_codes", diagnostic_proxy["failures"] == [
        "receipt_not_pass", "receipt_failure_present"]))
    receipt_case("reject_spent_dd6_grant", lambda r: r.update(
        launchctl_query_grant_id="q2-owner-launchctl-print-push-dd608845-20261013"), "receipt_grant")
    receipt_case("reject_spent_dd6_parent", lambda r: r.update(
        launchctl_query_pinned_parent="dd608845b73c0323f5cbcfa140ad52affb28ec7f"), "receipt_grant")
    receipt_case("reject_spent_2230_grant", lambda r: r.update(
        launchctl_query_grant_id="q2-owner-launchctl-print-push-2230f96f-20261013"), "receipt_grant")
    receipt_case("reject_spent_2230_parent", lambda r: r.update(
        launchctl_query_pinned_parent="2230f96fb61e1740c21154935a25c235bab71cf3"), "receipt_grant")
    # A FAIL receipt carrying the runner's bucketed fake-operation diagnostics (incl. "other") is a
    # fixed non-qualifying verdict, never an exception and never qualified.
    diagnostic_fail = run(SELF_SHA, fixture_runs(), mutated(lambda r: (r.update(
        result="fail", failure="owned_operation_allowlist", failure_guard="fake_herdr"),
        r["fixture_operations"].update(fake=[{"command": "agent get", "outcome": "failed", "count": 2},
            {"command": "other", "outcome": "failed", "count": 13}, {"command": "other", "outcome": "other", "count": 1}]))))
    expect("reject_fake_diagnostic_fail", diagnostic_fail, "receipt_not_pass")
    results.append(("reject_fake_diagnostic_fail_fixed_codes", diagnostic_fail["failures"] == [
        "receipt_not_pass", "receipt_failure_present"]))
    receipt_case("reject_schema_two", lambda r: r.update(schemaVersion=2), "receipt_identity")
    receipt_case("reject_scope", lambda r: r.update(scope="other"), "receipt_identity")
    receipt_case("reject_producer_stale", lambda r: r["producer"].update(fresh=False), "receipt_producer")
    receipt_case("reject_producer_sha", lambda r: r["producer"].update(sha="e" * 40), "receipt_producer")
    receipt_case("reject_producer_run", lambda r: r["producer"].update(run=1), "receipt_producer")
    receipt_case("reject_producer_artifact_bool", lambda r: r["producer"].update(artifact=True), "receipt_producer")
    receipt_case("reject_producer_digest", lambda r: r["producer"].update(zip_sha256="x"), "receipt_producer")
    receipt_case("reject_source_hash_shape", lambda r: r["source_hashes"].pop(FILES[0]), "receipt_source_hashes")
    # Phases, cases, trust, guard, cleanup, requests, operations, diagnostics.
    receipt_case("reject_failed_phase", lambda r: r["phases"][3].update(ok=False, exit_code=1), "receipt_phase")
    receipt_case("reject_phase_exit", lambda r: r["phases"][0].update(exit_code=1), "receipt_phase")
    receipt_case("reject_phase_overrun", lambda r: r["phases"][3].update(seconds=630), "receipt_phase")
    receipt_case("reject_phase_cap", lambda r: r["phases"][1].update(cap_seconds=301), "receipt_phase")
    receipt_case("reject_phase_missing", lambda r: r["phases"].pop(), "receipt_phases")
    receipt_case("reject_phase_bool_seconds", lambda r: r["phases"][0].update(seconds=True), "receipt_phase")
    receipt_case("reject_no_cases", lambda r: r.update(cases=[]), "receipt_cases")
    receipt_case("reject_case_list", lambda r: r["cases"][0].update(cases=list(ENROLL[:3])), "receipt_case")
    receipt_case("reject_case_failed", lambda r: r["cases"][1].update(ok=False), "receipt_case")
    receipt_case("reject_case_exit", lambda r: r["cases"][2].update(exit_code=1), "receipt_case")
    receipt_case("reject_case_context", lambda r: r["cases"][0].update(contexts_closed=False), "receipt_case")
    receipt_case("reject_missing_assertion", lambda r: r["cases"][0]["assertions"].pop("controller_command"),
                 "receipt_assertions")
    receipt_case("reject_false_assertion", lambda r: r["cases"][1]["assertions"].update(credentials_preserved=False),
                 "receipt_assertions")
    receipt_case("reject_zero_protocol_bytes", lambda r: r["cases"][0].update(result_bytes=0), "receipt_protocol_bytes")
    receipt_case("reject_case_seconds", lambda r: r["cases"][0].update(seconds=150), "receipt_case_seconds")
    receipt_case("reject_missing_browser_trust", lambda r: r["trust_controls"]["browser"]["observations"].pop(),
                 "receipt_browser_trust")
    receipt_case("reject_browser_args", lambda r: r["trust_controls"]["browser"].update(argv_ok=False), "receipt_browser_trust")
    receipt_case("reject_missing_tls_control", lambda r: r["trust_controls"]["python_curl_packaged_go"].pop(), "receipt_tls")
    receipt_case("reject_tls_category", lambda r: r["trust_controls"]["python_curl_packaged_go"][1]["curl"].update(
        category="positive"), "receipt_tls_tool")
    receipt_case("reject_python_verify", lambda r: r["trust_controls"]["python_curl_packaged_go"][1]["python"].update(
        verify_code=99), "receipt_python_tls")
    receipt_case("reject_go_unknown_ca_accepted", lambda r: r["trust_controls"]["python_curl_packaged_go"][1][
        "packaged_go"].update(exit_code=0), "receipt_tls_exit")
    receipt_case("reject_go_proof_missing", lambda r: r["trust_controls"]["python_curl_packaged_go"][1]["packaged_go"].pop("proof"),
                 "receipt_tls_tool")
    receipt_case("reject_go_proof_wrong", lambda r: r["trust_controls"]["python_curl_packaged_go"][2]["packaged_go"].update(
        proof="accepted"), "receipt_tls_proof")
    receipt_case("reject_go_unknown_ca_other_exit", lambda r: r["trust_controls"]["python_curl_packaged_go"][1][
        "packaged_go"].update(exit_code=2), "receipt_tls_exit")
    receipt_case("reject_curl_wrong_host", lambda r: r["trust_controls"]["python_curl_packaged_go"][2]["curl"].update(
        exit_code=0), "receipt_tls_exit")
    receipt_case("reject_numeric_query_observed", lambda r: r["service_guard"]["observations"][0].update(
        numeric_query_exit_observed=True), "receipt_guard_observation")
    receipt_case("reject_guard_proof_kind", lambda r: r["service_guard"]["observations"][1].update(proof_kind="traced"),
                 "receipt_guard_observation")
    receipt_case("reject_guard_starts", lambda r: r["service_guard"].update(starts=1), "receipt_guard")
    receipt_case("reject_guard_loaded", lambda r: r["service_guard"]["observations"][0].update(loaded_service=True),
                 "receipt_guard_observation")
    receipt_case("reject_cleanup_false", lambda r: r.update(cleanup=False), "receipt_cleanup")
    receipt_case("reject_cleanup_uncertain", lambda r: r["owned_cleanup"].update(uncertain=True), "receipt_cleanup")
    receipt_case("reject_cleanup_survivor", lambda r: r["owned_cleanup"].update(surviving_children=1), "receipt_cleanup")
    receipt_case("reject_cleanup_unobserved", lambda r: r["owned_cleanup"].update(unobserved_listeners=1), "receipt_cleanup")
    receipt_case("reject_cleanup_errors", lambda r: r["owned_cleanup"].update(closure_errors=["cleanup_deadline"]),
                 "receipt_cleanup")
    receipt_case("reject_root_kept", lambda r: r["owned_cleanup"].update(root_deleted=False), "receipt_cleanup")
    receipt_case("reject_empty_requests", lambda r: r.update(request_counts=[{}, {}, {}]), "receipt_request_controls")
    receipt_case("reject_missing_requests", lambda r: r.update(request_counts=[]), "receipt_requests")
    receipt_case("reject_negative_control_served", lambda r: r["request_counts"][2].update(http_get=1),
                 "receipt_request_controls")
    def fake_rows(function):
        def apply(receipt):
            rows = receipt["fixture_operations"]["fake"]
            function(rows, next(row for row in rows if row["command"] == FAKE_PROMPT))
        return apply
    # The fixture itself accepts read-only started/succeeded/failed rows (accept_all_pass above).
    receipt_case("reject_missing_prompt", fake_rows(lambda rows, prompt: rows.remove(prompt)), "receipt_fake_mutation")
    receipt_case("reject_prompt_twice", fake_rows(lambda rows, prompt: prompt.update(count=2)), "receipt_fake_operation")
    receipt_case("reject_fake_started", fake_rows(lambda rows, prompt: prompt.update(outcome="started")),
                 "receipt_fake_operation")
    receipt_case("reject_fake_prompt_failed", fake_rows(lambda rows, prompt: prompt.update(outcome="failed")),
                 "receipt_fake_operation")
    receipt_case("reject_fake_prompt_duplicate_row", fake_rows(lambda rows, prompt: rows.append(dict(prompt))),
                 "receipt_fake_operation")
    receipt_case("reject_fake_other_command", fake_rows(lambda rows, prompt: rows.append(
        {"command": "other", "outcome": "failed", "count": 1})), "receipt_fake_operation")
    receipt_case("reject_fake_other_outcome", fake_rows(lambda rows, prompt: rows.append(
        {"command": "agent list", "outcome": "other", "count": 1})), "receipt_fake_operation")
    receipt_case("reject_fake_duplicate_pair", fake_rows(lambda rows, prompt: rows.append(dict(rows[0]))),
                 "receipt_fake_operation")
    receipt_case("accept_fake_count_max", fake_rows(lambda rows, prompt: rows[0].update(count=COUNT_MAX)), None)
    receipt_case("reject_fake_count_over_runner_cap", fake_rows(lambda rows, prompt: rows[0].update(count=COUNT_MAX + 1)),
                 "receipt_fake_operation")
    receipt_case("reject_fake_zero_count", fake_rows(lambda rows, prompt: rows[0].update(count=0)),
                 "receipt_fake_operation")
    receipt_case("reject_fake_bool_count", fake_rows(lambda rows, prompt: rows[0].update(count=True)),
                 "receipt_fake_operation")
    receipt_case("reject_fake_extra_row_key", fake_rows(lambda rows, prompt: rows[0].update(argv=["x"])),
                 "receipt_fake_operation")
    for mutating in ("pane run", "pane send-keys", "pane send-text", "pane close", "workspace create", "workspace rename",
                     "tab create", "tab rename", "agent start", "agent rename", "worktree create", "worktree open",
                     "worktree remove", "integration install"):
        receipt_case("reject_fake_mutating_" + mutating.replace(" ", "_"), fake_rows(lambda rows, prompt, m=mutating:
            rows.append({"command": m, "outcome": "succeeded", "count": 1})), "receipt_fake_operation")
    receipt_case("reject_fake_row_cap", fake_rows(lambda rows, prompt: rows.extend(
        {"command": "agent list", "outcome": "succeeded", "count": index + 1} for index in range(FAKE_ROW_CAP))),
        "receipt_operations")

    def socket_mutation(function):
        def apply(receipt):
            function(receipt["fixture_operations"]["socket"])
            receipt["socket_diagnostics"]["rows"] = copy.deepcopy(receipt["fixture_operations"]["socket"])
        return apply
    receipt_case("reject_socket_failed_pair", socket_mutation(lambda rows: (rows.append({
        "method": "other", "outcome": "failed", "count": 1}), rows.sort(key=lambda r: (r["method"], r["outcome"])))),
        "receipt_socket_operations")
    receipt_case("reject_socket_probe_cap", socket_mutation(lambda rows: [row.update(count=65) for row in rows
        if row["method"] == "tab.move"]), "receipt_socket_operations")
    receipt_case("reject_socket_pane_read_low", socket_mutation(lambda rows: [row.update(count=1) for row in rows
        if (row["method"], row["outcome"]) == ("pane.read", "succeeded")]), "receipt_socket_operations")
    receipt_case("reject_socket_unsorted", socket_mutation(lambda rows: rows.reverse()), "receipt_socket_rows")
    receipt_case("reject_socket_duplicate", socket_mutation(lambda rows: rows.append(dict(rows[-1]))), "receipt_socket_rows")
    receipt_case("reject_socket_row_disagreement", lambda r: r["socket_diagnostics"]["rows"].pop(), "receipt_socket_diagnostics")
    receipt_case("reject_diag_guard", lambda r: r["socket_diagnostics"].update(guard="unexpected_pair"), "receipt_socket_guard")
    receipt_case("reject_diag_exception", lambda r: r["socket_diagnostics"]["stream_exceptions"].update(os_error=1),
                 "receipt_socket_exceptions")
    receipt_case("reject_diag_handler_exception", lambda r: r["socket_diagnostics"]["handler_exceptions"].update(
        timeout=1), "receipt_socket_exceptions")
    receipt_case("reject_diag_saturated", lambda r: r["socket_diagnostics"].update(saturated=True), "receipt_socket_saturated")
    receipt_case("reject_diag_dead_listener", lambda r: r["socket_diagnostics"].update(liveness_before_cleanup="early_death"),
                 "receipt_socket_liveness")
    receipt_case("reject_diag_open", lambda r: r["socket_diagnostics"].update(closed_after_cleanup="open"),
                 "receipt_socket_liveness")
    receipt_case("reject_diag_reason", lambda r: r["socket_diagnostics"]["reasons"].update(recv_failed=1),
                 "receipt_socket_diagnostics")
    receipt_case("reject_diag_reason_total", lambda r: r["socket_diagnostics"]["reasons"].update(ordinary=1),
                 "receipt_socket_diagnostics")
    receipt_case("reject_diag_missing", lambda r: r.pop("socket_diagnostics"), "receipt_fields")
    receipt_case("reject_output_cap", lambda r: r.update(captured_output_bytes=RAW_CAP + 1), "receipt_budget")
    receipt_case("reject_dependency_cap", lambda r: r.update(dependency_payload_bytes_peak=DEPENDENCY_CAP + 1), "receipt_budget")
    receipt_case("reject_bool_budget", lambda r: r.update(captured_output_bytes=True), "receipt_budget")
    receipt_case("reject_compiler", lambda r: r["compiler"].update(version="go1.26.0"), "receipt_compiler")
    receipt_case("reject_browser_identity", lambda r: r["browser_identity"].update(version="150.0.0.0"), "receipt_browser")
    receipt_case("reject_tools", lambda r: r["tools"].update(node="24.0.0"), "receipt_tools")
    receipt_case("reject_python_drift", lambda r: r["tools"].update(python="3.13.1"), "receipt_tools")
    receipt_case("reject_python_suffix", lambda r: r["tools"].update(python=PYTHON_VERSION + "1"), "receipt_tools")
    receipt_case("reject_source_proof", lambda r: r["source_proof"].update(go_tag="go1.26"), "receipt_source_proof")
    receipt_case("reject_profile_policy", lambda r: r["profile_policy"].update(platform_integration=True),
                 "receipt_profile_policy")
    receipt_case("reject_negative_profile", lambda r: r["profile_inputs"]["negative"].update(database_absent=False),
                 "receipt_negative_profile")
    receipt_case("reject_archive_name", lambda r: r.update(version="1.2.4"), "receipt_archive")
    receipt_case("reject_tree", lambda r: r.update(candidate_tree="x"), "receipt_tree")
    # Valid-JSON but unhashable/mistyped labels must give a fixed verdict, never an exception.
    for label, bad in (("list", []), ("dict", {}), ("none", None), ("int", 7), ("bool", True)):
        receipt_case("reject_fake_command_" + label, lambda r, b=bad: r["fixture_operations"]["fake"][0].update(command=b),
                     "receipt_fake_operation")
        receipt_case("reject_fake_outcome_" + label, lambda r, b=bad: r["fixture_operations"]["fake"][0].update(outcome=b),
                     "receipt_fake_operation")
        receipt_case("reject_socket_method_" + label, socket_mutation(lambda rows, b=bad: rows[0].update(method=b)),
                     "receipt_socket_rows")
        receipt_case("reject_socket_outcome_" + label, socket_mutation(lambda rows, b=bad: rows[0].update(outcome=b)),
                     "receipt_socket_rows")
    # Exhaustive type-confusion sweep: every node of the valid receipt (and of a run row) replaced by
    # each JSON-typed stand-in must still return the closed verdict shape (no traceback), never PASS
    # unless the replacement left the evidence valid.
    def nodes(item, path=()):
        yield path
        children = item.items() if type(item) is dict else enumerate(item) if type(item) is list else ()
        for key, child in children:
            yield from nodes(child, path + (key,))

    def replaced(root, path, value):
        root = copy.deepcopy(root)
        if not path:
            return value
        target = root
        for key in path[:-1]:
            target = target[key]
        target[path[-1]] = value
        return root
    swept, crashed = 0, []
    base_receipt, base_runs = fixture_receipt(), fixture_runs()
    for path in nodes(base_receipt):
        for stand_in in ([], {}, None, "x", True, -1, 1.5, [[]], {"a": {}}):
            swept += 1
            try:
                verdict = run(SELF_SHA, base_runs, replaced(base_receipt, path, stand_in))
                shaped = set(verdict) == {"qualified", "sha", "failures"} and type(verdict["failures"]) is list
            except Exception:  # noqa: BLE001 - the sweep exists to catch tracebacks
                shaped = False
            if not shaped:
                crashed.append(path)
    for path in nodes(base_runs):
        for stand_in in ([], {}, None, "x", True, -1, 1.5):
            swept += 1
            try:
                verdict = run(SELF_SHA, replaced(base_runs, path, stand_in), base_receipt)
                shaped = set(verdict) == {"qualified", "sha", "failures"} and type(verdict["failures"]) is list
            except Exception:  # noqa: BLE001
                shaped = False
            if not shaped:
                crashed.append(path)
    results.append(("type_confusion_sweep_no_crash", not crashed and swept > 1000))
    # Production git-acquisition seam with an injected fake Popen: bounded, no-fetch, reaped. No real process.
    class FakeProcess:
        def __init__(self, returncode=0, wait_error=None, stubborn=False):
            self.stdout, self.returncode, self.wait_error, self.stubborn = object(), returncode, wait_error, stubborn
            self.killed = self.waited = False
            self.waits = []

        def poll(self):
            if self.stubborn:
                return None
            return self.returncode if self.waited and not self.killed else (-9 if self.killed else None)

        def wait(self, timeout=None):
            self.waits.append(timeout)
            if self.stubborn:  # Killed child that cannot be reaped (e.g. blocked in filesystem I/O).
                raise subprocess.TimeoutExpired("git", timeout or 0)
            if self.wait_error is not None and not self.killed:
                raise self.wait_error
            self.waited = True
            return self.returncode

        def kill(self):
            self.killed = True

    def acquire(chunks, process=None, popen_error=None, repo=".", clock=None):
        process, calls, reads = process or FakeProcess(), [], []
        iterator = iter(chunks)

        def popen(argv, **kwargs):
            calls.append((argv, kwargs))
            if popen_error is not None:
                raise popen_error
            return process

        def read(size):
            reads.append(size)
            return next(iterator, b"")
        reader = git_blob_reader(repo, SELF_SHA, popen=popen, ready=lambda wait: True, read=read,
                                 **({} if clock is None else {"clock": clock}))
        try:
            return reader(FILES[1]), process, calls, reads
        except Refusal as refusal:
            return refusal.code, process, calls, reads
    data, process, calls, reads = acquire([b"abc", b""])
    argv, kwargs = calls[0] if calls else ([], {})
    results.append(("git_adapter_fixed_readonly_no_fetch_argv", data == b"abc" and argv == [
        "git", "--no-replace-objects", "--no-optional-locks", "-c", "protocol.allow=never", "cat-file", "blob",
        SELF_SHA + ":" + FILES[1]] and kwargs["shell"] is False and kwargs["cwd"] == "."
        and kwargs["env"]["GIT_NO_LAZY_FETCH"] == "1" and kwargs["env"]["GIT_ALLOW_PROTOCOL"] == "none"
        and kwargs["env"]["GIT_CONFIG_GLOBAL"] == "/dev/null" and kwargs["env"]["GIT_TERMINAL_PROMPT"] == "0"
        and kwargs["stdin"] is subprocess.DEVNULL and kwargs["stdout"] is subprocess.PIPE and not process.killed))
    data, process, calls, reads = acquire([b"x" * BLOB_CAP, b""])
    results.append(("git_adapter_accepts_exactly_cap", type(data) is bytes and len(data) == BLOB_CAP))
    data, process, calls, reads = acquire(iter(lambda: b"y" * (1024 * 1024), None))
    results.append(("git_adapter_overflow_stops_and_reaps", data == "source_blob_unavailable" and process.killed
                    and len(reads) <= BLOB_CAP // (1024 * 1024) + 1))
    data, process, _, _ = acquire([b"z" * (BLOB_CAP + 1)])
    results.append(("git_adapter_overflow_single_chunk", data == "source_blob_unavailable" and process.killed))
    data, process, _, _ = acquire([b"abc", b""], process=FakeProcess(returncode=128))
    results.append(("git_adapter_nonzero_exit", data == "source_blob_unavailable"))
    data, process, _, _ = acquire([b"abc", b""], process=FakeProcess(wait_error=subprocess.TimeoutExpired("git", 1)))
    results.append(("git_adapter_wait_timeout_reaped", data == "source_blob_unavailable" and process.killed))
    for name, chunks in (("normal_wait", [b"abc", b""]), ("overflow", iter(lambda: b"y" * (1024 * 1024), None))):
        data, process, _, _ = acquire(chunks, process=FakeProcess(stubborn=True))
        results.append(("git_adapter_unreapable_child_" + name + "_bounded_refusal", data == "source_blob_unavailable"
                        and process.killed and 1 <= len(process.waits) <= 2
                        and all(type(t) in (int, float) and 0 < t <= GIT_TIMEOUT for t in process.waits)
                        and process.waits[-1] == REAP_TIMEOUT))
    data, _, _, _ = acquire([], popen_error=OSError("inert"))
    results.append(("git_adapter_spawn_failure", data == "source_blob_unavailable"))
    data, _, calls, _ = acquire([b"abc"], repo="definitely-not-a-directory-for-self-test")
    results.append(("git_adapter_missing_repo_before_spawn", data == "source_blob_unavailable" and not calls))
    ticks = iter(range(0, 1000, 15))
    data, process, _, _ = acquire(iter(lambda: b"a", None), clock=lambda: next(ticks))
    results.append(("git_adapter_deadline_reaped", data == "source_blob_unavailable" and process.killed))
    # Source and reviewed hashes at the exact SHA (injected blobs; no git).
    expect("reject_receipt_hash_mismatch", run(SELF_SHA, fixture_runs(), mutated(lambda r: r["source_hashes"].update(
        {FILES[1]: "0" * 64}))), "source_hash_mismatch")
    expect("reject_blob_mismatch", run(SELF_SHA, fixture_runs(), fixture_receipt(), lambda path: b"changed"),
           "source_hash_mismatch")

    def failing(_path):
        raise Refusal("source_blob_unavailable")
    expect("reject_blob_unavailable", run(SELF_SHA, fixture_runs(), fixture_receipt(), failing), "source_blob_unavailable")
    expect("reject_blob_not_bytes", run(SELF_SHA, fixture_runs(), fixture_receipt(), lambda path: "text"),
           "source_blob_unavailable")
    expect("reject_reviewed_mismatch", run(SELF_SHA, fixture_runs(), fixture_receipt(), reviewed={**SELF_HASHES,
           FILES[0]: "1" * 64}), "reviewed_hash_mismatch")
    expect("reject_reviewed_shape", run(SELF_SHA, fixture_runs(), fixture_receipt(), reviewed={FILES[0]: SELF_HASHES[FILES[0]]}),
           "reviewed_hashes_shape")
    expect("reject_reviewed_extra_path", run(SELF_SHA, fixture_runs(), fixture_receipt(), reviewed={**SELF_HASHES,
           "scripts/qualify-darwin-package-browser.py": "2" * 64}), "reviewed_hashes_shape")
    expect("reject_reviewed_malformed", run(SELF_SHA, fixture_runs(), fixture_receipt(), reviewed=b"{"), "reviewed_json_invalid")
    # Verdict is a closed shape and never echoes input values.
    verdict = run(SELF_SHA, fixture_runs(), mutated(lambda r: r.update(candidate_sha="d" * 40)))
    results.append(("verdict_shape", set(verdict) == {"qualified", "sha", "failures"} and verdict["sha"] == SELF_SHA
                    and all(type(c) is str and re.fullmatch(r"[a-z0-9_]{1,64}", c) for c in verdict["failures"])
                    and "d" * 40 not in json.dumps(verdict)))
    failed = sorted(name for name, ok in results if not ok)
    print(json.dumps({"selfTest": True, "synthetic_offline_only": True, "ok": not failed, "cases": len(results),
                      "failed": failed}, separators=(",", ":")))
    return 0 if not failed else 1


# ------------------------------------------------------------------------- main

class Parser(argparse.ArgumentParser):
    def error(self, message):  # Never echo rejected argument values.
        sys.stderr.write("usage error: see --help\n")
        raise SystemExit(2)


def main(argv=None):
    parser = Parser(description="Offline Darwin package-browser qualification gate (see module docstring).",
                    epilog="RUNS.json: gh run list --branch " + BRANCH + " --commit SHA --json "
                           "databaseId,workflowName,headSha,headBranch,event,status,conclusion,attempt")
    parser.add_argument("--sha", help="exact 40-hex candidate commit")
    parser.add_argument("--runs", help="gh run list JSON file")
    parser.add_argument("--receipt", help="downloaded darwin-package-browser.json")
    parser.add_argument("--repo", default=".", help="local git checkout holding the candidate objects (default: .)")
    parser.add_argument("--reviewed-hashes", help="JSON map of the four runner source paths to reviewed SHA256")
    parser.add_argument("--self-test", action="store_true", help="embedded offline fixtures only")
    args = parser.parse_args(argv)
    if args.self_test:
        if any((args.sha, args.runs, args.receipt, args.reviewed_hashes)):
            parser.error("")
        return self_test()
    if not (args.sha and args.runs and args.receipt):
        parser.error("")
    verdict = evaluate(args.sha, lambda: read_file(args.runs, RUNS_CAP), lambda: read_file(args.receipt, RECEIPT_CAP),
                       git_blob_reader(args.repo, args.sha),
                       None if args.reviewed_hashes is None else (lambda: read_file(args.reviewed_hashes, HASHES_CAP)))
    print(json.dumps(verdict, separators=(",", ":")))
    return 0 if verdict["qualified"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

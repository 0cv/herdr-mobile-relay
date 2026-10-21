#!/usr/bin/env python3
"""Frozen case matrix, evidence schemas (native-capability-v1, native-producer-v1, native-cases-v1) and exact-once gate of the native-manager lifecycle workflow (partial Q1, not M3); pure, it reads only the files it is given."""
import argparse
import hashlib
import json
import os
import re
import sys
import tarfile

TIER = "q1-partial"
SCHEMAS = {"probe": "native-capability-v1", "producer": "native-producer-v1", "cases": "native-cases-v1"}
RUNNERS = {"macos-15": "Darwin", "ubuntu-24.04": "Linux"}
TARGETS = {"Darwin": "darwin_arm64", "Linux": "linux_amd64"}
COMMON = ("a-install-start", "a-stop-start-restart", "login-style-relaunch", "bad-token", "bad-policy",
          "public-wizard-premutation", "gateway-missing-app", "manager-missing-injected", "a-full-uninstall",
          "bystander-nonmutation")
ONLY = {"Darwin": ("foreign-launchd-path",), "Linux": ("foreign-systemd-fragment", "foreign-systemd-dropin")}
REFUSALS = frozenset(COMMON[3:8] + ONLY["Darwin"] + ONLY["Linux"])  # the product must exit nonzero and change nothing
LIVE = ("a-install-start", "a-stop-start-restart", "login-style-relaunch")  # cells that need real public HTTPS and socket traffic
LIMIT = 1 << 20
SNAP = ("definition_hash", "mode", "origin", "active", "enabled", "release_revision", "owned_pid_digest", "manager_generation")
ENUM = {"manager_pre": ("available", "unavailable", "unknown"), "manager_post": ("available", "unavailable", "unknown"),
        "isolated_uid": ("available", "unavailable", "unknown", "not-applicable"),
        "default_target": ("active", "inactive", "unknown", "not-applicable"),
        "trust_negative": ("refused", "accepted", "unknown"), "trust_positive": ("accepted", "refused", "unknown"),
        "linger_initial": ("yes", "no", "unknown", "not-applicable"), "linger_changed": ("yes", "no", "unknown", "not-applicable"),
        "cleanup": ("pass", "fail", "unknown")}
# Probe diagnostics (Q1D): EVIDENCE of where a probe stopped, validated strictly and read by no verdict (probe_passes and failure_phase never look at them; check_probe refuses a PASSING receipt whose diagnostics record a failed stage). Each is a fixed enum, an integer
# in a range, or text restricted to a sanitized character class, and `not-run` / `none` is what a stage that did not run, or a fact the driver did not write, becomes.
DIAG_ENUM = {"gui_nul": ("clean", "nul", "not-run"),
             "disabled_admission": ("pass", "over-limit", "nul", "header", "trailer", "empty", "not-run"),
             "baseline_failed": ("none", "path-conf", "path-release", "path-cache", "path-definition", "path-legacy-definition", "path-curlrc", "path-shim", "manager-path-unread",
                                 "command-provenance", "label-active", "legacy-active", "hosts", "not-run")}
DIAG_NUMBER = {"gui_print_status": 255, "disabled_status": 255, "gui_bytes": 10 ** 12, "disabled_bytes": 10 ** 12}  # an integer 0..limit (an exit status, a size), or `not-run`
HINT = r'[A-Za-z0-9 ={}"<>()._:/-]{1,64}'  # a launchctl answer line reduced to this class (and to known words, numbers and punctuation: hint_ok), `none` for nothing (scripts/ci/native-lifecycle-host.py shape_hint)
DIAG_HINT = ("disabled_head", "disabled_tail", "gui_print_err", "disabled_err")
STATE = r"not-run|[a-z-]{1,24}"  # the observer's `active` value at the baseline, or `not-run`
DIAG_STATE = ("baseline_label_state", "baseline_legacy_state")
DIAG_KEYS = tuple(DIAG_ENUM) + tuple(DIAG_NUMBER) + DIAG_HINT + DIAG_STATE
BASELINE_PASS = {"baseline_failed": "none", "baseline_label_state": "absent", "baseline_legacy_state": "absent"}  # what the baseline records when it passes (baseline_note none absent absent)
GUI_PASS = {"gui_print_status": 0, "disabled_status": 0, "disabled_admission": "pass"}  # macOS: the stages whose success makes the manager available
GUI_EVIDENCE = ("gui_nul",)  # recorded where the gui queries ran, never a stage: the product discards the domain listing and reads only its status, so a NUL in it fails nothing (`not-run` on Linux)
GUI_SIZES = ("gui_bytes", "disabled_bytes")  # recorded only where the gui queries ran (`not-run` on Linux)
TABLE_BOUND = "disabled_bytes"  # the print-disabled table is at most the product's 65536 bytes (the helper refuses a larger one), or not measured; the domain listing's size is evidence only: the product reads that listing by status and it is 89,044 bytes on macos-15


class Refusal(Exception):
    """A fixed, secret-free refusal."""


def require(condition, message):
    if not condition:
        raise Refusal(message)


def case_ids(os_name):
    require(os_name in TARGETS, "unknown runner OS")
    return COMMON + ONLY[os_name]


def _unique(pairs):
    require(len({k for k, _ in pairs}) == len(pairs), "duplicate JSON key")
    return dict(pairs)


def _finite(constant):
    raise Refusal("non-finite number " + constant)


def json_load(text):
    require(len(text) <= LIMIT, "JSON document too large")
    try:
        return json.loads(text, object_pairs_hook=_unique, parse_constant=_finite)
    except ValueError as error:
        raise Refusal("malformed JSON") from error


def text(value, pattern=r"[A-Za-z0-9._/:,>+ -]{1,160}"):
    return type(value) is str and re.fullmatch(pattern, value) is not None


WORDS = frozenset("""a an and are as at bad be boot-out bootout bootstrap busy by can cannot code command connection could denied directory disabled does domain domain-target enabled engaged enough
    error errors exit expected fail failed failure false file find for found from gui has have in inactive info input integrity invalid is it kickstart label launchctl launchd list load loaded login
    manager many no none not note of ok on open operation option or output path permission permitted plist print print-disabled privileges process protection read request requires
    running service service-target services session signal specified specifier state status such success support supported system target the time timed to too true try type unavailable unknown
    unload unloaded unrecognized unsupported usage user warn warning was while with write yes""".split())  # the ONLY words a hint may hold (the helper keeps the same set); no account-like word (root, admin, ...) is in it
NONPATH = r"Input/output|(?:gui|user|pid)/[0-9]{1,6}"


def pathlike(token):
    """The helper's rule (native-lifecycle-host.py pathlike): a blank-delimited token with a `/` that is not `Input/output` or a launchd domain such as `gui/501`. A hint holding one is refused (a
    private path is not evidence)."""
    return "/" in token and re.fullmatch(NONPATH, token) is None


def safe(token):
    """The helper's rule (native-lifecycle-host.py safe): at most 32 characters and only punctuation, a WORDS word (any case) or a number of at most six digits (with that punctuation around it), a
    non-path spelling, or the markers `<x>` / `<path>`. A name, host, label, encoded or joined text is not evidence a receipt may hold."""
    core = token.strip('{}=()<>"._:-')
    ok = token in ("<x>", "<path>") or core == "" or core.lower() in WORDS or re.fullmatch("[0-9]{1,6}", core) is not None
    return len(token) <= 32 and (ok or re.fullmatch(NONPATH, token) is not None)


def hint_ok(value):
    """The sanitized class, every token safe, no path-like token, and nothing after a `<path>`: the helper replaces everything from a path's first token to the end of the line (a path may hold
    blanks) and every token that is not a known word, number or punctuation by `<x>`."""
    if not text(value, HINT):
        return False
    tokens = value.split(" ")
    return all(safe(token) and not pathlike(token) for token in tokens) and "<path>" not in tokens[:-1]


def diagnostics_agree(doc, os_name):
    """A passing probe passed every stage its diagnostics record, as the driver fails closed on each: the baseline clean, on macOS the gui print and print-disabled admitted by the product's
    grammar with the table within its bound (or unmeasured; the domain listing's NUL and size are evidence, not stages), on Linux no gui stage ran. Hints are not read (a warning on stderr is not a failure)."""
    if any(doc[k] != v for k, v in BASELINE_PASS.items()):
        return False
    if os_name == "Linux":
        return all(doc[k] == "not-run" for k in tuple(GUI_PASS) + GUI_EVIDENCE + GUI_SIZES)
    return all(doc[k] == v for k, v in GUI_PASS.items()) and (doc[TABLE_BOUND] == "not-run" or doc[TABLE_BOUND] <= 65536)


def sha256(path):
    with open(path, "rb") as stream:
        return hashlib.sha256(stream.read()).hexdigest()


def header(kind, ident, os_name):
    sha, run_id, attempt = ident
    doc = {"schema": SCHEMAS[kind], "kind": kind, "candidate_sha": sha, "run_id": run_id, "run_attempt": attempt}
    if os_name:
        doc["runner_os"] = os_name
    return doc


def check_ident(doc, kind, ident, os_name, keys):
    require(type(doc) is dict and set(doc) == set(keys), "%s receipt has the wrong keys" % kind)
    for key, value in header(kind, ident, os_name).items():
        require(type(doc[key]) is str and doc[key] == value, "%s receipt differs from the expected %s" % (kind, key))
    require(type(doc["exit"]) is int and doc["result"] in ("pass", "fail"), "malformed %s result" % kind)



PROBE_KEYS = ("schema", "kind", "candidate_sha", "run_id", "run_attempt", "runner_os", "runner_uid", "exit", "result",
              "failure_phase", "curl_backend") + tuple(ENUM) + DIAG_KEYS


def number(value, high):
    """The integer a fact string spells canonically (no sign, blank or leading zero) when it is within 0..high, else None."""
    ok = type(value) is str and re.fullmatch(r"0|[1-9][0-9]{0,12}", value) is not None and int(value) <= high
    return int(value) if ok else None


def probe_passes(doc, os_name):
    linux = os_name == "Linux"
    # manager_post is the manager the lifecycle uses (macOS gui domain, or the disposable user's on Linux); manager_pre only gates macOS, where they are the same.
    return (doc["exit"] == 0 and doc["manager_post"] == "available" and (linux or doc["manager_pre"] == "available") and doc["cleanup"] == "pass"
            and doc["trust_negative"] == "refused" and doc["trust_positive"] == "accepted"
            and (doc["isolated_uid"], doc["default_target"]) == (("available", "active") if linux else ("not-applicable",) * 2))


def probe_receipt(facts, code, ident, os_name, uid):
    """Initialized `unknown`, filled only by recognized fixed-vocabulary facts: a failed probe still yields a schema-valid receipt."""
    seen = {}
    for line in facts.splitlines():
        key, _, value = line.partition("=")
        seen[key] = value
    doc = dict(header("probe", ident, os_name), runner_uid=uid, exit=code)
    for key, allowed in ENUM.items():
        doc[key] = seen.get(key) if seen.get(key) in allowed else "unknown"
    doc["curl_backend"] = seen.get("curl_backend") if text(seen.get("curl_backend"), r"[A-Za-z0-9._/ -]{1,80}") else "unknown"
    for key, allowed in DIAG_ENUM.items():
        doc[key] = seen.get(key) if seen.get(key) in allowed else "not-run"
    for key, high in DIAG_NUMBER.items():
        doc[key] = "not-run" if number(seen.get(key), high) is None else number(seen.get(key), high)
    for key in DIAG_HINT:
        doc[key] = seen.get(key) if hint_ok(seen.get(key)) else "none"
    for key in DIAG_STATE:
        doc[key] = seen.get(key) if text(seen.get(key), STATE) else "not-run"
    phase = seen.get("phase") if text(seen.get("phase"), r"[a-z0-9-]{1,40}") else "unknown"
    doc["result"] = "pass" if probe_passes(doc, os_name) else "fail"
    doc["failure_phase"] = "none" if doc["result"] == "pass" else phase
    return doc


def check_probe(doc, ident, os_name):
    check_ident(doc, "probe", ident, os_name, PROBE_KEYS)
    require(type(doc["runner_uid"]) is int and doc["runner_uid"] > 0, "malformed runner uid")
    require(all(doc[k] in allowed for k, allowed in ENUM.items()) and text(doc["failure_phase"], r"[a-z0-9-]{1,40}")
            and text(doc["curl_backend"], r"[A-Za-z0-9._/ -]{1,80}"), "malformed probe fields")
    require(all(type(doc[k]) is str and doc[k] in allowed for k, allowed in DIAG_ENUM.items())
            and all(doc[k] == "not-run" or (type(doc[k]) is int and 0 <= doc[k] <= high) for k, high in DIAG_NUMBER.items())
            and all(hint_ok(doc[k]) for k in DIAG_HINT) and all(text(doc[k], STATE) for k in DIAG_STATE), "malformed probe diagnostics")  # evidence: validated, never part of the verdict
    require(doc["result"] == ("pass" if probe_passes(doc, os_name) else "fail"), "probe result contradicts its facts")
    require((doc["result"] == "pass") == (doc["failure_phase"] == "none"), "probe failure_phase contradicts its result")  # a pass names no failure phase, a failure names one
    require(doc["result"] != "pass" or diagnostics_agree(doc, os_name), "probe diagnostics contradict a passing result")  # a pass that records a failed stage is not evidence
    require(doc["result"] == "pass", "the capability probe failed (%s)" % doc["failure_phase"])



PRODUCER_KEYS = ("schema", "kind", "candidate_sha", "run_id", "run_attempt", "runner_os", "tier", "artifact", "a", "b", "exit", "result")


def producer_receipt(release_dir, version, artifact, code, ident):
    """A: the revision under test, one archive and manifest digest per used target. B is null in Q1 (no update is exercised)."""
    sha = ident[0]
    targets = {}
    try:
        with open(os.path.join(release_dir, "checksums.txt"), encoding="utf-8") as stream:
            checks = stream.read()
        for target in TARGETS.values():
            name = "herdr-mobile-relay_%s_%s.tar.gz" % (version, target)
            archive = os.path.join(release_dir, name)
            digest = sha256(archive)
            with tarfile.open(archive) as tar:
                manifest = tar.extractfile("./release-manifest.json").read()
            doc = json_load(manifest.decode())
            ok = "%s  %s\n" % (digest, name) in checks and doc.get("revision") == sha and doc.get("version") == version
            targets[target] = {"archive_sha256": digest if ok else "invalid", "manifest_sha256": hashlib.sha256(manifest).hexdigest()}
        bundle = hashlib.sha256(checks.encode()).hexdigest()
    except (OSError, KeyError, AttributeError, tarfile.TarError, Refusal):
        targets, bundle = {}, "invalid"
    ok = code == 0 and len(targets) == len(TARGETS) and all(t["archive_sha256"] != "invalid" for t in targets.values())
    return dict(header("producer", ident, "Linux"), tier=TIER, artifact={"name": artifact, "digest": bundle},
                a={"commit": sha, "version": version, "targets": targets}, b=None, exit=code, result="pass" if ok else "fail")


def check_producer(doc, ident):
    check_ident(doc, "producer", ident, "Linux", PRODUCER_KEYS)
    a, artifact = doc["a"], doc["artifact"]
    require(doc["tier"] == TIER and doc["b"] is None, "producer tier or B differs (Q1 has no B)")
    require(type(artifact) is dict and set(artifact) == {"name", "digest"} and artifact["name"] == "lifecycle-bundle-%s-%s" % (ident[0], ident[2])
            and re.fullmatch(r"[0-9a-f]{64}", str(artifact["digest"])), "malformed producer artifact, or not this commit's and attempt's bundle")
    require(type(a) is dict and set(a) == {"commit", "version", "targets"} and a["commit"] == ident[0]
            and text(a["version"], r"[0-9.]{5,20}") and type(a["targets"]) is dict and set(a["targets"]) == set(TARGETS.values()),
            "malformed producer A")
    for target in a["targets"].values():
        require(type(target) is dict and set(target) == {"archive_sha256", "manifest_sha256"}
                and all(re.fullmatch(r"[0-9a-f]{64}", str(v)) for v in target.values()), "malformed producer digests")
    require(doc["exit"] == 0 and doc["result"] == "pass", "the producer did not pass")


def check_bundle(producer, bundle, version, ident):
    again = producer_receipt(bundle, version, producer["artifact"]["name"], 0, ident)
    require(again["result"] == "pass" and again["a"] == producer["a"] and again["artifact"] == producer["artifact"],
            "the downloaded bundle differs from the producer receipt")



CASE_KEYS = ("id", "phase", "result", "exit", "before", "after", "readiness", "socket", "https", "unregister", "signals",
             "manual_start", "target", "bystander", "bystander_stable", "cleanup", "mid")
CASES_KEYS = ("schema", "kind", "candidate_sha", "run_id", "run_attempt", "runner_os", "tier", "producer", "cases",
              "failed_case", "cleanup", "exit", "result")


def case_counts(tally, base):
    delta = lambda keys: {k: tally[k] - base[k] for k in keys}
    return delta(("agent.list", "workspace.list")), delta(("front", "child", "matched")), tally["unregister"] - base["unregister"]


# The exact form of every field the host helper emits (all eight of SNAP); a value outside its form is malformed. `unknown` is the helper's explicit answer for a
# value it could not observe, and the rules below refuse it wherever a known state is required. An origin is a path (a systemd origin may add `|dropins=` and
# the space-separated drop-in paths); a manager generation is a launchd manager pid, a systemd user manager `MainPID:InvocationID`, or `none` (no manager).
GENERATION = r"[1-9][0-9]*:[0-9a-f]{32}"  # a running systemd user manager
SNAP_FORMS = {"definition_hash": r"absent|unknown|[0-9a-f]{64}", "mode": r"absent|unknown|[0-7]{4}", "release_revision": r"absent|unknown|[0-9a-f]{40}",
              "enabled": r"true|false|unknown", "active": r"[a-z-]{1,24}", "owned_pid_digest": r"none|[0-9a-f]{64}",  # an unobserved process table (`unknown`) is never recordable
              "origin": r"absent|unknown|/[^\s|]+(\|dropins=/[^\s|]+( /[^\s|]+)*)?",
              "manager_generation": r"none|unknown|[0-9]+|" + GENERATION}


def check_snap(snap):
    require(type(snap) is dict and set(snap) == set(SNAP) and all(type(v) is str and 0 < len(v) <= 1024 for v in snap.values())
            and all(re.fullmatch(form, snap[key]) for key, form in SNAP_FORMS.items()), "malformed state snapshot")


def check_case(case, os_name, sha):
    require(type(case) is dict and set(case) == set(CASE_KEYS), "a case has the wrong keys")
    ident = case["id"]
    require(ident in case_ids(os_name) and text(case["phase"]) and type(case["exit"]) is int, "a case id, phase or exit is invalid")
    for key in ("before", "after"):
        check_snap(case[key])
    before, after = case["before"], case["after"]
    count = lambda mapping, names: type(mapping) is dict and set(mapping) == set(names) and all(type(v) is int and v >= 0 for v in mapping.values())
    require(count(case["socket"], ("agent.list", "workspace.list")) and count(case["https"], ("front", "child", "matched"))
            and type(case["unregister"]) is int and type(case["manual_start"]) is int and case["signals"] == {"sent": 0},
            "malformed case counters (Q1 sends no signal)")
    require(case["result"] == "pass" and case["cleanup"] == "pass" and case["bystander_stable"] is True, "a case failed, left residue or touched the bystander")
    require(re.fullmatch(r"[0-9a-f]{64}|none", case["readiness"]) and re.fullmatch(r"[0-9a-f]{64}", case["bystander"]), "malformed digests")
    # A refusal exits nonzero and every other case exits zero; 124 (timed out), 125 (could not be stopped) and 137 (killed) are neither, so a hang is never a refusal.
    require((case["exit"] != 0) == (ident in REFUSALS) and case["exit"] not in (124, 125, 137),
            "%s: the product exit must be %s, never a timeout" % (ident, "nonzero" if ident in REFUSALS else "zero"))
    require(case["unregister"] == (1 if ident == "a-full-uninstall" else 0), "%s: wrong Herdr unregister count" % ident)
    require(case["target"] == ("active" if (ident, os_name) == ("login-style-relaunch", "Linux") else "n/a"),
            "default.target must be observed active after the Linux login-style restart, and only there")
    if ident in REFUSALS or ident == "bystander-nonmutation":
        # No mutation is proven only by a KNOWN, unchanged state: two identical `unknown` snapshots (two failed observations) prove nothing. manager_generation is not
        # part of the definition state and launchctl managerpid availability is a hosted unknown, so it is deliberately not required to be known.
        known = "unknown" not in [before[k] for k in ("definition_hash", "mode", "active", "origin", "enabled", "release_revision")]
        require(before == after and known, "%s changed, or never observed, the owned definition, origin, state or process" % ident)
    if ident in LIVE:  # socket and https are THIS case's traffic (deltas), so an earlier case's hits cannot satisfy a later one
        https, socket = case["https"], case["socket"]
        require(min(https.values()) >= 1 and min(socket.values()) >= 1 and case["readiness"] != "none"
                and after["release_revision"] == sha and after["owned_pid_digest"] != "none"
                and after["active"] in ("active", "loaded"), "%s lacks its own real HTTPS (front and child), socket and readiness evidence" % ident)
    if ident == "a-install-start":
        require(before["definition_hash"] == before["active"] == "absent" and re.fullmatch(r"[0-9a-f]{64}", after["definition_hash"]) and after["enabled"] == "true",
                "install did not create an enabled definition")
    # The restart proof: the process digest between the manual start and the restart must be a real digest that the final one differs from; no other case has one.
    require((type(case["mid"]) is str and re.fullmatch(r"[0-9a-f]{64}", case["mid"]) is not None and case["mid"] != after["owned_pid_digest"])
            if ident == "a-stop-start-restart" else case["mid"] == "n/a", "%s: wrong or unknown intermediate process digest (the restart proof)" % ident)
    if ident in LIVE[1:]:
        require(after["definition_hash"] == before["definition_hash"] and re.fullmatch(r"[0-9a-f]{64}", after["definition_hash"])
                and after["owned_pid_digest"] != before["owned_pid_digest"], "%s did not relaunch the same definition" % ident)
    if ident == "login-style-relaunch":
        # On Linux both generations must be RUNNING user managers (pid and invocation id) and differ: `none`, `unknown` or any other string is no fresh manager.
        fresh = re.fullmatch(GENERATION, before["manager_generation"]) and re.fullmatch(GENERATION, after["manager_generation"]) and before["manager_generation"] != after["manager_generation"]
        require(case["manual_start"] == 0 and (os_name == "Darwin" or fresh), "the login-style cell used a manual start or no fresh manager generation")
    if ident == "a-full-uninstall":
        require(after["definition_hash"] == after["active"] == after["release_revision"] == "absent" and case["readiness"] == "none", "uninstall left the service or release")


def cases_receipt(records, producer, cleanup, failed, code, ident, os_name):
    """Never `pass` unless every frozen id ran exactly once, nothing failed, cleanup passed and the driver exited 0."""
    if failed == "none" and sorted(r.get("id") for r in records) != sorted(case_ids(os_name)):
        failed = "incomplete"
    done = cleanup == "pass" and failed == "none" and code == 0
    return dict(header("cases", ident, os_name), tier=TIER, producer=producer, cases=records, failed_case=failed,
                cleanup={"result": cleanup}, exit=code, result="pass" if done else "fail")


def check_cases(doc, ident, os_name, producer):
    check_ident(doc, "cases", ident, os_name, CASES_KEYS)
    target = producer["a"]["targets"][TARGETS[os_name]]
    want = {"artifact_digest": producer["artifact"]["digest"], "archive_sha256": target["archive_sha256"], "manifest_sha256": target["manifest_sha256"]}
    require(doc["tier"] == TIER and doc["producer"] == want, "cases receipt is bound to another tier, artifact or archive")
    require(doc["cleanup"] == {"result": "pass"} and doc["exit"] == 0 and doc["failed_case"] == "none" and doc["result"] == "pass", "the lifecycle failed or left residue")
    cases = doc["cases"]
    require(type(cases) is list and all(type(c) is dict for c in cases), "malformed case list")
    got = [c.get("id") for c in cases]
    require(len(got) == len(set(got)), "a case ran twice")
    require(not set(got) - set(case_ids(os_name)), "unexpected case: " + ", ".join(sorted(map(str, set(got) - set(case_ids(os_name))))[:3]))
    require(not set(case_ids(os_name)) - set(got), "missing case: " + ", ".join(sorted(set(case_ids(os_name)) - set(got))[:3]))
    for case in cases:
        check_case(case, os_name, ident[0])
    require(len({c["bystander"] for c in cases}) == 1, "the bystander digest changed between cases")



def read_artifact(directory, name, file):
    path = os.path.join(directory, name)
    require(os.path.isdir(path) and not os.path.islink(path), "missing artifact " + name)
    require(os.listdir(path) == [file], "artifact %s has the wrong files" % name)
    target = os.path.join(path, file)
    require(os.path.isfile(target) and not os.path.islink(target) and 0 < os.path.getsize(target) <= LIMIT, "empty or unsafe artifact " + name)
    with open(target, encoding="utf-8") as stream:
        return json_load(stream.read())


def gate(directory, ident, bundle, version):
    sha, run_id, attempt = ident
    require(re.fullmatch(r"[0-9a-f]{40}", sha) and run_id.isdigit() and attempt.isdigit(), "malformed run identity")
    suffix = "-%s-%s" % (sha, attempt)
    names = {"native-receipt-producer" + suffix: ("producer.json", None, None)}
    for runner, os_name in RUNNERS.items():
        names["native-receipt-probe-%s%s" % (runner, suffix)] = ("probe.json", os_name, check_probe)
        names["native-receipt-cases-%s%s" % (runner, suffix)] = ("cases.json", os_name, None)
    require(os.path.isdir(directory) and sorted(os.listdir(directory)) == sorted(names), "artifact set differs from the expected receipts")
    producer = read_artifact(directory, "native-receipt-producer" + suffix, "producer.json")
    check_producer(producer, ident)
    check_bundle(producer, bundle, version, ident)
    for name, (file, os_name, check) in sorted(names.items()):
        if check:
            check(read_artifact(directory, name, file), ident, os_name)
        elif os_name:
            check_cases(read_artifact(directory, name, file), ident, os_name, producer)


def check_needs(raw, names):
    needs = json_load(raw)
    require(type(needs) is dict and set(needs) == set(names), "unexpected dependency set")
    bad = sorted(n for n in names if type(needs[n]) is not dict or needs[n].get("result") != "success")
    require(not bad, "dependency did not succeed: " + ", ".join(bad))



def write(path, doc):
    with open(path + ".tmp", "x", encoding="utf-8") as stream:
        json.dump(doc, stream, indent=1)
        stream.write("\n")
    os.replace(path + ".tmp", path)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("needs").add_argument("names", nargs="+")
    for name in ("gate", "probe", "producer", "cases", "case-add", "bundle"):
        p = sub.add_parser(name)
        p.add_argument("--sha", required=True)
        p.add_argument("--run-id", required=True)
        p.add_argument("--run-attempt", required=True)
        if name == "gate":
            p.add_argument("--artifacts", required=True)
            p.add_argument("--bundle", required=True)
            p.add_argument("--version", required=True)
            continue
        p.add_argument("--os", default="Linux")
        if name == "case-add":
            for key in ("file", "id", "phase", "exit", "before", "after", "readiness", "counts", "base", "manual-start", "target", "bystander", "stable"):
                p.add_argument("--" + key, required=True)
            p.add_argument("--mid", default="n/a")
            continue
        if name == "bundle":
            p.add_argument("--producer", required=True)
            p.add_argument("--bundle", required=True)
            p.add_argument("--version", required=True)
            continue
        p.add_argument("--exit", type=int, required=True)
        p.add_argument("--output", required=True)
        if name == "probe":
            p.add_argument("--facts", required=True)
            p.add_argument("--uid", type=int, required=True)
        if name == "producer":
            p.add_argument("--release-dir", required=True)
            p.add_argument("--version", required=True)
            p.add_argument("--artifact", required=True)
        if name == "cases":
            p.add_argument("--records", required=True)
            p.add_argument("--producer", required=True)
            p.add_argument("--cleanup", required=True)
            p.add_argument("--failed", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "needs":
            check_needs(os.environ.get("HERDR_NEEDS_JSON", ""), args.names)
        elif args.command == "gate":
            gate(args.artifacts, (args.sha, args.run_id, args.run_attempt), args.bundle, args.version)
        else:
            ident = (args.sha, args.run_id, args.run_attempt)
            if args.command == "case-add":
                socket, https, unregister = case_counts(json_load(args.counts), json_load(args.base))
                case = {"id": args.id, "phase": args.phase, "result": "pass", "exit": int(args.exit), "before": json_load(args.before),
                        "after": json_load(args.after), "readiness": args.readiness, "socket": socket, "https": https,
                        "unregister": unregister, "signals": {"sent": 0}, "manual_start": int(args.manual_start), "target": args.target,
                        "bystander": args.bystander, "bystander_stable": args.stable == "true", "cleanup": "pass", "mid": args.mid}
                check_case(case, args.os, args.sha)
                with open(args.file, "a", encoding="utf-8") as stream:
                    stream.write(json.dumps(case) + "\n")
                return 0
            if args.command == "bundle":
                with open(args.producer, encoding="utf-8") as stream:
                    prod = json_load(stream.read(LIMIT))
                check_producer(prod, ident)
                check_bundle(prod, args.bundle, args.version, ident)
                return 0
            if args.command == "probe":
                with open(args.facts, encoding="utf-8") as stream:
                    doc = probe_receipt(stream.read(1 << 16), args.exit, ident, args.os, args.uid)
            elif args.command == "producer":
                doc = producer_receipt(args.release_dir, args.version, args.artifact, args.exit, ident)
            else:
                with open(args.records, encoding="utf-8") as stream:
                    records = [json_load(line) for line in stream.read(LIMIT).splitlines() if line]
                with open(args.producer, encoding="utf-8") as stream:
                    prod = json_load(stream.read(LIMIT))
                target = prod["a"]["targets"][TARGETS[args.os]]
                bound = {"artifact_digest": prod["artifact"]["digest"], "archive_sha256": target["archive_sha256"], "manifest_sha256": target["manifest_sha256"]}
                doc = cases_receipt(records, bound, args.cleanup, args.failed, args.exit, ident, args.os)
            write(args.output, doc)
            print("%s receipt: result=%s" % (args.command, doc["result"]))
            return 0 if doc["result"] == "pass" else 1
    except (Refusal, OSError, KeyError, TypeError, ValueError) as error:
        print("native lifecycle matrix: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

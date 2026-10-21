#!/usr/bin/env python3
"""Hermetic tests of the matrix/gate and the static wiring of the native-manager lifecycle workflow: only a fresh temporary directory is written; no manager, sudo, relay or network is touched."""
import atexit
import contextlib
import copy
import importlib.util
import io
import json
import os
import pathlib
import hashlib
import random
import itertools
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import time
import unittest.mock

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tests"))
import native_manager_lifecycle_matrix as m  # noqa: E402

GEN1, GEN2 = "7:" + "a" * 32, "8:" + "b" * 32  # two running systemd user-manager generations (MainPID:InvocationID)
SHA, RUN, ATT, VERSION = "a" * 40, "4242", "2", "0.22.4"  # attempt 2: receipts of attempt 1 of the same run are stale
IDENT = (SHA, RUN, ATT)
H = lambda c: c * 64
hashlib_sha256 = lambda body: hashlib.sha256(body).hexdigest()


def make_bundle(path, sha=SHA):
    lines = ""
    for target in m.TARGETS.values():
        name, body = "herdr-mobile-relay_%s_%s.tar.gz" % (VERSION, target), json.dumps({"version": VERSION, "revision": sha}).encode()
        with tarfile.open(os.path.join(path, name), "w:gz") as tar:
            info = tarfile.TarInfo("./release-manifest.json")
            info.size = len(body)
            tar.addfile(info, io.BytesIO(body))
        lines += "%s  %s\n" % (m.sha256(os.path.join(path, name)), name)
    pathlib.Path(path, "checksums.txt").write_text(lines)
    return path


BUNDLE = make_bundle(tempfile.mkdtemp())
atexit.register(shutil.rmtree, BUNDLE, True)
PROD = m.producer_receipt(BUNDLE, VERSION, "lifecycle-bundle-%s-%s" % (SHA, ATT), 0, IDENT)


def bound(os_name):
    target = PROD["a"]["targets"][m.TARGETS[os_name]]
    return {"artifact_digest": PROD["artifact"]["digest"], "archive_sha256": target["archive_sha256"], "manifest_sha256": target["manifest_sha256"]}


def snap(**over):
    return dict({"definition_hash": H("d"), "mode": "0600", "origin": "/owned", "active": "active", "enabled": "true",
                 "release_revision": SHA, "owned_pid_digest": H("1"), "manager_generation": "7"}, **over)


ABSENT = snap(definition_hash="absent", mode="absent", origin="absent", active="absent", enabled="false", release_revision="absent", owned_pid_digest="none")


def case(ident, os_name):
    c = {"id": ident, "phase": "p", "result": "pass", "exit": 0, "before": snap(), "after": snap(), "readiness": H("3"),
         "socket": {"agent.list": 2, "workspace.list": 2}, "https": {"front": 1, "child": 1, "matched": 1}, "unregister": 0,
         "signals": {"sent": 0}, "manual_start": 0, "target": "n/a", "bystander": H("4"), "bystander_stable": True, "cleanup": "pass", "mid": "n/a"}
    if ident == "a-install-start":
        c["before"] = ABSENT
    elif ident in m.LIVE[1:]:
        c["after"] = snap(owned_pid_digest=H("2"), manager_generation="8")
        c["mid"] = H("5") if ident == "a-stop-start-restart" else "n/a"  # the digest between the manual start and the restart
        c["target"] = "active" if (ident, os_name) == ("login-style-relaunch", "Linux") else "n/a"
        if (ident, os_name) == ("login-style-relaunch", "Linux"):
            c["before"], c["after"] = snap(manager_generation=GEN1), snap(owned_pid_digest=H("2"), manager_generation=GEN2)
    elif ident in m.REFUSALS:
        c.update(exit=1, before=ABSENT, after=ABSENT, readiness="none")
    elif ident == "a-full-uninstall":
        c.update(after=ABSENT, readiness="none", unregister=1)
    elif ident == "bystander-nonmutation":
        c.update(before=ABSENT, after=ABSENT, readiness="none")
    return c


def facts(os_name, **over):
    linux = os_name == "Linux"
    base = {"manager_pre": "available", "manager_post": "available", "trust_negative": "refused", "trust_positive": "accepted", "cleanup": "pass",
            "curl_backend": "curl 8.5 OpenSSL/3.0", "phase": "complete", "isolated_uid": "available" if linux else "not-applicable",
            "default_target": "active" if linux else "not-applicable", "linger_initial": "no" if linux else "not-applicable", "linger_changed": "no" if linux else "not-applicable"}
    base.update({k: DIAG[k] for k in m.BASELINE_PASS} if linux else DIAG)  # the diagnostics the driver writes for a pass: the clean baseline everywhere, the gui stages on macOS only
    return "".join("%s=%s\n" % kv for kv in dict(base, **over).items())


DIAG = {"gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "1234", "gui_print_err": "none", "disabled_status": "0", "disabled_bytes": "321", "disabled_admission": "pass",
        "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none", "baseline_failed": "none", "baseline_label_state": "absent", "baseline_legacy_state": "absent"}
HINT_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 ={}"<>()._:/-'  # the sanitized hint class, written out


def good():
    """artifact directory name -> (file name, document) for one complete, passing run."""
    out = {"native-receipt-producer-%s-%s" % (SHA, ATT): ("producer.json", copy.deepcopy(PROD))}
    for runner, os_name in m.RUNNERS.items():
        suffix = "%s-%s-%s" % (runner, SHA, ATT)
        out["native-receipt-probe-" + suffix] = ("probe.json", m.probe_receipt(facts(os_name), 0, IDENT, os_name, 1001))
        out["native-receipt-cases-" + suffix] = ("cases.json", m.cases_receipt([case(i, os_name) for i in m.case_ids(os_name)], bound(os_name), "pass", "none", 0, IDENT, os_name))
    return out


def run_gate(docs, bundle=BUNDLE):
    with tempfile.TemporaryDirectory() as tmp:
        for name, (file, doc) in docs.items():
            os.makedirs(os.path.join(tmp, name))
            with open(os.path.join(tmp, name, file), "w") as stream:
                stream.write(doc if isinstance(doc, str) else json.dumps(doc))
        m.gate(tmp, IDENT, bundle, VERSION)
        return True


def mutated(edit):
    docs = copy.deepcopy(good())
    edit(docs)
    return docs


def key(prefix, runner="ubuntu-24.04"):
    return "native-receipt-%s-%s-%s-%s" % (prefix, runner, SHA, ATT)


class Matrix(unittest.TestCase):
    def test_frozen_ids_and_applicability(self):
        common = ("a-install-start", "a-stop-start-restart", "login-style-relaunch", "bad-token", "bad-policy", "public-wizard-premutation",
                  "gateway-missing-app", "manager-missing-injected", "a-full-uninstall", "bystander-nonmutation")
        self.assertEqual(m.case_ids("Darwin"), common + ("foreign-launchd-path",))
        self.assertEqual(m.case_ids("Linux"), common + ("foreign-systemd-fragment", "foreign-systemd-dropin"))
        self.assertEqual(m.SCHEMAS, {"probe": "native-capability-v1", "producer": "native-producer-v1", "cases": "native-cases-v1"})

    def test_a_complete_run_passes(self):
        self.assertTrue(run_gate(good()))

    def test_gate_refuses_each_defect(self):
        linux, mac, prod, probe = key("cases"), key("cases", "macos-15"), "native-receipt-producer-%s-%s" % (SHA, ATT), key("probe")
        edit = lambda name, fn: lambda d: fn(d[name][1])
        up = lambda name, **kw: edit(name, lambda x: x.update(kw))
        cu = lambda i, name=linux, **kw: edit(name, lambda x: x["cases"][i].update(kw))
        put = lambda name, file, doc: lambda d: d.__setitem__(name, (file, doc))
        bad_probe = lambda code, **kw: put(probe, "probe.json", m.probe_receipt(facts("Linux", **kw), code, IDENT, "Linux", 1001))
        defects = {
            "absent artifact": lambda d: d.pop(probe),
            "absent producer": lambda d: d.pop(prod),
            "extra artifact": lambda d: d.__setitem__("native-receipt-cases-extra-%s-%s" % (SHA, ATT), d[linux]),
            "empty file": put(linux, "cases.json", ""),
            "malformed JSON": put(linux, "cases.json", "{"),
            "duplicate key": put(linux, "cases.json", '{"a": 1, "a": 2}'),
            "extra file": lambda d: d.__setitem__(linux, ("other.json", d[linux][1])),
            "failed probe receipt": bad_probe(1),
            "probe trust unknown": bad_probe(0, trust_positive="refused"),
            "probe pass that contradicts its facts": up(probe, trust_negative="accepted"),
            "probe pass that names a failure phase": up(probe, failure_phase="manager"),
            "stale sha": up(linux, candidate_sha="b" * 40),
            "stale run": up(linux, run_id="1"),
            "stale attempt": up(linux, run_attempt="1"),
            "wrong OS": up(linux, runner_os="Darwin"),
            "wrong tier": up(linux, tier="m3"),
            "cleanup failure": up(linux, cleanup={"result": "fail"}),
            "failed case": up(linux, failed_case="bad-token"),
            "driver exit": up(linux, exit=1),
            "other archive": edit(linux, lambda x: x["producer"].update(archive_sha256=H("9"))),
            "other artifact": edit(mac, lambda x: x["producer"].update(artifact_digest=H("9"))),
            "missing case": edit(linux, lambda x: x["cases"].pop()),
            "duplicate case": edit(linux, lambda x: x["cases"].append(x["cases"][0])),
            "extra case": edit(mac, lambda x: x["cases"].append(case("foreign-systemd-dropin", "Linux"))),
            "cross-OS case": cu(-1, mac, id="foreign-systemd-fragment"),
            "failed case result": cu(0, result="fail"),
            "front-only HTTPS hit": edit(linux, lambda x: x["cases"][0]["https"].update(child=0)),
            "no socket traffic": edit(linux, lambda x: x["cases"][0]["socket"].update({"agent.list": 0})),
            "bystander changed": cu(1, bystander=H("8")),
            "bystander unstable": cu(1, bystander_stable=False),
            "refusal that exited zero": cu(3, exit=0),
            "refusal that mutated": cu(3, after=snap()),
            "refusal that timed out": cu(3, exit=124),
            "refusal that could not be stopped": cu(3, exit=125),
            "refusal with identical unknown snapshots": cu(3, before=snap(active="unknown", origin="unknown"), after=snap(active="unknown", origin="unknown")),
            "refusal with identical unknown definition hash": cu(3, before=snap(definition_hash="unknown"), after=snap(definition_hash="unknown")),
            "refusal with identical unknown mode": cu(3, before=snap(mode="unknown"), after=snap(mode="unknown")),
            "refusal with identical unknown enabled state": cu(3, before=snap(enabled="unknown"), after=snap(enabled="unknown")),
            "refusal with identical unknown release revision": cu(3, before=snap(release_revision="unknown"), after=snap(release_revision="unknown")),
            "refusal with identical unknown process digest": cu(3, before=snap(owned_pid_digest="unknown"), after=snap(owned_pid_digest="unknown")),
            "snapshot whose hash is free text": cu(0, after=snap(definition_hash="not-a-hash")),
            "snapshot whose mode is free text": cu(0, after=snap(mode="rw-r--r--")),
            "restart with an unknown intermediate digest": cu(1, mid="unknown"),
            "restart whose intermediate digest is the final one": cu(1, mid=H("2")),
            "restart without an intermediate digest": cu(1, mid="n/a"),
            "intermediate digest outside the restart case": cu(0, mid=H("5")),
            "login with a manual start": cu(2, manual_start=1),
            "login without default.target evidence": cu(2, target="n/a"),
            "target claimed outside the Linux login cell": cu(0, target="active"),
            "login without a fresh manager": edit(linux, lambda x: x["cases"][2]["after"].update(manager_generation=GEN1)),
            "login whose new manager generation is none": edit(linux, lambda x: x["cases"][2]["after"].update(manager_generation="none")),
            "login whose new manager generation is unknown": edit(linux, lambda x: x["cases"][2]["after"].update(manager_generation="unknown")),
            "login whose generations are bare numbers": edit(linux, lambda x: x["cases"][2].update(before=snap(manager_generation="7"), after=snap(owned_pid_digest=H("2"), manager_generation="8"))),
            "snapshot whose manager generation is free text": cu(0, after=snap(manager_generation="the new one")),
            "snapshot whose origin is free text": cu(0, after=snap(origin="not a path")),
            "refusal whose origin is not a path": cu(3, before=snap(origin="x"), after=snap(origin="x")),
            "bystander digest unknown": cu(1, bystander="unknown"),
            "install with an unobserved definition hash": cu(0, after=snap(definition_hash="unknown")),
            "relaunch with identical unobserved definition hashes": cu(1, before=snap(definition_hash="unknown"), after=snap(definition_hash="unknown", owned_pid_digest=H("2"))),
            "producer digest not from the bundle": edit(prod, lambda x: x["a"]["targets"]["linux_amd64"].update(archive_sha256=H("9"))),
            "producer artifact of another attempt": edit(prod, lambda x: x["artifact"].update(name="lifecycle-bundle-x")),
            "uninstall without unregister": cu(8, unregister=0),
            "uninstall that left the release": edit(linux, lambda x: x["cases"][8]["after"].update(release_revision=SHA)),
            "install without a definition": edit(linux, lambda x: x["cases"][0]["after"].update(definition_hash="absent")),
            "signal sent": cu(0, signals={"sent": 1}),
            "wrong revision": edit(linux, lambda x: x["cases"][0]["after"].update(release_revision="c" * 40)),
            "unknown key": cu(0, extra=1),
        }
        for name, fn in defects.items():
            with self.subTest(name), self.assertRaises(m.Refusal):
                run_gate(mutated(fn))

    def test_gate_re_hashes_the_bundle_itself(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bundle in (make_bundle(tmp, sha="b" * 40), tmp + "/missing"):  # another revision's archives; no bundle at all
                with self.assertRaises(m.Refusal):
                    run_gate(good(), bundle)

    def test_case_add_records_this_cases_traffic_not_the_cumulative_counters(self):
        c = case("a-stop-start-restart", "Linux")
        tally = {"agent.list": 9, "workspace.list": 9, "front": 3, "child": 3, "matched": 3, "unregister": 0}

        def add(base):
            with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stderr(io.StringIO()):
                out = os.path.join(tmp, "c.jsonl")
                code = m.main(["case-add", "--file", out, "--os", "Linux", "--sha", SHA, "--run-id", RUN, "--run-attempt", ATT, "--id", c["id"], "--phase", "p",
                               "--exit", "0", "--before", json.dumps(c["before"]), "--after", json.dumps(c["after"]), "--readiness", c["readiness"],
                               "--counts", json.dumps(tally), "--base", json.dumps(base), "--manual-start", "0", "--target", "n/a", "--bystander", c["bystander"], "--stable", "true", "--mid", c["mid"]])
                return code, json.loads(open(out).read())["https"] if code == 0 else None
        self.assertEqual(add(tally)[0], 1)  # no new traffic in this case: refused although every cumulative counter is positive
        self.assertEqual(add(dict(tally, front=2, child=2, matched=2, **{"agent.list": 8}))[0], 1)  # no new workspace.list traffic either
        self.assertEqual(add(dict(tally, front=2, child=2, matched=2, **{"agent.list": 8, "workspace.list": 8})), (0, {"front": 1, "child": 1, "matched": 1}))

    def test_probe_receipt_is_always_valid_and_never_a_converted_failure(self):
        for code, extra in ((1, {}), (0, {"cleanup": "fail"}), (0, {"manager_post": "bogus"}), (1, {"phase": "BAD PHASE!"}), (124, {"default_target": "inactive"})):
            doc = m.probe_receipt(facts("Linux", **extra), code, IDENT, "Linux", 1001)
            self.assertEqual(doc["result"], "fail", (code, extra))
            self.assertEqual(set(doc), set(m.PROBE_KEYS))
            with self.assertRaises(m.Refusal):
                m.check_probe(doc, IDENT, "Linux")
        empty = m.probe_receipt("", 2, IDENT, "Darwin", 501)  # a probe that died before any observation
        self.assertEqual((empty["result"], empty["manager_post"], empty["failure_phase"]), ("fail", "unknown", "unknown"))
        self.assertEqual(m.probe_receipt(facts("Darwin"), 0, IDENT, "Darwin", 501)["failure_phase"], "none")
        # The failure phase is bound to the result in both directions: a passing receipt names none, a failing one names a phase (the gate refuses both contradictions).
        good_probe = m.probe_receipt(facts("Linux"), 0, IDENT, "Linux", 1001)
        m.check_probe(good_probe, IDENT, "Linux")
        failed = m.probe_receipt(facts("Linux", cleanup="fail"), 1, IDENT, "Linux", 1001)
        for contradiction in (dict(good_probe, failure_phase="manager"), dict(good_probe, failure_phase="unknown"), dict(failed, failure_phase="none")):
            with self.assertRaisesRegex(m.Refusal, "failure_phase contradicts its result"):
                m.check_probe(contradiction, IDENT, "Linux")

    def test_probe_diagnostics_are_strictly_validated_evidence_and_never_the_verdict(self):
        mac = lambda code=0, **over: m.probe_receipt(facts("Darwin", **dict(DIAG, **over)), code, IDENT, "Darwin", 501)
        # Well-formed diagnostics: integers for statuses and sizes, strings for the rest, every key exactly once; the gate accepts them.
        doc = mac()
        self.assertEqual(set(doc), set(m.PROBE_KEYS))
        self.assertTrue(set(m.DIAG_KEYS) < set(m.PROBE_KEYS) and len(m.DIAG_KEYS) == 13)
        self.assertEqual([doc[k] for k in ("gui_print_status", "gui_bytes", "disabled_status", "disabled_bytes")], [0, 1234, 0, 321])
        self.assertEqual([doc[k] for k in ("gui_nul", "disabled_admission", "disabled_head", "disabled_tail", "gui_print_err", "disabled_err", "baseline_failed", "baseline_label_state")],
                         ["clean", "pass", "disabled services = {", "}", "none", "none", "none", "absent"])
        m.check_probe(doc, IDENT, "Darwin")
        # A passing Linux probe runs no gui stage (those are `not-run`, every hint `none`) and records its clean baseline; a probe that stopped before its first stage records nothing and fails.
        bare = m.probe_receipt(facts("Linux"), 0, IDENT, "Linux", 1001)
        self.assertEqual({k: bare[k] for k in m.DIAG_KEYS}, dict({k: "not-run" for k in m.DIAG_KEYS}, **{k: "none" for k in m.DIAG_HINT}, **m.BASELINE_PASS))
        m.check_probe(bare, IDENT, "Linux")
        self.assertEqual({k: m.probe_receipt("", 2, IDENT, "Linux", 1001)[k] for k in m.DIAG_KEYS}, dict({k: "not-run" for k in m.DIAG_KEYS}, **{k: "none" for k in m.DIAG_HINT}))
        self.assertFalse(set(m.DIAG_KEYS) & (set(m.CASES_KEYS) | set(m.CASE_KEYS)))  # the lifecycle receipt does not share them
        # Every value domain at its edges, built from FACTS: a recognised value is kept, anything else becomes the stage default (a failed probe still yields a valid receipt).
        edge = [("gui_print_status", v, w) for v, w in (("0", 0), ("1", 1), ("113", 113), ("255", 255), ("256", "not-run"), ("-1", "not-run"), ("007", "not-run"), ("+1", "not-run"), (" 1", "not-run"),
                                                          ("1.0", "not-run"), ("", "not-run"), ("\u0661", "not-run"), ("not-run", "not-run"), ("x", "not-run"))]
        edge += [("disabled_status", "255", 255), ("disabled_status", "256", "not-run")]
        edge += [("gui_bytes", v, w) for v, w in (("0", 0), ("65537", 65537), ("1000000000000", 10 ** 12), ("1000000000001", "not-run"), ("9" * 14, "not-run"), ("-5", "not-run"), ("1e3", "not-run"))]
        edge += [("disabled_bytes", "0", 0), ("disabled_bytes", "1000000000001", "not-run")]
        edge += [("gui_nul", v, v if v in ("clean", "nul", "not-run") else "not-run") for v in ("clean", "nul", "not-run", "unknown", "Clean", "", "clean ")]
        edge += [("disabled_admission", v, v if v != "bogus" else "not-run") for v in ("pass", "over-limit", "nul", "header", "trailer", "empty", "not-run", "bogus")]
        edge += [("baseline_failed", v, v) for v in m.DIAG_ENUM["baseline_failed"]] + [("baseline_failed", "path-other", "not-run"), ("baseline_failed", "path-conf ", "not-run"), ("baseline_failed", "", "not-run")]
        for key_ in m.DIAG_HINT:
            edge += [(key_, v, v) for v in ("none", "disabled services = {", "<x> => true", "<>()._:-", " ", "}", "Input/output error", "gui/501", "user/501", "pid/12", "<path>", "cannot read <path>",
                                          "Could not find domain for user gui: 501", "Bootstrap failed: 5: Input/output error", "  could", "COULD NOT 501)", "could " * 10, "{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{")]
            edge += [(key_, v, "none") for v in ("", "x" * 65, "a;b", "a\tb", "a,b", "caf\u00e9", "a'b", "a\\b", "[x]", "a\x00b", "%2FUsers%2Frunner")]  # (a CR or other line separator would end the facts line itself)
            edge += [(key_, v, "none") for v in ("/", "/etc/passwd", "x/y/z", "a/", "a /b", "denied /Users/r/x", "../x", "./x", "https://h/x", "-/-", "<>()._:/-", "a//b", "gui/501/x", "a/b", "input/output", "gui/", "gui/5x")]  # a path is not evidence
            edge += [(key_, v, "none") for v in ("<path> denied", "x <path> y", "<path> <path>", "/Users/John Smith/x", "Smith/x", "My Docs/x", "Library/Application Support/x")]  # nor is anything after a path's first token
            edge += [(key_, v, "none") for v in ('"com.apple.x" => true', "x" * 64, HINT_CHARS[:64], "2FUsers2Frunner2Fsecret", "launchctl error 2FUsers2Frunner2Fsecret", "host john-macbook.local denied",
                                                 "user john", "john", "Users", "x", "<x", "<xx>", "<x>:", "gui/1234567", "could " * 11, "{" * 33, "Couldnot", "could.not", "domain-targets", "gui/501:")]  # nor a name, host, encoding or joined word
        for key_ in m.DIAG_STATE:
            edge += [(key_, v, v) for v in ("absent", "unknown", "loaded", "active", "not-run", "a" * 24)] + [(key_, v, "not-run") for v in ("", "a" * 25, "Active", "a b", "a1", "a_b", "loaded;")]
        for name, value, want in edge:
            got = mac(**{name: value})[name]
            self.assertEqual((type(got), got), (type(want), want), (name, value))
        # The gate REFUSES anything outside a domain: a typed value of the wrong type or range (bool is not an integer), an enum or hint outside its class, a missing or an extra key.
        good_probe = mac()
        bad = {k: ["unknown", "", None, 1, True, ["clean"], "Clean", "clean "] for k in m.DIAG_ENUM}
        bad.update({k: [256, -1, True, False, 1.5, "1", "", None, [], {}, "unknown"] for k in ("gui_print_status", "disabled_status")})
        bad.update({k: [10 ** 12 + 1, -1, True, 2.0, "5", "", None, "none"] for k in ("gui_bytes", "disabled_bytes")})
        bad.update({k: ["", "x" * 65, "a;b", "a\nb", "a\tb", "caf\u00e9", None, 1, ["none"], "a\x00b", "a\r", "/PRIVATE/HOME/Library/LaunchAgents/private.plist: denied", "a/b/c", "x /y", "./x", "a/", "<path> denied", "x <path> y", "Smith/x", "a/b",
                          "2FUsers2Frunner2Fsecret", "launchctl error 2FUsers2Frunner2Fsecret", "host john-macbook.local denied", "x" * 64, '"com.apple.x" => true', "{" * 33] for k in m.DIAG_HINT})
        bad.update({k: ["", "a" * 25, "Active", "a b", None, 1, ["absent"], "a\n"] for k in m.DIAG_STATE})
        self.assertEqual(set(bad), set(m.DIAG_KEYS))
        for name, values in bad.items():
            for value in values:
                with self.assertRaisesRegex(m.Refusal, "malformed probe diagnostics", msg=(name, value)):
                    m.check_probe(dict(good_probe, **{name: value}), IDENT, "Darwin")
        for name in m.DIAG_KEYS:  # a missing key and an extra key are refused as any other receipt key is
            with self.assertRaisesRegex(m.Refusal, "wrong keys"):
                m.check_probe({k: v for k, v in good_probe.items() if k != name}, IDENT, "Darwin")
        with self.assertRaisesRegex(m.Refusal, "wrong keys"):
            m.check_probe(dict(good_probe, gui_extra="x"), IDENT, "Darwin")
        # The verdict is the unchanged rule over the unchanged facts. Diagnostics that look perfect rescue no failing probe; alarming ones fail no passing probe (the existing result/facts and
        # failure_phase contradictions are still refused, with diagnostics present).
        for extra in ({"manager_pre": "unavailable"}, {"manager_post": "unavailable"}, {"manager_post": "unknown"}, {"cleanup": "fail"}, {"trust_negative": "accepted"}, {"trust_positive": "refused"}):
            failing = mac(**extra)
            self.assertEqual((failing["result"], failing["failure_phase"]), ("fail", "complete"), extra)
            with self.assertRaisesRegex(m.Refusal, "the capability probe failed"):
                m.check_probe(failing, IDENT, "Darwin")
            with self.assertRaisesRegex(m.Refusal, "contradicts its facts"):
                m.check_probe(dict(failing, result="pass", failure_phase="none"), IDENT, "Darwin")
        self.assertEqual(mac(1)["result"], "fail")  # a nonzero driver exit
        # A PASSING receipt that records a failed stage is a contradiction the gate REFUSES (the driver fails closed on each stage; the builder's verdict is unchanged, check_probe is what refuses it).
        alarming = dict(gui_print_status="113", gui_nul="nul", disabled_admission="header", baseline_failed="label-active", baseline_label_state="loaded", disabled_err="Could not find domain", disabled_head="x")
        stages = [(k, v) for k in ("gui_print_status", "disabled_status") for v in ("1", "113", "255", "not-run")] + [("gui_nul", v) for v in ("nul", "not-run")]
        stages += [("disabled_admission", v) for v in m.DIAG_ENUM["disabled_admission"][1:]] + [("gui_bytes", "65537"), ("disabled_bytes", "65537")]
        stages += [("baseline_failed", v) for v in m.DIAG_ENUM["baseline_failed"][1:]] + [(k, v) for k in m.DIAG_STATE for v in ("loaded", "unknown", "not-run")]
        for name, value in stages + [("combined", alarming)]:
            over = value if name == "combined" else {name: value}
            passing = mac(**over)
            self.assertEqual((passing["result"], passing["failure_phase"]), ("pass", "none"), over)
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=over):
                m.check_probe(passing, IDENT, "Darwin")
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=over):
                run_gate(mutated(lambda d: d.__setitem__(key("probe", "macos-15"), ("probe.json", passing))))
        for gui_stage in ("gui_print_status", "gui_nul", "gui_bytes", "disabled_status", "disabled_bytes", "disabled_admission"):  # Linux runs no gui stage: any recorded value is a contradiction
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=gui_stage):
                m.check_probe(m.probe_receipt(facts("Linux", **{gui_stage: DIAG[gui_stage]}), 0, IDENT, "Linux", 1001), IDENT, "Linux")
        for name, value in (("baseline_failed", "hosts"), ("baseline_label_state", "loaded"), ("baseline_legacy_state", "not-run")):
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=name):
                m.check_probe(m.probe_receipt(facts("Linux", **{name: value}), 0, IDENT, "Linux", 1001), IDENT, "Linux")
        for ok in ({"gui_bytes": "not-run"}, {"disabled_bytes": "not-run"}, {"gui_bytes": "65536", "disabled_bytes": "65536"}, {"gui_print_err": "warn: <x>", "disabled_err": "bad request", "disabled_head": "}", "disabled_tail": "x"}):
            m.check_probe(mac(**ok), IDENT, "Darwin")  # unmeasured sizes, sizes at the bound and any valid hint are no failed stage
        with self.assertRaisesRegex(m.Refusal, r"the capability probe failed \(complete\)"):
            m.check_probe(mac(manager_pre="unavailable", **alarming), IDENT, "Darwin")  # a failing probe with failed stages is still refused as a failure (the diagnostics explain it)
        original = lambda d, linux: (d["exit"] == 0 and d["manager_post"] == "available" and (linux or d["manager_pre"] == "available") and d["cleanup"] == "pass" and d["trust_negative"] == "refused"
                                     and d["trust_positive"] == "accepted" and (d["isolated_uid"], d["default_target"]) == (("available", "active") if linux else ("not-applicable",) * 2))
        sample = itertools.product((0, 1), ("available", "unavailable", "unknown"), ("available", "unknown"), ("pass", "fail"), ("refused", "accepted"), ("accepted", "refused"), (True, False))
        for exit_code, pre, post, cleanup, negative, positive, applicable in sample:
            for os_name in ("Linux", "Darwin"):
                base = {"exit": exit_code, "manager_pre": pre, "manager_post": post, "cleanup": cleanup, "trust_negative": negative, "trust_positive": positive,
                        "isolated_uid": "available" if (os_name == "Linux") == applicable else "not-applicable", "default_target": "active" if (os_name == "Linux") == applicable else "not-applicable"}
                for diag in ({}, {k: v for k, v in doc.items() if k in m.DIAG_KEYS}, {k: "not-run" for k in m.DIAG_KEYS}):
                    self.assertEqual(m.probe_passes(dict(base, **diag), os_name), original(base, os_name == "Linux"), (base, diag))

    def test_gate_accepts_well_formed_probe_diagnostics_and_refuses_malformed_or_extra_ones(self):
        mac, fresh = key("probe", "macos-15"), lambda **over: m.probe_receipt(facts("Darwin", **dict(DIAG, **over)), 0, IDENT, "Darwin", 501)
        self.assertTrue(run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", fresh())))))
        self.assertTrue(run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", fresh(disabled_head="}", gui_print_err="Could not find domain for user gui: 501"))))))  # hints are evidence, never a failed stage
        for name, edit in {"out-of-range status": lambda x: x.update(gui_print_status=300), "boolean size": lambda x: x.update(gui_bytes=True), "free-text enum": lambda x: x.update(gui_nul="maybe"),
                           "unsanitized hint": lambda x: x.update(disabled_head="a;b"), "encoded path hint": lambda x: x.update(gui_print_err="launchctl error 2FUsers2Frunner2Fsecret"),
                           "host name hint": lambda x: x.update(disabled_err="host john-macbook.local denied"), "overlong hint": lambda x: x.update(disabled_tail="x" * 65), "empty state": lambda x: x.update(baseline_label_state=""),
                           "missing key": lambda x: x.pop("baseline_failed"), "extra key": lambda x: x.update(extra="x"),
                           "string status": lambda x: x.update(disabled_status="0")}.items():
            doc = fresh()
            edit(doc)
            with self.subTest(name), self.assertRaises(m.Refusal):
                run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", doc))))
        failed = m.probe_receipt(facts("Darwin", **dict(DIAG, manager_pre="unavailable")), 1, IDENT, "Darwin", 501)  # all-clean diagnostics, a failed probe: the gate still refuses it
        with self.assertRaisesRegex(m.Refusal, r"the capability probe failed \(complete\)"):
            run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", failed))))

    def test_producer_receipt_binds_archives_checksums_and_revision(self):
        m.check_producer(PROD, IDENT)
        with tempfile.TemporaryDirectory() as tmp:
            make_bundle(tmp)
            for ident, version in (((SHA[:-1] + "b", RUN, ATT), VERSION), (IDENT, "0.22.5")):  # another revision, another version
                with self.assertRaises(m.Refusal):
                    m.check_producer(m.producer_receipt(tmp, version, "x", 0, ident), ident)
            pathlib.Path(tmp, "checksums.txt").write_text(pathlib.Path(tmp, "checksums.txt").read_text().replace("0", "1", 1))
            with self.assertRaises(m.Refusal):
                m.check_producer(m.producer_receipt(tmp, VERSION, "x", 0, IDENT), IDENT)
            self.assertEqual(m.producer_receipt(tmp + "/missing", VERSION, "x", 0, IDENT)["result"], "fail")

    def test_needs_gate_refuses_any_unsuccessful_dependency(self):
        names = ("decide", "probe", "producer", "lifecycle")
        ok = {n: {"result": "success"} for n in names}
        m.check_needs(json.dumps(ok), names)
        for broken in [json.dumps(dict(ok, probe={"result": r})) for r in ("failure", "skipped", "cancelled")] + ["", "[]", "{", json.dumps({k: ok[k] for k in names[:2]})]:
            with self.assertRaises(m.Refusal):
                m.check_needs(broken, names)


class Wiring(unittest.TestCase):
    def text(self, path):
        return (ROOT / path).read_text()

    def test_decide_marker_dispatch_and_gate_wiring(self):
        flow = self.text(".github/workflows/native-manager-lifecycle.yml")
        script = textwrap.dedent(flow.split("        run: |\n", 1)[1].split("\n\n", 1)[0])
        self.assertEqual(script.count('>> "$GITHUB_OUTPUT"'), 1)

        def decided(event, message, requested=""):
            with tempfile.TemporaryDirectory() as tmp:
                env = {"PATH": "/usr/bin:/bin", "EVENT_NAME": event, "HEAD_MESSAGE": message, "REQUESTED_SHA": requested, "GITHUB_SHA": SHA,
                       "GITHUB_OUTPUT": os.path.join(tmp, "out")}
                done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script], capture_output=True, text=True, env=env)
                return done.returncode, pathlib.Path(env["GITHUB_OUTPUT"]).read_text().strip() if done.returncode == 0 else ""
        self.assertEqual(decided("push", "ci: x\n\nbody [native-lifecycle] tail"), (0, "run=true"))
        for message in ("", "ci: x", "[native-lifecycle", "[Native-Lifecycle]", "[native-lifecycle ]", "[full-qualification]"):
            self.assertEqual(decided("push", message), (0, "run=false"), message)  # a markerless push is skipped, never red and never qualification
        self.assertEqual(decided("workflow_dispatch", ""), (0, "run=true"))
        self.assertEqual(decided("workflow_dispatch", "", SHA), (0, "run=true"))
        self.assertEqual(decided("workflow_dispatch", "", "b" * 40)[0], 1)
        for text in ("name: Lifecycle, partial Q1", "needs: [decide, probe, producer]", "needs: [decide, probe, producer, lifecycle]",
                     "if: ${{ always() && needs.decide.outputs.run != 'false' }}", "branches: [ci/tailscale-native-preflight-ef9f843]"):
            self.assertIn(text, flow)
        upload = flow.split("Upload the probe receipt (pass or fail)", 1)[1].split("- name:", 1)[0]
        for text in ("if: always()", "if-no-files-found: error", "retention-days: 1", "native-evidence/probe.json"):
            self.assertIn(text, upload)  # the failure receipt is uploaded, and a missing one is an explicit error
        self.assertEqual(flow.count("run: /bin/bash scripts/ci/run-native-lifecycle.sh"), 3)
        gate = flow.split("  gate:\n", 1)[1]
        for text in ("name: lifecycle-bundle-${{ github.sha }}-${{ github.run_attempt }}", "--bundle native-bundle", "--version"):
            self.assertIn(text, gate)  # the gate downloads and re-hashes the bundle itself

    def test_driver_sudo_is_only_the_listed_bounded_operations_and_root_runs_no_product_script(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        priv = driver.split("\npriv() {", 1)[1].split("\n}\n", 1)[0]
        for line in driver.replace(priv, "").splitlines():
            if "sudo" in line and not line.lstrip().startswith("#"):  # only root's own bounded runner and the product-as-subject wrapper (whose timeout runs as the test UID)
                self.assertTrue(line.startswith('bsudo() { sudo -n "$PY" -I -B "$HOSTPY" run --timeout 120 -- "$@"; }') or 'sudo -n -u "$SUBJ" /usr/bin/timeout -k 5 "$ASTO" env -i' in line, line)
        self.assertEqual(re.findall(r"sudo -n (\S+)", priv), ['"$PY"'])  # the long-lived TLS front is the one unbounded privileged call
        self.assertEqual(sorted(set(re.findall(r"bsudo (\S+)", priv))),
                         sorted(["useradd", "userdel", "loginctl", "systemctl", "tee", "install", "update-ca-certificates", "rm", "security"]))
        for forbidden in ("service.sh", "install.sh", "uninstall.sh", "sh -c"):
            self.assertNotIn(forbidden, priv)
        self.assertIsNone(re.search(r"sudo -n (bash|sh|/bin/)", priv))  # root runs no shell either
        for text in ("loginctl terminate-user \"$SUBJ\"", "systemctl start \"user@$SUID.service\"", "is-active --quiet \"$LABEL\"", "default.target did not activate the installed unit by itself"):
            self.assertIn(text, driver)
        self.assertNotIn("systemctl start \"$LABEL\"", driver.split("case_login_relaunch() {", 1)[1].split("\n}\n", 1)[0])  # no manual unit start in the login cell
        self.assertTrue("HERDR_BIN" not in driver and "OX run" not in driver)  # never HERDR_BIN; no unprivileged wrapper (it cannot signal a root- or test-UID-owned group)
        writer = driver.split("write_env() {", 1)[1].split("\n}\n", 1)[0]
        self.assertIsNone(re.search(r"(?i)proxy|CA_BUNDLE|SSL_CERT|CURL_", writer))  # no proxy or CA override can reach the product through relay.env
        self.assertIn("HERDR_SOCKET_PATH=", driver)
        self.assertIn('rest == ["plugin", "uninstall", "herdr-mobile-relay.events"]', self.text("scripts/ci/native-lifecycle-host.py"))

    def test_cleanup_fails_closed_and_refusals_hash_every_byte(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        part = lambda name: driver.split(name + "() {", 1)[1].split("\n}\n", 1)[0]
        self.assertLess(part("cl_fixtures").index("*) return 1 ;;"), part("cl_fixtures").index("rm -f"))  # an unknown bystander identity is retained, never deleted
        hosts = part("cl_hosts")  # only grep's exit 1 proves absence, and the privileged full-file write only follows a validated replacement
        self.assertTrue(hosts.index("1) return 0 ;; 0) ;; *) return 1 ;; esac") < hosts.index("-eq 1 ] || return 1") < hosts.index("priv hosts-del"))
        self.assertTrue(driver.count('grep -qF "$HOSTN"') == 1 and not any(bad in driver for bad in ("! grep", "! security", "| grep -qi", "! listening", "! as_subject", "kill -0", "! ps ", "! loginctl")))  # every other absence check is a definite none()/absent()/unlistened()
        self.assertTrue('gone "$linger"' in part("cl_user") and "|| bad=1" in part("cl_user"))  # definite absence of the linger file, never `[ ! -e ]`
        for name in ("cl_user", "cl_stage"):  # no callback deletes a home or stage that holds a definition an earlier callback retained
            self.assertIn('[ "$clean" = pass ] || return 1', part(name))
        stage, foreign = part("stage_base"), part("cl_foreign")  # a stage is registered first but removed only once an atomic mkdir proved this run created it; a foreign fixture only once its identity is verified
        self.assertTrue(driver.count("on_cleanup cl_stage") == 1 and stage.index("on_cleanup cl_stage") < stage.index('mkdir "$1"') < stage.index("STAGE_OWNED=yes") and '[ "$STAGE_OWNED" = yes ] || return 0' in driver)
        self.assertTrue(foreign.index('jget "$f"') < foreign.index("jget owned_pid_digest") < foreign.index("rm -rf"))
        self.assertIn('"BIN_DIR=$SHIMDIR" "HERDR_RELAY_BIN_DIR=$SHIMDIR"', part("senv_init"))  # install.sh and uninstall.sh place the product shim in SHIMDIR
        self.assertTrue("treeh)" in part("expect_refusal") and "0 | 124 | 125 | 137)" in part("expect_refusal"))  # a timed-out or unstoppable command is never a refusal
        self.assertIn('SHIMDIR="$RUNNER_TEMP/nlt-stage/bin"', driver)  # macOS shims and stubs never touch the runner's real ~/.local/bin
        self.assertNotIn('"$THOME/.local/bin/curl"', driver)
        self.assertNotIn("! id ", driver)  # an account lookup is definite (getent exit 2) or unknown, never a negated `id`
        cleanup = {"cl_user": 'getent passwd "$SUBJ"', "cl_product": "residue_gone", "cl_foreign": '[ "$v" != unknown ]', "baseline_clean": "systemctl --user show-environment"}
        for name, text in cleanup.items():
            self.assertIn(text, part(name))
        self.assertIn('no_such_user "$SUBJ" && gone "$THOME" && bsudo useradd', driver)  # a pre-existing home is refused before useradd can adopt it
        self.assertIn("owned_account", part("priv"))  # userdel --remove only for the account this job created (uid and home as recorded)
        self.assertIn('"$SUBJ:$SUID:$THOME"', driver)
        self.assertTrue(re.search(r'bsudo useradd [^\n]* && ACCT_CREATED=yes ;;', part("priv")) and driver.count("ACCT_CREATED=yes") == 1 and "ACCT_CREATED=''" in driver)  # the only place the flag is set: after this job's own successful useradd
        self.assertTrue('[ "$ACCT_CREATED" = yes ] && row=$(getent passwd "$SUBJ") && [ "$(printf' in driver and "getent passwd \"$SUBJ\" |" not in driver)  # ownership needs the flag, a lookup whose own status is checked, and the recorded name, uid and home
        self.assertLess(part("cl_user").index("owned_account"), part("cl_user").index("HX stop"))  # nothing runs as an account this job did not create
        self.assertIn('sgone "$p"', part("baseline_clean"))
        self.assertIn('[ "$by" != unknown ]', part("case_end"))  # an unobserved bystander is not "unchanged"
        self.assertIn("'' | unknown) fail", part("bystander_up"))
        self.assertIn('gone "/usr/local/share/ca-certificates/herdr-nlt-$RUN-$ATT.crt"', part("cl_trust"))
        self.assertIn("[ -n \"$before\" ]", part("expect_refusal"))
        self.assertIn("[ -n \"$after\" ]", part("expect_refusal"))
        self.assertTrue("! -e" not in driver and driver.count("! -L") == 3)  # only the hosted guards and stage_put's write guard still use a file test
        self.assertEqual(part("cl_product").count("residue_gone"), 2)  # the early success and the post-uninstall check both need every owned path gone
        self.assertTrue(all("residue_gone" in part(name) for name in ("teardown_refusal_base", "case_full_uninstall")))
        self.assertIsNone(re.search(r"as_subject (rm|test) |sh -c 'rm|as_subject (launchctl|systemctl)", part("cl_product") + part("cl_foreign") + part("cl_fixtures")))  # cleanup runs absolute utilities, not a PATH lookup
        self.assertIn('$(dirname "$DEF"),$THOME/.local/share/systemd/user,$THOME/.cache/herdr-mobile-relay"', driver)  # the foreign fragment's directory and the cache are in the refusal digest
        restart = part("case_stop_start_restart")  # the restart proof needs a known digest after the manual start, bound into the gated case
        self.assertTrue('known_digest "$pid1"' in restart and 'known_digest "$pid2"' in restart and "MID=$pid1" in restart and '--mid "${MID:-n/a}"' in part("case_end"))
        self.assertTrue("after=$(snap) tally=" not in driver and "jget lines" in part("lifecycle_main"))  # one statement per observation; the final refusal count needs a readable, non-empty log
        self.assertIn("launchctl getenv PATH", part("baseline_clean"))
        for text in ("[ ! -L \"$1\" ] && umask 077 && cat", "umask 077 && mkdir \"$1\"", '"$SHIMDIR/herdr-mobile-relay"; do', ".[!.]*/bin; do", "command -v herdr", '"${1##*/}")" = "$1"'):
            self.assertIn(text, driver)  # never write through a symlink; an atomic fresh stage; no occupied product shim, no real herdr on the service PATH, the system manager

    @staticmethod
    def load_host():
        spec = importlib.util.spec_from_file_location("nlt_host", ROOT / "scripts/ci/native-lifecycle-host.py")
        host = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(host)  # defines functions only
        return host

    def test_bounded_runner_kills_descendants_that_ignore_term(self):
        with tempfile.TemporaryDirectory() as tmp:
            pidfile = os.path.join(tmp, "pid")  # the leader dies on TERM; its descendant ignores it and would survive a leader-only wait
            script = "sh -c 'trap \"\" TERM; echo $$ > %s; exec sleep 60' & sleep 60" % pidfile
            self.assertEqual(self.load_host().run_bounded({"timeout": "1"}, ["/bin/sh", "-c", script]), 124)
            pid = int(pathlib.Path(pidfile).read_text())
            for _ in range(30):
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    return
                time.sleep(0.1)
            self.fail("a timed-out command's descendant survived")

    def test_bounded_runner_never_reports_an_unsignalable_group_as_stopped(self):
        with unittest.mock.patch("os.killpg", side_effect=PermissionError), contextlib.redirect_stderr(io.StringIO()):  # a member of another uid
            self.assertEqual(self.load_host().run_bounded({"timeout": "1"}, ["/bin/sleep", "3"]), 125)

    def test_driver_probes_accept_only_a_definite_negative(self):
        lines = self.text("scripts/ci/run-native-lifecycle.sh").splitlines()
        prelude = "\n".join(l for l in lines if l.startswith(("none() {", "absent() {"))) + "\nfail() { exit 9; }\nHOSTN=b.test\n"  # the driver's own definitions
        guard = next(l for l in lines if "tail -c1 /etc/hosts" in l)
        with tempfile.TemporaryDirectory() as tmp:
            for name, text in (("h", "127.0.0.1 a.test\n"), ("u", "127.0.0.1 a.test")):
                pathlib.Path(tmp, name).write_text(text)
            run = lambda body, *args: subprocess.run(["/bin/bash", "-c", prelude + body, "_", *args], capture_output=True).returncode
            self.assertEqual([run('absent "$@"', *a) for a in (("b.test", tmp + "/h"), ("A.TEST", tmp + "/h"), ("a.test", tmp + "/x"))], [0, 1, 1])  # no match; match; unreadable (grep exit 2)
            self.assertEqual([run('none sh -c "exit $1"', str(code)) for code in (1, 0, 2, 127)], [0, 1, 1, 1])  # only exit 1 (pgrep/grep: no match) is absence
            self.assertEqual([run(guard.replace("/etc/hosts", tmp + "/" + name)) for name in "hu"], [0, 9])  # an unterminated /etc/hosts is refused before the append

    @staticmethod
    def ps_stub(table=(0, "", ""), direct=(1, "", "")):
        """The process table answer, and the answer of `ps -p PID -o pid=` (exit 1 and no output: no such process)."""
        return lambda argv, **kw: direct if "-p" in argv else table

    def test_process_table_failure_is_unknown_never_none(self):
        host = self.load_host()
        for table, want in (((127, "", ""), "unknown"), ((0, "", ""), "none")):  # ps failed or timed out; ps listed nothing and the direct query says no such process
            with unittest.mock.patch.object(host, "sh", side_effect=self.ps_stub(table)):
                self.assertEqual(host.tree_digest(4242), want)

    def test_group_gone_confirms_the_whole_group(self):
        host, proc = self.load_host(), subprocess.Popen(["/bin/sleep", "30"], start_new_session=True)
        self.assertFalse(host.group_gone(proc.pid, 2))  # a live member: not stopped
        os.killpg(proc.pid, 9)
        proc.wait()
        self.assertTrue(host.group_gone(proc.pid, 2))

    def shell_function(self, name):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        return name + "() {" + driver.split("\n" + name + "() {", 1)[1].split("\n}\n", 1)[0] + "\n}\n"

    def run_cleanup(self, call, env, extra=(), oneliners=()):
        """Run the driver's own cleanup callbacks in a bash whose every side effect (sudo, manager, user, process, file removal) is a logging stub.

        `extra` names further driver functions to define from the driver itself (they replace a stub of the same name)."""
        lines = self.text("scripts/ci/run-native-lifecycle.sh").splitlines()
        one = lambda name: next(l for l in lines if l.startswith(name + "() {"))
        stubs = textwrap.dedent("""
            priv() { echo "priv $*" >> "$LOG"; }
            HX() { echo "HX $*" >> "$LOG"; [ "$1" != exists ] || { if grep -q '^RUNB' "$LOG" 2> /dev/null; then printf '%s' "${EXISTS_AFTER-$EXISTS}"; else printf '%s' "$EXISTS"; fi; }; }
            fact() { echo "fact $*" >> "$LOG"; }
            as_subject() { echo "AS $*" >> "$LOG"; }
            RUNB() { echo "RUNB $*" >> "$LOG"; cat > /dev/null; }
            wait_for() { shift; "$@"; }
            snap() { local n=0; [ ! -f "$NCALL" ] || n=$(cat "$NCALL"); echo $((n + 1)) > "$NCALL"; if [ "$n" -eq 0 ]; then printf '%s\\n' "$SNAP1"; else printf '%s\\n' "$SNAP2"; fi; }
            getent() { local n=0; [ ! -f "$GCALL" ] || n=$(cat "$GCALL"); echo $((n + 1)) > "$GCALL"; [ -z "$GETENT_OUT" ] || printf '%s\\n' "$GETENT_OUT"; set -- $GETENT_SEQ; [ "$n" -lt "$#" ] || n=$(($# - 1)); shift "$n"; return "$1"; }
            bsudo() { echo "bsudo $*" >> "$LOG"; }
            fail() { echo "fail $*" >> "$LOG"; exit 1; }
        """)
        with tempfile.TemporaryDirectory() as tmp:
            log = os.path.join(tmp, "log")
            head = "set +e\nLOG=%s NCALL=%s/ncall GCALL=%s/gcall PYO=%s HOSTPY=%s\n" % (shlex.quote(log), tmp, tmp, shlex.quote(sys.executable), shlex.quote(str(ROOT / "scripts/ci/native-lifecycle-host.py")))
            script = (head + "\n".join(one(n) for n in ("none", "absent", "no_such_user", "owned_account", "jget", "OX", "gone", "sgone") + tuple(oneliners)) + stubs
                      + "".join(self.shell_function(n) for n in ("cl_user", "cl_product", "cl_foreign") + tuple(extra)) + call)
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True,
                                  env=dict({"PATH": "/usr/bin:/bin", "HOME": tmp, "GETENT_OUT": "", "GETENT_SEQ": "0"}, **env))
            return done.returncode, pathlib.Path(log).read_text() if os.path.exists(log) else "", done.stderr

    def test_cleanup_callbacks_never_read_an_unanswered_inspection_as_absence(self):
        entry = "nlt-1-1:x:61234:61234::/h:/bin/bash"  # the account this job created: uid and home as recorded
        user = {"OS": "Linux", "SUBJ": "nlt-1-1", "S": "/s", "LINGER_ON": "", "SUID": "61234", "THOME": "/h", "GETENT_OUT": entry, "ACCT_CREATED": "yes"}
        # (getent statuses in order: lookup, ownership, after userdel; $clean; expected status; userdel attempted): only exit 2 (key not found) is an absent account.
        for seq, clean, want, userdel in (("2", "pass", 0, False), ("1", "pass", 1, False), ("3", "pass", 1, False), ("127", "pass", 1, False), ("0 0", "fail", 1, False),
                                          ("0 0 2", "pass", 0, True), ("0 0 1", "pass", 1, True), ("0 0 0", "pass", 1, True)):
            code, log, err = self.run_cleanup("clean=%s; cl_user" % clean, dict(user, GETENT_SEQ=seq))
            self.assertEqual((code, "priv userdel" in log), (want, userdel), (seq, clean, err))
        for status in ("1", "2", "3", "127", "130"):  # a lookup that printed the matching entry and then failed (or was interrupted) is not ownership: nothing runs as it, nothing is removed
            code, log, err = self.run_cleanup("clean=pass; cl_user", dict(user, GETENT_SEQ="0 %s" % status))
            self.assertEqual((code, log), (1, ""), (status, err))
        for other in ("nlt-1-1:x:61235:61235::/h:/bin/bash", "nlt-1-1:x:61234:61234::/elsewhere:/bin/bash", ""):  # a pre-existing account: nothing runs as it, nothing is removed
            code, log, err = self.run_cleanup("clean=pass; cl_user", dict(user, GETENT_OUT=other, GETENT_SEQ="0 0 2"))
            self.assertEqual((code, log), (1, ""), (other, err))
        # SUID is the Actions runner's uid until this job's own useradd; an account that existed before it, with that uid and the chosen home, is not this job's: useradd is refused (the
        # account exists), the created flag stays unset, and cleanup neither runs as it (HX stop) nor removes it, although uid:home equals "$SUID:$THOME".
        runner = str(os.getuid())
        existing = dict(user, SUID=runner, GETENT_OUT="nlt-1-1:x:%s:%s::/h:/bin/bash" % (runner, runner), GETENT_SEQ="0 0 0", ACCT_CREATED="")
        code, log, err = self.run_cleanup("priv useradd; echo \"created=$ACCT_CREATED\" >&2; clean=pass; cl_user", existing, extra=("priv",))
        self.assertEqual((code, log, "created=\n" in err), (1, "", True), (log, err))
        for uid in (runner, "61234"):  # with the runner's uid, and with a uid that equals the passwd entry's: the unset flag alone refuses, the uid:home equality alone does not admit
            code, log, err = self.run_cleanup("clean=pass; cl_user", dict(existing, SUID=uid, GETENT_OUT="nlt-1-1:x:%s:%s::/h:/bin/bash" % (uid, uid)))
            self.assertEqual((code, log), (1, ""), (uid, err))
        # The account this job created (the real `priv useradd` sets the flag after a successful useradd, then the recorded uid:home is checked) is cleaned up and removed.
        fresh = dict(user, GETENT_SEQ="2 0 0 0 2", ACCT_CREATED="", THOME=tempfile.gettempdir() + "/nlt-q1r5-never-created")
        fresh["GETENT_OUT"] = "nlt-1-1:x:61234:61234::%s:/bin/bash" % fresh["THOME"]
        code, log, err = self.run_cleanup("priv useradd && clean=pass && cl_user", fresh, extra=("priv",))
        self.assertEqual((code, "bsudo useradd" in log, "bsudo userdel --remove nlt-1-1" in log), (0, True, True), (log, err))
        j = lambda **kw: json.dumps(kw)
        gone = j(active="absent", origin="absent", definition_hash="absent")
        product = {"PRODUCT": "yes", "SHIMDIR": "/shim", "STUB": "", "REL": "/rel", "DEF": "/def", "LDEF": "/ldef", "CONF": "/conf", "THOME": "/h"}
        # (answer for every owned path before the uninstall, answer after it, snapshot, expected status, uninstall attempted): success needs the manager absent AND every owned
        # path (release, config, cache, link, definition) definitely gone; an empty or `unknown` answer, or residue the uninstall left, is never "clean".
        for before, after, snapshot, want, uninstall in (("no", "no", gone, 0, False), ("", "no", gone, 0, True), ("", "", gone, 1, True), ("unknown", "unknown", gone, 1, True),
                                                         ("yes", "no", gone, 0, True), ("yes", "yes", gone, 1, True),  # the uninstall left config or cache behind
                                                         ("no", "no", j(active="unknown", origin="unknown", definition_hash="absent"), 1, False),
                                                         ("yes", "no", j(active="loaded", origin="/foreign", definition_hash="d" * 64), 1, False)):
            code, log, err = self.run_cleanup("cl_product", dict(product, EXISTS=before, EXISTS_AFTER=after, SNAP1=snapshot, SNAP2=snapshot), extra=("residue_gone",))
            self.assertEqual((code, "RUNB" in log), (want, uninstall), (before, after, snapshot, err))
        planted = dict(origin="/f", definition_hash="a" * 64, active="inactive", owned_pid_digest="none")
        foreign = lambda before=None, **now: self.run_cleanup("cl_foreign", {"OS": "Linux", "FOREIGN_DEF": "/f", "BEFORE": j(**(before or planted)), "SNAP1": j(**dict(planted, **now)), "SNAP2": gone})
        self.assertEqual(foreign()[0], 0)
        self.assertIn("rm -rf", foreign()[1])  # the planted, identical, not-running fixture is the only thing removed
        unknown_before = dict(planted, origin="unknown", definition_hash="unknown")
        for before, now in ((unknown_before, dict(origin="unknown", definition_hash="unknown")),  # two identical unknown observations are not an identity
                            (None, dict(origin="unknown")), (None, dict(definition_hash="unknown")), (None, dict(origin="/other")), (None, dict(definition_hash="b" * 64)),
                            (None, dict(active="active")), (None, dict(active="unknown")), (None, dict(owned_pid_digest="unknown")), (None, dict(owned_pid_digest="c" * 64))):
            code, log, err = foreign(before, **now)
            self.assertEqual(code, 1, (before, now, err))
            self.assertNotIn("rm -rf", log, (before, now))

    def test_baseline_path_check_includes_the_manager_inherited_path_and_per_tool_bins(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        script = driver.split("as_subject /bin/sh -c '\n        P=", 1)[1].split("' _ \"$MGRPATH\" \"$mpath\"", 1)[0]
        script = "P=" + script
        base = "/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        want = subprocess.run(["/bin/sh", "-c", "PATH=%s; command -v sh" % base], capture_output=True, text=True).stdout.strip()
        with tempfile.TemporaryDirectory() as tmp:
            home = tmp + "/home"
            for d in ("/home/.local/bin", "/home/.agent/bin", "/inherited"):
                os.makedirs(tmp + d)
            run = lambda inherited: subprocess.run(["/bin/sh", "-c", script, "_", want, inherited], env={"HOME": home, "PATH": "/usr/bin:/bin"}, capture_output=True).returncode
            if shutil.which("herdr", path=base) is None:  # on a machine that has a herdr in a system directory the clean case is not assertable
                self.assertEqual(run(""), 0)
                self.assertEqual(run(tmp + "/inherited"), 0)
            for herdr in (tmp + "/inherited/herdr", home + "/.agent/bin/herdr"):
                pathlib.Path(herdr).write_text("#!/bin/sh\n")
                os.chmod(herdr, 0o755)
                self.assertNotEqual(run(tmp + "/inherited"), 0, herdr)  # a herdr on the manager's PATH or in a per-tool bin directory refuses the baseline

    def test_tree_digest_never_reads_an_unparsed_owned_row_as_no_process(self):
        host, row = self.load_host(), lambda pid, ppid, tail="Mon Oct 21 04:46:00 2025 /bin/sleep 5": " %d %d %s\n" % (pid, ppid, tail)
        for pid, listing, want in ((None, "", "unknown"), (0, "", "none"),
                                   (4242, row(7, 1), "none"),  # a fully parsed table without the pid, and the direct query says no such process
                                   (4242, row(4242, 1) + "continuation of a multi-line command\n", "digest"),
                                   (4242, row(4242, 1, "garbled"), "unknown"),  # the manager's own row cannot be parsed
                                   (4242, row(4242, 1) + row(4243, 4242, "garbled"), "unknown"),  # a descendant's row cannot be parsed
                                   (4242, row(4242, 1) + row(4243, 4242), "digest")):
            with unittest.mock.patch.object(host, "sh", side_effect=self.ps_stub((0, listing, ""))):
                got = host.tree_digest(pid)
            self.assertTrue(got == want or (want == "digest" and re.fullmatch(r"[0-9a-f]{64}", got)), (pid, listing, got))
        # A garbled or truncated head of the manager's own row is not in the parsed table: only the direct per-pid query decides.
        for listing, direct, want in ((" 4242\n", (0, "  4242\n", ""), "unknown"),  # the process exists, the table row is unusable
                                      (" 4242 garbled\n", (0, "4242\n", ""), "unknown"),
                                      (row(7, 1), (0, "4242\n", ""), "unknown"),  # absent from a parsed table but the direct query finds it
                                      (row(7, 1), (127, "", ""), "unknown"), (row(7, 1), (2, "", "boom"), "unknown"), (row(7, 1), (1, "4242\n", ""), "unknown"),
                                      (" 4242\n", (1, "", ""), "none")):  # the direct query definitively says it is gone
            with unittest.mock.patch.object(host, "sh", side_effect=self.ps_stub((0, listing, ""), direct)):
                self.assertEqual(host.tree_digest(4242), want, (listing, direct))

    @staticmethod
    def unit(**kw):
        fields = dict(dict(LoadState="loaded", ActiveState="inactive", UnitFileState="enabled", FragmentPath="/f", DropInPaths="", MainPID="0"), **kw)
        return 0, "".join("%s=%s\n" % pair for pair in fields.items()), ""

    def observed(self, manager, definition="/nonexistent/def", release="/nonexistent", options=None, raw=False, **replies):
        host = self.load_host()
        generation = (0, "MainPID=5\nInvocationID=%s\n" % ("ab" * 16), "")

        def fake(argv, timeout=10, limit=65536):
            if argv[0] == "ps":
                return replies.get("ps", (0, "", "") if "-p" not in argv else (1, "", ""))
            if manager == "launchd":
                return replies.get(argv[1], (0, "123\n" if argv[1] == "managerpid" else "", ""))
            return replies["unit"] if "--user" in argv else replies.get("userman", generation)
        opts = dict({"manager": manager, "label": "x.service", "definition": definition, "release": release}, **(options or {}))
        with unittest.mock.patch.object(host, "sh", side_effect=fake), contextlib.redirect_stdout(io.StringIO()) as out:
            host.observe(opts, [])
        return out.getvalue().strip() if raw else json.loads(out.getvalue())

    def test_observe_reports_an_ambiguous_manager_answer_as_an_unknown_process_digest(self):
        unit = self.unit
        ps = (0, " 123     1 Mon Oct 21 04:46:00 2025 /bin/sleep 5\n", "")
        hexdigest = lambda s: re.fullmatch(r"[0-9a-f]{64}", s["owned_pid_digest"]) is not None
        self.assertEqual(self.observed("systemd", unit=unit())["owned_pid_digest"], "none")  # inactive with no main pid: definitely no process
        # not-found is absence only as the product's own observer reads it (inactive, no main pid, no unit-file state); a contradictory reply is unknown, never "no unit, no process"
        absent = self.observed("systemd", unit=unit(LoadState="not-found", UnitFileState=""))
        self.assertEqual((absent["active"], absent["origin"], absent["enabled"], absent["owned_pid_digest"]), ("absent", "absent", "false", "none"))
        for odd in (dict(ActiveState="active", MainPID="123"), dict(ActiveState="active"), dict(ActiveState="activating"), dict(ActiveState="failed"), dict(MainPID="123"), dict(MainPID="n/a"), dict(UnitFileState="enabled")):
            seen = self.observed("systemd", unit=unit(LoadState="not-found", **dict(dict(UnitFileState=""), **odd)), ps=ps)
            self.assertEqual((seen["active"], seen["origin"], seen["enabled"], seen["owned_pid_digest"]), ("unknown",) * 4, odd)
        for reply in (unit(ActiveState="active"), unit(ActiveState="activating"), unit(MainPID="n/a"), (127, "", "")):  # running without a pid, no pid, no answer
            self.assertEqual(self.observed("systemd", unit=reply)["owned_pid_digest"], "unknown", reply)
        self.assertTrue(hexdigest(self.observed("systemd", unit=unit(ActiveState="active", MainPID="123"), ps=ps)))
        uid = os.getuid()
        missing = (113, "", 'Could not find service "x.service" in domain for user gui: %d' % uid)
        head = "gui/%d/x.service = {\n" % uid  # the product's own framing of a loaded answer
        framed = lambda body: (0, head + body + "}\n", "")
        quiet = self.observed("launchd", print=framed("\tpath = /p\n\tstate = not running\n"))
        self.assertEqual((quiet["active"], quiet["origin"], quiet["owned_pid_digest"]), ("loaded", "/p", "none"))
        self.assertEqual(self.observed("launchd", print=framed("\tstate = waiting\n"))["owned_pid_digest"], "none")
        self.assertEqual(self.observed("launchd", print=missing)["owned_pid_digest"], "none")
        for reply in (framed("\tpath = /p\n\tpid = 5\n\tpid = 6\n"), framed("\tpath = /p\n\tpid = x\n"), framed("\tpath = /p\n\tstate = running\n"), (1, "", "boom"),
                      (0, "", ""), (0, "\n", ""), (0, "garbage\n", ""), (0, "\tpath = /p\n\tstate = not running\n", ""),  # a status-0 answer that is empty, unframed or has no frame at all is no observation
                      (0, "gui/%d/other.service = {\n\tstate = not running\n}\n" % uid, ""), (0, "gui/%d/x.service = {\n\tstate = not running\n" % uid + "\x00}\n", ""),
                      framed("\tpath = /p\n"), (0, head.rstrip("\n").rstrip("{") + "{}", ""),  # framed but no single state line: not "no process" (the bare `{}` is the product's empty body)
                      framed("\tstate = not running\n\tstate = waiting\n"),
                      framed("\tstate = \n"), framed("\tstate =\n"), framed("\tstate = bogus\n"), framed("\tstate = spawn scheduled\n"), framed("\tstate = not running \n"),  # blank, unrecognized or padded state
                      framed("\tstate = Not Running\n"), framed("\tstate = running\n"), framed("\tstate = \x00\n")):
            self.assertEqual(self.observed("launchd", print=reply)["owned_pid_digest"], "unknown", reply)
        for reply in ((0, "", ""), (0, "garbage\n", ""), (0, "\tpath = /p\n\tstate = not running\n", "")):  # and the loaded state and the origin it carries are unknown as well
            seen = self.observed("launchd", print=reply)
            self.assertEqual((seen["active"], seen["origin"]), ("unknown", "unknown"), reply)
        self.assertTrue(hexdigest(self.observed("launchd", print=framed("\tpath = /p\n\tstate = running\n\tpid = 123\n"), ps=ps)))
        # A repeated systemd property is a contradiction, never "the last line wins" (the not-found tuple could otherwise be assembled from two replies).
        coherent = "LoadState=not-found\nActiveState=inactive\nUnitFileState=\nFragmentPath=\nDropInPaths=\nMainPID=0\n"
        self.assertEqual(self.observed("systemd", unit=(0, coherent, ""))["active"], "absent")
        for extra in ("ActiveState=active\n", "MainPID=123\n", "LoadState=loaded\n", "UnitFileState=enabled\n"):
            for text in (extra + coherent, coherent + extra):
                seen = self.observed("systemd", unit=(0, text, ""), ps=ps)
                self.assertEqual((seen["active"], seen["origin"], seen["enabled"], seen["owned_pid_digest"]), ("unknown",) * 4, text)
        loaded = self.unit(ActiveState="active", MainPID="123")[1]
        self.assertEqual(self.observed("systemd", unit=(0, loaded + "MainPID=0\n", ""), ps=ps)["owned_pid_digest"], "unknown")
        self.assertEqual(self.observed("systemd", unit=self.unit(), userman=(0, "MainPID=5\nMainPID=0\nInvocationID=%s\n" % ("ab" * 16), ""))["manager_generation"], "unknown")

    def test_observe_reports_an_uninspectable_file_as_unknown_never_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            definition = tmp + "/def"
            pathlib.Path(definition).write_text("x")
            os.chmod(definition, 0o600)
            os.makedirs(tmp + "/dir")
            os.symlink(tmp + "/nowhere", tmp + "/dangling")
            seen = lambda path: (lambda s: (s["definition_hash"], s["mode"]))(self.observed("systemd", definition=path, unit=self.unit()))
            self.assertEqual(seen(tmp + "/missing"), ("absent", "absent"))
            self.assertEqual(seen(tmp + "/def/under-a-file"), ("absent", "absent"))  # ENOTDIR
            self.assertEqual(seen(definition), (hashlib_sha256(b"x"), "0600"))
            self.assertEqual(seen(tmp + "/dir"), ("unknown", "unknown"))  # a directory is not a definition file
            self.assertEqual(seen(tmp + "/dangling"), ("unknown", "unknown"))
            if os.geteuid() != 0:
                os.makedirs(tmp + "/locked")
                pathlib.Path(tmp, "locked/def").write_text("x")
                os.chmod(tmp + "/locked", 0)
                try:
                    self.assertEqual(seen(tmp + "/locked/def"), ("unknown", "unknown"))  # permission denied is not "absent"
                finally:
                    os.chmod(tmp + "/locked", 0o700)
            manifest = tmp + "/rel/current/release-manifest.json"
            revision = lambda: self.observed("systemd", release=tmp + "/rel", unit=self.unit())["release_revision"]
            self.assertEqual(revision(), "absent")  # no release at all
            os.makedirs(tmp + "/rel/current")
            self.assertEqual(revision(), "absent")  # a release without a manifest file is still definitely absent
            for body, want in ((json.dumps({"revision": "c" * 40}), "c" * 40), ("{", "unknown"), (json.dumps({"revision": 5}), "unknown"), (json.dumps([1]), "unknown"), (json.dumps({"revision": "x"}), "unknown")):
                pathlib.Path(manifest).write_text(body)
                self.assertEqual(revision(), want, body)
            if os.geteuid() != 0:
                os.chmod(manifest, 0)
                try:
                    self.assertEqual(revision(), "unknown")  # an unreadable manifest is not "no release"
                finally:
                    os.chmod(manifest, 0o600)

    def test_observe_manager_generation_and_unobserved_digests(self):
        unit, key = self.unit, "ab" * 16
        gen = lambda **replies: self.observed("systemd", unit=unit(), **replies)["manager_generation"]
        self.assertEqual(gen(), "5:" + key)
        self.assertEqual(gen(userman=(0, "MainPID=0\nInvocationID=\n", "")), "none")  # the manager is definitely not running
        for reply in ((1, "", "boom"), (0, "MainPID=abc\n", ""), (0, "MainPID=5\n", ""), (0, "MainPID=5\nInvocationID=short\n", ""), (0, "", "")):
            self.assertEqual(gen(userman=reply), "unknown", reply)
        self.assertEqual(self.observed("launchd", print=(0, "gui/%d/x.service = {\n\tpath = /p\n}\n" % os.getuid(), ""), managerpid=(0, "123\n", ""))["manager_generation"], "123")
        # A digest over a snapshot with an unobserved field is `unknown` (the gate refuses it as a malformed bystander digest); the manager generation is not part of that evidence.
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "f").write_text("x")
            os.makedirs(tmp + "/d")
            hexed = lambda value: re.fullmatch(r"[0-9a-f]{64}", value) is not None
            digest = lambda kind, file, **replies: self.observed("systemd", options={"digest": kind, "file": file}, raw=True, **replies)
            self.assertTrue(hexed(digest("stable", tmp + "/f", unit=unit())))
            self.assertTrue(hexed(digest("stable", tmp + "/f", unit=unit(), userman=(1, "", ""))))  # an unknown manager generation alone does not poison it
            self.assertEqual(digest("stable", tmp + "/f", unit=(127, "", "")), "unknown")  # the manager query failed: active, origin and enabled are unknown
            self.assertEqual(digest("stable", tmp + "/d", unit=unit()), "unknown")  # the bystander file cannot be read
            self.assertTrue(hexed(digest("stable", tmp + "/f", unit=unit(ActiveState="active"))))  # stable ignores the process digest...
            self.assertEqual(digest("full", tmp + "/f", unit=unit(ActiveState="active")), "unknown")  # ...full does not: active with no main pid is unobserved
            self.assertTrue(hexed(digest("full", tmp + "/f", unit=unit())))

    def test_privileged_account_operations_require_a_fresh_home_and_the_account_this_job_created(self):
        base = {"OS": "Linux", "SUBJ": "nlt-1-1", "SUID": "61234"}
        entry = "nlt-1-1:x:%s:%s::%s:/bin/bash"
        with tempfile.TemporaryDirectory() as tmp:
            home = tmp + "/home"
            os.makedirs(tmp + "/existing")
            # userdel --remove deletes the home recursively: only the exact uid and home this job recorded may be removed.
            for out, seq, want in ((entry % ("61234", "61234", home), "0", True), (entry % ("61234", "61234", home + "x"), "0", False), (entry % ("61235", "61235", home), "0", False),
                                   ("", "2", False), ("", "1", False)):
                code, log, err = self.run_cleanup("priv userdel", dict(base, THOME=home, GETENT_OUT=out, GETENT_SEQ=seq, ACCT_CREATED="yes"), extra=("priv",))
                self.assertEqual(("bsudo userdel --remove nlt-1-1" in log, code == 0), (want, want), (out, seq, log, err))
            for status in ("1", "2", "3", "127", "130"):  # the exact entry printed, then a failed lookup: userdel --remove is refused
                code, log, err = self.run_cleanup("priv userdel", dict(base, THOME=home, GETENT_OUT=entry % ("61234", "61234", home), GETENT_SEQ=status, ACCT_CREATED="yes"), extra=("priv",))
                self.assertEqual(("bsudo userdel" in log, code), (False, 1), (status, log, err))
            code, log, err = self.run_cleanup("priv userdel", dict(base, THOME=home, GETENT_OUT="other:x:61234:61234::%s:/bin/bash" % home, GETENT_SEQ="0", ACCT_CREATED="yes"), extra=("priv",))
            self.assertEqual(("bsudo userdel" in log, code), (False, 1), (log, err))  # uid and home as recorded, but another name
            code, log, err = self.run_cleanup("priv userdel", dict(base, THOME=home, GETENT_OUT=entry % ("61234", "61234", home), GETENT_SEQ="0", ACCT_CREATED=""), extra=("priv",))
            self.assertEqual(("bsudo userdel" in log, code), (False, 1), (log, err))  # the exact uid and home, but this job's useradd never succeeded: refused
            # useradd: a definite "no such user" AND a definitely absent home; an existing home (which useradd would adopt) or an unanswered lookup refuses.
            cases = [(tmp + "/fresh", "2", True), (tmp + "/existing", "2", False), (tmp + "/fresh", "0", False), (tmp + "/fresh", "1", False)]
            if os.geteuid() != 0:
                os.makedirs(tmp + "/locked/inner")
                os.chmod(tmp + "/locked", 0)
                cases.append((tmp + "/locked/home", "2", False))  # the home path cannot be inspected
            try:
                for thome, seq, want in cases:
                    code, log, err = self.run_cleanup('priv useradd; rc=$?; echo "created=$ACCT_CREATED" >&2; exit $rc', dict(base, THOME=thome, GETENT_SEQ=seq), extra=("priv",))
                    self.assertEqual(("bsudo useradd" in log, code == 0, "created=yes" in err), (want, want, want), (thome, seq, log, err))  # the flag is set only by a successful useradd
                code, log, err = self.run_cleanup('bsudo() { echo "bsudo $*" >> "$LOG"; return 1; }; priv useradd; rc=$?; echo "created=$ACCT_CREATED" >&2; exit $rc',
                                                  dict(base, THOME=tmp + "/fresh", GETENT_SEQ="2"), extra=("priv",))
                self.assertEqual(("bsudo useradd" in log, code, "created=\n" in err), (True, 1, True), (log, err))  # a failed useradd (even one that may have created the account) is not ownership
            finally:
                if os.path.isdir(tmp + "/locked"):
                    os.chmod(tmp + "/locked", 0o700)

    def test_bystander_is_running_only_with_a_real_process_digest_and_paths_are_gone_only_when_definitely_absent(self):
        for value, want in (("f" * 64, 0), ("none", 1), ("unknown", 1), ("", 1)):
            code, log, err = self.run_cleanup('snap_of() { printf \'{"owned_pid_digest": "%s"}\\n\' "$D"; }; bystander_running', {"D": value}, oneliners=("bystander_running",))
            self.assertEqual(code, want, (value, err))
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "f").write_text("x")
            answers = [(tmp + "/f", 1), (tmp + "/missing", 0), (tmp + "/f/child", 0)]
            if os.geteuid() != 0:
                os.makedirs(tmp + "/locked/inner")
                os.chmod(tmp + "/locked", 0)
                answers.append((tmp + "/locked/inner", 1))  # permission denied is not "gone"
            try:
                for path, want in answers:
                    self.assertEqual(self.run_cleanup('gone "$P"', {"P": path})[0], want, path)
            finally:
                if os.path.isdir(tmp + "/locked"):
                    os.chmod(tmp + "/locked", 0o700)
        for answer, want in (("no", 0), ("yes", 1), ("unknown", 1), ("", 1)):
            self.assertEqual(self.run_cleanup('sgone /p', {"EXISTS": answer})[0], want, answer)

    def test_refusal_digest_fails_on_anything_it_cannot_inspect(self):
        host = self.load_host()

        def digest(paths):
            with contextlib.redirect_stdout(io.StringIO()) as out:
                host.tree({"paths": paths}, [])
            return out.getvalue().strip()
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(tmp + "/a/sub")
            pathlib.Path(tmp, "a/sub/f").write_text("1")
            first = digest(tmp + "/a")
            self.assertEqual(digest(tmp + "/a"), first)
            pathlib.Path(tmp, "a/sub/f").write_text("2")
            self.assertNotEqual(digest(tmp + "/a"), first)
            self.assertEqual(digest(tmp + "/missing"), digest(tmp + "/f/under-a-missing-file"))  # a definitely absent root contributes nothing
            if os.geteuid() == 0:
                return
            os.makedirs(tmp + "/locked/inner")
            for locked, probe in ((tmp + "/a/sub", tmp + "/a"), (tmp + "/locked", tmp + "/locked/inner")):  # an unscannable subtree; a root behind an unsearchable parent
                os.chmod(locked, 0)
                try:
                    with self.assertRaises(OSError):
                        digest(probe)
                finally:
                    os.chmod(locked, 0o700)

    def test_exists_answers_no_only_for_a_definite_absence(self):
        host = self.load_host()

        def ask(path):
            with contextlib.redirect_stdout(io.StringIO()) as out:
                host.exists({"path": path}, [])
            return out.getvalue().strip()
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "f").write_text("x")
            os.symlink(tmp + "/nowhere", tmp + "/dangling")
            self.assertEqual([ask(tmp + "/f"), ask(tmp + "/missing"), ask(tmp + "/f/child"), ask(tmp + "/dangling")], ["yes", "no", "no", "yes"])
            if os.geteuid() != 0:  # an uninspectable path (permission denied) is unknown, never "no"
                os.makedirs(tmp + "/locked/inner")
                os.chmod(tmp + "/locked", 0)
                try:
                    self.assertEqual(ask(tmp + "/locked/inner"), "unknown")
                finally:
                    os.chmod(tmp + "/locked", 0o700)

    def test_sh_never_returns_a_truncated_answer_and_a_truncated_process_table_is_unknown(self):
        host = self.load_host()
        self.assertEqual(host.sh(["/bin/echo", "abcdef"], limit=3), (126, "", ""))  # longer than the limit: status 126 and nothing is returned
        self.assertEqual(host.sh(["/bin/echo", "abc"], limit=100), (0, "abc\n", ""))
        self.assertEqual(host.sh(["/nonexistent/x"])[0], 127)
        # the manager's row is there, the listing exceeds 16 MiB: the real `sh` stops it at the bound, so the digest is unknown
        listing = "import sys; sys.stdout.write(' 4242 1 Mon Oct 21 04:46:00 2025 /bin/sleep 5\\n' + 'x' * (1 << 24))"
        real = host.sh
        with unittest.mock.patch.object(host, "sh", side_effect=lambda argv, **kw: real([sys.executable, "-c", listing], **kw)):
            self.assertEqual(host.tree_digest(4242), "unknown")

    def test_sh_reads_raw_bytes_at_most_the_limit_and_kills_what_exceeds_it(self):
        host = self.load_host()
        self.assertEqual(host.sh(["/usr/bin/printf", "a\\r\\nb\\r\\n"]), (0, "a\r\nb\r\n", ""))  # raw bytes: a CR is never translated to LF (the product's parser sees it)
        self.assertEqual(host.sh(["/bin/sh", "-c", "printf '\\303\\251'; echo oops >&2; exit 3"]), (3, "\u00e9", "oops\n"))
        self.assertEqual(host.sh([sys.executable, "-c", "import sys; sys.stdout.buffer.write('\u00e9'.encode() * 3)"], limit=5), (126, "", ""))  # 6 BYTES, 3 characters: over a 5-byte bound
        self.assertEqual(host.sh([sys.executable, "-c", "import sys; sys.stdout.buffer.write('\u00e9'.encode() * 2)"], limit=5)[0], 0)
        with tempfile.TemporaryDirectory() as tmp:
            for stream in ("stdout", "stderr"):  # an endless writer on either stream is stopped, killed and reaped; at most limit + 1 bytes of it are ever read
                pidfile = tmp + "/pid-" + stream
                flood = "import os, sys\nopen(%r, 'w').write(str(os.getpid()))\nwhile True: sys.%s.buffer.write(b'x' * 65536); sys.%s.buffer.flush()" % (pidfile, stream, stream)
                total, real_read = [0], os.read

                def counting(fd, size, real_read=real_read, total=total):
                    data = real_read(fd, size)
                    total[0] += len(data)
                    return data
                started = time.monotonic()
                with unittest.mock.patch.object(host.os, "read", side_effect=counting):
                    self.assertEqual(host.sh([sys.executable, "-c", flood], timeout=20, limit=1000), (126, "", ""), stream)
                self.assertLess(time.monotonic() - started, 10, stream)
                self.assertLessEqual(total[0], 1001, (stream, total[0]))
                with self.assertRaises(ProcessLookupError):
                    os.kill(int(pathlib.Path(pidfile).read_text()), 0)  # reaped: the program is gone
        started = time.monotonic()
        self.assertEqual(host.sh([sys.executable, "-c", "import time; time.sleep(30)"], timeout=1), (127, "", ""))  # a timeout is its own status, and the program is killed
        self.assertLess(time.monotonic() - started, 10)

    @staticmethod
    def product_enabled(code, text, label):
        """What the product's own observer (relay/native-install-transaction.sh, sourced: functions only) reads from the same table, with `launchctl` a function stub: its enabled flag, `unknown` when it fails."""
        script = textwrap.dedent("""\
            source relay/native-install-transaction.sh
            native_manager=launchd
            launchctl() { case "$1" in print) case "$2" in gui/*/*) printf '%s = {\\n}' "$2" ;; *) printf '%s = {}' "$2" ;; esac ;; print-disabled) cat "$TABLE_FILE"; return "$TABLE_RC" ;; esac; }
            rc=0; native_install_observe "$LABEL" || rc=$?; echo "$rc:$NATIVE_ENABLED"
        """)
        with tempfile.TemporaryDirectory() as tmp:  # the table goes through a file: an environment variable cannot carry a NUL byte
            pathlib.Path(tmp, "table").write_bytes(text.encode())
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True, cwd=ROOT,
                                  env={"PATH": "/usr/bin:/bin", "TABLE_FILE": tmp + "/table", "TABLE_RC": str(code), "LABEL": label})
        rc, _, flag = done.stdout.strip().partition(":")
        return flag if rc == "0" else "unknown"

    def test_launchd_enabled_needs_a_complete_well_formed_disabled_table(self):
        host = self.load_host()
        table = lambda *entries: "disabled services = {\n" + "".join("\t%s\n" % e for e in entries) + "}\n"
        # The product's grammar (relay/native-install-transaction.sh): a line naming the label is exactly `"label" => true` or `"label" => false`, anything else fails there. The aliases
        # `enabled`/`disabled` were accepted here before Q1R5 (the previous expectation for `"com.x" => disabled` was "false"); now they are unknown, as is any other value or trailing text.
        for code, text, want in ((0, table(), "true"), (0, table('"com.x" => false'), "true"), (0, table('"com.x" => true'), "false"),  # complete: an unlisted label is enabled
                                 (0, table('"com.x" => disabled', '"com.y" => true'), "unknown"), (0, table('"com.x" => enabled'), "unknown"), (0, table('"com.x" => disabled'), "unknown"),
                                 (0, table('"com.y" => enabled', '"com.x" => true'), "false"), (0, table('"com.y" => disabled', '"com.x" => false'), "true"),  # other labels' lines are not judged
                                 (0, table('"com.x" => 1'), "unknown"), (0, table('"com.x" => '), "unknown"), (0, table('"com.x" => true '), "unknown"), (0, table('"com.x" => false;'), "unknown"),
                                 (0, table('"com.x" => TRUE'), "unknown"), (0, table('"com.x"=>true'), "unknown"), (0, table('"com.x" => true\r'), "unknown"), (0, table('"com.x" => true\x0b'), "unknown"), (0, table('"com.x" => true', '"com.x" => enabled'), "unknown"), (0, table('"com.xy" => true'), "true"),
                                 # The raw framing, byte for byte as the product reads it (only trailing newlines are dropped; nothing else is normalized):
                                 (0, " " + table(), "unknown"), (0, "\n" + table(), "unknown"), (0, "\t" + table(), "unknown"), (0, "x" + table(), "unknown"),  # blanks or text before the header
                                 (0, table().rstrip("\n") + " ", "unknown"), (0, table() + " ", "unknown"), (0, table() + "\t\n", "unknown"),  # blanks after the closing brace
                                 (0, table().replace("\n", "\r\n"), "unknown"), (0, table('"com.x" => true').replace("}\n", "\x00}\n"), "unknown"),  # CRLF, an embedded NUL
                                 (0, table('"com.x" => true') + "\n\n\n", "false"), (0, "disabled services = {}\n", "true"), (0, "disabled services = {junk\n}\n", "true"),  # what the product accepts
                                 (0, table('"com.x" => true') + "}", "false"), (0, "disabled services = {\n\t\"com.x\" => true\n}\nextra\n}", "false"),
                                 (0, "", "unknown"), (0, 'disabled services = {\n\t"com.y" => true\n', "unknown"),  # truncated: no closing brace
                                 (0, "something else\n}\n", "unknown"), (0, table('"com.x" => maybe'), "unknown"), (0, table('"com.x" => true', '"com.x" => false'), "unknown"),
                                 (126, table('"com.x" => true'), "unknown"), (1, table(), "unknown")):
            self.assertEqual(host.disabled_flag(code, text, "com.x"), want, (code, text))
            self.assertEqual(self.product_enabled(code, text, "com.x"), want, (code, text))  # the product's own parser reads every one of these tables the same way
        path = (0, "gui/%d/x.service = {\n\tpath = /p\n}\n" % os.getuid(), "")
        self.assertEqual(self.observed("launchd", print=path, **{"print-disabled": (126, "", "")})["enabled"], "unknown")  # output over the limit
        self.assertEqual(self.observed("launchd", print=path, **{"print-disabled": (0, table(), "")})["enabled"], "true")

    def observed_real(self, answer, table):
        """host.observe over the REAL `sh` (raw bytes, byte caps) against a stub `launchctl` script that serves exactly these bytes for `print` and `print-disabled`."""
        host = self.load_host()
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "print").write_bytes(answer)
            pathlib.Path(tmp, "disabled").write_bytes(table)
            script = pathlib.Path(tmp, "launchctl")
            script.write_text('#!/bin/sh\ncase "$1" in print) cat "%s/print" ;; print-disabled) cat "%s/disabled" ;; managerpid) echo 123 ;; esac\n' % (tmp, tmp))
            script.chmod(0o700)
            opts = {"manager": "launchd", "label": "x.service", "definition": "/nonexistent/def", "release": "/nonexistent"}
            with unittest.mock.patch.object(host, "LAUNCHCTL", str(script)), contextlib.redirect_stdout(io.StringIO()) as out:
                host.observe(opts, [])
        return json.loads(out.getvalue())

    @staticmethod
    def product_state(manager, reply, code=0, label="x.service"):
        """What the product's own observer (relay/native-install-transaction.sh, sourced: functions only; `systemctl` and `launchctl` are shell stubs) reads from one manager reply: its
        state, or `unknown` when it fails. systemd: the three-property reply of `systemctl --user show`; launchd: the answer of `launchctl print gui/<uid>/<label>` with that status."""
        script = textwrap.dedent("""\
            source relay/native-install-transaction.sh
            native_manager=$MGR
            systemctl() { cat "$REPLY_FILE"; }
            launchctl() { case "$1" in print) case "$2" in gui/*/*) cat "$REPLY_FILE"; return "$REPLY_RC" ;; *) printf '%s = {}' "$2" ;; esac ;; print-disabled) printf 'disabled services = {\\n}\\n' ;; esac; }
            rc=0; native_install_observe "$LABEL" || rc=$?; echo "$rc:$NATIVE_STATE"
        """)
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "reply").write_bytes(reply.encode())
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True, cwd=ROOT,
                                  env={"PATH": "/usr/bin:/bin", "MGR": manager, "REPLY_FILE": tmp + "/reply", "REPLY_RC": str(code), "LABEL": label})
        rc, _, state = done.stdout.strip().partition(":")
        return state if rc == "0" else "unknown"

    def test_manager_replies_are_split_on_lf_only_as_the_product_reads_them(self):
        ps = (0, " 123     1 Mon Oct 21 04:46:00 2025 /bin/sleep 5\n", "")
        # systemd: the helper's six-property reply and the product's three-property one carry the same line terminators. Only LF delimits a record; a CR, FF, VT, NEL or LS is part of the
        # value (or of one long record), so the reply is not the product's and is not read: it was `absent` for CRLF before (`str.splitlines()` dropped the CR).
        for load, active, unit, main, eol, want in (("not-found", "inactive", "", "0", "\n", "absent"), ("loaded", "active", "enabled", "123", "\n", "active"),
                                                    ("loaded", "inactive", "disabled", "0", "\n", "inactive"),
                                                    ("not-found", "inactive", "", "0", "\r\n", "unknown"), ("loaded", "active", "enabled", "123", "\r\n", "unknown"),
                                                    ("loaded", "inactive", "disabled", "0", "\r\n", "unknown"), ("not-found", "inactive", "", "0", "\r", "unknown"),
                                                    ("not-found", "inactive", "", "0", "\x0c", "unknown"), ("not-found", "inactive", "", "0", "\x0b", "unknown"),
                                                    ("not-found", "inactive", "", "0", "\x85", "unknown"), ("not-found", "inactive", "", "0", "\u2028", "unknown"),
                                                    ("not-found", "inactive", "", "0", "\x1c", "unknown"), ("not-found", "inactive\r", "", "0", "\n", "unknown"),
                                                    ("not-found\r", "inactive", "", "0", "\n", "unknown"), ("not-found", "inactive", "\x0c", "0", "\n", "unknown")):
            lines = ["LoadState=" + load, "ActiveState=" + active, "UnitFileState=" + unit, "FragmentPath=/f", "DropInPaths=", "MainPID=" + main]
            six, three = eol.join(lines) + eol, eol.join(lines[:3]) + eol
            seen = self.observed("systemd", unit=(0, six, ""), ps=ps)
            self.assertEqual((seen["active"], self.product_state("systemd", three)), (want, want), (lines, eol))
            if want == "unknown":  # nothing else is read from such a reply either
                self.assertEqual((seen["enabled"], seen["origin"], seen["owned_pid_digest"]), ("unknown",) * 3, (lines, eol))
        # launchd: the not-found diagnostic is the product's exact text after trailing newlines only (any other blank, CR or leading newline is not it)
        uid = os.getuid()
        missing = 'Could not find service "x.service" in domain for user gui: %d' % uid
        for text, code, want in ((missing, 113, "absent"), (missing + "\n", 113, "absent"), (missing + "\n\n", 1, "absent"), (" " + missing, 113, "unknown"), (missing + " ", 113, "unknown"),
                                 (missing + "\r", 113, "unknown"), (missing + "\r\n", 113, "unknown"), ("\n" + missing, 113, "unknown"), ("\t" + missing + "\n", 113, "unknown"),
                                 (missing, 0, "unknown"), ("", 113, "unknown"), (missing.replace("x.service", "y.service"), 113, "unknown")):
            self.assertEqual((self.observed("launchd", print=(code, text, ""))["active"], self.product_state("launchd", text, code)), (want, want), (text, code))
            self.assertEqual(self.observed("launchd", print=(code, "", text))["active"], want, (text, code))  # the same text on stderr (the product merges the two streams)
        # launchd: a state or pid line is evidence only byte for byte; a CR, FF, VT, NEL or LS inside the answer makes the process digest unknown, never `none`
        head = "gui/%d/x.service = {\n" % uid
        for body, want in (("\tstate = not running\n", "none"), ("\tstate = not running\r\n", "unknown"), ("\tstate = not running\x0c\n", "unknown"), ("\tstate = waiting\r\n", "unknown"),
                           ("\tstate = not running\x0b\tpid = 123\n", "unknown"), ("\tstate = not running\x85\n", "unknown"), ("\tstate = not running\u2028\n", "unknown"),
                           ("\tstate = running\r\n\tpid = 123\r\n", "unknown"), ("\tstate = not running\n\tpid = 123\r\n", "unknown")):
            self.assertEqual(self.observed("launchd", print=(0, head + body + "}\n", ""), ps=ps)["owned_pid_digest"], want, body)
        # the manager generation is the digits of one line, not a trimmed answer; the user manager's properties are LF records too
        for text, want in (("123\n", "123"), ("123", "123"), ("123\r\n", "unknown"), (" 123\n", "unknown"), ("123 \n", "unknown"), ("123\n\n", "unknown"), ("12\n3\n", "unknown"), ("", "unknown")):
            self.assertEqual(self.observed("launchd", managerpid=(0, text, ""))["manager_generation"], want, text)
        good = "MainPID=5\nInvocationID=%s\n" % ("ab" * 16)
        self.assertEqual(self.observed("systemd", unit=self.unit(), userman=(0, good, ""))["manager_generation"], "5:" + "ab" * 16)
        for text in (good.replace("\n", "\r\n"), good.replace("\n", "\x0c"), good.replace("\n", "\r")):
            self.assertEqual(self.observed("systemd", unit=self.unit(), userman=(0, text, ""))["manager_generation"], "unknown", text)

    def test_the_runner_guard_needs_the_runner_worker_process_not_only_exported_variables(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        function = self.shell_function("runner_ancestor")
        self.assertEqual((function.count("local ps=/bin/ps "), function.count('"$ps" -o args= -p'), function.count('"$ps" -o ppid= -p')), (1, 1, 1))
        self.assertIsNone(re.search(r"(?m)^[^#]*[^/\w$\"]ps -", function))  # no bare `ps` call: it would be resolved through the caller's PATH or an exported function
        # The driver's own `runner_ancestor`, with the absolute /bin/ps replaced by a stub executable that answers from a table ("pid|ppid|args"; SELF is the shell that runs the function).
        # A shell that merely exports GITHUB_ACTIONS=true and RUNNER_ENVIRONMENT=github-hosted has no runner worker among its ancestors, so it is refused before anything is written.
        worker = "/home/runner/runners/2.328.0/bin/Runner.Worker spawnclient 5 6"
        shell = ("SELF|500|/bin/bash scripts/ci/run-native-lifecycle.sh probe", "500|400|/usr/bin/bash -e /home/runner/work/_temp/step.sh")
        rows = lambda *more: "\n".join(shell + more)
        deep = tuple("%d|%d|/bin/sh" % (400 + i, 401 + i) for i in range(14))  # a worker beyond the bound
        with tempfile.TemporaryDirectory() as tmp:
            stub = pathlib.Path(tmp, "ps")
            stub.write_text(textwrap.dedent("""\
                #!/bin/bash
                want=$4 field=$2
                [ "$want" != "$SELF_PID" ] || want=SELF
                while IFS='|' read -r pid ppid args; do
                    [ "$pid" = "$want" ] || continue
                    case "$field" in args=) printf '%s\\n' "$args" ;; ppid=) printf '  %s\\n' "$ppid" ;; esac
                    exit 0
                done <<< "$PS_TABLE"
                exit 1
            """))
            stub.chmod(0o700)
            tested = function.replace("local ps=/bin/ps ", "local ps=%s " % shlex.quote(str(stub)))
            self.assertNotEqual(tested, function)
            for table, want in ((rows("400|300|" + worker, "300|1|/sbin/init"), 0), (rows("400|300|/Users/runner/runners/2.328.0/bin/Runner.Worker", "300|1|/sbin/launchd"), 0),
                                (rows("400|300|Runner.Worker spawnclient 5 6"), 0),
                                (rows("400|300|/usr/bin/sshd", "300|1|/sbin/launchd"), 1),  # an environment-only shell: no worker among its ancestors
                                (rows("400|300|/bin/bash /home/runner/runners/2.328.0/bin/Runner.Worker", "300|1|/sbin/launchd"), 1),  # the name as an argument is not the process
                                (rows("400|300|/x/NotRunner.Worker", "300|1|/sbin/launchd"), 1), (rows("400|300|/x/Runner.Worker.sh", "300|1|/sbin/launchd"), 1),
                                (rows("400|300|/x/Runner.Worker", "300|1|/sbin/launchd").replace("500|400|", "500|9999|"), 1),  # a broken chain: no row for the parent, ps fails
                                (rows("400|x|/bin/sh", "x|1|" + worker), 1), (rows("400||/bin/sh", "|1|" + worker), 1), (rows("400|0|/bin/sh", "0|0|" + worker), 1), (rows("400|1|/bin/sh"), 1),  # a bad, empty or init parent
                                (rows("400|500|/bin/sh"), 1),  # a cycle ends at the bound
                                ("\n".join(shell + ("400|401|/bin/sh",) + deep + ("414|1|" + worker,)), 1)):
                done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", 'export SELF_PID=$$\n' + tested + "runner_ancestor"], capture_output=True, text=True,
                                      env={"PATH": "/usr/bin:/bin", "PS_TABLE": table}, timeout=60)
                self.assertEqual(done.returncode, want, (table, done.stderr))
            # A caller-controlled `ps` is never consulted: a shim first in PATH and an exported shell function both claim a Runner.Worker ancestor and leave a marker if they run.
            marker = pathlib.Path(tmp, "ran")
            shimbin = pathlib.Path(tmp, "shimbin")
            shimbin.mkdir()
            shim = pathlib.Path(shimbin, "ps")
            shim.write_text('#!/bin/sh\necho "$0" >> "%s"\necho /x/Runner.Worker\n' % marker)
            shim.chmod(0o700)
            script = 'ps() { echo "function" >> %s; echo /x/Runner.Worker; }\nexport -f ps\n/bin/bash --noprofile --norc -c %s' % (
                shlex.quote(str(marker)), shlex.quote(function + "runner_ancestor"))
            subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True, env={"PATH": "%s:/usr/bin:/bin" % shimbin}, timeout=60)
            self.assertFalse(marker.exists(), marker.read_text() if marker.exists() else "")  # neither was run (the real /bin/ps answered; its verdict depends on where the tests run)
        guard = driver.index("runner_ancestor || refuse ")
        self.assertTrue(driver.index('[ "${RUNNER_ENVIRONMENT:-}" = github-hosted ]') < guard < driver.index("OS=$(uname -s)") < driver.index("mkdir -p \"$EV\""))  # before anything is written
        self.assertTrue(guard < driver.index("sudo -n"))  # and before any privileged call

    def test_observation_reads_the_managers_raw_bytes_under_the_products_byte_bound(self):
        answer = ("gui/%d/x.service = {\n\tpath = /p\n\tstate = not running\n}\n" % os.getuid()).encode()
        table = lambda eol: eol.join(("disabled services = {", '\t"x.service" => true', "}", "")).encode()
        seen = self.observed_real(answer, table("\n"))
        self.assertEqual((seen["active"], seen["enabled"], seen["owned_pid_digest"], seen["manager_generation"]), ("loaded", "false", "none", "123"))
        # A CRLF table is not the product's table (it reads the bytes as they are): the real `sh` must not translate CRLF to LF before the parse.
        self.assertEqual(self.observed_real(answer, table("\r\n"))["enabled"], "unknown")
        self.assertEqual(self.observed_real(answer.replace(b"\n", b"\r\n"), table("\n"))["owned_pid_digest"], "unknown")
        # The bound is in BYTES, as in the product: 40,000 two-byte characters are under 65,536 characters and over 65,536 bytes.
        wide = "\u00e9" * 40000
        big_table = ("disabled services = {\n\t\"y.service\" => \"%s\"\n}\n" % wide).encode()
        self.assertEqual(self.observed_real(answer, big_table)["enabled"], "unknown")
        big_answer = ("gui/%d/x.service = {\n\tstate = not running\n\tnote = %s\n}\n" % (os.getuid(), wide)).encode()
        seen = self.observed_real(big_answer, table("\n"))
        self.assertEqual((seen["active"], seen["origin"], seen["owned_pid_digest"]), ("unknown", "unknown", "unknown"))
        padded = ("disabled services = {\n\t\"y.service\" => \"%s\"\n}\n" % ("\u00e9" * 20000)).encode()  # 40,000 bytes: within the bound
        self.assertEqual(self.observed_real(answer, padded)["enabled"], "true")

    def test_counts_fail_on_an_unreadable_hits_log_and_only_a_missing_log_is_zero(self):
        host = self.load_host()

        def tally(path):
            with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
                code = host.counts({"log": path, "host": "x.test", "ident": H("a")}, [])
            return code, json.loads(out.getvalue()) if out.getvalue() else None
        with tempfile.TemporaryDirectory() as tmp:
            log = tmp + "/hits"
            self.assertEqual((tally(log)[0], tally(log)[1]["lines"]), (0, 0))  # no log yet: the first case starts before any hit
            pathlib.Path(log).write_text("curl refused url\nadapter unregister\n")
            code, got = tally(log)
            self.assertEqual((code, got["lines"], got["curl_refused"], got["unregister"]), (0, 2, 1, 1))
            pathlib.Path(log).write_bytes(b"\xff\xfe\n")
            self.assertEqual(tally(log), (1, None))  # undecodable: no tally at all
            if os.geteuid() != 0:
                os.chmod(log, 0)
                try:
                    self.assertEqual(tally(log), (1, None))  # unreadable: unknown, never zero refusals
                finally:
                    os.chmod(log, 0o600)

    def test_stop_signals_only_this_runs_fixtures_and_an_unanswered_ps_is_neither_gone_nor_ours(self):
        host = self.load_host()
        with tempfile.TemporaryDirectory() as run:
            pidfile = run + "/front.pid"
            ours = "python3 -I -B /s/native-lifecycle-host.py tls-front --run %s" % run
            for answer, status, kept, signalled in (((127, "", ""), 1, True, False),  # ps failed: the pid file is kept and the status says so
                                                    ((1, "", ""), 0, False, False),  # no such process
                                                    ((0, "python3 /s/native-lifecycle-host.py tls-front --run /elsewhere", ""), 0, False, False),  # another run's, or a recycled pid
                                                    ((0, ours, ""), 0, False, True)):  # still ours after the whole wait
                pathlib.Path(pidfile).write_text("4242")
                with unittest.mock.patch.object(host, "sh", return_value=answer), unittest.mock.patch.object(host.time, "sleep"), unittest.mock.patch("os.kill") as kill:
                    self.assertEqual(host.stop({"run": run}, []), status, answer)
                self.assertEqual((os.path.exists(pidfile), kill.called), (kept, signalled), answer)

    def test_trust_negative_is_refused_only_for_a_certificate_verification_failure(self):
        for status, want, fact in (("60", 0, "refused"), ("0", 1, "accepted"), ("22", 1, "accepted"), ("6", 1, "unknown"), ("7", 1, "unknown"), ("28", 1, "unknown"), ("35", 1, "unknown")):
            code, log, err = self.run_cleanup("negative_trust %s" % status, {}, extra=("negative_trust",))
            self.assertEqual((code, "fact trust_negative %s" % fact in log, "fail" in log), (want, True, want == 1), (status, log, err))

    def test_gui_domain_size_check_uses_one_captured_listing(self):
        stub = r"""launchctl() { case "$1" in print) [ "$LC_BYTES" != fail ] || return 1; head -c "$LC_BYTES" /dev/zero | tr '\0' x ;; print-disabled) printf 'disabled services = {\n}\n' ;; esac; }
"""
        with tempfile.TemporaryDirectory() as ev:
            for size, want, fact in (("fail", 0, "unavailable"), ("100", 0, "available"), ("65536", 0, "available"), ("65537", 1, "available")):  # a failed listing is never a zero-byte one
                code, log, err = self.run_cleanup(stub + "probe_gui", {"EV": ev, "SUID": "501", "LC_BYTES": size}, extra=("probe_gui",))
                self.assertEqual((code, "fact manager_pre %s" % fact in log, "fail" in log), (want, True, want == 1), (size, log, err))

    def test_gui_probe_needs_a_successful_disabled_table_with_its_header(self):
        header = "disabled services = {\n"
        stub = r"""launchctl() { case "$1" in print) printf x ;; print-disabled) cat "$LD_FILE"; return "$LD_RC" ;; esac; }
"""
        # The product's admission (relay/native-install-transaction.sh): at most 65536 BYTES, no NUL, the text starting with the header and ending with `}`.
        pad = 65536 - len(header) - len("\n}\n")  # a table of exactly the bound, trailing newline included

        def sized(total):  # a COMPLETE, framed table of exactly `total` bytes of two-byte characters (one ASCII byte when odd): at the bound it is under 33,000 characters
            fill = total - len(header) - len("\n}\n")
            return header + "\u00e9" * (fill // 2) + "x" * (fill % 2) + "\n}\n"
        for total in (65536, 65537, 65538):
            self.assertEqual((len(sized(total).encode()), len(sized(total)) < 65536), (total, True))
        # (print-disabled output, its status, manager_pre): available only for a SUCCESSFUL, COMPLETE answer; a matching header and then a failing producer is not.
        for out, status, fact in ((header + "}\n", "0", "available"), (header + '\t"x" => true\n}\n', "0", "available"), ("disabled services = {}\n", "0", "available"),
                                  (header + "x" * pad + "\n}\n", "0", "available"), (header + "x" * (pad + 1) + "\n}\n", "0", "unavailable"), (header + "x" * 70000 + "\n}\n", "0", "unavailable"),  # the bound
                                  (header, "0", "unavailable"), (header + '\t"x" => true\n', "0", "unavailable"), (header.rstrip("\n"), "0", "unavailable"),  # header only or truncated: no closing brace
                                  (header + "}\n", "1", "unavailable"), (header, "1", "unavailable"), (header + "}\n", "124", "unavailable"), (header + "}\n", "126", "unavailable"),
                                  (header + "}\n", "127", "unavailable"), ("", "0", "unavailable"), ("something else\n}\n", "0", "unavailable"),
                                  ("\n" + header + "}\n", "0", "unavailable"), ("not " + header + "}\n", "0", "unavailable"), (header + "} \n", "0", "unavailable"),
                                  (header + "\x00}\n", "0", "unavailable"), (header + "x\x00\n}\n", "0", "unavailable"), ("\x00" + header + "}\n", "0", "unavailable"),  # a NUL (a shell substitution would drop it)
                                  (header + "\u00e9" * 40000 + "\n}\n", "0", "unavailable"), (header + "\u00e9" * 20000 + "\n}\n", "0", "available"),  # bytes, not characters
                                  (sized(65536), "0", "available"), (sized(65537), "0", "unavailable"), (sized(65538), "0", "unavailable")):  # the bound itself, with the closing brace in the read: a character count would admit 65,537
            with tempfile.TemporaryDirectory() as ev:
                pathlib.Path(ev, "served").write_bytes(out.encode())
                code, log, err = self.run_cleanup(stub + "probe_gui", {"EV": ev, "SUID": "501", "LD_FILE": ev + "/served", "LD_RC": status}, extra=("probe_gui",))
            self.assertEqual((code, log.count("fact manager_pre"), "fact manager_pre %s" % fact in log, "fail" in log), (0, 1, True, False), (out[:80], status, log, err))
        # The domain listing is admitted as the product admits it: a NUL (which a shell substitution would drop) makes the manager unavailable, wherever it sits within the bound.
        listing_stub = r"""launchctl() { case "$1" in print) cat "$LP_FILE"; return "$LP_RC" ;; print-disabled) printf 'disabled services = {\n}\n' ;; esac; }
"""
        for listing, status, fact, hard in ((b"gui/501 = {\n}\n", "0", "available", False), (b"", "0", "available", False), (b"gui/501 = {\n}\n", "1", "unavailable", False),
                                            (b"\x00", "0", "unavailable", False), (b"gui/501 = {\n\x00}\n", "0", "unavailable", False), (b"x" * 100 + b"\x00", "0", "unavailable", False),
                                            (b"\x00" + b"x" * 70000, "0", "unavailable", False), (b"x" * 65535 + b"\x00", "0", "unavailable", False),  # a NUL inside the bound is unavailable, not a size failure
                                            (b"x" * 65536, "0", "available", False), (b"x" * 65537, "0", "available", True)):  # an over-limit listing is still the size failure
            with tempfile.TemporaryDirectory() as ev:
                pathlib.Path(ev, "served").write_bytes(listing)
                code, log, err = self.run_cleanup(listing_stub + "probe_gui", {"EV": ev, "SUID": "501", "LP_FILE": ev + "/served", "LP_RC": status}, extra=("probe_gui",))
            self.assertEqual((code, log.count("fact manager_pre"), "fact manager_pre %s" % fact in log, "fail" in log), (1 if hard else 0, 1, True, hard), (listing[:20], len(listing), status, log, err))
        # The listing's size comes from wc's own status, never from a pipeline's: a failed wc that printed a number is a failure, not a measurement within the bound.
        for wc, want in (('wc() { printf "      70000"; return 1; }', 1), ('wc() { printf "      5"; return 1; }', 1), ('wc() { command wc "$@"; }', 0)):
            with tempfile.TemporaryDirectory() as ev:
                pathlib.Path(ev, "served").write_bytes((header + "}\n").encode())
                code, log, err = self.run_cleanup(stub + wc + "\nprobe_gui", {"EV": ev, "SUID": "501", "LD_FILE": ev + "/served", "LD_RC": "0"}, extra=("probe_gui",))
            self.assertEqual((code, "fail cannot measure" in log), (want, want == 1), (wc, log, err))
        probe = self.text("scripts/ci/run-native-lifecycle.sh").split("probe_gui() {", 1)[1].split("\n}\n", 1)[0]
        # The earlier `a && b && c && d` chain (the Q1 pins named its `2> /dev/null` redirects) is now four guarded statements in the same order, each keeping its own status (the first PIPESTATUS element of
        # its stderr capture, which writes at most 4096 bytes to the evidence file); a capture that cannot be opened stays status 1.
        self.assertTrue('    rc=1\n    {\n        launchctl print "gui/$SUID" 2>&1 1>&3 3>&- \\\n            | { /usr/bin/head -c 4096 > "$EV/gui.err"; /bin/cat > /dev/null; } 3>&-\n'
                        '        rc=${PIPESTATUS[0]}\n    } 3> "$EV/gui.txt" || rc=$?\n    gs=$rc\n    if [ "$gs" -eq 0 ]; then\n        rc=0\n        OX nul-free --path "$EV/gui.txt" || rc=$?' in probe
                        and probe.index("OX nul-free") < probe.index("launchctl print-disabled"))  # the listing is admitted (its status, then the NUL check) before the table is asked for
        self.assertTrue('if [ "$gn" = clean ]; then\n        rc=1\n        {\n            launchctl print-disabled "gui/$SUID" 2>&1 1>&3 3>&- \\\n'
                        '                | { /usr/bin/head -c 4096 > "$EV/disabled.err"; /bin/cat > /dev/null; } 3>&-\n            rc=${PIPESTATUS[0]}\n        } 3> "$EV/disabled.txt" || rc=$?\n        ds=$rc\n'
                        '        if [ "$ds" -eq 0 ] && OX table-ok --path "$EV/disabled.txt"; then avail=yes; fi\n    fi' in probe)  # its own status, then the helper's admission of the file
        self.assertIn('    if [ "$avail" = yes ]; then\n        fact manager_pre available\n', probe)  # the only way to `available`: those two deciders and nothing recorded as evidence
        self.assertTrue('size=$(wc -c < "$EV/gui.txt") || fail' in probe and "$(" not in probe.split("OX table-ok", 1)[0].split("print-disabled", 1)[1] and not any("|" in l.replace("||", "").replace("&&", "") for l in probe.splitlines() if "print" in l or "wc" in l))  # no pipeline anywhere in the measurements

    def test_table_reason_names_the_first_framing_defect_and_agrees_with_table_ok_and_the_product(self):
        host, header = self.load_host(), "disabled services = {\n"
        frames = {
            "pass": [header + "}\n", header + '\t"x" => true\n}\n', "disabled services = {}\n", header + "}\n\n\n", "disabled services = {junk\n}\n", header + "\u00e9" * 20000 + "\n}\n",
                     header + "x" * (65536 - len(header) - 3) + "\n}\n"],  # the last: exactly the 65536-byte bound
            "over-limit": [header + "x" * (65536 - len(header) - 2) + "\n}\n", header + "x" * 70000 + "\n}\n", "x" * 65537, header + "\u00e9" * 40000 + "\n}\n", header + "\x00" + "x" * 70000 + "}"],
            "nul": [header + "\x00}\n", "\x00" + header + "}\n", header + "x\x00\n}\n", "\x00"],
            "header": ["something else\n}\n", "\n" + header + "}\n", " " + header + "}\n", "not " + header + "}\n", "}\n", "x", "Disabled services = {\n}\n", "disabled services = \n}\n"],
            "trailer": [header, header.rstrip("\n"), header + '\t"x" => true\n', header + "} \n", header + "}\r\n", header + "}\nextra", "disabled services = {\n}\n}x"],
            "empty": ["", "\n", "\n\n\n"]}
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "table")
            for want, texts in frames.items():
                for text in texts:
                    raw, where = text.encode(), (want, text[:40], len(text.encode()))
                    pathlib.Path(path).write_bytes(raw)
                    self.assertEqual(host.table_reason(raw[:65537]), want, where)
                    with contextlib.redirect_stdout(io.StringIO()) as out:
                        self.assertEqual(host.table_why({"path": path}, []), 0)  # the op prints the reason of the file's first LIMIT + 1 bytes
                    self.assertEqual(out.getvalue(), want + "\n", where)
                    self.assertEqual(host.table_ok({"path": path}, []) == 0, want == "pass", where)  # the admission itself, unchanged: `pass` exactly when table-ok admits
                    self.assertEqual(self.product_enabled(0, text, "com.x") != "unknown", want == "pass", where)  # and exactly when the product's own observer admits the table
        with contextlib.redirect_stdout(io.StringIO()) as out, tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "t").write_bytes(b"\xff" + header.encode() + b"}\n")
            self.assertEqual((host.main(["h", "table-reason", "--path", tmp + "/t"]), out.getvalue()), (0, "header\n"))  # invalid UTF-8 is decoded as table-ok decodes it
            self.assertEqual(host.table_why({"path": tmp + "/missing"}, []), 1)  # an unreadable file prints nothing: the driver records not-run

    def test_diagnostic_helper_ops_read_at_most_limit_plus_one_bytes_and_print_only_sanitized_text(self):
        host = self.load_host()
        junk = bytes(c for c in range(1, 128) if chr(c) not in HINT_CHARS and c != 10)  # every ASCII character the class drops (LF splits lines)
        for raw, head, tail in ((b"", "none", "none"), (b"\n\n  \n\t\r\n", "none", "none"), (b'disabled services = {\n\t"com.x" => true\n}\n', "disabled services = {", "}"),
                                (b"\n\n  could\nmiddle\nfound  \n\n", "  could", "found  "), (b"\n\n  first\nmiddle\nlast  \n\n", "  <x>", "<x>  "), (b"a\r\nb\r\n", "a", "<x>"), (b"only", "<x>", "<x>"),
                                (HINT_CHARS.replace("/", "").encode(), '<x> ={}"<>()._:-', '<x> ={}"<>()._:-'), (("x" * 200 + "\nend").encode(), "<x>", "<x>"), (("y" * 64).encode(), "<x>", "<x>"),
                                (b";;;\n!!\n", "none", "none"), ("\u00e9\u0661\n".encode(), "none", "none"), (b"a\x00b\nc\x1b[31md\n", "<x>", "<x>"),
                                (b"Bad request.\nUsage: launchctl print <domain-target> | <service-target>\n", "Bad request.", "Usage: launchctl print <domain-target>  <service-target>"),
                                (b"user root denied\n", "user <x> denied", "user <x> denied"),
                                (junk + b"ok\n" + junk + b"\n", "ok", "ok"), (junk + b"\n", "none", "none"), (b"\xff\xfe" + b"ok", "ok", "ok")):
            self.assertEqual((host.shape_hint(raw, False), host.shape_hint(raw, True)), (head, tail), raw[:40])
            for hint in (head, tail):
                self.assertRegex(hint, "^[A-Za-z0-9 ={}\"<>()._:/-]{1,64}$")  # exactly the receipt's pattern: what the helper prints is what the gate accepts
                self.assertTrue(m.hint_ok(hint), hint)
        self.assertEqual(sum(1 for c in junk if chr(c) in HINT_CHARS), 0)
        for name, status in (("clean", 0), ("with-nul", 1)):
            with tempfile.TemporaryDirectory() as tmp:
                pathlib.Path(tmp, "f").write_bytes(b"ab" if status == 0 else b"a\x00b")
                with contextlib.redirect_stdout(io.StringIO()) as out:
                    self.assertEqual((host.nul_free({"path": tmp + "/f"}, []), out.getvalue()), (status, ""), name)  # 0 clean, 1 NUL, nothing printed
                    self.assertEqual([host.nul_free({"path": tmp + "/missing"}, []), host.nul_free({"path": tmp}, [])], [2, 2])  # unreadable: neither clean nor NUL-bearing
        class Spy:
            def __init__(self, real):
                self.real, self.asked = real, []

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                self.real.close()

            def fileno(self):
                return self.real.fileno()

            def read(self, size=-1):
                self.asked.append(size)
                return self.real.read(size)
        with tempfile.TemporaryDirectory() as tmp:
            big = b"could\n" + b"no " * 40000 + b"\nfound\n"  # known words, so the hints show which line they came from
            pathlib.Path(tmp, "big").write_bytes(big)
            o = {"path": tmp + "/big"}
            for name, call, want in (("table-ok", lambda: host.table_ok(o, []), None), ("nul-free", lambda: host.nul_free(o, []), None), ("table-reason", lambda: host.table_why(o, []), "over-limit\n"),
                                     ("shape head", lambda: host.shape(dict(o, field="head"), []), "could\n"), ("shape tail", lambda: host.shape(dict(o, field="tail"), []), "no " * 20 + "no\n")):
                spies = []

                def opener(path, mode="r", spies=spies):
                    spies.append(Spy(open(path, mode)))
                    return spies[-1]
                with unittest.mock.patch.object(host, "open", opener, create=True), contextlib.redirect_stdout(io.StringIO()) as out:
                    call()
                self.assertEqual([spy.asked for spy in spies], [[host.LIMIT + 1]], name)  # one read, of at most LIMIT + 1 bytes, however large the file
                self.assertTrue(want is None or out.getvalue() == want, (name, out.getvalue()))  # the tail is the last line WITHIN that window, never a later one
            spies.clear()
            with unittest.mock.patch.object(host, "open", opener, create=True), contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(host.shape(dict(o, field="bytes"), []), 0)
            self.assertEqual(([spy.asked for spy in spies], out.getvalue()), ([[]], "%d\n" % len(big)))  # the size comes from fstat: nothing is read
            with contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(host.main(["h", "shape", "--path", tmp + "/big", "--field", "head"]), 0)
                self.assertEqual(host.shape(dict(o, path=tmp + "/missing", field="head"), []), 1)
            self.assertEqual(out.getvalue(), "could\n")
            with self.assertRaises(SystemExit):
                host.shape(dict(o, field="body"), [])  # an unknown field, or none

    def gui_probe(self, print_rc=0, listing=b"gui/501 = {\n}\n", print_err="", dis_rc=0, table=b"disabled services = {\n}\n", dis_err="", shell="", after=""):
        """probe_gui as written in the driver, with `launchctl` a function stub serving these bytes, statuses and stderr and the helper's ops running for real over the captured files.
        Returns (exit status, [(fact, value)] in the order recorded, log)."""
        stub = r"""launchctl() { case "$1" in print) cat "$LP_FILE"; printf '%s' "$LP_ERR" >&2; return "$LP_RC" ;; print-disabled) cat "$LD_FILE"; printf '%s' "$LD_ERR" >&2; return "$LD_RC" ;; esac; }
"""
        with tempfile.TemporaryDirectory() as ev:
            pathlib.Path(ev, "listing").write_bytes(listing)
            pathlib.Path(ev, "table").write_bytes(table)
            env = {"EV": ev, "SUID": "501", "LP_FILE": ev + "/listing", "LP_RC": str(print_rc), "LP_ERR": print_err, "LD_FILE": ev + "/table", "LD_RC": str(dis_rc), "LD_ERR": dis_err}
            code, log, err = self.run_cleanup(stub + shell + 'probe_gui >> "$LOG"' + after, env, extra=("probe_gui",))  # the function's own output joins the log (fact lines start with `fact `)
        return code, [tuple(line[5:].split(" ", 1)) for line in log.splitlines() if line.startswith("fact ")], log

    GUI_FACTS = ("gui_print_status", "gui_nul", "gui_bytes", "gui_print_err", "disabled_status", "disabled_bytes", "disabled_admission", "disabled_head", "disabled_tail", "disabled_err")

    def test_gui_probe_records_every_diagnostic_fact_on_the_passing_and_every_failing_path(self):
        host, header = self.load_host(), "disabled services = {\n"
        table = '\t"com.x" => true\n}\n'.join((header, "")).encode()
        none = {"disabled_status": "not-run", "disabled_bytes": "not-run", "disabled_admission": "not-run", "disabled_head": "none", "disabled_tail": "none", "disabled_err": "none"}
        scenarios = (
            (dict(listing=b"gui/501 = {\n}\n", table=table, print_err="warn: x\n"), 0, "available", {
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "warn: <x>", "disabled_status": "0", "disabled_bytes": str(len(table)), "disabled_admission": "pass",
                "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none"}),
            (dict(print_rc=113, listing=b"", print_err="Could not find domain for user gui: 501\n"), 0, "unavailable", dict(none, gui_print_status="113", gui_nul="not-run", gui_bytes="0",
                                                                                                                                gui_print_err="Could not find domain for user gui: 501")),
            (dict(print_rc=127, listing=b"", print_err=""), 0, "unavailable", dict(none, gui_print_status="127", gui_nul="not-run", gui_bytes="0", gui_print_err="none")),
            (dict(listing=b"gui/501 = {\n\x00}\n"), 0, "unavailable", dict(none, gui_print_status="0", gui_nul="nul", gui_bytes="15", gui_print_err="none")),
            (dict(dis_rc=2, table=b"", dis_err="Could not\x01 find domain; x\n"), 0, "unavailable", {
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "none", "disabled_status": "2", "disabled_bytes": "0", "disabled_admission": "empty",
                "disabled_head": "none", "disabled_tail": "none", "disabled_err": "Could not find domain <x>"}),
            (dict(dis_rc=1, table=table), 0, "unavailable", {  # a complete table and a failing producer: the frame is fine, the status refuses it
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "none", "disabled_status": "1", "disabled_bytes": str(len(table)), "disabled_admission": "pass",
                "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none"}),
            (dict(listing=b"x" * 65537), 1, "available", dict(none, gui_print_status="0", gui_nul="clean", gui_bytes="65537", gui_print_err="none", disabled_status="0", disabled_bytes="24",  # the size failure
                                                              disabled_admission="pass", disabled_head="disabled services = {", disabled_tail="}")))
        for kwargs, want_exit, manager_pre, want in scenarios:
            code, pairs, log = self.gui_probe(**kwargs)
            self.assertEqual(sorted(k for k, _ in pairs), sorted(self.GUI_FACTS + ("manager_pre",)), log)  # every diagnostic fact, and the verdict fact, exactly once: whatever the outcome
            self.assertEqual(dict(pairs), dict(want, manager_pre=manager_pre), log)
            self.assertEqual((code, "fail the gui domain listing exceeds" in log), (want_exit, want_exit == 1), log)
        # Every table frame: the recorded admission is the helper's reason, and only `pass` is available. A hint carries nothing outside the sanitized class and at most 64 characters.
        for text, reason in (("something else\n}\n", "header"), (header, "trailer"), ("\n\n", "empty"), (header + "\x00}\n", "nul"), (header + "x" * 70000 + "\n}\n", "over-limit"), (header + "}\n", "pass")):
            code, pairs, log = self.gui_probe(table=text.encode())
            self.assertEqual((dict(pairs)["disabled_admission"], dict(pairs)["manager_pre"]), (reason, "available" if reason == "pass" else "unavailable"), reason)
            for name in ("disabled_head", "disabled_tail"):
                self.assertRegex(dict(pairs)[name], "^[A-Za-z0-9 ={}\"<>()._:/-]{1,64}$")
        # Parity with the earlier `&&` chain, evaluated here independently: manager_pre is `available` ONLY for a successful, NUL-free listing, a successful table and a framed table within the bound.
        old_available = lambda print_rc, listing, dis_rc, table: (print_rc == 0 and b"\x00" not in listing[:65537] and dis_rc == 0 and len(table[:65537]) <= 65536
                                                                  and host.framed_table(table[:65537].decode("utf-8", "replace")))
        tables = (table, b"nope\n}\n", header.encode(), b"", header.encode() + b"\x00}", b"x" * 65537)
        runs = [(rc, b"gui/501 = {\n}\n", 0, table) for rc in (1, 113, 127)] + [(0, b"x\x00y", 0, table)] + [(0, b"gui/501 = {\n}\n", rc, t) for rc in (0, 3) for t in tables]
        for print_rc, listing, dis_rc, text in runs:
            code, pairs, log = self.gui_probe(print_rc=print_rc, listing=listing, dis_rc=dis_rc, table=text)
            got = dict(pairs)
            self.assertEqual(got["manager_pre"] == "available", old_available(print_rc, listing, dis_rc, text), (print_rc, listing[:8], dis_rc, text[:20], log))
            stage = [got["gui_print_status"], got["gui_nul"], got["disabled_status"], got["disabled_admission"]]
            self.assertEqual(stage[:2], [str(print_rc), "not-run" if print_rc else ("nul" if b"\x00" in listing else "clean")])
            self.assertEqual(stage[2:] == ["not-run", "not-run"], print_rc != 0 or b"\x00" in listing)  # a stage that is not reached is `not-run`, exactly where the chain stopped
            self.assertTrue(stage[2:] == ["not-run", "not-run"] or (stage[2] == str(dis_rc) and stage[3] == host.table_reason(text[:65537])), (stage, log))
            self.assertEqual(code, 0, log)
        # The evidence helpers decide nothing: with `shape` and `table-reason` failing, the verdict and the deciding facts are those of the real helper, and the evidence falls back to `not-run` / `none`.
        evidence_down = 'OX() { case "$1" in shape | table-reason) return 1 ;; esac; "$PYO" -I -B "$HOSTPY" "$@"; }\n'
        fallback = {"gui_bytes": "not-run", "gui_print_err": "none", "disabled_bytes": "not-run", "disabled_admission": "not-run", "disabled_head": "none", "disabled_tail": "none", "disabled_err": "none"}
        for text, manager_pre in ((table, "available"), (b"nope\n}\n", "unavailable"), (header.encode(), "unavailable")):
            code, pairs, log = self.gui_probe(table=text, shell=evidence_down)
            self.assertEqual((code, dict(pairs)), (0, dict(fallback, gui_print_status="0", gui_nul="clean", disabled_status="0", manager_pre=manager_pre)), log)
        # An unreadable listing (the NUL check exits 2) is neither clean nor NUL-bearing: recorded as not-run, the manager unavailable, the table never asked for.
        code, pairs, log = self.gui_probe(shell='OX() { case "$1" in nul-free) return 2 ;; esac; "$PYO" -I -B "$HOSTPY" "$@"; }\n')
        self.assertEqual((code, dict(pairs)["gui_nul"], dict(pairs)["disabled_status"], dict(pairs)["manager_pre"]), (0, "not-run", "not-run", "unavailable"), log)
        probe = self.text("scripts/ci/run-native-lifecycle.sh").split("probe_gui() {", 1)[1].split("\n}\n", 1)[0]
        code = "\n".join(l for l in probe.splitlines() if not l.lstrip().startswith("#"))
        self.assertEqual(re.findall(r"launchctl (\S+)", code), ["print", "print-disabled"])  # the only manager queries are the two read-only ones the earlier probe made
        self.assertEqual(set(re.findall(r"OX (\S+)", code)), {"nul-free", "table-ok", "table-reason", "shape"})  # and the only helper reads: read-only ops over the captured files
        self.assertIsNone(re.search(r"\b(sudo|bsudo|priv|HX|as_subject|bootstrap|bootout|kickstart|rm|mv|tee|chmod)\b", code))  # no new host effect
        self.assertTrue('head -c 4096 > "$EV/gui.err"' in code and 'head -c 4096 > "$EV/disabled.err"' in code and code.count("2> /dev/null") == 7)  # stderr of both queries is kept, capped, in files under $EV; only the evidence helpers' own stderr is dropped
        self.assertFalse(re.search(r'launchctl print[^\n]*2> ?"?\$EV', code) or re.search(r'2> "\$EV/[a-z]+\.err"', code))  # no query writes its stderr straight to a file: every byte goes through the cap
        self.assertEqual(code.count("PIPESTATUS[0]"), 2)  # each query's own status, never the capture's
        self.assertTrue(all(code.count('fact %s "$' % name) == 1 for name in self.GUI_FACTS))  # recorded once, from a variable that every path assigns (`not-run` / `none` unless its stage ran)
        self.assertTrue(code.index('fact disabled_err "$de"') < code.index("fact manager_pre available") < code.index("fail ") and code.index('fact disabled_err "$de"') < code.index("fact manager_pre unavailable"))  # before any fail

    def test_launchctl_stderr_capture_is_capped_and_never_changes_a_status_or_the_verdict(self):
        flood = r"""launchctl() { case "$1" in
    print) cat "$LP_FILE"; { printf 'no error\n'; head -c 5242880 /dev/zero | tr '\0' x; printf '\nlast\n'; } >&2; return "$LP_RC" ;;
    print-disabled) cat "$LD_FILE"; { printf 'bad request\n'; head -c 5242880 /dev/zero | tr '\0' x; printf '\nlast\n'; } >&2; return "$LD_RC" ;; esac; }
"""
        sizes = r"""; for f in gui disabled; do [ ! -f "$EV/$f.err" ] || printf 'errsize %s %s\n' "$f" "$(wc -c < "$EV/$f.err")" >> "$LOG"; done"""
        table = b'disabled services = {\n\t"com.x" => true\n}\n'
        # (print status, table status, expected gui status, expected table status, manager_pre, files written): a 5 MiB stderr stream per query neither fills the file past 4096 bytes nor kills the query with SIGPIPE,
        # so each query's own status survives and the verdict is the unchanged one.
        for print_rc, dis_rc, want_gs, want_ds, manager_pre, files in ((0, 0, "0", "0", "available", ("gui", "disabled")), (113, 0, "113", "not-run", "unavailable", ("gui",)),
                                                                          (0, 3, "0", "3", "unavailable", ("gui", "disabled")), (0, 127, "0", "127", "unavailable", ("gui", "disabled"))):
            code, pairs, log = self.gui_probe(print_rc=print_rc, dis_rc=dis_rc, table=table, shell=flood, after=sizes)
            got = dict(pairs)
            self.assertEqual((code, got["gui_print_status"], got["disabled_status"], got["manager_pre"], got["gui_print_err"]), (0, want_gs, want_ds, manager_pre, "no error"), log)
            self.assertEqual(re.findall(r"errsize (\w+)\s+(\d+)", log), [(f, "4096") for f in files], log)  # capped at exactly 4096 bytes, and only the queries that ran wrote anything
            self.assertTrue(want_ds == "not-run" or got["disabled_err"] == "bad request", log)
        # A stderr file that cannot be written is diagnostic-only: the query's own status and the verdict are untouched and the hint is `none`. A stdout file that cannot be opened is the fail-closed case:
        # status 1, unavailable, under errexit too.
        code, pairs, log = self.gui_probe(shell='set -e\nmkdir "$EV/gui.err"\n')
        self.assertEqual((code, dict(pairs)["gui_print_status"], dict(pairs)["gui_print_err"], dict(pairs)["manager_pre"]), (0, "0", "none", "available"), log)
        code, pairs, log = self.gui_probe(print_rc=113, shell='set -e\nmkdir "$EV/gui.err"\n')
        self.assertEqual((code, dict(pairs)["gui_print_status"], dict(pairs)["manager_pre"]), (0, "113", "unavailable"), log)
        for name, want in (("gui.txt", ("1", "not-run", "unavailable")), ("disabled.txt", ("0", "1", "unavailable"))):
            code, pairs, log = self.gui_probe(shell='set -e\nmkdir "$EV/%s"\n' % name)
            got = dict(pairs)
            self.assertEqual((code, got["gui_print_status"], got["disabled_status"], got["manager_pre"]), (0,) + want, (name, log))

    def test_hints_replace_every_path_like_token_and_the_gate_refuses_one_that_survives(self):
        host = self.load_host()
        # Everything from the FIRST path-like token to the end of the line becomes one `<path>` (a path may hold blanks); text before it, and the two non-path spellings launchd prints, stay.
        for raw, want in ((b"/PRIVATE/HOME/Library/LaunchAgents/private.plist: denied\n", "<path>"), (b"cannot read /Users/runner/work/_temp/x.plist now\n", "cannot read <path>"),
                          (b'"/Users/runner/x.plist" => true\n', "<path>"), (b"~/Library/x\n", "<path>"), (b"./x ../y\n", "<path>"), (b"file:///Users/r/x\n", "<path>"),
                          (b"https://h.example/x\n", "<path>"), (b"a;/Users/r\n", "<path>"), (b"a\t/Users/r/x\n", "<path>"), ("x\u00e9/Users/r\n".encode(), "<path>"), (b"/Library/x /y/z\n", "<path>"),
                          (b"a/b/c\n", "<path>"), (b"a/b\n", "<path>"), (b"a/\n", "<path>"), (b"/\n", "<path>"), (b"gui/501/x\n", "<path>"), (b"input/output\n", "<path>"),
                          (b"/Users/John Smith/x\n", "<path>"), (b"/Users/John Smith\n", "<path>"), (b"/Users/a/My File.plist denied\n", "<path>"), (b"Library/Application Support/x\n", "<path>"),
                          (b"read /Users/John Smith/x failed\n", "read <path>"), (b"gui/501 not /Users/John Smith/x\n", "gui/501 not <path>"), (b"user/501 pid/12 Input/output\n", "user/501 pid/12 Input/output"),
                          (b"Bootstrap failed: 5: Input/output error\n", "Bootstrap failed: 5: Input/output error"), (b"Could not find domain for gui/501\n", "Could not find domain for gui/501"),
                          # the cut falls between tokens, never inside one (a cut word could be a fragment of a name): a token that does not fit, and all after it, is dropped
                          (("could " * 9 + "ab/cd").encode(), "could " * 9 + "<path>"), (("could " * 10 + "ab/cd").encode(), "could " * 9 + "could"), (("could " * 8 + "gui/501").encode(), "could " * 8 + "gui/501"),
                          (("could " * 10 + "gui/501").encode(), "could " * 9 + "could"), (("x" * 60 + " ab/cd").encode(), "<x> <path>"), (("x" * 31 + " ab/cd").encode(), "<x> <path>"),
                          # F001 (Q1DR): only known words, numbers and punctuation survive; a name, host, label, encoded or joined text becomes `<x>`
                          (b"launchctl error %2FUsers%2Frunner%2Fsecret\n", "launchctl error <x>"), (b"%2FUsers%2Frunner%2Fsecret\n", "<x>"), (b"host john-macbook.local denied\n", "<x> <x> denied"),
                          (b"user john denied\n", "user <x> denied"), (b'Could not find service "com.john.agent" in domain for gui/501\n', "Could not find service <x> in domain for gui/501"),
                          (b"Users\\John\\x\n", "<x>"), (b"C:\\Users\\john\n", "<x>"), (b"runner@Mac-1234.local: denied\n", "<x> denied"), (b"COULD NOT FIND DOMAIN\n", "COULD NOT FIND DOMAIN"),
                          (b"Couldnot find\n", "<x> find"), (b"could.not find\n", "<x> find"), (b"{domain} (gui: 501)\n", "{domain} (gui: 501)"), (b"gui: 1234567\n", "gui: <x>"), (b"gui/1234567\n", "<path>"),
                          (b"user root denied\n", "user <x> denied"), (b"user admin daemon nobody denied\n", "user <x> <x> <x> denied"),  # account-like words are not in the vocabulary
                          (b"a" * 33 + b" could\n", "<x> could"), (b"{" * 33 + b" could\n", "<x> could"), (b"{" * 32 + b" could\n", "{" * 32 + " could"), (b"5: 12345678\n", "5: <x>")):
            self.assertEqual(host.shape_hint(raw, False), want, raw)
        # F001, round 2 (Q1DR): a user or host named like a known word is still a name. The producing host redacts the names it knows (identities()), whatever the vocabulary says; the gate cannot know them.
        for raw, names, want in ((b"user system denied\n", ("system",), "user <x> denied"), (b"User SYSTEM denied\n", ("System",), "User <x> denied"), (b'"system": denied\n', ("system",), "<x> denied"),
                                 (b"domain target\n", ("tar",), "domain <x>"), (b"domain target\n", ("ta",), "domain target"), (b"no a\n", ("a",), "no <x>"), (b"could not\n", ("",), "could not"),
                                 (b"user gui: 501\n", ("gui", "501"), "user <x> <x>"), (b"user system denied\n", (), "user system denied")):
            self.assertEqual(host.shape_hint(raw, False, names), want, (raw, names))
            self.assertTrue(m.hint_ok(want), want)  # what is left is still what the gate accepts
        with unittest.mock.patch.dict(os.environ, {"USER": "Jane-Doe", "LOGNAME": "jd", "HOME": "/Users/Some.Body/"}), unittest.mock.patch.object(host.socket, "gethostname", return_value="Jane-Mac.local"):
            got = host.identities()
        self.assertTrue({"jane-doe", "jane", "doe", "jd", "some.body", "some", "body", "jane-mac.local", "mac", "local"} <= got and "" not in got)  # whole and by part, lowercase
        with unittest.mock.patch.dict(os.environ, {}, clear=True), unittest.mock.patch.object(host.socket, "gethostname", side_effect=OSError), unittest.mock.patch.object(host.pwd, "getpwuid", side_effect=KeyError):
            self.assertEqual(host.identities(), frozenset())  # a name that cannot be read adds nothing and fails nothing
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stdout(io.StringIO()) as out, unittest.mock.patch.object(host, "identities", return_value=frozenset({"disabled"})):
            pathlib.Path(tmp, "t").write_bytes(b"disabled services = {\n}\n")
            self.assertEqual((host.shape({"path": tmp + "/t", "field": "head"}, []), out.getvalue()), (0, "<x> services = {\n"))  # the `shape` op applies them
        rng = random.Random(7)  # nothing after a path's first token ever survives, however the rest is spelled
        for start in ("/Users/John", "~/x", "./y", "a/b/c", "Library/Application", "file:///Users/Jo", "a/b"):
            for tail in ("Smith/x", "Smith", "ZQXLEAK1/y z", "ZQXLEAK2 gui/501 ZQXLEAK3", "File.plist: ZQXLEAK4", "<path> ZQXLEAK5"):
                for before in ("", "read ", "Could not find "):
                    hint = host.shape_hint(("%s%s %s\n" % (before, start, tail)).encode(), False)
                    self.assertEqual(hint, before + "<path>", (before, start, tail))
                    self.assertTrue(m.hint_ok(hint))
        leaks = ("ZQXLEAK", "john", "runner", "macbook", "2F", "%2F", "Users", "local")  # names, hosts and encodings the sanitizer has to drop whatever surrounds them
        pieces = ["/", "//", ".", "..", "~", ";", ":", " ", " ", "\t", "\r", "\n", "a", "ab", "Z9", "\u00e9", "<", ">", "-", "_", "gui", "501", "x/y", "Input/output", "/Users/r", "{", "}", '"', "=", "\x00", "ab/c",
                  "could", "not", "find", "domain", "user", "<x>", "<path>"] + list(leaks)
        rng = random.Random(20260101)
        for _ in range(4000):
            raw = "".join(rng.choice(pieces) for _ in range(rng.randrange(0, 120)))
            for hint in (host.shape_hint(raw.encode(), False), host.shape_hint(raw.encode(), True)):
                self.assertRegex(hint, r'^[A-Za-z0-9 ={}"<>()._:/-]{1,64}$')
                self.assertFalse(any(host.pathlike(t) for t in hint.split(" ")), (raw, hint))  # whatever the input, no path-like token survives the cut
                self.assertNotIn("<path>", hint.split(" ")[:-1])  # and a `<path>` is always the last token: nothing that followed a path's start is kept
                self.assertTrue(m.hint_ok(hint), (raw, hint))  # and the receipt gate accepts exactly what the helper prints
                self.assertTrue(all(m.safe(t) for t in hint.split(" ")), (raw, hint))  # every token is punctuation, a known word, a number, a non-path spelling or a marker...
                self.assertIsNone(re.search("|".join(re.escape(w) for w in leaks if w[0] not in "2%"), hint, re.I), (raw, hint))  # ...so a name or host never survives (`2F` alone is a number's fragment)
                self.assertNotIn("2F", hint)
            token = "".join(rng.choice(pieces[:2] + pieces[4:5] + pieces[6:9] + pieces[12:21] + pieces[23:]) for _ in range(rng.randrange(0, 12)))
            self.assertEqual((host.pathlike(token), host.safe(token)), (m.pathlike(token), m.safe(token)), token)  # one rule on both sides
        for token, path in (("gui/501", False), ("user/9", False), ("pid/1", False), ("Input/output", False), ("input/output", True), ("Input/outputs", True), ("gui/", True), ("gui/5x", True), ("x/501", True),
                            ("session/5", True), ("x/y", True), ("a/b", True), ("/", True), ("gui/501/x", True), ("plain", False), ("<path>", False), ("a.b:c", False), ("gui/123456", False), ("gui/1234567", True)):
            self.assertEqual((host.pathlike(token), m.pathlike(token)), (path, path), token)  # the two non-path spellings are exactly these, on both sides
        # The vocabulary and the token rule are one rule on both sides: the same words, the same non-path spellings, the same verdict on every word, its decorations and mutations, and on odd tokens.
        self.assertEqual((host.WORDS, host.NONPATH), (m.WORDS, m.NONPATH))
        self.assertTrue(len(m.WORDS) > 100 and all(w == w.lower() and re.fullmatch(r"[a-z-]{1,20}", w) for w in m.WORDS) and not {"john", "runner", "users", "local", "admin", "x", "root", "daemon", "nobody", "wheel", "staff", "guest", "persona"} & m.WORDS)
        odd = ["", "<x>", "<path>", "{", "=>", "5:", "501)", "1234567", "123456", "gui/501", "Input/output", "input/output", "a" * 32, "a" * 33, "{" * 32, "{" * 33, "<x>:", "<xx>", "<path>.", "2F", "a.b", "Z9"]
        for word in sorted(m.WORDS):
            for token in (word, word.upper(), word.capitalize(), '"%s"' % word, "{%s}" % word, word + ":", word + "s", word[:-1], "x" + word, word + "." + word, word + "/" + word, "<%s>" % word):
                odd.append(token)
                self.assertEqual(host.safe(token), m.safe(token), token)
        self.assertTrue(all(m.safe(w) and m.safe(w.upper()) and m.safe('"%s":' % w) for w in m.WORDS if len(w) <= 20))  # every word is kept, in any case and with punctuation around it
        self.assertFalse(any(m.safe(w + "x") or m.safe("x" + w) or m.safe(w + "." + w) for w in m.WORDS))  # and nothing built from one is
        self.assertTrue(all(host.safe(t) == m.safe(t) for t in odd), odd)
        code, pairs, log = self.gui_probe(print_err="/PRIVATE/HOME/Library/LaunchAgents/private.plist: denied\n", dis_err="/Users/runner/x denied\n", table=b"/Users/r/y\n}\n")
        got = dict(pairs)
        self.assertEqual((got["gui_print_err"], got["disabled_err"], got["disabled_head"], got["disabled_tail"], got["disabled_admission"]), ("<path>", "<path>", "<path>", "}", "header"), log)
        self.assertIn("gui probe: status=0 nul=clean", log)  # the log line carries the same, redacted, facts
        self.assertFalse(any(word in log for word in ("PRIVATE", "Users", "runner", "LaunchAgents")), log)
        # F001 (Q1DR): percent-encoded paths, a host or user name with no slash, a quoted label: through the real function, into the facts and the log line
        code, pairs, log = self.gui_probe(print_err="launchctl error %2FUsers%2Frunner%2Fsecret\n", dis_err="host john-macbook.local denied\n", table=b'"com.john.agent" => true\n}\n')
        got = dict(pairs)
        self.assertEqual((got["gui_print_err"], got["disabled_err"], got["disabled_head"], got["disabled_tail"], got["disabled_admission"]), ("launchctl error <x>", "<x> <x> denied", "<x> => true", "}", "header"), log)
        self.assertIn("gui probe: status=0 nul=clean bytes=14 err=[launchctl error <x>]", log)
        self.assertFalse(any(word in log for word in ("2F", "Users", "runner", "secret", "john", "macbook", "com.", "agent")), log)

    def test_baseline_names_the_first_failing_sub_check_in_the_frozen_order_and_decides_as_before(self):
        order = ("path-conf", "path-release", "path-cache", "path-definition", "path-legacy-definition", "path-curlrc", "path-shim", "manager-path-unread", "command-provenance", "label-active",
                 "legacy-active", "hosts")
        self.assertEqual(order, m.DIAG_ENUM["baseline_failed"][1:-1])  # the receipt's enum is the driver's order (`none` first, `not-run` last)
        stub = r"""has() { case " $FAILS " in *" $1 "*) return 0 ;; esac; return 1; }
sgone() { case "$1" in "$CONF") has path-conf ;; "$REL") has path-release ;; "$THOME/.cache/herdr-mobile-relay") has path-cache ;; "$DEF") has path-definition ;; "$LDEF") has path-legacy-definition ;;
    "$THOME/.curlrc") has path-curlrc ;; "$SHIMDIR/herdr-mobile-relay") has path-shim ;; *) return 3 ;; esac; test $? -eq 1; }
as_subject() { case "$1" in systemctl | launchctl) ! has manager-path-unread && echo PATH=/usr/bin:/bin ;; /bin/sh) ! has command-provenance ;; *) return 3 ;; esac; }
snap() { if has label-active; then echo '{"active": "loaded"}'; else echo '{"active": "absent"}'; fi; }
snap_of() { if has legacy-active; then echo '{"active": "Act;ive"}'; else echo '{"active": "absent"}'; fi; }
absent() { ! has hosts; }
"""
        env = {"CONF": "/h/conf", "REL": "/h/rel", "THOME": "/h", "DEF": "/h/def", "LDEF": "/h/ldef", "SHIMDIR": "/h/shim", "MGRPATH": "/usr/bin/systemctl", "LEGACY": "x.service"}
        # (failing sub-checks present, expected first failure, label state, legacy state); the same failure set is also tried alone
        expected = {"label-active": ("loaded", "not-run"), "legacy-active": ("absent", "ctive"), "hosts": ("absent", "absent")}
        for os_name in ("Linux", "Darwin"):
            for k, name in enumerate(order):
                for fails in (order[k:], (name,)):
                    code, log, err = self.run_cleanup(stub + 'baseline_clean; echo "rc $? $BASEFAIL" >> "$LOG"', dict(env, OS=os_name, FAILS=" ".join(fails)), extra=("baseline_clean", "baseline_note", "state_fact"))
                    label, legacy = expected.get(name, ("not-run", "not-run"))
                    want = ["fact baseline_failed " + name, "fact baseline_label_state " + label, "fact baseline_legacy_state " + legacy, "rc 1 " + name]
                    self.assertEqual(log.splitlines(), want, (os_name, fails, err))  # one outcome, the FIRST failing sub-check, and it fails closed
            code, log, err = self.run_cleanup(stub + 'baseline_clean; echo "rc $? $BASEFAIL" >> "$LOG"', dict(env, OS=os_name, FAILS=""), extra=("baseline_clean", "baseline_note", "state_fact"))
            self.assertEqual(log.splitlines(), ["fact baseline_failed none", "fact baseline_label_state absent", "fact baseline_legacy_state absent", "rc 0 none"], (os_name, err))
        # A failed observation is `unknown` (never `absent`): nothing printed by snap, or a broken answer.
        for snap_stub in ("snap() { return 1; }", "snap() { echo '{\"active\": \"\"}'; }", "snap() { echo garbage; }"):
            code, log, err = self.run_cleanup(stub + snap_stub + '\nbaseline_clean; echo "rc $?" >> "$LOG"', dict(env, OS="Linux", FAILS=""), extra=("baseline_clean", "baseline_note", "state_fact"))
            self.assertEqual(log.splitlines(), ["fact baseline_failed label-active", "fact baseline_label_state unknown", "fact baseline_legacy_state not-run", "rc 1"], (snap_stub, err))
        for value, want in (("loaded", "loaded"), ("not-run", "not-run"), ("Act;ive 1", "ctive"), ("", "unknown"), ("A1;", "unknown"), ("a-b", "a-b"), ("x" * 30, "x" * 24), ("ab\ncd", "abcd"), ("caf\u00e9", "caf")):
            code, log, err = self.run_cleanup('state_fact baseline_label_state "$V"', {"V": value}, extra=("state_fact",))
            self.assertEqual(log, "fact baseline_label_state %s\n" % want, (value, err))  # only [a-z-], at most 24 characters, `unknown` for nothing
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        body = driver.split("\nbaseline_clean() {", 1)[1].split("\n}\n", 1)[0]
        self.assertEqual(driver.count('baseline_clean || fail "a product definition, release or mapping already exists on this runner (baseline sub-check: ${BASEFAIL:-unknown})"'), 2)  # probe and lifecycle name it
        self.assertTrue('names=(conf release cache definition legacy-definition curlrc shim)' in body and 'baseline_note "path-${names[i]}" not-run not-run; return 1; }' in body)
        self.assertEqual(re.findall(r"baseline_note (\S+) (\S+)", body)[1:], [("manager-path-unread", "not-run"), ("manager-path-unread", "not-run"), ("command-provenance", "not-run"),
                                                                             ("label-active", '"$la"'), ("legacy-active", '"$la"'), ("hosts", '"$la"'), ("none", '"$la"')])  # after the seven path names
        self.assertTrue(body.index("sgone") < body.index("show-environment") < body.index("launchctl getenv PATH") < body.index("command -v herdr") < body.index("label-active") < body.index("legacy-active") < body.index("hosts"))

    def test_default_target_dependency_needs_a_successful_query_and_an_exact_unit_entry(self):
        label = "herdr-mobile-relay.service"
        listing = lambda *units: "default.target\n" + "".join("  %s\n" % u for u in units)  # `--plain` indents every entry
        # (query status, listing, expected status): the listing is trusted only after the query succeeded, and only an exact entry counts (not a substring, not a prefix)
        for status, text, want in (("0", listing("basic.target", label), 0), ("0", listing(label), 0), ("0", listing("basic.target"), 1), ("0", "", 1),
                                   ("0", listing("x-" + label), 1), ("0", listing(label + ".d"), 1), ("0", listing("herdr-mobile-relay.service.wants"), 1),
                                   ("1", listing(label), 1), ("124", listing(label), 1), ("127", listing(label), 1)):
            code, log, err = self.run_cleanup('as_subject() { printf "%s" "$DEPS"; return "$DEPS_RC"; }; default_target_lists', {"LABEL": label, "DEPS": text, "DEPS_RC": status}, extra=("default_target_lists",))
            self.assertEqual(code, want, (status, text, log, err))
            self.assertEqual("fail cannot list" in log, status != "0", (status, log))  # a failed query is its own failure, not a missing entry
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        cell = driver.split("case_login_relaunch() {", 1)[1].split("\n}\n", 1)[0]
        self.assertNotIn("list-dependencies --plain default.target |", driver)  # no pipeline whose status is grep's
        self.assertTrue('TSTATE=$(as_subject systemctl --user is-active default.target 2> /dev/null) || fail' in cell and "is-active default.target 2> /dev/null || true" not in cell)  # the cell's is-active status is not masked (the probe's `default_target` fact records `inactive` on purpose)
        self.assertTrue('[ "$TSTATE" = active ] && default_target_lists || fail' in cell and "case_end login-relaunch 0 0 login" in cell)

    def test_systemctl_is_active_is_a_state_only_with_its_own_status(self):
        # (stdout, exit status, state): `systemctl is-active` prints the state and exits 0 for active and 3 for inactive; every other pair is an unanswered query (a timeout, a signal,
        # a missing binary, a state printed before a failure), never `active` and never `inactive`.
        pairs = (("active", "0", "active"), ("inactive", "3", "inactive"),
                 ("active", "1", "unknown"), ("active", "3", "unknown"), ("active", "4", "unknown"), ("active", "124", "unknown"), ("active", "127", "unknown"), ("active", "143", "unknown"),
                 ("inactive", "0", "unknown"), ("inactive", "1", "unknown"), ("inactive", "4", "unknown"), ("inactive", "124", "unknown"), ("inactive", "127", "unknown"), ("inactive", "137", "unknown"),
                 ("failed", "3", "unknown"), ("activating", "3", "unknown"), ("", "0", "unknown"), ("", "3", "unknown"), ("active\ninactive", "0", "unknown"))
        stub = 'systemctl() { printf "%s" "$SC_OUT"; return "$SC_RC"; }\nas_subject() { "$@"; }\n'
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        probe = next(l.strip() for l in driver.splitlines() if l.lstrip().startswith("fact default_target") and "unit_state" in l)  # the Linux probe's own observation, run as written
        for out, status, want in pairs:
            env = {"SC_OUT": out, "SC_RC": status, "SUID": "61234"}
            code, log, err = self.run_cleanup(stub + probe, env, extra=("unit_state",))
            self.assertEqual((code, log), (0, "fact default_target %s\n" % want), (out, status, err))  # the recorded fact; only a definite answer can be `active`
            code, log, err = self.run_cleanup(stub + "mgr_down", env, extra=("unit_state",), oneliners=("mgr_down",))
            self.assertEqual(code, 0 if (out, status) == ("inactive", "3") else 1, (out, status, err))  # the manager is down only for `inactive` with its own status
        self.assertFalse([l for l in driver.splitlines() if "is-active" in l and "|| true" in l])  # no is-active status is masked
        self.assertIn('wait_for 20 mgr_down || priv mgr-stop', driver)

    def test_readiness_and_trust_answers_need_a_successful_transfer(self):
        host = self.load_host()
        ready = json.dumps({"status": "ready", "inventory": {"state": "ready"}, "instance": "i", "release_version": "1", "revision": "r", "bundle_hash": "b"})
        digest = host.identity(ready)
        self.assertRegex(digest, "^[0-9a-f]{64}$")
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        lines = driver.splitlines()
        stubs = 'readyz_local() { printf "%s" "$BODY"; return "$BODY_RC"; }\nas_subject() { printf "%s" "$BODY"; return "$BODY_RC"; }\npublic_curl() { printf "%s" "$BODY"; return "$BODY_RC"; }\n'
        names = ("local_ident", "public_ident", "ready_ok", "public_ok", "known_digest")
        # (body, curl status, identity): the identity of a body counts only after curl itself succeeded; anything else is `none`, never empty
        for body, rc, want in ((ready, "0", digest), (ready, "28", "none"), (ready, "1", "none"), (ready, "22", "none"), (ready, "18", "none"), (ready, "124", "none"), (ready, "143", "none"),
                               ("", "0", "none"), ("garbage", "0", "none"), (ready.replace("ready", "starting"), "0", "none")):
            env = {"BODY": body, "BODY_RC": rc, "HOSTN": "x.test", "IDENT": digest}
            for ident in ("local_ident", "public_ident"):
                code, log, err = self.run_cleanup(stubs + 'fact id "$(%s)"' % ident, env, oneliners=names)
                self.assertEqual((code, log), (0, "fact id %s\n" % want), (ident, body[:20], rc, err))
            ok = want != "none"
            self.assertEqual([self.run_cleanup(stubs + call, env, oneliners=names)[0] for call in ("ready_ok", "public_ok")], [0 if ok else 1] * 2, (body[:20], rc))
        for ident in ("none", "", "unknown", "pending"):  # two `none` (or empty) answers are never "the same identity"
            for rc in ("0", "28"):
                self.assertEqual(self.run_cleanup(stubs + "public_ok", {"BODY": "garbage", "BODY_RC": rc, "HOSTN": "x.test", "IDENT": ident}, oneliners=names)[0], 1, (ident, rc))
        # the trusted positive request: the exact body AND a successful transfer
        positive = next(l.strip() for l in lines if "fact trust_positive accepted" in l)
        for body, rc, accepted in (('{"probe":"ok"}', "0", True), ('{"probe":"ok"}', "28", False), ('{"probe":"ok"}', "18", False), ('{"probe":"ok"}', "22", False),
                                   ('{"probe":"ok"}', "124", False), ('{"probe":"no"}', "0", False), ("", "0", False)):
            code, log, err = self.run_cleanup(stubs + positive, {"BODY": body, "BODY_RC": rc})
            self.assertEqual((code, "fact trust_positive accepted" in log, "fact trust_positive refused" in log, "fail" in log), (0 if accepted else 1, accepted, not accepted, not accepted), (body, rc, log, err))
        # the 502 probe of the lifecycle's trusted TLS (the front answers with a complete 502): the status code AND a successful transfer
        at = next(i for i, l in enumerate(lines) if "probe_code=$(" in l)
        reach = "\n".join(lines[at:at + 2]).strip()
        for code_out, rc, accepted in (("502", "0", True), ("502", "28", False), ("502", "18", False), ("502", "7", False), ("200", "0", False), ("", "0", False)):
            code, log, err = self.run_cleanup(stubs + reach, {"BODY": code_out, "BODY_RC": rc, "HOSTN": "x.test"})
            self.assertEqual((code, "fail trusted TLS" in log), (0 if accepted else 1, not accepted), (code_out, rc, log, err))
        self.assertFalse([l for l in lines if "curl" in l and "|| true" in l])  # no curl status is masked

    def test_curl_shim_refuses_redirects_overrides_and_foreign_urls(self):
        host = self.load_host()  # the refusal paths below return before any process or network use
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stderr(io.StringIO()):
            o = {"version": VERSION, "host": "nlt.test", "sha": SHA, "target": "linux_amd64", "ports": "18375", "art": tmp, "log": tmp + "/hits"}
            ready, local = "https://nlt.test/readyz", "http://127.0.0.1:18375/readyz"
            for args in (["-fsSL", local], ["--location", ready], ["--proxy", "http://x", ready], ["--resolve", "nlt.test:443:1.1.1.1", ready], ["-k", ready],
                         ["--cacert", "x", ready], ["-H", "Authorization: x", ready], ["https://example.com/"], ["http://127.0.0.1:9/readyz"], []):
                with self.subTest(args), self.assertRaises(SystemExit) as refused:
                    host.curl_shim(o, args)
                self.assertEqual(refused.exception.code, 2)

    def test_docs_name_every_probe_diagnostic_fact_as_evidence_not_admission(self):
        docs = " ".join(self.text("docs/ci-test-tiers.md").split())
        self.assertTrue(all("`%s`" % name in docs for name in m.DIAG_KEYS))  # every fact the gate validates is named
        for text in ("evidence, not admission", "the pass rule and the failure phase never read them", "cannot turn a failed probe into a pass", "refuses a *passing* receipt whose diagnostics record a failed stage", "`not-run`", "`over-limit`", "at most 64 characters", "`<path>`", "`<x>`", "known launchd words", "a name the producing host knows", "4096 bytes"):
            self.assertIn(text, docs)

    def test_docs_and_lint_list_name_the_workflow_as_partial_q1(self):
        docs = " ".join(self.text("docs/ci-test-tiers.md").split())
        self.assertTrue(all(t in docs for t in ("partial Q1", "[native-lifecycle]", "not M3", "native-capability-v1", "fresh user manager")))
        self.assertIn(".github/workflows/native-manager-lifecycle.yml", self.text(".github/workflows/check.yml"))
        # The runner guard is process-name provenance: the docs and the driver header say it stops casual runs and is not a security boundary, never that it refuses everywhere else.
        for text in (docs, " ".join(self.text("scripts/ci/run-native-lifecycle.sh").split("\n", 3)[1].split())):
            self.assertIn("not a security boundary", text)
            self.assertNotIn("refuses anywhere else", text)


if __name__ == "__main__":
    unittest.main()

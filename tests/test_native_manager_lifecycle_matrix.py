#!/usr/bin/env python3
"""Hermetic tests of the matrix/gate and the static wiring of the native-manager lifecycle workflow: only a fresh temporary directory is written; no manager, sudo, relay or network is touched."""
import ast
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
ANCESTOR_BUNDLE = make_bundle(tempfile.mkdtemp(), m.ANCESTOR)
atexit.register(shutil.rmtree, BUNDLE, True)
atexit.register(shutil.rmtree, ANCESTOR_BUNDLE, True)
PROD = m.producer_receipt(BUNDLE, ANCESTOR_BUNDLE, VERSION, "lifecycle-bundle-%s-%s" % (SHA, ATT), 0, IDENT)


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
    elif ident in m.Q2:
        resolved = m.ANCESTOR if ident == "b-public-fail-rollback" else SHA
        # The final tree digest differs from the one recorded at readiness (the wrapper's `sleep 1` children churn), as on the hosted runners.
        c.update(before=ABSENT, after=ABSENT if ident == "b-full-uninstall" else snap(release_revision=resolved, owned_pid_digest=H("7") if resolved == SHA else H("8")),
                 readiness="none" if ident == "b-full-uninstall" else H("a") if resolved == m.ANCESTOR else H("b"),
                 exit=1 if ident == "b-public-fail-rollback" else 0,
                 q2={"requested": SHA, "resolved": resolved, "cli_sha256": m.CLI[os_name][0], "cli_size": m.CLI[os_name][1],
                     "a_pid": H("5"), "b_pid": H("6"),
                     "a_identity": H("a"), "b_identity": "none" if ident == "b-public-fail-rollback" else H("b"),
                     "public": "a" if ident == "b-public-fail-rollback" else "b", "setup": "none", "rejected": "b" if ident == "b-public-fail-rollback" else "n/a",
                     "a_gone": "yes", "b_gone": "yes" if ident == "b-public-fail-rollback" else "n/a",
                     "registry": "absent" if ident == "b-full-uninstall" else "present"})
    return c


def facts(os_name, **over):
    linux = os_name == "Linux"
    base = {"manager_pre": "available", "manager_post": "available", "trust_negative": "refused", "trust_positive": "accepted", "cleanup": "pass",
            "curl_backend": "curl 8.5 OpenSSL/3.0", "phase": "complete", "isolated_uid": "available" if linux else "not-applicable",
            "default_target": "active" if linux else "not-applicable", "linger_initial": "no" if linux else "not-applicable", "linger_changed": "no" if linux else "not-applicable"}
    base.update({k: DIAG[k] for k in m.BASELINE_PASS} if linux else DIAG)  # the diagnostics the driver writes for a pass: the clean baseline everywhere, the gui stages on macOS only
    return "".join("%s=%s\n" % kv for kv in dict(base, **over).items())


DIAG = {"gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "1234", "gui_print_err": "none", "disabled_status": "0", "disabled_bytes": "321", "disabled_admission": "pass",
        "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none", "baseline_failed": "none", "baseline_label_state": "absent", "baseline_legacy_state": "absent",
        "baseline_label_status": "113", "baseline_label_stdout": "empty", "baseline_label_stderr": "bad-request+not-found"}  # the macOS probe's baseline label shape (Task Z), evidence only
HINT_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 ={}"<>()._:/-'  # the sanitized hint class, written out


def good():
    """artifact directory name -> (file name, document) for one complete, passing run."""
    out = {"native-receipt-producer-%s-%s" % (SHA, ATT): ("producer.json", copy.deepcopy(PROD))}
    for runner, os_name in m.RUNNERS.items():
        suffix = "%s-%s-%s" % (runner, SHA, ATT)
        out["native-receipt-probe-" + suffix] = ("probe.json", m.probe_receipt(facts(os_name), 0, IDENT, os_name, 1001))
        ancestor = PROD["ancestor"]["targets"][m.TARGETS[os_name]]
        q2 = {"ancestor_checksums_sha256": PROD["ancestor"]["checksums_sha256"], "ancestor_archive_sha256": ancestor["archive_sha256"],
              "ancestor_manifest_sha256": ancestor["manifest_sha256"], "cli_sha256": m.CLI[os_name][0], "cli_size": m.CLI[os_name][1]}
        out["native-receipt-cases-" + suffix] = ("cases.json", m.cases_receipt([case(i, os_name) for i in m.case_ids(os_name)], {"candidate": bound(os_name), "q2": q2}, "pass", "none", 0, IDENT, os_name))
    return out


def run_gate(docs, bundle=BUNDLE, ancestor=ANCESTOR_BUNDLE):
    with tempfile.TemporaryDirectory() as tmp:
        for name, (file, doc) in docs.items():
            os.makedirs(os.path.join(tmp, name))
            with open(os.path.join(tmp, name, file), "w") as stream:
                stream.write(doc if isinstance(doc, str) else json.dumps(doc))
        m.gate(tmp, IDENT, bundle, ancestor, VERSION)
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
        self.assertEqual(m.case_ids("Darwin"), common + ("foreign-launchd-path",) + m.Q2)
        self.assertEqual(m.case_ids("Linux"), common + ("foreign-systemd-fragment", "foreign-systemd-dropin") + m.Q2)
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
            "wrong ancestor digest": edit(prod, lambda x: x["ancestor"]["targets"]["linux_amd64"].update(archive_sha256=H("9"))),
            "wrong CLI digest": edit(linux, lambda x: x["q2"].update(cli_sha256=H("9"))),
            "missing Q2": edit(linux, lambda x: x["cases"].pop()),
            "extra Q2": edit(linux, lambda x: x["cases"].append(case("a-to-b-update", "Linux"))),
            "failed Q2": edit(linux, lambda x: x["cases"][-1].update(result="fail")),
            "wrong case CLI digest": edit(linux, lambda x: x["cases"][-1]["q2"].update(cli_sha256=H("9"))),
            "rollback not rejected by front": edit(linux, lambda x: x["cases"][-2]["q2"].update(rejected="n/a")),
            "A child not gone after B": edit(linux, lambda x: x["cases"][-3]["q2"].update(a_gone="no")),
            "stale B readiness identity": edit(linux, lambda x: x["cases"][-3].update(readiness=H("a"), q2=dict(x["cases"][-3]["q2"], b_identity=H("a")))),
            "failed B child never observed": edit(linux, lambda x: x["cases"][-2]["q2"].update(b_pid="none")),
            "A tree still running after B update": edit(linux, lambda x: x["cases"][-3]["after"].update(owned_pid_digest=H("5"))),
            "failed B tree still running after rollback": edit(linux, lambda x: x["cases"][-2]["after"].update(owned_pid_digest=H("6"))),
            "unknown final Q2 process": edit(linux, lambda x: x["cases"][-3]["after"].update(owned_pid_digest="unknown")),
            "failed B child survived": edit(linux, lambda x: x["cases"][-2]["q2"].update(b_gone="no")),
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
        self.assertTrue(set(m.DIAG_KEYS) < set(m.PROBE_KEYS) and len(m.DIAG_KEYS) == 16)
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
        edge += [("baseline_label_status", v, w) for v, w in (("0", 0), ("113", 113), ("255", 255), ("256", "not-run"), ("-1", "not-run"), ("007", "not-run"), ("1.0", "not-run"), ("", "not-run"), ("x", "not-run"))]
        edge += [(k, v, v if v in m.LABEL_STREAMS else "not-run") for k in ("baseline_label_stdout", "baseline_label_stderr") for v in m.LABEL_STREAMS + ("unknown", "Empty", "", "empty ", "bad request", "notfound", "framed ")]
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
        bad.update({k: [256, -1, True, False, 1.5, "1", "", None, [], {}, "unknown"] for k in ("gui_print_status", "disabled_status", "baseline_label_status")})
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
        stages = [(k, v) for k in ("gui_print_status", "disabled_status") for v in ("1", "113", "255", "not-run")]  # (gui_nul is no stage: see the accepted facts below)
        stages += [("disabled_admission", v) for v in m.DIAG_ENUM["disabled_admission"][1:]] + [("disabled_bytes", "65537")]  # (the domain listing's size is no stage: see the accepted sizes below)
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
        for shape in ({k: DIAG[k] for k in m.LABEL_SHAPE}, {"baseline_label_status": "0", "baseline_label_stdout": "framed", "baseline_label_stderr": "empty"}):  # Linux asks no label query: Task Z
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=shape):
                m.check_probe(m.probe_receipt(facts("Linux", **shape), 0, IDENT, "Linux", 1001), IDENT, "Linux")
        # A status alone (F002: no stream beside it is no coherent shape) is no longer kept by the builder: it records `not-run` throughout, which a Linux probe may carry; the same status forged into the
        # receipt is refused as malformed, and a coherent forged shape as a contradiction.
        alone = m.probe_receipt(facts("Linux", baseline_label_status="113"), 0, IDENT, "Linux", 1001)
        self.assertEqual([alone[k] for k in m.LABEL_SHAPE], ["not-run"] * 3)
        m.check_probe(alone, IDENT, "Linux")
        with self.assertRaisesRegex(m.Refusal, "malformed probe diagnostics"):
            m.check_probe(dict(alone, baseline_label_status=113), IDENT, "Linux")
        with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result"):
            m.check_probe(dict(alone, baseline_label_status=113, baseline_label_stdout="empty", baseline_label_stderr="bad-request+not-found"), IDENT, "Linux")
        for ok in ({"gui_bytes": "not-run"}, {"disabled_bytes": "not-run"}, {"gui_bytes": "65536", "disabled_bytes": "65536"},
                   {"gui_bytes": "65537"}, {"gui_bytes": "89044"}, {"gui_bytes": str(10 ** 12)}, {"gui_bytes": "89044", "disabled_bytes": "266"},  # macOS 15's domain listing: 89,044 bytes, read by status only; its size is evidence
                   {"gui_nul": "nul"}, {"gui_nul": "not-run"}, {"gui_nul": "nul", "gui_bytes": "89044"},  # the domain listing is read by status alone: a NUL in it (a diagnostic) fails nothing; it was a stage (Task XR F004)
                   {"gui_print_err": "warn: <x>", "disabled_err": "bad request", "disabled_head": "}", "disabled_tail": "x"},
                   # Task Z: the baseline label's shape is evidence, never a stage: whatever coherent shape the one extra query got (it is not the observation the baseline decides on) fails nothing
                   {"baseline_label_status": "113", "baseline_label_stdout": "bad-request", "baseline_label_stderr": "not-found"},
                   {"baseline_label_status": "0", "baseline_label_stdout": "framed", "baseline_label_stderr": "empty"},
                   {"baseline_label_status": "1", "baseline_label_stdout": "other", "baseline_label_stderr": "other"}, {k: "not-run" for k in m.LABEL_SHAPE}):
            m.check_probe(mac(**ok), IDENT, "Darwin")  # unmeasured sizes, sizes at the bound, a NUL in the listing, any valid hint and any coherent label shape are no failed stage
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
        self.assertTrue(run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", fresh(baseline_label_status="1", baseline_label_stdout="other", baseline_label_stderr="framed"))))))  # a label shape is evidence (Task Z)
        for shape in (dict(baseline_label_status="127", baseline_label_stdout="bad-request", baseline_label_stderr="not-found"), dict(baseline_label_status="126", baseline_label_stdout="empty", baseline_label_stderr="empty"),
                      {k: "not-run" for k in m.LABEL_SHAPE}):  # a launchctl that really exits 126 or 127 (F001), and a query that recorded nothing, are coherent
            self.assertTrue(run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", fresh(**shape))))))
        self.assertTrue(run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", fresh(disabled_head="}", gui_print_err="Could not find domain for user gui: 501"))))))  # hints are evidence, never a failed stage
        for name, edit in {"out-of-range status": lambda x: x.update(gui_print_status=300), "boolean size": lambda x: x.update(gui_bytes=True), "free-text enum": lambda x: x.update(gui_nul="maybe"),
                           "unsanitized hint": lambda x: x.update(disabled_head="a;b"), "encoded path hint": lambda x: x.update(gui_print_err="launchctl error 2FUsers2Frunner2Fsecret"),
                           "host name hint": lambda x: x.update(disabled_err="host john-macbook.local denied"), "overlong hint": lambda x: x.update(disabled_tail="x" * 65), "empty state": lambda x: x.update(baseline_label_state=""),
                           "missing key": lambda x: x.pop("baseline_failed"), "extra key": lambda x: x.update(extra="x"),
                           "string status": lambda x: x.update(disabled_status="0"),
                           "free-text label stream": lambda x: x.update(baseline_label_stderr="Could not find service"), "label status as text": lambda x: x.update(baseline_label_status="113"),
                           "label stream with a uid": lambda x: x.update(baseline_label_stdout="not-found 501"), "label shape half recorded": lambda x: x.update(baseline_label_stdout="not-run"),
                           "label status without streams": lambda x: x.update(baseline_label_status=0, baseline_label_stdout="not-run", baseline_label_stderr="not-run"),
                           "label 113 without streams": lambda x: x.update(baseline_label_status=113, baseline_label_stdout="not-run", baseline_label_stderr="not-run"),
                           "label 127 without streams": lambda x: x.update(baseline_label_status=127, baseline_label_stdout="not-run", baseline_label_stderr="not-run"),
                           "label streams without status": lambda x: x.update(baseline_label_status="not-run")}.items():
            doc = fresh()
            edit(doc)
            with self.subTest(name), self.assertRaises(m.Refusal):
                run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", doc))))
        failed = m.probe_receipt(facts("Darwin", **dict(DIAG, manager_pre="unavailable")), 1, IDENT, "Darwin", 501)  # all-clean diagnostics, a failed probe: the gate still refuses it
        with self.assertRaisesRegex(m.Refusal, r"the capability probe failed \(complete\)"):
            run_gate(mutated(lambda d: d.__setitem__(mac, ("probe.json", failed))))

    def test_baseline_label_shape_facts_are_strict_evidence_never_a_stage_and_linux_records_not_run(self):
        # Task Z: the macOS probe records the exit status and one fixed word per stream of the baseline's label query (never a byte, a label or a uid), as evidence only, like `gui_nul`.
        mac = lambda code=0, **over: m.probe_receipt(facts("Darwin", **dict(DIAG, **over)), code, IDENT, "Darwin", 501)
        linux = lambda **over: m.probe_receipt(facts("Linux", **over), 0, IDENT, "Linux", 1001)
        keys = m.LABEL_SHAPE
        self.assertEqual(keys, ("baseline_label_status", "baseline_label_stdout", "baseline_label_stderr"))
        self.assertEqual(m.LABEL_STREAMS, ("empty", "bad-request", "not-found", "bad-request+not-found", "not-found+bad-request", "framed", "other", "not-run"))
        self.assertTrue(set(keys) <= set(m.DIAG_KEYS) and set(keys) <= set(m.PROBE_KEYS) and m.DIAG_NUMBER["baseline_label_status"] == 255)
        self.assertEqual([m.DIAG_ENUM[k] for k in keys[1:]], [m.LABEL_STREAMS] * 2)
        good = mac()
        self.assertEqual([good[k] for k in keys], [113, "empty", "bad-request+not-found"])
        m.check_probe(good, IDENT, "Darwin")
        # Every coherent shape is kept and accepted, and none changes the verdict (the status is an integer, each stream a fixed word).
        for status in ("0", "1", "113", "126", "127", "255"):  # 126 and 127 included: a launchctl that exits so and wrote an answer is recorded like any other (F001)
            for out in m.LABEL_STREAMS[:-1]:
                for err in m.LABEL_STREAMS[:-1]:
                    doc = mac(baseline_label_status=status, baseline_label_stdout=out, baseline_label_stderr=err)
                    self.assertEqual([doc[k] for k in keys], [int(status), out, err])
                    self.assertEqual((doc["result"], doc["failure_phase"]), ("pass", "none"))
                    m.check_probe(doc, IDENT, "Darwin")
        # A query the helper could not run or had to end records nothing: `not-run` throughout (the helper prints no status for it). `not-run` is the one coherent absence.
        for over in ({k: "not-run" for k in keys},):
            doc = mac(**over)
            self.assertEqual([doc[k] for k in keys], ["not-run"] * 3)
            m.check_probe(doc, IDENT, "Darwin")
        # F002, both directions: a numeric status beside `not-run` streams (the helper cannot print it: a status always has both streams, `empty` for none) and `not-run` beside captured streams are
        # contradictions in a coherent word, and every triple is judged by one rule: all three captured or all three `not-run`.
        statuses = ("not-run", 0, 1, 113, 126, 127, 255)
        for status, out, err in itertools.product(statuses, m.LABEL_STREAMS, m.LABEL_STREAMS):
            doc = {"baseline_label_status": status, "baseline_label_stdout": out, "baseline_label_stderr": err}
            self.assertEqual(m.label_shape_coherent(doc), len({status == "not-run", out == "not-run", err == "not-run"}) == 1, doc)
        for status in (0, 1, 113, 126, 127, 255):  # a status with no stream at all, or with one stream only
            for out, err in (("not-run", "not-run"), ("not-run", "empty"), ("empty", "not-run")):
                over = {"baseline_label_status": str(status), "baseline_label_stdout": out, "baseline_label_stderr": err}
                self.assertFalse(m.label_shape_coherent(dict(over, baseline_label_status=status)), over)
                self.assertEqual([mac(**over)[k] for k in keys], ["not-run"] * 3, over)  # the builder records one observation or none
                m.check_probe(mac(**over), IDENT, "Darwin")
        for out, err in itertools.product(m.LABEL_STREAMS[:-1], m.LABEL_STREAMS[:-1]):  # no status beside captured streams
            over = {"baseline_label_status": "not-run", "baseline_label_stdout": out, "baseline_label_stderr": err}
            self.assertEqual([mac(**over)[k] for k in keys], ["not-run"] * 3, over)
        # The builder records one observation or none: a half-recorded or unrecognised shape becomes `not-run` throughout (a failed probe still yields a valid receipt).
        for over in ({"baseline_label_status": "not-run"}, {"baseline_label_status": "256"}, {"baseline_label_status": "-1"}, {"baseline_label_status": "113 "}, {"baseline_label_stdout": "not-run"},
                     {"baseline_label_stderr": "not-run"}, {"baseline_label_stdout": "maybe"}, {"baseline_label_stderr": "Not-found"}, {"baseline_label_stdout": ""},
                     {"baseline_label_stderr": 'Could not find service "com.herdr-mobile-relay.service" in domain for user gui: 501'}, {"baseline_label_stdout": "/Users/runner/x"},
                     {"baseline_label_stdout": "com.herdr-mobile-relay.service"}):
            doc = mac(**over)
            self.assertEqual([doc[k] for k in keys], ["not-run"] * 3, over)
            self.assertEqual((doc["result"], doc["failure_phase"]), ("pass", "none"))
            m.check_probe(doc, IDENT, "Darwin")
        # A probe that recorded no label shape at all (it died first, or the helper failed) has the same three `not-run`.
        self.assertEqual([m.probe_receipt("", 2, IDENT, "Darwin", 501)[k] for k in keys], ["not-run"] * 3)
        # The gate refuses a forged receipt: a wrong type or range, a word outside the enumeration (a label, a uid, a path or a sentence is not one), a half-recorded shape.
        coherent = dict(zip(keys, (113, "empty", "not-found")))
        forged = [dict(baseline_label_status=v) for v in (256, -1, True, False, 1.5, "113", "", None, [], "not-run ")]
        for k in keys[1:]:
            forged += [{k: v} for v in ("unknown", "", None, 1, True, ["empty"], "Empty", "empty ", "bad request", "framed\n", "Could not find service", "com.herdr-mobile-relay.service", "501", "gui/501", "x" * 65, "/Users/r/x")]
        forged += [dict(baseline_label_status="not-run"), dict(baseline_label_stdout="not-run"), dict(baseline_label_stderr="not-run"), dict(baseline_label_status="not-run", baseline_label_stdout="empty")]
        # F002: the contradictory tuples the gate once accepted, a numeric status with both streams `not-run` (a successful query with no bytes is `empty` `empty`), and `not-run` beside captured streams
        forged += [dict(baseline_label_status=status, baseline_label_stdout="not-run", baseline_label_stderr="not-run") for status in (0, 1, 113, 126, 127, 255)]
        forged += [dict(baseline_label_status="not-run", baseline_label_stdout=out, baseline_label_stderr=err) for out, err in (("empty", "empty"), ("other", "framed"), ("not-found", "bad-request"))]
        for over in forged:
            with self.assertRaisesRegex(m.Refusal, "malformed probe diagnostics", msg=over):
                m.check_probe(dict(mac(**coherent), **over), IDENT, "Darwin")
        for k in keys:  # a missing key and an extra key are refused as any other receipt key is
            with self.assertRaisesRegex(m.Refusal, "wrong keys"):
                m.check_probe({n: v for n, v in good.items() if n != k}, IDENT, "Darwin")
        # The verdict is unchanged by any shape: failing facts fail whatever it says, and a passing probe passes whatever it says (the pass rule never reads it).
        for over in ({"manager_pre": "unavailable"}, {"cleanup": "fail"}, {"trust_positive": "refused"}):
            failing = mac(**dict(over, **coherent))
            self.assertEqual((failing["result"], failing["failure_phase"]), ("fail", "complete"))
            with self.assertRaisesRegex(m.Refusal, "the capability probe failed"):
                m.check_probe(failing, IDENT, "Darwin")
        sample = itertools.product((0, 1), ("available", "unavailable"), ("available", "unknown"), ("pass", "fail"))
        for exit_code, pre, post, cleanup in sample:
            base = {"exit": exit_code, "manager_pre": pre, "manager_post": post, "cleanup": cleanup, "trust_negative": "refused", "trust_positive": "accepted", "isolated_uid": "not-applicable", "default_target": "not-applicable"}
            self.assertEqual({m.probe_passes(dict(base, **shape), "Darwin") for shape in ({}, coherent, {k: "not-run" for k in keys}, dict(coherent, baseline_label_stdout="other"))}, {m.probe_passes(base, "Darwin")})
        # Linux asks no label query: it records `not-run` and any recorded shape contradicts a passing Linux receipt (the macOS-only evidence is no Linux fact).
        bare = linux()
        self.assertEqual([bare[k] for k in keys], ["not-run"] * 3)
        m.check_probe(bare, IDENT, "Linux")
        for over in ({k: DIAG[k] for k in keys}, {"baseline_label_status": "0", "baseline_label_stdout": "other", "baseline_label_stderr": "other"}):
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=over):
                m.check_probe(linux(**over), IDENT, "Linux")
            with self.assertRaisesRegex(m.Refusal, "diagnostics contradict a passing result", msg=over):
                run_gate(mutated(lambda d: d.__setitem__(key("probe"), ("probe.json", linux(**over)))))
        # A status alone is half a shape: the builder drops it (F002), so the Linux receipt carries nothing; forged in afterwards it is malformed, through the gate too.
        self.assertEqual([linux(baseline_label_status="113")[k] for k in keys], ["not-run"] * 3)
        for forged_over in ({"baseline_label_status": 113}, {"baseline_label_status": 0, "baseline_label_stdout": "not-run", "baseline_label_stderr": "not-run"}):
            with self.assertRaisesRegex(m.Refusal, "malformed probe diagnostics", msg=forged_over):
                m.check_probe(dict(bare, **forged_over), IDENT, "Linux")
            with self.assertRaisesRegex(m.Refusal, "malformed probe diagnostics", msg=forged_over):
                run_gate(mutated(lambda d: d.__setitem__(key("probe"), ("probe.json", dict(bare, **forged_over)))))

    def test_producer_receipt_binds_archives_checksums_and_revision(self):
        m.check_producer(PROD, IDENT)
        with tempfile.TemporaryDirectory() as tmp:
            make_bundle(tmp)
            for ident, version in (((SHA[:-1] + "b", RUN, ATT), VERSION), (IDENT, "0.22.5")):  # another revision, another version
                with self.assertRaises(m.Refusal):
                    m.check_producer(m.producer_receipt(tmp, ANCESTOR_BUNDLE, version, "x", 0, ident), ident)
            pathlib.Path(tmp, "checksums.txt").write_text(pathlib.Path(tmp, "checksums.txt").read_text().replace("0", "1", 1))
            with self.assertRaises(m.Refusal):
                m.check_producer(m.producer_receipt(tmp, ANCESTOR_BUNDLE, VERSION, "x", 0, IDENT), IDENT)
            self.assertEqual(m.producer_receipt(tmp + "/missing", ANCESTOR_BUNDLE, VERSION, "x", 0, IDENT)["result"], "fail")

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
        for text in ("name: Lifecycle, Q1 + CLI update/rollback/uninstall", "needs: [decide, probe, producer]",  "needs: [decide, probe, producer, lifecycle]",
                     "if: ${{ always() && needs.decide.outputs.run != 'false' }}", "branches: [ci/tailscale-native-preflight-ef9f843]"):
            self.assertIn(text, flow)
        upload = flow.split("Upload the probe receipt (pass or fail)", 1)[1].split("- name:", 1)[0]
        for text in ("if: always()", "if-no-files-found: error", "retention-days: 1", "native-evidence/probe.json"):
            self.assertIn(text, upload)  # the failure receipt is uploaded, and a missing one is an explicit error
        self.assertEqual(flow.count("run: /bin/bash scripts/ci/run-native-lifecycle.sh"), 3)
        self.assertIn("--ancestor-bundle native-ancestor-bundle", flow)
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

    def test_case_end_without_q2_fields_runs_under_set_u_in_macos_bash(self):
        # Hosted be9758a7 (macOS /bin/bash 3.2): `"${extras[@]}"` of an empty array aborted every Q1 case_end under set -u.
        stubs = ('snap() { echo "{}"; }\ncounts() { echo "{}"; }\nbystander() { echo same; }\nlocal_ident() { echo none; }\n'
                 'fail() { echo "fail $*" >&2; exit 1; }\nmx() { printf "%s\\n" "$@" > "$OUT"; }\n')
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "argv")
            env = {"PATH": "/usr/bin:/bin", "OUT": out, "CASES": "c", "OS": "Darwin", "SHA": SHA, "RUN": "1", "ATT": "1", "CASE": "a-install-start",
                   "BEFORE": "{}", "BY_BASE": "same", "BYP0": "same", "TSTATE": "n/a", "CNT0": "{}", "MID": ""}
            for q2, extra in (('', []), ('{"x":1}', ['--q2', '{"x":1}'])):
                program = "set -euo pipefail\n" + stubs + self.shell_function("case_end") + 'Q2JSON=%s\ncase_end install 0\n' % shlex.quote(q2)
                subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", program], env=env, capture_output=True, text=True, check=True)
                argv = pathlib.Path(out).read_text().splitlines()
                self.assertEqual(argv[-len(extra) - 2:], ["--mid", "n/a"] + extra)

    def test_probe_stage_base_needs_no_artifact_facts_and_refuses_downloads(self):
        # Hosted 667d7aca: probe mode calls stage_base before artifact_facts, and `set -u` aborted on CAND_DIGEST.
        # Run the driver's own stage_base under `set -u` with no artifact facts; the curl shim must name `none` digests.
        stubs = 'on_cleanup() { :; }\nas_subject() { :; }\nfail() { echo "fail $*" >&2; exit 1; }\nstage_put() { :; }\n'
        with tempfile.TemporaryDirectory() as tmp:
            env = {"PATH": "/usr/bin:/bin", "EV": tmp, "OS": "Darwin", "S": "/stage", "SHIMDIR": "/stage/shim", "HOSTPY": "/host.py", "PY": "/py",
                   "VER": "0.0.0", "SHA": SHA, "ANCESTOR": m.ANCESTOR, "TARGET": "darwin_arm64", "HOSTN": "h.test", "RELAY_PORT": "18375"}
            program = "set -euo pipefail\n" + stubs + self.shell_function("stage_base") + "stage_base\n"
            subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", program], env=env, capture_output=True, text=True, check=True)
            shim = (pathlib.Path(tmp) / "shim.curl").read_text()
        for flag in ("--candidate-digest none", "--ancestor-digest none", "--candidate-archive none", "--ancestor-archive none"):
            self.assertIn(flag, shim)

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
            as_subject() { echo "AS $*" >> "$LOG"; case "$*" in *"cd -P"*) printf '%s\\n' "$PHYS" ;; esac; }
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
                      + "".join(self.shell_function(n) for n in ("cl_user", "cl_product", "cl_foreign", "physical_release") + tuple(extra)) + call)
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
        product = {"PRODUCT": "yes", "SHIMDIR": "/shim", "STUB": "", "REL": "/rel", "DEF": "/def", "LDEF": "/ldef", "CONF": "/conf", "THOME": "/h", "VER": VERSION, "SHA": SHA, "PHYS": "/rel/releases/%s-%s-linux-amd64" % (VERSION, SHA)}
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

    def mx_calls(self):
        """Every `mx SUBCOMMAND ...` call of the driver as an argv: continuation lines joined, `$id_args` expanded to the flags the driver defines, each value a placeholder."""
        driver = re.sub(r"\\\n\s*", " ", self.text("scripts/ci/run-native-lifecycle.sh"))
        self.assertIn('${extras[@]+"${extras[@]}"}', driver)  # the bash-3.2-safe expansion of the optional Q2 arguments
        driver = driver.replace('${extras[@]+"${extras[@]}"}', '--q2 "0"')  # the Q2 branch supplies an optional JSON value; Q1 supplies no extra argument
        id_args, calls = re.search(r'id_args="([^"]*)"', driver).group(1), []
        for line in (l for l in driver.splitlines() if not l.lstrip().startswith("#")):
            for found in re.finditer(r"(?<![\w$./-])mx (\S+)(.*)", line):
                lexer = shlex.shlex(found.group(2).replace("$id_args", id_args), posix=True, punctuation_chars=True)
                lexer.whitespace_split = True
                words = itertools.takewhile(lambda w: w[0] not in "|&;<>()", lexer)  # up to `;;`, `||`, ...
                calls.append([found.group(1)] + [w if w.startswith("--") else "0" for w in words])
        return calls

    def test_every_mx_call_in_the_driver_parses_with_the_real_parser(self):
        parser, calls = m.build_parser(), self.mx_calls()

        def refusal(argv):  # argparse's own message, or "" when the real parser accepts the call
            err = io.StringIO()
            with contextlib.redirect_stderr(err):
                try:
                    parser.parse_args(argv)
                except SystemExit:
                    return err.getvalue().strip().splitlines()[-1]
            return ""
        self.assertEqual(sorted(c[0] for c in calls), ["bundle", "case-add", "cases", "probe", "producer"])  # every receipt call is read, none skipped
        self.assertEqual(self.text("scripts/ci/run-native-lifecycle.sh").count('"$MATRIX"'), 1)  # and none reaches the module except through mx
        for call in calls:
            self.assertEqual(refusal(call), "", call)
            for flag in ("--run-id", "--run-attempt", "--sha"):  # non-vacuous: the parser refuses the call with any identity flag dropped, and any unknown flag
                self.assertIn("required", refusal(call[:call.index(flag)] + call[call.index(flag) + 2:]), (call[0], flag))
            self.assertIn("unrecognized", refusal(call + ["--bogus", "0"]), call[0])

    def test_no_driver_line_runs_the_full_uninstall_through_the_current_symlink(self):
        code = [l for l in self.text("scripts/ci/run-native-lifecycle.sh").splitlines() if not l.lstrip().startswith("#")]
        self.assertEqual([l for l in code if "current/relay/uninstall.sh" in l], [])  # the native remover refuses a symlinked ancestor; the product's own entrypoint never uses the link

    def test_the_physical_release_resolver_yields_the_physical_path_and_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            top, name = os.path.realpath(tmp), "%s-%s-linux-amd64" % (VERSION, SHA)
            rel, other = top + "/rel", "%s-%s-linux-amd64" % (VERSION, "b" * 40)
            for d in (name + "/relay", other, "9.9.9-%s-linux" % SHA):
                os.makedirs("%s/releases/%s" % (rel, d))
            os.makedirs(top + "/outside")
            os.symlink(top + "/outside", "%s/releases/%s-link" % (rel, name))  # named for this revision, but it points outside releases/
            os.symlink(rel, top + "/alias")

            def resolve(target, root=rel):
                if os.path.lexists(rel + "/current"):
                    os.remove(rel + "/current")
                if target:
                    os.symlink(target, rel + "/current")
                code, log, err = self.run_cleanup('as_subject() { "$@"; }\np=$(physical_release); rc=$?; echo "phys=$p" >> "$LOG"; exit $rc', {"REL": root, "VER": VERSION, "SHA": SHA})
                return code, log.strip()
            physical = (0, "phys=%s/releases/%s" % (rel, name))
            self.assertEqual(resolve("releases/" + name), physical)  # a relative link and an absolute one both yield the physical directory, never the link
            self.assertEqual(resolve("%s/releases/%s" % (rel, name)), physical)
            for target in (name + "-link", "releases/" + name + "-link", "releases/" + name + "/relay", "releases/" + other, "releases/9.9.9-%s-linux" % SHA, "releases", "../outside", "releases/missing", None):
                self.assertEqual(resolve(target), (1, "phys="), target)  # outside releases/, nested, another revision or version, the root itself, dangling, absent
            self.assertEqual(resolve("releases/" + name, top + "/alias"), (1, "phys="))  # a release root reached through a symlink is not the owned physical root

    def test_every_full_uninstall_site_runs_from_the_physical_release_and_fails_closed(self):
        name = "%s-%s-linux-amd64" % (VERSION, SHA)
        absent = json.dumps({"active": "absent", "origin": "absent", "definition_hash": "absent"})
        env = {"REL": "/rel", "VER": VERSION, "SHA": SHA, "PRODUCT": "yes", "SHIMDIR": "/shim", "STUB": "", "DEF": "/def", "LDEF": "/ldef", "CONF": "/conf", "THOME": "/h",
               "SNAP1": absent, "SNAP2": absent, "EXISTS": "yes", "EXISTS_AFTER": "no", "PHYS": "/rel/releases/" + name}
        stubs = 'case_begin() { CASE=$1; }\ncase_end() { echo "case_end $*" >> "$LOG"; }\nstage_put() { echo "stage_put $*" >> "$LOG"; }\nsnap_of() { echo "$SNAP1"; }\nprocs_gone() { :; }\nEV=$HOME\n'
        extra = ("expect", "residue_gone", "case_full_uninstall", "teardown_refusal_base")
        for call in ("cl_product", "case_full_uninstall", "teardown_refusal_base"):  # the real driver functions, every effect a logging stub
            code, log, err = self.run_cleanup(stubs + call, env, extra=extra)
            self.assertEqual(code, 0, (call, log, err))
            self.assertEqual(re.findall(r"(?m)^RUNB .*", log), ["RUNB 300 /rel/releases/%s/relay/uninstall.sh" % name], call)  # exactly one uninstall, from the physical directory
            self.assertNotIn("/current/", log)
            self.assertEqual(call == "cl_product", 'AS /bin/sh -c [ -x "$1" ] _ /rel/releases/%s/relay/uninstall.sh' % name in log, call)  # the executable check uses it too
            for bad in ("", "/rel/current", "/rel/releases", "/rel/releases/%s/relay" % name, "/rel/releases/9.9.9-%s-linux" % SHA, "/rel/releases/%s-%s-linux" % (VERSION, "b" * 40), "/elsewhere/releases/" + name, "/rel/other/" + name):
                code, log, err = self.run_cleanup(stubs + call, dict(env, PHYS=bad), extra=extra)
                self.assertEqual(code, 1, (call, bad, log, err))
                self.assertFalse(any(word in log for word in ("RUNB", "stage_put", "case_end")), (call, bad, log))  # nothing runs, no shim is staged, no case is recorded as passed

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
                                      (" 4242\n", (1, "", ""), "none"), (" 4242\n", (0, "", ""), "none"),
                                      (" 4242\n", (1, "", "permission denied"), "unknown")):  # both no-match statuses are valid; stderr is not
            with unittest.mock.patch.object(host, "sh", side_effect=self.ps_stub((0, listing, ""), direct)):
                self.assertEqual(host.tree_digest(4242), want, (listing, direct))
        with unittest.mock.patch.object(host, "sh", side_effect=self.ps_stub((0, row(4242, 1) + row(4243, 4242), ""))):
            self.assertEqual(host.tree_digest(4242, pids=True), "4242,4243")

    def test_old_manager_and_child_pid_queries_fail_closed(self):
        host = self.load_host()
        for response, expected in (((0, "", ""), 0), ((1, "", ""), 0), ((0, "4242\n", ""), 1),
                                   ((127, "", ""), 1), ((1, "", "denied"), 1)):
            with unittest.mock.patch.object(host, "sh", return_value=response) as query:
                self.assertEqual(host.pids_gone({"pids": "4242,4243"}, []), expected, response)
                self.assertEqual(query.call_count, 2 if expected == 0 else 1)
        for invalid in ("", "none", "4242,", "0,4243", "4242; echo bad"):
            with unittest.mock.patch.object(host, "sh") as query:
                self.assertEqual(host.pids_gone({"pids": invalid}, []), 1)
                query.assert_not_called()

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
        # F001: sh_kept tells sh's own outcomes (kept False: over the bound, could not run) from a program that really exits 126 or 127 (kept True, with the streams it wrote); sh is its tail, unchanged
        for argv, kwargs, want in ((["/bin/echo", "abcdef"], {"limit": 3}, (False, 126, "", "")), (["/nonexistent/x"], {}, (False, 127, "", "")), (["/bin/echo", "abc"], {"limit": 100}, (True, 0, "abc\n", "")),
                                   (["/bin/sh", "-c", "exit 126"], {}, (True, 126, "", "")), (["/bin/sh", "-c", "exit 127"], {}, (True, 127, "", "")),
                                   (["/bin/sh", "-c", "printf 'x\\n'; echo y >&2; exit 126"], {}, (True, 126, "x\n", "y\n")), (["/bin/sh", "-c", "printf 'x\\n'; echo y >&2; exit 127"], {}, (True, 127, "x\n", "y\n")),
                                   (["/bin/sh", "-c", "exit 3"], {}, (True, 3, "", "")), (["/bin/sh", "-c", "kill -9 $$"], {}, (True, -9, "", ""))):
            self.assertEqual(host.sh_kept(argv, **kwargs), want, argv)
            self.assertEqual(host.sh(argv, **kwargs), want[1:], argv)
        self.assertEqual(host.sh_kept([sys.executable, "-c", "import time; time.sleep(30)"], timeout=1), (False, 127, "", ""))  # a timeout is sh's own outcome too
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

    @staticmethod
    def product_launchd(observer, table, table_rc=0, domain=b"gui/501 = {\n}\n", domain_rc=0, label="com.herdr-mobile-relay.service", answer=None, answer_rc=0, answer_err=None):
        """What either product observer reads from one launchd answer set, `launchctl` a function stub serving these bytes and statuses: `install` is native_install_observe
        (relay/native-install-transaction.sh), `removal` service_removal_observe (relay/common.sh), both sourced (functions only). The label is loaded (`print gui/<uid>/<label>` answers its header, or the bytes of `answer` on stdout, then those of `answer_err` on stderr, with status `answer_rc`).
        Returns `state:enabled`, or `unknown:unknown` when the observer fails."""
        script = textwrap.dedent("""\
            set +e
            launchctl() { case "$1" in print) case "$2" in gui/*/*) if [ -n "$ANSWER_FILE" ]; then cat "$ANSWER_FILE"; [ -z "$ANSWER_ERR_FILE" ] || cat "$ANSWER_ERR_FILE" >&2; return "$ANSWER_RC"; else printf '%s = {\\n}' "$2"; fi ;; *) cat "$DOMAIN_FILE"; return "$DOMAIN_RC" ;; esac ;; print-disabled) cat "$TABLE_FILE"; return "$TABLE_RC" ;; esac; }
            if [ "$OBSERVER" = install ]; then
                native_manager=launchd; source relay/native-install-transaction.sh
                native_install_observe "$LABEL" && echo "$NATIVE_STATE:$NATIVE_ENABLED" || echo unknown:unknown
            else
                source relay/common.sh
                service_removal_observe launchd "$LABEL" && echo "$SERVICE_REMOVAL_STATE:$SERVICE_REMOVAL_ENABLED" || echo unknown:unknown
            fi
        """)
        with tempfile.TemporaryDirectory() as tmp:  # the answers go through files: an environment variable cannot carry a NUL byte
            pathlib.Path(tmp, "table").write_bytes(table if isinstance(table, bytes) else table.encode())
            pathlib.Path(tmp, "domain").write_bytes(domain)
            if answer is not None:
                pathlib.Path(tmp, "answer").write_bytes(answer if isinstance(answer, bytes) else answer.encode())
            if answer_err is not None:
                pathlib.Path(tmp, "answer-err").write_bytes(answer_err.encode())
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True, cwd=ROOT,
                                  env={"PATH": "/usr/bin:/bin", "HOME": tmp, "OBSERVER": observer, "TABLE_FILE": tmp + "/table", "TABLE_RC": str(table_rc),
                                       "DOMAIN_FILE": tmp + "/domain", "DOMAIN_RC": str(domain_rc), "LABEL": label,
                                       "ANSWER_FILE": tmp + "/answer" if answer is not None else "", "ANSWER_RC": str(answer_rc),
                                       "ANSWER_ERR_FILE": tmp + "/answer-err" if answer_err is not None else ""})
        return done.stdout.strip().splitlines()[-1] if done.stdout.strip() else "error:" + done.stderr[:200]

    def removal_enabled(self, code, text, label):
        """What service_removal_observe reads from the same table: the real removal label stands in for `label` (it refuses any other), everything else is the product_enabled drive."""
        flag = self.product_launchd("removal", text.replace(label, "com.herdr-mobile-relay.service"), code).partition(":")[2]
        return flag if flag in ("true", "false") else "unknown"

    def test_launchd_enabled_needs_a_complete_well_formed_disabled_table(self):
        host = self.load_host()
        table = lambda *entries: "disabled services = {\n" + "".join("\t%s\n" % e for e in entries) + "}\n"
        modern = lambda *entries: "\tdisabled services = {\n" + "".join("\t\t%s\n" % e for e in entries) + "\t}\n\n\tlogin item associations = {\n\t}\n"  # macOS 13+: indented, with a trailing section
        # The product's grammar (native_install_observe, relay/native-install-transaction.sh, and service_removal_observe, relay/common.sh: identical): blank lines and the blanks around a line are
        # ignored; a line naming the label is `"label" => true|disabled` (disabled) or `"label" => false|enabled` (enabled) and anything else naming it fails; further sections follow the table.
        # Before Task X the aliases `enabled`/`disabled`, a leading or trailing blank and a trailing section were unknown; those expectations are exactly what the grammar changed (marked below).
        for code, text, want in ((0, table(), "true"), (0, table('"com.x" => false'), "true"), (0, table('"com.x" => true'), "false"),  # complete: an unlisted label is enabled
                                 (0, table('"com.x" => disabled', '"com.y" => true'), "false"), (0, table('"com.x" => enabled'), "true"), (0, table('"com.x" => disabled'), "false"),  # keyword states (were unknown)
                                 (0, table('"com.y" => enabled', '"com.x" => true'), "false"), (0, table('"com.y" => disabled', '"com.x" => false'), "true"),  # other labels' lines are judged only by their shape
                                 (0, table('"com.x" => 1'), "unknown"), (0, table('"com.x" => '), "unknown"), (0, table('"com.x" => true '), "false"), (0, table('"com.x" => false;'), "unknown"),  # a trailing blank is ignored (was unknown)
                                 (0, table('"com.x" => TRUE'), "unknown"), (0, table('"com.x"=>true'), "unknown"), (0, table('"com.x" => true\r'), "false"), (0, table('"com.x" => true\x0b'), "false"),  # CR and VT are blanks (were unknown)
                                 (0, table('"com.x" => true', '"com.x" => enabled'), "unknown"), (0, table('"com.xy" => true'), "true"),
                                 # The framing: blank lines and blanks around lines are ignored (they were each unknown), text before the header is not
                                 (0, " " + table(), "true"), (0, "\n" + table(), "true"), (0, "\t" + table(), "true"), (0, "x" + table(), "unknown"),  # blanks before the header; text before it
                                 (0, table().rstrip("\n") + " ", "true"), (0, table() + " ", "true"), (0, table() + "\t\n", "true"),  # blanks after the closing brace
                                 (0, table().replace("\n", "\r\n"), "true"), (0, table('"com.x" => true').replace("}\n", "\x00}\n"), "unknown"),  # CRLF (CR is a blank), an embedded NUL
                                 (0, table('"com.x" => true') + "\n\n\n", "false"), (0, "disabled services = {}\n", "true"),
                                 (0, "disabled services = {junk\n}\n", "unknown"),  # the header is exactly `disabled services = {` or `disabled services = {}` (the product once admitted this)
                                 (0, table('"com.x" => true') + "}", "unknown"), (0, "disabled services = {\n\t\"com.x\" => true\n}\nextra\n}", "unknown"),  # stray text after the table (the product once admitted these)
                                 (0, "", "unknown"), (0, 'disabled services = {\n\t"com.y" => true\n', "unknown"),  # truncated: no closing brace
                                 (0, "something else\n}\n", "unknown"), (0, table('"com.x" => maybe'), "unknown"), (0, table('"com.x" => true', '"com.x" => false'), "unknown"),
                                 (126, table('"com.x" => true'), "unknown"), (1, table(), "unknown"),
                                 # Modern format (macOS 13+): keyword states, an indented header, a trailing login-item section
                                 (0, modern(), "true"), (0, modern('"com.x" => disabled'), "false"), (0, modern('"com.x" => enabled'), "true"),
                                 (0, modern('"com.y" => disabled', '"com.x" => enabled'), "true"), (0, modern('"com.y" => enabled', '"com.x" => disabled'), "false"),
                                 (0, modern('"com.x" => true'), "false"), (0, modern('"com.x" => false'), "true"),  # the legacy states inside the modern layout
                                 (0, modern('"com.x" => disabled', '"com.x" => enabled'), "unknown"), (0, modern('"com.x" => true', '"com.x" => disabled'), "unknown"),  # the label twice
                                 (0, modern('"com.x" => Disabled'), "unknown"), (0, modern('"com.x" => disabled;'), "unknown"), (0, modern('"com.x" => maybe'), "unknown"),
                                 (0, modern('"com.x" => disabled').replace("\t\t", "\t\t\t"), "false"), (0, modern('"com.x" => disabled').replace("\n", "\r\n"), "false"),  # deeper indentation, CRLF
                                 (0, modern('"com.x" => disabled').replace("\x09", "\x00", 1), "unknown"),
                                 # Inline empty forms, and further sections whose content is not interpreted
                                 (0, "disabled services = {}\n\nlogin item associations = {}\n", "true"), (0, "\tdisabled services = {}\n\n\tlogin item associations = {}\n", "true"),
                                 (0, table('"com.x" => disabled') + "\nlogin item associations = {\n\t\"com.x\" => 1\n\tjunk\n\n}\n", "false"),
                                 (0, table('"com.x" => disabled') + "a = {}\nb = {\n}\nc d = {\nx\n}\n", "false"), (0, "disabled services = {}\nlogin item associations = {\n\"com.x\" => disabled\n}\n", "true"),
                                 # Malformed: an unclosed or stray section, a second table (which could contradict the first), no header first, an unnamed or oddly named section, a row naming the label inside another
                                 (0, table('"com.x" => disabled') + "login item associations = {\n", "unknown"), (0, "disabled services = {}\n}\n", "unknown"),
                                 (0, "disabled services = {}\nstray\n", "unknown"), (0, table() + "stray text\n", "unknown"), (0, table() + "login item associations = {}\n}\n", "unknown"),
                                 (0, table('"com.x" => disabled') + "disabled services = {\n}\n", "unknown"), (0, "disabled services = {}\ndisabled services = {}\n", "unknown"),
                                 (0, "disabled services = {}\ndisabled services  = {\n}\n", "unknown"), (0, "login item associations = {}\n" + table(), "unknown"),
                                 (0, "\n\n  \t\n", "unknown"), (0, "disabled services = { }\n", "unknown"), (0, "disabled services = {\n\t\"com.x\" => disabled\n", "unknown"),
                                 (0, table() + "= {}\n", "unknown"), (0, table() + 'a"b = {}\n', "unknown"), (0, table() + "a = b = {\n}\n", "unknown"), (0, table() + "a={}\n", "unknown"),
                                 (0, table('"x" => true "com.x" => disabled'), "unknown"), (0, table('"com.x" => disabled "x" => true'), "unknown"), (0, table('stray'), "unknown"),
                                 # a row of ANOTHER label is a row only if its name holds no quote (an interior quote is no `"<label>" => <state>`): it must not be skipped as a well-formed row
                                 (0, table('"other"junk" => enabled'), "unknown"), (0, table('"other"junk" => disabled', '"com.x" => disabled'), "unknown"), (0, table('"a" => b" => true'), "unknown"),
                                 (0, table('"a"b" => true'), "unknown"), (0, table('"a"" => false'), "unknown"), (0, table('""a" => false'), "unknown"), (0, modern('"x"y" => enabled'), "unknown"),
                                 (0, table('"a => b" => true', '"com.x" => disabled'), "false"), (0, table('"" => enabled'), "true"), (0, table('"a b" => disabled'), "true"),  # ...but a name may hold ` => `, be empty or hold blanks
                                 (0, "disabled services = {\n\t\"com.x\" => disabled\n}\n" + "x" * 3, "unknown")):
            self.assertEqual(host.disabled_flag(code, text, "com.x"), want, (code, text))
            self.assertEqual(self.product_enabled(code, text, "com.x"), want, (code, text))  # native_install_observe reads every one of these tables the same way
            self.assertEqual(self.removal_enabled(code, text, "com.x"), want, (code, text))  # and so does service_removal_observe
        path = (0, "gui/%d/x.service = {\n\tpath = /p\n}\n" % os.getuid(), "")
        self.assertEqual(self.observed("launchd", print=path, **{"print-disabled": (126, "", "")})["enabled"], "unknown")  # output over the limit
        self.assertEqual(self.observed("launchd", print=path, **{"print-disabled": (0, table(), "")})["enabled"], "true")
        self.assertEqual(self.observed("launchd", print=path, **{"print-disabled": (0, modern('"x.service" => disabled'), "")})["enabled"], "false")
        for kind in ("true", "disabled"):  # the helper's two states for a disabled label, both observers
            self.assertEqual({self.product_launchd(o, modern('"com.herdr-mobile-relay.service" => ' + kind)) for o in ("install", "removal")}, {"loaded:false"})
        for kind in ("false", "enabled"):
            self.assertEqual({self.product_launchd(o, modern('"com.herdr-mobile-relay.service" => ' + kind)) for o in ("install", "removal")}, {"loaded:true"})
        self.assertEqual({self.product_launchd(o, "disabled services = {}\n") for o in ("install", "removal")}, {"loaded:true"})  # the inline empty form: an unlisted label is enabled

    def test_both_product_observers_read_the_domain_by_status_alone(self):
        # The domain listing's text is never read (macOS 15 prints 89,044 bytes of it, past the 65,536-byte capture bound): only its STATUS counts, in native_install_observe and in service_removal_observe.
        modern_table = "\tdisabled services = {\n\t\t\"com.herdr-mobile-relay.service\" => disabled\n\t}\n\n\tlogin item associations = {\n\t}\n"
        listing = b"gui/501 = {\n" + b"".join(b"\t\t%05d\t-\tcom.example.agent%05d\n" % (i, i) for i in range(2700)) + b"}\n"  # 89,114 bytes, the size macOS 15 reported (89,044)
        self.assertGreater(len(listing), 65536)
        for observer in ("install", "removal"):
            for domain, rc, want in ((listing, 0, "loaded:false"), (listing[:65536], 0, "loaded:false"), (listing[:65537], 0, "loaded:false"), (b"", 0, "loaded:false"), (b"x" * 100000, 0, "loaded:false"),
                                     (b"wrong-domain = {}\n", 0, "loaded:false"), (b"gui/501 = {\n\x00}\n", 0, "loaded:false"), (b"\x00" + b"x" * 70000, 0, "loaded:false"),  # whatever it prints, the status is the answer
                                     (listing, 1, "unknown:unknown"), (b"", 1, "unknown:unknown"), (b"gui/501 = {}\n", 113, "unknown:unknown"), (listing, 126, "unknown:unknown"),  # a failing query is unknown, never absence
                                     (b"gui/501 = {}\n", 127, "unknown:unknown"), (b"gui/501 = {}\n", 255, "unknown:unknown")):
                self.assertEqual(self.product_launchd(observer, modern_table, domain=domain, domain_rc=rc), want, (observer, len(domain), rc))
            # the table keeps its own bound and NUL refusal, whatever the domain answered
            for table, rc in ((modern_table + " " * 65536, 0), (modern_table.replace("services", "serv\x00ices"), 0), (modern_table, 1), (modern_table, 126)):
                self.assertEqual(self.product_launchd(observer, table, table_rc=rc, domain=listing), "unknown:unknown", (observer, len(table), rc))
            self.assertEqual(self.product_launchd(observer, modern_table + " " * (65536 - len(modern_table.encode())), domain=listing), "loaded:false", observer)  # exactly the bound

    def test_both_product_observers_carry_the_same_domain_check_and_disabled_grammar_text(self):
        # The two observers are kept textually parallel (the grammar is named once, at native_install_observe): the domain status check and the whole table loop, byte for byte but for the
        # variable that holds the answer ($disabled there, $output in common.sh). Drift in either file fails here.
        install, removal = (self.text(path) for path in ("relay/native-install-transaction.sh", "relay/common.sh"))
        loop = lambda source, variable: ("    enabled=true\n    count=0\n    while IFS= read -r line; do\n" + source.split("    enabled=true\n    count=0\n    while IFS= read -r line; do\n", 1)[1]
                                         .split('    done <<< "$%s"\n' % variable, 1)[0] + '    done <<< "$%s"\n' % variable).replace("$" + variable, "$ANSWER")
        self.assertEqual(loop(install, "disabled"), loop(removal, "output"))
        self.assertIn('            0) case "$line" in\n                   \'disabled services = {\') state=1 ;;', loop(install, "disabled"))
        domain = '    LC_ALL=C launchctl print "$domain" >/dev/null 2>&1 || return 1\n'
        self.assertEqual((install.count(domain), removal.count(domain)), (1, 1))
        frame = '''case "$output" in "$domain/$label = {"$'\\n'*'}'|"$domain/$label = {}") ;; *) return 1 ;; esac\n'''  # the label answer's framing (Task X F003): the same text, in both
        self.assertEqual((install.count(frame), removal.count(frame)), (1, 1))
        for source in (install, removal):  # the closing check, and no capture of the domain listing under the byte bound any more
            self.assertEqual(source.count('    [ "$state" -eq 2 ] && [ "$count" -le 1 ] || return 1\n'), 1)
            self.assertEqual(len(re.findall(r'launchctl print "\$domain"', source)), 1)

    def test_a_loaded_label_answer_must_be_closed_by_its_brace_in_the_helper_and_both_product_observers(self):
        # Task X F003: a status-0 `launchctl print gui/<uid>/<label>` answer is loaded only as `<header> = {`, LF, a body and the closing `}`, or as the inline `<header> = {}`; an answer cut before
        # its brace is unknown. service_removal_observe and the helper framed it so; native_install_observe admitted any header and body, however cut (an answer with no closing brace was "loaded").
        uid, label = os.getuid(), "com.herdr-mobile-relay.service"
        header = lambda name: "gui/%d/%s = {" % (uid, name)
        for tail, want in (("\n}\n", "loaded"), ("\n}", "loaded"), ("\n\tpath = /p\n\tstate = running\n}\n", "loaded"), ("}\n", "loaded"), ("}", "loaded"),  # closed, or the inline empty form
                           ("\n\tpath = /p\n\tstate = running\n", "unknown"), ("\n\tstate = running\n", "unknown"), ("\n\tpath = /p\n", "unknown"), ("\n", "unknown"), ("", "unknown"),  # cut before the brace
                           ("\n}\n\n", "unknown"), ("\n\tstate = running\n}\n\n\n", "unknown"), ("}\n\n", "unknown"),  # a second trailing LF (Task XR F001: one is dropped, by both observers and the helper)
                           ("\n\tstate = running\n} x\n", "unknown"), ("\n\tstate = running\n\x00}\n", "unknown"), ("\n\tstate = running\n}\x00", "unknown")):  # stray text after it; a NUL
            seen = {observer: self.product_launchd(observer, "disabled services = {}\n", answer=header(label) + tail) for observer in ("install", "removal")}
            self.assertEqual(seen, {observer: "loaded:true" if want == "loaded" else "unknown:unknown" for observer in seen}, repr(tail))
            self.assertEqual(self.observed("launchd", print=(0, header("x.service") + tail, ""))["active"], want, repr(tail))  # the CI helper frames the answer the same way

    def test_not_found_is_exact_after_one_lf_and_stderr_spoils_a_loaded_answer_in_the_helper_and_both_product_observers(self):
        # Task XR F001: ONE terminating LF of a label answer is dropped, by native_install_observe as by service_removal_observe and the helper (install dropped every trailing LF: `<not-found>\\n\\n` was
        # absence there, unknown in removal). Task XR F002: the product merges stderr into the label answer (`2>&1`); the helper keeps the two streams apart, so ANY stderr text on a status-0 answer leaves it
        # unframed (unknown), never loaded where the product refuses the merged text.
        uid, label = os.getuid(), "com.herdr-mobile-relay.service"
        missing = lambda name: 'Could not find service "%s" in domain for user gui: %d' % (name, uid)
        table = "disabled services = {}\n"
        for tail, code, want in (("", 113, "absent"), ("\n", 113, "absent"), ("\n\n", 113, "unknown"), ("\n\n\n", 1, "unknown"), (" \n", 113, "unknown"), ("\r\n", 113, "unknown"),  # exact, or one LF; no more
                                 ("\n", 0, "unknown"), ("\n", 1, "absent")):  # a status-0 diagnostic is no frame; any nonzero status admits the exact text
            seen = {observer: self.product_launchd(observer, table, answer=missing(label) + tail, answer_rc=code) for observer in ("install", "removal")}
            self.assertEqual(seen, {observer: ("absent:true" if want == "absent" else "unknown:unknown") for observer in seen}, (tail, code))
            self.assertEqual(self.observed("launchd", print=(code, missing("x.service") + tail, ""))["active"], want, (tail, code))
        closed = lambda name: "gui/%d/%s = {\n\tpath = /p\n\tstate = running\n}\n" % (uid, name)
        for warning, want in ((None, "loaded"), ("warning\n", "unknown"), ("\n", "unknown"), ("x", "unknown"), ("warning: unrelated\nsecond line\n", "unknown")):
            seen = {observer: self.product_launchd(observer, table, answer=closed(label), answer_err=warning) for observer in ("install", "removal")}
            self.assertEqual(seen, {observer: ("loaded:true" if want == "loaded" else "unknown:unknown") for observer in seen}, warning)
            helper = self.observed("launchd", print=(0, closed("x.service"), "" if warning is None else warning))
            self.assertEqual((helper["active"], helper["origin"]), (want, "/p" if want == "loaded" else "unknown"), warning)
        # Task XR F003 (turn 3): the helper reads two pipes and cannot know the order of the product's merged `2>&1` text. The exact diagnostic is absence only when ONE stream carries all of it: split across the
        # streams in either order, or with any text on the other stream, it is unknown (a merged text that the product might read as absent, or refuse, is not proven by the helper), never absence.
        diag = missing("x.service")
        for cut in range(1, len(diag)):
            for out, err in ((diag[:cut], diag[cut:]), (diag[cut:], diag[:cut])):
                seen = self.observed("launchd", print=(1, out, err))
                self.assertEqual((seen["active"], seen["origin"], seen["owned_pid_digest"]), ("unknown",) * 3, (out, err))
        for out, err, want in ((diag, "", "absent"), ("", diag, "absent"), (diag + "\n", "", "absent"), ("", diag + "\n", "absent"), (diag, "\n", "unknown"), ("\n", diag, "unknown"),
                               (diag, "warning\n", "unknown"), ("warning\n", diag, "unknown"), ("", "", "unknown"), (diag + "\n", "\n", "unknown")):
            self.assertEqual(self.observed("launchd", print=(1, out, err))["active"], want, (out, err))

    # Task Z: modern launchctl answers a label that is not loaded with TWO lines (`Bad request.`, then the diagnostic), status 113 (hosted macos-15 run 39664354384: the baseline label was `unknown`).
    # <b> is `Bad request.` and <d> the exact diagnostic of the observed label and uid; the product reads the merged text (`2>&1`) after dropping ONE terminating LF.
    NOT_FOUND_ACCEPTED = ("{d}", "{d}\n", "{b}\n{d}", "{b}\n{d}\n", "{d}\n{b}", "{d}\n{b}\n")
    NOT_FOUND_REFUSED = (
        "{b}", "{b}\n", "", "\n", "\n\n",  # the other line alone, nothing at all
        "{b}\n\n{d}", "{b}\n\n{d}\n", "{d}\n\n{b}", "{d}\n\n{b}\n",  # a blank line between the two
        "{b}\n{b}\n{d}", "{b}\n{d}\n{d}", "{b}\n{d}\n{b}", "{d}\n{b}\n{d}", "{d}\n{d}", "{d}\n{d}\n{b}", "{b}\n{b}",  # a repeated line
        "{b}\r\n{d}", "{b}\n{d}\r", "{b}\r\n{d}\r\n", "{d}\r\n{b}", "{d}\n{b}\r", "{b}\r{d}",  # a CR
        "{b} {d}", "{b}{d}", "{d} {b}", "{d}{b}", "{b}\n{d} {b}",  # the two on one line
        "bad request.\n{d}", "BAD REQUEST.\n{d}", "Bad Request.\n{d}", "{b}\n{dlow}", "{dlow}", "Bad request\n{d}", "Bad request..\n{d}", "{b}.\n{d}",  # case and spelling
        "{b}\n{dl}", "{dl}\n{b}", "{b}\n{du}", "{du}\n{b}", "{b}\n{dc}", "{dc}",  # another label, another uid, a cut diagnostic
        "{b}\n{d}\nextra", "extra\n{b}\n{d}", "{b}\nextra\n{d}", "{b}\n{d} ", " {b}\n{d}", "{b} \n{d}", "{b}\n {d}", "\n{b}\n{d}", "{d}\n{b}\nextra",  # extra text or blanks
        "{b}\n{d}\n\n", "{d}\n{b}\n\n", "{b}\n{d}\n\n\n", "{d}\n\n")  # a second trailing LF

    @classmethod
    def not_found(cls, template, name, uid=None):
        uid = os.getuid() if uid is None else uid
        diag = lambda who, user: 'Could not find service "%s" in domain for user gui: %d' % (who, user)
        return template.format(b="Bad request.", d=diag(name, uid), dl=diag("com.example.other", uid), du=diag(name, uid + 1), dc=diag(name, uid)[:-1], dlow=diag(name, uid).lower())

    def test_modern_two_line_not_found_is_absent_in_both_product_observers_and_only_one_stream_in_the_helper(self):
        label, helper_label, table = "com.herdr-mobile-relay.service", "x.service", "disabled services = {}\n"
        both = lambda *a, **k: {observer: self.product_launchd(observer, table, *a, **k) for observer in ("install", "removal")}
        absent, unknown = {o: "absent:true" for o in ("install", "removal")}, {o: "unknown:unknown" for o in ("install", "removal")}
        seen = lambda out, err, code=113: self.observed("launchd", print=(code, out, err))
        for template in self.NOT_FOUND_ACCEPTED:
            text, hosted = self.not_found(template, label), self.not_found(template, helper_label)
            for code in (113, 1, 255) if template in ("{d}", "{b}\n{d}\n", "{d}\n{b}\n") else (113,):  # any nonzero status admits exactly the closed set
                self.assertEqual(both(answer=text, answer_rc=code), absent, (template, code))  # install and removal read the same text the same way
                one, other = seen(hosted, "", code), seen("", hosted, code)  # one stream carries the whole answer: the helper reads it as the product does
                self.assertEqual([(s["active"], s["origin"], s["owned_pid_digest"]) for s in (one, other)], [("absent", "absent", "none")] * 2, (template, code))
            self.assertEqual(both(answer=text, answer_rc=0), unknown, template)  # a status-0 answer is never the not-found diagnostic, the closed set or not
            self.assertEqual(seen(hosted, "", 0)["active"], "unknown", template)
            self.assertEqual(seen("", hosted, 0)["active"], "unknown", template)
        for template in self.NOT_FOUND_REFUSED:
            text, hosted = self.not_found(template, label), self.not_found(template, helper_label)
            self.assertEqual(both(answer=text, answer_rc=113), unknown, (template, 113))  # nothing outside the closed set is absence, in either observer
            for out, err in ((hosted, ""), ("", hosted)):
                self.assertEqual(seen(out, err)["active"], "unknown", (template, out, err))
        for template in ("{b}\n\n{d}\n", "{b}\n{dl}", "{b}\n{du}", "{d}\n{b}\r", "{b}\n{d}\n\n"):  # refused variants with a status of 0 and others stay unknown
            text, hosted = self.not_found(template, label), self.not_found(template, helper_label)
            for code in (0, 2, 255):
                self.assertEqual(both(answer=text, answer_rc=code), unknown, (template, code))
                self.assertEqual(seen(hosted, "", code)["active"], "unknown", (template, code))
        # The two lines may come from two streams: the product merges them in the order they were written, the stub writes stdout first, so each ORDER is absence for the product, and the helper (which reads two
        # pipes and cannot know that order) is never looser: an answer split across the streams, at any byte, is unknown there.
        for template, first in (("{b}\n{d}\n", "{b}\n"), ("{d}\n{b}\n", "{d}\n"), ("{b}\n{d}", "{b}\n"), ("{d}\n{b}", "{d}\n"), ("{d}\n", "{d}")):
            text, hosted = self.not_found(template, label), self.not_found(template, helper_label)
            cut, hosted_cut = len(self.not_found(first, label)), len(self.not_found(first, helper_label))
            self.assertEqual(both(answer=text[:cut], answer_err=text[cut:], answer_rc=113), absent, (template, "split across stdout and stderr"))
            self.assertEqual(seen(hosted[:hosted_cut], hosted[hosted_cut:])["active"], "unknown", (template, "split across stdout and stderr"))
        for template in ("{b}\n{d}\n", "{d}\n{b}\n"):  # every byte of the two modern forms, as the helper reads them
            hosted = self.not_found(template, helper_label)
            for cut in range(len(hosted) + 1):
                want = "absent" if cut in (0, len(hosted)) else "unknown"
                self.assertEqual(seen(hosted[:cut], hosted[cut:])["active"], want, (template, cut))
        # A refused text stays refused whichever way the two streams share it (the product reads the merged text), and a split never makes the helper read what the product refuses.
        for template in ("{b}\n\n{d}\n", "{b}\n{dl}\n", "{b}\n{du}\n", "{b}\n{d}\n\n"):
            text, hosted = self.not_found(template, label), self.not_found(template, helper_label)
            cut = text.index("\n") + 1
            self.assertEqual(both(answer=text[:cut], answer_err=text[cut:], answer_rc=113), unknown, template)
            self.assertEqual(seen(hosted[:cut], hosted[cut:])["active"], "unknown", template)
        # A loaded or failing answer is never absence: a framed label answer is loaded at status 0 and unknown at any other, the closed set before or after a frame is unknown, and the byte bound and the NUL refusal stand.
        uid = os.getuid()
        frame = lambda name: "gui/%d/%s = {\n\tstate = running\n}\n" % (uid, name)
        self.assertEqual(both(answer=frame(label), answer_rc=0), {o: "loaded:true" for o in ("install", "removal")})
        self.assertEqual(seen(frame(helper_label), "", 0)["active"], "loaded")
        for code in (1, 113):
            self.assertEqual(both(answer=frame(label), answer_rc=code), unknown, code)
            self.assertEqual(seen(frame(helper_label), "", code)["active"], "unknown", code)
        modern = self.not_found("{b}\n{d}\n", label)
        for text in (modern + frame(label), frame(label) + modern, "{b}\n".format(b="Bad request.") + frame(label), modern + "\x00", "\x00" + modern, modern.replace("find", "fi\x00nd"),
                     modern + " " * 65536, "Bad request.\n" + "x" * 65536, "Bad request.\n" + self.not_found("{d}", label) + "\n" * 65536):
            self.assertEqual(both(answer=text.encode(), answer_rc=113), unknown, text[:40])
            self.assertEqual(seen(text.replace(label, helper_label), "")["active"], "unknown", text[:40])
        # With the label absent the disabled table still decides `enabled`: the two-line answer leaves it alone.
        for observer in ("install", "removal"):
            disabled = '\tdisabled services = {\n\t\t"%s" => disabled\n\t}\n' % label
            self.assertEqual(self.product_launchd(observer, disabled, answer=modern, answer_rc=113), "absent:false", observer)
            self.assertEqual(self.product_launchd(observer, disabled, answer=modern, answer_rc=113, table_rc=1), "unknown:unknown", observer)

    def test_both_product_observers_carry_the_same_not_found_grammar_and_the_helper_names_the_same_texts(self):
        install, removal = (self.text(path) for path in ("relay/native-install-transaction.sh", "relay/common.sh"))
        line = r'''case "$output" in "$notfound"|"Bad request."$'\n'"$notfound"|"$notfound"$'\n'"Bad request.") ;; *) return 1 ;; esac''' + "\n"  # the closed set, byte for byte, in both
        self.assertEqual((install.count(line), removal.count(line)), (1, 1))
        for source, uid in ((install, "$UID"), (removal, "$uid")):
            self.assertEqual(source.count('notfound="Could not find service \\"$label\\" in domain for user gui: %s"\n' % uid), 1)
            self.assertNotIn('[ "$output" = "Could not find service', source)  # the single-line equality was the defect: it is gone
            self.assertIn(" name state=0 notfound\n", source)  # a local of the observer, not a global
            self.assertEqual(len(re.findall(r'launchctl print "\$domain/\$label" 2>&1 \| \w+_read_bounded 65536', source)), 1)  # the merged query and its byte bound are unchanged
        host, label, uid = self.load_host(), "x.service", os.getuid()
        diag = 'Could not find service "%s" in domain for user gui: %d' % (label, uid)
        self.assertEqual(host.not_found_texts(label, uid), (diag, "Bad request.\n" + diag, diag + "\nBad request."))  # the helper's closed set is the product's three texts
        self.assertEqual(host.not_found_texts("other.service", 7)[0], 'Could not find service "other.service" in domain for user gui: 7')

    def test_hosted_not_found_controls_carry_every_variant_of_the_matrix_grammar(self):
        # F002: the hosted-guarded controls (not run here: they need a hosted runner) must exercise every accepted and every refused variant this matrix declares, not a subset. They are standalone
        # scripts, so each holds a byte-for-byte copy of NOT_FOUND_ACCEPTED / NOT_FOUND_REFUSED and the statements that drive them; this reads both files as text and compares, so a variant added
        # or changed here, or a driving statement dropped there, fails here instead of going unnoticed until a hosted run.
        driving = (
            'fill = lambda template: template.format(b=bad, d=ours, dl=diag("com.example.other"), du=diag(label, str(os.getuid() + 1)), dc=ours[:-1], dlow=ours.lower())',
            'accepted += [("", fill(x), code) for x in NOT_FOUND_ACCEPTED for code in ((113, 1, 255) if x in ("{d}", "{b}\\n{d}\\n", "{d}\\n{b}\\n") else (113,))]',
            'refused += [("", fill(x), 113) for x in NOT_FOUND_REFUSED] + [("", fill(x), 0) for x in NOT_FOUND_ACCEPTED]',
            'refused += [("", fill(x), code) for x in ("{b}\\n\\n{d}\\n", "{b}\\n{dl}", "{b}\\n{du}", "{d}\\n{b}\\r", "{b}\\n{d}\\n\\n") for code in (0, 2, 255)]',
            'for x, first in (("{b}\\n{d}\\n", "{b}\\n"), ("{d}\\n{b}\\n", "{d}\\n"), ("{b}\\n{d}", "{b}\\n"), ("{d}\\n{b}", "{d}\\n"), ("{d}\\n", "{d}")):',
            'accepted.append((fill(x)[:len(fill(first))], fill(x)[len(fill(first)):], 113))',
            'for x in ("{b}\\n\\n{d}\\n", "{b}\\n{dl}\\n", "{b}\\n{du}\\n", "{b}\\n{d}\\n\\n"):',
            'refused.append((fill(x)[:fill(x).index("\\n") + 1], fill(x)[fill(x).index("\\n") + 1:], 113))')
        for path, name in (("tests/test_native_install.sh", "native_modern_not_found_control"), ("tests/test_service_uninstall_cli.py", "launchd_not_found_control")):
            source = self.text(path)
            if path.endswith(".sh"):  # the suite is a shell script around one Python program
                source = source.split("python3 - \"$REPO_DIR\" \"$WORK_DIR\" <<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
            control = next(node for node in ast.parse(source).body if isinstance(node, ast.FunctionDef) and node.name == name)
            held = {target.id: ast.literal_eval(node.value) for node in ast.walk(control) if isinstance(node, ast.Assign) for target in node.targets
                    if isinstance(target, ast.Name) and target.id in ("NOT_FOUND_ACCEPTED", "NOT_FOUND_REFUSED")}
            self.assertEqual(held, {"NOT_FOUND_ACCEPTED": self.NOT_FOUND_ACCEPTED, "NOT_FOUND_REFUSED": self.NOT_FOUND_REFUSED}, path)
            body = ast.get_source_segment(source, control).replace("'", '"')
            for statement in driving:
                self.assertIn(statement, body, (path, statement))
            self.assertEqual((body.count("NOT_FOUND_ACCEPTED"), body.count("NOT_FOUND_REFUSED")), (4, 3), path)  # the comment, the definition and the loops over it (accepted twice: absence, and status 0); no other use
        # the variants the hosted subsets lacked are in the shared sets and are refused or accepted as the closed set says
        self.assertIn("{d}\n{b}", self.NOT_FOUND_ACCEPTED)
        self.assertTrue({"BAD REQUEST.\n{d}", "{d}\n\n{b}"} <= set(self.NOT_FOUND_REFUSED))

    def test_label_stream_names_one_stream_with_a_fixed_word_and_never_its_bytes(self):
        host, label, uid = self.load_host(), "x.service", os.getuid()
        diag = 'Could not find service "%s" in domain for user gui: %d' % (label, uid)
        target = "gui/%d/%s" % (uid, label)
        words = (("", "empty"), ("Bad request.", "bad-request"), ("Bad request.\n", "bad-request"), (diag, "not-found"), (diag + "\n", "not-found"),
                 ("Bad request.\n" + diag, "bad-request+not-found"), ("Bad request.\n" + diag + "\n", "bad-request+not-found"),
                 (diag + "\nBad request.", "not-found+bad-request"), (diag + "\nBad request.\n", "not-found+bad-request"),
                 (target + " = {}", "framed"), (target + " = {}\n", "framed"), (target + " = {\n\tstate = running\n}\n", "framed"), (target + " = {\n}", "framed"),
                 ("\n", "other"), (" ", "other"), ("\n\n", "other"), ("Bad request.\n\n", "other"), ("Bad request.\n\n" + diag, "other"), ("Bad request.\nBad request.\n", "other"), (diag + "\n\n", "other"),
                 ("Bad request.\r\n" + diag, "other"), ("bad request.", "other"), (diag.upper(), "other"), ('Could not find service "y.service" in domain for user gui: %d' % uid, "other"),
                 ('Could not find service "%s" in domain for user gui: %d' % (label, uid + 1), "other"), ("Bad request. " + diag, "other"), ("extra\n" + diag, "other"),
                 (target + " = {\n\tstate = running\n", "other"), (target + " = {\n\x00}\n", "other"), ("gui/%d/y.service = {}" % uid, "other"), (target + " = {} ", "other"),
                 ("controller unavailable\n", "other"), ("x" * 4096, "other"))
        for text, want in words:
            self.assertEqual(host.label_stream(text, label, uid), want, text[:60])
            self.assertIn(want, m.LABEL_STREAMS[:-1])  # every word the helper can name is one the receipt gate accepts (and `not-run` is the gate's own)
        self.assertEqual({w for _, w in words}, set(m.LABEL_STREAMS[:-1]))  # and every one of them is reached
        # label_shape: ONE read-only `launchctl print gui/<uid>/<label>`, printed as `<status> <stdout word> <stderr word>`; nothing else of the answer, the label or the uid.
        secret, uid = "secret-token-4711", 4242  # a uid that no status or word can contain
        diag, target = 'Could not find service "%s" in domain for user gui: %d' % (label, uid), "gui/%d/%s" % (uid, label)
        # Each reply is sh_kept's (kept, status, stdout, stderr): kept False is sh's own outcome (over the bound, could not run, timed out), kept True a program that ran to its own exit.
        for reply, want in (((True, 113, "", "Bad request.\n" + diag + "\n"), "113 empty bad-request+not-found"), ((True, 113, "Bad request.\n", diag + "\n"), "113 bad-request not-found"),
                            ((True, 113, diag + "\n", "Bad request.\n"), "113 not-found bad-request"), ((True, 113, "", diag), "113 empty not-found"), ((True, 0, target + " = {}\n", ""), "0 framed empty"),
                            ((True, 1, secret + "\n", secret), "1 other other"), ((True, 255, "", ""), "255 empty empty"), ((True, 113, "Bad request.\n" + diag + "\n", ""), "113 bad-request+not-found empty"),
                            ((False, 126, "", ""), "not-run not-run not-run"), ((False, 127, "", ""), "not-run not-run not-run"),  # sh ended the query or never ran it: nothing was captured, so no status either (F001, F002)
                            ((True, 126, "", ""), "126 empty empty"), ((True, 127, "", ""), "127 empty empty"),  # a launchctl that really exits 126 or 127 is recorded like any other status (F001)
                            ((True, 126, "Bad request.\n", diag + "\n"), "126 bad-request not-found"), ((True, 127, "", "Bad request.\n" + diag + "\n"), "127 empty bad-request+not-found"),
                            ((True, 127, diag + "\n", "Bad request.\n"), "127 not-found bad-request"), ((True, 126, target + " = {}\n", ""), "126 framed empty"), ((True, 127, secret, secret), "127 other other"),
                            ((True, -9, "", ""), "137 empty empty"), ((True, -15, "Bad request.\n", diag + "\n"), "143 bad-request not-found"),  # a signal leaves no exit status of its own: 128 + N, as the shell reads it, with the streams it wrote (F003)
                            ((True, -9, "Bad request.\n", diag + "\n"), "137 bad-request not-found"), ((True, -11, "", "Bad request.\n" + diag + "\n"), "139 empty bad-request+not-found"), ((True, -6, secret, secret), "134 other other")):
            calls = []
            with unittest.mock.patch.object(host, "sh_kept", side_effect=lambda argv, *a, **k: calls.append(argv) or reply), unittest.mock.patch.object(host.os, "getuid", return_value=uid), \
                    contextlib.redirect_stdout(io.StringIO()) as out:
                self.assertEqual(host.label_shape({"label": label}, []), 0)
            self.assertEqual(out.getvalue(), want + "\n", reply)
            self.assertEqual(calls, [[host.LAUNCHCTL, "print", target]])  # one query, read-only: print of this label and no other command
            self.assertRegex(out.getvalue(), r"\A([0-9]{1,3} (%s) (%s)|not-run not-run not-run)\n\Z" % (("|".join(map(re.escape, m.LABEL_STREAMS[:-1])),) * 2))  # never a number beside `not-run`, never `not-run` beside a number
            self.assertTrue(all(token not in out.getvalue() for token in (secret, label, str(uid), "gui", "service")))  # no byte of the answer, no label, no uid
        with unittest.mock.patch.object(host, "sh_kept", return_value=(True, 113, "", "Bad request.\n" + diag + "\n")), unittest.mock.patch.object(host.os, "getuid", return_value=uid), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(host.main(["native-lifecycle-host.py", "label-shape", "--label", label]), 0)  # the command is wired
        self.assertEqual(out.getvalue(), "113 empty bad-request+not-found\n")
        # a real run of a stand-in `launchctl`, over the real `sh_kept`: statuses and both streams are read as raw pipes
        with tempfile.TemporaryDirectory() as tmp:
            stub = pathlib.Path(tmp, "launchctl")
            host.LAUNCHCTL = str(stub)
            real_kept, uid = host.sh_kept, os.getuid()
            answer = "printf 'Bad request.\\n'\nprintf 'Could not find service \"%s\" in domain for user gui: %d\\n' >&2\n"

            def ask(script, timeout=10):
                stub.write_text("#!/bin/sh\n" + script)
                stub.chmod(0o755)
                with unittest.mock.patch.object(host, "sh_kept", side_effect=lambda argv, **kw: real_kept(argv, timeout=timeout, **kw)), contextlib.redirect_stdout(io.StringIO()) as out:
                    host.label_shape({"label": label}, [])
                return out.getvalue()
            for who, number, want in ((label, uid, "113 bad-request not-found\n"), ("y.service", uid, "113 bad-request other\n"), (label, uid + 1, "113 bad-request other\n")):
                self.assertEqual(ask(answer % (who, number) + "exit 113\n"), want, (who, number))  # both streams read raw: the diagnostic of this label and uid is `not-found`, any other is `other`
            # F001: the program's OWN exit 126 or 127 keeps what it wrote; only sh's outcomes (over the bound, timeout, no such program) are `not-run`
            for code in (126, 127):
                self.assertEqual(ask(answer % (label, uid) + "exit %d\n" % code), "%d bad-request not-found\n" % code, code)
                self.assertEqual(ask("exit %d\n" % code), "%d empty empty\n" % code, code)
            self.assertEqual(ask("head -c 70000 /dev/zero | tr '\\000' x\nexit 126\n"), "not-run not-run not-run\n")  # over the 65,536-byte bound: sh kills it and keeps nothing
            self.assertEqual(ask("sleep 30\n", timeout=1), "not-run not-run not-run\n")  # timed out
            self.assertEqual(ask(answer % (label, uid) + "kill -9 $$\n"), "137 bad-request not-found\n")  # F003: ended by a signal after it answered: 128 + 9, with the streams it wrote
            self.assertEqual(ask("kill -15 $$\n"), "143 empty empty\n")  # and with nothing written
            stub.unlink()
            with contextlib.redirect_stdout(io.StringIO()) as out:
                host.label_shape({"label": label}, [])  # no such program: sh could not run it
            self.assertEqual(out.getvalue(), "not-run not-run not-run\n")

    def test_helper_and_both_product_observers_agree_on_generated_tables(self):
        host, label, rng = self.load_host(), "com.herdr-mobile-relay.service", random.Random(20261022)
        blanks = ("", "", " ", "\t", "\t\t", "\r", "\x0b", "\x0c", " \t ")
        rows = ['"%s" => %s' % (label, v) for v in ("true", "false", "disabled", "enabled", "disabled", "maybe", "TRUE", "", "true;")] + ['"com.other" => %s' % v for v in ("true", "false", "disabled", "enabled", "maybe")]
        rows += ['"x" => true "%s" => disabled' % label, '"%sx" => true' % label, '"%s"=>true' % label, '"\u00e9" => enabled', '"" => true', '"other"junk" => enabled', '"a"b" => true', '"a => b" => disabled']
        heads = ("disabled services = {junk", "disabled services = { }", "login item associations = {", "login item associations = {}", "x = {", "x = {}", "disabled services = {", "disabled services = {}",
                 "disabled  services = {", "a = b = {", "= {}", 'q"x = {', "x{y = {}", "disabled services = {} x", "Disabled services = {")
        stray = ("}", "}}", "} x", "stray", "{", "", " ", "text = value")
        pad = lambda text: rng.choice(blanks) + text + rng.choice(blanks)

        def table():
            lines = [pad(rng.choice(("disabled services = {",) * 4 + ("disabled services = {}",)))]
            if lines[0].strip().endswith("{"):
                lines += [pad(rng.choice(rows)) for _ in range(rng.randint(0, 4))] + [pad("}")]
            for _ in range(rng.randint(0, 2)):
                lines.append(pad(rng.choice(("login item associations = {}", "login item associations = {"))))
                if lines[-1].strip().endswith("{"):
                    lines += [rng.choice(("\t\"x\" => 12", "junk", "", '"%s" => disabled' % label)) for _ in range(rng.randint(0, 2))] + [pad("}")]
            for _ in range(rng.randint(0, 2)):  # mutations: drop a line, add a head or stray line, repeat a line, or change one character
                kind, at = rng.random(), rng.randint(0, len(lines))
                if kind < .25 and lines:
                    del lines[min(at, len(lines) - 1)]
                elif kind < .5:
                    lines.insert(at, pad(rng.choice(heads + stray + tuple(rows))))
                elif kind < .7 and lines:
                    lines.insert(at, lines[min(at, len(lines) - 1)])
                elif lines and lines[min(at, len(lines) - 1)]:
                    j = min(at, len(lines) - 1)
                    k = rng.randrange(len(lines[j]))
                    lines[j] = lines[j][:k] + rng.choice((" ", "x", "}", "{", '"', "=", "\t", "\r")) + lines[j][k + 1:]
            return "\n".join(lines) + rng.choice(("\n", "", "\n\n"))
        tables = [table() for _ in range(800)] + ["disabled services = {\n}\n", "disabled services = {}\n", '\tdisabled services = {\n\t\t"%s" => disabled\n\t}\n\n\tlogin item associations = {\n\t}\n' % label]
        script = textwrap.dedent("""\
            set +e
            launchctl() { case "$1" in print) case "$2" in gui/*/*) printf '%s = {\\n}' "$2" ;; esac ;; print-disabled) cat "$TMP/t$I" ;; esac; }
            native_manager=launchd; source relay/native-install-transaction.sh; source relay/common.sh
            for ((I = 0; I < COUNT; I++)); do
                native_install_observe "$LABEL" && a=$NATIVE_ENABLED || a=unknown
                service_removal_observe launchd "$LABEL" && b=$SERVICE_REMOVAL_ENABLED || b=unknown
                echo "$a $b"
            done
        """)
        with tempfile.TemporaryDirectory() as tmp:  # one shell for all of them; the tables go through files (a NUL cannot ride in an environment variable)
            for number, text in enumerate(tables):
                pathlib.Path(tmp, "t%d" % number).write_bytes(text.encode())
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, text=True, cwd=ROOT,
                                  env={"PATH": "/usr/bin:/bin", "HOME": tmp, "TMP": tmp, "COUNT": str(len(tables)), "LABEL": label})
        answers = done.stdout.splitlines()
        self.assertEqual(len(answers), len(tables), done.stderr[:300])
        for text, answer in zip(tables, answers):
            want = host.disabled_flag(0, text, label)
            self.assertEqual(answer, "%s %s" % (want, want), text)  # the helper, native_install_observe and service_removal_observe read every generated table the same way
        outcomes = {flag: sum(1 for answer in answers if answer.split()[0] == flag) for flag in ("true", "false", "unknown")}
        self.assertTrue(all(outcomes[flag] >= 15 for flag in ("true", "false", "unknown")), outcomes)  # the generator reaches every outcome, so the agreement is not vacuous

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
        # launchd: the not-found diagnostic is the product's exact text after ONE terminating LF only (a second LF, any other blank, a CR or a leading newline is not it; Task XR F001 made install drop one LF, as removal does)
        uid = os.getuid()
        missing = 'Could not find service "x.service" in domain for user gui: %d' % uid
        for text, code, want in ((missing, 113, "absent"), (missing + "\n", 113, "absent"), (missing + "\n\n", 1, "unknown"), (missing + "\n\n\n", 113, "unknown"), (" " + missing, 113, "unknown"), (missing + " ", 113, "unknown"),
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
        # A CRLF table is read as the product reads it: CR is a blank (`[[:space:]]`), so each row still parses (it was unknown before Task X, when only a leading blank was trimmed). The label's own
        # answer is still read as the bytes are: a CRLF `print` reply is no reply, so the real `sh` must not translate CRLF to LF before the parse.
        self.assertEqual(self.observed_real(answer, table("\r\n"))["enabled"], "false")
        self.assertEqual(self.observed_real(answer.replace(b"\n", b"\r\n"), table("\n"))["owned_pid_digest"], "unknown")
        modern = b'\tdisabled services = {\n\t\t"x.service" => disabled\n\t}\n\n\tlogin item associations = {\n\t}\n'  # macOS 13+: indented, keyword states, a trailing section
        self.assertEqual(self.observed_real(answer, modern)["enabled"], "false")
        self.assertEqual(self.observed_real(answer, modern.replace(b"disabled\n", b"enabled\n"))["enabled"], "true")
        # The bound is in BYTES, as in the product: 40,000 two-byte characters are under 65,536 characters and over 65,536 bytes.
        wide = "\u00e9" * 40000
        big_table = ('disabled services = {\n\t"%s" => true\n}\n' % wide).encode()  # a well-formed row, so that the byte bound is what refuses it
        self.assertEqual(self.observed_real(answer, big_table)["enabled"], "unknown")
        big_answer = ("gui/%d/x.service = {\n\tstate = not running\n\tnote = %s\n}\n" % (os.getuid(), wide)).encode()
        seen = self.observed_real(big_answer, table("\n"))
        self.assertEqual((seen["active"], seen["origin"], seen["owned_pid_digest"]), ("unknown", "unknown", "unknown"))
        padded = ('disabled services = {\n\t"%s" => true\n}\n' % ("\u00e9" * 20000)).encode()  # 40,000 bytes: within the bound
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

    def test_trust_removal_deletes_the_macos_ca_by_sha1_without_the_hanging_remove_trusted_cert_and_proves_its_absence(self):
        sha, chain, fails = "A1B2" * 10, "/Library/Keychains/System.keychain", 'bsudo() { echo "bsudo $*" >> "$LOG"; return 1; }\n'
        base = {"S": "/s", "RUN": "7", "ATT": "1", "CA_SHA1": sha}
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        self.assertEqual([l for l in driver.splitlines() if "remove-trusted-cert" in l and not l.lstrip().startswith("#")], [])  # it hangs as root on current runner images (runner-images#12116): never run
        code, log, err = self.run_cleanup("priv trust-del", dict(base, OS="Darwin"), extra=("priv",))
        self.assertEqual((code, log.splitlines()), (0, ["bsudo security delete-certificate -Z %s %s" % (sha, chain)]), (log, err))  # exactly one privileged call: the delete by SHA-1
        self.assertNotIn("remove-trusted-cert", log)
        code, log, err = self.run_cleanup(fails + "priv trust-del", dict(base, OS="Darwin"), extra=("priv",))
        self.assertEqual((code, log.splitlines()), (1, ["bsudo security delete-certificate -Z %s %s" % (sha, chain)]), (log, err))  # a failed delete fails the operation
        code, log, err = self.run_cleanup("priv trust-del", dict(base, OS="Linux"), extra=("priv",))
        self.assertEqual((code, log.splitlines()), (0, ["bsudo rm -f /usr/local/share/ca-certificates/herdr-nlt-7-1.crt", "bsudo update-ca-certificates --fresh"]), (log, err))  # Linux unchanged
        code, log, err = self.run_cleanup(fails + "priv trust-del", dict(base, OS="Linux"), extra=("priv",))
        self.assertEqual((code, log.splitlines()), (1, ["bsudo rm -f /usr/local/share/ca-certificates/herdr-nlt-7-1.crt"]), (log, err))
        # cl_trust end to end (a stubbed keychain listing): the delete runs first, then the listing must hold no trace of the SHA-1; only a definite answer proves absence.
        listing = lambda *hashes: "".join("SHA-256 hash: 00\nSHA-1 hash: %s\nkeychain: \"%s\"\n" % (h, chain) for h in hashes)
        find = 'security() { echo "security $*" >> "$LOG"; [ -z "$KEYS" ] || printf "%s\\n" "$KEYS"; return "$KEYS_RC"; }\n'
        darwin = dict(base, OS="Darwin", TRUSTED="yes", KEYS_RC="0")
        for keys, rc, want in ((listing(sha), "0", 1), (listing("FFFF" * 10, sha.lower()), "0", 1), (listing("FFFF" * 10), "0", 0), ("", "44", 0),
                               (listing("FFFF" * 10), "1", 1), ("", "128", 1)):  # status 44 is the empty keychain; any other failed listing is unknown, never absent
            code, log, err = self.run_cleanup(find + "cl_trust", dict(darwin, KEYS=keys, KEYS_RC=rc), extra=("priv", "cl_trust"))
            self.assertEqual((code, log.splitlines()), (want, ["bsudo security delete-certificate -Z %s %s" % (sha, chain), "security find-certificate -a -Z %s" % chain]), (keys, rc, log, err))
        code, log, err = self.run_cleanup(fails + find + "cl_trust", dict(darwin, KEYS=""), extra=("priv", "cl_trust"))
        self.assertEqual((code, log.splitlines()), (1, ["bsudo security delete-certificate -Z %s %s" % (sha, chain)]), (log, err))  # a failed delete is never rescued by a clean listing
        code, log, err = self.run_cleanup(find + "cl_trust", dict(darwin, TRUSTED=""), extra=("priv", "cl_trust"))
        self.assertEqual((code, log), (0, ""), err)  # nothing was trusted: nothing to remove

    def test_gui_domain_size_check_uses_one_captured_listing(self):
        stub = r"""launchctl() { case "$1" in print) [ "$LC_BYTES" != fail ] || return 1; head -c "$LC_BYTES" /dev/zero | tr '\0' x ;; print-disabled) printf 'disabled services = {\n}\n' ;; esac; }
"""
        with tempfile.TemporaryDirectory() as ev:
            # The listing's size is a diagnostic only (the product reads the domain by status; macOS 15 prints 89,044 bytes): a listing over 65536 bytes no longer fails the probe.
            for size, want, fact in (("fail", 0, "unavailable"), ("100", 0, "available"), ("65536", 0, "available"), ("65537", 0, "available"), ("89044", 0, "available")):  # a failed listing is never a zero-byte one
                code, log, err = self.run_cleanup(stub + "probe_gui", {"EV": ev, "SUID": "501", "LC_BYTES": size}, extra=("probe_gui",))
                self.assertEqual((code, "fact manager_pre %s" % fact in log, "fail" in log), (want, True, want == 1), (size, log, err))

    def test_gui_probe_needs_a_successful_disabled_table_with_its_header(self):
        header = "disabled services = {\n"
        stub = r"""launchctl() { case "$1" in print) printf x ;; print-disabled) cat "$LD_FILE"; return "$LD_RC" ;; esac; }
"""
        # The product's admission (relay/native-install-transaction.sh): at most 65536 BYTES, no NUL, the text in its print-disabled grammar (blank lines ignored, a `disabled services = {` section of
        # `"label" => state` rows closed by `}`, further sections allowed). The bound is tested with blanks and with well-formed rows, so that it, not the grammar, is what refuses.
        pad = 65536 - len(header) - len("\n}\n")  # a table of exactly the bound, trailing newline included
        row = lambda name, state="true": '\t"' + name + '" => ' + state + "\n"

        def sized(total):  # a COMPLETE, well-formed table of exactly `total` bytes: one row naming two-byte characters (and one ASCII byte when odd): at the bound it is under 33,000 characters
            fill = total - len(header) - len(row("")) - len("}\n")
            return header + row("\u00e9" * (fill // 2) + "x" * (fill % 2)) + "}\n"
        for total in (65536, 65537, 65538):
            self.assertEqual((len(sized(total).encode()), len(sized(total)) < 65536), (total, True))
        # (print-disabled output, its status, manager_pre): available only for a SUCCESSFUL, COMPLETE answer; a matching header and then a failing producer is not.
        for out, status, fact in ((header + "}\n", "0", "available"), (header + '\t"x" => true\n}\n', "0", "available"), ("disabled services = {}\n", "0", "available"),
                                  (header + " " * pad + "\n}\n", "0", "available"), (header + " " * (pad + 1) + "\n}\n", "0", "unavailable"), (header + " " * 70000 + "\n}\n", "0", "unavailable"),  # the bound
                                  (header + "x" * pad + "\n}\n", "0", "unavailable"), (header + "x" * (pad + 1) + "\n}\n", "0", "unavailable"), (header + "x" * 70000 + "\n}\n", "0", "unavailable"),  # a stray line, within the bound or not
                                  (header, "0", "unavailable"), (header + '\t"x" => true\n', "0", "unavailable"), (header.rstrip("\n"), "0", "unavailable"),  # header only or truncated: no closing brace
                                  (header + "}\n", "1", "unavailable"), (header, "1", "unavailable"), (header + "}\n", "124", "unavailable"), (header + "}\n", "126", "unavailable"),
                                  (header + "}\n", "127", "unavailable"), ("", "0", "unavailable"), ("something else\n}\n", "0", "unavailable"),
                                  ("not " + header + "}\n", "0", "unavailable"), ("disabled services = {junk\n}\n", "0", "unavailable"),
                                  # Task X: blank lines and the blanks around a line are ignored (these were unavailable), as are further sections after the table; the keyword states are rows too
                                  ("\n" + header + "}\n", "0", "available"), (header + "} \n", "0", "available"), (" \t" + header + "}\r\n", "0", "available"), (header + "}\n\n  \n", "0", "available"),
                                  ("\tdisabled services = {\n\t\t\"x\" => disabled\n\t\t\"y\" => enabled\n\t}\n\n\tlogin item associations = {\n\t}\n", "0", "available"),
                                  ("disabled services = {}\n\nlogin item associations = {}\n", "0", "available"), (header + "}\nlogin item associations = {\n\tx\n}\nb = {}\n", "0", "available"),
                                  (header + "}\nlogin item associations = {\n", "0", "unavailable"), (header + "}\nstray\n", "0", "unavailable"), (header + "}\n}\n", "0", "unavailable"),  # an unclosed section, stray text
                                  (header + "}\n" + header + "}\n", "0", "unavailable"), (header + '\t"x" => maybe\n}\n', "0", "unavailable"), ("login item associations = {}\n" + header + "}\n", "0", "unavailable"),
                                  (header + "\x00}\n", "0", "unavailable"), (header + "x\x00\n}\n", "0", "unavailable"), ("\x00" + header + "}\n", "0", "unavailable"),  # a NUL (a shell substitution would drop it)
                                  (header + row("\u00e9" * 40000) + "}\n", "0", "unavailable"), (header + row("\u00e9" * 20000) + "}\n", "0", "available"),  # bytes, not characters
                                  (header + "\u00e9" * 40000 + "\n}\n", "0", "unavailable"), (header + "\u00e9" * 20000 + "\n}\n", "0", "unavailable"),  # (stray lines: unavailable at any size)
                                  (sized(65536), "0", "available"), (sized(65537), "0", "unavailable"), (sized(65538), "0", "unavailable")):  # the bound itself, with the closing brace in the read: a character count would admit 65,537
            with tempfile.TemporaryDirectory() as ev:
                pathlib.Path(ev, "served").write_bytes(out.encode())
                code, log, err = self.run_cleanup(stub + "probe_gui", {"EV": ev, "SUID": "501", "LD_FILE": ev + "/served", "LD_RC": status}, extra=("probe_gui",))
            self.assertEqual((code, log.count("fact manager_pre"), "fact manager_pre %s" % fact in log, "fail" in log), (0, 1, True, False), (out[:80], status, log, err))
        # The domain listing: the product discards it and reads only the query's status, so only a failed query makes the manager unavailable; neither its size nor a NUL in it does (Task XR F004: a NUL was this probe's own check).
        listing_stub = r"""launchctl() { case "$1" in print) cat "$LP_FILE"; return "$LP_RC" ;; print-disabled) printf 'disabled services = {\n}\n' ;; esac; }
"""
        for listing, status, fact, hard in ((b"gui/501 = {\n}\n", "0", "available", False), (b"", "0", "available", False), (b"gui/501 = {\n}\n", "1", "unavailable", False),
                                            (b"\x00", "0", "available", False), (b"gui/501 = {\n\x00}\n", "0", "available", False), (b"x" * 100 + b"\x00", "0", "available", False),
                                            (b"\x00" + b"x" * 70000, "0", "available", False), (b"x" * 65535 + b"\x00", "0", "available", False), (b"\x00", "1", "unavailable", False),  # a NUL in the listing is no failure (it was); a failed query still is
                                            (b"x" * 65536, "0", "available", False), (b"x" * 65537, "0", "available", False), (b"x" * 89044, "0", "available", False)):  # an over-limit listing is no failure (it was)
            with tempfile.TemporaryDirectory() as ev:
                pathlib.Path(ev, "served").write_bytes(listing)
                code, log, err = self.run_cleanup(listing_stub + "probe_gui", {"EV": ev, "SUID": "501", "LP_FILE": ev + "/served", "LP_RC": status}, extra=("probe_gui",))
            self.assertEqual((code, log.count("fact manager_pre"), "fact manager_pre %s" % fact in log, "fail" in log), (1 if hard else 0, 1, True, hard), (listing[:20], len(listing), status, log, err))
        # The listing's size (a diagnostic) comes from wc's own status, never from a pipeline's: a failed wc that printed a number is a failure, not a measurement.
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
                        and probe.index("OX nul-free") < probe.index("launchctl print-disabled"))  # the listing's status decides; its NUL is recorded (evidence) before the table is asked for
        self.assertEqual([l.strip().split(" ", 2)[:2] for l in probe.splitlines() if "$gn" in l and not l.lstrip().startswith("#")], [["fact", "gui_nul"], ["printf", "'gui"]])  # gui_nul decides nothing: it is assigned (evidence) and read only to be recorded and logged
        self.assertTrue('1) gn=nul ;; esac  # evidence only (any other status, unreadable, is recorded as not-run): the product discards the listing, so a NUL in it fails nothing\n        rc=1\n        {\n            launchctl print-disabled "gui/$SUID" 2>&1 1>&3 3>&- \\\n'
                        '                | { /usr/bin/head -c 4096 > "$EV/disabled.err"; /bin/cat > /dev/null; } 3>&-\n            rc=${PIPESTATUS[0]}\n        } 3> "$EV/disabled.txt" || rc=$?\n        ds=$rc\n'
                        '        if [ "$ds" -eq 0 ] && OX table-ok --path "$EV/disabled.txt"; then avail=yes; fi\n    fi' in probe)  # its own status, then the helper's admission of the file
        self.assertIn('    if [ "$avail" = yes ]; then\n        fact manager_pre available\n', probe)  # the only way to `available`: those two deciders and nothing recorded as evidence
        self.assertTrue('size=$(wc -c < "$EV/gui.txt") || fail' in probe and "$(" not in probe.split("OX table-ok", 1)[0].split("print-disabled", 1)[1] and not any("|" in l.replace("||", "").replace("&&", "") for l in probe.splitlines() if "print" in l or "wc" in l))  # no pipeline anywhere in the measurements

    def test_table_reason_names_the_first_framing_defect_and_agrees_with_table_ok_and_the_product(self):
        host, header = self.load_host(), "disabled services = {\n"
        row = lambda name: '\t"' + name + '" => true\n'
        modern = '\tdisabled services = {\n\t\t"x" => disabled\n\t}\n\n\tlogin item associations = {\n\t}\n'
        frames = {
            "pass": [header + "}\n", header + '\t"x" => true\n}\n', "disabled services = {}\n", header + "}\n\n\n", header + row("\u00e9" * 20000) + "}\n",
                     header + " " * (65536 - len(header) - 3) + "\n}\n",  # the last: exactly the 65536-byte bound
                     # Task X: blank lines and blanks around a line are ignored, rows may use the keyword states, further sections may follow (each of these was a `header` or `trailer` defect)
                     "\n" + header + "}\n", " " + header + "}\n", header + "} \n", header + "}\r\n", modern, "\tdisabled services = {}\n\n\tlogin item associations = {}\n",
                     header + '\t"x" => enabled\n}\n\nlogin item associations = {\n\tjunk\n}\nb = {}\n'],
            "over-limit": [header + "x" * (65536 - len(header) - 2) + "\n}\n", header + "x" * 70000 + "\n}\n", "x" * 65537, header + "\u00e9" * 40000 + "\n}\n", header + "\x00" + "x" * 70000 + "}",
                           header + row("\u00e9" * 40000) + "}\n", header + " " * (65536 - len(header) - 2) + "\n}\n"],
            "nul": [header + "\x00}\n", "\x00" + header + "}\n", header + "x\x00\n}\n", "\x00", modern.replace("login", "lo\x00gin")],
            "header": ["something else\n}\n", "not " + header + "}\n", "}\n", "x", "Disabled services = {\n}\n", "disabled services = \n}\n",
                       "disabled services = {junk\n}\n", "disabled services = { }\n", "login item associations = {}\n" + header + "}\n", "\t  junk\n", "xdisabled services = {\n}\n"],  # the first line decides
            "trailer": [header, header.rstrip("\n"), header + '\t"x" => true\n', header + "}\nextra", "disabled services = {\n}\n}x",
                        header + "stray\n}\n", header + '\t"x" => maybe\n}\n', "disabled services = {}\n}\n", header + "}\nlogin item associations = {\n", header + "}\ndisabled services = {}\n", header + '\t"other"junk" => enabled\n}\n',
                        header + "}\na = b = {\n}\n", header + "}\n= {}\n", modern + "}\n",  # a defect after a good header: a row, an unclosed or stray section, a second table
                        header + "\u00e9" * 20000 + "\n}\n", header + "x" * (65536 - len(header) - 3) + "\n}\n"],  # the bound's old pass frames, text instead of a row: a stray line at any size
            "empty": ["", "\n", "\n\n\n", " \n\t\r\n"]}
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
                    self.assertEqual(self.removal_enabled(0, text, "com.x") != "unknown", want == "pass", where)  # and the removal observer
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
        modern = b'\tdisabled services = {\n\t\t"x" => disabled\n\t}\n\n\tlogin item associations = {\n\t}\n'
        none = {"disabled_status": "not-run", "disabled_bytes": "not-run", "disabled_admission": "not-run", "disabled_head": "none", "disabled_tail": "none", "disabled_err": "none"}
        scenarios = (
            (dict(listing=b"gui/501 = {\n}\n", table=table, print_err="warn: x\n"), 0, "available", {
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "warn: <x>", "disabled_status": "0", "disabled_bytes": str(len(table)), "disabled_admission": "pass",
                "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none"}),
            (dict(print_rc=113, listing=b"", print_err="Could not find domain for user gui: 501\n"), 0, "unavailable", dict(none, gui_print_status="113", gui_nul="not-run", gui_bytes="0",
                                                                                                                                gui_print_err="Could not find domain for user gui: 501")),
            (dict(print_rc=127, listing=b"", print_err=""), 0, "unavailable", dict(none, gui_print_status="127", gui_nul="not-run", gui_bytes="0", gui_print_err="none")),
            (dict(listing=b"gui/501 = {\n\x00}\n"), 0, "available", dict(none, gui_print_status="0", gui_nul="nul", gui_bytes="15", gui_print_err="none", disabled_status="0",  # a NUL in the listing is recorded, no failure (Task XR F004)
                                                                          disabled_bytes="24", disabled_admission="pass", disabled_head="disabled services = {", disabled_tail="}")),
            (dict(dis_rc=2, table=b"", dis_err="Could not\x01 find domain; x\n"), 0, "unavailable", {
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "none", "disabled_status": "2", "disabled_bytes": "0", "disabled_admission": "empty",
                "disabled_head": "none", "disabled_tail": "none", "disabled_err": "Could not find domain <x>"}),
            (dict(dis_rc=1, table=table), 0, "unavailable", {  # a complete table and a failing producer: the frame is fine, the status refuses it
                "gui_print_status": "0", "gui_nul": "clean", "gui_bytes": "14", "gui_print_err": "none", "disabled_status": "1", "disabled_bytes": str(len(table)), "disabled_admission": "pass",
                "disabled_head": "disabled services = {", "disabled_tail": "}", "disabled_err": "none"}),
            (dict(listing=b"x" * 65537), 0, "available", dict(none, gui_print_status="0", gui_nul="clean", gui_bytes="65537", gui_print_err="none", disabled_status="0", disabled_bytes="24",  # no size failure any more:
                                                              disabled_admission="pass", disabled_head="disabled services = {", disabled_tail="}")),  # the listing's size is a diagnostic
            (dict(listing=b"x" * 89044, table=modern), 0, "available", dict(none, gui_print_status="0", gui_nul="clean", gui_bytes="89044",
                gui_print_err="none", disabled_status="0", disabled_bytes=str(len(modern)), disabled_admission="pass", disabled_head="disabled services = {", disabled_tail="}")))  # macOS 15's shape: 89,044 bytes, a modern table
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
        # Parity with the earlier `&&` chain, evaluated here independently: manager_pre is `available` ONLY for a successful listing query (its content is never read: the product reads its status), a successful table and a framed table within the bound.
        old_available = lambda print_rc, listing, dis_rc, table: (print_rc == 0 and dis_rc == 0 and len(table[:65537]) <= 65536
                                                                  and host.framed_table(table[:65537].decode("utf-8", "replace")))
        tables = (table, b"nope\n}\n", header.encode(), b"", header.encode() + b"\x00}", b"x" * 65537)
        runs = [(rc, b"gui/501 = {\n}\n", 0, table) for rc in (1, 113, 127)] + [(0, b"x\x00y", 0, table)] + [(0, b"gui/501 = {\n}\n", rc, t) for rc in (0, 3) for t in tables]
        for print_rc, listing, dis_rc, text in runs:
            code, pairs, log = self.gui_probe(print_rc=print_rc, listing=listing, dis_rc=dis_rc, table=text)
            got = dict(pairs)
            self.assertEqual(got["manager_pre"] == "available", old_available(print_rc, listing, dis_rc, text), (print_rc, listing[:8], dis_rc, text[:20], log))
            stage = [got["gui_print_status"], got["gui_nul"], got["disabled_status"], got["disabled_admission"]]
            self.assertEqual(stage[:2], [str(print_rc), "not-run" if print_rc else ("nul" if b"\x00" in listing else "clean")])
            self.assertEqual(stage[2:] == ["not-run", "not-run"], print_rc != 0)  # a stage that is not reached is `not-run`, exactly where the chain stopped (a NUL in the listing stops nothing)
            self.assertTrue(stage[2:] == ["not-run", "not-run"] or (stage[2] == str(dis_rc) and stage[3] == host.table_reason(text[:65537])), (stage, log))
            self.assertEqual(code, 0, log)
        # The evidence helpers decide nothing: with `shape` and `table-reason` failing, the verdict and the deciding facts are those of the real helper, and the evidence falls back to `not-run` / `none`.
        evidence_down = 'OX() { case "$1" in shape | table-reason) return 1 ;; esac; "$PYO" -I -B "$HOSTPY" "$@"; }\n'
        fallback = {"gui_bytes": "not-run", "gui_print_err": "none", "disabled_bytes": "not-run", "disabled_admission": "not-run", "disabled_head": "none", "disabled_tail": "none", "disabled_err": "none"}
        for text, manager_pre in ((table, "available"), (b"nope\n}\n", "unavailable"), (header.encode(), "unavailable")):
            code, pairs, log = self.gui_probe(table=text, shell=evidence_down)
            self.assertEqual((code, dict(pairs)), (0, dict(fallback, gui_print_status="0", gui_nul="clean", disabled_status="0", manager_pre=manager_pre)), log)
        # A listing whose NUL check cannot run (the helper exits 2) is neither clean nor NUL-bearing: recorded as not-run. That is evidence only: the product reads the query's status, so the table is still asked for and decides.
        code, pairs, log = self.gui_probe(shell='OX() { case "$1" in nul-free) return 2 ;; esac; "$PYO" -I -B "$HOSTPY" "$@"; }\n')
        self.assertEqual((code, dict(pairs)["gui_nul"], dict(pairs)["disabled_status"], dict(pairs)["manager_pre"]), (0, "not-run", "0", "available"), log)
        code, pairs, log = self.gui_probe(table=b"nope\n}\n", shell='OX() { case "$1" in nul-free) return 2 ;; esac; "$PYO" -I -B "$HOSTPY" "$@"; }\n')
        self.assertEqual((code, dict(pairs)["gui_nul"], dict(pairs)["manager_pre"]), (0, "not-run", "unavailable"), log)  # and a table the product refuses still makes the manager unavailable
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

    def test_driver_records_the_baseline_label_shape_as_evidence_before_the_baseline_on_macos_only(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        probe = driver.split("\nprobe_main() {", 1)[1].split("\n}\n", 1)[0].splitlines()
        start = [l.strip() for l in probe].index("phase baseline")
        self.assertEqual([l.split("  #")[0].strip() for l in probe[start + 1:start + 3]], ['[ "$OS" != Darwin ] || label_shape', 'baseline_clean || fail "a product definition, release or mapping already exists on this runner (baseline sub-check: ${BASEFAIL:-unknown})"'])
        self.assertEqual(driver.count("label_shape"), 2)  # its definition and that one call: the lifecycle never asks (no extra query on the cases' runner)
        self.assertEqual(driver.count("label-shape"), 1)
        self.assertNotIn("label_shape", driver.split("\nlifecycle_main() {", 1)[1].split("\n}\n", 1)[0])
        self.assertNotIn("label_shape", driver.split("\nbaseline_clean() {", 1)[1].split("\n}\n", 1)[0])  # the baseline sub-checks, their order and their facts are untouched
        code_lines = [re.sub(r"\s+#.*", "", l) for l in self.shell_function("label_shape").splitlines()]  # the function's code, comments dropped
        self.assertEqual([l.split()[:2] for l in code_lines if l.lstrip().startswith("fact ")], [["fact", "baseline_label_status"], ["fact", "baseline_label_stdout"], ["fact", "baseline_label_stderr"]])
        self.assertEqual(len(re.findall(r"\bHX\b|launchctl|\bas_subject\b|\bOX\b|\bsudo\b|\bpriv\b", "\n".join(code_lines))), 1)  # one helper call, the read-only query inside it; the function runs nothing else
        self.assertNotRegex("\n".join(code_lines), r"\bfail\b|\bexit\b|\breturn 1\b")  # evidence only: it can fail nothing
        wanted = lambda status, out, err: ["HX label-shape --label com.herdr-mobile-relay.service", "fact baseline_label_status " + status, "fact baseline_label_stdout " + out, "fact baseline_label_stderr " + err]
        for reply, rc, want in (("113 empty bad-request+not-found", 0, ("113", "empty", "bad-request+not-found")), ("113 bad-request+not-found empty\n", 0, ("113", "bad-request+not-found", "empty")),
                                ("0 framed empty", 0, ("0", "framed", "empty")), ("126 not-run not-run", 0, ("126", "not-run", "not-run")), ("255 other other", 0, ("255", "other", "other")),
                                ("not-run not-run not-run", 0, ("not-run",) * 3), ("127 bad-request not-found", 0, ("127", "bad-request", "not-found")), ("126 empty empty", 0, ("126", "empty", "empty")),  # F001: the helper's own words, and a real 126/127 with its streams
                                ("", 0, ("not-run",) * 3), ("garbage", 0, ("not-run",) * 3), ("113 empty", 0, ("not-run",) * 3), ("x 113 empty empty", 0, ("not-run",) * 3),
                                ("113 empty empty", 1, ("not-run",) * 3), ("113 empty empty", 127, ("not-run",) * 3),  # a helper that failed leaves nothing
                                ("113 empty x\ncleanup=pass", 0, ("113", "empty", "xcleanuppass")),  # nothing but [0-9a-z+ -] reaches the facts file: no newline, no `=`, so no second fact
                                ("113 empty empty\ncleanup=pass", 0, ("113", "empty", "emptycleanuppass"))):
            code, log, err = self.run_cleanup('HX() { echo "HX $*" >> "$LOG"; printf "%s\\n" "$SHAPE"; return "$SHAPE_RC"; }\nlabel_shape', {"SHAPE": reply, "SHAPE_RC": str(rc), "LABEL": "com.herdr-mobile-relay.service"}, extra=("label_shape",))
            self.assertEqual((code, log.splitlines()), (0, wanted(*want)), (reply, rc, err))
            self.assertEqual(sum(l.startswith("fact ") for l in log.splitlines()), 3)  # three facts, whatever the helper printed

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
        self.assertEqual(host.identity(ready, "r"), digest)
        self.assertEqual(host.identity(ready, SHA), "none")  # a stale A body cannot identify the candidate B
        candidate = ready.replace('"revision": "r"', '"revision": "%s"' % SHA)
        for body, rc, revision, want in ((candidate, "0", SHA, host.identity(candidate)),
                                        (ready, "0", SHA, "none"), (candidate, "28", SHA, "none")):
            stubs = 'readyz_local() { printf "%s" "$BODY"; return "$BODY_RC"; }\n'
            code, log, err = self.run_cleanup(stubs + 'fact id "$(local_ident_revision "$REVISION")"',
                                               {"BODY": body, "BODY_RC": rc, "REVISION": revision}, oneliners=("local_ident_revision", "OX"))
            self.assertEqual((code, log), (0, "fact id %s\n" % want), err)
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
            pathlib.Path(tmp, "selected").write_text(SHA)
            pathlib.Path(tmp, "checksums.txt").write_text("fixture")
            o = {"version": VERSION, "host": "nlt.test", "sha": SHA, "ancestor": m.ANCESTOR, "target": "linux_amd64", "ports": "18375", "art": tmp, "ancestor-art": tmp,
                 "selector": tmp + "/selected", "candidate-digest": hashlib_sha256(b"fixture"), "ancestor-digest": hashlib_sha256(b"fixture"),
                 "candidate-archive": H("1"), "ancestor-archive": H("2"), "log": tmp + "/hits"}
            ready, local = "https://nlt.test/readyz", "http://127.0.0.1:18375/readyz"
            for args in (["-fsSL", local], ["--location", ready], ["--proxy", "http://x", ready], ["--resolve", "nlt.test:443:1.1.1.1", ready], ["-k", ready],
                         ["--cacert", "x", ready], ["-H", "Authorization: x", ready], ["https://example.com/"], ["http://127.0.0.1:9/readyz"], []):
                with self.subTest(args), self.assertRaises(SystemExit) as refused:
                    host.curl_shim(o, args)
                self.assertEqual(refused.exception.code, 2)

    def test_q2_pins_wiring_and_registry_fail_closed(self):
        driver = self.text("scripts/ci/run-native-lifecycle.sh")
        flow = self.text(".github/workflows/native-manager-lifecycle.yml")
        self.assertIn(m.ANCESTOR, driver)
        self.assertIn(m.ANCESTOR, flow)
        for os_name, (digest, size) in m.CLI.items():
            self.assertIn(digest, driver)
            self.assertIn(str(size), driver)
        self.assertIn('HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1', driver)
        self.assertIn('env -u HERDR_SOCKET_PATH -u HERDR_SESSION -u HERDR_CLIENT_SOCKET_PATH', driver)
        self.assertIn('command -v herdr', driver)
        self.assertIn('"$CLI" plugin install 0cv/herdr-mobile-relay --ref "$1" --yes', driver)
        self.assertNotIn('"$SHIMDIR/herdr" 700\n    qinstall', driver)
        begin = self.shell_function("q2_begin")  # hosted 39865658354: relay.env must go to Herdr's plugin config dir, and the service env must follow it
        self.assertRegex(begin, r'\n    CONF="\$QHOME/\.config/herdr/plugins/config/herdr-mobile-relay\.events"[^\n]*\n    senv_init\n')
        self.assertLess(begin.index("senv_init"), begin.index("write_env cloudflare"))
        # Hosted 39911100878: install.sh's cache sentinel must land where the service (HOME=$THOME) caches, or uninstall refuses the cache.
        self.assertIn('XDG_CACHE_HOME="$THOME/.cache"', self.shell_function("qenv"))
        homes = 'RUNB() { for a; do case "$a" in HOME=*|XDG_CONFIG_HOME=*) printf "%s " "$a";; esac; done; }; QHOME=/q THOME=/t; qenv x'
        out = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", self.shell_function("qenv") + homes], capture_output=True, text=True, check=True).stdout
        self.assertEqual(out, "HOME=/t XDG_CONFIG_HOME=/q/.config ")
        # Hosted 39930183057 (macOS): B's launchd reload never reported B; a failed Q2 update now shows the service's own stderr first.
        diag = 'as_subject() { echo "$*"; }; THOME=/t LABEL=l; OS=Darwin q2_diag; OS=Linux q2_diag'
        out = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", self.shell_function("q2_diag") + diag], capture_output=True, text=True, check=True).stdout
        self.assertEqual(out, "tail -n 25 /t/Library/Logs/herdr-mobile-relay/service.err\njournalctl --user -u l -n 25 --no-pager\n")
        cases = self.shell_function("q2_cases")
        for step in ("'real A-to-B CLI update'", "'B update before uninstall'"):
            self.assertIn('[ "$RC" -eq 0 ] || q2_diag\n    expect "$RC" 0 ' + step, cases)  # install, update and uninstall alike (hosted 39925636374: a private HOME hid the service definition)
        host = self.load_host()
        with tempfile.TemporaryDirectory() as tmp:
            # Herdr v0.9.5's real shape (hosted 39860634906 showed the earlier invented shape never matched).
            reg = pathlib.Path(tmp) / ".config/herdr/plugins.json"
            reg.parent.mkdir(parents=True)
            self.assertEqual(host.registry({"home": tmp, "expected": "absent", "resolved": "absent"}, []), 0)
            entry = lambda **src: {"plugin_id": "herdr-mobile-relay.events", "name": "Herdr Mobile Relay", "enabled": True,
                                   "source": dict({"kind": "github", "owner": "0cv", "repo": "herdr-mobile-relay", "requested_ref": m.ANCESTOR, "resolved_commit": m.ANCESTOR}, **src)}
            reg.write_text(json.dumps([{"plugin_id": "other.plugin", "source": {"kind": "local"}}, entry()], indent=2))
            self.assertEqual(host.registry({"home": tmp, "expected": m.ANCESTOR, "resolved": m.ANCESTOR}, []), 0)
            self.assertEqual(host.registry({"home": tmp, "expected": SHA, "resolved": SHA}, []), 1)
            self.assertEqual(host.registry({"home": tmp, "expected": "absent", "resolved": "absent"}, []), 1)
            for bad in (entry(owner="someone"), entry(kind="local"), entry(resolved_commit=SHA)):
                reg.write_text(json.dumps([bad]))
                self.assertEqual(host.registry({"home": tmp, "expected": m.ANCESTOR, "resolved": m.ANCESTOR}, []), 1, bad)
            reg.write_text(json.dumps([entry(), entry()]))  # duplicate registration is ambiguous
            self.assertEqual(host.registry({"home": tmp, "expected": m.ANCESTOR, "resolved": m.ANCESTOR}, []), 1)
            reg.write_text(json.dumps({"plugins": [{"repository": "0cv/herdr-mobile-relay", "ref": m.ANCESTOR, "commit": m.ANCESTOR}]}))
            self.assertEqual(host.registry({"home": tmp, "expected": m.ANCESTOR, "resolved": m.ANCESTOR}, []), 1)  # not Herdr's format
            reg.write_text("[]")
            self.assertEqual(host.registry({"home": tmp, "expected": "absent", "resolved": "absent"}, []), 0)

    def test_q2_rollback_samples_b_tree_before_failed_cli_returns(self):
        # Run the driver's concurrent observation with an inert, bounded CLI stub; the
        # background child returns failure only after the manager/child tree was sampled.
        with tempfile.TemporaryDirectory() as tmp:
            stubs = '''
select_revision() { :; }
qinstall() { /bin/sleep 1; echo 'cli says'; return 9; }
snap() { printf '{"release_revision":"%s","owned_pid_digest":"%s"}' "$SHA" "$B_DIGEST"; }
owned_pids() { printf '4242,4243'; }
'''
            lines = self.text("scripts/ci/run-native-lifecycle.sh").splitlines()
            oneline = lambda name: next(line for line in lines if line.startswith(name + "() {"))
            program = ('set -eu\n' + stubs + '\n'.join(oneline(n) for n in ("jget", "known_digest", "valid_pids")) + '\n'
                       + self.shell_function("q2_update_observe_b") + 'q2_update_observe_b; printf "%s:%s:%s" "$RC" "$BPID" "$B_PIDS"')
            env = {"PATH": "/usr/bin:/bin", "PYO": sys.executable, "SHA": SHA, "APID": H("5"), "B_DIGEST": H("6"), "EV": tmp}
            result = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", program], env=env, capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout, "cli says\n9:%s:4242,4243" % H("6"), (result.stderr, (pathlib.Path(tmp) / "b-update.log").read_text()))
        # Hosted 39921339508: the runner cannot read the test user's private stage, so the hits log is read only as the subject.
        for line in self.text("scripts/ci/run-native-lifecycle.sh").splitlines():
            if "run/hits.log" in line:
                self.assertTrue(any(k in line for k in ("as_subject ", "HX ", "--log ")), line)

    def test_q2_uninstall_checks_private_cache_before_removing_home(self):
        # Run the actual Q2 cleanup body with stubbed manager, CLI and filesystem operations.
        # A present or uninspectable private-HOME cache must retain QHOME; only a proven absence permits its removal.
        stubs = '''
physical_release() { echo /release; }
qenv() { echo "qenv $*" >> "$LOG"; }
qregistry() { :; }
expect() { :; }
residue_gone() { :; }
procs_gone() { :; }
sgone() { echo "cache $*" >> "$LOG"; [ "$CACHE" = absent ]; }
'''
        for cache, allowed in (("absent", True), ("present", False), ("unknown", False)):
            code, log, _ = self.run_cleanup(stubs + 'QHOME=/private/q2 THOME=/home/t; q2_clear', {"CACHE": cache, "SNAP1": json.dumps({"active": "absent"})}, extra=("q2_clear",))
            self.assertIn('cache /private/q2/.cache/herdr-mobile-relay', log)
            # Hosted 39916869695: under the private CLI HOME the product refused releases "outside HOME" and found no service definition.
            self.assertIn('qenv /release/relay/uninstall.sh', log)
            self.assertEqual((code == 0, 'AS /bin/rm -rf /private/q2' in log), (allowed, allowed), cache)

    def test_q2_full_uninstall_metadata_keeps_verified_b_commit_after_unregister(self):
        # Exercise the driver's metadata constructor, not a hand-authored receipt: registry absence
        # after unregister is distinct from the B commit observed immediately before removal.
        env = {"PATH": "/usr/bin:/bin", "PYO": sys.executable, "SHA": SHA, "RESOLVED": SHA, "REGISTRY": "absent", "CASE": "b-full-uninstall",
               "CLI_SHA": m.CLI["Linux"][0], "CLI_SIZE": str(m.CLI["Linux"][1]), "APID": H("5"), "BPID": H("6"), "AIDENT": H("a"), "BIDENT": H("b"), "PUBLIC": "b"}
        code = self.shell_function("q2_meta") + 'q2_meta; printf "%s" "$Q2JSON"'
        result = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", code], env=env, text=True, capture_output=True, check=True)
        actual = json.loads(result.stdout)
        self.assertEqual((actual["resolved"], actual["registry"]), (SHA, "absent"))
        record = case("b-full-uninstall", "Linux")
        record["q2"] = actual
        m.check_case(record, "Linux", SHA)
        bad = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", code], env=dict(env, RESOLVED="absent"), text=True, capture_output=True, check=True)
        record["q2"] = json.loads(bad.stdout)
        with self.assertRaises(m.Refusal):
            m.check_case(record, "Linux", SHA)

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

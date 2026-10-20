#!/usr/bin/env python3
"""Hermetic tests of tests/service_qualification_matrix.py. They read repository text and write only under a fresh
temporary directory: no suite, installer, relay, manager, network, HOME or hosted variable is touched."""
import contextlib
import copy
import io
import json
import os
import pathlib
import re
import shutil
import sys
import tempfile
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tests"))
import service_qualification_matrix as m  # noqa: E402

SOURCE = (ROOT / m.NATIVE).read_text()
FAST = (ROOT / "tests/native_install_fast_cases.json").read_text()
SHA, RUN, ATTEMPT = "a" * 40, "4242", "2"  # attempt 2: attempt 1 artifacts of the same run are stale
CELLS = {c["id"]: c for c in m.enumerate_cells(SOURCE)}
INVENTORY = list(CELLS)
FAST_IDS = set(m.load_fast(FAST, INVENTORY))


def program(body):
    return "python3 - <<'PY'\ndef native_capture_cases(): pass\n" + body + "\nPY\n"


def projection(ids, family, *keep):
    return {tuple(a.get(k) for k in keep) for a in map(m.classify, ids) if a["family"] == family}


class Inventory(unittest.TestCase):
    def test_frozen_inventory_and_gate_fixture_agreement(self):
        self.assertEqual(m.totals(list(CELLS.values())), m.FROZEN)
        self.assertEqual((m.FROZEN["runs"], m.FROZEN["cancels"]), (1864, 8))
        self.assertEqual(m.enumerate_cells(SOURCE, legacy=True), list(CELLS.values()))
        self.assertTrue(all(m.classify(i) for i in INVENTORY))

    def test_enumerator_refuses_what_it_cannot_prove(self):
        gate = "for n in (1, 2):\n    if not cell(f'x-{n}'): continue\n"
        cases = {"duplicate id": "for n in (1, 2):\n    if not cell('x'): continue\n",
                 "unresolvable loop": "for n in unknown():\n    if not cell(f'x-{n}'): continue\n",
                 "run outside a cell": "run(f)\n", "ungated cell call": "x = cell('x')\n",
                 "second run in a cell": gate + "    run(f); run(f)\n",
                 "unresolvable condition": "if unknown():\n    if not cell('x'): pass\n",
                 "while with cells": "while True:\n    run(f)\n"}
        for name, body in cases.items():
            with self.subTest(name), self.assertRaises(m.Refusal):
                m.enumerate_cells(program(body))
        self.assertEqual([c["id"] for c in m.enumerate_cells(program(gate + "    run(f)\n"))], ["x-1", "x-2"])
        indirect = "def inner():\n    for n in (1, 2):\n        if not cell(f'x-{n}'): continue\n        run(f)\ndef outer():\n    inner()\nouter()\n"
        self.assertEqual([c["id"] for c in m.enumerate_cells(program(indirect))], ["x-1", "x-2"])

    def test_moving_a_run_between_cells_fails_even_when_every_aggregate_is_unchanged(self):
        # pointer (4 executions) and empty-identities (4) give up their installer run; capture-valid (8) gains one.
        moved = SOURCE.replace("                assert run(f).returncode == 0\n                assert snapshot(map(pathlib.Path, before)) == before",
                               "                assert snapshot(map(pathlib.Path, before)) == before")
        moved = moved.replace("                result = run(f)\n                assert result.returncode == 0 and \"unbound variable\"",
                              "                result = None\n                assert result.returncode == 0 and \"unbound variable\"")
        moved = moved.replace("        f = d.Fixture(parent, cell_id, transport, platform)\n        result, output = probe(f, label, \"quiet-if\"",
                              "        f = d.Fixture(parent, cell_id, transport, platform)\n        run(f)\n        result, output = probe(f, label, \"quiet-if\"")
        after, before = m.totals(m.enumerate_cells(moved)), m.totals(list(CELLS.values()))
        self.assertEqual({k: v for k, v in after.items() if k != "cell_digest"}, {k: v for k, v in before.items() if k != "cell_digest"})
        self.assertNotEqual(after["cell_digest"], before["cell_digest"])
        with self.assertRaises(m.Refusal):
            m.inventory_of(moved)

    def test_a_changed_inventory_fails_until_reconciled(self):
        for mutated in (SOURCE.replace('for number, spelling in enumerate(("0", "false", "False", "FALSE", "f", "F")):',
                                       'for number, spelling in enumerate(("0", "false", "False", "FALSE", "f")):'),
                        SOURCE.replace('if cell("shim-parity"): native_shim_parity()', "native_shim_parity()")):
            self.assertNotEqual(mutated, SOURCE)
            with self.assertRaises(m.Refusal):
                m.inventory_of(mutated)


class Selection(unittest.TestCase):
    def test_fast_subset_is_explicit_and_covers_every_distinct_seam(self):
        self.assertEqual(len(FAST_IDS), sum(len(g["ids"]) for g in json.loads(FAST)["groups"]))
        runs = sum(CELLS[i]["runs"] for i in FAST_IDS)
        cancels = sum("-cancel-" in i for i in FAST_IDS)
        self.assertEqual((len(FAST_IDS), runs, cancels), (592, 353, 8))
        covered = lambda family, *keep: self.assertEqual(projection(INVENTORY, family, *keep), projection(FAST_IDS, family, *keep), (family, keep))
        for family in ("parity", "origin", "origin-control", "origin-input", "cancel", "defaults"):
            self.assertEqual({i for i in INVENTORY if m.classify(i)["family"] == family},
                             {i for i in FAST_IDS if m.classify(i)["family"] == family})
        covered("probe", "platform", "role", "seam", "fault", "invocation")  # seam x role x shape x caller context
        covered("valid", "platform", "role")
        covered("exit", "platform", "role", "seam", "phase", "at", "conditional")  # phase and caller context per seam/label
        covered("exit", "platform", "transport", "phase")  # transport-specific behavior in every phase
        covered("exit", "platform", "initial", "phase")
        policy = {"refuse": ("name",), "alias": (), "key": ("length",), "false": ("number", "spelling"), "identities": (),
                  "overlap": ("pointer", "prior"), "pointer": ()}  # unsafe path/credential policy on both installers
        for family, keep in policy.items():
            covered(family, "platform", *keep)
        covered("matrix", "platform", "initial", "failure")
        public = [i for i in INVENTORY if m.classify(i).get("failure", "").startswith("public")]
        self.assertEqual(projection(public, "matrix", "platform", "failure", "transport"),
                         projection([i for i in FAST_IDS if i in public], "matrix", "platform", "failure", "transport"))
        exits = [m.classify(i) for i in FAST_IDS if m.classify(i)["family"] == "exit"]
        shapes = {(a["platform"], a["seam"], a["phase"], a["fault"]) for a in exits}
        for a in map(m.classify, INVENTORY):  # every malformed shape, in begin and in both rollback phases, on every seam
            if a["family"] == "exit" and a["phase"] != "fresh":
                self.assertIn((a["platform"], a["seam"], a["phase"], a["fault"]), shapes)

    def test_fast_file_refuses_duplicates_unknown_ids_and_wrong_shapes(self):
        document = json.loads(FAST)
        for mutate in (lambda d: d["groups"][0]["ids"].append(d["groups"][0]["ids"][0]),
                       lambda d: d["groups"][0]["ids"].append("no-such-cell"),
                       lambda d: d["groups"][1]["ids"].append(d["groups"][0]["ids"][0]),
                       lambda d: d["groups"][0].pop("rationale"), lambda d: d.update(schema_version=2),
                       lambda d: d["groups"][0].update(ids="x")):
            broken = copy.deepcopy(document)
            mutate(broken)
            with self.assertRaises(m.Refusal):
                m.load_fast(json.dumps(broken), INVENTORY)

    def test_shards_partition_each_tier_by_cost_class_and_keep_both_managers(self):
        for tier in ("fast", "full"):
            ids = INVENTORY if tier == "full" else [i for i in INVENTORY if i in FAST_IDS]
            shards, loads = m.assign(ids, m.SHARD_COUNTS[tier])
            self.assertEqual(shards, m.assign(ids, m.SHARD_COUNTS[tier])[0])
            self.assertEqual(sorted(sum(shards, [])), sorted(ids))
            self.assertLess(max(loads) / min(loads), 1.02)
            for shard in shards:
                self.assertEqual({m.classify(i).get("platform") for i in shard} - {None}, {"systemd", "launchd"})
        self.assertEqual(m.SHARD_COUNTS, {"fast": 2, "full": 8})

    def test_plan_defaults_to_full_and_refuses_invalid_requests(self):
        whole = m.plan(SOURCE, FAST, {})
        self.assertEqual((whole["tier"], whole["selected"], whole["inventory"]), ("full", INVENTORY, INVENTORY))
        one = m.plan(SOURCE, FAST, {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_SHARD": "2/2"})
        self.assertTrue(set(one["selected"]) <= FAST_IDS)
        self.assertEqual(m.plan(SOURCE, FAST, {"HERDR_NATIVE_INSTALL_CELL": INVENTORY[7]})["selected"], [INVENTORY[7]])
        foreign = next(i for i in m.assign(INVENTORY, 8)[0][1])
        bad = [{"HERDR_NATIVE_INSTALL_TIER": "all"}, {"HERDR_NATIVE_INSTALL_TIER": "FAST"},
               {"HERDR_NATIVE_INSTALL_SHARD": "1/8", "HERDR_NATIVE_INSTALL_TIER": "fast"}, {"HERDR_NATIVE_INSTALL_SHARD": "1/2"},
               {"HERDR_NATIVE_INSTALL_SHARD": "0/8"}, {"HERDR_NATIVE_INSTALL_SHARD": "9/8"}, {"HERDR_NATIVE_INSTALL_SHARD": "01/8"},
               {"HERDR_NATIVE_INSTALL_SHARD": "1/8/1"}, {"HERDR_NATIVE_INSTALL_SHARD": " 1/8"}, {"HERDR_NATIVE_INSTALL_CELL": "nope"},
               {"HERDR_NATIVE_INSTALL_SHARD": "1/8", "HERDR_NATIVE_INSTALL_CELL": foreign},
               {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_CELL": next(i for i in INVENTORY if i not in FAST_IDS)}]
        for env in bad:
            with self.subTest(env), self.assertRaises(m.Refusal):
                m.plan(SOURCE, FAST, env)


class Evidence(unittest.TestCase):
    def write(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value if isinstance(value, str) else json.dumps(value))

    def tree(self, root, tier, mutate=None):
        count = m.SHARD_COUNTS[tier]
        for runner, runner_os in m.RUNNER_OS.items():
            def summary(mode, names):
                return {"schema_version": 2, "candidate_sha": SHA, "mode": mode, "run_id": RUN, "run_attempt": ATTEMPT,
                        "runner": {"os": runner_os, "arch": "x"}, "bash_version": "b", "python_version": "p",
                        "suites": [{"name": n, "exit": 0, "seconds": 1} for n in names], "checkout_unchanged": True, "result": "pass"}
            self.write(root / ("service-suites-%s-%s-%s" % (tier, runner, SHA)) / "summary.json", summary("suites", m.SUITES))
            for index in range(1, count + 1):
                want = m.shard_expectation(tier, "%d/%d" % (index, count), SHA, RUN, ATTEMPT, runner_os, SOURCE, FAST)
                base = root / ("service-native-%s-%s-s%d-%s" % (tier, runner, index, SHA))
                self.write(base / "native-shard-evidence.json", m.shard_evidence(want, "".join(i + "\n" for i in want["ids"]), 0))
                self.write(base / "summary.json", summary("native", ["%s#%s-%dof%d" % (m.NATIVE, tier, index, count)]))
        if mutate:
            mutate(root, tier)

    def gate(self, tier, mutate=None, run_id=RUN, attempt=ATTEMPT):
        with tempfile.TemporaryDirectory() as tmp:
            self.tree(pathlib.Path(tmp), tier, mutate)
            m.gate(tier, SHA, run_id, attempt, tmp, SOURCE, FAST)

    def test_complete_fast_and_full_success_for_every_shard_and_os_group(self):
        for tier in ("fast", "full"):
            self.gate(tier)

    def test_incomplete_stale_or_cross_group_evidence_fails_the_aggregate(self):
        def shard(root, tier, runner="ubuntu-24.04", index=1):
            return root / ("service-native-%s-%s-s%d-%s" % (tier, runner, index, SHA)) / "native-shard-evidence.json"

        def summary(root, runner, pattern):
            return next(root.glob("service-%s-*-%s-*/summary.json" % (pattern, runner) if pattern == "suites" else "service-native-*-%s-s1-*/summary.json" % runner))

        def edit(change, locate=shard):
            def apply(root, tier):
                path = locate(root, tier)
                document = json.loads(path.read_text())
                change(document)
                path.write_text(json.dumps(document))
            return apply

        def suites(change, runner="ubuntu-24.04"):
            return edit(change, lambda root, tier: summary(root, runner, "suites"))

        def remove_shard(root, tier):
            shutil.rmtree(shard(root, tier).parent)

        def link_shard(root, tier):
            shutil.rmtree(shard(root, tier).parent)
            shard(root, tier).parent.symlink_to(root)
        cases = {
            "missing shard": (remove_shard, "artifact set differs"), "missing evidence file": (lambda r, t: shard(r, t).unlink(), "wrong files"),
            "empty evidence": (lambda r, t: shard(r, t).write_text(""), "empty or unsafe"), "malformed JSON": (lambda r, t: shard(r, t).write_text("{"), "malformed JSON"),
            "non-finite number": (lambda r, t: shard(r, t).write_text('{"expected_count": NaN}'), "non-finite"),
            "not an object": (lambda r, t: shard(r, t).write_text("[]"), "wrong keys"),
            "duplicate key": (lambda r, t: shard(r, t).write_text(shard(r, t).read_text().replace('"kind"', '"kind": 1, "kind"', 1)), "duplicate JSON key"),
            "duplicate cell": (edit(lambda d: d["completed"].append(d["completed"][0])), "duplicate completed"),
            "unexpected cell": (edit(lambda d: d["completed"].append("no-such-cell")), "unexpected completed"),
            "partial shard": (edit(lambda d: (d["completed"].pop(), d.update(completed_count=d["completed_count"] - 1))), "missing completed"),
            "start record only": (edit(lambda d: d.update(completed=[], completed_count=0)), "missing completed"),
            "stale candidate": (edit(lambda d: d.update(candidate_sha="b" * 40)), "candidate_sha"), "stale run": (edit(lambda d: d.update(run_id="1")), "run_id"),
            "prior attempt evidence": (edit(lambda d: d.update(run_attempt="1")), "run_attempt"),
            "future attempt evidence": (edit(lambda d: d.update(run_attempt="9")), "run_attempt"),
            "prior attempt native summary": (edit(lambda d: d.update(run_attempt="1"), lambda r, t: summary(r, "ubuntu-24.04", "native")), "stale or from another runner"),
            "prior attempt suites summary": (suites(lambda d: d.update(run_attempt="1"), "macos-15"), "stale or from another runner"),
            "cross tier": (edit(lambda d: d.update(tier="fast" if d["tier"] == "full" else "full")), "expected tier"),
            "cross OS": (lambda r, t: shard(r, t).write_text(shard(r, t, "macos-15").read_text()), "runner_os"),
            "cross shard": (edit(lambda d: d.update(shard_index=2)), "shard_index"), "stale inventory": (edit(lambda d: d.update(inventory_digest="0" * 64)), "inventory_digest"),
            "failed shard": (edit(lambda d: d.update(suite_exit=1, result="fail")), "did not pass"),
            "timed out shard": (edit(lambda d: d.update(suite_exit=124, result="fail")), "did not pass"),
            "claimed pass after failure": (edit(lambda d: d.update(suite_exit=1)), "did not pass"),
            "wrong type": (edit(lambda d: d.update(suite_exit=False)), "did not pass"), "extra key": (edit(lambda d: d.update(extra=1)), "wrong keys"),
            "extra artifact": (lambda r, t: (r / "service-native-stray").mkdir(), "artifact set differs"),
            "extra file": (lambda r, t: (shard(r, t).parent / "stray").write_text("x"), "wrong files"), "symlinked artifact": (link_shard, "missing artifact"),
            "native summary of another shard": (edit(lambda d: d["suites"][0].update(name="x"), lambda r, t: summary(r, "ubuntu-24.04", "native")), "suite records"),
            "suite failed": (suites(lambda d: d["suites"][0].update(exit=1)), "a suite failed"),
            "suite missing": (suites(lambda d: d["suites"].pop(), "macos-15"), "suite records"),
            "suite duplicated": (suites(lambda d: d["suites"].append(d["suites"][0]), "macos-15"), "suite records"),
            "nested doctor suite not required": (suites(lambda d: d["suites"].pop(4)), "suite records")}
        malformed = {  # every nested field of the driver's summary, in both the suites and the native summaries
            "runner without arch": (lambda d: d["runner"].pop("arch"), "malformed runner"),
            "runner arch empty": (lambda d: d["runner"].update(arch=""), "malformed runner"),
            "runner arch not a string": (lambda d: d["runner"].update(arch=1), "malformed runner"),
            "runner extra key": (lambda d: d["runner"].update(extra="x"), "malformed runner"),
            "runner not an object": (lambda d: d.update(runner="Linux"), "malformed runner"),
            "bash version null": (lambda d: d.update(bash_version=None), "malformed summary metadata"),
            "python version empty": (lambda d: d.update(python_version=""), "malformed summary metadata"),
            "run attempt number": (lambda d: d.update(run_attempt=2), "malformed summary metadata"),
            "schema version float": (lambda d: d.update(schema_version=2.0), "wrong keys"),
            "seconds null": (lambda d: d["suites"][0].update(seconds=None), "malformed suite records"),
            "seconds string": (lambda d: d["suites"][0].update(seconds="1"), "malformed suite records"),
            "seconds negative": (lambda d: d["suites"][0].update(seconds=-1), "malformed suite records"),
            "seconds boolean": (lambda d: d["suites"][0].update(seconds=True), "malformed suite records"),
            "seconds float": (lambda d: d["suites"][0].update(seconds=1.5), "malformed suite records"),
            "exit boolean": (lambda d: d["suites"][0].update(exit=False), "malformed suite records"),
            "suite name not a string": (lambda d: d["suites"][0].update(name=1), "malformed suite records"),
            "suite record extra key": (lambda d: d["suites"][0].update(extra=1), "malformed suite records")}
        for name, (change, reason) in malformed.items():
            for kind in ("suites", "native"):
                with self.subTest("malformed summary: %s (%s)" % (name, kind)), self.assertRaisesRegex(m.Refusal, reason):
                    self.gate("fast", edit(change, lambda r, t, k=kind: summary(r, "ubuntu-24.04", k)))
        for tier in ("fast", "full"):
            for name, (mutate, reason) in cases.items():
                with self.subTest(tier + " " + name), self.assertRaisesRegex(m.Refusal, reason):
                    self.gate(tier, mutate)
        with self.assertRaisesRegex(m.Refusal, "run_id"):
            self.gate("fast", run_id="1")
        with self.assertRaisesRegex(m.Refusal, "run_attempt"):
            self.gate("fast", attempt="3")  # every artifact is from attempt 2: a rerun may not reuse them
        with self.assertRaisesRegex(m.Refusal, "run_attempt"):
            self.gate("fast", attempt="1")

    def test_failed_empty_or_partial_cell_records_never_become_passing_evidence(self):
        want = m.shard_expectation("fast", "1/2", SHA, RUN, "1", "Linux", SOURCE, FAST)
        every = "".join(i + "\n" for i in want["ids"])
        self.assertEqual(m.shard_evidence(want, every, 0)["result"], "pass")
        for cells, code in ((every, 1), (every, 124), ("", 0), (every[:-1 - len(want["ids"][-1])], 0), (every + want["ids"][0] + "\n", 0),
                            (every + "no-such-cell\n", 0)):
            self.assertEqual(m.shard_evidence(want, cells, code)["result"], "fail")
        with self.assertRaises(m.Refusal):
            m.shard_evidence(want, every[:-1], 0)  # a torn last line is not a completed record

    def test_command_line_round_trip_plan_evidence_needs_and_gate(self):
        files = ["--source", str(ROOT / m.NATIVE), "--fast-cases", str(ROOT / "tests/native_install_fast_cases.json")]
        identity = ["--sha", SHA, "--run-id", RUN, "--run-attempt", ATTEMPT]
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()):
            tmp = pathlib.Path(directory)
            with mock.patch.dict(os.environ, {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_SHARD": "1/2"}):
                self.assertEqual(m.main(["plan", *files, "--output", str(tmp / "plan.json")]), 0)
                self.assertEqual(m.main(["plan", *files, "--output", str(tmp / "plan.json")]), 1)  # never overwrites
            selected = json.loads((tmp / "plan.json").read_text())["selected"]
            (tmp / "cells").write_text("".join(i + "\n" for i in selected))
            shard = ["evidence", *files, *identity, "--tier", "fast", "--shard", "1/2", "--runner-os", "Linux", "--cells", str(tmp / "cells")]
            self.assertEqual(m.main([*shard, "--suite-exit", "0", "--output", str(tmp / "pass.json")]), 0)
            self.assertEqual(m.main([*shard, "--suite-exit", "1", "--output", str(tmp / "fail.json")]), 1)
            self.assertEqual([json.loads((tmp / n).read_text())["result"] for n in ("pass.json", "fail.json")], ["pass", "fail"])
            with mock.patch.dict(os.environ, {"HERDR_NEEDS_JSON": json.dumps({"a": {"result": "success"}})}):
                self.assertEqual((m.main(["needs", "a"]), m.main(["needs", "a", "b"])), (0, 1))
            self.tree(tmp / "artifacts", "fast")
            self.assertEqual(m.main(["gate", *files, *identity, "--tier", "fast", "--artifacts", str(tmp / "artifacts")]), 0)
            self.assertEqual(m.main(["gate", *files, "--sha", SHA, "--run-id", "9", "--run-attempt", ATTEMPT, "--tier", "fast",
                                     "--artifacts", str(tmp / "artifacts")]), 1)

    def test_dependency_results_must_all_be_success(self):
        names = ("candidate", "suites", "native")
        good = {n: {"result": "success", "outputs": {}} for n in names}
        m.check_needs(json.dumps(good), names)
        for result in ("failure", "cancelled", "skipped", ""):
            for name in names:
                with self.subTest(name + result), self.assertRaises(m.Refusal):
                    m.check_needs(json.dumps(dict(good, **{name: {"result": result}})), names)
        for broken in (json.dumps({k: good[k] for k in names[:2]}), json.dumps(dict(good, extra=good["suites"])), "", "[]", "{"):
            with self.assertRaises(m.Refusal):
                m.check_needs(broken, names)


class Wiring(unittest.TestCase):
    def text(self, path):
        return (ROOT / path).read_text()

    def test_driver_runs_every_required_suite_once_and_workflows_match_the_shard_counts(self):
        driver = self.text("scripts/ci/run-service-qualification.sh")
        run = set(re.findall(r"^\s+run_suite (tests/\S+)", driver, re.M)) | set(re.findall(r"^\s+run_helper_suite (tests/\S+)", driver, re.M))
        run.add("tests/test_service_transport_dispatch.py")
        self.assertEqual(run, set(m.SUITES))
        self.assertIn("EXPECTED_SUITE_COUNT=%d" % len(m.SUITES), driver)
        for tier, name in (("fast", "service-qualification"), ("full", "service-full-qualification")):
            workflow, count = self.text(".github/workflows/%s.yml" % name), m.SHARD_COUNTS[tier]
            self.assertIn("shard: [%s]" % ", ".join(map(str, range(1, count + 1))), workflow)
            self.assertIn("native %s ${{ matrix.shard }}/%d" % (tier, count), workflow)
            self.assertIn("--tier %s" % tier, workflow)
            self.assertIn("if: ${{ always() }}", workflow)
            self.assertEqual(sorted(re.findall(r"runner: \[(.*)\]", workflow)), ["macos-15, ubuntu-24.04"] * 2)
            code = "\n".join(line for line in workflow.lower().splitlines() if not line.lstrip().startswith("#"))
            for forbidden in ("continue-on-error", "retry", "rerun"):
                self.assertNotIn(forbidden, code)
        self.assertIn("workflow_dispatch", self.text(".github/workflows/service-full-qualification.yml"))
        self.assertIn("pull_request", self.text(".github/workflows/service-full-qualification.yml"))
        self.assertIn("schedule", self.text(".github/workflows/service-full-qualification.yml"))

    def test_check_release_and_makefile_keep_every_event_covered(self):
        check, release, make = (self.text(p) for p in (".github/workflows/check.yml", ".github/workflows/release.yml", "Makefile"))
        self.assertIn("shell-check-ci-complement production-path-audit", check)
        self.assertNotIn("resume-benchmark-pilot:", check)
        for name in ("service-full-qualification.yml", "resume-benchmark.yml"):
            self.assertIn(".github/workflows/" + name, check)
        self.assertIn("--workflow service-full-qualification.yml", release)
        self.assertIn("--workflow check.yml", release)
        self.assertIn("deadline=$((SECONDS + 1800))", release)
        self.assertIn("shell-check: shell-check-ci-complement shell-check-service-suites", make)
        complement = make.split("shell-check-ci-complement:\n", 1)[1].split("\n\n", 1)[0]
        service = make.split("shell-check-service-suites:\n", 1)[1].split("\n\n", 1)[0]
        self.assertEqual(re.findall(r"tests/test_\w+\.(?:sh|py)", service), ["tests/test_native_install.sh", "tests/test_uninstall.sh", "tests/test_service_uninstall_cli.py"])
        self.assertFalse(set(re.findall(r"tests/test_\w+\.(?:sh|py)", service)) & set(re.findall(r"tests/test_\w+\.(?:sh|py)", complement)))


if __name__ == "__main__":
    unittest.main()

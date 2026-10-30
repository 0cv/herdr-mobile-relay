#!/usr/bin/env python3
"""Hermetic tests of tests/service_qualification_matrix.py. They read repository text and write only under a fresh
temporary directory: no suite, installer, relay, manager, network, HOME or hosted variable is touched."""
import ast
import contextlib
import copy
import io
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
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
CLI_SOURCE = (ROOT / m.UNINSTALL_CLI).read_text()
CLI_FAST = (ROOT / "tests/uninstall_cli_fast_cases.json").read_text()
CLI_CELLS = {c["id"]: c for c in m.enumerate_cells(CLI_SOURCE, cli=True)}
CLI_INVENTORY = list(CLI_CELLS)
CLI_FAST_IDS = set(m.load_fast(CLI_FAST, CLI_INVENTORY))
TEXTS = {"native": (SOURCE, FAST), "uninstall-cli": (CLI_SOURCE, CLI_FAST)}
FAST_N, FULL_N = m.SHARD_COUNTS["fast"], m.SHARD_COUNTS["full"]


def program(body):
    return "python3 - <<'PY'\ndef native_capture_cases(): pass\n" + body + "\nPY\n"


def projection(ids, family, *keep):
    return {tuple(a.get(k) for k in keep) for a in map(m.classify, ids) if a["family"] == family}


class Inventory(unittest.TestCase):
    def test_frozen_inventory_and_gate_fixture_agreement(self):
        self.assertEqual(m.totals(list(CELLS.values())), m.FROZEN)
        self.assertEqual((m.FROZEN["runs"], m.FROZEN["cancels"]), (1872, 8))
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
        self.assertEqual(m.SHARD_COUNTS, {"fast": 5, "full": 29})

    def test_failed_state_cases_are_distinct_and_refusals_are_not_priced_as_success(self):
        scenarios = ("active-ok", "failed-ok", "active-reset-fails", "active-reset-leaves", "active-start-rollback",
                     "failed-start-rollback", "active-reset-fails-then-leaves", "active-reset-fails-then-disable-fails")
        for scenario in scenarios:
            cell = "systemd-gateway-failed-" + scenario
            self.assertIn(cell, CELLS)
            self.assertEqual(CELLS[cell]["runs"], 1)
            self.assertNotIn(cell, FAST_IDS)
            self.assertEqual(m.cost_class(m.classify(cell)), "install-ok" if scenario.endswith("-ok") else "quiesce")

    def test_the_cost_model_prices_every_class_once_and_never_passes_a_projection_off_as_a_measurement(self):
        classes = {m.cost_class(m.classify(i)) for i in INVENTORY}
        self.assertEqual(set(m.CLASS_SECONDS) | set(m.MODELED_SECONDS), classes)
        self.assertEqual(set(m.CLASS_SECONDS) & set(m.MODELED_SECONDS), set())
        self.assertEqual(set(m.MODELED_SECONDS), {"probe", "valid", "cancel", "parity"})  # no invocation line times a cell of these classes
        self.assertEqual({m.cost_class(m.classify(i)) for i in INVENTORY if CELLS[i]["runs"]}, set(m.CLASS_SECONDS))  # the measured classes are the timed ones
        self.assertTrue(all(v > 0 for pair in m.CLASS_SECONDS.values() for v in pair) and m.MACOS_FACTOR > 1 and m.SHARD_OVERHEAD > 0)
        for i in INVENTORY:
            a = m.classify(i)
            name = m.cost_class(a)
            if name in m.CLASS_SECONDS:
                self.assertEqual(m.cost(i), m.CLASS_SECONDS[name][a.get("platform") == "launchd"])
            else:
                seconds, launchd = m.MODELED_SECONDS[name]
                self.assertAlmostEqual(m.cost(i), seconds * (launchd if a.get("platform") == "launchd" else 1.0) * m.MACOS_FACTOR, 2)
        self.assertGreaterEqual(pathlib.Path(m.__file__).read_text().count("never a measured hosted time"), 3)  # the disclaimer stays where the values live

    def test_counts_are_the_smallest_whose_modeled_macos_shards_stay_within_60_percent_of_the_suite_bound(self):
        self.assertEqual((m.SUITE_BOUND, m.PROJECTION_LIMIT), (900, .6 * 900))
        driver = pathlib.Path(ROOT / "scripts/ci/run-service-qualification.sh").read_text()
        self.assertIn("SUITE_TIMEOUT_SECONDS=%d\n" % m.SUITE_BOUND, driver)  # the bound was not raised to make a count fit
        for tier in ("fast", "full"):
            ids = INVENTORY if tier == "full" else [i for i in INVENTORY if i in FAST_IDS]
            count = m.SHARD_COUNTS[tier]
            projected = m.projection(ids, count)
            self.assertEqual(len(projected), count)
            self.assertLessEqual(max(projected), m.PROJECTION_LIMIT, tier)
            self.assertEqual(projected, [round(load + m.SHARD_OVERHEAD, 1) for load in m.assign(ids, count)[1]])
            for smaller in range(1, count):
                self.assertGreater(max(m.projection(ids, smaller)), m.PROJECTION_LIMIT, "%s would fit in %d shards" % (tier, smaller))
        # back-test: for the two-shard fast layout it was calibrated on, the model projects no less than the slowest hosted macOS shard (843 s)
        self.assertGreaterEqual(min(m.projection([i for i in INVENTORY if i in FAST_IDS], 2)), 843)

    def test_list_prints_the_modeled_shard_seconds_of_the_tiers_count(self):
        for tier, count in (("fast", FAST_N), ("full", FULL_N)):
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(m.main(["list", "--tier", tier]), 0)
            listed = json.loads(out.getvalue())["modeled_shard_seconds"]
            self.assertEqual(len(listed), count)
            self.assertLessEqual(max(listed), m.PROJECTION_LIMIT)

    def test_plan_defaults_to_full_and_refuses_invalid_requests(self):
        whole = m.plan(SOURCE, FAST, {})
        self.assertEqual((whole["tier"], whole["selected"], whole["inventory"]), ("full", INVENTORY, INVENTORY))
        one = m.plan(SOURCE, FAST, {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_SHARD": "%d/%d" % (FAST_N, FAST_N)})
        self.assertTrue(set(one["selected"]) <= FAST_IDS)
        self.assertEqual(m.plan(SOURCE, FAST, {"HERDR_NATIVE_INSTALL_CELL": INVENTORY[7]})["selected"], [INVENTORY[7]])
        foreign = next(i for i in m.assign(INVENTORY, FULL_N)[0][1])
        self.assertEqual(m.plan(SOURCE, FAST, {"HERDR_NATIVE_INSTALL_SHARD": "%d/%d" % (FULL_N, FULL_N)})["shard_count"], FULL_N)  # a two-digit shard is valid
        bad = [{"HERDR_NATIVE_INSTALL_TIER": "all"}, {"HERDR_NATIVE_INSTALL_TIER": "FAST"},
               {"HERDR_NATIVE_INSTALL_SHARD": "1/%d" % FULL_N, "HERDR_NATIVE_INSTALL_TIER": "fast"}, {"HERDR_NATIVE_INSTALL_SHARD": "1/%d" % FAST_N},
               {"HERDR_NATIVE_INSTALL_SHARD": "0/%d" % FULL_N}, {"HERDR_NATIVE_INSTALL_SHARD": "%d/%d" % (FULL_N + 1, FULL_N)},
               {"HERDR_NATIVE_INSTALL_SHARD": "01/%d" % FULL_N}, {"HERDR_NATIVE_INSTALL_SHARD": "1/%d/1" % FULL_N},
               {"HERDR_NATIVE_INSTALL_SHARD": " 1/%d" % FULL_N}, {"HERDR_NATIVE_INSTALL_CELL": "nope"},
               {"HERDR_NATIVE_INSTALL_SHARD": "1/%d" % FULL_N, "HERDR_NATIVE_INSTALL_CELL": foreign},
               {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_CELL": next(i for i in INVENTORY if i not in FAST_IDS)},
               {"HERDR_NATIVE_INSTALL_TIER": ""}, {"HERDR_NATIVE_INSTALL_SHARD": ""}, {"HERDR_NATIVE_INSTALL_CELL": ""}]  # set but empty: not "unset"
        for env in bad:
            with self.subTest(env), self.assertRaises(m.Refusal):
                m.plan(SOURCE, FAST, env)


def cli_projection(ids, family, *keep):
    return {tuple(a.get(k) for k in keep) for a in map(m.classify_cli, ids) if a["family"] == family}


class UninstallCli(unittest.TestCase):
    def test_inventory_is_frozen_complete_and_classified(self):
        self.assertEqual(m.totals(list(CLI_CELLS.values())), m.CLI_FROZEN)
        self.assertEqual((m.CLI_FROZEN["cells"], m.CLI_FROZEN["runs"]), (1040, 1665))  # 1657 Fixture.run invocations of the 0598331f suite and the eight of Task SF
        self.assertEqual([i for i in CLI_INVENTORY if CLI_CELLS[i]["runs"] == 0], ["control/" + n for n in m.CLI_CONTROLS])
        self.assertEqual({m.classify_cli(i)["family"] for i in CLI_INVENTORY} - {"control"}, set(m.CLI_FAMILIES))
        self.assertEqual({m.classify_cli(i).get("platform") for i in CLI_INVENTORY}, {None, "systemd", "launchd"})
        for bad in ("systemd/nonesuch", "sparc/present/0", "systemd/present", "systemd/present/0/1", "systemd/present/", "control/other", "present/0"):
            with self.subTest(bad), self.assertRaises(m.Refusal):
                m.classify_cli(bad)

    def test_enumerator_refuses_what_it_cannot_prove_for_the_cli(self):
        gate = "def main():\n    for n in (1, 2):\n        if not cell(f'systemd/present/{n}'): continue\n"
        cases = {"duplicate id": "def main():\n    for n in (1, 2):\n        if not cell('x'): continue\n        f.run()\n",
                 "unresolvable loop": "def main():\n    for n in unknown():\n        if not cell(f'x/{n}'): continue\n        f.run()\n",
                 "run outside a cell": "def main():\n    f.run()\n", "ungated cell call": "def main():\n    x = cell('x')\n",
                 "a cell without a run": gate + "        pass\n", "a control with a run": "def main():\n    if cell('control/x'): f.run()\n",
                 "unresolvable condition": "def main():\n    if unknown():\n        if not cell('x'): pass\n",
                 "while with cells": "def main():\n    while True:\n        f.run()\n"}
        for name, body in cases.items():
            with self.subTest(name), self.assertRaises(m.Refusal):
                m.enumerate_cells(body, cli=True)
        cells = m.enumerate_cells(gate + "        f.run(); f.run()\n", cli=True)
        self.assertEqual([(c["id"], c["runs"]) for c in cells], [("systemd/present/1", 2), ("systemd/present/2", 2)])
        helper = "def helper(f):\n    for n in (1, 2):\n        f.run()\ndef main():\n    if cell('systemd/present/0'): helper(f)\n"
        self.assertEqual([c["runs"] for c in m.enumerate_cells(helper, cli=True)], [2])
        for text in ("def main():\n    subprocess.run(['x'])\n", "def main():\n    g.run()\n"):  # only f.run is the entrypoint
            self.assertEqual(m.enumerate_cells(text, cli=True), [])
        with self.assertRaises(m.Refusal):
            m.enumerate_cells(CLI_SOURCE, legacy=True, cli=True)

    def test_every_entrypoint_run_sits_behind_a_cell_gate_and_a_removed_gate_is_caught(self):
        def undominated(source):
            main = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == "main")
            parents = {child: node for node in ast.walk(main) for child in ast.iter_child_nodes(node)}
            opens = lambda n: any(isinstance(c, ast.Call) and getattr(c.func, "id", "") == "cell" for c in ast.walk(n.test))
            skips = lambda n: isinstance(n, ast.If) and opens(n) and isinstance(n.test, ast.UnaryOp) and [type(x) for x in n.body] == [ast.Continue]
            entry = lambda n: isinstance(n, ast.Call) and (getattr(n.func, "id", "") == "configuration_refusal" or (
                isinstance(n.func, ast.Attribute) and n.func.attr == "run" and getattr(n.func.value, "id", "") == "f"))
            found = []
            for call in filter(entry, ast.walk(main)):
                node, covered = call, False
                while node in parents and not covered:  # inside `if cell(id): ...`, or after `if not cell(id): continue` in a loop body
                    parent = parents[node]
                    covered = isinstance(parent, ast.If) and opens(parent) and not skips(parent) and node in parent.body
                    covered = covered or isinstance(parent, ast.For) and node in parent.body and any(skips(x) for x in parent.body[:parent.body.index(node)])
                    node = parent
                found += [] if covered else [call.lineno]
            return found
        self.assertEqual(undominated(CLI_SOURCE), [])
        gate = "                if not cell(f'{manager}/definition/{shape}'): continue\n"
        self.assertEqual(CLI_SOURCE.count(gate), 1)
        broken = CLI_SOURCE.replace(gate, "")
        self.assertEqual(len(undominated(broken)), 1)  # that run now belongs to the previous cell's scope only by accident
        with self.assertRaises(m.Refusal):
            m.inventory_of(broken, m.CLI)  # and the frozen digests refuse the changed inventory
        self.assertEqual(CLI_SOURCE.rpartition("def main():")[2].count("for platform in ('Linux', 'Darwin'):"), 1)  # the Task G harness splices main()'s one loop

    def test_the_frozen_inventory_binds_every_invocation_call_fixture_parameters_and_order(self):
        base = m.totals(m.enumerate_cells(CLI_SOURCE, cli=True))
        self.assertEqual(base, m.CLI_FROZEN)
        direct = "                f = Fixture(parent, platform, fault=fault)\n                assert f.run(direct=True)[0] == (1 if fault else 0); f.unchanged()\n"
        prior = "            for prior in ('retained', 'removed'):\n"
        mutations = {"another argument of the same run": (direct, direct.replace("direct=True", "direct=False")),
                     "another Fixture under the same run": (direct, direct.replace("fault=fault", "fault_index=1, fault=fault")),
                     "two cells swapped in order": (prior, prior.replace("('retained', 'removed')", "('removed', 'retained')"))}
        for name, (old, new) in mutations.items():
            self.assertEqual(CLI_SOURCE.count(old), 1, name)
            mutated = CLI_SOURCE.replace(old, new)
            seen = m.totals(m.enumerate_cells(mutated, cli=True))
            self.assertNotEqual(seen["invocation_digest"], base["invocation_digest"], name)
            with self.subTest(name), self.assertRaises(m.Refusal):
                m.inventory_of(mutated, m.CLI)
        swapped = m.totals(m.enumerate_cells(CLI_SOURCE.replace(prior, prior.replace("('retained', 'removed')", "('removed', 'retained')")), cli=True))
        self.assertEqual({k: v for k, v in swapped.items() if k != "invocation_digest"}, {k: v for k, v in base.items() if k != "invocation_digest"})  # only the order digest sees it

    def test_fast_subset_is_explicit_and_covers_every_distinct_seam(self):
        self.assertEqual(len(CLI_FAST_IDS), sum(len(g["ids"]) for g in json.loads(CLI_FAST)["groups"]))
        self.assertEqual((len(CLI_FAST_IDS), sum(CLI_CELLS[i]["runs"] for i in CLI_FAST_IDS)), (345, 550))
        self.assertTrue({"control/" + n for n in m.CLI_CONTROLS} <= CLI_FAST_IDS and CLI_FAST_IDS <= set(CLI_INVENTORY))
        covered = lambda family, *keep: self.assertEqual(cli_projection(CLI_INVENTORY, family, *keep), cli_projection(CLI_FAST_IDS, family, *keep), (family, keep))
        for family in m.CLI_FAMILIES:
            if family == "reset-failed" and not cli_projection(CLI_FAST_IDS, family, "platform"): continue  # Task SF: full tier only until tests/uninstall_cli_fast_cases.json lists it; once it lists any, it must cover every model
            covered(family, "platform")  # every family on every model that has it
        complete = ("present", "initial", "direct", "opposite", "bound", "definition", "ancestor-absent", "ancestor-unsearchable", "route-spelling",
                    "route-choice", "route-failure", "route-prior", "usage", "env-text", "role-alias", "tool-fault", "legacy-selection", "config-input",
                    "stopped", "rollback-unauthorized", "config-absent", "ambient-transport", "quoted-route")
        for family in complete:  # every positive state and every safety-critical refusal class: all cells, both models
            self.assertEqual({i for i in CLI_INVENTORY if m.classify_cli(i).get("family") == family},
                             {i for i in CLI_FAST_IDS if m.classify_cli(i).get("family") == family}, family)
        covered("fault", "platform", "fault")
        for kind in ("query-fail", "stop-fail", "bootout-fail", "rm-fail", "reactivate"):  # direction-specific progress: both labels
            self.assertEqual({a["index"] for a in map(m.classify_cli, CLI_FAST_IDS) if a["family"] == "fault" and a["fault"] == kind}, {"0", "1"}, kind)
        for keep in (("seam", "form"), ("seam", "action"), ("seam", "index")):
            covered("capture", "platform", *keep)  # payload shape, caller context and label on every seam
        for keep in (("count",), ("action",), ("index",)):
            covered("late", "platform", *keep)  # observation-time branch
        covered("env-key", "platform", "key")
        self.assertEqual({a["value"] for a in map(m.classify_cli, CLI_FAST_IDS) if a["family"] == "env-key"}, {"empty", "untrusted"})  # both values, each key once per model
        for keep in (("role",), ("index",)):
            covered("role-selected", "platform", *keep)
        for keep in (("fault", "side"), ("role",), ("index",)):
            covered("identity-alias", "platform", *keep)
        covered("plist-label", "kind"); covered("plist-label", "index")
        for keep in (("signal", "seam"), ("index",), ("signal",)):
            covered("signal", "platform", *keep)
        covered("caller", "platform", "fault"); covered("caller", "platform", "caller"); covered("caller", "fault", "caller")
        covered("percent-specifier", "quoting", "form"); covered("percent-specifier", "index", "form")
        covered("override", "directive", "side"); covered("override", "quoting"); covered("override", "index")
        for keep in (("section",), ("quoting",), ("index",)):
            covered("section", *keep)
        for keep in (("location",), ("index",), ("present",)):
            covered("overlay", *keep)
        covered("metadata-field", "field", "fault"); covered("metadata-field", "present"); covered("metadata-field", "index")
        covered("metadata-fault", "fault"); covered("metadata-fault", "index"); covered("metadata-override", "override")
        covered("absent-override", "override"); covered("drift", "at", "field"); covered("drift", "index")  # every drift point with every property
        covered("meta-caller", "fault"); covered("meta-caller", "caller")
        for family in ("fragment-bystander", "cached-unit", "absent-not-found"):
            self.assertTrue(cli_projection(CLI_FAST_IDS, family, "index"), family)

    def test_cli_fast_file_refuses_duplicates_unknown_ids_and_wrong_shapes(self):
        document = json.loads(CLI_FAST)
        for mutate in (lambda d: d["groups"][0]["ids"].append(d["groups"][0]["ids"][0]),
                       lambda d: d["groups"][0]["ids"].append("systemd/present/9"),
                       lambda d: d["groups"][1]["ids"].append(d["groups"][0]["ids"][0]),
                       lambda d: d["groups"][0].pop("rationale"), lambda d: d.update(schema_version=2),
                       lambda d: d["groups"][0].update(ids="x")):
            broken = copy.deepcopy(document)
            mutate(broken)
            with self.assertRaises(m.Refusal):
                m.load_fast(json.dumps(broken), CLI_INVENTORY)

    def test_shards_partition_each_tier_pin_the_controls_balance_and_stay_under_the_bound(self):
        runs = m.runs_of(CLI_SOURCE, m.CLI)
        controls = ["control/" + n for n in m.CLI_CONTROLS]
        self.assertEqual(m.CLI.counts, {"fast": 1, "full": 3})
        for tier in ("fast", "full"):
            ids = CLI_INVENTORY if tier == "full" else [i for i in CLI_INVENTORY if i in CLI_FAST_IDS]
            shards, loads = m.assign(ids, m.CLI.counts[tier], m.CLI, runs)
            self.assertEqual(shards, m.assign(ids, m.CLI.counts[tier], m.CLI, runs)[0])
            self.assertEqual(sorted(sum(shards, [])), sorted(ids))
            self.assertEqual([[i for i in shard if i in controls] for shard in shards], [controls] + [[]] * (len(shards) - 1))  # exactly one shard
            self.assertLess(max(loads) / min(loads), 1.02)
            self.assertLessEqual(max(loads), .6 * 900, "a modeled shard must stay within 60% of the unchanged 900 s bound")
            for shard in shards:
                self.assertEqual({m.classify_cli(i).get("platform") for i in shard} - {None}, {"systemd", "launchd"})
        self.assertAlmostEqual(sum(m.assign(CLI_INVENTORY, 3, m.CLI, runs)[1]), 928.7, 1)  # modeled hosted-macOS seconds of the whole suite (912.6 s plus the eight Task SF cells)

    def test_plan_defaults_to_full_and_refuses_invalid_requests(self):
        plan = lambda env: m.plan(CLI_SOURCE, CLI_FAST, env, m.CLI)
        whole = plan({})
        self.assertEqual((whole["tier"], whole["selected"], whole["inventory"]), ("full", CLI_INVENTORY, CLI_INVENTORY))
        self.assertEqual(plan({"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_SHARD": "9/9"})["selected"], CLI_INVENTORY)  # the native variables are not ours
        self.assertEqual(plan({"HERDR_UNINSTALL_CLI_TIER": "fast", "HERDR_UNINSTALL_CLI_SHARD": "1/1"})["selected"], [i for i in CLI_INVENTORY if i in CLI_FAST_IDS])
        self.assertEqual(plan({"HERDR_UNINSTALL_CLI_CELL": CLI_INVENTORY[7]})["selected"], [CLI_INVENTORY[7]])
        foreign = m.assign(CLI_INVENTORY, 3, m.CLI, m.runs_of(CLI_SOURCE, m.CLI))[0][1][0]
        bad = [{"TIER": "all"}, {"TIER": "FAST"}, {"TIER": "fast", "SHARD": "1/3"}, {"SHARD": "1/1"}, {"SHARD": "0/3"}, {"SHARD": "4/3"},
               {"SHARD": "01/3"}, {"SHARD": "1/3/1"}, {"SHARD": " 1/3"}, {"SHARD": "1/2"}, {"CELL": "nope"}, {"SHARD": "1/3", "CELL": foreign},
               {"TIER": "fast", "CELL": next(i for i in CLI_INVENTORY if i not in CLI_FAST_IDS)},
               {"TIER": ""}, {"SHARD": ""}, {"CELL": ""}, {"TIER": "full", "SHARD": "1/3", "CELL": ""}]  # a variable that is set but empty is refused; unset is the default
        for env in bad:
            with self.subTest(env), self.assertRaises(m.Refusal):
                plan({"HERDR_UNINSTALL_CLI_" + k: v for k, v in env.items()})

    def test_a_cell_is_the_one_semantic_id_not_a_counter(self):
        self.assertEqual(len(m.SUITES), 9)
        self.assertEqual(m.SUITES[:3], ("tests/test_common.sh", "tests/test_service_credentials.sh", "tests/test_install.sh"))  # the hosted install run follows credentials
        self.assertNotIn(m.UNINSTALL_CLI, m.SUITES)  # it is its own sharded job now
        self.assertEqual(len({i.lower() for i in CLI_INVENTORY}), len(CLI_INVENTORY))
        self.assertNotRegex(" ".join(CLI_INVENTORY), r"invocation|counter|INVOCATIONS")


class Evidence(unittest.TestCase):
    def write(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value if isinstance(value, str) else json.dumps(value))

    def tree(self, root, tier, mutate=None):
        for runner, runner_os in m.RUNNER_OS.items():
            def summary(mode, names):
                return {"schema_version": 2, "candidate_sha": SHA, "mode": mode, "run_id": RUN, "run_attempt": ATTEMPT,
                        "runner": {"os": runner_os, "arch": "x"}, "bash_version": "b", "python_version": "p",
                        "suites": [{"name": n, "exit": 0, "seconds": 1} for n in names], "checkout_unchanged": True, "result": "pass"}
            self.write(root / ("service-suites-%s-%s-%s" % (tier, runner, SHA)) / "summary.json", summary("suites", m.SUITES))
            for suite in m.PROFILES:
                for index in range(1, suite.counts[tier] + 1):
                    want = m.shard_expectation(tier, "%d/%d" % (index, suite.counts[tier]), SHA, RUN, ATTEMPT, runner_os, *TEXTS[suite.name], suite)
                    base = root / ("service-%s-%s-%s-s%d-%s" % (suite.artifact, tier, runner, index, SHA))
                    self.write(base / suite.evidence, m.shard_evidence(want, "".join(i + "\n" for i in want["ids"]), 0))
                    self.write(base / "summary.json", summary(suite.name, ["%s#%s-%dof%d" % (suite.source, tier, index, suite.counts[tier])]))
        if mutate:
            mutate(root, tier)

    def gate(self, tier, mutate=None, run_id=RUN, attempt=ATTEMPT):
        with tempfile.TemporaryDirectory() as tmp:
            self.tree(pathlib.Path(tmp), tier, mutate)
            m.gate(tier, SHA, run_id, attempt, tmp, TEXTS)

    def test_complete_fast_and_full_success_for_every_shard_and_os_group(self):
        for tier in ("fast", "full"):
            self.gate(tier)

    def test_the_aggregate_accepts_exactly_the_current_shard_counts(self):
        previous = {"fast": 2, "full": 8}  # the layout before the counts were sized from hosted measurements
        for tier in ("fast", "full"):
            for other in (previous[tier], m.SHARD_COUNTS[tier] - 1, m.SHARD_COUNTS[tier] + 1):
                with self.subTest("%s artifacts of a %d-shard layout" % (tier, other)), tempfile.TemporaryDirectory() as tmp:
                    with mock.patch.dict(m.SHARD_COUNTS, {tier: other}):
                        self.tree(pathlib.Path(tmp), tier)
                    with self.assertRaisesRegex(m.Refusal, "artifact set differs"):
                        m.gate(tier, SHA, RUN, ATTEMPT, tmp, TEXTS)
            with self.subTest(tier + " one shard of the next index"), self.assertRaisesRegex(m.Refusal, "artifact set differs"):
                def extra(root, _tier):
                    first = root / ("service-native-%s-ubuntu-24.04-s1-%s" % (tier, SHA))
                    shutil.copytree(first, root / ("service-native-%s-ubuntu-24.04-s%d-%s" % (tier, m.SHARD_COUNTS[tier] + 1, SHA)))
                self.gate(tier, extra)

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
            "nested setup-menu suite not required": (suites(lambda d: d["suites"].pop(4)), "suite records")}
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

    def test_uninstall_cli_evidence_failures_and_control_placement_fail_the_aggregate(self):
        def path(root, tier, runner="ubuntu-24.04", index=1):
            return root / ("service-uninstall-cli-%s-%s-s%d-%s" % (tier, runner, index, SHA)) / "uninstall-cli-shard-evidence.json"

        def edit(change, index=1, runner="ubuntu-24.04"):
            def apply(root, tier):
                target = path(root, tier, runner, min(index, m.CLI.counts[tier]))
                document = json.loads(target.read_text())
                change(document)
                target.write_text(json.dumps(document))
            return apply

        def move(cell_id):  # the cell runs in shard 2 instead of the shard that owns it: the first shard misses it, the second has one too many
            def apply(root, tier):
                first, second = (path(root, tier, "ubuntu-24.04", n) for n in (1, 2))
                for target, change in ((first, -1), (second, 1)):
                    document = json.loads(target.read_text())
                    document["completed"] = [c for c in document["completed"] if c != cell_id] if change < 0 else document["completed"] + [cell_id]
                    document["completed_count"] += change
                    target.write_text(json.dumps(document))
            return apply
        control = "control/fast-stub-equivalence"
        cases = {"missing shard": (lambda r, t: shutil.rmtree(path(r, t).parent), "artifact set differs"),
                 "evidence of the native suite": (lambda r, t: path(r, t).write_text(next(r.glob("service-native-%s-ubuntu-24.04-s1-*/native-shard-evidence.json" % t)).read_text()), "differs from the expected"),
                 "duplicate cell": (edit(lambda d: d["completed"].append(d["completed"][0])), "duplicate completed"),
                 "unexpected cell": (edit(lambda d: d["completed"].append("systemd/nonesuch")), "unexpected completed"),
                 "partial shard": (edit(lambda d: (d["completed"].pop(), d.update(completed_count=d["completed_count"] - 1))), "missing completed"),
                 "no control completed": (edit(lambda d: (d["completed"].remove(control), d.update(completed_count=d["completed_count"] - 1))), "missing completed"),
                 "control completed twice": (edit(lambda d: d["completed"].append(control)), "duplicate completed"),
                 "stale candidate": (edit(lambda d: d.update(candidate_sha="b" * 40)), "candidate_sha"),
                 "prior attempt": (edit(lambda d: d.update(run_attempt="1")), "run_attempt"),
                 "other tier": (edit(lambda d: d.update(tier="full" if d["tier"] == "fast" else "fast")), "expected tier"),
                 "other runner": (edit(lambda d: d.update(runner_os="Darwin")), "runner_os"),
                 "stale inventory": (edit(lambda d: d.update(inventory_digest="0" * 64)), "inventory_digest"),
                 "failed shard": (edit(lambda d: d.update(suite_exit=1, result="fail")), "did not pass"),
                 "wrong kind": (edit(lambda d: d.update(kind="native-install-shard")), "kind")}
        for tier in ("fast", "full"):
            for name, (mutate, reason) in cases.items():
                with self.subTest(tier + " " + name), self.assertRaisesRegex(m.Refusal, reason):
                    self.gate(tier, mutate)
        with self.subTest("a control run in a shard that does not own it"):
            first_shard = json.loads(json.dumps(m.shard_expectation("full", "1/3", SHA, RUN, ATTEMPT, "Linux", CLI_SOURCE, CLI_FAST, m.CLI)["ids"]))
            self.assertIn(control, first_shard)  # the controls are in shard 1 and in no other
            with self.assertRaisesRegex(m.Refusal, "completed cell"):
                self.gate("full", move(control))
            self.assertTrue(all(control not in m.shard_expectation("full", "%d/3" % n, SHA, RUN, ATTEMPT, "Linux", CLI_SOURCE, CLI_FAST, m.CLI)["ids"] for n in (2, 3)))

    def test_failed_empty_or_partial_cell_records_never_become_passing_evidence(self):
        want = m.shard_expectation("fast", "1/%d" % FAST_N, SHA, RUN, "1", "Linux", SOURCE, FAST)
        every = "".join(i + "\n" for i in want["ids"])
        self.assertEqual(m.shard_evidence(want, every, 0)["result"], "pass")
        for cells, code in ((every, 1), (every, 124), ("", 0), (every[:-1 - len(want["ids"][-1])], 0), (every + want["ids"][0] + "\n", 0),
                            (every + "no-such-cell\n", 0)):
            self.assertEqual(m.shard_evidence(want, cells, code)["result"], "fail")
        with self.assertRaises(m.Refusal):
            m.shard_evidence(want, every[:-1], 0)  # a torn last line is not a completed record

    def test_command_line_round_trip_plan_evidence_needs_and_gate(self):
        files = ["--source", str(ROOT / m.NATIVE), "--fast-cases", str(ROOT / "tests/native_install_fast_cases.json")]
        gate_files = ["--native-source", files[1], "--native-fast-cases", files[3],
                      "--uninstall-cli-source", str(ROOT / m.UNINSTALL_CLI), "--uninstall-cli-fast-cases", str(ROOT / "tests/uninstall_cli_fast_cases.json")]
        identity = ["--sha", SHA, "--run-id", RUN, "--run-attempt", ATTEMPT]
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()):
            tmp = pathlib.Path(directory)
            with mock.patch.dict(os.environ, {"HERDR_NATIVE_INSTALL_TIER": "fast", "HERDR_NATIVE_INSTALL_SHARD": "1/%d" % FAST_N}):
                self.assertEqual(m.main(["plan", *files, "--output", str(tmp / "plan.json")]), 0)
                self.assertEqual(m.main(["plan", *files, "--output", str(tmp / "plan.json")]), 1)  # never overwrites
            selected = json.loads((tmp / "plan.json").read_text())["selected"]
            (tmp / "cells").write_text("".join(i + "\n" for i in selected))
            shard = ["evidence", *files, *identity, "--tier", "fast", "--shard", "1/%d" % FAST_N, "--runner-os", "Linux", "--cells", str(tmp / "cells")]
            self.assertEqual(m.main([*shard, "--suite-exit", "0", "--output", str(tmp / "pass.json")]), 0)
            self.assertEqual(m.main([*shard, "--suite-exit", "1", "--output", str(tmp / "fail.json")]), 1)
            self.assertEqual([json.loads((tmp / n).read_text())["result"] for n in ("pass.json", "fail.json")], ["pass", "fail"])
            with mock.patch.dict(os.environ, {"HERDR_NEEDS_JSON": json.dumps({"a": {"result": "success"}})}):
                self.assertEqual((m.main(["needs", "a"]), m.main(["needs", "a", "b"])), (0, 1))
            cli = ["--suite", "uninstall-cli", "--source", gate_files[5], "--fast-cases", gate_files[7]]  # the uninstall CLI, a full run of one shard
            with mock.patch.dict(os.environ, {"HERDR_UNINSTALL_CLI_TIER": "full", "HERDR_UNINSTALL_CLI_SHARD": "1/3"}):
                self.assertEqual(m.main(["plan", *cli, "--output", str(tmp / "cli-plan.json")]), 0)
            plan = json.loads((tmp / "cli-plan.json").read_text())
            self.assertEqual((plan["shard_index"], plan["shard_count"], plan["runs"]), (1, 3, m.runs_of(CLI_SOURCE, m.CLI)))
            (tmp / "cli-cells").write_text("".join(i + "\n" for i in plan["selected"]))
            shard = ["evidence", *cli, *identity, "--tier", "full", "--shard", "1/3", "--runner-os", "Linux", "--cells", str(tmp / "cli-cells")]
            self.assertEqual(m.main([*shard, "--suite-exit", "0", "--output", str(tmp / "cli-pass.json")]), 0)
            self.assertEqual(m.main([*shard, "--suite-exit", "1", "--output", str(tmp / "cli-fail.json")]), 1)
            self.assertEqual(json.loads((tmp / "cli-pass.json").read_text())["kind"], "uninstall-cli-shard")
            self.tree(tmp / "artifacts", "fast")
            self.assertEqual(m.main(["gate", *gate_files, *identity, "--tier", "fast", "--artifacts", str(tmp / "artifacts")]), 0)
            self.assertEqual(m.main(["gate", *gate_files, "--sha", SHA, "--run-id", "9", "--run-attempt", ATTEMPT, "--tier", "fast",
                                     "--artifacts", str(tmp / "artifacts")]), 1)

    def test_dependency_results_must_all_be_success(self):
        names = ("candidate", "suites", "native", "uninstall_cli")
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
        self.assertIn("%d fast, %d full," % (m.SHARD_COUNTS["fast"], m.SHARD_COUNTS["full"]), " ".join(driver.replace("#", " ").split()))  # its usage comment names the counts
        for tier, name in (("fast", "service-qualification"), ("full", "service-full-qualification")):
            workflow, count = self.text(".github/workflows/%s.yml" % name), m.SHARD_COUNTS[tier]
            self.assertIn("shard: [%s]" % ", ".join(map(str, range(1, count + 1))), workflow)
            self.assertIn("native %s ${{ matrix.shard }}/%d" % (tier, count), workflow)
            self.assertIn("name: Native install, %s (${{ matrix.runner }}, shard ${{ matrix.shard }}/%d)" % (tier, count), workflow)
            self.assertIn("%d shards per OS" % count, " ".join(workflow.split()))  # the header comment names the count
            macos_jobs = 1 + count + m.CLI.counts[tier]  # the suites job, the native shards and the uninstall CLI shards, on macos-15
            self.assertIn("macOS jobs per %s: %d (" % ("push" if tier == "fast" else "run", macos_jobs), workflow)
            self.assertIn("| native %d, uninstall CLI %d |" % (count, m.CLI.counts[tier]), self.text("docs/ci-test-tiers.md"))
            self.assertIn("shard: [%s]" % ", ".join(map(str, range(1, m.CLI.counts[tier] + 1))), workflow)
            self.assertIn("uninstall-cli %s ${{ matrix.shard }}/%d" % (tier, m.CLI.counts[tier]), workflow)
            self.assertIn("service-uninstall-cli-%s-${{ matrix.runner }}-s${{ matrix.shard }}-${{ github.sha }}" % tier, workflow)
            self.assertIn("uninstall-cli-shard-evidence.json", workflow)
            self.assertRegex(workflow, r"needs: \[(decide, candidate, )?suites, native, uninstall_cli\]")
            self.assertIn(" suites native uninstall_cli" if tier == "fast" else " decide candidate suites native uninstall_cli", workflow)
            self.assertIn("--tier %s" % tier, workflow)
            self.assertIn("if: ${{ always() }}" if tier == "fast" else "if: ${{ always() && needs.decide.outputs.run != 'false' }}", workflow)
            self.assertEqual(sorted(re.findall(r"runner: \[(.*)\]", workflow)), ["macos-15, ubuntu-24.04"] * 3)
            code = "\n".join(line for line in workflow.lower().splitlines() if not line.lstrip().startswith("#"))
            for forbidden in ("continue-on-error", "retry", "rerun"):
                self.assertNotIn(forbidden, code)
        self.assertIn("workflow_dispatch", self.text(".github/workflows/service-full-qualification.yml"))
        self.assertIn("schedule", self.text(".github/workflows/service-full-qualification.yml"))

    @staticmethod
    def blocks(text, indent):  # {key: its text} for the mapping entries at exactly this indent (comments before the first entry are dropped)
        return {re.match(r"\s*([\w-]+):", b).group(1): b for b in re.split(r"(?m)^(?=%s[\w-]+:)" % (" " * indent), text) if b.lstrip()[:1] not in ("#", "")}

    def test_full_triggers_are_dispatch_schedule_and_a_push_to_the_ci_branch_only(self):
        triggers = self.blocks(self.text(".github/workflows/service-full-qualification.yml").split("\non:\n", 1)[1].split("\npermissions:", 1)[0], 2)
        self.assertEqual(sorted(triggers), ["push", "schedule", "workflow_dispatch"])
        self.assertEqual(triggers["push"], "  push:\n    branches: [ci/tailscale-native-preflight-ef9f843]\n")  # no other branch, tag, path or filter
        self.assertEqual(triggers["schedule"], '  schedule:\n    - cron: "17 3 * * *"\n')
        self.assertEqual(triggers["workflow_dispatch"], "  workflow_dispatch:\n    inputs:\n      candidate_sha:\n        description: Optional full commit SHA the selected ref "
                         "must be at (the run fails otherwise)\n        required: false\n        type: string\n")

    def test_the_marker_decision_gates_every_job_and_a_skipped_push_run_neither_fails_nor_qualifies(self):
        workflow = self.text(".github/workflows/service-full-qualification.yml")
        jobs, marker = self.blocks(workflow.split("\njobs:\n", 1)[1], 2), "[full-qualification]"
        needs = {"decide": [], "candidate": ["decide"], "suites": ["decide", "candidate"], "native": ["decide", "candidate"],
                 "uninstall_cli": ["decide", "candidate"], "gate": ["decide", "candidate", "suites", "native", "uninstall_cli"]}
        self.assertEqual(sorted(jobs), sorted(needs))
        for name, job in jobs.items():
            found = re.search(r"(?m)^    needs: \[?([^\]\n]*)\]?$", job)
            self.assertEqual(found.group(1).split(", ") if found else [], needs[name], name)
            condition = ["${{ always() && needs.decide.outputs.run != 'false' }}" if name == "gate" else "${{ needs.decide.outputs.run == 'true' }}"]
            self.assertEqual(re.findall(r"(?m)^    if: (.*)$", job), [] if name == "decide" else condition, name)  # decide always runs
        for name in ("suites", "native", "uninstall_cli", "gate"):  # a push is bound to github.sha exactly like a dispatch
            self.assertIn('--expected-sha "$GITHUB_SHA"', jobs[name])
        self.assertIn('--sha "$GITHUB_SHA" --run-id "$GITHUB_RUN_ID" --run-attempt "$GITHUB_RUN_ATTEMPT"', " ".join(jobs["gate"].split()))
        self.assertIn("needs decide candidate suites native uninstall_cli", jobs["gate"])  # a skipped or failed dependency (decide too) refuses
        decide = jobs["decide"]
        self.assertIn("run: ${{ steps.decide.outputs.run }}", decide)
        self.assertIn("HEAD_MESSAGE: ${{ github.event.head_commit.message }}", decide)
        script = textwrap.dedent(decide.split("        run: |\n", 1)[1])
        self.assertNotIn("${{", script)  # the commit message reaches the script only as data
        self.assertEqual(script.count('>> "$GITHUB_OUTPUT"'), 1)
        probe = script.replace('>> "$GITHUB_OUTPUT"', "")  # the same script with its one output line printed instead of appended: nothing is written

        def decided(event, message):  # the real script, as hosted bash runs it, with the event's name and head message
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", probe], capture_output=True, text=True,
                                  env={"PATH": "/usr/bin:/bin", "EVENT_NAME": event, "HEAD_MESSAGE": message})
            self.assertEqual(done.returncode, 0, done.stderr)  # the skip path exits 0: never a red run
            return [line for line in done.stdout.splitlines() if line.startswith("run=")]
        for event in ("workflow_dispatch", "schedule"):  # these ignore the marker and always run
            for message in ("", "no marker", marker):
                self.assertEqual(decided(event, message), ["run=true"], (event, message))
        self.assertEqual(decided("push", "ci: x\n\nbody\n%s trailing\n" % marker), ["run=true"])
        for message in ("", "ci: x", "[full-qualification", "full-qualification", "[Full-Qualification]", "[full-qualification ]", "[ full-qualification]", "[full qualification]"):
            self.assertEqual(decided("push", message), ["run=false"], message)
        release, docs = self.text(".github/workflows/release.yml"), self.text("docs/ci-test-tiers.md")
        self.assertIn('select(.event == "workflow_dispatch" or .event == "schedule")', release)  # a push run, skipped or not, is never accepted
        self.assertNotIn('"push"', release)
        for text in (marker, "A skipped push run is not qualification"):
            self.assertIn(text, " ".join(docs.split()))

    def test_check_and_mount_controls_keep_only_their_intended_triggers(self):
        check = self.text(".github/workflows/check.yml")
        triggers = self.blocks(check.split("\non:\n", 1)[1].split("\npermissions:", 1)[0], 2)
        self.assertEqual(triggers, {"push": "  push:\n    branches: [main, dev]\n", "pull_request": "  pull_request:\n"})
        mount = self.text(".github/workflows/uninstall-mount-controls.yml")
        triggers = self.blocks(mount.split("\non:\n", 1)[1].split("\npermissions:", 1)[0], 2)
        self.assertEqual(triggers, {"pull_request": "  pull_request:\n", "workflow_dispatch": "  workflow_dispatch:\n"})

    def test_check_release_and_makefile_keep_shell_suite_split(self):
        check, release, make = (self.text(p) for p in (".github/workflows/check.yml", ".github/workflows/release.yml", "Makefile"))
        self.assertIn("shell-check-ci-complement production-path-audit", check)
        self.assertIn(".github/workflows/service-full-qualification.yml", check)
        self.assertIn("--workflow service-full-qualification.yml", release)
        self.assertIn("--workflow check.yml", release)
        self.assertIn("deadline=$((SECONDS + 1800))", release)
        self.assertIn("shell-check: shell-check-ci-complement shell-check-service-suites", make)
        complement = make.split("shell-check-ci-complement:\n", 1)[1].split("\n\n", 1)[0]
        service = make.split("shell-check-service-suites:\n", 1)[1].split("\n\n", 1)[0]
        self.assertEqual(re.findall(r"tests/test_\w+\.(?:sh|py)", service), ["tests/test_native_install.sh", "tests/test_uninstall.sh", "tests/test_service_uninstall_cli.py"])
        self.assertFalse(set(re.findall(r"tests/test_\w+\.(?:sh|py)", service)) & set(re.findall(r"tests/test_\w+\.(?:sh|py)", complement)))

    def test_check_keeps_the_hosted_macos_go_job_and_its_required_real_acl_and_cli_tests(self):
        check, docs = self.text(".github/workflows/check.yml"), " ".join(self.text("docs/ci-test-tiers.md").split())
        found = re.search(r"(?ms)^  go-darwin:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)", check)
        self.assertIsNotNone(found)
        job = found.group(1)
        core = 'go test -json -count=1 ./internal/darwinacl ./internal/support >"$log" 2>"$err" && go_exit=0 || go_exit=$?'
        cli = 'go test -json -count=1 ./cmd/herdr-mobile-relay -run \'^TestSupportCommandProjectsLoadedPrivateState$\' >>"$log" 2>>"$err" || go_exit=$?'
        self.assertEqual(job.count(core), 1)
        self.assertEqual(job.count(cli), 1)
        self.assertLess(job.index(core), job.index(cli))
        self.assertIn('"cmd/herdr-mobile-relay": ["TestSupportCommandProjectsLoadedPrivateState"]', job)
        self.assertIn('expected_package_ends = ["pass"]', job)
        self.assertIn('if ends.get((path, None)) != expected_package_ends:', job)
        self.assertIn("cache: false", job)
        self.assertIn("parse_constant=reject", job)
        required = {"darwinacl": ["TestRealPlainObjectsHaveNoACLOnAnAllowedVolume"],
                    "support": ["TestLoadDarwinHostedRealACL"]}
        for package, names in required.items():
            source = "".join(path.read_text() for path in (ROOT / "internal" / package).glob("*_test.go"))
            for name in names:
                self.assertIn('"internal/%s"' % package, job)
                self.assertIn(name, job)
                self.assertIn("`%s`" % name, docs)
                self.assertRegex(source, r"(?m)^func %s\(t \*testing\.T\)" % name)
        name = "TestSupportCommandProjectsLoadedPrivateState"
        self.assertIn("`%s`" % name, docs)
        self.assertRegex(self.text("cmd/herdr-mobile-relay/main_test.go"), r"(?m)^func %s\(t \*testing\.T\)" % name)

if __name__ == "__main__":
    unittest.main()

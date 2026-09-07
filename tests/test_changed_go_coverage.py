#!/usr/bin/env python3

import importlib.util
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "scripts" / "check-changed-go-coverage.py"
SPEC = importlib.util.spec_from_file_location("changed_go_coverage", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def sample_inventory():
    return {
        "schema": 1,
        "base": "base",
        "packages": ["./internal/a"],
        "functions": [{
            "id": "function|internal/a/a.go|2|1|thing",
            "file": "internal/a/a.go",
            "line": 2,
            "column": 1,
            "name": "thing",
            "markers": ["function|internal/a/a.go|2|1|thing|hit"],
        }],
        "statements": [{
            "id": "statement|internal/a/a.go|3|2|return",
            "file": "internal/a/a.go",
            "line": 3,
            "column": 2,
            "kind": "return",
            "markers": ["statement|internal/a/a.go|3|2|return|hit"],
        }],
        "decisions": [
            {
                "id": "decision|internal/a/a.go|2|16|if",
                "file": "internal/a/a.go",
                "line": 2,
                "column": 16,
                "kind": "if",
                "markers": [
                    "decision|internal/a/a.go|2|16|if|true",
                    "decision|internal/a/a.go|2|16|if|false",
                ],
            },
            {
                "id": "decision|internal/a/a.go|5|2|switch",
                "file": "internal/a/a.go",
                "line": 5,
                "column": 2,
                "kind": "switch",
                "markers": [
                    "decision|internal/a/a.go|5|2|switch|case@6:2",
                    "decision|internal/a/a.go|5|2|switch|default",
                ],
            },
        ],
    }


class ChangedGoCoverageTests(unittest.TestCase):
    def test_changed_lines_include_only_added_result_lines(self):
        diff = """diff --git a/internal/a/a.go b/internal/a/a.go
+++ b/internal/a/a.go
@@ -2 +2,2 @@
-old
+new
+next
"""
        self.assertEqual(MODULE.changed_lines(diff), {"internal/a/a.go": {2, 3}})

    def test_changed_lines_handle_new_deleted_and_context_lines(self):
        diff = """diff --git a/new.go b/new.go
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package sample
+func added() {}
diff --git a/old.go b/old.go
--- a/old.go
+++ /dev/null
@@ -1 +0,0 @@
-package old
diff --git a/kept.go b/kept.go
--- a/kept.go
+++ b/kept.go
@@ -1,3 +1,3 @@
 package kept
-var old = 1
+var current = 1
 func same() {}
"""
        self.assertEqual(MODULE.changed_lines(diff), {"new.go": {1, 2}, "kept.go": {2}})

    def test_complete_observations_pass(self):
        inventory = sample_inventory()
        observed = {
            marker
            for category in ("functions", "statements", "decisions")
            for entry in inventory[category]
            for marker in entry["markers"]
        }
        self.assertEqual(MODULE.validate_observations(inventory, observed), [])

    def test_omitted_function_and_statement_observations_fail(self):
        inventory = sample_inventory()
        observed = {
            marker
            for category in ("functions", "statements", "decisions")
            for entry in inventory[category]
            for marker in entry["markers"]
        }
        observed.remove(inventory["functions"][0]["markers"][0])
        observed.remove(inventory["statements"][0]["markers"][0])

        failures = MODULE.validate_observations(inventory, observed)

        self.assertTrue(any("uncovered changed function" in failure for failure in failures))
        self.assertTrue(any("uncovered changed statement" in failure for failure in failures))

    def test_one_sided_boolean_decision_fails(self):
        inventory = sample_inventory()
        boolean = inventory["decisions"][0]
        observed = {
            marker
            for category in ("functions", "statements", "decisions")
            for entry in inventory[category]
            for marker in entry["markers"]
        }
        observed.remove(boolean["markers"][1])

        failures = MODULE.validate_observations(inventory, observed)

        self.assertIn(
            "uncovered changed decision outcome: internal/a/a.go:2:16:if:false",
            failures,
        )

    def test_omitted_switch_arm_fails(self):
        inventory = sample_inventory()
        switch = inventory["decisions"][1]
        observed = {
            marker
            for category in ("functions", "statements", "decisions")
            for entry in inventory[category]
            for marker in entry["markers"]
        }
        observed.remove(switch["markers"][0])

        failures = MODULE.validate_observations(inventory, observed)

        self.assertIn(
            "uncovered changed decision outcome: internal/a/a.go:5:2:switch:case@6:2",
            failures,
        )

    def test_unknown_runtime_marker_fails(self):
        inventory = sample_inventory()
        observed = {
            marker
            for category in ("functions", "statements", "decisions")
            for entry in inventory[category]
            for marker in entry["markers"]
        }
        observed.add("statement|ghost.go|1|1|return|hit")

        self.assertIn(
            "runtime emitted marker absent from AST inventory: statement|ghost.go|1|1|return|hit",
            MODULE.validate_observations(inventory, observed),
        )

    def test_duplicate_inventory_marker_fails(self):
        inventory = sample_inventory()
        duplicate = dict(inventory["statements"][0])
        duplicate["id"] = "duplicate"
        inventory["statements"].append(duplicate)

        failures = MODULE.validate_observations(inventory, set())

        self.assertTrue(any("duplicate AST inventory marker" in failure for failure in failures))

    def test_parse_runtime_markers_ignores_unrelated_test_output(self):
        output = """=== RUN TestThing
ordinary log
__HERDR_CHANGED_GO_COVERAGE__decision|a.go|2|1|if|true
--- PASS: TestThing (0.00s)
"""
        self.assertEqual(
            MODULE.parse_runtime_markers(output),
            {"decision|a.go|2|1|if|true"},
        )

    def test_parse_runtime_markers_tolerates_concurrent_output_after_marker(self):
        output = """__HERDR_CHANGED_GO_COVERAGE__statement|internal/a/a.go|3|2|return|hitpc
__HERDR_CHANGED_GO_COVERAGE__function|internal/a/a.go|8|1|defaultThing|hit
"""
        self.assertEqual(
            MODULE.parse_runtime_markers(output),
            {
                "statement|internal/a/a.go|3|2|return|hit",
                "function|internal/a/a.go|8|1|defaultThing|hit",
            },
        )


if __name__ == "__main__":
    unittest.main()

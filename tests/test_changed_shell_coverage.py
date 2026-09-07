#!/usr/bin/env python3

import importlib.util
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "scripts" / "check-changed-shell-coverage.py"
SPEC = importlib.util.spec_from_file_location("changed_shell_coverage", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ChangedShellCoverageTests(unittest.TestCase):
    def test_changed_lines_include_only_added_result_lines(self):
        diff = """diff --git a/relay/a.sh b/relay/a.sh
+++ b/relay/a.sh
@@ -2 +2,2 @@
-old
+new
+next
"""
        self.assertEqual(MODULE.changed_lines(diff), {"relay/a.sh": {2, 3}})

    def test_trace_observations_record_hits_and_command_results(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "relay" / "a.sh"
            source.parent.mkdir()
            source.write_text("one\ntwo\n", encoding="utf-8")
            trace = (
                f"+HERDR_XTRACE|7|{source}|1|0|false\n"
                f"+HERDR_XTRACE|7|{source}|2|1|true\n"
                f"+HERDR_XTRACE|7|{source}|2|0|echo done\n"
            )
            hits, statuses = MODULE.trace_observations(trace, root)
        self.assertEqual(hits, {("relay/a.sh", 1), ("relay/a.sh", 2)})
        self.assertEqual(statuses[("relay/a.sh", 1)], {1})
        self.assertEqual(statuses[("relay/a.sh", 2)], {0})

    def test_source_inventory_finds_changed_function_and_decision_lines(self):
        lines = [
            "thing() {",
            "    if [ -n \"$value\" ]; then",
            "        return 0",
            "    fi",
            "}",
            "while :; do",
            "    break",
            "done",
        ]
        changed = {"relay/a.sh": set(range(1, len(lines) + 1))}
        functions, decisions = MODULE.source_inventory(changed, {"relay/a.sh": lines})
        self.assertEqual(functions, {("relay/a.sh", "thing")})
        self.assertEqual(decisions, {("relay/a.sh", 2), ("relay/a.sh", 6)})

    def test_source_inventory_finds_changed_case_arm_and_arithmetic_for(self):
        lines = [
            "case \"$value\" in",
            "    added) echo added ;;",
            "    *) echo other ;;",
            "esac",
            "for ((attempt = 1; attempt <= 2; attempt++)); do",
            "    :",
            "done",
        ]
        changed = {"relay/a.sh": {2, 5}}

        _, decisions = MODULE.source_inventory(changed, {"relay/a.sh": lines})

        self.assertEqual(decisions, {("relay/a.sh", 2), ("relay/a.sh", 5)})

    def test_changed_case_header_represents_the_whole_case_once(self):
        lines = [
            "case \"$value\" in",
            "    one) echo one ;;",
            "    two) echo two ;;",
            "esac",
        ]
        changed = {"relay/a.sh": {1, 2, 3, 4}}

        _, decisions = MODULE.source_inventory(changed, {"relay/a.sh": lines})

        self.assertEqual(decisions, {("relay/a.sh", 1)})

    def test_line_specs_expand_ranges(self):
        self.assertEqual(MODULE.expand_line_specs(["1", "3-5"]), {1, 3, 4, 5})

    def test_generated_installer_function_copy_maps_to_production_source(self):
        source = "/tmp/herdr-install-test.example/install-functions.sh"
        self.assertEqual(MODULE.relative_source(source, ROOT), "install.sh")

    def test_generated_plugin_function_copy_maps_to_production_source(self):
        source = "/tmp/herdr-plugin-build-coverage.example/plugin-build-functions.sh"
        self.assertEqual(MODULE.relative_source(source, ROOT), "relay/plugin-build.sh")

    def test_generated_service_installer_function_copies_map_to_production_sources(self):
        launchd = "/tmp/herdr-supervisor-service.example/install-service-functions.sh"
        systemd = "/tmp/herdr-supervisor-service.example/install-systemd-functions.sh"
        self.assertEqual(MODULE.relative_source(launchd, ROOT), "relay/install-service.sh")
        self.assertEqual(MODULE.relative_source(systemd, ROOT), "relay/install-systemd-user-service.sh")

    def test_generated_hostname_fragment_maps_to_production_source(self):
        source = "/tmp/herdr-hostname-coverage.example/change-hostname-fragment.sh"
        self.assertEqual(MODULE.relative_source(source, ROOT), "relay/change-hostname.sh")

    def test_generated_package_repo_script_maps_to_production_source(self):
        source = "/tmp/herdr-release-script-test.example/package-repo/scripts/package-release.sh"
        self.assertEqual(MODULE.relative_source(source, ROOT), "scripts/package-release.sh")

    def test_copied_uninstall_scripts_map_to_production_sources(self):
        uninstall = "/tmp/herdr-uninstall-test.example/relay/uninstall.sh"
        common = "/tmp/herdr-uninstall-test.example/relay/common.sh"
        self.assertEqual(MODULE.relative_source(uninstall, ROOT), "relay/uninstall.sh")
        self.assertEqual(MODULE.relative_source(common, ROOT), "relay/common.sh")

    def test_awk_program_terminator_is_not_a_shell_decision(self):
        self.assertFalse(MODULE.is_decision("' \"$service_file\" > \"$service_temp\" ||"))

    def test_source_inventory_ignores_multiline_single_quoted_program(self):
        lines = [
            "if ! awk '",
            "    $1 != \"TOKEN\" &&",
            "    $1 != \"SECRET\"",
            "' input; then",
            "    return 1",
            "fi",
        ]
        changed = {"relay/a.sh": set(range(1, len(lines) + 1))}

        _, decisions = MODULE.source_inventory(changed, {"relay/a.sh": lines})

        self.assertEqual(decisions, {("relay/a.sh", 1)})

    def test_validate_accepts_traced_zero_and_nonzero_outcomes(self):
        changed = {"relay/a.sh": {1}}
        inventory = {
            "decisions": [{
                "file": "relay/a.sh",
                "line": 1,
                "outcomes": [
                    {"name": "yes", "status": 0},
                    {"name": "no", "status": "nonzero"},
                ],
            }],
            "nonExecutable": {},
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "relay" / "a.sh"
            source.parent.mkdir()
            source.write_text("if true; then\n", encoding="utf-8")
            result = MODULE.validate(
                root,
                changed,
                {("relay/a.sh", 1)},
                {("relay/a.sh", 1): {0, 1}},
                inventory,
            )
        self.assertEqual(result[0], [])

    def test_validate_rejects_omitted_line_function_and_decision_observations(self):
        changed = {"relay/a.sh": {1, 2, 3}}
        inventory = {"nonExecutable": {}, "functions": [], "decisions": []}
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "relay" / "a.sh"
            source.parent.mkdir()
            source.write_text("thing() {\n    if true; then echo yes; fi\n}\n", encoding="utf-8")
            failures = MODULE.validate(root, changed, set(), {}, inventory)[0]

        self.assertTrue(any("uncovered changed executable shell line" in failure for failure in failures))
        self.assertIn("changed shell function missing from inventory: relay/a.sh:thing", failures)
        self.assertIn("changed shell decision missing from inventory: relay/a.sh:2", failures)

    def test_validate_rejects_one_sided_decision_and_out_of_function_evidence(self):
        changed = {"relay/a.sh": {1, 2, 3}}
        inventory = {
            "nonExecutable": {"relay/a.sh": ["1", "3"]},
            "functions": [{"file": "relay/a.sh", "name": "thing", "evidence": "relay/a.sh:4"}],
            "decisions": [{
                "file": "relay/a.sh",
                "line": 2,
                "outcomes": [
                    {"name": "yes", "status": 0},
                    {"name": "no", "status": "nonzero"},
                ],
            }],
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "relay" / "a.sh"
            source.parent.mkdir()
            source.write_text("thing() {\n    if true; then echo yes; fi\n}\necho outside\n", encoding="utf-8")
            failures = MODULE.validate(
                root,
                changed,
                {("relay/a.sh", 2), ("relay/a.sh", 4)},
                {("relay/a.sh", 2): {0}},
                inventory,
            )[0]

        self.assertTrue(any("function evidence is outside changed function" in failure for failure in failures))
        self.assertTrue(any("decision outcome status was not traced" in failure for failure in failures))


if __name__ == "__main__":
    unittest.main()

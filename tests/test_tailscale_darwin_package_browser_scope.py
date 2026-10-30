#!/usr/bin/env python3
"""Offline scope regression against the integrated checkout and frozen installed commit."""
import hashlib
import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("darwin_scope", ROOT / "tests/test_tailscale_darwin_package_browser.py")
RUNNER = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = RUNNER
SPEC.loader.exec_module(RUNNER)
INSTALLED_COMMIT = "59eda0d83a785822696da03461601eca906231c6"
OLD_CANDIDATE = "9aed9d0464ed28e241608cb9365d26f064a31cf9"


def paths(*args):
    return subprocess.check_output(["git", "diff", "--name-only", *args, "--"], cwd=ROOT, timeout=10).decode().splitlines()


class CandidateScopeIntegrationTest(unittest.TestCase):
    def test_integrated_scope(self):
        allowed = set(RUNNER.ALLOWED)
        installed = set(paths(RUNNER.SCOPE_BASE, INSTALLED_COMMIT)) - allowed
        self.assertEqual(len(installed), 275)
        self.assertEqual(hashlib.sha256(("\n".join(sorted(installed)) + "\n").encode()).hexdigest(),
                         RUNNER.INSTALLED_PATH_SET_SHA256)
        # Review the index that will be committed, not a worktree or previous HEAD.
        changed = paths("--cached", RUNNER.SCOPE_BASE)
        integrated = set(changed) - allowed - installed
        self.assertEqual(len(changed), len(set(changed)))
        self.assertFalse(integrated)
        self.assertTrue(installed <= set(changed))
        self.assertTrue(set(RUNNER.REQUIRED) <= set(changed))
        self.assertTrue(RUNNER.candidate_scope_allowed(changed))
        self.assertTrue(RUNNER.candidate_scope_allowed(list(reversed(changed))))
        self.assertTrue(RUNNER.candidate_scope_allowed(paths(RUNNER.SCOPE_BASE, OLD_CANDIDATE)))
        self.assertFalse(RUNNER.candidate_scope_allowed([p for p in changed if p != RUNNER.REQUIRED[0]]))
        self.assertFalse(RUNNER.candidate_scope_allowed([p for p in changed if p != sorted(installed)[0]]))
        self.assertFalse(RUNNER.candidate_scope_allowed([*changed, "unlisted/integration-path.txt"]))
        self.assertEqual(RUNNER.ALLOWED.count("frontend/tests/unit/store-last-known.test.ts"), 1)
        self.assertFalse(RUNNER.candidate_scope_allowed(
            [*changed, "frontend/tests/unit/store-last-known-extra.test.ts"]))
        self.assertFalse(RUNNER.candidate_scope_allowed([*changed, changed[0]]))


if __name__ == "__main__":
    unittest.main()

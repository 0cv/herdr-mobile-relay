#!/usr/bin/env python3
"""Refusal tests for exact Git checkout provenance verification."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "checkout_provenance", ROOT / "scripts" / "verify-checkout-provenance.py"
)
assert SPEC is not None and SPEC.loader is not None
PROVENANCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROVENANCE)


class GitFixture:
    def __init__(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="checkout-provenance-")
        self.base = Path(self.temporary.name)
        self.root = self.base / "source"
        self.root.mkdir()
        self.env = {
            "PATH": os.environ.get("PATH", os.defpath),
            "HOME": str(self.base),
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0",
            "LC_ALL": "C",
        }
        self.git("-c", "init.defaultBranch=main", "init", "-q")
        self.git("config", "core.filemode", "true")
        (self.root / "src").mkdir()
        (self.root / "src" / "run.sh").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        (self.root / "src" / "run.sh").chmod(0o755)
        (self.root / "data.txt").write_text("declared source\n", encoding="utf-8")
        (self.root / ".gitignore").write_text("*.ignored\n", encoding="utf-8")
        (self.root / "link").symlink_to("data.txt")
        self.commit()

    def git(self, *args):
        result = subprocess.run(
            ["git", *args],
            cwd=self.root,
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode != 0:
            raise AssertionError(f"fixture git command failed: {args!r}")
        return result.stdout.decode("ascii").strip()

    def commit(self):
        self.git("add", "-A")
        self.git(
            "-c", "user.name=Checkout Provenance Test",
            "-c", "user.email=checkout-provenance@example.invalid",
            "commit", "-qm", "fixture source",
        )

    def close(self):
        self.temporary.cleanup()


class CheckoutProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.fixture = GitFixture()
        self.addCleanup(self.fixture.close)

    def test_clean_tree_matches_commit_and_exact_membership(self):
        evidence = PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))
        self.assertEqual(evidence["commit"], self.fixture.git("rev-parse", "HEAD"))
        self.assertEqual(evidence["files"], 4)
        self.assertRegex(evidence["source_sha256"], r"^[0-9a-f]{64}$")

    def test_wrong_event_commit_is_refused(self):
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "expected source commit"):
            PROVENANCE.verify(self.fixture.root, "0" * 40)

    def test_tracked_byte_change_is_refused(self):
        (self.fixture.root / "data.txt").write_text("changed source\n", encoding="utf-8")
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "not clean"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_tracked_mode_change_is_refused(self):
        (self.fixture.root / "data.txt").chmod(0o755)
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "not clean"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_assume_unchanged_cannot_hide_modified_bytes(self):
        self.fixture.git("update-index", "--assume-unchanged", "data.txt")
        (self.fixture.root / "data.txt").write_text("changed source\n", encoding="utf-8")
        self.assertEqual(self.fixture.git("status", "--porcelain"), "")
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "tracked source bytes differ"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_assume_unchanged_cannot_hide_mode_change(self):
        self.fixture.git("update-index", "--assume-unchanged", "data.txt")
        (self.fixture.root / "data.txt").chmod(0o755)
        self.assertEqual(self.fixture.git("status", "--porcelain"), "")
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "tracked source file mode differs"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_untracked_source_is_refused(self):
        (self.fixture.root / "new-source.txt").write_text("undeclared\n", encoding="utf-8")
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "not clean"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_ignored_untracked_source_is_refused(self):
        (self.fixture.root / "private.ignored").write_text("undeclared\n", encoding="utf-8")
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "ignored files"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))

    def test_committed_escaping_symlink_is_refused(self):
        outside = self.fixture.base / "outside.txt"
        outside.write_text("outside source\n", encoding="utf-8")
        link = self.fixture.root / "link"
        link.unlink()
        link.symlink_to("../outside.txt")
        self.fixture.commit()
        with self.assertRaisesRegex(PROVENANCE.ProvenanceError, "symlink escapes"):
            PROVENANCE.verify(self.fixture.root, self.fixture.git("rev-parse", "HEAD"))


if __name__ == "__main__":
    unittest.main(verbosity=2)

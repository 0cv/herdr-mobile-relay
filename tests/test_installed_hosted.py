"""Pure offline validation. Never execute Go or set the hosted opt-in."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("installed_hosted", ROOT / "scripts/ci/installed_hosted.py")
hosted = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hosted)


def events(group=hosted.GROUP):
    result = [{"Action": "start"}]
    for name in group["tests"]:
        result += [{"Action": "run", "Test": name}, {"Action": "pass", "Test": name}]
    result.append({"Action": "pass"})
    return [dict(event, Package=group["package"]) for event in result]


def stream(items):
    return "\n".join(json.dumps(item) for item in items)


class HostedEvidence(unittest.TestCase):
    def test_fixed_distinct_selection(self):
        args = hosted.command("normal")
        self.assertIn("-tags=herdr_installed_inert_test,herdr_installed_hosted_test", args)
        self.assertEqual(args[-1], "^(TestInstalledHostedRealRunBindFailure|TestInstalledHostedRealRunShutdownRestart|TestInstalledHostedRealRunCancelBeforeServe)$")
        self.assertNotIn("-race", args)
        self.assertIn("-race", hosted.command("race"))
        for group in hosted.evidence.INVENTORY:
            self.assertFalse(set(group["tests"]) & set(hosted.GROUP["tests"]))
            self.assertNotIn("herdr_installed_hosted_test", " ".join(hosted.evidence.command(group, "normal")))

    def test_cleanup_retains_join_source_policy(self):
        # Source policy only, not behavioral evidence of cleanup or termination.
        source = (ROOT / "internal/app/installed_hosted_test.go").read_text()
        start = source.index("\t\tcleanup := func() {")
        end = source.index("\t\tt.Cleanup(cleanup)", start)
        cleanup = source[start:end]
        # Pin the full control flow: no timeout return/skip/fatal before joining.
        code = "\n".join(line for line in cleanup.splitlines()
                         if not line.lstrip().startswith("//"))
        self.assertEqual(code, """\t\tcleanup := func() {
\t\t\tcancel()
\t\t\treleaseServe()
\t\t\tfinish()
\t\t\tif !completed {
\t\t\t\tselect {
\t\t\t\tcase <-done:
\t\t\t\t\tcompleted = true
\t\t\t\tcase <-time.After(35 * time.Second):
\t\t\t\t\tt.Error("Run cleanup did not complete")
\t\t\t\t\t<-done
\t\t\t\t\tcompleted = true
\t\t\t\t}
\t\t\t}
\t\t}""")
        # Testing cleanups are LIFO: restoration/root removal registered first.
        self.assertLess(source.index("IsolateInstalledFixture(t)"), start)
        self.assertLess(source.index("t.Cleanup(func() { http.DefaultTransport = previousTransport })"), start)
        self.assertLess(source.index("tailscalecli.NewInstalledInertFixture(t)"), start)
        for mode in ("normal", "race"):
            args = hosted.command(mode)  # Construct only; never execute.
            self.assertEqual([arg for arg in args if arg.startswith("-timeout")],
                             ["-timeout=3m"])

    def test_required_events(self):
        good = events()
        self.assertTrue(hosted.evidence.validate(stream(good).splitlines(), 0, hosted.GROUP))
        bad = [good[:-1], good[:1] + good[3:], good + [good[-1]],
               [dict(e, Action="skip") if e.get("Test") == hosted.GROUP["tests"][0] and e["Action"] == "pass" else e for e in good],
               [dict(e, Package="other") for e in good]]
        for items in bad:
            with self.subTest(items=items):
                self.assertFalse(hosted.evidence.validate(stream(items).splitlines(), 0, hosted.GROUP))
        for name in hosted.GROUP["tests"]:
            missing = [e for e in good if e.get("Test") != name]
            self.assertFalse(hosted.evidence.validate(stream(missing).splitlines(), 0, hosted.GROUP))
        self.assertFalse(hosted.evidence.validate(stream(good).splitlines(), 1, hosted.GROUP))

    def test_grouped_matrix_exactness_and_privacy(self):
        self.assertEqual(len(hosted.GROUPS), 2)
        self.assertEqual(sum(len(g["tests"]) for g in hosted.GROUPS), 6)
        for system, mode in (("Linux", "normal"), ("Linux", "race"), ("Darwin", "normal")):
            for failed in (None, 0, 1):
                with tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "receipt"
                    calls = []
                    def fake(args):
                        index = len(calls)
                        group = hosted.GROUPS[index]
                        self.assertEqual(args, hosted.command(mode, group))
                        self.assertIn("-timeout=3m", args)
                        self.assertIn("-tags=herdr_installed_inert_test,herdr_installed_hosted_test", args)
                        calls.append(args)
                        rows = events(group)
                        rows.insert(1, dict(Package=group["package"], Action="output", Output="SECRET /private/token"))
                        return stream(rows), int(index == failed)
                    argv = ["--mode", mode, "--os", system, "--sha", "a" * 40,
                            "--run-id", "1", "--attempt", "2", "--evidence", str(path)]
                    with patch.object(hosted, "authorized", return_value=True), patch.object(hosted.platform, "system", return_value=system), patch.dict(hosted.os.environ, {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "2"}):
                        self.assertEqual(hosted.main(argv, fake), int(failed is not None))
                    self.assertEqual(len(calls), 2)
                    for secret in ("SECRET", "/private", directory, "Output"):
                        self.assertNotIn(secret, path.read_text())
        for group in hosted.GROUPS:
            good = events(group)
            variants = [good[:-1], good + [good[-1]], good[:3] + [good[2]] + good[3:],
                        [dict(e, Package="unknown") for e in good]]
            for name in group["tests"]:
                variants.append([e for e in good if e.get("Test") != name])
            for extra in ("TestUnexpected", group["tests"][0] + "/unexpected"):
                variants.append(good[:-1] + [dict(Package=group["package"], Test=extra, Action=a) for a in ("run", "pass")] + good[-1:])
            for action in ("fail", "skip"):
                variants.append(good[:2] + [dict(good[2], Action=action)] + good[2:])
            for rows in variants:
                self.assertFalse(hosted.evidence.validate(stream(rows).splitlines(), 0, group))
            for malformed in ("{", "null", stream(good) + "\n{"):
                self.assertFalse(hosted.evidence.validate(malformed.splitlines(), 0, group))
            for system in ("Darwin", "Linux"):
                for inert in hosted.evidence.selection(system):
                    self.assertFalse(set(inert["tests"]) & set(group["tests"]))
        source = (ROOT / "internal/localcontrol/installed_endpoint_hosted_test.go").read_text()
        self.assertTrue(source.startswith("//go:build (linux || darwin) && herdr_installed_inert_test && herdr_installed_hosted_test"))
        for name in hosted.GROUPS[1]["tests"]:
            self.assertIn("func " + name + "(t *testing.T) {\n\trequireInstalledEndpointHosted(t)", source)

    def test_wrong_os_mode_no_dispatch(self):
        for system, requested, mode in (("Darwin", "Linux", "normal"), ("Darwin", "Darwin", "race"), ("Windows", "Linux", "normal")):
            with tempfile.TemporaryDirectory() as directory, patch.object(hosted, "authorized", return_value=True), patch.object(hosted.platform, "system", return_value=system):
                path = Path(directory) / "receipt"
                argv = ["--mode", mode, "--os", requested, "--sha", "a" * 40,
                        "--run-id", "1", "--attempt", "2", "--evidence", str(path)]
                self.assertEqual(hosted.main(argv, lambda _: self.fail("dispatched")), 1)
                self.assertFalse(path.exists())

    def test_driver_guard_receipt_and_failure(self):
        sha = "a" * 40
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            args = ["--mode", "normal", "--os", "Linux", "--sha", sha,
                    "--run-id", "1", "--attempt", "2", "--evidence", str(path)]
            def forbidden(_):
                self.fail("guard dispatched execution")
            with patch.object(hosted, "authorized", return_value=False):
                self.assertEqual(hosted.main(args, forbidden), 1)
                self.assertFalse(path.exists())
            # Synthetic driver policy only; no environment opt-in and no subprocess.
            with patch.object(hosted, "authorized", return_value=True), \
                 patch.object(hosted.platform, "system", return_value="Linux"), \
                 patch.dict(hosted.os.environ, {"GITHUB_SHA": sha, "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "2"}):
                def fake(command):
                    group = next(g for g in hosted.GROUPS if g["package"] == command[-3])
                    return stream(events(group)), 0
                self.assertEqual(hosted.main(args, fake), 0)
                receipt = json.loads(path.read_text())
                self.assertEqual([r["required_tests"] for r in receipt["invocations"]], [g["tests"] for g in hosted.GROUPS])
                self.assertEqual(receipt["schema_version"], 2)
                self.assertEqual(receipt["result"], "pass")
                self.assertEqual(set(receipt), {"schema_version", "suite", "candidate_sha", "run_id", "run_attempt", "os", "mode", "invocations", "result"})
                self.assertEqual(hosted.main(args, forbidden), 1)  # no stale overwrite
                path.unlink()
                self.assertEqual(hosted.main(args, lambda _: (stream(events()[:-1]), 0)), 1)
                self.assertEqual(json.loads(path.read_text())["result"], "fail")


if __name__ == "__main__":
    unittest.main()

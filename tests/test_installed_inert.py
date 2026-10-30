"""Offline policy tests with fake executors; never launch Go or set hosted markers."""
import importlib.util
import contextlib
import io
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch
import copy
import hashlib

spec = importlib.util.spec_from_file_location("installed_inert", Path(__file__).resolve().parents[1] / "scripts/ci/installed_inert.py")
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)


def events(group):
    package = group["package"]
    rows = [{"Package": package, "Action": "start"}]
    for name in group["tests"]:
        rows.extend({"Package": package, "Test": name, "Action": action} for action in ("run", "pass"))
    rows.append({"Package": package, "Action": "pass"})
    return rows


def encoded(rows):
    return "\n".join(json.dumps(row) for row in rows)


# Deliberately distinct from every approved public identity/receipt value.
CANARIES = ("ICI003_SECRET_7e9c2a61", "/private/ici003-7e9c2a61/token",
            "ICI003_RAW_OUTPUT_7e9c2a61")


# Independent literal oracle: never copied from the runner's promotion group.
PROMOTION_GROUP = {
    "package": "github.com/0cv/herdr-mobile-relay/cmd/herdr-mobile-relay",
    "tagged": True,
    "tests": [
        "TestInstalledProductionPromotionInert",
        "TestInstalledProductionPromotionInert/owned_update_and_scoped_removal",
        "TestInstalledProductionPromotionInert/uncertain_stop_retains_authority",
        "TestInstalledProductionPromotionInert/consented_missing_route_removal",
    ],
}


def expected_receipt(system, failed_index, mode="normal"):
    return dict(schema_version=1, candidate_sha="a" * 40, run_id="123",
                run_attempt="2", os=system, mode=mode,
                result="pass" if failed_index is None else "fail",
                invocations=[
                    dict(package=g["package"], tagged=g["tagged"],
                         required_tests=list(g["tests"]),
                         result="fail" if i == failed_index else "pass")
                    for i, g in enumerate(ci.selection(system))])


def assert_minimized(case, actual, expected, canaries):
    # Boolean assertions intentionally never render a full receipt or secret.
    def walk(value):
        if isinstance(value, dict):
            case.assertTrue("Output" not in value, "raw Output field retained")
            for child in value.values():
                walk(child)
        elif isinstance(value, list):
            for child in value:
                walk(child)
    walk(actual)
    public = json.dumps(expected, sort_keys=True)
    serialized = json.dumps(actual, sort_keys=True)
    for canary in canaries:
        case.assertTrue(canary not in public, "canary overlaps approved metadata")
        case.assertTrue(canary not in serialized, "sensitive canary retained")
    # Canonical JSON equality also distinguishes bool from int, unlike dict ==.
    case.assertTrue(serialized == public, "receipt differs from exact projection")


# Synthetic observation data only; never install these values in os.environ.
def context_values(runner_os):
    return {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted",
            "RUNNER_OS": runner_os, "GITHUB_SHA": "a" * 40,
            "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2"}


def admission_cases():
    for system, runner_os in (("Linux", "Linux"), ("Darwin", "macOS")):
        valid = context_values(runner_os)
        yield system + "-matching", system, valid, True
        yield system + "-absent", system, {}, False
        for key, values in (
                ("GITHUB_ACTIONS", (None, "", "false", "True", "1", "true ")),
                ("RUNNER_ENVIRONMENT", (None, "", "self-hosted", "github-hosted ")),
                ("RUNNER_OS", (None, "", "macOS" if system == "Linux" else "Linux",
                               "Darwin", "macos", "Windows", runner_os + " "))):
            for value in values:
                observed = dict(valid)
                if value is None:
                    observed.pop(key)
                else:
                    observed[key] = value
                yield f"{system}-{key}-{value!r}", system, observed, False
    for system in ("Windows", "FreeBSD", "linux"):
        for runner_os in (None, "Linux"):
            yield f"unsupported-{system}-{runner_os}", system, context_values(runner_os), False


class EvidenceTests(unittest.TestCase):
    def assert_main_refused_without_effects(self, system, context, overrides=None, parser_error=False):
        for preexisting in (False, True):
            with self.subTest(preexisting=preexisting), tempfile.TemporaryDirectory() as root:
                path = Path(root) / "receipt.json"
                sentinel = b"existing evidence sentinel\n"
                if preexisting:
                    path.write_bytes(sentinel)
                    before = path.stat()
                options = {"--mode": "normal", "--os": "Darwin" if system == "Darwin" else "Linux",
                           "--sha": "a" * 40, "--run-id": "123", "--attempt": "2"}
                if overrides:
                    options.update(overrides)
                argv = [part for key, value in options.items() if value is not None for part in (key, value)]
                argv += ["--evidence", str(path)]
                def forbidden(*args, **kwargs):
                    self.fail("refused context reached an effect")
                # Mock observations, not process/child environment contents. The
                # actual admission and identity comparison logic remains active.
                with patch.object(ci.platform, "system", return_value=system), \
                     patch.object(ci.os.environ, "get", side_effect=context.get), \
                     patch("builtins.open", side_effect=forbidden) as opened, \
                     patch.object(ci.os, "chmod", side_effect=forbidden) as chmod, \
                     patch.object(ci, "run_suite", side_effect=forbidden) as dispatched, \
                     patch.object(ci.sys, "stderr"):
                    if parser_error:
                        with self.assertRaises(SystemExit) as stopped:
                            ci.main(argv, forbidden)
                        self.assertEqual(stopped.exception.code, 2)
                    else:
                        self.assertEqual(ci.main(argv, forbidden), 1)
                    opened.assert_not_called()
                    chmod.assert_not_called()
                    dispatched.assert_not_called()
                if preexisting:
                    self.assertEqual(path.read_bytes(), sentinel)
                    self.assertEqual(path.stat().st_mode, before.st_mode)
                    self.assertEqual(path.stat().st_mtime_ns, before.st_mtime_ns)
                else:
                    self.assertFalse(path.exists())

    def test_hosted_context_predicate(self):
        for label, system, context, expected in admission_cases():
            with self.subTest(label=label), \
                 patch.object(ci.platform, "system", return_value=system), \
                 patch.object(ci.os.environ, "get", side_effect=context.get):
                self.assertIs(ci.authorized(), expected)

    def test_main_hosted_context_refusals(self):
        for label, system, context, admitted in admission_cases():
            if not admitted:
                with self.subTest(label=label):
                    self.assert_main_refused_without_effects(system, context)

    def test_main_identity_binding_refusals(self):
        for system, runner_os in (("Linux", "Linux"), ("Darwin", "macOS")):
            for key, values in (
                    ("GITHUB_SHA", (None, "", "A" * 40, "a" * 39, "b" * 40)),
                    ("GITHUB_RUN_ID", (None, "", "0", "0123", "124")),
                    ("GITHUB_RUN_ATTEMPT", (None, "", "0", "02", "3"))):
                for value in values:
                    context = context_values(runner_os)
                    if value is None:
                        context.pop(key)
                    else:
                        context[key] = value
                    with self.subTest(system=system, key=key, value=value):
                        self.assert_main_refused_without_effects(system, context)

    def test_main_argument_os_and_mode_refusals(self):
        for system, runner_os in (("Linux", "Linux"), ("Darwin", "macOS")):
            for key, option, values in (
                    ("GITHUB_SHA", "--sha", ("", "A" * 40, "a" * 39, "a" * 40 + "\n")),
                    ("GITHUB_RUN_ID", "--run-id", ("", "0", "01", "-1", "123\n")),
                    ("GITHUB_RUN_ATTEMPT", "--attempt", ("", "0", "02", "-1", "2\n"))):
                for value in values:
                    context = context_values(runner_os)
                    context[key] = value  # Matching malformed metadata cannot bypass grammar.
                    with self.subTest(system=system, option=option, value=value):
                        self.assert_main_refused_without_effects(system, context, {option: value})
                with self.subTest(system=system, missing=option):
                    self.assert_main_refused_without_effects(
                        system, context_values(runner_os), {option: None}, parser_error=True)
            context = context_values(runner_os)
            for overrides, parser_error in (
                    ({"--os": "Darwin" if system == "Linux" else "Linux"}, False),
                    ({"--os": "Windows"}, True), ({"--mode": "unknown"}, True)):
                with self.subTest(system=system, overrides=overrides):
                    self.assert_main_refused_without_effects(system, context, overrides, parser_error)
            if system == "Darwin":
                self.assert_main_refused_without_effects(system, context, {"--mode": "race"})

    def test_fail_closed(self):
        group = ci.INVENTORY[1]
        good = events(group)
        self.assertTrue(ci.validate(encoded(good).splitlines(), 0, group))
        variants = {"empty": [], "truncated": good[:-1], "missing-parent": good[:1] + good[3:],
                    "missing-leaf": good[:3] + good[5:], "duplicate-test": good[:3] + [good[2]] + good[3:],
                    "duplicate-package": good + [good[-1]], "missing-start": good[1:]}
        for action in ("skip", "fail"):
            rows = [dict(r) for r in good]
            rows[4]["Action"] = action
            variants[action] = rows
        rows = [dict(r) for r in good]
        rows[4]["Package"] = "impostor"
        variants["cross-package"] = rows
        rows = [dict(r) for r in good]
        rows[3]["Test"] += "-renamed"
        rows[4]["Test"] += "-renamed"
        variants["renamed-leaf"] = rows
        for label, rows in variants.items():
            with self.subTest(label=label):
                self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))
        for malformed in ("{", "[]", "null", "", '{"Action":"fail","Action":"pass"}', '{"Elapsed":NaN}', encoded(good) + "\n{"):
            self.assertFalse(ci.validate(malformed.splitlines(), 0, group))
        self.assertFalse(ci.validate(encoded(good).splitlines(), 1, group))
        rows = good[:-1] + [{"Package": group["package"], "Test": group["tests"][0] + "/new", "Action": "skip"}] + good[-1:]
        self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))
        rows[-2]["Action"] = "fail"
        self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))

    def test_promotion_events_fail_closed(self):
        group = PROMOTION_GROUP
        good = events(group)
        self.assertTrue(ci.validate(encoded(good).splitlines(), 0, group))
        for name in group["tests"]:
            missing = [row for row in good if row.get("Test") != name]
            renamed = [dict(row, Test=name + "_renamed") if row.get("Test") == name else dict(row)
                       for row in good]
            variants = {"missing": missing, "renamed": renamed}
            for action in ("skip", "fail"):
                variants[action] = [dict(row, Action=action)
                                    if row.get("Test") == name and row["Action"] == "pass" else dict(row)
                                    for row in good]
            for label, rows in variants.items():
                with self.subTest(name=name, label=label):
                    self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))
        for extra in ("TestUnexpectedPromotion", "TestInstalledProductionPromotionInert/extra",
                      "TestInstalledProductionPromotionInert/owned_update_and_scoped_removal/extra"):
            rows = good[:-1] + [dict(Package=group["package"], Test=extra, Action=action)
                               for action in ("run", "pass")] + good[-1:]
            with self.subTest(extra=extra):
                self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))
        for status in (1, -9):
            self.assertFalse(ci.validate(encoded(good).splitlines(), status, group))

    def test_promotion_failure_log_uses_only_frozen_identities(self):
        group = ci.selection("Linux")[-1]
        rows = events(group)
        leaf = group["tests"][1]
        rows = [row for row in rows if row.get("Test") != leaf]
        rows.insert(1, {"Package": group["package"], "Action": "output", "Output": CANARIES[0], "Test": CANARIES[1]})
        output = io.StringIO()
        def failing(_):
            return encoded(rows), 1
        with patch.object(ci.platform, "system", return_value="Linux"), contextlib.redirect_stderr(output):
            results = ci.run_suite("normal", failing)
        self.assertEqual(results[-1]["result"], "fail")
        self.assertIn(leaf + " missing", output.getvalue())
        for canary in CANARIES:
            self.assertNotIn(canary, output.getvalue())
        self.assertEqual({name for name, _ in ci.promotion_failure_classes(rows, 1)}, set(group["tests"]))

    def test_promotion_missing_leaf_zero_exit_failed_receipt(self):
        missing = "TestInstalledProductionPromotionInert/consented_missing_route_removal"
        for system, mode in (("Darwin", "normal"), ("Linux", "normal"), ("Linux", "race")):
            with self.subTest(system=system, mode=mode), tempfile.TemporaryDirectory() as root:
                groups, calls = ci.selection(system), []
                def fake(args):
                    index = len(calls)
                    group = groups[index]
                    self.assertEqual(args, ci.command(group, mode))
                    calls.append(args)
                    rows = events(group)
                    if index == 21:
                        self.assertEqual(group, PROMOTION_GROUP)
                        rows = [row for row in rows if row.get("Test") != missing]
                        rows.insert(1, dict(Package=group["package"], Action="output",
                                            Output=" ".join((*CANARIES, root))))
                    return encoded(rows), 0  # Even the incomplete promotion exits zero.
                path = Path(root) / "receipt.json"
                argv = ["--mode", mode, "--os", system, "--sha", "a" * 40,
                        "--run-id", "123", "--attempt", "2", "--evidence", str(path)]
                metadata = {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2"}
                # Synthetic receipt policy only, with an explicit inert executor.
                with patch.object(ci.platform, "system", return_value=system), \
                     patch.object(ci, "authorized", return_value=True), \
                     patch.object(ci.os.environ, "get", side_effect=metadata.get):
                    self.assertEqual(ci.main(argv, fake), 1)
                self.assertEqual(len(calls), 22)
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                receipt = json.loads(path.read_text(), object_pairs_hook=ci.unique_object)
                assert_minimized(self, receipt, expected_receipt(system, 21, mode), (*CANARIES, root))

    def test_promotion_receipt_refusals(self):
        for system in ("Darwin", "Linux"):
            expected = expected_receipt(system, None)
            self.assertEqual(expected["invocations"][21],
                             dict(package=PROMOTION_GROUP["package"], tagged=True,
                                  required_tests=list(PROMOTION_GROUP["tests"]), result="pass"))
            for mutate in (
                    lambda r: r["invocations"].pop(),
                    lambda r: r["invocations"][21]["required_tests"].pop(),
                    lambda r: r["invocations"][21]["required_tests"].append("TestUnexpectedPromotion"),
                    lambda r: r["invocations"][21]["required_tests"].__setitem__(1, "TestRenamedPromotion"),
                    lambda r: r["invocations"][21].update(result="skip"),
                    lambda r: r["invocations"][21].update(result="fail"),
                    lambda r: r["invocations"][21].update(tagged=False)):
                bad = copy.deepcopy(expected)
                mutate(bad)
                with self.assertRaisesRegex(AssertionError, "exact projection"):
                    assert_minimized(self, bad, expected, CANARIES)
            bad = copy.deepcopy(expected)
            bad["invocations"][21]["Output"] = ""
            with self.assertRaisesRegex(AssertionError, "raw Output field retained"):
                assert_minimized(self, bad, expected, CANARIES)

    def test_promotion_source_labels(self):
        root = Path(__file__).resolve().parents[1]
        source = (root / "cmd/herdr-mobile-relay/installed_promotion_integration_test.go").read_text()
        self.assertTrue(source.startswith("//go:build herdr_installed_inert_test\n"))
        self.assertIn("func TestInstalledProductionPromotionInert(t *testing.T)", source)
        self.assertIn("app.DenyInstalledCommandHTTP(t)", source)
        labels = re.findall(r'\bt\.Run\("([^"]+)"', source)
        self.assertEqual(labels, ["owned update and scoped removal", "uncertain stop retains authority",
                                  "consented missing route removal"])
        # These labels have only ASCII spaces; Go testing rewrites each to '_'.
        self.assertEqual(["TestInstalledProductionPromotionInert"] +
                         ["TestInstalledProductionPromotionInert/" + label.replace(" ", "_") for label in labels],
                         PROMOTION_GROUP["tests"])

    def test_workflow_entrypoint_and_minimization(self):
        metadata = {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2"}
        # Synthetic receipt policy only: admission is stubbed, binding is real,
        # and every main call explicitly supplies an inert executor.
        for system, mode in (("Darwin", "normal"), ("Linux", "normal"), ("Linux", "race")):
            for failed_index in (None, *range(22)):
                with self.subTest(system=system, mode=mode, failed_index=failed_index), \
                     tempfile.TemporaryDirectory() as root, \
                     patch.object(ci.platform, "system", return_value=system), \
                     patch.object(ci, "authorized", return_value=True), \
                     patch.object(ci.os.environ, "get", side_effect=metadata.get):
                    calls = []
                    def fake(command):
                        index = len(calls)
                        group = ci.selection(system)[index]
                        self.assertEqual(command, ci.command(group, mode))
                        calls.append(command)
                        rows = events(group)
                        rows.insert(1, {"Package": group["package"], "Action": "output", "Output": " ".join((*CANARIES, root))})
                        return encoded(rows), int(index == failed_index)
                    path = Path(root) / "evidence.json"
                    argv = ["--mode", mode, "--os", system, "--sha", "a" * 40,
                            "--run-id", "123", "--attempt", "2", "--evidence", str(path)]
                    self.assertEqual(ci.main(argv, fake), int(failed_index is not None))
                    receipt = json.loads(path.read_text(), object_pairs_hook=ci.unique_object)
                    assert_minimized(self, receipt,
                                     expected_receipt(system, failed_index, mode),
                                     (*CANARIES, root))
                    self.assertEqual(len(calls), 22)
                    self.assertEqual(ci.main(argv, fake), 1)  # stale receipt
                    self.assertEqual(len(calls), 22)

    def test_minimization_fixed_identities_and_sensitive_injections(self):
        # Pure projection checks: no driver dispatch or executor defaults.
        for system in ("Darwin", "Linux"):
            expected = expected_receipt(system, None)
            names = {name for g in expected["invocations"] for name in g["required_tests"]}
            for name in ("TestInstalledHandoffPolicy/private-dir",
                         "TestInstalledHandoffPolicy/private-file",
                         "TestInstalledControlOutput",
                         "TestInstalledEndpointBlockedOutputJoin"):
                self.assertTrue(name in names, "required public identity missing")
            assert_minimized(self, copy.deepcopy(expected), expected, CANARIES)
            for location in ("top", "group", "nested"):
                bad = copy.deepcopy(expected)
                target = bad if location == "top" else bad["invocations"][0]
                if location == "nested":
                    target["extra"] = [{"Output": ""}]
                else:
                    target["Output"] = ""
                with self.subTest(system=system, location=location):
                    with self.assertRaisesRegex(AssertionError, "raw Output field retained"):
                        assert_minimized(self, bad, expected, CANARIES)
            for canary in CANARIES:
                bad = copy.deepcopy(expected)
                bad["invocations"][0]["required_tests"][0] += canary
                with self.assertRaisesRegex(AssertionError, "sensitive canary retained"):
                    assert_minimized(self, bad, expected, CANARIES)
            for field in ("stderr", "private_path", "credential"):
                bad = copy.deepcopy(expected)
                bad["invocations"][0][field] = "unapproved"
                with self.assertRaisesRegex(AssertionError, "exact projection"):
                    assert_minimized(self, bad, expected, CANARIES)
            for mutate in (
                    lambda r: r.pop("run_attempt"),
                    lambda r: r.update(schema_version=True),
                    lambda r: r["invocations"][0].update(result="fail"),
                    lambda r: r["invocations"].pop(),
                    lambda r: r["invocations"][0]["required_tests"].pop()):
                bad = copy.deepcopy(expected)
                mutate(bad)
                with self.assertRaisesRegex(AssertionError, "exact projection"):
                    assert_minimized(self, bad, expected, CANARIES)

    def test_isolation_source_boundary(self):
        root = Path(__file__).resolve().parents[1]
        app = root / "internal/app"
        ordinary = (app / "speech_languages.go").read_text()
        fixture = (app / "speech_fixture.go").read_text()
        self.assertTrue(ordinary.startswith("//go:build !herdr_installed_inert_test\n"))
        self.assertTrue(fixture.startswith("//go:build herdr_installed_inert_test\n"))
        self.assertNotIn("speech.Languages()", fixture)
        self.assertNotIn("speech.Status()", fixture)
        server = (app / "server.go").read_text()
        self.assertIn("serverSpeechLanguages()", server)
        self.assertIn("serverSpeechStatus,", server)
        name = "TestInstalledCLIConstructionPreservesStore"
        owners = [g for g in ci.INVENTORY if name in g["tests"]]
        self.assertEqual(len(owners), 1)
        self.assertTrue(owners[0]["tagged"])
        self.assertNotIn("func " + name, (app / "installed_cli_test.go").read_text())
        for path in [app / "installed_construction_test.go", app / "installed_integration_test.go",
                     app / "installed_hosted_test.go", app / "installed_setup_test.go",
                     app / "installed_setup_close_test.go",
                     root / "cmd/herdr-mobile-relay/installed_cli_integration_test.go",
                     root / "cmd/herdr-mobile-relay/installed_promotion_integration_test.go"]:
            source = path.read_text()
            self.assertIn("IsolateInstalledFixture(t)", source)
            self.assertNotIn("t.Parallel()", source)
        for filename, base in [("installed_construction_test.go", "root"),
                               ("installed_integration_test.go", "base")]:
            source = (app / filename).read_text()
            for field, leaf in [("ReleaseRoot", "release"), ("WebRoot", "web")]:
                self.assertIn(f'{field}: filepath.Join({base}, "{leaf}")', source)
        env = (app / "installed_environment_fixture.go").read_text()
        for marker in ["os.Environ()", 't.Setenv(key, "")', '"HOME"', '"PATH"',
                       '"XDG_CONFIG_HOME"', '"XDG_RUNTIME_DIR"']:
            self.assertIn(marker, env)
        self.assertIn("root := t.TempDir()", env)
        self.assertIn("\t}\n\tt.Chdir(root)\n\treturn root", "\n".join(
            line for line in env.splitlines() if not line.lstrip().startswith("//")))
        self.assertLess(env.rindex("t.Setenv("), env.index("t.Chdir(root)"))
        self.assertNotIn("t.Parallel()", env)
        self.assertEqual(len(ci.INVENTORY), 6)
        self.assertEqual(sum(len(g["tests"]) for g in ci.INVENTORY), 183)

    def test_setup_source_boundary(self):
        root = Path(__file__).resolve().parents[1]
        app = root / "internal/app"
        source = (app / "installed_setup.go").read_text()
        self.assertLess(source.index("manager.WithInstalledSetupSession("),
                        source.index("session.PrepareRuntime("))
        self.assertLess(source.index("session.PrepareRuntime("),
                        source.index("s := newServerWithStartup("))
        self.assertIn("s := newServerWithStartup(&cfg, version, revision, logger, nil, nil, installedServiceStartup)", source)
        self.assertIn("installedCLIRunner()(s, lifetime)", source)
        self.assertNotIn(".Run(", source)
        bind = source.index("join := func() { cancel(); <-done }")
        launch = source.index("go func()", bind)
        runner = source.index("err := installedCLIRunner()(s, lifetime)", launch)
        self.assertLess(bind, source.index("defer join()", bind))
        self.assertLess(source.index("defer join()", bind), launch)
        self.assertLess(launch, runner)
        self.assertLess(runner, source.index("join() // includes callback retirement", runner))
        self.assertIn("publication-failed-separate-lifecycle-required", source)
        backend = (app / "installed_backend.go").read_text()
        self.assertLess(backend.index("b.active = false"), backend.index("b.operations.Wait()"))
        self.assertLess(backend.index("b.operations.Wait()"), backend.index("b.Listener.Close()"))
        server = (app / "server.go").read_text()
        start = server.index("s.installedBackend = newInstalledBackend(ctx, ln)")
        self.assertLess(start, server.index("close(s.listenerBound)", start))
        self.assertIn("ln = s.installedBackend", server[start:])
        fixture = (root / "internal/tailscalecli/installed_setup_fixture.go").read_text()
        self.assertTrue(fixture.startswith("//go:build herdr_installed_inert_test\n"))
        self.assertNotIn("writeRegistration(", fixture)
        self.assertNotIn("writeBackendReservation(", fixture)

    def test_cumulative_inventory(self):
        # Freeze the original 153 exact identities in their original six tiers.
        # New setup cases are additive, never substitutes for historical coverage.
        import hashlib
        setup = set(["TestInstalledSetupCancelledDispatchedFailure","TestInstalledSetupDurableFailures","TestInstalledSetupDurableFailures/dispatch","TestInstalledSetupDurableFailures/promotion","TestInstalledSetupDurableFailures/readback","TestInstalledSetupDurableFailures/receipt","TestInstalledSetupFunnelPreflight","TestInstalledSetupFunnelPreflight/false","TestInstalledSetupFunnelPreflight/true","TestInstalledSetupLifecycle","TestInstalledSetupProductionClosed","TestInstalledSetupQueuedCancellation","TestInstalledSetupQueuedCancellation/caller","TestInstalledSetupQueuedCancellation/lifetime","TestInstalledSetupRefusals","TestInstalledSetupRefusals/attempt","TestInstalledSetupRefusals/binding","TestInstalledSetupRefusals/cancel","TestInstalledSetupRefusals/consent","TestInstalledSetupRefusals/development","TestInstalledSetupRefusals/inactive-lease","TestInstalledSetupRefusals/missing-lease","TestInstalledSetupRefusals/missing-reservation","TestInstalledSetupRefusals/risk","TestInstalledSetupRetirementDrains"])
        added = [set(), setup,
                 {"TestInstalledRecoveryJoinedPrecedence", "TestInstalledSetupMonitorTransition"},
                 set(),
                 {"TestInstalledSetupConcreteApp", "TestInstalledBackendCloseDrains",
                  "TestInstalledSetupPublicationClose"}, set()]
        frozen = [
            "f1957f1a153822823a0345ad2b3b852442a89db35cfa778d9e03d1da8c76760a",
            "4073e753f72b253662327fb627b9f59120858e6223a6e41cab2866440d478868",
            "421c2e3d31791cc825e7bec4761867034918076146969913ef30ff6f83d1e56a",
            "97190d551c04714af4964853f29ee809745fe32aa8360d8fbcb4ab5a3822ebb9",
            "c8161cad067cbac0b7c10e462fc3453cbe67b36ce25d53f13c2e96cb0cf72086",
            "5af6cd58d65646188d9b1cf4526d2f6db4bf7c7a8bde971a471bd4b159a57038"]
        self.assertEqual(len(setup), 25)
        for group, new, digest in zip(ci.INVENTORY, added, frozen):
            self.assertTrue(new <= set(group["tests"]))
            old = [name for name in group["tests"] if name not in new]
            self.assertEqual(hashlib.sha256("\n".join(old).encode()).hexdigest(), digest)
            for name in new:
                missing = [e for e in events(group) if e.get("Test") != name]
                self.assertFalse(ci.validate(encoded(missing).splitlines(), 0, group))

    def test_frozen_supplement_and_matrix(self):
        root = Path(__file__).resolve().parents[1]
        supplement = root / "scripts/ci/installed-inert-supplemental-inventory.json"
        self.assertEqual(hashlib.sha256(supplement.read_bytes()).hexdigest(),
                         "33ab5ef02ae68b55b7f280ccdae7e5e8b1a4980c4e9d50713caf64cbff4e9349")
        self.assertEqual(ci.SUPPLEMENTAL, json.loads(supplement.read_text()))
        self.assertEqual(hashlib.sha256((root / "scripts/ci/installed-inert-inventory.json").read_bytes()).hexdigest(),
                         "24d18df1af7ea4877c0e48568bbe00367e60ac102a169960a9fddafa35ed9fc4")
        for system, mode in (("Darwin", "normal"), ("Linux", "normal"), ("Linux", "race")):
            groups = ci.selection(system)
            self.assertEqual(len(groups), 22)
            self.assertEqual(sum(len(g["tests"]) for g in groups), 737 if system == "Darwin" else 739)
            historical = copy.deepcopy(ci.INVENTORY + ci.SUPPLEMENTAL["groups"])
            for group in historical:
                group["tests"].extend(group.pop("os_tests", {}).get(system, []))
            self.assertEqual(groups[:21], historical)
            self.assertEqual(sum(len(g["tests"]) for g in groups[6:21]), 550 if system == "Darwin" else 552)
            self.assertEqual(groups[21:], [PROMOTION_GROUP])
            promotion_command = ["go", "test", "-json", "-count=1", "-timeout=5m"]
            if mode == "race":
                promotion_command.append("-race")
            promotion_command += ["-tags=herdr_installed_inert_test",
                                  "github.com/0cv/herdr-mobile-relay/cmd/herdr-mobile-relay",
                                  "-run", "^(TestInstalledProductionPromotionInert)$"]
            self.assertEqual(ci.command(groups[21], mode), promotion_command)
            calls = []
            def fake(args):
                group = groups[len(calls)]
                self.assertEqual(args, ci.command(group, mode))
                self.assertEqual("-race" in args, mode == "race")
                self.assertNotIn("hosted", " ".join(args).lower())
                calls.append(args)
                return encoded(events(group)), 0
            with patch.object(ci.platform, "system", return_value=system):
                self.assertTrue(all(r["result"] == "pass" for r in ci.run_suite(mode, fake)))
            self.assertEqual(len(calls), 22)
            self.assertEqual(calls[21], promotion_command)
            for failed_index in range(22):
                index = [0]
                def failing(args):
                    current = index[0]
                    index[0] += 1
                    self.assertEqual(args, ci.command(groups[current], mode))
                    return encoded(events(groups[current])), int(current == failed_index)
                with patch.object(ci.platform, "system", return_value=system):
                    results = ci.run_suite(mode, failing)
                self.assertEqual(index[0], 22)
                self.assertEqual([r["result"] for r in results],
                                 ["fail" if i == failed_index else "pass" for i in range(22)])
            for group in groups:
                good = events(group)
                for name in group["tests"]:
                    self.assertFalse(ci.validate(encoded([e for e in good if e.get("Test") != name]).splitlines(), 0, group))
                for extra in ("TestUnexpected", group["tests"][0] + "/unexpected"):
                    rows = good[:-1] + [dict(Package=group["package"], Test=extra, Action=a) for a in ("run", "pass")] + good[-1:]
                    self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))
                rows = good[:2] + [dict(good[2], Action="fail")] + good[2:]
                self.assertFalse(ci.validate(encoded(rows).splitlines(), 0, group))

    def test_frozen_equals_descendants(self):
        frozen = {(g["package"], g["tagged"], name) for g in ci.INVENTORY
                  for name in g["tests"] if "=" in name}
        self.assertEqual(len(frozen), 40)
        for value in ("false", "true"):
            self.assertTrue(any(name == "TestInstalledResumeReservationEvidence/attempt/during=" + value
                                for _, _, name in frozen))
        for system in ("Darwin", "Linux"):
            with self.subTest(system=system):
                groups = ci.selection(system)
                self.assertEqual({(g["package"], g["tagged"], name) for g in groups
                                  for name in g["tests"] if "=" in name}, frozen)
                for group in groups:
                    selector = ci.command(group, "normal")[-1]
                    roots = sorted(name for name in group["tests"] if "/" not in name)
                    self.assertEqual(selector, "^(" + "|".join(roots) + ")$")
                    self.assertNotIn("=", selector)
                    if any("=" in name for name in group["tests"]):
                        self.assertTrue(ci.validate(encoded(events(group)).splitlines(), 0, group))

    def test_main_invalid_inventory_no_dispatch_failed_artifact(self):
        for system in ("Darwin", "Linux"):
            with self.subTest(system=system), tempfile.TemporaryDirectory() as directory:
                data = copy.deepcopy(ci.SUPPLEMENTAL)
                data["groups"][0]["tests"][0] = "TestInvalid=Root"
                calls = []
                def fake(args):
                    calls.append(args)
                    self.fail("invalid inventory dispatched")
                path = Path(directory) / "receipt.json"
                args = ["--mode", "normal", "--os", system, "--sha", "a" * 40,
                        "--run-id", "1", "--attempt", "1", "--evidence", str(path)]
                metadata = {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "1"}
                with patch.object(ci.platform, "system", return_value=system), \
                     patch.object(ci, "authorized", return_value=True), \
                     patch.object(ci.os.environ, "get", side_effect=metadata.get):
                    with patch.object(ci, "SUPPLEMENTAL", data):
                        self.assertEqual(ci.main(args, fake), 1)
                    # Exclusive creation precedes schema validation: empty is not a receipt.
                    self.assertEqual(path.read_bytes(), b"")
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    with self.assertRaises(json.JSONDecodeError):
                        json.loads(path.read_text())
                    # Even after restoring valid schema, the failed artifact cannot be reused.
                    self.assertEqual(ci.main(args, fake), 1)
                    self.assertEqual(path.read_bytes(), b"")
                self.assertEqual(calls, [])

    def test_descendant_grammar_refusals(self):
        for system in ("Darwin", "Linux"):
            for name in ("TestBad=Root", "TestBad.Root", "TestBad-Root", "TestBad/",
                         "TestBad//during=false", "TestBad/during=false|true",
                         "TestBad/during=false$", "TestBad/during= false",
                         "TestBad/during=false\n", "TestBad/" + "=" * 249):
                with self.subTest(system=system, name=name):
                    data = copy.deepcopy(ci.SUPPLEMENTAL)
                    data["groups"][0]["tests"][0] = name
                    with self.assertRaisesRegex(ValueError, "invalid test identity"):
                        ci.selection(system, supplemental=data)

    def test_promotion_schema_and_duplicate_refusals(self):
        for system in ("Darwin", "Linux"):
            for mutate in (
                    lambda g: g.update(extra=True),
                    lambda g: g.update(tagged=1),
                    lambda g: g.update(package="unknown"),
                    lambda g: g.update(tests=[]),
                    lambda g: g["tests"].append(g["tests"][0]),
                    lambda g: g["tests"].append("TestInstalledProductionPromotionInert/bad|selector"),
                    lambda g: g["tests"].pop(0),
                    lambda g: g["tests"].pop(),
                    lambda g: g["tests"].append("TestInstalledProductionPromotionInert/extra")):
                group = copy.deepcopy(PROMOTION_GROUP)
                mutate(group)
                with patch.object(ci, "PROMOTION", group), self.assertRaises(ValueError):
                    ci.selection(system)
            data = copy.deepcopy(ci.SUPPLEMENTAL)
            data["groups"][12]["tests"].extend(PROMOTION_GROUP["tests"])
            with self.assertRaisesRegex(ValueError, "duplicate identity"):
                ci.selection(system, supplemental=data)

    def test_inventory_schema_refusals(self):
        mutations = [
            lambda s: s.update(extra=True),
            lambda s: s.update(schema_version=True),
            lambda s: s["groups"][0].update(tagged=1),
            lambda s: s["groups"][0].update(package="unknown"),
            lambda s: s["groups"][0].update(extra=True),
            lambda s: s["groups"][0]["tests"].append(s["groups"][0]["tests"][0]),
            lambda s: s["groups"][0]["tests"].append("TestMissing/child"),
            lambda s: s["groups"][0]["tests"].append("TestBad|selector"),
            lambda s: s["groups"][9].pop("os_tests"),
            lambda s: s["groups"][9]["os_tests"].pop("Linux"),
            lambda s: s["groups"][9]["os_tests"].update(Windows=[]),
            lambda s: s["groups"].pop(),
        ]
        for mutate in mutations:
            data = copy.deepcopy(ci.SUPPLEMENTAL)
            mutate(data)
            with self.assertRaises((ValueError, TypeError)):
                ci.selection("Darwin", supplemental=data)
        with self.assertRaises(ValueError):
            ci.selection("Windows")
        with self.assertRaises(ValueError):
            json.loads('{"tests":[],"tests":[]}', object_pairs_hook=ci.unique_object)
        with patch.object(ci.platform, "system", return_value="Darwin"):
            with self.assertRaises(ValueError):
                ci.run_suite("race", lambda _: self.fail("dispatched"))
        with tempfile.TemporaryDirectory() as directory:
            args = ["--mode", "normal", "--os", "Linux", "--sha", "a" * 40,
                    "--run-id", "1", "--attempt", "1", "--evidence", str(Path(directory) / "receipt")]
            metadata = {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "1"}
            with patch.object(ci.platform, "system", return_value="Darwin"), \
                 patch.object(ci, "authorized", return_value=True), \
                 patch.object(ci.os.environ, "get", side_effect=metadata.get):
                self.assertEqual(ci.main(args, lambda _: self.fail("dispatched")), 1)

    def test_exact_selection(self):
        for group in ci.INVENTORY:
            cmd = ci.command(group, "race")
            self.assertIn("-race", cmd)
            self.assertNotIn("herdr_tailscale_test", " ".join(cmd))
            self.assertTrue(cmd[-1].startswith("^("))
            self.assertTrue(cmd[-1].endswith(")$"))
            self.assertEqual("-tags=herdr_installed_inert_test" in cmd, group["tagged"])


if __name__ == "__main__":
    unittest.main()

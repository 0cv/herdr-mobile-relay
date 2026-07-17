import fcntl
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from relay import update_support


class UpdateSupportTest(unittest.TestCase):
    def make_local_checkout(self, root: Path, version: str = "0.7.0") -> tuple[Path, Path]:
        repo = root / "repo"
        home = root / "home"
        repo.mkdir()
        home.mkdir()
        (repo / "relay").mkdir()
        (repo / "relay" / "herdr-mobile-relay-service.sh").write_text("#!/bin/sh\n")
        (repo / "herdr-plugin.toml").write_text(f'version = "{version}"\n')
        subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True)
        subprocess.run(["git", "-C", str(repo), "config", "user.email", "tests@example.test"], check=True)
        subprocess.run(["git", "-C", str(repo), "config", "user.name", "Tests"], check=True)
        subprocess.run(["git", "-C", str(repo), "config", "commit.gpgsign", "false"], check=True)
        subprocess.run(
            ["git", "-C", str(repo), "remote", "add", "origin", update_support.UPSTREAM_GIT_URL],
            check=True,
        )
        subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
        subprocess.run(["git", "-C", str(repo), "commit", "-q", "-m", "fixture"], check=True)
        service = home / ".config/systemd/user/herdr-mobile-relay.service"
        service.parent.mkdir(parents=True)
        service.write_text(
            f"WorkingDirectory={repo}\n"
            f"ExecStart={repo}/relay/herdr-mobile-relay-service.sh\n"
        )
        return repo, home

    def test_semver_and_manifest_versions_are_strict(self):
        self.assertEqual(update_support.semver("1.2.3"), (1, 2, 3))
        self.assertIsNone(update_support.semver("1.2"))
        self.assertIsNone(update_support.semver("01.2.3"))
        self.assertEqual(update_support.manifest_version('name = "relay"\nversion = "2.0.1"\n'), "2.0.1")
        with self.assertRaises(ValueError):
            update_support.manifest_version('version = "latest"\n')

    def test_local_checkout_reports_only_newer_releases_as_installable(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            repo, home = self.make_local_checkout(root)
            runtime = root / "runtime"

            available = update_support.check_for_update(
                repo,
                runtime,
                "herdr",
                now=123,
                remote_release=("a" * 40, "0.8.0"),
                home=home,
                system="Linux",
            )
            current = update_support.check_for_update(
                repo,
                runtime,
                "herdr",
                now=124,
                remote_release=("b" * 40, "0.7.0"),
                home=home,
                system="Linux",
            )

        self.assertEqual(available["state"], "available")
        self.assertTrue(available["can_install"])
        self.assertEqual(available["mode"], "local")
        self.assertEqual(available["target_revision"], "a" * 40)
        self.assertEqual(current["state"], "current")
        self.assertFalse(current["can_install"])
        self.assertEqual(current["available_version"], "")

    def test_dirty_local_checkout_blocks_an_available_update(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            repo, home = self.make_local_checkout(root)
            (repo / "local-change.txt").write_text("keep me\n")

            status = update_support.check_for_update(
                repo,
                root / "runtime",
                "herdr",
                remote_release=("c" * 40, "0.8.0"),
                home=home,
                system="Linux",
            )

        self.assertEqual(status["state"], "blocked")
        self.assertFalse(status["can_install"])
        self.assertIn("local changes", status["reason"])

    def test_managed_plugin_is_recognized_from_the_herdr_registry(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            repo, home = self.make_local_checkout(root)
            herdr = root / "herdr"
            herdr.write_text("#!/bin/sh\n")
            herdr.chmod(0o700)
            registry = home / ".config/herdr/plugins.json"
            registry.parent.mkdir(parents=True, exist_ok=True)
            registry.write_text(json.dumps([{
                "plugin_id": update_support.PLUGIN_ID,
                "plugin_root": str(repo),
                "source": {
                    "kind": "github",
                    "owner": "0cv",
                    "repo": "herdr-mobile-relay",
                },
            }]))

            eligibility = update_support.inspect_installation(
                repo,
                str(herdr),
                home=home,
                system="Linux",
            )

        self.assertEqual(eligibility, {"can_install": True, "mode": "managed", "reason": ""})

    def test_update_job_verifies_restart_before_marking_success(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            runtime = root / "runtime"
            runtime.mkdir()
            job_path = runtime / "job.json"
            job_path.write_text(json.dumps({
                "repo_root": str(root / "repo"),
                "runtime_dir": str(runtime),
                "herdr_bin": "herdr",
                "relay_env": "",
                "home": str(root),
                "system": "Linux",
                "mode": "local",
                "previous_version": "0.7.0",
                "previous_revision": "a" * 40,
                "target_version": "0.8.0",
                "target_revision": "b" * 40,
                "checked_at": 123,
            }))
            calls: list[str] = []
            with (
                patch.object(update_support, "_install_local", side_effect=lambda *_: calls.append("install")),
                patch.object(update_support, "_verify_target", side_effect=lambda *_: calls.append("target")),
                patch.object(update_support, "_install_service", side_effect=lambda *_: calls.append("service")),
                patch.object(update_support, "_verify_running_health", side_effect=lambda *_: calls.append("health")),
            ):
                result = update_support.run_update_job(job_path)
            state = json.loads(update_support.state_file(runtime).read_text())

        self.assertEqual(result, 0)
        self.assertEqual(calls, ["install", "target", "service", "health"])
        self.assertEqual(state["state"], "succeeded")
        self.assertEqual(state["current_version"], "0.8.0")
        self.assertFalse(job_path.exists())

    def test_failed_update_rolls_back_and_records_the_outcome(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            runtime = root / "runtime"
            runtime.mkdir()
            job_path = runtime / "job.json"
            job_path.write_text(json.dumps({
                "repo_root": str(root / "repo"),
                "runtime_dir": str(runtime),
                "herdr_bin": "herdr",
                "relay_env": "",
                "home": str(root),
                "system": "Linux",
                "mode": "local",
                "previous_version": "0.7.0",
                "previous_revision": "a" * 40,
                "target_version": "0.8.0",
                "target_revision": "b" * 40,
                "checked_at": 123,
            }))
            rollback = Mock()
            with (
                patch.object(update_support, "_install_local", side_effect=RuntimeError("install broke")),
                patch.object(update_support, "_rollback", rollback),
            ):
                result = update_support.run_update_job(job_path)
            state = json.loads(update_support.state_file(runtime).read_text())

        self.assertEqual(result, 1)
        rollback.assert_called_once()
        self.assertEqual(state["state"], "rolled_back")
        self.assertIn("install broke", state["error"])

    def test_update_job_lock_prevents_overlapping_installers(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            job_path = runtime / "job.json"
            job_path.write_text(json.dumps({"runtime_dir": str(runtime)}))
            with (runtime / update_support.UPDATE_LOCK_NAME).open("w") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                result = update_support.run_update_job(job_path)

        self.assertEqual(result, 3)
        self.assertFalse(job_path.exists())

    def test_linux_update_launches_in_a_separate_systemd_unit(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            repo, home = self.make_local_checkout(root)
            runtime = root / "runtime"
            completed = subprocess.CompletedProcess([], 0, "", "")
            with (
                patch.object(update_support.shutil, "which", return_value="/usr/bin/systemd-run"),
                patch.object(update_support, "run_command", return_value=completed) as run,
            ):
                label = update_support.launch_update_job(
                    repo,
                    runtime,
                    "herdr",
                    "",
                    {
                        "mode": "local",
                        "current_version": "0.7.0",
                        "available_version": "0.8.0",
                        "target_revision": "d" * 40,
                        "checked_at": 123,
                    },
                    python="/usr/bin/python3",
                    home=home,
                    system="Linux",
                )

            command = run.call_args.args[0]

        self.assertTrue(label.startswith("herdr-mobile-relay-update-"))
        self.assertEqual(command[:3], ["/usr/bin/systemd-run", "--user", "--collect"])
        self.assertIn("--run-job", command)


if __name__ == "__main__":
    unittest.main()

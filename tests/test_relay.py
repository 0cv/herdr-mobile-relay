import importlib.util
import json
import tempfile
import unittest
import urllib.parse
from pathlib import Path
from unittest.mock import AsyncMock, patch


RELAY_PATH = Path(__file__).parents[1] / "relay" / "herdr_relay.py"
SPEC = importlib.util.spec_from_file_location("herdr_relay_under_test", RELAY_PATH)
relay = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(relay)


class FakeWebSocket:
    def __init__(self):
        self.messages = []

    async def send(self, payload):
        self.messages.append(json.loads(payload))


class RelayHelpersTest(unittest.TestCase):
    def test_relay_env_example_stays_minimal(self):
        env_example = RELAY_PATH.with_name(".env.example")
        keys = {
            line.split("=", 1)[0]
            for line in env_example.read_text().splitlines()
            if line and not line.startswith("#")
        }
        self.assertEqual(keys, {"HERDR_RELAY_TOKEN", "CLOUDFLARED_CONFIG"})

    def test_respond_keys_select_first_middle_and_last(self):
        self.assertEqual(relay.respond_keys(0, 3), ["Enter"])
        self.assertEqual(relay.respond_keys(1, 3), ["Down", "Enter"])
        self.assertEqual(relay.respond_keys(2, 3), ["Escape"])

    def test_push_payload_contains_open_approve_and_deny_targets(self):
        payload = relay.push_payload({
            "event_id": "event-1",
            "host": "fedora",
            "pane_id": "w1:p2",
            "project": "relay",
            "command": "Run tests?",
            "options": ["yes", "always", "no"],
        })

        def target(url):
            encoded = url.split("#notify=", 1)[1]
            return json.loads(urllib.parse.unquote(encoded))

        self.assertEqual(target(payload["url"])["pane_id"], "w1:p2")
        self.assertEqual(target(payload["action_urls"]["approve"])["index"], 0)
        self.assertEqual(target(payload["action_urls"]["deny"])["index"], 2)
        self.assertEqual(target(payload["action_urls"]["deny"])["notification_id"], "event-1")

    def test_activity_round_trip_is_bounded_and_private(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            with patch.object(relay, "ACTIVITY_FILE", activity_file), patch.object(relay, "ACTIVITY_MAX_ITEMS", 2):
                relay.record_activity("prompt", "completed", "First")
                relay.record_activity("approval", "confirmed", "Second")
                relay.record_activity("agent_stop", "completed", "Third")
                entries = relay.load_activity(2)

            self.assertEqual([entry["summary"] for entry in entries], ["Second", "Third"])
            self.assertEqual(activity_file.stat().st_mode & 0o777, 0o600)

    def test_agent_profiles_are_detected_from_installed_executables(self):
        def find_executable(name):
            return f"/usr/bin/{name}" if name in {"codex", "claude"} else None

        with patch.object(relay.shutil, "which", side_effect=find_executable):
            profiles = relay.load_agent_profiles()
        self.assertEqual(set(profiles), {"codex", "claude"})
        self.assertEqual(profiles["claude"]["argv"], ["/usr/bin/claude"])

    def test_project_directory_navigation_lists_one_level_and_excludes_hidden_folders(self):
        with tempfile.TemporaryDirectory() as temp_dir, tempfile.TemporaryDirectory() as outside_dir:
            home = Path(temp_dir)
            development = home / "Development"
            project = development / "relay"
            downloads = home / "Downloads"
            hidden = home / ".private"
            outside_link = home / "outside"
            for path in (project, downloads, hidden):
                path.mkdir(parents=True, exist_ok=True)
            outside_link.symlink_to(outside_dir, target_is_directory=True)

            real_scandir = relay.os.scandir

            def macos_scandir(path):
                if Path(path) == downloads:
                    raise PermissionError("Operation not permitted")
                return real_scandir(path)

            with (
                patch.object(relay.Path, "home", return_value=home),
                patch.object(relay.os, "scandir", side_effect=macos_scandir),
            ):
                root, root_error = relay.list_project_directory()
                child, child_error = relay.list_project_directory(str(development))
                outside, outside_error = relay.list_project_directory(outside_dir)

        self.assertEqual(root_error, "")
        self.assertEqual(root["current"], {"path": str(home), "label": "~"})
        self.assertEqual(root["parent"], "")
        self.assertEqual([entry["name"] for entry in root["directories"]], ["Development"])
        self.assertEqual(child_error, "")
        self.assertEqual(child["parent"], str(home))
        self.assertEqual(child["directories"], [{"name": "relay", "path": str(project)}])
        self.assertIsNone(outside)
        self.assertIn("home directory", outside_error)

    def test_project_directory_navigation_reports_macos_privacy_denial(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            with (
                patch.object(relay.Path, "home", return_value=home),
                patch.object(relay.Path, "iterdir", side_effect=PermissionError("Operation not permitted")),
                patch.object(relay.sys, "platform", "darwin"),
            ):
                listing, error = relay.list_project_directory()

        self.assertIsNone(listing)
        self.assertEqual(error, "macOS denied access to this directory")

    def test_project_directory_navigation_has_no_flat_catalog_limit(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            for index in range(255):
                (home / f"project-{index:03d}").mkdir()
            with patch.object(relay.Path, "home", return_value=home):
                listing, error = relay.list_project_directory()

        self.assertEqual(error, "")
        self.assertEqual(len(listing["directories"]), 255)

    def test_agent_cwd_must_be_within_the_user_home(self):
        with tempfile.TemporaryDirectory() as allowed, tempfile.TemporaryDirectory() as outside:
            with patch.object(relay.Path, "home", return_value=Path(allowed)):
                resolved, error = relay.resolve_agent_cwd(outside)
        self.assertIsNone(resolved)
        self.assertIn("home directory", error)


class RelayCommandsTest(unittest.IsolatedAsyncioTestCase):
    async def test_approval_reports_accepted_then_confirmed(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-1", "pane_id": "w1:p1", "index": 0, "total": 3, "choice": "yes"}

        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(relay, "run_herdr_async_result", AsyncMock(return_value=(True, "", ""))) as run_command,
            patch.object(relay, "wait_for_approval_result", AsyncMock(return_value=(True, "working"))),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws, msg)

        self.assertEqual([message["phase"] for message in ws.messages], ["accepted", "confirmed"])
        run_command.assert_awaited_once_with("pane", "send-keys", "w1:p1", "Enter")

    async def test_stale_approval_is_rejected_without_sending_keys(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "working", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-2", "pane_id": "w1:p1", "index": 0, "total": 3}

        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as run_command,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws, msg)

        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("no longer blocked", ws.messages[0]["error"])
        run_command.assert_not_awaited()

    async def test_agent_start_sends_initial_task_as_literal_pane_text(self):
        ws = FakeWebSocket()
        msg = {
            "type": "agent_start",
            "request_id": "request-3",
            "profile_id": "test",
            "name": "mobile-test",
            "prompt": "--literal task text",
        }
        with tempfile.TemporaryDirectory() as cwd:
            msg["cwd"] = cwd
            command_results = [
                (True, json.dumps({"result": {"pane_id": "w2:p1", "workspace_id": "w2"}}), ""),
                (True, json.dumps({"result": {"move_result": {"pane": {"pane_id": "w2:p1", "tab_id": "w2:t2"}}}}), ""),
                (True, "", ""),
                (True, "", ""),
            ]
            with (
                patch.object(relay.Path, "home", return_value=Path(cwd)),
                patch.object(relay, "load_agent_profiles", return_value={"test": {"id": "test", "label": "Test", "argv": ["test-agent"]}}),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=command_results)) as run_command,
                patch.object(relay, "publish_activity", AsyncMock()),
                patch.object(relay.asyncio, "sleep", AsyncMock()),
            ):
                await relay.handle_agent_start_command(ws, msg)

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0][-2:], ("--", "test-agent"))
        self.assertNotIn("--literal task text", calls[0])
        self.assertEqual(calls[1], ("pane", "move", "w2:p1", "--new-tab", "--workspace", "w2", "--label", "mobile-test", "--no-focus"))
        self.assertEqual(calls[2], ("pane", "send-text", "w2:p1", "--literal task text"))
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_agent_clear_starts_replacement_before_closing_old_pane(self):
        ws = FakeWebSocket()
        with tempfile.TemporaryDirectory() as cwd:
            agent = {"pane_id": "w1:p1", "status": "idle", "agent": "codex", "project": "relay", "cwd": cwd}
            with (
                patch.object(relay.Path, "home", return_value=Path(cwd)),
                patch.object(relay, "agent_for_pane", return_value=(agent, "")),
                patch.object(relay, "load_agent_profiles", return_value={"codex": {"id": "codex", "label": "Codex", "argv": ["codex"]}}),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w1:p2", "workspace_id": "w1"}}), ""),
                    (True, json.dumps({"result": {"move_result": {"pane": {"pane_id": "w1:p2", "tab_id": "w1:t3"}}}}), ""),
                    (True, "", ""),
                ])) as run_command,
                patch.object(relay, "publish_activity", AsyncMock()),
            ):
                await relay.handle_agent_clear_command(ws, {
                    "type": "agent_clear",
                    "request_id": "request-4",
                    "pane_id": "w1:p1",
                })

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0][0:2], ("agent", "start"))
        self.assertEqual(calls[1][0:3], ("pane", "move", "w1:p2"))
        self.assertEqual(calls[2], ("pane", "close", "w1:p1"))
        self.assertTrue(ws.messages[-1]["ok"])


if __name__ == "__main__":
    unittest.main()

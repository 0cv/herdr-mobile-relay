import importlib.util
import json
import os
import tempfile
import time
import unittest
import urllib.parse
from pathlib import Path
from types import SimpleNamespace
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


class FakeHeaders:
    def __init__(self, items=None):
        self.items = items or []

    def raw_items(self):
        return self.items


class FakeRequest:
    def __init__(self, path="/", headers=None):
        self.path = path
        self.headers = FakeHeaders(headers)


class RelayHelpersTest(unittest.TestCase):
    def test_protocol_v1_is_the_unversioned_positional_baseline(self):
        self.assertEqual(relay.PROTOCOL_VERSION, 1)
        self.assertEqual(relay.client_protocol_version({}), 1)
        self.assertTrue(relay.client_protocol_matches({}))
        self.assertFalse(relay.client_protocol_matches({"protocol": 2}))
        self.assertFalse(relay.client_protocol_matches({"protocol": True}))

    def test_relay_version_marks_a_modified_checkout_dirty(self):
        results = [
            SimpleNamespace(returncode=0, stdout="abc1234\n"),
            SimpleNamespace(returncode=0, stdout=" M relay/herdr_relay.py\n"),
        ]
        with patch.object(relay.subprocess, "run", side_effect=results):
            self.assertEqual(relay.detect_relay_version(), "abc1234-dirty")

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

    def test_prune_uploads_removes_only_stale_files(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            upload_dir = Path(temp_dir)
            stale = upload_dir / "old.png"
            fresh = upload_dir / "new.png"
            stale.write_bytes(b"stale")
            fresh.write_bytes(b"fresh")
            stale_mtime = time.time() - (relay.UPLOAD_MAX_AGE_DAYS + 1) * 86400
            os.utime(stale, (stale_mtime, stale_mtime))

            with patch.object(relay, "UPLOAD_DIR", upload_dir):
                removed = relay.prune_uploads()

            self.assertEqual(removed, 1)
            self.assertFalse(stale.exists())
            self.assertTrue(fresh.exists())

    def test_prune_uploads_tolerates_missing_directory(self):
        with patch.object(relay, "UPLOAD_DIR", Path("/nonexistent/herdr-test-uploads")):
            self.assertEqual(relay.prune_uploads(), 0)

    def test_agent_profiles_are_detected_from_installed_executables(self):
        def find_executable(name):
            return f"/usr/bin/{name}" if name in {"codex", "claude"} else None

        with patch.object(relay.shutil, "which", side_effect=find_executable):
            profiles = relay.load_agent_profiles()
        self.assertEqual(set(profiles), {"codex", "claude"})
        self.assertEqual(profiles["claude"]["argv"], ["/usr/bin/claude"])

    def test_agent_listing_captures_lightweight_activity_signals(self):
        pane_result = json.dumps({
            "result": {
                "panes": [{
                    "agent": "codex",
                    "agent_status": "working",
                    "cwd": "/home/me/project",
                    "foreground_cwd": "/home/me/project",
                    "pane_id": "w1:p1",
                    "revision": 4,
                    "scroll": {"max_offset_from_bottom": 27},
                    "tab_id": "w1:t1",
                }],
            },
        })
        tab_result = json.dumps({"result": {"tabs": [{"tab_id": "w1:t1", "label": "project"}]}})

        with patch.object(relay, "run_herdr", side_effect=[pane_result, tab_result]):
            agents = relay.get_agents()

        self.assertEqual(
            agents[0]["_activity_fingerprint"],
            ("working", 4, 27, "/home/me/project", "/home/me/project", ""),
        )

    def test_agent_activity_timestamp_tracks_observed_changes_and_events(self):
        def agent(pane_id, fingerprint):
            return {
                "pane_id": pane_id,
                "status": "working",
                "_activity_fingerprint": fingerprint,
            }

        relay.agent_activity_state.clear()
        relay.agent_activity_initialized = False
        try:
            first = relay.stamp_agent_activity([agent("w1:p1", ("working", 10)), agent("w1:p2", ("idle", 20))], 1000)
            unchanged = relay.stamp_agent_activity([agent("w1:p1", ("working", 10)), agent("w1:p2", ("idle", 20))], 2000)
            changed = relay.stamp_agent_activity([agent("w1:p1", ("working", 10)), agent("w1:p2", ("working", 21))], 3000)
            relay.touch_agent_activity("w1:p1", 4000)
            event_updated = relay.stamp_agent_activity([agent("w1:p1", ("working", 10))], 5000)
            new_agent = relay.stamp_agent_activity(
                [agent("w1:p1", ("working", 10)), agent("w1:p3", ("idle", 1))],
                6000,
            )
        finally:
            relay.agent_activity_state.clear()
            relay.agent_activity_initialized = False

        self.assertEqual([item["updated_at"] for item in first], [0, 0])
        self.assertEqual([item["updated_at"] for item in unchanged], [0, 0])
        self.assertEqual([item["updated_at"] for item in changed], [0, 3000])
        self.assertEqual(event_updated[0]["updated_at"], 4000)
        self.assertNotIn("_activity_fingerprint", event_updated[0])
        self.assertEqual(new_agent[1]["updated_at"], 6000)

    def test_claude_history_accumulates_scrolled_snapshot_lines(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        first = ["A", "B", "C", "D", "E", "F", "G", "H", *footer]
        second = ["\x1b[38;2;56;162;223mB\x1b[0m", "C", "D", "E", "F", "G", "H", "I", *footer]
        third = ["C", "D", "E", "F", "G", "H", "I", "J", *footer]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(first), 30)
            relay.merge_claude_history("w1:p1", "\n".join(second), 30)
            merged = relay.merge_claude_history("w1:p1", "\n".join(third), 30)
        finally:
            relay.claude_history_state.clear()

        self.assertEqual(
            [relay.normalized_history_line(line) for line in merged.splitlines()],
            ["A", "B", *third],
        )

    def test_claude_history_ignores_laptop_viewport_navigation(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        first = ["A", "B", "C", "D", "E", "F", "G", "H", *footer]
        advanced = ["B", "C", "D", "E", "F", "G", "H", "I", *footer]
        scrolled_up = ["older", "A", "B", "C", "D", "E", "F", "G", *footer]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(first), 30)
            relay.merge_claude_history("w1:p1", "\n".join(advanced), 30)
            merged = relay.merge_claude_history("w1:p1", "\n".join(scrolled_up), 30)
        finally:
            relay.claude_history_state.clear()

        self.assertEqual(
            [relay.normalized_history_line(line) for line in merged.splitlines()],
            ["A", "B", "C", "D", "E", "F", "G", "H", "I", *footer],
        )

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

            def macos_scandir(path):
                self.assertEqual(Path(path), downloads)
                raise PermissionError("Operation not permitted")

            with (
                patch.object(relay.Path, "home", return_value=home),
                patch.object(relay.os, "scandir", side_effect=macos_scandir),
                patch.object(relay.sys, "platform", "darwin"),
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
    async def test_claude_history_capture_reads_ansi_snapshot(self):
        relay.agent_types["w1:p1"] = "claude"
        relay.claude_history_state.clear()
        try:
            with patch.object(relay, "run_herdr_async", AsyncMock(return_value="First\nSecond")) as read:
                await relay.capture_claude_history("w1:p1")
        finally:
            relay.agent_types.clear()
            relay.claude_history_inflight.clear()

        read.assert_awaited_once_with(
            "pane", "read", "w1:p1",
            "--lines", str(relay.CLAUDE_HISTORY_MAX_LINES),
            "--source", "recent-unwrapped",
            "--format", "ansi",
        )
        self.assertEqual(relay.claude_history_state["w1:p1"]["snapshot"], ["First", "Second"])
        relay.claude_history_state.clear()

    async def test_http_serves_phone_app_without_exposing_websocket(self):
        with patch.object(relay, "AUTH_TOKEN", "secret-token-value"):
            index = await relay.process_request(None, FakeRequest("/"))
            missing = await relay.process_request(None, FakeRequest("/../README.md"))
            unauthorized = await relay.process_request(
                None,
                FakeRequest("/", [("Upgrade", "websocket"), ("Origin", "https://relay.example.com")]),
            )
            authorized = await relay.process_request(
                None,
                FakeRequest(
                    "/?token=secret-token-value",
                    [("Upgrade", "websocket"), ("Origin", "https://relay.example.com")],
                ),
            )

        self.assertEqual(index.status_code, 200)
        self.assertIn(b"Herdr Mobile Relay", index.body)
        self.assertEqual(index.headers["Content-Type"], "text/html; charset=utf-8")
        self.assertEqual(missing.status_code, 404)
        self.assertEqual(unauthorized.status_code, 401)
        self.assertIsNone(authorized)

    async def test_health_preserves_plain_response_and_healthz_reports_details(self):
        health = await relay.process_request(None, FakeRequest("/health"))
        self.assertEqual(health.status_code, 200)
        self.assertEqual(health.headers["Content-Type"], "text/plain; charset=utf-8")
        self.assertEqual(health.body, b"ok\n")

        healthz = await relay.process_request(None, FakeRequest("/healthz"))
        self.assertEqual(healthz.status_code, 200)
        self.assertEqual(healthz.headers["Content-Type"], "application/json; charset=utf-8")
        payload = json.loads(healthz.body)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["protocol"], relay.PROTOCOL_VERSION)
        self.assertEqual(payload["version"], relay.RELAY_VERSION)

    async def test_incompatible_client_protocol_rejects_mutation(self):
        ws = FakeWebSocket()
        await relay.reject_incompatible_client_protocol(ws, {
            "type": "submit_prompt",
            "protocol": relay.PROTOCOL_VERSION + 1,
            "request_id": "request-1",
        })
        self.assertEqual(ws.messages[0]["type"], "command_result")
        self.assertEqual(ws.messages[0]["request_id"], "request-1")
        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("Incompatible app protocol", ws.messages[0]["error"])

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

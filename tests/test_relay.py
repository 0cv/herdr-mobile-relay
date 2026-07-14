import asyncio
import importlib.util
import json
import os
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.parse
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch


RELAY_PATH = Path(__file__).parents[1] / "relay" / "herdr_relay.py"
SPEC = importlib.util.spec_from_file_location("herdr_relay_under_test", RELAY_PATH)
relay = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(relay)


def inherited_socket_path(sock):
    descriptor_dir = Path("/proc/self/fd")
    if not descriptor_dir.is_dir():
        descriptor_dir = Path("/dev/fd")
    return str(descriptor_dir / str(sock.fileno()))


class FakeWebSocket:
    def __init__(self):
        self.messages = []

    async def send(self, payload):
        self.messages.append(json.loads(payload))

    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


class FakeHeaders:
    def __init__(self, items=None):
        self.items = items or []

    def raw_items(self):
        return self.items


class FakeRequest:
    def __init__(self, path="/", headers=None):
        self.path = path
        self.headers = FakeHeaders(headers)


class ClaudeHistoryIsolationMixin:
    """Point history persistence at a per-test directory so tests never touch
    the real cache and never leak state into each other."""

    def setUp(self):
        super().setUp()
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        patcher = patch.object(relay, "CLAUDE_HISTORY_DIR", Path(temp.name))
        patcher.start()
        self.addCleanup(patcher.stop)
        relay.claude_history_state.clear()
        relay.claude_history_save_times.clear()
        self.addCleanup(relay.claude_history_state.clear)
        self.addCleanup(relay.claude_history_save_times.clear)


class RelayHelpersTest(ClaudeHistoryIsolationMixin, unittest.TestCase):
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

    def test_relay_env_generates_one_stable_internal_instance_id(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            env_file = Path(temp_dir) / "relay.env"
            env_file.write_text("HERDR_RELAY_INSTANCE_ID=''\n")
            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    (
                        '. "$1"; ensure_relay_env "$2"; load_relay_env "$2"; '
                        'first="$HERDR_RELAY_INSTANCE_ID"; ensure_relay_env "$2"; '
                        'unset HERDR_RELAY_INSTANCE_ID; load_relay_env "$2"; '
                        'printf "%s\\n%s\\n" "$first" "$HERDR_RELAY_INSTANCE_ID"'
                    ),
                    "bash",
                    str(root / "relay" / "common.sh"),
                    str(env_file),
                ],
                capture_output=True,
                text=True,
            )
            instance_ids = result.stdout.splitlines()[-2:]
            mode = env_file.stat().st_mode & 0o777

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(instance_ids), 2)
        self.assertTrue(instance_ids[0])
        self.assertEqual(instance_ids[0], instance_ids[1])
        self.assertEqual(mode, 0o600)

    def test_respond_keys_select_first_middle_and_last(self):
        self.assertEqual(relay.respond_keys(0, 3), ["Enter"])
        self.assertEqual(relay.respond_keys(1, 3), ["Down", "Enter"])
        self.assertEqual(relay.respond_keys(2, 3), ["Escape"])

    def test_agent_message_is_canonical_and_pane_sorted(self):
        first = relay.agents_message([
            {"pane_id": "w1:p2", "status": "idle", "agent": "codex"},
            {"agent": "claude", "status": "working", "pane_id": "w1:p1"},
        ])
        second = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "claude"},
            {"status": "idle", "agent": "codex", "pane_id": "w1:p2"},
        ])

        self.assertEqual(first, second)
        self.assertEqual(
            [agent["pane_id"] for agent in json.loads(first)["agents"]],
            ["w1:p1", "w1:p2"],
        )

    def test_poll_interval_stays_fast_for_empty_or_active_agent_lists(self):
        self.assertEqual(relay.poll_interval_for(None), relay.POLL_INTERVAL)
        self.assertEqual(relay.poll_interval_for([]), relay.POLL_INTERVAL)
        for status in ("working", "blocked", "unknown"):
            with self.subTest(status=status):
                self.assertEqual(
                    relay.poll_interval_for([{"status": status}]),
                    relay.POLL_INTERVAL,
                )

    def test_poll_interval_slows_only_when_every_agent_is_inactive(self):
        self.assertEqual(
            relay.poll_interval_for([{"status": "idle"}, {"status": "done"}]),
            relay.IDLE_POLL_INTERVAL,
        )
        self.assertEqual(
            relay.poll_interval_for([{"status": "idle"}, {"status": "working"}]),
            relay.POLL_INTERVAL,
        )

    def test_push_payload_contains_only_the_safe_approve_action(self):
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
        self.assertEqual(
            target(payload["action_urls"]["approve"])["notification_id"], "event-1"
        )
        self.assertEqual(
            payload["actions"], [{"action": "approve", "title": "Approve once"}]
        )
        self.assertNotIn("deny", payload["action_urls"])

    def test_finished_push_payload_opens_agent_without_approval_actions(self):
        payload = relay.finished_push_payload({
            "host": "fedora",
            "pane_id": "w1:p2",
            "project": "relay",
            "agent": "codex",
        })
        target = json.loads(urllib.parse.unquote(payload["url"].split("#notify=", 1)[1]))

        self.assertEqual(payload["title"], "relay finished")
        self.assertEqual(payload["actions"], [])
        self.assertEqual(payload["action_urls"], {})
        self.assertEqual(target["pane_id"], "w1:p2")

    def test_finished_notification_is_emitted_once_per_work_cycle(self):
        pane = "w1:p1"
        relay.finished_notification_panes.clear()
        try:
            self.assertFalse(relay.register_finished_notification(pane, "working", "idle"))
            self.assertTrue(relay.register_finished_notification(pane, "idle", "working"))
            self.assertFalse(relay.register_finished_notification(pane, "idle", "working"))
            self.assertFalse(relay.register_finished_notification(pane, "working", "idle"))
            self.assertTrue(relay.register_finished_notification(pane, "done", "working"))
        finally:
            relay.finished_notification_panes.clear()

    def test_push_subscription_stores_finished_preference(self):
        subscription = {
            "endpoint": "https://push.example.test/one",
            "keys": {"p256dh": "key", "auth": "auth"},
        }
        with (
            patch.object(relay, "load_push_subscriptions", return_value=[]),
            patch.object(relay, "save_push_subscriptions") as save,
        ):
            stored = relay.store_push_subscription(
                subscription,
                client_id="phone",
                notify_finished=True,
            )

        self.assertTrue(stored)
        self.assertTrue(save.call_args.args[0][0]["notify_finished"])

    def test_finished_push_only_targets_opted_in_subscriptions(self):
        subscriptions = [
            {"subscription": {"endpoint": "https://push.example.test/one"}, "notify_finished": True},
            {"subscription": {"endpoint": "https://push.example.test/two"}},
        ]
        with (
            patch.object(relay, "load_push_subscriptions", return_value=subscriptions),
            patch.object(relay, "webpush") as webpush,
            patch.object(relay, "remove_push_subscriptions"),
        ):
            relay.send_finished_webpush_notifications({"pane_id": "w1:p1"})

        self.assertEqual(webpush.call_count, 1)
        self.assertEqual(
            webpush.call_args.kwargs["subscription_info"]["endpoint"],
            "https://push.example.test/one",
        )

    def test_plugin_manifest_is_marketplace_ready_at_repository_root(self):
        root = RELAY_PATH.parents[1]
        manifest = (root / "herdr-plugin.toml").read_text()

        self.assertFalse((root / "relay" / "herdr-plugin.toml").exists())
        self.assertIn('id = "herdr-mobile-relay.events"', manifest)
        self.assertIn('version = "0.5.1"', manifest)
        self.assertIn('id = "setup"', manifest)
        self.assertIn('command = "herdr-mobile-relay.events.setup"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "setup"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-setup-menu.sh"]', manifest)
        self.assertIn('id = "quick-start"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "quick-start"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-quick-start.sh"]', manifest)
        self.assertIn('id = "install-service"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "install-service"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-install-service.sh"]', manifest)
        self.assertIn('id = "stable-teardown"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "stable-teardown"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-stable-teardown.sh"]', manifest)
        self.assertIn('command = ["sh", "relay/plugin-on-event.sh"]', manifest)
        self.assertIn('[[build]]', manifest)
        self.assertIn('command = ["sh", "relay/plugin-build.sh"]', manifest)
        self.assertIn('id = "status"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "status"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-status.sh"]', manifest)
        plugin_installer = (root / "relay" / "plugin-install-service.sh").read_text()
        self.assertIn('. "$SCRIPT_DIR/common.sh"', plugin_installer)
        self.assertIn('"$SCRIPT_DIR/stable-setup.sh"', plugin_installer)

    def test_plugin_build_soft_fails_without_uv_or_network(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_curl = temp / "curl"
            fake_curl.write_text("#!/bin/sh\nexit 1\n")
            fake_curl.chmod(0o700)
            (temp / "env").symlink_to("/usr/bin/env")
            (temp / "sh").symlink_to("/bin/sh")
            env = os.environ.copy()
            env["PATH"] = str(temp)
            env["HOME"] = str(temp)
            env["HERDR_MOBILE_RELAY_NO_AUTO_SETUP"] = "1"

            result = subprocess.run(
                ["/bin/sh", str(root / "relay" / "plugin-build.sh")],
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Quick Start offers interactive installation", result.stderr)

    def test_plugin_build_soft_fails_when_uv_pre_warm_fails(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_uv = temp / "uv"
            fake_uv.write_text("#!/bin/sh\nexit 42\n")
            fake_uv.chmod(0o700)
            env = os.environ.copy()
            env["PATH"] = str(temp)
            env["HOME"] = str(temp)
            env["HERDR_MOBILE_RELAY_NO_AUTO_SETUP"] = "1"

            result = subprocess.run(
                ["/bin/sh", str(root / "relay" / "plugin-build.sh")],
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("dependency pre-warm failed", result.stderr)

    def test_plugin_event_uses_uv_when_python3_is_unavailable(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_uv = temp / "uv"
            fake_uv.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\"\n")
            fake_uv.chmod(0o700)
            env = os.environ.copy()
            env["PATH"] = str(temp)
            env["HOME"] = str(temp)

            result = subprocess.run(
                ["/bin/sh", str(root / "relay" / "plugin-on-event.sh")],
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            result.stdout.strip(),
            f"run --quiet python {root / 'relay' / 'on_event.py'}",
        )

    def test_plugin_build_detaches_post_install_waiter_inside_herdr(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            args_file = temp / "nohup-args.txt"
            fake_uv = temp / "uv"
            fake_uv.write_text("#!/bin/sh\nexit 0\n")
            fake_uv.chmod(0o700)
            fake_nohup = temp / "nohup"
            fake_nohup.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$NOHUP_ARGS"\n')
            fake_nohup.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "PATH": f"{temp}:/usr/bin:/bin",
                "HOME": str(temp),
                "NOHUP_ARGS": str(args_file),
            })

            socket_reader, socket_writer = socket.socketpair()
            with socket_reader, socket_writer:
                env["HERDR_SOCKET_PATH"] = inherited_socket_path(socket_reader)
                result = subprocess.run(
                    ["/bin/sh", str(root / "relay" / "plugin-build.sh")],
                    capture_output=True,
                    text=True,
                    env=env,
                    pass_fds=(socket_reader.fileno(),),
                )
                for _attempt in range(20):
                    if args_file.exists():
                        break
                    time.sleep(0.01)
                nohup_args = args_file.read_text()
            # The waiter must run from a copy outside the checkout: herdr
            # deletes the staging checkout right after the build exits.
            waiter_copy = temp / ".cache" / "herdr-mobile-relay" / "post-install.sh"
            waiter_copy_exists = waiter_copy.exists()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("setup will open automatically", result.stderr)
        self.assertIn(".cache/herdr-mobile-relay/post-install.sh", nohup_args)
        self.assertNotIn(str(root), nohup_args)
        self.assertTrue(waiter_copy_exists)
        self.assertIn("0.5.1", nohup_args)

    def test_plugin_build_releases_captured_pipes_before_waiter_exits(self):
        # Regression: herdr registers the plugin only after the install
        # returns, and the install waits on the build's stdout/stderr. A
        # waiter that inherits those pipes (e.g. via a `( cd .. && cmd & )`
        # wrapper) deadlocks the flow until its own timeout. The build must
        # return to a pipe-capturing parent immediately, with the waiter
        # still running.
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_uv = temp / "uv"
            fake_uv.write_text("#!/bin/sh\nexit 0\n")
            fake_uv.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "PATH": f"{temp}:/usr/bin:/bin",
                "HOME": str(temp),
                "HERDR_PLUGIN_REGISTRY": str(temp / "never-written.json"),
                "HERDR_POST_INSTALL_ATTEMPTS": "6",
                "HERDR_POST_INSTALL_DELAY": "1",
                "HERDR_POST_INSTALL_LOCK_DIR": str(temp / "lock"),
            })

            socket_reader, socket_writer = socket.socketpair()
            with socket_reader, socket_writer:
                env["HERDR_SOCKET_PATH"] = inherited_socket_path(socket_reader)
                started = time.monotonic()
                result = subprocess.run(
                    ["/bin/sh", str(root / "relay" / "plugin-build.sh")],
                    capture_output=True,
                    text=True,
                    env=env,
                    pass_fds=(socket_reader.fileno(),),
                )
                elapsed = time.monotonic() - started
            # Give the detached waiter time to exit before temp cleanup.
            time.sleep(0.2)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("setup will open automatically", result.stderr)
        self.assertLess(elapsed, 4.0, "build held the installer's pipes for the waiter's lifetime")

    def test_post_install_opens_already_matching_registered_version(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            registry = temp / "plugins.json"
            registry.write_text(json.dumps([{
                "plugin_id": "herdr-mobile-relay.events",
                "version": "0.5.1",
                "enabled": True,
                "plugin_root": str(root),
                "actions": [{"id": "setup"}],
            }]))
            args_file = temp / "herdr-args.txt"
            fake_herdr = temp / "herdr"
            fake_herdr.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_herdr.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "ARGS_FILE": str(args_file),
                "HERDR_BIN_PATH": str(fake_herdr),
                "HERDR_ENV": "1",
                "HERDR_PANE_ID": "w1:p1",
                "HERDR_PLUGIN_REGISTRY": str(registry),
                "HERDR_POST_INSTALL_ATTEMPTS": "1",
                "HERDR_POST_INSTALL_DELAY": "0",
                "HERDR_POST_INSTALL_LOCK_DIR": str(temp / "lock"),
            })

            socket_reader, socket_writer = socket.socketpair()
            with socket_reader, socket_writer:
                env["HERDR_SOCKET_PATH"] = inherited_socket_path(socket_reader)
                result = subprocess.run(
                    ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), "0.5.1", "0"],
                    capture_output=True,
                    text=True,
                    env=env,
                    pass_fds=(socket_reader.fileno(),),
                )
                invoked_args = args_file.read_text().splitlines()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(invoked_args, [
            "plugin", "pane", "open",
            "--plugin", "herdr-mobile-relay.events",
            "--entrypoint", "setup",
            "--placement", "zoomed",
            "--focus",
            "--target-pane", "w1:p1",
        ])

    def test_post_install_uses_live_socket_without_runtime_context(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            registry = temp / "plugins.json"
            registry.write_text(json.dumps([{
                "plugin_id": "herdr-mobile-relay.events",
                "version": "0.5.1",
                "enabled": True,
                "plugin_root": str(root),
                "actions": [{"id": "setup"}],
            }]))
            args_file = temp / "herdr-args.txt"
            fake_herdr = temp / "herdr"
            fake_herdr.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_herdr.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "ARGS_FILE": str(args_file),
                "HERDR_BIN_PATH": str(fake_herdr),
                "HERDR_PLUGIN_REGISTRY": str(registry),
                "HERDR_POST_INSTALL_ATTEMPTS": "1",
                "HERDR_POST_INSTALL_DELAY": "0",
                "HERDR_POST_INSTALL_LOCK_DIR": str(temp / "lock"),
            })
            env.pop("HERDR_ENV", None)
            env.pop("HERDR_PANE_ID", None)

            socket_reader, socket_writer = socket.socketpair()
            with socket_reader, socket_writer:
                env["HERDR_SOCKET_PATH"] = inherited_socket_path(socket_reader)
                result = subprocess.run(
                    ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), "0.5.1", "0"],
                    capture_output=True,
                    text=True,
                    env=env,
                    pass_fds=(socket_reader.fileno(),),
                )
                invoked_args = args_file.read_text().splitlines()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(invoked_args, [
            "plugin", "pane", "open",
            "--plugin", "herdr-mobile-relay.events",
            "--entrypoint", "setup",
            "--placement", "overlay",
            "--focus",
        ])

    def test_post_install_ignores_stale_registered_version(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            registry = temp / "plugins.json"
            registry.write_text(json.dumps([{
                "plugin_id": "herdr-mobile-relay.events",
                "version": "0.3.0",
                "enabled": True,
                "plugin_root": str(root),
                "actions": [{"id": "setup"}],
            }]))
            args_file = temp / "herdr-args.txt"
            fake_herdr = temp / "herdr"
            fake_herdr.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_herdr.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "ARGS_FILE": str(args_file),
                "HERDR_BIN_PATH": str(fake_herdr),
                "HERDR_ENV": "1",
                "HERDR_SOCKET_PATH": str(temp / "herdr.sock"),
                "HERDR_PANE_ID": "w1:p1",
                "HERDR_PLUGIN_REGISTRY": str(registry),
                "HERDR_POST_INSTALL_ATTEMPTS": "1",
                "HERDR_POST_INSTALL_DELAY": "0",
                "HERDR_POST_INSTALL_LOCK_DIR": str(temp / "lock"),
                "XDG_CURRENT_DESKTOP": "unknown",
                "KONSOLE_VERSION": "",
                "GNOME_TERMINAL_SERVICE": "",
            })

            result = subprocess.run(
                ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), "0.5.1", "0"],
                capture_output=True,
                text=True,
                env=env,
            )
            invoked = args_file.exists()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(invoked)

    def test_post_install_does_not_replace_lock_before_pid_is_written(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            lock_dir = temp / "lock"
            lock_dir.mkdir()
            env = os.environ.copy()
            env.update({
                "HERDR_POST_INSTALL_LOCK_DIR": str(lock_dir),
                "HERDR_POST_INSTALL_ATTEMPTS": "1",
                "HERDR_POST_INSTALL_DELAY": "0",
            })

            result = subprocess.run(
                ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), "0.5.1", "0"],
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(lock_dir.is_dir())
            self.assertFalse((lock_dir / "pid").exists())

    def test_plugin_setup_menu_routes_setup_and_teardown_modes(self):
        root = RELAY_PATH.parents[1]
        menu_source = root / "relay" / "plugin-setup-menu.sh"
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            menu = temp / menu_source.name
            menu.write_text(menu_source.read_text())
            menu.chmod(0o700)
            for name, output in (
                ("plugin-quick-start.sh", "quick"),
                ("plugin-install-service.sh", "stable"),
                ("plugin-stable-teardown.sh", "teardown"),
            ):
                script = temp / name
                script.write_text(f"#!/bin/sh\necho {output}\n")
                script.chmod(0o700)

            quick = subprocess.run(
                ["bash", str(menu)], input="1\n", capture_output=True, text=True, check=True,
            )
            stable = subprocess.run(
                ["bash", str(menu)], input="2\n", capture_output=True, text=True, check=True,
            )
            teardown = subprocess.run(
                ["bash", str(menu)], input="3\n", capture_output=True, text=True, check=True,
            )

        self.assertTrue(quick.stdout.rstrip().endswith("quick"))
        self.assertTrue(stable.stdout.rstrip().endswith("stable"))
        self.assertTrue(teardown.stdout.rstrip().endswith("teardown"))

    def test_relay_health_check_retries_until_detailed_health_is_ready(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            count_file = temp / "curl-count"
            fake_curl = temp / "curl"
            fake_curl.write_text(
                "#!/bin/bash\n"
                "count=0\n"
                "[ ! -f \"$COUNT_FILE\" ] || count=$(<\"$COUNT_FILE\")\n"
                "count=$((count + 1))\n"
                "printf '%s\\n' \"$count\" > \"$COUNT_FILE\"\n"
                "[ \"$count\" -ge 3 ] || exit 22\n"
                "printf '%s\\n' '{\"status\": \"ok\", \"instance\": \"instance-a\", \"version\": \"abc1234\", \"protocol\": 1}'\n"
            )
            fake_curl.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "COUNT_FILE": str(count_file),
                "PATH": f"{temp}:/usr/bin:/bin",
            })

            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    '. "$1"; wait_for_relay_health 8399 3 0',
                    "bash",
                    str(root / "relay" / "common.sh"),
                ],
                capture_output=True,
                text=True,
                env=env,
            )

            call_count = count_file.read_text().strip()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(call_count, "3")
        self.assertEqual(
            result.stdout.strip(),
            '{"status": "ok", "instance": "instance-a", "version": "abc1234", "protocol": 1}',
        )

    def test_relay_health_check_rejects_incomplete_payload(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_curl = temp / "curl"
            fake_curl.write_text("#!/bin/sh\nprintf '%s\\n' '{\"status\": \"ok\"}'\n")
            fake_curl.chmod(0o700)
            env = os.environ.copy()
            env["PATH"] = f"{temp}:/usr/bin:/bin"

            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    '. "$1"; wait_for_relay_health 8399 1 0',
                    "bash",
                    str(root / "relay" / "common.sh"),
                ],
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertNotEqual(result.returncode, 0)

    def test_service_installers_require_relay_health(self):
        root = RELAY_PATH.parents[1]
        macos = (root / "relay" / "install-service.sh").read_text()
        linux = (root / "relay" / "install-systemd-user-service.sh").read_text()

        self.assertIn('wait_for_relay_health "$PORT"', macos)
        self.assertIn('wait_for_relay_health "$PORT"', linux)
        self.assertIn('systemctl --user restart "$LABEL"', linux)
        self.assertIn("launchctl print", macos)
        self.assertIn("journalctl --user", linux)

    def test_terminal_launcher_uses_only_recognized_terminals(self):
        root = RELAY_PATH.parents[1]
        launcher = root / "relay" / "plugin-open-terminal.sh"
        setup_command = root / "relay" / "plugin-setup-terminal.command"
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            args_file = temp / "terminal-args.txt"
            fake_uname = temp / "uname"
            fake_uname.write_text("#!/bin/sh\necho Linux\n")
            fake_uname.chmod(0o700)
            fake_konsole = temp / "konsole"
            fake_konsole.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_konsole.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "ARGS_FILE": str(args_file),
                "PATH": f"{temp}:/usr/bin:/bin",
                "XDG_CURRENT_DESKTOP": "KDE",
                "KONSOLE_VERSION": "260403",
            })

            subprocess.run(["bash", str(launcher), str(root)], check=True, env=env)
            known_args = args_file.read_text().splitlines()

            fake_konsole.unlink()
            env.pop("KONSOLE_VERSION")
            env["XDG_CURRENT_DESKTOP"] = "XFCE"
            unknown = subprocess.run(
                ["bash", str(launcher), str(root)], capture_output=True, text=True, env=env,
            )

            fake_uname.write_text("#!/bin/sh\necho Darwin\n")
            fake_open = temp / "open"
            fake_open.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_open.chmod(0o700)
            env["TERM_PROGRAM"] = "Apple_Terminal"
            subprocess.run(["bash", str(launcher), str(root)], check=True, env=env)
            apple_args = args_file.read_text().splitlines()

            fake_uname.write_text("#!/bin/sh\necho Linux\n")
            fake_gnome_terminal = temp / "gnome-terminal"
            fake_gnome_terminal.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_gnome_terminal.chmod(0o700)
            env.pop("TERM_PROGRAM")
            env["XDG_CURRENT_DESKTOP"] = "GNOME"
            subprocess.run(["bash", str(launcher), str(root)], check=True, env=env)
            gnome_args = args_file.read_text().splitlines()

        self.assertEqual(known_args, ["--new-tab", "-e", str(setup_command)])
        self.assertNotEqual(unknown.returncode, 0)
        self.assertEqual(apple_args, ["-a", "Terminal", str(setup_command)])
        self.assertEqual(gnome_args, ["--", str(setup_command)])

    def test_plugin_config_is_stable_and_migrates_checkout_env(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            script_dir = temp / "relay"
            config_dir = temp / "plugin-config"
            script_dir.mkdir()
            (script_dir / ".env").write_text("HERDR_RELAY_TOKEN=legacy-token\n")
            (script_dir / "push").mkdir()
            (script_dir / "push" / "subscriptions.json").write_text("{}\n")
            env = os.environ.copy()
            env.pop("HERDR_RELAY_ENV", None)
            env["HERDR_PLUGIN_CONFIG_DIR"] = str(config_dir)

            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    '. "$1"; relay_env_file "$2"',
                    "bash",
                    str(root / "relay" / "common.sh"),
                    str(script_dir),
                ],
                check=True,
                capture_output=True,
                text=True,
                env=env,
            )

            plugin_env = config_dir / "relay.env"
            self.assertEqual(result.stdout.strip(), str(plugin_env))
            self.assertEqual(plugin_env.read_text(), "HERDR_RELAY_TOKEN=legacy-token\n")
            self.assertEqual(plugin_env.stat().st_mode & 0o777, 0o600)
            self.assertEqual(config_dir.stat().st_mode & 0o777, 0o700)
            self.assertEqual((config_dir / "push" / "subscriptions.json").read_text(), "{}\n")

    def test_plugin_runtime_data_uses_the_stable_config_directory(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            config_dir = Path(temp_dir) / "plugin-config"
            with patch.dict(
                relay.os.environ,
                {"HERDR_PLUGIN_CONFIG_DIR": str(config_dir)},
                clear=False,
            ):
                relay.os.environ.pop("HERDR_RELAY_ENV", None)
                self.assertEqual(relay.default_runtime_dir(), config_dir)

    def test_checkout_config_remains_the_default_without_plugin_context(self):
        root = RELAY_PATH.parents[1]
        env = os.environ.copy()
        env.pop("HERDR_RELAY_ENV", None)
        env.pop("HERDR_PLUGIN_CONFIG_DIR", None)
        result = subprocess.run(
            [
                "bash",
                "-c",
                '. "$1"; relay_env_file "$2"',
                "bash",
                str(root / "relay" / "common.sh"),
                str(root / "relay"),
            ],
            check=True,
            capture_output=True,
            text=True,
            env=env,
        )

        self.assertEqual(result.stdout.strip(), str(root / "relay" / ".env"))

    def test_plugin_action_opens_requested_managed_pane(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            fake_herdr = temp / "herdr"
            args_file = temp / "args.txt"
            fake_herdr.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$ARGS_FILE"\n')
            fake_herdr.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "ARGS_FILE": str(args_file),
                "HERDR_BIN_PATH": str(fake_herdr),
                "HERDR_PLUGIN_ID": "herdr-mobile-relay.events",
                "HERDR_PANE_ID": "w1:p2",
            })

            subprocess.run(
                [str(root / "relay" / "open-plugin-pane.sh"), "quick-start"],
                check=True,
                env=env,
            )

            self.assertEqual(args_file.read_text().splitlines(), [
                "plugin", "pane", "open",
                "--plugin", "herdr-mobile-relay.events",
                "--entrypoint", "quick-start",
                "--placement", "zoomed",
                "--focus",
                "--target-pane", "w1:p2",
            ])

            subprocess.run(
                [str(root / "relay" / "open-plugin-pane.sh"), "setup"],
                check=True,
                env=env,
            )

            self.assertEqual(args_file.read_text().splitlines(), [
                "plugin", "pane", "open",
                "--plugin", "herdr-mobile-relay.events",
                "--entrypoint", "setup",
                "--placement", "zoomed",
                "--focus",
                "--target-pane", "w1:p2",
            ])

            subprocess.run(
                [str(root / "relay" / "open-plugin-pane.sh"), "status"],
                check=True,
                env=env,
            )

            self.assertEqual(args_file.read_text().splitlines(), [
                "plugin", "pane", "open",
                "--plugin", "herdr-mobile-relay.events",
                "--entrypoint", "status",
                "--placement", "overlay",
                "--focus",
                "--target-pane", "w1:p2",
            ])

    def test_plugin_status_recognizes_installed_service_without_systemd_bus(self):
        if os.uname().sysname != "Linux":
            self.skipTest("systemd service status applies only to Linux")
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            service_dir = home / ".config" / "systemd" / "user"
            service_dir.mkdir(parents=True)
            (service_dir / "herdr-mobile-relay.service").write_text("[Service]\n")
            env = os.environ.copy()
            env.update({
                "HOME": str(home),
                "HERDR_PLUGIN_CONFIG_DIR": str(home / "plugin-config"),
                "HERDR_RELAY_PORT": "1",
            })

            result = subprocess.run(
                ["bash", str(root / "relay" / "plugin-status.sh")],
                check=True,
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertIn("Service:      installed (", result.stdout)

    def test_macos_service_passes_the_stable_env_path(self):
        installer = (RELAY_PATH.parent / "install-service.sh").read_text()
        self.assertIn("<key>EnvironmentVariables</key>", installer)
        self.assertIn("<key>HERDR_RELAY_ENV</key>", installer)

    def test_checkout_command_rejects_plugin_service_config(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            checkout_env = home / "checkout" / "relay" / ".env"
            plugin_env = home / "plugin-config" / "relay.env"
            service_file = home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
            checkout_env.parent.mkdir(parents=True)
            plugin_env.parent.mkdir(parents=True)
            service_file.parent.mkdir(parents=True)
            checkout_env.touch()
            plugin_env.touch()
            service_file.write_text(f"Environment=HERDR_RELAY_ENV={plugin_env}\n")
            env = os.environ.copy()
            env["HOME"] = str(home)

            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    '. "$1"; assert_service_env_matches "$2"',
                    "bash",
                    str(root / "relay" / "common.sh"),
                    str(checkout_env),
                ],
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 1)
            self.assertIn(f"This command resolved: {checkout_env}", result.stderr)
            self.assertIn(f"Installed service uses: {plugin_env}", result.stderr)

    def test_command_accepts_the_service_config_it_resolved(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            relay_env = home / "plugin-config" / "relay.env"
            service_file = home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
            relay_env.parent.mkdir(parents=True)
            service_file.parent.mkdir(parents=True)
            relay_env.touch()
            service_file.write_text(f"Environment=HERDR_RELAY_ENV={relay_env}\n")
            env = os.environ.copy()
            env["HOME"] = str(home)

            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    '. "$1"; assert_service_env_matches "$2"',
                    "bash",
                    str(root / "relay" / "common.sh"),
                    str(relay_env),
                ],
                capture_output=True,
                text=True,
                env=env,
            )

            self.assertEqual(result.returncode, 0, result.stderr)

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

    def test_finished_agents_read_done_until_viewed(self):
        pane = "w1:p1"
        relay.unseen_done_panes.clear()
        relay.acknowledged_done_panes.clear()
        try:
            relay.register_status_transition(pane, "working", None)
            self.assertEqual(relay.displayed_status(pane, "working"), "working")

            relay.register_status_transition(pane, "idle", "working")
            self.assertEqual(relay.displayed_status(pane, "idle"), "done")

            relay.unseen_done_panes.discard(pane)
            relay.register_status_transition(pane, "idle", "idle")
            self.assertEqual(relay.displayed_status(pane, "idle"), "idle")

            relay.register_status_transition(pane, "idle", "working")
            relay.register_status_transition(pane, "idle", "idle")
            self.assertEqual(relay.displayed_status(pane, "idle"), "done")

            relay.register_status_transition(pane, "idle", "done", focused=True)
            self.assertEqual(relay.displayed_status(pane, "idle"), "idle")

            relay.register_status_transition(pane, "blocked", "idle")
            relay.register_status_transition(pane, "idle", "blocked")
            self.assertEqual(relay.displayed_status(pane, "idle"), "done")
        finally:
            relay.unseen_done_panes.clear()
            relay.acknowledged_done_panes.clear()

    def test_raw_done_status_stays_idle_after_view_until_work_restarts(self):
        pane = "w1:p1"
        relay.unseen_done_panes.clear()
        relay.acknowledged_done_panes.clear()
        try:
            relay.register_status_transition(pane, "working", "idle")
            relay.register_status_transition(pane, "done", "working")
            self.assertEqual(relay.displayed_status(pane, "done"), "done")

            relay.acknowledged_done_panes.add(pane)
            self.assertEqual(relay.displayed_status(pane, "done"), "idle")
            self.assertEqual(relay.displayed_status(pane, "idle"), "idle")

            relay.register_status_transition(pane, "working", "done")
            relay.register_status_transition(pane, "done", "working")
            self.assertEqual(relay.displayed_status(pane, "done"), "done")
        finally:
            relay.unseen_done_panes.clear()
            relay.acknowledged_done_panes.clear()

    def test_idle_at_startup_is_not_done(self):
        relay.unseen_done_panes.clear()
        relay.acknowledged_done_panes.clear()
        try:
            relay.register_status_transition("w2:p1", "idle", None)
            self.assertEqual(relay.displayed_status("w2:p1", "idle"), "idle")
        finally:
            relay.unseen_done_panes.clear()
            relay.acknowledged_done_panes.clear()

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

    def test_claude_history_recovers_after_terminal_rewrites_recent_lines(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        output = [f"output {i}" for i in range(1, 6)]
        box = ["┌ This command requires approval", "│ Yes", "│ No", "│ hint", "└ esc to cancel"]
        collapsed = output + ["⏺ Bash(command ran)", "answer 1", "answer 2"]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(output + box + footer), 100)
            first = relay.merge_claude_history("w1:p1", "\n".join(collapsed + footer), 100)
            second = relay.merge_claude_history("w1:p1", "\n".join(collapsed + ["FINAL"] + footer), 100)
        finally:
            relay.claude_history_state.clear()

        self.assertIn("requires approval", first)
        self.assertNotIn("answer 1", first)
        self.assertNotIn("requires approval", second)
        self.assertIn("answer 2", second)
        self.assertIn("FINAL", second)

    def test_claude_history_rebases_when_divergence_persists_unchanged(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        output = [f"output {i}" for i in range(1, 6)]
        box = ["┌ This command requires approval", "│ Yes", "│ No", "│ hint", "└ esc to cancel"]
        collapsed = output + ["⏺ Bash(command ran)", "answer 1", "answer 2"]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(output + box + footer), 100)
            relay.merge_claude_history("w1:p1", "\n".join(collapsed + footer), 100)
            healed = relay.merge_claude_history("w1:p1", "\n".join(collapsed + footer), 100)
        finally:
            relay.claude_history_state.clear()

        self.assertNotIn("requires approval", healed)
        self.assertIn("answer 2", healed)

    def test_final_history_capture_bypasses_gate_and_survives_inflight(self):
        agent = {"pane_id": "w1:p1", "agent": "claude"}

        def reset():
            relay.claude_history_pending_captures.clear()
            relay.claude_history_capture_times.clear()
            relay.claude_history_inflight.clear()

        reset()
        try:
            with (
                # A plain Mock: auto-speccing would install an AsyncMock whose
                # call result is an unawaited coroutine.
                patch.object(relay, "capture_claude_history", Mock()),
                patch.object(relay.asyncio, "create_task") as create_task,
            ):
                relay.schedule_claude_history_capture(agent, timestamp=100.0)
                relay.schedule_claude_history_capture(agent, timestamp=101.0)
                self.assertEqual(create_task.call_count, 1)

                relay.schedule_claude_history_capture(agent, timestamp=102.0, force=True)
                self.assertEqual(create_task.call_count, 2)

                relay.claude_history_inflight.add("w1:p1")
                relay.schedule_claude_history_capture(agent, timestamp=103.0, force=True)
                self.assertEqual(create_task.call_count, 2)
                self.assertIn("w1:p1", relay.claude_history_pending_captures)

                relay.claude_history_inflight.clear()
                relay.schedule_claude_history_capture(agent, timestamp=104.0)
                self.assertEqual(create_task.call_count, 3)
                self.assertNotIn("w1:p1", relay.claude_history_pending_captures)
        finally:
            reset()

    def test_claude_history_grows_past_repeated_content(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        block = ["$ make check", "Ran 40 tests", "OK"]
        seed = ["start", *block, "middle", *block, "u1", "u2", "u3", "u4"]
        frame = [*block, "u1", "u2", "u3", "u4", "new 1", "new 2"]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(seed + footer), 100)
            merged = relay.merge_claude_history("w1:p1", "\n".join(frame + footer), 100)
        finally:
            relay.claude_history_state.clear()

        self.assertEqual(
            [relay.normalized_history_line(line) for line in merged.splitlines()],
            [*seed, "new 1", "new 2", *footer],
        )

    def test_claude_history_appends_when_match_is_deeper_than_one_screen(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        block = ["$ make check", "Ran 40 tests", "OK"]
        seed = ["start", *block, "m1", "m2", "m3", "m4", "m5", "m6", *block, "u1", "u2"]
        # No tail overlap (more than one viewport scrolled past between
        # captures) and the frame repeats an old block: the fuzzy match
        # anchors deeper in history than one screen, which must append,
        # never rebase.
        frame = [*block, "n1", "n2", "n3", "n4", "n5", "n6", "n7"]

        relay.claude_history_state.clear()
        try:
            relay.merge_claude_history("w1:p1", "\n".join(seed + footer), 100)
            merged = relay.merge_claude_history("w1:p1", "\n".join(frame + footer), 100)
        finally:
            relay.claude_history_state.clear()

        self.assertEqual(
            [relay.normalized_history_line(line) for line in merged.splitlines()],
            [*seed, *frame, *footer],
        )

    def test_claude_history_survives_relay_restart(self):
        footer = ["prompt", "separator", "model", "context", "mode", "status"]
        first = ["A", "B", "C", "D", "E", "F", "G", "H", *footer]
        after_restart = ["C", "D", "E", "F", "G", "H", "I", "J", *footer]

        with tempfile.TemporaryDirectory() as temp_dir:
            history_dir = Path(temp_dir)
            relay.claude_history_state.clear()
            relay.claude_history_save_times.clear()
            try:
                with patch.object(relay, "CLAUDE_HISTORY_DIR", history_dir):
                    relay.merge_claude_history("w1:p1", "\n".join(first), 30)
                    relay.save_claude_history_state("w1:p1", force=True)

                    # Simulate a relay restart: memory gone, files remain.
                    relay.claude_history_state.clear()
                    relay.claude_history_save_times.clear()

                    merged = relay.merge_claude_history("w1:p1", "\n".join(after_restart), 30)
                    history_file = relay.claude_history_file("w1:p1")
                    self.assertTrue(history_file.exists())
                    self.assertEqual(history_file.stat().st_mode & 0o777, 0o600)
            finally:
                relay.claude_history_state.clear()
                relay.claude_history_save_times.clear()

        self.assertEqual(
            [relay.normalized_history_line(line) for line in merged.splitlines()],
            ["A", "B", *after_restart],
        )

    def test_claude_history_prune_spares_live_panes_and_waits_for_inventory(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            history_dir = Path(temp_dir)
            stale_orphan = history_dir / "w1_p1.json"
            fresh_orphan = history_dir / "w2_p1.json"
            stale_live = history_dir / "w3_p1.json"
            for path in (stale_orphan, fresh_orphan, stale_live):
                path.write_text("{}")
            old = time.time() - (relay.CLAUDE_HISTORY_MAX_AGE_DAYS + 1) * 86400
            os.utime(stale_orphan, (old, old))
            os.utime(stale_live, (old, old))

            with (
                patch.object(relay, "CLAUDE_HISTORY_DIR", history_dir),
                patch.object(relay, "agent_types", {"w3:p1": "claude"}),
            ):
                with patch.object(relay, "agent_activity_initialized", False):
                    relay.prune_claude_history_files()
                self.assertTrue(stale_orphan.exists())

                with patch.object(relay, "agent_activity_initialized", True):
                    relay.prune_claude_history_files()

            self.assertFalse(stale_orphan.exists())
            self.assertTrue(fresh_orphan.exists())
            self.assertTrue(stale_live.exists())

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


class RelayCommandsTest(ClaudeHistoryIsolationMixin, unittest.IsolatedAsyncioTestCase):
    async def test_poll_gate_does_not_skip_status_or_history_bookkeeping(self):
        def agent_snapshot():
            return [{
                "pane_id": "w1:p1",
                "status": "working",
                "agent": "claude",
                "_focused": False,
                "_activity_fingerprint": ("working", 10),
            }]

        wakeup = asyncio.Event()
        with (
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay, "get_agents", side_effect=[agent_snapshot(), agent_snapshot()]),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "wait_for_next_poll",
                AsyncMock(side_effect=[None, asyncio.CancelledError()]),
            ),
            patch.object(relay, "broadcast_serialized", AsyncMock()) as broadcast,
            patch.object(relay, "schedule_claude_history_capture") as capture,
            patch.object(relay, "last_broadcast_agents_message", None),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "last_statuses", {}),
            patch.object(relay, "agent_types", {}),
            patch.object(relay, "agent_activity_state", {}),
            patch.object(relay, "agent_activity_initialized", False),
            patch.object(relay, "unseen_done_panes", set()),
            patch.object(relay, "acknowledged_done_panes", set()),
            patch.object(relay, "finished_notification_panes", set()),
            patch.object(relay, "claude_history_state", {}),
            patch.object(relay, "claude_history_capture_times", {}),
            patch.object(relay, "claude_history_pending_captures", set()),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await relay.poll_loop()

            self.assertEqual(relay.last_statuses, {"w1:p1": "working"})

        broadcast.assert_awaited_once()
        self.assertEqual(capture.call_count, 2)

    async def test_poll_wait_returns_immediately_when_woken(self):
        wakeup = asyncio.Event()
        with patch.object(relay, "poll_wakeup", wakeup):
            waiter = asyncio.create_task(relay.wait_for_next_poll([{"status": "idle"}]))
            await asyncio.sleep(0)
            relay.wake_poll_loop()
            await asyncio.wait_for(waiter, timeout=0.2)

    async def test_agent_broadcast_caches_latest_and_skips_identical_payloads(self):
        idle = [{"pane_id": "w1:p1", "status": "idle", "agent": "codex"}]
        working = [{"pane_id": "w1:p1", "status": "working", "agent": "codex"}]
        with (
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "last_broadcast_agents_message", None),
            patch.object(relay, "broadcast_serialized", AsyncMock()) as broadcast,
        ):
            first = await relay.broadcast_agents_if_changed(idle)
            duplicate = await relay.broadcast_agents_if_changed(list(reversed(idle)))
            changed = await relay.broadcast_agents_if_changed(working)
            latest = relay.latest_agents_message

        self.assertTrue(first)
        self.assertFalse(duplicate)
        self.assertTrue(changed)
        self.assertEqual(broadcast.await_count, 2)
        self.assertEqual(json.loads(latest)["agents"][0]["status"], "working")

    async def test_new_client_receives_cached_agents_immediately_after_config(self):
        ws = FakeWebSocket()
        cached = relay.agents_message([
            {"pane_id": "w1:p1", "status": "idle", "agent": "codex"},
        ])
        with (
            patch.object(relay, "latest_agents_message", cached),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", return_value=[]),
        ):
            await relay.handle_client(ws)

        self.assertEqual(
            [message["type"] for message in ws.messages],
            ["push_config", "agents", "activity_history"],
        )
        self.assertEqual(ws.messages[1]["agents"][0]["pane_id"], "w1:p1")
        self.assertNotIn(ws, relay.clients)

    async def test_only_successful_state_commands_wake_adaptive_polling(self):
        ws = FakeWebSocket()
        with patch.object(relay, "wake_poll_loop") as wake:
            await relay.send_command_result(ws, "request-1", "prompt", True)
            await relay.send_command_result(ws, "request-2", "prompt", False)
            await relay.send_command_result(ws, "request-3", "list_directories", True)

        wake.assert_called_once_with()

    async def test_udp_plugin_event_queues_payload_and_wakes_polling(self):
        queue = Mock()
        with (
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "wake_poll_loop") as wake,
        ):
            relay.UDPPlugin().datagram_received(b'{"pane_id":"w1:p1"}', None)

        queue.put_nowait.assert_called_once_with({"pane_id": "w1:p1"})
        wake.assert_called_once_with()

    async def test_udp_plugin_ignores_non_object_json(self):
        queue = Mock()
        with (
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "wake_poll_loop") as wake,
        ):
            relay.UDPPlugin().datagram_received(b'[]', None)

        queue.put_nowait.assert_not_called()
        wake.assert_not_called()

    async def test_viewing_done_pane_broadcasts_idle_immediately(self):
        pane = "w1:p1"
        relay.unseen_done_panes.add(pane)
        relay.acknowledged_done_panes.discard(pane)
        relay.agent_types[pane] = "codex"
        try:
            with (
                patch.object(relay, "broadcast", AsyncMock()) as broadcast,
                patch.object(relay, "wake_poll_loop") as wake,
            ):
                acknowledged = await relay.acknowledge_pane_viewed(pane)
                repeated = await relay.acknowledge_pane_viewed(pane)
                unknown = await relay.acknowledge_pane_viewed("missing:pane")
        finally:
            relay.unseen_done_panes.discard(pane)
            relay.acknowledged_done_panes.discard(pane)
            relay.agent_types.pop(pane, None)

        self.assertTrue(acknowledged)
        self.assertFalse(repeated)
        self.assertFalse(unknown)
        wake.assert_called_once_with()
        broadcast.assert_awaited_once_with({
            "type": "agent_update",
            "pane_id": pane,
            "raw_pane_id": pane,
            "status": "idle",
        })

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
        self.assertEqual(health.headers["X-Herdr-Relay-Instance"], relay.RELAY_INSTANCE_ID)
        self.assertEqual(health.body, b"ok\n")

        healthz = await relay.process_request(None, FakeRequest("/healthz"))
        self.assertEqual(healthz.status_code, 200)
        self.assertEqual(healthz.headers["Content-Type"], "application/json; charset=utf-8")
        payload = json.loads(healthz.body)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["instance"], relay.RELAY_INSTANCE_ID)
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
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
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
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
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
                patch.object(
                    relay.asyncio,
                    "to_thread",
                    AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
                ),
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

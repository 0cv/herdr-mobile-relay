import asyncio
import configparser
import copy
import importlib.util
import json
import os
import pty
import re
import runpy
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch
from contextlib import ExitStack, contextmanager


RELAY_PATH = Path(__file__).parents[1] / "relay" / "herdr_relay.py"
ROOT_PATH = RELAY_PATH.parents[1]
PLUGIN_MANIFEST = ROOT_PATH / "herdr-plugin.toml"
PLUGIN_VERSION_MATCH = re.search(r'^version = "([^"]+)"$', PLUGIN_MANIFEST.read_text(), re.MULTILINE)
if not PLUGIN_VERSION_MATCH:
    raise RuntimeError("herdr-plugin.toml does not declare a version")
PLUGIN_VERSION = PLUGIN_VERSION_MATCH.group(1)
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


MULTI_QUESTION_VIEW = """
Improvements  ✓ Submit →
Which further improvements should be included?
❯ 1. [✓] Remove duplicate embed
PJN_CarePlanTimeline.cmp embeds the updater twice.
2. [ ] Harden aura subscribe races
Store the subscribe promise synchronously.
3. [✓] Extend Case watch list
Add the program developer and record type fields.
4. [ ] Refresh old parent on reparent
Publish for the old care plan too.
5. [ ] Type something.
Submit
6. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
"""

CLAUDE_FIRST_QUESTION_VIEW = """
\x1b[48;2;55;55;55m Reconnect \x1b[0m ☐ Offline ☐ Feedback ✓ Submit →
What should drive reconnect attempts?
❯ 1. Backoff + jitter
Reduce synchronized retries.
2. Fixed retry
Keep timing predictable.
3. Event-driven
Retry only after connectivity changes.
4. Type something.
5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
"""

CODEX_QUESTION_VIEW = """
\x1b[48;2;240;240;240m  \x1b[2mQuestion 1/3 (3 unanswered)
\x1b[48;2;240;240;240m  \x1b[38;5;6mWhere should the reusable adapter boundary sit?
\x1b[48;2;240;240;240m
\x1b[48;2;240;240;240m  \x1b[1m› 1. Domain port (Recommended)  Define transport-agnostic contracts.
\x1b[48;2;240;240;240m    2. Protocol boundary           Keep domain logic relay-shaped.
\x1b[48;2;240;240;240m    3. Workflow adapter            Encapsulate the full workflow.
\x1b[48;2;240;240;240m    4. None of the above           Optionally, add details in notes (tab).
\x1b[48;2;240;240;240m
\x1b[48;2;240;240;240m  tab to add notes | enter to submit answer | ←/→ to navigate questions
"""


def write_service_env(home, relay_env):
    if os.uname().sysname == "Darwin":
        service_file = home / "Library" / "LaunchAgents" / "com.herdr-mobile-relay.service.plist"
        service_file.parent.mkdir(parents=True)
        service_file.write_text(
            "<key>HERDR_RELAY_ENV</key>\n"
            f"<string>{relay_env}</string>\n"
        )
        return
    service_file = home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
    service_file.parent.mkdir(parents=True)
    service_file.write_text(f"Environment=HERDR_RELAY_ENV={relay_env}\n")


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
        relay.blocked_agent_details.clear()
        relay.agent_profile_ids.clear()
        relay.agent_profile_seen_panes.clear()
        relay.latest_agents_message = relay.agents_message([])
        relay.last_broadcast_agents_message = None
        self.addCleanup(relay.claude_history_state.clear)
        self.addCleanup(relay.claude_history_save_times.clear)
        self.addCleanup(relay.blocked_agent_details.clear)
        self.addCleanup(relay.agent_profile_ids.clear)
        self.addCleanup(relay.agent_profile_seen_panes.clear)


class RelayHelpersTest(ClaudeHistoryIsolationMixin, unittest.TestCase):
    def test_protocol_v2_keeps_v1_as_the_unversioned_baseline(self):
        self.assertEqual(relay.PROTOCOL_VERSION, 2)
        self.assertIn("slash_commands", relay.RELAY_CAPABILITIES)
        self.assertEqual(relay.client_protocol_version({}), 1)
        self.assertFalse(relay.client_protocol_matches({}))
        self.assertTrue(relay.client_protocol_matches({"protocol": 2}))
        self.assertFalse(relay.client_protocol_matches({"protocol": True}))

    def test_app_deploy_status_never_exposes_cloudflare_credentials(self):
        config = {
            "configured": True,
            "origin": "https://app.example.test",
            "project": "herdr-app",
            "branch": "main",
            "reason": "",
            "npx": "/tmp/npx",
            "node_dir": "/tmp",
            "account_id": "account-secret",
            "api_token": "token-secret",
        }
        with (
            patch.object(relay, "APP_DEPLOY_CONFIG", config),
            patch.object(relay, "APP_DEPLOY_REVISION", "f" * 40),
        ):
            status = relay.public_app_deploy_status({"state": "idle"})

        self.assertNotIn("account_id", status)
        self.assertNotIn("api_token", status)
        self.assertNotIn("npx", status)
        self.assertNotIn("node_dir", status)

    def test_parses_claude_multi_select_with_descriptions_and_other(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)

        self.assertEqual(interaction["kind"], "multi_select")
        self.assertEqual(interaction["question"], "Which further improvements should be included?")
        self.assertEqual(len(interaction["options"]), 4)
        self.assertTrue(interaction["options"][0]["selected"])
        self.assertEqual(
            interaction["options"][1]["description"],
            "Store the subscribe promise synchronously.",
        )
        self.assertEqual(interaction["other"], {"selected": False, "text": ""})
        self.assertEqual(interaction["submit_label"], "Submit")
        self.assertTrue(interaction["can_chat"])
        self.assertEqual(interaction["_focus"], ("option", 0))

    def test_claude_first_question_uses_next_and_hides_redundant_chat(self):
        interaction = relay.parse_claude_question(CLAUDE_FIRST_QUESTION_VIEW)
        public = relay.public_question_interaction(interaction)

        self.assertEqual(interaction["kind"], "single_select")
        self.assertEqual(interaction["submit_label"], "Next")
        self.assertTrue(interaction["can_chat"])
        self.assertFalse(public["can_chat"])
        self.assertEqual(public["question_index"], 1)
        self.assertEqual(public["question_total"], 3)
        self.assertTrue(relay.question_has_next(CLAUDE_FIRST_QUESTION_VIEW))
        final = CLAUDE_FIRST_QUESTION_VIEW.replace(
            "\x1b[48;2;55;55;55m Reconnect \x1b[0m ☐ Offline ☐ Feedback",
            "✓ Reconnect ✓ Offline \x1b[48;2;55;55;55m Feedback \x1b[0m",
        )
        self.assertFalse(relay.question_has_next(final))
        self.assertEqual(relay.claude_question_position(final), (3, 3))
        self.assertEqual(relay.parse_claude_question(final)["submit_label"], "Submit")

    def test_claude_single_select_keeps_identity_and_separates_retained_other_text(self):
        answered = (
            CLAUDE_FIRST_QUESTION_VIEW
            .replace(
                "2. Fixed retry",
                "2. \x1b[38;2;78;186;101mFixed retry\x1b[0m "
                "\x1b[38;2;78;186;101m✔\x1b[0m",
            )
            .replace("4. Type something.", "4. Hello")
        )
        original = relay.parse_claude_question(CLAUDE_FIRST_QUESTION_VIEW)
        interaction = relay.parse_claude_question(answered)

        self.assertEqual(interaction["id"], original["id"])
        self.assertEqual(
            [(item["label"], item["selected"]) for item in interaction["options"]],
            [
                ("Backoff + jitter", False),
                ("Fixed retry", True),
                ("Event-driven", False),
            ],
        )
        self.assertEqual(interaction["other"], {"selected": False, "text": "Hello"})

        other_answered = answered.replace(
            "4. Hello",
            "4. \x1b[38;2;78;186;101mHello ✔\x1b[0m",
        ).replace(
            "2. \x1b[38;2;78;186;101mFixed retry\x1b[0m "
            "\x1b[38;2;78;186;101m✔\x1b[0m",
            "2. Fixed retry",
        )
        other = relay.parse_claude_question(other_answered)
        self.assertEqual(other["id"], original["id"])
        self.assertFalse(any(item["selected"] for item in other["options"]))
        self.assertEqual(other["other"], {"selected": True, "text": "Hello"})

    def test_historical_claude_question_does_not_hide_current_approval(self):
        approval = """
Plan complete. Claude is ready to proceed.
Do you want to proceed?
❯ 1. Yes, clear context and auto-accept edits
2. Yes, auto-accept edits
3. Yes, manually approve edits
4. Type here to tell Claude what to change
"""
        pane = CLAUDE_FIRST_QUESTION_VIEW + approval

        self.assertFalse(relay.question_layout_hint(pane))
        self.assertIsNone(relay.parse_question(pane, "claude"))
        self.assertEqual(
            relay.detect_options(pane),
            [
                "Yes, clear context and auto-accept edits",
                "Yes, auto-accept edits",
                "Yes, manually approve edits",
                "Type here to tell Claude what to change",
            ],
        )

    def test_parses_codex_plan_question_with_descriptions_and_navigation(self):
        interaction = relay.parse_codex_question(CODEX_QUESTION_VIEW)

        self.assertEqual(interaction["kind"], "single_select")
        self.assertEqual(
            interaction["question"],
            "Where should the reusable adapter boundary sit?",
        )
        self.assertEqual(
            [item["label"] for item in interaction["options"]],
            ["Domain port (Recommended)", "Protocol boundary", "Workflow adapter"],
        )
        self.assertEqual(
            interaction["options"][0]["description"],
            "Define transport-agnostic contracts.",
        )
        self.assertEqual(interaction["submit_label"], "Next")
        self.assertFalse(interaction["can_chat"])
        self.assertFalse(interaction["_can_go_back"])
        self.assertEqual(interaction["_focus"], ("option", 0))
        self.assertEqual(interaction["_agent"], "codex")
        self.assertEqual(interaction["other"]["label"], "None of the above")
        self.assertEqual(interaction["other"]["placeholder"], "Optional notes")
        self.assertTrue(interaction["other"]["allow_empty"])
        self.assertTrue(relay.question_layout_hint(CODEX_QUESTION_VIEW))
        self.assertEqual(
            relay.parse_question(CODEX_QUESTION_VIEW, "codex")["id"],
            interaction["id"],
        )

    def test_codex_question_exposes_previous_and_restores_custom_notes(self):
        answered = (
            CODEX_QUESTION_VIEW
            .replace("Question 1/3 (3 unanswered)", "Question 2/3 (2 unanswered)")
            .replace("\x1b[38;5;6mWhere", "Where")
            .replace("› 1. Domain", "  1. Domain")
            .replace("    4. None", "  › 4. None")
            .replace(
                "  tab to add notes",
                "  › Preserve only the public contract\n  tab or esc to clear notes",
            )
        )

        interaction = relay.parse_codex_question(answered)
        public = relay.public_question_interaction(interaction)

        self.assertTrue(interaction["_can_go_back"])
        self.assertTrue(public["can_go_back"])
        self.assertEqual(public["question_index"], 2)
        self.assertEqual(public["question_total"], 3)
        self.assertTrue(interaction["_notes_active"])
        self.assertTrue(interaction["other"]["selected"])
        self.assertEqual(
            interaction["other"]["text"], "Preserve only the public contract"
        )
        self.assertEqual(interaction["_focus"], ("option", 3))

    def test_chained_codex_questions_with_identical_content_get_distinct_ids(self):
        first = relay.parse_codex_question(CODEX_QUESTION_VIEW)
        second = relay.parse_codex_question(
            CODEX_QUESTION_VIEW.replace(
                "Question 1/3 (3 unanswered)", "Question 2/3 (2 unanswered)"
            )
        )

        self.assertIsNotNone(first)
        self.assertIsNotNone(second)
        self.assertNotEqual(first["id"], second["id"])
        self.assertEqual(
            relay.parse_codex_question(CODEX_QUESTION_VIEW)["id"], first["id"]
        )

    def test_parses_narrow_codex_option_table_without_losing_wrapped_labels(self):
        narrow = """
Question 1/3 (3 unanswered)
Where should the adapter boundary sit?

› 1. Domain port       Define
     (Recommended)     transport-agnostic
  2. Protocol          Keep domain
     boundary          logic relay-shaped.
  3. Workflow          Encapsulate
     adapter           the workflow.
  4. None of the       Optionally,
     above             add notes.

tab to add notes | enter to submit answer | ←/→ to navigate questions
"""

        interaction = relay.parse_codex_question(narrow)

        self.assertEqual(interaction["options"][0]["label"], "Domain port (Recommended)")
        self.assertEqual(
            interaction["options"][1]["description"], "Keep domain logic relay-shaped."
        )
        self.assertEqual(interaction["other"]["label"], "None of the above")

    def test_question_identity_survives_checkbox_and_header_redraws(self):
        initial = relay.parse_claude_question(
            MULTI_QUESTION_VIEW.replace(
                "Improvements  ✓ Submit →", "←  ☐ Scope  ☐ Devices  ✔ Submit  →"
            ).replace("1. [✓] Remove", "1. [ ] Remove")
        )
        selected = relay.parse_claude_question(
            MULTI_QUESTION_VIEW.replace(
                "Improvements  ✓ Submit →", "←  ☒ Scope  ☐ Devices  ✔ Submit  →"
            )
        )

        self.assertEqual(initial["question"], selected["question"])
        self.assertFalse(initial["options"][0]["selected"])
        self.assertTrue(selected["options"][0]["selected"])
        self.assertEqual(initial["id"], selected["id"])

    def test_question_header_exposes_previous_only_after_the_first_tab(self):
        first = relay.parse_claude_question(
            MULTI_QUESTION_VIEW.replace(
                "Improvements  ✓ Submit →",
                "←  \x1b[38;2;255;255;255m\x1b[48;2;87;105;247m ☐ Scope "
                "\x1b[0m ☐ Devices  ✔ Submit  →",
            )
        )
        later = relay.parse_claude_question(
            MULTI_QUESTION_VIEW.replace(
                "Improvements  ✓ Submit →",
                "←  ☒ Scope  \x1b[38;2;255;255;255m\x1b[48;2;87;105;247m"
                " ☐ Devices \x1b[0m ✔ Submit  →",
            )
        )

        self.assertFalse(first["_can_go_back"])
        self.assertFalse(relay.public_question_interaction(first)["can_go_back"])
        self.assertEqual(first["_question_index"], 1)
        self.assertEqual(first["_question_total"], 2)
        self.assertTrue(later["_can_go_back"])
        self.assertTrue(relay.public_question_interaction(later)["can_go_back"])
        self.assertEqual(later["_question_index"], 2)
        self.assertEqual(later["_question_total"], 2)
        self.assertFalse(relay.parse_claude_question(MULTI_QUESTION_VIEW)["_can_go_back"])

    def test_parses_claude_single_select_and_custom_other(self):
        interaction = relay.parse_claude_question("""
Deployment target →
Which environment should receive the build?
❯ 1. Development
Fast feedback for the team.
2. Staging
Production-like verification.
3. A dedicated scratch org ✔
4. Chat about this
""")

        self.assertEqual(interaction["kind"], "single_select")
        self.assertEqual([item["label"] for item in interaction["options"]], ["Development", "Staging"])
        self.assertEqual(interaction["other"], {"selected": True, "text": "A dedicated scratch org"})
        self.assertEqual(interaction["_all_option_count"], 3)

    def test_question_layout_hint_blocks_unsafe_fallback_when_parse_is_partial(self):
        partial = "Which improvements?\n1. [ ] First choice\nSubmit"
        self.assertTrue(relay.question_layout_hint(partial))
        self.assertIsNone(relay.parse_claude_question(partial))
        self.assertFalse(relay.question_layout_hint("Plan\n1. [ ] First task\n2. [ ] Second task"))

    def test_claude_question_uses_the_unstitched_live_viewport(self):
        live = "☒ Scope  ☐ Devices\nWhich phone environments should be included?"
        with patch.object(relay, "merge_claude_history") as merge:
            content = relay.claude_content_for_client(
                "w1:p1",
                live,
                500,
                "blocked",
                question_active=True,
            )

        self.assertEqual(content, live)
        merge.assert_not_called()

    def test_terminal_chrome_metadata_describes_claude_desktop_footer(self):
        self.assertEqual(relay.terminal_chrome_metadata("claude", "ansi"), {
            "desktop_footer_lines": 6,
            "desktop_prompt_lines": 2,
        })
        self.assertEqual(relay.terminal_chrome_metadata("codex", "ansi"), {})
        self.assertEqual(relay.terminal_chrome_metadata("claude", "text"), {})
        self.assertEqual(relay.terminal_chrome_metadata("claude", "ansi", question_active=True), {})

    def test_terminal_history_line_requests_are_validated_and_bounded(self):
        self.assertEqual(relay.requested_terminal_history_lines(100), 100)
        self.assertEqual(relay.requested_terminal_history_lines("10000"), 10000)
        self.assertEqual(relay.requested_terminal_history_lines(20000), 10000)
        self.assertEqual(relay.requested_terminal_history_lines(0), 1)
        self.assertEqual(relay.requested_terminal_history_lines("invalid"), 30)

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

    def test_phone_setup_fragment_carries_the_explicit_relay_url(self):
        root = RELAY_PATH.parents[1]
        result = subprocess.run(
            [
                "bash",
                "-c",
                '. "$1"; build_setup_fragment "$2" "$3" "$4"',
                "bash",
                str(root / "relay" / "common.sh"),
                "0123456789abcdef0123456789abcdef",
                "Fedora Workstation",
                "wss://relay-fedora.example.com",
            ],
            check=True,
            capture_output=True,
            text=True,
        )

        self.assertEqual(
            urllib.parse.parse_qs(result.stdout.strip()),
            {
                "setup": ["0123456789abcdef0123456789abcdef"],
                "label": ["Fedora Workstation"],
                "relay": ["wss://relay-fedora.example.com"],
            },
        )

    def test_phone_app_base_url_prefers_private_record_then_falls_back_to_relay(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test\n")
            origin_file = runtime / "phone-app-origin"
            origin_file.write_text("https://app.example.com\n")
            command = [
                "bash",
                "-c",
                '. "$1"; phone_app_base_url "$2" "$3"',
                "bash",
                str(root / "relay" / "common.sh"),
                "https://relay-fedora.example.com",
                str(env_file),
            ]

            recorded = subprocess.run(
                command,
                check=True,
                capture_output=True,
                text=True,
            )
            origin_file.unlink()
            fallback = subprocess.run(
                command,
                check=True,
                capture_output=True,
                text=True,
            )

        self.assertEqual(recorded.stdout.strip(), "https://app.example.com")
        self.assertEqual(fallback.stdout.strip(), "https://relay-fedora.example.com")

    def test_phone_app_origin_shell_record_is_private_and_replaceable(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test\n")
            command = [
                "bash",
                "-c",
                '. "$1"; record_phone_app_origin "$2" "$3"',
                "bash",
                str(root / "relay" / "common.sh"),
                "https://app.example.com",
                str(env_file),
            ]

            subprocess.run(command, check=True, capture_output=True, text=True)
            command[-2] = "https://replacement.example.com"
            subprocess.run(command, check=True, capture_output=True, text=True)
            target = runtime / "phone-app-origin"

            self.assertEqual(target.read_text(), "https://replacement.example.com\n")
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_guided_setup_link_prompts_for_and_records_existing_installed_app(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test-token\n")
            bin_dir = runtime / "bin"
            bin_dir.mkdir()
            fake_uv = bin_dir / "uv"
            fake_uv.write_text(
                "#!/bin/bash\n"
                "case \"$*\" in\n"
                "  *urllib.parse.urlencode*) echo 'setup=fake-token&label=workstation&relay=wss%3A%2F%2Frelay.example.test' ;;\n"
                "  *urllib.parse.urlsplit*) value=\"${!#}\"; case \"$value\" in *://*) ;; *) value=\"https://$value\" ;; esac; echo \"${value%/}\" ;;\n"
                "  *) exit 1 ;;\n"
                "esac\n"
            )
            fake_uv.chmod(0o700)
            local_bin = runtime / ".local" / "bin"
            local_bin.mkdir(parents=True)
            fake_curl = local_bin / "curl"
            fake_curl.write_text('#!/bin/bash\nprintf \'{"name":"Herdr Mobile Relay"}\\n\'\n')
            fake_curl.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "HOME": str(runtime),
                "HERDR_RELAY_ENV": str(env_file),
                "PATH": f"{bin_dir}:{env['PATH']}",
                "TERM": "dumb",
            })
            env.pop("HERDR_PHONE_APP_URL", None)
            master, slave = pty.openpty()
            try:
                process = subprocess.Popen(
                    [
                        "bash",
                        str(root / "relay" / "setup-link.sh"),
                        "relay.example.test",
                    ],
                    stdin=slave,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    env=env,
                )
                os.close(slave)
                slave = -1
                os.write(master, b"2\napp.example.com\n")
                stdout, stderr = process.communicate(timeout=10)
            finally:
                os.close(master)
                if slave >= 0:
                    os.close(slave)

            target = runtime / "phone-app-origin"
            self.assertEqual(process.returncode, 0, stderr)
            self.assertIn("Where should the phone setup link open?", stderr)
            self.assertIn("This relay (recommended for one relay)", stderr)
            self.assertIn("An existing installed Herdr app", stderr)
            self.assertIn("Installed app domain or URL", stderr)
            self.assertEqual(target.read_text(), "https://app.example.com\n")
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_guided_phone_app_origin_reprompts_and_adds_https(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test-token\n")
            bin_dir = runtime / "bin"
            bin_dir.mkdir()
            fake_curl = bin_dir / "curl"
            fake_curl.write_text(
                "#!/bin/bash\n"
                'case "${!#}" in\n'
                "  *missing.example.com/*) exit 22 ;;\n"
                '  *) printf \'{"name":"Herdr Mobile Relay"}\\n\' ;;\n'
                "esac\n"
            )
            fake_curl.chmod(0o700)
            env = os.environ.copy()
            env["PATH"] = f"{bin_dir}:{env['PATH']}"
            env.pop("HERDR_PHONE_APP_URL", None)
            master, slave = pty.openpty()
            try:
                process = subprocess.Popen(
                    [
                        "bash",
                        "-c",
                        '. "$1"; choose_phone_app_base_url "$2" "$3" stable',
                        "bash",
                        str(root / "relay" / "common.sh"),
                        "https://relay.example.test",
                        str(env_file),
                    ],
                    stdin=slave,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    env=env,
                )
                os.close(slave)
                slave = -1
                os.write(
                    master,
                    b"2\napp.example.com/path\nmissing.example.com\n\napp.example.com\n",
                )
                stdout, stderr = process.communicate(timeout=10)
            finally:
                os.close(master)
                if slave >= 0:
                    os.close(slave)

            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(stdout.strip(), "https://app.example.com")
            self.assertIn("Enter a domain or HTTPS URL without a path", stderr)
            self.assertIn("No Herdr app was found at https://missing.example.com", stderr)
            self.assertEqual(stderr.count("Installed app domain or URL"), 3)

    def test_guided_phone_app_origin_shows_and_reuses_the_saved_choice(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test-token\n")
            (runtime / "phone-app-origin").write_text("https://app.example.com\n")
            env = os.environ.copy()
            env.pop("HERDR_PHONE_APP_URL", None)
            master, slave = pty.openpty()
            try:
                process = subprocess.Popen(
                    [
                        "bash",
                        "-c",
                        '. "$1"; choose_phone_app_base_url "$2" "$3" stable',
                        "bash",
                        str(root / "relay" / "common.sh"),
                        "https://relay.example.test",
                        str(env_file),
                    ],
                    stdin=slave,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    env=env,
                )
                os.close(slave)
                slave = -1
                os.write(master, b"\n")
                stdout, stderr = process.communicate(timeout=10)
            finally:
                os.close(master)
                if slave >= 0:
                    os.close(slave)

            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(stdout.strip(), "https://app.example.com")
            self.assertIn("Current phone app: https://app.example.com", stderr)
            self.assertIn("Choice [keep current]:", stderr)

    def test_guided_temporary_setup_defaults_to_its_own_relay_app(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime = Path(temp_dir)
            env_file = runtime / "relay.env"
            env_file.write_text("HERDR_RELAY_TOKEN=test-token\n")
            bin_dir = runtime / "bin"
            bin_dir.mkdir()
            fake_uv = bin_dir / "uv"
            fake_uv.write_text(
                "#!/bin/bash\n"
                "case \"$*\" in\n"
                "  *urllib.parse.urlsplit*) value=\"${!#}\"; echo \"${value%/}\" ;;\n"
                "  *) exit 1 ;;\n"
                "esac\n"
            )
            fake_uv.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "HOME": str(runtime),
                "PATH": f"{bin_dir}:{env['PATH']}",
                "TERM": "dumb",
            })
            env.pop("HERDR_PHONE_APP_URL", None)
            master, slave = pty.openpty()
            try:
                process = subprocess.Popen(
                    [
                        "bash",
                        "-c",
                        '. "$1"; choose_phone_app_base_url "$2" "$3" temporary',
                        "bash",
                        str(root / "relay" / "common.sh"),
                        "https://temporary.trycloudflare.com",
                        str(env_file),
                    ],
                    stdin=slave,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    env=env,
                )
                os.close(slave)
                slave = -1
                os.write(master, b"\n")
                stdout, stderr = process.communicate(timeout=10)
            finally:
                os.close(master)
                if slave >= 0:
                    os.close(slave)

            self.assertEqual(process.returncode, 0, stderr)
            self.assertIn("This temporary relay", stderr)
            self.assertIn("An existing installed Herdr app", stderr)
            self.assertEqual(stdout.strip(), "https://temporary.trycloudflare.com")
            self.assertFalse((runtime / "phone-app-origin").exists())

    def test_phone_app_origin_is_validated_and_stored_privately(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            target = Path(temp_dir) / "runtime" / "phone-app-origin"
            with patch.object(relay, "PHONE_APP_ORIGIN_FILE", target):
                for invalid in (
                    "http://app.example.com",
                    "javascript:alert(1)",
                    "https://user@app.example.com",
                    "https://app.example.com/path",
                    "https://app.example.com?query=1",
                    "https://app.example.com:invalid",
                ):
                    self.assertFalse(relay.store_phone_app_origin(invalid), invalid)

                self.assertTrue(relay.store_phone_app_origin("https://app.example.com/"))

            self.assertEqual(target.read_text(), "https://app.example.com\n")
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

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

    def test_web_asset_path_allows_only_explicit_release_assets(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            web_dir = Path(temp_dir) / "web"
            (web_dir / "assets").mkdir(parents=True)
            (web_dir / "icons").mkdir()
            for relative in (
                "index.html",
                "manifest.webmanifest",
                "notification-icons.js",
                "sw.js",
                "version.json",
                "assets/app.js",
                "assets/app.css",
                "icons/icon.svg",
                "icons/icon-192.png",
            ):
                (web_dir / relative).write_text(relative)

            with patch.object(relay, "WEB_DIR", web_dir):
                accepted = {
                    path: relay.web_asset_path(path)
                    for path in (
                        "",
                        "/",
                        "/index.html",
                        "/manifest.webmanifest",
                        "/notification-icons.js",
                        "/sw.js",
                        "/version.json",
                        "/assets/app.js",
                        "/assets/app.css",
                        "/icons/icon.svg",
                        "/icons/icon-192.png",
                        "/assets/app.js?v=8",
                        "/icons/icon.svg?purpose=maskable",
                    )
                }

                rejected = [
                    "/missing",
                    "/_headers",
                    "/assets",
                    "/assets/",
                    "/assets/other.js",
                    "/assets/app.js.map",
                    "/assets/app.js.br",
                    "/assets/app.css.br",
                    "/index.html.br",
                    "/icons",
                    "/icons/",
                    "/icons/missing.svg",
                    "/icons/../index.html",
                    "/icons/%2e%2e/index.html",
                    "/icons/%2E%2E%2Findex.html",
                    "/icons\\..\\index.html",
                    "/icons/%5c..%5cindex.html",
                    "//etc/passwd",
                    "/C:/Windows/system.ini",
                ]
                rejected_results = {path: relay.web_asset_path(path) for path in rejected}

            self.assertTrue(all(accepted.values()), accepted)
            self.assertTrue(all(value is None for value in rejected_results.values()), rejected_results)

    def test_web_asset_path_rejects_symlinks_that_escape_release_root(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            web_dir = root / "web"
            icons_dir = web_dir / "icons"
            icons_dir.mkdir(parents=True)
            outside = root / "outside.svg"
            outside.write_text("outside")
            (icons_dir / "escape.svg").symlink_to(outside)
            source = icons_dir / "icon.svg"
            source.write_text("icon")
            compressed_outside = root / "outside.svg.br"
            compressed_outside.write_text("compressed outside")
            (icons_dir / "icon.svg.br").symlink_to(compressed_outside)

            with patch.object(relay, "WEB_DIR", web_dir):
                self.assertIsNone(relay.web_asset_path("/icons/escape.svg"))
                self.assertIsNone(relay.precompressed_asset_path(source, "br"))

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

    def test_blocked_details_are_cached_in_reconnect_snapshot_and_pruned(self):
        blocked = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        interaction = relay.public_question_interaction(
            relay.parse_claude_question(CLAUDE_FIRST_QUESTION_VIEW)
        )
        with (
            patch.object(relay, "blocked_agent_details", {}),
            patch.object(relay, "latest_agents_message", relay.agents_message([blocked])),
        ):
            relay.cache_blocked_agent_details({
                "pane_id": "w1:p1",
                "prompt": interaction["question"],
                "command": interaction["question"],
                "options": [],
                "interaction": interaction,
                "question_layout": True,
            })
            reconnect = json.loads(relay.latest_agents_message)["agents"][0]

            self.assertEqual(reconnect["interaction"]["id"], interaction["id"])
            self.assertTrue(reconnect["question_layout"])
            self.assertEqual(reconnect["options"], [])

            relay.prune_blocked_agent_details([
                {**blocked, "status": "working"},
            ])
            resumed = json.loads(relay.agents_message([
                {**blocked, "status": "working"},
            ]))["agents"][0]

        self.assertNotIn("interaction", resumed)
        self.assertNotIn("question_layout", resumed)

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

    def test_structured_question_push_opens_form_without_approval_action(self):
        interaction = relay.public_question_interaction(
            relay.parse_claude_question(MULTI_QUESTION_VIEW)
        )
        payload = relay.push_payload({
            "event_id": "event-2",
            "host": "fedora",
            "pane_id": "w1:p3",
            "project": "relay",
            "interaction": interaction,
            "question_layout": True,
        })

        self.assertEqual(payload["actions"], [])
        self.assertEqual(payload["action_urls"], {})
        self.assertIn("Which further improvements", payload["body"])

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
        description_match = re.search(r'^description = "([^"]+)"$', manifest, re.MULTILINE)

        self.assertFalse((root / "relay" / "herdr-plugin.toml").exists())
        self.assertIn('id = "herdr-mobile-relay.events"', manifest)
        self.assertIsNotNone(description_match)
        description = description_match.group(1).lower()
        self.assertIn("remote", description)
        self.assertIn("smartphone", description)
        self.assertRegex(PLUGIN_VERSION, r"^\d+\.\d+\.\d+$")
        self.assertIn('min_herdr_version = "0.7.5"', manifest)
        self.assertIn(f'**Current version:** `{PLUGIN_VERSION}`', (root / "README.md").read_text())
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
        self.assertIn('id = "setup-link"', manifest)
        self.assertIn('command = ["bash", "relay/open-plugin-pane.sh", "setup-link"]', manifest)
        self.assertIn('command = ["bash", "relay/plugin-setup-link.sh"]', manifest)
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

    def test_dev_tunnel_is_isolated_from_the_default_relay(self):
        root = Path(__file__).parents[1]
        script = (root / "relay" / "dev-tunnel.sh").read_text()
        makefile = (root / "Makefile").read_text()

        self.assertIn('HERDR_DEV_CONFIG_DIR:-$SCRIPT_DIR/.dev', script)
        self.assertIn('HERDR_DEV_RELAY_PORT:-18375', script)
        self.assertIn('HERDR_DEV_PLUGIN_PORT:-18376', script)
        self.assertIn('HERDR_RELAY_HOST="127.0.0.1"', script)
        self.assertIn('HERDR_WEB_ROOT="$REPO_DIR/frontend/dist"', script)
        self.assertIn('npm --prefix "$REPO_DIR/frontend" run build', script)
        self.assertIn("dev-tunnel:\n\trelay/dev-tunnel.sh", makefile)

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

    def test_plugin_event_identifies_its_herdr_socket(self):
        root = RELAY_PATH.parents[1]
        sent = []

        class FakeDatagramSocket:
            def sendto(self, payload, address):
                sent.append((payload, address))

            def close(self):
                pass

        fake_socket = SimpleNamespace(
            AF_INET=socket.AF_INET,
            SOCK_DGRAM=socket.SOCK_DGRAM,
            gethostname=lambda: "relay-host",
            socket=lambda *_args: FakeDatagramSocket(),
        )
        with (
            patch.dict(os.environ, {
                "HERDR_RELAY_PLUGIN_PORT": "18376",
                "HERDR_SOCKET_PATH": "/tmp/herdr-named.sock",
                "HERDR_PLUGIN_EVENT_JSON": json.dumps({"data": {"pane_id": "w1:p1"}}),
            }, clear=False),
            patch.dict(sys.modules, {"socket": fake_socket}),
        ):
            runpy.run_path(str(root / "relay" / "on_event.py"))

        payload = json.loads(sent[0][0].decode())

        self.assertEqual(payload["socket_path"], "/tmp/herdr-named.sock")
        self.assertEqual(sent[0][1], ("127.0.0.1", 18376))

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
        self.assertIn(PLUGIN_VERSION, nohup_args)

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
                "version": PLUGIN_VERSION,
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
                    ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), PLUGIN_VERSION, "0"],
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
            "--env", f"PATH={env['PATH']}",
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
                "version": PLUGIN_VERSION,
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
                    ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), PLUGIN_VERSION, "0"],
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
            "--env", f"PATH={env['PATH']}",
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
                ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), PLUGIN_VERSION, "0"],
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
                ["/bin/sh", str(root / "relay" / "plugin-post-install.sh"), PLUGIN_VERSION, "0"],
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
                ("plugin-setup-link.sh", "link"),
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
                ["bash", str(menu)], input="4\n", capture_output=True, text=True, check=True,
            )
            setup_link = subprocess.run(
                ["bash", str(menu)], input="3\n", capture_output=True, text=True, check=True,
            )

        self.assertTrue(quick.stdout.rstrip().endswith("quick"))
        self.assertTrue(stable.stdout.rstrip().endswith("stable"))
        self.assertTrue(setup_link.stdout.rstrip().endswith("link"))
        self.assertTrue(teardown.stdout.rstrip().endswith("teardown"))

    def test_plugin_setup_link_follows_the_installed_service_configuration(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            home = temp / "home"
            home.mkdir()
            relay_env = temp / "checkout" / "relay" / ".env"
            relay_env.parent.mkdir(parents=True)
            relay_env.write_text("HERDR_RELAY_TOKEN=test-token\n")
            write_service_env(home, relay_env)

            for name in ("plugin-setup-link.sh", "common.sh"):
                source = root / "relay" / name
                target = temp / name
                target.write_text(source.read_text())
                target.chmod(0o700)
            setup_link = temp / "setup-link.sh"
            setup_link.write_text('#!/bin/sh\nprintf "resolved=%s\\n" "$HERDR_RELAY_ENV"\n')
            setup_link.chmod(0o700)

            result = subprocess.run(
                ["bash", str(temp / "plugin-setup-link.sh")],
                check=True,
                capture_output=True,
                stdin=subprocess.DEVNULL,
                text=True,
                env={**os.environ, "HOME": str(home), "HERDR_PLUGIN_CONFIG_DIR": str(temp / "plugin-config")},
            )

        self.assertIn(f"resolved={relay_env}", result.stdout)

    def test_plugin_install_service_follows_a_checkout_service_configuration_on_macos(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            home = temp / "home"
            relay_env = temp / "checkout" / "relay" / ".env"
            relay_env.parent.mkdir(parents=True)
            relay_env.write_text("HERDR_RELAY_TOKEN=test-token\n")
            service_file = (
                home
                / "Library"
                / "LaunchAgents"
                / "com.herdr-mobile-relay.service.plist"
            )
            service_file.parent.mkdir(parents=True)
            service_file.write_text(
                "<key>HERDR_RELAY_ENV</key>\n"
                f"<string>{relay_env}</string>\n"
            )

            for name in ("plugin-install-service.sh", "common.sh"):
                source = root / "relay" / name
                target = temp / name
                target.write_text(source.read_text())
                target.chmod(0o700)
            stable_setup = temp / "stable-setup.sh"
            stable_setup.write_text(
                '#!/bin/sh\nprintf "resolved=%s\\n" "$HERDR_RELAY_ENV"\n'
            )
            stable_setup.chmod(0o700)
            bin_dir = temp / "bin"
            bin_dir.mkdir()
            fake_uname = bin_dir / "uname"
            fake_uname.write_text("#!/bin/sh\necho Darwin\n")
            fake_uname.chmod(0o700)
            env = os.environ.copy()
            env.pop("HERDR_RELAY_ENV", None)
            env.update({
                "HOME": str(home),
                "HERDR_PLUGIN_CONFIG_DIR": str(temp / "plugin-config"),
                "PATH": f"{bin_dir}:{env['PATH']}",
            })

            result = subprocess.run(
                ["bash", str(temp / "plugin-install-service.sh")],
                check=True,
                capture_output=True,
                stdin=subprocess.DEVNULL,
                text=True,
                env=env,
            )

        self.assertIn("Reusing the installed relay configuration", result.stdout)
        self.assertIn(f"resolved={relay_env}", result.stdout)

    def test_app_deploy_setup_preselects_the_only_project_and_retries_typos(self):
        root = RELAY_PATH.parents[1]
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            home = temp / "home"
            home.mkdir()
            relay_dir = temp / "relay"
            relay_dir.mkdir()
            for name in ("configure-app-deploy.sh", "common.sh"):
                source = root / "relay" / name
                target = relay_dir / name
                target.write_text(source.read_text())
                target.chmod(0o700)
            service = relay_dir / "service.sh"
            service.write_text("#!/bin/sh\necho service-installed\n")
            service.chmod(0o700)
            (temp / "herdr-plugin.toml").write_text('version = "0.8.1"\n')
            relay_env = relay_dir / ".env"
            relay_env.write_text(
                "HERDR_RELAY_TOKEN='test-token'\n"
                "HERDR_RELAY_INSTANCE_ID='test-instance'\n"
            )
            bin_dir = temp / "bin"
            bin_dir.mkdir()
            node = bin_dir / "node"
            node.write_text("#!/bin/sh\necho v24.18.0\n")
            node.chmod(0o700)
            npx = bin_dir / "npx"
            npx.write_text(
                "#!/bin/sh\n"
                "printf '%s\\n' "
                "'[{\"Project Name\":\"herdr-0cv\","
                "\"Project Domains\":\"herdr-0cv.pages.dev, herdr.example.test\"}]'\n"
            )
            npx.chmod(0o700)
            env = os.environ.copy()
            env.update({
                "HOME": str(home),
                "HERDR_RELAY_ENV": str(relay_env),
                "PATH": f"{bin_dir}:{env['PATH']}",
            })

            result = subprocess.run(
                ["bash", str(relay_dir / "configure-app-deploy.sh")],
                input="herdr.example.test\nmistyped\n\nn\n",
                capture_output=True,
                text=True,
                env=env,
            )
            configured = relay_env.read_text()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Project not available", result.stdout)
        self.assertIn("service-installed", result.stdout)
        self.assertIn(
            "HERDR_CLOUDFLARE_PAGES_PROJECT='herdr-0cv'",
            configured,
        )
        self.assertIn(
            "HERDR_APP_DEPLOY_ORIGIN='https://herdr.example.test'",
            configured,
        )

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
                "--env", f"PATH={env['PATH']}",
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
                "--env", f"PATH={env['PATH']}",
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
                "--env", f"PATH={env['PATH']}",
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
                stdin=subprocess.DEVNULL,
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
            home = Path(temp_dir).resolve()
            checkout_env = home / "checkout" / "relay" / ".env"
            plugin_env = home / "plugin-config" / "relay.env"
            checkout_env.parent.mkdir(parents=True)
            plugin_env.parent.mkdir(parents=True)
            checkout_env.touch()
            plugin_env.touch()
            write_service_env(home, plugin_env)
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
            home = Path(temp_dir).resolve()
            relay_env = home / "plugin-config" / "relay.env"
            relay_env.parent.mkdir(parents=True)
            relay_env.touch()
            write_service_env(home, relay_env)
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

    def test_clear_activity_history_removes_records_and_tombstones(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            tombstone_file = activity_file.with_suffix(".tombstones")
            activity_file.write_text('{"id":"activity-a"}\n')
            tombstone_file.write_text("stale-a\n")
            relay.activity_tombstones.add("stale-a")
            with patch.object(relay, "ACTIVITY_FILE", activity_file):
                ok, error = relay.clear_activity_history()

            self.assertTrue(ok)
            self.assertEqual(error, "")
            self.assertFalse(activity_file.exists())
            self.assertFalse(tombstone_file.exists())
            self.assertEqual(relay.activity_tombstones, set())

    def test_failed_tombstone_rewrite_preserves_other_stale_activity(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            tombstone_file = activity_file.with_suffix(".tombstones")
            stale_a = {"id": "stale-a", "kind": "blocked"}
            stale_b = {"id": "stale-b", "kind": "blocked"}
            activity_file.write_text(
                json.dumps(stale_a, separators=(",", ":")) + "\n"
                + json.dumps(stale_b, separators=(",", ":")) + "\n",
                encoding="utf-8",
            )
            tombstone_file.write_text("stale-a\nstale-b\n", encoding="utf-8")
            tombstones = {"stale-a", "stale-b"}

            with (
                patch.object(relay, "ACTIVITY_FILE", activity_file),
                patch.object(relay, "activity_tombstones", tombstones),
            ):
                self.assertTrue(relay.discard_recorded_activity("stale-a"))
                with patch.object(relay.os, "replace", side_effect=OSError("injected")):
                    self.assertFalse(relay.clear_activity_tombstone("stale-a"))

                self.assertEqual(
                    set(tombstone_file.read_text(encoding="utf-8").splitlines()),
                    {"stale-a", "stale-b"},
                )
                tombstones.clear()
                self.assertEqual(relay.load_activity(), [])
                self.assertIn("stale-b", relay.persisted_activity_tombstones())

    def test_record_activity_stores_multiline_extract(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            with patch.object(relay, "ACTIVITY_FILE", activity_file):
                entry = relay.record_activity(
                    "blocked",
                    "attention",
                    "Run tests?",
                    extract="line one   \n\n\n\nline two",
                )
            self.assertEqual(entry["extract"], "line one\n\nline two")

    def test_record_activity_omits_empty_extract(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            with patch.object(relay, "ACTIVITY_FILE", activity_file):
                entry = relay.record_activity("agent_status", "working", "Working", extract="   \n  ")
            self.assertNotIn("extract", entry)

    def test_compact_extract_caps_length(self):
        self.assertEqual(relay.ACTIVITY_EXTRACT_MAX_CHARS, relay.PROMPT_MAX_CHARS)
        capped = relay.compact_extract("x" * (relay.ACTIVITY_EXTRACT_MAX_CHARS + 50))
        self.assertEqual(len(capped), relay.ACTIVITY_EXTRACT_MAX_CHARS)

    def test_qoder_session_name_prefers_custom_title(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            project_dir = Path(temp_dir) / "-home-u-app"
            project_dir.mkdir()
            session_file = project_dir / "aaaa.jsonl"
            session_file.write_text(
                '{"type":"ai-title","aiTitle":"Auto name","sessionId":"s1"}\n'
                '{"type":"user","text":"hello"}\n'
                '{"type":"custom-title","customTitle":"My rename","sessionId":"s1"}\n'
            )
            with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.qoder_session_name("/home/u/app"), "My rename")
            session_file.write_text('{"type":"ai-title","aiTitle":"Auto name","sessionId":"s1"}\n')
            with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.qoder_session_name("/home/u/app"), "Auto name")
                self.assertEqual(relay.qoder_session_name("/home/u/other"), "")

    def test_claude_session_name_reads_title_records(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            project_dir = Path(temp_dir) / "-home-u-app"
            project_dir.mkdir()
            session_file = project_dir / "bbbb.jsonl"
            session_file.write_text('{"type":"ai-title","aiTitle":"Claude auto","sessionId":"s1"}\n')
            with patch.object(relay, "CLAUDE_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.claude_session_name("/home/u/app"), "Claude auto")
                self.assertEqual(relay.claude_session_name("/home/u/other"), "")
            session_file.write_text('{"type":"summary","summary":"Fix the relay tests","leafUuid":"x"}\n')
            with patch.object(relay, "CLAUDE_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.claude_session_name("/home/u/app"), "Fix the relay tests")

    def test_project_session_name_does_not_substitute_another_session(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            project_dir = Path(temp_dir) / "-home-u-app"
            project_dir.mkdir()
            (project_dir / "session-a.jsonl").write_text(
                '{"type":"custom-title","customTitle":"Session A"}\n'
            )
            with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.qoder_session_name("/home/u/app", "missing-session"), "")
            with patch.object(relay, "CLAUDE_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.claude_session_name("/home/u/app", "missing-session"), "")

            (project_dir / "session-b.jsonl").write_text(
                '{"type":"custom-title","customTitle":"Session B"}\n'
            )
            with patch.object(relay, "CLAUDE_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.claude_session_name("/home/u/app"), "")

    def test_project_dir_candidates_handle_dots_and_underscores(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            (Path(temp_dir) / "-home-u-my-app-v2").mkdir()
            with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.qoder_session_name("/home/u/my.app_v2"), "")
            (Path(temp_dir) / "-home-u-my-app-v2" / "cccc.jsonl").write_text(
                '{"type":"ai-title","aiTitle":"Dotted","sessionId":"s1"}\n'
            )
            with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                self.assertEqual(relay.qoder_session_name("/home/u/my.app_v2"), "Dotted")

    def test_codex_session_name_matches_by_cwd(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            sessions_dir = Path(temp_dir) / "sessions"
            day_dir = sessions_dir / "2026" / "07" / "21"
            day_dir.mkdir(parents=True)
            (day_dir / "rollout-2026-07-21T10-00-00-roll-1.jsonl").write_text(
                '{"type":"session_meta","payload":{"session_id":"sess-1","id":"roll-1","cwd":"/home/u/app"}}\n'
            )
            (day_dir / "rollout-2026-07-21T09-00-00-roll-0.jsonl").write_text(
                '{"type":"session_meta","payload":{"session_id":"sess-0","id":"roll-0","cwd":"/home/u/other"}}\n'
            )
            index_file = Path(temp_dir) / "session_index.jsonl"
            index_file.write_text(
                '{"id":"sess-0","thread_name":"other-thread"}\n'
                '{"id":"sess-1","thread_name":"my-thread"}\n'
            )
            with patch.object(relay, "CODEX_SESSIONS_DIR", sessions_dir), \
                    patch.object(relay, "CODEX_SESSION_INDEX_FILE", index_file):
                self.assertEqual(relay.codex_session_name("/home/u/app"), "my-thread")
                self.assertEqual(relay.codex_session_name("/home/u/other"), "other-thread")
                self.assertEqual(relay.codex_session_name("/home/u/missing"), "")

    def test_codex_session_name_uses_exact_id_for_shared_cwd(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            sessions_dir = Path(temp_dir) / "sessions"
            day_dir = sessions_dir / "2026" / "07" / "21"
            day_dir.mkdir(parents=True)
            (day_dir / "rollout-a.jsonl").write_text(
                '{"type":"session_meta","payload":{"session_id":"sess-a","cwd":"/home/u/app"}}\n'
            )
            (day_dir / "rollout-b.jsonl").write_text(
                '{"type":"session_meta","payload":{"session_id":"sess-b","cwd":"/home/u/app"}}\n'
            )
            index_file = Path(temp_dir) / "session_index.jsonl"
            index_file.write_text(
                '{"id":"sess-a","thread_name":"implementer"}\n'
                '{"id":"sess-b","thread_name":"reviewer"}\n'
            )
            with patch.object(relay, "CODEX_SESSIONS_DIR", sessions_dir), \
                    patch.object(relay, "CODEX_SESSION_INDEX_FILE", index_file):
                self.assertEqual(relay.codex_session_name("/home/u/app", "sess-a"), "implementer")
                self.assertEqual(relay.codex_session_name("/home/u/app", "sess-b"), "reviewer")
                self.assertEqual(relay.codex_session_name("/home/u/app", "missing"), "")
                self.assertEqual(relay.codex_session_name("/home/u/app"), "")
                self.assertEqual(
                    relay.session_name_for_activity("Codex", "/home/u/app", "sess-b"),
                    "reviewer",
                )

    def test_record_activity_stores_session_name(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp_path = Path(temp_dir)
            activity_file = temp_path / "activity.jsonl"
            project_dir = temp_path / "qoder" / "-home-u-app"
            project_dir.mkdir(parents=True)
            (project_dir / "dddd.jsonl").write_text('{"type":"custom-title","customTitle":"My rename","sessionId":"s1"}\n')
            snapshot = relay.agents_message([{"pane_id": "p1", "cwd": "/home/u/app"}])
            with patch.object(relay, "ACTIVITY_FILE", activity_file), \
                    patch.object(relay, "QODER_PROJECTS_DIR", temp_path / "qoder"), \
                    patch.object(relay, "latest_agents_message", snapshot):
                entry = relay.record_activity("blocked", "attention", "Run tests?", pane_id="p1", agent="Qoder")
                unnamed = relay.record_activity("blocked", "attention", "Run tests?", pane_id="p2", agent="Qoder")
            self.assertEqual(entry["session"], "My rename")
            self.assertNotIn("session", unnamed)

    def test_cached_session_name_refreshes_when_session_file_changes(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            project_dir = Path(temp_dir) / "-home-u-app"
            project_dir.mkdir()
            session_file = project_dir / "eeee.jsonl"
            session_file.write_text('{"type":"custom-title","customTitle":"First","sessionId":"s1"}\n')
            relay.session_name_cache.clear()
            try:
                with patch.object(relay, "QODER_PROJECTS_DIR", Path(temp_dir)):
                    self.assertEqual(relay.cached_session_name("Qoder", "/home/u/app"), "First")
                    session_file.write_text(
                        '{"type":"custom-title","customTitle":"Second title","sessionId":"s1"}\n'
                    )
                    self.assertEqual(relay.cached_session_name("Qoder", "/home/u/app"), "Second title")
            finally:
                relay.session_name_cache.clear()

    def test_cached_codex_session_name_refreshes_when_index_title_changes(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            sessions_dir = Path(temp_dir) / "sessions"
            day_dir = sessions_dir / "2026" / "07" / "21"
            day_dir.mkdir(parents=True)
            (day_dir / "rollout-a.jsonl").write_text(
                '{"type":"session_meta","payload":{"session_id":"sess-a","cwd":"/home/u/app"}}\n'
            )
            index_file = Path(temp_dir) / "session_index.jsonl"
            index_file.write_text('{"id":"sess-a","thread_name":"Before"}\n')
            relay.session_name_cache.clear()
            try:
                with patch.object(relay, "CODEX_SESSIONS_DIR", sessions_dir), \
                        patch.object(relay, "CODEX_SESSION_INDEX_FILE", index_file):
                    self.assertEqual(
                        relay.cached_session_name("Codex", "/home/u/app", "sess-a"),
                        "Before",
                    )
                    index_file.write_text('{"id":"sess-a","thread_name":"After rename"}\n')
                    self.assertEqual(
                        relay.cached_session_name("Codex", "/home/u/app", "sess-a"),
                        "After rename",
                    )
            finally:
                relay.session_name_cache.clear()

    def test_agent_inventory_sends_empty_session_to_clear_previous_title(self):
        pane_result = json.dumps({
            "result": {
                "panes": [{
                    "agent": "codex",
                    "agent_status": "working",
                    "cwd": "/home/me/project",
                    "pane_id": "w1:p1",
                    "tab_id": "w1:t1",
                }],
            },
        })
        tab_result = json.dumps({"result": {"tabs": []}})
        with patch.object(
            relay,
            "run_herdr_result",
            side_effect=[(True, pane_result, ""), (True, tab_result, "")],
        ), patch.object(relay, "cached_session_name", return_value=""):
            agents, inventory_error = relay.get_agent_inventory()

        self.assertIsNone(inventory_error)
        self.assertEqual(agents[0]["session"], "")

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
            self.assertIn(pane, relay.unseen_done_panes)
            relay.register_status_transition(pane, "idle", "done")
            self.assertEqual(relay.displayed_status(pane, "idle"), "done")

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

        with patch.object(relay, "cached_herdr_integrations", return_value=set()), \
                patch.object(relay.shutil, "which", side_effect=find_executable):
            profiles = relay.load_agent_profiles()
        self.assertEqual(set(profiles), {"codex", "claude"})
        self.assertEqual(profiles["claude"]["argv"], ["/usr/bin/claude"])
        self.assertEqual(profiles["claude"]["kind"], "claude")

    def test_generated_clear_agent_name_obeys_herdr_075_rules(self):
        with patch.object(relay.time, "time", return_value=123456):
            name = relay.generated_clear_agent_name("123.Custom Profile With A Very Long Name")

        self.assertLessEqual(len(name), 32)
        self.assertRegex(name, r"^[a-z][a-z0-9_-]{0,31}$")

    def test_herdr_installed_integrations_parses_status(self):
        status_output = "\n".join([
            "pi: not installed (/home/me/.pi/agent/extensions/herdr-agent-state.ts)",
            "codex: current (v6) (/home/me/.codex/herdr-agent-state.sh)",
            "opencode: outdated (v1) (/home/me/.config/opencode/plugins/herdr-agent-state.js)",
            "qodercli: current (v2) (/home/me/.qoder/hooks/herdr-agent-state.sh)",
        ])
        with patch.object(relay, "run_herdr_result", return_value=(True, status_output, "")):
            self.assertEqual(
                relay.herdr_installed_integrations(),
                {"codex", "opencode", "qodercli"},
            )

    def test_herdr_installed_integrations_empty_when_herdr_unavailable(self):
        with patch.object(relay, "run_herdr_result", return_value=(False, "", "herdr missing")):
            self.assertEqual(relay.herdr_installed_integrations(), set())

    def test_load_agent_profiles_includes_installed_integrations(self):
        def find_executable(name):
            return f"/usr/bin/{name}" if name in {"codex", "qodercli"} else None

        with patch.object(relay, "cached_herdr_integrations", return_value={"codex", "qodercli", "pi"}), \
                patch.object(relay.shutil, "which", side_effect=find_executable):
            profiles = relay.load_agent_profiles()
        self.assertIn("qodercli", profiles)
        self.assertEqual(profiles["qodercli"]["label"], "Qoder")
        self.assertEqual(profiles["qodercli"]["argv"], ["/usr/bin/qodercli"])
        # An installed integration whose binary is not on PATH is not advertised.
        self.assertNotIn("pi", profiles)
        # A default candidate without a binary is still excluded.
        self.assertNotIn("claude", profiles)

    def test_load_agent_profiles_configured_label_wins_over_integration(self):
        def find_executable(name):
            return f"/usr/bin/{name}" if name == "qodercli" else None

        with patch.object(relay, "cached_herdr_integrations", return_value={"qodercli"}), \
                patch.object(relay.shutil, "which", side_effect=find_executable), \
                patch.dict(relay.AGENT_PROFILE_CANDIDATES, {"qodercli": "Qoder CLI"}):
            profiles = relay.load_agent_profiles()
        self.assertEqual(profiles["qodercli"]["label"], "Qoder CLI")

    def test_replace_profiles_mode_suppresses_integration_auto_detection(self):
        ini_content = "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        parser = configparser.ConfigParser(interpolation=None)
        parser.read_string(ini_content)
        with patch.object(relay, "_AGENT_PROFILES_INI_CACHE", parser), \
                patch.dict(relay.AGENT_PROFILE_CANDIDATES, {"pi": "Pi"}, clear=True), \
                patch.object(relay, "cached_herdr_integrations", return_value={"codex", "qodercli"}) as mock_integrations, \
                patch.object(relay.shutil, "which", side_effect=lambda name: f"/usr/bin/{name}" if name == "pi" else None):
            profiles = relay.load_agent_profiles()
        self.assertEqual(set(profiles), {"pi"})
        mock_integrations.assert_not_called()

    def test_replace_profiles_mode_skips_startup_prewarm_discovery(self):
        ini_content = "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        parser = configparser.ConfigParser(interpolation=None)
        parser.read_string(ini_content)
        cache = {"names": set(), "expires": 0.0, "refreshing": False}
        with patch.object(relay, "_AGENT_PROFILES_INI_CACHE", parser), \
                patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "herdr_installed_integrations", return_value={"qodercli"}) as mock_discovery:
            relay.prewarm_herdr_integrations()
        mock_discovery.assert_not_called()
        self.assertEqual(cache["names"], set())

    def test_cached_herdr_integrations_returns_stale_and_refreshes_in_background(self):
        cache = {"names": {"codex"}, "expires": 0.0, "refreshing": False}
        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "herdr_installed_integrations", return_value={"codex", "qodercli"}), \
                patch.object(relay.threading, "Thread") as mock_thread:
            result = relay.cached_herdr_integrations()
        self.assertEqual(result, {"codex"})
        mock_thread.assert_called_once()
        self.assertTrue(cache["refreshing"])

    def test_prewarm_populates_cache_before_first_client(self):
        cache = {"names": set(), "expires": 0.0, "refreshing": False}
        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "herdr_installed_integrations", return_value={"qodercli", "codex"}):
            relay.prewarm_herdr_integrations()
        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay.threading, "Thread") as mock_thread:
            self.assertEqual(relay.cached_herdr_integrations(), {"qodercli", "codex"})
        mock_thread.assert_not_called()

    def test_obsolete_integration_refresh_cannot_update_cache_or_readiness(self):
        # A reload bumped the generation to 5 and cleared readiness; a refresh
        # started earlier under generation 4 now returns with stale data.
        cache = {"names": {"stale"}, "expires": 0.0, "refreshing": True}
        ready = threading.Event()
        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "_integrations_ready", ready), \
                patch.object(relay, "_integrations_generation", 5), \
                patch.object(relay, "herdr_installed_integrations", return_value={"stale-result"}):
            relay._refresh_herdr_integrations(4)
        self.assertEqual(cache["names"], {"stale"})
        self.assertTrue(cache["refreshing"])
        self.assertFalse(ready.is_set())

    def test_current_generation_integration_refresh_updates_cache_and_readiness(self):
        cache = {"names": set(), "expires": 0.0, "refreshing": True}
        ready = threading.Event()
        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "_integrations_ready", ready), \
                patch.object(relay, "_integrations_generation", 5), \
                patch.object(relay, "herdr_installed_integrations", return_value={"qodercli"}):
            relay._refresh_herdr_integrations(5)
        self.assertEqual(cache["names"], {"qodercli"})
        self.assertFalse(cache["refreshing"])
        self.assertTrue(ready.is_set())

    def test_load_activity_respects_byte_budget(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            activity_file = Path(temp_dir) / "activity.jsonl"
            entry = {"id": "x", "summary": "y" * 500}
            line = json.dumps(entry, separators=(",", ":")) + "\n"
            activity_file.write_text(line * 20)
            budget = len(line) * 5
            with patch.object(relay, "ACTIVITY_FILE", activity_file), \
                    patch.object(relay, "ACTIVITY_MAX_ITEMS", 500), \
                    patch.object(relay, "ACTIVITY_MAX_BYTES", budget):
                entries = relay.load_activity(500)
            self.assertEqual(len(entries), 5)

    def test_blocked_event_id_is_pre_generated_in_agents_snapshot(self):
        blocked = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex"}
        details = {}
        with patch.object(relay, "blocked_agent_details", details):
            details["w1:p1"] = {"event_id": "pre-generated-id"}
            msg = json.loads(relay.agents_message([blocked]))
        self.assertEqual(msg["agents"][0]["event_id"], "pre-generated-id")

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

        with patch.object(
            relay,
            "run_herdr_result",
            side_effect=[(True, pane_result, ""), (True, tab_result, "")],
        ):
            agents = relay.get_agents()

        self.assertEqual(
            agents[0]["_activity_fingerprint"],
            ("working", 4, 27, "/home/me/project", "/home/me/project", ""),
        )

    def test_agent_inventory_classifies_client_server_protocol_mismatch(self):
        error = (
            "client protocol 17 is newer than server protocol 16; "
            "restart the Herdr server before using this command"
        )
        with patch.object(relay, "run_herdr_result", return_value=(False, "", error)):
            agents, inventory_error = relay.get_agent_inventory()

        self.assertIsNone(agents)
        self.assertEqual(inventory_error[0], "protocol_mismatch")
        self.assertIn("herdr server live-handoff", inventory_error[1])

    def test_agent_inventory_classifies_machine_protocol_mismatch(self):
        with patch.object(
            relay,
            "run_herdr_result",
            return_value=(False, "", '{"code":"protocol_mismatch"}'),
        ):
            agents, inventory_error = relay.get_agent_inventory()

        self.assertIsNone(agents)
        self.assertEqual(inventory_error[0], "protocol_mismatch")

    def test_agent_inventory_accepts_successful_empty_pane_list(self):
        with patch.object(
            relay,
            "run_herdr_result",
            side_effect=[
                (True, '{"result":{"panes":[]}}', ""),
                (True, '{"result":{"tabs":[]}}', ""),
            ],
        ):
            agents, inventory_error = relay.get_agent_inventory()

        self.assertEqual(agents, [])
        self.assertIsNone(inventory_error)

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
            home = Path(temp_dir).resolve()
            development = home / "Development"
            project = development / "relay"
            downloads = home / "Downloads"
            hidden = home / ".private"
            outside_link = home / "outside"
            for path in (project, downloads, hidden):
                path.mkdir(parents=True, exist_ok=True)
            outside_link.symlink_to(outside_dir, target_is_directory=True)

            real_scandir = os.scandir

            def macos_scandir(path):
                if Path(path) == downloads:
                    raise PermissionError("Operation not permitted")
                return real_scandir(path)

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

    def test_slash_command_catalog_uses_agent_specific_builtins(self):
        codex = relay.slash_command_catalog({"agent": "codex", "cwd": "/tmp"})
        opencode = relay.slash_command_catalog({"agent": "opencode", "cwd": "/tmp"})

        commands = {entry["command"] for entry in codex["commands"]}
        self.assertIn("/permissions", commands)
        self.assertIn("/skills", commands)
        self.assertNotIn("/add-dir", commands)
        self.assertFalse(codex["truncated"])
        # Non-Claude, non-Codex agents dynamically discover skills.
        # The test environment has ~/.opencode/skills, so the result
        # is structural rather than a fixed snapshot.
        self.assertIsInstance(opencode["commands"], list)
        self.assertIsInstance(opencode["truncated"], bool)

    def test_claude_slash_commands_merge_project_and_personal_metadata(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            project = home / "Development" / "relay"
            cwd = project / "frontend"
            (project / ".git").mkdir(parents=True)
            cwd.mkdir()
            project_commands = project / ".claude" / "commands"
            project_commands.mkdir(parents=True)
            (project_commands / "deploy.md").write_text(
                "---\ndescription: Deploy from the project\nargument-hint: [environment]\n---\n"
            )
            project_skills = project / ".claude" / "skills"
            (project_skills / "hidden").mkdir(parents=True)
            (project_skills / "hidden" / "SKILL.md").write_text(
                "---\ndescription: Background context\nuser-invocable: false\n---\n"
            )
            (project_skills / "disabled").mkdir()
            (project_skills / "disabled" / "SKILL.md").write_text(
                "---\ndescription: Disabled from settings\n---\n"
            )
            settings_dir = project / ".claude"
            (settings_dir / "settings.local.json").write_text(json.dumps({
                "skillOverrides": {"disabled": "off", "verify": "off"},
            }))
            personal_skills = home / ".claude" / "skills" / "deploy"
            personal_skills.mkdir(parents=True)
            (personal_skills / "SKILL.md").write_text(
                "---\ndescription: Deploy using personal policy\nargument-hint: [target]\n---\n"
            )

            with patch.object(relay.Path, "home", return_value=home):
                catalog = relay.slash_command_catalog({"agent": "claude-code", "cwd": str(cwd)})

        commands = {entry["command"]: entry for entry in catalog["commands"]}
        self.assertEqual(commands["/deploy"], {
            "command": "/deploy",
            "description": "Deploy using personal policy",
            "argument_hint": "[target]",
            "source": "personal",
        })
        self.assertNotIn("/hidden", commands)
        self.assertNotIn("/disabled", commands)
        self.assertNotIn("/verify", commands)
        self.assertEqual(commands["/help"]["source"], "builtin")

    def test_claude_slash_command_discovery_is_bounded(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            home = Path(temp_dir)
            cwd = home / "project"
            command_dir = cwd / ".claude" / "commands"
            command_dir.mkdir(parents=True)
            (cwd / ".git").mkdir()
            (command_dir / "first.md").write_text("---\ndescription: First\n---\n")
            (command_dir / "second.md").write_text("---\ndescription: Second\n---\n")
            with (
                patch.object(relay.Path, "home", return_value=home),
                patch.object(relay, "SLASH_COMMAND_MAX_CUSTOM_FILES", 1),
            ):
                catalog = relay.slash_command_catalog({"agent": "claude", "cwd": str(cwd)})

        self.assertTrue(catalog["truncated"])
        commands = {entry["command"] for entry in catalog["commands"]}
        self.assertIn("/first", commands)
        self.assertNotIn("/second", commands)

    # ── Dynamic agent-profiles INI tests ─────────────────────────────

    def _make_test_ini(self, text, base_dir=""):
        """Write a temporary agent-profiles.ini and point the relay at it.

        Returns ``(ini, base)`` and registers cleanup for the temp file.
        The caller must still patch ``_CONFIG_HOME``, ``_AGENT_PROFILES_INI``,
        and ``_AGENT_PROFILES_INI_CACHE`` in the test body.
        """
        base = Path(base_dir) if base_dir else Path(tempfile.mkdtemp())
        ini = base / "agent-profiles.ini"
        ini.parent.mkdir(parents=True, exist_ok=True)
        ini.write_text(text)
        self.addCleanup(lambda: ini.unlink(missing_ok=True))
        return ini, base

    @contextmanager
    def _patch_ini_config(self, ini, base):
        """Context manager that patches the relay's INI wiring end-to-end.

        Patches ``_CONFIG_HOME``, ``_AGENT_PROFILES_INI``, the cached
        parser, **and** ``AGENT_PROFILE_CANDIDATES`` so that
        ``_load_agent_profiles_from_config`` and ``_agent_skill_dirs``
        see the temp file.  All state is auto-restored when the context
        exits — no stale parser or mutated constants leak into other tests.
        """
        saved_candidates = dict(relay.AGENT_PROFILE_CANDIDATES)
        with (
            patch.object(relay, "_CONFIG_HOME", base),
            patch.object(relay, "_AGENT_PROFILES_INI", ini),
        ):
            with patch.object(relay, "_AGENT_PROFILES_INI_CACHE", relay._read_agent_profiles_ini()):
                try:
                    relay.AGENT_PROFILE_CANDIDATES = (
                        relay._load_agent_profiles_from_config()
                        or relay._DEFAULT_AGENT_PROFILE_CANDIDATES
                    )
                    relay._MISSING_AGENT_WARNED.clear()
                    relay._INVALID_COMMAND_FORMAT_WARNED.clear()
                    yield ini, base
                finally:
                    relay.AGENT_PROFILE_CANDIDATES = saved_candidates
                    relay._MISSING_AGENT_WARNED.clear()
                    relay._INVALID_COMMAND_FORMAT_WARNED.clear()

    def test_dynamic_profiles_merge_with_defaults(self):
        ini, base = self._make_test_ini("[profiles]\npi = Pi\n")
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
        self.assertIn("codex", candidates)
        self.assertIn("claude", candidates)
        self.assertIn("opencode", candidates)
        self.assertIn("pi", candidates)
        self.assertEqual(candidates["pi"], "Pi")

    def test_dynamic_profiles_replace_mode(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        )
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
        self.assertEqual(set(candidates), {"pi"})

    def test_dynamic_profiles_missing_file_falls_back_to_defaults(self):
        ini, base = self._make_test_ini("")
        ini.unlink()
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
        # No file → None returned → defaults used by module constant.
        self.assertIsNone(candidates)

    def test_dynamic_profiles_empty_section_falls_back(self):
        ini, base = self._make_test_ini("[profiles]\n")
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
        self.assertIsNone(candidates)

    def test_dynamic_profiles_override_default_label(self):
        ini, base = self._make_test_ini("[profiles]\ncodex = OpenCode (local)\n")
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
        self.assertEqual(candidates["codex"], "OpenCode (local)")

    def test_percent_signs_do_not_crash_ini_parsing(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi %00\n"
            "[skills]\npi = /tmp/skills%%dir0" + os.pathsep + "/tmp/%%dir1\n"
            "[config]\nreplace_profiles = false\n"
        )
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
            dirs = relay._agent_skill_dirs("pi")
        self.assertEqual(candidates["pi"], "Pi %00")
        # interpolation=None preserves literal %% characters.
        self.assertEqual(dirs, ["/tmp/skills%%dir0", "/tmp/%%dir1"])

    def test_malformed_ini_does_not_crash(self):
        ini, base = self._make_test_ini("this is not valid INI at all [[[\n???\n")
        with self._patch_ini_config(ini, base):
            candidates = relay._load_agent_profiles_from_config()
            dirs = relay._agent_skill_dirs("pi")
        self.assertIsNone(candidates)
        # Falls back to default skill dirs.
        self.assertTrue(any("pi" in d for d in dirs))

    def test_skill_discovery_labels_first_dir_personal(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            (Path(skills_dir) / "alpha" / "SKILL.md").parent.mkdir(parents=True)
            (Path(skills_dir) / "alpha" / "SKILL.md").write_text(
                "---\nname: alpha\ndescription: First skill\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {skills_dir}\n"
            )
            with self._patch_ini_config(ini, base):
                dirs = relay._agent_skill_dirs("pi")
                cmds, _trunc = relay.discover_agent_skills("pi")
            self.assertEqual(dirs, [skills_dir])
            self.assertTrue(any(c["command"] == "/skill:alpha" for c in cmds))
            self.assertTrue(any(
                c["command"] == "/skill:alpha" and c["source"] == "personal"
                for c in cmds
            ))

    def test_skill_discovery_labels_second_dir_project(self):
        with tempfile.TemporaryDirectory() as dir1, tempfile.TemporaryDirectory() as dir2:
            (Path(dir1) / "alpha" / "SKILL.md").parent.mkdir(parents=True)
            (Path(dir1) / "alpha" / "SKILL.md").write_text(
                "---\nname: alpha\ndescription: Personal\n---\n"
            )
            (Path(dir2) / "beta" / "SKILL.md").parent.mkdir(parents=True)
            (Path(dir2) / "beta" / "SKILL.md").write_text(
                "---\nname: beta\ndescription: Project\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {dir1}{os.pathsep}{dir2}\n"
            )
            with self._patch_ini_config(ini, base):
                cmds, _trunc = relay.discover_agent_skills("pi")
            beta = next(c for c in cmds if c["command"] == "/skill:beta")
            self.assertEqual(beta["source"], "project")

    def test_skill_discovery_first_dir_wins_on_duplicate(self):
        with tempfile.TemporaryDirectory() as dir1, tempfile.TemporaryDirectory() as dir2:
            (Path(dir1) / "dup" / "SKILL.md").parent.mkdir(parents=True)
            (Path(dir1) / "dup" / "SKILL.md").write_text(
                "---\nname: dup\ndescription: First\n---\n"
            )
            (Path(dir2) / "dup" / "SKILL.md").parent.mkdir(parents=True)
            (Path(dir2) / "dup" / "SKILL.md").write_text(
                "---\nname: dup\ndescription: Second\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {dir1}{os.pathsep}{dir2}\n"
            )
            with self._patch_ini_config(ini, base):
                cmds, _trunc = relay.discover_agent_skills("pi")
            dup = [c for c in cmds if c["command"] == "/skill:dup"]
            self.assertEqual(len(dup), 1)
            self.assertEqual(dup[0]["description"], "First")

    def test_pi_skills_use_skill_prefix(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            (Path(skills_dir) / "triage" / "SKILL.md").parent.mkdir(parents=True)
            (Path(skills_dir) / "triage" / "SKILL.md").write_text(
                "---\nname: triage\ndescription: Triage issues\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {skills_dir}\n"
            )
            with self._patch_ini_config(ini, base):
                cmds, _trunc = relay.discover_agent_skills("pi")
            self.assertTrue(any(c["command"] == "/skill:triage" for c in cmds))

    def test_non_invocable_skills_are_filtered(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            (Path(skills_dir) / "visible" / "SKILL.md").parent.mkdir(parents=True)
            (Path(skills_dir) / "visible" / "SKILL.md").write_text(
                "---\nname: visible\ndescription: Seen\n---\n"
            )
            (Path(skills_dir) / "hidden" / "SKILL.md").parent.mkdir()
            (Path(skills_dir) / "hidden" / "SKILL.md").write_text(
                "---\nname: hidden\ndescription: Not invocable\nuser-invocable: false\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {skills_dir}\n"
            )
            with self._patch_ini_config(ini, base):
                cmds, _trunc = relay.discover_agent_skills("pi")
            names = {c["command"] for c in cmds}
            self.assertIn("/skill:visible", names)
            self.assertNotIn("/skill:hidden", names)

    def test_skill_discovery_truncation(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            for i in range(3):
                (Path(skills_dir) / f"s{i}" / "SKILL.md").parent.mkdir(parents=True)
                (Path(skills_dir) / f"s{i}" / "SKILL.md").write_text(
                    f"---\nname: s{i}\ndescription: Skill {i}\n---\n"
                )
            ini, base = self._make_test_ini(
                f"[skills]\npi = {skills_dir}\n"
            )
            with self._patch_ini_config(ini, base):
                with patch.object(relay, "SLASH_COMMAND_MAX_CUSTOM_FILES", 2):
                    cmds, truncated = relay.discover_agent_skills("pi")
            self.assertTrue(truncated)
            self.assertEqual(len(cmds), 2)

    def test_full_catalog_for_configured_agent_discovers_skills(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            (Path(skills_dir) / "firehose" / "SKILL.md").parent.mkdir(parents=True)
            (Path(skills_dir) / "firehose" / "SKILL.md").write_text(
                "---\nname: firehose\ndescription: Firehose skill\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[profiles]\npi = Pi\n[skills]\npi = {skills_dir}\n"
            )
            with self._patch_ini_config(ini, base):
                catalog = relay.slash_command_catalog({"agent": "pi"})
            self.assertTrue(any(
                c["command"] == "/skill:firehose" for c in catalog["commands"]
            ))

    # Reload takes a module lock, so only one test should do it.  This test
    # proves that the module-level constant ``AGENT_PROFILE_CANDIDATES`` is
    # computed from ``_load_agent_profiles_from_config()`` at import time
    # and falls back to ``_DEFAULT_AGENT_PROFILE_CANDIDATES`` when the INI
    # file is absent.
    def test_agent_profile_candidates_is_module_constant_from_config(self):
        """``AGENT_PROFILE_CANDIDATES`` is computed at import time.

        The constant is always a non-empty dict. When
        ``_load_agent_profiles_from_config`` returns ``None`` (no INI),
        it falls back to ``_DEFAULT_AGENT_PROFILE_CANDIDATES``.
        """
        # Sanity: the module constant exists and is a dict.
        self.assertIsInstance(relay.AGENT_PROFILE_CANDIDATES, dict)
        self.assertTrue(relay.AGENT_PROFILE_CANDIDATES)
        # The constant is composed of the defaults when no INI exists.
        # Use a temp config to avoid depending on the real environment.
        ini, base = self._make_test_ini("")
        ini.unlink()
        with self._patch_ini_config(ini, base):
            for key in ("codex", "claude", "opencode"):
                self.assertIn(key, relay.AGENT_PROFILE_CANDIDATES)

    def test_sighup_reloads_agent_profiles_from_changed_ini(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        )
        with self._patch_ini_config(ini, base):
            relay._reload_agent_profiles_ini()
            self.assertEqual(
                set(relay.AGENT_PROFILE_CANDIDATES),
                {"pi"},
                "After SIGHUP, profiles should reflect the changed INI",
            )

    def test_sighup_reverts_to_defaults_when_ini_removed(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        )
        with self._patch_ini_config(ini, base):
            relay._reload_agent_profiles_ini()
            self.assertIn("pi", relay.AGENT_PROFILE_CANDIDATES)
            # Simulate the INI being deleted: point to a nonexistent path
            # and reset the cache so reload picks up defaults.
            missing = base / "does-not-exist.ini"
            with patch.object(relay, "_AGENT_PROFILES_INI", missing):
                relay._AGENT_PROFILES_INI_CACHE = None
                relay._reload_agent_profiles_ini()
            self.assertNotIn("pi", relay.AGENT_PROFILE_CANDIDATES)
            for key in ("codex", "claude", "opencode"):
                self.assertIn(key, relay.AGENT_PROFILE_CANDIDATES)

    def test_missing_binary_warns_only_for_user_configured_profiles(self):
        ini, base = self._make_test_ini(
            "[profiles]\nnonexistent = Ghost\n[config]\nreplace_profiles = true\n"
        )
        with self._patch_ini_config(ini, base):
            relay._reload_agent_profiles_ini()
            with patch("builtins.print") as mock_print:
                relay.load_agent_profiles()
        warnings = [
            args[0]
            for args, _kwargs in mock_print.call_args_list
            if "WARNING" in str(args[0])
        ]
        self.assertTrue(
            any("nonexistent" in w for w in warnings),
            "Should warn for configured profile with no binary",
        )
        # Default profiles (codex, claude, opencode) are never warned about
        # even when absent.
        self.assertFalse(
            any("codex" in w or "claude" in w or "opencode" in w for w in warnings),
            "Default profiles should not produce warnings",
        )

    def test_workspace_selection_prefers_the_space_owned_by_the_working_directory(self):
        cwd = Path("/home/test/Development/project")
        panes = {
            "panes": [
                {"workspace_id": "w1", "cwd": "/home/test/other"},
                {"workspace_id": "w1", "cwd": str(cwd)},
                {"workspace_id": "w2", "cwd": str(cwd)},
                {"workspace_id": "w2", "cwd": str(cwd)},
            ],
        }
        workspaces = {
            "workspaces": [
                {"workspace_id": "w1", "label": "other"},
                {"workspace_id": "w2", "label": "project"},
            ],
        }

        self.assertEqual(relay.select_workspace_for_cwd(cwd, panes, workspaces), "w2")

    def test_workspace_selection_does_not_reuse_ambiguous_stray_panes(self):
        cwd = Path("/home/test/Development/project")
        panes = {
            "panes": [
                {"workspace_id": "w1", "cwd": "/home/test/one"},
                {"workspace_id": "w1", "cwd": str(cwd)},
                {"workspace_id": "w2", "cwd": "/home/test/two"},
                {"workspace_id": "w2", "cwd": str(cwd)},
            ],
        }
        workspaces = {
            "workspaces": [
                {"workspace_id": "w1", "label": "one"},
                {"workspace_id": "w2", "label": "two"},
            ],
        }

        self.assertEqual(relay.select_workspace_for_cwd(cwd, panes, workspaces), "")
        self.assertEqual(
            relay.select_workspace_for_cwd(
                cwd,
                {"panes": panes["panes"][:2]},
                {"workspaces": workspaces["workspaces"][:1]},
            ),
            "",
        )

    def test_profile_id_uses_remembered_pane_mapping_without_substring_guessing(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\npi-local = Pi Local\n"
            "[config]\nreplace_profiles = true\n"
            "[aliases]\npi-coding-agent = pi\n"
        )
        with self._patch_ini_config(ini, base):
            relay.remember_agent_profile("w1:p1", "pi-local")
            self.assertEqual(
                relay._profile_id_for_agent({
                    "pane_id": "w1:p1",
                    "agent": "pi-coding-agent",
                }),
                "pi-local",
            )
            self.assertEqual(
                relay._profile_id_for_agent({"agent": "pi-coding-agent"}),
                "pi",
            )
            self.assertEqual(relay._profile_id_for_agent({"agent": "pi"}), "pi")
            self.assertEqual(
                relay._profile_id_for_agent({"agent": "spider-agent"}),
                "",
            )

    def test_configured_alias_recovers_profile_for_existing_pane(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi-local = Pi Local\n"
            "[config]\nreplace_profiles = true\n"
            "[aliases]\npi-coding-agent = pi-local\n"
        )
        with self._patch_ini_config(ini, base):
            self.assertEqual(
                relay._profile_id_for_agent({
                    "pane_id": "w1:p1",
                    "agent": "pi-coding-agent",
                }),
                "pi-local",
            )

    def test_prune_agent_profiles_removes_only_closed_panes(self):
        relay.agent_profile_ids.update({
            "w1:p1": "pi",
            "w1:p2": "pi-local",
        })

        relay.prune_agent_profiles({"w1:p1", "w1:p2"})
        relay.prune_agent_profiles({"w1:p2"})

        self.assertEqual(relay.agent_profile_ids, {"w1:p2": "pi-local"})

    def test_prune_agent_profiles_keeps_new_mapping_until_pane_is_observed(self):
        relay.remember_agent_profile("w1:p1", "pi-local")

        relay.prune_agent_profiles(set())

        self.assertEqual(relay.agent_profile_ids, {"w1:p1": "pi-local"})

    def test_non_utf8_ini_falls_back_without_crashing(self):
        ini, base = self._make_test_ini("")
        # Write bytes that are not valid UTF-8
        ini.write_bytes(b"\xff\xfe[profiles]\npi = Pi\n")
        with self._patch_ini_config(ini, base):
            # Should return None (fallback) instead of crashing
            self.assertIsNone(relay._load_agent_profiles_from_config())

    def test_default_profiles_never_warn_about_missing_binaries(self):
        ini, base = self._make_test_ini("")
        ini.unlink()
        with self._patch_ini_config(ini, base):
            with patch("builtins.print") as mock_print:
                relay.load_agent_profiles()
            warnings = [
                args[0]
                for args, _kwargs in mock_print.call_args_list
                if "WARNING" in str(args[0])
            ]
            self.assertEqual(warnings, [], "Default profiles should never produce warnings")

    def test_command_format_is_none_for_unconfigured_agents(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
        )
        with self._patch_ini_config(ini, base):
            self.assertEqual(relay._agent_command_format("pi"), "skill:{name}")
            self.assertIsNone(relay._agent_command_format("opencode"))
            self.assertIsNone(relay._agent_command_format("unknown-agent"))

    def test_command_format_can_be_disabled_in_ini(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
            "[commands]\npi = off\n"
        )
        with self._patch_ini_config(ini, base):
            self.assertIsNone(relay._agent_command_format("pi"))

    def test_empty_command_format_disables_default(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
            "[commands]\npi =\n"
        )
        with self._patch_ini_config(ini, base):
            self.assertIsNone(relay._agent_command_format("pi"))

    def test_command_format_can_be_customised_in_ini(self):
        ini, base = self._make_test_ini(
            "[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n"
            "[commands]\npi = /custom:{name}\n"
        )
        with self._patch_ini_config(ini, base):
            self.assertEqual(relay._agent_command_format("pi"), "/custom:{name}")

    def test_invalid_command_format_disables_suggestions_without_crashing(self):
        with tempfile.TemporaryDirectory() as skills_dir:
            skill = Path(skills_dir) / "firehose" / "SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text(
                "---\nname: firehose\ndescription: Firehose skill\n---\n"
            )
            ini, base = self._make_test_ini(
                f"[profiles]\npi = Pi\n[skills]\npi = {skills_dir}\n"
                "[commands]\npi = {unknown}\n"
            )
            with self._patch_ini_config(ini, base):
                with patch("builtins.print") as mock_print:
                    commands, truncated = relay.discover_agent_skills("pi")
                    repeated = relay._agent_command_format("pi")

            self.assertEqual(commands, [])
            self.assertFalse(truncated)
            self.assertIsNone(repeated)
            mock_print.assert_called_once()

    def test_launched_custom_profile_keeps_identity_for_skill_catalog(self):
        with (
            tempfile.TemporaryDirectory() as temp_dir,
            tempfile.TemporaryDirectory() as skills_dir,
        ):
            skill = Path(skills_dir) / "firehose" / "SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text(
                "---\nname: firehose\ndescription: Firehose skill\n---\n"
            )
            ini, base = self._make_test_ini(
                "[profiles]\npi-local = Pi Local\n"
                "[config]\nreplace_profiles = true\n"
                f"[skills]\npi-local = {skills_dir}\n"
                "[commands]\npi-local = skill:{name}\n"
            )
            command_results = [
                (
                    True,
                    json.dumps({
                        "result": {"pane_id": "w7:p0", "workspace_id": "w7", "tab_id": "w7:t2"}
                    }),
                    "",
                ),
                (True, "", ""),
                (
                    True,
                    json.dumps({
                        "result": {"agent": {"pane_id": "w7:p0", "workspace_id": "w7"}}
                    }),
                    "",
                ),
                (True, "", ""),
            ]
            profile = {
                "id": "pi-local",
                "label": "Pi Local",
                "argv": ["pi-local"],
            }
            with (
                self._patch_ini_config(ini, base),
                patch.object(
                    relay,
                    "workspace_id_for_cwd",
                    AsyncMock(return_value="w7"),
                ),
                patch.object(
                    relay,
                    "run_herdr_async_result",
                    AsyncMock(side_effect=command_results),
                ),
            ):
                ok, _data, pane_id, placement_error, error = asyncio.run(
                    relay.start_agent_in_new_tab(
                        profile,
                        "mobile-pi",
                        Path(temp_dir),
                    )
                )
                catalog = relay.slash_command_catalog({
                    "pane_id": pane_id,
                    "agent": "pi-coding-agent",
                })

            self.assertTrue(ok)
            self.assertEqual(placement_error, "")
            self.assertEqual(error, "")
            self.assertEqual(pane_id, "w7:p0")
            self.assertEqual(relay.agent_profile_ids[pane_id], "pi-local")
            self.assertIn(
                "/skill:firehose",
                {entry["command"] for entry in catalog["commands"]},
            )


class RelayCommandsTest(ClaudeHistoryIsolationMixin, unittest.IsolatedAsyncioTestCase):
    async def test_clear_activities_broadcasts_empty_history_and_confirms(self):
        ws = FakeWebSocket()
        broadcast = AsyncMock()
        with (
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(return_value=(True, "")),
            ) as to_thread,
            patch.object(relay, "broadcast", broadcast),
        ):
            await relay.handle_clear_activities_command(ws, {
                "type": "clear_activities",
                "request_id": "clear-1",
            })

        to_thread.assert_awaited_once_with(relay.clear_activity_history)
        broadcast.assert_awaited_once_with({
            "type": "activity_history",
            "activities": [],
        })
        self.assertEqual(ws.messages[-1]["action"], "clear_activities")
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_publish_finished_records_excerpt_and_matches_notification(self):
        captured = {}

        async def fake_publish_activity(*args, **kwargs):
            captured["args"] = args
            captured["kwargs"] = kwargs
            return {"id": "activity-id", "timestamp": 123}

        with (
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "read_pane", return_value="planned the release\nall tests green"),
            patch.object(relay, "publish_activity", side_effect=fake_publish_activity),
            patch.object(relay, "send_finished_webpush_notifications") as send_finished,
        ):
            await relay.publish_finished({
                "pane_id": "w1:p2",
                "agent": "codex",
                "project": "relay",
                "host": "fedora",
            })

        self.assertEqual(captured["args"][0], "finished")
        self.assertEqual(captured["kwargs"]["extract"], "planned the release\nall tests green")
        event_id = captured["kwargs"]["details"]["event_id"]
        self.assertTrue(event_id)
        # The finished notification must carry the same id so the app can open
        # the stored excerpt instead of scanning the live pane.
        self.assertEqual(send_finished.call_args.args[1], event_id)

    async def test_submit_prompt_atomically_sends_and_records_full_wall_text(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "agent": "codex", "project": "relay"}
        prompt = "wall of text\n" * 7692 + "done"
        self.assertLessEqual(len(prompt), relay.PROMPT_MAX_CHARS)
        captured = {}

        async def fake_publish_activity(*args, **kwargs):
            captured["args"] = args
            captured["kwargs"] = kwargs
            return {"id": "activity-id", "timestamp": 123}

        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "run_herdr_async_result",
                AsyncMock(return_value=(True, "", "")),
            ) as run_command,
            patch.object(relay, "publish_activity", side_effect=fake_publish_activity),
        ):
            await relay.handle_submit_prompt_command(ws, {
                "type": "submit_prompt",
                "request_id": "request-1",
                "pane_id": "w1:p1",
                "text": prompt,
            })

        self.assertEqual(run_command.await_count, 1)
        self.assertEqual(run_command.await_args.args, ("agent", "prompt", "w1:p1", prompt))
        self.assertGreater(run_command.await_args.kwargs["deadline"], relay.time.monotonic() - 5)
        self.assertEqual(captured["args"][0], "prompt")
        self.assertEqual(captured["kwargs"]["extract"], prompt)

    async def test_slash_command_request_resolves_agent_from_pane(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "agent": "claude", "cwd": "/home/test/project"}
        catalog = {
            "commands": [{
                "command": "/help",
                "description": "Show help",
                "source": "builtin",
            }],
            "truncated": False,
        }
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")) as find_agent,
            patch.object(relay, "slash_command_catalog", return_value=catalog) as build_catalog,
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
        ):
            await relay.handle_list_slash_commands_command(ws, {
                "type": "list_slash_commands",
                "request_id": "request-slash",
                "pane_id": "w1:p1",
            })

        find_agent.assert_called_once_with("w1:p1")
        build_catalog.assert_called_once_with(agent)
        self.assertEqual(ws.messages, [{
            "type": "command_result",
            "request_id": "request-slash",
            "action": "list_slash_commands",
            "ok": True,
            "phase": "completed",
            "error": "",
            "pane_id": "w1:p1",
            "data": catalog,
        }])

    async def test_slash_command_request_rejects_missing_agent(self):
        ws = FakeWebSocket()
        await relay.handle_list_slash_commands_command(ws, {
            "type": "list_slash_commands",
            "request_id": "request-slash",
        })

        self.assertFalse(ws.messages[0]["ok"])
        self.assertEqual(ws.messages[0]["action"], "list_slash_commands")
        self.assertEqual(ws.messages[0]["error"], "Agent is required")

    async def _run_agent_rename(self, agent, herdr):
        ws = FakeWebSocket()
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "run_herdr_async_result", herdr),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_agent_rename_command(ws, {
                "type": "agent_rename",
                "request_id": "request-rename",
                "pane_id": "w1:p1",
                "name": "my-tab",
            })
        return ws

    async def test_agent_rename_also_renames_the_enclosing_tab(self):
        agent = {"pane_id": "w1:p1", "tab_id": "w1", "agent": "claude", "project": "relay"}
        herdr = AsyncMock(return_value=(True, "", ""))
        ws = await self._run_agent_rename(agent, herdr)
        calls = [call.args for call in herdr.await_args_list]
        self.assertIn(("agent", "rename", "w1:p1", "my-tab"), calls)
        self.assertIn(("tab", "rename", "w1", "my-tab"), calls)
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_agent_rename_reports_failure_when_tab_rename_fails(self):
        agent = {"pane_id": "w1:p1", "tab_id": "w1", "agent": "claude", "project": "relay"}
        herdr = AsyncMock(side_effect=[(True, "", ""), (False, "", "tab_not_found")])
        ws = await self._run_agent_rename(agent, herdr)
        self.assertFalse(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["error"], "tab_not_found")

    async def test_agent_rename_skips_tab_rename_without_a_tab_id(self):
        agent = {"pane_id": "w1:p1", "tab_id": "", "agent": "claude", "project": "relay"}
        herdr = AsyncMock(return_value=(True, "", ""))
        ws = await self._run_agent_rename(agent, herdr)
        self.assertEqual(herdr.await_count, 1)
        self.assertEqual(herdr.await_args.args, ("agent", "rename", "w1:p1", "my-tab"))
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_codex_choice_moves_focus_and_submits_current_answer(self):
        interaction = relay.parse_codex_question(CODEX_QUESTION_VIEW)
        at_choice = copy.deepcopy(interaction)
        at_choice["_focus"] = ("option", 2)
        move_focus = AsyncMock(return_value=(at_choice, ""))
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "move_question_focus", move_focus),
            patch.object(relay, "send_question_keys", send_keys),
        ):
            ok, error = await relay.execute_question_answer(
                "w1:p1", interaction, [2], False, ""
            )

        self.assertTrue(ok, error)
        move_focus.assert_awaited_once_with(
            "w1:p1", interaction, ("option", 2)
        )
        send_keys.assert_awaited_once_with("w1:p1", ["Enter"])

    async def test_codex_custom_answer_opens_notes_and_submits_them(self):
        interaction = relay.parse_codex_question(CODEX_QUESTION_VIEW)
        at_other = copy.deepcopy(interaction)
        at_other["_focus"] = ("option", 3)
        notes_open = copy.deepcopy(at_other)
        notes_open["_notes_active"] = True
        with_note = copy.deepcopy(notes_open)
        with_note["other"]["text"] = "Keep only the public contract"
        move_focus = AsyncMock(return_value=(at_other, ""))
        send_keys = AsyncMock(return_value=(True, ""))
        wait_state = AsyncMock(side_effect=[(notes_open, ""), (with_note, "")])
        send_text = AsyncMock(return_value=(True, "", ""))
        with (
            patch.object(relay, "move_question_focus", move_focus),
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(relay, "wait_for_question_state", wait_state),
            patch.object(relay, "run_herdr_async_result", send_text),
        ):
            ok, error = await relay.execute_question_answer(
                "w1:p1",
                interaction,
                [],
                True,
                "Keep only the public contract",
            )

        self.assertTrue(ok, error)
        move_focus.assert_awaited_once_with(
            "w1:p1", interaction, ("option", 3)
        )
        self.assertEqual(
            [call.args for call in send_keys.await_args_list],
            [("w1:p1", ["Tab"]), ("w1:p1", ["Ctrl+U"]), ("w1:p1", ["Enter"])],
        )
        send_text.assert_awaited_once_with(
            "pane", "send-text", "w1:p1", "Keep only the public contract"
        )

    async def test_codex_none_of_the_above_allows_empty_notes(self):
        interaction = relay.parse_codex_question(CODEX_QUESTION_VIEW)

        selected, other_selected, other_text, error = relay.validate_question_answer(
            {
                "selected_indices": [],
                "other_selected": True,
                "other_text": "",
            },
            interaction,
        )

        self.assertEqual(error, "")
        self.assertEqual(selected, [])
        self.assertTrue(other_selected)
        self.assertEqual(other_text, "")

    async def test_question_transition_ignores_one_transient_working_snapshot(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        next_interaction = {**interaction, "id": "next-question"}
        with (
            patch.object(
                relay,
                "get_agents",
                side_effect=[
                    [{"pane_id": "w1:p1", "status": "working"}],
                    [{"pane_id": "w1:p1", "status": "blocked"}],
                ],
            ),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(side_effect=[(MULTI_QUESTION_VIEW, interaction), ("next", next_interaction)]),
            ),
            patch.object(relay.asyncio, "sleep", AsyncMock()),
        ):
            transition = await relay.wait_for_question_transition(
                "w1:p1", interaction["id"]
            )

        self.assertEqual(transition, ("advanced", next_interaction, "blocked"))

    async def test_previous_question_sends_left_and_returns_the_prior_form(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        interaction["_can_go_back"] = True
        previous = {
            **interaction,
            "id": "previous-question",
            "question": "Which scope should be included?",
            "_can_go_back": False,
        }
        agent = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        ws = FakeWebSocket()
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction)),
            ),
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(
                relay,
                "wait_for_question_transition",
                AsyncMock(return_value=("advanced", previous, "blocked")),
            ),
            patch.object(relay, "publish_activity", AsyncMock()),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_navigate_question_command(ws, {
                "type": "navigate_question",
                "request_id": "question-previous",
                "pane_id": "w1:p1",
                "interaction_id": interaction["id"],
                "direction": "previous",
            })

        send_keys.assert_awaited_once_with("w1:p1", ["Left"])
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["phase"], "navigated")
        self.assertEqual(
            ws.messages[-1]["data"]["interaction"]["id"], "previous-question"
        )

    async def test_previous_question_rejects_first_tab_without_terminal_input(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        agent = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        ws = FakeWebSocket()
        send_keys = AsyncMock()
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction)),
            ),
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(relay, "publish_activity", AsyncMock()),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_navigate_question_command(ws, {
                "type": "navigate_question",
                "request_id": "question-first",
                "pane_id": "w1:p1",
                "interaction_id": interaction["id"],
                "direction": "previous",
            })

        send_keys.assert_not_awaited()
        self.assertFalse(ws.messages[-1]["ok"])
        self.assertEqual(
            ws.messages[-1]["data"]["interaction"]["id"], interaction["id"]
        )

    async def test_previous_question_keeps_the_current_form_when_navigation_sticks(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        interaction["_can_go_back"] = True
        agent = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        ws = FakeWebSocket()
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction)),
            ),
            patch.object(
                relay, "send_question_keys", AsyncMock(return_value=(True, ""))
            ),
            patch.object(
                relay,
                "wait_for_question_transition",
                AsyncMock(return_value=("stuck", interaction, "blocked")),
            ),
            patch.object(relay, "publish_activity", AsyncMock()),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_navigate_question_command(ws, {
                "type": "navigate_question",
                "request_id": "question-stuck",
                "pane_id": "w1:p1",
                "interaction_id": interaction["id"],
                "direction": "previous",
            })

        self.assertFalse(ws.messages[-1]["ok"])
        self.assertIn("did not open", ws.messages[-1]["error"])
        self.assertEqual(
            ws.messages[-1]["data"]["interaction"]["id"], interaction["id"]
        )

    async def test_question_option_uses_cursor_navigation_and_verifies_selection(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        focused = copy.deepcopy(interaction)
        focused["_focus"] = ("option", 1)
        selected = copy.deepcopy(focused)
        selected["options"][1]["selected"] = True
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(side_effect=[("focused", focused), ("selected", selected)]),
            ),
        ):
            latest, error = await relay.set_question_option(
                "w1:p1", interaction, 1, True
            )

        self.assertEqual(error, "")
        self.assertTrue(latest["options"][1]["selected"])
        self.assertEqual(
            [call.args for call in send_keys.await_args_list],
            [("w1:p1", ["Down"]), ("w1:p1", ["Enter"])],
        )

    async def test_question_other_text_avoids_unsupported_cursor_keys(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        focused = copy.deepcopy(interaction)
        focused["_focus"] = ("option", 4)
        edited = copy.deepcopy(focused)
        edited["other"] = {"selected": True, "text": "Back-port all four"}
        send_keys = AsyncMock(return_value=(True, ""))
        send_text = AsyncMock(return_value=(True, "", ""))
        with (
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(relay, "run_herdr_async_result", send_text),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(side_effect=[("focused", focused), ("edited", edited)]),
            ),
        ):
            latest, error = await relay.set_question_other_text(
                "w1:p1", interaction, "Back-port all four"
            )

        self.assertEqual(error, "")
        self.assertEqual(latest["other"]["text"], "Back-port all four")
        self.assertEqual(
            [call.args for call in send_keys.await_args_list],
            [("w1:p1", ["Down", "Down", "Down", "Down"]), ("w1:p1", ["Ctrl+U"])],
        )
        send_text.assert_awaited_once_with(
            "pane", "send-text", "w1:p1", "Back-port all four"
        )

    async def test_claude_normal_answer_clears_retained_other_text_first(self):
        interaction = relay.parse_claude_question(CLAUDE_FIRST_QUESTION_VIEW)
        interaction["other"] = {"selected": True, "text": "Hello"}
        cleared = copy.deepcopy(interaction)
        cleared["other"] = {"selected": False, "text": ""}
        set_other = AsyncMock(return_value=(cleared, ""))
        move_focus = AsyncMock(return_value=(cleared, ""))
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "set_question_other_text", set_other),
            patch.object(relay, "move_question_focus", move_focus),
            patch.object(relay, "send_question_keys", send_keys),
        ):
            ok, error = await relay.execute_question_answer(
                "w1:p1", interaction, [1], False, ""
            )

        self.assertTrue(ok, error)
        set_other.assert_awaited_once_with("w1:p1", interaction, "")
        move_focus.assert_awaited_once_with("w1:p1", cleared, ("option", 1))
        send_keys.assert_awaited_once_with("w1:p1", ["Enter"])

    async def test_multi_question_answer_rechecks_each_change_before_next(self):
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        after_first = copy.deepcopy(interaction)
        after_first["options"][0]["selected"] = False
        after_first["_focus"] = ("option", 0)
        after_second = copy.deepcopy(after_first)
        after_second["options"][1]["selected"] = True
        after_second["_focus"] = ("option", 1)
        after_other = copy.deepcopy(after_second)
        after_other["other"] = {"selected": True, "text": "Back-port all four"}
        after_other["_focus"] = ("option", 4)
        at_submit = copy.deepcopy(after_other)
        at_submit["_focus"] = ("submit", 0)

        set_option = AsyncMock(side_effect=[(after_first, ""), (after_second, "")])
        set_other = AsyncMock(return_value=(after_other, ""))
        move_focus = AsyncMock(return_value=(at_submit, ""))
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "set_question_option", set_option),
            patch.object(relay, "set_question_other_text", set_other),
            patch.object(relay, "move_question_focus", move_focus),
            patch.object(relay, "send_question_keys", send_keys),
        ):
            ok, error = await relay.execute_question_answer(
                "w1:p1", interaction, [1, 2], True, "Back-port all four"
            )

        self.assertTrue(ok, error)
        self.assertEqual(
            [call.args[2:] for call in set_option.await_args_list],
            [(0, False), (1, True)],
        )
        self.assertIs(set_option.await_args_list[1].args[1], after_first)
        set_other.assert_awaited_once_with(
            "w1:p1", after_second, "Back-port all four"
        )
        move_focus.assert_awaited_once_with(
            "w1:p1", after_other, ("submit", 0)
        )
        send_keys.assert_awaited_once_with("w1:p1", ["Enter"])

    async def test_question_key_delivery_stops_after_the_first_failure(self):
        command = AsyncMock(side_effect=[(True, "", ""), (False, "", "dropped")])
        with (
            patch.object(relay, "run_herdr_async_result", command),
            patch.object(relay.asyncio, "sleep", AsyncMock()) as sleep,
        ):
            ok, error = await relay.send_question_keys(
                "w1:p1", ["Down", "Down", "Enter"]
            )

        self.assertFalse(ok)
        self.assertEqual(error, "dropped")
        self.assertEqual(
            [call.args for call in command.await_args_list],
            [
                ("pane", "send-keys", "w1:p1", "Down"),
                ("pane", "send-keys", "w1:p1", "Down"),
            ],
        )
        sleep.assert_awaited_once_with(relay.QUESTION_KEY_DELAY)

    async def test_stale_question_is_rejected_without_terminal_input(self):
        ws = FakeWebSocket()
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        agent = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "read_current_question", AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction))),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as command,
            patch.object(relay, "publish_activity", AsyncMock()),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_answer_question_command(ws, {
                "type": "answer_question",
                "request_id": "question-1",
                "pane_id": "w1:p1",
                "interaction_id": "stale-question",
                "selected_indices": [0, 2],
                "other_selected": False,
                "other_text": "",
            })

        self.assertFalse(ws.messages[-1]["ok"])
        self.assertIn("question changed", ws.messages[-1]["error"])
        self.assertEqual(ws.messages[-1]["data"]["interaction"]["id"], interaction["id"])
        command.assert_not_awaited()

    async def test_live_question_is_accepted_when_agent_status_is_done(self):
        ws = FakeWebSocket()
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        agent = {
            "pane_id": "w1:p1",
            "status": "done",
            "agent": "claude",
            "project": "relay",
        }
        execute = AsyncMock(return_value=(True, ""))
        finish = AsyncMock()
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction)),
            ),
            patch.object(relay, "execute_question_answer", execute),
            patch.object(relay, "send_command_result", AsyncMock()),
            patch.object(relay, "finish_question_command", finish),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_answer_question_command(ws, {
                "type": "answer_question",
                "request_id": "question-live",
                "pane_id": "w1:p1",
                "interaction_id": interaction["id"],
                "selected_indices": [0, 2],
                "other_selected": False,
                "other_text": "",
            })

        execute.assert_awaited_once_with(
            "w1:p1", interaction, [0, 2], False, ""
        )
        finish.assert_awaited_once()

    async def test_live_question_chat_is_accepted_when_agent_status_is_done(self):
        ws = FakeWebSocket()
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        agent = {
            "pane_id": "w1:p1",
            "status": "done",
            "agent": "claude",
            "project": "relay",
        }
        send_keys = AsyncMock(return_value=(True, ""))
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "read_current_question",
                AsyncMock(return_value=(MULTI_QUESTION_VIEW, interaction)),
            ),
            patch.object(relay, "send_question_keys", send_keys),
            patch.object(
                relay,
                "wait_for_question_transition",
                AsyncMock(return_value=("confirmed", None, "working")),
            ),
            patch.object(relay, "publish_activity", AsyncMock()),
            patch.object(relay, "question_locks", {}),
        ):
            await relay.handle_clarify_question_command(ws, {
                "type": "clarify_question",
                "request_id": "question-chat-live",
                "pane_id": "w1:p1",
                "interaction_id": interaction["id"],
            })

        send_keys.assert_awaited_once()
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_answered_question_returns_the_next_chained_form(self):
        ws = FakeWebSocket()
        interaction = relay.parse_claude_question(MULTI_QUESTION_VIEW)
        next_interaction = {**interaction, "id": "next-question", "question": "Choose a release window"}
        agent = {
            "pane_id": "w1:p1",
            "status": "blocked",
            "agent": "claude",
            "project": "relay",
        }
        with (
            patch.object(
                relay,
                "wait_for_question_transition",
                AsyncMock(return_value=("advanced", next_interaction, "blocked")),
            ),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.finish_question_command(
                ws,
                {"request_id": "question-2", "source": "App"},
                agent,
                interaction,
            )

        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["phase"], "advanced")
        self.assertEqual(ws.messages[-1]["data"]["interaction"]["id"], "next-question")
        self.assertEqual(
            relay.blocked_agent_details["w1:p1"]["interaction"]["id"],
            "next-question",
        )

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
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(
                relay,
                "get_agent_inventory",
                side_effect=[(agent_snapshot(), None), (agent_snapshot(), None)],
            ),
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

    async def test_poll_retains_cached_agents_during_failure_then_recovers(self):
        cached = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "codex"},
        ])
        recovered = [{
            "pane_id": "w1:p1",
            "status": "idle",
            "agent": "codex",
            "_focused": False,
        }]
        waits = 0

        async def wait_for_next_poll(_agents):
            nonlocal waits
            waits += 1
            if waits == 1:
                self.assertEqual(relay.latest_agents_message, cached)
                self.assertEqual(relay.agent_inventory_status["state"], "error")
                return
            raise asyncio.CancelledError()

        inventory = {
            "state": "starting",
            "error_code": "",
            "message": "",
            "last_attempt_at": 0,
            "last_success_at": 0,
        }
        with (
            patch.object(relay, "agent_inventory_status", inventory),
            patch.object(relay, "get_agent_inventory", side_effect=[
                (None, ("protocol_mismatch", "Run live handoff")),
                (recovered, None),
            ]),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "wait_for_next_poll", side_effect=wait_for_next_poll),
            patch.object(relay, "broadcast_serialized", AsyncMock()) as broadcast,
            patch.object(relay, "publish_agent_status", AsyncMock()),
            patch.object(relay, "schedule_claude_history_capture"),
            patch.object(relay, "latest_agents_message", cached),
            patch.object(relay, "last_broadcast_agents_message", cached),
            patch.object(relay, "last_statuses", {"w1:p1": "working"}),
            patch.object(relay, "agent_types", {"w1:p1": "codex"}),
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

            self.assertEqual(relay.agent_inventory_status["state"], "ready")
            self.assertEqual(
                json.loads(relay.latest_agents_message)["agents"][0]["status"],
                "done",
            )

        self.assertEqual(
            [json.loads(call.args[0])["type"] for call in broadcast.await_args_list],
            ["inventory_status", "inventory_status", "agents"],
        )

    async def test_poll_discards_snapshot_invalidated_by_concurrent_event(self):
        wakeup = asyncio.Event()
        calls = []

        def fake_get_agents():
            calls.append(len(calls))
            if len(calls) == 1:
                # A plugin event lands while this refresh is in flight, so the
                # snapshot about to be returned already predates it.
                wakeup.set()
                return ([{"pane_id": "w1:p1", "status": "working", "agent": "codex", "_focused": False}], None)
            return ([{"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "_focused": False}], None)

        with (
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay, "get_agent_inventory", side_effect=fake_get_agents),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "wait_for_next_poll", AsyncMock(side_effect=asyncio.CancelledError())),
            patch.object(relay, "broadcast_serialized", AsyncMock()) as broadcast,
            patch.object(relay, "publish_agent_blocked", AsyncMock()) as publish_blocked,
            patch.object(relay, "schedule_claude_history_capture"),
            patch.multiple(
                relay,
                last_broadcast_agents_message=None,
                latest_agents_message=relay.agents_message([]),
                last_statuses={},
                agent_types={},
                blocked_agent_details={},
                agent_activity_state={},
                agent_activity_initialized=False,
                unseen_done_panes=set(),
                acknowledged_done_panes=set(),
                finished_notification_panes=set(),
                claude_history_state={},
                claude_history_capture_times={},
                claude_history_pending_captures=set(),
            ),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await relay.poll_loop()
            final_statuses = dict(relay.last_statuses)

        # The stale "working" snapshot was discarded and the loop re-polled at
        # once: only the fresh "blocked" snapshot was broadcast and published, so
        # one physical blocked cycle yields a single notification.
        self.assertEqual(len(calls), 2)
        self.assertEqual(final_statuses, {"w1:p1": "blocked"})
        broadcast.assert_awaited_once()
        publish_blocked.assert_awaited_once()

    async def test_plugin_event_completes_while_poll_inventory_read_is_blocked(self):
        pane_id = "w1:p1"
        queue = asyncio.Queue()
        wakeup = asyncio.Event()
        get_agents_started = asyncio.Event()
        release_get_agents = asyncio.Event()
        event_processed = asyncio.Event()
        activity_ids = []
        push_ids = []
        get_agents_calls = 0

        async def fake_to_thread(function, *args, **kwargs):
            nonlocal get_agents_calls
            if function is relay.get_agent_inventory:
                get_agents_calls += 1
                if get_agents_calls > 1:
                    raise asyncio.CancelledError()
                get_agents_started.set()
                await release_get_agents.wait()
                return ([{
                    "pane_id": pane_id,
                    "status": "working",
                    "agent": "codex",
                    "_focused": False,
                }], None)
            return function(*args, **kwargs)

        async def fake_broadcast_serialized(data):
            if json.loads(data).get("type") == "agent_update":
                event_processed.set()

        async def fake_publish_activity(kind, *_args, **kwargs):
            if kind == "blocked":
                activity_ids.append(kwargs["details"]["event_id"])
            return {"timestamp": 12345}

        async def fake_push_blocked(message):
            push_ids.append(message["event_id"])

        with (
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay.asyncio, "to_thread", side_effect=fake_to_thread),
            patch.object(relay, "broadcast_serialized", side_effect=fake_broadcast_serialized),
            patch.object(relay, "publish_activity", side_effect=fake_publish_activity),
            patch.object(relay, "push_blocked", side_effect=fake_push_blocked),
            patch.object(relay, "read_question_pane", return_value=""),
            patch.object(relay, "schedule_claude_history_capture"),
            patch.multiple(
                relay,
                last_broadcast_agents_message=None,
                latest_agents_message=relay.agents_message([
                    {"pane_id": pane_id, "status": "working", "agent": "codex"},
                ]),
                last_statuses={pane_id: "working"},
                agent_types={pane_id: "codex"},
                blocked_agent_details={},
                agent_activity_state={},
                agent_activity_initialized=False,
                unseen_done_panes=set(),
                acknowledged_done_panes=set(),
                finished_notification_panes=set(),
                claude_history_state={},
                claude_history_capture_times={},
                claude_history_pending_captures=set(),
            ),
        ):
            poll_task = asyncio.create_task(relay.poll_loop())
            event_task = asyncio.create_task(relay.event_push())
            try:
                await asyncio.wait_for(get_agents_started.wait(), timeout=1.5)
                relay.UDPPlugin().datagram_received(json.dumps({
                    "type": "agent_event",
                    "socket_path": relay.HERDR_SOCKET_PATH,
                    "pane_id": pane_id,
                    "status": "blocked",
                    "agent": "codex",
                }).encode(), None)

                # The plugin event must finish while the poll's worker thread is
                # still blocked, rather than waiting for its command timeouts.
                await asyncio.wait_for(event_processed.wait(), timeout=0.5)
                self.assertFalse(release_get_agents.is_set())
                await asyncio.sleep(0)
            finally:
                release_get_agents.set()
                event_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await event_task
                with self.assertRaises(asyncio.CancelledError):
                    await poll_task

        self.assertEqual(get_agents_calls, 2)
        self.assertEqual(len(activity_ids), 1)
        self.assertEqual(len(push_ids), 1)
        self.assertEqual(activity_ids, push_ids)

    async def test_working_udp_event_supersedes_stalled_poll_blocked_enrichment(self):
        pane_id = "w1:p1"
        queue = asyncio.Queue()
        wakeup = asyncio.Event()
        pane_read_started = threading.Event()
        release_pane_read = threading.Event()
        event_processed = asyncio.Event()
        activity_kinds = []
        pushed_blocked = []
        broadcasts = []

        def fake_get_agents():
            return ([{
                "pane_id": pane_id,
                "status": "blocked",
                "agent": "codex",
                "_focused": False,
            }], None)

        def stalled_question_read(_pane_id):
            pane_read_started.set()
            if not release_pane_read.wait(timeout=2):
                raise AssertionError("test did not release blocked enrichment")
            return "Allow command?"

        async def fake_publish_activity(kind, *_args, **_kwargs):
            activity_kinds.append(kind)
            return {"timestamp": 12345}

        async def fake_push_blocked(message):
            pushed_blocked.append(message)

        async def fake_broadcast(message):
            broadcasts.append(message)
            if message.get("type") == "agent_update" and message.get("status") == "working":
                event_processed.set()

        working_snapshot = relay.agents_message([
            {"pane_id": pane_id, "status": "working", "agent": "codex"},
        ])
        with (
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_revision_counter", 1),
            patch.object(relay, "agent_state_revisions", {pane_id: 1}),
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay, "get_agent_inventory", side_effect=fake_get_agents),
            patch.object(relay, "read_question_pane", side_effect=stalled_question_read),
            patch.object(relay, "publish_activity", side_effect=fake_publish_activity),
            patch.object(relay, "push_blocked", side_effect=fake_push_blocked),
            patch.object(relay, "broadcast", side_effect=fake_broadcast),
            patch.object(relay, "broadcast_serialized", AsyncMock()),
            patch.object(relay, "wait_for_next_poll", AsyncMock(side_effect=asyncio.CancelledError())),
            patch.object(relay, "schedule_claude_history_capture"),
            patch.multiple(
                relay,
                last_broadcast_agents_message=working_snapshot,
                latest_agents_message=working_snapshot,
                last_statuses={pane_id: "working"},
                agent_types={pane_id: "codex"},
                blocked_agent_details={},
                agent_activity_state={},
                agent_activity_initialized=False,
                unseen_done_panes=set(),
                acknowledged_done_panes=set(),
                finished_notification_panes=set(),
                claude_history_state={},
                claude_history_capture_times={},
                claude_history_pending_captures=set(),
            ),
        ):
            poll_task = asyncio.create_task(relay.poll_loop())
            event_task = asyncio.create_task(relay.event_push())
            try:
                started = await asyncio.wait_for(
                    asyncio.to_thread(pane_read_started.wait, 1), timeout=1.5
                )
                self.assertTrue(started)
                relay.UDPPlugin().datagram_received(json.dumps({
                    "type": "agent_event",
                    "socket_path": relay.HERDR_SOCKET_PATH,
                    "pane_id": pane_id,
                    "status": "working",
                    "agent": "codex",
                }).encode(), None)

                await asyncio.wait_for(event_processed.wait(), timeout=0.5)
                self.assertFalse(release_pane_read.is_set())
                self.assertEqual(relay.last_statuses[pane_id], "working")

                release_pane_read.set()
                with self.assertRaises(asyncio.CancelledError):
                    await asyncio.wait_for(poll_task, timeout=0.5)
                await asyncio.sleep(0)
            finally:
                release_pane_read.set()
                event_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await event_task
                if not poll_task.done():
                    poll_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await poll_task

        self.assertNotIn("blocked", activity_kinds)
        self.assertEqual(pushed_blocked, [])
        self.assertFalse(any(message.get("type") == "blocked" for message in broadcasts))

    async def test_working_udp_event_cancels_stalled_blocked_activity_publication(self):
        pane_id = "w1:p1"
        queue = asyncio.Queue()
        wakeup = asyncio.Event()
        blocked_activity_started = asyncio.Event()
        blocked_activity_cancelled = asyncio.Event()
        release_blocked_activity = asyncio.Event()
        event_processed = asyncio.Event()
        activities = []
        pushed_blocked = []
        broadcasts = []

        def fake_get_agents():
            return ([{
                "pane_id": pane_id,
                "status": "blocked",
                "agent": "codex",
                "_focused": False,
            }], None)

        async def stalled_publish_activity(kind, *_args, **_kwargs):
            if kind == "blocked":
                blocked_activity_started.set()
                try:
                    await release_blocked_activity.wait()
                except asyncio.CancelledError:
                    blocked_activity_cancelled.set()
                    raise
            activities.append(kind)
            return {"timestamp": 12345}

        async def fake_push_blocked(message):
            pushed_blocked.append(message)

        async def fake_broadcast(message):
            broadcasts.append(message)
            if message.get("type") == "agent_update" and message.get("status") == "working":
                event_processed.set()

        working_snapshot = relay.agents_message([
            {"pane_id": pane_id, "status": "working", "agent": "codex"},
        ])
        with (
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_revision_counter", 1),
            patch.object(relay, "agent_state_revisions", {pane_id: 1}),
            patch.object(relay, "agent_transition_publication_tasks", {}),
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay, "get_agent_inventory", side_effect=fake_get_agents),
            patch.object(relay, "read_question_pane", return_value="Allow command?"),
            patch.object(relay, "publish_activity", side_effect=stalled_publish_activity),
            patch.object(relay, "push_blocked", side_effect=fake_push_blocked),
            patch.object(relay, "broadcast", side_effect=fake_broadcast),
            patch.object(relay, "broadcast_serialized", AsyncMock()),
            patch.object(relay, "wait_for_next_poll", AsyncMock(side_effect=asyncio.CancelledError())),
            patch.object(relay, "schedule_claude_history_capture"),
            patch.multiple(
                relay,
                last_broadcast_agents_message=working_snapshot,
                latest_agents_message=working_snapshot,
                last_statuses={pane_id: "working"},
                agent_types={pane_id: "codex"},
                blocked_agent_details={},
                agent_activity_state={},
                agent_activity_initialized=False,
                unseen_done_panes=set(),
                acknowledged_done_panes=set(),
                finished_notification_panes=set(),
                claude_history_state={},
                claude_history_capture_times={},
                claude_history_pending_captures=set(),
            ),
        ):
            poll_task = asyncio.create_task(relay.poll_loop())
            event_task = asyncio.create_task(relay.event_push())
            try:
                await asyncio.wait_for(blocked_activity_started.wait(), timeout=0.5)
                relay.UDPPlugin().datagram_received(json.dumps({
                    "type": "agent_event",
                    "socket_path": relay.HERDR_SOCKET_PATH,
                    "pane_id": pane_id,
                    "status": "working",
                    "agent": "codex",
                }).encode(), None)

                await asyncio.wait_for(event_processed.wait(), timeout=0.5)
                await asyncio.wait_for(blocked_activity_cancelled.wait(), timeout=0.5)
                self.assertEqual(relay.last_statuses[pane_id], "working")

                release_blocked_activity.set()
                with self.assertRaises(asyncio.CancelledError):
                    await asyncio.wait_for(poll_task, timeout=0.5)
                await asyncio.sleep(0)
            finally:
                release_blocked_activity.set()
                event_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await event_task
                if not poll_task.done():
                    poll_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await poll_task

        self.assertNotIn("blocked", activities)
        self.assertIn("agent_status", activities)
        self.assertEqual(pushed_blocked, [])
        self.assertFalse(any(message.get("type") == "blocked" for message in broadcasts))

    async def test_reconnect_filters_tombstoned_activity_while_cleanup_is_paused(self):
        pane_id = "w1:p1"
        queue = asyncio.Queue()
        writer_started = threading.Event()
        release_writer = threading.Event()
        deletion_started = threading.Event()
        release_deletion = threading.Event()
        working_processed = asyncio.Event()
        history_reads = []
        original_record_activity = relay.record_activity
        original_discard_activity = relay.discard_recorded_activity
        original_load_activity = relay.load_activity

        def stalled_record_activity(kind, *args, **kwargs):
            if kind == "blocked":
                writer_started.set()
                if not release_writer.wait(timeout=2):
                    raise AssertionError("test did not release stale activity writer")
            return original_record_activity(kind, *args, **kwargs)

        def paused_discard_activity(activity_id):
            deletion_started.set()
            if not release_deletion.wait(timeout=2):
                raise AssertionError("test did not release stale activity deletion")
            return original_discard_activity(activity_id)

        def observed_load_activity(*args, **kwargs):
            activities = original_load_activity(*args, **kwargs)
            history_reads.append(list(activities))
            return activities

        async def fake_broadcast(message):
            if message.get("type") == "agent_update" and message.get("status") == "working":
                working_processed.set()

        blocked_snapshot = relay.agents_message([
            {"pane_id": pane_id, "status": "blocked", "agent": "codex"},
        ])
        with tempfile.TemporaryDirectory() as temp_dir:
            with ExitStack() as stack:
                stack.enter_context(patch.multiple(
                    relay,
                    ACTIVITY_FILE=Path(temp_dir) / "activity.jsonl",
                    activity_tombstones=set(),
                    activity_cleanup_tasks=set(),
                    activity_delivery_lock=asyncio.Lock(),
                    agent_state_application_lock=asyncio.Lock(),
                    agent_state_revision_counter=1,
                    agent_state_revisions={pane_id: 1},
                    agent_transition_publication_tasks={},
                    event_queue=queue,
                    poll_wakeup=asyncio.Event(),
                    clients=set(),
                    agent_refresh_clients=set(),
                    failed_clients=set(),
                    client_outbound_queues={},
                    client_outbound_tasks={},
                    client_handler_tasks={},
                    latest_agents_message=blocked_snapshot,
                    last_broadcast_agents_message=blocked_snapshot,
                    last_statuses={pane_id: "blocked"},
                    agent_types={pane_id: "codex"},
                    blocked_agent_details={},
                    unseen_done_panes=set(),
                    acknowledged_done_panes=set(),
                    finished_notification_panes=set(),
                ))
                for patcher in (
                    patch.object(relay, "record_activity", side_effect=stalled_record_activity),
                    patch.object(relay, "discard_recorded_activity", side_effect=paused_discard_activity),
                    patch.object(relay, "load_activity", side_effect=observed_load_activity),
                    patch.object(relay, "read_question_pane", return_value="Allow command?"),
                    patch.object(relay, "agent_cwd_for_pane", return_value=""),
                    patch.object(relay, "agent_session_for_pane", return_value=""),
                    patch.object(relay, "session_name_for_activity", return_value=""),
                    patch.object(relay, "broadcast", side_effect=fake_broadcast),
                    patch.object(relay, "push_blocked", AsyncMock()),
                    patch.object(relay, "schedule_claude_history_capture"),
                    patch.object(relay, "load_agent_profiles", return_value={}),
                    patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
                ):
                    stack.enter_context(patcher)
                event_task = asyncio.create_task(relay.event_push())
                blocked_task = asyncio.create_task(relay.publish_agent_blocked(
                    {"pane_id": pane_id, "status": "blocked", "agent": "codex"},
                    expected_revision=1,
                ))
                try:
                    started = await asyncio.wait_for(
                        asyncio.to_thread(writer_started.wait, 1), timeout=1.5
                    )
                    self.assertTrue(started)
                    relay.UDPPlugin().datagram_received(json.dumps({
                        "type": "agent_event",
                        "socket_path": relay.HERDR_SOCKET_PATH,
                        "pane_id": pane_id,
                        "status": "working",
                        "agent": "codex",
                    }).encode(), None)
                    await asyncio.wait_for(working_processed.wait(), timeout=0.5)
                    self.assertFalse(await asyncio.wait_for(blocked_task, timeout=0.5))
                    self.assertTrue(relay.activity_tombstones)
                    tombstoned_id = next(iter(relay.activity_tombstones))

                    release_writer.set()
                    deletion_ready = await asyncio.wait_for(
                        asyncio.to_thread(deletion_started.wait, 1), timeout=1.5
                    )
                    self.assertTrue(deletion_ready)

                    ws = FakeWebSocket()
                    await asyncio.wait_for(relay.handle_client(ws), timeout=0.5)
                    self.assertGreaterEqual(len(history_reads), 2)
                    for activities in history_reads[:2]:
                        self.assertNotIn(tombstoned_id, {
                            activity.get("id") for activity in activities
                        })
                    self.assertTrue(any(
                        activity.get("kind") == "agent_status"
                        for activity in history_reads[0]
                    ))
                finally:
                    release_writer.set()
                    release_deletion.set()
                    for _attempt in range(50):
                        if not relay.activity_cleanup_tasks:
                            break
                        await asyncio.sleep(0.01)
                    event_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await event_task
                    if not blocked_task.done():
                        blocked_task.cancel()
                        with self.assertRaises(asyncio.CancelledError):
                            await blocked_task

                self.assertEqual(relay.activity_tombstones, set())
                self.assertEqual(relay.activity_cleanup_tasks, set())

    async def test_revision_activity_does_not_write_without_durable_tombstone(self):
        pane_id = "w1:p1"
        record_activity = Mock()
        with (
            tempfile.TemporaryDirectory() as temp_dir,
            patch.object(relay, "ACTIVITY_FILE", Path(temp_dir) / "activity.jsonl"),
            patch.object(relay, "activity_tombstones", set()),
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_revisions", {pane_id: 1}),
            patch.object(relay, "last_statuses", {pane_id: "blocked"}),
            patch.object(relay, "record_activity", record_activity),
            patch.object(relay, "write_activity_tombstones_atomic", return_value=False),
            patch.object(relay, "broadcast", AsyncMock()),
        ):
            result = await relay.publish_activity(
                "blocked",
                "attention",
                "Agent needs approval",
                pane_id=pane_id,
                expected_revision=1,
                expected_status="blocked",
            )

        self.assertIsNone(result)
        record_activity.assert_not_called()

    async def test_restart_before_activity_commit_filters_write_ahead_id(self):
        pane_id = "w1:p1"
        clear_started = threading.Event()
        release_clear = threading.Event()
        original_clear_tombstone = relay.clear_activity_tombstone
        original_load_activity = relay.load_activity

        def paused_clear_tombstone(activity_id):
            clear_started.set()
            if not release_clear.wait(timeout=2):
                raise AssertionError("test did not release activity commit")
            return original_clear_tombstone(activity_id)

        with (
            tempfile.TemporaryDirectory() as temp_dir,
            patch.object(relay, "ACTIVITY_FILE", Path(temp_dir) / "activity.jsonl"),
            patch.object(relay, "activity_tombstones", set()),
            patch.object(relay, "activity_cleanup_tasks", set()),
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "agent_state_revisions", {pane_id: 1}),
            patch.object(relay, "last_statuses", {pane_id: "blocked"}),
            patch.object(relay, "agent_cwd_for_pane", return_value=""),
            patch.object(relay, "agent_session_for_pane", return_value=""),
            patch.object(relay, "session_name_for_activity", return_value=""),
            patch.object(relay, "clear_activity_tombstone", side_effect=paused_clear_tombstone),
            patch.object(relay, "broadcast", AsyncMock()),
        ):
            publication = asyncio.create_task(relay.publish_activity(
                "blocked",
                "attention",
                "Agent needs approval",
                pane_id=pane_id,
                expected_revision=1,
                expected_status="blocked",
            ))
            try:
                commit_ready = await asyncio.wait_for(
                    asyncio.to_thread(clear_started.wait, 1), timeout=1.5
                )
                self.assertTrue(commit_ready)
                written = [
                    json.loads(line)
                    for line in relay.ACTIVITY_FILE.read_text(encoding="utf-8").splitlines()
                ]
                self.assertEqual(len(written), 1)
                pending_id = written[0]["id"]

                relay.activity_tombstones.clear()
                self.assertEqual(
                    await asyncio.to_thread(original_load_activity),
                    [],
                )
                self.assertIn(pending_id, relay.persisted_activity_tombstones())
            finally:
                release_clear.set()

            committed = await asyncio.wait_for(publication, timeout=0.5)
            self.assertEqual(committed["id"], pending_id)
            relay.activity_tombstones.clear()
            self.assertEqual(
                [entry["id"] for entry in original_load_activity()],
                [pending_id],
            )

    async def test_plugin_blocked_event_during_poll_broadcast_is_applied_once(self):
        pane_id = "w1:p1"
        queue = asyncio.Queue()
        outbound = asyncio.Queue()
        wakeup = asyncio.Event()
        slow_send_started = asyncio.Event()
        release_slow_send = asyncio.Event()
        resume_poll = asyncio.Event()
        event_processed = asyncio.Event()
        delivery_complete = asyncio.Event()
        activity_ids = []
        push_ids = []
        delivered_types = []
        wait_calls = 0
        poll_snapshots = [
            [{"pane_id": pane_id, "status": "working", "agent": "codex", "_focused": False}],
            [{"pane_id": pane_id, "status": "blocked", "agent": "codex", "_focused": False}],
        ]

        class SlowWebSocket:
            async def send(self, data):
                delivered_types.append(json.loads(data)["type"])
                if len(delivered_types) >= 3:
                    delivery_complete.set()
                slow_send_started.set()
                await release_slow_send.wait()

        async def fake_publish_activity(kind, *_args, **kwargs):
            if kind == "blocked":
                activity_ids.append(kwargs["details"]["event_id"])
            return {"timestamp": 12345}

        async def fake_push_blocked(message):
            push_ids.append(message["event_id"])
            event_processed.set()

        def fake_get_agents():
            return poll_snapshots.pop(0), None

        async def fake_wait_for_next_poll(_agents):
            nonlocal wait_calls
            wait_calls += 1
            if wait_calls == 1:
                await resume_poll.wait()
                return
            raise asyncio.CancelledError()

        slow_client = SlowWebSocket()

        with (
            patch.object(relay, "agent_inventory_status", {
                "state": "ready", "error_code": "", "message": "",
                "last_attempt_at": 1, "last_success_at": 1,
            }),
            patch.object(relay, "agent_state_application_lock", asyncio.Lock()),
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "clients", {slow_client}),
            patch.object(relay, "poll_wakeup", wakeup),
            patch.object(relay, "get_agent_inventory", side_effect=fake_get_agents),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "wait_for_next_poll",
                side_effect=fake_wait_for_next_poll,
            ),
            patch.object(relay, "publish_activity", side_effect=fake_publish_activity),
            patch.object(relay, "push_blocked", side_effect=fake_push_blocked),
            patch.object(relay, "read_question_pane", return_value=""),
            patch.object(relay, "schedule_claude_history_capture"),
            patch.multiple(
                relay,
                last_broadcast_agents_message=None,
                latest_agents_message=relay.agents_message([]),
                last_statuses={},
                agent_types={},
                blocked_agent_details={},
                agent_activity_state={},
                agent_activity_initialized=False,
                unseen_done_panes=set(),
                acknowledged_done_panes=set(),
                finished_notification_panes=set(),
                claude_history_state={},
                claude_history_capture_times={},
                claude_history_pending_captures=set(),
            ),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            poll_task = asyncio.create_task(relay.poll_loop())
            event_task = asyncio.create_task(relay.event_push())
            try:
                await slow_send_started.wait()
                relay.UDPPlugin().datagram_received(json.dumps({
                    "type": "agent_event",
                    "socket_path": relay.HERDR_SOCKET_PATH,
                    "pane_id": pane_id,
                    "status": "blocked",
                    "agent": "codex",
                }).encode(), None)

                # Delivery of the poll snapshot is still stalled, but event
                # state, activity, and Web Push must already be complete.
                await asyncio.wait_for(event_processed.wait(), timeout=0.5)
                self.assertFalse(release_slow_send.is_set())
                self.assertEqual(relay.last_statuses, {pane_id: "blocked"})

                resume_poll.set()
                with self.assertRaises(asyncio.CancelledError):
                    await poll_task
                await asyncio.sleep(0)
            finally:
                release_slow_send.set()
                await asyncio.wait_for(outbound.join(), timeout=0.5)
                await asyncio.wait_for(delivery_complete.wait(), timeout=0.5)
                event_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await event_task
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task
                relay.stop_client_delivery(slow_client)
                if not poll_task.done():
                    poll_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await poll_task

            final_statuses = dict(relay.last_statuses)

        self.assertEqual(final_statuses, {pane_id: "blocked"})
        self.assertEqual(len(activity_ids), 1)
        self.assertEqual(len(push_ids), 1)
        self.assertEqual(activity_ids, push_ids)
        self.assertEqual(delivered_types[:3], ["agents", "blocked", "agent_update"])

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

    async def test_outbound_delivery_times_out_and_evicts_slow_client(self):
        outbound = asyncio.Queue()
        send_cancelled = asyncio.Event()
        close_started = asyncio.Event()

        class SlowWebSocket:
            async def send(self, _data):
                try:
                    await asyncio.Event().wait()
                finally:
                    send_cancelled.set()

            async def close(self):
                close_started.set()
                await asyncio.Event().wait()

        slow_client = SlowWebSocket()
        with (
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "clients", {slow_client}),
            patch.object(relay, "agent_refresh_clients", {slow_client}),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "WS_SEND_TIMEOUT", 0.01),
            patch.object(relay, "WS_CLOSE_TIMEOUT", 0.01),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            try:
                await relay.broadcast_serialized('{"type":"test"}')
                await asyncio.wait_for(outbound.join(), timeout=0.2)
                await asyncio.wait_for(close_started.wait(), timeout=0.2)
                await asyncio.sleep(0.02)
            finally:
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

            self.assertTrue(send_cancelled.is_set())
            self.assertNotIn(slow_client, relay.clients)
            self.assertNotIn(slow_client, relay.agent_refresh_clients)

    async def test_stalled_close_cancels_handler_and_clears_client_registries(self):
        outbound = asyncio.Queue()
        receive_started = asyncio.Event()
        stalled_send_started = asyncio.Event()
        close_started = asyncio.Event()
        transport_aborted = asyncio.Event()

        class StalledTransport:
            def abort(self):
                transport_aborted.set()

        class StalledWebSocket(FakeWebSocket):
            def __init__(self):
                super().__init__()
                self.transport = StalledTransport()
                self.send_count = 0

            async def send(self, payload):
                self.send_count += 1
                if self.send_count <= 3:
                    await super().send(payload)
                    return
                stalled_send_started.set()
                await asyncio.Event().wait()

            async def close(self):
                close_started.set()
                await asyncio.Event().wait()

            async def __anext__(self):
                receive_started.set()
                await asyncio.Event().wait()

        ws = StalledWebSocket()
        connected_clients = set()
        refresh_clients = set()
        failed_clients = set()
        delivery_queues = {}
        delivery_tasks = {}
        handler_tasks = {}
        with (
            patch.object(relay, "clients", connected_clients),
            patch.object(relay, "agent_refresh_clients", refresh_clients),
            patch.object(relay, "failed_clients", failed_clients),
            patch.object(relay, "client_outbound_queues", delivery_queues),
            patch.object(relay, "client_outbound_tasks", delivery_tasks),
            patch.object(relay, "client_handler_tasks", handler_tasks),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", return_value=[]),
            patch.object(relay, "WS_SEND_TIMEOUT", 0.01),
            patch.object(relay, "WS_CLOSE_TIMEOUT", 0.01),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            handler_task = asyncio.create_task(relay.handle_client(ws))
            try:
                await asyncio.wait_for(receive_started.wait(), timeout=0.2)
                self.assertIs(handler_tasks.get(ws), handler_task)

                await relay.broadcast_serialized('{"type":"test"}')
                await asyncio.wait_for(outbound.join(), timeout=0.2)
                await asyncio.wait_for(stalled_send_started.wait(), timeout=0.2)
                await asyncio.wait_for(close_started.wait(), timeout=0.2)
                with self.assertRaises(asyncio.CancelledError):
                    await asyncio.wait_for(handler_task, timeout=0.2)
                await asyncio.wait_for(transport_aborted.wait(), timeout=0.2)

                self.assertNotIn(ws, connected_clients)
                self.assertNotIn(ws, refresh_clients)
                self.assertNotIn(ws, failed_clients)
                self.assertNotIn(ws, delivery_queues)
                self.assertNotIn(ws, delivery_tasks)
                self.assertNotIn(ws, handler_tasks)
            finally:
                if not handler_task.done():
                    handler_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await handler_task
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

    async def test_slow_client_does_not_delay_healthy_client_next_message(self):
        outbound = asyncio.Queue()
        slow_started = asyncio.Event()
        release_slow = asyncio.Event()
        healthy_second = asyncio.Event()

        class SlowWebSocket:
            async def send(self, _data):
                slow_started.set()
                await release_slow.wait()

        class HealthyWebSocket:
            def __init__(self):
                self.messages = []

            async def send(self, data):
                self.messages.append(json.loads(data)["sequence"])
                if len(self.messages) == 2:
                    healthy_second.set()

        slow_client = SlowWebSocket()
        healthy_client = HealthyWebSocket()
        with (
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "clients", {slow_client, healthy_client}),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "WS_SEND_TIMEOUT", 1.0),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            try:
                await relay.broadcast_serialized('{"sequence":1}')
                await asyncio.wait_for(slow_started.wait(), timeout=0.2)
                await relay.broadcast_serialized('{"sequence":2}')

                await asyncio.wait_for(healthy_second.wait(), timeout=0.2)
                self.assertFalse(release_slow.is_set())
                self.assertEqual(healthy_client.messages, [1, 2])
            finally:
                release_slow.set()
                relay.stop_client_delivery(slow_client)
                relay.stop_client_delivery(healthy_client)
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

    async def test_client_backlog_coalesces_agents_then_evicts_on_overflow(self):
        outbound = asyncio.Queue()
        slow_started = asyncio.Event()
        release_slow = asyncio.Event()
        close_finished = asyncio.Event()

        class SlowWebSocket:
            async def send(self, _data):
                slow_started.set()
                await release_slow.wait()

            async def close(self):
                close_finished.set()

        slow_client = SlowWebSocket()
        with (
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "clients", {slow_client}),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "CLIENT_OUTBOUND_MAX_ITEMS", 2),
            patch.object(relay, "CLIENT_OUTBOUND_MAX_BYTES", 4096),
            patch.object(relay, "WS_SEND_TIMEOUT", 1.0),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            try:
                await relay.broadcast_serialized('{"type":"activity","sequence":0}')
                await asyncio.wait_for(slow_started.wait(), timeout=0.2)

                for sequence in (1, 2, 3):
                    await relay.broadcast_serialized(
                        json.dumps({"type": "agents", "sequence": sequence})
                    )
                await outbound.join()
                buffer = relay.client_outbound_queues[slow_client]
                self.assertEqual(buffer.qsize(), 1)
                self.assertEqual(json.loads(buffer.items[-1][0])["sequence"], 3)
                self.assertNotIn(slow_client, relay.failed_clients)

                await relay.broadcast_serialized('{"type":"activity","sequence":4}')
                await relay.broadcast_serialized('{"type":"activity","sequence":5}')
                await asyncio.wait_for(close_finished.wait(), timeout=0.2)
                self.assertIn(slow_client, relay.failed_clients)
                self.assertNotIn(slow_client, relay.clients)
            finally:
                release_slow.set()
                relay.stop_client_delivery(slow_client)
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

    async def test_timed_out_client_cannot_dispatch_agent_stop(self):
        outbound = asyncio.Queue()
        registered = asyncio.Event()

        class TrackingClients(set):
            def add(self, item):
                super().add(item)
                registered.set()

        class CommandWebSocket:
            def __init__(self):
                self.incoming = asyncio.Queue()
                self.stall_sends = False
                self.closed = False
                self.close_finished = asyncio.Event()

            async def send(self, _data):
                if self.stall_sends:
                    await asyncio.Event().wait()

            async def close(self):
                self.closed = True
                self.close_finished.set()

            def __aiter__(self):
                return self

            async def __anext__(self):
                return await self.incoming.get()

        ws = CommandWebSocket()
        tracked_clients = TrackingClients()
        with (
            patch.object(relay, "clients", tracked_clients),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", return_value=[]),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "WS_SEND_TIMEOUT", 0.01),
            patch.object(relay, "handle_agent_stop_command", AsyncMock()) as stop,
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            handler_task = asyncio.create_task(relay.handle_client(ws))
            try:
                await asyncio.wait_for(registered.wait(), timeout=0.2)
                ws.stall_sends = True
                await relay.broadcast_serialized('{"type":"agents","agents":[]}')
                await asyncio.wait_for(outbound.join(), timeout=0.2)
                await asyncio.wait_for(ws.close_finished.wait(), timeout=0.2)
                self.assertTrue(ws.closed)

                await ws.incoming.put(json.dumps({
                    "type": "agent_stop",
                    "pane_id": "w1:p1",
                    "protocol": relay.PROTOCOL_VERSION,
                }))
                await asyncio.wait_for(handler_task, timeout=0.2)
            finally:
                if not handler_task.done():
                    handler_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await handler_task
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

        stop.assert_not_awaited()
        self.assertNotIn(ws, tracked_clients)

    async def test_failed_startup_sends_never_register_client(self):
        class TrackingClients(set):
            def __init__(self):
                super().__init__()
                self.added = []

            def add(self, item):
                self.added.append(item)
                super().add(item)

        class StartupWebSocket(FakeWebSocket):
            def __init__(self, fail_at):
                super().__init__()
                self.fail_at = fail_at
                self.send_count = 0
                self.closed = False

            async def send(self, payload):
                self.send_count += 1
                if self.send_count == self.fail_at:
                    raise OSError("send failed")
                await super().send(payload)

            async def close(self):
                self.closed = True

        for fail_at in (1, 2):
            with self.subTest(fail_at=fail_at):
                ws = StartupWebSocket(fail_at)
                tracked_clients = TrackingClients()
                with (
                    patch.object(relay, "clients", tracked_clients),
                    patch.object(relay, "agent_refresh_clients", set()),
                    patch.object(relay, "failed_clients", set()),
                    patch.object(relay, "latest_agents_message", relay.agents_message([])),
                    patch.object(relay, "load_agent_profiles", return_value={}),
                    patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
                    patch.object(relay, "load_activity", return_value=[]),
                    patch.object(
                        relay.asyncio,
                        "to_thread",
                        AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
                    ),
                ):
                    await relay.handle_client(ws)
                    await asyncio.sleep(0)

                self.assertEqual(tracked_clients.added, [])
                self.assertNotIn(ws, tracked_clients)
                self.assertTrue(ws.closed)

    async def test_client_catches_up_state_changed_during_activity_history_send(self):
        outbound = asyncio.Queue()
        activity_send_started = asyncio.Event()
        release_activity_send = asyncio.Event()
        catchup_complete = asyncio.Event()
        catchup = {"agents": False, "activities": False}
        activity_entry = {"id": "activity-2", "kind": "blocked"}
        load_activity_calls = 0

        def fake_load_activity():
            nonlocal load_activity_calls
            load_activity_calls += 1
            return [] if load_activity_calls == 1 else [activity_entry]

        class HandshakeWebSocket(FakeWebSocket):
            async def send(self, payload):
                message = json.loads(payload)
                self.messages.append(message)
                if message.get("type") == "activity_history" and not activity_send_started.is_set():
                    activity_send_started.set()
                    await release_activity_send.wait()
                if (
                    message.get("type") == "agents"
                    and message.get("agents")
                    and message["agents"][0].get("status") == "blocked"
                ):
                    catchup["agents"] = True
                if message.get("type") == "activity_history" and message.get("activities"):
                    catchup["activities"] = True
                if all(catchup.values()):
                    catchup_complete.set()

            async def __anext__(self):
                await catchup_complete.wait()
                raise StopAsyncIteration

        ws = HandshakeWebSocket()
        working = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "codex"},
        ])
        inventory = {
            "state": "error",
            "error_code": "protocol_mismatch",
            "message": "Run live handoff",
            "last_attempt_at": 1,
            "last_success_at": 0,
        }
        with (
            patch.object(relay, "agent_inventory_status", inventory),
            patch.object(relay, "clients", set()),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "latest_agents_message", working),
            patch.object(relay, "last_broadcast_agents_message", working),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", side_effect=fake_load_activity),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            handler_task = asyncio.create_task(relay.handle_client(ws))
            try:
                await asyncio.wait_for(activity_send_started.wait(), timeout=0.2)
                relay.set_agent_inventory_ready()
                changed = await relay.broadcast_agents_if_changed([{
                    "pane_id": "w1:p1",
                    "status": "blocked",
                    "agent": "codex",
                }])
                self.assertTrue(changed)

                release_activity_send.set()
                await asyncio.wait_for(catchup_complete.wait(), timeout=0.2)
                await asyncio.wait_for(handler_task, timeout=0.2)
            finally:
                release_activity_send.set()
                if not handler_task.done():
                    handler_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await handler_task
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

        snapshots = [
            message["agents"][0]["status"]
            for message in ws.messages
            if message.get("type") == "agents" and message.get("agents")
        ]
        self.assertEqual(snapshots, ["working", "blocked"])
        ready_index = next(
            index
            for index, message in enumerate(ws.messages)
            if message.get("type") == "inventory_status" and message.get("state") == "ready"
        )
        blocked_index = next(
            index
            for index, message in enumerate(ws.messages)
            if message.get("type") == "agents"
            and message.get("agents")
            and message["agents"][0].get("status") == "blocked"
        )
        self.assertLess(ready_index, blocked_index)
        activity_snapshots = [
            message["activities"]
            for message in ws.messages
            if message.get("type") == "activity_history"
        ]
        self.assertEqual(activity_snapshots, [[], [activity_entry]])

    async def test_maximum_activity_history_fits_actual_catchup_queue(self):
        entry_count = 250
        activities = [
            {"id": str(index), "extract": ""}
            for index in range(entry_count)
        ]
        jsonl_size = sum(
            len((json.dumps(entry, separators=(",", ":")) + "\n").encode("utf-8"))
            for entry in activities
        )
        padding, remainder = divmod(relay.ACTIVITY_MAX_BYTES - jsonl_size, entry_count)
        for index, entry in enumerate(activities):
            entry["extract"] = "x" * (padding + (index < remainder))
        self.assertEqual(
            sum(
                len((json.dumps(entry, separators=(",", ":")) + "\n").encode("utf-8"))
                for entry in activities
            ),
            relay.ACTIVITY_MAX_BYTES,
        )
        catchup_payload = json.dumps({
            "type": "activity_history",
            "activities": activities,
        }, separators=(",", ":"))
        self.assertGreater(len(catchup_payload.encode("utf-8")), relay.ACTIVITY_MAX_BYTES)
        self.assertLessEqual(
            len(catchup_payload.encode("utf-8")),
            relay.CLIENT_OUTBOUND_MAX_BYTES,
        )

        outbound = asyncio.Queue()
        catchup_delivered = asyncio.Event()
        release_handler = asyncio.Event()
        load_activity_calls = 0

        def fake_load_activity():
            nonlocal load_activity_calls
            load_activity_calls += 1
            return [] if load_activity_calls == 1 else activities

        class HandshakeWebSocket(FakeWebSocket):
            async def send(self, payload):
                message = json.loads(payload)
                self.messages.append(message)
                if message.get("type") == "activity_history" and message.get("activities"):
                    catchup_delivered.set()

            async def __anext__(self):
                await release_handler.wait()
                raise StopAsyncIteration

        ws = HandshakeWebSocket()
        connected_clients = set()
        failed_clients = set()
        with (
            patch.object(relay, "clients", connected_clients),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", failed_clients),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", side_effect=fake_load_activity),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            handler_task = asyncio.create_task(relay.handle_client(ws))
            try:
                await asyncio.wait_for(catchup_delivered.wait(), timeout=1.0)
                self.assertIn(ws, connected_clients)
                self.assertNotIn(ws, failed_clients)
                self.assertEqual(ws.messages[-1]["activities"], activities)
            finally:
                release_handler.set()
                await asyncio.wait_for(handler_task, timeout=0.2)
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

    async def test_activity_catchup_is_enqueued_before_concurrent_live_activity(self):
        outbound = asyncio.Queue()
        initial_history_started = asyncio.Event()
        release_initial_history = asyncio.Event()
        catchup_read_started = asyncio.Event()
        release_catchup_read = asyncio.Event()
        delivery_complete = asyncio.Event()
        activity_a = {"id": "activity-a", "kind": "prompt"}
        activity_b = {
            "id": "activity-b",
            "kind": "blocked",
            "pane_id": "w1:p1",
            "timestamp": 2,
        }
        load_activity_calls = 0

        async def fake_to_thread(function, *args, **kwargs):
            nonlocal load_activity_calls
            if function is relay.load_activity:
                load_activity_calls += 1
                if load_activity_calls == 1:
                    return []
                catchup_read_started.set()
                await release_catchup_read.wait()
                return [activity_a]
            if function is relay.record_activity:
                return activity_b
            return function(*args, **kwargs)

        class HandshakeWebSocket(FakeWebSocket):
            async def send(self, payload):
                message = json.loads(payload)
                self.messages.append(message)
                if message.get("type") == "activity_history" and not initial_history_started.is_set():
                    initial_history_started.set()
                    await release_initial_history.wait()
                has_catchup = any(
                    item.get("id") == "activity-a"
                    for item in message.get("activities", [])
                )
                if has_catchup:
                    self.saw_catchup = True
                if message.get("type") == "activity" and message.get("activity", {}).get("id") == "activity-b":
                    self.saw_live = True
                if getattr(self, "saw_catchup", False) and getattr(self, "saw_live", False):
                    delivery_complete.set()

            async def __anext__(self):
                await delivery_complete.wait()
                raise StopAsyncIteration

        ws = HandshakeWebSocket()
        with (
            patch.object(relay, "clients", set()),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay.asyncio, "to_thread", side_effect=fake_to_thread),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            handler_task = asyncio.create_task(relay.handle_client(ws))
            live_activity_task = None
            try:
                await asyncio.wait_for(initial_history_started.wait(), timeout=0.2)
                release_initial_history.set()
                await asyncio.wait_for(catchup_read_started.wait(), timeout=0.2)

                live_activity_task = asyncio.create_task(relay.publish_activity(
                    "blocked", "attention", "Agent blocked", pane_id="w1:p1"
                ))
                await asyncio.sleep(0)
                self.assertFalse(live_activity_task.done())

                release_catchup_read.set()
                await asyncio.wait_for(delivery_complete.wait(), timeout=0.2)
                await asyncio.wait_for(live_activity_task, timeout=0.2)
                await asyncio.wait_for(handler_task, timeout=0.2)
            finally:
                release_initial_history.set()
                release_catchup_read.set()
                if live_activity_task is not None and not live_activity_task.done():
                    live_activity_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await live_activity_task
                if not handler_task.done():
                    handler_task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await handler_task
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

        activity_wire = [
            (message["type"], message.get("activities") or message.get("activity"))
            for message in ws.messages
            if message.get("type") in {"activity_history", "activity"}
        ]
        catchup_index = activity_wire.index(("activity_history", [activity_a]))
        live_index = activity_wire.index(("activity", activity_b))
        self.assertLess(catchup_index, live_index)

    async def test_requested_refresh_sends_unchanged_snapshot_to_requesting_client(self):
        delivered = asyncio.Event()

        class RefreshWebSocket(FakeWebSocket):
            async def send(self, payload):
                await super().send(payload)
                delivered.set()

        ws = RefreshWebSocket()
        outbound = asyncio.Queue()
        snapshot = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "codex"},
        ])
        with (
            patch.object(relay, "clients", {ws}),
            patch.object(relay, "agent_refresh_clients", {ws}),
            patch.object(relay, "outbound_queue", outbound),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "latest_agents_message", snapshot),
        ):
            outbound_task = asyncio.create_task(relay.outbound_push())
            try:
                await relay.send_requested_agent_refreshes()
                await outbound.join()
                await asyncio.wait_for(delivered.wait(), timeout=0.2)
            finally:
                relay.stop_client_delivery(ws)
                outbound_task.cancel()
                with self.assertRaises(asyncio.CancelledError):
                    await outbound_task

            self.assertEqual(relay.agent_refresh_clients, set())

        self.assertEqual(
            [message["type"] for message in ws.messages],
            ["inventory_status", "agents"],
        )
        self.assertEqual(ws.messages[1], json.loads(snapshot))

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
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
        ):
            await relay.handle_client(ws)

        self.assertEqual(
            [message["type"] for message in ws.messages],
            ["push_config", "agents", "activity_history"],
        )
        self.assertEqual(ws.messages[1]["agents"][0]["pane_id"], "w1:p1")
        self.assertNotIn(ws, relay.clients)

    async def test_client_refresh_replies_from_cache_and_wakes_poll(self):
        class RefreshClients(set):
            def __init__(self):
                super().__init__()
                self.added = []

            def add(self, item):
                self.added.append(item)
                super().add(item)

        class RefreshWebSocket(FakeWebSocket):
            def __init__(self):
                super().__init__()
                self.incoming = iter([json.dumps({"type": "refresh_agents"})])

            async def __anext__(self):
                try:
                    return next(self.incoming)
                except StopIteration:
                    raise StopAsyncIteration from None

        ws = RefreshWebSocket()
        cached = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "codex"},
        ])
        refresh_clients = RefreshClients()
        with (
            patch.object(relay, "latest_agents_message", cached),
            patch.object(relay, "load_agent_profiles", return_value={}),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", return_value=[]),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "agent_refresh_clients", refresh_clients),
            patch.object(relay, "wake_poll_loop") as wake,
        ):
            await relay.handle_client(ws)

        wake.assert_called_once_with()
        self.assertEqual(
            [message["type"] for message in ws.messages],
            [
                "push_config",
                "agents",
                "activity_history",
                "inventory_status",
                "agents",
            ],
        )
        self.assertEqual(ws.messages[-1]["agents"][0]["pane_id"], "w1:p1")
        self.assertEqual(refresh_clients.added, [ws])
        self.assertEqual(refresh_clients, set())
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
        payload = {"pane_id": "w1:p1", "socket_path": relay.HERDR_SOCKET_PATH}
        with (
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "wake_poll_loop") as wake,
        ):
            relay.UDPPlugin().datagram_received(json.dumps(payload).encode(), None)

        queue.put_nowait.assert_called_once_with(payload)
        wake.assert_called_once_with()

    async def test_udp_plugin_ignores_other_herdr_sessions(self):
        queue = Mock()
        with (
            patch.object(relay, "event_queue", queue),
            patch.object(relay, "wake_poll_loop") as wake,
        ):
            relay.UDPPlugin().datagram_received(
                b'{"pane_id":"w1:p1","socket_path":"/tmp/other-herdr.sock"}', None
            )

        queue.put_nowait.assert_not_called()
        wake.assert_not_called()

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

    async def test_viewing_working_pane_does_not_broadcast_false_idle(self):
        pane = "w1:p1"
        relay.agent_types[pane] = "codex"
        relay.last_statuses[pane] = "working"
        relay.unseen_done_panes.discard(pane)
        relay.acknowledged_done_panes.discard(pane)
        try:
            with (
                patch.object(relay, "broadcast", AsyncMock()) as broadcast,
                patch.object(relay, "wake_poll_loop") as wake,
            ):
                acknowledged = await relay.acknowledge_pane_viewed(pane)
        finally:
            relay.agent_types.pop(pane, None)
            relay.last_statuses.pop(pane, None)
            relay.unseen_done_panes.discard(pane)
            relay.acknowledged_done_panes.discard(pane)

        self.assertFalse(acknowledged)
        broadcast.assert_not_awaited()
        wake.assert_not_called()

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

    async def test_claude_history_capture_skips_mutable_question_viewport(self):
        relay.agent_types["w1:p1"] = "claude"
        try:
            with (
                patch.object(relay, "run_herdr_async", AsyncMock(return_value=MULTI_QUESTION_VIEW)),
                patch.object(relay, "merge_claude_history") as merge,
            ):
                await relay.capture_claude_history("w1:p1")
        finally:
            relay.agent_types.clear()
            relay.claude_history_inflight.clear()

        merge.assert_not_called()

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

    async def test_http_static_assets_have_exact_mime_and_security_headers(self):
        expected_types = {
            "/": "text/html; charset=utf-8",
            "/assets/app.js?v=8": "text/javascript; charset=utf-8",
            "/assets/app.css?v=8": "text/css; charset=utf-8",
            "/notification-icons.js": "text/javascript; charset=utf-8",
            "/version.json": "application/json; charset=utf-8",
            "/manifest.webmanifest": "application/manifest+json; charset=utf-8",
            "/icons/icon.svg": "image/svg+xml",
            "/icons/icon-192.png": "image/png",
        }

        for path, content_type in expected_types.items():
            with self.subTest(path=path):
                response = await relay.process_request(None, FakeRequest(path))
                self.assertEqual(response.status_code, 200)
                self.assertEqual(response.headers["Content-Type"], content_type)
                self.assertEqual(response.headers["Cache-Control"], "no-cache")
                self.assertRegex(response.headers["ETag"], r'^"[0-9a-f]{64}"$')
                self.assertEqual(response.headers["Vary"], "Accept-Encoding")
                self.assertEqual(response.headers["X-Content-Type-Options"], "nosniff")

        for path in ("/_headers", "/assets/unknown.css", "/icons", "/missing.txt"):
            with self.subTest(path=path):
                response = await relay.process_request(None, FakeRequest(path))
                self.assertEqual(response.status_code, 404)

    async def test_http_static_assets_return_304_for_matching_etags(self):
        initial = await relay.process_request(
            None, FakeRequest("/assets/app.js?v=35")
        )
        etag = initial.headers["ETag"]
        same_asset = await relay.process_request(
            None,
            FakeRequest(
                "/assets/app.js?v=999",
                [("If-None-Match", f'"other", W/{etag}')],
            ),
        )
        changed = await relay.process_request(
            None,
            FakeRequest(
                "/assets/app.js?v=35",
                [("If-None-Match", '"different"')],
            ),
        )

        self.assertEqual(initial.status_code, 200)
        self.assertTrue(initial.body)
        self.assertEqual(same_asset.status_code, 304)
        self.assertEqual(same_asset.body, b"")
        self.assertEqual(same_asset.headers["ETag"], etag)
        self.assertEqual(same_asset.headers["Cache-Control"], "no-cache")
        self.assertEqual(same_asset.headers["Vary"], "Accept-Encoding")
        self.assertEqual(same_asset.headers["X-Content-Type-Options"], "nosniff")
        self.assertEqual(changed.status_code, 200)
        self.assertEqual(changed.body, initial.body)

    async def test_http_static_assets_negotiate_brotli_with_distinct_etags(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            web_dir = Path(temp_dir) / "web"
            assets_dir = web_dir / "assets"
            assets_dir.mkdir(parents=True)
            source = b"const repeated = 'mobile relay';\n" * 20
            compressed = b"deterministic-brotli-representation"
            (assets_dir / "app.js").write_bytes(source)
            (assets_dir / "app.js.br").write_bytes(compressed)

            with patch.object(relay, "WEB_DIR", web_dir):
                identity = await relay.process_request(
                    None, FakeRequest("/assets/app.js", [("Accept-Encoding", "gzip")])
                )
                brotli = await relay.process_request(
                    None, FakeRequest("/assets/app.js", [("Accept-Encoding", "gzip, br")])
                )
                disabled = await relay.process_request(
                    None, FakeRequest("/assets/app.js", [("Accept-Encoding", "*, br;q=0")])
                )
                unchanged = await relay.process_request(
                    None,
                    FakeRequest(
                        "/assets/app.js",
                        [("Accept-Encoding", "br"), ("If-None-Match", brotli.headers["ETag"])],
                    ),
                )

        self.assertEqual(identity.body, source)
        self.assertIsNone(identity.headers.get("Content-Encoding"))
        self.assertEqual(brotli.body, compressed)
        self.assertEqual(brotli.headers["Content-Encoding"], "br")
        self.assertEqual(brotli.headers["Vary"], "Accept-Encoding")
        self.assertNotEqual(identity.headers["ETag"], brotli.headers["ETag"])
        self.assertEqual(disabled.body, source)
        self.assertIsNone(disabled.headers.get("Content-Encoding"))
        self.assertEqual(unchanged.status_code, 304)
        self.assertEqual(unchanged.body, b"")
        self.assertEqual(unchanged.headers["Content-Encoding"], "br")

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
        self.assertEqual(payload["release_version"], PLUGIN_VERSION)
        self.assertEqual(payload["revision"], relay.RELAY_VERSION)
        self.assertIn(payload["readiness"], {"starting", "ready", "degraded"})
        self.assertIn("inventory", payload)

    async def test_health_keeps_identity_valid_while_readiness_reports_inventory_failure(self):
        inventory = {
            "state": "error",
            "error_code": "protocol_mismatch",
            "message": "secret local command output",
            "last_attempt_at": 123,
            "last_success_at": 0,
        }
        with patch.object(relay, "agent_inventory_status", inventory):
            healthz = await relay.process_request(None, FakeRequest("/healthz"))
            readyz = await relay.process_request(None, FakeRequest("/readyz"))

        health_payload = json.loads(healthz.body)
        self.assertEqual(healthz.status_code, 200)
        self.assertEqual(health_payload["status"], "ok")
        self.assertEqual(health_payload["readiness"], "degraded")
        self.assertEqual(health_payload["inventory"]["error_code"], "protocol_mismatch")
        self.assertNotIn("message", health_payload["inventory"])
        self.assertNotIn("secret local command output", healthz.body.decode())
        self.assertEqual(readyz.status_code, 503)
        self.assertEqual(json.loads(readyz.body)["status"], "unavailable")

        inventory["state"] = "ready"
        inventory["error_code"] = ""
        with patch.object(relay, "agent_inventory_status", inventory):
            readyz = await relay.process_request(None, FakeRequest("/readyz"))
        self.assertEqual(readyz.status_code, 200)

    async def test_update_check_returns_the_checked_status(self):
        ws = FakeWebSocket()
        status = {
            "state": "available",
            "current_version": PLUGIN_VERSION,
            "current_revision": "abc123",
            "available_version": "9.0.0",
            "available_revision": "f" * 12,
            "target_revision": "f" * 40,
            "checked_at": 123,
            "can_install": True,
            "mode": "local",
            "reason": "",
            "error": "",
        }
        with patch.object(relay, "refresh_update_status", AsyncMock(return_value=status)):
            await relay.handle_check_update_command(ws, {
                "type": "check_update",
                "request_id": "update-check-1",
            })

        self.assertEqual(ws.messages[-1]["type"], "command_result")
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["data"]["update"]["target_revision"], "f" * 40)

    async def test_update_install_requires_the_exact_advertised_release(self):
        ws = FakeWebSocket()
        status = {
            "state": "available",
            "current_version": PLUGIN_VERSION,
            "current_revision": "abc123",
            "available_version": "9.0.0",
            "available_revision": "f" * 12,
            "target_revision": "f" * 40,
            "checked_at": 123,
            "can_install": True,
            "mode": "local",
            "reason": "",
            "error": "",
        }
        with patch.object(relay, "UPDATE_STATUS", status):
            await relay.handle_install_update_command(ws, {
                "type": "install_update",
                "request_id": "update-install-1",
                "expected_version": "9.0.0",
                "expected_revision": "e" * 40,
            })

        self.assertFalse(ws.messages[-1]["ok"])
        self.assertIn("changed", ws.messages[-1]["error"])

    async def test_update_install_schedules_an_external_verified_job(self):
        ws = FakeWebSocket()
        status = {
            "state": "available",
            "current_version": PLUGIN_VERSION,
            "current_revision": "abc123",
            "available_version": "9.0.0",
            "available_revision": "f" * 12,
            "target_revision": "f" * 40,
            "checked_at": 123,
            "can_install": True,
            "mode": "local",
            "reason": "",
            "error": "",
        }
        with tempfile.TemporaryDirectory() as temp_dir:
            with (
                patch.object(relay, "UPDATE_STATUS", status),
                patch.object(relay, "RUNTIME_DIR", Path(temp_dir)),
                patch.object(relay, "publish_update_status", AsyncMock()),
                patch.object(relay, "launch_update_job", return_value="update-job-1") as launch,
                patch.object(
                    relay.asyncio,
                    "to_thread",
                    AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
                ),
            ):
                await relay.handle_install_update_command(ws, {
                    "type": "install_update",
                    "request_id": "update-install-1",
                    "expected_version": "9.0.0",
                    "expected_revision": "f" * 40,
                })

        launch.assert_called_once()
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["phase"], "scheduled")
        self.assertEqual(ws.messages[-1]["data"]["job"], "update-job-1")

    async def test_app_deploy_requires_the_configured_origin_and_exact_release(self):
        ws = FakeWebSocket()
        config = {
            "configured": True,
            "origin": "https://app.example.test",
            "project": "herdr-app",
            "branch": "main",
            "reason": "",
        }
        with (
            patch.object(relay, "APP_DEPLOY_CONFIG", config),
            patch.object(relay, "APP_DEPLOY_STATUS", {"state": "idle"}),
            patch.object(relay, "publish_app_deploy_status", AsyncMock()),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(
                relay,
                "launch_app_deploy_job",
                side_effect=RuntimeError("this relay is not authorized to deploy the requested app origin"),
            ),
        ):
            await relay.handle_deploy_app_update_command(ws, {
                "type": "deploy_app_update",
                "request_id": "app-deploy-1",
                "expected_version": PLUGIN_VERSION,
                "expected_revision": "f" * 40,
                "expected_origin": "https://other.example.test",
            })

        self.assertFalse(ws.messages[-1]["ok"])
        self.assertIn("not authorized", ws.messages[-1]["error"])

    async def test_app_deploy_schedules_an_external_verified_job(self):
        ws = FakeWebSocket()
        config = {
            "configured": True,
            "origin": "https://app.example.test",
            "project": "herdr-app",
            "branch": "main",
            "reason": "",
        }
        with tempfile.TemporaryDirectory() as temp_dir:
            with (
                patch.object(relay, "APP_DEPLOY_CONFIG", config),
                patch.object(relay, "APP_DEPLOY_STATUS", {"state": "idle"}),
                patch.object(relay, "RUNTIME_DIR", Path(temp_dir)),
                patch.object(relay, "publish_app_deploy_status", AsyncMock()),
                patch.object(relay, "launch_app_deploy_job", return_value="app-deploy-job-1") as launch,
                patch.object(
                    relay.asyncio,
                    "to_thread",
                    AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
                ),
            ):
                await relay.handle_deploy_app_update_command(ws, {
                    "type": "deploy_app_update",
                    "request_id": "app-deploy-1",
                    "expected_version": PLUGIN_VERSION,
                    "expected_revision": "f" * 40,
                    "expected_origin": "https://app.example.test",
                })

        launch.assert_called_once()
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["phase"], "scheduled")
        self.assertEqual(ws.messages[-1]["data"]["job"], "app-deploy-job-1")

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

    async def test_slow_terminal_read_does_not_queue_a_later_mutation(self):
        read_started = asyncio.Event()
        release_read = asyncio.Event()
        mutation_handled = asyncio.Event()

        class QueueWebSocket(FakeWebSocket):
            def __init__(self):
                super().__init__()
                self.incoming = asyncio.Queue()

            async def __anext__(self):
                item = await self.incoming.get()
                if item is None:
                    raise StopAsyncIteration
                return json.dumps(item)

        async def slow_read(_ws, _msg):
            read_started.set()
            await release_read.wait()

        async def handle_prompt(_ws, _msg):
            mutation_handled.set()

        ws = QueueWebSocket()
        with (
            patch.object(relay, "clients", set()),
            patch.object(relay, "agent_refresh_clients", set()),
            patch.object(relay, "failed_clients", set()),
            patch.object(relay, "client_outbound_queues", {}),
            patch.object(relay, "client_outbound_tasks", {}),
            patch.object(relay, "client_handler_tasks", {}),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "activity_delivery_lock", asyncio.Lock()),
            patch.object(relay, "agent_profiles_for_new_client", AsyncMock(return_value={})),
            patch.object(relay, "ensure_vapid_public_key", return_value="public-key"),
            patch.object(relay, "load_activity", return_value=[]),
            patch.object(relay, "handle_read_pane_message", side_effect=slow_read),
            patch.object(relay, "handle_submit_prompt_command", side_effect=handle_prompt),
        ):
            handler = asyncio.create_task(relay.handle_client(ws))
            await ws.incoming.put({"type": "read_pane", "pane_id": "w1:p1"})
            await asyncio.wait_for(read_started.wait(), timeout=0.2)
            await ws.incoming.put({
                "type": "submit_prompt", "pane_id": "w1:p1", "text": "approve",
                "protocol": relay.PROTOCOL_VERSION,
            })
            await asyncio.wait_for(mutation_handled.wait(), timeout=0.2)
            release_read.set()
            await ws.incoming.put(None)
            await asyncio.wait_for(handler, timeout=0.2)

    async def test_approval_reports_accepted_then_confirmed(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-1", "pane_id": "w1:p1", "event_id": "event-1", "index": 0, "total": 3, "choice": "yes"}

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", {}),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "event-1"}}),
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
        run_command.assert_awaited_once()
        self.assertEqual(run_command.await_args.args, ("pane", "send-keys", "w1:p1", "Enter"))
        self.assertGreater(run_command.await_args.kwargs["deadline"], relay.time.monotonic() - 5)

    async def test_stale_approval_is_rejected_without_sending_keys(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "working", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-2", "pane_id": "w1:p1", "event_id": "event-1", "index": 0, "total": 3}

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", {}),
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

    async def test_approval_rejects_a_previous_blocked_event(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-stale", "pane_id": "w1:p1", "event_id": "old-event", "index": 0, "total": 3}

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", {}),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "new-event"}}),
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
        self.assertIn("stale", ws.messages[0]["error"])
        run_command.assert_not_awaited()

    async def test_concurrent_approvals_send_keys_once_per_event(self):
        first_ws = FakeWebSocket()
        second_ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        send_started = asyncio.Event()
        release_send = asyncio.Event()

        async def paused_send(*_args, **_kwargs):
            send_started.set()
            await release_send.wait()
            return True, "", ""

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", {}),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "event-1"}}),
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=paused_send)) as run_command,
            patch.object(relay, "wait_for_approval_result", AsyncMock(return_value=(False, "blocked"))),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            first = asyncio.create_task(relay.handle_respond_command(first_ws, {
                "type": "respond", "request_id": "request-1", "pane_id": "w1:p1",
                "event_id": "event-1", "index": 0, "total": 3,
            }))
            await asyncio.wait_for(send_started.wait(), timeout=0.2)
            second = asyncio.create_task(relay.handle_respond_command(second_ws, {
                "type": "respond", "request_id": "request-2", "pane_id": "w1:p1",
                "event_id": "event-1", "index": 0, "total": 3,
            }))
            await asyncio.sleep(0)
            release_send.set()
            await asyncio.gather(first, second)

        run_command.assert_awaited_once()
        self.assertEqual(run_command.await_args.args, ("pane", "send-keys", "w1:p1", "Enter"))
        self.assertTrue(second_ws.messages[-1]["ok"])
        self.assertEqual(second_ws.messages[-1]["phase"], "unconfirmed")

    async def test_conflicting_approval_for_same_event_is_rejected(self):
        ws = FakeWebSocket()
        msg = {
            "type": "respond", "request_id": "request-conflict", "pane_id": "w1:p1",
            "event_id": "event-1", "index": 2, "total": 3, "choice": "Reject",
        }

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(
                relay,
                "approved_event_results",
                {"w1:p1": {"event_id": "event-1", "phase": "confirmed", "index": 0, "total": 3}},
            ),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as run_command,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws, msg)

        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("different response", ws.messages[0]["error"])
        run_command.assert_not_awaited()

    async def test_approval_times_out_before_sending_keys(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-slow", "pane_id": "w1:p1", "event_id": "event-1", "index": 0, "total": 3}

        async def slow_inventory(*_args, **_kwargs):
            await asyncio.sleep(0.2)
            return agent, ""

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", {}),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "event-1"}}),
            patch.object(relay, "APPROVAL_DEADLINE_SECONDS", 0.05),
            patch.object(relay.asyncio, "to_thread", AsyncMock(side_effect=slow_inventory)),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as run_command,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws, msg)

        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("Timed out", ws.messages[0]["error"])
        run_command.assert_not_awaited()

    async def test_approval_deadline_starts_before_lock_acquisition(self):
        ws = FakeWebSocket()
        msg = {"type": "respond", "request_id": "request-lock", "pane_id": "w1:p1", "event_id": "event-1", "index": 0, "total": 3}
        lock = asyncio.Lock()
        await lock.acquire()

        async def release_later():
            await asyncio.sleep(0.08)
            lock.release()

        with (
            patch.object(relay, "approval_locks", {"w1:p1": lock}),
            patch.object(relay, "approved_event_results", {}),
            patch.object(relay, "APPROVAL_DEADLINE_SECONDS", 0.05),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as run_command,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            releaser = asyncio.create_task(release_later())
            await relay.handle_respond_command(ws, msg)
            await releaser

        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("Timed out waiting for the pane", ws.messages[0]["error"])
        run_command.assert_not_awaited()

    async def test_submit_prompt_times_out_before_sending(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "working", "agent": "codex", "project": "relay"}
        msg = {"type": "submit_prompt", "request_id": "request-prompt", "pane_id": "w1:p1", "text": "hello"}

        async def slow_inventory(*_args, **_kwargs):
            await asyncio.sleep(0.2)
            return agent, ""

        with (
            patch.object(relay, "COMMAND_DEADLINE_SECONDS", 0.05),
            patch.object(relay.asyncio, "to_thread", AsyncMock(side_effect=slow_inventory)),
            patch.object(relay, "send_prompt_to_pane", AsyncMock()) as send_prompt,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_submit_prompt_command(ws, msg)

        self.assertFalse(ws.messages[0]["ok"])
        self.assertIn("Timed out", ws.messages[0]["error"])
        send_prompt.assert_not_awaited()

    async def test_wait_for_approval_result_bounds_slow_inventory(self):
        async def slow_to_thread(function, *args, **kwargs):
            await asyncio.sleep(5)
            return function(*args, **kwargs)

        with (
            patch.object(relay, "get_agents", return_value=[{"pane_id": "w1:p1", "status": "blocked"}]),
            patch.object(relay.asyncio, "to_thread", AsyncMock(side_effect=slow_to_thread)),
        ):
            started = relay.time.monotonic()
            confirmed, status = await relay.wait_for_approval_result("w1:p1", timeout=0.6)
            elapsed = relay.time.monotonic() - started

        self.assertFalse(confirmed)
        self.assertEqual(status, "blocked")
        self.assertLess(elapsed, 2.0)

    def test_mutation_worker_rechecks_deadline_before_spawning(self):
        deadline = relay.time.monotonic() - 0.01
        with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
            ok, _output, error = relay._run_herdr_mutation(
                ("pane", "send-keys", "w1:p1", "Enter"), deadline, threading.Event()
            )
        self.assertFalse(ok)
        self.assertIn("Timed out", error)
        run_result.assert_not_called()

    def test_mutation_worker_sizes_subprocess_timeout_to_remaining_budget(self):
        deadline = relay.time.monotonic() + 0.5
        with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
            relay._run_herdr_mutation(("pane", "send-keys", "w1:p1", "Enter"), deadline, threading.Event())
        timeout = run_result.call_args.kwargs["timeout"]
        self.assertGreater(timeout, 0)
        self.assertLessEqual(timeout, 0.5)

    async def test_mutation_queued_past_deadline_never_spawns(self):
        async def delayed_worker(function, *args, **kwargs):
            await asyncio.sleep(0.08)
            return function(*args, **kwargs)

        deadline = relay.time.monotonic() + 0.05
        with (
            patch.object(relay.asyncio, "to_thread", AsyncMock(side_effect=delayed_worker)),
            patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result,
        ):
            ok, _output, error = await relay.run_herdr_async_result(
                "pane", "send-keys", "w1:p1", "Enter", deadline=deadline
            )
        self.assertFalse(ok)
        self.assertIn("Timed out", error)
        run_result.assert_not_called()

    async def test_question_keys_stop_once_the_mutation_deadline_expires(self):
        token = relay.mutation_deadline.set(relay.time.monotonic() - 0.01)
        try:
            with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
                ok, error = await relay.send_question_keys("w1:p1", ["Down", "Down", "Enter"])
        finally:
            relay.mutation_deadline.reset(token)
        self.assertFalse(ok)
        self.assertIn("Timed out", error)
        run_result.assert_not_called()

    async def test_rename_passes_the_same_deadline_to_every_stage(self):
        agent = {"pane_id": "w1:p1", "tab_id": "w1", "agent": "claude", "project": "relay"}
        herdr = AsyncMock(return_value=(True, "", ""))
        await self._run_agent_rename(agent, herdr)
        deadlines = [call.kwargs.get("deadline") for call in herdr.await_args_list]
        self.assertEqual(len(deadlines), 2)
        self.assertTrue(all(deadline is not None for deadline in deadlines))
        self.assertEqual(deadlines[0], deadlines[1])

    def test_mutation_worker_maps_subprocess_timeout_to_dispatched_unknown(self):
        deadline = relay.time.monotonic() + 5.0
        with patch.object(relay, "run_herdr_result", return_value=(False, "", "herdr command timed out")):
            ok, _output, error = relay._run_herdr_mutation(
                ("pane", "send-keys", "w1:p1", "Enter"), deadline, threading.Event()
            )
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_DISPATCHED_UNKNOWN)

    def test_mutation_worker_preserves_not_started_for_expired_deadline(self):
        deadline = relay.time.monotonic() - 0.01
        with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
            ok, _output, error = relay._run_herdr_mutation(
                ("pane", "send-keys", "w1:p1", "Enter"), deadline, threading.Event()
            )
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_NOT_STARTED)
        run_result.assert_not_called()

    async def test_approval_dispatched_unknown_blocks_retry(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "status": "blocked", "agent": "codex", "project": "relay"}
        msg = {"type": "respond", "request_id": "request-1", "pane_id": "w1:p1", "event_id": "event-1", "index": 0, "total": 3, "choice": "yes"}
        approved = {}

        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", approved),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "event-1"}}),
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "run_herdr_async_result", AsyncMock(return_value=(False, "", relay.MUTATION_DISPATCHED_UNKNOWN))),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws, msg)

        self.assertFalse(ws.messages[-1]["ok"])
        self.assertIn("not confirmed", ws.messages[-1]["error"])
        self.assertEqual(approved["w1:p1"]["event_id"], "event-1")
        self.assertEqual(approved["w1:p1"]["phase"], "dispatched_unknown")

        ws2 = FakeWebSocket()
        msg2 = {**msg, "request_id": "request-retry"}
        with (
            patch.object(relay, "approval_locks", {}),
            patch.object(relay, "approved_event_results", approved),
            patch.object(relay, "blocked_agent_details", {"w1:p1": {"event_id": "event-1"}}),
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "run_herdr_async_result", AsyncMock()) as run_command,
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_respond_command(ws2, msg2)

        self.assertFalse(ws2.messages[-1]["ok"])
        self.assertIn("retry blocked", ws2.messages[-1]["error"])
        run_command.assert_not_awaited()

    async def test_agent_start_passes_deadline_to_every_stage(self):
        with tempfile.TemporaryDirectory() as cwd:
            with (
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w7")) as ws_mock,
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w7:p1", "tab_id": "w7:t2"}}), ""),
                    (True, json.dumps({"result": {"agent": {"pane_id": "w7:p1"}}}), ""),
                ])) as run_command,
            ):
                deadline = relay.time.monotonic() + 40.0
                await relay.start_agent_in_new_tab(
                    {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"},
                    "test-agent",
                    Path(cwd),
                    deadline=deadline,
                )

        startup_deadline = deadline - relay.AGENT_START_ROLLBACK_SECONDS
        ws_mock.assert_awaited_once_with(Path(cwd), deadline=startup_deadline)
        deadlines = [call.kwargs.get("deadline") for call in run_command.await_args_list]
        self.assertTrue(all(d == startup_deadline for d in deadlines), f"Expected all deadlines to be {startup_deadline}, got {deadlines}")

    async def test_concurrent_clear_serialized_by_lifecycle_lock(self):
        ws1 = FakeWebSocket()
        ws2 = FakeWebSocket()

        with tempfile.TemporaryDirectory() as cwd:
            agent = {"pane_id": "w1:p1", "status": "idle", "agent": "codex", "project": "relay", "cwd": cwd}
            first_call = True

            async def bounded_agent_side_effect(pane_id, deadline):
                nonlocal first_call
                if first_call:
                    first_call = False
                    return agent, ""
                return None, "Agent is no longer available"

            with (
                patch.object(relay, "lifecycle_locks", {}),
                patch.object(relay, "bounded_agent_for_pane", AsyncMock(side_effect=bounded_agent_side_effect)),
                patch.object(relay, "_profile_id_for_agent", return_value="codex"),
                patch.object(relay, "load_agent_profiles", return_value={"codex": {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"}}),
                patch.object(relay, "resolve_agent_cwd", return_value=(Path(cwd), "")),
                patch.object(relay, "start_agent_in_new_tab", AsyncMock(return_value=(True, {}, "w1:p2", "", ""))),
                patch.object(relay, "run_herdr_async_result", AsyncMock(return_value=(True, "", ""))),
                patch.object(relay, "publish_activity", AsyncMock()),
            ):
                msg = {"type": "agent_clear", "request_id": "r1", "pane_id": "w1:p1"}
                await asyncio.gather(
                    relay.handle_agent_clear_command(ws1, {**msg, "request_id": "r1"}),
                    relay.handle_agent_clear_command(ws2, {**msg, "request_id": "r2"}),
                )

        results = [ws.messages[-1] for ws in (ws1, ws2)]
        ok_results = [r for r in results if r["ok"]]
        failed_results = [r for r in results if not r["ok"]]
        self.assertEqual(len(ok_results), 1)
        self.assertEqual(len(failed_results), 1)
        self.assertIn("no longer available", failed_results[0]["error"])

    async def test_outer_timeout_returns_dispatched_unknown_when_worker_started(self):
        def slow_mutation(args, deadline, started_signal):
            started_signal.set()
            time.sleep(2)
            return True, "", ""

        deadline = relay.time.monotonic() + 0.05
        with patch.object(relay, "_run_herdr_mutation", slow_mutation):
            ok, _output, error = await relay.run_herdr_async_result(
                "pane", "send-keys", "w1:p1", "Enter", deadline=deadline
            )
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_DISPATCHED_UNKNOWN)

    async def test_outer_timeout_returns_not_started_when_worker_never_started(self):
        def queued_mutation(args, deadline, started_signal):
            time.sleep(2)
            started_signal.set()
            return True, "", ""

        deadline = relay.time.monotonic() + 0.05
        with patch.object(relay, "_run_herdr_mutation", queued_mutation):
            ok, _output, error = await relay.run_herdr_async_result(
                "pane", "send-keys", "w1:p1", "Enter", deadline=deadline
            )
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_NOT_STARTED)

    async def test_prompt_dispatched_unknown_includes_data_flag(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "agent": "codex", "project": "relay"}
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(side_effect=lambda function, *args, **kwargs: function(*args, **kwargs)),
            ),
            patch.object(relay, "send_prompt_to_pane", AsyncMock(return_value=(False, relay.MUTATION_DISPATCHED_UNKNOWN))),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_submit_prompt_command(ws, {
                "type": "submit_prompt",
                "request_id": "req-prompt",
                "pane_id": "w1:p1",
                "text": "do something",
            })

        result = ws.messages[-1]
        self.assertFalse(result["ok"])
        self.assertEqual(result["data"]["dispatched_unknown"], True)

    async def test_stop_acquires_lifecycle_lock(self):
        ws = FakeWebSocket()
        agent = {"pane_id": "w1:p1", "agent": "codex", "project": "relay"}
        lock = asyncio.Lock()
        await lock.acquire()

        with (
            patch.object(relay, "lifecycle_locks", {"w1:p1": lock}),
            patch.object(relay, "bounded_agent_for_pane", AsyncMock(return_value=(agent, ""))),
            patch.object(relay, "run_herdr_async_result", AsyncMock(return_value=(True, "", ""))),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            task = asyncio.create_task(relay.handle_agent_stop_command(ws, {
                "type": "agent_stop", "request_id": "req-stop", "pane_id": "w1:p1",
            }))
            await asyncio.sleep(0.05)
            self.assertFalse(task.done())
            lock.release()
            await task

        self.assertTrue(ws.messages[-1]["ok"])

    async def test_close_failed_agent_target_receives_deadline(self):
        deadline = relay.time.monotonic() + 10.0
        with patch.object(relay, "run_herdr_async_result", AsyncMock(return_value=(True, "", ""))) as run_command:
            await relay.close_failed_agent_target("w1:p1", "start failed", deadline=deadline)
        self.assertEqual(run_command.await_args.kwargs["deadline"], deadline)

    def test_worker_sets_started_before_deadline_check(self):
        started = threading.Event()
        deadline = relay.time.monotonic() - 0.01
        with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
            ok, _output, error = relay._run_herdr_mutation(
                ("pane", "send-keys", "w1:p1", "Enter"), deadline, started
            )
        self.assertTrue(started.is_set())
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_NOT_STARTED)
        run_result.assert_not_called()

    async def test_lifecycle_guard_blocks_mutation_after_clear(self):
        relay.lifecycle_generations["w1:p1"] = 0
        gen = relay.lifecycle_generations.get("w1:p1", 0)
        token = relay.mutation_guard.set(lambda: relay.lifecycle_generations.get("w1:p1", 0) == gen)
        try:
            relay.lifecycle_generations["w1:p1"] = 1
            deadline = relay.time.monotonic() + 5.0
            started = threading.Event()
            with patch.object(relay, "run_herdr_result", return_value=(True, "", "")) as run_result:
                ok, _output, error = relay._run_herdr_mutation(
                    ("pane", "send-keys", "w1:p1", "Enter"), deadline, started
                )
        finally:
            relay.mutation_guard.reset(token)
            relay.lifecycle_generations.pop("w1:p1", None)
        self.assertFalse(ok)
        self.assertEqual(error, relay.MUTATION_PANE_REPLACED)
        run_result.assert_not_called()

    async def test_cleanup_gets_full_deadline_when_startup_expires(self):
        with tempfile.TemporaryDirectory() as cwd:
            deadline = relay.time.monotonic() + 40.0
            startup_deadline = deadline - relay.AGENT_START_ROLLBACK_SECONDS
            with (
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w7")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w7:p1", "tab_id": "w7:t2"}}), ""),
                    (False, "", "Agent start timed out"),
                    (True, "", ""),
                ])) as run_command,
            ):
                ok, _data, _pane_id, _placement, error = await relay.start_agent_in_new_tab(
                    {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"},
                    "test-agent",
                    Path(cwd),
                    deadline=deadline,
                )

        self.assertFalse(ok)
        cleanup_call = run_command.await_args_list[-1]
        self.assertEqual(cleanup_call.args[:2], ("pane", "close"))
        self.assertEqual(cleanup_call.kwargs["deadline"], deadline)
        self.assertNotEqual(cleanup_call.kwargs["deadline"], startup_deadline)

    async def test_stale_pane_read_does_not_resurrect_blocked_state(self):
        ws = FakeWebSocket()

        async def read_that_unblocks(*_args, **_kwargs):
            relay.last_statuses["w1:p1"] = "working"
            relay.advance_agent_state_revision("w1:p1")
            return CODEX_QUESTION_VIEW

        with (
            patch.object(relay, "agent_types", {"w1:p1": "codex"}),
            patch.object(relay, "last_statuses", {"w1:p1": "blocked"}),
            patch.object(relay, "agent_state_revisions", {"w1:p1": 7}),
            patch.object(relay, "blocked_agent_details", {}),
            patch.object(relay, "acknowledge_pane_viewed", AsyncMock()),
            patch.object(relay, "run_herdr_async", AsyncMock(side_effect=read_that_unblocks)),
        ):
            await relay.handle_read_pane_message(ws, {"pane_id": "w1:p1", "lines": 30})

        payload = ws.messages[0]
        self.assertEqual(payload["type"], "pane_content")
        self.assertIsNone(payload["interaction"])
        self.assertFalse(payload["question_layout"])
        self.assertEqual(relay.blocked_agent_details, {})

    async def test_fresh_pane_read_still_reports_blocked_question(self):
        ws = FakeWebSocket()

        with (
            patch.object(relay, "agent_types", {"w1:p1": "codex"}),
            patch.object(relay, "last_statuses", {"w1:p1": "blocked"}),
            patch.object(relay, "agent_state_revisions", {"w1:p1": 7}),
            patch.object(relay, "blocked_agent_details", {}),
            patch.object(relay, "latest_agents_message", relay.agents_message([])),
            patch.object(relay, "acknowledge_pane_viewed", AsyncMock()),
            patch.object(relay, "run_herdr_async", AsyncMock(return_value=CODEX_QUESTION_VIEW)),
        ):
            await relay.handle_read_pane_message(ws, {"pane_id": "w1:p1", "lines": 30})

        payload = ws.messages[0]
        self.assertEqual(payload["type"], "pane_content")
        self.assertIsNotNone(payload["interaction"])
        self.assertTrue(payload["question_layout"])

    async def test_agent_start_uses_live_facade_and_atomic_initial_prompt(self):
        ws = FakeWebSocket()
        msg = {
            "type": "agent_start",
            "request_id": "request-3",
            "profile_id": "codex",
            "name": "mobile-test",
            "prompt": "--literal task text",
        }
        with tempfile.TemporaryDirectory() as cwd:
            msg["cwd"] = cwd
            command_results = [
                (True, json.dumps({"result": {"pane_id": "w7:p1", "tab_id": "w7:t2"}}), ""),
                (True, json.dumps({"result": {"agent": {"pane_id": "w7:p1", "workspace_id": "w7"}}}), ""),
                (True, "", ""),
            ]
            with (
                patch.object(relay.Path, "home", return_value=Path(cwd)),
                patch.object(relay, "load_agent_profiles", return_value={"codex": {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"}}),
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w7")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=command_results)) as run_command,
                patch.object(relay, "publish_activity", AsyncMock()),
                patch.object(relay.asyncio, "sleep", AsyncMock()),
            ):
                await relay.handle_agent_start_command(ws, msg)

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0], (
            "tab", "create", "--workspace", "w7", "--cwd", cwd,
            "--label", "mobile-test", "--no-focus",
        ))
        self.assertEqual(calls[1], (
            "agent", "start", "mobile-test", "--kind", "codex", "--pane", "w7:p1",
            "--timeout", "30000",
        ))
        self.assertNotIn("--literal task text", calls[1])
        self.assertEqual(calls[2], ("agent", "prompt", "w7:p1", "--literal task text"))
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["data"]["pane_id"], "w7:p1")
        self.assertEqual(ws.messages[-1]["data"]["name"], "mobile-test")
        self.assertEqual(ws.messages[-1]["data"]["cwd"], cwd)

    async def test_agent_start_creates_a_space_when_the_working_directory_has_none(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            cwd = Path(temp_dir) / "new-project"
            cwd.mkdir()
            with (
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w10:p1", "tab_id": "w10:t1", "workspace_id": "w10"}}), ""),
                    (True, "", ""),
                    (True, json.dumps({"result": {"agent": {"pane_id": "w10:p1", "workspace_id": "w10"}}}), ""),
                ])) as run_command,
            ):
                ok, _data, pane_id, placement_error, error = await relay.start_agent_in_new_tab(
                    {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"},
                    "mobile-test",
                    cwd,
                )

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0], (
            "workspace", "create", "--cwd", str(cwd), "--label", "new-project", "--no-focus",
        ))
        self.assertEqual(calls[1], ("tab", "rename", "w10:t1", "mobile-test"))
        self.assertEqual(calls[2], (
            "agent", "start", "mobile-test", "--kind", "codex", "--pane", "w10:p1",
            "--timeout", "30000",
        ))
        self.assertTrue(ok)
        self.assertEqual(pane_id, "w10:p1")
        self.assertEqual(placement_error, "")
        self.assertEqual(error, "")

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
                patch.object(relay, "load_agent_profiles", return_value={"codex": {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"}}),
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w1")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w1:p2", "tab_id": "w1:t3"}}), ""),
                    (True, json.dumps({"result": {"agent": {"pane_id": "w1:p2", "workspace_id": "w1"}}}), ""),
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
        self.assertEqual(calls[0][0:2], ("tab", "create"))
        self.assertEqual(calls[1][0:2], ("agent", "start"))
        self.assertEqual(calls[2], ("pane", "close", "w1:p1"))
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["data"]["pane_id"], "w1:p2")
        self.assertEqual(ws.messages[-1]["data"]["cwd"], cwd)

    async def test_agent_clear_uses_remembered_custom_profile(self):
        ws = FakeWebSocket()
        profile = {
            "id": "pi-local",
            "label": "Pi Local",
            "argv": ["pi-local"],
        }
        with tempfile.TemporaryDirectory() as cwd:
            agent = {
                "pane_id": "w1:p1",
                "status": "idle",
                "agent": "pi-coding-agent",
                "project": "relay",
                "cwd": cwd,
            }
            relay.remember_agent_profile("w1:p1", "pi-local")
            start_agent = AsyncMock(
                return_value=(False, None, "", "", "start failed")
            )
            with (
                patch.object(relay.Path, "home", return_value=Path(cwd)),
                patch.object(relay, "agent_for_pane", return_value=(agent, "")),
                patch.object(
                    relay.asyncio,
                    "to_thread",
                    AsyncMock(
                        side_effect=lambda function, *args, **kwargs: function(
                            *args, **kwargs
                        )
                    ),
                ),
                patch.object(
                    relay,
                    "load_agent_profiles",
                    return_value={"pi-local": profile},
                ),
                patch.object(
                    relay,
                    "AGENT_PROFILE_CANDIDATES",
                    {"pi-local": "Pi Local"},
                ),
                patch.object(relay, "start_agent_in_new_tab", start_agent),
                patch.object(relay, "publish_activity", AsyncMock()),
            ):
                await relay.handle_agent_clear_command(ws, {
                    "type": "agent_clear",
                    "request_id": "request-custom-clear",
                    "pane_id": "w1:p1",
                })

        self.assertEqual(start_agent.await_args.args[0], profile)
        self.assertNotIn("does not match", ws.messages[-1]["error"])

    async def test_agent_stop_forgets_remembered_profile(self):
        ws = FakeWebSocket()
        agent = {
            "pane_id": "w1:p1",
            "status": "idle",
            "agent": "pi-coding-agent",
            "project": "relay",
        }
        relay.remember_agent_profile("w1:p1", "pi-local")
        with (
            patch.object(relay, "agent_for_pane", return_value=(agent, "")),
            patch.object(
                relay.asyncio,
                "to_thread",
                AsyncMock(
                    side_effect=lambda function, *args, **kwargs: function(
                        *args, **kwargs
                    )
                ),
            ),
            patch.object(
                relay,
                "run_herdr_async_result",
                AsyncMock(return_value=(True, "", "")),
            ),
            patch.object(relay, "publish_activity", AsyncMock()),
        ):
            await relay.handle_agent_stop_command(ws, {
                "type": "agent_stop",
                "request_id": "request-custom-stop",
                "pane_id": "w1:p1",
            })

        self.assertNotIn("w1:p1", relay.agent_profile_ids)
        self.assertTrue(ws.messages[-1]["ok"])

    async def test_custom_profile_runs_in_target_pane_and_keeps_identity(self):
        async_side_effect = [
            (True, json.dumps({"result": {"pane_id": "w9:p0", "tab_id": "w9:t2"}}), ""),
            (True, "", ""),
            (True, json.dumps({"result": {"agent": {"pane_id": "w9:p0"}}}), ""),
            (True, "", ""),
        ]
        with (
            patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w9")),
            patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=async_side_effect)) as run_command,
        ):
            ok, _data, pane_id, placement_error, error = await relay.start_agent_in_new_tab(
                {"id": "pi-local", "label": "Pi Local", "argv": ["/opt/Pi Local/pi", "--model", "fast"], "kind": ""},
                "mobile-pi", Path("/tmp"),
            )
        self.assertTrue(ok)
        self.assertEqual(pane_id, "w9:p0")
        self.assertEqual(placement_error, "")
        self.assertEqual(error, "")
        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[1], ("pane", "run", "w9:p0", "'/opt/Pi Local/pi' --model fast"))
        self.assertEqual(calls[2], ("agent", "get", "w9:p0"))
        self.assertEqual(calls[3], ("agent", "rename", "w9:p0", "mobile-pi"))
        self.assertEqual(relay.agent_profile_ids["w9:p0"], "pi-local")

    async def test_failed_agent_start_closes_unused_target_pane(self):
        async_side_effect = [
            (True, json.dumps({"result": {"pane_id": "w9:p0", "tab_id": "w9:t2"}}), ""),
            (False, "", "agent_prompt_stalled"),
            (True, "", ""),
        ]
        with (
            patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w9")),
            patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=async_side_effect)) as run_command,
        ):
            ok, _data, pane_id, placement_error, error = await relay.start_agent_in_new_tab(
                {"id": "codex", "label": "Codex", "argv": ["codex"], "kind": "codex"},
                "mobile-codex", Path("/tmp"),
            )
        self.assertFalse(ok)
        self.assertEqual(pane_id, "")
        self.assertEqual(placement_error, "")
        self.assertIn("agent_prompt_stalled", error)
        self.assertEqual(run_command.await_args_list[-1].args, ("pane", "close", "w9:p0"))

    async def test_replace_to_merge_reload_first_client_sees_discovered_integrations(self):
        def find_executable(name):
            return f"/usr/bin/{name}" if name in {"pi", "qodercli"} else None

        replace_parser = configparser.ConfigParser(interpolation=None)
        replace_parser.read_string("[profiles]\npi = Pi\n[config]\nreplace_profiles = true\n")
        merge_parser = configparser.ConfigParser(interpolation=None)
        merge_parser.read_string("[profiles]\npi = Pi\n")

        cache = {"names": set(), "expires": 0.0, "refreshing": False}
        ready = threading.Event()
        ready.set()

        with patch.object(relay, "_herdr_integrations_cache", cache), \
                patch.object(relay, "_integrations_ready", ready), \
                patch.object(relay, "_integrations_generation", 0), \
                patch.object(relay, "_AGENT_PROFILES_INI_CACHE", replace_parser), \
                patch.object(relay, "AGENT_PROFILE_CANDIDATES", {"pi": "Pi"}), \
                patch.object(relay, "herdr_installed_integrations", return_value={"qodercli"}), \
                patch.object(relay.shutil, "which", side_effect=find_executable):
            # Replacement-mode startup: prewarm skips discovery, cache stays empty.
            relay.prewarm_herdr_integrations()
            self.assertEqual(cache["names"], set())
            self.assertTrue(ready.is_set())

            # SIGHUP switches the config to merge mode and reloads.
            with patch.object(relay, "_read_agent_profiles_ini", return_value=merge_parser):
                relay._reload_agent_profiles_ini()

            # The reload cleared readiness and kicked off an async refresh; the
            # first client waits for readiness and sees the discovered agent.
            profiles = await relay.agent_profiles_for_new_client()

        self.assertIn("qodercli", profiles)
        self.assertIn("pi", profiles)

    async def test_publish_blocked_generates_shared_id_without_pregenerated_one(self):
        pane_id = "w1:p1"
        captured = {}
        spawned = []

        async def fake_publish_activity(*args, **kwargs):
            captured["activity_details"] = kwargs.get("details")
            return {"timestamp": 12345}

        async def fake_broadcast(msg):
            captured["broadcast"] = msg

        async def fake_push_blocked(msg):
            captured["push"] = msg

        def fake_create_task(coro, **kwargs):
            spawned.append(coro)
            return None

        cache = {}
        with patch.object(relay, "blocked_agent_details", cache), \
                patch.object(relay, "latest_agents_message", relay.agents_message([])), \
                patch.object(relay, "publish_activity", side_effect=fake_publish_activity), \
                patch.object(relay, "broadcast", side_effect=fake_broadcast), \
                patch.object(relay, "push_blocked", side_effect=fake_push_blocked), \
                patch.object(relay.asyncio, "create_task", side_effect=fake_create_task):
            await relay.publish_blocked({
                "type": "blocked",
                "pane_id": pane_id,
                "agent": "codex",
                "command": "Run tests?",
                "prompt": "Run tests?",
                "options": ["Yes", "No"],
            })
            for coro in spawned:
                await coro

        # No pre-generated ID was supplied (the plugin/UDP path), so
        # publish_blocked generated one and every artifact shares it — including
        # the reconnect snapshot a later client relies on for notification
        # routing.
        generated = cache[pane_id]["event_id"]
        self.assertTrue(generated)
        self.assertEqual(captured["activity_details"]["event_id"], generated)
        self.assertEqual(captured["broadcast"]["event_id"], generated)
        self.assertEqual(captured["push"]["event_id"], generated)

    async def test_rapid_plugin_blocked_cycles_mint_distinct_event_ids(self):
        pane_id = "w2:p7"
        blocked_ids = []
        spawned = []

        async def fake_publish_activity(*args, **kwargs):
            return {"timestamp": 12345}

        async def fake_broadcast(msg):
            if msg.get("type") == "blocked":
                blocked_ids.append(msg.get("event_id"))

        async def fake_push_blocked(msg):
            return None

        def fake_create_task(coro, **kwargs):
            spawned.append(coro)
            return None

        events = [
            {"type": "agent_event", "pane_id": pane_id, "status": "blocked", "agent": "codex"},
            {"type": "agent_event", "pane_id": pane_id, "status": "working", "agent": "codex"},
            {"type": "agent_event", "pane_id": pane_id, "status": "blocked", "agent": "codex"},
        ]

        async def fake_queue_get():
            if events:
                return events.pop(0)
            raise asyncio.CancelledError()

        snapshot = relay.agents_message([{"pane_id": pane_id, "status": "working", "agent": "codex"}])
        with patch.object(relay, "blocked_agent_details", {}), \
                patch.object(relay, "last_statuses", {}), \
                patch.object(relay, "latest_agents_message", snapshot), \
                patch.object(relay.event_queue, "get", side_effect=fake_queue_get), \
                patch.object(relay, "read_question_pane", return_value=""), \
                patch.object(relay, "register_finished_notification", return_value=False), \
                patch.object(relay, "register_status_transition", return_value=None), \
                patch.object(relay, "displayed_status", side_effect=lambda pid, status: status), \
                patch.object(relay, "publish_activity", side_effect=fake_publish_activity), \
                patch.object(relay, "broadcast", side_effect=fake_broadcast), \
                patch.object(relay, "push_blocked", side_effect=fake_push_blocked), \
                patch.object(relay.asyncio, "create_task", side_effect=fake_create_task):
            with self.assertRaises(asyncio.CancelledError):
                await relay.event_push()
            for coro in spawned:
                await coro

        # Two blocked cycles separated by a working transition — each must mint
        # its own event_id. Before the fix, the second cycle reused the stale
        # cached id because blocked_agent_details was only pruned on the next poll.
        self.assertEqual(len(blocked_ids), 2)
        self.assertTrue(blocked_ids[0])
        self.assertTrue(blocked_ids[1])
        self.assertNotEqual(blocked_ids[0], blocked_ids[1])

    async def test_plugin_event_updates_reconnect_snapshot_before_poll(self):
        pane_id = "w3:p1"
        queue = asyncio.Queue()
        sent_snapshots = []
        processed = asyncio.Event()
        spawned = []
        snapshot_at_blocked_broadcast = {}
        touch_stamp = 424242

        async def fake_broadcast(msg):
            if msg.get("type") == "blocked":
                # Capture the reconnect snapshot at the moment the blocked message
                # is published. The merge must already have flipped the status, so
                # a client reconnecting inside this window sees "blocked", not the
                # prior "working".
                snapshot_at_blocked_broadcast["agents"] = json.loads(relay.latest_agents_message)["agents"]
            if msg.get("type") == "agent_update":
                # The agent_update broadcast is the last step of event_push's
                # agent_event handling, so it signals processing is complete.
                processed.set()

        async def fake_publish_activity(*args, **kwargs):
            return {"timestamp": 1}

        async def fake_push_blocked(msg):
            return None

        def fake_create_task(coro, **kwargs):
            spawned.append(coro)
            return None

        class FakeWS:
            async def send(self, data):
                sent_snapshots.append(data)

        snapshot = relay.agents_message([
            {"pane_id": pane_id, "status": "working", "agent": "codex", "updated_at": 111},
        ])
        with patch.object(relay, "blocked_agent_details", {}), \
                patch.object(relay, "last_statuses", {}), \
                patch.object(relay, "latest_agents_message", snapshot), \
                patch.object(relay, "event_queue", queue), \
                patch.object(relay, "read_question_pane", return_value=""), \
                patch.object(relay, "register_finished_notification", return_value=False), \
                patch.object(relay, "register_status_transition", return_value=None), \
                patch.object(relay, "displayed_status", side_effect=lambda pid, status: status), \
                patch.object(relay, "touch_agent_activity", return_value=touch_stamp), \
                patch.object(relay, "publish_activity", side_effect=fake_publish_activity), \
                patch.object(relay, "broadcast", side_effect=fake_broadcast), \
                patch.object(relay, "push_blocked", side_effect=fake_push_blocked), \
                patch.object(relay.asyncio, "create_task", side_effect=fake_create_task):
            task = asyncio.get_running_loop().create_task(relay.event_push())
            try:
                # working -> blocked
                processed.clear()
                await queue.put({"type": "agent_event", "pane_id": pane_id, "status": "blocked", "agent": "codex"})
                await processed.wait()

                # The snapshot must have been merged before the blocked message was
                # published, not after.
                published_agent = snapshot_at_blocked_broadcast["agents"][0]
                self.assertEqual(published_agent["status"], "blocked")
                self.assertTrue(published_agent.get("event_id"))

                await relay.send_latest_agents(FakeWS())
                blocked_agent = json.loads(sent_snapshots[-1])["agents"][0]
                self.assertEqual(blocked_agent["status"], "blocked")
                self.assertTrue(blocked_agent.get("event_id"))
                self.assertEqual(blocked_agent.get("updated_at"), touch_stamp)

                # blocked -> working: the snapshot must drop the blocked state and
                # its event_id rather than lingering until the next poll.
                processed.clear()
                await queue.put({"type": "agent_event", "pane_id": pane_id, "status": "working", "agent": "codex"})
                await processed.wait()
                await relay.send_latest_agents(FakeWS())
                working_agent = json.loads(sent_snapshots[-1])["agents"][0]
                self.assertEqual(working_agent["status"], "working")
                self.assertNotIn("event_id", working_agent)
                self.assertEqual(working_agent.get("updated_at"), touch_stamp)
            finally:
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass
            for coro in spawned:
                coro.close()


class SuperviseTest(unittest.IsolatedAsyncioTestCase):
    async def test_restarts_loop_after_an_unhandled_exception(self):
        calls = []

        async def flaky():
            calls.append(len(calls))
            if len(calls) < 3:
                raise RuntimeError("boom")
            raise asyncio.CancelledError()

        with patch.object(relay.asyncio, "sleep", AsyncMock()):
            with self.assertRaises(asyncio.CancelledError):
                await relay.supervise(flaky, "flaky")

        # Two crashes, then the third run — the loop was restarted each time
        # rather than dying on the first exception.
        self.assertEqual(len(calls), 3)

    async def test_backoff_grows_then_resets_after_a_stable_run(self):
        delays = []

        async def crasher():
            raise RuntimeError("boom")

        monotonic_values = iter([
            0.0, 0.0,      # run 1: start, crash
            0.0, 0.0,      # run 2: start, crash
            0.0, 120.0,    # run 3: start, crash after 120s (stable)
        ])

        async def fake_sleep(seconds):
            delays.append(seconds)
            if len(delays) >= 3:
                raise asyncio.CancelledError()

        with (
            patch.object(relay.time, "monotonic", side_effect=lambda: next(monotonic_values)),
            patch.object(relay.asyncio, "sleep", side_effect=fake_sleep),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await relay.supervise(crasher, "crasher", min_backoff=1.0, max_backoff=30.0)

        # 1s, then doubled to 2s, then reset to 1s because run 3 lasted >= 60s.
        self.assertEqual(delays, [1.0, 2.0, 1.0])

    async def test_stops_supervising_when_the_loop_returns_cleanly(self):
        calls = []

        async def finite():
            calls.append(1)

        with patch.object(relay.asyncio, "sleep", AsyncMock()):
            await relay.supervise(finite, "finite")

        self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()

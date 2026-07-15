import asyncio
import copy
import importlib.util
import json
import os
import re
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
        relay.latest_agents_message = relay.agents_message([])
        relay.last_broadcast_agents_message = None
        self.addCleanup(relay.claude_history_state.clear)
        self.addCleanup(relay.claude_history_save_times.clear)
        self.addCleanup(relay.blocked_agent_details.clear)


class RelayHelpersTest(ClaudeHistoryIsolationMixin, unittest.TestCase):
    def test_protocol_v2_keeps_v1_as_the_unversioned_baseline(self):
        self.assertEqual(relay.PROTOCOL_VERSION, 2)
        self.assertIn("slash_commands", relay.RELAY_CAPABILITIES)
        self.assertEqual(relay.client_protocol_version({}), 1)
        self.assertFalse(relay.client_protocol_matches({}))
        self.assertTrue(relay.client_protocol_matches({"protocol": 2}))
        self.assertFalse(relay.client_protocol_matches({"protocol": True}))

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
        self.assertEqual(opencode, {"commands": [], "truncated": False})

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


class RelayCommandsTest(ClaudeHistoryIsolationMixin, unittest.IsolatedAsyncioTestCase):
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

    async def test_requested_refresh_sends_unchanged_snapshot_to_requesting_client(self):
        ws = FakeWebSocket()
        snapshot = relay.agents_message([
            {"pane_id": "w1:p1", "status": "working", "agent": "codex"},
        ])
        with (
            patch.object(relay, "clients", {ws}),
            patch.object(relay, "agent_refresh_clients", {ws}),
            patch.object(relay, "latest_agents_message", snapshot),
        ):
            await relay.send_requested_agent_refreshes()

            self.assertEqual(relay.agent_refresh_clients, set())

        self.assertEqual(ws.messages, [json.loads(snapshot)])

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
            ["push_config", "agents", "activity_history", "agents"],
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
                (True, json.dumps({"result": {"pane_id": "w2:p0", "workspace_id": "w2"}}), ""),
                (True, json.dumps({"result": {"move_result": {"pane": {"pane_id": "w2:p0", "tab_id": "w2:t2"}}}}), ""),
                (True, json.dumps({"result": {"agent": {"pane_id": "w2:p1", "workspace_id": "w2"}}}), ""),
                (True, "", ""),
                (True, "", ""),
            ]
            with (
                patch.object(relay.Path, "home", return_value=Path(cwd)),
                patch.object(relay, "load_agent_profiles", return_value={"test": {"id": "test", "label": "Test", "argv": ["test-agent"]}}),
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w7")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=command_results)) as run_command,
                patch.object(relay, "publish_activity", AsyncMock()),
                patch.object(relay.asyncio, "sleep", AsyncMock()),
            ):
                await relay.handle_agent_start_command(ws, msg)

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0], (
            "agent", "start", "mobile-test", "--cwd", cwd,
            "--workspace", "w7", "--no-focus", "--", "test-agent",
        ))
        self.assertNotIn("--literal task text", calls[0])
        self.assertEqual(calls[1], ("pane", "move", "w2:p0", "--new-tab", "--workspace", "w7", "--label", "mobile-test", "--no-focus"))
        self.assertEqual(calls[2], ("agent", "get", "mobile-test"))
        self.assertEqual(calls[3], ("pane", "send-text", "w2:p1", "--literal task text"))
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["data"]["pane_id"], "w2:p1")
        self.assertEqual(ws.messages[-1]["data"]["name"], "mobile-test")
        self.assertEqual(ws.messages[-1]["data"]["cwd"], cwd)

    async def test_agent_start_creates_a_space_when_the_working_directory_has_none(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            cwd = Path(temp_dir) / "new-project"
            cwd.mkdir()
            with (
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w8:p11", "workspace_id": "w8"}}), ""),
                    (True, json.dumps({"result": {"move_result": {"pane": {"pane_id": "w10:p1", "workspace_id": "w10"}}}}), ""),
                    (True, json.dumps({"result": {"agent": {"pane_id": "w10:p1", "workspace_id": "w10"}}}), ""),
                ])) as run_command,
            ):
                ok, _data, pane_id, placement_error, error = await relay.start_agent_in_new_tab(
                    {"id": "test", "label": "Test", "argv": ["test-agent"]},
                    "mobile-test",
                    cwd,
                )

        calls = [call.args for call in run_command.await_args_list]
        self.assertEqual(calls[0], (
            "agent", "start", "mobile-test", "--cwd", str(cwd),
            "--no-focus", "--", "test-agent",
        ))
        self.assertEqual(calls[1], (
            "pane", "move", "w8:p11", "--new-workspace", "--label", "new-project",
            "--tab-label", "mobile-test", "--no-focus",
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
                patch.object(relay, "load_agent_profiles", return_value={"codex": {"id": "codex", "label": "Codex", "argv": ["codex"]}}),
                patch.object(relay, "workspace_id_for_cwd", AsyncMock(return_value="w1")),
                patch.object(relay, "run_herdr_async_result", AsyncMock(side_effect=[
                    (True, json.dumps({"result": {"pane_id": "w1:p2", "workspace_id": "w1"}}), ""),
                    (True, json.dumps({"result": {"move_result": {"pane": {"pane_id": "w1:p2", "tab_id": "w1:t3"}}}}), ""),
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
        self.assertEqual(calls[0][0:2], ("agent", "start"))
        self.assertEqual(calls[1][0:3], ("pane", "move", "w1:p2"))
        self.assertEqual(calls[2][0:2], ("agent", "get"))
        self.assertEqual(calls[3], ("pane", "close", "w1:p1"))
        self.assertTrue(ws.messages[-1]["ok"])
        self.assertEqual(ws.messages[-1]["data"]["pane_id"], "w1:p2")
        self.assertEqual(ws.messages[-1]["data"]["cwd"], cwd)


if __name__ == "__main__":
    unittest.main()

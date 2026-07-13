#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["websockets>=14.0", "pywebpush>=2.0.0", "py-vapid>=1.9.2", "cryptography>=42.0.0"]
# ///
"""Herdr Mobile Relay server — polls local herdr and broadcasts to clients."""
import asyncio
import base64
import difflib
import hmac
import json
import os
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
from collections import deque
from pathlib import Path

try:
    from websockets.asyncio.server import serve
except ImportError:
    from websockets.server import serve
from websockets.exceptions import ConnectionClosed
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from py_vapid import Vapid
from pywebpush import WebPushException, webpush


def default_runtime_dir():
    relay_env = os.environ.get("HERDR_RELAY_ENV", "")
    if relay_env:
        return Path(relay_env).expanduser().parent
    plugin_config = os.environ.get("HERDR_PLUGIN_CONFIG_DIR", "")
    if plugin_config:
        return Path(plugin_config).expanduser()
    return Path(__file__).resolve().parent

def default_herdr_bin():
    for candidate in (
        shutil.which("herdr"),
        os.path.expanduser("~/.local/bin/herdr"),
        "/opt/homebrew/bin/herdr",
        "/usr/local/bin/herdr",
        "/home/linuxbrew/.linuxbrew/bin/herdr",
        "/home/linuxbrew/.linuxbrew/opt/herdr/bin/herdr",
    ):
        if candidate and os.path.exists(candidate):
            return candidate
    return "herdr"


HERDR = os.environ.get("HERDR_BIN") or default_herdr_bin()
WS_HOST = os.environ.get("HERDR_RELAY_HOST", "127.0.0.1")
WS_PORT = int(os.environ.get("HERDR_RELAY_PORT", "8375"))
POLL_INTERVAL = float(os.environ.get("HERDR_RELAY_POLL_INTERVAL", "2"))
IDLE_POLL_INTERVAL = max(POLL_INTERVAL, 15.0)
PLUGIN_PORT = int(os.environ.get("HERDR_RELAY_PLUGIN_PORT", "8376"))
AUTH_TOKEN = os.environ.get("HERDR_RELAY_TOKEN", "")  # Shared secret for public/browser relay auth
ALLOWED_ORIGINS = {
    origin.strip().rstrip("/")
    for origin in os.environ.get("HERDR_ALLOWED_ORIGINS", "").split(",")
    if origin.strip()
}
LOCAL_HOST = socket.gethostname().split(".")[0] or "local"
PUSH_DIR = default_runtime_dir() / "push"
PUSH_SUBSCRIPTIONS_FILE = PUSH_DIR / "subscriptions.json"
VAPID_PRIVATE_KEY_FILE = PUSH_DIR / "vapid_private.pem"
VAPID_SUBJECT = f"mailto:herdr-mobile-relay@{LOCAL_HOST}.local"
VAPID_PUBLIC_KEY = None
PUSH_LOCK = threading.RLock()
ACTIVITY_FILE = Path.home() / ".cache" / "herdr-mobile-relay" / "activity.jsonl"
ACTIVITY_MAX_ITEMS = 500
ACTIVITY_LOCK = threading.RLock()
CLAUDE_HISTORY_MAX_LINES = 500
CLAUDE_HISTORY_FOOTER_LINES = 6
CLAUDE_HISTORY_CAPTURE_INTERVAL = 4.0
CLAUDE_HISTORY_DIR = Path.home() / ".cache" / "herdr-mobile-relay" / "claude-history"
CLAUDE_HISTORY_SAVE_INTERVAL = 10.0
CLAUDE_HISTORY_MAX_AGE_DAYS = 7
UPLOAD_DIR = Path.home() / ".cache" / "herdr-mobile-relay" / "uploads"
UPLOAD_MAX_BYTES = 10 * 1024 * 1024
UPLOAD_MAX_AGE_DAYS = 7
WS_MAX_SIZE = max(16 * 1024 * 1024, UPLOAD_MAX_BYTES * 2 + 1024 * 1024)
WEB_DIR = Path(__file__).resolve().parent.parent / "web"
WEB_ASSET_CONTENT_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".json": "application/json; charset=utf-8",
    ".png": "image/png",
    ".svg": "image/svg+xml",
    ".webmanifest": "application/manifest+json; charset=utf-8",
}
IMAGE_MIME_EXTENSIONS = {
    "image/png": ".png",
    "image/jpeg": ".jpg",
    "image/webp": ".webp",
    "image/gif": ".gif",
    "image/heic": ".heic",
    "image/heif": ".heif",
}
AGENT_PROFILE_CANDIDATES = {
    "codex": "Codex",
    "claude": "Claude Code",
    "opencode": "OpenCode",
}
MACOS_PROTECTED_HOME_DIRECTORIES = {"Desktop", "Documents", "Downloads"}
RELAY_CAPABILITIES = ["directory_browser"]
# Version 1 is the existing positional approval protocol. Bump only when a
# WebSocket message changes incompatibly, together with APP_PROTOCOL_VERSION
# in web/index.html.
PROTOCOL_VERSION = 1
MUTATING_MESSAGE_TYPES = frozenset({
    "respond",
    "push_subscribe",
    "push_unsubscribe",
    "submit_prompt",
    "send_keys",
    "send_text",
    "agent_start",
    "agent_rename",
    "agent_stop",
    "agent_clear",
    "agent_restart",
    "acknowledge_pane",
    "upload_image",
})
POLL_WAKE_ACTIONS = frozenset({
    "acknowledge_pane",
    "agent_clear",
    "agent_rename",
    "agent_restart",
    "agent_start",
    "agent_stop",
    "approval",
    "keys",
    "prompt",
    "text",
})


def detect_relay_version():
    repo_dir = str(Path(__file__).resolve().parent)
    try:
        result = subprocess.run(
            ["git", "-C", repo_dir, "rev-parse", "--short", "HEAD"],
            capture_output=True, text=True, timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return "unknown"
    if result.returncode != 0 or not result.stdout.strip():
        return "unknown"

    version = result.stdout.strip()
    try:
        status = subprocess.run(
            ["git", "-C", repo_dir, "status", "--porcelain", "--untracked-files=normal"],
            capture_output=True, text=True, timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return version
    if status.returncode == 0 and status.stdout.strip():
        return f"{version}-dirty"
    return version


def client_protocol_version(msg):
    value = msg.get("protocol", 1)
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        return 0
    return value


def client_protocol_matches(msg):
    return client_protocol_version(msg) == PROTOCOL_VERSION


RELAY_VERSION = detect_relay_version()

TOOL_OPTIONS = ["yes, single permission", "trust, always allow", "no (tab to edit)"]
SUBAGENT_OPTIONS = ["approve all pending", "configure individually", "exit (cancel subagents)"]
ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")
CHROME_RE = re.compile(
    r"^[\s─━═_—│|◔◑◕●\s]+$"
    r"|Kiro\s[·•]"
    r"|esc to cancel"
    r"|type to queue"
    r"|^\s*[◔◑◕●]\s+(Shell|Bash)",
    re.IGNORECASE,
)
PROMPT_SKIP_RE = re.compile(
    r"^(?:"
    r"bash command"
    r"|do you want to proceed\??"
    r"|would you like to run\b.*"
    r"|environment:\s*\w+"
    r"|press enter to confirm\b.*"
    r"|esc to cancel\b.*"
    r")$",
    re.IGNORECASE,
)
MENU_OPTION_RE = re.compile(r"^\s*[❯›]?\s*(\d+)\.\s+(.+?)\s*$")
COMMAND_RE = re.compile(r"^\s*(?:[$>]|\u276f|\u203a)\s+(.+?)\s*$")

clients = set()
latest_agents_message = json.dumps(
    {"type": "agents", "agents": []},
    sort_keys=True,
    separators=(",", ":"),
)
last_broadcast_agents_message = None
last_statuses = {}
unseen_done_panes = set()
acknowledged_done_panes = set()
finished_notification_panes = set()
agent_activity_state = {}
agent_activity_initialized = False
agent_types = {}
claude_history_state = {}
claude_history_capture_times = {}
claude_history_save_times = {}
claude_history_inflight = set()
claude_history_pending_captures = set()
event_queue = asyncio.Queue()
poll_wakeup = asyncio.Event()


def run_herdr_result(*args):
    try:
        cmd = [HERDR, *args]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=15)
        if r.returncode != 0:
            error = (r.stderr or r.stdout or f"herdr exited with status {r.returncode}").strip()
            return False, "", error[:500]
        return True, r.stdout.strip(), ""
    except subprocess.TimeoutExpired:
        return False, "", "herdr command timed out"
    except Exception as exc:
        return False, "", str(exc)[:500] or "herdr command failed"


def run_herdr(*args):
    ok, output, _error = run_herdr_result(*args)
    return output if ok else None


async def run_herdr_async(*args):
    return await asyncio.to_thread(run_herdr, *args)


async def run_herdr_async_result(*args):
    return await asyncio.to_thread(run_herdr_result, *args)


def get_tabs():
    raw = run_herdr("tab", "list")
    if raw is None:
        return {}
    try:
        data = json.loads(raw)
        tabs = data.get("result", {}).get("tabs", [])
        return {t.get("tab_id"): t for t in tabs if t.get("tab_id")}
    except (json.JSONDecodeError, KeyError):
        return {}


def get_agents():
    raw = run_herdr("pane", "list")
    if raw is None:
        return None
    try:
        data = json.loads(raw)
        panes = data.get("result", {}).get("panes", [])
        tabs = get_tabs()
        agents = []
        for p in panes:
            if not p.get("agent"):
                continue
            raw_pane_id = p["pane_id"]
            tab_id = p.get("tab_id", "")
            tab = tabs.get(tab_id, {})
            scroll = p.get("scroll") if isinstance(p.get("scroll"), dict) else {}
            agents.append(
                {
                    "pane_id": raw_pane_id,
                    "raw_pane_id": raw_pane_id,
                    "terminal_id": p.get("terminal_id", ""),
                    "tab_id": tab_id,
                    "tab_label": tab.get("label", ""),
                    "tab_number": tab.get("number"),
                    "workspace_id": p.get("workspace_id", ""),
                    "agent": p.get("agent", ""),
                    "name": p.get("name") or p.get("label") or "",
                    "status": p.get("agent_status", "unknown"),
                    "_focused": bool(p.get("focused")),
                    "cwd": p.get("cwd", ""),
                    "project": os.path.basename(p.get("cwd", "")),
                    "host": LOCAL_HOST,
                    "_activity_fingerprint": (
                        p.get("agent_status", "unknown"),
                        p.get("revision"),
                        scroll.get("max_offset_from_bottom"),
                        p.get("foreground_cwd", ""),
                        p.get("cwd", ""),
                        p.get("name") or p.get("label") or "",
                    ),
                }
            )
        return agents
    except (json.JSONDecodeError, KeyError):
        return None


ATTENTION_STATUSES = {"working", "blocked"}
DONE_STATUSES = {"done", "complete", "completed", "finished", "success", "succeeded", "unread"}


def is_done_status(status):
    normalized = str(status or "").strip().lower().replace("_", "").replace("-", "").replace(" ", "")
    return normalized in DONE_STATUSES


def register_status_transition(pane_id, status, previous, focused=False):
    """Track Herdr's "finished, not yet viewed" state across API variants.

    Some snapshots expose only idle after completion while others briefly
    expose a done-like status. In both cases, completion remains done until the
    pane is focused in Herdr or viewed from the phone.
    """
    if status in ATTENTION_STATUSES:
        unseen_done_panes.discard(pane_id)
        acknowledged_done_panes.discard(pane_id)
    elif focused:
        unseen_done_panes.discard(pane_id)
        acknowledged_done_panes.add(pane_id)
    elif status == "idle" and previous in ATTENTION_STATUSES:
        acknowledged_done_panes.discard(pane_id)
        unseen_done_panes.add(pane_id)
    elif is_done_status(status) and previous in ATTENTION_STATUSES:
        acknowledged_done_panes.discard(pane_id)


def displayed_status(pane_id, status):
    if pane_id in acknowledged_done_panes and (status == "idle" or is_done_status(status)):
        return "idle"
    if status == "idle" and pane_id in unseen_done_panes:
        return "done"
    return status


def register_finished_notification(pane_id, status, previous):
    if status in ATTENTION_STATUSES:
        finished_notification_panes.discard(pane_id)
        return False
    if previous not in ATTENTION_STATUSES:
        return False
    if status != "idle" and not is_done_status(status):
        return False
    if pane_id in finished_notification_panes:
        return False
    finished_notification_panes.add(pane_id)
    return True


async def acknowledge_pane_viewed(pane_id):
    if pane_id not in agent_types:
        return False
    changed = pane_id in unseen_done_panes or pane_id not in acknowledged_done_panes
    unseen_done_panes.discard(pane_id)
    acknowledged_done_panes.add(pane_id)
    if not changed:
        return False
    wake_poll_loop()
    await broadcast({
        "type": "agent_update",
        "pane_id": pane_id,
        "raw_pane_id": pane_id,
        "status": "idle",
    })
    return changed


def now_millis():
    return int(time.time() * 1000)


def touch_agent_activity(pane_id, timestamp=None):
    if not pane_id:
        return int(now_millis() if timestamp is None else timestamp)
    updated_at = int(now_millis() if timestamp is None else timestamp)
    state = agent_activity_state.get(pane_id)
    if state:
        state["updated_at"] = updated_at
    else:
        agent_activity_state[pane_id] = {"fingerprint": None, "updated_at": updated_at}
    return updated_at


def stamp_agent_activity(agents, timestamp=None):
    global agent_activity_initialized
    updated_at = int(now_millis() if timestamp is None else timestamp)
    initial_snapshot = not agent_activity_initialized
    live_pane_ids = set()
    for agent in agents:
        pane_id = agent.get("pane_id", "")
        fingerprint = agent.pop("_activity_fingerprint", None)
        if not pane_id:
            agent["updated_at"] = updated_at
            continue
        live_pane_ids.add(pane_id)
        state = agent_activity_state.get(pane_id)
        if state is None:
            state = {
                "fingerprint": fingerprint,
                "updated_at": 0 if initial_snapshot else updated_at,
            }
            agent_activity_state[pane_id] = state
        elif state["fingerprint"] is None:
            state["fingerprint"] = fingerprint
        elif state["fingerprint"] != fingerprint:
            state["fingerprint"] = fingerprint
            state["updated_at"] = updated_at
        agent["updated_at"] = state["updated_at"]

    for pane_id in set(agent_activity_state) - live_pane_ids:
        del agent_activity_state[pane_id]
    agent_activity_initialized = True
    return agents


def normalized_history_line(line):
    return ANSI_RE.sub("", str(line or "")).replace("\r", "").rstrip()


def claude_sequence_match(previous, current):
    previous_keys = [normalized_history_line(line) for line in previous]
    current_keys = [normalized_history_line(line) for line in current]
    matcher = difflib.SequenceMatcher(None, previous_keys, current_keys, autojunk=False)
    candidates = []
    for match in matcher.get_matching_blocks():
        if match.size < 2:
            continue
        nonempty = sum(bool(value.strip()) for value in previous_keys[match.a:match.a + match.size])
        if nonempty >= 2:
            candidates.append(match)
    if not candidates:
        return None
    # Tie-break on the latest history anchor: the frame is the terminal's most
    # recent content, so among equal matches the one nearest the history tail
    # is the true alignment; repeated session content otherwise pulls the
    # anchor toward stale early occurrences.
    return max(candidates, key=lambda match: (match.size, match.a))


def claude_tail_overlap(previous, current):
    """Largest k where the last k history lines equal the first k lines of the
    new frame — the invariant of scrolling terminal output. Anchoring at the
    tail is immune to content that repeats earlier in history, which misleads
    the fuzzy matcher (its ranking prefers early anchors, truncating or
    freezing long histories on repetitive sessions)."""
    previous_keys = [normalized_history_line(line) for line in previous]
    current_keys = [normalized_history_line(line) for line in current]
    for k in range(min(len(previous_keys), len(current_keys)), 1, -1):
        if previous_keys[-k:] != current_keys[:k]:
            continue
        if sum(bool(key.strip()) for key in current_keys[:k]) >= 2:
            return k
    return 0


def split_claude_snapshot(snapshot):
    if len(snapshot) <= CLAUDE_HISTORY_FOOTER_LINES * 2:
        return snapshot, []
    return snapshot[:-CLAUDE_HISTORY_FOOTER_LINES], snapshot[-CLAUDE_HISTORY_FOOTER_LINES:]


def claude_history_content(state, limit=CLAUDE_HISTORY_MAX_LINES):
    try:
        limit = min(max(int(limit), 1), CLAUDE_HISTORY_MAX_LINES)
    except (TypeError, ValueError):
        limit = CLAUDE_HISTORY_MAX_LINES
    combined = state.get("history", []) + state.get("footer", [])
    return "\n".join(combined[-limit:])


def claude_history_file(pane_id):
    return CLAUDE_HISTORY_DIR / (re.sub(r"[^A-Za-z0-9-]", "_", str(pane_id)) + ".json")


def load_claude_history_state(pane_id):
    """In-memory state, lazily restored from disk so relay restarts keep the
    stitched history instead of starting over from one viewport."""
    state = claude_history_state.get(pane_id)
    if state is not None:
        return state
    try:
        raw = json.loads(claude_history_file(pane_id).read_text())
    except (OSError, ValueError):
        return None
    if not isinstance(raw, dict) or not isinstance(raw.get("history"), list):
        return None

    def line_list(key):
        value = raw.get(key)
        return [str(line) for line in value] if isinstance(value, list) else []

    state = {
        "history": line_list("history"),
        "footer": line_list("footer"),
        "snapshot": line_list("snapshot"),
        "stale_refusals": 0,
    }
    claude_history_state[pane_id] = state
    return state


def save_claude_history_state(pane_id, force=False):
    state = claude_history_state.get(pane_id)
    if state is None:
        return
    now = time.monotonic()
    if not force and now - claude_history_save_times.get(pane_id, 0.0) < CLAUDE_HISTORY_SAVE_INTERVAL:
        return
    claude_history_save_times[pane_id] = now
    path = claude_history_file(pane_id)
    tmp_path = path.with_suffix(".tmp")
    try:
        ensure_private_dir(CLAUDE_HISTORY_DIR)
        tmp_path.write_text(json.dumps({
            "history": state["history"],
            "footer": state["footer"],
            "snapshot": state["snapshot"],
        }))
        os.chmod(tmp_path, 0o600)
        os.replace(tmp_path, path)
    except OSError:
        pass


def discard_claude_history_state(pane_id):
    claude_history_state.pop(pane_id, None)
    claude_history_save_times.pop(pane_id, None)
    try:
        claude_history_file(pane_id).unlink(missing_ok=True)
    except OSError:
        pass


def merge_claude_history(pane_id, content, limit=CLAUDE_HISTORY_MAX_LINES):
    try:
        limit = min(max(int(limit), 1), CLAUDE_HISTORY_MAX_LINES)
    except (TypeError, ValueError):
        limit = CLAUDE_HISTORY_MAX_LINES
    current = str(content or "").splitlines()
    if not current:
        return ""
    current_body, current_footer = split_claude_snapshot(current)

    state = load_claude_history_state(pane_id)
    if state is None:
        state = {
            "history": current_body,
            "footer": current_footer,
            "snapshot": current,
            "stale_refusals": 0,
        }
        claude_history_state[pane_id] = state
    else:
        history = state["history"]
        if state["snapshot"] != current or state.get("stale_refusals"):
            overlap = claude_tail_overlap(history, current_body)
            match = None if overlap else claude_sequence_match(history, current_body)
            if overlap:
                if len(current_body) > overlap:
                    state["history"] = history + current_body[overlap:]
                state["stale_refusals"] = 0
            elif match:
                history_end = match.a + match.size
                current_end = match.b + match.size
                current_suffix = current_body[current_end:]
                history_tail = len(history) - history_end
                if not current_suffix:
                    # Nothing beyond the match: a scrolled-up viewport
                    # re-showing known content. Leave history untouched.
                    state["stale_refusals"] = 0
                elif history_tail >= len(current_body):
                    # A terminal rewrite can only touch lines that fit on one
                    # screen. A match this deep in history means repeated
                    # session content misled the matcher and this frame is
                    # genuinely new output that repeats old lines: append it
                    # whole rather than rebasing real history away.
                    state["history"].extend(current_body)
                    state["stale_refusals"] = 0
                elif history_tail <= 3:
                    state["history"] = history[:history_end] + current_suffix
                    state["stale_refusals"] = 0
                else:
                    # A divergent tail bounded by one screen is either a
                    # scrolled-up viewport (transient) or the terminal
                    # rewriting recent lines — e.g. Claude Code collapsing an
                    # approval box once answered (permanent). Refuse once to
                    # shield scrolls, then rebase so history follows the
                    # rewrite instead of freezing at a stale tail.
                    refusals = state.get("stale_refusals", 0) + 1
                    if refusals >= 2:
                        state["history"] = history[:history_end] + current_suffix
                        refusals = 0
                    state["stale_refusals"] = refusals
            elif current_body and state["snapshot"] != current:
                state["history"].extend(current_body)
                state["stale_refusals"] = 0
            state["footer"] = current_footer
            state["snapshot"] = current

    history_capacity = max(0, CLAUDE_HISTORY_MAX_LINES - len(state["footer"]))
    state["history"] = state["history"][-history_capacity:] if history_capacity else []
    save_claude_history_state(pane_id)
    return claude_history_content(state, limit)


async def capture_claude_history(pane_id):
    claude_history_inflight.add(pane_id)
    try:
        content = await run_herdr_async(
            "pane", "read", pane_id,
            "--lines", str(CLAUDE_HISTORY_MAX_LINES),
            "--source", "recent-unwrapped",
            "--format", "ansi",
        )
        if content and "claude" in agent_types.get(pane_id, ""):
            merge_claude_history(pane_id, content)
    finally:
        claude_history_inflight.discard(pane_id)


def schedule_claude_history_capture(agent, timestamp=None, force=False):
    """force marks the capture as must-run (end of a work cycle: the final
    frame would otherwise be lost forever once the pane sits idle). Forced
    captures bypass the interval gate, and survive an in-flight capture as a
    pending retry instead of being dropped."""
    pane_id = agent.get("pane_id", "")
    if not pane_id or "claude" not in str(agent.get("agent") or "").lower():
        return
    if force:
        claude_history_pending_captures.add(pane_id)
    if pane_id in claude_history_inflight:
        return
    now = time.monotonic() if timestamp is None else float(timestamp)
    last_capture = claude_history_capture_times.get(pane_id, 0.0)
    if (
        pane_id not in claude_history_pending_captures
        and now - last_capture < CLAUDE_HISTORY_CAPTURE_INTERVAL
    ):
        return
    claude_history_pending_captures.discard(pane_id)
    claude_history_capture_times[pane_id] = now
    asyncio.create_task(capture_claude_history(pane_id))


def read_pane(pane_id):
    raw = run_herdr("pane", "read", pane_id, "--lines", "20", "--source", "recent-unwrapped")
    if raw is None:
        return ""
    lines = []
    for line in raw.splitlines():
        clean = clean_pane_line(line)
        if clean and not CHROME_RE.search(clean) and not PROMPT_SKIP_RE.search(clean):
            lines.append(line)
    return "\n".join(lines[-12:])


def clean_pane_line(line):
    clean = ANSI_RE.sub("", line).strip()
    clean = re.sub(r"^[│|]\s*", "", clean)
    clean = re.sub(r"\s*[│|]$", "", clean)
    return clean.strip()


def detect_options(text):
    runs = []
    current = []
    expected = 1

    for line in text.splitlines():
        match = MENU_OPTION_RE.match(clean_pane_line(line))
        if not match:
            if current:
                runs.append(current)
                current = []
                expected = 1
            continue

        number = int(match.group(1))
        label = match.group(2).strip()
        if number == 1:
            if current:
                runs.append(current)
            current = [label]
            expected = 2
        elif current and number == expected:
            current.append(label)
            expected += 1
        else:
            if current:
                runs.append(current)
            current = []
            expected = 1

    if current:
        runs.append(current)

    menus = [run for run in runs if len(run) >= 2]
    if menus:
        return menus[-1]

    lower = text.lower()
    if "yes, single permission" in lower:
        return TOOL_OPTIONS
    if "approve all pending" in lower:
        return SUBAGENT_OPTIONS
    return None


def detect_command_context(text):
    command = ""
    fallback = ""
    for line in text.splitlines():
        clean = clean_pane_line(line)
        if (
            not clean
            or MENU_OPTION_RE.match(clean)
            or CHROME_RE.search(clean)
            or PROMPT_SKIP_RE.search(clean)
        ):
            continue
        match = COMMAND_RE.match(clean)
        if match:
            command = match.group(1).strip()
            continue
        fallback = clean
    return (command or fallback)[:240]


def upload_extension(filename, mime):
    mime = (mime or "").split(";", 1)[0].strip().lower()
    if mime in IMAGE_MIME_EXTENSIONS:
        return IMAGE_MIME_EXTENSIONS[mime]
    suffix = Path(filename or "").suffix.lower()
    return suffix if suffix in set(IMAGE_MIME_EXTENSIONS.values()) else ".img"


def safe_upload_stem(filename):
    stem = Path(filename or "image").stem
    stem = re.sub(r"[^A-Za-z0-9._-]+", "-", stem).strip(".-")
    return stem[:60] or "image"


def store_uploaded_image(filename, mime, data):
    if not isinstance(data, str) or not data:
        return False, "Missing image data", None
    mime = (mime or "").split(";", 1)[0].strip().lower()
    if mime and not mime.startswith("image/"):
        return False, "Only image uploads are supported", None

    payload = data
    if data.startswith("data:"):
        header, sep, payload = data.partition(",")
        if not sep or ";base64" not in header:
            return False, "Image data must be base64 encoded", None
        header_mime = header[5:].split(";", 1)[0].strip().lower()
        if header_mime:
            mime = header_mime
    if mime and not mime.startswith("image/"):
        return False, "Only image uploads are supported", None

    try:
        content = base64.b64decode(payload, validate=True)
    except Exception:
        return False, "Invalid image encoding", None
    if len(content) > UPLOAD_MAX_BYTES:
        mb = UPLOAD_MAX_BYTES // (1024 * 1024)
        return False, f"Image is larger than {mb} MB", None

    ensure_private_dir(UPLOAD_DIR)
    ext = upload_extension(filename, mime)
    stem = safe_upload_stem(filename)
    path = UPLOAD_DIR / f"{time.strftime('%Y%m%d-%H%M%S')}-{secrets.token_hex(4)}-{stem}{ext}"
    path.write_bytes(content)
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass
    return True, "", str(path)


def ensure_private_dir(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        os.chmod(path, 0o700)
    except OSError:
        pass


def prune_uploads():
    """Delete uploaded images past UPLOAD_MAX_AGE_DAYS; they only need to live
    long enough for the agent to read them."""
    cutoff = time.time() - UPLOAD_MAX_AGE_DAYS * 86400
    removed = 0
    try:
        entries = list(UPLOAD_DIR.iterdir())
    except OSError:
        return removed
    for entry in entries:
        try:
            if entry.is_file() and entry.stat().st_mtime < cutoff:
                entry.unlink()
                removed += 1
        except OSError:
            continue
    if removed:
        print(f"Pruned {removed} upload(s) older than {UPLOAD_MAX_AGE_DAYS} days")
    return removed


def prune_claude_history_files():
    """Remove history files for panes that are gone. Files of live panes are
    always exempt — idle panes are not recaptured, so their files age without
    being stale — and nothing is pruned before the first pane inventory, so a
    slow start cannot delete files lazy restoration has not read yet."""
    if not agent_activity_initialized:
        return
    live_files = {claude_history_file(pane_id).name for pane_id in agent_types}
    cutoff = time.time() - CLAUDE_HISTORY_MAX_AGE_DAYS * 86400
    try:
        entries = list(CLAUDE_HISTORY_DIR.iterdir())
    except OSError:
        return
    for entry in entries:
        try:
            if entry.name in live_files:
                continue
            if entry.is_file() and entry.stat().st_mtime < cutoff:
                entry.unlink()
        except OSError:
            continue


async def prune_uploads_loop():
    while True:
        await asyncio.to_thread(prune_uploads)
        # Give the first pane inventory a chance to finish so the history
        # prune knows which panes are live; it refuses to run before that.
        for _ in range(30):
            if agent_activity_initialized:
                break
            await asyncio.sleep(POLL_INTERVAL)
        await asyncio.to_thread(prune_claude_history_files)
        await asyncio.sleep(86400)


def compact_text(value, limit=240):
    return re.sub(r"\s+", " ", str(value or "")).strip()[:limit]


def load_agent_profiles():
    profiles = {}
    for profile_id, label in AGENT_PROFILE_CANDIDATES.items():
        executable = shutil.which(profile_id)
        if not executable:
            continue
        profiles[profile_id] = {
            "id": profile_id,
            "label": label,
            "argv": [executable],
        }
    return profiles


def directory_is_browsable(path):
    try:
        with os.scandir(path) as entries:
            next(entries, None)
        return True
    except OSError:
        return False


def list_project_directory(value=""):
    try:
        home = Path.home().resolve()
    except OSError:
        return None, "Home directory could not be resolved"

    current, error = resolve_agent_cwd(value or str(home))
    if error:
        return None, error

    directories = []
    try:
        children = list(current.iterdir())
    except PermissionError:
        if sys.platform == "darwin":
            return None, "macOS denied access to this directory"
        return None, "Permission denied while reading this directory"
    except OSError:
        return None, "Working directory could not be read"

    for child in children:
        if child.name.startswith("."):
            continue
        try:
            resolved = child.resolve()
        except OSError:
            continue
        if not resolved.is_dir() or not resolved.is_relative_to(home):
            continue
        if not os.access(resolved, os.R_OK | os.X_OK):
            continue
        needs_macos_privacy_probe = (
            sys.platform == "darwin"
            and current == home
            and child.name in MACOS_PROTECTED_HOME_DIRECTORIES
        )
        if needs_macos_privacy_probe and not directory_is_browsable(resolved):
            continue
        directories.append({"name": child.name, "path": str(resolved)})

    relative = current.relative_to(home)
    current_label = "~" if current == home else f"~/{relative.as_posix()}"
    parent = "" if current == home else str(current.parent)
    return {
        "current": {"path": str(current), "label": current_label},
        "parent": parent,
        "directories": sorted(directories, key=lambda item: (item["name"].casefold(), item["name"])),
    }, ""


def resolve_agent_cwd(value):
    cwd = Path(os.path.expandvars(str(value or "").strip())).expanduser()
    if not cwd.is_absolute() or not cwd.is_dir():
        return None, "Working directory must be an existing absolute directory"
    try:
        cwd = cwd.resolve()
    except OSError:
        return None, "Working directory could not be resolved"
    try:
        home = Path.home().resolve()
    except OSError:
        return None, "Home directory could not be resolved"
    if cwd.is_relative_to(home):
        return cwd, ""
    return None, "Working directory must be inside the current user's home directory"


def load_activity(limit=ACTIVITY_MAX_ITEMS):
    try:
        limit = int(limit or ACTIVITY_MAX_ITEMS)
    except (TypeError, ValueError):
        limit = ACTIVITY_MAX_ITEMS
    limit = max(1, min(limit, ACTIVITY_MAX_ITEMS))
    with ACTIVITY_LOCK:
        try:
            with ACTIVITY_FILE.open(encoding="utf-8") as activity_file:
                lines = deque(activity_file, maxlen=limit)
        except OSError:
            return []
    entries = []
    for line in lines:
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(entry, dict):
            entries.append(entry)
    return entries


def trim_activity_file():
    try:
        if ACTIVITY_FILE.stat().st_size < 2 * 1024 * 1024:
            return
    except OSError:
        return
    entries = load_activity(ACTIVITY_MAX_ITEMS)
    tmp = ACTIVITY_FILE.with_suffix(".jsonl.tmp")
    tmp.write_text("".join(json.dumps(entry, separators=(",", ":")) + "\n" for entry in entries))
    try:
        os.chmod(tmp, 0o600)
    except OSError:
        pass
    tmp.replace(ACTIVITY_FILE)


def record_activity(kind, status, summary, pane_id="", agent="", project="", request_id="", details=None):
    entry = {
        "id": secrets.token_urlsafe(12),
        "timestamp": now_millis(),
        "kind": compact_text(kind, 40),
        "status": compact_text(status, 24),
        "summary": compact_text(summary, 240),
        "host": LOCAL_HOST,
        "pane_id": compact_text(pane_id, 120),
        "agent": compact_text(agent, 80),
        "project": compact_text(project, 120),
        "request_id": compact_text(request_id, 120),
    }
    if isinstance(details, dict):
        entry["details"] = {
            compact_text(key, 40): compact_text(value, 240)
            for key, value in details.items()
            if value is not None and compact_text(key, 40)
        }
    with ACTIVITY_LOCK:
        try:
            ensure_private_dir(ACTIVITY_FILE.parent)
            with ACTIVITY_FILE.open("a", encoding="utf-8") as activity_file:
                activity_file.write(json.dumps(entry, separators=(",", ":")) + "\n")
            try:
                os.chmod(ACTIVITY_FILE, 0o600)
            except OSError:
                pass
            trim_activity_file()
        except OSError as exc:
            print(f"Activity history write failed: {exc}")
    return entry


async def publish_activity(*args, **kwargs):
    entry = await asyncio.to_thread(record_activity, *args, **kwargs)
    if entry.get("pane_id"):
        touch_agent_activity(entry["pane_id"], entry["timestamp"])
    await broadcast({"type": "activity", "activity": entry})
    return entry


def ensure_vapid_public_key():
    global VAPID_PUBLIC_KEY
    if VAPID_PUBLIC_KEY:
        return VAPID_PUBLIC_KEY
    with PUSH_LOCK:
        if VAPID_PUBLIC_KEY:
            return VAPID_PUBLIC_KEY
        ensure_private_dir(PUSH_DIR)
        ensure_private_dir(VAPID_PRIVATE_KEY_FILE.parent)
        vapid = Vapid.from_file(str(VAPID_PRIVATE_KEY_FILE))
        try:
            os.chmod(VAPID_PRIVATE_KEY_FILE, 0o600)
        except OSError:
            pass
        public_bytes = vapid.public_key.public_bytes(Encoding.X962, PublicFormat.UncompressedPoint)
        VAPID_PUBLIC_KEY = base64.urlsafe_b64encode(public_bytes).rstrip(b"=").decode()
        return VAPID_PUBLIC_KEY


def load_push_subscriptions():
    with PUSH_LOCK:
        try:
            data = json.loads(PUSH_SUBSCRIPTIONS_FILE.read_text())
            if isinstance(data, dict) and isinstance(data.get("subscriptions"), list):
                return data["subscriptions"]
        except (OSError, json.JSONDecodeError):
            pass
        return []


def save_push_subscriptions(subscriptions):
    with PUSH_LOCK:
        ensure_private_dir(PUSH_DIR)
        payload = {"subscriptions": subscriptions}
        tmp = PUSH_SUBSCRIPTIONS_FILE.with_suffix(".json.tmp")
        tmp.write_text(json.dumps(payload, indent=2, sort_keys=True))
        try:
            os.chmod(tmp, 0o600)
        except OSError:
            pass
        tmp.replace(PUSH_SUBSCRIPTIONS_FILE)


def push_subscription_endpoint(subscription):
    if not isinstance(subscription, dict):
        return ""
    endpoint = subscription.get("endpoint", "")
    return endpoint if isinstance(endpoint, str) else ""


def valid_push_subscription(subscription):
    endpoint = push_subscription_endpoint(subscription)
    keys = subscription.get("keys", {}) if isinstance(subscription, dict) else {}
    return bool(endpoint and isinstance(keys, dict) and keys.get("p256dh") and keys.get("auth"))


def store_push_subscription(
    subscription,
    user_agent="",
    client_id="",
    replace_endpoints=None,
    notify_finished=False,
):
    if not valid_push_subscription(subscription):
        return False
    endpoint = push_subscription_endpoint(subscription)
    client_id = client_id[:120] if isinstance(client_id, str) else ""
    stale_endpoints = {
        e for e in (replace_endpoints or [])
        if isinstance(e, str) and e
    }
    stale_endpoints.add(endpoint)
    with PUSH_LOCK:
        subscriptions = [
            s for s in load_push_subscriptions()
            if (
                push_subscription_endpoint(s.get("subscription", {})) not in stale_endpoints
                and not (client_id and s.get("client_id") == client_id)
            )
        ]
        subscriptions.append({
            "subscription": subscription,
            "client_id": client_id,
            "user_agent": user_agent[:240] if isinstance(user_agent, str) else "",
            "notify_finished": notify_finished is True,
        })
        save_push_subscriptions(subscriptions)
    return True


def remove_push_subscriptions(endpoints):
    if not endpoints:
        return
    stale = set(endpoints)
    with PUSH_LOCK:
        subscriptions = [
            s for s in load_push_subscriptions()
            if push_subscription_endpoint(s.get("subscription", {})) not in stale
        ]
        save_push_subscriptions(subscriptions)


def remove_push_subscription_records(endpoints=None, client_id=""):
    endpoints = {
        e for e in (endpoints or [])
        if isinstance(e, str) and e
    }
    client_id = client_id[:120] if isinstance(client_id, str) else ""
    if not endpoints and not client_id:
        return False
    with PUSH_LOCK:
        subscriptions = [
            s for s in load_push_subscriptions()
            if (
                push_subscription_endpoint(s.get("subscription", {})) not in endpoints
                and not (client_id and s.get("client_id") == client_id)
            )
        ]
        save_push_subscriptions(subscriptions)
    return True


def push_subscription_label(subscription):
    endpoint = push_subscription_endpoint(subscription)
    try:
        parsed = urllib.parse.urlparse(endpoint)
        return parsed.netloc or "unknown endpoint"
    except Exception:
        return "unknown endpoint"


def notification_target_url(host, pane_id, notification_id="", action="", index=None, total=None):
    base_target = {
        "host": host,
        "pane_id": pane_id,
        "notification_id": notification_id,
    }
    if action:
        base_target.update({"action": action, "index": index, "total": total})
    encoded = urllib.parse.quote(json.dumps(base_target, separators=(",", ":")))
    return f"./#notify={encoded}"


def push_payload(blocked_msg):
    project = blocked_msg.get("project") or blocked_msg.get("agent") or "agent"
    host = blocked_msg.get("host") or LOCAL_HOST
    pane_id = blocked_msg.get("pane_id", "")
    event_id = blocked_msg.get("event_id", "")
    command = blocked_msg.get("command") or "Agent needs approval"
    options = blocked_msg.get("options") if isinstance(blocked_msg.get("options"), list) else TOOL_OPTIONS
    total = max(2, len(options))

    return {
        "title": f"{project} blocked",
        "body": f"{command} · {host}",
        "tag": f"herdr-{host}-{pane_id}",
        "url": notification_target_url(host, pane_id, event_id),
        "actions": [{"action": "approve", "title": "Approve once"}],
        "action_urls": {
            "approve": notification_target_url(host, pane_id, event_id, "approve", 0, total),
        },
    }


def finished_push_payload(agent):
    project = agent.get("project") or agent.get("agent") or "Agent"
    agent_name = agent.get("agent") or "Agent"
    host = agent.get("host") or LOCAL_HOST
    pane_id = agent.get("pane_id", "")
    event_id = f"finished-{secrets.token_urlsafe(8)}"
    return {
        "title": f"{project} finished",
        "body": f"{agent_name} completed · {host}",
        "tag": f"herdr-finished-{host}-{pane_id}",
        "url": notification_target_url(host, pane_id, event_id),
        "action_urls": {},
        "actions": [],
    }


def send_webpush_payload(payload, include_subscription=None):
    subscriptions = load_push_subscriptions()
    if not subscriptions:
        return
    data = json.dumps(payload)
    stale = []
    for item in subscriptions:
        if include_subscription and not include_subscription(item):
            continue
        subscription = item.get("subscription", {})
        try:
            webpush(
                subscription_info=subscription,
                data=data,
                vapid_private_key=str(VAPID_PRIVATE_KEY_FILE),
                vapid_claims={"sub": VAPID_SUBJECT},
                ttl=300,
                timeout=10,
            )
        except WebPushException as exc:
            response = getattr(exc, "response", None)
            if response is not None and response.status_code in {401, 403, 404, 410}:
                print(f"Pruning stale Web Push subscription for {push_subscription_label(subscription)}: HTTP {response.status_code}")
                stale.append(push_subscription_endpoint(subscription))
            elif response is not None:
                print(f"Web Push failed for {push_subscription_label(subscription)}: HTTP {response.status_code}")
            else:
                print(f"Web Push failed for {push_subscription_label(subscription)}: {exc}")
        except Exception:
            print(f"Web Push failed for {push_subscription_label(subscription)}")
    remove_push_subscriptions(stale)


def send_webpush_notifications(blocked_msg):
    send_webpush_payload(push_payload(blocked_msg))


def send_finished_webpush_notifications(agent):
    send_webpush_payload(
        finished_push_payload(agent),
        lambda item: item.get("notify_finished") is True,
    )


async def push_blocked(blocked_msg):
    await asyncio.to_thread(send_webpush_notifications, blocked_msg)


async def push_finished(agent):
    await asyncio.to_thread(send_finished_webpush_notifications, agent)


async def publish_blocked(blocked_msg):
    blocked_msg = dict(blocked_msg)
    blocked_msg.setdefault("event_id", secrets.token_urlsafe(12))
    activity = await publish_activity(
        "blocked",
        "attention",
        blocked_msg.get("command") or "Agent needs approval",
        pane_id=blocked_msg.get("pane_id", ""),
        agent=blocked_msg.get("agent", ""),
        project=blocked_msg.get("project", ""),
        details={"event_id": blocked_msg["event_id"]},
    )
    blocked_msg["updated_at"] = activity["timestamp"]
    await broadcast(blocked_msg)
    asyncio.create_task(push_blocked(blocked_msg))


async def publish_agent_blocked(agent):
    pane_id = agent.get("pane_id", "")
    content = await asyncio.to_thread(read_pane, pane_id) or agent.get("prompt", "Agent is blocked")
    await publish_blocked({
        "type": "blocked",
        "pane_id": pane_id,
        "agent": agent.get("agent", ""),
        "project": agent.get("project", ""),
        "host": agent.get("host", LOCAL_HOST),
        "tab_id": agent.get("tab_id", ""),
        "tab_label": agent.get("tab_label", ""),
        "tab_number": agent.get("tab_number"),
        "workspace_id": agent.get("workspace_id", ""),
        "prompt": content[:500],
        "command": detect_command_context(content),
        "options": detect_options(content) or TOOL_OPTIONS,
    })


async def publish_agent_status(agent, status):
    await publish_activity(
        "agent_status",
        status or "unknown",
        f"Agent is now {status or 'unknown'}",
        pane_id=agent.get("pane_id", ""),
        agent=agent.get("agent", ""),
        project=agent.get("project", ""),
    )


def respond_keys(index, total=None):
    """Keys that select option `index` in an agent's approval menu.

    Codex and Claude Code both render approvals as an arrow-navigable list with
    option 1 pre-highlighted, so plain letters like "y" are ignored. Selecting by
    position works across both: Enter confirms the first (Yes) option, Esc cancels
    the last (No/exit) option, and Down×n + Enter reaches anything in between.
    """
    if index <= 0:
        return ["Enter"]
    if isinstance(total, int) and index >= total - 1:
        return ["Escape"]
    return ["Down"] * index + ["Enter"]


async def broadcast(msg):
    await broadcast_serialized(json.dumps(msg))


async def broadcast_serialized(data):
    dead = set()
    for ws in list(clients):
        try:
            await ws.send(data)
        except Exception:
            dead.add(ws)
    clients.difference_update(dead)


def agents_message(agents):
    """Serialize the exact authoritative payload sent to phone clients.

    Herdr's pane ordering is not part of the phone UI contract, so sort by the
    stable pane id before comparing snapshots. Dict keys and separators are
    canonical too, making equality independent of construction order.
    """
    ordered = sorted(agents, key=lambda agent: str(agent.get("pane_id", "")))
    return json.dumps(
        {"type": "agents", "agents": ordered},
        sort_keys=True,
        separators=(",", ":"),
    )


async def broadcast_agents_if_changed(agents):
    """Cache every current snapshot but send it only when clients would see a change."""
    global latest_agents_message, last_broadcast_agents_message
    message = agents_message(agents)
    latest_agents_message = message
    if message == last_broadcast_agents_message:
        return False
    last_broadcast_agents_message = message
    await broadcast_serialized(message)
    return True


async def send_latest_agents(ws):
    """Give a new client the latest full state, including a concurrent update."""
    while True:
        message = latest_agents_message
        try:
            await ws.send(message)
        except Exception:
            return False
        if message == latest_agents_message:
            return True


def wake_poll_loop():
    poll_wakeup.set()


def poll_interval_for(agents):
    if not agents:
        return POLL_INTERVAL
    for agent in agents:
        status = str(agent.get("status") or "unknown").strip().lower()
        if status != "idle" and not is_done_status(status):
            return POLL_INTERVAL
    return IDLE_POLL_INTERVAL


async def wait_for_next_poll(agents):
    try:
        await asyncio.wait_for(poll_wakeup.wait(), timeout=poll_interval_for(agents))
    except asyncio.TimeoutError:
        pass


async def poll_loop():
    while True:
        # Clear before reading so a hook or command arriving during the refresh
        # remains set and makes the next wait return immediately.
        poll_wakeup.clear()
        agents = await asyncio.to_thread(get_agents)
        if agents is None:
            await wait_for_next_poll(None)
            continue
        stamp_agent_activity(agents)
        live_pane_ids = {a["pane_id"] for a in agents}
        raw_statuses = {}
        finished_agents = []
        for a in agents:
            pid = a["pane_id"]
            raw_status = a["status"]
            raw_statuses[pid] = raw_status
            previous_status = last_statuses.get(pid)
            if register_finished_notification(pid, raw_status, previous_status):
                finished_agents.append(dict(a))
            register_status_transition(pid, raw_status, previous_status, a.pop("_focused", False))
            a["status"] = displayed_status(pid, raw_status)
        unseen_done_panes.intersection_update(live_pane_ids)
        acknowledged_done_panes.intersection_update(live_pane_ids)
        finished_notification_panes.intersection_update(live_pane_ids)
        agent_types.clear()
        agent_types.update({a["pane_id"]: str(a.get("agent") or "").lower() for a in agents})
        for pane_id in set(claude_history_state) - live_pane_ids:
            discard_claude_history_state(pane_id)
        for pane_id in set(claude_history_capture_times) - live_pane_ids:
            claude_history_capture_times.pop(pane_id, None)
        claude_history_pending_captures.intersection_update(live_pane_ids)
        await broadcast_agents_if_changed(agents)
        for agent in finished_agents:
            asyncio.create_task(push_finished(agent))
        for pane_id in set(last_statuses) - live_pane_ids:
            del last_statuses[pane_id]
        if agents:
            for a in agents:
                pid = a["pane_id"]
                status = raw_statuses[pid]
                previous_status = last_statuses.get(pid)
                finished_now = previous_status in ATTENTION_STATUSES and status not in ATTENTION_STATUSES
                if (
                    finished_now
                    or status in {"working", "blocked"}
                    or previous_status != status
                    or pid in claude_history_pending_captures
                ):
                    schedule_claude_history_capture(a, force=finished_now)
                if status == "blocked" and previous_status != "blocked":
                    await publish_agent_blocked(a)
                elif previous_status == "blocked" and status != "blocked":
                    await publish_agent_status(a, status)
                last_statuses[pid] = status
        await wait_for_next_poll(agents)


async def event_push():
    while True:
        event = await event_queue.get()
        raw_pane_id = event.get("pane_id", "")
        status = event.get("status", "")
        host = event.get("host", LOCAL_HOST)
        previous_status = last_statuses.get(raw_pane_id) if raw_pane_id else None
        was_blocked = previous_status == "blocked"
        notify_finished = False
        if raw_pane_id and status:
            notify_finished = register_finished_notification(raw_pane_id, status, previous_status)
            register_status_transition(raw_pane_id, status, previous_status)
            last_statuses[raw_pane_id] = status
            status = displayed_status(raw_pane_id, status)

        if status == "blocked" and raw_pane_id and not was_blocked:
            await publish_agent_blocked({**event, "pane_id": raw_pane_id, "host": host})
        elif raw_pane_id and was_blocked and status and status != "blocked":
            await publish_agent_status({**event, "pane_id": raw_pane_id}, status)

        if raw_pane_id and event.get("type") == "agent_event":
            updated_at = touch_agent_activity(raw_pane_id)
            schedule_claude_history_capture({
                "pane_id": raw_pane_id,
                "agent": event.get("agent", ""),
                "status": status,
            }, force=previous_status in ATTENTION_STATUSES and status not in ATTENTION_STATUSES)
            await broadcast({
                "type": "agent_update",
                "pane_id": raw_pane_id,
                "raw_pane_id": raw_pane_id,
                "tab_id": event.get("tab_id", ""),
                "tab_label": event.get("tab_label", ""),
                "tab_number": event.get("tab_number"),
                "workspace_id": event.get("workspace_id", ""),
                "agent": event.get("agent", ""),
                "status": status,
                "cwd": event.get("cwd", ""),
                "project": event.get("project", ""),
                "host": host,
                "updated_at": updated_at,
            })
        if notify_finished:
            asyncio.create_task(push_finished({**event, "pane_id": raw_pane_id, "host": host}))


def header_value(request, name):
    for key, value in request.headers.raw_items():
        if key.lower() == name.lower():
            return value
    return None


def query_value(path, name):
    if "?" not in (path or ""):
        return None
    _, qs = path.split("?", 1)
    params = urllib.parse.parse_qs(qs)
    return params.get(name, [None])[0]


def request_token(request):
    authorization = header_value(request, "authorization")
    if authorization:
        if authorization.lower().startswith("bearer "):
            return authorization[7:]
        return authorization
    return query_value(request.path, "token")


def origin_allowed(request):
    origin = header_value(request, "origin")
    if not origin:
        return True
    normalized = origin.rstrip("/")
    if "*" in ALLOWED_ORIGINS or normalized in ALLOWED_ORIGINS:
        return True
    return bool(AUTH_TOKEN)


def token_matches(token):
    if not token:
        return False
    return hmac.compare_digest(token.encode(), AUTH_TOKEN.encode())


def is_loopback_host(host):
    return host in {"127.0.0.1", "localhost", "::1"}


def is_websocket_upgrade(request):
    return any(
        key.lower() == "upgrade" and value.lower() == "websocket"
        for key, value in request.headers.raw_items()
    )


def web_asset_path(request_path):
    path = urllib.parse.urlsplit(request_path or "/").path
    relative = "index.html" if path in {"", "/"} else path.lstrip("/")
    root_assets = {"index.html", "manifest.webmanifest", "notification-icons.js", "sw.js"}
    if relative not in root_assets and not relative.startswith("icons/"):
        return None
    try:
        asset = (WEB_DIR / relative).resolve()
        web_root = WEB_DIR.resolve()
    except OSError:
        return None
    if not asset.is_relative_to(web_root) or not asset.is_file():
        return None
    return asset


async def process_request(connection, request):
    """Serve the phone app over HTTP and authenticate WebSocket upgrades."""
    from websockets.http11 import Response
    from websockets.datastructures import Headers

    if is_websocket_upgrade(request):
        if not origin_allowed(request):
            headers = Headers([("Content-Type", "text/plain")])
            return Response(403, "Forbidden", headers, b"Origin not allowed\n")
        if AUTH_TOKEN and not token_matches(request_token(request)):
            headers = Headers([("Content-Type", "text/plain")])
            return Response(401, "Unauthorized", headers, b"Invalid token\n")
        return None

    path = urllib.parse.urlsplit(request.path or "/").path
    if path == "/health":
        headers = Headers([("Content-Type", "text/plain; charset=utf-8")])
        return Response(200, "OK", headers, b"ok\n")
    if path == "/healthz":
        body = json.dumps({"status": "ok", "version": RELAY_VERSION, "protocol": PROTOCOL_VERSION}).encode() + b"\n"
        headers = Headers([("Content-Type", "application/json; charset=utf-8")])
        return Response(200, "OK", headers, body)

    asset = web_asset_path(request.path)
    if not asset:
        headers = Headers([("Content-Type", "text/plain; charset=utf-8")])
        return Response(404, "Not Found", headers, b"Not found\n")
    try:
        body = asset.read_bytes()
    except OSError:
        headers = Headers([("Content-Type", "text/plain; charset=utf-8")])
        return Response(404, "Not Found", headers, b"Not found\n")
    content_type = WEB_ASSET_CONTENT_TYPES.get(asset.suffix.lower(), "application/octet-stream")
    headers = Headers([
        ("Content-Type", content_type),
        ("Cache-Control", "no-cache"),
        ("X-Content-Type-Options", "nosniff"),
    ])
    return Response(200, "OK", headers, body)


def request_id_for(msg):
    request_id = msg.get("request_id", "") if isinstance(msg, dict) else ""
    if isinstance(request_id, str) and re.fullmatch(r"[A-Za-z0-9._:-]{1,120}", request_id):
        return request_id
    return f"relay-{secrets.token_urlsafe(10)}"


def command_details(msg, details=None):
    result = dict(details or {})
    client_id = compact_text(msg.get("client_id"), 120) if isinstance(msg, dict) else ""
    if client_id:
        result["client_id"] = client_id
    return result


def parsed_herdr_output(output):
    if not output:
        return None
    try:
        parsed = json.loads(output)
    except json.JSONDecodeError:
        return {"message": compact_text(output, 500)}
    if isinstance(parsed, dict) and isinstance(parsed.get("result"), dict):
        return parsed["result"]
    return parsed if isinstance(parsed, (dict, list)) else None


def nested_value(value, key):
    if isinstance(value, dict):
        if value.get(key):
            return value[key]
        for child in value.values():
            found = nested_value(child, key)
            if found:
                return found
    elif isinstance(value, list):
        for child in value:
            found = nested_value(child, key)
            if found:
                return found
    return None


async def resolve_started_agent(data, name):
    pane_id = nested_value(data, "pane_id")
    workspace_id = nested_value(data, "workspace_id")
    if pane_id and workspace_id:
        return str(pane_id), str(workspace_id)
    await asyncio.sleep(0.35)
    ok, output, _error = await run_herdr_async_result("agent", "get", name)
    if not ok:
        return "", ""
    current = parsed_herdr_output(output)
    return str(nested_value(current, "pane_id") or ""), str(nested_value(current, "workspace_id") or "")


async def move_started_agent_to_new_tab(pane_id, workspace_id, label):
    if not pane_id or not workspace_id:
        return False, None, "Started agent identity is incomplete"
    ok, output, error = await run_herdr_async_result(
        "pane", "move", pane_id,
        "--new-tab", "--workspace", workspace_id,
        "--label", label, "--no-focus",
    )
    return ok, parsed_herdr_output(output), error


async def start_agent_in_new_tab(profile, name, cwd):
    ok, output, error = await run_herdr_async_result(
        "agent", "start", name, "--cwd", str(cwd), "--no-focus", "--", *profile["argv"]
    )
    data = parsed_herdr_output(output)
    if not ok:
        return False, data, "", "", error

    pane_id, workspace_id = await resolve_started_agent(data, name)
    placed, placement, placement_error = await move_started_agent_to_new_tab(pane_id, workspace_id, name)
    if not placed:
        return True, data, pane_id, placement_error, ""

    data = {"agent": data, "placement": placement}
    pane_id = str(nested_value(placement, "pane_id") or pane_id)
    return True, data, pane_id, "", ""


async def send_prompt_to_pane(pane_id, prompt, is_codex=False):
    ok, _output, error = await run_herdr_async_result("pane", "send-text", pane_id, prompt)
    if ok:
        ok, _output, error = await run_herdr_async_result("pane", "send-keys", pane_id, "Enter")
    if ok and is_codex:
        await asyncio.sleep(0.16)
        ok, _output, error = await run_herdr_async_result("pane", "send-keys", pane_id, "Tab")
    return ok, error


def agent_for_pane(pane_id):
    agents = get_agents()
    if agents is None:
        return None, "Unable to read current Herdr agents"
    agent = next((item for item in agents if item.get("pane_id") == pane_id), None)
    if not agent:
        return None, "Agent is no longer available"
    return agent, ""


async def safe_send_json(ws, payload):
    try:
        await ws.send(json.dumps(payload))
        return True
    except Exception:
        return False


async def send_command_result(ws, request_id, action, ok, phase="completed", error="", pane_id="", data=None):
    if ok and action in POLL_WAKE_ACTIONS:
        wake_poll_loop()
    payload = {
        "type": "command_result",
        "request_id": request_id,
        "action": action,
        "ok": bool(ok),
        "phase": phase,
        "error": compact_text(error, 500),
        "pane_id": pane_id,
    }
    if data is not None:
        payload["data"] = data
    await safe_send_json(ws, payload)


async def complete_command(
    ws,
    request_id,
    action,
    ok,
    summary,
    *,
    error="",
    pane_id="",
    agent="",
    project="",
    phase="completed",
    data=None,
    details=None,
):
    await send_command_result(
        ws,
        request_id,
        action,
        ok,
        phase=phase if ok else "failed",
        error=error,
        pane_id=pane_id,
        data=data,
    )
    await publish_activity(
        action,
        phase if ok else "failed",
        summary if ok else f"{summary}: {error or 'failed'}",
        pane_id=pane_id,
        agent=agent,
        project=project,
        request_id=request_id,
        details=details,
    )


async def wait_for_approval_result(pane_id, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        await asyncio.sleep(0.35)
        agents = await asyncio.to_thread(get_agents)
        if agents is None:
            continue
        agent = next((item for item in agents if item.get("pane_id") == pane_id), None)
        if not agent:
            return True, "closed"
        status = str(agent.get("status") or "unknown")
        if status != "blocked":
            return True, status
    return False, "blocked"


async def handle_respond_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id")
    index = msg.get("index")
    total = msg.get("total")
    if (
        not pane_id
        or not isinstance(index, int)
        or isinstance(index, bool)
        or index < 0
        or index >= 20
        or (total is not None and (not isinstance(total, int) or isinstance(total, bool) or total < 2 or total > 20))
    ):
        await complete_command(ws, request_id, "approval", False, "Approval failed", error="Invalid approval request", pane_id=pane_id or "")
        return
    agent, error = await asyncio.to_thread(agent_for_pane, pane_id)
    if error:
        await complete_command(ws, request_id, "approval", False, "Approval failed", error=error, pane_id=pane_id)
        return
    if str(agent.get("status") or "").lower() != "blocked":
        await complete_command(ws, request_id, "approval", False, "Approval skipped", error="Agent is no longer blocked", pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""))
        return

    keys = respond_keys(index, total)
    ok, _output, error = await run_herdr_async_result("pane", "send-keys", pane_id, *keys)
    choice = compact_text(msg.get("choice") or f"option {index + 1}", 120)
    if not ok:
        await complete_command(ws, request_id, "approval", False, f"Approval {choice}", error=error, pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""))
        return

    await send_command_result(ws, request_id, "approval", True, phase="accepted", pane_id=pane_id)
    confirmed, status = await wait_for_approval_result(pane_id)
    phase = "confirmed" if confirmed else "unconfirmed"
    summary = f"Approval {choice}"
    await complete_command(
        ws,
        request_id,
        "approval",
        True,
        summary,
        pane_id=pane_id,
        agent=agent.get("agent", ""),
        project=agent.get("project", ""),
        phase=phase,
        details=command_details(msg, {"choice": choice, "resulting_status": status, "source": msg.get("source") or "App"}),
    )


async def handle_submit_prompt_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id", "")
    prompt = msg.get("text", "")
    if not pane_id or not isinstance(prompt, str) or not prompt.strip():
        await complete_command(ws, request_id, "prompt", False, "Prompt failed", error="Prompt text is required", pane_id=pane_id)
        return
    if len(prompt) > 20000:
        await complete_command(ws, request_id, "prompt", False, "Prompt failed", error="Prompt is longer than 20,000 characters", pane_id=pane_id)
        return
    agent, error = await asyncio.to_thread(agent_for_pane, pane_id)
    if error:
        await complete_command(ws, request_id, "prompt", False, "Prompt failed", error=error, pane_id=pane_id)
        return
    is_codex = bool(re.search(r"\bcodex\b", str(agent.get("agent") or ""), re.IGNORECASE))
    ok, error = await send_prompt_to_pane(pane_id, prompt, is_codex)
    await complete_command(
        ws,
        request_id,
        "prompt",
        ok,
        "Prompt sent",
        error=error,
        pane_id=pane_id,
        agent=agent.get("agent", ""),
        project=agent.get("project", ""),
        details=command_details(msg, {"preview": compact_text(prompt, 120)}),
    )


async def handle_send_keys_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id", "")
    keys = msg.get("keys", [])
    if not pane_id or not isinstance(keys, list) or not keys or not all(isinstance(key, str) and 0 < len(key) <= 40 for key in keys):
        await send_command_result(ws, request_id, "keys", False, phase="failed", error="Invalid key request", pane_id=pane_id)
        return
    ok, _output, error = await run_herdr_async_result("pane", "send-keys", pane_id, *keys)
    activity_label = compact_text(msg.get("activity_label"), 120)
    if activity_label:
        await complete_command(ws, request_id, "keys", ok, activity_label, error=error, pane_id=pane_id, details=command_details(msg, {"keys": ", ".join(keys)}))
    else:
        await send_command_result(ws, request_id, "keys", ok, error=error, phase="completed" if ok else "failed", pane_id=pane_id)


async def handle_list_directories_command(ws, msg):
    request_id = request_id_for(msg)
    data, error = await asyncio.to_thread(list_project_directory, msg.get("path", ""))
    await send_command_result(
        ws,
        request_id,
        "list_directories",
        not error,
        error=error,
        data=data,
    )


async def handle_agent_start_command(ws, msg):
    request_id = request_id_for(msg)
    profiles = load_agent_profiles()
    profile_id = str(msg.get("profile_id") or "")
    profile = profiles.get(profile_id)
    name = compact_text(msg.get("name"), 48)
    cwd_value = str(msg.get("cwd") or "").strip()
    prompt = msg.get("prompt", "")
    if not profile:
        await complete_command(ws, request_id, "agent_start", False, "Agent start failed", error="Unknown or unavailable agent profile")
        return
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,47}", name):
        await complete_command(ws, request_id, "agent_start", False, "Agent start failed", error="Name must use letters, numbers, dots, underscores, or dashes")
        return
    cwd, cwd_error = resolve_agent_cwd(cwd_value)
    if cwd_error:
        await complete_command(ws, request_id, "agent_start", False, "Agent start failed", error=cwd_error)
        return
    if not isinstance(prompt, str) or len(prompt) > 20000:
        await complete_command(ws, request_id, "agent_start", False, "Agent start failed", error="Initial task is longer than 20,000 characters")
        return

    ok, data, pane_id, placement_error, error = await start_agent_in_new_tab(profile, name, cwd)
    warnings = []
    if placement_error:
        warnings.append(f"Agent started, but a dedicated tab could not be created: {placement_error}")
    if ok and prompt.strip():
        if pane_id:
            await asyncio.sleep(0.25)
            prompt_ok, prompt_error = await send_prompt_to_pane(str(pane_id), prompt.strip(), profile_id == "codex")
            if not prompt_ok:
                warnings.append(f"Agent started, but the initial task failed: {prompt_error}")
        else:
            warnings.append("Agent started, but its pane could not be found for the initial task")
    warning = "; ".join(warnings)
    if isinstance(data, dict) and warning:
        data = {**data, "warning": warning}
    await complete_command(
        ws,
        request_id,
        "agent_start",
        ok,
        f"Started {name}",
        error=error,
        agent=profile["label"],
        project=cwd.name,
        data=data,
        phase="completed_with_warning" if ok and warning else "completed",
        details=command_details(msg, {"profile": profile["label"], "cwd": str(cwd)}),
    )


async def handle_agent_rename_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id", "")
    name = compact_text(msg.get("name"), 80)
    if not pane_id or not name:
        await complete_command(ws, request_id, "agent_rename", False, "Rename failed", error="Agent and name are required", pane_id=pane_id)
        return
    agent, error = await asyncio.to_thread(agent_for_pane, pane_id)
    if error:
        await complete_command(ws, request_id, "agent_rename", False, "Rename failed", error=error, pane_id=pane_id)
        return
    ok, _output, error = await run_herdr_async_result("agent", "rename", pane_id, name)
    await complete_command(ws, request_id, "agent_rename", ok, f"Renamed agent to {name}", error=error, pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""), details=command_details(msg))


async def handle_agent_stop_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id", "")
    if not pane_id:
        await complete_command(ws, request_id, "agent_stop", False, "Stop failed", error="Agent is required")
        return
    agent, error = await asyncio.to_thread(agent_for_pane, pane_id)
    if error:
        await complete_command(ws, request_id, "agent_stop", False, "Stop failed", error=error, pane_id=pane_id)
        return
    ok, _output, error = await run_herdr_async_result("pane", "close", pane_id)
    await complete_command(ws, request_id, "agent_stop", ok, "Stopped agent", error=error, pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""), details=command_details(msg))


async def handle_agent_clear_command(ws, msg):
    request_id = request_id_for(msg)
    pane_id = msg.get("pane_id", "")
    if not pane_id:
        await complete_command(ws, request_id, "agent_clear", False, "Clear failed", error="Agent is required")
        return
    agent, error = await asyncio.to_thread(agent_for_pane, pane_id)
    if error:
        await complete_command(ws, request_id, "agent_clear", False, "Clear failed", error=error, pane_id=pane_id)
        return
    profiles = load_agent_profiles()
    agent_label = re.sub(r"[^a-z0-9]+", "-", str(agent.get("agent") or "").lower()).strip("-")
    profile = profiles.get(agent_label)
    if not profile:
        profile = next((value for key, value in profiles.items() if key in agent_label or key in str(agent.get("agent") or "").lower()), None)
    if not profile:
        await complete_command(ws, request_id, "agent_clear", False, "Clear failed", error="This agent does not match an available launch profile", pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""))
        return
    cwd, cwd_error = resolve_agent_cwd(agent.get("cwd", ""))
    if cwd_error:
        await complete_command(ws, request_id, "agent_clear", False, "Clear failed", error=cwd_error, pane_id=pane_id, agent=agent.get("agent", ""), project=agent.get("project", ""))
        return

    name = f"clear-{profile['id']}-{int(time.time()) % 100000}"
    ok, data, replacement_pane_id, placement_error, error = await start_agent_in_new_tab(profile, name, cwd)
    warning = ""
    if ok and placement_error:
        if replacement_pane_id:
            await run_herdr_async_result("pane", "close", replacement_pane_id)
        ok = False
        error = f"Replacement could not be placed in a dedicated tab: {placement_error}"
    elif ok:
        close_ok, _close_output, close_error = await run_herdr_async_result("pane", "close", pane_id)
        if not close_ok:
            warning = f"Replacement started, but the old pane could not be closed: {close_error}"
            data["warning"] = warning
    await complete_command(
        ws,
        request_id,
        "agent_clear",
        ok,
        "Cleared agent",
        error=error,
        pane_id=pane_id,
        agent=agent.get("agent", ""),
        project=agent.get("project", ""),
        phase="completed_with_warning" if ok and warning else "completed",
        data=data,
        details=command_details(msg, {"profile": profile["label"], "cwd": str(cwd)}),
    )


async def reject_incompatible_client_protocol(ws, msg):
    msg_type = msg.get("type", "command")
    error = (
        f"Incompatible app protocol v{client_protocol_version(msg) or 'invalid'}; "
        f"relay requires v{PROTOCOL_VERSION}"
    )
    if msg_type == "upload_image":
        response = {
            "type": "upload_result",
            "ok": False,
            "error": error,
            "path": "",
            "pane_id": msg.get("pane_id", ""),
            "request_id": msg.get("request_id", ""),
        }
    elif msg_type in {"push_subscribe", "push_unsubscribe"}:
        response = {
            "type": "push_subscribed" if msg_type == "push_subscribe" else "push_unsubscribed",
            "ok": False,
            "error": error,
        }
    else:
        response = {
            "type": "command_result",
            "request_id": msg.get("request_id", ""),
            "action": msg_type,
            "ok": False,
            "phase": "failed",
            "error": error,
        }
    await safe_send_json(ws, response)


async def handle_client(ws):
    try:
        profiles = load_agent_profiles()
        await safe_send_json(ws, {
            "type": "push_config",
            "vapid_public_key": ensure_vapid_public_key(),
            "host": LOCAL_HOST,
            "protocol": PROTOCOL_VERSION,
            "version": RELAY_VERSION,
            "capabilities": RELAY_CAPABILITIES,
            "agent_profiles": [
                {"id": profile["id"], "label": profile["label"]}
                for profile in profiles.values()
            ],
        })
        clients.add(ws)
        await send_latest_agents(ws)
        await safe_send_json(ws, {
            "type": "activity_history",
            "activities": await asyncio.to_thread(load_activity),
        })
        async for raw in ws:
            try:
                msg = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if not isinstance(msg, dict):
                continue
            msg_type = msg.get("type")
            if msg_type in MUTATING_MESSAGE_TYPES and not client_protocol_matches(msg):
                await reject_incompatible_client_protocol(ws, msg)
                continue
            if msg_type == "respond":
                await handle_respond_command(ws, msg)
            elif msg_type == "push_subscribe":
                ok = await asyncio.to_thread(
                    store_push_subscription,
                    msg.get("subscription"),
                    msg.get("user_agent", ""),
                    msg.get("client_id", ""),
                    msg.get("replace_endpoints", []),
                    msg.get("notify_finished") is True,
                )
                await safe_send_json(ws, {"type": "push_subscribed", "ok": ok})
            elif msg_type == "push_unsubscribe":
                ok = await asyncio.to_thread(
                    remove_push_subscription_records,
                    msg.get("endpoints", []),
                    msg.get("client_id", ""),
                )
                await safe_send_json(ws, {"type": "push_unsubscribed", "ok": ok})
            elif msg_type == "get_activity":
                await safe_send_json(ws, {
                    "type": "activity_history",
                    "activities": await asyncio.to_thread(load_activity, msg.get("limit", ACTIVITY_MAX_ITEMS)),
                })
            elif msg_type == "read_pane":
                pane_id = msg.get("pane_id")
                if not pane_id:
                    continue
                # Opening a terminal on the phone counts as viewing the pane.
                await acknowledge_pane_viewed(pane_id)
                try:
                    lines = min(max(int(msg.get("lines", 30)), 1), CLAUDE_HISTORY_MAX_LINES)
                except (TypeError, ValueError):
                    lines = 30
                fmt = "ansi" if msg.get("format") == "ansi" else "text"
                content = await run_herdr_async(
                    "pane", "read", pane_id,
                    "--lines", str(lines),
                    "--source", "recent-unwrapped",
                    "--format", fmt,
                )
                if fmt == "ansi" and "claude" in agent_types.get(pane_id, ""):
                    state = load_claude_history_state(pane_id)
                    if state is not None and last_statuses.get(pane_id) not in {"working", "blocked"}:
                        content = claude_history_content(state, lines)
                    else:
                        content = merge_claude_history(pane_id, content, lines)
                await safe_send_json(ws, {"type": "pane_content", "pane_id": pane_id, "content": content or "", "format": fmt})
            elif msg_type == "acknowledge_pane":
                pane_id = msg.get("pane_id", "")
                if not pane_id or pane_id not in agent_types:
                    await send_command_result(
                        ws,
                        msg.get("request_id", ""),
                        "acknowledge_pane",
                        False,
                        phase="failed",
                        error="Agent is unavailable",
                    )
                    continue
                await acknowledge_pane_viewed(pane_id)
                await send_command_result(
                    ws,
                    msg.get("request_id", ""),
                    "acknowledge_pane",
                    True,
                    pane_id=pane_id,
                )
            elif msg_type == "submit_prompt":
                await handle_submit_prompt_command(ws, msg)
            elif msg_type == "send_keys":
                await handle_send_keys_command(ws, msg)
            elif msg_type == "list_directories":
                await handle_list_directories_command(ws, msg)
            elif msg_type == "send_text":
                request_id = request_id_for(msg)
                pane_id = msg.get("pane_id", "")
                text = msg.get("text", "")
                if not pane_id or not isinstance(text, str) or not text:
                    await complete_command(ws, request_id, "text", False, "Text input failed", error="Text and agent are required", pane_id=pane_id)
                    continue
                ok, _output, error = await run_herdr_async_result("pane", "send-text", pane_id, text)
                await complete_command(ws, request_id, "text", ok, "Text inserted", error=error, pane_id=pane_id, details=command_details(msg, {"preview": compact_text(text, 120)}))
            elif msg_type == "agent_start":
                await handle_agent_start_command(ws, msg)
            elif msg_type == "agent_rename":
                await handle_agent_rename_command(ws, msg)
            elif msg_type == "agent_stop":
                await handle_agent_stop_command(ws, msg)
            elif msg_type in {"agent_clear", "agent_restart"}:
                await handle_agent_clear_command(ws, msg)
            elif msg_type == "upload_image":
                pane_id = msg.get("pane_id", "")
                request_id = request_id_for(msg)
                ok, error, path = await asyncio.to_thread(
                    store_uploaded_image,
                    msg.get("filename", ""),
                    msg.get("mime", ""),
                    msg.get("data", ""),
                )
                await safe_send_json(ws, {
                    "type": "upload_result",
                    "ok": ok,
                    "error": error,
                    "path": path or "",
                    "pane_id": pane_id,
                    "request_id": request_id,
                })
                await publish_activity(
                    "upload",
                    "completed" if ok else "failed",
                    f"Attached {compact_text(msg.get('filename') or 'image', 100)}" if ok else f"Image upload failed: {error}",
                    pane_id=pane_id,
                    request_id=request_id,
                    details=command_details(msg, {"path": path or ""}),
                )
    except ConnectionClosed:
        pass
    finally:
        clients.discard(ws)


class UDPPlugin(asyncio.DatagramProtocol):
    def datagram_received(self, data, addr):
        try:
            event = json.loads(data.decode())
            if not isinstance(event, dict):
                return
            event_queue.put_nowait(event)
            wake_poll_loop()
        except Exception:
            pass


async def main():
    if not AUTH_TOKEN and not is_loopback_host(WS_HOST):
        raise SystemExit("Refusing to bind a tokenless relay outside loopback. Set HERDR_RELAY_TOKEN or HERDR_RELAY_HOST=127.0.0.1.")
    if not AUTH_TOKEN:
        print("WARNING: HERDR_RELAY_TOKEN is empty. Browser requests with an Origin header will be rejected unless HERDR_ALLOWED_ORIGINS allows them.")
    ensure_vapid_public_key()
    loop = asyncio.get_running_loop()
    try:
        await loop.create_datagram_endpoint(UDPPlugin, local_addr=("127.0.0.1", PLUGIN_PORT))
    except OSError:
        print(f"UDP {PLUGIN_PORT} in use, plugin push disabled")
    asyncio.create_task(poll_loop())
    asyncio.create_task(event_push())
    asyncio.create_task(prune_uploads_loop())
    server = await serve(handle_client, WS_HOST, WS_PORT, process_request=process_request, max_size=WS_MAX_SIZE)
    print(f"Herdr Mobile Relay {RELAY_VERSION} on {WS_HOST}:{WS_PORT} (WebSocket + phone app)")
    print(f"  polling: {LOCAL_HOST}")
    stop = loop.create_future()
    def request_stop():
        if not stop.done():
            stop.set_result(None)
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, request_stop)
    await stop
    # In-flight captures are deliberately not awaited here: cancellation lands
    # at their herdr-read await, before any merge, so state stays consistent,
    # and the next start recovers the missed frame from the still-visible
    # viewport via tail overlap.
    for pane_id in list(claude_history_state):
        save_claude_history_state(pane_id, force=True)
    server.close()


if __name__ == "__main__":
    asyncio.run(main())

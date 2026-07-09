#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["websockets>=14.0", "pywebpush>=2.0.0", "py-vapid>=1.9.2", "cryptography>=42.0.0"]
# ///
"""Herdr Mobile Relay server — polls local herdr and broadcasts to clients."""
import asyncio, base64, hmac, json, os, re, secrets, shutil, signal, socket, subprocess, threading, time, urllib.parse
from pathlib import Path

try:
    from websockets.asyncio.server import serve
except ImportError:
    from websockets.server import serve
from websockets.exceptions import ConnectionClosed
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from py_vapid import Vapid
from pywebpush import WebPushException, webpush

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
PLUGIN_PORT = int(os.environ.get("HERDR_RELAY_PLUGIN_PORT", "8376"))
AUTH_TOKEN = os.environ.get("HERDR_RELAY_TOKEN", "")  # Shared secret for public/browser relay auth
ALLOWED_ORIGINS = {
    origin.strip().rstrip("/")
    for origin in os.environ.get("HERDR_ALLOWED_ORIGINS", "").split(",")
    if origin.strip()
}
LOCAL_HOST = socket.gethostname().split(".")[0] or "local"
PUSH_DIR = Path(os.environ.get("HERDR_RELAY_PUSH_DIR", Path(__file__).with_name("push")))
PUSH_SUBSCRIPTIONS_FILE = PUSH_DIR / "subscriptions.json"
VAPID_PRIVATE_KEY_FILE = Path(os.environ.get("HERDR_VAPID_PRIVATE_KEY", PUSH_DIR / "vapid_private.pem"))
VAPID_SUBJECT = os.environ.get("HERDR_VAPID_SUBJECT", f"mailto:herdr-mobile-relay@{LOCAL_HOST}.local")
VAPID_PUBLIC_KEY = None
PUSH_LOCK = threading.RLock()
UPLOAD_DIR = Path(os.environ.get("HERDR_UPLOAD_DIR", Path.home() / ".cache" / "herdr-mobile-relay" / "uploads"))
UPLOAD_MAX_BYTES = int(os.environ.get("HERDR_UPLOAD_MAX_BYTES", str(10 * 1024 * 1024)))
WS_MAX_SIZE = int(os.environ.get("HERDR_WS_MAX_SIZE", str(max(16 * 1024 * 1024, UPLOAD_MAX_BYTES * 2 + 1024 * 1024))))
IMAGE_MIME_EXTENSIONS = {
    "image/png": ".png",
    "image/jpeg": ".jpg",
    "image/webp": ".webp",
    "image/gif": ".gif",
    "image/heic": ".heic",
    "image/heif": ".heif",
}

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
last_statuses = {}
event_queue = asyncio.Queue()


def run_herdr(*args):
    try:
        cmd = [HERDR, *args]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=15)
        if r.returncode != 0:
            return None
        return r.stdout.strip()
    except Exception:
        return None


async def run_herdr_async(*args):
    return await asyncio.to_thread(run_herdr, *args)


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
            agents.append(
                {
                    "pane_id": raw_pane_id,
                    "raw_pane_id": raw_pane_id,
                    "tab_id": tab_id,
                    "tab_label": tab.get("label", ""),
                    "tab_number": tab.get("number"),
                    "workspace_id": p.get("workspace_id", ""),
                    "agent": p.get("agent", ""),
                    "status": p.get("agent_status", "unknown"),
                    "cwd": p.get("cwd", ""),
                    "project": os.path.basename(p.get("cwd", "")),
                    "host": LOCAL_HOST,
                }
            )
        return agents
    except (json.JSONDecodeError, KeyError):
        return None


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


def store_push_subscription(subscription, user_agent="", client_id="", replace_endpoints=None):
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


def push_payload(blocked_msg):
    project = blocked_msg.get("project") or blocked_msg.get("agent") or "agent"
    host = blocked_msg.get("host") or LOCAL_HOST
    pane_id = blocked_msg.get("pane_id", "")
    command = blocked_msg.get("command") or "Agent needs approval"
    target = urllib.parse.quote(json.dumps({
        "host": host,
        "pane_id": pane_id,
    }, separators=(",", ":")))
    return {
        "title": f"{project} blocked",
        "body": f"{command} · {host}",
        "tag": f"herdr-{host}-{pane_id}",
        "url": f"./#notify={target}",
    }


def send_webpush_notifications(blocked_msg):
    subscriptions = load_push_subscriptions()
    if not subscriptions:
        return
    payload = json.dumps(push_payload(blocked_msg))
    stale = []
    for item in subscriptions:
        subscription = item.get("subscription", {})
        try:
            webpush(
                subscription_info=subscription,
                data=payload,
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


async def push_blocked(blocked_msg):
    await asyncio.to_thread(send_webpush_notifications, blocked_msg)


async def publish_blocked(blocked_msg):
    await broadcast(blocked_msg)
    asyncio.create_task(push_blocked(blocked_msg))


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
    data = json.dumps(msg)
    dead = set()
    for ws in list(clients):
        try:
            await ws.send(data)
        except Exception:
            dead.add(ws)
    clients.difference_update(dead)


async def poll_loop():
    while True:
        agents = await asyncio.to_thread(get_agents)
        if agents is None:
            await asyncio.sleep(POLL_INTERVAL)
            continue
        await broadcast({"type": "agents", "agents": agents})
        live_pane_ids = {a["pane_id"] for a in agents}
        for pane_id in set(last_statuses) - live_pane_ids:
            del last_statuses[pane_id]
        if agents:
            for a in agents:
                pid, status = a["pane_id"], a["status"]
                if status == "blocked" and last_statuses.get(pid) != "blocked":
                    content = await asyncio.to_thread(read_pane, pid)
                    options = detect_options(content)
                    command = detect_command_context(content)
                    await publish_blocked({
                        "type": "blocked", "pane_id": pid,
                        "agent": a["agent"], "project": a["project"],
                        "host": a.get("host", LOCAL_HOST),
                        "tab_id": a.get("tab_id", ""),
                        "tab_label": a.get("tab_label", ""),
                        "tab_number": a.get("tab_number"),
                        "workspace_id": a.get("workspace_id", ""),
                        "prompt": content[:500],
                        "command": command,
                        "options": options or TOOL_OPTIONS
                    })
                last_statuses[pid] = status
        await asyncio.sleep(POLL_INTERVAL)


async def event_push():
    while True:
        event = await event_queue.get()
        raw_pane_id = event.get("pane_id", "")
        status = event.get("status", "")
        host = event.get("host", LOCAL_HOST)
        was_blocked = raw_pane_id and last_statuses.get(raw_pane_id) == "blocked"
        if raw_pane_id and status:
            last_statuses[raw_pane_id] = status

        if status == "blocked" and raw_pane_id and not was_blocked:
            content = await asyncio.to_thread(read_pane, raw_pane_id) or event.get("prompt", "Agent is blocked")
            options = detect_options(content)
            command = detect_command_context(content)
            await publish_blocked({
                "type": "blocked", "pane_id": raw_pane_id,
                "agent": event.get("agent", ""),
                "project": event.get("project", ""),
                "host": host,
                "tab_id": event.get("tab_id", ""),
                "tab_label": event.get("tab_label", ""),
                "tab_number": event.get("tab_number"),
                "workspace_id": event.get("workspace_id", ""),
                "prompt": content[:500],
                "command": command,
                "options": options or TOOL_OPTIONS
            })

        if raw_pane_id and event.get("type") == "agent_event":
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
            })


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


async def process_request(connection, request):
    """Handle WebSocket upgrades and HTTP GET /push?d=... on the same port."""
    from websockets.http11 import Response
    from websockets.datastructures import Headers

    if not origin_allowed(request):
        headers = Headers([("Content-Type", "text/plain")])
        return Response(403, "Forbidden", headers, b"Origin not allowed\n")

    if AUTH_TOKEN:
        token = request_token(request)
        if not token_matches(token):
            headers = Headers([("Content-Type", "text/plain")])
            return Response(401, "Unauthorized", headers, b"Invalid token\n")

    # Check if this is a WebSocket upgrade
    upgrade = None
    for key, value in request.headers.raw_items():
        if key.lower() == "upgrade":
            upgrade = value.lower()
    if upgrade == "websocket":
        return None  # proceed with WebSocket handshake

    # HTTP GET — parse event from URL query params.
    # (since we can't read request body in websockets 16)
    # Plugins should encode payload in the URL path: /push?d=...
    if "?" in (request.path or ""):
        _, qs = request.path.split("?", 1)
        params = urllib.parse.parse_qs(qs)
        if "d" in params:
            try:
                event = json.loads(urllib.parse.unquote(params["d"][0]))
                event_queue.put_nowait(event)
            except Exception:
                pass

    headers = Headers()
    return Response(200, "OK", headers, b"ok\n")


async def handle_client(ws):
    clients.add(ws)
    try:
        try:
            await ws.send(json.dumps({
                "type": "push_config",
                "vapid_public_key": ensure_vapid_public_key(),
            }))
        except Exception:
            pass
        async for raw in ws:
            try:
                msg = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if not isinstance(msg, dict):
                continue
            msg_type = msg.get("type")
            if msg_type == "respond":
                pane_id = msg.get("pane_id")
                index = msg.get("index")
                if pane_id and isinstance(index, int) and index >= 0:
                    keys = respond_keys(index, msg.get("total"))
                    await run_herdr_async("pane", "send-keys", pane_id, *keys)
            elif msg_type == "agent_event":
                event_queue.put_nowait(msg)
            elif msg_type == "push_subscribe":
                ok = await asyncio.to_thread(
                    store_push_subscription,
                    msg.get("subscription"),
                    msg.get("user_agent", ""),
                    msg.get("client_id", ""),
                    msg.get("replace_endpoints", []),
                )
                await ws.send(json.dumps({"type": "push_subscribed", "ok": ok}))
            elif msg_type == "push_unsubscribe":
                ok = await asyncio.to_thread(
                    remove_push_subscription_records,
                    msg.get("endpoints", []),
                    msg.get("client_id", ""),
                )
                await ws.send(json.dumps({"type": "push_unsubscribed", "ok": ok}))
            elif msg_type == "read_pane":
                pane_id = msg.get("pane_id")
                if not pane_id:
                    continue
                lines = msg.get("lines", "30")
                fmt = "ansi" if msg.get("format") == "ansi" else "text"
                content = await run_herdr_async(
                    "pane", "read", pane_id,
                    "--lines", str(lines),
                    "--source", "recent",
                    "--format", fmt,
                )
                await ws.send(json.dumps({"type": "pane_content", "pane_id": pane_id, "content": content or "", "format": fmt}))
            elif msg_type == "send_keys":
                pane_id = msg.get("pane_id")
                if not pane_id:
                    continue
                keys = msg.get("keys", [])
                if isinstance(keys, list) and all(isinstance(k, str) for k in keys):
                    await run_herdr_async("pane", "send-keys", pane_id, *keys)
            elif msg_type == "send_text":
                pane_id = msg.get("pane_id")
                if not pane_id:
                    continue
                text = msg.get("text", "")
                if isinstance(text, str):
                    await run_herdr_async("pane", "send-text", pane_id, text)
            elif msg_type == "upload_image":
                pane_id = msg.get("pane_id", "")
                request_id = msg.get("request_id", "")
                ok, error, path = await asyncio.to_thread(
                    store_uploaded_image,
                    msg.get("filename", ""),
                    msg.get("mime", ""),
                    msg.get("data", ""),
                )
                await ws.send(json.dumps({
                    "type": "upload_result",
                    "ok": ok,
                    "error": error,
                    "path": path or "",
                    "pane_id": pane_id,
                    "request_id": request_id,
                }))
    except ConnectionClosed:
        pass
    finally:
        clients.discard(ws)


class UDPPlugin(asyncio.DatagramProtocol):
    def datagram_received(self, data, addr):
        try:
            event_queue.put_nowait(json.loads(data.decode()))
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
    server = await serve(handle_client, WS_HOST, WS_PORT, process_request=process_request, max_size=WS_MAX_SIZE)
    print(f"Herdr Mobile Relay on {WS_HOST}:{WS_PORT} (WebSocket + HTTP GET push)")
    print(f"  polling: {LOCAL_HOST}")
    stop = loop.create_future()
    def request_stop():
        if not stop.done():
            stop.set_result(None)
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, request_stop)
    await stop
    server.close()


if __name__ == "__main__":
    asyncio.run(main())

#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["websockets>=14.0"]
# ///
"""Herdr Mobile Relay server — polls local herdr and broadcasts to clients."""
import asyncio, hmac, json, os, re, shutil, signal, socket, subprocess, urllib.parse

try:
    from websockets.asyncio.server import serve
except ImportError:
    from websockets.server import serve

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
AUTH_TOKEN = os.environ.get("HERDR_RELAY_TOKEN", "")  # Optional: shared secret for relay auth
ALLOWED_ORIGINS = {
    origin.strip().rstrip("/")
    for origin in os.environ.get("HERDR_ALLOWED_ORIGINS", "").split(",")
    if origin.strip()
}
LOCAL_HOST = socket.gethostname().split(".")[0] or "local"

TOOL_OPTIONS = ["yes, single permission", "trust, always allow", "no (tab to edit)"]
SUBAGENT_OPTIONS = ["approve all pending", "configure individually", "exit (cancel subagents)"]
RESPONSE_KEYS = {
    "yes, single permission": "y",
    "trust, always allow": "a",
    "no (tab to edit)": "n",
    "approve all pending": "a",
    "configure individually": "c",
    "exit (cancel subagents)": "e",
}
CHROME_RE = re.compile(
    r"^[\s─━═_—│|◔◑◕●\s]+$"
    r"|Kiro\s[·•]"
    r"|esc to cancel"
    r"|type to queue"
    r"|^\s*[◔◑◕●]\s+(Shell|Bash)"
)

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
    raw = run_herdr("pane", "read", pane_id, "--lines", "20", "--source", "recent")
    if raw is None:
        return ""
    lines = [l for l in raw.splitlines() if l.strip() and not CHROME_RE.search(l)]
    return "\n".join(lines[-6:])


def detect_options(text):
    lower = text.lower()
    if "yes, single permission" in lower:
        return TOOL_OPTIONS
    if "approve all pending" in lower:
        return SUBAGENT_OPTIONS
    return None


def response_key(value):
    key = (value or "").strip().lower()
    if key in RESPONSE_KEYS:
        return RESPONSE_KEYS[key]
    if len(key) == 1 and key.isalnum():
        return key
    return None


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
                    await broadcast({
                        "type": "blocked", "pane_id": pid,
                        "agent": a["agent"], "project": a["project"],
                        "host": a.get("host", LOCAL_HOST),
                        "tab_id": a.get("tab_id", ""),
                        "tab_label": a.get("tab_label", ""),
                        "tab_number": a.get("tab_number"),
                        "workspace_id": a.get("workspace_id", ""),
                        "prompt": content[:500],
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
        if raw_pane_id and status:
            last_statuses[raw_pane_id] = status

        if status == "blocked" and raw_pane_id:
            content = await asyncio.to_thread(read_pane, raw_pane_id) or event.get("prompt", "Agent is blocked")
            options = detect_options(content)
            await broadcast({
                "type": "blocked", "pane_id": raw_pane_id,
                "agent": event.get("agent", ""),
                "project": event.get("project", ""),
                "host": host,
                "tab_id": event.get("tab_id", ""),
                "tab_label": event.get("tab_label", ""),
                "tab_number": event.get("tab_number"),
                "workspace_id": event.get("workspace_id", ""),
                "prompt": content[:500],
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
                key = response_key(msg.get("key") or msg.get("text"))
                if pane_id and key:
                    await run_herdr_async("pane", "send-text", pane_id, key)
            elif msg_type == "agent_event":
                event_queue.put_nowait(msg)
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
    loop = asyncio.get_running_loop()
    try:
        await loop.create_datagram_endpoint(UDPPlugin, local_addr=("127.0.0.1", PLUGIN_PORT))
    except OSError:
        print(f"UDP {PLUGIN_PORT} in use, plugin push disabled")
    asyncio.create_task(poll_loop())
    asyncio.create_task(event_push())
    server = await serve(handle_client, WS_HOST, WS_PORT, process_request=process_request)
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

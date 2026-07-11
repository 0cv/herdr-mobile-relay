#!/usr/bin/env python3
"""Local plugin hook for Herdr Mobile Relay."""
import json
import os
import socket
from pathlib import Path


def load_env_file():
    env_file = os.environ.get("HERDR_RELAY_ENV", "")
    if not env_file and os.environ.get("HERDR_PLUGIN_CONFIG_DIR"):
        env_file = str(Path(os.environ["HERDR_PLUGIN_CONFIG_DIR"]) / "relay.env")
    env_file = Path(env_file) if env_file else Path(__file__).with_name(".env")
    if not env_file.exists():
        return
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key and key not in os.environ:
            os.environ[key] = value


load_env_file()

event = json.loads(os.environ.get("HERDR_PLUGIN_EVENT_JSON", "{}"))
data = event.get("data", {})

payload = json.dumps({
    "type": "agent_event",
    "pane_id": data.get("pane_id", ""),
    "tab_id": data.get("tab_id", ""),
    "tab_label": data.get("tab_label") or data.get("tab_name") or data.get("label") or "",
    "tab_number": data.get("tab_number"),
    "workspace_id": data.get("workspace_id", ""),
    "status": (data.get("agent_status") or "").lower(),
    "agent": (data.get("agent") or data.get("display_agent") or "").lower(),
    "project": os.path.basename(data.get("cwd", "")),
    "cwd": data.get("cwd", ""),
    "host": socket.gethostname().split(".")[0],
}).encode()

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.sendto(payload, ("127.0.0.1", int(os.environ.get("HERDR_RELAY_PLUGIN_PORT", "8376"))))
sock.close()

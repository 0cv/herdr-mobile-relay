#!/usr/bin/env python3
"""Local plugin hook for Herdr Mobile Relay."""
import json, os, socket

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
sock.sendto(payload, ("127.0.0.1", 8376))
sock.close()

#!/usr/bin/env python3
"""Linux namespace-gated entry point for the managed launcher lifecycle fixture.

The former two-child fixture modeled a separately owned `tailscale serve` CLI
process. That process no longer exists: Go's held SessionAuthority owns Serve,
and the shared hosted fixture now checks relay supervision, control ordering,
retirement acknowledgements, and shell-pipe closure.
"""
import os
from pathlib import Path
import runpy

if os.environ.get("HERDR_S9B3_SANDBOX") != "1":
    raise SystemExit("Refusing launcher fixtures outside the private bwrap sandbox")
if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing launcher fixtures without the hosted lifecycle gate")
try:
    pid_namespace = os.readlink("/proc/self/ns/pid")
    net_namespace = os.readlink("/proc/self/ns/net")
except OSError as exc:
    raise SystemExit(f"Refusing launcher fixtures without namespace evidence: {exc}")
if pid_namespace == os.environ.get("HERDR_S9B3_HOST_PID_NS"):
    raise SystemExit("Refusing launcher fixtures: PID namespace is not private")
if net_namespace == os.environ.get("HERDR_S9B3_HOST_NET_NS"):
    raise SystemExit("Refusing launcher fixtures: network namespace is not private")
source = Path(os.environ.get("HERDR_S9B3_SOURCE", ""))
if not (source / "relay/common.sh").is_file() or not (source / "relay/tailscale.sh").is_file():
    raise SystemExit("Exact candidate relay scripts are required")

runpy.run_path(str(source / "tests/test_tailscale_launcher_lifecycle.py"), run_name="__main__")

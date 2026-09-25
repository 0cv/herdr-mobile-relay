#!/usr/bin/env python3
"""Disposable Darwin/arm64 entry point for managed launcher lifecycle fixtures.

The former fixture modeled an independently signalled Tailscale Serve child.
Managed Serve is now owned in-process by Go; shell lifecycle behavior is covered
by the shared control-socket fixture and Go supervisor tests.
"""
import os
from pathlib import Path
import platform
import runpy
import sys

if sys.platform != "darwin":
    raise SystemExit("Refusing Darwin launcher fixtures outside macOS")
if os.environ.get("HERDR_S9B3_DISPOSABLE_DARWIN") != "1":
    raise SystemExit("Refusing Darwin launcher fixtures without the disposable-runner gate")
if platform.machine().lower() not in ("arm64", "aarch64"):
    raise SystemExit("Refusing native qualification: this driver requires Darwin/arm64")
if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing launcher fixtures without the hosted lifecycle gate")
source = Path(os.environ.get("HERDR_S9B3_SOURCE", ""))
if not (source / "relay/common.sh").is_file() or not (source / "relay/tailscale.sh").is_file():
    raise SystemExit("Exact candidate relay scripts are required")

runpy.run_path(str(source / "tests/test_tailscale_launcher_lifecycle.py"), run_name="__main__")

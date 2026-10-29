#!/usr/bin/env python3
"""Disposable Darwin/arm64 entry point for developer and managed launcher fixtures.

The former fixture modeled an independently signalled Tailscale Serve child.
Managed Serve is now owned in-process by Go; shell lifecycle behavior is covered
by the shared control-socket fixture and Go supervisor tests.
"""
import os
import sys

# Admission precedes helper imports, source discovery and either suite.
if (os.name != "posix" or sys.platform != "darwin" or
        os.getuid() == 0 or os.geteuid() == 0 or
        os.uname().sysname != "Darwin" or
        os.uname().machine.lower() not in ("arm64", "aarch64") or
        os.environ.get("GITHUB_ACTIONS") != "true" or
        os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or
        os.environ.get("HERDR_S9B3_DISPOSABLE_DARWIN") != "1" or
        os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1"):
    raise SystemExit("Darwin launcher fixtures require non-root POSIX Darwin/arm64 GitHub-hosted CI and both disposable/lifecycle opt-ins")

source_root = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))
if os.environ.get("HERDR_S9B3_SOURCE") != source_root:
    raise SystemExit("HERDR_S9B3_SOURCE must name the executing driver's exact physical checkout")
for relative in (
        "relay/common.sh", "relay/tailscale.sh",
        "tests/test_tailscale_process_groups_darwin.py",
        "tests/test_dev_tailscale.py", "tests/test_tailscale_launcher_lifecycle.py"):
    required = os.path.join(source_root, relative)
    if not os.path.isfile(required) or os.path.realpath(required) != required:
        raise SystemExit("Complete exact candidate relay scripts and launcher suites are required without symlink redirection")

from pathlib import Path
import runpy
import subprocess

source = Path(source_root)
# Keep this ordinary child in the workflow-owned process group and timeout.
# The physical short temporary parent avoids Darwin AF_UNIX path overflow.
developer_env = os.environ.copy()
developer_env["TMPDIR"] = "/private/tmp"
print("START Darwin complete developer launcher fixtures", flush=True)
developer = subprocess.run(
    [sys.executable, str(source / "tests/test_dev_tailscale.py")],
    cwd=source, env=developer_env, stdin=subprocess.DEVNULL, check=False,
)
if developer.returncode != 0:
    print("FAIL Darwin complete developer launcher fixtures; managed lifecycle not started", file=sys.stderr, flush=True)
    raise SystemExit(developer.returncode if developer.returncode > 0 else 128 - developer.returncode)
print("PASS Darwin complete developer launcher fixtures", flush=True)

# unittest.main exits from this invocation; all mandatory work stays above it.
runpy.run_path(str(source / "tests/test_tailscale_launcher_lifecycle.py"), run_name="__main__")

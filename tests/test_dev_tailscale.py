#!/usr/bin/env python3
"""Hosted-only refusal/isolation checks for the explicit managed dev entrypoint.

This suite deliberately never runs a Tailscale CLI or an app/relay listener.
It does not replace the extracted-package managed browser acceptance gate.
"""

import os
from pathlib import Path
import pty
import subprocess
import tempfile

if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing development launcher tests outside hosted CI")

root = Path(__file__).resolve().parents[1]
script = root / "relay" / "dev-tailscale.sh"
tunnel_script = root / "relay" / "dev-tunnel.sh"
with tempfile.TemporaryDirectory(prefix="herdr-dev-tailscale-") as tmp:
    base = Path(tmp)
    home = base / "home"
    home.mkdir(mode=0o700)
    production = home / ".config" / "herdr-mobile-relay"
    production.mkdir(mode=0o700, parents=True)
    dev = base / "dev"
    dev.mkdir(mode=0o700)
    cli = base / "tailscale"
    herdr = base / "herdr"
    sentinel = base / "unexpected-tool-execution"
    for binary in (cli, herdr):
        binary.write_text(f"#!/bin/sh\nprintf invoked >> '{sentinel}'\nexit 97\n", encoding="utf-8")
        binary.chmod(0o700)

    env = dict(os.environ)
    env.update({
        "HOME": str(home),
        "XDG_CONFIG_HOME": str(home / ".config"),
        "XDG_DATA_HOME": str(home / ".local" / "share"),
        "HERDR_DEV_TAILSCALE_DIR": str(dev),
        "HERDR_DEV_TAILSCALE_BIN": str(cli),
        "HERDR_DEV_HERDR_BIN": str(herdr),
        "HERDR_DEV_HERDR_SOCKET": str(base / "herdr.sock"),
        "HERDR_DEV_TAILSCALE_PORT": "18377",
        "HERDR_DEV_TAILSCALE_PLUGIN_PORT": "18378",
        "HERDR_DEV_TAILSCALE_HTTPS_PORT": "18443",
    })
    for name in ("HERDR_RELAY_ENV", "HERDR_PLUGIN_CONFIG_DIR", "GH_TOKEN"):
        env.pop(name, None)

    def refused(case: str, settings: dict[str, str], state_root: Path = dev) -> None:
        result = subprocess.run(
            [str(script)], env=settings, cwd=root, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
        )
        if result.returncode == 0:
            raise AssertionError(f"{case}: unexpectedly admitted development state")
        if sentinel.exists() or (state_root / "relay.env").exists() or (state_root / ".herdr-dev-tailscale").exists():
            raise AssertionError(f"{case}: touched a CLI or private credential state before admission")
        print(f"PASS dev-tailscale preflight: {case}")

    without_consent = dict(env)
    without_consent.pop("HERDR_DEV_TAILSCALE_ENABLE", None)
    without_consent.pop("HERDR_DEV_TRANSPORT", None)
    refused("requires_explicit_opt_in", without_consent)

    def interactive_refused(case: str, entrypoint: Path, answers: bytes,
                            expected: bytes, **overrides: str) -> None:
        master, slave = pty.openpty()
        settings = dict(without_consent, HERDR_DEV_CONFIG_DIR=str(base / "unused-tunnel"))
        settings.update(overrides)
        try:
            process = subprocess.Popen(
                [str(entrypoint)], env=settings, cwd=root, stdin=slave,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            os.close(slave)
            os.write(master, answers)
            stdout, stderr = process.communicate(timeout=5)
            if process.returncode == 0 or expected not in stdout + stderr:
                raise AssertionError(f"{case}: unsafe interactive outcome: {stdout + stderr!r}")
            if sentinel.exists() or (dev / "relay.env").exists() or (base / "unused-tunnel").exists():
                raise AssertionError(f"{case}: touched a CLI or dev state before consent")
            print(f"PASS dev-tailscale preflight: {case}")
        finally:
            os.close(master)

    interactive_refused("direct_interactive_decline", script, b"n\n", b"Cancelled; nothing was started.")
    interactive_refused("dev_tunnel_tailscale_choice_decline", tunnel_script, b"2\nn\n", b"Cancelled; nothing was started.")
    interactive_refused("consent_does_not_create_root", script, b"y\n",
                        b"Create a private development root first",
                        HERDR_DEV_TAILSCALE_DIR=str(base / "missing"))
    delegated = subprocess.run(
        [str(tunnel_script)], env=dict(without_consent, HERDR_DEV_TRANSPORT="tailscale",
                                        HERDR_DEV_CONFIG_DIR=str(base / "unused-tunnel")),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if delegated.returncode == 0 or b"Interactive: make dev-tailscale" not in delegated.stderr:
        raise AssertionError("noninteractive Tailscale selection bypassed opt-in")
    if sentinel.exists() or (base / "unused-tunnel").exists():
        raise AssertionError("noninteractive Tailscale selection touched a CLI or tunnel state")
    print("PASS dev-tailscale preflight: noninteractive_delegation_requires_opt_in")

    enabled = dict(env, HERDR_DEV_TAILSCALE_ENABLE="1")
    reserved = dict(enabled, HERDR_DEV_TAILSCALE_PORT="8375")
    refused("rejects_production_backend_port", reserved)

    shared_root = dict(enabled, HERDR_DEV_TAILSCALE_DIR=str(production))
    refused("rejects_installed_config_root", shared_root, production)

    dev.chmod(0o755)
    refused("requires_private_directory_mode", enabled)
    dev.chmod(0o700)

    (dev / "relay.env").write_text("HERDR_RELAY_TOKEN=retained-untrusted-state\n", encoding="utf-8")
    before = (dev / "relay.env").read_bytes()
    result = subprocess.run(
        [str(script)], env=enabled, cwd=root, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
    )
    if result.returncode == 0 or (dev / "relay.env").read_bytes() != before or sentinel.exists():
        raise AssertionError("unmarked existing state was accepted or changed")
    print("PASS dev-tailscale preflight: rejects_unmarked_existing_state")

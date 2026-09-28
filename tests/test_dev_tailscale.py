#!/usr/bin/env python3
"""Hosted-only refusal/isolation checks for the explicit managed dev entrypoint.

This suite deliberately never runs a Tailscale CLI or an app/relay listener.
It does not replace the extracted-package managed browser acceptance gate.
"""

import os
from pathlib import Path
import pty
import shutil
import socket
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
    default_socket_dir = home / ".config" / "herdr"
    default_socket_dir.mkdir(mode=0o700)
    default_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    default_socket.bind(str(default_socket_dir / "herdr.sock"))
    fixture_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    fixture_socket.bind(str(base / "herdr.sock"))
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
            if entrypoint == tunnel_script and b"Set up isolated managed Tailscale" in stdout + stderr:
                raise AssertionError("menu selection demanded redundant development consent")
            if case == "menu_uses_all_safe_defaults":
                for value in (
                    b"Private development state: " + str(root / "relay" / ".dev-tailscale").encode(),
                    f"Tailscale CLI from PATH: {cli}".encode(),
                    f"Herdr executable from PATH: {herdr}".encode(),
                    f"Herdr socket path: {default_socket_dir / 'herdr.sock'}".encode(),
                    b"Development ports: relay 8375, plugin 18378, HTTPS Serve 8443",
                ):
                    if value not in stdout + stderr:
                        raise AssertionError(f"{case}: default not selected: {value!r}")
            if sentinel.exists() or (dev / "relay.env").exists() or (base / "unused-tunnel").exists():
                raise AssertionError(f"{case}: touched a CLI or dev state before consent")
            print(f"PASS dev-tailscale preflight: {case}")
        finally:
            os.close(master)

    interactive_refused("direct_interactive_decline", script, b"n\n", b"Cancelled; nothing was started.")
    interactive_refused("dev_tunnel_rejects_relative_state", tunnel_script, b"2\n",
                        b"Choose an absolute private state directory, or unset HERDR_DEV_TAILSCALE_DIR",
                        HERDR_DEV_TAILSCALE_DIR="relative")
    interactive_refused("consent_does_not_create_custom_root", script, b"y\n",
                        b"Create a custom private state directory first",
                        HERDR_DEV_TAILSCALE_DIR=str(base / "missing"))
    checkout_default = root / "relay" / ".dev-tailscale"
    if checkout_default.exists():
        raise AssertionError("hosted fixture expected an unused checkout-local dev root")
    interactive_refused("invalid_cli_does_not_create_default", tunnel_script, b"2\n",
                        b"Not an executable file:",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN=str(base / "missing-cli"))
    interactive_refused("menu_uses_all_safe_defaults", tunnel_script, b"2\n",
                        b"Production and dev-tunnel ports are reserved",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN="",
                        HERDR_DEV_HERDR_BIN="", HERDR_DEV_HERDR_SOCKET="",
                        HERDR_SOCKET_PATH="", HERDR_DEV_TAILSCALE_PORT="8375",
                        PATH=f"{base}:/usr/bin:/bin")
    if checkout_default.exists():
        raise AssertionError("created checkout-local state before validating the selected CLI")
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
    discovered = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_TAILSCALE_BIN="",
                                HERDR_DEV_HERDR_BIN=str(base / "missing-herdr"),
                                PATH=f"{base}:/usr/bin:/bin"),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (discovered.returncode == 0 or
        f"Tailscale CLI from PATH: {cli}".encode() not in discovered.stdout or
        b"Not an executable file:" not in discovered.stderr or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("PATH selection did not locate the fake CLI safely before other validation")
    print("PASS dev-tailscale preflight: resolves_cli_from_path_without_running_it")

    no_cli_path = base / "no-cli-path"
    no_cli_path.mkdir(mode=0o700)
    (no_cli_path / "dirname").symlink_to(shutil.which("dirname"))
    missing_cli = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_TAILSCALE_BIN="", PATH=str(no_cli_path)),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (missing_cli.returncode == 0 or
        b"No executable Tailscale CLI found on PATH" not in missing_cli.stderr or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("missing CLI did not fail closed before development state creation")
    print("PASS dev-tailscale preflight: missing_cli_requires_explicit_path")

    discovered_herdr = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_HERDR_BIN="",
                                HERDR_DEV_TAILSCALE_PORT="8375", PATH=f"{base}:/usr/bin:/bin"),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (discovered_herdr.returncode == 0 or
        f"Herdr executable from PATH: {herdr}".encode() not in discovered_herdr.stdout or
        b"Production and dev-tunnel ports are reserved" not in discovered_herdr.stderr or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("PATH selection did not locate fake Herdr without executing it")
    print("PASS dev-tailscale preflight: resolves_herdr_from_path_without_running_it")

    missing_herdr = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_HERDR_BIN="", PATH=str(no_cli_path)),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (missing_herdr.returncode == 0 or
        b"No executable Herdr found on PATH" not in missing_herdr.stderr or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("missing Herdr did not fail closed before development state creation")
    print("PASS dev-tailscale preflight: missing_herdr_requires_explicit_path")

    inherited_socket = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_HERDR_SOCKET="",
                                HERDR_SOCKET_PATH=str(base / "herdr.sock"),
                                HERDR_DEV_TAILSCALE_PORT="8375"),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (inherited_socket.returncode == 0 or
        f"Herdr socket path: {base / 'herdr.sock'}".encode() not in inherited_socket.stdout or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("explicit inherited Herdr socket was not selected before isolated HOME")
    print("PASS dev-tailscale preflight: inherits_herdr_socket_path")

    missing_socket = subprocess.run(
        [str(script)], env=dict(enabled, HERDR_DEV_HERDR_SOCKET=str(base / "missing.sock")),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (missing_socket.returncode == 0 or
        b"No Herdr Unix socket at" not in missing_socket.stderr or
        sentinel.exists() or (dev / "relay.env").exists()):
        raise AssertionError("missing Herdr socket was not refused before state creation")
    print("PASS dev-tailscale preflight: missing_herdr_socket_fails_early")

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
    fixture_socket.close()
    default_socket.close()

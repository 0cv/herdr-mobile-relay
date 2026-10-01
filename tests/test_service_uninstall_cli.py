#!/usr/bin/env python3
"""Hosted-only disposable fixture for CLI Serve route disposition on service removal."""

from __future__ import annotations

import os
import pty
import select
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]


# A literal backslash and consecutive percent signs exercise every systemd
# escape the installers apply. Decoding the unit twice turns this name into
# DOUBLE_DECODED_ENV_NAME, so a decoy there detects a second decode.
ESCAPED_ENV_NAME = "relay\\x%%.env"
DOUBLE_DECODED_ENV_NAME = "relayx%.env"


def serialized_environment(env_file: Path, quoting: str) -> str:
    """Serialize HERDR_RELAY_ENV with the shipped installer quoting helpers."""
    commands = {
        # Main's installer and plugin rewrite quote the complete assignment.
        "current": 'printf "Environment=%s" "$(systemd_quoted "HERDR_RELAY_ENV=$2")"',
        # Earlier branch installs quoted only the value.
        "legacy": 'printf "Environment=HERDR_RELAY_ENV=%s" "$(systemd_quote_value "$2")"',
    }
    result = subprocess.run(
        ["bash", "-c", '. "$1" && ' + commands[quoting], "_", str(REPO / "relay" / "common.sh"), str(env_file)],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True, text=True,
    )
    line = result.stdout
    if "\\\\x" not in line or "%%%%" not in line:
        raise AssertionError(f"{quoting} serializer did not escape the fixture path: {line!r}")
    return line


def fixture(
    disposition: bytes | None, unpublish_status: int = 0, platform: str = "Linux",
    env_name: str = "relay.env", quoting: str | None = None, decoy: str | None = None,
) -> tuple[int, bytes, str]:
    with tempfile.TemporaryDirectory(prefix="herdr-cli-uninstall-") as temporary:
        root = Path(temporary)
        relay = root / "relay"
        fakebin = root / "bin"
        home = root / "home"
        relay.mkdir()
        fakebin.mkdir()
        home.mkdir()
        for name in ("common.sh", "service.sh", "uninstall-systemd-user-service.sh", "uninstall-service.sh"):
            shutil.copy2(REPO / "relay" / name, relay / name)
        env_file = root / env_name
        env_file.write_text("HERDR_RELAY_TRANSPORT=tailscale-cli\n", encoding="utf-8")
        if decoy is not None:
            # A misdecoded path must not silently select another environment.
            (root / decoy).write_text("HERDR_RELAY_TRANSPORT=cloudflare\n", encoding="utf-8")
        if platform == "Linux":
            unit = home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
            unit.parent.mkdir(parents=True)
            environment = (
                f'Environment=HERDR_RELAY_ENV="{env_file}"' if quoting is None
                else serialized_environment(env_file, quoting)
            )
            unit.write_text(f"[Service]\n{environment}\n", encoding="utf-8")
        else:
            plist = home / "Library" / "LaunchAgents" / "com.herdr-mobile-relay.service.plist"
            plist.parent.mkdir(parents=True)
            plist.write_text("fixture plist", encoding="utf-8")
        calls = root / "calls"
        (fakebin / "uname").write_text(f"#!/bin/sh\nprintf '{platform}\\n'\n", encoding="utf-8")
        (fakebin / "systemctl").write_text(
            "#!/bin/sh\nprintf 'systemctl %s\\n' \"$*\" >> \"$SERVICE_CALLS\"\nexit 0\n",
            encoding="utf-8",
        )
        (fakebin / "launchctl").write_text(
            "#!/bin/sh\nprintf 'launchctl %s\\n' \"$*\" >> \"$SERVICE_CALLS\"\nexit 0\n",
            encoding="utf-8",
        )
        (fakebin / "plutil").write_text(
            "#!/bin/sh\nprintf '%s\\n' \"$PLIST_ENV_FILE\"\n",
            encoding="utf-8",
        )
        (relay / "tailscale-cli.sh").write_text(
            "#!/bin/sh\nprintf 'tailscale-cli %s\\n' \"$*\" >> \"$SERVICE_CALLS\"\n"
            "printf 'route-env=%s\\n' \"$HERDR_RELAY_ENV\" >> \"$SERVICE_CALLS\"\n"
            f"exit {unpublish_status}\n",
            encoding="utf-8",
        )
        for executable in (fakebin / "uname", fakebin / "systemctl", fakebin / "launchctl", fakebin / "plutil", relay / "tailscale-cli.sh"):
            executable.chmod(0o700)
        environment = os.environ.copy()
        environment.pop("HERDR_RELAY_ENV", None)
        environment.pop("HERDR_PLUGIN_CONFIG_DIR", None)
        environment.update({
            "PATH": f"{fakebin}:{environment['PATH']}",
            "HOME": str(home),
            "SERVICE_CALLS": str(calls),
            "PLIST_ENV_FILE": str(env_file),
        })

        if disposition is None:
            result = subprocess.run(
                ["bash", str(relay / "service.sh"), "uninstall"],
                cwd=root,
                env=environment,
                input=b"k\n",
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                check=False,
            )
            return result.returncode, result.stdout, calls.read_text(encoding="utf-8") if calls.exists() else ""

        master, slave = pty.openpty()
        process = subprocess.Popen(
            ["bash", str(relay / "service.sh"), "uninstall"],
            cwd=root,
            env=environment,
            stdin=slave,
            stdout=slave,
            stderr=slave,
            start_new_session=True,
            close_fds=True,
        )
        os.close(slave)
        output = bytearray()
        try:
            deadline = time.monotonic() + 10
            while b"Route disposition" not in output and process.poll() is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AssertionError("service uninstaller did not prompt for route disposition")
                ready, _, _ = select.select([master], [], [], remaining)
                if not ready:
                    continue
                try:
                    output.extend(os.read(master, 4096))
                except OSError:
                    break
            if b"Route disposition" not in output:
                raise AssertionError(f"route prompt missing: {output.decode(errors='replace')}")
            os.write(master, disposition + b"\n")
            while process.poll() is None:
                ready, _, _ = select.select([master], [], [], 10)
                if not ready:
                    raise AssertionError("service uninstaller did not finish after route choice")
                try:
                    output.extend(os.read(master, 4096))
                except OSError:
                    break
            return process.wait(), bytes(output), calls.read_text(encoding="utf-8") if calls.exists() else ""
        finally:
            os.close(master)
            if process.poll() is None:
                process.kill()
                process.wait()


def main() -> None:
    status, output, calls = fixture(b"k")
    text = output.decode(errors="replace")
    if status != 0 or "route will remain" not in text.lower():
        raise AssertionError(f"keep-route disposition failed ({status}): {text}")
    if "tailscale-cli unpublish" in calls:
        raise AssertionError("keep-route disposition attempted a persistent route mutation")
    if "systemctl --user disable --now herdr-mobile-relay.service" not in calls:
        raise AssertionError(f"service was not removed after explicit keep: {calls}")

    status, output, calls = fixture(b"r")
    if status != 0 or calls.find("tailscale-cli unpublish") < 0 or calls.find("tailscale-cli unpublish") > calls.find("systemctl "):
        raise AssertionError(f"remove-route disposition was not first: {output.decode(errors='replace')} {calls}")

    status, output, calls = fixture(b"c")
    if status == 0 or calls:
        raise AssertionError(f"cancel disposition changed services: {output.decode(errors='replace')} {calls}")

    status, output, calls = fixture(b"k", platform="Darwin")
    if status != 0 or "launchctl bootout" not in calls or "route will remain" not in output.decode(errors="replace").lower():
        raise AssertionError(f"launchd installed-environment disposition failed: {output.decode(errors='replace')} {calls}")

    status, output, calls = fixture(b"r", unpublish_status=1)
    if status == 0 or "systemctl " in calls or "launchctl " in calls or "service was left installed" not in output.decode(errors="replace"):
        raise AssertionError(f"failed route removal still uninstalled the service: {output.decode(errors='replace')} {calls}")

    status, output, calls = fixture(None)
    if status == 0 or calls or "interactive route disposition is required" not in output.decode(errors="replace").lower():
        raise AssertionError(f"noninteractive service removal lacked explicit route disposition: {output.decode(errors='replace')} {calls}")
    for quoting in ("current", "legacy"):
        for decoy in (None, DOUBLE_DECODED_ENV_NAME):
            status, output, calls = fixture(b"k", env_name=ESCAPED_ENV_NAME, quoting=quoting, decoy=decoy)
            text = output.decode(errors="replace")
            if (status != 0 or "route will remain" not in text.lower() or
                    "systemctl --user disable --now herdr-mobile-relay.service" not in calls):
                raise AssertionError(f"{quoting} escaped environment path did not round-trip (decoy={decoy}): {text} {calls}")
        status, output, calls = fixture(b"r", env_name=ESCAPED_ENV_NAME, quoting=quoting, decoy=DOUBLE_DECODED_ENV_NAME)
        route_envs = [line[len("route-env="):] for line in calls.splitlines() if line.startswith("route-env=")]
        if status != 0 or len(route_envs) != 1 or not route_envs[0].endswith("/" + ESCAPED_ENV_NAME):
            raise AssertionError(f"{quoting} route removal used the wrong environment: {output.decode(errors='replace')} {calls}")
    print("PASS service-uninstall fixture: persistent CLI Serve route receives explicit remove/keep/cancel disposition")
    print("PASS service-uninstall fixture: escaped installed environment paths decode once in current and legacy quoting")


if __name__ == "__main__":
    main()

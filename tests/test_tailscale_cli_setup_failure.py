#!/usr/bin/env python3
"""Hosted-only CLI setup rollback coverage with synthetic relay/service tools."""

from __future__ import annotations

import os
import pty
import shutil
import subprocess
import tempfile
from pathlib import Path

if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing CLI setup lifecycle tests outside hosted CI")

REPO = Path(__file__).resolve().parents[1]


def write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o700)


def run_setup(prior_definition: bool) -> tuple[int, str, list[str], str | None]:
    temporary = tempfile.TemporaryDirectory(prefix="herdr-cli-setup-rollback-")
    root = Path(temporary.name)
    relay = root / "relay"
    fakebin = root / "bin"
    home = root / "home"
    relay.mkdir(mode=0o700)
    fakebin.mkdir(mode=0o700)
    home.mkdir(mode=0o700)
    shutil.copy2(REPO / "relay" / "common.sh", relay / "common.sh")
    shutil.copy2(REPO / "relay" / "tailscale-cli.sh", relay / "tailscale-cli.sh")

    unit = home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
    if prior_definition:
        unit.parent.mkdir(parents=True)
        unit.write_text("preexisting service definition\n", encoding="utf-8")

    env_file = root / "relay.env"
    cli_binary = root / "tailscale-fixture"
    write_executable(cli_binary, "#!/bin/sh\nexit 97\n")
    env_file.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
        "HERDR_RELAY_INSTANCE_ID=fixture-instance\n"
        "HERDR_RELAY_TOKEN=fixture-token\n"
        "HERDR_RELAY_PORT=18377\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli_binary}\n"
        "HERDR_TAILSCALE_CLI_NODE_ID=fixture-node\n"
        "HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:8443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={home}/.local/state/herdr/registration\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={home}/.local/state/herdr/coordination\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=8443\n"
        "HERDR_PHONE_APP_URL=https://app.fixture.invalid\n",
        encoding="utf-8",
    )

    fake_relay = root / "herdr-fixture"
    write_executable(
        fake_relay,
        "#!/usr/bin/env python3\n"
        "import json, os, sys\n"
        "args = sys.argv[1:]\n"
        "def record(value):\n"
        "    with open(os.environ['CLI_SETUP_EVENTS'], 'a', encoding='utf-8') as stream:\n"
        "        stream.write(value + '\\n')\n"
        "if args and args[0] == 'json-field':\n"
        "    data = json.load(sys.stdin)\n"
        "    value = data.get(args[2])\n"
        "    if args[1] == 'string' and isinstance(value, str): print(value)\n"
        "    elif args[1] == 'bool' and isinstance(value, bool): print(str(value).lower())\n"
        "    sys.exit(0)\n"
        "if len(args) >= 2 and args[0] == 'tailscale-cli':\n"
        "    operation = args[1]\n"
        "    if operation in ('activation-check',): sys.exit(0)\n"
        "    if operation == 'resolve-binary':\n"
        "        print(args[args.index('--binary') + 1]); sys.exit(0)\n"
        "    if operation == 'preflight':\n"
        "        print(json.dumps({'node_id':'fixture-node','dns_name':'relay.fixture.invalid',"
        "'origin':'https://relay.fixture.invalid:8443','profile':'fixture'})); sys.exit(0)\n"
        "    if operation == 'publish': record('publish'); sys.exit(3)\n"
        "    if operation in ('reserve-backend-port', 'release-backend-port'):\n"
        "        record(operation); sys.exit(0)\n"
        "sys.exit(97)\n",
    )
    write_executable(fakebin / "uname", "#!/bin/sh\nprintf 'Linux\\n'\n")
    write_executable(
        relay / "service.sh",
        "#!/bin/bash\nset -euo pipefail\n"
        "printf 'service:%s\\n' \"$1\" >> \"$CLI_SETUP_EVENTS\"\n"
        "case \"$1\" in\n"
        "  install) mkdir -p \"$(dirname \"$CLI_SETUP_UNIT\")\"; touch \"$CLI_SETUP_UNIT\" ;;\n"
        "  rollback-cli-setup) rm -f \"$CLI_SETUP_UNIT\" ;;\n"
        "  stop) ;;\n"
        "  *) exit 2 ;;\n"
        "esac\n",
    )
    write_executable(
        fakebin / "curl",
        "#!/bin/sh\nprintf '%s\\n' '{\"status\":\"ok\",\"readiness\":\"ready\","
        "\"transport\":\"tailscale-cli\",\"instance\":\"fixture-instance\","
        "\"tailscale_cli_origin\":\"https://relay.fixture.invalid:8443\"}'\n",
    )

    events_path = root / "events"
    environment = os.environ.copy()
    for name in ("HERDR_PLUGIN_CONFIG_DIR", "HERDR_TAILSCALE_CLI_COORDINATION_ROOT"):
        environment.pop(name, None)
    environment.update({
        "PATH": f"{fakebin}:{environment['PATH']}",
        "HOME": str(home),
        "HERDR_RELAY_ENV": str(env_file),
        "HERDR_RELAY_BIN": str(fake_relay),
        "HERDR_RELAY_CONTROL_RUN_ID": "fixture-control-run",
        "HERDR_RELAY_PAIRING_SOCKET": str(root / "pairing.sock"),
        "HERDR_TAILSCALE_CLI_ENABLE": "1",
        "CLI_SETUP_EVENTS": str(events_path),
        "CLI_SETUP_UNIT": str(unit),
    })
    master, slave = pty.openpty()
    process: subprocess.Popen[bytes] | None = None
    try:
        process = subprocess.Popen(
            ["/bin/bash", str(relay / "tailscale-cli.sh"), "setup"],
            cwd=root,
            env=environment,
            stdin=slave,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        os.close(slave)
        slave = -1
        os.write(master, b"y\n")
        stdout, stderr = process.communicate(timeout=20)
        events = events_path.read_text(encoding="utf-8").splitlines() if events_path.exists() else []
        unit_contents = unit.read_text(encoding="utf-8") if unit.exists() else None
        return process.returncode, stdout.decode(errors="replace") + stderr.decode(errors="replace"), events, unit_contents
    finally:
        if process is not None and process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
        if slave >= 0:
            os.close(slave)
        temporary.cleanup()


def rollback_action(platform: str) -> tuple[int, str, list[str], bool]:
    with tempfile.TemporaryDirectory(prefix="herdr-cli-service-rollback-") as temporary:
        root = Path(temporary)
        relay = root / "relay"
        fakebin = root / "bin"
        home = root / "home"
        relay.mkdir()
        fakebin.mkdir()
        home.mkdir()
        shutil.copy2(REPO / "relay" / "common.sh", relay / "common.sh")
        shutil.copy2(REPO / "relay" / "service.sh", relay / "service.sh")
        if platform == "Linux":
            service_definition = home / ".config/systemd/user/herdr-mobile-relay.service"
        else:
            service_definition = home / "Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
        service_definition.parent.mkdir(parents=True)
        service_definition.write_text("new service definition\n", encoding="utf-8")
        calls = root / "calls"
        write_executable(fakebin / "uname", f"#!/bin/sh\nprintf '{platform}\\n'\n")
        for name in ("systemctl", "launchctl"):
            write_executable(
                fakebin / name,
                "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$SERVICE_CALLS\"\nexit 0\n",
            )
        environment = os.environ.copy()
        environment.update({
            "PATH": f"{fakebin}:{environment['PATH']}",
            "HOME": str(home),
            "SERVICE_CALLS": str(calls),
            "HERDR_CLI_SETUP_ROLLBACK": "1",
        })
        result = subprocess.run(
            ["/bin/bash", str(relay / "service.sh"), "rollback-cli-setup"],
            cwd=root,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=10,
            check=False,
        )
        recorded = calls.read_text(encoding="utf-8").splitlines() if calls.exists() else []
        return result.returncode, result.stdout.decode(errors="replace"), recorded, service_definition.exists()


def main() -> None:
    status, output, events, unit_contents = run_setup(prior_definition=False)
    if status != 3 or unit_contents is not None:
        raise AssertionError(f"new-service pre-dispatch failure was not rolled back ({status}): {output} {events}")
    required_order = ["reserve-backend-port", "service:install", "publish", "service:rollback-cli-setup", "release-backend-port"]
    positions = [events.index(event) if event in events else -1 for event in required_order]
    if positions != sorted(positions) or any(position < 0 for position in positions):
        raise AssertionError(f"service rollback and reservation release order was unsafe: {events}")

    status, output, events, unit_contents = run_setup(prior_definition=True)
    if status == 0 or unit_contents != "preexisting service definition\\n":
        raise AssertionError(f"pre-existing service definition was changed ({status}): {output} {events}")
    if "existing service definition already exists" not in output.lower() or events:
        raise AssertionError(f"existing service setup was not refused before mutation: {output} {events}")

    for platform in ("Linux", "Darwin"):
        status, output, calls, definition_exists = rollback_action(platform)
        if status != 0 or definition_exists:
            raise AssertionError(f"{platform} rollback action failed: {output} {calls}")
        expected = "systemctl --user disable --now herdr-mobile-relay.service" if platform == "Linux" else "launchctl bootout"
        if not any(expected in call for call in calls):
            raise AssertionError(f"{platform} rollback did not disable/unload its service: {calls}")
    print("PASS CLI setup failure fixture: non-dispatched publish rolls back new service before safe reservation release")


if __name__ == "__main__":
    main()

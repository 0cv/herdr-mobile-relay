#!/usr/bin/env python3
"""Hosted-only CLI setup rollback coverage with synthetic relay/service tools."""

from __future__ import annotations

import os
import pty
import shutil
import subprocess
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing CLI setup lifecycle tests outside hosted CI")

REPO = Path(__file__).resolve().parents[1]


def write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o700)


def service_paths(home: Path, platform: str) -> tuple[Path, Path]:
    if platform == "Linux":
        service_root = home / ".config" / "systemd" / "user"
        return service_root / "herdr-mobile-relay.service", service_root / "herdr-remote.service"
    service_root = home / "Library" / "LaunchAgents"
    return service_root / "com.herdr-mobile-relay.service.plist", service_root / "com.herdr-remote.service.plist"


def run_setup(
    prior_definition: bool | str,
    *,
    platform: str = "Linux",
    appear_after_reserve: str | None = None,
    shared_reservation: Path | None = None,
    reserve_ready: Path | None = None,
    reserve_release: Path | None = None,
) -> tuple[int, str, list[str], str | None, str | None]:
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

    unit, legacy_unit = service_paths(home, platform)
    reservation = shared_reservation or root / "backend-reservation.json"
    if prior_definition:
        selected_definition = legacy_unit if prior_definition == "legacy" else unit
        selected_definition.parent.mkdir(parents=True)
        selected_definition.write_text("preexisting service definition\n", encoding="utf-8")
    else:
        selected_definition = legacy_unit if appear_after_reserve == "legacy" else unit

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
        "from pathlib import Path\n"
        "import time\n"
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
        "    if operation == 'reserve-backend-port':\n"
        "        reservation = Path(os.environ['CLI_SETUP_RESERVATION'])\n"
        "        try: descriptor = os.open(reservation, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)\n"
        "        except FileExistsError: record('reserve-conflict'); sys.exit(1)\n"
        "        with os.fdopen(descriptor, 'w', encoding='utf-8') as stream: stream.write('reserved by concurrent setup\\n')\n"
        "        record(operation)\n"
        "        ready = os.environ.get('CLI_SETUP_RESERVE_READY', '')\n"
        "        release = os.environ.get('CLI_SETUP_RESERVE_RELEASE', '')\n"
        "        if ready: Path(ready).touch()\n"
        "        while release and not Path(release).exists(): time.sleep(0.01)\n"
        "        appeared = os.environ.get('CLI_SETUP_APPEAR_AFTER_RESERVE', '')\n"
        "        if appeared:\n"
        "            target = os.environ['CLI_SETUP_UNIT'] if appeared == 'current' else os.environ['CLI_SETUP_LEGACY_UNIT']\n"
        "            Path(target).parent.mkdir(parents=True, exist_ok=True)\n"
        "            Path(target).write_text('installed by concurrent setup\\n', encoding='utf-8')\n"
        "        sys.exit(0)\n"
        "    if operation == 'release-backend-port':\n"
        "        record(operation); Path(os.environ['CLI_SETUP_RESERVATION']).unlink(missing_ok=True); sys.exit(0)\n"
        "sys.exit(97)\n",
    )
    write_executable(fakebin / "uname", f"#!/bin/sh\nprintf '{platform}\\n'\n")
    if platform == "Darwin":
        uid = os.getuid()
        write_executable(
            fakebin / "stat",
            f"#!/bin/sh\ncase \"$2\" in %Lp) printf '700\\n' ;; %u) printf '{uid}\\n' ;; *) exit 2 ;; esac\n",
        )
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
        "CLI_SETUP_LEGACY_UNIT": str(legacy_unit),
        "CLI_SETUP_RESERVATION": str(reservation),
        "CLI_SETUP_APPEAR_AFTER_RESERVE": appear_after_reserve or "",
        "CLI_SETUP_RESERVE_READY": str(reserve_ready) if reserve_ready else "",
        "CLI_SETUP_RESERVE_RELEASE": str(reserve_release) if reserve_release else "",
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
        definition_contents = selected_definition.read_text(encoding="utf-8") if selected_definition.exists() else None
        reservation_contents = reservation.read_text(encoding="utf-8") if reservation.exists() else None
        return (
            process.returncode,
            stdout.decode(errors="replace") + stderr.decode(errors="replace"),
            events,
            definition_contents,
            reservation_contents,
        )
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


def installer_refusal(platform: str, definition_kind: str) -> tuple[int, str, str | None, bool, list[str]]:
    with tempfile.TemporaryDirectory(prefix="herdr-cli-installer-guard-") as temporary:
        root = Path(temporary)
        relay = root / "relay"
        fakebin = root / "bin"
        home = root / "home"
        relay.mkdir()
        fakebin.mkdir()
        home.mkdir()
        shutil.copy2(REPO / "relay" / "common.sh", relay / "common.sh")
        installer_name = "install-systemd-user-service.sh" if platform == "Linux" else "install-service.sh"
        shutil.copy2(REPO / "relay" / installer_name, relay / installer_name)
        current, legacy = service_paths(home, platform)
        selected = current if definition_kind == "current" else legacy
        selected.parent.mkdir(parents=True)
        selected.write_text("pre-existing service definition\n", encoding="utf-8")
        env_file = root / "relay.env"
        env_file.write_text("HERDR_RELAY_TRANSPORT=tailscale-cli\n", encoding="utf-8")
        calls = root / "calls"
        write_executable(fakebin / "uname", f"#!/bin/sh\nprintf '{platform}\\n'\n")
        for name in ("systemctl", "launchctl"):
            write_executable(
                fakebin / name,
                "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$INSTALLER_CALLS\"\nexit 0\n",
            )
        environment = os.environ.copy()
        environment.update({
            "PATH": f"{fakebin}:{environment['PATH']}",
            "HOME": str(home),
            "HERDR_RELAY_ENV": str(env_file),
            "HERDR_CLI_SETUP_NEW_SERVICE": "1",
            "INSTALLER_CALLS": str(calls),
        })
        result = subprocess.run(
            ["/bin/bash", str(relay / installer_name)],
            cwd=root,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=10,
            check=False,
        )
        calls_recorded = calls.read_text(encoding="utf-8").splitlines() if calls.exists() else []
        contents = selected.read_text(encoding="utf-8") if selected.exists() else None
        return result.returncode, result.stdout.decode(errors="replace"), contents, current.exists(), calls_recorded


def main() -> None:
    status, output, events, unit_contents, reservation = run_setup(prior_definition=False)
    if status != 3 or unit_contents is not None or reservation is not None:
        raise AssertionError(f"new-service pre-dispatch failure was not rolled back ({status}): {output} {events}")
    required_order = ["reserve-backend-port", "service:install", "publish", "service:rollback-cli-setup", "release-backend-port"]
    positions = [events.index(event) if event in events else -1 for event in required_order]
    if positions != sorted(positions) or any(position < 0 for position in positions):
        raise AssertionError(f"service rollback and reservation release order was unsafe: {events}")

    with tempfile.TemporaryDirectory(prefix="herdr-cli-concurrent-setup-") as temporary:
        root = Path(temporary)
        reservation = root / "shared-reservation.json"
        ready = root / "first-reservation-ready"
        release = root / "allow-first-setup-to-continue"
        with ThreadPoolExecutor(max_workers=1) as executor:
            first_attempt = executor.submit(
                run_setup,
                False,
                shared_reservation=reservation,
                reserve_ready=ready,
                reserve_release=release,
            )
            deadline = time.monotonic() + 15
            while not ready.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            if not ready.exists():
                release.touch()
                raise AssertionError("first concurrent setup did not reach its held reservation")
            try:
                second_status, second_output, second_events, _, second_reservation = run_setup(
                    False, shared_reservation=reservation
                )
                if second_status == 0 or second_events != ["reserve-conflict"] or not second_reservation:
                    raise AssertionError(
                        f"second setup did not fail without releasing the first reservation: "
                        f"{second_output} {second_events} {second_reservation}"
                    )
            finally:
                release.touch()
            first_status, first_output, first_events, first_unit, first_reservation = first_attempt.result(timeout=30)
            if first_status != 3 or first_unit is not None or first_reservation is not None:
                raise AssertionError(
                    f"winning concurrent setup lost its reservation or rollback: {first_output} {first_events}"
                )

    for platform in ("Linux", "Darwin"):
        for prior_definition in ("current", "legacy"):
            status, output, events, definition_contents, reservation = run_setup(
                prior_definition=prior_definition, platform=platform
            )
            if status == 0 or definition_contents != "preexisting service definition\n" or reservation is not None:
                raise AssertionError(
                    f"{platform} {prior_definition} service definition was changed ({status}): {output} {events}"
                )
            if "service definition already exists" not in output.lower() or events:
                raise AssertionError(f"{platform} existing service setup was not refused before mutation: {output} {events}")

        status, output, events, definition_contents, reservation = run_setup(
            prior_definition=False, platform=platform, appear_after_reserve="legacy"
        )
        if status == 0 or definition_contents != "installed by concurrent setup\n" or not reservation:
            raise AssertionError(f"{platform} legacy-service race changed ownership unsafely: {output} {events}")
        if events != ["reserve-backend-port"] or "reservation retained" not in output.lower():
            raise AssertionError(f"{platform} legacy-service race released another setup's reservation: {output} {events}")

        for definition_kind in ("current", "legacy"):
            status, output, contents, current_exists, calls = installer_refusal(platform, definition_kind)
            if status != 4 or contents != "pre-existing service definition\n" or current_exists != (definition_kind == "current"):
                raise AssertionError(
                    f"{platform} installer did not fail closed for {definition_kind} service: {status} {output} {calls}"
                )
            if calls or "refuses" not in output.lower():
                raise AssertionError(f"{platform} installer mutated services before refusing: {output} {calls}")

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

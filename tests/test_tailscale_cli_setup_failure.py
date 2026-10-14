#!/usr/bin/env python3
"""Hosted-only refusal and actual internal cleanup with inert finite managers."""
from __future__ import annotations
import os
import sys
# Admission precedes helper imports, discovery, roots and subprocesses.
if (os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
        or os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1" or os.getuid() == 0):
    raise SystemExit("CLI setup fixtures require genuine disposable non-root hosted CI and launcher admission")
if sys.version_info < (3, 11): raise SystemExit("CLI setup fixtures require Python >=3.11")
import hashlib
import shutil
import signal
import stat
import subprocess
import tempfile
from pathlib import Path
from test_service_uninstall_cli import ADAPTER, Fixture, snapshot
REPO = Path(__file__).resolve().parents[1]


def copy_source(name: str, destination: Path) -> None:
    source = REPO / "relay" / name
    shutil.copy2(source, destination / name)
    assert hashlib.sha256(source.read_bytes()).digest() == hashlib.sha256((destination / name).read_bytes()).digest()
    assert stat.S_IMODE(source.stat().st_mode) == stat.S_IMODE((destination / name).stat().st_mode)


def write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o700)


def owned_run(argv: list[str], root: Path, environment: dict[str, str]) -> subprocess.CompletedProcess[bytes]:
    process = subprocess.Popen(argv, cwd=root, env=environment, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    try:
        output = process.communicate(timeout=20)[0]
        return subprocess.CompletedProcess(argv, process.returncode, output)
    finally:
        try: os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        process.wait(timeout=2)


def service_paths(home: Path, platform: str) -> tuple[Path, Path]:
    if platform == "Linux":
        service_root = home / ".config" / "systemd" / "user"
        return service_root / "herdr-mobile-relay.service", service_root / "herdr-remote.service"
    service_root = home / "Library" / "LaunchAgents"
    return service_root / "com.herdr-mobile-relay.service.plist", service_root / "com.herdr-remote.service.plist"


def run_setup(platform: str, prior: str, guard_status: int = 0) -> None:
    # Actual CLI/common bytes. A supported synthetic read-only activation guard
    # is NOT CLI access or installed activation permission. Setup must still
    # refuse. Every service/CLI/network/server attempt is separately rejected.
    assert guard_status in (0, 23, 69)
    with tempfile.TemporaryDirectory(prefix="herdr-cli-setup-refusal-") as temporary:
        present = {"absent": (), "current": (0,), "legacy": (1,)}[prior]
        fixture = Fixture(Path(temporary).resolve(), platform, present=present, mode="tailscale-cli")
        copy_source("tailscale-cli.sh", fixture.relay)
        events = fixture.root / 'setup-events'
        adapter = '#!' + sys.executable + '\n' + '''import os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name; args = sys.argv[1:]
with pathlib.Path(os.environ['SETUP_EVENTS']).open('a') as stream:
    stream.write(name + ':' + ' '.join(args) + '\\n')
if name == 'guard-relay' and args == ['tailscale-cli', 'activation-check']:
    raise SystemExit(int(os.environ['SETUP_GUARD_STATUS']))
raise SystemExit(97)
'''
        for name in ('systemctl', 'launchctl', 'tailscale', 'curl'):
            write_executable(fixture.bin / name, adapter)
        # A rejecting service seam here; actual internal cleanup is exercised
        # below through the exact copied service.sh, not this refusal adapter.
        write_executable(fixture.relay / 'service.sh', adapter)
        guard = fixture.root / 'guard-relay'; write_executable(guard, adapter)
        cli = fixture.root / 'selected-cli'; write_executable(cli, adapter)
        fixture.environment.update(HERDR_RELAY_BIN=str(guard), HERDR_TAILSCALE_CLI_BIN=str(cli),
                                   SETUP_EVENTS=str(events), SETUP_GUARD_STATUS=str(guard_status))
        protected = (snapshot(fixture.home), snapshot(fixture.env_file), snapshot(fixture.private))
        result = owned_run(['/bin/bash', str(fixture.relay / 'tailscale-cli.sh'), 'setup'], fixture.root, fixture.environment)
        assert protected == (snapshot(fixture.home), snapshot(fixture.env_file), snapshot(fixture.private))
        calls = events.read_text().splitlines() if events.exists() else []
        assert calls == ['guard-relay:tailscale-cli activation-check'], calls
        assert result.returncode == (guard_status or 1)
        if guard_status == 0:
            assert b'Installed-service CLI setup is disabled' in result.stdout
            assert b'No Tailscale CLI, service manager, or relay server was contacted.' in result.stdout
        assert b'has been installed' not in result.stdout and b'Installed and verified' not in result.stdout
        assert all(value not in result.stdout for value in
                   (b'fixture-saved-canary', b'fixture-ambient-canary', b'fixture-ambient-github-canary'))


def rollback_action(platform: str, fault: str = "", *, cached: bool = False, admitted: bool = True, extra: bool = False) -> None:
    # Reviewed strict dynamic DATA adapters and actual copied service/common.
    # Legacy selection metadata is coherent but cleanup authority is current-only.
    with tempfile.TemporaryDirectory(prefix="herdr-cli-service-rollback-") as temporary:
        fixture = Fixture(Path(temporary).resolve(), platform, present=(1,) if cached else (0, 1), mode="tailscale-cli", fault=fault)
        for name in ('common.sh', 'service.sh'):
            actual = REPO / 'relay' / name; copied = fixture.relay / name
            assert hashlib.sha256(actual.read_bytes()).digest() == hashlib.sha256(copied.read_bytes()).digest()
            assert stat.S_IMODE(actual.stat().st_mode) == stat.S_IMODE(copied.stat().st_mode)
        if admitted: fixture.environment["HERDR_CLI_SETUP_ROLLBACK"] = "1"
        if cached:
            fixture.model['states'][fixture.labels[0]].update(state='active' if platform == 'Linux' else 'loaded', enabled=True)
            fixture.save()
        if fault == 'disable-noop':
            adapter = ADAPTER.replace("if op == 'disable': s['enabled'] = False", "if op == 'disable' and not fault(label, 'disable-noop'): s['enabled'] = False")
            for name in ('systemctl', 'launchctl'):
                write_executable(fixture.bin / name, '#!' + sys.executable + '\n' + adapter)
        if fault == 'lifecycle-output':
            fixture.model.update(lifecycle_seam='systemd' if platform == 'Linux' else 'label',
                                 lifecycle_text='\x00', lifecycle_status=0)
            fixture.save()
        current, legacy = fixture.definitions
        preserved = (snapshot(legacy), snapshot(fixture.env_file), snapshot(fixture.private))
        affected = snapshot(current)
        args = ('rollback-cli-setup', 'unexpected') if extra else ('rollback-cli-setup',)
        status, output, events = fixture.run(args=args)
        assert preserved == (snapshot(legacy), snapshot(fixture.env_file), snapshot(fixture.private))
        assert not any(e['op'] in ('unpublish', 'release-backend-port', 'restart', 'bootstrap', 'enable', 'kickstart') for e in events)
        if fault or not admitted or extra:
            assert status != 0 and snapshot(current) == affected
            assert not any(e['op'] == 'remove' for e in events)
            if not admitted or extra: assert status == 2 and not events
            assert b'cleanup verified' not in output
        else:
            assert status == 0 and not current.exists()
            assert b'Current relay service-definition cleanup verified' in output
            assert [e['label'] for e in events if e['op'] == 'remove'] == [fixture.labels[0]]
            assert any(e['op'] == ('stop' if platform == 'Linux' else 'bootout') for e in events)


def installer_refusal(platform: str, definition_kind: str) -> None:
    with tempfile.TemporaryDirectory(prefix="herdr-cli-installer-guard-") as temporary:
        root = Path(temporary).resolve()
        relay = root / "relay"; fakebin = root / "bin"; home = root / "home"
        for directory in (relay, fakebin, home): directory.mkdir(mode=0o700)
        installer_name = "install-systemd-user-service.sh" if platform == "Linux" else "install-service.sh"
        for name in ("common.sh", installer_name, "native-install-transaction.sh"):
            copy_source(name, relay)
        current, legacy = service_paths(home, platform)
        selected = current if definition_kind == "current" else legacy
        selected.parent.mkdir(parents=True)
        # This definition is never parsed: unavailable-policy refusal precedes
        # native admission. Internal cleanup above uses coherent real definitions.
        selected.write_text("pre-existing service definition\n", encoding="utf-8"); selected.chmod(0o600)
        env_file = root / "relay.env"
        env_file.write_text("HERDR_RELAY_TRANSPORT=tailscale-cli\n", encoding="utf-8"); env_file.chmod(0o600)
        calls = root / "calls"
        write_executable(fakebin / "uname", f"#!/bin/sh\nprintf '{platform}\\n'\n")
        for name in ("systemctl", "launchctl"):
            write_executable(fakebin / name, '#!/bin/sh\nprintf "%s\\n" "$*" >> "$INSTALLER_CALLS"\nexit 97\n')
        environment = {"PATH": f"{fakebin}:/usr/bin:/bin", "LC_ALL": "C", "HOME": str(home),
                       "HERDR_RELAY_ENV": str(env_file), "HERDR_CLI_SETUP_NEW_SERVICE": "1", "INSTALLER_CALLS": str(calls)}
        protected = home / 'private'; protected.mkdir(mode=0o700)
        for name in ('device', 'journal', 'reservation', 'route', 'recovery', 'bystander'):
            q = protected / name; q.write_text('protected fixture ' + name + '\n'); q.chmod(0o600)
        before = (snapshot(home), snapshot(env_file))
        result = owned_run(["/bin/bash", str(relay / installer_name)], root, environment)
        assert before == (snapshot(home), snapshot(env_file))
        assert result.returncode == 1 and b"Unavailable background transport." in result.stdout
        assert not calls.exists()
        assert current.exists() == (definition_kind == "current")
        assert b'Installed and verified' not in result.stdout


def main() -> None:
    for platform in ("Linux", "Darwin"):
        for prior in ('absent', 'current', 'legacy'):
            for guard_status in (0, 23, 69): run_setup(platform, prior, guard_status)
        for definition_kind in ("current", "legacy"): installer_refusal(platform, definition_kind)
        rollback_action(platform)
        rollback_action(platform, admitted=False)
        rollback_action(platform, extra=True)
        faults = ('query-fail', 'malformed', 'stop-noop', 'disable-fail', 'disable-noop', 'lifecycle-output',
                  'stop-fail' if platform == 'Linux' else 'bootout-fail',
                  'contradictory' if platform == 'Linux' else 'domain-fail',
                  'unknown' if platform == 'Linux' else 'disabled-fail')
        for fault in faults: rollback_action(platform, fault)
        rollback_action(platform, 'stop-noop', cached=True)
    print("PASS CLI setup refusal and actual dynamic internal cleanup fixtures")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Hosted-only refusal/isolation checks for managed Tailscale CLI fixtures.

Any service process in this suite is paired with a named fake executable and
private temporary state. It never invokes a real Tailscale CLI/daemon or a
personal service. The separate extracted-package browser gate exercises the
packaged CLI-backed app only inside its disposable network-disabled container.
"""

import os
from pathlib import Path
import pty
import shlex
import shutil
import socket
import subprocess
import tempfile

if os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1":
    raise SystemExit("Refusing development launcher tests outside hosted CI")

root = Path(__file__).resolve().parents[1]
script = root / "relay" / "dev-tailscale.sh"
tunnel_script = root / "relay" / "dev-tunnel.sh"
menu_source = tunnel_script.read_text(encoding="utf-8")
if 'read -r -p "Choice [1]: "' not in menu_source or "''|1) ;;" not in menu_source:
    raise AssertionError("pressing Enter must continue to select the temporary-tunnel option 1")
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
    cli_relay = base / "fake-cli-relay"
    activation_record = base / "activation-check-record"
    sentinel = base / "unexpected-tool-execution"
    cli_relay.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' \"$*\" >> \"$ACTIVATION_CHECK_RECORD\"\n"
        "if [ \"$1\" = tailscale-cli ] && [ \"$2\" = activation-check ] && [ \"$3\" = --scope ] && [ \"$4\" = development ]; then exit 0; fi\n"
        "echo 'production CLI activation remains disabled' >&2\n"
        "exit 2\n",
        encoding="utf-8",
    )
    cli_relay.chmod(0o700)
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
    for name in ("HERDR_RELAY_ENV", "HERDR_PLUGIN_CONFIG_DIR", "GH_TOKEN",
                 "HERDR_DEV_TAILSCALE_CLI_PORT", "HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT",
                 "HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT"):
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
                    b"1. Temporary Cloudflare tunnel",
                    b"quick public URL + QR",
                    b"no Tailscale needed",
                    b"2. CLI-backed Tailscale Serve",
                    b"for the supported App Store Tailscale 1.102.4 profile on macOS/arm64",
                    b"route persists after stop",
                    b"3. Legacy Tailscale Serve",
                    b"supported standalone tailscaled",
                    b"Choice [1]: ",
                    b"Private development state: " + str(root / "relay" / ".dev-tailscale").encode(),
                    f"Tailscale CLI from PATH: {cli}".encode(),
                    f"Herdr executable from PATH: {herdr}".encode(),
                    f"Herdr socket path: {default_socket_dir / 'herdr.sock'}".encode(),
                    b"Development ports: relay 8375, plugin 18378, HTTPS Serve 8443",
                ):
                    if value not in stdout + stderr:
                        raise AssertionError(f"{case}: default not selected: {value!r}")
            if case == "app_store_supported_version_keeps_cli_option_available":
                output = stdout + stderr
                if (b"2. CLI-backed Tailscale Serve" not in output or
                    b"[UNAVAILABLE: Only the App Store Tailscale 1.102.4 profile" in output):
                    raise AssertionError(f"{case}: supported profile was incorrectly marked unavailable: {output!r}")
            if sentinel.exists() or (dev / "relay.env").exists() or (base / "unused-tunnel").exists():
                raise AssertionError(f"{case}: touched a CLI or dev state before consent")
            print(f"PASS dev-tailscale preflight: {case}")
        finally:
            os.close(master)

    interactive_refused("direct_interactive_decline", script, b"n\n", b"Cancelled; nothing was started.")
    interactive_refused("explicit_cli_transport_requires_opt_in", tunnel_script, b"n\n", b"Cancelled; nothing was started.",
                        HERDR_DEV_TRANSPORT="tailscale-cli",
                        HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                        ACTIVATION_CHECK_RECORD=str(activation_record))
    interactive_refused("dev_tunnel_rejects_relative_state", tunnel_script, b"3\n",
                        b"Choose an absolute private state directory, or unset HERDR_DEV_TAILSCALE_DIR",
                        HERDR_DEV_TAILSCALE_DIR="relative")
    interactive_refused("consent_does_not_create_custom_root", script, b"y\n",
                        b"Create a custom private state directory first",
                        HERDR_DEV_TAILSCALE_DIR=str(base / "missing"))
    checkout_default = root / "relay" / ".dev-tailscale"
    checkout_default_existed = checkout_default.exists()

    def check_checkout_root_existence_unchanged() -> None:
        if checkout_default.exists() != checkout_default_existed:
            raise AssertionError("development preflight changed checkout-local state existence")

    check_checkout_root_existence_unchanged()
    interactive_refused("invalid_cli_does_not_create_default", tunnel_script, b"3\n",
                        b"Not an executable file:",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN=str(base / "missing-cli"))
    interactive_refused("menu_uses_all_safe_defaults", tunnel_script, b"3\n",
                        b"Production and dev-tunnel ports are reserved",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN="",
                        HERDR_DEV_HERDR_BIN="", HERDR_DEV_HERDR_SOCKET="",
                        HERDR_SOCKET_PATH="", HERDR_DEV_TAILSCALE_PORT="8375",
                        HERDR_DEV_TAILSCALE_PLUGIN_PORT="", HERDR_DEV_TAILSCALE_HTTPS_PORT="",
                        PATH=f"{base}:/usr/bin:/bin")
    check_checkout_root_existence_unchanged()

    app_store_bundle = home / "Applications" / "Tailscale.app"
    receipt = app_store_bundle / "Contents" / "_MASReceipt" / "receipt"
    receipt.parent.mkdir(mode=0o700, parents=True)
    receipt.write_bytes(b"synthetic App Store receipt marker")
    app_store_info = app_store_bundle / "Contents" / "Info.plist"
    app_store_info.write_text(
        '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>\n'
        '<key>CFBundleShortVersionString</key>\n<string>1.100.0</string>\n'
        '</dict></plist>\n',
        encoding="utf-8",
    )
    menu_cli_dir = home / "menu-bin"
    menu_cli_dir.mkdir(mode=0o700)
    menu_cli = menu_cli_dir / "tailscale"
    menu_cli.write_text(f"#!/bin/sh\nprintf invoked >> '{sentinel}'\nexit 97\n", encoding="utf-8")
    menu_cli.chmod(0o700)
    menu_uname = menu_cli_dir / "uname"
    menu_uname.write_text(
        "#!/bin/sh\n"
        "case \"$1\" in\n"
        "  -s) printf '%s\\n' \"${HERDR_TEST_UNAME_S:-Darwin}\" ;;\n"
        "  -m) printf '%s\\n' \"${HERDR_TEST_UNAME_M:-arm64}\" ;;\n"
        "  *) exit 2 ;;\n"
        "esac\n",
        encoding="utf-8",
    )
    menu_uname.chmod(0o700)
    menu_plutil = menu_cli_dir / "plutil"
    menu_plutil.write_text(
        "#!/bin/sh\n"
        "test \"$1\" = -extract && test \"$2\" = CFBundleShortVersionString && "
        "test \"$3\" = raw && test \"$4\" = -o && test \"$5\" = - || exit 2\n"
        "sed -n '/<key>CFBundleShortVersionString<\\/key>/{n;s/.*<string>\\([^<]*\\)<\\/string>.*/\\1/p;}' \"$6\"\n",
        encoding="utf-8",
    )
    menu_plutil.chmod(0o700)
    receipt.unlink()
    app_store_settings = {
        "HERDR_DEV_TAILSCALE_CLI_BIN": str(menu_cli),
        "HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(cli_relay),
        "ACTIVATION_CHECK_RECORD": str(activation_record),
        "PATH": f"{menu_cli_dir}:/usr/bin:/bin",
    }
    interactive_refused(
        "app_store_missing_marks_cli_mode_unavailable", tunnel_script, b"2\n",
        b"No App Store Tailscale bundle was detected", **app_store_settings,
    )
    for system, arch, case in (
        ("Linux", "x86_64", "linux_marks_cli_mode_unavailable"),
        ("Darwin", "x86_64", "macos_amd64_marks_cli_mode_unavailable"),
    ):
        interactive_refused(
            case, tunnel_script, b"2\n",
            b"CLI-backed development requires macOS/arm64",
            HERDR_TEST_UNAME_S=system, HERDR_TEST_UNAME_M=arch,
            **app_store_settings,
        )
    receipt.write_bytes(b"synthetic App Store receipt marker")
    activation_before_menu = activation_record.read_bytes() if activation_record.exists() else None
    interactive_refused(
        "app_store_unsupported_version_marks_cli_mode_unavailable", tunnel_script, b"2\n",
        b"Option 2 unavailable: Only the App Store Tailscale 1.102.4 profile is enabled",
        **app_store_settings,
    )
    activation_after_menu = activation_record.read_bytes() if activation_record.exists() else None
    if activation_after_menu != activation_before_menu:
        raise AssertionError("unsupported App Store version selection reached the CLI activation fixture")
    app_store_info.write_text(
        '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict></dict></plist>\n',
        encoding="utf-8",
    )
    interactive_refused(
        "app_store_unreadable_version_marks_cli_mode_unavailable", tunnel_script, b"2\n",
        b"Could not read the App Store bundle version", **app_store_settings,
    )
    app_store_info.write_text(
        '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>\n'
        '<key>CFBundleShortVersionString</key>\n<string>1.102.4</string>\n'
        '</dict></plist>\n',
        encoding="utf-8",
    )
    interactive_refused(
        "app_store_supported_version_still_requires_explicit_opt_in", tunnel_script, b"2\nn\n",
        b"Cancelled; nothing was started.", **app_store_settings,
    )
    interactive_refused(
        "app_store_supported_version_keeps_cli_option_available", tunnel_script, b"4\n",
        b"Choose 1, 2, or 3.", **app_store_settings,
    )
    interactive_refused("app_store_marks_legacy_mode_unavailable", tunnel_script, b"3\n",
                        b"Option 3 unavailable: App Store Tailscale was detected",
                        **app_store_settings)
    if sentinel.exists():
        raise AssertionError("menu selection executed the real-CLI stand-in instead of using filesystem-only detection")
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

    cli_delegated_env = dict(without_consent, HERDR_DEV_TRANSPORT="tailscale-cli",
                             HERDR_DEV_CONFIG_DIR=str(base / "unused-cli-mode"),
                             HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                             ACTIVATION_CHECK_RECORD=str(activation_record))
    cli_delegated = subprocess.run(
        [str(tunnel_script)], env=cli_delegated_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if cli_delegated.returncode == 0 or b"Scripted transport opt-in" not in cli_delegated.stdout + cli_delegated.stderr:
        raise AssertionError("CLI-backed development selection bypassed its explicit transport opt-in")
    if sentinel.exists() or (base / "unused-cli-mode").exists():
        raise AssertionError("CLI-backed refusal touched a Tailscale CLI or development state")
    check_checkout_root_existence_unchanged()
    private_cli_dev = base / "cli-dev-private"
    private_cli_dev.mkdir(mode=0o700)
    cli_dev_gate = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")],
        env=dict(without_consent, HERDR_DEV_TAILSCALE_CLI_ENABLE="1",
                 HERDR_DEV_TAILSCALE_CLI_DIR=str(private_cli_dev),
                 HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                 HERDR_DEV_PHONE_APP_URL="https://app.fixture.invalid",
                 ACTIVATION_CHECK_RECORD=str(activation_record)),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (cli_dev_gate.returncode == 0 or
        b"No unambiguous absolute Tailscale CLI candidate was selected" not in cli_dev_gate.stdout + cli_dev_gate.stderr or
        list(private_cli_dev.iterdir()) or sentinel.exists()):
        raise AssertionError("development qualification skipped profile selection or touched private state")
    print("PASS dev-tailscale-cli isolation: development qualification remains profile-gated and production state untouched")

    # The supplied App Store route is fixed to HTTPS 8443 -> backend 18377,
    # with plugin listener 18378. Port overrides must fail before even the
    # named activation fixture is contacted or private state is changed.
    for variable, value, expected in (
        ("HERDR_DEV_TAILSCALE_CLI_PORT", "18577", b"fixed at 18377"),
        ("HERDR_DEV_TAILSCALE_CLI_PORT", "18377", b"fixed at 18377"),
        ("HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT", "18578", b"fixed at 18378"),
        ("HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT", "18378", b"fixed at 18378"),
        ("HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT", "9443", b"fixed at 8443"),
        ("HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT", "8443", b"fixed at 8443"),
    ):
        port_root = base / ("port-override-" + variable.lower().replace("_", "-") + "-" + value)
        port_root.mkdir(mode=0o700)
        activation_before = activation_record.read_bytes()
        port_env = dict(without_consent, HERDR_DEV_TAILSCALE_CLI_ENABLE="1",
                        HERDR_DEV_TAILSCALE_CLI_DIR=str(port_root),
                        HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                        HERDR_DEV_HERDR_BIN=str(herdr), HERDR_DEV_HERDR_SOCKET=str(base / "herdr.sock"),
                        ACTIVATION_CHECK_RECORD=str(activation_record))
        port_env[variable] = value
        port_result = subprocess.run(
            [str(root / "relay" / "dev-tailscale-cli.sh")],
            env=port_env,
            cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=5, check=False,
        )
        if (port_result.returncode == 0 or expected not in port_result.stdout + port_result.stderr or
            list(port_root.iterdir()) or sentinel.exists() or activation_record.read_bytes() != activation_before):
            raise AssertionError(
                f"development CLI port override {variable}={value} was not rejected before mutation: "
                f"status={port_result.returncode} output={port_result.stdout + port_result.stderr!r}"
            )
    print("PASS dev-tailscale-cli ports: non-profile relay/plugin/HTTPS overrides fail before CLI access or state mutation")

    cli_lifecycle_env = base / "cli-lifecycle.env"
    cli_registration = base / "cli-registration"
    cli_coordination = base / "cli-coordination"
    cli_lifecycle_env.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
        "HERDR_RELAY_INSTANCE_ID=fixture-cli-instance\n"
        "HERDR_RELAY_PORT=18577\n"
        "HERDR_TAILSCALE_CLI_SCOPE=development\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli}\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={cli_registration}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={cli_coordination}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\n",
        encoding="utf-8",
    )
    lifecycle = subprocess.run(
        [str(root / "relay" / "tailscale-cli.sh"), "recover"],
        env=dict(env, HERDR_RELAY_ENV=str(cli_lifecycle_env), HERDR_RELAY_BIN=str(cli_relay),
                 ACTIVATION_CHECK_RECORD=str(activation_record)),
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    if (lifecycle.returncode == 0 or b"production CLI activation remains disabled" not in lifecycle.stdout + lifecycle.stderr or
        cli_registration.exists() or cli_coordination.exists() or sentinel.exists()):
        raise AssertionError("installed CLI lifecycle did not stop at the production activation gate")
    print("PASS CLI lifecycle fixture: installed-service recovery remains production-gated")

    activation_events = activation_record.read_text(encoding="utf-8").splitlines()
    activation_checks = [event for event in activation_events
                         if event.startswith("tailscale-cli activation-check")]
    if (activation_checks.count("tailscale-cli activation-check --scope development") != 3 or
        activation_checks.count("tailscale-cli activation-check") != 1):
        raise AssertionError(
            "CLI-backed development and production activation scopes were not kept distinct: "
            f"activation_checks={activation_checks!r}, all_cli_calls={activation_events!r}"
        )
    print("PASS dev-tailscale preflight: development_scope_is_explicit_and_production_remains_disabled")

    # Development uses the installed service's actual relay.env coordination
    # root, even when its custom state path differs from the checkout defaults.
    coordination_home = base / "coordination-home"
    coordination_home.mkdir(mode=0o700)
    custom_production_env = base / "installed-production.env"
    custom_coordination_root = base / "installed-coordination"
    custom_production_env.write_text(
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={custom_coordination_root}\n",
        encoding="utf-8",
    )
    service_config = coordination_home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
    if os.uname().sysname == "Linux":
        service_config.parent.mkdir(mode=0o700, parents=True)
        service_config.write_text(f"Environment=HERDR_RELAY_ENV={custom_production_env}\n", encoding="utf-8")
    else:
        service_config = coordination_home / "Library" / "LaunchAgents" / "com.herdr-mobile-relay.service.plist"
        service_config.parent.mkdir(mode=0o700, parents=True)
        service_config.write_text(
            '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>'
            '<key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key>'
            f'<string>{custom_production_env}</string></dict></dict></plist>', encoding="utf-8",
        )
    custom_dev_root = base / "custom-cli-dev"
    custom_dev_root.mkdir(mode=0o700)
    custom_dev_state = custom_dev_root / "registration"
    custom_dev_state.mkdir(mode=0o700)
    custom_coordination_root.mkdir(mode=0o700)
    (custom_dev_root / "relay.env").write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_RELAY_INSTANCE_ID=coord-fixture\n"
        "HERDR_RELAY_PORT=18377\nHERDR_RELAY_PLUGIN_PORT=18378\n"
        "HERDR_TAILSCALE_CLI_SCOPE=development\nHERDR_TAILSCALE_CLI_HTTPS_PORT=8443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={custom_dev_state}\n",
        encoding="utf-8",
    )
    custom_marker = custom_dev_root / ".herdr-dev-tailscale-cli"
    custom_marker.write_text(
        "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n"
        f"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT={custom_dev_state}\n"
        f"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT={custom_coordination_root}\n",
        encoding="utf-8",
    )
    custom_marker.chmod(0o600)
    coordination_record = base / "coordination-root-args"
    coordination_relay = base / "coordination-root-relay"
    coordination_relay.write_text(
        "#!/bin/sh\n"
        "case \"$1 $2\" in 'tailscale-cli activation-check') exit 0 ;; esac\n"
        "printf 'args=%s\\ncoordination=%s\\n' \"$*\" \"${HERDR_TAILSCALE_CLI_COORDINATION_ROOT:-}\" > \"$COORDINATION_ARGS\"\n"
        "exit 0\n", encoding="utf-8",
    )
    coordination_relay.chmod(0o700)
    coordination_env = dict(env)
    for name in ("HERDR_TAILSCALE_CLI_COORDINATION_ROOT", "HERDR_PLUGIN_CONFIG_DIR"):
        coordination_env.pop(name, None)
    coordination_env.update({
        "HOME": str(coordination_home), "HERDR_RELAY_ENV": str(custom_dev_root / "relay.env"),
        "HERDR_DEV_TAILSCALE_CLI_ENABLE": "1",
        "HERDR_DEV_TAILSCALE_CLI_DIR": str(custom_dev_root),
        "HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(coordination_relay),
        "COORDINATION_ARGS": str(coordination_record),
    })
    coordination_result = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "status"], env=coordination_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=5, check=False,
    )
    coordination_observed = (
        coordination_record.read_text(encoding="utf-8") if coordination_record.exists() else "<missing>"
    )
    if (coordination_result.returncode != 0 or
        f"coordination={custom_coordination_root}" not in coordination_observed):
        raise AssertionError(
            f"development diverged from the installed service coordination root: "
            f"exit={coordination_result.returncode} observed={coordination_observed!r} "
            f"output={coordination_result.stdout + coordination_result.stderr!r}"
        )
    print("PASS CLI development fixture: coordination lock root matches installed service relay.env")

    # Explicit shell unpublish is exercised only through a fake relay manager.
    # Declining consent must not dispatch route removal; accepting it passes the
    # exact scoped node/port and narrow route-removal consent to the manager.
    unpublish_root = base / "cli-unpublish-consent"
    unpublish_root.mkdir(mode=0o700)
    unpublish_coordination = base / "cli-unpublish-coordination"
    unpublish_coordination.mkdir(mode=0o700)
    unpublish_scripts = unpublish_root / "relay"
    unpublish_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "tailscale-cli.sh"):
        shutil.copy2(root / "relay" / name, unpublish_scripts / name)
    unpublish_env_file = unpublish_root / "relay.env"
    unpublish_env_file.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
        "HERDR_RELAY_INSTANCE_ID=fixture-unpublish-instance\n"
        "HERDR_RELAY_PORT=18577\n"
        "HERDR_TAILSCALE_CLI_SCOPE=development\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli}\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={unpublish_root}/registration\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={unpublish_coordination}\n"
        f"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT={unpublish_root}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\n"
        "HERDR_TAILSCALE_CLI_NODE_ID=node-unpublish-fixture\n",
        encoding="utf-8",
    )
    unpublish_registration = unpublish_root / "registration"
    unpublish_registration.mkdir(mode=0o700)
    unpublish_marker = unpublish_root / ".herdr-dev-tailscale-cli"
    unpublish_marker.write_text(
        "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n"
        f"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT={unpublish_registration}\n"
        f"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT={unpublish_coordination}\n",
        encoding="utf-8",
    )
    unpublish_marker.chmod(0o600)
    unpublish_journal = unpublish_registration / "registration.json"
    unpublish_journal.write_text('{"state":"registered","fixture":true}\n', encoding="utf-8")
    unpublish_journal_before = unpublish_journal.read_bytes()
    unpublish_record = unpublish_root / "manager-events"
    unpublish_relay = unpublish_root / "fake-relay"
    unpublish_relay.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' \"$*\" >> \"$UNPUBLISH_RECORD\"\n"
        "case \"$1 $2\" in 'tailscale-cli activation-check') exit 0 ;; esac\n"
        "case \"$1 $2\" in 'tailscale-cli unpublish') exit 0 ;; esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    unpublish_relay.chmod(0o700)
    unpublish_env = dict(env)
    unpublish_env.update({
        "HOME": str(home), "HERDR_RELAY_ENV": str(unpublish_env_file),
        "HERDR_RELAY_BIN": str(unpublish_relay), "UNPUBLISH_RECORD": str(unpublish_record),
    })
    for name in ("CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG", "GH_TOKEN"):
        unpublish_env.pop(name, None)

    def run_unpublish_consent(answer: bytes) -> tuple[int, bytes]:
        master, slave = pty.openpty()
        try:
            process = subprocess.Popen(
                [str(unpublish_scripts / "tailscale-cli.sh"), "unpublish"],
                env=unpublish_env, cwd=root, stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            os.close(slave)
            os.write(master, answer)
            stdout, stderr = process.communicate(timeout=10)
            if sentinel.exists() or unpublish_journal.read_bytes() != unpublish_journal_before:
                raise AssertionError("unpublish consent fixture contacted the CLI or changed its registration journal")
            return process.returncode, stdout + stderr
        finally:
            os.close(master)

    declined_status, declined_output = run_unpublish_consent(b"n\n")
    if declined_status == 0 or b"Cancelled; registration and route were left unchanged." not in declined_output:
        raise AssertionError(f"declined CLI unpublish was not cancelled: {declined_output!r}")
    if unpublish_record.read_text(encoding="utf-8").splitlines() != ["tailscale-cli activation-check"]:
        raise AssertionError("declining CLI unpublish dispatched the route-removal manager")
    accepted_status, accepted_output = run_unpublish_consent(b"y\n")
    accepted_events = unpublish_record.read_text(encoding="utf-8").splitlines()
    if (accepted_status != 0 or not accepted_events[-1].startswith(
            "tailscale-cli unpublish --development-root " + str(unpublish_root) +
            " --binary " + str(cli) + " --state-root ") or
        "--scope development --installation-id fixture-unpublish-instance" not in accepted_events[-1] or
        "--https-port 9443 --backend-port 18577 --accepted --node-id node-unpublish-fixture" not in accepted_events[-1] or
        "--accept-route-removal --accept-check-to-write-race --accept-no-remote-drain" not in accepted_events[-1] or
        "--accept-persistent-route" in accepted_events[-1] or sentinel.exists()):
        raise AssertionError(f"accepted CLI unpublish did not pass exact narrow consent: {accepted_output!r} {accepted_events!r}")
    print("PASS CLI shell unpublish fixture: decline is non-mutating; acceptance authorizes only the journaled exact route")

    # Installed-service CLI startup remains explicitly disabled even if a
    # synthetic relay reports activation-check success.
    cli_service_home = base / "cli-service-home"
    cli_service_home.mkdir(mode=0o700)
    cli_service_env = base / "cli-service.env"
    cli_service_record = base / "cli-service-record"
    cli_service_relay = base / "fake-cli-service-relay"
    cli_service_scripts = base / "cli-service-relay"
    cli_service_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "herdr-mobile-relay-service.sh", "tailscale-cli-service.sh"):
        shutil.copy2(root / "relay" / name, cli_service_scripts / name)
    cli_service_env.write_text(
        "HERDR_RELAY_TRANSPORT='tailscale-cli'\nHERDR_RELAY_PORT='18377'\n"
        "HERDR_RELAY_INSTANCE_ID='service-fixture-instance'\n"
        f"HERDR_TAILSCALE_CLI_BIN='{cli}'\nHERDR_TAILSCALE_CLI_SCOPE='development'\n",
        encoding="utf-8",
    )
    cli_service_relay.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' \"$*\" >> \"$HERDR_CLI_SERVICE_RECORD\"\n"
        "case \"$*\" in 'tailscale-cli activation-check') exit 0 ;; esac\n"
        "echo unexpected service relay command >&2; exit 97\n",
        encoding="utf-8",
    )
    cli_service_relay.chmod(0o700)
    service_env = dict(env)
    service_env.update({
        "HOME": str(cli_service_home), "HERDR_RELAY_ENV": str(cli_service_env),
        "HERDR_RELAY_BIN": str(cli_service_relay),
        "HERDR_CLI_SERVICE_RECORD": str(cli_service_record),
    })
    for entrypoint in ("herdr-mobile-relay-service.sh", "tailscale-cli-service.sh"):
        result = subprocess.run(
            [str(cli_service_scripts / entrypoint)], env=service_env, cwd=root,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
        )
        if (result.returncode != 0 or
            b"Installed-service CLI startup is disabled" not in result.stderr):
            raise AssertionError(f"{entrypoint} did not remain disabled: {result.stdout + result.stderr!r}")
    service_events = cli_service_record.read_text(encoding="utf-8").splitlines()
    if service_events != ["tailscale-cli activation-check", "tailscale-cli activation-check"] or sentinel.exists():
        raise AssertionError(f"disabled installed-service entrypoint contacted CLI or started relay: {service_events!r}")
    print("PASS CLI service fixture: direct and generic installed-service entrypoints stop before CLI execution and relay startup")

    # A named user-service fixture admits only a fake exact-route verifier and
    # fake systemctl/curl. It never reaches the host service manager, Tailscale,
    # Cloudflare, or a listening relay.
    cli_install_root = base / "cli install & 'quoted' fixture"
    cli_install_home = cli_install_root / "home"
    cli_install_home.mkdir(mode=0o700, parents=True)
    cli_install_root.chmod(0o700)
    cli_install_scripts = cli_install_root / "relay"
    cli_install_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "install-systemd-user-service.sh", "herdr-mobile-relay-service.sh",
                 "tailscale-cli-service.sh", "service.sh"):
        shutil.copy2(root / "relay" / name, cli_install_scripts / name)
    cli_install_bin = cli_install_root / "fake-bin"
    cli_install_bin.mkdir(mode=0o700)
    manager_record = cli_install_root / "manager-record"
    systemctl_record = cli_install_root / "systemctl-record"
    (cli_install_bin / "systemctl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYSTEMCTL_RECORD\"\nexit 0\n",
        encoding="utf-8",
    )
    (cli_install_bin / "systemctl").chmod(0o700)
    (cli_install_bin / "uname").write_text(
        """#!/bin/sh
printf 'Linux'
""",
        encoding="utf-8",
    )
    (cli_install_bin / "uname").chmod(0o700)
    (cli_install_bin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' '{\"status\":\"ok\",\"instance\":\"fixture-installation\",\"version\":\"0.9.0\",\"protocol\":1}'\n",
        encoding="utf-8",
    )
    (cli_install_bin / "curl").chmod(0o700)
    cli_install_user_bin = cli_install_home / ".local" / "bin"
    cli_install_user_bin.mkdir(mode=0o700, parents=True)
    for name in ("systemctl", "curl", "uname"):
        shutil.copy2(cli_install_bin / name, cli_install_user_bin / name)
    manager_relay = cli_install_root / "fake-manager-relay"
    manager_relay.write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MANAGER_RECORD\"\n"
        "case \"$*\" in 'tailscale-cli activation-check') exit 0 ;; esac\n"
        "case \"$*\" in 'tailscale-cli assert-ready'*) printf '%s\\n' '{\"route\":{\"journal_state\":\"registered\",\"readiness\":\"ready\",\"runtime_qualified\":true}}'; exit 0 ;; esac\n"
        "echo unexpected relay command >&2; exit 97\n",
        encoding="utf-8",
    )
    manager_relay.chmod(0o700)
    cli_install_coordination = base / "cli-install-coordination"
    cli_install_coordination.mkdir(mode=0o700)
    cli_install_state = cli_install_root / "registration"
    cli_install_state.mkdir(mode=0o700)
    cli_install_marker = cli_install_root / ".herdr-dev-tailscale-cli"
    cli_install_marker.write_text(
        "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n"
        f"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT={cli_install_state}\n"
        f"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT={cli_install_coordination}\n",
        encoding="utf-8",
    )
    cli_install_marker.chmod(0o600)
    cli_install_env = cli_install_root / "relay.env"
    cli_install_env.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
        "HERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef\n"
        "HERDR_RELAY_INSTANCE_ID=fixture-installation\n"
        "HERDR_RELAY_CONTROL_RUN_ID=fixture-control\n"
        f"HERDR_RELAY_PAIRING_SOCKET={shlex.quote(str(cli_install_root / 'control.sock'))}\n"
        "HERDR_RELAY_HOST=127.0.0.1\nHERDR_RELAY_PORT=18577\nHERDR_RELAY_PLUGIN_PORT=18578\n"
        f"HERDR_TAILSCALE_CLI_BIN={shlex.quote(str(cli))}\nHERDR_TAILSCALE_CLI_SCOPE=development\n"
        f"HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:9443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={shlex.quote(str(cli_install_state))}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={shlex.quote(str(cli_install_coordination))}\n"
        f"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT={shlex.quote(str(cli_install_root))}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\nHERDR_REACHABILITY_PORT_MAPPING=0\n",
        encoding="utf-8",
    )
    install_env = dict(env)
    install_env.update({
        "HOME": str(cli_install_home), "HERDR_RELAY_ENV": str(cli_install_env),
        "HERDR_RELAY_BIN": str(manager_relay),
        "HERDR_RELEASE_ROOT": str(cli_install_home / ".local" / "share" / "herdr-mobile-relay"),
        "HERDR_TAILSCALE_CLI_BIN": str(cli), "HERDR_TAILSCALE_CLI_SCOPE": "development",
        "HERDR_TAILSCALE_CLI_STATE_ROOT": str(cli_install_state),
        "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": str(cli_install_coordination),
        "HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT": str(cli_install_root),
        "HERDR_TAILSCALE_CLI_HTTPS_PORT": "9443", "HERDR_RELAY_INSTANCE_ID": "fixture-installation",
        "HERDR_RELAY_PORT": "18577", "SYSTEMCTL_RECORD": str(systemctl_record),
        "MANAGER_RECORD": str(manager_record),
        "PATH": f"{cli_install_bin}:{os.environ.get('PATH', '/usr/bin:/bin')}",
    })
    for name in ("CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG", "GH_TOKEN", "SUDO_USER"):
        install_env.pop(name, None)
    installed = subprocess.run(
        [str(cli_install_scripts / "install-systemd-user-service.sh")],
        env=install_env, cwd=root, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False,
    )
    setup_installed = subprocess.run(
        [str(cli_install_scripts / "install-systemd-user-service.sh")],
        env={**install_env, "HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START": "1"}, cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False,
    )
    unit = cli_install_home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
    if installed.returncode != 0 or setup_installed.returncode != 0 or not unit.is_file():
        raise AssertionError(
            f"CLI service installer failed with only named fixtures: "
            f"{installed.stdout + installed.stderr + setup_installed.stdout + setup_installed.stderr!r}"
        )
    unit_text = unit.read_text(encoding="utf-8")
    manager_events = manager_record.read_text(encoding="utf-8").splitlines()
    system_events = systemctl_record.read_text(encoding="utf-8").splitlines()
    systemd_quote = lambda value: '"' + value.replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%") + '"'
    expected_unit_lines = {
        "ExecStart=" + systemd_quote(str(cli_install_scripts / "herdr-mobile-relay-service.sh")),
        "WorkingDirectory=" + systemd_quote(os.path.realpath(cli_install_root)),
        "Environment=HERDR_RELAY_ENV=" + systemd_quote(str(cli_install_env)),
        "Restart=on-failure",
        "RestartSec=10",
    }
    if (not expected_unit_lines.issubset(set(unit_text.splitlines())) or
        "Description=Herdr Mobile Relay tailscale-cli" not in unit_text or
        "CLOUDFLARED_CONFIG" in unit_text or not manager_events or
        not manager_events[0].startswith("tailscale-cli assert-ready ") or
        manager_events.count("tailscale-cli activation-check") != 1 or
        any("serve" in event or "publish" in event or "unpublish" in event for event in manager_events) or
        not system_events or not cli_service_record.is_file() or sentinel.exists()):
        raise AssertionError(f"CLI installer touched an unexpected boundary: manager={manager_events!r}, system={system_events!r}, expected_unit={expected_unit_lines!r}, unit={unit_text!r}")
    stopped = subprocess.run(
        [str(cli_install_scripts / "service.sh"), "stop"],
        env=install_env, cwd=root, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
    )
    if (stopped.returncode != 0 or
        systemctl_record.read_text(encoding="utf-8").splitlines()[-1] != "--user stop herdr-mobile-relay.service" or
        manager_record.read_text(encoding="utf-8").splitlines() != manager_events or sentinel.exists()):
        raise AssertionError("CLI service stop mutated registration or escaped the fake service manager")
    print("PASS CLI user-service lifecycle fixture: exact-route gate, Cloudflare-free install, route-preserving stop")

    # macOS launchd acceptance also uses only named fixtures; launchctl/curl are
    # shadowed and the CLI route verifier is read-only and synthetic.
    mac_fixture = base / "mac service & 'quoted' fixture"
    mac_home = mac_fixture / "home"
    mac_home.mkdir(mode=0o700, parents=True)
    mac_fixture.chmod(0o700)
    mac_scripts = mac_fixture / "relay"
    mac_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "install-service.sh", "herdr-mobile-relay-service.sh",
                 "tailscale-cli-service.sh", "service.sh"):
        shutil.copy2(root / "relay" / name, mac_scripts / name)
    mac_bin = mac_fixture / "bin"
    mac_bin.mkdir(mode=0o700)
    launchctl_record = mac_fixture / "launchctl-record"
    mac_manager_record = mac_fixture / "manager-record"
    mac_systemctl_record = mac_fixture / "systemctl-record"
    (mac_bin / "uname").write_text("#!/bin/sh\nprintf 'Darwin\\n'\n", encoding="utf-8")
    (mac_bin / "uname").chmod(0o700)
    (mac_bin / "systemctl").write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' \"$*\" >> \"$MAC_SYSTEMCTL_RECORD\"\n"
        "exit 97\n",
        encoding="utf-8",
    )
    (mac_bin / "systemctl").chmod(0o700)
    (mac_bin / "launchctl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LAUNCHCTL_RECORD\"\n"
        "case \"$1\" in print) exit 1 ;; bootout|bootstrap|enable|kickstart) exit 0 ;; esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    (mac_bin / "launchctl").chmod(0o700)
    (mac_bin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' '{\"status\":\"ok\",\"instance\":\"mac-fixture-instance\",\"version\":\"0.9.0\",\"protocol\":1}'\n",
        encoding="utf-8",
    )
    (mac_bin / "curl").chmod(0o700)
    mac_manager = mac_fixture / "fake-manager-relay"
    mac_manager.write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MAC_MANAGER_RECORD\"\n"
        "case \"$*\" in 'tailscale-cli assert-ready'*) printf '%s\\n' '{\"route\":{\"journal_state\":\"registered\",\"readiness\":\"ready\",\"runtime_qualified\":true}}'; exit 0 ;; esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    mac_manager.chmod(0o700)
    mac_coordination = base / "mac-service-coordination"
    mac_coordination.mkdir(mode=0o700)
    mac_state = mac_fixture / "registration"
    mac_state.mkdir(mode=0o700)
    mac_marker = mac_fixture / ".herdr-dev-tailscale-cli"
    mac_marker.write_text(
        "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n"
        f"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT={mac_state}\n"
        f"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT={mac_coordination}\n",
        encoding="utf-8",
    )
    mac_marker.chmod(0o600)
    mac_env = mac_fixture / "relay.env"
    mac_env.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef\n"
        "HERDR_RELAY_INSTANCE_ID=mac-fixture-instance\nHERDR_RELAY_CONTROL_RUN_ID=mac-fixture-control\n"
        f"HERDR_RELAY_PAIRING_SOCKET={shlex.quote(str(mac_fixture / 'control.sock'))}\n"
        "HERDR_RELAY_HOST=127.0.0.1\nHERDR_RELAY_PORT=18577\nHERDR_RELAY_PLUGIN_PORT=18578\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli}\nHERDR_TAILSCALE_CLI_SCOPE=development\n"
        "HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:9443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={shlex.quote(str(mac_state))}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={shlex.quote(str(mac_coordination))}\n"
        f"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT={shlex.quote(str(mac_fixture))}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\nHERDR_REACHABILITY_PORT_MAPPING=0\n",
        encoding="utf-8",
    )
    mac_env_settings = dict(env)
    mac_env_settings.update({
        "HOME": str(mac_home), "HERDR_RELAY_ENV": str(mac_env),
        "HERDR_RELAY_BIN": str(mac_manager), "HERDR_RELAY_PORT": "18577",
        "HERDR_RELEASE_ROOT": str(mac_home / ".local" / "share" / "herdr-mobile-relay"),
        "HERDR_TAILSCALE_CLI_BIN": str(cli), "HERDR_TAILSCALE_CLI_SCOPE": "development",
        "HERDR_TAILSCALE_CLI_STATE_ROOT": str(mac_state),
        "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": str(mac_coordination),
        "HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT": str(mac_fixture),
        "HERDR_TAILSCALE_CLI_HTTPS_PORT": "9443", "MAC_MANAGER_RECORD": str(mac_manager_record),
        "LAUNCHCTL_RECORD": str(launchctl_record), "MAC_SYSTEMCTL_RECORD": str(mac_systemctl_record),
        "PATH": f"{mac_bin}:{os.environ.get('PATH', '/usr/bin:/bin')}",
    })
    for name in ("CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG", "GH_TOKEN", "SUDO_USER"):
        mac_env_settings.pop(name, None)
    mac_installed = subprocess.run(
        [str(mac_scripts / "install-service.sh")], env=mac_env_settings, cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15, check=False,
    )
    plist = mac_home / "Library" / "LaunchAgents" / "com.herdr-mobile-relay.service.plist"
    if mac_installed.returncode != 0 or not plist.is_file():
        raise AssertionError(f"macOS CLI service fixture failed: {mac_installed.stdout + mac_installed.stderr!r}")
    plist_text = plist.read_text(encoding="utf-8")
    mac_manager_events = mac_manager_record.read_text(encoding="utf-8").splitlines()
    launchctl_events = launchctl_record.read_text(encoding="utf-8").splitlines()
    if ("com.herdr-mobile-relay.service" not in plist_text or "&amp;" not in plist_text or "CLOUDFLARED_CONFIG" in plist_text or
        "<key>KeepAlive</key>" not in plist_text or "<key>SuccessfulExit</key>\n        <false/>" not in plist_text or
        "<key>ThrottleInterval</key>\n    <integer>10</integer>" not in plist_text or
        not mac_manager_events or not mac_manager_events[0].startswith("tailscale-cli assert-ready ") or
        any("publish" in event or "unpublish" in event for event in mac_manager_events) or
        not any(event.startswith("bootstrap ") for event in launchctl_events) or
        not any(event.startswith("kickstart ") for event in launchctl_events) or sentinel.exists() or
        mac_systemctl_record.exists()):
        raise AssertionError(f"macOS service fixture escaped its fake boundaries: manager={mac_manager_events!r}, launchctl={launchctl_events!r}")
    mac_stopped = subprocess.run(
        [str(mac_scripts / "service.sh"), "stop"], env=mac_env_settings, cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
    )
    if (mac_stopped.returncode != 0 or
        launchctl_record.read_text(encoding="utf-8").splitlines()[-1].startswith("bootout gui/") is False or
        mac_manager_record.read_text(encoding="utf-8").splitlines() != mac_manager_events or sentinel.exists() or
        mac_systemctl_record.exists()):
        raise AssertionError("macOS CLI service stop mutated registration or escaped fake launchd")
    print("PASS macOS CLI user-service fixture: launchd install/health/stop, exact-route gate, no route mutation")

    # Uninstall explicitly preserves a route only after a second affirmative
    # prompt and retains the private registration journal outside removed roots.
    cli_uninstall_root = base / "cli-uninstall-fixture"
    cli_uninstall_home = cli_uninstall_root / "home"
    cli_uninstall_home.mkdir(mode=0o700, parents=True)
    cli_uninstall_scripts = cli_uninstall_root / "relay"
    cli_uninstall_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "uninstall.sh", "uninstall-systemd-user-service.sh"):
        shutil.copy2(root / "relay" / name, cli_uninstall_scripts / name)
    cli_uninstall_bin = cli_uninstall_root / "bin"
    cli_uninstall_bin.mkdir(mode=0o700)
    uninstall_systemctl_record = cli_uninstall_root / "systemctl-record"
    (cli_uninstall_bin / "systemctl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$UNINSTALL_SYSTEMCTL_RECORD\"\n"
        "case \"$*\" in *is-active*) exit 1 ;; esac\nexit 0\n",
        encoding="utf-8",
    )
    (cli_uninstall_bin / "systemctl").chmod(0o700)
    (cli_uninstall_bin / "uname").write_text("#!/bin/sh\nprintf 'Linux'\n", encoding="utf-8")
    (cli_uninstall_bin / "uname").chmod(0o700)
    (cli_uninstall_bin / "herdr").write_text(
        "#!/bin/sh\ntest \"$*\" = 'plugin uninstall herdr-mobile-relay.events'\n",
        encoding="utf-8",
    )
    (cli_uninstall_bin / "herdr").chmod(0o700)
    uninstall_release = cli_uninstall_home / ".local" / "share" / "herdr-mobile-relay"
    uninstall_config = cli_uninstall_home / ".config" / "herdr-mobile-relay"
    uninstall_cache = cli_uninstall_home / ".cache" / "herdr-mobile-relay"
    uninstall_state = cli_uninstall_home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-registration"
    uninstall_coordination = cli_uninstall_home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-coordination"
    for path in (uninstall_release, uninstall_config, uninstall_cache, uninstall_state, uninstall_coordination):
        path.mkdir(mode=0o700, parents=True)
    for path in (uninstall_release, uninstall_config, uninstall_cache):
        canonical = path.resolve()
        (path / ".herdr-mobile-relay-installation").write_text(
            f"product=herdr-mobile-relay\nroot={canonical}\n", encoding="utf-8",
        )
    uninstall_env = uninstall_config / "relay.env"
    uninstall_env.write_text(
        "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli}\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={uninstall_state}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={uninstall_coordination}\n",
        encoding="utf-8",
    )
    registration_fixture = uninstall_state / "registration.json"
    registration_fixture.write_text('{"state":"registered","fixture":true}\n', encoding="utf-8")
    registration_snapshot = registration_fixture.read_bytes()
    uninstall_unit_dir = cli_uninstall_home / ".config" / "systemd" / "user"
    uninstall_unit_dir.mkdir(mode=0o700, parents=True)
    (uninstall_unit_dir / "herdr-mobile-relay.service").write_text(
        f'[Service]\nEnvironment="HERDR_RELAY_ENV={uninstall_env}"\n', encoding="utf-8",
    )
    uninstall_env_settings = dict(env)
    uninstall_env_settings.update({
        "HOME": str(cli_uninstall_home), "HERDR_RELAY_ENV": str(uninstall_env),
        "HERDR_RELEASE_ROOT": str(uninstall_release), "HERDR_PLUGIN_CONFIG_DIR": str(uninstall_config),
        "XDG_CACHE_HOME": str(cli_uninstall_home / ".cache"),
        "XDG_STATE_HOME": str(cli_uninstall_home / ".local" / "state"),
        "HERDR_RELAY_BIN_DIR": str(cli_uninstall_home / ".local" / "bin"),
        "UNINSTALL_SYSTEMCTL_RECORD": str(uninstall_systemctl_record),
        "PATH": f"{cli_uninstall_bin}:{os.environ.get('PATH', '/usr/bin:/bin')}",
    })
    for name in ("CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG", "GH_TOKEN", "SUDO_USER"):
        uninstall_env_settings.pop(name, None)
    master, slave = pty.openpty()
    try:
        uninstall_process = subprocess.Popen(
            [str(cli_uninstall_scripts / "uninstall.sh")], env=uninstall_env_settings, cwd=root,
            stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        os.close(slave)
        os.write(master, b"n\ny\ny\n")
        uninstall_stdout, uninstall_stderr = uninstall_process.communicate(timeout=15)
    finally:
        os.close(master)
    if (uninstall_process.returncode != 0 or b"Leaving persistent route" not in uninstall_stdout or
        registration_fixture.read_bytes() != registration_snapshot or not uninstall_state.is_dir() or
        not uninstall_coordination.is_dir() or uninstall_release.exists() or uninstall_config.exists() or
        uninstall_cache.exists() or sentinel.exists()):
        raise AssertionError(f"CLI uninstall did not retain only explicit route state: {uninstall_stdout + uninstall_stderr!r}")
    print("PASS CLI uninstall fixture: service removed, exact route explicitly retained, registration journal preserved")

    # Positive development setup/update uses isolated tool and relay fixtures.
    # The selected Tailscale executable and Herdr executable are sentinels that
    # fail if invoked; the fake Go output records manager argv and runtime env.
    cli_dev_root = base / "positive-cli-dev"
    cli_dev_root.mkdir(mode=0o700)
    production_env = production / "positive-fixture-baseline"
    production_env.write_text("production-state=preserved\n", encoding="utf-8")
    production_snapshot = production_env.read_bytes()
    cli_dev_fixture = base / "positive-cli-tools"
    cli_dev_fixture.mkdir(mode=0o700)
    fixture_bin = cli_dev_fixture / "bin"
    fixture_bin.mkdir(mode=0o700)
    fixture_log = cli_dev_fixture / "events"
    go_fixture = fixture_bin / "go"
    go_fixture.write_text(
        '''#!/bin/sh
out=
while [ $# -gt 0 ]; do
    if [ "$1" = -o ]; then out=$2; shift 2; else shift; fi
done
[ -n "$out" ] || exit 97
cat > "$out" <<'APP'
#!/bin/sh
if [ "$1" = json-field ]; then
    case "$3" in
        status) printf 'ok\\n' ;;
        readiness) printf 'ready\\n' ;;
        transport) printf 'tailscale-cli\\n' ;;
        instance) printf '%s\\n' "$HERDR_RELAY_INSTANCE_ID" ;;
        tailscale_cli_origin) printf '%s\\n' "$HERDR_TAILSCALE_CLI_ORIGIN" ;;
        ready|persistent_route_ready|invitation_armed) printf 'true\\n' ;;
        *) exit 1 ;;
    esac
    exit 0
fi
if [ "$1" = normalize-external-origin ]; then
    printf '%s\\n' "$2"
    exit 0
fi
if [ "$1" = dev-tailscale-cli ] && [ "$2" = preflight ]; then
    printf 'manager|workflow preflight --development-root %s\\n' "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" >> "$DEV_FIXTURE_LOG"
    printf '%s\\n' '{"node_id":"dev-node-fixture","dns_name":"relay.fixture.invalid","origin":"https://relay.fixture.invalid:8443","profile":"fixture"}'
    exit 0
fi
if [ "$1" = tailscale-cli ] && [ "$2" = activation-check ]; then
    printf 'manager|tailscale-cli activation-check\\n' >> "$DEV_FIXTURE_LOG"
    exit 0
fi
if [ "$1" = dev-tailscale-cli ]; then
    case "$2" in
        assert-ready)
            printf 'manager|workflow assert-ready --development-root %s\\n' "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" >> "$DEV_FIXTURE_LOG"
            printf '%s\\n' '{"journal_state":"registered","readiness":"ready","development_qualification_enabled":true,"runtime_qualified":false}'
            exit 0 ;;
        foreground)
            action=setup
            shift 2
            while [ $# -gt 0 ]; do
                if [ "$1" = --action ]; then action=$2; shift 2; else shift; fi
            done
            if [ "$action" = setup ]; then
                IFS= read -r answer || exit 2
                expected="PUBLISH DEVELOPMENT ROUTE node=$HERDR_TAILSCALE_CLI_NODE_ID origin=$HERDR_TAILSCALE_CLI_ORIGIN https-port=8443 backend=127.0.0.1:18377"
                [ "$answer" = "$expected" ] || exit 2
                printf 'manager|workflow reserve --development-root %s --scope development\\n' "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" >> "$DEV_FIXTURE_LOG"
                printf 'runtime|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\\n' \\
                    "$HERDR_RELAY_TRANSPORT" "$HERDR_RELAY_HOST" "$HERDR_RELAY_PORT" \\
                    "$HERDR_TAILSCALE_CLI_SCOPE" "$HERDR_TAILSCALE_CLI_STATE_ROOT" \\
                    "$HERDR_TAILSCALE_CLI_COORDINATION_ROOT" "$XDG_CONFIG_HOME" \\
                    "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "${GH_TOKEN:-}" >> "$DEV_FIXTURE_LOG"
                printf 'manager|workflow publish --development-root %s --scope development --node-id %s --origin %s\\n' \\
                    "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" "$HERDR_TAILSCALE_CLI_NODE_ID" "$HERDR_TAILSCALE_CLI_ORIGIN" >> "$DEV_FIXTURE_LOG"
                printf 'control|localcontrol admit\\n' >> "$DEV_FIXTURE_LOG"
                printf 'control|localcontrol arm_bootstrap\\n' >> "$DEV_FIXTURE_LOG"
            else
                printf 'manager|workflow assert-ready --development-root %s\\n' "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" >> "$DEV_FIXTURE_LOG"
                printf 'runtime|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\\n' \\
                    "$HERDR_RELAY_TRANSPORT" "$HERDR_RELAY_HOST" "$HERDR_RELAY_PORT" \\
                    "$HERDR_TAILSCALE_CLI_SCOPE" "$HERDR_TAILSCALE_CLI_STATE_ROOT" \\
                    "$HERDR_TAILSCALE_CLI_COORDINATION_ROOT" "$XDG_CONFIG_HOME" \\
                    "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "${GH_TOKEN:-}" >> "$DEV_FIXTURE_LOG"
                printf 'control|localcontrol admit\\n' >> "$DEV_FIXTURE_LOG"
            fi
            exit 0 ;;
    esac
fi
exit 97
APP
chmod 700 "$out"
''',
        encoding="utf-8",
    )
    go_fixture.chmod(0o700)
    positive_gate = cli_dev_fixture / "positive-gate-relay"
    positive_gate.write_text(
        "#!/bin/sh\n"
        "if [ \"$1\" = json-field ]; then\n"
        "  case \"$3\" in\n"
        "    node_id) printf '%s\\n' dev-node-fixture ;;\n"
        "    dns_name) printf '%s\\n' relay.fixture.invalid ;;\n"
        "    origin) printf '%s\\n' https://relay.fixture.invalid:8443 ;;\n"
        "    *) exit 1 ;;\n"
        "  esac\n"
        "  exit 0\n"
        "fi\n"
        "case \"$1\" in\n"
        "  tailscale-cli)\n"
        "    case \"$2\" in\n"
        "      activation-check) exit 0 ;;\n"
        "      resolve-binary) printf '%s\\n' \"$HERDR_DEV_TAILSCALE_CLI_BIN\"; exit 0 ;;\n"
        "      preflight) printf 'standalone preflight called\\n' >> \"$DEV_FIXTURE_LOG\"; exit 97 ;;\n"
        "      reserve-backend-port) printf 'manager|%s\\n' \"$*\" >> \"$DEV_FIXTURE_LOG\"; exit 0 ;;\n"
        "      release-backend-port) exit 0 ;;\n"
        "    esac ;;\n"
        "  normalize-external-origin) printf '%s\\n' \"$2\"; exit 0 ;;\n"
        "esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    positive_gate.chmod(0o700)
    bun_fixture = fixture_bin / "bun"
    bun_fixture.write_text(
        '''#!/bin/sh
if [ "$1" = run ]; then
    [ "$2" = --cwd ] || exit 97
    frontend=$3
    [ "$4" = build ] || exit 97
    # Deliberately ignore extra build arguments, as bun forwards them through
    # the frontend's && script chain rather than to Vite.
    if [ "${DEV_FIXTURE_BUN_MODE:-}" = missing-version ]; then
        rm -rf "$frontend/dist"
        mkdir -p "$frontend/dist"
        printf 'fixture asset\\n' > "$frontend/dist/app.js"
        exit 0
    fi
    mkdir -p "$frontend/dist"
    printf '{}\\n' > "$frontend/dist/version.json"
    exit 0
fi
case "$1" in
    */stamp-web-version.mjs) [ -f "$2" ] || exit 97 ;;
    */validate-build.mjs) [ -f "$2/version.json" ] || exit 97 ;;
    *) exit 97 ;;
esac
exit 0
''',
        encoding="utf-8",
    )
    bun_fixture.chmod(0o700)
    positive_curl = fixture_bin / "curl"
    positive_curl.write_text(
        "#!/bin/sh\nprintf '%s\\n' '{\"status\":\"ok\",\"readiness\":\"ready\",\"transport\":\"tailscale-cli\",\"instance\":\"'\"$HERDR_RELAY_INSTANCE_ID\"'\",\"tailscale_cli_origin\":\"'\"$HERDR_TAILSCALE_CLI_ORIGIN\"'\"}'\n",
        encoding="utf-8",
    )
    positive_curl.chmod(0o700)
    tool_sentinel = base / "positive-tool-invocation"
    for name in ("systemctl", "launchctl", "cloudflared"):
        wrapper = fixture_bin / name
        wrapper.symlink_to(cli)
        wrapper.chmod(0o700)
    fake_cli = cli
    fake_herdr = herdr
    positive_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    positive_socket.bind(str(cli_dev_fixture / "herdr.sock"))
    positive_env = dict(env)
    positive_env.update({
        "HOME": str(home), "HERDR_DEV_TAILSCALE_CLI_ENABLE": "1",
        "HERDR_DEV_TAILSCALE_CLI_DIR": str(cli_dev_root),
        "HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(positive_gate),
        "HERDR_DEV_TAILSCALE_CLI_BIN": str(fake_cli),
        "HERDR_DEV_TAILSCALE_CLI_ORIGIN": "https://relay.fixture.invalid:8443",
        "HERDR_DEV_TAILSCALE_CLI_NODE_ID": "dev-node-fixture",
        "HERDR_DEV_PHONE_APP_URL": "https://app.fixture.invalid",
        "HERDR_DEV_HERDR_BIN": str(fake_herdr),
        "HERDR_DEV_HERDR_SOCKET": str(cli_dev_fixture / "herdr.sock"),
        "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": str(home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-coordination"),
        "HERDR_DEV_TAILSCALE_CLI_FIXTURE_LOG": str(fixture_log),
        "DEV_FIXTURE_LOG": str(fixture_log), "GH_TOKEN": "fixture-only-secret",
        "PATH": f"{fixture_bin}:/usr/bin:/bin",
    })
    for name in ("HERDR_RELAY_ENV", "HERDR_PLUGIN_CONFIG_DIR", "CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG"):
        positive_env.pop(name, None)
    bypass_env = dict(positive_env, HERDR_DEV_TAILSCALE_CLI_PUBLISH="PUBLISH")
    bypass_cli = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")], env=bypass_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=10, check=False,
    )
    bypass_events = fixture_log.read_text(encoding="utf-8").splitlines() if fixture_log.exists() else []
    if (bypass_cli.returncode == 0 or
        any(event.startswith("manager|workflow publish") for event in bypass_events)):
        raise AssertionError("blanket environment-variable consent bypassed exact route confirmation")
    phone_link = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "setup-link"], env=positive_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False,
    )
    if phone_link.returncode == 0 or b"Lifecycle:" not in phone_link.stderr:
        raise AssertionError("development phone setup-link action was not refused")
    setup_cli = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")], env=positive_env,
        cwd=root, input=(b"PUBLISH DEVELOPMENT ROUTE node=dev-node-fixture "
                        b"origin=https://relay.fixture.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n"),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=False,
    )
    if setup_cli.returncode != 0:
        raise AssertionError(f"positive CLI development setup failed: {setup_cli.stdout + setup_cli.stderr!r}")
    all_events = fixture_log.read_text(encoding="utf-8").splitlines()
    manager_events = [event for event in all_events if event.startswith("manager|")]
    runtime_events = [event for event in all_events if event.startswith("runtime|")]
    arm_event = next((event for event in all_events if event.startswith("control|localcontrol admit")), "")
    bootstrap_event = next((event for event in all_events if event.startswith("control|localcontrol arm_bootstrap")), "")
    if (len(manager_events) != 3 or not manager_events[0].startswith("manager|workflow preflight ") or
        not manager_events[1].startswith("manager|workflow reserve ") or
        not manager_events[2].startswith("manager|workflow publish ") or
        any("--development-root " + str(cli_dev_root) not in event for event in manager_events) or
        "--scope development" not in manager_events[2] or "--node-id dev-node-fixture" not in manager_events[2] or
        "--origin https://relay.fixture.invalid:8443" not in manager_events[2] or len(runtime_events) != 1 or
        not runtime_events[0].startswith("runtime|tailscale-cli|127.0.0.1|18377|development|") or
        str(cli_dev_root / "registration") not in runtime_events[0] or
        str(home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-coordination") not in runtime_events[0] or
        str(cli_dev_root / "config") not in runtime_events[0] or runtime_events[0].endswith("|fixture-only-secret") or
        not arm_event or not bootstrap_event or not (all_events.index(manager_events[0]) <
                              all_events.index(manager_events[1]) < all_events.index(runtime_events[0]) <
                              all_events.index(manager_events[2]) < all_events.index(arm_event) <
                              all_events.index(bootstrap_event)) or
        "HERDR_RELAY_TRANSPORT='tailscale-cli'" not in (cli_dev_root / "relay.env").read_text(encoding="utf-8") or
        production_env.read_bytes() != production_snapshot or sentinel.exists() or tool_sentinel.exists()):
        raise AssertionError(f"positive CLI setup escaped development boundaries: {all_events!r}")
    setup_target = os.readlink(cli_dev_root / "current")
    if not (cli_dev_root / setup_target / "web" / "version.json").is_file():
        raise AssertionError("staged development release omitted web/version.json")
    setup_bootstrap_count = sum("--operation arm_bootstrap" in event for event in all_events)
    update_env = dict(positive_env)
    update_env["HERDR_DEV_TAILSCALE_CLI_RELAY_BIN"] = str(cli_dev_root / "current" / "bin" / "herdr-mobile-relay")
    update_cli = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "update"], env=update_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=30, check=False,
    )
    if update_cli.returncode != 0:
        raise AssertionError(f"positive CLI development update failed: {update_cli.stdout + update_cli.stderr!r}")
    updated_events = fixture_log.read_text(encoding="utf-8").splitlines()
    update_events = updated_events[len(all_events):]
    update_managers = [event for event in update_events if event.startswith("manager|")]
    if (len(update_managers) != 3 or
        not update_managers[0].startswith("manager|tailscale-cli activation-check") or
        any(not event.startswith("manager|workflow assert-ready ") or
            "--development-root " + str(cli_dev_root) not in event for event in update_managers[1:]) or
        any("publish" in event or "unpublish" in event for event in update_managers[1:]) or
        sum("--operation arm_bootstrap" in event for event in updated_events) != setup_bootstrap_count or
        production_env.read_bytes() != production_snapshot or sentinel.exists() or tool_sentinel.exists()):
        raise AssertionError(f"positive CLI update mutated route or production state: {updated_events!r}")

    current_pointer = cli_dev_root / "current"
    prior_target = os.readlink(current_pointer)
    prior_release = cli_dev_root / prior_target
    prior_binary = (prior_release / "bin" / "herdr-mobile-relay").read_bytes()
    prior_bundle = (prior_release / "web" / "version.json").read_bytes()
    missing_bundle = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "update"],
        env=dict(update_env, DEV_FIXTURE_BUN_MODE="missing-version"), cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=30, check=False,
    )
    if (missing_bundle.returncode == 0 or os.readlink(current_pointer) != prior_target or
        (prior_release / "bin" / "herdr-mobile-relay").read_bytes() != prior_binary or
        (prior_release / "web" / "version.json").read_bytes() != prior_bundle):
        raise AssertionError(
            "development release cut over without staged web/version.json or damaged the prior release: "
            f"{missing_bundle.stdout + missing_bundle.stderr!r}"
        )
    cutover_tools = cli_dev_fixture / "cutover-tools"
    cutover_tools.mkdir(mode=0o700)
    mv_wrapper = cutover_tools / "mv"
    mv_wrapper.write_text(
        '''#!/bin/sh
first=$1
last=
for arg in "$@"; do last=$arg; done
if [ "$last" = "$DEV_FIXTURE_ROOT/current" ]; then
    printf '%s\\n' "$DEV_FIXTURE_CUTOVER_MODE" > "$DEV_FIXTURE_CUTOVER_MARKER"
    case "$DEV_FIXTURE_CUTOVER_MODE" in
        fail) exit 71 ;;
        signal) kill -TERM "$PPID"; exit 143 ;;
    esac
fi
exec "$DEV_FIXTURE_REAL_MV" "$@"
''',
        encoding="utf-8",
    )
    mv_wrapper.chmod(0o700)
    for cutover_mode in ("fail", "signal"):
        cutover_marker = cli_dev_fixture / f"cutover-{cutover_mode}.marker"
        cutover_env = dict(update_env)
        cutover_env.update({
            "PATH": f"{cutover_tools}:{fixture_bin}:/usr/bin:/bin",
            "DEV_FIXTURE_ROOT": str(cli_dev_root),
            "DEV_FIXTURE_CUTOVER_MODE": cutover_mode,
            "DEV_FIXTURE_CUTOVER_MARKER": str(cutover_marker),
            "DEV_FIXTURE_REAL_MV": shutil.which("mv") or "/bin/mv",
        })
        cutover_result = subprocess.run(
            [str(root / "relay" / "dev-tailscale-cli.sh"), "update"], env=cutover_env,
            cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=30, check=False,
        )
        if (cutover_result.returncode == 0 or not cutover_marker.exists() or
            os.readlink(current_pointer) != prior_target or
            (prior_release / "bin" / "herdr-mobile-relay").read_bytes() != prior_binary or
            (prior_release / "web" / "version.json").read_bytes() != prior_bundle):
            raise AssertionError(
                f"{cutover_mode} release cutover did not preserve the prior coherent release: "
                f"{cutover_result.stdout + cutover_result.stderr!r}"
            )
    positive_socket.close()
    print("PASS CLI development lifecycle fixture: isolated positive setup/update, development scope, exact-route recheck, production state preserved")

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

#!/usr/bin/env python3
"""Hosted-only refusal/isolation checks for managed Tailscale CLI fixtures.

Any service process in this suite is paired with a named fake executable and
private temporary state. It never invokes a real Tailscale CLI/daemon or a
personal service. The separate extracted-package browser gate exercises the
packaged CLI-backed app only inside its disposable network-disabled container.
"""

import os

# Admission precedes helper imports, source discovery, roots and subprocesses.
if (os.name != "posix" or os.getuid() == 0 or os.geteuid() == 0 or
        os.environ.get("GITHUB_ACTIONS") != "true" or
        os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or
        os.environ.get("HERDR_TAILSCALE_LAUNCHER_CI") != "1"):
    raise SystemExit(
        "Development launcher fixtures require non-root POSIX GitHub-hosted CI with "
        "GITHUB_ACTIONS=true, RUNNER_ENVIRONMENT=github-hosted and HERDR_TAILSCALE_LAUNCHER_CI=1"
    )

from pathlib import Path
import json
import pty
import select
import shlex
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import termios
import time


def check_negative_admission() -> None:
    # Only an actually admitted parent reaches this regression. Children lose
    # admission from that inherited environment; none gains hosted markers or
    # a fabricated UID. Compile/execute the complete exact candidate, not an
    # extracted guard or fixture body. -I -S excludes local/site startup hooks.
    candidate = Path(__file__).resolve()
    source = candidate.read_bytes()
    if len(source) > 256 * 1024:
        raise AssertionError("launcher admission candidate exceeds the probe source bound")
    harness = r'''
import builtins
import os
import sys

candidate = sys.argv[1]
source = sys.stdin.buffer.read(256 * 1024 + 1)
code = compile(source, candidate, "exec")
sys.argv = [candidate]

def trip():
    os.write(2, b"launcher admission probe containment tripped\n")
    os._exit(97)

def guarded_import(name, globals=None, locals=None, fromlist=(), level=0):
    # Even cached helpers must not be imported after a missing/regressed guard.
    if name == "os" and not fromlist and level == 0:
        return os
    trip()

def before_effect(event, args):
    # The sole permitted audited operation is execution of this complete code
    # object. In particular, file opens, directory enumeration, sockets, forks,
    # subprocess dispatch and further compilation/exec are refused.
    if event != "exec" or args[0] is not code:
        trip()

builtins.__import__ = guarded_import
sys.addaudithook(before_effect)
try:
    exec(code, {"__name__": "__main__", "__file__": candidate})
except SystemExit as refusal:
    if type(refusal.code) is not str:
        trip()
    # Match normal Python's string SystemExit diagnostic/status, then exit
    # directly so interpreter-shutdown audit events cannot mask the result.
    os.write(2, refusal.code.encode("utf-8") + b"\n")
    os._exit(1)
except BaseException:
    trip()
trip()
'''
    expected = (
        b"Development launcher fixtures require non-root POSIX GitHub-hosted CI with "
        b"GITHUB_ACTIONS=true, RUNNER_ENVIRONMENT=github-hosted and HERDR_TAILSCALE_LAUNCHER_CI=1\n"
    )
    cases = (
        ("all-admission-absent", {"HERDR_TAILSCALE_LAUNCHER_CI": None,
                                  "GITHUB_ACTIONS": None, "RUNNER_ENVIRONMENT": None}),
        ("launcher-marker-only", {"GITHUB_ACTIONS": None, "RUNNER_ENVIRONMENT": None}),
        ("launcher-marker-absent", {"HERDR_TAILSCALE_LAUNCHER_CI": None}),
        ("launcher-marker-invalid", {"HERDR_TAILSCALE_LAUNCHER_CI": "0"}),
        ("github-marker-absent", {"GITHUB_ACTIONS": None}),
        ("github-marker-invalid", {"GITHUB_ACTIONS": "false"}),
        ("runner-marker-absent", {"RUNNER_ENVIRONMENT": None}),
        ("self-hosted-runner", {"RUNNER_ENVIRONMENT": "self-hosted"}),
    )
    for case, removals in cases:
        environment = dict(os.environ)
        for name, value in removals.items():
            if value is None:
                environment.pop(name, None)
            else:
                environment[name] = value
        process = subprocess.Popen(
            [sys.executable, "-I", "-S", "-B", "-c", harness, str(candidate)],
            env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, start_new_session=True,
        )
        try:
            try:
                stdout, stderr = process.communicate(input=source, timeout=5)
            except subprocess.TimeoutExpired:
                raise AssertionError(f"{case}: launcher admission probe timed out") from None
        finally:
            # Only this newly created child is owned here; the audit sentinel
            # forbids it from starting descendants. Do not signal host jobs.
            if process.poll() is None:
                process.kill()
                process.communicate(timeout=2)
        if process.returncode != 1 or stdout != b"" or stderr != expected:
            # Do not print arbitrary child output or private environment data.
            raise AssertionError(f"{case}: exact early launcher admission refusal not observed")
    print("PASS launcher admission: negative environments refuse before helper imports or effects")


check_negative_admission()

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
                 "HERDR_DEV_TAILSCALE_CLI_BIN", "HERDR_TAILSCALE_CLI_BIN",
                 "HERDR_TEST_UNAME_S", "HERDR_TEST_UNAME_M",
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
            if (entrypoint == tunnel_script and case not in {
                    "linux_without_app_store_uses_legacy_mode", "darwin_without_app_store_uses_legacy_mode",
                    "app_store_missing_maps_to_legacy_mode"} and
                    b"Set up isolated managed Tailscale" in stdout + stderr):
                raise AssertionError("menu selection demanded redundant development consent")
            if (entrypoint == tunnel_script and case != "explicit_cli_transport_requires_opt_in" and
                    b"Set up isolated CLI-backed Serve development?" in stdout + stderr):
                raise AssertionError("menu selection demanded redundant CLI development consent")
            if case == "menu_uses_all_safe_defaults":
                output = stdout + stderr
                for value in (
                    b"1. Cloudflare tunnel",
                    b"temporary public URL + QR",
                    b"no Tailscale needed",
                    b"2. Tailscale Serve",
                    b"private tailnet HTTPS + QR",
                    b"Choice [1]: ",
                    b"Private development state: " + str(root / "relay" / ".dev-tailscale").encode(),
                    f"Tailscale CLI from PATH: {cli}".encode(),
                    f"Herdr executable from PATH: {herdr}".encode(),
                    f"Herdr socket path: {default_socket_dir / 'herdr.sock'}".encode(),
                    b"Development ports: relay 8375, plugin 18378, HTTPS Serve 8443",
                ):
                    if value not in output:
                        raise AssertionError(f"{case}: default not selected: {value!r}")
                if sum(b". Tailscale Serve" in line for line in output.splitlines()) != 1:
                    raise AssertionError(f"{case}: expected exactly one Tailscale option: {output!r}")
            if case in {
                "app_store_supported_version_keeps_cli_option_available",
                "duplicate_symlinks_to_one_cli_candidate_are_deduplicated",
                "development_override_precedes_legacy_and_path_candidates",
                "legacy_override_is_used_when_development_override_is_empty",
                "exact_app_store_install_wrapper_aliases_bundle_in_menu",
                "app_store_menu_selection_is_cli_development_opt_in",
            }:
                output = stdout + stderr
                option2_lines = [line for line in output.splitlines() if b"2. Tailscale Serve" in line]
                if len(option2_lines) != 1 or b"[UNAVAILABLE:" in option2_lines[0]:
                    raise AssertionError(f"{case}: option 2 was incorrectly marked unavailable: {output!r}")
                if (case in {"exact_app_store_install_wrapper_aliases_bundle_in_menu",
                             "app_store_menu_selection_is_cli_development_opt_in"} and
                    b"Tailscale app's CLI" not in option2_lines[0]):
                    raise AssertionError(f"{case}: menu did not select CLI-backed mode: {output!r}")
            if case == "non_exact_app_store_wrapper_keeps_ambiguity_in_menu":
                output = stdout + stderr
                option2_lines = [line for line in output.splitlines() if b"2. Tailscale Serve" in line]
                if len(option2_lines) != 1 or b"[UNAVAILABLE:" not in option2_lines[0]:
                    raise AssertionError(f"{case}: non-exact wrapper did not keep the option unavailable: {output!r}")
            if case == "unsupported_os_marks_tailscale_unavailable":
                output = stdout + stderr
                option2_lines = [line for line in output.splitlines() if b"2. Tailscale Serve" in line]
                if (len(option2_lines) != 1 or b"[UNAVAILABLE:" not in option2_lines[0] or
                    b"Tailscale Serve unavailable:" not in output):
                    raise AssertionError(f"{case}: unsupported OS was offered or dispatched to legacy mode: {output!r}")
            if case in {
                "linux_without_app_store_uses_legacy_mode",
                "darwin_without_app_store_uses_legacy_mode",
                "app_store_missing_maps_to_legacy_mode",
            }:
                output = stdout + stderr
                option2_lines = [line for line in output.splitlines() if b"2. Tailscale Serve" in line]
                if (len(option2_lines) != 1 or b"[UNAVAILABLE:" in option2_lines[0] or
                    b"standalone Tailscale" not in option2_lines[0] or
                    b"route ends when the relay stops" not in option2_lines[0]):
                    raise AssertionError(f"{case}: option 2 did not select legacy mode: {output!r}")
            if sentinel.exists() or (dev / "relay.env").exists() or (base / "unused-tunnel").exists():
                raise AssertionError(f"{case}: touched a CLI or dev state before consent")
            print(f"PASS dev-tailscale preflight: {case}")
        finally:
            os.close(master)

    default_menu_tools = base / "default-menu-tools"
    default_menu_tools.mkdir(mode=0o700)
    default_menu_uname = default_menu_tools / "uname"
    default_menu_uname.write_text(
        "#!/bin/sh\ncase \"$1\" in\n"
        "  -s) printf '%s\\n' \"${HERDR_TEST_UNAME_S:-Linux}\" ;;\n"
        "  -m) printf '%s\\n' \"${HERDR_TEST_UNAME_M:-x86_64}\" ;;\n"
        "  *) exit 2 ;;\nesac\n",
        encoding="utf-8",
    )
    default_menu_uname.chmod(0o700)
    menu_linux_settings = {
        "HERDR_TEST_UNAME_S": "Linux",
        "HERDR_TEST_UNAME_M": "x86_64",
        "PATH": f"{default_menu_tools}:{base}:/usr/bin:/bin",
    }
    interactive_refused("direct_interactive_decline", script, b"n\n", b"Cancelled; nothing was started.")
    interactive_refused("explicit_cli_transport_requires_opt_in", tunnel_script, b"n\n", b"Cancelled; nothing was started.",
                        HERDR_DEV_TRANSPORT="tailscale-cli",
                        HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                        ACTIVATION_CHECK_RECORD=str(activation_record))
    interactive_refused("dev_tunnel_rejects_relative_state", tunnel_script, b"2\n",
                        b"Choose an absolute private state directory, or unset HERDR_DEV_TAILSCALE_DIR",
                        HERDR_DEV_TAILSCALE_DIR="relative", **menu_linux_settings)
    interactive_refused("consent_does_not_create_custom_root", script, b"y\n",
                        b"Create a custom private state directory first",
                        HERDR_DEV_TAILSCALE_DIR=str(base / "missing"))
    checkout_default = root / "relay" / ".dev-tailscale"
    checkout_default_existed = checkout_default.exists()

    def check_checkout_root_existence_unchanged() -> None:
        if checkout_default.exists() != checkout_default_existed:
            raise AssertionError("development preflight changed checkout-local state existence")

    check_checkout_root_existence_unchanged()
    interactive_refused("invalid_cli_does_not_create_default", tunnel_script, b"2\n",
                        b"Not an executable file:",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN=str(base / "missing-cli"),
                        **menu_linux_settings)
    interactive_refused("menu_uses_all_safe_defaults", tunnel_script, b"2\n",
                        b"Production and dev-tunnel ports are reserved",
                        HERDR_DEV_TAILSCALE_DIR="", HERDR_DEV_TAILSCALE_BIN="",
                        HERDR_DEV_HERDR_BIN="", HERDR_DEV_HERDR_SOCKET="",
                        HERDR_SOCKET_PATH="", HERDR_DEV_TAILSCALE_PORT="8375",
                        HERDR_DEV_TAILSCALE_PLUGIN_PORT="", HERDR_DEV_TAILSCALE_HTTPS_PORT="",
                        **menu_linux_settings)
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
    menu_tools_dir = home / "menu-tools"
    menu_tools_dir.mkdir(mode=0o700)
    menu_uname = menu_tools_dir / "uname"
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
    menu_plutil = menu_tools_dir / "plutil"
    menu_plutil.write_text(
        "#!/bin/sh\n"
        "test \"$1\" = -extract && test \"$2\" = CFBundleShortVersionString && "
        "test \"$3\" = raw && test \"$4\" = -o && test \"$5\" = - || exit 2\n"
        "sed -n '/<key>CFBundleShortVersionString<\\/key>/{n;s/.*<string>\\([^<]*\\)<\\/string>.*/\\1/p;}' \"$6\"\n",
        encoding="utf-8",
    )
    menu_plutil.chmod(0o700)
    for utility in ("readlink", "dirname", "basename", "sed"):
        (menu_tools_dir / utility).symlink_to(f"/usr/bin/{utility}")
    # Main's portable `#!/usr/bin/env bash` entrypoints resolve Bash from the
    # same controlled PATH used to enumerate Tailscale CLI candidates.
    (menu_tools_dir / "bash").symlink_to(shutil.which("bash") or "/bin/bash")
    app_store_cli = app_store_bundle / "Contents" / "MacOS" / "Tailscale"
    app_store_cli.parent.mkdir(mode=0o700, parents=True)
    app_store_cli.write_text(f"#!/bin/sh\nprintf invoked >> '{sentinel}'\nexit 97\n", encoding="utf-8")
    app_store_cli.chmod(0o700)
    receipt.unlink()
    app_store_settings = {
        "HERDR_DEV_TAILSCALE_CLI_BIN": str(menu_cli),
        "HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(cli_relay),
        "ACTIVATION_CHECK_RECORD": str(activation_record),
        "PATH": f"{menu_tools_dir}:{menu_cli_dir}:/usr/bin:/bin",
    }

    def menu_settings_for_path(path: str, **overrides: str) -> dict[str, str]:
        settings = dict(app_store_settings)
        settings.pop("HERDR_DEV_TAILSCALE_CLI_BIN", None)
        settings["HERDR_TAILSCALE_CLI_BIN"] = ""
        settings["PATH"] = path
        settings.update(overrides)
        return settings
    interactive_refused(
        "app_store_missing_maps_to_legacy_mode", tunnel_script, b"2\n",
        b"Production and dev-tunnel ports are reserved",
        HERDR_DEV_TAILSCALE_PORT="8375", **app_store_settings,
    )
    for system, arch, case in (
        ("Linux", "x86_64", "linux_without_app_store_uses_legacy_mode"),
        ("Darwin", "x86_64", "darwin_without_app_store_uses_legacy_mode"),
    ):
        interactive_refused(
            case, tunnel_script, b"2\n",
            b"Production and dev-tunnel ports are reserved",
            HERDR_TEST_UNAME_S=system, HERDR_TEST_UNAME_M=arch,
            HERDR_DEV_TAILSCALE_PORT="8375", **app_store_settings,
        )
    unsupported_os_settings = dict(
        menu_linux_settings, HERDR_TEST_UNAME_S="FreeBSD", HERDR_TEST_UNAME_M="amd64",
    )
    interactive_refused(
        "unsupported_os_marks_tailscale_unavailable", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: Tailscale development is supported only on Linux and macOS",
        HERDR_DEV_TAILSCALE_PORT="8375", **unsupported_os_settings,
    )
    receipt.write_bytes(b"synthetic App Store receipt marker")
    activation_before_menu = activation_record.read_bytes() if activation_record.exists() else None
    interactive_refused(
        "app_store_unsupported_version_marks_cli_mode_unavailable", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: Only the App Store Tailscale 1.102.4 profile is enabled",
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
        b"Tailscale Serve unavailable: Could not read the App Store bundle version", **app_store_settings,
    )
    app_store_info.write_text(
        '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>\n'
        '<key>CFBundleShortVersionString</key>\n<string>1.102.4</string>\n'
        '</dict></plist>\n',
        encoding="utf-8",
    )
    def write_menu_cli(path: Path) -> None:
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_text(f"#!/bin/sh\nprintf invoked >> '{sentinel}'\nexit 97\n", encoding="utf-8")
        path.chmod(0o700)

    menu_system_path = str(menu_tools_dir)
    menu_fixture_dir = home / "menu-script-fixture"
    menu_fixture_dir.mkdir(mode=0o700)
    menu_fixture_script = menu_fixture_dir / "dev-tunnel.sh"
    menu_source = tunnel_script.read_text(encoding="utf-8")
    app_store_cli_literal = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
    if menu_source.count("MENU_APP_STORE_CLI=" + app_store_cli_literal) != 1:
        raise AssertionError("menu fixture could not identify the fixed App Store fallback")
    menu_fixture_script.write_text(
        menu_source.replace("MENU_APP_STORE_CLI=" + app_store_cli_literal,
                            "MENU_APP_STORE_CLI=" + str(app_store_cli)),
        encoding="utf-8",
    )
    menu_fixture_script.chmod(0o700)
    exact_wrapper_dir = home / "exact-app-store-wrapper"
    exact_wrapper = exact_wrapper_dir / "tailscale"
    exact_wrapper_dir.mkdir(mode=0o700)
    exact_wrapper.write_text(f'#!/bin/sh\n{app_store_cli} "$@"\n', encoding="utf-8")
    exact_wrapper.chmod(0o700)
    exact_wrapper_path = f"{exact_wrapper_dir}:{menu_system_path}:/usr/bin:/bin"
    interactive_refused(
        "exact_app_store_install_wrapper_aliases_bundle_in_menu", menu_fixture_script, b"3\n",
        b"Choose 1 or 2.", **menu_settings_for_path(exact_wrapper_path),
    )
    exact_wrapper.write_text(f'#!/bin/sh\n{app_store_cli} "$@"\n# extra bytes\n', encoding="utf-8")
    interactive_refused(
        "non_exact_app_store_wrapper_keeps_ambiguity_in_menu", menu_fixture_script, b"2\n",
        b"Tailscale Serve unavailable: Multiple distinct executable CLI candidates",
        **menu_settings_for_path(exact_wrapper_path),
    )
    exact_wrapper.write_text(f'#!/bin/sh\n{app_store_cli} "$@"\n', encoding="utf-8")

    app_store_path_dir = home / "app-store-cli-path"
    app_store_path_dir.mkdir(mode=0o700)
    (app_store_path_dir / "tailscale").symlink_to(app_store_cli)
    # The executable text stand-in is a distinct resolver candidate, not a
    # symlink whose contents the menu could inspect or execute.
    distinct_path_dir = home / "distinct-cli-path"
    distinct_path_cli = distinct_path_dir / "tailscale"
    write_menu_cli(distinct_path_cli)
    two_path_a_dir = home / "two-path-a"
    two_path_b_dir = home / "two-path-b"
    two_path_a = two_path_a_dir / "tailscale"
    two_path_b = two_path_b_dir / "tailscale"
    write_menu_cli(two_path_a)
    write_menu_cli(two_path_b)
    shared_path_cli = home / "shared-cli" / "tailscale"
    write_menu_cli(shared_path_cli)
    duplicate_a_dir = home / "duplicate-path-a"
    duplicate_b_dir = home / "duplicate-path-b"
    duplicate_a_dir.mkdir(mode=0o700)
    duplicate_b_dir.mkdir(mode=0o700)
    (duplicate_a_dir / "tailscale").symlink_to(shared_path_cli)
    (duplicate_b_dir / "tailscale").symlink_to(shared_path_cli)
    relative_path_dir = base / "relative-path-cli"
    relative_path_cli = relative_path_dir / "tailscale"
    write_menu_cli(relative_path_cli)
    relative_path_entry = os.path.relpath(relative_path_dir, root)
    noexec_cli = home / "noexec-cli"
    noexec_cli.write_text("not executable\n", encoding="utf-8")
    noexec_cli.chmod(0o600)
    two_candidate_path = f"{two_path_a_dir}:{two_path_b_dir}:{menu_system_path}"
    activation_before_candidate_menu = activation_record.read_bytes() if activation_record.exists() else None
    interactive_refused(
        "supported_bundle_plus_distinct_path_cli_marks_option2_unavailable", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: Multiple distinct executable CLI candidates",
        **menu_settings_for_path(f"{app_store_path_dir}:{distinct_path_dir}:{menu_system_path}"),
    )
    interactive_refused(
        "two_distinct_path_candidates_mark_option2_unavailable", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: Multiple distinct executable CLI candidates",
        **menu_settings_for_path(two_candidate_path),
    )
    interactive_refused(
        "duplicate_symlinks_to_one_cli_candidate_are_deduplicated", tunnel_script, b"3\n",
        b"Choose 1 or 2.",
        **menu_settings_for_path(f"{duplicate_a_dir}:{duplicate_b_dir}:{menu_system_path}"),
    )
    interactive_refused(
        "relative_path_candidate_is_ignored", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: No executable Tailscale CLI candidate was found",
        **menu_settings_for_path(f"{relative_path_entry}:{menu_system_path}"),
    )
    interactive_refused(
        "development_override_precedes_legacy_and_path_candidates", tunnel_script, b"3\n",
        b"Choose 1 or 2.",
        **menu_settings_for_path(
            two_candidate_path,
            HERDR_DEV_TAILSCALE_CLI_BIN=str(two_path_a),
            HERDR_TAILSCALE_CLI_BIN=str(two_path_b),
        ),
    )
    interactive_refused(
        "legacy_override_is_used_when_development_override_is_empty", tunnel_script, b"3\n",
        b"Choose 1 or 2.",
        **menu_settings_for_path(
            two_candidate_path,
            HERDR_DEV_TAILSCALE_CLI_BIN="",
            HERDR_TAILSCALE_CLI_BIN=str(two_path_b),
        ),
    )
    interactive_refused(
        "invalid_development_override_does_not_fall_back", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: The selected Tailscale CLI override must be an absolute executable regular file.",
        **menu_settings_for_path(
            two_candidate_path,
            HERDR_DEV_TAILSCALE_CLI_BIN="relative/tailscale",
            HERDR_TAILSCALE_CLI_BIN=str(two_path_a),
        ),
    )
    interactive_refused(
        "invalid_legacy_override_does_not_fall_back_to_path", tunnel_script, b"2\n",
        b"Tailscale Serve unavailable: The selected Tailscale CLI override must be an absolute executable regular file.",
        **menu_settings_for_path(
            f"{distinct_path_dir}:{menu_system_path}",
            HERDR_DEV_TAILSCALE_CLI_BIN="",
            HERDR_TAILSCALE_CLI_BIN=str(noexec_cli),
        ),
    )
    activation_after_candidate_menu = activation_record.read_bytes() if activation_record.exists() else None
    if activation_after_candidate_menu != activation_before_candidate_menu:
        raise AssertionError("menu candidate detection dispatched CLI activation checks")
    interactive_refused(
        "app_store_menu_selection_is_cli_development_opt_in", tunnel_script, b"2\n\n",
        b"Custom development roots must already exist with mode 0700.",
        HERDR_DEV_TAILSCALE_CLI_DIR=str(base / "missing-cli-menu-root"), **app_store_settings,
    )
    interactive_refused(
        "menu_preserves_invalid_cli_opt_in_override", tunnel_script, b"2\n",
        b"HERDR_DEV_TAILSCALE_CLI_ENABLE must be 1 to opt in.",
        HERDR_DEV_TAILSCALE_CLI_ENABLE="0", **app_store_settings,
    )
    interactive_refused(
        "app_store_supported_version_keeps_cli_option_available", tunnel_script, b"3\n",
        b"Choose 1 or 2.", **app_store_settings,
    )
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
    # Probe CLI selection with the automatic app default. Custom-origin
    # normalization is independently covered with the strict compiled helper.
    cli_dev_gate = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")],
        env=dict(without_consent, HERDR_DEV_TAILSCALE_CLI_ENABLE="1",
                 HERDR_DEV_TAILSCALE_CLI_DIR=str(private_cli_dev),
                 HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(cli_relay),
                 HERDR_DEV_PHONE_APP_URL="", HERDR_PHONE_APP_URL="",
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
        "HERDR_TAILSCALE_CLI_SCOPE=production\n"
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
    # Direct decline, two menu refusals, unattended refusal, and CLI selection.
    if (activation_checks.count("tailscale-cli activation-check --scope development") != 5 or
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
    (custom_dev_root / "relay.env").chmod(0o600)
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

    # A development-scoped direct unpublish cannot claim an installed service.
    # Both answers must refuse before any route-removal manager call.
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

    for answer in (b"n\n", b"y\n"):
        status, output = run_unpublish_consent(answer)
        if status == 0 or b"installed CLI transport and service configuration must be selected explicitly" not in output:
            raise AssertionError(f"development-scoped CLI unpublish was not refused: {output!r}")
    if unpublish_record.read_text(encoding="utf-8").splitlines() != ["tailscale-cli activation-check"] * 2:
        raise AssertionError("development-scoped CLI unpublish dispatched a route-removal manager")
    print("PASS CLI shell unpublish fixture: development scope cannot remove an installed route")

    # Generic installed-service dispatch tries only activation-check and then
    # refuses (69); direct CLI dispatch without a selected service definition
    # refuses before activation (78). Neither contacts the CLI or starts a child.
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
        "[ \"$*\" = 'tailscale-cli activation-check' ] || { echo unexpected service relay command >&2; exit 97; }\n"
        "exit 69\n",
        encoding="utf-8",
    )
    cli_service_relay.chmod(0o700)
    service_env = dict(env)
    service_env.update({
        "HOME": str(cli_service_home), "HERDR_RELAY_ENV": str(cli_service_env),
        "HERDR_RELAY_BIN": str(cli_service_relay),
        "HERDR_CLI_SERVICE_RECORD": str(cli_service_record),
    })
    fixed_diagnostic = b"Installed-service CLI startup is disabled: unavailable/non-ready; no service was started.\n"
    for entrypoint, status, diagnostic in (
            ("herdr-mobile-relay-service.sh", 69, fixed_diagnostic),
            ("tailscale-cli-service.sh", 78, b"")):
        result = subprocess.run(
            [str(cli_service_scripts / entrypoint)], env=service_env, cwd=root,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
        )
        if result.returncode != status or result.stdout != b"" or result.stderr != diagnostic:
            raise AssertionError(
                f"{entrypoint} did not refuse before a CLI child: "
                f"{result.returncode!r} {result.stdout + result.stderr!r}")
    if (cli_service_record.read_text(encoding="utf-8").splitlines() != ["tailscale-cli activation-check"] or
            sentinel.exists()):
        raise AssertionError("installed-service entrypoint contacted CLI or ran unexpected relay commands")
    print("PASS CLI service fixture: generic activation gate (69) and unselected direct dispatch (78) refuse without CLI effects")

    # A named user-service fixture admits only a fake exact-route verifier and
    # fake systemctl/curl. It never reaches the host service manager, Tailscale,
    # Cloudflare, or a listening relay. Installers keep main's staged native
    # transaction and exact readiness gate, so the fake relay delegates strict
    # JSON fields and readiness verification to the compiled relay helper.
    readiness_helper = base / "readiness-helper"
    subprocess.run(
        ["go", "build", "-o", str(readiness_helper), str(root / "cmd" / "herdr-mobile-relay")],
        cwd=root, stdin=subprocess.DEVNULL, timeout=900, check=True,
    )
    fixture_identity = '{"version":"0.9.0","revision":"fixture-revision"}'
    fixture_manifest = '{"version":"0.9.0","revision":"fixture-revision","web_hash":"fixture-web"}\n'

    def fixture_readiness(instance: str) -> str:
        return (
            '{"status":"ready","inventory":{"state":"ready"},"instance":"' + instance +
            '","release_version":"0.9.0","revision":"fixture-revision","bundle_hash":"fixture-web","protocol":3}'
        )

    def fixture_relay_preamble() -> str:
        return (
            "case \"$1\" in json-field|verify-readiness) exec " + shlex.quote(str(readiness_helper)) + " \"$@\" ;; esac\n"
            "if [ \"$*\" = 'version --json' ]; then printf '%s\\n' '" + fixture_identity + "'; exit 0; fi\n"
        )

    cli_install_root = base / "cli install & 'quoted' fixture"
    cli_install_home = cli_install_root / "home"
    cli_install_home.mkdir(mode=0o700, parents=True)
    cli_install_root.chmod(0o700)
    cli_install_scripts = cli_install_root / "relay"
    cli_install_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "native-install-transaction.sh", "install-systemd-user-service.sh",
                 "herdr-mobile-relay-service.sh", "tailscale-cli-service.sh", "service.sh"):
        shutil.copy2(root / "relay" / name, cli_install_scripts / name)
    (cli_install_root / "release-manifest.json").write_text(fixture_manifest, encoding="utf-8")
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
        "#!/bin/sh\nprintf '%s\\n' '" + fixture_readiness("fixture-installation") + "'\n",
        encoding="utf-8",
    )
    (cli_install_bin / "curl").chmod(0o700)
    # The installer's systemd-analyze verification must not reach the host.
    (cli_install_bin / "systemd-analyze").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    (cli_install_bin / "systemd-analyze").chmod(0o700)
    cli_install_user_bin = cli_install_home / ".local" / "bin"
    cli_install_user_bin.mkdir(mode=0o700, parents=True)
    for name in ("systemctl", "curl", "uname", "systemd-analyze"):
        shutil.copy2(cli_install_bin / name, cli_install_user_bin / name)
    manager_relay = cli_install_root / "fake-manager-relay"
    manager_relay.write_text(
        "#!/bin/sh\n" + fixture_relay_preamble() +
        "printf '%s\\n' \"$*\" >> \"$MANAGER_RECORD\"\n"
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
        "HERDR_RELAY_HOST=127.0.0.1\nHERDR_RELAY_PORT=18577\nHERDR_RELAY_PLUGIN_PORT=18578\n"
        f"HERDR_TAILSCALE_CLI_BIN={shlex.quote(str(cli))}\nHERDR_TAILSCALE_CLI_SCOPE=development\n"
        f"HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:9443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={shlex.quote(str(cli_install_state))}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={shlex.quote(str(cli_install_coordination))}\n"
        f"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT={shlex.quote(str(cli_install_root))}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\nHERDR_REACHABILITY_PORT_MAPPING=0\n",
        encoding="utf-8",
    )
    # The same environment plus the saved foreground identities the installed policy refuses.
    cli_install_foreground_env = cli_install_root / "foreground.env"
    cli_install_foreground_env.write_text(
        "HERDR_RELAY_CONTROL_RUN_ID=fixture-control\n"
        f"HERDR_RELAY_PAIRING_SOCKET={shlex.quote(str(cli_install_root / 'control.sock'))}\n"
        + cli_install_env.read_text(encoding="utf-8"),
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
    # A1: the installed service never accepts the tailscale-cli transport. The installer refuses it
    # before it writes a unit, runs systemctl or calls the relay: an installed-form environment is
    # 'Unavailable background transport.', one that still carries the saved foreground identities is
    # 'Installed policy refuses foreground identities.' (relay/common.sh native_installer_admit),
    # with and without the setup opt-in variable.
    unit = cli_install_home / ".config" / "systemd" / "user" / "herdr-mobile-relay.service"
    for variant, selected_env, fixed_message in (
        ("installed-form", cli_install_env, b"Unavailable background transport."),
        ("foreground-identities", cli_install_foreground_env, b"Installed policy refuses foreground identities."),
    ):
        for extra in ({}, {"HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START": "1"}):
            installer_refusal = subprocess.run(
                [str(cli_install_scripts / "install-systemd-user-service.sh")],
                env={**install_env, "HERDR_RELAY_ENV": str(selected_env), **extra}, cwd=root,
                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False,
            )
            if installer_refusal.returncode == 0 or fixed_message not in installer_refusal.stdout + installer_refusal.stderr:
                raise AssertionError(
                    f"CLI service installer ({variant}, {sorted(extra)}) did not refuse with its fixed message: "
                    f"{installer_refusal.returncode!r} {installer_refusal.stdout + installer_refusal.stderr!r}"
                )
    if unit.exists() or systemctl_record.exists() or manager_record.exists() or sentinel.exists():
        raise AssertionError("refused CLI service installer wrote a unit, ran systemctl, called the relay or contacted the CLI")
    manager_events = manager_record.read_text(encoding="utf-8").splitlines() if manager_record.exists() else []
    stopped = subprocess.run(
        [str(cli_install_scripts / "service.sh"), "stop"],
        env=install_env, cwd=root, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
    )
    if (stopped.returncode != 0 or
        systemctl_record.read_text(encoding="utf-8").splitlines() != ["--user stop herdr-mobile-relay.service"] or
        (manager_record.read_text(encoding="utf-8").splitlines() if manager_record.exists() else []) != manager_events or
        sentinel.exists()):
        raise AssertionError("CLI service stop mutated registration or escaped the fake service manager")
    print("PASS CLI user-service fixture: the installed policy refuses the tailscale-cli transport and saved foreground identities with no unit, manager or systemctl effect; stop is route-preserving")

    # macOS launchd acceptance also uses only named fixtures; launchctl/curl are
    # shadowed and the CLI route verifier is read-only and synthetic.
    mac_fixture = base / "mac service & 'quoted' fixture"
    mac_home = mac_fixture / "home"
    mac_home.mkdir(mode=0o700, parents=True)
    mac_fixture.chmod(0o700)
    mac_scripts = mac_fixture / "relay"
    mac_scripts.mkdir(mode=0o700)
    for name in ("common.sh", "native-install-transaction.sh", "install-service.sh",
                 "herdr-mobile-relay-service.sh", "tailscale-cli-service.sh", "service.sh"):
        shutil.copy2(root / "relay" / name, mac_scripts / name)
    (mac_fixture / "release-manifest.json").write_text(fixture_manifest, encoding="utf-8")
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
    # The user domain is available for the native transaction snapshot; the
    # relay job itself is initially unloaded.
    (mac_bin / "launchctl").write_text(
        "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LAUNCHCTL_RECORD\"\n"
        "case \"$1\" in print) case \"$2\" in gui/*/*) exit 1 ;; gui/*) exit 0 ;; esac; exit 1 ;; "
        "bootout|bootstrap|enable|kickstart) exit 0 ;; esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    (mac_bin / "launchctl").chmod(0o700)
    # Hosted Linux has no plutil; the stand-in still rejects an empty plist.
    (mac_bin / "plutil").write_text("#!/bin/sh\n[ \"$1\" = -lint ] && [ -s \"$2\" ]\n", encoding="utf-8")
    (mac_bin / "plutil").chmod(0o700)
    (mac_bin / "curl").write_text(
        "#!/bin/sh\nprintf '%s\\n' '" + fixture_readiness("mac-fixture-instance") + "'\n",
        encoding="utf-8",
    )
    (mac_bin / "curl").chmod(0o700)
    mac_manager = mac_fixture / "fake-manager-relay"
    mac_manager.write_text(
        "#!/bin/sh\n" + fixture_relay_preamble() +
        "printf '%s\\n' \"$*\" >> \"$MAC_MANAGER_RECORD\"\n"
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
        "HERDR_RELAY_INSTANCE_ID=mac-fixture-instance\n"
        "HERDR_RELAY_HOST=127.0.0.1\nHERDR_RELAY_PORT=18577\nHERDR_RELAY_PLUGIN_PORT=18578\n"
        f"HERDR_TAILSCALE_CLI_BIN={cli}\nHERDR_TAILSCALE_CLI_SCOPE=development\n"
        "HERDR_TAILSCALE_CLI_ORIGIN=https://relay.fixture.invalid:9443\n"
        f"HERDR_TAILSCALE_CLI_STATE_ROOT={shlex.quote(str(mac_state))}\n"
        f"HERDR_TAILSCALE_CLI_COORDINATION_ROOT={shlex.quote(str(mac_coordination))}\n"
        f"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT={shlex.quote(str(mac_fixture))}\n"
        "HERDR_TAILSCALE_CLI_HTTPS_PORT=9443\nHERDR_REACHABILITY_PORT_MAPPING=0\n",
        encoding="utf-8",
    )
    # The same environment plus the saved foreground identities the installed policy refuses.
    mac_foreground_env = mac_fixture / "foreground.env"
    mac_foreground_env.write_text(
        "HERDR_RELAY_CONTROL_RUN_ID=mac-fixture-control\n"
        f"HERDR_RELAY_PAIRING_SOCKET={shlex.quote(str(mac_fixture / 'control.sock'))}\n"
        + mac_env.read_text(encoding="utf-8"),
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
    # A1: the launchd installer refuses the same two environments before it writes a plist, calls
    # launchctl or the relay, exactly like the systemd installer above.
    plist = mac_home / "Library" / "LaunchAgents" / "com.herdr-mobile-relay.service.plist"
    for variant, selected_env, fixed_message in (
        ("installed-form", mac_env, b"Unavailable background transport."),
        ("foreground-identities", mac_foreground_env, b"Installed policy refuses foreground identities."),
    ):
        mac_refused = subprocess.run(
            [str(mac_scripts / "install-service.sh")], env={**mac_env_settings, "HERDR_RELAY_ENV": str(selected_env)}, cwd=root,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15, check=False,
        )
        if mac_refused.returncode == 0 or fixed_message not in mac_refused.stdout + mac_refused.stderr:
            raise AssertionError(
                f"macOS CLI service installer ({variant}) did not refuse with its fixed message: "
                f"{mac_refused.returncode!r} {mac_refused.stdout + mac_refused.stderr!r}"
            )
    if (plist.exists() or launchctl_record.exists() or mac_manager_record.exists() or sentinel.exists() or
        mac_systemctl_record.exists()):
        raise AssertionError("refused macOS CLI service installer wrote a plist, called launchctl or the relay, or contacted the CLI")
    mac_manager_events = mac_manager_record.read_text(encoding="utf-8").splitlines() if mac_manager_record.exists() else []
    mac_stopped = subprocess.run(
        [str(mac_scripts / "service.sh"), "stop"], env=mac_env_settings, cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False,
    )
    if (mac_stopped.returncode != 0 or
        launchctl_record.read_text(encoding="utf-8").splitlines()[-1].startswith("bootout gui/") is False or
        (mac_manager_record.read_text(encoding="utf-8").splitlines() if mac_manager_record.exists() else []) != mac_manager_events or
        sentinel.exists() or mac_systemctl_record.exists()):
        raise AssertionError("macOS CLI service stop mutated registration or escaped fake launchd")
    print("PASS macOS CLI user-service fixture: the launchd installer refuses the tailscale-cli transport and saved foreground identities with no plist, launchctl or manager effect; stop is route-preserving")

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
    # The A1 removal admission (relay/common.sh service_removal_systemd_configuration and
    # service_removal_observe) refuses anything but the exact `systemctl --user show` property lines.
    # The installed unit is inactive and disabled, so no stop or disable is needed; the answer follows
    # whether the unit file still exists, so a later show reports it removed. Same record line and
    # is-active exit as before; every other call exits 0.
    (cli_uninstall_bin / "systemctl").write_text(
        r"""#!/bin/sh
printf '%s\n' "$*" >> "$UNINSTALL_SYSTEMCTL_RECORD"
case "$*" in *is-active*) exit 1 ;; esac
if [ "$2" = show ]; then
    label=$3
    unit="$HOME/.config/systemd/user/$label"
    if [ -f "$unit" ]; then
        load=loaded; enabled=disabled; fragment=$unit
    else
        load=not-found; enabled=; fragment=
    fi
    case "$*" in
        *--all*) printf 'Id=%s\nLoadState=%s\nActiveState=inactive\nUnitFileState=%s\nFragmentPath=%s\nDropInPaths=\nNeedDaemonReload=no\n' "$label" "$load" "$enabled" "$fragment" ;;
        *) printf 'LoadState=%s\nActiveState=inactive\nUnitFileState=%s\n' "$load" "$enabled" ;;
    esac
fi
exit 0
""",
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
        # Removal authority requires a private, singly linked sentinel.
        (path / ".herdr-mobile-relay-installation").chmod(0o600)
    # The Linux full uninstall stages the installed relay as its mount guard. This fixture proves route and journal
    # retention, not confinement, so the installed relay is a MODEL guard that records and allows (the real guard is run by
    # tests/test_uninstall.sh and tests/test_uninstall_mounts.py).
    uninstall_guard_record = cli_uninstall_root / "guard-record"
    (uninstall_release / "current").mkdir(mode=0o700)
    (uninstall_release / "current" / "herdr-mobile-relay").write_text(
        "#!/bin/sh\n"
        "[ \"$1\" = __private-uninstall-mount-roots ] || exit 64\n"
        "printf '%s\\n' \"$*\" >> \"$UNINSTALL_GUARD_RECORD\"\n",
        encoding="utf-8",
    )
    (uninstall_release / "current" / "herdr-mobile-relay").chmod(0o700)
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
        "UNINSTALL_GUARD_RECORD": str(uninstall_guard_record),
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
    # The guard ran for all three roots twice (the installed relay in place, then its staged copy), then before each destructive
    # step: releases (chmod, rm), config, cache.
    guard_calls = uninstall_guard_record.read_text(encoding="utf-8").splitlines()
    if len(guard_calls) != 6 or any(call.count(" ") != 3 for call in guard_calls[:2]) or any(call.count(" ") != 1 for call in guard_calls[2:]):
        raise AssertionError(f"CLI uninstall did not run the mount guard at its seams: {guard_calls!r}")
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
printf 'build|go\\n' >> "$DEV_FIXTURE_LOG"
out=
while [ $# -gt 0 ]; do
    if [ "$1" = -o ]; then out=$2; shift 2; else shift; fi
done
[ -n "$out" ] || exit 97
cat > "$out" <<'APP'
#!/bin/sh
# Use the compiled strict contracts, not permissive fixture URL/JSON parsing.
case "$1" in
    json-field|normalize-external-origin) exec "$DEV_FIXTURE_NORMALIZER" "$@" ;;
esac
if [ "$1" = dev-tailscale-cli ] && [ "$2" = preflight ]; then
    printf 'manager|workflow preflight --development-root %s\\n' "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT" >> "$DEV_FIXTURE_LOG"
    case "${DEV_FIXTURE_PREFLIGHT_MODE:-}" in
        drift) printf '%s\\n' '{"node_id":"other-node","dns_name":"other.fixture.invalid","origin":"https://other.fixture.invalid:8443","profile":"macos-appstore-1.102.4-supplied-metadata"}'; exit 0 ;;
        fail) exit 71 ;;
        tamper) printf 'HERDR_PHONE_APP_URL=\\047https://changed.fixture.invalid\\047\\n' >> "$HERDR_RELAY_ENV"; exit 71 ;;
        tamper-marker) printf 'changed marker\\n' >> "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT/.herdr-dev-tailscale-cli"; exit 71 ;;
        drop-old-env) rm "$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT/.setup-pending/old-env"; exit 71 ;;
    esac
    printf '%s\\n' '{"node_id":"dev-node-fixture","dns_name":"relay.fixture.invalid","origin":"https://relay.fixture.invalid:8443","profile":"macos-appstore-1.102.4-supplied-metadata"}'
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
        "if [ \"$1\" = json-field ]; then exec \"$DEV_FIXTURE_NORMALIZER\" \"$@\"; fi\n"
        "case \"$1\" in\n"
        "  tailscale-cli)\n"
        "    case \"$2\" in\n"
        "      activation-check) exit 0 ;;\n"
        "      resolve-binary) printf '%s\\n' \"$HERDR_DEV_TAILSCALE_CLI_BIN\"; exit 0 ;;\n"
        "      preflight) printf 'standalone preflight called\\n' >> \"$DEV_FIXTURE_LOG\"; exit 97 ;;\n"
        "      reserve-backend-port) printf 'manager|%s\\n' \"$*\" >> \"$DEV_FIXTURE_LOG\"; exit 0 ;;\n"
        "      release-backend-port) exit 0 ;;\n"
        "    esac ;;\n"
        "  dev-tailscale-cli)\n"
        "    if [ \"$2\" = discover ]; then\n"
        "      printf 'manager|workflow discover --development-root %s\\n' \"$HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT\" >> \"$DEV_FIXTURE_LOG\"\n"
        "      case \"${DEV_FIXTURE_DISCOVERY_MODE:-}\" in\n"
        "        fail) exit 71 ;;\n"
        "        unsupported) printf '%s\\n' '{\"node_id\":\"dev-node-fixture\",\"dns_name\":\"relay.fixture.invalid\",\"origin\":\"https://relay.fixture.invalid:8443\",\"profile\":\"unknown\"}'; exit 0 ;;\n"
        "        malformed) printf '%s\\n' '{invalid'; exit 0 ;;\n"
        "        drift) printf '%s\\n' '{\"node_id\":\"other-node\",\"dns_name\":\"other.fixture.invalid\",\"origin\":\"https://other.fixture.invalid:8443\",\"profile\":\"macos-appstore-1.102.4-supplied-metadata\"}'; exit 0 ;;\n"
        "      esac\n"
        "      printf '%s\\n' '{\"node_id\":\"dev-node-fixture\",\"dns_name\":\"relay.fixture.invalid\",\"origin\":\"https://relay.fixture.invalid:8443\",\"profile\":\"macos-appstore-1.102.4-supplied-metadata\"}'\n"
        "      exit 0\n"
        "    fi ;;\n"
        "  normalize-external-origin)\n"
        "    printf 'selection|normalize-external-origin\\n' >> \"$DEV_FIXTURE_LOG\"\n"
        "    exec \"$DEV_FIXTURE_NORMALIZER\" \"$@\" ;;\n"
        "esac\n"
        "exit 97\n",
        encoding="utf-8",
    )
    positive_gate.chmod(0o700)
    bun_fixture = fixture_bin / "bun"
    bun_fixture.write_text(
        '''#!/bin/sh
if [ "$1" = install ]; then
    printf 'build|bun-install\\n' >> "$DEV_FIXTURE_LOG"
    [ "$#" = 4 ] && [ "$2" = --cwd ] && [ "$4" = --frozen-lockfile ] || exit 97
    [ -f "$3/package.json" ] && [ -f "$3/bun.lock" ] || exit 97
    [ "$NODE_ENV" = development ] || exit 97
    [ "${DEV_FIXTURE_BUN_MODE:-}" != install-fail ] || exit 73
    exit 0
fi
if [ "$1" = run ]; then
    printf 'build|bun\\n' >> "$DEV_FIXTURE_LOG"
    if [ "${DEV_FIXTURE_BUN_MODE:-}" = wait ]; then sleep 60; exit 71; fi
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
    (fixture_bin / "herdr").symlink_to(fake_herdr)
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
        "HERDR_DEV_HERDR_SOCKET": str(cli_dev_fixture / "herdr.sock"),
        "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": str(home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-coordination"),
        "HERDR_DEV_TAILSCALE_CLI_FIXTURE_LOG": str(fixture_log),
        "DEV_FIXTURE_LOG": str(fixture_log),
        "DEV_FIXTURE_NORMALIZER": str(readiness_helper), "GH_TOKEN": "fixture-only-secret",
        "PATH": f"{fixture_bin}:/usr/bin:/bin",
    })
    for name in ("HERDR_RELAY_ENV", "HERDR_PLUGIN_CONFIG_DIR", "CLOUDFLARED_BIN", "CLOUDFLARED_CONFIG",
                 "HERDR_DEV_HERDR_BIN", "HERDR_BIN"):
        positive_env.pop(name, None)
    # This EOF refusal must have its own private root: it stages a valid saved
    # selection before foreground checks stdin, so it cannot be counted as the
    # successful fresh setup below or silently reused by that invocation.
    bypass_root = base / "cli-consent-refusal"
    bypass_root.mkdir(mode=0o700)
    bypass_env = dict(positive_env, HERDR_DEV_TAILSCALE_CLI_DIR=str(bypass_root),
                      HERDR_DEV_TAILSCALE_CLI_PUBLISH="PUBLISH")
    bypass_cli = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")], env=bypass_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=10, check=False,
    )
    bypass_events = fixture_log.read_text(encoding="utf-8").splitlines() if fixture_log.exists() else []
    if (bypass_cli.returncode == 0 or
        [event.split(" --development-root ", 1)[0] for event in bypass_events if event.startswith("manager|")] !=
        ["manager|workflow discover", "manager|workflow preflight"] or
        any("--development-root " + str(bypass_root) not in event
            for event in bypass_events if event.startswith("manager|")) or
        any(event.startswith(("manager|workflow reserve ", "manager|workflow publish ",
                              "runtime|", "control|")) for event in bypass_events) or
        not (bypass_root / "relay.env").is_file() or
        any((cli_dev_root / name).exists() or (cli_dev_root / name).is_symlink()
            for name in ("relay.env", ".herdr-dev-tailscale-cli", "current")) or
        production_env.read_bytes() != production_snapshot or sentinel.exists()):
        raise AssertionError("EOF or blanket environment consent bypassed the route gate or polluted fresh setup")
    phone_link = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "setup-link"], env=positive_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False,
    )
    if (phone_link.returncode == 0 or b"Lifecycle:" not in phone_link.stderr or
        fixture_log.read_text(encoding="utf-8").splitlines() != bypass_events):
        raise AssertionError("development phone setup-link action was not refused before route effects")
    setup_cli = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh")], env=positive_env,
        cwd=root, input=(b"PUBLISH DEVELOPMENT ROUTE node=dev-node-fixture "
                        b"origin=https://relay.fixture.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n"),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=False,
    )
    if setup_cli.returncode != 0:
        raise AssertionError(f"positive CLI development setup failed: {setup_cli.stdout + setup_cli.stderr!r}")
    all_events = fixture_log.read_text(encoding="utf-8").splitlines()
    fresh_setup_events = all_events[len(bypass_events):]
    manager_events = [event for event in fresh_setup_events if event.startswith("manager|")]
    runtime_events = [event for event in fresh_setup_events if event.startswith("runtime|")]
    arm_event = next((event for event in fresh_setup_events if event.startswith("control|localcontrol admit")), "")
    bootstrap_event = next((event for event in fresh_setup_events if event.startswith("control|localcontrol arm_bootstrap")), "")
    if (len(manager_events) != 4 or not manager_events[0].startswith("manager|workflow discover ") or
        not manager_events[1].startswith("manager|workflow preflight ") or
        not manager_events[2].startswith("manager|workflow reserve ") or
        not manager_events[3].startswith("manager|workflow publish ") or
        any("--development-root " + str(cli_dev_root) not in event for event in manager_events) or
        "--scope development" not in manager_events[3] or "--node-id dev-node-fixture" not in manager_events[3] or
        "--origin https://relay.fixture.invalid:8443" not in manager_events[3] or len(runtime_events) != 1 or
        not runtime_events[0].startswith("runtime|tailscale-cli|127.0.0.1|18377|development|") or
        str(cli_dev_root / "registration") not in runtime_events[0] or
        str(home / ".local" / "state" / "herdr-mobile-relay" / "tailscale-cli-coordination") not in runtime_events[0] or
        str(cli_dev_root / "config") not in runtime_events[0] or runtime_events[0].endswith("|fixture-only-secret") or
        not arm_event or not bootstrap_event or not (fresh_setup_events.index(manager_events[0]) <
                              fresh_setup_events.index(manager_events[1]) < fresh_setup_events.index(manager_events[2]) <
                              fresh_setup_events.index(runtime_events[0]) < fresh_setup_events.index(manager_events[3]) < fresh_setup_events.index(arm_event) <
                              fresh_setup_events.index(bootstrap_event)) or
        "HERDR_RELAY_TRANSPORT='tailscale-cli'" not in (cli_dev_root / "relay.env").read_text(encoding="utf-8") or
        "HERDR_BIN='" + str(fixture_bin / "herdr") + "'" not in (cli_dev_root / "relay.env").read_text(encoding="utf-8") or
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
    if (len(update_managers) != 4 or
        not update_managers[0].startswith("manager|tailscale-cli activation-check") or
        not update_managers[1].startswith("manager|workflow assert-ready ") or
        not update_managers[2].startswith("manager|workflow preflight ") or
        not update_managers[3].startswith("manager|workflow assert-ready ") or
        any("--development-root " + str(cli_dev_root) not in event for event in update_managers[1:]) or
        any("publish" in event or "unpublish" in event for event in update_managers[1:]) or
        sum("--operation arm_bootstrap" in event for event in updated_events) != setup_bootstrap_count or
        production_env.read_bytes() != production_snapshot or sentinel.exists() or tool_sentinel.exists()):
        raise AssertionError(f"positive CLI update mutated route or production state: {updated_events!r}")

    current_pointer = cli_dev_root / "current"
    env_snapshot = (cli_dev_root / "relay.env").read_bytes()
    marker_snapshot = (cli_dev_root / ".herdr-dev-tailscale-cli").read_bytes()
    active_target = os.readlink(current_pointer)
    before_failed_preflight = fixture_log.read_text(encoding="utf-8").splitlines()
    failed_preflight = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "update"],
        env=dict(update_env, DEV_FIXTURE_PREFLIGHT_MODE="fail"), cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=30, check=False,
    )
    failed_preflight_events = fixture_log.read_text(encoding="utf-8").splitlines()[len(before_failed_preflight):]
    if (failed_preflight.returncode == 0 or os.readlink(current_pointer) != active_target or
        (cli_dev_root / "relay.env").read_bytes() != env_snapshot or
        (cli_dev_root / ".herdr-dev-tailscale-cli").read_bytes() != marker_snapshot or
        any(event.startswith(("manager|workflow publish ", "manager|workflow reserve ", "control|"))
            for event in failed_preflight_events)):
        raise AssertionError("failed complete preflight did not restore prior saved app, marker and release pointer")
    before_tamper = fixture_log.read_text(encoding="utf-8").splitlines()
    tampered_saved = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "update"],
        env=dict(update_env, DEV_FIXTURE_PREFLIGHT_MODE="tamper"), cwd=root,
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=30, check=False,
    )
    changed_saved_env = (cli_dev_root / "relay.env").read_bytes()
    pending_saved = cli_dev_root / ".setup-pending"
    if (tampered_saved.returncode == 0 or not pending_saved.is_dir() or
        b"changed.fixture.invalid" not in changed_saved_env or
        (cli_dev_root / ".herdr-dev-tailscale-cli").read_bytes() != marker_snapshot or
        any(event.startswith(("manager|workflow reserve ", "manager|workflow publish ", "control|"))
            for event in fixture_log.read_text(encoding="utf-8").splitlines()[len(before_tamper):])):
        raise AssertionError("changed saved env was overwritten or reached route effects")
    # Restore only this fixture's original bytes to allow exact pending recovery.
    (cli_dev_root / "relay.env").write_bytes(env_snapshot)
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

    def relay_env_values(path: Path) -> dict[str, str]:
        values: dict[str, str] = {}
        for line in path.read_text(encoding="utf-8").splitlines():
            key, separator, raw_value = line.partition("=")
            if separator:
                parsed = shlex.split(raw_value)
                if len(parsed) != 1:
                    raise AssertionError(f"invalid fixture environment value for {key}")
                values[key] = parsed[0]
        return values

    cli_dev_env_file = cli_dev_root / "relay.env"
    env_before_migration = relay_env_values(cli_dev_env_file)
    migrated_socket = env_before_migration["HERDR_RELAY_PAIRING_SOCKET"]
    legacy_socket = cli_dev_root / "config" / "pairing-control.sock"
    if legacy_socket.exists() or legacy_socket.is_symlink():
        raise AssertionError("positive fixture unexpectedly has an old pairing socket")
    original_lines = cli_dev_env_file.read_text(encoding="utf-8").splitlines()
    old_socket_value = f"HERDR_RELAY_PAIRING_SOCKET='{legacy_socket}'"
    if sum(line.startswith("HERDR_RELAY_PAIRING_SOCKET=") for line in original_lines) != 1:
        raise AssertionError("positive fixture has no unique pairing socket setting")
    cli_dev_env_file.write_text(
        "\n".join(old_socket_value if line.startswith("HERDR_RELAY_PAIRING_SOCKET=") else line
                  for line in original_lines) + "\n",
        encoding="utf-8",
    )
    legacy_env_values = relay_env_values(cli_dev_env_file)
    migration_setup_env = dict(
        update_env, HERDR_DEV_TAILSCALE_CLI_RELAY_BIN=str(positive_gate),
    )
    legacy_bytes = cli_dev_env_file.read_bytes()
    legacy_pointer = os.readlink(current_pointer)
    for failure_settings in ({"DEV_FIXTURE_BUN_MODE": "missing-version"},
                             {"DEV_FIXTURE_PREFLIGHT_MODE": "fail"}):
        migration_failure = subprocess.run(
            [str(root / "relay" / "dev-tailscale-cli.sh"), "setup"],
            env=dict(migration_setup_env, **failure_settings), cwd=root,
            input=(b"PUBLISH DEVELOPMENT ROUTE node=dev-node-fixture "
                   b"origin=https://relay.fixture.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n"),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=False,
        )
        if (migration_failure.returncode == 0 or cli_dev_env_file.read_bytes() != legacy_bytes or
            os.readlink(current_pointer) != legacy_pointer or
            (cli_dev_root / ".setup-pending").exists()):
            raise AssertionError("failed staging or full preflight changed a saved legacy socket/app choice")
    setup_events_before_migration = fixture_log.read_text(encoding="utf-8").splitlines()
    migration_setup = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "setup"], env=migration_setup_env,
        cwd=root,
        input=(b"PUBLISH DEVELOPMENT ROUTE node=dev-node-fixture "
               b"origin=https://relay.fixture.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n"),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, check=False,
    )
    migrated_values = relay_env_values(cli_dev_env_file)
    setup_events = fixture_log.read_text(encoding="utf-8").splitlines()[len(setup_events_before_migration):]
    if (migration_setup.returncode != 0 or
        b"Moved this root's stopped pairing-control socket" not in migration_setup.stdout or
        migrated_values.get("HERDR_RELAY_PAIRING_SOCKET") != migrated_socket or
        {key: value for key, value in migrated_values.items() if key != "HERDR_RELAY_PAIRING_SOCKET"} !=
        {key: value for key, value in legacy_env_values.items() if key != "HERDR_RELAY_PAIRING_SOCKET"} or
        not any(event.startswith("manager|workflow reserve ") for event in setup_events) or
        not any(event.startswith("manager|workflow publish ") for event in setup_events)):
        raise AssertionError(
            "marked stopped legacy root did not migrate only the pairing socket and continue setup: "
            f"status={migration_setup.returncode} output={migration_setup.stdout + migration_setup.stderr!r} "
            f"events={setup_events!r}"
        )
    second_migration_update = subprocess.run(
        [str(root / "relay" / "dev-tailscale-cli.sh"), "update"], env=update_env,
        cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=30, check=False,
    )
    if (second_migration_update.returncode != 0 or
        b"Moved this root's stopped pairing-control socket" in second_migration_update.stdout or
        relay_env_values(cli_dev_env_file).get("HERDR_RELAY_PAIRING_SOCKET") != migrated_socket):
        raise AssertionError("pairing socket migration was not one-time and idempotent")

    def make_legacy_migration_root(name: str, configured_socket: str, marked: bool,
                                   old_path_kind: str = "absent") -> tuple[Path, Path, Path]:
        migration_root = base / name
        migration_root.mkdir(mode=0o700)
        config_dir = migration_root / "config"
        config_dir.mkdir(mode=0o700)
        migration_env_file = migration_root / "relay.env"
        socket_setting = f"HERDR_RELAY_PAIRING_SOCKET='{configured_socket}'\n"
        if old_path_kind == "hostile":
            socket_setting += 'touch "$MIGRATION_EXECUTION_SENTINEL"\n'
        migration_env_file.write_text(
            socket_setting * (2 if old_path_kind == "duplicate" else 1), encoding="utf-8",
        )
        migration_env_file.chmod(0o600)
        marker = migration_root / ".herdr-dev-tailscale-cli"
        expected_marker = (
            "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n"
            f"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT={migration_root}/registration\n"
            f"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT={home}/.local/state/herdr-mobile-relay/tailscale-cli-coordination\n"
        )
        if marked:
            marker.write_text(expected_marker, encoding="utf-8")
            marker.chmod(0o600)
        old_path = config_dir / "pairing-control.sock"
        if old_path_kind == "socket":
            old_listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            old_listener.bind(str(old_path))
            old_listener.close()
        elif old_path_kind == "symlink":
            old_target = migration_root / "old-socket-target"
            old_target.write_text("not a socket\n", encoding="utf-8")
            old_path.symlink_to(old_target)
        return migration_root, migration_env_file, old_path

    legacy_mismatch = str(base / "unrelated-old-pairing.sock")
    blocked_migrations = (
        ("legacy_socket_present_is_refused", "ls", str(base / "ls" / "config" / "pairing-control.sock"), True, "socket"),
        ("legacy_symlink_present_is_refused", "ly", str(base / "ly" / "config" / "pairing-control.sock"), True, "symlink"),
        ("different_nonlegacy_socket_is_refused", "dn", legacy_mismatch, True, "absent"),
        ("unmarked_hostile_legacy_state_is_rejected_without_execution", "um", str(base / "um" / "config" / "pairing-control.sock"), False, "hostile"),
        ("duplicate_legacy_socket_settings_are_refused", "dp", str(base / "dp" / "config" / "pairing-control.sock"), True, "duplicate"),
    )
    for name, root_name, configured_socket, marked, old_path_kind in blocked_migrations:
        migration_root, migration_env_file, old_path = make_legacy_migration_root(
            root_name, configured_socket, marked, old_path_kind,
        )
        env_snapshot = migration_env_file.read_bytes()
        marker_path = migration_root / ".herdr-dev-tailscale-cli"
        marker_snapshot = marker_path.read_bytes() if marker_path.exists() else None
        migration_sentinel = base / "migration-env-executed"
        mismatch_env = dict(
            update_env, HERDR_DEV_TAILSCALE_CLI_DIR=str(migration_root),
            MIGRATION_EXECUTION_SENTINEL=str(migration_sentinel),
        )
        mismatch_result = subprocess.run(
            [str(root / "relay" / "dev-tailscale-cli.sh"), "update"], env=mismatch_env,
            cwd=root, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=5, check=False,
        )
        marker_after = marker_path.read_bytes() if marker_path.exists() else None
        expected_refusal = (
            b"not a marked private CLI root" if old_path_kind == "hostile" else
            b"Existing development state records a different pairing socket"
        )
        if (mismatch_result.returncode == 0 or
            expected_refusal not in mismatch_result.stdout + mismatch_result.stderr or
            migration_env_file.read_bytes() != env_snapshot or marker_after != marker_snapshot or
            migration_sentinel.exists() or
            (old_path_kind == "socket" and not old_path.is_socket()) or
            (old_path_kind == "symlink" and not old_path.is_symlink())):
            raise AssertionError(f"{name}: unsafe migration or non-fail-closed outcome")
    # Exercise the byte-identical launcher in private checkout fixtures so its
    # default root can start absent. Nothing here qualifies TLS, a real route or
    # a phone: only the strict origin helper is real; builds/runtime are inert.
    selection_base = base / "origin-selection"
    selection_base.mkdir(mode=0o700)
    selection_tools = selection_base / "tools"
    selection_tools.mkdir(mode=0o700)
    selection_git = selection_tools / "git"
    selection_git.write_text(
        '#!/bin/sh\n[ "$#" = 4 ] && [ "$1" = -C ] && '
        '[ "$2" = "$DEV_FIXTURE_CHECKOUT" ] && [ "$3" = rev-parse ] && '
        '[ "$4" = HEAD ] || exit 97\n'
        "printf '%s\\n' 1111111111111111111111111111111111111111\n",
        encoding="utf-8",
    )
    selection_git.chmod(0o700)
    discovery_boundary = b"Go-owned read-only discovery selected node"
    route_confirmation = (b"PUBLISH DEVELOPMENT ROUTE node=dev-node-fixture "
                          b"origin=https://relay.fixture.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n")

    def selection_checkout(case: str) -> Path:
        checkout = selection_base / case
        (checkout / "relay").mkdir(mode=0o700, parents=True)
        (checkout / "frontend").mkdir(mode=0o700)
        for name in ("dev-tailscale-cli.sh", "common.sh"):
            shutil.copy2(root / "relay" / name, checkout / "relay" / name)
        shutil.copy2(root / "herdr-plugin.toml", checkout / "herdr-plugin.toml")
        for name in ("package.json", "bun.lock"):
            shutil.copy2(root / "frontend" / name, checkout / "frontend" / name)
        return checkout

    def snapshot_tree(directory: Path) -> dict | None:
        if not directory.exists():
            return None
        return {str(path.relative_to(directory)): (
                    path.stat().st_mode & 0o777,
                    os.readlink(path) if path.is_symlink() else
                    path.read_bytes() if path.is_file() else None)
                for path in (directory, *sorted(directory.rglob("*")))}

    selection_production_before = snapshot_tree(production)

    # Use an exec'd stdlib shim, not preexec_fn: terminal setup runs in a
    # fresh interpreter, without Python hooks using inherited fork-time locks.
    # setsid alone does not make an already-open PTY the controlling terminal.
    selection_terminal_exec = '''
import fcntl
import os
import sys
import termios

pid = os.getpid()
if os.getsid(0) != pid or os.getpgrp() != pid:
    raise SystemExit("origin fixture requires its own session and process group")
fcntl.ioctl(0, termios.TIOCSCTTY, 0)
os.tcsetpgrp(0, pid)
if os.tcgetpgrp(0) != pid:
    raise SystemExit("origin fixture requires foreground terminal ownership")
os.execv(sys.argv[1], sys.argv[1:])
'''

    def run_selection(checkout: Path, answers: bytes | None = None, *, terminal: bool = True,
                      overrides: dict[str, str] | None = None, action: str = "setup",
                      cancel_signal: int | None = None,
                      expect_discovery: bool = False) -> tuple[int, bytes, list[str]]:
        if (expect_discovery and not terminal) or (cancel_signal is not None and not expect_discovery):
            raise AssertionError("discovery cancellation requires a foreground controlling terminal")
        record = checkout / "events"
        before = record.read_text(encoding="utf-8").splitlines() if record.exists() else []
        settings = dict(positive_env, HERDR_DEV_TAILSCALE_CLI_DIR="",
                        HERDR_DEV_PHONE_APP_URL="", HERDR_PHONE_APP_URL="",
                        DEV_FIXTURE_CHECKOUT=str(checkout), DEV_FIXTURE_LOG=str(record),
                        PATH=f"{selection_tools}:{fixture_bin}:/usr/bin:/bin")
        # A truly fresh default supplies neither node nor domain; discovery
        # must derive both. Only explicit mismatch/precedence cases restore them.
        for name in ("HERDR_DEV_TAILSCALE_CLI_ORIGIN", "HERDR_TAILSCALE_CLI_ORIGIN",
                     "HERDR_DEV_TAILSCALE_CLI_NODE_ID", "HERDR_TAILSCALE_CLI_NODE_ID"):
            settings.pop(name, None)
        settings.update(overrides or {})
        master, slave = None, None
        process = None
        failure = None
        cleanup_deadline = None
        timeout_receipt = None
        write_actual = write_expected = "unavailable"
        cleanup_outcomes = {stage: {"outcome": "not_needed", "errno": None} for stage in (
            "group_signal", "child_signal", "pipe_drain", "child_reap",
        )}
        prefix = b""
        deadline = time.monotonic() + 30
        command = [str(checkout / "relay" / "dev-tailscale-cli.sh"), action]
        if terminal:
            command = [sys.executable, "-I", "-S", "-c", selection_terminal_exec, *command]
        try:
            if terminal:
                master, slave = pty.openpty()
                # This PTY supplies input only; unused echo can hold Darwin terminal-exit draining.
                attrs = termios.tcgetattr(slave)
                attrs[3] &= ~(termios.ECHO | termios.ECHONL)
                termios.tcsetattr(slave, termios.TCSANOW, attrs)
            process = subprocess.Popen(
                command, env=settings, cwd=checkout, stdin=slave if terminal else subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
            )
            if slave is not None:
                os.close(slave)
                slave = None
            # Cancellation waits for the new read-only discovery boundary;
            # saved and non-setup cases do not cross this boundary.
            if expect_discovery:
                prompt_deadline = min(deadline, time.monotonic() + 10)
                while discovery_boundary not in prefix:
                    remaining = prompt_deadline - time.monotonic()
                    if remaining <= 0 or not select.select([process.stdout], [], [], remaining)[0]:
                        raise AssertionError("origin selection fixture never reached discovery")
                    chunk = os.read(process.stdout.fileno(), 4096)
                    if not chunk:
                        raise AssertionError("origin selection fixture exited before discovery")
                    prefix += chunk
            if cancel_signal is not None:
                # start_new_session creates this exact child-owned group; the
                # shim exec preserves its PID. Never use a discovered PGID.
                os.killpg(process.pid, cancel_signal)
            elif terminal:
                write_expected = len(answers if answers is not None else b"\x04")
                write_actual = os.write(master, answers if answers is not None else b"\x04")
            stdout, stderr = process.communicate(
                input=None if terminal else answers, timeout=max(0, deadline - time.monotonic()),
            )
            events = record.read_text(encoding="utf-8").splitlines() if record.exists() else []
            if snapshot_tree(production) != selection_production_before or sentinel.exists() or tool_sentinel.exists():
                raise AssertionError("origin selection changed production baseline or invoked an external tool")
            return process.returncode, stdout + prefix + stderr, events[len(before):]
        except BaseException as error:
            failure = error
            if isinstance(error, subprocess.TimeoutExpired):
                # All observation consumes the existing cleanup budget. Keep
                # most of it for the original signal/drain/reap operations.
                cleanup_deadline = time.monotonic() + 5
                observation_deadline = min(cleanup_deadline, time.monotonic() + 1)
                # A wait-only timeout may omit stderr even after draining it.
                # Missing evidence cannot prove that the prompt was absent.
                prompt_reached = "inconclusive"
                if discovery_boundary in prefix:
                    prompt_reached = True
                elif error.stdout is not None:
                    prompt_reached = discovery_boundary in prefix + error.stdout
                timeout_receipt = {
                    "schema": 1, "observation_outcome": "ok",
                    "prompt_reached": prompt_reached,
                    "normalizer_marker": "inconclusive", "events_outcome": "unavailable",
                    "pty_write_actual": write_actual, "pty_write_expected": write_expected,
                    "streams_closed_before_cleanup": {
                        name: stream.closed if stream is not None else "unavailable"
                        for name, stream in (("stdin", process.stdin), ("stdout", process.stdout),
                                             ("stderr", process.stderr))
                    },
                    "ps_outcome": "unavailable", "ps_errno": None,
                    "state": "unavailable", "parent_matches": "unavailable",
                    "real_uid_matches": "unavailable", "effective_uid_matches": "unavailable",
                    "pgid": {"outcome": "unavailable", "errno": None, "matches_birth_pid": "unavailable"},
                    "sid": {"outcome": "unavailable", "errno": None, "matches_birth_pid": "unavailable"},
                    "observer_signal": {"outcome": "not_needed", "errno": None},
                    "observer_pipe_close": {"outcome": "not_needed", "errno": None},
                    "observer_reap": {"outcome": "not_started", "errno": None},
                    "cleanup": cleanup_outcomes,
                }
                observer = None
                try:
                    try:
                        # No poll/wait of the fixture child before this snapshot:
                        # its unreaped birth PID pins every query's exact target.
                        if process.returncode is None:
                            for name, query in (("pgid", os.getpgid), ("sid", os.getsid)):
                                try:
                                    timeout_receipt[name].update(
                                        outcome="ok", matches_birth_pid=query(process.pid) == process.pid,
                                    )
                                except OSError as probe_error:
                                    timeout_receipt[name].update(outcome="os_error", errno=probe_error.errno)
                            if time.monotonic() < observation_deadline - 0.25:
                                # Fixed numeric fields only; never read commands,
                                # args or environment, and never discover tools.
                                observer = subprocess.Popen(
                                    ["/bin/ps", "-p", str(process.pid), "-o", "stat=",
                                     "-o", "ppid=", "-o", "ruid=", "-o", "uid="],
                                    env={"PATH": "/usr/bin:/bin", "LC_ALL": "C", "LANG": "C"},
                                    cwd="/", stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, start_new_session=True,
                                )
                                timeout_receipt["observer_reap"]["outcome"] = "unavailable"
                                metadata, _ = observer.communicate(
                                    timeout=max(0, observation_deadline - time.monotonic() - 0.25),
                                )
                                timeout_receipt["ps_outcome"] = "unavailable"
                                if observer.returncode == 0 and len(metadata) <= 256:
                                    fields = metadata.decode("ascii").split()
                                    if len(fields) == 4 and all(value.isdecimal() for value in fields[1:]):
                                        timeout_receipt.update(
                                            ps_outcome="ok",
                                            state={"R": "running", "S": "sleeping", "I": "idle",
                                                   "D": "uninterruptible", "U": "uninterruptible",
                                                   "T": "stopped", "Z": "zombie", "X": "dead",
                                                   "W": "paging"}.get(fields[0][:1], "unavailable"),
                                            parent_matches=int(fields[1]) == os.getpid(),
                                            real_uid_matches=int(fields[2]) == os.getuid(),
                                            effective_uid_matches=int(fields[3]) == os.geteuid(),
                                        )
                            else:
                                timeout_receipt["ps_outcome"] = "budget_exhausted"
                        else:
                            timeout_receipt["ps_outcome"] = "already_reaped"
                    except subprocess.TimeoutExpired:
                        timeout_receipt["ps_outcome"] = "timeout"
                    except OSError as probe_error:
                        timeout_receipt.update(ps_outcome="os_error", ps_errno=probe_error.errno)
                    finally:
                        # ps is a separately owned, fixed system executable, not
                        # project runtime. Bound its retirement to the SAME
                        # one-second observation slice, with no unbounded run().
                        if observer is not None:
                            if observer.returncode is None:
                                try:
                                    observer.kill()
                                    timeout_receipt["observer_signal"]["outcome"] = "ok"
                                except ProcessLookupError as probe_error:
                                    timeout_receipt["observer_signal"].update(
                                        outcome="no_process", errno=probe_error.errno,
                                    )
                                except OSError as probe_error:
                                    timeout_receipt["observer_signal"].update(
                                        outcome="os_error", errno=probe_error.errno,
                                    )
                            if observer.stdout is not None:
                                try:
                                    observer.stdout.close()
                                    timeout_receipt["observer_pipe_close"]["outcome"] = "ok"
                                except OSError as probe_error:
                                    timeout_receipt["observer_pipe_close"].update(
                                        outcome="os_error", errno=probe_error.errno,
                                    )
                            try:
                                observer.wait(timeout=max(0, observation_deadline - time.monotonic()))
                                timeout_receipt["observer_reap"]["outcome"] = "ok"
                            except subprocess.TimeoutExpired:
                                timeout_receipt["observer_reap"]["outcome"] = "timeout"
                            except OSError as probe_error:
                                timeout_receipt["observer_reap"].update(
                                    outcome="os_error", errno=probe_error.errno,
                                )
                    if time.monotonic() < observation_deadline:
                        # Only this checkout's known private event file. A
                        # bounded complete append delta can prove absence; a
                        # missing/partial/oversized/changed read cannot.
                        descriptor = os.open(record, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
                        try:
                            initial = os.fstat(descriptor)
                            event_data = (os.read(descriptor, 16 * 1024 + 1)
                                          if stat.S_ISREG(initial.st_mode) else None)
                            final = os.fstat(descriptor)
                        finally:
                            os.close(descriptor)
                        if (event_data is not None and len(event_data) <= 16 * 1024 and
                                len(event_data) == initial.st_size == final.st_size and
                                initial.st_mtime_ns == final.st_mtime_ns and
                                (not event_data or event_data.endswith(b"\n"))):
                            event_lines = event_data.decode("utf-8").split("\n", 129)[:-1]
                            if len(event_lines) <= 128 and event_lines[:len(before)] == before:
                                timeout_receipt.update(
                                    events_outcome="complete",
                                    normalizer_marker=(
                                        "selection|normalize-external-origin" in event_lines[len(before):]
                                    ),
                                )
                        if timeout_receipt["events_outcome"] != "complete":
                            timeout_receipt["events_outcome"] = "inconclusive"
                    else:
                        timeout_receipt["events_outcome"] = "budget_exhausted"
                except BaseException:
                    # Diagnostics must never replace the primary timeout or
                    # prevent the original fixture cleanup below.
                    timeout_receipt["observation_outcome"] = "unavailable"
            raise
        finally:
            cleanup_problems = []
            if process is not None:
                if cleanup_deadline is None:
                    cleanup_deadline = time.monotonic() + 5
                # Do not poll/reap first: the unreaped child pins its PID even
                # if it exited while a descendant still holds a captured pipe.
                if process.returncode is None:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                        cleanup_outcomes["group_signal"]["outcome"] = "ok"
                    except ProcessLookupError as cleanup_error:
                        # No group remains; still drain and reap below.
                        cleanup_outcomes["group_signal"].update(outcome="no_process", errno=cleanup_error.errno)
                    except OSError as cleanup_error:
                        cleanup_outcomes["group_signal"].update(outcome="os_error", errno=cleanup_error.errno)
                        cleanup_problems.append("group-signal")
                        # Popen still owns this unreaped direct child. This
                        # fallback cannot prove group retirement and never
                        # turns a refused group signal into a passing cleanup.
                        try:
                            process.kill()
                            cleanup_outcomes["child_signal"]["outcome"] = "ok"
                        except ProcessLookupError as cleanup_error:
                            cleanup_outcomes["child_signal"].update(outcome="no_process", errno=cleanup_error.errno)
                        except OSError as cleanup_error:
                            cleanup_outcomes["child_signal"].update(outcome="os_error", errno=cleanup_error.errno)
                            cleanup_problems.append("child-signal")
                    try:
                        process.communicate(timeout=max(0, cleanup_deadline - time.monotonic()))
                        cleanup_outcomes["pipe_drain"]["outcome"] = "ok"
                    except subprocess.TimeoutExpired:
                        cleanup_outcomes["pipe_drain"]["outcome"] = "timeout"
                        cleanup_problems.append("pipe-drain")
                    except OSError as cleanup_error:
                        cleanup_outcomes["pipe_drain"].update(outcome="os_error", errno=cleanup_error.errno)
                        cleanup_problems.append("pipe-drain")
                # POSIX communicate uses no reader threads. Close every owned
                # pipe even if draining failed, then separately reap the child
                # within the SAME cleanup budget, not another five seconds.
                for stream in (process.stdin, process.stdout, process.stderr):
                    if stream is not None:
                        try:
                            stream.close()
                        except OSError:
                            cleanup_problems.append("pipe-close")
                if process.returncode is None:
                    try:
                        process.wait(timeout=max(0, cleanup_deadline - time.monotonic()))
                        cleanup_outcomes["child_reap"]["outcome"] = "ok"
                    except subprocess.TimeoutExpired:
                        cleanup_outcomes["child_reap"]["outcome"] = "timeout"
                        cleanup_problems.append("child-reap")
                    except OSError as cleanup_error:
                        cleanup_outcomes["child_reap"].update(outcome="os_error", errno=cleanup_error.errno)
                        cleanup_problems.append("child-reap")
                else:
                    cleanup_outcomes["child_reap"]["outcome"] = "already_reaped"
            if timeout_receipt is not None:
                if timeout_receipt["observer_signal"]["outcome"] == "os_error":
                    cleanup_problems.append("observer-signal")
                if timeout_receipt["observer_pipe_close"]["outcome"] == "os_error":
                    cleanup_problems.append("observer-pipe-close")
                if timeout_receipt["observer_reap"]["outcome"] not in ("not_started", "ok"):
                    cleanup_problems.append("observer-reap")
            for descriptor in (slave, master):
                if descriptor is not None:
                    try:
                        os.close(descriptor)
                    except OSError:
                        cleanup_problems.append("pty-close")
            if cleanup_problems:
                message = "origin selection cleanup incomplete: " + ",".join(cleanup_problems)
                if failure is None:
                    raise AssertionError(message)
                # Preserve the initial timeout/prompt/signal failure; print only
                # fixed stages, never environment or arbitrary captured output.
                try:
                    print(message + "; original failure retained", file=sys.stderr)
                except BaseException:
                    pass  # Reporting must also preserve the original failure.
            if timeout_receipt is not None:
                timeout_receipt["cleanup_problems"] = cleanup_problems
                try:
                    # Only allowlisted labels, booleans, counts and numeric
                    # errno; no event lines, IDs, output, paths or exceptions.
                    print("origin selection timeout receipt: " +
                          json.dumps(timeout_receipt, separators=(",", ":")), file=sys.stderr)
                except BaseException:
                    pass  # A reporting failure cannot replace the timeout.

    def assert_no_selection_effects(checkout: Path, events: list[str]) -> None:
        state = checkout / "relay" / ".dev-tailscale-cli"
        if (any((state / name).exists() or (state / name).is_symlink()
                for name in ("relay.env", ".herdr-dev-tailscale-cli", "current")) or
            any(event.startswith(("manager|workflow reserve ", "manager|workflow publish ",
                                  "runtime|", "control|")) for event in events)):
            raise AssertionError(f"refused discovery selected live state or entered runtime: {events!r}")

    for case, terminal in (("terminal-default", True), ("unattended-default", False)):
        checkout = selection_checkout(case)
        if (checkout / "frontend" / "node_modules").exists():
            raise AssertionError("fresh setup fixture unexpectedly has frontend dependencies")
        status, output, events = run_selection(
            checkout, route_confirmation, terminal=terminal, overrides={"NODE_ENV": "production"},
        )
        if ("build|bun-install" not in events or "build|bun" not in events or
            events.index("build|bun-install") >= events.index("build|bun")):
            raise AssertionError("fresh setup built without first installing locked development dependencies")
        selected_env = checkout / "relay" / ".dev-tailscale-cli" / "relay.env"
        selected = relay_env_values(selected_env)
        if (status != 0 or discovery_boundary not in output or
            selected.get("HERDR_PHONE_APP_URL") != "https://relay.fixture.invalid:8443" or
            selected.get("HERDR_TAILSCALE_CLI_ORIGIN") != "https://relay.fixture.invalid:8443" or
            selected.get("HERDR_TAILSCALE_CLI_NODE_ID") != "dev-node-fixture" or
            not any(event.startswith("manager|workflow discover ") for event in events) or
            not any(event.startswith("manager|workflow preflight ") for event in events) or
            "control|localcontrol arm_bootstrap" not in events):
            raise AssertionError(f"{case}: automatic canonical app selection failed: {output!r} {events!r}")
        if terminal:
            saved_checkout = checkout
            saved_origin = "https://relay.fixture.invalid:8443"

    checkout = selection_checkout("configured-identity-match")
    status, output, events = run_selection(
        checkout, route_confirmation, terminal=False,
        overrides={"HERDR_DEV_TAILSCALE_CLI_ORIGIN": "https://relay.fixture.invalid:8443",
                   "HERDR_DEV_TAILSCALE_CLI_NODE_ID": "dev-node-fixture"},
    )
    if (status != 0 or discovery_boundary not in output or
        relay_env_values(checkout / "relay" / ".dev-tailscale-cli" / "relay.env").get("HERDR_PHONE_APP_URL") !=
        "https://relay.fixture.invalid:8443" or
        not any(event.startswith("manager|workflow publish ") for event in events)):
        raise AssertionError(f"configured matching identity lost live discovery: {output!r} {events!r}")

    for case, selected, overrides in (
        ("preselected-dev-precedence", "https://app.fixture.invalid:8443",
         {"HERDR_DEV_PHONE_APP_URL": "https://app.fixture.invalid:8443",
          "HERDR_PHONE_APP_URL": "https://other.fixture.invalid"}),
        ("preselected-standard", "https://app.fixture.invalid",
         {"HERDR_PHONE_APP_URL": "https://app.fixture.invalid"}),
    ):
        checkout = selection_checkout(case)
        status, output, events = run_selection(checkout, route_confirmation, terminal=False, overrides=overrides)
        selected_env = checkout / "relay" / ".dev-tailscale-cli" / "relay.env"
        if (status != 0 or relay_env_values(selected_env).get("HERDR_PHONE_APP_URL") != selected or
            not any(event.startswith("manager|workflow publish ") for event in events)):
            raise AssertionError(f"{case}: explicit app choice lost: {output!r} {events!r}")

    for case, overrides in (
        ("invalid-app", {"HERDR_DEV_PHONE_APP_URL": "https://app.fixture.invalid/path"}),
        ("invalid-origin", {"HERDR_DEV_TAILSCALE_CLI_ORIGIN": "http://relay.fixture.invalid"}),
        ("wrong-node", {"HERDR_DEV_TAILSCALE_CLI_NODE_ID": "other-node"}),
        ("unsupported", {"DEV_FIXTURE_DISCOVERY_MODE": "unsupported"}),
        ("failed", {"DEV_FIXTURE_DISCOVERY_MODE": "fail"}),
        ("malformed", {"DEV_FIXTURE_DISCOVERY_MODE": "malformed"}),
        ("configured-discovery-drift", {"DEV_FIXTURE_DISCOVERY_MODE": "drift",
                                        "HERDR_DEV_TAILSCALE_CLI_ORIGIN": "https://relay.fixture.invalid:8443",
                                        "HERDR_DEV_TAILSCALE_CLI_NODE_ID": "dev-node-fixture"}),
    ):
        checkout = selection_checkout(case)
        status, output, events = run_selection(checkout, route_confirmation, terminal=False, overrides=overrides)
        if status == 0 or any(event.startswith("build|") for event in events):
            raise AssertionError(f"{case}: refused discovery built or succeeded: {output!r} {events!r}")
        assert_no_selection_effects(checkout, events)

    for cancel_signal, expected_status in ((signal.SIGINT, 130), (signal.SIGTERM, 143)):
        checkout = selection_checkout(f"signal-{cancel_signal}")
        status, output, events = run_selection(
            checkout, cancel_signal=cancel_signal, expect_discovery=True,
            overrides={"DEV_FIXTURE_BUN_MODE": "wait"},
        )
        if status != expected_status or discovery_boundary not in output:
            raise AssertionError(f"signal did not cancel after discovery: {status} {output!r} {events!r}")
        assert_no_selection_effects(checkout, events)

    status, output, events = run_selection(saved_checkout, route_confirmation)
    saved_root = saved_checkout / "relay" / ".dev-tailscale-cli"
    if (status != 0 or discovery_boundary in output or
        relay_env_values(saved_root / "relay.env").get("HERDR_PHONE_APP_URL") != saved_origin):
        raise AssertionError("saved app origin was discovered again or replaced")
    saved_before = snapshot_tree(saved_root)
    status, output, events = run_selection(
        saved_checkout, route_confirmation,
        overrides={"HERDR_DEV_PHONE_APP_URL": "https://different.fixture.invalid"},
    )
    if (status == 0 or discovery_boundary in output or
        any(event.startswith(("build|", "manager|", "runtime|", "control|")) for event in events) or
        snapshot_tree(saved_root) != saved_before or
        b"Existing CLI profile/origin changed; registration requires explicit recovery" not in output):
        raise AssertionError(f"persisted origin mismatch did not retain state: {output!r} {events!r}")
    saved_file = saved_root / "relay.env"
    saved_bytes = saved_file.read_bytes()
    try:
        saved_file.write_bytes(b"\n".join(line for line in saved_bytes.split(b"\n")
                                          if not line.startswith(b"HERDR_PHONE_APP_URL=")))
        incomplete_before = snapshot_tree(saved_root)
        status, output, events = run_selection(saved_checkout, route_confirmation)
        if (status == 0 or discovery_boundary in output or events or snapshot_tree(saved_root) != incomplete_before):
            raise AssertionError("incomplete saved app choice was rebound or changed")
    finally:
        saved_file.write_bytes(saved_bytes)
    saved_marker = saved_root / ".herdr-dev-tailscale-cli"
    saved_marker_bytes = saved_marker.read_bytes()
    try:
        saved_marker.write_bytes(saved_marker_bytes + b"\n")
        changed_before = snapshot_tree(saved_root)
        status, output, events = run_selection(saved_checkout, route_confirmation)
        if status == 0 or events or snapshot_tree(saved_root) != changed_before:
            raise AssertionError("non-exact saved marker was read as a valid binding or changed")
    finally:
        saved_marker.write_bytes(saved_marker_bytes)

    # Automatic app discovery never substitutes for independent exact route
    # confirmation. EOF, generic yes and wrong exact identity all refuse.
    for case, confirmation in (("discovery-only-eof", b""),
                               ("discovery-is-not-consent", b"y\n"),
                               ("wrong-exact-route", route_confirmation.replace(b"dev-node-fixture", b"other-node"))):
        checkout = selection_checkout(case)
        status, output, events = run_selection(checkout, confirmation, terminal=False)
        if (status == 0 or discovery_boundary not in output or
            not any(event.startswith("manager|workflow preflight ") for event in events) or
            any(event.startswith(("manager|workflow reserve ", "manager|workflow publish ",
                                  "runtime|", "control|")) for event in events)):
            raise AssertionError(f"discovery replaced exact route consent: {output!r} {events!r}")

    checkout = selection_checkout("dependency-install-failure-retry")
    status, output, events = run_selection(
        checkout, route_confirmation, terminal=False, overrides={"DEV_FIXTURE_BUN_MODE": "install-fail"},
    )
    if (status != 73 or "build|bun-install" not in events or "build|bun" in events or
        b"Frontend dependency installation failed" not in output):
        raise AssertionError("failed dependency installation did not stop before the frontend build")
    assert_no_selection_effects(checkout, events)
    status, output, events = run_selection(checkout, route_confirmation, terminal=False)
    if status != 0 or not any(event.startswith("manager|workflow publish ") for event in events):
        raise AssertionError("fresh retry after dependency installation failure did not succeed")
    installed_root = checkout / "relay" / ".dev-tailscale-cli"
    installed_before = snapshot_tree(installed_root)
    status, output, events = run_selection(
        checkout, action="update", terminal=False,
        overrides={"DEV_FIXTURE_BUN_MODE": "install-fail",
                   "HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(installed_root / "current" / "bin" / "herdr-mobile-relay")},
    )
    if (status != 73 or "build|bun-install" not in events or "build|bun" in events or
        snapshot_tree(installed_root) != installed_before):
        raise AssertionError("failed update dependency installation changed the saved development release")

    # A failed first build cannot publish a marker or broken env; retry with
    # the same checkout and no supplied app origin must discover again.
    checkout = selection_checkout("build-failure-retry")
    status, output, events = run_selection(
        checkout, route_confirmation, terminal=False, overrides={"DEV_FIXTURE_BUN_MODE": "missing-version"},
    )
    if status == 0 or not any(event == "build|bun" for event in events):
        raise AssertionError("fresh build failure did not exercise the staging seam")
    assert_no_selection_effects(checkout, events)
    status, output, events = run_selection(checkout, route_confirmation, terminal=False)
    if status != 0 or not any(event.startswith("manager|workflow publish ") for event in events):
        raise AssertionError(f"fresh retry did not discover and publish after safe staging: {output!r}")
    retry_root = checkout / "relay" / ".dev-tailscale-cli"
    retry_env_before = (retry_root / "relay.env").read_bytes()
    status, output, events = run_selection(
        checkout, action="update", terminal=False,
        overrides={"HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(retry_root / "current" / "bin" / "herdr-mobile-relay"),
                   "DEV_FIXTURE_PREFLIGHT_MODE": "drop-old-env"},
    )
    pending = retry_root / ".setup-pending"
    if (status == 0 or not pending.is_dir() or
        (pending / "old-existence").read_bytes() != b"pointer=1\nenv=1\nmarker=1\n" or
        (pending / "old-env").exists() or (retry_root / "relay.env").read_bytes() != retry_env_before):
        raise AssertionError("missing recorded old-env snapshot was treated as an absent saved choice")
    status, output, events = run_selection(checkout, route_confirmation, terminal=False)
    if status == 0 or events or (retry_root / "relay.env").read_bytes() != retry_env_before:
        raise AssertionError("retry repaired or discarded a missing old-env snapshot")

    for case, overrides in (
        ("full-preflight-failed", {"DEV_FIXTURE_PREFLIGHT_MODE": "fail"}),
        ("full-preflight-drift", {"DEV_FIXTURE_PREFLIGHT_MODE": "drift"}),
    ):
        checkout = selection_checkout(case)
        status, output, events = run_selection(checkout, route_confirmation, terminal=False, overrides=overrides)
        if status == 0 or not any(event.startswith("manager|workflow preflight ") for event in events):
            raise AssertionError(f"{case}: did not reach full-layout verification")
        assert_no_selection_effects(checkout, events)

    checkout = selection_checkout("changed-candidate-refusal")
    status, output, events = run_selection(
        checkout, route_confirmation, terminal=False, overrides={"DEV_FIXTURE_PREFLIGHT_MODE": "tamper"},
    )
    pending = checkout / "relay" / ".dev-tailscale-cli" / ".setup-pending"
    altered_env = checkout / "relay" / ".dev-tailscale-cli" / "relay.env"
    if status == 0 or not pending.is_dir() or b"changed.fixture.invalid" not in altered_env.read_bytes():
        raise AssertionError("unknown candidate bytes were silently rolled back")
    altered_before = altered_env.read_bytes()
    status, output, retry_events = run_selection(checkout, route_confirmation, terminal=False)
    if status == 0 or retry_events or altered_env.read_bytes() != altered_before or not pending.is_dir():
        raise AssertionError("retry overwrote changed candidate app bytes or crossed discovery")
    status, output, retry_events = run_selection(checkout, action="unpublish", terminal=False)
    if (status == 0 or retry_events or altered_env.read_bytes() != altered_before or
        b"needs proved recovery before any route action" not in output):
        raise AssertionError("uncertain setup journal admitted a separate route operation")

    checkout = selection_checkout("changed-candidate-marker")
    status, output, events = run_selection(
        checkout, route_confirmation, terminal=False, overrides={"DEV_FIXTURE_PREFLIGHT_MODE": "tamper-marker"},
    )
    pending = checkout / "relay" / ".dev-tailscale-cli" / ".setup-pending"
    altered_marker = checkout / "relay" / ".dev-tailscale-cli" / ".herdr-dev-tailscale-cli"
    if status == 0 or not pending.is_dir() or b"changed marker" not in altered_marker.read_bytes():
        raise AssertionError("unknown candidate marker was silently rolled back")
    marker_before = altered_marker.read_bytes()
    status, output, retry_events = run_selection(checkout, route_confirmation, terminal=False)
    if status == 0 or retry_events or altered_marker.read_bytes() != marker_before or not pending.is_dir():
        raise AssertionError("retry overwrote changed candidate marker or crossed discovery")

    for action in ("update", "recover", "repair-missing", "status"):
        checkout = selection_checkout(f"no-fresh-discovery-{action}")
        status, output, events = run_selection(checkout, action=action)
        if status == 0 or discovery_boundary in output or events:
            raise AssertionError(f"{action} entered fresh discovery: {output!r} {events!r}")
        assert_no_selection_effects(checkout, events)
    status, output, events = run_selection(
        saved_checkout, action="update",
        overrides={"HERDR_DEV_TAILSCALE_CLI_RELAY_BIN": str(saved_root / "current" / "bin" / "herdr-mobile-relay")},
    )
    if (status != 0 or discovery_boundary in output or
        events.count(f"manager|workflow assert-ready --development-root {saved_root}") != 2 or
        "control|localcontrol admit" not in events or
        relay_env_values(saved_root / "relay.env").get("HERDR_PHONE_APP_URL") != saved_origin or
        any(event.startswith(("manager|workflow publish ", "control|localcontrol arm_bootstrap")) for event in events)):
        raise AssertionError(f"saved update entered origin selection or publication: {output!r} {events!r}")
    print("PASS CLI origin discovery fixtures: terminal and unattended defaults, strict refusals, cancellation, staging retry, changed-candidate refusal, saved app and separate route consent")
    positive_socket.close()
    print("PASS CLI development lifecycle fixture: isolated setup/update, setup migration, one-time rewrite, refusal preservation, production state preserved")

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

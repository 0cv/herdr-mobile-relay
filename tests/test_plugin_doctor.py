#!/usr/bin/env python3
"""Disposable hosted-only wrapper/public-entrypoint fixtures; never live status.
Inert binaries prove wrapper selection/argv/exit only, NOT Go health behavior.
The native fixture's already-built ordinary helper also checks real dispatch;
its unstamped identity cannot qualify package/local/phone health.
"""
import os
import sys

# Guard before imports that perform fixture activity, temp creation/subprocesses.
if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
    print("doctor fixtures require disposable GitHub-hosted CI", file=sys.stderr)
    raise SystemExit(2)

import json
from pathlib import Path
import pty
import select
import shutil
import subprocess
import tempfile
import time
from types import SimpleNamespace

REPO = Path(__file__).resolve().parent.parent
HELPER = os.environ.get("NATIVE_HELPER", "")
if not HELPER or not Path(HELPER).is_file() or not os.access(HELPER, os.X_OK):
    print("doctor fixtures require the native fixture's compiled helper", file=sys.stderr)
    raise SystemExit(2)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


CAPTURE_LIMIT = 64 * 1024
TAIL_READ = 8 * 1024


def redactions():
    return ((globals().get("root", ""), "<fixture-root>"), (REPO, "<repo>"),
            (os.environ.get("HOME", ""), "<home>"))


def file_tail(handle, limit=TAIL_READ):
    # At most the last `limit` bytes of a file-backed capture; the file is never read whole.
    handle.seek(0, os.SEEK_END)
    handle.seek(max(0, handle.tell() - limit))
    return handle.read(limit)


def diagnostic_tail(raw, *private):
    # Evidence only, never an assertion: a failing check names what the real helper printed.
    # `raw` is an already bounded byte tail (file_tail). Each fixture path in `private` is
    # replaced by a label, then the last 40 lines and at most 4096 BYTES are kept.
    text = raw.decode("utf-8", errors="replace")
    for value, label in private:
        if str(value):
            text = text.replace(str(value), label)
    tail = "\n".join(text.splitlines()[-40:]).encode("utf-8")[-4096:]
    return tail.decode("utf-8", errors="replace")


def run_bounded(argv, env):
    # Same call as run_wrapper's subprocess.run (same 10 s bound), but stdout and stderr go to
    # unlinked temp files, so a verbose or runaway helper cannot fill this process's memory. Only
    # a head of CAPTURE_LIMIT bytes (for the unchanged checks) and a bounded tail are read back.
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        process = subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=err, timeout=10)
        captured = []
        for handle in (out, err):
            handle.seek(0)
            head = handle.read(CAPTURE_LIMIT + 1)
            captured.append((head, file_tail(handle)))
    (out_head, out_tail), (err_head, err_tail) = captured
    require(len(out_head) <= CAPTURE_LIMIT and len(err_head) <= CAPTURE_LIMIT,
            "helper output exceeded %d bytes; stdout tail %r; stderr tail %r" % (
                CAPTURE_LIMIT, diagnostic_tail(out_tail, *redactions()), diagnostic_tail(err_tail, *redactions())))
    return SimpleNamespace(returncode=process.returncode,
                           stdout=out_head.decode("utf-8", errors="replace"),
                           stderr=err_head.decode("utf-8", errors="replace"),
                           stdout_tail=out_tail, stderr_tail=err_tail)


def write_binary(path):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!" + sys.executable + "\n" + '''import json, os, sys
with open(os.environ["DOCTOR_ARGV"], "w") as out:
    json.dump(sys.argv[1:], out)
if os.environ.get("NOISY_LOADER"):
    print("private-path-canary secret-wrapper-canary", file=sys.stderr)
code = int(os.environ.get("DOCTOR_EXIT", "3"))
if "--json" in sys.argv[1:]:
    print(json.dumps({"fixture": "inert-wrapper-only", "schema_version": 1, "exit_code": code}))
else:
    print("inert doctor report exit=" + str(code))
raise SystemExit(code)
''')
    path.chmod(0o700)


def run_wrapper(script, env, args=(), code=3, bounded=False):
    argv = ["/bin/bash", str(script), *args]
    if bounded:
        result = run_bounded(argv, env)
    else:
        result = subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=10)
    require(result.returncode == code, "wrapper did not propagate chosen exit")
    require("Press Enter" not in result.stdout, "noninteractive wrapper paused")
    require(not any(x in result.stdout + result.stderr for x in
                    ["secret-wrapper-canary", "private-path-canary", "$(", "\x1b"]), "wrapper disclosed private input")
    return result


def terminal_wrapper(script, env, args, pause):
    master, slave = pty.openpty()
    process = subprocess.Popen(["/bin/bash", str(script), *args], env=env, stdin=slave, stdout=slave,
                               stderr=subprocess.PIPE, start_new_session=True)
    os.close(slave)
    output = bytearray()
    responded = False
    deadline = time.monotonic() + 10
    try:
        while time.monotonic() < deadline:
            readable, _, _ = select.select([master], [], [], 0.1)
            if readable:
                try:
                    block = os.read(master, 8192)
                except OSError:
                    break
                if not block:
                    break
                output.extend(block)
                if b"Press Enter" in output and not responded:
                    # EOF rather than a successful read must preserve exit 3.
                    os.write(master, b"\x04")
                    responded = True
            if process.poll() is not None and not readable:
                break
        remaining = max(0.1, deadline - time.monotonic())
        _, stderr = process.communicate(timeout=remaining)
        require(process.returncode == 3, "interactive pause overwrote diagnostic exit")
        text = output.decode()
        require(text.count("Press Enter") == (1 if pause else 0), "wrong/double terminal pause")
        require(not stderr, "unexpected interactive diagnostic stderr")
        if "--json" in args:
            require(json.loads(text)["exit_code"] == 3, "terminal JSON contaminated")
    finally:
        if process.poll() is None:
            process.kill()  # disposable fixture child only; never service cleanup
            process.wait(timeout=5)
        os.close(master)


with tempfile.TemporaryDirectory(prefix="herdr-doctor-fixtures-") as directory:
    root = Path(directory).resolve()  # macOS /var aliases must not defeat no-follow reads
    home = root / "isolated home & spaces"
    home.mkdir()
    package = root / "package with spaces"
    (package / "relay").mkdir(parents=True)
    script = package / "relay/plugin-status.sh"
    shutil.copy2(REPO / "relay/plugin-status.sh", script)
    (package / "release-manifest.json").write_text("{}\n")
    write_binary(package / "herdr-mobile-relay")
    traps = root / "traps"
    traps.mkdir()
    marker = root / "tool-called"
    for command in ["go", "bun", "npm", "npx", "curl", "tailscale", "tailscaled", "launchctl", "systemctl",
                    "cloudflared", "herdr", "ssh", "sudo", "brew", "uname", "id", "security", "open", "download", "touch"]:
        trap = traps / command
        trap.write_text('#!/bin/sh\nprintf called > "$DOCTOR_TRAP"\nexit 99\n')
        trap.chmod(0o700)
    env = {"PATH": str(traps), "HOME": str(home), "DOCTOR_ARGV": str(root / "argv.json"),
           "DOCTOR_TRAP": str(marker), "DOCTOR_EXIT": "3", "GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted"}
    malicious = package / "relay/.env"
    source_marker = root / "config-sourced"
    malicious.write_text('HERDR_RELAY_TOKEN=secret-wrapper-canary\nK=$(touch "' + str(source_marker) + '")\n')
    malicious.chmod(0o600)
    before = malicious.read_bytes(), malicious.stat().st_mode, sorted(x.name for x in malicious.parent.iterdir())
    # Adjacent package beats explicit fallback. Config is data, not evaluated.
    env["HERDR_RELAY_BIN"] = str(root / "private-path-canary-missing-binary")
    run_wrapper(script, env)
    require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == ["doctor", "--config", str(malicious)], "adjacent package/default selector mismatch")
    for code in [0, 1, 2, 3, 78]:
        local = dict(env, DOCTOR_EXIT=str(code), NOISY_LOADER="1")
        result = run_wrapper(script, local, ["--json", "--probe-ingress"], code)
        require(json.loads(result.stdout)["exit_code"] == code, "JSON exit changed")
        require(not result.stderr, "valid JSON report gained wrapper/loader stderr")
        require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == ["doctor", "--config", str(malicious), "--json", "--probe-ingress"], "quoted fixed diagnostic argv mismatch")
    # --concise selects only the fixed status verb, never an arbitrary command.
    # Consume it without disturbing argv ordering or explicit config values.
    for code in [0, 1, 2, 3]:
        for args, forwarded in [
            (["--concise"], ["status", "--config", str(malicious)]),
            (["--json", "--concise", "--probe-ingress"], ["status", "--config", str(malicious), "--json", "--probe-ingress"]),
            (["--config", str(malicious), "--concise", "--json"], ["status", "--config", str(malicious), "--json"]),
        ]:
            result = run_wrapper(script, dict(env, DOCTOR_EXIT=str(code)), args, code)
            require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == forwarded, "concise selector changed fixed argv/config data")
            require(not result.stderr, "concise report gained refusal text")
            if "--json" in args:
                require(json.loads(result.stdout)["exit_code"] == code, "concise JSON report/exit mismatch")
    human_refused_config = run_wrapper(script, dict(env, DOCTOR_EXIT="2"), code=2)
    require(human_refused_config.stdout == "inert doctor report exit=2\n" and not human_refused_config.stderr,
            "human report decision mistaken for invocation refusal")
    for selector, extra, want in [
        ("env", {"HERDR_RELAY_ENV": str(root / "private env & space")}, root / "private env & space"),
        ("dir", {"HERDR_PLUGIN_CONFIG_DIR": str(root / "private config dir")}, root / "private config dir/relay.env"),
        ("precedence", {"HERDR_RELAY_ENV": str(malicious), "HERDR_PLUGIN_CONFIG_DIR": str(root / "ignored")}, malicious),
    ]:
        run_wrapper(script, dict(env, **extra))
        require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == ["doctor", "--config", str(want)], "selector precedence/quoting mismatch")
    explicit = root / "explicit config"
    run_wrapper(script, env, ["--json", "--config", str(explicit)])
    require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == ["doctor", "--json", "--config", str(explicit)], "explicit config was duplicated/overridden")
    for args in [["--unknown=secret-wrapper-canary"], ["--config"], ["--json", "--json"], ["--concise", "--concise"], ["--concise=true"], ["--probe-ingress", "--probe-ingress"], ["--config", "x", "--config", "y"]]:
        result = run_wrapper(script, env, args, 2)
        require(not result.stdout and result.stderr == "invalid diagnostic invocation\n", "wrapper invocation error not fixed")
    require(not source_marker.exists() and not marker.exists(), "wrapper sourced config or invoked forbidden tool")
    after = malicious.read_bytes(), malicious.stat().st_mode, sorted(x.name for x in malicious.parent.iterdir())
    require(before == after, "wrapper changed config/package inventory")
    terminal_wrapper(script, env, [], True)
    terminal_wrapper(script, dict(env, HERDR_SETUP_MENU="1"), [], False)
    terminal_wrapper(script, env, ["--json"], False)
    terminal_wrapper(script, env, ["--concise"], True)
    terminal_wrapper(script, dict(env, HERDR_SETUP_MENU="1"), ["--concise"], False)
    terminal_wrapper(script, env, ["--concise", "--json"], False)

    # Unpackaged/source location: explicit binary, release root and XDG/HOME
    # selection are read-only compatibility paths, never a build/download.
    source = root / "source/relay/plugin-status.sh"
    source.parent.mkdir(parents=True)
    shutil.copy2(script, source)
    fallback = root / "fallback with spaces/herdr-mobile-relay"
    write_binary(fallback)
    run_wrapper(source, dict(env, HERDR_RELAY_BIN=str(fallback)))
    for key, base in [("HERDR_RELEASE_ROOT", root / "releases"), ("XDG_DATA_HOME", root / "xdg"), ("HOME", home)]:
        local = dict(env)
        local.pop("HERDR_RELAY_BIN", None)
        local[key] = str(base)
        if key == "HERDR_RELEASE_ROOT": binary = base / "current/herdr-mobile-relay"
        elif key == "XDG_DATA_HOME": binary = base / "herdr-mobile-relay/current/herdr-mobile-relay"
        else: binary = base / ".local/share/herdr-mobile-relay/current/herdr-mobile-relay"
        write_binary(binary)
        run_wrapper(source, local)
    installed = root / "installed with spaces"
    package_binary = installed / "releases/unit/herdr-mobile-relay"
    write_binary(package_binary)
    (installed / "current").symlink_to(package_binary.parent, target_is_directory=True)
    shim = root / "shim with spaces/herdr-mobile-relay"
    shim.parent.mkdir()
    shim.symlink_to(installed / "current/herdr-mobile-relay")
    run_wrapper(source, dict(env, HERDR_RELAY_BIN=str(shim)), ["--concise", "--json"])
    require(json.loads(Path(env["DOCTOR_ARGV"]).read_text()) == ["status", "--config", str(source.parent / ".env"), "--json"],
            "installed-style shim concise selector/argv changed")
    result = run_wrapper(source, dict(env, HERDR_RELAY_BIN=str(root / "private-path-canary-missing")), code=1)
    require(not result.stdout and result.stderr == "diagnostic executable unavailable\n", "missing executable disclosed selected path")
    bad_binary = root / "private-path-canary-invalid-binary"
    bad_binary.write_bytes(b"\x00not executable format\x00")
    bad_binary.chmod(0o700)
    result = subprocess.run(["/bin/bash", str(source)], env=dict(env, HERDR_RELAY_BIN=str(bad_binary)),
                            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=10)
    require(result.returncode != 0 and "private-path-canary" not in result.stderr, "loader refusal disclosed binary path")

    # The actual compiled public dispatcher must not source malicious config,
    # construct an app, consult service tools, or invent unstamped build health.
    actual_config = root / "actual-private-config"
    actual_config.write_text("HERDR_RELAY_TRANSPORT=tailscale-cli\n")
    actual_config.chmod(0o600)
    actual_env = dict(env, HERDR_RELAY_BIN=HELPER, HERDR_RELAY_ENV=str(actual_config),
                      HERDR_GATEWAY_URL="wss://secret-wrapper-canary@private-node-canary", HERDR_RELAY_REARM_BOOTSTRAP="bad")
    for invalid in [["--json", "--config", "relative-private-path-canary"], ["--json", "--config", "/private/../private-path-canary"], ["--concise", "--json", "--config", "--concise"]]:
        refused = run_wrapper(source, actual_env, invalid, 2)
        require(not refused.stdout and refused.stderr == "diagnostic input refused\n", "real invocation refusal not fixed/empty")
    result = run_wrapper(source, actual_env, ["--json"])
    require(json.loads(result.stdout)["schema_version"] == 1, "real compiled command dispatch missing")
    require(not result.stderr, "real valid JSON report gained wrapper stderr")
    require(json.loads(result.stdout)["relay_build"]["version"] == "unknown", "unstamped helper invented package identity")
    concise = run_wrapper(source, actual_env, ["--concise"], bounded=True)
    require(concise.stdout.startswith("selected_transport=tailscale-cli status=unknown exit=3") and not concise.stderr,
            "real concise status not the finite selected-transport report; stdout tail %r; stderr tail %r" % (
                diagnostic_tail(concise.stdout_tail, *redactions()),
                diagnostic_tail(concise.stderr_tail, *redactions())))
    concise_json = run_wrapper(source, actual_env, ["--concise", "--json"])
    require(json.loads(concise_json.stdout)["selected_transport"] == "tailscale-cli" and not concise_json.stderr,
            "real concise JSON disagreed on selected transport")
    actual_config.write_text('K=$(touch "' + str(source_marker) + '")\n')
    result = run_wrapper(source, actual_env, ["--json"], 2)
    require(json.loads(result.stdout)["exit_code"] == 2 and not source_marker.exists(), "actual parser evaluated source/config")
    require(not result.stderr, "real invalid-config JSON report gained refusal text")
    actual_config.write_text("HERDR_RELAY_TRANSPORT=cloudflare\n")
    actual_config.chmod(0o644)
    result = run_wrapper(source, actual_env, ["--json"], 2)
    require(json.loads(result.stdout)["exit_code"] == 2 and not result.stderr,
            "unsafe selected-file JSON report gained refusal text")
    result = run_wrapper(source, actual_env, ["--json", "--config", str(root / "missing selected config")])
    require(json.loads(result.stdout)["exit_code"] == 3 and not result.stderr,
            "unavailable selected-file JSON report not single-document/incomplete")
    require(not marker.exists(), "diagnostic invoked toolchain/network/service/CLI")

# Static list correspondence supplements (never replaces) later extracted-package
# execution with a stamped, manifest-bound real binary on each supported target.
packaging = (REPO / "scripts/package-release.sh").read_text()
require("        plugin-status.sh \\\n" in packaging, "status wrapper not reachable in package list")
require("cp \"$REPO_DIR/README.md\" \"$STAGE/README.md\"" in packaging, "operator guidance not packaged")
print("hosted doctor wrapper/public-dispatch fixtures passed (not full package/phone qualification)")

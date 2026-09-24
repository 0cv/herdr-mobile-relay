"""S6B3C1 managed-owner hold driver: relay/start.sh must take O before mutating.

Run only through S6B3C1/run-checks.sh. The HERDR_B3C1_SANDBOX guard prevents
accidental host execution; bwrap, not that guard, supplies filesystem, PID and
network isolation. The product entrypoints are exact copies of the candidate
scripts (optionally replaced by the pre-fix overlay in regression mode), and
every dependency adapter is an inert recording stub. The managed-state holder
itself is the frozen S9A build mounted read-only at /hold-bin, so these
scenarios exercise the real lock semantics rather than an emulation.
"""
import json
import os
from pathlib import Path
import select
import shutil
import signal
import subprocess
import sys
import time
import traceback

if os.environ.get("HERDR_B3C1_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use immutable S6B3C1/run-checks.sh")
SOURCE = Path(os.environ["HERDR_B3C1_SOURCE"])
if str(SOURCE) != "/src":
    raise SystemExit("Expected the wrapper's /src source mount")
LOG = Path(os.environ["HERDR_B3C1_LOG"])
OVERLAY = Path(os.environ["HERDR_B3C1_OVERLAY"])
REGRESSION = os.environ.get("HERDR_B3C1_PREFIX") == "1"

HOLD_BIN = "/hold-bin/herdr-mobile-relay"

FIXTURE_TOKEN = "a1" * 32
FIXTURE_INSTANCE = "b2" * 16
FIXTURE_GATEWAY = "wss://gw.example.invalid"

COPIED_SCRIPTS = ("common.sh", "start.sh")

RELAY_STUB = """#!/bin/bash
cmd="${1:-}"
printf 'relay\\t%s\\n' "$*" >> "$HERDR_B3C1_CALLS"
case "$cmd" in
    managed-state|normalize-origin)
        exec /hold-bin/herdr-mobile-relay "$@"
        ;;
    serve)
        trap 'exit 0' INT TERM
        while :; do sleep 0.2; done
        ;;
    check-port)
        exit 0
        ;;
    *)
        exit 91
        ;;
esac
"""

CURL_STUB = """#!/bin/bash
printf 'curl\\t%s\\n' "$*" >> "$HERDR_B3C1_CALLS"
case "$*" in
    *healthz*)
        if [ "${HERDR_B3C1_CURL:-}" = "0" ]; then
            printf '{"status":"ok","instance":"%s","version":"0.0.0","protocol":"1","gateway":{"enabled":true,"registered":true,"relay_id":"relay","clients":0}}\\n' "$HERDR_RELAY_INSTANCE_ID"
            exit 0
        fi
        exit 22
        ;;
esac
exit 22
"""

HERDR_STUB = """#!/bin/bash
printf 'herdr\\t%s\\n' "$*" >> "$HERDR_B3C1_CALLS"
exit 0
"""

SYSTEMCTL_STUB = """#!/bin/bash
printf 'systemctl\\t%s\\n' "$*" >> "$HERDR_B3C1_CALLS"
exit 3
"""

LAUNCHCTL_STUB = """#!/bin/bash
printf 'launchctl\\t%s\\n' "$*" >> "$HERDR_B3C1_CALLS"
exit 3
"""


class HarnessError(Exception):
    pass


def write_stub(path, text):
    path.write_text(text)
    path.chmod(0o755)


def setup_fixture():
    root = LOG / "fixture"
    if root.exists():
        shutil.rmtree(root)
    relay = root / "relay"
    bin_dir = root / "bin"
    config = root / "config"
    home = root / "home"
    xdg = root / "xdg"
    tmp = root / "tmp"
    for path in (relay, bin_dir, config, home, xdg, tmp):
        path.mkdir(parents=True)
    for name in COPIED_SCRIPTS:
        shutil.copy2(SOURCE / "relay" / name, relay / name)
    if REGRESSION:
        for name in COPIED_SCRIPTS:
            shutil.copy2(OVERLAY / name, relay / name)
    config.chmod(0o700)
    env_file = config / "relay.env"
    env_file.write_text(
        "HERDR_RELAY_TOKEN=" + FIXTURE_TOKEN + "\n"
        "HERDR_RELAY_INSTANCE_ID=" + FIXTURE_INSTANCE + "\n"
        "HERDR_GATEWAY_URL='" + FIXTURE_GATEWAY + "'\n"
    )
    env_file.chmod(0o600)
    write_stub(bin_dir / "herdr-mobile-relay", RELAY_STUB)
    write_stub(bin_dir / "curl", CURL_STUB)
    write_stub(bin_dir / "herdr", HERDR_STUB)
    write_stub(bin_dir / "systemctl", SYSTEMCTL_STUB)
    write_stub(bin_dir / "launchctl", LAUNCHCTL_STUB)
    return {
        "root": root,
        "relay": relay,
        "bin": bin_dir,
        "config": config,
        "home": home,
        "xdg": xdg,
        "tmp": tmp,
        "env_file": env_file,
        "calls": LOG / "calls.log",
        "lock": config / "owner.lock",
    }


def base_env(fx):
    return {
        "HOME": str(fx["home"]),
        "XDG_CONFIG_HOME": str(fx["xdg"]),
        "TMPDIR": str(fx["tmp"]),
        "PATH": str(fx["bin"]) + ":/usr/bin:/bin",
        "HERDR_PLUGIN_CONFIG_DIR": str(fx["config"]),
        "HERDR_RELAY_BIN": str(fx["bin"] / "herdr-mobile-relay"),
        "HERDR_RELAY_TOKEN": FIXTURE_TOKEN,
        "HERDR_RELAY_INSTANCE_ID": FIXTURE_INSTANCE,
        "HERDR_B3C1_CALLS": str(fx["calls"]),
        "TERM": "dumb",
        "NO_COLOR": "1",
    }


def scenario_env(fx, **overrides):
    env = base_env(fx)
    for key, value in overrides.items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value
    return env


def read_calls(fx):
    path = fx["calls"]
    if not path.exists():
        return []
    calls = []
    for line in path.read_text().splitlines():
        if not line:
            continue
        name, _, args = line.partition("\t")
        calls.append((name, args))
    return calls


def has_serve(calls):
    return any(name == "relay" and args.split(" ", 1)[0] == "serve"
               for name, args in calls)


def start_script(fx, env):
    return subprocess.Popen(
        ["/bin/bash", str(fx["relay"] / "start.sh")],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
        cwd=str(fx["root"]),
        start_new_session=True,
    )


def kill_group(proc):
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        pass


def strip_token_instance(fx):
    """Make the refusal case prove that no credential is created while another
    owner holds O: drop both keys so any ensure_relay_env write is observable."""
    kept = [
        line for line in fx["env_file"].read_text().splitlines()
        if not line.startswith("HERDR_RELAY_TOKEN=")
        and not line.startswith("HERDR_RELAY_INSTANCE_ID=")
    ]
    fx["env_file"].write_text("\n".join(kept) + "\n")


def scenario_refused_when_owner_held(fx):
    strip_token_instance(fx)
    start_len = len(read_calls(fx))
    env_before = fx["env_file"].read_bytes()
    # A real S9A owner holder owns O for the whole scenario.
    holder = subprocess.Popen(
        [HOLD_BIN, "managed-state", "hold",
         "--dir", str(fx["config"]), "--operation", "owner"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=base_env(fx),
        cwd=str(fx["root"]),
    )
    proc = None
    quick = False
    code = None
    try:
        ready, _, _ = select.select([holder.stdout], [], [], 10)
        if not ready:
            raise HarnessError("owner holder never reported readiness")
        line = holder.stdout.readline()
        if b'"ok":true' not in line:
            raise HarnessError("owner holder readiness line: " + repr(line))
        proc = start_script(fx, base_env(fx))
        try:
            proc.wait(timeout=6)
            code = proc.returncode
            quick = True
        except subprocess.TimeoutExpired:
            quick = False
            kill_group(proc)
    finally:
        if proc is not None and proc.poll() is None:
            kill_group(proc)
        holder.send_signal(signal.SIGTERM)
        try:
            holder.wait(timeout=10)
        except subprocess.TimeoutExpired:
            holder.kill()
            holder.wait()
        holder.stdout.close()
        holder.stderr.close()
    env_after = fx["env_file"].read_bytes()
    new_calls = read_calls(fx)[start_len:]
    serve = has_serve(new_calls)
    result = bool(quick and code is not None and code != 0
                  and not serve and env_after == env_before)
    return result, {
        "exitCode": code,
        "exitedQuickly": quick,
        "serveCalled": serve,
        "envUnchanged": env_after == env_before,
    }


def scenario_releases_on_clean_exit(fx):
    # No herdr on PATH and no HERDR_BIN: the script stops at the herdr
    # requirement, which is already after ensure_relay_env and its ownership
    # admission, so the EXIT trap must retire the holder.
    env = scenario_env(fx, PATH="/usr/bin:/bin")
    env.pop("HERDR_BIN", None)
    proc = subprocess.run(
        ["/bin/bash", str(fx["relay"] / "start.sh")],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
        cwd=str(fx["root"]),
        timeout=30,
    )
    text = fx["env_file"].read_text()
    token_present = "HERDR_RELAY_TOKEN=" in text
    instance_present = "HERDR_RELAY_INSTANCE_ID=" in text
    lock_absent = not fx["lock"].exists()
    result = bool(token_present and instance_present and lock_absent)
    return result, {
        "exitCode": proc.returncode,
        "tokenPresent": token_present,
        "instancePresent": instance_present,
        "lockAbsent": lock_absent,
    }


def scenario_retains_on_crash(fx):
    proc = start_script(fx, scenario_env(fx, HERDR_BIN=str(fx["bin"] / "herdr")))
    observed = False
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if fx["lock"].exists() and has_serve(read_calls(fx)):
            observed = True
            break
        if proc.poll() is not None:
            break
        time.sleep(0.1)
    if not observed:
        kill_group(proc)
        return False, {"lockDuringRun": False, "freshHolderCode": None}
    # SIGKILL only the launching script: the holder is orphaned and must
    # retain O through its parent-death guard rather than through cleanup.
    proc.kill()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        kill_group(proc)
    retained = False
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        if fx["lock"].exists():
            retained = True
            break
        time.sleep(0.05)
    fresh = subprocess.run(
        [HOLD_BIN, "managed-state", "hold",
         "--dir", str(fx["config"]), "--operation", "owner"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=base_env(fx),
        cwd=str(fx["root"]),
        timeout=15,
    )
    if fresh.returncode == 0:
        # A successful acquisition would otherwise block; it did not, so the
        # holder exited immediately. Nothing more to clean up.
        pass
    kill_group(proc)
    result = bool(retained and fresh.returncode == 3)
    return result, {"lockDuringRun": True, "lockRetained": retained,
                    "freshHolderCode": fresh.returncode}


def main():
    if not (SOURCE / "relay" / "common.sh").is_file():
        print("harness error: missing source relay/common.sh", file=sys.stderr)
        return 2
    try:
        fx = setup_fixture()
    except Exception as error:
        print("harness error: fixture setup failed: " + str(error), file=sys.stderr)
        return 2
    if not fx["lock"].parent.is_dir():
        print("harness error: fixture config missing", file=sys.stderr)
        return 2
    if fx["calls"].exists():
        fx["calls"].unlink()
    try:
        refused, refused_detail = scenario_refused_when_owner_held(fx)
        released, released_detail = scenario_releases_on_clean_exit(fx)
        retained, retained_detail = scenario_retains_on_crash(fx)
    except Exception:
        traceback.print_exc()
        print("harness error: scenario execution failed", file=sys.stderr)
        return 2
    report = {
        "refused_when_owner_held": refused,
        "releases_on_clean_exit": released,
        "retains_on_crash": retained,
    }
    (LOG / "queries.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({
        "sandbox": "S6B3C1",
        "regression": REGRESSION,
        "report": report,
        "detail": {
            "refused_when_owner_held": refused_detail,
            "releases_on_clean_exit": released_detail,
            "retains_on_crash": retained_detail,
        },
    }, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""S12B space-path quoting driver.

Run only through S12B/run-checks.sh. The launcher, common.sh and every recorded
adapter stub live under a fixture directory whose name contains spaces, so an
unquoted executable expansion splits on whitespace and cannot run. The driver
executes the real copied launcher with `--confirm-serve` and records four
booleans from snapshots taken while admission is acknowledged and while the
external `tailscale serve` child is spawned.

The synthetic tailscale CLI always exits 1, which is the deterministic failure
after admission. The expected post-fix run therefore reaches the intended
Serve-readiness failure, verifies route absence during cleanup through the
quoted inspect call, removes the session marker and rolls the managed env back.
The pre-fix overlay (three unquoted substitutions) cannot execute the cleanup
inspect: it retains the session marker and prints the cleanup-verification
error. This script exits 0 whenever the scenario ran; the S12B checkers judge
the booleans. Harness errors (missing source, failed fixture setup) exit 2.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

CLEANUP_ERROR = "Could not verify Tailscale Serve/Funnel state during cleanup"
SERVE_FAILURE = "Tailscale Serve exited before its route became ready"

if os.environ.get("HERDR_S12B_SANDBOX") != "1":
    sys.exit("Refusing unsandboxed execution; use S12B/run-checks.sh")

FIXTURE_TOKEN = "a1" * 32
FIXTURE_INSTANCE = "b2" * 16
FIXTURE_ENV = (
    "HERDR_RELAY_TRANSPORT=tailscale\n"
    f"HERDR_RELAY_TOKEN={FIXTURE_TOKEN}\n"
    f"HERDR_RELAY_INSTANCE_ID={FIXTURE_INSTANCE}\n"
    "HERDR_UNRELATED_KEY=keep-me\n"
)

# Synthetic inspection: verified empty state. serve_route_owned stays false so a
# positive Serve admission remains fail-closed while the read-only guards accept
# the absence of any Serve/Funnel route.
INSPECTION = (
    '{"backend_state":"Running","logged_in":true,'
    '"origin":"https://node.example.invalid:8443","serve_configured":false,'
    '"funnel_configured":false,"serve_route_count":0,"serve_route_owned":false,'
    '"serve_inspected":true,"exposure_complete":true}'
)

SNAPSHOT_HELPER = """#!/usr/bin/env python3
import json, os, sys
out = sys.argv[1]
def read(path):
    try:
        with open(path, "rb") as handle:
            return handle.read().decode("utf-8")
    except FileNotFoundError:
        return None
data = {
    "env": read(os.environ["HERDR_S12B_ENV_FILE"]),
    "origin": read(os.environ["HERDR_S12B_ORIGIN_FILE"]),
    "session": read(os.environ["HERDR_S12B_SESSION_FILE"]),
}
with open(out, "w", encoding="utf-8") as handle:
    json.dump(data, handle)
"""

RELAY_STUB = """#!/usr/bin/env bash
set -u
command="${1:-}"
shift || true
case "$command" in
    json-field)
        /usr/bin/python3 -c 'import json,sys; kind,key=sys.argv[1:]; value=json.load(sys.stdin).get(key); valid=((kind=="bool" and type(value) is bool) or (kind=="string" and isinstance(value,str)) or (kind=="number" and type(value) is int and value>=0)); sys.exit(1) if not valid else print(str(value).lower() if type(value) is bool else value)' "$@"
        ;;
    supervise)
        if [ "${1:-}" = --grace ]; then shift 2; fi
        [ "${1:-}" = -- ] || exit 96
        shift
        exec "$@"
        ;;
    check-port)
        exit 0
        ;;
    tailscale)
        printf '%s\\n' '__INSPECTION__'
        exit 0
        ;;
    pairing-control)
        /usr/bin/python3 "$HERDR_S12B_HELPER" "$HERDR_S12B_CONTROL_SNAPSHOT"
        printf '{"ok":true,"ready":true,"run_id":"%s","instance":"%s"}\\n' \\
            "$HERDR_RELAY_RUN_ID" "$HERDR_RELAY_INSTANCE_ID"
        exit 0
        ;;
    serve)
        /usr/bin/python3 "$HERDR_S12B_HELPER" "$HERDR_S12B_SERVE_SNAPSHOT"
        trap 'exit 0' INT TERM
        while :; do sleep 0.2; done
        ;;
esac
exit 1
"""

TAILSCALE_STUB = """#!/usr/bin/env bash
set -u
/usr/bin/python3 "$HERDR_S12B_HELPER" "$HERDR_S12B_TAILSCALE_SNAPSHOT"
exit 1
"""

CURL_STUB = """#!/usr/bin/env bash
printf '%s\\n' '{"status": "ok", "instance": "'"$HERDR_RELAY_INSTANCE_ID"'", "version": "0.0.0", "protocol": "1", "managed_run_id": "'"$HERDR_RELAY_RUN_ID"'", "transport": "tailscale", "tailscale_origin": "'"$HERDR_TAILSCALE_ORIGIN"'"}'
exit 0
"""

failures = []


def harness_error(message):
    print("S12B driver error: " + message, file=sys.stderr)
    raise SystemExit(2)


def load_snapshot(path):
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text())
    except ValueError:
        return None


def main():
    source = Path(os.environ.get("HERDR_S12B_SOURCE", ""))
    log = Path(os.environ.get("HERDR_S12B_LOG", ""))
    overlay = Path(os.environ.get("HERDR_S12B_OVERLAY", ""))
    prefix = os.environ.get("HERDR_S12B_PREFIX") == "1"
    if not os.environ.get("HERDR_S12B_SOURCE") or not log.is_dir():
        harness_error("HERDR_S12B_SOURCE and an existing HERDR_S12B_LOG are required")
    if not (source / "relay" / "common.sh").is_file() or not (source / "relay" / "tailscale.sh").is_file():
        harness_error("candidate relay scripts are missing from " + str(source))
    if prefix and not (overlay / "tailscale.sh").is_file():
        harness_error("pre-fix overlay launcher is missing from " + str(overlay))

    fixture = log / "fixture with spaces"
    if fixture.exists():
        shutil.rmtree(fixture)
    scripts = fixture / "relay"
    bin_dir = fixture / "bin"
    home = fixture / "home"
    config = fixture / "xdg"
    tmp = fixture / "tmp"
    for path in (scripts, bin_dir, home, config, tmp):
        path.mkdir(parents=True)

    env_file = fixture / "relay.env"
    origin_file = fixture / "phone-app-origin-configured"
    session_file = fixture / "tailscale-session.env"
    control_snapshot = log / "control-snapshot.json"
    serve_snapshot = log / "serve-snapshot.json"
    tailscale_snapshot = log / "tailscale-snapshot.json"
    helper = bin_dir / "s12b_snapshot.py"

    original_bytes = FIXTURE_ENV.encode("utf-8")
    original_text = FIXTURE_ENV

    shutil.copy2(source / "relay" / "common.sh", scripts / "common.sh")
    launcher = overlay / "tailscale.sh" if prefix else source / "relay" / "tailscale.sh"
    shutil.copy2(launcher, scripts / "tailscale.sh")
    if (scripts / "tailscale.sh").read_bytes() != launcher.read_bytes():
        harness_error("launcher copy differs from its source")
    env_file.write_bytes(original_bytes)

    def write(path, text):
        path.write_text(text)
        path.chmod(0o755)

    write(helper, SNAPSHOT_HELPER)
    write(bin_dir / "herdr-mobile-relay", RELAY_STUB.replace("__INSPECTION__", INSPECTION))
    write(bin_dir / "tailscale", TAILSCALE_STUB)
    write(bin_dir / "curl", CURL_STUB)

    env = {
        "HOME": str(home),
        "XDG_CONFIG_HOME": str(config),
        "XDG_DATA_HOME": str(config / "data"),
        "TMPDIR": str(tmp),
        "PATH": str(bin_dir) + ":/usr/bin:/bin",
        "TERM": "dumb",
        "NO_COLOR": "1",
        "HERDR_RELAY_ENV": str(env_file),
        "HERDR_RELAY_TOKEN": FIXTURE_TOKEN,
        "HERDR_RELAY_INSTANCE_ID": FIXTURE_INSTANCE,
        "HERDR_RELAY_PORT": "18375",
        "HERDR_RELAY_PLUGIN_PORT": "18376",
        "HERDR_TAILSCALE_HTTPS_PORT": "8443",
        "HERDR_TAILSCALE_BIN": str(bin_dir / "tailscale"),
        "HERDR_RELAY_BIN": str(bin_dir / "herdr-mobile-relay"),
        "HERDR_S12B_ENV_FILE": str(env_file),
        "HERDR_S12B_ORIGIN_FILE": str(origin_file),
        "HERDR_S12B_SESSION_FILE": str(session_file),
        "HERDR_S12B_CONTROL_SNAPSHOT": str(control_snapshot),
        "HERDR_S12B_SERVE_SNAPSHOT": str(serve_snapshot),
        "HERDR_S12B_TAILSCALE_SNAPSHOT": str(tailscale_snapshot),
        "HERDR_S12B_HELPER": str(helper),
    }

    try:
        result = subprocess.run(
            ["/bin/bash", str(scripts / "tailscale.sh"), "--confirm-serve"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env=env,
            cwd=str(fixture),
            timeout=120,
        )
    except subprocess.TimeoutExpired as error:
        output = (error.stdout or b"").decode(errors="replace")
        print(output)
        harness_error("launcher did not finish within the bounded timeout")
    except OSError as error:
        harness_error("could not execute the copied launcher: " + str(error))

    output = result.stdout.decode(errors="replace")
    print(output)

    control = load_snapshot(control_snapshot)
    tailscale = load_snapshot(tailscale_snapshot)
    admission_snapshot_clean = (
        control is not None
        and control.get("env") == original_text
        and control.get("session") is None
    )
    session_written_before_serve = tailscale is not None and tailscale.get("session") is not None
    env_restored_after_failure = env_file.read_bytes() == original_bytes
    session_removed_after_cleanup = (
        not session_file.exists() and CLEANUP_ERROR not in output
    )

    report = {
        "admission_snapshot_clean": admission_snapshot_clean,
        "session_written_before_serve": session_written_before_serve,
        "session_removed_after_cleanup": session_removed_after_cleanup,
        "env_restored_after_failure": env_restored_after_failure,
        "evidence": {
            "launcherExit": result.returncode,
            "intendedServeFailure": SERVE_FAILURE in output,
            "cleanupVerificationError": CLEANUP_ERROR in output,
            "controlSnapshotEnvMatchesOriginal": bool(control) and control.get("env") == original_text,
            "controlSnapshotSession": None if control is None else control.get("session"),
            "tailscaleSnapshotSession": None if tailscale is None else tailscale.get("session"),
            "sessionFileExistsAfterRun": session_file.exists(),
            "envBytesAfterRun": env_file.read_bytes().decode("utf-8", errors="replace"),
        },
    }
    (log / "queries.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except SystemExit:
        raise
    except Exception as error:  # harness boundary: report, never traceback-claim success
        harness_error("unexpected harness failure: " + repr(error))

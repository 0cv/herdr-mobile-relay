"""S6B3B foreground launcher ordering driver.

Run only through S6B3B/run-checks.sh. The HERDR_B3B_SANDBOX guard prevents
accidental host execution; bwrap, not that guard, supplies filesystem, PID and
network isolation. Every product entrypoint is an exact copy of the candidate
launcher (optionally replaced by the pre-fix overlay in regression mode); all
dependency adapters are inert recording stubs.

The S6B2 serve process opens its pairing socket only after NewOwned validated
the published owner, so the launcher's real proof of admission is the
pairing-control `status` acknowledgement. This sandbox has no real sockets, so
the `pairing-control` stub answers that acknowledgement deterministically; the
Go tests already prove the real socket/owner ordering. The stub records the
managed files at the exact moments the launcher contacts it, which is what the
ordering assertions below compare.
"""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

if os.environ.get("HERDR_B3B_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use immutable S6B3B/run-checks.sh")
SOURCE = Path(os.environ["HERDR_B3B_SOURCE"])
if str(SOURCE) != "/src":
    raise SystemExit("Expected the wrapper's /src source mount")
LOG = Path(os.environ["HERDR_B3B_LOG"])
OVERLAY = Path(os.environ["HERDR_B3B_OVERLAY"])
REGRESSION = os.environ.get("HERDR_B3B_PREFIX") == "1"

COPIED_SCRIPTS = ("common.sh", "tailscale.sh")
FIXTURE_TOKEN = "a1" * 32
FIXTURE_INSTANCE = "b2" * 16
FIXTURE_ENV = (
    "HERDR_RELAY_TRANSPORT=tailscale\n"
    f"HERDR_RELAY_TOKEN={FIXTURE_TOKEN}\n"
    f"HERDR_RELAY_INSTANCE_ID={FIXTURE_INSTANCE}\n"
    "HERDR_UNRELATED_KEY=keep-me\n"
)

# Synthetic inspection: verified absence. serve_route_owned stays false so the
# positive Serve admission path remains fail-closed (DR-1). serve_inspected and
# exposure_complete are true so the read-only S5 guards accept the empty state.
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
        with open(path, "rb") as f:
            return f.read().decode("utf-8")
    except FileNotFoundError:
        return None
data = {
    "env": read(os.environ["HERDR_B3B_ENV_FILE"]),
    "origin": read(os.environ["HERDR_B3B_ORIGIN_FILE"]),
    "session": read(os.environ["HERDR_B3B_SESSION_FILE"]),
}
with open(out, "w", encoding="utf-8") as f:
    json.dump(data, f)
"""

RELAY_STUB = """#!/bin/bash
set -u
command="$1"; shift || true
printf 'relay\\t%s\\n' "$command $*" >> "$HERDR_B3B_CALLS"
case "$command" in
    supervise)
        if [ "${{1:-}}" = --grace ]; then shift 2; fi
        [ "${{1:-}}" = -- ] || exit 96
        shift
        exec "$@"
        ;;
    check-port)
        exit 0
        ;;
    tailscale)
        printf 'inspection\\t%s\\n' '{inspection}' >> "$HERDR_B3B_CALLS"
        printf '%s\\n' '{inspection}'
        exit 0
        ;;
    pairing-control)
        /usr/bin/python3 "$HERDR_B3B_SNAPSHOT_HELPER" "$HERDR_B3B_SNAPSHOT_DIR/control-snapshot.json"
        printf '{{"ok":true,"ready":true,"run_id":"%s","instance":"%s"}}\\n' \\
            "$HERDR_RELAY_RUN_ID" "$HERDR_RELAY_INSTANCE_ID"
        exit 0
        ;;
    serve)
        /usr/bin/python3 "$HERDR_B3B_SNAPSHOT_HELPER" "$HERDR_B3B_SNAPSHOT_DIR/serve-snapshot.json"
        trap 'exit 0' INT TERM
        while :; do sleep 0.2; done
        ;;
esac
exit 1
"""

TAILSCALE_STUB = """#!/bin/bash
printf 'tailscale\\t%s\\n' "$*" >> "$HERDR_B3B_CALLS"
exit 1
"""

CURL_STUB = """#!/bin/bash
if [ "${HERDR_B3B_SLOW_HEALTH:-}" = 1 ]; then
    sleep 30
fi
printf '%s\\n' '{{"status": "ok", "instance": "'"$HERDR_RELAY_INSTANCE_ID"'", "version": "0.0.0", "protocol": "1", "managed_run_id": "'"$HERDR_RELAY_RUN_ID"'", "transport": "tailscale", "tailscale_origin": "'"$HERDR_TAILSCALE_ORIGIN"'"}}'
exit 0
"""

COOPERATIVE_RELAY_CHILD = """#!/usr/bin/env python3
import json
import os
import signal
import time

def read(path):
    try:
        with open(path, "rb") as source:
            return source.read().decode("utf-8")
    except FileNotFoundError:
        return None

snapshot = {
    "env": read(os.environ["HERDR_B3B_ENV_FILE"]),
    "origin": read(os.environ["HERDR_B3B_ORIGIN_FILE"]),
    "session": read(os.environ["HERDR_B3B_SESSION_FILE"]),
}
with open(os.path.join(os.environ["HERDR_B3B_SNAPSHOT_DIR"], "serve-snapshot.json"), "w", encoding="utf-8") as out:
    json.dump(snapshot, out)

def record(event):
    with open(os.environ["HERDR_B3B_CALLS"], "a", encoding="utf-8") as out:
        out.write(f"cooperative_child\\t{event}\\tpid={os.getpid()}\\n")
        out.flush()

def stop(signum, _frame):
    record(f"signal-{signal.Signals(signum).name}")
    print(f"cooperative relay child handled {signal.Signals(signum).name}", flush=True)
    raise SystemExit(0)

signal.signal(signal.SIGINT, stop)
signal.signal(signal.SIGTERM, stop)
record("start")
print("cooperative relay child started", flush=True)
while True:
    time.sleep(0.1)
"""


class Fixture:
    def __init__(self, label):
        self.root = Path(tempfile.mkdtemp(prefix=label + ".", dir=LOG))
        self.scripts = self.root / "relay"
        self.bin = self.root / "bin"
        self.home = self.root / "home"
        self.config = self.root / "xdg"
        self.tmp = self.root / "tmp"
        for path in (self.scripts, self.bin, self.home, self.config, self.tmp):
            path.mkdir()
        for name in COPIED_SCRIPTS:
            shutil.copy2(SOURCE / "relay" / name, self.scripts / name)
            if (self.scripts / name).read_bytes() != (SOURCE / "relay" / name).read_bytes():
                raise AssertionError("script copy differs: " + name)
        if REGRESSION:
            shutil.copy2(OVERLAY / "tailscale.sh", self.scripts / "tailscale.sh")
        self.env_file = self.root / "relay.env"
        self.env_file.write_text(FIXTURE_ENV)
        self.origin_file = self.root / "phone-app-origin-configured"
        self.session_file = self.root / "tailscale-session.env"
        self.calls_path = self.root / "calls"
        self._write(self.bin / "b3b_snapshot.py", SNAPSHOT_HELPER, 0o755)
        self._write(self.bin / "herdr-mobile-relay",
                    RELAY_STUB.format(inspection=INSPECTION), 0o755)
        self._write(self.bin / "tailscale", TAILSCALE_STUB, 0o755)
        self._write(self.bin / "curl", CURL_STUB, 0o755)
        self.env = {
            "HOME": str(self.home),
            "XDG_CONFIG_HOME": str(self.config),
            "TMPDIR": str(self.tmp),
            "PATH": str(self.bin) + ":/usr/bin:/bin",
            "TERM": "dumb",
            "NO_COLOR": "1",
            "HERDR_RELAY_ENV": str(self.env_file),
            "HERDR_RELAY_TOKEN": FIXTURE_TOKEN,
            "HERDR_RELAY_INSTANCE_ID": FIXTURE_INSTANCE,
            "HERDR_RELAY_HOST": "127.0.0.1",
            "HERDR_RELAY_PORT": "18375",
            "HERDR_RELAY_PLUGIN_PORT": "18376",
            "HERDR_TAILSCALE_HTTPS_PORT": "8443",
            "HERDR_TAILSCALE_BIN": str(self.bin / "tailscale"),
            "HERDR_RELAY_BIN": str(self.bin / "herdr-mobile-relay"),
            "HERDR_B3B_ENV_FILE": str(self.env_file),
            "HERDR_B3B_ORIGIN_FILE": str(self.origin_file),
            "HERDR_B3B_SESSION_FILE": str(self.session_file),
            "HERDR_B3B_SNAPSHOT_DIR": str(self.root),
            "HERDR_B3B_SNAPSHOT_HELPER": str(self.bin / "b3b_snapshot.py"),
            "HERDR_B3B_CALLS": str(self.calls_path),
        }

    @staticmethod
    def _write(path, text, mode):
        path.write_text(text)
        path.chmod(mode)

    def use_cooperative_relay_child(self):
        child = self.bin / "cooperative-relay-child.py"
        self._write(child, COOPERATIVE_RELAY_CHILD, 0o755)
        self.env["HERDR_B3B_COOPERATIVE_CHILD"] = str(child)
        stub_path = self.bin / "herdr-mobile-relay"
        stub = stub_path.read_text()
        old = '        shift\n        exec "$@"\n        ;;'
        new = '''        shift
        if [ "${1:-}" = "$HERDR_RELAY_BIN" ] && [ "${2:-}" = serve ]; then
            exec /usr/bin/python3 "$HERDR_B3B_COOPERATIVE_CHILD"
        fi
        exec "$@"
        ;;'''
        if stub.count(old) != 1:
            raise AssertionError("unexpected fixture supervise stub")
        self._write(stub_path, stub.replace(old, new, 1), 0o755)

    def inject_child_stop_proof_refusal(self):
        self._set_fixture_child_stop_result(1)

    def apply_skip_child_stop_proof_mutant(self):
        self._set_fixture_child_stop_result(0)

    def _set_fixture_child_stop_result(self, result):
        common = self.scripts / "common.sh"
        source = common.read_text()
        start_marker = "stop_child_job() {\n"
        end_marker = "\n}\n\nrelay_release_root() {"
        if source.count(start_marker) != 1:
            raise AssertionError("expected exactly one stop_child_job fixture function")
        start = source.index(start_marker)
        end = source.find(end_marker, start)
        if end < 0:
            raise AssertionError("stop_child_job fixture function end not found")
        replacement = f"stop_child_job() {{\n    return {result}\n}}"
        common.write_text(source[:start] + replacement + source[end + len("\n}\n"):])

    def inspections(self):
        return [json.loads(args) for name, args in self.calls() if name == "inspection"]

    def cooperative_child_events(self):
        return [args.split("\t", 1)[0] for name, args in self.calls() if name == "cooperative_child"]

    def spawn(self, extra=None):
        env = dict(self.env)
        for key, value in (extra or {}).items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = value
        return subprocess.Popen(
            ["/bin/bash", str(self.scripts / "tailscale.sh"), "--confirm-serve"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=env,
            cwd=self.root,
            start_new_session=True,
        )

    def run(self, extra=None, timeout=60):
        env = dict(self.env)
        for key, value in (extra or {}).items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = value
        result = subprocess.run(
            ["/bin/bash", str(self.scripts / "tailscale.sh"), "--confirm-serve"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env=env,
            cwd=self.root,
            timeout=timeout,
        )
        return result.returncode, result.stdout.decode(errors="replace")

    def calls(self):
        if not self.calls_path.exists():
            return []
        return [line.split("\t", 1) for line in self.calls_path.read_text().splitlines() if line]

    def snapshot(self, name):
        path = self.root / name
        if not path.is_file():
            raise AssertionError(f"missing snapshot {name}: {self.calls()}")
        return json.loads(path.read_text())


class LauncherOrderTests(unittest.TestCase):
    expected_env = FIXTURE_ENV

    def test_no_persisted_state_before_relay_start(self):
        fixture = Fixture("serve-snapshot")
        fixture.use_cooperative_relay_child()
        fixture.run()
        snap = fixture.snapshot("serve-snapshot.json")
        self.assertEqual(snap["env"], self.expected_env)
        self.assertIsNone(snap["origin"])
        self.assertIsNone(snap["session"])

    def test_no_persisted_state_before_control_ready(self):
        fixture = Fixture("control-snapshot")
        fixture.use_cooperative_relay_child()
        fixture.run()
        snap = fixture.snapshot("control-snapshot.json")
        self.assertEqual(snap["env"], self.expected_env)
        self.assertIsNone(snap["origin"])
        self.assertIsNone(snap["session"])

    def test_missing_config_root_refused_before_side_effects(self):
        fixture = Fixture("missing-root")
        missing = fixture.root / "missing-dir"
        code, text = fixture.run({"HERDR_RELAY_ENV": str(missing / "relay.env")})
        self.assertNotEqual(code, 0, text)
        self.assertFalse(missing.exists(), "configuration root was created")
        for name, args in fixture.calls():
            self.assertNotIn("check-port", args, fixture.calls())
            self.assertNotIn("serve", args, fixture.calls())

    def test_serve_failure_after_admission_reported(self):
        fixture = Fixture("serve-failure")
        fixture.use_cooperative_relay_child()
        code, text = fixture.run()
        self.assertNotEqual(code, 0, text)
        self.assertIn("Tailscale Serve exited before its route became ready", text)
        serve_calls = [args for name, args in fixture.calls()
                       if name == "tailscale" and args.startswith("serve ")]
        self.assertEqual(len(serve_calls), 1, fixture.calls())

    def test_post_failure_rollback_restores_original(self):
        fixture = Fixture("rollback")
        fixture.use_cooperative_relay_child()
        code, text = fixture.run()
        self.assertNotEqual(code, 0, text)
        self.assertIn("Tailscale Serve exited before its route became ready", text)
        self.assertNotIn("Child cleanup could not verify a managed generation", text)
        self.assertEqual(fixture.env_file.read_text(), self.expected_env)
        self.assertFalse(fixture.origin_file.exists(), "origin file was left behind")
        self.assertFalse(fixture.session_file.exists(), "session marker was left behind")
        self.assertEqual(list(fixture.root.glob(".tailscale-*-log.*")), [],
                         "verified cleanup should remove relay and Serve logs")
        self.assertIn("signal-SIGINT", fixture.cooperative_child_events())
        inspections = fixture.inspections()
        self.assertGreaterEqual(len(inspections), 3, inspections)
        self.assertTrue(inspections[-1]["serve_inspected"], inspections[-1])
        self.assertTrue(inspections[-1]["exposure_complete"], inspections[-1])
        self.assertFalse(inspections[-1]["serve_configured"], inspections[-1])
        self.assertFalse(inspections[-1]["funnel_configured"], inspections[-1])

    def test_cleanup_proof_refusal_retains_session_and_logs(self):
        fixture = Fixture("cleanup-proof-refusal")
        fixture.use_cooperative_relay_child()
        original_origin = b"https://installed-app.example.invalid\n"
        fixture.origin_file.write_bytes(original_origin)
        if os.environ.get("HERDR_B3B_MUTANT_SKIP_CHILD_STOP_PROOF") == "1":
            fixture.apply_skip_child_stop_proof_mutant()
        else:
            fixture.inject_child_stop_proof_refusal()

        code, text = fixture.run()
        self.assertNotEqual(code, 0, text)
        self.assertIn("Tailscale Serve exited before its route became ready", text)
        with self.subTest("cleanup reports the proof refusal"):
            self.assertIn("Child cleanup could not verify a managed generation", text)
        self.assertEqual(fixture.env_file.read_text(), self.expected_env)
        self.assertEqual(fixture.origin_file.read_bytes(), original_origin)
        with self.subTest("session marker is retained"):
            self.assertTrue(fixture.session_file.is_file(), "unverified child stop discarded the session marker")
        with self.subTest("child logs are retained"):
            log_files = list(fixture.root.glob(".tailscale-*-log.*"))
            self.assertEqual(len(log_files), 2, log_files)
        self.assertEqual(fixture.cooperative_child_events(), ["start"],
                         "injected proof refusal must not claim the child received its stop signal")
        inspections = fixture.inspections()
        self.assertGreaterEqual(len(inspections), 3, inspections)
        self.assertTrue(inspections[-1]["serve_inspected"], inspections[-1])
        self.assertTrue(inspections[-1]["exposure_complete"], inspections[-1])
        self.assertFalse(inspections[-1]["serve_configured"], inspections[-1])
        self.assertFalse(inspections[-1]["funnel_configured"], inspections[-1])

    def test_no_managed_state_survives_kill_before_admission(self):
        # The launcher is held in its health wait with the serve child already
        # running, then killed without any chance to run its cleanup/rollback
        # trap. A launcher that persisted managed files before admission leaves
        # them behind; one that persists only after admission leaves nothing.
        fixture = Fixture("kill-before-admission")
        process = fixture.spawn({"HERDR_B3B_SLOW_HEALTH": "1"})
        snapshot = fixture.root / "serve-snapshot.json"
        deadline = time.monotonic() + 20
        while not snapshot.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(snapshot.exists(), "serve child never started")
        os.killpg(os.getpgid(process.pid), signal.SIGKILL)
        process.wait(timeout=10)
        self.assertEqual(fixture.env_file.read_text(), self.expected_env)
        self.assertFalse(fixture.origin_file.exists(), "origin file was left behind")
        self.assertFalse(fixture.session_file.exists(), "session marker was left behind")


if __name__ == "__main__":
    unittest.main(verbosity=2)

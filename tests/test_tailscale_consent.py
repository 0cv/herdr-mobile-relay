"""S4 consent only: synthetic inspection stops before any managed lifecycle.

Run only through S4/run-checks.sh. The environment guard prevents accidental
execution; bwrap, not that guard, supplies filesystem/network/PID isolation.
Every product entrypoint is an exact copy, including the shared env loader.
Retained /work fixtures contain synthetic data and per-invocation traces.
"""
import errno
import hashlib
import json
import os
from pathlib import Path
import pty
import select
import shutil
import stat
import subprocess
import tempfile
import time
import unittest

if os.environ.get("HERDR_S4_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use immutable S4/run-checks.sh")
SOURCE = Path(os.environ["HERDR_S4_SOURCE_ROOT"])
if str(SOURCE) not in ("/src", "/before"):
    raise SystemExit("Expected the wrapper's /src or /before source mount")
SCRIPTS = ("common.sh", "tailscale.sh", "plugin-setup-menu.sh", "plugin-choose-transport.sh")
PROMPT = b"Configure this Tailscale Serve session? [y/N] "

# All dependency adapters are inert, including absolute sibling entrypoints.
# Exact read-only menu/service probes are distinguished from forbidden actions.
ADAPTER = r'''#!/usr/bin/python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ["S4_LOG"])
def record(kind):
    with (root / "calls").open("a") as f:
        f.write(json.dumps([kind, name, args]) + "\n")
def deny():
    record("forbidden")
    print("S4_FORBIDDEN: " + name, file=sys.stderr)
    sys.exit(91)
if name == "systemctl" and args in (["--user", "is-active", "--quiet", "herdr-mobile-relay.service"], ["--user", "is-active", "herdr-mobile-relay.service"]):
    record("probe")
    sys.exit(3)
if name == "curl" and args in (["-fsS", "--max-time", "2", "http://127.0.0.1:8375/healthz"], ["-fsS", "--max-time", "3", "https://app.example.invalid/version.json"]):
    record("probe")
    sys.exit(22)
if name == "fake-relay" and len(args) == 3 and args[0] == "json-field":
    kind, key = args[1:]
    try:
        value = json.load(sys.stdin).get(key)
    except (ValueError, AttributeError):
        sys.exit(1)
    if kind == "bool" and type(value) is bool:
        print(str(value).lower())
    elif kind == "string" and isinstance(value, str):
        print(value)
    elif kind == "number" and type(value) is int and value >= 0:
        print(value)
    else:
        sys.exit(1)
    sys.exit(0)
if name != "fake-relay":
    deny()
if args == ["normalize-origin", "--allow-loopback-http", "https://app.example.invalid"] or args == ["normalize-origin", "--allow-loopback-http", ""]:
    # Menu-only read-only probe; no app/health emulation or listener.
    record("probe")
    sys.exit(1)
if args in (["check-port", "--host", "127.0.0.1", "--port", "8375", "--protocol", "tcp"], ["check-port", "--host", "127.0.0.1", "--port", "8376", "--protocol", "udp"]):
    record("check-port")
    sys.exit(0)
if args != ["tailscale", "inspect", "--binary", os.environ["HERDR_TAILSCALE_BIN"], "--https-port", "443"]:
    deny()
if os.environ.get("S4_LOADED") != "fixture-loaded":
    print("S4_FIXTURE_LOADER_MISSING", file=sys.stderr)
    sys.exit(92)
state = root / "inspect-count"
n = int(state.read_text()) + 1 if state.exists() else 1
state.write_text(str(n))
record("inspect")
incomplete = os.environ.get("S5_INCOMPLETE", "")
if n == 2:
    record("consent-passed")
    if not incomplete.startswith("second:"):
        print("S4_STOP_SECOND_INSPECT_73", file=sys.stderr)
        sys.exit(73)
if n not in (1, 2):
    deny()
# Synthetic inspector metadata, NOT CLI output or successful exposure.
value = dict(backend_state="Running", logged_in=True, funnel_configured=False,
             serve_configured=False, origin="https://node.example.invalid",
             serve_inspected=True, exposure_complete=True)
if incomplete.startswith("first:" if n == 1 else "second:"):
    _, field, state = incomplete.split(":")
    if field not in ("serve_inspected", "exposure_complete") or state not in ("missing", "false"):
        deny()
    if state == "missing":
        value.pop(field)
    else:
        value[field] = False
case = os.environ.get("S4_PREFLIGHT", "")
if case == "auth":
    value["logged_in"] = False
elif case == "serve":
    value["serve_configured"] = True
elif case == "funnel":
    value["funnel_configured"] = True
elif case == "identity":
    value["origin"] = ""
print(json.dumps(value))
'''


class Fixture:
    def __init__(self, saved="", layout="override"):
        self.root = Path(tempfile.mkdtemp(prefix="s4-", dir="/work"))
        self.scripts = self.root / "relay"
        self.bin = self.root / "bin"
        self.logs = self.root / "logs"
        self.home = self.root / "home"
        self.config = self.root / "plugin-config"
        for p in (self.scripts, self.bin, self.logs, self.home):
            p.mkdir()
        # No real sibling can be reached: every repository shell sibling is a
        # fail-on-use adapter, then EXACTLY the four product files are copied.
        for p in Path("/src/relay").glob("*.sh"):
            self.executable(self.scripts / p.name, ADAPTER)
        for name in SCRIPTS:
            shutil.copy2(SOURCE / "relay" / name, self.scripts / name)
            if (self.scripts / name).read_bytes() != (SOURCE / "relay" / name).read_bytes():
                raise AssertionError("script copy differs")
        for name in ("fake-relay", "tailscale", "herdr", "cloudflared", "systemctl", "launchctl", "curl", "sudo", "brew", "apt-get", "ssh", "scp", "install", "piper", "say"):
            self.executable(self.bin / name, ADAPTER)
        self.env_file = self.config / "relay.env"
        data = ("HERDR_RELAY_TOKEN=" + "ab" * 32 + "\n"
                "HERDR_RELAY_INSTANCE_ID=synthetic-s4-instance\n"
                "HERDR_RELAY_TRANSPORT=tailscale\n"
                "S4_LOADED=fixture-loaded\n" + saved)
        if layout in ("override", "existing"):
            self.config.mkdir(mode=0o755)
            self.env_file.write_text(data)
            self.env_file.chmod(0o640)
            (self.config / "phone-app-origin-configured").write_text("https://app.example.invalid\n\n")
            (self.config / "device-auth.json").write_text('{"synthetic-sentinel":true}\n\n')
            (self.config / "device-auth.json").chmod(0o600)
        else:
            if layout == "legacy-existing":
                self.config.mkdir(mode=0o755)
            (self.scripts / ".env").write_text(data)
            (self.scripts / ".env").chmod(0o640)
            (self.scripts / "push").mkdir(mode=0o755)
            (self.scripts / "push" / "sentinel").write_text("synthetic legacy push\n")
        self.env = dict(os.environ)
        self.env.update(HOME=str(self.home), XDG_CONFIG_HOME=str(self.home / "config"),
                        XDG_DATA_HOME=str(self.home / "data"), XDG_CACHE_HOME=str(self.home / "cache"),
                        XDG_RUNTIME_DIR=str(self.home / "runtime"), TMPDIR=str(self.logs),
                        PATH=str(self.bin) + ":/usr/bin:/bin", S4_LOG=str(self.logs),
                        HERDR_RELAY_BIN=str(self.bin / "fake-relay"),
                        HERDR_TAILSCALE_BIN=str(self.bin / "tailscale"),
                        HERDR_PLUGIN_CONFIG_DIR=str(self.config))
        self.env.pop("S4_LOADED", None)
        if layout == "override":
            self.env["HERDR_RELAY_ENV"] = str(self.env_file)
        else:
            self.env.pop("HERDR_RELAY_ENV", None)
        self.before = self.snapshot()
        self.number = 0

    @staticmethod
    def executable(path, body):
        path.write_text(body)
        path.chmod(0o755)

    def snapshot(self):
        result = {}
        # Include absent roots, directories, bytes and modes, not log counters.
        for base in (self.config, self.home, self.scripts / ".env", self.scripts / "push"):
            paths = [base] + (sorted(base.rglob("*")) if base.is_dir() else [])
            for p in paths:
                key = str(p.relative_to(self.root))
                if not p.exists():
                    result[key] = None
                else:
                    result[key] = (stat.S_IMODE(p.stat().st_mode), p.read_bytes() if p.is_file() else None)
        return result

    def calls(self):
        p = self.logs / "calls"
        return [json.loads(line) for line in p.read_text().splitlines()] if p.exists() else []

    def reset_calls(self):
        for name in ("calls", "inspect-count"):
            (self.logs / name).unlink(missing_ok=True)

    def run(self, entry="tailscale.sh", args=(), data="", extra=None, answer=None):
        env = self.env.copy()
        env.update(extra or {})
        command = ["/bin/bash", str(self.scripts / entry), *args]
        self.number += 1
        output = b""
        if answer is None:
            result = subprocess.run(command, input=data.encode(), stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, env=env, cwd=self.root, timeout=8)
            code, output = result.returncode, result.stdout
        else:
            master, slave = pty.openpty()
            child = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave,
                                     env=env, cwd=self.root, close_fds=True)
            os.close(slave)
            sent = False
            deadline = time.monotonic() + 8
            try:
                while True:
                    if time.monotonic() >= deadline:
                        raise AssertionError("PTY deadline exceeded")
                    ready, _, _ = select.select([master], [], [], 0.05)
                    if ready:
                        try:
                            part = os.read(master, 65536)
                        except OSError as error:
                            if error.errno != errno.EIO:
                                raise
                            break
                        if not part:
                            break
                        output += part
                    if not sent and PROMPT in output:
                        os.write(master, answer)
                        sent = True
                    if child.poll() is not None and not ready:
                        break
                code = child.wait(timeout=1)
            finally:
                if child.poll() is None:
                    child.kill()  # exact Popen child only; no process-group scans/signals
                    child.wait(timeout=1)
                os.close(master)
            if not sent:
                raise AssertionError("real PTY consent prompt was not reached: " + output.decode(errors="replace"))
        text = output.decode(errors="replace")
        (self.logs / f"invocation-{self.number}.json").write_text(json.dumps(
            dict(command=command, code=code, output=text, calls=self.calls()), indent=2))
        return code, text


class ConsentTests(unittest.TestCase):
    def preserved(self, f):
        after = f.snapshot()
        def identities(snapshot):
            return {key: None if value is None else {
                "mode": value[0],
                "sha256": hashlib.sha256(value[1]).hexdigest() if value[1] is not None else None
            } for key, value in snapshot.items()}
        (f.logs / f"preservation-{f.number}.json").write_text(json.dumps(
            dict(before=identities(f.before), after=identities(after)), indent=2))
        self.assertEqual(f.before, after, "config/legacy bytes, modes or absent roots changed")
        self.assertFalse([c for c in f.calls() if c[0] == "forbidden"], f.calls())

    def refused(self, f, result, reason="Explicit Tailscale Serve consent is required", inspected=True, menu=False):
        code, text = result
        self.assertEqual(code, 0 if menu else 1, text)
        self.assertIn(reason, text)
        self.assertNotIn("S4_FIXTURE_LOADER_MISSING", text)
        self.assertEqual(sum(c[0] == "inspect" for c in f.calls()), int(inspected), f.calls())
        self.assertFalse([c for c in f.calls() if c[0] == "consent-passed"], f.calls())
        self.preserved(f)

    def accepted(self, f, result):
        code, text = result
        self.assertEqual(code, 1, text)  # launcher deliberately refuses failed second inspection
        self.assertIn("S4_STOP_SECOND_INSPECT_73", text)
        self.assertIn("Tailscale changed before setup could start", text)
        self.assertEqual(sum(c[0] == "inspect" for c in f.calls()), 2)
        self.assertEqual(sum(c[0] == "consent-passed" for c in f.calls()), 1)
        self.assertEqual(sum(c[0] == "check-port" for c in f.calls()), 2)
        self.preserved(f)

    def test_incomplete_inspection_refused(self):
        for stage in ("first", "second"):
            for field in ("serve_inspected", "exposure_complete"):
                for state in ("missing", "false"):
                    with self.subTest(stage=stage, field=field, state=state):
                        f = Fixture()
                        code, text = f.run(args=("--confirm-serve",), extra={
                            "S5_INCOMPLETE": f"{stage}:{field}:{state}"})
                        self.assertEqual(code, 1, text)
                        self.assertIn("exposure inspection is incomplete", text)
                        self.assertNotIn("S4_STOP_SECOND_INSPECT_73", text)
                        self.assertEqual(sum(c[0] == "inspect" for c in f.calls()),
                                         1 if stage == "first" else 2)
                        self.assertEqual(sum(c[0] == "consent-passed" for c in f.calls()),
                                         0 if stage == "first" else 1)
                        self.assertEqual(sum(c[0] == "check-port" for c in f.calls()), 2)
                        self.preserved(f)

    def test_saved_yes_refused(self):
        f = Fixture("HERDR_TAILSCALE_YES=1\n")
        self.refused(f, f.run())

    def test_inherited_yes_refused(self):
        for value in ("1", "true", "TRUE", "yes", "on"):
            with self.subTest(value=value):
                f = Fixture()
                self.refused(f, f.run(extra={"HERDR_TAILSCALE_YES": value}))

    def test_generic_setup_yes_refused(self):
        for name in ("HERDR_SETUP_YES", "HERDR_TAILSCALE_REQUEST", "HERDR_SETUP_MENU"):
            with self.subTest(name=name):
                f = Fixture()
                self.refused(f, f.run(extra={name: "1", "HERDR_RELAY_TRANSPORT": "tailscale"}))

    def test_non_tty_yes_input_refused(self):
        for data in ("y\n", "yes\n"):
            with self.subTest(data=data):
                f = Fixture()
                self.refused(f, f.run(data=data))

    def test_interactive_default_refused(self):
        f = Fixture()
        self.refused(f, f.run(answer=b"\n"), "Setup cancelled before changing Tailscale Serve")

    def test_interactive_no_refused(self):
        for answer in (b"n\n", b"maybe\n"):
            with self.subTest(answer=answer):
                f = Fixture()
                self.refused(f, f.run(answer=answer), "Setup cancelled before changing Tailscale Serve")

    def test_interactive_eof_refused(self):
        f = Fixture()
        self.refused(f, f.run(answer=b"\x04"), "Setup cancelled before changing Tailscale Serve")

    def test_explicit_flag_reaches_boundary(self):
        f = Fixture()
        self.accepted(f, f.run(args=("--confirm-serve",)))
        f = Fixture()
        self.accepted(f, f.run("plugin-choose-transport.sh", ("tailscale", "--confirm-serve")))

    def test_interactive_yes_reaches_boundary(self):
        for answer in (b"y\n", b"yes\n"):
            with self.subTest(answer=answer):
                f = Fixture()
                result = f.run(answer=answer)
                self.accepted(f, result)
                for scope in ("https://node.example.invalid", "http://127.0.0.1:8375", "foreground"):
                    self.assertIn(scope, result[1])

    def test_confirmation_not_reused(self):
        f = Fixture("HERDR_TAILSCALE_YES=1\n")
        self.accepted(f, f.run(args=("--confirm-serve",)))
        f.reset_calls()
        self.refused(f, f.run(extra={"HERDR_TAILSCALE_YES": "1"}))

    def test_menu_inherited_yes_refused(self):
        f = Fixture("HERDR_TAILSCALE_YES=1\n")
        self.refused(f, f.run("plugin-setup-menu.sh", data="t\nq\n",
                             extra={"HERDR_TAILSCALE_YES": "1"}), menu=True)
        self.assertTrue([c for c in f.calls() if c[1] == "curl" and c[0] == "probe"])

    def test_chooser_inherited_yes_refused(self):
        f = Fixture("HERDR_TAILSCALE_YES=1\n")
        self.refused(f, f.run("plugin-choose-transport.sh", ("tailscale",),
                             extra={"HERDR_TAILSCALE_YES": "1"}))

    def test_preconsent_no_migration(self):
        for layout in ("legacy-absent", "legacy-existing", "existing"):
            for entry, args, data in (("tailscale.sh", (), ""),
                                      ("plugin-choose-transport.sh", ("tailscale",), ""),
                                      ("plugin-setup-menu.sh", (), "t\nq\n")):
                with self.subTest(layout=layout, entry=entry):
                    f = Fixture(layout=layout)
                    configured = layout == "existing"
                    self.refused(f, f.run(entry, args, data),
                                 reason="Explicit Tailscale Serve consent is required" if configured else "Relay credentials are not configured; run setup before Tailscale Serve",
                                 inspected=configured, menu=entry == "plugin-setup-menu.sh")

    def test_unknown_arguments_refused(self):
        for args in (("--unknown",), ("extra",), ("--confirm-serve=false",),
                     ("--confirm-serve", "extra"), ("--help", "extra")):
            with self.subTest(args=args):
                f = Fixture(layout="legacy-absent")
                code, text = f.run(args=args)
                self.assertEqual(code, 2, text)
                self.assertIn("Usage:", text)
                self.assertFalse(f.calls(), f.calls())
                self.preserved(f)

    def test_help_read_only(self):
        f = Fixture(layout="legacy-absent")
        code, text = f.run(args=("--help",))
        self.assertEqual(code, 0, text)
        self.assertIn("--confirm-serve", text)
        self.assertNotIn("HERDR_TAILSCALE_YES", text)
        self.assertFalse(f.calls(), f.calls())
        self.preserved(f)

    def test_internal_flag_cannot_be_injected(self):
        # Production uses its original positional argv, not an internal env
        # flag. CONSENT is only a fresh read result, never an inherited answer.
        for values in ("CONSENT=yes\n", "TS_SERVE_CONFIRMED=1\n", "HERDR_TAILSCALE_CONFIRM_SERVE=1\n"):
            with self.subTest(values=values):
                f = Fixture(values)
                name, value = values.strip().split("=")
                self.refused(f, f.run(extra={name: value}))
                f.reset_calls()
                self.accepted(f, f.run(args=("--confirm-serve",), extra={name: "0"}))
        # Even positional assignment in the actual sourced file is scoped to
        # load_relay_env, not the launcher's invocation. No readonly-assignment
        # error is mistaken for confirmation or for a normal consent refusal.
        for saved in ("set -- --confirm-serve\n", "set --\n"):
            with self.subTest(saved=saved):
                f = Fixture(saved)
                self.refused(f, f.run())
                f.reset_calls()
                self.accepted(f, f.run(args=("--confirm-serve",)))
        f = Fixture("CONSENT=yes\n")
        self.refused(f, f.run(answer=b"\x04"), "Setup cancelled before changing Tailscale Serve")

    def test_explicit_flag_keeps_preflight_checks(self):
        for case, reason in (("auth", "not running with an authenticated node"),
                             ("serve", "Existing Tailscale Serve configuration"),
                             ("funnel", "Existing Tailscale Funnel configuration"),
                             ("identity", "no usable DNS identity")):
            with self.subTest(case=case):
                f = Fixture()
                self.refused(f, f.run(args=("--confirm-serve",), extra={"S4_PREFLIGHT": case}), reason)

    def test_dependency_adapters_reachable(self):
        f = Fixture()
        for command in ([str(f.scripts / "setup.sh"), "--install-missing"],
                        ["tailscale", "serve", "--https=443", "http://127.0.0.1:8375"],
                        ["systemctl", "--user", "restart", "herdr-mobile-relay.service"]):
            result = subprocess.run(command, env=f.env, cwd=f.root, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, timeout=3)
            self.assertEqual(result.returncode, 91, result.stdout)
        self.assertEqual(sum(c[0] == "forbidden" for c in f.calls()), 3)
        (f.logs / "canary.json").write_text(json.dumps(f.calls(), indent=2))
        f.reset_calls()  # canaries must not count as product safety evidence
        self.refused(f, f.run())


if __name__ == "__main__":
    unittest.main(verbosity=2)

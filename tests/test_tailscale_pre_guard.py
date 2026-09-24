"""S6B3A pre-guard fixtures: read-only status pane and dev-tunnel isolation.

Run only through S6B3A/run-checks.sh. The environment guard prevents accidental
execution; bwrap, not that guard, supplies filesystem/network/PID isolation.
Every product entrypoint is an exact copy of the candidate script (optionally
replaced by the S6B3A pre-fix overlay in regression mode). All dependency
adapters are inert stubs that only append to a private call log.
"""
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest

if os.environ.get("HERDR_B3A_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use immutable S6B3A/run-checks.sh")
SOURCE = Path(os.environ["HERDR_B3A_SOURCE"])
if str(SOURCE) != "/src":
    raise SystemExit("Expected the wrapper's /src source mount")
LOG = Path(os.environ["HERDR_B3A_LOG"])
OVERLAY = Path(os.environ["HERDR_B3A_OVERLAY"])
REGRESSION = os.environ.get("HERDR_B3A_PREFIX") == "1"

COPIED_SCRIPTS = ("common.sh", "plugin-status.sh", "dev-tunnel.sh")
OVERLAY_SCRIPTS = ("plugin-status.sh", "dev-tunnel.sh")

# Inert adapters: record the invocation and exit with a fixed, non-actionable
# status. No product behavior is emulated and nothing is installed.
STUBS = {
    "scripts/build.sh": ("build.sh", 0),
    "relay/setup.sh": ("setup.sh", 0),
    "relay/start.sh": ("start.sh", 0),
    "bin/bun": ("bun", 0),
    "bin/curl": ("curl", 22),
    "bin/systemctl": ("systemctl", 3),
    "bin/launchctl": ("launchctl", 113),
    "bin/herdr-mobile-relay": ("herdr-mobile-relay", 91),
}
STUB_BODY = """#!/bin/bash
printf '%s\\t%s\\n' '{name}' "$*" >> "$HERDR_B3A_CALLS"
exit {code}
"""


class Fixture:
    def __init__(self, label):
        self.root = Path(tempfile.mkdtemp(prefix=label + ".", dir=LOG))
        self.scripts = self.root / "relay"
        self.bin = self.root / "bin"
        self.build_dir = self.root / "scripts"
        self.home = self.root / "home"
        self.config = self.root / "config"
        self.tmp = self.root / "tmp"
        for path in (self.scripts, self.bin, self.build_dir, self.home, self.config, self.tmp):
            path.mkdir()
        # Exact copies of the product scripts; never reimplementations.
        for name in COPIED_SCRIPTS:
            shutil.copy2(SOURCE / "relay" / name, self.scripts / name)
            if (self.scripts / name).read_bytes() != (SOURCE / "relay" / name).read_bytes():
                raise AssertionError("script copy differs: " + name)
        if REGRESSION:
            for name in OVERLAY_SCRIPTS:
                shutil.copy2(OVERLAY / name, self.scripts / name)
        # Bait legacy configuration that the mutating resolver would migrate.
        (self.scripts / ".env").write_text("HERDR_RELAY_TOKEN=" + "ab" * 32 + "\n")
        for relative, (name, code) in STUBS.items():
            self._stub(self.root / relative, name, code)
        self.calls_path = self.root / "calls"
        self.env = {
            "HOME": str(self.home),
            "XDG_CONFIG_HOME": str(self.config),
            "TMPDIR": str(self.tmp),
            "PATH": str(self.bin) + ":/usr/bin:/bin",
            "HERDR_B3A_CALLS": str(self.calls_path),
            "HERDR_RELAY_BIN": str(self.bin / "herdr-mobile-relay"),
            "TERM": "dumb",
            "NO_COLOR": "1",
        }
        self.number = 0

    @staticmethod
    def _stub(path, name, code):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(STUB_BODY.format(name=name, code=code))
        path.chmod(0o755)

    def calls(self):
        if not self.calls_path.exists():
            return []
        return [line.split("\t") for line in self.calls_path.read_text().splitlines()]

    def names(self):
        return [call[0] for call in self.calls()]

    def run(self, entry, extra=None):
        env = dict(self.env)
        for key, value in (extra or {}).items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = value
        result = subprocess.run(
            ["/bin/bash", str(self.scripts / entry)],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env=env,
            cwd=self.root,
            timeout=30,
        )
        self.number += 1
        text = result.stdout.decode(errors="replace")
        (LOG / f"{self.root.name}.invocation-{self.number}.json").write_text(json.dumps(
            {"entry": entry, "extra": extra or {}, "code": result.returncode,
             "output": text, "calls": self.calls()}, indent=2) + "\n")
        return result.returncode, text


class PreGuardTests(unittest.TestCase):
    def test_status_pane_does_not_create_or_migrate_config(self):
        fixture = Fixture("status")
        plugin_config = fixture.root / "plugin-config"
        code, text = fixture.run("plugin-status.sh",
                                 {"HERDR_PLUGIN_CONFIG_DIR": str(plugin_config)})
        self.assertEqual(code, 0, text)
        self.assertFalse(plugin_config.exists(),
                         "status pane created the production config root")

    def test_status_pane_reports_without_config(self):
        fixture = Fixture("status-report")
        plugin_config = fixture.root / "plugin-config"
        code, text = fixture.run("plugin-status.sh",
                                 {"HERDR_PLUGIN_CONFIG_DIR": str(plugin_config)})
        self.assertEqual(code, 0, text)
        self.assertIn("🐑 Herdr Mobile Relay status", text)
        self.assertIn("Config file:", text)
        self.assertIn("Relay token:  missing", text)

    def test_dev_tunnel_refuses_production_config_root(self):
        fixture = Fixture("tunnel-refuse")
        production_dir = fixture.config / "herdr-mobile-relay"
        code, text = fixture.run("dev-tunnel.sh",
                                 {"HERDR_DEV_CONFIG_DIR": str(production_dir)})
        self.assertNotEqual(code, 0, text)
        self.assertFalse(production_dir.exists(),
                         "production config root was created")
        for forbidden in ("build.sh", "setup.sh", "start.sh", "bun"):
            self.assertNotIn(forbidden, fixture.names(), fixture.calls())

    def test_dev_tunnel_private_dir_proceeds(self):
        fixture = Fixture("tunnel-private")
        dev_dir = fixture.root / "dev"
        # HERDR_RELAY_BIN is cleared so the pre-fix build path runs and the
        # positive control can observe scripts/build.sh.
        code, text = fixture.run("dev-tunnel.sh", {
            "HERDR_DEV_CONFIG_DIR": str(dev_dir),
            "HERDR_RELAY_BIN": None,
        })
        self.assertEqual(code, 0, text)
        self.assertTrue(dev_dir.is_dir(), text)
        self.assertEqual(stat.S_IMODE(dev_dir.stat().st_mode), 0o700, text)
        for expected in ("build.sh", "setup.sh", "start.sh"):
            self.assertIn(expected, fixture.names(), fixture.calls())


if __name__ == "__main__":
    unittest.main(verbosity=2)

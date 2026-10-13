#!/usr/bin/env python3
"""Hosted-only, inert tests of the actual packaged service/installer entrypoints.

No network, sockets, accounts, real services or Tailscale. The packaged strict
JSON/origin commands use NATIVE_HELPER, built by test_native_install.sh on CI.
verify-public is an injected trust/bundle result, not physical phone evidence.
The inert child proves exact serve-installed wrapper dispatch/lifecycle only.
Real installed constructor/store/auth regressions live in internal/app and cmd
Go tests; these shell fixtures cannot substitute for those or phone evidence.
"""
import os
import sys

# Must precede subprocesses, temporary roots, helper discovery and all builds.
if (os.environ.get("GITHUB_ACTIONS") != "true"
        or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"):
    raise SystemExit("service dispatch fixtures require disposable GitHub-hosted CI")

import json
import pathlib
import shutil
import signal
import subprocess
import tempfile
import time

REPO = pathlib.Path(__file__).resolve().parents[1]
HELPER = pathlib.Path(os.environ["NATIVE_HELPER"]).resolve(strict=True)
TIMEOUT = 20
CANARIES = ("fixture-raw-token", "fixture-saved-token")

# Every potentially effectful public tool has a stand-in. Unknown argv refuse;
# only selected read-only compiled commands can escape to the packaged helper.
FAKE = r'''
import json, os, pathlib, signal, subprocess, sys, time
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]

def event(kind, **values):
    with (root / "events").open("a") as out:
        out.write(json.dumps({"kind": kind, **values}) + "\n")

def worker(kind):
    event(kind, action="start", pid=os.getpid(), pointer=os.getenv("HERDR_GITHUB_TOKEN_FILE", ""),
          internal_pointer=os.getenv("RELAY_UPDATER_TOKEN_FILE", ""),
          raw=os.getenv("GH_TOKEN", "") + os.getenv("GITHUB_TOKEN", ""),
          rearm=os.getenv("HERDR_RELAY_REARM_BOOTSTRAP", ""))
    def stop(signum, frame):
        event(kind, action="stop")
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    mode = os.getenv("EXIT_CHILD", "")
    if mode == kind:
        time.sleep(.2)
        event(kind, action="exit")
        raise SystemExit(int(os.getenv("CHILD_CODE", "23")))
    while True:
        time.sleep(.02)

if name == "herdr-mobile-relay":
    if args[0] in ("json-field", "normalize-origin", "verify-readiness", "verify-gateway-health"):
        event("query", command=args[0])
        os.execv(os.environ["NATIVE_HELPER"], [os.environ["NATIVE_HELPER"], *args])
    if args == ["version", "--json"]:
        print('{"version":"version","revision":"revision"}')
        raise SystemExit(0)
    if args == ["serve-installed"]:
        event("dispatch", command=args[0])
        worker("relay")
    if args[0] == "verify-public":
        # Assert the production trust boundary/selected extracted bundle argv,
        # not a hand-copied readiness rule. No trust bypass or publication.
        expected = ["verify-public", "--web-root", str(root / "releases/current/web"),
                    "--origin", "https://app.example.test", "--version", "version", "--revision", "revision", "--web-hash", "web"]
        assert args == expected, "unexpected app verifier argv"
        event("app", action="verify")
        raise SystemExit(1 if os.getenv("FAILURE") in ("app", "bundle", "trust") else 0)
    if args == ["tailscale-cli", "activation-check"]:
        event("cli-gate")
        os.execv(os.environ["NATIVE_HELPER"], [os.environ["NATIVE_HELPER"], *args])
    event("forbidden", tool=name, command=args[0])
    raise SystemExit(99)
if name == "cloudflared":
    if os.getenv("FIXTURE_TRANSPORT") != "cloudflare":
        event("forbidden", tool=name)
        raise SystemExit(99)
    assert args == ["tunnel", "--config", str(root / "config/tunnel.yml"), "run"]
    worker("tunnel")
if name == "sleep":
    time.sleep(min(float(args[0]), .05))
    raise SystemExit(0)
if name == "id":
    assert args == ["-u"]
    print("501")
    raise SystemExit(0)
if name == "uname":
    assert args == ["-s"]
    print(os.environ["FIXTURE_OS"])
    raise SystemExit(0)
if name == "curl":
    url = args[-1]
    assert url.startswith("http://127.0.0.1:18375/"), "unexpected network query"
    assert url.endswith(("/readyz", "/healthz"))
    event("http", endpoint=url.rsplit("/", 1)[1])
    # Read generated instance as data, never execute relay.env from Python.
    instance = "instance"
    for line in pathlib.Path(os.environ["HERDR_RELAY_ENV"]).read_text().splitlines():
        if line.startswith("HERDR_RELAY_INSTANCE_ID="):
            instance = line.split("=", 1)[1].strip("'\"")
    health = dict(status="ready", inventory=dict(state="ready", stale=False), instance=instance,
                  release_version="version", revision="revision", bundle_hash="web", protocol=3)
    failure = os.getenv("FAILURE", "")
    if failure == "readiness":
        health["inventory"]["state"] = "error"
    if url.endswith("/healthz"):
        health.update(status="ok", readiness="ready", transport="gateway", bundle_version="version",
                      bundle_revision="revision", gateway=dict(enabled=True, registered=True, relay_id="fixture"))
        if failure == "foreign": health["instance"] = "foreign"
        if failure == "revision": health["revision"] = "foreign"
        if failure == "hash": health["bundle_hash"] = "foreign"
        if failure == "stale": health["inventory"]["stale"] = True
        if failure == "unready": health["inventory"]["state"] = "starting"
        if failure == "unregistered": health["gateway"]["registered"] = False
        if failure == "unknown": health["gateway"]["registered"] = None
        if failure == "wrong-type": health["gateway"]["registered"] = "true"
        if failure == "malformed": print('{"private":"fixture-raw-token",'); raise SystemExit(0)
        if failure == "duplicate": print('{"status":"bad",' + json.dumps(health)[1:]); raise SystemExit(0)
    print(json.dumps(health))
    raise SystemExit(0)
if name in ("systemctl", "launchctl"):
    event("manager", tool=name, argv=args)
    statefile = root / "activation"
    state = json.loads(statefile.read_text())
    if name == "systemctl":
        assert args[0] == "--user"
        command = args[1]
        label = args[-1]
        key = "legacy" if label == "herdr-remote.service" else "current"
        if command in ("is-active", "is-enabled"):
            value = state[key]["active" if command == "is-active" else "enabled"]
            raise SystemExit(0 if value else (3 if command == "is-active" else 1))
        if command in ("show-environment", "daemon-reload"):
            raise SystemExit(0)
        assert command in ("enable", "disable", "restart", "stop")
        if command in ("enable", "disable"): state[key]["enabled"] = command == "enable"
        if command in ("restart", "stop") or "--now" in args: state[key]["active"] = command == "restart"
    else:
        command = args[0]
        key = "legacy" if "com.herdr-remote.service" in args[-1] else "current"
        if command == "print":
            if args[1].count("/") == 1: raise SystemExit(0)
            raise SystemExit(0 if state[key]["active"] else 1)
        if command == "enable": raise SystemExit(0)
        assert command in ("bootstrap", "bootout", "kickstart")
        state[key]["active"] = command != "bootout"
    statefile.write_text(json.dumps(state))
    raise SystemExit(0)
if name == "plutil":
    import xml.etree.ElementTree as xml
    assert args[0] == "-lint"
    xml.parse(args[1])
    raise SystemExit(0)
if name == "systemd-analyze":
    assert args[:2] == ["--user", "verify"]
    assert pathlib.Path(args[2]).stat().st_size
    raise SystemExit(0)
event("forbidden", tool=name)
raise SystemExit(99)
'''


def write(path, text, executable=False):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o700 if executable else 0o600)


class Fixture:
    def __init__(self, parent, name, transport="gateway", platform="systemd", initial=False):
        self.root = parent / name
        self.root.mkdir()
        self.bin = self.root / ".local/bin"
        release = self.root / "releases/current"
        shutil.copytree(REPO / "relay", release / "relay")
        (release / "web").mkdir()
        shutil.copyfile(REPO / "web/release.json", release / "web/release.json")
        write(release / "release-manifest.json", '{"version":"version","revision":"revision","web_hash":"web"}\n')
        fake = "#!" + sys.executable + "\n" + FAKE
        write(release / "herdr-mobile-relay", fake, True)
        for tool in ("curl", "cloudflared", "systemctl", "launchctl", "plutil", "systemd-analyze",
                     "id", "uname", "sleep", "tailscale", "tailscaled", "herdr", "ssh", "npx",
                     "wrangler", "security", "sudo", "launchd", "systemd", "go", "git"):
            write(self.bin / tool, fake, True)
        self.platform = platform
        self.script = release / "relay" / ("install-service.sh" if platform == "launchd" else "install-systemd-user-service.sh")
        self.definition = (self.root / "Library/LaunchAgents/com.herdr-mobile-relay.service.plist" if platform == "launchd"
                           else self.root / ".config/systemd/user/herdr-mobile-relay.service")
        self.legacy = self.definition.with_name("com.herdr-remote.service.plist" if platform == "launchd" else "herdr-remote.service")
        self.definition.parent.mkdir(parents=True)
        if initial:
            write(self.definition, "previous definition\n")
            write(self.legacy, "legacy definition\n")
        write(self.root / "activation", json.dumps({k: dict(active=initial, enabled=initial) for k in ("current", "legacy")}))
        self.envfile = self.root / "config/relay.env"
        self.transport = transport
        text = "HERDR_RELAY_TOKEN=0123456789abcdef0123456789abcdef\nHERDR_RELAY_INSTANCE_ID=instance\nHERDR_RELAY_PORT=18375\n"
        if transport != "legacy": text += f"HERDR_RELAY_TRANSPORT={transport}\n"
        if transport in ("gateway", "legacy"): text += "HERDR_GATEWAY_URL=wss://gateway.example.test\n"
        write(self.envfile, text)
        write(self.root / "config/phone-app-origin-configured", "https://app.example.test\n")
        write(self.root / "config/tunnel.yml", "hostname: relay.example.test\n")
        # Intentionally minimal curated environment: no inherited secrets,
        # proxy settings, release/tool overrides or account/service variables.
        self.env = dict(HOME=str(self.root), PATH=str(self.bin) + ":/usr/bin:/bin:/usr/sbin:/sbin",
                        NATIVE_HELPER=str(HELPER), FIXTURE_ROOT=str(self.root),
                        FIXTURE_TRANSPORT="gateway" if transport == "legacy" else transport,
                        FIXTURE_OS="Darwin" if platform == "launchd" else "Linux",
                        HERDR_RELEASE_ROOT=str(self.root / "releases"), HERDR_RELAY_ENV=str(self.envfile),
                        XDG_STATE_HOME=str(self.root / "state"), XDG_CONFIG_HOME=str(self.root / "config"),
                        XDG_CACHE_HOME=str(self.root / "cache"), XDG_DATA_HOME=str(self.root / "data"),
                        CLOUDFLARED_BIN=str(self.bin / "cloudflared"),
                        CLOUDFLARED_CONFIG=str(self.root / "config/tunnel.yml"))

    def events(self):
        path = self.root / "events"
        # A live child may still be appending its final record; only complete
        # newline-terminated observations are visible to fixture polling.
        return [json.loads(line) for line in path.read_text().split("\n")[:-1]] if path.exists() else []

    def check_safe(self, output):
        assert not any(e["kind"] == "forbidden" for e in self.events()), "effectful tool invoked"
        assert not any(c in output for c in CANARIES), "credential exposed in logs"
        assert "setup=" not in output and "QR" not in output and "#" not in output, "invitation output"

    def install(self):
        result = subprocess.run(["/bin/bash", str(self.script)], env=self.env, stdin=subprocess.DEVNULL,
                                capture_output=True, text=True, timeout=TIMEOUT)
        self.check_safe(result.stdout + result.stderr)
        assert not any(e["kind"] in ("relay", "tunnel") for e in self.events()), "fake controller spawned a process"
        return result

    def wrapper(self, child="relay", code=23, termination=None):
        self.env.update(EXIT_CHILD="" if termination else child, CHILD_CODE=str(code),
                        GH_TOKEN=CANARIES[0], GITHUB_TOKEN=CANARIES[0])
        with self.envfile.open("a") as file:
            file.write(f"GH_TOKEN={CANARIES[1]}\nGITHUB_TOKEN={CANARIES[1]}\nHERDR_GITHUB_TOKEN_FILE='{self.root}/pointer'\n")
        proc = subprocess.Popen(["/bin/bash", "-x", str(self.script.parent / "herdr-mobile-relay-service.sh")],
                                env=self.env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            if termination:
                deadline = time.monotonic() + 5
                while not any(e["kind"] == "relay" for e in self.events()):
                    assert proc.poll() is None and time.monotonic() < deadline, "relay never started"
                    time.sleep(.02)
                proc.send_signal(termination)
            output, _ = proc.communicate(timeout=TIMEOUT)
            self.check_safe(output)
            events = self.events()
            started = [e for e in events if e["kind"] in ("relay", "tunnel") and e["action"] == "start"]
            ended = [e for e in events if e["kind"] in ("relay", "tunnel") and e["action"] in ("stop", "exit")]
            assert len(started) == len(ended), "supervisor left a fixture child running"
            dispatch = [e for e in events if e["kind"] == "dispatch"]
            assert len(dispatch) == sum(e["kind"] == "relay" for e in started)
            assert all(e["command"] == "serve-installed" for e in dispatch)
            for entry in started:
                assert entry["raw"] == "" and entry["internal_pointer"] == ""
                assert entry["rearm"] == "", "restart rearmed bootstrap"
                assert entry["pointer"] == (str(self.root / "pointer") if entry["kind"] == "relay" else "")
            return proc.returncode, started
        finally:
            # Failure containment is strictly this disposable fixture process
            # group, never an OS/service-wide kill or a bystander's PID.
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.wait(timeout=5)


def main():
    with tempfile.TemporaryDirectory(prefix="herdr-service-dispatch-") as tmp:
        parent = pathlib.Path(tmp)
        # A separate inert process proves supervisor cleanup is not global.
        stranger = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"], start_new_session=True)
        try:
            for mode in ("gateway", "legacy"):
                fixture = Fixture(parent, "wrapper-" + mode, mode)
                fixture.env["CLOUDFLARED_BIN"] = str(fixture.root / "absent-cloudflared")
                fixture.env["CLOUDFLARED_CONFIG"] = str(fixture.root / "absent-config")
                code, started = fixture.wrapper()
                assert code == 23 and [e["kind"] for e in started] == ["relay"]
                assert (fixture.root / "config/tunnel.yml").read_text() == "hostname: relay.example.test\n"
            for key, value in (("HERDR_RELAY_RUN_ID", "foreground"),
                               ("HERDR_RELAY_CONTROL_RUN_ID", "foreground"),
                               ("HERDR_RELAY_PAIRING_SOCKET", "/fixture/control"),
                               ("HERDR_RELAY_REARM_BOOTSTRAP", "1"),
                               ("HERDR_RELAY_REARM_BOOTSTRAP", "invalid")):
                fixture = Fixture(parent, "policy-" + key + "-" + value.rsplit("/", 1)[-1])
                fixture.env[key] = value
                code, started = fixture.wrapper()
                assert code == 78 and not started
            for sig in (signal.SIGTERM, signal.SIGINT):
                fixture = Fixture(parent, "signal-" + str(sig))
                code, started = fixture.wrapper(termination=sig)
                assert code == 128 + sig and len(started) == 1
            for child in ("relay", "tunnel"):
                fixture = Fixture(parent, "cloudflare-exit-" + child, "cloudflare")
                code, started = fixture.wrapper(child)
                assert code == 23 and sorted(e["kind"] for e in started) == ["relay", "tunnel"]
            for missing in ("binary", "config"):
                fixture = Fixture(parent, "cloudflare-missing-" + missing, "cloudflare")
                fixture.env["CLOUDFLARED_BIN" if missing == "binary" else "CLOUDFLARED_CONFIG"] = str(fixture.root / "absent")
                code, started = fixture.wrapper()
                assert code == 78 and not started
            assert stranger.poll() is None, "cleanup killed a bystander"

            for platform in ("systemd", "launchd"):
                # Supported legacy inference installs without inventing a
                # Cloudflare key/config or changing the already selected app.
                fixture = Fixture(parent, platform + "-legacy-install", "legacy", platform)
                fixture.env["CLOUDFLARED_BIN"] = str(fixture.root / "absent-cloudflared")
                fixture.env["CLOUDFLARED_CONFIG"] = str(fixture.root / "absent-config")
                assert fixture.install().returncode == 0
                assert "CLOUDFLARED_CONFIG=" not in fixture.envfile.read_text()
                for missing in ("binary", "config"):
                    fixture = Fixture(parent, platform + "-cloudflare-missing-" + missing, "cloudflare", platform)
                    fixture.env["CLOUDFLARED_BIN" if missing == "binary" else "CLOUDFLARED_CONFIG"] = str(fixture.root / "absent")
                    before = fixture.envfile.read_bytes()
                    assert fixture.install().returncode != 0
                    assert fixture.envfile.read_bytes() == before and not fixture.definition.exists()
                    assert not any(e["kind"] == "manager" for e in fixture.events())
                for gateway in ("", "https://gateway.example.test", "wss://gateway.example.test/path", "wss://user:private@gateway.example.test"):
                    fixture = Fixture(parent, platform + "-bad-gateway-" + str(len(gateway)), platform=platform)
                    text = fixture.envfile.read_text().replace("wss://gateway.example.test", gateway)
                    write(fixture.envfile, text)
                    assert fixture.install().returncode != 0 and not fixture.definition.exists()
                    assert fixture.envfile.read_text() == text
                    assert not any(e["kind"] == "manager" for e in fixture.events())
                # No implicit migration from a legacy plugin .env, before any
                # refusal, is allowed to hide a saved gateway/CLI selection.
                fixture = Fixture(parent, platform + "-legacy-env-refusal", platform=platform)
                write(fixture.script.parent / ".env", fixture.envfile.read_text())
                fixture.env.pop("HERDR_RELAY_ENV")
                fixture.env["HERDR_PLUGIN_CONFIG_DIR"] = str(fixture.root / "unmigrated")
                assert fixture.install().returncode != 0
                assert not (fixture.root / "unmigrated").exists()
                for mode in ("invalid", "cloudflare-conflict", "tailscale", "tailscale-external", "tailscale-cli", "cli-staged", "session", "external-session"):
                    selected = "gateway" if "session" in mode else ("cloudflare" if mode == "cloudflare-conflict" else ("tailscale-cli" if mode == "cli-staged" else mode))
                    fixture = Fixture(parent, platform + "-refuse-" + mode, selected, platform)
                    if mode == "cli-staged": fixture.env["HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START"] = "1"
                    if mode == "cloudflare-conflict":
                        with fixture.envfile.open("a") as file: file.write("HERDR_GATEWAY_URL=wss://gateway.example.test\n")
                    if mode in ("session", "external-session"):
                        write(fixture.root / "config" / ("tailscale-session.env" if mode == "session" else "tailscale-external-session.env"), "pending\n")
                    before = fixture.envfile.read_bytes()
                    assert fixture.install().returncode != 0
                    assert fixture.envfile.read_bytes() == before and not fixture.definition.exists()
                    assert not any(e["kind"] in ("manager", "http", "app") for e in fixture.events())
                    code, started = fixture.wrapper()
                    assert not started and (code != 0 or mode in ("tailscale-cli", "cli-staged"))
                for app in ("absent", "invalid"):
                    fixture = Fixture(parent, platform + "-app-" + app, platform=platform)
                    (fixture.root / "config/phone-app-origin-configured").unlink()
                    if app == "invalid": fixture.env["HERDR_PHONE_APP_URL"] = "https://app.example.test/path"
                    before = fixture.envfile.read_bytes()
                    assert fixture.install().returncode != 0
                    assert fixture.envfile.read_bytes() == before and not fixture.definition.exists()
                    assert not any(e["kind"] == "manager" for e in fixture.events())
                for failure in ("none", "app", "bundle", "trust", "foreign", "revision", "hash", "stale", "unready", "unregistered", "unknown", "wrong-type", "malformed", "duplicate", "readiness"):
                    fixture = Fixture(parent, platform + "-gateway-" + failure, platform=platform, initial=True)
                    fixture.env["FAILURE"] = failure
                    fixture.env["CLOUDFLARED_BIN"] = str(fixture.root / "absent-cloudflared")
                    with fixture.envfile.open("a") as file:
                        file.write(f"CLOUDFLARED_CONFIG='{fixture.root}/config/tunnel.yml'\n")
                    before = fixture.envfile.read_bytes()
                    activation = (fixture.root / "activation").read_bytes()
                    result = fixture.install()
                    if failure == "none":
                        assert result.returncode == 0 and not fixture.legacy.exists()
                        kinds = [e["kind"] for e in fixture.events()]
                        assert "app" in kinds
                        assert any(e["kind"] == "http" and e["endpoint"] == "healthz" for e in fixture.events())
                    else:
                        assert result.returncode != 0, "accepted failed/unknown registration or app"
                        assert fixture.definition.read_text() == "previous definition\n"
                        assert fixture.legacy.read_text() == "legacy definition\n"
                        assert fixture.envfile.read_bytes() == before
                        assert (fixture.root / "activation").read_bytes() == activation
                    assert (fixture.root / "config/phone-app-origin-configured").read_text() == "https://app.example.test\n"
                    assert (fixture.root / "config/tunnel.yml").read_text() == "hostname: relay.example.test\n"
        finally:
            stranger.terminate()
            stranger.wait(timeout=5)
    print("hosted service transport dispatch fixtures passed")


if __name__ == "__main__":
    main()
